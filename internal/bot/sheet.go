package bot

// The /sheet, /credentials, and /screenname commands: Google Sheet and
// service-account swapping (with the self-restart machinery /sheet needs)
// and screen-name changes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// handleSheetCommand implements /sheet. With no argument, replies with the
// current sheet's URL (the original, only behavior). With an argument
// (a sheet name or a pasted URL — same resolveSheetInput branch RunSetup
// and EnsureSheetID use), switches to a different Google Sheet: validates
// with sheets.NewClient (which also creates any missing tabs on the new
// sheet) before saving, so an unreachable sheet is never persisted.
//
// This only updates config.json — it deliberately does not attempt to swap
// the already-running store for the rest of this process's life (the store
// is a required non-nil dependency threaded through goroutines that are
// already running, and hot-swapping it would need a much larger refactor
// for what's a rare, deliberate action). The worker drains collection before
// saving the new sheet ID and restarting the owning installation's service.
func (b *Bot) handleSheetCommand(chatID int64, args string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	if cfg.Setup.StorageBackend != nil && *cfg.Setup.StorageBackend == "sqlite" {
		send(b.tg, chatID, "This install uses local storage — there is no Google Sheet. Use /export to get your data as CSV.")
		return
	}

	args = strings.TrimSpace(args)
	if args == "" {
		send(b.tg, chatID, b.store.GetSheetURL())
		return
	}
	if client, ok := b.store.(*sheets.Client); ok && client.PendingWrites() > 0 {
		send(b.tg, chatID, "Wait for queued writes to sync to the current sheet before changing sheets.")
		return
	}

	name, id, err := resolveSheetInput(b.serviceAccountPath, args)
	if err != nil {
		send(b.tg, chatID, fmt.Sprintf("Couldn't find that sheet: %v", err))
		return
	}
	if _, err := sheets.NewClient(b.serviceAccountPath, id, ""); err != nil {
		send(b.tg, chatID, fmt.Sprintf("Couldn't connect to that sheet: %v\nMake sure it's shared with the service account (Editor access), then try again.", err))
		return
	}

	b.restartConfiguration(chatID, "sheet", true, func(store storage.Store) (func() error, error) {
		if client, ok := store.(*sheets.Client); ok && client.PendingWrites() > 0 {
			return nil, fmt.Errorf("queued writes must sync to the current sheet first")
		}
		current, err := config.LoadConfig(b.configPath)
		if err != nil {
			return nil, err
		}
		previous := *current
		current.Setup.GoogleSheetName, current.Setup.GoogleSheetID = name, &id
		if err := config.SaveConfig(b.configPath, current); err != nil {
			return nil, err
		}
		return func() error { return config.SaveConfig(b.configPath, &previous) }, nil
	}, func() {})
}

// handleScreenNameCommand implements /screenname. With no argument, replies
// with the last PC-confirmed name and pending edits. An argument queues a PC change.
func (b *Bot) handleScreenNameCommand(chatID int64, args string) {
	args = strings.TrimSpace(args)
	if args != "" {
		b.saveCollectorChange(chatID, config.CollectorSettings{ScreenName: &args})
		return
	}
	state, err := config.LoadCollectorSettings(filepath.Join(filepath.Dir(b.configPath), "collector_settings.json"))
	if err != nil {
		send(b.tg, chatID, "Could not read the PC settings queue. Check logs.")
		return
	}
	text := "The PC has not reported a screen name. Usage: /screenname <name>"
	if state.Confirmed != nil && state.Confirmed.ScreenName != nil {
		text = "Last PC-confirmed screen name: " + *state.Confirmed.ScreenName
	}
	if state.Pending != nil && state.Pending.Settings.ScreenName != nil {
		text += "\nPending screen name: " + *state.Pending.Settings.ScreenName
	}
	if state.Error != "" {
		text += "\nLast PC error: " + state.Error
	}
	send(b.tg, chatID, text+"\n\n"+collectorSettingsExplanation)
}

// startCredentialsFlow implements /credentials: swaps the Google service
// account JSON used to authenticate to Sheets/Drive. Unlike the four
// config flows (paths/alerts/hardstop/schedule), this one expects a file
// upload rather than a plain-text reply — see handleCredentialsDocument.
// Deliberately not gated to the sheets backend like /sheet is: on a sqlite
// install this is how the credentials /storage needs to migrate to
// Google Sheets get onto the machine in the first place.
func (b *Bot) startCredentialsFlow(chatID int64) {
	b.flow = "credentials"
	b.field = "file"
	send(b.tg, chatID, "Send your google_service_account.json file now as an attachment, or type skip to keep the current one.")
}

// handleCredentialsTextReply handles a plain-text reply during the
// /credentials flow — the only valid one is "skip"; anything else
// re-prompts, since a real reply here is a file, not text.
func (b *Bot) handleCredentialsTextReply(chatID int64, text string) {
	if strings.EqualFold(strings.TrimSpace(text), "skip") {
		b.reset()
		send(b.tg, chatID, "Kept the existing credentials file.")
		return
	}
	send(b.tg, chatID, "Send the google_service_account.json file as an attachment, or type skip.")
}

// handleCredentialsDocument validates an owned staged upload before replacing
// the live key. Sheets keeps its selected sheet and old key on validation failure.
func (b *Bot) handleCredentialsDocument(chatID int64, fileID string) {
	if err := os.MkdirAll(filepath.Dir(b.serviceAccountPath), 0755); err != nil {
		send(b.tg, chatID, "Could not prepare credentials storage. Check logs.")
		return
	}
	staged, err := os.CreateTemp(filepath.Dir(b.serviceAccountPath), "credentials-upload-*.json")
	if err != nil {
		send(b.tg, chatID, "Could not prepare credentials storage. Check logs.")
		return
	}
	stagePath := staged.Name()
	if err := staged.Close(); err != nil {
		os.Remove(stagePath)
		send(b.tg, chatID, "Could not prepare credentials storage. Check logs.")
		return
	}
	retained := false
	defer func() {
		if !retained {
			os.Remove(stagePath)
		}
	}()
	if err := downloadTelegramFile(b.tg, fileID, stagePath); err != nil {
		slog.Error("downloading staged credentials", "err", err)
		send(b.tg, chatID, "Couldn't download that file. Existing credentials unchanged. Try again, or type skip.")
		return
	}
	bytes, err := os.ReadFile(stagePath)
	if err != nil || !json.Valid(bytes) {
		send(b.tg, chatID, "That file is not valid JSON. Existing credentials unchanged.")
		return
	}
	if b.currentBackend() == "sqlite" {
		if err := os.Rename(stagePath, b.serviceAccountPath); err != nil {
			send(b.tg, chatID, "Could not save credentials. Existing credentials unchanged. Check logs.")
			return
		}
		b.reset()
		send(b.tg, chatID, "✅ Credentials file saved. You can now run /storage sheets <sheet name or URL> to migrate to Google Sheets.")
		return
	}
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil || cfg.Setup.GoogleSheetID == nil {
		send(b.tg, chatID, "Could not determine the current sheet. Existing credentials unchanged. Check /sheet and try again.")
		return
	}
	if _, err := sheets.NewClient(stagePath, *cfg.Setup.GoogleSheetID, ""); err != nil {
		slog.Error("validating uploaded credentials against current sheet", "err", err)
		send(b.tg, chatID, "The new credentials could not access the current sheet. Existing credentials and sheet unchanged. Share that sheet with the new service account as Editor, then try again.")
		return
	}
	old, err := os.ReadFile(b.serviceAccountPath)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		send(b.tg, chatID, "Could not preserve existing credentials. Nothing changed. Check logs.")
		return
	}
	backupPath := ""
	if !missing {
		backup, err := os.CreateTemp(filepath.Dir(b.serviceAccountPath), "credentials-previous-*.json")
		if err != nil {
			send(b.tg, chatID, "Could not preserve existing credentials. Nothing changed. Check logs.")
			return
		}
		backupPath = backup.Name()
		_, writeErr := backup.Write(old)
		closeErr := backup.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			os.Remove(backupPath)
			send(b.tg, chatID, "Could not preserve existing credentials. Nothing changed. Check logs.")
			return
		}
	}
	cleanup := func() {
		os.Remove(stagePath)
		if backupPath != "" {
			os.Remove(backupPath)
		}
	}
	retained = true
	b.reset()
	b.restartConfiguration(chatID, "credentials", false, func(storage.Store) (func() error, error) {
		if err := os.Rename(stagePath, b.serviceAccountPath); err != nil {
			return nil, err
		}
		return func() error {
			if missing {
				return os.Remove(b.serviceAccountPath)
			}
			return os.Rename(backupPath, b.serviceAccountPath)
		}, nil
	}, cleanup)
}
