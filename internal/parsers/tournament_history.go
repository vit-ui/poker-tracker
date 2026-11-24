package parsers

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
)

type TournamentFinish struct {
	Position       int
	HandID         string
	EntryCents     *money.Cents
	TotalCostCents *money.Cents
	PayoutCents    *money.Cents
	At             time.Time
}

var (
	handFinish    = regexp.MustCompile(`^finished the tournament in ([0-9]+)(?:st|nd|rd|th) place\b(.*)$`)
	handPayout    = regexp.MustCompile(`^(?:and )?received \$([0-9][0-9,]*(?:\.[0-9]+)?)(?: USD)?\.?$`)
	handTotalCost = regexp.MustCompile(`^paid a total of \$([0-9][0-9,]*(?:\.[0-9]+)?)(?: USD)? (?:to enter|in entry fees)\.?$`)
	handEntry     = regexp.MustCompile(`^\$([0-9][0-9,]*(?:\.[0-9]+)?(?:\+\$?[0-9][0-9,]*(?:\.[0-9]+)?)*) USD\b`)
)

func (f *TournamentFinish) readLine(line string) {
	switch {
	case strings.HasPrefix(line, "wins the tournament"):
		f.Position = 1
	default:
		if match := handFinish.FindStringSubmatch(line); match != nil {
			f.Position, _ = strconv.Atoi(match[1])
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(match[2]), ","))
		}
	}
	if match := handPayout.FindStringSubmatch(line); match != nil {
		f.PayoutCents = tournamentAmount(match[1])
	}
	if match := handTotalCost.FindStringSubmatch(line); match != nil {
		f.TotalCostCents = tournamentAmount(match[1])
	}
}

func tournamentAmount(text string) *money.Cents {
	cents, err := money.Parse(strings.ReplaceAll(text, ",", ""))
	if err != nil {
		return nil
	}
	return &cents
}

func tournamentEntry(header string) *money.Cents {
	_, tournament, _ := strings.Cut(header, "Tournament #")
	_, description, _ := strings.Cut(tournament, ", ")
	match := handEntry.FindStringSubmatch(description)
	if match == nil {
		return nil
	}
	var totalCents money.Cents
	for _, component := range strings.Split(match[1], "+") {
		valueCents := tournamentAmount(strings.TrimPrefix(component, "$"))
		if valueCents == nil {
			return nil
		}
		var err error
		totalCents, err = money.Add(totalCents, *valueCents)
		if err != nil {
			return nil
		}
	}
	return &totalCents
}
