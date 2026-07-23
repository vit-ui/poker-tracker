// Package bot implements the Telegram bot: outgoing session alerts and
// incoming commands for configuration and status queries.
//
// The package is split by concern: bot.go holds the Bot struct, the serve
// loop, and update dispatch; setup.go the --setup wizard and startup
// recovery; flows.go the four config flows; stats.go the read-side commands
// and scheduled summaries; alerts.go the worker-side alert evaluators;
// export.go /export; sheet.go /sheet, /credentials, /screenname;
// satellite.go the satellite/ticket confirmation flow; help.go the static
// help text.
package bot

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// Bot bundles the dependencies every serve-mode handler needs — the
// Telegram connection, the storage backend, the on-disk config/lockfile
// paths, and the watcher-restart callback main.go provides — so they aren't
// threaded hand-to-hand through every handler call. It also embeds the
// single configured chat's conversation state. Setup-mode code (RunSetup,
// EnsureSheetID) runs before a Bot exists and stays plain functions; the
// worker-side alert evaluators (alerts.go) run on the worker goroutine,
// which has no Bot, and stay plain functions too.
type Bot struct {
	tg                 *tgbotapi.BotAPI
	store              storage.Store
	configPath         string
	lockfilePath       string
	serviceAccountPath string
	dbPath             string
	restartWatcher     func()

	// pendingSwitch holds a /storage migration awaiting the player's
	// confirmation tap. Touched only from pollUpdates, like conversation.
	pendingSwitch *pendingBackendSwitch

	conversation
}

// conversation tracks the in-progress inline-keyboard flow, if any, for the
// single configured chat. It is only ever touched from the
// update-processing goroutine (pollUpdates), so no locking is needed.
type conversation struct {
	flow  string // "", "screen_name", "menu", "credentials"
	field string // field awaiting a text reply within flow, "" if none

	// menuMessageID is the message a cascade config menu currently lives in,
	// captured when a value prompt is issued (beginValueEntry) so the typed
	// reply can edit that same message back to the relevant screen instead of
	// sending a new one.
	menuMessageID int

	// resumeFlow names the flow (see ensureScreenName) to start once the
	// screen-name recovery prompt is answered.
	resumeFlow string
}

func (c *conversation) reset() {
	c.flow = ""
	c.field = ""
}

// Run starts the bot in serve mode. Forwards alerts from alertChan to
// Telegram, sends weekly/monthly summaries and satellite/ticket reminders on
// a schedule, and polls Telegram for incoming commands. Blocks until
// alertChan is closed.
//
// restartWatcher is called by the /paths and /screenname saves — it re-joins
// the base paths with the screen name and restarts the watcher goroutine.
//
// serviceAccountPath is only used by /sheet, /credentials, and /storage
// when switching to a different spreadsheet or service account. dbPath is the
// sqlite database path, used by /storage to construct a sqlite target.
func Run(botToken string, alertChan <-chan string, store storage.Store, configPath, lockfilePath, serviceAccountPath, dbPath string, restartWatcher func()) error {
	tgBot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return fmt.Errorf("starting telegram bot: %w", err)
	}

	b := &Bot{
		tg:                 tgBot,
		store:              store,
		configPath:         configPath,
		lockfilePath:       lockfilePath,
		serviceAccountPath: serviceAccountPath,
		dbPath:             dbPath,
		restartWatcher:     restartWatcher,
	}

	var lastWeeklySent, lastMonthlySent time.Time

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			if now.Weekday() == time.Monday && inSummaryWindow(now) && !sameDay(lastWeeklySent, now) {
				summary, net, err := buildWeeklySummary(b.store, now)
				if err != nil {
					slog.Error("building weekly summary", "err", err)
				} else {
					if b.summaryModeAllows(net, func(n config.Notifications) *string { return n.WeeklySummary }) {
						b.sendMessage(summary)
					}
					lastWeeklySent = now
				}
			}
			if now.Day() == 1 && inSummaryWindow(now) && !sameDay(lastMonthlySent, now) {
				summary, net, err := buildMonthlySummary(b.store, now)
				if err != nil {
					slog.Error("building monthly summary", "err", err)
				} else {
					if b.summaryModeAllows(net, func(n config.Notifications) *string { return n.MonthlySummary }) {
						b.sendMessage(summary)
					}
					lastMonthlySent = now
				}
			}
			b.checkPendingTournaments(now)
		}
	}()

	go b.pollUpdates()

	for msg := range alertChan {
		b.sendMessage(msg)
	}
	return nil
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

// emptyKeyboard is the markup that clears a message's buttons when edited in.
// It must be an empty-but-non-nil slice — NewInlineKeyboardMarkup() with no
// args produces a nil slice, which marshals to a null the Telegram API
// rejects for this field. Shared by clearKeyboard and resolveSatellite so
// that quirk is encoded once.
var emptyKeyboard = tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}

// clearKeyboard removes the inline keyboard from a message after it's been
// answered, leaving its text intact — a tapped confirmation shouldn't stay
// tappable.
func (b *Bot) clearKeyboard(chatID int64, messageID int) {
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, emptyKeyboard)
	if _, err := b.tg.Send(edit); err != nil {
		slog.Error("clearing telegram keyboard", "err", err)
	}
}

// sendMessage reads the configured Telegram chat ID and sends text to it.
// If no chat ID is configured yet, the message is silently discarded — with
// no chat there is nowhere to deliver or report it.
func (b *Bot) sendMessage(text string) {
	chatID, ok := configuredChatID(b.configPath)
	if !ok {
		return
	}
	send(b.tg, chatID, text)
}

// summaryModeAllows loads the current config and reports whether a result
// summary with the given net should go out under its configured mode (pick
// selects which summary's mode from Notifications). On a config read error it
// fails open — a transient error must never silently drop a summary.
func (b *Bot) summaryModeAllows(net float64, pick func(config.Notifications) *string) bool {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config for summary gate", "err", err)
		return true
	}
	return config.ShouldSendSummary(config.SummaryMode(pick(cfg.Notifications)), net)
}

// sendMessageWithKeyboard is sendMessage with an inline keyboard attached.
func (b *Bot) sendMessageWithKeyboard(text string, markup tgbotapi.InlineKeyboardMarkup) {
	chatID, ok := configuredChatID(b.configPath)
	if !ok {
		return
	}
	sendWithKeyboard(b.tg, chatID, text, markup)
}

// pollUpdates polls Telegram for incoming messages and callback queries and
// dispatches them. Runs in its own goroutine so it never blocks alertChan.
func (b *Bot) pollUpdates() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := b.tg.GetUpdatesChan(u)

	for update := range updates {
		// CallbackQuery.Message can be nil if the tapped button is on a
		// message Telegram no longer has (old or deleted). Every downstream
		// handler assumes a non-nil Message, so filter here before any of
		// them run — otherwise this goroutine panics and silently kills all
		// incoming command handling until the service is restarted.
		if update.CallbackQuery != nil && update.CallbackQuery.Message == nil {
			if _, err := b.tg.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, "Already resolved.")); err != nil {
				slog.Error("answering callback query", "err", err)
			}
			continue
		}
		b.handleUpdate(update)
	}
}

func (b *Bot) handleUpdate(update tgbotapi.Update) {
	var senderChatID int64
	switch {
	case update.Message != nil:
		senderChatID = update.Message.Chat.ID
	case update.CallbackQuery != nil:
		senderChatID = update.CallbackQuery.Message.Chat.ID
	default:
		return
	}

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config for incoming update", "err", err)
		return
	}
	senderChatIDStr := strconv.FormatInt(senderChatID, 10)
	if cfg.Setup.TelegramChatID == nil {
		cfg.Setup.TelegramChatID = &senderChatIDStr
		if err := config.SaveConfig(b.configPath, cfg); err != nil {
			slog.Error("saving telegram chat id", "err", err)
			return
		}
	} else if *cfg.Setup.TelegramChatID != senderChatIDStr {
		return // not the configured chat — ignore silently
	}

	if update.CallbackQuery != nil {
		b.handleCallback(update.CallbackQuery)
		return
	}

	msg := update.Message

	// A document upload has no Text — the check below would otherwise
	// silently drop it. This is the only flow that ever expects a file
	// instead of a text reply.
	if b.flow == "credentials" && b.field == "file" && msg.Document != nil {
		b.handleCredentialsDocument(msg.Chat.ID, msg.Document.FileID)
		return
	}

	if msg.Text == "" {
		return
	}

	if b.field != "" && !strings.HasPrefix(msg.Text, "/") {
		b.handleFlowReply(msg)
		return
	}

	if strings.HasPrefix(msg.Text, "/") {
		b.handleCommand(msg)
	}
}

func (b *Bot) handleFlowReply(msg *tgbotapi.Message) {
	switch b.flow {
	case "screen_name":
		b.handleScreenNameReply(msg.Chat.ID, msg.Text)
	case "menu":
		b.handleMenuReply(msg.Chat.ID, msg.Text)
	case "credentials":
		b.handleCredentialsTextReply(msg.Chat.ID, msg.Text)
	}
}

func (b *Bot) handleCallback(cb *tgbotapi.CallbackQuery) {
	if _, err := b.tg.Request(tgbotapi.NewCallback(cb.ID, "")); err != nil {
		slog.Error("answering callback query", "err", err)
	}

	chatID := cb.Message.Chat.ID
	messageID := cb.Message.MessageID
	data := cb.Data

	switch {
	case strings.HasPrefix(data, "satellite_cash:"):
		b.resolveSatellite(chatID, messageID, strings.TrimPrefix(data, "satellite_cash:"), true)
	case strings.HasPrefix(data, "satellite_ticket:"):
		b.resolveSatellite(chatID, messageID, strings.TrimPrefix(data, "satellite_ticket:"), false)
	case data == "storage:yes":
		b.resolveStorageSwitch(chatID, messageID, true)
	case data == "storage:no":
		b.resolveStorageSwitch(chatID, messageID, false)
	case strings.HasPrefix(data, "menu:"):
		b.handleMenuCallback(chatID, messageID, strings.TrimPrefix(data, "menu:"))
	}
}

// handleCommand dispatches a slash command to its handler.
func (b *Bot) handleCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	switch msg.Command() {
	case "start":
		send(b.tg, chatID, "Welcome to Poker Tracker. Let's configure your settings.\nYou can re-run /start at any time to update these settings.")
		b.startPathsFlow(chatID)
	case "deposit":
		b.handleDepositWithdraw(chatID, msg.CommandArguments(), "deposit")
	case "withdraw":
		b.handleDepositWithdraw(chatID, msg.CommandArguments(), "withdrawal")
	case "status":
		b.handleStatus(chatID)
	case "lastsession":
		b.handleLastSession(chatID)
	case "stats":
		b.handleStats(chatID, msg.CommandArguments())
	case "sheet":
		b.handleSheetCommand(chatID, msg.CommandArguments())
	case "storage":
		b.handleStorageCommand(chatID, msg.CommandArguments())
	case "export":
		b.handleExportCommand(chatID)
	case "credentials":
		b.startCredentialsFlow(chatID)
	case "screenname":
		b.handleScreenNameCommand(chatID, msg.CommandArguments())
	case "paths":
		b.startPathsFlow(chatID)
	case "alerts":
		b.startAlertsFlow(chatID)
	case "hardstop":
		b.handleHardstopCommand(chatID, msg.CommandArguments())
	case "schedule":
		b.startScheduleFlow(chatID)
	case "notify":
		b.startNotifyFlow(chatID)
	case "config":
		send(b.tg, chatID, configText)
	case "help":
		send(b.tg, chatID, helpText(msg.CommandArguments()))
	}
}
