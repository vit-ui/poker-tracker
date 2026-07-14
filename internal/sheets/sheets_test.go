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
