package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	UpdateQueued    = "queued"
	UpdateWaiting   = "waiting_for_pokerstars"
	UpdateRunning   = "running"
	UpdateSucceeded = "succeeded"
	UpdateFailed    = "failed"
)

// UpdateRequest pins an accepted Telegram request to one stable release.
type UpdateRequest struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type UpdateJob struct {
	Request UpdateRequest `json:"request"`
	State   string        `json:"state"`
	Version string        `json:"installed_version"`
	Error   string        `json:"error,omitempty"`
}

func (j UpdateJob) Pending() bool {
	return j.State == UpdateQueued || j.State == UpdateWaiting || j.State == UpdateRunning
}

// UpdateReport is an observation from the running installation, not a send receipt.
type UpdateReport struct {
	Version string     `json:"version"`
	Job     *UpdateJob `json:"job,omitempty"`
}

// CollectorUpdates is written only by the server worker. An unacknowledged
// request stays here across disconnects and is replayed with the same identity.
type CollectorUpdates struct {
	Request *UpdateRequest `json:"request,omitempty"`
	Report  UpdateReport   `json:"report"`
	Error   string         `json:"error,omitempty"`
}

type UpdateNotices struct {
	Release   string `json:"release,omitempty"`
	Local     string `json:"local_result,omitempty"`
	Collector string `json:"collector_result,omitempty"`
}

func loadUpdateState(path string, state any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading update state: %w", err)
	}
	if err := json.Unmarshal(data, state); err != nil {
		return fmt.Errorf("decoding update state: %w", err)
	}
	return nil
}

// saveUpdateState flushes the request before acknowledgement or process launch.
func saveUpdateState(path string, state any) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding update state: %w", err)
	}
	file, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening update state: %w", err)
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("flushing update state: %w", err)
	}
	if err := commitUpdateRecovery(path+".tmp", path); err != nil {
		return fmt.Errorf("saving update state: %w", err)
	}
	return nil
}

func LoadUpdateJob(path string) (*UpdateJob, error) {
	mu.RLock()
	defer mu.RUnlock()
	var state *UpdateJob
	err := loadUpdateState(path, &state)
	return state, err
}

func SaveUpdateJob(path string, state UpdateJob) error {
	mu.Lock()
	defer mu.Unlock()
	return saveUpdateState(path, state)
}

func LoadCollectorUpdates(path string) (CollectorUpdates, error) {
	mu.RLock()
	defer mu.RUnlock()
	var state CollectorUpdates
	err := loadUpdateState(path, &state)
	return state, err
}

func SaveCollectorUpdates(path string, state CollectorUpdates) error {
	mu.Lock()
	defer mu.Unlock()
	return saveUpdateState(path, state)
}

func LoadUpdateNotices(path string) (UpdateNotices, error) {
	mu.RLock()
	defer mu.RUnlock()
	var state UpdateNotices
	err := loadUpdateState(path, &state)
	return state, err
}

func SaveUpdateNotices(path string, state UpdateNotices) error {
	mu.Lock()
	defer mu.Unlock()
	return saveUpdateState(path, state)
}

// CheckUpdatesIdle prevents a storage/deployment handover from racing a saved
// update that can stop this owner or its collector as soon as PokerStars closes.
func CheckUpdatesIdle(root string) error {
	job, err := LoadUpdateJob(filepath.Join(root, "requested_update.json"))
	if err != nil {
		return err
	}
	if job != nil && job.Pending() {
		return fmt.Errorf("an installation update is pending; wait for /update to finish first")
	}
	state, err := LoadCollectorUpdates(filepath.Join(root, "collector_updates.json"))
	if err != nil {
		return err
	}
	if state.Report.Job != nil && state.Report.Job.Pending() {
		return fmt.Errorf("a poker-PC update is pending; wait for its confirmation before transferring or restarting settings")
	}
	if state.Request != nil && state.Error == "" {
		job := state.Report.Job
		if job == nil || job.Request != *state.Request || job.Pending() {
			return fmt.Errorf("a poker-PC update is pending; wait for its confirmation before transferring or restarting settings")
		}
	}
	return nil
}
