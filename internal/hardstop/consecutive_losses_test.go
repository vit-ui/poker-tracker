package hardstop

import (
	"testing"

	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// entry is a terse constructor for a storage.LedgerEntry used only to build
// test fixtures — entries are listed oldest first, matching the chronological
// order GetLedgerEntriesSince returns.
func entry(entryType string, amountCents money.Cents) storage.LedgerEntry {
	return storage.LedgerEntry{Type: entryType, AmountCents: amountCents}
}

// TestLosingStreakTriggered exercises the fixed decision end to end:
// filtering to session rows and windowing to the last n happen together
// (recentSessionRows) before the streak is measured, so a deposit or
// withdrawal anywhere in the ledger can never dilute or break the window a
// losing streak is measured against.
func TestLosingStreakTriggered(t *testing.T) {
	cases := []struct {
		name    string
		entries []storage.LedgerEntry
		n       int
		want    bool
	}{
		{
			name: "clean N-loss streak fires",
			entries: []storage.LedgerEntry{
				entry("cash_session", -500),
				entry("cash_session", -300),
				entry("cash_session", -200),
			},
			n:    3,
			want: true,
		},
		{
			name: "N-loss streak with a deposit interleaved still fires — this is the bug this fix addresses",
			entries: []storage.LedgerEntry{
				entry("cash_session", -500),
				entry("deposit", 5000),
				entry("cash_session", -300),
				entry("cash_session", -200),
			},
			n:    3,
			want: true,
		},
		{
			name: "streak broken by a winning session does not fire",
			entries: []storage.LedgerEntry{
				entry("cash_session", -500),
				entry("cash_session", 300),
				entry("cash_session", -200),
			},
			n:    3,
			want: false,
		},
		{
			name: "N-loss streak with a withdrawal interleaved still fires, even though the withdrawal amount is itself negative",
			entries: []storage.LedgerEntry{
				entry("cash_session", -500),
				entry("cash_session", -300),
				entry("withdrawal", -2000),
				entry("cash_session", -200),
			},
			n:    3,
			want: true,
		},
		{
			name: "fewer than N sessions total never fires, even if every one is a loss",
			entries: []storage.LedgerEntry{
				entry("cash_session", -500),
				entry("cash_session", -300),
			},
			n:    3,
			want: false,
		},
		{
			name: "an older win outside the N-row window doesn't block the current streak",
			entries: []storage.LedgerEntry{
				entry("cash_session", 1000), // old win, outside the last-3 window
				entry("cash_session", -500),
				entry("cash_session", -300),
				entry("cash_session", -200),
			},
			n:    3,
			want: true,
		},
		{
			name: "only deposits and withdrawals, no sessions at all, never fires",
			entries: []storage.LedgerEntry{
				entry("deposit", 10000),
				entry("withdrawal", -2000),
			},
			n:    3,
			want: false,
		},
		{
			name:    "empty ledger never fires",
			entries: nil,
			n:       3,
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := LosingStreakTriggered(c.entries, c.n)
			if got != c.want {
				t.Errorf("LosingStreakTriggered(%+v, n=%d) = %v, want %v", c.entries, c.n, got, c.want)
			}
		})
	}
}

// TestRecentSessionRows exercises the filtering+windowing helper directly:
// deposits/withdrawals are dropped before, not after, the last-n window is
// taken, and rows beyond the window (whether sessions or transfers) are
// dropped entirely.
func TestRecentSessionRows(t *testing.T) {
	t.Run("deposit is filtered out before windowing, not counted as a slot", func(t *testing.T) {
		entries := []storage.LedgerEntry{
			entry("cash_session", -500),
			entry("deposit", 5000),
			entry("cash_session", -300),
			entry("cash_session", -200),
		}
		got := recentSessionRows(entries, 3)
		if len(got) != 3 {
			t.Fatalf("len(got) = %d, want 3", len(got))
		}
		for _, e := range got {
			if e.Type == "deposit" || e.Type == "withdrawal" {
				t.Errorf("recentSessionRows returned a transfer row: %+v", e)
			}
		}
		wantAmountsCents := []money.Cents{-500, -300, -200}
		for i, e := range got {
			if e.AmountCents != wantAmountsCents[i] {
				t.Errorf("got[%d].AmountCents = %v, want %v", i, e.AmountCents, wantAmountsCents[i])
			}
		}
	})

	t.Run("returns fewer than n when there aren't enough session rows", func(t *testing.T) {
		entries := []storage.LedgerEntry{entry("cash_session", -500)}
		got := recentSessionRows(entries, 3)
		if len(got) != 1 {
			t.Errorf("len(got) = %d, want 1", len(got))
		}
	})

	t.Run("older session rows outside the window are dropped", func(t *testing.T) {
		entries := []storage.LedgerEntry{
			entry("cash_session", 99900), // far outside the last-2 window
			entry("cash_session", -500),
			entry("cash_session", -300),
		}
		got := recentSessionRows(entries, 2)
		if len(got) != 2 {
			t.Fatalf("len(got) = %d, want 2", len(got))
		}
		if got[0].AmountCents != -500 || got[1].AmountCents != -300 {
			t.Errorf("got = %+v, want last 2 rows only", got)
		}
	})
}
