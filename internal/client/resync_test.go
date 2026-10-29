package client

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

func TestRecoveredFileResyncResetsCollectorOffset(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigWithScreenName(t, dir, "AnonHero01")
	handDir := filepath.Join(dir, "AnonHero01")
	if err := os.Mkdir(handDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Paths.HandHistory = &dir
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(handDir, "table.txt")
	if err := os.WriteFile(source, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	link := transport.NewLink()
	collector := NewCollector(link, NewEnforcer(filepath.Join(dir, "lockfile.json")), nil)
	events := make(chan watcher.FileEvent)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() { collector.Run(events, path, done); close(stopped) }()
	defer func() { close(done); <-stopped }()
	receive := func() transport.Upload {
		select {
		case upload := <-link.Uploads:
			return upload
		case <-time.After(time.Second):
			t.Fatal("missing upload")
			return transport.Upload{}
		}
	}
	events <- watcher.FileEvent{Path: source, Type: "cash"}
	if first := receive(); first.Offset != 0 || string(first.Bytes) != "first" {
		t.Fatalf("first upload %+v", first)
	}
	if err := os.WriteFile(source, []byte("firstsecond"), 0600); err != nil {
		t.Fatal(err)
	}
	events <- watcher.FileEvent{Path: source, Type: "cash"}
	if next := receive(); next.Offset != 5 || string(next.Bytes) != "second" {
		t.Fatalf("delta upload %+v", next)
	}
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("must not upload"), 0600); err != nil {
		t.Fatal(err)
	}
	link.Commands <- transport.Command{Kind: transport.CommandResyncFiles, Files: []string{outside, source}}
	if full := receive(); full.FileID != source || full.Offset != 0 || string(full.Bytes) != "firstsecond" || full.Import {
		t.Fatalf("recovery replacement %+v", full)
	}
}
