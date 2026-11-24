package server

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

const rounderID = "9900000200"

func rounderHistory(hands int, terminal string) []byte {
	var history strings.Builder
	for i := 1; i <= hands; i++ {
		fmt.Fprintf(&history, "PokerStars Hand #%d: Tournament #%s, $0.98+$0.12 USD Hold'em No Limit - Level I (10/20) - 2025/02/10 22:00:00 ET\n", 990000020000+i, rounderID)
		history.WriteString("Dealt to AnonHero01 [As Ad]\n")
		if i == hands {
			history.WriteString(terminal)
		}
		history.WriteString("*** SUMMARY ***\nTotal pot 30\n\n")
	}
	return []byte(history.String())
}

func TestTournamentRecoveryRounderFileAndFolderScan(t *testing.T) {
	for _, file := range []bool{false, true} {
		t.Run(map[bool]string{true: "file", false: "folder"}[file], func(t *testing.T) {
			root := t.TempDir()
			store, err := sqlitestore.NewStore(filepath.Join(root, "tracker.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			alerts := make(chan string, 10)
			worker, link, stop := runRecoveredWorkerWithAlerts(t, store, filepath.Join(root, "config.json"), 150*time.Second, alerts)
			defer stop()
			history := rounderHistory(174, "AnonHero01 finished the tournament in 5th place and received $5.61.\n")
			scan := func() string {
				reply := make(chan storage.OperationResult, 1)
				if file {
					worker.Operations <- storage.Operation{ScanPath: "rounder.txt", Reply: reply}
					cmd := <-link.Commands
					if cmd.Kind != transport.CommandScanFile {
						t.Fatalf("unexpected command %+v", cmd)
					}
					link.Uploads <- transport.Upload{Scan: &transport.ScanChunk{RequestID: cmd.RequestID, ScreenName: "AnonHero01", Bytes: history, Done: true}}
					result := <-reply
					if result.Err != nil || result.Scan.TournamentHistories != 1 {
						t.Fatalf("scan result %+v", result)
					}
					return result.Scan.String()
				} else {
					link.Uploads <- transport.Upload{Type: transport.ImportStart}
					link.Uploads <- transport.Upload{Type: "cash", FileID: "scan/rounder.txt", Bytes: history, ScreenName: "AnonHero01", Import: true}
					link.Uploads <- transport.Upload{Type: transport.ImportDone}
					report, ok := waitForString(t, alerts, time.Second)
					if !ok {
						t.Fatal("missing folder scan report")
					}
					return report
				}
			}
			if report := scan(); !strings.Contains(report, storage.ScanReviewLabel) {
				t.Fatalf("scan report missing confirmation action: %s", report)
			}
			path := filepath.Join(filepath.Dir(worker.configPath), "pending_tournaments.json")
			pending, err := config.LoadPendingTournaments(path)
			if err != nil || len(pending) != 1 || pending[0].QuietUntil != nil || !pending[0].Historical || pending[0].Recovery.TotalCostCents != nil || *pending[0].Recovery.PayoutCents != 561 {
				t.Fatalf("pending = %+v, %v", pending, err)
			}
			if _, found, err := store.GetTournamentByID(rounderID); found || err != nil {
				t.Fatalf("tournament posted before confirming a total cost: found %v, %v", found, err)
			}
			answer := storage.TournamentAnswer{TournamentID: rounderID, HandID: pending[0].Recovery.HandID, Reason: config.PendingMissingResult, TotalCostCents: 110}
			reply := make(chan storage.OperationResult, 1)
			worker.Operations <- storage.Operation{TournamentAnswer: &answer, Reply: reply}
			if got := <-reply; got.Err != nil || !got.Found || got.NetCents != 451 {
				t.Fatalf("confirmation = %+v, want +$4.51", got)
			}
			if report := scan(); strings.Contains(report, storage.ScanReviewLabel) {
				t.Fatalf("repeat scan asked about verified result: %s", report)
			}
			select {
			case msg := <-alerts:
				t.Fatalf("unexpected historical recovery alert: %s", msg)
			default:
			}
			got, found, err := store.GetTournamentByID(rounderID)
			if err != nil || !found || got.FinishPos != 5 || got.TotalPlayers != 0 || got.Hands == nil || *got.Hands != 174 || got.ResultSource != parsers.TournamentSourceConfirmed {
				t.Fatalf("tournament = %+v, found %v, err %v", got, found, err)
			}
			rows, err := store.GetLedgerEntriesSince(time.Time{})
			if err != nil || len(rows) != 1 || rows[0].AmountCents != 451 || rows[0].RunningTotalCents != 451 {
				t.Fatalf("ledger = %+v, %v", rows, err)
			}
		})
	}
}

func TestTournamentRecoveryRestartAndLateSummary(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	pendingPath := filepath.Join(root, "pending_tournaments.json")
	db, err := sqlitestore.NewStore(filepath.Join(root, "tracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	alerts := make(chan string, 10)
	_, link, stop := runRecoveredWorkerWithAlerts(t, db, configPath, time.Hour, alerts)
	history := rounderHistory(2, "AnonHero01 finished the tournament in 5th place and received $5.61\nAnonHero01 paid a total of $1.10 in entry fees\n")
	link.Uploads <- transport.Upload{Type: "cash", FileID: "live/rounder.txt", ScreenName: "AnonHero01", Bytes: history}
	pending := waitTournamentQueue(t, pendingPath, func(entries []config.PendingTournament) bool {
		return len(entries) == 1 && entries[0].QuietUntil != nil
	})
	stop()
	at := time.Now().Add(-time.Second)
	pending[0].QuietUntil = &at
	if err := config.SavePendingTournaments(pendingPath, pending); err != nil {
		t.Fatal(err)
	}
	_, link, stop = runRecoveredWorkerWithAlerts(t, db, configPath, time.Hour, alerts)
	defer stop()
	awaitBankroll(t, db, 451, time.Second)
	if msg, ok := waitForString(t, alerts, time.Second); !ok || !strings.Contains(msg, "Finish: 5th\n") {
		t.Fatalf("recovered summary = %q", msg)
	}
	equivalent := fmt.Sprintf("PokerStars Tournament #%s, Hold'em No Limit\nBuy-In: $0.98/$0.12 USD\n18 players\nTournament started 2025/02/09 21:00:00 ET\n  5: AnonHero01 (Brazil), $5.61 (36%%)\n", rounderID)
	link.Uploads <- transport.Upload{Type: "tournament", FileID: "live/summary.txt", ScreenName: "AnonHero01", Bytes: []byte(equivalent)}
	if msg, ok := waitForString(t, alerts, 100*time.Millisecond); ok {
		t.Fatalf("equivalent summary repeated the alert: %s", msg)
	}
	secondID := "9900000201"
	second := parsers.Tournament{Date: time.Date(2025, 2, 10, 22, 0, 0, 0, time.UTC), TournamentID: secondID, Type: "mtt", FinishPos: 5, BuyInCents: 110, NetCents: 0, ResultSource: parsers.TournamentSourceHistory}
	if _, err := db.AppendTournament(second); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendLedgerEntry("deposit", 200, time.Now(), "money:after-tournament"); err != nil {
		t.Fatal(err)
	}
	official := fmt.Sprintf("PokerStars Tournament #%s, Hold'em No Limit\nBuy-In: $0.98/$0.12 USD\n18 players\nTotal Prize Pool: $17.64 USD\nTournament started 2025/02/09 21:00:00 ET\n  4: AnonHero01 (Brazil), $2.00 (36%%)\n", secondID)
	link.Uploads <- transport.Upload{Type: "tournament", FileID: "live/summary.txt", ScreenName: "AnonHero01", Bytes: []byte(official)}
	awaitBankroll(t, db, 741, time.Second)
	snapshot, err := db.ExportSnapshot()
	if err != nil || len(snapshot.Tournaments) != 2 || len(snapshot.Ledger) != 3 {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
	row := snapshot.Tournaments[0]
	ledger := snapshot.Ledger
	if row.ResultSource != parsers.TournamentSourceSummary || row.TotalPlayers != 18 || row.FinishPos != 5 || *row.Hands != 2 ||
		ledger[0].AmountCents != 451 || ledger[0].RunningTotalCents != 451 || ledger[1].RunningTotalCents != 541 ||
		ledger[2].RunningTotalCents != 741 || snapshot.Tournaments[1].FinishPos != 4 ||
		!ledger[0].Date.Equal(time.Date(2025, 2, 9, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("corrected snapshot = %+v", snapshot)
	}
}

func TestTournamentRecoveryFailedWriteHasNoResultEffects(t *testing.T) {
	root := t.TempDir()
	db, err := sqlitestore.NewStore(filepath.Join(root, "tracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2025, 2, 10, 22, 0, 0, 0, time.UTC)
	if err := db.AppendLedgerEntry("deposit", math.MaxInt64, at, "money:overflow-setup"); err != nil {
		t.Fatal(err)
	}
	alerts := make(chan string, 10)
	s := &Server{store: db, configPath: filepath.Join(root, "config.json"), alertChan: alerts}
	r := tournamentResults{server: s, fired: map[string]bool{}, enforce: func(string, int, string) { t.Error("enforced after failed write") }}
	tt := parsers.Tournament{Date: at, TournamentID: rounderID, Type: "mtt", FinishPos: 5, BuyInCents: 110, NetCents: 451}
	if result := r.write(tt, parsers.Tournament{}, false, false); result.Err == nil {
		t.Fatal("overflow should make the result write fail")
	}
	select {
	case msg := <-alerts:
		t.Errorf("unexpected message after failed result write: %s", msg)
	default:
	}
	rows, err := db.GetLedgerEntriesSince(time.Time{})
	if err != nil || len(rows) != 1 || rows[0].RunningTotalCents != money.Cents(math.MaxInt64) {
		t.Fatalf("ledger after failure = %+v, %v", rows, err)
	}
	if _, found, err := db.GetTournamentByID(rounderID); err != nil || found {
		t.Fatalf("result row was written despite ledger failure: %v, %v", found, err)
	}
}

func waitTournamentQueue(t *testing.T, path string, match func([]config.PendingTournament) bool) []config.PendingTournament {
	t.Helper()
	until := time.Now().Add(time.Second)
	for {
		pending, err := config.LoadPendingTournaments(path)
		if err != nil {
			t.Fatal(err)
		}
		if match(pending) {
			return pending
		}
		if time.Now().After(until) {
			t.Fatalf("unexpected pending tournaments: %+v", pending)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTournamentResumeInvalidatesFinishAndSummaryWinsScan(t *testing.T) {
	store, link, worker := startTestServer(t)
	path := filepath.Join(filepath.Dir(worker.configPath), "pending_tournaments.json")
	history := rounderHistory(2, "AnonHero01 finished the tournament in 5th place and received $5.61\n")
	link.Uploads <- transport.Upload{Type: "cash", FileID: "live/rounder.txt", ScreenName: "AnonHero01", Bytes: history}
	waitTournamentQueue(t, path, func(entries []config.PendingTournament) bool { return len(entries) == 1 })
	resumed := rounderHistory(3, "")
	link.Uploads <- transport.Upload{Type: "cash", FileID: "live/rounder.txt", ScreenName: "AnonHero01", Bytes: resumed}
	waitTournamentQueue(t, path, func(entries []config.PendingTournament) bool { return len(entries) == 0 })
	official := fmt.Sprintf("PokerStars Tournament #%s, Hold'em No Limit\nBuy-In: $0.98/$0.12 USD\n20 players\nTournament started 2025/02/10 22:00:00 ET\n  6: AnonHero01 (Brazil), $3.00 (36%%)\n", rounderID)
	link.Uploads <- transport.Upload{Type: transport.ImportStart}
	link.Uploads <- transport.Upload{Type: "cash", FileID: "folder/rounder.txt", ScreenName: "AnonHero01", Import: true, Bytes: history}
	link.Uploads <- transport.Upload{Type: "tournament", FileID: "folder/summary.txt", ScreenName: "AnonHero01", Import: true, Bytes: []byte(official)}
	link.Uploads <- transport.Upload{Type: transport.ImportDone}
	awaitBankroll(t, store, 190, time.Second)
	result, found, err := store.GetTournamentByID(rounderID)
	if err != nil || !found || result.FinishPos != 6 || result.ResultSource != parsers.TournamentSourceSummary {
		t.Fatalf("official summary did not win: %+v, %v, %v", result, found, err)
	}
}

func TestTournamentDeadlineSurvivesDeploymentTransfer(t *testing.T) {
	target := startTransferTarget(t, t.TempDir())
	var payload storage.DeploymentPayload
	if err := json.Unmarshal(deploymentBody(t), &payload); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2025, 2, 10, 22, 0, 0, 0, time.UTC)
	until := time.Now().Add(-time.Second)
	costCents, payoutCents := money.Cents(110), money.Cents(561)
	payload.Pending = append(payload.Pending, config.PendingTournament{
		Tournament: parsers.Tournament{Date: at, TournamentID: rounderID, Type: "mtt", FinishPos: 5, ResultSource: parsers.TournamentSourceHistory, ResultHandID: "990000030001"},
		Reason:     config.PendingMissingResult, QuietUntil: &until, Historical: true,
		Recovery: &parsers.TournamentFinish{Position: 5, HandID: "990000030001", At: at, TotalCostCents: &costCents, PayoutCents: &payoutCents},
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	id := "tournament-deadline-transfer"
	conn, _ := connectDeployment(t, target, id, raw)
	sendDeployment(t, conn, id, raw, 0)
	for _, kind := range []string{transport.TransferVerify, transport.TransferActivate} {
		if reply := transferExchange(t, conn, transport.TransferRequest{Kind: kind, ID: id}); reply.Error != "" {
			t.Fatalf("transfer %s: %+v", kind, reply)
		}
	}
	awaitBankroll(t, target.store, 233, time.Second)
	rec, found, err := target.store.GetTournamentByID(rounderID)
	if err != nil || !found || rec.ResultSource != parsers.TournamentSourceHistory || rec.NetCents != 451 {
		t.Fatalf("recovery after transfer = %+v, %v, %v", rec, found, err)
	}
}

func TestMixedFileOfficialSummarySupersedesPendingRecovery(t *testing.T) {
	store, link, worker := startTestServer(t)
	path := filepath.Join(filepath.Dir(worker.configPath), "pending_tournaments.json")
	history := rounderHistory(2, "AnonHero01 finished the tournament in 5th place and received $5.61\n")
	link.Uploads <- transport.Upload{Type: "cash", FileID: "live/rounder.txt", ScreenName: "AnonHero01", Bytes: history}
	waiting := waitTournamentQueue(t, path, func(entries []config.PendingTournament) bool {
		return len(entries) == 1 && entries[0].QuietUntil == nil
	})
	official := fmt.Sprintf("PokerStars Tournament #%s, Hold'em No Limit\nBuy-In: $0.98/$0.12 USD\n20 players\nTournament started 2025/02/10 22:00:00 ET\n  6: AnonHero01 (Brazil), $3.00 (36%%)\n", rounderID)
	reply := make(chan storage.OperationResult, 1)
	worker.Operations <- storage.Operation{ScanPath: "mixed.txt", Reply: reply}
	request := <-link.Commands
	link.Uploads <- transport.Upload{Scan: &transport.ScanChunk{
		RequestID: request.RequestID, Bytes: append(history, official...), ScreenName: "AnonHero01", Done: true,
	}}
	result := <-reply
	if result.Err != nil || result.Scan.Tournaments != 1 || result.Scan.MissingResults != 0 || result.Scan.TournamentHistories != 1 {
		t.Fatalf("mixed file scan = %+v", result)
	}
	answer := storage.TournamentAnswer{
		TournamentID: rounderID, HandID: waiting[0].Recovery.HandID, Reason: config.PendingMissingResult, TotalCostCents: 110,
	}
	worker.Operations <- storage.Operation{TournamentAnswer: &answer, Reply: reply}
	if result := <-reply; result.Err != nil || result.Found {
		t.Fatalf("superseded answer accepted: %+v", result)
	}
	ledger, err := store.GetLedgerEntriesSince(time.Time{})
	if err != nil || len(ledger) != 1 || ledger[0].AmountCents != 190 {
		t.Fatalf("ledger = %+v, %v", ledger, err)
	}
}
