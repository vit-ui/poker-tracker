package bot

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/vit-ui/self-tracker/internal/storage"
)

const maxScanAttachment = 20_000_000

func (b *Bot) renderScanMenu(chatID int64, messageID int, path string) {
	rows := [][]tgbotapi.InlineKeyboardButton{
		menuRow("Scan folders", "scan:folders"),
		menuRow("Scan a file on the poker PC", "scan:path"),
		menuRow("Upload a file", "scan:upload"),
		navButton(path),
	}
	b.showMenu(chatID, messageID, "Scan PokerStars history", rows)
}

func (b *Bot) handleScanChoice(chatID int64, messageID int, choice string) {
	b.reset()
	switch choice {
	case "folders":
		b.handleScanCommand(chatID)
	case "path", "upload":
		b.flow, b.field, b.menuMessageID = "scan", choice, messageID
		if choice == "path" {
			send(b.tg, chatID, "Send the path to the PokerStars .txt file on the poker PC, or type cancel.")
		} else {
			send(b.tg, chatID, "Attach a PokerStars .txt file up to 20 MB, or type cancel.")
		}
	}
}

func (b *Bot) handleScanReply(chatID int64, text string) {
	if strings.EqualFold(strings.TrimSpace(text), "cancel") {
		b.reset()
		send(b.tg, chatID, "Scan cancelled.")
		return
	}
	if b.field == "path" && strings.TrimSpace(text) != "" {
		b.reset()
		b.queueFileScan(chatID, strings.TrimSpace(text), "")
		return
	}
	send(b.tg, chatID, "Attach a PokerStars .txt file, or type cancel.")
}

func (b *Bot) handleScanAttachment(chatID int64, doc *tgbotapi.Document) {
	if !strings.EqualFold(filepath.Ext(doc.FileName), ".txt") || doc.FileSize > maxScanAttachment {
		send(b.tg, chatID, "Attach a PokerStars .txt file up to 20 MB, or use the poker-PC path option.")
		return
	}
	b.reset()
	if b.fileScanActive {
		send(b.tg, chatID, "A file scan is already in progress.")
		return
	}
	path, err := stageScanAttachment(b.tg, doc.FileID)
	if err != nil {
		send(b.tg, chatID, "Could not download the scan file: "+err.Error())
		return
	}
	b.queueFileScan(chatID, "", path)
}

func (b *Bot) queueFileScan(chatID int64, collectorPath, attachment string) {
	if b.fileScanActive {
		send(b.tg, chatID, "A file scan is already in progress.")
		return
	}
	b.fileScanActive = true
	b.queueOperation(nil, func(result storage.OperationResult) {
		b.fileScanActive = false
		if attachment != "" {
			os.Remove(attachment)
		}
		if result.Err != nil && result.Scan.Failed == 0 {
			send(b.tg, chatID, "Could not scan the file: "+result.Err.Error())
			return
		}
		send(b.tg, chatID, result.Scan.String())
	})
	request := &b.pendingOperations[len(b.pendingOperations)-1].request
	request.ScanPath, request.ScanAttachment = collectorPath, attachment
}

func stageScanAttachment(api *tgbotapi.BotAPI, fileID string) (string, error) {
	link, err := api.GetFileDirectURL(fileID)
	if err != nil {
		return "", fmt.Errorf("Telegram could not provide the file")
	}
	req, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		return "", fmt.Errorf("Telegram returned an invalid download URL")
	}
	resp, err := api.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Telegram file download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Telegram returned %s", resp.Status)
	}
	if resp.ContentLength > maxScanAttachment {
		return "", fmt.Errorf("use the poker-PC path option for files above 20 MB")
	}
	f, err := os.CreateTemp("", "poker-tracker-upload-*.txt")
	if err != nil {
		return "", fmt.Errorf("creating scan file: %w", err)
	}
	bytes, err := io.Copy(f, io.LimitReader(resp.Body, maxScanAttachment+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil || bytes == 0 || bytes > maxScanAttachment {
		os.Remove(f.Name())
		return "", fmt.Errorf("received an empty, oversized, or incomplete file")
	}
	return f.Name(), nil
}
