# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Releases are tagged
`vX.Y`, and each GitHub Release pulls its notes from the matching section below.

## v2.9 — 2026-07-22

### Changed
- The per-hand evaluation path fetches its data once per file event instead
  of once per check: the Bankroll Ledger was being downloaded up to six times
  and the Tournaments tab twice for every single hand (hard stop checks and
  their alert twins each fetched their own identical copy), which with
  several tables open could brush the Google Sheets per-minute request quota.
  Both backends now do one ledger read and at most one tournaments read per
  hand; `config.json` is likewise read once per event instead of ~7 times.
  Alert messages, hard stop decisions, and stored rows are unchanged.
- The Sheets ledger-cascade update no longer re-reads a cell it already
  fetched, saving one API call on every hand after a session's first.

### Fixed
- CLAUDE.md claimed the cash post-session summary waits 10 minutes of table
  quiet; the code says 5. Corrected the doc to match the code.

## v2.8 — 2026-07-22

Fixes from a code review of the v2.6/v2.7 changes.

### Fixed
- A satellite answered "used a ticket" could be asked about again on a stray
  file event (its recorded $0 buy-in never matched the file's real buy-in),
  and a second "paid cash" answer would have overwritten the correct record
  with a cash loss. Ticket-resolved tournaments are now correctly recognized
  as already handled.
- Re-entering a satellite while its ticket/cash question was still unanswered
  silently discarded the re-entry: the rewritten summary was dropped as a
  duplicate and the eventual answer recorded only the first bust's buy-in.
  The queued tournament now updates in place, so the answer records the full
  amount.
- Buy-in comparison in duplicate detection now uses a half-cent tolerance
  instead of exact float equality, which could permanently mis-classify
  re-entry tournaments on the Google Sheets backend.
- Amounts with thousands separators (e.g. a `$12,500.00` prize pool) were
  silently truncated at the comma (parsed as `$12`); commas inside numbers
  are now handled.
- A tag with no matching CHANGELOG section now fails the release job instead
  of publishing a release with a placeholder description.

### Changed
- The duplicate check reads one tournament row (new `GetTournamentByID` point
  lookup) instead of the entire tournament history on every file event.
- One shared signed-money formatter across `/stats`, the config flows, and
  the post-session/post-tournament alerts.

## v2.7 — 2026-07-21

### Fixed
- Re-entry tournaments were recorded only at the first bust and then frozen.
  PokerStars overwrites the tournament summary each time you bust an entry, and
  the duplicate-detection guard skipped every rewrite after the first — so the
  extra re-entry buy-ins were never counted (a three-entry $3.30 tournament
  recorded as −$1.10). Re-entry rewrites now update the tournament row, ledger,
  and bankroll in place.

### Added
- Re-entry tournaments send one corrected post-session alert per bust, with a
  line naming how many times you re-entered and the total number of entries.

## v2.6 — 2026-07-21

### Fixed
- Satellite/qualifier detection now fires on a ticket-denominated prize pool
  (e.g. a Power Path `$1.00 … Ticket` pool). A satellite you busted out of
  before any seat was decided still prompts the ticket-vs-cash question instead
  of being silently booked as a cash loss.

### Added
- Parser test coverage across every played format — NLHE, PLO, HORSE, 8-Game,
  Razz, 2-7 Single Draw, All-In Poker cash, and PKO/bounty — confirming the
  money fields parse correctly. (VPIP/PFR remain Hold'em/Omaha-only.)

## v2.5 — 2026-07-20

### Added
- Tournament re-entries, rebuys, and add-ons are parsed as a single summed
  total buy-in.
- Satellite detection from the summary's header/title line.

### Changed
- Renamed `LICENSE` to `LICENSE.md` so GitHub renders it.

## v2.4 — 2026-07-17

### Added
- `/storage` command to migrate all data between the Google Sheets and local
  SQLite backends.
- Program icon embedded in `tracker.exe` and the installer.

### Fixed
- Read date cells formatted as dates in Google Sheets, not only plain text.

## v2.3 — 2026-07-16

### Fixed
- Wired up alerts that were not firing, and corrected the date-window
  boundaries used by the stats commands.

### Changed
- Shrank the release binary by ~44%.

## v2.2 — 2026-07-15

### Added
- Write-ahead fallback for the Sheets backend, so a write survives a transient
  Google API failure and is flushed later.

## v2.1 — 2026-07-15

### Changed
- Accept a comma as the decimal separator in bot amount inputs (e.g.
  `/deposit 10,50`).
- Publish separate x64 and arm64 installers, with smaller binaries.

## v2.0 — 2026-07-15

### Added
- Local **SQLite** storage backend as an alternative to Google Sheets, chosen
  at install time — pure-Go, needs no Google credentials.
- `/export` command — all data as CSV, byte-identical on both backends.
- Backend-selection page in the installer.

### Changed
- README rewritten around the dual-backend design; adopted the PolyForm Strict
  license.

### Fixed
- `tracker.db` no longer survives a manual uninstall.

_(The v1.6–v1.9 tags were incremental milestones on the way to v2.0: extracting
a shared storage interface, adding the SQLite backend, backend selection,
tournament VPIP/PFR, and SQLite schema migration.)_

## v1.5 — 2026-07-14

### Changed
- Cash-session summary batched into one Telegram message per session instead of
  one per hand.

### Fixed
- Task Scheduler service now starts reliably after a real PC restart.

### Removed
- Dead, never-written Google Sheets tabs.

## v1.4 — 2026-07-14

### Fixed
- Cash sessions upsert a single row per session instead of appending a new
  Sheets row on every hand.

## v1.3 — 2026-07-14

### Fixed
- Installer preserves `config.json` across reruns and skips already-answered
  setup pages on an upgrade.

## v1.2 — 2026-07-14

### Fixed
- Consecutive-losses hard stop no longer fails to fire when a deposit or
  withdrawal sits next to the losing streak.

### Added
- Unit, module, and initialization test coverage across the internal packages.

## v1.1 — 2026-07-13

### Added
- Periodic re-enforcement that keeps PokerStars closed for the full duration of
  an active hard-stop lock.

### Fixed
- `/sheet` sheet-switch self-restart no longer leaves the service dead;
  suppressed a console flash on restart.

## v1.0 — 2026-07-13

First public release. Watches PokerStars hand-history and tournament-summary
files, records cash and tournament results to Google Sheets, and enforces
configurable loss-limit **hard stops** that close PokerStars — all controlled
through a Telegram bot with post-session alerts, satellite/ticket detection,
weekly and monthly recaps, and a Windows installer with Task Scheduler
autostart.

_(The v0.1–v0.9 tags were internal build-phase milestones during initial
development — parsers, config, watcher, bot, hard stop, and the setup wizard —
and are not itemized here.)_
