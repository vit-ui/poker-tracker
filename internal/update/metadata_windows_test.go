package update

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func TestInstalledMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, location, display, version string
		missing, wantErr                 bool
	}{
		{"upgrade", `C:\tracker\`, "Poker Tracker version 4.11 (64-bit)", "v4.12", false, false},
		{"interrupted metadata write", `C:\tracker`, "Poker Tracker version 4.12 (64-bit)", "v4.12", false, false},
		{"already current", `c:\TRACKER`, "Poker Tracker version 4.12 (64-bit)", "v4.12", false, false},
		{"installation suffix", `C:\tracker`, "Poker Tracker-test version 4.11 (64-bit)", "v4.12", false, false},
		{"other install", `C:\other`, "Poker Tracker version 4.11 (64-bit)", "v4.12", false, true},
		{"missing marker", `C:\tracker`, "Other application", "v4.12", false, true},
		{"invalid version", `C:\tracker`, "Poker Tracker version 4.11 (64-bit)", "invalid", false, true},
		{"portable", `C:\tracker`, "", "v4.12", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := fmt.Sprintf(`Software\PokerTrackerMetadataTest-%d`, time.Now().UnixNano())
			if tc.missing {
				if err := syncInstalledMetadata(registry.CURRENT_USER, path, `C:\tracker\tracker.exe`, tc.version); err != nil {
					t.Fatal(err)
				}
				if key, err := registry.OpenKey(registry.CURRENT_USER, path, registry.READ); err == nil {
					key.Close()
					t.Fatal("created portable entry")
				}
				return
			}
			key, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS|registry.WOW64_64KEY)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { key.Close(); registry.DeleteKey(registry.CURRENT_USER, path) }()
			oldVersion := "4.11"
			if tc.name == "already current" {
				oldVersion = "4.12"
			}
			values := map[string]string{"InstallLocation": tc.location, "DisplayName": tc.display, "DisplayVersion": oldVersion, "UninstallString": `C:\tracker\unins000.exe`, "Other": "untouched"}
			for name, value := range values {
				if err := key.SetStringValue(name, value); err != nil {
					t.Fatal(err)
				}
			}
			err = syncInstalledMetadata(registry.CURRENT_USER, path, filepath.Join(`C:\tracker`, "tracker.exe"), tc.version)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			for name, want := range values {
				if !tc.wantErr {
					if name == "DisplayVersion" {
						want = "4.12"
					}
					if name == "DisplayName" {
						want = "Poker Tracker version 4.12 (64-bit)"
						if tc.name == "installation suffix" {
							want = "Poker Tracker-test version 4.12 (64-bit)"
						}
					}
				}
				got, _, err := key.GetStringValue(name)
				if err != nil || got != want {
					t.Fatalf("%s=%q want %q err=%v", name, got, want, err)
				}
			}
		})
	}
}
