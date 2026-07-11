package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

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
		lock.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
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
	if cfg.Setup.GoogleSheetID == nil {
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

	go runWorker(sessionChan, alertChan, sheetsClient, configPath, lockfilePath)
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
	configPath, lockfilePath string,
) {
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
				bankroll, _ := sc.GetCurrentBankroll()
				sign, absNet := "+", session.NetUSD
				if absNet < 0 {
					sign, absNet = "-", -absNet
				}
				alertChan <- fmt.Sprintf("✅ Cash Session\nStake: %s\nNet: %s$%.2f\nHands: %d\nDuration: %d min\nBankroll: $%.2f",
					session.Stake, sign, absNet, session.Hands, session.DurationMin, bankroll)
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
				slog.Info("tournament queued for satellite/ticket confirmation", "tournament_id", t.TournamentID)
				continue
			}

			if err := sc.AppendTournament(t); err != nil {
				slog.Error("appending tournament to sheets", "err", err)
			}
			if err := hardstop.EvaluateTournament(t, sc, configPath, lockfilePath, alertChan); err != nil {
				slog.Error("evaluating hard stop", "err", err)
			}
			if err := bot.EvaluateDownswingAlert(t.BuyInUSD, sc, configPath, alertChan); err != nil {
				slog.Error("evaluating downswing alert", "err", err)
			}
			bankroll, _ := sc.GetCurrentBankroll()
			sign, absNet := "+", t.NetUSD
			if absNet < 0 {
				sign, absNet = "-", -absNet
			}
			var alert string
			if t.Type == "spingo" {
				alert = fmt.Sprintf("🎰 Spin & Go\nBuy-in: $%.2f\nPrize pool: $%.2f (%gx)\nFinish: %s\nNet: %s$%.2f\nBankroll: $%.2f",
					t.BuyInUSD, t.PrizePoolUSD, *t.Multiplier, ordinal(t.FinishPos), sign, absNet, bankroll)
			} else {
				alert = fmt.Sprintf("🏆 MTT\nBuy-in: $%.2f\nFinish: %s / %d\nNet: %s$%.2f\nBankroll: $%.2f",
					t.BuyInUSD, ordinal(t.FinishPos), t.TotalPlayers, sign, absNet, bankroll)
			}
			alertChan <- alert
		}
	}
}

// ordinal formats n as "1st", "2nd", "3rd", "4th", etc.
func ordinal(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return fmt.Sprintf("%dth", n)
	}
	switch n % 10 {
	case 1:
		return fmt.Sprintf("%dst", n)
	case 2:
		return fmt.Sprintf("%dnd", n)
	case 3:
		return fmt.Sprintf("%drd", n)
	default:
		return fmt.Sprintf("%dth", n)
	}
}
