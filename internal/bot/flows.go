package bot

// The cascading inline-keyboard config menus (/paths, /alerts, /hardstop,
// /schedule), the screen-name recovery gate they share, /deposit and
// /withdraw, and the input parsing/validation helpers.
//
// Menu model: every config screen is one message edited in place as the player
// drills in and out (BotFather-style), dispatched by a single router
// (handleMenuCallback) on a "menu:"-prefixed callback path. A screen opened at
// the top (via its command) shows ✖ Cancel; a screen drilled into shows
// ‹ Back to its parent (parent = the path with its last ":" segment removed).
// There is no Done button — a threshold field is turned off from its own
// screen, paths restart the watcher on save, and the hard-stop duration guard
// fires when leaving the hard-stop menu.

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

// --- menu router ---

// parentPath returns path with its last ":" segment removed, or "" if path is
// a single top-level segment. Back navigation targets this; an empty parent
// means the screen is a root and shows Cancel instead of Back.
func parentPath(path string) string {
	if i := strings.LastIndex(path, ":"); i >= 0 {
		return path[:i]
	}
	return ""
}

func menuRow(label, data string) []tgbotapi.InlineKeyboardButton {
	return tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(label, data))
}

// navButton returns a screen's footer: ‹ Back to its parent, or ✖ Cancel when
// it's a root.
func navButton(path string) []tgbotapi.InlineKeyboardButton {
	if parent := parentPath(path); parent != "" {
		return menuRow("‹ Back", "menu:"+parent)
	}
	return menuRow("✖ Cancel", "menu:cancel")
}

// showMenu sends a fresh keyboard message when messageID is 0 (a command
// opening a menu) or edits the existing one in place otherwise (a drill-down
// tap).
func (b *Bot) showMenu(chatID int64, messageID int, text string, rows [][]tgbotapi.InlineKeyboardButton) {
	markup := tgbotapi.NewInlineKeyboardMarkup(rows...)
	if messageID == 0 {
		sendWithKeyboard(b.tg, chatID, text, markup)
		return
	}
	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, text, markup)
	if _, err := b.tg.Send(edit); err != nil {
		slog.Error("editing menu message", "err", err)
	}
}

// handleMenuCallback routes a "menu:"-prefixed callback (path is everything
// after that prefix). Actions (:set, :off, leave) run and re-render; anything
// else is a screen path handed to renderScreen.
func (b *Bot) handleMenuCallback(chatID int64, messageID int, path string) {
	if path == "cancel" {
		b.reset()
		b.clearKeyboard(chatID, messageID)
		return
	}
	segs := strings.Split(path, ":")
	if segs[0] == "leave" {
		b.reset()
		b.handleHardstopLeave(chatID, messageID, strings.TrimPrefix(path, "leave:"))
		return
	}
	last := segs[len(segs)-1]
	switch {
	case last == "set":
		// Starts a value entry — sets the conversation state itself.
		b.beginValueEntry(chatID, messageID, strings.TrimSuffix(path, ":set"))
	case last == "toggle":
		b.reset()
		b.toggleStakeChange(chatID, messageID, strings.TrimSuffix(path, ":toggle"))
	case isSummaryMode(last) && strings.Contains(path, "summaries"):
		// A summary mode pick ("...:every|wins|losses|off"). Checked before the
		// plain ":off" case below, which "off" would otherwise also match.
		b.reset()
		b.setSummaryMode(chatID, messageID, path)
	case last == "off":
		b.reset()
		b.turnOff(chatID, messageID, strings.TrimSuffix(path, ":off"))
	case last == "deposit" || last == "withdraw":
		b.beginMoneyEntry(chatID, messageID, path)
	case isCommandAction(last):
		// A hub shortcut to an existing command — runs it (which sends its own
		// output or starts its flow); the menu message stays for more taps.
		b.reset()
		b.runCommandAction(chatID, last)
	default:
		// Any navigation abandons a pending value entry so a later stray
		// text message isn't applied to the field it was prompting for.
		b.reset()
		b.renderScreen(chatID, messageID, path)
	}
}

// isCommandAction reports whether a menu leaf token maps to an existing command
// (as opposed to a cascade screen or a config field).
func isCommandAction(token string) bool {
	switch token {
	case "screenname", "storage", "sheet", "credentials",
		"status", "lastsession", "stats", "export", "help":
		return true
	}
	return false
}

// runCommandAction invokes the command a hub button stands for. These send
// their own messages (or start their own flow), leaving the menu in place.
func (b *Bot) runCommandAction(chatID int64, token string) {
	switch token {
	case "screenname":
		b.handleScreenNameCommand(chatID, "")
	case "storage":
		b.handleStorageCommand(chatID, "")
	case "sheet":
		b.handleSheetCommand(chatID, "")
	case "credentials":
		b.startCredentialsFlow(chatID)
	case "status":
		b.handleStatus(chatID)
	case "lastsession":
		b.handleLastSession(chatID)
	case "stats":
		b.handleStats(chatID, "")
	case "export":
		b.handleExportCommand(chatID)
	case "help":
		send(b.tg, chatID, helpText(""))
	}
}

// renderScreen edits messageID to show the screen at path. The kind of screen
// is decided by path's last segment — a known menu name renders that menu, a
// field key renders its item (thresholds) or begins value entry (paths /
// schedule) — so it works whether the screen sits at a root (via a command) or
// nested under a hub.
func (b *Bot) renderScreen(chatID int64, messageID int, path string) {
	segs := strings.Split(path, ":")
	switch segs[len(segs)-1] {
	case "root":
		b.renderRootMenu(chatID, messageID, path)
	case "setup":
		b.renderSetupMenu(chatID, messageID, path)
	case "info":
		b.renderInfoMenu(chatID, messageID, path)
	case "money":
		b.renderMoneyMenu(chatID, messageID, path)
	case "alerts":
		b.renderAlertsMenu(chatID, messageID, path)
	case "hardstop":
		b.renderHardstopMenu(chatID, messageID, path, "")
	case "paths":
		b.renderPathsMenu(chatID, messageID, path)
	case "schedule":
		b.renderScheduleMenu(chatID, messageID, path)
	case "notify":
		b.renderNotifyMenu(chatID, messageID, path)
	case "summaries":
		b.renderSummariesMenu(chatID, messageID, path)
	default:
		// A field key: flow is the segment before it.
		flow := segs[len(segs)-2]
		switch flow {
		case "alerts", "hardstop":
			b.renderThresholdItem(chatID, messageID, path)
		case "summaries":
			b.renderSummaryItem(chatID, messageID, path)
		case "paths", "schedule":
			b.beginValueEntry(chatID, messageID, path)
		}
	}
}

// --- threshold flows (/alerts, /hardstop) ---

var alertKeys = []string{"downswing", "session_loss", "losing_streak", "daily_loss", "spingo_daily_loss"}
var hardstopKeys = []string{"session_loss", "downswing", "consecutive_losses", "daily_loss", "spingo_daily_loss", "duration"}

func floatText(v *float64) string {
	if v == nil {
		return "not set"
	}
	return fmt.Sprintf("%g", *v)
}

func intText(v *int) string {
	if v == nil {
		return "not set"
	}
	return strconv.Itoa(*v)
}

func alertLabel(key string) string {
	switch key {
	case "downswing":
		return "Downswing (buy-ins)"
	case "session_loss":
		return "Session loss (buy-ins)"
	case "losing_streak":
		return "Losing streak (sessions)"
	case "daily_loss":
		return "Daily loss, all games ($)"
	case "spingo_daily_loss":
		return "Spin & Go daily loss ($)"
	}
	return key
}

func alertValueText(cfg *config.Config, key string) string {
	switch key {
	case "downswing":
		return floatText(cfg.Alerts.DownswingBuyins)
	case "session_loss":
		return floatText(cfg.Alerts.SessionLossBuyins)
	case "losing_streak":
		return intText(cfg.Alerts.LosingStreakCount)
	case "daily_loss":
		return floatText(cfg.Alerts.DailyLossUSD)
	case "spingo_daily_loss":
		return floatText(cfg.Alerts.SpingoDailyLossUSD)
	}
	return "not set"
}

func hardstopLabel(key string) string {
	switch key {
	case "session_loss":
		return "Session loss (buy-ins)"
	case "downswing":
		return "Downswing (buy-ins)"
	case "consecutive_losses":
		return "Consecutive losses (sessions)"
	case "daily_loss":
		return "Daily loss, all games ($)"
	case "spingo_daily_loss":
		return "Spin & Go daily loss ($)"
	case "duration":
		return "Duration (hours)"
	}
	return key
}

func hardstopValueText(cfg *config.Config, key string) string {
	switch key {
	case "session_loss":
		return floatText(cfg.Hardstop.SessionLossBuyins)
	case "downswing":
		return floatText(cfg.Hardstop.DownswingBuyins)
	case "consecutive_losses":
		return intText(cfg.Hardstop.ConsecutiveLosses)
	case "daily_loss":
		return floatText(cfg.Hardstop.DailyLossUSD)
	case "spingo_daily_loss":
		return floatText(cfg.Hardstop.SpingoDailyLossUSD)
	case "duration":
		return intText(cfg.Hardstop.DurationHours)
	}
	return "not set"
}

func (b *Bot) renderAlertsMenu(chatID int64, messageID int, path string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, key := range alertKeys {
		rows = append(rows, menuRow(alertLabel(key)+" ["+alertValueText(cfg, key)+"]", "menu:"+path+":"+key))
	}
	rows = append(rows, navButton(path))
	b.showMenu(chatID, messageID, "Alert thresholds — tap one to change:", rows)
}

// renderHardstopMenu shows the hard-stop rules. warning, when non-empty, is
// prepended — used by handleHardstopLeave to keep the player on this screen
// when a rule is set with no duration.
func (b *Bot) renderHardstopMenu(chatID int64, messageID int, path, warning string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, key := range hardstopKeys {
		rows = append(rows, menuRow(hardstopLabel(key)+" ["+hardstopValueText(cfg, key)+"]", "menu:"+path+":"+key))
	}
	// The footer routes through the leave guard rather than navigating
	// directly, so backing out with a rule set but no duration warns and stays.
	label := "✖ Cancel"
	if parentPath(path) != "" {
		label = "‹ Back"
	}
	rows = append(rows, menuRow(label, "menu:leave:"+path))
	text := "Hard stop rules — tap one to change:"
	if warning != "" {
		text = warning + "\n\n" + text
	}
	b.showMenu(chatID, messageID, text, rows)
}

// renderThresholdItem shows one alert/hard-stop field's screen: Change value,
// Turn off (except Duration, which can't be turned off), and Back.
func (b *Bot) renderThresholdItem(chatID int64, messageID int, path string) {
	segs := strings.Split(path, ":")
	key := segs[len(segs)-1]
	flow := segs[len(segs)-2]

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	var label, value string
	if flow == "alerts" {
		label, value = alertLabel(key), alertValueText(cfg, key)
	} else {
		label, value = hardstopLabel(key), hardstopValueText(cfg, key)
	}
	notSet := value == "not set"
	canTurnOff := !(flow == "hardstop" && key == "duration")

	setLabel := "Change value"
	if notSet {
		setLabel = "Set value"
	}
	rows := [][]tgbotapi.InlineKeyboardButton{menuRow(setLabel, "menu:"+path+":set")}
	if canTurnOff && !notSet {
		rows = append(rows, menuRow("Turn off", "menu:"+path+":off"))
	}
	rows = append(rows, navButton(path))
	b.showMenu(chatID, messageID, fmt.Sprintf("%s\nCurrent: %s", label, value), rows)
}

// handleHardstopLeave enforces the duration guard when leaving the hard-stop
// menu: if any rule is set with no duration_hours, it re-renders with a warning
// and stays; otherwise it navigates to the parent (or clears the keyboard at a
// root).
func (b *Bot) handleHardstopLeave(chatID int64, messageID int, menuPath string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	if hardstopRuleSetWithoutDuration(cfg) {
		b.renderHardstopMenu(chatID, messageID, menuPath, "⚠️ Set a duration first — a hard stop rule with no duration can never fire.")
		return
	}
	if parent := parentPath(menuPath); parent != "" {
		b.renderScreen(chatID, messageID, parent)
		return
	}
	b.clearKeyboard(chatID, messageID)
}

func hardstopRuleSetWithoutDuration(cfg *config.Config) bool {
	h := cfg.Hardstop
	anyRule := h.SessionLossBuyins != nil || h.DownswingBuyins != nil || h.ConsecutiveLosses != nil ||
		h.DailyLossUSD != nil || h.SpingoDailyLossUSD != nil
	return anyRule && h.DurationHours == nil
}

// --- /paths flow ---

func (b *Bot) renderPathsMenu(chatID int64, messageID int, path string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	hh := "not set"
	if cfg.Paths.HandHistory != nil {
		hh = displayRelativeToHome(*cfg.Paths.HandHistory)
	}
	ts := "not set"
	if cfg.Paths.TournamentSummary != nil {
		ts = displayRelativeToHome(*cfg.Paths.TournamentSummary)
	}
	rows := [][]tgbotapi.InlineKeyboardButton{
		menuRow("Hand History: "+hh, "menu:"+path+":hand_history"),
		menuRow("Tournament Summary: "+ts, "menu:"+path+":tournament_summary"),
		navButton(path),
	}
	b.showMenu(chatID, messageID, "PokerStars folder locations — tap one to change:", rows)
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

// --- /schedule flow ---

func (b *Bot) renderScheduleMenu(chatID int64, messageID int, path string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	rows := [][]tgbotapi.InlineKeyboardButton{
		menuRow("Poker day start hour ["+intText(cfg.Schedule.DayStartHour)+"]", "menu:"+path+":day_start"),
		navButton(path),
	}
	b.showMenu(chatID, messageID, "Poker day schedule — tap to change (default 6 = 6am):", rows)
}

// --- /notify flow ---

var summaryKeys = []string{"cash", "tournament", "weekly", "monthly"}

func summaryLabel(key string) string {
	switch key {
	case "cash":
		return "Cash session summary"
	case "tournament":
		return "Tournament summary"
	case "weekly":
		return "Weekly summary"
	case "monthly":
		return "Monthly summary"
	}
	return key
}

// summaryModeLabel is the human label for a stored mode value.
func summaryModeLabel(mode string) string {
	switch mode {
	case "wins":
		return "Only wins"
	case "losses":
		return "Only losses"
	case "off":
		return "Off"
	default:
		return "Every"
	}
}

func isSummaryMode(s string) bool {
	return s == "every" || s == "wins" || s == "losses" || s == "off"
}

func summaryModePtr(cfg *config.Config, key string) *string {
	switch key {
	case "cash":
		return cfg.Notifications.CashSessionSummary
	case "tournament":
		return cfg.Notifications.TournamentSummary
	case "weekly":
		return cfg.Notifications.WeeklySummary
	case "monthly":
		return cfg.Notifications.MonthlySummary
	}
	return nil
}

func setSummaryModeField(cfg *config.Config, key, mode string) {
	m := mode
	switch key {
	case "cash":
		cfg.Notifications.CashSessionSummary = &m
	case "tournament":
		cfg.Notifications.TournamentSummary = &m
	case "weekly":
		cfg.Notifications.WeeklySummary = &m
	case "monthly":
		cfg.Notifications.MonthlySummary = &m
	}
}

func (b *Bot) startNotifyFlow(chatID int64) {
	b.renderNotifyMenu(chatID, 0, "notify")
}

func (b *Bot) renderNotifyMenu(chatID int64, messageID int, path string) {
	rows := [][]tgbotapi.InlineKeyboardButton{
		menuRow("Summaries", "menu:"+path+":summaries"),
		menuRow("Alerts", "menu:"+path+":alerts"),
		navButton(path),
	}
	b.showMenu(chatID, messageID, "Notifications — what should the bot send you?", rows)
}

func (b *Bot) renderSummariesMenu(chatID int64, messageID int, path string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, key := range summaryKeys {
		mode := config.SummaryMode(summaryModePtr(cfg, key))
		rows = append(rows, menuRow(summaryLabel(key)+" ["+summaryModeLabel(mode)+"]", "menu:"+path+":"+key))
	}
	stake := "Off"
	if config.NotificationEnabled(cfg.Notifications.StakeChange) {
		stake = "On"
	}
	rows = append(rows, menuRow("Stake change ["+stake+"]", "menu:"+path+":stake:toggle"))
	rows = append(rows, navButton(path))
	b.showMenu(chatID, messageID, "Summaries — tap a result summary to filter it (or toggle stake change):", rows)
}

// renderSummaryItem shows a result summary's mode screen (Every / Only wins /
// Only losses / Off), the current one marked.
func (b *Bot) renderSummaryItem(chatID int64, messageID int, path string) {
	segs := strings.Split(path, ":")
	key := segs[len(segs)-1]

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	current := config.SummaryMode(summaryModePtr(cfg, key))

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, m := range []string{"every", "wins", "losses", "off"} {
		label := summaryModeLabel(m)
		if m == current {
			label = "✓ " + label
		}
		rows = append(rows, menuRow(label, "menu:"+path+":"+m))
	}
	rows = append(rows, navButton(path))
	b.showMenu(chatID, messageID, summaryLabel(key)+" — when should I send it?", rows)
}

// setSummaryMode stores a result summary's chosen mode and re-renders its
// screen. path is the mode action, e.g. "notify:summaries:cash:losses".
func (b *Bot) setSummaryMode(chatID int64, messageID int, path string) {
	segs := strings.Split(path, ":")
	mode := segs[len(segs)-1]
	itemPath := strings.TrimSuffix(path, ":"+mode)
	key := segs[len(segs)-2]

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	setSummaryModeField(cfg, key, mode)
	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	b.renderSummaryItem(chatID, messageID, itemPath)
}

// toggleStakeChange flips the stake-change on/off and re-renders the summaries
// list. path is the item path, e.g. "notify:summaries:stake".
func (b *Bot) toggleStakeChange(chatID int64, messageID int, path string) {
	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	enabled := !config.NotificationEnabled(cfg.Notifications.StakeChange)
	cfg.Notifications.StakeChange = &enabled
	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	b.renderScreen(chatID, messageID, parentPath(path))
}

// --- /menu hub ---

func (b *Bot) startMenuFlow(chatID int64) {
	b.renderRootMenu(chatID, 0, "root")
}

func (b *Bot) renderRootMenu(chatID int64, messageID int, path string) {
	rows := [][]tgbotapi.InlineKeyboardButton{
		menuRow("⚙ Setup", "menu:"+path+":setup"),
		menuRow("🔔 Notifications", "menu:"+path+":notify"),
		menuRow("🛑 Hard stop", "menu:"+path+":hardstop"),
		menuRow("📊 Info", "menu:"+path+":info"),
		menuRow("💵 Money", "menu:"+path+":money"),
		menuRow("❓ Help", "menu:"+path+":help"),
		navButton(path),
	}
	b.showMenu(chatID, messageID, "Menu — what do you want to do?", rows)
}

func (b *Bot) renderSetupMenu(chatID int64, messageID int, path string) {
	rows := [][]tgbotapi.InlineKeyboardButton{
		menuRow("Paths", "menu:"+path+":paths"),
		menuRow("Schedule", "menu:"+path+":schedule"),
		menuRow("Screen name", "menu:"+path+":screenname"),
		menuRow("Storage", "menu:"+path+":storage"),
	}
	// Sheet and Credentials are only meaningful on the Google Sheets backend.
	if b.currentBackend() == "sheets" {
		rows = append(rows,
			menuRow("Sheet", "menu:"+path+":sheet"),
			menuRow("Credentials", "menu:"+path+":credentials"))
	}
	rows = append(rows, navButton(path))
	b.showMenu(chatID, messageID, "Setup — tap a setting:", rows)
}

func (b *Bot) renderInfoMenu(chatID int64, messageID int, path string) {
	rows := [][]tgbotapi.InlineKeyboardButton{
		menuRow("Status", "menu:"+path+":status"),
		menuRow("Last session", "menu:"+path+":lastsession"),
		menuRow("Stats", "menu:"+path+":stats"),
		menuRow("Export", "menu:"+path+":export"),
		navButton(path),
	}
	b.showMenu(chatID, messageID, "Info — tap one:", rows)
}

func (b *Bot) renderMoneyMenu(chatID int64, messageID int, path string) {
	rows := [][]tgbotapi.InlineKeyboardButton{
		menuRow("Deposit", "menu:"+path+":deposit"),
		menuRow("Withdraw", "menu:"+path+":withdraw"),
		navButton(path),
	}
	b.showMenu(chatID, messageID, "Money — log a deposit or withdrawal:", rows)
}

// beginMoneyEntry prompts for a deposit/withdrawal amount. path is the full
// menu path (its last segment "deposit"/"withdraw"), stored as the field so the
// reply re-renders the money menu at its real depth under the hub.
func (b *Bot) beginMoneyEntry(chatID int64, messageID int, path string) {
	verb := "deposit"
	if strings.HasSuffix(path, "withdraw") {
		verb = "withdraw"
	}
	b.flow = "menu"
	b.field = path
	b.menuMessageID = messageID
	send(b.tg, chatID, fmt.Sprintf("Send the USD amount to %s (as shown on PokerStars, not BRL).", verb))
}

// --- value entry ---

// beginValueEntry sends the prompt for a field and records the target and the
// menu message to edit back once the reply is saved.
func (b *Bot) beginValueEntry(chatID int64, messageID int, target string) {
	b.flow = "menu"
	b.field = target
	b.menuMessageID = messageID
	send(b.tg, chatID, promptFor(target))
}

func promptFor(target string) string {
	segs := strings.Split(target, ":")
	key := segs[len(segs)-1]
	switch segs[len(segs)-2] {
	case "alerts":
		return alertsPromptText(key)
	case "hardstop":
		return hardstopPromptText(key)
	case "paths":
		return pathPromptText(key)
	case "schedule":
		return "What hour does your poker day start? Send a number from 0 to 23 (e.g. 6 for 6am local time)."
	}
	return "Send the new value."
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

// turnOff clears an alert/hard-stop threshold (writes it to null) and
// re-renders its item screen.
func (b *Bot) turnOff(chatID int64, messageID int, target string) {
	segs := strings.Split(target, ":")
	key := segs[len(segs)-1]
	flow := segs[len(segs)-2]

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	if flow == "alerts" {
		clearAlertField(cfg, key)
	} else {
		clearHardstopField(cfg, key)
	}
	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	b.renderThresholdItem(chatID, messageID, target)
}

func clearAlertField(cfg *config.Config, key string) {
	switch key {
	case "downswing":
		cfg.Alerts.DownswingBuyins = nil
	case "session_loss":
		cfg.Alerts.SessionLossBuyins = nil
	case "losing_streak":
		cfg.Alerts.LosingStreakCount = nil
	case "daily_loss":
		cfg.Alerts.DailyLossUSD = nil
	case "spingo_daily_loss":
		cfg.Alerts.SpingoDailyLossUSD = nil
	}
}

func clearHardstopField(cfg *config.Config, key string) {
	switch key {
	case "session_loss":
		cfg.Hardstop.SessionLossBuyins = nil
	case "downswing":
		cfg.Hardstop.DownswingBuyins = nil
	case "consecutive_losses":
		cfg.Hardstop.ConsecutiveLosses = nil
	case "daily_loss":
		cfg.Hardstop.DailyLossUSD = nil
	case "spingo_daily_loss":
		cfg.Hardstop.SpingoDailyLossUSD = nil
	}
}

// handleMenuReply saves a value typed in response to beginValueEntry's prompt
// and edits the remembered menu message back to the relevant screen. On a
// validation error it sends the message and keeps the flow state so the player
// can retry.
func (b *Bot) handleMenuReply(chatID int64, text string) {
	target := b.field
	segs := strings.Split(target, ":")
	key := segs[len(segs)-1]
	flow := segs[len(segs)-2]

	// Money entry (Deposit / Withdraw) doesn't touch config — handleDepositWithdraw
	// sends its own confirmation or error. key is the menu token; map it to the
	// ledger kind.
	if flow == "money" {
		kind := "deposit"
		if key == "withdraw" {
			kind = "withdrawal"
		}
		messageID := b.menuMessageID
		b.reset()
		b.handleDepositWithdraw(chatID, text, kind)
		b.renderScreen(chatID, messageID, parentPath(target))
		return
	}

	cfg, err := config.LoadConfig(b.configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}

	pathsWasComplete := cfg.Paths.HandHistory != nil && cfg.Paths.TournamentSummary != nil

	var saveErr error
	switch flow {
	case "alerts":
		saveErr = saveAlertValue(cfg, key, text)
	case "hardstop":
		saveErr = saveHardstopValue(cfg, key, text)
	case "paths":
		saveErr = savePathValue(cfg, key, text)
	case "schedule":
		saveErr = saveScheduleValue(cfg, text)
	}
	if saveErr != nil {
		send(b.tg, chatID, saveErr.Error())
		return
	}
	if err := config.SaveConfig(b.configPath, cfg); err != nil {
		slog.Error("saving config", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		b.reset()
		return
	}

	messageID := b.menuMessageID
	b.reset()

	if flow == "paths" {
		b.afterPathSave(chatID, cfg, pathsWasComplete)
	}

	switch flow {
	case "alerts", "hardstop":
		b.renderThresholdItem(chatID, messageID, target)
	default:
		// paths / schedule: back to their list, at whatever depth it sits
		// (root, or nested under the /menu hub).
		b.renderScreen(chatID, messageID, parentPath(target))
	}
}

// afterPathSave restarts the watcher once both base paths are set (its only
// requirement), reporting first-time activation vs a re-config, and prompts for
// the other folder when only one is set — replacing the old paths "Done".
func (b *Bot) afterPathSave(chatID int64, cfg *config.Config, wasComplete bool) {
	if cfg.Paths.HandHistory == nil || cfg.Paths.TournamentSummary == nil {
		send(b.tg, chatID, "Saved. Set the other folder too before the tracker can start.")
		return
	}
	b.restartWatcher()
	if wasComplete {
		send(b.tg, chatID, "✅ Paths saved. Watcher restarted.")
	} else {
		send(b.tg, chatID, "✅ Paths saved. Tracker is now active.")
	}
}

func saveAlertValue(cfg *config.Config, key, text string) error {
	switch key {
	case "downswing":
		v, err := parsePositiveFloat(text)
		if err != nil {
			return fmt.Errorf("Invalid value. Please send a positive number.")
		}
		if err := validateNotAbove(v, cfg.Hardstop.DownswingBuyins, "downswing alert", "the hard stop downswing threshold"); err != nil {
			return err
		}
		cfg.Alerts.DownswingBuyins = &v
	case "session_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			return fmt.Errorf("Invalid value. Please send a positive number.")
		}
		if err := validateNotAbove(v, cfg.Hardstop.SessionLossBuyins, "session loss alert", "the hard stop session loss threshold"); err != nil {
			return err
		}
		cfg.Alerts.SessionLossBuyins = &v
	case "losing_streak":
		v, err := parsePositiveInt(text)
		if err != nil {
			return fmt.Errorf("Invalid value. Please send a positive whole number.")
		}
		if err := validateNotAbove(v, cfg.Hardstop.ConsecutiveLosses, "losing streak alert", "the hard stop consecutive-losses threshold"); err != nil {
			return err
		}
		cfg.Alerts.LosingStreakCount = &v
	case "daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			return fmt.Errorf("Invalid value. Please send a positive number.")
		}
		if err := validateNotAbove(v, cfg.Hardstop.DailyLossUSD, "daily loss alert", "the hard stop daily loss threshold"); err != nil {
			return err
		}
		cfg.Alerts.DailyLossUSD = &v
	case "spingo_daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			return fmt.Errorf("Invalid value. Please send a positive number.")
		}
		if err := validateNotAbove(v, cfg.Hardstop.SpingoDailyLossUSD, "Spin & Go daily loss alert", "the hard stop Spin & Go daily loss threshold"); err != nil {
			return err
		}
		cfg.Alerts.SpingoDailyLossUSD = &v
	}
	return nil
}

func saveHardstopValue(cfg *config.Config, key, text string) error {
	switch key {
	case "session_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			return fmt.Errorf("Invalid value. Please send a positive number.")
		}
		if err := validateNotBelow(v, cfg.Alerts.SessionLossBuyins, "hard stop session loss threshold", "the session loss alert"); err != nil {
			return err
		}
		cfg.Hardstop.SessionLossBuyins = &v
	case "downswing":
		v, err := parsePositiveFloat(text)
		if err != nil {
			return fmt.Errorf("Invalid value. Please send a positive number.")
		}
		if err := validateNotBelow(v, cfg.Alerts.DownswingBuyins, "hard stop downswing threshold", "the downswing alert"); err != nil {
			return err
		}
		cfg.Hardstop.DownswingBuyins = &v
	case "consecutive_losses":
		v, err := parsePositiveInt(text)
		if err != nil {
			return fmt.Errorf("Invalid value. Please send a positive whole number.")
		}
		if err := validateNotBelow(v, cfg.Alerts.LosingStreakCount, "hard stop consecutive-losses threshold", "the losing streak alert"); err != nil {
			return err
		}
		cfg.Hardstop.ConsecutiveLosses = &v
	case "daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			return fmt.Errorf("Invalid value. Please send a positive number.")
		}
		if err := validateNotBelow(v, cfg.Alerts.DailyLossUSD, "hard stop daily loss threshold", "the daily loss alert"); err != nil {
			return err
		}
		cfg.Hardstop.DailyLossUSD = &v
	case "spingo_daily_loss":
		v, err := parsePositiveFloat(text)
		if err != nil {
			return fmt.Errorf("Invalid value. Please send a positive number.")
		}
		if err := validateNotBelow(v, cfg.Alerts.SpingoDailyLossUSD, "hard stop Spin & Go daily loss threshold", "the Spin & Go daily loss alert"); err != nil {
			return err
		}
		cfg.Hardstop.SpingoDailyLossUSD = &v
	case "duration":
		v, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || (v != 12 && v != 24) {
			return fmt.Errorf("Duration must be 12 or 24.")
		}
		cfg.Hardstop.DurationHours = &v
	}
	return nil
}

func savePathValue(cfg *config.Config, key, text string) error {
	if cfg.Setup.PokerStarsScreenName == nil {
		return fmt.Errorf("Screen name isn't set. Run /paths again.")
	}
	full, err := resolveAgainstHomeDir(fixMissingDriveColon(NormalizeWindowsPath(text)))
	if err != nil {
		slog.Error("resolving home directory", "err", err)
		return fmt.Errorf("Something went wrong resolving that path. Send it again.")
	}
	joined := filepath.Join(full, *cfg.Setup.PokerStarsScreenName)
	if !windowsPathExists(joined) {
		return fmt.Errorf("That path doesn't exist: %s\nSend the folder path again.", joined)
	}
	switch key {
	case "hand_history":
		cfg.Paths.HandHistory = &full
	case "tournament_summary":
		cfg.Paths.TournamentSummary = &full
	}
	return nil
}

func saveScheduleValue(cfg *config.Config, text string) error {
	v, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || v < 0 || v > 23 {
		return fmt.Errorf("Invalid value. Please send a whole number from 0 to 23.")
	}
	cfg.Schedule.DayStartHour = &v
	return nil
}

// --- flow entry points (commands) ---

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
	b.renderPathsMenu(chatID, 0, "paths")
}

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
	b.renderAlertsMenu(chatID, 0, "alerts")
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
	b.renderHardstopMenu(chatID, 0, "hardstop", "")
}

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
	b.renderScheduleMenu(chatID, 0, "schedule")
}

// handleHardstopCommand implements /hardstop. "trigger" fires a manual hard
// stop; anything else opens the hard-stop menu.
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

// --- path helpers ---

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
