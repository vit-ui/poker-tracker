package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// SourceTransferState is written only by the running combined owner. Its frozen
// payload survives an installer interruption without reopening source history.
type SourceTransferState struct {
	ID             string          `json:"id"`
	Phase          string          `json:"phase"`
	SourceExited   bool            `json:"source_exited"`
	OwnerPID       int             `json:"owner_pid"`
	RequestID      string          `json:"request_id"`
	Connection     string          `json:"connection"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	OriginalConfig *Config         `json:"original_config,omitempty"`
	Error          string          `json:"error,omitempty"`
}

// SourceTransferRequest is written only by the installer helper. Separate files
// keep the request/result exchange from relying on a process-local mutex.
type SourceTransferRequest struct{ ID, RequestID, Kind, Connection string }

// LoadSourceTransfer reads owner progress without changing it.
func LoadSourceTransfer(path string) (*SourceTransferState, error) {
	mu.Lock()
	defer mu.Unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state *SourceTransferState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing source transfer: %w", err)
	}
	if state == nil {
		return nil, fmt.Errorf("source transfer state is empty")
	}
	switch state.Phase {
	case "idle":
	case "reserving", "preparing", "prepared", "stopping", "aborted", "complete":
		if state.ID == "" || state.Connection == "" {
			return nil, fmt.Errorf("source transfer identity is missing")
		}
	default:
		return nil, fmt.Errorf("unknown source transfer phase %q", state.Phase)
	}
	if (state.Phase == "prepared" || state.Phase == "stopping" || state.Phase == "complete") && (len(state.Payload) == 0 || state.OriginalConfig == nil) {
		return nil, fmt.Errorf("frozen source transfer is missing recovery material")
	}
	return state, nil
}

// SaveSourceTransfer persists a source owner result.
func SaveSourceTransfer(path string, state *SourceTransferState) error {
	mu.Lock()
	defer mu.Unlock()
	return saveTransferJSON(path, state)
}

// LoadSourceTransferRequest reads a durable helper request.
func LoadSourceTransferRequest(path string) (SourceTransferRequest, error) {
	mu.Lock()
	defer mu.Unlock()
	var r SourceTransferRequest
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &r)
	}
	return r, err
}

// SaveSourceTransferRequest persists a helper request before signalling its event.
func SaveSourceTransferRequest(path string, r SourceTransferRequest) error {
	mu.Lock()
	defer mu.Unlock()
	return saveTransferJSON(path, r)
}

func saveTransferJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("persisting transfer state: %w", err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return fmt.Errorf("replacing transfer state: %w", err)
	}
	return nil
}
