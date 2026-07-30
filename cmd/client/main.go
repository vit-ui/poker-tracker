// Command tracker-client is the collector half of the split build: it watches
// the PokerStars folders on the poker PC, streams each file's new bytes to the
// server over TLS, and enforces the hard-stop locks the server sends back
// (killing PokerStars, writing the lockfile). It never parses, stores, or talks
// to Telegram — that's the server. The all-local build is cmd/tracker; this
// binary is only for a poker PC paired to a separate server.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/vit-ui/self-tracker/internal/client"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

var (
	configPath   string
	lockfilePath string
)

func init() {
	exe, err := os.Executable()
	if err != nil {
		panic("getting executable path: " + err.Error())
	}
	dir := filepath.Dir(exe)
	configPath = filepath.Join(dir, "config.json")
	lockfilePath = filepath.Join(dir, "lockfile.json")
}

func main() {
	serve := flag.Bool("serve", false, "run the collector")
	setup := flag.Bool("setup", false, "configure folders, screen name, and the server connection string")
	checkLock := flag.Bool("check-lock", false, "check the local lockfile and exit (0 = clear, 1 = locked)")
	flag.Parse()

	switch {
	case *serve:
		runServe()
	case *setup:
		runSetup()
	case *checkLock:
		runCheckLock()
	default:
		fmt.Fprintln(os.Stderr, "usage: tracker-client --serve | --setup | --check-lock")
		os.Exit(2)
	}
}

func runServe() {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		os.Exit(1)
	}
	if cfg.Setup.ConnectionString == nil || cfg.Paths.HandHistory == nil ||
		cfg.Paths.TournamentSummary == nil || cfg.Setup.PokerStarsScreenName == nil {
		slog.Error("client is not set up — run: tracker-client --setup")
		os.Exit(1)
	}
	info, err := transport.ParseConnectionString(*cfg.Setup.ConnectionString)
	if err != nil {
		slog.Error("invalid connection string — re-run --setup", "err", err)
		os.Exit(1)
	}
	hh := filepath.Join(*cfg.Paths.HandHistory, *cfg.Setup.PokerStarsScreenName)
	ts := filepath.Join(*cfg.Paths.TournamentSummary, *cfg.Setup.PokerStarsScreenName)

	sessionChan := make(chan watcher.FileEvent, 10)
	go func() {
		if err := watcher.Watch(make(chan struct{}), hh, ts, sessionChan); err != nil {
			slog.Error("file watching stopped", "err", err)
			os.Exit(1)
		}
	}()

	// Enforce a still-active lock immediately on startup (e.g. after a restart
	// mid-lockout), independent of the server connection.
	go client.RunEnforcementLoop(lockfilePath, make(chan struct{}))

	link, dead, err := transport.DialServer(info)
	if err != nil {
		slog.Error("connecting to the server", "addr", info.Address(), "err", err)
		os.Exit(1) // the service manager restarts us, which re-dials
	}
	slog.Info("connected to the server", "addr", info.Address())

	collector := client.NewCollector(link, lockfilePath)
	collector.Run(sessionChan, configPath, dead) // blocks until the link drops
	slog.Error("server connection lost — exiting for the service manager to restart and reconnect")
	os.Exit(1)
}

func runSetup() {
	scanner := bufio.NewScanner(os.Stdin)
	prompt := func(label string) string {
		fmt.Print(label)
		if !scanner.Scan() {
			return ""
		}
		return strings.TrimSpace(scanner.Text())
	}

	fmt.Println("Connect this poker PC to your tracker server.")
	fmt.Println("Your server printed a connection string when you ran its setup — paste the whole line.")
	cs := prompt("Connection string: ")
	if _, err := transport.ParseConnectionString(cs); err != nil {
		fmt.Fprintln(os.Stderr, "that doesn't look like a valid connection string:", err)
		os.Exit(1)
	}
	hh := prompt("PokerStars Hand History base folder: ")
	ts := prompt("PokerStars Tournament Summary base folder: ")
	name := prompt("PokerStars screen name: ")
	if hh == "" || ts == "" || name == "" {
		fmt.Fprintln(os.Stderr, "all fields are required")
		os.Exit(1)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loading config:", err)
		os.Exit(1)
	}
	cfg.Setup.ConnectionString = &cs
	cfg.Setup.PokerStarsScreenName = &name
	cfg.Paths.HandHistory = &hh
	cfg.Paths.TournamentSummary = &ts
	if err := config.SaveConfig(configPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "saving config:", err)
		os.Exit(1)
	}
	fmt.Println("Configured. Start the collector with: tracker-client --serve")
}

// runCheckLock reads the local lockfile the server's lock commands wrote: absent
// → exit 0 (clear), expired → delete and exit 0, active → print and exit 1. It
// trusts the saved file and its dates; it never contacts the server. Same
// contract as the all-local tracker's --check-lock, over the client-side file.
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
