package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// UpdateRecovery is saved before the updater suspends the installation task.
// Executable paths are basenames relative to the installation directory.
type UpdateRecovery struct {
	Target         string `json:"target"`
	Previous       string `json:"previous"`
	Staged         string `json:"staged"`
	PreviousSHA256 string `json:"previous_sha256"`
	NextSHA256     string `json:"next_sha256"`
	OldVersion     string `json:"old_version"`
	NewVersion     string `json:"new_version"`
	Task           string `json:"task"`
	TaskExists     bool   `json:"task_exists"`
	TaskEnabled    bool   `json:"task_enabled"`
	TaskRunning    bool   `json:"task_running"`
	Manual         bool   `json:"manual"`
	Confirmed      bool   `json:"confirmed"`
}

func LoadUpdateRecovery(path string) (*UpdateRecovery, error) {
	mu.RLock()
	defer mu.RUnlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading update recovery: %w", err)
	}
	var state UpdateRecovery
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decoding update recovery: %w", err)
	}
	return &state, nil
}

func SaveUpdateRecovery(path string, state UpdateRecovery) error {
	mu.Lock()
	defer mu.Unlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding update recovery: %w", err)
	}
	file, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening update recovery: %w", err)
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("flushing update recovery: %w", err)
	}
	if err := commitUpdateRecovery(path+".tmp", path); err != nil {
		return fmt.Errorf("committing update recovery: %w", err)
	}
	return nil
}

func DeleteUpdateRecovery(path string) error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deleting update recovery: %w", err)
	}
	if err := os.Remove(path + ".tmp"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deleting partial update recovery: %w", err)
	}
	return nil
}
