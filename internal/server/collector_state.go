package server

import (
	"fmt"

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
