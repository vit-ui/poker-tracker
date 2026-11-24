package installation

import (
	"os"
	"path/filepath"

	"github.com/vit-ui/self-tracker/internal/config"
)

func ReportReady(dir, version string) error {
	path := filepath.Join(dir, "update_startup.json")
	state, err := config.LoadUpdateStartup(path)
	if err != nil {
		return err
	}
	if state == nil || state.Version != version {
		return nil
	}
	state.PID = os.Getpid()
	return config.SaveUpdateStartup(path, *state)
}

func ReadyAfter(dir, version string, stop <-chan struct{}, components ...<-chan struct{}) <-chan error {
	errors := make(chan error, 1)
	go func() {
		for _, ready := range components {
			select {
			case <-ready:
			case <-stop:
				return
			}
		}
		select {
		case <-stop:
			return
		default:
		}
		if err := ReportReady(dir, version); err != nil {
			errors <- err
		}
	}()
	return errors
}
