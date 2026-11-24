package bot

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func inSummaryWindow(now time.Time) bool {
	return now.Hour() == 9 && now.Minute() <= 1
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

func buildWeeklySummary(sc storage.Store, now time.Time) (string, money.Cents, error) {
	since := storage.AsStoredClock(now).AddDate(0, 0, -7)
	cashSessions, err := sc.GetCashSessionsSince(since)
	if err != nil {
		return "", 0, fmt.Errorf("loading cash sessions: %w", err)
	}
	tournaments, err := sc.GetTournamentsSince(since)
	if err != nil {
		return "", 0, fmt.Errorf("loading tournaments: %w", err)
	}
	rawLedger, err := sc.GetLedgerEntriesSince(since)
	if err != nil {
		return "", 0, fmt.Errorf("loading ledger entries: %w", err)
	}
	var netCents money.Cents
	for _, e := range rawLedger {
		if e.Type == "cash_session" || e.Type == "tournament" {
			netCents += e.AmountCents
		}
	}

	var totalHands int
	for _, s := range cashSessions {
		totalHands += s.Hands
	}
	sessionCount := len(cashSessions) + len(tournaments)
	bb100, _, _ := weightedCashAverages(cashSessions)

	return fmt.Sprintf(
		"📊 Weekly Summary\nSessions: %d\nNet: %s\nBB/100: %.1f\nSpin & Go ROI: %s\nMTT ROI: %s\nHands: %s", sessionCount,
		FormatSignedUSD(netCents),
		bb100,
		formatSignedPercent(roi(tournaments, "spingo")),
		formatSignedPercent(roi(tournaments, "mtt")),
		commas(totalHands),
	), netCents, nil
}

func buildMonthlySummary(sc storage.Store, now time.Time) (string, money.Cents, error) {
	stored := storage.AsStoredClock(now)
	firstOfThisMonth := time.Date(stored.Year(), stored.Month(), 1, 0, 0, 0, 0, time.UTC)
	firstOfPrevMonth := firstOfThisMonth.AddDate(0, -1, 0)

	rawCashSessions, err := sc.GetCashSessionsSince(firstOfPrevMonth)
	if err != nil {
		return "", 0, fmt.Errorf("loading cash sessions: %w", err)
	}
	rawTournaments, err := sc.GetTournamentsSince(firstOfPrevMonth)
	if err != nil {
		return "", 0, fmt.Errorf("loading tournaments: %w", err)
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

	var cashNetCents, spinsNetCents, mttNetCents money.Cents
	var totalHands int
	for _, s := range cashSessions {
		cashNetCents += s.NetCents
		totalHands += s.Hands
	}
	for _, t := range tournaments {
		if t.BaseType() == "spingo" {
			spinsNetCents += t.NetCents
		} else {
			mttNetCents += t.NetCents
		}
	}
	sessionCount := len(cashSessions) + len(tournaments)

	rawLedger, err := sc.GetLedgerEntriesSince(firstOfPrevMonth)
	if err != nil {
		return "", 0, fmt.Errorf("loading ledger entries: %w", err)
	}
	var totalNetCents money.Cents
	for _, e := range rawLedger {
		if e.Date.Before(firstOfThisMonth) && (e.Type == "cash_session" || e.Type == "tournament") {
			totalNetCents += e.AmountCents
		}
	}
	bb100, _, _ := weightedCashAverages(cashSessions)

	return fmt.Sprintf(
		"📅 Monthly Summary — %s\nCash Net: %s\nSpins Net: %s\nMTT Net: %s\nTotal: %s\nBB/100: %.1f\nSpins ROI: %s\nMTT ROI: %s\nHands: %s\nVolume: %d sessions", firstOfPrevMonth.Format("January 2006"),
		FormatSignedUSD(cashNetCents),
		FormatSignedUSD(spinsNetCents),
		FormatSignedUSD(mttNetCents),
		FormatSignedUSD(totalNetCents),
		bb100,
		formatSignedPercent(roi(tournaments, "spingo")),
		formatSignedPercent(roi(tournaments, "mtt")),
		commas(totalHands),
		sessionCount,
	), totalNetCents, nil
}

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

func weightedCashAverages(sessions []parsers.CashSession) (bb100, vpip, pfr float64) {
	res := weightedAverages(sessions, 3, func(s parsers.CashSession) (float64, []float64, bool) {
		return float64(s.Hands), []float64{s.BB100, s.VPIP, s.PFR}, true
	})
	return res[0], res[1], res[2]
}

func weightedTournamentAverages(tournaments []parsers.Tournament) (vpip, pfr float64) {
	res := weightedAverages(tournaments, 2, func(t parsers.Tournament) (float64, []float64, bool) {
		if t.Hands == nil || t.VPIP == nil || t.PFR == nil {
			return 0, nil, false
		}
		return float64(*t.Hands), []float64{*t.VPIP, *t.PFR}, true
	})
	return res[0], res[1]
}

func roi(tournaments []parsers.Tournament, tType string) float64 {
	var netSumCents, buyInSumCents money.Cents
	for _, t := range tournaments {
		if t.BaseType() != tType {
			continue
		}
		netSumCents += t.NetCents
		buyInSumCents += t.BuyInCents
	}
	if buyInSumCents == 0 {
		return 0
	}
	return money.Ratio(netSumCents, buyInSumCents) * 100
}

func FormatSignedUSD(amountCents money.Cents) string {
	text := amountCents.String()
	if amountCents < 0 {
		return "-$" + strings.TrimPrefix(text, "-")
	}
	return "+$" + text
}

func formatSignedPercent(v float64) string {
	sign := "+"
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%.0f%%", sign, v)
}

func formatPercent(v float64) string {
	return fmt.Sprintf("%.1f%%", v)
}

func formatPercentPtr(v *float64) string {
	if v == nil {
		return "N/A"
	}
	return formatPercent(*v)
}

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

func netSince(sc storage.Store, since time.Time) (money.Cents, error) {
	entries, err := sc.GetLedgerEntriesSince(since)
	if err != nil {
		return 0, err
	}
	var netCents money.Cents
	for _, e := range entries {
		netCents += e.AmountCents
	}
	return netCents, nil
}

func (b *Bot) handleStatus(chatID int64) {
	bankrollCents, err := b.store.GetCurrentBankroll()
	if err != nil {
		slog.Error("getting bankroll", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}

	now := storage.AsStoredClock(b.now())
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	startOfWeek := now.AddDate(0, 0, -7)
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	todayNetCents, err := netSince(b.store, startOfToday)
	if err != nil {
		slog.Error("getting today's netCents", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	weekNetCents, err := netSince(b.store, startOfWeek)
	if err != nil {
		slog.Error("getting week's netCents", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}
	monthNetCents, err := netSince(b.store, startOfMonth)
	if err != nil {
		slog.Error("getting month's netCents", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}

	text := fmt.Sprintf("💰 Status\nBankroll: $%s\nToday: %s\nWeek: %s\nMonth: %s", bankrollCents, FormatSignedUSD(todayNetCents), FormatSignedUSD(weekNetCents), FormatSignedUSD(monthNetCents))

	text += b.lockStatus()

	send(b.tg, chatID, text)
}

func SpinGoPrizePoolText(t parsers.Tournament) string {
	if t.PrizePoolDescription != "" {
		return t.PrizePoolDescription
	}
	if t.Recovered() && t.PrizePoolCents == 0 {
		return "Unknown"
	}
	mult := 0.0
	if t.Multiplier != nil {
		mult = *t.Multiplier
	}
	return fmt.Sprintf("$%s (%gx)", t.PrizePoolCents, mult)
}

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

	bankrollCents, err := b.store.GetCurrentBankroll()
	if err != nil {
		slog.Error("getting bankroll", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}

	if lastTourn == nil || (lastCash != nil && lastCash.Date.After(lastTourn.Date)) {
		send(b.tg, chatID, fmt.Sprintf("✅ Cash Session\nStake: %s\nNet: %s\nHands: %d\nDuration: %d min\nVPIP: %s\nPFR: %s\nBankroll: $%s", lastCash.Stake, FormatSignedUSD(lastCash.NetCents), lastCash.Hands, lastCash.DurationMin,
			formatPercent(lastCash.VPIP), formatPercent(lastCash.PFR), bankrollCents))
		return
	}

	if lastTourn.BaseType() == "spingo" {
		send(b.tg, chatID, fmt.Sprintf("🎰 Spin & Go\nBuy-in: $%s\nPrize pool: %s\nFinish: %s\nNet: %s\nVPIP: %s\nPFR: %s\nBankroll: $%s", lastTourn.BuyInCents, SpinGoPrizePoolText(*lastTourn), Ordinal(lastTourn.FinishPos), FormatSignedUSD(lastTourn.NetCents),
			formatPercentPtr(lastTourn.VPIP), formatPercentPtr(lastTourn.PFR), bankrollCents))
		return
	}
	send(b.tg, chatID, fmt.Sprintf("🏆 MTT\nBuy-in: $%s\nFinish: %s\nNet: %s\nVPIP: %s\nPFR: %s\nBankroll: $%s", lastTourn.BuyInCents, TournamentFinishText(*lastTourn), FormatSignedUSD(lastTourn.NetCents),
		formatPercentPtr(lastTourn.VPIP), formatPercentPtr(lastTourn.PFR), bankrollCents))
}

func (b *Bot) handleStats(chatID int64, args string) {
	gameType, period := ParseStatsArgs(args)
	since := statsSince(period, b.now())
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

func statsSince(period string, now time.Time) time.Time {
	now = storage.AsStoredClock(now)
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
	var netCents money.Cents
	var hands int
	for _, s := range sessions {
		netCents += s.NetCents
		hands += s.Hands
	}
	bb100, vpip, pfr := weightedCashAverages(sessions)
	return fmt.Sprintf("📈 Cash Stats (%s)\nNet: %s\nBB/100: %.1f\nVPIP: %s\nPFR: %s\nHands: %d\nSessions: %d", periodLabel, FormatSignedUSD(netCents), bb100, formatPercent(vpip), formatPercent(pfr), hands, len(sessions)), nil
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
	var netCents money.Cents
	for _, t := range filtered {
		netCents += t.NetCents
	}
	r := roi(filtered, tType)
	vpip, pfr := weightedTournamentAverages(filtered)

	if !includeAvgFinish {
		return fmt.Sprintf("%s %s Stats (%s)\nNet: %s\nROI: %s\nVPIP: %s\nPFR: %s\nGames: %d", emoji, label, periodLabel, FormatSignedUSD(netCents), formatSignedPercent(r), formatPercent(vpip), formatPercent(pfr), len(filtered)), nil
	}

	var avgFinishSum float64
	var cnt int
	for _, t := range filtered {
		if t.TotalPlayers > 0 {
			avgFinishSum += float64(t.FinishPos) / float64(t.TotalPlayers) * 100
			cnt++
		}
	}
	message := fmt.Sprintf("%s %s Stats (%s)\nNet: %s\nROI: %s\nVPIP: %s\nPFR: %s\nGames: %d", emoji, label, periodLabel, FormatSignedUSD(netCents), formatSignedPercent(r), formatPercent(vpip), formatPercent(pfr), len(filtered))
	if cnt > 0 {
		message += fmt.Sprintf("\nAvg finish: %.0f%%", avgFinishSum/float64(cnt))
	}
	return message, nil
}

func (b *Bot) lockStatus() string {
	if b.lockStatusText != nil {
		text, err := b.lockStatusText()
		if err != nil {
			slog.Error("reading saved lock status", "err", err)
			return "\n⚠️ Collector lock status unavailable."
		}
		return text
	}
	if b.currentLock == nil {
		return ""
	}
	lock, err := b.currentLock()
	if err != nil {
		return "\n⚠️ Collector lock status unavailable."
	}
	if lock != nil && lock.ExpiresAt.After(time.Now()) {
		return "\n🛑 " + hardstop.LockDetails(*lock, b.now())
	}
	return ""
}
