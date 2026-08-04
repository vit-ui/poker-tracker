package bot

// Tests for export.go's CSV formatting: per-column precision, nil-vs-zero
// rendering, and the CSV file writer. Moved verbatim from the former
// bot_calc_test.go so test files mirror the package's file-per-concern split.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestFormatIntPtr(t *testing.T) {
	t.Run("nil renders as empty cell", func(t *testing.T) {
		if got := formatIntPtr(nil); got != "" {
			t.Errorf("formatIntPtr(nil) = %q, want empty", got)
		}
	})
	t.Run("non-nil renders the value, including zero", func(t *testing.T) {
		v := 0
		if got := formatIntPtr(&v); got != "0" {
			t.Errorf("formatIntPtr(&0) = %q, want %q", got, "0")
		}
	})
}

func TestFormatFloat(t *testing.T) {
	t.Run("fullPrecision renders the shortest exact round-trip", func(t *testing.T) {
		cases := []struct {
			v    float64
			want string
		}{
			{0, "0"},
			{-5.25, "-5.25"},
			{100, "100"},
			{0.1, "0.1"},
			{88.0 / 347.0 * 100, "25.360230547550433"},
		}
		for _, c := range cases {
			if got := formatFloat(c.v, fullPrecision); got != c.want {
				t.Errorf("formatFloat(%v, fullPrecision) = %q, want %q", c.v, got, c.want)
			}
		}
	})

	// vpipPfrPrecision covers the exact case a real VPIP calculation
	// produces — an unrounded division like 88/347*100 — to confirm /export
	// no longer emits full float64 precision for VPIP/PFR while every other
	// numeric column (covered above) is untouched.
	t.Run("vpipPfrPrecision rounds to 2 decimal places", func(t *testing.T) {
		cases := []struct {
			v    float64
			want string
		}{
			{88.0 / 347.0 * 100, "25.36"}, // raw value: 25.360230547550433
			{0, "0.00"},
			{24.5, "24.50"},
			{18.204, "18.20"},
			{24.567, "24.57"},
		}
		for _, c := range cases {
			if got := formatFloat(c.v, vpipPfrPrecision); got != c.want {
				t.Errorf("formatFloat(%v, vpipPfrPrecision) = %q, want %q", c.v, got, c.want)
			}
		}
	})
}

func TestFormatFloatPtr(t *testing.T) {
	t.Run("nil renders as empty cell regardless of precision", func(t *testing.T) {
		if got := formatFloatPtr(nil, fullPrecision); got != "" {
			t.Errorf("formatFloatPtr(nil, fullPrecision) = %q, want empty", got)
		}
		if got := formatFloatPtr(nil, vpipPfrPrecision); got != "" {
			t.Errorf("formatFloatPtr(nil, vpipPfrPrecision) = %q, want empty", got)
		}
	})
	t.Run("non-nil renders the value, including zero", func(t *testing.T) {
		v := 0.0
		if got := formatFloatPtr(&v, fullPrecision); got != "0" {
			t.Errorf("formatFloatPtr(&0, fullPrecision) = %q, want %q — zero is a real value, not absence", got, "0")
		}
	})
	t.Run("non-nil rounds to 2 decimal places", func(t *testing.T) {
		v := 88.0 / 347.0 * 100
		if got := formatFloatPtr(&v, vpipPfrPrecision); got != "25.36" {
			t.Errorf("formatFloatPtr(%v, vpipPfrPrecision) = %q, want %q", v, got, "25.36")
		}
	})
}

func TestPrizePoolCSVCell(t *testing.T) {
	t.Run("cash pool renders the numeric amount", func(t *testing.T) {
		got := prizePoolCSVCell(parsers.Tournament{PrizePoolUSD: 1.65})
		if got != "1.65" {
			t.Errorf("prizePoolCSVCell(cash) = %q, want %q", got, "1.65")
		}
	})
	t.Run("ticket-denominated pool renders the description, not 0", func(t *testing.T) {
		got := prizePoolCSVCell(parsers.Tournament{PrizePoolDescription: "$1.00 Power Path Step 2 Ticket"})
		if got != "$1.00 Power Path Step 2 Ticket" {
			t.Errorf("prizePoolCSVCell(ticket) = %q, want the description", got)
		}
	})
}

func TestCashSessionsCSVRows(t *testing.T) {
	date := time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)
	records := []storage.CashSessionRecord{
		{
			CashSession: parsers.CashSession{
				// NetUSD/RakeUSD/BB100/VPIP/PFR are all chained-arithmetic
				// results with float64 noise past their meaningful precision
				// (e.g. 0.1+0.2 = 0.30000000000000004) — same shape as real
				// running-total/bounty-folding output, to prove rounding,
				// not just formatting a number that was already round.
				Date: date, Stake: "NL10", Hands: 50, DurationMin: 60,
				NetUSD: -(0.1 + 0.2), RakeUSD: 1.0 / 3.0, BB100: 10.0 / 3.0,
				VPIP: 88.0 / 347.0 * 100, PFR: 60.0 / 347.0 * 100,
			},
			SessionID: "abc123",
		},
	}
	rows := cashSessionsCSVRows(records)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	// Net/Rake round to 2dp (money), BB/100 to 1dp, VPIP/PFR to 2dp.
	want := []string{"2026-07-10T20:00:00Z", "NL10", "50", "60", "-0.30", "0.33", "3.3", "25.36", "17.29", "abc123"}
	for i, w := range want {
		if rows[0][i] != w {
			t.Errorf("rows[0][%d] = %q, want %q (row: %v)", i, rows[0][i], w, rows[0])
		}
	}
}

func TestTournamentsCSVRows(t *testing.T) {
	date := time.Date(2026, 7, 10, 21, 0, 0, 0, time.UTC)
	vpip, pfr := 40.0, 20.0
	hands := 55

	t.Run("cash pool with hand stats and bounty", func(t *testing.T) {
		mult := 3.0
		records := []storage.TournamentRecord{
			{
				Tournament: parsers.Tournament{
					Date: date, Type: "spingo-ko", TournamentID: "1000000001", BuyInUSD: 0.55,
					PrizePoolUSD: 1.65, Multiplier: &mult, FinishPos: 1, TotalPlayers: 3,
					NetUSD: 2.00, VPIP: &vpip, PFR: &pfr, Hands: &hands,
				},
				BountyWonUSD: 0.90,
				PrizeWonUSD:  2.55,
			},
		}
		rows := tournamentsCSVRows(records)
		want := []string{"2026-07-10T21:00:00Z", "spingo-ko", "1000000001", "0.55", "1.65", "3", "1", "3", "2.00", "40.00", "20.00", "0.90", "2.55", "55"}
		if len(rows) != 1 {
			t.Fatalf("len(rows) = %d, want 1", len(rows))
		}
		for i, w := range want {
			if rows[0][i] != w {
				t.Errorf("rows[0][%d] = %q, want %q (row: %v)", i, rows[0][i], w, rows[0])
			}
		}
	})

	t.Run("ticket pool with no hand stats yet — nil fields render empty, not zero", func(t *testing.T) {
		records := []storage.TournamentRecord{
			{
				Tournament: parsers.Tournament{
					Date: date, Type: "spingo", TournamentID: "1000000002",
					PrizePoolDescription: "$1.00 Power Path Step 2 Ticket", BuyInUSD: 0.25,
					FinishPos: 1, TotalPlayers: 3, NetUSD: -0.25,
				},
			},
		}
		rows := tournamentsCSVRows(records)
		if rows[0][4] != "$1.00 Power Path Step 2 Ticket" {
			t.Errorf("Prize Pool cell = %q, want the ticket description", rows[0][4])
		}
		if rows[0][5] != "" {
			t.Errorf("Multiplier cell = %q, want empty (no real cash pool)", rows[0][5])
		}
		if rows[0][9] != "" || rows[0][10] != "" {
			t.Errorf("VPIP/PFR cells = %q/%q, want empty (hand file hasn't arrived)", rows[0][9], rows[0][10])
		}
		if rows[0][11] != "0.00" || rows[0][12] != "0.00" {
			t.Errorf("Bounty Won/Prize Won cells = %q/%q, want 0.00 (real zero values, not absence)", rows[0][11], rows[0][12])
		}
		if rows[0][13] != "" {
			t.Errorf("Hands cell = %q, want empty (hand file hasn't arrived)", rows[0][13])
		}
	})
}

func TestBankrollLedgerCSVRows(t *testing.T) {
	date := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	entries := []storage.LedgerEntry{
		// RunningTotal is a chained-arithmetic result carrying float64 noise
		// past 2 decimal places — the exact shape cascadeLedgerAmounts/
		// cascadeRunningTotals produce in real use — to prove rounding, not
		// just formatting a number that was already clean.
		{Date: date, Type: "cash_session", AmountUSD: -(0.1 + 0.42), RunningTotal: -0.32000000000000006, SessionID: "abc123"},
		{Date: date, Type: "deposit", AmountUSD: 100, RunningTotal: 100, SessionID: ""},
	}
	rows := bankrollLedgerCSVRows(entries)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0][2] != "-0.52" || rows[0][3] != "-0.32" {
		t.Errorf("session row Amount/Running Total = %q/%q, want -0.52/-0.32 (rounded to 2dp)", rows[0][2], rows[0][3])
	}
	if rows[0][4] != "abc123" {
		t.Errorf("session row Session ID = %q, want %q", rows[0][4], "abc123")
	}
	if rows[1][4] != "" {
		t.Errorf("deposit row Session ID = %q, want empty", rows[1][4])
	}
}

func TestWriteCSVFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.csv")
	header := []string{"A", "B"}
	rows := [][]string{{"1", "2"}, {"3", "4"}}
	if err := writeCSVFile(path, header, rows); err != nil {
		t.Fatalf("writeCSVFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	want := "A,B\n1,2\n3,4\n"
	if string(data) != want {
		t.Errorf("file contents = %q, want %q", string(data), want)
	}

	t.Run("empty data set still writes a header-only file", func(t *testing.T) {
		emptyPath := filepath.Join(t.TempDir(), "empty.csv")
		if err := writeCSVFile(emptyPath, header, nil); err != nil {
			t.Fatalf("writeCSVFile: %v", err)
		}
		data, err := os.ReadFile(emptyPath)
		if err != nil {
			t.Fatalf("reading written file: %v", err)
		}
		if string(data) != "A,B\n" {
			t.Errorf("file contents = %q, want header-only %q", string(data), "A,B\n")
		}
	})
}
