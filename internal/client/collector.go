package client

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	// Stop requests a watcher drain while transport and enforcement remain available.
	Stop       <-chan struct{}
	link       *transport.Link
	enforcer   *Enforcer
	control    func(bool) ([]watcher.FileEvent, error)
	collection *collection
}

// NewCollector creates a Collector over the given link, writing enforced locks
// to lockfilePath. control pauses/resumes its watcher and returns drained events.
func NewCollector(link *transport.Link, enforcer *Enforcer, control func(bool) ([]watcher.FileEvent, error)) *Collector {
	return &Collector{link: link, enforcer: enforcer, control: control}
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
// then so the connection owner can reconnect while enforcement continues. In the
// all-local build the link never drops, so a nil dead is passed and Run blocks
// until sessionChan is closed.
func (c *Collector) Run(sessionChan <-chan watcher.FileEvent, configPath string, dead <-chan struct{}) error {
	finished := make(chan struct{})
	requests := make(chan transport.Command)
	commandsDone := make(chan struct{})
	go func() { defer close(commandsDone); c.applyCommands(dead, finished, requests) }()
	defer func() { close(finished); <-commandsDone }()
	lock, err := config.LoadLock(c.enforcer.path)
	status := &transport.CollectorStatus{Lock: lock}
	if err != nil {
		status.Error = err.Error()
	}
	if cfg, err := config.LoadConfig(configPath); err == nil {
		settings := config.CollectionSettings(cfg)
		status.Settings = &settings
		status.Watching = c.collection != nil && c.collection.active != nil
		if c.collection != nil && c.collection.err != nil {
			status.CollectionError = c.collection.err.Error()
		}
	}
	if !c.send(transport.Upload{Status: status, ClientClock: time.Now()}, dead) {
		return nil
	}
	sent := make(map[string]*sentEntry)
	paused := false
	upload := func(event watcher.FileEvent, recovery bool) error {
		now := time.Now()
		if len(sent) > sentStateCap {
			for path, entry := range sent {
				if now.Sub(entry.lastSeen) > sentStateTTL {
					delete(sent, path)
				}
			}
		}
		up, ok, err := readUpload(event, sent, now, configPath)
		if err != nil {
			return err
		}
		up.Recovery = recovery
		if ok && !c.send(up, dead) {
			return fmt.Errorf("collector disconnected while draining files")
		}
		return nil
	}
	for {
		var stopped <-chan struct{}
		if c.collection != nil {
			stopped = c.collection.stopped()
		}
		var events <-chan watcher.FileEvent
		if !paused {
			events = sessionChan
		}
		select {
		case <-c.Stop:
			var pending []watcher.FileEvent
			var err error
			if c.collection != nil && c.collection.active != nil {
				pending, err = c.collection.active.stop()
				c.collection.active = nil
			} else if c.control != nil {
				pending, err = c.control(true)
			}
		shutdownDrain:
			for {
				select {
				case event, ok := <-sessionChan:
					if !ok {
						break shutdownDrain
					}
					pending = append(pending, event)
				default:
					break shutdownDrain
				}
			}
			for _, event := range pending {
				err = errors.Join(err, upload(event, false))
			}
			return err
		case <-stopped:
			return fmt.Errorf("file watcher stopped: %w", c.collection.active.err)
		case <-dead:
			return nil
		case cmd := <-requests:
			var err error
			switch cmd.Kind {
			case transport.CommandCollectorConfig:
				var settings config.CollectorSettings
				if cmd.Settings != nil {
					settings, err = c.changeSettings(configPath, *cmd.Settings, upload)
				} else {
					var cfg *config.Config
					cfg, err = config.LoadConfig(configPath)
					if err == nil {
						settings = config.CollectionSettings(cfg)
					}
				}
				reply := &transport.CollectorConfigReply{RequestID: cmd.RequestID, Settings: settings, Watching: c.collection != nil && c.collection.active != nil}
				if c.collection != nil && c.collection.err != nil {
					reply.CollectionError = c.collection.err.Error()
				}
				if err != nil {
					reply.Error = err.Error()
				}
				if !c.send(transport.Upload{ConfigReply: reply, ClientClock: time.Now()}, dead) {
					return nil
				}
				continue
			case transport.CommandResyncFiles:
				cfg, err := config.LoadConfig(configPath)
				if err != nil || cfg.Paths.HandHistory == nil || cfg.Setup.PokerStarsScreenName == nil {
					slog.Error("cash resync has no configured collection folder", "err", err)
					continue
				}
				directory := filepath.Join(*cfg.Paths.HandHistory, *cfg.Setup.PokerStarsScreenName)
				expected, err := os.Stat(directory)
				if err != nil {
					slog.Error("reading cash resync folder", "err", err)
					continue
				}
				for _, path := range cmd.Files {
					parent, err := os.Stat(filepath.Dir(path))
					if err != nil || !os.SameFile(expected, parent) || !strings.EqualFold(filepath.Ext(path), ".txt") {
						slog.Error("refusing cash resync outside the configured hand-history folder", "file", path)
						continue
					}
					delete(sent, path)
					if err := upload(watcher.FileEvent{Path: path, Type: "cash"}, true); err != nil {
						slog.Error("resynchronizing pending cash file", "err", err)
					}
				}
				continue

			case transport.CommandRescan:
				if !paused {
					c.rescan(configPath, dead)
				}
				continue
			case transport.CommandPauseCollection, transport.CommandDrainCollection:
				if paused {
					err = fmt.Errorf("collection is already paused")
					break
				}
				if cmd.Kind == transport.CommandPauseCollection {
					running, checkErr := pokerStarsRunning()
					if checkErr != nil {
						err = checkErr
						break
					}
					if running {
						err = fmt.Errorf("close PokerStars before changing storage")
						break
					}
				}
				if c.control == nil {
					err = fmt.Errorf("collection pause is unavailable")
					break
				}
				var pending []watcher.FileEvent
				pending, err = c.control(true)
				if err != nil {
					break
				}
				paused = true
			drain:
				for {
					select {
					case event, ok := <-sessionChan:
						if !ok {
							err = errors.Join(err, fmt.Errorf("file collection stopped"))
							break drain
						}
						err = errors.Join(err, upload(event, false))
					default:
						break drain
					}
				}
				for _, event := range pending {
					err = errors.Join(err, upload(event, false))
				}
				if err != nil {
					_, resumeErr := c.control(false)
					err = errors.Join(err, resumeErr)
					paused = resumeErr != nil
				}
			case transport.CommandResumeCollection:
				if c.control == nil {
					err = fmt.Errorf("collection resume is unavailable")
					break
				}
				_, err = c.control(false)
				if err == nil {
					paused = false
				}
			}
			reply := &transport.CollectionReply{RequestID: cmd.RequestID}
			if err != nil {
				reply.Error = err.Error()
			}
			if !c.send(transport.Upload{CollectionReply: reply, ClientClock: time.Now()}, dead) {
				return nil
			}
		case event, ok := <-events:
			if !ok {
				return nil
			}
			if err := upload(event, false); err != nil {
				slog.Error("reading file for upload", "err", err)
			}
		}
	}

}

// applyCommands carries out commands from the server until the link dies: a
// lock kills PokerStars and writes the lockfile. File/control requests are
// forwarded to Run so uploads and pause acknowledgements keep their order.
func (c *Collector) applyCommands(dead, finished <-chan struct{}, requests chan<- transport.Command) {
	var pendingRequests []transport.Command
	var replies []transport.Upload
	for {
		var outgoingRequests chan<- transport.Command
		var request transport.Command
		if len(pendingRequests) > 0 {
			outgoingRequests = requests
			request = pendingRequests[0]
		}
		var outgoingReplies chan<- transport.Upload
		var reply transport.Upload
		if len(replies) > 0 {
			outgoingReplies = c.link.Uploads
			reply = replies[0]
		}
		select {
		case <-dead:
			return
		case <-finished:
			return
		case outgoingRequests <- request:
			pendingRequests[0] = transport.Command{}
			pendingRequests = pendingRequests[1:]
		case outgoingReplies <- reply:
			replies[0] = transport.Upload{}
			replies = replies[1:]
		case command, ok := <-c.link.Commands:
			if !ok {
				return
			}
			if command.Kind != transport.CommandLock {
				pendingRequests = append(pendingRequests, command)
				continue
			}
			if command.Lock == nil {
				continue
			}
			active, _, err := c.enforcer.Enforce(*command.Lock)
			if command.Lock.TriggerID != "" {
				result := &transport.LockReply{TriggerID: command.Lock.TriggerID, Lock: active}
				if err != nil {
					result.Error = err.Error()
				}
				replies = append(replies, transport.Upload{LockReply: result, ClientClock: time.Now()})
			}
		}
	}
}

// rescan walks both watched folders and uploads every file as an Import upload,
// bracketed by ImportStart/ImportDone markers, so the server can silently
// backfill everything not yet recorded (the /scan command). The paths are the
// config's base folders joined with the screen name — the same join the watcher
// is started with. A file that can't be read is logged and skipped. Sends stop
// early if the link dies mid-scan.
func (c *Collector) rescan(configPath string, dead <-chan struct{}) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config for rescan", "err", err)
		return
	}
	if cfg.Paths.HandHistory == nil || cfg.Paths.TournamentSummary == nil || cfg.Setup.PokerStarsScreenName == nil {
		slog.Error("rescan requested but paths or screen name not configured")
		return
	}
	screenName := *cfg.Setup.PokerStarsScreenName
	hhDir := filepath.Join(*cfg.Paths.HandHistory, screenName)
	tsDir := filepath.Join(*cfg.Paths.TournamentSummary, screenName)

	if !c.send(transport.Upload{Type: transport.ImportStart}, dead) {
		return
	}
	c.uploadDir(hhDir, "cash", screenName, dead)
	c.uploadDir(tsDir, "tournament", screenName, dead)
	c.send(transport.Upload{Type: transport.ImportDone}, dead)
}

// uploadDir sends every regular .txt file in dir as a whole-file Import upload
// of the given Type. PokerStars writes .txt files; anything else (or a
// subdirectory) is skipped.
func (c *Collector) uploadDir(dir, fileType, screenName string, dead <-chan struct{}) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Error("reading folder for rescan", "dir", dir, "err", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".txt") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		bytes, err := os.ReadFile(path)
		if err != nil {
			slog.Error("reading file for rescan", "path", path, "err", err)
			continue
		}
		up := transport.Upload{
			FileID:      path,
			Type:        fileType,
			Offset:      0,
			Bytes:       bytes,
			ScreenName:  screenName,
			ClientClock: time.Now(),
			Import:      true,
		}
		if !c.send(up, dead) {
			return
		}
	}
}

// send delivers one upload, returning false if the link died first so the
// caller can stop the scan.
func (c *Collector) send(up transport.Upload, dead <-chan struct{}) bool {
	select {
	case c.link.Uploads <- up:
		return true
	case <-dead:
		return false
	}
}

// readUpload builds the Upload for a file event. A cash hand-history file is
// append-only, so only the bytes past what was last sent are read (sent tracks
// the last offset per file); nothing new means no upload. A tournament summary
// is rewritten wholesale on each bust, so it is always sent whole (offset 0).
// The screen name is read fresh from config each event so a value filled in at
// runtime isn't missed.
func readUpload(event watcher.FileEvent, sent map[string]*sentEntry, now time.Time, configPath string) (transport.Upload, bool, error) {
	screenName := ""
	if cfg, err := config.LoadConfig(configPath); err == nil && cfg.Setup.PokerStarsScreenName != nil {
		screenName = *cfg.Setup.PokerStarsScreenName
	}

	f, err := os.Open(event.Path)
	if err != nil {
		return transport.Upload{}, false, fmt.Errorf("opening file for upload %s: %w", event.Path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return transport.Upload{}, false, fmt.Errorf("stat file for upload %s: %w", event.Path, err)
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
			return transport.Upload{}, false, nil // nothing new to send
		}
	}
	// Tournament summaries (and rewritten cash files) send whole from offset 0.

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return transport.Upload{}, false, fmt.Errorf("seeking file for upload %s: %w", event.Path, err)
	}
	buf := make([]byte, size-offset)
	if _, err := io.ReadFull(f, buf); err != nil {
		return transport.Upload{}, false, fmt.Errorf("reading file for upload %s: %w", event.Path, err)
	}
	sent[event.Path] = &sentEntry{offset: size, lastSeen: now}

	return transport.Upload{
		FileID:       event.Path,
		Type:         event.Type,
		Offset:       offset,
		Bytes:        buf,
		ScreenName:   screenName,
		ClientClock:  time.Now(),
		FileActivity: info.ModTime(),
	}, true, nil
}

// RunEnforcementLoop shares failed-save protection with command handling.
func (c *Collector) RunEnforcementLoop(done <-chan struct{}) { c.enforcer.Run(done) }
