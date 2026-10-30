// Package sqlitestore implements storage.Store against a local SQLite
// database file. It mirrors the Google Sheets backend's observable behavior
// method-for-method — same upsert keys, same running-total cascading, same
// two-file tournament correlation — but stores dates as RFC3339 UTC strings
// rather than the sheets backend's local "2006-01-02 15:04" format.
package sqlitestore

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

const dateFormat = time.RFC3339

const schema = `
CREATE TABLE IF NOT EXISTS cash_sessions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	date TEXT NOT NULL,
	stake TEXT NOT NULL,
	hands INTEGER NOT NULL,
	duration_min INTEGER NOT NULL,
	net_cents INTEGER NOT NULL,
	rake_cents INTEGER NOT NULL,
	bb100 REAL NOT NULL,
	vpip REAL NOT NULL,
	pfr REAL NOT NULL,
	session_id TEXT UNIQUE
);

CREATE TABLE IF NOT EXISTS tournaments (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	date TEXT,
	type TEXT NOT NULL,
	tournament_id TEXT NOT NULL UNIQUE,
	buy_in_cents INTEGER,
	prize_pool_cents INTEGER,
	prize_pool_description TEXT,
	multiplier REAL,
	finish_pos INTEGER,
	total_players INTEGER,
	net_cents INTEGER,
	vpip REAL,
	pfr REAL,
	bounty_cents INTEGER,
	prize_won_cents INTEGER,
	hands INTEGER
);

CREATE TABLE IF NOT EXISTS bankroll_ledger (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	date TEXT NOT NULL,
	type TEXT NOT NULL,
	amount_cents INTEGER NOT NULL,
	running_total_cents INTEGER NOT NULL,
	session_id TEXT
);
`

// columnDef is one column's name and bare SQLite type, used by
// expectedColumns/migrateSchema below.
type columnDef struct {
	name string
	typ  string
}

// tableDef pairs a table with the columns it's expected to have.
type tableDef struct {
	table   string
	columns []columnDef
}

// expectedColumns is the single source of truth migrateSchema diffs against
// PRAGMA table_info to find columns missing from a database created by an
// older binary — e.g. tournaments.hands, added after this backend's initial
// release, which CREATE TABLE IF NOT EXISTS alone never retrofits onto a
// table that already exists. Adding a new column to any table: add it to
// that table's CREATE TABLE statement in schema above AND add its
// (name, type) pair here — migrateSchema then ALTERs it into every existing
// database automatically, no other code changes needed.
//
// Types here are always bare (no NOT NULL, no DEFAULT) — SQLite refuses
// ALTER TABLE ADD COLUMN with NOT NULL and no non-NULL default on a table
// that already has rows, so a retrofitted column must always be optional.
// Existing rows get NULL for it, same as tournaments.hands does for any
// tournament recorded before this column existed.
var expectedColumns = []tableDef{
	{"cash_sessions", []columnDef{
		{"date", "TEXT"}, {"stake", "TEXT"}, {"hands", "INTEGER"},
		{"duration_min", "INTEGER"}, {"net_cents", "INTEGER"}, {"rake_cents", "INTEGER"},
		{"bb100", "REAL"}, {"vpip", "REAL"}, {"pfr", "REAL"}, {"session_id", "TEXT"},
	}},
	{"tournaments", []columnDef{
		{"date", "TEXT"}, {"type", "TEXT"}, {"tournament_id", "TEXT"},
		{"buy_in_cents", "INTEGER"}, {"prize_pool_cents", "INTEGER"}, {"prize_pool_description", "TEXT"},
		{"multiplier", "REAL"}, {"finish_pos", "INTEGER"}, {"total_players", "INTEGER"},
		{"net_cents", "INTEGER"}, {"vpip", "REAL"}, {"pfr", "REAL"}, {"bounty_cents", "INTEGER"},
		{"prize_won_cents", "INTEGER"}, {"hands", "INTEGER"},
	}},
	{"bankroll_ledger", []columnDef{
		{"date", "TEXT"}, {"type", "TEXT"}, {"amount_cents", "INTEGER"},
		{"running_total_cents", "INTEGER"}, {"session_id", "TEXT"},
	}},
}

// Store persists poker session data to a local SQLite database file.
type Store struct {
	db *sql.DB
}

var _ storage.Store = (*Store)(nil)

// NewStore opens (creating if necessary) the SQLite database at dbPath,
// applies the schema if the tables don't already exist, and migrates any
// existing tables that are missing columns a newer binary expects.
func NewStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", dbPath, err)
	}
	// The worker and bot goroutines both call methods on this Store
	// concurrently. SQLite serializes writes at the file level regardless,
	// but database/sql's default pool can still open a second connection
	// and hand back SQLITE_BUSY under overlap. Capping the pool at one
	// connection routes every call through the same connection instead,
	// so database/sql queues them rather than surfacing a busy error.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("opening %s: %w", dbPath, err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	// The Time Analysis feature was removed; shed the orphan table from any
	// database created by an older binary so it doesn't linger unused.
	if _, err := db.Exec(`DROP TABLE IF EXISTS time_analysis`); err != nil {
		return nil, fmt.Errorf("dropping legacy time_analysis table: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrateSchema(); err != nil {
		return nil, fmt.Errorf("migrating schema: %w", err)
	}
	return s, nil
}

// migrateSchema adds any column listed in expectedColumns but missing from
// the database on disk — the gap CREATE TABLE IF NOT EXISTS leaves behind
// when a binary update adds a new column to a table an older version of
// tracker.db already created. Idempotent: a column already present is left
// untouched, so running this against an up-to-date database is a no-op.
func (s *Store) migrateSchema() error {
	for _, td := range expectedColumns {
		existing, err := s.existingColumns(td.table)
		if err != nil {
			return fmt.Errorf("reading columns for %s: %w", td.table, err)
		}
		for _, col := range td.columns {
			if existing[col.name] {
				continue
			}
			if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", td.table, col.name, col.typ)); err != nil {
				return fmt.Errorf("adding column %s.%s: %w", td.table, col.name, err)
			}
		}
	}
	return nil
}

// existingColumns returns the set of column names table currently has, via
// PRAGMA table_info — SQLite has no information_schema, so this is the only
// reliable way to introspect a table's columns at runtime.
func (s *Store) existingColumns(table string) (map[string]bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	existing := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		existing[name] = true
	}
	return existing, rows.Err()
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// cashSessionID is the canonical key used for new rows by both backends.
func cashSessionID(date time.Time, firstHandID string) string {
	return storage.CashSessionIdentities(date, firstHandID)[0]
}

// write serializes a complete mutation on the store's single connection and
// commits its result rows and ledger corrections together.
func (s *Store) write(apply func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning storage write: %w", err)
	}
	defer tx.Rollback()
	if err := apply(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing storage write: %w", err)
	}
	return nil
}

// AppendCashSession upserts one row in cash_sessions and one row in
// bankroll_ledger — both keyed by the same cashSessionID, so a re-processed
// cash session corrects its existing rows in place instead of appending a new
// set. Mirrors sheets.Client.AppendCashSession.
func (s *Store) AppendCashSession(session parsers.CashSession) error {
	if err := storage.ValidateCashSession(session); err != nil {
		return err
	}
	return s.write(func(tx *sql.Tx) error { return s.appendCashSession(tx, session) })
}

func (s *Store) appendCashSession(tx *sql.Tx, session parsers.CashSession) error {
	keys := storage.CashSessionIdentities(session.Date, session.FirstHandID)
	id := keys[0]
	rows, err := tx.Query("SELECT session_id FROM cash_sessions WHERE session_id IN (?,?)", keys[0], keys[1])
	if err != nil {
		return fmt.Errorf("resolving cash identity: %w", err)
	}
	count := 0
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		count++
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return rowErr
	}
	if count > 1 {
		return fmt.Errorf("cash session has conflicting legacy identities")
	}

	if err := s.upsertCashSessionsRow(tx, id, session); err != nil {
		return err
	}
	return s.upsertLedgerCashSessionRow(tx, id, session.Date, session.NetCents)
}

func (s *Store) upsertCashSessionsRow(tx *sql.Tx, id string, session parsers.CashSession) error {
	_, err := tx.Exec(`
		INSERT INTO cash_sessions (date, stake, hands, duration_min, net_cents, rake_cents, bb100, vpip, pfr, session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			date = excluded.date, stake = excluded.stake, hands = excluded.hands,
			duration_min = excluded.duration_min, net_cents = excluded.net_cents,
			rake_cents = excluded.rake_cents, bb100 = excluded.bb100, vpip = excluded.vpip, pfr = excluded.pfr
	`, session.Date.UTC().Format(dateFormat), session.Stake, session.Hands, session.DurationMin,
		session.NetCents, session.RakeCents, session.BB100, session.VPIP, session.PFR, id)
	if err != nil {
		return fmt.Errorf("upserting cash session row: %w", err)
	}
	return nil
}

// upsertLedgerCashSessionRow writes (or corrects, if it already exists) the
// cash session's own bankroll_ledger row, keyed by the session ID — the same
// upsert semantics as upsertCashSessionsRow.
func (s *Store) upsertLedgerCashSessionRow(tx *sql.Tx, id string, date time.Time, netCents money.Cents) error {
	return s.upsertLedgerRow(tx, id, date, "cash_session", netCents)
}

// upsertLedgerTournamentRow corrects the tournament's own bankroll_ledger row
// in place, keyed by TournamentID. AppendTournament can legitimately run more
// than once for the same TournamentID (satellite/ticket flow, or a stray
// duplicate file event), so this is idempotency, not a hand-to-hand
// correction — without it the ledger would double-count a tournament whose
// tournaments row stays correctly deduplicated by tournament_id.
func (s *Store) upsertLedgerTournamentRow(tx *sql.Tx, tournamentID string, date time.Time, netCents money.Cents) error {
	return s.upsertLedgerRow(tx, tournamentID, date, "tournament", netCents)
}

// upsertLedgerRow finds the ledger row previously written for sessionID (the
// Session ID column — a cash session ID or a TournamentID) and corrects its
// amountCents and running total in place, cascading the running-total recompute to
// every later row (see cascadeRunningTotals). If no such row exists yet, it
// appends a fresh one built off the ledger's current last running total.
func (s *Store) upsertLedgerRow(tx *sql.Tx, sessionID string, date time.Time, entryType string, amountCents money.Cents) error {
	var rowID int64
	err := tx.QueryRow(`SELECT id FROM bankroll_ledger WHERE session_id = ?`, sessionID).Scan(&rowID)
	if err == sql.ErrNoRows {
		prevCents, err := lastRunningTotal(tx.QueryRow(lastRunningTotalQuery))
		if err != nil {
			return err
		}
		totalCents, err := money.Add(prevCents, amountCents)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`
			INSERT INTO bankroll_ledger (date, type, amount_cents, running_total_cents, session_id)
			VALUES (?, ?, ?, ?, ?)
		`, date.UTC().Format(dateFormat), entryType, amountCents, totalCents, sessionID)
		if err != nil {
			return fmt.Errorf("appending ledger row for %s: %w", sessionID, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("looking up ledger row for %s: %w", sessionID, err)
	}
	return s.cascadeLedgerUpdate(tx, rowID, amountCents)
}

// cascadeLedgerUpdate sets the bankroll_ledger row identified by rowID to
// newAmountCents and recomputes running_total_cents for that row and every row after it
// (by insertion order). A plain update of rowID alone is only correct when
// it's already the ledger's last row — with more than one table open at once,
// a second table's session row (or a mid-session deposit) can land after this
// row while it's still being corrected, and that later row's stored running
// total was computed from this row's old amountCents.
func (s *Store) cascadeLedgerUpdate(tx *sql.Tx, rowID int64, newAmountCents money.Cents) error {
	var baseCents money.Cents
	err := tx.QueryRow(`
		SELECT running_total_cents FROM bankroll_ledger WHERE id < ? ORDER BY id DESC LIMIT 1
	`, rowID).Scan(&baseCents)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("reading runningCents totalCents before row %d: %w", rowID, err)
	}

	rows, err := tx.Query(`
		SELECT id, amount_cents FROM bankroll_ledger WHERE id >= ? ORDER BY id ASC
	`, rowID)
	if err != nil {
		return fmt.Errorf("reading ledger rows from %d: %w", rowID, err)
	}
	var ids []int64
	var amountsCents []money.Cents
	for rows.Next() {
		var id int64
		var amountCents money.Cents
		if err := rows.Scan(&id, &amountCents); err != nil {
			rows.Close()
			return fmt.Errorf("scanning ledger row: %w", err)
		}
		ids = append(ids, id)
		amountsCents = append(amountsCents, amountCents)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	totalsCents, err := storage.CascadeRunningTotals(amountsCents, newAmountCents, baseCents)
	if err != nil {
		return err
	}

	for i, id := range ids {
		amountCents := amountsCents[i]
		if i == 0 {
			amountCents = newAmountCents
		}
		if _, err := tx.Exec("UPDATE bankroll_ledger SET amount_cents = ?, running_total_cents = ? WHERE id = ?",
			amountCents, totalsCents[i], id); err != nil {
			return fmt.Errorf("cascading ledger update: %w", err)
		}
	}
	return nil
}

const lastRunningTotalQuery = "SELECT running_total_cents FROM bankroll_ledger ORDER BY id DESC LIMIT 1"

func lastRunningTotal(row *sql.Row) (money.Cents, error) {
	var totalCents money.Cents
	err := row.Scan(&totalCents)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading last runningCents totalCents: %w", err)
	}
	return totalCents, nil
}

// tournamentSnapshot holds the subset of an existing tournaments row needed
// to reconcile NetCents/Type against knockout-bounty data, which is only ever
// known from the hand history file and never from the summary file
// AppendTournament parses its other fields from. Mirrors
// sheets.tournamentRowSnapshot.
type tournamentSnapshot struct {
	date        string // RFC3339, empty if NULL (summary hasn't arrived)
	typeVal     string
	buyInCents  money.Cents
	hasNetUSD   bool // false while the row is still a hand-file-only partial row
	netCents    money.Cents
	bountyCents money.Cents
}

func (s *Store) readTournamentSnapshot(tx *sql.Tx, tournamentID string) (snap tournamentSnapshot, found bool, err error) {
	var date sql.NullString
	var buyInCents, netCents, bountyCents sql.NullInt64
	row := tx.QueryRow(`
		SELECT date, type, buy_in_cents, net_cents, bounty_cents FROM tournaments WHERE tournament_id = ?
	`, tournamentID)
	if err := row.Scan(&date, &snap.typeVal, &buyInCents, &netCents, &bountyCents); err != nil {
		if err == sql.ErrNoRows {
			return tournamentSnapshot{}, false, nil
		}
		return tournamentSnapshot{}, false, fmt.Errorf("reading tournament %s: %w", tournamentID, err)
	}
	if date.Valid {
		snap.date = date.String
	}
	if buyInCents.Valid {
		snap.buyInCents = money.Cents(buyInCents.Int64)
	}
	if netCents.Valid {
		snap.hasNetUSD = true
		snap.netCents = money.Cents(netCents.Int64)
	}
	if bountyCents.Valid {
		snap.bountyCents = money.Cents(bountyCents.Int64)
	}
	return snap, true, nil
}

// AppendTournament upserts one row in tournaments (keyed by TournamentID) and
// upserts its bankroll_ledger row. If the tournaments row already exists
// (created by UpdateTournamentHandStats), it folds in whatever bounty
// winnings that row already has recorded and carries forward the knockout
// signal into a real "<base>-ko" Type. Leaves vpip/pfr/bounty_cents untouched.
// Returns the final NetCents (baseline net plus any bounty). Mirrors
// sheets.Client.AppendTournament.
func (s *Store) AppendTournament(t parsers.Tournament) (money.Cents, error) {
	if err := storage.ValidateTournament(t); err != nil {
		return 0, err
	}
	var netCents money.Cents
	err := s.write(func(tx *sql.Tx) error {
		var err error
		netCents, err = s.appendTournament(tx, t)
		return err
	})
	if err != nil {
		return 0, err
	}
	return netCents, nil
}

func (s *Store) appendTournament(tx *sql.Tx, t parsers.Tournament) (money.Cents, error) {
	var prizePoolCents sql.NullInt64
	var prizePoolDesc sql.NullString
	if t.PrizePoolDescription != "" {
		prizePoolDesc = sql.NullString{String: t.PrizePoolDescription, Valid: true}
	} else {
		prizePoolCents = sql.NullInt64{Int64: int64(t.PrizePoolCents), Valid: true}
	}
	var multiplier sql.NullFloat64
	if t.Multiplier != nil {
		multiplier = sql.NullFloat64{Float64: *t.Multiplier, Valid: true}
	}

	snap, found, err := s.readTournamentSnapshot(tx, t.TournamentID)
	if err != nil {
		return 0, err
	}

	var bountyCents money.Cents
	typeOut := t.Type
	if found {
		bountyCents = snap.bountyCents
		typeOut = storage.ReconcileKnockoutType(t.Type, snap.typeVal)
	}
	netCents, prizeWonCents, err := storage.FoldBountyIntoNet(t.NetCents, bountyCents, t.BuyInCents)
	if err != nil {
		return 0, err
	}

	if found {
		_, err := tx.Exec(`
			UPDATE tournaments SET
				date = ?, type = ?, buy_in_cents = ?, prize_pool_cents = ?, prize_pool_description = ?,
				multiplier = ?, finish_pos = ?, total_players = ?, net_cents = ?, prize_won_cents = ?
			WHERE tournament_id = ?
		`, t.Date.UTC().Format(dateFormat), typeOut, t.BuyInCents, prizePoolCents, prizePoolDesc,
			multiplier, t.FinishPos, t.TotalPlayers, netCents, prizeWonCents, t.TournamentID)
		if err != nil {
			return 0, fmt.Errorf("updating tournament row: %w", err)
		}
	} else {
		_, err := tx.Exec(`
			INSERT INTO tournaments (date, type, tournament_id, buy_in_cents, prize_pool_cents, prize_pool_description,
				multiplier, finish_pos, total_players, net_cents, prize_won_cents)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, t.Date.UTC().Format(dateFormat), typeOut, t.TournamentID, t.BuyInCents, prizePoolCents, prizePoolDesc,
			multiplier, t.FinishPos, t.TotalPlayers, netCents, prizeWonCents)
		if err != nil {
			return 0, fmt.Errorf("inserting tournament row: %w", err)
		}
	}

	if err := s.upsertLedgerTournamentRow(tx, t.TournamentID, t.Date, netCents); err != nil {
		return 0, err
	}
	return netCents, nil
}

// UpdateTournamentHandStats writes VPIP, PFR, hand count, and knockout-bounty
// winnings for a tournament identified by tournamentID, extracted from its
// hand history file. hands is the number of hands dealt to screenName so
// far — the weight /stats uses to combine a tournament's VPIP/PFR into a
// hands-weighted average, the same role Hands plays for cash sessions. If no
// row exists yet, creates a partial one and stops there — AppendTournament
// completes it once the summary file arrives. If a row exists but the
// summary hasn't landed yet either, only type/vpip/pfr/hands/bounty are
// written. If a row exists and already has summary data, NetCents and Prize
// Won are reconciled and the tournament's bankroll_ledger row is corrected
// to match. Mirrors sheets.Client.UpdateTournamentHandStats.
func (s *Store) UpdateTournamentHandStats(tournamentID string, vpip, pfr float64, hands int, bountyCents money.Cents, isKnockout bool) error {
	if err := storage.ValidateNumbers(vpip, pfr); err != nil {
		return err
	}
	return s.write(func(tx *sql.Tx) error {
		return s.updateTournamentHandStats(tx, tournamentID, vpip, pfr, hands, bountyCents, isKnockout)
	})
}

func (s *Store) updateTournamentHandStats(tx *sql.Tx, tournamentID string, vpip, pfr float64, hands int, bountyCents money.Cents, isKnockout bool) error {
	snap, found, err := s.readTournamentSnapshot(tx, tournamentID)
	if err != nil {
		return err
	}

	if !found {
		typeCell := ""
		if isKnockout {
			typeCell = storage.KnockoutPlaceholder
		}
		_, err := tx.Exec(`
			INSERT INTO tournaments (type, tournament_id, vpip, pfr, hands, bounty_cents)
			VALUES (?, ?, ?, ?, ?, ?)
		`, typeCell, tournamentID, vpip, pfr, hands, bountyCents)
		if err != nil {
			return fmt.Errorf("creating partial tournament row for %s: %w", tournamentID, err)
		}
		return nil
	}

	typeOut := snap.typeVal
	if isKnockout {
		typeOut = storage.AppendKnockoutSuffix(snap.typeVal)
	}

	if _, err := tx.Exec(`
		UPDATE tournaments SET type = ?, vpip = ?, pfr = ?, hands = ?, bounty_cents = ? WHERE tournament_id = ?
	`, typeOut, vpip, pfr, hands, bountyCents, tournamentID); err != nil {
		return fmt.Errorf("updating hand stats for tournament %s: %w", tournamentID, err)
	}

	if !snap.hasNetUSD {
		// Summary still hasn't arrived — nothing to reconcile yet.
		return nil
	}

	netCents, prizeWonCents, err := storage.ReconcileTournamentBounty(snap.netCents, snap.bountyCents, bountyCents, snap.buyInCents)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`
		UPDATE tournaments SET net_cents = ?, prize_won_cents = ? WHERE tournament_id = ?
	`, netCents, prizeWonCents, tournamentID); err != nil {
		return fmt.Errorf("reconciling net for tournament %s: %w", tournamentID, err)
	}

	date, err := time.Parse(dateFormat, snap.date)
	if err != nil {
		return fmt.Errorf("parsing stored date for tournament %s: %w", tournamentID, err)
	}
	return s.upsertLedgerTournamentRow(tx, tournamentID, date, netCents)
}

// AppendLedgerEntry appends a deposit or withdrawal row to bankroll_ledger.
// For withdrawals, the caller passes a negative amountCents. The date is
// stored via storage.AsStoredClock — local wall-clock digits, not the true
// UTC instant — so deposit/withdrawal rows live in the same date coordinate
// system as session rows (whose dates come from PokerStars' zone-less local
// timestamps) and window queries treat both alike.
func (s *Store) AppendLedgerEntry(entryType string, amountCents money.Cents, now time.Time, operationID string) error {
	if err := storage.ValidateMoneyOperationID(operationID); err != nil {
		return err
	}
	return s.write(func(tx *sql.Tx) error { return s.appendLedgerEntry(tx, entryType, amountCents, now, operationID) })
}

func (s *Store) appendLedgerEntry(tx *sql.Tx, entryType string, amountCents money.Cents, now time.Time, operationID string) error {
	if operationID != "" {
		var exists int
		err := tx.QueryRow("SELECT 1 FROM bankroll_ledger WHERE session_id = ?", operationID).Scan(&exists)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("checking money operation identity: %w", err)
		}
	}

	prevCents, err := lastRunningTotal(tx.QueryRow(lastRunningTotalQuery))
	if err != nil {
		return err
	}
	totalCents, err := money.Add(prevCents, amountCents)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
		INSERT INTO bankroll_ledger (date, type, amount_cents, running_total_cents,session_id) VALUES (?, ?, ?, ?,?)
	`, storage.AsStoredClock(now).Format(dateFormat), entryType, amountCents, totalCents, operationID)
	if err != nil {
		return fmt.Errorf("appending ledger entry: %w", err)
	}
	return nil
}

// GetCurrentBankroll returns the running total of the last bankroll_ledger
// row. Returns 0 if the ledger is empty.
func (s *Store) GetCurrentBankroll() (money.Cents, error) {
	return lastRunningTotal(s.db.QueryRow(lastRunningTotalQuery))
}

// GetSheetURL always returns an empty string — a sqlite install has no
// Google Sheet. Callers must branch on the storage backend before displaying
// this (see CLAUDE.md's /sheet command).
func (s *Store) GetSheetURL() string {
	return ""
}

// GetLedgerEntriesSince returns all bankroll_ledger rows where Date >= t.
func (s *Store) GetLedgerEntriesSince(t time.Time) ([]storage.LedgerEntry, error) {
	rows, err := s.db.Query(`
		SELECT date, type, amount_cents, running_total_cents, session_id FROM bankroll_ledger WHERE date >= ? ORDER BY id ASC
	`, t.UTC().Format(dateFormat))
	if err != nil {
		return nil, fmt.Errorf("reading bankroll ledger: %w", err)
	}
	defer rows.Close()

	var result []storage.LedgerEntry
	for rows.Next() {
		var dateStr, entryType string
		var amountCents, runningCents money.Cents
		var sessionID sql.NullString
		if err := rows.Scan(&dateStr, &entryType, &amountCents, &runningCents, &sessionID); err != nil {
			return nil, fmt.Errorf("scanning ledger row: %w", err)
		}
		date, err := time.Parse(dateFormat, dateStr)
		if err != nil {
			return nil, fmt.Errorf("parsing ledger date %q: %w", dateStr, err)
		}
		entry := storage.LedgerEntry{
			Date:              date,
			Type:              entryType,
			AmountCents:       amountCents,
			RunningTotalCents: runningCents,
		}
		if sessionID.Valid {
			entry.SessionID = sessionID.String
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

// queryCashSessions reads every cash_sessions row with Date >= since,
// including Session ID — the one column GetCashSessionsSince's existing
// callers (stats, summaries) don't need but /export does. GetCashSessionsSince
// and GetAllCashSessions are both thin wrappers over this single query,
// differing only in the date cutoff and whether the caller wants the
// Session ID kept or discarded.
func (s *Store) queryCashSessions(since time.Time) ([]storage.CashSessionRecord, error) {
	rows, err := s.db.Query(`
		SELECT date, stake, hands, duration_min, net_cents, rake_cents, bb100, vpip, pfr, session_id
		FROM cash_sessions WHERE date >= ? ORDER BY id ASC
	`, since.UTC().Format(dateFormat))
	if err != nil {
		return nil, fmt.Errorf("reading cash sessions: %w", err)
	}
	defer rows.Close()

	var result []storage.CashSessionRecord
	for rows.Next() {
		var dateStr, stake string
		var sessionID sql.NullString // NULL on migrated pre-v1.4 sheets rows
		var hands, durationMin int
		var netCents, rakeCents money.Cents
		var bb100, vpip, pfr float64
		if err := rows.Scan(&dateStr, &stake, &hands, &durationMin, &netCents, &rakeCents, &bb100, &vpip, &pfr, &sessionID); err != nil {
			return nil, fmt.Errorf("scanning cash session row: %w", err)
		}
		date, err := time.Parse(dateFormat, dateStr)
		if err != nil {
			return nil, fmt.Errorf("parsing cash session date %q: %w", dateStr, err)
		}
		result = append(result, storage.CashSessionRecord{
			CashSession: parsers.CashSession{
				Date:        date,
				Stake:       stake,
				Hands:       hands,
				DurationMin: durationMin,
				NetCents:    netCents,
				RakeCents:   rakeCents,
				BB100:       bb100,
				VPIP:        vpip,
				PFR:         pfr,
			},
			SessionID: sessionID.String,
		})
	}
	return result, rows.Err()
}

// GetCashSessionsSince returns all cash_sessions rows where Date >= t.
// BuyInCents is not stored and will be zero in returned structs, matching the
// sheets backend.
func (s *Store) GetCashSessionsSince(t time.Time) ([]parsers.CashSession, error) {
	records, err := s.queryCashSessions(t)
	if err != nil {
		return nil, err
	}
	result := make([]parsers.CashSession, len(records))
	for i, r := range records {
		result[i] = r.CashSession
	}
	return result, nil
}

// GetAllCashSessions returns every cash_sessions row, including Session ID.
// Used only by /export.
func (s *Store) GetAllCashSessions() ([]storage.CashSessionRecord, error) {
	return s.queryCashSessions(time.Time{})
}

// queryTournaments reads every tournaments row with Date >= since, including
// VPIP, PFR, Bounty Won, and Prize Won — columns GetTournamentsSince's
// existing callers don't need but /export does. Partial rows (no Date —
// created only by a hand file arriving before the summary) are skipped.
// GetTournamentsSince and GetAllTournaments are both thin wrappers over this
// single query, differing only in the date cutoff and whether the caller
// wants those extra columns kept or discarded.
func (s *Store) queryTournaments(since time.Time) ([]storage.TournamentRecord, error) {
	return s.queryTournamentRows(`date >= ?`, since.UTC().Format(dateFormat))
}

// queryTournamentRows is the single SELECT+scan queryTournaments and
// GetTournamentByID share; where is ANDed onto the partial-row skip.
func (s *Store) queryTournamentRows(where string, args ...any) ([]storage.TournamentRecord, error) {
	rows, err := s.db.Query(`
		SELECT date, type, tournament_id, buy_in_cents, prize_pool_cents, prize_pool_description,
			multiplier, finish_pos, total_players, net_cents, vpip, pfr, bounty_cents, prize_won_cents, hands
		FROM tournaments
		WHERE date IS NOT NULL AND `+
		where+`
		ORDER BY id ASC
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading tournaments: %w", err)
	}
	defer rows.Close()

	var result []storage.TournamentRecord
	for rows.Next() {
		var dateStr, typeVal, tournamentID string
		var buyInCents, netCents money.Cents
		var prizePoolCents, bountyCents, prizeWonCents sql.NullInt64
		var vpip, pfr sql.NullFloat64
		var prizePoolDesc sql.NullString
		var multiplier sql.NullFloat64
		var hands sql.NullInt64
		var finishPos, totalPlayers int
		if err := rows.Scan(&dateStr, &typeVal, &tournamentID, &buyInCents, &prizePoolCents, &prizePoolDesc,
			&multiplier, &finishPos, &totalPlayers, &netCents, &vpip, &pfr, &bountyCents, &prizeWonCents, &hands); err != nil {
			return nil, fmt.Errorf("scanning tournament row: %w", err)
		}
		date, err := time.Parse(dateFormat, dateStr)
		if err != nil {
			return nil, fmt.Errorf("parsing tournament date %q: %w", dateStr, err)
		}
		rec := storage.TournamentRecord{
			Tournament: parsers.Tournament{
				Date:         date,
				Type:         typeVal,
				TournamentID: tournamentID,
				BuyInCents:   buyInCents,
				FinishPos:    finishPos,
				TotalPlayers: totalPlayers,
				NetCents:     netCents,
			},
		}
		if prizePoolDesc.Valid && prizePoolDesc.String != "" {
			rec.PrizePoolDescription = prizePoolDesc.String
		} else if prizePoolCents.Valid {
			rec.PrizePoolCents = money.Cents(prizePoolCents.Int64)
		}
		if multiplier.Valid {
			m := multiplier.Float64
			rec.Multiplier = &m
		}
		if vpip.Valid {
			v := vpip.Float64
			rec.VPIP = &v
		}
		if pfr.Valid {
			p := pfr.Float64
			rec.PFR = &p
		}
		if bountyCents.Valid {
			rec.BountyWonCents = money.Cents(bountyCents.Int64)
		}
		if prizeWonCents.Valid {
			rec.PrizeWonCents = money.Cents(prizeWonCents.Int64)
		}
		if hands.Valid {
			h := int(hands.Int64)
			rec.Hands = &h
		}
		result = append(result, rec)
	}
	return result, rows.Err()
}

// GetTournamentsSince returns all tournaments rows where Date >= since.
// Partial rows (no Date) are skipped.
func (s *Store) GetTournamentsSince(since time.Time) ([]parsers.Tournament, error) {
	records, err := s.queryTournaments(since)
	if err != nil {
		return nil, err
	}
	result := make([]parsers.Tournament, len(records))
	for i, r := range records {
		result[i] = r.Tournament
	}
	return result, nil
}

// GetTournamentByID returns the single completed tournament row with this
// TournamentID — the point lookup the worker's duplicate check runs per file
// event (tournament_id is UNIQUE). found is false for an absent tournament
// and for a partial row (hand file before summary — no Date), matching
// GetTournamentsSince's partial-row skip.
func (s *Store) GetTournamentByID(tournamentID string) (parsers.Tournament, bool, error) {
	records, err := s.queryTournamentRows(`tournament_id = ?`, tournamentID)
	if err != nil || len(records) == 0 {
		return parsers.Tournament{}, false, err
	}
	return records[0].Tournament, true, nil
}

// GetAllTournaments returns every tournaments row, including VPIP, PFR,
// Bounty Won, and Prize Won. Used only by /export.
func (s *Store) GetAllTournaments() ([]storage.TournamentRecord, error) {
	return s.queryTournaments(time.Time{})
}

// ExportSnapshot returns every row of all three tables as full records, for a
// backend migration. Unlike GetAllTournaments it keeps partial tournament rows
// (no date), read via allTournamentRows.
func (s *Store) ExportSnapshot() (storage.Snapshot, error) {
	cash, err := s.GetAllCashSessions()
	if err != nil {
		return storage.Snapshot{}, err
	}
	tournaments, err := s.allTournamentRows()
	if err != nil {
		return storage.Snapshot{}, err
	}
	ledger, err := s.GetLedgerEntriesSince(time.Time{})
	if err != nil {
		return storage.Snapshot{}, err
	}
	return storage.Snapshot{
		CashSessions: cash,
		Tournaments:  tournaments,
		Ledger:       ledger,
	}, nil
}

// allTournamentRows reads every tournaments row including partial ones (date
// NULL — a hand file parsed before its summary arrived). Every column is
// scanned nullably, since a partial row leaves date, buy-in, finish position,
// net, and prize won unset. queryTournaments can't be reused: it filters
// partials out and scans those columns non-nullably.
func (s *Store) allTournamentRows() ([]storage.TournamentRecord, error) {
	rows, err := s.db.Query(`
		SELECT date, type, tournament_id, buy_in_cents, prize_pool_cents, prize_pool_description,
			multiplier, finish_pos, total_players, net_cents, vpip, pfr, bounty_cents, prize_won_cents, hands
		FROM tournaments ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("reading tournaments: %w", err)
	}
	defer rows.Close()

	var result []storage.TournamentRecord
	for rows.Next() {
		var date, prizePoolDesc sql.NullString
		var typeVal, tournamentID string
		var buyInCents, prizePoolCents, netCents, bountyCents, prizeWonCents sql.NullInt64
		var multiplier, vpip, pfr sql.NullFloat64
		var finishPos, totalPlayers, hands sql.NullInt64
		if err := rows.Scan(&date, &typeVal, &tournamentID, &buyInCents, &prizePoolCents, &prizePoolDesc,
			&multiplier, &finishPos, &totalPlayers, &netCents, &vpip, &pfr, &bountyCents, &prizeWonCents, &hands); err != nil {
			return nil, fmt.Errorf("scanning tournament row: %w", err)
		}
		rec := storage.TournamentRecord{
			Tournament: parsers.Tournament{Type: typeVal, TournamentID: tournamentID},
		}
		if date.Valid {
			d, err := time.Parse(dateFormat, date.String)
			if err != nil {
				return nil, fmt.Errorf("parsing tournament date %q: %w", date.String, err)
			}
			rec.Date = d
		}
		if buyInCents.Valid {
			rec.BuyInCents = money.Cents(buyInCents.Int64)
		}
		if prizePoolDesc.Valid && prizePoolDesc.String != "" {
			rec.PrizePoolDescription = prizePoolDesc.String
		} else if prizePoolCents.Valid {
			rec.PrizePoolCents = money.Cents(prizePoolCents.Int64)
		}
		if multiplier.Valid {
			m := multiplier.Float64
			rec.Multiplier = &m
		}
		if finishPos.Valid {
			rec.FinishPos = int(finishPos.Int64)
		}
		if totalPlayers.Valid {
			rec.TotalPlayers = int(totalPlayers.Int64)
		}
		if netCents.Valid {
			rec.NetCents = money.Cents(netCents.Int64)
		}
		if vpip.Valid {
			v := vpip.Float64
			rec.VPIP = &v
		}
		if pfr.Valid {
			p := pfr.Float64
			rec.PFR = &p
		}
		if bountyCents.Valid {
			rec.BountyWonCents = money.Cents(bountyCents.Int64)
		}
		if prizeWonCents.Valid {
			rec.PrizeWonCents = money.Cents(prizeWonCents.Int64)
		}
		if hands.Valid {
			h := int(hands.Int64)
			rec.Hands = &h
		}
		result = append(result, rec)
	}
	return result, rows.Err()
}

// ImportSnapshot writes a whole snapshot into an empty database in one
// transaction, copying every value verbatim — session IDs, dates, running
// totals, and each tournament's already-bounty-folded Net and Prize Won are
// stored as-read, never recomputed, so no re-folding or ledger cascade runs.
// The caller (storage.Migrate) guarantees the database is empty first.
func (s *Store) ImportSnapshot(snap storage.Snapshot) error {
	if err := storage.ValidateSnapshot(snap); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// A blank session ID (sheets rows from before v1.4, when the column was
	// added — never backfilled) is stored as NULL, the same blank↔NULL mapping
	// the ledger loop below uses. NULLs are mutually distinct under sqlite's
	// UNIQUE, so any number of such legacy rows import cleanly, and no live
	// upsert can ever match them — live writes always carry a computed ID.
	for _, c := range snap.CashSessions {
		if _, err := tx.Exec(`
			INSERT INTO cash_sessions (date, stake, hands, duration_min, net_cents, rake_cents, bb100, vpip, pfr, session_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, c.Date.UTC().Format(dateFormat), c.Stake, c.Hands, c.DurationMin, c.NetCents, c.RakeCents,
			c.BB100, c.VPIP, c.PFR, nullString(c.SessionID)); err != nil {
			return fmt.Errorf("importing cash session %s: %w", c.SessionID, err)
		}
	}

	for _, t := range snap.Tournaments {
		if t.Date.IsZero() {
			// Partial row: only the hand-file columns are set, exactly as
			// UpdateTournamentHandStats's partial INSERT writes them, so the
			// row's Net stays NULL and can't be mistaken for summary data.
			if _, err := tx.Exec(`
				INSERT INTO tournaments (type, tournament_id, vpip, pfr, hands, bounty_cents)
				VALUES (?, ?, ?, ?, ?, ?)
			`, t.Type, t.TournamentID, nullFloat(t.VPIP), nullFloat(t.PFR), nullInt(t.Hands), t.BountyWonCents); err != nil {
				return fmt.Errorf("importing partial tournament %s: %w", t.TournamentID, err)
			}
			continue
		}
		var prizePoolCents sql.NullInt64
		var prizePoolDesc sql.NullString
		if t.PrizePoolDescription != "" {
			prizePoolDesc = sql.NullString{String: t.PrizePoolDescription, Valid: true}
		} else {
			prizePoolCents = sql.NullInt64{Int64: int64(t.PrizePoolCents), Valid: true}
		}
		if _, err := tx.Exec(`
			INSERT INTO tournaments (date, type, tournament_id, buy_in_cents, prize_pool_cents, prize_pool_description,
				multiplier, finish_pos, total_players, net_cents, vpip, pfr, bounty_cents, prize_won_cents, hands)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, t.Date.UTC().Format(dateFormat), t.Type, t.TournamentID, t.BuyInCents, prizePoolCents, prizePoolDesc,
			nullFloat(t.Multiplier), t.FinishPos, t.TotalPlayers, t.NetCents, nullFloat(t.VPIP), nullFloat(t.PFR),
			t.BountyWonCents, t.PrizeWonCents, nullInt(t.Hands)); err != nil {
			return fmt.Errorf("importing tournament %s: %w", t.TournamentID, err)
		}
	}

	for _, e := range snap.Ledger {
		if _, err := tx.Exec(`
			INSERT INTO bankroll_ledger (date, type, amount_cents, running_total_cents, session_id)
			VALUES (?, ?, ?, ?, ?)
		`, e.Date.UTC().Format(dateFormat), e.Type, e.AmountCents, e.RunningTotalCents, nullString(e.SessionID)); err != nil {
			return fmt.Errorf("importing ledger entry: %w", err)
		}
	}

	return tx.Commit()
}

// nullString maps a blank string to NULL — the blank↔NULL convention every
// session_id column uses (see ImportSnapshot's cash-session comment).
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullFloat / nullInt map an optional field to a nullable column so an unset
// one (e.g. VPIP on a summary-only tournament) is stored NULL, not 0.
func nullFloat(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

func nullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}
