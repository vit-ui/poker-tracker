package bot

// The freeroll/Kick-Off buy-in confirmation flow: the inline-keyboard question
// asking whether a zero-buy-in tournament was entered for free or paid, and
// the answer handling that records the queued tournament. PokerStars records
// no buy-in and no entry method for these, but a knockout freeroll still pays
// real bounty cash (folded in from the hand history file), so the row must be
// recorded — only the entry cost is unknown, and the player supplies it.

import (
	"fmt"
	"log/slog"
	"path/filepath"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
)

// sendFreerollBuyInMenu asks whether a zero-buy-in tournament was entered for
// free (a won level-up) or paid. Includes buy-in-free, date, and finish/field
// size so the player can tell tournaments apart when more than one is pending —
// the identifying detail is otherwise just a TournamentID.
func (b *Bot) sendFreerollBuyInMenu(t parsers.Tournament) {
	gameType := "MTT"
	if t.BaseType() == "spingo" {
		gameType = "Spin & Go"
	}
	text := fmt.Sprintf("🎟️ Freeroll / Kick-Off detected — no buy-in recorded\n%s #%s\nDate: %s\nFinish: %s / %d\nDid you enter for free (won your way in), or pay for it?",
		gameType, t.TournamentID, t.Date.Local().Format("2006-01-02 15:04"), Ordinal(t.FinishPos), t.TotalPlayers)
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🆓 Free (won my way in)", "freeroll_free:"+t.TournamentID),
			tgbotapi.NewInlineKeyboardButtonData("💵 I paid", "freeroll_paid:"+t.TournamentID),
		),
	)
	b.sendMessageWithKeyboard(text, markup)
}

// beginFreerollBuyInEntry starts the single-flight text entry for a paid
// freeroll's buy-in amount, remembering the tournament ID and the menu message
// to edit back once the amount is typed. Mirrors beginValueEntry.
func (b *Bot) beginFreerollBuyInEntry(chatID int64, messageID int, tournamentID string) {
	b.flow = "freeroll_buyin"
	b.field = tournamentID
	b.menuMessageID = messageID
	send(b.tg, chatID, "How much did you pay to enter? Send the USD amount (as shown on PokerStars, not BRL).")
}

// handleFreerollBuyInReply saves the buy-in typed in response to
// beginFreerollBuyInEntry and records the tournament. On an invalid amount it
// re-prompts and keeps the flow state so the player can retry.
func (b *Bot) handleFreerollBuyInReply(chatID int64, text string) {
	amount, err := parsePositiveFloat(text)
	if err != nil {
		send(b.tg, chatID, "Invalid amount. Send the USD buy-in you paid (a positive number), or 0 isn't accepted here — tap 🆓 Free on the question instead.")
		return
	}
	tournamentID := b.field
	messageID := b.menuMessageID
	b.reset()
	b.recordFreerollEntry(chatID, messageID, tournamentID, amount)
}

// recordFreerollEntry records a queued freeroll with the resolved buy-in
// (0 for a free entry) and removes it from the pending queue. NetUSD from the
// parser is the payout with a zero buy-in, so subtracting the answered buy-in
// folds it in (same as re-entry's cost subtraction); AppendTournament then
// folds in whatever knockout bounty the hand history file already recorded.
// messageID is edited to the confirmation (a callback message for the free
// tap, the remembered menu message for the paid text reply).
func (b *Bot) recordFreerollEntry(chatID int64, messageID int, tournamentID string, buyInUSD float64) {
	pendingPath := filepath.Join(filepath.Dir(b.configPath), "pending_tournaments.json")
	pending, err := config.LoadPendingTournaments(pendingPath)
	if err != nil {
		slog.Error("loading pending tournaments", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}

	idx := -1
	for i, p := range pending {
		if p.Tournament.TournamentID == tournamentID {
			idx = i
			break
		}
	}
	if idx == -1 {
		send(b.tg, chatID, "Already resolved.")
		return
	}

	t := pending[idx].Tournament
	t.NetUSD -= buyInUSD
	t.BuyInUSD = buyInUSD
	netUSD, err := b.store.AppendTournament(t)
	if err != nil {
		slog.Error("appending tournament", "err", err)
		send(b.tg, chatID, "Something went wrong. Check logs.")
		return
	}

	pending = append(pending[:idx], pending[idx+1:]...)
	if err := config.SavePendingTournaments(pendingPath, pending); err != nil {
		slog.Error("saving pending tournaments", "err", err)
	}

	var resultText string
	if buyInUSD == 0 {
		resultText = fmt.Sprintf("✅ Logged as a free entry (buy-in $0).\nNet: %s", FormatSignedUSD(netUSD))
	} else {
		resultText = fmt.Sprintf("✅ Logged — buy-in $%.2f.\nNet: %s", buyInUSD, FormatSignedUSD(netUSD))
	}
	if bankroll, err := b.store.GetCurrentBankroll(); err != nil {
		slog.Error("getting bankroll", "err", err)
	} else {
		resultText += fmt.Sprintf("\nBankroll: $%.2f", bankroll)
	}

	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, resultText, emptyKeyboard)
	if _, err := b.tg.Send(edit); err != nil {
		slog.Error("editing telegram message", "err", err)
	}
}
