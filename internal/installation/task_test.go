package installation

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"
)

// TestUTF16File confirms the schtasks-required encoding: a little-endian
// BOM followed by each rune as a little-endian 16-bit code unit. schtasks
// /Create /XML silently rejects a plain UTF-8 file, so this encoding is
// load-bearing for --setup's Task Scheduler registration even though it's
// never exercised on this (non-Windows) test machine.
func TestUTF16File(t *testing.T) {
	got := utf16File("Ab")
	want := []byte{0xFF, 0xFE, 'A', 0x00, 'b', 0x00}
	if len(got) != len(want) {
		t.Fatalf("utf16File(%q) length = %d, want %d (%v)", "Ab", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("utf16File(%q)[%d] = %#x, want %#x", "Ab", i, got[i], want[i])
		}
	}

	t.Run("empty string is just the BOM", func(t *testing.T) {
		if got := utf16File(""); len(got) != 2 || got[0] != 0xFF || got[1] != 0xFE {
			t.Errorf("utf16File(\"\") = %v, want the 2-byte BOM only", got)
		}
	})

	t.Run("code point above the ASCII range keeps little-endian byte order", func(t *testing.T) {
		// U+00E9 (é) — high byte non-zero, so byte order is observable.
		got := utf16File("é")
		want := []byte{0xFF, 0xFE, 0xE9, 0x00}
		if len(got) != len(want) {
			t.Fatalf("utf16File(\"é\") = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("utf16File(\"é\")[%d] = %#x, want %#x", i, got[i], want[i])
			}
		}
	})
}

func TestWindowsTaskRecoveryPolicyAndEscapedPaths(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 34, 56, 0, time.UTC)
	executable := `C:\owned & isolated\tracker.exe`
	directory := `C:\owned & isolated`
	text := WindowsTaskXML(executable, directory, now)
	decoder := xml.NewDecoder(strings.NewReader(text))
	decoder.CharsetReader = func(_ string, reader io.Reader) (io.Reader, error) { return reader, nil }
	var task struct {
		Triggers struct {
			Boot      struct{ Delay string } `xml:"BootTrigger"`
			Repeating struct {
				Start      string `xml:"StartBoundary"`
				Repetition struct {
					Interval string
					Duration string
					Stop     bool `xml:"StopAtDurationEnd"`
				}
			} `xml:"TimeTrigger"`
		}
		Principals struct {
			Principal struct {
				UserID string `xml:"UserId"`
			}
		}
		Settings struct {
			Multiple  string `xml:"MultipleInstancesPolicy"`
			Power     bool   `xml:"DisallowStartIfOnBatteries"`
			StopPower bool   `xml:"StopIfGoingOnBatteries"`
			Execution string `xml:"ExecutionTimeLimit"`
			Start     bool   `xml:"StartWhenAvailable"`
			Restart   struct {
				Interval string
				Count    int
			} `xml:"RestartOnFailure"`
		}
		Actions struct {
			Exec struct{ Command, Arguments, WorkingDirectory string }
		}
	}
	if err := decoder.Decode(&task); err != nil {
		t.Fatal(err)
	}
	if task.Principals.Principal.UserID != "S-1-5-18" || task.Triggers.Boot.Delay != "PT30S" {
		t.Fatalf("boot protection changed: %+v", task)
	}
	if task.Triggers.Repeating.Start != "2026-01-02T12:34:56" || task.Triggers.Repeating.Repetition.Interval != "PT1M" || task.Triggers.Repeating.Repetition.Duration != "" || task.Triggers.Repeating.Repetition.Stop {
		t.Fatalf("recovery trigger does not repeat indefinitely: %+v", task.Triggers)
	}
	if task.Settings.Multiple != "IgnoreNew" || task.Settings.Power || task.Settings.StopPower || task.Settings.Execution != "PT0S" || !task.Settings.Start || task.Settings.Restart.Interval != "PT1M" || task.Settings.Restart.Count != 255 {
		t.Fatalf("recovery settings: %+v", task.Settings)
	}
	if task.Actions.Exec.Command != executable || task.Actions.Exec.WorkingDirectory != directory || task.Actions.Exec.Arguments != "--serve" {
		t.Fatalf("installation paths were not preserved: %+v", task.Actions)
	}
}
