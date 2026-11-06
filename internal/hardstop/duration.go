package hardstop

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

// Duration validates hours before converting or multiplying into a Go duration.
func Duration(hours int) (time.Duration, error) {
	if hours <= 0 || uint64(hours) > uint64(math.MaxInt64/int64(time.Hour)) {
		return 0, fmt.Errorf("duration must be positive and at most %d hours", math.MaxInt64/int64(time.Hour))
	}
	return time.Duration(hours) * time.Hour, nil
}

// ParseDurationHours accepts whole hours, days or weeks. Bare amounts are hours.
func ParseDurationHours(text string) (int, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	multiplier := uint64(1)
	if len(text) > 0 {
		switch text[len(text)-1] {
		case 'h':
			text = text[:len(text)-1]
		case 'd':
			multiplier = 24
			text = text[:len(text)-1]
		case 'w':
			multiplier = 168
			text = text[:len(text)-1]
		}
	}
	invalid := fmt.Errorf("send a positive whole duration, e.g. 12, 48h, 2d or 1w (at most %d hours)", math.MaxInt64/int64(time.Hour))
	if text == "" {
		return 0, invalid
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return 0, invalid
		}
	}
	amount, err := strconv.ParseUint(text, 10, 64)
	if err != nil || amount == 0 || amount > uint64(math.MaxInt64/int64(time.Hour))/multiplier {
		return 0, invalid
	}
	hours := int(amount * multiplier)
	return hours, nil
}

// Remaining rounds positive partial minutes upward, using absolute expiry.
func Remaining(expiry, now time.Time) string {
	if !expiry.After(now) {
		return "ended"
	}
	remaining := expiry.Sub(now)
	minutes := remaining / time.Minute
	if remaining%time.Minute != 0 {
		minutes++
	}
	return fmt.Sprintf("%dd %dh %dm", minutes/(24*60), minutes/60%24, minutes%60)
}

// LockDetails is shared by status, menus, triggers and delayed closure delivery.
func LockDetails(lock config.LockFile, now time.Time) string {
	expiry := lock.ExpiresAt.In(now.Location()).Format("2006-01-02 15:04")
	if !lock.ExpiresAt.After(now) {
		return "Lock ended: " + expiry
	}
	return fmt.Sprintf("Locked until %s\nRemaining: %s", expiry, Remaining(lock.ExpiresAt, now))
}

func ClosureMessage(event config.ClosureEvent, now time.Time) string {
	return fmt.Sprintf("🛑 PokerStars was closed\nClosed at: %s\n%s", event.ClosedAt.In(now.Location()).Format("2006-01-02 15:04:05"), LockDetails(event.Lock, now))
}
