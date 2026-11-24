package hardstop

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestSessionLossExceeds(t *testing.T) {
	cases := []struct {
		name       string
		lossCents  money.Cents
		buyInCents money.Cents
		threshold  float64
		want       bool
	}{
		{"under threshold", 400, 500, 1, false},
		{"exactly at threshold does not fire", 500, 500, 1, false},
		{"just over threshold fires", 501, 500, 1, true},
		{"well over threshold fires", 1158, 500, 1, true},
		{"under a higher threshold", 1158, 500, 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := money.LossExceeds(-c.lossCents, c.buyInCents, c.threshold)
			if got != c.want {
				t.Errorf("money.LossExceeds(loss=%v, buyIn=%v, threshold=%v) = %v, want %v",
					c.lossCents, c.buyInCents, c.threshold, got, c.want)
			}
		})
	}
}

func TestConsecutiveLossStreak(t *testing.T) {
	entriesOf := func(amountsCents ...money.Cents) []storage.LedgerEntry {
		entries := make([]storage.LedgerEntry, len(amountsCents))
		for i, aCents := range amountsCents {
			entries[i] = storage.LedgerEntry{Type: "cash_session", AmountCents: aCents}
		}
		return entries
	}

	cases := []struct {
		name         string
		amountsCents []money.Cents
		want         int
	}{
		{"all losses", []money.Cents{-100, -200, -300}, 3},
		{"streak broken by a win", []money.Cents{-100, 200, -300}, 1},
		{"all wins", []money.Cents{100, 200, 300}, 0},
		{"win in the middle, two trailing losses", []money.Cents{-100, -200, 100, -300, -400}, 2},
		{"empty", []money.Cents{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := consecutiveLossStreak(entriesOf(c.amountsCents...))
			if got != c.want {
				t.Errorf("consecutiveLossStreak(%v) = %d, want %d", c.amountsCents, got, c.want)
			}
		})
	}
}

func TestDailyLossExceeds(t *testing.T) {
	entriesOf := func(netsCents ...money.Cents) []storage.LedgerEntry {
		entries := make([]storage.LedgerEntry, len(netsCents))
		for i, nCents := range netsCents {
			entries[i] = storage.LedgerEntry{Type: "cash_session", AmountCents: nCents}
		}
		return entries
	}

	cases := []struct {
		name           string
		netsCents      []money.Cents
		thresholdCents money.Cents
		want           bool
	}{
		{"net loss equals threshold, does not fire", []money.Cents{-50, -50}, 100, false},
		{"net loss exceeds threshold, fires", []money.Cents{-50, -50, -50}, 100, true},
		{"big win offsets losses into net profit", []money.Cents{-100, -100, 400}, 100, false},
		{"small win doesn't fully offset losses", []money.Cents{-100, 25, -100}, 100, true},
		{"net loss exceeds a smaller threshold", []money.Cents{-50, 25, -50}, 50, true},
		{"no entries today", []money.Cents{}, 100, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DailyLossExceeds(entriesOf(c.netsCents...), c.thresholdCents)
			if got != c.want {
				t.Errorf("DailyLossExceeds(nets=%v, threshold=%v) = %v, want %v",
					c.netsCents, c.thresholdCents, got, c.want)
			}
		})
	}
}

func TestSpingoOnly(t *testing.T) {
	tournaments := []parsers.Tournament{
		{Type: "spingo", NetCents: -100},
		{Type: "spingo" + parsers.KnockoutSuffix, NetCents: -200},
		{Type: "mtt", NetCents: -300},
		{Type: "mtt" + parsers.KnockoutSuffix, NetCents: -400},
	}
	got := SpingoOnly(tournaments)
	if len(got) != 2 {
		t.Fatalf("len(SpingoOnly(...)) = %d, want 2", len(got))
	}
	if got[0].NetCents != -100 || got[1].NetCents != -200 {
		t.Errorf("SpingoOnly(...) = %+v, want the two spingo/spingo-ko rows only", got)
	}
}

func TestSpingoDailyLossExceeds(t *testing.T) {
	tournamentsOf := func(netsCents ...money.Cents) []parsers.Tournament {
		tourns := make([]parsers.Tournament, len(netsCents))
		for i, nCents := range netsCents {
			tourns[i] = parsers.Tournament{Type: "spingo", NetCents: nCents}
		}
		return tourns
	}

	cases := []struct {
		name           string
		netsCents      []money.Cents
		thresholdCents money.Cents
		want           bool
	}{
		{"net loss equals threshold, does not fire", []money.Cents{-50, -50}, 100, false},
		{"net loss exceeds threshold, fires", []money.Cents{-50, -50, -50}, 100, true},
		{"big win offsets losses into net profit", []money.Cents{-100, -100, 400}, 100, false},
		{"no spingo tournaments today", []money.Cents{}, 100, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SpingoDailyLossExceeds(tournamentsOf(c.netsCents...), c.thresholdCents)
			if got != c.want {
				t.Errorf("SpingoDailyLossExceeds(nets=%v, threshold=%v) = %v, want %v",
					c.netsCents, c.thresholdCents, got, c.want)
			}
		})
	}
}

func TestDailyLossIndependence(t *testing.T) {
	cases := []struct {
		name              string
		cashNetCents      money.Cents
		spingoNetCents    money.Cents
		spingoThreshCents money.Cents
		dailyThreshCents  money.Cents
		wantSpingoFire    bool
		wantDailyFire     bool
	}{
		{"spingo fires despite a net-positive day", 1000, -50, 40, 10000, true, false},
		{"neither fires, spingo under its own threshold", 1000, -30, 40, 10000, false, false},
		{"neither fires, daily threshold effectively disabled", -5000, -30, 40, 10000, false, false},
		{"spingo fires even though daily threshold is nowhere close", -5000, -50, 40, 10000, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spingoEntries := []parsers.Tournament{{Type: "spingo", NetCents: c.spingoNetCents}}
			if got := SpingoDailyLossExceeds(spingoEntries, c.spingoThreshCents); got != c.wantSpingoFire {
				t.Errorf("SpingoDailyLossExceeds(spingoNet=%v, threshold=%v) = %v, want %v",
					c.spingoNetCents, c.spingoThreshCents, got, c.wantSpingoFire)
			}

			dailyEntries := []storage.LedgerEntry{
				{Type: "cash_session", AmountCents: c.cashNetCents},
				{Type: "tournament", AmountCents: c.spingoNetCents},
			}
			if got := DailyLossExceeds(dailyEntries, c.dailyThreshCents); got != c.wantDailyFire {
				t.Errorf("DailyLossExceeds(cashNet=%v, spingoNet=%v, threshold=%v) = %v, want %v",
					c.cashNetCents, c.spingoNetCents, c.dailyThreshCents, got, c.wantDailyFire)
			}
		})
	}
}

func TestRollingPeak(t *testing.T) {
	entriesOf := func(amountsCents ...money.Cents) []storage.LedgerEntry {
		entries := make([]storage.LedgerEntry, len(amountsCents))
		for i, aCents := range amountsCents {
			entries[i] = storage.LedgerEntry{Type: "cash_session", AmountCents: aCents}
		}
		return entries
	}

	cases := []struct {
		name               string
		amountsCents       []money.Cents
		wantPeakCents      money.Cents
		wantCurrentCents   money.Cents
		wantDownswingCents money.Cents
	}{
		{"peak in the middle of the curve", []money.Cents{1000, -300, 500, -800}, 1200, 400, 800},
		{"peak at the very start, before any session", []money.Cents{-100, -200, -300}, 0, -600, 600},
		{"peak at the end, no downswing", []money.Cents{100, 200, 300}, 600, 600, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			peakCents, currentCents := ComputeEquityCurve(entriesOf(c.amountsCents...))
			if peakCents != c.wantPeakCents || currentCents != c.wantCurrentCents {
				t.Errorf("ComputeEquityCurve(%v) = (peak=%v, current=%v), want (peak=%v, current=%v)",
					c.amountsCents, peakCents, currentCents, c.wantPeakCents, c.wantCurrentCents)
			}
			if downswingCents := peakCents - currentCents; downswingCents != c.wantDownswingCents {
				t.Errorf("peak-current = %v, want downswing %v", downswingCents, c.wantDownswingCents)
			}
		})
	}
}

func TestDownswingExcludesTransfers(t *testing.T) {
	cases := []struct {
		name               string
		entries            []storage.LedgerEntry
		wantPeakCents      money.Cents
		wantCurrentCents   money.Cents
		wantDownswingCents money.Cents
	}{
		{
			"deposit in window is ignored",
			[]storage.LedgerEntry{
				{Type: "cash_session", AmountCents: 1000},
				{Type: "deposit", AmountCents: 10000},
				{Type: "cash_session", AmountCents: -300},
			},
			1000, 700, 300,
		},
		{
			"withdrawal in window is ignored",
			[]storage.LedgerEntry{
				{Type: "cash_session", AmountCents: -500},
				{Type: "withdrawal", AmountCents: -5000},
				{Type: "cash_session", AmountCents: -500},
			},
			0, -1000, 1000,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			peakCents, currentCents := ComputeEquityCurve(sessionOnly(c.entries))
			if peakCents != c.wantPeakCents || currentCents != c.wantCurrentCents {
				t.Errorf("got peak=%v current=%v, want peak=%v current=%v", peakCents, currentCents, c.wantPeakCents, c.wantCurrentCents)
			}
			if downswingCents := peakCents - currentCents; downswingCents != c.wantDownswingCents {
				t.Errorf("peak-current = %v, want downswing %v", downswingCents, c.wantDownswingCents)
			}
		})
	}
}

func TestComputeEquityCurve(t *testing.T) {
	entriesOf := func(amountsCents ...money.Cents) []storage.LedgerEntry {
		entries := make([]storage.LedgerEntry, len(amountsCents))
		for i, aCents := range amountsCents {
			entries[i] = storage.LedgerEntry{Type: "cash_session", AmountCents: aCents}
		}
		return entries
	}

	cases := []struct {
		name             string
		amountsCents     []money.Cents
		wantPeakCents    money.Cents
		wantCurrentCents money.Cents
	}{
		{"peak in the middle", []money.Cents{1000, -300, 500, -800}, 1200, 400},
		{"all wins, peak at the end", []money.Cents{500, 500, 500}, 1500, 1500},
		{"all losses, peak stays at the starting point", []money.Cents{-500, -500, -500}, 0, -1500},
		{"empty, curve never leaves the starting point", []money.Cents{}, 0, 0},
		{"recovery after a big loss doesn't reach the earlier peak", []money.Cents{1000, -1500, 300}, 1000, -200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			peakCents, currentCents := ComputeEquityCurve(entriesOf(c.amountsCents...))
			if peakCents != c.wantPeakCents || currentCents != c.wantCurrentCents {
				t.Errorf("ComputeEquityCurve(%v) = (peak=%v, current=%v), want (peak=%v, current=%v)",
					c.amountsCents, peakCents, currentCents, c.wantPeakCents, c.wantCurrentCents)
			}
		})
	}
}

func TestDownswingEquityCurve(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.Local)
	cutoff := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	dated := func(entryType string, amountCents money.Cents, date time.Time) storage.LedgerEntry {
		return storage.LedgerEntry{Type: entryType, AmountCents: amountCents, Date: date}
	}
	entries := []storage.LedgerEntry{
		dated("cash_session", 5000, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
		dated("cash_session", 1000, cutoff),
		dated("cash_session", 1000, cutoff.Add(time.Minute)),
		dated("deposit", 10000, time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)),
		dated("cash_session", -300, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)),
		dated("withdrawal", -4000, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)),
		dated("tournament", -200, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)),
	}

	peakCents, currentCents := DownswingEquityCurve(entries, now)
	if peakCents != 1000 || currentCents != 500 {
		t.Errorf("DownswingEquityCurve(...) = (peak=%v, current=%v), want (peak=10, current=5)", peakCents, currentCents)
	}
}

func TestHistoricalLedgerOrderForLossProtection(t *testing.T) {
	asOf := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	win := storage.LedgerEntry{Date: asOf.Add(-time.Hour), Type: "cash_session", AmountCents: 1000}
	loss := storage.LedgerEntry{Date: asOf, Type: "tournament", AmountCents: -800}
	tiedWin := win
	tiedWin.Date = loss.Date
	transfer := storage.LedgerEntry{Date: asOf.Add(time.Minute), Type: "withdrawal", AmountCents: -5000}

	cases := []struct {
		name        string
		entries     []storage.LedgerEntry
		wantPeak    money.Cents
		wantTrigger bool
	}{
		{"older win inserted after loss", []storage.LedgerEntry{loss, win}, 1000, true},
		{"withdrawal stays outside results", []storage.LedgerEntry{loss, transfer, win}, 1000, true},
		{"equal timestamps retain ledger order", []storage.LedgerEntry{loss, tiedWin}, 200, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := tc.entries[0]
			peakCents, currentCents := DownswingEquityCurve(tc.entries, asOf.Add(time.Hour))
			if peakCents != tc.wantPeak || currentCents != 200 {
				t.Fatalf("equity curve = (%d, %d), want (%d, 200)", peakCents, currentCents, tc.wantPeak)
			}
			if got := LosingStreakTriggered(tc.entries, 1); got != tc.wantTrigger {
				t.Fatalf("streak triggered = %t, want %t", got, tc.wantTrigger)
			}
			if tc.entries[0] != first {
				t.Fatal("calculation reordered the input ledger")
			}
		})
	}
}

func TestPokerDaySessionEntries(t *testing.T) {
	cfg := &config.Config{}
	now := time.Date(2026, 7, 13, 15, 0, 0, 0, time.Local)
	dayStart := time.Date(2026, 7, 13, 6, 0, 0, 0, time.UTC)

	dated := func(entryType string, amountCents money.Cents, date time.Time) storage.LedgerEntry {
		return storage.LedgerEntry{Type: entryType, AmountCents: amountCents, Date: date}
	}
	entries := []storage.LedgerEntry{
		dated("cash_session", -500, dayStart.Add(-time.Minute)),
		dated("deposit", 10000, dayStart.Add(2*time.Hour)),
		dated("cash_session", -300, dayStart),
		dated("tournament", -200, dayStart.Add(6*time.Hour)),
	}

	got := PokerDaySessionEntries(entries, cfg, now)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].AmountCents != -300 || got[1].AmountCents != -200 {
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

func lockWriter(lockPath string, ch chan string) func(reason string, durationHours int) {
	return func(reason string, durationHours int) {
		lock, msg, _ := BuildTrigger(reason, durationHours, time.Now().UTC())
		_ = config.SaveLock(lockPath, &lock)
		if ch != nil {
			ch <- msg
		}
	}
}

func TestEvaluateTriggers(t *testing.T) {
	duration := 12
	stored := storage.AsStoredClock(time.Now())
	sessionRow := func(amountCents money.Cents, age time.Duration) storage.LedgerEntry {
		return storage.LedgerEntry{Type: "cash_session", AmountCents: amountCents, Date: stored.Add(-age)}
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

	losingSession := parsers.CashSession{BuyInCents: 500, NetCents: -1200}
	calmSession := parsers.CashSession{BuyInCents: 500, NetCents: 0}

	cases := []struct {
		name           string
		cfg            *config.Config
		session        parsers.CashSession
		ledger         []storage.LedgerEntry
		dayTournaments []parsers.Tournament
		wantReason     string
	}{
		{
			name:    "session loss is not evaluated by Evaluate",
			cfg:     &config.Config{Hardstop: config.Hardstop{SessionLossBuyins: pf(1), DurationHours: pi(duration)}},
			session: losingSession,
		},
		{
			name:       "consecutive losses",
			cfg:        &config.Config{Hardstop: config.Hardstop{ConsecutiveLosses: pi(2), DurationHours: pi(duration)}},
			session:    calmSession,
			ledger:     []storage.LedgerEntry{sessionRow(-300, 2*time.Hour), sessionRow(-200, time.Hour)},
			wantReason: "consecutive_losses",
		},
		{
			name:       "downswing",
			cfg:        &config.Config{Hardstop: config.Hardstop{DownswingBuyins: pf(2), DurationHours: pi(duration)}},
			session:    calmSession,
			ledger:     []storage.LedgerEntry{sessionRow(10000, 2*time.Hour), sessionRow(-3000, time.Hour)},
			wantReason: "downswing_buyins",
		},
		{
			name:           "spin & go daily loss",
			cfg:            &config.Config{Hardstop: config.Hardstop{SpingoDailyLossCents: centsPtr(100), DurationHours: pi(duration)}},
			session:        calmSession,
			dayTournaments: []parsers.Tournament{{Type: "spingo", NetCents: -60}, {Type: "spingo", NetCents: -60}},
			wantReason:     "spingo_daily_loss_cents",
		},
		{
			name:       "unified daily loss",
			cfg:        &config.Config{Hardstop: config.Hardstop{DailyLossCents: centsPtr(500), DurationHours: pi(duration)}},
			session:    calmSession,
			ledger:     []storage.LedgerEntry{sessionRow(-300, 2*time.Hour), sessionRow(-300, time.Hour)},
			wantReason: "daily_loss_cents",
		},
		{
			name:    "nothing configured never triggers",
			cfg:     &config.Config{},
			session: losingSession,
			ledger:  []storage.LedgerEntry{sessionRow(-10000, time.Hour)},
		},
		{
			name:    "thresholds not breached",
			cfg:     &config.Config{Hardstop: config.Hardstop{SessionLossBuyins: pf(10), DailyLossCents: centsPtr(50000), DurationHours: pi(duration)}},
			session: losingSession,
			ledger:  []storage.LedgerEntry{sessionRow(-1200, time.Hour)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lockPath := filepath.Join(t.TempDir(), "lockfile.json")
			ch := make(chan string, 2)
			if err := Evaluate(c.session, c.cfg, c.ledger, c.dayTournaments, time.Now(), lockWriter(lockPath, ch)); err != nil {
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
		ledger := []storage.LedgerEntry{sessionRow(10000, 2*time.Hour), sessionRow(-3000, time.Hour)}
		if err := EvaluateTournament(parsers.Tournament{BuyInCents: 500}, cfg, ledger, nil, time.Now(), lockWriter(lockPath, ch)); err != nil {
			t.Fatalf("EvaluateTournament: %v", err)
		}
		assertLocked(t, lockPath, "downswing_buyins", ch)
	})

	t.Run("breach with duration_hours unset errors instead of locking", func(t *testing.T) {
		lockPath := filepath.Join(t.TempDir(), "lockfile.json")
		ch := make(chan string, 2)
		cfg := &config.Config{Hardstop: config.Hardstop{DownswingBuyins: pf(2)}}
		ledger := []storage.LedgerEntry{sessionRow(10000, 2*time.Hour), sessionRow(-3000, time.Hour)}
		err := Evaluate(calmSession, cfg, ledger, nil, time.Now(), lockWriter(lockPath, ch))
		if err == nil {
			t.Fatal("Evaluate = nil error, want the missing-duration error")
		}
		if lock, _ := config.LoadLock(lockPath); lock != nil {
			t.Errorf("lockfile written despite missing duration_hours: %+v", lock)
		}
	})
}

func TestEvaluateSessionLoss(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	pi := func(v int) *int { return &v }
	duration := 12

	cases := []struct {
		name     string
		cfg      *config.Config
		session  parsers.CashSession
		wantLock bool
		wantErr  bool
	}{
		{
			name:     "breach locks",
			cfg:      &config.Config{Hardstop: config.Hardstop{SessionLossBuyins: pf(1), DurationHours: pi(duration)}},
			session:  parsers.CashSession{BuyInCents: 500, NetCents: -1200},
			wantLock: true,
		},
		{
			name:    "within threshold does not lock",
			cfg:     &config.Config{Hardstop: config.Hardstop{SessionLossBuyins: pf(10), DurationHours: pi(duration)}},
			session: parsers.CashSession{BuyInCents: 500, NetCents: -1200},
		},
		{
			name:    "loss exactly at the threshold does not lock",
			cfg:     &config.Config{Hardstop: config.Hardstop{SessionLossBuyins: pf(1), DurationHours: pi(duration)}},
			session: parsers.CashSession{BuyInCents: 500, NetCents: -500},
		},
		{
			name:     "zero buy-in makes any loss exceed the zero threshold",
			cfg:      &config.Config{Hardstop: config.Hardstop{SessionLossBuyins: pf(1), DurationHours: pi(duration)}},
			session:  parsers.CashSession{BuyInCents: 0, NetCents: -1},
			wantLock: true,
		},
		{
			name:    "winning session does not lock",
			cfg:     &config.Config{Hardstop: config.Hardstop{SessionLossBuyins: pf(1), DurationHours: pi(duration)}},
			session: parsers.CashSession{BuyInCents: 500, NetCents: 2000},
		},
		{
			name:    "unset threshold does not lock",
			cfg:     &config.Config{Hardstop: config.Hardstop{DurationHours: pi(duration)}},
			session: parsers.CashSession{BuyInCents: 500, NetCents: -1200},
		},
		{
			name:    "breach without duration errors",
			cfg:     &config.Config{Hardstop: config.Hardstop{SessionLossBuyins: pf(1)}},
			session: parsers.CashSession{BuyInCents: 500, NetCents: -1200},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lockPath := filepath.Join(t.TempDir(), "lockfile.json")
			ch := make(chan string, 1)
			err := EvaluateSessionLoss(c.session, c.cfg, lockWriter(lockPath, ch))
			if c.wantErr {
				if err == nil {
					t.Fatal("EvaluateSessionLoss = nil error, want the missing-duration error")
				}
				if lock, _ := config.LoadLock(lockPath); lock != nil {
					t.Errorf("lockfile written despite missing duration_hours: %+v", lock)
				}
				return
			}
			if err != nil {
				t.Fatalf("EvaluateSessionLoss: %v", err)
			}
			lock, _ := config.LoadLock(lockPath)
			if c.wantLock {
				if lock == nil || lock.Reason != "session_loss_buyins" {
					t.Errorf("lock = %+v, want session_loss_buyins", lock)
				}
			} else if lock != nil {
				t.Errorf("lockfile = %+v, want none", lock)
			}
		})
	}
}

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
		if err := ManualTrigger(configPath, nil, lockWriter(lockPath, ch)); err != nil {
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

		if err := ManualTrigger(configPath, nil, lockWriter(lockPath, make(chan string, 1))); err == nil {
			t.Fatal("ManualTrigger = nil error, want the not-configured error")
		}
		if lock, _ := config.LoadLock(lockPath); lock != nil {
			t.Errorf("lockfile written despite missing duration_hours: %+v", lock)
		}
	})
}

func TestBuildTrigger(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	lock, alert, _ := BuildTrigger("session_loss_buyins", 12, now)

	if lock.Reason != "session_loss_buyins" || lock.DurationHours != 12 {
		t.Errorf("lock = reason %q / %dh, want session_loss_buyins / 12h", lock.Reason, lock.DurationHours)
	}
	if !lock.TriggeredAt.Equal(now) {
		t.Errorf("TriggeredAt = %v, want %v", lock.TriggeredAt, now)
	}
	if want := now.Add(12 * time.Hour); !lock.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", lock.ExpiresAt, want)
	}
	if !strings.Contains(alert, "Hard Stop triggered") || !strings.Contains(alert, reasonPhrase("session_loss_buyins")) {
		t.Errorf("alert = %q, want it to name the hard stop and the reason phrase", alert)
	}
}

func TestReasonPhrase(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"session_loss_buyins", "Session loss limit reached"},
		{"consecutive_losses", "Too many losing sessions in a row"},
		{"downswing_buyins", "30-day downswing limit reached"},
		{"daily_loss_cents", "Daily loss limit reached"},
		{"spingo_daily_loss_cents", "Spin & Go daily loss limit reached"},
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

func centsPtr(valueCents money.Cents) *money.Cents { return &valueCents }
