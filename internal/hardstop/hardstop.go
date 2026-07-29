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
	"github.com/vit-ui/self-tracker/internal/storage"
)

// Evaluate checks hard stop conditions after a cash session and triggers a
// hard stop if any condition is met. ledger is the full Bankroll Ledger and
// dayTournaments the current poker day's tournaments, both fetched once per
// file event by the worker (main.runWorker) and shared with the alert
// evaluators — no function on this path fetches from the store itself, so a
// hand that runs every check costs one ledger read, not six.
func Evaluate(session parsers.CashSession, cfg *config.Config, ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament, lockfilePath string, alertChan chan<- string) error {
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

	if h.ConsecutiveLosses != nil && LosingStreakTriggered(ledger, *h.ConsecutiveLosses) {
		if h.DurationHours == nil {
			return fmt.Errorf("consecutive_losses configured without duration_hours")
		}
		trigger("consecutive_losses", *h.DurationHours, lockfilePath, alertChan)
		return nil
	}

	if triggered, err := evaluateDownswing(cfg, ledger, session.BuyInUSD, lockfilePath, alertChan); err != nil {
		return err
	} else if triggered {
		return nil
	}

	_, err := evaluateDailyLoss(cfg, ledger, dayTournaments, lockfilePath, alertChan)
	return err
}

// LosingStreakTriggered reports whether entries — the full Bankroll Ledger,
// any type, in chronological order — show n consecutive losing session rows
// (cash_session or tournament) among the account's most recent session
// history. The shared decision behind both the consecutive-losses hard stop
// and the losing-streak alert (bot.EvaluateLosingStreakAlert), so the two
// sides can never disagree on what counts as a streak. Deposits and
// withdrawals are filtered out before the n-row window is taken (see
// recentSessionRows), so a transfer never occupies one of the n slots and
// never breaks up an otherwise-contiguous losing streak. False when the
// account doesn't yet have n session rows in its history at all — an early
// streak of fewer than n rows, even if every one of them is a loss, must not
// fire.
func LosingStreakTriggered(entries []storage.LedgerEntry, n int) bool {
	sessionRows := recentSessionRows(entries, n)
	return len(sessionRows) == n && consecutiveLossStreak(sessionRows) == len(sessionRows)
}

// sessionLossExceeds reports whether a session loss of sessionLoss USD
// exceeds thresholdBuyins × buyInUSD.
func sessionLossExceeds(sessionLoss, buyInUSD, thresholdBuyins float64) bool {
	return sessionLoss > thresholdBuyins*buyInUSD
}

// consecutiveLossStreak returns how many of the most recent contiguous
// entries at the end of entries (in chronological order) have a negative
// AmountUSD. Counting stops at the first non-negative entry.
func consecutiveLossStreak(entries []storage.LedgerEntry) int {
	count := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].AmountUSD >= 0 {
			break
		}
		count++
	}
	return count
}

// recentSessionRows filters entries to session type (cash_session or
// tournament — see sessionOnly) and returns the last n of them in
// chronological order. Deposits and withdrawals are dropped before the
// n-row window is taken, so a transfer anywhere in entries never occupies
// one of the n slots and never breaks up an otherwise-contiguous run of
// session rows. Returns fewer than n rows when entries has fewer than n
// session rows in total.
func recentSessionRows(entries []storage.LedgerEntry, n int) []storage.LedgerEntry {
	rows := sessionOnly(entries)
	if len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	return rows
}

// PokerDayStart computes the start of the poker day containing now, using
// cfg.Schedule.DayStartHour (default 6) as the daily reset hour. Computed in
// the stored-date coordinate system (storage.AsStoredClock — local
// wall-clock digits read as UTC) so it compares correctly against dates read
// back from either backend, which carry no real zone offset.
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

// rollingWindowStart computes the start of a rolling window of the given
// duration ending at now, in the same stored-date coordinate system as
// PokerDayStart (storage.AsStoredClock) so it stays comparable against dates
// read back from either backend.
func rollingWindowStart(now time.Time, window time.Duration) time.Time {
	return storage.AsStoredClock(now).Add(-window)
}

// PokerDaySessionEntries filters entries — the full Bankroll Ledger in
// chronological order — down to the current poker day's session-result rows
// (cash_session/tournament only — transfers excluded, see sessionOnly). The
// day boundary is inclusive (Date >= PokerDayStart), matching the
// GetLedgerEntriesSince windowed fetch this filter replaced. The shared
// decision behind both the unified daily-loss hard stop and the daily-loss
// alert (bot.EvaluateDailyLossAlert).
func PokerDaySessionEntries(entries []storage.LedgerEntry, cfg *config.Config, now time.Time) []storage.LedgerEntry {
	start := PokerDayStart(cfg, now)
	var day []storage.LedgerEntry
	for _, e := range entries {
		if !e.Date.Before(start) {
			day = append(day, e)
		}
	}
	return sessionOnly(day)
}

// inRollingWindow reports whether entryDate falls after cutoff. The
// boundary is exclusive: an entry dated exactly at cutoff (e.g. exactly 30
// days ago) is outside the window, not in it. cutoff is normally computed
// via rollingWindowStart.
func inRollingWindow(entryDate, cutoff time.Time) bool {
	return entryDate.After(cutoff)
}

// SpingoOnly filters tournaments to Spin & Go rows only, matching by
// BaseType so a knockout Spin & Go ("spingo-ko") is still included — a
// knockout tournament must count toward the Spin & Go daily-loss hard stop
// the same as a non-knockout one. Exported because the Spin & Go daily-loss
// alert (bot.EvaluateSpingoDailyLossAlert) applies the same filter to the
// same worker-fetched day window.
func SpingoOnly(tournaments []parsers.Tournament) []parsers.Tournament {
	var result []parsers.Tournament
	for _, t := range tournaments {
		if t.BaseType() == "spingo" {
			result = append(result, t)
		}
	}
	return result
}

// sessionOnly filters entries to cash_session and tournament rows only,
// excluding deposit/withdrawal transfers. RunningTotal includes transfers,
// so mixing them into an equity curve would read a large withdrawal as an
// equally large poker downswing.
func sessionOnly(entries []storage.LedgerEntry) []storage.LedgerEntry {
	var result []storage.LedgerEntry
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
func ComputeEquityCurve(rows []storage.LedgerEntry) (peak, current float64) {
	var sum float64
	for _, r := range rows {
		sum += r.AmountUSD
		if sum > peak {
			peak = sum
		}
	}
	return peak, sum
}

// DownswingEquityCurve windows entries — the full Bankroll Ledger in
// chronological order — to the rolling 30 days ending at now (see
// rollingWindowStart/inRollingWindow; the boundary is exclusive), filters
// them to poker results only (see sessionOnly), and returns the resulting
// equity curve. Shared by the hard stop check and the downswing alert.
func DownswingEquityCurve(entries []storage.LedgerEntry, now time.Time) (peak, current float64) {
	cutoff := rollingWindowStart(now, 30*24*time.Hour)
	var windowed []storage.LedgerEntry
	for _, e := range entries {
		if inRollingWindow(e.Date, cutoff) {
			windowed = append(windowed, e)
		}
	}
	return ComputeEquityCurve(sessionOnly(windowed))
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
func evaluateDownswing(cfg *config.Config, ledger []storage.LedgerEntry, buyInUSD float64, lockfilePath string, alertChan chan<- string) (triggered bool, err error) {
	h := cfg.Hardstop
	if h.DownswingBuyins == nil || buyInUSD == 0 {
		return false, nil
	}

	peak, current := DownswingEquityCurve(ledger, time.Now())
	if DownswingExceeds(peak, current, buyInUSD, *h.DownswingBuyins) {
		if h.DurationHours == nil {
			return false, fmt.Errorf("downswing_buyins configured without duration_hours")
		}
		trigger("downswing_buyins", *h.DurationHours, lockfilePath, alertChan)
		return true, nil
	}
	return false, nil
}

// DailyLossExceeds sums AmountUSD across entries (expected to already be
// filtered to the current poker day and Type == "cash_session" or
// "tournament", i.e. PokerDaySessionEntries' output) and reports whether
// that net P&L falls below -thresholdUSD. This is net P&L, not gross loss —
// a win elsewhere in the same day offsets losses. Exported so the daily-loss
// alert applies the exact same comparison as the hard stop, just against its
// own threshold.
func DailyLossExceeds(entries []storage.LedgerEntry, thresholdUSD float64) bool {
	var net float64
	for _, e := range entries {
		net += e.AmountUSD
	}
	return net < -thresholdUSD
}

// SpingoDailyLossExceeds sums NetUSD across tournaments (expected to already
// be filtered to the current poker day and Spin & Go only — SpingoOnly
// applied to the worker's poker-day fetch) and reports whether that net P&L falls
// below -thresholdUSD. Same net-P&L semantics as DailyLossExceeds, scoped to
// Spin & Go; exported for the same alert-side reuse.
func SpingoDailyLossExceeds(tournaments []parsers.Tournament, thresholdUSD float64) bool {
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
func evaluateDailyLoss(cfg *config.Config, ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament, lockfilePath string, alertChan chan<- string) (triggered bool, err error) {
	h := cfg.Hardstop
	if h.SpingoDailyLossUSD == nil && h.DailyLossUSD == nil {
		return false, nil
	}

	if h.SpingoDailyLossUSD != nil {
		if SpingoDailyLossExceeds(SpingoOnly(dayTournaments), *h.SpingoDailyLossUSD) {
			if h.DurationHours == nil {
				return false, fmt.Errorf("spingo_daily_loss_usd configured without duration_hours")
			}
			trigger("spingo_daily_loss_usd", *h.DurationHours, lockfilePath, alertChan)
			return true, nil
		}
	}

	if h.DailyLossUSD != nil {
		if DailyLossExceeds(PokerDaySessionEntries(ledger, cfg, time.Now()), *h.DailyLossUSD) {
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
// whole account rather than filtering by t's type. ledger and dayTournaments
// carry the same worker-fetched, once-per-event data Evaluate documents.
func EvaluateTournament(t parsers.Tournament, cfg *config.Config, ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament, lockfilePath string, alertChan chan<- string) error {
	if triggered, err := evaluateDownswing(cfg, ledger, t.BuyInUSD, lockfilePath, alertChan); err != nil {
		return err
	} else if triggered {
		return nil
	}

	_, err := evaluateDailyLoss(cfg, ledger, dayTournaments, lockfilePath, alertChan)
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

	slog.Info("hard stop triggered", "reason", reason, "expires_at", expiresAt.Format(time.RFC3339))

	alertChan <- fmt.Sprintf("🛑 Hard Stop triggered\nReason: %s\nExpires: %s", reason, expiresAt.Local().Format("2006-01-02 15:04"))
}
