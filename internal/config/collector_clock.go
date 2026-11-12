package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// CollectorClock retains calendar context, not an adjustment to absolute time.
type CollectorClock struct {
	Name          string `json:"name"`
	OffsetSeconds int    `json:"offset_seconds"`
}

func (clock CollectorClock) Location() *time.Location {
	if clock.Name != "" && clock.Name != "Local" {
		if loc, err := time.LoadLocation(clock.Name); err == nil {
			return loc
		}
	}
	return time.FixedZone(clock.Name, clock.OffsetSeconds)
}

func LoadCollectorClock(path string) (*CollectorClock, error) {
	mu.RLock()
	defer mu.RUnlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading collector clock: %w", err)
	}
	var clock CollectorClock
	if err := json.Unmarshal(data, &clock); err != nil {
		return nil, fmt.Errorf("decoding collector clock: %w", err)
	}
	return &clock, nil
}

func SaveCollectorClock(path string, clock CollectorClock) error {
	mu.Lock()
	defer mu.Unlock()
	data, err := json.MarshalIndent(clock, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding collector clock: %w", err)
	}
	if err := replaceStateFile(path, data); err != nil {
		return fmt.Errorf("saving collector clock: %w", err)
	}
	return nil
}
