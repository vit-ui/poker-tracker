//go:build !windows

package installation

import (
	"os"
	"os/signal"
	"syscall"
)

// ListenShutdown routes termination signals to the process owner.
func ListenShutdown(_ string) (<-chan struct{}, func(), error) {
	signals := make(chan os.Signal, 1)
	done, stop, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer close(stopped)
		select {
		case <-signals:
			close(done)
		case <-stop:
		}
	}()
	return done, func() { signal.Stop(signals); close(stop); <-stopped }, nil
}
