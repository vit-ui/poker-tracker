package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/joho/godotenv"
	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

// BotToken is injected at build time via -ldflags "-X main.BotToken=...".
var BotToken string

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
	// Ignore .env load error; LOCKFILE_PATH may also be set in the system environment.
	_ = godotenv.Load()
	lockfilePath := os.Getenv("LOCKFILE_PATH")
	if lockfilePath == "" {
		fmt.Fprintln(os.Stderr, "LOCKFILE_PATH not set in .env or environment")
		os.Exit(1)
	}
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
		lock.ExpiresAt.Format("2006-01-02 15:04:05 UTC"))
	os.Exit(1)
}

func runSetup() {
	if err := godotenv.Load(); err != nil {
		panic("loading .env: " + err.Error())
	}
	serviceAccountPath := os.Getenv("GOOGLE_SERVICE_ACCOUNT_PATH")
	configPath := os.Getenv("CONFIG_PATH")
	lockfilePath := os.Getenv("LOCKFILE_PATH")
	if serviceAccountPath == "" || configPath == "" || lockfilePath == "" {
		panic("GOOGLE_SERVICE_ACCOUNT_PATH, CONFIG_PATH, and LOCKFILE_PATH must be set in .env")
	}

	bot.RunSetup(BotToken, configPath, serviceAccountPath)

	exePath, err := os.Executable()
	if err != nil {
		panic("getting executable path: " + err.Error())
	}
	if err := exec.Command("schtasks.exe",
		"/Create",
		"/TN", "Poker Tracker",
		"/TR", exePath+" --serve",
		"/SC", "ONLOGON",
		"/RL", "HIGHEST",
		"/F",
	).Run(); err != nil {
		panic("registering Task Scheduler entry: " + err.Error())
	}
	os.Exit(0)
}

func runServe() {
	if err := godotenv.Load(); err != nil {
		panic("loading .env: " + err.Error())
	}
	serviceAccountPath := os.Getenv("GOOGLE_SERVICE_ACCOUNT_PATH")
	configPath := os.Getenv("CONFIG_PATH")
	lockfilePath := os.Getenv("LOCKFILE_PATH")
	if serviceAccountPath == "" || configPath == "" || lockfilePath == "" {
		panic("GOOGLE_SERVICE_ACCOUNT_PATH, CONFIG_PATH, and LOCKFILE_PATH must be set in .env")
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		panic("loading config: " + err.Error())
	}
	if cfg.Setup.GoogleSheetID == nil || cfg.Setup.PokerStarsScreenName == nil {
		panic("Run setup first: use the installer or tracker.exe --setup")
	}

	sheetsClient, err := sheets.NewClient(serviceAccountPath, *cfg.Setup.GoogleSheetID)
	if err != nil {
		panic("initializing sheets client: " + err.Error())
	}
	slog.Info("startup complete")

	sessionChan := make(chan watcher.FileEvent, 10)
	alertChan := make(chan string, 20)

	var watcherDone chan struct{}
	if cfg.Paths.HandHistory != nil && cfg.Paths.TournamentSummary != nil {
		hhPath, tsPath := resolvePaths(cfg)
		watcherDone = make(chan struct{})
		go func() {
			if err := watcher.Watch(watcherDone, hhPath, tsPath, sessionChan); err != nil {
				slog.Error("watcher stopped", "err", err)
			}
		}()
	} else {
		slog.Info("watcher not started: run /paths to configure folder locations")
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

	screenName := *cfg.Setup.PokerStarsScreenName
	go runWorker(sessionChan, alertChan, sheetsClient, screenName, configPath, lockfilePath)
	go bot.Run(BotToken, alertChan, sheetsClient, configPath, lockfilePath, restartWatcher)

	select {}
}

// resolvePaths joins the base hand history and tournament summary paths with
// the player's screen name, which PokerStars uses as a subfolder for all files.
func resolvePaths(cfg *config.Config) (string, string) {
	return filepath.Join(*cfg.Paths.HandHistory, *cfg.Setup.PokerStarsScreenName),
		filepath.Join(*cfg.Paths.TournamentSummary, *cfg.Setup.PokerStarsScreenName)
}

func runWorker(
	sessionChan <-chan watcher.FileEvent,
	alertChan chan<- string,
	sc *sheets.Client,
	screenName, configPath, lockfilePath string,
) {
	for event := range sessionChan {
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
				hardstop.Evaluate(*session, sc, configPath, lockfilePath, alertChan)
				bankroll, _ := sc.GetCurrentBankroll()
				sign := "+"
				if session.NetUSD < 0 {
					sign = ""
				}
				alertChan <- fmt.Sprintf("Cash %s | Net: %s$%.2f | Hands: %d | %dmin | Bankroll: $%.2f",
					session.Stake, sign, session.NetUSD, session.Hands, session.DurationMin, bankroll)
			} else if tourneyStats != nil {
				if err := sc.UpdateTournamentVPIPPFR(tourneyStats.TournamentID, tourneyStats.VPIP, tourneyStats.PFR); err != nil {
					slog.Error("updating tournament VPIP/PFR", "err", err)
				}
				slog.Info("tournament hand stats recorded", "tournament_id", tourneyStats.TournamentID)
			} else {
				slog.Error("ParseCashFile returned no session and no tournament stats", "path", event.Path)
			}

		case "spingo":
			t, err := parsers.ParseTournamentFile(event.Path, screenName)
			if errors.Is(err, parsers.ErrFreeroll) {
				continue
			}
			if err != nil {
				slog.Error("parsing tournament", "path", event.Path, "err", err)
				continue
			}
			if err := sc.AppendTournament(t); err != nil {
				slog.Error("appending tournament to sheets", "err", err)
			}
			hardstop.EvaluateTournament(t, sc, configPath, lockfilePath, alertChan)
			bankroll, _ := sc.GetCurrentBankroll()
			gameType := "MTT"
			if t.Type == "spingo" {
				gameType = "Spin & Go"
			}
			sign := "+"
			if t.NetUSD < 0 {
				sign = ""
			}
			alertChan <- fmt.Sprintf("%s $%.2f | Net: %s$%.2f | Pos: %d/%d | Bankroll: $%.2f",
				gameType, t.BuyInUSD, sign, t.NetUSD, t.FinishPos, t.TotalPlayers, bankroll)
		}
	}
}
