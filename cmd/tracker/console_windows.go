//go:build windows

package main

import "syscall"

// hideConsole detaches the process from any console it's attached to.
// -H=windowsgui (see the release build command) should already prevent one
// from being created in the first place. taskSchedulerXML now runs the
// service via BootTrigger as SYSTEM in Session 0, which has no desktop to
// allocate a console on anyway — but this previously ran under a
// group-based elevated principal (RunLevel HighestAvailable, no explicit
// LogonType), which Microsoft documents as an interactive logon type
// (TASK_LOGON_GROUP) and which was observed in practice to still allocate
// a console for a windowsgui binary. FreeConsole is kept as the standard
// runtime fallback for that case — closes/hides whatever console exists
// rather than depending on it never having been created.
func hideConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	freeConsole := kernel32.NewProc("FreeConsole")
	freeConsole.Call()
}
