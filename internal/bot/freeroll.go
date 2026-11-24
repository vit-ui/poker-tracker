package bot

import (
	"fmt"
	"log/slog"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func (b *Bot) sendFreerollBuyInMenu(t parsers.Tournament) {
	gameType := "MTT"
	if t.BaseType() == "spingo" {
		gameType = "Spin & Go"
	}
	text := fmt.Sprintf("🎟️ Freeroll / Kick-Off detected — no buy-in recorded\n%s #%s\nDate: %s\nFinish: %s\nDid you enter for free (won your way in), or pay for it?", gameType, t.TournamentID, t.Date.Format("2006-01-02 15:04"), TournamentFinishText(t))
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🆓 Free (won my way in)", "freeroll_free:"+t.TournamentID),
			tgbotapi.NewInlineKeyboardButtonData("💵 I paid", "freeroll_paid:"+t.TournamentID),
		),
	)
	b.sendMessageWithKeyboard(text, markup)
}

func (b *Bot) beginFreerollBuyInEntry(chatID int64, messageID int, tournamentID string) {
	b.flow = "freeroll_buyin"
	b.field = tournamentID
	b.menuMessageID = messageID
	send(b.tg, chatID, "How much did you pay to enter? Send the USD amount (as shown on PokerStars, not BRL).")
}

func (b *Bot) handleFreerollBuyInReply(chatID int64, text string) {
	amountCents, err := parsePositiveMoney(text)
	if err != nil {
		send(b.tg, chatID, "Invalid amount. Send the USD buy-in you paid (a positive number), or 0 isn't accepted here — tap 🆓 Free on the question instead.")
		return
	}
	tournamentID := b.field
	messageID := b.menuMessageID
	b.reset()
	b.recordFreerollEntry(chatID, messageID, tournamentID, amountCents)
}

func (b *Bot) recordFreerollEntry(chatID int64, messageID int, tournamentID string, buyInCents money.Cents) {
	b.queueTournamentAnswer(storage.TournamentAnswer{
		TournamentID: tournamentID, Reason: config.PendingFreerollBuyin, TotalCostCents: buyInCents,
	}, func(result storage.OperationResult) {
		if result.Err != nil {
			slog.Error("recording resolved freeroll", "err", result.Err)
			send(b.tg, chatID, "Something went wrong. Check logs.")
			return
		}
		if !result.Found {
			send(b.tg, chatID, "Already resolved.")
			return
		}
		var text string
		if buyInCents == 0 {
			text = fmt.Sprintf("✅ Logged as a free entry (buy-in $0).\nNet: %s", FormatSignedUSD(result.NetCents))
		} else {
			text = fmt.Sprintf("✅ Logged — buy-in $%s.\nNet: %s", buyInCents, FormatSignedUSD(result.NetCents))
		}
		text += fmt.Sprintf("\nBankroll: $%s", result.BankrollCents)
		edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, text, emptyKeyboard)
		if _, err := b.tg.Send(edit); err != nil {
			slog.Error("editing telegram message", "err", err)
		}
	})
}
