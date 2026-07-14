package parsers

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrFreeroll is returned by ParseTournamentFile when the file has no buy-in.
// The caller should skip freerolls — they have no monetary value.
var ErrFreeroll = errors.New("freeroll")

// KnockoutSuffix marks a knockout (bounty) tournament on Tournament.Type,
// e.g. "mtt-ko" or "spingo-ko". Only ever appended by the storage layer once
// the hand history file reveals it — the tournament summary file, which
// ParseTournamentFile derives the base "spingo"/"mtt" from, never mentions
// bounties at all (why two files: plan §Why Tournaments Need Two Files).
// Code that filters or branches on game type should compare against
// BaseType(), not Type directly, so a knockout tournament still counts as
// its base type.
const KnockoutSuffix = "-ko"

// Tournament holds results for a single tournament (Spin & Go or MTT).
type Tournament struct {
	Date         time.Time
	Type         string // "spingo" or "mtt", plus KnockoutSuffix if it's a knockout tournament
	TournamentID string
	BuyInUSD     float64
	PrizePoolUSD float64 // 0 when the pool itself is ticket/pass-denominated — see PrizePoolDescription

	// PrizePoolDescription holds the "Total Prize Pool" line's exact text
	// when it's a ticket/pass rather than a dollar amount (e.g. "$1.00
	// Power Path Step 2 Ticket" — the ticket's own face-value name, not a
	// real cash pool). Empty for a normal cash-denominated prize pool, in
	// which case PrizePoolUSD holds the real amount instead.
	PrizePoolDescription string
	Multiplier           *float64 // non-nil only for spingo with a real cash prize pool (nil if PrizePoolDescription is set)
	FinishPos            int
	TotalPlayers         int
	NetUSD               float64
	VPIP                 *float64 // populated from hand history via UpdateTournamentHandStats
	PFR                  *float64 // populated from hand history via UpdateTournamentHandStats
	// LikelySatellite is true if finish position 1's payout reads as a
	// ticket/pass, or if screenName's own payout does. The second trigger
	// exists because the summary file can be written while other players
	// are still in the tournament — position 1's row reads "still playing"
	// until they're decided, which is not the same as position 1 not being
	// a ticket, so relying on position 1 alone misses a screenName who
	// themselves won a ticket in a still-in-progress tournament.
	LikelySatellite bool

	// WinnerPrizeDescription is finish position 1's exact payout text if
	// that triggered LikelySatellite, else screenName's own payout text if
	// that did instead. Empty if not flagged.
	WinnerPrizeDescription string
}

// BaseType returns Type with any KnockoutSuffix stripped, so filtering or
// branching logic ("spingo" vs "mtt") treats a knockout tournament the same
// as its non-knockout counterpart.
func (t Tournament) BaseType() string {
	return strings.TrimSuffix(t.Type, KnockoutSuffix)
}

// ParseTournamentFile parses a PokerStars tournament summary (.txt) file.
// screenName is the player to track. Spin & Go games are identified by having
// exactly 3 players; all others are classified as MTT.
func ParseTournamentFile(path, screenName string) (Tournament, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Tournament{}, fmt.Errorf("reading %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 {
		l := lines[0]
		if len(l) >= 3 && l[0] == '\xef' && l[1] == '\xbb' && l[2] == '\xbf' {
			lines[0] = l[3:]
		}
	}

	var (
		t           Tournament
		playerCount int
	)
	playerMarker := ": " + screenName + " ("

	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")

		switch {
		case strings.HasPrefix(line, "PokerStars Tournament #"):
			rest := line[len("PokerStars Tournament #"):]
			if end := strings.Index(rest, ","); end > 0 {
				t.TournamentID = rest[:end]
			}

		case strings.HasSuffix(line, " players"):
			if n, err := strconv.Atoi(strings.TrimSuffix(line, " players")); err == nil {
				playerCount = n
			}

		case strings.HasPrefix(line, "Buy-In: $"):
			rest := line[len("Buy-In: $"):]
			slashIdx := strings.Index(rest, "/")
			if slashIdx >= 0 {
				prize, ok1 := parseLeadingAmount(rest[:slashIdx])
				rake, ok2 := parseLeadingAmount(rest[slashIdx+1:])
				if ok1 && ok2 {
					t.BuyInUSD = prize + rake
				}
			}

		case strings.HasPrefix(line, "Total Prize Pool: $"):
			// A satellite/qualifier's prize pool can itself be
			// ticket-denominated (e.g. "Total Prize Pool: $1.00 Power Path
			// Step 2 Ticket") — that leading dollar amount is the ticket's
			// own face-value name, not a real cash pool, so it must never
			// be parsed as PrizePoolUSD (same distinction the player's own
			// payout and finish-position-1 checks make).
			rest := strings.TrimRight(line[len("Total Prize Pool: $"):], " \r")
			if isTicketOrPass(rest) {
				t.PrizePoolDescription = "$" + rest
			} else if amt, ok := parseLeadingAmount(rest); ok {
				t.PrizePoolUSD = amt
			}

		case strings.HasPrefix(line, "Tournament started "):
			ts := line[len("Tournament started "):]
			// PokerStars sometimes emits single-digit hours ("9:27:41" not "09:27:41").
			if len(ts) >= 18 && ts[12] == ':' {
				ts = ts[:11] + "0" + ts[11:]
			}
			if len(ts) >= 19 {
				if parsed, err := time.Parse("2006/01/02 15:04:05", ts[:19]); err == nil {
					t.Date = parsed
				}
			}

		case strings.Contains(line, playerMarker):
			trimmed := strings.TrimLeft(line, " ")
			colonIdx := strings.Index(trimmed, ":")
			if colonIdx > 0 {
				if pos, err := strconv.Atoi(strings.TrimSpace(trimmed[:colonIdx])); err == nil {
					t.FinishPos = pos
				}
			}
			// Payout text is after "), " — empty when the player won
			// nothing. A ticket/pass payout (e.g. "$1.00 Power Path Step 2
			// Ticket") still starts with a dollar sign — that's the ticket's
			// own face-value name, not a cash credit PokerStars actually
			// paid out — so it must never be parsed as a NetUSD amount (the
			// satellite-detection loop below decides whether it's a ticket).
			// A screenName who wins a ticket has NetUSD computed the same as
			// a screenName who won nothing at all: just -BuyInUSD.
			const payoutTextMarker = "), "
			t.NetUSD = -t.BuyInUSD
			if payIdx := strings.Index(line, payoutTextMarker); payIdx >= 0 {
				payoutText := strings.TrimRight(line[payIdx+len(payoutTextMarker):], " \r")
				if payoutText != "" && !isTicketOrPass(payoutText) {
					if payout, ok := parseLeadingAmount(payoutText); ok {
						t.NetUSD = payout - t.BuyInUSD
					}
				}
			}
		}

		// Satellite/qualifier detection: scan every result row, not just
		// finish position 1 — the file lists rows in ascending position
		// order, so the first ticket/pass payout found here is always the
		// best (numerically lowest) already-decided one, matching the
		// original position-1-only intent whenever position 1 is decided.
		// Checking every row (not stopping at position 1) is what catches
		// two cases position-1-only missed entirely: a screenName who wins
		// a ticket at a position other than 1st, and a still-in-progress
		// tournament — the summary can be written the moment screenName
		// busts, while the tournament continues for players still in, so
		// position 1's row reads "still playing" rather than showing a
		// payout at all. Position 1 not yet being decided is not the same
		// as position 1 not being a ticket, and if any other row worse than
		// screenName is already decided as a ticket (e.g. a multi-tier
		// payout structure with more than one ticket awarded), that's still
		// as reliable a satellite signal as position 1's would be.
		//
		// This does not close every gap: if screenName busts before any
		// ticket-awarding position has been decided at all (busted early,
		// won nothing, and nobody else has cashed a ticket yet either),
		// there is genuinely no signal left in this file to detect it from
		// — PokerStars' own export format doesn't expose a tournament's
		// satellite/qualifier structure independent of an actual decided
		// payout. This is a known hard problem for third-party trackers
		// generally, not unique to this parser.
		if !t.LikelySatellite {
			if _, payout, ok := parseFinishLine(line); ok && isTicketOrPass(payout) {
				t.LikelySatellite = true
				t.WinnerPrizeDescription = payout
			}
		}
	}

	if t.BuyInUSD == 0 {
		return Tournament{}, ErrFreeroll
	}

	t.TotalPlayers = playerCount

	if playerCount == 3 {
		t.Type = "spingo"
		// A ticket-denominated prize pool (PrizePoolDescription set) has no
		// real dollar amount to divide by BuyInUSD — Multiplier stays nil,
		// same as it already does for every MTT.
		if t.PrizePoolDescription == "" {
			m := t.PrizePoolUSD / t.BuyInUSD
			t.Multiplier = &m
		}
	} else {
		t.Type = "mtt"
	}

	return t, nil
}

// isTicketOrPass reports whether a payout description is a tournament
// ticket or pass rather than a real cash payout. These can still start with
// a dollar sign (e.g. "$1.00 Power Path Step 2 Ticket" — the ticket's own
// face-value name, not a cash credit PokerStars actually paid out), so this
// is a text match, not a numeric check.
func isTicketOrPass(payout string) bool {
	lower := strings.ToLower(payout)
	return strings.Contains(lower, "ticket") || strings.Contains(lower, "pass")
}

// parseFinishLine extracts the finish position and exact payout text from a
// results line like "  1: Name (Country), $1.50 Power Path Step 2 Ticket".
// ok is false for lines that aren't results lines (no "), " marker, or the
// text before the colon isn't a number).
func parseFinishLine(line string) (pos int, payout string, ok bool) {
	const marker = "), "
	idx := strings.Index(line, marker)
	if idx < 0 {
		return 0, "", false
	}
	trimmed := strings.TrimLeft(line, " ")
	colonIdx := strings.Index(trimmed, ":")
	if colonIdx <= 0 {
		return 0, "", false
	}
	p, err := strconv.Atoi(strings.TrimSpace(trimmed[:colonIdx]))
	if err != nil {
		return 0, "", false
	}
	return p, strings.TrimRight(line[idx+len(marker):], " \r"), true
}
