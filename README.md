# Poker Tracker

A Windows background service that automatically tracks a poker player's sessions on PokerStars, maintains bankroll accounting in Google Sheets, and provides a Telegram bot for alerts and controls. It starts on boot and runs with zero manual interaction.

## What it does

- Watches PokerStars' Hand History and Tournament Summary folders and parses every cash session and tournament (Spin & Go and MTT) as it's written.
- Logs everything to a Google Sheet: cash sessions, tournaments, a running bankroll ledger, and time-of-day/day-of-week breakdowns.
- Sends a Telegram summary after every session, plus configurable alerts for downswings, losing streaks, and daily loss thresholds.
- Can automatically lock PokerStars (kill the client for a configured duration) when a hard-stop condition is triggered — a session loss, a rolling drawdown, a losing streak, or a daily loss limit.
- Everything is configured through the Telegram bot — no config files to hand-edit, no dashboard to log into.

## Installing

Download the latest `Setup.exe` from [Releases](../../releases) and run it. The installer offers two setup paths:

- **Telegram (recommended)** — the installer launches the bot, and you complete setup entirely by chatting with it: service account credentials, Google Sheet, PokerStars screen name, and hand history / tournament summary folders.
- **Manual** — enter the same information directly in the installer if you'd rather not use Telegram for initial setup.

Either way, the service registers itself in Task Scheduler and starts automatically, including on every subsequent login.

### Requirements

- Windows, with PokerStars already installed.
- A Google Cloud service account with access to the Sheets and Drive APIs, and a blank Google Sheet shared with it (Editor access).
- A Telegram bot token (create one via [@BotFather](https://t.me/BotFather)) — only needed if you're building from source; the official release has one built in.

## Using it

Once installed, talk to your bot on Telegram. A few starting points:

| Command | What it does |
|---|---|
| `/start` | Runs the full setup wizard (paths, alerts, hard stop rules). Re-runnable any time. |
| `/status` | Current bankroll, today/week/month P&L, and any active lock. |
| `/lastsession` | Summary of the most recent session. |
| `/stats` | Filtered stats — by game type and time range. |
| `/deposit`, `/withdraw` | Log a bankroll transfer. |
| `/config` | Lists all configuration commands (`/paths`, `/alerts`, `/hardstop`, `/schedule`). |
| `/help` | Full command reference. |

Every command has detailed help via `/help <command>`.

## Building from source

```bash
go build ./...          # build
go test ./...            # run tests

# release build, with the bot token injected at compile time
GOOS=windows GOARCH=amd64 go build \
  -ldflags "-H=windowsgui -X main.BotToken=${TELEGRAM_BOT_TOKEN}" \
  -o tracker.exe ./cmd/tracker
```

Pushing a `v*` tag triggers `.github/workflows/release.yml`, which builds `tracker.exe`, packages it with `setup.iss` (Inno Setup) into `Setup.exe`, and publishes both to a GitHub Release.

Run `tracker.exe --setup` to walk through setup manually, or `tracker.exe --serve` to run the service directly, from an existing terminal.

## Project layout and internals

See [`CLAUDE.md`](CLAUDE.md) for the full technical reference — architecture, data models, config schema, coding conventions, and the complete build order. [`poker-tracker-plan.md`](poker-tracker-plan.md) has the original design plan.

This is a single-user tool built for one specific setup. It's intentionally lean — no database, no web interface, no retry logic beyond what the underlying libraries provide. Google Sheets is the data store; Telegram is the only interface besides the installer.
