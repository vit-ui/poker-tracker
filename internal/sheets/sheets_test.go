package sheets

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
