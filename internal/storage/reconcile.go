package storage

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
)

// This file holds the pure reconciliation math both backends share. The two
// backends must never diverge in how they compute the player's money, so the
// functions live here exactly once and each backend's I/O code calls them —
// they used to be duplicated copy-for-copy in internal/sheets and
// internal/sqlitestore, which is precisely how a fix applied to one backend
// but not the other would have produced different bankroll numbers depending
// on which backend an install uses.

// KnockoutPlaceholder marks the Type of a partial tournament row (created by
// UpdateTournamentHandStats before the summary file has arrived) as a
// knockout tournament, since the base "spingo"/"mtt" classification isn't
// known yet at that point — AppendTournament combines it into the real
// "<base>-ko" Type once it does.
const KnockoutPlaceholder = "ko"

// ReconcileKnockoutType computes the Type value AppendTournament should
// write once the base type ("spingo"/"mtt") is known, combining it with
// whatever knockout signal an existing row (created by
// UpdateTournamentHandStats before the summary file arrived) already
// recorded — either KnockoutPlaceholder or a Type already carrying
// parsers.KnockoutSuffix.
func ReconcileKnockoutType(baseType, existingTypeCell string) string {
	if existingTypeCell == KnockoutPlaceholder || strings.HasSuffix(existingTypeCell, parsers.KnockoutSuffix) {
		return baseType + parsers.KnockoutSuffix
	}
	return baseType
}

// AppendKnockoutSuffix computes the Type value UpdateTournamentHandStats
// should write when a hand history file flags a tournament as knockout,
// preserving whatever base type (or lack thereof, via KnockoutPlaceholder)
// the row's Type already has. Idempotent — a Type that's already suffixed or
// already the placeholder is returned unchanged.
func AppendKnockoutSuffix(existingTypeCell string) string {
	if existingTypeCell == KnockoutPlaceholder || strings.HasSuffix(existingTypeCell, parsers.KnockoutSuffix) {
		return existingTypeCell
	}
	if existingTypeCell == "" {
		return KnockoutPlaceholder
	}
	return existingTypeCell + parsers.KnockoutSuffix
}

// FoldBountyIntoNet computes the final NetCents and Prize Won (USD) once the
// summary file's baseline net (payout - buyin) is known, folding in whatever
// bounty winnings the hand history file has already recorded for this row.
func FoldBountyIntoNet(baselineNetCents, bountyCents, buyInCents money.Cents) (netCents, prizeWonCents money.Cents, err error) {
	netCents, err = money.Add(baselineNetCents, bountyCents)
	if err != nil {
		return 0, 0, err
	}
	prizeWonCents, err = money.Add(netCents, buyInCents)
	return netCents, prizeWonCents, err
}

// ReconcileTournamentBounty computes the corrected NetCents and Prize Won
// (USD) after a freshly reparsed cumulative bounty total replaces whatever
// this row's previous bounty contribution to storedNetCents was.
func ReconcileTournamentBounty(storedNetCents, storedBountyCents, newBountyCents, buyInCents money.Cents) (netCents, prizeWonCents money.Cents, err error) {
	baselineCents, err := money.Subtract(storedNetCents, storedBountyCents)
	if err != nil {
		return 0, 0, err
	}
	return FoldBountyIntoNet(baselineCents, newBountyCents, buyInCents)
}

// CascadeRunningTotals recomputes running totals for a contiguous tail of
// the Bankroll Ledger (amounts, oldest first, amounts[0] being the row that
// changed). amounts[0] is treated as newAmount; every later row keeps its
// own amount but needs a recomputed running total, since a change to an
// earlier row shifts every total after it. base is the running total of the
// ledger row immediately before amounts[0] (0 if amounts[0] is the ledger's
// first row).
func CascadeRunningTotals(amountsCents []money.Cents, newAmountCents, baseCents money.Cents) ([]money.Cents, error) {
	if len(amountsCents) == 0 {
		return nil, nil
	}
	totalsCents := make([]money.Cents, len(amountsCents))
	runningCents := baseCents
	for i, amountCents := range amountsCents {
		if i == 0 {
			amountCents = newAmountCents
		}
		var err error
		runningCents, err = money.Add(runningCents, amountCents)
		if err != nil {
			return nil, err
		}
		totalsCents[i] = runningCents
	}
	return totalsCents, nil
}

// AsStoredClock reinterprets t's own wall-clock reading — the digits in
// whatever zone t already carries — as a UTC instant, the coordinate system
// every stored date lives in. Both backends store dates without a real zone
// offset (the sheets backend's "2006-01-02 15:04" cells and the sqlite
// backend's Z-suffixed RFC3339 strings both hold local wall-clock digits,
// matching the zone-less timestamps PokerStars writes), so a stored "20:00"
// always reads back as the instant 20:00 UTC regardless of the machine's real
// zone. Any window boundary compared against stored dates must be computed
// from this, never from a real local-zone instant — otherwise every boundary
// shifts by the machine's UTC offset.
//
// It must NOT re-localize t to the process's zone (an earlier version called
// t.Local() first): the split server passes Server.ClientNow() — the current
// instant expressed in the *collector's* timezone, so its digits are the poker
// PC's wall clock. t.Local() would overwrite that with the server's zone
// (x.In(clientLoc).Local() == x.Local()), silently shifting every split-mode
// window by the server↔collector offset and defeating client-clock anchoring
// entirely. Reading t's own digits keeps all-local unchanged (there t is
// already a time.Local instant) while making the anchored split correct.
func AsStoredClock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

func ledgerSum(rows []LedgerEntry) (money.Cents, error) {
	var sumCents money.Cents
	for _, row := range rows {
		var err error
		sumCents, err = money.Add(sumCents, row.AmountCents)
		if err != nil {
			return 0, err
		}
	}
	return sumCents, nil
}

func sameNumber(a, b, tolerance float64) bool {
	return !math.IsNaN(a) && !math.IsNaN(b) && !math.IsInf(a, 0) && !math.IsInf(b, 0) && math.Abs(a-b) <= tolerance
}

func sameOptionalNumber(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sameNumber(*a, *b, 1e-9)
}

func sameDate(a, b time.Time, precision time.Duration) bool {
	return a.UTC().Truncate(precision).Equal(b.UTC().Truncate(precision))
}

// ValidateNumbers rejects NaN and infinities before they reach config or storage.
func ValidateNumbers(values ...float64) error {
	for i, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("numeric value %d must be finite", i+1)
		}
	}
	return nil
}

// ValidateCashSession checks the numeric fields used by both backends.
func ValidateCashSession(session parsers.CashSession) error {
	return ValidateNumbers(session.BB100, session.VPIP, session.PFR)
}

// ValidateTournament preserves optional values while checking every number.
func ValidateTournament(tournament parsers.Tournament) error {
	for _, value := range []*float64{tournament.Multiplier, tournament.VPIP, tournament.PFR} {
		if value != nil {
			if err := ValidateNumbers(*value); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateSnapshot checks a raw snapshot before any import writes are made.
func ValidateSnapshot(snapshot Snapshot) error {
	for _, session := range snapshot.CashSessions {
		if err := ValidateCashSession(session.CashSession); err != nil {
			return fmt.Errorf("cash session %s: %w", session.SessionID, err)
		}
	}
	for _, tournament := range snapshot.Tournaments {
		if err := ValidateTournament(tournament.Tournament); err != nil {
			return fmt.Errorf("tournament %s: %w", tournament.TournamentID, err)
		}
	}
	_, err := ledgerSum(snapshot.Ledger)
	return err
}

func sameOptionalInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ValidateMoneyOperationID keeps movement identities separate from parser-owned
// session/tournament keys. Blank identities are retained only for legacy rows.
func ValidateMoneyOperationID(id string) error {
	if id != "" && (!strings.HasPrefix(id, "money:") || len(id) == len("money:")) {
		return fmt.Errorf("invalid money operation identity")
	}
	return nil
}
