// Package transport defines collector/server messages and their in-process
// channel link. The same messages cross the authenticated, pinned TLS link
// when collection and server processing run in separate installations.
package transport

import (
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

// ImportStart and ImportDone are sentinel Upload.Type values that bracket a
// /scan backfill (see Collector.rescan): the collector sends ImportStart, then
// one Import upload per file in the watched folders, then ImportDone. They carry
// no bytes; the server uses them to reset and report the per-scan counters.
const (
	ImportStart = "import_start"
	ImportDone  = "import_done"
)

// Upload is sent from the client to the server when a watched file changes. It
// carries the bytes appended to that file since the previous Upload (the whole
// file when Offset is 0), plus what the server needs to reconstruct the file,
// parse it, and time-stamp the results against the client's clock.
// CollectionReply acknowledges a collector-side pause/resume request. Errors
// describe observed failure; an absent reply never establishes completion.
type CollectionReply struct {
	RequestID string
	Error     string
}

type LockReply struct {
	TriggerID string
	Lock      *config.LockFile
	Error     string
}

type Upload struct {
	Recovery        bool
	FileActivity    time.Time
	LockReply       *LockReply
	CollectionReply *CollectionReply
	FileID          string    // stable identity of the source file (its path on the client)
	Type            string    // "cash" or "tournament" (or ImportStart/ImportDone marker), matching watcher.FileEvent.Type
	Offset          int64     // byte offset in the file where Bytes begins
	Bytes           []byte    // the appended bytes (the whole file when Offset == 0)
	ScreenName      string    // the PokerStars screen name whose results to score
	ClientClock     time.Time // the client's wall-clock time when the change was seen

	// Import marks an upload produced by a /scan backfill (Collector.rescan)
	// rather than a live file change. The server records these silently — no
	// hard stop, no alerts, no summary, no quiet-period deferral — and skips
	// tournaments that need a player answer. See Server.importUpload.
	Import bool
}

// Commands ask the collector to enforce a lock, scan histories, or pause/resume
// collection. Pause/resume carry RequestID and receive an ordered CollectionReply
// upload. A pause checks PokerStars closure before draining its watcher.
const (
	CommandLock             = "lock"
	CommandRescan           = "rescan"
	CommandResyncFiles      = "resync_files"
	CommandPauseCollection  = "pause_collection"
	CommandResumeCollection = "resume_collection"
)

// Command is sent from the server to the client.
type Command struct {
	Files     []string
	RequestID string
	Kind      string           // one of the Command constants
	Lock      *config.LockFile // set when Kind == CommandLock
}

// Link is the in-process connection between one client and one server: uploads
// flow up (client → server), commands flow down (server → client). Split builds
// bridge each end to TLS; all-local shares this channel pair directly.
type Link struct {
	ConnectionChanged chan struct{}
	connectionMu      sync.Mutex
	collectorDone     <-chan struct{}
	localDone         chan struct{}
	Uploads           chan Upload
	Commands          chan Command
}

// NewLink creates a Link with small buffers, matching the sessionChan/alertChan
// buffering runServe already uses so a brief stall on one side doesn't block the
// other.
func NewLink() *Link {
	localDone := make(chan struct{})
	return &Link{
		ConnectionChanged: make(chan struct{}, 1),
		collectorDone:     localDone,
		localDone:         localDone,
		Uploads:           make(chan Upload, 10),
		Commands:          make(chan Command, 10),
	}
}

// CollectorDone closes when the currently selected collector disconnects. An
// in-process collector remains available until its owning process shuts down.
func (l *Link) CollectorDone() <-chan struct{} {
	l.connectionMu.Lock()
	defer l.connectionMu.Unlock()
	return l.collectorDone
}

func (l *Link) useNetwork() {
	l.connectionMu.Lock()
	defer l.connectionMu.Unlock()
	close(l.localDone)
	l.collectorDone = l.localDone
	select {
	case l.ConnectionChanged <- struct{}{}:
	default:
	}
}

func (l *Link) setCollectorDone(done <-chan struct{}) {
	l.connectionMu.Lock()
	defer l.connectionMu.Unlock()
	l.collectorDone = done
	select {
	case l.ConnectionChanged <- struct{}{}:
	default:
	}
}
