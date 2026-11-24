//go:build windows

package bot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const createBreakawayFromJob = 0x01000000

const createNoWindow = 0x08000000

func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createBreakawayFromJob | createNoWindow}
}

func noWindowSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNoWindow}
}

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
