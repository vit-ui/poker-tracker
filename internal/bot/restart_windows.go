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

// createNoWindow is CREATE_NO_WINDOW. This process's own console was already
// torn down (see hideConsole in main.go), so without this flag Windows
// allocates cmd.exe a brand new console — a visible terminal flashing open
// for the player during an otherwise invisible restart.
const createNoWindow = 0x08000000

func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createBreakawayFromJob | createNoWindow}
}

// noWindowSysProcAttr omits createBreakawayFromJob — used for selfRestart's
// retry after a Start failure, in case that failure was breakaway itself
// being rejected (some Task Scheduler configurations do). Keeps
// createNoWindow either way; that flag isn't the one at risk of rejection.
func noWindowSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNoWindow}
}
