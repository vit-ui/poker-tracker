package bot

// /config and /help: static command descriptions.

import (
	"fmt"
	"strings"
)

const configText = "/paths — set PokerStars hand history and tournament folder locations\n" +
	"/alerts — configure notification thresholds (downswing, losing streak, Spin & Go drain)\n" +
	"/hardstop — configure automatic hard stop rules and lock duration\n" +
	"/schedule — set your poker day start hour for daily loss tracking\n" +
	"/notify — choose which summaries and alerts the bot sends"

var commandList = []struct {
	name string
	desc string
}{
	{"start", "Run the full setup wizard (paths, alerts, hard stop, schedule)."},
	{"deposit", "Log a deposit to your bankroll ledger."},
	{"withdraw", "Log a withdrawal from your bankroll ledger."},
	{"status", "Show current bankroll and P&L."},
	{"lastsession", "Show the summary of the last recorded session."},
	{"stats", "Show filtered performance stats."},
	{"sheet", "Get the Google Sheets URL, or switch to a different sheet."},
	{"storage", "View or switch where your data is stored."},
	{"export", "Export all your data as CSV files."},
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
	"help": "Use /help alone to list all commands. Use /help <command> to get details on a specific one. Example: /help stats",
	"stats": "Shows performance stats.\n" +
		"Usage: /stats [cash|spins|mtt] [today|week|month|all]\n" +
		"Both arguments are optional. Default is all-time stats for all game types.\n" +
		"Examples:\n" +
		"  /stats — all-time, all game types\n" +
		"  /stats cash — cash game stats, all time\n" +
		"  /stats spins week — Spin & Go stats for the past 7 days\n" +
		"  /stats mtt month — MTT stats for this calendar month\n" +
		"  /stats cash month — cash game stats for this calendar month",
	"deposit": "Logs a deposit to your bankroll ledger.\n" +
		"Usage: /deposit <amount>\n" +
		"Enter the USD amount shown on PokerStars after the deposit clears — not BRL.\n" +
		"Example: /deposit 50.00",
	"withdraw": "Logs a withdrawal from your bankroll ledger.\n" +
		"Usage: /withdraw <amount>\n" +
		"Enter the USD amount shown on PokerStars — not BRL.\n" +
		"Example: /withdraw 20.00",
	"hardstop": "Configures hard stop rules or triggers one manually.\n" +
		"Usage (configure): /hardstop — opens the settings menu\n" +
		"Usage (manual trigger): /hardstop trigger — activates a hard stop immediately\n" +
		"Fields: session loss (buy-ins), downswing (buy-ins, rolling 30-day peak), consecutive losses (sessions), daily loss (all games, $), Spin & Go daily loss ($), duration (hours).\n" +
		"The hard stop closes PokerStars and blocks the launcher for the configured duration.\n" +
		"Duration must be configured before using /hardstop trigger.\n" +
		"A hard stop threshold can never be set lower than its corresponding alert threshold, if that alert is configured.",
	"alerts": "Configures notification alert thresholds.\n" +
		"Usage: /alerts — opens the settings menu\n" +
		"Fields: downswing (buy-ins, rolling 30-day peak), session loss (buy-ins), losing streak (sessions), daily loss (all games, $), Spin & Go daily loss ($).\n" +
		"Alerts fire automatically and require no further action from you.\n" +
		"An alert threshold can never be set higher than its corresponding hard stop threshold, if that hard stop is configured.",
	"lastsession": "Shows the summary of the last recorded session (cash or Spin & Go).\n" +
		"Usage: /lastsession\n" +
		"No arguments.",
	"start": "Runs the full setup wizard: /paths, then /alerts, then /hardstop, then /schedule.\n" +
		"Usage: /start\n" +
		"Re-runnable at any time.",
	"status": "Shows current bankroll and profit/loss.\n" +
		"Usage: /status\n" +
		"No arguments.",
	"sheet": "With no argument, returns the URL of your Google Sheets tracker. With an argument, switches to a different sheet — send its name (resolved via Drive) or paste its full URL. Validates access before saving; requires a restart of the tracker service to take effect.\n" +
		"Usage: /sheet [name or URL]\n" +
		"Examples:\n" +
		"  /sheet — show the current sheet URL\n" +
		"  /sheet My Other Bankroll — switch by name\n" +
		"  /sheet https://docs.google.com/spreadsheets/d/... — switch by URL",
	"storage": "Shows where your data is stored, or migrates your entire history to the other backend and restarts the tracker on it.\n" +
		"Usage:\n" +
		"  /storage — show the current storage\n" +
		"  /storage local — switch to the local file\n" +
		"  /storage sheets <sheet name or URL> — switch to Google Sheets (an empty sheet shared with your service account; run /credentials first if this install has no credentials yet)\n" +
		"The target must be empty — there's no merge. You'll see a confirmation with row counts before anything is copied; close PokerStars first so no hand is played mid-switch. Nothing changes until the copy is verified.",
	"export": "Exports all your data as CSV files: cash sessions, tournaments, bankroll ledger, time analysis.\n" +
		"Usage: /export\n" +
		"No arguments. Works on both storage backends and produces identical output either way. Sends four files, one per data set — an empty data set still sends a header-only file.",
	"credentials": "Swaps the Google service account credentials file used to access Sheets/Drive.\n" +
		"Usage: /credentials — then send the google_service_account.json file as an attachment, or type skip to cancel.\n" +
		"If a sheet is already configured, it's re-checked against the new credentials — if it's no longer reachable (not shared with the new service account, or a different Google Cloud project entirely), the sheet is cleared from config and you'll need to run /sheet again.\n" +
		"Requires a restart of the tracker service to take effect.\n" +
		"On the local-file backend, this is how you provide the credentials /storage needs before migrating to Google Sheets.",
	"screenname": "Gets or changes your PokerStars screen name (exact capitalisation matters).\n" +
		"Usage: /screenname [name]\n" +
		"With no argument, shows the current screen name. With an argument, changes it and restarts the watcher immediately — no service restart needed.\n" +
		"Examples:\n" +
		"  /screenname — show the current screen name\n" +
		"  /screenname NewHandle99 — change it",
	"paths": "Configures the PokerStars hand history and tournament summary folder locations.\n" +
		"Usage: /paths — opens the settings menu\n" +
		"Enter the base folder only — not the folder with your screen name in it.",
	"config": "Lists the available configuration commands.\n" +
		"Usage: /config\n" +
		"No arguments.",
	"notify": "Chooses which messages the bot sends you.\n" +
		"Usage: /notify — opens the notifications menu\n" +
		"Two sections:\n" +
		"  Summaries — the cash, tournament, weekly, and monthly result summaries. Each can be set to Every, Only wins, Only losses, or Off. Stake-change notices are a plain On/Off.\n" +
		"  Alerts — the warning thresholds (same as /alerts).\n" +
		"For weekly and monthly, wins/losses is judged on the whole period's net.",
	"schedule": "Sets the hour your poker day starts, used for the daily loss\n" +
		"hard stop and alert thresholds.\n" +
		"Usage: /schedule — opens the settings menu\n" +
		"Default is 6 (6am local time) if never set.\n" +
		"Example: set to 6 if you never play before 6am.",
}

func helpText(arg string) string {
	arg = strings.TrimSpace(strings.ToLower(arg))
	if arg == "" {
		var b strings.Builder
		b.WriteString("Available commands:\n")
		for _, c := range commandList {
			fmt.Fprintf(&b, "/%s — %s\n", c.name, c.desc)
		}
		return strings.TrimRight(b.String(), "\n")
	}
	arg = strings.TrimPrefix(arg, "/")
	if text, ok := commandHelp[arg]; ok {
		return text
	}
	return fmt.Sprintf("No help available for %q.", arg)
}
