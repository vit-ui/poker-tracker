package storage

import (
	"math"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

func migrationFixture() Snapshot {
	date := time.Date(2026, 7, 10, 20, 0, 30, 0, time.UTC)
	vpip, pfr, hands := 20.0, 10.0, 10
	return Snapshot{
		CashSessions: []CashSessionRecord{{SessionID: "cash-a", CashSession: parsers.CashSession{Date: date, Stake: "NL5", Hands: 10, DurationMin: 2, NetUSD: -5, RakeUSD: 0.1, BB100: -1000, VPIP: 20, PFR: 10}}},
		Tournaments: []TournamentRecord{
			{Tournament: parsers.Tournament{Date: date, Type: "mtt-ko", TournamentID: "T-COMPLETE", BuyInUSD: 3, NetUSD: -2, FinishPos: 5, TotalPlayers: 10, VPIP: &vpip, PFR: &pfr, Hands: &hands}, BountyWonUSD: 1, PrizeWonUSD: 1},
			{Tournament: parsers.Tournament{Type: "ko", TournamentID: "T-PARTIAL"}, BountyWonUSD: 2.5},
		},
		Ledger: []LedgerEntry{
			{Date: date, Type: "deposit", AmountUSD: 100, RunningTotal: 100},
			{Date: date, Type: "cash_session", SessionID: "cash-a", AmountUSD: -5, RunningTotal: 95},
			{Date: date, Type: "tournament", SessionID: "T-COMPLETE", AmountUSD: -2, RunningTotal: 93},
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
		{"destination amount", func(s *Snapshot) { s.Ledger[0].AmountUSD = 90 }, time.Second, true},
		{"balanced amount corruption", func(s *Snapshot) { s.Ledger[0].AmountUSD = 90; s.Ledger[1].AmountUSD = 5 }, time.Second, true},
		{"running total", func(s *Snapshot) { s.Ledger[1].RunningTotal = 96 }, time.Second, true},
		{"legacy identity changed", func(s *Snapshot) { s.Ledger[0].SessionID = "invented" }, time.Second, true},
		{"cash identity", func(s *Snapshot) { s.CashSessions[0].SessionID = "other" }, time.Second, true},
		{"cash rake", func(s *Snapshot) { s.CashSessions[0].RakeUSD = 0.2 }, time.Second, true},
		{"cash count", func(s *Snapshot) { s.CashSessions = nil }, time.Second, true},
		{"partial identity", func(s *Snapshot) { s.Tournaments[1].TournamentID = "other" }, time.Second, true},
		{"partial bounty", func(s *Snapshot) { s.Tournaments[1].BountyWonUSD = 3 }, time.Second, true},
		{"folded bounty", func(s *Snapshot) { s.Tournaments[0].BountyWonUSD = 2 }, time.Second, true},
		{"prize won", func(s *Snapshot) { s.Tournaments[0].PrizeWonUSD = 2 }, time.Second, true},
		{"null multiplier versus zero", func(s *Snapshot) { v := 0.0; s.Tournaments[0].Multiplier = &v }, time.Second, true},
		{"null stats versus zero", func(s *Snapshot) { v := 0.0; s.Tournaments[1].VPIP = &v }, time.Second, true},
		{"hand count changed", func(s *Snapshot) { v := 11; s.Tournaments[0].Hands = &v }, time.Second, true},
		{"stat round-trip precision", func(s *Snapshot) { v := 20.0 + 1e-12; s.Tournaments[0].VPIP = &v }, time.Second, false},
		{"null hands versus zero", func(s *Snapshot) { v := 0; s.Tournaments[1].Hands = &v }, time.Second, true},
		{"tournament count", func(s *Snapshot) { s.Tournaments = s.Tournaments[:1] }, time.Second, true},
		{"NaN", func(s *Snapshot) { s.Ledger[0].AmountUSD = math.NaN() }, time.Second, true},
		{"infinity", func(s *Snapshot) { s.Ledger[2].RunningTotal = math.Inf(1) }, time.Second, true},
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
		name    string
		amounts []float64
		want    float64
	}{
		{"empty", nil, 0}, {"movements and losses", []float64{100, -5, -2}, 93}, {"withdrawal", []float64{20, -30}, -10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rows []LedgerEntry
			for _, amount := range tc.amounts {
				rows = append(rows, LedgerEntry{AmountUSD: amount})
			}
			if got := ledgerSum(rows); got != tc.want {
				t.Fatalf("sum = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSameNumber(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b float64
		want bool
	}{
		{"equal", 93, 93, true}, {"within half cent", 0, 0.004, true}, {"half cent", 0, 0.005, true},
		{"outside half cent", 0, 0.006, false}, {"negative difference", -5, -5.01, false},
		{"NaN", math.NaN(), 0, false}, {"same infinity", math.Inf(1), math.Inf(1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameNumber(tc.a, tc.b, bankrollEpsilon); got != tc.want {
				t.Fatalf("sameNumber = %v, want %v", got, tc.want)
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
