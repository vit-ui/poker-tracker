//go:build !linux && !windows

package update

import "fmt"

func extractPackage(string, string, string) error {
	return fmt.Errorf("unsupported installer platform")
}
