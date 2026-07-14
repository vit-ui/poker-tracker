package hardstop

import (
	"testing"

	"github.com/vit-ui/self-tracker/internal/sheets"
)

// entry is a terse constructor for a sheets.LedgerEntry used only to build
// test fixtures — entries are listed oldest first, matching the chronological
// order both GetLedgerEntriesSince and GetRecentLedgerRows return.
func entry(entryType string, amount float64) sheets.LedgerEntry {
	return sheets.LedgerEntry{Type: entryType, AmountUSD: amount}
}

// TestConsecutiveLossesTriggered exercises the fixed decision end to end:
// filtering to session rows and windowing to the last n happen together
// (recentSessionRows) before the streak is measured, so a deposit or
// withdrawal anywhere in the ledger can never dilute or break the window a
// losing streak is measured against.
func TestConsecutiveLossesTriggered(t *testing.T) {
	cases := []struct {
		name    string
		entries []sheets.LedgerEntry
		n       int
		want    bool
	}{
		{
			name: "clean N-loss streak fires",
			entries: []sheets.LedgerEntry{
				entry("cash_session", -5),
				entry("cash_session", -3),
				entry("cash_session", -2),
			},
			n:    3,
			want: true,
		},
		{
			name: "N-loss streak with a deposit interleaved still fires — this is the bug this fix addresses",
			entries: []sheets.LedgerEntry{
				entry("cash_session", -5),
				entry("deposit", 50),
				entry("cash_session", -3),
				entry("cash_session", -2),
			},
			n:    3,
			want: true,
		},
		{
			name: "streak broken by a winning session does not fire",
			entries: []sheets.LedgerEntry{
				entry("cash_session", -5),
				entry("cash_session", 3),
				entry("cash_session", -2),
			},
			n:    3,
			want: false,
		},
		{
			name: "N-loss streak with a withdrawal interleaved still fires, even though the withdrawal amount is itself negative",
			entries: []sheets.LedgerEntry{
				entry("cash_session", -5),
				entry("cash_session", -3),
				entry("withdrawal", -20),
				entry("cash_session", -2),
			},
			n:    3,
			want: true,
		},
		{
			name: "fewer than N sessions total never fires, even if every one is a loss",
			entries: []sheets.LedgerEntry{
				entry("cash_session", -5),
				entry("cash_session", -3),
			},
			n:    3,
			want: false,
		},
		{
			name: "an older win outside the N-row window doesn't block the current streak",
			entries: []sheets.LedgerEntry{
				entry("cash_session", 10), // old win, outside the last-3 window
				entry("cash_session", -5),
				entry("cash_session", -3),
				entry("cash_session", -2),
			},
			n:    3,
			want: true,
		},
		{
			name: "only deposits and withdrawals, no sessions at all, never fires",
			entries: []sheets.LedgerEntry{
				entry("deposit", 100),
				entry("withdrawal", -20),
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
			got := consecutiveLossesTriggered(c.entries, c.n)
			if got != c.want {
				t.Errorf("consecutiveLossesTriggered(%+v, n=%d) = %v, want %v", c.entries, c.n, got, c.want)
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
		entries := []sheets.LedgerEntry{
			entry("cash_session", -5),
			entry("deposit", 50),
			entry("cash_session", -3),
			entry("cash_session", -2),
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
		wantAmounts := []float64{-5, -3, -2}
		for i, e := range got {
			if e.AmountUSD != wantAmounts[i] {
				t.Errorf("got[%d].AmountUSD = %v, want %v", i, e.AmountUSD, wantAmounts[i])
			}
		}
	})

	t.Run("returns fewer than n when there aren't enough session rows", func(t *testing.T) {
		entries := []sheets.LedgerEntry{entry("cash_session", -5)}
		got := recentSessionRows(entries, 3)
		if len(got) != 1 {
			t.Errorf("len(got) = %d, want 1", len(got))
		}
	})

	t.Run("older session rows outside the window are dropped", func(t *testing.T) {
		entries := []sheets.LedgerEntry{
			entry("cash_session", 999), // far outside the last-2 window
			entry("cash_session", -5),
			entry("cash_session", -3),
		}
		got := recentSessionRows(entries, 2)
		if len(got) != 2 {
			t.Fatalf("len(got) = %d, want 2", len(got))
		}
		if got[0].AmountUSD != -5 || got[1].AmountUSD != -3 {
			t.Errorf("got = %+v, want last 2 rows only", got)
		}
	})
}
