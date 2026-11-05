package config

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
)

// OperationalSettings moves player policy without backend or pairing identity.
type OperationalSettings struct {
	TelegramChatID *string       `json:"telegram_chat_id"`
	Alerts         Alerts        `json:"alerts"`
	Hardstop       Hardstop      `json:"hardstop"`
	Schedule       Schedule      `json:"schedule"`
	Notifications  Notifications `json:"notifications"`
}

// GetOperational selects only transferable player settings.
func GetOperational(cfg *Config) OperationalSettings {
	return OperationalSettings{cfg.Setup.TelegramChatID, cfg.Alerts, cfg.Hardstop, cfg.Schedule, cfg.Notifications}
}

// ApplyOperational refuses a foreign saved chat and preserves an existing chat
// when the source has none. Backend, paths and pairing remain destination-owned.
func ApplyOperational(cfg *Config, settings OperationalSettings) error {
	if settings.TelegramChatID != nil {
		id, err := strconv.ParseInt(*settings.TelegramChatID, 10, 64)
		if err != nil || id == 0 {
			return fmt.Errorf("source Telegram chat is invalid")
		}
		if cfg.Setup.TelegramChatID != nil && *cfg.Setup.TelegramChatID != *settings.TelegramChatID {
			return fmt.Errorf("destination Telegram chat differs from source")
		}
	} else {
		settings.TelegramChatID = cfg.Setup.TelegramChatID
	}
	RestoreOperational(cfg, settings)
	return nil
}

// RestoreOperational restores a previously verified transfer-owned policy.
func RestoreOperational(cfg *Config, settings OperationalSettings) {
	cfg.Setup.TelegramChatID = settings.TelegramChatID
	cfg.Alerts, cfg.Hardstop, cfg.Schedule, cfg.Notifications = settings.Alerts, settings.Hardstop, settings.Schedule, settings.Notifications
}

// DeploymentTransferState is the durable destination reservation and receipt.
// Payload bytes live beside it and are acknowledged only after their fsync.
type DeploymentTransferState struct {
	ID         string               `json:"id"`
	Phase      string               `json:"phase"`
	SHA256     string               `json:"sha256"`
	TotalBytes int64                `json:"total_bytes"`
	NextOffset int64                `json:"next_offset"`
	Before     *OperationalSettings `json:"before,omitempty"`
	Backend    string               `json:"backend"`
	SheetID    *string              `json:"sheet_id"`
}

// DeploymentInProgress retains reservations for unknown/nonterminal phases.
func DeploymentInProgress(state *DeploymentTransferState) bool {
	return state != nil && state.Phase != "activated" && state.Phase != "rolled-back"
}

// LoadDeploymentTransfer reads a reservation without inventing a fresh one.
func LoadDeploymentTransfer(path string) (*DeploymentTransferState, error) {
	mu.Lock()
	defer mu.Unlock()
	return loadDeploymentTransfer(path)
}

func loadDeploymentTransfer(path string) (*DeploymentTransferState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading deployment transfer: %w", err)
	}
	var state *DeploymentTransferState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing deployment transfer: %w", err)
	}
	if state == nil {
		return nil, fmt.Errorf("deployment transfer state is empty")
	}
	digest, decodeErr := hex.DecodeString(state.SHA256)
	if state.ID == "" || state.Phase == "" || state.TotalBytes < 0 || state.NextOffset < 0 || state.NextOffset > state.TotalBytes || state.Before == nil || ((state.TotalBytes > 0 && (decodeErr != nil || len(digest) != 32)) || (state.TotalBytes == 0 && (state.SHA256 != "" || (state.Phase != "reserved" && state.Phase != "rolling-back" && state.Phase != "rolled-back")))) || (state.Backend != "sqlite" && state.Backend != "sheets") {
		return nil, fmt.Errorf("invalid deployment transfer reservation")
	}
	return state, nil
}

// SaveDeploymentTransfer atomically records durable progress under config ownership.
func SaveDeploymentTransfer(path string, state *DeploymentTransferState) error {
	mu.Lock()
	defer mu.Unlock()
	return saveTransferJSON(path, state)
}

// CommitDeploymentSettings changes policy only for the reserved transfer. The
// caller has verified current policy ownership before invoking rollback.
func CommitDeploymentSettings(path, id string, settings OperationalSettings, restore bool) error {
	mu.Lock()
	defer mu.Unlock()
	state, err := loadDeploymentTransfer(filepath.Join(filepath.Dir(path), "deployment_transfer.json"))
	if err != nil {
		return err
	}
	if !DeploymentInProgress(state) || state.ID != id {
		return fmt.Errorf("deployment settings are not reserved by this transfer")
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	if restore {
		RestoreOperational(cfg, settings)
	} else if err := ApplyOperational(cfg, settings); err != nil {
		return err
	}
	return saveConfigUnchecked(path, cfg)
}

// CommitDeploymentPending replaces the queue only for the reserved transfer.
func CommitDeploymentPending(path, id string, pending []PendingTournament) error {
	mu.Lock()
	defer mu.Unlock()
	state, err := loadDeploymentTransfer(filepath.Join(filepath.Dir(path), "deployment_transfer.json"))
	if err != nil {
		return err
	}
	if !DeploymentInProgress(state) || state.ID != id {
		return fmt.Errorf("deployment questions are not reserved by this transfer")
	}
	return savePendingTournamentsUnchecked(path, pending)
}

func guardDeploymentSettings(path string, cfg *Config) error {
	state, err := loadDeploymentTransfer(filepath.Join(filepath.Dir(path), "deployment_transfer.json"))
	if err != nil {
		return err
	}
	if !DeploymentInProgress(state) {
		return nil
	}
	before, err := loadConfig(path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(GetOperational(before), GetOperational(cfg)) {
		return fmt.Errorf("player settings are reserved by a deployment transfer")
	}
	return nil
}

func guardDeploymentPending(path string, pending []PendingTournament) error {
	state, err := loadDeploymentTransfer(filepath.Join(filepath.Dir(path), "deployment_transfer.json"))
	if err != nil {
		return err
	}
	if !DeploymentInProgress(state) {
		return nil
	}
	before, err := loadPendingTournaments(path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, pending) {
		return fmt.Errorf("tournament questions are reserved by a deployment transfer")
	}
	return nil
}
