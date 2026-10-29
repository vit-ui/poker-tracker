package sqlitestore

import (
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestConcurrentMoneyMovements(t *testing.T) {
	for _, tc := range []struct {
		name    string
		amounts []float64
		count   int
		want    float64
	}{
		{"deposits", []float64{1}, 32, 32},
		{"deposits and withdrawals", []float64{2, -0.5}, 32, 48},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			start := make(chan struct{})
			errors := make(chan error, tc.count*len(tc.amounts))
			var writers sync.WaitGroup
			for range tc.count {
				for _, amount := range tc.amounts {
					writers.Go(func() {
						<-start
						kind := "deposit"
						if amount < 0 {
							kind = "withdrawal"
						}
						errors <- s.AppendLedgerEntry(kind, amount, time.Now())
					})
				}
			}
			close(start)
			writers.Wait()
			close(errors)
			for err := range errors {
				if err != nil {
					t.Fatal(err)
				}
			}
			rows, err := s.GetLedgerEntriesSince(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.count*len(tc.amounts) {
				t.Fatalf("ledger rows = %d", len(rows))
			}
			var sum float64
			for i, row := range rows {
				sum += row.AmountUSD
				if !closeEnough(row.RunningTotal, sum) {
					t.Fatalf("row %d total = %v, amount sum = %v", i, row.RunningTotal, sum)
				}
			}
			bankroll, err := s.GetCurrentBankroll()
			if err != nil {
				t.Fatal(err)
			}
			if !closeEnough(sum, tc.want) || !closeEnough(bankroll, tc.want) {
				t.Fatalf("sum = %v, bankroll = %v, want %v", sum, bankroll, tc.want)
			}
		})
	}
}

func TestNonFiniteWritesDoNotChangeStore(t *testing.T) {
	for _, invalid := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, tc := range []struct {
			name  string
			write func(*Store) error
		}{
			{"movement", func(s *Store) error { return s.AppendLedgerEntry("deposit", invalid, time.Now()) }},
			{"cash", func(s *Store) error { return s.AppendCashSession(parsers.CashSession{NetUSD: invalid}) }},
			{"tournament", func(s *Store) error {
				_, err := s.AppendTournament(parsers.Tournament{TournamentID: "T-FINITE", BuyInUSD: invalid})
				return err
			}},
			{"stats", func(s *Store) error { return s.UpdateTournamentHandStats("T-FINITE", 20, 10, 10, invalid, true) }},
			{"snapshot", func(s *Store) error {
				return s.ImportSnapshot(storage.Snapshot{Ledger: []storage.LedgerEntry{{AmountUSD: invalid}}})
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := newTestStore(t)
				if err := tc.write(s); err == nil {
					t.Fatal("accepted non-finite input")
				}
				snapshot, err := s.ExportSnapshot()
				if err != nil {
					t.Fatal(err)
				}
				if !snapshot.IsEmpty() {
					t.Fatalf("rejected write changed storage: %+v", snapshot)
				}
			})
		}
	}
}

func TestOverlappingCorrectionsAndMovements(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	cash := buildCashSession(date, "cash-a", 20, -5)
	tournament := parsers.Tournament{Date: date, Type: "mtt", TournamentID: "T-ATOMIC-1", BuyInUSD: 1, NetUSD: -1}
	if err := s.AppendLedgerEntry("deposit", 100, date); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendCashSession(cash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendTournament(tournament); err != nil {
		t.Fatal(err)
	}
	cash.NetUSD = -7
	tournament.BuyInUSD, tournament.NetUSD = 2, -2
	operations := []func() error{
		func() error { return s.AppendLedgerEntry("deposit", 10, date) },
		func() error { return s.AppendLedgerEntry("withdrawal", -20, date) },
		func() error { return s.AppendCashSession(cash) },
		func() error { _, err := s.AppendTournament(tournament); return err },
		func() error { return s.UpdateTournamentHandStats(tournament.TournamentID, 20, 10, 20, 1, true) },
		func() error { return s.AppendCashSession(buildCashSession(date, "cash-b", 30, 3)) },
	}
	start := make(chan struct{})
	errors := make(chan error, len(operations))
	var writers sync.WaitGroup
	for _, operation := range operations {
		writers.Go(func() { <-start; errors <- operation() })
	}
	close(start)
	writers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	snap, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Ledger) != 6 || len(snap.CashSessions) != 2 || len(snap.Tournaments) != 1 {
		t.Fatalf("unexpected record counts: %+v", snap)
	}
	var total float64
	for i, row := range snap.Ledger {
		total += row.AmountUSD
		if !closeEnough(row.RunningTotal, total) {
			t.Fatalf("row %d total = %v, want %v", i, row.RunningTotal, total)
		}
	}
	// 100 + 10 - 20 - 7 + (-2 + 1 bounty) + 3 = 85.
	if !closeEnough(total, 85) {
		t.Fatalf("final bankroll = %v, want 85", total)
	}
	if row := snap.Tournaments[0]; row.NetUSD != -1 || row.BountyWonUSD != 1 || row.PrizeWonUSD != 1 {
		t.Fatalf("bounty/correction lost: %+v", row)
	}
}

func TestMutationsRollBackWithLedgerFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update bool
		write  func(*Store, parsers.CashSession, parsers.Tournament) error
	}{
		{"cash insert", false, func(s *Store, cash parsers.CashSession, _ parsers.Tournament) error {
			cash.FirstHandID = "new-cash"
			return s.AppendCashSession(cash)
		}},
		{"tournament insert", false, func(s *Store, _ parsers.CashSession, tournament parsers.Tournament) error {
			tournament.TournamentID = "T-ATOMIC-NEW"
			_, err := s.AppendTournament(tournament)
			return err
		}},
		{"cash correction", true, func(s *Store, cash parsers.CashSession, _ parsers.Tournament) error {
			cash.NetUSD = -7
			return s.AppendCashSession(cash)
		}},
		{"tournament correction", true, func(s *Store, _ parsers.CashSession, tournament parsers.Tournament) error {
			tournament.BuyInUSD, tournament.NetUSD = 2, -2
			_, err := s.AppendTournament(tournament)
			return err
		}},
		{"bounty correction", true, func(s *Store, _ parsers.CashSession, tournament parsers.Tournament) error {
			return s.UpdateTournamentHandStats(tournament.TournamentID, 20, 10, 20, 1, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			date := mustTime(t, "2026-07-10T20:00:00Z")
			cash := buildCashSession(date, "cash-original", 20, -5)
			tournament := parsers.Tournament{Date: date, Type: "mtt", TournamentID: "T-ATOMIC-ORIGINAL", BuyInUSD: 1, NetUSD: -1}
			if err := s.AppendLedgerEntry("deposit", 100, date); err != nil {
				t.Fatal(err)
			}
			if err := s.AppendCashSession(cash); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendTournament(tournament); err != nil {
				t.Fatal(err)
			}
			if err := s.AppendLedgerEntry("withdrawal", -10, date); err != nil {
				t.Fatal(err)
			}
			before, err := s.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER fail_ledger BEFORE INSERT ON bankroll_ledger BEGIN SELECT RAISE(ABORT, 'injected ledger failure'); END`
			if tc.update {
				trigger = `CREATE TRIGGER fail_ledger BEFORE UPDATE ON bankroll_ledger WHEN OLD.id = (SELECT MAX(id) FROM bankroll_ledger) BEGIN SELECT RAISE(ABORT, 'injected cascade failure'); END`
			}
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if err := tc.write(s, cash, tournament); err == nil {
				t.Fatal("expected ledger failure")
			}
			after, err := s.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("partial mutation survived rollback:\nbefore: %+v\nafter: %+v", before, after)
			}
		})
	}
}
