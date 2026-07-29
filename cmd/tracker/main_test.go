package main

import (
	"path/filepath"
	"testing"

	"github.com/vit-ui/self-tracker/internal/config"
)

// TestResolveStorageBackend covers the three startup-guard outcomes from
// CLAUDE.md's v2.0 backend-aware guard: an already-set backend is returned
// as-is, a v1.0 install (storage_backend unset, google_sheet_id already set)
// is silently migrated to "sheets" and the migration is persisted to disk,
// and an install where neither field is set errors instead of guessing.
func TestResolveStorageBackend(t *testing.T) {
	t.Run("already set is returned as-is", func(t *testing.T) {
		backend := "sqlite"
		cfg := &config.Config{Setup: config.Setup{StorageBackend: &backend}}

		got, err := resolveStorageBackend(cfg, filepath.Join(t.TempDir(), "config.json"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "sqlite" {
			t.Errorf("got %q, want %q", got, "sqlite")
		}
	})

	t.Run("v1.0 install migrates to sheets and persists it", func(t *testing.T) {
		sheetID := "abc123"
		cfg := &config.Config{Setup: config.Setup{GoogleSheetID: &sheetID}}
		path := filepath.Join(t.TempDir(), "config.json")

		got, err := resolveStorageBackend(cfg, path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "sheets" {
			t.Errorf("got %q, want %q", got, "sheets")
		}
		if cfg.Setup.StorageBackend == nil || *cfg.Setup.StorageBackend != "sheets" {
			t.Errorf("cfg.Setup.StorageBackend not migrated in memory: %v", cfg.Setup.StorageBackend)
		}

		reloaded, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if reloaded.Setup.StorageBackend == nil || *reloaded.Setup.StorageBackend != "sheets" {
			t.Errorf("migration not persisted to disk: %v", reloaded.Setup.StorageBackend)
		}
	})

	t.Run("neither field set errors", func(t *testing.T) {
		cfg := &config.Config{}

		_, err := resolveStorageBackend(cfg, filepath.Join(t.TempDir(), "config.json"))
		if err == nil {
			t.Fatal("expected an error when setup has never run")
		}
	})
}

// TestUTF16File confirms the schtasks-required encoding: a little-endian
// BOM followed by each rune as a little-endian 16-bit code unit. schtasks
// /Create /XML silently rejects a plain UTF-8 file, so this encoding is
// load-bearing for --setup's Task Scheduler registration even though it's
// never exercised on this (non-Windows) test machine.
func TestUTF16File(t *testing.T) {
	got := utf16File("Ab")
	want := []byte{0xFF, 0xFE, 'A', 0x00, 'b', 0x00}
	if len(got) != len(want) {
		t.Fatalf("utf16File(%q) length = %d, want %d (%v)", "Ab", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("utf16File(%q)[%d] = %#x, want %#x", "Ab", i, got[i], want[i])
		}
	}

	t.Run("empty string is just the BOM", func(t *testing.T) {
		if got := utf16File(""); len(got) != 2 || got[0] != 0xFF || got[1] != 0xFE {
			t.Errorf("utf16File(\"\") = %v, want the 2-byte BOM only", got)
		}
	})

	t.Run("code point above the ASCII range keeps little-endian byte order", func(t *testing.T) {
		// U+00E9 (é) — high byte non-zero, so byte order is observable.
		got := utf16File("é")
		want := []byte{0xFF, 0xFE, 0xE9, 0x00}
		if len(got) != len(want) {
			t.Fatalf("utf16File(\"é\") = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("utf16File(\"é\")[%d] = %#x, want %#x", i, got[i], want[i])
			}
		}
	})
}
