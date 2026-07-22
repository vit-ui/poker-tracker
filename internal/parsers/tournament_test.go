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
	if got.ReEntries != 1 {
		t.Errorf("ReEntries = %d, want 1", got.ReEntries)
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
	if got.ReEntries != 0 {
		t.Errorf("ReEntries = %d, want 0 (single entry)", got.ReEntries)
	}
	if math.Abs(got.NetUSD-2.00) > 1e-9 {
		t.Errorf("NetUSD = %.4f, want 2.00 (payout 3.00 - buy-in 1.00)", got.NetUSD)
	}
}

// satelliteBustedSummary is the real #4017575047 Power Path satellite where
// Mayor2003 busted 3rd. PokerStars wrote the file mid-tournament, so the other
// two rows read "still playing" and Mayor2003's own row is empty — the only
// satellite signal left is the ticket-denominated "Total Prize Pool" line.
// This is the regression guard for that case.
const satelliteBustedSummary = `PokerStars Tournament #4017575047, No Limit Hold'em
Buy-In: $0.46/$0.04 USD
3 players
Total Prize Pool: $1.00 Power Path Step 2 Ticket
Tournament started 2026/07/20 21:16:34 BRT [2026/07/20 20:16:34 ET]

  1: CuspedeGelo (Brazil), still playing
  2: HeimaS (Ukraine), still playing
  3: Mayor2003 (Brazil),

You finished in 3rd place.
`

// satelliteWonSummary is the real #4017575075 Power Path satellite where
// Mayor2003 won the ticket outright — position 1's row shows it, so the row
// scan flags LikelySatellite and derives WinnerPrizeDescription from that row.
// The post-loop prize-pool guard must not clobber that.
const satelliteWonSummary = `PokerStars Tournament #4017575075, No Limit Hold'em
Buy-In: $0.46/$0.04 USD
3 players
Total Prize Pool: $1.00 Power Path Step 2 Ticket
Tournament started 2026/07/20 21:16:47 BRT [2026/07/20 20:16:47 ET]
Tournament finished 2026/07/20 21:18:24 BRT [2026/07/20 20:18:24 ET]
  1: Mayor2003 (Brazil), $1.00 Power Path Step 2 Ticket
  2: TomasStark (Brazil),
  3: bugaAA634 (Brazil),

You finished in 1st place (eliminated at hand #261496886217).
`

// freerollSummary is the real #4017348137 freeroll (no "Buy-In: $" line) —
// ParseTournamentFile must return ErrFreeroll so the caller skips it.
const freerollSummary = `PokerStars Tournament #4017348137, No Limit Hold'em
Freeroll
4 players
Tournament started 2026/07/19 21:53:39 BRT [2026/07/19 20:53:39 ET]

  1: AAkriss (Panama), still playing
  2: lukitasrione (Argentina), still playing
  3: Mayor2003 (Brazil),
  4: BigDegs99 (United Kingdom),

You finished in 3rd place (eliminated at hand #261487659541).
`

func TestParseTournamentFileSatelliteBustedPrizePool(t *testing.T) {
	got, err := ParseTournamentFile(writeTempSummary(t, satelliteBustedSummary), "Mayor2003")
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if !got.LikelySatellite {
		t.Error("LikelySatellite = false, want true (ticket-denominated prize pool)")
	}
	if got.PrizePoolDescription != "$1.00 Power Path Step 2 Ticket" {
		t.Errorf("PrizePoolDescription = %q, want %q", got.PrizePoolDescription, "$1.00 Power Path Step 2 Ticket")
	}
	if got.PrizePoolUSD != 0 {
		t.Errorf("PrizePoolUSD = %.2f, want 0 (non-cash pool)", got.PrizePoolUSD)
	}
	if got.Multiplier != nil {
		t.Errorf("Multiplier = %v, want nil (non-cash pool)", *got.Multiplier)
	}
	if got.Type != "spingo" {
		t.Errorf("Type = %q, want \"spingo\"", got.Type)
	}
	if got.FinishPos != 3 {
		t.Errorf("FinishPos = %d, want 3", got.FinishPos)
	}
	if math.Abs(got.BuyInUSD-0.50) > 1e-9 {
		t.Errorf("BuyInUSD = %.4f, want 0.50", got.BuyInUSD)
	}
	if math.Abs(got.NetUSD-(-0.50)) > 1e-9 {
		t.Errorf("NetUSD = %.4f, want -0.50 (won nothing; ticket not counted as cash)", got.NetUSD)
	}
}

func TestParseTournamentFileSatelliteWonPrizePool(t *testing.T) {
	got, err := ParseTournamentFile(writeTempSummary(t, satelliteWonSummary), "Mayor2003")
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if !got.LikelySatellite {
		t.Error("LikelySatellite = false, want true")
	}
	if got.WinnerPrizeDescription != "$1.00 Power Path Step 2 Ticket" {
		t.Errorf("WinnerPrizeDescription = %q, want %q (row-derived, not clobbered by the pool guard)", got.WinnerPrizeDescription, "$1.00 Power Path Step 2 Ticket")
	}
	if got.FinishPos != 1 {
		t.Errorf("FinishPos = %d, want 1", got.FinishPos)
	}
	if math.Abs(got.NetUSD-(-0.50)) > 1e-9 {
		t.Errorf("NetUSD = %.4f, want -0.50 (ticket win is not cash)", got.NetUSD)
	}
}

func TestParseTournamentFileFreeroll(t *testing.T) {
	_, err := ParseTournamentFile(writeTempSummary(t, freerollSummary), "Mayor2003")
	if !errors.Is(err, ErrFreeroll) {
		t.Errorf("err = %v, want ErrFreeroll", err)
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
