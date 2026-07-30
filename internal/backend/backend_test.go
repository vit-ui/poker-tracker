package backend

import (
	"path/filepath"
	"testing"

	"github.com/vit-ui/self-tracker/internal/config"
)

// TestResolveStorageBackend covers the three startup-guard outcomes: an
// already-set backend is returned as-is, a v1.0 install (storage_backend unset,
// google_sheet_id already set) is silently migrated to "sheets" and persisted,
// and an install where neither field is set errors instead of guessing.
func TestResolveStorageBackend(t *testing.T) {
	t.Run("already set is returned as-is", func(t *testing.T) {
		name := "sqlite"
		cfg := &config.Config{Setup: config.Setup{StorageBackend: &name}}

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
