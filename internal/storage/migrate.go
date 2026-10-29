package storage

import (
	"fmt"
	"time"
)

// bankrollEpsilon absorbs the float round-trip through a Google Sheets cell
// (JSON number → string → JSON number); the sqlite path is exact.
const bankrollEpsilon = 0.005

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
	expected := ledgerSum(snap.Ledger)
	for _, backend := range []struct {
		name  string
		store Store
	}{{"source", src}, {"destination", dst}} {
		bankroll, err := backend.store.GetCurrentBankroll()
		if err != nil {
			return fmt.Errorf("reading %s bankroll: %w", backend.name, err)
		}
		if !sameNumber(bankroll, expected, bankrollEpsilon) {
			return fmt.Errorf("%s bankroll mismatch after migration: amounts sum to %.2f, bankroll %.2f", backend.name, expected, bankroll)
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
	sourceSum, destinationSum := ledgerSum(expected.Ledger), ledgerSum(actual.Ledger)
	if !sameNumber(sourceSum, destinationSum, bankrollEpsilon) {
		return fmt.Errorf("ledger amount mismatch after migration: source sum %.2f, destination sum %.2f", sourceSum, destinationSum)
	}
	for _, data := range []struct {
		name string
		rows []LedgerEntry
		sum  float64
	}{
		{"source", expected.Ledger, sourceSum}, {"destination", actual.Ledger, destinationSum},
	} {
		final := 0.0
		if len(data.rows) > 0 {
			final = data.rows[len(data.rows)-1].RunningTotal
		}
		if !sameNumber(final, data.sum, bankrollEpsilon) {
			return fmt.Errorf("%s ledger mismatch: amounts sum to %.2f, final running total %.2f", data.name, data.sum, final)
		}
	}
	for i, want := range expected.Ledger {
		got := actual.Ledger[i]
		if !sameDate(want.Date, got.Date, precision) || want.Type != got.Type || want.SessionID != got.SessionID ||
			!sameNumber(want.AmountUSD, got.AmountUSD, bankrollEpsilon) || !sameNumber(want.RunningTotal, got.RunningTotal, bankrollEpsilon) {
			return fmt.Errorf("ledger entry %d differs after migration", i+1)
		}
	}
	for i, want := range expected.CashSessions {
		got := actual.CashSessions[i]
		if want.SessionID != got.SessionID || !sameDate(want.Date, got.Date, precision) || want.Stake != got.Stake ||
			want.Hands != got.Hands || want.DurationMin != got.DurationMin ||
			!sameNumber(want.NetUSD, got.NetUSD, bankrollEpsilon) || !sameNumber(want.RakeUSD, got.RakeUSD, bankrollEpsilon) ||
			!sameNumber(want.BB100, got.BB100, 1e-9) || !sameNumber(want.VPIP, got.VPIP, 1e-9) || !sameNumber(want.PFR, got.PFR, 1e-9) {
			return fmt.Errorf("cash session %d differs after migration", i+1)
		}
	}
	for i, want := range expected.Tournaments {
		got := actual.Tournaments[i]
		if want.TournamentID != got.TournamentID || !sameDate(want.Date, got.Date, precision) || want.Type != got.Type ||
			want.PrizePoolDescription != got.PrizePoolDescription || want.FinishPos != got.FinishPos || want.TotalPlayers != got.TotalPlayers ||
			!sameNumber(want.BuyInUSD, got.BuyInUSD, bankrollEpsilon) || !sameNumber(want.PrizePoolUSD, got.PrizePoolUSD, bankrollEpsilon) ||
			!sameNumber(want.NetUSD, got.NetUSD, bankrollEpsilon) || !sameNumber(want.BountyWonUSD, got.BountyWonUSD, bankrollEpsilon) ||
			!sameNumber(want.PrizeWonUSD, got.PrizeWonUSD, bankrollEpsilon) || !sameOptionalNumber(want.Multiplier, got.Multiplier) ||
			!sameOptionalNumber(want.VPIP, got.VPIP) || !sameOptionalNumber(want.PFR, got.PFR) || !sameOptionalInt(want.Hands, got.Hands) {
			return fmt.Errorf("tournament %d differs after migration", i+1)
		}
	}
	return nil
}
