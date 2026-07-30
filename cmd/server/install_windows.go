//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// installService registers the server as a Task Scheduler task that runs on
// boot as SYSTEM. Run from an elevated (Administrator) prompt. This is the
// simple on-start variant; the all-local installer uses a fuller XML definition
// for AC-power/time-limit settings, which a dedicated server rarely needs.
func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating executable: %w", err)
	}
	cmd := exec.Command("schtasks", "/Create",
		"/SC", "ONSTART",
		"/TN", "PokerTrackerServer",
		"/TR", fmt.Sprintf(`"%s" --serve`, exe),
		"/RU", "SYSTEM",
		"/RL", "HIGHEST",
		"/F",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("schtasks (run as Administrator?): %w", err)
	}
	return nil
}
