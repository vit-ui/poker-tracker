package storage

import (
	"math"
	"testing"
	"time"
)

// closeEnough reports whether a and b differ by less than half a cent — USD
// amounts derived from chained float64 arithmetic can land a few ULPs off an
// exact decimal value.
func closeEnough(a, b float64) bool {
	return math.Abs(a-b) < 0.005
}

func TestCascadeRunningTotals(t *testing.T) {
	t.Run("single row, last in ledger — matches a simple update", func(t *testing.T) {
		got := CascadeRunningTotals([]float64{-5}, -8, 100)
		want := []float64{92}
		if len(got) != 1 || !closeEnough(got[0], want[0]) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("interleaved tables — later row's running total must shift too", func(t *testing.T) {
		// Two tables open at once, start bankroll 100:
		//   deposit          +100 -> RT 100 (base, not part of amounts)
		//   table A hand 1     -5 -> RT  95
		//   table B hand 1     -3 -> RT  92
		// Table A's 2nd hand arrives: cumulative net now -8, correcting row0.
		// row1 (table B) must be recomputed too.
		got := CascadeRunningTotals([]float64{-5, -3}, -8, 100)
		want := []float64{92, 89}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if !closeEnough(got[i], want[i]) {
				t.Errorf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("changed row is the only row, base zero", func(t *testing.T) {
		got := CascadeRunningTotals([]float64{-5}, -8, 0)
		want := []float64{-8}
		if len(got) != 1 || !closeEnough(got[0], want[0]) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("three rows after the changed one", func(t *testing.T) {
		got := CascadeRunningTotals([]float64{10, 1, 2, 3}, 20, 0)
		want := []float64{20, 21, 23, 26}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if !closeEnough(got[i], want[i]) {
				t.Errorf("index %d: got %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("empty amounts returns nil", func(t *testing.T) {
		if got := CascadeRunningTotals(nil, -8, 0); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

// TestReEntryRewriteSequenceMath is the sheets-side twin of sqlitestore's
// TestAppendTournament_ReEntryRewriteUpsertsInPlace: the sqlite test proves
// the three-bust re-entry sequence end-to-end against a real .db, while the
// sheets backend composes the same per-bust update from these shared pure
// helpers (rawAppendTournament: FoldBountyIntoNet on the found row;
// updateLedgerAmountCascading: CascadeRunningTotals). Asserting the sequence
// here pins that both backends' re-entry arithmetic agrees — the "backends
// never diverge" invariant for this path — without live-sheets I/O.
func TestReEntryRewriteSequenceMath(t *testing.T) {
	// A re-entry sequence (anonymized T9000000006): three busts, no cashes, no bounties.
	busts := []struct{ buyIn, summaryNet float64 }{
		{1.10, -1.10},
		{2.20, -2.20},
		{3.30, -3.30},
	}
	var net, prizeWon float64
	for _, b := range busts {
		net, prizeWon = FoldBountyIntoNet(b.summaryNet, 0, b.buyIn)
	}
	if !closeEnough(net, -3.30) || !closeEnough(prizeWon, 0) {
		t.Errorf("final fold = (net %.4f, prizeWon %.4f), want (-3.30, 0)", net, prizeWon)
	}

	// A knockout variant: a $0.25 bounty already on the row (hand file ran
	// first) must be re-folded against each successively larger buy-in, not
	// lost or double-counted across rewrites.
	var koNet, koPrize float64
	for _, b := range busts {
		koNet, koPrize = FoldBountyIntoNet(b.summaryNet, 0.25, b.buyIn)
	}
	if !closeEnough(koNet, -3.05) || !closeEnough(koPrize, 0.25) {
		t.Errorf("knockout fold = (net %.4f, prizeWon %.4f), want (-3.05, 0.25)", koNet, koPrize)
	}

	// The ledger side: the tournament's single row is corrected in place on
	// each bust, cascading over a later row (a cash session that landed
	// after the first bust). Start bankroll 20.
	amounts := []float64{-1.10, 2.50} // tournament row, then a later cash win
	var totals []float64
	for _, b := range busts[1:] {
		totals = CascadeRunningTotals(amounts, b.summaryNet, 20)
		amounts[0] = b.summaryNet
	}
	if len(totals) != 2 || !closeEnough(totals[0], 16.70) || !closeEnough(totals[1], 19.20) {
		t.Errorf("cascaded totals = %v, want [16.70 19.20] (20 - 3.30, then + 2.50)", totals)
	}
}

func TestReconcileKnockoutType(t *testing.T) {
	cases := []struct {
		name             string
		baseType         string
		existingTypeCell string
		want             string
	}{
		{"placeholder from a knockout-flagged partial row", "mtt", KnockoutPlaceholder, "mtt-ko"},
		{"spingo variant", "spingo", KnockoutPlaceholder, "spingo-ko"},
		{"no prior knockout signal at all", "mtt", "", "mtt"},
		{"already-suffixed row from an earlier reconcile — idempotent", "mtt", "mtt-ko", "mtt-ko"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReconcileKnockoutType(c.baseType, c.existingTypeCell); got != c.want {
				t.Errorf("ReconcileKnockoutType(%q, %q) = %q, want %q", c.baseType, c.existingTypeCell, got, c.want)
			}
		})
	}
}

func TestAppendKnockoutSuffix(t *testing.T) {
	cases := []struct {
		name             string
		existingTypeCell string
		want             string
	}{
		{"base type not known yet — summary hasn't arrived", "", KnockoutPlaceholder},
		{"base type already known", "mtt", "mtt-ko"},
		{"spingo variant", "spingo", "spingo-ko"},
		{"already the placeholder — idempotent", KnockoutPlaceholder, KnockoutPlaceholder},
		{"already suffixed — idempotent", "mtt-ko", "mtt-ko"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AppendKnockoutSuffix(c.existingTypeCell); got != c.want {
				t.Errorf("AppendKnockoutSuffix(%q) = %q, want %q", c.existingTypeCell, got, c.want)
			}
		})
	}
}

func TestFoldBountyIntoNet(t *testing.T) {
	cases := []struct {
		name                             string
		baselineNetUSD, bountyUSD, buyIn float64
		wantNet, wantPrizeWon            float64
	}{
		{"no bounty yet — hand file hasn't arrived", -0.12, 0, 0.55, -0.12, 0.43},
		{"real KO tournament — a loss on paper becomes a win", -0.12, 0.90, 0.55, 0.78, 1.33},
		{"non-knockout tournament, bounty always zero", 2.00, 0, 5.00, 2.00, 7.00},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotNet, gotPrizeWon := FoldBountyIntoNet(c.baselineNetUSD, c.bountyUSD, c.buyIn)
			if !closeEnough(gotNet, c.wantNet) || !closeEnough(gotPrizeWon, c.wantPrizeWon) {
				t.Errorf("FoldBountyIntoNet(%v, %v, %v) = (%v, %v), want (%v, %v)",
					c.baselineNetUSD, c.bountyUSD, c.buyIn, gotNet, gotPrizeWon, c.wantNet, c.wantPrizeWon)
			}
		})
	}
}

func TestReconcileTournamentBounty(t *testing.T) {
	cases := []struct {
		name                                               string
		storedNetUSD, storedBountyUSD, newBountyUSD, buyIn float64
		wantNet, wantPrizeWon                              float64
	}{
		{"bounty grows — more KOs since the last hand-file parse", 0.78, 0.90, 1.26, 0.55, 1.14, 1.69},
		{"unchanged re-parse is a no-op", 0.78, 0.90, 0.90, 0.55, 0.78, 1.33},
		{"first bounty applied after summary already landed", -0.12, 0, 0.90, 0.55, 0.78, 1.33},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotNet, gotPrizeWon := ReconcileTournamentBounty(c.storedNetUSD, c.storedBountyUSD, c.newBountyUSD, c.buyIn)
			if !closeEnough(gotNet, c.wantNet) || !closeEnough(gotPrizeWon, c.wantPrizeWon) {
				t.Errorf("ReconcileTournamentBounty(%v, %v, %v, %v) = (%v, %v), want (%v, %v)",
					c.storedNetUSD, c.storedBountyUSD, c.newBountyUSD, c.buyIn, gotNet, gotPrizeWon, c.wantNet, c.wantPrizeWon)
			}
		})
	}
}

// TestAsStoredClock checks the defining behavior: the result is a UTC
// instant whose wall-clock digits equal the input instant's *local*
// wall-clock reading — the coordinate system stored dates live in. The
// machine zone is pinned to UTC-3 (Brazil, the player's zone) for the test
// so the expected values are absolute rather than re-derived from the
// implementation.
func TestAsStoredClock(t *testing.T) {
	orig := time.Local
	t.Cleanup(func() { time.Local = orig })
	time.Local = time.FixedZone("BRT", -3*60*60)

	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{
			"UTC instant reads back as its Brazil wall clock",
			time.Date(2026, 7, 15, 23, 30, 45, 0, time.UTC), // 20:30:45 in UTC-3
			time.Date(2026, 7, 15, 20, 30, 45, 0, time.UTC),
		},
		{
			"crossing midnight backwards — early-UTC instant is still the previous local day",
			time.Date(2026, 7, 16, 1, 15, 0, 0, time.UTC), // 22:15 on the 15th in UTC-3
			time.Date(2026, 7, 15, 22, 15, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AsStoredClock(c.in)
			if !got.Equal(c.want) || got.Location() != time.UTC {
				t.Errorf("AsStoredClock(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
