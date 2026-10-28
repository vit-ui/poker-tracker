package bot

// The /sheet, /credentials, and /screenname commands: Google Sheet and
// service-account swapping (with the self-restart machinery /sheet needs)
// and screen-name changes.

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sheets"
)

// selfRestart kills this process by PID and re-runs the "Poker Tracker"
// scheduled task, so the fresh process picks up whatever handleSheetCommand
// just saved to config.json. Used only there.
//
// The kill-then-run sequence is written out as a .bat and run as
// cmd.exe /C <path> rather than as an inline `cmd.exe /C "a & b & c"`
// string: passing a single clean path removes a layer of quote-mangling
// that an inline string doesn't get for free (Go's argv escaping, then
// cmd.exe's own parser, both operating on the same `"Poker Tracker"`
// argument — two chances to get it wrong instead of one). Written next to
// config.json rather than into os.TempDir() — the system temp path can
// itself contain spaces (e.g. a "First Last" profile directory), which
// would just relocate the exact quoting hazard this is meant to avoid onto
// cmd's handling of the .bat path itself; config.json's directory is the
// installer's fixed, space-free C:\poker-tracker. The script's last line
// deletes itself, so nothing is left behind in that directory once the
// restart has fired — confirmed working on the real machine, so this
// doesn't keep a diagnostic log around for a failure mode that's no
// longer expected.
//
// CREATE_BREAKAWAY_FROM_JOB (see detachedSysProcAttr) keeps the helper
// alive after this process exits — without it, Task Scheduler's Job Object
// kills every child of "Poker Tracker" the instant its main process ends,
// before schtasks /Run ever gets to run. Confirmed in practice: without
// this flag, /sheet killed the tracker and nothing came back. Some Task
// Scheduler configurations reject breakaway outright (CreateProcess fails
// with access denied); if Start fails, this retries once without it rather
// than giving up — an unprotected restart attempt is still better odds
// than none.
func selfRestart(configPath string) error {
	dir := filepath.Dir(configPath)
	batPath := filepath.Join(dir, "restart.bat")
	script := fmt.Sprintf(
		"@echo off\r\n"+
			"taskkill /F /PID %d\r\n"+
			"ping -n 4 127.0.0.1 >nul\r\n"+
			"schtasks /Run /TN \"Poker Tracker\"\r\n"+
			"del \"%%~f0\"\r\n",
		os.Getpid())

	if err := os.WriteFile(batPath, []byte(script), 0644); err != nil {
		return fmt.Errorf("writing restart script: %w", err)
	}

	cmd := exec.Command("cmd.exe", "/C", batPath)
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		cmd = exec.Command("cmd.exe", "/C", batPath)
		cmd.SysProcAttr = noWindowSysProcAttr()
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("starting restart script: %w", err)
		}
	}
	return nil
}

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
// for what's a rare, deliberate action). Instead, once the new sheet ID is
// saved this process kills itself and restarts the "Poker Tracker"
// scheduled task via a detached command, so the fresh process picks up the
// new sheet ID from config.json on startup.
func (b *Bot) handleSheetCommand(chatID int64, args string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		b.reply(chatID, "Something went wrong. Check logs.")
		return
	}
	if cfg.Setup.StorageBackend != nil && *cfg.Setup.StorageBackend == "sqlite" {
		b.reply(chatID, "This install uses local storage — there is no Google Sheet. Use /export to get your data as CSV.")
		return
	}

	args = strings.TrimSpace(args)
	if args == "" {
		b.reply(chatID, b.store.GetSheetURL())
		return
	}

	name, id, err := resolveSheetInput(b.serviceAccountPath, args)
	if err != nil {
		b.reply(chatID, fmt.Sprintf("Couldn't find that sheet: %v", err))
		return
	}
	if _, err := sheets.NewClient(b.serviceAccountPath, id, ""); err != nil {
		b.reply(chatID, fmt.Sprintf("Couldn't connect to that sheet: %v\nMake sure it's shared with the service account (Editor access), then try again.", err))
		return
	}

	cfg.Setup.GoogleSheetName = name
	cfg.Setup.GoogleSheetID = &id
	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		b.reply(chatID, "Something went wrong. Check logs.")
		return
	}

	b.reply(chatID, "✅ Sheet updated. Restarting tracker in a moment...")

	if err := selfRestart(b.configPath); err != nil {
		// A live process on stale config beats a dead one — don't exit if
		// the restart helper never got off the ground.
		slog.Error("restarting tracker after sheet switch", "err", err)
		b.reply(chatID, "Couldn't restart automatically — restart the tracker service by hand (Task Scheduler → Poker Tracker → Run, or reboot).")
		return
	}
	os.Exit(0)
}

// handleScreenNameCommand implements /screenname. With no argument, replies
// with the current PokerStars screen name. With an argument, changes it —
// unlike /sheet and /credentials, this takes effect immediately via
// restartWatcher rather than needing a service restart: the watched folder
// path is the base path joined with the screen name (see resolvePaths in
// main.go), resolved once at watcher-start time, so a stale watcher would
// otherwise keep watching the old screen name's subfolder until restarted
// — exactly the same reason /paths calls restartWatcher on save.
//
// This is the always-available counterpart to ensureScreenName's recovery
// path, which only ever fires when the screen name is still null — once
// it's set, ensureScreenName never touches it again, so this was the only
// way to change an already-configured screen name.
func (b *Bot) handleScreenNameCommand(chatID int64, args string) {
	args = strings.TrimSpace(args)

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		b.reply(chatID, "Something went wrong. Check logs.")
		return
	}

	if args == "" {
		if cfg.Setup.PokerStarsScreenName == nil {
			b.reply(chatID, "No screen name configured yet. Usage: /screenname <name>")
			return
		}
		b.reply(chatID, "Current PokerStars screen name: "+*cfg.Setup.PokerStarsScreenName)
		return
	}

	cfg.Setup.PokerStarsScreenName = &args
	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		b.reply(chatID, "Something went wrong. Check logs.")
		return
	}

	b.restartWatcher()
	b.reply(chatID, "✅ Screen name updated to "+args+". Watcher restarted.")
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
	b.reply(chatID, "Send your google_service_account.json file now as an attachment, or type skip to keep the current one.")
}

// handleCredentialsTextReply handles a plain-text reply during the
// /credentials flow — the only valid one is "skip"; anything else
// re-prompts, since a real reply here is a file, not text.
func (b *Bot) handleCredentialsTextReply(chatID int64, text string) {
	if strings.EqualFold(strings.TrimSpace(text), "skip") {
		b.reset()
		b.reply(chatID, "Kept the existing credentials file.")
		return
	}
	b.reply(chatID, "Send the google_service_account.json file as an attachment, or type skip.")
}

// handleCredentialsDocument saves an uploaded google_service_account.json
// to serviceAccountPath (overwriting whatever was there), then
// re-validates the currently configured sheet against it. A different
// service account is very likely a different Google Cloud identity, and a
// sheet only works if it's been explicitly shared with whichever service
// account is actually in use — the sheet that worked under the old
// credentials may well not be reachable under the new ones. If it isn't,
// the now-unreachable sheet is cleared from config (rather than left
// pointing at something the tracker can no longer write to) and the
// player is told to run /sheet to reconnect one. Either way this only
// touches config.json and the credentials file on disk — same as /sheet,
// the already-running store isn't affected until the service restarts.
func (b *Bot) handleCredentialsDocument(chatID int64, fileID string) {
	if err := downloadTelegramFile(b.tg, fileID, b.serviceAccountPath); err != nil {
		slog.Error("downloading credentials file", "err", err)
		b.reply(chatID, "Couldn't download that file. Try again, or type skip.")
		return
	}
	b.reset()

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		b.reply(chatID, "✅ Credentials file saved, but something went wrong checking your sheet. Check logs, and run /sheet to reconnect if needed.")
		return
	}

	// On the sqlite backend there's no sheet to re-validate — the credentials
	// exist solely so /storage can migrate to Google Sheets.
	if cfg.Setup.StorageBackend != nil && *cfg.Setup.StorageBackend == "sqlite" {
		b.reply(chatID, "✅ Credentials file saved. You can now run /storage sheets <sheet name or URL> to migrate to Google Sheets.")
		return
	}

	if cfg.Setup.GoogleSheetID == nil {
		b.reply(chatID, "✅ Credentials file saved. Restart the tracker service for this to take effect. No sheet is configured yet — run /sheet to connect one.")
		return
	}

	if _, err := sheets.NewClient(b.serviceAccountPath, *cfg.Setup.GoogleSheetID, ""); err != nil {
		cfg.Setup.GoogleSheetID = nil
		cfg.Setup.GoogleSheetName = nil
		if saveErr := config.SaveConfig(b.configPath, cfg); saveErr != nil {
			slog.Error("saving config", "err", saveErr)
		}
		b.reply(chatID, "✅ Credentials file saved, but your current sheet isn't accessible with it — it needs to be shared with the new service account, or you're pointing at a different Google Cloud project entirely. Run /sheet to connect a sheet this service account can reach.")
		return
	}

	b.reply(chatID, "✅ Credentials file saved. Your existing sheet is still accessible with it. Restart the tracker service for this to take effect.")
}
