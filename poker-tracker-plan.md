# Poker Tracker — Full Technical Plan

## Overview

This system automatically tracks a poker player's sessions on PokerStars, manages bankroll accounting, and provides alerts and controls via a Telegram bot. It runs on Windows as a background service that starts on boot and requires zero manual interaction during normal use. The only manual inputs in the entire system are logging deposits and withdrawals via Telegram commands.

The player plays Cash Games as their primary game type and Spin & Go tournaments as a secondary format. These two game types have fundamentally different structures and are tracked separately with metrics appropriate to each. They share a single bankroll ledger so that the true profit/loss is always accurate regardless of which format drained or grew the bankroll.

---

## Problem Statement

### Bankroll accounting
PokerStars does not expose the player's account balance via any API. The only way to know the current balance is to look at the client. To work around this, the system maintains a ledger of every money movement: deposits into PokerStars, withdrawals from PokerStars, and the net result of every session. At any point in time:

```
Current Bankroll = sum(deposits) - sum(withdrawals) + sum(all session net results)
```

This means that if the player deposits $100, loses it all, then deposits another $100, the system correctly shows -$100 P&L (profit and loss: total money made or lost), not $100. Reloads are not resets — they are tracked as negative events in the ledger.

### Session data source
PokerStars automatically writes hand history files to disk during and after each session. These files are the data source for everything. No data is entered manually by the player.

- Cash game hands are written to: `C:\Users\<username>\AppData\Local\PokerStars\HandHistory\<ScreenName>\`
- Tournament summaries (including Spin & Go) are written to: `C:\Users\<username>\AppData\Local\PokerStars\TournamentSummaries\<ScreenName>\`

Note the `<ScreenName>` subfolder — PokerStars always nests the actual files one level below the base folder, in a directory named after the player's screen name. The player configures only the base folder (`HandHistory\` / `TournamentSummaries\`) via `/paths`; the system appends the screen-name subfolder automatically using `setup.pokerstars_screen_name`, which is already known. This avoids asking the player to type a path they'd likely get wrong.

The system watches both resolved folders and processes new files automatically.

---

## Architecture

The system is a single Go binary (`tracker.exe`) with three entry points controlled by a command-line flag:

- `tracker.exe --serve` — runs the background service (watcher + bot + all logic)
- `tracker.exe --setup` — runs the first-time setup conversation via Telegram, called by the installer
- `tracker.exe --check-lock` — checks whether a hard stop lockfile is active and exits with code 0 (no lock, proceed) or 1 (locked, abort)

The `--check-lock` flag is called by the `.bat` launcher before opening PokerStars. The `--serve` flag is called by Windows Task Scheduler on boot. The `--setup` flag is called by the installer wizard.

The bot token is injected into the binary at build time via Go's `-ldflags` from a GitHub Actions secret. It is never stored in the repo or on disk. The player's Telegram chat ID is auto-detected from the first message they send to the bot and saved to `config.json`.

### Concurrency model (`--serve` mode)

The service runs three concurrent goroutines communicating through channels. A goroutine is Go's lightweight concurrent execution unit, and a channel is the typed pipe through which goroutines pass data to each other safely.

```
Watcher goroutine
    Detects new or modified files in the hand history and tournament summary folders.
    Base paths are read from config.json; main.go joins each with the screen name
    (config.Setup.PokerStarsScreenName) before passing the resolved paths to the watcher.
    Accepts a done chan struct{} — closing it stops the watcher.
    Sends the file path and type (cash or tournament) to sessionChan.

Worker goroutine
    Receives from sessionChan.
    Calls the appropriate parser.
    Writes the result to Google Sheets.
    Evaluates hard stop trigger conditions.
    Sends alert messages to alertChan.

Bot goroutine
    The only goroutine that communicates with Telegram.
    Listens on alertChan and sends outgoing messages.
    Also listens for incoming commands from the player.
    Holds a restartWatcher callback passed from main.go.
    Calls restartWatcher when /paths saves new folder paths.
```

If `config.json` has null paths on startup, the watcher does not start. The bot notifies the player to run `/paths`. The watcher starts when paths are first configured. Every time `/paths` saves new base paths, `main.go`'s `restartWatcher` callback re-joins them with the screen name before starting the new watcher goroutine — the join happens in `main.go`, never inside `watcher.go`.

No goroutine writes to config.json or lockfile.json concurrently. All file reads and writes for these two files go through `config.go`, which uses a package-level mutex (mutual exclusion lock: a mechanism that ensures only one goroutine can access a resource at a time) to prevent race conditions.

---

## Repository Structure

```
poker-tracker/
├── cmd/
│   └── tracker/
│       └── main.go               ← entry point, flag parsing, goroutine startup
├── internal/
│   ├── watcher/
│   │   └── watcher.go            ← file system watcher
│   ├── parsers/
│   │   ├── cash.go               ← cash game hand history parser
│   │   └── spingo.go             ← spin & go tournament summary parser
│   ├── sheets/
│   │   └── sheets.go             ← google sheets integration
│   ├── bot/
│   │   └── bot.go                ← telegram bot, commands and alerts
│   ├── hardstop/
│   │   └── hardstop.go           ← lock trigger, taskkill, lockfile write/read
│   └── config/
│       └── config.go             ← config.json and lockfile.json read/write
├── credentials/
│   └── google_service_account.json   ← never committed to git
├── testdata/
│   ├── cash/                     ← real hand history .txt files for parser tests
│   └── spingo/                   ← real tournament summary .txt files for parser tests
├── .github/
│   └── workflows/
│       └── release.yml           ← builds tracker.exe + Setup.exe on tag push
├── setup.iss                     ← inno setup installer script
├── VERSION                       ← current version string, e.g. "1.0.0"
├── config.json                   ← runtime settings, never committed to git
├── lockfile.json                 ← only exists when a hard stop is active
├── .env                          ← three static deployment paths, never committed to git
├── .gitignore
├── go.mod
└── go.sum
```

The `internal/` directory is a Go convention that prevents these packages from being imported by code outside this repository.

---

## Environment Variables (`.env`)

Written once by the installer. Never committed to git. Contains only three static deployment paths that never change after installation.

```
GOOGLE_SERVICE_ACCOUNT_PATH=.\credentials\google_service_account.json
CONFIG_PATH=.\config.json
LOCKFILE_PATH=.\lockfile.json
```

`TELEGRAM_BOT_TOKEN` is not in `.env`. It is injected into the binary at build time via Go's `-ldflags` from a GitHub Actions secret `TELEGRAM_BOT_TOKEN`. It is never stored on disk.

Everything else (sheet name, screen name, folder paths, chat ID, thresholds) lives in `config.json` and is set via the bot during `--setup` mode or via bot commands afterwards.

---

## Dependencies

All managed via Go modules (`go.mod`).

- `github.com/fsnotify/fsnotify` — file system watcher. Uses Windows-native `ReadDirectoryChangesW` API under the hood, which means events are pushed by the OS, not polled. No performance cost, no delay.
- `google.golang.org/api/sheets/v4` — official Google Sheets API client
- `google.golang.org/api/drive/v3` — used by `sheets.go` to find the spreadsheet by name
- `golang.org/x/oauth2/google` — Google service account authentication
- `github.com/go-telegram-bot-api/telegram-bot-api/v5` — Telegram bot library
- `github.com/joho/godotenv` — loads `.env` file into environment variables at startup

---

## Module: `config/config.go`

Owns all reads and writes to `config.json` and `lockfile.json`. Every other module imports this one to read or update settings and lock state.

### `config.json` schema

All values start as null. Set during `--setup` mode or via bot commands.

```json
{
  "setup": {
    "telegram_chat_id": null,
    "google_sheet_id": null,
    "google_sheet_name": null,
    "pokerstars_screen_name": null
  },
  "paths": {
    "hand_history": null,
    "tournament_summary": null
  },
  "alerts": {
    "downswing_buyins": null,
    "losing_streak_count": null,
    "spingo_daily_loss_usd": null
  },
  "hardstop": {
    "session_loss_buyins": null,
    "consecutive_losses": null,
    "spingo_daily_loss_usd": null,
    "duration_hours": null
  }
}
```

- `telegram_chat_id` — auto-detected from the first message sent to the bot. Never asked for explicitly.
- `google_sheet_name` — exact name of the Google Sheet. Set during setup.
- `pokerstars_screen_name` — used by the cash parser to identify the player's hands. Set during setup.
- `hand_history` / `tournament_summary` — **base** Windows paths to the PokerStars folders, not including the screen-name subfolder PokerStars nests underneath each one. Set during setup or via `/paths`. Changing them triggers a watcher restart. The screen-name subfolder is appended automatically by `main.go` using `pokerstars_screen_name` — never asked for separately and never stored in this field.
- `downswing_buyins` — alert fires when bankroll drops this many buy-ins below the monthly peak.
- `losing_streak_count` — alert fires after this many consecutive losing sessions.
- `spingo_daily_loss_usd` — alert fires when Spin & Go losses in one day exceed this amount.
- `session_loss_buyins` — hard stop fires when a single session loss exceeds this many buy-ins.
- `consecutive_losses` — hard stop fires after this many consecutive losing sessions.
- `duration_hours` — how long the hard stop lock lasts. Accepts 12 or 24.

A buy-in is the standard buy-in for the stake being played. The stake is read directly from the hand history file, so the dollar value of one buy-in is derived automatically. For example, at NL10 ($0.05/$0.10), one buy-in is $10 (100 big blinds).

### `lockfile.json` schema

Only exists on disk when a hard stop is active. Created by `hardstop.go`, read and deleted by `hardstop.go`.

```json
{
  "triggered_at": "2025-01-15T22:30:00Z",
  "expires_at": "2025-01-16T10:30:00Z",
  "reason": "session_loss_buyins",
  "duration_hours": 12
}
```

---

## Module: `watcher/watcher.go`

Uses `fsnotify` to watch both the hand history and tournament summary folders for `WRITE` and `CREATE` events. `watcher.go` has no knowledge of `config.json`, base paths, or screen names — it only ever receives two already-resolved, fully-joined folder paths as arguments to `Watch`. Resolving `config.Paths.HandHistory` / `TournamentSummary` (base folders) into the real watched paths by joining them with `config.Setup.PokerStarsScreenName` is `main.go`'s job, done with `filepath.Join`, both at startup and inside the `restartWatcher` callback.

The `Watch` function accepts a `done <-chan struct{}` channel. Closing it stops the watcher. This is how `main.go` restarts the watcher when paths change: it re-resolves the joined paths, closes the current done channel, creates a new one, and starts a new watcher goroutine with the newly resolved paths and the same `sessionChan`.

When an event fires, the watcher:

1. Waits 2 seconds after the last event on that file before acting. This debounce (delay to avoid processing a file that is still being written) prevents the parser from reading an incomplete file while PokerStars is still writing it.
2. Determines the file type: if the path is under the hand history folder, it is a cash game file. If under tournament summaries, it is a tournament file (Spin & Go or MTT — the parser determines which).
3. Sends a `FileEvent{Path, Type}` struct to `sessionChan`. Type is `"cash"` or `"tournament"`.

---

## Module: `parsers/cash.go`

Parses a `.txt` file from the Hand History folder. This folder contains **two kinds of files**: real cash game hands, and tournament hands (PokerStars writes both there — a tournament's hand-by-hand play is a Hand History file, separate from its end-of-game summary in TournamentSummaries). The parser must distinguish between them and return the appropriate result for whichever kind it received — never both, never an error for the tournament case, since a tournament hand file is a perfectly valid file, just not a cash one.

Each cash hand begins with a header line like:

```
PokerStars Hand #123456789: Hold'em No Limit ($0.05/$0.10 USD) - 2025/01/15 22:00:00 ET
```

A tournament hand's header instead reads:

```
PokerStars Hand #123456789: Tournament #4009242649, $0.98+$0.12 USD Hold'em No Limit - Level ...
```

The parser reads the screen name from `config.Setup.PokerStarsScreenName` (passed in as a parameter by the caller) to identify the player's lines.

**If the file contains cash hands:** extracts from each hand — timestamp; stake (parsed from the header, e.g. `$0.05/$0.10` → `NL10`); net result for the player (difference between starting stack and ending stack for that hand); rake paid; whether the player voluntarily put money in preflop (VPIP: voluntarily put money in pot, the percentage of hands where the player calls or raises before the flop); whether the player raised preflop (PFR: preflop raise percentage, a measure of preflop aggression). After reading all hands, groups them into one session record.

**If the file contains tournament hands:** extracts the tournament ID from `Tournament #<id>` in the header, and computes VPIP/PFR across all hands in the file using the same logic as the cash case — but there is no dollar data to extract, since PokerStars doesn't know a tournament's dollar outcome until it ends (see "Why Tournaments Need Two Files" further down).

```go
type CashSession struct {
    Date        time.Time
    Stake       string    // e.g. "NL10"
    BuyInUSD    float64   // dollar value of one buy-in at this stake
    Hands       int
    DurationMin int
    NetUSD      float64
    RakeUSD     float64
    BB100       float64   // net result expressed in big blinds per 100 hands
    VPIP        float64   // percentage
    PFR         float64   // percentage
}

// returned instead of CashSession when the file is a tournament hand file
type TournamentHandStats struct {
    TournamentID string    // extracted from "Tournament #<id>" in the header
    VPIP         float64   // percentage
    PFR          float64   // percentage
}
```

`ParseCashFile(path, screenName string) (session *CashSession, tourneyStats *TournamentHandStats, err error)` returns exactly one of `session` or `tourneyStats` as non-nil on success — never both. `err` is non-nil only for a genuine parse failure (malformed file, screen name not found in any hand), never for the tournament case.

BB/100 (big blinds won per 100 hands: the standard win rate metric for cash games) is calculated as:

```
BB100 = (NetUSD / BigBlindValue) / Hands * 100
```

---

## Module: `parsers/tournament.go`

Parses a PokerStars tournament summary `.txt` file. Every tournament — Spin & Go or MTT — produces exactly one summary file after the game ends.

The parser reads the first line of the file to determine the tournament type. If `"Spin & Go"` is present, `Type` is set to `"spingo"`. Otherwise `Type` is `"mtt"`. This is the authoritative discriminator.

A summary file contains:

- Tournament type (from first line)
- Tournament ID (from `Tournament #<id>` in the header)
- Buy-in amount
- Total prize pool
- Finish position
- Total players registered
- Amount won

The parser returns:

```go
type Tournament struct {
    Date         time.Time
    TournamentID string    // extracted from "Tournament #<id>" in the header
    Type         string    // "spingo" or "mtt"
    BuyInUSD     float64
    PrizePoolUSD float64
    Multiplier   *float64  // only set for spingo: PrizePoolUSD / (BuyInUSD * 3). nil for mtt.
    FinishPos    int
    TotalPlayers int       // always 3 for spingo; field size for mtt
    NetUSD       float64   // payout minus buy-in
    VPIP         *float64  // percentage. nil until a matching hand file is parsed.
    PFR          *float64  // percentage. nil until a matching hand file is parsed.
}
```

ROI (return on investment: `(total winnings - total buyins) / total buyins * 100`) is computed by Google Sheets formulas across all rows of the relevant type, not by the parser.

---

## Why Tournaments Need Two Files (and Cash Doesn't)

Cash game hand histories record real dollars at every point — each hand shows the player's USD stack before and after, so net result, rake, and everything else comes from that one file alone.

Tournaments are structurally different: play happens in chips, not dollars. A chip's dollar value depends on how the player finishes and the prize structure, neither of which is known until the tournament ends. The tournament **hand history** file (in the Hand History folder, alongside cash hands) only ever contains chip actions — it has no buy-in, no payout, no dollar net result, because PokerStars itself doesn't know the dollar outcome yet. That data is written once, at the end, to the **tournament summary** file (in TournamentSummaries). Neither file is a superset of the other: the hand file is the only source for VPIP/PFR, the summary file is the only source for buy-in/payout/finish.

Because the two files arrive independently, in no guaranteed order, and can even be separated by a long time (an MTT hand file might be written hours before its summary), they are correlated by `TournamentID` on the same Tournaments tab row rather than kept as two separate records or tracked with in-memory pending state — see `AppendTournament` and `UpdateTournamentVPIPPFR` below.

---

## Module: `sheets/sheets.go`

Handles all interaction with the Google Sheets API. Authenticates using a service account (a Google Cloud identity with its own credentials JSON file, used for server-to-server access without a user login flow).

### Client construction

`NewClient(serviceAccountPath, spreadsheetID string) (*Client, error)` — authenticates using the service account JSON and connects directly using the spreadsheet ID. No Drive API call. Used by `--serve` and `--setup` (after the ID has been resolved).

`ResolveSheetID(serviceAccountPath, sheetName string) (string, error)` — uses the Drive API to find a spreadsheet by name and return its ID. Called exactly once during `--setup`. The resolved ID is saved to `config.Setup.GoogleSheetID`. After that, the Drive API is never used again.

### On startup (called by `main.go` before the watcher starts)

The module checks whether the target spreadsheet exists and contains all required tabs with correct headers. If anything is missing, it creates it. This means the player never has to manually create or configure the spreadsheet. The tabs created and their headers are:

**Dashboard** — contains only formulas that reference other tabs. Never written to by the script. Shows: current bankroll, total deposited, total profit, total loss, this month's P&L, all-time BB/100, all-time Spin & Go ROI, all-time MTT ROI, volume this month, best and worst time slots from the Time Analysis tab.

**Cash Sessions**
`Date | Stake | Hands | Duration (min) | Net (USD) | Rake (USD) | BB/100 | VPIP | PFR`

**Tournaments**
`Date | Type | Tournament ID | Buy-In (USD) | Prize Pool (USD) | Multiplier | Finish Position | Total Players | Net (USD) | VPIP | PFR`

For Spin & Go rows, Multiplier is the float value. For MTT rows, the Multiplier cell is empty. VPIP/PFR cells are blank until a hand file for that tournament is parsed (see "Why Tournaments Need Two Files" above) — rows are not appended blindly but correlated by Tournament ID, since the buy-in/payout data and the VPIP/PFR data arrive from two separate files in no guaranteed order.

**Bankroll Ledger**
`Date | Type | Amount (USD) | Running Total (USD)`

The `Type` column contains one of: `deposit`, `withdrawal`, `cash_session`, `tournament`. The `Running Total` column is written by the script, not a formula, so the ledger is self-contained and does not depend on other tabs.

**Cash Stats** — formulas only. BB/100 by month, BB/100 by stake, hourly rate, VPIP and PFR over time. Nothing written by the script.

**Tournament Stats** — formulas only. ROI overall and by type, Spin & Go ROI by multiplier tier, MTT avg finish percentile, total volume, total net. Nothing written by the script.

**Time Analysis** — written by the script after each session. One row per session, contains hour of day and day of week in addition to the net result. Formulas in the same tab aggregate this into a grid showing average net result by hour and day.

### Methods exposed to other modules

- `AppendCashSession(session CashSession)` — appends one row to Cash Sessions, one row to Bankroll Ledger, one row to Time Analysis
- `findTournamentRow(tournamentID string) (rowIndex int, found bool, err error)` — unexported. Searches the Tournaments tab's Tournament ID column for a match. Used by both methods below.
- `AppendTournament(t Tournament)` — calls `findTournamentRow(t.TournamentID)`. If found (a stub row was already created by `UpdateTournamentVPIPPFR` because the hand file arrived first), updates that row's Type, Buy-In, Prize Pool, Multiplier, Finish Position, Total Players, and Net columns, leaving the existing VPIP/PFR cells untouched. If not found, appends a new row with all of `t`'s fields (VPIP/PFR blank if nil). Either way, always appends exactly one row to Bankroll Ledger with type `"tournament"` — `AppendTournament` is only ever called once per tournament (the summary file is written exactly once by PokerStars), so the ledger entry is unconditional, regardless of whether the Tournaments-tab row was new or a pre-existing stub.
- `UpdateTournamentVPIPPFR(tournamentID string, vpip, pfr float64) error` — calls `findTournamentRow(tournamentID)`. If found, updates only the VPIP and PFR cells on that row, leaving every other cell untouched. If not found, appends a new row with only Tournament ID, VPIP, and PFR filled in — every other cell blank, to be filled in later when `AppendTournament` runs for the same ID.
- `AppendLedgerEntry(entryType string, amountUSD float64)` — used by the bot for deposits and withdrawals
- `GetCurrentBankroll() float64` — reads the running total from the last row of the Bankroll Ledger
- `GetSheetURL() string` — returns the full URL to the spreadsheet
- `GetRecentLedgerRows(n int) ([]LedgerEntry, error)` — returns the last n rows from Bankroll Ledger; used by hardstop for consecutive loss check
- `GetLedgerEntriesSince(t time.Time) ([]LedgerEntry, error)` — used by bot for P&L queries
- `GetCashSessionsSince(t time.Time) ([]CashSession, error)` — used by bot for `/stats cash` and summaries
- `GetTournamentsSince(t time.Time) ([]Tournament, error)` — used by bot for `/stats spins`, `/stats mtt`, summaries, and by hardstop for daily Spin & Go loss check (caller filters by `Type == "spingo"`)

---

## Module: `hardstop/hardstop.go`

### Trigger evaluation (called by the worker goroutine after every session)

After a session is written to sheets, the worker calls the appropriate evaluate function. For cash sessions: `hardstop.Evaluate(session, sheetsClient, configPath, lockfilePath, alertChan)`. For tournaments: `hardstop.EvaluateTournament(tournament, sheetsClient, configPath, lockfilePath, alertChan)`.

`Evaluate`:
1. Reads the current config from `config.go`
2. If all hard stop values in config are null, returns immediately — no hard stop is configured
3. Checks each configured condition against the session data and recent history read from the sheet via `sheetsClient`
4. If any condition is met, calls the unexported `trigger(reason, durationHours, lockfilePath, alertChan)`

The consecutive losses check reads the last N rows from the Bankroll Ledger and checks if all have `Amount < 0` and `Type` is `cash_session` or `tournament`.

`EvaluateTournament`:
1. Reads config. Returns immediately if `SpingoDailyLossUSD` is null.
2. Returns immediately if `tournament.Type != "spingo"` — MTTs do not trigger this condition.
3. Calls `sheetsClient.GetTournamentsSince(startOfToday)`, filters by `Type == "spingo"`, sums `NetUSD`.
4. If the sum is more negative than the configured threshold, calls `trigger`.

### `trigger` (unexported)

1. Runs `exec.Command("taskkill.exe", "/IM", "PokerStarsUpdate.exe", "/F")` to force-close PokerStars
2. Writes `lockfile.json` with the current time, expiry time, reason, and duration
3. Sends an alert string to `alertChan` for the bot to deliver via Telegram

### `ManualTrigger` (exported)

Called by the Telegram bot when the player sends `/hardstop trigger`. Reads `duration_hours` from config and returns an error if it is null (bot replies asking the player to configure it first). Otherwise calls the unexported `trigger` with reason `"manual"`.

### Lock check (called when `tracker.exe --check-lock` is run)

1. Checks if `lockfile.json` exists
2. If it does not exist: exits with code 0 (no lock active)
3. If it exists and `expires_at` is in the future: prints a message stating the lock is active and when it expires, exits with code 1
4. If it exists and `expires_at` has passed: deletes `lockfile.json`, exits with code 0

The `.bat` launcher reads the exit code and aborts if it is 1, so PokerStars never opens while a lock is active. Expiry is evaluated lazily — only when the launcher runs. There is no background process checking for expiry. This is intentional.

---

## Module: `bot/bot.go`

The Telegram bot runs in its own goroutine. It does two things simultaneously using Go's `select` statement (a control structure that waits on multiple channel operations): listens for messages on `alertChan` and sends them as Telegram messages, and polls the Telegram API for incoming commands from the player.

The bot token is a package-level variable set at build time via `-ldflags`. It is passed to `bot.Run` and `bot.RunSetup` by `main.go`.

On every incoming message: if `config.Setup.TelegramChatID` is null, the sender's chat ID is saved to config. After that, all messages from other chat IDs are silently ignored.

`bot.Run` accepts a `restartWatcher func()` callback from `main.go`. It calls this when `/paths` saves new paths to config.

`bot.RunSetup` is a separate entry point for `--setup` mode. It starts with no sheets client and no watcher, runs the full setup conversation via Telegram, and returns when setup is complete.

### Inline keyboard flows

`/paths`, `/alerts`, and `/hardstop` all use Telegram inline keyboards (buttons rendered inside the chat message) to walk the player through setting config values. The flow for each:

1. Bot sends a message with buttons for each configurable field, plus a "Done" button
2. Player taps a field button
3. Bot sends a message asking for the new value
4. Player types the value
5. Bot validates it, writes it to config.json via `config.go`, confirms with a message
6. Bot re-sends the keyboard so the player can set another field or press Done

### Commands

**`/start`**
Runs the full setup wizard in sequence: `/paths` → `/alerts` → `/hardstop`. Re-runnable at any time.

**`/deposit <usd_amount>`**
Logs a deposit. Amount is the USD value shown on PokerStars after the deposit clears — not BRL. Replies with the new running bankroll total.

**`/withdraw <usd_amount>`**
Logs a withdrawal. Amount is the USD value shown on PokerStars — not BRL. Replies with the new running bankroll total.

**`/status`**
Replies with current bankroll, today's P&L, this week's P&L, this month's P&L, and active hard stop lock info if any.

**`/lastsession`**
Replies with the full data from the most recently parsed session: game type, stake or buy-in, result, hands or duration, and updated bankroll.

**`/stats [cash|spins] [today|week|month|all]`**
Queries the relevant sheet tab and returns a summary. Defaults to all-time stats for both game types if no arguments given.

**`/sheet`**
Replies with the Google Sheets URL.

**`/paths`**
Opens the inline keyboard flow for setting the *base* hand history and tournament summary folder paths — the parent folder PokerStars writes into, not the screen-name subfolder underneath it. The bot's prompt text should make this distinction explicit (e.g. "Send the HandHistory folder path — do not include your screen name at the end, that part is added automatically"). On Done, calls `restartWatcher()`, which re-joins the base paths with the screen name. Required before sessions can be tracked.

**`/alerts`**
Opens the inline keyboard flow for configuring alert thresholds.

**`/hardstop`** (no argument)
Opens the inline keyboard flow for configuring hard stop rules and duration.

**`/hardstop trigger`**
Calls `hardstop.ManualTrigger`. If duration is not configured, bot replies asking the player to set it first.

**`/config`**
Lists `/paths`, `/alerts`, and `/hardstop` with a one-line description of each. No logic of its own.

**`/help [command]`**
With no argument: lists all commands with a one-line description.
With a command name: explains that command in detail with parameters and a usage example.

### Automatic outgoing alerts (sent to the player without any command)

All of these are sent by the bot goroutine when it receives the corresponding message on `alertChan`.

- **Post-session summary** — sent after every session without exception. Contains: game type, stake (cash) or buy-in (Spin & Go), net result, hands played (cash) or finish position (Spin & Go), session duration, and current bankroll.
- **Downswing alert** — sent when the bankroll drops `downswing_buyins` below the highest bankroll recorded this calendar month. Only fires if the config value is not null.
- **Losing streak alert** — sent when `losing_streak_count` consecutive sessions have all been losing sessions. Counts across both game types. Only fires if the config value is not null.
- **Spin & Go drain warning** — sent when total Spin & Go losses today exceed `spingo_daily_loss_usd`. Only fires if the config value is not null.
- **Stake change detected** — sent when the stake of the current cash session differs from the stake of the previous cash session. Purely informational, no action required.
- **Hard stop triggered** — sent immediately when any hard stop condition fires or `/hardstop trigger` is used. Includes the reason and the exact expiry time.
- **Weekly summary** — sent automatically every Monday at 09:00. Contains: total sessions, total hands, net P&L, BB/100, Spin & Go ROI, and volume for the past 7 days.
- **Monthly summary** — sent automatically on the 1st of every month at 09:00. Contains: full breakdown of the month just ended for both game types, total P&L, and updated all-time stats.

Weekly and monthly summaries are scheduled using a ticker inside the bot goroutine that checks the current time every minute.

---

## The `.bat` Launcher (updated)

The existing launcher script gets one new step at the top. Before opening any application, it calls `tracker.exe --check-lock`. If the exit code is 1, the launcher prints a message and exits. If the exit code is 0, it proceeds normally.

```batch
@echo off
C:\poker-tracker\tracker.exe --check-lock
if %ERRORLEVEL% neq 0 (
    echo PokerStars is locked until the hard stop expires. Check Telegram for details.
    pause
    exit
)
start "" "C:\Program Files (x86)\PokerStars\PokerStarsUpdate.exe"
start "" "C:\Program Files (x86)\Xeester\xeester-update.exe"
start "" "C:\StarsCaption\StarsCaption.exe"
exit
```

The path to `tracker.exe` in the `.bat` must be the full path or the binary must be on the system `PATH`.

---

## Windows Task Scheduler Setup

On system boot, Task Scheduler runs `tracker.exe --serve` as administrator. Administrator privileges are required to write to `lockfile.json` from a directory that requires elevated access and to run `taskkill.exe` against PokerStars reliably. The setup is:

1. Open Task Scheduler → Create Task
2. General tab: check "Run with highest privileges"
3. Triggers tab: New → "At startup" → Delay task for 30 seconds
4. Actions tab: Start a program → full path to `tracker.exe`, argument `--serve`, Start in `C:\poker-tracker`
5. Conditions tab: uncheck "Start the task only if the computer is on AC power"
6. Settings tab: check "If the task is already running, do not start a new instance" → uncheck "Stop the task if it runs longer than"

---

## Google Service Account Setup (one-time, done during installation)

1. Go to Google Cloud Console → create a new project
2. Enable the Google Sheets API for the project
3. Enable the Google Drive API for the project (needed once, during `--setup`, to find the spreadsheet by name)
4. Create a Service Account under IAM & Admin
5. Generate a JSON key for the service account → save as `credentials/google_service_account.json`
6. Copy the service account's email address (looks like `name@project.iam.gserviceaccount.com`)
7. Open the target Google Sheet → Share → paste the service account email → give Editor access

After this, the binary can read and write the sheet using the JSON key, with no browser login.

---

## Build Order

Each part must be fully working and independently verifiable before the next begins. Do not move to the next part until the current one is verified against real data.

### Part 1 — Parsers
Build `parsers/cash.go` and `parsers/tournament.go`. Test against real hand history files in `testdata/`. Verify stakes, net results, rake, VPIP, PFR, BB/100, and timestamps for cash. Verify Type, Multiplier (nil for MTT, float for Spin & Go), FinishPos, and TotalPlayers for tournaments.

### Part 2 — Config module
Build `config/config.go` with read/write for `config.json` and `lockfile.json`. Unit test all cases: missing file, null values, partial config, full config, expired lockfile.

### Part 3 — Google Sheets integration
Set up the service account (one-time manual step, see above). Build `sheets/sheets.go`. Test by pushing fake session structs and verifying that the correct rows appear in the correct tabs with correct values. Verify that the startup check creates missing tabs correctly.

### Part 4 — Watcher
Build `watcher/watcher.go` with `done chan struct{}` support. Wire it to the parsers and sheets module via `main.go`. Verify the full pipeline: play a session, sheet populates. Verify `main.go` correctly joins the base paths from `config.json` with the screen name before starting the watcher.

### Part 5 — Telegram bot, outgoing alerts only
Build `bot.go` for outgoing messages only. Chat ID auto-detection from first message. No commands yet. Verify post-session alert arrives on Telegram after a real session.

### Part 6 — Hard stop module
Build `hardstop/hardstop.go`. Test trigger, taskkill, lockfile write. Test `--check-lock` flag. Test the `.bat` launcher.

### Part 7 — Telegram bot, incoming commands
Add all commands to `bot.go`. Test each manually. Includes `/paths` and watcher restart. Test all inline keyboard flows end to end.

### Part 8 — `/start` wizard and scheduled summaries
Add `/start` wizard (`/paths` → `/alerts` → `/hardstop`). Add `bot.RunSetup` for `--setup` mode. Verify weekly/monthly summary queries and the full wizard flow.

### Part 9 — Manual packaging and Task Scheduler
Build final binary with `-ldflags` token injection. Copy manually to `C:\poker-tracker\`. Set up Task Scheduler. Full end-to-end test after reboot.

### Part 10 — Installer
Build `setup.iss`, `--setup` flag in `main.go`, `.github/workflows/release.yml`. Tag a release. Verify `Setup.exe` is published to GitHub Releases and installs correctly on a clean machine.
