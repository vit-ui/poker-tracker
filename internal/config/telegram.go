package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vit-ui/self-tracker/internal/fileops"
)

func TelegramTokenPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "credentials", "telegram.token")
}

func LoadTelegramToken(configPath string) (string, error) {
	raw, err := os.ReadFile(TelegramTokenPath(configPath))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading Telegram credential: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func SaveTelegramToken(configPath, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("missing Telegram bot token")
	}
	path := TelegramTokenPath(configPath)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating credential directory: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".telegram-*")
	if err != nil {
		return fmt.Errorf("creating temporary Telegram credential: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := restrictTelegramToken(f.Name()); err != nil {
		return fmt.Errorf("protecting Telegram credential: %w", err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		return fmt.Errorf("writing Telegram credential: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing Telegram credential: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing Telegram credential: %w", err)
	}
	if err := fileops.RenameDurably(f.Name(), path); err != nil {
		return fmt.Errorf("installing Telegram credential: %w", err)
	}
	return nil
}
