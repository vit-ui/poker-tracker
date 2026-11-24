package parsers

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
)

const KnockoutSuffix = "-ko"

const (
	TournamentSourceSummary   = "summary"
	TournamentSourceHistory   = "hand_history"
	TournamentSourceConfirmed = "confirmed_hand_history"
)

func (t Tournament) Recovered() bool {
	return t.ResultSource == TournamentSourceHistory || t.ResultSource == TournamentSourceConfirmed
}

type Tournament struct {
	Date           time.Time
	Type           string
	TournamentID   string
	ResultSource   string `json:",omitempty"`
	ResultHandID   string `json:",omitempty"`
	BuyInCents     money.Cents
	PrizePoolCents money.Cents

	PrizePoolDescription string
	Multiplier           *float64
	FinishPos            int
	TotalPlayers         int
	NetCents             money.Cents

	ReEntries int
	VPIP      *float64
	PFR       *float64
	Hands     *int

	LikelySatellite bool

	WinnerPrizeDescription string
}

func (t Tournament) BaseType() string {
	return strings.TrimSuffix(t.Type, KnockoutSuffix)
}

func ParseTournamentFile(path, screenName string) (Tournament, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Tournament{}, fmt.Errorf("reading %s: %w", path, err)
	}

	return parseTournamentSummary(data, screenName)
}

func parseTournamentSummary(data []byte, screenName string) (Tournament, error) {
	var err error
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 {
		l := lines[0]
		if len(l) >= 3 && l[0] == '\xef' && l[1] == '\xbb' && l[2] == '\xbf' {
			lines[0] = l[3:]
		}
	}

	var (
		t                Tournament
		playerCount      int
		beforeBuyIn      bool
		foundPlayerRow   bool
		reEntryCostCents money.Cents
		reEntryCount     int
	)
	playerParen := ": " + screenName + " ("
	playerBracket := ": " + screenName + " ["

	t.ResultSource = TournamentSourceSummary
	var amountErr error
	parseAmount := func(text string) (money.Cents, bool) {
		valueCents, ok := parseLeadingAmount(text)
		if !ok && amountErr == nil {
			amountErr = fmt.Errorf("invalid tournament amount %q", text)
		}
		return valueCents, ok
	}

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
				prizeCents, ok1 := parseAmount(rest[:slashIdx])
				rakeCents, ok2 := parseAmount(rest[slashIdx+1:])
				if ok1 && ok2 {
					t.BuyInCents, err = money.Add(prizeCents, rakeCents)
					if err != nil {
						return Tournament{}, err
					}
				}
			}

		case strings.HasPrefix(line, "Total Prize Pool: $"):
			rest := strings.TrimRight(line[len("Total Prize Pool: $"):], " \r")
			if isNonCashPrize(rest) {
				t.PrizePoolDescription = "$" + rest
			} else if amtCents, ok := parseAmount(rest); ok {
				t.PrizePoolCents = amtCents
			}

		case strings.HasPrefix(line, "Tournament started "):
			ts := line[len("Tournament started "):]
			if len(ts) >= 18 && ts[12] == ':' {
				ts = ts[:11] + "0" + ts[11:]
			}
			if len(ts) >= 19 {
				if parsed, err := time.Parse("2006/01/02 15:04:05", ts[:19]); err == nil {
					t.Date = parsed
				}
			}

		case strings.HasPrefix(line, "You made ") && strings.Contains(line, "for a total of $"):
			rest := line[strings.Index(line, "for a total of $")+len("for a total of $"):]
			rest = strings.TrimRight(rest, ". \r")
			if amtCents, ok := parseAmount(rest); ok {
				reEntryCostCents, err = money.Add(reEntryCostCents, amtCents)
				if err != nil {
					return Tournament{}, err
				}
			}
			if fields := strings.Fields(line[len("You made "):]); len(fields) > 0 {
				if count, err := strconv.Atoi(fields[0]); err == nil {
					reEntryCount += count
				}
			}

		case !foundPlayerRow && (strings.Contains(line, playerParen) || strings.Contains(line, playerBracket)):
			foundPlayerRow = true
			trimmed := strings.TrimLeft(line, " ")
			colonIdx := strings.Index(trimmed, ":")
			if colonIdx > 0 {
				if pos, err := strconv.Atoi(strings.TrimSpace(trimmed[:colonIdx])); err == nil {
					t.FinishPos = pos
				}
			}
			const payoutTextMarker = "), "
			t.NetCents = -t.BuyInCents
			if payIdx := strings.Index(line, payoutTextMarker); payIdx >= 0 {
				payoutText := strings.TrimRight(line[payIdx+len(payoutTextMarker):], " \r")
				if payoutText != "" && !isNonCashPrize(payoutText) {
					if payoutCents, ok := parseAmount(payoutText); ok {
						t.NetCents, err = money.Subtract(payoutCents, t.BuyInCents)
						if err != nil {
							return Tournament{}, err
						}
					}
				}
			}
		}

		if !t.LikelySatellite && beforeBuyIn && isNonCashPrize(line) {
			t.LikelySatellite = true
			t.WinnerPrizeDescription = line
		}

		if !t.LikelySatellite {
			if _, payout, ok := parseFinishLine(line); ok && isNonCashPrize(payout) {
				t.LikelySatellite = true
				t.WinnerPrizeDescription = payout
			}
		}
	}

	t.BuyInCents, err = money.Add(t.BuyInCents, reEntryCostCents)
	if err != nil {
		return Tournament{}, err
	}
	t.NetCents, err = money.Subtract(t.NetCents, reEntryCostCents)
	if err != nil {
		return Tournament{}, err
	}
	t.ReEntries = reEntryCount

	t.TotalPlayers = playerCount

	if playerCount == 3 {
		t.Type = "spingo"
		if t.PrizePoolDescription == "" && t.BuyInCents != 0 {
			m := money.Ratio(t.PrizePoolCents, t.BuyInCents)
			t.Multiplier = &m
		}
	} else {
		t.Type = "mtt"
	}

	if !t.LikelySatellite && t.PrizePoolDescription != "" {
		t.LikelySatellite = true
		t.WinnerPrizeDescription = t.PrizePoolDescription
	}

	if amountErr != nil {
		return Tournament{}, amountErr
	}
	return t, nil
}

func isNonCashPrize(payout string) bool {
	lower := strings.ToLower(payout)
	for _, word := range []string{"ticket", "pass", "seat", "package", "t$", "satellite", "sat", "qualifier", "qualy", "qually"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

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
