package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
)

func TestLoadPendingTournaments_MissingFileReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending_tournaments.json")

	pending, err := config.LoadPendingTournaments(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pending != nil {
		t.Errorf("expected nil for missing file, got %+v", pending)
	}
}

func TestSavePendingTournaments_LoadPendingTournaments_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending_tournaments.json")

	mult := 2.5
	queuedAt := time.Date(2026, 7, 8, 21, 40, 0, 0, time.UTC)
	remindedAt := time.Date(2026, 7, 8, 22, 10, 0, 0, time.UTC)

	pending := []config.PendingTournament{
		{
			Tournament: parsers.Tournament{
				Date:                   time.Date(2026, 7, 8, 21, 30, 0, 0, time.UTC),
				Type:                   "spingo",
				TournamentID:           "111222333",
				BuyInCents:             110,
				PrizePoolCents:         300,
				Multiplier:             &mult,
				FinishPos:              1,
				TotalPlayers:           3,
				NetCents:               190,
				LikelySatellite:        true,
				WinnerPrizeDescription: "$3.00 Spin & Go Ticket",
			},
			FirstQueuedAt:  queuedAt,
			LastRemindedAt: remindedAt,
		},
	}

	if err := config.SavePendingTournaments(path, pending); err != nil {
		t.Fatalf("SavePendingTournaments: %v", err)
	}

	loaded, err := config.LoadPendingTournaments(path)
	if err != nil {
		t.Fatalf("LoadPendingTournaments: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("len(loaded) = %d, want 1", len(loaded))
	}

	got := loaded[0]
	if got.Tournament.TournamentID != "111222333" {
		t.Errorf("TournamentID = %q, want 111222333", got.Tournament.TournamentID)
	}
	if got.Tournament.Multiplier == nil || *got.Tournament.Multiplier != mult {
		t.Errorf("Multiplier = %v, want %v", got.Tournament.Multiplier, mult)
	}
	if !got.Tournament.LikelySatellite {
		t.Error("LikelySatellite should round-trip as true")
	}
	if !got.FirstQueuedAt.Equal(queuedAt) {
		t.Errorf("FirstQueuedAt = %v, want %v", got.FirstQueuedAt, queuedAt)
	}
	if !got.LastRemindedAt.Equal(remindedAt) {
		t.Errorf("LastRemindedAt = %v, want %v", got.LastRemindedAt, remindedAt)
	}
}

func TestSavePendingTournaments_EmptySliceRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending_tournaments.json")

	if err := config.SavePendingTournaments(path, nil); err != nil {
		t.Fatalf("SavePendingTournaments: %v", err)
	}
	loaded, err := config.LoadPendingTournaments(path)
	if err != nil {
		t.Fatalf("LoadPendingTournaments: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("expected empty slice, got %+v", loaded)
	}
}

func TestLoadConfig_MalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0644); err != nil {
		t.Fatalf("writing malformed config: %v", err)
	}

	if _, err := config.LoadConfig(path); err == nil {
		t.Error("expected error loading malformed config.json")
	}
}

func TestLoadLock_MalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockfile.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0644); err != nil {
		t.Fatalf("writing malformed lockfile: %v", err)
	}

	if _, err := config.LoadLock(path); err == nil {
		t.Error("expected error loading malformed lockfile.json")
	}
}

func TestLoadPendingTournaments_MalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending_tournaments.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0644); err != nil {
		t.Fatalf("writing malformed pending tournaments: %v", err)
	}

	if _, err := config.LoadPendingTournaments(path); err == nil {
		t.Error("expected error loading malformed pending_tournaments.json")
	}
}

// TestLoadConfig_PartialFile confirms fields absent from a hand-written
// partial config.json load as nil rather than zero-erroring, and fields
// present survive alongside them — the null-vs-zero contract every pointer
// field in Config exists for.
func TestLoadConfig_PartialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	partial := `{
		"setup": { "telegram_chat_id": "123456" },
		"hardstop": { "duration_hours": 12 }
	}`
	if err := os.WriteFile(path, []byte(partial), 0644); err != nil {
		t.Fatalf("writing partial config: %v", err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Hardstop.DurationHours == nil || *cfg.Hardstop.DurationHours != 12 {
		t.Errorf("DurationHours = %v, want 12", cfg.Hardstop.DurationHours)
	}
	if cfg.Setup.TelegramChatID == nil || *cfg.Setup.TelegramChatID != "123456" {
		t.Errorf("TelegramChatID = %v, want 123456", cfg.Setup.TelegramChatID)
	}
	if cfg.Setup.GoogleSheetID != nil {
		t.Error("GoogleSheetID should be nil — absent from the partial file")
	}
	if cfg.Alerts.DownswingBuyins != nil {
		t.Error("DownswingBuyins should be nil — absent from the partial file")
	}
	if cfg.Hardstop.SessionLossBuyins != nil {
		t.Error("SessionLossBuyins should be nil — absent from the partial file")
	}
}
