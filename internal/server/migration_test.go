package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func migrationWorker(t *testing.T) (*Server, *transport.Link, *sqlitestore.Store) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(path, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	link := transport.NewLink()
	server, err := NewServer(store, path, link, make(chan string, 100))
	if err != nil {
		t.Fatal(err)
	}
	server.QuietPeriod = time.Hour
	stopped := make(chan struct{})
	go func() { server.Run(); close(stopped) }()
	t.Cleanup(func() {
		close(link.Uploads)
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop")
		}
		store.Close()
		os.RemoveAll(server.workDir)
	})
	return server, link, store
}

func migrationCommand(t *testing.T, link *transport.Link, kind string) transport.Command {
	t.Helper()
	select {
	case command := <-link.Commands:
		if command.Kind != kind || command.RequestID == "" {
			t.Fatalf("command %+v; want %s with identity", command, kind)
		}
		return command
	case <-time.After(3 * time.Second):
		t.Fatalf("missing %s command", kind)
		return transport.Command{}
	}
}

func migrationReply(t *testing.T, reply <-chan storage.OperationResult) storage.OperationResult {
	t.Helper()
	select {
	case result := <-reply:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("worker operation did not complete")
		return storage.OperationResult{}
	}
}

func TestMigrationDrainsCashAndHoldsWorkerUntilResume(t *testing.T) {
	server, link, source := migrationWorker(t)
	if err := source.AppendLedgerEntry("deposit", 1, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	destination, err := sqlitestore.NewStore(filepath.Join(t.TempDir(), "destination.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	resumed := make(chan error, 1)
	reply := make(chan storage.OperationResult, 1)
	operation := storage.Operation{PrepareMigration: true, Release: release, Resumed: resumed, Reply: reply, Apply: func(store storage.Store) storage.OperationResult {
		snap, err := storage.Migrate(store, destination)
		return storage.OperationResult{Snapshot: &snap, Err: err}
	}}
	server.Operations <- operation
	pause := migrationCommand(t, link, transport.CommandPauseCollection)
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	link.Uploads <- transport.Upload{FileID: "drained-table", Type: "cash", Bytes: data, ScreenName: "AnonHero01"}
	link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: pause.RequestID}}
	result := migrationReply(t, reply)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.Snapshot == nil || len(result.Snapshot.CashSessions) != 1 || len(result.Snapshot.Ledger) != 2 {
		t.Fatalf("pending cash missing from copy: %+v", result.Snapshot)
	}
	if got := result.Snapshot.Ledger[1].RunningTotal; got < .969999 || got > .970001 {
		t.Fatalf("copied bankroll %v, want .97", got)
	}
	next := storage.Operation{Reply: make(chan storage.OperationResult, 1), Apply: func(store storage.Store) storage.OperationResult {
		return storage.OperationResult{Err: store.AppendLedgerEntry("deposit", 2, time.Now(), "")}
	}}
	accepted := make(chan struct{})
	cancel := make(chan struct{})
	defer close(cancel)
	go func() {
		select {
		case server.Operations <- next:
			close(accepted)
		case <-cancel:
		}
	}()
	select {
	case <-accepted:
		t.Fatal("worker accepted a write while held for restart")
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	resume := migrationCommand(t, link, transport.CommandResumeCollection)
	if resume.RequestID == pause.RequestID {
		t.Fatal("pause and resume reused an acknowledgement identity")
	}
	link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: pause.RequestID}}
	select {
	case <-accepted:
		t.Fatal("stale pause acknowledgement released worker")
	case <-time.After(50 * time.Millisecond):
	}
	link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: resume.RequestID}}
	select {
	case err := <-resumed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resume was not acknowledged")
	}
	if result := migrationReply(t, next.Reply); result.Err != nil {
		t.Fatal(result.Err)
	}
	total, err := source.GetCurrentBankroll()
	if err != nil || total < 2.969999 || total > 2.970001 {
		t.Fatalf("resumed source total %v, error %v", total, err)
	}
}

func TestMigrationCollectorRefusalDoesNotCopy(t *testing.T) {
	server, link, _ := migrationWorker(t)
	reply := make(chan storage.OperationResult, 1)
	copied := false
	server.Operations <- storage.Operation{PrepareMigration: true, Reply: reply, Apply: func(storage.Store) storage.OperationResult { copied = true; return storage.OperationResult{} }}
	pause := migrationCommand(t, link, transport.CommandPauseCollection)
	link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: pause.RequestID, Error: "close PokerStars before changing storage"}}
	result := migrationReply(t, reply)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "close PokerStars") || copied {
		t.Fatalf("refusal result %+v, copied=%v", result, copied)
	}
}

func TestMigrationDrainFailureResumesWithoutCopy(t *testing.T) {
	server, link, _ := migrationWorker(t)
	reply := make(chan storage.OperationResult, 1)
	copied := false
	server.Operations <- storage.Operation{PrepareMigration: true, Reply: reply, Apply: func(storage.Store) storage.OperationResult { copied = true; return storage.OperationResult{} }}
	pause := migrationCommand(t, link, transport.CommandPauseCollection)
	if err := os.RemoveAll(server.workDir); err != nil {
		t.Fatal(err)
	}
	link.Uploads <- transport.Upload{FileID: "broken-summary", Type: "tournament", ScreenName: "AnonHero01", Bytes: []byte("not a tournament summary")}
	link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: pause.RequestID}}
	resume := migrationCommand(t, link, transport.CommandResumeCollection)
	link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: resume.RequestID}}
	result := migrationReply(t, reply)
	if result.Err == nil || copied {
		t.Fatalf("failed drain result %+v, copied=%v", result, copied)
	}
}

func TestFailedCashPersistenceRetainsPendingSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(path, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(filepath.Join(dir, "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, path, transport.NewLink(), make(chan string, 10))
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(server.workDir)
	store.Close()
	pending := map[string]parsers.CashSession{"table": {Date: time.Now(), FirstHandID: "pending-hand", NetUSD: -1, BuyInUSD: 2}}
	timers := map[string]*time.Timer{"table": time.NewTimer(time.Hour)}
	defer timers["table"].Stop()
	if err := server.endCashSession("table", pending, timers, map[string]bool{}, func(string, int) { t.Fatal("failed persistence evaluated enforcement") }); err == nil {
		t.Fatal("closed storage accepted pending cash")
	}
	if len(pending) != 1 || pending["table"].FirstHandID != "pending-hand" || len(timers) != 1 {
		t.Fatal("failed persistence discarded pending state")
	}
}

func TestMigrationCopyFailureWaitsForResumeAcknowledgement(t *testing.T) {
	server, link, _ := migrationWorker(t)
	reply := make(chan storage.OperationResult, 1)
	server.Operations <- storage.Operation{PrepareMigration: true, Reply: reply, Apply: func(storage.Store) storage.OperationResult {
		return storage.OperationResult{Err: fmt.Errorf("injected copy failure")}
	}}
	pause := migrationCommand(t, link, transport.CommandPauseCollection)
	link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: pause.RequestID}}
	resume := migrationCommand(t, link, transport.CommandResumeCollection)
	select {
	case result := <-reply:
		t.Fatalf("copy failure returned before collection resumed: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	link.Uploads <- transport.Upload{CollectionReply: &transport.CollectionReply{RequestID: resume.RequestID, Error: "watcher stopped"}}
	result := migrationReply(t, reply)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "injected copy failure") || !strings.Contains(result.Err.Error(), "watcher stopped") {
		t.Fatalf("lost copy/resume failure: %+v", result)
	}
}

func TestMigrationRefusesDisconnectedCollector(t *testing.T) {
	server, link, _ := migrationWorker(t)
	cert, key, _, err := transport.GenerateServerCert()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := transport.Listen("127.0.0.1:0", cert, key)
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	transport.Serve(listener, "test-token", link)
	reply := make(chan storage.OperationResult, 1)
	copied := false
	server.Operations <- storage.Operation{PrepareMigration: true, Reply: reply, Apply: func(storage.Store) storage.OperationResult { copied = true; return storage.OperationResult{} }}
	result := migrationReply(t, reply)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "disconnected") || copied {
		t.Fatalf("disconnected result %+v, copied=%v", result, copied)
	}
}
