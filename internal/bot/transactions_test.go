package bot

import (
	"fmt"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestTransactionReports(t *testing.T) {
	date := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	entries := []storage.LedgerEntry{
		{Date: date, Type: "deposit", AmountCents: 5000},
		{Date: date.Add(time.Minute), Type: "withdrawal", AmountCents: -2000},
		{Date: date.Add(2 * time.Minute), Type: "reward", AmountCents: 251},
		{Date: date, Type: "cash_session", AmountCents: -999},
		{Date: date, Type: "tournament", AmountCents: 999},
	}
	for _, history := range []bool{false, true} {
		pages, err := transactionPages(entries, "all", history)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Deposits: +$50.00", "Withdrawals: -$20.00", "Rewards: +$2.51", "Net bankroll change: +$32.51"} {
			if !strings.Contains(pages[0], want) {
				t.Fatalf("missing %q: %s", want, pages[0])
			}
		}
		if strings.Contains(pages[0], "2026-11-18") != history {
			t.Fatal("history visibility")
		}
	}
	var many []storage.LedgerEntry
	for i := 0; i < 25; i++ {
		many = append(many, storage.LedgerEntry{Date: date.Add(time.Duration(i) * time.Minute), Type: "reward", AmountCents: 1})
	}
	pages, err := transactionPages(many, "month", true)
	if err != nil || len(pages) != 3 {
		t.Fatalf("pages=%d err=%v", len(pages), err)
	}
	joined := strings.Join(pages, "\n")
	for i := 0; i < 25; i++ {
		want := fmt.Sprintf("2026-11-18 12:%02d · reward · +$0.01", i)
		if strings.Count(joined, want) != 1 {
			t.Fatalf("missing/duplicate %s", want)
		}
	}
	if !strings.Contains(pages[0], "12:24") || !strings.Contains(pages[2], "12:00") {
		t.Fatal("not newest first")
	}
	for _, page := range pages {
		if len([]rune(page)) > 4096 {
			t.Fatal("message too long")
		}
	}
	for _, history := range []bool{true, false} {
		pages, err := transactionPages(nil, "all", history)
		if err != nil || len(pages) != 1 {
			t.Fatal(pages, err)
		}
	}
	_, err = transactionPages([]storage.LedgerEntry{{Type: "reward", AmountCents: math.MaxInt64}, {Type: "reward", AmountCents: 1}}, "all", false)
	if err == nil {
		t.Fatal("overflow accepted")
	}
}

func TestTransactionArguments(t *testing.T) {
	for _, tc := range []struct {
		args, period   string
		history, valid bool
	}{
		{"", "all", false, true}, {"month", "month", false, true}, {"history", "all", true, true},
		{"history today", "today", true, true}, {"history week", "week", true, true}, {"history all", "all", true, true},
		{"year", "", false, false}, {"month history", "", false, false}, {"history all extra", "", false, false},
	} {
		t.Run(tc.args, func(t *testing.T) {
			history, period, err := parseTransactionsArgs(tc.args)
			if (err == nil) != tc.valid || tc.valid && (history != tc.history || period != tc.period) {
				t.Fatal(history, period, err)
			}
		})
	}
}

func TestTransactionWindowsAndResultsExclusion(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.FixedZone("player", -3*3600))
	for _, tc := range []struct {
		period string
		want   string
	}{{"today", "+$2.00"}, {"week", "+$2.00"}, {"month", "+$2.00"}, {"all", "+$3.00"}} {
		t.Run(tc.period, func(t *testing.T) {
			since := statsSince(tc.period, now)
			local, err := sqlitestore.NewStore(filepath.Join(t.TempDir(), "window.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer local.Close()
			boundary := since
			if boundary.IsZero() {
				boundary = storage.AsStoredClock(now)
			}
			if err := local.AppendLedgerEntry("reward", 100, boundary.Add(-time.Minute), ""); err != nil {
				t.Fatal(err)
			}
			if err := local.AppendLedgerEntry("reward", 200, boundary, ""); err != nil {
				t.Fatal(err)
			}
			entries, err := local.GetLedgerEntriesSince(since)
			if err != nil {
				t.Fatal(err)
			}
			pages, err := transactionPages(entries, tc.period, false)
			if err != nil || !strings.Contains(pages[0], "Rewards: "+tc.want) {
				t.Fatal(pages, err)
			}
		})
	}
	rows := []storage.LedgerEntry{{Date: storage.AsStoredClock(now), Type: "reward", AmountCents: 100000}}
	if entries := hardstop.PokerDaySessionEntries(rows, &config.Config{}, now); len(entries) != 0 {
		t.Fatal(entries)
	}
	peak, current := hardstop.DownswingEquityCurve(rows, storage.AsStoredClock(now))
	if peak != 0 || current != 0 {
		t.Fatal(peak, current)
	}
}
