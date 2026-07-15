package sqlitestore

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

// closeEnough reports whether a and b differ by less than half a cent — USD
// amounts derived from chained float64 arithmetic can land a few ULPs off an
// exact decimal value.
func closeEnough(a, b float64) bool {
	return math.Abs(a-b) < 0.005
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "tracker.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return tm
}

// ---------------------------------------------------------------------------
// Pure calculation functions
// ---------------------------------------------------------------------------

func TestCascadeRunningTotals(t *testing.T) {
	t.Run("single row, last in ledger — matches a simple update", func(t *testing.T) {
		got := cascadeRunningTotals([]float64{-5}, -8, 100)
		want := []float64{92}
		if len(got) != 1 || !closeEnough(got[0], want[0]) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("interleaved tables — later row's running total must shift too", func(t *testing.T) {
		// Two tables open at once, start bankroll 100:
		//   deposit          +100 -> RT 100 (base, not part of amounts)
		//   table A hand 1     -5 -> RT  95
		//   table B hand 1     -3 -> RT  92
		// Table A's 2nd hand arrives: cumulative net now -8, correcting row0.
		// row1 (table B) must be recomputed too.
		got := cascadeRunningTotals([]float64{-5, -3}, -8, 100)
		want := []float64{92, 89}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if !closeEnough(got[i], want[i]) {
				t.Errorf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("changed row is the only row, base zero", func(t *testing.T) {
		got := cascadeRunningTotals([]float64{-5}, -8, 0)
		want := []float64{-8}
		if len(got) != 1 || !closeEnough(got[0], want[0]) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("three rows after the changed one", func(t *testing.T) {
		got := cascadeRunningTotals([]float64{10, 1, 2, 3}, 20, 0)
		want := []float64{20, 21, 23, 26}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if !closeEnough(got[i], want[i]) {
				t.Errorf("index %d: got %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("empty amounts returns nil", func(t *testing.T) {
		if got := cascadeRunningTotals(nil, -8, 0); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

func TestReconcileKnockoutType(t *testing.T) {
	cases := []struct {
		name             string
		baseType         string
		existingTypeCell string
		want             string
	}{
		{"placeholder from a knockout-flagged partial row", "mtt", knockoutPlaceholder, "mtt-ko"},
		{"spingo variant", "spingo", knockoutPlaceholder, "spingo-ko"},
		{"no prior knockout signal at all", "mtt", "", "mtt"},
		{"already-suffixed row from an earlier reconcile — idempotent", "mtt", "mtt-ko", "mtt-ko"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reconcileKnockoutType(c.baseType, c.existingTypeCell); got != c.want {
				t.Errorf("reconcileKnockoutType(%q, %q) = %q, want %q", c.baseType, c.existingTypeCell, got, c.want)
			}
		})
	}
}

func TestAppendKnockoutSuffix(t *testing.T) {
	cases := []struct {
		name             string
		existingTypeCell string
		want             string
	}{
		{"base type not known yet — summary hasn't arrived", "", knockoutPlaceholder},
		{"base type already known", "mtt", "mtt-ko"},
		{"spingo variant", "spingo", "spingo-ko"},
		{"already the placeholder — idempotent", knockoutPlaceholder, knockoutPlaceholder},
		{"already suffixed — idempotent", "mtt-ko", "mtt-ko"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := appendKnockoutSuffix(c.existingTypeCell); got != c.want {
				t.Errorf("appendKnockoutSuffix(%q) = %q, want %q", c.existingTypeCell, got, c.want)
			}
		})
	}
}

func TestFoldBountyIntoNet(t *testing.T) {
	cases := []struct {
		name                             string
		baselineNetUSD, bountyUSD, buyIn float64
		wantNet, wantPrizeWon            float64
	}{
		{"no bounty yet — hand file hasn't arrived", -0.12, 0, 0.55, -0.12, 0.43},
		{"real KO tournament — a loss on paper becomes a win", -0.12, 0.90, 0.55, 0.78, 1.33},
		{"non-knockout tournament, bounty always zero", 2.00, 0, 5.00, 2.00, 7.00},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotNet, gotPrizeWon := foldBountyIntoNet(c.baselineNetUSD, c.bountyUSD, c.buyIn)
			if !closeEnough(gotNet, c.wantNet) || !closeEnough(gotPrizeWon, c.wantPrizeWon) {
				t.Errorf("foldBountyIntoNet(%v, %v, %v) = (%v, %v), want (%v, %v)",
					c.baselineNetUSD, c.bountyUSD, c.buyIn, gotNet, gotPrizeWon, c.wantNet, c.wantPrizeWon)
			}
		})
	}
}

func TestReconcileTournamentBounty(t *testing.T) {
	cases := []struct {
		name                                               string
		storedNetUSD, storedBountyUSD, newBountyUSD, buyIn float64
		wantNet, wantPrizeWon                              float64
	}{
		{"bounty grows — more KOs since the last hand-file parse", 0.78, 0.90, 1.26, 0.55, 1.14, 1.69},
		{"unchanged re-parse is a no-op", 0.78, 0.90, 0.90, 0.55, 0.78, 1.33},
		{"first bounty applied after summary already landed", -0.12, 0, 0.90, 0.55, 0.78, 1.33},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotNet, gotPrizeWon := reconcileTournamentBounty(c.storedNetUSD, c.storedBountyUSD, c.newBountyUSD, c.buyIn)
			if !closeEnough(gotNet, c.wantNet) || !closeEnough(gotPrizeWon, c.wantPrizeWon) {
				t.Errorf("reconcileTournamentBounty(%v, %v, %v, %v) = (%v, %v), want (%v, %v)",
					c.storedNetUSD, c.storedBountyUSD, c.newBountyUSD, c.buyIn, gotNet, gotPrizeWon, c.wantNet, c.wantPrizeWon)
			}
		})
	}
}

func TestCashSessionID(t *testing.T) {
	d1 := mustTime(t, "2026-07-01T20:00:00Z")
	d2 := mustTime(t, "2026-07-01T20:01:00Z")

	id1a := cashSessionID(d1, "200000000001")
	id1b := cashSessionID(d1, "200000000001")
	if id1a != id1b {
		t.Errorf("cashSessionID not stable across identical inputs: %q vs %q", id1a, id1b)
	}

	if diff := cashSessionID(d1, "200000000002"); diff == id1a {
		t.Error("cashSessionID collided for two different first-hand IDs on the same date")
	}

	if diff := cashSessionID(d2, "200000000001"); diff == id1a {
		t.Error("cashSessionID collided for two different dates with the same first-hand ID")
	}
}

// ---------------------------------------------------------------------------
// NewStore / schema
// ---------------------------------------------------------------------------

func TestNewStore_CreatesAllFourTables(t *testing.T) {
	s := newTestStore(t)
	want := []string{"cash_sessions", "tournaments", "bankroll_ledger", "time_analysis"}
	for _, table := range want {
		var name string
		err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}
}

func TestNewStore_ReopenExistingDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tracker.db")

	s1, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("first NewStore: %v", err)
	}
	if err := s1.AppendLedgerEntry("deposit", 100); err != nil {
		t.Fatalf("AppendLedgerEntry: %v", err)
	}
	s1.Close()

	s2, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("second NewStore (reopen): %v", err)
	}
	defer s2.Close()

	bankroll, err := s2.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if !closeEnough(bankroll, 100) {
		t.Errorf("bankroll after reopen = %v, want 100 (data must survive reopen, schema creation must be idempotent)", bankroll)
	}
}

func TestNewStore_InvalidDirectory(t *testing.T) {
	_, err := NewStore(filepath.Join(t.TempDir(), "does-not-exist", "tracker.db"))
	if err == nil {
		t.Fatal("expected error opening a db file in a nonexistent directory")
	}
}

// TestNewStore_MigratesSchemaAddingHandsColumn simulates a tracker.db
// created by an older binary, before tournaments.hands existed: the
// tournaments table is recreated by hand without that column and seeded
// with a real row, bypassing NewStore/the current schema entirely. Reopening
// through NewStore must then migrate the missing column in — CREATE TABLE
// IF NOT EXISTS alone is a no-op on a table that already exists, so without
// migrateSchema this row's hand-file data would be unreadable/unwritable
// forever after a binary update, with no signal beyond a runtime scan error.
func TestNewStore_MigratesSchemaAddingHandsColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tracker.db")

	oldDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening pre-migration db: %v", err)
	}
	if _, err := oldDB.Exec(`
		CREATE TABLE tournaments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			date TEXT,
			type TEXT NOT NULL,
			tournament_id TEXT NOT NULL UNIQUE,
			buy_in_usd REAL,
			prize_pool_usd REAL,
			prize_pool_description TEXT,
			multiplier REAL,
			finish_pos INTEGER,
			total_players INTEGER,
			net_usd REAL,
			vpip REAL,
			pfr REAL,
			bounty_usd REAL,
			prize_won_usd REAL
		)
	`); err != nil {
		t.Fatalf("creating pre-migration tournaments table: %v", err)
	}
	if _, err := oldDB.Exec(`
		INSERT INTO tournaments (date, type, tournament_id, buy_in_usd, prize_pool_usd, finish_pos, total_players, net_usd, vpip, pfr, bounty_usd, prize_won_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "2026-07-10T20:00:00Z", "mtt", "9000000001", 5.00, 100.0, 3, 50, -5.00, 22.0, 10.0, 0.0, 0.0); err != nil {
		t.Fatalf("seeding pre-migration row: %v", err)
	}
	if err := oldDB.Close(); err != nil {
		t.Fatalf("closing pre-migration db: %v", err)
	}

	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore (should migrate): %v", err)
	}
	defer s.Close()

	cols, err := s.existingColumns("tournaments")
	if err != nil {
		t.Fatalf("existingColumns: %v", err)
	}
	if !cols["hands"] {
		t.Fatal("hands column missing after migration")
	}

	// The pre-existing row's data must survive untouched, with hands NULL —
	// no hand file was ever parsed under the old schema, so there's nothing
	// to backfill it with.
	var typeVal, tournamentID string
	var buyIn, netUSD, vpip, pfr float64
	var hands sql.NullInt64
	err = s.db.QueryRow(`
		SELECT type, tournament_id, buy_in_usd, net_usd, vpip, pfr, hands FROM tournaments WHERE tournament_id = ?
	`, "9000000001").Scan(&typeVal, &tournamentID, &buyIn, &netUSD, &vpip, &pfr, &hands)
	if err != nil {
		t.Fatalf("reading migrated row: %v", err)
	}
	if typeVal != "mtt" || tournamentID != "9000000001" || !closeEnough(buyIn, 5.00) || !closeEnough(netUSD, -5.00) {
		t.Errorf("existing row data changed by migration: type=%q id=%q buyIn=%v netUSD=%v", typeVal, tournamentID, buyIn, netUSD)
	}
	if !closeEnough(vpip, 22.0) || !closeEnough(pfr, 10.0) {
		t.Errorf("existing VPIP/PFR changed by migration: vpip=%v pfr=%v", vpip, pfr)
	}
	if hands.Valid {
		t.Errorf("hands = %v, want NULL for a pre-migration row", hands.Int64)
	}

	// The migrated table must be immediately usable for new writes too.
	if err := s.UpdateTournamentHandStats("9000000001", 22.0, 10.0, 88, 0, false); err != nil {
		t.Fatalf("UpdateTournamentHandStats after migration: %v", err)
	}
	tournaments, err := s.GetAllTournaments()
	if err != nil {
		t.Fatalf("GetAllTournaments: %v", err)
	}
	if len(tournaments) != 1 || tournaments[0].Hands == nil || *tournaments[0].Hands != 88 {
		t.Errorf("tournaments = %+v, want a single row with Hands=88 after the post-migration write", tournaments)
	}
}

// TestMigrateSchema_Idempotent confirms running migrateSchema against an
// already-current database (the normal case for every install after the
// first) is a no-op — no duplicate-column error, no data change.
func TestMigrateSchema_Idempotent(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	if err := s.AppendCashSession(buildCashSession(date, "M0001", 10, -1)); err != nil {
		t.Fatalf("AppendCashSession: %v", err)
	}

	if err := s.migrateSchema(); err != nil {
		t.Fatalf("migrateSchema on an up-to-date database: %v", err)
	}

	sessions, err := s.GetCashSessionsSince(time.Time{})
	if err != nil {
		t.Fatalf("GetCashSessionsSince: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Hands != 10 {
		t.Errorf("sessions = %+v, want unchanged after a no-op migration", sessions)
	}
}

// ---------------------------------------------------------------------------
// Cash sessions
// ---------------------------------------------------------------------------

func buildCashSession(date time.Time, firstHandID string, hands int, netUSD float64) parsers.CashSession {
	return parsers.CashSession{
		Date:        date,
		Stake:       "NL10",
		BuyInUSD:    10, // never stored — GetCashSessionsSince must return 0
		Hands:       hands,
		DurationMin: hands * 2,
		NetUSD:      netUSD,
		RakeUSD:     float64(hands) * 0.05,
		BB100:       netUSD / 10 / float64(hands) * 100,
		VPIP:        24.5,
		PFR:         18.2,
		FirstHandID: firstHandID,
	}
}

func TestAppendCashSession_InsertsNewSession(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	session := buildCashSession(date, "200000000001", 50, -5.25)

	if err := s.AppendCashSession(session); err != nil {
		t.Fatalf("AppendCashSession: %v", err)
	}

	sessions, err := s.GetCashSessionsSince(time.Time{})
	if err != nil {
		t.Fatalf("GetCashSessionsSince: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(sessions))
	}
	got := sessions[0]
	if got.BuyInUSD != 0 {
		t.Errorf("BuyInUSD = %v, want 0 (never stored)", got.BuyInUSD)
	}
	if got.Stake != "NL10" || got.Hands != 50 || !closeEnough(got.NetUSD, -5.25) {
		t.Errorf("session = %+v, unexpected", got)
	}
	if !got.Date.Equal(date) {
		t.Errorf("Date = %v, want %v", got.Date, date)
	}

	// The ledger and time analysis rows must exist too, keyed by the same
	// session ID — verified directly since neither is fully exposed by the
	// storage.Store interface.
	id := cashSessionID(date, "200000000001")
	var hour int
	var dayOfWeek string
	var netUSD float64
	err = s.db.QueryRow(`SELECT hour, day_of_week, net_usd FROM time_analysis WHERE session_id = ?`, id).
		Scan(&hour, &dayOfWeek, &netUSD)
	if err != nil {
		t.Fatalf("reading time_analysis row: %v", err)
	}
	if hour != date.Hour() || dayOfWeek != date.Weekday().String() || !closeEnough(netUSD, -5.25) {
		t.Errorf("time_analysis row = (%d, %s, %v), want (%d, %s, -5.25)", hour, dayOfWeek, netUSD, date.Hour(), date.Weekday().String())
	}

	bankroll, err := s.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if !closeEnough(bankroll, -5.25) {
		t.Errorf("bankroll = %v, want -5.25", bankroll)
	}
}

// TestAppendCashSession_ReparseUpdatesInPlace mirrors a PokerStars hand
// history file growing hand-by-hand: the same file is re-parsed from scratch
// on every hand, so Hands/NetUSD/etc. keep changing for the *same* session ID
// (Date + FirstHandID are fixed from the file's first hand). Every table's
// row for that session must be corrected in place, never appended twice.
func TestAppendCashSession_ReparseUpdatesInPlace(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")

	if err := s.AppendCashSession(buildCashSession(date, "200000000001", 10, -1.00)); err != nil {
		t.Fatalf("first AppendCashSession: %v", err)
	}
	if err := s.AppendCashSession(buildCashSession(date, "200000000001", 25, 3.50)); err != nil {
		t.Fatalf("second AppendCashSession: %v", err)
	}

	sessions, err := s.GetCashSessionsSince(time.Time{})
	if err != nil {
		t.Fatalf("GetCashSessionsSince: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1 (re-parse must update in place, not append)", len(sessions))
	}
	if sessions[0].Hands != 25 || !closeEnough(sessions[0].NetUSD, 3.50) {
		t.Errorf("session = %+v, want latest snapshot (25 hands, net 3.50)", sessions[0])
	}

	bankroll, err := s.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if !closeEnough(bankroll, 3.50) {
		t.Errorf("bankroll = %v, want 3.50 (ledger must reflect latest net, not sum of both parses)", bankroll)
	}

	var ledgerRowCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM bankroll_ledger`).Scan(&ledgerRowCount); err != nil {
		t.Fatalf("counting ledger rows: %v", err)
	}
	if ledgerRowCount != 1 {
		t.Errorf("ledger row count = %d, want 1", ledgerRowCount)
	}
}

func TestAppendCashSession_DistinctSessionsAppend(t *testing.T) {
	s := newTestStore(t)
	d1 := mustTime(t, "2026-07-10T20:00:00Z")
	d2 := mustTime(t, "2026-07-11T20:00:00Z")

	if err := s.AppendCashSession(buildCashSession(d1, "200000000001", 10, -1.00)); err != nil {
		t.Fatalf("session 1: %v", err)
	}
	if err := s.AppendCashSession(buildCashSession(d2, "200000000002", 20, 2.00)); err != nil {
		t.Fatalf("session 2: %v", err)
	}

	sessions, err := s.GetCashSessionsSince(time.Time{})
	if err != nil {
		t.Fatalf("GetCashSessionsSince: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("len(sessions) = %d, want 2", len(sessions))
	}

	bankroll, err := s.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if !closeEnough(bankroll, 1.00) {
		t.Errorf("bankroll = %v, want 1.00 (-1.00 + 2.00)", bankroll)
	}
}

// TestLedgerCascade_InterleavedTables reproduces the exact scenario CLAUDE.md
// documents for updateLedgerAmountCascading: two tables open at once, then
// the first table's session is corrected — the second table's already-written
// ledger row must shift its running total too, even though it belongs to an
// unrelated session.
func TestLedgerCascade_InterleavedTables(t *testing.T) {
	s := newTestStore(t)
	tableA := mustTime(t, "2026-07-10T20:00:00Z")
	tableB := mustTime(t, "2026-07-10T20:05:00Z")

	if err := s.AppendLedgerEntry("deposit", 100); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if err := s.AppendCashSession(buildCashSession(tableA, "A0001", 5, -5)); err != nil {
		t.Fatalf("table A hand 1: %v", err)
	}
	if err := s.AppendCashSession(buildCashSession(tableB, "B0001", 3, -3)); err != nil {
		t.Fatalf("table B hand 1: %v", err)
	}
	// Table A's second hand arrives, cumulative net now -8.
	if err := s.AppendCashSession(buildCashSession(tableA, "A0001", 10, -8)); err != nil {
		t.Fatalf("table A hand 2: %v", err)
	}

	entries, err := s.GetLedgerEntriesSince(time.Time{})
	if err != nil {
		t.Fatalf("GetLedgerEntriesSince: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3 (deposit, table A, table B)", len(entries))
	}
	// Order is insertion order: deposit, table A, table B.
	if !closeEnough(entries[0].RunningTotal, 100) {
		t.Errorf("deposit running total = %v, want 100", entries[0].RunningTotal)
	}
	if !closeEnough(entries[1].AmountUSD, -8) || !closeEnough(entries[1].RunningTotal, 92) {
		t.Errorf("table A row = (amount %v, total %v), want (-8, 92)", entries[1].AmountUSD, entries[1].RunningTotal)
	}
	if !closeEnough(entries[2].AmountUSD, -3) || !closeEnough(entries[2].RunningTotal, 89) {
		t.Errorf("table B row = (amount %v, total %v), want (-3, 89) — must shift even though its own amount is unchanged",
			entries[2].AmountUSD, entries[2].RunningTotal)
	}
}

func TestGetCashSessionsSince_DateFiltering(t *testing.T) {
	s := newTestStore(t)
	early := mustTime(t, "2026-07-01T10:00:00Z")
	late := mustTime(t, "2026-07-15T10:00:00Z")
	cutoff := mustTime(t, "2026-07-10T00:00:00Z")

	if err := s.AppendCashSession(buildCashSession(early, "E0001", 5, -1)); err != nil {
		t.Fatalf("early session: %v", err)
	}
	if err := s.AppendCashSession(buildCashSession(late, "L0001", 5, 1)); err != nil {
		t.Fatalf("late session: %v", err)
	}

	sessions, err := s.GetCashSessionsSince(cutoff)
	if err != nil {
		t.Fatalf("GetCashSessionsSince: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Hands != 5 || !sessions[0].Date.Equal(late) {
		t.Errorf("sessions = %+v, want exactly the late session", sessions)
	}

	if err := s.AppendLedgerEntry("deposit", 0); err != nil { // no-op sanity: ledger untouched by the date filter
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetCashSessionsSince_CutoffIsInclusive(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T00:00:00Z")
	if err := s.AppendCashSession(buildCashSession(date, "X0001", 5, 1)); err != nil {
		t.Fatalf("AppendCashSession: %v", err)
	}
	sessions, err := s.GetCashSessionsSince(date)
	if err != nil {
		t.Fatalf("GetCashSessionsSince: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("len(sessions) = %d, want 1 (cutoff equal to the row's date must be inclusive)", len(sessions))
	}
}

// ---------------------------------------------------------------------------
// Tournaments — two-file correlation, both arrival orders
// ---------------------------------------------------------------------------

func TestAppendTournament_SummaryFirstThenHandStats(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	tourn := parsers.Tournament{
		Date:         date,
		Type:         "spingo",
		TournamentID: "1000000001",
		BuyInUSD:     0.55,
		PrizePoolUSD: 1.65,
		Multiplier:   floatPtr(3),
		FinishPos:    1,
		TotalPlayers: 3,
		NetUSD:       1.10, // 1.65 payout - 0.55 buyin
	}

	netUSD, err := s.AppendTournament(tourn)
	if err != nil {
		t.Fatalf("AppendTournament: %v", err)
	}
	if !closeEnough(netUSD, 1.10) {
		t.Errorf("netUSD = %v, want 1.10 (no hand stats yet, no bounty to fold in)", netUSD)
	}

	if err := s.UpdateTournamentHandStats(tourn.TournamentID, 45.0, 20.0, 100, 0, false); err != nil {
		t.Fatalf("UpdateTournamentHandStats: %v", err)
	}

	tournaments, err := s.GetTournamentsSince(time.Time{})
	if err != nil {
		t.Fatalf("GetTournamentsSince: %v", err)
	}
	if len(tournaments) != 1 {
		t.Fatalf("len(tournaments) = %d, want 1", len(tournaments))
	}
	got := tournaments[0]
	if got.Type != "spingo" || got.TournamentID != "1000000001" || !closeEnough(got.NetUSD, 1.10) {
		t.Errorf("tournament = %+v, unexpected", got)
	}
	if got.Multiplier == nil || !closeEnough(*got.Multiplier, 3) {
		t.Errorf("Multiplier = %v, want 3", got.Multiplier)
	}
	if got.VPIP == nil || !closeEnough(*got.VPIP, 45.0) {
		t.Errorf("VPIP = %v, want 45.0 (GetTournamentsSince must return hand-file stats too)", got.VPIP)
	}
	if got.PFR == nil || !closeEnough(*got.PFR, 20.0) {
		t.Errorf("PFR = %v, want 20.0", got.PFR)
	}
	if got.Hands == nil || *got.Hands != 100 {
		t.Errorf("Hands = %v, want 100", got.Hands)
	}

	bankroll, err := s.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if !closeEnough(bankroll, 1.10) {
		t.Errorf("bankroll = %v, want 1.10", bankroll)
	}
}

func TestAppendTournament_HandStatsFirstThenSummary(t *testing.T) {
	s := newTestStore(t)
	tournamentID := "1000000002"

	// Hand history file arrives before the summary — a partial row must be
	// created, and it must not show up in GetTournamentsSince yet (no Date).
	if err := s.UpdateTournamentHandStats(tournamentID, 30.0, 15.0, 80, 0, false); err != nil {
		t.Fatalf("UpdateTournamentHandStats (partial): %v", err)
	}

	tournaments, err := s.GetTournamentsSince(time.Time{})
	if err != nil {
		t.Fatalf("GetTournamentsSince: %v", err)
	}
	if len(tournaments) != 0 {
		t.Fatalf("len(tournaments) = %d, want 0 (partial row must be hidden until the summary arrives)", len(tournaments))
	}

	date := mustTime(t, "2026-07-10T21:00:00Z")
	tourn := parsers.Tournament{
		Date:         date,
		Type:         "mtt",
		TournamentID: tournamentID,
		BuyInUSD:     5.00,
		PrizePoolUSD: 100,
		FinishPos:    2,
		TotalPlayers: 20,
		NetUSD:       10.00,
	}
	netUSD, err := s.AppendTournament(tourn)
	if err != nil {
		t.Fatalf("AppendTournament: %v", err)
	}
	if !closeEnough(netUSD, 10.00) {
		t.Errorf("netUSD = %v, want 10.00 (no bounty for a non-knockout tournament)", netUSD)
	}

	tournaments, err = s.GetTournamentsSince(time.Time{})
	if err != nil {
		t.Fatalf("GetTournamentsSince: %v", err)
	}
	if len(tournaments) != 1 {
		t.Fatalf("len(tournaments) = %d, want 1 once the summary lands", len(tournaments))
	}
	if tournaments[0].Type != "mtt" {
		t.Errorf("Type = %q, want %q", tournaments[0].Type, "mtt")
	}
}

func TestKnockout_SummaryFirstThenHandStats(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	tourn := parsers.Tournament{
		Date: date, Type: "mtt", TournamentID: "2000000001",
		BuyInUSD: 5.00, PrizePoolUSD: 100, FinishPos: 3, TotalPlayers: 50, NetUSD: -5.00,
	}
	if _, err := s.AppendTournament(tourn); err != nil {
		t.Fatalf("AppendTournament: %v", err)
	}
	// Hand file reveals it's a knockout tournament, screenName won a bounty.
	if err := s.UpdateTournamentHandStats(tourn.TournamentID, 22.0, 10.0, 120, 0.90, true); err != nil {
		t.Fatalf("UpdateTournamentHandStats: %v", err)
	}

	var typeVal string
	var netUSD, prizeWon float64
	err := s.db.QueryRow(`SELECT type, net_usd, prize_won_usd FROM tournaments WHERE tournament_id = ?`,
		tourn.TournamentID).Scan(&typeVal, &netUSD, &prizeWon)
	if err != nil {
		t.Fatalf("reading tournament row: %v", err)
	}
	if typeVal != "mtt-ko" {
		t.Errorf("Type = %q, want %q", typeVal, "mtt-ko")
	}
	// -5.00 (baseline loss) + 0.90 (bounty) = -4.10; prize won = -4.10 + 5.00 = 0.90
	if !closeEnough(netUSD, -4.10) {
		t.Errorf("NetUSD = %v, want -4.10 (loss on paper, bounty makes it less bad)", netUSD)
	}
	if !closeEnough(prizeWon, 0.90) {
		t.Errorf("PrizeWon = %v, want 0.90", prizeWon)
	}

	bankroll, err := s.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if !closeEnough(bankroll, -4.10) {
		t.Errorf("bankroll = %v, want -4.10 (ledger must reconcile too)", bankroll)
	}
}

func TestKnockout_HandStatsFirstThenSummary(t *testing.T) {
	s := newTestStore(t)
	tournamentID := "2000000002"

	if err := s.UpdateTournamentHandStats(tournamentID, 22.0, 10.0, 120, 0.90, true); err != nil {
		t.Fatalf("UpdateTournamentHandStats (partial, knockout): %v", err)
	}

	var typeCell string
	if err := s.db.QueryRow(`SELECT type FROM tournaments WHERE tournament_id = ?`, tournamentID).Scan(&typeCell); err != nil {
		t.Fatalf("reading partial row: %v", err)
	}
	if typeCell != knockoutPlaceholder {
		t.Errorf("partial row Type = %q, want placeholder %q", typeCell, knockoutPlaceholder)
	}

	date := mustTime(t, "2026-07-10T22:00:00Z")
	tourn := parsers.Tournament{
		Date: date, Type: "spingo", TournamentID: tournamentID,
		PrizePoolUSD: 3.30, Multiplier: floatPtr(2), BuyInUSD: 1.65, FinishPos: 1, TotalPlayers: 3, NetUSD: 1.65,
	}
	netUSD, err := s.AppendTournament(tourn)
	if err != nil {
		t.Fatalf("AppendTournament: %v", err)
	}
	// 1.65 (baseline) + 0.90 (bounty already recorded) = 2.55
	if !closeEnough(netUSD, 2.55) {
		t.Errorf("netUSD = %v, want 2.55 (placeholder-recorded bounty must be folded in)", netUSD)
	}

	var typeVal string
	if err := s.db.QueryRow(`SELECT type FROM tournaments WHERE tournament_id = ?`, tournamentID).Scan(&typeVal); err != nil {
		t.Fatalf("reading final row: %v", err)
	}
	if typeVal != "spingo-ko" {
		t.Errorf("Type = %q, want %q (placeholder must become the real base type + suffix)", typeVal, "spingo-ko")
	}
}

// TestBounty_GrowingHandFileReparse mirrors a knockout tournament's hand
// history file being reparsed repeatedly as it grows across the whole
// tournament, each time recomputing the cumulative bounty from scratch.
func TestBounty_GrowingHandFileReparse(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	tourn := parsers.Tournament{
		Date: date, Type: "mtt", TournamentID: "3000000001",
		BuyInUSD: 5.00, PrizePoolUSD: 4.88, FinishPos: 1, TotalPlayers: 50, NetUSD: -0.12,
	}
	if _, err := s.AppendTournament(tourn); err != nil {
		t.Fatalf("AppendTournament: %v", err)
	}

	// First KO.
	if err := s.UpdateTournamentHandStats(tourn.TournamentID, 20, 10, 50, 0.90, true); err != nil {
		t.Fatalf("first reparse: %v", err)
	}
	bankroll, _ := s.GetCurrentBankroll()
	if !closeEnough(bankroll, 0.78) { // -0.12 + 0.90
		t.Errorf("bankroll after 1st KO = %v, want 0.78", bankroll)
	}

	// Second KO — cumulative bounty grows to 1.80 (not incremented, recomputed).
	if err := s.UpdateTournamentHandStats(tourn.TournamentID, 20, 10, 90, 1.80, true); err != nil {
		t.Fatalf("second reparse: %v", err)
	}
	bankroll, _ = s.GetCurrentBankroll()
	if !closeEnough(bankroll, 1.68) { // -0.12 + 1.80
		t.Errorf("bankroll after 2nd KO = %v, want 1.68 (must replace, not add, the previous bounty)", bankroll)
	}

	// Unchanged reparse (no new KO since last hand) must be a no-op.
	if err := s.UpdateTournamentHandStats(tourn.TournamentID, 20, 10, 90, 1.80, true); err != nil {
		t.Fatalf("third (unchanged) reparse: %v", err)
	}
	bankroll, _ = s.GetCurrentBankroll()
	if !closeEnough(bankroll, 1.68) {
		t.Errorf("bankroll after unchanged reparse = %v, want 1.68 (unchanged)", bankroll)
	}

	var ledgerRowCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM bankroll_ledger`).Scan(&ledgerRowCount); err != nil {
		t.Fatalf("counting ledger rows: %v", err)
	}
	if ledgerRowCount != 1 {
		t.Errorf("ledger row count = %d, want 1 (every reparse must upsert, never append)", ledgerRowCount)
	}
}

// TestAppendTournament_DuplicateCallIsIdempotent covers the satellite/ticket
// flow and stray duplicate file events: AppendTournament can legitimately run
// more than once for the same TournamentID, and must not double-count the
// ledger even though the Tournaments-equivalent table stays deduplicated by
// tournament_id (UNIQUE).
func TestAppendTournament_DuplicateCallIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	tourn := parsers.Tournament{
		Date: date, Type: "mtt", TournamentID: "4000000001",
		BuyInUSD: 2.00, PrizePoolUSD: 40, FinishPos: 5, TotalPlayers: 40, NetUSD: 3.00,
	}

	if _, err := s.AppendTournament(tourn); err != nil {
		t.Fatalf("first AppendTournament: %v", err)
	}
	if _, err := s.AppendTournament(tourn); err != nil {
		t.Fatalf("duplicate AppendTournament: %v", err)
	}

	tournaments, err := s.GetTournamentsSince(time.Time{})
	if err != nil {
		t.Fatalf("GetTournamentsSince: %v", err)
	}
	if len(tournaments) != 1 {
		t.Fatalf("len(tournaments) = %d, want 1 (tournament_id is UNIQUE)", len(tournaments))
	}

	bankroll, err := s.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if !closeEnough(bankroll, 3.00) {
		t.Errorf("bankroll = %v, want 3.00 (duplicate call must not double-count the ledger)", bankroll)
	}

	var ledgerRowCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM bankroll_ledger`).Scan(&ledgerRowCount); err != nil {
		t.Fatalf("counting ledger rows: %v", err)
	}
	if ledgerRowCount != 1 {
		t.Errorf("ledger row count = %d, want 1", ledgerRowCount)
	}
}

func TestAppendTournament_PrizePoolMutualExclusivity(t *testing.T) {
	s := newTestStore(t)

	cashPool := parsers.Tournament{
		Date: mustTime(t, "2026-07-10T20:00:00Z"), Type: "spingo", TournamentID: "5000000001",
		BuyInUSD: 0.55, PrizePoolUSD: 1.65, Multiplier: floatPtr(3), FinishPos: 1, TotalPlayers: 3, NetUSD: 1.10,
	}
	ticketPool := parsers.Tournament{
		Date: mustTime(t, "2026-07-10T20:05:00Z"), Type: "spingo", TournamentID: "5000000002",
		PrizePoolDescription: "$1.00 Power Path Step 2 Ticket", BuyInUSD: 0.25, FinishPos: 1, TotalPlayers: 3, NetUSD: -0.25,
	}

	if _, err := s.AppendTournament(cashPool); err != nil {
		t.Fatalf("cashPool AppendTournament: %v", err)
	}
	if _, err := s.AppendTournament(ticketPool); err != nil {
		t.Fatalf("ticketPool AppendTournament: %v", err)
	}

	tournaments, err := s.GetTournamentsSince(time.Time{})
	if err != nil {
		t.Fatalf("GetTournamentsSince: %v", err)
	}
	if len(tournaments) != 2 {
		t.Fatalf("len(tournaments) = %d, want 2", len(tournaments))
	}

	byID := map[string]parsers.Tournament{}
	for _, tr := range tournaments {
		byID[tr.TournamentID] = tr
	}

	cash := byID["5000000001"]
	if cash.PrizePoolDescription != "" {
		t.Errorf("cash pool PrizePoolDescription = %q, want empty", cash.PrizePoolDescription)
	}
	if !closeEnough(cash.PrizePoolUSD, 1.65) {
		t.Errorf("cash pool PrizePoolUSD = %v, want 1.65", cash.PrizePoolUSD)
	}
	if cash.Multiplier == nil || !closeEnough(*cash.Multiplier, 3) {
		t.Errorf("cash pool Multiplier = %v, want 3", cash.Multiplier)
	}

	ticket := byID["5000000002"]
	if ticket.PrizePoolDescription != "$1.00 Power Path Step 2 Ticket" {
		t.Errorf("ticket pool PrizePoolDescription = %q, want the ticket text", ticket.PrizePoolDescription)
	}
	if ticket.PrizePoolUSD != 0 {
		t.Errorf("ticket pool PrizePoolUSD = %v, want 0 (mutually exclusive with description)", ticket.PrizePoolUSD)
	}
	if ticket.Multiplier != nil {
		t.Errorf("ticket pool Multiplier = %v, want nil (no real cash pool to divide by)", ticket.Multiplier)
	}
}

func TestAppendTournament_MultiplierNilForMTT(t *testing.T) {
	s := newTestStore(t)
	mtt := parsers.Tournament{
		Date: mustTime(t, "2026-07-10T20:00:00Z"), Type: "mtt", TournamentID: "6000000001",
		BuyInUSD: 5, PrizePoolUSD: 500, FinishPos: 10, TotalPlayers: 100, NetUSD: 45,
	}
	if _, err := s.AppendTournament(mtt); err != nil {
		t.Fatalf("AppendTournament: %v", err)
	}
	tournaments, err := s.GetTournamentsSince(time.Time{})
	if err != nil {
		t.Fatalf("GetTournamentsSince: %v", err)
	}
	if len(tournaments) != 1 || tournaments[0].Multiplier != nil {
		t.Errorf("MTT Multiplier = %v, want nil", tournaments[0].Multiplier)
	}
}

func TestGetTournamentsSince_DateFiltering(t *testing.T) {
	s := newTestStore(t)
	early := parsers.Tournament{
		Date: mustTime(t, "2026-07-01T10:00:00Z"), Type: "mtt", TournamentID: "7000000001",
		BuyInUSD: 5, PrizePoolUSD: 100, FinishPos: 1, TotalPlayers: 10, NetUSD: 20,
	}
	late := parsers.Tournament{
		Date: mustTime(t, "2026-07-15T10:00:00Z"), Type: "mtt", TournamentID: "7000000002",
		BuyInUSD: 5, PrizePoolUSD: 100, FinishPos: 1, TotalPlayers: 10, NetUSD: 20,
	}
	if _, err := s.AppendTournament(early); err != nil {
		t.Fatalf("early: %v", err)
	}
	if _, err := s.AppendTournament(late); err != nil {
		t.Fatalf("late: %v", err)
	}

	cutoff := mustTime(t, "2026-07-10T00:00:00Z")
	tournaments, err := s.GetTournamentsSince(cutoff)
	if err != nil {
		t.Fatalf("GetTournamentsSince: %v", err)
	}
	if len(tournaments) != 1 || tournaments[0].TournamentID != "7000000002" {
		t.Errorf("tournaments = %+v, want only the late one", tournaments)
	}
}

// ---------------------------------------------------------------------------
// Bankroll ledger (deposit/withdrawal), GetCurrentBankroll, GetSheetURL
// ---------------------------------------------------------------------------

func TestAppendLedgerEntry_DepositAndWithdrawalAccumulate(t *testing.T) {
	s := newTestStore(t)
	if err := s.AppendLedgerEntry("deposit", 100); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if err := s.AppendLedgerEntry("withdrawal", -30); err != nil {
		t.Fatalf("withdrawal: %v", err)
	}
	bankroll, err := s.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if !closeEnough(bankroll, 70) {
		t.Errorf("bankroll = %v, want 70", bankroll)
	}
}

// TestAppendLedgerEntry_RunningTotalCanGoNegative confirms the running total
// is stored as-is with no floor at zero — a player can withdraw more than
// their tracked bankroll (e.g. a deposit made outside the tracker) and the
// ledger must reflect that honestly rather than clamping.
func TestAppendLedgerEntry_RunningTotalCanGoNegative(t *testing.T) {
	s := newTestStore(t)
	if err := s.AppendLedgerEntry("deposit", 20); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if err := s.AppendLedgerEntry("withdrawal", -50); err != nil {
		t.Fatalf("withdrawal: %v", err)
	}
	bankroll, err := s.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if !closeEnough(bankroll, -30) {
		t.Errorf("bankroll = %v, want -30 (negative running totals are valid, not clamped)", bankroll)
	}

	entries, err := s.GetLedgerEntriesSince(time.Time{})
	if err != nil {
		t.Fatalf("GetLedgerEntriesSince: %v", err)
	}
	if len(entries) != 2 || !closeEnough(entries[1].RunningTotal, -30) {
		t.Errorf("entries = %+v, want second row's running total -30", entries)
	}
}

func TestGetCurrentBankroll_EmptyLedger(t *testing.T) {
	s := newTestStore(t)
	bankroll, err := s.GetCurrentBankroll()
	if err != nil {
		t.Fatalf("GetCurrentBankroll: %v", err)
	}
	if bankroll != 0 {
		t.Errorf("bankroll = %v, want 0 for an empty ledger", bankroll)
	}
}

func TestGetLedgerEntriesSince_ZeroTimeReturnsWholeLedger(t *testing.T) {
	s := newTestStore(t)
	if err := s.AppendLedgerEntry("deposit", 50); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if err := s.AppendLedgerEntry("deposit", 25); err != nil {
		t.Fatalf("deposit 2: %v", err)
	}
	entries, err := s.GetLedgerEntriesSince(time.Time{})
	if err != nil {
		t.Fatalf("GetLedgerEntriesSince: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
}

func TestGetLedgerEntriesSince_EmptyLedgerReturnsNoRowsNoError(t *testing.T) {
	s := newTestStore(t)
	entries, err := s.GetLedgerEntriesSince(time.Time{})
	if err != nil {
		t.Fatalf("GetLedgerEntriesSince: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("len(entries) = %d, want 0", len(entries))
	}
}

func TestGetSheetURL_ReturnsEmptyString(t *testing.T) {
	s := newTestStore(t)
	if got := s.GetSheetURL(); got != "" {
		t.Errorf("GetSheetURL() = %q, want empty string (sqlite backend has no Google Sheet)", got)
	}
}

// ---------------------------------------------------------------------------
// Mixed-entry-type ledger cascade
// ---------------------------------------------------------------------------

// TestLedgerCascade_MixedEntryTypes exercises the exact edge case CLAUDE.md
// calls out: a mid-session deposit landing after a session's own ledger row
// while that row is still being corrected hand-to-hand. Every later row —
// regardless of its own entry type — must have its running total shifted.
func TestLedgerCascade_MixedEntryTypes(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")

	if err := s.AppendCashSession(buildCashSession(date, "M0001", 5, -2)); err != nil {
		t.Fatalf("cash session hand 1: %v", err)
	}
	if err := s.AppendLedgerEntry("deposit", 50); err != nil {
		t.Fatalf("mid-session deposit: %v", err)
	}
	// The session keeps playing and its net changes.
	if err := s.AppendCashSession(buildCashSession(date, "M0001", 10, 4)); err != nil {
		t.Fatalf("cash session hand 2: %v", err)
	}

	entries, err := s.GetLedgerEntriesSince(time.Time{})
	if err != nil {
		t.Fatalf("GetLedgerEntriesSince: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (session row upserted, not appended again)", len(entries))
	}
	if !closeEnough(entries[0].AmountUSD, 4) || !closeEnough(entries[0].RunningTotal, 4) {
		t.Errorf("session row = (amount %v, total %v), want (4, 4)", entries[0].AmountUSD, entries[0].RunningTotal)
	}
	if !closeEnough(entries[1].AmountUSD, 50) || !closeEnough(entries[1].RunningTotal, 54) {
		t.Errorf("deposit row = (amount %v, total %v), want (50, 54) — must shift to follow the corrected session row",
			entries[1].AmountUSD, entries[1].RunningTotal)
	}
}

// ---------------------------------------------------------------------------
// GetAll* — /export's full-fidelity readers
// ---------------------------------------------------------------------------

func TestGetAllCashSessions_IncludesSessionID(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	if err := s.AppendCashSession(buildCashSession(date, "200000000001", 50, -5.25)); err != nil {
		t.Fatalf("AppendCashSession: %v", err)
	}

	records, err := s.GetAllCashSessions()
	if err != nil {
		t.Fatalf("GetAllCashSessions: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	want := cashSessionID(date, "200000000001")
	if records[0].SessionID != want {
		t.Errorf("SessionID = %q, want %q", records[0].SessionID, want)
	}
	if records[0].Hands != 50 || !closeEnough(records[0].NetUSD, -5.25) {
		t.Errorf("record = %+v, unexpected", records[0])
	}
}

func TestGetAllCashSessions_ReparseStillReturnsOneRow(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	if err := s.AppendCashSession(buildCashSession(date, "200000000001", 10, -1.00)); err != nil {
		t.Fatalf("first AppendCashSession: %v", err)
	}
	if err := s.AppendCashSession(buildCashSession(date, "200000000001", 25, 3.50)); err != nil {
		t.Fatalf("second AppendCashSession: %v", err)
	}

	records, err := s.GetAllCashSessions()
	if err != nil {
		t.Fatalf("GetAllCashSessions: %v", err)
	}
	if len(records) != 1 || records[0].Hands != 25 {
		t.Errorf("records = %+v, want a single row reflecting the latest reparse", records)
	}
}

func TestGetAllTournaments_IncludesHandStatsAndBounty(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	tourn := parsers.Tournament{
		Date: date, Type: "mtt", TournamentID: "8000000001",
		BuyInUSD: 5.00, PrizePoolUSD: 100, FinishPos: 3, TotalPlayers: 50, NetUSD: -5.00,
	}
	if _, err := s.AppendTournament(tourn); err != nil {
		t.Fatalf("AppendTournament: %v", err)
	}
	if err := s.UpdateTournamentHandStats(tourn.TournamentID, 22.0, 10.0, 120, 0.90, true); err != nil {
		t.Fatalf("UpdateTournamentHandStats: %v", err)
	}

	records, err := s.GetAllTournaments()
	if err != nil {
		t.Fatalf("GetAllTournaments: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	got := records[0]
	if got.Type != "mtt-ko" {
		t.Errorf("Type = %q, want %q", got.Type, "mtt-ko")
	}
	if got.VPIP == nil || !closeEnough(*got.VPIP, 22.0) {
		t.Errorf("VPIP = %v, want 22.0", got.VPIP)
	}
	if got.PFR == nil || !closeEnough(*got.PFR, 10.0) {
		t.Errorf("PFR = %v, want 10.0", got.PFR)
	}
	if got.Hands == nil || *got.Hands != 120 {
		t.Errorf("Hands = %v, want 120", got.Hands)
	}
	if !closeEnough(got.BountyWonUSD, 0.90) {
		t.Errorf("BountyWonUSD = %v, want 0.90", got.BountyWonUSD)
	}
	// -5.00 baseline + 0.90 bounty = -4.10; prize won = -4.10 + 5.00 = 0.90
	if !closeEnough(got.NetUSD, -4.10) {
		t.Errorf("NetUSD = %v, want -4.10", got.NetUSD)
	}
	if !closeEnough(got.PrizeWonUSD, 0.90) {
		t.Errorf("PrizeWonUSD = %v, want 0.90", got.PrizeWonUSD)
	}
}

func TestGetAllTournaments_SkipsPartialRows(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpdateTournamentHandStats("8000000002", 30.0, 15.0, 80, 0, false); err != nil {
		t.Fatalf("UpdateTournamentHandStats (partial): %v", err)
	}
	records, err := s.GetAllTournaments()
	if err != nil {
		t.Fatalf("GetAllTournaments: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("len(records) = %d, want 0 (partial row must be hidden until the summary arrives)", len(records))
	}
}

func TestGetAllTournaments_VPIPPFRHandsNilBeforeHandFile(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	tourn := parsers.Tournament{
		Date: date, Type: "mtt", TournamentID: "8000000003",
		BuyInUSD: 5.00, PrizePoolUSD: 100, FinishPos: 3, TotalPlayers: 50, NetUSD: 5.00,
	}
	if _, err := s.AppendTournament(tourn); err != nil {
		t.Fatalf("AppendTournament: %v", err)
	}
	records, err := s.GetAllTournaments()
	if err != nil {
		t.Fatalf("GetAllTournaments: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	if records[0].VPIP != nil || records[0].PFR != nil || records[0].Hands != nil {
		t.Errorf("VPIP/PFR/Hands = %v/%v/%v, want nil (no hand file parsed yet)", records[0].VPIP, records[0].PFR, records[0].Hands)
	}
}

func TestGetAllTimeAnalysis_IncludesSessionID(t *testing.T) {
	s := newTestStore(t)
	date := mustTime(t, "2026-07-10T20:00:00Z")
	if err := s.AppendCashSession(buildCashSession(date, "200000000001", 50, -5.25)); err != nil {
		t.Fatalf("AppendCashSession: %v", err)
	}

	records, err := s.GetAllTimeAnalysis()
	if err != nil {
		t.Fatalf("GetAllTimeAnalysis: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	want := cashSessionID(date, "200000000001")
	if records[0].SessionID != want {
		t.Errorf("SessionID = %q, want %q", records[0].SessionID, want)
	}
	if records[0].Hour != date.Hour() || records[0].DayOfWeek != date.Weekday().String() {
		t.Errorf("record = %+v, unexpected", records[0])
	}
}

func TestGetAllCashSessions_EmptyReturnsNoRowsNoError(t *testing.T) {
	s := newTestStore(t)
	records, err := s.GetAllCashSessions()
	if err != nil {
		t.Fatalf("GetAllCashSessions: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("len(records) = %d, want 0", len(records))
	}
}

func floatPtr(f float64) *float64 { return &f }
