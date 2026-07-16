package sheets

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

// closeEnough reports whether a and b differ by less than half a cent —
// USD amounts derived from chained float64 arithmetic (e.g. -0.12 + 0.55)
// can land a few ULPs off an exact decimal value.
func closeEnough(a, b float64) bool {
	return math.Abs(a-b) < 0.005
}

func TestParseInt(t *testing.T) {
	cases := []struct {
		name  string
		input interface{}
		want  int
	}{
		{"float64 from UNFORMATTED_VALUE", float64(42), 42},
		{"float64 truncates fractional part", float64(42.9), 42},
		{"numeric string", "17", 17},
		{"non-numeric string", "abc", 0},
		{"empty string", "", 0},
		{"unsupported type (bool)", true, 0},
		{"nil", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseInt(c.input)
			if got != c.want {
				t.Errorf("parseInt(%v) = %d, want %d", c.input, got, c.want)
			}
		})
	}
}

func TestParseFloat(t *testing.T) {
	cases := []struct {
		name  string
		input interface{}
		want  float64
	}{
		{"float64 passthrough", float64(3.14), 3.14},
		{"numeric string", "2.50", 2.50},
		{"negative numeric string", "-12.5", -12.5},
		{"non-numeric string", "abc", 0},
		{"empty string", "", 0},
		{"unsupported type (bool)", true, 0},
		{"nil", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseFloat(c.input)
			if got != c.want {
				t.Errorf("parseFloat(%v) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

func TestParseLedgerRows(t *testing.T) {
	t.Run("well-formed rows", func(t *testing.T) {
		rows := [][]interface{}{
			{"2026-07-01 10:00", "deposit", float64(100), float64(100)},
			{"2026-07-02 09:30", "cash_session", float64(-5.5), float64(94.5)},
		}
		entries, err := parseLedgerRows(rows)
		if err != nil {
			t.Fatalf("parseLedgerRows: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("len(entries) = %d, want 2", len(entries))
		}
		wantDate0, _ := time.Parse(dateFormat, "2026-07-01 10:00")
		if !entries[0].Date.Equal(wantDate0) {
			t.Errorf("entries[0].Date = %v, want %v", entries[0].Date, wantDate0)
		}
		if entries[0].Type != "deposit" || entries[0].AmountUSD != 100 || entries[0].RunningTotal != 100 {
			t.Errorf("entries[0] = %+v, unexpected", entries[0])
		}
		if entries[1].Type != "cash_session" || entries[1].AmountUSD != -5.5 || entries[1].RunningTotal != 94.5 {
			t.Errorf("entries[1] = %+v, unexpected", entries[1])
		}
	})

	t.Run("short row is skipped, not erroring", func(t *testing.T) {
		rows := [][]interface{}{
			{"2026-07-01 10:00", "deposit"}, // only 2 columns, skipped
			{"2026-07-02 09:30", "cash_session", float64(-5.5), float64(94.5)},
		}
		entries, err := parseLedgerRows(rows)
		if err != nil {
			t.Fatalf("parseLedgerRows: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1 (short row skipped)", len(entries))
		}
	})

	t.Run("malformed date aborts the whole parse", func(t *testing.T) {
		rows := [][]interface{}{
			{"2026-07-01 10:00", "deposit", float64(100), float64(100)},
			{"not-a-date", "cash_session", float64(-5.5), float64(94.5)},
		}
		_, err := parseLedgerRows(rows)
		if err == nil {
			t.Fatal("expected error for malformed date")
		}
	})

	t.Run("empty input", func(t *testing.T) {
		entries, err := parseLedgerRows(nil)
		if err != nil {
			t.Fatalf("parseLedgerRows(nil): %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("len(entries) = %d, want 0", len(entries))
		}
	})
}

// TestCashSessionID confirms the upsert key is stable across repeated
// re-parses of the same growing file (same date + first hand ID always
// produces the same ID) and distinct for anything that actually differs.
func TestCashSessionID(t *testing.T) {
	d1, _ := time.Parse(dateFormat, "2026-07-01 20:00")
	d2, _ := time.Parse(dateFormat, "2026-07-01 20:01")

	id1a := cashSessionID(d1, "200000000001")
	id1b := cashSessionID(d1, "200000000001")
	if id1a != id1b {
		t.Errorf("cashSessionID not stable across identical inputs: %q vs %q", id1a, id1b)
	}

	diffHandID := cashSessionID(d1, "200000000002")
	if diffHandID == id1a {
		t.Error("cashSessionID collided for two different first-hand IDs on the same date")
	}

	diffDate := cashSessionID(d2, "200000000001")
	if diffDate == id1a {
		t.Error("cashSessionID collided for two different dates with the same first-hand ID")
	}
}

func TestCascadeLedgerAmounts(t *testing.T) {
	t.Run("single row, last in ledger — matches simple update", func(t *testing.T) {
		// amounts = [deposit 100, session -5]; correcting the session row
		// (index 1) to -8 with base = running total before it (100).
		got := cascadeLedgerAmounts([]float64{100, -5}, 1, -8, 100)
		want := [][]interface{}{{-8.0, 92.0}}
		if !equalUpdates(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("interleaved tables — later row's running total must shift too", func(t *testing.T) {
		// Two tables open at once, start bankroll 100:
		//   row0 deposit          +100 -> RT 100 (not part of amounts; base=100)
		//   row1 table A hand 1     -5 -> RT  95
		//   row2 table B hand 1     -3 -> RT  92
		// Table A's 2nd hand arrives: cumulative net now -8, correcting row1.
		// row2 (table B) must be recomputed too — it comes after row1 in the
		// ledger even though it belongs to a different, unrelated session.
		amounts := []float64{-5, -3} // row1, row2 (base 100 is the deposit before them)
		got := cascadeLedgerAmounts(amounts, 0, -8, 100)
		want := [][]interface{}{
			{-8.0, 92.0}, // row1 corrected: 100 - 8
			{-3.0, 89.0}, // row2 recomputed: 92 - 3, not left at its stale 92
		}
		if !equalUpdates(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("changed row is the only row", func(t *testing.T) {
		got := cascadeLedgerAmounts([]float64{-5}, 0, -8, 0)
		want := [][]interface{}{{-8.0, -8.0}}
		if !equalUpdates(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("out of range index returns nil", func(t *testing.T) {
		if got := cascadeLedgerAmounts([]float64{-5}, 5, -8, 0); got != nil {
			t.Errorf("got %v, want nil", got)
		}
		if got := cascadeLedgerAmounts([]float64{-5}, -1, -8, 0); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

func equalUpdates(got, want [][]interface{}) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if len(got[i]) != 2 || len(want[i]) != 2 {
			return false
		}
		if got[i][0].(float64) != want[i][0].(float64) || got[i][1].(float64) != want[i][1].(float64) {
			return false
		}
	}
	return true
}

// TestColumnLetter confirms the 0-based-index-to-spreadsheet-column
// conversion backfillTabHeaders relies on to know which range to write.
func TestColumnLetter(t *testing.T) {
	cases := []struct {
		idx  int
		want string
	}{
		{0, "A"},
		{1, "B"},
		{11, "L"}, // Bounty Won (USD), the first backfilled column in the real Tournaments migration
		{12, "M"}, // Prize Won (USD)
		{25, "Z"},
		{26, "AA"},
		{27, "AB"},
		{51, "AZ"},
		{52, "BA"},
	}
	for _, c := range cases {
		if got := columnLetter(c.idx); got != c.want {
			t.Errorf("columnLetter(%d) = %q, want %q", c.idx, got, c.want)
		}
	}
}

// TestNewClient_MissingCredentialsFile confirms NewClient fails fast at the
// os.ReadFile step for a nonexistent service account path — no network
// access is attempted. This is the only part of Client construction
// testable without a live Google Sheets API call.
func TestNewClient_MissingCredentialsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	_, err := NewClient(path, "some-spreadsheet-id", "")
	if err == nil {
		t.Fatal("expected error for missing credentials file")
	}
}

// TestResolveSheetID_MissingCredentialsFile mirrors
// TestNewClient_MissingCredentialsFile for ResolveSheetID's own
// credentials read.
func TestResolveSheetID_MissingCredentialsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	_, err := ResolveSheetID(path, "My Poker Sheet")
	if err == nil {
		t.Fatal("expected error for missing credentials file")
	}
}

// TestNewClient_InvalidCredentialsJSON confirms a file that exists but
// isn't valid service-account JSON fails at credential parsing, still
// before any network call.
func TestNewClient_InvalidCredentialsJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "google_service_account.json")
	if err := os.WriteFile(path, []byte("not valid json"), 0644); err != nil {
		t.Fatalf("writing fake credentials: %v", err)
	}
	_, err := NewClient(path, "some-spreadsheet-id", "")
	if err == nil {
		t.Fatal("expected error for invalid credentials JSON")
	}
}

func TestFlushDecision(t *testing.T) {
	cases := []struct {
		name          string
		cached, sheet int
		exists        bool
		want          flushChoice
	}{
		{"row absent replays", 50, 0, false, flushReplay},
		{"sheet behind replays", 51, 50, true, flushReplay},
		{"sheet equal replays", 50, 50, true, flushReplay},
		{"sheet ahead discards as superseded", 50, 51, true, flushDiscard},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flushDecision(tc.cached, tc.sheet, tc.exists); got != tc.want {
				t.Errorf("flushDecision(%d, %d, %v) = %v, want %v", tc.cached, tc.sheet, tc.exists, got, tc.want)
			}
		})
	}
}

// TestLoadFallback_MissingFileIsEmpty confirms an absent fallback file is the
// normal empty state, not an error that would brick startup.
func TestLoadFallback_MissingFileIsEmpty(t *testing.T) {
	c := &Client{fallbackPath: filepath.Join(t.TempDir(), "nope.json")}
	if err := c.loadFallback(); err != nil {
		t.Fatalf("loadFallback on missing file: %v", err)
	}
	if len(c.fallback) != 0 {
		t.Errorf("want empty map, got %d entries", len(c.fallback))
	}
}

// TestFallbackRoundTrip caches all three entry kinds — including a tournament
// summary and its hand-stats sharing one TournamentID, which must coexist
// under distinct composite keys — persists them via cacheFailed, and confirms
// a fresh client reloads them intact and that a deletion survives a reload.
func TestFallbackRoundTrip(t *testing.T) {
	fbPath := filepath.Join(t.TempDir(), "sheets_fallback.json")
	c := &Client{fallbackPath: fbPath, fallback: make(map[string]fallbackEntry)}

	date := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	cash := parsers.CashSession{Date: date, Stake: "NL10", Hands: 42, NetUSD: -3.5, FirstHandID: "H1"}
	cashKey := fallbackKindCash + ":" + cashSessionID(cash.Date, cash.FirstHandID)
	c.cacheFailed(cashKey, fallbackEntry{Kind: fallbackKindCash, Cash: &cash})

	tid := "T999"
	tourney := parsers.Tournament{Date: date, Type: "spingo", TournamentID: tid, BuyInUSD: 5, NetUSD: -5}
	summaryKey := fallbackKindSummary + ":" + tid
	statsKey := fallbackKindStats + ":" + tid
	c.cacheFailed(summaryKey, fallbackEntry{Kind: fallbackKindSummary, Summary: &tourney})
	c.cacheFailed(statsKey, fallbackEntry{Kind: fallbackKindStats, HandStats: &handStatsArgs{
		TournamentID: tid, VPIP: 25, PFR: 20, Hands: 18, BountyUSD: 2, IsKnockout: true,
	}})

	c2 := &Client{fallbackPath: fbPath, fallback: make(map[string]fallbackEntry)}
	if err := c2.loadFallback(); err != nil {
		t.Fatalf("loadFallback: %v", err)
	}
	if len(c2.fallback) != 3 {
		t.Fatalf("got %d entries, want 3 (summary + stats coexist under one TID)", len(c2.fallback))
	}

	if got := c2.fallback[cashKey].Cash; got == nil || got.Hands != 42 || got.Stake != "NL10" {
		t.Errorf("cash entry round-trip wrong: %+v", c2.fallback[cashKey])
	}
	if got := c2.fallback[statsKey].HandStats; got == nil || got.Hands != 18 || !got.IsKnockout {
		t.Errorf("stats entry round-trip wrong: %+v", c2.fallback[statsKey])
	}
	if got := c2.fallback[summaryKey].Summary; got == nil || got.TournamentID != tid {
		t.Errorf("summary entry round-trip wrong: %+v", c2.fallback[summaryKey])
	}

	c2.fallbackMu.Lock()
	delete(c2.fallback, cashKey)
	if err := c2.persistFallbackLocked(); err != nil {
		c2.fallbackMu.Unlock()
		t.Fatalf("persistFallbackLocked: %v", err)
	}
	c2.fallbackMu.Unlock()

	c3 := &Client{fallbackPath: fbPath, fallback: make(map[string]fallbackEntry)}
	if err := c3.loadFallback(); err != nil {
		t.Fatalf("loadFallback after delete: %v", err)
	}
	if len(c3.fallback) != 2 {
		t.Fatalf("after delete got %d entries, want 2", len(c3.fallback))
	}
	if _, ok := c3.fallback[cashKey]; ok {
		t.Error("deleted cash entry still present after reload")
	}
}
