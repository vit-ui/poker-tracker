package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
