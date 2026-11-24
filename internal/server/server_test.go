package server

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func TestIsDuplicateTournament(t *testing.T) {
	pendingOf := func(tourneys ...parsers.Tournament) []config.PendingTournament {
		pending := make([]config.PendingTournament, len(tourneys))
		for i, tt := range tourneys {
			pending[i] = config.PendingTournament{Tournament: tt}
		}
		return pending
	}
	tourney := func(id string, buyInCents money.Cents, finishPos, players int) parsers.Tournament {
		return parsers.Tournament{TournamentID: id, BuyInCents: buyInCents, FinishPos: finishPos, TotalPlayers: players}
	}

	firstBust := tourney("111", 110, 50, 126)
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
		{"brand new tournament, neither pending nor recorded", tourney("111", 110, 50, 126), nil, none, false, false, -1},
		{"already awaiting a satellite/ticket answer, unchanged content", tourney("111", 50, 3, 3), pendingOf(tourney("111", 50, 3, 3)), none, false, true, -1},
		{"pending + re-entry rewrite: still handled but queue must refresh", tourney("111", 220, 60, 140), pendingOf(tourney("111", 110, 50, 126)), none, false, true, 0},
		{"pending + re-entry rewrite at a later index", tourney("111", 220, 60, 140), pendingOf(tourney("999", 500, 1, 3), tourney("111", 110, 50, 126)), none, false, true, 1},
		{"identical re-parse of a recorded tournament is a duplicate", firstBust, nil, firstBust, true, true, -1},
		{"re-entry rewrite: larger buy-in is not a duplicate", tourney("111", 220, 60, 140), nil, firstBust, true, false, -1},
		{"re-entry rewrite: final $3.30 / 43rd of 162 is not a duplicate", tourney("111", 330, 43, 162), nil, firstBust, true, false, -1},
		{"only finish position changed is not a duplicate", tourney("111", 110, 43, 126), nil, firstBust, true, false, -1},
		{"only field size changed is not a duplicate", tourney("111", 110, 50, 162), nil, firstBust, true, false, -1},
		{"a different tournament pending doesn't false-positive", tourney("111", 110, 50, 126), pendingOf(tourney("222", 110, 50, 126)), none, false, false, -1},
		{"ticket-resolved satellite, unchanged file re-parse is a duplicate", tourney("111", 50, 3, 3), nil, ticketResolved, true, true, -1},
		{"ticket-resolved satellite, re-entry rewrite (new finish) is not a duplicate", tourney("111", 100, 2, 3), nil, ticketResolved, true, false, -1},
		{"summed buy-in cents still match", tourney("111", 110+220, 43, 162), nil, tourney("111", 330, 43, 162), true, true, -1},
		{"freeroll answered paid, unchanged file re-parse is a duplicate", tourney("111", 0, 3, 4), nil, tourney("111", 100, 3, 4), true, true, -1},
		{"freeroll answered free, unchanged file re-parse is a duplicate", tourney("111", 0, 3, 4), nil, tourney("111", 0, 3, 4), true, true, -1},
		{"freeroll re-entry with a new finish is not a duplicate", tourney("111", 0, 2, 4), nil, tourney("111", 100, 3, 4), true, false, -1},
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

func TestServer_ScreenNameRecovery(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
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
	srv.QuietPeriod = 50 * time.Millisecond
	go srv.Run()
	go func() {
		for range alertChan {
		}
	}()

	const fileID = `C:\hh\nlhe_cash.txt`
	link.Uploads <- transport.Upload{FileID: fileID, Type: "cash", Offset: 0, Bytes: data[:split], ScreenName: ""}
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
		name          string
		session       parsers.CashSession
		bankrollCents money.Cents
		want          string
	}{
		{
			name: "winning session",
			session: parsers.CashSession{
				Stake: "NL10", NetCents: 550, Hands: 42, DurationMin: 37,
			},
			bankrollCents: 10550,
			want:          "✅ Cash Session\nStake: NL10\nNet: +$5.50\nHands: 42\nDuration: 37 min\nBankroll: $105.50"},
		{
			name: "losing session",
			session: parsers.CashSession{
				Stake: "NL25", NetCents: -1275, Hands: 10, DurationMin: 5,
			},
			bankrollCents: 8725,
			want:          "✅ Cash Session\nStake: NL25\nNet: -$12.75\nHands: 10\nDuration: 5 min\nBankroll: $87.25"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cashSessionSummaryText(c.session, c.bankrollCents)
			if got != c.want {
				t.Errorf("cashSessionSummaryText() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTournamentAlreadyHandledIntegration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	store, err := sqlitestore.NewStore(filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	date := time.Date(2026, 7, 21, 23, 21, 0, 0, time.UTC)
	tourney := func(id string, buyInCents money.Cents, finishPos, players int, netCents money.Cents) parsers.Tournament {
		return parsers.Tournament{
			Date: date, Type: "mtt", TournamentID: id,
			BuyInCents: buyInCents, FinishPos: finishPos, TotalPlayers: players, NetCents: netCents,
		}
	}
	handled := func(tt parsers.Tournament) bool {
		t.Helper()
		results := tournamentResults{server: &Server{store: store}, path: filepath.Join(filepath.Dir(configPath), "pending_tournaments.json")}
		_, _, got, err := results.inspectOfficial(tt, false)
		if err != nil {
			t.Fatalf("tournamentAlreadyHandled(%s): %v", tt.TournamentID, err)
		}
		return got
	}

	firstBust := tourney("111", 110, 50, 126, -110)
	if handled(firstBust) {
		t.Error("fresh tournament reported as already handled")
	}
	if _, err := store.AppendTournament(firstBust); err != nil {
		t.Fatalf("AppendTournament: %v", err)
	}
	if !handled(firstBust) {
		t.Error("identical re-parse of a recorded tournament not reported as duplicate")
	}
	rewrite := tourney("111", 220, 60, 140, -220)
	if handled(rewrite) {
		t.Error("re-entry rewrite wrongly reported as duplicate — the frozen-at-first-bust bug")
	}
	if _, err := store.AppendTournament(rewrite); err != nil {
		t.Fatalf("AppendTournament(rewrite): %v", err)
	}
	if !handled(rewrite) {
		t.Error("re-parse after processing the rewrite not reported as duplicate")
	}

	satRecorded := parsers.Tournament{Date: date, Type: "spingo", TournamentID: "555", BuyInCents: 0, FinishPos: 3, TotalPlayers: 3, NetCents: 0}
	if _, err := store.AppendTournament(satRecorded); err != nil {
		t.Fatalf("AppendTournament(satellite): %v", err)
	}
	satParse := parsers.Tournament{Date: date, Type: "spingo", TournamentID: "555", BuyInCents: 50, FinishPos: 3, TotalPlayers: 3, NetCents: -50}
	if !handled(satParse) {
		t.Error("ticket-resolved satellite's unchanged file re-parse not reported as duplicate — would re-ask the answered question")
	}

	pendingPath := filepath.Join(dir, "pending_tournaments.json")
	stale := tourney("777", 110, 50, 126, -110)
	if err := config.SavePendingTournaments(pendingPath, []config.PendingTournament{{Tournament: stale, FirstQueuedAt: date, LastRemindedAt: date}}); err != nil {
		t.Fatalf("SavePendingTournaments: %v", err)
	}
	rewrite777 := tourney("777", 220, 60, 140, -220)
	if !handled(rewrite777) {
		t.Error("rewrite of a pending tournament not reported as handled — would queue a second entry")
	}
	pending, err := config.LoadPendingTournaments(pendingPath)
	if err != nil {
		t.Fatalf("LoadPendingTournaments: %v", err)
	}
	if len(pending) != 1 || pending[0].Tournament.BuyInCents != 220 || pending[0].Tournament.FinishPos != 60 {
		t.Errorf("pending queue not refreshed with the rewrite: %+v", pending)
	}
	if !pending[0].FirstQueuedAt.Equal(date) || !pending[0].LastRemindedAt.Equal(date) {
		t.Errorf("pending refresh must keep the reminder timestamps: %+v", pending[0])
	}

	if err := store.UpdateTournamentHandStats("888", 20, 10, 50, 0, false); err != nil {
		t.Fatalf("UpdateTournamentHandStats: %v", err)
	}
	if handled(tourney("888", 110, 12, 90, -110)) {
		t.Error("partial hand-file-only row wrongly reported as handled — its summary would never be recorded")
	}
}

func TestLedgerBankroll(t *testing.T) {
	t.Run("empty ledger is 0", func(t *testing.T) {
		if gotCents := ledgerBankroll(nil); gotCents != 0 {
			t.Errorf("ledgerBankroll(nil) = %v, want 0", gotCents)
		}
	})

	t.Run("matches GetCurrentBankroll on a real store", func(t *testing.T) {
		store, err := sqlitestore.NewStore(filepath.Join(t.TempDir(), "tracker.db"))
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		defer store.Close()

		if err := store.AppendLedgerEntry("deposit", 10000, time.Now(), ""); err != nil {
			t.Fatalf("AppendLedgerEntry: %v", err)
		}
		session := parsers.CashSession{Date: storage.AsStoredClock(time.Now()), Stake: "NL5", NetCents: -600, FirstHandID: "h1"}
		if err := store.AppendCashSession(session); err != nil {
			t.Fatalf("AppendCashSession: %v", err)
		}

		ledger, err := store.GetLedgerEntriesSince(time.Time{})
		if err != nil {
			t.Fatalf("GetLedgerEntriesSince: %v", err)
		}
		wantCents, err := store.GetCurrentBankroll()
		if err != nil {
			t.Fatalf("GetCurrentBankroll: %v", err)
		}
		if gotCents := ledgerBankroll(ledger); gotCents != wantCents || gotCents != 9400 {
			t.Errorf("ledgerBankroll = %v, GetCurrentBankroll = %v, want both 94", gotCents, wantCents)
		}
	})
}

func TestWorkerAlertEvaluationEndToEnd(t *testing.T) {
	store, err := sqlitestore.NewStore(filepath.Join(t.TempDir(), "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sessionLoss := 1.0
	streak := 2
	dailyCents := money.Cents(500)
	cfg := &config.Config{Alerts: config.Alerts{
		SessionLossBuyins: &sessionLoss,
		LosingStreakCount: &streak,
		DailyLossCents:    &dailyCents,
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

	if err := store.AppendLedgerEntry("deposit", 10000, time.Now(), ""); err != nil {
		t.Fatalf("AppendLedgerEntry: %v", err)
	}

	now := storage.AsStoredClock(time.Now())
	session1 := parsers.CashSession{Date: now, Stake: "NL5", BuyInCents: 500, NetCents: -600, Hands: 40, FirstHandID: "h1"}
	if err := store.AppendCashSession(session1); err != nil {
		t.Fatalf("AppendCashSession(session1): %v", err)
	}

	ledger, dayTournaments := fetchEvalData(store, cfg, time.Now(), false)
	bankrollCents := ledgerBankroll(ledger)
	if bankrollCents != 9400 {
		t.Fatalf("bankroll after session 1 = %v, want 94", bankrollCents)
	}
	bot.EvaluateSessionLossAlert(session1, cfg, func() money.Cents { return bankrollCents }, alertChan)
	evaluateOncePerKeyAlerts(cfg, ledger, dayTournaments, bankrollCents, time.Now(), alertChan, firedAlerts, "cash:h1")
	assertMsgs("session 1", []string{
		"⚠️ Session loss alert\nLost 1.2 buy-ins this session ($6.00)\nBankroll: $94.00", "⚠️ Daily loss alert\nDown $6.00 across all games this poker day\nBankroll: $94.00"})

	session2 := parsers.CashSession{Date: now.Add(time.Minute), Stake: "NL5", BuyInCents: 500, NetCents: -400, Hands: 30, FirstHandID: "h2"}
	if err := store.AppendCashSession(session2); err != nil {
		t.Fatalf("AppendCashSession(session2): %v", err)
	}

	ledger, dayTournaments = fetchEvalData(store, cfg, time.Now(), false)
	bankrollCents = ledgerBankroll(ledger)
	if bankrollCents != 9000 {
		t.Fatalf("bankroll after session 2 = %v, want 90", bankrollCents)
	}
	bot.EvaluateSessionLossAlert(session2, cfg, func() money.Cents { return bankrollCents }, alertChan)
	evaluateOncePerKeyAlerts(cfg, ledger, dayTournaments, bankrollCents, time.Now(), alertChan, firedAlerts, "cash:h2")
	assertMsgs("session 2", []string{
		"⚠️ Losing streak alert\n2 losing sessions in a row\nBankroll: $90.00"})
}

func TestTournamentAlertText(t *testing.T) {
	mtt := func(finish, players, reEntries int, buyInCents money.Cents) parsers.Tournament {
		return parsers.Tournament{Type: "mtt", BuyInCents: buyInCents, FinishPos: finish, TotalPlayers: players, ReEntries: reEntries}
	}
	cases := []struct {
		name      string
		t         parsers.Tournament
		netCents  money.Cents
		bankCents money.Cents
		wantHas   string
		reEntry   bool
	}{
		{
			name: "plain MTT has no re-entry line", t: mtt(50, 126, 0, 110), netCents: -110, bankCents: 2200,
			reEntry: false,
		},
		{
			name: "re-entry MTT names the entry count", t: mtt(43, 162, 2, 330), netCents: -330, bankCents: 1980,
			wantHas: "🔁 Re-entered 2× — 3 entries total", reEntry: true,
		},
	}
	mult := 2.0
	spingoCases := []struct {
		name      string
		t         parsers.Tournament
		netCents  money.Cents
		bankCents money.Cents
		want      string
	}{
		{
			name: "spingo with a real cash pool",
			t: parsers.Tournament{
				Type: "spingo", BuyInCents: 50, PrizePoolCents: 100,
				Multiplier: &mult, FinishPos: 1, TotalPlayers: 3,
			},
			netCents: 50, bankCents: 2358,
			want: "🎰 Spin & Go\nBuy-in: $0.50\nPrize pool: $1.00 (2x)\nFinish: 1st\nNet: +$0.50\nBankroll: $23.58"},
		{
			name: "spingo with a ticket-denominated pool (nil multiplier)",
			t: parsers.Tournament{
				Type: "spingo", BuyInCents: 50,
				PrizePoolDescription: "$1.00 Power Path Step 2 Ticket",
				FinishPos:            3, TotalPlayers: 3,
			},
			netCents: -50, bankCents: 2308,
			want: "🎰 Spin & Go\nBuy-in: $0.50\nPrize pool: $1.00 Power Path Step 2 Ticket\nFinish: 3rd\nNet: -$0.50\nBankroll: $23.08"},
		{
			name: "knockout spingo re-entry keeps the spingo format plus the re-entry line",
			t: parsers.Tournament{
				Type: "spingo-ko", BuyInCents: 100, PrizePoolCents: 200,
				Multiplier: &mult, FinishPos: 2, TotalPlayers: 3, ReEntries: 1,
			},
			netCents: -100, bankCents: 2208,
			want: "🎰 Spin & Go\nBuy-in: $1.00\nPrize pool: $2.00 (2x)\nFinish: 2nd\nNet: -$1.00\nBankroll: $22.08\n🔁 Re-entered 1× — 2 entries total for this tournament; buy-in and net cover all of them, updating the earlier alert."},
	}
	for _, c := range spingoCases {
		t.Run(c.name, func(t *testing.T) {
			if got := tournamentAlertText(c.t, c.netCents, c.bankCents); got != c.want {
				t.Errorf("tournamentAlertText() =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tournamentAlertText(c.t, c.netCents, c.bankCents)
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
	srv.QuietPeriod = 50 * time.Millisecond
	done := make(chan struct{})
	go func() { srv.Run(); close(done) }()
	t.Cleanup(func() {
		close(link.Uploads)
		<-done
		store.Close()
	})
	return store, link, alertChan
}

func importUp(t *testing.T, fileID, typ, sub, name string) transport.Upload {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", sub, name))
	if err != nil {
		t.Fatal(err)
	}
	return transport.Upload{FileID: fileID, Type: typ, Offset: 0, Bytes: b, ScreenName: importHero, ClientClock: time.Now(), Import: true}
}

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
	mtt, found, _ := store.GetTournamentByID("9000000003")
	if !found {
		t.Error("MTT 9000000003 not recorded, want recorded")
	}
	if _, found, _ := store.GetTournamentByID("9000000013"); found {
		t.Error("satellite 9000000013 recorded, want skipped")
	}

	if gotCents := sessions[0].NetCents; !closeEnoughServer(gotCents, -3) {
		t.Errorf("imported cash session net = %v, want -0.03", gotCents)
	}
	if gotCents := mtt.NetCents; !closeEnoughServer(gotCents, -110) {
		t.Errorf("imported MTT net = %v, want -1.10", gotCents)
	}
	bankrollCents, err := store.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if !closeEnoughServer(bankrollCents, -113) {
		t.Errorf("bankroll after import = %v, want -1.13", bankrollCents)
	}
}

func TestServer_ImportKnockoutTwoFile(t *testing.T) {
	store, link, alertChan := startCapturingServer(t)
	report := feedImport(t, link, alertChan,
		importUp(t, "hh/pko.txt", "cash", "hand_history", "draw27_pko_T9000000005.txt"),
		importUp(t, "ts/pko.txt", "tournament", "tournament_summary", "draw27_pko_T9000000005.txt"),
	)
	if !strings.Contains(report, "recorded 0 cash session(s) and 1 tournament(s)") {
		t.Errorf("report = %q, want 0 cash / 1 tournament", report)
	}
	got, found, err := store.GetTournamentByID("9000000005")
	if err != nil || !found {
		t.Fatalf("KO tournament not found (err %v)", err)
	}
	if got.BaseType() != "mtt" || !strings.HasSuffix(got.Type, "-ko") {
		t.Errorf("Type = %q, want an mtt -ko (proves hand+summary correlation)", got.Type)
	}
	if n := countTournamentRows(t, store, "9000000005"); n != 1 {
		t.Errorf("%d rows for the KO tournament, want exactly 1 (counted once)", n)
	}
}

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

func TestServer_ImportFreerollHandFileLeavesNoMoney(t *testing.T) {
	store, link, alertChan := startCapturingServer(t)
	report := feedImport(t, link, alertChan,
		importUp(t, "hh/freeroll.txt", "cash", "hand_history", "freeroll_zero_T9000000002.txt"),
		importUp(t, "ts/freeroll.txt", "tournament", "tournament_summary", "freeroll_zero_T9000000002.txt"),
	)
	if !strings.Contains(report, "recorded 0 cash session(s) and 0 tournament(s)") || !strings.Contains(report, "Skipped 1") {
		t.Errorf("report = %q, want 0/0 recorded and 1 skipped", report)
	}
	if _, found, _ := store.GetTournamentByID("9000000002"); found {
		t.Error("stats-only stub reported as a recorded tournament by GetTournamentByID, want not-found")
	}
	snap, err := store.ExportSnapshot()
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	var stub *storage.TournamentRecord
	for i := range snap.Tournaments {
		if snap.Tournaments[i].TournamentID == "9000000002" {
			stub = &snap.Tournaments[i]
		}
	}
	if stub == nil {
		t.Fatal("partial tournament row not created by the hand file, want a stats-only stub")
	}
	if stub.NetCents != 0 || stub.BuyInCents != 0 {
		t.Errorf("partial row carries money: net=%v buy-in=%v, want both 0", stub.NetCents, stub.BuyInCents)
	}
	bankrollCents, err := store.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if bankrollCents != 0 {
		t.Errorf("bankroll = %v after a skipped freeroll, want 0 (no ledger impact)", bankrollCents)
	}
}

func TestServer_ImportIdempotent(t *testing.T) {
	store, link, alertChan := startCapturingServer(t)
	ups := []transport.Upload{
		importUp(t, "hh/nlhe_cash.txt", "cash", "hand_history", "nlhe_cash.txt"),
		importUp(t, "ts/horse.txt", "tournament", "tournament_summary", "horse_T9000000003.txt"),
	}
	first := feedImport(t, link, alertChan, ups...)
	bankroll1Cents, _ := store.GetCurrentBankroll()
	second := feedImport(t, link, alertChan, ups...)
	bankroll2Cents, _ := store.GetCurrentBankroll()

	if !strings.Contains(first, "4 new, 0 already recorded") || !strings.Contains(second, "0 new, 4 already recorded") {
		t.Errorf("unexpected scan reports:\n first: %q\nsecond: %q", first, second)
	}
	if !closeEnoughServer(bankroll1Cents, bankroll2Cents) {
		t.Errorf("bankroll drifted on re-scan: %v → %v", bankroll1Cents, bankroll2Cents)
	}
	sessions, _ := store.GetAllCashSessions()
	if len(sessions) != 1 {
		t.Errorf("%d cash sessions after two scans, want 1 (no duplicate)", len(sessions))
	}
	if n := countTournamentRows(t, store, "9000000003"); n != 1 {
		t.Errorf("%d rows for the MTT after two scans, want 1 (no duplicate)", n)
	}
}

func closeEnoughServer(aCents, bCents money.Cents) bool { return aCents == bCents }

func waitForCommand(t *testing.T, ch <-chan transport.Command, timeout time.Duration) (transport.Command, bool) {
	t.Helper()
	select {
	case c := <-ch:
		return c, true
	case <-time.After(timeout):
		return transport.Command{}, false
	}
}

func TestServer_SessionEndLedgerHardStop(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.NewStore(filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	n, hours := 2, 1
	configPath := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(configPath, &config.Config{
		Hardstop: config.Hardstop{ConsecutiveLosses: &n, DurationHours: &hours},
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	prev := parsers.CashSession{
		Date:  storage.AsStoredClock(time.Now()).Add(-time.Hour),
		Stake: "NL2", BuyInCents: 200, NetCents: -150, Hands: 20, DurationMin: 15, FirstHandID: "prevloss1",
	}
	if err := store.AppendCashSession(prev); err != nil {
		t.Fatalf("pre-seeding losing session: %v", err)
	}

	link := transport.NewLink()
	alertChan := make(chan string, 100)
	srv, err := NewServer(store, configPath, link, alertChan)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.QuietPeriod = 50 * time.Millisecond
	done := make(chan struct{})
	go func() { srv.Run(); close(done) }()
	t.Cleanup(func() {
		close(link.Uploads)
		<-done
		store.Close()
	})
	go func() {
		for range alertChan {
		}
	}()

	cashBytes, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	link.Uploads <- transport.Upload{FileID: "hh/nlhe.txt", Type: "cash", Offset: 0, Bytes: cashBytes, ScreenName: importHero, ClientClock: time.Now()}

	cmd, ok := waitForCommand(t, link.Commands, 5*time.Second)
	if !ok {
		t.Fatal("no lock command at session end — the ledger-based hard stop didn't fire")
	}
	if cmd.Kind != transport.CommandLock || cmd.Lock == nil || cmd.Lock.Reason != "consecutive_losses" {
		t.Errorf("lock command = %+v, want a consecutive_losses lock", cmd)
	}
}

func TestServer_LedgerHardStopDeferredNotPerHand(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.NewStore(filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	n, hours := 2, 1
	configPath := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(configPath, &config.Config{
		Hardstop: config.Hardstop{ConsecutiveLosses: &n, DurationHours: &hours},
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	for i, hid := range []string{"prevloss1", "prevloss2"} {
		prev := parsers.CashSession{
			Date:  storage.AsStoredClock(time.Now()).Add(-time.Duration(2-i) * time.Hour),
			Stake: "NL2", BuyInCents: 200, NetCents: -150, Hands: 20, DurationMin: 15, FirstHandID: hid,
		}
		if err := store.AppendCashSession(prev); err != nil {
			t.Fatalf("pre-seeding losing session %s: %v", hid, err)
		}
	}

	link := transport.NewLink()
	alertChan := make(chan string, 100)
	srv, err := NewServer(store, configPath, link, alertChan)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.QuietPeriod = 500 * time.Millisecond
	done := make(chan struct{})
	go func() { srv.Run(); close(done) }()
	t.Cleanup(func() {
		close(link.Uploads)
		<-done
		store.Close()
	})
	go func() {
		for range alertChan {
		}
	}()

	cashBytes, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	link.Uploads <- transport.Upload{FileID: "hh/nlhe.txt", Type: "cash", Offset: 0, Bytes: cashBytes, ScreenName: importHero, ClientClock: time.Now()}

	if cmd, ok := waitForCommand(t, link.Commands, 200*time.Millisecond); ok {
		t.Fatalf("hard stop fired before session end — the ledger check ran per hand: %+v", cmd)
	}
	cmd, ok := waitForCommand(t, link.Commands, 5*time.Second)
	if !ok {
		t.Fatal("no lock command at session end — the ledger-based hard stop didn't fire")
	}
	if cmd.Kind != transport.CommandLock || cmd.Lock == nil || cmd.Lock.Reason != "consecutive_losses" {
		t.Errorf("lock command = %+v, want a consecutive_losses lock", cmd)
	}
	if extra, ok := waitForCommand(t, link.Commands, 300*time.Millisecond); ok {
		t.Errorf("a second lock command fired, want exactly one: %+v", extra)
	}
}

func TestArmCashSessionEnd_FiresAfterQuietPeriod(t *testing.T) {
	timers := make(map[string]*quietTimer)
	sessionEnd := make(chan quietSignal, 10)

	armQuietTimer(timers, "table-a.txt", 50*time.Millisecond, sessionEnd, nil)

	got, ok := waitForCashSignal(t, sessionEnd, 500*time.Millisecond)
	if !ok {
		t.Fatal("expected a session-end signal after the quiet period, got none")
	}
	if got != "table-a.txt" {
		t.Errorf("got %q, want %q", got, "table-a.txt")
	}
}

func TestArmCashSessionEnd_ResetOnNewHand(t *testing.T) {
	timers := make(map[string]*quietTimer)
	sessionEnd := make(chan quietSignal, 10)

	armQuietTimer(timers, "table-a.txt", 100*time.Millisecond, sessionEnd, nil)
	time.Sleep(30 * time.Millisecond)
	armQuietTimer(timers, "table-a.txt", 100*time.Millisecond, sessionEnd, nil)

	got, ok := waitForCashSignal(t, sessionEnd, 500*time.Millisecond)
	if !ok {
		t.Fatal("expected one session-end after the quiet period, got none")
	}
	if got != "table-a.txt" {
		t.Errorf("got %q, want %q", got, "table-a.txt")
	}
	if _, ok := waitForCashSignal(t, sessionEnd, 200*time.Millisecond); ok {
		t.Error("expected only one session-end for a table that kept getting new hands, got a second")
	}
}

func TestArmCashSessionEnd_IndependentPerTable(t *testing.T) {
	timers := make(map[string]*quietTimer)
	sessionEnd := make(chan quietSignal, 10)

	armQuietTimer(timers, "table-a.txt", 50*time.Millisecond, sessionEnd, nil)
	armQuietTimer(timers, "table-b.txt", 50*time.Millisecond, sessionEnd, nil)

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		got, ok := waitForCashSignal(t, sessionEnd, 500*time.Millisecond)
		if !ok {
			t.Fatal("expected two independent session-end signals, got fewer")
		}
		seen[got] = true
	}
	if !seen["table-a.txt"] || !seen["table-b.txt"] {
		t.Errorf("expected session-end from both tables, got %v", seen)
	}
}

func TestEndCashSession_PrunesTimer(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.NewStore(filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	alertChan := make(chan string, 10)
	srv, err := NewServer(store, filepath.Join(dir, "config.json"), transport.NewLink(), alertChan)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	const fileID = `C:\hh\table.txt`
	summaryTimers := make(map[string]*quietTimer)
	sessionEnd := make(chan quietSignal, 1)
	armQuietTimer(summaryTimers, fileID, time.Hour, sessionEnd, nil)
	timer := summaryTimers[fileID]

	pendingSessions := map[string]parsers.CashSession{
		fileID: {Date: storage.AsStoredClock(time.Now()), Stake: "NL2", BuyInCents: 200, NetCents: -100, Hands: 10, DurationMin: 8, FirstHandID: "h1"},
	}
	firedAlerts := make(map[string]bool)
	srv.endCashSession(fileID, "", pendingSessions, summaryTimers, firedAlerts, func(string, int, string) {})

	if _, ok := summaryTimers[fileID]; ok {
		t.Error("summaryTimers entry not pruned — the per-table timer leaks")
	}
	if _, ok := pendingSessions[fileID]; ok {
		t.Error("pendingSessions entry not removed")
	}
	if timer.Stop() {
		t.Error("endCashSession did not stop the timer before dropping it — a superseded timer could fire a stale send")
	}
	sessions, err := store.GetAllCashSessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("GetAllCashSessions = %d rows (err %v), want the one written session", len(sessions), err)
	}
}

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
	srv.QuietPeriod = 50 * time.Millisecond
	done := make(chan struct{})
	go func() { srv.Run(); close(done) }()
	go func() {
		for range alertChan {
		}
	}()
	t.Cleanup(func() {
		close(link.Uploads)
		<-done
		close(alertChan)
		store.Close()
	})
	return store, link, srv
}

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

func TestServer_ClientClockAnchoring(t *testing.T) {
	_, _, srv := startTestServer(t)

	if loc := srv.ClientNow().Location(); loc != time.Local {
		t.Fatalf("pre-upload ClientNow location = %v, want the server's time.Local fallback", loc)
	}

	_, hostOff := time.Now().Zone()
	collectorZone := time.FixedZone("COLLECTOR", hostOff+5*60*60)
	srv.updateClock(time.Now().In(collectorZone))

	cn := srv.ClientNow()
	if cn.Location() != collectorZone {
		t.Fatalf("ClientNow location = %v, want the collector's zone", cn.Location())
	}
	stored := storage.AsStoredClock(cn)
	want := time.Date(cn.Year(), cn.Month(), cn.Day(), cn.Hour(), cn.Minute(), cn.Second(), cn.Nanosecond(), time.UTC)
	if !stored.Equal(want) {
		t.Fatalf("AsStoredClock(ClientNow) = %v, want the collector's wall clock %v — anchoring re-localized to the server zone", stored, want)
	}
}

func TestServer_StreamingChunksMatchWholeFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}

	wholeStore, wholeLink, _ := startTestServer(t)
	wholeLink.Uploads <- transport.Upload{FileID: `C:\hh\whole.txt`, Type: "cash", Offset: 0, Bytes: data, ScreenName: "AnonHero01"}
	want := waitForCashSession(t, wholeStore, 4)

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

	got.SessionID = ""
	want.SessionID = ""
	got.HandDetails, want.HandDetails = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("streamed session != whole-file session:\n streamed=%+v\n whole   =%+v", got, want)
	}
}

func TestServer_RewriteSelfHealsThroughServer(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	offs := handHeaderOffsetsBytes(data)
	if len(offs) < 3 {
		t.Fatalf("need >=3 hands, got %d", len(offs))
	}
	twoHands := data[:offs[2]]

	store, link, _ := startTestServer(t)
	const fileID = `C:\hh\rewrite.txt`
	link.Uploads <- transport.Upload{FileID: fileID, Type: "cash", Offset: 0, Bytes: data, ScreenName: "AnonHero01"}
	waitForCashSession(t, store, 4)

	link.Uploads <- transport.Upload{FileID: fileID, Type: "cash", Offset: 0, Bytes: twoHands, ScreenName: "AnonHero01"}
	got := waitForCashSession(t, store, 2)

	tmp := filepath.Join(t.TempDir(), "two.txt")
	if err := os.WriteFile(tmp, twoHands, 0o644); err != nil {
		t.Fatal(err)
	}
	want, _, err := parsers.ParseCashFile(tmp, "AnonHero01")
	if err != nil {
		t.Fatalf("reference parse: %v", err)
	}
	if got.Hands != want.Hands || got.NetCents != want.NetCents {
		t.Fatalf("rewrite not self-healed: stored {hands:%d net:%v} want {hands:%d net:%v}",
			got.Hands, got.NetCents, want.Hands, want.NetCents)
	}
	if got.SessionID == "" {
		t.Fatal("self-healed session has no Session ID persisted")
	}
}

func handHeaderOffsetsBytes(data []byte) []int64 {
	var offs []int64
	var off int64
	for _, raw := range splitKeepNL(data) {
		line := strings.TrimRight(string(raw), "\n")
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

func waitForCashSignal(t *testing.T, ch <-chan quietSignal, wait time.Duration) (string, bool) {
	t.Helper()
	select {
	case signal := <-ch:
		return signal.Key, true
	case <-time.After(wait):
		return "", false
	}
}
