//go:build windows

package main

import "syscall"

// hideConsole detaches the process from any console it's attached to.
// -H=windowsgui (see the release build command) should already prevent one
// from being created in the first place, but a Task Scheduler task running
// under a group-based elevated principal (RunLevel HighestAvailable, no
// explicit LogonType — see taskSchedulerXML) is documented by Microsoft as
// an interactive logon type (TASK_LOGON_GROUP), and that combination has
// been observed in practice to still allocate a console for a windowsgui
// binary. FreeConsole is the standard runtime fallback for exactly this
// case — closes/hides whatever console exists rather than depending on it
// never having been created.
func hideConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	freeConsole := kernel32.NewProc("FreeConsole")
	freeConsole.Call()
}
