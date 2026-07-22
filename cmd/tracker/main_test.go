package main

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// TestResolveStorageBackend covers the three startup-guard outcomes from
// CLAUDE.md's v2.0 backend-aware guard: an already-set backend is returned
// as-is, a v1.0 install (storage_backend unset, google_sheet_id already set)
// is silently migrated to "sheets" and the migration is persisted to disk,
// and an install where neither field is set errors instead of guessing.
func TestResolveStorageBackend(t *testing.T) {
	t.Run("already set is returned as-is", func(t *testing.T) {
		backend := "sqlite"
		cfg := &config.Config{Setup: config.Setup{StorageBackend: &backend}}

		got, err := resolveStorageBackend(cfg, filepath.Join(t.TempDir(), "config.json"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "sqlite" {
			t.Errorf("got %q, want %q", got, "sqlite")
		}
	})

	t.Run("v1.0 install migrates to sheets and persists it", func(t *testing.T) {
		sheetID := "abc123"
		cfg := &config.Config{Setup: config.Setup{GoogleSheetID: &sheetID}}
		path := filepath.Join(t.TempDir(), "config.json")

		got, err := resolveStorageBackend(cfg, path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "sheets" {
			t.Errorf("got %q, want %q", got, "sheets")
		}
		if cfg.Setup.StorageBackend == nil || *cfg.Setup.StorageBackend != "sheets" {
			t.Errorf("cfg.Setup.StorageBackend not migrated in memory: %v", cfg.Setup.StorageBackend)
		}

		reloaded, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if reloaded.Setup.StorageBackend == nil || *reloaded.Setup.StorageBackend != "sheets" {
			t.Errorf("migration not persisted to disk: %v", reloaded.Setup.StorageBackend)
		}
	})

	t.Run("neither field set errors", func(t *testing.T) {
		cfg := &config.Config{}

		_, err := resolveStorageBackend(cfg, filepath.Join(t.TempDir(), "config.json"))
		if err == nil {
			t.Fatal("expected an error when setup has never run")
		}
	})
}

// TestIsDuplicateTournament exercises the single choke point guarding
// against a duplicate tournament summary file event (a stray re-trigger, or
// literally the same file copied) — the exact bug that caused a repeated
// Telegram alert and a repeated satellite/ticket question for the same
// tournament before this check existed — while still letting a re-entry
// rewrite (a larger buy-in / different finish for an already-recorded
// TournamentID) through to update the row, ledger, and bankroll, refreshing
// a stale pending-queue snapshot, and never re-asking a satellite question
// already answered "used a ticket" (recorded buy-in zeroed).
func TestIsDuplicateTournament(t *testing.T) {
	pendingOf := func(tourneys ...parsers.Tournament) []config.PendingTournament {
		pending := make([]config.PendingTournament, len(tourneys))
		for i, tt := range tourneys {
			pending[i] = config.PendingTournament{Tournament: tt}
		}
		return pending
	}
	// tourney builds a parsed/recorded tournament with the money-relevant
	// fields isDuplicateTournament compares.
	tourney := func(id string, buyIn float64, finishPos, players int) parsers.Tournament {
		return parsers.Tournament{TournamentID: id, BuyInUSD: buyIn, FinishPos: finishPos, TotalPlayers: players}
	}

	// The first bust of a re-entry MTT, as already recorded (buy-in $1.10,
	// busted 50th of 126) — see data/ TS…T4016700037.
	firstBust := tourney("111", 1.10, 50, 126)
	// A satellite the player answered "used a ticket" for: RecoverTicketPayout
	// zeroed the recorded buy-in, but a re-parse of the unchanged file still
	// reads the real $0.50.
	ticketResolved := tourney("111", 0, 3, 3)
	none := parsers.Tournament{}

	cases := []struct {
		name          string
		t             parsers.Tournament
		pending       []config.PendingTournament
		recorded      parsers.Tournament
		recordedFound bool
		want          bool
		wantRefresh   int
	}{
		{"brand new tournament, neither pending nor recorded", tourney("111", 1.10, 50, 126), nil, none, false, false, -1},
		{"already awaiting a satellite/ticket answer, unchanged content", tourney("111", 0.50, 3, 3), pendingOf(tourney("111", 0.50, 3, 3)), none, false, true, -1},
		{"pending + re-entry rewrite: still handled but queue must refresh", tourney("111", 2.20, 60, 140), pendingOf(tourney("111", 1.10, 50, 126)), none, false, true, 0},
		{"pending + re-entry rewrite at a later index", tourney("111", 2.20, 60, 140), pendingOf(tourney("999", 5, 1, 3), tourney("111", 1.10, 50, 126)), none, false, true, 1},
		{"identical re-parse of a recorded tournament is a duplicate", firstBust, nil, firstBust, true, true, -1},
		{"re-entry rewrite: larger buy-in is not a duplicate", tourney("111", 2.20, 60, 140), nil, firstBust, true, false, -1},
		{"re-entry rewrite: final $3.30 / 43rd of 162 is not a duplicate", tourney("111", 3.30, 43, 162), nil, firstBust, true, false, -1},
		{"only finish position changed is not a duplicate", tourney("111", 1.10, 43, 126), nil, firstBust, true, false, -1},
		{"only field size changed is not a duplicate", tourney("111", 1.10, 50, 162), nil, firstBust, true, false, -1},
		{"a different tournament pending doesn't false-positive", tourney("111", 1.10, 50, 126), pendingOf(tourney("222", 1.10, 50, 126)), none, false, false, -1},
		// The ticket-answer regression: recorded buy-in is 0 (satellite
		// resolved as a ticket entry), the file re-parses as $0.50 — an
		// unchanged file must still be a duplicate, or every ticket-resolved
		// tournament gets re-queued and re-asked on any stray event.
		{"ticket-resolved satellite, unchanged file re-parse is a duplicate", tourney("111", 0.50, 3, 3), nil, ticketResolved, true, true, -1},
		{"ticket-resolved satellite, re-entry rewrite (new finish) is not a duplicate", tourney("111", 1.00, 2, 3), nil, ticketResolved, true, false, -1},
		// Buy-in comparison is a half-cent tolerance, not float equality: a
		// re-entry sum (1.10+2.20) is not exactly 3.30 in float64, and the
		// sheets backend's number round-trip may not preserve the last bit.
		{"float-noise buy-in difference still matches", tourney("111", 1.10+2.20, 43, 162), nil, tourney("111", 3.30, 43, 162), true, true, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, gotRefresh := isDuplicateTournament(c.t, c.pending, c.recorded, c.recordedFound)
			if got != c.want || gotRefresh != c.wantRefresh {
				t.Errorf("isDuplicateTournament(%q, ...) = (%v, %d), want (%v, %d)", c.t.TournamentID, got, gotRefresh, c.want, c.wantRefresh)
			}
		})
	}
}

func TestCashSessionSummaryText(t *testing.T) {
	cases := []struct {
		name     string
		session  parsers.CashSession
		bankroll float64
		want     string
	}{
		{
			name: "winning session",
			session: parsers.CashSession{
				Stake: "NL10", NetUSD: 5.5, Hands: 42, DurationMin: 37,
			},
			bankroll: 105.5,
			want:     "✅ Cash Session\nStake: NL10\nNet: +$5.50\nHands: 42\nDuration: 37 min\nBankroll: $105.50",
		},
		{
			name: "losing session",
			session: parsers.CashSession{
				Stake: "NL25", NetUSD: -12.75, Hands: 10, DurationMin: 5,
			},
			bankroll: 87.25,
			want:     "✅ Cash Session\nStake: NL25\nNet: -$12.75\nHands: 10\nDuration: 5 min\nBankroll: $87.25",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cashSessionSummaryText(c.session, c.bankroll)
			if got != c.want {
				t.Errorf("cashSessionSummaryText() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestTournamentAlreadyHandledIntegration drives the real I/O wrapper — a
// sqlitestore in a temp dir plus the real pending_tournaments.json round-trip
// — through the duplicate/rewrite scenarios the pure isDuplicateTournament
// test covers in isolation. The original frozen-at-first-bust bug lived in
// this wiring (the pure decision was fine; the composition wasn't), so the
// composition is what this test pins.
func TestTournamentAlreadyHandledIntegration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	store, err := sqlitestore.NewStore(filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	date := time.Date(2026, 7, 21, 23, 21, 0, 0, time.UTC)
	tourney := func(id string, buyIn float64, finishPos, players int, net float64) parsers.Tournament {
		return parsers.Tournament{
			Date: date, Type: "mtt", TournamentID: id,
			BuyInUSD: buyIn, FinishPos: finishPos, TotalPlayers: players, NetUSD: net,
		}
	}
	handled := func(tt parsers.Tournament) bool {
		t.Helper()
		got, err := tournamentAlreadyHandled(tt, configPath, store)
		if err != nil {
			t.Fatalf("tournamentAlreadyHandled(%s): %v", tt.TournamentID, err)
		}
		return got
	}

	firstBust := tourney("111", 1.10, 50, 126, -1.10)
	if handled(firstBust) {
		t.Error("fresh tournament reported as already handled")
	}
	if _, err := store.AppendTournament(firstBust); err != nil {
		t.Fatalf("AppendTournament: %v", err)
	}
	if !handled(firstBust) {
		t.Error("identical re-parse of a recorded tournament not reported as duplicate")
	}
	rewrite := tourney("111", 2.20, 60, 140, -2.20)
	if handled(rewrite) {
		t.Error("re-entry rewrite wrongly reported as duplicate — the frozen-at-first-bust bug")
	}
	if _, err := store.AppendTournament(rewrite); err != nil {
		t.Fatalf("AppendTournament(rewrite): %v", err)
	}
	if !handled(rewrite) {
		t.Error("re-parse after processing the rewrite not reported as duplicate")
	}

	// Ticket-resolved satellite: recorded with buy-in zeroed (as
	// bot.RecoverTicketPayout stores it); the unchanged file still parses
	// the real $0.50 — must be a duplicate, not a re-queue + second question.
	satRecorded := parsers.Tournament{Date: date, Type: "spingo", TournamentID: "555", BuyInUSD: 0, FinishPos: 3, TotalPlayers: 3, NetUSD: 0}
	if _, err := store.AppendTournament(satRecorded); err != nil {
		t.Fatalf("AppendTournament(satellite): %v", err)
	}
	satParse := parsers.Tournament{Date: date, Type: "spingo", TournamentID: "555", BuyInUSD: 0.50, FinishPos: 3, TotalPlayers: 3, NetUSD: -0.50}
	if !handled(satParse) {
		t.Error("ticket-resolved satellite's unchanged file re-parse not reported as duplicate — would re-ask the answered question")
	}

	// Pending-queue refresh: a rewrite arriving while the satellite question
	// is unanswered is still handled (no second question) but must replace
	// the queued stale snapshot with the current money.
	pendingPath := filepath.Join(dir, "pending_tournaments.json")
	stale := tourney("777", 1.10, 50, 126, -1.10)
	if err := config.SavePendingTournaments(pendingPath, []config.PendingTournament{{Tournament: stale, FirstQueuedAt: date, LastRemindedAt: date}}); err != nil {
		t.Fatalf("SavePendingTournaments: %v", err)
	}
	rewrite777 := tourney("777", 2.20, 60, 140, -2.20)
	if !handled(rewrite777) {
		t.Error("rewrite of a pending tournament not reported as handled — would queue a second entry")
	}
	pending, err := config.LoadPendingTournaments(pendingPath)
	if err != nil {
		t.Fatalf("LoadPendingTournaments: %v", err)
	}
	if len(pending) != 1 || pending[0].Tournament.BuyInUSD != 2.20 || pending[0].Tournament.FinishPos != 60 {
		t.Errorf("pending queue not refreshed with the rewrite: %+v", pending)
	}
	if !pending[0].FirstQueuedAt.Equal(date) || !pending[0].LastRemindedAt.Equal(date) {
		t.Errorf("pending refresh must keep the reminder timestamps: %+v", pending[0])
	}

	// Partial row (hand file arrived before the summary): not yet handled —
	// the summary still needs processing.
	if err := store.UpdateTournamentHandStats("888", 20, 10, 50, 0, false); err != nil {
		t.Fatalf("UpdateTournamentHandStats: %v", err)
	}
	if handled(tourney("888", 1.10, 12, 90, -1.10)) {
		t.Error("partial hand-file-only row wrongly reported as handled — its summary would never be recorded")
	}
}

// TestLedgerBankroll confirms the bankroll derived from an already-fetched
// full ledger — its last row's running total — equals what a separate
// GetCurrentBankroll store call returns, which is the read it replaced on
// the worker's per-event path.
func TestLedgerBankroll(t *testing.T) {
	t.Run("empty ledger is 0", func(t *testing.T) {
		if got := ledgerBankroll(nil); got != 0 {
			t.Errorf("ledgerBankroll(nil) = %v, want 0", got)
		}
	})

	t.Run("matches GetCurrentBankroll on a real store", func(t *testing.T) {
		store, err := sqlitestore.NewStore(filepath.Join(t.TempDir(), "tracker.db"))
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		defer store.Close()

		if err := store.AppendLedgerEntry("deposit", 100); err != nil {
			t.Fatalf("AppendLedgerEntry: %v", err)
		}
		session := parsers.CashSession{Date: storage.AsStoredClock(time.Now()), Stake: "NL5", NetUSD: -6, FirstHandID: "h1"}
		if err := store.AppendCashSession(session); err != nil {
			t.Fatalf("AppendCashSession: %v", err)
		}

		ledger, err := store.GetLedgerEntriesSince(time.Time{})
		if err != nil {
			t.Fatalf("GetLedgerEntriesSince: %v", err)
		}
		want, err := store.GetCurrentBankroll()
		if err != nil {
			t.Fatalf("GetCurrentBankroll: %v", err)
		}
		if got := ledgerBankroll(ledger); got != want || got != 94 {
			t.Errorf("ledgerBankroll = %v, GetCurrentBankroll = %v, want both 94", got, want)
		}
	})
}

// TestWorkerAlertEvaluationEndToEnd drives the worker's post-write
// evaluation path — one shared fetchEvalData per event feeding every
// evaluator — against a real sqlite store, asserting the exact alert
// messages the old per-evaluator-fetch code produced for the same rows.
// Hand-checked sequence: deposit $100; session 1 loses $6 at a $5 buy-in
// (session-loss alert at 1.2 buy-ins, daily-loss alert at -$6, bankroll
// $94); session 2 loses $4 (under the session-loss threshold, completes the
// 2-session losing streak, daily alert already fired for this poker day and
// must not repeat, bankroll $90). Hard stop config left null so Evaluate
// short-circuits — its trigger path (taskkill + lockfile) stays out of unit
// tests per the working agreement.
func TestWorkerAlertEvaluationEndToEnd(t *testing.T) {
	store, err := sqlitestore.NewStore(filepath.Join(t.TempDir(), "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sessionLoss := 1.0
	streak := 2
	daily := 5.0
	cfg := &config.Config{Alerts: config.Alerts{
		SessionLossBuyins: &sessionLoss,
		LosingStreakCount: &streak,
		DailyLossUSD:      &daily,
	}}

	alertChan := make(chan string, 10)
	firedAlerts := make(map[string]bool)
	drain := func() []string {
		var msgs []string
		for {
			select {
			case m := <-alertChan:
				msgs = append(msgs, m)
			default:
				return msgs
			}
		}
	}
	assertMsgs := func(step string, want []string) {
		t.Helper()
		got := drain()
		if len(got) != len(want) {
			t.Fatalf("%s: got %d alerts %q, want %d %q", step, len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: alert[%d] = %q, want %q", step, i, got[i], want[i])
			}
		}
	}

	if err := store.AppendLedgerEntry("deposit", 100); err != nil {
		t.Fatalf("AppendLedgerEntry: %v", err)
	}

	now := storage.AsStoredClock(time.Now())
	session1 := parsers.CashSession{Date: now, Stake: "NL5", BuyInUSD: 5, NetUSD: -6, Hands: 40, FirstHandID: "h1"}
	if err := store.AppendCashSession(session1); err != nil {
		t.Fatalf("AppendCashSession(session1): %v", err)
	}

	ledger, dayTournaments := fetchEvalData(store, cfg, false)
	bankroll := ledgerBankroll(ledger)
	if bankroll != 94 {
		t.Fatalf("bankroll after session 1 = %v, want 94", bankroll)
	}
	bot.EvaluateSessionLossAlert(session1, cfg, bankroll, alertChan)
	evaluateOncePerKeyAlerts(cfg, ledger, dayTournaments, bankroll, alertChan, firedAlerts, "cash:h1")
	assertMsgs("session 1", []string{
		"⚠️ Session loss alert\nLost 1.2 buy-ins this session ($6.00)\nBankroll: $94.00",
		"⚠️ Daily loss alert\nDown $6.00 across all games this poker day\nBankroll: $94.00",
	})

	session2 := parsers.CashSession{Date: now.Add(time.Minute), Stake: "NL5", BuyInUSD: 5, NetUSD: -4, Hands: 30, FirstHandID: "h2"}
	if err := store.AppendCashSession(session2); err != nil {
		t.Fatalf("AppendCashSession(session2): %v", err)
	}

	ledger, dayTournaments = fetchEvalData(store, cfg, false)
	bankroll = ledgerBankroll(ledger)
	if bankroll != 90 {
		t.Fatalf("bankroll after session 2 = %v, want 90", bankroll)
	}
	bot.EvaluateSessionLossAlert(session2, cfg, bankroll, alertChan)
	evaluateOncePerKeyAlerts(cfg, ledger, dayTournaments, bankroll, alertChan, firedAlerts, "cash:h2")
	assertMsgs("session 2", []string{
		"⚠️ Losing streak alert\n2 losing sessions in a row\nBankroll: $90.00",
	})
}

func TestTournamentAlertText(t *testing.T) {
	mtt := func(finish, players, reEntries int, buyIn float64) parsers.Tournament {
		return parsers.Tournament{Type: "mtt", BuyInUSD: buyIn, FinishPos: finish, TotalPlayers: players, ReEntries: reEntries}
	}
	cases := []struct {
		name    string
		t       parsers.Tournament
		netUSD  float64
		bank    float64
		wantHas string // substring the re-entry line must (not) contain
		reEntry bool
	}{
		{
			name: "plain MTT has no re-entry line", t: mtt(50, 126, 0, 1.10), netUSD: -1.10, bank: 22.00,
			reEntry: false,
		},
		{
			name: "re-entry MTT names the entry count", t: mtt(43, 162, 2, 3.30), netUSD: -3.30, bank: 19.80,
			wantHas: "🔁 Re-entered 2× — 3 entries total", reEntry: true,
		},
	}
	// The Spin & Go branch renders the prize pool via bot.SpinGoPrizePoolText
	// — the nil-*Multiplier ticket-pool path once caused a production panic
	// when formatted directly, so both pool shapes are pinned in full here.
	mult := 2.0
	spingoCases := []struct {
		name   string
		t      parsers.Tournament
		netUSD float64
		bank   float64
		want   string
	}{
		{
			name: "spingo with a real cash pool",
			t: parsers.Tournament{
				Type: "spingo", BuyInUSD: 0.50, PrizePoolUSD: 1.00,
				Multiplier: &mult, FinishPos: 1, TotalPlayers: 3,
			},
			netUSD: 0.50, bank: 23.58,
			want: "🎰 Spin & Go\nBuy-in: $0.50\nPrize pool: $1.00 (2x)\nFinish: 1st\nNet: +$0.50\nBankroll: $23.58",
		},
		{
			name: "spingo with a ticket-denominated pool (nil multiplier)",
			t: parsers.Tournament{
				Type: "spingo", BuyInUSD: 0.50,
				PrizePoolDescription: "$1.00 Power Path Step 2 Ticket",
				FinishPos:            3, TotalPlayers: 3,
			},
			netUSD: -0.50, bank: 23.08,
			want: "🎰 Spin & Go\nBuy-in: $0.50\nPrize pool: $1.00 Power Path Step 2 Ticket\nFinish: 3rd\nNet: -$0.50\nBankroll: $23.08",
		},
		{
			name: "knockout spingo re-entry keeps the spingo format plus the re-entry line",
			t: parsers.Tournament{
				Type: "spingo-ko", BuyInUSD: 1.00, PrizePoolUSD: 2.00,
				Multiplier: &mult, FinishPos: 2, TotalPlayers: 3, ReEntries: 1,
			},
			netUSD: -1.00, bank: 22.08,
			want: "🎰 Spin & Go\nBuy-in: $1.00\nPrize pool: $2.00 (2x)\nFinish: 2nd\nNet: -$1.00\nBankroll: $22.08\n🔁 Re-entered 1× — 2 entries total for this tournament; buy-in and net cover all of them, updating the earlier alert.",
		},
	}
	for _, c := range spingoCases {
		t.Run(c.name, func(t *testing.T) {
			if got := tournamentAlertText(c.t, c.netUSD, c.bank); got != c.want {
				t.Errorf("tournamentAlertText() =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tournamentAlertText(c.t, c.netUSD, c.bank)
			has := strings.Contains(got, "🔁")
			if has != c.reEntry {
				t.Errorf("re-entry line present = %v, want %v\ngot: %q", has, c.reEntry, got)
			}
			if c.reEntry && !strings.Contains(got, c.wantHas) {
				t.Errorf("missing %q in:\n%s", c.wantHas, got)
			}
		})
	}
}

// waitForString blocks until a string arrives on ch or timeout elapses.
func waitForString(t *testing.T, ch <-chan string, timeout time.Duration) (string, bool) {
	t.Helper()
	select {
	case s := <-ch:
		return s, true
	case <-time.After(timeout):
		return "", false
	}
}

// TestScheduleCashSessionSummary_FiresAfterQuietPeriod confirms a single
// schedule call sends the built message once the quiet period elapses.
func TestScheduleCashSessionSummary_FiresAfterQuietPeriod(t *testing.T) {
	var mu sync.Mutex
	timers := make(map[string]*time.Timer)
	alertChan := make(chan string, 10)

	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 50*time.Millisecond, alertChan, func() string {
		return "final summary"
	})

	got, ok := waitForString(t, alertChan, 500*time.Millisecond)
	if !ok {
		t.Fatal("expected a summary after the quiet period, got none")
	}
	if got != "final summary" {
		t.Errorf("got %q, want %q", got, "final summary")
	}
}

// TestScheduleCashSessionSummary_ResetOnNewHand confirms rescheduling before
// the quiet period elapses (simulating a new hand at the same table) cancels
// the pending send and collapses to exactly one message, built from the
// latest snapshot rather than the first one.
func TestScheduleCashSessionSummary_ResetOnNewHand(t *testing.T) {
	var mu sync.Mutex
	timers := make(map[string]*time.Timer)
	alertChan := make(chan string, 10)

	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 100*time.Millisecond, alertChan, func() string {
		return "hand 1 snapshot"
	})
	time.Sleep(30 * time.Millisecond)
	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 100*time.Millisecond, alertChan, func() string {
		return "hand 2 snapshot"
	})

	got, ok := waitForString(t, alertChan, 500*time.Millisecond)
	if !ok {
		t.Fatal("expected one summary after the quiet period, got none")
	}
	if got != "hand 2 snapshot" {
		t.Errorf("got %q, want the latest snapshot %q", got, "hand 2 snapshot")
	}
	if _, ok := waitForString(t, alertChan, 200*time.Millisecond); ok {
		t.Error("expected only one summary for a table that kept getting new hands, got a second")
	}
}

// TestScheduleCashSessionSummary_IndependentPerTable confirms two different
// tables (different paths) each get their own independent timer and summary
// — resetting one table's timer must never cancel another table's.
func TestScheduleCashSessionSummary_IndependentPerTable(t *testing.T) {
	var mu sync.Mutex
	timers := make(map[string]*time.Timer)
	alertChan := make(chan string, 10)

	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 50*time.Millisecond, alertChan, func() string {
		return "table a"
	})
	scheduleCashSessionSummary(&mu, timers, "table-b.txt", 50*time.Millisecond, alertChan, func() string {
		return "table b"
	})

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		got, ok := waitForString(t, alertChan, 500*time.Millisecond)
		if !ok {
			t.Fatal("expected two independent summaries, got fewer")
		}
		seen[got] = true
	}
	if !seen["table a"] || !seen["table b"] {
		t.Errorf("expected summaries from both tables, got %v", seen)
	}
}
