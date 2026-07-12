//go:build !windows

package main

// hideConsole is a no-op outside Windows — there's no console-allocation
// quirk to work around on other platforms.
func hideConsole() {}
