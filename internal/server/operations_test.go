package server

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func TestWorkerOperationsSerializeWithUploadsAndBufferedReplies(t *testing.T) {
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
	link := transport.NewLink()
	alerts := make(chan string, 10)
	server, err := NewServer(store, path, link, alerts)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(server.workDir)
	done := make(chan struct{})
	go func() { server.Run(); close(done) }()
	defer func() { close(link.Uploads); <-done }()
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	first := storage.Operation{Reply: make(chan storage.OperationResult, 1), Apply: func(s storage.Store) storage.OperationResult {
		close(entered)
		<-release
		return storage.OperationResult{Err: s.AppendLedgerEntry("deposit", 1, time.Now(), "")}
	}}
	server.Operations <- first
	<-entered
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	link.Uploads <- transport.Upload{FileID: "import-cash", Type: "cash", Bytes: data, ScreenName: "AnonHero01", Import: true}
	link.Uploads <- transport.Upload{Type: transport.ImportDone}
	accepted := make(chan struct{})
	second := storage.Operation{Reply: make(chan storage.OperationResult, 1), Apply: func(s storage.Store) storage.OperationResult {
		return storage.OperationResult{Err: s.AppendLedgerEntry("deposit", 2, time.Now(), "")}
	}}
	go func() {
		select {
		case server.Operations <- second:
			close(accepted)
		case <-done:
		}
	}()
	select {
	case <-accepted:
		t.Error("worker accepted a second operation during the first")
	case <-time.After(30 * time.Millisecond):
	}
	unblock.Do(func() { close(release) })
	// Do not consume the first reply yet: it must not block subsequent work.
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("unconsumed reply blocked the worker")
	}
	if result := <-second.Reply; result.Err != nil {
		t.Fatal(result.Err)
	}
	select {
	case <-alerts:
	case <-time.After(2 * time.Second):
		t.Fatal("upload was not processed")
	}
	if result := <-first.Reply; result.Err != nil {
		t.Fatal(result.Err)
	}
	rows, err := store.GetLedgerEntriesSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("ledger rows = %d, want 3", len(rows))
	}
	var sum float64
	for _, row := range rows {
		sum += row.AmountUSD
		if !closeEnoughServer(row.RunningTotal, sum) {
			t.Fatalf("running total = %v, sum = %v", row.RunningTotal, sum)
		}
	}
	// Two deposits ($1 + $2) and the committed fixture's -$0.03 session.
	if !closeEnoughServer(sum, 2.97) {
		t.Fatalf("bankroll = %v, want 2.97", sum)
	}
}
