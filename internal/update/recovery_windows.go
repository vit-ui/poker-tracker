package update

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/installation"
	"golang.org/x/sys/windows"
)

func (s *serviceState) recoveryPath() string {
	return filepath.Join(filepath.Dir(s.target), "update_recovery.json")
}
func (s *serviceState) recoveryTask() string { return s.name + " Update Recovery" }
func (s *serviceState) recoveryHelper() string {
	return filepath.Join(filepath.Dir(s.target), recoveryHelperName)
}
func (s *serviceState) recoveryArguments() string {
	return "--recover-update " + syscall.EscapeArg(s.target)
}

func (s *serviceState) prepareUpdate(staged, oldVersion, newVersion string) error {
	prior, err := config.LoadUpdateRecovery(s.recoveryPath())
	if err != nil {
		return err
	}
	if prior != nil {
		return fmt.Errorf("an unfinished update must be recovered before replacing this installation")
	}
	// TASK_CREATE refuses a name collision; it never overwrites another task.
	output, err := powershell(`$ErrorActionPreference='Stop'; $scheduler=New-Object -ComObject Schedule.Service; $scheduler.Connect(); foreach($t in $scheduler.GetFolder('\').GetTasks(1)){if($t.Name -eq ` + psLiteral(s.recoveryTask()) + `){throw 'Update recovery task already exists'}}`)
	if err != nil {
		return fmt.Errorf("checking recovery task: %w: %s", err, output)
	}
	previous, err := reservePrevious(s.target)
	if err != nil {
		return err
	}
	oldHash, err := executableSHA256(s.target)
	if err != nil {
		return err
	}
	newHash, err := executableSHA256(staged)
	if err != nil {
		return err
	}
	state := config.UpdateRecovery{Target: filepath.Base(s.target), Previous: filepath.Base(previous), Staged: filepath.Base(staged), PreviousSHA256: oldHash, NextSHA256: newHash, OldVersion: oldVersion, NewVersion: newVersion, Task: s.name, TaskExists: s.Exists, TaskEnabled: s.Enabled, TaskRunning: s.Running, Manual: s.Manual}
	if err := validateRecovery(state, s.target, s.name); err != nil {
		return err
	}
	helper := s.recoveryHelper()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	partial := helper + ".tmp"
	if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := copyFile(exe, partial, 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(partial, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(partial)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(helper)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("committing recovery helper: %w", err)
	}
	if err := config.SaveUpdateRecovery(s.recoveryPath(), state); err != nil {
		return err
	}
	s.recovery = &state
	xml := installation.WindowsCommandTaskXML(helper, filepath.Dir(s.target), s.recoveryArguments(), time.Now())
	output, err = powershell(`$ErrorActionPreference='Stop'; $scheduler=New-Object -ComObject Schedule.Service; $scheduler.Connect(); $scheduler.GetFolder('\').RegisterTask(` + psLiteral(s.recoveryTask()) + `,` + psLiteral(xml) + `,2,$null,$null,5) | Out-Null`)
	if err != nil {
		return errors.Join(fmt.Errorf("registering update recovery: %w: %s", err, output), s.finishUpdate())
	}
	return nil
}

func (s *serviceState) previousExecutable() (string, error) {
	if s.recovery == nil {
		return "", fmt.Errorf("update recovery was not prepared")
	}
	return filepath.Join(filepath.Dir(s.target), s.recovery.Previous), nil
}

func (s *serviceState) confirmUpdate() error {
	if s.recovery == nil {
		return fmt.Errorf("update recovery was not prepared")
	}
	state := *s.recovery
	state.Confirmed = true
	if err := config.SaveUpdateRecovery(s.recoveryPath(), state); err != nil {
		return err
	}
	s.recovery = &state
	return nil
}

func (s *serviceState) finishUpdate() error {
	if s.recovery == nil {
		return nil
	}
	// Delete the definition, not its running instance: recovery can clean up itself.
	script := `$ErrorActionPreference='Stop'; $scheduler=New-Object -ComObject Schedule.Service; $scheduler.Connect(); $folder=$scheduler.GetFolder('\'); foreach($task in $folder.GetTasks(1)){if($task.Name -eq ` + psLiteral(s.recoveryTask()) + `){[xml]$xml=$task.Xml; if([string]$xml.Task.Actions.Exec.Command -ne ` + psLiteral(s.recoveryHelper()) + ` -or [string]$xml.Task.Actions.Exec.Arguments -ne ` + psLiteral(s.recoveryArguments()) + `){throw 'Recovery task belongs to another installation'}; $folder.DeleteTask($task.Name,0)}}`
	output, err := powershell(script)
	if err != nil {
		return fmt.Errorf("removing update recovery task: %w: %s", err, output)
	}
	if err := config.DeleteUpdateRecovery(s.recoveryPath()); err != nil {
		return err
	}
	for _, name := range []string{s.recovery.Staged, s.recovery.Previous, recoveryHelperName} {
		// A mapped previous image/helper may remain until its process exits. These
		// reserved executable artifacts are removed by normal uninstall or the next update.
		os.Remove(filepath.Join(filepath.Dir(s.target), name))
	}
	s.recovery = nil
	return nil
}

func recoverPendingUpdate(target, role, id string, parent int) (string, error) {
	root := filepath.Dir(target)
	state, err := config.LoadUpdateRecovery(filepath.Join(root, "update_recovery.json"))
	if err != nil || state == nil {
		return "", err
	}
	name := installation.TaskName(id, role == "server")
	if err := validateRecovery(*state, target, name); err != nil {
		return "", err
	}
	release, err := installation.AcquireTransferLock(root)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return "", nil
	} // The active updater owns the transaction.
	if err != nil {
		return "", err
	}
	defer release()
	// Re-read after taking ownership; another helper may have completed cleanup.
	state, err = config.LoadUpdateRecovery(filepath.Join(root, "update_recovery.json"))
	if err != nil || state == nil {
		return "", err
	}
	if err := validateRecovery(*state, target, name); err != nil {
		return "", err
	}
	s := &serviceState{target: target, name: name, Exists: state.TaskExists, Enabled: state.TaskEnabled, Running: state.TaskRunning, Manual: state.Manual, parent: parent, recovery: state}
	if s.Exists {
		output, err := powershell(s.taskScript() + `[xml]$xml=$task.Xml; if([string]$xml.Task.Actions.Exec.Command -ne ` + psLiteral(target) + `){throw 'Installation task target changed during update'}`)
		if err != nil {
			return "", fmt.Errorf("checking interrupted installation: %w: %s", err, output)
		}
	}
	if err := s.stop(); err != nil {
		return "", fmt.Errorf("stopping interrupted installation: %w", err)
	}
	if err := recoverExecutable(*state, target); err != nil {
		return "", fmt.Errorf("restoring interrupted executable: %w", err)
	}
	version := state.OldVersion
	if state.Confirmed {
		version = state.NewVersion
	}
	if err := restartAndConfirm(s, version); err != nil {
		return "", fmt.Errorf("restarting recovered installation: %w", err)
	}
	if err := s.finishUpdate(); err != nil {
		return "", err
	}
	mode := "previous"
	if state.Confirmed {
		mode = "confirmed"
	}
	slog.Info("recovered interrupted update", "executable", filepath.Base(target), "version", version, "image", mode)
	return version, nil
}
