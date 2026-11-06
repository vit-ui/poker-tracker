// Package config owns all reads and writes to config.json and lockfile.json.
// No other package touches these files directly.
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

// Setup holds values written once during --setup mode.
type Setup struct {
	TelegramChatID       *string `json:"telegram_chat_id"`
	StorageBackend       *string `json:"storage_backend"` // "sheets" or "sqlite"
	GoogleSheetID        *string `json:"google_sheet_id"`
	GoogleSheetName      *string `json:"google_sheet_name"`
	PokerStarsScreenName *string `json:"pokerstars_screen_name"`

	// ConnectionString and PairToken exist only for the split build (v3.7+) and
	// stay null in an all-local install. ConnectionString is set on the client
	// (the tracker://host:port/token#fp=... its server printed); PairToken is
	// the shared secret generated on the server at setup. Both null = all-local,
	// so an existing combined config is unaffected.
	ConnectionString *string `json:"connection_string"`
	PairToken        *string `json:"pair_token"`
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

// Hardstop holds the hard stop trigger rules and lock duration. The threshold
// rules carry the same *_enabled flags as Alerts (see EffectiveHardstop);
// DurationHours has none — it's not a rule that fires, and has no turn-off.
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
	TriggerID     string    `json:"trigger_id,omitempty"`
	TriggeredAt   time.Time `json:"triggered_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Reason        string    `json:"reason"`
	DurationHours int       `json:"duration_hours"`
}

// Pending-tournament queue reasons — why a tournament is held awaiting a
// player answer before it's recorded. An empty Reason on an existing queue
// file means the original satellite question, so no migration is needed.
const (
	// PendingSatellite: a LikelySatellite tournament awaiting the
	// ticket-vs-cash entry question.
	PendingSatellite = "satellite"
	// PendingFreerollBuyin: a zero-buy-in freeroll/Kick-Off awaiting the
	// free-vs-paid entry-cost question — PokerStars records no buy-in and no
	// entry method, so the player supplies the cost.
	PendingFreerollBuyin = "freeroll_buyin"
)

// PendingTournament is one tournament awaiting a player answer before it's
// recorded, held in pending_tournaments.json. Reason (see the Pending*
// constants) selects which question the bot asks; empty means the original
// satellite question.
type PendingTournament struct {
	Tournament     parsers.Tournament `json:"tournament"`
	Reason         string             `json:"reason,omitempty"`
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
	mu.Lock()
	defer mu.Unlock()
	cfg, err := loadConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg = &Config{}
		err = saveConfig(path, cfg)
	}
	return cfg, err
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

// UpdateConfig atomically changes selected fields without overwriting concurrent
// collector or bot changes. update must not call other config operations.
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

// SaveConfig writes cfg to path atomically (temp file then rename).
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

// LoadLock reads lockfile.json at path. Returns nil, nil if the file does not
// exist — that is the normal state when no hard stop is active.
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

// SaveLock retains trigger receipts while replacing a legacy lock directly.
// Production enforcement uses ApplyHardstop for atomic replay decisions.
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

// DeleteLock clears the active lock without discarding replay receipts. Old
// installations with no receipts retain their original file-removal behavior.
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

// SavePendingTournaments writes pending to path.
func SavePendingTournaments(path string, pending []PendingTournament) error {
	mu.Lock()
	defer mu.Unlock()
	return savePendingTournaments(path, pending)
}

// UpdatePendingTournaments atomically reads, changes and replaces the pending
// queue. update must only change the supplied data, without I/O or config calls.
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

// replaceStateFile is called with mu held by the owning config operation.
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

// ClosureEvent captures one successful termination, independently of its delivery.
// Lock is the effective lock at closure, so delayed messages never reset expiry.
type ClosureEvent struct {
	ID       string    `json:"id"`
	ClosedAt time.Time `json:"closed_at"`
	Lock     LockFile  `json:"lock"`
}

// ClosureNotification remains as a receipt after delivery to deduplicate replay.
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

// QueueClosures merges newly observed collector events with pending ones.
// The enforcer serializes queueing with acknowledgements through its mutex.
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

// CheckClosureTransferReady protects both collector events and unsent bot messages.
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
