package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsCollectorAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, input string
		invalid     bool
	}{
		{"full path", dir, false},
		{"forward slashes", strings.ReplaceAll(dir, `\`, "/"), false},
		{"missing drive colon", strings.Replace(dir, ":", "", 1), false},
		{"quoted path", `"` + dir + `"`, false},
		{"drive relative", `C:AppData`, true},
		{"drive alone", `C:`, true},
		{"root relative", `\AppData`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveCollectorPath(tc.input)
			if (err != nil) != tc.invalid || !tc.invalid && got != dir {
				t.Fatalf("path %q: %q %v", tc.input, got, err)
			}
		})
	}
}

func TestWindowsCollectorRelativePathUsesActiveDesktop(t *testing.T) {
	home, err := activeDesktopHome()
	if err != nil {
		if _, err := ResolveCollectorPath("AppData"); err == nil {
			t.Fatal("relative path guessed a home without desktop identity")
		}
		t.Log("No accessible active desktop identity: a full path is required")
		return
	}
	// The real desktop home is read only. No directories are created there.
	path, err := ResolveCollectorPath(".")
	if err != nil || path != filepath.Clean(home) {
		t.Fatalf("relative path %q %v; desktop home %q", path, err, home)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("desktop home unavailable: %v", err)
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		systemHome, err := token.GetUserProfileDirectory()
		if err != nil {
			t.Fatal(err)
		}
		if strings.EqualFold(filepath.Clean(home), filepath.Clean(systemHome)) {
			t.Fatal("SYSTEM profile was used instead of active desktop profile")
		}
		t.Log("Verified active desktop profile while running as SYSTEM")
	} else {
		t.Log("Verified active desktop profile from an interactive process")
	}
}
