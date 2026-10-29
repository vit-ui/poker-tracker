//go:build !windows

package client

import "fmt"

func pokerStarsRunning() (bool, error) {
	return false, fmt.Errorf("PokerStars closure must be checked by a Windows collector")
}
