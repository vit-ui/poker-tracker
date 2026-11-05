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

func TestOwnerShutdownDrainsBufferedAndDebouncedFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	name := "AnonHero01"
	if err := config.SaveConfig(path, &config.Config{Setup: config.Setup{PokerStarsScreenName: &name}}); err != nil {
		t.Fatal(err)
	}
	events := make(chan watcher.FileEvent, 2)
	expected := map[string]string{}
	var pending []watcher.FileEvent
	for i, content := range []string{"buffered one", "buffered two", "debounced three"} {
		file := filepath.Join(root, content+".txt")
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		event := watcher.FileEvent{Path: file, Type: "cash"}
		if i < 2 {
			events <- event
		} else {
			pending = append(pending, event)
		}
		expected[file] = content
	}
	link := transport.NewLink()
	collector := NewCollector(link, NewEnforcer(filepath.Join(root, "lockfile.json")), func(paused bool) ([]watcher.FileEvent, error) {
		if !paused {
			t.Error("shutdown resumed collection")
		}
		return pending, nil
	})
	stop, finished := make(chan struct{}), make(chan error, 1)
	collector.Stop = stop
	go func() { finished <- collector.Run(events, path, nil) }()
	if up := collectionUpload(t, link); up.Status == nil {
		t.Fatal("missing startup status")
	}
	close(stop)
	for range 3 {
		up := collectionUpload(t, link)
		content, ok := expected[up.FileID]
		if !ok || string(up.Bytes) != content || up.ScreenName != name {
			t.Fatalf("drained upload=%+v", up)
		}
		delete(expected, up.FileID)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("collector did not join command handling")
	}
}
