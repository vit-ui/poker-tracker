// Package sheets manages the Google Sheets integration: tab setup, row appends,
// and reads used by the bot and hardstop evaluators.
package sheets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

const dateFormat = "2006-01-02 15:04"

// sheetsEpoch is Google Sheets' serial-date origin: serial 0 is 1899-12-30,
// and the fractional part is the fraction of a day elapsed. See parseSheetDate.
var sheetsEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

// parseSheetDate reads a Date column cell that may arrive in either of two
// serializations. The tracker always writes dates as plain "2006-01-02 15:04"
// text, but if the player (or a spreadsheet template) formats that column as a
// Date/Time in Sheets, the API returns the cell as a serial-number float64
// under UNFORMATTED_VALUE instead of the string — the same instant, a
// different wire type. fmt.Sprint on that float then yields "46218.613..."
// which time.Parse can't read, crashing every read path (/status, /stats,
// migration). Handling both here, in the one helper every date read funnels
// through, keeps a date-formatted column from breaking the tracker no matter
// which column the player formats.
//
// Stored dates carry minute precision (the write format has no seconds), so
// the serial is rounded to the nearest minute — reproducing the exact
// wall-clock the string form would, with no float dust in the seconds place.
func parseSheetDate(v interface{}) (time.Time, error) {
	if serial, ok := v.(float64); ok {
		minutes := math.Round(serial * 24 * 60)
		return sheetsEpoch.Add(time.Duration(minutes) * time.Minute), nil
	}
	return time.Parse(dateFormat, fmt.Sprint(v))
}

// Client holds the authenticated Sheets service and spreadsheet ID. It also
// owns the write-ahead fallback (see fallback.go concepts inlined below): when
// a mutating write fails against the Sheets API — the transient 500/503s
// Google occasionally returns — the write's replayable state is cached to
// fallbackPath keyed by session/tournament ID, then flushed back to the sheet
// the moment the sheet is proven reachable again (on startup and after any
// successful write). fallbackPath == "" disables it entirely, used for the
// short-lived validation clients bot.go constructs during setup/sheet changes,
// which never persist a session and so must never touch the fallback file.
type Client struct {
	svc           *restService
	spreadsheetID string

	fallbackPath string // "" disables the write-ahead fallback
	fallbackMu   sync.Mutex
	fallback     map[string]fallbackEntry
}

var _ storage.Store = (*Client)(nil)

// NewClient authenticates with the service account JSON and connects to the
// spreadsheet identified by spreadsheetID. Calls ensureTabs on startup.
// fallbackPath, if non-empty, is where the write-ahead fallback file lives;
// its queued entries (left by a previous crashed or offline run) are loaded and
// flushed once here, since ensureTabs succeeding just proved the sheet is
// reachable. Pass "" for a throwaway validation client that must not own the
// fallback.
func NewClient(serviceAccountPath, spreadsheetID, fallbackPath string) (*Client, error) {
	data, err := os.ReadFile(serviceAccountPath)
	if err != nil {
		return nil, fmt.Errorf("reading service account: %w", err)
	}

	svc, err := newRESTService(data, spreadsheetsScope)
	if err != nil {
		return nil, err
	}

	c := &Client{
		svc:           svc,
		spreadsheetID: spreadsheetID,
		fallbackPath:  fallbackPath,
		fallback:      make(map[string]fallbackEntry),
	}

	if err := c.ensureTabs(); err != nil {
		return nil, err
	}

	if fallbackPath != "" {
		// A corrupt fallback file must not brick startup — the whole tracker
		// (sheet writes, bot, hard stop) would be dead over one unreplayable
		// recovery file. Log and continue empty; the next failed write
		// overwrites it with valid JSON.
		if err := c.loadFallback(); err != nil {
			slog.Error("loading sheets fallback, starting empty", "err", err)
			c.fallback = make(map[string]fallbackEntry)
		}
		c.flush()
	}

	return c, nil
}

// ResolveSheetID searches Google Drive for a spreadsheet with the given name
// and returns its ID. Returns an error if the spreadsheet is not found or if
// more than one match is found.
func ResolveSheetID(serviceAccountPath, sheetName string) (string, error) {
	data, err := os.ReadFile(serviceAccountPath)
	if err != nil {
		return "", fmt.Errorf("reading service account: %w", err)
	}

	driveSvc, err := newRESTService(data, driveMetadataReadonlyScope)
	if err != nil {
		return "", err
	}

	ids, err := driveSvc.driveFindSpreadsheets(sheetName)
	if err != nil {
		return "", fmt.Errorf("searching for spreadsheet: %w", err)
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("spreadsheet %q not found", sheetName)
	}
	if len(ids) > 1 {
		return "", fmt.Errorf("spreadsheet name %q is ambiguous: %d matches found", sheetName, len(ids))
	}

	return ids[0], nil
}

func (c *Client) ensureTabs() error {
	titles, err := c.svc.spreadsheetTabTitles(c.spreadsheetID)
	if err != nil {
		return fmt.Errorf("getting spreadsheet: %w", err)
	}

	existing := make(map[string]bool, len(titles))
	for _, title := range titles {
		existing[title] = true
	}

	type tabDef struct {
		name    string
		headers []interface{}
	}

	tabs := []tabDef{
		{"Cash Sessions", []interface{}{"Date", "Stake", "Hands", "Duration (min)", "Net (USD)", "Rake (USD)", "BB/100", "VPIP", "PFR", "Session ID"}},
		{"Tournaments", []interface{}{"Date", "Type", "Tournament ID", "Buy-In (USD)", "Prize Pool (USD)", "Multiplier", "Finish Position", "Total Players", "Net (USD)", "VPIP", "PFR", "Bounty Won (USD)", "Prize Won (USD)", "Hands"}},
		{"Bankroll Ledger", []interface{}{"Date", "Type", "Amount (USD)", "Running Total (USD)", "Session ID"}},
		{"Time Analysis", []interface{}{"Date", "Hour", "Day of Week", "Net (USD)", "Session ID"}},
	}

	for _, tab := range tabs {
		if !existing[tab.name] {
			if err := c.svc.addSheet(c.spreadsheetID, tab.name); err != nil {
				return fmt.Errorf("creating tab %q: %w", tab.name, err)
			}

			if err := c.appendRow(tab.name, tab.headers); err != nil {
				return fmt.Errorf("writing headers for tab %q: %w", tab.name, err)
			}
			continue
		}

		// Tab already exists on this spreadsheet — an older release may
		// have shipped fewer columns than tab.headers currently has (e.g.
		// Tournaments' Bounty Won/Prize Won, added after installs already
		// had the tab). Without backfilling, a newly added trailing column
		// would silently collect real data under a blank header forever,
		// since this loop only ever writes headers when creating a brand
		// new tab.
		if err := c.backfillTabHeaders(tab.name, tab.headers); err != nil {
			return err
		}
	}

	return nil
}

// backfillTabHeaders writes whichever of want's header cells are missing
// from tab's row 1 — the columns beyond however many the sheet's row 1
// currently has — without touching any cell that already has a value, in
// case the player has edited an existing header's text. A no-op if row 1
// already has at least len(want) cells.
func (c *Client) backfillTabHeaders(tab string, want []interface{}) error {
	resp, err := c.svc.valuesGet(c.spreadsheetID, fmt.Sprintf("%s!1:1", tab))
	if err != nil {
		return fmt.Errorf("reading %s header row: %w", tab, err)
	}
	have := 0
	if len(resp.Values) > 0 {
		have = len(resp.Values[0])
	}
	if have >= len(want) {
		return nil
	}

	rangeStr := fmt.Sprintf("%s!%s1:%s1", tab, columnLetter(have), columnLetter(len(want)-1))
	if err := c.svc.valuesUpdate(c.spreadsheetID, rangeStr, [][]interface{}{want[have:]}); err != nil {
		return fmt.Errorf("backfilling %s headers: %w", tab, err)
	}
	return nil
}

// columnLetter converts a 0-based column index to its spreadsheet column
// letter: 0 -> "A", 25 -> "Z", 26 -> "AA", and so on.
func columnLetter(idx int) string {
	letters := ""
	for idx >= 0 {
		letters = string(rune('A'+idx%26)) + letters
		idx = idx/26 - 1
	}
	return letters
}

func (c *Client) appendRow(tab string, values []interface{}) error {
	if err := c.svc.valuesAppend(c.spreadsheetID, tab+"!A1", [][]interface{}{values}); err != nil {
		return fmt.Errorf("appending to %q: %w", tab, err)
	}
	return nil
}

// getLastRunningTotal reads the last value from the Running Total column of
// Bankroll Ledger. Returns 0 when the ledger has no data rows.
func (c *Client) getLastRunningTotal() (float64, error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, "Bankroll Ledger!D:D")
	if err != nil {
		return 0, fmt.Errorf("reading bankroll ledger: %w", err)
	}
	// resp.Values[0] is the header row; data starts at [1]
	if len(resp.Values) <= 1 {
		return 0, nil
	}
	last := resp.Values[len(resp.Values)-1]
	if len(last) == 0 {
		return 0, nil
	}
	return parseFloat(last[0]), nil
}

// cashSessionID derives a stable identifier for a cash session from its
// first hand's timestamp and PokerStars Hand ID. Both are fixed the moment
// the first hand in the file is parsed and never change as PokerStars keeps
// appending later hands to the same file, so re-parsing the growing file on
// every hand always yields the same ID — the same role TournamentID plays
// for tournaments, but cash hand histories carry no single-field session
// identifier of their own to key off of directly.
func cashSessionID(date time.Time, firstHandID string) string {
	sum := sha256.Sum256([]byte(date.Format(dateFormat) + "|" + firstHandID))
	return hex.EncodeToString(sum[:])[:12]
}

// findRowByColumnValue searches column columnLetter of tab for a row whose
// value equals want, starting after the header row. Returns the 1-based
// sheet row index, or found=false if not present. Shared by every tab that
// upserts on a stored ID column (Cash Sessions, Time Analysis, Bankroll
// Ledger's cash_session rows, and — via findTournamentRow — Tournaments).
func (c *Client) findRowByColumnValue(tab, columnLetter, want string) (rowIndex int, found bool, err error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, fmt.Sprintf("%s!%s:%s", tab, columnLetter, columnLetter))
	if err != nil {
		return 0, false, fmt.Errorf("reading %s column %s: %w", tab, columnLetter, err)
	}
	for i, row := range resp.Values {
		if i == 0 {
			continue // header row
		}
		if len(row) > 0 && fmt.Sprint(row[0]) == want {
			return i + 1, true, nil
		}
	}
	return 0, false, nil
}

// AppendCashSession upserts one row in Cash Sessions, one row in Time
// Analysis, and one row in Bankroll Ledger — all three keyed by the same
// cashSessionID, so a cash hand history file that's re-parsed after every
// hand corrects its existing rows in place instead of appending a new set
// each time. Mirrors AppendTournament's find-or-append pattern, applied to
// all three tabs a cash session touches rather than just one.
func (c *Client) rawAppendCashSession(session parsers.CashSession) error {
	id := cashSessionID(session.Date, session.FirstHandID)

	if err := c.upsertCashSessionsRow(id, session); err != nil {
		return err
	}
	if err := c.upsertTimeAnalysisRow(id, session); err != nil {
		return err
	}
	return c.upsertLedgerCashSessionRow(id, session.Date, session.NetUSD)
}

func (c *Client) upsertCashSessionsRow(id string, session parsers.CashSession) error {
	values := []interface{}{
		session.Date.Format(dateFormat),
		session.Stake,
		session.Hands,
		session.DurationMin,
		session.NetUSD,
		session.RakeUSD,
		session.BB100,
		session.VPIP,
		session.PFR,
		id,
	}
	rowIndex, found, err := c.findRowByColumnValue("Cash Sessions", "J", id)
	if err != nil {
		return err
	}
	if !found {
		return c.appendRow("Cash Sessions", values)
	}
	rangeStr := fmt.Sprintf("Cash Sessions!A%d:J%d", rowIndex, rowIndex)
	if err := c.svc.valuesUpdate(c.spreadsheetID, rangeStr, [][]interface{}{values}); err != nil {
		return fmt.Errorf("updating cash session row: %w", err)
	}
	return nil
}

func (c *Client) upsertTimeAnalysisRow(id string, session parsers.CashSession) error {
	values := []interface{}{
		session.Date.Format(dateFormat),
		session.Date.Hour(),
		session.Date.Weekday().String(),
		session.NetUSD,
		id,
	}
	rowIndex, found, err := c.findRowByColumnValue("Time Analysis", "E", id)
	if err != nil {
		return err
	}
	if !found {
		return c.appendRow("Time Analysis", values)
	}
	rangeStr := fmt.Sprintf("Time Analysis!A%d:E%d", rowIndex, rowIndex)
	if err := c.svc.valuesUpdate(c.spreadsheetID, rangeStr, [][]interface{}{values}); err != nil {
		return fmt.Errorf("updating time analysis row: %w", err)
	}
	return nil
}

// upsertLedgerCashSessionRow corrects the cash session's own Bankroll Ledger
// row in place as its net changes hand-to-hand, rather than appending a new
// transfer for every hand. This isn't a retroactive correction of settled
// history — deposits, withdrawals, and tournament entries stay pure
// appends, never touched again — it's the still-in-progress session's one
// ledger entry catching up to the session's current state, the same way its
// Cash Sessions and Time Analysis rows do.
func (c *Client) upsertLedgerCashSessionRow(id string, date time.Time, netUSD float64) error {
	rowIndex, found, err := c.findRowByColumnValue("Bankroll Ledger", "E", id)
	if err != nil {
		return err
	}
	if !found {
		prev, err := c.getLastRunningTotal()
		if err != nil {
			return err
		}
		return c.appendRow("Bankroll Ledger", []interface{}{
			date.Format(dateFormat),
			"cash_session",
			netUSD,
			prev + netUSD,
			id,
		})
	}

	return c.updateLedgerAmountCascading(rowIndex, netUSD)
}

// updateLedgerAmountCascading sets rowIndex's Amount to newAmount and
// recomputes Running Total for rowIndex and every row after it through the
// end of the ledger. A plain update of rowIndex alone is only correct when
// it's the sheet's last data row — with multiple tables open at once, a
// second table's session row (or a mid-session /deposit) can land after the
// first table's row while that row is still being corrected hand-to-hand;
// those later rows' stored Running Total was computed from this row's old
// Amount and goes stale the moment this row's Amount changes.
func (c *Client) updateLedgerAmountCascading(rowIndex int, newAmount float64) error {
	resp, err := c.svc.valuesGet(c.spreadsheetID, "Bankroll Ledger!C:D")
	if err != nil {
		return fmt.Errorf("reading bankroll ledger for cascade update: %w", err)
	}

	base := runningTotalAbove(resp.Values, rowIndex)

	amounts := make([]float64, len(resp.Values))
	for i, row := range resp.Values {
		if len(row) > 0 {
			amounts[i] = parseFloat(row[0])
		}
	}

	updates := cascadeLedgerAmounts(amounts, rowIndex-1, newAmount, base)
	if len(updates) == 0 {
		return nil
	}

	rangeStr := fmt.Sprintf("Bankroll Ledger!C%d:D%d", rowIndex, rowIndex+len(updates)-1)
	if err := c.svc.valuesUpdate(c.spreadsheetID, rangeStr, updates); err != nil {
		return fmt.Errorf("cascading bankroll ledger update: %w", err)
	}
	return nil
}

// runningTotalAbove returns the Bankroll Ledger running total of the row
// immediately above rowIndex (1-based sheet row, header at row 1) — 0 if
// rowIndex is the first data row — read from the C:D values the cascade
// update already fetched, so no separate single-cell API read is needed.
// Starting the cascade from the row above (rather than getLastRunningTotal,
// the sheet's last row) matters because that last row could be this very
// row, whose own stale amount would then be double-counted. values holds the
// header at index 0, so sheet row r sits at values[r-1] and its Running
// Total (column D) at [1].
func runningTotalAbove(values [][]interface{}, rowIndex int) float64 {
	if rowIndex <= 2 || rowIndex-2 >= len(values) {
		return 0
	}
	row := values[rowIndex-2]
	if len(row) < 2 {
		return 0
	}
	return parseFloat(row[1])
}

// cascadeLedgerAmounts computes updated (Amount, Running Total) pairs for
// the ledger's Amount column from changedIdx (0-based, data rows only, no
// header) through the last row — the running-total math itself is
// storage.CascadeRunningTotals, shared with the sqlite backend; this only
// bounds-checks changedIdx and zips the results into the two-column cell
// shape the Sheets update range wants. Pure — the Sheets I/O wrapper is
// updateLedgerAmountCascading.
func cascadeLedgerAmounts(amounts []float64, changedIdx int, newAmount, base float64) [][]interface{} {
	if changedIdx < 0 || changedIdx >= len(amounts) {
		return nil
	}
	tail := amounts[changedIdx:]
	totals := storage.CascadeRunningTotals(tail, newAmount, base)
	updates := make([][]interface{}, len(tail))
	for i := range tail {
		amount := tail[i]
		if i == 0 {
			amount = newAmount
		}
		updates[i] = []interface{}{amount, totals[i]}
	}
	return updates
}

// findTournamentRow searches column C (Tournament ID) for the given ID.
// Returns the 1-based sheet row index, or found=false if not present.
func (c *Client) findTournamentRow(tournamentID string) (rowIndex int, found bool, err error) {
	return c.findRowByColumnValue("Tournaments", "C", tournamentID)
}

// tournamentRowSnapshot holds the subset of an existing Tournaments row
// needed to reconcile NetUSD/Type against knockout-bounty data, which is
// only ever known from the hand history file (UpdateTournamentHandStats) and
// never from the summary file AppendTournament parses its other fields from.
type tournamentRowSnapshot struct {
	date      time.Time
	typeVal   string
	buyInUSD  float64
	hasNetUSD bool // false while the row is still a hand-file-only partial row
	netUSD    float64
	bountyUSD float64
}

// readTournamentRow reads the fields of an existing Tournaments row (by
// 1-based sheet rowIndex) needed to compute a bounty-reconciled NetUSD/Type
// from either side of the two-file correlation.
func (c *Client) readTournamentRow(rowIndex int) (tournamentRowSnapshot, error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, fmt.Sprintf("Tournaments!A%d:M%d", rowIndex, rowIndex))
	if err != nil {
		return tournamentRowSnapshot{}, fmt.Errorf("reading tournament row %d: %w", rowIndex, err)
	}
	if len(resp.Values) == 0 {
		return tournamentRowSnapshot{}, nil
	}
	row := resp.Values[0]
	var snap tournamentRowSnapshot
	if len(row) > 0 {
		if date, err := parseSheetDate(row[0]); err == nil {
			snap.date = date
		}
	}
	if len(row) > 1 {
		snap.typeVal = fmt.Sprint(row[1])
	}
	if len(row) > 3 {
		snap.buyInUSD = parseFloat(row[3])
	}
	if len(row) > 8 && fmt.Sprint(row[8]) != "" {
		snap.hasNetUSD = true
		snap.netUSD = parseFloat(row[8])
	}
	if len(row) > 11 {
		snap.bountyUSD = parseFloat(row[11])
	}
	return snap, nil
}

// AppendTournament upserts one row in Tournaments (keyed by TournamentID) and
// upserts its Bankroll Ledger row (see upsertLedgerTournamentRow). If the
// Tournaments row already exists (created by UpdateTournamentHandStats),
// updates columns A–I, folding in whatever bounty winnings that row already
// has recorded (column L, Bounty Won) — the summary file this data comes
// from has no concept of bounties at all, only the hand history file's
// elimination lines do (why two files: plan §Why Tournaments Need Two
// Files) — and carries forward the storage.KnockoutPlaceholder into a real
// "<base>-ko" Type if the row was flagged knockout. Writes Prize Won
// (column M, = NetUSD + BuyInUSD) alongside. Leaves VPIP/PFR/Bounty Won
// (J:L) untouched. Returns the final NetUSD (baseline net plus any bounty)
// so callers can report or evaluate against the real total rather than the
// pre-bounty figure.
func (c *Client) rawAppendTournament(t parsers.Tournament) (float64, error) {
	var multiplier interface{}
	if t.Multiplier != nil {
		multiplier = *t.Multiplier
	} else {
		multiplier = ""
	}

	// A ticket-denominated prize pool (e.g. "$1.00 Power Path Step 2
	// Ticket") is never a real dollar amount — writing the raw description
	// text here instead of PrizePoolUSD (always 0 in that case) keeps the
	// cell from reading as a misleading cash figure.
	var prizePool interface{} = t.PrizePoolUSD
	if t.PrizePoolDescription != "" {
		prizePool = t.PrizePoolDescription
	}

	rowIndex, found, err := c.findTournamentRow(t.TournamentID)
	if err != nil {
		return 0, err
	}

	var bounty float64
	typeOut := t.Type
	if found {
		snap, err := c.readTournamentRow(rowIndex)
		if err != nil {
			return 0, err
		}
		bounty = snap.bountyUSD
		typeOut = storage.ReconcileKnockoutType(t.Type, snap.typeVal)
	}
	netUSD, prizeWon := storage.FoldBountyIntoNet(t.NetUSD, bounty, t.BuyInUSD)

	if found {
		// Row was pre-created by UpdateTournamentHandStats — update the main
		// columns (A:I) and Prize Won (M) in one batched call, leaving
		// VPIP/PFR/Bounty Won (J:L) and Hands (N) untouched. One call
		// instead of two also means no window where Net is written but
		// Prize Won isn't yet.
		if err := c.svc.valuesBatchUpdate(c.spreadsheetID, []valueRange{
			{
				Range: fmt.Sprintf("Tournaments!A%d:I%d", rowIndex, rowIndex),
				Values: [][]interface{}{{
					t.Date.Format(dateFormat),
					typeOut,
					t.TournamentID,
					t.BuyInUSD,
					prizePool,
					multiplier,
					t.FinishPos,
					t.TotalPlayers,
					netUSD,
				}},
			},
			{
				Range:  fmt.Sprintf("Tournaments!M%d", rowIndex),
				Values: [][]interface{}{{prizeWon}},
			},
		}); err != nil {
			return 0, fmt.Errorf("updating tournament %s: %w", t.TournamentID, err)
		}
	} else {
		if err := c.appendRow("Tournaments", []interface{}{
			t.Date.Format(dateFormat),
			typeOut,
			t.TournamentID,
			t.BuyInUSD,
			prizePool,
			multiplier,
			t.FinishPos,
			t.TotalPlayers,
			netUSD,
			"", // VPIP — filled later by UpdateTournamentHandStats
			"", // PFR  — filled later by UpdateTournamentHandStats
			"", // Bounty Won (USD) — filled later by UpdateTournamentHandStats
			prizeWon,
			"", // Hands — filled later by UpdateTournamentHandStats
		}); err != nil {
			return 0, err
		}
	}

	if err := c.upsertLedgerTournamentRow(t.TournamentID, t.Date, netUSD); err != nil {
		return 0, err
	}
	return netUSD, nil
}

// upsertLedgerTournamentRow corrects the tournament's own Bankroll Ledger row
// in place, keyed by TournamentID in the Session ID column, rather than
// appending a new one on every call. AppendTournament can legitimately run
// more than once for the same TournamentID — the satellite/ticket flow calls
// it once a queued tournament's cash-vs-ticket answer arrives, and a stray
// duplicate file event is otherwise indistinguishable from a real one to the
// watcher — and without this, the Tournaments tab stays deduplicated (see
// findTournamentRow) while the ledger silently double-counts the same
// tournament's net result. Mirrors upsertLedgerCashSessionRow.
func (c *Client) upsertLedgerTournamentRow(tournamentID string, date time.Time, netUSD float64) error {
	rowIndex, found, err := c.findRowByColumnValue("Bankroll Ledger", "E", tournamentID)
	if err != nil {
		return err
	}
	if !found {
		prev, err := c.getLastRunningTotal()
		if err != nil {
			return err
		}
		return c.appendRow("Bankroll Ledger", []interface{}{
			date.Format(dateFormat),
			"tournament",
			netUSD,
			prev + netUSD,
			tournamentID,
		})
	}

	return c.updateLedgerAmountCascading(rowIndex, netUSD)
}

// UpdateTournamentHandStats writes VPIP, PFR, and knockout-bounty winnings
// for a tournament identified by tournamentID, extracted from its hand
// history file (see parsers.TournamentHandStats) — the summary file the rest
// of the row's fields come from has no concept of bounties at all, only the
// hand history file's per-elimination lines and per-seat bounty markers do
// (why two files: plan §Why Tournaments Need Two Files).
//
// If no row exists yet, creates a partial one exactly as before, extended
// with Bounty Won and (if isKnockout) storage.KnockoutPlaceholder in place of Type,
// and stops there — AppendTournament will complete it once the summary file
// arrives, combining the placeholder into a real "<base>-ko" Type and
// folding in this bounty (see its doc comment). If a row exists but summary
// data (NetUSD, column I) hasn't arrived yet either, only J:L, N (and
// possibly B, for the same storage.KnockoutPlaceholder reason) are written and
// NetUSD/Prize Won/the ledger are left alone. If a row exists and already
// has summary data — the common case, since the hand history file grows and
// is reparsed (recomputing this tournament's cumulative bounty from scratch)
// throughout the tournament while the summary only ever arrives once at the
// end — NetUSD is reconciled as (storedNetUSD - storedBounty) + bountyUSD,
// undoing this row's previous bounty contribution and applying the freshly
// reparsed total; Prize Won (column M) and the tournament's Bankroll Ledger
// row are corrected to match via upsertLedgerTournamentRow. If isKnockout
// and the row's Type doesn't carry KnockoutSuffix yet, it's appended.
//
// hands is the number of hands dealt to screenName in this tournament so
// far (column N, added alongside VPIP/PFR) — the weight /stats uses to
// combine a tournament's VPIP/PFR into a hands-weighted average, the same
// role Hands plays for cash sessions.
func (c *Client) rawUpdateTournamentHandStats(tournamentID string, vpip, pfr float64, hands int, bountyUSD float64, isKnockout bool) error {
	rowIndex, found, err := c.findTournamentRow(tournamentID)
	if err != nil {
		return err
	}
	if !found {
		// Summary not yet processed — create a partial row; AppendTournament
		// will fill in the remaining columns when the summary file arrives.
		typeCell := ""
		if isKnockout {
			typeCell = storage.KnockoutPlaceholder
		}
		return c.appendRow("Tournaments", []interface{}{
			"", // Date
			typeCell,
			tournamentID,
			"", // Buy-In
			"", // Prize Pool
			"", // Multiplier
			"", // Finish Position
			"", // Total Players
			"", // Net
			vpip,
			pfr,
			bountyUSD,
			"", // Prize Won (USD) — filled in once AppendTournament knows BuyInUSD
			hands,
		})
	}

	snap, err := c.readTournamentRow(rowIndex)
	if err != nil {
		return err
	}

	// Every affected cell goes out in one batched call — previously this was
	// up to five sequential Values.Update calls (B, J:L, N, I, M), each gap
	// a window where a failure left the row internally inconsistent (e.g. a
	// new bounty recorded but Net not yet reconciled to include it).
	typeOut := snap.typeVal
	if isKnockout {
		typeOut = storage.AppendKnockoutSuffix(snap.typeVal)
	}
	data := []valueRange{
		{
			Range:  fmt.Sprintf("Tournaments!J%d:L%d", rowIndex, rowIndex),
			Values: [][]interface{}{{vpip, pfr, bountyUSD}},
		},
		{
			Range:  fmt.Sprintf("Tournaments!N%d", rowIndex),
			Values: [][]interface{}{{hands}},
		},
	}
	if typeOut != snap.typeVal {
		data = append(data, valueRange{
			Range:  fmt.Sprintf("Tournaments!B%d", rowIndex),
			Values: [][]interface{}{{typeOut}},
		})
	}

	var netUSD float64
	if snap.hasNetUSD {
		var prizeWon float64
		netUSD, prizeWon = storage.ReconcileTournamentBounty(snap.netUSD, snap.bountyUSD, bountyUSD, snap.buyInUSD)
		data = append(data,
			valueRange{
				Range:  fmt.Sprintf("Tournaments!I%d", rowIndex),
				Values: [][]interface{}{{netUSD}},
			},
			valueRange{
				Range:  fmt.Sprintf("Tournaments!M%d", rowIndex),
				Values: [][]interface{}{{prizeWon}},
			},
		)
	}

	if err := c.svc.valuesBatchUpdate(c.spreadsheetID, data); err != nil {
		return fmt.Errorf("updating hand stats for tournament %s: %w", tournamentID, err)
	}

	if !snap.hasNetUSD {
		// Summary still hasn't arrived — nothing to reconcile yet.
		return nil
	}
	return c.upsertLedgerTournamentRow(tournamentID, snap.date, netUSD)
}

// ── Write-ahead fallback ────────────────────────────────────────────────
//
// Two protections that must stay conceptually distinct: the raw* upserts
// already dedup by ID (find-or-append), so replaying a write that actually
// landed re-upserts the same *row* in place — no duplicate rows. Separately,
// the hand-count guard below stops a stale cached snapshot from *overwriting*
// newer data a later direct write already put on the sheet.

const (
	fallbackKindCash    = "cash"
	fallbackKindStats   = "tourney_stats"
	fallbackKindSummary = "tourney_summary"
)

// fallbackEntry is one queued write awaiting replay, holding exactly the
// arguments its raw* method needs. Kind selects which of the three payloads is
// populated; the map key is "<kind>:<id>" so a tournament's summary write and
// its hand-stats write (both keyed by TournamentID) never collide.
type fallbackEntry struct {
	Kind      string               `json:"kind"`
	Cash      *parsers.CashSession `json:"cash,omitempty"`
	Summary   *parsers.Tournament  `json:"summary,omitempty"`
	HandStats *handStatsArgs       `json:"hand_stats,omitempty"`
}

// handStatsArgs mirrors rawUpdateTournamentHandStats's parameter list so a
// failed hand-stats write can be replayed verbatim.
type handStatsArgs struct {
	TournamentID string  `json:"tournament_id"`
	VPIP         float64 `json:"vpip"`
	PFR          float64 `json:"pfr"`
	Hands        int     `json:"hands"`
	BountyUSD    float64 `json:"bounty_usd"`
	IsKnockout   bool    `json:"is_knockout"`
}

// AppendCashSession writes the session and, only if that fails, caches it for
// later replay; a success proves the sheet is reachable and flushes anything
// queued. The fallback wrappers below all follow this shape — the raw* method
// does the Sheets I/O and knows nothing about the fallback, so flush (which
// calls raw* directly) can never recurse back through here.
func (c *Client) AppendCashSession(session parsers.CashSession) error {
	if err := c.rawAppendCashSession(session); err != nil {
		key := fallbackKindCash + ":" + cashSessionID(session.Date, session.FirstHandID)
		c.cacheFailed(key, fallbackEntry{Kind: fallbackKindCash, Cash: &session})
		return err
	}
	c.flush()
	return nil
}

// AppendTournament wraps rawAppendTournament, preserving its (netUSD, error)
// return on both paths. Its fallback entry needs no hand-count guard —
// rawAppendTournament reads the row's bounty fresh every call and reconciles
// NetUSD from it, so a re-flush self-corrects regardless of ordering.
func (c *Client) AppendTournament(t parsers.Tournament) (float64, error) {
	netUSD, err := c.rawAppendTournament(t)
	if err != nil {
		key := fallbackKindSummary + ":" + t.TournamentID
		c.cacheFailed(key, fallbackEntry{Kind: fallbackKindSummary, Summary: &t})
		return netUSD, err
	}
	c.flush()
	return netUSD, nil
}

// UpdateTournamentHandStats wraps rawUpdateTournamentHandStats with the same
// cache-on-failure / flush-on-success shape.
func (c *Client) UpdateTournamentHandStats(tournamentID string, vpip, pfr float64, hands int, bountyUSD float64, isKnockout bool) error {
	if err := c.rawUpdateTournamentHandStats(tournamentID, vpip, pfr, hands, bountyUSD, isKnockout); err != nil {
		key := fallbackKindStats + ":" + tournamentID
		c.cacheFailed(key, fallbackEntry{Kind: fallbackKindStats, HandStats: &handStatsArgs{
			TournamentID: tournamentID, VPIP: vpip, PFR: pfr, Hands: hands, BountyUSD: bountyUSD, IsKnockout: isKnockout,
		}})
		return err
	}
	c.flush()
	return nil
}

// loadFallback reads the queued entries from fallbackPath into memory. An
// absent file is the normal empty state, not an error.
func (c *Client) loadFallback() error {
	data, err := os.ReadFile(c.fallbackPath)
	if errors.Is(err, os.ErrNotExist) {
		c.fallback = make(map[string]fallbackEntry)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading sheets fallback: %w", err)
	}
	var m map[string]fallbackEntry
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parsing sheets fallback: %w", err)
	}
	if m == nil {
		m = make(map[string]fallbackEntry)
	}
	c.fallback = m
	return nil
}

// persistFallbackLocked writes the map to disk atomically (tmp then rename,
// mirroring config.SaveConfig). The caller must hold fallbackMu.
func (c *Client) persistFallbackLocked() error {
	data, err := json.MarshalIndent(c.fallback, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling sheets fallback: %w", err)
	}
	tmp := c.fallbackPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("writing sheets fallback: %w", err)
	}
	if err := os.Rename(tmp, c.fallbackPath); err != nil {
		return fmt.Errorf("renaming sheets fallback: %w", err)
	}
	return nil
}

// cacheFailed queues a failed write for later replay and persists immediately
// so a crash never loses it. A no-op when the fallback is disabled.
func (c *Client) cacheFailed(key string, entry fallbackEntry) {
	if c.fallbackPath == "" {
		return
	}
	c.fallbackMu.Lock()
	c.fallback[key] = entry
	err := c.persistFallbackLocked()
	c.fallbackMu.Unlock()
	// Log after releasing the lock: slog.Error routes to alertChan
	// (telegramErrorHandler), and blocking on a full alertChan while holding
	// fallbackMu would deadlock the bot goroutine that drains it if it's
	// parked waiting for this same lock.
	if err != nil {
		slog.Error("persisting sheets fallback", "err", err)
	}
}

// flush replays every queued entry against the sheet, deleting each that
// resolves (replayed successfully, or discarded as superseded). Holds
// fallbackMu across the whole loop — for a single-user tool, serializing the
// two writer goroutines through one flush is simpler than any finer scheme and
// costs nothing. A no-op when disabled or empty.
func (c *Client) flush() {
	if c.fallbackPath == "" {
		return
	}
	c.fallbackMu.Lock()
	if len(c.fallback) == 0 {
		c.fallbackMu.Unlock()
		return
	}
	changed := false
	for key, entry := range c.fallback {
		if c.resolveFallbackEntry(entry) {
			delete(c.fallback, key)
			changed = true
		}
	}
	var persistErr error
	if changed {
		persistErr = c.persistFallbackLocked()
	}
	c.fallbackMu.Unlock()
	// Log after unlock — see cacheFailed for why holding fallbackMu across an
	// alertChan-routed slog call risks deadlocking the bot goroutine.
	if persistErr != nil {
		slog.Error("persisting sheets fallback after flush", "err", persistErr)
	}
}

// resolveFallbackEntry attempts one entry and reports whether it's resolved
// (should be deleted). false means keep it queued — a sheet read failed, or the
// replay itself failed transiently — to retry after the next successful write.
func (c *Client) resolveFallbackEntry(entry fallbackEntry) bool {
	switch entry.Kind {
	case fallbackKindCash:
		sheetHands, exists, err := c.cashSheetHandCount(cashSessionID(entry.Cash.Date, entry.Cash.FirstHandID))
		if err != nil {
			return false
		}
		if flushDecision(entry.Cash.Hands, sheetHands, exists) == flushDiscard {
			return true
		}
		return c.rawAppendCashSession(*entry.Cash) == nil
	case fallbackKindStats:
		sheetHands, exists, err := c.tournamentSheetHandCount(entry.HandStats.TournamentID)
		if err != nil {
			return false
		}
		if flushDecision(entry.HandStats.Hands, sheetHands, exists) == flushDiscard {
			return true
		}
		return c.rawUpdateTournamentHandStats(
			entry.HandStats.TournamentID, entry.HandStats.VPIP, entry.HandStats.PFR,
			entry.HandStats.Hands, entry.HandStats.BountyUSD, entry.HandStats.IsKnockout,
		) == nil
	case fallbackKindSummary:
		_, err := c.rawAppendTournament(*entry.Summary)
		return err == nil
	}
	return true // unknown kind — drop rather than retry forever
}

type flushChoice int

const (
	flushReplay flushChoice = iota
	flushDiscard
)

// flushDecision is the pure hand-count guard: discard a queued entry only when
// the sheet already holds a strictly higher hand count for the same key (a
// later direct write superseded this snapshot — replaying it would regress the
// sheet backward). Otherwise replay. A row that doesn't exist yet has nothing
// to supersede, so it always replays.
func flushDecision(cachedHands, sheetHands int, rowExists bool) flushChoice {
	if rowExists && sheetHands > cachedHands {
		return flushDiscard
	}
	return flushReplay
}

// cashSheetHandCount reads the Hands value (column C) of the Cash Sessions row
// with the given session ID. exists is false when no such row is on the sheet
// yet, in which case there's nothing to supersede.
func (c *Client) cashSheetHandCount(sessionID string) (hands int, exists bool, err error) {
	rowIndex, found, err := c.findRowByColumnValue("Cash Sessions", "J", sessionID)
	if err != nil {
		return 0, false, err
	}
	if !found {
		return 0, false, nil
	}
	resp, err := c.svc.valuesGet(c.spreadsheetID, fmt.Sprintf("Cash Sessions!C%d", rowIndex))
	if err != nil {
		return 0, false, fmt.Errorf("reading cash session hand count: %w", err)
	}
	if len(resp.Values) == 0 || len(resp.Values[0]) == 0 {
		return 0, true, nil
	}
	return parseInt(resp.Values[0][0]), true, nil
}

// tournamentSheetHandCount reads the Hands value (column N) of the Tournaments
// row with the given ID. exists is false when no such row is on the sheet yet.
func (c *Client) tournamentSheetHandCount(tournamentID string) (hands int, exists bool, err error) {
	rowIndex, found, err := c.findTournamentRow(tournamentID)
	if err != nil {
		return 0, false, err
	}
	if !found {
		return 0, false, nil
	}
	resp, err := c.svc.valuesGet(c.spreadsheetID, fmt.Sprintf("Tournaments!N%d", rowIndex))
	if err != nil {
		return 0, false, fmt.Errorf("reading tournament hand count: %w", err)
	}
	if len(resp.Values) == 0 || len(resp.Values[0]) == 0 {
		return 0, true, nil
	}
	return parseInt(resp.Values[0][0]), true, nil
}

// AppendLedgerEntry appends a deposit or withdrawal row to Bankroll Ledger.
// For withdrawals, the caller passes a negative amountUSD. The date is
// written as the local wall clock, not UTC — session rows' dates come from
// PokerStars' zone-less local timestamps, so writing a true-UTC time here
// would put deposits in a date coordinate system shifted from every session
// row's by the machine's UTC offset, and window queries would misplace them
// near day boundaries.
func (c *Client) AppendLedgerEntry(entryType string, amountUSD float64) error {
	prev, err := c.getLastRunningTotal()
	if err != nil {
		return err
	}
	return c.appendRow("Bankroll Ledger", []interface{}{
		time.Now().Format(dateFormat),
		entryType,
		amountUSD,
		prev + amountUSD,
	})
}

// GetCurrentBankroll returns the running total from the last Bankroll Ledger row.
// Returns 0 if the ledger is empty.
func (c *Client) GetCurrentBankroll() (float64, error) {
	return c.getLastRunningTotal()
}

// GetSheetURL returns the Google Sheets URL for this spreadsheet.
func (c *Client) GetSheetURL() string {
	return "https://docs.google.com/spreadsheets/d/" + c.spreadsheetID
}

// GetLedgerEntriesSince returns all Bankroll Ledger rows where Date >= t.
func (c *Client) GetLedgerEntriesSince(t time.Time) ([]storage.LedgerEntry, error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, "Bankroll Ledger!A:E")
	if err != nil {
		return nil, fmt.Errorf("reading bankroll ledger: %w", err)
	}
	if len(resp.Values) <= 1 {
		return nil, nil
	}
	entries, err := parseLedgerRows(resp.Values[1:])
	if err != nil {
		return nil, err
	}
	var result []storage.LedgerEntry
	for _, e := range entries {
		if !e.Date.Before(t) {
			result = append(result, e)
		}
	}
	return result, nil
}

// queryCashSessions reads every Cash Sessions row with Date >= since,
// including Session ID — the one column GetCashSessionsSince's existing
// callers (stats, summaries) don't need but /export does. GetCashSessionsSince
// and GetAllCashSessions are both thin wrappers over this single read,
// differing only in the date cutoff and whether the caller wants the
// Session ID kept or discarded.
func (c *Client) queryCashSessions(since time.Time) ([]storage.CashSessionRecord, error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, "Cash Sessions!A:J")
	if err != nil {
		return nil, fmt.Errorf("reading cash sessions: %w", err)
	}
	if len(resp.Values) <= 1 {
		return nil, nil
	}
	var result []storage.CashSessionRecord
	for _, row := range resp.Values[1:] {
		rec, ok := parseCashRow(row)
		if !ok || rec.Date.Before(since) {
			continue
		}
		result = append(result, rec)
	}
	return result, nil
}

// parseCashRow parses one Cash Sessions row (columns A:J) into a record. ok is
// false when the row is shorter than the 9 always-written columns (a
// hand-edited or otherwise malformed row — the script never writes fewer) or
// column A doesn't hold a valid date. Shared by queryCashSessions and the
// migration round-trip so both decode a row identically.
func parseCashRow(row []interface{}) (rec storage.CashSessionRecord, ok bool) {
	if len(row) < 9 {
		return storage.CashSessionRecord{}, false
	}
	date, err := parseSheetDate(cell(row, 0))
	if err != nil {
		return storage.CashSessionRecord{}, false
	}
	rec = storage.CashSessionRecord{
		CashSession: parsers.CashSession{
			Date:        date,
			Stake:       fmt.Sprint(cell(row, 1)),
			Hands:       parseInt(cell(row, 2)),
			DurationMin: parseInt(cell(row, 3)),
			NetUSD:      parseFloat(cell(row, 4)),
			RakeUSD:     parseFloat(cell(row, 5)),
			BB100:       parseFloat(cell(row, 6)),
			VPIP:        parseFloat(cell(row, 7)),
			PFR:         parseFloat(cell(row, 8)),
		},
		SessionID: fmt.Sprint(cell(row, 9)),
	}
	return rec, true
}

// GetCashSessionsSince returns all Cash Sessions rows where Date >= t.
// BuyInUSD is not stored in the sheet and will be zero in returned structs.
func (c *Client) GetCashSessionsSince(t time.Time) ([]parsers.CashSession, error) {
	records, err := c.queryCashSessions(t)
	if err != nil {
		return nil, err
	}
	result := make([]parsers.CashSession, len(records))
	for i, r := range records {
		result[i] = r.CashSession
	}
	return result, nil
}

// GetAllCashSessions returns every Cash Sessions row, including Session ID.
// Used only by /export.
func (c *Client) GetAllCashSessions() ([]storage.CashSessionRecord, error) {
	return c.queryCashSessions(time.Time{})
}

// queryTournaments reads every Tournaments row with Date >= since, including
// VPIP, PFR, Bounty Won, and Prize Won — columns GetTournamentsSince's
// existing callers don't need but /export does. Partial rows (no Date)
// created by UpdateTournamentHandStats before the summary file arrives are
// skipped. GetTournamentsSince and GetAllTournaments are both thin wrappers
// over this single read, differing only in the date cutoff and whether the
// caller wants those extra columns kept or discarded.
func (c *Client) queryTournaments(since time.Time) ([]storage.TournamentRecord, error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, "Tournaments!A:N")
	if err != nil {
		return nil, fmt.Errorf("reading tournaments: %w", err)
	}
	if len(resp.Values) <= 1 {
		return nil, nil
	}
	var result []storage.TournamentRecord
	for _, row := range resp.Values[1:] {
		// Completed rows always have at least the 9 summary columns; shorter
		// means hand-edited/malformed (partial rows are skipped below via
		// hasDate regardless).
		if len(row) < 9 {
			continue
		}
		rec, hasDate := parseTournamentRow(row)
		if !hasDate || rec.Date.Before(since) {
			continue
		}
		result = append(result, rec)
	}
	return result, nil
}

// parseTournamentRow parses one Tournaments row (columns A:N) into a record.
// hasDate is false for a partial row — a hand file parsed before its summary
// arrived, whose Date cell (column A) is blank — which callers reading only
// completed tournaments skip, but the migration snapshot keeps (with a zero
// Date). Shared by queryTournaments, allTournamentRows, and the migration
// round-trip so every path decodes a row identically.
func parseTournamentRow(row []interface{}) (rec storage.TournamentRecord, hasDate bool) {
	rec.Type = fmt.Sprint(cell(row, 1))
	rec.TournamentID = fmt.Sprint(cell(row, 2))
	if date, err := parseSheetDate(cell(row, 0)); err == nil {
		rec.Date = date
		hasDate = true
	}
	rec.BuyInUSD = parseFloat(cell(row, 3))
	// Prize Pool (col E) holds a number for a real cash pool, or text for a
	// ticket-denominated one (e.g. "$1.00 Power Path Step 2 Ticket") — see
	// AppendTournament's prizePool write.
	if desc, ok := cell(row, 4).(string); ok && desc != "" {
		rec.PrizePoolDescription = desc
	} else {
		rec.PrizePoolUSD = parseFloat(cell(row, 4))
	}
	// Multiplier (col F) is empty for MTT rows.
	if mult := fmt.Sprint(cell(row, 5)); mult != "" {
		m := parseFloat(cell(row, 5))
		rec.Multiplier = &m
	}
	rec.FinishPos = parseInt(cell(row, 6))
	rec.TotalPlayers = parseInt(cell(row, 7))
	rec.NetUSD = parseFloat(cell(row, 8))
	if v := fmt.Sprint(cell(row, 9)); v != "" {
		f := parseFloat(cell(row, 9))
		rec.VPIP = &f
	}
	if p := fmt.Sprint(cell(row, 10)); p != "" {
		f := parseFloat(cell(row, 10))
		rec.PFR = &f
	}
	rec.BountyWonUSD = parseFloat(cell(row, 11))
	rec.PrizeWonUSD = parseFloat(cell(row, 12))
	if h := fmt.Sprint(cell(row, 13)); h != "" {
		n := parseInt(cell(row, 13))
		rec.Hands = &n
	}
	return rec, hasDate
}

// allTournamentRows reads every Tournaments row including partial ones (blank
// Date), for the migration snapshot. queryTournaments can't be reused: it
// drops partials, which a migration must carry so an in-progress tournament
// keeps its hand-file stats across a backend switch.
func (c *Client) allTournamentRows() ([]storage.TournamentRecord, error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, "Tournaments!A:N")
	if err != nil {
		return nil, fmt.Errorf("reading tournaments: %w", err)
	}
	if len(resp.Values) <= 1 {
		return nil, nil
	}
	var result []storage.TournamentRecord
	for _, row := range resp.Values[1:] {
		if len(row) < 3 {
			continue
		}
		rec, _ := parseTournamentRow(row)
		result = append(result, rec)
	}
	return result, nil
}

// GetTournamentByID returns the single completed tournament row with this
// TournamentID — the point lookup the worker's duplicate check runs per file
// event: an ID-column read plus one row read, instead of the whole tab.
// found is false for an absent tournament and for a partial row (hand file
// before summary — blank Date cell), matching GetTournamentsSince's
// partial-row skip.
func (c *Client) GetTournamentByID(tournamentID string) (parsers.Tournament, bool, error) {
	rowIndex, found, err := c.findTournamentRow(tournamentID)
	if err != nil || !found {
		return parsers.Tournament{}, false, err
	}
	resp, err := c.svc.valuesGet(c.spreadsheetID, fmt.Sprintf("Tournaments!A%d:N%d", rowIndex, rowIndex))
	if err != nil {
		return parsers.Tournament{}, false, fmt.Errorf("reading tournament row %d: %w", rowIndex, err)
	}
	if len(resp.Values) == 0 {
		return parsers.Tournament{}, false, nil
	}
	rec, hasDate := parseTournamentRow(resp.Values[0])
	if !hasDate {
		return parsers.Tournament{}, false, nil
	}
	return rec.Tournament, true, nil
}

// GetTournamentsSince returns all Tournaments rows where Date >= since.
// Partial rows (no Date) created by UpdateTournamentHandStats are skipped.
func (c *Client) GetTournamentsSince(since time.Time) ([]parsers.Tournament, error) {
	records, err := c.queryTournaments(since)
	if err != nil {
		return nil, err
	}
	result := make([]parsers.Tournament, len(records))
	for i, r := range records {
		result[i] = r.Tournament
	}
	return result, nil
}

// GetAllTournaments returns every Tournaments row, including VPIP, PFR,
// Bounty Won, and Prize Won. Used only by /export.
func (c *Client) GetAllTournaments() ([]storage.TournamentRecord, error) {
	return c.queryTournaments(time.Time{})
}

// GetAllTimeAnalysis returns every Time Analysis row. Used only by /export.
func (c *Client) GetAllTimeAnalysis() ([]storage.TimeAnalysisRecord, error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, "Time Analysis!A:E")
	if err != nil {
		return nil, fmt.Errorf("reading time analysis: %w", err)
	}
	if len(resp.Values) <= 1 {
		return nil, nil
	}
	var result []storage.TimeAnalysisRecord
	for _, row := range resp.Values[1:] {
		rec, ok := parseTimeRow(row)
		if !ok {
			continue
		}
		result = append(result, rec)
	}
	return result, nil
}

// parseTimeRow parses one Time Analysis row (columns A:E). ok is false when
// the row is shorter than the 4 always-written columns (hand-edited or
// malformed) or column A doesn't hold a valid date. Shared by
// GetAllTimeAnalysis and the migration round-trip.
func parseTimeRow(row []interface{}) (rec storage.TimeAnalysisRecord, ok bool) {
	if len(row) < 4 {
		return storage.TimeAnalysisRecord{}, false
	}
	date, err := parseSheetDate(cell(row, 0))
	if err != nil {
		return storage.TimeAnalysisRecord{}, false
	}
	return storage.TimeAnalysisRecord{
		Date:      date,
		Hour:      parseInt(cell(row, 1)),
		DayOfWeek: fmt.Sprint(cell(row, 2)),
		NetUSD:    parseFloat(cell(row, 3)),
		SessionID: fmt.Sprint(cell(row, 4)),
	}, true
}

// ExportSnapshot returns every row of all four tabs as full records, for a
// backend migration. Unlike GetAllTournaments its Tournaments slice keeps
// partial rows (blank Date), read via allTournamentRows.
func (c *Client) ExportSnapshot() (storage.Snapshot, error) {
	cash, err := c.GetAllCashSessions()
	if err != nil {
		return storage.Snapshot{}, err
	}
	tournaments, err := c.allTournamentRows()
	if err != nil {
		return storage.Snapshot{}, err
	}
	ledger, err := c.GetLedgerEntriesSince(time.Time{})
	if err != nil {
		return storage.Snapshot{}, err
	}
	timeAnalysis, err := c.GetAllTimeAnalysis()
	if err != nil {
		return storage.Snapshot{}, err
	}
	return storage.Snapshot{
		CashSessions: cash,
		Tournaments:  tournaments,
		Ledger:       ledger,
		TimeAnalysis: timeAnalysis,
	}, nil
}

// ImportSnapshot writes a whole snapshot into an empty spreadsheet, one
// batched append per tab (four API calls, not one per row — a real history
// would otherwise stall against the Sheets write quota). Every value is
// written verbatim: session IDs, dates (truncated to the tab's minute
// precision), running totals, and each tournament's already-bounty-folded Net
// and Prize Won are stored as-read, never recomputed. The caller
// (storage.Migrate) guarantees the tabs hold only their header row first.
func (c *Client) ImportSnapshot(snap storage.Snapshot) error {
	if rows := buildCashRows(snap.CashSessions); len(rows) > 0 {
		if err := c.svc.valuesAppend(c.spreadsheetID, "Cash Sessions!A1", rows); err != nil {
			return fmt.Errorf("importing cash sessions: %w", err)
		}
	}
	if rows := buildTournamentRows(snap.Tournaments); len(rows) > 0 {
		if err := c.svc.valuesAppend(c.spreadsheetID, "Tournaments!A1", rows); err != nil {
			return fmt.Errorf("importing tournaments: %w", err)
		}
	}
	if rows := buildLedgerRows(snap.Ledger); len(rows) > 0 {
		if err := c.svc.valuesAppend(c.spreadsheetID, "Bankroll Ledger!A1", rows); err != nil {
			return fmt.Errorf("importing bankroll ledger: %w", err)
		}
	}
	if rows := buildTimeRows(snap.TimeAnalysis); len(rows) > 0 {
		if err := c.svc.valuesAppend(c.spreadsheetID, "Time Analysis!A1", rows); err != nil {
			return fmt.Errorf("importing time analysis: %w", err)
		}
	}
	return nil
}

// buildCashRows / buildTournamentRows / buildLedgerRows / buildTimeRows map
// records to the cell layout each tab's parse* counterpart reads back. They're
// pure (no Client, no I/O) so a migration's column alignment is unit-tested by
// round-tripping build → parse without a live sheet.
func buildCashRows(sessions []storage.CashSessionRecord) [][]interface{} {
	rows := make([][]interface{}, len(sessions))
	for i, s := range sessions {
		rows[i] = []interface{}{
			s.Date.UTC().Format(dateFormat), s.Stake, s.Hands, s.DurationMin,
			s.NetUSD, s.RakeUSD, s.BB100, s.VPIP, s.PFR, s.SessionID,
		}
	}
	return rows
}

func buildTournamentRows(tournaments []storage.TournamentRecord) [][]interface{} {
	rows := make([][]interface{}, len(tournaments))
	for i, t := range tournaments {
		if t.Date.IsZero() {
			// Partial row: Date and every summary-only column blank, exactly
			// as rawUpdateTournamentHandStats's partial append writes them, so
			// Net (col I) stays empty and isn't read back as summary data.
			rows[i] = []interface{}{
				"", t.Type, t.TournamentID, "", "", "", "", "", "",
				floatCell(t.VPIP), floatCell(t.PFR), t.BountyWonUSD, "", intCell(t.Hands),
			}
			continue
		}
		var prizePool interface{} = t.PrizePoolUSD
		if t.PrizePoolDescription != "" {
			prizePool = t.PrizePoolDescription
		}
		var multiplier interface{} = ""
		if t.Multiplier != nil {
			multiplier = *t.Multiplier
		}
		rows[i] = []interface{}{
			t.Date.UTC().Format(dateFormat), t.Type, t.TournamentID, t.BuyInUSD, prizePool, multiplier,
			t.FinishPos, t.TotalPlayers, t.NetUSD, floatCell(t.VPIP), floatCell(t.PFR),
			t.BountyWonUSD, t.PrizeWonUSD, intCell(t.Hands),
		}
	}
	return rows
}

func buildLedgerRows(entries []storage.LedgerEntry) [][]interface{} {
	rows := make([][]interface{}, len(entries))
	for i, e := range entries {
		rows[i] = []interface{}{
			e.Date.UTC().Format(dateFormat), e.Type, e.AmountUSD, e.RunningTotal, e.SessionID,
		}
	}
	return rows
}

func buildTimeRows(records []storage.TimeAnalysisRecord) [][]interface{} {
	rows := make([][]interface{}, len(records))
	for i, r := range records {
		rows[i] = []interface{}{
			r.Date.UTC().Format(dateFormat), r.Hour, r.DayOfWeek, r.NetUSD, r.SessionID,
		}
	}
	return rows
}

// floatCell / intCell write an optional numeric field as its value, or an
// empty cell when unset (nil) — VPIP/PFR/Hands on a tournament that has no hand
// data yet, which must read back as a nil pointer, not 0.
func floatCell(p *float64) interface{} {
	if p == nil {
		return ""
	}
	return *p
}

func intCell(p *int) interface{} {
	if p == nil {
		return ""
	}
	return *p
}

// PendingWrites reports how many failed writes are queued in the write-ahead
// fallback. A migration must refuse to run while any are pending — they're
// invisible to the read methods, so the source data on the sheet is
// incomplete until they flush. Always 0 for a fallback-disabled client.
func (c *Client) PendingWrites() int {
	c.fallbackMu.Lock()
	defer c.fallbackMu.Unlock()
	return len(c.fallback)
}

// ClearAllData deletes every data row (row 2 onward) from all four tabs,
// leaving the header rows. Used to roll a spreadsheet back to empty after a
// failed migration import, so a retry passes the empty-target check.
func (c *Client) ClearAllData() error {
	for _, tab := range []string{"Cash Sessions", "Tournaments", "Bankroll Ledger", "Time Analysis"} {
		if err := c.svc.valuesClear(c.spreadsheetID, tab+"!A2:Z"); err != nil {
			return fmt.Errorf("clearing %s: %w", tab, err)
		}
	}
	return nil
}

func parseLedgerRows(rows [][]interface{}) ([]storage.LedgerEntry, error) {
	entries := make([]storage.LedgerEntry, 0, len(rows))
	for _, row := range rows {
		if len(row) < 4 {
			continue
		}
		date, err := parseSheetDate(row[0])
		if err != nil {
			return nil, fmt.Errorf("parsing ledger date %q: %w", row[0], err)
		}
		entry := storage.LedgerEntry{
			Date:         date,
			Type:         fmt.Sprint(row[1]),
			AmountUSD:    parseFloat(row[2]),
			RunningTotal: parseFloat(row[3]),
		}
		if len(row) > 4 {
			entry.SessionID = fmt.Sprint(row[4])
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// cell returns row[i], or "" when the row is shorter than i+1 — the Sheets API
// omits trailing empty cells, so a row read back is often shorter than its
// full column count. Lets the row parsers index any column without a length
// check at every access.
func cell(row []interface{}, i int) interface{} {
	if i < len(row) {
		return row[i]
	}
	return ""
}

func parseInt(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func parseFloat(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}
