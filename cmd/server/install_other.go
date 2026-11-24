//go:build !linux && !windows

package main

import "fmt"

func installService() error {
	return fmt.Errorf("--install is only supported on Linux and Windows; run --serve under your own service manager")
}
