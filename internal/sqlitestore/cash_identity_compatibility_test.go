package sqlitestore

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestCashCorrectionPreservesImportedLegacySheetsIdentity(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "migrated.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	date := time.Date(2026, 1, 2, 12, 0, 15, 0, time.UTC)
	session := parsers.CashSession{Date: date, FirstHandID: "owned-hand", NetCents: -100, BuyInCents: 200, Hands: 2}
	legacy := storage.CashSessionIdentities(date, session.FirstHandID)[1]
	snapshot := storage.Snapshot{CashSessions: []storage.CashSessionRecord{{CashSession: session, SessionID: legacy}}, Ledger: []storage.LedgerEntry{
		{Date: date, Type: "cash_session", SessionID: legacy, AmountCents: -100, RunningTotalCents: -100},
		{Date: date, Type: "deposit", SessionID: "money:later", AmountCents: 200, RunningTotalCents: 100},
	}}
	if err := store.ImportSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	session.NetCents = -50
	if err := store.AppendCashSession(session); err != nil {
		t.Fatal(err)
	}
	after, err := store.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.CashSessions) != 1 || len(after.Ledger) != 2 || after.CashSessions[0].SessionID != legacy || after.Ledger[0].SessionID != legacy {
		t.Fatalf("legacy correction duplicated/rewrote IDs: %+v", after)
	}
	if after.Ledger[0].AmountCents != -50 || after.Ledger[1].RunningTotalCents != 150 {
		t.Fatalf("correction did not cascade: %+v", after.Ledger)
	}
}
