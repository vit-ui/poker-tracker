package sqlitestore

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

func TestTransferClearRollsBackAllTablesOnFailure(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "transfer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.AppendCashSession(parsers.CashSession{Date: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), FirstHandID: "9000000130", Hands: 4, NetCents: -3}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER refuse_transfer_clear BEFORE DELETE ON bankroll_ledger BEGIN SELECT RAISE(FAIL, 'owned deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearAllData(); err == nil {
		t.Fatal("clear did not report ledger deletion failure")
	}
	snapshot, err := store.ExportSnapshot()
	if err != nil || len(snapshot.CashSessions) != 1 || len(snapshot.Ledger) != 1 || snapshot.Ledger[0].AmountCents != -3 {
		t.Fatalf("partial transfer clear escaped rollback: %+v %v", snapshot, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER refuse_transfer_clear`); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearAllData(); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.ExportSnapshot()
	if err != nil || !snapshot.IsEmpty() {
		t.Fatalf("clear=%+v %v", snapshot, err)
	}
}
