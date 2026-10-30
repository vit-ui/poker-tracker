package bot

// Tests for flows.go's pure input helpers: numeric input validation, the
// comma-decimal normalization /deposit accepts, and the /paths input
// normalization helpers. Moved verbatim from the former bot_calc_test.go so
// test files mirror the package's file-per-concern split.

import (
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
		{"NaN is rejected", "NaN", 0, true},
		{"infinity is rejected", "Inf", 0, true},
		{"positive infinity is rejected", "+Inf", 0, true},
		{"overflow is rejected", "1e309", 0, true},
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

func TestParsePositiveMoney(t *testing.T) {
	for _, tc := range []struct {
		text      string
		wantCents money.Cents
		invalid   bool
	}{
		{"2,54", 254, false}, {"1.001", 101, false}, {"0.00001", 1, false},
		{"0", 0, true}, {"-1.001", 0, true}, {"NaN", 0, true}, {"Inf", 0, true}, {"1e100", 0, true},
	} {
		t.Run(tc.text, func(t *testing.T) {
			gotCents, err := parsePositiveMoney(tc.text)
			if (err != nil) != tc.invalid || gotCents != tc.wantCents {
				t.Fatalf("got %d, %v; want %d, invalid %v", gotCents, err, tc.wantCents, tc.invalid)
			}
		})
	}
}

func TestMoneyThresholdMenusDistinguishUnsetAndZero(t *testing.T) {
	zeroCents, configuredCents := money.Cents(0), money.Cents(125)
	for _, tc := range []struct {
		name       string
		valueCents *money.Cents
		want       string
	}{
		{"unset", nil, "not set"}, {"zero", &zeroCents, "0.00"}, {"configured", &configuredCents, "1.25"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Alerts: config.Alerts{DailyLossCents: tc.valueCents}, Hardstop: config.Hardstop{DailyLossCents: tc.valueCents}}
			if got := alertValueText(cfg, "daily_loss"); got != tc.want {
				t.Fatalf("alert text %q; want %q", got, tc.want)
			}
			if got := hardstopValueText(cfg, "daily_loss"); got != tc.want {
				t.Fatalf("hardstop text %q; want %q", got, tc.want)
			}
		})
	}
}
