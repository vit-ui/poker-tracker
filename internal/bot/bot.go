package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/update"
)

var apiEndpoint = tgbotapi.APIEndpoint

type Bot struct {
	updater            *update.Agent
	releaseResults     chan releaseCheckResult
	checkingRelease    bool
	lastReleaseCheck   time.Time
	latestVersion      string
	sendLock           func(config.LockFile)
	updateID           int
	migrating          bool
	restarting         bool
	ownerPaused        bool
	worker             chan<- storage.Operation
	pendingOperations  []pendingOperation
	taskName           string
	tg                 *tgbotapi.BotAPI
	chatID             int64
	store              storage.Store
	configPath         string
	lockStatusText     func() (string, error)
	currentLock        func() (*config.LockFile, error)
	serviceAccountPath string
	dbPath             string

	requestRescan  func()
	fileScanActive bool
	menus          map[menuKey]*openMenu

	now func() time.Time

	pendingSwitch         *pendingBackendSwitch
	storageResetMessageID int

	pendingStorageSheetArg string

	conversation
}

type conversation struct {
	flow  string
	field string

	menuMessageID       int
	recovery            *config.PendingTournament
	recoveryPayoutCents *money.Cents
}

func (c *conversation) reset() {
	c.flow = ""
	c.field = ""
	c.menuMessageID = 0
	c.recovery = nil
	c.recoveryPayoutCents = nil
}

func Run(alertChan <-chan string, store storage.Store, configPath, serviceAccountPath, dbPath string, sendLock func(config.LockFile), requestRescan func(), now func() time.Time, taskName string, worker chan<- storage.Operation, currentLock func() (*config.LockFile, error), stop <-chan struct{}, drained chan<- struct{}, controls <-chan OwnerControl, standby bool, notifications ClosureNotifications, lockStatusText func() (string, error), ready chan<- struct{}, updater *update.Agent, releaseChecks <-chan struct{}) error {

	b := &Bot{
		updater:            updater,
		releaseResults:     make(chan releaseCheckResult, 1),
		worker:             worker,
		taskName:           taskName,
		store:              store,
		configPath:         configPath,
		currentLock:        currentLock,
		lockStatusText:     lockStatusText,
		serviceAccountPath: serviceAccountPath,
		dbPath:             dbPath,
		sendLock:           sendLock,
		requestRescan:      requestRescan,
		now:                now,
	}

	var sender telegramSender
	sender.resume()
	defer sender.interrupt()
	ownerDone := make(chan struct{})
	defer close(ownerDone)
	controls, stop = sender.watchOwner(controls, stop, ownerDone)

	var lastWeeklySent, lastMonthlySent, lastHeartbeatCheck time.Time
	var sheetsDown bool

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	menuTimer := time.NewTimer(0)
	menuTimer.Stop()
	defer menuTimer.Stop()
	defer func() {
		for key := range b.menus {
			b.closeMenu(key.chatID, key.messageID)
		}
	}()
	var poller *updatePoller
	var updates <-chan tgbotapi.Update
	var connection telegramConnection
	reconnect := time.NewTicker(3 * time.Second)
	defer reconnect.Stop()
	var cancelProbe context.CancelFunc
	var candidates <-chan telegramConnection
	var probing telegramCredentials
	stopProbe := func() {
		if cancelProbe != nil {
			cancelProbe()
		}
		cancelProbe, candidates = nil, nil
	}
	defer stopProbe()
	connect := func() {
		credentials, err := loadTelegramCredentials(configPath)
		if candidates != nil && credentials == probing && err == nil {
			return
		}
		stopProbe()
		result := make(chan telegramConnection, 1)
		candidates, probing = result, credentials
		if err != nil || credentials == connection.credentials || credentials.token == "" {
			result <- telegramConnection{credentials: credentials, needsSetup: credentials.token == "" && err == nil, err: err}
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancelProbe = cancel
		go func() {
			result <- probeTelegram(ctx, credentials)
		}()
	}
	b.ownerPaused = standby
	pollingPaused := standby
	nextUpdate := 0
	stopPoller := func() {
		if poller != nil {
			poller.stop()
			poller, updates = nil, nil
		}
	}
	startPoller := func() {
		if b.tg != nil && poller == nil && !pollingPaused {
			poller = startUpdatePoller(b.tg, nextUpdate)
			updates = poller.updates
		}
	}
	defer stopPoller()
	if !standby && updater != nil {
		b.checkRelease(releaseCheckResult{})
	}
	connect()
	if ready != nil {
		ready <- struct{}{}
	}
	var active *pendingOperation
	var results <-chan storage.OperationResult
	stopping, notified, setupPending := false, false, false
	var pausedReply chan error
	var closureResult *ClosureResult
	for {
		if b.restarting {
			stopping = true
		}
		if stopping {
			stopPoller()
			stopProbe()
			reconnect.Stop()
			ticker.Stop()
			stop = nil
		}
		if stopping && !notified && active == nil && len(b.pendingOperations) == 0 {
			if drained != nil {
				close(drained)
			}
			notified = true
		}
		if pausedReply != nil && active == nil && len(b.pendingOperations) == 0 {
			pausedReply <- nil
			pausedReply = nil
		}
		var outbound chan<- storage.Operation
		var request storage.Operation
		if active == nil && len(b.pendingOperations) > 0 {
			outbound = b.worker
			request = b.pendingOperations[0].request
		}
		var closureResults chan ClosureResult
		var releaseResults <-chan releaseCheckResult
		var pokerStarsChecks <-chan struct{}
		if !stopping {
			releaseResults = b.releaseResults
			if b.updater != nil && !b.ownerPaused && !b.migrating && !b.checkingRelease {
				pokerStarsChecks = releaseChecks
			}
		}
		var closureReply ClosureResult
		closures := notifications.Deliveries
		if closureResult != nil {
			closures = nil
			closureResults, closureReply = notifications.Results, *closureResult
		}
		var menuExpiry <-chan time.Time
		if next := b.nextMenuExpiry(); !next.IsZero() {
			menuTimer.Reset(time.Until(next))
			menuExpiry = menuTimer.C
		} else {
			menuTimer.Stop()
		}
		select {
		case <-reconnect.C:
			connect()
		case next := <-candidates:
			stopProbe()
			if stopping {
				continue
			}
			if next.needsSetup && !standby {
				if !setupPending {
					slog.Warn(telegramSetupPending())
				}
				setupPending = true
			} else if next.err == nil {
				setupPending = false
			}
			if next.err != nil {
				if !connection.failed {
					slog.Error("connecting Telegram", "err", next.err)
				}
				connection.failed = true
				continue
			}
			connection.failed = false
			if next.credentials == connection.credentials {
				startPoller()
				continue
			}
			if active != nil || len(b.pendingOperations) > 0 {
				continue
			}
			stopPoller()
			b.reset()
			b.menus = nil
			if b.pendingSwitch != nil {
				closeStore(b.pendingSwitch.dst)
			}
			b.pendingSwitch, b.storageResetMessageID, b.pendingStorageSheetArg = nil, 0, ""
			connection = next
			b.tg, b.chatID = next.bot, next.credentials.chatID
			if b.tg != nil {
				b.tg.Client = &sender
			}
			nextUpdate = 0
			startPoller()
		case <-menuExpiry:
			b.expireMenus(time.Now())
		case result := <-releaseResults:
			b.finishReleaseCheck(result)
		case <-pokerStarsChecks:
			b.checkRelease(releaseCheckResult{})
		case event := <-closures:
			result := b.sendClosure(event)
			closureResult = &result
		case closureResults <- closureReply:
			closureResult = nil
		case control := <-controls:
			select {
			case <-stop:
				stopping = true
			default:
				sender.resume()
			}
			if control.Pause {
				if b.migrating || pausedReply != nil {
					control.Reply <- fmt.Errorf("a configuration operation is already in progress")
				} else {
					b.ownerPaused = true
					if control.StopPolling {
						pollingPaused = true
						stopPoller()
					}
					pausedReply = control.Reply
				}
			} else {
				b.ownerPaused = false
				pollingPaused = false
				standby = false
				if b.updater != nil && b.lastReleaseCheck.IsZero() {
					b.checkRelease(releaseCheckResult{})
				}
				startPoller()
				control.Reply <- nil
			}
			continue
		case <-stop:
			stopping = true
			continue
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
				stopPoller()
				continue
			}
			nextUpdate = update.UpdateID + 1
			b.handleUpdate(update)
		case msg, ok := <-alertChan:
			if !ok {
				return nil
			}
			if strings.HasPrefix(msg, storage.ScanReportPrefix) {
				b.sendScanReport(msg)
			} else {
				b.sendMessage(msg)
			}
		case <-ticker.C:
			if b.migrating || b.ownerPaused || b.deploymentPaused() {
				continue
			}
			if active == nil && len(b.pendingOperations) == 0 {
				b.queueOperation(func(store storage.Store) storage.OperationResult {
					if client, ok := store.(*sheets.Client); ok && client.PendingWrites() > 0 {
						before := client.PendingWrites()
						client.DrainPendingWrites()
						return storage.OperationResult{Recovered: before - client.PendingWrites()}
					}
					return storage.OperationResult{}
				}, func(result storage.OperationResult) {
					if result.Recovered > 0 {
						b.sendMessage(fmt.Sprintf("✅ Synced %d queued write(s) to Sheets", result.Recovered))
					}
				})
				request := &b.pendingOperations[len(b.pendingOperations)-1].request
				request.FinalizeCash = true
				request.RetryHardstops = true
			}

			if b.updater != nil {
				b.queueUpdates(nil, func(result storage.OperationResult) { b.observeUpdates(result) })
				if time.Since(b.lastReleaseCheck) >= 24*time.Hour {
					b.checkRelease(releaseCheckResult{})
				}
			}

			if b.tg == nil {
				continue
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
				summary, netCents, err := buildWeeklySummary(b.store, now)
				if err != nil {
					slog.Error("building weekly summary", "err", err)
				} else {
					if b.summaryModeAllows(netCents, func(n config.Notifications) *string { return n.WeeklySummary }) {
						b.sendMessage(summary)
					}
					lastWeeklySent = now
				}
			}
			if now.Day() == 1 && inSummaryWindow(now) && !sameDay(lastMonthlySent, now) {
				summary, netCents, err := buildMonthlySummary(b.store, now)
				if err != nil {
					slog.Error("building monthly summary", "err", err)
				} else {
					if b.summaryModeAllows(netCents, func(n config.Notifications) *string { return n.MonthlySummary }) {
						b.sendMessage(summary)
					}
					lastMonthlySent = now
				}
			}
			b.checkPendingTournaments(realNow)
		}
	}
}

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

func (b *Bot) connectedChat() (int64, bool) {
	return b.chatID, b.tg != nil && b.chatID > 0
}

func send(tgBot *tgbotapi.BotAPI, chatID int64, text string) {
	if tgBot == nil {
		return
	}
	if _, err := tgBot.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		slog.Error("sending telegram message", "err", err)
	}
}

func sendWithKeyboard(tgBot *tgbotapi.BotAPI, chatID int64, text string, markup tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = markup
	if _, err := tgBot.Send(msg); err != nil {
		slog.Error("sending telegram message", "err", err)
	}
}

var emptyKeyboard = tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}

func (b *Bot) clearKeyboard(chatID int64, messageID int) {
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, emptyKeyboard)
	if _, err := b.tg.Send(edit); err != nil {
		slog.Error("clearing telegram keyboard", "err", err)
	}
}

func (b *Bot) sendMessage(text string) {
	chatID, ok := b.connectedChat()
	if !ok {
		return
	}
	send(b.tg, chatID, text)
}

func (b *Bot) summaryModeAllows(netCents money.Cents, pick func(config.Notifications) *string) bool {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config for summary gate", "err", err)
		return true
	}
	return config.ShouldSendSummary(config.SummaryMode(pick(cfg.Notifications)), netCents)
}

func (b *Bot) sendMessageWithKeyboard(text string, markup tgbotapi.InlineKeyboardMarkup) {
	chatID, ok := b.connectedChat()
	if !ok {
		return
	}
	sendWithKeyboard(b.tg, chatID, text, markup)
}

func (b *Bot) handleUpdate(update tgbotapi.Update) {
	b.updateID = update.UpdateID
	var chat *tgbotapi.Chat
	switch {
	case update.Message != nil:
		chat = update.Message.Chat
	case update.CallbackQuery != nil && update.CallbackQuery.Message != nil:
		chat = update.CallbackQuery.Message.Chat
	}
	if !privateChat(chat) || chat.ID != b.chatID {
		return
	}
	senderChatID := chat.ID

	if b.migrating || b.ownerPaused || b.deploymentPaused() {
		if msg := update.Message; msg != nil && msg.IsCommand() && msg.Command() == "hardstop" && manualHardstopRequested(msg.CommandArguments()) {
			b.handleHardstopCommand(senderChatID, msg.CommandArguments())
			return
		}
		if update.CallbackQuery != nil {
			if _, err := b.tg.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, "A configuration change is in progress. Try again after restart.")); err != nil {
				slog.Error("answering callback during migration", "err", err)
			}
		} else {
			send(b.tg, senderChatID, "A configuration change is in progress. This command was not accepted; try again after restart.")
		}
		return
	}

	b.expireMenus(time.Now())
	if update.CallbackQuery != nil {
		b.handleCallback(update.CallbackQuery)
		return
	}

	msg := update.Message

	if msg.Document != nil {
		b.refreshMenuPrompt(msg.Chat.ID)
	}
	if msg.Document != nil && b.flow == "scan" && b.field == "upload" {
		b.handleScanAttachment(msg.Chat.ID, msg.Document)
		return
	}
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
		b.refreshMenuPrompt(msg.Chat.ID)
		b.handleFlowReply(msg)
		return
	}

	if strings.HasPrefix(msg.Text, "/") {
		b.handleCommand(msg)
	}
}

func (b *Bot) handleFlowReply(msg *tgbotapi.Message) {
	switch b.flow {
	case "menu":
		b.handleMenuReply(msg.Chat.ID, msg.Text)
	case "credentials":
		b.handleCredentialsTextReply(msg.Chat.ID, msg.Text)
	case "storage_credentials":
		b.handleStorageCredentialsText(msg.Chat.ID, msg.Text)
	case "scan":
		b.handleScanReply(msg.Chat.ID, msg.Text)
	case "freeroll_buyin":
		b.handleFreerollBuyInReply(msg.Chat.ID, msg.Text)
	case "missing_result":
		b.handleRecoveryReply(msg.Chat.ID, msg.Text)
	}
}

func (b *Bot) handleCallback(cb *tgbotapi.CallbackQuery) {
	chatID := cb.Message.Chat.ID
	messageID := cb.Message.MessageID
	data := cb.Data
	key := menuKey{chatID, messageID}
	stale := menuCallback(data) && !b.acceptMenuCallback(key, data)
	answer := ""
	if stale {
		answer = "This menu or button has expired."
	}
	if _, err := b.tg.Request(tgbotapi.NewCallback(cb.ID, answer)); err != nil {
		slog.Error("answering callback query", "err", err)
	}
	if stale {
		if b.menus[key] == nil {
			_, _ = b.tg.Request(tgbotapi.NewDeleteMessage(chatID, messageID))
		}
		return
	}

	switch {
	case data == "missing_results":
		b.reviewMissingResults(chatID, "")
	case strings.HasPrefix(data, "missing_result:"):
		b.reviewMissingResults(chatID, strings.TrimPrefix(data, "missing_result:"))
	case strings.HasPrefix(data, "scan:"):
		b.handleScanChoice(chatID, messageID, strings.TrimPrefix(data, "scan:"))
	case strings.HasPrefix(data, "transactions:"):
		b.handleTransactionsCallback(chatID, messageID, strings.TrimPrefix(data, "transactions:"))
	case strings.HasPrefix(data, "update:check:"):
		b.handleUpdateAction(chatID, messageID, strings.TrimPrefix(data, "update:check:"), "check")
	case strings.HasPrefix(data, "update:"):
		b.handleUpdateCommand(chatID, strings.TrimPrefix(data, "update:"))
	case strings.HasPrefix(data, "satellite_cash:"):
		b.resolveSatellite(chatID, messageID, strings.TrimPrefix(data, "satellite_cash:"), true)
	case strings.HasPrefix(data, "satellite_ticket:"):
		b.resolveSatellite(chatID, messageID, strings.TrimPrefix(data, "satellite_ticket:"), false)
	case strings.HasPrefix(data, "freeroll_free:"):
		b.recordFreerollEntry(chatID, messageID, strings.TrimPrefix(data, "freeroll_free:"), 0)
	case strings.HasPrefix(data, "freeroll_paid:"):
		b.beginFreerollBuyInEntry(chatID, messageID, strings.TrimPrefix(data, "freeroll_paid:"))
	case data == "storage:reset:yes" || data == "storage:reset:no":
		b.resolveStorageReset(chatID, messageID, data == "storage:reset:yes")
	case data == "storage:yes":
		b.resolveStorageSwitch(chatID, messageID, true)
	case data == "storage:no":
		b.resolveStorageSwitch(chatID, messageID, false)
	case strings.HasPrefix(data, "menu:"):
		b.handleMenuCallback(chatID, messageID, strings.TrimPrefix(data, "menu:"))
	}
}

func (b *Bot) handleCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	switch msg.Command() {
	case "update":
		b.handleUpdateCommand(chatID, msg.CommandArguments())
	case "start":
		send(b.tg, chatID, "Welcome to Poker Tracker. Use the menu below any time — or type a command directly.")
		b.startMenuFlow(chatID)
	case "reward":
		b.handleMoneyMovement(chatID, msg.CommandArguments(), "reward")
	case "transactions":
		b.handleTransactionsCommand(chatID, msg.CommandArguments())
	case "deposit":
		b.handleMoneyMovement(chatID, msg.CommandArguments(), "deposit")
	case "withdraw":
		b.handleMoneyMovement(chatID, msg.CommandArguments(), "withdrawal")
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
		switch strings.TrimSpace(msg.CommandArguments()) {
		case "":
			b.handleScanCommand(chatID)
		case "file":
			b.renderScanMenu(chatID, 0, "scan")
		default:
			send(b.tg, chatID, "Use /scan to scan folders or /scan file for a single file.")
		}
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
