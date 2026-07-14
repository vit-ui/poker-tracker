package hardstop

import (
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

// TestPokerDayStart_DefaultHour covers the day-boundary edge case named in
// the testing brief: a session recorded just before the reset hour belongs
// to the previous poker day, one recorded at or after it belongs to today's.
// now is constructed directly in time.Local so its wall-clock digits are
// exactly what the test picks, regardless of the machine's real zone offset
// (pokerDayStart reinterprets those digits as UTC — see its doc comment).
func TestPokerDayStart_DefaultHour(t *testing.T) {
	cfg := &config.Config{}

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			"one second before the default 6am reset falls in the previous poker day",
			time.Date(2026, 7, 13, 5, 59, 59, 0, time.Local),
			time.Date(2026, 7, 12, 6, 0, 0, 0, time.UTC),
		},
		{
			"exactly 6am is the start of today's poker day",
			time.Date(2026, 7, 13, 6, 0, 0, 0, time.Local),
			time.Date(2026, 7, 13, 6, 0, 0, 0, time.UTC),
		},
		{
			"one second after 6am is still today's poker day",
			time.Date(2026, 7, 13, 6, 0, 1, 0, time.Local),
			time.Date(2026, 7, 13, 6, 0, 0, 0, time.UTC),
		},
		{
			"just before midnight is still yesterday's poker day",
			time.Date(2026, 7, 13, 23, 59, 59, 0, time.Local),
			time.Date(2026, 7, 13, 6, 0, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pokerDayStart(cfg, c.now)
			if !got.Equal(c.want) {
				t.Errorf("pokerDayStart(%v) = %v, want %v", c.now, got, c.want)
			}
		})
	}
}

// TestPokerDayStart_CustomHour confirms a non-default DayStartHour shifts
// the same boundary logic.
func TestPokerDayStart_CustomHour(t *testing.T) {
	hour := 14
	cfg := &config.Config{Schedule: config.Schedule{DayStartHour: &hour}}

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			"one minute before 2pm falls in the previous poker day",
			time.Date(2026, 7, 13, 13, 59, 0, 0, time.Local),
			time.Date(2026, 7, 12, 14, 0, 0, 0, time.UTC),
		},
		{
			"one minute after 2pm is today's poker day",
			time.Date(2026, 7, 13, 14, 1, 0, 0, time.Local),
			time.Date(2026, 7, 13, 14, 0, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pokerDayStart(cfg, c.now)
			if !got.Equal(c.want) {
				t.Errorf("pokerDayStart(%v) = %v, want %v", c.now, got, c.want)
			}
		})
	}
}

// TestRollingWindowStart confirms the cutoff is now's wall-clock digits
// (reinterpreted as UTC, same as pokerDayStart) minus the window duration.
func TestRollingWindowStart(t *testing.T) {
	now := time.Date(2026, 7, 13, 15, 30, 0, 0, time.Local)
	want := time.Date(2026, 6, 13, 15, 30, 0, 0, time.UTC)

	got := rollingWindowStart(now, 30*24*time.Hour)
	if !got.Equal(want) {
		t.Errorf("rollingWindowStart(%v, 30 days) = %v, want %v", now, got, want)
	}
}

// TestDownswingExceeds exercises the shared downswing comparison directly —
// bot.go's alert path and hardstop's own trigger path both call this.
func TestDownswingExceeds(t *testing.T) {
	cases := []struct {
		name      string
		peak      float64
		current   float64
		buyInUSD  float64
		threshold float64
		want      bool
	}{
		{"under threshold", 10, 8, 5.00, 1, false},
		{"exactly at threshold does not fire", 10, 5, 5.00, 1, false},
		{"just over threshold fires", 10, 4.99, 5.00, 1, true},
		{"no downswing at all", 10, 10, 5.00, 1, false},
		{"current above peak (shouldn't happen, but not negative downswing)", 5, 10, 5.00, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DownswingExceeds(c.peak, c.current, c.buyInUSD, c.threshold)
			if got != c.want {
				t.Errorf("DownswingExceeds(peak=%v, current=%v, buyIn=%v, threshold=%v) = %v, want %v",
					c.peak, c.current, c.buyInUSD, c.threshold, got, c.want)
			}
		})
	}
}
