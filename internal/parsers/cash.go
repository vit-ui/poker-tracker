package parsers

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
)

type CashSession struct {
	Date        time.Time
	Stake       string
	BuyInCents  money.Cents
	Hands       int
	DurationMin int
	NetCents    money.Cents
	RakeCents   money.Cents
	BB100       float64
	VPIP        float64
	PFR         float64

	FirstHandID string
}

type CashHand struct {
	ID            string      `json:"id"`
	At            time.Time   `json:"at"`
	NetCents      money.Cents `json:"net_cents"`
	RakeCents     money.Cents `json:"rake_cents"`
	BigBlindCents money.Cents `json:"big_blind_cents"`
	VPIP          bool        `json:"vpip,omitempty"`
	PFR           bool        `json:"pfr,omitempty"`
	LiveSource    string      `json:"live_source,omitempty"`
}

func CashHands(path, screenName string) ([]CashHand, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening cash history: %w", err)
	}
	defer f.Close()
	var hands []CashHand
	_, _, _, err, _ = parseCash(f, path, screenName, cashAgg{}, 0, false, &hands)
	return hands, err
}

func SessionFromCashHands(stake string, hands []CashHand) (CashSession, error) {
	if len(hands) == 0 {
		return CashSession{}, fmt.Errorf("cash session has no completed dealt hands")
	}
	agg := cashAgg{
		stake: stake, bigBlindCents: hands[0].BigBlindCents, firstHandID: hands[0].ID,
		firstTime: hands[0].At, lastTime: hands[0].At,
	}
	for _, hand := range hands {
		if hand.At.Before(agg.firstTime) {
			agg.firstTime = hand.At
		}
		if hand.At.After(agg.lastTime) {
			agg.lastTime = hand.At
		}
		if err := agg.addCashHand(hand); err != nil {
			return CashSession{}, err
		}
	}
	return agg.session()
}

type TournamentHandStats struct {
	TournamentID     string
	LastPlayedHandID string
	LastActivity     time.Time
	Date             time.Time
	Finish           *TournamentFinish
	SpinAndGo        bool
	VPIP             float64
	PFR              float64

	Hands int

	BountyCents money.Cents

	IsKnockout bool
}

func (s TournamentHandStats) LastObservedHandID() string {
	if s.Finish != nil {
		return s.Finish.HandID
	}
	return s.LastPlayedHandID
}

type cashAgg struct {
	firstTime, lastTime time.Time
	bigBlindCents       money.Cents
	stake               string
	firstHandID         string
	hands               int
	netCents, rakeCents money.Cents
	vpipCount, pfrCount int

	isTournament     bool
	tournamentID     string
	bountyCents      money.Cents
	isKnockout       bool
	tournamentDate   time.Time
	lastPlayedHandID string
	lastActivity     time.Time
	finish           TournamentFinish
	spinAndGo        bool
}

type ParseState struct {
	agg            cashAgg
	consumedOffset int64
}

func ParseCashFile(path, screenName string) (session *CashSession, tourneyStats *TournamentHandStats, err error) {
	session, tourneyStats, _, err = ParseCashFileFrom(path, screenName, nil)
	if err == nil && session == nil && tourneyStats == nil {
		err = fmt.Errorf("no complete dealt cash hands found in %s", path)
	}
	return session, tourneyStats, err
}

func ParseCashFileFrom(path, screenName string, prior *ParseState) (*CashSession, *TournamentHandStats, *ParseState, error) {
	if prior != nil {
		session, stats, st, err, ok := parseCashFrom(path, screenName, prior)
		if ok {
			return session, stats, st, err
		}
	}
	session, stats, st, err, _ := parseCashFrom(path, screenName, nil)
	return session, stats, st, err
}

func parseCashFrom(path, screenName string, prior *ParseState) (*CashSession, *TournamentHandStats, *ParseState, error, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening %s: %w", path, err), true
	}
	defer f.Close()

	var (
		startOffset int64
		base        cashAgg
	)
	if prior != nil {
		info, err := f.Stat()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stat %s: %w", path, err), true
		}
		if prior.consumedOffset > info.Size() {
			return nil, nil, nil, nil, false
		}
		if _, err := f.Seek(prior.consumedOffset, io.SeekStart); err != nil {
			return nil, nil, nil, nil, false
		}
		startOffset = prior.consumedOffset
		base = prior.agg
	}

	return parseCash(f, path, screenName, base, startOffset, prior != nil, nil)
}

func parseCash(in io.Reader, path, screenName string, base cashAgg, startOffset int64, resuming bool, details *[]CashHand) (*CashSession, *TournamentHandStats, *ParseState, error, bool) {
	r := bufio.NewReader(in)

	st := base
	committed := base
	newConsumedOffset := startOffset

	var (
		handID             string
		handAt             time.Time
		handActive         bool
		handHasSummary     bool
		finish             TournamentFinish
		sawUnfinishedCash  bool
		inPreflop          bool
		playerDealt        bool
		playerVPIP         bool
		playerPFR          bool
		streetTotalCents   money.Cents
		handInvestedCents  money.Cents
		handCollectedCents money.Cents
		handUncalledCents  money.Cents
		handRakeCents      money.Cents
	)

	ap := screenName + ": "
	dealtPfx := "Dealt to " + screenName + " ["
	collectedPfx := screenName + " collected "
	uncalledPfx := "Uncalled bet ("
	uncalledSfx := ") returned to " + screenName
	bountyWinPfx := screenName + " wins $"
	const bountySeatMarker = " bounty)"

	var amountErr error
	add := func(targetCents *money.Cents, valueCents money.Cents) {
		if amountErr == nil {
			*targetCents, amountErr = money.Add(*targetCents, valueCents)
		}
	}
	parseAmount := func(text string) (money.Cents, bool) {
		valueCents, ok := parseLeadingAmount(text)
		if !ok && amountErr == nil {
			amountErr = fmt.Errorf("invalid hand amount %q", text)
		}
		return valueCents, ok
	}
	finalizeHand := func() {
		if !handActive {
			return
		}
		if !st.isTournament && !handHasSummary {
			st = committed
			playerDealt = false
			sawUnfinishedCash = true
		}
		if st.isTournament {
			if playerDealt {
				st.lastPlayedHandID = finish.HandID
				st.finish = TournamentFinish{}
			}
			if handHasSummary && finish.Position > 0 {
				st.finish = finish
			}
		}
		if playerDealt {
			handNetCents, err := money.Add(handCollectedCents, handUncalledCents)
			if err == nil {
				handNetCents, err = money.Subtract(handNetCents, handInvestedCents)
			}
			hand := CashHand{ID: handID, At: handAt, NetCents: handNetCents, RakeCents: handRakeCents, BigBlindCents: st.bigBlindCents, VPIP: playerVPIP, PFR: playerPFR}
			if err == nil {
				err = st.addCashHand(hand)
			}
			if err != nil && amountErr == nil {
				amountErr = err
			}
			if details != nil && !st.isTournament {
				*details = append(*details, hand)
			}
		}

		handActive = false
		handHasSummary = false
		finish = TournamentFinish{}
		inPreflop = false
		playerDealt = false
		playerVPIP = false
		playerPFR = false
		streetTotalCents = 0
		handInvestedCents = 0
		handCollectedCents = 0
		handUncalledCents = 0
		handRakeCents = 0
	}

	offset := startOffset
	firstLine := true
	for {
		raw, readErr := r.ReadString('\n')
		if len(raw) > 0 {
			lineStart := offset
			offset += int64(len(raw))
			line := trimLineEnding(raw)
			if len(line) >= 3 && line[0] == '\xef' && line[1] == '\xbb' && line[2] == '\xbf' {
				line = line[3:]
			}

			if firstLine && resuming && !isHandHeader(line) {
				return nil, nil, nil, nil, false
			}
			firstLine = false

			switch {
			case isHandHeader(line):
				finalizeHand()
				committed = st
				newConsumedOffset = lineStart

				if strings.Contains(line, "Tournament #") {
					if !st.isTournament {
						st.isTournament = true
						st.tournamentID = handTournamentID(line)
						st.tournamentDate = handTime(line)
						st.spinAndGo = strings.Contains(strings.ToLower(line), "spin & go")
					}
					finish.HandID = extractHandID(line)
					finish.At = handTime(line)
					if finish.At.After(st.lastActivity) {
						st.lastActivity = finish.At
					}
					finish.EntryCents = tournamentEntry(line)
				} else {
					if details != nil {
						handID = extractHandID(line)
						handAt = handTime(line)
					}
					if st.bigBlindCents == 0 {
						st.firstHandID = extractHandID(line)

						parenIdx := strings.Index(line, "($")
						if parenIdx == -1 {
							return nil, nil, nil, fmt.Errorf("no blinds in header: %s", line), true
						}
						afterParen := line[parenIdx+2:]
						slashIdx := strings.Index(afterParen, "/")
						if slashIdx == -1 {
							return nil, nil, nil, fmt.Errorf("no / in blinds: %s", line), true
						}
						bbStr := afterParen[slashIdx+1:]
						if len(bbStr) > 0 && bbStr[0] == '$' {
							bbStr = bbStr[1:]
						}
						bbCents, ok := parseAmount(bbStr)
						if !ok {
							return nil, nil, nil, fmt.Errorf("bad BB in header: %s", line), true
						}
						st.bigBlindCents = bbCents
						st.stake = "NL" + strconv.FormatInt(int64(bbCents), 10)
					}

					if t := handTime(line); !t.IsZero() {
						if st.firstTime.IsZero() {
							st.firstTime = t
						}
						st.lastTime = t
					}
				}

				handActive = true

			case line == "*** HOLE CARDS ***":
				inPreflop = true

			case strings.HasPrefix(line, dealtPfx):
				playerDealt = true

			case strings.HasPrefix(line, "Seat ") && strings.Contains(line, bountySeatMarker):
				st.isKnockout = true

			case strings.HasPrefix(line, bountyWinPfx):
				if amtCents, ok := parseAmount(line[len(bountyWinPfx):]); ok {
					add(&st.bountyCents, amtCents)
				}

			case strings.HasPrefix(line, "*** FLOP ***"):
				inPreflop = false
				streetTotalCents = 0

			case strings.HasPrefix(line, "*** TURN ***"):
				streetTotalCents = 0

			case strings.HasPrefix(line, "*** RIVER ***"):
				streetTotalCents = 0

			case line == "*** SHOW DOWN ***":
				inPreflop = false

			case line == "*** SUMMARY ***":
				handHasSummary = true
				inPreflop = false

			case strings.HasPrefix(line, ap+"posts "):
				rest := line[len(ap+"posts "):]
				dollarIdx := strings.Index(rest, "$")
				if dollarIdx >= 0 {
					if amtCents, ok := parseAmount(rest[dollarIdx+1:]); ok {
						add(&handInvestedCents, amtCents)
						add(&streetTotalCents, amtCents)
					}
				}

			case strings.HasPrefix(line, ap+"calls "):
				if amtCents, ok := parseAmount(line[len(ap+"calls "):]); ok {
					add(&handInvestedCents, amtCents)
					add(&streetTotalCents, amtCents)
					if inPreflop {
						playerVPIP = true
					}
				}

			case strings.HasPrefix(line, ap+"bets "):
				if amtCents, ok := parseAmount(line[len(ap+"bets "):]); ok {
					add(&handInvestedCents, amtCents)
					add(&streetTotalCents, amtCents)
				}

			case strings.HasPrefix(line, ap+"raises "):
				toIdx := strings.Index(line, " to ")
				if toIdx >= 0 {
					if totalCents, ok := parseAmount(line[toIdx+4:]); ok {
						incrementCents := totalCents - streetTotalCents
						if incrementCents < 0 {
							incrementCents = 0
						}
						add(&handInvestedCents, incrementCents)
						streetTotalCents = totalCents
						if inPreflop {
							playerVPIP = true
							playerPFR = true
						}
					}
				}

			case st.isTournament && handActive && strings.HasPrefix(line, screenName+" ") && !strings.HasPrefix(line, collectedPfx):
				finish.readLine(strings.TrimPrefix(line, screenName+" "))

			case strings.HasPrefix(line, uncalledPfx) && strings.HasSuffix(line, uncalledSfx):
				closeIdx := strings.Index(line, ")")
				if closeIdx > len(uncalledPfx) {
					if amtCents, ok := parseAmount(line[len(uncalledPfx):closeIdx]); ok {
						add(&handUncalledCents, amtCents)
					}
				}

			case strings.HasPrefix(line, collectedPfx):
				if amtCents, ok := parseAmount(line[len(collectedPfx):]); ok {
					add(&handCollectedCents, amtCents)
				}

			case strings.HasPrefix(line, "Total pot ") && strings.Contains(line, "| Rake $"):
				rakeIdx := strings.Index(line, "| Rake $")
				if rakeCents, ok := parseAmount(line[rakeIdx+8:]); ok {
					handRakeCents = rakeCents
				}
			}
		}

		if readErr != nil {
			if readErr != io.EOF {
				return nil, nil, nil, fmt.Errorf("scanning %s: %w", path, readErr), true
			}
			break
		}
	}

	finalizeHand()

	if amountErr != nil {
		return nil, nil, nil, fmt.Errorf("accumulating hand amounts: %w", amountErr), true
	}
	newState := &ParseState{agg: committed, consumedOffset: newConsumedOffset}
	if st.hands == 0 && sawUnfinishedCash {
		return nil, nil, newState, nil, true
	}
	if st.hands == 0 && st.finish.Position == 0 {
		return nil, nil, nil, fmt.Errorf("no dealt hands found in %s", path), true
	}

	if st.isTournament {
		if st.tournamentID == "" {
			return nil, nil, nil, fmt.Errorf("could not extract tournament ID from %s", path), true
		}
		var result *TournamentFinish
		if st.finish.Position > 0 && !st.finish.At.IsZero() {
			result = &st.finish
		}
		stats := &TournamentHandStats{
			TournamentID: st.tournamentID, LastPlayedHandID: st.lastPlayedHandID, LastActivity: st.lastActivity, Date: st.tournamentDate,
			Finish: result, SpinAndGo: st.spinAndGo, Hands: st.hands, BountyCents: st.bountyCents, IsKnockout: st.isKnockout,
		}
		if st.hands > 0 {
			stats.VPIP = float64(st.vpipCount) / float64(st.hands) * 100
			stats.PFR = float64(st.pfrCount) / float64(st.hands) * 100
		}
		return nil, stats, newState, nil, true
	}

	session, err := st.session()
	if err != nil {
		return nil, nil, nil, err, true
	}
	return &session, nil, newState, nil, true
}

func (st *cashAgg) addCashHand(hand CashHand) error {
	st.hands++
	if hand.VPIP {
		st.vpipCount++
	}
	if hand.PFR {
		st.pfrCount++
	}
	var err error
	st.netCents, err = money.Add(st.netCents, hand.NetCents)
	if err != nil {
		return err
	}
	st.rakeCents, err = money.Add(st.rakeCents, hand.RakeCents)
	return err
}

func (st cashAgg) session() (CashSession, error) {
	buyInCents, err := money.Multiply(st.bigBlindCents, 100)
	if err != nil {
		return CashSession{}, err
	}
	handCount := float64(st.hands)
	return CashSession{
		Date:        st.firstTime,
		Stake:       st.stake,
		BuyInCents:  buyInCents,
		Hands:       st.hands,
		DurationMin: int(st.lastTime.Sub(st.firstTime).Minutes()),
		NetCents:    st.netCents,
		RakeCents:   st.rakeCents,
		BB100:       money.Ratio(st.netCents, st.bigBlindCents) / handCount * 100,
		VPIP:        float64(st.vpipCount) / handCount * 100,
		PFR:         float64(st.pfrCount) / handCount * 100,
		FirstHandID: st.firstHandID,
	}, nil
}

func isHandHeader(line string) bool {
	return strings.HasPrefix(line, "PokerStars Hand #") || strings.HasPrefix(line, "PokerStars Zoom Hand #")
}

func trimLineEnding(s string) string {
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}

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

func parseLeadingAmount(s string) (money.Cents, bool) {
	if len(s) > 0 && s[0] == '$' {
		s = s[1:]
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '.' || (c >= '0' && c <= '9') {
			i++
			continue
		}
		if c == ',' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return 0, false
	}
	vCents, err := money.Parse(strings.ReplaceAll(s[:i], ",", ""))
	return vCents, err == nil
}
