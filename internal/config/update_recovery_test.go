package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUpdateRecoveryJournalRoundTripAndFailedReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update_recovery.json")
	state, err := LoadUpdateRecovery(path)
	if err != nil || state != nil {
		t.Fatalf("missing journal=%+v err=%v", state, err)
	}
	original := UpdateRecovery{Target: "tracker.exe", Previous: ".tracker-previous-original", Staged: ".tracker-update-next.exe", PreviousSHA256: "old", NextSHA256: "next", OldVersion: "v4.8", NewVersion: "v4.9", Task: "isolated", TaskExists: true, TaskEnabled: true, TaskRunning: true}
	if err := SaveUpdateRecovery(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	changed := original
	changed.Confirmed = true
	if err := SaveUpdateRecovery(path, changed); err == nil {
		t.Fatal("write failure not reported")
	}
	state, err = LoadUpdateRecovery(path)
	if err != nil || !reflect.DeepEqual(state, &original) {
		t.Fatalf("original journal changed: %+v %v", state, err)
	}
	if err := os.Remove(path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := SaveUpdateRecovery(path, changed); err != nil {
		t.Fatal(err)
	}
	state, err = LoadUpdateRecovery(path)
	if err != nil || !reflect.DeepEqual(state, &changed) {
		t.Fatalf("confirmation not stored: %+v %v", state, err)
	}
	if err := DeleteUpdateRecovery(path); err != nil {
		t.Fatal(err)
	}
	if err := DeleteUpdateRecovery(path); err != nil {
		t.Fatal(err)
	}
	state, err = LoadUpdateRecovery(path)
	if err != nil || state != nil {
		t.Fatalf("journal not removed: %+v %v", state, err)
	}
}
