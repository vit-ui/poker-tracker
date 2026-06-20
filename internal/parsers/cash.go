package parsers

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// CashSession holds aggregated stats for a single cash game session file.
type CashSession struct {
	Date        time.Time
	Stake       string
	BuyInUSD    float64
	Hands       int
	DurationMin int
	NetUSD      float64
	RakeUSD     float64
	BB100       float64
	VPIP        float64
	PFR         float64
}

// ParseCashSession parses a PokerStars cash game hand history file.
// screenName is the player to track, passed from POKERSTARS_SCREEN_NAME.
func ParseCashSession(path, screenName string) (CashSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return CashSession{}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var (
		firstTime, lastTime time.Time
		bigBlind            float64
		stake               string
		hands               int
		netUSD, rakeUSD     float64
		vpipCount, pfrCount int

		handActive    bool
		inPreflop     bool
		playerDealt   bool
		playerVPIP    bool
		playerPFR     bool
		streetTotal   float64
		handInvested  float64
		handCollected float64
		handUncalled  float64
		handRake      float64
	)

	ap := screenName + ": "
	dealtPfx := "Dealt to " + screenName + " ["
	collectedPfx := screenName + " collected $"
	uncalledPfx := "Uncalled bet ($"
	uncalledSfx := ") returned to " + screenName

	finalizeHand := func() {
		if !handActive {
			return
		}
		if playerDealt {
			hands++
			netUSD += handCollected + handUncalled - handInvested
			rakeUSD += handRake
			if playerVPIP {
				vpipCount++
			}
			if playerPFR {
				pfrCount++
			}
		}
		handActive = false
		inPreflop = false
		playerDealt = false
		playerVPIP = false
		playerPFR = false
		streetTotal = 0
		handInvested = 0
		handCollected = 0
		handUncalled = 0
		handRake = 0
	}

	for scanner.Scan() {
		line := scanner.Text()
		if len(line) >= 3 && line[0] == '\xef' && line[1] == '\xbb' && line[2] == '\xbf' {
			line = line[3:]
		}

		switch {
		case strings.HasPrefix(line, "PokerStars Hand #"):
			if strings.Contains(line, "Tournament #") {
				return CashSession{}, fmt.Errorf("%s is a tournament hand history, not a cash game", path)
			}
			finalizeHand()

			if bigBlind == 0 {
				parenIdx := strings.Index(line, "($")
				if parenIdx == -1 {
					return CashSession{}, fmt.Errorf("no blinds in header: %s", line)
				}
				afterParen := line[parenIdx+2:]
				slashIdx := strings.Index(afterParen, "/")
				if slashIdx == -1 {
					return CashSession{}, fmt.Errorf("no / in blinds: %s", line)
				}
				bbStr := afterParen[slashIdx+1:]
				if len(bbStr) > 0 && bbStr[0] == '$' {
					bbStr = bbStr[1:]
				}
				bb, ok := parseLeadingAmount(bbStr)
				if !ok {
					return CashSession{}, fmt.Errorf("bad BB in header: %s", line)
				}
				bigBlind = bb
				stake = "NL" + strconv.Itoa(int(math.Round(bb*100)))
			}

			// Timestamp is after the last " - " in the header.
			tsPart := line
			for {
				idx := strings.Index(tsPart, " - ")
				if idx == -1 {
					break
				}
				tsPart = tsPart[idx+3:]
			}
			ts := tsPart
			// PokerStars sometimes emits single-digit hours ("1:01:07" not "01:01:07").
			if len(ts) >= 18 && ts[12] == ':' {
				ts = ts[:11] + "0" + ts[11:]
			}
			if len(ts) >= 19 {
				t, err := time.Parse("2006/01/02 15:04:05", ts[:19])
				if err == nil {
					if firstTime.IsZero() {
						firstTime = t
					}
					lastTime = t
				}
			}

			handActive = true

		case line == "*** HOLE CARDS ***":
			inPreflop = true

		case strings.HasPrefix(line, dealtPfx):
			playerDealt = true

		case strings.HasPrefix(line, "*** FLOP ***"):
			inPreflop = false
			streetTotal = 0

		case strings.HasPrefix(line, "*** TURN ***"):
			streetTotal = 0

		case strings.HasPrefix(line, "*** RIVER ***"):
			streetTotal = 0

		case line == "*** SHOW DOWN ***", line == "*** SUMMARY ***":
			inPreflop = false

		case strings.HasPrefix(line, ap+"posts "):
			rest := line[len(ap+"posts "):]
			dollarIdx := strings.Index(rest, "$")
			if dollarIdx >= 0 {
				if amt, ok := parseLeadingAmount(rest[dollarIdx+1:]); ok {
					handInvested += amt
					streetTotal += amt
				}
			}

		case strings.HasPrefix(line, ap+"calls $"):
			if amt, ok := parseLeadingAmount(line[len(ap+"calls $"):]); ok {
				handInvested += amt
				streetTotal += amt
				if inPreflop {
					playerVPIP = true
				}
			}

		case strings.HasPrefix(line, ap+"bets $"):
			if amt, ok := parseLeadingAmount(line[len(ap+"bets $"):]); ok {
				handInvested += amt
				streetTotal += amt
			}

		case strings.HasPrefix(line, ap+"raises "):
			toIdx := strings.Index(line, " to $")
			if toIdx >= 0 {
				if total, ok := parseLeadingAmount(line[toIdx+5:]); ok {
					increment := total - streetTotal
					if increment < 0 {
						increment = 0
					}
					handInvested += increment
					streetTotal = total
					if inPreflop {
						playerVPIP = true
						playerPFR = true
					}
				}
			}

		case strings.HasPrefix(line, uncalledPfx) && strings.HasSuffix(line, uncalledSfx):
			closeIdx := strings.Index(line, ")")
			if closeIdx > len(uncalledPfx) {
				if amt, ok := parseLeadingAmount(line[len(uncalledPfx):closeIdx]); ok {
					handUncalled += amt
				}
			}

		case strings.HasPrefix(line, collectedPfx):
			if amt, ok := parseLeadingAmount(line[len(collectedPfx):]); ok {
				handCollected += amt
			}

		case strings.HasPrefix(line, "Total pot ") && strings.Contains(line, "| Rake $"):
			rakeIdx := strings.Index(line, "| Rake $")
			if rake, ok := parseLeadingAmount(line[rakeIdx+8:]); ok {
				handRake = rake
			}
		}
	}

	finalizeHand()

	if err := scanner.Err(); err != nil {
		return CashSession{}, fmt.Errorf("scanning %s: %w", path, err)
	}
	if hands == 0 {
		return CashSession{}, fmt.Errorf("no dealt hands found in %s", path)
	}

	dur := int(lastTime.Sub(firstTime).Minutes())

	var bb100 float64
	if bigBlind > 0 {
		bb100 = (netUSD / bigBlind) / float64(hands) * 100
	}

	return CashSession{
		Date:        firstTime,
		Stake:       stake,
		BuyInUSD:    bigBlind * 100,
		Hands:       hands,
		DurationMin: dur,
		NetUSD:      netUSD,
		RakeUSD:     rakeUSD,
		BB100:       bb100,
		VPIP:        float64(vpipCount) / float64(hands) * 100,
		PFR:         float64(pfrCount) / float64(hands) * 100,
	}, nil
}

// parseLeadingAmount parses a leading decimal number from s, ignoring a leading '$'.
func parseLeadingAmount(s string) (float64, bool) {
	if len(s) > 0 && s[0] == '$' {
		s = s[1:]
	}
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(s[:i], 64)
	return v, err == nil
}
