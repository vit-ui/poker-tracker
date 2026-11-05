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
	"runtime/debug"
	"strings"

	"github.com/vit-ui/self-tracker/internal/client"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/installation"
	"github.com/vit-ui/self-tracker/internal/logging"
	"github.com/vit-ui/self-tracker/internal/transport"
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
	printRole := flag.Bool("role", false, "print this executable role")
	roleFile := flag.String("role-output", "", "write the executable role to a setup result file")
	stopInstallation := flag.String("stop-installation", "", "stop tracker processes in the specified installation directory")
	serve := flag.Bool("serve", false, "run the collector")
	setup := flag.Bool("setup", false, "configure folders, screen name, and the server connection string")
	checkLock := flag.Bool("check-lock", false, "check the local lockfile and exit (0 = clear, 1 = locked)")
	flag.Parse()

	switch {
	case *printRole:
		if *roleFile != "" {
			if err := os.WriteFile(*roleFile, []byte("client"), 0600); err != nil {
				os.Exit(1)
			}
		} else {
			fmt.Println("client")
		}
	case *stopInstallation != "":
		if err := installation.Stop(*stopInstallation); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case *serve:
		if err := runServe(); err != nil {
			os.Exit(1)
		}
	case *setup:
		runSetup()
	case *checkLock:
		runCheckLock()
	default:
		fmt.Fprintln(os.Stderr, "usage: tracker-client --serve | --setup | --check-lock")
		os.Exit(2)
	}
}

func runServe() (failure error) {
	closeLog := logging.Configure(filepath.Join(filepath.Dir(configPath), "tracker.log"))
	defer closeLog()
	defer func() {
		if failure != nil {
			slog.Error("service stopped", "err", failure)
		}
	}()
	shutdown, releaseShutdown, err := installation.ListenShutdown(filepath.Dir(configPath))
	if err != nil {
		return err
	}
	defer releaseShutdown()
	// Bound this idle background service's footprint. Same knobs, and the same
	// reasoning, as cmd/tracker's runServe (see its long comment): the scheduled
	// task can't set GOMEMLIMIT/GOGC env vars, so set the in-code equivalents.
	// The collector is the lightest role — it only watches files, streams bytes,
	// and enforces locks; it never parses or stores — so the combined build's
	// 32 MiB soft limit is ample here too.
	debug.SetMemoryLimit(32 << 20)
	debug.SetGCPercent(50)

	// Enforcement belongs to the process, not a network connection. The same
	// enforcer retains unsaved protection across every reconnect attempt.
	enforcer := client.NewEnforcer(lockfilePath)
	if err := client.RunNetworkCollector(configPath, enforcer, shutdown); err != nil {
		slog.Error("collector stopped unexpectedly", "err", err)
		return err
	}
	return nil
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
