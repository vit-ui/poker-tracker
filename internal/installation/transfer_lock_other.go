//go:build !windows

package installation

import "fmt"

func AcquireTransferLock(string) (func(), error) {
	return nil, fmt.Errorf("installer transfer helpers require Windows")
}
