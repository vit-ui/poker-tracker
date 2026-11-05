//go:build windows

package bot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

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

// selfRestart waits for owner cleanup and runs the injected Windows task, preserving
// the role and disposable installation identity. The batch file lives beside
// config.json and deletes itself after requesting the restart. A detached
// helper survives the process's Task Scheduler Job Object; if Windows rejects
// breakaway at startup, the existing fallback starts it without that flag.
func selfRestart(configPath, taskName string) error {
	dir := filepath.Dir(configPath)
	batPath := filepath.Join(dir, "restart.bat")
	script := fmt.Sprintf(
		"@echo off\r\n"+
			"powershell.exe -NoProfile -NonInteractive -Command \"$ErrorActionPreference='Stop'; try { $p=[Diagnostics.Process]::GetProcessById(%d) } catch [ArgumentException] { exit 0 }; $p.WaitForExit()\"\r\n"+
			"if errorlevel 1 goto cleanup\r\n"+
			"ping -n 4 127.0.0.1 >nul\r\n"+
			"schtasks /Run /TN \"%s\"\r\n"+
			":cleanup\r\n"+
			"del \"%%~f0\"\r\n",
		os.Getpid(), taskName)

	if err := os.WriteFile(batPath, []byte(script), 0644); err != nil {
		return fmt.Errorf("writing restart script: %w", err)
	}

	cmd := exec.Command("cmd.exe", "/C", batPath)
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		cmd = exec.Command("cmd.exe", "/C", batPath)
		cmd.SysProcAttr = noWindowSysProcAttr()
		if err := cmd.Start(); err != nil {
			os.Remove(batPath)
			return fmt.Errorf("starting restart script: %w", err)
		}
	}
	return cmd.Process.Release()
}
