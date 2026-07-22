package main

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
)

// TestResolveStorageBackend covers the three startup-guard outcomes from
// CLAUDE.md's v2.0 backend-aware guard: an already-set backend is returned
// as-is, a v1.0 install (storage_backend unset, google_sheet_id already set)
// is silently migrated to "sheets" and the migration is persisted to disk,
// and an install where neither field is set errors instead of guessing.
func TestResolveStorageBackend(t *testing.T) {
	t.Run("already set is returned as-is", func(t *testing.T) {
		backend := "sqlite"
		cfg := &config.Config{Setup: config.Setup{StorageBackend: &backend}}

		got, err := resolveStorageBackend(cfg, filepath.Join(t.TempDir(), "config.json"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "sqlite" {
			t.Errorf("got %q, want %q", got, "sqlite")
		}
	})

	t.Run("v1.0 install migrates to sheets and persists it", func(t *testing.T) {
		sheetID := "abc123"
		cfg := &config.Config{Setup: config.Setup{GoogleSheetID: &sheetID}}
		path := filepath.Join(t.TempDir(), "config.json")

		got, err := resolveStorageBackend(cfg, path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "sheets" {
			t.Errorf("got %q, want %q", got, "sheets")
		}
		if cfg.Setup.StorageBackend == nil || *cfg.Setup.StorageBackend != "sheets" {
			t.Errorf("cfg.Setup.StorageBackend not migrated in memory: %v", cfg.Setup.StorageBackend)
		}

		reloaded, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if reloaded.Setup.StorageBackend == nil || *reloaded.Setup.StorageBackend != "sheets" {
			t.Errorf("migration not persisted to disk: %v", reloaded.Setup.StorageBackend)
		}
	})

	t.Run("neither field set errors", func(t *testing.T) {
		cfg := &config.Config{}

		_, err := resolveStorageBackend(cfg, filepath.Join(t.TempDir(), "config.json"))
		if err == nil {
			t.Fatal("expected an error when setup has never run")
		}
	})
}

// TestIsDuplicateTournament exercises the single choke point guarding
// against a duplicate tournament summary file event (a stray re-trigger, or
// literally the same file copied) — the exact bug that caused a repeated
// Telegram alert and a repeated satellite/ticket question for the same
// tournament before this check existed — while still letting a re-entry
// rewrite (a larger buy-in / different finish for an already-recorded
// TournamentID) through to update the row, ledger, and bankroll.
func TestIsDuplicateTournament(t *testing.T) {
	pendingOf := func(ids ...string) []config.PendingTournament {
		pending := make([]config.PendingTournament, len(ids))
		for i, id := range ids {
			pending[i] = config.PendingTournament{Tournament: parsers.Tournament{TournamentID: id}}
		}
		return pending
	}
	// tourney builds a parsed/recorded tournament with the money-relevant
	// fields isDuplicateTournament compares.
	tourney := func(id string, buyIn float64, finishPos, players int) parsers.Tournament {
		return parsers.Tournament{TournamentID: id, BuyInUSD: buyIn, FinishPos: finishPos, TotalPlayers: players}
	}

	// The first bust of a re-entry MTT, as already recorded (buy-in $1.10,
	// busted 50th of 126) — see data/ TS…T4016700037.
	firstBust := tourney("111", 1.10, 50, 126)

	cases := []struct {
		name     string
		t        parsers.Tournament
		pending  []config.PendingTournament
		recorded []parsers.Tournament
		want     bool
	}{
		{"brand new tournament, neither pending nor recorded", tourney("111", 1.10, 50, 126), nil, nil, false},
		{"already awaiting a satellite/ticket answer", tourney("111", 0.50, 3, 3), pendingOf("111"), nil, true},
		{"identical re-parse of a recorded tournament is a duplicate", firstBust, nil, []parsers.Tournament{firstBust}, true},
		{"re-entry rewrite: larger buy-in is not a duplicate", tourney("111", 2.20, 60, 140), nil, []parsers.Tournament{firstBust}, false},
		{"re-entry rewrite: final $3.30 / 43rd of 162 is not a duplicate", tourney("111", 3.30, 43, 162), nil, []parsers.Tournament{firstBust}, false},
		{"only finish position changed is not a duplicate", tourney("111", 1.10, 43, 126), nil, []parsers.Tournament{firstBust}, false},
		{"only field size changed is not a duplicate", tourney("111", 1.10, 50, 162), nil, []parsers.Tournament{firstBust}, false},
		{"a different tournament pending doesn't false-positive", tourney("111", 1.10, 50, 126), pendingOf("222"), nil, false},
		{"a different tournament recorded doesn't false-positive", tourney("111", 1.10, 50, 126), nil, []parsers.Tournament{tourney("222", 1.10, 50, 126)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isDuplicateTournament(c.t, c.pending, c.recorded)
			if got != c.want {
				t.Errorf("isDuplicateTournament(%q, ...) = %v, want %v", c.t.TournamentID, got, c.want)
			}
		})
	}
}

func TestCashSessionSummaryText(t *testing.T) {
	cases := []struct {
		name     string
		session  parsers.CashSession
		bankroll float64
		want     string
	}{
		{
			name: "winning session",
			session: parsers.CashSession{
				Stake: "NL10", NetUSD: 5.5, Hands: 42, DurationMin: 37,
			},
			bankroll: 105.5,
			want:     "✅ Cash Session\nStake: NL10\nNet: +$5.50\nHands: 42\nDuration: 37 min\nBankroll: $105.50",
		},
		{
			name: "losing session",
			session: parsers.CashSession{
				Stake: "NL25", NetUSD: -12.75, Hands: 10, DurationMin: 5,
			},
			bankroll: 87.25,
			want:     "✅ Cash Session\nStake: NL25\nNet: -$12.75\nHands: 10\nDuration: 5 min\nBankroll: $87.25",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cashSessionSummaryText(c.session, c.bankroll)
			if got != c.want {
				t.Errorf("cashSessionSummaryText() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTournamentAlertText(t *testing.T) {
	mtt := func(finish, players, reEntries int, buyIn float64) parsers.Tournament {
		return parsers.Tournament{Type: "mtt", BuyInUSD: buyIn, FinishPos: finish, TotalPlayers: players, ReEntries: reEntries}
	}
	cases := []struct {
		name    string
		t       parsers.Tournament
		netUSD  float64
		bank    float64
		wantHas string // substring the re-entry line must (not) contain
		reEntry bool
	}{
		{
			name: "plain MTT has no re-entry line", t: mtt(50, 126, 0, 1.10), netUSD: -1.10, bank: 22.00,
			reEntry: false,
		},
		{
			name: "re-entry MTT names the entry count", t: mtt(43, 162, 2, 3.30), netUSD: -3.30, bank: 19.80,
			wantHas: "🔁 Re-entered 2× — 3 entries total", reEntry: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tournamentAlertText(c.t, c.netUSD, c.bank)
			has := strings.Contains(got, "🔁")
			if has != c.reEntry {
				t.Errorf("re-entry line present = %v, want %v\ngot: %q", has, c.reEntry, got)
			}
			if c.reEntry && !strings.Contains(got, c.wantHas) {
				t.Errorf("missing %q in:\n%s", c.wantHas, got)
			}
		})
	}
}

// waitForString blocks until a string arrives on ch or timeout elapses.
func waitForString(t *testing.T, ch <-chan string, timeout time.Duration) (string, bool) {
	t.Helper()
	select {
	case s := <-ch:
		return s, true
	case <-time.After(timeout):
		return "", false
	}
}

// TestScheduleCashSessionSummary_FiresAfterQuietPeriod confirms a single
// schedule call sends the built message once the quiet period elapses.
func TestScheduleCashSessionSummary_FiresAfterQuietPeriod(t *testing.T) {
	var mu sync.Mutex
	timers := make(map[string]*time.Timer)
	alertChan := make(chan string, 10)

	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 50*time.Millisecond, alertChan, func() string {
		return "final summary"
	})

	got, ok := waitForString(t, alertChan, 500*time.Millisecond)
	if !ok {
		t.Fatal("expected a summary after the quiet period, got none")
	}
	if got != "final summary" {
		t.Errorf("got %q, want %q", got, "final summary")
	}
}

// TestScheduleCashSessionSummary_ResetOnNewHand confirms rescheduling before
// the quiet period elapses (simulating a new hand at the same table) cancels
// the pending send and collapses to exactly one message, built from the
// latest snapshot rather than the first one.
func TestScheduleCashSessionSummary_ResetOnNewHand(t *testing.T) {
	var mu sync.Mutex
	timers := make(map[string]*time.Timer)
	alertChan := make(chan string, 10)

	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 100*time.Millisecond, alertChan, func() string {
		return "hand 1 snapshot"
	})
	time.Sleep(30 * time.Millisecond)
	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 100*time.Millisecond, alertChan, func() string {
		return "hand 2 snapshot"
	})

	got, ok := waitForString(t, alertChan, 500*time.Millisecond)
	if !ok {
		t.Fatal("expected one summary after the quiet period, got none")
	}
	if got != "hand 2 snapshot" {
		t.Errorf("got %q, want the latest snapshot %q", got, "hand 2 snapshot")
	}
	if _, ok := waitForString(t, alertChan, 200*time.Millisecond); ok {
		t.Error("expected only one summary for a table that kept getting new hands, got a second")
	}
}

// TestScheduleCashSessionSummary_IndependentPerTable confirms two different
// tables (different paths) each get their own independent timer and summary
// — resetting one table's timer must never cancel another table's.
func TestScheduleCashSessionSummary_IndependentPerTable(t *testing.T) {
	var mu sync.Mutex
	timers := make(map[string]*time.Timer)
	alertChan := make(chan string, 10)

	scheduleCashSessionSummary(&mu, timers, "table-a.txt", 50*time.Millisecond, alertChan, func() string {
		return "table a"
	})
	scheduleCashSessionSummary(&mu, timers, "table-b.txt", 50*time.Millisecond, alertChan, func() string {
		return "table b"
	})

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		got, ok := waitForString(t, alertChan, 500*time.Millisecond)
		if !ok {
			t.Fatal("expected two independent summaries, got fewer")
		}
		seen[got] = true
	}
	if !seen["table a"] || !seen["table b"] {
		t.Errorf("expected summaries from both tables, got %v", seen)
	}
}
