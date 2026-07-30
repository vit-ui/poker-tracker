// Package server is the brain half of the tracker: it receives file uploads
// from the collector (internal/client), reconstructs each file, parses it,
// writes storage, evaluates the hard stop and the outgoing alerts, and runs the
// Telegram bot. Everything except watching the PokerStars folders and enforcing
// locks lives here, so this half can run on a machine that never sees
// PokerStars.
package server

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

// cashSessionSummaryQuietPeriod is how long a cash table must go without a
// new hand before its post-session summary is sent. A hand history file
// gets a fresh event on every hand played at that table, so sending
// immediately would fire the summary once per hand with a growing,
// still-incomplete snapshot each time — this waits for the table to go
// quiet and sends the final numbers once, the same time-gap technique
// poker trackers use to detect a session boundary. 5 minutes trades a
// faster summary after quitting against a higher chance of a mid-session
// break tripping it early (a resumed table then sends an updated summary on
// the next quiet period).
const cashSessionSummaryQuietPeriod = 5 * time.Minute

// parseEntry is one file's carried incremental-parse state plus when it was
// last touched, so idle entries can be evicted (see parseStateCap/TTL).
type parseEntry struct {
	state    *parsers.ParseState
	lastSeen time.Time
}

// parseStateCap and parseStateTTL bound the per-file parse-state map the worker
// keeps for the incremental parser. A long-lived server processing a new
// hand-history file per table per day would otherwise grow the map without
// limit; when it exceeds the cap, entries not touched within the TTL (finished
// sessions) are dropped. An actively-growing file keeps a recent lastSeen and
// is never evicted; a wrongly-evicted file just full-reparses once on its next
// event (always correct), so the bound can be aggressive.
const (
	parseStateCap = 128
	parseStateTTL = 12 * time.Hour
)

// Server holds the brain-side dependencies. It consumes uploads from the link,
// reconstructs each file under workDir (keyed by the upload's FileID), and
// emits alerts on alertChan and hard-stop lock commands back down the link.
type Server struct {
	store      storage.Store
	configPath string
	link       *transport.Link
	alertChan  chan<- string
	workDir    string
}

// NewServer creates a Server with a fresh working directory for reconstructed
// files. The directory is created under the OS temp dir and left for the OS to
// reclaim; it holds only transient copies of files the collector already has.
func NewServer(store storage.Store, configPath string, link *transport.Link, alertChan chan<- string) (*Server, error) {
	workDir, err := os.MkdirTemp("", "tracker-work-")
	if err != nil {
		return nil, fmt.Errorf("creating server work dir: %w", err)
	}
	return &Server{store: store, configPath: configPath, link: link, alertChan: alertChan, workDir: workDir}, nil
}

// reconstruct writes an upload's bytes into the working file for its FileID and
// returns that file's path. Offset 0 replaces the file wholesale (a fresh cash
// file or a rewritten tournament summary); a non-zero offset appends the new
// bytes at that position, rebuilding the growing hand-history file the
// collector is streaming.
func (s *Server) reconstruct(up transport.Upload) (string, error) {
	name := fmt.Sprintf("%x", sha256.Sum256([]byte(up.FileID)))
	path := filepath.Join(s.workDir, name)
	if up.Offset == 0 {
		if err := os.WriteFile(path, up.Bytes, 0644); err != nil {
			return "", fmt.Errorf("writing working file: %w", err)
		}
		return path, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return "", fmt.Errorf("opening working file: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteAt(up.Bytes, up.Offset); err != nil {
		return "", fmt.Errorf("appending to working file: %w", err)
	}
	if err := f.Truncate(up.Offset + int64(len(up.Bytes))); err != nil {
		return "", fmt.Errorf("truncating working file: %w", err)
	}
	return path, nil
}

// Run is the worker loop: it receives uploads, reconstructs and parses each
// file, writes storage, and evaluates the hard stop and alerts. It blocks until
// the link's Uploads channel is closed.
func (s *Server) Run() {
	var summaryMu sync.Mutex
	summaryTimers := make(map[string]*time.Timer)

	// firedAlerts dedups the streak/daily/stake alert evaluations: a cash hand
	// history file gets a fresh event on every hand played, and those
	// conditions, once true, stay true for the rest of the session or poker
	// day — without this, the same warning would re-send (and re-query the
	// store) on every single hand. Keys are per session (FirstHandID /
	// TournamentID) or per poker day (the day's start time); the map lives for
	// the process and is rebuilt empty on restart, worst case repeating one
	// already-sent warning.
	firedAlerts := make(map[string]bool)

	// Sent at most once per run: a missing screen name skips every file, so
	// without this a folder of unparsed files would fire one prompt each.
	screenNameWarned := false

	// parseStates carries per-file incremental parse state so a growing session
	// file is resumed from its last byte offset instead of re-parsed from zero
	// on every hand event (parsers.ParseCashFileFrom). Local to this goroutine —
	// no global state — and bounded (parseStateCap/TTL). Keyed by upload FileID.
	parseStates := make(map[string]*parseEntry)

	// enforce turns a hard-stop decision into its two effects: the alert goes to
	// Telegram, and a lock command goes down the link to the collector, which
	// kills PokerStars and writes the lockfile. hardstop only decides.
	enforce := func(reason string, durationHours int) {
		lock, alert := hardstop.BuildTrigger(reason, durationHours, time.Now().UTC())
		s.link.Commands <- transport.Command{Kind: transport.CommandLock, Lock: &lock}
		s.alertChan <- alert
	}

	for up := range s.link.Uploads {
		// Reconstruct FIRST, before any guard that might skip this event. The
		// collector advances its per-file sent offset unconditionally, so the
		// working file must absorb every upload's bytes even when we skip
		// parsing below (e.g. the screen name isn't set yet, or config failed to
		// load). Otherwise a later delta would land at an offset past a
		// never-written working file and corrupt it — and when the skip
		// condition clears, a full parse of the now-complete working file
		// records everything, matching the pre-split whole-file re-read.
		path, err := s.reconstruct(up)
		if err != nil {
			slog.Error("reconstructing uploaded file", "err", err, "file", up.FileID)
			continue
		}

		// Re-read on every event rather than capturing once at startup — config
		// can change at runtime (e.g. the screen name filled in via the bot).
		cfg, err := config.LoadConfig(s.configPath)
		if err != nil {
			slog.Error("loading config", "err", err)
			continue
		}
		if up.ScreenName == "" {
			slog.Error("pokerstars screen name not configured, skipping file", "file", up.FileID)
			if !screenNameWarned {
				s.alertChan <- "🔧 I found a poker file but don't know your PokerStars screen name yet — I can't track anything until it's set. Open /menu → Setup → Screen name."
				screenNameWarned = true
			}
			continue
		}
		screenName := up.ScreenName

		// Threshold evaluation must see a turned-off alert/hard-stop as unset.
		// Blank the off ones out here (EffectiveAlerts/EffectiveHardstop) so the
		// evaluators and the per-event fetch never learn about the *_enabled
		// flags; cfg stays raw for the notification gates further down.
		evalCfg := *cfg
		evalCfg.Alerts = config.EffectiveAlerts(cfg.Alerts)
		evalCfg.Hardstop = config.EffectiveHardstop(cfg.Hardstop)

		switch up.Type {
		case "cash":
			now := time.Now()
			if len(parseStates) > parseStateCap {
				for p, e := range parseStates {
					if now.Sub(e.lastSeen) > parseStateTTL {
						delete(parseStates, p)
					}
				}
			}
			var prior *parsers.ParseState
			if e := parseStates[up.FileID]; e != nil {
				prior = e.state
			}
			session, tourneyStats, newState, err := parsers.ParseCashFileFrom(path, screenName, prior)
			if err != nil {
				slog.Error("parsing cash file", "file", up.FileID, "err", err)
				continue
			}
			// newState is always non-nil on the no-error path.
			parseStates[up.FileID] = &parseEntry{state: newState, lastSeen: now}
			if session != nil {
				before := sheetsPending(s.store)
				if err := s.store.AppendCashSession(*session); err != nil {
					slog.Error("appending cash session to sheets", "err", err,
						"date", session.Date.Format("2006-01-02 15:04"), "stake", session.Stake,
						"session_id", sheets.CashSessionID(session.Date, session.FirstHandID))
					if msg := blipMessage(before, sheetsPending(s.store)); msg != "" {
						s.alertChan <- msg
					}
				} else if msg := syncedMessage(before, sheetsPending(s.store)); msg != "" {
					s.alertChan <- msg
				}
				// Fetched after the write so the shared data includes the
				// session just recorded — the same state each evaluator saw
				// back when it did its own fetch.
				ledger, dayTournaments := fetchEvalData(s.store, &evalCfg, false)
				bankroll := ledgerBankroll(ledger)
				if err := hardstop.Evaluate(*session, &evalCfg, ledger, dayTournaments, enforce); err != nil {
					slog.Error("evaluating hard stop", "err", err)
				}
				bot.EvaluateSessionLossAlert(*session, &evalCfg, bankroll, s.alertChan)
				bot.EvaluateDownswingAlert(session.BuyInUSD, &evalCfg, ledger, bankroll, s.alertChan)
				sessionKey := "cash:" + session.FirstHandID
				if config.NotificationEnabled(cfg.Notifications.StakeChange) && !firedAlerts["stake:"+sessionKey] {
					if err := bot.EvaluateStakeChangeAlert(*session, s.store, s.alertChan); err != nil {
						slog.Error("evaluating stake change alert", "err", err,
							"date", session.Date.Format("2006-01-02 15:04"), "stake", session.Stake,
							"session_id", sheets.CashSessionID(session.Date, session.FirstHandID))
					} else {
						// Marked fired whether or not it sent — the previous
						// stake can't change mid-session, so one evaluation
						// per session settles it either way.
						firedAlerts["stake:"+sessionKey] = true
					}
				}
				evaluateOncePerKeyAlerts(&evalCfg, ledger, dayTournaments, bankroll, s.alertChan, firedAlerts, sessionKey)
				scheduleCashSessionSummary(&summaryMu, summaryTimers, up.FileID, cashSessionSummaryQuietPeriod, s.alertChan, func() string {
					// Re-read config at fire time: the mode may have changed
					// during the quiet period, and the session net is only
					// final now. A config read error falls through and sends
					// (fail open); a muted/filtered-out result returns "".
					if fireCfg, err := config.LoadConfig(s.configPath); err != nil {
						slog.Error("loading config for cash summary gate", "err", err)
					} else if !config.ShouldSendSummary(config.SummaryMode(fireCfg.Notifications.CashSessionSummary), session.NetUSD) {
						return ""
					}
					bankroll, _ := s.store.GetCurrentBankroll()
					return cashSessionSummaryText(*session, bankroll)
				})
			} else if tourneyStats != nil {
				before := sheetsPending(s.store)
				if err := s.store.UpdateTournamentHandStats(tourneyStats.TournamentID, tourneyStats.VPIP, tourneyStats.PFR, tourneyStats.Hands, tourneyStats.BountyUSD, tourneyStats.IsKnockout); err != nil {
					slog.Error("updating tournament hand stats", "err", err, "tournament_id", tourneyStats.TournamentID)
					if msg := blipMessage(before, sheetsPending(s.store)); msg != "" {
						s.alertChan <- msg
					}
				} else if msg := syncedMessage(before, sheetsPending(s.store)); msg != "" {
					s.alertChan <- msg
				}
			} else {
				slog.Error("ParseCashFile returned no session and no tournament stats", "file", up.FileID)
			}

		case "tournament":
			t, err := parsers.ParseTournamentFile(path, screenName)
			if err != nil {
				slog.Error("parsing tournament", "file", up.FileID, "err", err)
				continue
			}

			// Checked once, here, before either the satellite queue or the
			// direct-append path below runs — a duplicate file event for a
			// tournament already handled must never send a second Telegram
			// alert, ask the satellite/ticket question twice, or queue a second
			// pending entry. A re-entry rewrite is deliberately *not* treated as
			// a duplicate: PokerStars overwrites the summary each time
			// screenName busts an entry, and each rewrite carries a larger
			// buy-in / different finish that must update the row, ledger, and
			// bankroll. See tournamentAlreadyHandled's doc comment.
			duplicate, err := tournamentAlreadyHandled(t, s.configPath, s.store)
			if err != nil {
				slog.Error("checking for duplicate tournament processing", "err", err)
			} else if duplicate {
				continue
			}

			// Two kinds of tournament can't be recorded straight away because
			// the summary file doesn't carry enough to know the real money
			// movement: a LikelySatellite (ticket vs cash entry unknown) and a
			// zero-buy-in freeroll/Kick-Off (entry cost unknown). Both queue
			// here and the bot asks the player; only the answer records the row.
			// Satellite is checked first so a satellite that also has no buy-in
			// keeps the ticket-vs-cash question.
			var reason string
			switch {
			case t.LikelySatellite:
				reason = config.PendingSatellite
			case t.BuyInUSD == 0:
				reason = config.PendingFreerollBuyin
			}
			if reason != "" {
				pendingPath := filepath.Join(filepath.Dir(s.configPath), "pending_tournaments.json")
				pending, err := config.LoadPendingTournaments(pendingPath)
				if err != nil {
					slog.Error("loading pending tournaments", "err", err)
					continue
				}
				pending = append(pending, config.PendingTournament{
					Tournament:    t,
					Reason:        reason,
					FirstQueuedAt: time.Now(),
					// Zero value, not time.Now(): the ticker only sends once
					// time.Since(LastRemindedAt) exceeds 30 minutes, and a zero
					// time is always more than 30 minutes in the past — this
					// makes the first prompt fire on the next tick instead of
					// being delayed by a full reminder interval.
					LastRemindedAt: time.Time{},
				})
				if err := config.SavePendingTournaments(pendingPath, pending); err != nil {
					slog.Error("saving pending tournaments", "err", err)
				}
				continue
			}

			before := sheetsPending(s.store)
			netUSD, err := s.store.AppendTournament(t)
			if err != nil {
				slog.Error("appending tournament to sheets", "err", err,
					"date", t.Date.Format("2006-01-02 15:04"), "tournament_id", t.TournamentID)
				if msg := blipMessage(before, sheetsPending(s.store)); msg != "" {
					s.alertChan <- msg
				}
			} else if msg := syncedMessage(before, sheetsPending(s.store)); msg != "" {
				s.alertChan <- msg
			}
			// Fetched after the write, like the cash branch — and always
			// including the ledger here, since the post-tournament alert below
			// needs the bankroll (its last running total) regardless of what's
			// configured.
			ledger, dayTournaments := fetchEvalData(s.store, &evalCfg, true)
			bankroll := ledgerBankroll(ledger)
			if err := hardstop.EvaluateTournament(t, &evalCfg, ledger, dayTournaments, enforce); err != nil {
				slog.Error("evaluating hard stop", "err", err)
			}
			bot.EvaluateDownswingAlert(t.BuyInUSD, &evalCfg, ledger, bankroll, s.alertChan)
			evaluateOncePerKeyAlerts(&evalCfg, ledger, dayTournaments, bankroll, s.alertChan, firedAlerts, "tournament:"+t.TournamentID)
			if config.ShouldSendSummary(config.SummaryMode(cfg.Notifications.TournamentSummary), netUSD) {
				s.alertChan <- tournamentAlertText(t, netUSD, bankroll)
			}
		}
	}
}

// sheetsPending returns the sheets backend's current write-ahead fallback
// queue length, or 0 on the sqlite backend, which has no such queue.
func sheetsPending(sc storage.Store) int {
	if c, ok := sc.(*sheets.Client); ok {
		return c.PendingWrites()
	}
	return 0
}

// syncedMessage reports how many previously-failed Sheets writes just resolved,
// as a Telegram-ready message, or "" if none did. before/after are sheetsPending
// snapshots taken immediately around one of the three sheets write calls — each
// of those calls flushes the fallback queue internally on success, so a drop in
// the count means queued entries from an earlier outage just synced.
func syncedMessage(before, after int) string {
	if n := before - after; n > 0 {
		return fmt.Sprintf("✅ Synced %d queued write(s) to Sheets", n)
	}
	return ""
}

// blipMessage is syncedMessage's counterpart for the failure side: a Sheets
// write that fails is cached to the fallback queue, so sheetsPending rises.
// Fired only on the 0→nonzero transition (the start of an outage) so a burst of
// failed hands during one outage sends a single notice, not one per hand.
// Returns "" on sqlite (sheetsPending always 0) and mid-outage (before > 0).
func blipMessage(before, after int) string {
	if before == 0 && after > 0 {
		return "💔 Sheets briefly unreachable — your results are saved locally and I'll sync them automatically when it's back."
	}
	return ""
}

// evaluateOncePerKeyAlerts runs the losing-streak, daily-loss, and Spin & Go
// daily-loss alert evaluators, each deduped via firedAlerts (per session for the
// streak, per poker day for the two daily checks) since a cash file fires an
// event per hand.
func evaluateOncePerKeyAlerts(cfg *config.Config, ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament, bankroll float64, alertChan chan<- string, firedAlerts map[string]bool, sessionKey string) {
	if !firedAlerts["streak:"+sessionKey] {
		if bot.EvaluateLosingStreakAlert(cfg, ledger, bankroll, alertChan) {
			firedAlerts["streak:"+sessionKey] = true
		}
	}

	dayKey := hardstop.PokerDayStart(cfg, time.Now()).Format(time.RFC3339)
	if !firedAlerts["daily:"+dayKey] {
		if bot.EvaluateDailyLossAlert(cfg, ledger, bankroll, alertChan) {
			firedAlerts["daily:"+dayKey] = true
		}
	}
	if !firedAlerts["spingo_daily:"+dayKey] {
		if bot.EvaluateSpingoDailyLossAlert(cfg, dayTournaments, bankroll, alertChan) {
			firedAlerts["spingo_daily:"+dayKey] = true
		}
	}
}

// fetchEvalData reads the shared data every post-write evaluation runs on — the
// full Bankroll Ledger and the current poker day's tournaments — at most once
// per file event, replacing the six-per-hand identical fetches the evaluators
// used to do themselves. Each fetch is skipped entirely when nothing configured
// needs it; forTournament forces the ledger fetch because the post-tournament
// alert needs the bankroll regardless of config. A failed fetch is logged and
// returns nil for that data set — every consumer treats nil as "no rows".
func fetchEvalData(sc storage.Store, cfg *config.Config, forTournament bool) (ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament) {
	a, h := cfg.Alerts, cfg.Hardstop
	needLedger := forTournament ||
		a.SessionLossBuyins != nil || a.DownswingBuyins != nil || a.LosingStreakCount != nil ||
		a.DailyLossUSD != nil || a.SpingoDailyLossUSD != nil ||
		h.ConsecutiveLosses != nil || h.DownswingBuyins != nil || h.DailyLossUSD != nil
	if needLedger {
		var err error
		ledger, err = sc.GetLedgerEntriesSince(time.Time{})
		if err != nil {
			slog.Error("reading bankroll ledger for alert evaluation", "err", err)
		}
	}
	if a.SpingoDailyLossUSD != nil || h.SpingoDailyLossUSD != nil {
		var err error
		dayTournaments, err = sc.GetTournamentsSince(hardstop.PokerDayStart(cfg, time.Now()))
		if err != nil {
			slog.Error("reading tournaments for daily loss evaluation", "err", err)
		}
	}
	return ledger, dayTournaments
}

// ledgerBankroll returns the account's current bankroll from an already-fetched
// full ledger — the last row's running total, exactly what
// storage.Store.GetCurrentBankroll reads, without a second store call. 0 for an
// empty (or unfetched) ledger, matching GetCurrentBankroll's empty-ledger
// behavior.
func ledgerBankroll(ledger []storage.LedgerEntry) float64 {
	if len(ledger) == 0 {
		return 0
	}
	return ledger[len(ledger)-1].RunningTotal
}

// tournamentAlreadyHandled reports whether tournament t has already been queued
// for a satellite/ticket answer, or already fully recorded with the same
// money-relevant data. The single place guarding against a duplicate tournament
// summary file event causing a second Telegram alert, a second satellite/ticket
// question, or a second pending-queue entry for the same TournamentID.
//
// A re-entry rewrite is intentionally *not* a duplicate: PokerStars overwrites
// the same summary each time screenName busts an entry, and each rewrite carries
// a larger total buy-in and a different finish, which must flow through to the
// row/ledger/bankroll (AppendTournament upserts by TournamentID).
func tournamentAlreadyHandled(t parsers.Tournament, configPath string, sc storage.Store) (bool, error) {
	pendingPath := filepath.Join(filepath.Dir(configPath), "pending_tournaments.json")
	pending, err := config.LoadPendingTournaments(pendingPath)
	if err != nil {
		return false, err
	}
	// Point lookup, not a full-history read — this runs on every tournament
	// file event (and once per re-entry bust). found is false for a partial row
	// (hand file arrived before the summary), so a tournament whose hand stats
	// landed first is correctly reported as not yet handled.
	recorded, found, err := sc.GetTournamentByID(t.TournamentID)
	if err != nil {
		return false, err
	}
	duplicate, refreshIdx := isDuplicateTournament(t, pending, recorded, found)
	if refreshIdx >= 0 {
		// Re-entry rewrite while the satellite/ticket question is still
		// unanswered: no second question, but the queued snapshot is stale —
		// refresh it so the eventual answer records the current buy-in and
		// finish instead of the first bust's. Reminder timestamps are kept.
		pending[refreshIdx].Tournament = t
		if err := config.SavePendingTournaments(pendingPath, pending); err != nil {
			slog.Error("refreshing pending tournament after re-entry rewrite", "err", err)
		}
	}
	return duplicate, nil
}

// isDuplicateTournament is the pure decision tournamentAlreadyHandled wraps
// around its I/O: t is a duplicate if it's already awaiting an answer in the
// pending queue, or already recorded with the same content (see
// sameTournamentContent).
//
// refreshPendingIdx >= 0 means t matched a pending entry by ID but its content
// changed — a re-entry rewrite arriving while the satellite/ticket question is
// unanswered. The event is still handled (duplicate true), but the caller must
// replace the queued snapshot with t so the eventual answer records the current
// money, not the first bust's.
//
// Net is deliberately not compared — a hand file folding in knockout bounty
// changes the recorded net without any re-entry.
func isDuplicateTournament(t parsers.Tournament, pending []config.PendingTournament, recorded parsers.Tournament, recordedFound bool) (duplicate bool, refreshPendingIdx int) {
	for i, p := range pending {
		if p.Tournament.TournamentID == t.TournamentID {
			if sameTournamentContent(t, p.Tournament) {
				return true, -1
			}
			return true, i
		}
	}
	if recordedFound {
		// The buy-in can't always be compared, because either side may hold a
		// value that came from a player answer the summary file doesn't
		// reproduce (a satellite answered "used a ticket", or a freeroll whose
		// file never carries a buy-in). In either case fall back to finish +
		// field size (ignore buy-in).
		if recorded.BuyInUSD == 0 || t.BuyInUSD == 0 {
			return t.FinishPos == recorded.FinishPos && t.TotalPlayers == recorded.TotalPlayers, -1
		}
		return sameTournamentContent(t, recorded), -1
	}
	return false, -1
}

// sameTournamentContent reports whether two parses of a tournament summary carry
// the same money-relevant content: buy-in (within half a cent), finish
// position, and field size. Exact float equality would misfire: a re-entry
// buy-in is a sum, and the sheets backend's number round-trip is not guaranteed
// to preserve the last bit.
func sameTournamentContent(a, b parsers.Tournament) bool {
	return math.Abs(a.BuyInUSD-b.BuyInUSD) < 0.005 &&
		a.FinishPos == b.FinishPos &&
		a.TotalPlayers == b.TotalPlayers
}

// scheduleCashSessionSummary (re)starts a quietPeriod timer for key. A new hand
// for the same table stops the pending timer and starts a fresh one, so an
// actively-played table never fires early; the summary only goes out once
// quietPeriod has passed with no new hand, built from whatever buildMessage
// returns at that moment. Mirrors watcher.go's mutex-protected timer map, since
// the timer callback runs on its own goroutine.
func scheduleCashSessionSummary(mu *sync.Mutex, timers map[string]*time.Timer, key string, quietPeriod time.Duration, alertChan chan<- string, buildMessage func() string) {
	mu.Lock()
	defer mu.Unlock()
	if t, ok := timers[key]; ok {
		t.Stop()
	}
	timers[key] = time.AfterFunc(quietPeriod, func() {
		mu.Lock()
		delete(timers, key)
		mu.Unlock()
		// An empty message means the caller's closure decided not to send
		// (e.g. the cash summary is muted or filtered out by win/loss).
		if msg := buildMessage(); msg != "" {
			alertChan <- msg
		}
	})
}

// tournamentAlertText formats the post-tournament summary alert. When the
// summary reports re-entries, a trailing line makes clear this is a re-entry
// bust and supersedes the earlier alert for the same tournament.
func tournamentAlertText(t parsers.Tournament, netUSD, bankroll float64) string {
	var msg string
	if t.BaseType() == "spingo" {
		msg = fmt.Sprintf("🎰 Spin & Go\nBuy-in: $%.2f\nPrize pool: %s\nFinish: %s\nNet: %s\nBankroll: $%.2f",
			t.BuyInUSD, bot.SpinGoPrizePoolText(t), bot.Ordinal(t.FinishPos), bot.FormatSignedUSD(netUSD), bankroll)
	} else {
		msg = fmt.Sprintf("🏆 MTT\nBuy-in: $%.2f\nFinish: %s / %d\nNet: %s\nBankroll: $%.2f",
			t.BuyInUSD, bot.Ordinal(t.FinishPos), t.TotalPlayers, bot.FormatSignedUSD(netUSD), bankroll)
	}
	if t.ReEntries > 0 {
		msg += fmt.Sprintf("\n🔁 Re-entered %d× — %d entries total for this tournament; buy-in and net cover all of them, updating the earlier alert.",
			t.ReEntries, t.ReEntries+1)
	}
	return msg
}

// cashSessionSummaryText formats the post-session summary alert.
func cashSessionSummaryText(session parsers.CashSession, bankroll float64) string {
	return fmt.Sprintf("✅ Cash Session\nStake: %s\nNet: %s\nHands: %d\nDuration: %d min\nBankroll: $%.2f",
		session.Stake, bot.FormatSignedUSD(session.NetUSD), session.Hands, session.DurationMin, bankroll)
}
