package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/vit-ui/self-tracker/internal/config"
)

const recoveryHelperName = ".tracker-update-recovery.exe"

func reservePrevious(target string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(target), ".tracker-previous-*")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func executableSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateRecovery(state config.UpdateRecovery, target, task string) error {
	if !strings.EqualFold(state.Target, filepath.Base(target)) || !strings.EqualFold(state.Task, task) {
		return fmt.Errorf("update recovery belongs to another installation")
	}
	for _, item := range []struct{ name, prefix string }{{state.Previous, ".tracker-previous-"}, {state.Staged, ".tracker-update-"}} {
		if filepath.Base(item.name) != item.name || !strings.HasPrefix(item.name, item.prefix) || item.name == recoveryHelperName {
			return fmt.Errorf("invalid update recovery executable path")
		}
	}
	for _, value := range []string{state.PreviousSHA256, state.NextSHA256} {
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("invalid update recovery executable checksum")
		}
	}
	for _, value := range []string{state.OldVersion, state.NewVersion} {
		if _, err := versionParts(value); err != nil {
			return err
		}
	}
	if (!state.TaskExists && (state.TaskEnabled || state.TaskRunning)) || (state.TaskRunning && state.Manual) {
		return fmt.Errorf("invalid update recovery task state")
	}
	return nil
}

// recoverExecutable is repeatable at every rename boundary. A missing backup
// is safe only when the installed executable is still the previous image.
func recoverExecutable(state config.UpdateRecovery, target string) error {
	if state.Confirmed {
		got, err := executableSHA256(target)
		if err != nil {
			return err
		}
		if got != state.NextSHA256 {
			return fmt.Errorf("confirmed update executable checksum changed")
		}
		return nil
	}
	previous := filepath.Join(filepath.Dir(target), state.Previous)
	hash, err := executableSHA256(previous)
	if errors.Is(err, os.ErrNotExist) {
		hash, err = executableSHA256(target)
		if err != nil {
			return err
		}
		if hash != state.PreviousSHA256 {
			return fmt.Errorf("previous executable is missing; refusing to guess a recovery image")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if hash != state.PreviousSHA256 {
		return fmt.Errorf("previous executable checksum changed")
	}
	if _, err := os.Stat(target); err == nil {
		staged := filepath.Join(filepath.Dir(target), state.Staged)
		if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := moveExecutable(target, staged); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return moveExecutable(previous, target)
}
