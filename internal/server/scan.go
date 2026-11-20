package server

import (
	"errors"
	"fmt"
	"os"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/transport"
)

type fileScan struct {
	request      storage.Operation
	command      transport.Command
	disconnected <-chan struct{}
	sent         bool
	file         *os.File
	stop         chan struct{}
	results      <-chan scanResult
	result       storage.OperationResult
}

type scanResult struct {
	cash       *parsers.CashSession
	tournament *parsers.TournamentHandStats
	err        error
	done       bool
}

func (s *fileScan) close() {
	if s.stop != nil {
		close(s.stop)
	}
	if s.file != nil {
		s.file.Close()
		os.Remove(s.file.Name())
	}
}

func (s *fileScan) accept(chunk transport.ScanChunk) error {
	if chunk.Error != "" {
		return errors.New(chunk.Error)
	}
	if len(chunk.Bytes) > 0 {
		if s.file == nil {
			f, err := os.CreateTemp("", "poker-tracker-scan-*.txt")
			if err != nil {
				return fmt.Errorf("staging scan file: %w", err)
			}
			s.file = f
		}
		if _, err := s.file.Write(chunk.Bytes); err != nil {
			return fmt.Errorf("staging scan bytes: %w", err)
		}
	}
	if !chunk.Done {
		return nil
	}
	if s.file == nil {
		return fmt.Errorf("the poker PC sent an empty file")
	}
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("closing staged scan: %w", err)
	}
	path := s.file.Name()
	s.file = nil
	s.start(path, chunk.ScreenName, true)
	return nil
}

func (s *fileScan) start(path, screenName string, removeFile bool) {
	results := make(chan scanResult)
	stop := make(chan struct{})
	s.stop, s.results, s.disconnected, s.sent = stop, results, nil, true
	go func() {
		if removeFile {
			defer os.Remove(path)
		}
		send := func(record scanResult) bool {
			select {
			case results <- record:
				return true
			case <-stop:
				return false
			}
		}
		err := parsers.ScanHandHistory(path, screenName, true, func(cash *parsers.CashSession, tournament *parsers.TournamentHandStats, err error) bool {
			return send(scanResult{cash: cash, tournament: tournament, err: err})
		})
		send(scanResult{err: err, done: true})
	}()
}

func (s *fileScan) record(store storage.Store, parsed scanResult) {
	if parsed.done {
		if s.result.Err == nil {
			s.result.Err = parsed.err
		}
		return
	}
	if err := s.result.Scan.RecordHandHistory(store, parsed.cash, parsed.tournament, parsed.err); err != nil && s.result.Err == nil {
		s.result.Err = err
	}
}
