package server

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func settingsOperation(t *testing.T, worker *Server, change *config.CollectorChange) storage.OperationResult {
	t.Helper()
	reply := make(chan storage.OperationResult, 1)
	worker.Operations <- storage.Operation{CollectorSettings: true, CollectorChange: change, Reply: reply}
	result := migrationReply(t, reply)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	return result
}

func settingsState(t *testing.T, worker *Server, ready func(config.CollectorSettingsState) bool) config.CollectorSettingsState {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		state := settingsOperation(t, worker, nil).CollectorSettings
		if ready(state) {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("settings queue did not reach expected state: %+v", state)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCollectorQueueSurvivesRestartAndRetiresOnlyAcknowledgedRevision(t *testing.T) {
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
	name, first, second := "Hero", "first PC path", "newer PC path"
	result := settingsOperation(t, worker, &config.CollectorChange{ID: "first", Settings: config.CollectorSettings{ScreenName: &name, HandHistory: &first}})
	if result.CollectorSettings.Pending == nil || result.CollectorSettings.Pending.ID != "first" {
		t.Fatalf("not durable pending: %+v", result)
	}
	cmd := migrationCommand(t, link, transport.CommandCollectorConfig)
	if cmd.RequestID != "first" || *cmd.Settings.HandHistory != first {
		t.Fatalf("wrong queued command: %+v", cmd)
	}
	// Lost acknowledgement: restart without any reply, preserving local queue.
	stop()
	worker, link, stop = runRecoveredWorker(t, store, path, time.Hour)
	defer stop()
	cmd = migrationCommand(t, link, transport.CommandCollectorConfig)
	if cmd.RequestID != "first" {
		t.Fatal("restart changed operation identity")
	}
	settingsOperation(t, worker, &config.CollectorChange{ID: "second", Settings: config.CollectorSettings{HandHistory: &second}})
	// The already transmitted first revision cannot retire the newer pending edit.
	link.Uploads <- transport.Upload{ConfigReply: &transport.CollectorConfigReply{RequestID: "first", Settings: config.CollectorSettings{ScreenName: &name, HandHistory: &first}, Watching: true}}
	cmd = migrationCommand(t, link, transport.CommandCollectorConfig)
	if cmd.RequestID != "second" || *cmd.Settings.HandHistory != second || *cmd.Settings.ScreenName != name {
		t.Fatalf("newer edit lost: %+v", cmd)
	}
	state := settingsOperation(t, worker, nil).CollectorSettings
	if state.Pending == nil || state.Pending.ID != "second" {
		t.Fatalf("older reply retired newer edit: %+v", state)
	}
	// Unrecognized replies cannot invent success.
	link.Uploads <- transport.Upload{ConfigReply: &transport.CollectorConfigReply{RequestID: "unrecognized", Settings: config.CollectorSettings{HandHistory: &first}, Watching: true}}
	link.Uploads <- transport.Upload{ConfigReply: &transport.CollectorConfigReply{RequestID: "second", Settings: config.CollectorSettings{ScreenName: &name, HandHistory: &second}, Watching: true}}
	state = settingsState(t, worker, func(s config.CollectorSettingsState) bool { return s.Pending == nil })
	if !state.Watching || state.Confirmed == nil || *state.Confirmed.HandHistory != second {
		t.Fatalf("wrong confirmed state: %+v", state)
	}
	disk, err := config.LoadCollectorSettings(filepath.Join(root, "collector_settings.json"))
	if err != nil || !reflect.DeepEqual(disk, state) {
		t.Fatalf("ack not durable: %+v %v", disk, err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil || cfg.Paths.HandHistory != nil || cfg.Setup.PokerStarsScreenName != nil {
		t.Fatalf("PC edit changed server config: %+v %v", cfg, err)
	}
}

func TestCollectorQueueFailurePersistsUntilCorrectedAndSourceSaveFailureIsNotQueued(t *testing.T) {
	worker, link, _ := migrationWorker(t)
	name, invalid, valid := "Hero", "missing", "valid"
	settingsOperation(t, worker, &config.CollectorChange{ID: "invalid", Settings: config.CollectorSettings{ScreenName: &name, HandHistory: &invalid}})
	migrationCommand(t, link, transport.CommandCollectorConfig)
	link.Uploads <- transport.Upload{ConfigReply: &transport.CollectorConfigReply{RequestID: "invalid", Error: "folder missing"}}
	state := settingsState(t, worker, func(s config.CollectorSettingsState) bool { return s.Error != "" })
	if state.Pending == nil || state.Pending.ID != "invalid" {
		t.Fatal("failed validation discarded durable edit")
	}
	select {
	case cmd := <-link.Commands:
		t.Fatalf("invalid edit retried without correction: %+v", cmd)
	default:
	}
	settingsOperation(t, worker, &config.CollectorChange{ID: "fixed", Settings: config.CollectorSettings{HandHistory: &valid}})
	cmd := migrationCommand(t, link, transport.CommandCollectorConfig)
	if *cmd.Settings.ScreenName != name || *cmd.Settings.HandHistory != valid {
		t.Fatalf("correction failed to retain other pending fields: %+v", cmd)
	}
	// Fail the real journal write; the accepted revision must stay intact.
	journal := filepath.Join(filepath.Dir(worker.configPath), "collector_settings.json")
	if err := os.Mkdir(journal+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	reply := make(chan storage.OperationResult, 1)
	worker.Operations <- storage.Operation{CollectorSettings: true, CollectorChange: &config.CollectorChange{ID: "not-saved", Settings: config.CollectorSettings{HandHistory: &invalid}}, Reply: reply}
	result := migrationReply(t, reply)
	if result.Err == nil || result.CollectorSettings.Pending.ID != "fixed" {
		t.Fatalf("failed local save claimed queued: %+v", result)
	}
	if err := os.Remove(journal + ".tmp"); err != nil {
		t.Fatal(err)
	}
	state, err := config.LoadCollectorSettings(journal)
	if err != nil || state.Pending.ID != "fixed" {
		t.Fatalf("failed write changed queue: %+v %v", state, err)
	}
}

func TestOfflineCollectorQueueAcceptsAndCoalescesWithoutWritingServerSettings(t *testing.T) {
	worker, link, _ := migrationWorker(t)
	cert, key, _, err := transport.GenerateServerCert()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.Listen("127.0.0.1:0", cert, key)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	transport.Serve(ln, "offline", link)
	name, hh, ts := "Hero", `AppData\Local\PokerStars\HandHistory`, `AppData\Local\PokerStars\TournamentSummaries`
	for i, patch := range []config.CollectorSettings{{ScreenName: &name}, {HandHistory: &hh}, {TournamentSummary: &ts}} {
		result := settingsOperation(t, worker, &config.CollectorChange{ID: []string{"name", "hh", "ts"}[i], Settings: patch})
		if result.CollectorOnline {
			t.Fatal("offline collector reported connected")
		}
	}
	state := settingsOperation(t, worker, nil).CollectorSettings
	if !state.Desired().Complete() || state.Pending.ID != "ts" || *state.Pending.Settings.ScreenName != name {
		t.Fatalf("lost offline fields: %+v", state)
	}
	select {
	case cmd := <-link.Commands:
		t.Fatalf("sent command without collector: %+v", cmd)
	default:
	}
	// Server-owned changes remain independent, including no screen-name gate.
	if _, err := config.UpdateConfig(worker.configPath, func(cfg *config.Config) error { hour := 8; cfg.Schedule.DayStartHour = &hour; return nil }); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(worker.configPath)
	if err != nil || *cfg.Schedule.DayStartHour != 8 || cfg.Setup.PokerStarsScreenName != nil {
		t.Fatalf("wrong setting owner: %+v %v", cfg, err)
	}
}

func TestCollectorStatusReportsInactiveCollectionAndLeavesPendingChanges(t *testing.T) {
	worker, link, _ := migrationWorker(t)
	path := "renamed"
	settingsOperation(t, worker, &config.CollectorChange{ID: "waiting", Settings: config.CollectorSettings{HandHistory: &path}})
	link.Uploads <- transport.Upload{Status: &transport.CollectorStatus{Settings: &config.CollectorSettings{HandHistory: &path}, CollectionError: "missing folder"}}
	state := settingsState(t, worker, func(s config.CollectorSettingsState) bool { return s.CollectionError != "" })
	if state.Watching || state.Pending == nil {
		t.Fatalf("status inferred edit success: %+v", state)
	}
}
