package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func TestCollectorClockRestoredBeforeUploads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int
	}{{"west", -3 * 3600}, {"east", 9 * 3600}, {"UTC", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			newWorker := func() *Server {
				worker, err := NewServer(nil, path, transport.NewLink(), make(chan string))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(worker.workDir) })
				return worker
			}
			first := newWorker()
			first.updateClock(time.Date(2026, 11, 12, 0, 30, 0, 0, time.FixedZone(tc.name, tc.offset)))
			second := newWorker()
			_, offset := second.ClientNow().Zone()
			if offset != tc.offset {
				t.Fatalf("offset=%d want %d", offset, tc.offset)
			}
			// At 02:30 UTC, -03:00 must retain the preceding calendar day.
			instant := time.Date(2026, 11, 12, 2, 30, 0, 0, time.UTC)
			got := storage.AsStoredClock(instant.In(second.clientLoc))
			want := instant.Add(time.Duration(tc.offset) * time.Second)
			if got.Day() != want.Day() || got.Hour() != want.Hour() {
				t.Fatalf("stored=%s want wall digits %s", got, want)
			}
			second.updateClock(instant.In(time.FixedZone("refreshed", 2*3600)))
			third := newWorker()
			_, offset = third.ClientNow().Zone()
			if offset != 2*3600 {
				t.Fatal("reconnect clock not restored")
			}
		})
	}
}

func TestCashRecoveryPreservesLatestCollectorClock(t *testing.T) {
	for _, test := range []struct {
		name  string
		saved bool
		want  int
	}{
		{"saved observation wins over all snapshots", true, 9 * 3600},
		{"freshest historical clock when no observation exists", false, 2 * 3600},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.json")
			if err := config.SaveConfig(path, &config.Config{}); err != nil {
				t.Fatal(err)
			}
			clockPath := filepath.Join(root, "collector_clock.json")
			if test.saved {
				if err := config.SaveCollectorClock(clockPath, config.CollectorClock{Name: "saved", OffsetSeconds: 9 * 3600}); err != nil {
					t.Fatal(err)
				}
			}
			session := recoveryFixture(t)
			now := time.Now()
			if err := config.SavePendingCash(filepath.Join(root, "pending_cash.json"), []config.PendingCash{
				{FileID: "a-newer", Session: session, LastActivity: now, ClientClock: now.Add(-time.Minute).In(time.FixedZone("newer", 2*3600))},
				{FileID: "z-older", Session: session, LastActivity: now, ClientClock: now.Add(-time.Hour).In(time.FixedZone("older", -3*3600))},
			}); err != nil {
				t.Fatal(err)
			}
			store, err := sqlitestore.NewStore(filepath.Join(root, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			worker, _, stop := runRecoveredWorker(t, store, path, time.Hour)
			defer stop()
			<-worker.Ready
			_, got := worker.ClientNow().Zone()
			if got != test.want {
				t.Fatalf("offset=%d want %d", got, test.want)
			}
			saved, err := config.LoadCollectorClock(clockPath)
			if err != nil {
				t.Fatal(err)
			}
			if saved == nil || saved.OffsetSeconds != test.want {
				t.Fatalf("saved clock=%+v want offset %d", saved, test.want)
			}
		})
	}
}
