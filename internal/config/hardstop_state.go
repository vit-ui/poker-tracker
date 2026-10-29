package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// The active lock and receipts share one atomic replacement. A crash can never
// leave a receipt claiming enforcement without its corresponding durable lock.
// Receipts survive expiry and --check-lock cleanup; dropping old IDs would let
// delayed recovery restart an already served lock.
type lockState struct {
	LockFile
	Triggers map[string]time.Time `json:"triggers,omitempty"`
}

func loadLockState(path string) (lockState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return lockState{}, nil
	}
	if err != nil {
		return lockState{}, fmt.Errorf("reading lockfile: %w", err)
	}
	var state lockState
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("parsing lockfile: %w", err)
	}
	return state, nil
}

func saveLockState(path string, state lockState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding hard-stop state: %w", err)
	}
	if err := replaceStateFile(path, data); err != nil {
		return fmt.Errorf("saving hard-stop state: %w", err)
	}
	return nil
}

// ApplyHardstop returns the effective active lock and whether this is a new
// trigger. A duplicate never changes expiry; an expired duplicate does not
// reactivate protection, but an unrelated active lock still remains enforced.
func ApplyHardstop(path string, proposed LockFile, now time.Time) (*LockFile, bool, error) {
	mu.Lock()
	defer mu.Unlock()
	state, err := loadLockState(path)
	if err != nil {
		return nil, false, err
	}
	if proposed.TriggerID != "" {
		if _, seen := state.Triggers[proposed.TriggerID]; seen {
			if state.ExpiresAt.After(now) {
				lock := state.LockFile
				return &lock, false, nil
			}
			return nil, false, nil
		}
	}
	effective := proposed
	if state.ExpiresAt.After(effective.ExpiresAt) {
		effective = state.LockFile
	}
	state.LockFile = effective
	if proposed.TriggerID != "" {
		if state.Triggers == nil {
			state.Triggers = make(map[string]time.Time)
		}
		state.Triggers[proposed.TriggerID] = effective.ExpiresAt
	}
	if err := saveLockState(path, state); err != nil {
		return nil, false, err
	}
	if !effective.ExpiresAt.After(now) {
		return nil, true, nil
	}
	return &effective, true, nil
}
