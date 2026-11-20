package bot

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestResetStorageHistory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pending    bool
		backupFail bool
		clearFail  bool
	}{
		{name: "preserves a restorable snapshot"},
		{name: "refuses pending tournament questions", pending: true},
		{name: "refuses when backup cannot be saved", backupFail: true},
		{name: "failed clear rolls back all tables", clearFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "tracker.db")
			db, err := sqlitestore.NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			date := time.Date(2025, 2, 10, 12, 0, 0, 0, time.UTC)
			if err := db.AppendCashSession(parsers.CashSession{Date: date, Stake: "NL2", FirstHandID: "8800000001", Hands: 2, NetCents: -4}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpdateTournamentHandStats("9900000001", 20, 10, 10, 49, true); err != nil {
				t.Fatal(err)
			}
			if err := db.AppendLedgerEntry("deposit", 5000, date, "money:telegram:1:2:3"); err != nil {
				t.Fatal(err)
			}
			before, err := db.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if tc.pending {
				pending := []config.PendingTournament{{Tournament: parsers.Tournament{TournamentID: "9900000002"}}}
				if err := config.SavePendingTournaments(filepath.Join(dir, "pending_tournaments.json"), pending); err != nil {
					t.Fatal(err)
				}
			}
			if tc.backupFail {
				if err := os.WriteFile(filepath.Join(dir, "storage-backups"), []byte("occupied"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.clearFail {
				failureDB, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer failureDB.Close()
				if _, err := failureDB.Exec(`CREATE TRIGGER fail_clear BEFORE DELETE ON bankroll_ledger BEGIN SELECT RAISE(ABORT, 'test clear failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			backupPath, err := resetStorageHistory(db, dir)
			if tc.pending || tc.backupFail || tc.clearFail {
				if err == nil {
					t.Fatal("reset succeeded despite failed preflight")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				after, err := db.ExportSnapshot()
				if err != nil || !after.IsEmpty() {
					t.Fatalf("reset left history: %+v (%v)", after, err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("active database path changed: %v", err)
				}
				files, err := filepath.Glob(filepath.Join(dir, "storage-backups", "before-reset-*.json"))
				if err != nil || len(files) != 1 {
					t.Fatalf("recovery snapshots: %v (%v)", files, err)
				}
				if backupPath != files[0] {
					t.Fatal("reset reported a different recovery snapshot")
				}
				data, err := os.ReadFile(backupPath)
				if err != nil {
					t.Fatal(err)
				}
				var backup storage.Snapshot
				if err := json.Unmarshal(data, &backup); err != nil {
					t.Fatal(err)
				}
				if err := storage.VerifySnapshot(before, backup, time.Nanosecond); err != nil {
					t.Fatal(err)
				}
				if err := db.ImportSnapshot(backup); err != nil {
					t.Fatal(err)
				}
			}
			after, err := db.ExportSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.VerifySnapshot(before, after, time.Nanosecond); err != nil {
				t.Fatal(err)
			}
		})
	}
}
