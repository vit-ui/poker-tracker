package config_test

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
)

func TestSaveConfigRejectsNonFiniteWithoutChangingFile(t *testing.T) {
	for _, invalid := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := &config.Config{}
			if err := config.SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Alerts.DownswingBuyins = &invalid
			if err := config.SaveConfig(path, cfg); err == nil {
				t.Fatal("saved a non-finite threshold")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("rejected threshold changed config")
			}
		})
	}
}

func TestLoadConfig_MissingFileCreatesAllNulls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Setup.TelegramChatID != nil {
		t.Error("TelegramChatID should be nil")
	}
	if cfg.Setup.GoogleSheetName != nil {
		t.Error("GoogleSheetName should be nil")
	}
	if cfg.Setup.PokerStarsScreenName != nil {
		t.Error("PokerStarsScreenName should be nil")
	}
	if cfg.Paths.HandHistory != nil {
		t.Error("HandHistory should be nil")
	}
	if cfg.Paths.TournamentSummary != nil {
		t.Error("TournamentSummary should be nil")
	}
	if cfg.Alerts.DownswingBuyins != nil {
		t.Error("DownswingBuyins should be nil")
	}
	if cfg.Alerts.LosingStreakCount != nil {
		t.Error("LosingStreakCount should be nil")
	}
	if cfg.Alerts.DailyLossCents != nil {
		t.Error("DailyLossCents should be nil")
	}
	if cfg.Hardstop.SessionLossBuyins != nil {
		t.Error("SessionLossBuyins should be nil")
	}
	if cfg.Hardstop.ConsecutiveLosses != nil {
		t.Error("ConsecutiveLosses should be nil")
	}
	if cfg.Hardstop.DailyLossCents != nil {
		t.Error("DailyLossCents should be nil")
	}
	if cfg.Hardstop.DurationHours != nil {
		t.Error("DurationHours should be nil")
	}

	if _, err := os.Stat(path); err != nil {
		t.Error("config.json should have been created on disk")
	}
}

func TestValidateExistingConfigDoesNotChangeInstallation(t *testing.T) {
	for _, tt := range []struct {
		name, content      string
		missing, wantError bool
	}{
		{name: "configured", content: `{"setup":{"connection_string":"tracker://example.invalid:8443/test#fp=test","pokerstars_screen_name":"AnonHero01"},"paths":{"hand_history":"C:\\Hands","tournament_summary":"C:\\Summaries"}}`},
		{name: "incomplete", content: `{"setup":{},"paths":{}}`},
		{name: "malformed", content: `{"setup":`, wantError: true},
		{name: "wrong field type", content: `{"setup":{"connection_string":42}}`, wantError: true},
		{name: "empty", wantError: true},
		{name: "missing", missing: true, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if !tt.missing {
				if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := config.ValidateExistingConfig(path); (err != nil) != tt.wantError {
				t.Fatalf("validation error = %v, want error %v", err, tt.wantError)
			}
			after, err := os.ReadFile(path)
			if tt.missing {
				if !os.IsNotExist(err) {
					t.Fatalf("validation created a configuration: %v", err)
				}
			} else if err != nil || string(after) != tt.content {
				t.Fatalf("validation changed configuration: %q, %v", after, err)
			}
		})
	}
}

func TestSaveConfig_LoadConfig_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	sheetName := "My Poker Sheet"
	buyins := 5.0
	streak := 3
	duration := 12

	cfg := &config.Config{
		Setup: config.Setup{
			GoogleSheetName: &sheetName,
		},
		Alerts: config.Alerts{
			DownswingBuyins:   &buyins,
			LosingStreakCount: &streak,
		},
		Hardstop: config.Hardstop{
			DurationHours: &duration,
		},
	}

	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if loaded.Setup.GoogleSheetName == nil || *loaded.Setup.GoogleSheetName != sheetName {
		t.Errorf("GoogleSheetName: got %v, want %q", loaded.Setup.GoogleSheetName, sheetName)
	}
	if loaded.Alerts.DownswingBuyins == nil || *loaded.Alerts.DownswingBuyins != buyins {
		t.Errorf("DownswingBuyins: got %v, want %v", loaded.Alerts.DownswingBuyins, buyins)
	}
	if loaded.Alerts.LosingStreakCount == nil || *loaded.Alerts.LosingStreakCount != streak {
		t.Errorf("LosingStreakCount: got %v, want %v", loaded.Alerts.LosingStreakCount, streak)
	}
	if loaded.Hardstop.DurationHours == nil || *loaded.Hardstop.DurationHours != duration {
		t.Errorf("DurationHours: got %v, want %v", loaded.Hardstop.DurationHours, duration)
	}
	// Unset fields must survive as nil.
	if loaded.Setup.TelegramChatID != nil {
		t.Error("TelegramChatID should remain nil")
	}
	if loaded.Paths.HandHistory != nil {
		t.Error("HandHistory should remain nil")
	}
}

func TestLoadLock_MissingFileReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockfile.json")

	lock, err := config.LoadLock(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lock != nil {
		t.Error("expected nil for missing lockfile")
	}
}

func TestSaveLock_LoadLock_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockfile.json")

	triggered := time.Date(2025, 1, 15, 22, 30, 0, 0, time.UTC)
	expires := time.Date(2025, 1, 16, 10, 30, 0, 0, time.UTC)

	lock := &config.LockFile{
		TriggeredAt:   triggered,
		ExpiresAt:     expires,
		Reason:        "session_loss_buyins",
		DurationHours: 12,
	}

	if err := config.SaveLock(path, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}

	loaded, err := config.LoadLock(path)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil lock")
	}

	if !loaded.TriggeredAt.Equal(triggered) {
		t.Errorf("TriggeredAt: got %v, want %v", loaded.TriggeredAt, triggered)
	}
	if !loaded.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt: got %v, want %v", loaded.ExpiresAt, expires)
	}
	if loaded.Reason != "session_loss_buyins" {
		t.Errorf("Reason: got %q, want %q", loaded.Reason, "session_loss_buyins")
	}
	if loaded.DurationHours != 12 {
		t.Errorf("DurationHours: got %d, want 12", loaded.DurationHours)
	}
}

func TestDeleteLock_RemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockfile.json")

	lock := &config.LockFile{
		TriggeredAt:   time.Now().UTC(),
		ExpiresAt:     time.Now().Add(time.Hour).UTC(),
		Reason:        "manual",
		DurationHours: 1,
	}
	if err := config.SaveLock(path, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}

	if err := config.DeleteLock(path); err != nil {
		t.Fatalf("DeleteLock: %v", err)
	}

	loaded, err := config.LoadLock(path)
	if err != nil {
		t.Fatalf("LoadLock after delete: %v", err)
	}
	if loaded != nil {
		t.Error("expected nil after delete")
	}
}

func TestShouldSendSummary(t *testing.T) {
	cases := []struct {
		mode     string
		netCents money.Cents
		want     bool
	}{
		{"off", 1000, false},
		{"off", -1000, false},
		{"off", 0, false},
		{"every", 1000, true},
		{"every", -1000, true},
		{"every", 0, true},
		{"wins", 1000, true},
		{"wins", -1000, false},
		{"wins", 0, false}, // breakeven is not a win
		{"losses", -1000, true},
		{"losses", 1000, false},
		{"losses", 0, false}, // breakeven is not a loss
		{"", 500, true},      // unrecognized/empty falls back to "every"
	}
	for _, c := range cases {
		if got := config.ShouldSendSummary(c.mode, c.netCents); got != c.want {
			t.Errorf("ShouldSendSummary(%q, %v) = %v, want %v", c.mode, c.netCents, got, c.want)
		}
	}
}

func TestSummaryMode(t *testing.T) {
	if got := config.SummaryMode(nil); got != "every" {
		t.Errorf("SummaryMode(nil) = %q, want %q", got, "every")
	}
	losses := "losses"
	if got := config.SummaryMode(&losses); got != "losses" {
		t.Errorf("SummaryMode(&\"losses\") = %q, want %q", got, "losses")
	}
}

func TestNotificationEnabled(t *testing.T) {
	tru, fls := true, false
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"nil defaults enabled", nil, true},
		{"explicit true", &tru, true},
		{"explicit false", &fls, false},
	}
	for _, c := range cases {
		if got := config.NotificationEnabled(c.in); got != c.want {
			t.Errorf("%s: NotificationEnabled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFlagEnabled(t *testing.T) {
	tru, fls := true, false
	if !config.FlagEnabled(nil) {
		t.Error("nil flag should default to enabled")
	}
	if !config.FlagEnabled(&tru) {
		t.Error("true should be enabled")
	}
	if config.FlagEnabled(&fls) {
		t.Error("false should be disabled")
	}
}

func fp(x float64) *float64 { return &x }
func ip(x int) *int         { return &x }
func bp(x bool) *bool       { return &x }

func TestEffectiveAlerts(t *testing.T) {
	a := config.Alerts{
		DownswingBuyins: fp(5), DownswingEnabled: bp(false), // off → blanked
		SessionLossBuyins: fp(3), SessionLossEnabled: bp(true), // on → kept
		LosingStreakCount: ip(4),                                       // nil flag → on → kept
		DailyLossCents:    centsPtr(2000), DailyLossEnabled: bp(false), // off → blanked
		// SpingoDailyLossCents unset → stays nil
	}
	eff := config.EffectiveAlerts(a)
	if eff.DownswingBuyins != nil {
		t.Error("off downswing should be blanked to nil")
	}
	if eff.SessionLossBuyins == nil || *eff.SessionLossBuyins != 3 {
		t.Error("on session loss should be kept")
	}
	if eff.LosingStreakCount == nil || *eff.LosingStreakCount != 4 {
		t.Error("nil-flag losing streak should stay on")
	}
	if eff.DailyLossCents != nil {
		t.Error("off daily loss should be blanked to nil")
	}
	if eff.SpingoDailyLossCents != nil {
		t.Error("unset spingo daily loss should stay nil")
	}
	// The input must not be mutated (value receiver → the caller's config keeps
	// the remembered numbers).
	if a.DownswingBuyins == nil || *a.DownswingBuyins != 5 {
		t.Error("EffectiveAlerts must not mutate its argument")
	}
}

func TestEffectiveHardstop(t *testing.T) {
	dur := ip(12)
	h := config.Hardstop{
		SessionLossBuyins: fp(8), SessionLossEnabled: bp(false), // off → blanked
		DownswingBuyins: fp(10), // nil flag → on → kept
		DurationHours:   dur,    // no flag → always kept
	}
	eff := config.EffectiveHardstop(h)
	if eff.SessionLossBuyins != nil {
		t.Error("off session loss should be blanked to nil")
	}
	if eff.DownswingBuyins == nil || *eff.DownswingBuyins != 10 {
		t.Error("nil-flag downswing should stay on")
	}
	if eff.DurationHours == nil || *eff.DurationHours != 12 {
		t.Error("duration has no flag and must be kept")
	}
	if h.SessionLossBuyins == nil || *h.SessionLossBuyins != 8 {
		t.Error("EffectiveHardstop must not mutate its argument")
	}
}

func TestIsLockExpired(t *testing.T) {
	past := &config.LockFile{ExpiresAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	if !config.IsLockExpired(past) {
		t.Error("expected past expiry to be expired")
	}

	future := &config.LockFile{ExpiresAt: time.Now().Add(time.Hour)}
	if config.IsLockExpired(future) {
		t.Error("expected future expiry to not be expired")
	}
}

func centsPtr(valueCents money.Cents) *money.Cents { return &valueCents }
