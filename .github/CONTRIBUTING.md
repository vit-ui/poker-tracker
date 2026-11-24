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

Three binaries share the `internal/` packages: `cmd/tracker` (all-local, both roles in one process), `cmd/server` (split brain — storage + bot), `cmd/client` (split collector — watcher + enforcement). Setup provisions the locally protected Telegram credential for `tracker` and `server`. The collector needs no Telegram credential.

```bash
go build ./...

GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -H=windowsgui" \
  -o tracker-amd64.exe ./cmd/tracker

GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w" -o tracker-client-amd64.exe ./cmd/client

GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w" \
  -o tracker-server-windows-amd64.exe ./cmd/server
GOOS=linux GOARCH=arm64 go build -trimpath \
  -ldflags "-s -w" \
  -o tracker-server-linux-arm64 ./cmd/server
```

The release ships **two files**: one Windows installer and one Linux server bundle.

- **Windows — one installer for everything.** CI runs `iscc` **once** (no `/DArch`), producing `PokerTracker-Setup-<version>.exe`. It carries all three roles' binaries for **both** CPU architectures; `ArchitecturesInstallIn64BitMode=x64compatible arm64` plus per-file `IsARM64` Check wrappers (`packaging/windows/setup.iss`) install the matching one at runtime. The role page picks all-local / connect-to-a-server / run-a-server-here.
- **Linux server — one self-extracting file.** CI packs both Linux server builds + `packaging/linux/install.sh` into a `makeself` archive `PokerTracker-Server-linux`, shipped inside `PokerTracker-Server-linux.tar.gz` (the tarball preserves the execute bit a raw download would lose). Running it detects the CPU, installs the right build into the user's home, and registers a **per-user** systemd service (`~/.config/systemd/user`, no root) via `tracker-server --install`; enabling start-on-boot without login is the one privileged step (`loginctl enable-linger` through `pkexec`/`sudo`, and optional). The Windows server role registers a `PokerTrackerServer` Task Scheduler task the same way.

Changing what `iscc`/makeself produce (output names, artifact list) requires a matching update to `.github/workflows/package.yml`.

### Disposable Windows installations

Compile the combined and server binaries with the same unique, alphanumeric
test identity, for example `-ldflags "-X main.InstallationID=isolation123"`,
and compile `packaging/windows/setup.iss` with `/DInstallationID=isolation123`. Build the collector
normally. The installer uses separate task names, installation-directory
defaults and uninstall registration for that identity; production builds leave
it empty and retain `Poker Tracker` and `PokerTrackerServer`.

Stage all six Windows payloads beside the moved installer script in a disposable source copy and compile there.
Use a separate test bot and empty spreadsheet for integration tests. Do not
Provision a disposable credential for local setup checks. Native
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
go test ./...
go test ./internal/parsers/...
```

- For isolated timer regressions, use `testing/synctest` with real temporary
  stores to assert before/at deadlines independently of CPU scheduling. Keep
  the real watcher-to-SQLite end-to-end suite for filesystem/network behavior.
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

Pull requests run `.github/workflows/ci.yml`: the full Go suite, vet, and eight
pure-Go cross-builds (Windows combined/collector/server and Linux server, each on
amd64 and arm64), without a bot token. Branch pushes do not trigger a workflow;
run tests and vet locally before pushing work without a pull request.
Pull requests and the release workflow share the same Go checks and eight
cross-build targets. The packaging workflow builds the installers from the
approved release tag before publication.
Compilation does not establish native ARM64 execution or installer acceptance.

Update request state lives in `requested_update.json` on each installation.
Only its service agent accepts a request; after detached launch, the helper owns
progress/result writes until completion. A separate installation lock excludes
concurrent request helpers. Keep release pins, checksum validation, startup
confirmation and existing rollback. Windows helpers need an elevated installed
task and must survive its Task Scheduler job; Linux managed servers use an
independent transient user unit. Native service behavior needs isolated checks;
Go tests and cross-builds alone do not verify it.

User update instructions belong near the beginning of the README and in the
Telegram `/help update` topic and `/update` menu (also linked from Menu → Info). Keep installed
filenames aligned with `packaging/windows/setup.iss`: combined and collector use `tracker.exe`,
while Windows server uses `tracker-server.exe`. `/update` accepts local or durable
collector requests through the worker and authenticates the configured chat.
`/help update` focuses on Telegram requests and links to `/help update terminal`
for terminal alternatives and older-install guidance. Run release checks
asynchronously at startup, daily, and on PokerStars process transitions reported
by the connected Windows collector. Only the bot goroutine sends and deduplicates
release notices after reading actual local and collector observations. Never
mark a queued send as completed; offline requests retain their identity until
confirmed. New operational failures require a fresh request. Pending updates
block storage/deployment handover; do not run setup/uninstall concurrently.

Before queuing a bot-host or combined request, check that the active private
Telegram pairing is saved on that host. `--telegram-status` checks saved
pairing without contacting Telegram. `--connect-telegram-if-needed` reuses
it or opens local pairing in an interactive session. Keep Tracker startup
confirmation independent of Telegram and keep extraction-only updater paths
free of setup prompts. Exercise interactive and silent installer paths
separately, including cancellation and an upgrade with a saved pairing.

The parser intentionally limits VPIP/PFR to Hold'em/Omaha preflop actions;
mixed-game hand counts include stud/draw hands. Satellite classification requires
structural summary clues. Scans count official summaries that need an entry-cost
answer and link to confirmations for missing results recovered from hand history.

## Hand-history scans

Keep export grouping on the server. The collector streams a requested file
without parsing it. Give official summaries priority over recovery candidates
from the same scan. Evaluate older histories immediately and include
**Review missing results** in the final report when the player needs to confirm
a payout or total cash entry cost. Test with anonymized fixtures and disposable
storage for larger private exports.

## Storage reset

Exercise reset with disposable SQLite data or the Sheets HTTP test seam.
Verify the recovery snapshot before clearing the active backend. Preserve
headers and use one atomic write for the three history datasets.

## Code style

- Reuse existing functions and extract shared functions when call sites implement the same behavior. Use descriptive names and control flow. Omit explanatory comments throughout source, tests, scripts, configuration and code examples. Preserve directives required by compilers, interpreters and tooling.

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
  operation and fallback replay. Use one grid batch for each result and its
  ledger changes, including bounty corrections. Match ledger rows by type and ID.
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
- Existing Windows installations default to an upgrade of the detected role,
  with no configuration rewrite or first-time setup. Configuration/role changes
  require the explicit wizard choice. The bundled collector's read-only
  `--validate-config <path>` check rejects unreadable configuration before setup
  proceeds; it must never create a missing file. Unknown or conflicting executable
  roles stop setup instead of defaulting to all-local.
  Validate the actual interactive wizard without `TESTROLE` or other answer
  overrides for collector, combined and server installations, including custom
  folders, cancellation and incomplete configuration. Silent tests with supplied
  answers verify those answers, not detection or default page navigation.
  Shutdown failures must retain the helper's stderr (stdout when stderr is empty)
  or the Windows launch error, and identify the saved temporary error log. Verify
  both launch and process-exit failures with disposable native installer checks.
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
- Cash and tournament quiet timers default to 150 seconds. Callbacks signal
  the worker and cancel when it exits. The worker rejects stale generations.
  Restore per-file activity from `pending_cash.json` and resynchronize recovered
  cash files without postponing unchanged snapshots. Retire cash recovery state
  after persistence or durable Sheets fallback acceptance.
- Tournament deadlines and confirmations stay keyed by tournament ID in
  `pending_tournaments.json`. Resumed play invalidates a pending finish.
  Preserve result provenance in raw transfers and calculate finish percentages
  from results with known field sizes.
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

- Install Commitizen, Gitleaks, and Lefthook, then run `lefthook install`.
- Stage files by name and make one verified logical change per commit. Use a
  Conventional Commit title up to 72 characters and the applicable
  `Co-Authored-By` trailer. Use `fix` for a patch, `feat` for a minor change,
  and `!` or `BREAKING CHANGE` for incompatible changes.
- Verify runtime, Telegram, and installer changes directly where applicable.
  Document any checks that need a disposable native installation.

## Releases

Release Please opens or updates a release PR after checks pass on `master`.
Approve the PR's Actions run, verify the proposed version and notes, and merge
the PR when the release is ready. GitHub Actions builds from the resulting
`vMAJOR.MINOR.PATCH` tag. A draft becomes public only after both installer
digests match. If packaging or upload fails, rerun the failed jobs on that
workflow run so the draft and tag remain pinned. Manual tag pushes do not build
or publish anything.

The repository owner must enable **Allow GitHub Actions to create and approve
pull requests** under Actions settings. The workflow uses its built-in token.

### Rebuild the historical releases

1. Dispatch **Build historical installers** from `master` for `1.0.0`,
   `2.0.0`, and `3.0.0`. Inspect each run's named installer artifacts. The
   first version has a Windows installer. The other two also have a Linux
   bundle. The installers contain no embedded Telegram credential.
   The v2.0.0 build copy applies the Inno syntax correction from old v4.1.
   Its tag still points to the original source commit.
2. Dispatch the same workflow with **Publish the verified historical installers**
   enabled for each version. It uses the curated changelog notes, verifies the
   uploaded digests, and publishes against the original commit without setting
   it as latest. Using the workflow token to create historical tags keeps the
   old tag-triggered workflows from running.
3. The bootstrap manifest starts at restored `v3.0.0`. Include a
   `Release-As: 4.0.0` footer on the transition commit. Dispatch **Release**
   on `master` once the reconstructed baseline is published. Review the
   release PR's body and changelog together, using the curated notes for the
   whole final era, before merging. The follow-up with the audit fixes uses
   its own release PR. The historical release creation step may require
   maintainer authentication when an old commit carries different workflows.
   Verify the existing tags and attach the tested artifacts to draft releases.
4. Confirm the rebuilt release is latest and its two assets have matching
   digests. Inventory the legacy release and tag names and SHAs before removing
   the legacy releases, assets, and corresponding tags. The original branches
   and commits preserve the historical source. Keep the old Telegram bot active
   for the migration.

The reconstructed releases through `v4.0.0` contain no working Telegram token.
An existing `v5.0` installation stays in place until it can use `/update` to
move to the follow-up `v5.0.1` release. These archived installers should be
identified as such in their release notes.

Hard-stop display state (`collector_observation.json`) is a config-owned server
copy. Never use it as live protection confirmation for migration or deployment.
Manual requests go through the worker's durable delivery queue; reconnects retain
the original trigger identity and absolute expiry.

Collector calendar context persists in config-owned `collector_clock.json`.
Restore it before bot startup; never use the saved offset to shift absolute lock
expiry or reminder deadlines.

Release builds inject `-X main.Version=${GITHUB_REF_NAME}` into all roles.
Build the eight role/architecture binaries as internal CI artifacts, but publish
ONLY `PokerTracker-Setup-<version>.exe` and `PokerTracker-Server-linux.tar.gz`.
GitHub adds two source archives automatically, giving four visible entries.
Do not publish individual binaries, checksum manifests or extra updater assets.
The updater verifies the installer's size and GitHub metadata `digest` before
extracting a payload. Windows uses the installer's extraction-only
`/UPDATEFILE` + `/UPDATEOUT` entry: InitializeSetup aborts with Inno exit code 1
before wizard/config/task code; a fresh output file and role/version probes
are required. Linux reads the makeself member from the outer tar.gz and uses
`--tar xOf` to stream only `./tracker-server-<arch>`, never `install.sh`.
Use the existing two packages and keep installer output names unchanged. Never include the bot token in a
collector build. Exercise updater replacement and recovery in disposable roots
and service identities, with PokerStars closed, before claiming native behavior
verified. The updater downloads and validates before stopping the owner and
never runs the setup/install flow. Development builds use `Version=dev` and
refuse updates. Versions 4.8/4.9 require a one-time manual installer upgrade;
there is no legacy raw-asset fallback.

Terminal updates use config-owned `update_startup.json` receipts and fsnotify
startup acknowledgement. Combined/server owners wait for required components;
the offline collector confirms local enforcement initialization independently of
its network connection. Do not replace this with a fixed startup delay or service
active-state check: both can accept a process that exits during initialization.

Lock status displays use current-connection observations while available and the
saved display copy offline. A display-cache failure must not hide known live
protection; this does not change enforcement acknowledgement requirements.

Recovery snapshots must not replace an existing saved collector clock. When no
saved observation exists, choose the freshest historical timestamp across cash
and closure records before establishing calendar context.

Elevated update launchers must wait for the helper PID only. PowerShell
`Start-Process -Wait` also waits for descendants, including a restarted
foreground owner; cover that distinction with isolated native processes.

Windows update transactions persist `update_recovery.json` and a flushed recovery
helper before registering the temporary recovery task; only then may normal
recovery be suspended. The temporary task uses the existing boot/minute policy
and skips an installation while its update/transfer mutex is held. Interrupted
unconfirmed images roll back; durable startup confirmation keeps the new image.
Keep recovery armed on restoration failures, validate installation/file identity
before recovery, and remove the temporary definition before deleting its journal.
Native interruption tests must use disposable tasks/roots and kill only their own
helper processes. Compiler and file-state tests do not establish actual reboot
behavior or ARM64 execution.

Update-status checks must not prevent the bot's idle maintenance from being queued.
Collector update acknowledgements and asynchronous reports may arrive out of order;
a pending report must never replace a terminal result for the same request.

### Money movement changes

Deposits, withdrawals and rewards share the bot money-entry handler and `AppendLedgerEntry` path, including operation identities and Sheets recovery. Keep movement types excluded from results-only math. Transaction reports reuse the worker, ledger reads, statistics date boundaries and USD formatter; history pages must make every matching movement reachable.

Manual `/hardstop now [duration]` requests reuse the duration parser and durable lock-delivery path. Overrides must not update automatic settings, shorten an active lock, wait for storage, or lose request identity during offline replay.

Windows updater metadata changes must target the existing 64-bit installer uninstall key for the same installation ID and directory. Reuse the synchronization function after confirmed replacement and confirmed recovery; never run it on rollback. Test only disposable registry entries, preserving unrelated values. Metadata failures retain confirmed executables and recovery state.

Windows shutdown verification must work with debug privilege initially disabled in an elevated token and a real SYSTEM-owned fixture. Check exact-path isolation, caller/updater exclusions, graceful cleanup, and restoration of the previous privilege state on success and error. Never test against the working installation or kill processes by name.
