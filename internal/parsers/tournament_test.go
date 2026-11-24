package parsers

import (
	"os"
	"path/filepath"
	"testing"
)

const reEntrySummary = `PokerStars Tournament #9000000011, No Limit Hold'em
Buy-In: $0.90/$0.10 USD
32 players
Total Prize Pool: $28.80 USD
Tournament started 2026/07/20 16:31:04 BRT [2026/07/20 15:31:04 ET]

  1: Player2 (Brazil), still playing
  2: Player3 (Brazil), still playing
  3: Player4 (Germany), still playing
  4: Player5 (Brazil), still playing
  5: AnonHero01 [2] (Brazil), $2.07 (7.187%)
  6: Player6 (Brazil),
 32: AnonHero01 (Brazil),

You finished in 5th place (eliminated at hand #261494486121).You finished the tournament.


You made 1 re-entries for a total of $1.00.
`

const singleEntrySummary = `PokerStars Tournament #9000000012, No Limit Hold'em
Buy-In: $0.90/$0.10 USD
32 players
Total Prize Pool: $28.80 USD
Tournament started 2026/07/20 16:31:04 BRT [2026/07/20 15:31:04 ET]

  1: SomeoneElse (Brazil), still playing
  3: AnonHero01 (Brazil), $3.00 (10.41%)

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
	got, err := ParseTournamentFile(writeTempSummary(t, reEntrySummary), testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if got.FinishPos != 5 {
		t.Errorf("FinishPos = %d, want 5 (best row, not the busted 32nd)", got.FinishPos)
	}
	if got.BuyInCents != 200 {
		t.Errorf("BuyInCents = %v, want 2.00 (base 1.00 + re-entry 1.00)", got.BuyInCents)
	}
	if (got.NetCents - 7) != 0 {
		t.Errorf("NetCents = %v, want 0.07 (payout 2.07 - total buy-in 2.00)", got.NetCents)
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
	got, err := ParseTournamentFile(writeTempSummary(t, singleEntrySummary), testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if got.FinishPos != 3 {
		t.Errorf("FinishPos = %d, want 3", got.FinishPos)
	}
	if got.BuyInCents != 100 {
		t.Errorf("BuyInCents = %v, want 1.00 (no re-entry)", got.BuyInCents)
	}
	if got.ReEntries != 0 {
		t.Errorf("ReEntries = %d, want 0 (single entry)", got.ReEntries)
	}
	if (got.NetCents - 200) != 0 {
		t.Errorf("NetCents = %v, want 2.00 (payout 3.00 - buy-in 1.00)", got.NetCents)
	}
}

const satelliteBustedSummary = `PokerStars Tournament #9000000013, No Limit Hold'em
Buy-In: $0.46/$0.04 USD
3 players
Total Prize Pool: $1.00 Power Path Step 2 Ticket
Tournament started 2026/07/20 21:16:34 BRT [2026/07/20 20:16:34 ET]

  1: Player2 (Brazil), still playing
  2: Player3 (Ukraine), still playing
  3: AnonHero01 (Brazil),

You finished in 3rd place.
`

const satelliteWonSummary = `PokerStars Tournament #9000000014, No Limit Hold'em
Buy-In: $0.46/$0.04 USD
3 players
Total Prize Pool: $2.00 in Power Path Step 2 Tickets
Tournament started 2026/07/20 21:16:47 BRT [2026/07/20 20:16:47 ET]
Tournament finished 2026/07/20 21:18:24 BRT [2026/07/20 20:18:24 ET]
  1: AnonHero01 (Brazil), $1.00 Power Path Step 2 Ticket
  2: Player2 (Brazil),
  3: Player3 (Brazil),

You finished in 1st place (eliminated at hand #261496886217).
`

const rebuyAddonSummary = `PokerStars Tournament #9000000015, No Limit Hold'em
Buy-In: $0.90/$0.10 USD
40 players
Total Prize Pool: $60.00 USD
Tournament started 2026/07/21 20:00:00 BRT [2026/07/21 19:00:00 ET]

  1: SomeoneElse (Brazil), still playing
  9: AnonHero01 (Brazil),

You finished in 9th place.You finished the tournament.


You made 2 rebuys for a total of $2.00.
You made 1 addons for a total of $1.00.
`

func TestParseTournamentFileRebuyAddonAccumulation(t *testing.T) {
	got, err := ParseTournamentFile(writeTempSummary(t, rebuyAddonSummary), testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if (got.BuyInCents - 400) != 0 {
		t.Errorf("BuyInCents = %v, want 4.00 (base 1.00 + rebuys 2.00 + addon 1.00)", got.BuyInCents)
	}
	if (got.NetCents - -400) != 0 {
		t.Errorf("NetCents = %v, want -4.00 (won nothing)", got.NetCents)
	}
	if got.ReEntries != 3 {
		t.Errorf("ReEntries = %d, want 3 (2 rebuys + 1 addon summed across both lines)", got.ReEntries)
	}
}

func TestParseTournamentFileSatelliteBustedPrizePool(t *testing.T) {
	got, err := ParseTournamentFile(writeTempSummary(t, satelliteBustedSummary), testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if !got.LikelySatellite {
		t.Error("LikelySatellite = false, want true (ticket-denominated prize pool)")
	}
	if got.PrizePoolDescription != "$1.00 Power Path Step 2 Ticket" {
		t.Errorf("PrizePoolDescription = %q, want %q", got.PrizePoolDescription, "$1.00 Power Path Step 2 Ticket")
	}
	if got.PrizePoolCents != 0 {
		t.Errorf("PrizePoolCents = %v, want 0 (non-cash pool)", got.PrizePoolCents)
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
	if (got.BuyInCents - 50) != 0 {
		t.Errorf("BuyInCents = %v, want 0.50", got.BuyInCents)
	}
	if (got.NetCents - -50) != 0 {
		t.Errorf("NetCents = %v, want -0.50 (won nothing; ticket not counted as cash)", got.NetCents)
	}
}

func TestParseTournamentFileSatelliteWonPrizePool(t *testing.T) {
	got, err := ParseTournamentFile(writeTempSummary(t, satelliteWonSummary), testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if !got.LikelySatellite {
		t.Error("LikelySatellite = false, want true")
	}
	if got.PrizePoolDescription != "$2.00 in Power Path Step 2 Tickets" {
		t.Errorf("PrizePoolDescription = %q, want the pool line's text", got.PrizePoolDescription)
	}
	if got.WinnerPrizeDescription != "$1.00 Power Path Step 2 Ticket" {
		t.Errorf("WinnerPrizeDescription = %q, want %q (row-derived, not clobbered by the pool guard)", got.WinnerPrizeDescription, "$1.00 Power Path Step 2 Ticket")
	}
	if got.FinishPos != 1 {
		t.Errorf("FinishPos = %d, want 1", got.FinishPos)
	}
	if (got.NetCents - -50) != 0 {
		t.Errorf("NetCents = %v, want -0.50 (ticket win is not cash)", got.NetCents)
	}
}

func TestParseTournamentFileFreeroll(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "tournament_summary", "freeroll_zero_T9000000002.txt")
	got, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("err = %v, want nil (freeroll parses to a zero-buy-in tournament)", err)
	}
	if got.BuyInCents != 0 {
		t.Errorf("BuyInCents = %v, want 0", got.BuyInCents)
	}
	if got.NetCents != 0 {
		t.Errorf("NetCents = %v, want 0 (no buy-in, no cash payout row)", got.NetCents)
	}
	if got.TournamentID != "9000000002" {
		t.Errorf("TournamentID = %q, want 9000000002", got.TournamentID)
	}
	if got.FinishPos != 3 {
		t.Errorf("FinishPos = %d, want 3", got.FinishPos)
	}
	if got.TotalPlayers != 4 {
		t.Errorf("TotalPlayers = %d, want 4", got.TotalPlayers)
	}
	if got.Type != "mtt" {
		t.Errorf("Type = %q, want mtt (4 players)", got.Type)
	}
	if got.Multiplier != nil {
		t.Errorf("Multiplier = %v, want nil (freeroll has no multiplier)", *got.Multiplier)
	}
}

func TestParseTournamentFile(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "tournament_summary", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures in testdata/tournament_summary/ — committed anonymized summaries are expected here")
	}
	for _, f := range files {
		tournament, err := ParseTournamentFile(f, testScreenName)
		if err != nil {
			t.Errorf("ParseTournamentFile(%s): %v", filepath.Base(f), err)
			continue
		}
		t.Logf("%s: %+v", filepath.Base(f), tournament)
	}
}
