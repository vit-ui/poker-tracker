package parsers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vit-ui/self-tracker/internal/money"
)

func TestTournamentFinishEvidence(t *testing.T) {
	const first = "PokerStars Hand #9900000101: Tournament #9900000111, $0.98+$0.12 USD Hold'em No Limit - Level I (10/20) - 2025/02/10 22:00:00 ET\nDealt to AnonHero01 [As Ad]\n"
	const next = "PokerStars Hand #9900000102: Tournament #9900000111, $0.98+$0.12 USD Hold'em No Limit - Level I (10/20) - 2025/02/10 22:01:00 ET\n"
	const finish = "AnonHero01 finished the tournament in 5th place and received $5.61\n"
	const summary = "*** SUMMARY ***\n"
	for _, test := range []struct {
		name, body   string
		position     int
		payout, cost money.Cents
	}{
		{"cash receipt and per-entry price", first + finish + summary, 5, 561, -1},
		{"explicit grand total", first + finish + "AnonHero01 paid a total of $2.20 in entry fees\n" + summary, 5, 561, 220},
		{"missing cash evidence", first + "AnonHero01 finished the tournament in 5th place\n" + summary, 5, -1, -1},
		{"explicit zero receipt", first + "AnonHero01 finished the tournament in 5th place and received $0.00.\n" + summary, 5, 0, -1},
		{"ticket face value", first + "AnonHero01 finished the tournament in 5th place and received $5.61 Ticket\n" + summary, 5, -1, -1},
		{"other player's finish", first + "AnonOpponent02 finished the tournament in 5th place and received $5.61\n" + summary, 0, -1, -1},
		{"incomplete candidate", first + finish, 0, -1, -1},
		{"resumed play in unfinished hand", first + finish + summary + next + "Dealt to AnonHero01 [8s 9d]\n", 0, -1, -1},
		{"table activity after finish", first + finish + summary + next + summary, 5, 561, -1},
		{"winner", first + "AnonHero01 wins the tournament - congratulations!\n" + summary, 1, -1, -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tournament.txt")
			if err := os.WriteFile(path, []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			_, stats, err := ParseCashFile(path, "AnonHero01")
			if err != nil {
				t.Fatal(err)
			}
			if test.position == 0 {
				if stats.Finish != nil {
					t.Fatalf("unexpected finish %+v", stats.Finish)
				}
				return
			}
			finish := stats.Finish
			if finish == nil || finish.Position != test.position || finish.EntryCents == nil || *finish.EntryCents != 110 {
				t.Fatalf("finish = %+v", finish)
			}
			for _, check := range []struct {
				name string
				got  *money.Cents
				want money.Cents
			}{{"payout", finish.PayoutCents, test.payout}, {"entry total", finish.TotalCostCents, test.cost}} {
				if (check.got == nil) != (check.want == -1) || check.got != nil && *check.got != check.want {
					t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
				}
			}
		})
	}
}
