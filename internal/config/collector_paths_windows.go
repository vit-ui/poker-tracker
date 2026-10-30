package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func collectorAbsolutePath(input string) (string, error) {
	path := strings.ReplaceAll(input, "/", `\`)
	letter := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
	if len(path) >= 2 && letter(path[0]) && path[1] == '\\' {
		path = path[:1] + ":" + path[1:]
	}
	if filepath.IsAbs(path) {
		return path, nil
	}
	if filepath.VolumeName(path) != "" || strings.HasPrefix(path, `\`) {
		return "", fmt.Errorf("send a full path such as C:\\Users\\you\\AppData\\Local\\PokerStars\\HandHistory")
	}
	home, err := activeDesktopHome()
	if err != nil {
		return "", fmt.Errorf("cannot identify the active Windows desktop user; send a full path: %w", err)
	}
	return filepath.Join(home, path), nil
}

func activeDesktopHome() (string, error) {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		return "", err
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
	var active *windows.WTS_SESSION_INFO
	for _, session := range unsafe.Slice(sessions, count) {
		if session.State != windows.WTSActive || session.SessionID == 0 {
			continue
		}
		if active != nil {
			return "", fmt.Errorf("more than one active desktop session")
		}
		copy := session
		active = &copy
	}
	if active == nil {
		return "", fmt.Errorf("no active desktop session")
	}
	// An interactively launched collector can use its own token only when it
	// belongs to that active desktop. A SYSTEM task must use the desktop token.
	var ownSession uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &ownSession); err == nil && ownSession == active.SessionID {
		token := windows.GetCurrentProcessToken()
		user, err := token.GetTokenUser()
		if err == nil && !user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
			return token.GetUserProfileDirectory()
		}
	}
	var token windows.Token
	if err := windows.WTSQueryUserToken(active.SessionID, &token); err != nil {
		return "", err
	}
	defer token.Close()
	return token.GetUserProfileDirectory()
}
