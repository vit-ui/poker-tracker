package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/backend"
	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/client"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/deployment"
	"github.com/vit-ui/self-tracker/internal/installation"
	"github.com/vit-ui/self-tracker/internal/logging"
	"github.com/vit-ui/self-tracker/internal/server"
	"github.com/vit-ui/self-tracker/internal/sqlitestore"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/uiprompt"
	"github.com/vit-ui/self-tracker/internal/update"
)

var InstallationID string

var (
	serviceAccountPath string
	configPath         string
	lockfilePath       string
	dbPath             string
	fallbackPath       string
	logPath            string
)

func init() {
	dir := installation.ExecutableDir()
	serviceAccountPath = filepath.Join(dir, "credentials", "google_service_account.json")
	configPath = filepath.Join(dir, "config.json")
	lockfilePath = filepath.Join(dir, "lockfile.json")
	dbPath = filepath.Join(dir, "tracker.db")
	fallbackPath = filepath.Join(dir, "sheets_fallback.json")
	logPath = filepath.Join(dir, "tracker.log")
}

var Version = "dev"

func main() {
	updater := update.Register(flag.CommandLine)
	printRole := flag.Bool("role", false, "print this executable role")
	roleFile := flag.String("role-output", "", "write the executable role to a setup result file")
	stopInstallation := flag.String("stop-installation", "", "stop tracker processes in the specified installation directory")
	serve := flag.Bool("serve", false, "run the background service")
	checkLock := flag.Bool("check-lock", false, "check lockfile state and exit")
	setup := flag.Bool("setup", false, "run setup wizard via Telegram bot")
	installerOwnsTask := flag.Bool("installer-owns-task", false, "leave task registration and startup to the installer")
	transferStage := flag.String("transfer-stage", "", "prepare and verify combined-to-split transfer")
	transferActivate := flag.Bool("transfer-activate", false, "activate verified deployment transfer")
	transferComplete := flag.Bool("transfer-complete", false, "record completed collector cutover")
	transferRollback := flag.Bool("transfer-rollback", false, "roll back a transfer-owned destination and restore source")
	resolveStorage := flag.Bool("resolve-storage", false, "validate and save the selected storage settings")
	telegramSetup := bot.RegisterTelegramSetup(flag.CommandLine)
	flag.Parse()
	if handled, err := updater.Handle(flag.CommandLine, "combined", Version, InstallationID); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if updater.LocalUpdate() {
			if err := bot.FinishTelegramSetup(configPath); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}
		return
	}
	if handled, exitCode := telegramSetup.Handle(flag.CommandLine, configPath); handled {
		os.Exit(exitCode)
	}

	switch {
	case *resolveStorage:
		cfg, err := config.LoadConfig(configPath)
		if err == nil {
			err = backend.ResolveSheetsSetup(cfg, serviceAccountPath)
		}
		if err == nil {
			err = config.SaveConfig(configPath, cfg)
		}
		if err != nil {
			_ = uiprompt.TelegramSetupMessage(err.Error())
			os.Exit(1)
		}
	case *transferStage != "":
		runTransfer(func() error { return deployment.Stage(configPath, *transferStage) })
	case *transferComplete:
		runTransfer(func() error { return deployment.Complete(configPath) })
	case *transferActivate:
		runTransfer(func() error { return deployment.Activate(configPath) })
	case *transferRollback:
		runTransfer(func() error { return deployment.Rollback(configPath) })
	case *printRole:
		if err := installation.ReportRole("combined", *roleFile); err != nil {
			os.Exit(1)
		}
	case *stopInstallation != "":
		if err := installation.Stop(*stopInstallation); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case *checkLock:
		os.Exit(client.CheckLock(lockfilePath, os.Stdout, os.Stderr))
	case *setup:
		runSetup(*installerOwnsTask)
	case *serve:
		if err := runServe(); err != nil {
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: tracker [--serve | --check-lock | --setup]")
		os.Exit(1)
	}
}

func runSetup(installerOwnsTask bool) {
	if err := installation.Stop(filepath.Dir(configPath)); err != nil {
		panic("stopping this installation before setup: " + err.Error())
	}

	token, err := bot.TelegramTokenForSetup(configPath)
	if err == nil {
		err = bot.RunSetup(token, configPath, serviceAccountPath)
	}
	if err != nil {
		_ = uiprompt.TelegramSetupMessage(err.Error())
		os.Exit(1)
	}
	if installerOwnsTask {
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		panic("getting executable path: " + err.Error())
	}

	if err := installation.RegisterWindowsTask(installation.TaskName(InstallationID, false), exePath); err != nil {
		panic(err.Error())
	}

	if out, err := exec.Command("schtasks.exe", "/Run", "/TN", installation.TaskName(InstallationID, false)).CombinedOutput(); err != nil {
		slog.Error("starting task scheduler entry", "err", err, "output", string(out))
	}
	os.Exit(0)
}

func runServe() (failure error) {
	debug.SetMemoryLimit(32 << 20)
	debug.SetGCPercent(50)

	closeLog := logging.Configure(logPath)
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

	hideConsole()

	enforcer := client.NewEnforcer(lockfilePath)
	enforcer.EnforceActive()
	enforcementStop, enforcementDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(enforcementDone); enforcer.Run(enforcementStop) }()
	defer func() { close(enforcementStop); <-enforcementDone }()

	sc, backendName, err := backend.Open(configPath, serviceAccountPath, dbPath, fallbackPath)
	if err != nil {
		slog.Error("opening storage backend", "err", err)
		return err
	}

	if db, ok := sc.(*sqlitestore.Store); ok {
		defer func() { failure = errors.Join(failure, db.Close()) }()
	}
	slog.Info("tracker service starting", "backend", backendName)
	alertChan := make(chan string, 20)

	link := transport.NewLink()
	srv, err := server.NewServer(sc, configPath, link, alertChan)
	if err != nil {
		slog.Error("creating server", "err", err)
		return err
	}
	workerDone := make(chan error, 1)
	workerDrain := make(chan struct{})
	srv.Drain = workerDrain
	updater, err := update.NewAgent("combined", Version, InstallationID)
	if err != nil {
		return err
	}
	updateStop := make(chan struct{})
	go updater.Run(updateStop, srv.Ready)
	defer func() { close(updateStop); <-updater.Done() }()
	workerStopped := make(chan struct{})
	go func() { defer close(workerStopped); workerDone <- srv.Run() }()
	sourceOwner, err := deployment.OpenSourceOwner(configPath)
	if err != nil {
		alertsDrained := make(chan struct{})
		go func() {
			defer close(alertsDrained)
			for range alertChan {
			}
		}()
		close(link.Uploads)
		<-workerDone
		close(alertChan)
		<-alertsDrained
		return err
	}
	sourceStop := make(chan struct{})
	ownerControls := make(chan bot.OwnerControl)
	sourceOwner.Controls, sourceOwner.Operations, sourceOwner.Stop, sourceOwner.WorkerStopped = ownerControls, srv.Operations, sourceStop, workerStopped
	sourceStartPaused := sourceOwner.StartPaused()
	go sourceOwner.Run()

	collector := client.NewCollector(link, enforcer, nil)
	collector.Updater = updater
	collectorStop, collectorDone := make(chan struct{}), make(chan error, 1)
	collector.Stop = collectorStop
	collector.StartPaused = sourceStartPaused
	go func() { collectorDone <- collector.Serve(configPath, nil) }()

	sendLock := srv.RequestHardstop
	requestRescan := func() { link.Commands <- transport.Command{Kind: transport.CommandRescan} }

	botReady := make(chan struct{}, 1)
	botStop, botDrained, botDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		botDone <- bot.Run(alertChan, sc, configPath, serviceAccountPath, dbPath, sendLock, requestRescan, time.Now, installation.TaskName(InstallationID, false), srv.Operations, srv.CollectorLock, botStop, botDrained, ownerControls, false, srv.ClosureNotifications, srv.LockStatusText, botReady, updater, srv.ReleaseChecks)
	}()
	startupStop := make(chan struct{})
	defer close(startupStop)
	startupErrors := installation.ReadyAfter(filepath.Dir(configPath), Version, startupStop, srv.Ready, botReady, collector.Ready)
	workerEnded, botEnded := false, false
	var discards sync.WaitGroup
	workerFailed := func(err error) {
		workerEnded = true
		workerDone = nil
		failure = errors.Join(failure, fmt.Errorf("required worker stopped: %v", err))
		close(alertChan)
		discards.Go(func() {
			for range link.Uploads {
			}
		})
	}
	botFailed := func(err error) {
		botEnded = true
		botDone = nil
		failure = errors.Join(failure, fmt.Errorf("required bot stopped: %v", err))
		if !workerEnded {
			discards.Go(func() {
				for range alertChan {
				}
			})
		}
	}
	collectorEnded := false
	select {
	case err := <-startupErrors:
		failure = fmt.Errorf("confirming startup: %w", err)
	case <-sourceOwner.Shutdown:
	case err := <-sourceOwner.Errors:
		failure = err
	case <-shutdown:
	case <-botDrained:
	case err := <-botDone:
		botFailed(err)
	case err := <-workerDone:
		workerFailed(err)
	case err := <-collectorDone:
		collectorEnded = true
		failure = fmt.Errorf("required collector stopped: %v", err)
	}
	close(sourceStop)
	sourceOwner.Close()
	close(botStop)
	if !botEnded && !workerEnded {
		select {
		case <-botDrained:
		case err := <-botDone:
			botFailed(err)
		case err := <-workerDone:
			workerFailed(err)
		}
	}
	close(workerDrain)
	close(collectorStop)
	for !collectorEnded {
		select {
		case err := <-collectorDone:
			collectorEnded = true
			failure = errors.Join(failure, err)
		case err := <-workerDone:
			workerFailed(err)
		case err := <-botDone:
			botFailed(err)
		}
	}
	close(link.Uploads)
	if !workerEnded {
		waiting := true
		for waiting {
			select {
			case err := <-workerDone:
				failure = errors.Join(failure, err)
				waiting = false
			case err := <-botDone:
				botFailed(err)
			}
		}
		close(alertChan)
	}
	if !botEnded {
		failure = errors.Join(failure, <-botDone)
	}
	discards.Wait()

	return failure
}

func runTransfer(action func() error) {
	path := filepath.Join(filepath.Dir(configPath), "transfer-error.txt")
	err := func() error {
		release, err := installation.AcquireTransferLock(filepath.Dir(configPath))
		if err != nil {
			return err
		}
		defer release()
		return action()
	}()
	if err != nil {
		os.WriteFile(path, []byte(err.Error()), 0600)
		os.Exit(1)
	}
	os.Remove(path)
}
