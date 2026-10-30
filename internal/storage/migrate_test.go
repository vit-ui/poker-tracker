package storage

import (
	"math"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
)

func migrationFixture() Snapshot {
	date := time.Date(2026, 7, 10, 20, 0, 30, 0, time.UTC)
	vpip, pfr, hands := 20.0, 10.0, 10
	return Snapshot{
		CashSessions: []CashSessionRecord{{SessionID: "cash-a", CashSession: parsers.CashSession{Date: date, Stake: "NL5", Hands: 10, DurationMin: 2, NetCents: -500, RakeCents: 10, BB100: -1000, VPIP: 20, PFR: 10}}},
		Tournaments: []TournamentRecord{
			{Tournament: parsers.Tournament{Date: date, Type: "mtt-ko", TournamentID: "T-COMPLETE", BuyInCents: 300, NetCents: -200, FinishPos: 5, TotalPlayers: 10, VPIP: &vpip, PFR: &pfr, Hands: &hands}, BountyWonCents: 100, PrizeWonCents: 100},
			{Tournament: parsers.Tournament{Type: "ko", TournamentID: "T-PARTIAL"}, BountyWonCents: 250},
		},
		Ledger: []LedgerEntry{
			{Date: date, Type: "deposit", AmountCents: 10000, RunningTotalCents: 10000},
			{Date: date, Type: "cash_session", SessionID: "cash-a", AmountCents: -500, RunningTotalCents: 9500},
			{Date: date, Type: "tournament", SessionID: "T-COMPLETE", AmountCents: -200, RunningTotalCents: 9300},
		},
	}
}

func TestVerifySnapshot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*Snapshot)
		precision time.Duration
		wantError bool
	}{
		{"identical", func(*Snapshot) {}, time.Second, false},
		{"one-cent corruption", func(s *Snapshot) { s.Ledger[0].AmountCents++ }, time.Second, true},
		{"balanced one-cent corruption", func(s *Snapshot) { s.Ledger[0].AmountCents++; s.Ledger[1].AmountCents-- }, time.Second, true},
		{"destination amount", func(s *Snapshot) { s.Ledger[0].AmountCents = 9000 }, time.Second, true},
		{"balanced amount corruption", func(s *Snapshot) { s.Ledger[0].AmountCents = 9000; s.Ledger[1].AmountCents = 500 }, time.Second, true},
		{"running total", func(s *Snapshot) { s.Ledger[1].RunningTotalCents = 9600 }, time.Second, true},
		{"legacy identity changed", func(s *Snapshot) { s.Ledger[0].SessionID = "invented" }, time.Second, true},
		{"cash identity", func(s *Snapshot) { s.CashSessions[0].SessionID = "other" }, time.Second, true},
		{"cash rake", func(s *Snapshot) { s.CashSessions[0].RakeCents = 20 }, time.Second, true},
		{"cash count", func(s *Snapshot) { s.CashSessions = nil }, time.Second, true},
		{"partial identity", func(s *Snapshot) { s.Tournaments[1].TournamentID = "other" }, time.Second, true},
		{"partial bounty", func(s *Snapshot) { s.Tournaments[1].BountyWonCents = 300 }, time.Second, true},
		{"folded bounty", func(s *Snapshot) { s.Tournaments[0].BountyWonCents = 200 }, time.Second, true},
		{"prize won", func(s *Snapshot) { s.Tournaments[0].PrizeWonCents = 200 }, time.Second, true},
		{"null multiplier versus zero", func(s *Snapshot) { v := 0.0; s.Tournaments[0].Multiplier = &v }, time.Second, true},
		{"null stats versus zero", func(s *Snapshot) { v := 0.0; s.Tournaments[1].VPIP = &v }, time.Second, true},
		{"hand count changed", func(s *Snapshot) { v := 11; s.Tournaments[0].Hands = &v }, time.Second, true},
		{"stat round-trip precision", func(s *Snapshot) { v := 20.0 + 1e-12; s.Tournaments[0].VPIP = &v }, time.Second, false},
		{"null hands versus zero", func(s *Snapshot) { v := 0; s.Tournaments[1].Hands = &v }, time.Second, true},
		{"tournament count", func(s *Snapshot) { s.Tournaments = s.Tournaments[:1] }, time.Second, true},
		{"NaN", func(s *Snapshot) { v := math.NaN(); s.Tournaments[0].VPIP = &v }, time.Second, true},
		{"infinity", func(s *Snapshot) { v := math.Inf(1); s.Tournaments[0].PFR = &v }, time.Second, true},
		{"stats NaN", func(s *Snapshot) { v := math.NaN(); s.Tournaments[0].VPIP = &v }, time.Second, true},
		{"Sheets minute precision", func(s *Snapshot) { s.Ledger[0].Date = s.Ledger[0].Date.Truncate(time.Minute) }, time.Minute, false},
		{"SQLite retains seconds", func(s *Snapshot) { s.Ledger[0].Date = s.Ledger[0].Date.Truncate(time.Minute) }, time.Second, true},
		{"wrong date", func(s *Snapshot) { s.Ledger[0].Date = s.Ledger[0].Date.Add(time.Hour) }, time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, got := migrationFixture(), migrationFixture()
			tc.change(&got)
			if err := VerifySnapshot(want, got, tc.precision); (err != nil) != tc.wantError {
				t.Fatalf("VerifySnapshot error = %v, wantError %v", err, tc.wantError)
			}
		})
	}
}

func TestLedgerSum(t *testing.T) {
	for _, tc := range []struct {
		name         string
		amountsCents []money.Cents
		wantCents    money.Cents
	}{
		{"empty", nil, 0}, {"movements and losses", []money.Cents{10000, -500, -200}, 9300}, {"withdrawal", []money.Cents{2000, -3000}, -1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rows []LedgerEntry
			for _, amountCents := range tc.amountsCents {
				rows = append(rows, LedgerEntry{AmountCents: amountCents})
			}
			if gotCents, err := ledgerSum(rows); err != nil || gotCents != tc.wantCents {
				t.Fatalf("sum = %v, want %v", gotCents, tc.wantCents)
			}
		})
	}
}

func TestValidateNumbers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		values    []float64
		wantError bool
	}{
		{"empty", nil, false}, {"finite signed values", []float64{0, -5, 100}, false},
		{"NaN", []float64{math.NaN()}, true}, {"positive infinity", []float64{math.Inf(1)}, true},
		{"negative infinity", []float64{math.Inf(-1)}, true}, {"later invalid value", []float64{1, math.Inf(1)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateNumbers(tc.values...); (err != nil) != tc.wantError {
				t.Fatalf("ValidateNumbers error = %v, wantError %v", err, tc.wantError)
			}
		})
	}
}
