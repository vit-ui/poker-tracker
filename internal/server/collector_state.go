package server

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/vit-ui/self-tracker/internal/hardstop"

	"github.com/vit-ui/self-tracker/internal/config"
)

func (s *Server) observeCollectorLock(lock *config.LockFile, message string, connection <-chan struct{}) {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	s.collectorLock = nil
	if lock != nil {
		copy := *lock
		s.collectorLock = &copy
	}
	s.collectorLockKnown = true
	s.collectorLockError = message
	s.collectorLockConnection = connection
}

// CollectorLock reports observed enforcement state from the current collector,
// independent of the server machine's filesystem or timezone.
func (s *Server) CollectorLock() (*config.LockFile, error) {
	connection := s.link.CollectorDone()
	select {
	case <-connection:
		return nil, fmt.Errorf("collector disconnected")
	default:
	}
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	if !s.collectorLockKnown || s.collectorLockConnection != connection {
		return nil, fmt.Errorf("waiting for collector protection status")
	}
	if s.collectorLockError != "" {
		return nil, fmt.Errorf("collector protection status: %s", s.collectorLockError)
	}
	if s.collectorLock == nil {
		return nil, nil
	}
	copy := *s.collectorLock
	return &copy, nil
}

// LockStatusText reads display state without establishing enforcement confirmation.
func (s *Server) LockStatusText() (string, error) {
	liveLock, liveErr := s.CollectorLock()
	state := config.CollectorObservation{Known: liveErr == nil, Lock: liveLock}
	if liveErr != nil {
		var err error
		state, err = config.LoadCollectorObservation(filepath.Join(filepath.Dir(s.configPath), "collector_observation.json"))
		if err != nil {
			return "", err
		}
	}
	deliveries, err := config.LoadHardstopDeliveries(filepath.Join(filepath.Dir(s.configPath), "hardstop_deliveries.json"))
	if err != nil {
		return "", err
	}
	// Existing confirmation receipts can reconstruct a missing display copy.
	// A newer explicit collector observation (including clear) always wins.
	if !state.Known {
		for _, delivery := range deliveries {
			if delivery.Confirmed && delivery.EffectiveLock != nil && (state.Lock == nil || delivery.EffectiveLock.ExpiresAt.After(state.Lock.ExpiresAt)) {
				copy := *delivery.EffectiveLock
				state.Lock = &copy
			}
		}
		state.Known = state.Lock != nil
	}
	now := s.ClientNow()
	text := ""
	if state.Lock != nil && state.Lock.ExpiresAt.After(now) {
		text = "\n🛑 " + hardstop.LockDetails(*state.Lock, now)
	}
	var pending *config.LockFile
	for _, delivery := range deliveries {
		if !delivery.Confirmed && delivery.Lock.ExpiresAt.After(time.Now()) && (pending == nil || delivery.Lock.ExpiresAt.After(pending.ExpiresAt)) {
			copy := delivery.Lock
			pending = &copy
		}
	}
	if pending != nil {
		text += "\n⏳ Pending PC confirmation: " + hardstop.LockDetails(*pending, now)
	}
	if !state.Known && pending == nil {
		text += "\n⚠️ Collector lock status unknown."
	}
	select {
	case <-s.link.CollectorDone():
		text += "\n⚠️ Poker PC is offline. Showing saved lock status."
	default:
		if liveErr != nil {
			text += "\n⚠️ Live collector protection status unavailable."
		}
	}
	return text, nil
}

func (s *Server) saveCollectorObservation(lock *config.LockFile, message string) error {
	if message != "" {
		return nil
	}
	return config.SaveCollectorObservation(filepath.Join(filepath.Dir(s.configPath), "collector_observation.json"), config.CollectorObservation{Known: true, Lock: lock})
}

// RequestHardstop leaves the bot free to drain alerts while the worker accepts
// the request. The journal already holds successful bot-side saves; failed
// saves retain this in-memory request until the worker accepts it or exits.
func (s *Server) RequestHardstop(lock config.LockFile) {
	go func() {
		select {
		case s.hardstopRequests <- lock:
		case <-s.stopped:
		}
	}()
}
