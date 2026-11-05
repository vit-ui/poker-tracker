package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func TestOwnerShutdownDrainsUploadsPersistsCashAndRemovesWorkDir(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart-held=%v", held), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.json")
			name := "AnonHero01"
			if err := config.SaveConfig(path, &config.Config{Setup: config.Setup{PokerStarsScreenName: &name}}); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(root, "lockfile.json")
			lock := config.LockFile{TriggerID: "shutdown-lock", ExpiresAt: time.Now().Add(time.Hour)}
			if err := config.SaveLock(lockPath, &lock); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			db, err := sqlitestore.NewStore(filepath.Join(root, "tracker.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			link := transport.NewLink()
			worker, err := NewServer(db, path, link, make(chan string, 100))
			if err != nil {
				t.Fatal(err)
			}
			worker.QuietPeriod = time.Hour
			drain, finished := make(chan struct{}), make(chan error, 1)
			worker.Drain = drain
			go func() { finished <- worker.Run() }()
			reply := make(chan storage.OperationResult, 1)
			worker.Operations <- storage.Operation{Reply: reply, Apply: func(store storage.Store) storage.OperationResult {
				return storage.OperationResult{Err: store.AppendLedgerEntry("deposit", 100, time.Now(), "money:shutdown")}
			}}
			if result := migrationReply(t, reply); result.Err != nil {
				t.Fatal(result.Err)
			}
			if held {
				worker.Operations <- storage.Operation{PrepareRestart: true, Release: make(chan struct{}), Reply: reply, Apply: func(storage.Store) storage.OperationResult { return storage.OperationResult{} }}
				cmd := migrationCommand(t, link, transport.CommandDrainCollection)
				link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: cmd.RequestID}}
				if result := migrationReply(t, reply); result.Err != nil {
					t.Fatal(result.Err)
				}
			}
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
			if err != nil {
				t.Fatal(err)
			}
			link.Uploads <- transport.Upload{FileID: "accepted.txt", Type: "cash", Bytes: data, ScreenName: name, ClientClock: time.Now()}
			close(drain)
			close(link.Uploads) // No producer may send after the owner joins it.
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown left worker held")
			}
			pending, err := config.LoadPendingCash(filepath.Join(root, "pending_cash.json"))
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending=%+v, err=%v", pending, err)
			}
			if pending[0].Session.Hands != 4 || pending[0].Session.NetCents != -3 {
				t.Fatalf("cash snapshot=%+v", pending[0])
			}
			snapshot, err := db.ExportSnapshot()
			if err != nil || len(snapshot.Ledger) != 1 || snapshot.Ledger[0].AmountCents != 100 || snapshot.Ledger[0].SessionID != "money:shutdown" {
				t.Fatalf("accepted movement=%+v, %v", snapshot, err)
			}
			if _, err := os.Stat(worker.workDir); !os.IsNotExist(err) {
				t.Fatalf("working directory survived cleanup: %v", err)
			}
			after, err := os.ReadFile(lockPath)
			if err != nil || string(after) != string(before) {
				t.Fatalf("lock changed: %s, %v", after, err)
			}
		})
	}
}
