package main

import (
	"testing"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/uiprompt"
)

// deref returns the pointed-at string, or "<nil>" for a nil pointer, so a test
// can assert on a *string field without a nil-panic.
func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestApplyStorageChoice pins how the server records the storage backend the
// installer collected. The Windows installer runs the hidden-console --setup and
// passes the choice as --backend/--sheet-name/--sheet-id; a regression here would
// silently misconfigure a user's storage. Only the flag-driven and
// re-install-preserve paths are covered (they never reach the interactive
// chooseBackend); the terminal/dialog prompt stays manually verified.
func TestApplyStorageChoice(t *testing.T) {
	// p is never invoked on any path this test drives (each returns before
	// chooseBackend), but applyStorageChoice requires a non-nil Prompter.
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
			name: "flag sheets with name only — id resolved later at serve time",
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
