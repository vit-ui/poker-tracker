package installation

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// TransferMailbox is the combined owner's installation/PID-scoped event pair.
// Durable request/result files contain the exchange; events only wake owners.
type TransferMailbox struct {
	Events               <-chan struct{}
	request, reply, stop windows.Handle
	stopped              chan struct{}
	canceled             chan struct{}
}

func transferEventNames(dir string, pid uint32) (*uint16, *uint16, error) {
	base, err := shutdownEvent(dir, pid)
	if err != nil {
		return nil, nil, err
	}
	name := windows.UTF16PtrToString(base)
	request, err := windows.UTF16PtrFromString(name + "-transfer-request")
	if err != nil {
		return nil, nil, err
	}
	reply, err := windows.UTF16PtrFromString(name + "-transfer-result")
	return request, reply, err
}

// NewTransferMailbox registers a source owner's request and acknowledgement events.
func NewTransferMailbox(dir string) (*TransferMailbox, error) {
	reqName, resName, err := transferEventNames(dir, uint32(os.Getpid()))
	if err != nil {
		return nil, err
	}
	security, err := ownerEventSecurity()
	if err != nil {
		return nil, err
	}
	request, err := windows.CreateEvent(security, 0, 0, reqName)
	if err != nil {
		if request != 0 {
			windows.CloseHandle(request)
		}
		return nil, err
	}
	reply, err := windows.CreateEvent(security, 1, 0, resName)
	if err != nil {
		windows.CloseHandle(request)
		if reply != 0 {
			windows.CloseHandle(reply)
		}
		return nil, err
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(request)
		windows.CloseHandle(reply)
		return nil, err
	}
	events, stopped := make(chan struct{}), make(chan struct{})
	m := &TransferMailbox{events, request, reply, stop, stopped, make(chan struct{})}
	go func() {
		defer close(stopped)
		defer close(events)
		for {
			result, err := windows.WaitForMultipleObjects([]windows.Handle{stop, request}, false, windows.INFINITE)
			if err != nil || result != windows.WAIT_OBJECT_0+1 {
				return
			}
			select {
			case events <- struct{}{}:
			case <-m.canceled:
				return
			}
		}
	}()
	return m, nil
}

// Acknowledge signals that a durable result is ready for its requesting helper.
func (m *TransferMailbox) Acknowledge() error { return windows.SetEvent(m.reply) }

// Close stops and joins event delivery before releasing kernel resources.
func (m *TransferMailbox) Close() {
	close(m.canceled)
	windows.SetEvent(m.stop)
	<-m.stopped
	windows.CloseHandle(m.stop)
	windows.CloseHandle(m.request)
	windows.CloseHandle(m.reply)
}

// TransferPeer addresses the recorded source owner and detects its process exit.
type TransferPeer struct{ request, reply, process windows.Handle }

// OpenTransferPeer opens only the named source owner's exchange events.
func OpenTransferPeer(dir string, pid int) (*TransferPeer, error) {
	reqName, resName, err := transferEventNames(dir, uint32(pid))
	if err != nil {
		return nil, err
	}
	request, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, reqName)
	if err != nil {
		return nil, fmt.Errorf("source owner has no transfer event: %w", err)
	}
	reply, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE|windows.SYNCHRONIZE, false, resName)
	if err != nil {
		windows.CloseHandle(request)
		return nil, err
	}
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		windows.CloseHandle(request)
		windows.CloseHandle(reply)
		return nil, err
	}
	return &TransferPeer{request, reply, process}, nil
}
func (p *TransferPeer) Reset() error  { return windows.ResetEvent(p.reply) }
func (p *TransferPeer) Notify() error { return windows.SetEvent(p.request) }
func (p *TransferPeer) Wait() error {
	result, err := windows.WaitForMultipleObjects([]windows.Handle{p.reply, p.process}, false, windows.INFINITE)
	if err != nil {
		return err
	}
	if result != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("source owner exited before acknowledgement")
	}
	return nil
}
func (p *TransferPeer) Close() {
	windows.CloseHandle(p.request)
	windows.CloseHandle(p.reply)
	windows.CloseHandle(p.process)
}
