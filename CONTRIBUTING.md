# Contributing

This is a single-purpose personal poker tracker: it watches PokerStars
hand-history and tournament-summary files, records results to Google Sheets or
a local SQLite database, and enforces loss-limit hard stops — all driven from a
Telegram bot. Its scope is deliberately fixed. Changes should fit that scope
and keep the codebase lean; new dependencies, config formats, and network
interfaces are avoided by default.

## Prerequisites

- **Go 1.26+** (see `go.mod`).
- **Linux or WSL** for the release build, which cross-compiles Windows
  binaries. The SQLite driver is pure Go (`modernc.org/sqlite`), so no cgo and
  no C toolchain are required — do not switch to a cgo-based driver.

## Building

Three binaries share the `internal/` packages: `cmd/tracker` (all-local, both roles in one process), `cmd/server` (split brain — storage + bot), `cmd/client` (split collector — watcher + enforcement). The bot token is injected only into the Telegram-talking builds (`tracker` and `server`); the client never carries it.

```bash
# Local build for the host platform (all three binaries).
go build ./...

# Release builds, as CI (release.yml) does them.
# TELEGRAM_BOT_TOKEN is injected at build time — never stored in the repo.

# All-local (combined) — Windows GUI, with the bot token.
GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -H=windowsgui -X main.BotToken=${TELEGRAM_BOT_TOKEN}" \
  -o tracker-amd64.exe ./cmd/tracker

# Split client — Windows console app, NO bot token.
GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w" -o tracker-client-amd64.exe ./cmd/client

# Split server — Windows and Linux, amd64 and arm64, with the bot token.
GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.BotToken=${TELEGRAM_BOT_TOKEN}" \
  -o tracker-server-windows-amd64.exe ./cmd/server
GOOS=linux GOARCH=arm64 go build -trimpath \
  -ldflags "-s -w -X main.BotToken=${TELEGRAM_BOT_TOKEN}" \
  -o tracker-server-linux-arm64 ./cmd/server
```

The release ships **two files**: one Windows installer and one Linux server bundle.

- **Windows — one installer for everything.** CI runs `iscc` **once** (no `/DArch`), producing `PokerTracker-Setup-<version>.exe`. It carries all three roles' binaries for **both** CPU architectures; `ArchitecturesInstallIn64BitMode=x64compatible arm64` plus per-file `IsARM64` Check wrappers (`setup.iss`) install the matching one at runtime. The role page picks all-local / connect-to-a-server / run-a-server-here.
- **Linux server — one self-extracting file.** CI packs both Linux server builds + `packaging/install.sh` into a `makeself` archive `PokerTracker-Server-linux`, shipped inside `PokerTracker-Server-linux.tar.gz` (the tarball preserves the execute bit a raw download would lose). Running it detects the CPU, installs the right build into the user's home, and registers a **per-user** systemd service (`~/.config/systemd/user`, no root) via `tracker-server --install`; enabling start-on-boot without login is the one privileged step (`loginctl enable-linger` through `pkexec`/`sudo`, and optional). The Windows server role registers a `PokerTrackerServer` Task Scheduler task the same way.

Changing what `iscc`/makeself produce (output names, artifact list) requires a matching update to `.github/workflows/release.yml`.

### Disposable Windows installations

Compile the combined and server binaries with the same unique, alphanumeric
test identity, for example `-ldflags "-X main.InstallationID=isolation123"`,
and compile `setup.iss` with `/DInstallationID=isolation123`. Build the collector
normally. The installer uses separate task names, installation-directory
defaults and uninstall registration for that identity; production builds leave
it empty and retain `Poker Tracker` and `PokerTrackerServer`.

Stage all six Windows payloads in a disposable source copy and compile there.
Use a separate test bot and empty spreadsheet for integration tests. Do not
inject the working bot token into a disposable combined/server build. Native
installer and process-isolation checks require an elevated Windows session;
Wine compilation does not verify Task Scheduler or Windows process isolation.
The installer invokes the collector's `--stop-installation <directory>` helper,
which verifies executable paths, signals each matching process's shutdown event,
and waits for owner cleanup; it includes collectors and excludes itself. It does
not force-kill processes. The uninstaller uses the installed role's executable.
An unresponsive process must be investigated before retrying installation.
Before stopping the owner, setup disables existing installation tasks so minute
triggers and failure recovery cannot restart it during configuration or file
replacement. Installer-launched Telegram setup uses `--installer-owns-task`;
Inno registers/starts the task only after replacement. Completion/cancellation
restores tasks that setup suspended; previously disabled tasks stay disabled on
cancellation. Manual uninstall removes config-owned recovery/transfer state and
its temporary writes alongside player data; silent uninstall preserves it.
PokerStars enforcement continues to target `PokerStars.exe` and requires a
separately coordinated test window.

Controlled shutdown must join producers before stopping the worker: stop bot
intake and drain accepted operations, release any prepared restart hold, then
stop/drain collection or close/join the server transport. The worker drains
buffered uploads and saves pending cash before removing its temporary directory.
Keep the bot consuming alerts until the worker exits; join the bot poller and
close SQLite before process exit. Keep the enforcer alive through cleanup and
preserve the original lock expiry. Windows restart helpers wait for the old PID;
Linux user-service restarts wait for SIGTERM handling and process exit.

## Tests

```bash
go test ./...                       # everything
go test ./internal/parsers/...      # one package
```

- **Write tests in the same change as the code.** Any function that takes data
  and returns a number or a decision (rake, BB/100, ROI, running totals, equity
  curve, poker-day boundaries, hard-stop and alert thresholds, duplicate
  detection) gets a table-driven test with hand-checked expected values.
- **No mocks.** Test against real temp files and real structs — parsers against
  real hand histories, the SQLite store against a temp `.db` (`t.TempDir()`).
- Anonymized fixtures in `testdata/hand_history/` and
  `testdata/tournament_summary/` are tracked and required by parser and real
  watcher → collector → server → SQLite tests. Keep usernames and tournament
  identities anonymized and consistent across files. Raw player histories in
  ignored `data/` are optional local smoke-test input.
- Live Telegram/Sheets, actual installer flows, Task Scheduler, reboot behavior
  and native execution need their relevant manual checks. Local HTTP hooks and
  cross-builds provide separate evidence; they do not establish those checks.
- Ordinary Windows tests must not invoke active PokerStars enforcement. Keep
  side-effectful ignored local tests out of blanket test runs.
- `cmd/tracker/deployment_parity_test.go` compares complete snapshots, stable IDs,
  alerts, notification filtering and bankroll through real channels and pinned
  TLS pipelines with separate collector/server configs. Import-only uploads
  verify collector-clock poker-day boundaries. Identical exported rows and the
  shared CSV formatter tests establish formatting parity; Telegram delivery
  remains a separate manual check.

Hard-stop closure checks use real collector/server journals and channels or
pinned TLS. They verify acknowledgements, replay deduplication, failed bot send
results and restart recovery without invoking Windows termination. Keep Bot API
HTTP checks in ignored local tests through the existing endpoint seam. A new
native enforcement check requires a separately coordinated window; ordinary
tests must never terminate the user's PokerStars process. Closure notifications
are at least once; pending queues block installation transfer.

Branch pushes and pull requests run `.github/workflows/ci.yml`: the full Go
suite, vet, and eight pure-Go cross-builds (Windows combined/collector/server and
Linux server, each on amd64 and arm64). These checks use no bot token. Packaging
and publication remain in the tag-triggered release workflow. Compilation does
not establish native ARM64 execution or installer acceptance.

The parser intentionally limits VPIP/PFR to Hold'em/Omaha preflop actions;
mixed-game hand counts include stud/draw hands. Satellite classification requires
structural summary clues, and `/scan` skips/counts tournaments needing an answer.
Preserve these limitations in documentation until their behavior is explicitly
changed and verified.

## Code style

- Run `gofmt` before every commit; `go vet ./...` should be clean.
- Keep it lean: inline single-use helpers, prefer concrete types over
  interfaces (`storage.Store` is the one sanctioned interface), and skip
  defensive code the program flow already makes impossible.
- Errors are returned with context (`fmt.Errorf("...: %w", err)`), never
  `panic` outside startup validation.
- Don't add dependencies without a concrete reason. The current set is
  `fsnotify`, the Telegram bot library, `golang.org/x/oauth2`, and
  `modernc.org/sqlite`; the Google Sheets/Drive calls go through a small
  hand-rolled REST client rather than the official SDK.
- The two storage backends (Sheets and SQLite) must stay behaviorally
  identical — same semantics, same date formats, same `/export` output.
- Keep SQLite mutation reads, result writes and ledger cascades inside the
  same transaction. In Sheets, hold the mutation lock through the complete
  operation and fallback replay; HTTP quota pacing alone does not serialize
  a read followed by a write.
- Serialize entire transport frames per connection, including reconnect lock
  replay. Closing a connection must interrupt blocked writes and upload delivery;
  never hold the listener state lock during a frame write. Keep authentication,
  certificate pinning, TLS 1.3 and the pre-authentication deadline intact.
- Bot-originated writes and migration run on the server file worker. Keep
  Telegram responses on the bot event loop. A migration waits for the collector's
  process check and ordered drain acknowledgement, flushes pending cash, and
  holds the worker through restart or rollback. Disconnect cannot imply success.
- Telegram setup filters initial reinstall contact to the saved chat ID. Fresh
  setup may detect the first text sender. Never overwrite a configured chat with
  another sender, and refresh config after waiting so concurrent settings survive.
- The split collector owns reconnection after an outage. Keep one enforcer alive
  across dialing and transport sessions, including unsaved in-memory protection.
  Use the 30-second idle cadence for actual connect attempts, without a separate
  network probe or custom network timeout. Task Scheduler still recovers unexpected
  process failures. A closed connection must release blocked readers/writers;
  transport cancellation covers dialing and TLS handshake as well as active I/O.
  Ordinary Windows tests must not exercise active enforcement; use the ignored
  native check only in a user-coordinated closed-PokerStars window.
- Save server-owned limits/schedule/notification fields through atomic config
  updates. They do not require a collector or screen name. PC folder/name edits
  use typed authenticated commands and a config-owned `collector_settings.json`
  queue; only collector validation, persistence and watcher activation acknowledge
  success. Keep last confirmed values separate from pending edits. Coalesce by
  field without letting older acknowledgements retire newer revisions; preserve
  unacknowledged revisions across restart. Failed validation waits for a corrected
  edit. Never report disconnection as successful application.
- Windows installer writes must preserve every config field, including raw
  notification/threshold flag objects and pairing settings. Telegram preseeding
  must leave an existing config intact; collector pages start from saved values.
  Extend the ignored native role/upgrade checks when config fields change. Keep
  ordinary interactive pairing dialogs while honoring silent suppression flags.
- Linux installer upgrades stage beside the destination and rename atomically;
  never copy over a running executable. Re-registering the user unit must restart
  that exact installation identity, including when it is already enabled/running.
  Fresh Linux Sheets selection bootstraps SQLite so Telegram credential upload
  and verified migration are available. Setup cannot silently replace an existing
  backend selection. Verify with disposable user units; do not change global linger
  settings as a side effect of tests.
- Both modes use the collector-owned watcher lifecycle: prepare a replacement
  paused, stop/drain accepted old events using the old screen name, save only
  collector fields, then activate and acknowledge. Failed replacement restores
  the previous watcher/settings. Missing startup folders leave collection inactive
  while commands and enforcement stay available; unexpected runtime watcher exit
  still fails the process. Resolve relative Windows paths using the active desktop
  user's profile, never SYSTEM's profile; require a full path when unavailable.
- Pending question append, refresh, reminder and removal use config's atomic
  queue update API so concurrent reminder and worker changes preserve entries.
- Money operation IDs use the `money:` namespace. Preserve them through fallback,
  raw migration and CSV export; replay an existing ID without a second append.
  Preserve legacy blank IDs and never deduplicate movements by amount or date.
- Superseding a queued snapshot must durably preserve the newest candidate before
  its remote write, and retire the queue entry only after success. Keep pending
  entries visible if queue-file replacement fails; use the existing worker/ticker
  and Sheets quota pacing for recovery.
- Cash upserts recognize both historical backend identity formats and preserve
  existing IDs. Keep canonical and legacy key calculations in shared storage
  code; correcting a migrated session must update its original ledger row.
- Cash quiet timers default to 150 seconds and carry a per-arm generation.
  Callbacks only signal the worker and cancel when it exits. Config owns atomic
  `pending_cash.json` replacement; restore per-table activity and resynchronize
  recovered files without postponing unchanged snapshots. Retire recovery state
  only after persistence or durable Sheets fallback acceptance.
- Use `money.Cents` (`int64`) for USD amounts and explicit `Cents` suffixes on
  monetary fields and variables. Parse decimal input directly; fractional cents
  round away from zero. Persist cent integers in JSON and SQLite `_cents` columns;
  convert to decimal dollars only for Sheets cells and display/CSV. Keep ratios
  and percentages separate, and compare fractional-buy-in limits without
  converting monetary values to floats. Check overflow before storage mutations.
- This unreleased version is the compatibility baseline. Compatibility with
  earlier schemas, configuration and protocol formats is not required; maintain
  compatibility from this version forward. Use disposable new-format test stores.
- Keep hard-stop trigger identity separate from delivery and expiry. Persist
  outgoing requests and collector receipts, including expired receipts; replay
  cannot extend or restart the same trigger. Financial changes remain eligible
  for new decisions. Manual protection bypasses storage and upload backpressure.
  Ordinary tests must never terminate a real PokerStars process: actual Windows
  enforcement requires a coordinated closed-game window and owned dummy process.
  Active protection checks every second, idle state every 30 seconds; accepting a
  lock wakes the idle timer. Windows checks process presence before taskkill.
- All serve roles configure `internal/logging` before startup work, using the
  executable directory's rotating log and discard on open failure. Keep required
  bot/worker/collector failures visible as nonzero process exits; a running PID
  must not conceal a terminated required component.
- Configuration restarts pause and drain collection before changing startup
  settings; credentials preserve pending cash deadlines, while a sheet change
  finalizes cash in the source sheet and refuses queued writes. Keep the worker
  held through restart or restored settings and acknowledged resume. Offline
  preparation drains already queued uploads before applying the change.
- Keep Windows task policy equivalent in `installation.WindowsTaskXML` and
  Inno's `BuildTaskXML`: SYSTEM boot, IgnoreNew, no power/runtime interruption,
  failure restarts and an indefinite minute trigger. The latter avoids the
  [255-attempt failure-restart limit](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-tsch/2ff4aa5a-7bc4-449f-bbb1-27475645867f).
  Native checks must prove recovery and one running PID using disposable task
  identities, then verify uninstall cleanup and working-installation preservation.
- Migration copies raw records and verifies independently read destination
  amounts and identities. Preserve partial tournaments and nil-versus-zero
  fields; allow the documented minute precision when Sheets participates.
- Combined-to-split transfer uses the running source owner's Windows event and
  durable request/result exchange. Keep collection paused and the worker held
  after draining/finalizing cash; manual protection remains available. Reserve
  the destination before freezing the source and stop destination polling before
  acknowledging reservation. Resume polling only after durable activation and
  verified source exit. Rollback leaves destination polling in standby.
- Transfer traffic must authenticate and classify before collector replacement.
  Refuse a connected destination collector, nonempty history, conflicting saved
  chats and pending operational queues. Serialize admission with durable worker
  replies, including replies whose requesting socket disconnected. A chunk ACK
  follows data fsync and saved offset; never infer completion from a socket close.
- Keep transfer staging/journals beside configuration under config ownership.
  Import raw snapshots on the worker and independently verify records, IDs,
  ledger amounts and bankroll before changing policy. Preserve destination
  credentials/backend/TLS identity and PC lock bytes/expiry. Rollback may clear
  only verified transfer-owned rows; refuse new history or pending work. Installer
  retries use the original destination and identity, with the old task disabled
  across executable replacement. Keep the original executable and source data.
  Before source export, check protection on the worker after finalizing cash:
  merge saved/manual requests with in-memory intents and require durable collector
  confirmation. Refusal resumes collection without changing the original expiry.
- Transfer checks need real TLS/SQLite worker tests plus disposable native
  Windows owner/installer cutover, interruption and rollback checks. Local Bot
  API hooks stay ignored and isolated. Compiler output alone does not prove task
  registration, Telegram handoff, native ARM64 execution or live Sheets transfer.

## Commits

- Stage files by name (`git add <path>`), never `git add -A`.
- One logical unit per commit: imperative title ≤ 72 chars, a short body, and a
  `Co-Authored-By` trailer when applicable.
- Commit only after the change is verified against real data or real
  interactions — a clean build and passing tests alone are not sufficient for
  runtime/Telegram/installer changes.

## Releases

- Pushing a `vX.Y` tag triggers `.github/workflows/release.yml`, which builds
  the binaries and installers and publishes a **public GitHub Release**.
- Add a matching `## vX.Y — <date>` section to [CHANGELOG.md](CHANGELOG.md)
  **before** tagging — the release job extracts that section as the release
  description, and **fails the release** if no section matches the tag (the
  heading must start exactly `## vX.Y ` — tag, then a space).
- To check the extraction before pushing a tag, run the job's exact awk
  locally and confirm it prints the section:

  ```bash
  TAG=v2.8; awk -v h="## ${TAG} " \
    'index($0,h)==1{g=1;next} /^## /&&g{exit} g{print}' CHANGELOG.md
  ```
- Tag and push only after verifying the build end-to-end (a real session, a
  Telegram round-trip, an installer upgrade). Releases are never automated from
  a commit.
