//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const systemdUnitPath = "/etc/systemd/system/tracker-server.service"

// installService writes a systemd unit that runs the server on boot and
// restarts it on failure, then enables and starts it. Requires root (it writes
// under /etc/systemd/system and drives systemctl), so run it with sudo. The
// unit runs as root by default; edit it to add a dedicated User= if you prefer.
func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating executable: %w", err)
	}
	unit := fmt.Sprintf(`[Unit]
Description=Poker Tracker server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --serve
WorkingDirectory=%s
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
`, exe, filepath.Dir(exe))

	if err := os.WriteFile(systemdUnitPath, []byte(unit), 0644); err != nil {
		return fmt.Errorf("writing %s (run with sudo?): %w", systemdUnitPath, err)
	}
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", "--now", "tracker-server"},
	} {
		cmd := exec.Command("systemctl", args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("systemctl %v: %w", args, err)
		}
	}
	return nil
}
