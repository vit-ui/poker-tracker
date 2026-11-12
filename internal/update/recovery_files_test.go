package update

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/vit-ui/self-tracker/internal/config"
)

func recoveryFixture(t *testing.T) (config.UpdateRecovery, string) {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "tracker.exe")
	old := sha256.Sum256([]byte("old executable"))
	next := sha256.Sum256([]byte("new executable"))
	return config.UpdateRecovery{Target: "tracker.exe", Previous: ".tracker-previous-test", Staged: ".tracker-update-test.exe", PreviousSHA256: hex.EncodeToString(old[:]), NextSHA256: hex.EncodeToString(next[:]), OldVersion: "v4.8", NewVersion: "v4.9", Task: "isolated", TaskExists: true, TaskEnabled: true, TaskRunning: true}, target
}

func TestRecoveryRejectsUnrelatedOrUnsafeState(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*config.UpdateRecovery)
	}{
		{"other target", func(s *config.UpdateRecovery) { s.Target = "other.exe" }},
		{"other task", func(s *config.UpdateRecovery) { s.Task = "other" }},
		{"escaped backup", func(s *config.UpdateRecovery) { s.Previous = "../.tracker-previous-test" }},
		{"data file as backup", func(s *config.UpdateRecovery) { s.Previous = "config.json" }},
		{"escaped staging", func(s *config.UpdateRecovery) { s.Staged = "../.tracker-update-test.exe" }},
		{"recovery helper as staging", func(s *config.UpdateRecovery) { s.Staged = recoveryHelperName }},
		{"bad checksum", func(s *config.UpdateRecovery) { s.PreviousSHA256 = "bad" }},
		{"invalid version", func(s *config.UpdateRecovery) { s.NewVersion = "dev" }},
		{"missing task marked running", func(s *config.UpdateRecovery) { s.TaskExists = false }},
		{"two owner modes", func(s *config.UpdateRecovery) { s.Manual = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, target := recoveryFixture(t)
			test.change(&state)
			if err := validateRecovery(state, target, "isolated"); err == nil {
				t.Fatal("unsafe recovery state accepted")
			}
		})
	}
}

func TestExecutableRecoveryAtEveryReplacementBoundary(t *testing.T) {
	for _, test := range []struct {
		name, current, previous string
		confirmed, fail         bool
		want                    string
	}{
		{"before replacement", "old executable", "", false, false, "old executable"},
		{"old executable moved", "", "old executable", false, false, "old executable"},
		{"new executable installed", "new executable", "old executable", false, false, "old executable"},
		{"rollback already restored old", "old executable", "", false, false, "old executable"},
		{"confirmed update", "new executable", "old executable", true, false, "new executable"},
		{"missing backup", "new executable", "", false, true, "new executable"},
		{"damaged backup", "new executable", "damaged", false, true, "new executable"},
		{"damaged confirmed image", "damaged", "old executable", true, true, "damaged"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, target := recoveryFixture(t)
			state.Confirmed = test.confirmed
			for name, data := range map[string]string{state.Target: test.current, state.Previous: test.previous, state.Staged: "staged executable", "config.json": "settings", "lockfile.json": "original lock and expiry", "tracker.db": "history"} {
				if data != "" {
					if err := os.WriteFile(filepath.Join(filepath.Dir(target), name), []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := validateRecovery(state, target, "isolated"); err != nil {
				t.Fatal(err)
			}
			err := recoverExecutable(state, target)
			if (err != nil) != test.fail {
				t.Fatalf("recovery error=%v want failure=%v", err, test.fail)
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != test.want {
				t.Fatalf("installed image=%q err=%v want %q", got, err, test.want)
			}
			if !test.fail {
				if err := recoverExecutable(state, target); err != nil {
					t.Fatalf("repeat recovery: %v", err)
				}
			}
			for name, want := range map[string]string{"config.json": "settings", "lockfile.json": "original lock and expiry", "tracker.db": "history"} {
				data, err := os.ReadFile(filepath.Join(filepath.Dir(target), name))
				if err != nil || string(data) != want {
					t.Fatalf("recovery changed %s: %q %v", name, data, err)
				}
			}
		})
	}
}
