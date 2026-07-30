package storage

import (
	"strings"
	"time"

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

// FoldBountyIntoNet computes the final NetUSD and Prize Won (USD) once the
// summary file's baseline net (payout - buyin) is known, folding in whatever
// bounty winnings the hand history file has already recorded for this row.
func FoldBountyIntoNet(baselineNetUSD, bountyUSD, buyInUSD float64) (netUSD, prizeWonUSD float64) {
	netUSD = baselineNetUSD + bountyUSD
	return netUSD, netUSD + buyInUSD
}

// ReconcileTournamentBounty computes the corrected NetUSD and Prize Won
// (USD) after a freshly reparsed cumulative bounty total replaces whatever
// this row's previous bounty contribution to storedNetUSD was.
func ReconcileTournamentBounty(storedNetUSD, storedBountyUSD, newBountyUSD, buyInUSD float64) (netUSD, prizeWonUSD float64) {
	netUSD = (storedNetUSD - storedBountyUSD) + newBountyUSD
	return netUSD, netUSD + buyInUSD
}

// CascadeRunningTotals recomputes running totals for a contiguous tail of
// the Bankroll Ledger (amounts, oldest first, amounts[0] being the row that
// changed). amounts[0] is treated as newAmount; every later row keeps its
// own amount but needs a recomputed running total, since a change to an
// earlier row shifts every total after it. base is the running total of the
// ledger row immediately before amounts[0] (0 if amounts[0] is the ledger's
// first row).
func CascadeRunningTotals(amounts []float64, newAmount, base float64) []float64 {
	if len(amounts) == 0 {
		return nil
	}
	totals := make([]float64, len(amounts))
	running := base
	for i, amt := range amounts {
		if i == 0 {
			amt = newAmount
		}
		running += amt
		totals[i] = running
	}
	return totals
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
