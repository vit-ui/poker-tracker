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

	// FirstHandID is the PokerStars Hand # of the first hand in the file —
	// globally unique and assigned once, so it never changes as PokerStars
	// appends later hands to the same file. Storage backends use it (with
	// Date) to identify this session across repeated re-parses of the same
	// growing file, the same role TournamentID plays for tournaments.
	FirstHandID string
}

// TournamentHandStats holds stats extracted from a tournament hand history
// file. Returned by ParseCashFile when the file is a tournament session.
// Knockout-bounty winnings only ever appear here — the tournament summary
// file has no concept of them at all, only the hand history file's
// elimination lines do (why two files: plan §Why Tournaments Need Two
// Files) — so BountyUSD and IsKnockout, like VPIP/PFR, are always populated
// from this struct, never from ParseTournamentFile.
type TournamentHandStats struct {
	TournamentID string
	VPIP         float64
	PFR          float64

	// Hands is the number of hands dealt to screenName in this tournament so
	// far — the same weight CashSession.Hands provides for cash sessions,
	// letting /stats weight a tournament's VPIP/PFR by how much of a sample
	// they actually represent. Recomputed from scratch on every parse, same
	// as VPIP/PFR/BountyUSD, since the file keeps growing across the whole
	// tournament.
	Hands int

	// BountyUSD is the cumulative knockout-bounty cash won by screenName
	// across every "<name> wins $X.XX for eliminating ..." line in the file,
	// recomputed from scratch on every parse — the same file keeps growing
	// with every hand across the whole tournament (not per table), so this
	// is always the tournament's true cumulative bounty total, not a
	// per-call increment. Zero for a non-knockout tournament.
	BountyUSD float64

	// IsKnockout is true if any seat in the file shows a bounty ("$X.XX
	// bounty)" in the pre-hand seat listing) — present for every seat in a
	// knockout tournament regardless of whether screenName ever eliminates
	// anyone, unlike BountyUSD which can legitimately be zero in a knockout
	// tournament screenName didn't score a KO in.
	IsKnockout bool
}

// ParseCashFile parses a PokerStars hand history file from the hand history
// folder. Returns a CashSession for cash game files, or TournamentHandStats
// for tournament hand history files. Exactly one of the two will be non-nil.
func ParseCashFile(path, screenName string) (session *CashSession, tourneyStats *TournamentHandStats, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var (
		firstTime, lastTime time.Time
		bigBlind            float64
		stake               string
		firstHandID         string
		hands               int
		netUSD, rakeUSD     float64
		vpipCount, pfrCount int

		isTournament bool
		tournamentID string
		bountyUSD    float64
		isKnockout   bool

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
	collectedPfx := screenName + " collected "
	uncalledPfx := "Uncalled bet ("
	uncalledSfx := ") returned to " + screenName
	bountyWinPfx := screenName + " wins $"
	const bountySeatMarker = " bounty)"

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
		case strings.HasPrefix(line, "PokerStars Hand #") || strings.HasPrefix(line, "PokerStars Zoom Hand #"):
			if strings.Contains(line, "Tournament #") {
				if !isTournament {
					isTournament = true
					if tidx := strings.Index(line, "Tournament #"); tidx >= 0 {
						rest := line[tidx+len("Tournament #"):]
						if end := strings.Index(rest, ","); end > 0 {
							tournamentID = rest[:end]
						}
					}
				}
				finalizeHand()
				handActive = true
			} else {
				finalizeHand()

				if bigBlind == 0 {
					firstHandID = extractHandID(line)

					parenIdx := strings.Index(line, "($")
					if parenIdx == -1 {
						return nil, nil, fmt.Errorf("no blinds in header: %s", line)
					}
					afterParen := line[parenIdx+2:]
					slashIdx := strings.Index(afterParen, "/")
					if slashIdx == -1 {
						return nil, nil, fmt.Errorf("no / in blinds: %s", line)
					}
					bbStr := afterParen[slashIdx+1:]
					if len(bbStr) > 0 && bbStr[0] == '$' {
						bbStr = bbStr[1:]
					}
					bb, ok := parseLeadingAmount(bbStr)
					if !ok {
						return nil, nil, fmt.Errorf("bad BB in header: %s", line)
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
			}

		case line == "*** HOLE CARDS ***":
			// Only Hold'em/Omaha deal via "*** HOLE CARDS ***", so VPIP/PFR
			// (preflop concepts) are only counted for those games. Stud/draw
			// rounds in a mixed game (HORSE, 8-Game) open with "*** 3rd STREET
			// ***" / "*** DEALING HANDS ***" instead and never set inPreflop,
			// so their hands land in the denominator (playerDealt still fires
			// on "Dealt to") but never register a VPIP/PFR action — VPIP/PFR
			// are deliberately Hold'em/Omaha-only and read as diluted (or 0
			// for an all-stud/draw sample) on mixed formats. Money fields are
			// unaffected: tournament net comes from the summary plus folded
			// bounty, not from these streets.
			inPreflop = true

		case strings.HasPrefix(line, dealtPfx):
			playerDealt = true

		case strings.HasPrefix(line, "Seat ") && strings.Contains(line, bountySeatMarker):
			// Every seat in a knockout tournament shows a bounty in its
			// pre-hand listing ("Seat 2: Name (5000 in chips, $0.36
			// bounty)"), repeated on every hand — checked regardless of
			// screenName, since screenName can finish a knockout tournament
			// without ever scoring a KO themselves (bountyUSD would then
			// stay 0, but it's still a knockout tournament).
			isKnockout = true

		case strings.HasPrefix(line, bountyWinPfx):
			// "<screenName> wins $X.XX for eliminating <Name> and their own
			// bounty increases by $Y.YY to $Z.ZZ" — the summary file has no
			// concept of bounties at all, so this is the only source for
			// knockout winnings (why two files: plan §Why Tournaments Need
			// Two Files). The amount right after "wins $" is the real cash
			// credited, which can differ by a cent from the "increases by"
			// figure due to the 50/50 split-and-round on an odd bounty.
			if amt, ok := parseLeadingAmount(line[len(bountyWinPfx):]); ok {
				bountyUSD += amt
			}

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

		case strings.HasPrefix(line, ap+"calls "):
			// Both cash ($X) and tournament (X chips) use this prefix.
			if amt, ok := parseLeadingAmount(line[len(ap+"calls "):]); ok {
				handInvested += amt
				streetTotal += amt
				if inPreflop {
					playerVPIP = true
				}
			}

		case strings.HasPrefix(line, ap+"bets "):
			if amt, ok := parseLeadingAmount(line[len(ap+"bets "):]); ok {
				handInvested += amt
				streetTotal += amt
			}

		case strings.HasPrefix(line, ap+"raises "):
			// Cash: "raises $X to $Y"; tournament: "raises X to Y [and is all-in]"
			toIdx := strings.Index(line, " to ")
			if toIdx >= 0 {
				if total, ok := parseLeadingAmount(line[toIdx+4:]); ok {
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
		return nil, nil, fmt.Errorf("scanning %s: %w", path, err)
	}
	if hands == 0 {
		return nil, nil, fmt.Errorf("no dealt hands found in %s", path)
	}

	if isTournament {
		if tournamentID == "" {
			return nil, nil, fmt.Errorf("could not extract tournament ID from %s", path)
		}
		return nil, &TournamentHandStats{
			TournamentID: tournamentID,
			VPIP:         float64(vpipCount) / float64(hands) * 100,
			PFR:          float64(pfrCount) / float64(hands) * 100,
			Hands:        hands,
			BountyUSD:    bountyUSD,
			IsKnockout:   isKnockout,
		}, nil
	}

	dur := int(lastTime.Sub(firstTime).Minutes())

	var bb100 float64
	if bigBlind > 0 {
		bb100 = (netUSD / bigBlind) / float64(hands) * 100
	}

	return &CashSession{
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
		FirstHandID: firstHandID,
	}, nil, nil
}

// extractHandID returns the digits between "Hand #" and the following ":" in
// a PokerStars hand header line (cash or Zoom — both contain "Hand #").
func extractHandID(line string) string {
	idx := strings.Index(line, "Hand #")
	if idx < 0 {
		return ""
	}
	rest := line[idx+len("Hand #"):]
	end := strings.Index(rest, ":")
	if end < 0 {
		return rest
	}
	return rest[:end]
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
