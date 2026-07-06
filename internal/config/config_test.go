package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

func TestLoadConfig_MissingFileCreatesAllNulls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Setup.TelegramChatID != nil {
		t.Error("TelegramChatID should be nil")
	}
	if cfg.Setup.GoogleSheetName != nil {
		t.Error("GoogleSheetName should be nil")
	}
	if cfg.Setup.PokerStarsScreenName != nil {
		t.Error("PokerStarsScreenName should be nil")
	}
	if cfg.Paths.HandHistory != nil {
		t.Error("HandHistory should be nil")
	}
	if cfg.Paths.TournamentSummary != nil {
		t.Error("TournamentSummary should be nil")
	}
	if cfg.Alerts.DownswingBuyins != nil {
		t.Error("DownswingBuyins should be nil")
	}
	if cfg.Alerts.LosingStreakCount != nil {
		t.Error("LosingStreakCount should be nil")
	}
	if cfg.Alerts.SpingoDailyLossUSD != nil {
		t.Error("SpingoDailyLossUSD should be nil")
	}
	if cfg.Hardstop.SessionLossBuyins != nil {
		t.Error("SessionLossBuyins should be nil")
	}
	if cfg.Hardstop.ConsecutiveLosses != nil {
		t.Error("ConsecutiveLosses should be nil")
	}
	if cfg.Hardstop.SpingoDailyLossUSD != nil {
		t.Error("SpingoDailyLossUSD should be nil")
	}
	if cfg.Hardstop.DurationHours != nil {
		t.Error("DurationHours should be nil")
	}

	if _, err := os.Stat(path); err != nil {
		t.Error("config.json should have been created on disk")
	}
}

func TestSaveConfig_LoadConfig_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	sheetName := "My Poker Sheet"
	buyins := 5.0
	streak := 3
	duration := 12

	cfg := &config.Config{
		Setup: config.Setup{
			GoogleSheetName: &sheetName,
		},
		Alerts: config.Alerts{
			DownswingBuyins:  &buyins,
			LosingStreakCount: &streak,
		},
		Hardstop: config.Hardstop{
			DurationHours: &duration,
		},
	}

	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if loaded.Setup.GoogleSheetName == nil || *loaded.Setup.GoogleSheetName != sheetName {
		t.Errorf("GoogleSheetName: got %v, want %q", loaded.Setup.GoogleSheetName, sheetName)
	}
	if loaded.Alerts.DownswingBuyins == nil || *loaded.Alerts.DownswingBuyins != buyins {
		t.Errorf("DownswingBuyins: got %v, want %v", loaded.Alerts.DownswingBuyins, buyins)
	}
	if loaded.Alerts.LosingStreakCount == nil || *loaded.Alerts.LosingStreakCount != streak {
		t.Errorf("LosingStreakCount: got %v, want %v", loaded.Alerts.LosingStreakCount, streak)
	}
	if loaded.Hardstop.DurationHours == nil || *loaded.Hardstop.DurationHours != duration {
		t.Errorf("DurationHours: got %v, want %v", loaded.Hardstop.DurationHours, duration)
	}
	// Unset fields must survive as nil.
	if loaded.Setup.TelegramChatID != nil {
		t.Error("TelegramChatID should remain nil")
	}
	if loaded.Paths.HandHistory != nil {
		t.Error("HandHistory should remain nil")
	}
}

func TestLoadLock_MissingFileReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockfile.json")

	lock, err := config.LoadLock(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lock != nil {
		t.Error("expected nil for missing lockfile")
	}
}

func TestSaveLock_LoadLock_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockfile.json")

	triggered := time.Date(2025, 1, 15, 22, 30, 0, 0, time.UTC)
	expires := time.Date(2025, 1, 16, 10, 30, 0, 0, time.UTC)

	lock := &config.LockFile{
		TriggeredAt:   triggered,
		ExpiresAt:     expires,
		Reason:        "session_loss_buyins",
		DurationHours: 12,
	}

	if err := config.SaveLock(path, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}

	loaded, err := config.LoadLock(path)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil lock")
	}

	if !loaded.TriggeredAt.Equal(triggered) {
		t.Errorf("TriggeredAt: got %v, want %v", loaded.TriggeredAt, triggered)
	}
	if !loaded.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt: got %v, want %v", loaded.ExpiresAt, expires)
	}
	if loaded.Reason != "session_loss_buyins" {
		t.Errorf("Reason: got %q, want %q", loaded.Reason, "session_loss_buyins")
	}
	if loaded.DurationHours != 12 {
		t.Errorf("DurationHours: got %d, want 12", loaded.DurationHours)
	}
}

func TestDeleteLock_RemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockfile.json")

	lock := &config.LockFile{
		TriggeredAt:   time.Now().UTC(),
		ExpiresAt:     time.Now().Add(time.Hour).UTC(),
		Reason:        "manual",
		DurationHours: 1,
	}
	if err := config.SaveLock(path, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}

	if err := config.DeleteLock(path); err != nil {
		t.Fatalf("DeleteLock: %v", err)
	}

	loaded, err := config.LoadLock(path)
	if err != nil {
		t.Fatalf("LoadLock after delete: %v", err)
	}
	if loaded != nil {
		t.Error("expected nil after delete")
	}
}

func TestIsLockExpired(t *testing.T) {
	past := &config.LockFile{ExpiresAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	if !config.IsLockExpired(past) {
		t.Error("expected past expiry to be expired")
	}

	future := &config.LockFile{ExpiresAt: time.Now().Add(time.Hour)}
	if config.IsLockExpired(future) {
		t.Error("expected future expiry to not be expired")
	}
}
