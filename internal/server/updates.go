package server

import (
	"fmt"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/update"
)

// collectorUpdateQueue is owned by the worker, just like collector settings.
// Delivery is replayed on reconnect; only an observed job retires a request.
type collectorUpdateQueue struct {
	path  string
	state config.CollectorUpdates
	sent  bool
}

func (q *collectorUpdateQueue) save(state config.CollectorUpdates) error {
	// Acceptance replies and asynchronous reports can arrive out of order.
	// A pending observation cannot undo the same request's terminal result.
	previous, incoming := q.state.Report.Job, state.Report.Job
	if previous != nil && incoming != nil && previous.Request == incoming.Request &&
		(previous.State == config.UpdateSucceeded || previous.State == config.UpdateFailed) && incoming.Pending() {
		state.Report = q.state.Report
	}
	if err := config.SaveCollectorUpdates(q.path, state); err != nil {
		return err
	}
	q.state = state
	return nil
}

func (q *collectorUpdateQueue) accept(request config.UpdateRequest) error {
	if request.ID == "" {
		return fmt.Errorf("update request has no identity")
	}
	if err := update.ValidateRequestVersion(request.Version); err != nil {
		return err
	}
	if q.state.Request != nil && (q.state.Request.ID == request.ID || (q.state.Error == "" && !q.completed())) {
		if q.state.Request.Version != request.Version {
			return fmt.Errorf("poker-PC update %s is already pending; wait for it to finish first", q.state.Request.Version)
		}
		return nil
	}
	state := q.state
	state.Request, state.Error = &request, ""
	if err := q.save(state); err != nil {
		return err
	}
	q.sent = false
	return nil
}

func (q *collectorUpdateQueue) completed() bool {
	job := q.state.Report.Job
	return q.state.Request != nil && job != nil && job.Request == *q.state.Request && !job.Pending()
}

func (q *collectorUpdateQueue) command() *transport.Command {
	if q.sent || q.state.Request == nil || q.completed() || q.state.Error != "" {
		return nil
	}
	return &transport.Command{Kind: transport.CommandUpdate, RequestID: q.state.Request.ID, Update: q.state.Request}
}

func (q *collectorUpdateQueue) observe(report config.UpdateReport) error {
	state := q.state
	state.Report = report
	return q.save(state)
}

func (q *collectorUpdateQueue) acknowledge(reply transport.UpdateReply) error {
	if q.state.Request == nil || q.state.Request.ID != reply.RequestID {
		return nil
	}
	state := q.state
	state.Report, state.Error = reply.Report, reply.Error
	if reply.Error == "" && reply.Report.Job != nil && reply.Report.Job.Pending() {
		request := reply.Report.Job.Request
		state.Request = &request
	}
	return q.save(state)
}
