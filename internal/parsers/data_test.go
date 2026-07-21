package parsers

import (
	"bufio"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dataScreenName is the real nick in the gitignored data/ fixtures (the
// player's own hand histories and tournament summaries, one file per real
// session across every format they play).
const dataScreenName = "Mayor2003"

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

// TestParseDataFiles parses every real fixture in data/ (gitignored,
// dev-machine-only) and asserts none fail to parse — summaries via
// ParseTournamentFile (freerolls skipped), hand histories via ParseCashFile.
// This is the broad "every format I play parses without error" sweep across
// NLHE, PLO, HORSE, 8-Game, Razz, 2-7 draw, All-In Poker cash, and PKO files.
func TestParseDataFiles(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "data", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no files in data/ — gitignored real player fixtures, only present on the dev machine")
	}

	var summaries, cashHH, tourneyHH int
	for _, f := range files {
		base := filepath.Base(f)
		switch line := firstLine(t, f); {
		case strings.HasPrefix(line, "PokerStars Tournament #"):
			summaries++
			if _, err := ParseTournamentFile(f, dataScreenName); err != nil && !errors.Is(err, ErrFreeroll) {
				t.Errorf("ParseTournamentFile(%s): %v", base, err)
			}
		case strings.HasPrefix(line, "PokerStars Hand #"), strings.HasPrefix(line, "PokerStars Zoom Hand #"):
			session, tourneyStats, err := ParseCashFile(f, dataScreenName)
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

// findData returns the data/ file whose basename starts with prefix ("TS" for
// a summary, "HH" for a hand history) and contains sub — the tournament ID is
// shared between a tournament's summary and its hand history, so the prefix
// disambiguates. Skips the whole test if data/ is absent.
func findData(t *testing.T, prefix, sub string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "data", prefix+"*"+sub+"*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skipf("no data/ %s file matching %q (gitignored fixtures absent)", prefix, sub)
	}
	return files[0]
}

// TestParseDataSummaryMoney spot-checks exact money/finish values on one
// representative summary per non-NLHE format, proving the game type is
// irrelevant to the money fields.
func TestParseDataSummaryMoney(t *testing.T) {
	cases := []struct {
		sub          string
		typ          string
		buyIn        float64
		players      int
		finishPos    int
		prizePoolUSD float64
		satellite    bool
	}{
		{"T4014978222 HORSE", "mtt", 1.10, 69, 41, 75.00, false},
		{"T4014977703 Pot Limit Omaha", "mtt", 1.10, 183, 59, 87.84, false},
		// PKO 2-7 draw: "Buy-In: $2.94/$0.36" — the $2.94 half already sums
		// the base buy-in and the bounty, so BuyInUSD is the full 3.30.
		{"T4016701188 No Limit 2-7 Single Draw", "mtt", 3.30, 19, 0, 71.50, false},
	}
	for _, c := range cases {
		t.Run(c.sub, func(t *testing.T) {
			got, err := ParseTournamentFile(findData(t, "TS", c.sub), dataScreenName)
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
		})
	}
}

// TestParseDataHandHistoryFormats spot-checks that the exotic hand-history
// formats classify correctly: a PKO draw tournament flags IsKnockout, and the
// All-In Poker cash table parses as a cash session with the right stake.
func TestParseDataHandHistoryFormats(t *testing.T) {
	t.Run("2-7 draw PKO is knockout", func(t *testing.T) {
		session, stats, err := ParseCashFile(findData(t, "HH", "T4016701188 No Limit 2-7 Single Draw"), dataScreenName)
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
		session, stats, err := ParseCashFile(findData(t, "HH", "No Limit All-In Poker"), dataScreenName)
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
