package bot

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func (b *Bot) confirmStorageReset(chatID int64) {
	destination := "the local tracker.db"
	if b.currentBackend() == "sheets" {
		destination = b.store.GetSheetURL()
	}
	text := "Reset " + destination + "? This empties cash sessions, tournaments, and the bankroll ledger. A recovery snapshot is saved on the storage machine. Close PokerStars and keep the poker PC connected before confirming."
	b.storageResetMessageID = b.showMenu(chatID, 0, text, [][]tgbotapi.InlineKeyboardButton{
		menuRow("Reset history", "storage:reset:yes"),
		menuRow("Cancel", "storage:reset:no"),
	})
}

func (b *Bot) resolveStorageReset(chatID int64, messageID int, confirmed bool) {
	if b.storageResetMessageID == 0 || messageID != b.storageResetMessageID {
		return
	}
	b.closeMenu(chatID, messageID)
	if !confirmed {
		send(b.tg, chatID, "Storage reset cancelled.")
		return
	}
	if b.fileScanActive {
		send(b.tg, chatID, "Wait for the file scan to finish before resetting storage.")
		return
	}
	root := filepath.Dir(b.configPath)
	b.migrating = true
	send(b.tg, chatID, "Draining collection before backing up and resetting storage…")
	var backup string
	b.queueOperation(func(store storage.Store) storage.OperationResult {
		var err error
		backup, err = resetStorageHistory(store, root)
		return storage.OperationResult{Err: err}
	}, func(result storage.OperationResult) {
		if result.Err != nil {
			b.migrating = false
			slog.Error("resetting storage", "err", result.Err)
			send(b.tg, chatID, "Could not complete the storage reset: "+result.Err.Error())
			return
		}
		if err := selfRestart(b.configPath, b.taskName); err != nil {
			slog.Error("restarting after storage reset", "err", err)
			send(b.tg, chatID, "Storage is empty and the recovery snapshot is saved at "+backup+". Automatic restart failed. Restart the tracker service manually to resume collection.")
			return
		}
		b.restarting = true
		send(b.tg, chatID, "Storage is empty. Restarting the tracker. Recovery snapshot: "+backup)
	})
	// Pause and drain like migration. Once cleared, a restart drops old
	// in-memory accounting state before collection can resume.
	request := &b.pendingOperations[len(b.pendingOperations)-1].request
	request.PrepareMigration = true
}

func resetStorageHistory(store storage.Store, root string) (string, error) {
	pending, err := config.LoadPendingTournaments(filepath.Join(root, "pending_tournaments.json"))
	if err != nil {
		return "", fmt.Errorf("checking pending tournaments: %w", err)
	}
	if len(pending) > 0 {
		return "", fmt.Errorf("answer the pending tournament questions before resetting storage")
	}
	var clear func() error
	switch current := store.(type) {
	case *sqlitestore.Store:
		clear = current.ClearAllData
	case *sheets.Client:
		if err := current.CheckTransferReady(); err != nil {
			return "", err
		}
		clear = current.ClearHistory
	default:
		return "", fmt.Errorf("unsupported storage backend")
	}
	snapshot, err := store.ExportSnapshot()
	if err != nil {
		return "", fmt.Errorf("reading storage for backup: %w", err)
	}
	backup, err := saveResetBackup(root, snapshot)
	if err != nil {
		return "", err
	}
	clearErr := clear()
	current, readErr := store.ExportSnapshot()
	if readErr != nil || !current.IsEmpty() {
		if readErr == nil {
			readErr = fmt.Errorf("storage still contains rows")
		}
		return "", fmt.Errorf("could not verify the reset (recovery snapshot %s): %w", backup, errors.Join(clearErr, readErr))
	}
	// A Sheets response can fail after the server applied the entire batch.
	// A verified empty store establishes success in that case as well.
	return backup, nil
}

func saveResetBackup(root string, snapshot storage.Snapshot) (path string, err error) {
	dir := filepath.Join(root, "storage-backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("creating storage backup directory: %w", err)
	}
	f, err := os.CreateTemp(dir, "before-reset-*.json")
	if err != nil {
		return "", fmt.Errorf("creating recovery snapshot: %w", err)
	}
	defer func() {
		f.Close()
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	if err := json.NewEncoder(f).Encode(snapshot); err != nil {
		return "", fmt.Errorf("writing recovery snapshot: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("syncing recovery snapshot: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return "", fmt.Errorf("opening recovery snapshot for verification: %w", err)
	}
	var saved storage.Snapshot
	if err := json.NewDecoder(f).Decode(&saved); err != nil {
		return "", fmt.Errorf("reading recovery snapshot: %w", err)
	}
	if err := storage.VerifySnapshot(snapshot, saved, time.Nanosecond); err != nil {
		return "", fmt.Errorf("verifying recovery snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("closing recovery snapshot: %w", err)
	}
	return f.Name(), nil
}
