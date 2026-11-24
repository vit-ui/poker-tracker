package bot

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func EvaluateSessionLossAlert(session parsers.CashSession, cfg *config.Config, bankrollCents func() money.Cents, alertChan chan<- string) {
	if cfg.Alerts.SessionLossBuyins == nil || session.BuyInCents == 0 || session.NetCents >= 0 {
		return
	}

	if !money.LossExceeds(session.NetCents, session.BuyInCents, *cfg.Alerts.SessionLossBuyins) {
		return
	}

	buyins := -money.Ratio(session.NetCents, session.BuyInCents)
	PostAlert(alertChan, fmt.Sprintf("⚠️ Session loss alert\nLost %.1f buy-ins this session ($%s)\nBankroll: $%s", buyins, strings.TrimPrefix(session.NetCents.String(), "-"), bankrollCents()))
}

func EvaluateDownswingAlert(buyInCents money.Cents, cfg *config.Config, ledger []storage.LedgerEntry, bankrollCents money.Cents, now time.Time, alertChan chan<- string) {
	if cfg.Alerts.DownswingBuyins == nil || buyInCents == 0 {
		return
	}

	peakCents, currentCents := hardstop.DownswingEquityCurve(ledger, now)
	if !hardstop.DownswingExceeds(peakCents, currentCents, buyInCents, *cfg.Alerts.DownswingBuyins) {
		return
	}

	buyins := money.Ratio(peakCents, buyInCents) - money.Ratio(currentCents, buyInCents)
	PostAlert(alertChan, fmt.Sprintf("⚠️ Downswing alert\nDown %.1f buy-ins from your 30-day peak\nPeak: $%s  Current: $%s\nBankroll: $%s", buyins, peakCents, currentCents, bankrollCents))
}

func EvaluateLosingStreakAlert(cfg *config.Config, ledger []storage.LedgerEntry, bankrollCents money.Cents, alertChan chan<- string) (fired bool) {
	if cfg.Alerts.LosingStreakCount == nil {
		return false
	}

	if !hardstop.LosingStreakTriggered(ledger, *cfg.Alerts.LosingStreakCount) {
		return false
	}

	PostAlert(alertChan, fmt.Sprintf("⚠️ Losing streak alert\n%d losing sessions in a row\nBankroll: $%s", *cfg.Alerts.LosingStreakCount, bankrollCents))
	return true
}

func EvaluateDailyLossAlert(cfg *config.Config, ledger []storage.LedgerEntry, bankrollCents money.Cents, now time.Time, alertChan chan<- string) (fired bool) {
	if cfg.Alerts.DailyLossCents == nil {
		return false
	}

	entries := hardstop.PokerDaySessionEntries(ledger, cfg, now)
	if !hardstop.DailyLossExceeds(entries, *cfg.Alerts.DailyLossCents) {
		return false
	}

	var netCents money.Cents
	for _, e := range entries {
		netCents += e.AmountCents
	}
	PostAlert(alertChan, fmt.Sprintf("⚠️ Daily loss alert\nDown $%s across all games this poker day\nBankroll: $%s", strings.TrimPrefix(netCents.String(), "-"), bankrollCents))
	return true
}

func EvaluateSpingoDailyLossAlert(cfg *config.Config, dayTournaments []parsers.Tournament, bankrollCents money.Cents, alertChan chan<- string) (fired bool) {
	if cfg.Alerts.SpingoDailyLossCents == nil {
		return false
	}

	spingos := hardstop.SpingoOnly(dayTournaments)
	if !hardstop.SpingoDailyLossExceeds(spingos, *cfg.Alerts.SpingoDailyLossCents) {
		return false
	}

	var netCents money.Cents
	for _, t := range spingos {
		netCents += t.NetCents
	}
	PostAlert(alertChan, fmt.Sprintf("⚠️ Spin & Go drain alert\nDown $%s on Spin & Gos this poker day\nBankroll: $%s", strings.TrimPrefix(netCents.String(), "-"), bankrollCents))
	return true
}

func EvaluateStakeChangeAlert(session parsers.CashSession, sc storage.Store, alertChan chan<- string) error {
	sessions, err := sc.GetCashSessionsSince(time.Time{})
	if err != nil {
		return err
	}
	prev, ok := previousStake(sessions, session)
	if !ok || prev == session.Stake {
		return nil
	}
	PostAlert(alertChan, fmt.Sprintf("🃏 Stake change detected\nPrevious cash session: %s\nThis session: %s", prev, session.Stake))
	return nil
}

func previousStake(sessions []parsers.CashSession, current parsers.CashSession) (stake string, ok bool) {
	cutoff := current.Date.Truncate(time.Minute)
	best := -1
	for i, s := range sessions {
		if !s.Date.Before(cutoff) {
			continue
		}
		if best == -1 || !s.Date.Before(sessions[best].Date) {
			best = i
		}
	}
	if best == -1 {
		return "", false
	}
	return sessions[best].Stake, true
}

func PostAlert(alerts chan<- string, text string) {
	select {
	case alerts <- text:
	default:
		slog.Error("Telegram alert queue is full")
	}
}
