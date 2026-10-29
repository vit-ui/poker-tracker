package bot

// The /storage command: with no argument, shows where the install's data
// lives; with one, switches to the other backend — moving the entire history
// across and flipping config.Setup.StorageBackend, so the tracker comes back
// on the new backend after the self-restart. Same view-vs-change shape as
// /sheet.
//
// The migration is a raw row copy (storage.Migrate → ExportSnapshot/
// ImportSnapshot), never a replay through the Append* methods — replaying
// would re-fold tournament bounties and recompute cash session IDs off a
// FirstHandID the read side never carries, silently corrupting the bankroll.
// Like /sheet, it doesn't hot-swap the live store: it writes config and
// restarts the process (selfRestart), which reconstructs the store on the new
// backend. Destructive, so it asks for inline-keyboard confirmation first.

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
)

const storageUsage = "Usage:\n" +
	"/storage — show where your data is stored\n" +
	"/storage local — switch to the local file\n" +
	"/storage sheets <sheet name or URL> — switch to Google Sheets (needs an empty sheet shared with your service account)"

// pendingBackendSwitch holds the target backend prepared during /storage
// preflight, kept until the player confirms or cancels. The target store is
// already constructed and validated (reachable, empty); confirming imports
// into it, cancelling discards it. Single-goroutine access on the bot event loop, same
// as conversation, so no locking.
type pendingBackendSwitch struct {
	target    string        // "sheets" | "sqlite"
	dst       storage.Store // constructed and validated empty; sheets target has its write-ahead fallback disabled
	sheetName *string       // config fields to persist on confirm (sheets target only)
	sheetID   string
}

// handleStorageCommand implements /storage. No argument shows the active
// backend. An argument names the target backend and runs the switch preflight:
// refuse if the source has unflushed writes or the target isn't
// empty/reachable, then ask the player to confirm the copy. The actual
// migration runs only on confirmation (resolveStorageSwitch).
func (b *Bot) handleStorageCommand(chatID int64, args string) {
	// A second /storage before answering a pending confirmation discards the
	// prepared target (closing and removing the empty db it may have created)
	// and abandons a half-started inline credentials upload.
	b.discardPendingSwitch()
	if b.flow == "storage_credentials" {
		b.reset()
	}
	b.pendingStorageSheetArg = ""

	current := b.currentBackend()

	args = strings.TrimSpace(args)
	if args == "" {
		if current == "sheets" {
			send(b.tg, chatID, "Storage: Google Sheets — "+b.store.GetSheetURL()+"\nSwitch to the local file with /storage local")
		} else {
			send(b.tg, chatID, "Storage: local file (tracker.db next to the tracker). Use /export to get your data as CSV.\nSwitch with /storage sheets <sheet name or URL>")
		}
		return
	}

	first := strings.Fields(args)[0]
	var target string
	switch strings.ToLower(first) {
	case "sheets", "sheet", "google":
		target = "sheets"
	case "local", "sqlite", "file":
		target = "sqlite"
	default:
		send(b.tg, chatID, fmt.Sprintf("Unknown storage %q. %s", first, storageUsage))
		return
	}
	if target == current {
		send(b.tg, chatID, "Already using "+backendLabel(current)+" — nothing changed.")
		return
	}
	sheetArg := strings.TrimSpace(strings.TrimPrefix(args, first))

	// Source preflight: a sheets source with queued write-ahead writes would
	// migrate incomplete data, since those writes are invisible to the reads.
	// Re-checked at confirm time (resolveStorageSwitch) — the tap can come
	// minutes from now.
	if sheetsSrc, ok := b.store.(*sheets.Client); ok {
		if n := sheetsSrc.PendingWrites(); n > 0 {
			send(b.tg, chatID, fmt.Sprintf("Can't migrate yet — %d sheet write(s) are still queued for retry (the sheet was briefly unreachable). Wait a minute for them to flush, then try again.", n))
			return
		}
	}

	// Migrating to Sheets needs the service-account key on disk. Rather than
	// erroring "run /credentials first", collect it inline and resume the
	// switch once it's uploaded (handleStorageCredentialsDocument).
	if target == "sheets" && sheetArg != "" {
		if _, err := os.Stat(b.serviceAccountPath); err != nil {
			b.pendingStorageSheetArg = sheetArg
			b.flow = "storage_credentials"
			b.field = "file"
			send(b.tg, chatID, "This install has no Google credentials yet. Send your google_service_account.json file as an attachment and I'll continue the migration — or type cancel.")
			return
		}
	}

	b.prepareAndConfirmSwitch(chatID, target, sheetArg)
}

// prepareAndConfirmSwitch constructs and validates the target backend, then
// asks the player to confirm the copy (resolveStorageSwitch runs it). Shared by
// the direct /storage path and the inline-credentials resume.
func (b *Bot) prepareAndConfirmSwitch(chatID int64, target, sheetArg string) {
	dst, sheetName, sheetID, err := b.prepareTarget(target, sheetArg)
	if err != nil {
		send(b.tg, chatID, err.Error())
		return
	}

	// Confirm the target is empty before offering to migrate — no merge mode.
	existing, err := dst.ExportSnapshot()
	if err != nil {
		closeStore(dst)
		send(b.tg, chatID, fmt.Sprintf("Couldn't read the target %s: %v", target, err))
		return
	}
	if !existing.IsEmpty() {
		closeStore(dst)
		if target == "sqlite" {
			send(b.tg, chatID, "The local database already has data. Delete tracker.db (next to the tracker) and try again — migration only runs into a fresh backend.")
		} else {
			send(b.tg, chatID, "That sheet already has data. Migration needs an empty sheet — use a brand-new one, then try again.")
		}
		return
	}

	// Counts for the confirmation come from a source read here; the migration
	// on confirm re-reads fresh (storage.Migrate), so a change between now and
	// the tap can't slip through unseen. The bankroll is the ledger's last
	// running total, already in the snapshot.
	snap, err := b.store.ExportSnapshot()
	if err != nil {
		closeStore(dst)
		send(b.tg, chatID, fmt.Sprintf("Couldn't read your current data: %v", err))
		return
	}
	var bankroll float64
	if n := len(snap.Ledger); n > 0 {
		bankroll = snap.Ledger[n-1].RunningTotal
	}

	b.pendingSwitch = &pendingBackendSwitch{target: target, dst: dst, sheetName: sheetName, sheetID: sheetID}

	text := fmt.Sprintf(
		"⚠️ Switch storage: %s → %s\n\nThis copies everything to the new backend and restarts the tracker on it:\n• %d cash sessions\n• %d tournaments\n• %d ledger entries\nCurrent bankroll: $%.2f\n\nClose PokerStars first — the collector will check before pausing collection. Proceed?",
		backendLabel(b.currentBackend()), backendLabel(target),
		len(snap.CashSessions), len(snap.Tournaments), len(snap.Ledger), bankroll)
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Migrate", "storage:yes"),
			tgbotapi.NewInlineKeyboardButtonData("✖ Cancel", "storage:no"),
		),
	)
	b.sendMessageWithKeyboard(text, markup)
}

// handleStorageCredentialsDocument saves the service-account key uploaded
// during an inline /storage migration and resumes the switch with the sheet
// argument the player originally gave.
func (b *Bot) handleStorageCredentialsDocument(chatID int64, fileID string) {
	if err := downloadTelegramFile(b.tg, fileID, b.serviceAccountPath); err != nil {
		slog.Error("downloading credentials file", "err", err)
		send(b.tg, chatID, "Couldn't download that file. Try again, or type cancel.")
		return
	}
	sheetArg := b.pendingStorageSheetArg
	b.pendingStorageSheetArg = ""
	b.reset()
	send(b.tg, chatID, "✅ Credentials saved. Continuing the migration…")
	b.prepareAndConfirmSwitch(chatID, "sheets", sheetArg)
}

// handleStorageCredentialsText handles a plain-text reply during the inline
// /storage credentials upload — only "cancel" (or "skip") is valid; anything
// else re-prompts, since the real reply here is a file.
func (b *Bot) handleStorageCredentialsText(chatID int64, text string) {
	t := strings.TrimSpace(text)
	if strings.EqualFold(t, "cancel") || strings.EqualFold(t, "skip") {
		b.pendingStorageSheetArg = ""
		b.reset()
		send(b.tg, chatID, "Cancelled. No data moved.")
		return
	}
	send(b.tg, chatID, "Send the google_service_account.json file as an attachment, or type cancel.")
}

// prepareTarget constructs and validates the destination backend. For sheets
// it resolves and connects to the named/pasted sheet with the write-ahead
// fallback disabled (a failed migration write must error, never buffer); for
// sqlite it opens tracker.db. Errors carry a player-facing message.
func (b *Bot) prepareTarget(target, sheetArg string) (dst storage.Store, sheetName *string, sheetID string, err error) {
	switch target {
	case "sheets":
		if _, statErr := os.Stat(b.serviceAccountPath); statErr != nil {
			return nil, nil, "", fmt.Errorf("no Google credentials on this install yet. Run /credentials to upload your google_service_account.json, then /storage sheets <sheet name or URL>")
		}
		if sheetArg == "" {
			return nil, nil, "", fmt.Errorf("which sheet? Usage: /storage sheets <sheet name or URL> (must be an empty sheet shared with your service account)")
		}
		name, id, resolveErr := resolveSheetInput(b.serviceAccountPath, sheetArg)
		if resolveErr != nil {
			return nil, nil, "", fmt.Errorf("couldn't find that sheet: %v", resolveErr)
		}
		client, connErr := sheets.NewClient(b.serviceAccountPath, id, "")
		if connErr != nil {
			return nil, nil, "", fmt.Errorf("couldn't connect to that sheet: %v\nMake sure it's shared with the service account (Editor access), then try again", connErr)
		}
		return client, name, id, nil
	default: // sqlite
		store, openErr := sqlitestore.NewStore(b.dbPath)
		if openErr != nil {
			return nil, nil, "", fmt.Errorf("couldn't open the local database: %v", openErr)
		}
		return store, nil, "", nil
	}
}

// resolveStorageSwitch handles a tap on the /storage confirmation. On confirm
// it runs the migration, and only if it verifies does it flip the backend in
// config and restart. On any failure config is left untouched, so the tracker
// stays on the current backend, and the half-written target is rolled back to
// empty for a clean retry.
func (b *Bot) resolveStorageSwitch(chatID int64, messageID int, confirmed bool) {
	b.clearKeyboard(chatID, messageID)

	ps := b.pendingSwitch
	if ps == nil {
		send(b.tg, chatID, "That switch already expired. Run /storage again.")
		return
	}
	b.pendingSwitch = nil

	if !confirmed {
		// Nothing was written to the target — discard, don't roll back.
		b.discardTarget(ps)
		send(b.tg, chatID, "Cancelled. No data moved.")
		return
	}

	// Recheck here for a prompt refusal; the worker repeats this check after
	// the collector's pause/drain acknowledgement and pending cash finalization.

	if sheetsSrc, ok := b.store.(*sheets.Client); ok {
		if n := sheetsSrc.PendingWrites(); n > 0 {
			b.discardTarget(ps)
			send(b.tg, chatID, fmt.Sprintf("Can't migrate — %d sheet write(s) queued since the confirmation was sent. Wait a minute for them to flush, then run /storage again.", n))
			return
		}
	}

	b.migrating = true
	send(b.tg, chatID, "Checking that PokerStars is closed and draining collection before migrating…")
	release := make(chan struct{})
	resumed := make(chan error, 1)
	var previous *config.Config
	b.queueOperation(func(source storage.Store) storage.OperationResult {
		slog.Info("backend migration starting", "target", ps.target)
		migrated, err := storage.Migrate(source, ps.dst)
		if err != nil {
			b.rollbackTarget(ps)
			return storage.OperationResult{Err: err}
		}
		cfg, err := config.LoadConfig(b.configPath)
		if err != nil {
			b.rollbackTarget(ps)
			return storage.OperationResult{Err: fmt.Errorf("loading backend configuration: %w", err)}
		}
		saved := *cfg
		previous = &saved
		cfg.Setup.StorageBackend = &ps.target
		if ps.target == "sheets" {
			cfg.Setup.GoogleSheetName = ps.sheetName
			cfg.Setup.GoogleSheetID = &ps.sheetID
		}
		if err := config.SaveConfig(b.configPath, cfg); err != nil {
			b.rollbackTarget(ps)
			return storage.OperationResult{Err: fmt.Errorf("saving backend configuration: %w", err)}
		}
		return storage.OperationResult{Snapshot: &migrated}
	}, func(result storage.OperationResult) {
		if result.Err != nil {
			// A collector refusal occurs before Apply, so release the untouched target
			// too. Rollback after an Apply failure has already run on the worker.
			b.discardTarget(ps)
			b.migrating = false
			slog.Error("migrating backend", "err", result.Err)
			send(b.tg, chatID, fmt.Sprintf("Migration failed; still using %s.\n%v", backendLabel(b.currentBackend()), result.Err))
			return
		}
		snap := result.Snapshot
		closeStore(ps.dst)
		slog.Info("backend migration verified", "target", ps.target, "cash_sessions", len(snap.CashSessions), "tournaments", len(snap.Tournaments), "ledger", len(snap.Ledger))
		send(b.tg, chatID, fmt.Sprintf("✅ Migrated %d cash sessions, %d tournaments, and %d ledger entries to %s. Amounts and bankroll verified. Restarting tracker…", len(snap.CashSessions), len(snap.Tournaments), len(snap.Ledger), backendLabel(ps.target)))
		if err := selfRestart(b.configPath, b.taskName); err == nil {
			os.Exit(0)
		} else {
			slog.Error("restarting tracker after backend switch", "err", err)
		}
		// The worker and collector are still paused. Restore configuration before
		// releasing either, so later accepted writes cannot fork the two backends.
		if err := config.SaveConfig(b.configPath, previous); err != nil {
			slog.Error("restoring configuration after failed restart", "err", err)
			send(b.tg, chatID, "Automatic restart failed and the previous settings could not be restored. Collection remains paused and the verified destination is preserved. Restart the tracker service manually to use the copied data.")
			return
		}
		b.rollbackTarget(ps)
		close(release)
		b.queueOperation(func(storage.Store) storage.OperationResult { return storage.OperationResult{Err: <-resumed} }, func(result storage.OperationResult) {
			b.migrating = false
			if result.Err != nil {
				send(b.tg, chatID, fmt.Sprintf("Automatic restart failed. The previous storage settings were restored, but collection could not resume: %v. Restart the collector service manually.", result.Err))
			} else {
				send(b.tg, chatID, "Automatic restart failed. The copy was rolled back and collection resumed using the previous storage. Nothing changed.")
			}
		})
	})
	request := &b.pendingOperations[len(b.pendingOperations)-1].request
	request.PrepareMigration = true
	request.Release = release
	request.Resumed = resumed
}

// rollbackTarget returns a half-written target to empty so a retry passes the
// empty-target check: a sheets target's data rows are cleared, a sqlite
// target's file is closed and deleted. Best-effort — a failure here is
// logged, not surfaced, since the migration itself already failed and config
// was never changed.
func (b *Bot) rollbackTarget(ps *pendingBackendSwitch) {
	switch dst := ps.dst.(type) {
	case *sheets.Client:
		if err := dst.ClearAllData(); err != nil {
			slog.Error("clearing target sheet after failed migration", "err", err)
		}
	case *sqlitestore.Store:
		dst.Close()
		if err := os.Remove(b.dbPath); err != nil && !os.IsNotExist(err) {
			slog.Error("removing target database after failed migration", "err", err)
		}
	}
}

// discardTarget releases a prepared target that was never written to: closes
// the sqlite handle and removes the freshly created empty tracker.db. Unlike
// rollbackTarget there is nothing to clear on a sheets target — no row was
// appended.
func (b *Bot) discardTarget(ps *pendingBackendSwitch) {
	if dst, ok := ps.dst.(*sqlitestore.Store); ok {
		dst.Close()
		if err := os.Remove(b.dbPath); err != nil && !os.IsNotExist(err) {
			slog.Error("removing unused target database", "err", err)
		}
	}
}

// discardPendingSwitch drops a prepared-but-unanswered switch.
func (b *Bot) discardPendingSwitch() {
	if b.pendingSwitch == nil {
		return
	}
	b.discardTarget(b.pendingSwitch)
	b.pendingSwitch = nil
}

// currentBackend reports which backend the live store is — the store's
// concrete type is definitionally the running backend (runServe constructed
// it from config at startup).
func (b *Bot) currentBackend() string {
	if _, ok := b.store.(*sheets.Client); ok {
		return "sheets"
	}
	return "sqlite"
}

// closeStore closes a store that owns a closable resource (the sqlite file);
// the sheets client has nothing to close.
func closeStore(s storage.Store) {
	if c, ok := s.(*sqlitestore.Store); ok {
		c.Close()
	}
}

func backendLabel(backend string) string {
	if backend == "sheets" {
		return "Google Sheets"
	}
	return "local file"
}
