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

// Stop terminates tracker processes whose executable belongs to dir. The
// caller is excluded so setup and the uninstaller can use the installed binary.
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
		if int(entry.ProcessID) == os.Getpid() || (!strings.EqualFold(name, "tracker.exe") && !strings.EqualFold(name, "tracker-server.exe")) {
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
	terminator, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening installation process %d for termination: %w", pid, err)
	}
	defer windows.CloseHandle(terminator)
	// Recheck the second handle in case the original process exited and its
	// PID was reused between opening the two handles.
	size = uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(terminator, 0, &buffer[0], &size); err != nil {
		return fmt.Errorf("verifying installation process %d before termination: %w", pid, err)
	}
	if !strings.EqualFold(filepath.Clean(windows.UTF16ToString(buffer[:size])), target) {
		return nil
	}
	if err := windows.TerminateProcess(terminator, 0); err != nil {
		return fmt.Errorf("stopping tracker process %d in %s: %w", pid, filepath.Dir(target), err)
	}
	if _, err := windows.WaitForSingleObject(terminator, windows.INFINITE); err != nil {
		return fmt.Errorf("waiting for tracker process %d to stop: %w", pid, err)
	}
	return nil
}
