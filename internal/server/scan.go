package server

import (
	"fmt"
	"os"

	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

type fileScan struct {
	request      storage.Operation
	command      transport.Command
	disconnected <-chan struct{}
	sent         bool
	file         *os.File
}

func (s *fileScan) close() {
	if s.file != nil {
		s.file.Close()
		os.Remove(s.file.Name())
	}
}

func (s *fileScan) accept(store storage.Store, chunk transport.ScanChunk) (storage.OperationResult, bool) {
	if chunk.Error != "" {
		return storage.OperationResult{Err: fmt.Errorf("%s", chunk.Error)}, true
	}
	if len(chunk.Bytes) > 0 {
		if s.file == nil {
			f, err := os.CreateTemp("", "poker-tracker-scan-*.txt")
			if err != nil {
				return storage.OperationResult{Err: fmt.Errorf("staging scan file: %w", err)}, true
			}
			s.file = f
		}
		if _, err := s.file.Write(chunk.Bytes); err != nil {
			return storage.OperationResult{Err: fmt.Errorf("staging scan bytes: %w", err)}, true
		}
	}
	if !chunk.Done {
		return storage.OperationResult{}, false
	}
	if s.file == nil {
		return storage.OperationResult{Err: fmt.Errorf("the poker PC sent an empty file")}, true
	}
	return scanHandFile(store, s.file.Name(), chunk.ScreenName), true
}

func scanHandFile(store storage.Store, path, screenName string) storage.OperationResult {
	var result storage.OperationResult
	if screenName == "" {
		result.Err = fmt.Errorf("set the PokerStars screen name before scanning")
		return result
	}
	result.Err = result.Scan.HandHistory(store, path, screenName, true)
	return result
}
