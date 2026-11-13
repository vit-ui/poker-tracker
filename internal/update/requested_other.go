//go:build !linux && !windows

package update

import "fmt"

func lockRequested(string) (func(), error) { return nil, fmt.Errorf("unsupported update platform") }
func startRequestedHelper(string, []string, string, string, string) error {
	return fmt.Errorf("unsupported update platform")
}
func pokerStarsOpen() (bool, error) { return false, fmt.Errorf("unsupported update platform") }
