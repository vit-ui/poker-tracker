package bot

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TournamentFinishText(t parsers.Tournament) string {
	if t.TotalPlayers == 0 {
		return Ordinal(t.FinishPos)
	}
	return fmt.Sprintf("%s / %d", Ordinal(t.FinishPos), t.TotalPlayers)
}

func (b *Bot) sendScanReport(text string) {
	if !strings.Contains(text, "\n"+storage.ScanReviewLabel+" (") {
		b.sendMessage(text)
		return
	}
	b.sendMessageWithKeyboard(text, tgbotapi.NewInlineKeyboardMarkup(menuRow(storage.ScanReviewLabel, "missing_results")))
}

func (b *Bot) sendMissingResult(entry config.PendingTournament) {
	t := entry.Tournament
	finish := entry.Recovery
	text := fmt.Sprintf("Tournament #%s · %s\nFinish %s", t.TournamentID, t.Date.Format("2006-01-02 15:04"), TournamentFinishText(t))
	if finish.PayoutCents != nil {
		text += fmt.Sprintf("\nCash payout $%s", *finish.PayoutCents)
	}
	if finish.TotalCostCents != nil {
		text += fmt.Sprintf("\nTotal cash entry cost $%s", *finish.TotalCostCents)
	} else if finish.EntryCents != nil {
		text += fmt.Sprintf("\nListed price per entry $%s", *finish.EntryCents)
	}
	b.sendMessageWithKeyboard(text, tgbotapi.NewInlineKeyboardMarkup(menuRow("Confirm result", "missing_result:"+t.TournamentID+":"+t.ResultHandID)))
}

func (b *Bot) reviewMissingResults(chatID int64, key string) {
	pending, err := config.LoadPendingTournaments(filepath.Join(filepath.Dir(b.configPath), "pending_tournaments.json"))
	if err != nil {
		slog.Error("loading missing tournament results", "err", err)
		send(b.tg, chatID, "Could not load missing results. Check the tracker log.")
		return
	}
	for _, entry := range pending {
		t := entry.Tournament
		if entry.Recovery == nil || entry.QuietUntil != nil {
			continue
		}
		if key == "" {
			if entry.Historical {
				b.sendMissingResult(entry)
				return
			}
			continue
		}
		if key == t.TournamentID+":"+t.ResultHandID {
			b.reset()
			b.flow, b.field = "missing_result", key
			b.recovery = &entry
			b.recoveryPayoutCents = entry.Recovery.PayoutCents
			b.promptRecoveryAmount(chatID)
			return
		}
	}
	send(b.tg, chatID, "No missing results are ready for confirmation.")
}

func (b *Bot) promptRecoveryAmount(chatID int64) {
	if b.recoveryPayoutCents == nil {
		send(b.tg, chatID, "Send the cash payout in USD, excluding any knockout bounties. Send 0 for no cash payout or type cancel.")
	} else {
		send(b.tg, chatID, "Send your total cash entry cost in USD, including any re-entries. Send 0 if you paid no cash or type cancel.")
	}
}

func (b *Bot) handleRecoveryReply(chatID int64, text string) {
	if strings.EqualFold(strings.TrimSpace(text), "cancel") {
		b.reset()
		send(b.tg, chatID, "Result confirmation cancelled.")
		return
	}
	amountCents, err := money.Parse(commaAsDecimal(strings.TrimSpace(text)))
	if err != nil || amountCents < 0 {
		b.promptRecoveryAmount(chatID)
		return
	}
	entry := b.recovery
	var costCents money.Cents
	if b.recoveryPayoutCents == nil {
		b.recoveryPayoutCents = &amountCents
		if entry.Recovery.TotalCostCents == nil {
			b.promptRecoveryAmount(chatID)
			return
		}
		costCents = *entry.Recovery.TotalCostCents
	} else {
		costCents = amountCents
	}
	answer := storage.TournamentAnswer{
		Reason:       config.PendingMissingResult,
		TournamentID: entry.Tournament.TournamentID, HandID: entry.Tournament.ResultHandID,
		TotalCostCents: costCents, PayoutCents: b.recoveryPayoutCents,
	}
	b.reset()
	b.queueTournamentAnswer(answer, func(result storage.OperationResult) {
		switch {
		case result.Err != nil:
			slog.Error("recording confirmed tournament result", "err", result.Err)
			send(b.tg, chatID, "Could not record the result. Check the tracker log.")
		case !result.Found:
			send(b.tg, chatID, "This result is no longer awaiting confirmation.")
		default:
			send(b.tg, chatID, fmt.Sprintf("✅ Tournament #%s recorded. Net %s", answer.TournamentID, FormatSignedUSD(result.NetCents)))
		}
	})
}
