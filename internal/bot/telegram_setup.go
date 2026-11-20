package bot

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fsnotify/fsnotify"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/uiprompt"
)

// The Telegram client puts the bot token in the request URL. Keep transport
// errors from including that URL in process logs or update status.
type telegramHTTPClient struct{}

func (telegramHTTPClient) Do(req *http.Request) (*http.Response, error) {
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Telegram HTTP request failed")
	}
	return response, nil
}

func newTelegramBot(token string) (*tgbotapi.BotAPI, error) {
	return tgbotapi.NewBotAPIWithClient(token, apiEndpoint, telegramHTTPClient{})
}

// ConnectTelegram provisions a bot for this installation. The short pairing
// code proves the incoming chat is the one controlled by the person at setup.
func ConnectTelegram(configPath string) error {
	token, err := config.LoadTelegramToken(configPath)
	if err != nil || token != "" {
		return err
	}
	proceed, err := uiprompt.EnsureTelegramSetupPrivileges()
	if err != nil || !proceed {
		return err
	}
	for token == "" {
		value, err := uiprompt.AskTelegramToken()
		if err != nil {
			return err
		}
		if value == "" {
			return fmt.Errorf("Telegram setup was closed")
		}
		tg, err := newTelegramBot(value)
		if err != nil {
			if err := uiprompt.TelegramSetupMessage("Tracker could not verify that token. Check your connection and paste the token BotFather gave you."); err != nil {
				return err
			}
			continue
		}
		if err := pairTelegram(configPath, tg, value); err != nil {
			return err
		}
		return uiprompt.TelegramSetupMessage("Telegram is connected. Open @" + tg.Self.UserName + " to use Poker Tracker.")
	}
	return nil
}

func pairTelegram(configPath string, tg *tgbotapi.BotAPI, token string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	code := "/start " + rand.Text()
	instructions := "Open https://t.me/" + tg.Self.UserName + " in Telegram and send:\n\n" + code + "\n\nThen return to this window and continue."
	if err := uiprompt.TelegramSetupMessage(instructions); err != nil {
		return err
	}

	query := tgbotapi.NewUpdate(0)
	query.Timeout = 30
	for {
		updates, err := tg.GetUpdates(query)
		if err != nil {
			return fmt.Errorf("could not receive the Telegram pairing message")
		}
		for _, update := range updates {
			query.Offset = update.UpdateID + 1
			message := update.Message
			if message == nil || message.Chat == nil || strings.TrimSpace(message.Text) != code {
				continue
			}
			id := strconv.FormatInt(message.Chat.ID, 10)
			if cfg.Setup.TelegramChatID != nil && *cfg.Setup.TelegramChatID != id {
				return fmt.Errorf("send the pairing code from the Telegram account already linked to this installation")
			}
			if _, err := config.UpdateConfig(configPath, func(current *config.Config) error {
				if saved := current.Setup.TelegramChatID; saved != nil && *saved != id {
					return fmt.Errorf("the linked Telegram chat changed during setup")
				}
				current.Setup.TelegramChatID = &id
				return nil
			}); err != nil {
				return err
			}
			if err := config.SaveTelegramToken(configPath, token); err != nil {
				return err
			}
			send(tg, message.Chat.ID, "Poker Tracker is connected. Use /menu to get started.")
			return nil
		}
	}
}

func waitForTelegramToken(configPath string, alerts <-chan string, stop <-chan struct{}, ready chan<- struct{}) (string, error) {
	dir := filepath.Dir(config.TelegramTokenPath(configPath))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("creating credentials directory: %w", err)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return "", fmt.Errorf("watching Telegram setup: %w", err)
	}
	defer watcher.Close()
	if err := watcher.Add(dir); err != nil {
		return "", fmt.Errorf("watching credentials directory: %w", err)
	}
	token, err := config.LoadTelegramToken(configPath)
	if err != nil {
		return "", err
	}
	if ready != nil {
		ready <- struct{}{}
	}
	if token != "" {
		return token, nil
	}

	slog.Info("Telegram setup is pending; open this installation with --connect-telegram")
	if err := uiprompt.ShowPendingTelegramSetup(); err != nil {
		slog.Error("opening Telegram setup window", "err", err)
	}
	for {
		select {
		case <-stop:
			return "", nil
		case _, ok := <-alerts:
			if !ok {
				return "", nil
			}
		case err := <-watcher.Errors:
			return "", fmt.Errorf("watching Telegram setup: %w", err)
		case event := <-watcher.Events:
			if event.Name != config.TelegramTokenPath(configPath) {
				continue
			}
			token, err := config.LoadTelegramToken(configPath)
			if token != "" || err != nil {
				return token, err
			}
		}
	}
}
