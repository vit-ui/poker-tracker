//go:build windows

package main

import (
	"fmt"
	"os"

	"github.com/vit-ui/self-tracker/internal/installation"
)

func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating executable: %w", err)
	}
	return installation.RegisterWindowsTask(installation.TaskName(InstallationID, true), exe)
}
