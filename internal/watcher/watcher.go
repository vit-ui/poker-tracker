// Package watcher monitors PokerStars hand history and tournament summary
// directories for new or modified session files.
package watcher

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// FileEvent represents a new or modified session file detected by the watcher.
type FileEvent struct {
	Path string
	Type string // "cash" or "spingo"
}

// Watch monitors handHistoryPath and tournamentPath for Write and Create events
// on .txt files. Each file is debounced by 2 seconds after the last event on
// that file before sending to sessionChan, preventing reads mid-write. Blocks
// until done is closed or a fatal watcher error occurs.
func Watch(done <-chan struct{}, handHistoryPath, tournamentPath string, sessionChan chan<- FileEvent) error {
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

	var mu sync.Mutex
	timers := make(map[string]*time.Timer)

	schedule := func(path, fileType string) {
		mu.Lock()
		defer mu.Unlock()
		if t, ok := timers[path]; ok {
			t.Stop()
		}
		timers[path] = time.AfterFunc(2*time.Second, func() {
			mu.Lock()
			delete(timers, path)
			mu.Unlock()
			sessionChan <- FileEvent{Path: path, Type: fileType}
		})
	}

	for {
		select {
		case <-done:
			return nil
		case event, ok := <-w.Events:
			if !ok {
				return nil
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			if strings.ToLower(filepath.Ext(event.Name)) != ".txt" {
				continue
			}
			absPath, _ := filepath.Abs(event.Name)
			dir := filepath.Dir(absPath)
			var fileType string
			switch dir {
			case absHand:
				fileType = "cash"
			case absTourney:
				fileType = "spingo"
			default:
				continue
			}
			schedule(absPath, fileType)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			return fmt.Errorf("watcher error: %w", err)
		}
	}
}
