package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUpdateStateSurvivesRestart(t *testing.T) {
	for _, tc := range []struct {
		state   string
		pending bool
	}{
		{UpdateQueued, true}, {UpdateWaiting, true}, {UpdateRunning, true}, {UpdateSucceeded, false}, {UpdateFailed, false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "requested_update.json")
			if got, err := LoadUpdateJob(path); err != nil || got != nil {
				t.Fatalf("missing job = %v, %v", got, err)
			}
			want := UpdateJob{Request: UpdateRequest{ID: "telegram:123", Version: "v4.9"}, State: tc.state, Version: "v4.8"}
			if err := SaveUpdateJob(path, want); err != nil {
				t.Fatal(err)
			}
			got, err := LoadUpdateJob(path)
			if err != nil || !reflect.DeepEqual(got, &want) || got.Pending() != tc.pending {
				t.Fatalf("job = %+v, err %v", got, err)
			}
			queue := CollectorUpdates{Request: &want.Request, Report: UpdateReport{Version: "v4.8", Job: &want}, Error: "offline"}
			queuePath := filepath.Join(filepath.Dir(path), "collector_updates.json")
			if err := SaveCollectorUpdates(queuePath, queue); err != nil {
				t.Fatal(err)
			}
			if got, err := LoadCollectorUpdates(queuePath); err != nil || !reflect.DeepEqual(got, queue) {
				t.Fatalf("queue = %+v, %v", got, err)
			}
			notices := UpdateNotices{Release: "v4.9", Local: "telegram:123:running", Collector: "telegram:122:succeeded"}
			noticePath := filepath.Join(filepath.Dir(path), "update_notices.json")
			if err := SaveUpdateNotices(noticePath, notices); err != nil {
				t.Fatal(err)
			}
			if got, err := LoadUpdateNotices(noticePath); err != nil || got != notices {
				t.Fatalf("notices = %+v, %v", got, err)
			}
			if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadUpdateJob(path); err == nil {
				t.Fatal("accepted a corrupt update journal")
			}
		})
	}
}

func TestUpdateStateSaveFailurePreservesConfirmedRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requested_update.json")
	old := UpdateJob{Request: UpdateRequest{ID: "saved", Version: "v4.9"}, State: UpdateQueued, Version: "v4.8"}
	if err := SaveUpdateJob(path, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	next := old
	next.Request.ID = "unsaved"
	if err := SaveUpdateJob(path, next); err == nil {
		t.Fatal("save succeeded with blocked temporary path")
	}
	if got, err := LoadUpdateJob(path); err != nil || !reflect.DeepEqual(got, &old) {
		t.Fatalf("old request lost: %+v, %v", got, err)
	}
}

func TestPendingUpdatesBlockHandover(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		local, remote                          string
		unconfirmed, failedAcceptance, blocked bool
	}{
		{name: "empty"}, {name: "local queued", local: UpdateQueued, blocked: true}, {name: "local waiting", local: UpdateWaiting, blocked: true},
		{name: "local running", local: UpdateRunning, blocked: true}, {name: "local confirmed", local: UpdateSucceeded},
		{name: "local failed", local: UpdateFailed}, {name: "PC no receipt", unconfirmed: true, blocked: true},
		{name: "PC waiting", remote: UpdateWaiting, blocked: true}, {name: "PC confirmed", remote: UpdateSucceeded},
		{name: "PC failed", remote: UpdateFailed}, {name: "PC rejected", unconfirmed: true, failedAcceptance: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			r := UpdateRequest{ID: "request", Version: "v4.9"}
			if tc.local != "" {
				if err := SaveUpdateJob(filepath.Join(root, "requested_update.json"), UpdateJob{Request: r, State: tc.local, Version: "v4.8"}); err != nil {
					t.Fatal(err)
				}
			}
			state := CollectorUpdates{}
			if tc.unconfirmed || tc.remote != "" {
				state.Request = &r
			}
			if tc.remote != "" {
				state.Report.Job = &UpdateJob{Request: r, State: tc.remote, Version: "v4.8"}
			}
			if tc.failedAcceptance {
				state.Error = "not accepted"
			}
			if err := SaveCollectorUpdates(filepath.Join(root, "collector_updates.json"), state); err != nil {
				t.Fatal(err)
			}
			if err := CheckUpdatesIdle(root); (err != nil) != tc.blocked {
				t.Fatalf("blocked=%v err=%v", tc.blocked, err)
			}
		})
	}
}
