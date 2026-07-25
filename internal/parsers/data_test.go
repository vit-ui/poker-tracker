package parsers

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixturePath returns the path to a committed anonymized fixture — sub is
// "hand_history" or "tournament_summary".
func fixturePath(sub, name string) string {
	return filepath.Join("..", "..", "testdata", sub, name)
}

// firstLine returns the first line of a file with any UTF-8 BOM stripped.
func firstLine(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Base(path), err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return ""
	}
	line := sc.Text()
	if len(line) >= 3 && line[0] == '\xef' && line[1] == '\xbb' && line[2] == '\xbf' {
		line = line[3:]
	}
	return line
}

// TestFixtureSummaryMoney spot-checks exact money/finish values on one
// committed summary fixture per non-NLHE format, proving the game type is
// irrelevant to the money fields. The fixtures are anonymized copies of real
// summaries (see testdata/tournament_summary/), so the values are real.
func TestFixtureSummaryMoney(t *testing.T) {
	cases := []struct {
		file         string
		typ          string
		buyIn        float64
		players      int
		finishPos    int
		prizePoolUSD float64
		satellite    bool
		reEntries    int
	}{
		{"horse_T9000000003.txt", "mtt", 1.10, 69, 41, 75.00, false, 0},
		{"plo_T9000000004.txt", "mtt", 1.10, 183, 59, 87.84, false, 0},
		// PKO 2-7 draw: "Buy-In: $2.94/$0.36" — the $2.94 half already sums
		// the base buy-in and the bounty, so BuyInUSD is the full 3.30.
		{"draw27_pko_T9000000005.txt", "mtt", 3.30, 19, 0, 71.50, false, 0},
		// PLO re-entry: the hero played 3 entries, busted 43rd of 162, $2.20
		// across 2 re-entries on top of the $1.10 base.
		{"plo_reentry_T9000000006.txt", "mtt", 3.30, 162, 43, 77.76, false, 2},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			got, err := ParseTournamentFile(fixturePath("tournament_summary", c.file), testScreenName)
			if err != nil {
				t.Fatalf("ParseTournamentFile: %v", err)
			}
			if got.Type != c.typ {
				t.Errorf("Type = %q, want %q", got.Type, c.typ)
			}
			if math.Abs(got.BuyInUSD-c.buyIn) > 1e-9 {
				t.Errorf("BuyInUSD = %.4f, want %.2f", got.BuyInUSD, c.buyIn)
			}
			if got.TotalPlayers != c.players {
				t.Errorf("TotalPlayers = %d, want %d", got.TotalPlayers, c.players)
			}
			if c.finishPos != 0 && got.FinishPos != c.finishPos {
				t.Errorf("FinishPos = %d, want %d", got.FinishPos, c.finishPos)
			}
			if math.Abs(got.PrizePoolUSD-c.prizePoolUSD) > 1e-9 {
				t.Errorf("PrizePoolUSD = %.4f, want %.2f", got.PrizePoolUSD, c.prizePoolUSD)
			}
			if got.LikelySatellite != c.satellite {
				t.Errorf("LikelySatellite = %v, want %v", got.LikelySatellite, c.satellite)
			}
			if got.ReEntries != c.reEntries {
				t.Errorf("ReEntries = %d, want %d", got.ReEntries, c.reEntries)
			}
		})
	}
}

// TestFixtureHandHistoryFormats spot-checks that the exotic hand-history
// formats classify correctly: a PKO draw tournament flags IsKnockout, and the
// All-In Poker cash table parses as a cash session with the right stake.
func TestFixtureHandHistoryFormats(t *testing.T) {
	t.Run("2-7 draw PKO is knockout", func(t *testing.T) {
		session, stats, err := ParseCashFile(fixturePath("hand_history", "draw27_pko_T9000000005.txt"), testScreenName)
		if err != nil {
			t.Fatalf("ParseCashFile: %v", err)
		}
		if session != nil || stats == nil {
			t.Fatalf("want TournamentHandStats, got session=%v stats=%v", session, stats)
		}
		if !stats.IsKnockout {
			t.Error("IsKnockout = false, want true (seats show a $ bounty)")
		}
	})

	t.Run("All-In Poker cash", func(t *testing.T) {
		session, stats, err := ParseCashFile(fixturePath("hand_history", "allin_poker_cash.txt"), testScreenName)
		if err != nil {
			t.Fatalf("ParseCashFile: %v", err)
		}
		if stats != nil || session == nil {
			t.Fatalf("want CashSession, got session=%v stats=%v", session, stats)
		}
		if session.Stake != "NL20" {
			t.Errorf("Stake = %q, want \"NL20\" ($0.10/$0.20)", session.Stake)
		}
	})
}

// detectHero returns the screen name PokerStars wrote in the file's first
// "Dealt to <name>" line — always the hero in a hand history. Empty if none.
// Lets the dev-only sweep parse real hand histories without naming the real
// player anywhere in committed source.
func detectHero(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Base(path), err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	const marker = "Dealt to "
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, marker); i >= 0 {
			rest := line[i+len(marker):]
			if b := strings.LastIndex(rest, " ["); b > 0 {
				return rest[:b]
			}
			return rest
		}
	}
	return ""
}

// TestParseDataFiles is a dev-machine-only smoke test: it parses every raw file
// in data/ (gitignored real hand histories/summaries, present only where the
// player keeps them) and asserts none fail to parse. It skips when data/ is
// absent (CI, a fresh checkout). Hand histories are parsed for their own hero
// (auto-detected via detectHero, so the real nick never appears in source);
// summaries parse without needing the nick.
func TestParseDataFiles(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "data", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no files in data/ — gitignored raw real fixtures, only present on the dev machine")
	}

	var summaries, cashHH, tourneyHH int
	for _, f := range files {
		base := filepath.Base(f)
		switch line := firstLine(t, f); {
		case strings.HasPrefix(line, "PokerStars Tournament #"):
			summaries++
			if _, err := ParseTournamentFile(f, testScreenName); err != nil {
				t.Errorf("ParseTournamentFile(%s): %v", base, err)
			}
		case strings.HasPrefix(line, "PokerStars Hand #"), strings.HasPrefix(line, "PokerStars Zoom Hand #"):
			session, tourneyStats, err := ParseCashFile(f, detectHero(t, f))
			if err != nil {
				t.Errorf("ParseCashFile(%s): %v", base, err)
				continue
			}
			switch {
			case session != nil && tourneyStats == nil:
				cashHH++
			case session == nil && tourneyStats != nil:
				tourneyHH++
			default:
				t.Errorf("ParseCashFile(%s): want exactly one of session/tourneyStats", base)
			}
		default:
			t.Errorf("%s: unrecognized first line %q", base, line)
		}
	}
	t.Logf("parsed %d summaries, %d cash HH, %d tournament HH", summaries, cashHH, tourneyHH)
}
