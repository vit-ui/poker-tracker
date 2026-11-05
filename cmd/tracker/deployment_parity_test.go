package main

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/client"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/server"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

type deploymentPipeline struct {
	t                           *testing.T
	root, collectorPath, hh, ts string
	link, peer                  *transport.Link
	worker                      *server.Server
	store                       *sqlitestore.Store
	alerts                      chan string
	collectorStop               chan struct{}
}

// Both paths use the real watcher and collector. Split mode has separate PC and
// server configuration files as well as an authenticated, pinned TLS connection.
func newDeploymentPipeline(t *testing.T, network, watch bool) *deploymentPipeline {
	t.Helper()
	root := t.TempDir()
	hh, ts := filepath.Join(root, "hands"), filepath.Join(root, "summaries")
	for _, base := range []string{hh, ts} {
		if err := os.MkdirAll(filepath.Join(base, e2eHero), 0700); err != nil {
			t.Fatal(err)
		}
	}
	name := e2eHero
	threshold := .01
	wins := "wins"
	cfg := &config.Config{Alerts: config.Alerts{SessionLossBuyins: &threshold}, Notifications: config.Notifications{TournamentSummary: &wins}}
	serverPath, collectorPath := filepath.Join(root, "server.json"), filepath.Join(root, "collector.json")
	collectorCfg := &config.Config{Setup: config.Setup{PokerStarsScreenName: &name}, Paths: config.Paths{HandHistory: &hh, TournamentSummary: &ts}}
	if !network {
		collectorPath = serverPath
		cfg.Setup = collectorCfg.Setup
		cfg.Paths = collectorCfg.Paths
	}
	if err := config.SaveConfig(serverPath, cfg); err != nil {
		t.Fatal(err)
	}
	if network {
		if err := config.SaveConfig(collectorPath, collectorCfg); err != nil {
			t.Fatal(err)
		}
	}
	store, err := sqlitestore.NewStore(filepath.Join(root, "tracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	link := transport.NewLink()
	p := &deploymentPipeline{t: t, root: root, collectorPath: collectorPath, hh: hh, ts: ts, link: link, peer: link, store: store, alerts: make(chan string, 100), collectorStop: make(chan struct{})}
	var connection *transport.CollectorConnection
	var listener net.Listener
	var transportDone chan struct{}
	if network {
		cert, key, fp, err := transport.GenerateServerCert()
		if err != nil {
			t.Fatal(err)
		}
		listener, err = transport.Listen("127.0.0.1:0", cert, key)
		if err != nil {
			t.Fatal(err)
		}
		transportDone = make(chan struct{})
		go func() { defer close(transportDone); transport.Serve(listener, "parity-token", link) }()
		host, portText, err := net.SplitHostPort(listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			t.Fatal(err)
		}
		connection, err = transport.ConnectCollector(transport.ConnInfo{Host: host, Port: port, Token: "parity-token", Fingerprint: fp}, nil)
		if err != nil {
			t.Fatal(err)
		}
		p.peer = connection.Link
	}
	p.worker, err = server.NewServer(store, serverPath, link, p.alerts)
	if err != nil {
		t.Fatal(err)
	}
	p.worker.QuietPeriod = 100 * time.Millisecond
	workerDone := make(chan error, 1)
	go func() { workerDone <- p.worker.Run() }()
	collectorDone := make(chan error, 1)
	if watch {
		collector := client.NewCollector(p.peer, client.NewEnforcer(filepath.Join(root, "lockfile.json")), nil)
		collector.Stop = p.collectorStop
		var dead <-chan struct{}
		if connection != nil {
			dead = connection.Done
		}
		go func() { collectorDone <- collector.Serve(collectorPath, dead) }()
	}
	t.Cleanup(func() {
		if watch {
			close(p.collectorStop)
			select {
			case err := <-collectorDone:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("collector shutdown blocked")
				return
			}
		}
		if connection != nil {
			connection.Drain()
			listener.Close()
			<-transportDone
		}
		close(link.Uploads)
		select {
		case err := <-workerDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker shutdown blocked")
			return
		}
		close(p.alerts)
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if watch {
		p.wait("collector watcher confirmation", func() bool {
			state, err := config.LoadCollectorSettings(filepath.Join(root, "collector_settings.json"))
			return err == nil && state.Watching && state.Confirmed != nil
		})
	}
	return p
}

func (p *deploymentPipeline) wait(label string, check func() bool) {
	p.t.Helper()
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.t.Fatal("timed out waiting for " + label)
}

func (p *deploymentPipeline) operation(apply func(storage.Store) storage.OperationResult) storage.OperationResult {
	p.t.Helper()
	reply := make(chan storage.OperationResult, 1)
	select {
	case p.worker.Operations <- storage.Operation{Apply: apply, Reply: reply}:
	case <-time.After(5 * time.Second):
		p.t.Fatal("worker operation blocked")
	}
	select {
	case result := <-reply:
		if result.Err != nil {
			p.t.Fatal(result.Err)
		}
		return result
	case <-time.After(5 * time.Second):
		p.t.Fatal("worker reply blocked")
		return storage.OperationResult{}
	}
}

func (p *deploymentPipeline) snapshot() storage.Snapshot {
	p.t.Helper()
	result := p.operation(func(s storage.Store) storage.OperationResult {
		snap, err := s.ExportSnapshot()
		return storage.OperationResult{Snapshot: &snap, Err: err}
	})
	return *result.Snapshot
}

func (p *deploymentPipeline) drop(sub, name string) {
	p.t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", sub, name))
	if err != nil {
		p.t.Fatal(err)
	}
	base := p.ts
	if sub == "hand_history" {
		base = p.hh
	}
	if err := os.WriteFile(filepath.Join(base, e2eHero, name), data, 0600); err != nil {
		p.t.Fatal(err)
	}
}

func (p *deploymentPipeline) messages() []string {
	var messages []string
	for {
		select {
		case msg := <-p.alerts:
			messages = append(messages, msg)
		default:
			return messages
		}
	}
}

func TestDeploymentChannelsAndTLSRecordAndAlertParity(t *testing.T) {
	var reference storage.Snapshot
	var referenceAlerts []string
	for _, network := range []bool{false, true} {
		t.Run(map[bool]string{false: "channels", true: "TLS"}[network], func(t *testing.T) {
			p := newDeploymentPipeline(t, network, true)
			p.operation(func(s storage.Store) storage.OperationResult {
				return storage.OperationResult{Err: s.AppendLedgerEntry("deposit", 100, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), "money:parity:deposit")}
			})
			p.drop("hand_history", "nlhe_cash.txt")
			p.wait("cash session", func() bool { return len(p.snapshot().CashSessions) == 1 })
			p.drop("tournament_summary", "spingo_won_T9000000007.txt")
			p.wait("Spin & Go", func() bool { return len(p.snapshot().Tournaments) == 1 })
			p.drop("hand_history", "draw27_pko_T9000000005.txt")
			p.wait("partial knockout row", func() bool { return len(p.snapshot().Tournaments) == 2 })
			p.drop("tournament_summary", "draw27_pko_T9000000005.txt")
			p.wait("knockout summary", func() bool { return len(p.snapshot().Ledger) == 4 })
			snapshot := p.snapshot()
			messages := p.messages()
			if len(snapshot.CashSessions) != 1 || len(snapshot.Tournaments) != 2 || len(snapshot.Ledger) != 4 {
				t.Fatalf("snapshot=%+v", snapshot)
			}
			wantAmounts := []money.Cents{100, -3, 1000, -330}
			wantTotals := []money.Cents{100, 97, 1097, 767}
			for i, row := range snapshot.Ledger {
				if row.AmountCents != wantAmounts[i] || row.RunningTotalCents != wantTotals[i] || row.SessionID == "" {
					t.Fatalf("ledger row %d=%+v", i, row)
				}
			}
			if snapshot.CashSessions[0].Hands != 4 || snapshot.Tournaments[1].Type != "mtt-ko" || snapshot.Tournaments[1].Hands == nil || *snapshot.Tournaments[1].Hands == 0 {
				t.Fatalf("hand/tournament details=%+v", snapshot)
			}
			result := p.operation(func(s storage.Store) storage.OperationResult {
				value, err := s.GetCurrentBankroll()
				return storage.OperationResult{BankrollCents: value, Err: err}
			})
			if result.BankrollCents != 767 {
				t.Fatalf("bankroll=%v, want 767 cents", result.BankrollCents)
			}
			for _, fragment := range []string{"⚠️ Session loss alert", "✅ Cash Session", "🎰 Spin & Go"} {
				count := 0
				for _, msg := range messages {
					if strings.Contains(msg, fragment) {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("%q count=%d; alerts=%v", fragment, count, messages)
				}
			}
			for _, msg := range messages {
				if strings.Contains(msg, "🏆 MTT") {
					t.Fatalf("wins-only tournament notifications sent a loss: %v", messages)
				}
			}
			if _, err := os.Stat(filepath.Join(p.root, "lockfile.json")); !os.IsNotExist(err) {
				t.Fatalf("alert-only settings created lockfile: %v", err)
			}
			if network {
				if !reflect.DeepEqual(snapshot, reference) {
					t.Fatalf("TLS snapshot differs from channels\nTLS=%+v\nchannels=%+v", snapshot, reference)
				}
				if !reflect.DeepEqual(messages, referenceAlerts) {
					t.Fatalf("TLS alerts=%v, channels=%v", messages, referenceAlerts)
				}
			} else {
				reference = snapshot
				referenceAlerts = messages
			}
		})
	}
}

// Controlled collector clocks travel in import-only messages over both actual
// links. No live/status upload may establish the timezone for this test.
func TestDeploymentScanOnlyClockDateWindows(t *testing.T) {
	for _, network := range []bool{false, true} {
		t.Run(map[bool]string{false: "channels", true: "TLS"}[network], func(t *testing.T) {
			p := newDeploymentPipeline(t, network, false)
			instant := time.Now()
			_, hostOffset := instant.Zone()
			offset := (12 - instant.UTC().Hour()) * 60 * 60
			if offset == hostOffset {
				offset += 6 * 60 * 60
			}
			zone := time.FixedZone("collector", offset)
			clock := instant.In(zone)
			cfg := &config.Config{}
			// Place the collector at noon (or 18:00), safely away from rollover.
			// Expected stored digits are independent of the shared clock helper.
			start := time.Date(clock.Year(), clock.Month(), clock.Day(), 6, 0, 0, 0, time.UTC)
			fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "tournament_summary", "spingo_won_T9000000007.txt"))
			if err != nil {
				t.Fatal(err)
			}
			p.peer.Uploads <- transport.Upload{Type: transport.ImportStart, ClientClock: clock}
			for i, at := range []time.Time{start.Add(-time.Minute), start.Add(time.Minute)} {
				original := "2026/07/19 21:57:37"
				data := strings.Replace(string(fixture), original, at.Format("2006/01/02 15:04:05"), 1)
				id := []string{"9000000021", "9000000022"}[i]
				data = strings.ReplaceAll(data, "9000000007", id)
				p.peer.Uploads <- transport.Upload{FileID: id + ".txt", Type: "tournament", Bytes: []byte(data), ScreenName: e2eHero, Import: true, ClientClock: clock}
			}
			p.peer.Uploads <- transport.Upload{Type: transport.ImportDone, ClientClock: clock}
			p.wait("scan rows", func() bool { return len(p.snapshot().Ledger) == 2 })
			var messages []string
			p.wait("scan report", func() bool { messages = append(messages, p.messages()...); return len(messages) > 0 })
			if len(messages) != 1 || !strings.Contains(messages[0], "Scan complete — recorded 0 cash session(s) and 2 tournament(s)") {
				t.Fatalf("scan alerts=%v", messages)
			}
			now := p.worker.ClientNow()
			_, gotOffset := now.Zone()
			if gotOffset != offset {
				t.Fatalf("collector zone offset=%d, want %d", gotOffset, offset)
			}
			boundary := hardstop.PokerDayStart(cfg, now)
			if boundary.Location() != time.UTC || !boundary.Equal(start) {
				t.Fatalf("stored boundary=%v, want collector digits %v in UTC", boundary, start)
			}
			rows, err := p.store.GetLedgerEntriesSince(boundary)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].SessionID != "9000000022" || rows[0].AmountCents != 1000 {
				t.Fatalf("player poker-day rows=%+v, boundary=%v", rows, boundary)
			}
			tournaments, err := p.store.GetTournamentsSince(boundary)
			if err != nil || len(tournaments) != 1 || tournaments[0].TournamentID != "9000000022" {
				t.Fatalf("player poker-day tournaments=%+v, %v", tournaments, err)
			}
			if pending, err := config.LoadPendingCash(filepath.Join(p.root, "pending_cash.json")); err != nil || len(pending) != 0 {
				t.Fatalf("scan armed pending cash: %+v %v", pending, err)
			}
		})
	}
}
