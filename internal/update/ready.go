package update

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/vit-ui/self-tracker/internal/config"
)

func restartAndConfirm(service *serviceState, version string) error {
	if !service.wasRunning() {
		return service.restore()
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	dir := filepath.Dir(service.target)
	if err := watcher.Add(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, "update_startup.json")
	expected := config.UpdateStartup{Token: rand.Text(), Version: version}
	if err := config.SaveUpdateStartup(path, expected); err != nil {
		return err
	}
	defer os.Remove(path)
	if err := service.restore(); err != nil {
		return err
	}
	// This checks process/service exit, not a startup timeout or a folder scan.
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	readState := true
	for {
		alive, err := service.alive()
		if err != nil {
			return err
		}
		if !alive {
			return fmt.Errorf("new process exited before confirming startup")
		}
		if readState {
			state, err := config.LoadUpdateStartup(path)
			if err != nil {
				return err
			}
			if state != nil && state.Token == expected.Token && state.Version == version && state.PID > 0 {
				return nil
			}
			readState = false
		}
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("startup watcher closed")
			}
			readState = event.Name == path
		case err, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("startup watcher closed")
			}
			return fmt.Errorf("watching startup confirmation: %w", err)
		case <-ticker.C:
		}
	}
}
