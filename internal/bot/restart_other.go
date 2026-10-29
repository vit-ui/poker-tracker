//go:build !windows && !linux

package bot

import "fmt"

func selfRestart(configPath, serviceName string) error {
	return fmt.Errorf("automatic restart is supported only on Windows and Linux")
}
