package bot

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/storage"
	"github.com/vit-ui/self-tracker/internal/update"
)

type releaseCheckResult struct {
	version, action, requestID string
	chatID                     int64
	err                        error
}

func (b *Bot) queueUpdates(request *config.UpdateRequest, complete func(storage.OperationResult)) {
	b.queueOperation(nil, complete)
	op := &b.pendingOperations[len(b.pendingOperations)-1].request
	op.Updates, op.UpdateRequest = true, request
}

func (b *Bot) handleUpdateCommand(chatID int64, arg string) {
	if b.updater == nil {
		send(b.tg, chatID, "Install the current release before requesting Telegram updates. See /help update.")
		return
	}
	action := strings.ToLower(strings.TrimSpace(arg))
	if action == "" {
		b.queueUpdates(nil, func(result storage.OperationResult) { b.renderUpdateMenu(chatID, result) })
		return
	}
	if action != "pc" && action != "server" && action != "all" && action != "check" {
		send(b.tg, chatID, "Use /update for the menu, or /update pc, /update server, /update all, /update check.")
		return
	}
	if b.updater.Role == "combined" && action == "server" {
		send(b.tg, chatID, "This all-local installation runs on the poker PC. Use /update pc or /update all.")
		return
	}
	id := fmt.Sprintf("telegram-update:%d:%d:%s", b.tg.Self.ID, b.updateID, action)
	b.checkRelease(chatID, action, id)
}

func (b *Bot) renderUpdateMenu(chatID int64, result storage.OperationResult) {
	if result.Err != nil {
		send(b.tg, chatID, "Could not read update requests: "+result.Err.Error())
		return
	}
	report, err := b.updater.Report()
	if err != nil {
		send(b.tg, chatID, "Could not read local update request: "+err.Error())
		return
	}
	label := "Server"
	if b.updater.Role == "combined" {
		label = "This PC"
	}
	text := label + ": " + report.Version + "\n" + updateJobText(label, report.Job)
	rows := [][]tgbotapi.InlineKeyboardButton{}
	if b.updater.Role == "server" {
		pcVersion := result.Updates.Report.Version
		if pcVersion == "" {
			pcVersion = "not reported yet"
		}
		online := "online"
		if !result.CollectorOnline {
			online = "offline"
		}
		text += "\nPoker PC: " + pcVersion + " (" + online + ")\n" + collectorUpdateText(result.Updates)
		rows = append(rows, menuRow("Update poker PC", "update:pc"), menuRow("Update server", "update:server"), menuRow("Update both", "update:all"))
	} else {
		rows = append(rows, menuRow("Update this PC", "update:pc"))
	}
	if b.latestVersion != "" {
		text += "\nLatest stable release: " + b.latestVersion
	}
	text += "\n\nRequests are saved before confirmation. The poker PC waits until PokerStars closes. Settings, history and lock expiry are preserved."
	rows = append(rows, menuRow("Check for new version", "update:check"))
	b.showMenu(chatID, 0, text, rows)
}

func (b *Bot) checkRelease(chatID int64, action, requestID string) {
	if b.checkingRelease {
		if chatID != 0 {
			send(b.tg, chatID, "A release check is already in progress. Please try /update again after it finishes.")
		}
		return
	}
	if b.releaseResults == nil {
		b.releaseResults = make(chan releaseCheckResult, 1)
	}
	b.checkingRelease, b.lastReleaseCheck = true, time.Now()
	results := b.releaseResults
	go func() {
		version, err := update.Check()
		results <- releaseCheckResult{version: version, err: err, chatID: chatID, action: action, requestID: requestID}
	}()
	if chatID != 0 {
		send(b.tg, chatID, "Checking the latest stable release…")
	}
}

func (b *Bot) finishReleaseCheck(checked releaseCheckResult) {
	b.checkingRelease = false
	if checked.err != nil {
		slog.Error("checking release availability", "err", checked.err)
		if checked.chatID != 0 {
			send(b.tg, checked.chatID, "Could not check GitHub releases. No update request was saved: "+checked.err.Error())
		}
		return
	}
	b.latestVersion = checked.version
	if b.migrating || b.ownerPaused || b.deploymentPaused() {
		if checked.chatID != 0 {
			send(b.tg, checked.chatID, "A configuration change is in progress. No update request was saved; retry /update afterward.")
		}
		return
	}
	request := config.UpdateRequest{ID: checked.requestID, Version: checked.version}
	if checked.action == "pc" || checked.action == "all" {
		if b.updater.Role == "server" {
			b.queueUpdates(&request, func(result storage.OperationResult) {
				if result.Err != nil {
					send(b.tg, checked.chatID, "Could not save the poker-PC update: "+result.Err.Error())
					return
				}
				state := result.Updates
				if state.Request != nil {
					send(b.tg, checked.chatID, "Poker-PC update request saved for "+state.Request.Version+". It will run when the PC connects and PokerStars is closed.\n"+collectorUpdateText(state))
				}
				if checked.action == "all" {
					b.requestLocalUpdate(checked.chatID, request)
				}
			})
			return
		}
	}
	if checked.action == "pc" || checked.action == "all" || checked.action == "server" {
		b.requestLocalUpdate(checked.chatID, request)
		return
	}
	b.queueUpdates(nil, func(result storage.OperationResult) {
		b.observeUpdates(result)
		if checked.chatID != 0 {
			b.renderUpdateMenu(checked.chatID, result)
		}
	})
}

func (b *Bot) requestLocalUpdate(chatID int64, request config.UpdateRequest) {
	report, err := b.updater.Request(request)
	if err != nil {
		send(b.tg, chatID, "Could not save this installation's update request: "+err.Error())
		return
	}
	label := "Server"
	if b.updater.Role == "combined" {
		label = "This PC"
	}
	send(b.tg, chatID, label+" update request saved.\n"+updateJobText(label, report.Job))
}

func updateJobText(label string, job *config.UpdateJob) string {
	if job == nil {
		return label + ": no update requested."
	}
	prefix := label + " → " + job.Request.Version + ": "
	switch job.State {
	case config.UpdateQueued:
		return prefix + "queued."
	case config.UpdateWaiting:
		return prefix + "waiting for PokerStars to close."
	case config.UpdateRunning:
		return prefix + "updating; waiting for confirmed startup."
	case config.UpdateSucceeded:
		return prefix + "completed (" + job.Version + ")."
	case config.UpdateFailed:
		return prefix + "failed: " + job.Error + ". Check this machine's tracker.log and use /update to try again."
	default:
		return prefix + "status unavailable."
	}
}

func collectorUpdateText(state config.CollectorUpdates) string {
	if state.Error != "" {
		return "Poker-PC update could not be accepted: " + state.Error
	}
	if state.Request != nil && (state.Report.Job == nil || state.Report.Job.Request != *state.Request) {
		return "Poker PC → " + state.Request.Version + ": saved; awaiting PC confirmation."
	}
	return updateJobText("Poker PC", state.Report.Job)
}

func (b *Bot) observeUpdates(result storage.OperationResult) {
	if result.Err != nil {
		slog.Error("reading collector update status", "err", result.Err)
		return
	}
	report, err := b.updater.Report()
	if err != nil {
		slog.Error("reading local update status", "err", err)
		return
	}
	path := filepath.Join(filepath.Dir(b.configPath), "update_notices.json")
	notices, err := config.LoadUpdateNotices(path)
	if err != nil {
		slog.Error("reading update notices", "err", err)
		return
	}
	chatID, ok := configuredChatID(b.configPath)
	if !ok {
		return
	}
	label := "Server"
	if b.updater.Role == "combined" {
		label = "This PC"
	}
	for _, item := range []struct {
		label   string
		job     *config.UpdateJob
		receipt *string
	}{
		{label, report.Job, &notices.Local}, {"Poker PC", result.Updates.Report.Job, &notices.Collector},
	} {
		if b.updater.Role == "combined" && item.label == "Poker PC" {
			continue
		}
		if item.job == nil {
			continue
		}
		key := item.job.Request.ID + ":" + item.job.State + ":" + item.job.Error
		if *item.receipt == key {
			continue
		}
		if _, err := b.tg.Send(tgbotapi.NewMessage(chatID, updateJobText(item.label, item.job))); err != nil {
			slog.Error("sending update result", "err", err)
			continue
		}
		*item.receipt = key
		if err := config.SaveUpdateNotices(path, notices); err != nil {
			slog.Error("saving update notice receipt", "err", err)
			return
		}
	}
	if releaseNoticeNeeded(b.latestVersion, notices.Release, report.Version, result.Updates.Report.Version) {
		text := "Poker Tracker " + b.latestVersion + " is available.\nUse /update to update your installation(s); an offline poker PC keeps its saved request until it connects, and waits for PokerStars to close.\nhttps://github.com/vit-ui/poker-tracker/releases/tag/" + b.latestVersion
		if _, err := b.tg.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
			slog.Error("sending release notice", "err", err)
			return
		}
		notices.Release = b.latestVersion
		if err := config.SaveUpdateNotices(path, notices); err != nil {
			slog.Error("saving release notice receipt", "err", err)
		}
	}
}

func releaseNoticeNeeded(latest, notified string, installed ...string) bool {
	if notified != "" {
		newer, err := update.Newer(latest, notified)
		if err != nil || !newer {
			return false
		}
	}
	for _, current := range installed {
		if newer, err := update.Newer(latest, current); err == nil && newer {
			return true
		}
	}
	return false
}
