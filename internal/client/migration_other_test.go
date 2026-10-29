//go:build !windows

package client

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

func TestUnsupportedCollectorCannotConfirmPokerStarsClosure(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigWithScreenName(t, dir, "AnonHero01")
	link := transport.NewLink()
	collector := NewCollector(link, NewEnforcer(filepath.Join(dir, "lockfile.json")), func(bool) ([]watcher.FileEvent, error) {
		t.Error("unsupported process check paused collection")
		return nil, nil
	})
	events := make(chan watcher.FileEvent)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() { collector.Run(events, path, done); close(stopped) }()
	defer func() { close(done); <-stopped }()
	link.Commands <- transport.Command{Kind: transport.CommandPauseCollection, RequestID: "check-closed"}
	select {
	case upload := <-link.Uploads:
		if upload.CollectionReply == nil || upload.CollectionReply.RequestID != "check-closed" || !strings.Contains(upload.CollectionReply.Error, "Windows collector") {
			t.Fatalf("unexpected closure response: %+v", upload.CollectionReply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("collector did not refuse unsupported process check")
	}
}
