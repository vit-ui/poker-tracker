// Package hardstop implements the hard stop decision: detecting loss
// conditions and deciding when to trigger. Deciding is all this package does —
// the enforcement half (killing PokerStars, writing the lockfile) lives with
// the collector in internal/client, because only the machine PokerStars runs
// on can carry it out. A decision here is signalled through an onTrigger
// callback the caller supplies; the caller turns it into the alert plus the
// lock command the client enforces.
package hardstop

import (
	"fmt"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// EvaluateSessionLoss checks the one hard-stop condition that reads only the
// just-parsed session, not the stored ledger — the session's own netCents loss
// against session_loss_buyins × buy-in — and calls onTrigger if it's breached.
// It runs on every hand (in-memory, no store I/O) so a bleeding session is
// stopped mid-session, unlike the ledger-based checks in Evaluate, which run
// once the session closes (see the server's cash branch). Errors if the
// threshold is set without duration_hours, matching Evaluate.
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

// Evaluate checks the ledger-based hard stop conditions after a cash session
// has closed and calls onTrigger (reason, durationHours) for the first
// condition met, if any: consecutive losses, then downswing, then daily loss.
// The session's own netCents-loss check is EvaluateSessionLoss (run per hand, from
// the in-memory session); this reads the stored ledger, so it runs once at
// session end, when the just-closed session is in the ledger. ledger is the
// full Bankroll Ledger and dayTournaments the currentCents poker day's tournaments,
// both fetched once per file event by the worker (internal/server) and shared
// with the alert evaluators — no function on this path fetches from the store
// itself.
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

// consecutiveLossStreak returns how many of the most recent contiguous
// entries at the end of entries (in chronological order) have a negative
// AmountCents. Counting stops at the first non-negative entry.
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
// chronological order — down to the currentCents poker day's session-result rows
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
// excluding deposit/withdrawal transfers. RunningTotalCents includes transfers,
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
// summing AmountCents cumulatively. peakCents is the highest point reached on that
// curve, including the implicit starting point of 0 before any row; currentCents
// is the final point. Exported so the hard stop check (hardstop.go) and the
// downswing alert (bot.go) compute the exact same curve rather than each
// deriving their own.
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

// DownswingEquityCurve windows entries — the full Bankroll Ledger in
// chronological order — to the rolling 30 days ending at now (see
// rollingWindowStart/inRollingWindow; the boundary is exclusive), filters
// them to poker results only (see sessionOnly), and returns the resulting
// equity curve. Shared by the hard stop check and the downswing alert.
func DownswingEquityCurve(entries []storage.LedgerEntry, now time.Time) (peakCents, currentCents money.Cents) {
	cutoff := rollingWindowStart(now, 30*24*time.Hour)
	var windowed []storage.LedgerEntry
	for _, e := range entries {
		if inRollingWindow(e.Date, cutoff) {
			windowed = append(windowed, e)
		}
	}
	return ComputeEquityCurve(sessionOnly(windowed))
}

// DownswingExceeds reports whether the equity-curve peakCents minus currentCents
// exceeds thresholdBuyins × buyInCents. Exported alongside ComputeEquityCurve
// so the alert side (bot.go) applies the exact same formula as the hard
// stop side.
func DownswingExceeds(peakCents, currentCents, buyInCents money.Cents, thresholdBuyins float64) bool {
	return money.DifferenceExceeds(peakCents, currentCents, buyInCents, thresholdBuyins)
}

// evaluateDownswing checks whether the rolling 30-day results-only equity
// curve has dropped from its peakCents by more than DownswingBuyins ×
// buyInCents, and calls onTrigger if so. buyInCents of 0 (nil buy-in, or a
// satellite entry corrected to a ticket — see RecoverTicketPayout in
// bot.go) skips the check entirely: a 0 buy-in would make the threshold 0,
// so any downswing at all would trigger it.
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

// DailyLossExceeds sums AmountCents across entries (expected to already be
// filtered to the currentCents poker day and Type == "cash_session" or
// "tournament", i.e. PokerDaySessionEntries' output) and reports whether
// that netCents P&L falls below -thresholdCents. This is netCents P&L, not gross loss —
// a win elsewhere in the same day offsets losses. Exported so the daily-loss
// alert applies the exact same comparison as the hard stop, just against its
// own threshold.
func DailyLossExceeds(entries []storage.LedgerEntry, thresholdCents money.Cents) bool {
	var netCents money.Cents
	for _, e := range entries {
		netCents += e.AmountCents
	}
	return netCents < -thresholdCents
}

// SpingoDailyLossExceeds sums NetCents across tournaments (expected to already
// be filtered to the currentCents poker day and Spin & Go only — SpingoOnly
// applied to the worker's poker-day fetch) and reports whether that netCents P&L falls
// below -thresholdCents. Same netCents-P&L semantics as DailyLossExceeds, scoped to
// Spin & Go; exported for the same alert-side reuse.
func SpingoDailyLossExceeds(tournaments []parsers.Tournament, thresholdCents money.Cents) bool {
	var netCents money.Cents
	for _, t := range tournaments {
		netCents += t.NetCents
	}
	return netCents < -thresholdCents
}

// evaluateDailyLoss runs two independent daily-loss checks for the currentCents
// poker day — a Spin & Go-specific one and a unified one across all game
// types — and calls onTrigger if either has fallen below its configured
// threshold. Either check firing is sufficient; they are not mutually
// exclusive, but only the first one to fire actually triggers — triggered
// reports whether one did.
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

// EvaluateTournament checks hard stop conditions after a tournament (cash or
// Spin & Go). The downswing and daily loss checks re-evaluate against the
// whole account rather than filtering by t's type. ledger and dayTournaments
// carry the same worker-fetched, once-per-event data Evaluate documents.
func EvaluateTournament(t parsers.Tournament, cfg *config.Config, ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament, now time.Time, onTrigger func(reason string, durationHours int)) error {
	if triggered, err := evaluateDownswing(cfg, ledger, t.BuyInCents, now, onTrigger); err != nil {
		return err
	} else if triggered {
		return nil
	}

	_, err := evaluateDailyLoss(cfg, ledger, dayTournaments, now, onTrigger)
	return err
}

// ManualTrigger decides a hard stop on demand, for /hardstop trigger: it loads
// the configured duration and, if set, calls onTrigger("manual", duration).
// Errors if duration_hours is unset.
func ManualTrigger(configPath string, onTrigger func(reason string, durationHours int)) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if cfg.Hardstop.DurationHours == nil {
		return fmt.Errorf("duration_hours is not configured, run /hardstop first")
	}
	onTrigger("manual", *cfg.Hardstop.DurationHours)
	return nil
}

// BuildTrigger constructs the lockfile and the user-facing alert text for a
// hard stop of the given reason and duration, as of now. The caller (the
// worker's or the bot's onTrigger) sends the alert to Telegram and hands the
// lockfile to the client, which kills PokerStars and writes it. Splitting the
// build out from enforcement is what lets the decision stay on the server
// while the taskkill and lockfile write happen on the collector.
func BuildTrigger(reason string, durationHours int, now time.Time) (config.LockFile, string) {
	expiresAt := now.Add(time.Duration(durationHours) * time.Hour)
	lock := config.LockFile{
		TriggeredAt:   now,
		ExpiresAt:     expiresAt,
		Reason:        reason,
		DurationHours: durationHours,
	}
	alert := LockMessage(lock, time.Local)
	return lock, alert
}

// LockMessage formats the collector-confirmed lock instead of a speculative
// retry expiry. Split mode supplies the collector's observed timezone.
func LockMessage(lock config.LockFile, location *time.Location) string {
	return fmt.Sprintf("🛑 Hard Stop triggered\nReason: %s\nExpires: %s", reasonPhrase(lock.Reason), lock.ExpiresAt.In(location).Format("2006-01-02 15:04"))
}

// reasonPhrase turns a lockfile reason key into the plain-language line the
// Telegram alert shows. The raw key stays in the lockfile and the Info log;
// only the user-facing message is humanized. An unrecognized key falls back to
// itself, so a future reason never renders blank.
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
