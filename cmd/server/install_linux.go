//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vit-ui/self-tracker/internal/installation"
	"github.com/vit-ui/self-tracker/internal/uiprompt"
)

func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating executable: %w", err)
	}
	unitDir, err := userUnitDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(unitDir, 0755); err != nil {
		return fmt.Errorf("creating %s: %w", unitDir, err)
	}
	serviceName := installation.ServerServiceName(InstallationID)
	unitPath := filepath.Join(unitDir, serviceName)
	if err := os.WriteFile(unitPath, []byte(serverUnit(exe)), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", serviceName},
		{"--user", "restart", serviceName},
	} {
		cmd := exec.Command("systemctl", args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("systemctl %v: %w", args, err)
		}
	}
	enableLinger()
	return nil
}

func serverUnit(exe string) string {
	unitArg := func(path string) string {
		return strconv.Quote(strings.ReplaceAll(path, "%", "%%"))
	}
	return fmt.Sprintf(`[Unit]
Description=Poker Tracker server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=:/bin/sh -c 'cd "$1" && exec "$2" --serve' tracker-server %s %s
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`, unitArg(filepath.Dir(exe)), unitArg(exe))
}

func userUnitDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func enableLinger() {
	who := currentUser()
	loginctl, lerr := exec.LookPath("loginctl")
	manual := fmt.Sprintf("  sudo loginctl enable-linger %s", who)
	if who == "" || lerr != nil {
		fmt.Println("Could not enable start-on-boot automatically. To start the server on boot without logging in, run:")
		fmt.Println(manual)
		return
	}

	runner := lingerRunner()
	if runner == "" {
		fmt.Println("Could not find a way to elevate for start-on-boot. To enable it later, run:")
		fmt.Println(manual)
		return
	}

	cmd := exec.Command(runner, loginctl, "enable-linger", who)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Println("Start-on-boot was not enabled (you can skip this). To enable it later, run:")
		fmt.Println(manual)
		return
	}
	fmt.Println("Start-on-boot enabled: the server will come up automatically after a reboot.")
}

func lingerRunner() string {
	if uiprompt.HasGraphicalSession() {
		if _, err := exec.LookPath("pkexec"); err == nil {
			return "pkexec"
		}
	}
	if _, err := exec.LookPath("sudo"); err == nil {
		return "sudo"
	}
	return ""
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
