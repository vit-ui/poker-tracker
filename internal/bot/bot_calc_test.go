package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
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

func TestCommaAsDecimal(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"comma decimal", "2,54", "2.54"},
		{"dot decimal unchanged", "2.54", "2.54"},
		{"leading/trailing space trimmed", "  2,54  ", "2.54"},
		{"integer, no separator", "50", "50"},
		{"dot present, comma left alone", "1,234.56", "1,234.56"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := commaAsDecimal(c.input)
			if got != c.want {
				t.Errorf("commaAsDecimal(%q) = %q, want %q", c.input, got, c.want)
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
		got := statsSince("today")
		if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
			t.Errorf("statsSince(today) = %v, want midnight", got)
		}
		if got.Year() != now.Year() || got.YearDay() != now.YearDay() {
			t.Errorf("statsSince(today) = %v, want same day as %v", got, now)
		}
	})

	t.Run("month truncates to the 1st of the current month", func(t *testing.T) {
		now := time.Now()
		got := statsSince("month")
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
		got := statsSince("week")
		want := now.AddDate(0, 0, -7)
		if diff := got.Sub(want); diff < -5*time.Second || diff > 5*time.Second {
			t.Errorf("statsSince(week) = %v, want approximately %v", got, want)
		}
	})

	t.Run("all-time (empty period) returns the zero time", func(t *testing.T) {
		if got := statsSince(""); !got.IsZero() {
			t.Errorf("statsSince(\"\") = %v, want zero time", got)
		}
	})

	t.Run("unrecognized period returns the zero time", func(t *testing.T) {
		if got := statsSince("bogus"); !got.IsZero() {
			t.Errorf("statsSince(bogus) = %v, want zero time", got)
		}
	})
}

func TestParsePositiveFloat(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    float64
		wantErr bool
	}{
		{"valid positive", "5.5", 5.5, false},
		{"valid with surrounding whitespace", "  5.5  ", 5.5, false},
		{"zero is rejected", "0", 0, true},
		{"negative is rejected", "-5", 0, true},
		{"non-numeric is rejected", "abc", 0, true},
		{"empty is rejected", "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parsePositiveFloat(c.input)
			if (err != nil) != c.wantErr {
				t.Fatalf("parsePositiveFloat(%q) error = %v, wantErr %v", c.input, err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("parsePositiveFloat(%q) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

func TestParsePositiveInt(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{"valid positive", "5", 5, false},
		{"zero is rejected", "0", 0, true},
		{"negative is rejected", "-5", 0, true},
		{"decimal is rejected (not a whole number)", "5.5", 0, true},
		{"non-numeric is rejected", "abc", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parsePositiveInt(c.input)
			if (err != nil) != c.wantErr {
				t.Fatalf("parsePositiveInt(%q) error = %v, wantErr %v", c.input, err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("parsePositiveInt(%q) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

// TestResolveSheetInput_URLBranch exercises only the URL branch of
// resolveSheetInput, which returns before ever touching serviceAccountPath
// or the network — the name-lookup branch calls sheets.ResolveSheetID (a
// live Drive API call) and is not testable without real credentials.
func TestResolveSheetInput_URLBranch(t *testing.T) {
	name, id, err := resolveSheetInput("/nonexistent/credentials.json", "https://docs.google.com/spreadsheets/d/1AbC-23_xyz/edit#gid=0")
	if err != nil {
		t.Fatalf("resolveSheetInput: %v", err)
	}
	if name != nil {
		t.Errorf("name = %v, want nil for a URL input (nothing to save as a resolved name)", *name)
	}
	if id != "1AbC-23_xyz" {
		t.Errorf("id = %q, want 1AbC-23_xyz", id)
	}
}

func TestIsASCIILetter(t *testing.T) {
	cases := []struct {
		input byte
		want  bool
	}{
		{'A', true}, {'Z', true}, {'a', true}, {'z', true},
		{'0', false}, {'9', false}, {':', false}, {'\\', false}, {' ', false},
	}
	for _, c := range cases {
		t.Run(string(c.input), func(t *testing.T) {
			got := isASCIILetter(c.input)
			if got != c.want {
				t.Errorf("isASCIILetter(%q) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

func TestWindowsPathExists(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.txt")
	if err := os.WriteFile(existing, []byte("x"), 0644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	missing := filepath.Join(dir, "does-not-exist.txt")

	if !windowsPathExists(existing) {
		t.Errorf("windowsPathExists(%q) = false, want true", existing)
	}
	if windowsPathExists(missing) {
		t.Errorf("windowsPathExists(%q) = true, want false", missing)
	}
}

func TestResolveAgainstHomeDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}

	t.Run("absolute path is returned unchanged", func(t *testing.T) {
		got, err := resolveAgainstHomeDir(`C:\Users\VITOR\Hand History`)
		if err != nil {
			t.Fatalf("resolveAgainstHomeDir: %v", err)
		}
		if got != `C:\Users\VITOR\Hand History` {
			t.Errorf("got %q, want unchanged absolute path", got)
		}
	})

	t.Run("relative path is joined with the home directory", func(t *testing.T) {
		got, err := resolveAgainstHomeDir("AppData/Local/PokerStars")
		if err != nil {
			t.Fatalf("resolveAgainstHomeDir: %v", err)
		}
		want := filepath.Join(home, "AppData/Local/PokerStars")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestDisplayRelativeToHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}

	t.Run("path under home is shortened", func(t *testing.T) {
		full := filepath.Join(home, "AppData", "Local", "PokerStars")
		got := displayRelativeToHome(full)
		want := filepath.Join("AppData", "Local", "PokerStars")
		if got != want {
			t.Errorf("displayRelativeToHome(%q) = %q, want %q", full, got, want)
		}
	})

	t.Run("path outside home is returned unchanged", func(t *testing.T) {
		outside := "/completely/unrelated/path"
		if strings.HasPrefix(outside, home) {
			t.Skip("test environment's home directory unexpectedly contains the outside path")
		}
		got := displayRelativeToHome(outside)
		if got != outside {
			t.Errorf("displayRelativeToHome(%q) = %q, want unchanged", outside, got)
		}
	})
}

func TestHelpText(t *testing.T) {
	t.Run("empty arg lists all commands", func(t *testing.T) {
		got := helpText("")
		if !strings.Contains(got, "/start") || !strings.Contains(got, "/help") {
			t.Errorf("helpText(\"\") missing expected commands: %q", got)
		}
	})

	t.Run("known command returns its detailed help", func(t *testing.T) {
		got := helpText("stats")
		if !strings.Contains(got, "/stats") {
			t.Errorf("helpText(stats) = %q, want detail mentioning /stats", got)
		}
	})

	t.Run("leading slash is stripped before lookup", func(t *testing.T) {
		got := helpText("/stats")
		if !strings.Contains(got, "/stats") {
			t.Errorf("helpText(/stats) = %q, want detail mentioning /stats", got)
		}
	})

	t.Run("unknown command", func(t *testing.T) {
		got := helpText("nosuchcommand")
		want := `No help available for "nosuchcommand".`
		if got != want {
			t.Errorf("helpText(nosuchcommand) = %q, want %q", got, want)
		}
	})

	t.Run("case-insensitive lookup", func(t *testing.T) {
		got := helpText("STATS")
		if !strings.Contains(got, "/stats") {
			t.Errorf("helpText(STATS) = %q, want detail mentioning /stats", got)
		}
	})
}

// ---------------------------------------------------------------------------
// /export CSV formatting
// ---------------------------------------------------------------------------

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

func TestTimeAnalysisCSVRows(t *testing.T) {
	date := time.Date(2026, 7, 10, 22, 30, 0, 0, time.UTC)
	entries := []storage.TimeAnalysisRecord{
		// NetUSD carries float64 noise past 2 decimal places, same shape as
		// a real chained-arithmetic result — proves rounding, not just
		// formatting a number that was already clean.
		{Date: date, Hour: 22, DayOfWeek: "Friday", NetUSD: -(0.1 + 0.42), SessionID: "abc123"},
	}
	rows := timeAnalysisCSVRows(entries)
	want := []string{"2026-07-10T22:30:00Z", "22", "Friday", "-0.52", "abc123"}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	for i, w := range want {
		if rows[0][i] != w {
			t.Errorf("rows[0][%d] = %q, want %q (row: %v)", i, rows[0][i], w, rows[0])
		}
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
