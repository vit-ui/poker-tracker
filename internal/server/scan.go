package server

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

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
	tournaments  tournamentScan
}

type scanResult struct {
	cash       *parsers.CashSession
	hands      []parsers.CashHand
	tournament *parsers.TournamentHandStats
	summary    *parsers.Tournament
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
		err := parsers.ScanHistoryFile(path, screenName, func(cash *parsers.CashSession, hands []parsers.CashHand, tournament *parsers.TournamentHandStats, err error) bool {
			return send(scanResult{cash: cash, hands: hands, tournament: tournament, err: err})
		}, func(summary parsers.Tournament, err error) bool {
			if err != nil {
				return send(scanResult{err: err})
			}
			return send(scanResult{summary: &summary})
		})
		send(scanResult{err: err, done: true})
	}()
}

func (s *fileScan) record(results *tournamentResults, parsed scanResult) {
	var err error
	switch {
	case parsed.done:
		err = errors.Join(parsed.err, s.tournaments.finish(results, &s.result.Scan))
	case parsed.summary != nil:
		err = s.tournaments.summary(results, &s.result.Scan, *parsed.summary)
	default:
		err = s.tournaments.record(results.server.store, &s.result.Scan, parsed)
	}
	if err != nil && s.result.Err == nil {
		s.result.Err = err
	}
}

type tournamentScan struct {
	candidates map[string]parsers.TournamentHandStats
	summaries  map[string]bool
}

func (s *tournamentScan) record(store storage.Store, tally *storage.ScanTally, parsed scanResult) error {
	err := tally.RecordHandHistory(store, parsed.cash, parsed.hands, parsed.tournament, parsed.err)
	if err == nil && parsed.tournament != nil {
		if s.candidates == nil {
			s.candidates = make(map[string]parsers.TournamentHandStats)
		}
		stats := *parsed.tournament
		if prior, found := s.candidates[stats.TournamentID]; !found || compareHandIDs(stats.LastObservedHandID(), prior.LastObservedHandID()) >= 0 {
			s.candidates[stats.TournamentID] = stats
		}
	}
	return err
}

func (s *tournamentScan) summary(results *tournamentResults, tally *storage.ScanTally, t parsers.Tournament) error {
	if s.summaries == nil {
		s.summaries = make(map[string]bool)
	}
	if validateOfficialSummary(t) == nil {
		s.summaries[t.TournamentID] = true
	}
	posted, skipped, err := results.official(t, true)
	switch {
	case err != nil:
		tally.Failed++
	case posted:
		tally.Tournaments++
	case skipped:
		tally.Skipped++
	}
	return err
}

func (s *tournamentScan) finish(results *tournamentResults, tally *storage.ScanTally) error {
	ids := make([]string, 0, len(s.candidates))
	for id := range s.candidates {
		if !s.summaries[id] && s.candidates[id].Finish != nil {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b string) int {
		if order := s.candidates[a].Finish.At.Compare(s.candidates[b].Finish.At); order != 0 {
			return order
		}
		return strings.Compare(a, b)
	})
	now := time.Now()
	clientNow := storage.AsStoredClock(results.server.ClientNow())
	var combined error
	for _, id := range ids {
		stats := s.candidates[id]
		activity := now.Add(min(0, stats.LastActivity.Sub(clientNow)))
		posted, review, err := results.history(stats, activity, true)
		switch {
		case err != nil:
			tally.Failed++
			combined = errors.Join(combined, err)
		case posted:
			tally.Tournaments++
		case review:
			tally.MissingResults++
		}
	}
	return combined
}
