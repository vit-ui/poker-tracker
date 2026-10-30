package client

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func collectionTestLink(t *testing.T, network bool) (*transport.Link, *transport.Link, <-chan struct{}, func()) {
	t.Helper()
	if !network {
		link := transport.NewLink()
		done := make(chan struct{})
		var once sync.Once
		return link, link, done, func() { once.Do(func() { close(done) }) }
	}
	cert, key, fp, err := transport.GenerateServerCert()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.Listen("127.0.0.1:0", cert, key)
	if err != nil {
		t.Fatal(err)
	}
	link := transport.NewLink()
	stopped := make(chan struct{})
	go func() { transport.Serve(ln, "collection-test", link); close(stopped) }()
	host, portText, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portText)
	peer, done, err := transport.DialServer(transport.ConnInfo{Host: host, Port: port, Token: "collection-test", Fingerprint: fp})
	if err != nil {
		ln.Close()
		<-stopped
		t.Fatal(err)
	}
	var once sync.Once
	return link, peer, done, func() { once.Do(func() { ln.Close(); <-stopped }) }
}

func collectionUpload(t *testing.T, link *transport.Link) transport.Upload {
	t.Helper()
	select {
	case up := <-link.Uploads:
		return up
	case <-time.After(5 * time.Second):
		t.Fatal("missing collection upload")
		return transport.Upload{}
	}
}

func collectionReply(t *testing.T, link *transport.Link, id string) transport.CollectorConfigReply {
	t.Helper()
	up := collectionUpload(t, link)
	if up.ConfigReply == nil || up.ConfigReply.RequestID != id {
		t.Fatalf("want config reply %s, got %+v", id, up)
	}
	return *up.ConfigReply
}

func collectionFolders(t *testing.T, root, name string) (string, string) {
	t.Helper()
	hh, ts := filepath.Join(root, "hands"), filepath.Join(root, "summaries")
	for _, base := range []string{hh, ts} {
		if err := os.MkdirAll(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return hh, ts
}

func TestCollectorSettingsReplaceDrainAndRollbackOverBothLinks(t *testing.T) {
	for _, network := range []bool{false, true} {
		t.Run(map[bool]string{false: "channel", true: "TLS"}[network], func(t *testing.T) {
			root := t.TempDir()
			name, nextName := "Hero", "NextHero"
			hh, ts := collectionFolders(t, filepath.Join(root, "old"), name)
			nextHH, nextTS := collectionFolders(t, filepath.Join(root, "new"), nextName)
			path := filepath.Join(root, "config.json")
			if err := config.SaveConfig(path, &config.Config{Setup: config.Setup{PokerStarsScreenName: &name}, Paths: config.Paths{HandHistory: &hh, TournamentSummary: &ts}}); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(root, "lockfile.json")
			lock := config.LockFile{TriggerID: "preserve", ExpiresAt: time.Now().Add(time.Hour), Reason: "existing protection"}
			if err := config.SaveLock(lockPath, &lock); err != nil {
				t.Fatal(err)
			}
			link, peer, done, disconnect := collectionTestLink(t, network)
			collector := NewCollector(peer, NewEnforcer(lockPath), nil)
			stopped := make(chan error, 1)
			go func() { stopped <- collector.Serve(path, done) }()
			defer func() {
				disconnect()
				if err := <-stopped; err != nil {
					t.Error(err)
				}
			}()
			up := collectionUpload(t, link)
			if up.Status == nil || !up.Status.Watching || up.Status.Settings == nil || *up.Status.Settings.ScreenName != name {
				t.Fatalf("initial status: %+v", up)
			}
			oldFile := filepath.Join(hh, name, "old.txt")
			if err := os.WriteFile(oldFile, []byte("accepted before replacement"), 0600); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			// A concurrent server edit must survive the collector's field-only save.
			if _, err := config.UpdateConfig(path, func(cfg *config.Config) error { hour := 9; cfg.Schedule.DayStartHour = &hour; return nil }); err != nil {
				t.Fatal(err)
			}
			patch := config.CollectorSettings{ScreenName: &nextName, HandHistory: &nextHH, TournamentSummary: &nextTS}
			link.Commands <- transport.Command{Kind: transport.CommandCollectorConfig, RequestID: "replace", Settings: &patch}
			drained := collectionUpload(t, link)
			if drained.ConfigReply != nil || drained.FileID != oldFile || drained.ScreenName != name || string(drained.Bytes) != "accepted before replacement" {
				t.Fatalf("old event was not drained using old settings: %+v", drained)
			}
			reply := collectionReply(t, link, "replace")
			if reply.Error != "" || !reply.Watching || *reply.Settings.ScreenName != nextName {
				t.Fatalf("replacement: %+v", reply)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Schedule.DayStartHour == nil || *cfg.Schedule.DayStartHour != 9 || *cfg.Setup.PokerStarsScreenName != nextName {
				t.Fatalf("lost config: %+v", cfg)
			}
			unchanged, err := config.LoadLock(lockPath)
			if err != nil || !unchanged.ExpiresAt.Equal(lock.ExpiresAt) {
				t.Fatalf("settings changed active protection: %+v %v", unchanged, err)
			}
			invalid := filepath.Join(root, "missing")
			link.Commands <- transport.Command{Kind: transport.CommandCollectorConfig, RequestID: "invalid", Settings: &config.CollectorSettings{HandHistory: &invalid}}
			reply = collectionReply(t, link, "invalid")
			if reply.Error == "" || !reply.Watching || *reply.Settings.HandHistory != nextHH {
				t.Fatalf("invalid path changed active settings: %+v", reply)
			}
			// Fail the actual atomic config replacement after the new watcher is ready.
			rollbackHH, _ := collectionFolders(t, filepath.Join(root, "rollback"), nextName)
			if err := os.Mkdir(path+".tmp", 0700); err != nil {
				t.Fatal(err)
			}
			link.Commands <- transport.Command{Kind: transport.CommandCollectorConfig, RequestID: "save-fails", Settings: &config.CollectorSettings{HandHistory: &rollbackHH}}
			reply = collectionReply(t, link, "save-fails")
			if reply.Error == "" || !reply.Watching || *reply.Settings.HandHistory != nextHH {
				t.Fatalf("failed persistence did not restore old watcher: %+v", reply)
			}
			if err := os.Remove(path + ".tmp"); err != nil {
				t.Fatal(err)
			}
			// Repeated desired values are idempotent, and queries return PC-owned data.
			link.Commands <- transport.Command{Kind: transport.CommandCollectorConfig, RequestID: "repeat", Settings: &patch}
			if reply := collectionReply(t, link, "repeat"); reply.Error != "" || !reply.Watching {
				t.Fatalf("replay: %+v", reply)
			}
			link.Commands <- transport.Command{Kind: transport.CommandCollectorConfig, RequestID: "query"}
			if reply := collectionReply(t, link, "query"); reply.Error != "" || *reply.Settings.HandHistory != nextHH {
				t.Fatalf("query: %+v", reply)
			}
			file := filepath.Join(nextHH, nextName, "new.txt")
			if err := os.WriteFile(file, []byte("new collection"), 0600); err != nil {
				t.Fatal(err)
			}
			received := collectionUpload(t, link)
			if received.FileID != file || received.ScreenName != nextName || string(received.Bytes) != "new collection" {
				t.Fatalf("new watcher not active: %+v", received)
			}
		})
	}
}

func TestCollectorStaysConnectedWithMissingFoldersAndAcceptsCorrection(t *testing.T) {
	root := t.TempDir()
	name := "Hero"
	missing := filepath.Join(root, "deleted")
	path := filepath.Join(root, "config.json")
	if err := config.SaveConfig(path, &config.Config{Setup: config.Setup{PokerStarsScreenName: &name}, Paths: config.Paths{HandHistory: &missing, TournamentSummary: &missing}}); err != nil {
		t.Fatal(err)
	}
	link := transport.NewLink()
	done := make(chan struct{})
	stopped := make(chan error, 1)
	collector := NewCollector(link, NewEnforcer(filepath.Join(root, "lockfile.json")), nil)
	go func() { stopped <- collector.Serve(path, done) }()
	defer func() {
		close(done)
		if err := <-stopped; err != nil {
			t.Error(err)
		}
	}()
	up := collectionUpload(t, link)
	if up.Status == nil || up.Status.Watching || up.Status.CollectionError == "" || up.Status.Settings == nil {
		t.Fatalf("missing folders not reported inactive: %+v", up)
	}
	// An expired lock exercises command handling without terminating any process.
	expired := config.LockFile{TriggerID: "expired-safe-check", ExpiresAt: time.Now().Add(-time.Hour)}
	link.Commands <- transport.Command{Kind: transport.CommandLock, Lock: &expired}
	if up := collectionUpload(t, link); up.LockReply == nil || up.LockReply.TriggerID != expired.TriggerID || up.LockReply.Error != "" {
		t.Fatalf("inactive collection blocked enforcement commands: %+v", up)
	}
	hh, ts := collectionFolders(t, root, name)
	link.Commands <- transport.Command{Kind: transport.CommandCollectorConfig, RequestID: "recover", Settings: &config.CollectorSettings{HandHistory: &hh, TournamentSummary: &ts}}
	reply := collectionReply(t, link, "recover")
	if reply.Error != "" || !reply.Watching || reply.CollectionError != "" {
		t.Fatalf("queued correction did not recover collection: %+v", reply)
	}
	// Re-applying identical settings also repairs a watcher missing at startup.
	disconnectFile := filepath.Join(hh, name, "recovered.txt")
	if err := os.WriteFile(disconnectFile, []byte("recovered"), 0600); err != nil {
		t.Fatal(err)
	}
	if up := collectionUpload(t, link); up.FileID != disconnectFile {
		t.Fatalf("recovered watcher: %+v", up)
	}
}
