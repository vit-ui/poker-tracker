//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRelativeCollectionPathUsesCollectorHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "AppData", "PokerStars")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveCollectorPath(filepath.Join("AppData", "PokerStars"))
	if err != nil || got != dir {
		t.Fatalf("got %q %v, want %q", got, err, dir)
	}
	t.Setenv("HOME", "")
	if _, err := ResolveCollectorPath("AppData"); err == nil {
		t.Fatal("relative path accepted without home")
	}
	if got, err := ResolveCollectorPath(dir); err != nil || got != dir {
		t.Fatalf("full path required home: %q %v", got, err)
	}
}
