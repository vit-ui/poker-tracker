package parsers

import (
	"bufio"
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

// CashQuietPeriod also defines the boundary between cash sessions in a
// PokerStars export, whose hands have no session IDs.
const CashQuietPeriod = 150 * time.Second

type scannedHand struct {
	id, group     string
	at            time.Time
	offset, bytes int64
	tournament    bool
	complete      bool
}

func handTime(header string) time.Time {
	_, tail, ok := strings.Cut(header, " - ")
	for ok {
		header = tail
		_, tail, ok = strings.Cut(header, " - ")
	}
	if len(header) >= 18 && header[12] == ':' {
		header = header[:11] + "0" + header[11:]
	}
	if len(header) < 19 {
		return time.Time{}
	}
	t, _ := time.Parse("2006/01/02 15:04:05", header[:19])
	return t
}

func handTournamentID(header string) string {
	_, tail, ok := strings.Cut(header, "Tournament #")
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(tail, ",")
	if !ok {
		return ""
	}
	return id
}

// ScanHandHistory indexes hand locations on disk, then passes each group
// through the regular parser. The contents of the export stay on disk.
func ScanHandHistory(path, screenName string, splitCash bool, visit func(*CashSession, *TournamentHandStats, error) bool) error {
	if screenName == "" {
		return fmt.Errorf("set the PokerStars screen name before scanning")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening hand history: %w", err)
	}
	defer f.Close()
	groups, skipped, err := indexScanHands(f)
	if err != nil {
		return err
	}
	if skipped > 0 {
		if !visit(nil, nil, fmt.Errorf("%d incomplete, unidentified or repeated hands skipped", skipped)) {
			return nil
		}
	}
	if len(groups) == 0 {
		return fmt.Errorf("no complete PokerStars hands found")
	}
	// Preserve normal folder-scan boundaries for traditional per-table files.
	splitCash = splitCash || len(groups) > 1
	var sessions [][]scannedHand
	for _, hands := range groups {
		slices.SortFunc(hands, compareScannedHands)
		start := 0
		if splitCash && !hands[0].tournament {
			for i := 1; i < len(hands); i++ {
				if hands[i].at.Sub(hands[i-1].at) >= CashQuietPeriod {
					sessions = append(sessions, hands[start:i])
					start = i
				}
			}
		}
		sessions = append(sessions, hands[start:])
	}
	slices.SortFunc(sessions, func(a, b []scannedHand) int { return compareScannedHands(a[0], b[0]) })
	for _, hands := range sessions {
		readers := make([]io.Reader, 0, 2*len(hands))
		for _, hand := range hands {
			readers = append(readers, io.NewSectionReader(f, hand.offset, hand.bytes), strings.NewReader("\n"))
		}
		session, tournament, _, err, _ := parseCash(io.MultiReader(readers...), "scanned hand group", screenName, cashAgg{}, 0, false)
		if !visit(session, tournament, err) {
			return nil
		}
	}
	return nil
}

func compareScannedHands(a, b scannedHand) int {
	if order := a.at.Compare(b.at); order != 0 {
		return order
	}
	// PokerStars IDs can have different lengths. Within a second they sort numerically.
	if order := cmp.Compare(len(a.id), len(b.id)); order != 0 {
		return order
	}
	return cmp.Compare(a.id, b.id)
}

func indexScanHands(r io.Reader) (map[string][]scannedHand, int, error) {
	lines := bufio.NewReader(r)
	groups := make(map[string][]scannedHand)
	seen := make(map[string]bool)
	var hand scannedHand
	var active bool
	var offset int64
	var skipped int
	finish := func(end int64) {
		if !active {
			return
		}
		if hand.id == "" || hand.group == "" || hand.at.IsZero() || !hand.complete || seen[hand.id] {
			skipped++
			return
		}
		seen[hand.id] = true
		hand.bytes = end - hand.offset
		groups[hand.group] = append(groups[hand.group], hand)
	}
	for {
		raw, err := lines.ReadString('\n')
		line := strings.TrimPrefix(trimLineEnding(raw), "\ufeff")
		switch {
		case isHandHeader(line):
			finish(offset)
			hand = scannedHand{id: extractHandID(line), at: handTime(line), offset: offset, tournament: strings.Contains(line, "Tournament #")}
			if hand.tournament {
				if id := handTournamentID(line); id != "" {
					hand.group = "tournament:" + id
				}
			} else if _, limits, ok := strings.Cut(line, "($"); ok {
				stakes, _, ok := strings.Cut(limits, ")")
				if ok {
					hand.group = "cash:" + strings.TrimSuffix(stakes, " USD")
				}
			}
			active = true
		case active && !hand.tournament && strings.HasPrefix(line, "Table '"):
			if end := strings.LastIndex(line, "' "); end > len("Table '") && hand.group != "" {
				hand.group += "\x00" + line[len("Table '"):end]
			}
		case active && line == "*** SUMMARY ***":
			hand.complete = hand.tournament || strings.Contains(hand.group, "\x00")
		}
		offset += int64(len(raw))
		if err == io.EOF {
			finish(offset)
			return groups, skipped, nil
		}
		if err != nil {
			return nil, 0, fmt.Errorf("reading hand history: %w", err)
		}
	}
}
