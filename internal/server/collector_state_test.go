package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func TestCollectorLockUsesObservedStateAndCopiesSnapshots(t *testing.T) {
	worker, link, _ := migrationWorker(t)
	if _, err := worker.CollectorLock(); err == nil {
		t.Fatal("unknown status reported as confirmed")
	}
	lock := config.LockFile{TriggerID: "observed", Reason: "manual", ExpiresAt: time.Now().Add(time.Hour)}
	link.Uploads <- transport.Upload{Status: &transport.CollectorStatus{Lock: &lock}, ClientClock: time.Now().In(time.FixedZone("player", -3*3600))}
	deadline := time.Now().Add(time.Second)
	for {
		got, err := worker.CollectorLock()
		if err == nil && got != nil {
			if !got.ExpiresAt.Equal(lock.ExpiresAt) {
				t.Fatal("observed expiry changed")
			}
			got.Reason = "caller mutation"
			again, err := worker.CollectorLock()
			if err != nil || again.Reason != "manual" {
				t.Fatal("caller mutated synchronized collector state")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("collector status not observed: %+v %v", got, err)
		}
		time.Sleep(time.Millisecond)
	}
	_, offset := worker.ClientNow().Zone()
	if offset != -3*3600 {
		t.Fatal("status upload did not anchor player timezone")
	}
	link.Uploads <- transport.Upload{Status: &transport.CollectorStatus{Error: "owned lock read failure"}}
	deadline = time.Now().Add(time.Second)
	for {
		if _, err := worker.CollectorLock(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("collector read failure reported as clear")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestConfirmedRecoveryUsesEffectiveLockRatherThanProposedExpiry(t *testing.T) {
	for _, active := range []bool{true, false} {
		t.Run(map[bool]string{true: "longer active lock", false: "expired duplicate acknowledged clear"}[active], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := config.SaveConfig(path, &config.Config{}); err != nil {
				t.Fatal(err)
			}
			store, err := sqlitestore.NewStore(filepath.Join(dir, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			proposed := config.LockFile{TriggerID: "short-request", Reason: "manual", ExpiresAt: time.Now().Add(time.Hour)}
			journal := filepath.Join(dir, "hardstop_deliveries.json")
			if err := config.SaveHardstopDeliveries(journal, map[string]config.HardstopDelivery{proposed.TriggerID: {Lock: proposed}}); err != nil {
				t.Fatal(err)
			}
			_, link, stop := runRecoveredWorker(t, store, path, time.Second)
			command := awaitLockCommand(t, link)
			if command.TriggerID != proposed.TriggerID {
				t.Fatal("pending request identity changed")
			}
			var effective *config.LockFile
			if active {
				effective = &config.LockFile{TriggerID: "existing-long", Reason: "manual", ExpiresAt: time.Now().Add(12 * time.Hour)}
			}
			link.Uploads <- transport.Upload{LockReply: &transport.LockReply{TriggerID: proposed.TriggerID, Lock: effective}}
			deadline := time.Now().Add(time.Second)
			for {
				entries, err := config.LoadHardstopDeliveries(journal)
				if err != nil {
					t.Fatal(err)
				}
				if entries[proposed.TriggerID].Confirmed {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("effective acknowledgement was not persisted")
				}
				time.Sleep(time.Millisecond)
			}
			stop()
			_, link, stop = runRecoveredWorker(t, store, path, time.Second)
			defer stop()
			if active {
				restored := awaitLockCommand(t, link)
				if restored.TriggerID != effective.TriggerID || !restored.ExpiresAt.Equal(effective.ExpiresAt) {
					t.Fatalf("restored proposed clock instead of confirmed protection: %+v", restored)
				}
			} else {
				select {
				case command := <-link.Commands:
					t.Fatalf("acknowledged clear trigger reactivated: %+v", command)
				case <-time.After(80 * time.Millisecond):
				}
			}
		})
	}
}
