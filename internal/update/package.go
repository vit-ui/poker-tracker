package update

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func packageName(version, goos string) (string, error) {
	if _, err := versionParts(version); err != nil {
		return "", err
	}
	switch goos {
	case "windows":
		return "PokerTracker-Setup-" + strings.TrimPrefix(version, "v") + ".exe", nil
	case "linux":
		return "PokerTracker-Server-linux.tar.gz", nil
	default:
		return "", fmt.Errorf("unsupported installer platform %s", goos)
	}
}

// downloadPackage validates the published package before any embedded code runs.
// GitHub supplies the digest in release metadata; no extra checksum asset exists.
func downloadPackage(client *http.Client, asset releaseAsset, path string) (err error) {
	expected, valid := strings.CutPrefix(asset.Digest, "sha256:")
	decoded, decodeErr := hex.DecodeString(expected)
	if !valid || decodeErr != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("installer %s has no valid GitHub SHA-256 digest", asset.Name)
	}
	response, err := get(client, asset.URL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		file.Close()
		if err != nil {
			os.Remove(path)
		}
	}()
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, asset.Size+1))
	if err != nil {
		return err
	}
	if count != asset.Size {
		return fmt.Errorf("incomplete or oversized installer download")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected) {
		return fmt.Errorf("installer checksum mismatch")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}

func stage(client *http.Client, value release, payload, dir string) (path string, err error) {
	name, err := packageName(value.Tag, runtime.GOOS)
	if err != nil {
		return "", err
	}
	asset, err := findAsset(value, name)
	if err != nil {
		return "", err
	}
	scratch, err := os.MkdirTemp("", "poker-tracker-package-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)
	installer := filepath.Join(scratch, name)
	if err := downloadPackage(client, asset, installer); err != nil {
		return "", err
	}
	extracted := filepath.Join(scratch, "payload")
	if err := extractPackage(installer, payload, extracted); err != nil {
		return "", fmt.Errorf("extracting %s: %w", payload, err)
	}
	input, err := os.Open(extracted)
	if err != nil {
		return "", err
	}
	defer input.Close()
	file, err := os.CreateTemp(dir, ".tracker-update-*")
	if err != nil {
		return "", err
	}
	staged := file.Name()
	defer func() {
		file.Close()
		if err != nil {
			os.Remove(staged)
		}
	}()
	if _, err = io.Copy(file, input); err != nil {
		return "", err
	}
	if err = file.Chmod(0700); err != nil {
		return "", err
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	return staged, nil
}
