// Package config owns all reads and writes to config.json and lockfile.json.
// No other package touches these files directly.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

var mu sync.RWMutex

// Setup holds values written once during --setup mode.
type Setup struct {
	TelegramChatID       *string `json:"telegram_chat_id"`
	GoogleSheetID        *string `json:"google_sheet_id"`
	GoogleSheetName      *string `json:"google_sheet_name"`
	PokerStarsScreenName *string `json:"pokerstars_screen_name"`
}

// Paths holds the PokerStars folder locations configured via /paths.
type Paths struct {
	HandHistory       *string `json:"hand_history"`
	TournamentSummary *string `json:"tournament_summary"`
}

// Alerts holds thresholds for automatic outgoing alerts.
type Alerts struct {
	DownswingBuyins    *float64 `json:"downswing_buyins"`
	LosingStreakCount  *int     `json:"losing_streak_count"`
	SpingoDailyLossUSD *float64 `json:"spingo_daily_loss_usd"`
}

// Hardstop holds the hard stop trigger rules and lock duration.
type Hardstop struct {
	SessionLossBuyins  *float64 `json:"session_loss_buyins"`
	ConsecutiveLosses  *int     `json:"consecutive_losses"`
	SpingoDailyLossUSD *float64 `json:"spingo_daily_loss_usd"`
	DurationHours      *int     `json:"duration_hours"`
}

// Config is the in-memory representation of config.json.
// All leaf fields are pointers so that null and zero are distinguishable.
type Config struct {
	Setup    Setup    `json:"setup"`
	Paths    Paths    `json:"paths"`
	Alerts   Alerts   `json:"alerts"`
	Hardstop Hardstop `json:"hardstop"`
}

// LockFile is the in-memory representation of lockfile.json.
type LockFile struct {
	TriggeredAt   time.Time `json:"triggered_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Reason        string    `json:"reason"`
	DurationHours int       `json:"duration_hours"`
}

// PendingTournament is one tournament awaiting a satellite/ticket answer from
// the player, held in pending_tournaments.json.
type PendingTournament struct {
	Tournament     parsers.Tournament `json:"tournament"`
	FirstQueuedAt  time.Time          `json:"first_queued_at"`
	LastRemindedAt time.Time          `json:"last_reminded_at"`
}

// PendingTournaments is the in-memory representation of pending_tournaments.json.
type PendingTournaments struct {
	Pending []PendingTournament `json:"pending"`
}

// LoadConfig reads config.json at path. If the file does not exist it is
// created with all null values and the empty Config is returned.
func LoadConfig(path string) (*Config, error) {
	mu.RLock()
	data, err := os.ReadFile(path)
	mu.RUnlock() // explicit release so SaveConfig can acquire the write lock below
	if errors.Is(err, os.ErrNotExist) {
		cfg := &Config{}
		return cfg, SaveConfig(path, cfg)
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &cfg, nil
}

// SaveConfig writes cfg to path atomically (temp file then rename).
func SaveConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}
	tmp := path + ".tmp"
	mu.Lock()
	defer mu.Unlock()
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming config: %w", err)
	}
	return nil
}

// LoadLock reads lockfile.json at path. Returns nil, nil if the file does not
// exist — that is the normal state when no hard stop is active.
func LoadLock(path string) (*LockFile, error) {
	mu.RLock()
	defer mu.RUnlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading lockfile: %w", err)
	}
	var lock LockFile
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("parsing lockfile: %w", err)
	}
	return &lock, nil
}

// SaveLock writes lock to path.
func SaveLock(path string, lock *LockFile) error {
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling lockfile: %w", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing lockfile: %w", err)
	}
	return nil
}

// DeleteLock removes lockfile.json at path.
func DeleteLock(path string) error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("deleting lockfile: %w", err)
	}
	return nil
}

// IsLockExpired reports whether the lock's expiry time is in the past.
func IsLockExpired(lock *LockFile) bool {
	return lock.ExpiresAt.Before(time.Now().UTC())
}

// LoadPendingTournaments reads pending_tournaments.json at path. Returns an
// empty slice and nil error if the file does not exist — that is the normal
// state when nothing is awaiting a satellite/ticket answer.
func LoadPendingTournaments(path string) ([]PendingTournament, error) {
	mu.RLock()
	defer mu.RUnlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading pending tournaments: %w", err)
	}
	var pt PendingTournaments
	if err := json.Unmarshal(data, &pt); err != nil {
		return nil, fmt.Errorf("parsing pending tournaments: %w", err)
	}
	return pt.Pending, nil
}

// SavePendingTournaments writes pending to path.
func SavePendingTournaments(path string, pending []PendingTournament) error {
	data, err := json.MarshalIndent(PendingTournaments{Pending: pending}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling pending tournaments: %w", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing pending tournaments: %w", err)
	}
	return nil
}
