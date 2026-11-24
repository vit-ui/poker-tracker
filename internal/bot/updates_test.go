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

func TestUpdateMenuShowsOnlyRelevantJobStatus(t *testing.T) {
	old := config.UpdateRequest{ID: "earlier", Version: "v5.1.0"}
	next := config.UpdateRequest{ID: "next", Version: "v5.1.2"}
	completed := config.UpdateJob{Request: old, State: config.UpdateSucceeded, Version: "v5.1.0"}
	for _, tc := range []struct {
		name       string
		report     config.UpdateReport
		menu       string
		superseded bool
	}{
		{name: "no receipt", report: config.UpdateReport{Version: "v5.1.1"}},
		{name: "terminal update", report: config.UpdateReport{Version: "v5.1.1", Job: &completed}, superseded: true},
		{name: "numeric version ordering", report: config.UpdateReport{Version: "v5.10.0", Job: &completed}, superseded: true},
		{name: "current version already listed", report: config.UpdateReport{Version: "v5.1.0", Job: &completed}},
		{name: "collector did not report its version", report: config.UpdateReport{Job: &completed}, menu: "completed (v5.1.0)"},
		{name: "running version is older than receipt", report: config.UpdateReport{Version: "v5.0.1", Job: &completed}, menu: "completed (v5.1.0)"},
		{
			name: "current request still waiting",
			report: config.UpdateReport{Version: "v5.1.1", Job: &config.UpdateJob{
				Request: next, State: config.UpdateWaiting, Version: "v5.1.1",
			}},
			menu: "waiting for PokerStars",
		},
		{
			name: "failed update remains visible",
			report: config.UpdateReport{Version: "v5.1.1", Job: &config.UpdateJob{
				Request: next, State: config.UpdateFailed, Version: "v5.1.0", Error: "permission denied",
			}},
			menu: "failed: permission denied",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statuses := []string{
				updateMenuJobText("Server", tc.report),
				collectorUpdateText(config.CollectorUpdates{Report: tc.report}),
			}
			for _, got := range statuses {
				if tc.menu == "" && got != "" || !strings.Contains(got, tc.menu) {
					t.Errorf("menu status = %q, want %q", got, tc.menu)
				}
			}
			if got := completedUpdateSuperseded(tc.report); got != tc.superseded {
				t.Errorf("suppress completion notice = %v, want %v", got, tc.superseded)
			}
		})
	}

	state := config.CollectorUpdates{
		Request: &next,
		Report:  config.UpdateReport{Version: "v5.1.1", Job: &completed},
	}
	if got := collectorUpdateText(state); got != "Poker PC → v5.1.2: saved; awaiting PC confirmation." {
		t.Errorf("pending request with superseded receipt: %q", got)
	}
}
