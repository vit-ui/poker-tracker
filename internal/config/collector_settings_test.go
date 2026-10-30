package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestConfigFieldUpdatesPreserveConcurrentOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	// Include creation: an absent-config read must not replace another owner's save.
	for owner := range 3 {
		wg.Go(func() {
			for i := range 30 {
				if _, err := LoadConfig(path); err != nil {
					errs <- err
					return
				}
				_, err := UpdateConfig(path, func(cfg *Config) error {
					switch owner {
					case 0:
						name := "Hero"
						cfg.Setup.PokerStarsScreenName = &name
					case 1:
						cfg.Schedule.DayStartHour = &i
					case 2:
						enabled := false
						cfg.Notifications.StakeChange = &enabled
					}
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Setup.PokerStarsScreenName == nil || *cfg.Setup.PokerStarsScreenName != "Hero" || cfg.Schedule.DayStartHour == nil || *cfg.Schedule.DayStartHour != 29 || cfg.Notifications.StakeChange == nil || *cfg.Notifications.StakeChange {
		t.Fatalf("lost field update: %+v", cfg)
	}
	before := *cfg
	if _, err := UpdateConfig(path, func(cfg *Config) error { cfg.Setup.PokerStarsScreenName = nil; return errors.New("reject edit") }); err == nil {
		t.Fatal("accepted failed edit")
	}
	cfg, _ = LoadConfig(path)
	if !reflect.DeepEqual(before, *cfg) {
		t.Fatal("rejected edit changed disk")
	}
}

func TestResolveCollectionValidatesPCFoldersWithoutChangingConfig(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "hands")
	name, missingName := "Hero", "OtherPlayer"
	if err := os.MkdirAll(filepath.Join(base, name), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("not a folder"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		patch   CollectorSettings
		invalid bool
	}{
		{"quoted full folder", CollectorSettings{HandHistory: ptrString(`"` + base + `"`)}, false},
		{"missing folder", CollectorSettings{HandHistory: ptrString(filepath.Join(root, "missing"))}, true},
		{"file instead of folder", CollectorSettings{HandHistory: &file}, true},
		{"missing player folder", CollectorSettings{ScreenName: &missingName}, true},
		{"traversal name", CollectorSettings{ScreenName: ptrString("../OtherPlayer")}, true},
		{"empty path", CollectorSettings{HandHistory: ptrString(" ")}, true},
		{"nul path", CollectorSettings{HandHistory: ptrString("\x00")}, true},
		{"same valid settings", CollectorSettings{ScreenName: &name}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Setup: Setup{PokerStarsScreenName: &name}, Paths: Paths{HandHistory: &base}}
			before := cfg
			got, err := ResolveCollection(&cfg, tc.patch)
			if (err != nil) != tc.invalid {
				t.Fatalf("settings %+v, error %v", got, err)
			}
			if !reflect.DeepEqual(before, cfg) {
				t.Fatal("validation mutated original config")
			}
			if !tc.invalid && (*got.ScreenName != name || *got.HandHistory != base) {
				t.Fatalf("resolved %+v", got)
			}
		})
	}
}

func ptrString(s string) *string { return &s }
