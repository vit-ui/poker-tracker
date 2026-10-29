//go:build !windows

package client

// Production collection/enforcement is Windows-only. In particular, running
// the decision/storage pipeline tests under WSL must never discover and execute
// a Windows taskkill.exe from the inherited PATH.
func killPokerStars() error { return nil }
