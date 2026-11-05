package deployment

import (
	"fmt"
	"path/filepath"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
)

// DestinationStandby keeps the source as the only command reader until cutover.
// Rolled-back destinations remain in standby while their source is restored.
func DestinationStandby(state *config.DeploymentTransferState) bool {
	return state != nil && state.Phase != transport.TransferPhaseActivated
}

// DestinationOwner drains accepted bot work before reserving the worker, then
// hands polling over only after the worker's durable activation receipt.
type DestinationOwner struct {
	Path     string
	Requests <-chan transport.TransferCall
	Worker   chan<- transport.TransferCall
	Controls chan<- bot.OwnerControl
	Stop     <-chan struct{}
	Standby  bool
}

func (o *DestinationOwner) control(pause bool) error {
	reply := make(chan error, 1)
	select {
	case o.Controls <- bot.OwnerControl{Pause: pause, StopPolling: pause, Reply: reply}:
	case <-o.Stop:
		return fmt.Errorf("destination owner is stopping")
	}
	select {
	case err := <-reply:
		return err
	case <-o.Stop:
		return fmt.Errorf("destination owner is stopping")
	}
}

// Run serializes the polling/worker ownership boundary without a second writer.
func (o *DestinationOwner) Run() {
	for {
		select {
		case <-o.Stop:
			return
		case call := <-o.Requests:
			if (call.Request.Kind == transport.TransferReserve || call.Request.Kind == transport.TransferBegin || call.Request.Kind == transport.TransferRollback) && !o.Standby {
				if err := o.control(true); err != nil {
					call.Reply <- transport.TransferReply{ID: call.Request.ID, Error: err.Error()}
					continue
				}
				o.Standby = true
			}
			workerCall := transport.TransferCall{Request: call.Request, Reply: make(chan transport.TransferReply, 1)}
			select {
			case o.Worker <- workerCall:
			case <-o.Stop:
				return
			}
			var result transport.TransferReply
			select {
			case result = <-workerCall.Reply:
			case <-o.Stop:
				return
			}
			// A refusal before reservation returns this destination to its prior owner;
			// a completed rollback keeps it in standby for the resumed combined source.
			if result.Phase == transport.TransferPhaseActivated || (result.Phase == "" && o.Standby) {
				state, err := config.LoadDeploymentTransfer(filepath.Join(filepath.Dir(o.Path), "deployment_transfer.json"))
				if err == nil && !DestinationStandby(state) {
					if err := o.control(false); err != nil {
						result.Error = err.Error()
					} else {
						o.Standby = false
					}
				}
			}
			call.Reply <- result
		}
	}
}
