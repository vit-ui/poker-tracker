package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/client"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/server"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

func TestScanMixedFile(t *testing.T) {
	hero := "AnonExportHero"
	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "scan", "mixed_export.txt"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	cfg := &config.Config{}
	cfg.Setup.PokerStarsScreenName = &hero
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	db, err := sqlitestore.NewStore(filepath.Join(root, "tracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	link := transport.NewLink()
	alerts := make(chan string, 20)
	worker, err := server.NewServer(db, configPath, link, alerts)
	if err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run() }()
	defer func() {
		close(link.Uploads)
		if err := <-workerDone; err != nil {
			t.Error(err)
		}
	}()
	collector := client.NewCollector(link, client.NewEnforcer(filepath.Join(root, "lockfile.json")), nil)
	events := make(chan watcher.FileEvent)
	collectorDone := make(chan error, 1)
	go func() { collectorDone <- collector.Run(events, configPath, nil) }()
	defer func() {
		close(events)
		if err := <-collectorDone; err != nil {
			t.Error(err)
		}
	}()
	<-collector.Ready
	for _, test := range []struct {
		name       string
		collector  string
		attachment string
	}{
		{name: "collector path", collector: path},
		{name: "Telegram attachment retry", attachment: path},
	} {
		t.Run(test.name, func(t *testing.T) {
			reply := make(chan storage.OperationResult, 1)
			worker.Operations <- storage.Operation{ScanPath: test.collector, ScanAttachment: test.attachment, Reply: reply}
			select {
			case result := <-reply:
				if result.Err != nil || result.Scan != (storage.ScanTally{Cash: 3, TournamentHistories: 2}) {
					t.Fatalf("scan result = %+v", result)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("scan timed out")
			}
			snapshot, err := db.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.CashSessions) != 3 || len(snapshot.Tournaments) != 2 || len(snapshot.Ledger) != 3 {
				t.Fatalf("unexpected row counts: %+v", snapshot)
			}
			want := []struct {
				hands int
				net   int64
			}{{2, -9}, {1, 28}, {1, 63}}
			for i, row := range snapshot.CashSessions {
				if row.Hands != want[i].hands || int64(row.NetCents) != want[i].net {
					t.Errorf("cash row %d = %+v, want %+v", i, row, want[i])
				}
			}
			for _, record := range snapshot.Tournaments {
				if record.Hands == nil || *record.Hands != 1 {
					t.Errorf("tournament hand count = %+v", record)
				}
			}
			if len(alerts) != 0 {
				t.Fatal("scan produced live alerts")
			}
		})
	}
}

func TestFileScanDoesNotDuplicateFullLiveCashFile(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "scan", "mixed_export.txt"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(fixture), "PokerStars Hand #")
	var contents string
	for _, id := range []string{"281000000013", "281000000017", "281000000012"} {
		for _, part := range parts {
			if strings.HasPrefix(part, id) {
				contents += "PokerStars Hand #" + part
			}
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cash.txt")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	session, _, err := parsers.ParseCashFile(path, "AnonExportHero")
	if err != nil || session.Hands != 3 || session.NetCents != 54 {
		t.Fatalf("unexpected live parse: %+v (%v)", session, err)
	}
	db, err := sqlitestore.NewStore(filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.AppendCashSession(*session); err != nil {
		t.Fatal(err)
	}
	var tally storage.ScanTally
	if err := tally.HandHistory(db, path, "AnonExportHero"); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendCashSession(*session); err != nil {
		t.Fatal(err)
	}
	bankroll, err := db.GetCurrentBankroll()
	if err != nil {
		t.Fatal(err)
	}
	if bankroll != session.NetCents {
		t.Fatalf("scan and live upsert yield %d cents, want %d cents", bankroll, session.NetCents)
	}
}
