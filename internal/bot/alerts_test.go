package bot

import (
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

// TestPreviousStake exercises the pure decision behind the stake-change
// alert: which stored cash session counts as "the previous one" relative to
// the session just recorded — most recent by start time, excluding the
// current session's own stored row (whose date can read back a few seconds
// earlier than the parsed one, since the sheets backend stores dates at
// minute precision).
func TestPreviousStake(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse("2006-01-02 15:04:05", s)
		if err != nil {
			t.Fatalf("parsing %q: %v", s, err)
		}
		return tm
	}
	session := func(date, stake string) parsers.CashSession {
		return parsers.CashSession{Date: at(date), Stake: stake}
	}

	current := session("2026-07-15 20:30:45", "NL25")

	cases := []struct {
		name      string
		sessions  []parsers.CashSession
		wantStake string
		wantOK    bool
	}{
		{
			"no stored sessions at all — first session ever",
			nil,
			"", false,
		},
		{
			"only the current session's own row (stored at minute precision) — still first session",
			[]parsers.CashSession{session("2026-07-15 20:30:00", "NL25")},
			"", false,
		},
		{
			"one earlier session at a different stake",
			[]parsers.CashSession{
				session("2026-07-14 19:00:00", "NL10"),
				session("2026-07-15 20:30:00", "NL25"), // current's own row
			},
			"NL10", true,
		},
		{
			"most recent earlier session wins, not the first",
			[]parsers.CashSession{
				session("2026-07-10 19:00:00", "NL2"),
				session("2026-07-14 19:00:00", "NL10"),
				session("2026-07-15 20:30:00", "NL25"),
			},
			"NL10", true,
		},
		{
			"earlier session at the same stake is still returned — the caller compares",
			[]parsers.CashSession{
				session("2026-07-14 19:00:00", "NL25"),
				session("2026-07-15 20:30:00", "NL25"),
			},
			"NL25", true,
		},
		{
			"rows are not assumed sorted — latest by date is picked regardless of position",
			[]parsers.CashSession{
				session("2026-07-14 19:00:00", "NL10"),
				session("2026-07-10 19:00:00", "NL2"),
				session("2026-07-15 20:30:00", "NL25"),
			},
			"NL10", true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stake, ok := previousStake(c.sessions, current)
			if stake != c.wantStake || ok != c.wantOK {
				t.Errorf("previousStake(...) = (%q, %v), want (%q, %v)", stake, ok, c.wantStake, c.wantOK)
			}
		})
	}
}
