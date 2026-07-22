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
		// Thousands separators: a comma inside a digit run is consumed and
		// stripped ("$12,500.00" was silently parsed as 12 before this).
		{"thousands-separated dollars", "$12,500.00", 12500.00, true},
		{"thousands-separated with trailing text", "1,048.00 USD) - 2026", 1048.00, true},
		{"multiple thousands groups", "1,234,567.89", 1234567.89, true},
		{"sentence comma after the amount stops parsing", "1, and more", 1, true},
		{"trailing comma not consumed", "5,", 5, true},
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
	if session.FirstHandID != "200000000001" {
		t.Errorf("FirstHandID = %q, want 200000000001", session.FirstHandID)
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
	// FirstHandID must stay pinned to the file's first hand, not drift to the
	// last one parsed — this is what keeps the session's storage key stable
	// as PokerStars keeps appending hands to the same file.
	if session.FirstHandID != "200000000001" {
		t.Errorf("FirstHandID = %q, want 200000000001 (the first hand, not the last)", session.FirstHandID)
	}
}

func TestExtractHandID(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"cash hand", "PokerStars Hand #200000000001:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:30:00 ET", "200000000001"},
		{"zoom hand", "PokerStars Zoom Hand #251912345678:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:30:00 ET", "251912345678"},
		{"tournament hand", "PokerStars Hand #300000000001: Tournament #987654321, $1.00+$0.10 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:30:00 ET", "300000000001"},
		{"no hand marker", "Table 'Test' 6-max Seat #1 is the button", ""},
		{"no colon after id", "PokerStars Hand #123", "123"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractHandID(c.line); got != c.want {
				t.Errorf("extractHandID(%q) = %q, want %q", c.line, got, c.want)
			}
		})
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
	if tourneyStats.Hands != 1 {
		t.Errorf("Hands = %v, want 1", tourneyStats.Hands)
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
	if tourneyStats.Hands != 2 {
		t.Errorf("Hands = %v, want 2", tourneyStats.Hands)
	}
}

// TestParseCashFile_KnockoutBounty covers a knockout tournament hand where
// hero eliminates one villain (scoring a bounty) and is later eliminated by
// the other (no bounty for hero on that one) — format confirmed against a
// real PokerStars progressive-knockout hand history.
func TestParseCashFile_KnockoutBounty(t *testing.T) {
	contents := `PokerStars Hand #300000000004: Tournament #987654323, $0.49+$0.06 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:30:00 ET
Table '987654323 1' 3-max Seat #1 is the button
Seat 1: AnonHero01 (5000 in chips, $0.36 bounty)
Seat 2: Villain1 (5000 in chips, $0.36 bounty)
Seat 3: Villain2 (4880 in chips, $0.36 bounty)
AnonHero01: posts small blind 25
Villain1: posts big blind 50
*** HOLE CARDS ***
Dealt to AnonHero01 [Ah Ks]
Villain2: folds
AnonHero01: raises 4975 to 5000 and is all-in
Villain1: calls 4950 and is all-in
*** SHOW DOWN ***
AnonHero01: shows [Ah Ks] (a pair of Kings)
Villain1: shows [Qc Qd] (a pair of Queens)
AnonHero01 collected 10000 from pot
AnonHero01 wins $0.18 for eliminating Villain1 and their own bounty increases by $0.18 to $0.54
Villain1 finished the tournament
*** SUMMARY ***
Total pot 10000 | Rake 0
PokerStars Hand #300000000005: Tournament #987654323, $0.49+$0.06 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:35:00 ET
Seat 1: AnonHero01 (10000 in chips, $0.54 bounty)
Seat 3: Villain2 (4880 in chips, $0.36 bounty)
AnonHero01: posts small blind 25
Villain2: posts big blind 50
*** HOLE CARDS ***
Dealt to AnonHero01 [2s 3d]
AnonHero01: raises 9975 to 10000 and is all-in
Villain2: calls 4830 and is all-in
*** SHOW DOWN ***
Villain2: shows [Ac Kc] (high card Ace)
AnonHero01: shows [2s 3d] (high card King)
Villain2 collected 9710 from pot
Villain2 wins $0.18 for eliminating AnonHero01 and their own bounty increases by $0.18 to $0.54
AnonHero01 finished the tournament in 2nd place and received $0.43.
*** SUMMARY ***
Total pot 9710 | Rake 0
`
	path := writeHandHistory(t, contents)
	_, tourneyStats, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if tourneyStats == nil {
		t.Fatal("expected non-nil tourneyStats")
	}
	if !tourneyStats.IsKnockout {
		t.Error("IsKnockout = false, want true")
	}
	if tourneyStats.BountyUSD != 0.18 {
		t.Errorf("BountyUSD = %v, want 0.18 (only hero's own elimination counts, not villain2's)", tourneyStats.BountyUSD)
	}
}

// TestParseCashFile_NonKnockoutTournament confirms a plain (non-bounty)
// tournament hand never flags IsKnockout or accrues BountyUSD, even though
// its lines happen to contain "wins" and "collected".
func TestParseCashFile_NonKnockoutTournament(t *testing.T) {
	contents := `PokerStars Hand #300000000006: Tournament #987654324, $1.00+$0.10 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:30:00 ET
Seat 1: AnonHero01 (1500 in chips)
Seat 2: Villain1 (1500 in chips)
Seat 3: Villain2 (1500 in chips)
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
	_, tourneyStats, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if tourneyStats == nil {
		t.Fatal("expected non-nil tourneyStats")
	}
	if tourneyStats.IsKnockout {
		t.Error("IsKnockout = true, want false")
	}
	if tourneyStats.BountyUSD != 0 {
		t.Errorf("BountyUSD = %v, want 0", tourneyStats.BountyUSD)
	}
}

// TestParseCashFile_KnockoutBounty_MultipleEliminations confirms BountyUSD
// accumulates across more than one elimination by hero in the same file,
// not just the single-KO case.
func TestParseCashFile_KnockoutBounty_MultipleEliminations(t *testing.T) {
	contents := `PokerStars Hand #300000000007: Tournament #987654325, $0.49+$0.06 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:30:00 ET
Seat 1: AnonHero01 (5000 in chips, $0.36 bounty)
Seat 2: Villain1 (5000 in chips, $0.36 bounty)
Seat 3: Villain2 (5000 in chips, $0.36 bounty)
*** HOLE CARDS ***
Dealt to AnonHero01 [Ah Ks]
AnonHero01: raises 4975 to 5000 and is all-in
Villain1: calls 4950 and is all-in
*** SHOW DOWN ***
AnonHero01 collected 10000 from pot
AnonHero01 wins $0.18 for eliminating Villain1 and their own bounty increases by $0.18 to $0.54
Villain1 finished the tournament
*** SUMMARY ***
Total pot 10000 | Rake 0
PokerStars Hand #300000000008: Tournament #987654325, $0.49+$0.06 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:35:00 ET
Seat 1: AnonHero01 (10000 in chips, $0.54 bounty)
Seat 3: Villain2 (5000 in chips, $0.36 bounty)
*** HOLE CARDS ***
Dealt to AnonHero01 [2s 3d]
AnonHero01: raises 9975 to 10000 and is all-in
Villain2: calls 4950 and is all-in
*** SHOW DOWN ***
AnonHero01 collected 10000 from pot
AnonHero01 wins $0.18 for eliminating Villain2 and their own bounty increases by $0.18 to $0.72
Villain2 finished the tournament
*** SUMMARY ***
Total pot 10000 | Rake 0
`
	path := writeHandHistory(t, contents)
	_, tourneyStats, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if tourneyStats == nil {
		t.Fatal("expected non-nil tourneyStats")
	}
	if tourneyStats.BountyUSD != 0.36 {
		t.Errorf("BountyUSD = %v, want 0.36 (0.18 + 0.18 across two eliminations)", tourneyStats.BountyUSD)
	}
}

// TestParseCashFile_KnockoutTournament_NoHeroBounty confirms IsKnockout is
// still true when hero plays a knockout tournament but never scores a KO
// themselves — BountyUSD stays 0, but the tournament is still flagged as
// knockout (every seat carries a bounty marker regardless of who cashes in
// on it), the distinction documented on TournamentHandStats.IsKnockout.
func TestParseCashFile_KnockoutTournament_NoHeroBounty(t *testing.T) {
	contents := `PokerStars Hand #300000000009: Tournament #987654326, $0.49+$0.06 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:30:00 ET
Seat 1: AnonHero01 (5000 in chips, $0.36 bounty)
Seat 2: Villain1 (5000 in chips, $0.36 bounty)
Seat 3: Villain2 (5000 in chips, $0.36 bounty)
*** HOLE CARDS ***
Dealt to AnonHero01 [7c 2d]
AnonHero01: folds
Villain1 collected 150
*** SUMMARY ***
Total pot 150 | Rake 0
`
	path := writeHandHistory(t, contents)
	_, tourneyStats, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if tourneyStats == nil {
		t.Fatal("expected non-nil tourneyStats")
	}
	if !tourneyStats.IsKnockout {
		t.Error("IsKnockout = false, want true — every seat shows a bounty marker")
	}
	if tourneyStats.BountyUSD != 0 {
		t.Errorf("BountyUSD = %v, want 0 — hero never eliminated anyone", tourneyStats.BountyUSD)
	}
}

// TestParseCashFile_KnockoutBounty_SittingOutSeat confirms the bounty seat
// marker is still detected when trailing text follows it on the same line
// (e.g. "... bounty) is sitting out") — a suffix-only check would miss this,
// since "bounty)" isn't the end of the line in that case.
func TestParseCashFile_KnockoutBounty_SittingOutSeat(t *testing.T) {
	contents := `PokerStars Hand #300000000010: Tournament #987654327, $0.49+$0.06 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:30:00 ET
Seat 1: AnonHero01 (5000 in chips, $0.36 bounty) is sitting out
Seat 2: Villain1 (5000 in chips, $0.36 bounty)
Seat 3: Villain2 (5000 in chips, $0.36 bounty)
*** HOLE CARDS ***
Dealt to AnonHero01 [7c 2d]
AnonHero01: folds
Villain1 collected 150
*** SUMMARY ***
Total pot 150 | Rake 0
`
	path := writeHandHistory(t, contents)
	_, tourneyStats, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if tourneyStats == nil {
		t.Fatal("expected non-nil tourneyStats")
	}
	if !tourneyStats.IsKnockout {
		t.Error("IsKnockout = false, want true — trailing \"is sitting out\" text must not hide the bounty marker")
	}
}

// TestParseCashFile_KnockoutBounty_OddCentRounding confirms BountyUSD uses
// the "wins $X.XX" amount, not the "bounty increases by $Y.YY" amount —
// these can differ by a cent on an odd bounty, since PokerStars splits the
// eliminated player's bounty 50/50 (half cash to the eliminator, half stays
// on their own head) and rounds each half independently, confirmed against
// a real hand history: "Milbak703 wins $0.68 for eliminating ChaboDER and
// their own bounty increases by $0.67 to $1.03".
func TestParseCashFile_KnockoutBounty_OddCentRounding(t *testing.T) {
	contents := `PokerStars Hand #300000000011: Tournament #987654328, $0.49+$0.06 USD Hold'em No Limit - Level V (25/50) - 2025/01/15 22:30:00 ET
Seat 1: AnonHero01 (5000 in chips, $0.68 bounty)
Seat 2: Villain1 (5000 in chips, $1.35 bounty)
*** HOLE CARDS ***
Dealt to AnonHero01 [Ah Ks]
AnonHero01: raises 4975 to 5000 and is all-in
Villain1: calls 4950 and is all-in
*** SHOW DOWN ***
AnonHero01 collected 10000 from pot
AnonHero01 wins $0.68 for eliminating Villain1 and their own bounty increases by $0.67 to $1.35
Villain1 finished the tournament
*** SUMMARY ***
Total pot 10000 | Rake 0
`
	path := writeHandHistory(t, contents)
	_, tourneyStats, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if tourneyStats == nil {
		t.Fatal("expected non-nil tourneyStats")
	}
	if tourneyStats.BountyUSD != 0.68 {
		t.Errorf("BountyUSD = %v, want 0.68 (the real cash credited — \"wins $\" — not 0.67 from \"increases by\")", tourneyStats.BountyUSD)
	}
}

// TestParseCashFile_UncalledBet confirms an uncalled raise's returned
// portion is added back into NetUSD, not counted as money lost — hero
// raises to $0.30, villain folds, and PokerStars refunds the $0.20 hero
// never had to have called by anyone via "Uncalled bet ($0.20) returned to
// AnonHero01". Only the $0.10 that was actually contested (won back via
// "collected") should count as hero's net for the hand.
func TestParseCashFile_UncalledBet(t *testing.T) {
	contents := `PokerStars Hand #200000000010:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:30:00 ET
Table 'Test' 6-max Seat #1 is the button
Seat 1: AnonHero01 ($10 in chips)
Seat 2: Villain ($10 in chips)
AnonHero01: posts small blind $0.05
Villain: posts big blind $0.10
*** HOLE CARDS ***
Dealt to AnonHero01 [Ah Kd]
AnonHero01: raises $0.20 to $0.30
Villain: folds
Uncalled bet ($0.20) returned to AnonHero01
AnonHero01 collected $0.20 from pot
*** SUMMARY ***
Total pot $0.20 | Rake $0
`
	path := writeHandHistory(t, contents)
	session, _, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if session == nil {
		t.Fatal("expected non-nil session")
	}
	// Invested $0.30 total (posted $0.05 SB + raised $0.25 more to $0.30),
	// got back $0.20 uncalled plus collected $0.20 pot (villain's $0.10 BB
	// plus hero's own $0.10 that stayed in after the uncalled refund):
	// net = 0.20 (uncalled) + 0.20 (collected) - 0.30 (invested) = 0.10.
	if !approxEqual(session.NetUSD, 0.10) {
		t.Errorf("NetUSD = %v, want 0.10 (uncalled portion refunded, not counted as invested)", session.NetUSD)
	}
}

// TestParseCashFile_RakeAccumulation confirms RakeUSD sums across multiple
// hands — every existing hand-parsing test up to this one uses "Rake $0",
// so real rake accumulation was never actually exercised.
func TestParseCashFile_RakeAccumulation(t *testing.T) {
	contents := `PokerStars Hand #200000000011:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:30:00 ET
Seat 1: AnonHero01 ($10 in chips)
Seat 2: Villain ($10 in chips)
AnonHero01: posts small blind $0.05
Villain: posts big blind $0.10
*** HOLE CARDS ***
Dealt to AnonHero01 [Ah Kd]
AnonHero01: calls $0.05
Villain: checks
*** FLOP *** [2h 3h 4h]
Villain: checks
AnonHero01: checks
*** SUMMARY ***
Total pot $0.20 | Rake $0.01
PokerStars Hand #200000000012:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:31:00 ET
Seat 1: AnonHero01 ($10 in chips)
Seat 2: Villain ($10 in chips)
Villain: posts small blind $0.05
AnonHero01: posts big blind $0.10
*** HOLE CARDS ***
Dealt to AnonHero01 [Ac Kc]
Villain: calls $0.05
AnonHero01: checks
*** FLOP *** [2h 3h 4h]
AnonHero01: checks
Villain: checks
*** SUMMARY ***
Total pot $0.20 | Rake $0.02
`
	path := writeHandHistory(t, contents)
	session, _, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if session == nil {
		t.Fatal("expected non-nil session")
	}
	if !approxEqual(session.RakeUSD, 0.03) {
		t.Errorf("RakeUSD = %v, want 0.03 (0.01 + 0.02 across two hands)", session.RakeUSD)
	}
}

// TestParseCashFile_SidePot confirms a "collected $X from side pot" line
// (distinct from the plain "collected $X from pot" / "collected $X" forms
// already exercised elsewhere) is still recognized — the collectedPfx match
// is a prefix check, so it must not care what follows "collected $X".
func TestParseCashFile_SidePot(t *testing.T) {
	contents := `PokerStars Hand #200000000013:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:30:00 ET
Seat 1: AnonHero01 ($10 in chips)
Seat 2: Villain1 ($0.50 in chips)
Seat 3: Villain2 ($10 in chips)
AnonHero01: posts small blind $0.05
Villain1: posts big blind $0.10
*** HOLE CARDS ***
Dealt to AnonHero01 [Ah Ad]
Villain1: raises $0.40 to $0.50 and is all-in
Villain2: calls $0.50
AnonHero01: calls $0.45
*** SHOW DOWN ***
AnonHero01 collected $0.30 from side pot
AnonHero01 collected $1.50 from main pot
*** SUMMARY ***
Total pot $1.80 | Main pot $1.50 | Side pot $0.30 | Rake $0
`
	path := writeHandHistory(t, contents)
	session, _, err := ParseCashFile(path, testScreenName)
	if err != nil {
		t.Fatalf("ParseCashFile: %v", err)
	}
	if session == nil {
		t.Fatal("expected non-nil session")
	}
	// Invested $0.50 (posted $0.05 SB + called $0.45 more); collected both
	// pots: 0.30 + 1.50 - 0.50 = 1.30.
	if !approxEqual(session.NetUSD, 1.30) {
		t.Errorf("NetUSD = %v, want 1.30 (both side pot and main pot collections counted)", session.NetUSD)
	}
}

// TestParseCashFile_ZoomHand_EndToEnd confirms a full Zoom-format hand
// history parses a complete, correct session — extractHandID already covers
// the header prefix in isolation, but that alone doesn't prove the rest of
// the session (Stake, Net, VPIP/PFR) computes correctly for a real Zoom
// file, since Zoom and regular cash hands are otherwise formatted the same.
func TestParseCashFile_ZoomHand_EndToEnd(t *testing.T) {
	contents := `PokerStars Zoom Hand #251912345678:  Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:30:00 ET
Table 'Test' 6-max Seat #1 is the button
Seat 1: AnonHero01 ($10 in chips)
Seat 2: Villain ($10 in chips)
AnonHero01: posts small blind $0.05
Villain: posts big blind $0.10
*** HOLE CARDS ***
Dealt to AnonHero01 [Ah Kd]
AnonHero01: raises $0.20 to $0.30
Villain: folds
Uncalled bet ($0.20) returned to AnonHero01
AnonHero01 collected $0.20 from pot
*** SUMMARY ***
Total pot $0.20 | Rake $0
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
	if session.FirstHandID != "251912345678" {
		t.Errorf("FirstHandID = %q, want 251912345678", session.FirstHandID)
	}
	if session.Hands != 1 {
		t.Errorf("Hands = %d, want 1", session.Hands)
	}
	if session.VPIP != 100 || session.PFR != 100 {
		t.Errorf("VPIP=%v PFR=%v, want 100/100 (hero raised preflop)", session.VPIP, session.PFR)
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
