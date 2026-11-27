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

var sheetsEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

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

type Client struct {
	mutationMu    sync.Mutex
	svc           *restService
	spreadsheetID string

	fallbackPath string
	fallbackMu   sync.Mutex
	fallback     map[string]fallbackEntry
}

var _ storage.Store = (*Client)(nil)

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
		if err := c.loadFallback(); err != nil {
			slog.Error("loading sheets fallback, starting empty", "err", err)
			c.fallback = make(map[string]fallbackEntry)
		}
		c.flush()
	}

	return c, nil
}

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

		if err := c.backfillTabHeaders(tab.name, tab.headers); err != nil {
			return err
		}
	}

	return nil
}

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

func (c *Client) getLastRunningTotal() (money.Cents, error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, "Bankroll Ledger!D:D")
	if err != nil {
		return 0, fmt.Errorf("reading bankroll ledger: %w", err)
	}
	if len(resp.Values) <= 1 {
		return 0, nil
	}
	last := resp.Values[len(resp.Values)-1]
	if len(last) == 0 {
		return 0, nil
	}
	return parseMoney(last[0])
}

func CashSessionID(date time.Time, firstHandID string) string {
	return storage.CashSessionIdentities(date, firstHandID).LegacySheets
}

func (c *Client) findRowByColumnValue(tab, columnLetter, want string) (rowIndex int, found bool, err error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, fmt.Sprintf("%s!%s:%s", tab, columnLetter, columnLetter))
	if err != nil {
		return 0, false, fmt.Errorf("reading %s column %s: %w", tab, columnLetter, err)
	}
	for i, row := range resp.Values {
		if i == 0 {
			continue
		}
		if len(row) > 0 && fmt.Sprint(row[0]) == want {
			return i + 1, true, nil
		}
	}
	return max(2, len(resp.Values)+1), false, nil
}

func (c *Client) rawAppendCashSession(session parsers.CashSession) error {
	identities := storage.CashSessionIdentities(session.Date, session.FirstHandID)
	id := identities.Canonical
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
		if identities.Matches(value) {
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

const (
	tournamentDateColumn = iota
	tournamentTypeColumn
	tournamentIDColumn
	tournamentBuyInColumn
	tournamentPrizePoolColumn
	tournamentMultiplierColumn
	tournamentFinishColumn
	tournamentFieldSizeColumn
	tournamentNetColumn
	tournamentVPIPColumn
	tournamentPFRColumn
	tournamentBountyColumn
	tournamentPrizeWonColumn
	tournamentHandsColumn
	tournamentResultSourceColumn
	tournamentResultHandIDColumn
)

type tournamentRowSnapshot struct {
	date        time.Time
	typeVal     string
	buyInCents  money.Cents
	hasNetUSD   bool
	netCents    money.Cents
	bountyCents money.Cents
	hands       int
}

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
	snap.hands = parseInt(cell(row, tournamentHandsColumn))
	if len(row) > tournamentDateColumn {
		if date, err := parseSheetDate(row[tournamentDateColumn]); err == nil {
			snap.date = date
		}
	}
	if len(row) > tournamentTypeColumn {
		snap.typeVal = fmt.Sprint(row[tournamentTypeColumn])
	}
	if len(row) > tournamentBuyInColumn {
		snap.buyInCents, err = parseMoney(row[tournamentBuyInColumn])
		if err != nil {
			return tournamentRowSnapshot{}, err
		}
	}
	if len(row) > tournamentNetColumn && fmt.Sprint(row[tournamentNetColumn]) != "" {
		snap.hasNetUSD = true
		snap.netCents, err = parseMoney(row[tournamentNetColumn])
		if err != nil {
			return tournamentRowSnapshot{}, err
		}
	}
	if len(row) > tournamentBountyColumn {
		snap.bountyCents, err = parseMoney(row[tournamentBountyColumn])
		if err != nil {
			return tournamentRowSnapshot{}, err
		}
	}
	return snap, nil
}

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
		{"Tournaments", row - 1, tournamentDateColumn, [][]interface{}{values}},
		{"Tournaments", row - 1, tournamentPrizeWonColumn, [][]interface{}{{prizeWonCents}}},
		{"Tournaments", row - 1, tournamentResultSourceColumn, [][]interface{}{{t.ResultSource, t.ResultHandID}}},
	}
	err = c.writeResults(writes, []storage.LedgerEntry{{Date: t.Date, Type: "tournament", AmountCents: netCents, SessionID: t.TournamentID}})
	return netCents, err
}

func (c *Client) rawUpdateTournamentHandStats(tournamentID string, vpip, pfr float64, hands int, bountyCents money.Cents, isKnockout, preserveBounty bool) error {
	rowIndex, found, err := c.findTournamentRow(tournamentID)
	if err != nil {
		return err
	}
	if !found {
		typeCell := ""
		if isKnockout {
			typeCell = storage.KnockoutPlaceholder
		}
		partial := storage.TournamentRecord{
			Tournament:     parsers.Tournament{Type: typeCell, TournamentID: tournamentID, VPIP: &vpip, PFR: &pfr, Hands: &hands},
			BountyWonCents: bountyCents,
		}
		return c.appendRow("Tournaments", partialTournamentRow(partial))
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
	writes := []gridWrite{{"Tournaments", rowIndex - 1, tournamentBountyColumn, [][]interface{}{{bountyCents}}}}
	if storage.UseTournamentSample(preserveBounty, hands, snap.hands) {
		writes = append(writes, gridWrite{"Tournaments", rowIndex - 1, tournamentVPIPColumn, [][]interface{}{{vpip, pfr}}}, gridWrite{"Tournaments", rowIndex - 1, tournamentHandsColumn, [][]interface{}{{hands}}})
	}
	if typeOut != snap.typeVal {
		writes = append(writes, gridWrite{"Tournaments", rowIndex - 1, tournamentTypeColumn, [][]interface{}{{typeOut}}})
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
	writes = append(writes, gridWrite{"Tournaments", rowIndex - 1, tournamentNetColumn, [][]interface{}{{netCents}}}, gridWrite{"Tournaments", rowIndex - 1, tournamentPrizeWonColumn, [][]interface{}{{prizeWonCents}}})
	return c.writeResults(writes, []storage.LedgerEntry{{Date: snap.date, Type: "tournament", AmountCents: netCents, SessionID: tournamentID}})
}

const (
	fallbackKindCash    = "cash"
	fallbackKindStats   = "tourney_stats"
	fallbackKindSummary = "tourney_summary"
	fallbackKindLedger  = "ledger"
)

type fallbackEntry struct {
	QueuedAt  time.Time            `json:"queued_at,omitempty"`
	Kind      string               `json:"kind"`
	Cash      *parsers.CashSession `json:"cash,omitempty"`
	CashHands *cashHandWrite       `json:"cash_hands,omitempty"`
	Summary   *parsers.Tournament  `json:"summary,omitempty"`
	HandStats *handStatsArgs       `json:"hand_stats,omitempty"`
	Ledger    *ledgerArgs          `json:"ledger,omitempty"`
}

type handStatsArgs struct {
	TournamentID   string      `json:"tournament_id"`
	VPIP           float64     `json:"vpip"`
	PFR            float64     `json:"pfr"`
	Hands          int         `json:"hands"`
	BountyCents    money.Cents `json:"bounty_cents"`
	IsKnockout     bool        `json:"is_knockout"`
	PreserveBounty bool        `json:"preserve_bounty,omitempty"`
}

type ledgerArgs struct {
	OperationID string      `json:"operation_id,omitempty"`
	EntryType   string      `json:"entry_type"`
	AmountCents money.Cents `json:"amount_cents"`
	Now         time.Time   `json:"now"`
}

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
	if err != nil {
		slog.Error("persisting sheets fallback", "err", err)
	}
	return err
}

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
	if persistErr != nil {
		slog.Error("persisting sheets fallback after flush", "err", persistErr)
	}
}

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
		return c.rawAppendLedgerEntry(entry.Ledger.EntryType, entry.Ledger.AmountCents, entry.Ledger.Now, entry.Ledger.OperationID) == nil
	}
	return true
}

type flushChoice int

const (
	flushReplay flushChoice = iota
	flushDiscard
)

func flushDecision(cachedHands, sheetHands int, rowExists bool) flushChoice {
	if rowExists && sheetHands > cachedHands {
		return flushDiscard
	}
	return flushReplay
}

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

func (c *Client) GetCurrentBankroll() (money.Cents, error) {
	return c.getLastRunningTotal()
}

func (c *Client) GetSheetURL() string {
	return "https://docs.google.com/spreadsheets/d/" + c.spreadsheetID
}

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

func (c *Client) GetAllCashSessions() ([]storage.CashSessionRecord, error) {
	return c.queryCashSessions(time.Time{})
}

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

func parseTournamentRow(row []interface{}) (rec storage.TournamentRecord, hasDate bool, err error) {
	rec.Type = fmt.Sprint(cell(row, tournamentTypeColumn))
	rec.TournamentID = fmt.Sprint(cell(row, tournamentIDColumn))
	rec.ResultSource = fmt.Sprint(cell(row, tournamentResultSourceColumn))
	rec.ResultHandID = fmt.Sprint(cell(row, tournamentResultHandIDColumn))
	if date, err := parseSheetDate(cell(row, tournamentDateColumn)); err == nil {
		rec.Date = date
		hasDate = true
	}
	rec.BuyInCents, err = parseMoney(cell(row, tournamentBuyInColumn))
	if err != nil {
		return rec, false, err
	}
	if desc, ok := cell(row, tournamentPrizePoolColumn).(string); ok && desc != "" {
		rec.PrizePoolDescription = desc
	} else {
		rec.PrizePoolCents, err = parseMoney(cell(row, tournamentPrizePoolColumn))
		if err != nil {
			return rec, false, err
		}
	}
	if mult := fmt.Sprint(cell(row, tournamentMultiplierColumn)); mult != "" {
		m := parseFloat(cell(row, tournamentMultiplierColumn))
		rec.Multiplier = &m
	}
	rec.FinishPos = parseInt(cell(row, tournamentFinishColumn))
	rec.TotalPlayers = parseInt(cell(row, tournamentFieldSizeColumn))
	rec.NetCents, err = parseMoney(cell(row, tournamentNetColumn))
	if err != nil {
		return rec, false, err
	}
	if v := fmt.Sprint(cell(row, tournamentVPIPColumn)); v != "" {
		f := parseFloat(cell(row, tournamentVPIPColumn))
		rec.VPIP = &f
	}
	if p := fmt.Sprint(cell(row, tournamentPFRColumn)); p != "" {
		f := parseFloat(cell(row, tournamentPFRColumn))
		rec.PFR = &f
	}
	rec.BountyWonCents, err = parseMoney(cell(row, tournamentBountyColumn))
	if err != nil {
		return rec, false, err
	}
	rec.PrizeWonCents, err = parseMoney(cell(row, tournamentPrizeWonColumn))
	if err != nil {
		return rec, false, err
	}
	if h := fmt.Sprint(cell(row, tournamentHandsColumn)); h != "" {
		n := parseInt(cell(row, tournamentHandsColumn))
		rec.Hands = &n
	}
	return rec, hasDate, nil
}

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

func (c *Client) GetAllTournaments() ([]storage.TournamentRecord, error) {
	return c.queryTournaments(time.Time{})
}

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
			rows[i] = partialTournamentRow(t)
			continue
		}
		rows[i] = append(tournamentResultCells(t.Tournament),
			floatCell(t.VPIP), floatCell(t.PFR), t.BountyWonCents, t.PrizeWonCents, intCell(t.Hands), t.ResultSource, t.ResultHandID,
		)
	}
	return rows
}

func partialTournamentRow(t storage.TournamentRecord) []interface{} {
	return []interface{}{
		tournamentDateColumn:       "",
		tournamentTypeColumn:       t.Type,
		tournamentIDColumn:         t.TournamentID,
		tournamentBuyInColumn:      "",
		tournamentPrizePoolColumn:  "",
		tournamentMultiplierColumn: "",
		tournamentFinishColumn:     "",
		tournamentFieldSizeColumn:  "",
		tournamentNetColumn:        "",
		tournamentVPIPColumn:       floatCell(t.VPIP),
		tournamentPFRColumn:        floatCell(t.PFR),
		tournamentBountyColumn:     t.BountyWonCents,
		tournamentPrizeWonColumn:   "",
		tournamentHandsColumn:      intCell(t.Hands),
	}
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

func (c *Client) Health() (down bool, since time.Time, err error) {
	return c.svc.health()
}

func (c *Client) PendingWrites() int {
	c.fallbackMu.Lock()
	defer c.fallbackMu.Unlock()
	return len(c.fallback)
}

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

func (c *Client) DrainPendingWrites() {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	c.flush()
}

func parseMoney(value interface{}) (money.Cents, error) {
	if value == nil || value == "" {
		return 0, nil
	}
	return money.Parse(fmt.Sprint(value))
}

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
