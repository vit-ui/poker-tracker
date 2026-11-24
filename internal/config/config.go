package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
)

var mu sync.RWMutex

type Setup struct {
	TelegramChatID       *string `json:"telegram_chat_id"`
	StorageBackend       *string `json:"storage_backend"`
	GoogleSheetID        *string `json:"google_sheet_id"`
	GoogleSheetName      *string `json:"google_sheet_name"`
	PokerStarsScreenName *string `json:"pokerstars_screen_name"`

	ConnectionString *string `json:"connection_string"`
	PairToken        *string `json:"pair_token"`
}

type Paths struct {
	HandHistory       *string `json:"hand_history"`
	TournamentSummary *string `json:"tournament_summary"`
}

type Alerts struct {
	DownswingBuyins        *float64     `json:"downswing_buyins"`
	DownswingEnabled       *bool        `json:"downswing_buyins_enabled"`
	LosingStreakCount      *int         `json:"losing_streak_count"`
	LosingStreakEnabled    *bool        `json:"losing_streak_count_enabled"`
	DailyLossCents         *money.Cents `json:"daily_loss_cents"`
	DailyLossEnabled       *bool        `json:"daily_loss_cents_enabled"`
	SpingoDailyLossCents   *money.Cents `json:"spingo_daily_loss_cents"`
	SpingoDailyLossEnabled *bool        `json:"spingo_daily_loss_cents_enabled"`
	SessionLossBuyins      *float64     `json:"session_loss_buyins"`
	SessionLossEnabled     *bool        `json:"session_loss_buyins_enabled"`
}

type Hardstop struct {
	SessionLossBuyins        *float64     `json:"session_loss_buyins"`
	SessionLossEnabled       *bool        `json:"session_loss_buyins_enabled"`
	ConsecutiveLosses        *int         `json:"consecutive_losses"`
	ConsecutiveLossesEnabled *bool        `json:"consecutive_losses_enabled"`
	DailyLossCents           *money.Cents `json:"daily_loss_cents"`
	DailyLossEnabled         *bool        `json:"daily_loss_cents_enabled"`
	SpingoDailyLossCents     *money.Cents `json:"spingo_daily_loss_cents"`
	SpingoDailyLossEnabled   *bool        `json:"spingo_daily_loss_cents_enabled"`
	DurationHours            *int         `json:"duration_hours"`
	DownswingBuyins          *float64     `json:"downswing_buyins"`
	DownswingEnabled         *bool        `json:"downswing_buyins_enabled"`
}

type Schedule struct {
	DayStartHour *int `json:"day_start_hour"`
}

type Notifications struct {
	CashSessionSummary *string `json:"cash_session_summary"`
	TournamentSummary  *string `json:"tournament_summary"`
	WeeklySummary      *string `json:"weekly_summary"`
	MonthlySummary     *string `json:"monthly_summary"`
	StakeChange        *bool   `json:"stake_change"`
}

func SummaryMode(mode *string) string {
	if mode == nil {
		return "every"
	}
	return *mode
}

func ShouldSendSummary(mode string, netCents money.Cents) bool {
	switch mode {
	case "off":
		return false
	case "wins":
		return netCents > 0
	case "losses":
		return netCents < 0
	default:
		return true
	}
}

func FlagEnabled(b *bool) bool {
	return b == nil || *b
}

func NotificationEnabled(enabled *bool) bool {
	return FlagEnabled(enabled)
}

func EffectiveAlerts(a Alerts) Alerts {
	if !FlagEnabled(a.DownswingEnabled) {
		a.DownswingBuyins = nil
	}
	if !FlagEnabled(a.LosingStreakEnabled) {
		a.LosingStreakCount = nil
	}
	if !FlagEnabled(a.DailyLossEnabled) {
		a.DailyLossCents = nil
	}
	if !FlagEnabled(a.SpingoDailyLossEnabled) {
		a.SpingoDailyLossCents = nil
	}
	if !FlagEnabled(a.SessionLossEnabled) {
		a.SessionLossBuyins = nil
	}
	return a
}

func EffectiveHardstop(h Hardstop) Hardstop {
	if !FlagEnabled(h.SessionLossEnabled) {
		h.SessionLossBuyins = nil
	}
	if !FlagEnabled(h.ConsecutiveLossesEnabled) {
		h.ConsecutiveLosses = nil
	}
	if !FlagEnabled(h.DailyLossEnabled) {
		h.DailyLossCents = nil
	}
	if !FlagEnabled(h.SpingoDailyLossEnabled) {
		h.SpingoDailyLossCents = nil
	}
	if !FlagEnabled(h.DownswingEnabled) {
		h.DownswingBuyins = nil
	}
	return h
}

type Config struct {
	Setup         Setup         `json:"setup"`
	Paths         Paths         `json:"paths"`
	Alerts        Alerts        `json:"alerts"`
	Hardstop      Hardstop      `json:"hardstop"`
	Schedule      Schedule      `json:"schedule"`
	Notifications Notifications `json:"notifications"`
}

type LockFile struct {
	TriggerID     string    `json:"trigger_id,omitempty"`
	TriggeredAt   time.Time `json:"triggered_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Reason        string    `json:"reason"`
	DurationHours int       `json:"duration_hours"`
}

const (
	PendingSatellite     = "satellite"
	PendingFreerollBuyin = "freeroll_buyin"
	PendingMissingResult = "missing_result"
)

type PendingTournament struct {
	Tournament     parsers.Tournament        `json:"tournament"`
	Reason         string                    `json:"reason,omitempty"`
	FirstQueuedAt  time.Time                 `json:"first_queued_at"`
	LastRemindedAt time.Time                 `json:"last_reminded_at"`
	Recovery       *parsers.TournamentFinish `json:"recovery,omitempty"`
	QuietUntil     *time.Time                `json:"quiet_until,omitempty"`
	Historical     bool                      `json:"historical,omitempty"`
}

type PendingTournaments struct {
	Pending []PendingTournament `json:"pending"`
}

func LoadConfig(path string) (*Config, error) {
	mu.Lock()
	defer mu.Unlock()
	cfg, err := loadConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg = &Config{}
		err = saveConfig(path, cfg)
	}
	return cfg, err
}

func ValidateExistingConfig(path string) error {
	_, err := ReadConfig(path)
	return err
}

func ReadConfig(path string) (*Config, error) {
	mu.RLock()
	defer mu.RUnlock()
	return loadConfig(path)
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &cfg, nil
}

func UpdateConfig(path string, update func(*Config) error) (*Config, error) {
	mu.Lock()
	defer mu.Unlock()
	cfg, err := loadConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg, err = &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := update(cfg); err != nil {
		return nil, err
	}
	return cfg, saveConfig(path, cfg)
}

func SaveConfig(path string, cfg *Config) error {
	mu.Lock()
	defer mu.Unlock()
	return saveConfig(path, cfg)
}

func saveConfig(path string, cfg *Config) error {
	if err := guardDeploymentSettings(path, cfg); err != nil {
		return err
	}
	return saveConfigUnchecked(path, cfg)
}

func saveConfigUnchecked(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}
	if err := replaceStateFile(path, data); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}

func LoadLock(path string) (*LockFile, error) {
	mu.RLock()
	defer mu.RUnlock()
	state, err := loadLockState(path)
	if err != nil {
		return nil, err
	}
	if state.ExpiresAt.IsZero() {
		return nil, nil
	}
	lock := state.LockFile
	return &lock, nil
}

func SaveLock(path string, lock *LockFile) error {
	mu.Lock()
	defer mu.Unlock()
	state, err := loadLockState(path)
	if err != nil {
		return err
	}
	state.LockFile = *lock
	return saveLockState(path, state)
}

func DeleteLock(path string) error {
	mu.Lock()
	defer mu.Unlock()
	state, err := loadLockState(path)
	if err != nil {
		return err
	}
	if len(state.Triggers) == 0 {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("deleting lockfile: %w", err)
		}
		return nil
	}
	state.LockFile = LockFile{}
	return saveLockState(path, state)
}

func IsLockExpired(lock *LockFile) bool {
	return lock.ExpiresAt.Before(time.Now().UTC())
}

func LoadPendingTournaments(path string) ([]PendingTournament, error) {
	mu.RLock()
	defer mu.RUnlock()
	return loadPendingTournaments(path)
}

func loadPendingTournaments(path string) ([]PendingTournament, error) {
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

func SavePendingTournaments(path string, pending []PendingTournament) error {
	mu.Lock()
	defer mu.Unlock()
	return savePendingTournaments(path, pending)
}

func UpdatePendingTournaments(path string, update func([]PendingTournament) ([]PendingTournament, bool)) error {
	mu.Lock()
	defer mu.Unlock()
	pending, err := loadPendingTournaments(path)
	if err != nil {
		return err
	}
	pending, changed := update(pending)
	if !changed {
		return nil
	}
	return savePendingTournaments(path, pending)
}

func savePendingTournaments(path string, pending []PendingTournament) error {
	if err := guardDeploymentPending(path, pending); err != nil {
		return err
	}
	return savePendingTournamentsUnchecked(path, pending)
}

func savePendingTournamentsUnchecked(path string, pending []PendingTournament) error {
	data, err := json.MarshalIndent(PendingTournaments{Pending: pending}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling pending tournaments: %w", err)
	}
	if err := replaceStateFile(path, data); err != nil {
		return fmt.Errorf("saving pending tournaments: %w", err)
	}
	return nil
}

func replaceStateFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("writing temporary state file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing state file: %w", err)
	}
	return nil
}

type ClosureEvent struct {
	ID       string    `json:"id"`
	ClosedAt time.Time `json:"closed_at"`
	Lock     LockFile  `json:"lock"`
}

type ClosureNotification struct {
	Event     ClosureEvent `json:"event"`
	Delivered bool         `json:"delivered"`
}

func LoadClosureNotifications(path string) (map[string]ClosureNotification, error) {
	mu.RLock()
	defer mu.RUnlock()
	return loadClosureNotifications(path)
}

func loadClosureNotifications(path string) (map[string]ClosureNotification, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]ClosureNotification), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading closure notifications: %w", err)
	}
	entries := make(map[string]ClosureNotification)
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decoding closure notifications: %w", err)
	}
	if entries == nil {
		entries = make(map[string]ClosureNotification)
	}
	for id, entry := range entries {
		if id == "" || entry.Event.ID != id || entry.Event.ClosedAt.IsZero() || !entry.Event.Lock.ExpiresAt.After(entry.Event.ClosedAt) {
			return nil, fmt.Errorf("invalid closure event %q", id)
		}
	}
	return entries, nil
}

func SaveClosureNotifications(path string, entries map[string]ClosureNotification) error {
	mu.Lock()
	defer mu.Unlock()
	return saveClosureNotifications(path, entries)
}

func saveClosureNotifications(path string, entries map[string]ClosureNotification) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding closure notifications: %w", err)
	}
	file, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening closure journal: %w", err)
	}
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return fmt.Errorf("persisting closure journal: %w", err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return fmt.Errorf("replacing closure journal: %w", err)
	}
	return nil
}

func QueueClosures(path string, events []ClosureEvent) error {
	mu.Lock()
	defer mu.Unlock()
	entries, err := loadClosureNotifications(path)
	if err != nil {
		return err
	}
	for _, event := range events {
		if _, exists := entries[event.ID]; !exists {
			entries[event.ID] = ClosureNotification{Event: event}
		}
	}
	return saveClosureNotifications(path, entries)
}

func AcknowledgeClosure(path, id string) error {
	mu.Lock()
	defer mu.Unlock()
	entries, err := loadClosureNotifications(path)
	if err != nil {
		return err
	}
	if _, exists := entries[id]; !exists {
		return nil
	}
	delete(entries, id)
	return saveClosureNotifications(path, entries)
}

func CheckClosureTransferReady(root string) error {
	for _, name := range []string{"collector_closures.json", "closure_notifications.json"} {
		entries, err := LoadClosureNotifications(filepath.Join(root, name))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.Delivered {
				return fmt.Errorf("PokerStars closure notifications are pending")
			}
		}
	}
	return nil
}
