package installation

import "testing"

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
