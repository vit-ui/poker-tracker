package config_test

import (
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
)

func TestPendingQueueConcurrentUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending_tournaments.json")
	queued := time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)
	reminded := queued.Add(time.Hour)
	if err := config.SavePendingTournaments(path, []config.PendingTournament{
		{Tournament: parsers.Tournament{TournamentID: "T-REMOVE"}, FirstQueuedAt: queued},
		{Tournament: parsers.Tournament{TournamentID: "T-REFRESH", BuyInUSD: 1.1}, FirstQueuedAt: queued},
	}); err != nil {
		t.Fatal(err)
	}
	updates := []func([]config.PendingTournament) ([]config.PendingTournament, bool){
		func(p []config.PendingTournament) ([]config.PendingTournament, bool) {
			for i := range p {
				p[i].LastRemindedAt = reminded
			}
			return p, true
		},
		func(p []config.PendingTournament) ([]config.PendingTournament, bool) {
			for i := range p {
				if p[i].Tournament.TournamentID == "T-REMOVE" {
					return append(p[:i], p[i+1:]...), true
				}
			}
			return p, false
		},
		func(p []config.PendingTournament) ([]config.PendingTournament, bool) {
			for i := range p {
				if p[i].Tournament.TournamentID == "T-REFRESH" {
					p[i].Tournament.BuyInUSD = 2.2
				}
			}
			return p, true
		},
		func(p []config.PendingTournament) ([]config.PendingTournament, bool) {
			return append(p, config.PendingTournament{Tournament: parsers.Tournament{TournamentID: "T-NEW"}, FirstQueuedAt: queued}), true
		},
	}
	start := make(chan struct{})
	errors := make(chan error, len(updates))
	var workers sync.WaitGroup
	for _, update := range updates {
		workers.Go(func() { <-start; errors <- config.UpdatePendingTournaments(path, update) })
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	pending, err := config.LoadPendingTournaments(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending count = %d, want 2", len(pending))
	}
	seen := make(map[string]bool)
	for _, entry := range pending {
		id := entry.Tournament.TournamentID
		if seen[id] || id == "T-REMOVE" {
			t.Fatalf("duplicate or resurrected entry: %s", id)
		}
		seen[id] = true
		if !entry.FirstQueuedAt.Equal(queued) {
			t.Fatal("first-queued timestamp lost")
		}
		if id == "T-REFRESH" && (entry.Tournament.BuyInUSD != 2.2 || !entry.LastRemindedAt.Equal(reminded)) {
			t.Fatalf("refresh/reminder lost: %+v", entry)
		}
	}
	if !seen["T-REFRESH"] || !seen["T-NEW"] {
		t.Fatalf("missing entries: %+v", seen)
	}
}

func TestPendingQueueNoOpAndRejectedUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending_tournaments.json")
	if err := config.UpdatePendingTournaments(path, func(p []config.PendingTournament) ([]config.PendingTournament, bool) { return p, false }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("no-op created a pending file")
	}
	if err := config.SavePendingTournaments(path, []config.PendingTournament{{Tournament: parsers.Tournament{TournamentID: "T-KEEP"}}}); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdatePendingTournaments(path, func(p []config.PendingTournament) ([]config.PendingTournament, bool) {
		p[0].Tournament.BuyInUSD = math.NaN()
		return p, true
	}); err == nil {
		t.Fatal("expected JSON rejection")
	}
	pending, err := config.LoadPendingTournaments(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Tournament.BuyInUSD != 0 {
		t.Fatalf("failed update changed queue: %+v", pending)
	}
}
