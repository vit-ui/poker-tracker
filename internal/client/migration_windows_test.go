package client

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/server"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

// This test only reads process names. Lock commands and enforcement are never
// invoked, and all files/databases/listeners belong to the test directory.
func TestCollectorMigrationPauseDrainAndResume(t *testing.T) {
	running, err := pokerStarsRunning()
	if err != nil {
		t.Fatal(err)
	}
	if running {
		t.Skip("PokerStars is open; positive migration pause requires it closed")
	}
	for _, network := range []bool{false, true} {
		name := "channels"
		if network {
			name = "TLS"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			hh, ts := filepath.Join(dir, "hands"), filepath.Join(dir, "summaries")
			for _, path := range []string{hh, ts} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			configPath := writeConfigWithScreenName(t, dir, "AnonHero01")
			source, err := sqlitestore.NewStore(filepath.Join(dir, "source.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			destination, err := sqlitestore.NewStore(filepath.Join(dir, "destination.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer destination.Close()
			serverLink := transport.NewLink()
			collectorLink := serverLink
			stop := make(chan struct{})
			collectorDead := (<-chan struct{})(stop)
			var listener net.Listener
			var served chan struct{}
			if network {
				cert, key, fp, err := transport.GenerateServerCert()
				if err != nil {
					t.Fatal(err)
				}
				listener, err = transport.Listen("127.0.0.1:0", cert, key)
				if err != nil {
					t.Fatal(err)
				}
				served = make(chan struct{})
				go func() { transport.Serve(listener, "temporary-test-token", serverLink); close(served) }()
				_, portText, _ := net.SplitHostPort(listener.Addr().String())
				port, _ := strconv.Atoi(portText)
				collectorLink, collectorDead, err = transport.DialServer(transport.ConnInfo{Host: "127.0.0.1", Port: port, Token: "temporary-test-token", Fingerprint: fp})
				if err != nil {
					t.Fatal(err)
				}
				// An ordinary upload establishes authentication before worker preparation.
				collectorLink.Uploads <- transport.Upload{Type: transport.ImportStart}
				select {
				case <-serverLink.Uploads:
				case <-time.After(3 * time.Second):
					t.Fatal("TLS authentication did not complete")
				}
			}
			brain, err := server.NewServer(source, configPath, serverLink, make(chan string, 100))
			if err != nil {
				t.Fatal(err)
			}
			brain.QuietPeriod = time.Hour
			workerStopped := make(chan struct{})
			go func() { brain.Run(); close(workerStopped) }()
			control := watcher.NewControl()
			events := make(chan watcher.FileEvent, 10)
			watcherStopped := make(chan error, 1)
			go func() { watcherStopped <- watcher.WatchControlled(stop, hh, ts, events, control) }()
			if _, err := control.SetPaused(false); err != nil {
				t.Fatal(err)
			}
			collector := NewCollector(collectorLink, filepath.Join(dir, "lockfile.json"), control.SetPaused)
			collectorStopped := make(chan struct{})
			go func() { collector.Run(events, configPath, collectorDead); close(collectorStopped) }()
			defer func() {
				close(stop)
				if listener != nil {
					listener.Close()
					<-served
				}
				<-collectorStopped
				if err := <-watcherStopped; err != nil {
					t.Error(err)
				}
				close(serverLink.Uploads)
				select {
				case <-workerStopped:
				case <-time.After(3 * time.Second):
					t.Error("worker did not stop")
				}
				if _, err := os.Stat(filepath.Join(dir, "lockfile.json")); !os.IsNotExist(err) {
					t.Error("migration wrote an enforcement lock")
				}
			}()
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(hh, "table.txt")
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond) // inside the normal two-second debounce
			release := make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			resumed := make(chan error, 1)
			result := make(chan storage.OperationResult, 1)
			brain.Operations <- storage.Operation{PrepareMigration: true, Release: release, Resumed: resumed, Reply: result, Apply: func(store storage.Store) storage.OperationResult {
				snap, err := storage.Migrate(store, destination)
				return storage.OperationResult{Snapshot: &snap, Err: err}
			}}
			select {
			case copied := <-result:
				if copied.Err != nil {
					t.Fatal(copied.Err)
				}
				if copied.Snapshot == nil || len(copied.Snapshot.CashSessions) != 1 || len(copied.Snapshot.Ledger) != 1 {
					t.Fatalf("drained cash missing: %+v", copied.Snapshot)
				}
				if net := copied.Snapshot.Ledger[0].AmountUSD; net < -0.030001 || net > -0.029999 {
					t.Fatalf("copied net %v, want -.03", net)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("migration pause/drain did not complete")
			}
			close(release)
			released = true
			select {
			case err := <-resumed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("collection did not resume")
			}
			if err := os.WriteFile(file, append(data, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			// Acknowledged resume is also reflected in the real watcher, rather than
			// changing only server state. Pausing it yields the accepted new update.
			time.Sleep(100 * time.Millisecond)
			pending, err := control.SetPaused(true)
			if err != nil || len(pending) != 1 || pending[0].Path != file {
				t.Fatalf("resumed watcher pending=%v, error=%v", pending, err)
			}
			if _, err := control.SetPaused(false); err != nil {
				t.Fatal(err)
			}
		})
	}
}
