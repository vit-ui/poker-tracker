//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTelegramCredentialStaysPrivateAcrossReplacement(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if token, err := LoadTelegramToken(configPath); err != nil || token != "" {
		t.Fatalf("missing credential: token present = %t, err = %v", token != "", err)
	}
	for _, token := range []string{"100:synthetic-first", "100:synthetic-replacement"} {
		if err := SaveTelegramToken(configPath, token); err != nil {
			t.Fatal(err)
		}
		got, err := LoadTelegramToken(configPath)
		if err != nil || got != token {
			t.Fatal("saved credential did not round-trip")
		}
		info, err := os.Stat(TelegramTokenPath(configPath))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("credential mode = %o, want 600", info.Mode().Perm())
		}
	}
}
