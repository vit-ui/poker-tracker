//go:build !windows

package installation

import "fmt"

type TransferMailbox struct{ Events <-chan struct{} }

func NewTransferMailbox(string) (*TransferMailbox, error) {
	return nil, fmt.Errorf("installation transfer events require Windows")
}
func (*TransferMailbox) Acknowledge() error { return nil }
func (*TransferMailbox) Close()             {}

type TransferPeer struct{}

func OpenTransferPeer(string, int) (*TransferPeer, error) {
	return nil, fmt.Errorf("installation transfer events require Windows")
}
func (*TransferPeer) Reset() error  { return fmt.Errorf("transfer events require Windows") }
func (*TransferPeer) Notify() error { return fmt.Errorf("transfer events require Windows") }
func (*TransferPeer) Wait() error   { return fmt.Errorf("transfer events require Windows") }
func (*TransferPeer) Close()        {}
