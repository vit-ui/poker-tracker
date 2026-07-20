package parsers

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// reEntrySummary is a real PokerStars MTT summary where screenName (Mayor2003)
// re-entered once: they appear on row 5 ("[2]", cashed $2.07) and row 32 (the
// busted first entry). "You made 1 re-entries for a total of $1.00" is the
// extra cost. Correct result: finished 5th, buy-in $2.00 total, net +$0.07.
const reEntrySummary = `PokerStars Tournament #4017504585, No Limit Hold'em
Buy-In: $0.90/$0.10 USD
32 players
Total Prize Pool: $28.80 USD
Tournament started 2026/07/20 16:31:04 BRT [2026/07/20 15:31:04 ET]

  1: ADLBAL (Brazil), still playing
  2: GAOLBO52 (Brazil), still playing
  3: RobertWa1860 (Germany), still playing
  4: nilisss17 (Brazil), still playing
  5: Mayor2003 [2] (Brazil), $2.07 (7.187%)
  6: Guigz_CS (Brazil),
 32: Mayor2003 (Brazil),

You finished in 5th place (eliminated at hand #261494486121).You finished the tournament.


You made 1 re-entries for a total of $1.00.
`

// singleEntrySummary is a plain single-entry MTT (no "You made" line): the
// re-entry handling must be a no-op. Buy-in $1.00, cashed $3.00 → net +$2.00.
const singleEntrySummary = `PokerStars Tournament #4017504999, No Limit Hold'em
Buy-In: $0.90/$0.10 USD
32 players
Total Prize Pool: $28.80 USD
Tournament started 2026/07/20 16:31:04 BRT [2026/07/20 15:31:04 ET]

  1: SomeoneElse (Brazil), still playing
  3: Mayor2003 (Brazil), $3.00 (10.41%)

You finished in 3rd place.You finished the tournament.
`

func writeTempSummary(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "summary.txt")
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseTournamentFileReEntry(t *testing.T) {
	got, err := ParseTournamentFile(writeTempSummary(t, reEntrySummary), "Mayor2003")
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if got.FinishPos != 5 {
		t.Errorf("FinishPos = %d, want 5 (best row, not the busted 32nd)", got.FinishPos)
	}
	if got.BuyInUSD != 2.00 {
		t.Errorf("BuyInUSD = %.2f, want 2.00 (base 1.00 + re-entry 1.00)", got.BuyInUSD)
	}
	if math.Abs(got.NetUSD-0.07) > 1e-9 {
		t.Errorf("NetUSD = %.4f, want 0.07 (payout 2.07 - total buy-in 2.00)", got.NetUSD)
	}
	if got.Type != "mtt" {
		t.Errorf("Type = %q, want \"mtt\"", got.Type)
	}
	if got.TotalPlayers != 32 {
		t.Errorf("TotalPlayers = %d, want 32", got.TotalPlayers)
	}
}

func TestParseTournamentFileSingleEntryUnchanged(t *testing.T) {
	got, err := ParseTournamentFile(writeTempSummary(t, singleEntrySummary), "Mayor2003")
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if got.FinishPos != 3 {
		t.Errorf("FinishPos = %d, want 3", got.FinishPos)
	}
	if got.BuyInUSD != 1.00 {
		t.Errorf("BuyInUSD = %.2f, want 1.00 (no re-entry)", got.BuyInUSD)
	}
	if math.Abs(got.NetUSD-2.00) > 1e-9 {
		t.Errorf("NetUSD = %.4f, want 2.00 (payout 3.00 - buy-in 1.00)", got.NetUSD)
	}
}

func TestParseTournamentFile(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "tournaments", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no files found in testdata/tournaments/ — this directory is gitignored real player data, only present on a dev machine that's played tournaments; skipping on a checkout without it")
	}
	for _, f := range files {
		tournament, err := ParseTournamentFile(f, testScreenName)
		if errors.Is(err, ErrFreeroll) {
			continue
		}
		if err != nil {
			t.Errorf("ParseTournamentFile(%s): %v", filepath.Base(f), err)
			continue
		}
		t.Logf("%s: %+v", filepath.Base(f), tournament)
	}
}
