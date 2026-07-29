package main

import (
	"os"
	"sync"
)

// logMaxBytes caps tracker.log before it rolls over to its .old sibling. An
// internal implementation size (like the 32<<20 memory limit in runServe),
// not a user-facing threshold — nothing configurable depends on it.
const logMaxBytes = 5 << 20

// rotatingWriter is an io.Writer backed by a single log file that rolls over
// to one ".old" sibling once it passes maxBytes: the current file is renamed
// to path+".old" (replacing any previous .old) and a fresh empty file is
// opened. At most two files ever exist, bounding disk use at ~2×maxBytes while
// keeping the current log plus the immediately-preceding one — including
// across a restart, since the size is seeded from the existing file on open.
// It is the sink for --serve's slog handler: a plain file handle, never the
// FreeConsole-invalidated stderr, so a log write can't hang startup.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	f        *os.File
	size     int64
}

// newRotatingWriter opens path for appending, seeding size from the existing
// file so a rollover that would straddle a restart still triggers at the
// right point rather than resetting to zero.
func newRotatingWriter(path string, maxBytes int64) (*rotatingWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &rotatingWriter{path: path, maxBytes: maxBytes, f: f, size: info.Size()}, nil
}

// Write appends p, rotating first if it would push a non-empty file past
// maxBytes. The size>0 guard means a single write larger than maxBytes lands
// in a fresh file and overflows it (the next write then rotates), rather than
// rotating an empty file on every oversized line.
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate closes the current file, renames it to path+".old" (os.Rename
// replaces any existing .old on both Linux and Windows), and reopens path.
// It's best-effort: if the rename fails — e.g. a transient AV/indexer lock on
// Windows over the months this service runs — logging must not die, so path is
// reopened for append either way and size is re-seeded from whatever file is
// there (an empty fresh file after a successful rename → size 0; the still-full
// original after a failed one → the next write simply retries the rotate). The
// original file is never truncated, so a failed rename can't lose log content.
// Close must precede the rename because Windows can't rename an open file.
// Caller holds w.mu.
func (w *rotatingWriter) rotate() {
	w.f.Close()
	os.Rename(w.path, w.path+".old")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	w.f = f
	if info, err := f.Stat(); err == nil {
		w.size = info.Size()
	} else {
		w.size = 0
	}
}

// Close releases the underlying file handle.
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
