// Package transport defines the messages the collector (client) and the server
// exchange, and the in-process wiring that connects them when both roles run in
// one binary (the all-local build). The same message types will carry over a
// network connection in the split build; this package holds only the message
// types and the in-process channels — no networking, and deliberately no
// interface while there is a single (in-process) implementation.
package transport

import (
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
type Upload struct {
	FileID      string    // stable identity of the source file (its path on the client)
	Type        string    // "cash" or "tournament" (or ImportStart/ImportDone marker), matching watcher.FileEvent.Type
	Offset      int64     // byte offset in the file where Bytes begins
	Bytes       []byte    // the appended bytes (the whole file when Offset == 0)
	ScreenName  string    // the PokerStars screen name whose results to score
	ClientClock time.Time // the client's wall-clock time when the change was seen

	// Import marks an upload produced by a /scan backfill (Collector.rescan)
	// rather than a live file change. The server records these silently — no
	// hard stop, no alerts, no summary, no quiet-period deferral — and skips
	// tournaments that need a player answer. See Server.importUpload.
	Import bool
}

// CommandLock and CommandRescan are the Kinds of command the server sends the
// client. A lock kills PokerStars and writes the lockfile; a rescan makes the
// collector walk both watched folders and upload every file for a silent
// backfill (see Collector.rescan). Both decisions are made on the server side;
// the client only carries them out.
const (
	CommandLock   = "lock"
	CommandRescan = "rescan"
)

// Command is sent from the server to the client.
type Command struct {
	Kind string           // CommandLock or CommandRescan
	Lock *config.LockFile // set when Kind == CommandLock
}

// Link is the in-process connection between one client and one server: uploads
// flow up (client → server), commands flow down (server → client). The split
// build will replace this with a network connection carrying the same two
// message types; the all-local build uses this channel pair.
type Link struct {
	Uploads  chan Upload
	Commands chan Command
}

// NewLink creates a Link with small buffers, matching the sessionChan/alertChan
// buffering runServe already uses so a brief stall on one side doesn't block the
// other.
func NewLink() *Link {
	return &Link{
		Uploads:  make(chan Upload, 10),
		Commands: make(chan Command, 10),
	}
}
