package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"unicode/utf16"

	"github.com/vit-ui/self-tracker/internal/backend"
	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/client"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/server"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

// BotToken is injected at build time via -ldflags "-X main.BotToken=...".
var BotToken string

// serviceAccountPath, configPath, and lockfilePath are resolved relative to
// the directory tracker.exe itself lives in — never the process's current
// working directory. Task Scheduler defaults an unspecified working
// directory to %SystemRoot%\System32, and relying on cwd here would
// silently resolve these to files in the wrong place whenever the service
// is launched by the scheduled task rather than a shell already sitting in
// C:\poker-tracker (this actually happened: schtasks /Run in runSetup
// launched --serve with cwd=System32, which read/wrote a fresh, empty
// config.json there instead of the real one, making it look like the
// sheet and credentials were never configured). The installer always
// installs to a fixed, non-user-selectable directory (setup.iss:
// DefaultDirName=C:\poker-tracker), so wherever tracker.exe itself is is
// always the right anchor, regardless of who launched it or how.
var (
	serviceAccountPath string
	configPath         string
	lockfilePath       string
	dbPath             string
	fallbackPath       string
	logPath            string
)

func init() {
	exePath, err := os.Executable()
	if err != nil {
		panic("getting executable path: " + err.Error())
	}
	dir := filepath.Dir(exePath)
	serviceAccountPath = filepath.Join(dir, "credentials", "google_service_account.json")
	configPath = filepath.Join(dir, "config.json")
	lockfilePath = filepath.Join(dir, "lockfile.json")
	dbPath = filepath.Join(dir, "tracker.db")
	fallbackPath = filepath.Join(dir, "sheets_fallback.json")
	logPath = filepath.Join(dir, "tracker.log")
}

func main() {
	serve := flag.Bool("serve", false, "run the background service")
	checkLock := flag.Bool("check-lock", false, "check lockfile state and exit")
	setup := flag.Bool("setup", false, "run setup wizard via Telegram bot")
	flag.Parse()

	switch {
	case *checkLock:
		runCheckLock()
	case *setup:
		runSetup()
	case *serve:
		runServe()
	default:
		fmt.Fprintln(os.Stderr, "usage: tracker [--serve | --check-lock | --setup]")
		os.Exit(1)
	}
}

func runCheckLock() {
	lock, err := config.LoadLock(lockfilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading lockfile: %v\n", err)
		os.Exit(1)
	}
	if lock == nil {
		os.Exit(0)
	}
	if config.IsLockExpired(lock) {
		if err := config.DeleteLock(lockfilePath); err != nil {
			fmt.Fprintf(os.Stderr, "deleting lockfile: %v\n", err)
		}
		os.Exit(0)
	}
	fmt.Printf("PokerStars is locked until %s. Check Telegram for details.\n",
		lock.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	os.Exit(1)
}

func runSetup() {
	// Kills any previously-running tracker.exe --serve before starting the
	// wizard — found the hard way: MultipleInstancesPolicy=IgnoreNew (both
	// task XML templates) means schtasks /Run below silently no-ops if an
	// old instance is still alive, so a stale process from an earlier
	// setup keeps running with old config forever, never picking up
	// what this run is about to write. The stale process also risks
	// polling the same bot token concurrently with bot.RunSetup below.
	// /FI excludes this process's own PID — taskkill /IM tracker.exe
	// would otherwise kill itself, since this process is also named
	// tracker.exe.
	_, _ = exec.Command("taskkill.exe", "/F", "/IM", "tracker.exe",
		"/FI", fmt.Sprintf("PID ne %d", os.Getpid())).CombinedOutput()

	bot.RunSetup(BotToken, configPath, serviceAccountPath)

	exePath, err := os.Executable()
	if err != nil {
		panic("getting executable path: " + err.Error())
	}

	// Plain schtasks flags (/SC ONLOGON /RL HIGHEST) can't turn off "Start
	// the task only if the computer is on AC power" or the default
	// execution time limit — there's no flag for either. /Create /XML is
	// the only way to control every task property, so the task definition
	// is built inline and written to a temp file for schtasks to read.
	// schtasks /Create /XML rejects a UTF-8 file — it requires UTF-16 with
	// a byte-order mark, hence utf16File instead of a plain WriteString.
	xmlFile, err := os.CreateTemp("", "poker-tracker-task-*.xml")
	if err != nil {
		panic("creating task scheduler xml: " + err.Error())
	}
	defer os.Remove(xmlFile.Name())
	if err := xmlFile.Close(); err != nil {
		panic("closing task scheduler xml: " + err.Error())
	}
	if err := os.WriteFile(xmlFile.Name(), utf16File(fmt.Sprintf(taskSchedulerXML, exePath, filepath.Dir(exePath))), 0644); err != nil {
		panic("writing task scheduler xml: " + err.Error())
	}

	if out, err := exec.Command("schtasks.exe",
		"/Create",
		"/TN", "Poker Tracker",
		"/XML", xmlFile.Name(),
		"/F",
	).CombinedOutput(); err != nil {
		panic(fmt.Sprintf("registering Task Scheduler entry: %v: %s", err, out))
	}

	// Starts the service now instead of leaving the player to reboot or
	// start it manually. Not fatal if it fails — the task is already
	// registered and will still run on the next logon — so this only logs.
	if out, err := exec.Command("schtasks.exe", "/Run", "/TN", "Poker Tracker").CombinedOutput(); err != nil {
		slog.Error("starting task scheduler entry", "err", err, "output", string(out))
	}
	os.Exit(0)
}

// utf16File encodes s as UTF-16LE with a leading byte-order mark — the
// encoding schtasks /Create /XML requires; it rejects a plain UTF-8 file.
func utf16File(s string) []byte {
	points := utf16.Encode([]rune(s))
	buf := make([]byte, 2+2*len(points))
	buf[0], buf[1] = 0xFF, 0xFE
	for i, p := range points {
		buf[2+2*i] = byte(p)
		buf[2+2*i+1] = byte(p >> 8)
	}
	return buf
}

// taskSchedulerXML is the Task Scheduler task definition registered during
// --setup, with the tracker.exe path substituted twice: once for Command,
// once for WorkingDirectory (both %s, same value — see the exePath/dir
// split in runSetup).
//
// BootTrigger + a SYSTEM Principal, not LogonTrigger + a group Principal:
// an earlier version used <LogonTrigger> with the Administrators-group SID
// (S-1-5-32-544) as Principal, matching CLAUDE.md's original description.
// That combination showed as "Ready" in Task Scheduler but did not
// reliably start tracker.exe on a real PC restart — LogonTrigger fires off
// an interactive logon, and evaluating "is this logon a member of group X"
// against a UAC-split token is a documented source of trigger misfires,
// on top of depending on an interactive session existing at all.
// BootTrigger fires at system startup itself, independent of any user
// logon, and Principal UserId S-1-5-18 (SYSTEM) needs no logon token to
// evaluate — Task Scheduler always has a SYSTEM context to run as. This
// matches the original hand-verified manual setup (plan §Windows Task
// Scheduler Setup: "At startup", not "At log on"). SYSTEM has full local
// filesystem access to the player's PokerStars folders and enough
// privilege to taskkill.exe PokerStars.exe regardless of which user
// account is running it. The 30-second Delay mirrors that same manual
// setup, giving the filesystem/network/antivirus time to settle right
// after boot. RunLevel HighestAvailable is kept even though it's moot for
// SYSTEM (already maximally privileged) — harmless and keeps the two
// Principal fields together as one block. WorkingDirectory is
// belt-and-suspenders now that the three deployment paths are resolved
// from os.Executable() rather than cwd (see the var block above) — but it
// matches the documented manual Task Scheduler setup ("Start in
// C:\poker-tracker") and costs nothing to set correctly.
const taskSchedulerXML = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers>
    <BootTrigger>
      <Enabled>true</Enabled>
      <Delay>PT30S</Delay>
    </BootTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-18</UserId>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>true</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>--serve</Arguments>
      <WorkingDirectory>%s</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`

func runServe() {
	// Keep this idle background service's footprint bounded. Despite the
	// package name, runtime/debug is where Go's production runtime knobs
	// live: these two calls are the in-code equivalents of the GOMEMLIMIT
	// and GOGC environment variables (the officially documented GC tuning
	// mechanism) — set here because Task Scheduler can't set per-process
	// env vars. The soft memory limit (Go 1.19+) continuously paces both
	// the GC and the scavenger that returns freed pages to the OS
	// (MEM_DECOMMIT on Windows, so Task Manager reflects it). The limit
	// caps read spikes (/export and /stats pull whole tabs); the lower GC
	// percent tightens the idle floor, since below the limit the collector
	// would otherwise still let the heap float to 2× live data. Soft means
	// no OOM risk — a too-low limit only costs extra GC cycles.
	debug.SetMemoryLimit(32 << 20)
	debug.SetGCPercent(50)

	// The default slog handler writes to os.Stderr. hideConsole's
	// FreeConsole call below leaves that handle NULL/invalid — confirmed
	// via live testing (isolated by reverting to a build without this fix
	// and reproducing the exact same failure) to hang indefinitely on the
	// first write to it afterward, wedging startup before the
	// watcher/worker/bot goroutines are ever launched (the "--serve
	// starts but never responds" bug). runSetup never hits this because
	// it never calls hideConsole — its console handle, whatever it is,
	// is never invalidated out from under it. So the log must never touch
	// stderr: route it to a real file (tracker.log, anchored to the exe
	// dir, size-capped with one .old rollover), which is a valid handle
	// hideConsole never invalidates. This is the only log destination —
	// errors and Info-level transition records both land here; nothing is
	// auto-forwarded to Telegram (the worker sends the few user-actionable
	// notices explicitly). If the file can't be opened, fall back to
	// io.Discard — never stderr, which reintroduces the hang.
	if w, err := newRotatingWriter(logPath, logMaxBytes); err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	} else {
		slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})))
	}

	// Belt-and-suspenders on top of -H=windowsgui — see hideConsole's doc
	// comment for why the build flag alone wasn't enough in practice.
	hideConsole()

	// Construct the storage backend (shared with the split server via
	// internal/backend). On the sheets path this blocks in EnsureSheetID until a
	// sheet is configured; the watcher/server/bot goroutines can't run without a
	// store, so it resolves first.
	sc, backendName, err := backend.Open(BotToken, configPath, serviceAccountPath, dbPath, fallbackPath)
	if err != nil {
		panic(err.Error())
	}

	// Reload — EnsureSheetID inside Open may have written telegram_chat_id
	// and/or google_sheet_id back to config.json.
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		panic("loading config: " + err.Error())
	}

	slog.Info("tracker service starting", "backend", backendName)

	sessionChan := make(chan watcher.FileEvent, 10)
	alertChan := make(chan string, 20)

	var watcherDone chan struct{}
	if cfg.Paths.HandHistory != nil && cfg.Paths.TournamentSummary != nil {
		hhPath, tsPath := resolvePaths(cfg)
		slog.Info("watcher starting", "hand_history", hhPath, "tournament_summary", tsPath)
		watcherDone = make(chan struct{})
		go func() {
			if err := watcher.Watch(watcherDone, hhPath, tsPath, sessionChan); err != nil {
				slog.Error("watcher stopped", "err", err)
				alertChan <- "⚠️ File watching stopped unexpectedly — I'm no longer seeing new poker sessions. Restart the tracker (or your PC) to recover."
			}
		}()
	}

	restartWatcher := func() {
		newCfg, err := config.LoadConfig(configPath)
		if err != nil {
			slog.Error("loading config for watcher restart", "err", err)
			return
		}
		if newCfg.Paths.HandHistory == nil || newCfg.Paths.TournamentSummary == nil {
			return
		}
		hhPath, tsPath := resolvePaths(newCfg)
		slog.Info("watcher restarting", "hand_history", hhPath, "tournament_summary", tsPath)
		if watcherDone != nil {
			close(watcherDone)
		}
		watcherDone = make(chan struct{})
		go func() {
			if err := watcher.Watch(watcherDone, hhPath, tsPath, sessionChan); err != nil {
				slog.Error("watcher stopped", "err", err)
				alertChan <- "⚠️ File watching stopped unexpectedly — I'm no longer seeing new poker sessions. Restart the tracker (or your PC) to recover."
			}
		}()
	}

	// The collector (watcher-side) and server (brain-side) run in one process
	// here, connected by an in-process link: file changes go up as uploads, the
	// hard-stop lock comes back down as a command. The same two roles are the
	// two separate binaries of the split build; combined mode is just both over
	// this in-process link.
	link := transport.NewLink()
	srv, err := server.NewServer(sc, configPath, link, alertChan)
	if err != nil {
		panic("creating server: " + err.Error())
	}
	go srv.Run()

	collector := client.NewCollector(link, lockfilePath)
	go collector.Run(sessionChan, configPath, nil) // nil dead: the in-process link never drops

	// Runs for the life of this process — the done channel is never closed,
	// since RunEnforcementLoop is only meant to stop when --serve itself
	// exits.
	go client.RunEnforcementLoop(lockfilePath, make(chan struct{}))

	// All-local: a manual /hardstop trigger enforces inline, exactly as before
	// the split — same client.Enforce (taskkill + write the local lockfile), no
	// change in behavior. The split server instead sends the lock down the link.
	sendLock := func(lock config.LockFile) { client.Enforce(lockfilePath, lock) }

	go func() {
		err := bot.Run(BotToken, alertChan, sc, configPath, lockfilePath, serviceAccountPath, dbPath, restartWatcher, sendLock)
		// bot.Run's error return was previously discarded entirely by a
		// bare "go bot.Run(...)" — if NewBotAPI failed inside it (bad
		// token, network issue at that instant), the goroutine would exit
		// silently and nothing would ever be visibly wrong: the process
		// stays alive (select{} below blocks forever), Task Scheduler
		// shows it as running, and the bot simply never listens for
		// anything. This log is the only way that failure was ever going
		// to be visible.
		slog.Error("bot.Run goroutine returned — the bot is no longer listening for anything", "err", err)
	}()

	select {}
}

// resolvePaths joins the base hand history and tournament summary paths with
// the player's screen name, which PokerStars uses as a subfolder for all files.
func resolvePaths(cfg *config.Config) (string, string) {
	return filepath.Join(*cfg.Paths.HandHistory, *cfg.Setup.PokerStarsScreenName),
		filepath.Join(*cfg.Paths.TournamentSummary, *cfg.Setup.PokerStarsScreenName)
}
