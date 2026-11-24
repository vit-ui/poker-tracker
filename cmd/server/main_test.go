package main

import (
	"reflect"
	"testing"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/uiprompt"
)

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestApplyStorageChoice(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	p := uiprompt.New()

	preset := func(backend string) *config.Config {
		cfg := &config.Config{}
		if backend != "" {
			b := backend
			cfg.Setup.StorageBackend = &b
		}
		return cfg
	}

	cases := []struct {
		name                        string
		cfg                         *config.Config
		backend, sheetName, sheetID string
		wantBackend                 string
		wantName                    string
		wantID                      string
	}{
		{
			name: "flag sqlite on a fresh config",
			cfg:  preset(""), backend: "sqlite",
			wantBackend: "sqlite", wantName: "<nil>", wantID: "<nil>",
		},
		{
			name: "flag sheets with name and id",
			cfg:  preset(""), backend: "sheets", sheetName: "My Sheet", sheetID: "ABC123",
			wantBackend: "sheets", wantName: "My Sheet", wantID: "ABC123",
		},
		{
			name: "flag sheets with name only — id resolved during setup",
			cfg:  preset(""), backend: "sheets", sheetName: "My Sheet",
			wantBackend: "sheets", wantName: "My Sheet", wantID: "<nil>",
		},
		{
			name: "flag sheets with id only",
			cfg:  preset(""), backend: "sheets", sheetID: "ABC123",
			wantBackend: "sheets", wantName: "<nil>", wantID: "ABC123",
		},
		{
			name: "no flag preserves an existing backend on re-install",
			cfg:  preset("sheets"), backend: "",
			wantBackend: "sheets", wantName: "<nil>", wantID: "<nil>",
		},
		{
			name: "an unrecognized flag does not overwrite an existing backend",
			cfg:  preset("sqlite"), backend: "garbage",
			wantBackend: "sqlite", wantName: "<nil>", wantID: "<nil>",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyStorageChoice(c.cfg, p, c.backend, c.sheetName, c.sheetID)
			if got := deref(c.cfg.Setup.StorageBackend); got != c.wantBackend {
				t.Errorf("StorageBackend = %q, want %q", got, c.wantBackend)
			}
			if got := deref(c.cfg.Setup.GoogleSheetName); got != c.wantName {
				t.Errorf("GoogleSheetName = %q, want %q", got, c.wantName)
			}
			if got := deref(c.cfg.Setup.GoogleSheetID); got != c.wantID {
				t.Errorf("GoogleSheetID = %q, want %q", got, c.wantID)
			}
		})
	}
}

func TestSetupChoiceCannotSwitchConfiguredStorage(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	name, token, screen, backend := "Existing sheet", "pairing-identity", "Hero", "sqlite"
	hour, enabled := 9, false
	cfg := config.Config{Setup: config.Setup{StorageBackend: &backend, GoogleSheetName: &name, PairToken: &token, PokerStarsScreenName: &screen}, Schedule: config.Schedule{DayStartHour: &hour}, Notifications: config.Notifications{StakeChange: &enabled}}
	before := cfg
	applyStorageChoice(&cfg, uiprompt.New(), "sheets", "New sheet", "new-id")
	if !reflect.DeepEqual(before, cfg) {
		t.Fatalf("setup changed configured installation: %+v", cfg)
	}
}
