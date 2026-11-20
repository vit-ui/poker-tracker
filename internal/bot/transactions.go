package bot

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func parseTransactionsArgs(args string) (history bool, period string, err error) {
	fields := strings.Fields(args)
	if len(fields) > 0 && fields[0] == "history" {
		history = true
		fields = fields[1:]
	}
	period = "all"
	if len(fields) > 1 {
		return false, "", fmt.Errorf("invalid transaction arguments")
	}
	if len(fields) == 1 {
		period = fields[0]
	}
	switch period {
	case "today", "week", "month", "all":
		return history, period, nil
	}
	return false, "", fmt.Errorf("invalid transaction period")
}

func (b *Bot) handleTransactionsCommand(chatID int64, args string) {
	history, period, err := parseTransactionsArgs(args)
	if err != nil {
		send(b.tg, chatID, "Usage: /transactions [today|week|month|all]\nHistory: /transactions history [period]")
		return
	}
	b.showTransactions(chatID, 0, "transactions", period, statsSince(period, b.now()), history, 0)
}

func (b *Bot) handleTransactionsCallback(chatID int64, messageID int, data string) {
	fields := strings.SplitN(data, ":", 4)
	if len(fields) != 4 {
		return
	}
	_, period, err := parseTransactionsArgs(fields[0])
	if err != nil {
		return
	}
	stamp, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return
	}
	page, err := strconv.Atoi(fields[2])
	if err != nil || page < 0 {
		return
	}
	b.showTransactions(chatID, messageID, fields[3], period, time.Unix(stamp, 0).UTC(), true, page)
}

func (b *Bot) showTransactions(chatID int64, messageID int, path, period string, since time.Time, history bool, page int) {
	var text string
	var rows [][]tgbotapi.InlineKeyboardButton
	b.queueOperation(func(store storage.Store) storage.OperationResult {
		entries, err := store.GetLedgerEntriesSince(since)
		if err != nil {
			return storage.OperationResult{Err: err}
		}
		pages, err := transactionPages(entries, period, history)
		if err != nil {
			return storage.OperationResult{Err: err}
		}
		if page >= len(pages) {
			page = len(pages) - 1
		}
		text = pages[page]
		if history {
			callback := func(n int) string { return fmt.Sprintf("transactions:%s:%d:%d:%s", period, since.Unix(), n, path) }
			var buttons []tgbotapi.InlineKeyboardButton
			if page > 0 {
				buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("‹ Previous", callback(page-1)))
			}
			if page+1 < len(pages) {
				buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("Next ›", callback(page+1)))
			}
			if len(buttons) > 0 {
				rows = append(rows, buttons)
			}
		}
		return storage.OperationResult{}
	}, func(result storage.OperationResult) {
		if result.Err != nil {
			send(b.tg, chatID, "Could not load transactions. Please try again.")
			return
		}
		if history {
			b.showMenu(chatID, messageID, text, append(rows, navButton(path)))
		} else {
			send(b.tg, chatID, text)
		}
	})
}

// transactionPages shares filtering and exact totals between summaries and history.
// Stable date sorting preserves ledger order for equal timestamps before reversal.
func transactionPages(entries []storage.LedgerEntry, period string, history bool) ([]string, error) {
	totals := map[string]money.Cents{}
	var movements []storage.LedgerEntry
	for _, entry := range entries {
		switch entry.Type {
		case "deposit", "withdrawal", "reward":
		default:
			continue
		}
		total, err := money.Add(totals[entry.Type], entry.AmountCents)
		if err != nil {
			return nil, fmt.Errorf("summing %s: %w", entry.Type, err)
		}
		totals[entry.Type] = total
		movements = append(movements, entry)
	}
	netCents, err := money.Add(totals["deposit"], totals["withdrawal"])
	if err != nil {
		return nil, err
	}
	netCents, err = money.Add(netCents, totals["reward"])
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("Transactions (%s)\nDeposits: %s\nWithdrawals: %s\nRewards: %s\nNet bankroll change: %s", period, FormatSignedUSD(totals["deposit"]), FormatSignedUSD(totals["withdrawal"]), FormatSignedUSD(totals["reward"]), FormatSignedUSD(netCents))
	if !history {
		return []string{header}, nil
	}
	sort.SliceStable(movements, func(i, j int) bool { return movements[i].Date.Before(movements[j].Date) })
	var bodies []string
	body := ""
	count := 0
	for i := len(movements) - 1; i >= 0; i-- {
		entry := movements[i]
		line := fmt.Sprintf("\n%s · %s · %s", entry.Date.Format("2006-01-02 15:04"), entry.Type, FormatSignedUSD(entry.AmountCents))
		// Reserve space for the header and page indicator below Telegram's limit.
		if count == 10 || len([]rune(header+body+line)) > 3900 {
			bodies = append(bodies, body)
			body = ""
			count = 0
		}
		body += line
		count++
	}
	if body == "" && len(bodies) == 0 {
		body = "\nNo transactions in this period."
	}
	if body != "" {
		bodies = append(bodies, body)
	}
	pages := make([]string, len(bodies))
	for i, body := range bodies {
		pages[i] = fmt.Sprintf("%s\nHistory · page %d/%d\n%s", header, i+1, len(bodies), body)
	}
	return pages, nil
}
