package bot

// The /switchbackend command: moves an install's entire history between the
// Google Sheets and SQLite backends and flips config.Setup.StorageBackend, so
// the tracker comes back on the other backend after the self-restart.
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

// pendingBackendSwitch holds the target backend prepared during /switchbackend
// preflight, kept until the player confirms or cancels. The target store is
// already constructed and validated (reachable, empty); confirming imports
// into it, cancelling discards it. Single-goroutine access (pollUpdates), same
// as conversation, so no locking.
type pendingBackendSwitch struct {
	target    string        // "sheets" | "sqlite"
	dst       storage.Store // constructed and validated empty; sheets target has its write-ahead fallback disabled
	sheetName *string       // config fields to persist on confirm (sheets target only)
	sheetID   string
}

// handleSwitchBackendCommand runs /switchbackend's preflight: determines the
// target backend, refuses if the source has unflushed writes or the target
// isn't empty/reachable, then asks the player to confirm the copy. The actual
// migration runs only on confirmation (resolveSwitchBackend).
//
// For a sheets target, args is the destination sheet name or URL (resolved the
// same way /sheet does); a sqlite target ignores args.
func (b *Bot) handleSwitchBackendCommand(chatID int64, args string) {
	// A second /switchbackend before answering the first discards the first's
	// prepared target (closing a sqlite handle it may have opened).
	b.discardPendingSwitch()

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}

	current := b.currentBackend(cfg)
	target := "sqlite"
	if current == "sqlite" {
		target = "sheets"
	}

	// Source preflight: a sheets source with queued write-ahead writes would
	// migrate incomplete data, since those writes are invisible to the reads.
	if sheetsSrc, ok := b.store.(*sheets.Client); ok {
		if n := sheetsSrc.PendingWrites(); n > 0 {
			send(b.tg, chatID, fmt.Sprintf("Can't migrate yet — %d sheet write(s) are still queued for retry (the sheet was briefly unreachable). Wait a minute for them to flush, then try again.", n))
			return
		}
	}

	dst, sheetName, sheetID, err := b.prepareTarget(target, strings.TrimSpace(args), cfg)
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
	// the tap can't slip through unseen.
	snap, err := b.store.ExportSnapshot()
	if err != nil {
		closeStore(dst)
		send(b.tg, chatID, fmt.Sprintf("Couldn't read your current data: %v", err))
		return
	}
	bankroll, err := b.store.GetCurrentBankroll()
	if err != nil {
		closeStore(dst)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}

	b.pendingSwitch = &pendingBackendSwitch{target: target, dst: dst, sheetName: sheetName, sheetID: sheetID}

	text := fmt.Sprintf(
		"⚠️ Switch storage backend: %s → %s\n\nThis copies everything to the new backend and restarts the tracker on it:\n• %d cash sessions\n• %d tournaments\n• %d ledger entries\n• %d time-analysis rows\nCurrent bankroll: $%.2f\n\nClose PokerStars first — a hand played mid-switch would be double-counted. Proceed?",
		backendLabel(current), backendLabel(target),
		len(snap.CashSessions), len(snap.Tournaments), len(snap.Ledger), len(snap.TimeAnalysis), bankroll)
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Migrate", "switchbackend:yes"),
			tgbotapi.NewInlineKeyboardButtonData("✖ Cancel", "switchbackend:no"),
		),
	)
	b.sendMessageWithKeyboard(text, markup)
}

// prepareTarget constructs and validates the destination backend. For sheets
// it resolves and connects to the named/pasted sheet with the write-ahead
// fallback disabled (a failed migration write must error, never buffer); for
// sqlite it opens tracker.db. Errors carry a player-facing message.
func (b *Bot) prepareTarget(target, args string, cfg *config.Config) (dst storage.Store, sheetName *string, sheetID string, err error) {
	switch target {
	case "sheets":
		if _, statErr := os.Stat(b.serviceAccountPath); statErr != nil {
			return nil, nil, "", fmt.Errorf("no Google credentials on this install yet. Run /credentials to upload your google_service_account.json, then /switchbackend <sheet name or URL>")
		}
		if args == "" {
			return nil, nil, "", fmt.Errorf("which sheet? Usage: /switchbackend <sheet name or URL> (must be an empty sheet shared with your service account)")
		}
		name, id, resolveErr := resolveSheetInput(b.serviceAccountPath, args)
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

// resolveSwitchBackend handles a tap on the /switchbackend confirmation. On
// confirm it runs the migration, and only if it verifies does it flip the
// backend in config and restart. On any failure config is left untouched, so
// the tracker stays on the current backend, and the half-written target is
// rolled back to empty for a clean retry.
func (b *Bot) resolveSwitchBackend(chatID int64, messageID int, confirmed bool) {
	b.clearKeyboard(chatID, messageID)

	ps := b.pendingSwitch
	if ps == nil {
		send(b.tg, chatID, "That switch already expired. Run /switchbackend again.")
		return
	}
	b.pendingSwitch = nil

	if !confirmed {
		b.rollbackTarget(ps)
		send(b.tg, chatID, "Cancelled. No data moved.")
		return
	}

	send(b.tg, chatID, "Migrating… this can take a moment.")

	migrated, err := storage.Migrate(b.store, ps.dst)
	if err != nil {
		b.rollbackTarget(ps)
		slog.Error("migrating backend", "err", err)
		send(b.tg, chatID, fmt.Sprintf("Migration failed — nothing changed, still on %s.\n%v\n\nBefore retrying, make sure the target is empty (a fresh sheet, or delete tracker.db).", backendLabel(b.currentBackendFromStore()), err))
		return
	}

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config after migration", "err", err)
		send(b.tg, chatID, "Data copied, but saving the backend choice failed. Check logs — do not run /switchbackend again until resolved.")
		return
	}
	cfg.Setup.StorageBackend = &ps.target
	if ps.target == "sheets" {
		cfg.Setup.GoogleSheetName = ps.sheetName
		cfg.Setup.GoogleSheetID = &ps.sheetID
	}
	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config after migration", "err", err)
		send(b.tg, chatID, "Data copied, but saving the backend choice failed. Check logs — do not run /switchbackend again until resolved.")
		return
	}

	// The restarted process opens the new backend fresh; close our sqlite
	// handle so the file isn't held open across the restart.
	closeStore(ps.dst)

	send(b.tg, chatID, fmt.Sprintf("✅ Migrated %d cash sessions, %d tournaments, and %d ledger entries to %s — bankroll verified equal. Restarting tracker in a moment…",
		len(migrated.CashSessions), len(migrated.Tournaments), len(migrated.Ledger), backendLabel(ps.target)))
	if err := selfRestart(b.configPath); err != nil {
		slog.Error("restarting tracker after backend switch", "err", err)
		send(b.tg, chatID, "Couldn't restart automatically — restart the tracker service by hand (Task Scheduler → Poker Tracker → Run, or reboot).")
		return
	}
	os.Exit(0)
}

// rollbackTarget returns a prepared/half-written target to empty so a retry
// passes the empty-target check: a sheets target's data rows are cleared, a
// sqlite target's file is closed and deleted. Best-effort — a failure here is
// logged, not surfaced, since the migration itself already failed or was
// cancelled and config was never changed.
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

// discardPendingSwitch drops a prepared-but-unanswered switch, releasing a
// sqlite handle it may hold. Unlike rollbackTarget it doesn't delete data —
// nothing was written yet.
func (b *Bot) discardPendingSwitch() {
	if b.pendingSwitch == nil {
		return
	}
	closeStore(b.pendingSwitch.dst)
	b.pendingSwitch = nil
}

// currentBackend reads the configured backend, falling back to the live
// store's type if config hasn't recorded it (a pre-backend-field install
// main.go would normally have migrated at startup).
func (b *Bot) currentBackend(cfg *config.Config) string {
	if cfg.Setup.StorageBackend != nil && *cfg.Setup.StorageBackend != "" {
		return *cfg.Setup.StorageBackend
	}
	return b.currentBackendFromStore()
}

func (b *Bot) currentBackendFromStore() string {
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
