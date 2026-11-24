package bot

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

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
	polling.Client = pollingClient{ctx, telegramHTTPClient{}}
	go func() {
		defer close(p.done)
		defer close(p.updates)
		u := tgbotapi.NewUpdate(offset)
		u.Timeout = 30
		for {
			updates, err := polling.GetUpdates(u)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				slog.Error("receiving Telegram updates", "err", err)
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

type telegramSender struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

func (s *telegramSender) resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.ctx.Err() != nil {
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}
}

func (s *telegramSender) interrupt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel()
}

func (s *telegramSender) Do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	return telegramHTTPClient{}.Do(req.WithContext(ctx))
}

func (s *telegramSender) watchOwner(controls <-chan OwnerControl, stop, done <-chan struct{}) (<-chan OwnerControl, <-chan struct{}) {
	next := make(chan OwnerControl)
	stopped := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				close(stopped)
				s.interrupt()
				return
			case <-done:
				return
			case control, ok := <-controls:
				if !ok {
					controls = nil
					continue
				}
				s.interrupt()
				select {
				case next <- control:
				case <-stop:
					close(stopped)
					return
				case <-done:
					return
				}
			}
		}
	}()
	return next, stopped
}
