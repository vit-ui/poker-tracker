package bot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vit-ui/self-tracker/internal/config"
)

func TestLoadTelegramCredentials(t *testing.T) {
	for _, tc := range []struct {
		name       string
		config     string
		token      string
		wantChatID int64
		wantError  bool
	}{
		{name: "fresh directory"},
		{name: "legacy chat without token", config: "31415"},
		{name: "token without chat", config: "null", token: "fixture-token"},
		{name: "whitespace token", config: "31415", token: " \n"},
		{name: "token without config", token: "fixture-token"},
		{name: "group must be paired again", config: "-100123", token: "fixture-token"},
		{name: "zero chat is unset", config: "0", token: "fixture-token"},
		{name: "malformed chat ID", config: "abc", token: "fixture-token"},
		{name: "saved private pairing", config: "31415", token: "fixture-token", wantChatID: 31415},
		{name: "unreadable config format", config: "{", token: "fixture-token", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			switch tc.config {
			case "{":
				if err := os.WriteFile(path, []byte(tc.config), 0600); err != nil {
					t.Fatal(err)
				}
			case "null":
				if err := config.SaveConfig(path, &config.Config{}); err != nil {
					t.Fatal(err)
				}
			case "":
			default:
				if err := config.SaveConfig(path, &config.Config{Setup: config.Setup{TelegramChatID: &tc.config}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.token != "" {
				tokenPath := config.TelegramTokenPath(path)
				if err := os.MkdirAll(filepath.Dir(tokenPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(tokenPath, []byte(tc.token), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := loadTelegramCredentials(path)
			if (err != nil) != tc.wantError {
				t.Fatalf("load error = %v, want error = %v", err, tc.wantError)
			}
			want := telegramCredentials{}
			if tc.wantChatID != 0 {
				want = telegramCredentials{token: tc.token, chatID: tc.wantChatID}
			}
			if got != want {
				t.Errorf("saved pairing = %v with chat ID %d, want %v with chat ID %d",
					got.token != "", got.chatID, want.token != "", want.chatID)
			}
			if tc.config == "" {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("pairing check changed a missing config: %v", err)
				}
			}
		})
	}
}
