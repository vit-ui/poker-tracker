package parsers

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTournament_BaseType(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		want string
	}{
		{"plain mtt", "mtt", "mtt"},
		{"plain spingo", "spingo", "spingo"},
		{"knockout mtt", "mtt" + KnockoutSuffix, "mtt"},
		{"knockout spingo", "spingo" + KnockoutSuffix, "spingo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tourn := Tournament{Type: c.typ}
			if got := tourn.BaseType(); got != c.want {
				t.Errorf("Tournament{Type: %q}.BaseType() = %q, want %q", c.typ, got, c.want)
			}
		})
	}
}

// TestIsNonCashPrize covers the full word list — ticket/pass confirmed
// against real PokerStars files, seat/package/T$ added on PokerStars' own
// documented terminology for non-cash prizes without a confirmed real file
// yet (deliberately: a false positive here only costs one extra tap on the
// cash/ticket confirmation, a false negative costs silently wrong bankroll
// data with no way to notice — see isNonCashPrize's doc comment).
func TestIsNonCashPrize(t *testing.T) {
	cases := []struct {
		name    string
		payout  string
		nonCash bool
	}{
		{"real cash amount", "$5.00", false},
		{"real cash amount with percentage", "$0.43 (1.243%)", false},
		{"ticket, confirmed against a real file", "$1.00 Power Path Step 2 Ticket", true},
		{"tournament ticket, confirmed against a real file", "Tournament Ticket ($1.50 Step 2 - Power Path)", true},
		{"pass", "Sunday Storm Freeroll Pass", true},
		{"seat", "$5,000 Seat Package", true},
		{"package alone", "WCOOP Main Event Package", true},
		{"tournament dollars", "T$11.00", true},
		{"satellite", "Sunday Million Satellite Seat", true},
		{"sat, deliberately broad — also matches inside an ordinary word", "$50 Saturday Special", true},
		{"qualifier", "Sunday Storm Qualifier", true},
		{"qualy", "Sunday Qualy", true},
		{"qually", "Mega Qually", true},
		{"case insensitive", "TICKET", true},
		{"empty string", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isNonCashPrize(c.payout); got != c.nonCash {
				t.Errorf("isNonCashPrize(%q) = %v, want %v", c.payout, got, c.nonCash)
			}
		})
	}
}

// TestParseTournamentFile_TournamentDollarsPayout confirms a T$
// (Tournament Dollars) payout is treated as non-cash end to end: excluded
// from NetUSD and flagged as LikelySatellite, the same as a Ticket/Pass
// payout — even though "T$11.00" starts with a recognizable amount, it's
// PokerStars' own tournament-only currency, not real cash.
func TestParseTournamentFile_TournamentDollarsPayout(t *testing.T) {
	contents := `PokerStars Tournament #666666666, $5.00+$0.50 USD
Hold'em No Limit
Buy-In: $5.00/$0.50
9 players
Total Prize Pool: $45.00
  1: AnonHero01 (Cyprus), T$11.00
  2: Villain1 (Brazil), $9.00
`
	path := writeTournamentSummary(t, contents)
	tourn, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if !approxEqual(tourn.NetUSD, -5.50) {
		t.Errorf("NetUSD = %v, want -5.50 (T$ is not real cash — only the buy-in lost)", tourn.NetUSD)
	}
	if !tourn.LikelySatellite {
		t.Error("LikelySatellite = false, want true — hero's own payout is T$ (Tournament Dollars), not cash")
	}
}

// TestParseTournamentFile_SatelliteFromHeader covers a satellite whose only
// signal is the title line: a real cash prize pool, and every result row still
// reading "still playing" or blank because PokerStars wrote the summary the
// moment hero busted while the field played on. The result-row scan finds no
// non-cash payout — LikelySatellite must fire from the "Super Satellite" title
// (confirmed against a real summary, Tournament #4014688191).
func TestParseTournamentFile_SatelliteFromHeader(t *testing.T) {
	contents := `PokerStars Tournament #4014688191, No Limit Hold'em
Super Satellite
Buy-In: $1.40/$0.10 USD
87 players
Total Prize Pool: $330.00 USD
Target Tournament #4014688078 Buy-In: $11.00 USD
Tournament started 2026/07/17 23:05:00 ET
  1: Villain1 (South Korea), still playing
  2: Villain2 (Canada), still playing
  3: AnonHero01 (Brazil),
  4: Villain3 (Brazil), still playing

You finished the tournament (eliminated at hand #261468132892).
`
	path := writeTournamentSummary(t, contents)
	tourn, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if tourn.PrizePoolUSD != 330.00 {
		t.Errorf("PrizePoolUSD = %v, want 330.00 (a real cash pool, not the satellite signal)", tourn.PrizePoolUSD)
	}
	if !tourn.LikelySatellite {
		t.Error("LikelySatellite = false, want true — the 'Super Satellite' title is the only signal, no row has a non-cash payout")
	}
	if tourn.WinnerPrizeDescription != "Super Satellite" {
		t.Errorf("WinnerPrizeDescription = %q, want %q", tourn.WinnerPrizeDescription, "Super Satellite")
	}
}

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

// TestParseTournamentFile_TicketPrizePool confirms a "Total Prize Pool" line
// suffixed with ticket text (e.g. "$1.00 Power Path Step 2 Ticket", the real
// PokerStars format for a Power Path Spin & Go satellite) is never parsed as
// a real dollar amount — PrizePoolUSD stays 0, the raw text is captured in
// PrizePoolDescription instead, and Multiplier stays nil since there's no
// real cash pool to divide by BuyInUSD — confirmed against a real summary
// file. (TestParseTournamentFile_SpinGoWin covers the real-cash-pool case,
// where PrizePoolUSD and Multiplier are populated normally.)
func TestParseTournamentFile_TicketPrizePool(t *testing.T) {
	contents := `PokerStars Tournament #4016076423, No Limit Hold'em
Buy-In: $0.46/$0.04 USD
3 players
Total Prize Pool: $1.00 Power Path Step 2 Ticket
Tournament started 2026/07/14 17:04:07 ET
  1: Villain1 (Brazil), $1.00 Power Path Step 2 Ticket
  2: AnonHero01 (Canada),
  3: Villain2 (Austria),
`
	path := writeTournamentSummary(t, contents)
	tourn, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}

	if tourn.PrizePoolUSD != 0 {
		t.Errorf("PrizePoolUSD = %v, want 0 (a ticket-denominated pool is not real cash)", tourn.PrizePoolUSD)
	}
	if tourn.PrizePoolDescription != "$1.00 Power Path Step 2 Ticket" {
		t.Errorf("PrizePoolDescription = %q, want %q", tourn.PrizePoolDescription, "$1.00 Power Path Step 2 Ticket")
	}
	if tourn.Type != "spingo" {
		t.Fatalf("Type = %q, want spingo", tourn.Type)
	}
	if tourn.Multiplier != nil {
		t.Errorf("Multiplier = %v, want nil — no real cash prize pool to divide by BuyInUSD", *tourn.Multiplier)
	}
}

// TestParseTournamentFile_HeroWinsTicket covers a Power Path-style satellite
// where hero themselves finishes 1st and wins a ticket, not cash — the exact
// PokerStars text still starts with a dollar sign (the ticket's own
// face-value name, e.g. "$1.00 Power Path Step 2 Ticket"), which must not be
// parsed as a real cash payout: NetUSD should reflect only the lost buy-in,
// the same as winning nothing at all, confirmed against a real hand history.
func TestParseTournamentFile_HeroWinsTicket(t *testing.T) {
	contents := `PokerStars Tournament #4016076423, No Limit Hold'em
Buy-In: $0.46/$0.04 USD
3 players
Total Prize Pool: $1.00 Power Path Step 2 Ticket
Tournament started 2026/07/14 17:04:07 BRT [2026/07/14 16:04:07 ET]
Tournament finished 2026/07/14 17:09:47 BRT [2026/07/14 16:09:47 ET]
  1: AnonHero01 (Brazil), $1.00 Power Path Step 2 Ticket
  2: Villain1 (Canada),
  3: Villain2 (Austria),

You finished in 1st place (eliminated at hand #261437820632).
`
	path := writeTournamentSummary(t, contents)
	tourn, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}

	if tourn.FinishPos != 1 {
		t.Errorf("FinishPos = %d, want 1", tourn.FinishPos)
	}
	if !approxEqual(tourn.NetUSD, -0.50) {
		t.Errorf("NetUSD = %v, want -0.50 (only the buy-in lost — the ticket has no cash value)", tourn.NetUSD)
	}
	if !tourn.LikelySatellite {
		t.Error("LikelySatellite = false, want true — hero's own payout says 'Ticket'")
	}
	if tourn.WinnerPrizeDescription != "$1.00 Power Path Step 2 Ticket" {
		t.Errorf("WinnerPrizeDescription = %q, want %q", tourn.WinnerPrizeDescription, "$1.00 Power Path Step 2 Ticket")
	}
}

// TestParseTournamentFile_HeroWinsTicket_Position1StillPlaying covers a
// Power Path-style satellite where the summary is captured before finish
// position 1 is decided (their row reads "still playing" — PokerStars can
// write the summary once hero busts, while the tournament continues for
// other players) and hero themselves, not in 1st, wins a ticket. Relying on
// finish position 1's payout alone misses this entirely — LikelySatellite
// must also trigger from hero's own row, confirmed against a real summary
// file (Tournament #4016109507).
func TestParseTournamentFile_HeroWinsTicket_Position1StillPlaying(t *testing.T) {
	contents := `PokerStars Tournament #4016109507, No Limit Hold'em
Buy-In: $0.96/$0.04 USD
16 players
Total Prize Pool: $15.36 USD

  1: Villain1 (Canada), still playing
  2: Villain2 (Brazil), still playing
  3: AnonHero01 (Brazil), Tournament Ticket ($1.50 Step 2 - Power Path)
  4: Villain3 (United Kingdom), $1.36 (8.854%)

You finished in 3rd place (eliminated at hand #261439247515).
`
	path := writeTournamentSummary(t, contents)
	tourn, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}

	if tourn.FinishPos != 3 {
		t.Errorf("FinishPos = %d, want 3", tourn.FinishPos)
	}
	if !approxEqual(tourn.NetUSD, -1.00) {
		t.Errorf("NetUSD = %v, want -1.00 (only the buy-in lost — the ticket has no cash value)", tourn.NetUSD)
	}
	if !tourn.LikelySatellite {
		t.Error("LikelySatellite = false, want true — hero's own payout says 'Tournament Ticket', even though position 1 hasn't finished yet")
	}
	if tourn.WinnerPrizeDescription != "Tournament Ticket ($1.50 Step 2 - Power Path)" {
		t.Errorf("WinnerPrizeDescription = %q, want %q", tourn.WinnerPrizeDescription, "Tournament Ticket ($1.50 Step 2 - Power Path)")
	}
}

// TestParseTournamentFile_Position1TicketTakesPriorityOverHero confirms that
// when BOTH finish position 1 and hero have ticket payouts, the description
// shown is position 1's — the more universally meaningful one for a player
// recognizing which tournament this is about — not silently overwritten by
// hero's own text processed later in the file.
func TestParseTournamentFile_Position1TicketTakesPriorityOverHero(t *testing.T) {
	contents := `PokerStars Tournament #555555555, $2.00+$0.20 USD
Hold'em No Limit
Buy-In: $2.00/$0.20
9 players
Total Prize Pool: $18.00
  1: Villain1 (Brazil), $10.00 Sunday Special Ticket
  2: AnonHero01 (Cyprus), $5.00 Sunday Special Ticket
  3: Villain2 (USA), $3.00
`
	path := writeTournamentSummary(t, contents)
	tourn, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if tourn.WinnerPrizeDescription != "$10.00 Sunday Special Ticket" {
		t.Errorf("WinnerPrizeDescription = %q, want %q (position 1's, not hero's)", tourn.WinnerPrizeDescription, "$10.00 Sunday Special Ticket")
	}
}

// TestParseTournamentFile_TicketAtOtherDecidedPosition covers the case
// neither position-1-only nor hero-own-row-only detection could catch:
// position 1 hasn't been decided yet ("still playing"), hero busted with no
// payout at all, but a worse (already-decided, since worse positions bust
// first) position's ticket is still visible in the file. Scanning every row
// is what catches this — position 1 and hero's own row alone would both
// report LikelySatellite = false here.
func TestParseTournamentFile_TicketAtOtherDecidedPosition(t *testing.T) {
	contents := `PokerStars Tournament #4016109507, No Limit Hold'em
Buy-In: $0.96/$0.04 USD
16 players
Total Prize Pool: $15.36 USD
  1: Villain1 (Canada), still playing
  2: Villain2 (Brazil), still playing
  3: Villain3 (Brazil), Tournament Ticket ($1.50 Step 2 - Power Path)
  4: Villain4 (United Kingdom), $1.36 (8.854%)
  5: AnonHero01 (Venezuela),
  6: Villain5 (Brazil),
`
	path := writeTournamentSummary(t, contents)
	tourn, err := ParseTournamentFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseTournamentFile: %v", err)
	}
	if tourn.FinishPos != 5 {
		t.Errorf("FinishPos = %d, want 5", tourn.FinishPos)
	}
	if !approxEqual(tourn.NetUSD, -1.00) {
		t.Errorf("NetUSD = %v, want -1.00 (hero won nothing at all)", tourn.NetUSD)
	}
	if !tourn.LikelySatellite {
		t.Error("LikelySatellite = false, want true — position 3's already-decided ticket is a valid satellite signal even though it's neither position 1 nor hero")
	}
	if tourn.WinnerPrizeDescription != "Tournament Ticket ($1.50 Step 2 - Power Path)" {
		t.Errorf("WinnerPrizeDescription = %q, want %q", tourn.WinnerPrizeDescription, "Tournament Ticket ($1.50 Step 2 - Power Path)")
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
