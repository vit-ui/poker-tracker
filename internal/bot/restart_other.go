//go:build !windows

package bot

import "syscall"

// detachedSysProcAttr and noWindowSysProcAttr are no-ops outside Windows —
// there's no Job Object to break away from, and no console window to
// suppress, on other platforms.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func noWindowSysProcAttr() *syscall.SysProcAttr {
	return nil
}
