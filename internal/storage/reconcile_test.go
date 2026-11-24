package storage

import (
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
)

func closeEnough(aCents, bCents money.Cents) bool { return aCents == bCents }

func TestCascadeRunningTotals(t *testing.T) {
	t.Run("single row, last in ledger — matches a simple update", func(t *testing.T) {
		gotCents, err := CascadeRunningTotals([]money.Cents{-500}, -800, 10000)
		if err != nil {
			t.Fatal(err)
		}
		wantCents := []money.Cents{9200}
		if len(gotCents) != 1 || !closeEnough(gotCents[0], wantCents[0]) {
			t.Errorf("got %v, want %v", gotCents, wantCents)
		}
	})

	t.Run("interleaved tables — later row's running total must shift too", func(t *testing.T) {
		gotCents, err := CascadeRunningTotals([]money.Cents{-500, -300}, -800, 10000)
		if err != nil {
			t.Fatal(err)
		}
		wantCents := []money.Cents{9200, 8900}
		if len(gotCents) != len(wantCents) {
			t.Fatalf("got %v, want %v", gotCents, wantCents)
		}
		for i := range wantCents {
			if !closeEnough(gotCents[i], wantCents[i]) {
				t.Errorf("got %v, want %v", gotCents, wantCents)
			}
		}
	})

	t.Run("changed row is the only row, base zero", func(t *testing.T) {
		gotCents, err := CascadeRunningTotals([]money.Cents{-500}, -800, 0)
		if err != nil {
			t.Fatal(err)
		}
		wantCents := []money.Cents{-800}
		if len(gotCents) != 1 || !closeEnough(gotCents[0], wantCents[0]) {
			t.Errorf("got %v, want %v", gotCents, wantCents)
		}
	})

	t.Run("three rows after the changed one", func(t *testing.T) {
		gotCents, err := CascadeRunningTotals([]money.Cents{1000, 100, 200, 300}, 2000, 0)
		if err != nil {
			t.Fatal(err)
		}
		wantCents := []money.Cents{2000, 2100, 2300, 2600}
		if len(gotCents) != len(wantCents) {
			t.Fatalf("got %v, want %v", gotCents, wantCents)
		}
		for i := range wantCents {
			if !closeEnough(gotCents[i], wantCents[i]) {
				t.Errorf("index %d: got %v, want %v", i, gotCents[i], wantCents[i])
			}
		}
	})

	t.Run("empty amounts returns nil", func(t *testing.T) {
		if gotCents, err := CascadeRunningTotals(nil, -800, 0); err != nil || gotCents != nil {
			t.Errorf("got %v, want nil", gotCents)
		}
	})
}

func TestReEntryRewriteSequenceMath(t *testing.T) {
	busts := []struct{ buyInCents, summaryNetCents money.Cents }{
		{110, -110},
		{220, -220},
		{330, -330},
	}
	var netCents, prizeWonCents money.Cents
	var err error
	for _, b := range busts {
		netCents, prizeWonCents, err = FoldBountyIntoNet(b.summaryNetCents, 0, b.buyInCents)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !closeEnough(netCents, -330) || !closeEnough(prizeWonCents, 0) {
		t.Errorf("final fold = (net %v, prizeWon %v), want (-3.30, 0)", netCents, prizeWonCents)
	}

	var koNetCents, koPrizeCents money.Cents
	for _, b := range busts {
		koNetCents, koPrizeCents, err = FoldBountyIntoNet(b.summaryNetCents, 25, b.buyInCents)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !closeEnough(koNetCents, -305) || !closeEnough(koPrizeCents, 25) {
		t.Errorf("knockout fold = (net %v, prizeWon %v), want (-3.05, 0.25)", koNetCents, koPrizeCents)
	}

	amountsCents := []money.Cents{-110, 250}
	var totalsCents []money.Cents
	for _, b := range busts[1:] {
		totalsCents, err = CascadeRunningTotals(amountsCents, b.summaryNetCents, 2000)
		if err != nil {
			t.Fatal(err)
		}
		amountsCents[0] = b.summaryNetCents
	}
	if len(totalsCents) != 2 || !closeEnough(totalsCents[0], 1670) || !closeEnough(totalsCents[1], 1920) {
		t.Errorf("cascaded totals = %v, want [16.70 19.20] (20 - 3.30, then + 2.50)", totalsCents)
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
		name                                      string
		baselineNetCents, bountyCents, buyInCents money.Cents
		wantNetCents, wantPrizeWonCents           money.Cents
	}{
		{"no bounty yet — hand file hasn't arrived", -12, 0, 55, -12, 43},
		{"real KO tournament — a loss on paper becomes a win", -12, 90, 55, 78, 133},
		{"non-knockout tournament, bounty always zero", 200, 0, 500, 200, 700},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotNetCents, gotPrizeWonCents, err := FoldBountyIntoNet(c.baselineNetCents, c.bountyCents, c.buyInCents)
			if err != nil {
				t.Fatal(err)
			}
			if !closeEnough(gotNetCents, c.wantNetCents) || !closeEnough(gotPrizeWonCents, c.wantPrizeWonCents) {
				t.Errorf("FoldBountyIntoNet(%v, %v, %v) = (%v, %v), want (%v, %v)",
					c.baselineNetCents, c.bountyCents, c.buyInCents, gotNetCents, gotPrizeWonCents, c.wantNetCents, c.wantPrizeWonCents)
			}
		})
	}
}

func TestReconcileTournamentBounty(t *testing.T) {
	cases := []struct {
		name                                                          string
		storedNetCents, storedBountyCents, newBountyCents, buyInCents money.Cents
		wantNetCents, wantPrizeWonCents                               money.Cents
	}{
		{"bounty grows — more KOs since the last hand-file parse", 78, 90, 126, 55, 114, 169},
		{"unchanged re-parse is a no-op", 78, 90, 90, 55, 78, 133},
		{"first bounty applied after summary already landed", -12, 0, 90, 55, 78, 133},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotNetCents, gotPrizeWonCents, err := ReconcileTournamentBounty(c.storedNetCents, c.storedBountyCents, c.newBountyCents, c.buyInCents)
			if err != nil {
				t.Fatal(err)
			}
			if !closeEnough(gotNetCents, c.wantNetCents) || !closeEnough(gotPrizeWonCents, c.wantPrizeWonCents) {
				t.Errorf("ReconcileTournamentBounty(%v, %v, %v, %v) = (%v, %v), want (%v, %v)",
					c.storedNetCents, c.storedBountyCents, c.newBountyCents, c.buyInCents, gotNetCents, gotPrizeWonCents, c.wantNetCents, c.wantPrizeWonCents)
			}
		})
	}
}

func TestAsStoredClock(t *testing.T) {
	orig := time.Local
	t.Cleanup(func() { time.Local = orig })
	time.Local = time.UTC

	brt := time.FixedZone("BRT", -3*60*60)

	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{
			"collector-zone instant keeps its own wall clock, not the server's",
			time.Date(2026, 7, 15, 20, 30, 45, 0, brt),
			time.Date(2026, 7, 15, 20, 30, 45, 0, time.UTC),
		},
		{
			"a local (server-zone) instant keeps its digits too — the all-local path",
			time.Date(2026, 7, 15, 20, 30, 45, 0, time.Local),
			time.Date(2026, 7, 15, 20, 30, 45, 0, time.UTC),
		},
		{
			"late collector-zone evening stays on its own day even as the server rolls past midnight",
			time.Date(2026, 7, 15, 22, 15, 0, 0, brt),
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
