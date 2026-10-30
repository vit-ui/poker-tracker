package config

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

func TestPendingCashAtomicRoundTripAndFailedReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending_cash.json")
	if pending, err := LoadPendingCash(path); err != nil || len(pending) != 0 {
		t.Fatalf("missing journal: %v, %v", pending, err)
	}
	entry := PendingCash{FileID: "table", Session: parsers.CashSession{Date: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC), FirstHandID: "owned-hand", NetCents: -100}, LastActivity: time.Now().UTC()}
	if err := SavePendingCash(path, []PendingCash{entry}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := LoadPendingCash(path)
	if err != nil || len(pending) != 1 || pending[0].Session.NetCents != -100 || !pending[0].LastActivity.Equal(entry.LastActivity) {
		t.Fatalf("journal round trip: %v, %v", pending, err)
	}
	entry.Session.BB100 = math.NaN()
	if err := SavePendingCash(path, []PendingCash{entry}); err == nil {
		t.Fatal("nonfinite snapshot saved")
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(original) != string(unchanged) {
		t.Fatal("failed replacement changed recoverable state")
	}
	if err := SavePendingCash(path, nil); err != nil {
		t.Fatal(err)
	}
	pending, err = LoadPendingCash(path)
	if err != nil || len(pending) != 0 {
		t.Fatal("completed cash not retired")
	}
}
