//go:build !windows

package client

func killPokerStars() (bool, error) { return false, nil }
