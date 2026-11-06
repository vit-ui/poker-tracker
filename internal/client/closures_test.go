package client

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

// Exercise real journals using termination outcomes. No process termination is
// invoked: actual Windows enforcement needs a separately coordinated window.
func TestClosureOutcomesAndPersistence(t *testing.T) {
	for _, test := range []struct {
		name    string
		closed  bool
		err     error
		expired bool
		count   int
	}{
		{name: "idle"}, {name: "failed", err: errors.New("termination failed")}, {name: "failed result", closed: true, err: errors.New("termination failed")}, {name: "success", closed: true, count: 2}, {name: "expired", closed: true, expired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lockfile.json")
			e := NewEnforcer(path)
			now := time.Now().UTC()
			lock := config.LockFile{TriggerID: "owned-lock", Reason: "manual", TriggeredAt: now, ExpiresAt: now.Add(48 * time.Hour), DurationHours: 48}
			if test.expired {
				lock.ExpiresAt = now
			}
			if err := config.SaveLock(path, &lock); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := e.recordClosure(&lock, test.closed, test.err, now); !errors.Is(err, test.err) {
					t.Fatal(err)
				}
				e.persistClosures()
			}
			entries, err := config.LoadClosureNotifications(e.closuresPath())
			if err != nil || len(entries) != test.count {
				t.Fatalf("entries=%+v err=%v", entries, err)
			}
			for id, entry := range entries {
				if id == "" || entry.Event.ID != id || !entry.Event.Lock.ExpiresAt.Equal(lock.ExpiresAt) || !entry.Event.ClosedAt.Equal(now) {
					t.Fatalf("bad event: %+v", entry)
				}
				if err := NewEnforcer(path).acknowledgeClosure(id); err != nil {
					t.Fatal(err)
				}
			}
			entries, err = config.LoadClosureNotifications(e.closuresPath())
			if err != nil || len(entries) != 0 {
				t.Fatal(entries, err)
			}
			saved, err := config.LoadLock(path)
			if err != nil || !saved.ExpiresAt.Equal(lock.ExpiresAt) {
				t.Fatal("closure changed lock", saved, err)
			}
		})
	}
}

func TestClosureSaveFailureRetainsEventAndProtection(t *testing.T) {
	root := t.TempDir()
	e := NewEnforcer(filepath.Join(root, "lockfile.json"))
	now := time.Now().UTC()
	lock := config.LockFile{TriggerID: "owned", ExpiresAt: now.Add(48 * time.Hour), DurationHours: 48}
	if err := config.SaveLock(e.path, &lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.closuresPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := e.recordClosure(&lock, true, nil, now); err != nil {
		t.Fatal(err)
	}
	e.persistClosures()
	if len(e.unsavedClosures) != 1 {
		t.Fatal("failed persistence lost closure")
	}
	if err := e.checkClosureTransferReady(); err == nil {
		t.Fatal("unsaved event allowed transfer")
	}
	saved, err := config.LoadLock(e.path)
	if err != nil || !saved.ExpiresAt.Equal(lock.ExpiresAt) {
		t.Fatal(saved, err)
	}
	if err := os.Remove(e.closuresPath()); err != nil {
		t.Fatal(err)
	}
	e.persistClosures()
	restarted := NewEnforcer(e.path)
	entries, err := config.LoadClosureNotifications(restarted.closuresPath())
	if err != nil || len(entries) != 1 || len(e.unsavedClosures) != 0 {
		t.Fatal(entries, err)
	}
	if err := restarted.checkClosureTransferReady(); err == nil {
		t.Fatal("pending event allowed transfer")
	}
}
