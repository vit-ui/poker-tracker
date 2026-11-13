package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

func launchRequested(target, role, installationID, requestID string) error {
	dir, err := os.MkdirTemp("", "poker-tracker-request-")
	if err != nil {
		return err
	}
	helper := filepath.Join(dir, "update-helper")
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	if err := copyFile(target, helper, 0700); err != nil {
		os.RemoveAll(dir)
		return err
	}
	args := []string{"--update-target", target, "--update-request", requestID}
	if err := startRequestedHelper(helper, args, role, installationID, requestID); err != nil {
		os.RemoveAll(dir)
		return err
	}
	return nil
}

func runRequested(target, role, version, installationID, requestID string, out io.Writer) error {
	release, err := lockRequested(filepath.Join(filepath.Dir(target), ".tracker-request.lock"))
	if err != nil {
		return err
	}
	defer release()
	path := filepath.Join(filepath.Dir(target), "requested_update.json")
	job, err := config.LoadUpdateJob(path)
	if err != nil {
		return err
	}
	if job == nil || job.Request.ID != requestID || !job.Pending() {
		return nil
	}
	updateErr := run(target, role, version, installationID, 0, out, job)
	job.State, job.Error = config.UpdateSucceeded, ""
	if updateErr != nil {
		job.State, job.Error = config.UpdateFailed, updateErr.Error()
	}
	return errors.Join(updateErr, config.SaveUpdateJob(path, *job))
}

// waitForPokerStars leaves enforcement and collection running. A saved request
// resumes after reboot; operational failures are reported instead of retried.
func waitForPokerStars(path, role string, job *config.UpdateJob) error {
	if role == "server" {
		return nil
	}
	var ticker *time.Ticker
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	for {
		open, err := pokerStarsOpen()
		if err != nil {
			return fmt.Errorf("checking PokerStars before update: %w", err)
		}
		if !open {
			return nil
		}
		if job != nil && job.State != config.UpdateWaiting {
			job.State = config.UpdateWaiting
			if err := config.SaveUpdateJob(path, *job); err != nil {
				return err
			}
		}
		if ticker == nil {
			ticker = time.NewTicker(30 * time.Second)
		}
		<-ticker.C
	}
}
