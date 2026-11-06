// Package deployment coordinates the combined-to-split installation transaction.
package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"time"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/installation"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// SourceOwner prepares transfers on the existing combined process's worker.
// Only this goroutine writes source_transfer.json; the helper writes requests.
type SourceOwner struct {
	Path          string
	Controls      chan<- bot.OwnerControl
	Operations    chan<- storage.Operation
	Stop          <-chan struct{}
	WorkerStopped <-chan struct{}
	Shutdown      chan struct{}
	Errors        chan error
	mailbox       *installation.TransferMailbox
	state         *config.SourceTransferState
	release       chan struct{}
	resumed       chan error
	done          chan struct{}
}

// OpenSourceOwner registers events before exposing the owner's durable PID.
func OpenSourceOwner(path string) (*SourceOwner, error) {
	owner := &SourceOwner{Path: path, Shutdown: make(chan struct{}), Errors: make(chan error, 1), done: make(chan struct{})}
	if runtime.GOOS != "windows" {
		close(owner.done)
		return owner, nil
	}
	state, err := config.LoadSourceTransfer(owner.statePath())
	if err != nil {
		return nil, err
	}
	if state == nil {
		state = &config.SourceTransferState{Phase: "idle"}
	}
	owner.state = state
	owner.mailbox, err = installation.NewTransferMailbox(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	state.OwnerPID = os.Getpid()
	state.SourceExited = false
	if err := config.SaveSourceTransfer(owner.statePath(), state); err != nil {
		owner.mailbox.Close()
		return nil, err
	}
	return owner, nil
}
func (o *SourceOwner) statePath() string {
	return filepath.Join(filepath.Dir(o.Path), "source_transfer.json")
}

// StartPaused prevents collection from resuming an interrupted frozen transfer.
func (o *SourceOwner) StartPaused() bool {
	return o.state != nil && o.state.Phase != "idle" && o.state.Phase != "aborted" && o.state.Phase != "complete"
}

// Run serializes source requests while the main owner continues failure handling.
func (o *SourceOwner) Run() {
	if o.mailbox == nil {
		return
	}
	defer close(o.done)
	if o.StartPaused() {
		wasStopping := o.state.Phase == "stopping"
		if err := o.prepare(); err != nil {
			o.Errors <- err
			return
		}
		if wasStopping {
			o.state.Phase = "stopping"
			if err := config.SaveSourceTransfer(o.statePath(), o.state); err != nil {
				o.Errors <- err
				return
			}
		}
	}
	for {
		select {
		case <-o.Stop:
			return
		case _, ok := <-o.mailbox.Events:
			if !ok {
				return
			}
			req, err := config.LoadSourceTransferRequest(filepath.Join(filepath.Dir(o.Path), "source_transfer_request.json"))
			if err != nil {
				o.Errors <- err
				return
			}
			if req.ID == "" || req.RequestID == "" {
				err = fmt.Errorf("transfer request has no identity")
			} else if o.StartPaused() && o.state.ID != "" && (req.ID != o.state.ID || req.Connection != o.state.Connection) {
				err = fmt.Errorf("source is frozen for another transfer")
			} else {
				if o.state.Phase == "idle" || o.state.Phase == "aborted" || o.state.Phase == "complete" {
					o.state.Payload = nil
					o.state.OriginalConfig = nil
				}
				o.state.ID, o.state.Connection = req.ID, req.Connection
				switch req.Kind {
				case "reserve":
					if o.release != nil || len(o.state.Payload) > 0 {
						break
					}
					cfg, loadErr := config.LoadConfig(o.Path)
					if loadErr != nil {
						err = loadErr
					} else {
						o.state.OriginalConfig = cfg
						o.state.Phase = "reserving"
					}
				case "prepare":
					err = o.prepare()
				case "abort":
					err = o.abort()
				case "shutdown":
					if o.state.Phase != "prepared" {
						err = fmt.Errorf("source has not prepared its snapshot")
					} else {
						o.state.Phase = "stopping"
					}
				default:
					err = fmt.Errorf("unknown source transfer request")
				}
			}
			o.state.RequestID = req.RequestID
			o.state.Error = ""
			if err != nil {
				o.state.Error = err.Error()
			}
			if err := config.SaveSourceTransfer(o.statePath(), o.state); err != nil {
				o.Errors <- err
				return
			}
			if err := o.mailbox.Acknowledge(); err != nil {
				o.Errors <- err
				return
			}
			if req.Kind == "shutdown" && err == nil {
				close(o.Shutdown)
				return
			}
		}
	}
}
func (o *SourceOwner) pause(paused bool) error {
	reply := make(chan error, 1)
	select {
	case o.Controls <- bot.OwnerControl{Pause: paused, Reply: reply}:
	case <-o.Stop:
		return fmt.Errorf("owner is shutting down")
	}
	select {
	case err := <-reply:
		return err
	case <-o.Stop:
		return fmt.Errorf("owner is shutting down")
	}
}
func (o *SourceOwner) prepare() error {
	if o.release != nil {
		return nil
	}
	original, err := config.LoadConfig(o.Path)
	if err != nil {
		return err
	}
	if o.state.OriginalConfig == nil {
		o.state.OriginalConfig = original
	}
	o.state.Phase = "preparing"
	if err := config.SaveSourceTransfer(o.statePath(), o.state); err != nil {
		return err
	}
	if err := o.pause(true); err != nil {
		o.state.Phase = "aborted"
		return err
	}
	release, resumed, reply := make(chan struct{}), make(chan error, 1), make(chan storage.OperationResult, 1)
	var payload []byte
	op := storage.Operation{PrepareMigration: true, RequireConfirmedProtection: true, Release: release, Resumed: resumed, Reply: reply, Apply: func(store storage.Store) storage.OperationResult {
		if err := config.CheckClosureTransferReady(filepath.Dir(o.Path)); err != nil {
			return storage.OperationResult{Err: err}
		}
		snapshot, err := store.ExportSnapshot()
		if err != nil {
			return storage.OperationResult{Err: err}
		}
		cfg, err := config.LoadConfig(o.Path)
		if err != nil {
			return storage.OperationResult{Err: err}
		}
		questions, err := config.LoadPendingTournaments(filepath.Join(filepath.Dir(o.Path), "pending_tournaments.json"))
		if err != nil {
			return storage.OperationResult{Err: err}
		}
		if len(o.state.Payload) > 0 {
			var frozen storage.DeploymentPayload
			if err := json.Unmarshal(o.state.Payload, &frozen); err != nil {
				return storage.OperationResult{Err: err}
			}
			precision := storagePrecision(store)
			if err := storage.VerifySnapshot(frozen.Snapshot, snapshot, precision); err != nil {
				return storage.OperationResult{Err: fmt.Errorf("source changed while transfer was frozen: %w", err)}
			}
			if !reflect.DeepEqual(frozen.Settings, config.GetOperational(cfg)) || !reflect.DeepEqual(frozen.Pending, questions) {
				return storage.OperationResult{Err: fmt.Errorf("source operational state changed while frozen")}
			}
			payload = o.state.Payload
		} else {
			o.state.OriginalConfig = cfg
			payload, err = json.Marshal(storage.DeploymentPayload{Snapshot: snapshot, Settings: config.GetOperational(cfg), Pending: questions, ClientClock: time.Now()})
		}
		return storage.OperationResult{Err: err}
	}}
	select {
	case o.Operations <- op:
	case <-o.Stop:
		return fmt.Errorf("owner is shutting down")
	}
	select {
	case result := <-reply:
		if result.Err != nil {
			o.state.Phase = "aborted"
			return errors.Join(result.Err, o.pause(false))
		}
	case <-o.WorkerStopped:
		return fmt.Errorf("source worker stopped during transfer preparation")
	}
	o.release, o.resumed = release, resumed
	o.state.Payload = payload
	o.state.Phase = "prepared"
	return config.SaveSourceTransfer(o.statePath(), o.state)
}
func (o *SourceOwner) abort() error {
	if o.release != nil {
		release, resumed := o.release, o.resumed
		o.release, o.resumed = nil, nil
		close(release)
		select {
		case err := <-resumed:
			if err != nil {
				return err
			}
		case <-o.Stop:
			return fmt.Errorf("owner is shutting down")
		}
	}
	if err := o.pause(false); err != nil {
		return err
	}
	o.state.Phase = "aborted"
	o.state.Payload = nil
	o.state.OriginalConfig = nil
	return nil
}

// Close joins source request handling after the main owner cancels Stop.
func (o *SourceOwner) Close() {
	if o.mailbox != nil {
		<-o.done
		o.mailbox.Close()
	}
}
