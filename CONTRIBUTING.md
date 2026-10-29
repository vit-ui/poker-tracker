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
which verifies executable paths before stopping processes and excludes itself.
PokerStars enforcement continues to target `PokerStars.exe` and requires a
separately coordinated test window.

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
- Real player hand histories live in gitignored fixture folders (`data/`,
  `testdata/`); tests over them skip cleanly when the folder is absent, so a
  fresh checkout still passes.
- Telegram interactions, the installer, and Task Scheduler registration are
  verified manually — they are the only things not covered by automated tests.

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
- Pending question append, refresh, reminder and removal use config's atomic
  queue update API so concurrent reminder and worker changes preserve entries.
- Money operation IDs use the `money:` namespace. Preserve them through fallback,
  raw migration and CSV export; replay an existing ID without a second append.
  Preserve legacy blank IDs and never deduplicate movements by amount or date.
- Superseding a queued snapshot must durably preserve the newest candidate before
  its remote write, and retire the queue entry only after success. Keep pending
  entries visible if queue-file replacement fails; use the existing worker/ticker
  and Sheets quota pacing for recovery.
- Migration copies raw records and verifies independently read destination
  amounts and identities. Preserve partial tournaments and nil-versus-zero
  fields; allow the documented minute precision when Sheets participates.

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
