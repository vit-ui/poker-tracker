# Poker Tracker

A Windows background service that automatically tracks your PokerStars sessions, maintains your bankroll accounting, and exposes it all through a Telegram bot: session alerts, stats, and a hard-stop lock that closes PokerStars when you go on tilt.

It starts on boot via Task Scheduler and runs unattended. PokerStars writes hand histories and tournament summaries to disk; the tracker parses them as they land, and you talk to it entirely through Telegram. Your data lives in **either a Google Sheet or a local SQLite file**, whichever you pick at install.

## Highlights

- **Automatic parsing** of cash games, Spin & Go, and MTT sessions, read straight from PokerStars' hand-history and tournament-summary folders. Nothing is entered by hand.
- **Your choice of storage:** a live **Google Sheet** you can open in a browser, or a single local **SQLite file** that needs no Google account and no configuration.
- **Telegram summaries** after every session, plus configurable alerts for downswings, session losses, losing streaks, and daily-loss limits. It also points out when you sit at a different stake than your previous cash session.
- **Automatic hard stop.** When a limit you set is breached (a session loss, a rolling 30-day drawdown, a losing streak, or a daily-loss cap), it closes the PokerStars client and keeps it locked for a set duration.
- **Satellite / ticket detection** flags tournaments likely entered with a ticket and asks you to confirm, so a free entry never counts as a cash buy-in against your bankroll.
- **One running ledger** for deposits, withdrawals, and every session net, so your true P&L stays correct through reloads.
- **CSV export** on both backends, in identical format, so your data stays portable whichever storage you chose.

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

## Using it

Once installed, talk to your bot on Telegram. Every command also has detailed help via `/help <command>`. Expand any entry below for the details:

<details>
<summary><code>/start</code> — run the full setup wizard</summary>

Chains the four configuration flows in order — paths → alerts → hard stop → schedule — pressing Done on each to advance. Re-runnable any time to reconfigure.
</details>

<details>
<summary><code>/status</code> — current bankroll and P&L</summary>

Shows your current bankroll, today / this week / this month P&L, and details of any active hard-stop lock.
</details>

<details>
<summary><code>/lastsession</code> — summary of your most recent session</summary>

Full breakdown of the last session parsed: game type, stake or buy-in, net result, hands or finish position, duration, and updated bankroll.
</details>

<details>
<summary><code>/stats [cash|spins|mtt] [today|week|month|all]</code> — filtered statistics</summary>

Volume, P&L, and win rates, filtered by game type and time range. `spins` covers Spin & Go, `mtt` covers multi-table tournaments. With no arguments it reports all game types, all-time.

Example: `/stats cash week` — cash-game stats for the past week.
</details>

<details>
<summary><code>/deposit &lt;amount&gt;</code> / <code>/withdraw &lt;amount&gt;</code> — log a bankroll transfer</summary>

Records money moving into or out of PokerStars, using the USD value shown on PokerStars. Replies with your new bankroll. Transfers are kept separate from session results in the ledger, so a reload adjusts your bankroll without ever counting as winnings.

Example: `/deposit 100`
</details>

<details>
<summary><code>/export</code> — download all your data as CSV</summary>

Exports the four data sets — cash sessions, tournaments, the bankroll ledger, and time analysis — as four CSV files sent to you as Telegram documents. Output is identical on both the Google Sheets and SQLite backends.
</details>

<details>
<summary><code>/sheet [name or URL]</code> — link to your data, or switch spreadsheets</summary>

With no argument, replies with the spreadsheet URL. With a sheet name or a pasted URL, switches the tracker to that spreadsheet — access is validated before anything is saved, then the service restarts itself to pick up the change. On the local-file backend there is no sheet; the command points you to `/export` instead.
</details>

<details>
<summary><code>/screenname [name]</code> — view or change your PokerStars screen name</summary>

With no argument, shows the configured screen name. With one, saves it and restarts the file watcher immediately, since the watched folders include the screen name as a subfolder. Capitalisation matters.
</details>

<details>
<summary><code>/credentials</code> — swap the Google service account key</summary>

Google Sheets backend only. Send the new `google_service_account.json` as an attachment; the current sheet is re-checked against it, and if the new account can't reach it you're prompted to reconnect one with `/sheet`. Takes effect after a service restart.
</details>

<details>
<summary><code>/paths</code> — set your PokerStars folders</summary>

Configures the base Hand History and Tournament Summary folders (the parent folders — not the screen-name subfolder underneath, which is added automatically). Required before any session can be tracked; saving restarts the file watcher.
</details>

<details>
<summary><code>/alerts</code> — configure warning thresholds</summary>

Set the warning-only thresholds: downswing, session loss, losing streak, daily loss, and Spin & Go daily loss. These warn you before the hard stop steps in.
</details>

<details>
<summary><code>/hardstop</code> — configure the automatic lock</summary>

Set the rules that close PokerStars and lock it for a fixed duration: session loss, rolling drawdown, consecutive losses, daily loss, Spin & Go daily loss, and how long the lock lasts. Send `/hardstop trigger` to lock immediately, by hand.
</details>

<details>
<summary><code>/schedule</code> — set your poker-day boundary</summary>

Sets the hour (0–23) your poker day starts, used as the reset point for the daily-loss checks. Defaults to 6am.
</details>

<details>
<summary><code>/config</code> — list the configuration commands</summary>

Lists `/paths`, `/alerts`, `/hardstop`, and `/schedule` with a one-line description of each.
</details>

<details>
<summary><code>/help [command]</code> — command reference</summary>

With no argument, lists every command. With a command name, explains it in detail with parameters and an example.
</details>

## Notes

This is a single-user tool built for one specific personal setup, and it's intentionally lean: no web UI, no REST API, no dashboard. Telegram and the installer are the only interfaces. It's provided as-is, with no warranty, and is not affiliated with or endorsed by PokerStars.

## License

© 2026 Vitor Cipriano. Released under the [PolyForm Strict License 1.0.0](LICENSE): free for personal, noncommercial use, but you may not redistribute, modify, or use it commercially. See [`LICENSE`](LICENSE) for the full terms.
