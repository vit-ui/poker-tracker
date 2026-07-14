package parsers

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestParseFinishLine(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantPos    int
		wantPayout string
		wantOk     bool
	}{
		{"normal winner line", "  1: AnonHero01 (Cyprus), $6.00", 1, "$6.00", true},
		{"ticket payout", "  1: Villain1 (Brazil), $10.00 Sunday Special Ticket", 1, "$10.00 Sunday Special Ticket", true},
		{"no marker at all", "  1: AnonHero01 busted", 0, "", false},
		{"no colon before marker", "not a results line), $5.00", 0, "", false},
		{"non-numeric position", "  x: AnonHero01 (Cyprus), $6.00", 0, "", false},
		{"trailing carriage return trimmed", "  2: Hero (USA), $3.00\r", 2, "$3.00", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pos, payout, ok := parseFinishLine(c.input)
			if ok != c.wantOk || pos != c.wantPos || payout != c.wantPayout {
				t.Errorf("parseFinishLine(%q) = (%d, %q, %v), want (%d, %q, %v)",
					c.input, pos, payout, ok, c.wantPos, c.wantPayout, c.wantOk)
			}
		})
	}
}

func writeTournamentSummary(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "summary.txt")
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("writing tournament summary: %v", err)
	}
	return path
}

// TestParseTournamentFile_SpinGoWin covers a 3-player Spin & Go where hero
// wins — every field is hand-checked, including the Multiplier computation.
func TestParseTournamentFile_SpinGoWin(t *testing.T) {
	contents := `PokerStars Tournament #123456789, $1.00+$0.10 USD
Hold'em No Limit
Buy-In: $1.00/$0.10
3 players
Total Prize Pool: $3.00
Tournament started 2025/01/15 9:27:41 ET
  1: AnonHero01 (Cyprus), $6.00
  2: Villain1 (Brazil), $0.00
  3: Villain2 (USA), $0.00
Tournament finished 2025/01/15 9:35:12 ET
`
	path := writeTournamentSummary(t, contents)
	tourn, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}

	if tourn.TournamentID != "123456789" {
		t.Errorf("TournamentID = %q, want 123456789", tourn.TournamentID)
	}
	if tourn.Type != "spingo" {
		t.Errorf("Type = %q, want spingo", tourn.Type)
	}
	if !approxEqual(tourn.BuyInUSD, 1.10) {
		t.Errorf("BuyInUSD = %v, want 1.10", tourn.BuyInUSD)
	}
	if !approxEqual(tourn.PrizePoolUSD, 3.00) {
		t.Errorf("PrizePoolUSD = %v, want 3.00", tourn.PrizePoolUSD)
	}
	if tourn.Multiplier == nil {
		t.Fatal("expected non-nil Multiplier for a 3-player game")
	}
	wantMult := 3.00 / 1.10
	if !approxEqual(*tourn.Multiplier, wantMult) {
		t.Errorf("Multiplier = %v, want %v", *tourn.Multiplier, wantMult)
	}
	if tourn.FinishPos != 1 {
		t.Errorf("FinishPos = %d, want 1", tourn.FinishPos)
	}
	if tourn.TotalPlayers != 3 {
		t.Errorf("TotalPlayers = %d, want 3", tourn.TotalPlayers)
	}
	if !approxEqual(tourn.NetUSD, 4.90) {
		t.Errorf("NetUSD = %v, want 4.90", tourn.NetUSD)
	}
	if tourn.LikelySatellite {
		t.Error("LikelySatellite = true, want false for a cash payout")
	}
	if tourn.Date.IsZero() {
		t.Error("Date should be parsed from 'Tournament started'")
	}
}

// TestParseTournamentFile_MTTSatelliteTicket covers a 9-player MTT where the
// winner (not hero) receives a ticket, exercising LikelySatellite detection
// independent of which player is being tracked.
func TestParseTournamentFile_MTTSatelliteTicket(t *testing.T) {
	contents := `PokerStars Tournament #223456789, $2.00+$0.20 USD
Hold'em No Limit
Buy-In: $2.00/$0.20
9 players
Total Prize Pool: $18.00
Tournament started 2025/02/01 14:05:00 ET
  1: Villain1 (Brazil), $10.00 Sunday Special Ticket
  2: AnonHero01 (Cyprus), $5.00
  3: Villain2 (USA), $3.00
`
	path := writeTournamentSummary(t, contents)
	tourn, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}

	if tourn.Type != "mtt" {
		t.Errorf("Type = %q, want mtt", tourn.Type)
	}
	if tourn.Multiplier != nil {
		t.Errorf("Multiplier = %v, want nil for MTT", *tourn.Multiplier)
	}
	if tourn.FinishPos != 2 {
		t.Errorf("FinishPos = %d, want 2", tourn.FinishPos)
	}
	if tourn.TotalPlayers != 9 {
		t.Errorf("TotalPlayers = %d, want 9", tourn.TotalPlayers)
	}
	if !approxEqual(tourn.NetUSD, 2.80) {
		t.Errorf("NetUSD = %v, want 2.80", tourn.NetUSD)
	}
	if !tourn.LikelySatellite {
		t.Error("LikelySatellite = false, want true — winner's payout says 'Ticket'")
	}
	if tourn.WinnerPrizeDescription != "$10.00 Sunday Special Ticket" {
		t.Errorf("WinnerPrizeDescription = %q, want %q", tourn.WinnerPrizeDescription, "$10.00 Sunday Special Ticket")
	}
}

// TestParseTournamentFile_HeroBusted covers a tournament where hero's results
// line has no payout marker at all (busted out of the money), which the code
// treats as a loss of the full buy-in.
func TestParseTournamentFile_HeroBusted(t *testing.T) {
	contents := `PokerStars Tournament #323456789, $5.00+$0.50 USD
Hold'em No Limit
Buy-In: $5.00/$0.50
6 players
Total Prize Pool: $30.00
Tournament started 2025/03/10 20:00:00 ET
  1: Villain1 (Brazil), $15.00
  2: Villain2 (USA), $9.00
  3: Villain3 (Canada), $6.00
  4: AnonHero01 (Cyprus)
`
	path := writeTournamentSummary(t, contents)
	tourn, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}

	if tourn.FinishPos != 4 {
		t.Errorf("FinishPos = %d, want 4", tourn.FinishPos)
	}
	if !approxEqual(tourn.NetUSD, -5.50) {
		t.Errorf("NetUSD = %v, want -5.50 (full buy-in lost)", tourn.NetUSD)
	}
}

// TestParseTournamentFile_Freeroll covers the ErrFreeroll path: no "Buy-In: $"
// line anywhere in the file leaves BuyInUSD at 0.
func TestParseTournamentFile_Freeroll(t *testing.T) {
	contents := `PokerStars Tournament #333456789, Freeroll
Hold'em No Limit
3 players
Total Prize Pool: $0.00
Tournament started 2025/03/01 10:00:00 ET
  1: AnonHero01 (Cyprus), $0.00
`
	path := writeTournamentSummary(t, contents)
	_, err := ParseTournamentFile(path, testScreenName)
	if !errors.Is(err, ErrFreeroll) {
		t.Errorf("expected ErrFreeroll, got %v", err)
	}
}

// TestParseTournamentFile_MissingFile confirms the os.ReadFile error path is wrapped.
func TestParseTournamentFile_MissingFile(t *testing.T) {
	_, err := ParseTournamentFile(filepath.Join(t.TempDir(), "does-not-exist.txt"), testScreenName)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected wrapped os.ErrNotExist, got %v", err)
	}
}
