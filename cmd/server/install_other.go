//go:build !linux && !windows

package main

import "fmt"

// installService is unsupported outside Linux and Windows — the only server
// targets the project builds. Run --serve under your own supervisor instead.
func installService() error {
	return fmt.Errorf("--install is only supported on Linux and Windows; run --serve under your own service manager")
}
