package installation

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func shutdownEvent(dir string, pid uint32) (*uint16, error) {
	path, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(path))))
	return windows.UTF16PtrFromString(fmt.Sprintf(`Global\PokerTracker-Shutdown-%x-%d`, key, pid))
}

// ListenShutdown registers this process's installation-scoped shutdown event.
// The owner must finish cleanup before releasing the registration.
func ListenShutdown(dir string) (<-chan struct{}, func(), error) {
	name, err := shutdownEvent(dir, uint32(os.Getpid()))
	if err != nil {
		return nil, nil, err
	}
	event, err := windows.CreateEvent(nil, 1, 0, name)
	if err != nil {
		return nil, nil, fmt.Errorf("creating installation shutdown event: %w", err)
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		windows.WaitForSingleObject(event, windows.INFINITE)
		close(done)
	}()
	return done, func() { windows.SetEvent(event); <-stopped; windows.CloseHandle(event) }, nil
}
