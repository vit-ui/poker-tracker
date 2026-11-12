//go:build !linux && !windows

package update

import (
	"fmt"
	"os"
)

func attachConsole() {}
func runHelper(string, []string) error {
	return fmt.Errorf("updates are supported on Windows and Linux")
}

type serviceState struct{ target string }

func inspectService(string, string, string, int) (*serviceState, error) {
	return nil, fmt.Errorf("unsupported platform")
}
func (*serviceState) close()         {}
func (*serviceState) stop() error    { return fmt.Errorf("unsupported platform") }
func (*serviceState) restore() error { return fmt.Errorf("unsupported platform") }

func (*serviceState) wasRunning() bool     { return false }
func (*serviceState) alive() (bool, error) { return false, fmt.Errorf("unsupported platform") }

func recoverPendingUpdate(string, string, string, int) (string, error) { return "", nil }
func (*serviceState) prepareUpdate(string, string, string) error       { return nil }
func (s *serviceState) previousExecutable() (string, error)            { return reservePrevious(s.target) }
func (*serviceState) confirmUpdate() error                             { return nil }
func (*serviceState) finishUpdate() error                              { return nil }

func moveExecutable(from, to string) error { return os.Rename(from, to) }
