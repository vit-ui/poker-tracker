package hardstop

import (
	"os"
	"path/filepath"
	"strings"
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

// TestEvaluateTriggers drives the full trigger path — Evaluate through
// trigger to the lockfile and alert — for each hard stop reason, against a
// real temp lockfile. The taskkill.exe call inside trigger fails harmlessly
// on a non-Windows test machine and its error is ignored by design, so the
// observable outcomes are exactly the lockfile contents and the alert
// message. Ledger dates are recent stored-clock instants so the downswing
// window and poker-day filters see them.
func TestEvaluateTriggers(t *testing.T) {
	duration := 12
	stored := storage.AsStoredClock(time.Now())
	sessionRow := func(amount float64, age time.Duration) storage.LedgerEntry {
		return storage.LedgerEntry{Type: "cash_session", AmountUSD: amount, Date: stored.Add(-age)}
	}

	assertLocked := func(t *testing.T, lockPath, wantReason string, ch chan string) {
		t.Helper()
		lock, err := config.LoadLock(lockPath)
		if err != nil {
			t.Fatalf("LoadLock: %v", err)
		}
		if lock == nil {
			t.Fatal("no lockfile written, want a hard stop")
		}
		if lock.Reason != wantReason || lock.DurationHours != duration {
			t.Errorf("lock = reason %q / %dh, want %q / %dh", lock.Reason, lock.DurationHours, wantReason, duration)
		}
		if want := lock.TriggeredAt.Add(12 * time.Hour); !lock.ExpiresAt.Equal(want) {
			t.Errorf("ExpiresAt = %v, want TriggeredAt+12h (%v)", lock.ExpiresAt, want)
		}
		select {
		case msg := <-ch:
			// The lockfile keeps the raw reason key; the alert shows its
			// humanized phrase (reasonPhrase).
			wantPhrase := reasonPhrase(wantReason)
			if !strings.Contains(msg, "Hard Stop triggered") || !strings.Contains(msg, wantPhrase) {
				t.Errorf("alert = %q, want it to name the hard stop and reason %q", msg, wantPhrase)
			}
		default:
			t.Error("no alert sent for the hard stop")
		}
	}

	pf := func(v float64) *float64 { return &v }
	pi := func(v int) *int { return &v }

	losingSession := parsers.CashSession{BuyInUSD: 5, NetUSD: -12}
	calmSession := parsers.CashSession{BuyInUSD: 5, NetUSD: 0}

	cases := []struct {
		name           string
		cfg            *config.Config
		session        parsers.CashSession
		ledger         []storage.LedgerEntry
		dayTournaments []parsers.Tournament
		wantReason     string // "" = must not trigger
	}{
		{
			// Loss 12 > 1 × $5 buy-in.
			name:       "session loss",
			cfg:        &config.Config{Hardstop: config.Hardstop{SessionLossBuyins: pf(1), DurationHours: pi(duration)}},
			session:    losingSession,
			wantReason: "session_loss_buyins",
		},
		{
			name:       "consecutive losses",
			cfg:        &config.Config{Hardstop: config.Hardstop{ConsecutiveLosses: pi(2), DurationHours: pi(duration)}},
			session:    calmSession,
			ledger:     []storage.LedgerEntry{sessionRow(-3, 2*time.Hour), sessionRow(-2, time.Hour)},
			wantReason: "consecutive_losses",
		},
		{
			// Curve +100 → +70: drawdown 30 > 2 × $5.
			name:       "downswing",
			cfg:        &config.Config{Hardstop: config.Hardstop{DownswingBuyins: pf(2), DurationHours: pi(duration)}},
			session:    calmSession,
			ledger:     []storage.LedgerEntry{sessionRow(100, 2*time.Hour), sessionRow(-30, time.Hour)},
			wantReason: "downswing_buyins",
		},
		{
			// Spin net -1.20 < -$1.00.
			name:           "spin & go daily loss",
			cfg:            &config.Config{Hardstop: config.Hardstop{SpingoDailyLossUSD: pf(1), DurationHours: pi(duration)}},
			session:        calmSession,
			dayTournaments: []parsers.Tournament{{Type: "spingo", NetUSD: -0.60}, {Type: "spingo", NetUSD: -0.60}},
			wantReason:     "spingo_daily_loss_usd",
		},
		{
			// Day net -6 < -$5.
			name:       "unified daily loss",
			cfg:        &config.Config{Hardstop: config.Hardstop{DailyLossUSD: pf(5), DurationHours: pi(duration)}},
			session:    calmSession,
			ledger:     []storage.LedgerEntry{sessionRow(-3, 2*time.Hour), sessionRow(-3, time.Hour)},
			wantReason: "daily_loss_usd",
		},
		{
			name:    "nothing configured never triggers",
			cfg:     &config.Config{},
			session: losingSession,
			ledger:  []storage.LedgerEntry{sessionRow(-100, time.Hour)},
		},
		{
			name:    "thresholds not breached",
			cfg:     &config.Config{Hardstop: config.Hardstop{SessionLossBuyins: pf(10), DailyLossUSD: pf(500), DurationHours: pi(duration)}},
			session: losingSession,
			ledger:  []storage.LedgerEntry{sessionRow(-12, time.Hour)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lockPath := filepath.Join(t.TempDir(), "lockfile.json")
			ch := make(chan string, 2)
			if err := Evaluate(c.session, c.cfg, c.ledger, c.dayTournaments, lockPath, ch); err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if c.wantReason == "" {
				if lock, err := config.LoadLock(lockPath); err != nil || lock != nil {
					t.Errorf("lockfile = %v (err %v), want none", lock, err)
				}
				if got := len(ch); got != 0 {
					t.Errorf("%d alerts sent, want none", got)
				}
				return
			}
			assertLocked(t, lockPath, c.wantReason, ch)
		})
	}

	t.Run("EvaluateTournament triggers the downswing check", func(t *testing.T) {
		lockPath := filepath.Join(t.TempDir(), "lockfile.json")
		ch := make(chan string, 2)
		cfg := &config.Config{Hardstop: config.Hardstop{DownswingBuyins: pf(2), DurationHours: pi(duration)}}
		ledger := []storage.LedgerEntry{sessionRow(100, 2*time.Hour), sessionRow(-30, time.Hour)}
		if err := EvaluateTournament(parsers.Tournament{BuyInUSD: 5}, cfg, ledger, nil, lockPath, ch); err != nil {
			t.Fatalf("EvaluateTournament: %v", err)
		}
		assertLocked(t, lockPath, "downswing_buyins", ch)
	})

	t.Run("breach with duration_hours unset errors instead of locking", func(t *testing.T) {
		lockPath := filepath.Join(t.TempDir(), "lockfile.json")
		ch := make(chan string, 2)
		cfg := &config.Config{Hardstop: config.Hardstop{DownswingBuyins: pf(2)}}
		ledger := []storage.LedgerEntry{sessionRow(100, 2*time.Hour), sessionRow(-30, time.Hour)}
		err := Evaluate(calmSession, cfg, ledger, nil, lockPath, ch)
		if err == nil {
			t.Fatal("Evaluate = nil error, want the missing-duration error")
		}
		if lock, _ := config.LoadLock(lockPath); lock != nil {
			t.Errorf("lockfile written despite missing duration_hours: %+v", lock)
		}
	})
}

// TestManualTrigger covers /hardstop trigger's path: it loads the duration
// from a real config.json and refuses when duration_hours was never set.
func TestManualTrigger(t *testing.T) {
	t.Run("locks for the configured duration with reason manual", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		lockPath := filepath.Join(dir, "lockfile.json")
		duration := 8
		if err := config.SaveConfig(configPath, &config.Config{Hardstop: config.Hardstop{DurationHours: &duration}}); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}

		ch := make(chan string, 1)
		if err := ManualTrigger(configPath, lockPath, ch); err != nil {
			t.Fatalf("ManualTrigger: %v", err)
		}
		lock, err := config.LoadLock(lockPath)
		if err != nil || lock == nil {
			t.Fatalf("LoadLock = %v, %v — want an active lock", lock, err)
		}
		if lock.Reason != "manual" || lock.DurationHours != 8 {
			t.Errorf("lock = reason %q / %dh, want manual / 8h", lock.Reason, lock.DurationHours)
		}
	})

	t.Run("errors when duration_hours is not configured", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		lockPath := filepath.Join(dir, "lockfile.json")
		if err := config.SaveConfig(configPath, &config.Config{}); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}

		if err := ManualTrigger(configPath, lockPath, make(chan string, 1)); err == nil {
			t.Fatal("ManualTrigger = nil error, want the not-configured error")
		}
		if lock, _ := config.LoadLock(lockPath); lock != nil {
			t.Errorf("lockfile written despite missing duration_hours: %+v", lock)
		}
	})
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

// TestReasonPhrase pins the lockfile-key → user-facing phrase mapping used in
// the hard stop alert. Every trigger reason must map to a phrase, and an
// unknown key must fall back to itself (never blank).
func TestReasonPhrase(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"session_loss_buyins", "Session loss limit reached"},
		{"consecutive_losses", "Too many losing sessions in a row"},
		{"downswing_buyins", "30-day downswing limit reached"},
		{"daily_loss_usd", "Daily loss limit reached"},
		{"spingo_daily_loss_usd", "Spin & Go daily loss limit reached"},
		{"manual", "Triggered manually"},
		{"some_future_reason", "some_future_reason"},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			if got := reasonPhrase(c.reason); got != c.want {
				t.Errorf("reasonPhrase(%q) = %q, want %q", c.reason, got, c.want)
			}
		})
	}
}
