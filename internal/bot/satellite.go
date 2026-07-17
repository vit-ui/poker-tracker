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
	"github.com/vit-ui/self-tracker/internal/parsers"
)

// checkPendingTournaments re-sends the satellite/ticket confirmation menu for
// any pending entry more than 30 minutes past its last reminder.
func (b *Bot) checkPendingTournaments(now time.Time) {
	pendingPath := filepath.Join(filepath.Dir(b.configPath), "pending_tournaments.json")
	pending, err := config.LoadPendingTournaments(pendingPath)
	if err != nil {
		slog.Error("loading pending tournaments", "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	changed := false
	for i := range pending {
		if now.Sub(pending[i].LastRemindedAt) <= 30*time.Minute {
			continue
		}
		b.sendSatelliteMenu(pending[i].Tournament)
		pending[i].LastRemindedAt = now
		changed = true
	}
	if changed {
		if err := config.SavePendingTournaments(pendingPath, pending); err != nil {
			slog.Error("saving pending tournaments", "err", err)
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
	text := fmt.Sprintf("🎫 Satellite/qualifier detected\n%s #%s\nDate: %s\nBuy-in: $%.2f\nFinish: %s / %d\nPrize: %s\nDid you pay cash for your entry, or use a ticket?",
		gameType, t.TournamentID, t.Date.Local().Format("2006-01-02 15:04"), t.BuyInUSD, Ordinal(t.FinishPos), t.TotalPlayers, t.WinnerPrizeDescription)
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💵 Paid cash", "satellite_cash:"+t.TournamentID),
			tgbotapi.NewInlineKeyboardButtonData("🎫 Used a ticket", "satellite_ticket:"+t.TournamentID),
		),
	)
	b.sendMessageWithKeyboard(text, markup)
}

// RecoverTicketPayout computes the corrected BuyInUSD/NetUSD for a
// LikelySatellite tournament once the player confirms they paid with a
// ticket rather than cash. NetUSD was originally computed as payout minus
// buy-in, so adding them back recovers the payout before zeroing the buy-in.
func RecoverTicketPayout(buyInUSD, netUSD float64) (newBuyInUSD, newNetUSD float64) {
	return 0, netUSD + buyInUSD
}

// resolveSatellite handles a tap on the satellite/ticket confirmation menu.
func (b *Bot) resolveSatellite(chatID int64, messageID int, tournamentID string, paidCash bool) {
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
	var resultText string
	if paidCash {
		if _, err := b.store.AppendTournament(t); err != nil {
			slog.Error("appending tournament", "err", err)
			send(b.tg, chatID, "Something went wrong. Check logs.")
			return
		}
		resultText = "✅ Logged as cash entry."
	} else {
		t.BuyInUSD, t.NetUSD = RecoverTicketPayout(t.BuyInUSD, t.NetUSD)
		if _, err := b.store.AppendTournament(t); err != nil {
			slog.Error("appending tournament", "err", err)
			send(b.tg, chatID, "Something went wrong. Check logs.")
			return
		}
		resultText = "✅ Logged as ticket entry — buy-in corrected to $0."
	}

	pending = append(pending[:idx], pending[idx+1:]...)
	if err := config.SavePendingTournaments(pendingPath, pending); err != nil {
		slog.Error("saving pending tournaments", "err", err)
	}

	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, resultText, emptyKeyboard)
	if _, err := b.tg.Send(edit); err != nil {
		slog.Error("editing telegram message", "err", err)
	}
}
