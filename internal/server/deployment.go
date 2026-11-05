package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

type deploymentReceiver struct {
	server *Server
	state  *config.DeploymentTransferState
}

func (d *deploymentReceiver) path() string {
	return filepath.Join(filepath.Dir(d.server.configPath), "deployment_transfer.json")
}
func (d *deploymentReceiver) dataPath() string {
	return filepath.Join(filepath.Dir(d.server.configPath), "deployment_transfer.data")
}
func (d *deploymentReceiver) save(state *config.DeploymentTransferState) error {
	if err := config.SaveDeploymentTransfer(d.path(), state); err != nil {
		return err
	}
	d.state = state
	return nil
}
func (d *deploymentReceiver) phase(phase string) error {
	next := *d.state
	next.Phase = phase
	return d.save(&next)
}
func (d *deploymentReceiver) reserved() bool { return config.DeploymentInProgress(d.state) }
func (d *deploymentReceiver) reply(id string, err error) transport.TransferReply {
	r := transport.TransferReply{ID: id}
	if d.state != nil && d.state.ID == id {
		r.Phase = d.state.Phase
		r.NextOffset = d.state.NextOffset
		r.SHA256 = d.state.SHA256
	}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}
func (d *deploymentReceiver) handle(req transport.TransferRequest) transport.TransferReply {
	if req.Kind == transport.TransferStatus {
		return d.reply(req.ID, nil)
	}
	var err error
	if req.Kind == transport.TransferReserve {
		err = d.reserve(req)
	} else if req.Kind == transport.TransferBegin {
		err = d.begin(req)
	} else if d.state == nil || d.state.ID != req.ID {
		err = fmt.Errorf("unknown transfer identity")
	} else if d.state.Phase == transport.TransferPhaseActivated && req.Kind != transport.TransferRollback {
		err = fmt.Errorf("activated transfer cannot be changed")
	} else if d.state.Phase == transport.TransferPhaseRolledBack {
		err = fmt.Errorf("transfer was rolled back; begin a new transfer")
	} else {
		switch req.Kind {
		case transport.TransferChunk:
			err = d.chunk(req)
		case transport.TransferVerify:
			err = d.verify()
		case transport.TransferActivate:
			err = d.activate()
		case transport.TransferRollback:
			err = d.rollback()
		default:
			err = fmt.Errorf("unknown transfer operation")
		}
	}
	return d.reply(req.ID, err)
}
func (d *deploymentReceiver) reserve(req transport.TransferRequest) error {
	if d.state != nil && d.state.ID == req.ID {
		if d.state.Phase == transport.TransferPhaseRolledBack {
			return fmt.Errorf("transfer was rolled back; use a new identity")
		}
		return nil
	}
	if d.reserved() {
		return fmt.Errorf("another transfer reserves this destination")
	}
	cfg, err := config.LoadConfig(d.server.configPath)
	if err != nil {
		return err
	}
	if cfg.Setup.StorageBackend == nil {
		return fmt.Errorf("configure destination storage before transfer")
	}
	if err := d.queuesEmpty(); err != nil {
		return err
	}
	snapshot, err := d.server.store.ExportSnapshot()
	if err != nil {
		return err
	}
	if !snapshot.IsEmpty() {
		return fmt.Errorf("destination history is not empty")
	}
	before := config.GetOperational(cfg)
	candidate := *cfg
	if err := config.ApplyOperational(&candidate, config.OperationalSettings{TelegramChatID: req.TelegramChatID}); err != nil {
		return err
	}
	state := &config.DeploymentTransferState{ID: req.ID, Phase: transport.TransferPhaseReserved, Before: &before, Backend: *cfg.Setup.StorageBackend, SheetID: cfg.Setup.GoogleSheetID}
	return d.save(state)
}
func (d *deploymentReceiver) begin(req transport.TransferRequest) error {
	if err := d.reserve(req); err != nil {
		return err
	}
	if d.state.TotalBytes > 0 {
		if d.state.TotalBytes != req.TotalBytes || d.state.SHA256 != req.SHA256 {
			return fmt.Errorf("transfer manifest differs from its durable reservation")
		}
		return nil
	}
	if d.state.Phase != transport.TransferPhaseReserved {
		return fmt.Errorf("transfer is not awaiting a manifest")
	}
	file, err := os.OpenFile(d.dataPath(), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = errors.Join(file.Sync(), file.Close())
	if err != nil {
		return err
	}
	next := *d.state
	next.Phase = transport.TransferPhaseReceiving
	next.TotalBytes = req.TotalBytes
	next.SHA256 = req.SHA256
	return d.save(&next)
}

func (d *deploymentReceiver) queuesEmpty() error {
	questions, err := config.LoadPendingTournaments(filepath.Join(filepath.Dir(d.server.configPath), "pending_tournaments.json"))
	if err != nil {
		return err
	}
	if len(questions) != 0 {
		return fmt.Errorf("destination tournament questions are pending")
	}
	return d.noNewWork()
}
func (d *deploymentReceiver) noNewWork() error {
	if c, ok := d.server.store.(*sheets.Client); ok {
		if err := c.CheckTransferReady(); err != nil {
			return fmt.Errorf("destination transfer: %w", err)
		}
	}
	root := filepath.Dir(d.server.configPath)
	cash, err := config.LoadPendingCash(filepath.Join(root, "pending_cash.json"))
	if err != nil {
		return err
	}
	if len(cash) != 0 {
		return fmt.Errorf("destination cash sessions are pending")
	}
	edits, err := config.LoadCollectorSettings(filepath.Join(root, "collector_settings.json"))
	if err != nil {
		return err
	}
	if edits.Pending != nil {
		return fmt.Errorf("destination collector edits are pending")
	}
	deliveries, err := config.LoadHardstopDeliveries(filepath.Join(root, "hardstop_deliveries.json"))
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		if !delivery.Confirmed {
			return fmt.Errorf("destination protection deliveries are pending")
		}
	}
	return nil
}

func (d *deploymentReceiver) chunk(req transport.TransferRequest) error {
	if d.state.Phase != transport.TransferPhaseReceiving && d.state.Phase != transport.TransferPhaseReserved {
		return fmt.Errorf("transfer is not receiving chunks")
	}
	if req.Offset > d.state.NextOffset || int64(len(req.Data)) > d.state.TotalBytes-req.Offset {
		return fmt.Errorf("transfer chunk is out of order or exceeds its manifest")
	}
	file, err := os.OpenFile(d.dataPath(), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if req.Offset < d.state.NextOffset {
		if int64(len(req.Data)) > d.state.NextOffset-req.Offset {
			return fmt.Errorf("chunk overlaps unacknowledged bytes")
		}
		old := make([]byte, len(req.Data))
		if _, err := file.ReadAt(old, req.Offset); err != nil {
			return err
		}
		if !bytes.Equal(old, req.Data) {
			return fmt.Errorf("replayed chunk differs from acknowledged bytes")
		}
		return nil
	}
	if err := file.Truncate(d.state.NextOffset); err != nil {
		return err
	}
	if _, err := file.WriteAt(req.Data, req.Offset); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	next := *d.state
	next.Phase = transport.TransferPhaseReceiving
	next.NextOffset += int64(len(req.Data))
	return d.save(&next)
}
func (d *deploymentReceiver) payload() (storage.DeploymentPayload, error) {
	var payload storage.DeploymentPayload
	if d.state.TotalBytes == 0 || d.state.NextOffset != d.state.TotalBytes {
		return payload, fmt.Errorf("transfer has unreceived bytes")
	}
	file, err := os.Open(d.dataPath())
	if err != nil {
		return payload, err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return payload, err
	}
	if n != d.state.TotalBytes || fmt.Sprintf("%x", hash.Sum(nil)) != d.state.SHA256 {
		return payload, fmt.Errorf("transfer payload does not match its manifest")
	}
	if _, err := file.Seek(0, 0); err != nil {
		return payload, err
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, fmt.Errorf("decoding transfer payload: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return payload, fmt.Errorf("transfer payload must contain one JSON document")
	}
	if payload.ClientClock.IsZero() {
		return payload, fmt.Errorf("source collector clock is missing")
	}
	if err := storage.ValidateSnapshot(payload.Snapshot); err != nil {
		return payload, err
	}
	return payload, nil
}
func (d *deploymentReceiver) identity() (*config.Config, error) {
	cfg, err := config.LoadConfig(d.server.configPath)
	if err != nil {
		return nil, err
	}
	if cfg.Setup.StorageBackend == nil || *cfg.Setup.StorageBackend != d.state.Backend || !reflect.DeepEqual(cfg.Setup.GoogleSheetID, d.state.SheetID) {
		return nil, fmt.Errorf("destination storage identity changed during transfer")
	}
	return cfg, nil
}
func (d *deploymentReceiver) precision() time.Duration {
	if d.server.store.GetSheetURL() != "" {
		return time.Minute
	}
	return time.Second
}
func (d *deploymentReceiver) verify() error {
	if d.state.Phase != transport.TransferPhaseReceiving && d.state.Phase != transport.TransferPhaseImporting && d.state.Phase != transport.TransferPhaseVerified {
		return fmt.Errorf("transfer is not ready for verification")
	}
	payload, err := d.payload()
	if err != nil {
		return err
	}
	cfg, err := d.identity()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(config.GetOperational(cfg), *d.state.Before) {
		return fmt.Errorf("destination policy changed outside the reserved transfer")
	}
	if err := config.ApplyOperational(cfg, payload.Settings); err != nil {
		return err
	}
	if err := d.queuesEmpty(); err != nil {
		return err
	}
	actual, err := d.server.store.ExportSnapshot()
	if err != nil {
		return err
	}
	if storage.VerifySnapshot(payload.Snapshot, actual, d.precision()) != nil {
		if d.state.Phase == transport.TransferPhaseReceiving && !actual.IsEmpty() {
			return fmt.Errorf("destination is no longer empty")
		}
		if !actual.IsEmpty() {
			if err := d.clearOwned(payload.Snapshot, actual); err != nil {
				return err
			}
		}
		if err := d.phase(transport.TransferPhaseImporting); err != nil {
			return err
		}
		if err := d.server.store.ImportSnapshot(payload.Snapshot); err != nil {
			return fmt.Errorf("importing transfer rows: %w", err)
		}
		actual, err = d.server.store.ExportSnapshot()
		if err != nil {
			return err
		}
	}
	if err := storage.VerifySnapshot(payload.Snapshot, actual, d.precision()); err != nil {
		return err
	}
	bankrollCents, err := d.server.store.GetCurrentBankroll()
	if err != nil {
		return err
	}
	finalCents := money.Cents(0)
	if len(payload.Snapshot.Ledger) > 0 {
		finalCents = payload.Snapshot.Ledger[len(payload.Snapshot.Ledger)-1].RunningTotalCents
	}
	if bankrollCents != finalCents {
		return fmt.Errorf("independently read destination bankroll differs from verified ledger")
	}
	return d.phase(transport.TransferPhaseVerified)
}

// ownedPrefix compares raw rows per data set; it does not require a partial
// nontransactional import to already have a complete reconciled ledger.
func ownedPrefix(expected, actual storage.Snapshot, precision time.Duration) error {
	if len(actual.CashSessions) > len(expected.CashSessions) || len(actual.Tournaments) > len(expected.Tournaments) || len(actual.Ledger) > len(expected.Ledger) {
		return fmt.Errorf("destination contains rows outside this transfer")
	}
	// VerifySnapshot also checks sums, so compare complete zero-ledger snapshots
	// per record class and validate ledger prefixes with their original totals.
	want := storage.Snapshot{CashSessions: expected.CashSessions[:len(actual.CashSessions)], Tournaments: expected.Tournaments[:len(actual.Tournaments)], Ledger: expected.Ledger[:len(actual.Ledger)]}
	if err := storage.VerifySnapshot(want, actual, precision); err != nil {
		return fmt.Errorf("destination rows are not owned by this transfer: %w", err)
	}
	return nil
}
func (d *deploymentReceiver) clearOwned(expected, actual storage.Snapshot) error {
	if err := ownedPrefix(expected, actual, d.precision()); err != nil {
		return err
	}
	switch store := d.server.store.(type) {
	case *sqlitestore.Store:
		return store.ClearAllData()
	case *sheets.Client:
		return store.ClearAllData()
	default:
		return fmt.Errorf("unsupported destination store")
	}
}
func (d *deploymentReceiver) activate() error {
	if err := d.noNewWork(); err != nil {
		return err
	}
	if d.state.Phase != transport.TransferPhaseVerified && d.state.Phase != transport.TransferPhaseApplyingSettings {
		return fmt.Errorf("transfer has not been verified")
	}
	payload, err := d.payload()
	if err != nil {
		return err
	}
	cfg, err := d.identity()
	if err != nil {
		return err
	}
	expected := *cfg
	config.RestoreOperational(&expected, *d.state.Before)
	if err := config.ApplyOperational(&expected, payload.Settings); err != nil {
		return err
	}
	live := config.GetOperational(cfg)
	if !reflect.DeepEqual(live, *d.state.Before) && !reflect.DeepEqual(live, config.GetOperational(&expected)) {
		return fmt.Errorf("destination policy no longer belongs to this transfer")
	}
	actual, err := d.server.store.ExportSnapshot()
	if err != nil {
		return err
	}
	if err := storage.VerifySnapshot(payload.Snapshot, actual, d.precision()); err != nil {
		return err
	}
	questions, err := config.LoadPendingTournaments(filepath.Join(filepath.Dir(d.server.configPath), "pending_tournaments.json"))
	if err != nil {
		return err
	}
	if len(questions) > 0 && !reflect.DeepEqual(questions, payload.Pending) {
		return fmt.Errorf("destination questions changed outside the transfer")
	}
	if err := d.phase(transport.TransferPhaseApplyingSettings); err != nil {
		return err
	}
	if err := config.CommitDeploymentSettings(d.server.configPath, d.state.ID, payload.Settings, false); err != nil {
		return err
	}
	if err := config.CommitDeploymentPending(filepath.Join(filepath.Dir(d.server.configPath), "pending_tournaments.json"), d.state.ID, payload.Pending); err != nil {
		return err
	}
	d.server.updateClock(payload.ClientClock)
	return d.phase(transport.TransferPhaseActivated)
}
func (d *deploymentReceiver) rollback() error {
	if err := d.noNewWork(); err != nil {
		return err
	}
	payload, err := d.payload()
	if err != nil && d.state.Phase != transport.TransferPhaseReserved && d.state.Phase != transport.TransferPhaseReceiving && d.state.Phase != transport.TransferPhaseRollingBack {
		return err
	}
	cfg, err := d.identity()
	if err != nil {
		return err
	}
	current := config.GetOperational(cfg)
	desired := *cfg
	config.RestoreOperational(&desired, *d.state.Before)
	if err := config.ApplyOperational(&desired, payload.Settings); err != nil {
		return err
	}
	if !reflect.DeepEqual(current, *d.state.Before) && !reflect.DeepEqual(current, config.GetOperational(&desired)) {
		return fmt.Errorf("refusing to overwrite destination policy changed outside the transfer")
	}
	actual, err := d.server.store.ExportSnapshot()
	if err != nil {
		return err
	}
	if d.state.Phase == transport.TransferPhaseActivated {
		if err := storage.VerifySnapshot(payload.Snapshot, actual, d.precision()); err != nil {
			return fmt.Errorf("activated destination has changed; rollback refused: %w", err)
		}
	}
	if !actual.IsEmpty() {
		if err := ownedPrefix(payload.Snapshot, actual, d.precision()); err != nil {
			return err
		}
	}
	questions, err := config.LoadPendingTournaments(filepath.Join(filepath.Dir(d.server.configPath), "pending_tournaments.json"))
	if err != nil {
		return err
	}
	if len(questions) > 0 && !reflect.DeepEqual(questions, payload.Pending) {
		return fmt.Errorf("refusing to remove foreign pending questions")
	}
	if err := d.phase(transport.TransferPhaseRollingBack); err != nil {
		return err
	}
	if !actual.IsEmpty() {
		if err := d.clearOwned(payload.Snapshot, actual); err != nil {
			return err
		}
	}
	if err := config.CommitDeploymentSettings(d.server.configPath, d.state.ID, *d.state.Before, true); err != nil {
		return err
	}
	if err := config.CommitDeploymentPending(filepath.Join(filepath.Dir(d.server.configPath), "pending_tournaments.json"), d.state.ID, nil); err != nil {
		return err
	}
	return d.phase(transport.TransferPhaseRolledBack)
}
