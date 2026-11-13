# Poker Tracker

Track PokerStars cash games, Spin & Go and tournaments automatically, see your
bankroll in Telegram, and set hard stops that close PokerStars when you reach
your limits. Runs in the background on your Windows poker PC; an optional
Windows or Linux server keeps your bot and history available while that PC is off.

**[Download the latest release](https://github.com/vit-ui/poker-tracker/releases/latest)**
· [What's new](CHANGELOG.md) · [Already installed? Update](#updating)

## Contents

- [Install and start tracking](#install-and-start-tracking)
- [Everyday Telegram commands](#everyday-telegram-commands)
- [Updating](#updating)
- [Alerts and hard stops](#alerts-and-hard-stops)
- [History, storage and exports](#history-storage-and-exports)
- [Running on two machines](#running-on-two-machines)
- [Troubleshooting and backups](#troubleshooting-and-backups)
- [Limitations](#limitations)
- [Development and license](#development-and-license)

## Install and start tracking

1. On your Windows poker PC, enable PokerStars hand-history and tournament-summary
   saving. Note the folders and your exact PokerStars screen name.
2. Download **`PokerTracker-Setup-<version>.exe`** from
   [Releases](https://github.com/vit-ui/poker-tracker/releases/latest) and run it.
   One installer supports Intel/AMD and ARM PCs. Choose the all-local role to
   keep everything on this PC, or [connect to a server](#running-on-two-machines).
3. Choose **Telegram** setup (recommended) and finish in your bot's chat, or
   **Manual** to enter the settings in the installer. Pick your storage:

   | Storage | What you need |
   | --- | --- |
   | **Local file (SQLite)** | Nothing extra. History lives in `tracker.db` beside the program. |
   | **Google Sheets** | A Google Cloud service account with Sheets and Drive APIs enabled, and an empty sheet shared with it as Editor. |

4. Set the **base** Hand History and Tournament Summary folders, without the
   screen-name subfolder. The tracker adds your screen name itself.
5. In Telegram, use `/status` to check the tracker, `/scan` to import existing
   history, and `/hardstop` to set your limits and lock duration.

The official release includes its bot token. Only source builds need a token
from [BotFather](https://t.me/BotFather). The tracker starts automatically at
boot. Reinstalling in the same folder preserves settings and data, and Telegram
setup accepts contact only from your saved chat.

## Everyday Telegram commands

Start with **`/menu`**. Tap an item, **Back** to return, or **Done** to close a
root menu. Use `/help <command>` for details, such as `/help stats` or
**`/help update`**. Update instructions are also in **Menu → Info → How to update**.

| What you want | Command or example |
| --- | --- |
| Bankroll, recent P&L and lock status | `/status` |
| Most recent session | `/lastsession` |
| Overall performance | `/stats` |
| Cash results for the past 7 days | `/stats cash week` |
| Today's Spin & Go results | `/stats spins today` |
| This month's tournament results | `/stats mtt month` |
| Record a deposit or withdrawal in USD | `/deposit 50.00` · `/withdraw 20.00` |
| Set hard-stop rules and duration | `/hardstop` |
| Lock immediately for the configured duration | `/hardstop trigger` |
| Warning thresholds | `/alerts` |
| Choose summaries and alerts | `/notify` |
| Download all history as three CSV files | `/export` |
| Import existing PokerStars files | `/scan` |

`/stats [cash|spins|mtt] [today|week|month|all]` defaults to all games and all time.
Deposits and withdrawals change your bankroll, but never count as poker winnings
or losses. Use the USD amount shown on PokerStars; comma decimals also work.

Cash sessions appear after **2 minutes 30 seconds without a table-file update**,
including updates while sitting out. Session-loss warnings and hard stops still
work immediately, per hand. For satellites and zero-buy-in tournaments, answer
the bot's ticket/cash or free/paid question so it can record the entry correctly;
real knockout bounties are retained.

## Updating

**Close PokerStars before updating your poker PC.** Run the command on each
machine you want to update. It downloads the latest stable release and selects
the correct role and CPU automatically.

### Windows

Open **PowerShell**, then enter your installation folder (the default is
`C:\poker-tracker`; use your chosen folder if different):

```powershell
cd C:\poker-tracker
.\tracker.exe --version
.\tracker.exe --update
```

This works for both all-local and installed collector roles: the Windows
installer names both `tracker.exe`. For a **Windows server**, use:

```powershell
cd C:\poker-tracker
.\tracker-server.exe --version
.\tracker-server.exe --update
```

Accept the administrator prompt if Windows asks. A standalone collector kept
under its download name uses `.\tracker-client.exe --update` instead.

### Linux server

Open a terminal as the user who installed the server:

```sh
~/.local/share/poker-tracker/tracker-server --version
~/.local/share/poker-tracker/tracker-server --update
```

Wait for **`Updated ...`** or **`Already up to date`**. Run `--version` again to
check the installed version. In split mode, update **both machines** to matching
versions; updating the server does not update the poker PC.

Settings, credentials, history, pairing and active lock expiry stay in place.
A previously running installation restarts; a stopped one stays stopped. The
updater verifies downloads before stopping the running application and requires
the new application to confirm startup before reporting success.

**If `--update` is unrecognized:** install a current release in the existing
folder first. Older releases have no updater. You can also upgrade by rerunning
the latest Windows or Linux installer in the same location.

<details>
<summary>Update recovery and installer details</summary>

If replacement or startup fails, the updater attempts to restore the previous
executable. On Windows, temporary recovery also covers a killed helper or a
restart during replacement: boot/minute recovery restores an unconfirmed update
and the previous task state, or keeps a durably confirmed update. Active locks
retain their original expiry across the brief enforcement interruption.

Do not run setup/uninstall concurrently with a terminal update. Windows may
retain a `.tracker-previous-*` executable until uninstall because the terminal
caller still has the old image open. Terminal updates do not rerun setup or
change the version displayed by the Windows uninstaller.

Development builds cannot update an installation. A release missing the required
executable assets or `SHA256SUMS` must be installed with its installer.

</details>

## Alerts and hard stops

Use **`/alerts`** for warnings and **`/hardstop`** for rules that close PokerStars.
Set a duration before enabling a hard-stop rule or using `/hardstop trigger`.

| Rule | Measures |
| --- | --- |
| Session loss | Loss in cash buy-ins, checked per hand |
| Downswing | Drawdown in buy-ins from the rolling 30-day results peak |
| Consecutive losses | Losing cash sessions |
| Daily loss | USD losses across all games in your poker day |
| Spin & Go daily loss | USD losses in Spin & Go in your poker day |

Durations accept whole hours, days or weeks: **`12`**, **`48h`**, **`2d`**, **`1w`**.
A hard stop runs its **full duration and cannot be cancelled**. Changing the
configured duration does not change an existing lock's expiry. PokerStars is
closed again if reopened while locked.

An enabled hard-stop threshold must be at least its matching enabled alert
threshold. Turn a rule off without losing its saved number; disabling a hard-stop
rule asks for confirmation. `/schedule` sets the poker-day start hour (default
**06:00**). `/notify` filters cash, tournament, weekly and monthly summaries to
**Every**, **Only wins**, **Only losses**, or **Off**, and toggles stake-change notices.

**While the poker PC is offline:** rules and schedules still save on the server.
`/hardstop trigger` saves a request whose duration starts immediately. The PC
applies only the remaining time when it reconnects; expired requests do not start
a new lock. `/status` shows expiry, time remaining, offline state and pending PC
confirmation. Dates follow the poker PC's last reported timezone across server
restarts, refreshed when that PC reports again.

The collector enforces an active lock even while disconnected from its server.
Successful PokerStars closures produce Telegram notices; offline notices arrive
after reconnection and keep the original closure time.

## History, storage and exports

| Task | Command |
| --- | --- |
| Import files already in your PokerStars folders | `/scan` |
| Export cash sessions, tournaments and ledger | `/export` |
| Show the current storage backend | `/storage` |
| Move history to SQLite | `/storage local` |
| Move history to an empty, shared Google Sheet | `/storage sheets My Tracker` |
| Open the current sheet | `/sheet` |
| Switch to a different sheet | `/sheet My Other Tracker` or `/sheet <URL>` |
| Replace the Google service-account key | `/credentials`, then attach the JSON file |
| Show or queue a new screen name | `/screenname` or `/screenname NewHandle99` |
| Edit PokerStars base folders | `/paths` |
| List configuration commands | `/config` |

**Import:** `/scan` records silently, with one final report and no hard stops,
alerts or session summaries. Already stored sessions are not duplicated.
Tournaments needing ticket/cash or free/paid answers are skipped and counted.
A large Sheets import can take a few minutes to sync.

**Storage migration:** close PokerStars and keep the poker PC connected. The
destination must be empty; histories are not merged. Confirm the row counts in
Telegram. The tracker verifies the copy before switching and restarting. If
needed, `/storage sheets ...` asks you to upload the service-account key first.
Exports have the same three-file format on both backends, including headers for
empty datasets.

**Folder edits:** use the base folder without your screen name. Relative Windows
paths resolve against the active desktop user's home; otherwise provide a full
drive or UNC path. Drive-relative paths such as `C:AppData` are rejected.
`/paths` and `/screenname` changes stay pending until the poker PC validates,
saves and activates them. Invalid edits keep the previous settings; send a
corrected value to replace a pending edit.

**Accounting:** money uses whole USD cents; fractional cents round up in
magnitude (`$1.001` → `$1.01`). Session corrections update the existing result
and subsequent ledger totals. Deposits and withdrawals stay separate from
results, drawdown and streak calculations. Bounties are included once.

<details>
<summary>Sheets connectivity and configuration changes</summary>

If Sheets is unreachable, writes queue locally. The bot sends one notice, a
reminder every 30 minutes while offline, and confirmation after syncing.
New money movements carry operation IDs to avoid duplicate recovery writes.
Older entries without IDs retain at-least-once recovery and may duplicate an
append if its acknowledgement was lost; equal amounts are never merged.

Queued writes must finish before migration or switching sheets. The tracker
drains accepted work and records pending cash before switching. Migration copies
raw history, IDs, totals and partial tournaments, and verifies counts, ledger
amounts and bankroll. Sheets stores timestamps with minute precision.

`/sheet <name or URL>` validates access and restarts automatically. `/credentials`
validates a replacement against the active sheet before replacing the key; type
`skip` to cancel. A restart-helper failure restores the previous setting or key.
On SQLite, credentials can be saved for a later migration without restarting.

</details>

## Running on two machines

Use this if you want Telegram commands and history available while your poker
PC is off. Keep collector and server on **matching versions**.

| Machine | Role |
| --- | --- |
| Windows poker PC | Collector: watches PokerStars files and enforces locks |
| Always-on Windows or Linux machine | Server: stores history, runs Telegram and evaluates rules |

1. Install the server:

   - **Windows:** run `PokerTracker-Setup-<version>.exe` and choose **Run a server
     on this PC**. Configure through Telegram or in the installer.
   - **Linux (amd64 or arm64):** download `PokerTracker-Server-linux.tar.gz`,
     extract it, and run `PokerTracker-Server-linux`. It installs in
     `~/.local/share/poker-tracker` as your user. It asks for elevated approval
     only to enable startup without login; you may skip that step.

2. Copy the connection string shown by setup:
   `tracker://192.168.1.40:8443/<token>#fp=<fingerprint>`.
3. On the poker PC, run the Windows installer, choose **Connect to a server**,
   and paste the string. You can also run `tracker-client --setup` for a fresh
   standalone collector.

The poker PC initiates an encrypted, authenticated TLS connection to the server.
Only the PC needs to reach that server address; your phone communicates through
Telegram. On Linux, print the connection string again with:

```sh
~/.local/share/poker-tracker/tracker-server --pair
```

Fresh server setup can start with SQLite before Google credentials are available.
To move to Sheets afterward, pair the PC, close PokerStars, and use
`/storage sheets <name or URL>` in Telegram.

<details>
<summary>Linux setup dialogs</summary>

Desktop setup uses zenity on GTK desktops or kdialog on KDE. If neither is
available, or on a headless machine, run the installer from a terminal to answer
its prompts. The user service runs while logged in unless startup without login
was enabled. Rerunning the installer preserves the existing storage choice.

</details>

<details>
<summary>Move an existing all-local installation to a server</summary>

Configure the destination server's storage first. Close PokerStars and rerun the
Windows installer **in the existing tracker folder**, choose **Connect to a
server**, and paste its connection string. Accept the offered history transfer.
Running `tracker-client --setup` alone does not transfer an all-local installation.

The destination needs empty history, no connected collector, and no pending cash,
tournament questions, collector edits or unconfirmed hard-stop deliveries. Saved
Telegram chats must match; an unset destination chat is copied from the source.
Pending Sheets writes must finish. Histories are never merged.

History, running totals, rules, notifications, schedules and unanswered tournament
questions move together. Each machine keeps its own Google credentials, storage
configuration and pairing identity. Source history and the original executable
are retained. Active protection keeps its original expiry. If protection has not
been confirmed by the collector, keep the tracker running until it is confirmed
and retry. The destination takes over Telegram only after verified handover.

If interrupted, rerun setup in the same folder with the **same connection
string**. A disconnection never counts as success. Verified rollback removes
only transfer-owned rows and restores the source; it refuses to erase newly
accepted destination work. If the destination cannot be reached, keep the saved
files and retry later. Older executables unable to identify their role need an
upgrade in their existing role first; older protocols/schemas are unsupported.

</details>

## Troubleshooting and backups

| Symptom | What to check |
| --- | --- |
| No cash session yet | Wait 2 min 30 sec after the last table-file update. Check `/paths` and `/screenname`. |
| No historical results after setup | Run `/scan`; review its skipped-tournament count. |
| Folder/name edit stays pending | Keep the poker PC online. Check the bot's validation message and send a corrected value if needed. |
| Saved folders unavailable at startup | Collection stays inactive. Correct `/paths`; commands and hard-stop enforcement remain available. |
| Bot unavailable with an all-local setup | The poker PC must be on and connected. Use a separate server for access while it is off. |
| Sheets unavailable | Let queued results sync; the bot reports recovery. |
| `--update` unknown | Install the latest release in the existing folder first. |
| Another failure | Check `tracker.log` beside the executable on the affected machine. |

Back up the **installation folder**, including settings, local database,
credentials, pairing and saved protection/recovery state. Keep the original
PokerStars histories too: they recover file bytes the tracker had not received.
A normal Windows uninstall removes local player data; silent uninstall preserves
it for upgrades. Prefer the updater or rerun the installer to upgrade.

<details>
<summary>Recovery files and background operation</summary>

Preserve `pending_cash.json`, `lockfile.json`, `hardstop_deliveries.json`,
`collector_observation.json`, `collector_clock.json`, `collector_closures.json`
and `closure_notifications.json` with their respective installation. In split
mode, closure receipts live on their respective PC/server; in all-local mode,
they are together. Keep pending tournament questions and any saved transfer or
update-recovery state as well.

Pending cash retains each table's remaining quiet interval across restart and
requests its source file again. An unchanged recovery upload preserves its
deadline; later corrections update the existing session. Saved collector clocks
preserve offline date windows and prefer the latest saved observation.

Windows Task Scheduler restarts failures and checks every minute for a stopped
process. The collector reconnects every 30 seconds, checks active locks every
second and retains 30-second idle checks. Setup suspends scheduled recovery
while replacing/configuring an installation and restores it on completion or
cancellation. Controlled stops drain accepted work and save pending cash.

Logs are capped at 5 MB with one `tracker.log.old`. If a log cannot open, logging
is discarded. Technical errors are logged; user-facing notices go through the
bot. Closure notifications are at least once: a crash after Telegram accepts a
message but before its receipt is saved may produce a duplicate.

</details>

## Limitations

- Single-user PokerStars tool; all-local Telegram access requires the PC to be on.
- VPIP/PFR use Hold'em and Omaha preflop actions. Stud/draw hands contribute to
  hand counts, so mixed-game percentages are diluted and all-stud/draw shows zero.
- Satellite detection needs structural clues in the summary; tournaments without
  those clues may not be identified. `/scan` skips tournaments needing answers.
- A hard-stop lock has no cancel or bypass command.

## Development and license

Builds, tests, architecture and contribution rules are in
[CONTRIBUTING.md](CONTRIBUTING.md). Recent additions and fixes are in the
[Changelog](CHANGELOG.md).

© 2026 Vitor Cipriano. Released under the [PolyForm Strict License 1.0.0](LICENSE).
See the license for permissions and restrictions. Provided as-is, without
warranty; not affiliated with or endorsed by PokerStars.
