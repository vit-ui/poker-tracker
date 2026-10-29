# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Releases are tagged `vX.Y`, and each GitHub Release
pulls its notes from the matching section below.

## Unreleased

### Fixed

- Deposits and withdrawals use stable, namespaced operation IDs in both stores,
  exports, migration and durable Sheets fallback. Recovery reconciles remotely
  accepted writes before replay; equal intentional deposits stay distinct.
- Sheets recovery preserves newer tournament re-entry, cash and bounty corrections
  even with unchanged hand counts. Replay order is deterministic, and the bot's
  existing ticker requests worker-owned draining when idle. Replies distinguish
  recorded movements, durable local queueing and local save failures.

- Linux configuration restarts use the owning systemd user service rather than
  Windows restart commands. Setup and restart share the server installation
  identity; a manually launched process cannot restart another installation.

- Bot money movements and tournament answers share the file-processing worker;
  incoming updates, reminders and replies run on one bot event loop. Pending
  tournament queue changes use atomic config-owned updates and replacement.
- Storage migration requires the Windows collector to confirm PokerStars is
  closed. It drains pending file events, finalizes pending cash, copies on the
  worker and holds writes until restart or rollback. Failed cash persistence
  retains the pending snapshot and does not send a successful session summary.

- Split transport serializes complete command frames with reconnect lock replay.
  Replayed locks precede newer commands; replaced connections and listener
  shutdown release blocked uploads and close their owned sockets.

- Concurrent storage writes preserve ledger amounts and running totals. SQLite
  commits each session, tournament, bounty correction or money movement in one
  transaction; a failed ledger write rolls back the associated result changes.
  Sheets serializes complete mutation sequences, including fallback replay.
- Backend migration verifies destination records, stable IDs and independently
  summed ledger amounts, in addition to counts and bankroll. It detects altered
  amounts even when copied running totals match, while preserving partial
  tournaments, folded bounties and the documented Sheets date precision.
- Reject NaN and infinite amounts or thresholds before saving them. Both
  storage backends also validate numeric inputs before writes or fallback updates.
- Windows setup and uninstall stop tracker processes by their executable's
  installation directory instead of terminating every tracker installation.
  Early setup reads configuration and credentials from the selected directory.
- Windows setup, startup, restart and uninstall use matching task identities.
  Disposable builds can use separate tasks, installation folders and uninstall
  registry entries while production keeps its existing names.

## v4.4 - 2026-08-04

### Fixed

- **`/scan` no longer floods the Google Sheets quota.** On the Sheets backend a
  large backfill recorded one file at a time with no pacing, firing hundreds of
  API calls in seconds and blowing Google's per-user limit (60 reads/min and
  60 writes/min) — a real scan produced ~412 consecutive `429 Too Many Requests`
  errors, and a `/deposit` right after it failed with "Something went wrong"
  because the quota was still exhausted. Every Sheets call now passes through a
  rate limiter (~50 calls/min, small idle burst) at the single HTTP choke point,
  so a scan, a queued-write replay, and normal use all stay under quota. The
  write-ahead fallback still catches the occasional failed write, and can now
  actually drain instead of re-hitting the limit. A big first scan takes a few
  minutes and runs quietly; nothing is lost.

- **A failed `/deposit` or `/withdraw` is no longer silently lost** (Sheets
  backend). Deposits and withdrawals now use the same write-ahead fallback as
  session and tournament writes: if the Sheets write fails (a transient outage,
  a rate-limit spike), the entry is queued and replayed on the next successful
  write, with its running total recomputed so it still totals correctly.

## v4.3 - 2026-08-04

### Added

- **`/scan` — backfill from your PokerStars folders.** Records every cash session
  and tournament sitting in your hand-history and tournament-summary folders that
  isn't stored yet — handy right after a new install to seed your history. It
  records **silently**: no per-session summaries, no hard stops, no alerts.
  Tournaments that need an answer (a satellite's ticket-vs-cash, or a freeroll's
  buy-in) are skipped and counted — play or re-open those in live to record them.
  Safe to run more than once (already-stored sessions aren't duplicated); you get
  one message with the counts when it finishes. Works in both the all-local and
  split client/server builds. On the Google Sheets backend a large first scan can
  take a few minutes to fully sync (Sheets rate-limits writes) — data is never
  lost (the write-ahead fallback queues and replays it), and re-running `/scan`
  once it settles reports the full totals.

### Changed

- **Cash sessions are recorded once, when the session ends, instead of on every
  hand.** A cash session now writes to storage (its Cash Sessions row and its
  Bankroll Ledger entry) a single time, once the table has gone quiet for the
  session-end window — the same moment the post-session summary is sent, and the
  same way tournaments have always recorded. The ledger-based checks (downswing,
  losing streak, daily loss) and the post-session summary run at that point too.
  The **session-loss hard stop and its warning still run on every hand**, from
  the just-parsed session, so a session that's bleeding is still stopped
  mid-play. One consequence: `/status`, `/stats`, and `/lastsession` don't
  reflect a cash session until it ends (a few minutes after your last hand).

### Removed

- **The Time Analysis tab/table is gone.** It was never used. `/export` now
  sends three CSV files (cash sessions, tournaments, bankroll ledger) instead of
  four, and backend migration copies three data sets. On the SQLite backend the
  `time_analysis` table is dropped automatically on the next start. On the Google
  Sheets backend the physical "Time Analysis" tab is left untouched (the tracker
  simply stops writing to it) — delete it by hand if you want it gone.

### Fixed

- **A slow memory creep in the long-running service.** After the shift to
  recording cash sessions at session end, each cash table's quiet-period timer
  was left in an internal map once its session was written, so the map grew by
  one small entry per distinct hand-history file (a new one per table per day)
  for the life of the process. The timer is now dropped when its session ends, so
  the map stays bounded. No effect on stored data, alerts, or enforcement.

## v4.2 - 2026-08-01

### Changed

- **The client↔server connection drops a stalled, unauthenticated peer instead of
  waiting on it forever.** The server now closes any connection that completes the
  TLS handshake but fails to present its pairing token within a few seconds, so a
  peer that opens a socket and then goes silent can no longer tie up server
  resources. This matters most when the server runs on a public machine (a VPS):
  reading and writing were always gated by the token and the pinned certificate —
  a stranger still cannot read your data, inject sessions, or send commands — but
  the mere act of connecting is now bounded too.
- **The connection requires TLS 1.3.** Both ends are the tracker's own code, so the
  link now refuses anything older than the newest TLS version — no possibility of
  negotiating down to an older, weaker one.

## v4.1 - 2026-08-01

### Changed

- **The installer asks "Use Telegram?" first, and the Telegram path now sets up storage too.**
  Previously the installer asked your storage backend on its own page before the Telegram-or-manual
  choice. Now it asks how you want to set up first: pick **Telegram** and you choose everything —
  storage backend included — by chatting with the bot; pick **Manual** and the storage page (and the
  rest) stay in the installer as before. One fewer installer page when you use Telegram, and the
  storage question lives in one place per path instead of being asked on a native page you might not
  expect.
- **The Windows server installer now offers the same "configure from Telegram (recommended) vs set
  storage here now" choice the Linux server already had.** Choosing *from Telegram* stores your data
  in a local file to start (switch to Google Sheets anytime with `/storage`) and lets you configure
  storage, alerts, and rules from your phone after pairing — no storage decision needed at install
  time. The two server installers now behave the same.

### Fixed

- **The Windows installer builds again.** A line in the installer script began with `#13#10`, which
  Inno Setup reads as a preprocessor directive at the start of a line and rejects (`Unknown
  preprocessor directive`), aborting compilation of `PokerTracker-Setup.exe`. The line is
  rejoined so it no longer starts with `#`, and the installer compiles cleanly again.

## v4.0 - 2026-07-31

### Changed

- **One installer per OS, download-and-run — no CPU choice.** The Windows release
  is now a single `PokerTracker-Setup.exe` that carries both Intel/AMD and ARM
  builds and installs the matching one automatically (previously two separate
  per-CPU installers). The installer's role page gains a third option, **"Run a
  server on this PC"**, so a Windows server is set up from the same installer — it
  asks the same storage choice (Local file or Google Sheets) as the all-local
  install, and handles the certificate, pairing token, boot task, and connection
  string for you.
- **One-file Linux server installer.** The Linux server ships as
  `PokerTracker-Server-linux.tar.gz`; extract it (file manager or `tar xzf`) and
  run the single `PokerTracker-Server-linux` — it detects your CPU, installs the
  right build, and sets up the service. It opens a **setup window** on a desktop
  (falling back to a terminal on a headless box), replacing the previous
  extract-and-run-a-shell-script flow.
- **The Linux server no longer needs root to install.** It installs into your
  home folder and registers a per-user service (`systemctl --user`). The one step
  that needs a password — starting on boot without logging in — is now a single
  graphical prompt on a desktop (or one `sudo` in a terminal), and can be skipped
  (the server then runs while you're logged in).

## v3.8 - 2026-07-30

First public release of the optional split deployment. The pieces landed green across the
v3.5–v3.7 work; v3.8 is the release that ships them, plus the fixes and polish below.

### Added

- **Optional split deployment.** Run collection and storage on separate machines:
  `tracker-server` (stores your data, runs the Telegram bot, enforces the rules; Windows or
  Linux) and `tracker-client` (watches the PokerStars folders, streams file changes to the
  server, enforces hard-stop locks; Windows). They connect over one TLS connection the client
  dials to the server, authenticated by a pinned self-signed certificate and a shared token
  carried in a `tracker://host:port/token#fp=...` connection string the server prints at
  setup. The installer now opens with a role page — **"Run everything on this PC"** (the
  default, all-local, unchanged for existing users) vs **"Connect to a server"**. The server
  registers its own boot service with `tracker-server --install` (systemd on Linux, Task
  Scheduler on Windows). **The all-local build (`tracker`) is unchanged** — it's the same two
  roles wired together in one process.
- Faster hand-history parsing: a growing session file resumes from where the last parse left
  off instead of re-reading it from the top on every hand. The result is identical to a full
  parse; a rewritten or shortened file transparently re-parses from scratch.

### Fixed

- **Cross-timezone date windows in split mode.** The server now computes every date boundary
  (poker day, `/status`, `/stats`, the 30-day downswing, weekly/monthly summaries, and
  deposit/withdrawal timestamps) on the poker PC's wall clock, so a server running in another
  timezone reports the player's local poker day correctly and both storage backends timestamp
  transfers identically. The all-local build is unaffected.
- **Hard-stop lock delivery across reconnects.** A lock decided while the poker PC is briefly
  disconnected is now held and delivered when it reconnects, instead of being dropped
  (re-delivery is idempotent; an already-expired lock is not re-sent).

### Changed

- The split `tracker-server` and `tracker-client` keep their idle memory footprint small like
  the all-local build: the client caps tightly (it only watches files and enforces locks),
  the server keeps a lean idle floor with headroom for whole-tab `/export` and `/stats` reads.
  Runtime tuning only — no change to stored data, alerts, or enforcement.

## v3.7 - 2026-07-30

### Added

- **Optional split deployment (build-to-green; not yet released).** Two new binaries let you run collection
  and storage on separate machines: `tracker-server` (stores data, runs the Telegram bot, enforces the
  rules; Windows or Linux) and `tracker-client` (watches the PokerStars folders, streams file changes to the
  server, enforces hard-stop locks; Windows). They connect over one TLS connection the client dials out to
  the server, authenticated by a pinned self-signed certificate and a shared token bundled in a
  `tracker://host:port/token#fp=...` connection string the server prints at setup. Date windows on the
  server anchor to the poker PC's clock, so a server in another timezone still reports the player's local
  poker day. **The all-local build (`tracker`) is unchanged** — it's the same two roles wired in one process
  over an in-process link. Cross-machine networking and installers are still being verified; this entry
  covers the code landing, not a released feature.

### Changed

- The collector's per-file sent-offset map is now bounded the same way the server's parse-state map is
  (evict entries idle past a TTL once the map exceeds a cap), so a collector running for months without a
  restart can't grow it without limit. Internal only — no user-visible change; a wrongly-evicted file just
  re-sends whole once, which the server rebuilds from scratch.

## v3.6 - 2026-07-29

### Changed

- Internal restructuring toward the upcoming client/server split — no user-visible change. The one process
  is now organized into two roles connected by an in-process link: a **collector** (`internal/client`) that
  watches the PokerStars folders and sends each file's new bytes up, and a **server** (`internal/server`)
  that reconstructs the file, parses it, writes storage, evaluates the hard stop and alerts, and runs the
  Telegram bot. Message types live in `internal/transport`. The hard stop is now split too: `internal/hardstop`
  only *decides*, and enforcement (killing PokerStars, writing the lockfile) is carried out by the collector.
  All behavior — stored data, alerts, hard-stop locking, `/export` — is unchanged, verified by the existing
  end-to-end pipeline suite driving the real watcher → collector → server → sqlite path with no assertion
  changes.

## v3.5 - 2026-07-29

### Changed

- Hand-history files are now parsed **incrementally**. PokerStars appends one hand at a time and the
  watcher fires an event per hand, so the parser used to re-scan the entire growing file from the start on
  every hand — O(n²) work over a long session. The parser now carries per-file state (last byte offset plus
  the running aggregates) and, on each event, reads only the newly-appended bytes. Results are byte-for-byte
  identical to a full parse — verified by a test that grows every fixture one hand at a time and compares
  each step to a from-scratch parse, plus a dev-only sweep over 158 real hand histories (cash, PLO, HORSE,
  tournaments). The optimization is self-healing: if a file is ever rewritten rather than appended to, it
  transparently falls back to a full re-parse. No change to any stored value, alert, or the tournament
  summary parser. (Foundation for the upcoming client/server split, where the same offset lets the collector
  send only new bytes over the network.)

## v3.4 - 2026-07-29

### Changed

- Telegram error messages are now plain-language and no longer flood. Previously every internal failure
  was force-forwarded to Telegram as a raw developer log line (`⚠️ updating tournament hand stats err=…`),
  so a brief network hiccup could spray a dozen cryptic messages. Errors now go to `tracker.log` (added in
  v3.3); Telegram gets only the few things you can actually act on, in readable form:
  - A short Sheets outage now sends **one** "💔 Sheets briefly unreachable — your results are saved
    locally and I'll sync them automatically when it's back," instead of one raw error per hand. The
    existing "✅ Synced N" still confirms recovery. (The 30-minute heartbeat only catches outages that are
    still going when it samples; this covers the short blips it would miss.)
  - The connectivity heartbeat no longer tacks a raw Go error string onto its "💔 Sheets unreachable"
    message.
  - Hard stop alerts name the reason in words ("Session loss limit reached") instead of the internal key
    (`session_loss_buyins`).
  - If a poker file shows up before your screen name is configured, you get one clear prompt to set it
    (via /menu → Setup → Screen name) rather than a repeated raw skip message.
  - If file watching stops unexpectedly, you get one plain notice to restart.

## v3.3 - 2026-07-29

### Added

- Local log file. The background service now writes to `tracker.log` (next to `config.json`), so you can
  see the actual errors and what the tracker was doing — previously the only window into failures was
  Telegram, and everything below error level was discarded entirely. The log captures every error in full
  technical detail plus a handful of milestones (service start, watcher start/restart, hard stop triggered,
  backend migration, screen-name set at runtime). It's size-capped at 5 MB with a single `tracker.log.old`
  rollover, so it never grows without bound and survives restarts. Telegram behaviour is unchanged in this
  release — errors still go there as before, now also on disk.

## v3.2 - 2026-07-25

### Fixed

- Freeroll / Kick-Off tournaments (no buy-in line in the summary) were silently dropped — including the real
  knockout-bounty cash they pay out, which never reached your bankroll, `/status`, `/stats`, or `/export`.
  They're now recorded: the tracker asks, per tournament, whether you entered **🆓 Free** (won your way in) or
  **💵 paid** (enter the amount), then logs the row with the bounty folded in. The question re-sends every 30
  minutes until answered, like the satellite/ticket prompt.

### Added

- Sheets connectivity heartbeat: on a Sheets-backed install, going unreachable now sends a Telegram message
  ("💔 Sheets unreachable since HH:MM"), repeated every 30 minutes while still down, with one "💓 Sheets
  reachable again" message on recovery. Silent while healthy — no message unless something's actually wrong.
- Sync confirmation: once a write that failed and got queued locally (`sheets_fallback.json`) successfully
  replays, a "✅ Synced N queued write(s) to Sheets" message confirms it — previously that queue drained
  silently with no way to tell it had recovered.

### Changed

- Telegram error messages for a failed cash session write, tournament write, hand stats update, or
  stake-change check now include the date, stake, and session ID (or tournament ID) of the specific record
  affected, instead of just the raw API error.

### Internal

- Test suite hardened against silent regressions: added an end-to-end suite that drives the real file-watcher
  → worker → local database with committed sample hand-history/summary files and checks the resulting
  bankroll, ledger, and alerts. Test fixtures are now committed (anonymized) so the suite runs on any
  checkout, and no real screen name or tournament ID remains in the source.

## v3.1 - 2026-07-23

### Changed

- Turning off an alert or hard-stop threshold now **keeps its number** instead of erasing it. Each threshold
  has an on/off switch: off stops it firing but remembers the value, so you can switch it back on from the
  menu without retyping. A set-but-off threshold shows **Turn on**; turning a hard stop off asks for
  confirmation first (it's a safety rule) and explains what it protects.
- Menu polish: the button to close a menu now reads **Done** (was Cancel), and a threshold's screen keeps a
  stable button order (Change value, then the on/off toggle) instead of the two swapping places between
  states.
- Release workflow's `build` job now runs on `ubuntu-latest` instead of `windows-latest`. The
  `go build GOOS=windows` cross-compile step never needed a Windows host; this removes one of the two slow
  Windows-runner boots from the release pipeline. The `installer` job stays on `windows-latest` since Inno
  Setup requires it.

## v3.0 — 2026-07-23

### Added

- Notification controls for the always-on messages. Each result summary — cash session, tournament, weekly,
  and monthly — now takes a mode: send **every** one, only **wins**, only **losses**, or **off**. The
  stake-change note can be switched off too. Set them in the new **`/notify`** menu (Summaries + Alerts); all
  default to the previous always-on behavior. The threshold warning alerts are unaffected — those already only
  fire on a loss.

### Changed

- New `/menu` command — one grouped hub for everything: Setup (paths, schedule, screen name, storage, and on
  Sheets installs sheet/credentials), Notifications, Hard stop, Info (status, last session, stats, export),
  Money (deposit, withdraw), and Help. Drill in and tap Back to step out. Every item is still its own command,
  and `/start` now opens this menu.
- The `/paths`, `/alerts`, `/hardstop`, and `/schedule` settings menus are now cascading, BotFather-style: one
  message you drill into and back out of, with a Back button and no more Done. Each alert/hard-stop threshold
  has its own screen where you can change it or **turn it off** (clearing the value) — previously there was no
  way to unset one from the menu. The `/paths` watcher restart and the hard-stop "set a duration" guard are
  preserved (the guard now warns when you leave the hard-stop menu). The old forced paths→alerts→hard
  stop→schedule wizard is gone.
- Migrating a local-file install to Google Sheets with `/storage sheets` no longer errors when there's no
  service-account key on the machine — it prompts you to upload the `google_service_account.json` right there
  and continues the migration once you do.

## v2.9 — 2026-07-22

### Changed

- The per-hand evaluation path fetches its data once per file event instead of once per check: the Bankroll
  Ledger was being downloaded up to six times and the Tournaments tab twice for every single hand (hard stop
  checks and their alert twins each fetched their own identical copy), which with several tables open could
  brush the Google Sheets per-minute request quota. Both backends now do one ledger read and at most one
  tournaments read per hand; `config.json` is likewise read once per event instead of ~7 times. Alert
  messages, hard stop decisions, and stored rows are unchanged.
- The Sheets ledger-cascade update no longer re-reads a cell it already fetched, saving one API call on every
  hand after a session's first.

### Fixed

- CLAUDE.md claimed the cash post-session summary waits 10 minutes of table quiet; the code says 5. Corrected
  the doc to match the code.

## v2.8 — 2026-07-22

Fixes from a code review of the v2.6/v2.7 changes.

### Fixed

- A satellite answered "used a ticket" could be asked about again on a stray file event (its recorded $0
  buy-in never matched the file's real buy-in), and a second "paid cash" answer would have overwritten the
  correct record with a cash loss. Ticket-resolved tournaments are now correctly recognized as already
  handled.
- Re-entering a satellite while its ticket/cash question was still unanswered silently discarded the re-entry:
  the rewritten summary was dropped as a duplicate and the eventual answer recorded only the first bust's
  buy-in. The queued tournament now updates in place, so the answer records the full amount.
- Buy-in comparison in duplicate detection now uses a half-cent tolerance instead of exact float equality,
  which could permanently mis-classify re-entry tournaments on the Google Sheets backend.
- Amounts with thousands separators (e.g. a `$12,500.00` prize pool) were silently truncated at the comma
  (parsed as `$12`); commas inside numbers are now handled.
- A tag with no matching CHANGELOG section now fails the release job instead of publishing a release with a
  placeholder description.

### Changed

- The duplicate check reads one tournament row (new `GetTournamentByID` point lookup) instead of the entire
  tournament history on every file event.
- One shared signed-money formatter across `/stats`, the config flows, and the post-session/post-tournament
  alerts.

## v2.7 — 2026-07-21

### Fixed

- Re-entry tournaments were recorded only at the first bust and then frozen. PokerStars overwrites the
  tournament summary each time you bust an entry, and the duplicate-detection guard skipped every rewrite
  after the first — so the extra re-entry buy-ins were never counted (a three-entry $3.30 tournament recorded
  as −$1.10). Re-entry rewrites now update the tournament row, ledger, and bankroll in place.

### Added

- Re-entry tournaments send one corrected post-session alert per bust, with a line naming how many times you
  re-entered and the total number of entries.

## v2.6 — 2026-07-21

### Fixed

- Satellite/qualifier detection now fires on a ticket-denominated prize pool (e.g. a Power Path
  `$1.00 … Ticket` pool). A satellite you busted out of before any seat was decided still prompts the
  ticket-vs-cash question instead of being silently booked as a cash loss.

### Added

- Parser test coverage across every played format — NLHE, PLO, HORSE, 8-Game, Razz, 2-7 Single Draw, All-In
  Poker cash, and PKO/bounty — confirming the money fields parse correctly. (VPIP/PFR remain
  Hold'em/Omaha-only.)

## v2.5 — 2026-07-20

### Added

- Tournament re-entries, rebuys, and add-ons are parsed as a single summed total buy-in.
- Satellite detection from the summary's header/title line.

### Changed

- Renamed `LICENSE` to `LICENSE.md` so GitHub renders it.

## v2.4 — 2026-07-17

### Added

- `/storage` command to migrate all data between the Google Sheets and local SQLite backends.
- Program icon embedded in `tracker.exe` and the installer.

### Fixed

- Read date cells formatted as dates in Google Sheets, not only plain text.

## v2.3 — 2026-07-16

### Fixed

- Wired up alerts that were not firing, and corrected the date-window boundaries used by the stats commands.

### Changed

- Shrank the release binary by ~44%.

## v2.2 — 2026-07-15

### Added

- Write-ahead fallback for the Sheets backend, so a write survives a transient Google API failure and is
  flushed later.

## v2.1 — 2026-07-15

### Changed

- Accept a comma as the decimal separator in bot amount inputs (e.g. `/deposit 10,50`).
- Publish separate x64 and arm64 installers, with smaller binaries.

## v2.0 — 2026-07-15

### Added

- Local **SQLite** storage backend as an alternative to Google Sheets, chosen at install time — pure-Go, needs
  no Google credentials.
- `/export` command — all data as CSV, byte-identical on both backends.
- Backend-selection page in the installer.

### Changed

- README rewritten around the dual-backend design; adopted the PolyForm Strict license.

### Fixed

- `tracker.db` no longer survives a manual uninstall.

_(The v1.6–v1.9 tags were incremental milestones on the way to v2.0: extracting a shared storage interface,
adding the SQLite backend, backend selection, tournament VPIP/PFR, and SQLite schema migration.)_

## v1.5 — 2026-07-14

### Changed

- Cash-session summary batched into one Telegram message per session instead of one per hand.

### Fixed

- Task Scheduler service now starts reliably after a real PC restart.

### Removed

- Dead, never-written Google Sheets tabs.

## v1.4 — 2026-07-14

### Fixed

- Cash sessions upsert a single row per session instead of appending a new Sheets row on every hand.

## v1.3 — 2026-07-14

### Fixed

- Installer preserves `config.json` across reruns and skips already-answered setup pages on an upgrade.

## v1.2 — 2026-07-14

### Fixed

- Consecutive-losses hard stop no longer fails to fire when a deposit or withdrawal sits next to the losing
  streak.

### Added

- Unit, module, and initialization test coverage across the internal packages.

## v1.1 — 2026-07-13

### Added

- Periodic re-enforcement that keeps PokerStars closed for the full duration of an active hard-stop lock.

### Fixed

- `/sheet` sheet-switch self-restart no longer leaves the service dead; suppressed a console flash on restart.

## v1.0 — 2026-07-13

First public release. Watches PokerStars hand-history and tournament-summary files, records cash and
tournament results to Google Sheets, and enforces configurable loss-limit **hard stops** that close PokerStars
— all controlled through a Telegram bot with post-session alerts, satellite/ticket detection, weekly and
monthly recaps, and a Windows installer with Task Scheduler autostart.

_(The v0.1–v0.9 tags were internal build-phase milestones during initial development — parsers, config,
watcher, bot, hard stop, and the setup wizard — and are not itemized here.)_
