// Package bot implements the Telegram bot: outgoing session alerts and
// incoming commands for configuration and status queries.
package bot

import (
	"github.com/vit-ui/self-tracker/internal/sheets"
)

// Run starts the bot in serve mode. Forwards alerts from alertChan to Telegram
// and polls for incoming commands. Blocks until the process exits.
// Implemented in step 5 of the build order.
func Run(token string, alertChan <-chan string, sc *sheets.Client, configPath, lockfilePath string, restartWatcher func()) {
}

// RunSetup starts the bot in setup mode and blocks until the user completes
// the configuration wizard via Telegram. Implemented in step 8 of the build order.
func RunSetup(token, configPath, serviceAccountPath string) {
}
