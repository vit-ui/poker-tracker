package bot

// Tests for stats.go: the pure display/aggregation helpers (prize-pool text,
// hands-weighted averages, ROI, formatters, summary-schedule windows) and the
// store-reading summary builders behind /stats, /status, and the weekly/
// monthly summaries, driven against a real sqlite store. The pure-helper
// tests were moved verbatim from the former bot_calc_test.go so test files
// mirror the package's file-per-concern split.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// TestSpinGoPrizePoolText covers both the real-cash-pool and
// ticket-denominated-pool cases — the second is the one that used to crash
// main.go's post-session alert by dereferencing a nil Multiplier.
func TestSpinGoPrizePoolText(t *testing.T) {
	mult := 3.0
	cases := []struct {
		name  string
		tourn parsers.Tournament
		want  string
	}{
		{
			"real cash prize pool",
			parsers.Tournament{PrizePoolUSD: 3.00, Multiplier: &mult},
			"$3.00 (3x)",
		},
		{
			"ticket-denominated prize pool — no real dollar amount or multiplier",
			parsers.Tournament{PrizePoolDescription: "$1.00 Power Path Step 2 Ticket"},
			"$1.00 Power Path Step 2 Ticket",
		},
		{
			"real cash prize pool but nil Multiplier defensively doesn't panic",
			parsers.Tournament{PrizePoolUSD: 3.00, Multiplier: nil},
			"$3.00 (0x)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SpinGoPrizePoolText(c.tourn); got != c.want {
				t.Errorf("SpinGoPrizePoolText(%+v) = %q, want %q", c.tourn, got, c.want)
			}
		})
	}
}

// TestWeightedCashAverages covers weightedCashAverages' shared weighting
// loop for BB/100, VPIP, and PFR together — a 10-hand session must not count
// the same as a 200-hand one, for any of the three.
func TestWeightedCashAverages(t *testing.T) {
	cases := []struct {
		name      string
		sessions  []parsers.CashSession
		wantBB100 float64
		wantVPIP  float64
		wantPFR   float64
	}{
		{"zero hands total returns 0, 0, 0", []parsers.CashSession{{Hands: 0, BB100: 25, VPIP: 30, PFR: 20}}, 0, 0, 0},
		{"no sessions returns 0, 0, 0", nil, 0, 0, 0},
		{
			"single session returns its own values",
			[]parsers.CashSession{{Hands: 100, BB100: 10, VPIP: 24, PFR: 18}},
			10, 24, 18,
		},
		{
			"weighted average across sessions of different sizes",
			[]parsers.CashSession{
				{Hands: 10, BB100: 50, VPIP: 80, PFR: 70},  // a small, loose sample session
				{Hands: 200, BB100: -2, VPIP: 20, PFR: 15}, // a large, representative session
			},
			(10*50 + 200*-2) / 210.0,
			(10*80 + 200*20) / 210.0,
			(10*70 + 200*15) / 210.0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotBB100, gotVPIP, gotPFR := weightedCashAverages(c.sessions)
			if gotBB100 != c.wantBB100 || gotVPIP != c.wantVPIP || gotPFR != c.wantPFR {
				t.Errorf("weightedCashAverages(%+v) = (%v, %v, %v), want (%v, %v, %v)",
					c.sessions, gotBB100, gotVPIP, gotPFR, c.wantBB100, c.wantVPIP, c.wantPFR)
			}
		})
	}

	t.Run("a 10-hand session does not count the same as a 200-hand session", func(t *testing.T) {
		// If hands weren't weighted, a plain average of 80 and 20 would be 50 —
		// far from the hands-weighted true value, which sits close to the
		// 200-hand session's own 20 since it dominates the sample.
		_, gotVPIP, _ := weightedCashAverages([]parsers.CashSession{
			{Hands: 10, VPIP: 80},
			{Hands: 200, VPIP: 20},
		})
		if gotVPIP >= 25 {
			t.Errorf("weightedCashAverages gave VPIP = %v, want close to 20 (hands-weighted), not near the unweighted average of 50", gotVPIP)
		}
	})
}

// TestWeightedAverages covers the generic core both weightedCashAverages and
// weightedTournamentAverages are built on — the ok=false exclusion path
// neither of those two exercises by itself is what makes a tournament
// without a hand file parsed yet correctly drop out of the average instead
// of counting as a 0-hand (and therefore zero-weight, but present) item.
func TestWeightedAverages(t *testing.T) {
	t.Run("no items returns zero values, not a panic", func(t *testing.T) {
		got := weightedAverages([]int{}, 2, func(int) (float64, []float64, bool) { return 0, nil, false })
		if len(got) != 2 || got[0] != 0 || got[1] != 0 {
			t.Errorf("weightedAverages(empty) = %v, want [0 0]", got)
		}
	})

	t.Run("excluded items (ok=false) are dropped, not counted as zero-weight", func(t *testing.T) {
		type item struct {
			hands      float64
			val        float64
			handFileIn bool
		}
		items := []item{
			{hands: 100, val: 50, handFileIn: true},
			{hands: 999, val: 0, handFileIn: false}, // excluded — would swamp the average as a 0 if counted
		}
		got := weightedAverages(items, 1, func(it item) (float64, []float64, bool) {
			if !it.handFileIn {
				return 0, nil, false
			}
			return it.hands, []float64{it.val}, true
		})
		if got[0] != 50 {
			t.Errorf("weightedAverages = %v, want [50] (the excluded item must not pull the average toward 0)", got[0])
		}
	})

	t.Run("all items excluded returns zero values", func(t *testing.T) {
		got := weightedAverages([]int{1, 2, 3}, 2, func(int) (float64, []float64, bool) { return 0, nil, false })
		if len(got) != 2 || got[0] != 0 || got[1] != 0 {
			t.Errorf("weightedAverages(all excluded) = %v, want [0 0]", got)
		}
	})
}

// TestWeightedTournamentAverages mirrors TestWeightedCashAverages for
// tournaments — a tournament whose hand file hasn't arrived yet (Hands/
// VPIP/PFR all nil) must be excluded entirely, not counted as a 0-hand
// (zero-weight) tournament.
func TestWeightedTournamentAverages(t *testing.T) {
	handsFor := func(n int) *int { return &n }
	pctFor := func(v float64) *float64 { return &v }

	cases := []struct {
		name        string
		tournaments []parsers.Tournament
		wantVPIP    float64
		wantPFR     float64
	}{
		{"no tournaments returns 0, 0", nil, 0, 0},
		{
			"hand file not yet parsed is excluded, not zero",
			[]parsers.Tournament{{Hands: nil, VPIP: nil, PFR: nil}},
			0, 0,
		},
		{
			"single tournament returns its own values",
			[]parsers.Tournament{{Hands: handsFor(50), VPIP: pctFor(24), PFR: pctFor(18)}},
			24, 18,
		},
		{
			"weighted average across tournaments of different hand counts",
			[]parsers.Tournament{
				{Hands: handsFor(10), VPIP: pctFor(80), PFR: pctFor(70)},
				{Hands: handsFor(200), VPIP: pctFor(20), PFR: pctFor(15)},
			},
			(10*80 + 200*20) / 210.0,
			(10*70 + 200*15) / 210.0,
		},
		{
			"a tournament missing its hand file is excluded from an otherwise-weighted set",
			[]parsers.Tournament{
				{Hands: handsFor(100), VPIP: pctFor(30), PFR: pctFor(20)},
				{Hands: nil, VPIP: nil, PFR: nil}, // summary arrived, hand file hasn't — excluded
			},
			30, 20,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotVPIP, gotPFR := weightedTournamentAverages(c.tournaments)
			if gotVPIP != c.wantVPIP || gotPFR != c.wantPFR {
				t.Errorf("weightedTournamentAverages(%+v) = (%v, %v), want (%v, %v)",
					c.tournaments, gotVPIP, gotPFR, c.wantVPIP, c.wantPFR)
			}
		})
	}
}

func TestFormatPercent(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{24.3, "24.3%"},
		{0, "0.0%"},
		{88.0 / 347.0 * 100, "25.4%"}, // raw value 25.360230547550433, rounds to 1dp
	}
	for _, c := range cases {
		if got := formatPercent(c.v); got != c.want {
			t.Errorf("formatPercent(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestFormatPercentPtr(t *testing.T) {
	t.Run("nil renders N/A, not a misleading 0.0%", func(t *testing.T) {
		if got := formatPercentPtr(nil); got != "N/A" {
			t.Errorf("formatPercentPtr(nil) = %q, want %q", got, "N/A")
		}
	})
	t.Run("non-nil renders the value, including zero", func(t *testing.T) {
		v := 0.0
		if got := formatPercentPtr(&v); got != "0.0%" {
			t.Errorf("formatPercentPtr(&0) = %q, want %q", got, "0.0%")
		}
	})
}

func TestROI(t *testing.T) {
	cases := []struct {
		name        string
		tournaments []parsers.Tournament
		tType       string
		want        float64
	}{
		{"no buy-ins returns 0", nil, "spingo", 0},
		{
			"filters by type",
			[]parsers.Tournament{
				{Type: "spingo", NetUSD: 5, BuyInUSD: 10},
				{Type: "mtt", NetUSD: 100, BuyInUSD: 5},
			},
			"spingo",
			50,
		},
		{
			"net loss produces negative ROI",
			[]parsers.Tournament{{Type: "mtt", NetUSD: -3, BuyInUSD: 6}},
			"mtt",
			-50,
		},
		{
			"all buy-ins zero across matching type returns 0",
			[]parsers.Tournament{{Type: "spingo", NetUSD: 5, BuyInUSD: 0}},
			"spingo",
			0,
		},
		{
			"knockout suffix still counts as its base type",
			[]parsers.Tournament{
				{Type: "mtt" + parsers.KnockoutSuffix, NetUSD: 5, BuyInUSD: 10},
				{Type: "spingo", NetUSD: 100, BuyInUSD: 5},
			},
			"mtt",
			50,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := roi(c.tournaments, c.tType)
			if got != c.want {
				t.Errorf("roi(%+v, %q) = %v, want %v", c.tournaments, c.tType, got, c.want)
			}
		})
	}
}

func TestFormatSignedUSD(t *testing.T) {
	cases := []struct {
		name  string
		input float64
		want  string
	}{
		{"positive", 12.5, "+$12.50"},
		{"negative", -12.5, "-$12.50"},
		{"zero is shown with a plus sign", 0, "+$0.00"},
		{"rounds to two decimals", 1.005, "+$1.00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FormatSignedUSD(c.input)
			if got != c.want {
				t.Errorf("FormatSignedUSD(%v) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestFormatSignedPercent(t *testing.T) {
	cases := []struct {
		name  string
		input float64
		want  string
	}{
		{"positive", 15.4, "+15%"},
		{"negative", -15.4, "-15%"},
		{"zero", 0, "+0%"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatSignedPercent(c.input)
			if got != c.want {
				t.Errorf("formatSignedPercent(%v) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestCommas(t *testing.T) {
	cases := []struct {
		name  string
		input int
		want  string
	}{
		{"zero", 0, "0"},
		{"under a thousand, no comma", 999, "999"},
		{"exactly a thousand", 1000, "1,000"},
		{"four digits", 4201, "4,201"},
		{"millions, two commas", 1234567, "1,234,567"},
		{"negative number", -4201, "-4,201"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := commas(c.input)
			if got != c.want {
				t.Errorf("commas(%d) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestOrdinal(t *testing.T) {
	cases := []struct {
		input int
		want  string
	}{
		{1, "1st"}, {2, "2nd"}, {3, "3rd"}, {4, "4th"},
		{11, "11th"}, {12, "12th"}, {13, "13th"},
		{21, "21st"}, {22, "22nd"}, {23, "23rd"},
		{100, "100th"}, {101, "101st"}, {111, "111th"}, {113, "113th"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			got := Ordinal(c.input)
			if got != c.want {
				t.Errorf("Ordinal(%d) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestSameDay(t *testing.T) {
	cases := []struct {
		name string
		a, b time.Time
		want bool
	}{
		{"same instant", time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC), true},
		{"same day, different times", time.Date(2026, 7, 13, 0, 0, 1, 0, time.UTC), time.Date(2026, 7, 13, 23, 59, 0, 0, time.UTC), true},
		{"different days", time.Date(2026, 7, 13, 23, 59, 0, 0, time.UTC), time.Date(2026, 7, 14, 0, 0, 1, 0, time.UTC), false},
		{"same day-of-year, different years", time.Date(2025, 7, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC), false},
		{"zero values", time.Time{}, time.Time{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sameDay(c.a, c.b)
			if got != c.want {
				t.Errorf("sameDay(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestInSummaryWindow(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"09:00:00 is in window", time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC), true},
		{"09:01:59 is in window", time.Date(2026, 7, 13, 9, 1, 59, 0, time.UTC), true},
		{"09:02:00 is outside the window", time.Date(2026, 7, 13, 9, 2, 0, 0, time.UTC), false},
		{"08:59:59 is outside the window", time.Date(2026, 7, 13, 8, 59, 59, 0, time.UTC), false},
		{"10:00:00 is outside the window", time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := inSummaryWindow(c.now)
			if got != c.want {
				t.Errorf("inSummaryWindow(%v) = %v, want %v", c.now, got, c.want)
			}
		})
	}
}

// TestStatsSince checks structural properties against an independently
// computed "now" rather than asserting on exact timestamps, since statsSince
// calls time.Now() internally — a day-boundary race between the two calls is
// theoretically possible but not what these assertions depend on.
func TestStatsSince(t *testing.T) {
	t.Run("today truncates to midnight of the current day", func(t *testing.T) {
		now := time.Now()
		got := statsSince("today", now)
		if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
			t.Errorf("statsSince(today) = %v, want midnight", got)
		}
		if got.Year() != now.Year() || got.YearDay() != now.YearDay() {
			t.Errorf("statsSince(today) = %v, want same day as %v", got, now)
		}
	})

	t.Run("month truncates to the 1st of the current month", func(t *testing.T) {
		now := time.Now()
		got := statsSince("month", now)
		if got.Day() != 1 {
			t.Errorf("statsSince(month).Day() = %d, want 1", got.Day())
		}
		if got.Month() != now.Month() || got.Year() != now.Year() {
			t.Errorf("statsSince(month) = %v, want current month/year", got)
		}
	})

	t.Run("week is roughly 7 days ago in the stored-date coordinate system", func(t *testing.T) {
		// statsSince computes windows via storage.AsStoredClock (local
		// wall-clock digits read as UTC), so the reference point must be
		// converted the same way — comparing against a real local-zone
		// instant would be off by the machine's UTC offset.
		now := storage.AsStoredClock(time.Now())
		got := statsSince("week", time.Now())
		want := now.AddDate(0, 0, -7)
		if diff := got.Sub(want); diff < -5*time.Second || diff > 5*time.Second {
			t.Errorf("statsSince(week) = %v, want approximately %v", got, want)
		}
	})

	t.Run("all-time (empty period) returns the zero time", func(t *testing.T) {
		if got := statsSince("", time.Now()); !got.IsZero() {
			t.Errorf("statsSince(\"\") = %v, want zero time", got)
		}
	})

	t.Run("unrecognized period returns the zero time", func(t *testing.T) {
		if got := statsSince("bogus", time.Now()); !got.IsZero() {
			t.Errorf("statsSince(bogus) = %v, want zero time", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Store-reading summary builders — the money numbers the player reads in
// /stats and the weekly/monthly summaries, against a real sqlite store.
// ---------------------------------------------------------------------------

// newStatsStore returns an empty sqlite store in a temp directory.
func newStatsStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	store, err := sqlitestore.NewStore(filepath.Join(t.TempDir(), "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestBuildWeeklySummary hand-checks the full weekly message: 3 in-window
// sessions netting -3.00 (-3 cash, +2 spin, -2 mtt), one 10-day-old winning
// session outside the window, and an in-window deposit that must not turn
// the losing week into an apparent winning one.
func TestBuildWeeklySummary(t *testing.T) {
	store := newStatsStore(t)
	now := time.Now()
	stored := storage.AsStoredClock(now)

	if err := store.AppendCashSession(parsers.CashSession{
		Date: stored.Add(-24 * time.Hour), Stake: "NL10", Hands: 100, DurationMin: 60,
		NetUSD: -3.00, BB100: -15, VPIP: 25, PFR: 20, FirstHandID: "w1",
	}); err != nil {
		t.Fatalf("AppendCashSession(in-window): %v", err)
	}
	if err := store.AppendCashSession(parsers.CashSession{
		Date: stored.Add(-10 * 24 * time.Hour), Stake: "NL10", Hands: 500,
		NetUSD: 50.00, BB100: 30, FirstHandID: "w2",
	}); err != nil {
		t.Fatalf("AppendCashSession(outside window): %v", err)
	}
	if _, err := store.AppendTournament(parsers.Tournament{
		Date: stored.Add(-48 * time.Hour), Type: "spingo", TournamentID: "9001",
		BuyInUSD: 1.00, FinishPos: 1, TotalPlayers: 3, NetUSD: 2.00,
	}); err != nil {
		t.Fatalf("AppendTournament(spingo): %v", err)
	}
	if _, err := store.AppendTournament(parsers.Tournament{
		Date: stored.Add(-72 * time.Hour), Type: "mtt", TournamentID: "9002",
		BuyInUSD: 2.00, FinishPos: 50, TotalPlayers: 100, NetUSD: -2.00,
	}); err != nil {
		t.Fatalf("AppendTournament(mtt): %v", err)
	}
	// Dated now by AppendLedgerEntry — inside the window, excluded by type.
	if err := store.AppendLedgerEntry("deposit", 100, time.Now()); err != nil {
		t.Fatalf("AppendLedgerEntry: %v", err)
	}

	got, net, err := buildWeeklySummary(store, now)
	if err != nil {
		t.Fatalf("buildWeeklySummary: %v", err)
	}
	// Sessions: 1 in-window cash + 2 tournaments. Net: -3+2-2 = -3 (deposit
	// and the 10-day-old +50 both excluded). BB/100: the one in-window cash
	// session's -15. Spins ROI: 2/1 = +200%. MTT ROI: -2/2 = -100%.
	want := "📊 Weekly Summary\nSessions: 3\nNet: -$3.00\nBB/100: -15.0\nSpin & Go ROI: +200%\nMTT ROI: -100%\nHands: 100"
	if got != want {
		t.Errorf("buildWeeklySummary =\n%q\nwant\n%q", got, want)
	}
	if net != -3.00 {
		t.Errorf("weekly net = %v, want -3.00", net)
	}
}

// TestBuildMonthlySummary hand-checks the previous-calendar-month message
// with fully fixed dates: June 2026 holds +10 cash / -1 spin / +4 mtt, and a
// July 2 session that must stay out of June's summary.
func TestBuildMonthlySummary(t *testing.T) {
	store := newStatsStore(t)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.Local)

	if err := store.AppendCashSession(parsers.CashSession{
		Date: time.Date(2026, 6, 15, 20, 0, 0, 0, time.UTC), Stake: "NL10", Hands: 200,
		NetUSD: 10.00, BB100: 5, VPIP: 24, PFR: 18, FirstHandID: "m1",
	}); err != nil {
		t.Fatalf("AppendCashSession(June): %v", err)
	}
	if _, err := store.AppendTournament(parsers.Tournament{
		Date: time.Date(2026, 6, 20, 21, 0, 0, 0, time.UTC), Type: "spingo", TournamentID: "8001",
		BuyInUSD: 1.00, FinishPos: 3, TotalPlayers: 3, NetUSD: -1.00,
	}); err != nil {
		t.Fatalf("AppendTournament(spingo): %v", err)
	}
	if _, err := store.AppendTournament(parsers.Tournament{
		Date: time.Date(2026, 6, 25, 21, 0, 0, 0, time.UTC), Type: "mtt", TournamentID: "8002",
		BuyInUSD: 2.00, FinishPos: 3, TotalPlayers: 90, NetUSD: 4.00,
	}); err != nil {
		t.Fatalf("AppendTournament(mtt): %v", err)
	}
	if err := store.AppendCashSession(parsers.CashSession{
		Date: time.Date(2026, 7, 2, 20, 0, 0, 0, time.UTC), Stake: "NL10", Hands: 50,
		NetUSD: -99.00, BB100: -80, FirstHandID: "m2",
	}); err != nil {
		t.Fatalf("AppendCashSession(July): %v", err)
	}

	got, net, err := buildMonthlySummary(store, now)
	if err != nil {
		t.Fatalf("buildMonthlySummary: %v", err)
	}
	// June only: Cash +10, Spins -1, MTT +4, Total +13. BB/100 from the one
	// June cash session. Spins ROI -1/1 = -100%; MTT ROI 4/2 = +200%.
	want := "📅 Monthly Summary — June 2026\nCash Net: +$10.00\nSpins Net: -$1.00\nMTT Net: +$4.00\nTotal: +$13.00\nBB/100: 5.0\nSpins ROI: -100%\nMTT ROI: +200%\nHands: 200\nVolume: 3 sessions"
	if got != want {
		t.Errorf("buildMonthlySummary =\n%q\nwant\n%q", got, want)
	}
	if net != 13.00 {
		t.Errorf("monthly net = %v, want 13.00", net)
	}
}

// TestCashStatsMessage hand-checks the /stats cash section: one session
// inside the window, one before it.
func TestCashStatsMessage(t *testing.T) {
	store := newStatsStore(t)
	stored := storage.AsStoredClock(time.Now())

	if err := store.AppendCashSession(parsers.CashSession{
		Date: stored.Add(-24 * time.Hour), Stake: "NL10", Hands: 100,
		NetUSD: -3.00, BB100: -15, VPIP: 25, PFR: 20, FirstHandID: "c1",
	}); err != nil {
		t.Fatalf("AppendCashSession: %v", err)
	}
	if err := store.AppendCashSession(parsers.CashSession{
		Date: stored.Add(-10 * 24 * time.Hour), Stake: "NL10", Hands: 500,
		NetUSD: 50.00, BB100: 30, VPIP: 22, PFR: 17, FirstHandID: "c2",
	}); err != nil {
		t.Fatalf("AppendCashSession(old): %v", err)
	}

	got, err := cashStatsMessage(store, stored.AddDate(0, 0, -7), "week")
	if err != nil {
		t.Fatalf("cashStatsMessage: %v", err)
	}
	want := "📈 Cash Stats (week)\nNet: -$3.00\nBB/100: -15.0\nVPIP: 25.0%\nPFR: 20.0%\nHands: 100\nSessions: 1"
	if got != want {
		t.Errorf("cashStatsMessage =\n%q\nwant\n%q", got, want)
	}
}

// TestTournamentStatsMessage hand-checks both /stats tournament sections —
// Spin & Go without the avg-finish line, MTT with it — including the type
// filter keeping each other's rows out. VPIP/PFR render 0.0% because no hand
// file has arrived for any row (nil stats exclude the row from the weighted
// average entirely).
func TestTournamentStatsMessage(t *testing.T) {
	store := newStatsStore(t)
	date := time.Date(2026, 7, 20, 21, 0, 0, 0, time.UTC)

	seed := []parsers.Tournament{
		{Date: date, Type: "spingo", TournamentID: "7001", BuyInUSD: 1.00, FinishPos: 1, TotalPlayers: 3, NetUSD: 2.00},
		{Date: date, Type: "mtt", TournamentID: "7002", BuyInUSD: 2.00, FinishPos: 10, TotalPlayers: 100, NetUSD: -2.00},
		{Date: date, Type: "mtt", TournamentID: "7003", BuyInUSD: 2.00, FinishPos: 40, TotalPlayers: 200, NetUSD: 4.00},
	}
	for _, tt := range seed {
		if _, err := store.AppendTournament(tt); err != nil {
			t.Fatalf("AppendTournament(%s): %v", tt.TournamentID, err)
		}
	}

	t.Run("spingo, no avg finish line", func(t *testing.T) {
		got, err := tournamentStatsMessage(store, time.Time{}, "spingo", "all", "Spin & Go", "🎰", false)
		if err != nil {
			t.Fatalf("tournamentStatsMessage: %v", err)
		}
		want := "🎰 Spin & Go Stats (all)\nNet: +$2.00\nROI: +200%\nVPIP: 0.0%\nPFR: 0.0%\nGames: 1"
		if got != want {
			t.Errorf("spingo stats =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("mtt, with avg finish", func(t *testing.T) {
		got, err := tournamentStatsMessage(store, time.Time{}, "mtt", "all", "MTT", "🏆", true)
		if err != nil {
			t.Fatalf("tournamentStatsMessage: %v", err)
		}
		// Net -2+4 = +2; ROI 2/4 = +50%; avg finish (10/100 + 40/200)/2 = 15%.
		want := "🏆 MTT Stats (all)\nNet: +$2.00\nROI: +50%\nVPIP: 0.0%\nPFR: 0.0%\nGames: 2\nAvg finish: 15%"
		if got != want {
			t.Errorf("mtt stats =\n%q\nwant\n%q", got, want)
		}
	})
}

// TestNetSince confirms /status's window sum includes transfers — unlike the
// weekly/monthly summaries, /status shows bankroll movement, so a deposit
// counts.
func TestNetSince(t *testing.T) {
	store := newStatsStore(t)
	if err := store.AppendLedgerEntry("deposit", 100, time.Now()); err != nil {
		t.Fatalf("AppendLedgerEntry: %v", err)
	}
	if err := store.AppendCashSession(parsers.CashSession{
		Date: storage.AsStoredClock(time.Now()), Stake: "NL10", NetUSD: -6.00, FirstHandID: "n1",
	}); err != nil {
		t.Fatalf("AppendCashSession: %v", err)
	}

	got, err := netSince(store, time.Time{})
	if err != nil {
		t.Fatalf("netSince: %v", err)
	}
	if got != 94 {
		t.Errorf("netSince(all time) = %v, want 94 (100 deposit - 6 session)", got)
	}
}
