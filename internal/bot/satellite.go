package bot

// The satellite/ticket confirmation flow: the reminder loop that re-asks
// unanswered questions, the inline-keyboard question itself, and the answer
// handling that finally writes the queued tournament.

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

// checkPendingTournaments re-sends the satellite/ticket confirmation menu for
// any pending entry more than 30 minutes past its last reminder.
func (b *Bot) checkPendingTournaments(now time.Time) {
	pendingPath := filepath.Join(filepath.Dir(b.configPath), "pending_tournaments.json")
	var due []config.PendingTournament
	err := config.UpdatePendingTournaments(pendingPath, func(pending []config.PendingTournament) ([]config.PendingTournament, bool) {
		for i := range pending {
			if now.Sub(pending[i].LastRemindedAt) <= 30*time.Minute {
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
		if entry.Reason == config.PendingFreerollBuyin {
			b.sendFreerollBuyInMenu(entry.Tournament)
		} else {
			b.sendSatelliteMenu(entry.Tournament)
		}
	}
}

// sendSatelliteMenu asks the cash-vs-ticket entry question. The prize shown
// isn't always finish position 1's — WinnerPrizeDescription is set from
// whichever row's payout first read as a ticket/pass, scanning every result
// row rather than just position 1 (which can still read "still playing" if
// the summary was captured while other players were still in) — so it's
// labeled generically rather than claiming it's always what 1st place won.
// Includes buy-in, date, and finish position/field size — enough for the
// player to tell tournaments apart when more than one is pending at once,
// since the identifying detail here is otherwise just a TournamentID.
func (b *Bot) sendSatelliteMenu(t parsers.Tournament) {
	gameType := "MTT"
	if t.BaseType() == "spingo" {
		gameType = "Spin & Go"
	}
	text := fmt.Sprintf("🎫 Satellite/qualifier detected\n%s #%s\nDate: %s\nBuy-in: $%s\nFinish: %s / %d\nPrize: %s\nDid you pay cash for your entry, or use a ticket?", gameType, t.TournamentID, t.Date.Format("2006-01-02 15:04"), t.BuyInCents, Ordinal(t.FinishPos), t.TotalPlayers, t.WinnerPrizeDescription)
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💵 Paid cash", "satellite_cash:"+t.TournamentID),
			tgbotapi.NewInlineKeyboardButtonData("🎫 Used a ticket", "satellite_ticket:"+t.TournamentID),
		),
	)
	b.sendMessageWithKeyboard(text, markup)
}

// RecoverTicketPayout computes the corrected BuyInCents/NetCents for a
// LikelySatellite tournament once the player confirms they paid with a
// ticket rather than cash. NetCents was originally computed as payout minus
// buy-in, so adding them back recovers the payout before zeroing the buy-in.
func RecoverTicketPayout(buyInCents, netCents money.Cents) (newBuyInCents, newNetCents money.Cents) {
	return 0, netCents + buyInCents
}

// resolveSatellite handles a tap on the satellite/ticket confirmation menu.
func (b *Bot) resolveSatellite(chatID int64, messageID int, tournamentID string, paidCash bool) {
	b.queueOperation(func(store storage.Store) storage.OperationResult {
		return recordPendingTournament(store, b.configPath, tournamentID, func(t parsers.Tournament) parsers.Tournament {
			if !paidCash {
				t.BuyInCents, t.NetCents = RecoverTicketPayout(t.BuyInCents, t.NetCents)
			}
			return t
		})
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
