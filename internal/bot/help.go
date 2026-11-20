package bot

// /config and /help: static command descriptions.

import (
	"fmt"
	"strings"
)

const configText = "/menu — open the full menu (settings, notifications, info, money)\n" +
	"/paths — set PokerStars hand history and tournament folder locations\n" +
	"/alerts — configure notification thresholds (downswing, losing streak, Spin & Go drain)\n" +
	"/hardstop — configure automatic hard stop rules and lock duration\n" +
	"/schedule — set your poker day start hour for daily loss tracking\n" +
	"/notify — choose which summaries and alerts the bot sends"

var commandList = []struct {
	name string
	desc string
}{
	{"menu", "Open the full menu: settings, notifications, info, money."},
	{"update", "Check versions and queue updates for the poker PC or server."},
	{"deposit", "Log a deposit to your bankroll ledger."},
	{"withdraw", "Log a withdrawal from your bankroll ledger."},
	{"reward", "Record chest or promotional money in your bankroll."},
	{"transactions", "Show deposits, withdrawals and rewards; optional history."},
	{"status", "Show current bankroll and P&L."},
	{"lastsession", "Show the summary of the last recorded session."},
	{"stats", "Show filtered performance stats."},
	{"sheet", "Get the Google Sheets URL, or switch to a different sheet."},
	{"storage", "View or switch where your data is stored."},
	{"export", "Export all your data as CSV files."},
	{"scan", "Backfill: record everything in your folders not stored yet."},
	{"credentials", "Swap the Google service account credentials file."},
	{"screenname", "Get or change your PokerStars screen name."},
	{"paths", "Configure PokerStars folder locations."},
	{"alerts", "Configure alert thresholds."},
	{"hardstop", "Configure hard stop rules, or trigger one manually."},
	{"schedule", "Set your poker day start hour."},
	{"notify", "Choose which summaries and alerts the bot sends."},
	{"config", "List configuration commands."},
	{"help", "Show this help, or details on one command."},
}

var commandHelp = map[string]string{
	"help": `Help

/help — list all commands
/help <command> — show instructions

Example: /help update`,
	"update": `Updates

1. Open /update and choose the poker PC, server or both.
2. Wait for the bot to confirm the request was saved.
3. Close PokerStars to let the poker PC update.

Shortcuts
/update pc — update the poker PC
/update server — update the server (split setup)
/update all — update both
/update check — check for a new release

What happens next
• The PC waits for PokerStars to close; offline PCs wait until they reconnect. No further command is needed.
• The server updates independently. Requests keep the selected release.
• The bot reports success or failure after restart.
• Settings, credentials, history, pairing and lock expiry are kept.

Checks run at startup and daily; you get one notice per new version. Updates install only when requested.

If the bot is offline, wait for its saved-request confirmation. Telegram keeps unreceived commands for up to 24 hours.

Older installs or terminal commands:
/help update terminal

Reading /help update does not start an update.`,
	"update terminal": `Update from a terminal

Close PokerStars before updating the poker PC.
On Windows, open PowerShell in the installation folder (default: C:\poker-tracker).

Poker PC:
.\tracker.exe --update

Windows server:
.\tracker-server.exe --update

Standalone collector named tracker-client.exe:
.\tracker-client.exe --update

Accept the administrator prompt if asked.

Linux server (run as the user who installed it):
~/.local/share/poker-tracker/tracker-server --update

After running
• Wait for "Updated ..." or "Already up to date".
• Replace --update with --version to check the installed version.
• In split mode, update both machines separately to matching versions.

The command installs the latest stable release for this machine's role and CPU. Settings, credentials, history, pairing and lock expiry are kept. Running installations restart; stopped ones stay stopped.

Older installs
Versions 4.8/4.9 need a one-time manual installer upgrade in the existing folder. If --update is unrecognized, install the current release there first:
https://github.com/vit-ui/poker-tracker/releases/latest

For Telegram updates: /help update`,
	"stats": `Performance stats

/stats [cash|spins|mtt] [today|week|month|all]
Both filters are optional. Default: all games, all time.

Examples
/stats — all games, all time
/stats cash — cash games, all time
/stats spins week — Spin & Go, past 7 days
/stats mtt month — MTTs, current calendar month`,
	"deposit": `Log a deposit

/deposit <amount>
Example: /deposit 50.00

Use the USD amount shown on PokerStars after the deposit clears, not BRL.`,
	"reward": `Record a reward

/reward <amount>
Example: /reward 2.50

Enter chest or promotional money in USD. Rewards increase bankroll without changing game results or loss limits.`,
	"transactions": `Transactions

/transactions [today|week|month|all] — separate totals and net bankroll change
/transactions history [period] — totals and every movement, with Previous/Next pages

The default period is all. Today starts at midnight, week means the last seven days, and month starts on the first. History is shown only when requested.
Example: /transactions history month`,
	"withdraw": `Log a withdrawal

/withdraw <amount>
Example: /withdraw 20.00

Use the USD amount shown on PokerStars, not BRL.`,
	"hardstop": `Hard stops

/hardstop — configure rules and duration
/hardstop now — activate immediately using the saved duration
/hardstop now 10h — lock for 10 hours this time
/hardstop now 1d — lock for one day this time

Overrides accept whole hours, days or weeks (10, 10h, 1d, 1w). They work without a saved duration and do not change automatic settings. An existing longer lock cannot be shortened.

Limits
• Session loss: buy-ins
• Downswing: buy-ins below the rolling 30-day peak
• Losing streak: consecutive losing sessions
• Daily loss: USD across all games
• Spin & Go daily loss: USD
• Duration: e.g. 12, 48h, 2d or 1w

A hard stop closes PokerStars and blocks the launcher for the full duration. Changing the duration keeps an active lock's original expiry.

Each hard stop limit must be at least its matching alert limit.`,
	"alerts": `Alerts

/alerts — configure warning limits

Limits
• Downswing: buy-ins below the rolling 30-day peak
• Session loss: buy-ins
• Losing streak: consecutive losing sessions
• Daily loss: USD across all games
• Spin & Go daily loss: USD

Alerts are sent automatically. Each alert limit must be at most its matching hard stop limit.`,
	"lastsession": `/lastsession

Show the last recorded cash or Spin & Go session summary.`,
	"menu": `Main menu

/menu — open the full menu

• Setup — folders, schedule, screen name, storage, sheet (Sheets only) and credentials
• Notifications — summaries and alerts
• Hard stop — rules and duration
• Info — status, last session, stats, export and updates
• Money — deposits, withdrawals, rewards and transaction reports
• Help — command list

Tap Back to go up a level or Done to close a root menu.
For update instructions: /help update`,
	"status": `/status

Show your bankroll and profit/loss for today, this week and this month.`,
	"sheet": `Google Sheet

/sheet — show the current sheet URL
/sheet <name or URL> — switch sheets

Examples
/sheet My Other Bankroll
/sheet https://docs.google.com/spreadsheets/d/...

Sheets storage only. Access is checked before saving; the tracker then restarts automatically.`,
	"storage": `Data storage

/storage — show current storage
/storage local — switch to a local file
/storage sheets <name or URL> — switch to Google Sheets

Before switching
• Close PokerStars.
• The destination must be empty; existing data cannot be merged.
• For Sheets, share the sheet with your service account. If credentials are missing, the bot asks for google_service_account.json.

The bot shows row counts and asks for confirmation before copying. It verifies the full history, then switches storage and restarts.`,
	"export": `Export data

/export — download three CSV files

• Cash sessions
• Tournaments
• Bankroll ledger

Works with local storage and Google Sheets. Empty data sets still include column headers.`,
	"scan": `Import existing history

/scan — scan your PokerStars folders
/scan file — scan one .txt export by poker-PC path or Telegram upload (up to 20 MB)

• Safe to repeat: saved sessions are not duplicated.
• No session summaries, alerts or hard stops during import.
• Tournaments needing a ticket/cash or buy-in answer are skipped and counted; play or reopen them live to record them.
• One message reports the totals when the scan finishes.

A large first import to Google Sheets may need a few minutes to sync. Run /scan again after syncing for the full totals.`,
	"credentials": `Google credentials

1. Send /credentials.
2. Attach google_service_account.json, or type skip to cancel.

With Sheets storage, the bot checks access to the current sheet before replacing the key and restarting. Failed validation keeps the old key and sheet; a failed restart helper restores the old key.

With local storage, this supplies credentials for a later switch using /storage sheets <name or URL>.`,
	"screenname": `PokerStars screen name

/screenname — show the last confirmed name and any pending change
/screenname NewHandle99 — change your name (exact capitalisation matters)

Changes are queued until the poker PC validates and confirms them, then collection restarts with the new name.`,
	"paths": `PokerStars folders

/paths — configure hand history and tournament summary locations

• Enter the base folder, without the screen-name subfolder.
• Relative paths start at the active Windows desktop user's home. Use a full path if no desktop user is available.
• Changes are queued until the poker PC validates and confirms them.`,
	"config": `/config

List the configuration commands. Use /menu to open the full menu.`,
	"notify": `Notifications

/notify — choose which messages you receive

Summaries
• Cash, tournament, weekly and monthly results: Every, Only wins, Only losses or Off
• Stake changes: On or Off
• Weekly/monthly wins and losses use the whole period's net result.

Alerts
Configure warning limits, also available through /alerts.`,
	"schedule": `Poker day start

/schedule — set the start hour for daily loss alerts and hard stops

Default: 6 (6am local time).
Example: choose 6 if you never play before 6am.`,
}

func helpText(arg string) string {
	arg = strings.TrimSpace(strings.ToLower(arg))
	if arg == "" {
		var b strings.Builder
		b.WriteString("Available commands:\n")
		for _, c := range commandList {
			fmt.Fprintf(&b, "/%s — %s\n", c.name, c.desc)
		}
		b.WriteString("\nDetails: /help <command>\nExample: /help update\n")
		return strings.TrimRight(b.String(), "\n")
	}
	arg = strings.TrimPrefix(arg, "/")
	if text, ok := commandHelp[arg]; ok {
		return text
	}
	return fmt.Sprintf("No help available for %q.", arg)
}
