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

	"github.com/vit-ui/self-tracker/internal/backend"
	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/server"
	"github.com/vit-ui/self-tracker/internal/transport"
)

// BotToken is injected at build time via -ldflags "-X main.BotToken=...", the
// same as cmd/tracker. Only the server talks to Telegram, so only the server
// (and combined) build carries the token.
var BotToken string

// listenPort is the fixed TCP port the server listens on and advertises in the
// connection string. Fixed rather than configurable to keep pairing simple.
const listenPort = 8443

var (
	serviceAccountPath string
	configPath         string
	lockfilePath       string
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
	lockfilePath = filepath.Join(dir, "lockfile.json")
	dbPath = filepath.Join(dir, "tracker.db")
	fallbackPath = filepath.Join(dir, "sheets_fallback.json")
	certPath = filepath.Join(dir, "server_cert.pem")
	keyPath = filepath.Join(dir, "server_key.pem")
}

func main() {
	serve := flag.Bool("serve", false, "run the server")
	setup := flag.Bool("setup", false, "generate credentials and print the connection string")
	pair := flag.Bool("pair", false, "reprint the connection string to pair another collector")
	install := flag.Bool("install", false, "register the server as a boot service (systemd on Linux, Task Scheduler on Windows)")
	flag.Parse()

	switch {
	case *serve:
		runServe()
	case *setup:
		runSetup()
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
	fmt.Println("Installed as a service — it will start on boot and is starting now.")
}

func runServe() {
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
	go srv.Run()

	// A hard-stop decision (manual via the bot, or automatic in the server) is
	// sent down the link to the collector, which kills PokerStars and writes the
	// lockfile on the poker PC.
	sendLock := func(lock config.LockFile) {
		link.Commands <- transport.Command{Kind: transport.CommandLock, Lock: &lock}
	}

	go func() {
		// No watcher on the server (the collector watches), so /paths' watcher
		// restart is a no-op here. The bot's read-side date windows anchor to
		// the server's client-anchored clock, so /status and /stats follow the
		// player's local day.
		err := bot.Run(BotToken, alertChan, sc, configPath, lockfilePath, serviceAccountPath, dbPath, func() {}, sendLock, srv.ClientNow)
		slog.Error("bot.Run returned — the bot is no longer listening", "err", err)
	}()

	select {}
}

func runSetup() {
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
	if cfg.Setup.StorageBackend == nil {
		sqlite := "sqlite"
		cfg.Setup.StorageBackend = &sqlite // sensible server default; switch via /storage
	}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "saving config:", err)
		os.Exit(1)
	}
	printConnectionString(cfg)
}

func runPair() {
	cfg, err := config.LoadConfig(configPath)
	if err != nil || cfg.Setup.PairToken == nil {
		fmt.Fprintln(os.Stderr, "server is not set up — run: tracker-server --setup")
		os.Exit(1)
	}
	printConnectionString(cfg)
}

func printConnectionString(cfg *config.Config) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading certificate:", err)
		os.Exit(1)
	}
	fp, err := transport.FingerprintPEM(certPEM)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading certificate:", err)
		os.Exit(1)
	}
	cs := transport.ConnectionString(localIP(), listenPort, *cfg.Setup.PairToken, fp)
	fmt.Println("This is the tracker server. It stores your data, runs the Telegram bot,")
	fmt.Println("and enforces your rules. Keep it running; your poker PC connects to it.")
	fmt.Println("Configure storage/alerts/thresholds from Telegram once your poker PC is paired.")
	fmt.Println()
	fmt.Println("Pair your poker PC by pasting this line into its installer")
	fmt.Println("(replace the address if the PC is on a different network than this server):")
	fmt.Println()
	fmt.Println("  " + cs)
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
