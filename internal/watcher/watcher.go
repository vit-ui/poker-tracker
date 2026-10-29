// Package watcher monitors PokerStars hand history and tournament summary
// directories for new or modified session files.
package watcher

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// FileEvent represents a new or modified session file detected by the watcher.
type FileEvent struct {
	Path string
	Type string // "cash" (hand history folder) or "tournament" (tournament summary folder)
}

type pendingFile struct {
	event      FileEvent
	timer      *time.Timer
	generation uint64
	ready      bool
}

type expiredFile struct {
	path       string
	generation uint64
}

// Watch monitors Write/Create events on .txt files, debouncing each file for
// two seconds. Closing done stops the watcher and its pending timer callbacks.
func Watch(done <-chan struct{}, handHistoryPath, tournamentPath string, sessionChan chan<- FileEvent) error {
	return WatchControlled(done, handHistoryPath, tournamentPath, sessionChan, nil)
}

// WatchControlled adds a collector-owned pause/drain transition to Watch.
// Every map mutation and event delivery happens on this watcher goroutine.
func WatchControlled(done <-chan struct{}, handHistoryPath, tournamentPath string, sessionChan chan<- FileEvent, control *Control) error {
	finished := make(chan struct{})
	pending := make(map[string]*pendingFile)
	defer func() {
		close(finished)
		for _, entry := range pending {
			if entry.timer != nil {
				entry.timer.Stop()
			}
		}
		if control != nil {
			close(control.done)
		}
	}()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer w.Close()
	if err := w.Add(handHistoryPath); err != nil {
		return fmt.Errorf("watching %s: %w", handHistoryPath, err)
	}
	if err := w.Add(tournamentPath); err != nil {
		return fmt.Errorf("watching %s: %w", tournamentPath, err)
	}
	absHand, _ := filepath.Abs(handHistoryPath)
	absTourney, _ := filepath.Abs(tournamentPath)
	expired := make(chan expiredFile)
	var generation uint64
	paused := false
	var requests <-chan pauseRequest
	if control != nil {
		requests = control.requests
	}
	schedule := func(entry *pendingFile) {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		generation++
		entry.generation = generation
		entry.ready = false
		signal := expiredFile{path: entry.event.Path, generation: generation}
		entry.timer = time.AfterFunc(2*time.Second, func() {
			select {
			case expired <- signal:
			case <-finished:
			}
		})
	}
	accept := func(event fsnotify.Event) {
		if event.Op&(fsnotify.Write|fsnotify.Create) == 0 || !strings.EqualFold(filepath.Ext(event.Name), ".txt") {
			return
		}
		absPath, _ := filepath.Abs(event.Name)
		var fileType string
		switch filepath.Dir(absPath) {
		case absHand:
			fileType = "cash"
		case absTourney:
			fileType = "tournament"
		default:
			return
		}
		entry := pending[absPath]
		if entry == nil {
			entry = &pendingFile{event: FileEvent{Path: absPath, Type: fileType}}
			pending[absPath] = entry
		}
		if !paused {
			schedule(entry)
		}
	}
	for {
		var output chan<- FileEvent
		var next FileEvent
		if !paused {
			for _, entry := range pending {
				if entry.ready {
					output = sessionChan
					next = entry.event
					break
				}
			}
		}
		select {
		case <-done:
			return nil
		case output <- next:
			delete(pending, next.Path)
		case signal := <-expired:
			if entry := pending[signal.path]; entry != nil && !paused && entry.generation == signal.generation {
				entry.ready = true
			}
		case request := <-requests:
			var drained []FileEvent
			if request.paused && !paused {
				// Include fsnotify events already accepted into its delivery channel.
				// Unreceived source bytes remain recoverable from the source histories.
			drain:
				for {
					select {
					case event, ok := <-w.Events:
						if !ok {
							return fmt.Errorf("watcher stopped while draining")
						}
						accept(event)
					default:
						break drain
					}
				}
				for path, entry := range pending {
					if entry.timer != nil {
						entry.timer.Stop()
					}
					drained = append(drained, entry.event)
					delete(pending, path)
				}
				sort.Slice(drained, func(i, j int) bool { return drained[i].Path < drained[j].Path })
			}
			wasPaused := paused
			paused = request.paused
			if wasPaused && !paused {
				for _, entry := range pending {
					schedule(entry)
				}
			}
			request.reply <- pauseResult{events: drained}
		case event, ok := <-w.Events:
			if !ok {
				return nil
			}
			accept(event)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			return fmt.Errorf("watcher error: %w", err)
		}
	}
}
