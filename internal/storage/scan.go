package storage

import (
	"fmt"
	"log/slog"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

// ScanTally counts recorded rows and omissions from a silent scan.
type ScanTally struct {
	Cash, Tournaments, TournamentHistories, Skipped, Failed int
	NewCashHands, RecordedCashHands                         int
}

func (t ScanTally) String() string {
	msg := fmt.Sprintf("📥 Scan complete — recorded %d cash session(s) and %d tournament(s).", t.Cash, t.Tournaments)
	msg += fmt.Sprintf("\nCash hands: %d new, %d already recorded.", t.NewCashHands, t.RecordedCashHands)
	if t.TournamentHistories > 0 {
		msg += fmt.Sprintf("\nUpdated hand history for %d tournament(s). Summaries supply final results.", t.TournamentHistories)
	}
	if t.Skipped > 0 {
		msg += fmt.Sprintf("\nSkipped %d tournament(s) that need a ticket-vs-cash or buy-in answer.", t.Skipped)
	}
	if t.Failed > 0 {
		msg += fmt.Sprintf("\nCould not record %d item(s). See the tracker log for details.", t.Failed)
	}
	return msg
}

func (t *ScanTally) HandHistory(store Store, path, screenName string) error {
	var firstErr error
	err := parsers.ScanHandHistory(path, screenName, func(session *parsers.CashSession, hands []parsers.CashHand, tournament *parsers.TournamentHandStats, err error) bool {
		if err := t.RecordHandHistory(store, session, hands, tournament, err); err != nil && firstErr == nil {
			firstErr = err
		}
		return true
	})
	if err != nil {
		if err := t.RecordHandHistory(store, nil, nil, nil, err); firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// RecordHandHistory applies one parsed group on the storage owner.
func (t *ScanTally) RecordHandHistory(store Store, session *parsers.CashSession, hands []parsers.CashHand, tournament *parsers.TournamentHandStats, err error) error {
	if err == nil {
		if session != nil {
			var counts CashWrite
			counts, err = store.RecordCashHands(*session, hands, "")
			if err == nil {
				if counts.New > 0 {
					t.Cash++
				}
				t.NewCashHands += counts.New
				t.RecordedCashHands += counts.Recorded
			}
		} else {
			err = store.ScanTournamentHandStats(*tournament)
			if err == nil {
				t.TournamentHistories++
			}
		}
	}
	if err != nil {
		t.Failed++
		slog.Error("scanning hand history", "err", err)
	}
	return err
}

func (t *ScanTally) TournamentSummary(store Store, path, screenName string) error {
	tournament, err := parsers.ParseTournamentFile(path, screenName)
	if err == nil {
		if tournament.LikelySatellite || tournament.BuyInCents == 0 {
			t.Skipped++
			return nil
		}
		_, err = store.AppendTournament(tournament)
	}
	if err != nil {
		t.Failed++
		slog.Error("scanning tournament summary", "err", err)
		return err
	}
	t.Tournaments++
	return nil
}
