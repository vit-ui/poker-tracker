package client

import (
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

// Collector is the collector-side loop: it turns file events into uploads of a
// file's newly-appended bytes (sent up the link) and applies the lock commands
// that come back down. It never parses or stores anything — that's the server's
// job.
type Collector struct {
	link         *transport.Link
	lockfilePath string
}

// NewCollector creates a Collector over the given link, writing enforced locks
// to lockfilePath.
func NewCollector(link *transport.Link, lockfilePath string) *Collector {
	return &Collector{link: link, lockfilePath: lockfilePath}
}

// Run consumes file events from sessionChan, sends an Upload for each, and (in a
// background goroutine) applies incoming lock commands. configPath is read per
// event for the screen name the server needs to score results. Blocks until
// sessionChan is closed.
func (c *Collector) Run(sessionChan <-chan watcher.FileEvent, configPath string) {
	go c.applyCommands()
	sent := make(map[string]int64)
	for event := range sessionChan {
		if up, ok := readUpload(event, sent, configPath); ok {
			c.link.Uploads <- up
		}
	}
}

// applyCommands enforces lock commands from the server for the life of the link.
func (c *Collector) applyCommands() {
	for cmd := range c.link.Commands {
		if cmd.Kind == transport.CommandLock && cmd.Lock != nil {
			Enforce(c.lockfilePath, *cmd.Lock)
		}
	}
}

// readUpload builds the Upload for a file event. A cash hand-history file is
// append-only, so only the bytes past what was last sent are read (sent tracks
// the last offset per file); nothing new means no upload. A tournament summary
// is rewritten wholesale on each bust, so it is always sent whole (offset 0).
// The screen name is read fresh from config each event so a value filled in at
// runtime isn't missed.
func readUpload(event watcher.FileEvent, sent map[string]int64, configPath string) (transport.Upload, bool) {
	screenName := ""
	if cfg, err := config.LoadConfig(configPath); err == nil && cfg.Setup.PokerStarsScreenName != nil {
		screenName = *cfg.Setup.PokerStarsScreenName
	}

	f, err := os.Open(event.Path)
	if err != nil {
		slog.Error("opening file for upload", "path", event.Path, "err", err)
		return transport.Upload{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		slog.Error("stat file for upload", "path", event.Path, "err", err)
		return transport.Upload{}, false
	}
	size := info.Size()

	var offset int64
	if event.Type == "cash" {
		last := sent[event.Path]
		if size >= last {
			offset = last // append case: send only the new bytes
		}
		// size < last means the file was rewritten/truncated: offset stays 0,
		// resending the whole file so the server rebuilds it from scratch.
		if size == offset {
			return transport.Upload{}, false // nothing new to send
		}
	}
	// Tournament summaries (and rewritten cash files) send whole from offset 0.

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		slog.Error("seeking file for upload", "path", event.Path, "err", err)
		return transport.Upload{}, false
	}
	buf := make([]byte, size-offset)
	if _, err := io.ReadFull(f, buf); err != nil {
		slog.Error("reading file for upload", "path", event.Path, "err", err)
		return transport.Upload{}, false
	}
	sent[event.Path] = size

	return transport.Upload{
		FileID:      event.Path,
		Type:        event.Type,
		Offset:      offset,
		Bytes:       buf,
		ScreenName:  screenName,
		ClientClock: time.Now(),
	}, true
}
