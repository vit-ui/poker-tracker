package update

import (
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

func TestAcceptDurableUpdateRequest(t *testing.T) {
	for _, tc := range []struct {
		name, current, state, id, version string
		keep, invalid                     bool
	}{
		{"fresh", "v4.8", "", "new", "v4.9", false, false},
		{"queued coalesces", "v4.8", config.UpdateQueued, "new", "v4.9", true, false},
		{"waiting coalesces", "v4.8", config.UpdateWaiting, "new", "v4.9", true, false},
		{"running coalesces", "v4.8", config.UpdateRunning, "new", "v4.9", true, false},
		{"pending different version refused", "v4.8", config.UpdateWaiting, "new", "v4.10", true, true},
		{"completed identity replay", "v4.9", config.UpdateSucceeded, "old", "v4.9", true, false},
		{"failed identity replay", "v4.8", config.UpdateFailed, "old", "v4.9", true, false},
		{"new request after failure", "v4.8", config.UpdateFailed, "new", "v4.9", false, false},
		{"development build", "dev", "", "new", "v4.9", false, true},
		{"missing identity", "v4.8", "", "", "v4.9", false, true},
		{"prerelease", "v4.8", "", "new", "v4.9-rc1", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "requested_update.json")
			var old *config.UpdateJob
			if tc.state != "" {
				old = &config.UpdateJob{Request: config.UpdateRequest{ID: "old", Version: "v4.9"}, State: tc.state, Version: tc.current}
				if err := config.SaveUpdateJob(path, *old); err != nil {
					t.Fatal(err)
				}
			}
			err := acceptRequest(path, tc.current, old, config.UpdateRequest{ID: tc.id, Version: tc.version})
			if (err != nil) != tc.invalid {
				t.Fatalf("accept error = %v", err)
			}
			got, err := config.LoadUpdateJob(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.invalid {
				if old != nil {
					if got == nil || *got != *old {
						t.Fatal("refused request changed saved job")
					}
					return
				}
				if got != nil {
					t.Fatal("invalid request wrote state")
				}
				return
			}
			if tc.keep {
				if got == nil || *got != *old {
					t.Fatalf("active/replayed request replaced: %+v", got)
				}
			} else if got == nil || got.Request.ID != tc.id || got.Request.Version != tc.version || got.State != config.UpdateQueued {
				t.Fatalf("request not saved: %+v", got)
			}
		})
	}
}

func TestCompletedUpdateIsNotAppliedAgain(t *testing.T) {
	target := filepath.Join(t.TempDir(), "tracker-server")
	job := config.UpdateJob{Request: config.UpdateRequest{ID: "complete", Version: "v4.9"}, State: config.UpdateSucceeded, Version: "v4.9"}
	path := filepath.Join(filepath.Dir(target), "requested_update.json")
	if err := config.SaveUpdateJob(path, job); err != nil {
		t.Fatal(err)
	}
	if err := runRequested(target, "server", "v4.9", "", "complete", io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadUpdateJob(path)
	if err != nil || got == nil || *got != job {
		t.Fatalf("completed job changed: %+v, %v", got, err)
	}
}

func TestAgentReplaysSavedResultAndReportsRealLaunchFailure(t *testing.T) {
	target := filepath.Join(t.TempDir(), "missing-executable")
	job := config.UpdateJob{Request: config.UpdateRequest{ID: "complete", Version: "v4.9"}, State: config.UpdateSucceeded, Version: "v4.9"}
	path := filepath.Join(filepath.Dir(target), "requested_update.json")
	if err := config.SaveUpdateJob(path, job); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Role: "server", Version: "v4.9", target: target, requests: make(chan queuedRequest), Changes: make(chan config.UpdateReport, 1), done: make(chan struct{})}
	stop := make(chan struct{})
	go a.Run(stop, nil)
	defer func() { close(stop); <-a.Done() }()
	select {
	case report := <-a.Changes:
		if report.Job == nil || *report.Job != job {
			t.Fatalf("lost result: %+v", report)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("initial report missing")
	}
	report, err := a.Request(job.Request)
	if err != nil || report.Job == nil || *report.Job != job {
		t.Fatalf("replay changed completion: %+v %v", report, err)
	}
	report, err = a.Request(config.UpdateRequest{ID: "new", Version: "v4.10"})
	if err != nil || report.Job == nil || report.Job.State != config.UpdateFailed || report.Job.Error == "" {
		t.Fatalf("real copy failure not reported: %+v %v", report, err)
	}
	if got, err := config.LoadUpdateJob(path); err != nil || got == nil || got.State != config.UpdateFailed {
		t.Fatalf("launch failure not durable: %+v %v", got, err)
	}
}

func TestRequestedUpdateLockIsInstallationScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requested.lock")
	release, err := lockRequested(path)
	if err != nil {
		t.Fatal(err)
	}
	if unlock, err := lockRequested(path); err == nil {
		unlock()
		t.Error("two helpers acquired the same installation")
	}
	other, err := lockRequested(filepath.Join(t.TempDir(), "requested.lock"))
	if err != nil {
		t.Fatal(err)
	}
	other()
	release()
	again, err := lockRequested(path)
	if err != nil {
		t.Fatal(err)
	}
	again()
}
