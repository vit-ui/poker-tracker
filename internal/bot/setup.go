package bot

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/backend"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sheets"
)

// RunSetup starts the bot in setup mode and blocks until the player
// completes the configuration wizard via Telegram: chat ID detection,
// an optional service account credentials upload, Google Sheet, PokerStars
// screen name, and both base folder paths. Used only by --setup — no
// sheets client and no watcher exist yet at this point, so this runs
// sequentially rather than through the conversation-state machinery used by
// the four config flows in --serve mode.
//
// Every step past chat ID detection is skipped if its field is already
// set in the loaded config — matching setup.iss's manual-path ShouldSkipPage
// gating exactly (SheetAlreadySet/ScreenNameAlreadySet/HandHistoryAlreadySet/
// TournamentAlreadySet/CredentialsAlreadySet there). A rerun over a complete
// install then does nothing but the chat handshake and a "still all set"
// confirmation. Reinstalls wait for contact from the saved chat; fresh installs
// detect their chat from the first text message.
func RunSetup(token, configPath, serviceAccountPath string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("loading setup config: %w", err)
	}
	var allowedChatID int64
	if cfg.Setup.TelegramChatID != nil {
		allowedChatID, err = strconv.ParseInt(*cfg.Setup.TelegramChatID, 10, 64)
		if err != nil || allowedChatID == 0 {
			return fmt.Errorf("invalid saved setup chat ID")
		}
	}
	tgBot, err := newTelegramBot(token)
	if err != nil {
		return fmt.Errorf("starting Telegram setup: check the token and network")
	}
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := tgBot.GetUpdatesChan(u)
	defer tgBot.StopReceivingUpdates()
	chatID := waitForFirstMessage(updates, allowedChatID)
	if chatID == 0 {
		return fmt.Errorf("setup update stream closed before chat contact")
	}
	chatIDStr := strconv.FormatInt(chatID, 10)
	cfg, err = config.UpdateConfig(configPath, func(current *config.Config) error {
		if current.Setup.TelegramChatID == nil {
			current.Setup.TelegramChatID = &chatIDStr
		} else if *current.Setup.TelegramChatID != chatIDStr {
			return fmt.Errorf("saved setup chat changed while waiting for contact")
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("saving setup chat: %w", err)
	}
	_, credentialsErr := os.Stat(serviceAccountPath)
	// StorageBackend deliberately excluded: setup.iss's WritePreSeedConfig
	// writes it into a brand-new config.json before ever launching --setup,
	// so it's non-nil on every fresh Telegram install too, not just a real
	// upgrade. The other fields are only ever set by a completed prior run
	// of this same wizard.
	isUpgrade := cfg.Setup.GoogleSheetName != nil || cfg.Setup.GoogleSheetID != nil ||
		cfg.Setup.PokerStarsScreenName != nil || cfg.Paths.HandHistory != nil ||
		cfg.Paths.TournamentSummary != nil || credentialsErr == nil

	if isUpgrade {
		send(tgBot, chatID, "Welcome back! I found your existing configuration — I'll only ask about what's missing.")
	} else {
		send(tgBot, chatID, "Hi! I'm your Poker Tracker bot. Let's get you set up.")
	}

	// Step 1.5 — storage backend. Skipped if already set. Gates every Google
	// step below (credentials, sheet) — a sqlite install must never need
	// Google credentials on disk, so this has to be decided before either
	// of those runs.
	var backendName string
	if cfg.Setup.StorageBackend != nil {
		backendName = *cfg.Setup.StorageBackend
		send(tgBot, chatID, "✅ Keeping existing storage backend.")
	} else {
		markup := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Google Sheets", "storage:sheets"),
				tgbotapi.NewInlineKeyboardButtonData("Local file", "storage:sqlite"),
			),
		)
		sendWithKeyboard(tgBot, chatID, "How do you want to store your data?", markup)
		backendName = strings.TrimPrefix(waitForCallbackChoice(tgBot, updates, chatID, "storage:sheets", "storage:sqlite"), "storage:")
		cfg.Setup.StorageBackend = &backendName
		if err := config.SaveConfig(configPath, cfg); err != nil {
			slog.Error("saving config", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return err
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
	if backendName == "sqlite" {
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
	if backendName == "sqlite" {
		// no-op: sqlite installs have no Google Sheet
	} else if cfg.Setup.GoogleSheetName != nil || cfg.Setup.GoogleSheetID != nil {
		if err := backend.ResolveSheetsSetup(cfg, serviceAccountPath); err != nil {
			send(tgBot, chatID, err.Error())
			return err
		}
		if err := config.SaveConfig(configPath, cfg); err != nil {
			send(tgBot, chatID, err.Error())
			return err
		}
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

			if _, err := sheets.NewClient(serviceAccountPath, id, ""); err != nil {
				send(tgBot, chatID, fmt.Sprintf("Couldn't connect to that sheet: %v\nMake sure it's shared with the service account (Editor access), then try again.", err))
				continue
			}

			cfg.Setup.GoogleSheetName = name
			cfg.Setup.GoogleSheetID = &id
			if err := config.SaveConfig(configPath, cfg); err != nil {
				slog.Error("saving config", "err", err)
				send(tgBot, chatID, "Something went wrong. Check logs.")
				return err
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
			return err
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
			return err
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
			return err
		}
	}

	// Step 6 — Complete.
	send(tgBot, chatID, "✅ Setup complete! Poker Tracker will start automatically on next login.\nSend /start to configure alerts and hard stop rules.")
	return nil
}

// waitForFirstMessage blocks until the first text message arrives on
// updates from allowedChatID, or any chat when it is zero, and returns its ID.
func waitForFirstMessage(updates tgbotapi.UpdatesChannel, allowedChatID int64) int64 {
	for update := range updates {
		if update.Message == nil || !privateChat(update.Message.Chat) || update.Message.Text == "" ||
			allowedChatID != 0 && update.Message.Chat.ID != allowedChatID {
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
		if update.Message == nil || !privateChat(update.Message.Chat) || update.Message.Chat.ID != chatID || update.Message.Text == "" {
			continue
		}
		return update.Message.Text
	}
	return ""
}

// waitForCallbackChoice blocks until a callback query from chatID arrives
// with Data matching one of accepted, answers it, and returns the matched
// value. Used by RunSetup's storage backend prompt, the only inline keyboard
// RunSetup shows — RunSetup runs sequentially before the bot.Run event loop
// callback machinery exists, so it needs its own blocking wait here rather
// than the Bot's handleCallback.
func waitForCallbackChoice(tgBot *tgbotapi.BotAPI, updates tgbotapi.UpdatesChannel, chatID int64, accepted ...string) string {
	for update := range updates {
		if update.CallbackQuery == nil || update.CallbackQuery.Message == nil || !privateChat(update.CallbackQuery.Message.Chat) || update.CallbackQuery.Message.Chat.ID != chatID {
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
		if update.Message == nil || !privateChat(update.Message.Chat) || update.Message.Chat.ID != chatID {
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
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building Telegram download request")
	}
	resp, err := tgBot.Client.Do(req)
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
	// The destination directory may not exist yet — a sqlite install never
	// has a credentials\ folder until /credentials uploads one so
	// /storage can migrate to sheets.
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("creating destination directory: %w", err)
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
// free text. RunSetup's blocking prompts have no
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

// setupResolvePath asks for a base PokerStars folder path (relative to the
// player's home directory, or a full path starting with a drive letter),
// validates it by checking the screen-name-joined path exists on disk, and
// loops until a valid one is given. Returns the base path (not joined).
func setupResolvePath(tgBot *tgbotapi.BotAPI, updates tgbotapi.UpdatesChannel, chatID int64, folderLabel, exampleSuffix, screenName string) string {
	send(tgBot, chatID, fmt.Sprintf("What is the base %s folder path? Do not include your screen name. Relative paths (e.g. %s) use the active Windows desktop user's home. Send a full path if no desktop user is available.", folderLabel, exampleSuffix))

	for {
		reply := readReply(tgBot, updates, chatID)
		full, err := config.ResolveCollectorPath(reply)
		if err != nil {
			send(tgBot, chatID, err.Error())
			continue
		}
		cfg := &config.Config{Setup: config.Setup{PokerStarsScreenName: &screenName}}
		if _, err := config.ResolveCollection(cfg, config.CollectorSettings{HandHistory: &full}); err != nil {
			send(tgBot, chatID, err.Error())
			continue
		}
		return full
	}
}
