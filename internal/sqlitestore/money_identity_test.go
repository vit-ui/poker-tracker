package sqlitestore

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestMoneyIdentitiesSurviveRestartAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	source, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	if err := source.AppendLedgerEntry("deposit", 1, now, "money:first"); err != nil {
		t.Fatal(err)
	}
	source.Close()
	source, err = NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, entry := range []struct {
		kind   string
		amount float64
		id     string
	}{
		{"deposit", 1, "money:first"},
		{"deposit", 1, "money:second"},
		{"withdrawal", -.25, "money:withdrawal"},
		{"withdrawal", -.25, "money:withdrawal"},
		{"deposit", 1, ""},
		{"deposit", 1, ""},
	} {
		if err := source.AppendLedgerEntry(entry.kind, entry.amount, now, entry.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.AppendLedgerEntry("deposit", 99, now, "session-collision"); err == nil {
		t.Fatal("accepted unnamespaced movement ID")
	}
	snapshot, err := source.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Ledger) != 5 {
		t.Fatalf("ledger rows=%d, want 5", len(snapshot.Ledger))
	}
	wantTotals := []float64{1, 2, 1.75, 2.75, 3.75}
	wantIDs := []string{"money:first", "money:second", "money:withdrawal", "", ""}
	for i, entry := range snapshot.Ledger {
		if entry.RunningTotal != wantTotals[i] || entry.SessionID != wantIDs[i] {
			t.Fatalf("row %d: %+v", i, entry)
		}
	}
	target, err := NewStore(filepath.Join(t.TempDir(), "destination.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := storage.Migrate(source, target); err != nil {
		t.Fatal(err)
	}
	if err := target.AppendLedgerEntry("deposit", 1, now, "money:first"); err != nil {
		t.Fatal(err)
	}
	copied, err := target.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.VerifySnapshot(snapshot, copied, time.Second); err != nil {
		t.Fatal(err)
	}
}
