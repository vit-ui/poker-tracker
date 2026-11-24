package client

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func killPokerStars() (bool, error) {
	running, err := pokerStarsRunning()
	if err != nil {
		return false, err
	}
	if !running {
		return false, nil
	}
	directory, err := windows.GetSystemDirectory()
	if err != nil {
		return false, fmt.Errorf("locating Windows enforcement command: %w", err)
	}
	if err := exec.Command(filepath.Join(directory, "taskkill.exe"), "/IM", "PokerStars.exe", "/F").Run(); err != nil {
		running, checkErr := pokerStarsRunning()
		if checkErr != nil {
			return false, errors.Join(err, checkErr)
		}
		if running {
			return false, fmt.Errorf("terminating PokerStars: %w", err)
		}
		return false, nil
	}
	return true, nil
}
