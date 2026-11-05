package deployment

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/installation"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func sourcePath(path string) string { return filepath.Join(filepath.Dir(path), "source_transfer.json") }
func exchangeSource(path string, state *config.SourceTransferState, kind string) (*config.SourceTransferState, error) {
	peer, err := installation.OpenTransferPeer(filepath.Dir(path), state.OwnerPID)
	if err != nil {
		return nil, err
	}
	defer peer.Close()
	request := config.SourceTransferRequest{ID: state.ID, RequestID: rand.Text(), Kind: kind, Connection: state.Connection}
	if err := peer.Reset(); err != nil {
		return nil, err
	}
	if err := config.SaveSourceTransferRequest(filepath.Join(filepath.Dir(path), "source_transfer_request.json"), request); err != nil {
		return nil, err
	}
	if err := peer.Notify(); err != nil {
		return nil, err
	}
	for {
		if err := peer.Reset(); err != nil {
			return nil, err
		}
		result, err := config.LoadSourceTransfer(sourcePath(path))
		if err != nil {
			return nil, err
		}
		if result != nil && result.RequestID == request.RequestID {
			if result.ID != request.ID {
				return nil, fmt.Errorf("source acknowledged another transfer")
			}
			if result.Error != "" {
				return result, errors.New(result.Error)
			}
			return result, nil
		}
		if err := peer.Wait(); err != nil {
			return nil, err
		}
	}
}
func remote(state *config.SourceTransferState, kind string) (*transport.TransferConnection, transport.TransferReply, error) {
	info, err := transport.ParseConnectionString(state.Connection)
	if err != nil {
		return nil, transport.TransferReply{}, err
	}
	req := transport.TransferRequest{Kind: kind, ID: state.ID}
	if kind == transport.TransferReserve && state.OriginalConfig != nil {
		req.TelegramChatID = state.OriginalConfig.Setup.TelegramChatID
	}
	if kind == transport.TransferBegin {
		hash := sha256.Sum256(state.Payload)
		req.SHA256 = fmt.Sprintf("%x", hash)
		req.TotalBytes = int64(len(state.Payload))
	}
	conn, reply, err := transport.ConnectTransfer(info, req, nil)
	if err == nil && reply.Error != "" {
		conn.Close()
		return nil, reply, errors.New(reply.Error)
	}
	return conn, reply, err
}

// BackupPath keeps the original combined executable separate from role overwrite.
func BackupPath(path, id string) string {
	hash := sha256.Sum256([]byte(id))
	return filepath.Join(filepath.Dir(path), fmt.Sprintf("tracker-source-%x.exe", hash[:12]))
}
func preserveExecutable(path, id string) error {
	original := filepath.Join(filepath.Dir(path), "tracker.exe")
	data, err := os.ReadFile(original)
	if err != nil {
		return err
	}
	backup := BackupPath(path, id)
	if old, err := os.ReadFile(backup); err == nil {
		if !bytes.Equal(old, data) {
			return fmt.Errorf("source backup differs from the current executable")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	return errors.Join(writeErr, file.Sync(), file.Close())
}

// Stage freezes the existing source owner, verifies the raw destination import,
// then waits for source shutdown. It never activates the destination or collector.
func Stage(path, connection string) error {
	if _, err := transport.ParseConnectionString(connection); err != nil {
		return err
	}
	state, err := config.LoadSourceTransfer(sourcePath(path))
	if err != nil {
		return err
	}
	if state == nil {
		return fmt.Errorf("start the updated combined tracker before transferring")
	}
	if state.Phase == "idle" || state.Phase == "aborted" || state.Phase == "complete" {
		state.ID = rand.Text()
		state.Connection = connection
		state.Payload = nil
	} else if state.Connection != connection {
		return fmt.Errorf("retry the existing transfer with its original destination")
	}
	if state.Phase == "stopping" {
		conn, reply, err := remote(state, transport.TransferStatus)
		if err != nil {
			return fmt.Errorf("resolving interrupted cutover: %w", err)
		}
		conn.Close()
		switch reply.Phase {
		case transport.TransferPhaseActivated, transport.TransferPhaseApplyingSettings:
			// An installed collector may already be connected after a lost final
			// acknowledgement. Status does not require disconnecting/replacing it.
			return finishSourceCutover(path, state)
		case transport.TransferPhaseRollingBack, transport.TransferPhaseRolledBack:
			if err := Rollback(path); err != nil {
				return err
			}
			return fmt.Errorf("interrupted transfer was rolled back; rerun setup after the combined source resumes")
		}
	}
	// Reserve destination command ownership before pausing/finalizing the source.
	if state.Phase != "stopping" {
		state, err = exchangeSource(path, state, "reserve")
		if err != nil {
			return fmt.Errorf("registering source transfer: %w", err)
		}
	}
	reserved, reserveReply, err := remote(state, transport.TransferReserve)
	if err != nil {
		return failStage(path, state, err, reserveReply.ID == "" || reserveReply.Phase != "")
	}
	reserved.Close()
	if state.Phase != "stopping" {
		state, err = exchangeSource(path, state, "prepare")
		if err != nil {
			return failStage(path, state, fmt.Errorf("preparing source owner: %w", err), true)
		}
	}
	if len(state.Payload) == 0 {
		return fmt.Errorf("source has no frozen transfer snapshot")
	}
	conn, reply, err := remote(state, transport.TransferBegin)
	if err != nil {
		return failStage(path, state, err, reply.ID == "" || reply.Phase != "")
	}
	defer conn.Close()
	if reply.Phase == transport.TransferPhaseActivated {
		return finishSourceCutover(path, state)
	}
	if reply.Phase == transport.TransferPhaseReserved || reply.Phase == transport.TransferPhaseReceiving {
		for offset := reply.NextOffset; offset < int64(len(state.Payload)); {
			end := offset + transport.TransferChunkBytes
			if end > int64(len(state.Payload)) {
				end = int64(len(state.Payload))
			}
			next, err := conn.Exchange(transport.TransferRequest{Kind: transport.TransferChunk, ID: state.ID, Offset: offset, Data: state.Payload[offset:end]})
			if err != nil {
				return failStage(path, state, err, true)
			}
			if next.Error != "" {
				return failStage(path, state, errors.New(next.Error), true)
			}
			if next.NextOffset != end {
				return failStage(path, state, fmt.Errorf("destination acknowledged an unexpected chunk offset"), true)
			}
			offset = end
		}
	}
	reply, err = conn.Exchange(transport.TransferRequest{Kind: transport.TransferVerify, ID: state.ID})
	if err != nil {
		return failStage(path, state, err, true)
	}
	if reply.Error != "" {
		return failStage(path, state, errors.New(reply.Error), true)
	}
	if reply.Phase != transport.TransferPhaseVerified {
		return failStage(path, state, fmt.Errorf("destination has not verified the transfer"), true)
	}
	return finishSourceCutover(path, state)
}
func finishSourceCutover(path string, state *config.SourceTransferState) error {
	if state.Phase != "stopping" {
		if err := preserveExecutable(path, state.ID); err != nil {
			return failStage(path, state, err, true)
		}
		if _, err := exchangeSource(path, state, "shutdown"); err != nil {
			return fmt.Errorf("destination verified; source shutdown is uncertain: %w; retry this transfer", err)
		}
	}
	if err := installation.Stop(filepath.Dir(path)); err != nil {
		return fmt.Errorf("waiting for verified source shutdown: %w", err)
	}
	latest, err := config.LoadSourceTransfer(sourcePath(path))
	if err != nil {
		return err
	}
	if latest == nil || latest.ID != state.ID || !bytes.Equal(latest.Payload, state.Payload) {
		return fmt.Errorf("source transfer identity changed during shutdown")
	}
	latest.Phase = "stopping"
	latest.SourceExited = true
	return config.SaveSourceTransfer(sourcePath(path), latest)
}

func failStage(path string, state *config.SourceTransferState, cause error, remoteMayExist bool) error {
	if remoteMayExist {
		conn, reply, err := remote(state, transport.TransferStatus)
		if err != nil {
			return fmt.Errorf("transfer interrupted; source remains paused until destination status can be resolved: %w", cause)
		}
		if reply.Phase == transport.TransferPhaseActivated {
			conn.Close()
			return fmt.Errorf("destination already activated; continue collector installation rather than restoring the source")
		}
		if reply.Phase == "" && len(state.Payload) > 0 {
			conn.Close()
			return fmt.Errorf("destination lost its transfer receipt; source remains paused: %w", cause)
		}
		if reply.Phase != "" && reply.Phase != transport.TransferPhaseRolledBack {
			reply, err = conn.Exchange(transport.TransferRequest{Kind: transport.TransferRollback, ID: state.ID})
			if err != nil || reply.Error != "" || reply.Phase != transport.TransferPhaseRolledBack {
				conn.Close()
				return fmt.Errorf("transfer failed; destination rollback is unconfirmed and source remains paused: %w", cause)
			}
		}
		conn.Close()
	}
	if state.Phase == "stopping" {
		if err := Rollback(path); err != nil {
			return fmt.Errorf("transfer failed; restoring stopped source requires retry: %w; %v", cause, err)
		}
		return cause
	}
	if _, err := exchangeSource(path, state, "abort"); err != nil {
		return fmt.Errorf("transfer failed and source could not resume: %w; %v", cause, err)
	}
	return cause
}

// Activate commits verified operational settings after the old source has exited.
func Activate(path string) error {
	state, err := config.LoadSourceTransfer(sourcePath(path))
	if err != nil {
		return err
	}
	if state == nil || (state.Phase != "stopping" || !state.SourceExited) {
		return fmt.Errorf("source cutover has not been prepared")
	}
	conn, reply, err := remote(state, transport.TransferStatus)
	if err != nil {
		return err
	}
	defer conn.Close()
	if reply.Phase != transport.TransferPhaseVerified && reply.Phase != transport.TransferPhaseApplyingSettings && reply.Phase != transport.TransferPhaseActivated {
		return fmt.Errorf("destination has not verified the source snapshot")
	}
	if reply.Phase == transport.TransferPhaseActivated {
		return nil
	}
	reply, err = conn.Exchange(transport.TransferRequest{Kind: transport.TransferActivate, ID: state.ID})
	if err != nil {
		return fmt.Errorf("activation acknowledgement was lost; retry this transfer: %w", err)
	}
	if reply.Error != "" {
		return errors.New(reply.Error)
	}
	if reply.Phase != transport.TransferPhaseActivated {
		return fmt.Errorf("destination activation is unconfirmed")
	}
	return nil
}

// Rollback confirms destination ownership and rollback before restoring source
// configuration/executable. It leaves PC lock files and receipts untouched.
func Rollback(path string) error {
	state, err := config.LoadSourceTransfer(sourcePath(path))
	if err != nil {
		return err
	}
	if state == nil {
		return fmt.Errorf("no source transfer exists")
	}
	// Setup disables the installation task before requesting rollback. Stop its
	// replacement collector before reserving the destination, which deliberately
	// refuses any connected collector. On failure setup restarts the current role.
	if state.Phase == "stopping" {
		if err := installation.Stop(filepath.Dir(path)); err != nil {
			return err
		}
	}
	conn, reply, err := remote(state, transport.TransferStatus)
	if err != nil {
		return err
	}
	defer conn.Close()
	if reply.Phase == "" && len(state.Payload) > 0 {
		return fmt.Errorf("destination has no matching transfer receipt; refusing to restore a possibly duplicated source")
	}
	if reply.Phase != transport.TransferPhaseRolledBack && reply.Phase != "" {
		reply, err = conn.Exchange(transport.TransferRequest{Kind: transport.TransferRollback, ID: state.ID})
		if err != nil {
			return err
		}
		if reply.Error != "" {
			return errors.New(reply.Error)
		}
		if reply.Phase != transport.TransferPhaseRolledBack {
			return fmt.Errorf("destination rollback has not been confirmed")
		}
	}
	if state.Phase != "stopping" {
		_, err = exchangeSource(path, state, "abort")
		return err
	}
	backup, err := os.ReadFile(BackupPath(path, state.ID))
	if err != nil {
		return err
	}
	if state.OriginalConfig == nil {
		return fmt.Errorf("original source configuration is missing")
	}
	if err := config.SaveConfig(path, state.OriginalConfig); err != nil {
		return err
	}
	executable := filepath.Join(filepath.Dir(path), "tracker.exe")
	file, err := os.OpenFile(executable+".restore", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0700)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(backup)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(executable+".restore", executable); err != nil {
		return err
	}
	state.Phase = "aborted"
	state.Payload = nil
	state.Error = ""
	// The old owner is stopped; the helper now owns this terminal source update.
	return config.SaveSourceTransfer(sourcePath(path), state)
}

// Complete records installer handoff only after verified destination activation.
func Complete(path string) error {
	state, err := config.LoadSourceTransfer(sourcePath(path))
	if err != nil {
		return err
	}
	if state == nil {
		return fmt.Errorf("source transfer is missing")
	}
	conn, reply, err := remote(state, transport.TransferStatus)
	if err != nil {
		return err
	}
	defer conn.Close()
	if reply.Phase != transport.TransferPhaseActivated {
		return fmt.Errorf("destination activation is not confirmed")
	}
	state.Phase = "complete"
	return config.SaveSourceTransfer(sourcePath(path), state)
}
