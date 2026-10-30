package bot

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/money"
)

// TestHeartbeatEvent covers the pure decision behind the Sheets connectivity
// heartbeat: a down reading sends (and re-sends, on every 30-min check while
// still down) the same "unreachable" message; the down-to-healthy transition
// sends one recovery message; an already-healthy reading stays silent.
func TestHeartbeatEvent(t *testing.T) {
	since := time.Date(2026, 7, 24, 22, 16, 0, 0, time.UTC)
	errDown := errors.New("dial tcp: lookup sheets.googleapis.com: no such host")
	downMsg := fmt.Sprintf("💔 Sheets unreachable since %s", since.Local().Format("2006-01-02 15:04"))

	cases := []struct {
		name        string
		down        bool
		wasDown     bool
		wantMsg     string
		wantNowDown bool
	}{
		{"healthy and was healthy sends nothing", false, false, "", false},
		{"newly down sends the down message", true, false, downMsg, true},
		{"still down re-sends the same down message", true, true, downMsg, true},
		{"recovered sends one recovery message", false, true, "💓 Sheets reachable again", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var err error
			if c.down {
				err = errDown
			}
			msg, nowDown := heartbeatEvent(c.down, since, err, c.wasDown)
			if msg != c.wantMsg || nowDown != c.wantNowDown {
				t.Errorf("heartbeatEvent(%v, since, err, %v) = (%q, %v), want (%q, %v)",
					c.down, c.wasDown, msg, nowDown, c.wantMsg, c.wantNowDown)
			}
		})
	}
}

func TestExtractSheetIDFromURL(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		wantID string
		wantOK bool
	}{
		{"edit URL", "https://docs.google.com/spreadsheets/d/1AbC-23_xyz/edit#gid=0", "1AbC-23_xyz", true},
		{"bare URL", "https://docs.google.com/spreadsheets/d/1AbC-23_xyz", "1AbC-23_xyz", true},
		{"plain sheet name is not a URL", "My Poker Tracker", "", false},
		{"empty string is not a URL", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := extractSheetIDFromURL(c.input)
			if ok != c.wantOK || id != c.wantID {
				t.Errorf("extractSheetIDFromURL(%q) = (%q, %v), want (%q, %v)", c.input, id, ok, c.wantID, c.wantOK)
			}
		})
	}
}

func TestLooksLikeCommand(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"slash command", "/status", true},
		{"slash command with leading/trailing whitespace", "  /start  ", true},
		{"plain sheet name", "My Poker Tracker", false},
		{"sheet URL", "https://docs.google.com/spreadsheets/d/1AbC-23_xyz", false},
		{"empty string", "", false},
		{"single slash", "/", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := looksLikeCommand(c.input)
			if got != c.want {
				t.Errorf("looksLikeCommand(%q) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

func TestValidateNotBelow(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	n := func(v int) *int { return &v }

	floatCases := []struct {
		name    string
		value   float64
		other   *float64
		wantErr bool
	}{
		{"other unset, always valid", 5.0, nil, false},
		{"value above other, valid", 30.0, f(20.0), false},
		{"value equal to other, valid", 20.0, f(20.0), false},
		{"value below other, invalid", 10.0, f(20.0), true},
	}
	for _, c := range floatCases {
		t.Run(c.name, func(t *testing.T) {
			err := validateNotBelow(c.value, c.other, "hardstop", "alert")
			if (err != nil) != c.wantErr {
				t.Errorf("validateNotBelow(%v, %v) error = %v, wantErr %v", c.value, c.other, err, c.wantErr)
			}
		})
	}

	intCases := []struct {
		name    string
		value   int
		other   *int
		wantErr bool
	}{
		{"other unset, always valid", 3, nil, false},
		{"value above other, valid", 5, n(3), false},
		{"value equal to other, valid", 3, n(3), false},
		{"value below other, invalid", 2, n(3), true},
	}
	for _, c := range intCases {
		t.Run(c.name, func(t *testing.T) {
			err := validateNotBelow(c.value, c.other, "hardstop", "alert")
			if (err != nil) != c.wantErr {
				t.Errorf("validateNotBelow(%v, %v) error = %v, wantErr %v", c.value, c.other, err, c.wantErr)
			}
		})
	}
}

func TestValidateNotAbove(t *testing.T) {
	f := func(v float64) *float64 { return &v }

	cases := []struct {
		name    string
		value   float64
		other   *float64
		wantErr bool
	}{
		{"other unset, always valid", 100.0, nil, false},
		{"value below other, valid", 10.0, f(20.0), false},
		{"value equal to other, valid", 20.0, f(20.0), false},
		{"value above other, invalid", 30.0, f(20.0), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateNotAbove(c.value, c.other, "alert", "hardstop")
			if (err != nil) != c.wantErr {
				t.Errorf("validateNotAbove(%v, %v) error = %v, wantErr %v", c.value, c.other, err, c.wantErr)
			}
		})
	}
}

func TestParseStatsArgs(t *testing.T) {
	cases := []struct {
		name       string // the full command line this args string represents
		args       string
		wantType   string
		wantPeriod string
	}{
		{"/stats", "", "", ""},
		{"/stats cash", "cash", "cash", ""},
		{"/stats spins week", "spins week", "spins", "week"},
		{"/stats mtt month", "mtt month", "mtt", "month"},
		{"/stats all", "all", "", "all"},
		{"/stats cash today", "cash today", "cash", "today"},
		{"/stats CASH WEEK", "CASH WEEK", "cash", "week"},
		{"/stats invalid", "invalid", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotType, gotPeriod := ParseStatsArgs(c.args)
			if gotType != c.wantType || gotPeriod != c.wantPeriod {
				t.Errorf("ParseStatsArgs(%q) = (%q, %q), want (%q, %q)",
					c.args, gotType, gotPeriod, c.wantType, c.wantPeriod)
			}
		})
	}
}

// TestSessionLossBuyinsCrossValidation exercises the actual cross-validation
// behavior — hard rejection, not a warning — using session_loss_buyins as
// the representative pair. The same validateNotBelow/validateNotAbove logic
// applies to every alert/hardstop field pair; this covers both save
// directions, since together they enforce "hardstop must never be lower
// than alert" regardless of which side is being saved.
func TestSessionLossBuyinsCrossValidation(t *testing.T) {
	f := func(v float64) *float64 { return &v }

	t.Run("saving hard stop session loss (validateNotBelow)", func(t *testing.T) {
		cases := []struct {
			name        string
			alert       *float64
			hardstopVal float64
			wantErr     bool
		}{
			{"alert=5.0, hardstop=3.0 -> rejected (hardstop < alert)", f(5.0), 3.0, true},
			{"alert=5.0, hardstop=5.0 -> accepted (equal is allowed)", f(5.0), 5.0, false},
			{"alert=5.0, hardstop=7.0 -> accepted (hardstop > alert)", f(5.0), 7.0, false},
			{"alert=nil, hardstop=3.0 -> accepted (alert not set, no comparison)", nil, 3.0, false},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				err := validateNotBelow(c.hardstopVal, c.alert, "hard stop session loss threshold", "the session loss alert")
				if (err != nil) != c.wantErr {
					t.Errorf("validateNotBelow(hardstop=%v, alert=%v) error = %v, wantErr %v",
						c.hardstopVal, c.alert, err, c.wantErr)
				}
			})
		}
	})

	t.Run("saving alert session loss (validateNotAbove)", func(t *testing.T) {
		cases := []struct {
			name     string
			alertVal float64
			hardstop *float64
			wantErr  bool
		}{
			{"alert=5.0, hardstop=3.0 -> rejected (alert > hardstop)", 5.0, f(3.0), true},
			{"alert=5.0, hardstop=5.0 -> accepted (equal is allowed)", 5.0, f(5.0), false},
			{"alert=3.0, hardstop=5.0 -> accepted (alert < hardstop)", 3.0, f(5.0), false},
			{"alert=5.0, hardstop=nil -> accepted (hardstop not set, no comparison)", 5.0, nil, false},
			{"alert=5.0, hardstop=nil (neither field set case) -> accepted", 5.0, nil, false},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				err := validateNotAbove(c.alertVal, c.hardstop, "session loss alert", "the hard stop session loss threshold")
				if (err != nil) != c.wantErr {
					t.Errorf("validateNotAbove(alert=%v, hardstop=%v) error = %v, wantErr %v",
						c.alertVal, c.hardstop, err, c.wantErr)
				}
			})
		}
	})
}

// TestDownswingAlertThreshold exercises the same fires/does-not-fire
// decision EvaluateDownswingAlert makes: an unconfigured (nil) threshold
// never fires, otherwise fire only when peak-current strictly exceeds
// threshold × buyInCents — equal is not over.
func TestDownswingAlertThreshold(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	const peak, current, buyInCents = 12.0, 4.0, 5.00

	cases := []struct {
		name      string
		threshold *float64
		wantFire  bool
	}{
		{"threshold=1.0, 8 > 5 -> fires", f(1.0), true},
		{"threshold=2.0, 8 > 10 is false -> does not fire", f(2.0), false},
		{"threshold=1.5, 8 > 7.5 -> fires", f(1.5), true},
		{"threshold=1.6, 8 > 8.0 is false (equal is not over) -> does not fire", f(1.6), false},
		{"threshold=nil, not configured -> does not fire", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fires := c.threshold != nil && hardstop.DownswingExceeds(1200, 400, 500, *c.threshold)
			if fires != c.wantFire {
				t.Errorf("fires = %v, want %v", fires, c.wantFire)
			}
		})
	}
}

func TestRecoverTicketPayout(t *testing.T) {
	cases := []struct {
		name           string
		buyInCents     money.Cents
		netCents       money.Cents
		wantBuyInCents money.Cents
		wantNetCents   money.Cents
	}{
		{"small loss recovers to break-even", 50, -50, 0, 0},
		{"small win recovers to full payout", 50, 50, 0, 100},
		{"break-even loss recovers to zero", 100, -100, 0, 0},
		{"big win recovers to full payout", 100, 400, 0, 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotBuyInCents, gotNetCents := RecoverTicketPayout(c.buyInCents, c.netCents)
			if gotBuyInCents != c.wantBuyInCents || gotNetCents != c.wantNetCents {
				t.Errorf("RecoverTicketPayout(buyIn=%v, net=%v) = (%v, %v), want (%v, %v)",
					c.buyInCents, c.netCents, gotBuyInCents, gotNetCents, c.wantBuyInCents, c.wantNetCents)
			}
		})
	}
}
