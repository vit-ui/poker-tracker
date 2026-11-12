package installation

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

func TestStartupAcknowledgementRequiresMatchingBuild(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		wantReady     bool
	}{{"matching", "v4.8", true}, {"other build", "v4.7", false}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "update_startup.json")
			if err := config.SaveUpdateStartup(path, config.UpdateStartup{Token: "single-update", Version: "v4.8"}); err != nil {
				t.Fatal(err)
			}
			if err := ReportReady(dir, tc.version); err != nil {
				t.Fatal(err)
			}
			state, err := config.LoadUpdateStartup(path)
			if err != nil {
				t.Fatal(err)
			}
			if state.Token != "single-update" || (state.PID == os.Getpid()) != tc.wantReady {
				t.Fatalf("receipt=%+v", state)
			}
		})
	}
}

func TestStartupWaitsForRequiredComponents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "update_startup.json")
	if err := config.SaveUpdateStartup(path, config.UpdateStartup{Token: "single-update", Version: "v4.8"}); err != nil {
		t.Fatal(err)
	}
	worker, bot, stop := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(stop)
	errors := ReadyAfter(dir, "v4.8", stop, worker, bot)
	close(worker)
	state, err := config.LoadUpdateStartup(path)
	if err != nil || state.PID != 0 {
		t.Fatalf("premature receipt=%+v err=%v", state, err)
	}
	close(bot)
	deadline := time.Now().Add(time.Second)
	for {
		state, err := config.LoadUpdateStartup(path)
		if err != nil {
			t.Fatal(err)
		}
		if state.PID == os.Getpid() {
			break
		}
		select {
		case err := <-errors:
			t.Fatal(err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("startup was not acknowledged")
		}
		time.Sleep(time.Millisecond)
	}
}
