package bot

import (
	"crypto/rand"
	"fmt"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/storage"
)

const collectorSettingsExplanation = "Limits and schedules are saved on the server. Folder and screen-name changes are queued until the poker PC confirms them, because only that PC can validate its folders and restart collection. Relative paths use its active Windows desktop user's home; send a full path if no desktop user is available."

func (b *Bot) saveCollectorChange(chatID int64, settings config.CollectorSettings) {
	if err := settings.Validate(); err != nil {
		send(b.tg, chatID, err.Error())
		return
	}
	id := "collector-settings:" + rand.Text()
	if b.updateID != 0 {
		id = fmt.Sprintf("collector-settings:%d:%d:%d", b.tg.Self.ID, chatID, b.updateID)
	}
	// A queue-save reply is immediate local durability, never PC acknowledgement.
	b.queueOperation(nil, func(result storage.OperationResult) {
		if result.Err != nil {
			send(b.tg, chatID, "Could not save the PC change locally. It was not queued. Check logs.")
			return
		}
		message := "PC change saved in the server's queue, pending confirmation."
		if !result.CollectorOnline {
			message += " The poker PC is disconnected; the edit will be sent when it reconnects."
		}
		send(b.tg, chatID, message+"\n\n"+collectorSettingsExplanation)
	})
	request := &b.pendingOperations[len(b.pendingOperations)-1].request
	request.CollectorSettings = true
	request.CollectorChange = &config.CollectorChange{ID: id, Settings: settings}
	b.reset()
}
