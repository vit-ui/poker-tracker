package hardstop

import (
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
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
	cfg.Hardstop.DailyLossUSD = &threshold
	cfg.Hardstop.SpingoDailyLossUSD = &threshold
	cfg.Hardstop.ConsecutiveLosses = &count
	for _, reason := range []string{"session_loss_buyins", "consecutive_losses", "downswing_buyins", "daily_loss_usd", "spingo_daily_loss_usd"} {
		t.Run(reason, func(t *testing.T) {
			cash := parsers.CashSession{Date: now, FirstHandID: "owned-first-hand", NetUSD: -2, BuyInUSD: 1, Hands: 10}
			tournament := parsers.Tournament{Date: now, Type: "spingo", TournamentID: "owned-tournament", NetUSD: -2, BuyInUSD: 1}
			ledger := []storage.LedgerEntry{{Date: now, Type: "cash_session", SessionID: "cash-result", AmountUSD: -2, RunningTotal: -2}}
			id, err := AutomaticTriggerID(reason, 1, cfg, now, &cash, &tournament, ledger, []parsers.Tournament{tournament})
			if err != nil {
				t.Fatal(err)
			}
			cash.VPIP = 88
			cash.PFR = 44
			ledger[0].RunningTotal = 999
			ledger[0].Date = ledger[0].Date.Truncate(time.Minute)
			ledger = append(ledger, storage.LedgerEntry{Date: now, Type: "deposit", AmountUSD: 100, SessionID: "money:ignored"})
			again, err := AutomaticTriggerID(reason, 1, cfg, now.Add(time.Minute), &cash, &tournament, ledger, []parsers.Tournament{tournament})
			if err != nil {
				t.Fatal(err)
			}
			if id != again {
				t.Fatal("replay time, date precision, transfers or display stats changed identity")
			}
			cash.NetUSD = -3
			tournament.NetUSD = -3
			ledger[0].AmountUSD = -3
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
