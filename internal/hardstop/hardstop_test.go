package hardstop

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestSessionLossExceeds(t *testing.T) {
	cases := []struct {
		name      string
		loss      float64
		buyInUSD  float64
		threshold float64
		want      bool
	}{
		{"under threshold", 4.00, 5.00, 1, false},
		{"exactly at threshold does not fire", 5.00, 5.00, 1, false},
		{"just over threshold fires", 5.01, 5.00, 1, true},
		{"well over threshold fires", 11.58, 5.00, 1, true},
		{"under a higher threshold", 11.58, 5.00, 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sessionLossExceeds(c.loss, c.buyInUSD, c.threshold)
			if got != c.want {
				t.Errorf("sessionLossExceeds(loss=%.2f, buyIn=%.2f, threshold=%.0f) = %v, want %v",
					c.loss, c.buyInUSD, c.threshold, got, c.want)
			}
		})
	}
}

func TestConsecutiveLossStreak(t *testing.T) {
	entriesOf := func(amounts ...float64) []storage.LedgerEntry {
		entries := make([]storage.LedgerEntry, len(amounts))
		for i, a := range amounts {
			entries[i] = storage.LedgerEntry{Type: "cash_session", AmountUSD: a}
		}
		return entries
	}

	cases := []struct {
		name    string
		amounts []float64
		want    int
	}{
		{"all losses", []float64{-1, -2, -3}, 3},
		{"streak broken by a win", []float64{-1, 2, -3}, 1},
		{"all wins", []float64{1, 2, 3}, 0},
		{"win in the middle, two trailing losses", []float64{-1, -2, 1, -3, -4}, 2},
		{"empty", []float64{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := consecutiveLossStreak(entriesOf(c.amounts...))
			if got != c.want {
				t.Errorf("consecutiveLossStreak(%v) = %d, want %d", c.amounts, got, c.want)
			}
		})
	}
}

func TestDailyLossExceeds(t *testing.T) {
	entriesOf := func(nets ...float64) []storage.LedgerEntry {
		entries := make([]storage.LedgerEntry, len(nets))
		for i, n := range nets {
			entries[i] = storage.LedgerEntry{Type: "cash_session", AmountUSD: n}
		}
		return entries
	}

	cases := []struct {
		name      string
		nets      []float64
		threshold float64
		want      bool
	}{
		{"net loss equals threshold, does not fire", []float64{-0.50, -0.50}, 1.00, false},
		{"net loss exceeds threshold, fires", []float64{-0.50, -0.50, -0.50}, 1.00, true},
		{"big win offsets losses into net profit", []float64{-1.00, -1.00, 4.00}, 1.00, false},
		{"small win doesn't fully offset losses", []float64{-1.00, 0.25, -1.00}, 1.00, true},
		{"net loss exceeds a smaller threshold", []float64{-0.50, 0.25, -0.50}, 0.50, true},
		{"no entries today", []float64{}, 1.00, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DailyLossExceeds(entriesOf(c.nets...), c.threshold)
			if got != c.want {
				t.Errorf("DailyLossExceeds(nets=%v, threshold=%.2f) = %v, want %v",
					c.nets, c.threshold, got, c.want)
			}
		})
	}
}

// TestSpingoOnly confirms a knockout Spin & Go ("spingo-ko") still counts
// toward the Spin & Go daily-loss hard stop, the same as a non-knockout one
// — evaluateDailyLoss filters by BaseType() specifically so a knockout
// tournament isn't silently excluded from this check.
func TestSpingoOnly(t *testing.T) {
	tournaments := []parsers.Tournament{
		{Type: "spingo", NetUSD: -1},
		{Type: "spingo" + parsers.KnockoutSuffix, NetUSD: -2},
		{Type: "mtt", NetUSD: -3},
		{Type: "mtt" + parsers.KnockoutSuffix, NetUSD: -4},
	}
	got := SpingoOnly(tournaments)
	if len(got) != 2 {
		t.Fatalf("len(SpingoOnly(...)) = %d, want 2", len(got))
	}
	if got[0].NetUSD != -1 || got[1].NetUSD != -2 {
		t.Errorf("SpingoOnly(...) = %+v, want the two spingo/spingo-ko rows only", got)
	}
}

func TestSpingoDailyLossExceeds(t *testing.T) {
	tournamentsOf := func(nets ...float64) []parsers.Tournament {
		tourns := make([]parsers.Tournament, len(nets))
		for i, n := range nets {
			tourns[i] = parsers.Tournament{Type: "spingo", NetUSD: n}
		}
		return tourns
	}

	cases := []struct {
		name      string
		nets      []float64
		threshold float64
		want      bool
	}{
		{"net loss equals threshold, does not fire", []float64{-0.50, -0.50}, 1.00, false},
		{"net loss exceeds threshold, fires", []float64{-0.50, -0.50, -0.50}, 1.00, true},
		{"big win offsets losses into net profit", []float64{-1.00, -1.00, 4.00}, 1.00, false},
		{"no spingo tournaments today", []float64{}, 1.00, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SpingoDailyLossExceeds(tournamentsOf(c.nets...), c.threshold)
			if got != c.want {
				t.Errorf("SpingoDailyLossExceeds(nets=%v, threshold=%.2f) = %v, want %v",
					c.nets, c.threshold, got, c.want)
			}
		})
	}
}

// TestDailyLossIndependence confirms the Spin & Go-specific check and the
// unified all-game-types check are evaluated independently — a winning cash
// session must not mask a Spin & Go-specific breach, and vice versa.
func TestDailyLossIndependence(t *testing.T) {
	cases := []struct {
		name           string
		cashNet        float64
		spingoNet      float64
		spingoThresh   float64
		dailyThresh    float64
		wantSpingoFire bool
		wantDailyFire  bool
	}{
		{"spingo fires despite a net-positive day", 10.00, -0.50, 0.40, 100, true, false},
		{"neither fires, spingo under its own threshold", 10.00, -0.30, 0.40, 100, false, false},
		{"neither fires, daily threshold effectively disabled", -50.00, -0.30, 0.40, 100, false, false},
		{"spingo fires even though daily threshold is nowhere close", -50.00, -0.50, 0.40, 100, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spingoEntries := []parsers.Tournament{{Type: "spingo", NetUSD: c.spingoNet}}
			if got := SpingoDailyLossExceeds(spingoEntries, c.spingoThresh); got != c.wantSpingoFire {
				t.Errorf("SpingoDailyLossExceeds(spingoNet=%.2f, threshold=%.2f) = %v, want %v",
					c.spingoNet, c.spingoThresh, got, c.wantSpingoFire)
			}

			dailyEntries := []storage.LedgerEntry{
				{Type: "cash_session", AmountUSD: c.cashNet},
				{Type: "tournament", AmountUSD: c.spingoNet},
			}
			if got := DailyLossExceeds(dailyEntries, c.dailyThresh); got != c.wantDailyFire {
				t.Errorf("DailyLossExceeds(cashNet=%.2f, spingoNet=%.2f, threshold=%.2f) = %v, want %v",
					c.cashNet, c.spingoNet, c.dailyThresh, got, c.wantDailyFire)
			}
		})
	}
}

// TestRollingPeak confirms the downswing peak is the highest point reached
// on the results-only equity curve (cumulative AmountUSD), not the highest
// RunningTotal — RunningTotal includes deposits/withdrawals and would read
// a large withdrawal as an equally large downswing (see
// TestDownswingExcludesTransfers for that specific case).
func TestRollingPeak(t *testing.T) {
	entriesOf := func(amounts ...float64) []storage.LedgerEntry {
		entries := make([]storage.LedgerEntry, len(amounts))
		for i, a := range amounts {
			entries[i] = storage.LedgerEntry{Type: "cash_session", AmountUSD: a}
		}
		return entries
	}

	cases := []struct {
		name          string
		amounts       []float64
		wantPeak      float64
		wantCurrent   float64
		wantDownswing float64
	}{
		{"peak in the middle of the curve", []float64{10, -3, 5, -8}, 12, 4, 8},
		{"peak at the very start, before any session", []float64{-1, -2, -3}, 0, -6, 6},
		{"peak at the end, no downswing", []float64{1, 2, 3}, 6, 6, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			peak, current := ComputeEquityCurve(entriesOf(c.amounts...))
			if peak != c.wantPeak || current != c.wantCurrent {
				t.Errorf("ComputeEquityCurve(%v) = (peak=%v, current=%v), want (peak=%v, current=%v)",
					c.amounts, peak, current, c.wantPeak, c.wantCurrent)
			}
			if downswing := peak - current; downswing != c.wantDownswing {
				t.Errorf("peak-current = %v, want downswing %v", downswing, c.wantDownswing)
			}
		})
	}
}

// TestDownswingExcludesTransfers confirms deposit/withdrawal rows never
// enter the equity curve computed by evaluateDownswing — sessionOnly must
// filter them out before ComputeEquityCurve ever sees them.
func TestDownswingExcludesTransfers(t *testing.T) {
	cases := []struct {
		name          string
		entries       []storage.LedgerEntry
		wantPeak      float64
		wantCurrent   float64
		wantDownswing float64
	}{
		{
			"deposit in window is ignored",
			[]storage.LedgerEntry{
				{Type: "cash_session", AmountUSD: 10},
				{Type: "deposit", AmountUSD: 100},
				{Type: "cash_session", AmountUSD: -3},
			},
			10, 7, 3,
		},
		{
			"withdrawal in window is ignored",
			[]storage.LedgerEntry{
				{Type: "cash_session", AmountUSD: -5},
				{Type: "withdrawal", AmountUSD: -50},
				{Type: "cash_session", AmountUSD: -5},
			},
			0, -10, 10,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			peak, current := ComputeEquityCurve(sessionOnly(c.entries))
			if peak != c.wantPeak || current != c.wantCurrent {
				t.Errorf("got peak=%v current=%v, want peak=%v current=%v", peak, current, c.wantPeak, c.wantCurrent)
			}
			if downswing := peak - current; downswing != c.wantDownswing {
				t.Errorf("peak-current = %v, want downswing %v", downswing, c.wantDownswing)
			}
		})
	}
}

// TestComputeEquityCurve exercises the peak/current equity-curve math in
// isolation, independent of any type filtering or windowing.
func TestComputeEquityCurve(t *testing.T) {
	entriesOf := func(amounts ...float64) []storage.LedgerEntry {
		entries := make([]storage.LedgerEntry, len(amounts))
		for i, a := range amounts {
			entries[i] = storage.LedgerEntry{Type: "cash_session", AmountUSD: a}
		}
		return entries
	}

	cases := []struct {
		name        string
		amounts     []float64
		wantPeak    float64
		wantCurrent float64
	}{
		{"peak in the middle", []float64{10, -3, 5, -8}, 12, 4},
		{"all wins, peak at the end", []float64{5, 5, 5}, 15, 15},
		{"all losses, peak stays at the starting point", []float64{-5, -5, -5}, 0, -15},
		{"empty, curve never leaves the starting point", []float64{}, 0, 0},
		{"recovery after a big loss doesn't reach the earlier peak", []float64{10, -15, 3}, 10, -2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			peak, current := ComputeEquityCurve(entriesOf(c.amounts...))
			if peak != c.wantPeak || current != c.wantCurrent {
				t.Errorf("ComputeEquityCurve(%v) = (peak=%v, current=%v), want (peak=%v, current=%v)",
					c.amounts, peak, current, c.wantPeak, c.wantCurrent)
			}
		})
	}
}

// TestDownswingEquityCurve confirms the data-taking curve — which windows a
// full-ledger fetch locally — produces exactly what the old per-evaluator
// windowed fetch did: the 30-day boundary stays exclusive, transfers stay
// out of the curve, and rows older than the window never contribute to the
// peak. now is constructed in time.Local so its wall-clock digits are fixed
// regardless of the machine's zone (see TestPokerDayStart_DefaultHour);
// entry dates are constructed directly in UTC, the stored-date coordinate.
func TestDownswingEquityCurve(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.Local)
	cutoff := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) // now's digits minus 30 days

	dated := func(entryType string, amount float64, date time.Time) storage.LedgerEntry {
		return storage.LedgerEntry{Type: entryType, AmountUSD: amount, Date: date}
	}
	entries := []storage.LedgerEntry{
		dated("cash_session", 50, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)), // outside window
		dated("cash_session", 10, cutoff),                                       // exactly at cutoff — excluded
		dated("cash_session", 10, cutoff.Add(time.Minute)),                      // just inside
		dated("deposit", 100, time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)),    // transfer — excluded
		dated("cash_session", -3, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)),
		dated("withdrawal", -40, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)), // transfer — excluded
		dated("tournament", -2, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)),
	}

	// Curve over the three included rows (+10, -3, -2): 10 → 7 → 5.
	peak, current := DownswingEquityCurve(entries, now)
	if peak != 10 || current != 5 {
		t.Errorf("DownswingEquityCurve(...) = (peak=%v, current=%v), want (peak=10, current=5)", peak, current)
	}
}

// TestPokerDaySessionEntries confirms the data-taking poker-day filter —
// applied to a full-ledger fetch — matches the old windowed
// GetLedgerEntriesSince fetch: the day-start boundary is inclusive, rows
// before it are dropped, and transfers inside the day are excluded.
func TestPokerDaySessionEntries(t *testing.T) {
	cfg := &config.Config{} // default 6am day start
	now := time.Date(2026, 7, 13, 15, 0, 0, 0, time.Local)
	dayStart := time.Date(2026, 7, 13, 6, 0, 0, 0, time.UTC)

	dated := func(entryType string, amount float64, date time.Time) storage.LedgerEntry {
		return storage.LedgerEntry{Type: entryType, AmountUSD: amount, Date: date}
	}
	entries := []storage.LedgerEntry{
		dated("cash_session", -5, dayStart.Add(-time.Minute)), // previous poker day — dropped
		dated("deposit", 100, dayStart.Add(2*time.Hour)),      // in the day but a transfer — dropped
		dated("cash_session", -3, dayStart),                   // exactly at the boundary — kept (inclusive)
		dated("tournament", -2, dayStart.Add(6*time.Hour)),
	}

	got := PokerDaySessionEntries(entries, cfg, now)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].AmountUSD != -3 || got[1].AmountUSD != -2 {
		t.Errorf("got = %+v, want the boundary session (-3) and the tournament (-2)", got)
	}
}

func TestInRollingWindow(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)

	cases := []struct {
		name    string
		rowDate time.Time
		want    bool
	}{
		{"29 days 23 hours ago is included", now.Add(-29*24*time.Hour - 23*time.Hour), true},
		{"exactly 30 days ago is excluded", now.Add(-30 * 24 * time.Hour), false},
		{"30 days and 1 second ago is excluded", now.Add(-30*24*time.Hour - time.Second), false},
		{"1 hour ago is included", now.Add(-1 * time.Hour), true},
		{"31 days ago is excluded", now.Add(-31 * 24 * time.Hour), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := inRollingWindow(c.rowDate, cutoff)
			if got != c.want {
				t.Errorf("inRollingWindow(%v, cutoff=%v) = %v, want %v", c.rowDate, cutoff, got, c.want)
			}
		})
	}
}

func TestLockfileActiveAt(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	writeLockfile := func(t *testing.T, contents string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "lockfile.json")
		if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
			t.Fatalf("writing lockfile: %v", err)
		}
		return path
	}

	t.Run("absent file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "lockfile.json")
		if got := lockfileActiveAt(path, now); got != false {
			t.Errorf("lockfileActiveAt(absent) = %v, want false", got)
		}
	})

	t.Run("expires in the future", func(t *testing.T) {
		path := writeLockfile(t, `{
			"triggered_at": "2026-07-13T11:00:00Z",
			"expires_at": "2026-07-13T13:00:00Z",
			"reason": "manual",
			"duration_hours": 2
		}`)
		if got := lockfileActiveAt(path, now); got != true {
			t.Errorf("lockfileActiveAt(future expiry) = %v, want true", got)
		}
	})

	t.Run("expires in the past", func(t *testing.T) {
		path := writeLockfile(t, `{
			"triggered_at": "2026-07-13T09:00:00Z",
			"expires_at": "2026-07-13T11:00:00Z",
			"reason": "manual",
			"duration_hours": 2
		}`)
		if got := lockfileActiveAt(path, now); got != false {
			t.Errorf("lockfileActiveAt(past expiry) = %v, want false", got)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		path := writeLockfile(t, `{not valid json`)
		if got := lockfileActiveAt(path, now); got != false {
			t.Errorf("lockfileActiveAt(malformed) = %v, want false", got)
		}
	})
}
