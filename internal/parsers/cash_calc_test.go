package parsers

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestParseLeadingAmount(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   float64
		wantOk bool
	}{
		{"dollar amount", "$5.00", 5.00, true},
		{"bare integer", "5", 5, true},
		{"decimal with trailing text", "0.10 USD) - 2025", 0.10, true},
		{"trailing non-numeric text stops parsing", "5.5x", 5.5, true},
		{"no leading digits", "abc", 0, false},
		{"empty string", "", 0, false},
		{"dollar sign only", "$", 0, false},
		{"chips, no dollar sign", "25", 25, true},
		{"zero", "0", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseLeadingAmount(c.input)
			if ok != c.wantOk || got != c.want {
				t.Errorf("parseLeadingAmount(%q) = (%v, %v), want (%v, %v)", c.input, got, ok, c.want, c.wantOk)
			}
		})
	}
}

// writeHandHistory writes contents to a temp .txt file and returns its path.
func writeHandHistory(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hand.txt")
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("writing hand history: %v", err)
	}
	return path
}

// TestParseCashFile_SingleHand covers the "single session" edge case: one
// dealt hand, hero posts the small blind and folds preflop with no further
// action. Every returned field is hand-checked against the algorithm.
func TestParseCashFile_SingleHand(t *testing.T) {
	contents := `PokerStars Hand #200000000001:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:30:00 ET
Table 'Test' 6-max Seat #1 is the button
Seat 1: AnonHero01 ($10 in chips)
Seat 2: Villain ($10 in chips)
AnonHero01: posts small blind $0.05
Villain: posts big blind $0.10
*** HOLE CARDS ***
Dealt to AnonHero01 [Ah Kd]
AnonHero01: folds
Villain collected $0.15
*** SUMMARY ***
Total pot $0.15 | Rake $0
`
	path := writeHandHistory(t, contents)
	session, tourneyStats, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if tourneyStats != nil {
		t.Fatalf("expected cash session, got tournament stats: %+v", tourneyStats)
	}
	if session == nil {
		t.Fatal("expected non-nil session")
	}

	if session.Stake != "NL10" {
		t.Errorf("Stake = %q, want NL10", session.Stake)
	}
	if session.BuyInUSD != 10.0 {
		t.Errorf("BuyInUSD = %v, want 10.0", session.BuyInUSD)
	}
	if session.Hands != 1 {
		t.Errorf("Hands = %d, want 1", session.Hands)
	}
	if session.DurationMin != 0 {
		t.Errorf("DurationMin = %d, want 0 (single hand)", session.DurationMin)
	}
	if session.NetUSD != -0.05 {
		t.Errorf("NetUSD = %v, want -0.05", session.NetUSD)
	}
	if session.RakeUSD != 0 {
		t.Errorf("RakeUSD = %v, want 0", session.RakeUSD)
	}
	if session.BB100 != -50 {
		t.Errorf("BB100 = %v, want -50", session.BB100)
	}
	if session.VPIP != 0 {
		t.Errorf("VPIP = %v, want 0", session.VPIP)
	}
	if session.PFR != 0 {
		t.Errorf("PFR = %v, want 0", session.PFR)
	}
}

// TestParseCashFile_MultiHand covers a 4-hand session exercising fold, call
// (VPIP only), and raise (VPIP+PFR) preflop actions, hand-checking BB/100,
// VPIP, PFR, and DurationMin over the whole session.
func TestParseCashFile_MultiHand(t *testing.T) {
	contents := `PokerStars Hand #200000000001:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:30:00 ET
Table 'Test' 6-max Seat #1 is the button
Seat 1: AnonHero01 ($10 in chips)
Seat 2: Villain ($10 in chips)
AnonHero01: posts small blind $0.05
Villain: posts big blind $0.10
*** HOLE CARDS ***
Dealt to AnonHero01 [Ah Kd]
AnonHero01: folds
Villain collected $0.15
*** SUMMARY ***
Total pot $0.15 | Rake $0
PokerStars Hand #200000000002:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:31:00 ET
Table 'Test' 6-max Seat #1 is the button
Seat 1: AnonHero01 ($9.95 in chips)
Seat 2: Villain ($10 in chips)
Villain: posts small blind $0.05
Villain: posts big blind $0.10
*** HOLE CARDS ***
Dealt to AnonHero01 [2c 7d]
AnonHero01: calls $0.10
*** FLOP *** [2h 3h 4h]
Villain: bets $0.20
AnonHero01: folds
Villain collected $0.30
*** SUMMARY ***
Total pot $0.30 | Rake $0
PokerStars Hand #200000000003:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:32:00 ET
Table 'Test' 6-max Seat #1 is the button
Seat 1: AnonHero01 ($9.85 in chips)
Seat 2: Villain ($10 in chips)
Villain: posts small blind $0.05
Villain: posts big blind $0.10
*** HOLE CARDS ***
Dealt to AnonHero01 [Ac Ad]
AnonHero01: raises $0.20 to $0.30
Villain: folds
AnonHero01 collected $0.15
*** SUMMARY ***
Total pot $0.15 | Rake $0
PokerStars Hand #200000000004:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:33:00 ET
Table 'Test' 6-max Seat #1 is the button
Seat 1: AnonHero01 ($9.70 in chips)
Seat 2: Villain ($10 in chips)
AnonHero01: posts small blind $0.05
Villain: posts big blind $0.10
*** HOLE CARDS ***
Dealt to AnonHero01 [7s 2d]
AnonHero01: folds
Villain collected $0.15
*** SUMMARY ***
Total pot $0.15 | Rake $0
`
	path := writeHandHistory(t, contents)
	session, tourneyStats, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if tourneyStats != nil {
		t.Fatalf("expected cash session, got tournament stats: %+v", tourneyStats)
	}
	if session == nil {
		t.Fatal("expected non-nil session")
	}

	if session.Hands != 4 {
		t.Errorf("Hands = %d, want 4", session.Hands)
	}
	if session.DurationMin != 3 {
		t.Errorf("DurationMin = %d, want 3", session.DurationMin)
	}
	if !approxEqual(session.NetUSD, -0.35) {
		t.Errorf("NetUSD = %v, want -0.35", session.NetUSD)
	}
	if !approxEqual(session.BB100, -87.5) {
		t.Errorf("BB100 = %v, want -87.5", session.BB100)
	}
	if session.VPIP != 50 {
		t.Errorf("VPIP = %v, want 50", session.VPIP)
	}
	if session.PFR != 25 {
		t.Errorf("PFR = %v, want 25", session.PFR)
	}
}

// TestParseCashFile_NoHandsDealt covers the zero-hands edge case: a header
// with no matching "Dealt to <screenName>" line anywhere in the file.
func TestParseCashFile_NoHandsDealt(t *testing.T) {
	contents := `PokerStars Hand #200000000005:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:30:00 ET
Table 'Test' 6-max Seat #1 is the button
Seat 1: SomeoneElse ($10 in chips)
Seat 2: Villain ($10 in chips)
*** HOLE CARDS ***
SomeoneElse: folds
*** SUMMARY ***
Total pot $0.15 | Rake $0
`
	path := writeHandHistory(t, contents)
	session, tourneyStats, err := ParseCashFile(path, testScreenName)
	if err == nil {
		t.Fatalf("expected error for zero dealt hands, got session=%+v tourneyStats=%+v", session, tourneyStats)
	}
}

// TestParseCashFile_MalformedHeader covers a cash hand header missing the
// "($" blinds marker entirely.
func TestParseCashFile_MalformedHeader(t *testing.T) {
	contents := `PokerStars Hand #200000000006:  Hold'em No Limit - 2025/01/15 22:30:00 ET
Dealt to AnonHero01 [Ah Kd]
`
	path := writeHandHistory(t, contents)
	if _, _, err := ParseCashFile(path, testScreenName); err == nil {
		t.Fatal("expected error for header missing blinds marker")
	}
}

// TestParseCashFile_TournamentHand covers the tournament-hand branch: a hand
// history file whose header contains "Tournament #" returns TournamentHandStats
// instead of a CashSession, with VPIP/PFR computed the same way.
func TestParseCashFile_TournamentHand(t *testing.T) {
	contents := `PokerStars Hand #300000000001: Tournament #987654321, $1.00+$0.10 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:30:00 ET
Table '987654321 1' 3-max Seat #1 is the button
Seat 1: AnonHero01 (1500 in chips)
Seat 2: Villain1 (1500 in chips)
Seat 3: Villain2 (1500 in chips)
AnonHero01: posts small blind 25
Villain1: posts big blind 50
*** HOLE CARDS ***
Dealt to AnonHero01 [Ah Ks]
Villain2: folds
AnonHero01: raises 25 to 50
Villain1: folds
AnonHero01 collected 75
*** SUMMARY ***
Total pot 75 | Rake 0
`
	path := writeHandHistory(t, contents)
	session, tourneyStats, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if session != nil {
		t.Fatalf("expected tournament stats, got cash session: %+v", session)
	}
	if tourneyStats == nil {
		t.Fatal("expected non-nil tourneyStats")
	}
	if tourneyStats.TournamentID != "987654321" {
		t.Errorf("TournamentID = %q, want 987654321", tourneyStats.TournamentID)
	}
	if tourneyStats.VPIP != 100 {
		t.Errorf("VPIP = %v, want 100", tourneyStats.VPIP)
	}
	if tourneyStats.PFR != 100 {
		t.Errorf("PFR = %v, want 100", tourneyStats.PFR)
	}
}

// TestParseCashFile_TournamentHand_MixedActions covers a 2-hand tournament
// file where hero calls in one hand (VPIP only) and folds in the other
// (neither), producing non-100% VPIP/PFR.
func TestParseCashFile_TournamentHand_MixedActions(t *testing.T) {
	contents := `PokerStars Hand #300000000002: Tournament #987654322, $1.00+$0.10 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:30:00 ET
Seat 1: AnonHero01 (1500 in chips)
Seat 2: Villain1 (1500 in chips)
Seat 3: Villain2 (1500 in chips)
*** HOLE CARDS ***
Dealt to AnonHero01 [7c 2d]
AnonHero01: calls 50
*** FLOP ***
AnonHero01: folds
Villain1 collected 150
*** SUMMARY ***
Total pot 150 | Rake 0
PokerStars Hand #300000000003: Tournament #987654322, $1.00+$0.10 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:31:00 ET
Seat 1: AnonHero01 (1450 in chips)
Seat 2: Villain1 (1500 in chips)
Seat 3: Villain2 (1500 in chips)
*** HOLE CARDS ***
Dealt to AnonHero01 [2s 3d]
AnonHero01: folds
Villain1 collected 75
*** SUMMARY ***
Total pot 75 | Rake 0
`
	path := writeHandHistory(t, contents)
	_, tourneyStats, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if tourneyStats == nil {
		t.Fatal("expected non-nil tourneyStats")
	}
	if tourneyStats.VPIP != 50 {
		t.Errorf("VPIP = %v, want 50", tourneyStats.VPIP)
	}
	if tourneyStats.PFR != 0 {
		t.Errorf("PFR = %v, want 0", tourneyStats.PFR)
	}
}

// TestParseCashFile_MissingFile confirms the os.Open error path is wrapped.
func TestParseCashFile_MissingFile(t *testing.T) {
	_, _, err := ParseCashFile(filepath.Join(t.TempDir(), "does-not-exist.txt"), testScreenName)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected wrapped os.ErrNotExist, got %v", err)
	}
}
