package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// CollectorSettings contains only PC-owned fields. In a change, nil means leave
// the field unchanged; in a confirmed snapshot, nil means not configured.
type CollectorSettings struct {
	ScreenName        *string `json:"screen_name,omitempty"`
	HandHistory       *string `json:"hand_history,omitempty"`
	TournamentSummary *string `json:"tournament_summary,omitempty"`
}

func CollectionSettings(cfg *Config) CollectorSettings {
	return CollectorSettings{cfg.Setup.PokerStarsScreenName, cfg.Paths.HandHistory, cfg.Paths.TournamentSummary}
}

func (s CollectorSettings) Apply(cfg *Config) {
	if s.ScreenName != nil {
		cfg.Setup.PokerStarsScreenName = s.ScreenName
	}
	if s.HandHistory != nil {
		cfg.Paths.HandHistory = s.HandHistory
	}
	if s.TournamentSummary != nil {
		cfg.Paths.TournamentSummary = s.TournamentSummary
	}
}

func (s CollectorSettings) Complete() bool {
	return s.ScreenName != nil && s.HandHistory != nil && s.TournamentSummary != nil
}

// Validate checks syntax only. Folder existence and relative-path resolution
// belong to the collector, never the server accepting an offline edit.
func (s CollectorSettings) Validate() error {
	if s.ScreenName != nil && (strings.TrimSpace(*s.ScreenName) == "" || strings.ContainsAny(*s.ScreenName, `/\<>:"|?*`+"\x00") || *s.ScreenName == "." || *s.ScreenName == "..") {
		return fmt.Errorf("send a screen name containing a single folder name")
	}
	for _, path := range []*string{s.HandHistory, s.TournamentSummary} {
		if path != nil && (strings.TrimSpace(*path) == "" || strings.ContainsRune(*path, 0)) {
			return fmt.Errorf("send a nonempty folder path")
		}
	}
	return nil
}

type CollectorChange struct {
	ID       string            `json:"id"`
	Settings CollectorSettings `json:"settings"`
}

// CollectorSettingsState lives on the server. Failed validation remains visible
// and durable until a new edit replaces it; reconnection alone does not retry an
// invalid path. A missing acknowledgement leaves a change pending for reconnect.
type CollectorSettingsState struct {
	Confirmed       *CollectorSettings `json:"confirmed,omitempty"`
	Pending         *CollectorChange   `json:"pending,omitempty"`
	Error           string             `json:"error,omitempty"`
	Watching        bool               `json:"watching"`
	CollectionError string             `json:"collection_error,omitempty"`
}

func (s CollectorSettingsState) Desired() CollectorSettings {
	var cfg Config
	if s.Confirmed != nil {
		s.Confirmed.Apply(&cfg)
	}
	if s.Pending != nil {
		s.Pending.Settings.Apply(&cfg)
	}
	return CollectionSettings(&cfg)
}

func LoadCollectorSettings(path string) (CollectorSettingsState, error) {
	mu.RLock()
	defer mu.RUnlock()
	var state CollectorSettingsState
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("reading collector settings: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decoding collector settings: %w", err)
	}
	if state.Pending != nil && state.Pending.ID == "" {
		return state, fmt.Errorf("collector change has no identity")
	}
	return state, nil
}

func SaveCollectorSettings(path string, state CollectorSettingsState) error {
	mu.Lock()
	defer mu.Unlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding collector settings: %w", err)
	}
	if err := replaceStateFile(path, data); err != nil {
		return fmt.Errorf("saving collector settings: %w", err)
	}
	return nil
}
