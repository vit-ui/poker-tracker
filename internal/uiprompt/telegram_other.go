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
		original, err := terminalState()
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

func terminalState() ([]byte, error) {
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = os.Stdin
	return cmd.Output()
}

func CanPromptTelegram() bool {
	if New().Graphical() {
		return true
	}
	_, err := terminalState()
	return err == nil
}

func TelegramSetupMessage(text string) error {
	return New().Info("Connect Telegram", text)
}

func EnsureTelegramSetupPrivileges() (bool, error) {
	return true, nil
}

func ConfirmTelegramSetup(message string) (bool, error) {
	answer, err := New().Choose("Connect Telegram", message, []string{"Continue", "Cancel"})
	return answer == "Continue", err
}
