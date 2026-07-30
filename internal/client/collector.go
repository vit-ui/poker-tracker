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

// sentEntry is a file's last-sent byte offset plus when it was last touched, so
// idle entries can be evicted (see sentStateCap/TTL). Mirrors the server's
// parseEntry bookkeeping so both sides of the link bound their per-file maps the
// same way.
type sentEntry struct {
	offset   int64
	lastSeen time.Time
}

// sentStateCap and sentStateTTL bound the per-file sent-offset map. A long-lived
// collector watching a folder that accumulates a new hand-history file per table
// per day would otherwise grow the map without limit; when it exceeds the cap,
// entries not touched within the TTL (finished sessions) are dropped. An
// actively-growing file keeps a recent lastSeen and is never evicted; a
// wrongly-evicted file just re-sends whole once (the server rebuilds it from
// offset 0, always correct), so the bound can be aggressive. Same shape as the
// server's parseStateCap/TTL.
const (
	sentStateCap = 128
	sentStateTTL = 12 * time.Hour
)

// Run consumes file events from sessionChan, sends an Upload for each, and (in a
// background goroutine) applies incoming lock commands. configPath is read per
// event for the screen name the server needs to score results. dead is closed
// by the network transport when the connection to the server drops; Run returns
// then so the caller can exit and let its service manager restart it. In the
// all-local build the link never drops, so a nil dead is passed and Run blocks
// until sessionChan is closed.
func (c *Collector) Run(sessionChan <-chan watcher.FileEvent, configPath string, dead <-chan struct{}) {
	go c.applyCommands(dead)
	sent := make(map[string]*sentEntry)
	for {
		select {
		case <-dead:
			return
		case event, ok := <-sessionChan:
			if !ok {
				return
			}
			now := time.Now()
			if len(sent) > sentStateCap {
				for p, e := range sent {
					if now.Sub(e.lastSeen) > sentStateTTL {
						delete(sent, p)
					}
				}
			}
			if up, ok := readUpload(event, sent, now, configPath); ok {
				select {
				case c.link.Uploads <- up:
				case <-dead:
					return // link died mid-send; don't block
				}
			}
		}
	}
}

// applyCommands enforces lock commands from the server until the link dies.
func (c *Collector) applyCommands(dead <-chan struct{}) {
	for {
		select {
		case <-dead:
			return
		case cmd, ok := <-c.link.Commands:
			if !ok {
				return
			}
			if cmd.Kind == transport.CommandLock && cmd.Lock != nil {
				Enforce(c.lockfilePath, *cmd.Lock)
			}
		}
	}
}

// readUpload builds the Upload for a file event. A cash hand-history file is
// append-only, so only the bytes past what was last sent are read (sent tracks
// the last offset per file); nothing new means no upload. A tournament summary
// is rewritten wholesale on each bust, so it is always sent whole (offset 0).
// The screen name is read fresh from config each event so a value filled in at
// runtime isn't missed.
func readUpload(event watcher.FileEvent, sent map[string]*sentEntry, now time.Time, configPath string) (transport.Upload, bool) {
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
		var last int64
		if e := sent[event.Path]; e != nil {
			last = e.offset
		}
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
	sent[event.Path] = &sentEntry{offset: size, lastSeen: now}

	return transport.Upload{
		FileID:      event.Path,
		Type:        event.Type,
		Offset:      offset,
		Bytes:       buf,
		ScreenName:  screenName,
		ClientClock: time.Now(),
	}, true
}
