package update

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func extractPackage(installer, payload, output string) error {
	cmd := exec.Command(installer, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/SP-", "/UPDATEFILE="+payload, "/UPDATEOUT="+output)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	err := cmd.Run()
	// The extraction-only InitializeSetup callback deliberately returns False
	// before the wizard starts. Inno reports this as initialization exit code 1.
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return fmt.Errorf("installer did not finish extraction-only initialization: %v", err)
	}
	info, err := os.Lstat(output)
	if err != nil {
		return fmt.Errorf("installer did not extract %s: %w", payload, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("installer extracted an invalid payload")
	}
	return nil
}
