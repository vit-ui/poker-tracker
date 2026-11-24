package installation

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ownerEventSecurity() (*windows.SecurityAttributes, error) {
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;OW)S:(ML;;NW;;;ME)")
	if err != nil {
		return nil, fmt.Errorf("creating owner event security: %w", err)
	}
	attributes := &windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	return attributes, nil
}

func shutdownEvent(dir string, pid uint32) (*uint16, error) {
	path, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(path))))
	return windows.UTF16PtrFromString(fmt.Sprintf(`Global\PokerTracker-Shutdown-%x-%d`, key, pid))
}

func ListenShutdown(dir string) (<-chan struct{}, func(), error) {
	name, err := shutdownEvent(dir, uint32(os.Getpid()))
	if err != nil {
		return nil, nil, err
	}
	security, err := ownerEventSecurity()
	if err != nil {
		return nil, nil, err
	}
	event, err := windows.CreateEvent(security, 1, 0, name)
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
