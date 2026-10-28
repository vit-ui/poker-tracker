package bot

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestMenuReplyReplacesClickedMessage(t *testing.T) {
	cases := []struct {
		name string
		path string
		back string
	}{
		{"status result", "root:info:status", "menu:root:info"},
		{"help result", "root:help", "menu:root"},
		{"credentials prompt", "root:setup:credentials", "menu:root:setup"},
		{"amount prompt", "root:money:deposit", "menu:root:money"},
		{"folder prompt", "root:setup:paths:hh", "menu:root:setup:paths"},
		{"threshold prompt", "root:notify:alerts:downswing:set", "menu:root:notify:alerts:downswing"},
		{"standalone settings menu", "schedule:day_start", "menu:schedule"},
		{"root result", "root", "menu:cancel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := conversation{menuReplyMessageID: 42, menuReplyPath: tc.path}
			edit, ok := c.replyConfig(123, "Result or prompt").(tgbotapi.EditMessageTextConfig)
			if !ok {
				t.Fatal("menu reply would create a new message instead of editing the clicked one")
			}
			if edit.ChatID != 123 || edit.MessageID != 42 || edit.Text != "Result or prompt" {
				t.Fatalf("wrong menu edit: %+v", edit)
			}
			if edit.ReplyMarkup == nil || len(edit.ReplyMarkup.InlineKeyboard) != 1 || len(edit.ReplyMarkup.InlineKeyboard[0]) != 1 {
				t.Fatalf("result must replace the old buttons with one navigation button: %+v", edit.ReplyMarkup)
			}
			button := edit.ReplyMarkup.InlineKeyboard[0][0]
			if button.CallbackData == nil || *button.CallbackData != tc.back {
				t.Fatalf("wrong Back/Done target: %+v", button)
			}
		})
	}
}

func TestMenuReplyRetainsTargetAfterFlowReset(t *testing.T) {
	c := conversation{flow: "credentials", field: "file", menuReplyMessageID: 42, menuReplyPath: "root:setup:credentials"}
	c.reset()
	if c.flow != "" || c.field != "" {
		t.Fatal("reset did not end the input flow")
	}
	if _, ok := c.replyConfig(123, "Kept the existing credentials file.").(tgbotapi.EditMessageTextConfig); !ok {
		t.Fatal("completion reply lost its menu target after reset")
	}
}

func TestCommandReplyUsesStandaloneMessage(t *testing.T) {
	c := conversation{}
	msg, ok := c.replyConfig(123, "Status").(tgbotapi.MessageConfig)
	if !ok {
		t.Fatal("a reply without a menu target must create a standalone message")
	}
	if msg.ChatID != 123 || msg.Text != "Status" || msg.ReplyMarkup != nil {
		t.Fatalf("wrong standalone reply: %+v", msg)
	}
}
