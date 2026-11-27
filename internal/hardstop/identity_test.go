package hardstop

import (
	"slices"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestAutomaticTriggerIdentityTracksFinancialBasis(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 10, 0, time.UTC)
	threshold := 1.0
	count := 1
	cfg := &config.Config{}
	cfg.Hardstop.SessionLossBuyins = &threshold
	cfg.Hardstop.DownswingBuyins = &threshold
	dailyCents := money.Cents(10000)
	cfg.Hardstop.DailyLossCents = &dailyCents
	cfg.Hardstop.SpingoDailyLossCents = &dailyCents
	cfg.Hardstop.ConsecutiveLosses = &count
	for _, reason := range []string{"session_loss_buyins", "consecutive_losses", "downswing_buyins", "daily_loss_cents", "spingo_daily_loss_cents"} {
		t.Run(reason, func(t *testing.T) {
			cash := parsers.CashSession{Date: now, FirstHandID: "owned-first-hand", NetCents: -200, BuyInCents: 100, Hands: 10}
			tournament := parsers.Tournament{Date: now, Type: "spingo", TournamentID: "owned-tournament", NetCents: -200, BuyInCents: 100}
			ledger := []storage.LedgerEntry{{Date: now, Type: "cash_session", SessionID: "cash-result", AmountCents: -200, RunningTotalCents: -200}}
			id, err := AutomaticTriggerID(reason, 1, cfg, now, &cash, &tournament, ledger, []parsers.Tournament{tournament})
			if err != nil {
				t.Fatal(err)
			}
			cash.VPIP = 88
			cash.PFR = 44
			ledger[0].RunningTotalCents = 99900
			ledger[0].Date = ledger[0].Date.Truncate(time.Minute)
			ledger = append(ledger, storage.LedgerEntry{Date: now, Type: "deposit", AmountCents: 10000, SessionID: "money:ignored"})
			again, err := AutomaticTriggerID(reason, 1, cfg, now.Add(time.Minute), &cash, &tournament, ledger, []parsers.Tournament{tournament})
			if err != nil {
				t.Fatal(err)
			}
			if id != again {
				t.Fatal("replay time, date precision, transfers or display stats changed identity")
			}
			cash.NetCents = -201
			tournament.NetCents = -201
			ledger[0].AmountCents = -201
			changed, err := AutomaticTriggerID(reason, 1, cfg, now, &cash, &tournament, ledger, []parsers.Tournament{tournament})
			if err != nil {
				t.Fatal(err)
			}
			if changed == id {
				t.Fatal("a new financial loss reused the previous trigger")
			}
		})
	}
}

func TestDownswingResultsKeepLedgerOrderForTriggerReplay(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -30)
	entries := []storage.LedgerEntry{
		{Date: now, Type: "cash_session", AmountCents: -300},
		{Date: cutoff, Type: "tournament", AmountCents: 4500},
		{Date: cutoff.Add(time.Second), Type: "tournament", AmountCents: 900},
	}
	for _, movement := range []string{"deposit", "withdrawal", "reward"} {
		t.Run(movement, func(t *testing.T) {
			ledger := append(slices.Clone(entries), storage.LedgerEntry{Type: movement, Date: now, AmountCents: 10000})
			if got := downswingResults(ledger, now); !slices.Equal(got, []storage.LedgerEntry{entries[0], entries[2]}) {
				t.Fatalf("trigger results: %v", got)
			}
			peak, current := DownswingEquityCurve(ledger, now)
			if peak != 900 || current != 600 {
				t.Fatalf("equity curve peak=%d current=%d", peak, current)
			}
		})
	}
}
