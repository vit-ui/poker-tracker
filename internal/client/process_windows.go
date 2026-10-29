package client

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pokerStarsRunning only inspects process names. It never terminates a process
// or writes an enforcement lock while checking the migration precondition.
func pokerStarsRunning() (bool, error) { return processRunning("PokerStars.exe") }

func processRunning(name string) (bool, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, fmt.Errorf("listing poker-PC processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), name) {
			return true, nil
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return false, fmt.Errorf("checking poker-PC processes: %w", err)
	}
	return false, nil
}
