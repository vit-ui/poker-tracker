package server

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func updateOperation(t *testing.T, worker *Server, request *config.UpdateRequest) storage.OperationResult {
	t.Helper()
	reply := make(chan storage.OperationResult, 1)
	worker.Operations <- storage.Operation{Updates: true, UpdateRequest: request, Reply: reply}
	return migrationReply(t, reply)
}

func TestUpdateRequestSurvivesRestartWithoutInventingCompletion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	if err := config.SaveConfig(path, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.NewStore(filepath.Join(root, "tracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	worker, link, stop := runRecoveredWorker(t, store, path, time.Hour)
	request := config.UpdateRequest{ID: "telegram-update:1", Version: "v4.9"}
	result := updateOperation(t, worker, &request)
	if result.Err != nil || result.Updates.Request == nil || *result.Updates.Request != request {
		t.Fatalf("request not saved: %+v", result)
	}
	cmd := migrationCommand(t, link, transport.CommandUpdate)
	if cmd.Update == nil || *cmd.Update != request {
		t.Fatalf("wrong command: %+v", cmd)
	}
	// A queued send is not evidence that the PC has updated.
	if result.Updates.Report.Job != nil {
		t.Fatal("send invented completion")
	}
	stop()
	worker, link, stop = runRecoveredWorker(t, store, path, time.Hour)
	defer stop()
	cmd = migrationCommand(t, link, transport.CommandUpdate)
	if cmd.RequestID != request.ID {
		t.Fatal("restart changed request identity")
	}
	job := config.UpdateJob{Request: request, State: config.UpdateWaiting, Version: "v4.8"}
	waiting := job
	link.Uploads <- transport.Upload{UpdateReply: &transport.UpdateReply{RequestID: request.ID, Report: config.UpdateReport{Version: "v4.8", Job: &waiting}}}
	// Synchronize with the real worker using an ordered observation upload.
	job.State = config.UpdateSucceeded
	job.Version = "v4.9"
	link.Uploads <- transport.Upload{Update: &config.UpdateReport{Version: "v4.9", Job: &job}}
	deadline := time.Now().Add(3 * time.Second)
	for {
		state := updateOperation(t, worker, nil).Updates
		if state.Report.Job != nil && state.Report.Job.State == config.UpdateSucceeded {
			if state.Report.Version != "v4.9" {
				t.Fatalf("wrong installed version: %+v", state)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("collector result not observed")
		}
		time.Sleep(time.Millisecond)
	}
	state, err := config.LoadCollectorUpdates(filepath.Join(root, "collector_updates.json"))
	if err != nil || state.Report.Job == nil || state.Report.Job.State != config.UpdateSucceeded {
		t.Fatalf("result not durable: %+v %v", state, err)
	}
}

func TestCollectorUpdateQueueDecisions(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		complete    bool
	}{
		{"no acknowledgement", "", false}, {"waiting", config.UpdateWaiting, false}, {"running", config.UpdateRunning, false}, {"confirmed", config.UpdateSucceeded, true}, {"failed", config.UpdateFailed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := collectorUpdateQueue{path: filepath.Join(t.TempDir(), "collector_updates.json")}
			r := config.UpdateRequest{ID: "one", Version: "v4.9"}
			if err := q.accept(r); err != nil {
				t.Fatal(err)
			}
			if tc.state != "" {
				if err := q.observe(config.UpdateReport{Version: "v4.8", Job: &config.UpdateJob{Request: r, State: tc.state, Version: "v4.8"}}); err != nil {
					t.Fatal(err)
				}
			}
			if q.completed() != tc.complete {
				t.Fatalf("complete=%v", q.completed())
			}
			if (q.command() == nil) != tc.complete {
				t.Fatalf("wrong delivery decision: %+v", q.state)
			}
			q.sent = true
			if q.command() != nil {
				t.Fatal("repeated command without reconnect")
			}
			before := q.state
			if err := q.acknowledge(transport.UpdateReply{RequestID: "stale", Error: "stale"}); err != nil {
				t.Fatal(err)
			}
			if q.state.Request.ID != before.Request.ID || q.state.Error != "" {
				t.Fatal("stale receipt changed active request")
			}
		})
	}
}

func TestUpdateQueueSaveFailureDoesNotConfirmAcceptance(t *testing.T) {
	worker, _, _ := migrationWorker(t)
	path := filepath.Join(filepath.Dir(worker.configPath), "collector_updates.json.tmp")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	result := updateOperation(t, worker, &config.UpdateRequest{ID: "unsaved", Version: "v4.9"})
	if result.Err == nil || result.Updates.Request != nil {
		t.Fatalf("unsaved request accepted: %+v", result)
	}
}

func TestOfflineUpdateDeliveredOverPinnedTLSWithSameIdentity(t *testing.T) {
	worker, link, _ := migrationWorker(t)
	cert, key, fp, err := transport.GenerateServerCert()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.Listen("127.0.0.1:0", cert, key)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() { defer close(served); transport.Serve(ln, "update-test", link) }()
	defer func() { ln.Close(); <-served }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case <-link.CollectorDone():
			goto offline
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("transport not offline")
		}
		time.Sleep(time.Millisecond)
	}
offline:
	r := config.UpdateRequest{ID: "offline-update", Version: "v4.9"}
	result := updateOperation(t, worker, &r)
	if result.Err != nil || result.CollectorOnline {
		t.Fatalf("offline acceptance: %+v", result)
	}
	address := ln.Addr().(*net.TCPAddr)
	info := transport.ConnInfo{Host: "127.0.0.1", Port: address.Port, Token: "update-test", Fingerprint: fp}
	for attempt := 0; attempt < 2; attempt++ {
		peer, err := transport.ConnectCollector(info, nil)
		if err != nil {
			t.Fatal(err)
		}
		delivered := false
		for !delivered {
			select {
			case command := <-peer.Link.Commands:
				if command.Kind != transport.CommandUpdate {
					continue
				}
				if command.RequestID != r.ID || command.Update == nil || *command.Update != r {
					t.Fatalf("changed request after reconnect: %+v", command)
				}
				delivered = true
			case <-time.After(3 * time.Second):
				peer.Close()
				t.Fatal("offline update was not delivered")
			}
		}
		if attempt == 1 {
			job := config.UpdateJob{Request: r, State: config.UpdateSucceeded, Version: "v4.9"}
			peer.Link.Uploads <- transport.Upload{UpdateReply: &transport.UpdateReply{RequestID: r.ID, Report: config.UpdateReport{Version: "v4.9", Job: &job}}}
			deadline = time.Now().Add(3 * time.Second)
			for {
				state := updateOperation(t, worker, nil).Updates
				if state.Report.Job != nil && state.Report.Job.State == config.UpdateSucceeded {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("TLS result not observed")
				}
				time.Sleep(time.Millisecond)
			}
		}
		peer.Close()
	}
}

func TestPendingUpdateRefusesConfigurationRestartWithoutPausingCollector(t *testing.T) {
	worker, link, _ := migrationWorker(t)
	if result := updateOperation(t, worker, &config.UpdateRequest{ID: "pending", Version: "v4.9"}); result.Err != nil {
		t.Fatal(result.Err)
	}
	migrationCommand(t, link, transport.CommandUpdate)
	for _, migration := range []bool{false, true} {
		reply := make(chan storage.OperationResult, 1)
		worker.Operations <- storage.Operation{PrepareRestart: !migration, PrepareMigration: migration, Reply: reply, Apply: func(storage.Store) storage.OperationResult {
			t.Error("handover applied while an update was pending")
			return storage.OperationResult{}
		}}
		if result := migrationReply(t, reply); result.Err == nil {
			t.Fatal("pending update did not block handover")
		}
	}
	select {
	case command := <-link.Commands:
		t.Fatalf("handover paused collector: %+v", command)
	default:
	}
}

func TestTerminalUpdateSurvivesDelayedProgress(t *testing.T) {
	for _, terminal := range []string{config.UpdateSucceeded, config.UpdateFailed} {
		for _, pending := range []string{config.UpdateQueued, config.UpdateWaiting, config.UpdateRunning} {
			for _, acknowledgement := range []bool{false, true} {
				t.Run(terminal+"/"+pending+map[bool]string{false: "/report", true: "/ack"}[acknowledgement], func(t *testing.T) {
					root := t.TempDir()
					q := collectorUpdateQueue{path: filepath.Join(root, "collector_updates.json")}
					request := config.UpdateRequest{ID: "request", Version: "v4.11"}
					if err := q.accept(request); err != nil {
						t.Fatal(err)
					}
					job := config.UpdateJob{Request: request, State: terminal, Version: "v4.10"}
					if terminal == config.UpdateSucceeded {
						job.Version = "v4.11"
					} else {
						job.Error = "download failed"
					}
					report := config.UpdateReport{Version: job.Version, Job: &job}
					if err := q.observe(report); err != nil {
						t.Fatal(err)
					}
					delayed := config.UpdateReport{Version: "v4.10", Job: &config.UpdateJob{Request: request, State: pending, Version: "v4.10"}}
					var err error
					if acknowledgement {
						err = q.acknowledge(transport.UpdateReply{RequestID: request.ID, Report: delayed})
					} else {
						err = q.observe(delayed)
					}
					if err != nil {
						t.Fatal(err)
					}
					saved, err := config.LoadCollectorUpdates(q.path)
					if err != nil || saved.Report.Job == nil || *saved.Report.Job != job || saved.Report.Version != report.Version {
						t.Fatalf("terminal result lost: %+v, %v", saved, err)
					}
					if err := config.CheckUpdatesIdle(root); err != nil {
						t.Fatal(err)
					}
					next := config.UpdateRequest{ID: "next", Version: "v4.11"}
					if err := q.accept(next); err != nil {
						t.Fatal(err)
					}
					delayed.Job.Request = next
					if err := q.observe(delayed); err != nil {
						t.Fatal(err)
					}
					if q.state.Report.Job.Request != next || !q.state.Report.Job.Pending() {
						t.Fatal("new request progress was ignored")
					}
				})
			}
		}
	}
}
