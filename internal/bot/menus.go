package bot

import (
	"log/slog"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const menuIdleTime = 5 * time.Minute

type menuKey struct {
	chatID    int64
	messageID int
}

type openMenu struct {
	expires   time.Time
	callbacks map[string]bool
	revision  uint64
}

func (b *Bot) showMenu(chatID int64, messageID int, text string, rows [][]tgbotapi.InlineKeyboardButton) int {
	markup := tgbotapi.NewInlineKeyboardMarkup(rows...)
	key := menuKey{chatID, messageID}
	menu := b.menus[key]
	if messageID == 0 {
		msg := tgbotapi.NewMessage(chatID, text)
		msg.ReplyMarkup = markup
		sent, err := b.tg.Send(msg)
		if err != nil {
			slog.Error("sending menu message", "err", err)
			return 0
		}
		key.messageID = sent.MessageID
		if b.menus == nil {
			b.menus = make(map[menuKey]*openMenu)
		}
		menu = &openMenu{}
		b.menus[key] = menu
	} else {
		if menu == nil || !time.Now().Before(menu.expires) {
			return 0
		}
		edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, text, markup)
		if _, err := b.tg.Send(edit); err != nil {
			if !strings.Contains(err.Error(), "message is not modified") {
				slog.Error("editing menu message", "err", err)
				return 0
			}
		}
	}
	menu.expires = time.Now().Add(menuIdleTime)
	menu.callbacks = make(map[string]bool)
	for _, row := range rows {
		for _, button := range row {
			if button.CallbackData != nil {
				menu.callbacks[*button.CallbackData] = true
			}
		}
	}
	return key.messageID
}

func (b *Bot) nextMenuExpiry() time.Time {
	var next time.Time
	for _, menu := range b.menus {
		if next.IsZero() || menu.expires.Before(next) {
			next = menu.expires
		}
	}
	return next
}

func (b *Bot) expireMenus(now time.Time) {
	for key, menu := range b.menus {
		if !now.Before(menu.expires) {
			b.closeMenu(key.chatID, key.messageID)
		}
	}
}

func (b *Bot) closeMenu(chatID int64, messageID int) {
	key := menuKey{chatID, messageID}
	if b.menus[key] == nil {
		return
	}
	delete(b.menus, key)
	b.resetMenuPrompt(messageID)
	if b.storageResetMessageID == messageID {
		b.storageResetMessageID = 0
	}
	if ps := b.pendingSwitch; ps != nil && ps.messageID == messageID {
		closeStore(ps.dst)
		b.pendingSwitch = nil
	}
	if _, err := b.tg.Request(tgbotapi.NewDeleteMessage(chatID, messageID)); err != nil {
		slog.Error("deleting menu message", "err", err)
		b.clearKeyboard(chatID, messageID)
	}
}

func (b *Bot) resetMenuPrompt(messageID int) {
	if b.menuMessageID == messageID {
		b.reset()
	}
}

func (b *Bot) refreshMenuPrompt(chatID int64) {
	if menu := b.menus[menuKey{chatID, b.menuMessageID}]; menu != nil {
		menu.expires = time.Now().Add(menuIdleTime)
	}
}

func menuCallback(data string) bool {
	return strings.HasPrefix(data, "menu:") || strings.HasPrefix(data, "scan:") ||
		strings.HasPrefix(data, "update:") || strings.HasPrefix(data, "transactions:") ||
		strings.HasPrefix(data, "storage:")
}

func (b *Bot) acceptMenuCallback(key menuKey, data string) bool {
	menu := b.menus[key]
	now := time.Now()
	if menu == nil || !now.Before(menu.expires) || !menu.callbacks[data] {
		return false
	}
	menu.expires = now.Add(menuIdleTime)
	menu.revision++
	return true
}

func (b *Bot) menuRevision(chatID int64, messageID int) uint64 {
	if menu := b.menus[menuKey{chatID, messageID}]; menu != nil {
		return menu.revision
	}
	return 0
}

func (b *Bot) menuCurrent(chatID int64, messageID int, revision uint64) bool {
	if messageID == 0 {
		return true
	}
	menu := b.menus[menuKey{chatID, messageID}]
	return menu != nil && time.Now().Before(menu.expires) && menu.revision == revision
}
