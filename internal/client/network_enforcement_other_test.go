//go:build !windows

package client

import "testing"

func TestNetworkCollectorKeepsEnforcerAliveDuringOutage(t *testing.T) {
	verifyNetworkCollectorRecovery(t, true)
	verifyNetworkCollectorEnforcesInvalidPairing(t)
}
