package uiprompt

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type Prompter struct {
	tool    string
	scanner *bufio.Scanner
}

func New() *Prompter {
	return &Prompter{
		tool:    detectTool(os.Environ(), exec.LookPath),
		scanner: bufio.NewScanner(os.Stdin),
	}
}

func (p *Prompter) Graphical() bool { return p.tool != "" }

func HasGraphicalSession() bool { return hasDisplay(os.Environ()) }

func (p *Prompter) Info(title, message string) error {
	switch p.tool {
	case "zenity":
		return exec.Command("zenity", "--info", "--no-wrap", "--title", title, "--text", message).Run()
	case "kdialog":
		return exec.Command("kdialog", "--title", title, "--msgbox", message).Run()
	default:
		fmt.Println(message)
		return nil
	}
}

func (p *Prompter) Ask(title, prompt string) (string, error) {
	switch p.tool {
	case "zenity":
		out, err := exec.Command("zenity", "--entry", "--title", title, "--text", prompt).Output()
		return strings.TrimSpace(string(out)), err
	case "kdialog":
		out, err := exec.Command("kdialog", "--title", title, "--inputbox", prompt).Output()
		return strings.TrimSpace(string(out)), err
	default:
		fmt.Print(prompt + " ")
		if !p.scanner.Scan() {
			return "", nil
		}
		return strings.TrimSpace(p.scanner.Text()), nil
	}
}

func (p *Prompter) Choose(title, prompt string, options []string) (string, error) {
	switch p.tool {
	case "zenity":
		args := []string{"--list", "--title", title, "--text", prompt, "--hide-header", "--column", "Option"}
		args = append(args, options...)
		out, err := exec.Command("zenity", args...).Output()
		return strings.TrimSpace(string(out)), err
	case "kdialog":
		args := []string{"--title", title, "--menu", prompt}
		for _, o := range options {
			args = append(args, o, o)
		}
		out, err := exec.Command("kdialog", args...).Output()
		return strings.TrimSpace(string(out)), err
	default:
		fmt.Println(prompt)
		for i, o := range options {
			fmt.Printf("  %d) %s\n", i+1, o)
		}
		fmt.Print("Enter a number: ")
		if !p.scanner.Scan() {
			return "", nil
		}
		idx, err := strconv.Atoi(strings.TrimSpace(p.scanner.Text()))
		if err != nil || idx < 1 || idx > len(options) {
			return "", fmt.Errorf("invalid choice %q", strings.TrimSpace(p.scanner.Text()))
		}
		return options[idx-1], nil
	}
}

func detectTool(environ []string, look func(string) (string, error)) string {
	if !hasDisplay(environ) {
		return ""
	}
	for _, tool := range []string{"zenity", "kdialog"} {
		if _, err := look(tool); err == nil {
			return tool
		}
	}
	return ""
}

func hasDisplay(environ []string) bool {
	return envValue(environ, "DISPLAY") != "" || envValue(environ, "WAYLAND_DISPLAY") != ""
}

func envValue(environ []string, key string) string {
	prefix := key + "="
	for _, kv := range environ {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}
