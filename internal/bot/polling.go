package bot

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// pollingClient scopes library HTTP cancellation to the polling copy of BotAPI.
// Message sends keep the original client and remain on the bot event loop.
type pollingClient struct {
	context context.Context
	client  tgbotapi.HTTPClient
}

func (c pollingClient) Do(request *http.Request) (*http.Response, error) {
	return c.client.Do(request.WithContext(c.context))
}

type updatePoller struct {
	updates chan tgbotapi.Update
	cancel  context.CancelFunc
	done    chan struct{}
}

func startUpdatePoller(api *tgbotapi.BotAPI, offset int) *updatePoller {
	ctx, cancel := context.WithCancel(context.Background())
	p := &updatePoller{updates: make(chan tgbotapi.Update), cancel: cancel, done: make(chan struct{})}
	polling := *api
	polling.Client = pollingClient{ctx, api.Client}
	go func() {
		defer close(p.done)
		defer close(p.updates)
		u := tgbotapi.NewUpdate(offset)
		u.Timeout = 30 // Preserve the existing Telegram long poll.
		for {
			updates, err := polling.GetUpdates(u)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				slog.Error("receiving Telegram updates", "err", err)
				// This is the library poller's existing retry cadence, now owner-cancellable.
				timer := time.NewTimer(3 * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			for _, update := range updates {
				if update.UpdateID < u.Offset {
					continue
				}
				select {
				case p.updates <- update:
					u.Offset = update.UpdateID + 1
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return p
}
func (p *updatePoller) stop() { p.cancel(); <-p.done }
