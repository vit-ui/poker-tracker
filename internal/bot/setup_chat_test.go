package bot

import (
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"testing"
)

func TestSetupContactKeepsSavedChat(t *testing.T) {
	message := func(chat int64, text string) tgbotapi.Update {
		return tgbotapi.Update{Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: chat}, Text: text}}
	}
	for _, tc := range []struct {
		name    string
		saved   int64
		updates []tgbotapi.Update
		want    int64
	}{
		{"fresh first sender", 0, []tgbotapi.Update{message(123, "/start"), message(4242, "hello")}, 123},
		{"saved private chat", 4242, []tgbotapi.Update{message(123, "/start"), message(4242, "hello")}, 4242},
		{"saved group", -4242, []tgbotapi.Update{message(123, "/start"), message(-4242, "hello")}, -4242},
		{"foreign chat cannot replace saved", 4242, []tgbotapi.Update{message(123, "hello")}, 0},
		{"non text ignored", 4242, []tgbotapi.Update{{}, {Message: &tgbotapi.Message{Text: "missing chat"}}, message(4242, ""), message(4242, "/start")}, 4242},
		{"closed stream", 0, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updates := make(chan tgbotapi.Update, len(tc.updates))
			for _, up := range tc.updates {
				updates <- up
			}
			close(updates)
			if got := waitForFirstMessage(updates, tc.saved); got != tc.want {
				t.Fatalf("chat %d, want %d", got, tc.want)
			}
		})
	}
}
