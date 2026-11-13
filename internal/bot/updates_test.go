package bot

import (
	"strings"
	"testing"

	"github.com/vit-ui/self-tracker/internal/config"
)

func TestReleaseNotificationDecision(t *testing.T) {
	for _, tc := range []struct {
		latest, server, pc string
		want               bool
	}{
		{"v4.9", "v4.8", "v4.8", true}, {"v4.9", "v4.9", "v4.8", true},
		{"v4.9", "v4.8", "", true}, {"v4.9", "v4.9", "", false},
		{"v4.9", "v4.9", "v4.9", false}, {"v4.9", "v4.10", "v4.10", false},
		{"v4.9", "dev", "", false}, {"v4.9-rc1", "v4.8", "v4.8", false},
		{"v4.9.1", "v4.9", "v4.9.0", true},
	} {
		t.Run(tc.latest+tc.server+tc.pc, func(t *testing.T) {
			if got := releaseNoticeNeeded(tc.latest, "", tc.server, tc.pc); got != tc.want {
				t.Fatalf("releaseNeeded=%v want %v", got, tc.want)
			}
		})
	}
	for _, notified := range []string{"v4.9", "v4.10"} {
		if releaseNoticeNeeded("v4.9", notified, "v4.8") {
			t.Fatalf("repeated/regressed notice after %s", notified)
		}
	}
}

func TestUpdateStatusDoesNotClaimSuccessBeforeAcknowledgement(t *testing.T) {
	r := config.UpdateRequest{ID: "request", Version: "v4.9"}
	for _, tc := range []struct{ state, want string }{
		{config.UpdateQueued, "queued"}, {config.UpdateWaiting, "waiting for PokerStars"},
		{config.UpdateRunning, "waiting for confirmed startup"}, {config.UpdateSucceeded, "completed"}, {config.UpdateFailed, "failed"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			job := config.UpdateJob{Request: r, State: tc.state, Version: "v4.8", Error: "permission denied"}
			text := updateJobText("Poker PC", &job)
			if !strings.Contains(text, tc.want) {
				t.Fatalf("wrong state: %s", text)
			}
			if tc.state != config.UpdateSucceeded && strings.Contains(text, "completed") {
				t.Fatal("unconfirmed update shown as complete")
			}
		})
	}
	text := collectorUpdateText(config.CollectorUpdates{Request: &r})
	if !strings.Contains(text, "awaiting PC confirmation") {
		t.Fatalf("unacknowledged request: %s", text)
	}
}
