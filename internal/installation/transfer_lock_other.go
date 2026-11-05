//go:build !windows

package installation

import "fmt"

// AcquireTransferLock is used by the Windows installer transfer helper only.
func AcquireTransferLock(string) (func(), error) {
	return nil, fmt.Errorf("installer transfer helpers require Windows")
}
