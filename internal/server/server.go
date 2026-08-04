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

// cashSessionSummaryQuietPeriod is the default Server.QuietPeriod: how long a
// cash table must go without a new hand before its session is treated as over.
// A hand history file gets a fresh event on every hand played at that table;
// rather than write and evaluate on every hand (a growing, still-incomplete
// snapshot each time), the tracker waits for the table to go quiet, then writes
// the session once and runs the ledger-based hard stops, alerts, and summary —
// the same time-gap technique poker trackers use to detect a session boundary.
// (The session-loss hard stop and alert still run per hand, from the in-memory
// session, so a bleeding session is stopped mid-play.) 5 minutes trades a
// faster session record after quitting against a higher chance of a mid-session
// break tripping it early (a resumed table then re-writes on the next quiet
// period — an idempotent upsert).
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

	// QuietPeriod is how long a cash table must go without a new hand before
	// its session is treated as over — the point the session is written to
	// storage and the ledger-based hard stops/alerts/summary run (see the cash
	// branch in Run). Defaults to cashSessionSummaryQuietPeriod; the end-to-end
	// pipeline test overrides it to a few hundred ms so session-end fires fast.
	QuietPeriod time.Duration

	// clockOffset is the difference between the collector's clock and this
	// server's, updated from each upload's ClientClock. Every date-window
	// computation ("today", the poker day, the rolling-30-day downswing) uses
	// ClientNow() instead of the server's own clock, so a server in a different
	// timezone than the poker PC still windows against the player's local day.
	// Absolute-time uses (lock expiry, parse-state TTL, pending reminders) keep
	// the server's real clock. Guarded because ClientNow is read from the bot
	// goroutine too.
	//
	// It's the collector's timezone (its *time.Location), not a duration: the
	// two machines' clocks agree on the absolute instant (both keep real time),
	// so what differs is the wall-clock *calendar* — the timezone. A duration
	// offset from clientClock.Sub(now) would capture only clock skew (≈0 with
	// NTP) and do nothing for the timezone, which is the whole point. Taken from
	// Upload.ClientClock.Location(): over the network the client's UTC offset
	// survives the RFC3339 JSON round-trip as a fixed zone; in-process it's the
	// client's actual time.Local.
	clockMu   sync.Mutex
	clientLoc *time.Location
}

// NewServer creates a Server with a fresh working directory for reconstructed
// files. The directory is created under the OS temp dir and left for the OS to
// reclaim; it holds only transient copies of files the collector already has.
func NewServer(store storage.Store, configPath string, link *transport.Link, alertChan chan<- string) (*Server, error) {
	workDir, err := os.MkdirTemp("", "tracker-work-")
	if err != nil {
		return nil, fmt.Errorf("creating server work dir: %w", err)
	}
	return &Server{store: store, configPath: configPath, link: link, alertChan: alertChan, workDir: workDir, QuietPeriod: cashSessionSummaryQuietPeriod}, nil
}

// updateClock records the collector's timezone (from an upload's ClientClock)
// so ClientNow can express the current instant on the player's wall clock. A
// zero ClientClock (e.g. an internally-generated event) is ignored.
func (s *Server) updateClock(clientClock time.Time) {
	if clientClock.IsZero() {
		return
	}
	s.clockMu.Lock()
	s.clientLoc = clientClock.Location()
	s.clockMu.Unlock()
}

// ClientNow returns the current instant expressed in the collector's timezone,
// so AsStoredClock reads the player's local wall-clock digits. Used for every
// poker-day / date-window computation, including the bot's read-side (/status,
// /stats, the weekly and monthly tickers), so windows follow the player's local
// day. Before any upload the timezone is unknown, so it falls back to this
// server's local clock.
func (s *Server) ClientNow() time.Time {
	s.clockMu.Lock()
	loc := s.clientLoc
	s.clockMu.Unlock()
	if loc == nil {
		return time.Now()
	}
	return time.Now().In(loc)
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
	// summaryTimers holds one quiet-period timer per cash table (FileID). A new
	// hand resets its table's timer; when the table goes quiet the timer signals
	// sessionEndChan, and the worker writes the session and runs the
	// ledger-based evaluation (endCashSession). Both the map and pendingSessions
	// are touched only on this goroutine — the timer callback merely sends on the
	// channel — so no mutex is needed.
	summaryTimers := make(map[string]*time.Timer)
	pendingSessions := make(map[string]parsers.CashSession)
	sessionEndChan := make(chan string)

	// firedAlerts dedups the streak/daily/stake alert evaluations: multiple cash
	// sessions in one poker day each reach endCashSession, and those conditions,
	// once true, stay true for the rest of the session or poker day — without
	// this, the same warning would re-send (and re-query the store). Keys are per
	// session (FirstHandID / TournamentID) or per poker day (the day's start
	// time); the map lives for the process and is rebuilt empty on restart, worst
	// case repeating one already-sent warning.
	firedAlerts := make(map[string]bool)

	// Sent at most once per run: a missing screen name skips every file, so
	// without this a folder of unparsed files would fire one prompt each.
	screenNameWarned := false

	// importCounts accumulates what the current /scan backfill records, reset by
	// the ImportStart marker and reported by ImportDone. Touched only on this
	// goroutine.
	var importCounts importTally

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

	for {
		var up transport.Upload
		select {
		case fileID := <-sessionEndChan:
			// A cash table went quiet: write the now-complete session and run
			// the ledger-based hard stops, alerts, and summary — all on this
			// goroutine. The per-hand session-loss guard already ran below.
			s.endCashSession(fileID, pendingSessions, firedAlerts, enforce)
			continue
		case u, ok := <-s.link.Uploads:
			if !ok {
				return // link closed → shut down
			}
			up = u
		}

		// Import markers bracket a /scan backfill. They carry no bytes and no
		// screen name, so handle them here, before reconstruct and the
		// screen-name guard. ImportStart resets the per-scan tally; ImportDone
		// reports it.
		switch up.Type {
		case transport.ImportStart:
			importCounts = importTally{}
			continue
		case transport.ImportDone:
			s.alertChan <- importReport(importCounts)
			importCounts = importTally{}
			continue
		}

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

		// A /scan backfill upload is recorded silently — no hard stop, no
		// alerts, no summary, no quiet-period deferral — and never touches the
		// live evaluation path below.
		if up.Import {
			s.importUpload(path, up, screenName, &importCounts)
			continue
		}

		// Threshold evaluation must see a turned-off alert/hard-stop as unset.
		// Blank the off ones out here (EffectiveAlerts/EffectiveHardstop) so the
		// evaluators and the per-event fetch never learn about the *_enabled
		// flags; cfg stays raw for the notification gates further down.
		evalCfg := *cfg
		evalCfg.Alerts = config.EffectiveAlerts(cfg.Alerts)
		evalCfg.Hardstop = config.EffectiveHardstop(cfg.Hardstop)

		// Anchor every date-window computation to the collector's clock, so a
		// server in a different timezone than the poker PC still windows against
		// the player's local poker day.
		s.updateClock(up.ClientClock)
		evalNow := s.ClientNow()

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
				// Per hand, from the in-memory session only — no store write and
				// no ledger read. The session-loss hard stop is the one guard
				// that must fire mid-session, and the session-loss alert warns at
				// its lower threshold before it. The write, the ledger-based hard
				// stops/alerts, and the summary all run once the table goes quiet
				// (endCashSession, via the sessionEndChan timer below).
				if err := hardstop.EvaluateSessionLoss(*session, &evalCfg, enforce); err != nil {
					slog.Error("evaluating session-loss hard stop", "err", err)
				}
				// The alert's bankroll is fetched lazily — only if it fires — so
				// a hand that doesn't breach costs no store I/O. The session isn't
				// written yet, so the live bankroll is the stored (prior) bankroll
				// plus this session's in-memory net.
				bot.EvaluateSessionLossAlert(*session, &evalCfg, func() float64 {
					prior, _ := s.store.GetCurrentBankroll()
					return prior + session.NetUSD
				}, s.alertChan)

				// Remember the latest parse for this table and (re)arm its
				// quiet-period timer; the last hand's session is what gets
				// written when the table falls quiet.
				pendingSessions[up.FileID] = *session
				armCashSessionEnd(summaryTimers, up.FileID, s.QuietPeriod, sessionEndChan)
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
			ledger, dayTournaments := fetchEvalData(s.store, &evalCfg, evalNow, true)
			bankroll := ledgerBankroll(ledger)
			if err := hardstop.EvaluateTournament(t, &evalCfg, ledger, dayTournaments, evalNow, enforce); err != nil {
				slog.Error("evaluating hard stop", "err", err)
			}
			bot.EvaluateDownswingAlert(t.BuyInUSD, &evalCfg, ledger, bankroll, evalNow, s.alertChan)
			evaluateOncePerKeyAlerts(&evalCfg, ledger, dayTournaments, bankroll, evalNow, s.alertChan, firedAlerts, "tournament:"+t.TournamentID)
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
func evaluateOncePerKeyAlerts(cfg *config.Config, ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament, bankroll float64, now time.Time, alertChan chan<- string, firedAlerts map[string]bool, sessionKey string) {
	if !firedAlerts["streak:"+sessionKey] {
		if bot.EvaluateLosingStreakAlert(cfg, ledger, bankroll, alertChan) {
			firedAlerts["streak:"+sessionKey] = true
		}
	}

	dayKey := hardstop.PokerDayStart(cfg, now).Format(time.RFC3339)
	if !firedAlerts["daily:"+dayKey] {
		if bot.EvaluateDailyLossAlert(cfg, ledger, bankroll, now, alertChan) {
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
// full Bankroll Ledger and the current poker day's tournaments — once per
// evaluation, replacing the per-hand identical fetches the evaluators used to do
// themselves. Each fetch is skipped entirely when nothing configured needs it;
// forceLedger forces the ledger fetch because the post-session and
// post-tournament summaries need the bankroll regardless of config. A failed
// fetch is logged and returns nil for that data set — every consumer treats nil
// as "no rows".
func fetchEvalData(sc storage.Store, cfg *config.Config, now time.Time, forceLedger bool) (ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament) {
	a, h := cfg.Alerts, cfg.Hardstop
	needLedger := forceLedger ||
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
		dayTournaments, err = sc.GetTournamentsSince(hardstop.PokerDayStart(cfg, now))
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

// armCashSessionEnd (re)starts a quietPeriod timer for a cash table (keyed by
// FileID). A new hand for the same table stops the pending timer and starts a
// fresh one, so an actively-played table never fires early; once quietPeriod
// passes with no new hand the timer signals sessionEndChan and the worker
// writes the session (endCashSession). The map is touched only by the worker
// goroutine (this function and endCashSession); the timer callback merely sends
// on the channel, so no mutex is needed.
func armCashSessionEnd(timers map[string]*time.Timer, fileID string, quietPeriod time.Duration, sessionEndChan chan<- string) {
	if t, ok := timers[fileID]; ok {
		t.Stop()
	}
	timers[fileID] = time.AfterFunc(quietPeriod, func() {
		sessionEndChan <- fileID
	})
}

// endCashSession runs when a cash table has gone quiet: it writes the
// now-complete session (Cash Sessions row + Bankroll Ledger row, one write —
// like a tournament, not per hand), then runs the ledger-based hard stops and
// alerts and sends the post-session summary. The per-hand session-loss guard
// already ran in Run's cash branch. A missing pendingSessions entry means this
// FileID was already flushed (a stale timer signal), so it's a no-op. Runs on
// the worker goroutine, so pendingSessions/firedAlerts stay single-threaded.
func (s *Server) endCashSession(fileID string, pendingSessions map[string]parsers.CashSession, firedAlerts map[string]bool, enforce func(reason string, durationHours int)) {
	session, ok := pendingSessions[fileID]
	if !ok {
		return
	}
	delete(pendingSessions, fileID)

	// Re-read config: it may have changed during the quiet period, and the
	// notification gates below need the raw (flag-preserving) config while the
	// evaluators need the effective one.
	cfg, err := config.LoadConfig(s.configPath)
	if err != nil {
		slog.Error("loading config for cash session end", "err", err)
		return
	}
	evalCfg := *cfg
	evalCfg.Alerts = config.EffectiveAlerts(cfg.Alerts)
	evalCfg.Hardstop = config.EffectiveHardstop(cfg.Hardstop)
	evalNow := s.ClientNow()

	before := sheetsPending(s.store)
	if err := s.store.AppendCashSession(session); err != nil {
		slog.Error("appending cash session to sheets", "err", err,
			"date", session.Date.Format("2006-01-02 15:04"), "stake", session.Stake,
			"session_id", sheets.CashSessionID(session.Date, session.FirstHandID))
		if msg := blipMessage(before, sheetsPending(s.store)); msg != "" {
			s.alertChan <- msg
		}
	} else if msg := syncedMessage(before, sheetsPending(s.store)); msg != "" {
		s.alertChan <- msg
	}

	// Always fetch the ledger (forceLedger) — the summary below needs the
	// bankroll regardless of what alerts/hard stops are configured. The fetch is
	// after the write, so the ledger includes the session just recorded.
	ledger, dayTournaments := fetchEvalData(s.store, &evalCfg, evalNow, true)
	bankroll := ledgerBankroll(ledger)
	if err := hardstop.Evaluate(session, &evalCfg, ledger, dayTournaments, evalNow, enforce); err != nil {
		slog.Error("evaluating hard stop", "err", err)
	}
	bot.EvaluateDownswingAlert(session.BuyInUSD, &evalCfg, ledger, bankroll, evalNow, s.alertChan)
	sessionKey := "cash:" + session.FirstHandID
	if config.NotificationEnabled(cfg.Notifications.StakeChange) && !firedAlerts["stake:"+sessionKey] {
		if err := bot.EvaluateStakeChangeAlert(session, s.store, s.alertChan); err != nil {
			slog.Error("evaluating stake change alert", "err", err,
				"date", session.Date.Format("2006-01-02 15:04"), "stake", session.Stake,
				"session_id", sheets.CashSessionID(session.Date, session.FirstHandID))
		} else {
			firedAlerts["stake:"+sessionKey] = true
		}
	}
	evaluateOncePerKeyAlerts(&evalCfg, ledger, dayTournaments, bankroll, evalNow, s.alertChan, firedAlerts, sessionKey)

	// Post-session summary, gated by its notification mode against the final
	// net (raw cfg — the effective copy blanks thresholds, not notifications).
	if config.ShouldSendSummary(config.SummaryMode(cfg.Notifications.CashSessionSummary), session.NetUSD) {
		s.alertChan <- cashSessionSummaryText(session, bankroll)
	}
}

// importTally counts what a /scan backfill recorded, for the completion report.
type importTally struct {
	cash        int // cash sessions appended
	tournaments int // tournaments appended
	skipped     int // tournaments skipped for needing a player answer
}

// importUpload records one /scan file silently — no hard stop, no alerts, no
// summary, no quiet-period deferral. A cash file appends its session
// immediately; a cash file that's actually a tournament hand enriches its
// tournament row (not counted as a session). A tournament summary appends
// unless it needs a player answer (a likely satellite, or a zero-buy-in
// freeroll), which is skipped and counted so the player knows to play it in
// live. Parse/store failures are logged and skipped. Idempotent: re-running a
// scan re-upserts identical rows.
func (s *Server) importUpload(path string, up transport.Upload, screenName string, tally *importTally) {
	switch up.Type {
	case "cash":
		session, tourneyStats, err := parsers.ParseCashFile(path, screenName)
		if err != nil {
			slog.Error("import: parsing cash file", "file", up.FileID, "err", err)
			return
		}
		switch {
		case session != nil:
			if err := s.store.AppendCashSession(*session); err != nil {
				slog.Error("import: appending cash session", "file", up.FileID, "err", err)
				return
			}
			tally.cash++
		case tourneyStats != nil:
			if err := s.store.UpdateTournamentHandStats(tourneyStats.TournamentID, tourneyStats.VPIP, tourneyStats.PFR, tourneyStats.Hands, tourneyStats.BountyUSD, tourneyStats.IsKnockout); err != nil {
				slog.Error("import: updating tournament hand stats", "tournament_id", tourneyStats.TournamentID, "err", err)
			}
		}
	case "tournament":
		t, err := parsers.ParseTournamentFile(path, screenName)
		if err != nil {
			slog.Error("import: parsing tournament", "file", up.FileID, "err", err)
			return
		}
		// Skip the ones the live pipeline would queue for a player answer:
		// recording them without the answer would corrupt bankroll (a ticket
		// counted as cash) or store a wrong zero buy-in. Skipping also protects
		// any already-answered row from being clobbered on a re-scan.
		if t.LikelySatellite || t.BuyInUSD == 0 {
			tally.skipped++
			return
		}
		if _, err := s.store.AppendTournament(t); err != nil {
			slog.Error("import: appending tournament", "tournament_id", t.TournamentID, "err", err)
			return
		}
		tally.tournaments++
	}
}

// importReport is the one message a /scan sends when it finishes: what it
// recorded, and how many tournaments it skipped for needing a player answer.
func importReport(t importTally) string {
	msg := fmt.Sprintf("📥 Scan complete — recorded %d cash session(s) and %d tournament(s).", t.cash, t.tournaments)
	if t.skipped > 0 {
		msg += fmt.Sprintf("\nSkipped %d tournament(s) that need a ticket-vs-cash or buy-in answer — play or re-open those in live to record them.", t.skipped)
	}
	return msg
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
