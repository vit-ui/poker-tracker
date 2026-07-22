package bot

// The four inline-keyboard config flows (/paths, /alerts, /hardstop,
// /schedule), the screen-name recovery gate they share, /deposit and
// /withdraw, and the input parsing/validation helpers.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
)

// commaAsDecimal lets every plain-text amount typed into the bot — /deposit,
// /withdraw, and the /alerts and /hardstop threshold flows (downswing,
// session loss, daily loss, Spin & Go daily loss) — accept a comma as the
// decimal separator ("2,54") alongside a dot ("2.54"): the player is
// Brazilian, where comma is the standard decimal separator, but
// strconv.ParseFloat only accepts a dot. Only swaps when no dot is already
// present, so it can't corrupt an already-dotted input.
func commaAsDecimal(s string) string {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, ".") {
		s = strings.Replace(s, ",", ".", 1)
	}
	return s
}

func parsePositiveFloat(s string) (float64, error) {
	v, err := strconv.ParseFloat(commaAsDecimal(s), 64)
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
func (b *Bot) handleDepositWithdraw(chatID int64, args, kind string) {
	amount, err := parsePositiveFloat(args)
	if err != nil {
		if kind == "deposit" {
			send(b.tg, chatID, "Invalid amount. Usage: /deposit <usd_amount> — enter the USD amount shown on PokerStars after the deposit clears, not BRL.")
		} else {
			send(b.tg, chatID, "Invalid amount. Usage: /withdraw <usd_amount> — enter the USD amount shown on PokerStars, not BRL.")
		}
		return
	}

	ledgerAmount := amount
	label := "Deposit"
	if kind == "withdrawal" {
		ledgerAmount = -amount
		label = "Withdrawal"
	}

	if err := b.store.AppendLedgerEntry(kind, ledgerAmount); err != nil {
		slog.Error("appending ledger entry", "kind", kind, "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	bankroll, err := b.store.GetCurrentBankroll()
	if err != nil {
		slog.Error("getting bankroll", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	send(b.tg, chatID, fmt.Sprintf("✅ %s logged: %s\nBankroll: $%.2f", label, FormatSignedUSD(ledgerAmount), bankroll))
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
func (b *Bot) ensureScreenName(chatID int64, cfg *config.Config, resumeFlow string) bool {
	if cfg.Setup.PokerStarsScreenName != nil {
		return true
	}
	b.flow = "screen_name"
	b.field = "screen_name"
	b.resumeFlow = resumeFlow
	send(b.tg, chatID, "Your PokerStars screen name isn't set. Send it now (exact spelling, case-sensitive) before continuing.")
	return false
}

// handleScreenNameReply saves the PokerStars screen name entered in
// response to ensureScreenName's recovery prompt, then resumes whichever
// flow (paths, alerts, hardstop, schedule) triggered it.
func (b *Bot) handleScreenNameReply(chatID int64, text string) {
	name := strings.TrimSpace(text)
	if name == "" {
		send(b.tg, chatID, "Screen name can't be empty. Send your exact PokerStars screen name.")
		return
	}

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}
	cfg.Setup.PokerStarsScreenName = &name
	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}

	send(b.tg, chatID, fmt.Sprintf("✅ Screen name saved: %s", name))

	resumeFlow := b.resumeFlow
	b.resumeFlow = ""
	switch resumeFlow {
	case "paths":
		b.startPathsFlow(chatID)
	case "alerts":
		b.startAlertsFlow(chatID)
	case "hardstop":
		b.startHardstopFlow(chatID)
	case "schedule":
		b.startScheduleFlow(chatID)
	}
}

// --- /paths flow ---

func (b *Bot) startPathsFlow(chatID int64) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	if !b.ensureScreenName(chatID, cfg, "paths") {
		return
	}
	b.flow = "paths"
	b.field = ""
	b.pathsFirstTimeSetup = cfg.Paths.HandHistory == nil && cfg.Paths.TournamentSummary == nil
	b.sendPathsKeyboard(chatID, cfg)
}

func (b *Bot) sendPathsKeyboard(chatID int64, cfg *config.Config) {
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
	sendWithKeyboard(b.tg, chatID, "Configure PokerStars folder locations:", markup)
}

func (b *Bot) handlePathsCallback(chatID int64, action string) {
	if action == "done" {
		cfg, err := config.LoadConfig(b.configPath)
		if err != nil {
			slog.Error("loading config", "err", err)
			send(b.tg, chatID, "Something went wrong. Check logs.")
			return
		}

		if cfg.Paths.HandHistory == nil || cfg.Paths.TournamentSummary == nil {
			send(b.tg, chatID, "Both paths must be set before continuing. Tap a field to configure it.")
			b.sendPathsKeyboard(chatID, cfg)
			return
		}

		wasFirstTime := b.pathsFirstTimeSetup
		inWizard := b.startWizard
		b.reset()

		b.restartWatcher()

		if wasFirstTime {
			send(b.tg, chatID, "✅ Paths saved. Tracker is now active.")
		} else {
			send(b.tg, chatID, "✅ Paths saved. Watcher restarted.")
		}

		if inWizard {
			b.startAlertsFlow(chatID)
		}
		return
	}

	b.flow = "paths"
	b.field = action
	send(b.tg, chatID, pathPromptText(action))
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

func (b *Bot) handlePathsReply(chatID int64, text string) {
	normalized := fixMissingDriveColon(NormalizeWindowsPath(text))

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}
	if cfg.Setup.PokerStarsScreenName == nil {
		// Lost between startPathsFlow and this reply (e.g. config.json
		// touched externally mid-flow) — re-enter recovery rather than
		// dead-ending on an error.
		b.startPathsFlow(chatID)
		return
	}

	full, err := resolveAgainstHomeDir(normalized)
	if err != nil {
		slog.Error("resolving home directory", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}

	joined := filepath.Join(full, *cfg.Setup.PokerStarsScreenName)
	if !windowsPathExists(joined) {
		send(b.tg, chatID, fmt.Sprintf("That path doesn't exist: %s\nSend the folder path again.", joined))
		return
	}

	switch b.field {
	case "hand_history":
		cfg.Paths.HandHistory = &full
	case "tournament_summary":
		cfg.Paths.TournamentSummary = &full
	}
	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}
	b.field = ""
	b.sendPathsKeyboard(chatID, cfg)
}

// --- /alerts flow ---

func (b *Bot) startAlertsFlow(chatID int64) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	if !b.ensureScreenName(chatID, cfg, "alerts") {
		return
	}
	b.flow = "alerts"
	b.field = ""
	b.sendAlertsKeyboard(chatID, cfg)
}

func (b *Bot) sendAlertsKeyboard(chatID int64, cfg *config.Config) {
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
	sendWithKeyboard(b.tg, chatID, "Configure alert thresholds:", markup)
}

func (b *Bot) handleAlertsCallback(chatID int64, action string) {
	if action == "done" {
		inWizard := b.startWizard
		b.reset()
		send(b.tg, chatID, "✅ Alert settings saved.")
		if inWizard {
			b.startHardstopFlow(chatID)
		}
		return
	}
	b.flow = "alerts"
	b.field = action
	send(b.tg, chatID, alertsPromptText(action))
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

func (b *Bot) handleAlertsReply(chatID int64, text string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}

	switch b.field {
	case "downswing":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(b.tg, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotAbove(v, cfg.Hardstop.DownswingBuyins, "downswing alert", "the hard stop downswing threshold"); err != nil {
			send(b.tg, chatID, err.Error())
			return
		}
		cfg.Alerts.DownswingBuyins = &v
	case "session_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(b.tg, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotAbove(v, cfg.Hardstop.SessionLossBuyins, "session loss alert", "the hard stop session loss threshold"); err != nil {
			send(b.tg, chatID, err.Error())
			return
		}
		cfg.Alerts.SessionLossBuyins = &v
	case "losing_streak":
		v, err := parsePositiveInt(text)
		if err != nil {
			send(b.tg, chatID, "Invalid value. Please send a positive whole number.")
			return
		}
		if err := validateNotAbove(v, cfg.Hardstop.ConsecutiveLosses, "losing streak alert", "the hard stop consecutive-losses threshold"); err != nil {
			send(b.tg, chatID, err.Error())
			return
		}
		cfg.Alerts.LosingStreakCount = &v
	case "daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(b.tg, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotAbove(v, cfg.Hardstop.DailyLossUSD, "daily loss alert", "the hard stop daily loss threshold"); err != nil {
			send(b.tg, chatID, err.Error())
			return
		}
		cfg.Alerts.DailyLossUSD = &v
	case "spingo_daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(b.tg, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotAbove(v, cfg.Hardstop.SpingoDailyLossUSD, "Spin & Go daily loss alert", "the hard stop Spin & Go daily loss threshold"); err != nil {
			send(b.tg, chatID, err.Error())
			return
		}
		cfg.Alerts.SpingoDailyLossUSD = &v
	}

	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}
	b.field = ""
	b.sendAlertsKeyboard(chatID, cfg)
}

// --- /hardstop flow ---

func (b *Bot) handleHardstopCommand(chatID int64, args string) {
	if strings.TrimSpace(args) == "trigger" {
		manualAlert := make(chan string, 1)
		if err := hardstop.ManualTrigger(b.configPath, b.lockfilePath, manualAlert); err != nil {
			send(b.tg, chatID, "Configure duration first with /hardstop")
			return
		}
		send(b.tg, chatID, <-manualAlert)
		return
	}
	b.startHardstopFlow(chatID)
}

func (b *Bot) startHardstopFlow(chatID int64) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	if !b.ensureScreenName(chatID, cfg, "hardstop") {
		return
	}
	b.flow = "hardstop"
	b.field = ""
	b.sendHardstopKeyboard(chatID, cfg)
}

func (b *Bot) sendHardstopKeyboard(chatID int64, cfg *config.Config) {
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
	sendWithKeyboard(b.tg, chatID, "Configure hard stop rules:", markup)
}

func (b *Bot) handleHardstopCallback(chatID int64, action string) {
	if action == "done" {
		cfg, err := config.LoadConfig(b.configPath)
		if err != nil {
			slog.Error("loading config", "err", err)
			send(b.tg, chatID, "Something went wrong. Check logs.")
			return
		}

		anyRuleConfigured := cfg.Hardstop.SessionLossBuyins != nil ||
			cfg.Hardstop.DownswingBuyins != nil ||
			cfg.Hardstop.ConsecutiveLosses != nil ||
			cfg.Hardstop.DailyLossUSD != nil ||
			cfg.Hardstop.SpingoDailyLossUSD != nil
		if cfg.Hardstop.DurationHours == nil && anyRuleConfigured {
			send(b.tg, chatID, "Duration must be set if any hard stop rule is configured. Tap Duration to set it.")
			b.sendHardstopKeyboard(chatID, cfg)
			return
		}

		inWizard := b.startWizard
		b.reset()
		send(b.tg, chatID, "✅ Hard stop settings saved.")
		if inWizard {
			b.startScheduleFlow(chatID)
		}
		return
	}
	b.flow = "hardstop"
	b.field = action
	send(b.tg, chatID, hardstopPromptText(action))
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

func (b *Bot) handleHardstopReply(chatID int64, text string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}

	switch b.field {
	case "session_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(b.tg, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotBelow(v, cfg.Alerts.SessionLossBuyins, "hard stop session loss threshold", "the session loss alert"); err != nil {
			send(b.tg, chatID, err.Error())
			return
		}
		cfg.Hardstop.SessionLossBuyins = &v
	case "downswing":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(b.tg, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotBelow(v, cfg.Alerts.DownswingBuyins, "hard stop downswing threshold", "the downswing alert"); err != nil {
			send(b.tg, chatID, err.Error())
			return
		}
		cfg.Hardstop.DownswingBuyins = &v
	case "consecutive_losses":
		v, err := parsePositiveInt(text)
		if err != nil {
			send(b.tg, chatID, "Invalid value. Please send a positive whole number.")
			return
		}
		if err := validateNotBelow(v, cfg.Alerts.LosingStreakCount, "hard stop consecutive-losses threshold", "the losing streak alert"); err != nil {
			send(b.tg, chatID, err.Error())
			return
		}
		cfg.Hardstop.ConsecutiveLosses = &v
	case "daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(b.tg, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotBelow(v, cfg.Alerts.DailyLossUSD, "hard stop daily loss threshold", "the daily loss alert"); err != nil {
			send(b.tg, chatID, err.Error())
			return
		}
		cfg.Hardstop.DailyLossUSD = &v
	case "spingo_daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			send(b.tg, chatID, "Invalid value. Please send a positive number.")
			return
		}
		if err := validateNotBelow(v, cfg.Alerts.SpingoDailyLossUSD, "hard stop Spin & Go daily loss threshold", "the Spin & Go daily loss alert"); err != nil {
			send(b.tg, chatID, err.Error())
			return
		}
		cfg.Hardstop.SpingoDailyLossUSD = &v
	case "duration":
		v, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || (v != 12 && v != 24) {
			send(b.tg, chatID, "Duration must be 12 or 24.")
			return
		}
		cfg.Hardstop.DurationHours = &v
	}

	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}
	b.field = ""
	b.sendHardstopKeyboard(chatID, cfg)
}

// --- /schedule flow ---

func (b *Bot) startScheduleFlow(chatID int64) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	if !b.ensureScreenName(chatID, cfg, "schedule") {
		return
	}
	b.flow = "schedule"
	b.field = ""
	b.sendScheduleKeyboard(chatID, cfg)
}

func (b *Bot) sendScheduleKeyboard(chatID int64, cfg *config.Config) {
	dayStartHour := "not set"
	if cfg.Schedule.DayStartHour != nil {
		dayStartHour = strconv.Itoa(*cfg.Schedule.DayStartHour)
	}
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Poker day start hour", "schedule:day_start_hour")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Done", "schedule:done")),
	)
	text := fmt.Sprintf("Configure your poker day schedule:\nPoker day start hour: %s", dayStartHour)
	sendWithKeyboard(b.tg, chatID, text, markup)
}

func (b *Bot) handleScheduleCallback(chatID int64, action string) {
	if action == "done" {
		inWizard := b.startWizard
		b.startWizard = false
		b.reset()
		send(b.tg, chatID, "✅ Schedule saved.")
		if inWizard {
			send(b.tg, chatID, "✅ Setup complete. The tracker is running. Type /help to see all commands.")
		}
		return
	}
	b.flow = "schedule"
	b.field = action
	send(b.tg, chatID, "What hour does your poker day start? Send a number from 0 to 23 (e.g. 6 for 6am local time).")
}

func (b *Bot) handleScheduleReply(chatID int64, text string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}

	v, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || v < 0 || v > 23 {
		send(b.tg, chatID, "Invalid value. Please send a whole number from 0 to 23.")
		return
	}
	cfg.Schedule.DayStartHour = &v

	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}
	b.field = ""
	b.sendScheduleKeyboard(chatID, cfg)
}
