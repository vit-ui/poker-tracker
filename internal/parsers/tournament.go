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
	PrizePoolUSD float64 // 0 when the pool itself is non-cash-denominated — see PrizePoolDescription

	// PrizePoolDescription holds the "Total Prize Pool" line's exact text
	// when it's non-cash rather than a dollar amount (e.g. "$1.00 Power
	// Path Step 2 Ticket" — the prize's own face-value name, not a real
	// cash pool). Empty for a normal cash-denominated prize pool, in which
	// case PrizePoolUSD holds the real amount instead.
	PrizePoolDescription string
	Multiplier           *float64 // non-nil only for spingo with a real cash prize pool (nil if PrizePoolDescription is set)
	FinishPos            int
	TotalPlayers         int
	NetUSD               float64

	// ReEntries is the number of re-entries/rebuys/add-ons beyond the first
	// entry, from the "You made N re-entries for a total of $X" line (0 for a
	// single-entry tournament). BuyInUSD/NetUSD already fold in their cost;
	// this count is kept only so the post-session alert can say the row is a
	// re-entry and how many entries it represents. Not persisted by either
	// storage backend — it's read straight off the summary each parse.
	ReEntries int
	VPIP      *float64 // populated from hand history via UpdateTournamentHandStats
	PFR       *float64 // populated from hand history via UpdateTournamentHandStats
	Hands     *int     // populated from hand history via UpdateTournamentHandStats; the weight /stats uses for VPIP/PFR

	// LikelySatellite is true if the tournament title (the summary's
	// subtitle line, e.g. "Super Satellite") or any result row's payout is
	// non-cash (ticket, seat/package, T$, or qualifier — see isNonCashPrize).
	// Rows are scanned in ascending finish-position order rather than just
	// position 1 — the summary file can be written the moment screenName
	// busts, while the tournament continues for players still in, so position
	// 1's row can still read "still playing" (and every other row with it),
	// leaving the title line as the only satellite signal in the file.
	LikelySatellite bool

	// WinnerPrizeDescription is the first non-cash payout text found
	// scanning result rows in ascending finish-position order (so position
	// 1's, whenever it's decided). Empty if not flagged.
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
		t              Tournament
		playerCount    int
		beforeBuyIn    bool    // between the "PokerStars Tournament #" line and "Buy-In:" — the title/subtitle region
		foundPlayerRow bool    // screenName's best result row has been read (rows are ascending, so the first is best)
		reEntryCost    float64 // extra buy-ins beyond the first (re-entries / rebuys / add-ons), folded into BuyInUSD after the loop
		reEntryCount   int     // number of re-entries beyond the first entry (the "N" in "You made N re-entries")
	)
	// A re-entry lists screenName once per entry, each row after the first
	// carrying a "[N]" bullet marker between the name and the country paren
	// ("Name [2] (Country)"). Match both the plain "(" and the "[" forms so the
	// re-entered bullets are visible — otherwise only the busted first entry
	// (which has no marker) is ever read.
	playerParen := ": " + screenName + " ("
	playerBracket := ": " + screenName + " ["

	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")

		switch {
		case strings.HasPrefix(line, "PokerStars Tournament #"):
			rest := line[len("PokerStars Tournament #"):]
			if end := strings.Index(rest, ","); end > 0 {
				t.TournamentID = rest[:end]
			}
			beforeBuyIn = true

		case strings.HasSuffix(line, " players"):
			if n, err := strconv.Atoi(strings.TrimSuffix(line, " players")); err == nil {
				playerCount = n
			}

		case strings.HasPrefix(line, "Buy-In: $"):
			beforeBuyIn = false
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
			if isNonCashPrize(rest) {
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

		case strings.HasPrefix(line, "You made ") && strings.Contains(line, "for a total of $"):
			// Re-entries / rebuys / add-ons are extra money spent beyond the
			// base buy-in ("You made 1 re-entries for a total of $1.00.").
			// Anchoring on "for a total of $" (not the word "re-entries")
			// covers all three uniformly. The total is folded into BuyInUSD
			// after the loop, so NetUSD ends up as payout − total buy-in.
			rest := line[strings.Index(line, "for a total of $")+len("for a total of $"):]
			// The amount carries a trailing period ("$1.00.") that
			// parseLeadingAmount would otherwise consume as a digit and fail on.
			rest = strings.TrimRight(rest, ". \r")
			if amt, ok := parseLeadingAmount(rest); ok {
				reEntryCost += amt
			}
			// The count is the leading integer right after "You made "
			// ("You made 2 re-entries …" → 2), kept only for the alert text.
			if n, ok := parseLeadingAmount(line[len("You made "):]); ok {
				reEntryCount += int(n)
			}

		// Only the first matching row wins: rows are in ascending finish-position
		// order, so screenName's first match is their best (deepest) finish — the
		// re-entered bullet that lasted longest, not an earlier busted one.
		case !foundPlayerRow && (strings.Contains(line, playerParen) || strings.Contains(line, playerBracket)):
			foundPlayerRow = true
			trimmed := strings.TrimLeft(line, " ")
			colonIdx := strings.Index(trimmed, ":")
			if colonIdx > 0 {
				if pos, err := strconv.Atoi(strings.TrimSpace(trimmed[:colonIdx])); err == nil {
					t.FinishPos = pos
				}
			}
			// Payout text is after "), " — empty when the player won
			// nothing. A non-cash payout (e.g. "$1.00 Power Path Step 2
			// Ticket") can still start with a dollar sign — that's the
			// prize's own face-value name, not a cash credit PokerStars
			// actually paid out — so it must never be parsed as a NetUSD
			// amount (the satellite-detection loop below decides whether
			// it's non-cash). A screenName who wins a non-cash prize has
			// NetUSD computed the same as a screenName who won nothing at
			// all: just -BuyInUSD.
			const payoutTextMarker = "), "
			t.NetUSD = -t.BuyInUSD
			if payIdx := strings.Index(line, payoutTextMarker); payIdx >= 0 {
				payoutText := strings.TrimRight(line[payIdx+len(payoutTextMarker):], " \r")
				if payoutText != "" && !isNonCashPrize(payoutText) {
					if payout, ok := parseLeadingAmount(payoutText); ok {
						t.NetUSD = payout - t.BuyInUSD
					}
				}
			}
		}

		// Header-title satellite detection: a summary can name its
		// satellite/qualifier only in the title line (e.g. "Super Satellite")
		// while every result row still reads "still playing" and the prize
		// pool is a real cash amount — the summary is written the moment
		// screenName busts, with the field still in, so the row scan below
		// finds no non-cash payout at all. Word-check only the title/subtitle
		// region between the header line and "Buy-In:"; the "Total Prize Pool"
		// line (which can itself be ticket-denominated) sits after Buy-In and
		// is already handled as PrizePoolDescription, so it's excluded here.
		if !t.LikelySatellite && beforeBuyIn && isNonCashPrize(line) {
			t.LikelySatellite = true
			t.WinnerPrizeDescription = line
		}

		// Satellite/qualifier detection: scan every result row, not just
		// finish position 1 — the file lists rows in ascending position
		// order, so the first non-cash payout found here is always the
		// best (numerically lowest) already-decided one, matching the
		// original position-1-only intent whenever position 1 is decided.
		// Checking every row (not stopping at position 1) is what catches
		// two cases position-1-only missed entirely: a screenName who wins
		// a non-cash prize at a position other than 1st, and a
		// still-in-progress tournament — the summary can be written the
		// moment screenName busts, while the tournament continues for
		// players still in, so position 1's row reads "still playing"
		// rather than showing a payout at all. Position 1 not yet being
		// decided is not the same as position 1 not being non-cash, and if
		// any other row worse than screenName is already decided as
		// non-cash (e.g. a multi-tier payout structure with more than one
		// ticket awarded), that's still as reliable a satellite signal as
		// position 1's would be.
		//
		// This does not close every gap, but the remaining one is narrow: a
		// ticket-denominated prize pool is caught after the loop via
		// PrizePoolDescription (the "Total Prize Pool" line names the ticket
		// even when every result row still reads "still playing"), so only a
		// *cash*-denominated satellite is undetectable here — and only when
		// screenName busts before any seat has been decided (won nothing, and
		// nobody else has cashed a non-cash prize yet either), leaving no
		// signal in the file at all. PokerStars' export format doesn't expose
		// a cash-pool tournament's satellite/qualifier structure independent
		// of an actual decided payout. This is a known hard problem for
		// third-party trackers generally, not unique to this parser.
		if !t.LikelySatellite {
			if _, payout, ok := parseFinishLine(line); ok && isNonCashPrize(payout) {
				t.LikelySatellite = true
				t.WinnerPrizeDescription = payout
			}
		}
	}

	if t.BuyInUSD == 0 {
		return Tournament{}, ErrFreeroll
	}

	// Re-entries make BuyInUSD the sum of every entry's buy-in, and NetUSD the
	// payout minus that total (NetUSD was computed in-loop against the base
	// buy-in only). Both are no-ops for a single-entry tournament.
	t.BuyInUSD += reEntryCost
	t.NetUSD -= reEntryCost
	t.ReEntries = reEntryCount

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

	// A ticket-denominated prize pool (PrizePoolDescription set) is itself a
	// definitive satellite/qualifier signal, even when every result row still
	// reads "still playing" and the title line carries no keyword — the
	// summary is written the moment screenName busts, with the field still in.
	// Guarded so a row-scan/title hit keeps its own (row-derived)
	// WinnerPrizeDescription.
	if !t.LikelySatellite && t.PrizePoolDescription != "" {
		t.LikelySatellite = true
		t.WinnerPrizeDescription = t.PrizePoolDescription
	}

	return t, nil
}

// isNonCashPrize reports whether a payout description (or a tournament title
// line — see the header-title check in ParseTournamentFile) is a non-cash
// prize: ticket, seat/package, satellite/sat, qualifier/qualy/qually, or T$
// tournament dollars, rather than a real cash payout. These can still start
// with a dollar sign (e.g.
// "$1.00 Power Path Step 2 Ticket" — the ticket's own face-value name, not a
// cash credit PokerStars actually paid out, or "T$11.00" — Tournament
// Dollars, usable only for buy-ins, not real cash), so this is a text
// match, not a numeric check.
//
// The word list is deliberately generous, "sat" included despite being a
// short substring that can also match inside an ordinary word (e.g.
// "Saturday") — a false positive here only costs one extra tap on the
// cash/ticket confirmation (see sendSatelliteMenu), the player answers
// "paid cash" and nothing changes. A false negative costs silently wrong
// bankroll data with no way to notice or recover it. That asymmetry means
// new candidate words from PokerStars' own terminology are worth adding
// speculatively here, unlike NetUSD/BuyInUSD parsing elsewhere in this
// file, where a wrong guess corrupts real numbers with no user-facing
// signal that anything went wrong at all.
func isNonCashPrize(payout string) bool {
	lower := strings.ToLower(payout)
	for _, word := range []string{"ticket", "pass", "seat", "package", "t$", "satellite", "sat", "qualifier", "qualy", "qually"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
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
