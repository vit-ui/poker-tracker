package sheets

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

const cashHistoryTab = "Cash Sessions"

type cashHandWrite struct {
	Session    parsers.CashSession
	Hands      []parsers.CashHand
	LiveSource string
}

func (c *Client) RecordCashHands(session parsers.CashSession, hands []parsers.CashHand, liveSource string) (storage.CashWrite, error) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	input := cashHandWrite{session, hands, liveSource}
	data, err := json.Marshal(input)
	if err != nil {
		return storage.CashWrite{}, err
	}
	key := fmt.Sprintf("cash_hands:%x", sha256.Sum256(data))
	entry := fallbackEntry{Kind: "cash_hands", CashHands: &input}
	if c.hasPendingCash() {
		c.flush()
		if c.hasPendingCash() {
			return storage.CashWrite{}, c.queueWriteFailure(key, entry, fmt.Errorf("earlier cash history writes are pending"))
		}
	}
	counts, err := c.rawRecordCashHands(input)
	if err != nil {
		return storage.CashWrite{}, c.queueWriteFailure(key, entry, err)
	}
	c.flush()
	return counts, nil
}

func (c *Client) hasPendingCash() bool {
	c.fallbackMu.Lock()
	defer c.fallbackMu.Unlock()
	for _, entry := range c.fallback {
		if entry.Cash != nil || entry.CashHands != nil {
			return true
		}
	}
	return false
}

func (c *Client) cashValues() ([][]interface{}, error) {
	resp, err := c.svc.valuesGet(c.spreadsheetID, "'Cash Sessions'")
	if err != nil {
		return nil, fmt.Errorf("reading cash session ownership: %w", err)
	}
	return resp.Values, nil
}

func (c *Client) rawRecordCashHands(input cashHandWrite) (storage.CashWrite, error) {
	var counts storage.CashWrite
	cash, err := c.cashValues()
	if err != nil {
		return counts, err
	}
	if len(cash) == 0 {
		return counts, fmt.Errorf("Cash Sessions header is missing")
	}
	var existing []storage.CashSessionRecord
	rowsByID := make(map[string]int)
	for i, values := range cash[1:] {
		rec, valid, err := parseCashRow(values)
		if err != nil {
			return counts, err
		}
		if valid {
			rowsByID[rec.SessionID] = i + 1
			existing = append(existing, rec)
		}
	}
	changed, counts, err := storage.ReconcileCashHands(existing, input.Session, input.Hands, input.LiveSource)
	if err != nil || len(changed) == 0 {
		return counts, err
	}

	var writes []gridWrite
	newLedger := make([]storage.LedgerEntry, 0, len(changed))
	nextCashRow := len(cash)
	for _, row := range changed {
		index, exists := rowsByID[row.SessionID]
		if !exists {
			index = nextCashRow
			nextCashRow++
		}
		values := buildCashRows([]storage.CashSessionRecord{row})[0]
		if exists {
			for len(values) < len(cash[index]) && len(values) < 10+cashMetadataWidth(cash[index]) {
				values = append(values, "")
			}
		}
		writes = append(writes, gridWrite{cashHistoryTab, index, 0, [][]interface{}{values}})
		newLedger = append(newLedger, storage.LedgerEntry{Date: row.Date, Type: "cash_session", SessionID: row.SessionID, AmountCents: row.NetCents})
	}
	return counts, c.writeResults(writes, newLedger)
}

func (c *Client) writeResults(writes []gridWrite, changes []storage.LedgerEntry) error {
	ledger, err := c.svc.valuesGet(c.spreadsheetID, "Bankroll Ledger!A:E")
	if err != nil {
		return err
	}
	if len(ledger.Values) == 0 {
		return fmt.Errorf("Bankroll Ledger header is missing")
	}
	ledgerWrites, err := resultLedgerWrites(ledger.Values, changes)
	if err != nil {
		return err
	}
	return c.svc.writeHistoryGrid(c.spreadsheetID, append(writes, ledgerWrites...))
}

func resultLedgerWrites(ledger [][]interface{}, changes []storage.LedgerEntry) ([]gridWrite, error) {
	type resultKey struct{ kind, id string }
	updates := make(map[resultKey]storage.LedgerEntry, len(changes))
	for _, entry := range changes {
		updates[resultKey{entry.Type, entry.SessionID}] = entry
	}
	keyFor := func(row []interface{}) resultKey {
		return resultKey{fmt.Sprint(cell(row, 1)), fmt.Sprint(cell(row, 4))}
	}
	first := len(ledger)
	for i, row := range ledger[1:] {
		if _, changed := updates[keyFor(row)]; changed {
			first = i + 1
			break
		}
	}

	var totalCents money.Cents
	var err error
	if first > 1 {
		totalCents, err = parseMoney(cell(ledger[first-1], 3))
		if err != nil {
			return nil, err
		}
	}
	var writes []gridWrite
	var tail []money.Cents
	for i, row := range ledger[first:] {
		amountCents, err := parseMoney(cell(row, 2))
		if err != nil {
			return nil, err
		}
		if changed, ok := updates[keyFor(row)]; ok {
			amountCents = changed.AmountCents
			if changed.Type == "tournament" {
				writes = append(writes, gridWrite{"Bankroll Ledger", first + i, 0, [][]interface{}{{changed.Date.Format(dateFormat)}}})
			}
			delete(updates, keyFor(row))
		}
		tail = append(tail, amountCents)
	}
	if len(tail) > 0 {
		totals, err := storage.CascadeRunningTotals(tail, tail[0], totalCents)
		if err != nil {
			return nil, err
		}
		pairs := make([][]interface{}, len(totals))
		for i, total := range totals {
			pairs[i] = []interface{}{tail[i], total}
		}
		totalCents = totals[len(totals)-1]
		writes = append(writes, gridWrite{"Bankroll Ledger", first, 2, pairs})
	}
	var appended []storage.LedgerEntry
	for _, entry := range changes {
		if _, appendRow := updates[resultKey{entry.Type, entry.SessionID}]; !appendRow {
			continue
		}
		totalCents, err = money.Add(totalCents, entry.AmountCents)
		if err != nil {
			return nil, err
		}
		entry.RunningTotalCents = totalCents
		appended = append(appended, entry)
	}
	if len(appended) > 0 {
		writes = append(writes, gridWrite{"Bankroll Ledger", len(ledger), 0, buildLedgerRows(appended)})
	}
	return writes, nil
}

const cashMetadataChunk = 40000

func encodeCashMetadata(hands []parsers.CashHand) []interface{} {
	if len(hands) == 0 {
		return nil
	}
	raw, _ := json.Marshal(hands)
	width := (len(raw) + cashMetadataChunk - 1) / cashMetadataChunk
	cells := make([]interface{}, 0, width)
	for i := 0; i < len(raw); i += cashMetadataChunk {
		cells = append(cells, string(raw[i:min(i+cashMetadataChunk, len(raw))]))
	}
	cells[0] = fmt.Sprintf("hands:%d:%s", width, cells[0])
	return cells
}

func cashMetadataWidth(row []interface{}) int {
	prefix := fmt.Sprint(cell(row, 10))
	rest, ok := strings.CutPrefix(prefix, "hands:")
	if !ok {
		return 0
	}
	count, _, _ := strings.Cut(rest, ":")
	width, _ := strconv.Atoi(count)
	return width
}

func decodeCashMetadata(row []interface{}) ([]parsers.CashHand, error) {
	width := cashMetadataWidth(row)
	if !strings.HasPrefix(fmt.Sprint(cell(row, 10)), "hands:") {
		return nil, nil
	}
	if width <= 0 || width > len(row)-10 {
		return nil, fmt.Errorf("cash hand metadata is incomplete")
	}
	var raw strings.Builder
	_, payload, _ := strings.Cut(strings.TrimPrefix(fmt.Sprint(row[10]), "hands:"), ":")
	raw.WriteString(payload)
	for _, chunk := range row[11 : 10+width] {
		raw.WriteString(fmt.Sprint(chunk))
	}
	var hands []parsers.CashHand
	if err := json.Unmarshal([]byte(raw.String()), &hands); err != nil {
		return nil, fmt.Errorf("decoding cash hand metadata: %w", err)
	}
	return hands, nil
}
