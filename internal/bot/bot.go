// Package bot implements the Telegram bot: outgoing session alerts and
// incoming commands for configuration and status queries.
package bot

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sheets"
)

// Run starts the bot in serve mode. Forwards alerts from alertChan to
// Telegram, sends weekly/monthly summaries and satellite/ticket reminders on
// a schedule, and polls Telegram for incoming commands. Blocks until
// alertChan is closed.
//
// restartWatcher is called by the /paths command once new folder paths are
// saved — it re-joins the base paths with the screen name and restarts the
// watcher goroutine.
func Run(botToken string, alertChan <-chan string, sheetsClient *sheets.Client, configPath, lockfilePath string, restartWatcher func()) error {
	tgBot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return fmt.Errorf("starting telegram bot: %w", err)
	}
	slog.Info("telegram bot ready", "username", tgBot.Self.UserName)

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

	go pollUpdates(tgBot, sheetsClient, configPath, lockfilePath, restartWatcher)

	for msg := range alertChan {
		sendMessage(tgBot, configPath, msg)
	}
	return nil
}

// RunSetup starts the bot in setup mode and blocks until the user completes
// the configuration wizard via Telegram. Used by --setup mode only; the
// full conversation flow is implemented alongside the installer (build
// order part 10).
func RunSetup(token, configPath, serviceAccountPath string) {
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
		slog.Info("no chat ID configured yet — message the bot once, or set telegram_chat_id manually in config.json for testing")
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
func buildWeeklySummary(sc *sheets.Client, now time.Time) (string, error) {
	since := now.AddDate(0, 0, -7)
	cashSessions, err := sc.GetCashSessionsSince(since)
	if err != nil {
		return "", fmt.Errorf("loading cash sessions: %w", err)
	}
	tournaments, err := sc.GetTournamentsSince(since)
	if err != nil {
		return "", fmt.Errorf("loading tournaments: %w", err)
	}

	var totalHands int
	var netUSD float64
	for _, s := range cashSessions {
		totalHands += s.Hands
		netUSD += s.NetUSD
	}
	for _, t := range tournaments {
		netUSD += t.NetUSD
	}
	sessionCount := len(cashSessions) + len(tournaments)

	return fmt.Sprintf(
		"📊 Weekly Summary\nSessions: %d\nNet: %s\nBB/100: %.1f\nSpin & Go ROI: %s\nMTT ROI: %s\nHands: %s",
		sessionCount,
		formatSignedUSD(netUSD),
		weightedBB100(cashSessions),
		formatSignedPercent(roi(tournaments, "spingo")),
		formatSignedPercent(roi(tournaments, "mtt")),
		commas(totalHands),
	), nil
}

// buildMonthlySummary formats the previous calendar month's activity.
func buildMonthlySummary(sc *sheets.Client, now time.Time) (string, error) {
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
		if t.Type == "spingo" {
			spinsNet += t.NetUSD
		} else {
			mttNet += t.NetUSD
		}
	}
	sessionCount := len(cashSessions) + len(tournaments)

	return fmt.Sprintf(
		"📅 Monthly Summary — %s\nCash Net: %s\nSpins Net: %s\nMTT Net: %s\nTotal: %s\nBB/100: %.1f\nSpins ROI: %s\nMTT ROI: %s\nHands: %s\nVolume: %d sessions",
		firstOfPrevMonth.Format("January 2006"),
		formatSignedUSD(cashNet),
		formatSignedUSD(spinsNet),
		formatSignedUSD(mttNet),
		formatSignedUSD(cashNet+spinsNet+mttNet),
		weightedBB100(cashSessions),
		formatSignedPercent(roi(tournaments, "spingo")),
		formatSignedPercent(roi(tournaments, "mtt")),
		commas(totalHands),
		sessionCount,
	), nil
}

// weightedBB100 returns the hands-weighted average BB/100 across sessions.
func weightedBB100(sessions []parsers.CashSession) float64 {
	var totalHands int
	var weightedSum float64
	for _, s := range sessions {
		totalHands += s.Hands
		weightedSum += s.BB100 * float64(s.Hands)
	}
	if totalHands == 0 {
		return 0
	}
	return weightedSum / float64(totalHands)
}

// roi returns net / buy-in as a percentage across tournaments of the given type.
func roi(tournaments []parsers.Tournament, tType string) float64 {
	var netSum, buyinSum float64
	for _, t := range tournaments {
		if t.Type != tType {
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

func sendSatelliteMenu(tgBot *tgbotapi.BotAPI, configPath string, t parsers.Tournament) {
	text := fmt.Sprintf("🎫 Satellite/qualifier detected\nTournament #%s\n1st place won: %s\nDid you pay cash for your entry, or use a ticket?",
		t.TournamentID, t.WinnerPrizeDescription)
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
	flow  string // "", "paths", "alerts", "hardstop"
	field string // field awaiting a text reply within flow, "" if none

	// pathsFirstTimeSetup is captured when the /paths flow starts, recording
	// whether both paths were null at that point — used to pick between the
	// "watcher restarted" and "tracker now active" messages on Done.
	pathsFirstTimeSetup bool
}

func (s *conversationState) reset() {
	s.flow = ""
	s.field = ""
}

// pollUpdates polls Telegram for incoming messages and callback queries and
// dispatches them. Runs in its own goroutine so it never blocks alertChan.
func pollUpdates(tgBot *tgbotapi.BotAPI, sc *sheets.Client, configPath, lockfilePath string, restartWatcher func()) {
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
		state.handleUpdate(tgBot, update, sc, configPath, lockfilePath, restartWatcher)
	}
}

func (s *conversationState) handleUpdate(tgBot *tgbotapi.BotAPI, update tgbotapi.Update, sc *sheets.Client, configPath, lockfilePath string, restartWatcher func()) {
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
	if msg.Text == "" {
		return
	}

	if s.field != "" && !strings.HasPrefix(msg.Text, "/") {
		s.handleFlowReply(tgBot, msg, configPath)
		return
	}

	if strings.HasPrefix(msg.Text, "/") {
		s.handleCommand(tgBot, msg, sc, configPath, lockfilePath, restartWatcher)
	}
}

func (s *conversationState) handleFlowReply(tgBot *tgbotapi.BotAPI, msg *tgbotapi.Message, configPath string) {
	switch s.flow {
	case "paths":
		s.handlePathsReply(tgBot, msg.Chat.ID, msg.Text, configPath)
	case "alerts":
		s.handleAlertsReply(tgBot, msg.Chat.ID, msg.Text, configPath)
	case "hardstop":
		s.handleHardstopReply(tgBot, msg.Chat.ID, msg.Text, configPath)
	case "schedule":
		s.handleScheduleReply(tgBot, msg.Chat.ID, msg.Text, configPath)
	}
}

func (s *conversationState) handleCallback(tgBot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery, sc *sheets.Client, configPath, lockfilePath string, restartWatcher func()) {
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
		s.handleAlertsCallback(tgBot, chatID, strings.TrimPrefix(data, "alerts:"))
	case strings.HasPrefix(data, "hardstop:"):
		s.handleHardstopCallback(tgBot, chatID, strings.TrimPrefix(data, "hardstop:"))
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
func resolveSatellite(tgBot *tgbotapi.BotAPI, chatID int64, messageID int, tournamentID string, paidCash bool, sc *sheets.Client, configPath string) {
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
		if err := sc.AppendTournament(t); err != nil {
			slog.Error("appending tournament", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}
		resultText = "✅ Logged as cash entry."
	} else {
		t.BuyInUSD, t.NetUSD = RecoverTicketPayout(t.BuyInUSD, t.NetUSD)
		if err := sc.AppendTournament(t); err != nil {
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
func (s *conversationState) handleCommand(tgBot *tgbotapi.BotAPI, msg *tgbotapi.Message, sc *sheets.Client, configPath, lockfilePath string, restartWatcher func()) {
	chatID := msg.Chat.ID
	switch msg.Command() {
	case "start":
		send(tgBot, chatID, "Setup wizard coming soon.")
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
		send(tgBot, chatID, sc.GetSheetURL())
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
func handleDepositWithdraw(tgBot *tgbotapi.BotAPI, chatID int64, args string, sc *sheets.Client, kind string) {
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

func netSince(sc *sheets.Client, since time.Time) (float64, error) {
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
func handleStatus(tgBot *tgbotapi.BotAPI, chatID int64, sc *sheets.Client, lockfilePath string) {
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

// handleLastSession implements /lastsession. The Bankroll Ledger alone
// doesn't carry enough detail (stake, hands, buy-in, finish, ...) to
// reproduce the post-session summary format, so this pulls the most recent
// row from Cash Sessions and Tournaments directly and picks whichever is
// more recent.
func handleLastSession(tgBot *tgbotapi.BotAPI, chatID int64, sc *sheets.Client) {
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
		send(tgBot, chatID, fmt.Sprintf("✅ Cash Session\nStake: %s\nNet: %s\nHands: %d\nDuration: %d min\nBankroll: $%.2f",
			lastCash.Stake, formatSignedUSD(lastCash.NetUSD), lastCash.Hands, lastCash.DurationMin, bankroll))
		return
	}

	if lastTourn.Type == "spingo" {
		mult := 0.0
		if lastTourn.Multiplier != nil {
			mult = *lastTourn.Multiplier
		}
		send(tgBot, chatID, fmt.Sprintf("🎰 Spin & Go\nBuy-in: $%.2f\nPrize pool: $%.2f (%gx)\nFinish: %s\nNet: %s\nBankroll: $%.2f",
			lastTourn.BuyInUSD, lastTourn.PrizePoolUSD, mult, ordinal(lastTourn.FinishPos), formatSignedUSD(lastTourn.NetUSD), bankroll))
		return
	}
	send(tgBot, chatID, fmt.Sprintf("🏆 MTT\nBuy-in: $%.2f\nFinish: %s / %d\nNet: %s\nBankroll: $%.2f",
		lastTourn.BuyInUSD, ordinal(lastTourn.FinishPos), lastTourn.TotalPlayers, formatSignedUSD(lastTourn.NetUSD), bankroll))
}

// handleStats implements /stats.
func handleStats(tgBot *tgbotapi.BotAPI, chatID int64, args string, sc *sheets.Client) {
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

func cashStatsMessage(sc *sheets.Client, since time.Time, periodLabel string) (string, error) {
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
	return fmt.Sprintf("📈 Cash Stats (%s)\nNet: %s\nBB/100: %.1f\nHands: %d\nSessions: %d",
		periodLabel, formatSignedUSD(net), weightedBB100(sessions), hands, len(sessions)), nil
}

func tournamentStatsMessage(sc *sheets.Client, since time.Time, tType, periodLabel, label, emoji string, includeAvgFinish bool) (string, error) {
	tournaments, err := sc.GetTournamentsSince(since)
	if err != nil {
		return "", err
	}
	var filtered []parsers.Tournament
	for _, t := range tournaments {
		if t.Type == tType {
			filtered = append(filtered, t)
		}
	}
	var net float64
	for _, t := range filtered {
		net += t.NetUSD
	}
	r := roi(filtered, tType)

	if !includeAvgFinish {
		return fmt.Sprintf("%s %s Stats (%s)\nNet: %s\nROI: %s\nGames: %d",
			emoji, label, periodLabel, formatSignedUSD(net), formatSignedPercent(r), len(filtered)), nil
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
	return fmt.Sprintf("%s %s Stats (%s)\nNet: %s\nROI: %s\nGames: %d\nAvg finish: %.0f%%",
		emoji, label, periodLabel, formatSignedUSD(net), formatSignedPercent(r), len(filtered), avgFinish), nil
}

// --- /paths flow ---

func (s *conversationState) startPathsFlow(tgBot *tgbotapi.BotAPI, chatID int64, configPath string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
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
		wasFirstTime := s.pathsFirstTimeSetup
		s.reset()

		cfg, err := config.LoadConfig(configPath)
		if err != nil {
			slog.Error("loading config", "err", err)
			send(tgBot, chatID, "Something went wrong. Check logs.")
			return
		}

		restartWatcher()

		if wasFirstTime && cfg.Paths.HandHistory != nil && cfg.Paths.TournamentSummary != nil {
			send(tgBot, chatID, "✅ Paths saved. Tracker is now active.")
		} else {
			send(tgBot, chatID, "✅ Paths saved. Watcher restarted.")
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

func (s *conversationState) handlePathsReply(tgBot *tgbotapi.BotAPI, chatID int64, text, configPath string) {
	normalized := fixMissingDriveColon(NormalizeWindowsPath(text))

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(tgBot, chatID, "Something went wrong. Check logs.")
		s.reset()
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
func EvaluateSessionLossAlert(session parsers.CashSession, sc *sheets.Client, configPath string, alertChan chan<- string) error {
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
func EvaluateDownswingAlert(buyInUSD float64, sc *sheets.Client, configPath string, alertChan chan<- string) error {
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

func (s *conversationState) handleAlertsCallback(tgBot *tgbotapi.BotAPI, chatID int64, action string) {
	if action == "done" {
		s.reset()
		send(tgBot, chatID, "✅ Alert settings saved.")
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

func (s *conversationState) handleHardstopCallback(tgBot *tgbotapi.BotAPI, chatID int64, action string) {
	if action == "done" {
		s.reset()
		send(tgBot, chatID, "✅ Hard stop settings saved.")
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
		s.reset()
		send(tgBot, chatID, "✅ Schedule saved.")
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
	{"start", "Run the full setup wizard (paths, alerts, hard stop)."},
	{"deposit", "Log a deposit to your bankroll ledger."},
	{"withdraw", "Log a withdrawal from your bankroll ledger."},
	{"status", "Show current bankroll and P&L."},
	{"lastsession", "Show the summary of the last recorded session."},
	{"stats", "Show filtered performance stats."},
	{"sheet", "Get the Google Sheets URL."},
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
	"start": "Runs the full setup wizard: /paths, then /alerts, then /hardstop.\n" +
		"Usage: /start\n" +
		"Re-runnable at any time.",
	"status": "Shows current bankroll and profit/loss.\n" +
		"Usage: /status\n" +
		"No arguments.",
	"sheet": "Returns the URL of your Google Sheets tracker.\n" +
		"Usage: /sheet\n" +
		"No arguments.",
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
