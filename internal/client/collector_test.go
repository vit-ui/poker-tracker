package client

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

// writeConfigWithScreenName writes a config.json carrying only the screen name,
// which is all readUpload reads from it.
func writeConfigWithScreenName(t *testing.T, dir, name string) string {
	t.Helper()
	configPath := filepath.Join(dir, "config.json")
	cfg, err := config.LoadConfig(configPath) // missing file → all-null config
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if name != "" {
		cfg.Setup.PokerStarsScreenName = &name
	}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	return configPath
}

// TestReadUpload_CashAppendOnlyStreaming is the collector-side counterpart of
// the server's streaming test: a growing cash hand-history file must be streamed
// as byte deltas — the first event sends the whole file from offset 0, each
// later event sends only the bytes appended since the last send (at the right
// offset), and an event with no new bytes sends nothing. This byte-delta
// contract is exactly what the server's reconstruct depends on; a change here
// silently corrupts the reconstructed file.
func TestReadUpload_CashAppendOnlyStreaming(t *testing.T) {
	dir := t.TempDir()
	configPath := writeConfigWithScreenName(t, dir, "Hero")
	path := filepath.Join(dir, "session.txt")
	event := watcher.FileEvent{Path: path, Type: "cash"}
	sent := map[string]*sentEntry{}
	now := time.Now()

	// First write, first event: whole file from offset 0.
	if err := os.WriteFile(path, []byte("AAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, ok := readUpload(event, sent, now, configPath)
	if !ok || up.Offset != 0 || string(up.Bytes) != "AAAA" {
		t.Fatalf("first event: got ok=%v offset=%d bytes=%q, want whole file from 0", ok, up.Offset, string(up.Bytes))
	}
	if up.ScreenName != "Hero" || up.Type != "cash" || up.FileID != path {
		t.Fatalf("first event metadata wrong: %+v", up)
	}
	if up.ClientClock.IsZero() {
		t.Fatal("first event: ClientClock must be set")
	}

	// Append: a second event must send only the appended bytes at offset 4.
	if err := os.WriteFile(path, []byte("AAAABBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, ok = readUpload(event, sent, now, configPath)
	if !ok || up.Offset != 4 || string(up.Bytes) != "BBB" {
		t.Fatalf("append event: got ok=%v offset=%d bytes=%q, want only \"BBB\" at offset 4", ok, up.Offset, string(up.Bytes))
	}

	// No new bytes: an event with the file unchanged sends nothing.
	if _, ok = readUpload(event, sent, now, configPath); ok {
		t.Fatal("unchanged file: expected no upload, got one")
	}
}

// TestReadUpload_CashRewriteResendsWhole confirms the self-heal side: if the
// file shrinks (rewritten/truncated, size < last sent), the collector resends
// the whole file from offset 0 so the server rebuilds it from scratch rather
// than appending onto stale bytes.
func TestReadUpload_CashRewriteResendsWhole(t *testing.T) {
	dir := t.TempDir()
	configPath := writeConfigWithScreenName(t, dir, "Hero")
	path := filepath.Join(dir, "session.txt")
	event := watcher.FileEvent{Path: path, Type: "cash"}
	sent := map[string]*sentEntry{}
	now := time.Now()

	if err := os.WriteFile(path, []byte("AAAABBBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readUpload(event, sent, now, configPath); !ok {
		t.Fatal("initial send failed")
	}

	// Rewrite to something shorter.
	if err := os.WriteFile(path, []byte("CC"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, ok := readUpload(event, sent, now, configPath)
	if !ok || up.Offset != 0 || string(up.Bytes) != "CC" {
		t.Fatalf("rewrite: got ok=%v offset=%d bytes=%q, want whole file \"CC\" from 0", ok, up.Offset, string(up.Bytes))
	}
}

// TestReadUpload_TournamentAlwaysWhole confirms a tournament summary is always
// sent whole from offset 0, even when a prior send recorded an offset — the
// summary is rewritten wholesale on each bust, never appended, so a byte-delta
// would corrupt it.
func TestReadUpload_TournamentAlwaysWhole(t *testing.T) {
	dir := t.TempDir()
	configPath := writeConfigWithScreenName(t, dir, "Hero")
	path := filepath.Join(dir, "T123.txt")
	event := watcher.FileEvent{Path: path, Type: "tournament"}
	sent := map[string]*sentEntry{}
	now := time.Now()

	if err := os.WriteFile(path, []byte("first version"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readUpload(event, sent, now, configPath); !ok {
		t.Fatal("initial tournament send failed")
	}

	// A rewritten (and here longer) summary must still be sent whole from 0.
	if err := os.WriteFile(path, []byte("second longer version"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, ok := readUpload(event, sent, now, configPath)
	if !ok || up.Offset != 0 || string(up.Bytes) != "second longer version" {
		t.Fatalf("tournament rewrite: got ok=%v offset=%d bytes=%q, want whole file from 0", ok, up.Offset, string(up.Bytes))
	}
}

// TestReadUpload_MissingScreenNameSendsEmpty confirms the screen name is read
// per event and left empty when unconfigured (the server then holds the file
// until it's set); the upload itself is still produced so no bytes are lost.
func TestReadUpload_MissingScreenNameSendsEmpty(t *testing.T) {
	dir := t.TempDir()
	configPath := writeConfigWithScreenName(t, dir, "") // no screen name
	path := filepath.Join(dir, "session.txt")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, ok := readUpload(watcher.FileEvent{Path: path, Type: "cash"}, map[string]*sentEntry{}, time.Now(), configPath)
	if !ok {
		t.Fatal("expected an upload even with no screen name")
	}
	if up.ScreenName != "" {
		t.Fatalf("ScreenName = %q, want empty when unconfigured", up.ScreenName)
	}
}

// TestReadUpload_MissingFileNoUpload confirms a vanished file (opened between
// the watcher event and the read) yields no upload rather than a panic.
func TestReadUpload_MissingFileNoUpload(t *testing.T) {
	dir := t.TempDir()
	configPath := writeConfigWithScreenName(t, dir, "Hero")
	if _, ok := readUpload(watcher.FileEvent{Path: filepath.Join(dir, "gone.txt"), Type: "cash"}, map[string]*sentEntry{}, time.Now(), configPath); ok {
		t.Fatal("expected no upload for a missing file")
	}
}
