# Poker Tracker

A Windows background service that automatically tracks your PokerStars sessions, maintains your bankroll accounting, and exposes it all through a Telegram bot: session alerts, stats, and a hard-stop lock that closes PokerStars when you go on tilt.

It starts on boot via Task Scheduler and runs unattended. PokerStars writes hand histories and tournament summaries to disk; the tracker parses them as they land, and you talk to it entirely through Telegram. Your data lives in **either a Google Sheet or a local SQLite file**, whichever you pick at install — and you can move it between the two later with a single command.

## Highlights

- **Automatic parsing** of cash games, Spin & Go, and MTT sessions, read straight from PokerStars' hand-history and tournament-summary folders. Nothing is entered by hand.
- **Your choice of storage:** a live **Google Sheet** you can open in a browser, or a single local **SQLite file** that needs no Google account and no configuration.
- **Telegram summaries** after every session, weekly and monthly recaps, plus configurable alerts for downswings, session losses, losing streaks, and daily-loss limits. It also points out when you sit at a different stake than your previous cash session. Choose which of these fire — and filter each summary to only wins or only losses — from the `/notify` menu.
- **Sheets connectivity awareness** (Google Sheets backend only): if the tracker can't reach Sheets, you get one plain heads-up that it's briefly unreachable — your results are saved locally and synced automatically when it's back — a heartbeat reminder every 30 minutes while it's still down, and a confirmation once queued writes have synced. No raw error spam (the technical detail goes to `tracker.log`), and no silent data gaps to discover later.
- **Automatic hard stop.** When a limit you set is breached (a session loss, a rolling 30-day drawdown, a losing streak, or a daily-loss cap), it closes the PokerStars client and keeps it locked for a set duration.
- **Satellite / ticket detection** flags tournaments likely entered with a ticket and asks you to confirm, so a free entry never counts as a cash buy-in against your bankroll.
- **Freeroll / Kick-Off handling.** Tournaments with no buy-in in the summary (freerolls, Kick-Off ladders) still pay real knockout-bounty cash, so instead of dropping them the tracker asks whether you entered for free or paid — and records the bounty either way, so it always reaches your bankroll.
- **One running ledger** for deposits, withdrawals, and every session net, so your true P&L stays correct through reloads.
- **CSV export** on both backends, in identical format, so your data stays portable whichever storage you chose.
- **Switch storage anytime** with `/storage`, which migrates your whole history between Google Sheets and the local file and verifies the copy before flipping over.

## Installing

Download the installer for your machine from [Releases](../../releases) — `PokerTracker-v<version>-x64.exe` for Intel/AMD PCs, `PokerTracker-v<version>-arm.exe` for Windows on ARM — and run it. The installer walks you through two choices:

- **Storage backend**
  - **Google Sheets** — a live spreadsheet you can open in any browser. Requires a one-time Google Cloud service account.
  - **Local file** — everything lives in a single `tracker.db` next to the program. No Google account, nothing to share, nothing to configure.
- **Setup method**
  - **Telegram (recommended)** — the installer launches the bot and you finish setup by chatting with it.
  - **Manual** — enter the same details directly in the installer if you'd rather not use Telegram for the initial setup.

Either way, the service registers itself in Task Scheduler and starts automatically, including on every subsequent boot.

### Requirements

- Windows, with PokerStars already installed.
- **Only if you choose the Google Sheets backend:** a Google Cloud service account with access to the Sheets and Drive APIs, and a blank Google Sheet shared with it (Editor access). The local-file backend needs none of this.
- A Telegram bot token (create one via [@BotFather](https://t.me/BotFather)) — only if you're building from source; the official release has one built in.

## Running on two machines (optional)

By default everything runs on your poker PC, so `/stats`, `/export`, and the rest only work while that PC is on. If you'd rather keep your data and commands available when it's off, you can split the tracker across two machines:

- **Server** (`tracker-server`) — runs on any always-on machine (a home server, a Raspberry Pi, a VPS; Windows or Linux). It stores your data, runs the Telegram bot, and enforces your rules.
- **Client** (`tracker-client`) — runs on the poker PC (Windows). It watches PokerStars, streams file changes to the server, and enforces hard-stop locks.

Setup:

1. On the always-on machine, set up the server. **Linux:** extract `tracker-server-linux-<arch>.tar.gz` and run `./install.sh`. **Windows:** run `tracker-server.exe --setup`, then `tracker-server.exe --install` (as Administrator). Either way it prints a one-line **connection string** like `tracker://192.168.1.40:8443/<token>#fp=<fingerprint>`.
2. On the poker PC, run the Windows installer and choose **"Connect to a server"** (or run `tracker-client --setup`), then paste that connection string.

They talk over a single encrypted (TLS) connection the poker PC dials out to the server — only the poker PC needs to reach the server, and your phone always talks to Telegram, never the server directly. Configure storage, alerts, and thresholds from Telegram once paired. The all-local install above is unchanged; this is only if you want the split.

## Using it

Once installed, talk to your bot on Telegram. Every command also has detailed help via `/help <command>`. Expand any entry below for the details:

<details>
<summary><code>/menu</code> — the full menu</summary>

One place to reach everything, drilled into and back out of like BotFather — Back steps out one level, ✓ Done closes the menu. Groups: **Setup** (paths, schedule, screen name, storage, and — on the Google Sheets backend — sheet and credentials), **Notifications** (the `/notify` summaries and alerts), **Hard stop**, **Info** (status, last session, stats, export), **Money** (deposit, withdraw), and **Help**. Every item is also its own command, so you can skip the menu whenever you like.

Usage example: `/menu`
</details>

<details>
<summary><code>/status</code> — current bankroll and P&L</summary>

Shows your current bankroll, today / this week / this month P&L, and details of any active hard-stop lock.

Usage example: `/status`
</details>

<details>
<summary><code>/lastsession</code> — summary of your most recent session</summary>

Full breakdown of the last session parsed: game type, stake or buy-in, net result, hands or finish position, duration, and updated bankroll.

Usage example: `/lastsession`
</details>

<details>
<summary><code>/stats [cash|spins|mtt] [today|week|month|all]</code> — filtered statistics</summary>

Volume, P&L, and win rates, filtered by game type and time range. `spins` covers Spin & Go, `mtt` covers multi-table tournaments. With no arguments it reports all game types, all-time.

Usage examples:
- `/stats` — all game types, all-time
- `/stats cash week` — cash-game stats for the past 7 days
- `/stats spins today` — Spin & Go stats for the current poker day
- `/stats mtt month` — MTT stats for this calendar month
</details>

<details>
<summary><code>/deposit &lt;amount&gt;</code> / <code>/withdraw &lt;amount&gt;</code> — log a bankroll transfer</summary>

Records money moving into or out of PokerStars, using the USD value shown on PokerStars. Replies with your new bankroll. Transfers are kept separate from session results in the ledger, so a reload adjusts your bankroll without ever counting as winnings.

Usage examples:
- `/deposit 100`
- `/withdraw 52.75`
- `/deposit 12,50` — a comma decimal works too
</details>

<details>
<summary><code>/export</code> — download all your data as CSV</summary>

Exports the four data sets — cash sessions, tournaments, the bankroll ledger, and time analysis — as four CSV files sent to you as Telegram documents. Output is identical on both the Google Sheets and SQLite backends.

Usage example: `/export`
</details>

<details>
<summary><code>/sheet [name or URL]</code> — link to your data, or switch spreadsheets</summary>

Two jobs in one command: viewing and **switching spreadsheets**. With no argument, replies with the current spreadsheet's URL. With a sheet name or a pasted URL, switches the tracker to that spreadsheet — access is validated (and any missing tabs created) before anything is saved, then the service restarts itself to pick up the change. On the local-file backend there is no sheet; the command points you to `/export` instead.

Usage examples:
- `/sheet` — show the current spreadsheet URL
- `/sheet My Other Bankroll` — switch to a different sheet by name
- `/sheet https://docs.google.com/spreadsheets/d/…` — switch by pasted URL
</details>

<details>
<summary><code>/storage [local | sheets &lt;name or URL&gt;]</code> — view your storage, or migrate to the other backend</summary>

With no argument, shows where your data lives. `/storage local` moves everything to the local file; `/storage sheets <sheet name or URL>` moves everything to an empty Google Sheet shared with your service account (if this install has no service-account key yet, you'll be prompted to upload it right there and the migration continues). Naming the storage you're already on changes nothing. The target must be empty — there is no merge. You confirm a summary of row counts before anything is copied (close PokerStars first so no hand is played mid-switch), and the copy is verified against your current bankroll before the backend is flipped, so a failed migration leaves you exactly where you were.

Usage examples:
- `/storage` — show the active storage
- `/storage local` — migrate everything to the local file
- `/storage sheets My New Tracker` — migrate everything to that (empty) sheet
</details>

<details>
<summary><code>/screenname [name]</code> — view or change your PokerStars screen name</summary>

With no argument, shows the configured screen name. With one, saves it and restarts the file watcher immediately, since the watched folders include the screen name as a subfolder. Capitalisation matters.

Usage examples:
- `/screenname` — show the current screen name
- `/screenname NewHandle99` — change it
</details>

<details>
<summary><code>/credentials</code> — swap the Google service account key</summary>

Send the new `google_service_account.json` as an attachment, or type `skip` to keep the current one. On the Google Sheets backend the current sheet is re-checked against the new credentials, and if the new account can't reach it you're prompted to reconnect one with `/sheet`; takes effect after a service restart. On the local-file backend this is how you provide the credentials `/storage` needs before migrating to Google Sheets.

Usage example: `/credentials` — then attach the JSON file
</details>

<details>
<summary><code>/paths</code> — set your PokerStars folders</summary>

Configures the base Hand History and Tournament Summary folders (the parent folders — not the screen-name subfolder underneath, which is added automatically). Required before any session can be tracked; saving restarts the file watcher. Forward slashes are converted for you, and a path without a drive letter is resolved from your home folder.

Usage example: `/paths` — then tap a field and reply with the folder, e.g. `C:\Users\you\AppData\Local\PokerStars\HandHistory`
</details>

<details>
<summary><code>/alerts</code> — configure warning thresholds</summary>

Set the warning-only thresholds: downswing, session loss, losing streak, daily loss, and Spin & Go daily loss. These warn you before the hard stop steps in. An alert can never be set higher than its matching hard-stop threshold, when that hard stop is configured.

Tap a threshold to change its value or turn it off, then Back to step out — there's no Done button. Turning one off keeps its number (it just stops firing), so you can turn it back on later without retyping.

Usage example: `/alerts` — then tap a field and reply with the value, e.g. `5`
</details>

<details>
<summary><code>/hardstop</code> — configure the automatic lock</summary>

Set the rules that close PokerStars and lock it for a fixed duration: session loss, rolling drawdown, consecutive losses, daily loss, Spin & Go daily loss, and how long the lock lasts. The duration must be set if any rule is; a hard stop can never be set lower than its matching alert threshold. Each rule can be turned off (it keeps its number for later), but since it's a safety lock, turning one off asks you to confirm first. **There is no cancel — once triggered, the lock runs its full duration.**

Usage examples:
- `/hardstop` — open the settings menu
- `/hardstop trigger` — lock immediately, by hand (requires the duration to be configured)
</details>

<details>
<summary><code>/schedule</code> — set your poker-day boundary</summary>

Sets the hour (0–23) your poker day starts, used as the reset point for the daily-loss checks. Defaults to 6am.

Usage example: `/schedule` — then tap the field and reply `6`
</details>

<details>
<summary><code>/notify</code> — choose which messages the bot sends</summary>

Two sections. **Summaries**: the cash, tournament, weekly, and monthly result summaries — each can be set to send on **every** result, only **wins**, only **losses**, or **off**; stake-change notices are a plain on/off. (For weekly and monthly, wins/losses is judged on the whole period's net.) **Alerts**: the same warning thresholds as `/alerts`. Everything defaults to on, so nothing changes until you turn something down.

Usage example: `/notify`
</details>

<details>
<summary><code>/config</code> — list the configuration commands</summary>

Lists the configuration commands — `/menu`, `/paths`, `/alerts`, `/hardstop`, `/schedule`, and `/notify` — with a one-line description of each.

Usage example: `/config`
</details>

<details>
<summary><code>/help [command]</code> — command reference</summary>

With no argument, lists every command. With a command name, explains it in detail with parameters and an example.

Usage examples:
- `/help` — list every command
- `/help stats` — detailed help for `/stats`
</details>

## Notes

This is a single-user tool built for one specific personal setup, and it's intentionally lean: no web UI, no REST API, no dashboard. Telegram and the installer are the only interfaces. It's provided as-is, with no warranty, and is not affiliated with or endorsed by PokerStars.

The background service writes a `tracker.log` next to the program for troubleshooting — errors plus a few milestones (start-up, hard stops, migrations). It's capped at 5 MB and rolls over to a single `tracker.log.old`, so it never grows without bound.

## License

© 2026 Vitor Cipriano. Released under the [PolyForm Strict License 1.0.0](LICENSE): free for personal, noncommercial use, but you may not redistribute, modify, or use it commercially. See [`LICENSE`](LICENSE) for the full terms.
