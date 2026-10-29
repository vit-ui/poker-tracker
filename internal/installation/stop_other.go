//go:build !windows

package installation

import "fmt"

// Stop is used by the Windows installer only.
func Stop(dir string) error {
	return fmt.Errorf("stopping installation %s: supported only on Windows", dir)
}
