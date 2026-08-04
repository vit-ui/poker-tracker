package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

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
	// busted 50th of 126).
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
		// Freeroll/Kick-Off: the summary never carries a buy-in, so a parse is
		// always $0. After the player answers "paid $X" the recorded buy-in is
		// X > 0; a stray re-trigger re-parses $0 and must still be a duplicate
		// on finish + field, or the entry-cost question gets re-asked and a
		// wrong second answer overwrites the recorded row.
		{"freeroll answered paid, unchanged file re-parse is a duplicate", tourney("111", 0, 3, 4), nil, tourney("111", 1.00, 3, 4), true, true, -1},
		{"freeroll answered free, unchanged file re-parse is a duplicate", tourney("111", 0, 3, 4), nil, tourney("111", 0, 3, 4), true, true, -1},
		{"freeroll re-entry with a new finish is not a duplicate", tourney("111", 0, 2, 4), nil, tourney("111", 1.00, 3, 4), true, false, -1},
		{"freeroll still awaiting its buy-in answer, unchanged content", tourney("111", 0, 3, 4), pendingOf(tourney("111", 0, 3, 4)), none, false, true, -1},
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

// TestServer_ScreenNameRecovery is the regression for the collector/server
// offset desync: the collector advances its per-file sent offset on every
// upload, so a file that arrives while the screen name is empty (config wiped
// mid-session) must still be reconstructed on the server, or the next delta
// lands past a never-written working file and corrupts it. When the screen name
// returns, a full parse of the now-complete working file must record every
// hand — matching the pre-split whole-file re-read.
func TestServer_ScreenNameRecovery(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Split after the first two hands (offset of the 3rd header). The first
	// header may sit just past a BOM, not a newline.
	sep := "PokerStars Hand #"
	text := string(data)
	var offs []int
	firstHeader := true
	for start := 0; ; {
		idx := strings.Index(text[start:], sep)
		if idx < 0 {
			break
		}
		abs := start + idx
		if firstHeader || text[abs-1] == '\n' {
			offs = append(offs, abs)
			firstHeader = false
		}
		start = abs + len(sep)
	}
	if len(offs) < 3 {
		t.Fatalf("need >=3 hands in nlhe_cash.txt, got %d", len(offs))
	}
	split := offs[2]

	dir := t.TempDir()
	store, err := sqlitestore.NewStore(filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	link := transport.NewLink()
	alertChan := make(chan string, 10)
	srv, err := NewServer(store, filepath.Join(dir, "config.json"), link, alertChan)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.QuietPeriod = 50 * time.Millisecond // fire the cash session-end fast
	go srv.Run()
	go func() { // drain alerts so the screen-name warning never blocks
		for range alertChan {
		}
	}()

	const fileID = `C:\hh\nlhe_cash.txt`
	// Event 1: the first two hands, screen name not yet known — skipped for
	// parsing but must still be reconstructed on the server.
	link.Uploads <- transport.Upload{FileID: fileID, Type: "cash", Offset: 0, Bytes: data[:split], ScreenName: ""}
	// Event 2: the appended remainder, screen name now recovered.
	link.Uploads <- transport.Upload{FileID: fileID, Type: "cash", Offset: int64(split), Bytes: data[split:], ScreenName: "AnonHero01"}

	deadline := time.Now().Add(10 * time.Second)
	for {
		sessions, err := store.GetAllCashSessions()
		if err == nil && len(sessions) == 1 && sessions[0].Hands == 4 {
			return
		}
		if time.Now().After(deadline) {
			sessions, _ := store.GetAllCashSessions()
			t.Fatalf("session not fully recorded after screen-name recovery: %+v", sessions)
		}
		time.Sleep(50 * time.Millisecond)
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

		if err := store.AppendLedgerEntry("deposit", 100, time.Now()); err != nil {
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

	if err := store.AppendLedgerEntry("deposit", 100, time.Now()); err != nil {
		t.Fatalf("AppendLedgerEntry: %v", err)
	}

	now := storage.AsStoredClock(time.Now())
	session1 := parsers.CashSession{Date: now, Stake: "NL5", BuyInUSD: 5, NetUSD: -6, Hands: 40, FirstHandID: "h1"}
	if err := store.AppendCashSession(session1); err != nil {
		t.Fatalf("AppendCashSession(session1): %v", err)
	}

	ledger, dayTournaments := fetchEvalData(store, cfg, time.Now(), false)
	bankroll := ledgerBankroll(ledger)
	if bankroll != 94 {
		t.Fatalf("bankroll after session 1 = %v, want 94", bankroll)
	}
	bot.EvaluateSessionLossAlert(session1, cfg, func() float64 { return bankroll }, alertChan)
	evaluateOncePerKeyAlerts(cfg, ledger, dayTournaments, bankroll, time.Now(), alertChan, firedAlerts, "cash:h1")
	assertMsgs("session 1", []string{
		"⚠️ Session loss alert\nLost 1.2 buy-ins this session ($6.00)\nBankroll: $94.00",
		"⚠️ Daily loss alert\nDown $6.00 across all games this poker day\nBankroll: $94.00",
	})

	session2 := parsers.CashSession{Date: now.Add(time.Minute), Stake: "NL5", BuyInUSD: 5, NetUSD: -4, Hands: 30, FirstHandID: "h2"}
	if err := store.AppendCashSession(session2); err != nil {
		t.Fatalf("AppendCashSession(session2): %v", err)
	}

	ledger, dayTournaments = fetchEvalData(store, cfg, time.Now(), false)
	bankroll = ledgerBankroll(ledger)
	if bankroll != 90 {
		t.Fatalf("bankroll after session 2 = %v, want 90", bankroll)
	}
	bot.EvaluateSessionLossAlert(session2, cfg, func() float64 { return bankroll }, alertChan)
	evaluateOncePerKeyAlerts(cfg, ledger, dayTournaments, bankroll, time.Now(), alertChan, firedAlerts, "cash:h2")
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

const importHero = "AnonHero01"

// startCapturingServer is startTestServer's sibling that returns alertChan
// undrained, so a test can assert exactly which messages the server emits.
func startCapturingServer(t *testing.T) (*sqlitestore.Store, *transport.Link, chan string) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlitestore.NewStore(filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	link := transport.NewLink()
	alertChan := make(chan string, 100)
	srv, err := NewServer(store, filepath.Join(dir, "config.json"), link, alertChan)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	done := make(chan struct{})
	go func() { srv.Run(); close(done) }()
	t.Cleanup(func() {
		close(link.Uploads)
		<-done
		store.Close()
	})
	return store, link, alertChan
}

// importUp builds one /scan Import upload from a committed fixture file.
func importUp(t *testing.T, fileID, typ, sub, name string) transport.Upload {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", sub, name))
	if err != nil {
		t.Fatal(err)
	}
	return transport.Upload{FileID: fileID, Type: typ, Offset: 0, Bytes: b, ScreenName: importHero, ClientClock: time.Now(), Import: true}
}

// feedImport sends an ImportStart / uploads / ImportDone sequence and returns
// the scan-complete report. It also asserts the import is silent: the report
// (emitted on ImportDone, processed last) is the FIRST and ONLY message — any
// live-path alert would have been emitted earlier and would fail this.
func feedImport(t *testing.T, link *transport.Link, alertChan chan string, ups ...transport.Upload) string {
	t.Helper()
	link.Uploads <- transport.Upload{Type: transport.ImportStart}
	for _, up := range ups {
		link.Uploads <- up
	}
	link.Uploads <- transport.Upload{Type: transport.ImportDone}

	report, ok := waitForString(t, alertChan, 5*time.Second)
	if !ok {
		t.Fatal("no scan-complete report received")
	}
	if !strings.Contains(report, "Scan complete") {
		t.Fatalf("first message was not the scan report (a stray alert during a silent import?): %q", report)
	}
	if extra, ok := waitForString(t, alertChan, 200*time.Millisecond); ok {
		t.Errorf("unexpected extra alert during silent import: %q", extra)
	}
	return report
}

func countTournamentRows(t *testing.T, store *sqlitestore.Store, tournamentID string) int {
	t.Helper()
	snap, err := store.ExportSnapshot()
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	n := 0
	for _, r := range snap.Tournaments {
		if r.TournamentID == tournamentID {
			n++
		}
	}
	return n
}

// TestServer_ImportBackfill drives the /scan silent-import path through the real
// Run loop: a cash file, a real MTT summary, and a satellite summary. The cash
// session and the MTT are recorded, the satellite is skipped (it needs a player
// answer), no live-path alert fires, and the completion report names the counts.
func TestServer_ImportBackfill(t *testing.T) {
	store, link, alertChan := startCapturingServer(t)
	report := feedImport(t, link, alertChan,
		importUp(t, "hh/nlhe_cash.txt", "cash", "hand_history", "nlhe_cash.txt"),
		importUp(t, "ts/horse.txt", "tournament", "tournament_summary", "horse_T9000000003.txt"),
		importUp(t, "ts/satellite.txt", "tournament", "tournament_summary", "satellite_busted_T9000000013.txt"),
	)
	if !strings.Contains(report, "recorded 1 cash session(s) and 1 tournament(s)") || !strings.Contains(report, "Skipped 1") {
		t.Errorf("report = %q, want 1 cash, 1 tournament, 1 skipped", report)
	}
	sessions, err := store.GetAllCashSessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("GetAllCashSessions = %d rows (err %v), want 1", len(sessions), err)
	}
	if _, found, _ := store.GetTournamentByID("9000000003"); !found {
		t.Error("MTT 9000000003 not recorded, want recorded")
	}
	if _, found, _ := store.GetTournamentByID("9000000013"); found {
		t.Error("satellite 9000000013 recorded, want skipped")
	}
}

// TestServer_ImportKnockoutTwoFile covers the most common real backfill shape: a
// knockout tournament whose hand-history file AND summary are both in the
// folders. The hand file (parsed as "cash" but carrying tournament stats)
// enriches the row rather than counting as a cash session; the summary records
// it. The two correlate into one -ko row, counted once as a tournament.
func TestServer_ImportKnockoutTwoFile(t *testing.T) {
	store, link, alertChan := startCapturingServer(t)
	report := feedImport(t, link, alertChan,
		importUp(t, "hh/pko.txt", "cash", "hand_history", "draw27_pko_T9000000005.txt"),
		importUp(t, "ts/pko.txt", "tournament", "tournament_summary", "draw27_pko_T9000000005.txt"),
	)
	// The hand file is not a cash session; only the summary counts.
	if !strings.Contains(report, "recorded 0 cash session(s) and 1 tournament(s)") {
		t.Errorf("report = %q, want 0 cash / 1 tournament", report)
	}
	got, found, err := store.GetTournamentByID("9000000005")
	if err != nil || !found {
		t.Fatalf("KO tournament not found (err %v)", err)
	}
	// A -ko suffix only appears once the hand file's bounty markers are folded
	// into the summary row — so it proves the two files correlated.
	if got.BaseType() != "mtt" || !strings.HasSuffix(got.Type, "-ko") {
		t.Errorf("Type = %q, want an mtt -ko (proves hand+summary correlation)", got.Type)
	}
	if n := countTournamentRows(t, store, "9000000005"); n != 1 {
		t.Errorf("%d rows for the KO tournament, want exactly 1 (counted once)", n)
	}
}

// TestServer_ImportSkipsFreeroll confirms a zero-buy-in freeroll summary is
// skipped (its real entry cost is unknown without a player answer), counted as
// skipped, and never recorded.
func TestServer_ImportSkipsFreeroll(t *testing.T) {
	store, link, alertChan := startCapturingServer(t)
	report := feedImport(t, link, alertChan,
		importUp(t, "ts/freeroll.txt", "tournament", "tournament_summary", "freeroll_zero_T9000000002.txt"),
	)
	if !strings.Contains(report, "recorded 0 cash session(s) and 0 tournament(s)") || !strings.Contains(report, "Skipped 1") {
		t.Errorf("report = %q, want 0/0 recorded and 1 skipped", report)
	}
	if _, found, _ := store.GetTournamentByID("9000000002"); found {
		t.Error("freeroll 9000000002 recorded, want skipped")
	}
}

// TestServer_ImportIdempotent runs the same scan twice: the second run
// re-upserts the same rows (cash by session ID, tournament by TournamentID)
// rather than duplicating them, and reports the same counts.
func TestServer_ImportIdempotent(t *testing.T) {
	store, link, alertChan := startCapturingServer(t)
	ups := []transport.Upload{
		importUp(t, "hh/nlhe_cash.txt", "cash", "hand_history", "nlhe_cash.txt"),
		importUp(t, "ts/horse.txt", "tournament", "tournament_summary", "horse_T9000000003.txt"),
	}
	first := feedImport(t, link, alertChan, ups...)
	bankroll1, _ := store.GetCurrentBankroll()
	second := feedImport(t, link, alertChan, ups...)
	bankroll2, _ := store.GetCurrentBankroll()

	if first != second {
		t.Errorf("report changed on re-scan:\n first: %q\nsecond: %q", first, second)
	}
	if !closeEnoughServer(bankroll1, bankroll2) {
		t.Errorf("bankroll drifted on re-scan: %.2f → %.2f", bankroll1, bankroll2)
	}
	sessions, _ := store.GetAllCashSessions()
	if len(sessions) != 1 {
		t.Errorf("%d cash sessions after two scans, want 1 (no duplicate)", len(sessions))
	}
	if n := countTournamentRows(t, store, "9000000003"); n != 1 {
		t.Errorf("%d rows for the MTT after two scans, want 1 (no duplicate)", n)
	}
}

func closeEnoughServer(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 0.005
}

// TestArmCashSessionEnd_FiresAfterQuietPeriod confirms a single arm call
// signals the table's FileID once the quiet period elapses.
func TestArmCashSessionEnd_FiresAfterQuietPeriod(t *testing.T) {
	timers := make(map[string]*time.Timer)
	sessionEnd := make(chan string, 10)

	armCashSessionEnd(timers, "table-a.txt", 50*time.Millisecond, sessionEnd)

	got, ok := waitForString(t, sessionEnd, 500*time.Millisecond)
	if !ok {
		t.Fatal("expected a session-end signal after the quiet period, got none")
	}
	if got != "table-a.txt" {
		t.Errorf("got %q, want %q", got, "table-a.txt")
	}
}

// TestArmCashSessionEnd_ResetOnNewHand confirms re-arming before the quiet
// period elapses (a new hand at the same table) cancels the pending signal and
// collapses to exactly one session-end.
func TestArmCashSessionEnd_ResetOnNewHand(t *testing.T) {
	timers := make(map[string]*time.Timer)
	sessionEnd := make(chan string, 10)

	armCashSessionEnd(timers, "table-a.txt", 100*time.Millisecond, sessionEnd)
	time.Sleep(30 * time.Millisecond)
	armCashSessionEnd(timers, "table-a.txt", 100*time.Millisecond, sessionEnd)

	got, ok := waitForString(t, sessionEnd, 500*time.Millisecond)
	if !ok {
		t.Fatal("expected one session-end after the quiet period, got none")
	}
	if got != "table-a.txt" {
		t.Errorf("got %q, want %q", got, "table-a.txt")
	}
	if _, ok := waitForString(t, sessionEnd, 200*time.Millisecond); ok {
		t.Error("expected only one session-end for a table that kept getting new hands, got a second")
	}
}

// TestArmCashSessionEnd_IndependentPerTable confirms two different tables each
// get their own independent timer — re-arming one must never cancel another.
func TestArmCashSessionEnd_IndependentPerTable(t *testing.T) {
	timers := make(map[string]*time.Timer)
	sessionEnd := make(chan string, 10)

	armCashSessionEnd(timers, "table-a.txt", 50*time.Millisecond, sessionEnd)
	armCashSessionEnd(timers, "table-b.txt", 50*time.Millisecond, sessionEnd)

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		got, ok := waitForString(t, sessionEnd, 500*time.Millisecond)
		if !ok {
			t.Fatal("expected two independent session-end signals, got fewer")
		}
		seen[got] = true
	}
	if !seen["table-a.txt"] || !seen["table-b.txt"] {
		t.Errorf("expected session-end from both tables, got %v", seen)
	}
}

// TestSyncedMessage covers the pure decision behind the "queued writes just
// synced" Telegram notice: a message only when the sheets write-ahead
// fallback queue (PendingWrites) shrank between two snapshots taken around a
// successful write, naming exactly how many resolved. An unchanged or grown
// count sends nothing.
func TestSyncedMessage(t *testing.T) {
	cases := []struct {
		name          string
		before, after int
		want          string
	}{
		{"no pending writes at all", 0, 0, ""},
		{"still pending, unchanged", 2, 2, ""},
		{"one resolved", 1, 0, "✅ Synced 1 queued write(s) to Sheets"},
		{"several resolved", 3, 1, "✅ Synced 2 queued write(s) to Sheets"},
		{"grew instead of shrinking", 0, 1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := syncedMessage(c.before, c.after)
			if got != c.want {
				t.Errorf("syncedMessage(%d, %d) = %q, want %q", c.before, c.after, got, c.want)
			}
		})
	}
}

// startTestServer builds a Server over a temp sqlite store, starts its Run
// loop, and drains alertChan so nothing blocks. It returns the store (to assert
// the final persisted state), the link (to feed uploads), and the Server (for
// white-box access to the clock helpers). Teardown stops Run (by closing the
// Uploads channel) and waits for it to return before closing the store, so no
// write lands on a closed database and no worker goroutine leaks past the test.
func startTestServer(t *testing.T) (*sqlitestore.Store, *transport.Link, *Server) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlitestore.NewStore(filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	link := transport.NewLink()
	alertChan := make(chan string, 100)
	srv, err := NewServer(store, filepath.Join(dir, "config.json"), link, alertChan)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.QuietPeriod = 50 * time.Millisecond // fire the cash session-end fast
	done := make(chan struct{})
	go func() { srv.Run(); close(done) }()
	go func() {
		for range alertChan {
		}
	}()
	t.Cleanup(func() {
		close(link.Uploads) // ends Run's range over Uploads
		<-done              // wait until no more store writes can happen
		close(alertChan)    // now safe: Run (the only writer) has stopped
		store.Close()
	})
	return store, link, srv
}

// waitForCashSession polls until exactly one cash session with wantHands is
// stored, or the deadline passes, and returns it. A generous deadline covers
// the asynchronous worker loop.
func waitForCashSession(t *testing.T, store *sqlitestore.Store, wantHands int) storage.CashSessionRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		sessions, err := store.GetAllCashSessions()
		if err == nil && len(sessions) == 1 && sessions[0].Hands == wantHands {
			return sessions[0]
		}
		if time.Now().After(deadline) {
			got, _ := store.GetAllCashSessions()
			t.Fatalf("no single cash session with %d hands within deadline; got %+v", wantHands, got)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// TestServer_ClientClockAnchoring pins the split-mode client-clock anchoring at
// the composed layer, without mutating any process global (time.Local is shared
// with every other test's goroutines). After an upload carrying a collector
// clock, ClientNow() must express the current instant in the collector's zone,
// and storage.AsStoredClock(ClientNow()) must read the collector's *own*
// wall-clock digits — never re-localize to the server's zone.
//
// The collector's zone is built to differ from the test host's real zone by a
// fixed 5h, so the pre-fix t.Local() bug (which re-localizes to the host) is
// always caught regardless of where the test runs: under the bug AsStoredClock
// would yield the host's digits, which differ from the collector's by 5h; under
// the fix it yields the collector's own digits. The value is captured from a
// single ClientNow() read, so the comparison is exact — no clock-skew tolerance.
//
// All-local behavior is unchanged: before any upload ClientNow falls back to the
// server's own clock (time.Local), which the fallback assertion pins.
func TestServer_ClientClockAnchoring(t *testing.T) {
	_, _, srv := startTestServer(t)

	// Before any upload the collector's zone is unknown: ClientNow falls back to
	// the server's own local clock.
	if loc := srv.ClientNow().Location(); loc != time.Local {
		t.Fatalf("pre-upload ClientNow location = %v, want the server's time.Local fallback", loc)
	}

	// A collector in a zone 5h off the test host (guaranteed distinct) sends an
	// upload; the server adopts its zone from ClientClock.
	_, hostOff := time.Now().Zone()
	collectorZone := time.FixedZone("COLLECTOR", hostOff+5*60*60)
	srv.updateClock(time.Now().In(collectorZone))

	cn := srv.ClientNow()
	if cn.Location() != collectorZone {
		t.Fatalf("ClientNow location = %v, want the collector's zone", cn.Location())
	}
	// The stored-clock coordinate must be the collector's own wall-clock digits
	// relabelled UTC — i.e. AsStoredClock must read cn's fields, not re-localize.
	stored := storage.AsStoredClock(cn)
	want := time.Date(cn.Year(), cn.Month(), cn.Day(), cn.Hour(), cn.Minute(), cn.Second(), cn.Nanosecond(), time.UTC)
	if !stored.Equal(want) {
		t.Fatalf("AsStoredClock(ClientNow) = %v, want the collector's wall clock %v — anchoring re-localized to the server zone", stored, want)
	}
}

// TestServer_StreamingChunksMatchWholeFile is the split's core parity guarantee:
// a hand-history file streamed to the server as many small byte-delta uploads
// (the collector's real behavior — reconstruct appends each delta, the parser
// resumes) must persist byte-for-byte the same cash session as the identical
// file delivered in one whole upload. Chunk boundaries are arbitrary byte
// offsets (mid-hand and mid-line), the adversarial case the hand-boundary
// equivalence unit test doesn't cover, exercised here through the real
// reconstruct + incremental-parse path in Server.Run.
func TestServer_StreamingChunksMatchWholeFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}

	// Reference: the whole file in a single upload.
	wholeStore, wholeLink, _ := startTestServer(t)
	wholeLink.Uploads <- transport.Upload{FileID: `C:\hh\whole.txt`, Type: "cash", Offset: 0, Bytes: data, ScreenName: "AnonHero01"}
	// nlhe_cash.txt has 4 dealt hands (see TestServer_ScreenNameRecovery).
	want := waitForCashSession(t, wholeStore, 4)

	// Streamed: the same bytes in 7 arbitrary-sized append deltas.
	chunkStore, chunkLink, _ := startTestServer(t)
	const fileID = `C:\hh\stream.txt`
	const n = 7
	step := len(data) / n
	var off int
	for i := 0; i < n; i++ {
		end := off + step
		if i == n-1 {
			end = len(data)
		}
		if end <= off {
			continue
		}
		chunkLink.Uploads <- transport.Upload{FileID: fileID, Type: "cash", Offset: int64(off), Bytes: data[off:end], ScreenName: "AnonHero01"}
		off = end
	}
	got := waitForCashSession(t, chunkStore, 4)

	// Session ID differs (it hashes FirstHandID+Date, identical here) — compare
	// the money-and-stats payload, which must be identical.
	got.SessionID = ""
	want.SessionID = ""
	if got != want {
		t.Fatalf("streamed session != whole-file session:\n streamed=%+v\n whole   =%+v", got, want)
	}
}

// TestServer_RewriteSelfHealsThroughServer confirms the reconstruct self-heal:
// an offset-0 upload replaces the working file wholesale, and a subsequent parse
// of a *shorter* rewritten file (fewer hands) discards the carried parse state
// and re-parses from scratch, so the stored session reflects the new, smaller
// content — never a stale merge of old and new. PokerStars only appends in
// practice, but the split's reconstruct + parser must degrade correctly if a
// file is ever replaced.
func TestServer_RewriteSelfHealsThroughServer(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	offs := handHeaderOffsetsBytes(data)
	if len(offs) < 3 {
		t.Fatalf("need >=3 hands, got %d", len(offs))
	}
	twoHands := data[:offs[2]] // first two hands only

	store, link, _ := startTestServer(t)
	const fileID = `C:\hh\rewrite.txt`
	// Full file first (4 hands).
	link.Uploads <- transport.Upload{FileID: fileID, Type: "cash", Offset: 0, Bytes: data, ScreenName: "AnonHero01"}
	waitForCashSession(t, store, 4)

	// A shorter rewrite at offset 0 (2 hands) must replace, not append.
	link.Uploads <- transport.Upload{FileID: fileID, Type: "cash", Offset: 0, Bytes: twoHands, ScreenName: "AnonHero01"}
	got := waitForCashSession(t, store, 2)

	// Independent expectation: a from-scratch parse of the two-hand prefix.
	// (FirstHandID is not a persisted column — it only seeds the Session ID — so
	// it reads back empty and isn't compared; the self-heal is proven by the
	// hand count dropping 4→2 and the net matching the shorter parse.)
	tmp := filepath.Join(t.TempDir(), "two.txt")
	if err := os.WriteFile(tmp, twoHands, 0o644); err != nil {
		t.Fatal(err)
	}
	want, _, err := parsers.ParseCashFile(tmp, "AnonHero01")
	if err != nil {
		t.Fatalf("reference parse: %v", err)
	}
	if got.Hands != want.Hands || got.NetUSD != want.NetUSD {
		t.Fatalf("rewrite not self-healed: stored {hands:%d net:%.4f} want {hands:%d net:%.4f}",
			got.Hands, got.NetUSD, want.Hands, want.NetUSD)
	}
	if got.SessionID == "" {
		t.Fatal("self-healed session has no Session ID persisted")
	}
}

// handHeaderOffsetsBytes returns the byte offset of every PokerStars hand header
// line in data, for slicing the fixture at complete-hand boundaries.
func handHeaderOffsetsBytes(data []byte) []int64 {
	var offs []int64
	var off int64
	for _, raw := range splitKeepNL(data) {
		line := strings.TrimRight(string(raw), "\r\n")
		if len(line) >= 3 && line[0] == '\xef' && line[1] == '\xbb' && line[2] == '\xbf' {
			line = line[3:]
		}
		if strings.HasPrefix(line, "PokerStars Hand #") || strings.HasPrefix(line, "PokerStars Zoom Hand #") {
			offs = append(offs, off)
		}
		off += int64(len(raw))
	}
	return offs
}

// splitKeepNL splits data into lines, keeping each line's trailing newline.
func splitKeepNL(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			lines = append(lines, data[start:i+1])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

// TestBlipMessage covers syncedMessage's failure-side counterpart: a Sheets
// write failure caches to the fallback queue, so PendingWrites rises. A message
// fires only on the 0→nonzero transition (the start of an outage), so a burst
// of failed hands during one outage notifies once, not per hand; sqlite (always
// 0) and mid-outage (before already nonzero) stay silent.
func TestBlipMessage(t *testing.T) {
	const want = "💔 Sheets briefly unreachable — your results are saved locally and I'll sync them automatically when it's back."
	cases := []struct {
		name          string
		before, after int
		want          string
	}{
		{"sqlite or healthy — no queue movement", 0, 0, ""},
		{"outage starts (first failed write)", 0, 1, want},
		{"already mid-outage, another failure", 1, 2, ""},
		{"already mid-outage, count unchanged", 2, 2, ""},
		{"queue shrank (a recovery, not a blip)", 2, 1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := blipMessage(c.before, c.after)
			if got != c.want {
				t.Errorf("blipMessage(%d, %d) = %q, want %q", c.before, c.after, got, c.want)
			}
		})
	}
}
