//go:build !windows

package uiprompt

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func AskTelegramToken() (string, error) {
	const message = "Open https://t.me/BotFather, send /newbot, and paste the token BotFather gives you."
	p := New()
	switch p.tool {
	case "zenity":
		out, err := exec.Command("zenity", "--entry", "--hide-text", "--title", "Connect Telegram", "--text", message).Output()
		return strings.TrimSpace(string(out)), err
	case "kdialog":
		out, err := exec.Command("kdialog", "--title", "Connect Telegram", "--password", message).Output()
		return strings.TrimSpace(string(out)), err
	default:
		fmt.Println(message)
		state := exec.Command("stty", "-g")
		state.Stdin = os.Stdin
		original, err := state.Output()
		if err != nil {
			return "", fmt.Errorf("open a terminal to enter the bot token privately")
		}
		tty := exec.Command("stty", "-echo")
		tty.Stdin = os.Stdin
		if err := tty.Run(); err != nil {
			return "", fmt.Errorf("open a terminal to enter the bot token privately")
		}
		defer func() {
			tty := exec.Command("stty", strings.TrimSpace(string(original)))
			tty.Stdin = os.Stdin
			_ = tty.Run()
			fmt.Println()
		}()
		return p.Ask("Connect Telegram", "Bot token:")
	}
}

func TelegramSetupMessage(text string) error {
	return New().Info("Connect Telegram", text)
}

func ShowPendingTelegramSetup() error {
	// Background systemd services cannot own an interactive terminal. A
	// terminal or desktop launch uses the same setup command explicitly.
	return nil
}

func EnsureTelegramSetupPrivileges() (bool, error) {
	return true, nil
}
