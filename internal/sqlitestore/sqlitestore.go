// Package sqlitestore implements storage.Store against a local SQLite
// database file. It mirrors the Google Sheets backend's observable behavior
// method-for-method — same upsert keys, same running-total cascading, same
// two-file tournament correlation — but stores dates as RFC3339 UTC strings
// rather than the sheets backend's local "2006-01-02 15:04" format.
package sqlitestore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

const dateFormat = time.RFC3339

// knockoutPlaceholder marks the Type column of a partial tournament row
// (created by UpdateTournamentHandStats before the summary file has arrived)
// as a knockout tournament, since the base "spingo"/"mtt" classification
// isn't known yet at that point — AppendTournament combines it into the real
// "<base>-ko" Type once it does. Mirrors sheets.knockoutPlaceholder.
const knockoutPlaceholder = "ko"

const schema = `
CREATE TABLE IF NOT EXISTS cash_sessions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	date TEXT NOT NULL,
	stake TEXT NOT NULL,
	hands INTEGER NOT NULL,
	duration_min INTEGER NOT NULL,
	net_usd REAL NOT NULL,
	rake_usd REAL NOT NULL,
	bb100 REAL NOT NULL,
	vpip REAL NOT NULL,
	pfr REAL NOT NULL,
	session_id TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS tournaments (
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
	prize_won_usd REAL,
	hands INTEGER
);

CREATE TABLE IF NOT EXISTS bankroll_ledger (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	date TEXT NOT NULL,
	type TEXT NOT NULL,
	amount_usd REAL NOT NULL,
	running_total REAL NOT NULL,
	session_id TEXT
);

CREATE TABLE IF NOT EXISTS time_analysis (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	date TEXT NOT NULL,
	hour INTEGER NOT NULL,
	day_of_week TEXT NOT NULL,
	net_usd REAL NOT NULL,
	session_id TEXT NOT NULL UNIQUE
);
`

// Store persists poker session data to a local SQLite database file.
type Store struct {
	db *sql.DB
}

var _ storage.Store = (*Store)(nil)

// NewStore opens (creating if necessary) the SQLite database at dbPath and
// applies the schema if the tables don't already exist.
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
	return &Store{db: db}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// cashSessionID derives a stable identifier for a cash session from its
// first hand's timestamp and PokerStars Hand #, the same role TournamentID
// plays for tournaments. Mirrors sheets.cashSessionID; the two backends'
// values need not match each other since an install never switches backends.
func cashSessionID(date time.Time, firstHandID string) string {
	sum := sha256.Sum256([]byte(date.UTC().Format(dateFormat) + "|" + firstHandID))
	return hex.EncodeToString(sum[:])[:12]
}

// AppendCashSession upserts one row in cash_sessions, one row in
// time_analysis, and one row in bankroll_ledger — all three keyed by the same
// cashSessionID, so a cash hand history file re-parsed after every hand
// corrects its existing rows in place instead of appending a new set each
// time. Mirrors sheets.Client.AppendCashSession.
func (s *Store) AppendCashSession(session parsers.CashSession) error {
	id := cashSessionID(session.Date, session.FirstHandID)

	if err := s.upsertCashSessionsRow(id, session); err != nil {
		return err
	}
	if err := s.upsertTimeAnalysisRow(id, session); err != nil {
		return err
	}
	return s.upsertLedgerCashSessionRow(id, session.Date, session.NetUSD)
}

func (s *Store) upsertCashSessionsRow(id string, session parsers.CashSession) error {
	_, err := s.db.Exec(`
		INSERT INTO cash_sessions (date, stake, hands, duration_min, net_usd, rake_usd, bb100, vpip, pfr, session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			date = excluded.date, stake = excluded.stake, hands = excluded.hands,
			duration_min = excluded.duration_min, net_usd = excluded.net_usd,
			rake_usd = excluded.rake_usd, bb100 = excluded.bb100, vpip = excluded.vpip, pfr = excluded.pfr
	`, session.Date.UTC().Format(dateFormat), session.Stake, session.Hands, session.DurationMin,
		session.NetUSD, session.RakeUSD, session.BB100, session.VPIP, session.PFR, id)
	if err != nil {
		return fmt.Errorf("upserting cash session row: %w", err)
	}
	return nil
}

func (s *Store) upsertTimeAnalysisRow(id string, session parsers.CashSession) error {
	_, err := s.db.Exec(`
		INSERT INTO time_analysis (date, hour, day_of_week, net_usd, session_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			date = excluded.date, hour = excluded.hour, day_of_week = excluded.day_of_week, net_usd = excluded.net_usd
	`, session.Date.UTC().Format(dateFormat), session.Date.Hour(), session.Date.Weekday().String(), session.NetUSD, id)
	if err != nil {
		return fmt.Errorf("upserting time analysis row: %w", err)
	}
	return nil
}

// upsertLedgerCashSessionRow corrects the cash session's own bankroll_ledger
// row in place as its net changes hand-to-hand, rather than appending a new
// entry for every hand — the same still-in-progress-session semantics as
// upsertCashSessionsRow/upsertTimeAnalysisRow.
func (s *Store) upsertLedgerCashSessionRow(id string, date time.Time, netUSD float64) error {
	return s.upsertLedgerRow(id, date, "cash_session", netUSD)
}

// upsertLedgerTournamentRow corrects the tournament's own bankroll_ledger row
// in place, keyed by TournamentID. AppendTournament can legitimately run more
// than once for the same TournamentID (satellite/ticket flow, or a stray
// duplicate file event), so this is idempotency, not a hand-to-hand
// correction — without it the ledger would double-count a tournament whose
// tournaments row stays correctly deduplicated by tournament_id.
func (s *Store) upsertLedgerTournamentRow(tournamentID string, date time.Time, netUSD float64) error {
	return s.upsertLedgerRow(tournamentID, date, "tournament", netUSD)
}

// upsertLedgerRow finds the ledger row previously written for sessionID (the
// Session ID column — a cash session ID or a TournamentID) and corrects its
// amount and running total in place, cascading the running-total recompute to
// every later row (see cascadeRunningTotals). If no such row exists yet, it
// appends a fresh one built off the ledger's current last running total.
func (s *Store) upsertLedgerRow(sessionID string, date time.Time, entryType string, amount float64) error {
	var rowID int64
	err := s.db.QueryRow(`SELECT id FROM bankroll_ledger WHERE session_id = ?`, sessionID).Scan(&rowID)
	if err == sql.ErrNoRows {
		prev, err := s.lastRunningTotal()
		if err != nil {
			return err
		}
		_, err = s.db.Exec(`
			INSERT INTO bankroll_ledger (date, type, amount_usd, running_total, session_id)
			VALUES (?, ?, ?, ?, ?)
		`, date.UTC().Format(dateFormat), entryType, amount, prev+amount, sessionID)
		if err != nil {
			return fmt.Errorf("appending ledger row for %s: %w", sessionID, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("looking up ledger row for %s: %w", sessionID, err)
	}
	return s.cascadeLedgerUpdate(rowID, amount)
}

// cascadeLedgerUpdate sets the bankroll_ledger row identified by rowID to
// newAmount and recomputes running_total for that row and every row after it
// (by insertion order). A plain update of rowID alone is only correct when
// it's already the ledger's last row — with more than one table open at once,
// a second table's session row (or a mid-session deposit) can land after this
// row while it's still being corrected, and that later row's stored running
// total was computed from this row's old amount.
func (s *Store) cascadeLedgerUpdate(rowID int64, newAmount float64) error {
	var base float64
	err := s.db.QueryRow(`
		SELECT running_total FROM bankroll_ledger WHERE id < ? ORDER BY id DESC LIMIT 1
	`, rowID).Scan(&base)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("reading running total before row %d: %w", rowID, err)
	}

	rows, err := s.db.Query(`
		SELECT id, amount_usd FROM bankroll_ledger WHERE id >= ? ORDER BY id ASC
	`, rowID)
	if err != nil {
		return fmt.Errorf("reading ledger rows from %d: %w", rowID, err)
	}
	var ids []int64
	var amounts []float64
	for rows.Next() {
		var id int64
		var amt float64
		if err := rows.Scan(&id, &amt); err != nil {
			rows.Close()
			return fmt.Errorf("scanning ledger row: %w", err)
		}
		ids = append(ids, id)
		amounts = append(amounts, amt)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	totals := cascadeRunningTotals(amounts, newAmount, base)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for i, id := range ids {
		amt := amounts[i]
		if i == 0 {
			amt = newAmount
		}
		if _, err := tx.Exec(`UPDATE bankroll_ledger SET amount_usd = ?, running_total = ? WHERE id = ?`,
			amt, totals[i], id); err != nil {
			tx.Rollback()
			return fmt.Errorf("cascading ledger update: %w", err)
		}
	}
	return tx.Commit()
}

// cascadeRunningTotals recomputes running totals for a contiguous tail of the
// ledger (amounts, oldest first, amounts[0] being the row that changed).
// amounts[0] is treated as newAmount; every later row keeps its own amount
// but needs a recomputed running total, since a change to an earlier row
// shifts every total after it. base is the running total of the ledger row
// immediately before amounts[0] (0 if amounts[0] is the ledger's first row).
// Pure — the DB I/O wrapper is cascadeLedgerUpdate.
func cascadeRunningTotals(amounts []float64, newAmount, base float64) []float64 {
	if len(amounts) == 0 {
		return nil
	}
	totals := make([]float64, len(amounts))
	running := base
	for i, amt := range amounts {
		if i == 0 {
			amt = newAmount
		}
		running += amt
		totals[i] = running
	}
	return totals
}

func (s *Store) lastRunningTotal() (float64, error) {
	var total float64
	err := s.db.QueryRow(`SELECT running_total FROM bankroll_ledger ORDER BY id DESC LIMIT 1`).Scan(&total)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading last running total: %w", err)
	}
	return total, nil
}

// tournamentSnapshot holds the subset of an existing tournaments row needed
// to reconcile NetUSD/Type against knockout-bounty data, which is only ever
// known from the hand history file and never from the summary file
// AppendTournament parses its other fields from. Mirrors
// sheets.tournamentRowSnapshot.
type tournamentSnapshot struct {
	date      string // RFC3339, empty if NULL (summary hasn't arrived)
	typeVal   string
	buyInUSD  float64
	hasNetUSD bool // false while the row is still a hand-file-only partial row
	netUSD    float64
	bountyUSD float64
}

func (s *Store) readTournamentSnapshot(tournamentID string) (snap tournamentSnapshot, found bool, err error) {
	var date sql.NullString
	var buyIn, netUSD, bountyUSD sql.NullFloat64
	row := s.db.QueryRow(`
		SELECT date, type, buy_in_usd, net_usd, bounty_usd FROM tournaments WHERE tournament_id = ?
	`, tournamentID)
	if err := row.Scan(&date, &snap.typeVal, &buyIn, &netUSD, &bountyUSD); err != nil {
		if err == sql.ErrNoRows {
			return tournamentSnapshot{}, false, nil
		}
		return tournamentSnapshot{}, false, fmt.Errorf("reading tournament %s: %w", tournamentID, err)
	}
	if date.Valid {
		snap.date = date.String
	}
	if buyIn.Valid {
		snap.buyInUSD = buyIn.Float64
	}
	if netUSD.Valid {
		snap.hasNetUSD = true
		snap.netUSD = netUSD.Float64
	}
	if bountyUSD.Valid {
		snap.bountyUSD = bountyUSD.Float64
	}
	return snap, true, nil
}

// reconcileKnockoutType computes the Type value AppendTournament should write
// once the base type ("spingo"/"mtt") is known, combining it with whatever
// knockout signal an existing row (created by UpdateTournamentHandStats
// before the summary file arrived) already recorded — either
// knockoutPlaceholder or a Type already carrying parsers.KnockoutSuffix.
// Mirrors sheets.reconcileKnockoutType.
func reconcileKnockoutType(baseType, existingTypeCell string) string {
	if existingTypeCell == knockoutPlaceholder || strings.HasSuffix(existingTypeCell, parsers.KnockoutSuffix) {
		return baseType + parsers.KnockoutSuffix
	}
	return baseType
}

// appendKnockoutSuffix computes the Type value UpdateTournamentHandStats
// should write when a hand history file flags a tournament as knockout,
// preserving whatever base type (or lack thereof) the row's Type column
// already has. Idempotent. Mirrors sheets.appendKnockoutSuffix.
func appendKnockoutSuffix(existingTypeCell string) string {
	if existingTypeCell == knockoutPlaceholder || strings.HasSuffix(existingTypeCell, parsers.KnockoutSuffix) {
		return existingTypeCell
	}
	if existingTypeCell == "" {
		return knockoutPlaceholder
	}
	return existingTypeCell + parsers.KnockoutSuffix
}

// foldBountyIntoNet computes the final NetUSD and Prize Won once the summary
// file's baseline net (payout - buyin) is known, folding in whatever bounty
// winnings the hand history file has already recorded for this row. Mirrors
// sheets.foldBountyIntoNet.
func foldBountyIntoNet(baselineNetUSD, bountyUSD, buyInUSD float64) (netUSD, prizeWonUSD float64) {
	netUSD = baselineNetUSD + bountyUSD
	return netUSD, netUSD + buyInUSD
}

// reconcileTournamentBounty computes the corrected NetUSD and Prize Won after
// a freshly reparsed cumulative bounty total replaces whatever this row's
// previous bounty contribution to storedNetUSD was. Mirrors
// sheets.reconcileTournamentBounty.
func reconcileTournamentBounty(storedNetUSD, storedBountyUSD, newBountyUSD, buyInUSD float64) (netUSD, prizeWonUSD float64) {
	netUSD = (storedNetUSD - storedBountyUSD) + newBountyUSD
	return netUSD, netUSD + buyInUSD
}

// AppendTournament upserts one row in tournaments (keyed by TournamentID) and
// upserts its bankroll_ledger row. If the tournaments row already exists
// (created by UpdateTournamentHandStats), it folds in whatever bounty
// winnings that row already has recorded and carries forward the knockout
// signal into a real "<base>-ko" Type. Leaves vpip/pfr/bounty_usd untouched.
// Returns the final NetUSD (baseline net plus any bounty). Mirrors
// sheets.Client.AppendTournament.
func (s *Store) AppendTournament(t parsers.Tournament) (float64, error) {
	var prizePoolUSD sql.NullFloat64
	var prizePoolDesc sql.NullString
	if t.PrizePoolDescription != "" {
		prizePoolDesc = sql.NullString{String: t.PrizePoolDescription, Valid: true}
	} else {
		prizePoolUSD = sql.NullFloat64{Float64: t.PrizePoolUSD, Valid: true}
	}
	var multiplier sql.NullFloat64
	if t.Multiplier != nil {
		multiplier = sql.NullFloat64{Float64: *t.Multiplier, Valid: true}
	}

	snap, found, err := s.readTournamentSnapshot(t.TournamentID)
	if err != nil {
		return 0, err
	}

	var bounty float64
	typeOut := t.Type
	if found {
		bounty = snap.bountyUSD
		typeOut = reconcileKnockoutType(t.Type, snap.typeVal)
	}
	netUSD, prizeWon := foldBountyIntoNet(t.NetUSD, bounty, t.BuyInUSD)

	if found {
		_, err := s.db.Exec(`
			UPDATE tournaments SET
				date = ?, type = ?, buy_in_usd = ?, prize_pool_usd = ?, prize_pool_description = ?,
				multiplier = ?, finish_pos = ?, total_players = ?, net_usd = ?, prize_won_usd = ?
			WHERE tournament_id = ?
		`, t.Date.UTC().Format(dateFormat), typeOut, t.BuyInUSD, prizePoolUSD, prizePoolDesc,
			multiplier, t.FinishPos, t.TotalPlayers, netUSD, prizeWon, t.TournamentID)
		if err != nil {
			return 0, fmt.Errorf("updating tournament row: %w", err)
		}
	} else {
		_, err := s.db.Exec(`
			INSERT INTO tournaments (date, type, tournament_id, buy_in_usd, prize_pool_usd, prize_pool_description,
				multiplier, finish_pos, total_players, net_usd, prize_won_usd)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, t.Date.UTC().Format(dateFormat), typeOut, t.TournamentID, t.BuyInUSD, prizePoolUSD, prizePoolDesc,
			multiplier, t.FinishPos, t.TotalPlayers, netUSD, prizeWon)
		if err != nil {
			return 0, fmt.Errorf("inserting tournament row: %w", err)
		}
	}

	if err := s.upsertLedgerTournamentRow(t.TournamentID, t.Date, netUSD); err != nil {
		return 0, err
	}
	return netUSD, nil
}

// UpdateTournamentHandStats writes VPIP, PFR, hand count, and knockout-bounty
// winnings for a tournament identified by tournamentID, extracted from its
// hand history file. hands is the number of hands dealt to screenName so
// far — the weight /stats uses to combine a tournament's VPIP/PFR into a
// hands-weighted average, the same role Hands plays for cash sessions. If no
// row exists yet, creates a partial one and stops there — AppendTournament
// completes it once the summary file arrives. If a row exists but the
// summary hasn't landed yet either, only type/vpip/pfr/hands/bounty are
// written. If a row exists and already has summary data, NetUSD and Prize
// Won are reconciled and the tournament's bankroll_ledger row is corrected
// to match. Mirrors sheets.Client.UpdateTournamentHandStats.
func (s *Store) UpdateTournamentHandStats(tournamentID string, vpip, pfr float64, hands int, bountyUSD float64, isKnockout bool) error {
	snap, found, err := s.readTournamentSnapshot(tournamentID)
	if err != nil {
		return err
	}

	if !found {
		typeCell := ""
		if isKnockout {
			typeCell = knockoutPlaceholder
		}
		_, err := s.db.Exec(`
			INSERT INTO tournaments (type, tournament_id, vpip, pfr, hands, bounty_usd)
			VALUES (?, ?, ?, ?, ?, ?)
		`, typeCell, tournamentID, vpip, pfr, hands, bountyUSD)
		if err != nil {
			return fmt.Errorf("creating partial tournament row for %s: %w", tournamentID, err)
		}
		return nil
	}

	typeOut := snap.typeVal
	if isKnockout {
		typeOut = appendKnockoutSuffix(snap.typeVal)
	}

	if _, err := s.db.Exec(`
		UPDATE tournaments SET type = ?, vpip = ?, pfr = ?, hands = ?, bounty_usd = ? WHERE tournament_id = ?
	`, typeOut, vpip, pfr, hands, bountyUSD, tournamentID); err != nil {
		return fmt.Errorf("updating hand stats for tournament %s: %w", tournamentID, err)
	}

	if !snap.hasNetUSD {
		// Summary still hasn't arrived — nothing to reconcile yet.
		return nil
	}

	netUSD, prizeWon := reconcileTournamentBounty(snap.netUSD, snap.bountyUSD, bountyUSD, snap.buyInUSD)

	if _, err := s.db.Exec(`
		UPDATE tournaments SET net_usd = ?, prize_won_usd = ? WHERE tournament_id = ?
	`, netUSD, prizeWon, tournamentID); err != nil {
		return fmt.Errorf("reconciling net for tournament %s: %w", tournamentID, err)
	}

	date, err := time.Parse(dateFormat, snap.date)
	if err != nil {
		return fmt.Errorf("parsing stored date for tournament %s: %w", tournamentID, err)
	}
	return s.upsertLedgerTournamentRow(tournamentID, date, netUSD)
}

// AppendLedgerEntry appends a deposit or withdrawal row to bankroll_ledger.
// For withdrawals, the caller passes a negative amountUSD.
func (s *Store) AppendLedgerEntry(entryType string, amountUSD float64) error {
	prev, err := s.lastRunningTotal()
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO bankroll_ledger (date, type, amount_usd, running_total) VALUES (?, ?, ?, ?)
	`, time.Now().UTC().Format(dateFormat), entryType, amountUSD, prev+amountUSD)
	if err != nil {
		return fmt.Errorf("appending ledger entry: %w", err)
	}
	return nil
}

// GetCurrentBankroll returns the running total of the last bankroll_ledger
// row. Returns 0 if the ledger is empty.
func (s *Store) GetCurrentBankroll() (float64, error) {
	return s.lastRunningTotal()
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
		SELECT date, type, amount_usd, running_total, session_id FROM bankroll_ledger WHERE date >= ? ORDER BY id ASC
	`, t.UTC().Format(dateFormat))
	if err != nil {
		return nil, fmt.Errorf("reading bankroll ledger: %w", err)
	}
	defer rows.Close()

	var result []storage.LedgerEntry
	for rows.Next() {
		var dateStr, entryType string
		var amount, running float64
		var sessionID sql.NullString
		if err := rows.Scan(&dateStr, &entryType, &amount, &running, &sessionID); err != nil {
			return nil, fmt.Errorf("scanning ledger row: %w", err)
		}
		date, err := time.Parse(dateFormat, dateStr)
		if err != nil {
			return nil, fmt.Errorf("parsing ledger date %q: %w", dateStr, err)
		}
		entry := storage.LedgerEntry{
			Date:         date,
			Type:         entryType,
			AmountUSD:    amount,
			RunningTotal: running,
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
		SELECT date, stake, hands, duration_min, net_usd, rake_usd, bb100, vpip, pfr, session_id
		FROM cash_sessions WHERE date >= ? ORDER BY id ASC
	`, since.UTC().Format(dateFormat))
	if err != nil {
		return nil, fmt.Errorf("reading cash sessions: %w", err)
	}
	defer rows.Close()

	var result []storage.CashSessionRecord
	for rows.Next() {
		var dateStr, stake, sessionID string
		var hands, durationMin int
		var netUSD, rakeUSD, bb100, vpip, pfr float64
		if err := rows.Scan(&dateStr, &stake, &hands, &durationMin, &netUSD, &rakeUSD, &bb100, &vpip, &pfr, &sessionID); err != nil {
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
				NetUSD:      netUSD,
				RakeUSD:     rakeUSD,
				BB100:       bb100,
				VPIP:        vpip,
				PFR:         pfr,
			},
			SessionID: sessionID,
		})
	}
	return result, rows.Err()
}

// GetCashSessionsSince returns all cash_sessions rows where Date >= t.
// BuyInUSD is not stored and will be zero in returned structs, matching the
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
	rows, err := s.db.Query(`
		SELECT date, type, tournament_id, buy_in_usd, prize_pool_usd, prize_pool_description,
			multiplier, finish_pos, total_players, net_usd, vpip, pfr, bounty_usd, prize_won_usd, hands
		FROM tournaments
		WHERE date IS NOT NULL AND date >= ?
		ORDER BY id ASC
	`, since.UTC().Format(dateFormat))
	if err != nil {
		return nil, fmt.Errorf("reading tournaments: %w", err)
	}
	defer rows.Close()

	var result []storage.TournamentRecord
	for rows.Next() {
		var dateStr, typeVal, tournamentID string
		var buyIn, netUSD float64
		var prizePoolUSD, vpip, pfr, bountyUSD, prizeWonUSD sql.NullFloat64
		var prizePoolDesc sql.NullString
		var multiplier sql.NullFloat64
		var hands sql.NullInt64
		var finishPos, totalPlayers int
		if err := rows.Scan(&dateStr, &typeVal, &tournamentID, &buyIn, &prizePoolUSD, &prizePoolDesc,
			&multiplier, &finishPos, &totalPlayers, &netUSD, &vpip, &pfr, &bountyUSD, &prizeWonUSD, &hands); err != nil {
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
				BuyInUSD:     buyIn,
				FinishPos:    finishPos,
				TotalPlayers: totalPlayers,
				NetUSD:       netUSD,
			},
		}
		if prizePoolDesc.Valid && prizePoolDesc.String != "" {
			rec.PrizePoolDescription = prizePoolDesc.String
		} else if prizePoolUSD.Valid {
			rec.PrizePoolUSD = prizePoolUSD.Float64
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
		if bountyUSD.Valid {
			rec.BountyWonUSD = bountyUSD.Float64
		}
		if prizeWonUSD.Valid {
			rec.PrizeWonUSD = prizeWonUSD.Float64
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

// GetAllTournaments returns every tournaments row, including VPIP, PFR,
// Bounty Won, and Prize Won. Used only by /export.
func (s *Store) GetAllTournaments() ([]storage.TournamentRecord, error) {
	return s.queryTournaments(time.Time{})
}

// GetAllTimeAnalysis returns every time_analysis row. Used only by /export.
func (s *Store) GetAllTimeAnalysis() ([]storage.TimeAnalysisRecord, error) {
	rows, err := s.db.Query(`
		SELECT date, hour, day_of_week, net_usd, session_id FROM time_analysis ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("reading time analysis: %w", err)
	}
	defer rows.Close()

	var result []storage.TimeAnalysisRecord
	for rows.Next() {
		var dateStr, dayOfWeek, sessionID string
		var hour int
		var netUSD float64
		if err := rows.Scan(&dateStr, &hour, &dayOfWeek, &netUSD, &sessionID); err != nil {
			return nil, fmt.Errorf("scanning time analysis row: %w", err)
		}
		date, err := time.Parse(dateFormat, dateStr)
		if err != nil {
			return nil, fmt.Errorf("parsing time analysis date %q: %w", dateStr, err)
		}
		result = append(result, storage.TimeAnalysisRecord{
			Date:      date,
			Hour:      hour,
			DayOfWeek: dayOfWeek,
			NetUSD:    netUSD,
			SessionID: sessionID,
		})
	}
	return result, rows.Err()
}
