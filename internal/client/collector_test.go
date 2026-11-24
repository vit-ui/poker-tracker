package client

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

func writeConfigWithScreenName(t *testing.T, dir, name string) string {
	t.Helper()
	configPath := filepath.Join(dir, "config.json")
	cfg, err := config.LoadConfig(configPath)
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

func TestReadUpload_CashAppendOnlyStreaming(t *testing.T) {
	dir := t.TempDir()
	configPath := writeConfigWithScreenName(t, dir, "Hero")
	path := filepath.Join(dir, "session.txt")
	event := watcher.FileEvent{Path: path, Type: "cash"}
	sent := map[string]*sentEntry{}
	now := time.Now()

	if err := os.WriteFile(path, []byte("AAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, ok, _ := readUpload(event, sent, now, configPath)
	if !ok || up.Offset != 0 || string(up.Bytes) != "AAAA" {
		t.Fatalf("first event: got ok=%v offset=%d bytes=%q, want whole file from 0", ok, up.Offset, string(up.Bytes))
	}
	if up.ScreenName != "Hero" || up.Type != "cash" || up.FileID != path {
		t.Fatalf("first event metadata wrong: %+v", up)
	}
	if up.ClientClock.IsZero() {
		t.Fatal("first event: ClientClock must be set")
	}

	if err := os.WriteFile(path, []byte("AAAABBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, ok, _ = readUpload(event, sent, now, configPath)
	if !ok || up.Offset != 4 || string(up.Bytes) != "BBB" {
		t.Fatalf("append event: got ok=%v offset=%d bytes=%q, want only \"BBB\" at offset 4", ok, up.Offset, string(up.Bytes))
	}

	if _, ok, _ = readUpload(event, sent, now, configPath); ok {
		t.Fatal("unchanged file: expected no upload, got one")
	}
}

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
	if _, ok, _ := readUpload(event, sent, now, configPath); !ok {
		t.Fatal("initial send failed")
	}

	if err := os.WriteFile(path, []byte("CC"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, ok, _ := readUpload(event, sent, now, configPath)
	if !ok || up.Offset != 0 || string(up.Bytes) != "CC" {
		t.Fatalf("rewrite: got ok=%v offset=%d bytes=%q, want whole file \"CC\" from 0", ok, up.Offset, string(up.Bytes))
	}
}

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
	if _, ok, _ := readUpload(event, sent, now, configPath); !ok {
		t.Fatal("initial tournament send failed")
	}

	if err := os.WriteFile(path, []byte("second longer version"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, ok, _ := readUpload(event, sent, now, configPath)
	if !ok || up.Offset != 0 || string(up.Bytes) != "second longer version" {
		t.Fatalf("tournament rewrite: got ok=%v offset=%d bytes=%q, want whole file from 0", ok, up.Offset, string(up.Bytes))
	}
}

func TestReadUpload_MissingScreenNameSendsEmpty(t *testing.T) {
	dir := t.TempDir()
	configPath := writeConfigWithScreenName(t, dir, "")
	path := filepath.Join(dir, "session.txt")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	up, ok, _ := readUpload(watcher.FileEvent{Path: path, Type: "cash"}, map[string]*sentEntry{}, time.Now(), configPath)
	if !ok {
		t.Fatal("expected an upload even with no screen name")
	}
	if up.ScreenName != "" {
		t.Fatalf("ScreenName = %q, want empty when unconfigured", up.ScreenName)
	}
}

func TestReadUpload_MissingFileNoUpload(t *testing.T) {
	dir := t.TempDir()
	configPath := writeConfigWithScreenName(t, dir, "Hero")
	if _, ok, err := readUpload(watcher.FileEvent{Path: filepath.Join(dir, "gone.txt"), Type: "cash"}, map[string]*sentEntry{}, time.Now(), configPath); ok || err == nil {
		t.Fatalf("missing file: upload=%v error=%v; need a reported failure for migration drain", ok, err)
	}
}

func TestCollectorRescan(t *testing.T) {
	const hero = "AnonHero01"
	dir := t.TempDir()
	hhBase := filepath.Join(dir, "hh")
	tsBase := filepath.Join(dir, "ts")
	hhDir := filepath.Join(hhBase, hero)
	tsDir := filepath.Join(tsBase, hero)
	for _, d := range []string{hhDir, tsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	readFixture := func(sub, name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", sub, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cashBytes := readFixture("hand_history", "nlhe_cash.txt")
	mttBytes := readFixture("tournament_summary", "horse_T9000000003.txt")
	if err := os.WriteFile(filepath.Join(hhDir, "nlhe_cash.txt"), cashBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tsDir, "horse.txt"), mttBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hhDir, "notes.md"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(dir, "config.json")
	cfg := &config.Config{}
	screen := hero
	cfg.Setup.PokerStarsScreenName = &screen
	cfg.Paths.HandHistory = &hhBase
	cfg.Paths.TournamentSummary = &tsBase
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	link := transport.NewLink()
	c := NewCollector(link, NewEnforcer(filepath.Join(dir, "lockfile.json")), nil)
	go c.rescan(configPath, nil)

	var got []transport.Upload
	for {
		up := <-link.Uploads
		got = append(got, up)
		if up.Type == transport.ImportDone {
			break
		}
	}

	if len(got) != 4 {
		t.Fatalf("got %d uploads, want 4: %+v", len(got), got)
	}
	if got[0].Type != transport.ImportStart {
		t.Errorf("first upload Type = %q, want %q", got[0].Type, transport.ImportStart)
	}
	if got[len(got)-1].Type != transport.ImportDone {
		t.Errorf("last upload Type = %q, want %q", got[len(got)-1].Type, transport.ImportDone)
	}

	cash, tourn := got[1], got[2]
	if cash.Type != "cash" || !cash.Import || cash.Offset != 0 || cash.ScreenName != hero || !bytes.Equal(cash.Bytes, cashBytes) {
		t.Errorf("cash upload wrong: type=%q import=%v offset=%d screen=%q bytesMatch=%v",
			cash.Type, cash.Import, cash.Offset, cash.ScreenName, bytes.Equal(cash.Bytes, cashBytes))
	}
	if tourn.Type != "tournament" || !tourn.Import || tourn.Offset != 0 || tourn.ScreenName != hero || !bytes.Equal(tourn.Bytes, mttBytes) {
		t.Errorf("tournament upload wrong: type=%q import=%v offset=%d screen=%q bytesMatch=%v",
			tourn.Type, tourn.Import, tourn.Offset, tourn.ScreenName, bytes.Equal(tourn.Bytes, mttBytes))
	}
}
