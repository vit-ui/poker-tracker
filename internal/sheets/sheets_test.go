package sheets

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
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

	id1a := CashSessionID(d1, "200000000001")
	id1b := CashSessionID(d1, "200000000001")
	if id1a != id1b {
		t.Errorf("CashSessionID not stable across identical inputs: %q vs %q", id1a, id1b)
	}

	diffHandID := CashSessionID(d1, "200000000002")
	if diffHandID == id1a {
		t.Error("CashSessionID collided for two different first-hand IDs on the same date")
	}

	diffDate := CashSessionID(d2, "200000000001")
	if diffDate == id1a {
		t.Error("CashSessionID collided for two different dates with the same first-hand ID")
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

// TestRunningTotalAbove confirms the cascade's base — the running total of
// the row above the corrected one — is read correctly out of the C:D range
// the cascade already fetched, for every row position. This replaced a
// second single-cell API read of the same value (the old
// runningTotalBeforeRow), so each case's expected value is exactly what that
// read returned: 0 for the first data row, the row-above's D cell otherwise.
func TestRunningTotalAbove(t *testing.T) {
	// The fetched C:D range, header at index 0; data rows are sheet rows 2-4:
	//   row2 deposit +100 -> RT 100, row3 session -5 -> RT 95, row4 session -3 -> RT 92.
	values := [][]interface{}{
		{"Amount (USD)", "Running Total (USD)"},
		{100.0, 100.0},
		{-5.0, 95.0},
		{-3.0, 92.0},
	}

	cases := []struct {
		name     string
		rowIndex int
		want     float64
	}{
		{"first data row has nothing above it", 2, 0},
		{"second data row reads the first's total", 3, 100},
		{"last data row reads the middle's total", 4, 95},
		{"row index past the fetched range", 6, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runningTotalAbove(values, c.rowIndex); got != c.want {
				t.Errorf("runningTotalAbove(values, %d) = %v, want %v", c.rowIndex, got, c.want)
			}
		})
	}

	t.Run("row above has its D cell omitted by the API", func(t *testing.T) {
		short := [][]interface{}{
			{"Amount (USD)", "Running Total (USD)"},
			{100.0}, // trailing empty D cell omitted in the response
			{-5.0, 95.0},
		}
		if got := runningTotalAbove(short, 3); got != 0 {
			t.Errorf("runningTotalAbove(short, 3) = %v, want 0", got)
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
	cashKey := fallbackKindCash + ":" + CashSessionID(cash.Date, cash.FirstHandID)
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

// ---------------------------------------------------------------------------
// Migration row builders — pure build → parse round-trip (no live Sheets API)
// ---------------------------------------------------------------------------

// viaWire round-trips a built row through JSON the way the Sheets API does:
// every numeric cell a build* function writes as a Go int/float64 comes back
// as float64 (JSON has one number type), and trailing empty cells are dropped.
// Parsing this — not the raw built row — is what proves the parse* functions
// decode real API output, since parseInt/parseFloat only handle float64/string.
func viaWire(row []interface{}) []interface{} {
	data, err := json.Marshal(row)
	if err != nil {
		panic(err)
	}
	var out []interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	n := len(out)
	for n > 0 {
		if s, ok := out[n-1].(string); ok && s == "" {
			n--
			continue
		}
		break
	}
	return out[:n]
}

func TestBuildTournamentRows_RoundTrip(t *testing.T) {
	utc := func(s string) time.Time {
		tm, err := time.Parse(dateFormat, s)
		if err != nil {
			t.Fatalf("parsing %q: %v", s, err)
		}
		return tm
	}
	want := []storage.TournamentRecord{
		{ // full knockout Spin & Go with hand stats and a cash prize pool
			Tournament: parsers.Tournament{
				Date: utc("2026-07-12 18:00"), Type: "spingo-ko", TournamentID: "T-SPIN-1",
				BuyInUSD: 5, PrizePoolUSD: 30, Multiplier: floatPtr(6), FinishPos: 1, TotalPlayers: 3,
				NetUSD: 29.5, VPIP: floatPtr(40), PFR: floatPtr(33), Hands: intPtr(18),
			},
			BountyWonUSD: 4.5, PrizeWonUSD: 34.5,
		},
		{ // summary-only MTT: nil multiplier, nil VPIP/PFR/Hands
			Tournament: parsers.Tournament{
				Date: utc("2026-07-15 20:00"), Type: "mtt", TournamentID: "T-MTT-2",
				BuyInUSD: 3, PrizePoolUSD: 200, FinishPos: 40, TotalPlayers: 100, NetUSD: -3,
			},
			PrizeWonUSD: 0,
		},
		{ // ticket-denominated satellite: description prize pool, no cash value
			Tournament: parsers.Tournament{
				Date: utc("2026-07-13 19:00"), Type: "spingo", TournamentID: "T-SAT-1",
				BuyInUSD: 1, PrizePoolDescription: "$1.00 Power Path Step 2 Ticket",
				FinishPos: 2, TotalPlayers: 3, NetUSD: -1,
			},
		},
		{ // hand-file-only partial: blank Date, placeholder Type, no summary cols
			Tournament: parsers.Tournament{
				Type: "ko", TournamentID: "T-PARTIAL-1",
				VPIP: floatPtr(30), PFR: floatPtr(25), Hands: intPtr(45),
			},
			BountyWonUSD: 2,
		},
	}

	rows := buildTournamentRows(want)
	if len(rows) != len(want) {
		t.Fatalf("built %d rows, want %d", len(rows), len(want))
	}
	for i, row := range rows {
		got, hasDate := parseTournamentRow(viaWire(row))
		if hasDate == want[i].Date.IsZero() {
			t.Errorf("row %d hasDate = %v, but Date.IsZero = %v", i, hasDate, want[i].Date.IsZero())
		}
		if !reflect.DeepEqual(got, want[i]) {
			t.Errorf("tournament %d round-trip differs:\n want %+v\n  got %+v", i, want[i], got)
		}
	}
}

func TestBuildLedgerRows_RoundTrip(t *testing.T) {
	d := func(s string) time.Time {
		tm, _ := time.Parse(dateFormat, s)
		return tm
	}
	want := []storage.LedgerEntry{
		{Date: d("2026-07-10 12:00"), Type: "deposit", AmountUSD: 100, RunningTotal: 100},
		{Date: d("2026-07-11 14:30"), Type: "cash_session", AmountUSD: -5.25, RunningTotal: 94.75, SessionID: "abc123def456"},
		{Date: d("2026-07-12 09:00"), Type: "withdrawal", AmountUSD: -20, RunningTotal: 74.75},
	}
	rows := buildLedgerRows(want)
	got, err := parseLedgerRows(rows)
	if err != nil {
		t.Fatalf("parseLedgerRows: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ledger round-trip differs:\n want %+v\n  got %+v", want, got)
	}
}

func TestBuildCashRows_RoundTrip(t *testing.T) {
	tm, _ := time.Parse(dateFormat, "2026-07-10 20:00")
	want := storage.CashSessionRecord{
		CashSession: parsers.CashSession{
			Date: tm, Stake: "NL10", Hands: 50, DurationMin: 100,
			NetUSD: -5.25, RakeUSD: 2.5, BB100: -10.5, VPIP: 24.5, PFR: 18.2,
		},
		SessionID: "abc123def456",
	}
	rows := buildCashRows([]storage.CashSessionRecord{want})
	got, ok := parseCashRow(viaWire(rows[0]))
	if !ok {
		t.Fatal("parseCashRow returned ok=false")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cash round-trip differs:\n want %+v\n  got %+v", want, got)
	}
}

func TestBuildTimeRows_RoundTrip(t *testing.T) {
	tm, _ := time.Parse(dateFormat, "2026-07-10 20:00")
	want := storage.TimeAnalysisRecord{
		Date: tm, Hour: 20, DayOfWeek: "Friday", NetUSD: -5.25, SessionID: "abc123def456",
	}
	rows := buildTimeRows([]storage.TimeAnalysisRecord{want})
	got, ok := parseTimeRow(viaWire(rows[0]))
	if !ok {
		t.Fatal("parseTimeRow returned ok=false")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("time round-trip differs:\n want %+v\n  got %+v", want, got)
	}
}

// TestParseRowGuards_ShortRowsSkipped pins the pre-refactor behavior: a
// hand-edited row with a valid date but fewer cells than the script ever
// writes is skipped, not parsed into a zeroed record that would feed /stats
// and the migration snapshot. Script-written rows are never short — the
// Sheets API only omits trailing empty cells and every writer ends rows with
// a non-empty cell — so these guards only re-exclude corrupt rows.
func TestParseRowGuards_ShortRowsSkipped(t *testing.T) {
	if _, ok := parseCashRow([]interface{}{"2026-07-01 20:00", "NL10", float64(50)}); ok {
		t.Error("parseCashRow accepted a 3-cell row")
	}
	if _, ok := parseTimeRow([]interface{}{"2026-07-01 20:00", float64(20)}); ok {
		t.Error("parseTimeRow accepted a 2-cell row")
	}
	// The shortest legitimate rows — 9-column pre-v1.4 cash rows and 4-column
	// pre-v1.4 time rows (no Session ID yet) — still parse.
	if _, ok := parseCashRow(viaWire([]interface{}{"2026-07-01 20:00", "NL10", 50, 100, -5.0, 2.5, -10.0, 24.5, 18.2})); !ok {
		t.Error("parseCashRow rejected a 9-cell pre-v1.4 row")
	}
	if _, ok := parseTimeRow(viaWire([]interface{}{"2026-07-01 20:00", 20, "Friday", -5.0})); !ok {
		t.Error("parseTimeRow rejected a 4-cell pre-v1.4 row")
	}
}

// TestParseSheetDate covers both serializations a Date cell can arrive in: the
// plain text the tracker writes, and the serial-number float64 Sheets returns
// when the column has been formatted as a Date/Time (the bug that crashed
// /status with "parsing time \"46218.61319444444\"").
func TestParseSheetDate(t *testing.T) {
	want, _ := time.Parse(dateFormat, "2026-07-17 16:49")

	// String form — the normal case.
	if got, err := parseSheetDate("2026-07-17 16:49"); err != nil || !got.Equal(want) {
		t.Errorf("string form: got %v, err %v; want %v", got, err, want)
	}

	// Serial form — same instant expressed as days-since-1899-12-30. Computed
	// the way Sheets would, then fed back to prove the round-trip.
	serial := float64(want.Sub(sheetsEpoch)) / float64(24*time.Hour)
	if got, err := parseSheetDate(serial); err != nil || !got.Equal(want) {
		t.Errorf("serial form: got %v, err %v; want %v (serial %.10f)", got, err, want, serial)
	}

	// The exact value from the crash must parse (to some sane mid-2026 date)
	// rather than error.
	got, err := parseSheetDate(46218.61319444444)
	if err != nil {
		t.Fatalf("crash value errored: %v", err)
	}
	if got.Year() != 2026 || got.Month() != time.July {
		t.Errorf("crash value parsed to %v, want a July 2026 date", got)
	}

	// A blank/garbage cell still errors, so callers keep skipping it.
	if _, err := parseSheetDate(""); err == nil {
		t.Error("empty cell should still error")
	}
}

func floatPtr(f float64) *float64 { return &f }
func intPtr(i int) *int           { return &i }
