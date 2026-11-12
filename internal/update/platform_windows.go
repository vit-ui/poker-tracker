package update

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/vit-ui/self-tracker/internal/installation"
	"golang.org/x/sys/windows"
)

func attachConsole() {
	handle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err == nil && handle != 0 && handle != windows.InvalidHandle {
		return
	}
	attached, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole").Call(uintptr(^uint32(0)))
	if attached != 0 {
		if file, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stdout = file
			os.Stderr = file
		}
	}
}

func psLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
func powershell(script string) ([]byte, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.CombinedOutput()
}
func runHelper(path string, args []string) error {
	if windows.GetCurrentProcessToken().IsElevated() {
		cmd := exec.Command(path, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	escaped := make([]string, len(args))
	for i, arg := range args {
		escaped[i] = syscall.EscapeArg(arg)
	}
	script := "$ErrorActionPreference='Stop'; $p=Start-Process -FilePath " + psLiteral(path) + " -ArgumentList " + psLiteral(strings.Join(escaped, " ")) + " -Verb RunAs -Wait -PassThru; exit $p.ExitCode"
	output, err := powershell(script)
	if err != nil {
		return fmt.Errorf("elevated update: %w: %s", err, output)
	}
	return nil
}

type serviceState struct {
	target, name                     string
	Exists, Enabled, Running, Manual bool
	release                          func()
	parent                           int
}

func inspectService(target, role, id string, parent int) (*serviceState, error) {
	s := &serviceState{target: target, name: installation.TaskName(id, role == "server"), parent: parent}
	release, err := installation.AcquireTransferLock(filepath.Dir(target))
	if err != nil {
		return nil, err
	}
	s.release = release
	script := `$ErrorActionPreference='Stop'; $scheduler=New-Object -ComObject Schedule.Service; $scheduler.Connect(); $tasks=$scheduler.GetFolder('\').GetTasks(1); $task=$null; foreach($t in $tasks){if($t.Name -eq ` + psLiteral(s.name) + `){$task=$t}}; $exists=$false; $enabled=$false; $running=$false; if($task){ [xml]$xml=$task.Xml; if([string]$xml.Task.Actions.Exec.Command -ne ` + psLiteral(target) + `){throw 'Scheduled task belongs to another installation'}; $exists=$true; $enabled=[bool]$task.Enabled; $running=($task.GetInstances(0).Count -gt 0) }; $owners=@(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq ` + psLiteral(target) + ` -and $_.ProcessId -ne ` + strconv.Itoa(parent) + ` }); if(` + psLiteral(role) + ` -ne 'server' -and (Get-Process -Name PokerStars -ErrorAction SilentlyContinue)){throw 'Close PokerStars before updating'}; @{Exists=$exists;Enabled=$enabled;Running=$running;Manual=($owners.Count -gt 0 -and -not $running)} | ConvertTo-Json -Compress`
	output, err := powershell(script)
	if err != nil {
		s.close()
		return nil, fmt.Errorf("checking installation: %w: %s", err, output)
	}
	if err := json.Unmarshal(output, s); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}
func (s *serviceState) close() {
	if s.release != nil {
		s.release()
	}
}
func (s *serviceState) taskScript() string {
	return `$ErrorActionPreference='Stop'; $scheduler=New-Object -ComObject Schedule.Service; $scheduler.Connect(); $task=$scheduler.GetFolder('\').GetTask(` + psLiteral(s.name) + `); `
}
func (s *serviceState) stop() error {
	if s.Exists {
		output, err := powershell(s.taskScript() + `$task.Enabled=$false`)
		if err != nil {
			return fmt.Errorf("suspending scheduled recovery: %w: %s", err, output)
		}
	}
	// The terminal caller remains mapped until the helper returns, but is not a service owner.
	return installation.StopExcept(filepath.Dir(s.target), s.parent)
}
func (s *serviceState) restore() error {
	if s.Exists {
		script := s.taskScript()
		if s.Running {
			script += `$task.Enabled=$true; $instance=$task.Run($null); `
		}
		if s.Enabled {
			script += `$task.Enabled=$true`
		} else {
			script += `$task.Enabled=$false`
		}
		output, err := powershell(script)
		if err != nil {
			return fmt.Errorf("restoring scheduled task: %w: %s", err, output)
		}
	}
	if s.Manual {
		output, err := powershell(`$ErrorActionPreference='Stop'; @(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq ` + psLiteral(s.target) + ` -and $_.ProcessId -ne ` + strconv.Itoa(s.parent) + ` }).Count`)
		if err != nil {
			return fmt.Errorf("checking manual owner before restart: %w: %s", err, output)
		}
		count, err := strconv.Atoi(strings.TrimSpace(string(output)))
		if err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		cmd := exec.Command(s.target, "--serve")
		cmd.Dir = filepath.Dir(s.target)
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	return nil
}

func (s *serviceState) wasRunning() bool { return s.Running || s.Manual }
func (s *serviceState) alive() (bool, error) {
	script := `$ErrorActionPreference='Stop'; @(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq ` + psLiteral(s.target) + ` -and $_.ProcessId -ne ` + strconv.Itoa(s.parent) + ` }).Count -gt 0`
	if s.Exists && s.Running {
		script = s.taskScript() + `$task.GetInstances(0).Count -gt 0`
	}
	output, err := powershell(script)
	if err != nil {
		return false, fmt.Errorf("checking startup owner: %w: %s", err, output)
	}
	return strings.TrimSpace(string(output)) == "True", nil
}
