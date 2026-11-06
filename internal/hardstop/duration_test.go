package hardstop

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

func TestParseDurationHours(t *testing.T) {
	for _, test := range []struct {
		input string
		hours int
	}{
		{"12", 12}, {"24", 24}, {"48", 48}, {"48h", 48}, {"2d", 48}, {"1w", 168}, {" 2D \t", 48}, {"1W", 168}, {"1H", 1}, {"2562047h", 2562047}, {"106751d", 2562024}, {"15250w", 2562000},
		{"", 0}, {"0", 0}, {"0w", 0}, {"-2d", 0}, {"+1h", 0}, {"1.5h", 0}, {"1.0", 0}, {"2m", 0}, {"1 h", 0}, {"1d2h", 0}, {"2562048", 0}, {"106752d", 0}, {"15251w", 0}, {"18446744073709551616w", 0}, {"18446744073709551615w", 0},
	} {
		t.Run(test.input, func(t *testing.T) {
			hours, err := ParseDurationHours(test.input)
			if hours != test.hours || (err != nil) != (test.hours == 0) {
				t.Fatalf("ParseDurationHours(%q) = %d, %v; want %d", test.input, hours, err, test.hours)
			}
		})
	}
}

func TestDurationAndBuildTrigger(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		hours    int
		duration time.Duration
	}{
		{12, 12 * time.Hour}, {24, 24 * time.Hour}, {48, 48 * time.Hour}, {168, 168 * time.Hour}, {2562047, 2562047 * time.Hour}, {0, 0}, {-1, 0}, {2562048, 0}, {math.MaxInt, 0},
	} {
		duration, err := Duration(test.hours)
		if duration != test.duration || (err != nil) != (test.duration == 0) {
			t.Fatalf("Duration(%d) = %v, %v", test.hours, duration, err)
		}
		lock, _, buildErr := BuildTrigger("manual", test.hours, now)
		if (buildErr != nil) != (test.duration == 0) {
			t.Fatalf("BuildTrigger(%d): %v", test.hours, buildErr)
		}
		if buildErr == nil && (!lock.ExpiresAt.Equal(now.Add(test.duration)) || lock.DurationHours != test.hours) {
			t.Fatalf("wrong lock: %+v", lock)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		hours := test.hours
		if err := config.SaveConfig(path, &config.Config{Hardstop: config.Hardstop{DurationHours: &hours}}); err != nil {
			t.Fatal(err)
		}
		triggered := false
		err = ManualTrigger(path, func(string, int) { triggered = true })
		if triggered != (test.duration > 0) || (err != nil) != (test.duration == 0) {
			t.Fatalf("manual duration %d: triggered=%v err=%v", hours, triggered, err)
		}
	}
}

func TestRemainingAndClosureMessage(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.FixedZone("player", -3*3600))
	for _, test := range []struct {
		delta time.Duration
		want  string
	}{
		{48 * time.Hour, "2d 0h 0m"}, {168 * time.Hour, "7d 0h 0m"}, {24*time.Hour - time.Nanosecond, "1d 0h 0m"}, {time.Hour, "0d 1h 0m"}, {time.Hour + time.Nanosecond, "0d 1h 1m"}, {time.Minute, "0d 0h 1m"}, {time.Nanosecond, "0d 0h 1m"}, {0, "ended"}, {-time.Nanosecond, "ended"},
	} {
		expiry := now.UTC().Add(test.delta)
		if got := Remaining(expiry, now); got != test.want {
			t.Fatalf("Remaining(%v)=%q want %q", test.delta, got, test.want)
		}
		lock := config.LockFile{ExpiresAt: expiry, Reason: "manual"}
		details := LockDetails(lock, now)
		if test.delta > 0 && !strings.Contains(details, "Remaining: "+test.want) {
			t.Fatal(details)
		}
		if test.delta <= 0 && (strings.Contains(details, "Locked until") || !strings.Contains(details, "Lock ended:")) {
			t.Fatal(details)
		}
	}
	event := config.ClosureEvent{ID: "closure:one", ClosedAt: now.Add(-time.Hour).UTC(), Lock: config.LockFile{ExpiresAt: now.Add(48 * time.Hour).UTC()}}
	text := ClosureMessage(event, now)
	for _, part := range []string{"PokerStars was closed", "Closed at: 2026-11-05 11:00:00", "Locked until 2026-11-07 12:00", "Remaining: 2d 0h 0m"} {
		if !strings.Contains(text, part) {
			t.Fatal(text)
		}
	}
	delayed := ClosureMessage(event, now.Add(49*time.Hour))
	if !strings.Contains(delayed, "Closed at: 2026-11-05 11:00:00") || !strings.Contains(delayed, "Lock ended: 2026-11-07 12:00") || strings.Contains(delayed, "Remaining:") {
		t.Fatal(delayed)
	}
}
