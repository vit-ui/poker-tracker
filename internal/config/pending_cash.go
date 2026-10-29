package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

// PendingCash is a successfully processed table snapshot and its last observed
// activity. Absolute activity times restore the remaining quiet interval;
// session dates retain their stored wall-clock representation.
type PendingCash struct {
	FileID       string              `json:"file_id"`
	Session      parsers.CashSession `json:"session"`
	ClientClock  time.Time           `json:"client_clock,omitempty"`
	LastActivity time.Time           `json:"last_activity"`
}

func LoadPendingCash(path string) ([]PendingCash, error) {
	mu.RLock()
	defer mu.RUnlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading pending cash: %w", err)
	}
	var journal struct {
		Pending []PendingCash `json:"pending"`
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		return nil, fmt.Errorf("decoding pending cash: %w", err)
	}
	for _, entry := range journal.Pending {
		if entry.FileID == "" || entry.LastActivity.IsZero() || entry.Session.FirstHandID == "" {
			return nil, fmt.Errorf("invalid pending cash snapshot")
		}
	}
	return journal.Pending, nil
}

func SavePendingCash(path string, pending []PendingCash) error {
	mu.Lock()
	defer mu.Unlock()
	data, err := json.MarshalIndent(struct {
		Pending []PendingCash `json:"pending"`
	}{pending}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding pending cash: %w", err)
	}
	if err := replaceStateFile(path, data); err != nil {
		return fmt.Errorf("saving pending cash: %w", err)
	}
	return nil
}
