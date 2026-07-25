package parsers

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// reEntrySummary is an MTT summary (anonymized, adapted from a real one) where
// the hero re-entered once: they appear on row 5 ("[2]", cashed $2.07) and row
// 32 (the busted first entry). "You made 1 re-entries for a total of $1.00" is
// the extra cost. Correct result: finished 5th, buy-in $2.00 total, net +$0.07.
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

// singleEntrySummary is a plain single-entry MTT (no "You made" line): the
// re-entry handling must be a no-op. Buy-in $1.00, cashed $3.00 → net +$2.00.
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
	got, err := ParseTournamentFile(writeTempSummary(t, singleEntrySummary), testScreenName)
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

// satelliteBustedSummary (anonymized, adapted from a real Power Path satellite)
// is one the hero busted 3rd. PokerStars wrote the file mid-tournament, so the
// other two rows read "still playing" and the hero's own row is empty — the
// only satellite signal left is the ticket-denominated "Total Prize Pool" line.
// This is the regression guard for that case.
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

// satelliteWonSummary (anonymized, adapted from a real Power Path satellite the
// hero won outright) — position 1's row shows the ticket, so the row scan flags
// LikelySatellite and derives WinnerPrizeDescription from that row. The
// post-loop prize-pool guard must not clobber that. The pool line's text is
// deliberately altered to DIFFER from the row payout text: with identical
// strings the clobber assertion is unfalsifiable — the guard could overwrite
// the row-derived value with the same bytes and the test would still pass.
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

// rebuyAddonSummary exercises the extra-spend accumulation across MULTIPLE
// "You made … for a total of $" lines — the case arm is deliberately anchored
// on the money phrase so re-entries, rebuys, and add-ons all fold into
// BuyInUSD uniformly, and ReEntries sums every line's leading count. Only the
// single-line re-entry form exists in the file fixtures otherwise, so the
// += accumulation path had no other coverage.
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
	if math.Abs(got.BuyInUSD-4.00) > 1e-9 {
		t.Errorf("BuyInUSD = %.4f, want 4.00 (base 1.00 + rebuys 2.00 + addon 1.00)", got.BuyInUSD)
	}
	if math.Abs(got.NetUSD-(-4.00)) > 1e-9 {
		t.Errorf("NetUSD = %.4f, want -4.00 (won nothing)", got.NetUSD)
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
	// The discriminating assertion: pool text and row text differ, and the
	// row-derived one must win — a guard regression that overwrote it with
	// PrizePoolDescription fails here.
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

// TestParseTournamentFileFreeroll reads the committed anonymized freeroll
// fixture (no "Buy-In: $" line) and confirms it parses to a zero-buy-in
// tournament so the worker can ask the player for the real entry cost rather
// than dropping the bounty cash.
func TestParseTournamentFileFreeroll(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "tournament_summary", "freeroll_zero_T9000000002.txt")
	got, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("err = %v, want nil (freeroll parses to a zero-buy-in tournament)", err)
	}
	if got.BuyInUSD != 0 {
		t.Errorf("BuyInUSD = %v, want 0", got.BuyInUSD)
	}
	if got.NetUSD != 0 {
		t.Errorf("NetUSD = %v, want 0 (no buy-in, no cash payout row)", got.NetUSD)
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

// TestParseTournamentFile parses every committed tournament-summary fixture in
// testdata/tournament_summary/ (anonymized, derived from real files) and
// asserts each parses without error.
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
