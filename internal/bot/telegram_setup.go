package bot

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/uiprompt"
)

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

type TelegramSetupOptions struct {
	connect, ifNeeded, status bool
}

func RegisterTelegramSetup(flags *flag.FlagSet) *TelegramSetupOptions {
	options := new(TelegramSetupOptions)
	flags.BoolVar(&options.connect, "connect-telegram", false, "link this installation to your Telegram bot")
	flags.BoolVar(&options.ifNeeded, "connect-telegram-if-needed", false, "link Telegram interactively if setup is pending")
	flags.BoolVar(&options.status, "telegram-status", false, "check whether a local Telegram pairing is saved")
	return options
}

func (o *TelegramSetupOptions) Handle(flags *flag.FlagSet, path string) (handled bool, exitCode int) {
	if (o.status || o.ifNeeded || o.connect) && (flags.NFlag() != 1 || flags.NArg() != 0) {
		fmt.Fprintln(os.Stderr, "Use a Telegram setup command by itself.")
		return true, 2
	}
	switch {
	case o.status:
		credentials, err := loadTelegramCredentials(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 2
		}
		if credentials.token == "" {
			fmt.Println(telegramSetupPending())
			return true, 1
		}
		fmt.Println("Telegram pairing saved.")
	case o.ifNeeded:
		if err := FinishTelegramSetup(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
	case o.connect || runtime.GOOS == "windows" && flags.NFlag() == 0 && flags.NArg() == 0:
		if err := ConnectTelegram(path); err != nil {
			_ = uiprompt.TelegramSetupMessage(err.Error())
			return true, 1
		}
	default:
		return false, 0
	}
	return true, 0
}

func FinishTelegramSetup(path string) error {
	credentials, err := loadTelegramCredentials(path)
	if err != nil {
		return err
	}
	if credentials.token != "" {
		return nil
	}
	if !uiprompt.CanPromptTelegram() {
		return errors.New(telegramSetupPending())
	}
	if err := ConnectTelegram(path); err != nil {
		return fmt.Errorf("%s\n%w", telegramSetupPending(), err)
	}
	credentials, err = loadTelegramCredentials(path)
	if err != nil {
		return err
	}
	if credentials.token == "" {
		return errors.New(telegramSetupPending())
	}
	return nil
}

func telegramSetupPending() string {
	exe, err := os.Executable()
	if err != nil {
		exe = filepath.Base(os.Args[0])
	}
	if runtime.GOOS == "windows" {
		return "Telegram setup pending. On the bot host, run in PowerShell: & '" + strings.ReplaceAll(exe, "'", "''") + "' --connect-telegram"
	}
	return "Telegram setup pending. On the bot host, run: '" + strings.ReplaceAll(exe, "'", "'\"'\"'") + "' --connect-telegram"
}

func ConnectTelegram(configPath string) error {
	proceed, err := uiprompt.EnsureTelegramSetupPrivileges()
	if err != nil || !proceed {
		return err
	}
	oldToken, err := config.LoadTelegramToken(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	saved := chatIDValue(cfg)
	if saved < 0 {
		confirm, err := uiprompt.ConfirmTelegramSetup("This installation is linked to a group. Switch to a private chat?")
		if err != nil || !confirm {
			return err
		}
	}
	for {
		token, err := uiprompt.AskTelegramToken()
		if err != nil {
			return err
		}
		if token == "" {
			return fmt.Errorf("Telegram setup was closed")
		}
		tg, err := newTelegramBot(token)
		if err != nil {
			if err := uiprompt.TelegramSetupMessage("Tracker could not verify that token. Check your connection and paste the token BotFather gave you."); err != nil {
				return err
			}
			continue
		}
		if token != oldToken || saved <= 0 {
			if err := pairTelegram(configPath, tg, token, saved); err != nil {
				return err
			}
		}
		if err := uiprompt.TelegramSetupMessage("Telegram is connected. Open @" + tg.Self.UserName + " to use Poker Tracker."); err != nil {
			slog.Error("displaying Telegram connection confirmation", "err", err)
		}
		return nil
	}
}

func TelegramTokenForSetup(path string) (string, error) {
	credentials, err := loadTelegramCredentials(path)
	if err != nil || credentials.token != "" {
		return credentials.token, err
	}
	if err := ConnectTelegram(path); err != nil {
		return "", err
	}
	credentials, err = loadTelegramCredentials(path)
	if err == nil && credentials.token == "" {
		err = fmt.Errorf("finish Connect Telegram before using the Telegram setup wizard")
	}
	return credentials.token, err
}

func chatIDValue(cfg *config.Config) int64 {
	if cfg.Setup.TelegramChatID == nil {
		return 0
	}
	id, _ := strconv.ParseInt(*cfg.Setup.TelegramChatID, 10, 64)
	return id
}

func privateChat(chat *tgbotapi.Chat) bool {
	return chat != nil && chat.IsPrivate() && chat.ID > 0
}

func pairTelegram(configPath string, tg *tgbotapi.BotAPI, token string, saved int64) error {
	code := "/start " + rand.Text()
	instructions := "Open https://t.me/" + tg.Self.UserName + " in a private chat and send:\n\n" + code
	answer, err := uiprompt.ConfirmTelegramSetup(instructions + "\n\nContinue after sending the code.")
	if err != nil {
		return err
	}
	if !answer {
		return fmt.Errorf("Telegram setup was closed")
	}

	query := tgbotapi.NewUpdate(0)
	for {
		updates, err := tg.GetUpdates(query)
		if err != nil {
			return fmt.Errorf("could not receive the Telegram pairing message")
		}
		if len(updates) == 0 {
			return fmt.Errorf("pairing code was not received in the linked private chat")
		}
		for _, update := range updates {
			query.Offset = update.UpdateID + 1
			msg := update.Message
			if msg == nil || !privateChat(msg.Chat) || strings.TrimSpace(msg.Text) != code {
				continue
			}
			id := msg.Chat.ID
			if saved > 0 && saved != id {
				return fmt.Errorf("send the pairing code from the Telegram account already linked to this installation")
			}
			if saved == id {
				return config.SaveTelegramToken(configPath, token)
			}
			var previous *string
			if _, err := config.UpdateConfig(configPath, func(current *config.Config) error {
				if chatIDValue(current) != saved {
					return fmt.Errorf("the linked Telegram chat changed during setup")
				}
				previous = current.Setup.TelegramChatID
				value := strconv.FormatInt(id, 10)
				current.Setup.TelegramChatID = &value
				return nil
			}); err != nil {
				return err
			}
			if err := config.SaveTelegramToken(configPath, token); err != nil {
				_, rollback := config.UpdateConfig(configPath, func(current *config.Config) error {
					if chatIDValue(current) == id {
						current.Setup.TelegramChatID = previous
					}
					return nil
				})
				return errors.Join(err, rollback)
			}
			return nil
		}
	}
}

type telegramCredentials struct {
	token  string
	chatID int64
}

type telegramConnection struct {
	credentials telegramCredentials
	bot         *tgbotapi.BotAPI
	needsSetup  bool
	failed      bool
	err         error
}

func loadTelegramCredentials(path string) (telegramCredentials, error) {
	token, err := config.LoadTelegramToken(path)
	if err != nil || token == "" {
		return telegramCredentials{}, err
	}
	cfg, err := config.ReadConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		return telegramCredentials{}, nil
	}
	if err != nil {
		return telegramCredentials{}, err
	}
	chatID := chatIDValue(cfg)
	if chatID <= 0 {
		return telegramCredentials{}, nil
	}
	return telegramCredentials{token, chatID}, nil
}

func probeTelegram(ctx context.Context, credentials telegramCredentials) telegramConnection {
	tg, err := tgbotapi.NewBotAPIWithClient(credentials.token, apiEndpoint, pollingClient{ctx, telegramHTTPClient{}})
	if err != nil {
		var apiErr *tgbotapi.Error
		return telegramConnection{
			credentials: credentials,
			needsSetup:  errors.As(err, &apiErr) && (apiErr.Code == 401 || apiErr.Code == 404),
			err:         fmt.Errorf("Telegram did not accept the connection; check the bot token and network"),
		}
	}
	tg.Client = telegramHTTPClient{}
	return telegramConnection{credentials: credentials, bot: tg}
}
