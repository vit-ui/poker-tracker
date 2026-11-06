package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
)

func TestClosureQueueRequiresPersistenceBeforeAcknowledgement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closure_notifications.json")
	q, err := loadClosureQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	event := config.ClosureEvent{ID: "closure:first", ClosedAt: at, Lock: config.LockFile{ExpiresAt: at.Add(48 * time.Hour), DurationHours: 48}}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := q.receive(event); err == nil {
		t.Fatal("failed journal replacement was acknowledged")
	}
	if len(q.acknowledgements) != 0 || q.next() != nil {
		t.Fatal("unsaved event was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := q.receive(event); err != nil {
		t.Fatal(err)
	}
	if !q.acknowledgements[event.ID] {
		t.Fatal("saved event was not acknowledged")
	}
	// Distinct replay data must not replace the original lock snapshot.
	replay := event
	replay.Lock.ExpiresAt = at.Add(168 * time.Hour)
	if err := q.receive(replay); err != nil {
		t.Fatal(err)
	}
	if !q.next().Lock.ExpiresAt.Equal(event.Lock.ExpiresAt) {
		t.Fatal("replay changed original expiry")
	}
	q.attempted[event.ID], q.inflight[event.ID] = true, true
	if err := os.Mkdir(path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	q.result(bot.ClosureResult{ID: event.ID})
	if !q.dirty || q.next() != nil {
		t.Fatal("failed result persistence was lost or re-sent immediately")
	}
	saved, err := config.LoadClosureNotifications(path)
	if err != nil || saved[event.ID].Delivered {
		t.Fatal(saved, err)
	}
	if err := os.Remove(path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	q.retry()
	restarted, err := loadClosureQueue(path)
	if err != nil || restarted.next() != nil || !restarted.entries[event.ID].Delivered {
		t.Fatal("result did not survive restart", err)
	}
	if err := restarted.receive(event); err != nil {
		t.Fatal(err)
	}
	if restarted.next() != nil {
		t.Fatal("delivered replay produced a new notification")
	}
	if err := config.CheckClosureTransferReady(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
}

func TestClosureQueueRejectsInvalidEvents(t *testing.T) {
	at := time.Now().UTC()
	for _, event := range []config.ClosureEvent{
		{}, {ID: "no-time", Lock: config.LockFile{ExpiresAt: at}}, {ID: "no-lock", ClosedAt: at}, {ID: "expired", ClosedAt: at, Lock: config.LockFile{ExpiresAt: at}},
	} {
		q, err := loadClosureQueue(filepath.Join(t.TempDir(), "notifications.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := q.receive(event); err == nil || len(q.acknowledgements) != 0 {
			t.Fatalf("invalid event accepted: %+v", event)
		}
	}
}
