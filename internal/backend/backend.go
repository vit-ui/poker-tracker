// Package backend is the storage composition root shared by the binaries that
// run the server role (the all-local tracker and the split server): it resolves
// which storage backend an install uses and constructs it. It lives outside the
// mains so the construction isn't duplicated between them.
package backend

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// Open resolves the configured storage backend and constructs it, returning the
// store and the backend name ("sheets" or "sqlite").
func Open(configPath, serviceAccountPath, dbPath, fallbackPath string) (storage.Store, string, error) {
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
		if cfg.Setup.GoogleSheetID == nil || strings.TrimSpace(*cfg.Setup.GoogleSheetID) == "" {
			return nil, "", fmt.Errorf("Google Sheets setup is incomplete; run local setup to resolve the spreadsheet")
		}
		client, err := sheets.NewClient(serviceAccountPath, *cfg.Setup.GoogleSheetID, fallbackPath)
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

// ResolveSheetsSetup validates the selected spreadsheet and stores its ID in cfg.
func ResolveSheetsSetup(cfg *config.Config, serviceAccountPath string) error {
	if cfg.Setup.StorageBackend == nil || *cfg.Setup.StorageBackend != "sheets" {
		return nil
	}
	var id string
	if cfg.Setup.GoogleSheetID != nil {
		id = strings.TrimSpace(*cfg.Setup.GoogleSheetID)
	}
	if id == "" {
		if cfg.Setup.GoogleSheetName == nil || strings.TrimSpace(*cfg.Setup.GoogleSheetName) == "" {
			return fmt.Errorf("Google Sheets setup needs a spreadsheet name or URL")
		}
		var err error
		id, err = sheets.ResolveSheetID(serviceAccountPath, *cfg.Setup.GoogleSheetName)
		if err != nil {
			return fmt.Errorf("resolving spreadsheet during setup: %w", err)
		}
	}
	if _, err := sheets.NewClient(serviceAccountPath, id, ""); err != nil {
		return fmt.Errorf("checking spreadsheet during setup: %w", err)
	}
	cfg.Setup.GoogleSheetID = &id
	return nil
}
