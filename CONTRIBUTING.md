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

```bash
# Local build for the host platform
go build ./...

# Release build (Windows, both architectures), as CI does it.
# TELEGRAM_BOT_TOKEN is injected at build time — never stored in the repo.
GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -H=windowsgui -X main.BotToken=${TELEGRAM_BOT_TOKEN}" \
  -o tracker-amd64.exe ./cmd/tracker
GOOS=windows GOARCH=arm64 go build -trimpath \
  -ldflags "-s -w -H=windowsgui -X main.BotToken=${TELEGRAM_BOT_TOKEN}" \
  -o tracker-arm64.exe ./cmd/tracker
```

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
  description, so a missing section means an empty release body.
- Tag and push only after verifying the build end-to-end (a real session, a
  Telegram round-trip, an installer upgrade). Releases are never automated from
  a commit.
