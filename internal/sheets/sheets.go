// Package sheets manages the Google Sheets integration: tab setup, row appends,
// and reads used by the bot and hardstop evaluators.
package sheets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
	sheetsapi "google.golang.org/api/sheets/v4"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

const dateFormat = "2006-01-02 15:04"

// Client holds the authenticated Sheets service and spreadsheet ID.
type Client struct {
	svc           *sheetsapi.Service
	spreadsheetID string
}

var _ storage.Store = (*Client)(nil)

// NewClient authenticates with the service account JSON and connects to the
// spreadsheet identified by spreadsheetID. Calls ensureTabs on startup.
func NewClient(serviceAccountPath, spreadsheetID string) (*Client, error) {
	data, err := os.ReadFile(serviceAccountPath)
	if err != nil {
		return nil, fmt.Errorf("reading service account: %w", err)
	}

	creds, err := google.CredentialsFromJSON(context.Background(), data,
		sheetsapi.SpreadsheetsScope,
	)
	if err != nil {
		return nil, fmt.Errorf("parsing credentials: %w", err)
	}

	svc, err := sheetsapi.NewService(context.Background(), option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("creating sheets service: %w", err)
	}

	c := &Client{
		svc:           svc,
		spreadsheetID: spreadsheetID,
	}

	if err := c.ensureTabs(); err != nil {
		return nil, err
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

	creds, err := google.CredentialsFromJSON(context.Background(), data,
		drive.DriveMetadataReadonlyScope,
	)
	if err != nil {
		return "", fmt.Errorf("parsing credentials: %w", err)
	}

	driveSvc, err := drive.NewService(context.Background(), option.WithCredentials(creds))
	if err != nil {
		return "", fmt.Errorf("creating drive service: %w", err)
	}

	q := fmt.Sprintf("name = '%s' and mimeType = 'application/vnd.google-apps.spreadsheet'", sheetName)
	list, err := driveSvc.Files.List().Q(q).Fields("files(id)").Do()
	if err != nil {
		return "", fmt.Errorf("searching for spreadsheet: %w", err)
	}
	if len(list.Files) == 0 {
		return "", fmt.Errorf("spreadsheet %q not found", sheetName)
	}
	if len(list.Files) > 1 {
		return "", fmt.Errorf("spreadsheet name %q is ambiguous: %d matches found", sheetName, len(list.Files))
	}

	return list.Files[0].Id, nil
}

func (c *Client) ensureTabs() error {
	sp, err := c.svc.Spreadsheets.Get(c.spreadsheetID).Do()
	if err != nil {
		return fmt.Errorf("getting spreadsheet: %w", err)
	}

	existing := make(map[string]bool, len(sp.Sheets))
	for _, sheet := range sp.Sheets {
		existing[sheet.Properties.Title] = true
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
			_, err := c.svc.Spreadsheets.BatchUpdate(c.spreadsheetID, &sheetsapi.BatchUpdateSpreadsheetRequest{
				Requests: []*sheetsapi.Request{{
					AddSheet: &sheetsapi.AddSheetRequest{
						Properties: &sheetsapi.SheetProperties{Title: tab.name},
					},
				}},
			}).Do()
			if err != nil {
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
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		fmt.Sprintf("%s!1:1", tab),
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
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
	if _, err := c.svc.Spreadsheets.Values.Update(
		c.spreadsheetID, rangeStr,
		&sheetsapi.ValueRange{Values: [][]interface{}{want[have:]}},
	).ValueInputOption("RAW").Do(); err != nil {
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
	_, err := c.svc.Spreadsheets.Values.Append(
		c.spreadsheetID,
		tab+"!A1",
		&sheetsapi.ValueRange{Values: [][]interface{}{values}},
	).ValueInputOption("RAW").Do()
	if err != nil {
		return fmt.Errorf("appending to %q: %w", tab, err)
	}
	return nil
}

// getLastRunningTotal reads the last value from the Running Total column of
// Bankroll Ledger. Returns 0 when the ledger has no data rows.
func (c *Client) getLastRunningTotal() (float64, error) {
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		"Bankroll Ledger!D:D",
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
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
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		fmt.Sprintf("%s!%s:%s", tab, columnLetter, columnLetter),
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
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

// runningTotalBeforeRow returns the Bankroll Ledger running total as of the
// row immediately above rowIndex — 0 if rowIndex is the first data row. Used
// when correcting an existing cash-session ledger row in place, where
// getLastRunningTotal (the sheet's last row) would double-count that row's
// own stale amount if it happens to already be the last row.
func (c *Client) runningTotalBeforeRow(rowIndex int) (float64, error) {
	if rowIndex <= 2 {
		return 0, nil
	}
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		fmt.Sprintf("Bankroll Ledger!D%d", rowIndex-1),
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
	if err != nil {
		return 0, fmt.Errorf("reading running total above row %d: %w", rowIndex, err)
	}
	if len(resp.Values) == 0 || len(resp.Values[0]) == 0 {
		return 0, nil
	}
	return parseFloat(resp.Values[0][0]), nil
}

// AppendCashSession upserts one row in Cash Sessions, one row in Time
// Analysis, and one row in Bankroll Ledger — all three keyed by the same
// cashSessionID, so a cash hand history file that's re-parsed after every
// hand corrects its existing rows in place instead of appending a new set
// each time. Mirrors AppendTournament's find-or-append pattern, applied to
// all three tabs a cash session touches rather than just one.
func (c *Client) AppendCashSession(session parsers.CashSession) error {
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
	if _, err := c.svc.Spreadsheets.Values.Update(
		c.spreadsheetID, rangeStr,
		&sheetsapi.ValueRange{Values: [][]interface{}{values}},
	).ValueInputOption("RAW").Do(); err != nil {
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
	if _, err := c.svc.Spreadsheets.Values.Update(
		c.spreadsheetID, rangeStr,
		&sheetsapi.ValueRange{Values: [][]interface{}{values}},
	).ValueInputOption("RAW").Do(); err != nil {
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
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID, "Bankroll Ledger!C:D",
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
	if err != nil {
		return fmt.Errorf("reading bankroll ledger for cascade update: %w", err)
	}

	base, err := c.runningTotalBeforeRow(rowIndex)
	if err != nil {
		return err
	}

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
	if _, err := c.svc.Spreadsheets.Values.Update(
		c.spreadsheetID, rangeStr,
		&sheetsapi.ValueRange{Values: updates},
	).ValueInputOption("RAW").Do(); err != nil {
		return fmt.Errorf("cascading bankroll ledger update: %w", err)
	}
	return nil
}

// cascadeLedgerAmounts computes updated (Amount, Running Total) pairs for
// the ledger's Amount column from changedIdx (0-based, data rows only, no
// header) through the last row. amounts[changedIdx] becomes newAmount;
// every other row keeps its existing amount but gets a recomputed running
// total, since a change earlier in the ledger shifts every total after it.
// base is the running total of the row immediately before changedIdx (0 if
// changedIdx is the first data row). Pure — the Sheets I/O wrapper is
// updateLedgerAmountCascading.
func cascadeLedgerAmounts(amounts []float64, changedIdx int, newAmount, base float64) [][]interface{} {
	if changedIdx < 0 || changedIdx >= len(amounts) {
		return nil
	}
	running := base
	updates := make([][]interface{}, 0, len(amounts)-changedIdx)
	for i := changedIdx; i < len(amounts); i++ {
		amount := amounts[i]
		if i == changedIdx {
			amount = newAmount
		}
		running += amount
		updates = append(updates, []interface{}{amount, running})
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

// knockoutPlaceholder marks the Type cell of a partial row (created by
// UpdateTournamentHandStats before the summary file has arrived) as a
// knockout tournament, since the base "spingo"/"mtt" classification isn't
// known yet at that point — AppendTournament combines it into the real
// "<base>-ko" Type once it does.
const knockoutPlaceholder = "ko"

// readTournamentRow reads the fields of an existing Tournaments row (by
// 1-based sheet rowIndex) needed to compute a bounty-reconciled NetUSD/Type
// from either side of the two-file correlation.
func (c *Client) readTournamentRow(rowIndex int) (tournamentRowSnapshot, error) {
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		fmt.Sprintf("Tournaments!A%d:M%d", rowIndex, rowIndex),
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
	if err != nil {
		return tournamentRowSnapshot{}, fmt.Errorf("reading tournament row %d: %w", rowIndex, err)
	}
	if len(resp.Values) == 0 {
		return tournamentRowSnapshot{}, nil
	}
	row := resp.Values[0]
	var snap tournamentRowSnapshot
	if len(row) > 0 {
		if date, err := time.Parse(dateFormat, fmt.Sprint(row[0])); err == nil {
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

// foldBountyIntoNet computes the final NetUSD and Prize Won (USD) once the
// summary file's baseline net (payout - buyin) is known, folding in whatever
// bounty winnings the hand history file has already recorded for this row.
// Pure — the Sheets I/O wrapper is AppendTournament.
func foldBountyIntoNet(baselineNetUSD, bountyUSD, buyInUSD float64) (netUSD, prizeWonUSD float64) {
	netUSD = baselineNetUSD + bountyUSD
	return netUSD, netUSD + buyInUSD
}

// reconcileKnockoutType computes the Type value AppendTournament should
// write once the base type ("spingo"/"mtt") is known, combining it with
// whatever knockout signal an existing row (created by
// UpdateTournamentHandStats before the summary file arrived) already
// recorded in its Type cell — either knockoutPlaceholder or a Type already
// carrying parsers.KnockoutSuffix. Pure — the Sheets I/O wrapper is
// AppendTournament.
func reconcileKnockoutType(baseType, existingTypeCell string) string {
	if existingTypeCell == knockoutPlaceholder || strings.HasSuffix(existingTypeCell, parsers.KnockoutSuffix) {
		return baseType + parsers.KnockoutSuffix
	}
	return baseType
}

// appendKnockoutSuffix computes the Type value UpdateTournamentHandStats
// should write when a hand history file flags a tournament as knockout,
// preserving whatever base type (or lack thereof, via knockoutPlaceholder)
// the row's Type cell already has. Idempotent — a Type that's already
// suffixed or already the placeholder is returned unchanged. Pure — the
// Sheets I/O wrapper is UpdateTournamentHandStats.
func appendKnockoutSuffix(existingTypeCell string) string {
	if existingTypeCell == knockoutPlaceholder || strings.HasSuffix(existingTypeCell, parsers.KnockoutSuffix) {
		return existingTypeCell
	}
	if existingTypeCell == "" {
		return knockoutPlaceholder
	}
	return existingTypeCell + parsers.KnockoutSuffix
}

// reconcileTournamentBounty computes the corrected NetUSD and Prize Won
// (USD) after a freshly reparsed cumulative bounty total replaces whatever
// this row's previous bounty contribution to storedNetUSD was. Pure — the
// Sheets I/O wrapper is UpdateTournamentHandStats.
func reconcileTournamentBounty(storedNetUSD, storedBountyUSD, newBountyUSD, buyInUSD float64) (netUSD, prizeWonUSD float64) {
	netUSD = (storedNetUSD - storedBountyUSD) + newBountyUSD
	return netUSD, netUSD + buyInUSD
}

// AppendTournament upserts one row in Tournaments (keyed by TournamentID) and
// upserts its Bankroll Ledger row (see upsertLedgerTournamentRow). If the
// Tournaments row already exists (created by UpdateTournamentHandStats),
// updates columns A–I, folding in whatever bounty winnings that row already
// has recorded (column L, Bounty Won) — the summary file this data comes
// from has no concept of bounties at all, only the hand history file's
// elimination lines do (why two files: plan §Why Tournaments Need Two
// Files) — and carries forward the knockoutPlaceholder into a real
// "<base>-ko" Type if the row was flagged knockout. Writes Prize Won
// (column M, = NetUSD + BuyInUSD) alongside. Leaves VPIP/PFR/Bounty Won
// (J:L) untouched. Returns the final NetUSD (baseline net plus any bounty)
// so callers can report or evaluate against the real total rather than the
// pre-bounty figure.
func (c *Client) AppendTournament(t parsers.Tournament) (float64, error) {
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
		typeOut = reconcileKnockoutType(t.Type, snap.typeVal)
	}
	netUSD, prizeWon := foldBountyIntoNet(t.NetUSD, bounty, t.BuyInUSD)

	if found {
		// Row was pre-created by UpdateTournamentHandStats — update main
		// columns only, leaving VPIP/PFR/Bounty Won (J:L) untouched.
		rangeStr := fmt.Sprintf("Tournaments!A%d:I%d", rowIndex, rowIndex)
		_, err := c.svc.Spreadsheets.Values.Update(
			c.spreadsheetID, rangeStr,
			&sheetsapi.ValueRange{Values: [][]interface{}{{
				t.Date.Format(dateFormat),
				typeOut,
				t.TournamentID,
				t.BuyInUSD,
				prizePool,
				multiplier,
				t.FinishPos,
				t.TotalPlayers,
				netUSD,
			}}},
		).ValueInputOption("RAW").Do()
		if err != nil {
			return 0, fmt.Errorf("updating tournament row: %w", err)
		}

		mRange := fmt.Sprintf("Tournaments!M%d", rowIndex)
		if _, err := c.svc.Spreadsheets.Values.Update(
			c.spreadsheetID, mRange,
			&sheetsapi.ValueRange{Values: [][]interface{}{{prizeWon}}},
		).ValueInputOption("RAW").Do(); err != nil {
			return 0, fmt.Errorf("updating prize won for tournament %s: %w", t.TournamentID, err)
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
// with Bounty Won and (if isKnockout) knockoutPlaceholder in place of Type,
// and stops there — AppendTournament will complete it once the summary file
// arrives, combining the placeholder into a real "<base>-ko" Type and
// folding in this bounty (see its doc comment). If a row exists but summary
// data (NetUSD, column I) hasn't arrived yet either, only J:L, N (and
// possibly B, for the same knockoutPlaceholder reason) are written and
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
func (c *Client) UpdateTournamentHandStats(tournamentID string, vpip, pfr float64, hands int, bountyUSD float64, isKnockout bool) error {
	rowIndex, found, err := c.findTournamentRow(tournamentID)
	if err != nil {
		return err
	}
	if !found {
		// Summary not yet processed — create a partial row; AppendTournament
		// will fill in the remaining columns when the summary file arrives.
		typeCell := ""
		if isKnockout {
			typeCell = knockoutPlaceholder
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

	typeOut := snap.typeVal
	if isKnockout {
		typeOut = appendKnockoutSuffix(snap.typeVal)
	}
	if typeOut != snap.typeVal {
		bRange := fmt.Sprintf("Tournaments!B%d", rowIndex)
		if _, err := c.svc.Spreadsheets.Values.Update(
			c.spreadsheetID, bRange,
			&sheetsapi.ValueRange{Values: [][]interface{}{{typeOut}}},
		).ValueInputOption("RAW").Do(); err != nil {
			return fmt.Errorf("updating type for tournament %s: %w", tournamentID, err)
		}
	}

	jlRange := fmt.Sprintf("Tournaments!J%d:L%d", rowIndex, rowIndex)
	if _, err := c.svc.Spreadsheets.Values.Update(
		c.spreadsheetID, jlRange,
		&sheetsapi.ValueRange{Values: [][]interface{}{{vpip, pfr, bountyUSD}}},
	).ValueInputOption("RAW").Do(); err != nil {
		return fmt.Errorf("updating hand stats for tournament %s: %w", tournamentID, err)
	}

	nRange := fmt.Sprintf("Tournaments!N%d", rowIndex)
	if _, err := c.svc.Spreadsheets.Values.Update(
		c.spreadsheetID, nRange,
		&sheetsapi.ValueRange{Values: [][]interface{}{{hands}}},
	).ValueInputOption("RAW").Do(); err != nil {
		return fmt.Errorf("updating hands for tournament %s: %w", tournamentID, err)
	}

	if !snap.hasNetUSD {
		// Summary still hasn't arrived — nothing to reconcile yet.
		return nil
	}

	netUSD, prizeWon := reconcileTournamentBounty(snap.netUSD, snap.bountyUSD, bountyUSD, snap.buyInUSD)

	iRange := fmt.Sprintf("Tournaments!I%d", rowIndex)
	if _, err := c.svc.Spreadsheets.Values.Update(
		c.spreadsheetID, iRange,
		&sheetsapi.ValueRange{Values: [][]interface{}{{netUSD}}},
	).ValueInputOption("RAW").Do(); err != nil {
		return fmt.Errorf("reconciling net for tournament %s: %w", tournamentID, err)
	}

	mRange := fmt.Sprintf("Tournaments!M%d", rowIndex)
	if _, err := c.svc.Spreadsheets.Values.Update(
		c.spreadsheetID, mRange,
		&sheetsapi.ValueRange{Values: [][]interface{}{{prizeWon}}},
	).ValueInputOption("RAW").Do(); err != nil {
		return fmt.Errorf("updating prize won for tournament %s: %w", tournamentID, err)
	}

	return c.upsertLedgerTournamentRow(tournamentID, snap.date, netUSD)
}

// AppendLedgerEntry appends a deposit or withdrawal row to Bankroll Ledger.
// For withdrawals, the caller passes a negative amountUSD.
func (c *Client) AppendLedgerEntry(entryType string, amountUSD float64) error {
	prev, err := c.getLastRunningTotal()
	if err != nil {
		return err
	}
	return c.appendRow("Bankroll Ledger", []interface{}{
		time.Now().UTC().Format(dateFormat),
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
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		"Bankroll Ledger!A:E",
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
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
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		"Cash Sessions!A:J",
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
	if err != nil {
		return nil, fmt.Errorf("reading cash sessions: %w", err)
	}
	if len(resp.Values) <= 1 {
		return nil, nil
	}
	var result []storage.CashSessionRecord
	for _, row := range resp.Values[1:] {
		if len(row) < 9 {
			continue
		}
		date, err := time.Parse(dateFormat, fmt.Sprint(row[0]))
		if err != nil || date.Before(since) {
			continue
		}
		rec := storage.CashSessionRecord{
			CashSession: parsers.CashSession{
				Date:        date,
				Stake:       fmt.Sprint(row[1]),
				Hands:       parseInt(row[2]),
				DurationMin: parseInt(row[3]),
				NetUSD:      parseFloat(row[4]),
				RakeUSD:     parseFloat(row[5]),
				BB100:       parseFloat(row[6]),
				VPIP:        parseFloat(row[7]),
				PFR:         parseFloat(row[8]),
			},
		}
		if len(row) > 9 {
			rec.SessionID = fmt.Sprint(row[9])
		}
		result = append(result, rec)
	}
	return result, nil
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
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		"Tournaments!A:N",
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
	if err != nil {
		return nil, fmt.Errorf("reading tournaments: %w", err)
	}
	if len(resp.Values) <= 1 {
		return nil, nil
	}
	var result []storage.TournamentRecord
	for _, row := range resp.Values[1:] {
		if len(row) < 9 {
			continue
		}
		date, err := time.Parse(dateFormat, fmt.Sprint(row[0]))
		if err != nil || date.Before(since) {
			continue
		}
		rec := storage.TournamentRecord{
			Tournament: parsers.Tournament{
				Date:         date,
				Type:         fmt.Sprint(row[1]),
				TournamentID: fmt.Sprint(row[2]),
				BuyInUSD:     parseFloat(row[3]),
				FinishPos:    parseInt(row[6]),
				TotalPlayers: parseInt(row[7]),
				NetUSD:       parseFloat(row[8]),
			},
		}
		// Prize Pool (col E) holds a number for a real cash pool, or text
		// for a ticket-denominated one (e.g. "$1.00 Power Path Step 2
		// Ticket") — see AppendTournament's prizePool write.
		if desc, ok := row[4].(string); ok && desc != "" {
			rec.PrizePoolDescription = desc
		} else {
			rec.PrizePoolUSD = parseFloat(row[4])
		}
		// Multiplier (col F) is empty for MTT rows.
		if mult := fmt.Sprint(row[5]); mult != "" {
			m := parseFloat(row[5])
			rec.Multiplier = &m
		}
		if len(row) > 9 && fmt.Sprint(row[9]) != "" {
			v := parseFloat(row[9])
			rec.VPIP = &v
		}
		if len(row) > 10 && fmt.Sprint(row[10]) != "" {
			p := parseFloat(row[10])
			rec.PFR = &p
		}
		if len(row) > 11 {
			rec.BountyWonUSD = parseFloat(row[11])
		}
		if len(row) > 12 {
			rec.PrizeWonUSD = parseFloat(row[12])
		}
		if len(row) > 13 && fmt.Sprint(row[13]) != "" {
			h := parseInt(row[13])
			rec.Hands = &h
		}
		result = append(result, rec)
	}
	return result, nil
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
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		"Time Analysis!A:E",
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
	if err != nil {
		return nil, fmt.Errorf("reading time analysis: %w", err)
	}
	if len(resp.Values) <= 1 {
		return nil, nil
	}
	var result []storage.TimeAnalysisRecord
	for _, row := range resp.Values[1:] {
		if len(row) < 4 {
			continue
		}
		date, err := time.Parse(dateFormat, fmt.Sprint(row[0]))
		if err != nil {
			continue
		}
		rec := storage.TimeAnalysisRecord{
			Date:      date,
			Hour:      parseInt(row[1]),
			DayOfWeek: fmt.Sprint(row[2]),
			NetUSD:    parseFloat(row[3]),
		}
		if len(row) > 4 {
			rec.SessionID = fmt.Sprint(row[4])
		}
		result = append(result, rec)
	}
	return result, nil
}

func parseLedgerRows(rows [][]interface{}) ([]storage.LedgerEntry, error) {
	entries := make([]storage.LedgerEntry, 0, len(rows))
	for _, row := range rows {
		if len(row) < 4 {
			continue
		}
		date, err := time.Parse(dateFormat, fmt.Sprint(row[0]))
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
