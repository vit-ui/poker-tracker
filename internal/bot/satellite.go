package bot

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func (b *Bot) checkPendingTournaments(now time.Time) {
	pendingPath := filepath.Join(filepath.Dir(b.configPath), "pending_tournaments.json")
	var due []config.PendingTournament
	err := config.UpdatePendingTournaments(pendingPath, func(pending []config.PendingTournament) ([]config.PendingTournament, bool) {
		for i := range pending {
			if pending[i].QuietUntil != nil || pending[i].Historical || now.Sub(pending[i].LastRemindedAt) <= 30*time.Minute {
				continue
			}
			due = append(due, pending[i])
			pending[i].LastRemindedAt = now
		}
		return pending, len(due) > 0
	})
	if err != nil {
		slog.Error("updating pending tournament reminders", "err", err)
		return
	}
	for _, entry := range due {
		switch entry.Reason {
		case config.PendingFreerollBuyin:
			b.sendFreerollBuyInMenu(entry.Tournament)
		case config.PendingMissingResult:
			b.sendMissingResult(entry)
		default:
			b.sendSatelliteMenu(entry.Tournament)
		}
	}
}

func (b *Bot) sendSatelliteMenu(t parsers.Tournament) {
	gameType := "MTT"
	if t.BaseType() == "spingo" {
		gameType = "Spin & Go"
	}
	text := fmt.Sprintf("🎫 Satellite/qualifier detected\n%s #%s\nDate: %s\nBuy-in: $%s\nFinish: %s\nPrize: %s\nDid you pay cash for your entry, or use a ticket?", gameType, t.TournamentID, t.Date.Format("2006-01-02 15:04"), t.BuyInCents, TournamentFinishText(t), t.WinnerPrizeDescription)
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💵 Paid cash", "satellite_cash:"+t.TournamentID),
			tgbotapi.NewInlineKeyboardButtonData("🎫 Used a ticket", "satellite_ticket:"+t.TournamentID),
		),
	)
	b.sendMessageWithKeyboard(text, markup)
}

func RecoverTicketPayout(buyInCents, netCents money.Cents) (newBuyInCents, newNetCents money.Cents) {
	return 0, netCents + buyInCents
}

func (b *Bot) resolveSatellite(chatID int64, messageID int, tournamentID string, paidCash bool) {
	b.queueTournamentAnswer(storage.TournamentAnswer{
		TournamentID: tournamentID, Reason: config.PendingSatellite, Ticket: !paidCash,
	}, func(result storage.OperationResult) {
		if result.Err != nil {
			slog.Error("recording resolved satellite", "err", result.Err)
			send(b.tg, chatID, "Something went wrong. Check logs.")
			return
		}
		if !result.Found {
			send(b.tg, chatID, "Already resolved.")
			return
		}
		text := "✅ Logged as cash entry."
		if !paidCash {
			text = "✅ Logged as ticket entry — buy-in corrected to $0."
		}
		edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, text, emptyKeyboard)
		if _, err := b.tg.Send(edit); err != nil {
			slog.Error("editing telegram message", "err", err)
		}
	})
}
