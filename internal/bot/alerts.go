package bot

// Worker-side alert evaluators. All of these are called from the worker
// goroutine (main.runWorker) right after a session is parsed and written to
// the store, so they must go through alertChan rather than sending to
// Telegram directly (only the bot goroutine may do that) — which is also
// why they're plain functions, not Bot methods: the worker has no Bot.
//
// None of them fetch from the store or load config: the worker loads config
// once per file event (its documented contract) and fetches the shared data
// — the full Bankroll Ledger, the poker day's tournaments, and the bankroll
// (the ledger's last running total) — exactly once, passing all of it here
// and to the hardstop evaluators. Before this, every evaluator re-read
// config.json and re-fetched its own copy of the same ledger, costing six
// identical reads per hand on the sheets backend. The one exception is
// EvaluateStakeChangeAlert, which needs Cash Sessions rows nothing else on
// this path uses and runs once per session, not per hand.
//
// The threshold-based evaluators return fired so the worker can dedup: a
// cash hand history file gets a fresh event on every hand played, and the
// streak/daily conditions, once true, stay true for the rest of the session
// or poker day — without dedup they would re-send the same warning on every
// single hand. The worker remembers what fired per session/poker-day key
// and skips re-evaluation (see main.runWorker's firedAlerts map).

import (
	"fmt"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// EvaluateSessionLossAlert sends a warning (not a hard stop) if the cash
// session's net loss exceeds Alerts.SessionLossBuyins × BuyInUSD.
func EvaluateSessionLossAlert(session parsers.CashSession, cfg *config.Config, bankroll float64, alertChan chan<- string) {
	if cfg.Alerts.SessionLossBuyins == nil || session.BuyInUSD == 0 || session.NetUSD >= 0 {
		return
	}

	loss := -session.NetUSD
	if loss <= *cfg.Alerts.SessionLossBuyins*session.BuyInUSD {
		return
	}

	buyins := loss / session.BuyInUSD
	alertChan <- fmt.Sprintf("⚠️ Session loss alert\nLost %.1f buy-ins this session ($%.2f)\nBankroll: $%.2f", buyins, loss, bankroll)
}

// EvaluateDownswingAlert sends a warning (not a hard stop) if the rolling
// 30-day results-only equity curve (see hardstop.ComputeEquityCurve) has
// dropped from its peak by more than Alerts.DownswingBuyins × buyInUSD.
// Called after any session — cash or tournament. Fires independently of the
// downswing hard stop; since cross-validation enforces alert ≤ hardstop for
// this field, the alert always fires at or before the hard stop's own
// threshold, never after.
func EvaluateDownswingAlert(buyInUSD float64, cfg *config.Config, ledger []storage.LedgerEntry, bankroll float64, alertChan chan<- string) {
	if cfg.Alerts.DownswingBuyins == nil || buyInUSD == 0 {
		return
	}

	peak, current := hardstop.DownswingEquityCurve(ledger, time.Now())
	if !hardstop.DownswingExceeds(peak, current, buyInUSD, *cfg.Alerts.DownswingBuyins) {
		return
	}

	buyins := (peak - current) / buyInUSD
	alertChan <- fmt.Sprintf("⚠️ Downswing alert\nDown %.1f buy-ins from your 30-day peak\nPeak: $%.2f  Current: $%.2f\nBankroll: $%.2f", buyins, peak, current, bankroll)
}

// EvaluateLosingStreakAlert sends a warning (not a hard stop) when the
// account's most recent Alerts.LosingStreakCount session rows are all
// losses — the same streak definition as the consecutive-losses hard stop,
// via the shared hardstop.LosingStreakTriggered, just against the alert's
// own (lower or equal) threshold.
func EvaluateLosingStreakAlert(cfg *config.Config, ledger []storage.LedgerEntry, bankroll float64, alertChan chan<- string) (fired bool) {
	if cfg.Alerts.LosingStreakCount == nil {
		return false
	}

	if !hardstop.LosingStreakTriggered(ledger, *cfg.Alerts.LosingStreakCount) {
		return false
	}

	alertChan <- fmt.Sprintf("⚠️ Losing streak alert\n%d losing sessions in a row\nBankroll: $%.2f", *cfg.Alerts.LosingStreakCount, bankroll)
	return true
}

// EvaluateDailyLossAlert sends a warning (not a hard stop) when the current
// poker day's net P&L across all game types falls below
// -Alerts.DailyLossUSD — the same window, filtering, and comparison as the
// daily-loss hard stop (hardstop.PokerDaySessionEntries /
// hardstop.DailyLossExceeds), just against the alert's own threshold.
func EvaluateDailyLossAlert(cfg *config.Config, ledger []storage.LedgerEntry, bankroll float64, alertChan chan<- string) (fired bool) {
	if cfg.Alerts.DailyLossUSD == nil {
		return false
	}

	entries := hardstop.PokerDaySessionEntries(ledger, cfg, time.Now())
	if !hardstop.DailyLossExceeds(entries, *cfg.Alerts.DailyLossUSD) {
		return false
	}

	var net float64
	for _, e := range entries {
		net += e.AmountUSD
	}
	alertChan <- fmt.Sprintf("⚠️ Daily loss alert\nDown $%.2f across all games this poker day\nBankroll: $%.2f", -net, bankroll)
	return true
}

// EvaluateSpingoDailyLossAlert is EvaluateDailyLossAlert's Spin & Go-only
// counterpart, against Alerts.SpingoDailyLossUSD — same shared filter and
// comparison as the Spin & Go daily-loss hard stop, applied to the
// worker-fetched poker-day tournaments.
func EvaluateSpingoDailyLossAlert(cfg *config.Config, dayTournaments []parsers.Tournament, bankroll float64, alertChan chan<- string) (fired bool) {
	if cfg.Alerts.SpingoDailyLossUSD == nil {
		return false
	}

	spingos := hardstop.SpingoOnly(dayTournaments)
	if !hardstop.SpingoDailyLossExceeds(spingos, *cfg.Alerts.SpingoDailyLossUSD) {
		return false
	}

	var net float64
	for _, t := range spingos {
		net += t.NetUSD
	}
	alertChan <- fmt.Sprintf("⚠️ Spin & Go drain alert\nDown $%.2f on Spin & Gos this poker day\nBankroll: $%.2f", -net, bankroll)
	return true
}

// EvaluateStakeChangeAlert sends an informational note when the cash session
// just recorded is at a different stake than the cash session played before
// it. Not threshold-gated — there's nothing to configure; it exists so a
// mis-click onto the wrong table (or a shot-take the player wants to be
// conscious of) is called out the moment it happens. The worker calls this
// once per session (its stake can't change mid-session — one file, one
// table, one stake), so no fired return is needed — and that once-per-session
// cadence is also why this evaluator keeps its own store fetch instead of
// sharing the worker's per-event data, which doesn't include Cash Sessions.
func EvaluateStakeChangeAlert(session parsers.CashSession, sc storage.Store, alertChan chan<- string) error {
	sessions, err := sc.GetCashSessionsSince(time.Time{})
	if err != nil {
		return err
	}
	prev, ok := previousStake(sessions, session)
	if !ok || prev == session.Stake {
		return nil
	}
	alertChan <- fmt.Sprintf("🃏 Stake change detected\nPrevious cash session: %s\nThis session: %s", prev, session.Stake)
	return nil
}

// previousStake returns the stake of the most recent stored cash session
// that started before current did, and ok=false when there is none (current
// is the account's first cash session). current's own stored row is
// excluded by comparing against its start minute rather than its exact
// timestamp: the sheets backend stores dates at minute precision, so
// current's own row can read back a few seconds earlier than
// current.Date — cutting at the minute keeps it out without needing the
// storage-layer session ID, at the cost of also ignoring another session
// started within the same minute (in practice: two tables opened within 60
// seconds of each other).
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
