package sqlitestore

import (
	"testing"

	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestMigrationRejectsCorruptedDestination(t *testing.T) {
	for _, tc := range []struct{ name, trigger string }{
		{"destination amounts", `CREATE TRIGGER corrupt_import AFTER INSERT ON bankroll_ledger WHEN NEW.id = 1 BEGIN UPDATE bankroll_ledger SET amount_usd = amount_usd - 10 WHERE id = NEW.id; END`},
		{"balanced amount changes", `CREATE TRIGGER corrupt_import AFTER INSERT ON bankroll_ledger WHEN NEW.id <= 2 BEGIN UPDATE bankroll_ledger SET amount_usd = amount_usd + CASE WHEN NEW.id = 1 THEN -10 ELSE 10 END WHERE id = NEW.id; END`},
		{"cash identity", `CREATE TRIGGER corrupt_import AFTER INSERT ON cash_sessions BEGIN UPDATE cash_sessions SET session_id = session_id || '-corrupt' WHERE id = NEW.id; END`},
		{"partial tournament identity", `CREATE TRIGGER corrupt_import AFTER INSERT ON tournaments WHEN NEW.date IS NULL BEGIN UPDATE tournaments SET tournament_id = tournament_id || '-corrupt' WHERE id = NEW.id; END`},
		{"already-folded bounty", `CREATE TRIGGER corrupt_import AFTER INSERT ON tournaments BEGIN UPDATE tournaments SET bounty_usd = COALESCE(bounty_usd,0) + 1 WHERE id = NEW.id; END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := newTestStore(t), newTestStore(t)
			populateStore(t, src)
			if _, err := dst.db.Exec(tc.trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.Migrate(src, dst); err == nil {
				t.Fatal("migration accepted corrupted destination records with copied running totals and matching counts")
			}
		})
	}
}
