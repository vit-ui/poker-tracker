package update

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func lockRequested(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		file.Close()
		return nil, err
	}
	return func() { file.Close() }, nil
}

func startRequestedHelper(helper string, args []string, role, installationID, requestID string) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("run the installed elevated tracker task, or use --update from a terminal")
	}
	cmd := exec.Command(helper, args...)
	// A detached helper must survive Task Scheduler stopping the owner job.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x01000000 | 0x08000000}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting detached update helper: %w", err)
	}
	return cmd.Process.Release()
}

func pokerStarsOpen() (bool, error) {
	output, err := powershell(`$ErrorActionPreference='Stop'; if(Get-Process -Name PokerStars -ErrorAction SilentlyContinue){'open'}else{'closed'}`)
	if err != nil {
		return false, fmt.Errorf("listing PokerStars processes: %w: %s", err, output)
	}
	return strings.TrimSpace(string(output)) == "open", nil
}
