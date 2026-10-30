package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func awaitLockCommand(t *testing.T, link *transport.Link) config.LockFile {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case command := <-link.Commands:
			if command.Kind == transport.CommandLock {
				return *command.Lock
			}
		case <-deadline:
			t.Fatal("hard stop was not delivered")
			return config.LockFile{}
		}
	}
}

func TestHardstopDeliverySurvivesLostAcknowledgementWithoutExtendingExpiry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	streak, duration := 1, 1
	cfg := &config.Config{}
	cfg.Hardstop.ConsecutiveLosses = &streak
	cfg.Hardstop.DurationHours = &duration
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := recoveryFixture(t)
	saved := []config.PendingCash{{FileID: "table", Session: session, LastActivity: time.Now().Add(-time.Hour)}}
	journal := filepath.Join(dir, "pending_cash.json")
	if err := config.SavePendingCash(journal, saved); err != nil {
		t.Fatal(err)
	}
	_, link, stop := runRecoveredWorker(t, store, path, time.Second)
	initial := awaitLockCommand(t, link)
	collectorPath := filepath.Join(t.TempDir(), "lockfile.json")
	effective, fresh, err := config.ApplyHardstop(collectorPath, initial, time.Now())
	if err != nil || !fresh || effective == nil {
		t.Fatalf("first collector application: %+v %v %v", effective, fresh, err)
	}
	stop() // applied on collector, acknowledgement lost before reaching server
	if err := config.SavePendingCash(journal, saved); err != nil {
		t.Fatal(err)
	}
	_, link, stop = runRecoveredWorker(t, store, path, time.Second)
	replay := awaitLockCommand(t, link)
	if replay.TriggerID != initial.TriggerID || !replay.ExpiresAt.Equal(initial.ExpiresAt) {
		t.Fatal("server regenerated a pending trigger's clock")
	}
	effective, fresh, err = config.ApplyHardstop(collectorPath, replay, time.Now())
	if err != nil || fresh || effective == nil || !effective.ExpiresAt.Equal(initial.ExpiresAt) {
		t.Fatal("lost acknowledgement extended the lock")
	}
	link.Uploads <- transport.Upload{LockReply: &transport.LockReply{TriggerID: replay.TriggerID, Lock: effective}, ClientClock: time.Now()}
	deadline := time.Now().Add(time.Second)
	for {
		entries, err := config.LoadHardstopDeliveries(filepath.Join(dir, "hardstop_deliveries.json"))
		if err != nil {
			t.Fatal(err)
		}
		if entries[initial.TriggerID].Confirmed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("acknowledgement was not durably confirmed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	// Recovering the identical row restores only the confirmed active clock,
	// never a new decision or a freshly calculated expiry.
	if err := config.SavePendingCash(journal, saved); err != nil {
		t.Fatal(err)
	}
	_, link, stop = runRecoveredWorker(t, store, path, time.Second)
	defer stop()
	timeout := time.After(80 * time.Millisecond)
	restored := false
	for {
		select {
		case command := <-link.Commands:
			if command.Kind == transport.CommandLock {
				if restored || command.Lock.TriggerID != initial.TriggerID || !command.Lock.ExpiresAt.Equal(initial.ExpiresAt) {
					t.Fatal("confirmed recovery changed or repeated the active clock")
				}
				restored = true
			}
		case <-timeout:
			if !restored {
				t.Fatal("confirmed active lock was not restored")
			}
			snapshot, err := store.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Ledger) != 1 {
				t.Fatal("cash recovery duplicated money")
			}
			return
		}
	}
}

func TestExpiredCollectorReceiptRejectsFreshServerReplayButAllowsNewLoss(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	streak, duration := 1, 1
	cfg := &config.Config{}
	cfg.Hardstop.ConsecutiveLosses = &streak
	cfg.Hardstop.DurationHours = &duration
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := recoveryFixture(t)
	if err := store.AppendCashSession(session); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	id, err := hardstop.AutomaticTriggerID("consecutive_losses", duration, cfg, time.Now(), &session, nil, snapshot.Ledger, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	old, _ := hardstop.BuildTrigger("consecutive_losses", duration, oldTime)
	old.TriggerID = id
	collectorPath := filepath.Join(t.TempDir(), "lockfile.json")
	if _, _, err := config.ApplyHardstop(collectorPath, old, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := config.DeleteLock(collectorPath); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePendingCash(filepath.Join(dir, "pending_cash.json"), []config.PendingCash{{FileID: "table", Session: session, LastActivity: oldTime}}); err != nil {
		t.Fatal(err)
	}
	_, link, stop := runRecoveredWorker(t, store, path, time.Second)
	replay := awaitLockCommand(t, link)
	effective, fresh, err := config.ApplyHardstop(collectorPath, replay, time.Now())
	if err != nil || fresh || effective != nil {
		t.Fatalf("expired trigger restarted: %+v %v %v", effective, fresh, err)
	}
	link.Uploads <- transport.Upload{LockReply: &transport.LockReply{TriggerID: replay.TriggerID}}
	stop()
	session.FirstHandID += "-new-loss"
	session.NetCents = -10
	if err := config.SavePendingCash(filepath.Join(dir, "pending_cash.json"), []config.PendingCash{{FileID: "new-table", Session: session, LastActivity: oldTime}}); err != nil {
		t.Fatal(err)
	}
	_, link, stop = runRecoveredWorker(t, store, path, time.Second)
	defer stop()
	newLock := awaitLockCommand(t, link)
	if newLock.TriggerID == id {
		t.Fatal("new financial loss reused an old trigger")
	}
	effective, fresh, err = config.ApplyHardstop(collectorPath, newLock, time.Now())
	if err != nil || !fresh || effective == nil {
		t.Fatal("new trigger was suppressed")
	}
}
