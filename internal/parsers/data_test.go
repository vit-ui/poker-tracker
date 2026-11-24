package parsers

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vit-ui/self-tracker/internal/money"
)

func fixturePath(sub, name string) string {
	return filepath.Join("..", "..", "testdata", sub, name)
}

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

func TestFixtureSummaryMoney(t *testing.T) {
	cases := []struct {
		file              string
		typ               string
		buyInCents        money.Cents
		players           int
		finishPos         int
		prizePoolUSDCents money.Cents
		satellite         bool
		reEntries         int
	}{
		{"horse_T9000000003.txt", "mtt", 110, 69, 41, 7500, false, 0},
		{"plo_T9000000004.txt", "mtt", 110, 183, 59, 8784, false, 0},
		{"draw27_pko_T9000000005.txt", "mtt", 330, 19, 0, 7150, false, 0},
		{"plo_reentry_T9000000006.txt", "mtt", 330, 162, 43, 7776, false, 2},
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
			if (got.BuyInCents - c.buyInCents) != 0 {
				t.Errorf("BuyInCents = %v, want %v", got.BuyInCents, c.buyInCents)
			}
			if got.TotalPlayers != c.players {
				t.Errorf("TotalPlayers = %d, want %d", got.TotalPlayers, c.players)
			}
			if c.finishPos != 0 && got.FinishPos != c.finishPos {
				t.Errorf("FinishPos = %d, want %d", got.FinishPos, c.finishPos)
			}
			if (got.PrizePoolCents - c.prizePoolUSDCents) != 0 {
				t.Errorf("PrizePoolCents = %v, want %v", got.PrizePoolCents, c.prizePoolUSDCents)
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
