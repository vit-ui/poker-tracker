package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitForEvent(t *testing.T, ch <-chan FileEvent, timeout time.Duration) (FileEvent, bool) {
	t.Helper()
	select {
	case ev := <-ch:
		return ev, true
	case <-time.After(timeout):
		return FileEvent{}, false
	}
}

func TestWatch_DetectsFileTypeByDirectory(t *testing.T) {
	handDir := t.TempDir()
	tourneyDir := t.TempDir()

	done := make(chan struct{})
	sessionChan := make(chan FileEvent, 10)
	watchErr := make(chan error, 1)

	go func() {
		watchErr <- Watch(done, handDir, tourneyDir, sessionChan)
	}()

	time.Sleep(100 * time.Millisecond)

	handFile := filepath.Join(handDir, "session1.txt")
	if err := os.WriteFile(handFile, []byte("hand history contents"), 0644); err != nil {
		t.Fatalf("writing hand history file: %v", err)
	}

	ev, ok := waitForEvent(t, sessionChan, 4*time.Second)
	if !ok {
		t.Fatal("expected a FileEvent for the hand history .txt file, got none")
	}
	if ev.Type != "cash" {
		t.Errorf("Type = %q, want cash", ev.Type)
	}
	wantPath, _ := filepath.Abs(handFile)
	if ev.Path != wantPath {
		t.Errorf("Path = %q, want %q", ev.Path, wantPath)
	}

	tourneyFile := filepath.Join(tourneyDir, "summary1.txt")
	if err := os.WriteFile(tourneyFile, []byte("tournament summary contents"), 0644); err != nil {
		t.Fatalf("writing tournament summary file: %v", err)
	}

	ev, ok = waitForEvent(t, sessionChan, 4*time.Second)
	if !ok {
		t.Fatal("expected a FileEvent for the tournament summary .txt file, got none")
	}
	if ev.Type != "tournament" {
		t.Errorf("Type = %q, want tournament", ev.Type)
	}

	otherFile := filepath.Join(handDir, "notes.json")
	if err := os.WriteFile(otherFile, []byte("{}"), 0644); err != nil {
		t.Fatalf("writing non-.txt file: %v", err)
	}
	if _, ok := waitForEvent(t, sessionChan, 3*time.Second); ok {
		t.Error("expected no FileEvent for a non-.txt file")
	}

	close(done)
	select {
	case err := <-watchErr:
		if err != nil {
			t.Errorf("Watch returned error after done closed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Watch did not return after done was closed")
	}
}

func TestWatch_DebouncesRapidWrites(t *testing.T) {
	handDir := t.TempDir()
	tourneyDir := t.TempDir()

	done := make(chan struct{})
	defer close(done)
	sessionChan := make(chan FileEvent, 10)

	go func() {
		_ = Watch(done, handDir, tourneyDir, sessionChan)
	}()
	time.Sleep(100 * time.Millisecond)

	handFile := filepath.Join(handDir, "session2.txt")
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(handFile, []byte("update"), 0644); err != nil {
			t.Fatalf("writing hand history file: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if _, ok := waitForEvent(t, sessionChan, 4*time.Second); !ok {
		t.Fatal("expected one debounced FileEvent, got none")
	}
	if _, ok := waitForEvent(t, sessionChan, 500*time.Millisecond); ok {
		t.Error("expected only one event for rapid writes within the debounce window, got a second")
	}
}

func TestWatch_ConstructionError(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	sessionChan := make(chan FileEvent, 1)

	err := Watch(done, filepath.Join(t.TempDir(), "does-not-exist"), t.TempDir(), sessionChan)
	if err == nil {
		t.Fatal("expected error watching a nonexistent directory")
	}
}
