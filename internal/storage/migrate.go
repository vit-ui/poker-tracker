package storage

import (
	"fmt"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
)

// Migrate copies every row from src into dst — which must be empty — as a raw
// snapshot (see Snapshot), then verifies the copy landed correctly. It does not
// touch config.json and does not restart anything; the caller does that only
// after Migrate returns without error. The returned Snapshot is what was
// copied, for the caller to report row counts.
//
// dst must be constructed so a failed write errors rather than being buffered
// (the sheets backend's write-ahead fallback must be disabled) — a migration
// that silently queued half its rows would report success with data missing.
func Migrate(src, dst Store) (Snapshot, error) {
	snap, err := src.ExportSnapshot()
	if err != nil {
		return Snapshot{}, fmt.Errorf("reading source data: %w", err)
	}

	existing, err := dst.ExportSnapshot()
	if err != nil {
		return Snapshot{}, fmt.Errorf("reading destination data: %w", err)
	}
	if !existing.IsEmpty() {
		return Snapshot{}, fmt.Errorf("destination backend is not empty — migration only runs into a fresh backend")
	}

	if err := dst.ImportSnapshot(snap); err != nil {
		return Snapshot{}, fmt.Errorf("writing to destination: %w", err)
	}

	if err := verifyMigration(src, dst, snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// verifyMigration compares the destination's independently read records and
// amounts with the source snapshot, then checks both live bankroll readings.
func verifyMigration(src, dst Store, snap Snapshot) error {
	check, err := dst.ExportSnapshot()
	if err != nil {
		return fmt.Errorf("re-reading destination for verification: %w", err)
	}
	precision := time.Second
	if src.GetSheetURL() != "" || dst.GetSheetURL() != "" {
		precision = time.Minute
	}
	if err := VerifySnapshot(snap, check, precision); err != nil {
		return err
	}
	expectedCents, err := ledgerSum(snap.Ledger)
	if err != nil {
		return err
	}
	for _, backend := range []struct {
		name  string
		store Store
	}{{"source", src}, {"destination", dst}} {
		bankrollCents, err := backend.store.GetCurrentBankroll()
		if err != nil {
			return fmt.Errorf("reading %s bankroll: %w", backend.name, err)
		}
		if !(bankrollCents == expectedCents) {
			return fmt.Errorf("%s bankroll mismatch after migration: amounts sum to %s, bankroll %s", backend.name, expectedCents, bankrollCents)
		}
	}
	return nil
}

// VerifySnapshot verifies a raw import without recalculating or changing any
// stored values. precision reflects the backend's stored date precision (one
// minute for Sheets, one second for SQLite). Record order and blank IDs matter.
func VerifySnapshot(expected, actual Snapshot, precision time.Duration) error {
	for _, count := range []struct {
		name      string
		want, got int
	}{
		{"cash sessions", len(expected.CashSessions), len(actual.CashSessions)},
		{"tournaments", len(expected.Tournaments), len(actual.Tournaments)},
		{"ledger entries", len(expected.Ledger), len(actual.Ledger)},
	} {
		if count.want != count.got {
			return fmt.Errorf("%s count mismatch after migration: wrote %d, read back %d", count.name, count.want, count.got)
		}
	}
	sourceSumCents, err := ledgerSum(expected.Ledger)
	if err != nil {
		return err
	}
	destinationSumCents, err := ledgerSum(actual.Ledger)
	if err != nil {
		return err
	}
	if !(sourceSumCents == destinationSumCents) {
		return fmt.Errorf("ledger amount mismatch after migration: source sum %s, destination sum %s", sourceSumCents, destinationSumCents)
	}
	for _, data := range []struct {
		name     string
		rows     []LedgerEntry
		sumCents money.Cents
	}{
		{"source", expected.Ledger, sourceSumCents}, {"destination", actual.Ledger, destinationSumCents},
	} {
		var finalCents money.Cents
		if len(data.rows) > 0 {
			finalCents = data.rows[len(data.rows)-1].RunningTotalCents
		}
		if !(finalCents == data.sumCents) {
			return fmt.Errorf("%s ledger mismatch: amounts sum to %s, final running total %s", data.name, data.sumCents, finalCents)
		}
	}
	for i, want := range expected.Ledger {
		got := actual.Ledger[i]
		if !sameDate(want.Date, got.Date, precision) || want.Type != got.Type || want.SessionID != got.SessionID ||
			!(want.AmountCents == got.AmountCents) || !(want.RunningTotalCents == got.RunningTotalCents) {
			return fmt.Errorf("ledger entry %d differs after migration", i+1)
		}
	}
	for i, want := range expected.CashSessions {
		got := actual.CashSessions[i]
		if want.SessionID != got.SessionID || !sameDate(want.Date, got.Date, precision) || want.Stake != got.Stake ||
			want.Hands != got.Hands || want.DurationMin != got.DurationMin ||
			!(want.NetCents == got.NetCents) || !(want.RakeCents == got.RakeCents) ||
			!sameNumber(want.BB100, got.BB100, 1e-9) || !sameNumber(want.VPIP, got.VPIP, 1e-9) || !sameNumber(want.PFR, got.PFR, 1e-9) {
			return fmt.Errorf("cash session %d differs after migration", i+1)
		}
	}
	for i, want := range expected.Tournaments {
		got := actual.Tournaments[i]
		if want.TournamentID != got.TournamentID || !sameDate(want.Date, got.Date, precision) || want.Type != got.Type ||
			want.PrizePoolDescription != got.PrizePoolDescription || want.FinishPos != got.FinishPos || want.TotalPlayers != got.TotalPlayers ||
			!(want.BuyInCents == got.BuyInCents) || !(want.PrizePoolCents == got.PrizePoolCents) ||
			!(want.NetCents == got.NetCents) || !(want.BountyWonCents == got.BountyWonCents) ||
			!(want.PrizeWonCents == got.PrizeWonCents) || !sameOptionalNumber(want.Multiplier, got.Multiplier) ||
			!sameOptionalNumber(want.VPIP, got.VPIP) || !sameOptionalNumber(want.PFR, got.PFR) || !sameOptionalInt(want.Hands, got.Hands) {
			return fmt.Errorf("tournament %d differs after migration", i+1)
		}
	}
	return nil
}
