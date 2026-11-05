// Package backend is the storage composition root shared by the binaries that
// run the server role (the all-local tracker and the split server): it resolves
// which storage backend an install uses and constructs it. It lives outside the
// mains so the construction isn't duplicated between them.
package backend

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// Open resolves the configured storage backend and constructs it, returning the
// store and the backend name ("sheets" or "sqlite"). On the sheets path it
// blocks in bot.EnsureSheetID until a sheet is configured (opening Telegram if
// needed); the sqlite path never touches Google. Panics are the caller's job —
// Open returns errors so the split server can log-and-exit cleanly.
func Open(botToken, configPath, serviceAccountPath, dbPath, fallbackPath string) (storage.Store, string, error) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, "", fmt.Errorf("loading config: %w", err)
	}
	name, err := resolveStorageBackend(cfg, configPath)
	if err != nil {
		return nil, "", err
	}
	switch name {
	case "sheets":
		root := filepath.Dir(configPath)
		dest, err := config.LoadDeploymentTransfer(filepath.Join(root, "deployment_transfer.json"))
		if err != nil {
			return nil, "", err
		}
		source, err := config.LoadSourceTransfer(filepath.Join(root, "source_transfer.json"))
		if err != nil {
			return nil, "", err
		}
		frozenSource := source != nil && (source.Phase == "preparing" || source.Phase == "prepared" || source.Phase == "stopping")
		if config.DeploymentInProgress(dest) || frozenSource {
			n, err := sheets.PendingFallbackCount(fallbackPath)
			if err != nil {
				return nil, "", err
			}
			if n > 0 {
				return nil, "", fmt.Errorf("deployment transfer cannot replay pending Sheets writes")
			}
		}
		sheetID, err := bot.EnsureSheetID(botToken, configPath, serviceAccountPath)
		if err != nil {
			return nil, "", fmt.Errorf("resolving google sheet: %w", err)
		}
		client, err := sheets.NewClient(serviceAccountPath, sheetID, fallbackPath)
		if err != nil {
			return nil, "", fmt.Errorf("initializing sheets client: %w", err)
		}
		return client, name, nil
	case "sqlite":
		store, err := sqlitestore.NewStore(dbPath)
		if err != nil {
			return nil, "", fmt.Errorf("initializing sqlite store: %w", err)
		}
		return store, name, nil
	default:
		return nil, "", fmt.Errorf("unknown storage backend %q", name)
	}
}

// resolveStorageBackend returns which storage backend to construct: "sheets" or
// "sqlite". An already-set backend is returned as-is. storage_backend unset but
// google_sheet_id already set is a v1.0 install predating the field — it's
// silently migrated to "sheets" and written back so this runs only once. Both
// unset means --setup has never run.
func resolveStorageBackend(cfg *config.Config, configPath string) (string, error) {
	if cfg.Setup.StorageBackend != nil {
		return *cfg.Setup.StorageBackend, nil
	}
	if cfg.Setup.GoogleSheetID != nil {
		name := "sheets"
		cfg.Setup.StorageBackend = &name
		if err := config.SaveConfig(configPath, cfg); err != nil {
			return "", fmt.Errorf("saving config: %w", err)
		}
		return name, nil
	}
	return "", errors.New("Run setup first: use the installer or tracker.exe --setup")
}
