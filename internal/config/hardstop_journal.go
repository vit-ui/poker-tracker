package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// HardstopDelivery is both the durable outgoing command and its confirmation
// receipt. Confirmed entries remain to suppress recovery of the same decision.
type HardstopDelivery struct {
	Lock          LockFile  `json:"lock"`
	Confirmed     bool      `json:"confirmed"`
	EffectiveLock *LockFile `json:"effective_lock,omitempty"`
}

func LoadHardstopDeliveries(path string) (map[string]HardstopDelivery, error) {
	mu.RLock()
	defer mu.RUnlock()
	return loadHardstopDeliveries(path)
}

func loadHardstopDeliveries(path string) (map[string]HardstopDelivery, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]HardstopDelivery), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading hard-stop deliveries: %w", err)
	}
	var entries map[string]HardstopDelivery
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decoding hard-stop deliveries: %w", err)
	}
	if entries == nil {
		entries = make(map[string]HardstopDelivery)
	}
	return entries, nil
}

func SaveHardstopDeliveries(path string, entries map[string]HardstopDelivery) error {
	mu.Lock()
	defer mu.Unlock()
	return saveHardstopDeliveries(path, entries)
}

func saveHardstopDeliveries(path string, entries map[string]HardstopDelivery) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding hard-stop deliveries: %w", err)
	}
	if err := replaceStateFile(path, data); err != nil {
		return fmt.Errorf("saving hard-stop deliveries: %w", err)
	}
	return nil
}

// MergeHardstopDeliveries preserves concurrent manual requests and worker
// confirmations. Confirmation is monotonic and an existing trigger retains its
// original clock, including after a lost acknowledgement.
func MergeHardstopDeliveries(path string, changes map[string]HardstopDelivery) (map[string]HardstopDelivery, error) {
	mu.Lock()
	defer mu.Unlock()
	entries, err := loadHardstopDeliveries(path)
	if err != nil {
		return nil, err
	}
	for id, change := range changes {
		if original, exists := entries[id]; exists {
			if !original.Confirmed && change.Confirmed {
				original.EffectiveLock = change.EffectiveLock
			}
			original.Confirmed = original.Confirmed || change.Confirmed
			entries[id] = original
		} else {
			entries[id] = change
		}
	}
	if err := saveHardstopDeliveries(path, entries); err != nil {
		return nil, err
	}
	return entries, nil
}
