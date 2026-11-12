package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCollectorClockLocationRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name       string
		clock      CollectorClock
		instant    time.Time
		wantOffset int
	}{
		{"fixed collector offset", CollectorClock{Name: "player", OffsetSeconds: -10800}, time.Date(2026, 11, 12, 2, 30, 0, 0, time.UTC), -10800},
		{"local name cannot use server timezone", CollectorClock{Name: "Local", OffsetSeconds: 32400}, time.Date(2026, 11, 12, 2, 30, 0, 0, time.UTC), 32400},
		{"IANA summer rules", CollectorClock{Name: "Europe/Berlin", OffsetSeconds: 3600}, time.Date(2026, 7, 12, 2, 30, 0, 0, time.UTC), 7200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "collector_clock.json")
			missing, err := LoadCollectorClock(path)
			if err != nil || missing != nil {
				t.Fatalf("missing=%v err=%v", missing, err)
			}
			if err := SaveCollectorClock(path, tc.clock); err != nil {
				t.Fatal(err)
			}
			saved, err := LoadCollectorClock(path)
			if err != nil {
				t.Fatal(err)
			}
			if *saved != tc.clock {
				t.Fatal("clock changed on disk")
			}
			_, offset := tc.instant.In(saved.Location()).Zone()
			if offset != tc.wantOffset {
				t.Fatalf("offset=%d want %d", offset, tc.wantOffset)
			}
		})
	}
}
