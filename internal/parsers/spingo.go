package parsers

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrFreeroll is returned by ParseSpinGo when the file has no buy-in.
// The caller should skip freerolls — they have no monetary value.
var ErrFreeroll = errors.New("freeroll")

// ErrNotSpinGo is returned by ParseSpinGo for tournament files that are not
// 3-player Spin & Go games (MTTs, HORSE, Stud, etc.). The caller should skip them.
var ErrNotSpinGo = errors.New("not a spin & go")

// SpinGoGame holds results for a single Spin & Go tournament.
type SpinGoGame struct {
	Date         time.Time
	BuyInUSD     float64
	PrizePoolUSD float64
	Multiplier   float64
	FinishPos    int
	NetUSD       float64
}

// ParseSpinGo parses a PokerStars tournament summary (.txt) file.
// screenName is the player to track, passed from POKERSTARS_SCREEN_NAME.
func ParseSpinGo(path, screenName string) (SpinGoGame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SpinGoGame{}, fmt.Errorf("reading %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 {
		l := lines[0]
		if len(l) >= 3 && l[0] == '\xef' && l[1] == '\xbb' && l[2] == '\xbf' {
			lines[0] = l[3:]
		}
	}

	var (
		g           SpinGoGame
		playerCount int
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
					g.BuyInUSD = prize + rake
				}
			}

		case strings.HasPrefix(line, "Total Prize Pool: $"):
			if amt, ok := parseLeadingAmount(line[len("Total Prize Pool: $"):]); ok {
				g.PrizePoolUSD = amt
			}

		case strings.HasPrefix(line, "Tournament started "):
			ts := line[len("Tournament started "):]
			// PokerStars sometimes emits single-digit hours ("9:27:41" not "09:27:41").
			if len(ts) >= 18 && ts[12] == ':' {
				ts = ts[:11] + "0" + ts[11:]
			}
			if len(ts) >= 19 {
				if t, err := time.Parse("2006/01/02 15:04:05", ts[:19]); err == nil {
					g.Date = t
				}
			}

		case strings.Contains(line, playerMarker):
			trimmed := strings.TrimLeft(line, " ")
			colonIdx := strings.Index(trimmed, ":")
			if colonIdx > 0 {
				if pos, err := strconv.Atoi(strings.TrimSpace(trimmed[:colonIdx])); err == nil {
					g.FinishPos = pos
				}
			}
			// Payout is after "), $" — absent when player won nothing.
			const payoutMarker = "), $"
			if payIdx := strings.Index(line, payoutMarker); payIdx >= 0 {
				if payout, ok := parseLeadingAmount(line[payIdx+len(payoutMarker):]); ok {
					g.NetUSD = payout - g.BuyInUSD
				}
			} else {
				g.NetUSD = -g.BuyInUSD
			}
		}
	}

	if g.BuyInUSD == 0 {
		return SpinGoGame{}, ErrFreeroll
	}
	if playerCount != 3 {
		return SpinGoGame{}, ErrNotSpinGo
	}
	g.Multiplier = g.PrizePoolUSD / g.BuyInUSD

	return g, nil
}
