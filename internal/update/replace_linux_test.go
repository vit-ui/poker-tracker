package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplacementPreservesStoppedInstallation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "tracker-server")
	staged := filepath.Join(dir, "new")
	for path, value := range map[string]string{target: "old", staged: "new", filepath.Join(dir, "config.json"): "settings", filepath.Join(dir, "lockfile.json"): "lock", filepath.Join(dir, "tracker.db"): "history"} {
		if err := os.WriteFile(path, []byte(value), 0700); err != nil {
			t.Fatal(err)
		}
	}
	service := &serviceState{target: target}
	if err := replace(target, staged, service, "v4.8", "v4.7"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{target: "new", filepath.Join(dir, "config.json"): "settings", filepath.Join(dir, "lockfile.json"): "lock", filepath.Join(dir, "tracker.db"): "history"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %s err=%v", path, got, err)
		}
	}
}
func TestReplacementFailureRestoresOriginal(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "tracker-server")
	if err := os.WriteFile(target, []byte("old"), 0700); err != nil {
		t.Fatal(err)
	}
	err := replace(target, filepath.Join(dir, "missing"), &serviceState{target: target}, "v4.8", "v4.7")
	if err == nil {
		t.Fatal("missing staged executable succeeded")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "old" {
		t.Fatalf("original lost: %s %v", got, err)
	}
}
