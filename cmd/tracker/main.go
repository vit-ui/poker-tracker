package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/vit-ui/self-tracker/internal/backend"
	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/client"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/installation"
	"github.com/vit-ui/self-tracker/internal/logging"
	"github.com/vit-ui/self-tracker/internal/server"
	"github.com/vit-ui/self-tracker/internal/transport"
)

// BotToken is injected at build time via -ldflags "-X main.BotToken=...".
var BotToken string

// InstallationID is injected into disposable builds; production leaves it empty.
var InstallationID string

// serviceAccountPath, configPath, and lockfilePath are resolved relative to
// the directory tracker.exe itself lives in — never the process's current
// working directory. Task Scheduler defaults an unspecified working
// directory to %SystemRoot%\System32, and relying on cwd here would
// silently resolve these to files in the wrong place whenever the service
// is launched by the scheduled task rather than a shell already sitting in
// C:\poker-tracker (this actually happened: schtasks /Run in runSetup
// launched --serve with cwd=System32, which read/wrote a fresh, empty
// config.json there instead of the real one, making it look like the
// sheet and credentials were never configured). The executable directory
// also keeps disposable installations separate from the working installation.
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
	stopInstallation := flag.String("stop-installation", "", "stop tracker processes in the specified installation directory")
	serve := flag.Bool("serve", false, "run the background service")
	checkLock := flag.Bool("check-lock", false, "check lockfile state and exit")
	setup := flag.Bool("setup", false, "run setup wizard via Telegram bot")
	flag.Parse()

	switch {
	case *stopInstallation != "":
		if err := installation.Stop(*stopInstallation); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
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
	if err := installation.Stop(filepath.Dir(configPath)); err != nil {
		panic("stopping this installation before setup: " + err.Error())
	}

	bot.RunSetup(BotToken, configPath, serviceAccountPath)

	exePath, err := os.Executable()
	if err != nil {
		panic("getting executable path: " + err.Error())
	}

	if err := installation.RegisterWindowsTask(installation.TaskName(InstallationID, false), exePath); err != nil {
		panic(err.Error())
	}

	// Starts the service now instead of leaving the player to reboot or
	// start it manually. Not fatal if it fails — the task is already
	// registered and will still run on the next logon — so this only logs.
	if out, err := exec.Command("schtasks.exe", "/Run", "/TN", installation.TaskName(InstallationID, false)).CombinedOutput(); err != nil {
		slog.Error("starting task scheduler entry", "err", err, "output", string(out))
	}
	os.Exit(0)
}

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
	closeLog := logging.Configure(logPath)
	defer closeLog()

	// Belt-and-suspenders on top of -H=windowsgui — see hideConsole's doc
	// comment for why the build flag alone wasn't enough in practice.
	hideConsole()

	// Existing protection must start before any network-backed startup work.
	enforcer := client.NewEnforcer(lockfilePath)
	enforcer.EnforceActive()
	go enforcer.Run(make(chan struct{}))

	// Construct the storage backend (shared with the split server via
	// internal/backend). On the sheets path this blocks in EnsureSheetID until a
	// sheet is configured; the watcher/server/bot goroutines can't run without a
	// store, so it resolves first.
	sc, backendName, err := backend.Open(BotToken, configPath, serviceAccountPath, dbPath, fallbackPath)
	if err != nil {
		slog.Error("opening storage backend", "err", err)
		os.Exit(1)
	}

	slog.Info("tracker service starting", "backend", backendName)
	alertChan := make(chan string, 20)

	// The collector (watcher-side) and server (brain-side) run in one process
	// here, connected by an in-process link: file changes go up as uploads, the
	// hard-stop lock comes back down as a command. The same two roles are the
	// two separate binaries of the split build; combined mode is just both over
	// this in-process link.
	link := transport.NewLink()
	srv, err := server.NewServer(sc, configPath, link, alertChan)
	if err != nil {
		slog.Error("creating server", "err", err)
		os.Exit(1)
	}
	go func() { err := srv.Run(); slog.Error("server worker stopped", "err", err); os.Exit(1) }()

	collector := client.NewCollector(link, enforcer, nil)
	go func() {
		err := collector.Serve(configPath, nil)
		slog.Error("collector stopped unexpectedly", "err", err)
		os.Exit(1)
	}()

	// All-local: a manual /hardstop trigger enforces inline, exactly as before
	// the split — same client.Enforce (taskkill + write the local lockfile), no
	// change in behavior. The split server instead sends the lock down the link.

	// A /scan backfill is triggered the same way in both builds: send a rescan
	// command down the link, which the collector (here, in-process) turns into a
	// walk of the watched folders.
	sendLock := func(lock config.LockFile) {
		link.Commands <- transport.Command{Kind: transport.CommandLock, Lock: &lock}
	}
	requestRescan := func() { link.Commands <- transport.Command{Kind: transport.CommandRescan} }

	go func() {
		// All-local: server and collector share this machine's clock, so the
		// read-side date windows anchor to time.Now — unchanged behavior.
		err := bot.Run(BotToken, alertChan, sc, configPath, serviceAccountPath, dbPath, sendLock, requestRescan, time.Now, installation.TaskName(InstallationID, false), srv.Operations, srv.CollectorLock)
		// bot.Run's error return was previously discarded entirely by a
		// bare "go bot.Run(...)" — if NewBotAPI failed inside it (bad
		// token, network issue at that instant), the goroutine would exit
		// silently and nothing would ever be visibly wrong: the process
		// stays alive (select{} below blocks forever), Task Scheduler
		// shows it as running, and the bot simply never listens for
		// anything. This log is the only way that failure was ever going
		// to be visible.
		slog.Error("bot.Run goroutine returned — the bot is no longer listening for anything", "err", err)
		os.Exit(1)
	}()

	select {}
}
