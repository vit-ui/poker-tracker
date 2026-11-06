package client

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/server"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

type closurePipeline struct {
	t             *testing.T
	link          *transport.Link
	peer          *transport.Link
	info          transport.ConnInfo
	connection    *transport.CollectorConnection
	listener      net.Listener
	served        chan struct{}
	collectorStop chan struct{}
	collectorDone chan error
	worker        *server.Server
	workerDone    chan error
	configPath    string
	collectorPath string
	lockPath      string
}

func closureWait(t *testing.T, label string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out: " + label)
}

func newClosurePipeline(t *testing.T, network bool, root string, store *sqlitestore.Store) *closurePipeline {
	t.Helper()
	p := &closurePipeline{t: t, link: transport.NewLink(), configPath: filepath.Join(root, "server.json"), collectorPath: filepath.Join(root, "pc", "config.json"), lockPath: filepath.Join(root, "pc", "lockfile.json")}
	p.peer = p.link
	if network {
		cert, key, fp, err := transport.GenerateServerCert()
		if err != nil {
			t.Fatal(err)
		}
		p.listener, err = transport.Listen("127.0.0.1:0", cert, key)
		if err != nil {
			t.Fatal(err)
		}
		host, portText, _ := net.SplitHostPort(p.listener.Addr().String())
		port, _ := strconv.Atoi(portText)
		p.info = transport.ConnInfo{Host: host, Port: port, Token: "closure-token", Fingerprint: fp}
		p.served = make(chan struct{})
		go func() { defer close(p.served); transport.Serve(p.listener, p.info.Token, p.link) }()
	}
	var err error
	p.worker, err = server.NewServer(store, p.configPath, p.link, make(chan string, 100))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *closurePipeline) startCollector() {
	p.t.Helper()
	var dead <-chan struct{}
	if p.listener != nil {
		var err error
		p.connection, err = transport.ConnectCollector(p.info, nil)
		if err != nil {
			p.t.Fatal(err)
		}
		p.peer, dead = p.connection.Link, p.connection.Done
	}
	collector := NewCollector(p.peer, NewEnforcer(p.lockPath), nil)
	p.collectorStop, p.collectorDone = make(chan struct{}), make(chan error, 1)
	collector.Stop = p.collectorStop
	go func() { p.collectorDone <- collector.Run(make(chan watcher.FileEvent), p.collectorPath, dead) }()
}

func (p *closurePipeline) stopCollector(disconnect bool) {
	p.t.Helper()
	if disconnect && p.connection != nil {
		p.connection.Close()
	} else {
		close(p.collectorStop)
	}
	select {
	case err := <-p.collectorDone:
		if err != nil {
			p.t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		p.t.Fatal("collector blocked")
	}
	if p.connection != nil {
		p.connection.Drain()
		p.connection = nil
	}
}

func (p *closurePipeline) startWorker() {
	p.workerDone = make(chan error, 1)
	go func() { p.workerDone <- p.worker.Run() }()
}

func (p *closurePipeline) stop() {
	p.t.Helper()
	p.stopCollector(false)
	if p.listener != nil {
		p.listener.Close()
		<-p.served
	}
	close(p.link.Uploads)
	select {
	case err := <-p.workerDone:
		if err != nil {
			p.t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		p.t.Fatal("worker blocked")
	}
}

func (p *closurePipeline) delivery() config.ClosureEvent {
	p.t.Helper()
	select {
	case event := <-p.worker.ClosureNotifications.Deliveries:
		return event
	case <-time.After(5 * time.Second):
		p.t.Fatal("closure delivery missing")
		return config.ClosureEvent{}
	}
}

func (p *closurePipeline) barrier() {
	p.t.Helper()
	reply := make(chan storage.OperationResult, 1)
	p.worker.Operations <- storage.Operation{Reply: reply, Apply: func(storage.Store) storage.OperationResult { return storage.OperationResult{} }}
	if result := <-reply; result.Err != nil {
		p.t.Fatal(result.Err)
	}
}

func TestClosureDeliveryRecoversAcknowledgementsAndRestarts(t *testing.T) {
	for _, network := range []bool{false, true} {
		name := "channel"
		if network {
			name = "TLS"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			serverPath, pcPath := filepath.Join(root, "server.json"), filepath.Join(root, "pc", "config.json")
			if err := config.SaveConfig(serverPath, &config.Config{}); err != nil {
				t.Fatal(err)
			}
			// SaveConfig requires its directory to exist.
			pcRoot := filepath.Dir(pcPath)
			if err := makeClosurePC(pcRoot, pcPath); err != nil {
				t.Fatal(err)
			}
			now := time.Now().In(time.FixedZone("player", -3*3600))
			lock := config.LockFile{TriggerID: "original-lock", TriggeredAt: now.Add(-time.Hour), ExpiresAt: now.Add(48 * time.Hour), DurationHours: 48, Reason: "manual"}
			lockPath := filepath.Join(pcRoot, "lockfile.json")
			if err := config.SaveLock(lockPath, &lock); err != nil {
				t.Fatal(err)
			}
			event := config.ClosureEvent{ID: "closure:original", ClosedAt: now, Lock: lock}
			collectorJournal := filepath.Join(pcRoot, "collector_closures.json")
			serverJournal := filepath.Join(root, "closure_notifications.json")
			if err := config.QueueClosures(collectorJournal, []config.ClosureEvent{event}); err != nil {
				t.Fatal(err)
			}
			store, err := sqlitestore.NewStore(filepath.Join(root, "history.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			p := newClosurePipeline(t, network, root, store)
			p.startCollector()
			// Lose the connection before a server worker can acknowledge anything.
			closureWait(t, "accepted event before disconnect", func() bool { return len(p.link.Uploads) >= 2 })
			p.stopCollector(true)
			p.startWorker()
			first := p.delivery()
			if first.ID != event.ID || !first.Lock.ExpiresAt.Equal(lock.ExpiresAt) {
				t.Fatal(first)
			}
			entries, err := config.LoadClosureNotifications(serverJournal)
			if err != nil || entries[event.ID].Delivered || entries[event.ID].Event.ID != event.ID {
				t.Fatal(entries, err)
			}
			// A failed bot send must remain pending, even after collector acknowledgement.
			p.worker.ClosureNotifications.Results <- bot.ClosureResult{ID: event.ID, Err: errors.New("Telegram unavailable")}
			p.barrier()
			p.startCollector()
			closureWait(t, "durable collector acknowledgement", func() bool {
				entries, err := config.LoadClosureNotifications(collectorJournal)
				return err == nil && len(entries) == 0
			})
			// Reconnect is an existing retry opportunity. A failed event may retry once.
			if network {
				retry := p.delivery()
				if retry.ID != event.ID {
					t.Fatal(retry)
				}
				p.worker.ClosureNotifications.Results <- bot.ClosureResult{ID: event.ID, Err: errors.New("still unavailable")}
				p.barrier()
			}
			// The bot's existing maintenance operation is also a delivery opportunity
			// on both transports; no independent notification retry loop is needed.
			maintenanceReply := make(chan storage.OperationResult, 1)
			p.worker.Operations <- storage.Operation{RetryHardstops: true, Reply: maintenanceReply, Apply: func(storage.Store) storage.OperationResult { return storage.OperationResult{} }}
			if result := <-maintenanceReply; result.Err != nil {
				t.Fatal(result.Err)
			}
			retried := p.delivery()
			if retried.ID != event.ID {
				t.Fatal(retried)
			}
			p.worker.ClosureNotifications.Results <- bot.ClosureResult{ID: event.ID, Err: errors.New("maintenance send failed")}
			p.barrier()
			p.stop()
			// Both server and collector restart with fresh objects and the same files.
			p = newClosurePipeline(t, network, root, store)
			p.startWorker()
			p.startCollector()
			recovered := p.delivery()
			if recovered.ID != event.ID || !recovered.Lock.ExpiresAt.Equal(lock.ExpiresAt) {
				t.Fatal(recovered)
			}
			p.worker.ClosureNotifications.Results <- bot.ClosureResult{ID: event.ID}
			closureWait(t, "successful send receipt", func() bool {
				entries, err := config.LoadClosureNotifications(serverJournal)
				return err == nil && entries[event.ID].Delivered
			})
			// Replaying a delivered ID is acknowledged, while another real closure on
			// the very same lock is a distinct notification with unchanged expiry.
			second := event
			second.ID = "closure:second"
			second.ClosedAt = now.Add(time.Second)
			if err := config.QueueClosures(collectorJournal, []config.ClosureEvent{event, second}); err != nil {
				t.Fatal(err)
			}
			p.link.Commands <- transport.Command{Kind: transport.CommandReplayClosures}
			next := p.delivery()
			if next.ID != second.ID || !next.Lock.ExpiresAt.Equal(lock.ExpiresAt) {
				t.Fatal("replay or expiry changed", next)
			}
			p.worker.ClosureNotifications.Results <- bot.ClosureResult{ID: second.ID}
			closureWait(t, "second receipt", func() bool {
				entries, err := config.LoadClosureNotifications(serverJournal)
				return err == nil && len(entries) == 2 && entries[second.ID].Delivered
			})
			closureWait(t, "all events acknowledged", func() bool {
				entries, err := config.LoadClosureNotifications(collectorJournal)
				return err == nil && len(entries) == 0
			})
			saved, err := config.LoadLock(lockPath)
			if err != nil || !saved.ExpiresAt.Equal(lock.ExpiresAt) {
				t.Fatal(saved, err)
			}
			p.stop()
			p = newClosurePipeline(t, network, root, store)
			p.startWorker()
			p.startCollector()
			p.barrier()
			select {
			case duplicate := <-p.worker.ClosureNotifications.Deliveries:
				t.Fatal("delivered receipt lost after restart", duplicate)
			case <-time.After(50 * time.Millisecond):
			}
			p.stop()
		})
	}
}

func makeClosurePC(root, path string) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	return config.SaveConfig(path, &config.Config{})
}
