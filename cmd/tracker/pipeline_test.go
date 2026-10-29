package main

// End-to-end scenario suite: drives the REAL pipeline — the real fsnotify
// watcher, the real runWorker, and a real sqlite store — with committed
// anonymized fixture files, and asserts the real stored state (bankroll,
// ledger, tournament rows), the alert strings the bot would send (drained off
// alertChan), and pending_tournaments.json. No mocks, no skips.
//
// The only manual boundary is the Telegram *send*: the bot goroutine isn't
// started, so alerts are asserted as the strings runWorker emits. The freeroll
// *answer* (🆓/💵 tap) likewise runs in the bot and is covered by
// sqlitestore.TestFreeroll_BuyInAnswer; here the freeroll scenario asserts up
// to "queued + bounty stub written".
//
// Time-windowed triggers (rolling-30-day downswing, poker-day daily loss,
// losing streak) depend on time.Now() vs each fixture's fixed historical date,
// so they'd be date-fragile here — they stay covered by the hardstop/alerts
// unit tests, which build ledgers with controlled dates. The session-loss hard
// stop below is date-agnostic (a single session's net vs a threshold), so it
// is exercised end-to-end.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/client"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/server"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

const e2eHero = "AnonHero01"

type pipeline struct {
	t            *testing.T
	dir          string
	hhDir, tsDir string
	lockfilePath string
	store        *sqlitestore.Store

	mu     sync.Mutex
	alerts []string
}

// newPipeline starts the real watcher + worker over temp dirs, backed by a real
// sqlite store and the given config (screen name defaulted to the fixtures'
// hero). Alerts are collected off alertChan.
func newPipeline(t *testing.T, cfg *config.Config) *pipeline {
	t.Helper()
	dir := t.TempDir()
	hhDir := filepath.Join(dir, "hand_history")
	tsDir := filepath.Join(dir, "tournament_summary")
	for _, d := range []string{hhDir, tsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, "config.json")
	lockfilePath := filepath.Join(dir, "lockfile.json")
	dbPath := filepath.Join(dir, "tracker.db")

	if cfg.Setup.PokerStarsScreenName == nil {
		hero := e2eHero
		cfg.Setup.PokerStarsScreenName = &hero
	}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	sessionChan := make(chan watcher.FileEvent, 10)
	alertChan := make(chan string, 100)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	p := &pipeline{t: t, dir: dir, hhDir: hhDir, tsDir: tsDir, lockfilePath: lockfilePath, store: store}

	go func() {
		if err := watcher.Watch(done, hhDir, tsDir, sessionChan); err != nil {
			// Watch returns when done closes; a real construction error is worth surfacing.
			t.Logf("watcher stopped: %v", err)
		}
	}()

	// Drive the real split pipeline: watcher -> collector -> in-process link ->
	// server -> store, with the server's lock commands flowing back to the
	// collector's enforcement (which writes the lockfile the hard-stop scenario
	// asserts). The Telegram send is still the only manual boundary.
	link := transport.NewLink()
	srv, err := server.NewServer(store, configPath, link, alertChan)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Cash sessions are written once the table goes quiet; shorten that window
	// so the session-end write/eval/summary fires within the test's deadline.
	srv.QuietPeriod = 150 * time.Millisecond
	go srv.Run()
	collector := client.NewCollector(link, lockfilePath, nil)
	go collector.Run(sessionChan, configPath, nil)
	go client.RunEnforcementLoop(lockfilePath, done)

	go func() {
		for m := range alertChan {
			p.mu.Lock()
			p.alerts = append(p.alerts, m)
			p.mu.Unlock()
		}
	}()

	// Let fsnotify register the watches before any fixture is dropped.
	time.Sleep(200 * time.Millisecond)
	return p
}

// drop copies a committed fixture into the matching watched folder, which the
// real watcher then picks up. sub is "hand_history" or "tournament_summary".
func (p *pipeline) drop(sub, name string) {
	p.t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", sub, name))
	if err != nil {
		p.t.Fatal(err)
	}
	dst := p.tsDir
	if sub == "hand_history" {
		dst = p.hhDir
	}
	if err := os.WriteFile(filepath.Join(dst, name), data, 0644); err != nil {
		p.t.Fatal(err)
	}
}

// waitFor polls cond until it holds or a generous deadline (covering the
// watcher's 2s debounce plus processing) elapses.
func (p *pipeline) waitFor(what string, cond func() bool) {
	p.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.t.Fatalf("timed out waiting for: %s", what)
}

func (p *pipeline) bankroll() float64 {
	b, err := p.store.GetCurrentBankroll()
	if err != nil {
		p.t.Fatalf("GetCurrentBankroll: %v", err)
	}
	return b
}

func (p *pipeline) pending() []config.PendingTournament {
	pt, err := config.LoadPendingTournaments(filepath.Join(p.dir, "pending_tournaments.json"))
	if err != nil {
		p.t.Fatalf("LoadPendingTournaments: %v", err)
	}
	return pt
}

func (p *pipeline) alertSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.alerts))
	copy(out, p.alerts)
	return out
}

func (p *pipeline) countAlerts(substr string) int {
	n := 0
	for _, a := range p.alertSnapshot() {
		if strings.Contains(a, substr) {
			n++
		}
	}
	return n
}

// bountyStub returns the recorded bounty for a tournament ID via ExportSnapshot
// (which, unlike GetAllTournaments, includes the date-less partial stub row a
// hand-history-first tournament produces).
func (p *pipeline) bountyStub(tournamentID string) (float64, bool) {
	snap, err := p.store.ExportSnapshot()
	if err != nil {
		p.t.Fatalf("ExportSnapshot: %v", err)
	}
	for _, r := range snap.Tournaments {
		if r.TournamentID == tournamentID {
			return r.BountyWonUSD, true
		}
	}
	return 0, false
}

func closeEnough(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 0.005
}

// TestPipeline_WonFreeroll: the hand file writes the knockout-bounty stub
// ($0.88), and the summary (no buy-in) queues the tournament for the entry-cost
// question rather than recording it — no ledger movement, no alert, until the
// player answers.
func TestPipeline_WonFreeroll(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("hand_history", "freeroll_won_T9000000001.txt")
	p.waitFor("bounty stub written", func() bool {
		b, ok := p.bountyStub("9000000001")
		return ok && closeEnough(b, 0.88)
	})
	p.drop("tournament_summary", "freeroll_won_T9000000001.txt")
	p.waitFor("freeroll queued", func() bool { return len(p.pending()) == 1 })

	pend := p.pending()
	if pend[0].Reason != config.PendingFreerollBuyin {
		t.Errorf("Reason = %q, want %q", pend[0].Reason, config.PendingFreerollBuyin)
	}
	if pend[0].Tournament.TournamentID != "9000000001" {
		t.Errorf("queued TournamentID = %q, want 9000000001", pend[0].Tournament.TournamentID)
	}
	if !closeEnough(p.bankroll(), 0) {
		t.Errorf("bankroll = %.2f, want 0 (deferred until the player answers)", p.bankroll())
	}
	if n := p.countAlerts("Freeroll"); n != 0 {
		t.Errorf("got %d freeroll alerts from the worker; the bot ticker asks, not the worker", n)
	}
}

// TestPipeline_CashSession: a cash hand history flows through to a recorded
// session and its ledger entry once the table goes quiet (the session is now
// written at session end, not per hand); bankroll equals the session net.
func TestPipeline_CashSession(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("hand_history", "nlhe_cash.txt")
	// Nothing is stored until the quiet period elapses; then the whole session
	// lands in one write. Poll the final invariant: one session row exists and
	// bankroll equals its net.
	p.waitFor("session recorded at session end", func() bool {
		s, err := p.store.GetAllCashSessions()
		return err == nil && len(s) == 1 && closeEnough(p.bankroll(), s[0].NetUSD)
	})
	sessions, _ := p.store.GetAllCashSessions()
	if sessions[0].Stake != "NL2" {
		t.Errorf("stake = %q, want NL2", sessions[0].Stake)
	}
	// The post-session summary is sent once, at session end.
	p.waitFor("cash session summary", func() bool { return p.countAlerts("✅ Cash Session") >= 1 })
	if n := p.countAlerts("✅ Cash Session"); n != 1 {
		t.Errorf("got %d cash session summaries, want 1; alerts=%v", n, p.alertSnapshot())
	}
}

// TestPipeline_SpinGoWin: a won Spin & Go summary (real buy-in, not a freeroll)
// records immediately — net +$10 to the ledger and a "🎰 Spin & Go" alert.
func TestPipeline_SpinGoWin(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("tournament_summary", "spingo_won_T9000000007.txt")
	p.waitFor("spin & go recorded", func() bool { return closeEnough(p.bankroll(), 10.00) })

	// The alert is emitted just after the store write, on the worker goroutine,
	// then appended by the collector — wait for it rather than racing it.
	p.waitFor("spin & go alert", func() bool { return p.countAlerts("🎰 Spin & Go") >= 1 })
	if n := p.countAlerts("🎰 Spin & Go"); n != 1 {
		t.Errorf("got %d Spin & Go alerts, want 1; alerts=%v", n, p.alertSnapshot())
	}
	got, found, _ := p.store.GetTournamentByID("9000000007")
	if !found || got.BaseType() != "spingo" {
		t.Errorf("tournament row = (found %v, type %q), want spingo", found, got.Type)
	}
}

// TestPipeline_MTTLoss: a busted MTT summary records a losing tournament and a
// "🏆 MTT" alert.
func TestPipeline_MTTLoss(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("tournament_summary", "horse_T9000000003.txt")
	p.waitFor("mtt loss recorded", func() bool { return closeEnough(p.bankroll(), -1.10) })

	p.waitFor("mtt alert", func() bool { return p.countAlerts("🏆 MTT") >= 1 })
	if n := p.countAlerts("🏆 MTT"); n != 1 {
		t.Errorf("got %d MTT alerts, want 1; alerts=%v", n, p.alertSnapshot())
	}
}

// TestPipeline_SatelliteQueued: a ticket-denominated satellite summary queues
// for the cash-vs-ticket question (reason satellite), not recorded, no ledger.
func TestPipeline_SatelliteQueued(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("tournament_summary", "satellite_busted_T9000000013.txt")
	p.waitFor("satellite queued", func() bool { return len(p.pending()) == 1 })

	if p.pending()[0].Reason != config.PendingSatellite {
		t.Errorf("Reason = %q, want %q", p.pending()[0].Reason, config.PendingSatellite)
	}
	if !closeEnough(p.bankroll(), 0) {
		t.Errorf("bankroll = %.2f, want 0 (deferred)", p.bankroll())
	}
}

// TestPipeline_DuplicateEvent: dropping the same summary twice records the
// tournament once — one ledger movement, one alert.
func TestPipeline_DuplicateEvent(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("tournament_summary", "spingo_won_T9000000007.txt")
	p.waitFor("first record", func() bool { return closeEnough(p.bankroll(), 10.00) })

	// Re-drop the identical file; the debounce fires a fresh event that must be
	// recognized as a duplicate and skipped.
	p.drop("tournament_summary", "spingo_won_T9000000007.txt")
	time.Sleep(4 * time.Second) // 2s debounce + processing margin

	if !closeEnough(p.bankroll(), 10.00) {
		t.Errorf("bankroll = %.2f, want 10.00 (duplicate must not double-count)", p.bankroll())
	}
	if n := p.countAlerts("🎰 Spin & Go"); n != 1 {
		t.Errorf("got %d Spin & Go alerts, want 1 (no second alert for a duplicate)", n)
	}
}

// TestPipeline_TwoFileCorrelationKnockout: a knockout MTT's summary and hand
// history correlate into one row that carries the "-ko" type once the hand file
// flags the bounty markers, in either arrival order (summary-first here).
func TestPipeline_TwoFileCorrelationKnockout(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("tournament_summary", "draw27_pko_T9000000005.txt")
	p.waitFor("mtt recorded", func() bool { return closeEnough(p.bankroll(), -3.30) })
	p.drop("hand_history", "draw27_pko_T9000000005.txt")
	p.waitFor("knockout type reconciled", func() bool {
		got, found, _ := p.store.GetTournamentByID("9000000005")
		return found && strings.HasSuffix(got.Type, "-ko")
	})
	got, _, _ := p.store.GetTournamentByID("9000000005")
	if got.BaseType() != "mtt" {
		t.Errorf("BaseType = %q, want mtt", got.BaseType())
	}
}

// TestPipeline_HardStopSessionLoss: a cash session whose loss exceeds the
// session-loss threshold trips the hard stop — a lockfile is written and a
// "🛑 Hard Stop triggered" alert is sent. The threshold is set intentionally
// tiny so the small committed fixture loss ($0.03) crosses it; session loss is
// date-agnostic, so this runs end-to-end without a now-relative window.
func TestPipeline_HardStopSessionLoss(t *testing.T) {
	buyins := 0.01 // threshold = 0.01 buy-ins * $2 (NL2) = $0.02, below the $0.03 loss
	hours := 1
	cfg := &config.Config{Hardstop: config.Hardstop{SessionLossBuyins: &buyins, DurationHours: &hours}}
	p := newPipeline(t, cfg)
	p.drop("hand_history", "nlhe_cash.txt")
	p.waitFor("hard stop lockfile written", func() bool {
		lock, err := config.LoadLock(p.lockfilePath)
		return err == nil && lock != nil
	})

	lock, _ := config.LoadLock(p.lockfilePath)
	if lock.Reason != "session_loss_buyins" {
		t.Errorf("lock reason = %q, want session_loss_buyins", lock.Reason)
	}
	p.waitFor("hard stop alert", func() bool { return p.countAlerts("🛑 Hard Stop triggered") >= 1 })
	if n := p.countAlerts("🛑 Hard Stop triggered"); n != 1 {
		t.Errorf("got %d hard-stop alerts, want 1; alerts=%v", n, p.alertSnapshot())
	}
}

// TestPipeline_IncrementalGrowingFile drives a hand-history file that grows
// across two separate watcher events — the real per-hand append pattern — so it
// exercises runWorker's incremental parse-state map end-to-end: event 1 is
// parsed with no prior state and (once the table falls quiet) records the first
// hands, event 2 appends more and is parsed with the carried state (resume from
// a non-zero byte offset), then re-records the grown session. The final recorded
// session must equal a whole-file parse regardless, which also guards the
// self-healing fallback: a wiring bug can't corrupt the data, only lose the
// speedup, and this asserts the data is right.
func TestPipeline_IncrementalGrowingFile(t *testing.T) {
	full, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Split after the first two hands so event 2 resumes from offset > 0.
	sep := "PokerStars Hand #"
	text := string(full)
	var offs []int
	first := true // the file's first header may sit just past a BOM, not a newline
	for start := 0; ; {
		idx := strings.Index(text[start:], sep)
		if idx < 0 {
			break
		}
		abs := start + idx
		if first || text[abs-1] == '\n' {
			offs = append(offs, abs)
			first = false
		}
		start = abs + len(sep)
	}
	if len(offs) < 3 {
		t.Fatalf("need >=3 hands in nlhe_cash.txt, found %d", len(offs))
	}
	prefix := full[:offs[2]] // hands 1..2

	p := newPipeline(t, &config.Config{})

	p.writeHH("nlhe_cash.txt", prefix)
	p.waitFor("first two hands recorded", func() bool {
		s, err := p.store.GetAllCashSessions()
		return err == nil && len(s) == 1 && s[0].Hands == 2
	})

	p.writeHH("nlhe_cash.txt", full)
	p.waitFor("all four hands recorded, bankroll consistent", func() bool {
		s, err := p.store.GetAllCashSessions()
		return err == nil && len(s) == 1 && s[0].Hands == 4 && closeEnough(p.bankroll(), s[0].NetUSD)
	})

	// The grown-across-two-events session must match a whole-file parse.
	want, _, err := parsers.ParseCashFile(filepath.Join(p.hhDir, "nlhe_cash.txt"), e2eHero)
	if err != nil {
		t.Fatalf("whole-file parse: %v", err)
	}
	got, _ := p.store.GetAllCashSessions()
	if got[0].Hands != want.Hands || !closeEnough(got[0].NetUSD, want.NetUSD) || !closeEnough(got[0].RakeUSD, want.RakeUSD) {
		t.Errorf("grown session = {hands %d, net %.4f, rake %.4f}, whole-file = {hands %d, net %.4f, rake %.4f}",
			got[0].Hands, got[0].NetUSD, got[0].RakeUSD, want.Hands, want.NetUSD, want.RakeUSD)
	}
}

// writeHH creates or grows a hand-history file in the watched folder; the real
// watcher then debounces and delivers it, exactly as PokerStars appending a hand
// would.
func (p *pipeline) writeHH(name string, data []byte) {
	p.t.Helper()
	if err := os.WriteFile(filepath.Join(p.hhDir, name), data, 0644); err != nil {
		p.t.Fatal(err)
	}
}

// TestPipeline_ScanBackfill drives the whole /scan path end to end — a real
// collector walking real folders → the in-process link → a real server → a real
// sqlite store — triggered exactly as the bot would, by a CommandRescan down the
// link. It seeds the two folders (screen-name-nested, as PokerStars nests them)
// with a cash session, a plain MTT, a satellite, and a knockout tournament whose
// hand file AND summary are both present, then asserts the silent import
// recorded the right rows (knockout correlated to one -ko row), skipped the
// satellite, and reported the right counts.
func TestPipeline_ScanBackfill(t *testing.T) {
	const hero = e2eHero
	dir := t.TempDir()
	hhBase := filepath.Join(dir, "hh")
	tsBase := filepath.Join(dir, "ts")
	hhDir := filepath.Join(hhBase, hero)
	tsDir := filepath.Join(tsBase, hero)
	for _, d := range []string{hhDir, tsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	copyFixture := func(sub, name, dst string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", sub, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Cash + the knockout's hand file in the hand-history folder; the plain MTT,
	// the satellite, and the knockout's summary in the tournament-summary folder.
	copyFixture("hand_history", "nlhe_cash.txt", hhDir)
	copyFixture("hand_history", "draw27_pko_T9000000005.txt", hhDir)
	copyFixture("tournament_summary", "horse_T9000000003.txt", tsDir)
	copyFixture("tournament_summary", "satellite_busted_T9000000013.txt", tsDir)
	copyFixture("tournament_summary", "draw27_pko_T9000000005.txt", tsDir)

	configPath := filepath.Join(dir, "config.json")
	cfg := &config.Config{}
	screen := hero
	cfg.Setup.PokerStarsScreenName = &screen
	cfg.Paths.HandHistory = &hhBase
	cfg.Paths.TournamentSummary = &tsBase
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	store, err := sqlitestore.NewStore(filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	link := transport.NewLink()
	alertChan := make(chan string, 100)
	srv, err := server.NewServer(store, configPath, link, alertChan)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	serverDone := make(chan struct{})
	go func() { srv.Run(); close(serverDone) }()
	t.Cleanup(func() {
		close(link.Uploads) // ends the server's Run loop
		<-serverDone
		store.Close()
	})

	// The real collector: its applyCommands loop turns the CommandRescan into a
	// folder walk. A never-closed sessionChan (no watcher here) is fine — the
	// scan is command-driven.
	collector := client.NewCollector(link, filepath.Join(dir, "lockfile.json"), nil)
	go collector.Run(make(chan watcher.FileEvent), configPath, nil)

	var mu sync.Mutex
	var alerts []string
	go func() {
		for m := range alertChan {
			mu.Lock()
			alerts = append(alerts, m)
			mu.Unlock()
		}
	}()
	report := func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		for _, a := range alerts {
			if strings.Contains(a, "Scan complete") {
				return a, true
			}
		}
		return "", false
	}

	// Trigger the scan exactly as the bot's requestRescan does.
	link.Commands <- transport.Command{Kind: transport.CommandRescan}

	deadline := time.Now().Add(15 * time.Second)
	var got string
	for {
		if r, ok := report(); ok {
			got = r
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no scan-complete report; alerts so far: %v", alerts)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !strings.Contains(got, "recorded 1 cash session(s) and 2 tournament(s)") || !strings.Contains(got, "Skipped 1") {
		t.Errorf("report = %q, want 1 cash, 2 tournaments (MTT + KO), 1 skipped (satellite)", got)
	}
	// Silent import: the only message the scan emits is the report.
	mu.Lock()
	nAlerts := len(alerts)
	mu.Unlock()
	if nAlerts != 1 {
		t.Errorf("got %d alerts, want exactly 1 (the report): %v", nAlerts, alerts)
	}

	if s, err := store.GetAllCashSessions(); err != nil || len(s) != 1 {
		t.Errorf("GetAllCashSessions = %d (err %v), want 1", len(s), err)
	}
	if _, found, _ := store.GetTournamentByID("9000000003"); !found {
		t.Error("MTT 9000000003 not recorded")
	}
	if ko, found, _ := store.GetTournamentByID("9000000005"); !found || !strings.HasSuffix(ko.Type, "-ko") {
		t.Errorf("knockout 9000000005: found=%v type=%q, want a recorded -ko row", found, ko.Type)
	}
	if _, found, _ := store.GetTournamentByID("9000000013"); found {
		t.Error("satellite 9000000013 recorded, want skipped")
	}
}
