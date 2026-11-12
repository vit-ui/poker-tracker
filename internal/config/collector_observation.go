package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// CollectorObservation is a display copy, never proof of live enforcement.
type CollectorObservation struct {
	Known bool      `json:"known"`
	Lock  *LockFile `json:"lock,omitempty"`
}

func LoadCollectorObservation(path string) (CollectorObservation, error) {
	mu.RLock()
	defer mu.RUnlock()
	var state CollectorObservation
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("reading collector observation: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decoding collector observation: %w", err)
	}
	return state, nil
}

func SaveCollectorObservation(path string, state CollectorObservation) error {
	mu.Lock()
	defer mu.Unlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding collector observation: %w", err)
	}
	if err := replaceStateFile(path, data); err != nil {
		return fmt.Errorf("saving collector observation: %w", err)
	}
	return nil
}
