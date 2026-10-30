package bot

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
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

// drainAlerts empties a buffered alert channel into a slice.
func drainAlerts(ch chan string) []string {
	var msgs []string
	for {
		select {
		case m := <-ch:
			msgs = append(msgs, m)
		default:
			return msgs
		}
	}
}

// TestEvaluateSpingoDailyLossAlert hand-checks the Spin & Go drain warning
// against the worker-fetched poker-day tournaments: two -$0.60 spins breach
// a $1.00 threshold ($1.20 net loss), an in-day MTT loss must not count, and
// an unconfigured threshold stays silent.
func TestEvaluateSpingoDailyLossAlert(t *testing.T) {
	thresholdCents := money.Cents(100)
	dayTournaments := []parsers.Tournament{
		{Type: "spingo", NetCents: -60},
		{Type: "spingo" + parsers.KnockoutSuffix, NetCents: -60},
		{Type: "mtt", NetCents: -5000}, // not a spin — must not count toward the drain
	}

	t.Run("breach fires with the spin-only net", func(t *testing.T) {
		cfg := &config.Config{Alerts: config.Alerts{SpingoDailyLossCents: &thresholdCents}}
		ch := make(chan string, 1)
		if fired := EvaluateSpingoDailyLossAlert(cfg, dayTournaments, 5000, ch); !fired {
			t.Fatal("fired = false, want true")
		}
		want := "⚠️ Spin & Go drain alert\nDown $1.20 on Spin & Gos this poker day\nBankroll: $50.00"
		if got := drainAlerts(ch); len(got) != 1 || got[0] != want {
			t.Errorf("alerts = %q, want [%q]", got, want)
		}
	})

	t.Run("loss equal to the threshold does not fire", func(t *testing.T) {
		cfg := &config.Config{Alerts: config.Alerts{SpingoDailyLossCents: &thresholdCents}}
		ch := make(chan string, 1)
		atThreshold := []parsers.Tournament{{Type: "spingo", NetCents: -100}}
		if fired := EvaluateSpingoDailyLossAlert(cfg, atThreshold, 5000, ch); fired {
			t.Error("fired = true at exactly the threshold, want false")
		}
	})

	t.Run("unconfigured threshold is silent", func(t *testing.T) {
		ch := make(chan string, 1)
		if fired := EvaluateSpingoDailyLossAlert(&config.Config{}, dayTournaments, 5000, ch); fired {
			t.Error("fired = true with no threshold configured, want false")
		}
	})
}

// TestEvaluateStakeChangeAlert drives the one evaluator that keeps its own
// store fetch against a real sqlite store: a session at a new stake alerts,
// a session at the unchanged stake stays silent, and the first session ever
// stays silent.
func TestEvaluateStakeChangeAlert(t *testing.T) {
	store, err := sqlitestore.NewStore(filepath.Join(t.TempDir(), "tracker.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	base := storage.AsStoredClock(time.Now()).Add(-time.Hour)
	first := parsers.CashSession{Date: base, Stake: "NL2", NetCents: -100, FirstHandID: "s1"}
	if err := store.AppendCashSession(first); err != nil {
		t.Fatalf("AppendCashSession(first): %v", err)
	}

	t.Run("first session ever is silent — nothing to compare against", func(t *testing.T) {
		ch := make(chan string, 1)
		if err := EvaluateStakeChangeAlert(first, store, ch); err != nil {
			t.Fatalf("EvaluateStakeChangeAlert: %v", err)
		}
		if got := drainAlerts(ch); len(got) != 0 {
			t.Errorf("alerts = %q, want none", got)
		}
	})

	moved := parsers.CashSession{Date: base.Add(10 * time.Minute), Stake: "NL5", NetCents: -100, FirstHandID: "s2"}
	if err := store.AppendCashSession(moved); err != nil {
		t.Fatalf("AppendCashSession(moved): %v", err)
	}

	t.Run("different stake than the previous session alerts", func(t *testing.T) {
		ch := make(chan string, 1)
		if err := EvaluateStakeChangeAlert(moved, store, ch); err != nil {
			t.Fatalf("EvaluateStakeChangeAlert: %v", err)
		}
		want := "🃏 Stake change detected\nPrevious cash session: NL2\nThis session: NL5"
		if got := drainAlerts(ch); len(got) != 1 || got[0] != want {
			t.Errorf("alerts = %q, want [%q]", got, want)
		}
	})

	same := parsers.CashSession{Date: base.Add(20 * time.Minute), Stake: "NL5", NetCents: 200, FirstHandID: "s3"}
	if err := store.AppendCashSession(same); err != nil {
		t.Fatalf("AppendCashSession(same): %v", err)
	}

	t.Run("unchanged stake is silent", func(t *testing.T) {
		ch := make(chan string, 1)
		if err := EvaluateStakeChangeAlert(same, store, ch); err != nil {
			t.Fatalf("EvaluateStakeChangeAlert: %v", err)
		}
		if got := drainAlerts(ch); len(got) != 0 {
			t.Errorf("alerts = %q, want none", got)
		}
	})
}
