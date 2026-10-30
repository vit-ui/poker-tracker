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

Telegram setup keeps your saved chat during reinstall and waits for a message from that chat. Messages from other chats cannot replace it. A fresh install still identifies your chat from the first message.

Windows upgrades keep saved preferences, credentials, pairing identity, history and pending cash/questions. Existing collector values are prefilled rather than replaced with empty answers. Unattended server setup respects `/SUPPRESSMSGBOXES`; interactive setup still displays its pairing information.


Download **`PokerTracker-Setup.exe`** from [Releases](../../releases) and run it — one installer for every PC (it detects your CPU, Intel/AMD or ARM, and installs the right build; there's no separate download to pick). The installer first asks **how you'd like to set up**:

- **Telegram (recommended)** — the installer launches the bot and you finish setup by chatting with it, **including choosing where your data lives**.
- **Manual** — set everything up in the installer instead, starting with the storage backend.

Your **storage backend** — asked in the chat on the Telegram path, or on its own page on the manual path — is either:

- **Google Sheets** — a live spreadsheet you can open in any browser. Requires a one-time Google Cloud service account.
- **Local file** — everything lives in a single `tracker.db` next to the program. No Google account, nothing to share, nothing to configure.

You can switch between them anytime later with `/storage`. Either way, the service registers itself in Task Scheduler and starts automatically, including on every subsequent boot.

Setup reads existing settings from the selected installation folder. When
replacing or uninstalling the program, it stops tracker processes belonging
to that folder. Builds for disposable installation tests use separate task
names and uninstall registration; see [Contributing](CONTRIBUTING.md#disposable-windows-installations).

Windows service tasks restart failures and check every minute for a stopped
process. A running copy stays in place. The collector keeps enforcing active
locks during server outages and reconnects itself every 30 seconds. Task recovery
handles unexpected process failures.

### Requirements

- Windows, with PokerStars already installed.
- **Only if you choose the Google Sheets backend:** a Google Cloud service account with access to the Sheets and Drive APIs, and a blank Google Sheet shared with it (Editor access). The local-file backend needs none of this.
- A Telegram bot token (create one via [@BotFather](https://t.me/BotFather)) — only if you're building from source; the official release has one built in.

## Running on two machines (optional)

By default everything runs on your poker PC, so `/stats`, `/export`, and the rest only work while that PC is on. If you'd rather keep your data and commands available when it's off, you can split the tracker across two machines:

- **Server** (`tracker-server`) — runs on any always-on machine (a home server, a Raspberry Pi, a VPS; Windows or Linux). It stores your data, runs the Telegram bot, and enforces your rules.
- **Client** (`tracker-client`) — runs on the poker PC (Windows). It watches PokerStars, streams file changes to the server, and enforces hard-stop locks.

Setup:

1. On the always-on machine, set up the server — one file per OS, download and run:
   - **Linux:** download `PokerTracker-Server-linux.tar.gz`, extract it (double-click in your file manager, or `tar xzf PokerTracker-Server-linux.tar.gz`), and run the extracted **`PokerTracker-Server-linux`**. It detects your CPU and installs the right build into your home folder — no root. A setup window opens (a terminal prompt instead on a headless box). It asks for your password once, in a normal desktop dialog, only to enable start-on-boot; skip that and the server still runs while you're logged in.
     - A fresh Google Sheets choice starts with SQLite so the bot can run before a Google key or sheet is connected. Then send `/storage sheets <sheet name or URL>` to upload the key and migrate to an empty shared sheet. Keep the poker PC connected and PokerStars closed for migration.
     - To upgrade, rerun the installer. It stages the new executable and replaces it atomically, preserves saved configuration and data, and restarts the installation’s user service. Existing storage choices stay in place; use `/storage` to change them with verified migration.
     - The setup window uses your desktop's standard dialog tool — **zenity** on GNOME and other GTK desktops (Ubuntu, Fedora, Mint, Xfce, MATE, Cinnamon…), **kdialog** on KDE Plasma (Kubuntu, openSUSE KDE…). On a desktop that has neither installed, or a headless box, setup falls back to **terminal prompts** — so run it from a terminal there rather than double-clicking. You can reprint the connection string anytime with `~/.local/share/poker-tracker/tracker-server --pair`.
   - **Windows:** run `PokerTracker-Setup.exe` and choose **"Run a server on this PC"**. It asks whether to configure from Telegram (recommended — defaults to a local file, switch anytime with `/storage`) or set the storage backend here now — the same choice the Linux server offers — then sets everything up.

   Either way it shows a one-line **connection string** like `tracker://192.168.1.40:8443/<token>#fp=<fingerprint>`.
2. On the poker PC, run `PokerTracker-Setup.exe` and choose **"Connect to a server"** (or run `tracker-client --setup`), then paste that connection string.

They talk over a single encrypted (TLS) connection the poker PC dials out to the server — only the poker PC needs to reach the server, and your phone always talks to Telegram, never the server directly. Configure storage, alerts, and thresholds from Telegram once paired.

Server settings (`/alerts`, `/hardstop` rules and duration, `/schedule`)
save immediately even while the poker PC is offline. An existing lock keeps its
full duration. `/paths` and `/screenname` edits are saved in a durable server queue:
the PC must confirm them before they take effect, because only it can validate
its folders, save local collection settings and restart its watcher. The bot
reports pending edits separately from the last PC-confirmed values. Connection
loss preserves pending edits across restart; a reported validation failure stays
pending until you send a corrected value. If saved folders are unavailable at
startup, collection stays inactive and the collector stays connected to accept
corrections; hard-stop enforcement continues.

The collector’s enforcement loop stays alive while the server is disconnected,
including when the server is unavailable at boot. It keeps checking an active
lock every second and preserves its original expiry while reconnecting. TLS,
certificate pinning and pairing authentication still apply to each connection.

Collector-confirmed hard-stop expiry survives server restarts and collector
disconnects and sent again when the collector reconnects. Reconnecting replaces
the previous connection; lock replay and ordinary commands use the same frame
writer. Folder and screen-name edits are durably queued; rescan requests remain
one-shot commands.
`/status` uses the collector’s observed protection state and reports when that
state is unavailable. Lock times follow the poker PC’s timezone.

Money is accounted for in whole USD cents. Extra fractional cents round up in
magnitude when entered: $1.001 becomes $1.01. Dollar displays and CSV columns
keep two decimal places.

New money movements carry an operation ID in the existing ledger identity column,
including CSV exports. Sheets recovery checks that ID before replaying a write.
Legacy ledger rows and fallback entries with blank IDs remain readable; their
recovery retains the older at-least-once behavior and may duplicate an append
whose acknowledgement was lost. Equal amounts are never merged.

Cash sessions are recorded after **2 minutes 30 seconds without a table-file
update**, including updates while you sit out. The tracker saves processed
pending cash snapshots in `pending_cash.json` beside its configuration. After
restart it restores each table's remaining interval and asks the collector for
the whole source file. An unchanged recovery upload keeps the original deadline;
later corrections update the same stored session. Keep the PokerStars histories:
they are the recovery source for bytes the tracker never received or processed.

Hard-stop requests and collector receipts are durable. Recovery of the same
trigger preserves its original expiry and cannot restart an expired lock;
distinct new financial triggers can still enforce. The collector retains active
protection independently of connectivity. Keep `lockfile.json` and
`hardstop_deliveries.json` with the installation when backing it up or upgrading.
While locked, it checks for a reopened PokerStars client every second and closes
it again. Startup and newly received locks enforce immediately; the idle check
remains every 30 seconds.
Use matching collector and server versions for recovery acknowledgements and
whole-file resynchronization; older peers do not support these operations.

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

Exports the three data sets — cash sessions, tournaments, and the bankroll ledger — as three CSV files sent to you as Telegram documents. Output is identical on both the Google Sheets and SQLite backends.

Usage example: `/export`
</details>

<details>
<summary><code>/scan</code> — backfill history already sitting in your folders</summary>

Walks both PokerStars folders and records every cash session and tournament that isn't stored yet — useful right after a new install to seed your existing history (the tracker otherwise only sees files that change while it's running). Records silently: no per-session summaries, no hard stops, no alerts. Tournaments that need an answer (a satellite's ticket-vs-cash, or a freeroll's buy-in) are skipped and counted — play or re-open those in live to record them. Safe to run more than once; already-stored sessions aren't duplicated. You get one message with the counts when it finishes. On the Google Sheets backend a large first scan is paced to stay within Google's rate limits, so it can take a few minutes to fully sync — let it settle before checking totals.

Usage example: `/scan`
</details>

<details>
<summary><code>/sheet [name or URL]</code> — link to your data, or switch spreadsheets</summary>

Two jobs in one command: viewing and **switching spreadsheets**. With no argument, replies with the current spreadsheet's URL. With a sheet name or a pasted URL, switches the tracker to that spreadsheet — access is validated (and any missing tabs created) before anything is saved, then the service restarts itself to pick up the change. On the local-file backend there is no sheet; the command points you to `/export` instead.

The current sheet's queued writes must sync first. Before switching, collection
drains and pending cash finishes against the old sheet. An automatic restart
failure restores the previous setting before collection resumes.

Usage examples:
- `/sheet` — show the current spreadsheet URL
- `/sheet My Other Bankroll` — switch to a different sheet by name
- `/sheet https://docs.google.com/spreadsheets/d/…` — switch by pasted URL
</details>

<details>
<summary><code>/storage [local | sheets &lt;name or URL&gt;]</code> — view your storage, or migrate to the other backend</summary>

With no argument, shows where your data lives. `/storage local` moves everything to the local file; `/storage sheets <sheet name or URL>` moves everything to an empty Google Sheet shared with your service account (if this install has no service-account key yet, you'll be prompted to upload it right there and the migration continues). Naming the storage you're already on changes nothing. The target must be empty — there is no merge. You confirm a summary of row counts before anything is copied (close PokerStars first so no hand is played mid-switch), and the copy is verified against your current bankroll before the backend is flipped, so a failed migration leaves you exactly where you were.

The collector checks that PokerStars is closed before pausing collection. It
drains accepted file updates, and the server records pending cash before copying.
Commands received during migration are refused with a request to retry after
restart. The poker PC must be connected in split mode, with matching collector
and server versions supporting the pause/acknowledgement protocol.

Verification compares the copied records and IDs, row counts, independently
summed ledger amounts and bankroll. Partial tournaments and already-folded
bounties are copied directly; Sheets retains its minute-level date precision.
Later cash corrections recognize both historical backend ID formats and retain
the copied ID, so a resumed or corrected file updates the existing result.

Usage examples:
- `/storage` — show the active storage
- `/storage local` — migrate everything to the local file
- `/storage sheets My New Tracker` — migrate everything to that (empty) sheet
</details>

<details>
<summary><code>/screenname [name]</code> — view or change your PokerStars screen name</summary>

With no argument, shows the last PC-confirmed screen name and any pending edit. With one, durably queues the change on the server, including while the PC is offline. The PC validates its screen-name subfolders, saves its own settings and activates the watcher before the bot confirms success. Capitalisation matters.

Usage examples:
- `/screenname` — show the current screen name
- `/screenname NewHandle99` — change it
</details>

<details>
<summary><code>/credentials</code> — swap the Google service account key</summary>

Send the new `google_service_account.json` as an attachment, or type `skip` to keep the current one. On the Google Sheets backend the upload is staged and checked against the current sheet. If validation fails, the old key and sheet stay unchanged. A valid replacement drains accepted work, preserves pending cash and restarts the service; restart-helper failure restores the previous key before collection resumes. On the local-file backend the key is saved without a restart, ready for `/storage` migration to Google Sheets.

Usage example: `/credentials` — then attach the JSON file
</details>

<details>
<summary><code>/paths</code> — set your PokerStars folders</summary>

Configures the base Hand History and Tournament Summary folders (the parent folders — not the screen-name subfolder underneath, which is added automatically). Required before any session can be tracked. Edits queue on the server until the PC validates and applies them; they do not change server-local paths. Forward slashes are converted on Windows. Relative paths resolve against the active Windows desktop user’s home, including when the collector runs as SYSTEM. If that user cannot be identified, send a full drive or UNC path. A drive-relative path such as `C:AppData` is rejected. Invalid edits preserve the previous collection settings and remain visible for correction.

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

Each background role writes a `tracker.log` next to its program for troubleshooting — errors plus a few milestones (start-up, hard stops, migrations). It's capped at 5 MB and rolls over to a single `tracker.log.old`. If the log cannot open, the service discards logging; it does not fall back to a hidden console. Unexpected termination of the required bot, worker or collector exits with failure so the service manager can restart the process.

## License

© 2026 Vitor Cipriano. Released under the [PolyForm Strict License 1.0.0](LICENSE): free for personal, noncommercial use, but you may not redistribute, modify, or use it commercially. See [`LICENSE`](LICENSE) for the full terms.
