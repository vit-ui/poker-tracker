package sqlitestore

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestCentsWritesRejectOverflowWithoutChangingRecords(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  func(*Store) error
		write func(*Store) error
	}{
		{
			"deposit",
			func(s *Store) error {
				return s.AppendLedgerEntry("deposit", money.Cents(math.MaxInt64), time.Now(), "money:seed")
			},
			func(s *Store) error { return s.AppendLedgerEntry("deposit", 1, time.Now(), "money:overflow") },
		},
		{
			"correction cascades past a later deposit",
			func(s *Store) error {
				if err := s.AppendCashSession(parsers.CashSession{Date: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC), FirstHandID: "overflow-table", NetCents: -1}); err != nil {
					return err
				}
				return s.AppendLedgerEntry("deposit", money.Cents(math.MaxInt64), time.Now(), "money:seed")
			},
			func(s *Store) error {
				return s.AppendCashSession(parsers.CashSession{Date: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC), FirstHandID: "overflow-table", NetCents: 1})
			},
		},
		{
			"bounty folding",
			func(s *Store) error { return s.UpdateTournamentHandStats("overflow-tournament", 20, 10, 5, 1, true) },
			func(s *Store) error {
				_, err := s.AppendTournament(parsers.Tournament{Date: time.Now(), TournamentID: "overflow-tournament", NetCents: money.Cents(math.MaxInt64)})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			if err := tc.seed(store); err != nil {
				t.Fatal(err)
			}
			before, err := store.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.write(store); !errors.Is(err, money.ErrOverflow) {
				t.Fatalf("write error = %v; want overflow", err)
			}
			after, err := store.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.VerifySnapshot(before, after, time.Second); err != nil {
				t.Fatalf("rejected write changed storage: %v", err)
			}
		})
	}
}
