package server

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func recoveryFixture(t *testing.T) parsers.CashSession {
	t.Helper()
	session, _, err := parsers.ParseCashFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"), "AnonHero01")
	if err != nil {
		t.Fatal(err)
	}
	return *session
}

func awaitPendingCash(t *testing.T, path string, ready func([]config.PendingCash) bool) []config.PendingCash {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		pending, err := config.LoadPendingCash(path)
		if err != nil {
			t.Fatal(err)
		}
		if ready(pending) {
			return pending
		}
		if time.Now().After(deadline) {
			t.Fatalf("cash journal did not reach expected state: %+v", pending)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRecoveredTablesKeepIndependentDeadlines(t *testing.T) {
	dir := t.TempDir()
	path, journal := filepath.Join(dir, "config.json"), filepath.Join(dir, "pending_cash.json")
	if err := config.SaveConfig(path, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	first, second := recoveryFixture(t), recoveryFixture(t)
	first.NetUSD, second.NetUSD = -.1, -.2
	second.FirstHandID = "second-table"
	if err := config.SavePendingCash(journal, []config.PendingCash{
		{FileID: "first", Session: first, LastActivity: time.Now().Add(-time.Hour)},
		{FileID: "second", Session: second, LastActivity: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, _, stop := runRecoveredWorker(t, store, path, 400*time.Millisecond)
	defer stop()
	awaitBankroll(t, store, -.1, 250*time.Millisecond)
	awaitPendingCash(t, journal, func(entries []config.PendingCash) bool { return len(entries) == 1 && entries[0].FileID == "second" })
	awaitBankroll(t, store, -.1-.2, time.Second)
	snapshot, err := store.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.CashSessions) != 2 || len(snapshot.Ledger) != 2 {
		t.Fatalf("table recovery lost or duplicated results: %+v", snapshot)
	}
}

func TestFailedRecoveredCashWriteRemainsRecoverable(t *testing.T) {
	dir := t.TempDir()
	path, journal, db := filepath.Join(dir, "config.json"), filepath.Join(dir, "pending_cash.json"), filepath.Join(dir, "test.db")
	if err := config.SaveConfig(path, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	session := recoveryFixture(t)
	if err := config.SavePendingCash(journal, []config.PendingCash{{FileID: "table", Session: session, LastActivity: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	worker, _, stop := runRecoveredWorker(t, store, path, time.Second)
	reply := make(chan storage.OperationResult, 1)
	worker.Operations <- storage.Operation{FinalizeCash: true, Apply: func(storage.Store) storage.OperationResult { return storage.OperationResult{} }, Reply: reply}
	<-reply
	stop()
	pending, err := config.LoadPendingCash(journal)
	if err != nil || len(pending) != 1 {
		t.Fatalf("failed storage retired recovery snapshot: %+v %v", pending, err)
	}
	store, err = sqlitestore.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, _, stop = runRecoveredWorker(t, store, path, time.Second)
	defer stop()
	awaitBankroll(t, store, session.NetUSD, time.Second)
	awaitPendingCash(t, journal, func(entries []config.PendingCash) bool { return len(entries) == 0 })
}

func TestRecoveredCashShorterReplacementUpdatesSameSession(t *testing.T) {
	dir := t.TempDir()
	path, journal := filepath.Join(dir, "config.json"), filepath.Join(dir, "pending_cash.json")
	if err := config.SaveConfig(path, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	session := recoveryFixture(t)
	if err := config.SavePendingCash(journal, []config.PendingCash{{FileID: "table", Session: session, LastActivity: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// An earlier write may have succeeded before interruption of journal retirement.
	if err := store.AppendCashSession(session); err != nil {
		t.Fatal(err)
	}
	_, link, stop := runRecoveredWorker(t, store, path, 100*time.Millisecond)
	defer stop()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	offsets := handHeaderOffsetsBytes(data)
	link.Uploads <- transport.Upload{FileID: "table", Type: "cash", ScreenName: "AnonHero01", Bytes: data[:offsets[2]], Recovery: true, FileActivity: time.Now(), ClientClock: time.Now()}
	waitForCashSession(t, store, 2)
	awaitBankroll(t, store, -.03, time.Second) // first hand big blind .02 + second hand small blind .01
	snapshot, err := store.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.CashSessions) != 1 || len(snapshot.Ledger) != 1 || snapshot.CashSessions[0].Hands != 2 {
		t.Fatalf("shorter recovery did not correct original row: %+v", snapshot)
	}
}

func TestSitOutUpdateRearmsPendingCash(t *testing.T) {
	dir := t.TempDir()
	path, journal := filepath.Join(dir, "config.json"), filepath.Join(dir, "pending_cash.json")
	if err := config.SaveConfig(path, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, link, stop := runRecoveredWorker(t, store, path, 400*time.Millisecond)
	defer stop()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	link.Uploads <- transport.Upload{FileID: "table", Type: "cash", ScreenName: "AnonHero01", Bytes: data}
	initial := awaitPendingCash(t, journal, func(entries []config.PendingCash) bool { return len(entries) == 1 })[0]
	time.Sleep(250 * time.Millisecond)
	offsets := handHeaderOffsetsBytes(data)
	sitout := strings.TrimPrefix(string(data[:offsets[1]]), "\ufeff")
	sitout = strings.ReplaceAll(sitout, "AnonHero01", "AwayPlayer")
	sitout = strings.ReplaceAll(sitout, "#261427127186", "#900000000001")
	link.Uploads <- transport.Upload{FileID: "table", Type: "cash", Offset: int64(len(data)), ScreenName: "AnonHero01", Bytes: []byte(sitout)}
	updated := awaitPendingCash(t, journal, func(entries []config.PendingCash) bool {
		return len(entries) == 1 && entries[0].LastActivity.After(initial.LastActivity)
	})[0]
	if updated.Session.Hands != initial.Session.Hands || updated.Session.NetUSD != initial.Session.NetUSD {
		t.Fatalf("sit-out hand changed player results: %+v", updated)
	}
	time.Sleep(200 * time.Millisecond) // past original deadline, before the rearmed deadline
	if bankroll, err := store.GetCurrentBankroll(); err != nil || bankroll != 0 {
		t.Fatalf("old deadline completed rearmed session: %v %v", bankroll, err)
	}
	awaitBankroll(t, store, initial.Session.NetUSD, time.Second)
}

func runRecoveredWorker(t *testing.T, store *sqlitestore.Store, path string, quiet time.Duration) (*Server, *transport.Link, func()) {
	t.Helper()
	link := transport.NewLink()
	worker, err := NewServer(store, path, link, make(chan string, 100))
	if err != nil {
		t.Fatal(err)
	}
	worker.QuietPeriod = quiet
	finished := make(chan error, 1)
	go func() { finished <- worker.Run() }()
	return worker, link, func() {
		close(link.Uploads)
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("recovered worker did not stop")
		}
	}
}

func awaitBankroll(t *testing.T, store *sqlitestore.Store, want float64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got, err := store.GetCurrentBankroll()
		if err == nil && math.Abs(got-want) < 1e-9 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bankroll=%v, error=%v; want %v", got, err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRecoveredCashRestoresRemainingQuietInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(path, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	session := recoveryFixture(t)
	journal := filepath.Join(dir, "pending_cash.json")
	if err := config.SavePendingCash(journal, []config.PendingCash{{FileID: "table", Session: session, LastActivity: time.Now().Add(-300 * time.Millisecond)}}); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, link, stop := runRecoveredWorker(t, store, path, 400*time.Millisecond)
	defer stop()
	select {
	case command := <-link.Commands:
		if command.Kind != transport.CommandResyncFiles || len(command.Files) != 1 || command.Files[0] != "table" {
			t.Fatalf("resync command %+v", command)
		}
	case <-time.After(time.Second):
		t.Fatal("missing recovered source resync")
	}
	if value, err := store.GetCurrentBankroll(); err != nil || value != 0 {
		t.Fatalf("restored timer finalized prematurely: %v, %v", value, err)
	}
	awaitBankroll(t, store, session.NetUSD, 300*time.Millisecond)
}

func TestExpiredCashRecoveryUpsertsAfterInterruptedJournalRetirement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(path, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "pending_cash.json")
	session := recoveryFixture(t)
	saved := []config.PendingCash{{FileID: "table", Session: session, LastActivity: time.Now().Add(-time.Hour)}}
	store, err := sqlitestore.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 2; i++ {
		if err := config.SavePendingCash(journal, saved); err != nil {
			t.Fatal(err)
		}
		_, _, stop := runRecoveredWorker(t, store, path, time.Second)
		awaitBankroll(t, store, session.NetUSD, time.Second)
		deadline := time.Now().Add(time.Second)
		for {
			pending, err := config.LoadPendingCash(journal)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("worker did not retire the recovered cash journal")
			}
			time.Sleep(time.Millisecond)
		}
		stop()
		snapshot, err := store.ExportSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.CashSessions) != 1 || len(snapshot.Ledger) != 1 {
			t.Fatalf("recovery duplicated rows: %+v", snapshot)
		}
		pending, err := config.LoadPendingCash(journal)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatal("stored cash remained in journal")
		}
	}
}

func TestCashQuietDefaultAndSupersededGeneration(t *testing.T) {
	if cashSessionSummaryQuietPeriod != 150*time.Second {
		t.Fatalf("quiet default %v", cashSessionSummaryQuietPeriod)
	}
	timers := make(map[string]*cashTimer)
	signals := make(chan cashEndSignal)
	done := make(chan struct{})
	defer close(done)
	armCashSessionEnd(timers, "table", time.Millisecond, signals, done)
	previous := timers["table"]
	time.Sleep(10 * time.Millisecond)
	armCashSessionEnd(timers, "table", time.Hour, signals, done)
	defer timers["table"].Stop()
	select {
	case signal := <-signals:
		if signal.Generation != previous || signal.Generation == timers[signal.FileID] {
			t.Fatal("stale callback carries the current generation")
		}
	case <-time.After(time.Second):
		t.Fatal("expired callback not observed")
	}
}

func TestUnchangedRecoveryReplacementDoesNotRestartQuietPeriod(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(path, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	session := recoveryFixture(t)
	lastActivity := time.Now().Add(-300 * time.Millisecond)
	if err := config.SavePendingCash(filepath.Join(dir, "pending_cash.json"), []config.PendingCash{{FileID: "table", Session: session, LastActivity: lastActivity}}); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, link, stop := runRecoveredWorker(t, store, path, 400*time.Millisecond)
	defer stop()
	select {
	case <-link.Commands:
	case <-time.After(time.Second):
		t.Fatal("missing resync request")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	link.Uploads <- transport.Upload{FileID: "table", Type: "cash", ScreenName: "AnonHero01", Bytes: data, Recovery: true, FileActivity: lastActivity.Add(-time.Second), ClientClock: time.Now()}
	awaitBankroll(t, store, session.NetUSD, 300*time.Millisecond)
}

func TestRemainingQuietClampsFutureClockAndMakesExpiredEligible(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		last time.Time
		want time.Duration
	}{
		{"remaining", now.Add(-time.Minute), 90 * time.Second},
		{"expired", now.Add(-3 * time.Minute), -30 * time.Second},
		{"clock moved backward", now.Add(time.Hour), 150 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := remainingCashQuiet(150*time.Second, test.last, now); got != test.want {
				t.Fatalf("remaining=%v, want %v", got, test.want)
			}
		})
	}
}
