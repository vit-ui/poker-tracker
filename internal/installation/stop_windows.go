package installation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Stop requests owner shutdown and waits for processes in dir to exit.
// The caller is excluded so setup and uninstall can use the installed binary.
func Stop(dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving installation directory: %w", err)
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return fmt.Errorf("listing installation processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		name := windows.UTF16ToString(entry.ExeFile[:])
		if int(entry.ProcessID) == os.Getpid() || (!strings.EqualFold(name, "tracker.exe") && !strings.EqualFold(name, "tracker-server.exe") && !strings.EqualFold(name, "tracker-client.exe")) {
			continue
		}
		if err := stopProcess(entry.ProcessID, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("enumerating installation processes: %w", err)
	}
	return nil
}

func stopProcess(pid uint32, target string) error {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil // The process exited after the snapshot.
	}
	if err != nil {
		return fmt.Errorf("opening tracker process %d to verify installation: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return fmt.Errorf("locating tracker process %d: %w", pid, err)
	}
	if !strings.EqualFold(filepath.Clean(windows.UTF16ToString(buffer[:size])), target) {
		return nil
	}
	waiter, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening installation process %d for shutdown: %w", pid, err)
	}
	defer windows.CloseHandle(waiter)
	size = uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(waiter, 0, &buffer[0], &size); err != nil {
		return fmt.Errorf("verifying shutdown process %d: %w", pid, err)
	}
	if !strings.EqualFold(filepath.Clean(windows.UTF16ToString(buffer[:size])), target) {
		return nil
	}
	name, err := shutdownEvent(filepath.Dir(target), pid)
	if err != nil {
		return err
	}
	event, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return fmt.Errorf("opening shutdown event for installation process %d: %w", pid, err)
	}
	defer windows.CloseHandle(event)
	if err := windows.SetEvent(event); err != nil {
		return fmt.Errorf("requesting installation process %d shutdown: %w", pid, err)
	}
	if _, err := windows.WaitForSingleObject(waiter, windows.INFINITE); err != nil {
		return fmt.Errorf("waiting for installation process %d cleanup: %w", pid, err)
	}
	return nil
}
