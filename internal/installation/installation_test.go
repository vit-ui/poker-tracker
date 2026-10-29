package installation

import (
	"runtime"
	"testing"
)

func TestTaskName(t *testing.T) {
	for _, tc := range []struct {
		id     string
		server bool
		want   string
	}{
		{"", false, "Poker Tracker"},
		{"", true, "PokerTrackerServer"},
		{"disposable-123", false, "Poker Tracker-disposable-123"},
		{"disposable-123", true, "PokerTrackerServer-disposable-123"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := TaskName(tc.id, tc.server); got != tc.want {
				t.Fatalf("TaskName(%q, %v) = %q, want %q", tc.id, tc.server, got, tc.want)
			}
		})
	}
}

func TestServerServiceName(t *testing.T) {
	for _, tc := range []struct{ id, linux, windows string }{
		{"", "poker-tracker.service", "PokerTrackerServer"},
		{"reliability-restart", "poker-tracker-reliability-restart.service", "PokerTrackerServer-reliability-restart"},
	} {
		t.Run(tc.linux, func(t *testing.T) {
			want := tc.windows
			if runtime.GOOS == "linux" {
				want = tc.linux
			}
			if got := ServerServiceName(tc.id); got != want {
				t.Fatalf("ServerServiceName(%q)=%q, want %q", tc.id, got, want)
			}
		})
	}
}
