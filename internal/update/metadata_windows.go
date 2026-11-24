package update

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func (s *serviceState) syncMetadata(version string) error {
	suffix := ""
	if s.installationID != "" {
		suffix = "-" + s.installationID
	}
	path := `Software\Microsoft\Windows\CurrentVersion\Uninstall\{5D7656F7-7CCB-4B00-8694-954DDE2986AE}` + suffix + "_is1"
	if err := syncInstalledMetadata(registry.LOCAL_MACHINE, path, s.target, version); err != nil {
		return fmt.Errorf("%s is installed and started, but Windows app metadata could not be updated: %w", version, err)
	}
	return nil
}

func syncInstalledMetadata(root registry.Key, path, target, version string) error {
	if err := ValidateRequestVersion(version); err != nil {
		return err
	}
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE|registry.SET_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening installer metadata: %w", err)
	}
	defer key.Close()
	location, _, err := key.GetStringValue("InstallLocation")
	if err != nil {
		return fmt.Errorf("reading install location: %w", err)
	}
	if !strings.EqualFold(filepath.Clean(location), filepath.Clean(filepath.Dir(target))) {
		return fmt.Errorf("installer location %q does not match executable %q", location, target)
	}
	oldVersion, _, err := key.GetStringValue("DisplayVersion")
	if err != nil {
		return fmt.Errorf("reading display version: %w", err)
	}
	name, _, err := key.GetStringValue("DisplayName")
	if err != nil {
		return fmt.Errorf("reading display name: %w", err)
	}
	version = strings.TrimPrefix(version, "v")
	marker := " version " + oldVersion + " "
	newMarker := " version " + version + " "
	if !strings.Contains(name, marker) && !strings.Contains(name, newMarker) {
		return fmt.Errorf("installer display name has no version marker")
	}
	newName := strings.Replace(name, marker, newMarker, 1)
	if err := key.SetStringValue("DisplayName", newName); err != nil {
		return fmt.Errorf("writing display name: %w", err)
	}
	if err := key.SetStringValue("DisplayVersion", version); err != nil {
		return errors.Join(fmt.Errorf("writing display version: %w", err), key.SetStringValue("DisplayName", name))
	}
	return nil
}
