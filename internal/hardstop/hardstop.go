// Package hardstop implements the hard stop trigger: detecting loss conditions,
// killing PokerStars, and writing/reading the lockfile.
package hardstop

import (
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sheets"
)

// Evaluate checks hard stop conditions after a cash session and triggers a
// hard stop if any condition is met.
func Evaluate(session parsers.CashSession, sheetsClient *sheets.Client, configPath, lockfilePath string, alertChan chan<- string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	h := cfg.Hardstop
	if h.SessionLossBuyins == nil && h.ConsecutiveLosses == nil && h.DailyLossUSD == nil &&
		h.SpingoDailyLossUSD == nil && h.DownswingBuyins == nil && h.DurationHours == nil {
		return nil
	}

	if h.SessionLossBuyins != nil && session.NetUSD < 0 &&
		sessionLossExceeds(math.Abs(session.NetUSD), session.BuyInUSD, *h.SessionLossBuyins) {
		if h.DurationHours == nil {
			return fmt.Errorf("session_loss_buyins configured without duration_hours")
		}
		trigger("session_loss_buyins", *h.DurationHours, lockfilePath, alertChan)
		return nil
	}

	if h.ConsecutiveLosses != nil {
		rows, err := sheetsClient.GetRecentLedgerRows(*h.ConsecutiveLosses)
		if err != nil {
			return err
		}
		var sessionRows []sheets.LedgerEntry
		for _, row := range rows {
			if row.Type == "cash_session" || row.Type == "tournament" {
				sessionRows = append(sessionRows, row)
			}
		}
		if len(rows) == *h.ConsecutiveLosses && consecutiveLossStreak(sessionRows) == len(rows) {
			if h.DurationHours == nil {
				return fmt.Errorf("consecutive_losses configured without duration_hours")
			}
			trigger("consecutive_losses", *h.DurationHours, lockfilePath, alertChan)
			return nil
		}
	}

	if triggered, err := evaluateDownswing(cfg, sheetsClient, session.BuyInUSD, lockfilePath, alertChan); err != nil {
		return err
	} else if triggered {
		return nil
	}

	_, err = evaluateDailyLoss(cfg, sheetsClient, lockfilePath, alertChan)
	return err
}

// sessionLossExceeds reports whether a session loss of sessionLoss USD
// exceeds thresholdBuyins × buyInUSD.
func sessionLossExceeds(sessionLoss, buyInUSD, thresholdBuyins float64) bool {
	return sessionLoss > thresholdBuyins*buyInUSD
}

// consecutiveLossStreak returns how many of the most recent contiguous
// entries at the end of entries (in chronological order) have a negative
// AmountUSD. Counting stops at the first non-negative entry.
func consecutiveLossStreak(entries []sheets.LedgerEntry) int {
	count := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].AmountUSD >= 0 {
			break
		}
		count++
	}
	return count
}

// pokerDayStart computes the start of the poker day containing now, using
// cfg.Schedule.DayStartHour (default 6) as the daily reset hour.
//
// Session and tournament dates round-trip through Google Sheets via
// dateFormat, which carries no zone offset (see sheets.go) — so a date
// written as local wall-clock "06:30" always reads back as the instant
// 06:30 UTC, regardless of the machine's real zone. To stay comparable
// against those dates, this reinterprets now's local wall-clock digits as
// UTC rather than converting to now's true UTC instant.
func pokerDayStart(cfg *config.Config, now time.Time) time.Time {
	hour := 6
	if cfg.Schedule.DayStartHour != nil {
		hour = *cfg.Schedule.DayStartHour
	}
	local := now.Local()
	nowAsStoredWouldRead := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), time.UTC)
	start := time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, time.UTC)
	if nowAsStoredWouldRead.Before(start) {
		start = start.AddDate(0, 0, -1)
	}
	return start
}

// rollingWindowStart computes the start of a rolling window of the given
// duration ending at now, using the same UTC-wall-clock reinterpretation as
// pokerDayStart (see its comment) so it stays comparable against dates read
// back from Google Sheets.
func rollingWindowStart(now time.Time, window time.Duration) time.Time {
	local := now.Local()
	nowAsStoredWouldRead := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), time.UTC)
	return nowAsStoredWouldRead.Add(-window)
}

// inRollingWindow reports whether entryDate falls after cutoff. The
// boundary is exclusive: an entry dated exactly at cutoff (e.g. exactly 30
// days ago) is outside the window, not in it. cutoff is normally computed
// via rollingWindowStart.
func inRollingWindow(entryDate, cutoff time.Time) bool {
	return entryDate.After(cutoff)
}

// sessionOnly filters entries to cash_session and tournament rows only,
// excluding deposit/withdrawal transfers. RunningTotal includes transfers,
// so mixing them into an equity curve would read a large withdrawal as an
// equally large poker downswing.
func sessionOnly(entries []sheets.LedgerEntry) []sheets.LedgerEntry {
	var result []sheets.LedgerEntry
	for _, e := range entries {
		if e.Type == "cash_session" || e.Type == "tournament" {
			result = append(result, e)
		}
	}
	return result
}

// ComputeEquityCurve reconstructs a results-only equity curve from rows
// (expected in chronological order, already filtered to poker results) by
// summing AmountUSD cumulatively. peak is the highest point reached on that
// curve, including the implicit starting point of 0 before any row; current
// is the final point. Exported so the hard stop check (hardstop.go) and the
// downswing alert (bot.go) compute the exact same curve rather than each
// deriving their own.
func ComputeEquityCurve(rows []sheets.LedgerEntry) (peak, current float64) {
	var sum float64
	for _, r := range rows {
		sum += r.AmountUSD
		if sum > peak {
			peak = sum
		}
	}
	return peak, sum
}

// DownswingEquityCurve fetches Bankroll Ledger rows within the rolling
// 30-day window (see rollingWindowStart/inRollingWindow), filters them to
// poker results only (see sessionOnly), and returns the resulting equity
// curve. Shared by the hard stop check and the downswing alert.
func DownswingEquityCurve(sheetsClient *sheets.Client) (peak, current float64, err error) {
	cutoff := rollingWindowStart(time.Now(), 30*24*time.Hour)
	// GetLedgerEntriesSince is inclusive of cutoff, so this is a superset;
	// inRollingWindow below applies the precise exclusive boundary.
	candidates, err := sheetsClient.GetLedgerEntriesSince(cutoff)
	if err != nil {
		return 0, 0, err
	}
	var windowed []sheets.LedgerEntry
	for _, e := range candidates {
		if inRollingWindow(e.Date, cutoff) {
			windowed = append(windowed, e)
		}
	}
	peak, current = ComputeEquityCurve(sessionOnly(windowed))
	return peak, current, nil
}

// DownswingExceeds reports whether the equity-curve peak minus current
// exceeds thresholdBuyins × buyInUSD. Exported alongside ComputeEquityCurve
// so the alert side (bot.go) applies the exact same formula as the hard
// stop side.
func DownswingExceeds(peak, current, buyInUSD, thresholdBuyins float64) bool {
	return peak-current > thresholdBuyins*buyInUSD
}

// evaluateDownswing checks whether the rolling 30-day results-only equity
// curve has dropped from its peak by more than DownswingBuyins ×
// buyInUSD, and triggers a hard stop if so. buyInUSD of 0 (nil buy-in, or a
// satellite entry corrected to a ticket — see RecoverTicketPayout in
// bot.go) skips the check entirely: a 0 buy-in would make the threshold 0,
// so any downswing at all would trigger it.
func evaluateDownswing(cfg *config.Config, sheetsClient *sheets.Client, buyInUSD float64, lockfilePath string, alertChan chan<- string) (triggered bool, err error) {
	h := cfg.Hardstop
	if h.DownswingBuyins == nil || buyInUSD == 0 {
		return false, nil
	}

	peak, current, err := DownswingEquityCurve(sheetsClient)
	if err != nil {
		return false, err
	}

	if DownswingExceeds(peak, current, buyInUSD, *h.DownswingBuyins) {
		if h.DurationHours == nil {
			return false, fmt.Errorf("downswing_buyins configured without duration_hours")
		}
		trigger("downswing_buyins", *h.DurationHours, lockfilePath, alertChan)
		return true, nil
	}
	return false, nil
}

// dailyLossExceeds sums AmountUSD across entries (expected to already be
// filtered to the current poker day and Type == "cash_session" or
// "tournament") and reports whether that net P&L falls below -thresholdUSD.
// This is net P&L, not gross loss — a win elsewhere in the same day offsets
// losses.
func dailyLossExceeds(entries []sheets.LedgerEntry, thresholdUSD float64) bool {
	var net float64
	for _, e := range entries {
		net += e.AmountUSD
	}
	return net < -thresholdUSD
}

// spingoDailyLossExceeds sums NetUSD across tournaments (expected to already
// be filtered to the current poker day and Type == "spingo") and reports
// whether that net P&L falls below -thresholdUSD. Same net-P&L semantics as
// dailyLossExceeds, scoped to Spin & Go only.
func spingoDailyLossExceeds(tournaments []parsers.Tournament, thresholdUSD float64) bool {
	var net float64
	for _, t := range tournaments {
		net += t.NetUSD
	}
	return net < -thresholdUSD
}

// evaluateDailyLoss runs two independent daily-loss checks for the current
// poker day — a Spin & Go-specific one and a unified one across all game
// types — and triggers a hard stop if either has fallen below its
// configured threshold. Either check firing is sufficient; they are not
// mutually exclusive, but only the first one to fire actually triggers —
// triggered reports whether one did.
func evaluateDailyLoss(cfg *config.Config, sheetsClient *sheets.Client, lockfilePath string, alertChan chan<- string) (triggered bool, err error) {
	h := cfg.Hardstop
	if h.SpingoDailyLossUSD == nil && h.DailyLossUSD == nil {
		return false, nil
	}

	start := pokerDayStart(cfg, time.Now())

	if h.SpingoDailyLossUSD != nil {
		tournaments, err := sheetsClient.GetTournamentsSince(start)
		if err != nil {
			return false, err
		}
		var spingoToday []parsers.Tournament
		for _, t := range tournaments {
			if t.Type == "spingo" {
				spingoToday = append(spingoToday, t)
			}
		}
		if spingoDailyLossExceeds(spingoToday, *h.SpingoDailyLossUSD) {
			if h.DurationHours == nil {
				return false, fmt.Errorf("spingo_daily_loss_usd configured without duration_hours")
			}
			trigger("spingo_daily_loss_usd", *h.DurationHours, lockfilePath, alertChan)
			return true, nil
		}
	}

	if h.DailyLossUSD != nil {
		entries, err := sheetsClient.GetLedgerEntriesSince(start)
		if err != nil {
			return false, err
		}
		var sessionEntries []sheets.LedgerEntry
		for _, e := range entries {
			if e.Type == "cash_session" || e.Type == "tournament" {
				sessionEntries = append(sessionEntries, e)
			}
		}
		if dailyLossExceeds(sessionEntries, *h.DailyLossUSD) {
			if h.DurationHours == nil {
				return false, fmt.Errorf("daily_loss_usd configured without duration_hours")
			}
			trigger("daily_loss_usd", *h.DurationHours, lockfilePath, alertChan)
			return true, nil
		}
	}

	return false, nil
}

// EvaluateTournament checks hard stop conditions after a tournament (cash or
// Spin & Go). The downswing and daily loss checks re-evaluate against the
// whole account rather than filtering by t's type.
func EvaluateTournament(t parsers.Tournament, sheetsClient *sheets.Client, configPath, lockfilePath string, alertChan chan<- string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}

	if triggered, err := evaluateDownswing(cfg, sheetsClient, t.BuyInUSD, lockfilePath, alertChan); err != nil {
		return err
	} else if triggered {
		return nil
	}

	_, err = evaluateDailyLoss(cfg, sheetsClient, lockfilePath, alertChan)
	return err
}

// ManualTrigger activates a hard stop on demand, for /hardstop trigger.
func ManualTrigger(configPath, lockfilePath string, alertChan chan<- string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if cfg.Hardstop.DurationHours == nil {
		return fmt.Errorf("duration_hours is not configured, run /hardstop first")
	}
	trigger("manual", *cfg.Hardstop.DurationHours, lockfilePath, alertChan)
	return nil
}

// lockfileActiveAt reports whether the lockfile at lockfilePath exists and
// hasn't expired as of now. Goes through config.LoadLock rather than reading
// the file directly — config.go documents that no other package touches
// config.json/lockfile.json directly. Never deletes the file even when
// expired; that stays --check-lock's responsibility (see runCheckLock in
// main.go).
func lockfileActiveAt(lockfilePath string, now time.Time) bool {
	lock, err := config.LoadLock(lockfilePath)
	if err != nil || lock == nil {
		return false
	}
	return now.Before(lock.ExpiresAt)
}

// RunEnforcementLoop re-kills PokerStars every 30 seconds for as long as the
// lockfile at lockfilePath is active, so a player who relaunches PokerStars
// mid-lockout gets killed again rather than slipping through until the next
// hand-history file triggers a check. Runs for the life of the --serve
// process; stops when done is closed. Because it checks lockfile state on
// every tick from the moment it starts, restarting --serve (e.g. Task
// Scheduler after a reboot) automatically resumes enforcement if a lockfile
// written before the restart is still active.
func RunEnforcementLoop(lockfilePath string, done <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !lockfileActiveAt(lockfilePath, time.Now()) {
				continue
			}
			lock, err := config.LoadLock(lockfilePath)
			if err != nil || lock == nil {
				continue
			}
			slog.Info("hard stop lock active, killing PokerStars", "expires_at", lock.ExpiresAt)
			_ = exec.Command("taskkill.exe", "/IM", "PokerStars.exe", "/F").Run()
		}
	}
}

// trigger kills PokerStars, writes the lockfile, and sends the hard stop
// alert.
func trigger(reason string, durationHours int, lockfilePath string, alertChan chan<- string) {
	_ = exec.Command("taskkill.exe", "/IM", "PokerStars.exe", "/F").Run()

	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(durationHours) * time.Hour)
	lock := &config.LockFile{
		TriggeredAt:   now,
		ExpiresAt:     expiresAt,
		Reason:        reason,
		DurationHours: durationHours,
	}
	if err := config.SaveLock(lockfilePath, lock); err != nil {
		slog.Error("saving lockfile", "error", err)
	}

	alertChan <- fmt.Sprintf("🛑 Hard Stop triggered\nReason: %s\nExpires: %s", reason, expiresAt.Local().Format("2006-01-02 15:04"))
}
