package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

const resultQuietPeriod = parsers.CashQuietPeriod

type parseEntry struct {
	state    *parsers.ParseState
	lastSeen time.Time
}

const (
	parseStateCap = 128
	parseStateTTL = 12 * time.Hour
)

type Server struct {
	Ready                chan struct{}
	Operations           chan storage.Operation
	hardstopRequests     chan config.LockFile
	stopped              chan struct{}
	ClosureNotifications bot.ClosureNotifications
	ReleaseChecks        chan struct{}
	TransferCalls        <-chan transport.TransferCall
	Drain                <-chan struct{}
	store                storage.Store
	configPath           string
	link                 *transport.Link
	alertChan            chan<- string
	workDir              string

	QuietPeriod time.Duration

	clockMu                 sync.Mutex
	clientLoc               *time.Location
	savedClock              *config.CollectorClock
	lockMu                  sync.Mutex
	collectorLock           *config.LockFile
	collectorLockKnown      bool
	collectorLockError      string
	collectorLockConnection <-chan struct{}
}

func NewServer(store storage.Store, configPath string, link *transport.Link, alertChan chan<- string) (*Server, error) {
	clock, err := config.LoadCollectorClock(filepath.Join(filepath.Dir(configPath), "collector_clock.json"))
	if err != nil {
		return nil, err
	}
	var loc *time.Location
	if clock != nil {
		loc = clock.Location()
	}
	workDir, err := os.MkdirTemp("", "tracker-work-")
	if err != nil {
		return nil, fmt.Errorf("creating server work dir: %w", err)
	}
	return &Server{Ready: make(chan struct{}), clientLoc: loc, savedClock: clock, ClosureNotifications: bot.ClosureNotifications{Deliveries: make(chan config.ClosureEvent), Results: make(chan bot.ClosureResult)}, ReleaseChecks: make(chan struct{}, 1), TransferCalls: link.Transfers, hardstopRequests: make(chan config.LockFile), stopped: make(chan struct{}), Operations: make(chan storage.Operation), store: store, configPath: configPath, link: link, alertChan: alertChan, workDir: workDir, QuietPeriod: resultQuietPeriod}, nil
}

func (s *Server) updateClock(clientClock time.Time) {
	if clientClock.IsZero() {
		return
	}
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	s.clientLoc = clientClock.Location()
	_, offset := clientClock.Zone()
	clock := config.CollectorClock{Name: clientClock.Location().String(), OffsetSeconds: offset}
	if s.savedClock != nil && *s.savedClock == clock {
		return
	}
	if err := config.SaveCollectorClock(filepath.Join(filepath.Dir(s.configPath), "collector_clock.json"), clock); err != nil {
		slog.Error("saving collector clock", "err", err)
		return
	}
	s.savedClock = &clock
}

func (s *Server) ClientNow() time.Time {
	s.clockMu.Lock()
	loc := s.clientLoc
	s.clockMu.Unlock()
	if loc == nil {
		return time.Now()
	}
	return time.Now().In(loc)
}

func (s *Server) reconstruct(up transport.Upload) (string, error) {
	path := filepath.Join(s.workDir, cashWorkingName(up.FileID))
	if up.Type == "cash" && !up.Import {
		path = s.cashHistoryPath(up.FileID)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", fmt.Errorf("creating cash history directory: %w", err)
		}
	}
	if up.Offset == 0 {
		if err := os.WriteFile(path, up.Bytes, 0644); err != nil {
			return "", fmt.Errorf("writing working file: %w", err)
		}
		return path, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return "", fmt.Errorf("opening working file: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteAt(up.Bytes, up.Offset); err != nil {
		return "", fmt.Errorf("appending to working file: %w", err)
	}
	if err := f.Truncate(up.Offset + int64(len(up.Bytes))); err != nil {
		return "", fmt.Errorf("truncating working file: %w", err)
	}
	return path, nil
}

func cashWorkingName(fileID string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fileID)))
}

func (s *Server) cashHistoryPath(fileID string) string {
	return filepath.Join(filepath.Dir(s.configPath), "pending-cash-history", cashWorkingName(fileID))
}

func (s *Server) pruneCashHistory(pending map[string]string) {
	dir := filepath.Dir(s.cashHistoryPath(""))
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		slog.Error("reading pending cash history directory", "err", err)
		return
	}
	keep := make(map[string]bool)
	for fileID := range pending {
		keep[cashWorkingName(fileID)] = true
	}
	for _, file := range files {
		if !file.IsDir() && len(file.Name()) == sha256.Size*2 && !keep[file.Name()] {
			if _, err := hex.DecodeString(file.Name()); err != nil {
				continue
			}
			if err := os.Remove(filepath.Join(dir, file.Name())); err != nil {
				slog.Error("removing completed cash history", "err", err)
			}
		}
	}
}

func (s *Server) Run() error {
	defer close(s.stopped)
	defer os.RemoveAll(s.workDir)
	receiver := deploymentReceiver{server: s}
	var loadErr error
	receiver.state, loadErr = config.LoadDeploymentTransfer(receiver.path())
	if loadErr != nil {
		return loadErr
	}
	closures, err := loadClosureQueue(filepath.Join(filepath.Dir(s.configPath), "closure_notifications.json"))
	if err != nil {
		return err
	}
	defer closures.persist()
	var lastClosure time.Time
	for _, entry := range closures.entries {
		if entry.Event.ClosedAt.After(lastClosure) {
			lastClosure = entry.Event.ClosedAt
		}
	}
	s.clockMu.Lock()
	clockKnown := s.clientLoc != nil
	s.clockMu.Unlock()
	recoveryClock := lastClosure
	settingsQueue := collectorSettingsQueue{path: filepath.Join(filepath.Dir(s.configPath), "collector_settings.json")}
	var settingsErr error
	settingsQueue.state, settingsErr = config.LoadCollectorSettings(settingsQueue.path)
	if settingsErr != nil {
		return settingsErr
	}
	updateQueue := collectorUpdateQueue{path: filepath.Join(filepath.Dir(s.configPath), "collector_updates.json")}
	var pokerStarsState *bool
	updateQueue.state, settingsErr = config.LoadCollectorUpdates(updateQueue.path)
	if settingsErr != nil {
		return settingsErr
	}
	summaryTimers := make(map[string]*quietTimer)
	defer func() {
		for _, timer := range summaryTimers {
			timer.Stop()
		}
	}()
	pendingSessions := make(map[string]parsers.CashSession)
	pendingScreenNames := make(map[string]string)
	sessionEndChan := make(chan quietSignal)
	workerDone := make(chan struct{})
	defer close(workerDone)
	journalPath := filepath.Join(filepath.Dir(s.configPath), "pending_cash.json")
	activity := make(map[string]time.Time)
	clocks := make(map[string]time.Time)
	resyncPending := make(map[string]bool)
	recoveredSnapshots := make(map[string]config.PendingCash)
	resyncRequested := false
	recovered, err := config.LoadPendingCash(journalPath)
	if err != nil {
		return err
	}
	for _, entry := range recovered {
		pendingSessions[entry.FileID] = entry.Session
		if entry.ScreenName != "" {
			pendingScreenNames[entry.FileID] = entry.ScreenName
		}
		activity[entry.FileID] = entry.LastActivity
		clocks[entry.FileID] = entry.ClientClock
		if entry.ClientClock.After(recoveryClock) {
			recoveryClock = entry.ClientClock
		}
		resyncPending[entry.FileID] = true
		recoveredSnapshots[entry.FileID] = entry
		remaining := remainingCashQuiet(s.QuietPeriod, entry.LastActivity, time.Now())
		armQuietTimer(summaryTimers, entry.FileID, remaining, sessionEndChan, workerDone)
	}
	s.pruneCashHistory(pendingScreenNames)
	defer s.pruneCashHistory(pendingScreenNames)
	if !clockKnown {
		s.updateClock(recoveryClock)
	}
	persistPending := func() error {
		entries := make([]config.PendingCash, 0, len(pendingSessions))
		for file, session := range pendingSessions {
			entries = append(entries, config.PendingCash{FileID: file, Session: session, LastActivity: activity[file], ClientClock: clocks[file], ScreenName: pendingScreenNames[file]})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].FileID < entries[j].FileID })
		return config.SavePendingCash(journalPath, entries)
	}

	firedAlerts := make(map[string]bool)

	screenNameWarned := false

	var importCounts storage.ScanTally
	var importTournaments tournamentScan
	var folderScan bool
	var folderScanDisconnected <-chan struct{}
	var scanningFile *fileScan
	defer func() {
		if scanningFile != nil {
			scanningFile.close()
		}
	}()

	parseStates := make(map[string]*parseEntry)

	deliveriesPath := filepath.Join(filepath.Dir(s.configPath), "hardstop_deliveries.json")
	deliveries, err := config.LoadHardstopDeliveries(deliveriesPath)
	if err != nil {
		return err
	}
	sentLocks := make(map[string]bool)
	var confirmedLock *config.LockFile
	for _, entry := range deliveries {
		if lock := entry.EffectiveLock; entry.Confirmed && lock != nil && lock.ExpiresAt.After(time.Now()) && (confirmedLock == nil || lock.ExpiresAt.After(confirmedLock.ExpiresAt)) {
			copy := *lock
			confirmedLock = &copy
		}
	}
	restoredLockSent := false
	lockFailures := make(map[string]bool)
	deliveriesDirty := false
	saveDeliveries := func() error {
		merged, err := config.MergeHardstopDeliveries(deliveriesPath, deliveries)
		if err == nil {
			deliveries = merged
		}
		return err
	}
	refreshDeliveries := func() {
		stored, err := config.LoadHardstopDeliveries(deliveriesPath)
		if err != nil {
			slog.Error("refreshing hard-stop deliveries", "err", err)
			return
		}
		for id, entry := range stored {
			if _, known := deliveries[id]; !known || entry.Confirmed {
				deliveries[id] = entry
			}
		}
	}

	enforce := func(reason string, durationHours int, id string) {
		if _, known := deliveries[id]; known {
			return
		}
		lock, _, err := hardstop.BuildTrigger(reason, durationHours, time.Now().UTC())
		if err != nil {
			slog.Error("building hard stop", "err", err)
			return
		}
		lock.TriggerID = id
		deliveries[id] = config.HardstopDelivery{Lock: lock}
		if err := saveDeliveries(); err != nil {
			deliveriesDirty = true
			slog.Error("persisting outgoing hard stop", "err", err)
			bot.PostAlert(s.alertChan, "⚠️ Could not save the hard-stop delivery locally. Protection is being requested; keep the tracker running and check logs.")
		} else {
			deliveriesDirty = false
		}
	}

	tournaments := tournamentResults{
		server: s, path: filepath.Join(filepath.Dir(s.configPath), "pending_tournaments.json"),
		timers: make(map[string]*quietTimer), signals: make(chan quietSignal), done: workerDone,
		enforce: enforce, fired: firedAlerts,
	}
	if !receiver.reserved() {
		if err := tournaments.restoreTimers(); err != nil {
			return err
		}
	}
	defer func() {
		for _, timer := range tournaments.timers {
			timer.Stop()
		}
	}()

	finalizeCash := func(fileID string) error {
		snapshot, present := pendingSessions[fileID]
		if err := s.endCashSession(fileID, pendingScreenNames[fileID], pendingSessions, summaryTimers, firedAlerts, enforce); err != nil {
			return err
		}
		if deliveriesDirty && present {
			pendingSessions[fileID] = snapshot
			if err := persistPending(); err != nil {
				return err
			}
			return fmt.Errorf("cash recorded but hard-stop delivery could not be persisted")
		}
		delete(activity, fileID)
		delete(clocks, fileID)
		delete(pendingScreenNames, fileID)
		return persistPending()
	}
	var pendingCollection *collectionOperation
	var offlinePreparation *storage.Operation
	var offlineDrainError error
	recordDrainFailure := func(err error) {
		if err != nil && pendingCollection != nil && pendingCollection.command.Kind != transport.CommandResumeCollection {
			pendingCollection.result.Err = errors.Join(pendingCollection.result.Err, err)
		}
		if err != nil && offlinePreparation != nil {
			offlineDrainError = errors.Join(offlineDrainError, err)
		}
	}
	var heldOperation *storage.Operation
	heldCollectionPaused := false
	prepareRestart := func(operation storage.Operation) error {
		if operation.PrepareMigration || operation.FinalizeAllCash {
			ids := make([]string, 0, len(pendingSessions))
			for id := range pendingSessions {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				if err := finalizeCash(id); err != nil {
					return err
				}
			}
		}
		if operation.RequireConfirmedProtection {
			if err := saveDeliveries(); err != nil {
				return fmt.Errorf("persisting source protection before transfer: %w", err)
			}
			deliveriesDirty = false
			for _, delivery := range deliveries {
				if !delivery.Confirmed {
					return fmt.Errorf("source protection is unconfirmed; keep the tracker running and retry after collector acknowledgement")
				}
			}
		}
		return persistPending()
	}
	var requestSequence uint64
	nextRequestID := func() string {
		requestSequence++
		return fmt.Sprintf("migration-%d", requestSequence)
	}
	drain := s.Drain
	close(s.Ready)
	for {
		if offlinePreparation != nil && len(s.link.Uploads) == 0 {
			operation := *offlinePreparation
			done := s.link.CollectorDone()
			select {
			case <-done:
				result := storage.OperationResult{Err: offlineDrainError}
				if result.Err == nil {
					result.Err = prepareRestart(operation)
				}
				if result.Err == nil {
					result = operation.Apply(s.store)
				}
				if result.Err == nil {
					heldOperation = &operation
					heldCollectionPaused = false
				}
				operation.Reply <- result
			default:
				pendingCollection = &collectionOperation{operation: operation, command: transport.Command{Kind: transport.CommandDrainCollection, RequestID: nextRequestID()}, disconnected: done, result: storage.OperationResult{Err: offlineDrainError}}
			}
			offlinePreparation = nil
			offlineDrainError = nil
			continue
		}
		var operations <-chan storage.Operation
		var uploads <-chan transport.Upload
		var endings, tournamentEndings <-chan quietSignal
		var release <-chan struct{}
		var commands chan<- transport.Command
		var command transport.Command
		restoringLock := false
		var disconnected, scanDisconnected, folderDisconnected <-chan struct{}
		if len(s.link.Uploads) == 0 {
			folderDisconnected = folderScanDisconnected
		}
		var scanResults <-chan scanResult
		if heldOperation == nil {
			uploads = s.link.Uploads
			endings = sessionEndChan
			if !folderScan && scanningFile == nil && len(s.link.Uploads) == 0 {
				tournamentEndings = tournaments.signals
			}
			if pendingCollection == nil && offlinePreparation == nil {
				operations = s.Operations
			}
		} else {
			release = heldOperation.Release
		}
		if pendingCollection != nil {
			disconnected = pendingCollection.disconnected
			if !pendingCollection.sent {
				commands = s.link.Commands
				command = pendingCollection.command
			}
		}
		if pendingCollection == nil && offlinePreparation == nil && heldOperation == nil {
			select {
			case <-s.link.CollectorDone():
			default:
				if next := settingsQueue.command(); next != nil {
					commands, command = s.link.Commands, *next
				}
				if commands == nil {
					if next := updateQueue.command(); next != nil {
						commands, command = s.link.Commands, *next
					}
				}
			}
		}
		if scanningFile != nil {
			scanDisconnected = scanningFile.disconnected
			scanResults = scanningFile.results
			if commands == nil && !scanningFile.sent {
				commands, command = s.link.Commands, scanningFile.command
			}
		}
		select {
		case <-s.link.CollectorDone():
		default:
			if confirmedLock != nil && confirmedLock.ExpiresAt.After(time.Now()) && !restoredLockSent {
				copy := *confirmedLock
				commands = s.link.Commands
				command = transport.Command{Kind: transport.CommandLock, Lock: &copy}
				restoringLock = true
			}
			ids := make([]string, 0)
			for id, entry := range deliveries {
				if !entry.Confirmed && !sentLocks[id] {
					ids = append(ids, id)
				}
			}
			if len(ids) > 0 {
				sort.Strings(ids)
				lock := deliveries[ids[0]].Lock
				commands = s.link.Commands
				command = transport.Command{Kind: transport.CommandLock, Lock: &lock}
				restoringLock = false
			}
		}
		var resync chan<- transport.Command
		var resyncCommand transport.Command
		if len(resyncPending) > 0 && !resyncRequested && pendingCollection == nil && offlinePreparation == nil && heldOperation == nil {
			select {
			case <-s.link.CollectorDone():
			default:
				resync = s.link.Commands
				resyncCommand.Kind = transport.CommandResyncFiles
				for file := range resyncPending {
					resyncCommand.Files = append(resyncCommand.Files, file)
				}
				sort.Strings(resyncCommand.Files)
			}
		}
		var closureDelivery chan config.ClosureEvent
		var closureEvent config.ClosureEvent
		if next := closures.next(); next != nil && !receiver.reserved() {
			closureDelivery, closureEvent = s.ClosureNotifications.Deliveries, *next
		}
		var closureCommands chan<- transport.Command
		var closureCommand transport.Command
		if !receiver.reserved() {
			select {
			case <-s.link.CollectorDone():
			default:
				if closures.replay {
					closureCommands, closureCommand = s.link.Commands, transport.Command{Kind: transport.CommandReplayClosures}
				}
				for id := range closures.acknowledgements {
					closureCommands, closureCommand = s.link.Commands, transport.Command{Kind: transport.CommandClosureAck, ClosureID: id}
					break
				}
			}
		}
		var up transport.Upload

		if receiver.reserved() {
			endings, tournamentEndings = nil, nil
			commands = nil
			resync = nil
		}
		select {
		case closureDelivery <- closureEvent:
			closures.attempted[closureEvent.ID] = true
			closures.inflight[closureEvent.ID] = true
			continue
		case result := <-s.ClosureNotifications.Results:
			closures.result(result)
			continue
		case closureCommands <- closureCommand:
			if closureCommand.Kind == transport.CommandClosureAck {
				delete(closures.acknowledgements, closureCommand.ClosureID)
			} else {
				closures.replay = false
			}
			continue
		case call := <-s.TransferCalls:
			if (call.Request.Kind == transport.TransferReserve || call.Request.Kind == transport.TransferBegin) && (scanningFile != nil || folderScan || len(s.link.Uploads) > 0) {
				call.Reply <- receiver.reply(call.Request.ID, fmt.Errorf("accepted collector uploads have not drained"))
			} else {
				wasReserved := receiver.reserved()
				reply := receiver.handle(call.Request)
				if wasReserved && !receiver.reserved() {
					if err := tournaments.restoreTimers(); err != nil {
						slog.Error("restoring tournament recovery after transfer", "err", err)
					}
				}
				call.Reply <- reply
			}
			continue
		case <-drain:
			drain = nil
			heldOperation = nil
			continue
		case lock := <-s.hardstopRequests:
			if _, exists := deliveries[lock.TriggerID]; !exists {
				deliveries[lock.TriggerID] = config.HardstopDelivery{Lock: lock}
			}
			if err := saveDeliveries(); err != nil {
				deliveriesDirty = true
				slog.Error("saving manual hard-stop delivery", "err", err)
			}
			continue
		case <-s.link.ConnectionChanged:
			folderScan = false
			updateQueue.sent = false
			closures.retry()
			settingsQueue.inflight = nil
			sentLocks = make(map[string]bool)
			restoredLockSent = false
			resyncRequested = false
			continue
		case resync <- resyncCommand:
			resyncRequested = true
			continue

		case commands <- command:
			if command.Kind == transport.CommandLock {
				if restoringLock {
					restoredLockSent = true
				} else {
					sentLocks[command.Lock.TriggerID] = true
				}
			} else if command.Kind == transport.CommandCollectorConfig {
				settingsQueue.inflight = &config.CollectorChange{ID: command.RequestID, Settings: *command.Settings}
			} else if command.Kind == transport.CommandUpdate {
				updateQueue.sent = true
			} else if command.Kind == transport.CommandScanFile {
				scanningFile.sent = true
			} else {
				pendingCollection.sent = true
			}
			continue
		case parsed := <-scanResults:
			scanningFile.record(&tournaments, parsed)
			if parsed.done {
				scanningFile.request.Reply <- scanningFile.result
				scanningFile.close()
				scanningFile = nil
			}
			continue
		case <-folderDisconnected:
			folderScan, folderScanDisconnected = false, nil
			importCounts, importTournaments = storage.ScanTally{}, tournamentScan{}
			continue
		case <-scanDisconnected:
			scanningFile.request.Reply <- storage.OperationResult{Err: fmt.Errorf("the poker PC disconnected before the file finished transferring")}
			scanningFile.close()
			scanningFile = nil
			continue
		case <-disconnected:
			pendingCollection.finish(collectorFailure())
			pendingCollection = nil
			continue
		case <-release:
			if !heldCollectionPaused {
				heldOperation.Resumed <- nil
				heldOperation = nil
				continue
			}
			pendingCollection = &collectionOperation{operation: *heldOperation, replied: true}
			pendingCollection.resume(nextRequestID(), s.link.CollectorDone())
			heldOperation = nil
			continue
		case operation := <-operations:
			if receiver.reserved() {
				operation.Reply <- storage.OperationResult{Err: fmt.Errorf("destination is reserved for deployment transfer")}
				continue
			}
			if answer := operation.TournamentAnswer; answer != nil {
				operation.Reply <- tournaments.confirm(*answer)
				continue
			}
			if operation.ScanPath != "" || operation.ScanAttachment != "" {
				if scanningFile != nil || folderScan {
					operation.Reply <- storage.OperationResult{Err: fmt.Errorf("a scan is already in progress")}
					continue
				}
				if operation.ScanAttachment != "" {
					var screenName string
					if settings := settingsQueue.state.Confirmed; settings != nil && settings.ScreenName != nil {
						screenName = *settings.ScreenName
					}
					scanningFile = &fileScan{request: operation}
					scanningFile.start(operation.ScanAttachment, screenName, false)
					continue
				}
				done := s.link.CollectorDone()
				select {
				case <-done:
					operation.Reply <- storage.OperationResult{Err: fmt.Errorf("the poker PC is offline")}
				default:
					scanningFile = &fileScan{
						request: operation, disconnected: done,
						command: transport.Command{Kind: transport.CommandScanFile, RequestID: nextRequestID(), ScanPath: operation.ScanPath},
					}
				}
				continue
			}
			if operation.Updates {
				var err error
				if operation.UpdateRequest != nil {
					err = updateQueue.accept(*operation.UpdateRequest)
				}
				online := true
				select {
				case <-s.link.CollectorDone():
					online = false
				default:
				}
				operation.Reply <- storage.OperationResult{Updates: updateQueue.state, CollectorOnline: online, Err: err}
				continue
			}
			if operation.PrepareMigration || operation.PrepareRestart {
				if scanningFile != nil || folderScan {
					operation.Reply <- storage.OperationResult{Err: fmt.Errorf("wait for the scan to finish before changing storage or configuration")}
					continue
				}
				if err := config.CheckUpdatesIdle(filepath.Dir(s.configPath)); err != nil {
					operation.Reply <- storage.OperationResult{Err: err}
					continue
				}
			}
			if operation.CollectorSettings {
				var err error
				if operation.CollectorChange != nil {
					err = settingsQueue.accept(*operation.CollectorChange)
				}
				online := true
				select {
				case <-s.link.CollectorDone():
					online = false
				default:
				}
				operation.Reply <- storage.OperationResult{CollectorSettings: settingsQueue.state, CollectorOnline: online, Err: err}
				continue
			}
			if operation.PrepareMigration {
				if err := config.CheckClosureTransferReady(filepath.Dir(s.configPath)); err != nil {
					operation.Reply <- storage.OperationResult{Err: err}
					continue
				}
				if client, ok := s.store.(*sheets.Client); ok {
					if err := client.CheckTransferReady(); err != nil {
						operation.Reply <- storage.OperationResult{Err: err}
						continue
					}
				}
			}
			if !operation.PrepareMigration && !operation.PrepareRestart {

				if operation.RetryHardstops {
					closures.retry()
					refreshDeliveries()
					sentLocks = make(map[string]bool)
					restoredLockSent = false
					if deliveriesDirty {
						if err := saveDeliveries(); err == nil {
							deliveriesDirty = false
						} else {
							slog.Error("persisting queued hard-stop deliveries", "err", err)
						}
					}
				}
				result := operation.Apply(s.store)
				if operation.FinalizeCash {
					if err := tournaments.restoreExpired(); err != nil {
						slog.Error("restoring expired tournament recovery", "err", err)
					}
					for file := range pendingSessions {
						if time.Since(activity[file]) >= s.QuietPeriod {
							if err := finalizeCash(file); err != nil {
								slog.Error("finalizing recovered cash after Sheets drain", "err", err)
							}
						}
					}
				}
				operation.Reply <- result
				continue
			}
			done := s.link.CollectorDone()
			select {
			case <-done:
				if operation.PrepareRestart && !operation.PrepareMigration {
					offlinePreparation = &operation
					continue
				}
				operation.Reply <- storage.OperationResult{Err: collectorFailure()}
				continue
			default:
			}
			pendingCollection = &collectionOperation{operation: operation, command: transport.Command{Kind: transport.CommandPauseCollection, RequestID: nextRequestID()}, disconnected: done}
			if operation.PrepareRestart && !operation.PrepareMigration {
				pendingCollection.command.Kind = transport.CommandDrainCollection
			}
			continue
		case signal := <-tournamentEndings:
			if tournaments.timers[signal.Key] == signal.Generation {
				stopQuietTimer(tournaments.timers, signal.Key)
				if err := tournaments.expire(signal.Key); err != nil {
					recordDrainFailure(err)
					slog.Error("finalizing pending tournament", "err", err)
				}
			}
			continue
		case signal := <-endings:
			if summaryTimers[signal.Key] != signal.Generation {
				continue
			}
			fileID := signal.Key
			if err := finalizeCash(fileID); err != nil {
				slog.Error("finalizing pending cash", "err", err)
			}
			continue
		case u, ok := <-uploads:
			if !ok {
				return persistPending()
			}
			up = u
		}

		if receiver.reserved() {
			return fmt.Errorf("collector upload reached reserved deployment destination")
		}
		connection, current := s.link.UploadConnection(up)
		if chunk := up.Scan; chunk != nil {
			if current && scanningFile != nil && connection == scanningFile.disconnected && scanningFile.command.RequestID == chunk.RequestID {
				if err := scanningFile.accept(*chunk); err != nil {
					scanningFile.request.Reply <- storage.OperationResult{Err: err}
					scanningFile.close()
					scanningFile = nil
				}
			}
			continue
		}
		if (up.LockReply != nil || up.CollectionReply != nil || up.Status != nil || up.ConfigReply != nil || up.Update != nil || up.UpdateReply != nil) && !current {
			continue
		}
		if running := up.PokerStarsRunning; running != nil && current {
			alreadyPlaying := pokerStarsState == nil && *running
			changed := pokerStarsState != nil && *pokerStarsState != *running
			if alreadyPlaying || changed {
				select {
				case s.ReleaseChecks <- struct{}{}:
				default:
				}
			}
			pokerStarsState = running
		}
		if event := up.Closure; event != nil {
			if current {
				s.updateClock(up.ClientClock)
			}
			if err := closures.receive(*event); err != nil {
				slog.Error("receiving PokerStars closure event", "err", err)
			}
			continue
		}
		if status := up.Status; status != nil {
			if status.Update != nil {
				if err := updateQueue.observe(*status.Update); err != nil {
					return err
				}
			}
			s.updateClock(up.ClientClock)
			s.observeCollectorLock(status.Lock, status.Error, connection)
			if err := s.saveCollectorObservation(status.Lock, status.Error); err != nil {
				slog.Error("saving collector observation", "err", err)
			}
			if status.Settings != nil {
				if err := settingsQueue.observe(*status.Settings, status.Watching, status.CollectionError); err != nil {
					return err
				}
				if status.CollectionError != "" {
					bot.PostAlert(s.alertChan, "⚠️ Collection is inactive on the poker PC: "+status.CollectionError+"\nThe collector remains connected so /paths and /screenname can correct it. Hard-stop enforcement remains active.")
				}
			}
			continue
		}
		if up.Update != nil {
			s.updateClock(up.ClientClock)
			if err := updateQueue.observe(*up.Update); err != nil {
				return err
			}
			continue
		}
		if up.UpdateReply != nil {
			s.updateClock(up.ClientClock)
			if err := updateQueue.acknowledge(*up.UpdateReply); err != nil {
				return err
			}
			continue
		}
		if reply := up.ConfigReply; reply != nil {
			s.updateClock(up.ClientClock)
			message, err := settingsQueue.acknowledge(*reply)
			if err != nil {
				return err
			}
			if message != "" {
				bot.PostAlert(s.alertChan, message)
			}
			continue
		}
		if reply := up.LockReply; reply != nil {
			s.updateClock(up.ClientClock)
			s.observeCollectorLock(reply.Lock, reply.Error, connection)
			if err := s.saveCollectorObservation(reply.Lock, reply.Error); err != nil {
				slog.Error("saving collector observation", "err", err)
			}
			refreshDeliveries()
			entry, known := deliveries[reply.TriggerID]
			if !known || entry.Confirmed {
				continue
			}
			if reply.Error != "" {
				if !lockFailures[reply.TriggerID] {
					bot.PostAlert(s.alertChan, "⚠️ The collector could not complete hard-stop protection. Keep it running and check its logs.")
					lockFailures[reply.TriggerID] = true
				}
				continue
			}
			delete(lockFailures, reply.TriggerID)
			if !known || entry.Confirmed {
				continue
			}
			previous := entry
			entry.Confirmed = true
			entry.EffectiveLock = reply.Lock
			deliveries[reply.TriggerID] = entry
			if err := saveDeliveries(); err != nil {
				deliveries[reply.TriggerID] = previous
				deliveriesDirty = true
				slog.Error("saving hard-stop acknowledgement", "err", err)
				continue
			}
			deliveriesDirty = false
			if lock := reply.Lock; lock != nil && (confirmedLock == nil || lock.ExpiresAt.After(confirmedLock.ExpiresAt)) {
				copy := *lock
				confirmedLock = &copy
			}
			if reply.Lock != nil && confirmedLock != nil && !confirmedLock.ExpiresAt.After(reply.Lock.ExpiresAt) {
				restoredLockSent = true
			}
			if reply.Lock != nil {
				bot.PostAlert(s.alertChan, hardstop.LockMessage(*reply.Lock, s.ClientNow()))
			}
			continue
		}

		if reply := up.CollectionReply; reply != nil {
			s.updateClock(up.ClientClock)
			if pendingCollection == nil || reply.RequestID != pendingCollection.command.RequestID {
				continue
			}
			select {
			case <-pendingCollection.disconnected:
				pendingCollection.finish(collectorFailure())
				pendingCollection = nil
				continue
			default:
			}

			if pendingCollection.command.Kind == transport.CommandResumeCollection {
				var err error
				if reply.Error != "" {
					err = fmt.Errorf("resuming collection: %s", reply.Error)
				}
				pendingCollection.finish(err)
				pendingCollection = nil
				continue
			}
			if reply.Error != "" {
				pendingCollection.finish(fmt.Errorf("cannot pause collection: %s", reply.Error))
				pendingCollection = nil
				continue
			}
			err := pendingCollection.result.Err
			if err == nil {
				err = prepareRestart(pendingCollection.operation)
			}
			if err == nil && pendingCollection.operation.PrepareMigration && sheetsPending(s.store) > 0 {
				err = fmt.Errorf("source has pending Sheets writes; wait for recovery before migrating")
			}
			if err != nil {
				pendingCollection.result.Err = err
			} else {
				pendingCollection.result = pendingCollection.operation.Apply(s.store)
			}
			if pendingCollection.result.Err != nil {
				pendingCollection.resume(nextRequestID(), s.link.CollectorDone())
				continue
			}
			operation := pendingCollection.operation
			heldOperation = &operation
			heldCollectionPaused = true
			operation.Reply <- pendingCollection.result
			pendingCollection = nil
			continue
		}

		s.updateClock(up.ClientClock)

		switch up.Type {
		case transport.ImportStart:
			folderScan, folderScanDisconnected = true, connection
			importCounts = storage.ScanTally{}
			importTournaments = tournamentScan{}
			continue
		case transport.ImportDone:
			folderScan, folderScanDisconnected = false, nil
			if err := importTournaments.finish(&tournaments, &importCounts); err != nil {
				slog.Error("recovering tournament results from folder scan", "err", err)
			}
			bot.PostAlert(s.alertChan, importCounts.String())
			importCounts = storage.ScanTally{}
			importTournaments = tournamentScan{}
			continue
		}

		path, err := s.reconstruct(up)
		if err != nil {
			recordDrainFailure(err)
			slog.Error("reconstructing uploaded file", "err", err, "file", up.FileID)
			continue
		}

		cfg, err := config.LoadConfig(s.configPath)
		if err != nil {
			recordDrainFailure(err)
			slog.Error("loading config", "err", err)
			continue
		}
		if up.ScreenName == "" {
			recordDrainFailure(fmt.Errorf("screen name missing for drained file %s", up.FileID))
			slog.Error("pokerstars screen name not configured, skipping file", "file", up.FileID)
			if !screenNameWarned {
				bot.PostAlert(s.alertChan, "🔧 I found a poker file but don't know your PokerStars screen name yet — I can't track anything until it's set. Open /menu → Setup → Screen name.")
				screenNameWarned = true
			}
			continue
		}
		screenName := up.ScreenName

		if up.Import {
			s.updateClock(up.ClientClock)
			switch up.Type {
			case "cash":
				err := parsers.ScanHistoryFile(path, screenName, func(session *parsers.CashSession, hands []parsers.CashHand, stats *parsers.TournamentHandStats, err error) bool {
					recordDrainFailure(importTournaments.record(s.store, &importCounts, scanResult{cash: session, hands: hands, tournament: stats, err: err}))
					return true
				}, func(summary parsers.Tournament, err error) bool {
					if err != nil {
						recordDrainFailure(importTournaments.record(s.store, &importCounts, scanResult{err: err}))
					} else {
						recordDrainFailure(importTournaments.summary(&tournaments, &importCounts, summary))
					}
					return true
				})
				if err != nil {
					recordDrainFailure(importTournaments.record(s.store, &importCounts, scanResult{err: err}))
				}
			case "tournament":
				t, err := parsers.ParseTournamentFile(path, screenName)
				if err != nil {
					importCounts.Failed++
					slog.Error("parsing tournament summary during scan", "err", err)
				} else {
					err = importTournaments.summary(&tournaments, &importCounts, t)
				}
				recordDrainFailure(err)
			}
			continue
		}

		evalCfg := *cfg
		evalCfg.Alerts = config.EffectiveAlerts(cfg.Alerts)
		evalCfg.Hardstop = config.EffectiveHardstop(cfg.Hardstop)

		s.updateClock(up.ClientClock)
		evalNow := s.ClientNow()

		switch up.Type {
		case "cash":
			now := time.Now()
			if len(parseStates) > parseStateCap {
				for p, e := range parseStates {
					if now.Sub(e.lastSeen) > parseStateTTL {
						delete(parseStates, p)
					}
				}
			}
			lastActivity := now
			if up.Recovery {
				lastActivity = activity[up.FileID]
				if previous, ok := recoveredSnapshots[up.FileID]; ok && lastActivity.IsZero() {
					lastActivity = previous.LastActivity
				}
				observed := up.FileActivity
				if observed.After(now) {
					observed = now
				}
				if observed.After(lastActivity) {
					lastActivity = observed
				}
			}
			var prior *parsers.ParseState
			if up.Offset == 0 {
				delete(parseStates, up.FileID)
			}
			if e := parseStates[up.FileID]; e != nil {
				prior = e.state
			}
			session, tourneyStats, newState, err := parsers.ParseCashFileFrom(path, screenName, prior)
			if err != nil || session == nil && tourneyStats == nil {
				if err != nil {
					recordDrainFailure(err)
					slog.Error("parsing cash file", "file", up.FileID, "err", err)
				}
				if _, exists := pendingSessions[up.FileID]; exists {
					activity[up.FileID] = lastActivity
					clocks[up.FileID] = up.ClientClock
					if err := persistPending(); err != nil {
						return err
					}
					armQuietTimer(summaryTimers, up.FileID, remainingCashQuiet(s.QuietPeriod, lastActivity, time.Now()), sessionEndChan, workerDone)
				}

				continue
			}
			parseStates[up.FileID] = &parseEntry{state: newState, lastSeen: now}
			if session != nil {
				if previous, known := recoveredSnapshots[up.FileID]; known && up.Recovery {
					_, pending := pendingSessions[up.FileID]
					if !pending && !up.FileActivity.After(previous.LastActivity) && sameCashSnapshot(previous.Session, *session) {
						delete(resyncPending, up.FileID)
						delete(recoveredSnapshots, up.FileID)
						continue
					}
				}
				if err := hardstop.EvaluateSessionLoss(*session, &evalCfg, automaticEnforcement(&evalCfg, evalNow, session, nil, nil, nil, enforce)); err != nil {
					recordDrainFailure(err)
					slog.Error("evaluating session-loss hard stop", "err", err)
				}
				bot.EvaluateSessionLossAlert(*session, &evalCfg, func() money.Cents {
					priorCents, _ := s.store.GetCurrentBankroll()
					return priorCents + session.NetCents
				}, s.alertChan)

				pendingSessions[up.FileID] = *session
				pendingScreenNames[up.FileID] = screenName
				activity[up.FileID] = lastActivity
				clocks[up.FileID] = up.ClientClock
				if err := persistPending(); err != nil {
					return fmt.Errorf("journaling processed cash: %w", err)
				}
				delete(resyncPending, up.FileID)
				delete(recoveredSnapshots, up.FileID)
				armQuietTimer(summaryTimers, up.FileID, remainingCashQuiet(s.QuietPeriod, lastActivity, time.Now()), sessionEndChan, workerDone)
			} else if tourneyStats != nil {
				if err := tournaments.invalidateFinish(*tourneyStats); err != nil {
					return fmt.Errorf("invalidating tournament finish: %w", err)
				}
				before := sheetsPending(s.store)
				err := s.store.UpdateTournamentHandStats(tourneyStats.TournamentID, tourneyStats.VPIP, tourneyStats.PFR, tourneyStats.Hands, tourneyStats.BountyCents, tourneyStats.IsKnockout)
				if err != nil {
					recordDrainFailure(err)
					slog.Error("updating tournament hand stats", "err", err, "tournament_id", tourneyStats.TournamentID)
					if msg := writeFailureMessage(before, sheetsPending(s.store), err); msg != "" {
						bot.PostAlert(s.alertChan, msg)
					}
				} else if msg := syncedMessage(before, sheetsPending(s.store)); msg != "" {
					bot.PostAlert(s.alertChan, msg)
				}
				if err == nil && tourneyStats.Finish != nil {
					if lastActivity.IsZero() {
						lastActivity = now
					}
					_, _, err := tournaments.history(*tourneyStats, lastActivity, false)
					if err != nil {
						return fmt.Errorf("journaling tournament activity: %w", err)
					}
				}
			} else {
				slog.Error("ParseCashFile returned no session and no tournament stats", "file", up.FileID)
			}

		case "tournament":
			t, err := parsers.ParseTournamentFile(path, screenName)
			if err != nil {
				recordDrainFailure(err)
				slog.Error("parsing tournament", "file", up.FileID, "err", err)
				continue
			}

			if _, _, err := tournaments.official(t, false); err != nil {
				recordDrainFailure(err)
				slog.Error("processing tournament summary", "err", err)
			}
		}
	}
}

func sheetsPending(sc storage.Store) int {
	if c, ok := sc.(*sheets.Client); ok {
		return c.PendingWrites()
	}
	return 0
}

func syncedMessage(before, after int) string {
	if n := before - after; n > 0 {
		return fmt.Sprintf("✅ Synced %d queued write(s) to Sheets", n)
	}
	return ""
}

func writeFailureMessage(before, after int, err error) string {
	var writeErr *storage.WriteError
	if errors.As(err, &writeErr) && !writeErr.Queued {
		return "⚠️ Could not save the failed write locally. Keep the source histories for recovery and check logs."
	}
	return blipMessage(before, after)
}

func blipMessage(before, after int) string {
	if before == 0 && after > 0 {
		return "💔 Sheets briefly unreachable — your results are saved locally and I'll sync them automatically when it's back."
	}
	return ""
}

func evaluateOncePerKeyAlerts(cfg *config.Config, ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament, bankrollCents money.Cents, now time.Time, alertChan chan<- string, firedAlerts map[string]bool, sessionKey string) {
	if !firedAlerts["streak:"+sessionKey] {
		if bot.EvaluateLosingStreakAlert(cfg, ledger, bankrollCents, alertChan) {
			firedAlerts["streak:"+sessionKey] = true
		}
	}

	dayKey := hardstop.PokerDayStart(cfg, now).Format(time.RFC3339)
	if !firedAlerts["daily:"+dayKey] {
		if bot.EvaluateDailyLossAlert(cfg, ledger, bankrollCents, now, alertChan) {
			firedAlerts["daily:"+dayKey] = true
		}
	}
	if !firedAlerts["spingo_daily:"+dayKey] {
		if bot.EvaluateSpingoDailyLossAlert(cfg, dayTournaments, bankrollCents, alertChan) {
			firedAlerts["spingo_daily:"+dayKey] = true
		}
	}
}

func fetchEvalData(sc storage.Store, cfg *config.Config, now time.Time, forceLedger bool) (ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament) {
	a, h := cfg.Alerts, cfg.Hardstop
	needLedger := forceLedger ||
		a.SessionLossBuyins != nil || a.DownswingBuyins != nil || a.LosingStreakCount != nil ||
		a.DailyLossCents != nil || a.SpingoDailyLossCents != nil ||
		h.ConsecutiveLosses != nil || h.DownswingBuyins != nil || h.DailyLossCents != nil
	if needLedger {
		var err error
		ledger, err = sc.GetLedgerEntriesSince(time.Time{})
		if err != nil {
			slog.Error("reading bankroll ledger for alert evaluation", "err", err)
		}
	}
	if a.SpingoDailyLossCents != nil || h.SpingoDailyLossCents != nil {
		var err error
		dayTournaments, err = sc.GetTournamentsSince(hardstop.PokerDayStart(cfg, now))
		if err != nil {
			slog.Error("reading tournaments for daily loss evaluation", "err", err)
		}
	}
	return ledger, dayTournaments
}

func ledgerBankroll(ledger []storage.LedgerEntry) money.Cents {
	if len(ledger) == 0 {
		return 0
	}
	return ledger[len(ledger)-1].RunningTotalCents
}

func isDuplicateTournament(t parsers.Tournament, pending []config.PendingTournament, recorded parsers.Tournament, recordedFound bool) (duplicate bool, refreshPendingIdx int) {
	for i, p := range pending {
		if p.Tournament.TournamentID == t.TournamentID {
			if sameTournamentContent(t, p.Tournament) {
				return true, -1
			}
			return true, i
		}
	}
	if recordedFound && !recorded.Recovered() {
		if recorded.BuyInCents == 0 || t.BuyInCents == 0 {
			return t.FinishPos == recorded.FinishPos && t.TotalPlayers == recorded.TotalPlayers, -1
		}
		return sameTournamentContent(t, recorded), -1
	}
	return false, -1
}

func sameTournamentContent(a, b parsers.Tournament) bool {
	return a.BuyInCents == b.BuyInCents &&
		a.FinishPos == b.FinishPos &&
		a.TotalPlayers == b.TotalPlayers
}

type quietTimer struct{ *time.Timer }
type quietSignal struct {
	Key        string
	Generation *quietTimer
}

func stopQuietTimer(timers map[string]*quietTimer, key string) {
	if timer := timers[key]; timer != nil {
		timer.Stop()
		delete(timers, key)
	}
}

func armQuietTimer(timers map[string]*quietTimer, key string, quietPeriod time.Duration, signals chan<- quietSignal, done <-chan struct{}) {
	stopQuietTimer(timers, key)
	generation := &quietTimer{}
	timers[key] = generation
	generation.Timer = time.AfterFunc(quietPeriod, func() {
		select {
		case signals <- quietSignal{Key: key, Generation: generation}:
		case <-done:
		}
	})
}

func (s *Server) endCashSession(fileID, screenName string, pendingSessions map[string]parsers.CashSession, summaryTimers map[string]*quietTimer, firedAlerts map[string]bool, enforce func(reason string, durationHours int, triggerID string)) error {
	session, ok := pendingSessions[fileID]
	if !ok {
		return nil
	}

	cfg, err := config.LoadConfig(s.configPath)
	if err != nil {
		slog.Error("loading config for cash session end", "err", err)
		return err
	}
	before := sheetsPending(s.store)
	var writeErr error
	if screenName == "" {
		writeErr = s.store.AppendCashSession(session)
	} else {
		hands, err := parsers.CashHands(s.cashHistoryPath(fileID), screenName)
		if err != nil {
			return fmt.Errorf("reading cash hands for %s: %w", fileID, err)
		}
		if len(hands) == 0 {
			return fmt.Errorf("no completed hands remain for %s", fileID)
		}
		_, writeErr = s.store.RecordCashHands(session, hands, cashWorkingName(fileID))
	}
	if err := writeErr; err != nil {
		slog.Error("recording cash session", "err", err,
			"date", session.Date.Format("2006-01-02 15:04"), "stake", session.Stake,
			"session_id", sheets.CashSessionID(session.Date, session.FirstHandID))
		if msg := writeFailureMessage(before, sheetsPending(s.store), err); msg != "" {
			bot.PostAlert(s.alertChan, msg)
		}
		return fmt.Errorf("persisting cash session %s: %w", fileID, err)
	} else if msg := syncedMessage(before, sheetsPending(s.store)); msg != "" {
		bot.PostAlert(s.alertChan, msg)
	}

	delete(pendingSessions, fileID)
	stopQuietTimer(summaryTimers, fileID)
	s.evaluateResult(cfg, &session, nil, session.NetCents, true, firedAlerts, enforce)
	return nil
}

func (s *Server) evaluateResult(cfg *config.Config, cash *parsers.CashSession, tournament *parsers.Tournament, netCents money.Cents, notify bool, firedAlerts map[string]bool, enforce func(string, int, string)) money.Cents {
	evalCfg := *cfg
	evalCfg.Alerts = config.EffectiveAlerts(cfg.Alerts)
	evalCfg.Hardstop = config.EffectiveHardstop(cfg.Hardstop)
	now := s.ClientNow()
	ledger, dayTournaments := fetchEvalData(s.store, &evalCfg, now, true)
	bankrollCents := ledgerBankroll(ledger)
	trigger := automaticEnforcement(&evalCfg, now, cash, tournament, ledger, dayTournaments, enforce)
	var err error
	var buyInCents money.Cents
	var key string
	var mode *string
	if cash != nil {
		err = hardstop.Evaluate(*cash, &evalCfg, ledger, dayTournaments, now, trigger)
		buyInCents, key, mode = cash.BuyInCents, "cash:"+cash.FirstHandID, cfg.Notifications.CashSessionSummary
	} else {
		err = hardstop.EvaluateTournament(*tournament, &evalCfg, ledger, dayTournaments, now, trigger)
		buyInCents, key, mode = tournament.BuyInCents, "tournament:"+tournament.TournamentID, cfg.Notifications.TournamentSummary
	}
	if err != nil {
		slog.Error("evaluating hard stop", "err", err)
	}
	if !notify {
		return bankrollCents
	}
	bot.EvaluateDownswingAlert(buyInCents, &evalCfg, ledger, bankrollCents, now, s.alertChan)
	if cash != nil && config.NotificationEnabled(cfg.Notifications.StakeChange) && !firedAlerts["stake:"+key] {
		if err := bot.EvaluateStakeChangeAlert(*cash, s.store, s.alertChan); err != nil {
			slog.Error("evaluating stake change alert", "err", err, "session_id", cash.FirstHandID)
		} else {
			firedAlerts["stake:"+key] = true
		}
	}
	evaluateOncePerKeyAlerts(&evalCfg, ledger, dayTournaments, bankrollCents, now, s.alertChan, firedAlerts, key)
	if config.ShouldSendSummary(config.SummaryMode(mode), netCents) {
		if cash != nil {
			bot.PostAlert(s.alertChan, cashSessionSummaryText(*cash, bankrollCents))
		} else {
			bot.PostAlert(s.alertChan, tournamentAlertText(*tournament, netCents, bankrollCents))
		}
	}
	return bankrollCents
}

func tournamentAlertText(t parsers.Tournament, netCents, bankrollCents money.Cents) string {
	var msg string
	if t.BaseType() == "spingo" {
		msg = fmt.Sprintf("🎰 Spin & Go\nBuy-in: $%s\nPrize pool: %s\nFinish: %s\nNet: %s\nBankroll: $%s", t.BuyInCents, bot.SpinGoPrizePoolText(t), bot.Ordinal(t.FinishPos), bot.FormatSignedUSD(netCents), bankrollCents)
	} else {
		msg = fmt.Sprintf("🏆 MTT\nBuy-in: $%s\nFinish: %s\nNet: %s\nBankroll: $%s", t.BuyInCents, bot.TournamentFinishText(t), bot.FormatSignedUSD(netCents), bankrollCents)
	}
	if t.ReEntries > 0 {
		msg += fmt.Sprintf("\n🔁 Re-entered %d× — %d entries total for this tournament; buy-in and net cover all of them, updating the earlier alert.", t.ReEntries, t.ReEntries+1)
	}
	return msg
}

func cashSessionSummaryText(session parsers.CashSession, bankrollCents money.Cents) string {
	return fmt.Sprintf("✅ Cash Session\nStake: %s\nNet: %s\nHands: %d\nDuration: %d min\nBankroll: $%s", session.Stake, bot.FormatSignedUSD(session.NetCents), session.Hands, session.DurationMin, bankrollCents)
}

func remainingCashQuiet(quiet time.Duration, lastActivity, now time.Time) time.Duration {
	elapsed := now.Sub(lastActivity)
	if elapsed < 0 {
		elapsed = 0
	}
	return quiet - elapsed
}

func sameCashSnapshot(a, b parsers.CashSession) bool {
	sameDate := a.Date.Equal(b.Date)
	a.Date = time.Time{}
	b.Date = time.Time{}
	return sameDate && a == b
}
