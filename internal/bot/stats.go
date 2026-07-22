package bot

// Read-side commands and scheduled summaries: /status, /lastsession,
// /stats, the weekly/monthly summary builders, and the shared display/math
// helpers they use.
//
// Every date window in this file is computed in the stored-date coordinate
// system (storage.AsStoredClock — the machine's local wall-clock digits
// read as UTC), because that's the coordinate system stored dates live in
// on both backends. Computing "today" as a real local-zone instant instead
// shifts the boundary by the machine's UTC offset — on the player's UTC-3
// machine that silently dropped any session played between midnight and
// 3am from /status and /stats today, while the hard stop's poker-day math
// (which always compensated — see hardstop.PokerDayStart) counted it.

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// inSummaryWindow reports whether now falls within the 09:00–09:01 local
// trigger window for scheduled summaries.
func inSummaryWindow(now time.Time) bool {
	return now.Hour() == 9 && now.Minute() <= 1
}

// sameDay reports whether a and b fall on the same calendar day.
func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// buildWeeklySummary formats the past 7 days of activity.
func buildWeeklySummary(sc storage.Store, now time.Time) (string, error) {
	since := storage.AsStoredClock(now).AddDate(0, 0, -7)
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
		FormatSignedUSD(netUSD),
		bb100,
		formatSignedPercent(roi(tournaments, "spingo")),
		formatSignedPercent(roi(tournaments, "mtt")),
		commas(totalHands),
	), nil
}

// buildMonthlySummary formats the previous calendar month's activity.
func buildMonthlySummary(sc storage.Store, now time.Time) (string, error) {
	stored := storage.AsStoredClock(now)
	firstOfThisMonth := time.Date(stored.Year(), stored.Month(), 1, 0, 0, 0, 0, time.UTC)
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
		FormatSignedUSD(cashNet),
		FormatSignedUSD(spinsNet),
		FormatSignedUSD(mttNet),
		FormatSignedUSD(totalNet),
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

// FormatSignedUSD renders a net amount as "+$5.50" / "-$12.75" — the single
// signed-USD formatter, shared by /stats, /status, the flow displays, and
// main.go's post-session and post-tournament alerts. Do not hand-roll the
// sign/abs split at a call site.
func FormatSignedUSD(v float64) string {
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

// Ordinal formats n as "1st", "2nd", "3rd", "4th", etc. Exported because
// main.go's post-tournament alert needs the same formatting — previously
// each package kept its own identical copy.
func Ordinal(n int) string {
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
func (b *Bot) handleStatus(chatID int64) {
	bankroll, err := b.store.GetCurrentBankroll()
	if err != nil {
		slog.Error("getting bankroll", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}

	now := storage.AsStoredClock(time.Now())
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	startOfWeek := now.AddDate(0, 0, -7)
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	todayNet, err := netSince(b.store, startOfToday)
	if err != nil {
		slog.Error("getting today's net", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	weekNet, err := netSince(b.store, startOfWeek)
	if err != nil {
		slog.Error("getting week's net", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	monthNet, err := netSince(b.store, startOfMonth)
	if err != nil {
		slog.Error("getting month's net", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}

	text := fmt.Sprintf("💰 Status\nBankroll: $%.2f\nToday: %s\nWeek: %s\nMonth: %s",
		bankroll, FormatSignedUSD(todayNet), FormatSignedUSD(weekNet), FormatSignedUSD(monthNet))

	lock, err := config.LoadLock(b.lockfilePath)
	if err != nil {
		slog.Error("loading lockfile", "err", err)
	} else if lock != nil && !config.IsLockExpired(lock) {
		text += fmt.Sprintf("\n🛑 Locked until %s", lock.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}

	send(b.tg, chatID, text)
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
func (b *Bot) handleLastSession(chatID int64) {
	cashSessions, err := b.store.GetCashSessionsSince(time.Time{})
	if err != nil {
		slog.Error("getting cash sessions", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	tournaments, err := b.store.GetTournamentsSince(time.Time{})
	if err != nil {
		slog.Error("getting tournaments", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
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
		send(b.tg, chatID, "No sessions recorded yet.")
		return
	}

	bankroll, err := b.store.GetCurrentBankroll()
	if err != nil {
		slog.Error("getting bankroll", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}

	if lastTourn == nil || (lastCash != nil && lastCash.Date.After(lastTourn.Date)) {
		send(b.tg, chatID, fmt.Sprintf("✅ Cash Session\nStake: %s\nNet: %s\nHands: %d\nDuration: %d min\nVPIP: %s\nPFR: %s\nBankroll: $%.2f",
			lastCash.Stake, FormatSignedUSD(lastCash.NetUSD), lastCash.Hands, lastCash.DurationMin,
			formatPercent(lastCash.VPIP), formatPercent(lastCash.PFR), bankroll))
		return
	}

	if lastTourn.BaseType() == "spingo" {
		send(b.tg, chatID, fmt.Sprintf("🎰 Spin & Go\nBuy-in: $%.2f\nPrize pool: %s\nFinish: %s\nNet: %s\nVPIP: %s\nPFR: %s\nBankroll: $%.2f",
			lastTourn.BuyInUSD, SpinGoPrizePoolText(*lastTourn), Ordinal(lastTourn.FinishPos), FormatSignedUSD(lastTourn.NetUSD),
			formatPercentPtr(lastTourn.VPIP), formatPercentPtr(lastTourn.PFR), bankroll))
		return
	}
	send(b.tg, chatID, fmt.Sprintf("🏆 MTT\nBuy-in: $%.2f\nFinish: %s / %d\nNet: %s\nVPIP: %s\nPFR: %s\nBankroll: $%.2f",
		lastTourn.BuyInUSD, Ordinal(lastTourn.FinishPos), lastTourn.TotalPlayers, FormatSignedUSD(lastTourn.NetUSD),
		formatPercentPtr(lastTourn.VPIP), formatPercentPtr(lastTourn.PFR), bankroll))
}

// handleStats implements /stats.
func (b *Bot) handleStats(chatID int64, args string) {
	gameType, period := ParseStatsArgs(args)
	since := statsSince(period)
	periodLabel := period
	if periodLabel == "" {
		periodLabel = "all"
	}

	if gameType == "" || gameType == "cash" {
		msg, err := cashStatsMessage(b.store, since, periodLabel)
		if err != nil {
			slog.Error("getting cash stats", "err", err)
			send(b.tg, chatID, "Something went wrong. Check logs.")
			return
		}
		send(b.tg, chatID, msg)
	}
	if gameType == "" || gameType == "spins" {
		msg, err := tournamentStatsMessage(b.store, since, "spingo", periodLabel, "Spin & Go", "🎰", false)
		if err != nil {
			slog.Error("getting spins stats", "err", err)
			send(b.tg, chatID, "Something went wrong. Check logs.")
			return
		}
		send(b.tg, chatID, msg)
	}
	if gameType == "" || gameType == "mtt" {
		msg, err := tournamentStatsMessage(b.store, since, "mtt", periodLabel, "MTT", "🏆", true)
		if err != nil {
			slog.Error("getting mtt stats", "err", err)
			send(b.tg, chatID, "Something went wrong. Check logs.")
			return
		}
		send(b.tg, chatID, msg)
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

// statsSince computes the window start for a /stats period, in the
// stored-date coordinate system (see the file comment above).
func statsSince(period string) time.Time {
	now := storage.AsStoredClock(time.Now())
	switch period {
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case "week":
		return now.AddDate(0, 0, -7)
	case "month":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
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
		periodLabel, FormatSignedUSD(net), bb100, formatPercent(vpip), formatPercent(pfr), hands, len(sessions)), nil
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
			emoji, label, periodLabel, FormatSignedUSD(net), formatSignedPercent(r), formatPercent(vpip), formatPercent(pfr), len(filtered)), nil
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
		emoji, label, periodLabel, FormatSignedUSD(net), formatSignedPercent(r), formatPercent(vpip), formatPercent(pfr), len(filtered), avgFinish), nil
}
