package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeploymentOperationalChatPolicyAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, source, dest string
		reject             bool
	}{{"copy unset", "4242", "", false}, {"same", "4242", "4242", false}, {"foreign", "4242", "9999", true}, {"retain destination", "", "4242", false}, {"invalid zero", "0", "", true}, {"invalid text", "abc", "", true}, {"group", "-4242", "-4242", false}} {
		t.Run(tc.name, func(t *testing.T) {
			backend, sheet, token, connection, folder := "sheets", "owned-sheet", "owned-token", "owned-connection", "owned-folder"
			cfg := &Config{Setup: Setup{StorageBackend: &backend, GoogleSheetID: &sheet, PairToken: &token, ConnectionString: &connection}, Paths: Paths{HandHistory: &folder}}
			if tc.dest != "" {
				cfg.Setup.TelegramChatID = &tc.dest
			}
			settings := OperationalSettings{}
			if tc.source != "" {
				settings.TelegramChatID = &tc.source
			}
			err := ApplyOperational(cfg, settings)
			if (err != nil) != tc.reject {
				t.Fatalf("error=%v, reject=%v", err, tc.reject)
			}
			if *cfg.Setup.StorageBackend != backend || *cfg.Setup.GoogleSheetID != sheet || *cfg.Setup.PairToken != token || *cfg.Setup.ConnectionString != connection || *cfg.Paths.HandHistory != folder {
				t.Fatal("operational transfer touched destination identity")
			}
			if !tc.reject {
				want := tc.source
				if want == "" {
					want = tc.dest
				}
				got := ""
				if cfg.Setup.TelegramChatID != nil {
					got = *cfg.Setup.TelegramChatID
				}
				if got != want {
					t.Fatalf("chat=%q want %q", got, want)
				}
			}
		})
	}
}

func TestDeploymentJournalRejectsCorruptionAndKeepsReservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment_transfer.json")
	for _, data := range []string{`{`, `null`, `{"id":"owned","phase":"receiving","total_bytes":1}`, `{"id":"owned","phase":"receiving","total_bytes":1,"next_offset":2}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDeploymentTransfer(path); err == nil {
			t.Fatalf("corrupt state accepted: %s", data)
		}
	}
	state := &DeploymentTransferState{ID: "owned", Phase: "receiving", TotalBytes: 10, NextOffset: 3, SHA256: strings.Repeat("a", 64), Before: &OperationalSettings{}, Backend: "sqlite"}
	if err := SaveDeploymentTransfer(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadDeploymentTransfer(path)
	if err != nil || loaded.NextOffset != 3 || !DeploymentInProgress(loaded) {
		t.Fatalf("loaded=%+v %v", loaded, err)
	}
	for _, phase := range []string{"reserved", "receiving", "importing", "verified", "applying-settings", "rolling-back", "unknown"} {
		loaded.Phase = phase
		if !DeploymentInProgress(loaded) {
			t.Fatalf("reservation lost for %s", phase)
		}
	}
	for _, phase := range []string{"activated", "rolled-back"} {
		loaded.Phase = phase
		if DeploymentInProgress(loaded) {
			t.Fatalf("terminal reservation held for %s", phase)
		}
	}
}

func TestSourceTransferJournalRequiresFrozenRecoveryMaterial(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		reject     bool
	}{
		{"idle", `{"phase":"idle"}`, false},
		{"null", `null`, true},
		{"unknown", `{"phase":"unexpected"}`, true},
		{"missing identity", `{"phase":"preparing"}`, true},
		{"preparing", `{"phase":"preparing","id":"owned","connection":"owned-destination"}`, false},
		{"missing payload", `{"phase":"prepared","id":"owned","connection":"owned-destination","original_config":{}}`, true},
		{"missing config", `{"phase":"stopping","id":"owned","connection":"owned-destination","payload":{}}`, true},
		{"frozen", `{"phase":"prepared","id":"owned","connection":"owned-destination","payload":{},"original_config":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source_transfer.json")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadSourceTransfer(path); (err != nil) != tc.reject {
				t.Fatalf("error=%v reject=%v", err, tc.reject)
			}
		})
	}
}
