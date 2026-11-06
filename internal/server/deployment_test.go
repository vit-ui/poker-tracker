package server

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

type transferTarget struct {
	worker *Server
	link   *transport.Link
	store  *sqlitestore.Store
	info   transport.ConnInfo
	close  func()
}

func TestDeploymentReservationPreflightAndRollback(t *testing.T) {
	for _, conflict := range []string{"none", "history", "chat", "cash", "unconfirmed-protection", "confirmed-protection", "pending-closure", "delivered-closure", "collector-closure"} {
		t.Run(conflict, func(t *testing.T) {
			root := t.TempDir()
			target := startTransferTarget(t, root)
			at := time.Now()
			switch conflict {
			case "pending-closure", "delivered-closure", "collector-closure":
				name := "closure_notifications.json"
				if conflict == "collector-closure" {
					name = "collector_closures.json"
				}
				event := config.ClosureEvent{ID: "closure:preflight", ClosedAt: at, Lock: config.LockFile{ExpiresAt: at.Add(time.Hour)}}
				if err := config.SaveClosureNotifications(filepath.Join(root, name), map[string]config.ClosureNotification{event.ID: {Event: event, Delivered: conflict == "delivered-closure"}}); err != nil {
					t.Fatal(err)
				}
			case "history":
				if err := target.store.AppendLedgerEntry("deposit", 1, storage.AsStoredClock(at), "money:existing"); err != nil {
					t.Fatal(err)
				}
			case "chat":
				_, err := config.UpdateConfig(target.worker.configPath, func(cfg *config.Config) error { chat := "9999"; cfg.Setup.TelegramChatID = &chat; return nil })
				if err != nil {
					t.Fatal(err)
				}
			case "cash":
				if err := config.SavePendingCash(filepath.Join(root, "pending_cash.json"), []config.PendingCash{{FileID: "owned-pending", LastActivity: at, Session: parsers.CashSession{FirstHandID: "9000000100", Hands: 1}}}); err != nil {
					t.Fatal(err)
				}
			case "unconfirmed-protection", "confirmed-protection":
				lock := config.LockFile{TriggerID: "existing-protection", TriggeredAt: at, ExpiresAt: at.Add(time.Hour), DurationHours: 1, Reason: "manual"}
				if err := config.SaveHardstopDeliveries(filepath.Join(root, "hardstop_deliveries.json"), map[string]config.HardstopDelivery{lock.TriggerID: {Lock: lock, Confirmed: conflict == "confirmed-protection"}}); err != nil {
					t.Fatal(err)
				}
			}
			chat := "4242"
			id := "preflight-" + conflict
			conn, reply, err := transport.ConnectTransfer(target.info, transport.TransferRequest{Kind: transport.TransferReserve, ID: id, TelegramChatID: &chat}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			accept := conflict == "none" || conflict == "confirmed-protection" || conflict == "delivered-closure"
			if (reply.Error == "") != accept {
				t.Fatalf("reservation=%+v accept=%v", reply, accept)
			}
			if accept {
				if reply.Phase != transport.TransferPhaseReserved {
					t.Fatalf("reservation=%+v", reply)
				}
				reply = transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferRollback, ID: id})
				if reply.Error != "" || reply.Phase != transport.TransferPhaseRolledBack {
					t.Fatalf("reservation rollback=%+v", reply)
				}
			}
			if conflict == "history" {
				bankrollCents, err := target.store.GetCurrentBankroll()
				if err != nil || bankrollCents != 1 {
					t.Fatalf("existing bankroll=%d %v", bankrollCents, err)
				}
			}
		})
	}
}

func TestDeploymentConcurrentReceiptsPreserveCollectorAdmission(t *testing.T) {
	target := startTransferTarget(t, t.TempDir())
	body := deploymentBody(t)
	id := "concurrent-receipts"
	conn, _ := connectDeployment(t, target, id, body)
	sendDeployment(t, conn, id, body, 0)
	if reply := transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferVerify, ID: id}); reply.Error != "" {
		t.Fatal(reply.Error)
	}
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			peer, reply, err := transport.ConnectTransfer(target.info, transport.TransferRequest{Kind: transport.TransferStatus, ID: id}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer peer.Close()
			if reply.Error != "" || (reply.Phase != transport.TransferPhaseVerified && reply.Phase != transport.TransferPhaseActivated) {
				t.Errorf("concurrent receipt=%+v", reply)
			}
		})
	}
	if reply := transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferActivate, ID: id}); reply.Error != "" {
		t.Fatal(reply.Error)
	}
	readers.Wait()
	collector, err := transport.ConnectCollector(target.info, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Close()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-target.link.CollectorDone():
		case <-deadline:
			t.Fatal("stale transfer receipt kept collector admission reserved")
		default:
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDeploymentInterruptedImportOwnsOnlySourcePrefixes(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		ledger     bool
		foreign    bool
	}{
		{"cash tab only", transport.TransferVerify, false, false},
		{"ledger prefix", transport.TransferVerify, true, false},
		{"rollback prefix", transport.TransferRollback, true, false},
		{"foreign verify", transport.TransferVerify, true, true},
		{"foreign rollback", transport.TransferRollback, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			target := startTransferTarget(t, root)
			body := deploymentBody(t)
			id := "interrupted-import"
			conn, _ := connectDeployment(t, target, id, body)
			sendDeployment(t, conn, id, body, 0)
			conn.Close()
			target.close()

			// Persist the state left by a nontransactional import interrupted
			// between tabs/batches. Use a real store without a Sheets simulator.
			var payload storage.DeploymentPayload
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			partial := storage.Snapshot{CashSessions: payload.Snapshot.CashSessions}
			if tc.ledger {
				partial.Tournaments = payload.Snapshot.Tournaments[:1]
				partial.Ledger = payload.Snapshot.Ledger[:1]
			}
			if tc.foreign {
				partial.CashSessions[0].NetCents = -2 // source is -3 cents
			}
			db, err := sqlitestore.NewStore(filepath.Join(root, "target.db"))
			if err != nil {
				t.Fatal(err)
			}
			err = db.ImportSnapshot(partial)
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(root, "deployment_transfer.json")
			state, err := config.LoadDeploymentTransfer(journal)
			if err != nil {
				t.Fatal(err)
			}
			state.Phase = transport.TransferPhaseImporting
			if err := config.SaveDeploymentTransfer(journal, state); err != nil {
				t.Fatal(err)
			}
			target = startTransferTarget(t, root)
			conn, reply, err := transport.ConnectTransfer(target.info, transport.TransferRequest{Kind: transport.TransferStatus, ID: id}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if reply.Phase != transport.TransferPhaseImporting {
				t.Fatalf("restart receipt=%+v", reply)
			}
			reply = transferExchange(t, conn, transport.TransferRequest{Kind: tc.kind, ID: id})
			actual, err := target.store.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if tc.foreign {
				if reply.Error == "" || len(actual.CashSessions) != 1 || actual.CashSessions[0].NetCents != -2 {
					t.Fatalf("foreign partial rows changed: %+v %+v", reply, actual)
				}
				return
			}
			if reply.Error != "" {
				t.Fatal(reply.Error)
			}
			if tc.kind == transport.TransferRollback {
				if !actual.IsEmpty() || reply.Phase != transport.TransferPhaseRolledBack {
					t.Fatalf("partial rollback=%+v %+v", reply, actual)
				}
			} else {
				if err := storage.VerifySnapshot(payload.Snapshot, actual, time.Second); err != nil {
					t.Fatal(err)
				}
				bankrollCents, err := target.store.GetCurrentBankroll()
				if err != nil || bankrollCents != -218 || reply.Phase != transport.TransferPhaseVerified {
					t.Fatalf("recovered bankroll=%d receipt=%+v error=%v", bankrollCents, reply, err)
				}
			}
		})
	}
}

func startTransferTarget(t *testing.T, root string) *transferTarget {
	t.Helper()
	path := filepath.Join(root, "config.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		backend := "sqlite"
		if err := config.SaveConfig(path, &config.Config{Setup: config.Setup{StorageBackend: &backend}}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sqlitestore.NewStore(filepath.Join(root, "target.db"))
	if err != nil {
		t.Fatal(err)
	}
	link := transport.NewLink()
	state, err := config.LoadDeploymentTransfer(filepath.Join(root, "deployment_transfer.json"))
	if err != nil {
		t.Fatal(err)
	}
	if config.DeploymentInProgress(state) {
		link.InitialTransferID = state.ID
	}
	worker, err := NewServer(db, path, link, make(chan string, 100))
	if err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run() }()
	cert, key, fp, err := transport.GenerateServerCert()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := transport.Listen("127.0.0.1:0", cert, key)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() { defer close(served); transport.Serve(listener, "transfer-test-token", link) }()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	target := &transferTarget{worker: worker, link: link, store: db, info: transport.ConnInfo{Host: host, Port: port, Token: "transfer-test-token", Fingerprint: fp}}
	closed := false
	target.close = func() {
		if closed {
			return
		}
		closed = true
		listener.Close()
		<-served
		close(link.Uploads)
		select {
		case err := <-workerDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("reserved worker did not stop")
		}
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(target.close)
	return target
}
func deploymentBody(t *testing.T) []byte {
	t.Helper()
	db, err := sqlitestore.NewStore(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	if err := db.AppendLedgerEntry("deposit", 100, at, "money:transfer-deposit"); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendCashSession(parsers.CashSession{Date: at.Add(time.Minute), FirstHandID: "9000000100", Stake: "0.01/0.02", Hands: 4, NetCents: -3, BuyInCents: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendTournament(parsers.Tournament{Date: at.Add(2 * time.Minute), Type: "mtt", TournamentID: "9000000101", BuyInCents: 330, NetCents: -330, FinishPos: 5, TotalPlayers: 10}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTournamentHandStats("9000000101", 25, 10, 20, 15, true); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTournamentHandStats("9000000102", 20, 5, 12, 7, true); err != nil {
		t.Fatal(err)
	}
	snapshot, err := db.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	chat := "4242"
	loss := money.Cents(500)
	pending := []config.PendingTournament{{Tournament: parsers.Tournament{TournamentID: "9000000102", Type: "mtt", BuyInCents: 0, NetCents: 20}, Reason: "freeroll_buyin", FirstQueuedAt: at, LastRemindedAt: at}}
	body, err := json.Marshal(storage.DeploymentPayload{Snapshot: snapshot, Settings: config.OperationalSettings{TelegramChatID: &chat, Alerts: config.Alerts{DailyLossCents: &loss}}, Pending: pending, ClientClock: at.In(time.FixedZone("player", -3*3600))})
	if err != nil {
		t.Fatal(err)
	}
	return body
}
func connectDeployment(t *testing.T, target *transferTarget, id string, body []byte) (*transport.TransferConnection, transport.TransferReply) {
	t.Helper()
	sum := sha256.Sum256(body)
	conn, reply, err := transport.ConnectTransfer(target.info, transport.TransferRequest{Kind: transport.TransferBegin, ID: id, TotalBytes: int64(len(body)), SHA256: fmt.Sprintf("%x", sum)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Error != "" {
		conn.Close()
		t.Fatal(reply.Error)
	}
	t.Cleanup(conn.Close)
	return conn, reply
}
func transferExchange(t *testing.T, conn *transport.TransferConnection, req transport.TransferRequest) transport.TransferReply {
	t.Helper()
	reply, err := conn.Exchange(req)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}
func sendDeployment(t *testing.T, conn *transport.TransferConnection, id string, body []byte, offset int64) {
	t.Helper()
	for offset < int64(len(body)) {
		end := offset + 257
		if end > int64(len(body)) {
			end = int64(len(body))
		}
		reply := transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferChunk, ID: id, Offset: offset, Data: body[offset:end]})
		if reply.Error != "" || reply.NextOffset != end {
			t.Fatalf("chunk reply=%+v", reply)
		}
		offset = end
	}
}
func TestDeploymentTransferDurableResumeAndActivation(t *testing.T) {
	root := t.TempDir()
	target := startTransferTarget(t, root)
	body := deploymentBody(t)
	id := "owned-transfer-resume"
	conn, _ := connectDeployment(t, target, id, body)
	first := transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferChunk, ID: id, Data: body[:257]})
	if first.Error != "" || first.NextOffset != 257 {
		t.Fatalf("first=%+v", first)
	}
	// A replay must match durable bytes, and no unacknowledged gap is accepted.
	for _, req := range []transport.TransferRequest{{Kind: transport.TransferChunk, ID: id, Offset: 258, Data: []byte("gap")}, {Kind: transport.TransferChunk, ID: id, Data: []byte("wrong")}} {
		reply := transferExchange(t, conn, req)
		if reply.Error == "" || reply.NextOffset != 257 {
			t.Fatalf("invalid chunk accepted: %+v", reply)
		}
	}
	if _, err := config.UpdateConfig(target.worker.configPath, func(cfg *config.Config) error { n := money.Cents(1); cfg.Alerts.DailyLossCents = &n; return nil }); err == nil {
		t.Fatal("bot settings bypassed reservation")
	}
	if err := config.UpdatePendingTournaments(filepath.Join(root, "pending_tournaments.json"), func(current []config.PendingTournament) ([]config.PendingTournament, bool) {
		return append(current, config.PendingTournament{}), true
	}); err == nil {
		t.Fatal("pending queue bypassed reservation")
	}
	conn.Close()
	target.close()
	// Simulate a crash after bytes were written but before their offset was saved.
	file, err := os.OpenFile(filepath.Join(root, "deployment_transfer.data"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Write([]byte("unacknowledged tail"))
	file.Close()
	target = startTransferTarget(t, root)
	conn, reply := connectDeployment(t, target, id, body)
	if reply.NextOffset != 257 {
		t.Fatalf("resume offset=%d", reply.NextOffset)
	}
	sendDeployment(t, conn, id, body, 257)
	reply = transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferVerify, ID: id})
	if reply.Error != "" || reply.Phase != transport.TransferPhaseVerified {
		t.Fatalf("verify=%+v", reply)
	}
	cfg, err := config.LoadConfig(target.worker.configPath)
	if err != nil || cfg.Setup.TelegramChatID != nil {
		t.Fatalf("policy committed before verification/cutover: %+v %v", cfg, err)
	}
	var payload storage.DeploymentPayload
	json.Unmarshal(body, &payload)
	before, err := target.store.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.VerifySnapshot(payload.Snapshot, before, time.Second); err != nil {
		t.Fatal(err)
	}
	bankroll, err := target.store.GetCurrentBankroll()
	if err != nil || bankroll != -218 {
		t.Fatalf("bankroll=%d %v, want -218", bankroll, err)
	}
	reply = transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferActivate, ID: id})
	if reply.Error != "" || reply.Phase != transport.TransferPhaseActivated {
		t.Fatalf("activate=%+v", reply)
	}
	cfg, err = config.LoadConfig(target.worker.configPath)
	if err != nil || cfg.Setup.TelegramChatID == nil || *cfg.Setup.TelegramChatID != "4242" || *cfg.Setup.StorageBackend != "sqlite" {
		t.Fatalf("activated config=%+v %v", cfg, err)
	}
	questions, err := config.LoadPendingTournaments(filepath.Join(root, "pending_tournaments.json"))
	if err != nil || len(questions) != 1 || questions[0].Tournament.TournamentID != "9000000102" {
		t.Fatalf("questions=%+v %v", questions, err)
	}
	conn.Close()
	target.close()
	target = startTransferTarget(t, root)
	status, reply, err := transport.ConnectTransfer(target.info, transport.TransferRequest{Kind: transport.TransferStatus, ID: id}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer status.Close()
	if reply.Phase != transport.TransferPhaseActivated {
		t.Fatalf("lost ACK receipt=%+v", reply)
	}
	// A later intentional movement makes rollback unsafe; it must survive refusal.
	if err := target.store.AppendLedgerEntry("deposit", 1, time.Now(), "money:after-activation"); err != nil {
		t.Fatal(err)
	}
	reply = transferExchange(t, status, transport.TransferRequest{Kind: transport.TransferRollback, ID: id})
	if reply.Error == "" {
		t.Fatal("rollback erased history created after activation")
	}
	after, err := target.store.ExportSnapshot()
	if err != nil || len(after.Ledger) != len(before.Ledger)+1 {
		t.Fatalf("foreign rows lost: %+v %v", after, err)
	}
}

func TestDeploymentTransferRefusesBusyCollectorAndOperationalConflicts(t *testing.T) {
	for _, conflict := range []string{"collector", "chat", "questions", "collector-edits"} {
		t.Run(conflict, func(t *testing.T) {
			root := t.TempDir()
			target := startTransferTarget(t, root)
			body := deploymentBody(t)
			id := "owned-" + conflict
			if conflict == "collector" {
				peer, err := transport.ConnectCollector(target.info, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer peer.Close()
				peer.Link.Uploads <- transport.Upload{Status: &transport.CollectorStatus{}, ClientClock: time.Now()}
				deadline := time.Now().Add(3 * time.Second)
				for {
					connected := false
					select {
					case <-target.link.CollectorDone():
					default:
						connected = true
					}
					if connected {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("collector never became current")
					}
					time.Sleep(time.Millisecond)
				}
				sum := sha256.Sum256(body)
				session, reply, err := transport.ConnectTransfer(target.info, transport.TransferRequest{Kind: transport.TransferBegin, ID: id, TotalBytes: int64(len(body)), SHA256: fmt.Sprintf("%x", sum)}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer session.Close()
				if !strings.Contains(reply.Error, "collector") {
					t.Fatalf("busy collector not refused: %+v", reply)
				}
				select {
				case <-peer.Done:
					t.Fatal("transfer replaced existing collector")
				default:
				}
				return
			}
			if conflict == "chat" {
				cfg, _ := config.LoadConfig(target.worker.configPath)
				chat := "9999"
				cfg.Setup.TelegramChatID = &chat
				if err := config.SaveConfig(target.worker.configPath, cfg); err != nil {
					t.Fatal(err)
				}
			}
			if conflict == "questions" {
				if err := config.SavePendingTournaments(filepath.Join(root, "pending_tournaments.json"), []config.PendingTournament{{Tournament: parsers.Tournament{TournamentID: "9000000199"}}}); err != nil {
					t.Fatal(err)
				}
			}
			if conflict == "collector-edits" {
				if err := config.SaveCollectorSettings(filepath.Join(root, "collector_settings.json"), config.CollectorSettingsState{Pending: &config.CollectorChange{ID: "pending-edit"}}); err != nil {
					t.Fatal(err)
				}
			}
			sum := sha256.Sum256(body)
			conn, reply, err := transport.ConnectTransfer(target.info, transport.TransferRequest{Kind: transport.TransferBegin, ID: id, TotalBytes: int64(len(body)), SHA256: fmt.Sprintf("%x", sum)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if conflict == "chat" {
				if reply.Error != "" {
					t.Fatal(reply.Error)
				}
				sendDeployment(t, conn, id, body, 0)
				reply = transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferVerify, ID: id})
			}
			if reply.Error == "" {
				t.Fatalf("operational conflict accepted: %s %+v", conflict, reply)
			}
			snapshot, err := target.store.ExportSnapshot()
			if err != nil || !snapshot.IsEmpty() {
				t.Fatalf("refusal changed destination: %+v %v", snapshot, err)
			}
		})
	}
}

func TestDeploymentTransferRollbackAndDurableAckFailures(t *testing.T) {
	for _, phase := range []string{"receiving", "verified", "applying-settings", "activated"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			target := startTransferTarget(t, root)
			body := deploymentBody(t)
			id := "owned-rollback-" + phase
			conn, _ := connectDeployment(t, target, id, body)
			if phase != "receiving" {
				sendDeployment(t, conn, id, body, 0)
				reply := transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferVerify, ID: id})
				if reply.Error != "" {
					t.Fatal(reply.Error)
				}
				if phase == "applying-settings" {
					// A failed pending-queue replacement occurs after settings commit.
					blocker := filepath.Join(root, "pending_tournaments.json.tmp")
					if err := os.Mkdir(blocker, 0700); err != nil {
						t.Fatal(err)
					}
					reply = transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferActivate, ID: id})
					if reply.Error == "" || reply.Phase != transport.TransferPhaseApplyingSettings {
						t.Fatalf("injected commit failure=%+v", reply)
					}
					if err := os.Remove(blocker); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "activated" {
					reply = transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferActivate, ID: id})
					if reply.Error != "" {
						t.Fatal(reply.Error)
					}
				}
			} else {
				blocker := filepath.Join(root, "deployment_transfer.json.tmp")
				if err := os.Mkdir(blocker, 0700); err != nil {
					t.Fatal(err)
				}
				reply := transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferChunk, ID: id, Data: body[:257]})
				if reply.Error == "" || reply.NextOffset != 0 {
					t.Fatalf("bytes were acknowledged without durable offset: %+v", reply)
				}
				if err := os.Remove(blocker); err != nil {
					t.Fatal(err)
				}
			}
			reply := transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferRollback, ID: id})
			if reply.Error != "" || reply.Phase != transport.TransferPhaseRolledBack {
				t.Fatalf("rollback=%+v", reply)
			}
			snapshot, err := target.store.ExportSnapshot()
			if err != nil || !snapshot.IsEmpty() {
				t.Fatalf("rollback rows=%+v %v", snapshot, err)
			}
			cfg, err := config.LoadConfig(target.worker.configPath)
			if err != nil || cfg.Setup.TelegramChatID != nil || cfg.Alerts.DailyLossCents != nil || *cfg.Setup.StorageBackend != "sqlite" {
				t.Fatalf("rollback config=%+v %v", cfg, err)
			}
			questions, err := config.LoadPendingTournaments(filepath.Join(root, "pending_tournaments.json"))
			if err != nil || len(questions) != 0 {
				t.Fatalf("rollback questions=%+v %v", questions, err)
			}
			// An intentional later write remains usable through the original open handle.
			if err := target.store.AppendLedgerEntry("deposit", 1, time.Now(), "money:after-rollback"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeploymentTransferDigestAndOwnershipRefusal(t *testing.T) {
	for _, corruption := range []string{"digest", "foreign-row", "foreign-policy"} {
		t.Run(corruption, func(t *testing.T) {
			root := t.TempDir()
			target := startTransferTarget(t, root)
			body := deploymentBody(t)
			id := "owned-corrupt-" + corruption
			conn, _ := connectDeployment(t, target, id, body)
			sendDeployment(t, conn, id, body, 0)
			if corruption == "digest" {
				file, err := os.OpenFile(filepath.Join(root, "deployment_transfer.data"), os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				file.WriteAt([]byte("!"), 0)
				file.Close()
			}
			if corruption == "foreign-row" {
				if err := target.store.AppendLedgerEntry("deposit", 1, time.Now(), "money:foreign"); err != nil {
					t.Fatal(err)
				}
			}
			if corruption == "foreign-policy" {
				// Simulate an out-of-process edit, bypassing the application's mutex/guard.
				data, err := os.ReadFile(target.worker.configPath)
				if err != nil {
					t.Fatal(err)
				}
				var cfg config.Config
				if err := json.Unmarshal(data, &cfg); err != nil {
					t.Fatal(err)
				}
				n := money.Cents(123)
				cfg.Alerts.DailyLossCents = &n
				data, err = json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target.worker.configPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			reply := transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferVerify, ID: id})
			if reply.Error == "" {
				t.Fatal("corrupt/foreign destination verified")
			}
			if corruption != "digest" {
				reply = transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferRollback, ID: id})
				if reply.Error == "" {
					t.Fatal("rollback removed unowned destination state")
				}
			}
			snapshot, err := target.store.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if corruption == "foreign-row" && (len(snapshot.Ledger) != 1 || snapshot.Ledger[0].AmountCents != 1) {
				t.Fatalf("foreign movement lost: %+v", snapshot)
			}
		})
	}
}

func TestDeploymentRollbackPreservesNewPendingCash(t *testing.T) {
	root := t.TempDir()
	target := startTransferTarget(t, root)
	body := deploymentBody(t)
	id := "owned-new-pending-cash"
	conn, _ := connectDeployment(t, target, id, body)
	sendDeployment(t, conn, id, body, 0)
	for _, kind := range []string{transport.TransferVerify, transport.TransferActivate} {
		reply := transferExchange(t, conn, transport.TransferRequest{Kind: kind, ID: id})
		if reply.Error != "" {
			t.Fatal(reply.Error)
		}
	}
	peer, err := transport.ConnectCollector(target.info, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hand_history", "nlhe_cash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	peer.Link.Uploads <- transport.Upload{FileID: "new-after-activation.txt", Type: "cash", Bytes: data, ScreenName: "AnonHero01", ClientClock: time.Now()}
	deadline := time.Now().Add(3 * time.Second)
	for {
		pending, err := config.LoadPendingCash(filepath.Join(root, "pending_cash.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new cash was not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	peer.Close()
	deadline = time.Now().Add(time.Second)
	for {
		reply := transferExchange(t, conn, transport.TransferRequest{Kind: transport.TransferRollback, ID: id})
		if strings.Contains(reply.Error, "cash sessions are pending") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending cash rollback refusal=%+v", reply)
		}
		time.Sleep(time.Millisecond)
	}
	snapshot, err := target.store.ExportSnapshot()
	if err != nil || len(snapshot.Ledger) != 3 {
		t.Fatalf("existing transfer rows lost: %+v %v", snapshot, err)
	}
	pending, err := config.LoadPendingCash(filepath.Join(root, "pending_cash.json"))
	if err != nil || len(pending) != 1 || pending[0].Session.Hands != 4 {
		t.Fatalf("new pending cash lost: %+v %v", pending, err)
	}
}
