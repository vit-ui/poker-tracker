# CLAUDE.md — Poker Tracker

This file is the authoritative reference for working on this project. Read it fully before doing anything.

---

## What This Project Is

A Go background service that automatically tracks a poker player's sessions on PokerStars, maintains bankroll accounting, and provides a Telegram bot interface for alerts and controls. It runs on Windows, starts on boot via Task Scheduler, and requires zero manual interaction during normal operation.

The full technical plan is in `poker-tracker-plan.md` at the repo root. Read it if you need context on a decision. This file covers what you need to work on the code.

---

## Repository Layout

```
poker-tracker/
├── cmd/
│   └── tracker/
│       └── main.go               ← entry point only. flag parsing, goroutine startup.
├── internal/
│   ├── watcher/
│   │   └── watcher.go            ← file system watcher
│   ├── parsers/
│   │   ├── cash.go               ← cash game hand history parser
│   │   └── tournament.go         ← tournament parser (Spin & Go and MTT)
│   ├── sheets/
│   │   └── sheets.go             ← google sheets integration
│   ├── bot/
│   │   └── bot.go                ← telegram bot, all commands and outgoing alerts
│   ├── hardstop/
│   │   └── hardstop.go           ← lock trigger, taskkill, lockfile write/read
│   └── config/
│       └── config.go             ← config.json and lockfile.json read/write with mutex
├── credentials/
│   └── google_service_account.json   ← gitignored
├── testdata/
│   ├── cash/                     ← real hand history .txt files for parser tests
│   └── tournaments/              ← real tournament summary .txt files (Spin & Go and MTT)
├── .github/
│   └── workflows/
│       └── release.yml           ← builds tracker.exe + Setup.exe on tag push
├── setup.iss                     ← inno setup installer script
├── config.json                   ← gitignored
├── lockfile.json                 ← gitignored, only exists when hard stop is active
├── .env                          ← gitignored
├── poker-tracker-plan.md         ← full technical plan
├── go.mod
└── go.sum
```

Keep this structure exactly. Do not add files outside of it without a clear reason.

---

## Environment Variables

Loaded from `.env` at startup via `github.com/joho/godotenv`. This file is written once by the installer and never changed manually.

```
GOOGLE_SERVICE_ACCOUNT_PATH=.\credentials\google_service_account.json
CONFIG_PATH=.\config.json
LOCKFILE_PATH=.\lockfile.json
```

These three values are static deployment paths that never change after installation. Everything else (sheet name, screen name, paths, chat ID) lives in `config.json` and is set via the bot.

`TELEGRAM_BOT_TOKEN` is not in `.env`. It is injected into the binary at build time via Go's `-ldflags` from a GitHub Actions secret. It is never stored in the repo or on disk.

---

## Commands

```bash
# build with bot token injected (GitHub Actions does this automatically on release)
GOOS=windows GOARCH=amd64 go build \
  -ldflags "-X main.BotToken=${TELEGRAM_BOT_TOKEN}" \
  -o tracker.exe ./cmd/tracker

# run the background service
./tracker.exe --serve

# run setup mode (called by the installer)
./tracker.exe --setup

# check lockfile state (used by the .bat launcher)
./tracker.exe --check-lock

# run all tests
go test ./...

# run tests for a specific package
go test ./internal/parsers/...

# copy binary to Windows after build
cp tracker.exe /mnt/c/poker-tracker/tracker.exe
```

`BotToken` is a package-level variable in `main.go` declared as `var BotToken string`. It is set by `-ldflags` at build time and passed to `bot.Run`. It is never read from `.env` or `config.json`.

---

## Dependencies

```
github.com/fsnotify/fsnotify
google.golang.org/api/sheets/v4
google.golang.org/api/drive/v3
golang.org/x/oauth2/google
github.com/go-telegram-bot-api/telegram-bot-api/v5
github.com/joho/godotenv
```

`drive/v3` is only used in `sheets.ResolveSheetID`, which is called once during `--setup`. It is never imported by or called from the running service.

Do not add dependencies without a concrete reason. This project's scope is fixed.

---

## Architecture

### `--serve` mode

Three goroutines communicate through two channels. This is the entire concurrency model — do not complicate it.

```
watcher goroutine   →[sessionChan]→   worker goroutine   →[alertChan]→   bot goroutine
```

- **watcher**: detects new/modified files, sends `FileEvent{Path, Type}` to `sessionChan`. Accepts a `done chan struct{}` — closing it stops the watcher.
- **worker**: receives from `sessionChan`, parses, writes to sheets, evaluates hard stop, sends alerts to `alertChan`. The "cash" branch is special: a file in the Hand History folder can be either a real cash hand or a tournament hand (PokerStars writes both there) — `ParseCashFile` returns exactly one of `*CashSession` or `*TournamentHandStats`, never both. See "Why Tournaments Need Two Files" below.
- **bot**: receives from `alertChan` and sends Telegram messages; also polls Telegram for incoming commands

**Path resolution.** `config.Paths.HandHistory` and `config.Paths.TournamentSummary` are the *base* folders — the parent directories PokerStars writes into, not including the player's screen-name subfolder (PokerStars always nests the actual files one level deeper, in a folder named after the screen name). `main.go` builds the real watched paths with `filepath.Join(cfg.Paths.HandHistory, cfg.Setup.PokerStarsScreenName)` and the tournament equivalent, then passes those joined paths to `watcher.Watch`. `watcher.go` itself has no knowledge of screen names — it just watches whatever paths it's given.

`main.go` passes a `restartWatcher` callback function to `bot.Run`. When `/paths` updates config, the bot calls this callback. The callback re-joins the base paths with the screen name, closes the current watcher's `done` channel, and starts a new watcher goroutine with the newly joined paths. The same `sessionChan` is reused — only the watcher goroutine is replaced.

If `config.json` has null paths on startup, the watcher does not start. The bot sends a message: "Run /paths to configure your PokerStars folder locations before the tracker can start." The watcher starts when `/paths` is called for the first time.

### `--setup` mode

A separate entry point. Starts only the bot goroutine in setup mode — no sheets client, no watcher. The bot walks the user through all required values via Telegram. On completion: writes `config.json`, copies credentials, registers Task Scheduler, exits with code 0. The installer window detects exit code 0 and shows the finish screen.

### `--check-lock` mode

Reads `lockfile.json`, checks expiry, exits with code 0 (proceed) or 1 (locked). Used by the `.bat` launcher.

`config.go` is the only module that reads or writes `config.json` and `lockfile.json`. It uses a package-level `sync.RWMutex` for safe concurrent access. All other modules call `config.go` — they never touch those files directly.

No other shared mutable state exists. If you find yourself needing shared state outside of `config.go`, stop and reconsider the design.

---

## Data Models

These structs are the contracts between parsers, sheets, hardstop, and the bot. Do not change field names without updating every consumer.

```go
// parsers/cash.go
type CashSession struct {
    Date        time.Time
    Stake       string    // e.g. "NL10"
    BuyInUSD    float64   // dollar value of 100bb at this stake
    Hands       int
    DurationMin int
    NetUSD      float64
    RakeUSD     float64
    BB100       float64
    VPIP        float64   // percentage, 0-100
    PFR         float64   // percentage, 0-100
}

// parsers/tournament.go
type Tournament struct {
    Date         time.Time
    TournamentID string    // extracted from "Tournament #<id>" in the summary header
    Type         string    // "spingo" or "mtt"
    BuyInUSD     float64
    PrizePoolUSD float64
    Multiplier   *float64  // only set for Spin & Go; nil for MTT
    FinishPos    int
    TotalPlayers int       // meaningful for MTT; always 3 for Spin & Go
    NetUSD       float64
    VPIP         *float64  // percentage, 0-100. nil until a matching hand file is parsed
    PFR          *float64  // percentage, 0-100. nil until a matching hand file is parsed
}

// parsers/cash.go — returned when a hand history file is a tournament hand,
// not a cash hand. The Hand History folder contains both.
type TournamentHandStats struct {
    TournamentID string    // extracted from "Tournament #<id>" in the hand header
    VPIP         float64
    PFR          float64
}

// sheets.go — used for ledger entries
type LedgerEntry struct {
    Date         time.Time
    Type         string    // "deposit" | "withdrawal" | "cash_session" | "tournament"
    AmountUSD    float64
    RunningTotal float64
}
```

---

## `config.json` Schema

All values start as `null`. Nothing is hardcoded. Values are set during the `--setup` flow or via bot commands.

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

Use pointer types (`*string`, `*float64`, `*int`) for all fields so that null and zero/empty are distinguishable. A null value means the feature is unconfigured. An empty string or zero is invalid — reject it during setup and config flows.

The `setup` section is written once during `--setup` mode and not exposed to the user afterwards. `telegram_chat_id` is auto-detected from the first message the user sends to the bot and saved here — it is never asked for explicitly.

`google_sheet_name` is asked during setup and used once to resolve the spreadsheet ID via the Drive API. The resolved ID is saved as `google_sheet_id`. All subsequent runs use `google_sheet_id` directly — the Drive API is never called again at runtime. `NewClient` in `sheets.go` accepts the spreadsheet ID, not the name. `ResolveSheetID` in `sheets.go` is the only function that uses the Drive API and is only called during `--setup`.

`paths.hand_history` and `paths.tournament_summary` are **base folders only** — the parent directory PokerStars writes into (e.g. `C:\Users\VITOR\AppData\Local\PokerStars\HandHistory`), never including the screen-name subfolder PokerStars creates underneath it. The screen-name subfolder is appended automatically at runtime using `setup.pokerstars_screen_name` — see the Path Resolution note in the Architecture section above. Do not ask the player for the subfolder path; asking for the base folder is what `/paths` collects.

---

## `lockfile.json` Schema

```json
{
  "triggered_at": "2025-01-15T22:30:00Z",
  "expires_at":   "2025-01-16T10:30:00Z",
  "reason":       "session_loss_buyins",
  "duration_hours": 12
}
```

This file only exists on disk while a hard stop is active. `hardstop.go` creates it. `hardstop.go` deletes it. Nothing else writes to it.

---

## Why Tournaments Need Two Files (and Cash Doesn't)

Cash game hand histories record real dollars at every point — each hand shows the player's USD stack before and after, so net result, rake, and everything else comes from that one file alone.

Tournaments are structurally different: play happens in chips, not dollars. A chip's dollar value depends on how the player finishes and the prize structure, which isn't known until the tournament ends. The tournament **hand history** file (in the Hand History folder, alongside cash hands) only ever contains chip actions — it has no buy-in, no payout, no dollar net result, because PokerStars itself doesn't know the dollar outcome yet. That data is written once, at the end, to the **tournament summary** file (in TournamentSummaries). Neither file is a superset of the other: the hand file is the only source for VPIP/PFR, the summary file is the only source for buy-in/payout/finish.

Because the two files arrive independently and in no guaranteed order, they are correlated by `TournamentID` (extracted from `Tournament #<id>` in both files' headers) on the same Tournaments tab row, rather than kept as two separate records or handled with in-memory pending state. See `AppendTournament` and `UpdateTournamentVPIPPFR` in `sheets.go`.

---

## Google Sheets Tabs

The spreadsheet structure is created by `sheets.go` on startup if it does not exist. The tab names and column headers below are canonical — do not change them, as the Dashboard tab's formulas depend on exact column positions.

| Tab | Written by |
|---|---|
| Dashboard | Never (formulas only) |
| Cash Sessions | Script |
| Tournaments | Script |
| Bankroll Ledger | Script + bot (`/deposit`, `/withdraw`) |
| Cash Stats | Never (formulas only) |
| Tournament Stats | Never (formulas only) |
| Time Analysis | Script |

**Cash Sessions columns (in order):**
`Date, Stake, Hands, Duration (min), Net (USD), Rake (USD), BB/100, VPIP, PFR`

**Tournaments columns (in order):**
`Date, Type, Tournament ID, Buy-In (USD), Prize Pool (USD), Multiplier, Finish Position, Total Players, Net (USD), VPIP, PFR`

For Spin & Go rows, Multiplier is the float value. For MTT rows, Multiplier cell is left empty.

**Tournament rows are correlated by Tournament ID, not appended blindly.** A tournament's buy-in/payout data and its VPIP/PFR come from two separate PokerStars files (the summary file and the hand history file) that arrive independently — whichever arrives first creates the row with whatever it has; the second to arrive finds that row by Tournament ID and fills in the rest. See `AppendTournament` and `UpdateTournamentVPIPPFR` below. VPIP/PFR cells are blank until a hand file for that tournament is parsed — the Hand History folder contains both cash hands and tournament hands, and PokerStars gives no dollar data for tournaments in the hand file itself (see "Why Tournaments Need Two Files" below).

**Bankroll Ledger columns (in order):**
`Date, Type, Amount (USD), Running Total (USD)`

**Time Analysis columns (in order):**
`Date, Hour, Day of Week, Net (USD)`

---

## Telegram Commands

Every command must have a `/help <command>` entry that explains what it does, its parameters, and a usage example. `/help` with no argument lists all commands.

| Command | Behavior |
|---|---|
| `/start` | Runs the full setup wizard: `/paths` first, then `/alerts`, then `/hardstop`. Re-runnable at any time. |
| `/deposit <amount>` | Logs a deposit. Amount is the USD value shown on PokerStars after the deposit clears — not BRL. Writes to Bankroll Ledger. Replies with new bankroll. |
| `/withdraw <amount>` | Logs a withdrawal. Amount is the USD value shown on PokerStars — not BRL. Writes to Bankroll Ledger. Replies with new bankroll. |
| `/status` | Current bankroll, today/week/month P&L, active lock info if any. |
| `/lastsession` | Full summary of the last parsed session. |
| `/stats [cash\|spins\|mtt] [today\|week\|month\|all]` | Filtered stats. Defaults to all-time if no args. `spins` filters Tournaments tab by Type == "spingo". `mtt` filters by Type == "mtt". |
| `/sheet` | Returns the Google Sheets URL. |
| `/paths` | Inline keyboard flow to set the *base* hand history and tournament summary folder paths (the parent folder, not the screen-name subfolder underneath it — that part is appended automatically). Triggers watcher restart on save. Required before the watcher can start. |
| `/alerts` | Inline keyboard flow to set alert thresholds. |
| `/hardstop` | Inline keyboard flow to set hard stop rules and duration. |
| `/hardstop trigger` | Manually activates a hard stop. Requires `duration_hours` to be configured. No cancel. |
| `/config` | Lists `/paths`, `/alerts`, and `/hardstop` with descriptions. No logic of its own. |
| `/help [command]` | Help text. |

### Inline keyboard flows (`/alerts` and `/hardstop`)

Both follow the same pattern:
1. Bot sends message with one button per configurable field plus a "Done" button
2. Player taps a field
3. Bot asks for the new value as a plain text reply
4. Bot validates (must be a positive number, not zero), writes to `config.json`, confirms
5. Bot re-sends the keyboard so the player can continue or press Done

---

## Automatic Outgoing Alerts

These fire without any command from the player. All are sent only if the relevant config value is non-null, except the post-session summary which always fires.

| Alert | Trigger |
|---|---|
| Post-session summary | Every session. Game type, stake/buy-in, net, hands/position, duration, current bankroll. |
| Downswing alert | Bankroll drops `downswing_buyins` below this month's peak. |
| Losing streak alert | `losing_streak_count` consecutive losing sessions (across both game types). |
| Spin & Go drain warning | Spin & Go losses today exceed `spingo_daily_loss_usd` in the alerts config. Filters Tournaments tab by Type == "spingo". |
| Stake change detected | Current cash session stake differs from previous cash session stake. |
| Hard stop triggered | Any hard stop condition fires or `/hardstop trigger` is used. Includes reason and expiry time. |
| Weekly summary | Every Monday 09:00. Volume, P&L, BB/100, Spin & Go ROI for the past 7 days. |
| Monthly summary | 1st of every month 09:00. Full breakdown of the month just ended. |

Weekly and monthly summaries are triggered by a ticker in the bot goroutine that checks the current time every minute.

---

## Hard Stop Behavior

### Trigger conditions (any one is sufficient)
- Session net loss exceeds `session_loss_buyins × BuyInUSD` from the parsed `CashSession`
- `consecutive_losses` losing sessions in a row (read from recent Bankroll Ledger rows)
- Spin & Go losses today exceed the `spingo_daily_loss_usd` value in the hardstop config
- Manual `/hardstop trigger` command

### On trigger
1. `exec.Command("taskkill.exe", "/IM", "PokerStarsUpdate.exe", "/F").Run()`
2. Write `lockfile.json`
3. Send alert to `alertChan`

### Exported functions in `hardstop.go`
- `Evaluate` — called by worker after every cash session
- `EvaluateTournament` — called by worker after every tournament (cash or Spin & Go). Filters by `Type == "spingo"` internally when checking the daily loss threshold.
- `ManualTrigger` — called by the bot for `/hardstop trigger`. Reads duration from config, returns error if `duration_hours` is nil, otherwise calls the unexported `trigger`.

### On `--check-lock` flag
- No `lockfile.json` → exit code 0
- `lockfile.json` exists, not expired → print message with expiry time, exit code 1
- `lockfile.json` exists, expired → delete file, exit code 0

There is no cancel mechanism. The lock runs its full duration.

---

## Coding Conventions

- **Error handling**: never use `panic` outside of `main.go` startup validation. All other errors return as `error` values. Log errors with context (`fmt.Errorf("parsing cash session: %w", err)`).
- **Logging**: use `log/slog` with structured logging. Info level for normal events, error level for failures. No debug logs left in committed code.
- **No global variables** except the channel declarations in `main.go`. Pass dependencies explicitly.
- **Config access**: always go through `config.go`. Never read `config.json` directly from another module.
- **Formatting**: `gofmt` before every commit. No exceptions.
- **Tests**: every parser function has a corresponding test in `parsers/*_test.go` using real anonymised hand history samples stored in `testdata/`. Do not use mocks for the parsers — test against real file content.
- **Comments**: exported functions and types get a doc comment. Internal logic gets inline comments only where the reason is non-obvious. Do not comment what the code does — comment why.

---

## Build Order

Work strictly in this order. Do not start a part until the previous one is verified against real data or real interactions.

1. **Parsers** — `cash.go` and `tournament.go`. Verified against real hand history files.
2. **Config module** — `config.go`. All cases unit tested: missing file, null values, partial config, full config, expired lockfile.
3. **Google Sheets** — `sheets.go`. Verified by pushing real parsed structs and checking the sheet.
4. **Watcher** — `watcher.go` with `done chan struct{}`. Full pipeline end-to-end: play a session, sheet populates.
5. **Bot, outgoing only** — post-session alert and threshold alerts. Verified with real session.
6. **Hard stop** — trigger, taskkill, lockfile, `--check-lock`. Verified with `.bat` launcher.
7. **Bot, incoming commands** — all commands except `/start`. Each tested manually. Includes `/paths` and watcher restart.
8. **`/start` wizard and scheduled summaries** — full setup flow (`/paths` → `/alerts` → `/hardstop`) and time-based triggers.
9. **Task Scheduler and manual packaging** — final binary built with `-ldflags`, manual Task Scheduler setup, full end-to-end after reboot.
10. **Installer** — `setup.iss`, `--setup` flag in `main.go`, `.github/workflows/release.yml`. Verified by tagging a release and running the produced `Setup.exe`.

---

## Code Must Be Lean — Non-Negotiable

This is a single-user tool running on one machine. It is not a production service. Do not write it like one. Every rule below is a hard constraint, not a style suggestion.

**No unnecessary abstractions.**
If a function is called exactly once, inline it. If an interface has exactly one implementation, use the concrete type. Do not create wrapper types around primitives. Do not create adapter layers between packages unless there is a concrete reason stated in this file.

**No defensive code that the program flow makes impossible.**
If a value cannot be nil at a given point because the startup validation already checked it, do not add a nil check. If a file write cannot fail because the path was already validated, do not add a fallback. Only handle errors that can actually occur at that point.

**No extra validation layers.**
Validate user input once — at the entry point where it arrives (Telegram command handler, `.env` loader). Do not re-validate the same value as it passes through functions. A positive float64 that was validated when the user typed it does not need to be validated again when it reaches `sheets.go`.

**No retry logic unless explicitly required.**
Do not add retry loops to API calls. If a Sheets write fails, log it and move on. If the Telegram send fails, log it and move on. The plan does not specify retry behavior anywhere, so there is none.

**No context.Context threading.**
The three goroutines communicate through channels. That is the cancellation and coordination mechanism. Do not add `context.Context` parameters to functions unless a library explicitly requires it in its function signature.

**No timeouts beyond library defaults.**
Do not set custom HTTP timeouts, dial timeouts, or deadline contexts. The Google and Telegram libraries have sensible defaults. Leave them alone.

**No logging of function entry or exit.**
Log meaningful events only: session parsed, alert sent, hard stop triggered, sheet tab created, startup complete, error occurred. Do not log "entering function X" or "leaving function X".

**No metrics, no telemetry, no instrumentation.**
The plan does not include any. Do not add any.

**No "just in case" code paths.**
Do not add alternate strategies, fallback implementations, or feature flags. One implementation per feature. If the primary approach fails during development, fix it — do not work around it with a second approach.

**Tests test behavior, not implementation.**
Test what a function returns given an input. Do not test private functions. Do not test that a specific internal method was called. Parser tests use real hand history files from `testdata/` — not mocks, not generated strings.

**No comments that describe what the code does.**
A comment that says `// increment counter` above `counter++` is noise. Comments explain why something is done a non-obvious way. If the code is obvious, no comment is needed.

---

## Source Control

### Commits

Stage files by name — never `git add -A` or `git add .`.

Commit each logical unit of work as its own commit. A unit is typically one build-order part, one subsystem, or one cohesive change. Do not bundle unrelated files. Do not leave work uncommitted at the end of a session.

**Message format:**
- Title: imperative mood, ≤ 72 chars, no trailing period ("Add", "Fix", "Rename" — not "Added" or "Fixes")
- Body: one or two context sentences, then bullet points for notable details
- Always end with the `Co-Authored-By` trailer using the exact model name that wrote the code

Pass the message via heredoc to preserve formatting:
```
git commit -m "$(cat <<'EOF'
Title here

Body here.
- detail
- detail

Co-Authored-By: Claude <model-name> <noreply@anthropic.com>
EOF
)"
```

### Tags

Each tag triggers the GitHub Actions release build and produces `tracker.exe + Setup.exe`. One tag per completed build order part:

| Tag   | Build Order Part |
|-------|-----------------|
| v0.1  | Repo scaffold |
| v0.2  | Part 1 — Parsers |
| v0.3  | Part 2 — Config module |
| v0.4  | Part 3 — Google Sheets |
| v0.5  | Part 4 — Watcher |
| v0.6  | Part 5 — Bot outgoing alerts |
| v0.7  | Part 6 — Hard stop |
| v0.8  | Part 7 — Bot incoming commands |
| v0.9  | Part 8 — `/start` wizard + scheduled summaries |
| v0.10 | Part 9 — Manual packaging + Task Scheduler |
| v1.0  | Part 10 — Installer |

**Tag only when the build order part meets its full verification criterion** as written in the Build Order section — tests passing alone is not sufficient. Tag immediately in the same session as verification, and push immediately: `git push origin <tag>`.

---

## What Not To Do

- Do not add a cancel mechanism for hard stops.
- Do not poll the hand history folder. Use `fsnotify` events only.
- Do not write to `config.json` or `lockfile.json` from any module other than `config.go`.
- Do not send Telegram messages from any goroutine other than the bot goroutine. Use `alertChan`.
- Do not hardcode any threshold, duration, stake name, or dollar value. Everything configurable lives in `config.json`.
- Do not create the spreadsheet or its tabs manually. `sheets.go` handles this on startup.
- Do not add a new Google Sheets tab without updating the startup check in `sheets.go`.
- Do not add dependencies not listed in this file.
- Do not generate boilerplate (getters, setters, builder patterns). Access struct fields directly.
- Do not add a configuration file format other than the `.env` and `config.json` already specified.
- Do not add a web interface, REST API, or any network interface other than the Telegram bot.
- Do not add database storage. Google Sheets is the data store.
- Do not read `HAND_HISTORY_PATH` or `TOURNAMENT_SUMMARY_PATH` from `.env`. They live in `config.json` under `paths`.
- Do not read `TELEGRAM_BOT_TOKEN` from `.env` or `config.json`. It is a build-time constant injected via `-ldflags`.
- Do not ask the user for their Telegram chat ID. Auto-detect it from the first message they send to the bot and save it to `config.json`.
- Do not store or ask for the full path including the PokerStars screen-name subfolder in `paths.hand_history` / `paths.tournament_summary`. Store the base folder only; join with `setup.pokerstars_screen_name` at the point the watcher is constructed.
