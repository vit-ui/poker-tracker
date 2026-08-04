package storage

import (
	"fmt"
	"math"
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

// verifyMigration confirms the destination reproduces the source's bankroll and
// per-data-set row counts. Two bankroll checks: the destination's final running
// total must equal the source's bankroll, and — since ImportSnapshot copies
// each ledger row's stored running total verbatim, which makes that first
// comparison alone near-tautological — it must also equal the copied Amount
// column summed independently, so a dropped or mangled amount fails even
// though the totals were copied. A dropped row also fails the count check.
func verifyMigration(src, dst Store, snap Snapshot) error {
	var expected float64
	for _, e := range snap.Ledger {
		expected += e.AmountUSD
	}

	dstBankroll, err := dst.GetCurrentBankroll()
	if err != nil {
		return fmt.Errorf("reading destination bankroll: %w", err)
	}
	if math.Abs(dstBankroll-expected) > bankrollEpsilon {
		return fmt.Errorf("bankroll mismatch after migration: ledger amounts sum to %.2f but destination running total is %.2f", expected, dstBankroll)
	}
	srcBankroll, err := src.GetCurrentBankroll()
	if err != nil {
		return fmt.Errorf("reading source bankroll: %w", err)
	}
	if math.Abs(dstBankroll-srcBankroll) > bankrollEpsilon {
		return fmt.Errorf("bankroll mismatch after migration: source %.2f, destination %.2f", srcBankroll, dstBankroll)
	}

	check, err := dst.ExportSnapshot()
	if err != nil {
		return fmt.Errorf("re-reading destination for verification: %w", err)
	}
	for _, m := range []struct {
		name       string
		got, wrote int
	}{
		{"cash sessions", len(check.CashSessions), len(snap.CashSessions)},
		{"tournaments", len(check.Tournaments), len(snap.Tournaments)},
		{"ledger entries", len(check.Ledger), len(snap.Ledger)},
	} {
		if m.got != m.wrote {
			return fmt.Errorf("%s count mismatch after migration: wrote %d, read back %d", m.name, m.wrote, m.got)
		}
	}
	return nil
}
