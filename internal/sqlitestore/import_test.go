package sqlitestore

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestCashHandOverlapAndTransfer(t *testing.T) {
	at := time.Date(2026, 7, 3, 16, 30, 20, 0, time.UTC)
	hands := make([]parsers.CashHand, 10)
	for i := range hands {
		hands[i] = parsers.CashHand{ID: fmt.Sprint(900000000 + i), At: at.Add(time.Duration(i) * time.Minute),
			BigBlindCents: 2, NetCents: money.Cents(i - 2), RakeCents: 1, VPIP: i%2 == 0, PFR: i%3 == 0}
	}
	for _, firstSource := range []string{"live", ""} {
		t.Run(fmt.Sprintf("live first %t", firstSource != ""), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cash.db")
			db, err := NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { db.Close() }()
			record := func(sample []parsers.CashHand, source string, want storage.CashWrite) {
				t.Helper()
				session, err := parsers.SessionFromCashHands("NL2", sample)
				if err != nil {
					t.Fatal(err)
				}
				got, err := db.RecordCashHands(session, sample, source)
				if err != nil || got != want {
					t.Fatalf("recorded %+v (%v), want %+v", got, err, want)
				}
			}
			record(hands[:7], firstSource, storage.CashWrite{New: 7})
			if err := db.AppendLedgerEntry("deposit", 100, at, "money:import:deposit"); err != nil {
				t.Fatal(err)
			}
			secondSource := "live"
			if firstSource != "" {
				secondSource = ""
			}
			record(hands[1:], secondSource, storage.CashWrite{New: 3, Recorded: 6})
			record(hands, "", storage.CashWrite{Recorded: 10})
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			rewritten := append([]parsers.CashHand(nil), hands...)
			rewritten[2].NetCents = -25
			record(rewritten, "live", storage.CashWrite{Recorded: 10})
			dest, err := NewStore(filepath.Join(t.TempDir(), "migrated.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer dest.Close()
			if _, err := storage.Migrate(db, dest); err != nil {
				t.Fatal(err)
			}
			snap, err := dest.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.CashSessions) != 1 || len(snap.Ledger) != 2 {
				t.Fatalf("snapshot row counts: %+v", snap)
			}
			cash := snap.CashSessions[0]
			if cash.Hands != 10 || len(cash.HandDetails) != 10 || cash.NetCents != 0 || cash.RakeCents != 10 ||
				cash.VPIP != 50 || cash.PFR != 40 || cash.BB100 != 0 || cash.DurationMin != 9 ||
				snap.Ledger[0].RunningTotalCents != 0 || snap.Ledger[1].RunningTotalCents != 100 {
				t.Fatalf("corrected cash and ledger: %+v %+v", cash, snap.Ledger)
			}
		})
	}
}

func TestTournamentImportUsesLargerStatsAndIndependentBounty(t *testing.T) {
	for _, test := range []struct {
		name               string
		incoming, want     int
		bounty, wantBounty money.Cents
		wantVPIP           float64
	}{
		{"smaller sample with new bounty", 3, 10, 175, 175, 20},
		{"equal sample with lower bounty", 10, 10, 50, 125, 20},
		{"larger sample with lower bounty", 12, 12, 50, 125, 60},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := NewStore(filepath.Join(t.TempDir(), "tournaments.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			const id = "9000000311"
			if err := db.UpdateTournamentHandStats(id, 20, 10, 10, 125, true); err != nil {
				t.Fatal(err)
			}
			if _, err := db.AppendTournament(parsers.Tournament{Date: time.Now().UTC(), Type: "mtt", TournamentID: id, BuyInCents: 100, NetCents: -100}); err != nil {
				t.Fatal(err)
			}
			if err := db.ScanTournamentHandStats(parsers.TournamentHandStats{TournamentID: id, Hands: test.incoming, VPIP: 60, BountyCents: test.bounty}); err != nil {
				t.Fatal(err)
			}
			snap, err := db.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			row := snap.Tournaments[0]
			if *row.Hands != test.want || *row.VPIP != test.wantVPIP || row.BountyWonCents != test.wantBounty ||
				row.NetCents != test.wantBounty-100 || snap.Ledger[0].RunningTotalCents != row.NetCents {
				t.Fatalf("imported tournament and ledger: %+v %+v", row, snap.Ledger)
			}
			if err := db.UpdateTournamentHandStats(id, 15, 7, 8, 75, true); err != nil {
				t.Fatal(err)
			}
			snap, err = db.ExportSnapshot()
			if err != nil || *snap.Tournaments[0].Hands != 8 || snap.Ledger[0].RunningTotalCents != -25 {
				t.Fatalf("live correction: %+v %v", snap, err)
			}
		})
	}
}

func TestLegacyCashTotalsStayWhenOwnershipIsUncertain(t *testing.T) {
	at := time.Date(2026, 7, 3, 16, 30, 20, 0, time.UTC)
	full := []parsers.CashHand{
		{ID: "900000001", At: at, NetCents: -2, BigBlindCents: 2},
		{ID: "900000002", At: at.Add(time.Minute), NetCents: 9, BigBlindCents: 2},
	}
	extended := append(append([]parsers.CashHand(nil), full...), parsers.CashHand{
		ID: "900000003", At: at.Add(2 * time.Minute), NetCents: -12, BigBlindCents: 2,
	})
	for _, test := range []struct {
		name     string
		sample   []parsers.CashHand
		source   string
		attached int
		newHands int
	}{
		{"matching live history", full, "table", 2, 0},
		{"shorter live history", full[:1], "table", 0, 0},
		{"matching import", full, "", 0, 0},
		{"larger import with a proven prefix", extended, "", 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := NewStore(filepath.Join(t.TempDir(), "legacy.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			saved, err := parsers.SessionFromCashHands("NL2", full)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AppendCashSession(saved); err != nil {
				t.Fatal(err)
			}
			incoming, err := parsers.SessionFromCashHands("NL2", test.sample)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := db.RecordCashHands(incoming, test.sample, test.source); err != nil || got.New != test.newHands {
				t.Fatalf("legacy write: %+v %v", got, err)
			}
			if got, err := db.RecordCashHands(incoming, test.sample, test.source); err != nil || got.New != 0 {
				t.Fatalf("legacy replay: %+v %v", got, err)
			}
			snap, err := db.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.CashSessions) != 1+test.newHands || len(snap.Ledger) != 1+test.newHands {
				t.Fatalf("legacy row counts: %+v", snap)
			}
			row := snap.CashSessions[0]
			if row.SessionID != cashSessionID(saved.Date, saved.FirstHandID) || row.NetCents != 7 ||
				row.Hands != 2 || len(row.HandDetails) != test.attached || snap.Ledger[0].RunningTotalCents != 7 {
				t.Fatalf("legacy state: %+v %+v", row, snap.Ledger)
			}
			if test.newHands > 0 && (snap.CashSessions[1].NetCents != -12 || snap.Ledger[1].RunningTotalCents != -5) {
				t.Fatalf("new hands after legacy row: %+v", snap)
			}
		})
	}
}
