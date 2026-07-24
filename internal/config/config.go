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
	StorageBackend       *string `json:"storage_backend"` // "sheets" or "sqlite"
	GoogleSheetID        *string `json:"google_sheet_id"`
	GoogleSheetName      *string `json:"google_sheet_name"`
	PokerStarsScreenName *string `json:"pokerstars_screen_name"`
}

// Paths holds the PokerStars folder locations configured via /paths.
type Paths struct {
	HandHistory       *string `json:"hand_history"`
	TournamentSummary *string `json:"tournament_summary"`
}

// Alerts holds thresholds for automatic outgoing alerts. Each threshold has a
// sibling *_enabled flag: a set value with the flag false is "off but
// remembered" — kept in config so it can be switched back on without retyping,
// but blanked out for evaluation by EffectiveAlerts. A null flag defaults to
// on, so a value that predates this field (or was just set) is active.
type Alerts struct {
	DownswingBuyins        *float64 `json:"downswing_buyins"`
	DownswingEnabled       *bool    `json:"downswing_buyins_enabled"`
	LosingStreakCount      *int     `json:"losing_streak_count"`
	LosingStreakEnabled    *bool    `json:"losing_streak_count_enabled"`
	DailyLossUSD           *float64 `json:"daily_loss_usd"`
	DailyLossEnabled       *bool    `json:"daily_loss_usd_enabled"`
	SpingoDailyLossUSD     *float64 `json:"spingo_daily_loss_usd"`
	SpingoDailyLossEnabled *bool    `json:"spingo_daily_loss_usd_enabled"`
	SessionLossBuyins      *float64 `json:"session_loss_buyins"`
	SessionLossEnabled     *bool    `json:"session_loss_buyins_enabled"`
}

// Hardstop holds the hard stop trigger rules and lock duration. The threshold
// rules carry the same *_enabled flags as Alerts (see EffectiveHardstop);
// DurationHours has none — it's not a rule that fires, and has no turn-off.
type Hardstop struct {
	SessionLossBuyins        *float64 `json:"session_loss_buyins"`
	SessionLossEnabled       *bool    `json:"session_loss_buyins_enabled"`
	ConsecutiveLosses        *int     `json:"consecutive_losses"`
	ConsecutiveLossesEnabled *bool    `json:"consecutive_losses_enabled"`
	DailyLossUSD             *float64 `json:"daily_loss_usd"`
	DailyLossEnabled         *bool    `json:"daily_loss_usd_enabled"`
	SpingoDailyLossUSD       *float64 `json:"spingo_daily_loss_usd"`
	SpingoDailyLossEnabled   *bool    `json:"spingo_daily_loss_usd_enabled"`
	DurationHours            *int     `json:"duration_hours"`
	DownswingBuyins          *float64 `json:"downswing_buyins"`
	DownswingEnabled         *bool    `json:"downswing_buyins_enabled"`
}

// Schedule holds player-specific timing settings.
type Schedule struct {
	// DayStartHour is the local hour (0-23) the player's poker day starts,
	// used as the daily reset boundary for daily loss tracking. Nil means
	// default to 6 (6am local time).
	DayStartHour *int `json:"day_start_hour"`
}

// Notifications controls the always-on summary messages. The four result
// summaries carry a *string mode — "every" | "wins" | "losses" | "off" — that
// also filters them by win/loss; nil means "every" (the original always-on
// behavior). StakeChange has no P&L, so it's a plain on/off; nil means On.
type Notifications struct {
	CashSessionSummary *string `json:"cash_session_summary"`
	TournamentSummary  *string `json:"tournament_summary"`
	WeeklySummary      *string `json:"weekly_summary"`
	MonthlySummary     *string `json:"monthly_summary"`
	StakeChange        *bool   `json:"stake_change"`
}

// SummaryMode returns a result summary's configured mode, defaulting to
// "every" (send every result) when unset.
func SummaryMode(mode *string) string {
	if mode == nil {
		return "every"
	}
	return *mode
}

// ShouldSendSummary reports whether a result summary with the given net should
// be sent under mode. "off" never sends; "wins" only a net above zero;
// "losses" only a net below zero; "every" (and any unrecognized value) always.
// A breakeven net of exactly zero is neither a win nor a loss, so it sends
// under neither "wins" nor "losses".
func ShouldSendSummary(mode string, net float64) bool {
	switch mode {
	case "off":
		return false
	case "wins":
		return net > 0
	case "losses":
		return net < 0
	default:
		return true
	}
}

// FlagEnabled reports whether an on/off flag is enabled, defaulting to enabled
// when unset (nil). Shared by NotificationEnabled and the threshold flags.
func FlagEnabled(b *bool) bool {
	return b == nil || *b
}

// NotificationEnabled reports whether an on/off notification is enabled,
// defaulting to enabled when unset.
func NotificationEnabled(enabled *bool) bool {
	return FlagEnabled(enabled)
}

// EffectiveAlerts returns a copy of a with every turned-off threshold's value
// blanked to nil — "off" is a set value whose *_enabled flag is false. The
// alert evaluators read this so an off threshold looks exactly like an unset
// one, without their firing logic having to know about the flags.
func EffectiveAlerts(a Alerts) Alerts {
	if !FlagEnabled(a.DownswingEnabled) {
		a.DownswingBuyins = nil
	}
	if !FlagEnabled(a.LosingStreakEnabled) {
		a.LosingStreakCount = nil
	}
	if !FlagEnabled(a.DailyLossEnabled) {
		a.DailyLossUSD = nil
	}
	if !FlagEnabled(a.SpingoDailyLossEnabled) {
		a.SpingoDailyLossUSD = nil
	}
	if !FlagEnabled(a.SessionLossEnabled) {
		a.SessionLossBuyins = nil
	}
	return a
}

// EffectiveHardstop is EffectiveAlerts' counterpart for the hard-stop rules.
// DurationHours has no flag and is left untouched.
func EffectiveHardstop(h Hardstop) Hardstop {
	if !FlagEnabled(h.SessionLossEnabled) {
		h.SessionLossBuyins = nil
	}
	if !FlagEnabled(h.ConsecutiveLossesEnabled) {
		h.ConsecutiveLosses = nil
	}
	if !FlagEnabled(h.DailyLossEnabled) {
		h.DailyLossUSD = nil
	}
	if !FlagEnabled(h.SpingoDailyLossEnabled) {
		h.SpingoDailyLossUSD = nil
	}
	if !FlagEnabled(h.DownswingEnabled) {
		h.DownswingBuyins = nil
	}
	return h
}

// Config is the in-memory representation of config.json.
// All leaf fields are pointers so that null and zero are distinguishable.
type Config struct {
	Setup         Setup         `json:"setup"`
	Paths         Paths         `json:"paths"`
	Alerts        Alerts        `json:"alerts"`
	Hardstop      Hardstop      `json:"hardstop"`
	Schedule      Schedule      `json:"schedule"`
	Notifications Notifications `json:"notifications"`
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
