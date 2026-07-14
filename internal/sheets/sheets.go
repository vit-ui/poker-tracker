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
		{"Tournaments", []interface{}{"Date", "Type", "Tournament ID", "Buy-In (USD)", "Prize Pool (USD)", "Multiplier", "Finish Position", "Total Players", "Net (USD)", "VPIP", "PFR"}},
		{"Bankroll Ledger", []interface{}{"Date", "Type", "Amount (USD)", "Running Total (USD)", "Session ID"}},
		{"Time Analysis", []interface{}{"Date", "Hour", "Day of Week", "Net (USD)", "Session ID"}},
	}

	for _, tab := range tabs {
		if existing[tab.name] {
			continue
		}
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
	}

	return nil
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

// AppendTournament upserts one row in Tournaments (keyed by TournamentID) and
// appends one row to Bankroll Ledger. If the row already exists (created by
// UpdateTournamentVPIPPFR), updates columns A–I and leaves VPIP/PFR untouched.
func (c *Client) AppendTournament(t parsers.Tournament) error {
	var multiplier interface{}
	if t.Multiplier != nil {
		multiplier = *t.Multiplier
	} else {
		multiplier = ""
	}

	rowIndex, found, err := c.findTournamentRow(t.TournamentID)
	if err != nil {
		return err
	}

	if found {
		// Row was pre-created by UpdateTournamentVPIPPFR — update main columns
		// only, leaving VPIP/PFR (J:K) untouched.
		rangeStr := fmt.Sprintf("Tournaments!A%d:I%d", rowIndex, rowIndex)
		_, err := c.svc.Spreadsheets.Values.Update(
			c.spreadsheetID, rangeStr,
			&sheetsapi.ValueRange{Values: [][]interface{}{{
				t.Date.Format(dateFormat),
				t.Type,
				t.TournamentID,
				t.BuyInUSD,
				t.PrizePoolUSD,
				multiplier,
				t.FinishPos,
				t.TotalPlayers,
				t.NetUSD,
			}}},
		).ValueInputOption("RAW").Do()
		if err != nil {
			return fmt.Errorf("updating tournament row: %w", err)
		}
	} else {
		if err := c.appendRow("Tournaments", []interface{}{
			t.Date.Format(dateFormat),
			t.Type,
			t.TournamentID,
			t.BuyInUSD,
			t.PrizePoolUSD,
			multiplier,
			t.FinishPos,
			t.TotalPlayers,
			t.NetUSD,
			"", // VPIP — filled later by UpdateTournamentVPIPPFR
			"", // PFR  — filled later by UpdateTournamentVPIPPFR
		}); err != nil {
			return err
		}
	}

	prev, err := c.getLastRunningTotal()
	if err != nil {
		return err
	}
	return c.appendRow("Bankroll Ledger", []interface{}{
		t.Date.Format(dateFormat),
		"tournament",
		t.NetUSD,
		prev + t.NetUSD,
	})
}

// UpdateTournamentVPIPPFR writes VPIP and PFR for a tournament identified by
// tournamentID. If the row exists (created by AppendTournament), updates J:K.
// If not, appends a partial row so the stats are available when the summary
// file arrives later and AppendTournament completes the row.
func (c *Client) UpdateTournamentVPIPPFR(tournamentID string, vpip, pfr float64) error {
	rowIndex, found, err := c.findTournamentRow(tournamentID)
	if err != nil {
		return err
	}
	if found {
		rangeStr := fmt.Sprintf("Tournaments!J%d:K%d", rowIndex, rowIndex)
		_, err := c.svc.Spreadsheets.Values.Update(
			c.spreadsheetID, rangeStr,
			&sheetsapi.ValueRange{Values: [][]interface{}{{vpip, pfr}}},
		).ValueInputOption("RAW").Do()
		if err != nil {
			return fmt.Errorf("updating VPIP/PFR for tournament %s: %w", tournamentID, err)
		}
		return nil
	}
	// Summary not yet processed — create a partial row; AppendTournament will
	// fill in the remaining columns when the summary file arrives.
	return c.appendRow("Tournaments", []interface{}{
		"", // Date
		"", // Type
		tournamentID,
		"", // Buy-In
		"", // Prize Pool
		"", // Multiplier
		"", // Finish Position
		"", // Total Players
		"", // Net
		vpip,
		pfr,
	})
}

// AppendLedgerEntry appends a deposit or withdrawal row to Bankroll Ledger.
// For withdrawals, the caller passes a negative amountUSD.
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
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		"Bankroll Ledger!A:D",
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

// GetCashSessionsSince returns all Cash Sessions rows where Date >= t.
// BuyInUSD is not stored in the sheet and will be zero in returned structs.
func (c *Client) GetCashSessionsSince(t time.Time) ([]parsers.CashSession, error) {
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		"Cash Sessions!A:I",
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
	if err != nil {
		return nil, fmt.Errorf("reading cash sessions: %w", err)
	}
	if len(resp.Values) <= 1 {
		return nil, nil
	}
	var result []parsers.CashSession
	for _, row := range resp.Values[1:] {
		if len(row) < 9 {
			continue
		}
		date, err := time.Parse(dateFormat, fmt.Sprint(row[0]))
		if err != nil || date.Before(t) {
			continue
		}
		result = append(result, parsers.CashSession{
			Date:        date,
			Stake:       fmt.Sprint(row[1]),
			Hands:       parseInt(row[2]),
			DurationMin: parseInt(row[3]),
			NetUSD:      parseFloat(row[4]),
			RakeUSD:     parseFloat(row[5]),
			BB100:       parseFloat(row[6]),
			VPIP:        parseFloat(row[7]),
			PFR:         parseFloat(row[8]),
		})
	}
	return result, nil
}

// GetTournamentsSince returns all Tournaments rows where Date >= since.
// Partial rows (no Date) created by UpdateTournamentVPIPPFR are skipped.
func (c *Client) GetTournamentsSince(since time.Time) ([]parsers.Tournament, error) {
	resp, err := c.svc.Spreadsheets.Values.Get(
		c.spreadsheetID,
		"Tournaments!A:K",
	).ValueRenderOption("UNFORMATTED_VALUE").Do()
	if err != nil {
		return nil, fmt.Errorf("reading tournaments: %w", err)
	}
	if len(resp.Values) <= 1 {
		return nil, nil
	}
	var result []parsers.Tournament
	for _, row := range resp.Values[1:] {
		if len(row) < 9 {
			continue
		}
		date, err := time.Parse(dateFormat, fmt.Sprint(row[0]))
		if err != nil || date.Before(since) {
			continue
		}
		tourn := parsers.Tournament{
			Date:         date,
			Type:         fmt.Sprint(row[1]),
			TournamentID: fmt.Sprint(row[2]),
			BuyInUSD:     parseFloat(row[3]),
			PrizePoolUSD: parseFloat(row[4]),
			FinishPos:    parseInt(row[6]),
			TotalPlayers: parseInt(row[7]),
			NetUSD:       parseFloat(row[8]),
		}
		// Multiplier (col F) is empty for MTT rows.
		if mult := fmt.Sprint(row[5]); mult != "" {
			m := parseFloat(row[5])
			tourn.Multiplier = &m
		}
		result = append(result, tourn)
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
		entries = append(entries, storage.LedgerEntry{
			Date:         date,
			Type:         fmt.Sprint(row[1]),
			AmountUSD:    parseFloat(row[2]),
			RunningTotal: parseFloat(row[3]),
		})
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
