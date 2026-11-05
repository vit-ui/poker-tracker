package installation

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// AcquireTransferLock refuses concurrent installer helpers for this installation.
// It does not wait or retry; a crashed helper releases its kernel mutex.
func AcquireTransferLock(dir string) (func(), error) {
	base, err := shutdownEvent(dir, 0)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(windows.UTF16PtrToString(base) + "-transfer-helper")
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		if handle != 0 {
			windows.CloseHandle(handle)
		}
		return nil, fmt.Errorf("another helper owns this installation transfer or it cannot be opened: %w", err)
	}
	return func() { windows.CloseHandle(handle) }, nil
}
