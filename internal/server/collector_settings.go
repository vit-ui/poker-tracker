package server

import (
	"fmt"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
)

// collectorSettingsQueue is owned by Run alongside uploads and storage writes.
// Edits coalesce by field; an older acknowledgement cannot retire newer edits.
type collectorSettingsQueue struct {
	path     string
	state    config.CollectorSettingsState
	inflight *config.CollectorChange
}

func (q *collectorSettingsQueue) save(state config.CollectorSettingsState) error {
	if err := config.SaveCollectorSettings(q.path, state); err != nil {
		return err
	}
	q.state = state
	return nil
}

func (q *collectorSettingsQueue) accept(change config.CollectorChange) error {
	if change.ID == "" {
		return fmt.Errorf("collector edit has no identity")
	}
	if err := change.Settings.Validate(); err != nil {
		return err
	}
	var cfg config.Config
	if q.state.Pending != nil {
		q.state.Pending.Settings.Apply(&cfg)
	}
	change.Settings.Apply(&cfg)
	change.Settings = config.CollectionSettings(&cfg)
	state := q.state
	state.Pending, state.Error = &change, ""
	return q.save(state)
}

func (q *collectorSettingsQueue) command() *transport.Command {
	if q.inflight != nil || q.state.Pending == nil || q.state.Error != "" {
		return nil
	}
	change := q.state.Pending
	return &transport.Command{Kind: transport.CommandCollectorConfig, RequestID: change.ID, Settings: &change.Settings}
}

func (q *collectorSettingsQueue) observe(settings config.CollectorSettings, watching bool, collectionError string) error {
	state := q.state
	state.Confirmed = &settings
	state.Watching, state.CollectionError = watching, collectionError
	return q.save(state)
}

func (q *collectorSettingsQueue) acknowledge(reply transport.CollectorConfigReply) (string, error) {
	if q.inflight == nil || q.inflight.ID != reply.RequestID {
		return "", nil
	}
	state := q.state
	state.Confirmed = &reply.Settings
	state.Watching = reply.Watching
	state.CollectionError = reply.CollectionError
	current := state.Pending != nil && state.Pending.ID == reply.RequestID
	if current {
		if reply.Error == "" {
			state.Pending = nil
		}
		state.Error = reply.Error
	}
	if err := q.save(state); err != nil {
		return "", err
	}
	q.inflight = nil
	if !current {
		return "", nil
	}
	if reply.Error != "" {
		return "⚠️ The poker PC could not apply the queued settings: " + reply.Error + "\nThe edit remains pending. Check /paths or /screenname and send the corrected value.", nil
	}
	if !reply.Watching {
		return "✅ The poker PC saved the queued settings. Set both folders and a screen name before collection can start.", nil
	}
	return "✅ The poker PC confirmed the queued settings and activated its file watcher.", nil
}
