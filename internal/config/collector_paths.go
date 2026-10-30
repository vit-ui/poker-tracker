package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveCollectorPath runs on the collecting machine. Relative Windows paths
// use the active desktop user's profile, even when the task runs as SYSTEM.
func ResolveCollectorPath(input string) (string, error) {
	path := strings.Trim(strings.TrimSpace(input), `"`)
	if path == "" {
		return "", fmt.Errorf("send a folder path")
	}
	path, err := collectorAbsolutePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("reading folder %s: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a folder", path)
	}
	return filepath.Clean(path), nil
}

// ResolveCollection validates and resolves only edited fields, then verifies
// configured screen-name subfolders before activating a complete watcher.
func ResolveCollection(cfg *Config, change CollectorSettings) (CollectorSettings, error) {
	if err := change.Validate(); err != nil {
		return CollectorSettings{}, err
	}
	for _, field := range []**string{&change.HandHistory, &change.TournamentSummary} {
		if *field == nil {
			continue
		}
		path, err := ResolveCollectorPath(**field)
		if err != nil {
			return CollectorSettings{}, err
		}
		*field = &path
	}
	copy := *cfg
	change.Apply(&copy)
	settings := CollectionSettings(&copy)
	if err := settings.Validate(); err != nil {
		return CollectorSettings{}, err
	}
	if settings.ScreenName != nil {
		for _, base := range []*string{settings.HandHistory, settings.TournamentSummary} {
			if base == nil {
				continue
			}
			path := filepath.Join(*base, *settings.ScreenName)
			info, err := os.Stat(path)
			if err != nil {
				return CollectorSettings{}, fmt.Errorf("reading player folder %s: %w", path, err)
			}
			if !info.IsDir() {
				return CollectorSettings{}, fmt.Errorf("%s is not a folder", path)
			}
		}
	}
	return settings, nil
}
