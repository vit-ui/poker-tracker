//go:build !windows

package config

import "os"

func restrictTelegramToken(path string) error {
	return os.Chmod(path, 0600)
}
