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

// Tournament holds results for a single tournament (Spin & Go or MTT).
type Tournament struct {
	Date         time.Time
	Type         string   // "spingo" or "mtt"
	BuyInUSD     float64
	PrizePoolUSD float64
	Multiplier   *float64 // non-nil only for spingo
	FinishPos    int
	TotalPlayers int
	NetUSD       float64
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
		t            Tournament
		playerCount  int
	)
	playerMarker := ": " + screenName + " ("

	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")

		switch {
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
			if amt, ok := parseLeadingAmount(line[len("Total Prize Pool: $"):]); ok {
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
			// Payout is after "), $" — absent when player won nothing.
			const payoutMarker = "), $"
			if payIdx := strings.Index(line, payoutMarker); payIdx >= 0 {
				if payout, ok := parseLeadingAmount(line[payIdx+len(payoutMarker):]); ok {
					t.NetUSD = payout - t.BuyInUSD
				}
			} else {
				t.NetUSD = -t.BuyInUSD
			}
		}
	}

	if t.BuyInUSD == 0 {
		return Tournament{}, ErrFreeroll
	}

	t.TotalPlayers = playerCount

	if playerCount == 3 {
		t.Type = "spingo"
		m := t.PrizePoolUSD / t.BuyInUSD
		t.Multiplier = &m
	} else {
		t.Type = "mtt"
	}

	return t, nil
}
