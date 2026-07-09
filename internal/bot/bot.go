// Package bot implements the Telegram bot: outgoing session alerts and
// incoming commands for configuration and status queries.
package bot

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sheets"
)

// Run starts the bot in serve mode. Forwards alerts from alertChan to
// Telegram and sends weekly/monthly summaries on a schedule. Blocks until
// alertChan is closed.
//
// restartWatcher is accepted here so this signature matches the call already
// wired in main.go; it is wired to the /paths command in Part 7 and is not
// called by this function.
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
		}
	}()

	for msg := range alertChan {
		sendMessage(tgBot, configPath, msg)
	}
	return nil
}

// RunSetup starts the bot in setup mode and blocks until the user completes
// the configuration wizard via Telegram. Implemented in step 8 of the build order.
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

// sendMessage reads the configured Telegram chat ID and sends text to it.
// If no chat ID is configured yet, the message is logged and discarded.
func sendMessage(tgBot *tgbotapi.BotAPI, configPath, text string) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config for telegram send", "err", err)
		return
	}
	if cfg.Setup.TelegramChatID == nil {
		slog.Info("no chat ID configured yet — message the bot once, or set telegram_chat_id manually in config.json for testing")
		return
	}
	chatID, err := strconv.ParseInt(*cfg.Setup.TelegramChatID, 10, 64)
	if err != nil {
		slog.Error("parsing telegram chat id", "err", err)
		return
	}
	if _, err := tgBot.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		slog.Error("sending telegram message", "err", err)
	}
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
