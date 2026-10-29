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
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// apiEndpoint is the Telegram Bot API root every bot connection dials. It is
// always the real endpoint in any committed build; the only thing that ever
// reassigns it is this package's own local, gitignored tests (*_local_test.go),
// which point it at an httptest server so the setup wizard and the incoming-
// command handlers can be driven without a real bot. This mirrors the sheets
// REST transport's baseURL test seam — an inert var whose default is production.
var apiEndpoint = tgbotapi.APIEndpoint

// Bot bundles the dependencies every serve-mode handler needs — the
// Telegram connection, the storage backend, the on-disk config/lockfile
// paths, and the watcher-restart callback main.go provides — so they aren't
// threaded hand-to-hand through every handler call. It also embeds the
// single configured chat's conversation state. Setup-mode code (RunSetup,
// EnsureSheetID) runs before a Bot exists and stays plain functions; the
// worker-side alert evaluators (alerts.go) run on the worker goroutine,
// which has no Bot, and stay plain functions too.
type Bot struct {
	updateID           int
	migrating          bool
	worker             chan<- storage.Operation
	pendingOperations  []pendingOperation
	taskName           string
	tg                 *tgbotapi.BotAPI
	store              storage.Store
	configPath         string
	lockfilePath       string
	serviceAccountPath string
	dbPath             string
	restartWatcher     func()

	// sendLock carries out a manual /hardstop trigger's lock. The caller
	// supplies it so the bot doesn't care where enforcement happens: the
	// all-local build passes the same inline client.Enforce it always used
	// (unchanged behavior), the split server passes a send down the link to the
	// remote collector. The bot only builds the lock and hands it over.
	sendLock func(config.LockFile)

	// requestRescan triggers a /scan backfill. Like sendLock, the caller
	// supplies it: both builds send a CommandRescan down the link, which the
	// collector (in-process or remote) turns into a walk of the watched folders.
	// The bot only asks; the collector does the walk and the server records.
	requestRescan func()

	// now returns the time the read-side date windows (/status, /stats, weekly
	// and monthly summaries) anchor to. The all-local build passes time.Now; the
	// split server passes its client-anchored clock so windows follow the
	// player's local day even when the server is in another timezone.
	now func() time.Time

	// pendingSwitch holds a /storage migration awaiting the player's
	// confirmation tap. Touched only on the bot event loop, like conversation.
	pendingSwitch *pendingBackendSwitch

	// pendingStorageSheetArg holds the sheet name/URL from a /storage sheets
	// migration that paused to collect the service-account key inline, resumed
	// once the file is uploaded. Touched only on the bot event loop.
	pendingStorageSheetArg string

	conversation
}

// conversation tracks the in-progress inline-keyboard flow, if any, for the
// single configured chat. It is only ever touched from the
// bot event loop, so no locking is needed.
type conversation struct {
	flow  string // "", "screen_name", "menu", "credentials", "storage_credentials", "freeroll_buyin"
	field string // field awaiting a text reply within flow, "" if none (freeroll_buyin: the tournament ID)

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
// taskName identifies the role/installation Windows task or Linux user unit for restarts.
func Run(botToken string, alertChan <-chan string, store storage.Store, configPath, lockfilePath, serviceAccountPath, dbPath string, restartWatcher func(), sendLock func(config.LockFile), requestRescan func(), now func() time.Time, taskName string, worker chan<- storage.Operation) error {
	tgBot, err := tgbotapi.NewBotAPIWithAPIEndpoint(botToken, apiEndpoint)
	if err != nil {
		return fmt.Errorf("starting telegram bot: %w", err)
	}

	b := &Bot{
		worker:             worker,
		taskName:           taskName,
		tg:                 tgBot,
		store:              store,
		configPath:         configPath,
		lockfilePath:       lockfilePath,
		serviceAccountPath: serviceAccountPath,
		dbPath:             dbPath,
		restartWatcher:     restartWatcher,
		sendLock:           sendLock,
		requestRescan:      requestRescan,
		now:                now,
	}

	var lastWeeklySent, lastMonthlySent, lastHeartbeatCheck time.Time
	var sheetsDown bool

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := tgBot.GetUpdatesChan(u)
	defer tgBot.StopReceivingUpdates()
	var active *pendingOperation
	var results <-chan storage.OperationResult
	for {
		var outbound chan<- storage.Operation
		var request storage.Operation
		if active == nil && len(b.pendingOperations) > 0 {
			outbound = b.worker
			request = b.pendingOperations[0].request
		}
		select {
		case outbound <- request:
			next := b.pendingOperations[0]
			b.pendingOperations[0] = pendingOperation{}
			b.pendingOperations = b.pendingOperations[1:]
			active = &next
			results = next.request.Reply
		case result := <-results:
			complete := active.complete
			active = nil
			results = nil
			complete(result)
		case update, ok := <-updates:
			if !ok {
				return fmt.Errorf("telegram update stream closed")
			}
			b.handleUpdate(update)
		case msg, ok := <-alertChan:
			if !ok {
				return nil
			}
			b.sendMessage(msg)
		case <-ticker.C:
			if b.migrating {
				continue
			}
			// realNow drives real-elapsed timing (the 30-min heartbeat, pending
			// reminders); now is the player-anchored clock the weekly/monthly
			// day windows fire on, so "Monday 09:00" is the player's, not the
			// server host's.
			if active == nil && len(b.pendingOperations) == 0 {
				if client, ok := b.store.(*sheets.Client); ok && client.PendingWrites() > 0 {
					b.queueOperation(func(store storage.Store) storage.OperationResult {
						client := store.(*sheets.Client)
						before := client.PendingWrites()
						client.DrainPendingWrites()
						return storage.OperationResult{Recovered: before - client.PendingWrites()}
					}, func(result storage.OperationResult) {
						if result.Recovered > 0 {
							b.sendMessage(fmt.Sprintf("✅ Synced %d queued write(s) to Sheets", result.Recovered))
						}
					})
				}
			}
			realNow := time.Now()
			now := b.now()
			if realNow.Sub(lastHeartbeatCheck) >= 30*time.Minute {
				lastHeartbeatCheck = realNow
				if sheetsSrc, ok := b.store.(*sheets.Client); ok {
					down, since, herr := sheetsSrc.Health()
					msg, nowDown := heartbeatEvent(down, since, herr, sheetsDown)
					sheetsDown = nowDown
					if msg != "" {
						b.sendMessage(msg)
					}
				}
			}
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
			b.checkPendingTournaments(realNow)
		}
	}
}

// heartbeatEvent decides what (if anything) to send given the latest
// Health() reading and whether Sheets was already known down. Every 30-min
// check while still down re-sends the down message (a persistent reminder,
// matching the caller's check cadence); one recovery message fires on the
// down→healthy transition; an already-healthy check stays silent. The raw
// Health() error is deliberately not shown to the user — it's a developer
// string, and the failing operation already logged it at its call site; the
// user-facing message carries only the outage's start time.
func heartbeatEvent(down bool, since time.Time, err error, wasDown bool) (msg string, nowDown bool) {
	switch {
	case down:
		return fmt.Sprintf("💔 Sheets unreachable since %s", since.Local().Format("2006-01-02 15:04")), true
	case wasDown:
		return "💓 Sheets reachable again", false
	default:
		return "", false
	}
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

func (b *Bot) handleUpdate(update tgbotapi.Update) {
	b.updateID = update.UpdateID
	if update.CallbackQuery != nil && update.CallbackQuery.Message == nil {
		if _, err := b.tg.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, "Already resolved.")); err != nil {
			slog.Error("answering callback query", "err", err)
		}
		return
	}
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

	if b.migrating {
		if update.CallbackQuery != nil {
			if _, err := b.tg.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, "Storage migration is in progress. Try again after restart.")); err != nil {
				slog.Error("answering callback during migration", "err", err)
			}
		} else {
			send(b.tg, senderChatID, "Storage migration is in progress. This command was not accepted; try again after restart.")
		}
		return
	}

	if update.CallbackQuery != nil {
		b.handleCallback(update.CallbackQuery)
		return
	}

	msg := update.Message

	// A document upload has no Text — the check below would otherwise
	// silently drop it. This is the only flow that ever expects a file
	// instead of a text reply.
	if b.field == "file" && msg.Document != nil {
		switch b.flow {
		case "credentials":
			b.handleCredentialsDocument(msg.Chat.ID, msg.Document.FileID)
			return
		case "storage_credentials":
			b.handleStorageCredentialsDocument(msg.Chat.ID, msg.Document.FileID)
			return
		}
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
	case "storage_credentials":
		b.handleStorageCredentialsText(msg.Chat.ID, msg.Text)
	case "freeroll_buyin":
		b.handleFreerollBuyInReply(msg.Chat.ID, msg.Text)
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
	case strings.HasPrefix(data, "freeroll_free:"):
		b.recordFreerollEntry(chatID, messageID, strings.TrimPrefix(data, "freeroll_free:"), 0)
	case strings.HasPrefix(data, "freeroll_paid:"):
		b.beginFreerollBuyInEntry(chatID, messageID, strings.TrimPrefix(data, "freeroll_paid:"))
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
		send(b.tg, chatID, "Welcome to Poker Tracker. Use the menu below any time — or type a command directly.")
		b.startMenuFlow(chatID)
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
	case "scan":
		b.handleScanCommand(chatID)
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
	case "menu":
		b.startMenuFlow(chatID)
	case "config":
		send(b.tg, chatID, configText)
	case "help":
		send(b.tg, chatID, helpText(msg.CommandArguments()))
	}
}
