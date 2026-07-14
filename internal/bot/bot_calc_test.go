package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
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

func TestWeightedBB100(t *testing.T) {
	cases := []struct {
		name     string
		sessions []parsers.CashSession
		want     float64
	}{
		{"zero hands total returns 0", []parsers.CashSession{{Hands: 0, BB100: 25}}, 0},
		{"no sessions returns 0", nil, 0},
		{"single session returns its own BB100", []parsers.CashSession{{Hands: 100, BB100: 10}}, 10},
		{
			"weighted average across sessions of different sizes",
			[]parsers.CashSession{
				{Hands: 100, BB100: 10},
				{Hands: 300, BB100: -2},
			},
			(100*10 + 300*-2) / 400.0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := weightedBB100(c.sessions)
			if got != c.want {
				t.Errorf("weightedBB100(%+v) = %v, want %v", c.sessions, got, c.want)
			}
		})
	}
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
			got := formatSignedUSD(c.input)
			if got != c.want {
				t.Errorf("formatSignedUSD(%v) = %q, want %q", c.input, got, c.want)
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
			got := ordinal(c.input)
			if got != c.want {
				t.Errorf("ordinal(%d) = %q, want %q", c.input, got, c.want)
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

	t.Run("week is roughly 7 days ago", func(t *testing.T) {
		now := time.Now()
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
