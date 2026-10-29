package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPauseDrainsDebounceAndResumeRetainsChanges(t *testing.T) {
	root := t.TempDir()
	hh, ts := filepath.Join(root, "hands"), filepath.Join(root, "summaries")
	for _, dir := range []string{hh, ts} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	control := NewControl()
	done := make(chan struct{})
	stopped := make(chan error, 1)
	events := make(chan FileEvent, 10)
	go func() { stopped <- WatchControlled(done, hh, ts, events, control) }()
	defer func() {
		close(done)
		if err := <-stopped; err != nil {
			t.Error(err)
		}
	}()
	if _, err := control.SetPaused(false); err != nil {
		t.Fatal(err)
	} // watcher has added both folders
	cash, summary := filepath.Join(hh, "cash.txt"), filepath.Join(ts, "summary.txt")
	for _, path := range []string{cash, summary} {
		if err := os.WriteFile(path, []byte("initial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Allow fsnotify delivery, while remaining well inside the debounce period.
	time.Sleep(100 * time.Millisecond)
	drained, err := control.SetPaused(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(drained) != 2 {
		t.Fatalf("drained %v; want both pending files", drained)
	}
	want := map[string]string{cash: "cash", summary: "tournament"}
	for _, event := range drained {
		if want[event.Path] != event.Type {
			t.Fatalf("unexpected drained event %+v", event)
		}
		delete(want, event.Path)
	}
	if len(want) != 0 {
		t.Fatalf("missing files: %v", want)
	}
	if err := os.WriteFile(cash, []byte("changed while paused"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("paused watcher emitted %+v", event)
	case <-time.After(2200 * time.Millisecond):
	}
	if _, err := control.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Path != cash || event.Type != "cash" {
			t.Fatalf("resumed event %+v", event)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("change observed while paused was lost")
	}
	select {
	case event := <-events:
		t.Fatalf("drained event emitted again: %+v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatcherStopsWithUndeliveredEvent(t *testing.T) {
	root := t.TempDir()
	control := NewControl()
	done := make(chan struct{})
	stopped := make(chan error, 1)
	go func() { stopped <- WatchControlled(done, root, root, make(chan FileEvent), control) }()
	if _, err := control.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blocked.txt"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2200 * time.Millisecond)
	close(done)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher could not stop with blocked output")
	}
	if _, err := control.SetPaused(true); err == nil {
		t.Fatal("stopped watcher acknowledged pause")
	}
}
