package bot

// Tests for help.go's command help lookup. Moved verbatim from the former
// bot_calc_test.go so test files mirror the package's file-per-concern split.

import (
	"strings"
	"testing"
)

func TestHelpText(t *testing.T) {
	t.Run("empty arg lists all commands", func(t *testing.T) {
		got := helpText("")
		if !strings.Contains(got, "/menu") || !strings.Contains(got, "/help") {
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
