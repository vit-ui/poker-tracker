package main

import (
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

func TestCashSessionSummaryText(t *testing.T) {
	cases := []struct {
		name     string
		session  parsers.CashSession
		bankroll float64
		want     string
	}{
		{
			name: "winning session",
			session: parsers.CashSession{
				Stake: "NL10", NetUSD: 5.5, Hands: 42, DurationMin: 37,
			},
			bankroll: 105.5,
			want:     "✅ Cash Session\nStake: NL10\nNet: +$5.50\nHands: 42\nDuration: 37 min\nBankroll: $105.50",
		},
		{
			name: "losing session",
			session: parsers.CashSession{
				Stake: "NL25", NetUSD: -12.75, Hands: 10, DurationMin: 5,
			},
			bankroll: 87.25,
			want:     "✅ Cash Session\nStake: NL25\nNet: -$12.75\nHands: 10\nDuration: 5 min\nBankroll: $87.25",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cashSessionSummaryText(c.session, c.bankroll)
			if got != c.want {
				t.Errorf("cashSessionSummaryText() = %q, want %q", got, c.want)
			}
		})
	}
}

// waitForString blocks until a string arrives on ch or timeout elapses.
func waitForString(t *testing.T, ch <-chan string, timeout time.Duration) (string, bool) {
	t.Helper()
	select {
	case s := <-ch:
		return s, true
	case <-time.After(timeout):
		return "", false
	}
}

// TestScheduleCashSessionSummary_FiresAfterQuietPeriod confirms a single
// schedule call sends the built message once the quiet period elapses.
func TestScheduleCashSessionSummary_FiresAfterQuietPeriod(t *testing.T) {
	var mu sync.Mutex
	timers := make(map[string]*time.Timer)
	alertChan := make(chan string, 10)

	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 50*time.Millisecond, alertChan, func() string {
		return "final summary"
	})

	got, ok := waitForString(t, alertChan, 500*time.Millisecond)
	if !ok {
		t.Fatal("expected a summary after the quiet period, got none")
	}
	if got != "final summary" {
		t.Errorf("got %q, want %q", got, "final summary")
	}
}

// TestScheduleCashSessionSummary_ResetOnNewHand confirms rescheduling before
// the quiet period elapses (simulating a new hand at the same table) cancels
// the pending send and collapses to exactly one message, built from the
// latest snapshot rather than the first one.
func TestScheduleCashSessionSummary_ResetOnNewHand(t *testing.T) {
	var mu sync.Mutex
	timers := make(map[string]*time.Timer)
	alertChan := make(chan string, 10)

	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 100*time.Millisecond, alertChan, func() string {
		return "hand 1 snapshot"
	})
	time.Sleep(30 * time.Millisecond)
	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 100*time.Millisecond, alertChan, func() string {
		return "hand 2 snapshot"
	})

	got, ok := waitForString(t, alertChan, 500*time.Millisecond)
	if !ok {
		t.Fatal("expected one summary after the quiet period, got none")
	}
	if got != "hand 2 snapshot" {
		t.Errorf("got %q, want the latest snapshot %q", got, "hand 2 snapshot")
	}
	if _, ok := waitForString(t, alertChan, 200*time.Millisecond); ok {
		t.Error("expected only one summary for a table that kept getting new hands, got a second")
	}
}

// TestScheduleCashSessionSummary_IndependentPerTable confirms two different
// tables (different paths) each get their own independent timer and summary
// — resetting one table's timer must never cancel another table's.
func TestScheduleCashSessionSummary_IndependentPerTable(t *testing.T) {
	var mu sync.Mutex
	timers := make(map[string]*time.Timer)
	alertChan := make(chan string, 10)

	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 50*time.Millisecond, alertChan, func() string {
		return "table a"
	})
	scheduleCashSessionSummary(&mu, timers, "table-b.txt", 50*time.Millisecond, alertChan, func() string {
		return "table b"
	})

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		got, ok := waitForString(t, alertChan, 500*time.Millisecond)
		if !ok {
			t.Fatal("expected two independent summaries, got fewer")
		}
		seen[got] = true
	}
	if !seen["table a"] || !seen["table b"] {
		t.Errorf("expected summaries from both tables, got %v", seen)
	}
}
