//go:build !windows

package client

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func TestRecoveredPersistenceCannotShortenRetainedHardstop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockfile.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	enforcer := NewEnforcer(path)
	now := time.Now().UTC()
	long := config.LockFile{TriggerID: "long-unsaved", ExpiresAt: now.Add(12 * time.Hour), Reason: "manual"}
	if active, _, err := enforcer.Enforce(long); err == nil || active == nil || !active.ExpiresAt.Equal(long.ExpiresAt) {
		t.Fatalf("failed write lost in-memory protection: %+v %v", active, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	short := config.LockFile{TriggerID: "short-new", ExpiresAt: now.Add(time.Hour), Reason: "manual"}
	active, fresh, err := enforcer.Enforce(short)
	if err != nil || !fresh || active == nil || !active.ExpiresAt.Equal(long.ExpiresAt) {
		t.Fatalf("recovered write shortened retained protection: %+v %v %v", active, fresh, err)
	}
	replay := long
	replay.ExpiresAt = now.Add(24 * time.Hour)
	active, fresh, err = enforcer.Enforce(replay)
	if err != nil || fresh || active == nil || !active.ExpiresAt.Equal(long.ExpiresAt) {
		t.Fatalf("retained trigger receipt was lost: %+v %v %v", active, fresh, err)
	}
}

func TestLockCommandsRemainResponsiveWithBlockedUploadsAndControlRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockfile.json")
	link := transport.NewLink()
	link.Uploads = make(chan transport.Upload)
	collector := NewCollector(link, NewEnforcer(path), nil)
	finished, stopped := make(chan struct{}), make(chan struct{})
	requests := make(chan transport.Command) // deliberately no receiver
	go func() { collector.applyCommands(nil, finished, requests); close(stopped) }()
	defer func() { close(finished); <-stopped }()
	link.Commands <- transport.Command{Kind: transport.CommandRescan}
	now := time.Now().UTC()
	for _, test := range []struct {
		id       string
		duration time.Duration
	}{{"hardstop:first", time.Hour}, {"hardstop:second", 2 * time.Hour}} {
		lock := config.LockFile{TriggerID: test.id, Reason: "manual", TriggeredAt: now, ExpiresAt: now.Add(test.duration), DurationHours: int(test.duration / time.Hour)}
		link.Commands <- transport.Command{Kind: transport.CommandLock, Lock: &lock}
		deadline := time.Now().Add(time.Second)
		for {
			saved, err := config.LoadLock(path)
			if err == nil && saved != nil && saved.TriggerID == test.id {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("lock %s was blocked by upload/control backpressure: %+v %v", test.id, saved, err)
			}
			time.Sleep(time.Millisecond)
		}
	}
}
