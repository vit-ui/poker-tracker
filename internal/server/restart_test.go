package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/client"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

func TestConfigurationRestartDrainsAcceptedCashBeforeChangingSettings(t *testing.T) {
	for _, finalize := range []bool{false, true} {
		t.Run(fmt.Sprintf("finalize=%v", finalize), func(t *testing.T) {
			worker, link, store := migrationWorker(t)
			cfg, err := config.LoadConfig(worker.configPath)
			if err != nil {
				t.Fatal(err)
			}
			name := "AnonHero01"
			cfg.Setup.PokerStarsScreenName = &name
			if err := config.SaveConfig(worker.configPath, cfg); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(t.TempDir(), "pending.txt")
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			collector := client.NewCollector(link, client.NewEnforcer(filepath.Join(t.TempDir(), "lockfile.json")), func(paused bool) ([]watcher.FileEvent, error) {
				if paused {
					return []watcher.FileEvent{{Path: source, Type: "cash"}}, nil
				}
				return nil, nil
			})
			done, stopped := make(chan struct{}), make(chan struct{})
			go func() { collector.Run(make(chan watcher.FileEvent), worker.configPath, done); close(stopped) }()
			defer func() { close(done); <-stopped }()
			release, resumed, reply := make(chan struct{}), make(chan error, 1), make(chan storage.OperationResult, 1)
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			worker.Operations <- storage.Operation{PrepareRestart: true, FinalizeAllCash: finalize, Release: release, Resumed: resumed, Reply: reply, Apply: func(source storage.Store) storage.OperationResult {
				pending, err := config.LoadPendingCash(filepath.Join(filepath.Dir(worker.configPath), "pending_cash.json"))
				if err != nil {
					return storage.OperationResult{Err: err}
				}
				snapshot, err := source.ExportSnapshot()
				if err != nil {
					return storage.OperationResult{Err: err}
				}
				if finalize {
					if len(pending) != 0 || len(snapshot.Ledger) != 1 || snapshot.Ledger[0].AmountCents != -3 {
						return storage.OperationResult{Err: fmt.Errorf("sheet change overtook cash finalization: %+v %+v", pending, snapshot)}
					}
				} else if len(pending) != 1 || pending[0].Session.Hands != 4 || len(snapshot.Ledger) != 0 {
					return storage.OperationResult{Err: fmt.Errorf("restart lost cash or finalized early: %+v %+v", pending, snapshot)}
				}
				return storage.OperationResult{}
			}}
			result := migrationReply(t, reply)
			if result.Err != nil {
				t.Fatal(result.Err)
			}
			next := make(chan storage.OperationResult, 1)
			accepted := make(chan struct{})
			go func() {
				worker.Operations <- storage.Operation{Reply: next, Apply: func(source storage.Store) storage.OperationResult {
					return storage.OperationResult{Err: source.AppendLedgerEntry("deposit", 100, time.Now(), "money:after-restart")}
				}}
				close(accepted)
			}()
			select {
			case <-accepted:
				t.Fatal("worker accepted writes before restart rollback")
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-resumed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("collection did not resume")
			}
			if result := migrationReply(t, next); result.Err != nil {
				t.Fatal(result.Err)
			}
			bankrollCents, err := store.GetCurrentBankroll()
			wantCents := money.Cents(100)
			if finalize {
				wantCents = 97
			}
			if err != nil || bankrollCents != wantCents {
				t.Fatalf("bankroll=%v %v, want %v", bankrollCents, err, wantCents)
			}
		})
	}
}

func TestConfigurationRestartWithoutConnectedCollector(t *testing.T) {
	worker, link, _ := migrationWorker(t)
	cert, key, _, err := transport.GenerateServerCert()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := transport.Listen("127.0.0.1:0", cert, key)
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	transport.Serve(listener, "owned-token", link)
	release, resumed, reply := make(chan struct{}), make(chan error, 1), make(chan storage.OperationResult, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	link.Uploads <- transport.Upload{FileID: "last-disconnected-file", Type: "cash", Bytes: data, ScreenName: "AnonHero01"}
	worker.Operations <- storage.Operation{PrepareRestart: true, Release: release, Resumed: resumed, Reply: reply, Apply: func(storage.Store) storage.OperationResult {
		pending, err := config.LoadPendingCash(filepath.Join(filepath.Dir(worker.configPath), "pending_cash.json"))
		if err != nil || len(pending) != 1 {
			return storage.OperationResult{Err: fmt.Errorf("offline preparation overtook accepted uploads: %+v %v", pending, err)}
		}
		return storage.OperationResult{Found: true}
	}}
	if result := migrationReply(t, reply); result.Err != nil || !result.Found {
		t.Fatalf("offline restart: %+v", result)
	}
	close(release)
	select {
	case err := <-resumed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("offline rollback waited for collector")
	}
}
