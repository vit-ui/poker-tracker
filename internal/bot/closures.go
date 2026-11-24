package bot

import (
	"fmt"
	"log/slog"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
)

type ClosureNotifications struct {
	Deliveries chan config.ClosureEvent
	Results    chan ClosureResult
}

type ClosureResult struct {
	ID  string
	Err error
}

func (b *Bot) sendClosure(event config.ClosureEvent) ClosureResult {
	result := ClosureResult{ID: event.ID}
	chatID, ok := b.connectedChat()
	if !ok {
		result.Err = fmt.Errorf("no configured Telegram chat for closure notification")
		return result
	}
	_, result.Err = b.tg.Send(tgbotapi.NewMessage(chatID, hardstop.ClosureMessage(event, b.now())))
	if result.Err != nil {
		slog.Error("sending PokerStars closure notification", "event_id", event.ID, "err", result.Err)
	}
	return result
}
