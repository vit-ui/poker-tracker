package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/client"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
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
			t.Logf("watcher stopped: %v", err)
		}
	}()

	link := transport.NewLink()
	srv, err := server.NewServer(store, configPath, link, alertChan)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.QuietPeriod = 150 * time.Millisecond
	go srv.Run()
	collector := client.NewCollector(link, client.NewEnforcer(lockfilePath), nil)
	go collector.Run(sessionChan, configPath, nil)
	go client.RunEnforcementLoop(lockfilePath, done)

	go func() {
		for m := range alertChan {
			p.mu.Lock()
			p.alerts = append(p.alerts, m)
			p.mu.Unlock()
		}
	}()

	time.Sleep(200 * time.Millisecond)
	return p
}

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

func (p *pipeline) bankrollCents() money.Cents {
	bCents, err := p.store.GetCurrentBankroll()
	if err != nil {
		p.t.Fatalf("GetCurrentBankroll: %v", err)
	}
	return bCents
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

func (p *pipeline) bountyStub(tournamentID string) (money.Cents, bool) {
	snap, err := p.store.ExportSnapshot()
	if err != nil {
		p.t.Fatalf("ExportSnapshot: %v", err)
	}
	for _, r := range snap.Tournaments {
		if r.TournamentID == tournamentID {
			return r.BountyWonCents, true
		}
	}
	return 0, false
}

func closeEnough(aCents, bCents money.Cents) bool { return aCents == bCents }

func TestPipeline_WonFreeroll(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("hand_history", "freeroll_won_T9000000001.txt")
	p.waitFor("bounty stub written", func() bool {
		bCents, ok := p.bountyStub("9000000001")
		return ok && closeEnough(bCents, 88)
	})
	p.waitFor("hand-history result queued", func() bool {
		pending := p.pending()
		return len(pending) == 1 && pending[0].Reason == config.PendingMissingResult
	})
	p.drop("tournament_summary", "freeroll_won_T9000000001.txt")
	p.waitFor("official freeroll question replaces recovery", func() bool {
		pending := p.pending()
		return len(pending) == 1 && pending[0].Reason == config.PendingFreerollBuyin
	})

	pend := p.pending()
	if pend[0].Reason != config.PendingFreerollBuyin {
		t.Errorf("Reason = %q, want %q", pend[0].Reason, config.PendingFreerollBuyin)
	}
	if pend[0].Tournament.TournamentID != "9000000001" {
		t.Errorf("queued TournamentID = %q, want 9000000001", pend[0].Tournament.TournamentID)
	}
	if !closeEnough(p.bankrollCents(), 0) {
		t.Errorf("bankroll = %v, want 0 (deferred until the player answers)", p.bankrollCents())
	}
	if n := p.countAlerts("Freeroll"); n != 0 {
		t.Errorf("got %d freeroll alerts from the worker; the bot ticker asks, not the worker", n)
	}
}

func TestPipeline_CashSession(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("hand_history", "nlhe_cash.txt")
	p.waitFor("session recorded at session end", func() bool {
		s, err := p.store.GetAllCashSessions()
		return err == nil && len(s) == 1 && closeEnough(p.bankrollCents(), s[0].NetCents)
	})
	sessions, _ := p.store.GetAllCashSessions()
	if sessions[0].Stake != "NL2" {
		t.Errorf("stake = %q, want NL2", sessions[0].Stake)
	}
	p.waitFor("cash session summary", func() bool { return p.countAlerts("✅ Cash Session") >= 1 })
	if n := p.countAlerts("✅ Cash Session"); n != 1 {
		t.Errorf("got %d cash session summaries, want 1; alerts=%v", n, p.alertSnapshot())
	}
}

func TestPipeline_SpinGoWin(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("tournament_summary", "spingo_won_T9000000007.txt")
	p.waitFor("spin & go recorded", func() bool { return closeEnough(p.bankrollCents(), 1000) })

	p.waitFor("spin & go alert", func() bool { return p.countAlerts("🎰 Spin & Go") >= 1 })
	if n := p.countAlerts("🎰 Spin & Go"); n != 1 {
		t.Errorf("got %d Spin & Go alerts, want 1; alerts=%v", n, p.alertSnapshot())
	}
	got, found, _ := p.store.GetTournamentByID("9000000007")
	if !found || got.BaseType() != "spingo" {
		t.Errorf("tournament row = (found %v, type %q), want spingo", found, got.Type)
	}
}

func TestPipeline_MTTLoss(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("tournament_summary", "horse_T9000000003.txt")
	p.waitFor("mtt loss recorded", func() bool { return closeEnough(p.bankrollCents(), -110) })

	p.waitFor("mtt alert", func() bool { return p.countAlerts("🏆 MTT") >= 1 })
	if n := p.countAlerts("🏆 MTT"); n != 1 {
		t.Errorf("got %d MTT alerts, want 1; alerts=%v", n, p.alertSnapshot())
	}
}

func TestPipeline_SatelliteQueued(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("tournament_summary", "satellite_busted_T9000000013.txt")
	p.waitFor("satellite queued", func() bool { return len(p.pending()) == 1 })

	if p.pending()[0].Reason != config.PendingSatellite {
		t.Errorf("Reason = %q, want %q", p.pending()[0].Reason, config.PendingSatellite)
	}
	if !closeEnough(p.bankrollCents(), 0) {
		t.Errorf("bankroll = %v, want 0 (deferred)", p.bankrollCents())
	}
}

func TestPipeline_DuplicateEvent(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("tournament_summary", "spingo_won_T9000000007.txt")
	p.waitFor("first record", func() bool { return closeEnough(p.bankrollCents(), 1000) })

	p.drop("tournament_summary", "spingo_won_T9000000007.txt")
	time.Sleep(4 * time.Second)

	if !closeEnough(p.bankrollCents(), 1000) {
		t.Errorf("bankroll = %v, want 10.00 (duplicate must not double-count)", p.bankrollCents())
	}
	if n := p.countAlerts("🎰 Spin & Go"); n != 1 {
		t.Errorf("got %d Spin & Go alerts, want 1 (no second alert for a duplicate)", n)
	}
}

func TestPipeline_TwoFileCorrelationKnockout(t *testing.T) {
	p := newPipeline(t, &config.Config{})
	p.drop("tournament_summary", "draw27_pko_T9000000005.txt")
	p.waitFor("mtt recorded", func() bool { return closeEnough(p.bankrollCents(), -330) })
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

func TestPipeline_HardStopSessionLoss(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real enforcement requires the coordinated owned-process native harness")
	}
	buyins := 0.01
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

func TestPipeline_IncrementalGrowingFile(t *testing.T) {
	full, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	sep := "PokerStars Hand #"
	text := string(full)
	var offs []int
	first := true
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
	prefix := full[:offs[2]]

	p := newPipeline(t, &config.Config{})

	p.writeHH("nlhe_cash.txt", prefix)
	p.waitFor("first two hands recorded", func() bool {
		s, err := p.store.GetAllCashSessions()
		return err == nil && len(s) == 1 && s[0].Hands == 2
	})

	p.writeHH("nlhe_cash.txt", full)
	p.waitFor("all four hands recorded, bankroll consistent", func() bool {
		s, err := p.store.GetAllCashSessions()
		return err == nil && len(s) == 1 && s[0].Hands == 4 && closeEnough(p.bankrollCents(), s[0].NetCents)
	})

	want, _, err := parsers.ParseCashFile(filepath.Join(p.hhDir, "nlhe_cash.txt"), e2eHero)
	if err != nil {
		t.Fatalf("whole-file parse: %v", err)
	}
	got, _ := p.store.GetAllCashSessions()
	if got[0].Hands != want.Hands || !closeEnough(got[0].NetCents, want.NetCents) || !closeEnough(got[0].RakeCents, want.RakeCents) {
		t.Errorf("grown session = {hands %d, net %v, rake %v}, whole-file = {hands %d, net %v, rake %v}",
			got[0].Hands, got[0].NetCents, got[0].RakeCents, want.Hands, want.NetCents, want.RakeCents)
	}
}

func (p *pipeline) writeHH(name string, data []byte) {
	p.t.Helper()
	if err := os.WriteFile(filepath.Join(p.hhDir, name), data, 0644); err != nil {
		p.t.Fatal(err)
	}
}

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
		close(link.Uploads)
		<-serverDone
		store.Close()
	})

	collector := client.NewCollector(link, client.NewEnforcer(filepath.Join(dir, "lockfile.json")), nil)
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
