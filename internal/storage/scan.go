package storage

import (
	"fmt"
	"log/slog"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

// ScanTally counts recorded rows and omissions from a silent scan.
type ScanTally struct {
	Cash, Tournaments, TournamentHistories, Skipped, Failed int
	NewCashHands, RecordedCashHands, MissingResults         int
}

const ScanReportPrefix = "📥 Scan complete"
const ScanReviewLabel = "Review missing results"

func (t ScanTally) String() string {
	msg := fmt.Sprintf(ScanReportPrefix+" — recorded %d cash session(s) and %d tournament(s).", t.Cash, t.Tournaments)
	msg += fmt.Sprintf("\nCash hands: %d new, %d already recorded.", t.NewCashHands, t.RecordedCashHands)
	if t.TournamentHistories > 0 {
		msg += fmt.Sprintf("\nUpdated hand history for %d tournament(s).", t.TournamentHistories)
	}
	if t.Skipped > 0 {
		msg += fmt.Sprintf("\nSkipped %d tournament(s) that need a ticket-vs-cash or buy-in answer.", t.Skipped)
	}
	if t.MissingResults > 0 {
		msg += fmt.Sprintf("\n%s (%d awaiting confirmation).", ScanReviewLabel, t.MissingResults)
	}
	if t.Failed > 0 {
		msg += fmt.Sprintf("\nCould not record %d item(s). See the tracker log for details.", t.Failed)
	}
	return msg
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
