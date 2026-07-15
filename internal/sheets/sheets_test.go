package sheets

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// TestFoldBountyIntoNet exercises the summary-file side of knockout-bounty
// reconciliation: folding whatever bounty a hand history file has already
// recorded into the freshly parsed baseline net (payout - buyin).
func TestFoldBountyIntoNet(t *testing.T) {
	cases := []struct {
		name                             string
		baselineNetUSD, bountyUSD, buyIn float64
		wantNet, wantPrizeWon            float64
	}{
		{"no bounty yet — hand file hasn't arrived", -0.12, 0, 0.55, -0.12, 0.43},
		{"real KO tournament — a loss on paper becomes a win", -0.12, 0.90, 0.55, 0.78, 1.33},
		{"non-knockout tournament, bounty always zero", 2.00, 0, 5.00, 2.00, 7.00},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotNet, gotPrizeWon := foldBountyIntoNet(c.baselineNetUSD, c.bountyUSD, c.buyIn)
			if !closeEnough(gotNet, c.wantNet) || !closeEnough(gotPrizeWon, c.wantPrizeWon) {
				t.Errorf("foldBountyIntoNet(%v, %v, %v) = (%v, %v), want (%v, %v)",
					c.baselineNetUSD, c.bountyUSD, c.buyIn, gotNet, gotPrizeWon, c.wantNet, c.wantPrizeWon)
			}
		})
	}
}

// TestReconcileTournamentBounty exercises the hand-file side of knockout-
// bounty reconciliation: replacing this row's previous bounty contribution
// to an already-summary-completed row with a freshly reparsed total, without
// double-counting the invariant payout portion.
func TestReconcileTournamentBounty(t *testing.T) {
	cases := []struct {
		name                                               string
		storedNetUSD, storedBountyUSD, newBountyUSD, buyIn float64
		wantNet, wantPrizeWon                              float64
	}{
		{"bounty grows — more KOs since the last hand-file parse", 0.78, 0.90, 1.26, 0.55, 1.14, 1.69},
		{"unchanged re-parse is a no-op", 0.78, 0.90, 0.90, 0.55, 0.78, 1.33},
		{"first bounty applied after summary already landed", -0.12, 0, 0.90, 0.55, 0.78, 1.33},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotNet, gotPrizeWon := reconcileTournamentBounty(c.storedNetUSD, c.storedBountyUSD, c.newBountyUSD, c.buyIn)
			if !closeEnough(gotNet, c.wantNet) || !closeEnough(gotPrizeWon, c.wantPrizeWon) {
				t.Errorf("reconcileTournamentBounty(%v, %v, %v, %v) = (%v, %v), want (%v, %v)",
					c.storedNetUSD, c.storedBountyUSD, c.newBountyUSD, c.buyIn, gotNet, gotPrizeWon, c.wantNet, c.wantPrizeWon)
			}
		})
	}
}

// TestReconcileKnockoutType exercises the summary-file side of Type
// reconciliation: combining the freshly parsed base type with whatever
// knockout signal a hand-file-created partial row already left behind.
func TestReconcileKnockoutType(t *testing.T) {
	cases := []struct {
		name             string
		baseType         string
		existingTypeCell string
		want             string
	}{
		{"placeholder from a knockout-flagged partial row", "mtt", knockoutPlaceholder, "mtt-ko"},
		{"spingo variant", "spingo", knockoutPlaceholder, "spingo-ko"},
		{"no prior knockout signal at all", "mtt", "", "mtt"},
		{"already-suffixed row from an earlier reconcile — idempotent", "mtt", "mtt-ko", "mtt-ko"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reconcileKnockoutType(c.baseType, c.existingTypeCell); got != c.want {
				t.Errorf("reconcileKnockoutType(%q, %q) = %q, want %q", c.baseType, c.existingTypeCell, got, c.want)
			}
		})
	}
}

// TestAppendKnockoutSuffix exercises the hand-file side of Type
// reconciliation: flagging a row as knockout whether or not the base type is
// known yet, without ever double-suffixing on a repeat call.
func TestAppendKnockoutSuffix(t *testing.T) {
	cases := []struct {
		name             string
		existingTypeCell string
		want             string
	}{
		{"base type not known yet — summary hasn't arrived", "", knockoutPlaceholder},
		{"base type already known", "mtt", "mtt-ko"},
		{"spingo variant", "spingo", "spingo-ko"},
		{"already the placeholder — idempotent", knockoutPlaceholder, knockoutPlaceholder},
		{"already suffixed — idempotent", "mtt-ko", "mtt-ko"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := appendKnockoutSuffix(c.existingTypeCell); got != c.want {
				t.Errorf("appendKnockoutSuffix(%q) = %q, want %q", c.existingTypeCell, got, c.want)
			}
		})
	}
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
	_, err := NewClient(path, "some-spreadsheet-id")
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
	_, err := NewClient(path, "some-spreadsheet-id")
	if err == nil {
		t.Fatal("expected error for invalid credentials JSON")
	}
}
