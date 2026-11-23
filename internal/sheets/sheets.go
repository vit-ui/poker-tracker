// Package sheets manages the Google Sheets integration: tab setup, row appends,
// and reads used by the bot and hardstop evaluators.
package sheets

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
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
	if number, ok := v.(json.Number); ok {
		serial, err := number.Float64()
		if err != nil {
			return time.Time{}, err
		}
		v = serial
	}
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
	// mutationMu covers complete read/write/replay sequences, not individual HTTP calls.
	mutationMu    sync.Mutex
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

type tabDef struct {
	name    string
	headers []interface{}
}

func historyTabs() [3]tabDef {
	return [3]tabDef{
		{"Cash Sessions", []interface{}{"Date", "Stake", "Hands", "Duration (min)", "Net (USD)", "Rake (USD)", "BB/100", "VPIP", "PFR", "Session ID", "Hand Details"}},
		{"Tournaments", []interface{}{"Date", "Type", "Tournament ID", "Buy-In (USD)", "Prize Pool (USD)", "Multiplier", "Finish Position", "Total Players", "Net (USD)", "VPIP", "PFR", "Bounty Won (USD)", "Prize Won (USD)", "Hands", "Result Source", "Result Hand ID"}},
		{"Bankroll Ledger", []interface{}{"Date", "Type", "Amount (USD)", "Running Total (USD)", "Session ID"}},
	}
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

	for _, tab := range historyTabs() {
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
func (c *Client) getLastRunningTotal() (money.Cents, error) {
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
	return parseMoney(last[0])
}

// CashSessionID retains the historical Sheets fingerprint for fallback keys
// and legacy callers. Stored rows recognize both historical identity formats.
func CashSessionID(date time.Time, firstHandID string) string {
	return storage.CashSessionIdentities(date, firstHandID)[1]
}

// findRowByColumnValue searches column columnLetter of tab for a row whose
// value equals want, starting after the header row. Returns the 1-based
// sheet row index, or found=false if not present. Shared by every tab that
// upserts on a stored ID column (Cash Sessions, Bankroll Ledger's
// cash_session rows, and — via findTournamentRow — Tournaments).
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
	return max(2, len(resp.Values)+1), false, nil
}

// AppendCashSession upserts one row in Cash Sessions and one row in Bankroll
// Ledger — both keyed by the same CashSessionID, so a re-processed cash
// session corrects its existing rows in place instead of appending a new set.
// Mirrors AppendTournament's find-or-append pattern, applied to both tabs a
// cash session touches rather than just one.
func (c *Client) rawAppendCashSession(session parsers.CashSession) error {
	keys := storage.CashSessionIdentities(session.Date, session.FirstHandID)
	id := keys[0]
	response, err := c.svc.valuesGet(c.spreadsheetID, "Cash Sessions!J:J")
	if err != nil {
		return fmt.Errorf("resolving cash identity: %w", err)
	}
	rowIndex := 0
	for i, row := range response.Values {
		if i == 0 || len(row) == 0 {
			continue
		}
		value := fmt.Sprint(row[0])
		if value == keys[0] || value == keys[1] {
			if rowIndex != 0 {
				return fmt.Errorf("cash session has conflicting legacy identities")
			}
			rowIndex = i + 1
			id = value
		}
	}
	if rowIndex == 0 {
		rowIndex = max(2, len(response.Values)+1)
	}
	values := buildCashRows([]storage.CashSessionRecord{{CashSession: session, SessionID: id}})
	writes := []gridWrite{{cashHistoryTab, rowIndex - 1, 0, values}}
	return c.writeResults(writes, []storage.LedgerEntry{{Date: session.Date, Type: "cash_session", AmountCents: session.NetCents, SessionID: id}})
}

func (c *Client) findTournamentRow(tournamentID string) (rowIndex int, found bool, err error) {
	return c.findRowByColumnValue("Tournaments", "C", tournamentID)
}

// tournamentRowSnapshot holds the subset of an existing Tournaments row
// needed to reconcile NetCents/Type against knockout-bounty data, which is
// only ever known from the hand history file (UpdateTournamentHandStats) and
// never from the summary file AppendTournament parses its other fields from.
type tournamentRowSnapshot struct {
	date        time.Time
	typeVal     string
	buyInCents  money.Cents
	hasNetUSD   bool // false while the row is still a hand-file-only partial row
	netCents    money.Cents
	bountyCents money.Cents
	hands       int
}

// readTournamentRow reads the fields of an existing Tournaments row (by
// 1-based sheet rowIndex) needed to compute a bounty-reconciled NetCents/Type
// from either side of the two-file correlation.
func (c *Client) readTournamentRow(rowIndex int) (tournamentRowSnapshot, error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, fmt.Sprintf("Tournaments!A%d:N%d", rowIndex, rowIndex))
	if err != nil {
		return tournamentRowSnapshot{}, fmt.Errorf("reading tournament row %d: %w", rowIndex, err)
	}
	if len(resp.Values) == 0 {
		return tournamentRowSnapshot{}, nil
	}
	row := resp.Values[0]
	var snap tournamentRowSnapshot
	snap.hands = parseInt(cell(row, 13))
	if len(row) > 0 {
		if date, err := parseSheetDate(row[0]); err == nil {
			snap.date = date
		}
	}
	if len(row) > 1 {
		snap.typeVal = fmt.Sprint(row[1])
	}
	if len(row) > 3 {
		snap.buyInCents, err = parseMoney(row[3])
		if err != nil {
			return tournamentRowSnapshot{}, err
		}
	}
	if len(row) > 8 && fmt.Sprint(row[8]) != "" {
		snap.hasNetUSD = true
		snap.netCents, err = parseMoney(row[8])
		if err != nil {
			return tournamentRowSnapshot{}, err
		}
	}
	if len(row) > 11 {
		snap.bountyCents, err = parseMoney(row[11])
		if err != nil {
			return tournamentRowSnapshot{}, err
		}
	}
	return snap, nil
}

// rawAppendTournament writes the completed result and its ledger correction
// in one Sheets batch, preserving the existing hand-stat cells.
func (c *Client) rawAppendTournament(t parsers.Tournament) (money.Cents, error) {
	row, found, err := c.findTournamentRow(t.TournamentID)
	if err != nil {
		return 0, err
	}
	var snap tournamentRowSnapshot
	if found {
		snap, err = c.readTournamentRow(row)
		if err != nil {
			return 0, err
		}
	}
	t.Type = storage.ReconcileKnockoutType(t.Type, snap.typeVal)
	netCents, prizeWonCents, err := storage.FoldBountyIntoNet(t.NetCents, snap.bountyCents, t.BuyInCents)
	if err != nil {
		return 0, err
	}
	t.NetCents = netCents
	values := tournamentResultCells(t)
	writes := []gridWrite{
		{"Tournaments", row - 1, 0, [][]interface{}{values}},
		{"Tournaments", row - 1, 12, [][]interface{}{{prizeWonCents}}},
		{"Tournaments", row - 1, 14, [][]interface{}{{t.ResultSource, t.ResultHandID}}},
	}
	err = c.writeResults(writes, []storage.LedgerEntry{{Date: t.Date, Type: "tournament", AmountCents: netCents, SessionID: t.TournamentID}})
	return netCents, err
}

// rawUpdateTournamentHandStats creates or updates hand-history data. For
// completed results a bounty correction and its ledger cascade commit together.
func (c *Client) rawUpdateTournamentHandStats(tournamentID string, vpip, pfr float64, hands int, bountyCents money.Cents, isKnockout, preserveBounty bool) error {
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
			bountyCents,
			"", // Prize Won (USD) — filled in once AppendTournament knows BuyInCents
			hands,
		})
	}

	snap, err := c.readTournamentRow(rowIndex)
	if err != nil {
		return err
	}

	if preserveBounty {
		bountyCents = max(bountyCents, snap.bountyCents)
	}

	typeOut := snap.typeVal
	if isKnockout {
		typeOut = storage.AppendKnockoutSuffix(snap.typeVal)
	}
	writes := []gridWrite{{"Tournaments", rowIndex - 1, 11, [][]interface{}{{bountyCents}}}}
	if storage.UseTournamentSample(preserveBounty, hands, snap.hands) {
		writes = append(writes, gridWrite{"Tournaments", rowIndex - 1, 9, [][]interface{}{{vpip, pfr}}}, gridWrite{"Tournaments", rowIndex - 1, 13, [][]interface{}{{hands}}})
	}
	if typeOut != snap.typeVal {
		writes = append(writes, gridWrite{"Tournaments", rowIndex - 1, 1, [][]interface{}{{typeOut}}})
	}
	if !snap.hasNetUSD {
		data := make([]valueRange, 0, len(writes))
		for _, write := range writes {
			data = append(data, valueRange{
				Range:  fmt.Sprintf("Tournaments!%s%d:%s%d", columnLetter(write.column), rowIndex, columnLetter(write.column+len(write.values[0])-1), rowIndex),
				Values: write.values,
			})
		}
		return c.svc.valuesBatchUpdate(c.spreadsheetID, data)
	}
	netCents, prizeWonCents, err := storage.ReconcileTournamentBounty(snap.netCents, snap.bountyCents, bountyCents, snap.buyInCents)
	if err != nil {
		return err
	}
	writes = append(writes, gridWrite{"Tournaments", rowIndex - 1, 8, [][]interface{}{{netCents}}}, gridWrite{"Tournaments", rowIndex - 1, 12, [][]interface{}{{prizeWonCents}}})
	return c.writeResults(writes, []storage.LedgerEntry{{Date: snap.date, Type: "tournament", AmountCents: netCents, SessionID: tournamentID}})
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
	fallbackKindLedger  = "ledger"
)

// fallbackEntry is one queued write awaiting replay, holding exactly the
// arguments its raw* method needs. Kind selects which of the three payloads is
// populated; the map key is "<kind>:<id>" so a tournament's summary write and
// its hand-stats write (both keyed by TournamentID) never collide.
type fallbackEntry struct {
	QueuedAt  time.Time            `json:"queued_at,omitempty"`
	Kind      string               `json:"kind"`
	Cash      *parsers.CashSession `json:"cash,omitempty"`
	CashHands *cashHandWrite       `json:"cash_hands,omitempty"`
	Summary   *parsers.Tournament  `json:"summary,omitempty"`
	HandStats *handStatsArgs       `json:"hand_stats,omitempty"`
	Ledger    *ledgerArgs          `json:"ledger,omitempty"`
}

// handStatsArgs mirrors rawUpdateTournamentHandStats's parameter list so a
// failed hand-stats write can be replayed verbatim.
type handStatsArgs struct {
	TournamentID   string      `json:"tournament_id"`
	VPIP           float64     `json:"vpip"`
	PFR            float64     `json:"pfr"`
	Hands          int         `json:"hands"`
	BountyCents    money.Cents `json:"bounty_cents"`
	IsKnockout     bool        `json:"is_knockout"`
	PreserveBounty bool        `json:"preserve_bounty,omitempty"`
}

// ledgerArgs holds a failed deposit/withdrawal for replay. The Running Total is
// deliberately *not* stored — rawAppendLedgerEntry recomputes it from the
// sheet's current last row at replay time, so a deposit that lands after other
// rows queued ahead of it still gets a correct cumulative total.
type ledgerArgs struct {
	OperationID string      `json:"operation_id,omitempty"`
	EntryType   string      `json:"entry_type"`
	AmountCents money.Cents `json:"amount_cents"`
	Now         time.Time   `json:"now"`
}

// AppendCashSession writes the session and, only if that fails, caches it for
// later replay; a success proves the sheet is reachable and flushes anything
// queued. The fallback wrappers below all follow this shape — the raw* method
// does the Sheets I/O and knows nothing about the fallback, so flush (which
// calls raw* directly) can never recurse back through here.
func (c *Client) AppendCashSession(session parsers.CashSession) error {
	if err := storage.ValidateCashSession(session); err != nil {
		return err
	}
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	key := fallbackKindCash + ":" + CashSessionID(session.Date, session.FirstHandID)
	entry := fallbackEntry{Kind: fallbackKindCash, Cash: &session}
	if err := c.prepareQueuedReplacement(key, entry); err != nil {
		return err
	}
	if err := c.rawAppendCashSession(session); err != nil {
		return c.queueWriteFailure(key, entry, err)
	}
	if err := c.retireFallback(key); err != nil {
		return err
	}
	c.flush()
	return nil
}

// DiscardTournamentRecovery drops a queued hand-history result superseded
// by resumed play or an official summary. An official result keeps its queue.
func (c *Client) DiscardTournamentRecovery(tournamentID string) error {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	key := fallbackKindSummary + ":" + tournamentID
	c.fallbackMu.Lock()
	entry := c.fallback[key]
	c.fallbackMu.Unlock()
	if entry.Summary != nil && entry.Summary.Recovered() {
		return c.retireFallback(key)
	}
	return nil
}

func (c *Client) AppendTournament(t parsers.Tournament) (money.Cents, error) {
	if err := storage.ValidateTournament(t); err != nil {
		return 0, err
	}
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	key := fallbackKindSummary + ":" + t.TournamentID
	if t.Recovered() {
		c.fallbackMu.Lock()
		pending := c.fallback[key].Summary
		c.fallbackMu.Unlock()
		if pending != nil && !pending.Recovered() {
			return 0, fmt.Errorf("official tournament result %s is queued", t.TournamentID)
		}
	}
	entry := fallbackEntry{Kind: fallbackKindSummary, Summary: &t}
	if err := c.prepareQueuedReplacement(key, entry); err != nil {
		return 0, err
	}
	netCents, err := c.rawAppendTournament(t)
	if err != nil {
		return netCents, c.queueWriteFailure(key, entry, err)
	}
	if err := c.retireFallback(key); err != nil {
		return netCents, err
	}
	c.flush()
	return netCents, nil
}

func (c *Client) UpdateTournamentHandStats(id string, vpip, pfr float64, hands int, bountyCents money.Cents, knockout bool) error {
	return c.writeTournamentHandStats(id, vpip, pfr, hands, bountyCents, knockout, false)
}

func (c *Client) ScanTournamentHandStats(stats parsers.TournamentHandStats) error {
	return c.writeTournamentHandStats(stats.TournamentID, stats.VPIP, stats.PFR, stats.Hands, stats.BountyCents, stats.IsKnockout, true)
}

func (c *Client) writeTournamentHandStats(id string, vpip, pfr float64, hands int, bountyCents money.Cents, knockout, preserveBounty bool) error {
	if err := storage.ValidateNumbers(vpip, pfr); err != nil {
		return err
	}
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	key := fallbackKindStats + ":" + id
	stats := handStatsArgs{TournamentID: id, VPIP: vpip, PFR: pfr, Hands: hands, BountyCents: bountyCents, IsKnockout: knockout, PreserveBounty: preserveBounty}
	if preserveBounty {
		c.fallbackMu.Lock()
		pending := c.fallback[key].HandStats
		c.fallbackMu.Unlock()
		if pending != nil {
			if !storage.UseTournamentSample(true, hands, pending.Hands) {
				stats.Hands, stats.VPIP, stats.PFR = pending.Hands, pending.VPIP, pending.PFR
			}
			stats.BountyCents = max(stats.BountyCents, pending.BountyCents)
			stats.IsKnockout = stats.IsKnockout || pending.IsKnockout
			stats.PreserveBounty = pending.PreserveBounty
		}
	}
	entry := fallbackEntry{Kind: fallbackKindStats, HandStats: &stats}
	if err := c.prepareQueuedReplacement(key, entry); err != nil {
		return err
	}
	if err := c.applyHandStats(stats); err != nil {
		return c.queueWriteFailure(key, entry, err)
	}
	if err := c.retireFallback(key); err != nil {
		return err
	}
	c.flush()
	return nil
}

func (c *Client) applyHandStats(stats handStatsArgs) error {
	return c.rawUpdateTournamentHandStats(stats.TournamentID, stats.VPIP, stats.PFR, stats.Hands,
		stats.BountyCents, stats.IsKnockout, stats.PreserveBounty)
}

// If an older snapshot is queued, persist its replacement before attempting a
// newer remote correction. The queue remains pending until success; if retiring
// it later fails, restart can only replay the newer snapshot, never the old one.
func (c *Client) prepareQueuedReplacement(key string, entry fallbackEntry) error {
	c.fallbackMu.Lock()
	_, exists := c.fallback[key]
	c.fallbackMu.Unlock()
	if !exists {
		return nil
	}
	if err := c.cacheFailed(key, entry); err != nil {
		return &storage.WriteError{Err: err}
	}
	return nil
}

func (c *Client) queueWriteFailure(key string, entry fallbackEntry, remoteErr error) error {
	if errors.Is(remoteErr, money.ErrOverflow) {
		return remoteErr
	}
	err := c.cacheFailed(key, entry)
	return &storage.WriteError{Err: errors.Join(remoteErr, err), Queued: err == nil}
}

func (c *Client) retireFallback(key string) error {
	c.fallbackMu.Lock()
	defer c.fallbackMu.Unlock()
	entry, exists := c.fallback[key]
	if !exists {
		return nil
	}
	delete(c.fallback, key)
	if err := c.persistFallbackLocked(); err != nil {
		c.fallback[key] = entry
		return &storage.WriteError{Err: err, Queued: true}
	}
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
func (c *Client) cacheFailed(key string, entry fallbackEntry) error {
	if c.fallbackPath == "" {
		return fmt.Errorf("local fallback is disabled")
	}
	entry.QueuedAt = time.Now()
	c.fallbackMu.Lock()
	if previous, exists := c.fallback[key]; exists && entry.CashHands != nil {
		entry.QueuedAt = previous.QueuedAt
	}
	c.fallback[key] = entry
	err := c.persistFallbackLocked()
	c.fallbackMu.Unlock()
	// Log only after releasing the queue mutex.
	if err != nil {
		slog.Error("persisting sheets fallback", "err", err)
	}
	return err
}

// flush replays every queued entry against the sheet, deleting each that
// resolves (replayed successfully, or discarded as superseded). Holds
// fallbackMu across the whole loop — for a single-user tool, serializing the
// fallback accounting. Public mutations hold mutationMu across the original
// operation and this replay; startup calls it before publishing the client.
// A no-op when disabled or empty.
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
	removed := make(map[string]fallbackEntry)
	keys := make([]string, 0, len(c.fallback))
	for key := range c.fallback {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := c.fallback[keys[i]].QueuedAt, c.fallback[keys[j]].QueuedAt
		if left.Equal(right) {
			return keys[i] < keys[j]
		}
		return left.Before(right)
	})
	cashBlocked := false
	for _, key := range keys {
		entry := c.fallback[key]
		isCash := entry.Cash != nil || entry.CashHands != nil
		if cashBlocked && isCash {
			continue
		}
		if c.resolveFallbackEntry(entry) {
			removed[key] = entry
			delete(c.fallback, key)
			changed = true
		} else if isCash {
			cashBlocked = true
		}
	}
	var persistErr error
	if changed {
		persistErr = c.persistFallbackLocked()
		if persistErr != nil {
			for key, entry := range removed {
				c.fallback[key] = entry
			}
		}
	}
	c.fallbackMu.Unlock()
	// Log after releasing the queue mutex.
	if persistErr != nil {
		slog.Error("persisting sheets fallback after flush", "err", persistErr)
	}
}

// resolveFallbackEntry attempts one entry and reports whether it's resolved
// (should be deleted). false means keep it queued — a sheet read failed, or the
// replay itself failed transiently — to retry after the next successful write.
func (c *Client) resolveFallbackEntry(entry fallbackEntry) bool {
	switch entry.Kind {
	case "cash_hands":
		_, err := c.rawRecordCashHands(*entry.CashHands)
		return err == nil
	case fallbackKindCash:
		sheetHands, exists, err := c.cashSheetHandCount(CashSessionID(entry.Cash.Date, entry.Cash.FirstHandID))
		if err != nil {
			return false
		}
		if flushDecision(entry.Cash.Hands, sheetHands, exists) == flushDiscard {
			return true
		}
		return c.rawAppendCashSession(*entry.Cash) == nil
	case fallbackKindStats:
		stats := *entry.HandStats
		if !stats.PreserveBounty {
			sheetHands, exists, err := c.tournamentSheetHandCount(stats.TournamentID)
			if err != nil {
				return false
			}
			if flushDecision(stats.Hands, sheetHands, exists) == flushDiscard {
				return true
			}
		}
		return c.applyHandStats(stats) == nil
	case fallbackKindSummary:
		_, err := c.rawAppendTournament(*entry.Summary)
		return err == nil
	case fallbackKindLedger:
		// No supersede guard: a deposit/withdrawal is a pure append whose
		// running total rawAppendLedgerEntry recomputes fresh, so there's no
		// stale snapshot to protect against — just replay it.
		return c.rawAppendLedgerEntry(entry.Ledger.EntryType, entry.Ledger.AmountCents, entry.Ledger.Now, entry.Ledger.OperationID) == nil
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
// For withdrawals, the caller passes a negative amountCents. The date is
// written as the local wall clock, not UTC — session rows' dates come from
// PokerStars' zone-less local timestamps, so writing a true-UTC time here
// would put deposits in a date coordinate system shifted from every session
// row's by the machine's UTC offset, and window queries would misplace them
// near day boundaries.
// Identified movements reconcile their existing ledger ID before replay, including
// a remotely committed append whose acknowledgement was lost. Legacy blank IDs
// retain append semantics and cannot provide exactly-once recovery.
func (c *Client) AppendLedgerEntry(entryType string, amountCents money.Cents, now time.Time, operationID string) error {
	if err := storage.ValidateMoneyOperationID(operationID); err != nil {
		return err
	}
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	if err := c.rawAppendLedgerEntry(entryType, amountCents, now, operationID); err != nil {
		if errors.Is(err, money.ErrOverflow) {
			return err
		}
		key := fmt.Sprintf("%s:%d", fallbackKindLedger, now.UnixNano())
		if operationID != "" {
			key = fallbackKindLedger + ":" + operationID
		}
		saveErr := c.cacheFailed(key, fallbackEntry{Kind: fallbackKindLedger, Ledger: &ledgerArgs{
			EntryType: entryType, AmountCents: amountCents, Now: now, OperationID: operationID,
		}})
		return &storage.WriteError{Err: errors.Join(err, saveErr), Queued: saveErr == nil}
	}
	c.flush()
	return nil
}

func (c *Client) rawAppendLedgerEntry(entryType string, amountCents money.Cents, now time.Time, operationID string) error {
	if operationID != "" {
		_, found, err := c.findRowByColumnValue("Bankroll Ledger", "E", operationID)
		if err != nil {
			return fmt.Errorf("checking money operation identity: %w", err)
		}
		if found {
			return nil
		}
	}

	prevCents, err := c.getLastRunningTotal()
	if err != nil {
		return err
	}
	totalCents, err := money.Add(prevCents, amountCents)
	if err != nil {
		return err
	}
	return c.appendRow("Bankroll Ledger", []interface{}{
		now.Format(dateFormat),
		entryType,
		amountCents,
		totalCents,
		operationID,
	})
}

// GetCurrentBankroll returns the running total from the last Bankroll Ledger row.
// Returns 0 if the ledger is empty.
func (c *Client) GetCurrentBankroll() (money.Cents, error) {
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
	return readCashValues(resp.Values, since)
}

func readCashValues(values [][]interface{}, since time.Time) ([]storage.CashSessionRecord, error) {
	if len(values) <= 1 {
		return nil, nil
	}
	var result []storage.CashSessionRecord
	for _, row := range values[1:] {
		rec, ok, err := parseCashRow(row)
		if err != nil {
			return nil, err
		}
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
func parseCashRow(row []interface{}) (rec storage.CashSessionRecord, ok bool, err error) {
	if len(row) < 9 {
		return storage.CashSessionRecord{}, false, nil
	}
	date, err := parseSheetDate(cell(row, 0))
	if err != nil {
		return storage.CashSessionRecord{}, false, nil
	}
	netCents, err := parseMoney(cell(row, 4))
	if err != nil {
		return rec, false, err
	}
	rakeCents, err := parseMoney(cell(row, 5))
	if err != nil {
		return rec, false, err
	}
	rec = storage.CashSessionRecord{
		CashSession: parsers.CashSession{
			Date:        date,
			Stake:       fmt.Sprint(cell(row, 1)),
			Hands:       parseInt(cell(row, 2)),
			DurationMin: parseInt(cell(row, 3)),
			NetCents:    netCents,
			RakeCents:   rakeCents,
			BB100:       parseFloat(cell(row, 6)),
			VPIP:        parseFloat(cell(row, 7)),
			PFR:         parseFloat(cell(row, 8)),
		},
		SessionID: fmt.Sprint(cell(row, 9)),
	}
	rec.HandDetails, err = decodeCashMetadata(row)
	return rec, err == nil, err
}

// GetCashSessionsSince returns all Cash Sessions rows where Date >= t.
// BuyInCents is not stored in the sheet and will be zero in returned structs.
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
	resp, err := c.svc.valuesGet(c.spreadsheetID, "Tournaments!A:P")
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
		rec, hasDate, err := parseTournamentRow(row)
		if err != nil {
			return nil, err
		}
		if !hasDate || rec.Date.Before(since) {
			continue
		}
		result = append(result, rec)
	}
	return result, nil
}

// parseTournamentRow parses one Tournaments row (columns A:P) into a record.
// hasDate is false for a partial row — a hand file parsed before its summary
// arrived, whose Date cell (column A) is blank — which callers reading only
// completed tournaments skip, but the migration snapshot keeps (with a zero
// Date). Shared by queryTournaments, allTournamentRows, and the migration
// round-trip so every path decodes a row identically.
func parseTournamentRow(row []interface{}) (rec storage.TournamentRecord, hasDate bool, err error) {
	rec.Type = fmt.Sprint(cell(row, 1))
	rec.TournamentID = fmt.Sprint(cell(row, 2))
	rec.ResultSource = fmt.Sprint(cell(row, 14))
	rec.ResultHandID = fmt.Sprint(cell(row, 15))
	if date, err := parseSheetDate(cell(row, 0)); err == nil {
		rec.Date = date
		hasDate = true
	}
	rec.BuyInCents, err = parseMoney(cell(row, 3))
	if err != nil {
		return rec, false, err
	}
	// Prize Pool (col E) holds a number for a real cash pool, or text for a
	// ticket-denominated one (e.g. "$1.00 Power Path Step 2 Ticket") — see
	// AppendTournament's prizePool write.
	if desc, ok := cell(row, 4).(string); ok && desc != "" {
		rec.PrizePoolDescription = desc
	} else {
		rec.PrizePoolCents, err = parseMoney(cell(row, 4))
		if err != nil {
			return rec, false, err
		}
	}
	// Multiplier (col F) is empty for MTT rows.
	if mult := fmt.Sprint(cell(row, 5)); mult != "" {
		m := parseFloat(cell(row, 5))
		rec.Multiplier = &m
	}
	rec.FinishPos = parseInt(cell(row, 6))
	rec.TotalPlayers = parseInt(cell(row, 7))
	rec.NetCents, err = parseMoney(cell(row, 8))
	if err != nil {
		return rec, false, err
	}
	if v := fmt.Sprint(cell(row, 9)); v != "" {
		f := parseFloat(cell(row, 9))
		rec.VPIP = &f
	}
	if p := fmt.Sprint(cell(row, 10)); p != "" {
		f := parseFloat(cell(row, 10))
		rec.PFR = &f
	}
	rec.BountyWonCents, err = parseMoney(cell(row, 11))
	if err != nil {
		return rec, false, err
	}
	rec.PrizeWonCents, err = parseMoney(cell(row, 12))
	if err != nil {
		return rec, false, err
	}
	if h := fmt.Sprint(cell(row, 13)); h != "" {
		n := parseInt(cell(row, 13))
		rec.Hands = &n
	}
	return rec, hasDate, nil
}

// allTournamentRows reads every Tournaments row including partial ones (blank
// Date), for the migration snapshot. queryTournaments can't be reused: it
// drops partials, which a migration must carry so an in-progress tournament
// keeps its hand-file stats across a backend switch.
func (c *Client) allTournamentRows() ([]storage.TournamentRecord, error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, "Tournaments!A:P")
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
		rec, _, err := parseTournamentRow(row)
		if err != nil {
			return nil, err
		}
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
	resp, err := c.svc.valuesGet(c.spreadsheetID, fmt.Sprintf("Tournaments!A%d:P%d", rowIndex, rowIndex))
	if err != nil {
		return parsers.Tournament{}, false, fmt.Errorf("reading tournament row %d: %w", rowIndex, err)
	}
	if len(resp.Values) == 0 {
		return parsers.Tournament{}, false, nil
	}
	rec, hasDate, err := parseTournamentRow(resp.Values[0])
	if err != nil {
		return parsers.Tournament{}, false, err
	}
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

// ExportSnapshot returns every row of all three tabs as full records, for a
// backend migration. Unlike GetAllTournaments its Tournaments slice keeps
// partial rows (blank Date), read via allTournamentRows.
func (c *Client) ExportSnapshot() (storage.Snapshot, error) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	values, err := c.cashValues()
	if err != nil {
		return storage.Snapshot{}, err
	}
	cash, err := readCashValues(values, time.Time{})
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
	return storage.Snapshot{
		CashSessions: cash,
		Tournaments:  tournaments,
		Ledger:       ledger,
	}, nil
}

// ImportSnapshot writes raw storage rows to the verified empty destination.
// The cash metadata and ledger enter the destination in the same batch.
func (c *Client) ImportSnapshot(snap storage.Snapshot) error {
	if err := storage.ValidateSnapshot(snap); err != nil {
		return err
	}
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	var writes []gridWrite
	for _, tab := range []struct {
		name string
		rows [][]interface{}
	}{
		{cashHistoryTab, buildCashRows(snap.CashSessions)},
		{"Tournaments", buildTournamentRows(snap.Tournaments)},
		{"Bankroll Ledger", buildLedgerRows(snap.Ledger)},
	} {
		if len(tab.rows) > 0 {
			writes = append(writes, gridWrite{tab.name, 1, 0, tab.rows})
		}
	}
	if len(writes) == 0 {
		return nil
	}
	return c.svc.writeHistoryGrid(c.spreadsheetID, writes)
}

// buildCashRows / buildTournamentRows / buildLedgerRows map records to the
// cell layout each tab's parse* counterpart reads back. They're pure (no
// Client, no I/O) so a migration's column alignment is unit-tested by
// round-tripping build → parse without a live sheet.
func buildCashRows(sessions []storage.CashSessionRecord) [][]interface{} {
	rows := make([][]interface{}, len(sessions))
	for i, s := range sessions {
		rows[i] = []interface{}{
			s.Date.UTC().Format(dateFormat), s.Stake, s.Hands, s.DurationMin,
			s.NetCents, s.RakeCents, s.BB100, s.VPIP, s.PFR, s.SessionID,
		}
		rows[i] = append(rows[i], encodeCashMetadata(s.HandDetails)...)
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
				floatCell(t.VPIP), floatCell(t.PFR), t.BountyWonCents, "", intCell(t.Hands),
			}
			continue
		}
		rows[i] = append(tournamentResultCells(t.Tournament),
			floatCell(t.VPIP), floatCell(t.PFR), t.BountyWonCents, t.PrizeWonCents, intCell(t.Hands), t.ResultSource, t.ResultHandID,
		)
	}
	return rows
}

func tournamentResultCells(t parsers.Tournament) []interface{} {
	var prizePool interface{} = t.PrizePoolCents
	if t.PrizePoolDescription != "" {
		prizePool = t.PrizePoolDescription
	}
	return []interface{}{
		t.Date.UTC().Format(dateFormat), t.Type, t.TournamentID, t.BuyInCents, prizePool, floatCell(t.Multiplier),
		t.FinishPos, t.TotalPlayers, t.NetCents,
	}
}

func buildLedgerRows(entries []storage.LedgerEntry) [][]interface{} {
	rows := make([][]interface{}, len(entries))
	for i, e := range entries {
		rows[i] = []interface{}{
			e.Date.UTC().Format(dateFormat), e.Type, e.AmountCents, e.RunningTotalCents, e.SessionID,
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

// Health reports whether the Sheets API is currently reachable, based on the
// most recent call through any endpoint (see restService.doJSON). down is
// true if that call failed; since is when the outage began (zero if
// healthy); err is nil if healthy. Sheets-only surface, reached by type
// assertion — see PendingWrites.
func (c *Client) Health() (down bool, since time.Time, err error) {
	return c.svc.health()
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

// ClearHistory clears the active store's history in place, preserving tab
// headers, formatting and cells outside the tracker tables.
func (c *Client) ClearHistory() error {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	columns := make(map[string]int, len(historyTabs()))
	for _, tab := range historyTabs() {
		columns[tab.name] = len(tab.headers)
	}
	cash, err := c.cashValues()
	if err != nil {
		return err
	}
	extra := make(map[int]int)
	for i, row := range cash {
		if width := cashMetadataWidth(row); i > 0 && width > 1 {
			extra[i] = 10 + min(width, len(row)-10)
		}
	}
	return c.svc.clearHistoryValues(c.spreadsheetID, columns, extra)
}

// ClearAllData rolls a transfer-owned sheet back to empty history.
func (c *Client) ClearAllData() error {
	return c.ClearHistory()
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
		amountCents, err := parseMoney(row[2])
		if err != nil {
			return nil, err
		}
		totalCents, err := parseMoney(row[3])
		if err != nil {
			return nil, err
		}
		entry := storage.LedgerEntry{
			Date:              date,
			Type:              fmt.Sprint(row[1]),
			AmountCents:       amountCents,
			RunningTotalCents: totalCents,
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
	case json.Number:
		n, _ := x.Float64()
		return int(n)
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
	case json.Number:
		value, _ := x.Float64()
		return value
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

// DrainPendingWrites is called only by the server worker, including its bot-ticker
// requests. It reuses normal replay and quota pacing without a connectivity probe.
func (c *Client) DrainPendingWrites() {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	c.flush()
}

// parseMoney decodes a USD-denominated cell into integer cents.
func parseMoney(value interface{}) (money.Cents, error) {
	if value == nil || value == "" {
		return 0, nil
	}
	return money.Parse(fmt.Sprint(value))
}

// PendingFallbackCount inspects durable writes without constructing a Google
// client or replaying them. Transfer startup must refuse pending/corrupt state.
func PendingFallbackCount(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading pending Sheets writes: %w", err)
	}
	var pending map[string]json.RawMessage
	if err := json.Unmarshal(data, &pending); err != nil {
		return 0, fmt.Errorf("parsing pending Sheets writes: %w", err)
	}
	return len(pending), nil
}

// CheckTransferReady refuses both cached and durable queued writes without
// replaying either. A corrupt queue cannot establish an empty source.
func (c *Client) CheckTransferReady() error {
	if c.PendingWrites() > 0 {
		return fmt.Errorf("Sheets writes are pending")
	}
	if c.fallbackPath != "" {
		n, err := PendingFallbackCount(c.fallbackPath)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("durable Sheets writes are pending")
		}
	}
	return nil
}
