package server

import (
	"errors"
	"fmt"

	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

// collectionOperation exists only on the worker. The pause acknowledgement is
// ordered after drained uploads; a successful copy then holds the worker until
// restart or rollback. Disconnect is failure, never evidence of completion.
type collectionOperation struct {
	operation    storage.Operation
	command      transport.Command
	sent         bool
	disconnected <-chan struct{}
	result       storage.OperationResult
	replied      bool
}

func (p *collectionOperation) resume(id string, done <-chan struct{}) {
	p.command = transport.Command{Kind: transport.CommandResumeCollection, RequestID: id}
	p.sent = false
	p.disconnected = done
}

func (p *collectionOperation) finish(err error) {
	if p.replied {
		p.operation.Resumed <- err
		return
	}
	p.result.Err = errors.Join(p.result.Err, err)
	p.operation.Reply <- p.result
}

func collectorFailure() error {
	return fmt.Errorf("collector disconnected; migration requires the poker PC to be connected")
}
