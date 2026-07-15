// Package bot implements the Telegram bot: outgoing session alerts and
// incoming commands for configuration and status queries.
package bot

import (
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// Run starts the bot in serve mode. Forwards alerts from alertChan to
// Telegram, sends weekly/monthly summaries and satellite/ticket reminders on
// a schedule, and polls Telegram for incoming commands. Blocks until
// alertChan is closed.
//
// restartWatcher is called by the /paths command once new folder paths are
// saved — it re-joins the base paths with the screen name and restarts the
// watcher goroutine.
//
// serviceAccountPath is only used by /sheet when switching to a different
// spreadsheet — see handleSheetCommand.
func Run(botToken string, alertChan <-chan string, sheetsClient storage.Store, configPath, lockfilePath, serviceAccountPath string, restartWatcher func()) error {
	tgBot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return fmt.Errorf("starting telegram bot: %w", err)
	}

	var lastWeeklySent, lastMonthlySent time.Time

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			if now.Weekday() == time.Monday && inSummaryWindow(now) && !sameDay(lastWeeklySent, now) {
				summary, err := buildWeeklySummary(sheetsClient, now)
				if err != nil {
					slog.Error("building weekly summary", "err", err)
				} else {
					sendMessage(tgBot, configPath, summary)
					lastWeeklySent = now
				}
			}
			if now.Day() == 1 && inSummaryWindow(now) && !sameDay(lastMonthlySent, now) {
				summary, err := buildMonthlySummary(sheetsClient, now)
				if err != nil {
					slog.Error("building monthly summary", "err", err)
				} else {
					sendMessage(tgBot, configPath, summary)
					lastMonthlySent = now
				}
			}
			checkPendingTournaments(tgBot, configPath, now)
		}
	}()

	go pollUpdates(tgBot, sheetsClient, configPath, lockfilePath, serviceAccountPath, restartWatcher)

	for msg := range alertChan {
		sendMessage(tgBot, configPath, msg)
	}
	return nil
}

// RunSetup starts the bot in setup mode and blocks until the player
// completes the configuration wizard via Telegram: chat ID detection,
// an optional service account credentials upload, Google Sheet, PokerStars
// screen name, and both base folder paths. Used only by --setup — no
// sheets client and no watcher exist yet at this point, so this runs
// sequentially rather than through the conversationState machinery used by
// the four config flows in --serve mode.
//
// Every step past chat ID detection is skipped if its field is already
// set in the loaded config — matching setup.iss's manual-path ShouldSkipPage
// gating exactly (SheetAlreadySet/ScreenNameAlreadySet/HandHistoryAlreadySet/
// TournamentAlreadySet/CredentialsAlreadySet there). A rerun over a complete
// install then does nothing but the chat handshake and a "still all set"
// confirmation. Chat ID detection itself is never skipped — it's not a
// question being asked, it's how this process learns who to talk to at all
// for this run, regardless of whether a chat ID was already known.
func RunSetup(token, configPath, serviceAccountPath string) {
	tgBot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		slog.Error("starting telegram bot", "err", err)
		return
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := tgBot.GetUpdatesChan(u)

	chatID := waitForFirstMessage(updates)

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		return
	}
	_, credentialsErr := os.Stat(serviceAccountPath)
	isUpgrade := cfg.Setup.StorageBackend != nil || cfg.Setup.GoogleSheetName != nil || cfg.Setup.GoogleSheetID != nil ||
		cfg.Setup.PokerStarsScreenName != nil || cfg.Paths.HandHistory != nil ||
		cfg.Paths.TournamentSummary != nil || credentialsErr == nil

	chatIDStr := strconv.FormatInt(chatID, 10)
	cfg.Setup.TelegramChatID = &chatIDStr
	if err := config.SaveConfig(configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		return
	}
	if isUpgrade {
		send(tgBot, chatID, "Welcome back! I found your existing configuration — I'll only ask about what's missing.")
	} else {
		send(tgBot, chatID, "Hi! I'm your Poker Tracker bot. Let's get you set up.")
	}

	// Step 1.5 — storage backend. Skipped if already set. Gates every Google
	// step below (credentials, sheet) — a sqlite install must never need
	// Google credentials on disk, so this has to be decided before either
	// of those runs.
	var backend string
	if cfg.Setup.StorageBackend != nil {
		backend = *cfg.Setup.StorageBackend
		send(tgBot, chatID, "✅ Keeping existing storage backend.")
	} else {
		markup := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Google Sheets", "storage:sheets"),
				tgbotapi.NewInlineKeyboardButtonData("Local file", "storage:sqlite"),
			),
		)
		sendWithKeyboard(tgBot, chatID, "How do you want to store your data?", markup)
		backend = strings.TrimPrefix(waitForCallbackChoice(tgBot, updates, chatID, "storage:sheets", "storage:sqlite"), "storage:")
		cfg.Setup.StorageBackend = &backend
		if err := config.SaveConfig(configPath, cfg); err != nil {
			slog.Error("saving config", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}
		send(tgBot, chatID, "✅ Storage backend saved.")
	}

	// Step 1.6 — Google service account credentials. Sheets-only — a sqlite
	// install has nothing to authenticate to. Skipped entirely if the file
	// already exists at serviceAccountPath — the installer's manual-setup
	// path already places this file before --setup ever runs, and on a
	// rerun there's no reason to ask again for a file that's already
	// correct on disk. "skip" is still accepted as a manual fallback for
	// --setup run standalone.
	if backend == "sqlite" {
		// no-op: sqlite installs skip every Google step below
	} else if credentialsErr == nil {
		send(tgBot, chatID, "✅ Keeping existing credentials file.")
	} else {
		send(tgBot, chatID, "Send your google_service_account.json file now as an attachment, or type skip if it's already in place.")
		for {
			text, fileID := readReplyOrDocument(tgBot, updates, chatID)
			if fileID != "" {
				if err := downloadTelegramFile(tgBot, fileID, serviceAccountPath); err != nil {
					slog.Error("downloading credentials file", "err", err)
					send(tgBot, chatID, "Couldn't download that file. Try again, or type skip.")
					continue
				}
				send(tgBot, chatID, "✅ Credentials file saved.")
				break
			}
			if strings.EqualFold(strings.TrimSpace(text), "skip") {
				if _, err := os.Stat(serviceAccountPath); err != nil {
					send(tgBot, chatID, "No credentials file found at the expected location yet. Send it as an attachment now, or place it correctly before typing skip again.")
					continue
				}
				break
			}
			send(tgBot, chatID, "Send the file as an attachment, or type skip.")
		}
	}

	// Step 2 — Google Sheet. Sheets-only. Skipped if either field is already
	// set (only one of them ever is — see BuildConfigJSON's mirror of this
	// in setup.iss). Accepts either a pasted Sheets URL (ID is already
	// known, no Drive lookup needed) or a sheet name (resolved via the
	// Drive API) — see resolveSheetInput.
	if backend == "sqlite" {
		// no-op: sqlite installs have no Google Sheet
	} else if cfg.Setup.GoogleSheetName != nil || cfg.Setup.GoogleSheetID != nil {
		send(tgBot, chatID, "✅ Keeping existing Google Sheet.")
	} else {
		send(tgBot, chatID, "What is the name of your Google Sheet, or paste its full URL?\n(Create a blank sheet and share it with the service account before answering)")
		for {
			reply := readReply(tgBot, updates, chatID)
			if looksLikeCommand(reply) {
				send(tgBot, chatID, "Commands aren't available until setup finishes — send your Google Sheet name or paste its URL to continue.")
				continue
			}
			name, id, err := resolveSheetInput(serviceAccountPath, reply)
			if err != nil {
				send(tgBot, chatID, fmt.Sprintf("%v\nTry again — send the sheet name or its URL.", err))
				continue
			}

			if _, err := sheets.NewClient(serviceAccountPath, id); err != nil {
				send(tgBot, chatID, fmt.Sprintf("Couldn't connect to that sheet: %v\nMake sure it's shared with the service account (Editor access), then try again.", err))
				continue
			}

			cfg.Setup.GoogleSheetName = name
			cfg.Setup.GoogleSheetID = &id
			if err := config.SaveConfig(configPath, cfg); err != nil {
				slog.Error("saving config", "err", err)
				send(tgBot, chatID, "Something went wrong. Check logs.")
				return
			}
			send(tgBot, chatID, "✅ Connected to sheet.")
			break
		}
	}

	// Step 3 — PokerStars screen name. Skipped if already set, but
	// screenName is still needed below (Steps 4/5 join it onto the hand
	// history / tournament summary paths) regardless of which branch set it.
	var screenName string
	if cfg.Setup.PokerStarsScreenName != nil {
		screenName = *cfg.Setup.PokerStarsScreenName
		send(tgBot, chatID, "✅ Keeping existing screen name.")
	} else {
		send(tgBot, chatID, "What is your PokerStars screen name? (Exact capitalisation matters)")
		for {
			screenName = strings.TrimSpace(readReply(tgBot, updates, chatID))
			if looksLikeCommand(screenName) {
				send(tgBot, chatID, "Commands aren't available until setup finishes — send your exact PokerStars screen name to continue.")
				continue
			}
			if screenName != "" {
				break
			}
			send(tgBot, chatID, "Screen name can't be empty. Send your exact PokerStars screen name.")
		}
		cfg.Setup.PokerStarsScreenName = &screenName
		if err := config.SaveConfig(configPath, cfg); err != nil {
			slog.Error("saving config", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}
		send(tgBot, chatID, "✅ Screen name saved.")
	}

	// Step 4 — Hand history path. Skipped if already set.
	if cfg.Paths.HandHistory != nil {
		send(tgBot, chatID, "✅ Keeping existing Hand History folder.")
	} else {
		handHistory := setupResolvePath(tgBot, updates, chatID, "Hand History", `AppData\Local\PokerStars\HandHistory`, screenName)
		cfg.Paths.HandHistory = &handHistory
		if err := config.SaveConfig(configPath, cfg); err != nil {
			slog.Error("saving config", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}
	}

	// Step 5 — Tournament summary path. Skipped if already set.
	if cfg.Paths.TournamentSummary != nil {
		send(tgBot, chatID, "✅ Keeping existing Tournament Summary folder.")
	} else {
		tournamentSummary := setupResolvePath(tgBot, updates, chatID, "Tournament Summary", `AppData\Local\PokerStars\TournamentSummaries`, screenName)
		cfg.Paths.TournamentSummary = &tournamentSummary
		if err := config.SaveConfig(configPath, cfg); err != nil {
			slog.Error("saving config", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}
	}

	// Step 6 — Complete.
	send(tgBot, chatID, "✅ Setup complete! Poker Tracker will start automatically on next login.\nSend /start to configure alerts and hard stop rules.")
}

// EnsureSheetID guarantees Setup.GoogleSheetID is set before --serve
// continues starting up, self-healing the same way ensureScreenName does
// for pokerstars_screen_name rather than hard-panicking. If it's already
// configured, this returns immediately without touching Telegram at all —
// the normal case once --setup has run.
//
// Otherwise it opens its own bot connection (bot.Run and its watcher/worker
// goroutines can't start yet without a sheets client, so nothing else can
// ask through Telegram at this point in startup) and blocks until a working
// sheet ID is saved, asking through whichever chat is already configured —
// or the first chat to message in if even that's unset. The connection is
// torn down via StopReceivingUpdates before returning, so it doesn't keep
// polling Telegram alongside the fresh connection bot.Run opens right after.
func EnsureSheetID(token, configPath, serviceAccountPath string) (string, error) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return "", fmt.Errorf("loading config: %w", err)
	}
	if cfg.Setup.GoogleSheetID != nil {
		return *cfg.Setup.GoogleSheetID, nil
	}

	// A name may already be sitting here from a step that couldn't
	// resolve it itself — setup.iss's manual-install path collects a
	// sheet name via a native dialog, but has no Drive API access from
	// Pascal Script to turn it into an ID, so it saves the name alone and
	// leaves the ID null for this function to finish later. Try it before
	// ever bothering Telegram: if it resolves and the sheet is reachable,
	// this returns immediately with zero user interaction, instead of
	// silently discarding something already provided and asking again
	// from scratch.
	if cfg.Setup.GoogleSheetName != nil {
		name, id, err := resolveSheetInput(serviceAccountPath, *cfg.Setup.GoogleSheetName)
		if err == nil {
			_, clientErr := sheets.NewClient(serviceAccountPath, id)
			if clientErr == nil {
				cfg.Setup.GoogleSheetName = name
				cfg.Setup.GoogleSheetID = &id
				if err := config.SaveConfig(configPath, cfg); err != nil {
					return "", fmt.Errorf("saving config: %w", err)
				}
				return id, nil
			}
		}
	}

	tgBot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return "", fmt.Errorf("starting telegram bot: %w", err)
	}
	defer tgBot.StopReceivingUpdates()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := tgBot.GetUpdatesChan(u)

	var chatID int64
	if cfg.Setup.TelegramChatID != nil {
		chatID, err = strconv.ParseInt(*cfg.Setup.TelegramChatID, 10, 64)
		if err != nil {
			return "", fmt.Errorf("parsing telegram chat id: %w", err)
		}
	} else {
		chatID = waitForFirstMessage(updates)
		chatIDStr := strconv.FormatInt(chatID, 10)
		cfg.Setup.TelegramChatID = &chatIDStr
		if err := config.SaveConfig(configPath, cfg); err != nil {
			return "", fmt.Errorf("saving config: %w", err)
		}
	}

	send(tgBot, chatID, "Your Google Sheet isn't configured. Send the sheet name or paste its full URL to continue.\n(Create a blank sheet and share it with the service account before answering)")
	for {
		reply := readReply(tgBot, updates, chatID)
		if looksLikeCommand(reply) {
			send(tgBot, chatID, "Commands aren't available until the sheet is configured — send your Google Sheet name or paste its URL to continue.")
			continue
		}
		name, id, err := resolveSheetInput(serviceAccountPath, reply)
		if err != nil {
			send(tgBot, chatID, fmt.Sprintf("%v\nTry again — send the sheet name or its URL.", err))
			continue
		}
		_, clientErr := sheets.NewClient(serviceAccountPath, id)
		if clientErr != nil {
			send(tgBot, chatID, fmt.Sprintf("Couldn't connect to that sheet: %v\nMake sure it's shared with the service account (Editor access), then try again.", clientErr))
			continue
		}

		cfg.Setup.GoogleSheetName = name
		cfg.Setup.GoogleSheetID = &id
		if err := config.SaveConfig(configPath, cfg); err != nil {
			return "", fmt.Errorf("saving config: %w", err)
		}
		send(tgBot, chatID, "✅ Connected to sheet.")
		return id, nil
	}
}

// waitForFirstMessage blocks until the first text message arrives on
// updates and returns its sender's chat ID.
func waitForFirstMessage(updates tgbotapi.UpdatesChannel) int64 {
	for update := range updates {
		if update.Message == nil || update.Message.Text == "" {
			continue
		}
		return update.Message.Chat.ID
	}
	return 0
}

// readReply blocks until the next text message from chatID arrives on
// updates, ignoring messages from any other chat.
func readReply(tgBot *tgbotapi.BotAPI, updates tgbotapi.UpdatesChannel, chatID int64) string {
	for update := range updates {
		if update.Message == nil || update.Message.Chat.ID != chatID || update.Message.Text == "" {
			continue
		}
		return update.Message.Text
	}
	return ""
}

// waitForCallbackChoice blocks until a callback query from chatID arrives
// with Data matching one of accepted, answers it, and returns the matched
// value. Used by RunSetup's storage backend prompt, the only inline keyboard
// RunSetup shows — RunSetup runs sequentially before the bot.Run/pollUpdates
// callback machinery exists, so it needs its own blocking wait here rather
// than conversationState.handleCallback.
func waitForCallbackChoice(tgBot *tgbotapi.BotAPI, updates tgbotapi.UpdatesChannel, chatID int64, accepted ...string) string {
	for update := range updates {
		if update.CallbackQuery == nil || update.CallbackQuery.Message == nil || update.CallbackQuery.Message.Chat.ID != chatID {
			continue
		}
		for _, a := range accepted {
			if update.CallbackQuery.Data == a {
				if _, err := tgBot.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, "")); err != nil {
					slog.Error("answering callback query", "err", err)
				}
				return a
			}
		}
	}
	return ""
}

// readReplyOrDocument blocks until the next message from chatID that is
// either a document (returns its file ID, text empty) or non-empty text
// (returns the text, fileID empty). Used only where a reply may
// legitimately be either — the optional credentials-upload step in
// RunSetup, and the /credentials command's document upload.
func readReplyOrDocument(tgBot *tgbotapi.BotAPI, updates tgbotapi.UpdatesChannel, chatID int64) (text, fileID string) {
	for update := range updates {
		if update.Message == nil || update.Message.Chat.ID != chatID {
			continue
		}
		if update.Message.Document != nil {
			return "", update.Message.Document.FileID
		}
		if update.Message.Text != "" {
			return update.Message.Text, ""
		}
	}
	return "", ""
}

// downloadTelegramFile downloads the Telegram-hosted file identified by
// fileID and writes it to destPath, overwriting any existing file there.
func downloadTelegramFile(tgBot *tgbotapi.BotAPI, fileID, destPath string) error {
	url, err := tgBot.GetFileDirectURL(fileID)
	if err != nil {
		return fmt.Errorf("getting file url: %w", err)
	}
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("downloading file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading file: unexpected status %s", resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading downloaded file: %w", err)
	}
	if err := os.WriteFile(destPath, data, 0644); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}
	return nil
}

// sheetURLPattern matches a Google Sheets URL and captures its spreadsheet ID.
var sheetURLPattern = regexp.MustCompile(`docs\.google\.com/spreadsheets/d/([a-zA-Z0-9-_]+)`)

// extractSheetIDFromURL returns the spreadsheet ID embedded in a pasted
// Google Sheets URL, and whether input looked like one at all.
func extractSheetIDFromURL(input string) (id string, ok bool) {
	m := sheetURLPattern.FindStringSubmatch(input)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// looksLikeCommand reports whether reply is a Telegram slash command
// rather than an actual answer — sent reflexively (habit, or just
// checking whether the bot is alive) while a setup prompt is blocking on
// free text. RunSetup and EnsureSheetID's blocking prompts have no
// command dispatch at all yet (the real command loop, bot.Run, doesn't
// exist until setup finishes), so without this check a stray "/status"
// either gets silently accepted as if it were the real answer (e.g.
// saved as the screen name) or run through a lookup as if it were real
// data (e.g. resolveSheetInput reporting a confusing "spreadsheet
// '/status' not found" instead of a clear one).
func looksLikeCommand(reply string) bool {
	return strings.HasPrefix(strings.TrimSpace(reply), "/")
}

// resolveSheetInput accepts either a pasted Google Sheets URL or a sheet
// name typed during setup. A URL's ID is already known, so it's used
// directly with no Drive API lookup and no resolved name to save — a name
// is resolved to an ID via sheets.ResolveSheetID (the Drive API).
func resolveSheetInput(serviceAccountPath, input string) (name *string, id string, err error) {
	input = strings.TrimSpace(input)
	if sheetID, ok := extractSheetIDFromURL(input); ok {
		return nil, sheetID, nil
	}
	id, err = sheets.ResolveSheetID(serviceAccountPath, input)
	if err != nil {
		return nil, "", err
	}
	return &input, id, nil
}

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
// the already-running sheetsClient passed throughout bot.Run/runWorker for
// the rest of this process's life (sheetsClient is a required non-nil
// dependency threaded through goroutines that are already running, and
// hot-swapping it would need a much larger refactor for what's a rare,
// deliberate action). Instead, once the new sheet ID is saved this process
// kills itself and restarts the "Poker Tracker" scheduled task via a
// detached command, so the fresh process picks up the new sheet ID from
// config.json on startup.
func handleSheetCommand(tgBot *tgbotapi.BotAPI, chatID int64, args string, sc storage.Store, configPath, serviceAccountPath string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	if cfg.Setup.StorageBackend != nil && *cfg.Setup.StorageBackend == "sqlite" {
		send(tgBot, chatID, "This install uses local storage — there is no Google Sheet. Use /export to get your data as CSV.")
		return
	}

	args = strings.TrimSpace(args)
	if args == "" {
		send(tgBot, chatID, sc.GetSheetURL())
		return
	}

	name, id, err := resolveSheetInput(serviceAccountPath, args)
	if err != nil {
		send(tgBot, chatID, fmt.Sprintf("Couldn't find that sheet: %v", err))
		return
	}
	if _, err := sheets.NewClient(serviceAccountPath, id); err != nil {
		send(tgBot, chatID, fmt.Sprintf("Couldn't connect to that sheet: %v\nMake sure it's shared with the service account (Editor access), then try again.", err))
		return
	}

	cfg.Setup.GoogleSheetName = name
	cfg.Setup.GoogleSheetID = &id
	if err := config.SaveConfig(configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}

	send(tgBot, chatID, "✅ Sheet updated. Restarting tracker in a moment...")

	if err := selfRestart(configPath); err != nil {
		// A live process on stale config beats a dead one — don't exit if
		// the restart helper never got off the ground.
		slog.Error("restarting tracker after sheet switch", "err", err)
		send(tgBot, chatID, "Couldn't restart automatically — restart the tracker service by hand (Task Scheduler → Poker Tracker → Run, or reboot).")
		return
	}
	os.Exit(0)
}

// --- /export ---

var (
	cashSessionsCSVHeader   = []string{"Date", "Stake", "Hands", "Duration (min)", "Net (USD)", "Rake (USD)", "BB/100", "VPIP", "PFR", "Session ID"}
	tournamentsCSVHeader    = []string{"Date", "Type", "Tournament ID", "Buy-In (USD)", "Prize Pool (USD)", "Multiplier", "Finish Position", "Total Players", "Net (USD)", "VPIP", "PFR", "Bounty Won (USD)", "Prize Won (USD)", "Hands"}
	bankrollLedgerCSVHeader = []string{"Date", "Type", "Amount (USD)", "Running Total (USD)", "Session ID"}
	timeAnalysisCSVHeader   = []string{"Date", "Hour", "Day of Week", "Net (USD)", "Session ID"}
)

// fullPrecision tells formatFloat/formatFloatPtr to render the shortest
// string that round-trips the value exactly, rather than a fixed decimal
// count — pass this for every numeric CSV column except VPIP/PFR.
const fullPrecision = -1

// vpipPfrPrecision is the fixed decimal count VPIP/PFR are rounded to.
// Both are raw-division results (e.g. 25.36231884057971) with no observable
// meaning beyond a couple of decimal places, unlike every other numeric CSV
// column, which keeps full precision.
const vpipPfrPrecision = 2

// moneyPrecision is the fixed decimal count every USD-denominated CSV
// column is rounded to, matching how Telegram already displays USD amounts
// (formatSignedUSD's "$%.2f") — chained float64 arithmetic (running totals,
// bounty folding, ...) otherwise leaves values like -0.32000000000000006
// with no observable meaning beyond a cent.
const moneyPrecision = 2

// bb100Precision is the fixed decimal count BB/100 is rounded to in CSV
// output, matching how Telegram already displays it ("%.1f" in
// cashStatsMessage, buildWeeklySummary, buildMonthlySummary).
const bb100Precision = 1

// formatFloat renders v to prec decimal places (fullPrecision for the
// shortest exact round-trip, a fixed count to round off meaningless
// precision).
func formatFloat(v float64, prec int) string {
	return strconv.FormatFloat(v, 'f', prec, 64)
}

// formatFloatPtr is formatFloat for a possibly-nil pointer (e.g. an MTT's
// Multiplier, or a tournament's VPIP/PFR before its hand file has arrived),
// rendering an empty cell for nil rather than a misleading "0".
func formatFloatPtr(v *float64, prec int) string {
	if v == nil {
		return ""
	}
	return formatFloat(*v, prec)
}

// formatIntPtr renders an empty cell for nil (a tournament's Hands, before
// its hand file has arrived) rather than a misleading "0".
func formatIntPtr(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

// prizePoolCSVCell coalesces Tournament's two mutually exclusive prize pool
// fields into the single cell the sheet itself stores — description text for
// a non-cash-denominated pool, otherwise the numeric amount. Without this,
// /export would either emit two columns or silently drop one, breaking the
// byte-identical output CLAUDE.md requires between backends (SQLite splits
// this into two nullable columns internally; the sheet never did).
func prizePoolCSVCell(t parsers.Tournament) string {
	if t.PrizePoolDescription != "" {
		return t.PrizePoolDescription
	}
	return formatFloat(t.PrizePoolUSD, moneyPrecision)
}

func cashSessionsCSVRows(sessions []storage.CashSessionRecord) [][]string {
	rows := make([][]string, 0, len(sessions))
	for _, s := range sessions {
		rows = append(rows, []string{
			s.Date.UTC().Format(time.RFC3339),
			s.Stake,
			strconv.Itoa(s.Hands),
			strconv.Itoa(s.DurationMin),
			formatFloat(s.NetUSD, moneyPrecision),
			formatFloat(s.RakeUSD, moneyPrecision),
			formatFloat(s.BB100, bb100Precision),
			formatFloat(s.VPIP, vpipPfrPrecision),
			formatFloat(s.PFR, vpipPfrPrecision),
			s.SessionID,
		})
	}
	return rows
}

func tournamentsCSVRows(tournaments []storage.TournamentRecord) [][]string {
	rows := make([][]string, 0, len(tournaments))
	for _, t := range tournaments {
		rows = append(rows, []string{
			t.Date.UTC().Format(time.RFC3339),
			t.Type,
			t.TournamentID,
			formatFloat(t.BuyInUSD, moneyPrecision),
			prizePoolCSVCell(t.Tournament),
			formatFloatPtr(t.Multiplier, fullPrecision),
			strconv.Itoa(t.FinishPos),
			strconv.Itoa(t.TotalPlayers),
			formatFloat(t.NetUSD, moneyPrecision),
			formatFloatPtr(t.VPIP, vpipPfrPrecision),
			formatFloatPtr(t.PFR, vpipPfrPrecision),
			formatFloat(t.BountyWonUSD, moneyPrecision),
			formatFloat(t.PrizeWonUSD, moneyPrecision),
			formatIntPtr(t.Hands),
		})
	}
	return rows
}

func bankrollLedgerCSVRows(entries []storage.LedgerEntry) [][]string {
	rows := make([][]string, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, []string{
			e.Date.UTC().Format(time.RFC3339),
			e.Type,
			formatFloat(e.AmountUSD, moneyPrecision),
			formatFloat(e.RunningTotal, moneyPrecision),
			e.SessionID,
		})
	}
	return rows
}

func timeAnalysisCSVRows(entries []storage.TimeAnalysisRecord) [][]string {
	rows := make([][]string, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, []string{
			e.Date.UTC().Format(time.RFC3339),
			strconv.Itoa(e.Hour),
			e.DayOfWeek,
			formatFloat(e.NetUSD, moneyPrecision),
			e.SessionID,
		})
	}
	return rows
}

// writeCSVFile writes header followed by rows to path, using encoding/csv.
func writeCSVFile(path string, header []string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		return fmt.Errorf("writing header to %s: %w", path, err)
	}
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			return fmt.Errorf("writing row to %s: %w", path, err)
		}
	}
	w.Flush()
	return w.Error()
}

// handleExportCommand implements /export: queries every row of all four data
// sets via storage.Store — never a backend-specific type, so the command is
// identical on both backends — writes each to a CSV file in os.TempDir(),
// sends each as a Telegram document, then deletes the temp files. An empty
// data set still sends a header-only CSV. Dates are RFC3339 UTC on both
// backends: the sqlite backend already stores them that way, and the sheets
// backend's Date field is parsed back from its internal
// "2006-01-02 15:04" format by GetAllCashSessions/GetAllTournaments/
// GetAllTimeAnalysis before ever reaching here, so formatting with
// time.RFC3339 here is enough to keep output byte-identical between
// backends.
func handleExportCommand(tgBot *tgbotapi.BotAPI, chatID int64, sc storage.Store) {
	cashSessions, err := sc.GetAllCashSessions()
	if err != nil {
		slog.Error("getting cash sessions for export", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	tournaments, err := sc.GetAllTournaments()
	if err != nil {
		slog.Error("getting tournaments for export", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	ledger, err := sc.GetLedgerEntriesSince(time.Time{})
	if err != nil {
		slog.Error("getting ledger entries for export", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	timeAnalysis, err := sc.GetAllTimeAnalysis()
	if err != nil {
		slog.Error("getting time analysis for export", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}

	exports := []struct {
		filename string
		header   []string
		rows     [][]string
	}{
		{"cash_sessions.csv", cashSessionsCSVHeader, cashSessionsCSVRows(cashSessions)},
		{"tournaments.csv", tournamentsCSVHeader, tournamentsCSVRows(tournaments)},
		{"bankroll_ledger.csv", bankrollLedgerCSVHeader, bankrollLedgerCSVRows(ledger)},
		{"time_analysis.csv", timeAnalysisCSVHeader, timeAnalysisCSVRows(timeAnalysis)},
	}

	var paths []string
	for _, e := range exports {
		path := filepath.Join(os.TempDir(), e.filename)
		if err := writeCSVFile(path, e.header, e.rows); err != nil {
			slog.Error("writing export csv", "file", e.filename, "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			for _, p := range paths {
				os.Remove(p)
			}
			return
		}
		paths = append(paths, path)
	}

	for _, p := range paths {
		if _, err := tgBot.Send(tgbotapi.NewDocument(chatID, tgbotapi.FilePath(p))); err != nil {
			slog.Error("sending export file", "file", p, "err", err)
		}
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			slog.Error("removing export temp file", "file", p, "err", err)
		}
	}

	send(tgBot, chatID, "✅ Export complete — 4 files.")
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
func handleScreenNameCommand(tgBot *tgbotapi.BotAPI, chatID int64, args, configPath string, restartWatcher func()) {
	args = strings.TrimSpace(args)

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}

	if args == "" {
		if cfg.Setup.PokerStarsScreenName == nil {
			send(tgBot, chatID, "No screen name configured yet. Usage: /screenname <name>")
			return
		}
		send(tgBot, chatID, "Current PokerStars screen name: "+*cfg.Setup.PokerStarsScreenName)
		return
	}

	cfg.Setup.PokerStarsScreenName = &args
	if err := config.SaveConfig(configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}

	restartWatcher()
	send(tgBot, chatID, "✅ Screen name updated to "+args+". Watcher restarted.")
}

// --- /credentials flow ---

// startCredentialsFlow implements /credentials: swaps the Google service
// account JSON used to authenticate to Sheets/Drive. Unlike the four
// config flows (paths/alerts/hardstop/schedule), this one expects a file
// upload rather than a plain-text reply — see handleCredentialsDocument.
// Sheets-only, same gating as /sheet — a sqlite install has no Google
// credentials to swap.
func (s *conversationState) startCredentialsFlow(tgBot *tgbotapi.BotAPI, chatID int64, configPath string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	if cfg.Setup.StorageBackend != nil && *cfg.Setup.StorageBackend == "sqlite" {
		send(tgBot, chatID, "This install uses local storage — there is no Google Sheet. Use /export to get your data as CSV.")
		return
	}

	s.flow = "credentials"
	s.field = "file"
	send(tgBot, chatID, "Send your google_service_account.json file now as an attachment, or type skip to keep the current one.")
}

// handleCredentialsTextReply handles a plain-text reply during the
// /credentials flow — the only valid one is "skip"; anything else
// re-prompts, since a real reply here is a file, not text.
func (s *conversationState) handleCredentialsTextReply(tgBot *tgbotapi.BotAPI, chatID int64, text string) {
	if strings.EqualFold(strings.TrimSpace(text), "skip") {
		s.reset()
		send(tgBot, chatID, "Kept the existing credentials file.")
		return
	}
	send(tgBot, chatID, "Send the google_service_account.json file as an attachment, or type skip.")
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
// the already-running sheetsClient isn't affected until the service
// restarts.
func (s *conversationState) handleCredentialsDocument(tgBot *tgbotapi.BotAPI, chatID int64, fileID, configPath, serviceAccountPath string) {
	if err := downloadTelegramFile(tgBot, fileID, serviceAccountPath); err != nil {
		slog.Error("downloading credentials file", "err", err)
		send(tgBot, chatID, "Couldn't download that file. Try again, or type skip.")
		return
	}
	s.reset()

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "✅ Credentials file saved, but something went wrong checking your sheet. Check logs, and run /sheet to reconnect if needed.")
		return
	}

	if cfg.Setup.GoogleSheetID == nil {
		send(tgBot, chatID, "✅ Credentials file saved. Restart the tracker service for this to take effect. No sheet is configured yet — run /sheet to connect one.")
		return
	}

	if _, err := sheets.NewClient(serviceAccountPath, *cfg.Setup.GoogleSheetID); err != nil {
		cfg.Setup.GoogleSheetID = nil
		cfg.Setup.GoogleSheetName = nil
		if saveErr := config.SaveConfig(configPath, cfg); saveErr != nil {
			slog.Error("saving config", "err", saveErr)
		}
		send(tgBot, chatID, "✅ Credentials file saved, but your current sheet isn't accessible with it — it needs to be shared with the new service account, or you're pointing at a different Google Cloud project entirely. Run /sheet to connect a sheet this service account can reach.")
		return
	}

	send(tgBot, chatID, "✅ Credentials file saved. Your existing sheet is still accessible with it. Restart the tracker service for this to take effect.")
}

// setupResolvePath asks for a base PokerStars folder path (relative to the
// player's home directory, or a full path starting with a drive letter),
// validates it by checking the screen-name-joined path exists on disk, and
// loops until a valid one is given. Returns the base path (not joined).
func setupResolvePath(tgBot *tgbotapi.BotAPI, updates tgbotapi.UpdatesChannel, chatID int64, folderLabel, exampleSuffix, screenName string) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		slog.Error("getting user home directory", "err", err)
	}
	send(tgBot, chatID, fmt.Sprintf(
		"What is the base %s folder path?\nYour home directory is: %s\nYou can type just the subfolder from there (e.g. %s)\nor type a full path starting with a drive letter if your folder is elsewhere.\nDo NOT include your screen name at the end — that part is added automatically.",
		folderLabel, homeDir, exampleSuffix))

	for {
		reply := readReply(tgBot, updates, chatID)
		normalized := fixMissingDriveColon(NormalizeWindowsPath(reply))
		full, err := resolveAgainstHomeDir(normalized)
		if err != nil {
			slog.Error("resolving home directory", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			continue
		}

		joined := filepath.Join(full, screenName)
		if !windowsPathExists(joined) {
			send(tgBot, chatID, fmt.Sprintf("That path doesn't exist: %s\nSend the folder path again.", joined))
			continue
		}
		return full
	}
}

// inSummaryWindow reports whether now falls within the 09:00–09:01 local
// trigger window for scheduled summaries.
func inSummaryWindow(now time.Time) bool {
	return now.Hour() == 9 && now.Minute() <= 1
}

// sameDay reports whether a and b fall on the same calendar day.
func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// configuredChatID reads the configured Telegram chat ID from config.json.
// ok is false if no chat ID is configured yet or it cannot be parsed.
func configuredChatID(configPath string) (chatID int64, ok bool) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config for telegram send", "err", err)
		return 0, false
	}
	if cfg.Setup.TelegramChatID == nil {
		return 0, false
	}
	chatID, err = strconv.ParseInt(*cfg.Setup.TelegramChatID, 10, 64)
	if err != nil {
		slog.Error("parsing telegram chat id", "err", err)
		return 0, false
	}
	return chatID, true
}

// send sends text to chatID.
func send(tgBot *tgbotapi.BotAPI, chatID int64, text string) {
	if _, err := tgBot.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		slog.Error("sending telegram message", "err", err)
	}
}

// sendWithKeyboard sends text with an inline keyboard to chatID.
func sendWithKeyboard(tgBot *tgbotapi.BotAPI, chatID int64, text string, markup tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = markup
	if _, err := tgBot.Send(msg); err != nil {
		slog.Error("sending telegram message", "err", err)
	}
}

// sendMessage reads the configured Telegram chat ID and sends text to it.
// If no chat ID is configured yet, the message is logged and discarded.
func sendMessage(tgBot *tgbotapi.BotAPI, configPath, text string) {
	chatID, ok := configuredChatID(configPath)
	if !ok {
		return
	}
	send(tgBot, chatID, text)
}

// sendMessageWithKeyboard is sendMessage with an inline keyboard attached.
func sendMessageWithKeyboard(tgBot *tgbotapi.BotAPI, configPath, text string, markup tgbotapi.InlineKeyboardMarkup) {
	chatID, ok := configuredChatID(configPath)
	if !ok {
		return
	}
	sendWithKeyboard(tgBot, chatID, text, markup)
}

// buildWeeklySummary formats the past 7 days of activity.
func buildWeeklySummary(sc storage.Store, now time.Time) (string, error) {
	since := now.AddDate(0, 0, -7)
	cashSessions, err := sc.GetCashSessionsSince(since)
	if err != nil {
		return "", fmt.Errorf("loading cash sessions: %w", err)
	}
	tournaments, err := sc.GetTournamentsSince(since)
	if err != nil {
		return "", fmt.Errorf("loading tournaments: %w", err)
	}
	// Net is session results only, unlike /status — deposits/withdrawals are
	// excluded so a deposit this week can't turn a losing week into an
	// apparent winning one.
	rawLedger, err := sc.GetLedgerEntriesSince(since)
	if err != nil {
		return "", fmt.Errorf("loading ledger entries: %w", err)
	}
	var netUSD float64
	for _, e := range rawLedger {
		if e.Type == "cash_session" || e.Type == "tournament" {
			netUSD += e.AmountUSD
		}
	}

	var totalHands int
	for _, s := range cashSessions {
		totalHands += s.Hands
	}
	sessionCount := len(cashSessions) + len(tournaments)
	bb100, _, _ := weightedCashAverages(cashSessions)

	return fmt.Sprintf(
		"📊 Weekly Summary\nSessions: %d\nNet: %s\nBB/100: %.1f\nSpin & Go ROI: %s\nMTT ROI: %s\nHands: %s",
		sessionCount,
		formatSignedUSD(netUSD),
		bb100,
		formatSignedPercent(roi(tournaments, "spingo")),
		formatSignedPercent(roi(tournaments, "mtt")),
		commas(totalHands),
	), nil
}

// buildMonthlySummary formats the previous calendar month's activity.
func buildMonthlySummary(sc storage.Store, now time.Time) (string, error) {
	firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	firstOfPrevMonth := firstOfThisMonth.AddDate(0, -1, 0)

	rawCashSessions, err := sc.GetCashSessionsSince(firstOfPrevMonth)
	if err != nil {
		return "", fmt.Errorf("loading cash sessions: %w", err)
	}
	rawTournaments, err := sc.GetTournamentsSince(firstOfPrevMonth)
	if err != nil {
		return "", fmt.Errorf("loading tournaments: %w", err)
	}

	var cashSessions []parsers.CashSession
	for _, s := range rawCashSessions {
		if s.Date.Before(firstOfThisMonth) {
			cashSessions = append(cashSessions, s)
		}
	}
	var tournaments []parsers.Tournament
	for _, t := range rawTournaments {
		if t.Date.Before(firstOfThisMonth) {
			tournaments = append(tournaments, t)
		}
	}

	var cashNet, spinsNet, mttNet float64
	var totalHands int
	for _, s := range cashSessions {
		cashNet += s.NetUSD
		totalHands += s.Hands
	}
	for _, t := range tournaments {
		if t.BaseType() == "spingo" {
			spinsNet += t.NetUSD
		} else {
			mttNet += t.NetUSD
		}
	}
	sessionCount := len(cashSessions) + len(tournaments)

	// Total is session results only, unlike /status — deposits/withdrawals
	// are excluded so a deposit last month can't turn a losing month into an
	// apparent winning one.
	rawLedger, err := sc.GetLedgerEntriesSince(firstOfPrevMonth)
	if err != nil {
		return "", fmt.Errorf("loading ledger entries: %w", err)
	}
	var totalNet float64
	for _, e := range rawLedger {
		if e.Date.Before(firstOfThisMonth) && (e.Type == "cash_session" || e.Type == "tournament") {
			totalNet += e.AmountUSD
		}
	}
	bb100, _, _ := weightedCashAverages(cashSessions)

	return fmt.Sprintf(
		"📅 Monthly Summary — %s\nCash Net: %s\nSpins Net: %s\nMTT Net: %s\nTotal: %s\nBB/100: %.1f\nSpins ROI: %s\nMTT ROI: %s\nHands: %s\nVolume: %d sessions",
		firstOfPrevMonth.Format("January 2006"),
		formatSignedUSD(cashNet),
		formatSignedUSD(spinsNet),
		formatSignedUSD(mttNet),
		formatSignedUSD(totalNet),
		bb100,
		formatSignedPercent(roi(tournaments, "spingo")),
		formatSignedPercent(roi(tournaments, "mtt")),
		commas(totalHands),
		sessionCount,
	), nil
}

// weightedAverages computes the hands-weighted average of each of an item's
// values — a low-hand-count item (a 10-hand cash session, or a tournament
// whose hand file hasn't arrived yet) must not count the same as a
// high-hand-count one. extract returns an item's hand count and its n
// values (aligned across every item), or ok=false to exclude that item
// entirely. One shared weighting loop, reused for both cash sessions
// (hands, BB/100, VPIP, PFR — always known) and tournaments (hands, VPIP,
// PFR — nil, and so excluded, until the hand file arrives).
func weightedAverages[T any](items []T, n int, extract func(T) (hands float64, values []float64, ok bool)) []float64 {
	sums := make([]float64, n)
	var totalHands float64
	for _, item := range items {
		hands, values, ok := extract(item)
		if !ok {
			continue
		}
		totalHands += hands
		for i := 0; i < n; i++ {
			sums[i] += values[i] * hands
		}
	}
	result := make([]float64, n)
	if totalHands == 0 {
		return result
	}
	for i, sum := range sums {
		result[i] = sum / totalHands
	}
	return result
}

// weightedCashAverages returns the hands-weighted average BB/100, VPIP, and
// PFR across cash sessions. Thin wrapper over weightedAverages, kept named
// since it's called from three places (buildWeeklySummary,
// buildMonthlySummary, cashStatsMessage).
func weightedCashAverages(sessions []parsers.CashSession) (bb100, vpip, pfr float64) {
	res := weightedAverages(sessions, 3, func(s parsers.CashSession) (float64, []float64, bool) {
		return float64(s.Hands), []float64{s.BB100, s.VPIP, s.PFR}, true
	})
	return res[0], res[1], res[2]
}

// weightedTournamentAverages returns the hands-weighted average VPIP and PFR
// across tournaments. Thin wrapper over weightedAverages, mirroring
// weightedCashAverages — a tournament whose hand file hasn't arrived yet
// (Hands/VPIP/PFR all nil) is excluded entirely rather than counted as 0.
func weightedTournamentAverages(tournaments []parsers.Tournament) (vpip, pfr float64) {
	res := weightedAverages(tournaments, 2, func(t parsers.Tournament) (float64, []float64, bool) {
		if t.Hands == nil || t.VPIP == nil || t.PFR == nil {
			return 0, nil, false
		}
		return float64(*t.Hands), []float64{*t.VPIP, *t.PFR}, true
	})
	return res[0], res[1]
}

// roi returns net / buy-in as a percentage across tournaments of the given type.
func roi(tournaments []parsers.Tournament, tType string) float64 {
	var netSum, buyinSum float64
	for _, t := range tournaments {
		if t.BaseType() != tType {
			continue
		}
		netSum += t.NetUSD
		buyinSum += t.BuyInUSD
	}
	if buyinSum == 0 {
		return 0
	}
	return netSum / buyinSum * 100
}

func formatSignedUSD(v float64) string {
	sign := "+"
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s$%.2f", sign, v)
}

func formatSignedPercent(v float64) string {
	sign := "+"
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%.0f%%", sign, v)
}

// formatPercent renders v as a plain (unsigned) percentage with 1 decimal
// place, e.g. "24.3%" — used for VPIP/PFR, which are always non-negative,
// unlike formatSignedPercent's ROI use (signed, whole number).
func formatPercent(v float64) string {
	return fmt.Sprintf("%.1f%%", v)
}

// formatPercentPtr is formatPercent for a possibly-nil VPIP/PFR pointer (a
// tournament's, before its hand file has arrived) — renders "N/A" for nil
// rather than a misleading "0.0%".
func formatPercentPtr(v *float64) string {
	if v == nil {
		return "N/A"
	}
	return formatPercent(*v)
}

// commas formats n with thousands separators, e.g. 4201 -> "4,201".
func commas(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// ordinal formats n as "1st", "2nd", "3rd", "4th", etc.
func ordinal(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return fmt.Sprintf("%dth", n)
	}
	switch n % 10 {
	case 1:
		return fmt.Sprintf("%dst", n)
	case 2:
		return fmt.Sprintf("%dnd", n)
	case 3:
		return fmt.Sprintf("%drd", n)
	default:
		return fmt.Sprintf("%dth", n)
	}
}

// checkPendingTournaments re-sends the satellite/ticket confirmation menu for
// any pending entry more than 30 minutes past its last reminder.
func checkPendingTournaments(tgBot *tgbotapi.BotAPI, configPath string, now time.Time) {
	pendingPath := filepath.Join(filepath.Dir(configPath), "pending_tournaments.json")
	pending, err := config.LoadPendingTournaments(pendingPath)
	if err != nil {
		slog.Error("loading pending tournaments", "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	changed := false
	for i := range pending {
		if now.Sub(pending[i].LastRemindedAt) <= 30*time.Minute {
			continue
		}
		sendSatelliteMenu(tgBot, configPath, pending[i].Tournament)
		pending[i].LastRemindedAt = now
		changed = true
	}
	if changed {
		if err := config.SavePendingTournaments(pendingPath, pending); err != nil {
			slog.Error("saving pending tournaments", "err", err)
		}
	}
}

// sendSatelliteMenu asks the cash-vs-ticket entry question. The prize shown
// isn't always finish position 1's — WinnerPrizeDescription is set from
// whichever row's payout first read as a ticket/pass, scanning every result
// row rather than just position 1 (which can still read "still playing" if
// the summary was captured while other players were still in) — so it's
// labeled generically rather than claiming it's always what 1st place won.
// Includes buy-in, date, and finish position/field size — enough for the
// player to tell tournaments apart when more than one is pending at once,
// since the identifying detail here is otherwise just a TournamentID.
func sendSatelliteMenu(tgBot *tgbotapi.BotAPI, configPath string, t parsers.Tournament) {
	gameType := "MTT"
	if t.BaseType() == "spingo" {
		gameType = "Spin & Go"
	}
	text := fmt.Sprintf("🎫 Satellite/qualifier detected\n%s #%s\nDate: %s\nBuy-in: $%.2f\nFinish: %s / %d\nPrize: %s\nDid you pay cash for your entry, or use a ticket?",
		gameType, t.TournamentID, t.Date.Local().Format("2006-01-02 15:04"), t.BuyInUSD, ordinal(t.FinishPos), t.TotalPlayers, t.WinnerPrizeDescription)
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💵 Paid cash", "satellite_cash:"+t.TournamentID),
			tgbotapi.NewInlineKeyboardButtonData("🎫 Used a ticket", "satellite_ticket:"+t.TournamentID),
		),
	)
	sendMessageWithKeyboard(tgBot, configPath, text, markup)
}

// conversationState tracks the in-progress inline-keyboard flow, if any, for
// the single configured chat. It lives entirely inside the update-processing
// goroutine (pollUpdates), so no locking is needed.
type conversationState struct {
	flow  string // "", "screen_name", "paths", "alerts", "hardstop", "schedule", "credentials"
	field string // field awaiting a text reply within flow, "" if none

	// pathsFirstTimeSetup is captured when the /paths flow starts, recording
	// whether both paths were null at that point — used to pick between the
	// "watcher restarted" and "tracker now active" messages on Done.
	pathsFirstTimeSetup bool

	// startWizard is true while /start is chaining paths -> alerts ->
	// hardstop -> schedule. Each flow's Done handler checks it to decide
	// whether to auto-advance into the next flow instead of just stopping.
	// reset() doesn't touch it, so it survives across the four flows.
	startWizard bool

	// resumeFlow names the flow (see ensureScreenName) to start once the
	// screen-name recovery prompt is answered.
	resumeFlow string
}

func (s *conversationState) reset() {
	s.flow = ""
	s.field = ""
}

// pollUpdates polls Telegram for incoming messages and callback queries and
// dispatches them. Runs in its own goroutine so it never blocks alertChan.
func pollUpdates(tgBot *tgbotapi.BotAPI, sc storage.Store, configPath, lockfilePath, serviceAccountPath string, restartWatcher func()) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := tgBot.GetUpdatesChan(u)

	state := &conversationState{}
	for update := range updates {
		// CallbackQuery.Message can be nil if the tapped button is on a
		// message Telegram no longer has (old or deleted). Every downstream
		// handler assumes a non-nil Message, so filter here before any of
		// them run — otherwise this goroutine panics and silently kills all
		// incoming command handling until the service is restarted.
		if update.CallbackQuery != nil && update.CallbackQuery.Message == nil {
			if _, err := tgBot.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, "Already resolved.")); err != nil {
				slog.Error("answering callback query", "err", err)
			}
			continue
		}
		state.handleUpdate(tgBot, update, sc, configPath, lockfilePath, serviceAccountPath, restartWatcher)
	}
}

func (s *conversationState) handleUpdate(tgBot *tgbotapi.BotAPI, update tgbotapi.Update, sc storage.Store, configPath, lockfilePath, serviceAccountPath string, restartWatcher func()) {
	var senderChatID int64
	switch {
	case update.Message != nil:
		senderChatID = update.Message.Chat.ID
	case update.CallbackQuery != nil:
		senderChatID = update.CallbackQuery.Message.Chat.ID
	default:
		return
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config for incoming update", "err", err)
		return
	}
	senderChatIDStr := strconv.FormatInt(senderChatID, 10)
	if cfg.Setup.TelegramChatID == nil {
		cfg.Setup.TelegramChatID = &senderChatIDStr
		if err := config.SaveConfig(configPath, cfg); err != nil {
			slog.Error("saving telegram chat id", "err", err)
			return
		}
	} else if *cfg.Setup.TelegramChatID != senderChatIDStr {
		return // not the configured chat — ignore silently
	}

	if update.CallbackQuery != nil {
		s.handleCallback(tgBot, update.CallbackQuery, sc, configPath, lockfilePath, restartWatcher)
		return
	}

	msg := update.Message

	// A document upload has no Text — the check below would otherwise
	// silently drop it. This is the only flow that ever expects a file
	// instead of a text reply.
	if s.flow == "credentials" && s.field == "file" && msg.Document != nil {
		s.handleCredentialsDocument(tgBot, msg.Chat.ID, msg.Document.FileID, configPath, serviceAccountPath)
		return
	}

	if msg.Text == "" {
		return
	}

	if s.field != "" && !strings.HasPrefix(msg.Text, "/") {
		s.handleFlowReply(tgBot, msg, configPath)
		return
	}

	if strings.HasPrefix(msg.Text, "/") {
		s.handleCommand(tgBot, msg, sc, configPath, lockfilePath, serviceAccountPath, restartWatcher)
	}
}

func (s *conversationState) handleFlowReply(tgBot *tgbotapi.BotAPI, msg *tgbotapi.Message, configPath string) {
	switch s.flow {
	case "screen_name":
		s.handleScreenNameReply(tgBot, msg.Chat.ID, msg.Text, configPath)
	case "paths":
		s.handlePathsReply(tgBot, msg.Chat.ID, msg.Text, configPath)
	case "alerts":
		s.handleAlertsReply(tgBot, msg.Chat.ID, msg.Text, configPath)
	case "hardstop":
		s.handleHardstopReply(tgBot, msg.Chat.ID, msg.Text, configPath)
	case "schedule":
		s.handleScheduleReply(tgBot, msg.Chat.ID, msg.Text, configPath)
	case "credentials":
		s.handleCredentialsTextReply(tgBot, msg.Chat.ID, msg.Text)
	}
}

func (s *conversationState) handleCallback(tgBot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery, sc storage.Store, configPath, lockfilePath string, restartWatcher func()) {
	if _, err := tgBot.Request(tgbotapi.NewCallback(cb.ID, "")); err != nil {
		slog.Error("answering callback query", "err", err)
	}

	chatID := cb.Message.Chat.ID
	messageID := cb.Message.MessageID
	data := cb.Data

	switch {
	case strings.HasPrefix(data, "satellite_cash:"):
		resolveSatellite(tgBot, chatID, messageID, strings.TrimPrefix(data, "satellite_cash:"), true, sc, configPath)
	case strings.HasPrefix(data, "satellite_ticket:"):
		resolveSatellite(tgBot, chatID, messageID, strings.TrimPrefix(data, "satellite_ticket:"), false, sc, configPath)
	case strings.HasPrefix(data, "paths:"):
		s.handlePathsCallback(tgBot, chatID, strings.TrimPrefix(data, "paths:"), configPath, restartWatcher)
	case strings.HasPrefix(data, "alerts:"):
		s.handleAlertsCallback(tgBot, chatID, strings.TrimPrefix(data, "alerts:"), configPath)
	case strings.HasPrefix(data, "hardstop:"):
		s.handleHardstopCallback(tgBot, chatID, strings.TrimPrefix(data, "hardstop:"), configPath)
	case strings.HasPrefix(data, "schedule:"):
		s.handleScheduleCallback(tgBot, chatID, strings.TrimPrefix(data, "schedule:"))
	}
}

// RecoverTicketPayout computes the corrected BuyInUSD/NetUSD for a
// LikelySatellite tournament once the player confirms they paid with a
// ticket rather than cash. NetUSD was originally computed as payout minus
// buy-in, so adding them back recovers the payout before zeroing the buy-in.
func RecoverTicketPayout(buyInUSD, netUSD float64) (newBuyInUSD, newNetUSD float64) {
	return 0, netUSD + buyInUSD
}

// resolveSatellite handles a tap on the satellite/ticket confirmation menu.
func resolveSatellite(tgBot *tgbotapi.BotAPI, chatID int64, messageID int, tournamentID string, paidCash bool, sc storage.Store, configPath string) {
	pendingPath := filepath.Join(filepath.Dir(configPath), "pending_tournaments.json")
	pending, err := config.LoadPendingTournaments(pendingPath)
	if err != nil {
		slog.Error("loading pending tournaments", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}

	idx := -1
	for i, p := range pending {
		if p.Tournament.TournamentID == tournamentID {
			idx = i
			break
		}
	}
	if idx == -1 {
		send(tgBot, chatID, "Already resolved.")
		return
	}

	t := pending[idx].Tournament
	var resultText string
	if paidCash {
		if _, err := sc.AppendTournament(t); err != nil {
			slog.Error("appending tournament", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}
		resultText = "✅ Logged as cash entry."
	} else {
		t.BuyInUSD, t.NetUSD = RecoverTicketPayout(t.BuyInUSD, t.NetUSD)
		if _, err := sc.AppendTournament(t); err != nil {
			slog.Error("appending tournament", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}
		resultText = "✅ Logged as ticket entry — buy-in corrected to $0."
	}

	pending = append(pending[:idx], pending[idx+1:]...)
	if err := config.SavePendingTournaments(pendingPath, pending); err != nil {
		slog.Error("saving pending tournaments", "err", err)
	}

	// An empty (but non-nil) InlineKeyboard is required to actually clear the
	// buttons — NewInlineKeyboardMarkup() with no args produces a nil slice,
	// which marshals to a null the Telegram API rejects for this field.
	noButtons := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, resultText, noButtons)
	if _, err := tgBot.Send(edit); err != nil {
		slog.Error("editing telegram message", "err", err)
	}
}

// handleCommand dispatches a slash command to its handler.
func (s *conversationState) handleCommand(tgBot *tgbotapi.BotAPI, msg *tgbotapi.Message, sc storage.Store, configPath, lockfilePath, serviceAccountPath string, restartWatcher func()) {
	chatID := msg.Chat.ID
	// Disarm wizard chaining for every command except the one that (re-)arms
	// it. Without this, abandoning /start partway (or running /paths, /alerts,
	// etc. standalone afterwards) would leave startWizard stuck true, and a
	// later unrelated flow's Done would silently chain into the next wizard
	// step.
	s.startWizard = false
	switch msg.Command() {
	case "start":
		s.startWizard = true
		send(tgBot, chatID, "Welcome to Poker Tracker. Let's configure your settings.\nYou can re-run /start at any time to update these settings.")
		s.startPathsFlow(tgBot, chatID, configPath)
	case "deposit":
		handleDepositWithdraw(tgBot, chatID, msg.CommandArguments(), sc, "deposit")
	case "withdraw":
		handleDepositWithdraw(tgBot, chatID, msg.CommandArguments(), sc, "withdrawal")
	case "status":
		handleStatus(tgBot, chatID, sc, lockfilePath)
	case "lastsession":
		handleLastSession(tgBot, chatID, sc)
	case "stats":
		handleStats(tgBot, chatID, msg.CommandArguments(), sc)
	case "sheet":
		handleSheetCommand(tgBot, chatID, msg.CommandArguments(), sc, configPath, serviceAccountPath)
	case "export":
		handleExportCommand(tgBot, chatID, sc)
	case "credentials":
		s.startCredentialsFlow(tgBot, chatID, configPath)
	case "screenname":
		handleScreenNameCommand(tgBot, chatID, msg.CommandArguments(), configPath, restartWatcher)
	case "paths":
		s.startPathsFlow(tgBot, chatID, configPath)
	case "alerts":
		s.startAlertsFlow(tgBot, chatID, configPath)
	case "hardstop":
		s.handleHardstopCommand(tgBot, chatID, msg.CommandArguments(), configPath, lockfilePath)
	case "schedule":
		s.startScheduleFlow(tgBot, chatID, configPath)
	case "config":
		send(tgBot, chatID, configText)
	case "help":
		send(tgBot, chatID, helpText(msg.CommandArguments()))
	}
}

func parsePositiveFloat(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return v, nil
}

func parsePositiveInt(s string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return v, nil
}

// validateNotBelow rejects value if other is set and value is lower than
// it. Used when saving a hard stop threshold, to ensure it isn't set below
// an already-configured alert for the same metric — alerts are meant to
// warn before hard stop intervenes, so hard stop must never trigger first.
func validateNotBelow[T int | float64](value T, other *T, valueLabel, otherLabel string) error {
	if other != nil && value < *other {
		return fmt.Errorf("%s must be at least %v — %s is already set to that.", valueLabel, *other, otherLabel)
	}
	return nil
}

// validateNotAbove is the mirror of validateNotBelow, used when saving an
// alert threshold to ensure it isn't set above an already-configured hard
// stop for the same metric.
func validateNotAbove[T int | float64](value T, other *T, valueLabel, otherLabel string) error {
	if other != nil && value > *other {
		return fmt.Errorf("%s must be at most %v — %s is already set to that.", valueLabel, *other, otherLabel)
	}
	return nil
}

// handleDepositWithdraw implements /deposit and /withdraw. kind is
// "deposit" or "withdrawal" — passed directly as the Bankroll Ledger Type.
func handleDepositWithdraw(tgBot *tgbotapi.BotAPI, chatID int64, args string, sc storage.Store, kind string) {
	amount, err := parsePositiveFloat(args)
	if err != nil {
		if kind == "deposit" {
			send(tgBot, chatID, "Invalid amount. Usage: /deposit <usd_amount> — enter the USD amount shown on PokerStars after the deposit clears, not BRL.")
		} else {
			send(tgBot, chatID, "Invalid amount. Usage: /withdraw <usd_amount> — enter the USD amount shown on PokerStars, not BRL.")
		}
		return
	}

	ledgerAmount := amount
	label := "Deposit"
	if kind == "withdrawal" {
		ledgerAmount = -amount
		label = "Withdrawal"
	}

	if err := sc.AppendLedgerEntry(kind, ledgerAmount); err != nil {
		slog.Error("appending ledger entry", "kind", kind, "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	bankroll, err := sc.GetCurrentBankroll()
	if err != nil {
		slog.Error("getting bankroll", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	send(tgBot, chatID, fmt.Sprintf("✅ %s logged: %s\nBankroll: $%.2f", label, formatSignedUSD(ledgerAmount), bankroll))
}

func netSince(sc storage.Store, since time.Time) (float64, error) {
	entries, err := sc.GetLedgerEntriesSince(since)
	if err != nil {
		return 0, err
	}
	var net float64
	for _, e := range entries {
		net += e.AmountUSD
	}
	return net, nil
}

// handleStatus implements /status.
func handleStatus(tgBot *tgbotapi.BotAPI, chatID int64, sc storage.Store, lockfilePath string) {
	bankroll, err := sc.GetCurrentBankroll()
	if err != nil {
		slog.Error("getting bankroll", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}

	now := time.Now()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	startOfWeek := now.AddDate(0, 0, -7)
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	todayNet, err := netSince(sc, startOfToday)
	if err != nil {
		slog.Error("getting today's net", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	weekNet, err := netSince(sc, startOfWeek)
	if err != nil {
		slog.Error("getting week's net", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	monthNet, err := netSince(sc, startOfMonth)
	if err != nil {
		slog.Error("getting month's net", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}

	text := fmt.Sprintf("💰 Status\nBankroll: $%.2f\nToday: %s\nWeek: %s\nMonth: %s",
		bankroll, formatSignedUSD(todayNet), formatSignedUSD(weekNet), formatSignedUSD(monthNet))

	lock, err := config.LoadLock(lockfilePath)
	if err != nil {
		slog.Error("loading lockfile", "err", err)
	} else if lock != nil && !config.IsLockExpired(lock) {
		text += fmt.Sprintf("\n🛑 Locked until %s", lock.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}

	send(tgBot, chatID, text)
}

// SpinGoPrizePoolText formats a Spin & Go's prize pool for display: the real
// dollar amount and multiplier when there's a real cash pool, or the pool's
// ticket/pass description text when there isn't (e.g. "$1.00 Power Path
// Step 2 Ticket" — see parsers.Tournament.PrizePoolDescription; a
// ticket-denominated pool has no real dollar amount to show a multiplier
// for, so Multiplier is nil in that case). The one place this is computed,
// so every notification that shows a Spin & Go's prize pool (the
// post-session alert in main.go, /lastsession here) renders it identically
// and never risks dereferencing a nil Multiplier.
func SpinGoPrizePoolText(t parsers.Tournament) string {
	if t.PrizePoolDescription != "" {
		return t.PrizePoolDescription
	}
	mult := 0.0
	if t.Multiplier != nil {
		mult = *t.Multiplier
	}
	return fmt.Sprintf("$%.2f (%gx)", t.PrizePoolUSD, mult)
}

// handleLastSession implements /lastsession. The Bankroll Ledger alone
// doesn't carry enough detail (stake, hands, buy-in, finish, ...) to
// reproduce the post-session summary format, so this pulls the most recent
// row from Cash Sessions and Tournaments directly and picks whichever is
// more recent.
func handleLastSession(tgBot *tgbotapi.BotAPI, chatID int64, sc storage.Store) {
	cashSessions, err := sc.GetCashSessionsSince(time.Time{})
	if err != nil {
		slog.Error("getting cash sessions", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	tournaments, err := sc.GetTournamentsSince(time.Time{})
	if err != nil {
		slog.Error("getting tournaments", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}

	var lastCash *parsers.CashSession
	if len(cashSessions) > 0 {
		lastCash = &cashSessions[len(cashSessions)-1]
	}
	var lastTourn *parsers.Tournament
	if len(tournaments) > 0 {
		lastTourn = &tournaments[len(tournaments)-1]
	}

	if lastCash == nil && lastTourn == nil {
		send(tgBot, chatID, "No sessions recorded yet.")
		return
	}

	bankroll, err := sc.GetCurrentBankroll()
	if err != nil {
		slog.Error("getting bankroll", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}

	if lastTourn == nil || (lastCash != nil && lastCash.Date.After(lastTourn.Date)) {
		send(tgBot, chatID, fmt.Sprintf("✅ Cash Session\nStake: %s\nNet: %s\nHands: %d\nDuration: %d min\nVPIP: %s\nPFR: %s\nBankroll: $%.2f",
			lastCash.Stake, formatSignedUSD(lastCash.NetUSD), lastCash.Hands, lastCash.DurationMin,
			formatPercent(lastCash.VPIP), formatPercent(lastCash.PFR), bankroll))
		return
	}

	if lastTourn.BaseType() == "spingo" {
		send(tgBot, chatID, fmt.Sprintf("🎰 Spin & Go\nBuy-in: $%.2f\nPrize pool: %s\nFinish: %s\nNet: %s\nVPIP: %s\nPFR: %s\nBankroll: $%.2f",
			lastTourn.BuyInUSD, SpinGoPrizePoolText(*lastTourn), ordinal(lastTourn.FinishPos), formatSignedUSD(lastTourn.NetUSD),
			formatPercentPtr(lastTourn.VPIP), formatPercentPtr(lastTourn.PFR), bankroll))
		return
	}
	send(tgBot, chatID, fmt.Sprintf("🏆 MTT\nBuy-in: $%.2f\nFinish: %s / %d\nNet: %s\nVPIP: %s\nPFR: %s\nBankroll: $%.2f",
		lastTourn.BuyInUSD, ordinal(lastTourn.FinishPos), lastTourn.TotalPlayers, formatSignedUSD(lastTourn.NetUSD),
		formatPercentPtr(lastTourn.VPIP), formatPercentPtr(lastTourn.PFR), bankroll))
}

// handleStats implements /stats.
func handleStats(tgBot *tgbotapi.BotAPI, chatID int64, args string, sc storage.Store) {
	gameType, period := ParseStatsArgs(args)
	since := statsSince(period)
	periodLabel := period
	if periodLabel == "" {
		periodLabel = "all"
	}

	if gameType == "" || gameType == "cash" {
		msg, err := cashStatsMessage(sc, since, periodLabel)
		if err != nil {
			slog.Error("getting cash stats", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}
		send(tgBot, chatID, msg)
	}
	if gameType == "" || gameType == "spins" {
		msg, err := tournamentStatsMessage(sc, since, "spingo", periodLabel, "Spin & Go", "🎰", false)
		if err != nil {
			slog.Error("getting spins stats", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}
		send(tgBot, chatID, msg)
	}
	if gameType == "" || gameType == "mtt" {
		msg, err := tournamentStatsMessage(sc, since, "mtt", periodLabel, "MTT", "🏆", true)
		if err != nil {
			slog.Error("getting mtt stats", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}
		send(tgBot, chatID, msg)
	}
}

func ParseStatsArgs(args string) (gameType, period string) {
	for _, tok := range strings.Fields(args) {
		tok = strings.ToLower(tok)
		switch tok {
		case "cash", "spins", "mtt":
			gameType = tok
		case "today", "week", "month", "all":
			period = tok
		}
	}
	return gameType, period
}

func statsSince(period string) time.Time {
	now := time.Now()
	switch period {
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	case "week":
		return now.AddDate(0, 0, -7)
	case "month":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	default:
		return time.Time{}
	}
}

func cashStatsMessage(sc storage.Store, since time.Time, periodLabel string) (string, error) {
	sessions, err := sc.GetCashSessionsSince(since)
	if err != nil {
		return "", err
	}
	var net float64
	var hands int
	for _, s := range sessions {
		net += s.NetUSD
		hands += s.Hands
	}
	bb100, vpip, pfr := weightedCashAverages(sessions)
	return fmt.Sprintf("📈 Cash Stats (%s)\nNet: %s\nBB/100: %.1f\nVPIP: %s\nPFR: %s\nHands: %d\nSessions: %d",
		periodLabel, formatSignedUSD(net), bb100, formatPercent(vpip), formatPercent(pfr), hands, len(sessions)), nil
}

func tournamentStatsMessage(sc storage.Store, since time.Time, tType, periodLabel, label, emoji string, includeAvgFinish bool) (string, error) {
	tournaments, err := sc.GetTournamentsSince(since)
	if err != nil {
		return "", err
	}
	var filtered []parsers.Tournament
	for _, t := range tournaments {
		if t.BaseType() == tType {
			filtered = append(filtered, t)
		}
	}
	var net float64
	for _, t := range filtered {
		net += t.NetUSD
	}
	r := roi(filtered, tType)
	vpip, pfr := weightedTournamentAverages(filtered)

	if !includeAvgFinish {
		return fmt.Sprintf("%s %s Stats (%s)\nNet: %s\nROI: %s\nVPIP: %s\nPFR: %s\nGames: %d",
			emoji, label, periodLabel, formatSignedUSD(net), formatSignedPercent(r), formatPercent(vpip), formatPercent(pfr), len(filtered)), nil
	}

	var avgFinishSum float64
	var cnt int
	for _, t := range filtered {
		if t.TotalPlayers > 0 {
			avgFinishSum += float64(t.FinishPos) / float64(t.TotalPlayers) * 100
			cnt++
		}
	}
	var avgFinish float64
	if cnt > 0 {
		avgFinish = avgFinishSum / float64(cnt)
	}
	return fmt.Sprintf("%s %s Stats (%s)\nNet: %s\nROI: %s\nVPIP: %s\nPFR: %s\nGames: %d\nAvg finish: %.0f%%",
		emoji, label, periodLabel, formatSignedUSD(net), formatSignedPercent(r), formatPercent(vpip), formatPercent(pfr), len(filtered), avgFinish), nil
}

// ensureScreenName checks Setup.PokerStarsScreenName before any of the four
// config flows (paths, alerts, hardstop, schedule) starts. Screen name is
// normally set once by --setup and never touched again — but --serve no
// longer requires it to be set at startup, and config.json is re-read from
// disk on every message, so it can still be null here (fresh install before
// --setup is fully built, or lost/reset while --serve keeps running). If
// it's missing, this interrupts with a recovery prompt instead of the
// caller's own flow, and remembers resumeFlow so the right flow starts once
// it's answered. Returns true if the caller may proceed immediately.
func (s *conversationState) ensureScreenName(tgBot *tgbotapi.BotAPI, chatID int64, cfg *config.Config, resumeFlow string) bool {
	if cfg.Setup.PokerStarsScreenName != nil {
		return true
	}
	s.flow = "screen_name"
	s.field = "screen_name"
	s.resumeFlow = resumeFlow
	send(tgBot, chatID, "Your PokerStars screen name isn't set. Send it now (exact spelling, case-sensitive) before continuing.")
	return false
}

// --- /paths flow ---

func (s *conversationState) startPathsFlow(tgBot *tgbotapi.BotAPI, chatID int64, configPath string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	if !s.ensureScreenName(tgBot, chatID, cfg, "paths") {
		return
	}
	s.flow = "paths"
	s.field = ""
	s.pathsFirstTimeSetup = cfg.Paths.HandHistory == nil && cfg.Paths.TournamentSummary == nil
	sendPathsKeyboard(tgBot, chatID, cfg)
}

func sendPathsKeyboard(tgBot *tgbotapi.BotAPI, chatID int64, cfg *config.Config) {
	// The value is shown on the button only when set, and only as the part
	// relative to the user's home directory (displayRelativeToHome) — short
	// enough now to fit the button without the Telegram client truncating
	// it, unlike the full absolute path shown here previously.
	hhLabel := "Hand History"
	if cfg.Paths.HandHistory != nil {
		hhLabel = "Hand History: " + displayRelativeToHome(*cfg.Paths.HandHistory)
	}
	tsLabel := "Tournament Summary"
	if cfg.Paths.TournamentSummary != nil {
		tsLabel = "Tournament Summary: " + displayRelativeToHome(*cfg.Paths.TournamentSummary)
	}
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(hhLabel, "paths:hand_history")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(tsLabel, "paths:tournament_summary")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Done", "paths:done")),
	)
	sendWithKeyboard(tgBot, chatID, "Configure PokerStars folder locations:", markup)
}

func (s *conversationState) handlePathsCallback(tgBot *tgbotapi.BotAPI, chatID int64, action, configPath string, restartWatcher func()) {
	if action == "done" {
		cfg, err := config.LoadConfig(configPath)
		if err != nil {
			slog.Error("loading config", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}

		if cfg.Paths.HandHistory == nil || cfg.Paths.TournamentSummary == nil {
			send(tgBot, chatID, "Both paths must be set before continuing. Tap a field to configure it.")
			sendPathsKeyboard(tgBot, chatID, cfg)
			return
		}

		wasFirstTime := s.pathsFirstTimeSetup
		inWizard := s.startWizard
		s.reset()

		restartWatcher()

		if wasFirstTime {
			send(tgBot, chatID, "✅ Paths saved. Tracker is now active.")
		} else {
			send(tgBot, chatID, "✅ Paths saved. Watcher restarted.")
		}

		if inWizard {
			s.startAlertsFlow(tgBot, chatID, configPath)
		}
		return
	}

	s.flow = "paths"
	s.field = action
	send(tgBot, chatID, pathPromptText(action))
}

func pathPromptText(field string) string {
	if field == "tournament_summary" {
		return "Send the Tournament Summary folder path, relative to your Windows user folder — not including your screen name at the end.\n" +
			`Example: AppData\Local\PokerStars\TournamentSummaries` + "\n" +
			`(C:\Users\<you>\ and \YourScreenName are both added automatically — a full path with a drive letter, e.g. D:\..., also works if your data isn't under your user folder)`
	}
	return "Send the Hand History folder path, relative to your Windows user folder — not including your screen name at the end.\n" +
		`Example: AppData\Local\PokerStars\HandHistory` + "\n" +
		`(C:\Users\<you>\ and \YourScreenName are both added automatically — a full path with a drive letter, e.g. D:\..., also works if your data isn't under your user folder)`
}

func NormalizeWindowsPath(input string) string {
	return strings.ReplaceAll(strings.TrimSpace(input), "/", "\\")
}

// windowsPathExists reports whether path exists. Uses os.Stat directly
// rather than shelling out to cmd.exe's "if exist" — paths containing
// spaces require embedding quotes in the shelled-out command, but Go's
// Windows argument escaping then wraps the whole string in its own outer
// quotes too, producing more than the two quote characters cmd.exe's /C
// handling can cleanly preserve. It falls back to stripping only the first
// and last quote character in the line, leaving the inner backslash-escaped
// quotes (and their backslashes) in place, which corrupts the path.
func windowsPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isAbsoluteWindowsPath reports whether path already starts with a drive
// letter (e.g. "C:\..."), meaning it should be used as typed rather than
// resolved against the user's home directory.
func isAbsoluteWindowsPath(path string) bool {
	return len(path) >= 2 && path[1] == ':'
}

// fixMissingDriveColon repairs a path typed as a drive letter without its
// colon (e.g. "C/Users/..." normalizes to "C\Users\..." with no ":"). A
// single letter immediately followed by a separator is never a legitimate
// first-level folder name under the player's home directory, so this is
// unambiguous: it's a drive letter, not a relative path starting with a
// folder literally named "C". Without this, such a path would silently be
// treated as relative and double-joined onto the home directory instead of
// failing (or working) cleanly.
func fixMissingDriveColon(path string) string {
	if len(path) >= 2 && isASCIILetter(path[0]) && path[1] == '\\' {
		return path[:1] + ":" + path[1:]
	}
	return path
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// resolveAgainstHomeDir resolves a path the player typed relative to their
// own Windows user folder — e.g. "AppData\Local\PokerStars\HandHistory"
// becomes "C:\Users\<them>\AppData\Local\PokerStars\HandHistory". PokerStars
// always writes under the player's profile, so this is the common case and
// saves them from typing (and mistyping) "C:\Users\<name>\" every time. A
// path that's already absolute (starts with a drive letter) is returned
// unchanged, so a full path still works if the data lives elsewhere.
func resolveAgainstHomeDir(path string) (string, error) {
	if isAbsoluteWindowsPath(path) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("getting user home directory: %w", err)
	}
	return filepath.Join(home, path), nil
}

// displayRelativeToHome shortens path for display by stripping the user's
// home directory prefix, if present, so keyboard confirmation messages stay
// short enough to read comfortably on a phone screen.
func displayRelativeToHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// handleScreenNameReply saves the PokerStars screen name entered in
// response to ensureScreenName's recovery prompt, then resumes whichever
// flow (paths, alerts, hardstop, schedule) triggered it.
func (s *conversationState) handleScreenNameReply(tgBot *tgbotapi.BotAPI, chatID int64, text, configPath string) {
	name := strings.TrimSpace(text)
	if name == "" {
		send(tgBot, chatID, "Screen name can't be empty. Send your exact PokerStars screen name.")
		return
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
		return
	}
	cfg.Setup.PokerStarsScreenName = &name
	if err := config.SaveConfig(configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
		return
	}

	send(tgBot, chatID, fmt.Sprintf("✅ Screen name saved: %s", name))

	resumeFlow := s.resumeFlow
	s.resumeFlow = ""
	switch resumeFlow {
	case "paths":
		s.startPathsFlow(tgBot, chatID, configPath)
	case "alerts":
		s.startAlertsFlow(tgBot, chatID, configPath)
	case "hardstop":
		s.startHardstopFlow(tgBot, chatID, configPath)
	case "schedule":
		s.startScheduleFlow(tgBot, chatID, configPath)
	}
}

func (s *conversationState) handlePathsReply(tgBot *tgbotapi.BotAPI, chatID int64, text, configPath string) {
	normalized := fixMissingDriveColon(NormalizeWindowsPath(text))

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
		return
	}
	if cfg.Setup.PokerStarsScreenName == nil {
		// Lost between startPathsFlow and this reply (e.g. config.json
		// touched externally mid-flow) — re-enter recovery rather than
		// dead-ending on an error.
		s.startPathsFlow(tgBot, chatID, configPath)
		return
	}

	full, err := resolveAgainstHomeDir(normalized)
	if err != nil {
		slog.Error("resolving home directory", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
		return
	}

	joined := filepath.Join(full, *cfg.Setup.PokerStarsScreenName)
	if !windowsPathExists(joined) {
		send(tgBot, chatID, fmt.Sprintf("That path doesn't exist: %s\nSend the folder path again.", joined))
		return
	}

	switch s.field {
	case "hand_history":
		cfg.Paths.HandHistory = &full
	case "tournament_summary":
		cfg.Paths.TournamentSummary = &full
	}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
		return
	}
	s.field = ""
	sendPathsKeyboard(tgBot, chatID, cfg)
}

// EvaluateSessionLossAlert sends a warning (not a hard stop) if the cash
// session's net loss exceeds Alerts.SessionLossBuyins × BuyInUSD. Called
// from the worker goroutine right after a cash session is parsed and
// written to sheets, so it must go through alertChan rather than sending to
// Telegram directly (only the bot goroutine may do that).
func EvaluateSessionLossAlert(session parsers.CashSession, sc storage.Store, configPath string, alertChan chan<- string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if cfg.Alerts.SessionLossBuyins == nil || session.BuyInUSD == 0 || session.NetUSD >= 0 {
		return nil
	}

	loss := -session.NetUSD
	if loss <= *cfg.Alerts.SessionLossBuyins*session.BuyInUSD {
		return nil
	}

	bankroll, err := sc.GetCurrentBankroll()
	if err != nil {
		return err
	}
	buyins := loss / session.BuyInUSD
	alertChan <- fmt.Sprintf("⚠️ Session loss alert\nLost %.1f buy-ins this session ($%.2f)\nBankroll: $%.2f", buyins, loss, bankroll)
	return nil
}

// EvaluateDownswingAlert sends a warning (not a hard stop) if the rolling
// 30-day results-only equity curve (see hardstop.ComputeEquityCurve) has
// dropped from its peak by more than Alerts.DownswingBuyins × buyInUSD.
// Called from the worker goroutine after any session — cash or tournament —
// is written to sheets. Fires independently of the downswing hard stop;
// since cross-validation enforces alert ≤ hardstop for this field, the
// alert always fires at or before the hard stop's own threshold, never
// after.
func EvaluateDownswingAlert(buyInUSD float64, sc storage.Store, configPath string, alertChan chan<- string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if cfg.Alerts.DownswingBuyins == nil || buyInUSD == 0 {
		return nil
	}

	peak, current, err := hardstop.DownswingEquityCurve(sc)
	if err != nil {
		return err
	}
	if !hardstop.DownswingExceeds(peak, current, buyInUSD, *cfg.Alerts.DownswingBuyins) {
		return nil
	}

	bankroll, err := sc.GetCurrentBankroll()
	if err != nil {
		return err
	}
	buyins := (peak - current) / buyInUSD
	alertChan <- fmt.Sprintf("⚠️ Downswing alert\nDown %.1f buy-ins from your 30-day peak\nPeak: $%.2f  Current: $%.2f\nBankroll: $%.2f", buyins, peak, current, bankroll)
	return nil
}

// --- /alerts flow ---

func (s *conversationState) startAlertsFlow(tgBot *tgbotapi.BotAPI, chatID int64, configPath string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	if !s.ensureScreenName(tgBot, chatID, cfg, "alerts") {
		return
	}
	s.flow = "alerts"
	s.field = ""
	sendAlertsKeyboard(tgBot, chatID, cfg)
}

func sendAlertsKeyboard(tgBot *tgbotapi.BotAPI, chatID int64, cfg *config.Config) {
	downswing := "not set"
	if cfg.Alerts.DownswingBuyins != nil {
		downswing = fmt.Sprintf("%g", *cfg.Alerts.DownswingBuyins)
	}
	streak := "not set"
	if cfg.Alerts.LosingStreakCount != nil {
		streak = strconv.Itoa(*cfg.Alerts.LosingStreakCount)
	}
	dailyLoss := "not set"
	if cfg.Alerts.DailyLossUSD != nil {
		dailyLoss = fmt.Sprintf("%g", *cfg.Alerts.DailyLossUSD)
	}
	spingoLoss := "not set"
	if cfg.Alerts.SpingoDailyLossUSD != nil {
		spingoLoss = fmt.Sprintf("%g", *cfg.Alerts.SpingoDailyLossUSD)
	}
	sessionLoss := "not set"
	if cfg.Alerts.SessionLossBuyins != nil {
		sessionLoss = fmt.Sprintf("%g", *cfg.Alerts.SessionLossBuyins)
	}
	// Values are shown in brackets directly on the button — these are all
	// short numeric thresholds, unlike the long file paths in /paths, so
	// there's no truncation risk on narrow screens.
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Downswing (buy-ins) ["+downswing+"]", "alerts:downswing")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Session loss (buy-ins) ["+sessionLoss+"]", "alerts:session_loss")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Losing streak (sessions) ["+streak+"]", "alerts:losing_streak")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Daily loss, all games ($) ["+dailyLoss+"]", "alerts:daily_loss")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Spin & Go daily loss ($) ["+spingoLoss+"]", "alerts:spingo_daily_loss")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Done", "alerts:done")),
	)
	sendWithKeyboard(tgBot, chatID, "Configure alert thresholds:", markup)
}

func (s *conversationState) handleAlertsCallback(tgBot *tgbotapi.BotAPI, chatID int64, action, configPath string) {
	if action == "done" {
		inWizard := s.startWizard
		s.reset()
		send(tgBot, chatID, "✅ Alert settings saved.")
		if inWizard {
			s.startHardstopFlow(tgBot, chatID, configPath)
		}
		return
	}
	s.flow = "alerts"
	s.field = action
	send(tgBot, chatID, alertsPromptText(action))
}

func alertsPromptText(field string) string {
	switch field {
	case "downswing":
		return "Send the downswing alert threshold, in buy-ins (e.g. 5 — alerts when bankroll drops 5 buy-ins below your rolling 30-day peak)."
	case "session_loss":
		return "Send the session loss threshold, in buy-ins, that should trigger a warning alert (e.g. 3 — the hard stop threshold for this, if set, must be at least this high)."
	case "losing_streak":
		return "Send the number of consecutive losing sessions that should trigger an alert (e.g. 3)."
	case "daily_loss":
		return "Send the daily loss threshold in USD, across all game types, that should trigger an alert (e.g. 20)."
	case "spingo_daily_loss":
		return "Send the Spin & Go daily loss threshold in USD that should trigger an alert (e.g. 20)."
	}
	return "Send the new value."
}

func (s *conversationState) handleAlertsReply(tgBot *tgbotapi.BotAPI, chatID int64, text, configPath string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
		return
	}

	switch s.field {
	case "downswing":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(tgBot, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotAbove(v, cfg.Hardstop.DownswingBuyins, "downswing alert", "the hard stop downswing threshold"); err != nil {
			send(tgBot, chatID, err.Error())
			return
		}
		cfg.Alerts.DownswingBuyins = &v
	case "session_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(tgBot, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotAbove(v, cfg.Hardstop.SessionLossBuyins, "session loss alert", "the hard stop session loss threshold"); err != nil {
			send(tgBot, chatID, err.Error())
			return
		}
		cfg.Alerts.SessionLossBuyins = &v
	case "losing_streak":
		v, err := parsePositiveInt(text)
		if err != nil {
			send(tgBot, chatID, "Invalid value. Please send a positive whole number.")
			return
		}
		if err := validateNotAbove(v, cfg.Hardstop.ConsecutiveLosses, "losing streak alert", "the hard stop consecutive-losses threshold"); err != nil {
			send(tgBot, chatID, err.Error())
			return
		}
		cfg.Alerts.LosingStreakCount = &v
	case "daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(tgBot, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotAbove(v, cfg.Hardstop.DailyLossUSD, "daily loss alert", "the hard stop daily loss threshold"); err != nil {
			send(tgBot, chatID, err.Error())
			return
		}
		cfg.Alerts.DailyLossUSD = &v
	case "spingo_daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(tgBot, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotAbove(v, cfg.Hardstop.SpingoDailyLossUSD, "Spin & Go daily loss alert", "the hard stop Spin & Go daily loss threshold"); err != nil {
			send(tgBot, chatID, err.Error())
			return
		}
		cfg.Alerts.SpingoDailyLossUSD = &v
	}

	if err := config.SaveConfig(configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
		return
	}
	s.field = ""
	sendAlertsKeyboard(tgBot, chatID, cfg)
}

// --- /hardstop flow ---

func (s *conversationState) handleHardstopCommand(tgBot *tgbotapi.BotAPI, chatID int64, args, configPath, lockfilePath string) {
	if strings.TrimSpace(args) == "trigger" {
		manualAlert := make(chan string, 1)
		if err := hardstop.ManualTrigger(configPath, lockfilePath, manualAlert); err != nil {
			send(tgBot, chatID, "Configure duration first with /hardstop")
			return
		}
		send(tgBot, chatID, <-manualAlert)
		return
	}
	s.startHardstopFlow(tgBot, chatID, configPath)
}

func (s *conversationState) startHardstopFlow(tgBot *tgbotapi.BotAPI, chatID int64, configPath string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	if !s.ensureScreenName(tgBot, chatID, cfg, "hardstop") {
		return
	}
	s.flow = "hardstop"
	s.field = ""
	sendHardstopKeyboard(tgBot, chatID, cfg)
}

func sendHardstopKeyboard(tgBot *tgbotapi.BotAPI, chatID int64, cfg *config.Config) {
	sessionLoss := "not set"
	if cfg.Hardstop.SessionLossBuyins != nil {
		sessionLoss = fmt.Sprintf("%g", *cfg.Hardstop.SessionLossBuyins)
	}
	consecutive := "not set"
	if cfg.Hardstop.ConsecutiveLosses != nil {
		consecutive = strconv.Itoa(*cfg.Hardstop.ConsecutiveLosses)
	}
	dailyLoss := "not set"
	if cfg.Hardstop.DailyLossUSD != nil {
		dailyLoss = fmt.Sprintf("%g", *cfg.Hardstop.DailyLossUSD)
	}
	spingoLoss := "not set"
	if cfg.Hardstop.SpingoDailyLossUSD != nil {
		spingoLoss = fmt.Sprintf("%g", *cfg.Hardstop.SpingoDailyLossUSD)
	}
	downswing := "not set"
	if cfg.Hardstop.DownswingBuyins != nil {
		downswing = fmt.Sprintf("%g", *cfg.Hardstop.DownswingBuyins)
	}
	duration := "not set"
	if cfg.Hardstop.DurationHours != nil {
		duration = strconv.Itoa(*cfg.Hardstop.DurationHours)
	}
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Session loss (buy-ins) ["+sessionLoss+"]", "hardstop:session_loss")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Downswing (buy-ins) ["+downswing+"]", "hardstop:downswing")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Consecutive losses (sessions) ["+consecutive+"]", "hardstop:consecutive_losses")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Daily loss, all games ($) ["+dailyLoss+"]", "hardstop:daily_loss")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Spin & Go daily loss ($) ["+spingoLoss+"]", "hardstop:spingo_daily_loss")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Duration (hours) ["+duration+"]", "hardstop:duration")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Done", "hardstop:done")),
	)
	sendWithKeyboard(tgBot, chatID, "Configure hard stop rules:", markup)
}

func (s *conversationState) handleHardstopCallback(tgBot *tgbotapi.BotAPI, chatID int64, action, configPath string) {
	if action == "done" {
		cfg, err := config.LoadConfig(configPath)
		if err != nil {
			slog.Error("loading config", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}

		anyRuleConfigured := cfg.Hardstop.SessionLossBuyins != nil ||
			cfg.Hardstop.DownswingBuyins != nil ||
			cfg.Hardstop.ConsecutiveLosses != nil ||
			cfg.Hardstop.DailyLossUSD != nil ||
			cfg.Hardstop.SpingoDailyLossUSD != nil
		if cfg.Hardstop.DurationHours == nil && anyRuleConfigured {
			send(tgBot, chatID, "Duration must be set if any hard stop rule is configured. Tap Duration to set it.")
			sendHardstopKeyboard(tgBot, chatID, cfg)
			return
		}

		inWizard := s.startWizard
		s.reset()
		send(tgBot, chatID, "✅ Hard stop settings saved.")
		if inWizard {
			s.startScheduleFlow(tgBot, chatID, configPath)
		}
		return
	}
	s.flow = "hardstop"
	s.field = action
	send(tgBot, chatID, hardstopPromptText(action))
}

func hardstopPromptText(field string) string {
	switch field {
	case "session_loss":
		return "Send the session loss threshold, in buy-ins, that should trigger a hard stop (e.g. 5)."
	case "downswing":
		return "Send the downswing threshold, in buy-ins, that should trigger a hard stop (e.g. 10 — fires when your rolling 30-day peak minus your current bankroll exceeds this many buy-ins)."
	case "consecutive_losses":
		return "Send the number of consecutive losing sessions that should trigger a hard stop (e.g. 4)."
	case "daily_loss":
		return "Send the daily loss threshold in USD, across all game types, that should trigger a hard stop (e.g. 30)."
	case "spingo_daily_loss":
		return "Send the Spin & Go daily loss threshold in USD that should trigger a hard stop (e.g. 30)."
	case "duration":
		return "Send the hard stop lock duration in hours. Must be 12 or 24."
	}
	return "Send the new value."
}

func (s *conversationState) handleHardstopReply(tgBot *tgbotapi.BotAPI, chatID int64, text, configPath string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
		return
	}

	switch s.field {
	case "session_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(tgBot, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotBelow(v, cfg.Alerts.SessionLossBuyins, "hard stop session loss threshold", "the session loss alert"); err != nil {
			send(tgBot, chatID, err.Error())
			return
		}
		cfg.Hardstop.SessionLossBuyins = &v
	case "downswing":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(tgBot, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotBelow(v, cfg.Alerts.DownswingBuyins, "hard stop downswing threshold", "the downswing alert"); err != nil {
			send(tgBot, chatID, err.Error())
			return
		}
		cfg.Hardstop.DownswingBuyins = &v
	case "consecutive_losses":
		v, err := parsePositiveInt(text)
		if err != nil {
			send(tgBot, chatID, "Invalid value. Please send a positive whole number.")
			return
		}
		if err := validateNotBelow(v, cfg.Alerts.LosingStreakCount, "hard stop consecutive-losses threshold", "the losing streak alert"); err != nil {
			send(tgBot, chatID, err.Error())
			return
		}
		cfg.Hardstop.ConsecutiveLosses = &v
	case "daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(tgBot, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotBelow(v, cfg.Alerts.DailyLossUSD, "hard stop daily loss threshold", "the daily loss alert"); err != nil {
			send(tgBot, chatID, err.Error())
			return
		}
		cfg.Hardstop.DailyLossUSD = &v
	case "spingo_daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(tgBot, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotBelow(v, cfg.Alerts.SpingoDailyLossUSD, "hard stop Spin & Go daily loss threshold", "the Spin & Go daily loss alert"); err != nil {
			send(tgBot, chatID, err.Error())
			return
		}
		cfg.Hardstop.SpingoDailyLossUSD = &v
	case "duration":
		v, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || (v != 12 && v != 24) {
			send(tgBot, chatID, "Duration must be 12 or 24.")
			return
		}
		cfg.Hardstop.DurationHours = &v
	}

	if err := config.SaveConfig(configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
		return
	}
	s.field = ""
	sendHardstopKeyboard(tgBot, chatID, cfg)
}

// --- /schedule flow ---

func (s *conversationState) startScheduleFlow(tgBot *tgbotapi.BotAPI, chatID int64, configPath string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		return
	}
	if !s.ensureScreenName(tgBot, chatID, cfg, "schedule") {
		return
	}
	s.flow = "schedule"
	s.field = ""
	sendScheduleKeyboard(tgBot, chatID, cfg)
}

func sendScheduleKeyboard(tgBot *tgbotapi.BotAPI, chatID int64, cfg *config.Config) {
	dayStartHour := "not set"
	if cfg.Schedule.DayStartHour != nil {
		dayStartHour = strconv.Itoa(*cfg.Schedule.DayStartHour)
	}
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Poker day start hour", "schedule:day_start_hour")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Done", "schedule:done")),
	)
	text := fmt.Sprintf("Configure your poker day schedule:\nPoker day start hour: %s", dayStartHour)
	sendWithKeyboard(tgBot, chatID, text, markup)
}

func (s *conversationState) handleScheduleCallback(tgBot *tgbotapi.BotAPI, chatID int64, action string) {
	if action == "done" {
		inWizard := s.startWizard
		s.startWizard = false
		s.reset()
		send(tgBot, chatID, "✅ Schedule saved.")
		if inWizard {
			send(tgBot, chatID, "✅ Setup complete. The tracker is running. Type /help to see all commands.")
		}
		return
	}
	s.flow = "schedule"
	s.field = action
	send(tgBot, chatID, "What hour does your poker day start? Send a number from 0 to 23 (e.g. 6 for 6am local time).")
}

func (s *conversationState) handleScheduleReply(tgBot *tgbotapi.BotAPI, chatID int64, text, configPath string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
		return
	}

	v, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || v < 0 || v > 23 {
		send(tgBot, chatID, "Invalid value. Please send a whole number from 0 to 23.")
		return
	}
	cfg.Schedule.DayStartHour = &v

	if err := config.SaveConfig(configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
		return
	}
	s.field = ""
	sendScheduleKeyboard(tgBot, chatID, cfg)
}

// --- /config and /help ---

const configText = "/paths — set PokerStars hand history and tournament folder locations\n" +
	"/alerts — configure notification thresholds (downswing, losing streak, Spin & Go drain)\n" +
	"/hardstop — configure automatic hard stop rules and lock duration\n" +
	"/schedule — set your poker day start hour for daily loss tracking"

var commandList = []struct {
	name string
	desc string
}{
	{"start", "Run the full setup wizard (paths, alerts, hard stop, schedule)."},
	{"deposit", "Log a deposit to your bankroll ledger."},
	{"withdraw", "Log a withdrawal from your bankroll ledger."},
	{"status", "Show current bankroll and P&L."},
	{"lastsession", "Show the summary of the last recorded session."},
	{"stats", "Show filtered performance stats."},
	{"sheet", "Get the Google Sheets URL, or switch to a different sheet."},
	{"export", "Export all your data as CSV files."},
	{"credentials", "Swap the Google service account credentials file."},
	{"screenname", "Get or change your PokerStars screen name."},
	{"paths", "Configure PokerStars folder locations."},
	{"alerts", "Configure alert thresholds."},
	{"hardstop", "Configure hard stop rules, or trigger one manually."},
	{"schedule", "Set your poker day start hour."},
	{"config", "List configuration commands."},
	{"help", "Show this help, or details on one command."},
}

var commandHelp = map[string]string{
	"help": "Use /help alone to list all commands. Use /help <command> to get details on a specific one. Example: /help stats",
	"stats": "Shows performance stats.\n" +
		"Usage: /stats [cash|spins|mtt] [today|week|month|all]\n" +
		"Both arguments are optional. Default is all-time stats for all game types.\n" +
		"Examples:\n" +
		"  /stats — all-time, all game types\n" +
		"  /stats cash — cash game stats, all time\n" +
		"  /stats spins week — Spin & Go stats for the past 7 days\n" +
		"  /stats mtt month — MTT stats for this calendar month\n" +
		"  /stats cash month — cash game stats for this calendar month",
	"deposit": "Logs a deposit to your bankroll ledger.\n" +
		"Usage: /deposit <amount>\n" +
		"Enter the USD amount shown on PokerStars after the deposit clears — not BRL.\n" +
		"Example: /deposit 50.00",
	"withdraw": "Logs a withdrawal from your bankroll ledger.\n" +
		"Usage: /withdraw <amount>\n" +
		"Enter the USD amount shown on PokerStars — not BRL.\n" +
		"Example: /withdraw 20.00",
	"hardstop": "Configures hard stop rules or triggers one manually.\n" +
		"Usage (configure): /hardstop — opens the settings menu\n" +
		"Usage (manual trigger): /hardstop trigger — activates a hard stop immediately\n" +
		"Fields: session loss (buy-ins), downswing (buy-ins, rolling 30-day peak), consecutive losses (sessions), daily loss (all games, $), Spin & Go daily loss ($), duration (hours).\n" +
		"The hard stop closes PokerStars and blocks the launcher for the configured duration.\n" +
		"Duration must be configured before using /hardstop trigger.\n" +
		"A hard stop threshold can never be set lower than its corresponding alert threshold, if that alert is configured.",
	"alerts": "Configures notification alert thresholds.\n" +
		"Usage: /alerts — opens the settings menu\n" +
		"Fields: downswing (buy-ins, rolling 30-day peak), session loss (buy-ins), losing streak (sessions), daily loss (all games, $), Spin & Go daily loss ($).\n" +
		"Alerts fire automatically and require no further action from you.\n" +
		"An alert threshold can never be set higher than its corresponding hard stop threshold, if that hard stop is configured.",
	"lastsession": "Shows the summary of the last recorded session (cash or Spin & Go).\n" +
		"Usage: /lastsession\n" +
		"No arguments.",
	"start": "Runs the full setup wizard: /paths, then /alerts, then /hardstop, then /schedule.\n" +
		"Usage: /start\n" +
		"Re-runnable at any time.",
	"status": "Shows current bankroll and profit/loss.\n" +
		"Usage: /status\n" +
		"No arguments.",
	"sheet": "With no argument, returns the URL of your Google Sheets tracker. With an argument, switches to a different sheet — send its name (resolved via Drive) or paste its full URL. Validates access before saving; requires a restart of the tracker service to take effect.\n" +
		"Usage: /sheet [name or URL]\n" +
		"Examples:\n" +
		"  /sheet — show the current sheet URL\n" +
		"  /sheet My Other Bankroll — switch by name\n" +
		"  /sheet https://docs.google.com/spreadsheets/d/... — switch by URL",
	"export": "Exports all your data as CSV files: cash sessions, tournaments, bankroll ledger, time analysis.\n" +
		"Usage: /export\n" +
		"No arguments. Works on both storage backends and produces identical output either way. Sends four files, one per data set — an empty data set still sends a header-only file.",
	"credentials": "Swaps the Google service account credentials file used to access Sheets/Drive.\n" +
		"Usage: /credentials — then send the google_service_account.json file as an attachment, or type skip to cancel.\n" +
		"If a sheet is already configured, it's re-checked against the new credentials — if it's no longer reachable (not shared with the new service account, or a different Google Cloud project entirely), the sheet is cleared from config and you'll need to run /sheet again.\n" +
		"Requires a restart of the tracker service to take effect.",
	"screenname": "Gets or changes your PokerStars screen name (exact capitalisation matters).\n" +
		"Usage: /screenname [name]\n" +
		"With no argument, shows the current screen name. With an argument, changes it and restarts the watcher immediately — no service restart needed.\n" +
		"Examples:\n" +
		"  /screenname — show the current screen name\n" +
		"  /screenname NewHandle99 — change it",
	"paths": "Configures the PokerStars hand history and tournament summary folder locations.\n" +
		"Usage: /paths — opens the settings menu\n" +
		"Enter the base folder only — not the folder with your screen name in it.",
	"config": "Lists the available configuration commands.\n" +
		"Usage: /config\n" +
		"No arguments.",
	"schedule": "Sets the hour your poker day starts, used for the daily loss\n" +
		"hard stop and alert thresholds.\n" +
		"Usage: /schedule — opens the settings menu\n" +
		"Default is 6 (6am local time) if never set.\n" +
		"Example: set to 6 if you never play before 6am.",
}

func helpText(arg string) string {
	arg = strings.TrimSpace(strings.ToLower(arg))
	if arg == "" {
		var b strings.Builder
		b.WriteString("Available commands:\n")
		for _, c := range commandList {
			fmt.Fprintf(&b, "/%s — %s\n", c.name, c.desc)
		}
		return strings.TrimRight(b.String(), "\n")
	}
	arg = strings.TrimPrefix(arg, "/")
	if text, ok := commandHelp[arg]; ok {
		return text
	}
	return fmt.Sprintf("No help available for %q.", arg)
}
