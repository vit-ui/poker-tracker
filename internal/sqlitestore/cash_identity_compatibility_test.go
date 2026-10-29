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
	session := parsers.CashSession{Date: date, FirstHandID: "owned-hand", NetUSD: -1, BuyInUSD: 2, Hands: 2}
	legacy := storage.CashSessionIdentities(date, session.FirstHandID)[1]
	snapshot := storage.Snapshot{CashSessions: []storage.CashSessionRecord{{CashSession: session, SessionID: legacy}}, Ledger: []storage.LedgerEntry{
		{Date: date, Type: "cash_session", SessionID: legacy, AmountUSD: -1, RunningTotal: -1},
		{Date: date, Type: "deposit", SessionID: "money:later", AmountUSD: 2, RunningTotal: 1},
	}}
	if err := store.ImportSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	session.NetUSD = -.5
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
	if after.Ledger[0].AmountUSD != -.5 || after.Ledger[1].RunningTotal != 1.5 {
		t.Fatalf("correction did not cascade: %+v", after.Ledger)
	}
}
