//go:build windows

package bot

import "syscall"

// createBreakawayFromJob is CREATE_BREAKAWAY_FROM_JOB — without it, the
// detached restart helper spawned by handleSheetCommand inherits the Job
// Object Task Scheduler assigns to "Poker Tracker"'s process tree, and gets
// killed alongside it the moment this process exits, before it ever reaches
// schtasks /Run. Confirmed in practice: without this flag, /sheet killed the
// tracker and nothing ever restarted it.
const createBreakawayFromJob = 0x01000000

func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createBreakawayFromJob}
}
