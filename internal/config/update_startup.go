package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// UpdateStartup is an installation-scoped, single-update startup acknowledgement.
type UpdateStartup struct {
	Token   string `json:"token"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

func LoadUpdateStartup(path string) (*UpdateStartup, error) {
	mu.RLock()
	defer mu.RUnlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading update startup: %w", err)
	}
	var state UpdateStartup
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decoding update startup: %w", err)
	}
	return &state, nil
}
func SaveUpdateStartup(path string, state UpdateStartup) error {
	mu.Lock()
	defer mu.Unlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding update startup: %w", err)
	}
	if err := replaceStateFile(path, data); err != nil {
		return fmt.Errorf("saving update startup: %w", err)
	}
	return nil
}
