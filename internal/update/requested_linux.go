package update

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/vit-ui/self-tracker/internal/installation"
	"golang.org/x/sys/unix"
)

func lockRequested(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return func() { file.Close() }, nil
}

func startRequestedHelper(helper string, args []string, role, installationID, requestID string) error {
	// A process in the server's systemd cgroup would be killed by its own
	// systemctl stop. Launch a separate transient user unit when managed.
	unit := installation.ServerServiceName(installationID)
	output, err := exec.Command("systemctl", "--user", "show", unit, "--property=MainPID", "--value").Output()
	if err == nil && strings.TrimSpace(string(output)) == fmt.Sprint(os.Getpid()) {
		command := []string{"--user", "--collect", "--property=Type=exec", "--", helper}
		command = append(command, args...)
		output, err := exec.Command("systemd-run", command...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("starting independent update helper: %w: %s", err, output)
		}
		return nil
	}
	cmd := exec.Command(helper, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func pokerStarsOpen() (bool, error) { return false, nil }
