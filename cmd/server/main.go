// Command tracker-server is the brain half of the split build: it stores data,
// runs the Telegram bot, evaluates the rules, and listens for a collector (the
// poker PC) to connect over TLS. It never watches PokerStars files or kills
// PokerStars — the collector does that. The all-local build is cmd/tracker;
// this binary is only for running the server on a separate always-on machine.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/vit-ui/self-tracker/internal/backend"
	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/installation"
	"github.com/vit-ui/self-tracker/internal/logging"
	"github.com/vit-ui/self-tracker/internal/server"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/uiprompt"
)

// BotToken is injected at build time via -ldflags "-X main.BotToken=...", the
// same as cmd/tracker. Only the server talks to Telegram, so only the server
// (and combined) build carries the token.
var BotToken string

// InstallationID is injected into disposable builds; production leaves it empty.
var InstallationID string

// listenPort is the fixed TCP port the server listens on and advertises in the
// connection string. Fixed rather than configurable to keep pairing simple.
const listenPort = 8443

var (
	serviceAccountPath string
	configPath         string
	dbPath             string
	fallbackPath       string
	certPath           string
	keyPath            string
)

func init() {
	exe, err := os.Executable()
	if err != nil {
		panic("getting executable path: " + err.Error())
	}
	dir := filepath.Dir(exe)
	serviceAccountPath = filepath.Join(dir, "credentials", "google_service_account.json")
	configPath = filepath.Join(dir, "config.json")
	dbPath = filepath.Join(dir, "tracker.db")
	fallbackPath = filepath.Join(dir, "sheets_fallback.json")
	certPath = filepath.Join(dir, "server_cert.pem")
	keyPath = filepath.Join(dir, "server_key.pem")
}

func main() {
	stopInstallation := flag.String("stop-installation", "", "stop tracker processes in the specified installation directory")
	serve := flag.Bool("serve", false, "run the server")
	setup := flag.Bool("setup", false, "generate credentials and print the connection string")
	pair := flag.Bool("pair", false, "reprint the connection string to pair another collector")
	install := flag.Bool("install", false, "register the server as a boot service (systemd on Linux, Task Scheduler on Windows)")
	// The Windows installer collects the storage choice on its own wizard pages
	// and passes it here, since it runs --setup in a hidden console that can't
	// prompt. Empty --backend means ask (Linux desktop/terminal) or keep an
	// existing/default backend. --sheet-name/--sheet-id preseed a Sheets install.
	backend := flag.String("backend", "", `storage backend to preseed on --setup: "sqlite" or "sheets"`)
	sheetName := flag.String("sheet-name", "", "Google Sheet name to preseed on --setup (sheets backend)")
	sheetID := flag.String("sheet-id", "", "Google Sheet ID to preseed on --setup (sheets backend)")
	flag.Parse()

	switch {
	case *stopInstallation != "":
		if err := installation.Stop(*stopInstallation); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case *serve:
		runServe()
	case *setup:
		runSetup(*backend, *sheetName, *sheetID)
	case *pair:
		runPair()
	case *install:
		runInstall()
	default:
		fmt.Fprintln(os.Stderr, "usage: tracker-server --serve | --setup | --install | --pair")
		os.Exit(2)
	}
}

// runInstall registers the server as a boot service via the per-OS
// installService, so it starts on boot and restarts on failure.
func runInstall() {
	if err := installService(); err != nil {
		fmt.Fprintln(os.Stderr, "install failed:", err)
		os.Exit(1)
	}
	fmt.Println("Installed as a service — it is running now.")
}

func runServe() {
	closeLog := logging.Configure(filepath.Join(filepath.Dir(configPath), "tracker.log"))
	defer closeLog()
	// Keep the idle footprint small without starving the read spikes. GOGC=50
	// (vs the default 100) halves the heap growth ratio, so the idle heap floor
	// stays low and freed pages are scavenged back to the OS sooner — this is
	// the lever that matters, since the server is idle almost all the time.
	//
	// The soft memory limit is deliberately generous (not the combined build's
	// 32 MiB): unlike the poker PC, the server is the query endpoint — /export
	// and /stats materialize whole storage tabs — and a limit sized near that
	// working set would make the GC burn CPU and overshoot anyway under a big
	// read, on a machine where memory isn't the scarce resource. 128 MiB is far
	// above any single user's read spike yet still caps a runaway. (On Linux the
	// systemd unit could set GOMEMLIMIT/GOGC or a cgroup cap instead; the
	// Windows server has no such hook, so set a portable default in code.) These
	// are runtime knobs only — stored data, alerts, and enforcement are
	// unaffected.
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(128 << 20)

	sc, backendName, err := backend.Open(BotToken, configPath, serviceAccountPath, dbPath, fallbackPath)
	if err != nil {
		slog.Error("opening storage backend", "err", err)
		os.Exit(1)
	}

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	cfg, cfgErr := config.LoadConfig(configPath)
	if certErr != nil || keyErr != nil || cfgErr != nil || cfg.Setup.PairToken == nil {
		slog.Error("server is not set up — run: tracker-server --setup")
		os.Exit(1)
	}
	token := *cfg.Setup.PairToken

	slog.Info("tracker server starting", "backend", backendName, "port", listenPort)

	link := transport.NewLink()
	ln, err := transport.Listen(fmt.Sprintf(":%d", listenPort), certPEM, keyPEM)
	if err != nil {
		slog.Error("listening for the collector", "err", err)
		os.Exit(1)
	}
	go transport.Serve(ln, token, link)

	alertChan := make(chan string, 20)
	srv, err := server.NewServer(sc, configPath, link, alertChan)
	if err != nil {
		slog.Error("creating server", "err", err)
		os.Exit(1)
	}
	go func() { err := srv.Run(); slog.Error("server worker stopped", "err", err); os.Exit(1) }()

	// A hard-stop decision (manual via the bot, or automatic in the server) is
	// sent down the link to the collector, which kills PokerStars and writes the
	// lockfile on the poker PC.

	// A /scan backfill sends a rescan command down the link; the remote
	// collector walks the poker PC's folders and uploads them for a silent
	// import.
	sendLock := func(lock config.LockFile) {
		link.Commands <- transport.Command{Kind: transport.CommandLock, Lock: &lock}
	}
	requestRescan := func() {
		link.Commands <- transport.Command{Kind: transport.CommandRescan}
	}

	go func() {
		// No watcher on the server (the collector watches), so /paths' watcher
		// restart is a no-op here. The bot's read-side date windows anchor to
		// the server's client-anchored clock, so /status and /stats follow the
		// player's local day.
		err := bot.Run(BotToken, alertChan, sc, configPath, serviceAccountPath, dbPath, func() {}, sendLock, requestRescan, srv.ClientNow, installation.ServerServiceName(InstallationID), srv.Operations, srv.CollectorLock)
		slog.Error("bot.Run returned — the bot is no longer listening", "err", err)
		os.Exit(1)
	}()

	select {}
}

func runSetup(backend, sheetName, sheetID string) {
	p := uiprompt.New()

	// Generate the TLS cert/key once and keep them; the fingerprint the client
	// pins is derived from the cert, so it must not change across restarts.
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		certPEM, keyPEM, _, err := transport.GenerateServerCert()
		if err != nil {
			fmt.Fprintln(os.Stderr, "generating certificate:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
			fmt.Fprintln(os.Stderr, "writing certificate:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
			fmt.Fprintln(os.Stderr, "writing key:", err)
			os.Exit(1)
		}
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loading config:", err)
		os.Exit(1)
	}
	if cfg.Setup.PairToken == nil {
		token := randomToken()
		cfg.Setup.PairToken = &token
	}
	applyStorageChoice(cfg, p, backend, sheetName, sheetID)
	if err := config.SaveConfig(configPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "saving config:", err)
		os.Exit(1)
	}
	showConnection(p, cfg)
}

// applyStorageChoice sets the storage backend for a new install. The Windows
// installer collects it on its own wizard pages (like the all-local install)
// and passes it as flags; on Linux with no flag it's asked via chooseBackend
// (desktop dialog or terminal), defaulting SQLite. An install that already has a
// backend keeps it — LoadConfig read the existing config, so a re-run preserves
// the backend and the pairing token rather than resetting them.
func applyStorageChoice(cfg *config.Config, p *uiprompt.Prompter, backend, sheetName, sheetID string) {
	if backend == "sqlite" || backend == "sheets" {
		b := backend
		cfg.Setup.StorageBackend = &b
		if backend == "sheets" {
			if sheetName != "" {
				n := sheetName
				cfg.Setup.GoogleSheetName = &n
			}
			if sheetID != "" {
				id := sheetID
				cfg.Setup.GoogleSheetID = &id
			}
		}
		return
	}
	if cfg.Setup.StorageBackend == nil {
		cfg.Setup.StorageBackend = chooseBackend(p)
	}
}

// chooseBackend asks how the player wants to configure the server. Everything
// (storage, alerts, rules) can be set from Telegram once paired, so that's the
// recommended default and lands on SQLite (switchable anytime via /storage). If
// they'd rather set storage here, they pick SQLite or Google Sheets; Sheets is
// finished from Telegram since it needs the sheet shared and the key uploaded.
func chooseBackend(p *uiprompt.Prompter) *string {
	sqlite, sheets := "sqlite", "sheets"
	// Skip the prompt only on Windows: there the server is set up by the Inno
	// installer, which runs this in a hidden console with no interactive stdin,
	// so a terminal-mode read could block the installer — default to SQLite
	// silently (switchable anytime from Telegram via /storage). On Linux both
	// the desktop dialog and a real terminal (SSH) can answer safely, and a
	// non-interactive stdin (double-click with no terminal) just reads EOF and
	// falls through to the default — so the window and terminal ask identically.
	if runtime.GOOS == "windows" {
		return &sqlite
	}
	mode, err := p.Choose("Poker Tracker server setup",
		"How do you want to configure storage, alerts, and rules?",
		[]string{"From Telegram on my phone (recommended)", "Set storage here now"})
	if err != nil || !strings.Contains(mode, "here") {
		return &sqlite
	}
	pick, err := p.Choose("Storage backend", "Where should your poker data live?",
		[]string{"A local file on this server (SQLite)", "Google Sheets"})
	if err == nil && strings.Contains(pick, "Sheets") {
		p.Info("Google Sheets",
			"Storage set to Google Sheets. Finish connecting the sheet from Telegram: "+
				"message your bot and send /storage — it walks you through sharing the "+
				"sheet with the service account and uploading the key.")
		return &sheets
	}
	return &sqlite
}

func runPair() {
	cfg, err := config.LoadConfig(configPath)
	if err != nil || cfg.Setup.PairToken == nil {
		fmt.Fprintln(os.Stderr, "server is not set up — run: tracker-server --setup")
		os.Exit(1)
	}
	showConnection(uiprompt.New(), cfg)
}

// showConnection surfaces the pairing connection string: always to stdout (so a
// terminal/log run never loses it), plus a dialog window on a graphical desktop.
func showConnection(p *uiprompt.Prompter, cfg *config.Config) {
	msg, err := connectionMessage(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(msg)
	if p.Graphical() {
		p.Info("Poker Tracker server", msg)
	}
}

// connectionMessage builds the explainer + the tracker:// connection string a
// poker PC pastes into its installer to pair with this server.
func connectionMessage(cfg *config.Config) (string, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return "", fmt.Errorf("reading certificate: %w", err)
	}
	fp, err := transport.FingerprintPEM(certPEM)
	if err != nil {
		return "", fmt.Errorf("reading certificate: %w", err)
	}
	cs := transport.ConnectionString(localIP(), listenPort, *cfg.Setup.PairToken, fp)
	return strings.Join([]string{
		"This is the tracker server. It stores your data, runs the Telegram bot,",
		"and enforces your rules. Keep it running; your poker PC connects to it.",
		"Configure storage/alerts/thresholds from Telegram once your poker PC is paired.",
		"",
		"Pair your poker PC by pasting this line into its installer",
		"(replace the address if the PC is on a different network than this server):",
		"",
		"  " + cs,
	}, "\n"), nil
}

// randomToken returns a 128-bit hex token for the pairing secret.
func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("generating token: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// localIP returns this machine's first non-loopback IPv4 address, a reasonable
// default for a server and collector on the same LAN. For a VPS the user
// replaces it with the public address in the printed connection string.
func localIP() string {
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					return ip4.String()
				}
			}
		}
	}
	return "127.0.0.1"
}
