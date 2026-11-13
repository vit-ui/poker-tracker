package update

import (
	"net/http"
)

// Check returns only a validated stable release version. It installs nothing.
func Check() (string, error) {
	value, err := latest(http.DefaultClient, latestURL)
	if err != nil {
		return "", err
	}
	if _, err := versionParts(value.Tag); err != nil {
		return "", err
	}
	return value.Tag, nil
}

// Newer shares terminal/Telegram version ordering, including patch versions.
func Newer(latest, current string) (bool, error) { return newer(latest, current) }

func ValidateRequestVersion(version string) error {
	_, err := versionParts(version)
	return err
}
