package bot

import (
	"fmt"
	"log/slog"

	"github.com/vit-ui/self-tracker/internal/storage"
)

// restartConfiguration drains accepted collection and worker operations before
// changing startup settings. The worker remains held until restart or rollback.
func (b *Bot) restartConfiguration(chatID int64, label string, finalizeCash bool, change func(storage.Store) (func() error, error), cleanup func()) {
	b.migrating = true
	release, resumed := make(chan struct{}), make(chan error, 1)
	var rollback func() error
	send(b.tg, chatID, "Draining accepted work before updating "+label+"…")
	b.queueOperation(func(store storage.Store) storage.OperationResult {
		var err error
		rollback, err = change(store)
		return storage.OperationResult{Err: err}
	}, func(result storage.OperationResult) {
		if result.Err != nil {
			cleanup()
			b.migrating = false
			send(b.tg, chatID, fmt.Sprintf("Could not update %s: %v", label, result.Err))
			return
		}
		send(b.tg, chatID, "✅ Updated "+label+". Restarting tracker…")
		if err := selfRestart(b.configPath, b.taskName); err == nil {
			cleanup()
			b.restarting = true
			return
		} else {
			slog.Error("restarting after configuration change", "setting", label, "err", err)
		}
		if err := rollback(); err != nil {
			slog.Error("restoring settings after failed restart", "setting", label, "err", err)
			send(b.tg, chatID, "Automatic restart failed and the previous settings could not be restored. Collection remains paused. Keep the installation files and restart this tracker service manually.")
			return
		}
		cleanup()
		close(release)
		b.queueOperation(func(storage.Store) storage.OperationResult { return storage.OperationResult{Err: <-resumed} }, func(result storage.OperationResult) {
			b.migrating = false
			if result.Err != nil {
				send(b.tg, chatID, fmt.Sprintf("Previous settings restored, but collection could not resume: %v. Restart the collector service manually.", result.Err))
			} else {
				send(b.tg, chatID, "Automatic restart failed. Previous settings restored and collection resumed.")
			}
		})
	})
	request := &b.pendingOperations[len(b.pendingOperations)-1].request
	request.PrepareRestart, request.FinalizeAllCash = true, finalizeCash
	request.Release, request.Resumed = release, resumed
}
