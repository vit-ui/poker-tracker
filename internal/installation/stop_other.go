//go:build !windows

package installation

import "fmt"

func Stop(dir string) error {
	return fmt.Errorf("stopping installation %s: supported only on Windows", dir)
}
