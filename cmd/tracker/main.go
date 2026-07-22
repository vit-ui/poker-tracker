package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/storage"
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

// telegramErrorHandler wraps the default slog handler and additionally
// forwards Error-level records to alertChan, so operational failures stay
// visible via Telegram now that release builds use -H=windowsgui (no
// console window is ever created, so a Task-Scheduler-launched --serve
// has nowhere else to show them — see the release build command). Records
// below Error still go through the wrapped handler unchanged; this never
// suppresses anything, only adds a second destination for the ones that
// matter operationally.
type telegramErrorHandler struct {
	slog.Handler
	alertChan chan<- string
}

func (h *telegramErrorHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		var attrs strings.Builder
		r.Attrs(func(a slog.Attr) bool {
			fmt.Fprintf(&attrs, " %s=%v", a.Key, a.Value)
			return true
		})
		h.alertChan <- fmt.Sprintf("⚠️ %s%s", r.Message, attrs.String())
	}
	return h.Handler.Handle(ctx, r)
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
	// is never invalidated out from under it. Route the default handler
	// to io.Discard before hideConsole runs so no log write ever touches
	// that handle afterward. Error-level records still reach Telegram
	// once telegramErrorHandler wraps this default further down — only
	// the dead local destination underneath it is removed.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Belt-and-suspenders on top of -H=windowsgui — see hideConsole's doc
	// comment for why the build flag alone wasn't enough in practice.
	hideConsole()

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		panic("loading config: " + err.Error())
	}

	backend, err := resolveStorageBackend(cfg, configPath)
	if err != nil {
		panic(err.Error())
	}

	var sc storage.Store
	switch backend {
	case "sheets":
		// Blocks and asks via Telegram if google_sheet_id isn't set yet,
		// rather than refusing to start — nothing else here can run without
		// a sheets client, so this has to resolve before the
		// watcher/worker/bot goroutines exist. Only called on the sheets
		// path — a sqlite install must never need Google credentials on
		// disk.
		sheetID, err := bot.EnsureSheetID(BotToken, configPath, serviceAccountPath)
		if err != nil {
			panic("resolving google sheet: " + err.Error())
		}
		sheetsClient, err := sheets.NewClient(serviceAccountPath, sheetID, fallbackPath)
		if err != nil {
			panic("initializing sheets client: " + err.Error())
		}
		sc = sheetsClient
	case "sqlite":
		store, err := sqlitestore.NewStore(dbPath)
		if err != nil {
			panic("initializing sqlite store: " + err.Error())
		}
		sc = store
	}

	// Reload — EnsureSheetID above may have written telegram_chat_id and/or
	// google_sheet_id back to config.json.
	cfg, err = config.LoadConfig(configPath)
	if err != nil {
		panic("loading config: " + err.Error())
	}

	sessionChan := make(chan watcher.FileEvent, 10)
	alertChan := make(chan string, 20)

	// From here on, an slog.Error anywhere in this process also reaches
	// Telegram via alertChan (see telegramErrorHandler) — the bot goroutine
	// started below is the only thing that ever reads alertChan and sends
	// to Telegram, so this doesn't violate that rule, it just adds a
	// second producer.
	slog.SetDefault(slog.New(&telegramErrorHandler{
		Handler:   slog.Default().Handler(),
		alertChan: alertChan,
	}))

	var watcherDone chan struct{}
	if cfg.Paths.HandHistory != nil && cfg.Paths.TournamentSummary != nil {
		hhPath, tsPath := resolvePaths(cfg)
		watcherDone = make(chan struct{})
		go func() {
			if err := watcher.Watch(watcherDone, hhPath, tsPath, sessionChan); err != nil {
				slog.Error("watcher stopped", "err", err)
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
		if watcherDone != nil {
			close(watcherDone)
		}
		watcherDone = make(chan struct{})
		go func() {
			if err := watcher.Watch(watcherDone, hhPath, tsPath, sessionChan); err != nil {
				slog.Error("watcher stopped", "err", err)
			}
		}()
	}

	go runWorker(sessionChan, alertChan, sc, configPath, lockfilePath)

	// Runs for the life of this process — the done channel is never closed,
	// since RunEnforcementLoop is only meant to stop when --serve itself
	// exits.
	go hardstop.RunEnforcementLoop(lockfilePath, make(chan struct{}))

	go func() {
		err := bot.Run(BotToken, alertChan, sc, configPath, lockfilePath, serviceAccountPath, dbPath, restartWatcher)
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

// resolveStorageBackend returns which storage backend runServe should
// construct: "sheets" or "sqlite". storage_backend already set is the normal
// case and is returned as-is. storage_backend unset but google_sheet_id
// already set is a v1.0 install predating this field — it's silently
// migrated to "sheets" and written back so this check only ever runs once
// per install. Both unset means --setup has never run.
func resolveStorageBackend(cfg *config.Config, configPath string) (string, error) {
	if cfg.Setup.StorageBackend != nil {
		return *cfg.Setup.StorageBackend, nil
	}
	if cfg.Setup.GoogleSheetID != nil {
		backend := "sheets"
		cfg.Setup.StorageBackend = &backend
		if err := config.SaveConfig(configPath, cfg); err != nil {
			return "", fmt.Errorf("saving config: %w", err)
		}
		return backend, nil
	}
	return "", errors.New("Run setup first: use the installer or tracker.exe --setup")
}

// resolvePaths joins the base hand history and tournament summary paths with
// the player's screen name, which PokerStars uses as a subfolder for all files.
func resolvePaths(cfg *config.Config) (string, string) {
	return filepath.Join(*cfg.Paths.HandHistory, *cfg.Setup.PokerStarsScreenName),
		filepath.Join(*cfg.Paths.TournamentSummary, *cfg.Setup.PokerStarsScreenName)
}

// cashSessionSummaryQuietPeriod is how long a cash table must go without a
// new hand before its post-session summary is sent. A hand history file
// gets a fresh event on every hand played at that table, so sending
// immediately would fire the summary once per hand with a growing,
// still-incomplete snapshot each time — this waits for the table to go
// quiet and sends the final numbers once, the same time-gap technique
// poker trackers use to detect a session boundary. 5 minutes trades a
// faster summary after quitting against a higher chance of a mid-session
// break tripping it early (a resumed table then sends an updated summary on
// the next quiet period).
const cashSessionSummaryQuietPeriod = 5 * time.Minute

func runWorker(
	sessionChan <-chan watcher.FileEvent,
	alertChan chan<- string,
	sc storage.Store,
	configPath, lockfilePath string,
) {
	var summaryMu sync.Mutex
	summaryTimers := make(map[string]*time.Timer)

	// firedAlerts dedups the streak/daily/stake alert evaluations: a cash
	// hand history file gets a fresh event on every hand played, and those
	// conditions, once true, stay true for the rest of the session or poker
	// day — without this, the same warning would re-send (and re-query the
	// store) on every single hand. Keys are per session (FirstHandID /
	// TournamentID) or per poker day (the day's start time); the map lives
	// for the process and is rebuilt empty on restart, worst case repeating
	// one already-sent warning. The immediate per-hand alerts (session loss,
	// downswing) keep their original fire-every-time behavior — they carry a
	// changing number and are safety-relevant.
	firedAlerts := make(map[string]bool)

	for event := range sessionChan {
		// Re-read on every event rather than capturing once at startup —
		// the startup guard no longer requires pokerstars_screen_name to be
		// set, so it can still be null when this goroutine launches and get
		// filled in later via the bot's recovery prompt.
		cfg, err := config.LoadConfig(configPath)
		if err != nil {
			slog.Error("loading config for screen name", "err", err)
			continue
		}
		if cfg.Setup.PokerStarsScreenName == nil {
			slog.Error("pokerstars screen name not configured, skipping file", "path", event.Path)
			continue
		}
		screenName := *cfg.Setup.PokerStarsScreenName

		switch event.Type {
		case "cash":
			session, tourneyStats, err := parsers.ParseCashFile(event.Path, screenName)
			if err != nil {
				slog.Error("parsing cash file", "path", event.Path, "err", err)
				continue
			}
			if session != nil {
				if err := sc.AppendCashSession(*session); err != nil {
					slog.Error("appending cash session to sheets", "err", err)
				}
				if err := hardstop.Evaluate(*session, sc, configPath, lockfilePath, alertChan); err != nil {
					slog.Error("evaluating hard stop", "err", err)
				}
				if err := bot.EvaluateSessionLossAlert(*session, sc, configPath, alertChan); err != nil {
					slog.Error("evaluating session loss alert", "err", err)
				}
				if err := bot.EvaluateDownswingAlert(session.BuyInUSD, sc, configPath, alertChan); err != nil {
					slog.Error("evaluating downswing alert", "err", err)
				}
				sessionKey := "cash:" + session.FirstHandID
				if !firedAlerts["stake:"+sessionKey] {
					if err := bot.EvaluateStakeChangeAlert(*session, sc, alertChan); err != nil {
						slog.Error("evaluating stake change alert", "err", err)
					} else {
						// Marked fired whether or not it sent — the previous
						// stake can't change mid-session, so one evaluation
						// per session settles it either way.
						firedAlerts["stake:"+sessionKey] = true
					}
				}
				evaluateOncePerKeyAlerts(cfg, sc, configPath, alertChan, firedAlerts, sessionKey)
				scheduleCashSessionSummary(&summaryMu, summaryTimers, event.Path, cashSessionSummaryQuietPeriod, alertChan, func() string {
					bankroll, _ := sc.GetCurrentBankroll()
					return cashSessionSummaryText(*session, bankroll)
				})
			} else if tourneyStats != nil {
				if err := sc.UpdateTournamentHandStats(tourneyStats.TournamentID, tourneyStats.VPIP, tourneyStats.PFR, tourneyStats.Hands, tourneyStats.BountyUSD, tourneyStats.IsKnockout); err != nil {
					slog.Error("updating tournament hand stats", "err", err)
				}
			} else {
				slog.Error("ParseCashFile returned no session and no tournament stats", "path", event.Path)
			}

		case "tournament":
			t, err := parsers.ParseTournamentFile(event.Path, screenName)
			if errors.Is(err, parsers.ErrFreeroll) {
				continue
			}
			if err != nil {
				slog.Error("parsing tournament", "path", event.Path, "err", err)
				continue
			}

			// Checked once, here, before either the satellite queue or the
			// direct-append path below runs — a duplicate file event for a
			// tournament already handled (a stray re-trigger, or literally
			// the same file copied) must never send a second Telegram
			// alert, ask the satellite/ticket question twice, or queue a
			// second pending entry. A re-entry rewrite is deliberately *not*
			// treated as a duplicate: PokerStars overwrites the summary each
			// time screenName busts an entry, and each rewrite carries a
			// larger buy-in / different finish that must update the row,
			// ledger, and bankroll. See tournamentAlreadyHandled's doc comment.
			duplicate, err := tournamentAlreadyHandled(t, configPath, sc)
			if err != nil {
				slog.Error("checking for duplicate tournament processing", "err", err)
			} else if duplicate {
				continue
			}

			if t.LikelySatellite {
				pendingPath := filepath.Join(filepath.Dir(configPath), "pending_tournaments.json")
				pending, err := config.LoadPendingTournaments(pendingPath)
				if err != nil {
					slog.Error("loading pending tournaments", "err", err)
					continue
				}
				pending = append(pending, config.PendingTournament{
					Tournament:    t,
					FirstQueuedAt: time.Now(),
					// Zero value, not time.Now(): the ticker only sends once
					// time.Since(LastRemindedAt) exceeds 30 minutes, and a
					// zero time is always more than 30 minutes in the past —
					// this makes the first prompt fire on the next tick
					// instead of being delayed by a full reminder interval.
					LastRemindedAt: time.Time{},
				})
				if err := config.SavePendingTournaments(pendingPath, pending); err != nil {
					slog.Error("saving pending tournaments", "err", err)
				}
				continue
			}

			netUSD, err := sc.AppendTournament(t)
			if err != nil {
				slog.Error("appending tournament to sheets", "err", err)
			}
			if err := hardstop.EvaluateTournament(t, sc, configPath, lockfilePath, alertChan); err != nil {
				slog.Error("evaluating hard stop", "err", err)
			}
			if err := bot.EvaluateDownswingAlert(t.BuyInUSD, sc, configPath, alertChan); err != nil {
				slog.Error("evaluating downswing alert", "err", err)
			}
			evaluateOncePerKeyAlerts(cfg, sc, configPath, alertChan, firedAlerts, "tournament:"+t.TournamentID)
			bankroll, _ := sc.GetCurrentBankroll()
			alertChan <- tournamentAlertText(t, netUSD, bankroll)
		}
	}
}

// evaluateOncePerKeyAlerts runs the three threshold alerts whose condition,
// once true, stays true for the rest of the streak or poker day — losing
// streak, unified daily loss, Spin & Go daily loss — deduped through
// firedAlerts (see its declaration in runWorker). The streak alert is keyed
// per session (a new session is a new streak length to judge); the two
// daily alerts are keyed per poker day, so each sends at most once between
// day boundaries no matter how many sessions land in it.
func evaluateOncePerKeyAlerts(cfg *config.Config, sc storage.Store, configPath string, alertChan chan<- string, firedAlerts map[string]bool, sessionKey string) {
	if !firedAlerts["streak:"+sessionKey] {
		fired, err := bot.EvaluateLosingStreakAlert(sc, configPath, alertChan)
		if err != nil {
			slog.Error("evaluating losing streak alert", "err", err)
		} else if fired {
			firedAlerts["streak:"+sessionKey] = true
		}
	}

	dayKey := hardstop.PokerDayStart(cfg, time.Now()).Format(time.RFC3339)
	if !firedAlerts["daily:"+dayKey] {
		fired, err := bot.EvaluateDailyLossAlert(sc, configPath, alertChan)
		if err != nil {
			slog.Error("evaluating daily loss alert", "err", err)
		} else if fired {
			firedAlerts["daily:"+dayKey] = true
		}
	}
	if !firedAlerts["spingo_daily:"+dayKey] {
		fired, err := bot.EvaluateSpingoDailyLossAlert(sc, configPath, alertChan)
		if err != nil {
			slog.Error("evaluating spin & go daily loss alert", "err", err)
		} else if fired {
			firedAlerts["spingo_daily:"+dayKey] = true
		}
	}
}

// tournamentAlreadyHandled reports whether tournament t has already been
// queued for a satellite/ticket answer, or already fully recorded in the
// Tournaments sheet with the same money-relevant data. This is the single
// place that guards against a duplicate tournament summary file event (a
// stray re-trigger, or literally the same file copied) causing a second
// Telegram alert, a second satellite/ticket question, or a second
// pending-queue entry for the same TournamentID — every caller checks here
// first, so this is the only place that ever needs to.
//
// A re-entry rewrite is intentionally *not* a duplicate: PokerStars overwrites
// the same summary each time screenName busts an entry, and each rewrite
// carries a larger total buy-in and a different finish, which must flow
// through to the row/ledger/bankroll (AppendTournament upserts by
// TournamentID, so re-processing corrects in place rather than duplicating).
func tournamentAlreadyHandled(t parsers.Tournament, configPath string, sc storage.Store) (bool, error) {
	pendingPath := filepath.Join(filepath.Dir(configPath), "pending_tournaments.json")
	pending, err := config.LoadPendingTournaments(pendingPath)
	if err != nil {
		return false, err
	}
	// GetTournamentsSince skips partial rows (no Date — created only by a
	// hand file arriving before the summary; see sheets.GetTournamentsSince),
	// so a tournament whose hand stats arrived first but whose summary
	// hasn't been processed yet is correctly reported as not yet handled.
	recorded, err := sc.GetTournamentsSince(time.Time{})
	if err != nil {
		return false, err
	}
	return isDuplicateTournament(t, pending, recorded), nil
}

// isDuplicateTournament is the pure decision tournamentAlreadyHandled wraps
// around its two I/O reads: t is a duplicate if it's already awaiting an
// answer in the pending queue, or already recorded in the Tournaments sheet
// with the same buy-in, finish position, and field size.
//
// The content comparison is what lets a re-entry rewrite through: an
// already-recorded tournament whose buy-in / finish / field size have changed
// is a fresh bust (screenName re-entered and busted again), not a duplicate,
// so it must be re-processed to update the stored data. Net is deliberately
// not compared — a hand file folding in knockout bounty changes the recorded
// net without any re-entry, and that is not a re-entry the summary should
// overwrite.
func isDuplicateTournament(t parsers.Tournament, pending []config.PendingTournament, recorded []parsers.Tournament) bool {
	for _, p := range pending {
		if p.Tournament.TournamentID == t.TournamentID {
			return true
		}
	}
	for _, r := range recorded {
		if r.TournamentID == t.TournamentID {
			return t.BuyInUSD == r.BuyInUSD && t.FinishPos == r.FinishPos && t.TotalPlayers == r.TotalPlayers
		}
	}
	return false
}

// scheduleCashSessionSummary (re)starts a quietPeriod timer for path, keyed
// the same way watcher.go's own write-debounce is — one growing hand
// history file per table sitting. A new hand for the same table stops the
// pending timer and starts a fresh one, so an actively-played table never
// fires early; the summary only goes out once quietPeriod has passed with
// no new hand, built from whatever buildMessage returns at that moment
// (the latest snapshot, captured by the caller's closure). Mirrors
// watcher.go's mutex-protected timer map exactly, since the timer callback
// runs on its own goroutine, not the caller's.
func scheduleCashSessionSummary(mu *sync.Mutex, timers map[string]*time.Timer, path string, quietPeriod time.Duration, alertChan chan<- string, buildMessage func() string) {
	mu.Lock()
	defer mu.Unlock()
	if t, ok := timers[path]; ok {
		t.Stop()
	}
	timers[path] = time.AfterFunc(quietPeriod, func() {
		mu.Lock()
		delete(timers, path)
		mu.Unlock()
		alertChan <- buildMessage()
	})
}

// tournamentAlertText formats the post-tournament summary alert. When the
// summary reports re-entries, a trailing line makes clear this is a re-entry
// bust and that it supersedes the earlier alert for the same tournament —
// PokerStars overwrites the summary on each bust, so screenName gets one
// corrected alert per entry, each reflecting the running total buy-in and net.
func tournamentAlertText(t parsers.Tournament, netUSD, bankroll float64) string {
	sign, absNet := "+", netUSD
	if absNet < 0 {
		sign, absNet = "-", -absNet
	}
	var msg string
	if t.BaseType() == "spingo" {
		msg = fmt.Sprintf("🎰 Spin & Go\nBuy-in: $%.2f\nPrize pool: %s\nFinish: %s\nNet: %s$%.2f\nBankroll: $%.2f",
			t.BuyInUSD, bot.SpinGoPrizePoolText(t), bot.Ordinal(t.FinishPos), sign, absNet, bankroll)
	} else {
		msg = fmt.Sprintf("🏆 MTT\nBuy-in: $%.2f\nFinish: %s / %d\nNet: %s$%.2f\nBankroll: $%.2f",
			t.BuyInUSD, bot.Ordinal(t.FinishPos), t.TotalPlayers, sign, absNet, bankroll)
	}
	if t.ReEntries > 0 {
		msg += fmt.Sprintf("\n🔁 Re-entered %d× — %d entries total for this tournament; buy-in and net cover all of them, updating the earlier alert.",
			t.ReEntries, t.ReEntries+1)
	}
	return msg
}

// cashSessionSummaryText formats the post-session summary alert.
func cashSessionSummaryText(session parsers.CashSession, bankroll float64) string {
	sign, absNet := "+", session.NetUSD
	if absNet < 0 {
		sign, absNet = "-", -absNet
	}
	return fmt.Sprintf("✅ Cash Session\nStake: %s\nNet: %s$%.2f\nHands: %d\nDuration: %d min\nBankroll: $%.2f",
		session.Stake, sign, absNet, session.Hands, session.DurationMin, bankroll)
}
