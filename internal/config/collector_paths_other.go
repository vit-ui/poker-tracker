//go:build !windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
)

func collectorAbsolutePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding collector user home; send a full path: %w", err)
	}
	return filepath.Join(home, path), nil
}
