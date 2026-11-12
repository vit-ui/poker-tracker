package server

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func TestSavedLockDisplaySurvivesRestart(t *testing.T) {
	for _, tc := range []struct {
		name           string
		known, pending bool
		remaining      time.Duration
		want           string
	}{
		{"unknown", false, false, 0, "status unknown"},
		{"active copy", true, false, time.Hour, "Locked until"},
		{"expired copy", true, false, -time.Hour, ""},
		{"pending", false, true, time.Hour, "Pending PC confirmation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lock := config.LockFile{TriggerID: "manual", ExpiresAt: time.Now().Add(tc.remaining)}
			observation := config.CollectorObservation{Known: tc.known}
			if tc.known {
				observation.Lock = &lock
			}
			if err := config.SaveCollectorObservation(filepath.Join(dir, "collector_observation.json"), observation); err != nil {
				t.Fatal(err)
			}
			if tc.pending {
				if err := config.SaveHardstopDeliveries(filepath.Join(dir, "hardstop_deliveries.json"), map[string]config.HardstopDelivery{"manual": {Lock: lock}}); err != nil {
					t.Fatal(err)
				}
			}
			worker, err := NewServer(nil, filepath.Join(dir, "config.json"), transport.NewLink(), make(chan string, 10))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(worker.workDir) })
			text, err := worker.LockStatusText()
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && !strings.Contains(text, tc.want) {
				t.Fatalf("status=%s", text)
			}
			if tc.remaining < 0 && strings.Contains(text, "Locked until") {
				t.Fatalf("expired lock active: %s", text)
			}
			if _, err := worker.CollectorLock(); err == nil {
				t.Fatal("saved display established live confirmation")
			}
			if err := worker.saveCollectorObservation(nil, "read failed"); err != nil {
				t.Fatal(err)
			}
			saved, err := config.LoadCollectorObservation(filepath.Join(dir, "collector_observation.json"))
			if err != nil {
				t.Fatal(err)
			}
			if saved.Known != observation.Known || (observation.Lock != nil && saved.Lock == nil) {
				t.Fatal("error erased observation")
			}
		})
	}
}

func TestManualRequestQueueKeepsAbsoluteExpiry(t *testing.T) {
	for _, remaining := range []time.Duration{24 * time.Hour, -time.Hour} {
		t.Run(remaining.String(), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			store, err := sqlitestore.NewStore(filepath.Join(dir, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			worker, link, stop := runRecoveredWorker(t, store, path, time.Hour)
			lock := config.LockFile{TriggerID: "manual-offline", ExpiresAt: time.Now().Add(remaining)}
			worker.RequestHardstop(lock)
			got := awaitLockCommand(t, link)
			if got.TriggerID != lock.TriggerID || !got.ExpiresAt.Equal(lock.ExpiresAt) {
				t.Fatal("request timer changed")
			}
			stop()
			_, link, stop = runRecoveredWorker(t, store, path, time.Hour)
			defer stop()
			got = awaitLockCommand(t, link)
			if !got.ExpiresAt.Equal(lock.ExpiresAt) {
				t.Fatal("restart changed expiry")
			}
			active, _, err := config.ApplyHardstop(filepath.Join(dir, "lockfile.json"), got, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if (active != nil) != (remaining > 0) {
				t.Fatalf("active=%+v remaining=%s", active, remaining)
			}
		})
	}
}

func TestOfflineRequestDeliveredOnTLSReconnect(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	worker, link, stop := runRecoveredWorker(t, store, filepath.Join(dir, "config.json"), time.Hour)
	defer stop()
	cert, key, fingerprint, err := transport.GenerateServerCert()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := transport.Listen("127.0.0.1:0", cert, key)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() { defer close(served); transport.Serve(listener, "offline-test-token", link) }()
	defer func() { listener.Close(); <-served }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case <-link.CollectorDone():
			goto offline
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("transport never became offline")
		}
		time.Sleep(time.Millisecond)
	}
offline:
	lock := config.LockFile{TriggerID: "offline-fixed-expiry", ExpiresAt: time.Now().Add(24 * time.Hour), Reason: "manual"}
	worker.RequestHardstop(lock)
	// Synchronize with the worker's durable write before asking the display reader.
	for {
		text, err := worker.LockStatusText()
		if err == nil && strings.Contains(text, "Pending PC confirmation") && strings.Contains(text, "PC is offline") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("offline status=%q err=%v", text, err)
		}
		time.Sleep(time.Millisecond)
	}
	address := listener.Addr().(*net.TCPAddr)
	connection, err := transport.ConnectCollector(transport.ConnInfo{Host: "127.0.0.1", Port: address.Port, Token: "offline-test-token", Fingerprint: fingerprint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	for {
		select {
		case command := <-connection.Link.Commands:
			if command.Kind != transport.CommandLock {
				continue
			}
			if command.Lock.TriggerID != lock.TriggerID || !command.Lock.ExpiresAt.Equal(lock.ExpiresAt) {
				t.Fatal("reconnect changed timer")
			}
			connection.Link.Uploads <- transport.Upload{LockReply: &transport.LockReply{TriggerID: lock.TriggerID, Lock: &lock}, ClientClock: time.Now()}
			goto confirmed
		case <-time.After(2 * time.Second):
			t.Fatal("offline request not delivered")
		}
	}
confirmed:
	deadline = time.Now().Add(2 * time.Second)
	for {
		text, err := worker.LockStatusText()
		if err == nil && strings.Contains(text, "Locked until") && !strings.Contains(text, "Pending PC confirmation") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("confirmed status=%q err=%v", text, err)
		}
		time.Sleep(time.Millisecond)
	}
	connection.Close()
	deadline = time.Now().Add(2 * time.Second)
	for {
		text, err := worker.LockStatusText()
		if err == nil && strings.Contains(text, "Locked until") && strings.Contains(text, "PC is offline") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disconnected status=%q err=%v", text, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSavedLockDisplayCanUseDurableConfirmationReceipt(t *testing.T) {
	worker, _, _ := migrationWorker(t)
	lock := config.LockFile{TriggerID: "confirmed", ExpiresAt: time.Now().Add(time.Hour)}
	path := filepath.Join(filepath.Dir(worker.configPath), "hardstop_deliveries.json")
	if err := config.SaveHardstopDeliveries(path, map[string]config.HardstopDelivery{lock.TriggerID: {Lock: lock, Confirmed: true, EffectiveLock: &lock}}); err != nil {
		t.Fatal(err)
	}
	text, err := worker.LockStatusText()
	if err != nil || !strings.Contains(text, "Locked until") {
		t.Fatalf("status=%s err=%v", text, err)
	}
	if err := worker.saveCollectorObservation(nil, ""); err != nil {
		t.Fatal(err)
	}
	text, err = worker.LockStatusText()
	if err != nil || strings.Contains(text, "Locked until") {
		t.Fatalf("old receipt overrode clear observation: %s %v", text, err)
	}
}

func TestLockStatusPrefersLiveObservationWhenCacheCannotBeSaved(t *testing.T) {
	for _, test := range []struct {
		name                                   string
		cachedActive, liveActive, corruptCache bool
	}{
		{"live lock after saved clear", false, true, false},
		{"live clear after saved lock", true, false, false},
		{"live lock with corrupt cache", false, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker, link, _ := migrationWorker(t)
			cached := config.LockFile{TriggerID: "cached", ExpiresAt: time.Now().Add(2 * time.Hour)}
			var old *config.LockFile
			if test.cachedActive {
				old = &cached
			}
			if err := worker.saveCollectorObservation(old, ""); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(filepath.Dir(worker.configPath), "collector_observation.json")
			if test.corruptCache {
				if err := os.WriteFile(path, []byte("invalid JSON"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(path+".tmp", 0700); err != nil {
				t.Fatal(err)
			}
			lock := config.LockFile{TriggerID: "live", ExpiresAt: time.Now().Add(time.Hour)}
			var current *config.LockFile
			if test.liveActive {
				current = &lock
			}
			link.Uploads <- transport.Upload{Status: &transport.CollectorStatus{Lock: current}}
			deadline := time.Now().Add(time.Second)
			for {
				got, err := worker.CollectorLock()
				if err == nil && ((current == nil && got == nil) || (current != nil && got != nil && got.TriggerID == current.TriggerID)) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("live observation not received: %v %v", got, err)
				}
				time.Sleep(time.Millisecond)
			}
			text, err := worker.LockStatusText()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(text, "Locked until") != test.liveActive {
				t.Fatalf("status used stale cache: %q", text)
			}
			if strings.Contains(text, "unavailable") || strings.Contains(text, "unknown") {
				t.Fatalf("live observation was unavailable: %q", text)
			}
		})
	}
}
