package client

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func killPokerStars() error {
	running, err := pokerStarsRunning()
	if err != nil {
		return err
	}
	if !running {
		return nil
	}
	directory, err := windows.GetSystemDirectory()
	if err != nil {
		return fmt.Errorf("locating Windows enforcement command: %w", err)
	}
	if err := exec.Command(filepath.Join(directory, "taskkill.exe"), "/IM", "PokerStars.exe", "/F").Run(); err != nil {
		running, checkErr := pokerStarsRunning()
		if checkErr != nil {
			return errors.Join(err, checkErr)
		}
		if running {
			return fmt.Errorf("terminating PokerStars: %w", err)
		}
		// The game was already closed, or exited during the command.
	}
	return nil
}
