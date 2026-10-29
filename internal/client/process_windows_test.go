package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcessPresenceReadOnly(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		running bool
	}{
		{filepath.Base(executable), true},
		{"tracker-absent-process-73b61579.exe", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := processRunning(test.name)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.running {
				t.Fatalf("processRunning(%q)=%v, want %v", test.name, got, test.running)
			}
		})
	}
}
