//go:build !windows

package bot

import "syscall"

// detachedSysProcAttr is a no-op outside Windows — there's no Job Object to
// break away from on other platforms.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return nil
}
