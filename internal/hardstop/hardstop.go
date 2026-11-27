package hardstop

import (
	"fmt"
	"slices"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func EvaluateSessionLoss(session parsers.CashSession, cfg *config.Config, onTrigger func(reason string, durationHours int)) error {
	h := cfg.Hardstop
	if h.SessionLossBuyins != nil && session.NetCents < 0 &&
		money.LossExceeds(session.NetCents, session.BuyInCents, *h.SessionLossBuyins) {
		if h.DurationHours == nil {
			return fmt.Errorf("session_loss_buyins configured without duration_hours")
		}
		onTrigger("session_loss_buyins", *h.DurationHours)
	}
	return nil
}

func Evaluate(session parsers.CashSession, cfg *config.Config, ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament, now time.Time, onTrigger func(reason string, durationHours int)) error {
	h := cfg.Hardstop
	if h.ConsecutiveLosses == nil && h.DailyLossCents == nil &&
		h.SpingoDailyLossCents == nil && h.DownswingBuyins == nil && h.DurationHours == nil {
		return nil
	}

	if h.ConsecutiveLosses != nil && LosingStreakTriggered(ledger, *h.ConsecutiveLosses) {
		if h.DurationHours == nil {
			return fmt.Errorf("consecutive_losses configured without duration_hours")
		}
		onTrigger("consecutive_losses", *h.DurationHours)
		return nil
	}

	if triggered, err := evaluateDownswing(cfg, ledger, session.BuyInCents, now, onTrigger); err != nil {
		return err
	} else if triggered {
		return nil
	}

	_, err := evaluateDailyLoss(cfg, ledger, dayTournaments, now, onTrigger)
	return err
}

func LosingStreakTriggered(entries []storage.LedgerEntry, n int) bool {
	sessionRows := recentSessionRows(entries, n)
	return len(sessionRows) == n && consecutiveLossStreak(sessionRows) == len(sessionRows)
}

func consecutiveLossStreak(entries []storage.LedgerEntry) int {
	count := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].AmountCents >= 0 {
			break
		}
		count++
	}
	return count
}

func recentSessionRows(entries []storage.LedgerEntry, n int) []storage.LedgerEntry {
	rows := sessionOnly(entries)
	sortBySessionDate(rows)
	if len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	return rows
}

func sortBySessionDate(rows []storage.LedgerEntry) {
	slices.SortStableFunc(rows, func(a, b storage.LedgerEntry) int {
		return a.Date.Compare(b.Date)
	})
}

func PokerDayStart(cfg *config.Config, now time.Time) time.Time {
	hour := 6
	if cfg.Schedule.DayStartHour != nil {
		hour = *cfg.Schedule.DayStartHour
	}
	stored := storage.AsStoredClock(now)
	start := time.Date(stored.Year(), stored.Month(), stored.Day(), hour, 0, 0, 0, time.UTC)
	if stored.Before(start) {
		start = start.AddDate(0, 0, -1)
	}
	return start
}

func rollingWindowStart(now time.Time, window time.Duration) time.Time {
	return storage.AsStoredClock(now).Add(-window)
}

func PokerDaySessionEntries(entries []storage.LedgerEntry, cfg *config.Config, now time.Time) []storage.LedgerEntry {
	start := PokerDayStart(cfg, now)
	var day []storage.LedgerEntry
	for _, e := range entries {
		if e.IsPokerResult() && !e.Date.Before(start) {
			day = append(day, e)
		}
	}
	return day
}

func inRollingWindow(entryDate, cutoff time.Time) bool {
	return entryDate.After(cutoff)
}

func SpingoOnly(tournaments []parsers.Tournament) []parsers.Tournament {
	var result []parsers.Tournament
	for _, t := range tournaments {
		if t.BaseType() == "spingo" {
			result = append(result, t)
		}
	}
	return result
}

func sessionOnly(entries []storage.LedgerEntry) []storage.LedgerEntry {
	var result []storage.LedgerEntry
	for _, e := range entries {
		if e.IsPokerResult() {
			result = append(result, e)
		}
	}
	return result
}

func ComputeEquityCurve(rows []storage.LedgerEntry) (peakCents, currentCents money.Cents) {
	var sumCents money.Cents
	for _, r := range rows {
		sumCents += r.AmountCents
		if sumCents > peakCents {
			peakCents = sumCents
		}
	}
	return peakCents, sumCents
}

func DownswingEquityCurve(entries []storage.LedgerEntry, now time.Time) (peakCents, currentCents money.Cents) {
	results := downswingResults(entries, now)
	sortBySessionDate(results)
	return ComputeEquityCurve(results)
}

func downswingResults(entries []storage.LedgerEntry, now time.Time) []storage.LedgerEntry {
	const downswingWindow = 30 * 24 * time.Hour
	cutoff := rollingWindowStart(now, downswingWindow)
	var results []storage.LedgerEntry
	for _, entry := range entries {
		if entry.IsPokerResult() && inRollingWindow(entry.Date, cutoff) {
			results = append(results, entry)
		}
	}
	return results
}

func DownswingExceeds(peakCents, currentCents, buyInCents money.Cents, thresholdBuyins float64) bool {
	return money.DifferenceExceeds(peakCents, currentCents, buyInCents, thresholdBuyins)
}

func evaluateDownswing(cfg *config.Config, ledger []storage.LedgerEntry, buyInCents money.Cents, now time.Time, onTrigger func(reason string, durationHours int)) (triggered bool, err error) {
	h := cfg.Hardstop
	if h.DownswingBuyins == nil || buyInCents == 0 {
		return false, nil
	}

	peakCents, currentCents := DownswingEquityCurve(ledger, now)
	if DownswingExceeds(peakCents, currentCents, buyInCents, *h.DownswingBuyins) {
		if h.DurationHours == nil {
			return false, fmt.Errorf("downswing_buyins configured without duration_hours")
		}
		onTrigger("downswing_buyins", *h.DurationHours)
		return true, nil
	}
	return false, nil
}

func DailyLossExceeds(entries []storage.LedgerEntry, thresholdCents money.Cents) bool {
	var netCents money.Cents
	for _, e := range entries {
		netCents += e.AmountCents
	}
	return netCents < -thresholdCents
}

func SpingoDailyLossExceeds(tournaments []parsers.Tournament, thresholdCents money.Cents) bool {
	var netCents money.Cents
	for _, t := range tournaments {
		netCents += t.NetCents
	}
	return netCents < -thresholdCents
}

func evaluateDailyLoss(cfg *config.Config, ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament, now time.Time, onTrigger func(reason string, durationHours int)) (triggered bool, err error) {
	h := cfg.Hardstop
	if h.SpingoDailyLossCents == nil && h.DailyLossCents == nil {
		return false, nil
	}

	if h.SpingoDailyLossCents != nil {
		if SpingoDailyLossExceeds(SpingoOnly(dayTournaments), *h.SpingoDailyLossCents) {
			if h.DurationHours == nil {
				return false, fmt.Errorf("spingo_daily_loss_cents configured without duration_hours")
			}
			onTrigger("spingo_daily_loss_cents", *h.DurationHours)
			return true, nil
		}
	}

	if h.DailyLossCents != nil {
		if DailyLossExceeds(PokerDaySessionEntries(ledger, cfg, now), *h.DailyLossCents) {
			if h.DurationHours == nil {
				return false, fmt.Errorf("daily_loss_cents configured without duration_hours")
			}
			onTrigger("daily_loss_cents", *h.DurationHours)
			return true, nil
		}
	}

	return false, nil
}

func EvaluateTournament(t parsers.Tournament, cfg *config.Config, ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament, now time.Time, onTrigger func(reason string, durationHours int)) error {
	if triggered, err := evaluateDownswing(cfg, ledger, t.BuyInCents, now, onTrigger); err != nil {
		return err
	} else if triggered {
		return nil
	}

	_, err := evaluateDailyLoss(cfg, ledger, dayTournaments, now, onTrigger)
	return err
}

func ManualTrigger(configPath string, overrideHours *int, onTrigger func(reason string, durationHours int)) error {
	durationHours := overrideHours
	if durationHours == nil {
		cfg, err := config.LoadConfig(configPath)
		if err != nil {
			return err
		}
		durationHours = cfg.Hardstop.DurationHours
	}
	if durationHours == nil {
		return fmt.Errorf("no saved duration; use /hardstop now 10h or configure one with /hardstop")
	}
	if _, err := Duration(*durationHours); err != nil {
		return err
	}
	onTrigger("manual", *durationHours)
	return nil
}

func BuildTrigger(reason string, durationHours int, now time.Time) (config.LockFile, string, error) {
	duration, err := Duration(durationHours)
	if err != nil {
		return config.LockFile{}, "", err
	}
	expiresAt := now.Add(duration)
	lock := config.LockFile{
		TriggeredAt:   now,
		ExpiresAt:     expiresAt,
		Reason:        reason,
		DurationHours: durationHours,
	}
	alert := LockMessage(lock, now.In(time.Local))
	return lock, alert, nil
}

func LockMessage(lock config.LockFile, now time.Time) string {
	return fmt.Sprintf("🛑 Hard Stop triggered\nReason: %s\n%s", reasonPhrase(lock.Reason), LockDetails(lock, now))
}

func reasonPhrase(reason string) string {
	switch reason {
	case "session_loss_buyins":
		return "Session loss limit reached"
	case "consecutive_losses":
		return "Too many losing sessions in a row"
	case "downswing_buyins":
		return "30-day downswing limit reached"
	case "daily_loss_cents":
		return "Daily loss limit reached"
	case "spingo_daily_loss_cents":
		return "Spin & Go daily loss limit reached"
	case "manual":
		return "Triggered manually"
	default:
		return reason
	}
}
