package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func TestDeploymentPreparationRequiresDurableProtection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		saved      bool
		confirmed  bool
		cash       bool
		unsaved    bool
		wantRefuse bool
	}{
		{name: "no protection"},
		{name: "saved unconfirmed", saved: true, wantRefuse: true},
		{name: "saved confirmed", saved: true, confirmed: true},
		{name: "cash finalization triggers protection", cash: true, wantRefuse: true},
		{name: "in-memory request after failed save", cash: true, unsaved: true, wantRefuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, link, store := migrationWorker(t)
			root := filepath.Dir(worker.configPath)
			journal := filepath.Join(root, "hardstop_deliveries.json")
			lock := config.LockFile{TriggerID: "original", Reason: "test protection", TriggeredAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
			commandOfKind := func(kind string) transport.Command {
				t.Helper()
				deadline := time.After(3 * time.Second)
				for {
					select {
					case command := <-link.Commands:
						if command.Kind == transport.CommandLock {
							lock = *command.Lock
						}
						if command.Kind == kind {
							return command
						}
					case <-deadline:
						t.Fatalf("missing %s command", kind)
					}
				}
			}
			if tc.saved {
				if err := config.SaveHardstopDeliveries(journal, map[string]config.HardstopDelivery{lock.TriggerID: {Lock: lock, Confirmed: tc.confirmed, EffectiveLock: &lock}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.cash {
				cfg := &config.Config{}
				streak, duration := 1, 1
				cfg.Hardstop.ConsecutiveLosses, cfg.Hardstop.DurationHours = &streak, &duration
				if err := config.SaveConfig(worker.configPath, cfg); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
				if err != nil {
					t.Fatal(err)
				}
				link.Uploads <- transport.Upload{FileID: "table", Type: "cash", Bytes: data, ScreenName: "AnonHero01"}
				awaitPendingCash(t, filepath.Join(root, "pending_cash.json"), func(entries []config.PendingCash) bool { return len(entries) == 1 })
			}
			if tc.unsaved {
				// Force finalization to retain an intent only in worker memory.
				if err := os.Mkdir(journal+".tmp", 0700); err != nil {
					t.Fatal(err)
				}
				worker.Operations <- storage.Operation{FinalizeAllCash: true, PrepareRestart: true, Reply: make(chan storage.OperationResult, 1)}
				pause := commandOfKind(transport.CommandDrainCollection)
				link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: pause.RequestID}}
				lock = awaitLockCommand(t, link)
				resume := commandOfKind(transport.CommandResumeCollection)
				link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: resume.RequestID}}
				if _, err := os.Stat(journal); !os.IsNotExist(err) {
					t.Fatalf("intent unexpectedly saved: %v", err)
				}
				if err := os.Remove(journal + ".tmp"); err != nil {
					t.Fatal(err)
				}
			}

			release, resumed := make(chan struct{}), make(chan error, 1)
			defer close(release)
			copied := false
			reply := make(chan storage.OperationResult, 1)
			worker.Operations <- storage.Operation{PrepareMigration: true, RequireConfirmedProtection: true, Release: release, Resumed: resumed, Reply: reply, Apply: func(source storage.Store) storage.OperationResult {
				copied = true
				snapshot, err := source.ExportSnapshot()
				return storage.OperationResult{Snapshot: &snapshot, Err: err}
			}}
			pause := commandOfKind(transport.CommandPauseCollection)
			link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: pause.RequestID}}
			if tc.wantRefuse {
				resume := commandOfKind(transport.CommandResumeCollection)
				link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: resume.RequestID}}
			}
			result := migrationReply(t, reply)
			if tc.wantRefuse {
				wantError := "protection is unconfirmed"
				if tc.unsaved {
					wantError = "hard-stop delivery could not be persisted"
				}
				if result.Err == nil || !strings.Contains(result.Err.Error(), wantError) || copied {
					t.Fatalf("unconfirmed protection exported: result=%+v copied=%v", result, copied)
				}
				entries, err := config.LoadHardstopDeliveries(journal)
				if err != nil || (!tc.unsaved && len(entries) != 1) {
					t.Fatalf("replay intent lost: %+v, %v", entries, err)
				}
				for _, entry := range entries {
					if entry.Confirmed || !entry.Lock.ExpiresAt.Equal(lock.ExpiresAt) {
						t.Fatalf("replay changed original protection: %+v, want %+v", entry, lock)
					}
				}
				link.Uploads <- transport.Upload{LockReply: &transport.LockReply{TriggerID: lock.TriggerID, Lock: &lock}}
				// A subsequent preparation can succeed only after a saved receipt.
				worker.Operations <- storage.Operation{PrepareMigration: true, RequireConfirmedProtection: true, Release: release, Resumed: resumed, Reply: reply, Apply: func(source storage.Store) storage.OperationResult {
					copied = true
					return storage.OperationResult{}
				}}
				pause = commandOfKind(transport.CommandPauseCollection)
				link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: pause.RequestID}}
				result = migrationReply(t, reply)
			}
			if result.Err != nil || !copied {
				t.Fatalf("confirmed preparation failed: %+v, copied=%v", result, copied)
			}
			if tc.cash {
				snapshot, err := store.ExportSnapshot()
				if err != nil || len(snapshot.CashSessions) != 1 || len(snapshot.Ledger) != 1 || snapshot.Ledger[0].AmountCents != -3 {
					t.Fatalf("cash finalization lost or duplicated the 3-cent loss: %+v, %v", snapshot, err)
				}
			}
		})
	}
}
