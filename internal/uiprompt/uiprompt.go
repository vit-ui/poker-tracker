// Package uiprompt is a small setup front-end. When a graphical dialog tool and
// a display are available (a Linux desktop), it collects input through zenity or
// kdialog so the server/client --setup shows a window like the Windows installer
// does; otherwise — a headless SSH box, or Windows, where either the terminal or
// the Inno wizard drives setup — it falls back to plain stdin/stdout prompts. It
// shells out to the dialog tools rather than taking a GUI dependency, the same
// approach the rest of the program uses for taskkill/schtasks/systemctl.
package uiprompt

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Prompter renders setup questions either as GUI dialogs or as terminal prompts,
// decided once at construction from the environment.
type Prompter struct {
	tool    string // "zenity", "kdialog", or "" for the terminal fallback
	scanner *bufio.Scanner
}

// New picks the dialog tool from the current environment (a graphical desktop
// with zenity or kdialog installed) and falls back to the terminal otherwise.
func New() *Prompter {
	return &Prompter{
		tool:    detectTool(os.Environ(), exec.LookPath),
		scanner: bufio.NewScanner(os.Stdin),
	}
}

// Graphical reports whether prompts render as GUI dialogs (true) or as terminal
// prompts (false), so callers can word follow-up instructions to match.
func (p *Prompter) Graphical() bool { return p.tool != "" }

// HasGraphicalSession reports whether the current process has a graphical
// desktop session (an X11 or Wayland display), the shared signal for "prefer a
// window / a graphical password prompt over a terminal one".
func HasGraphicalSession() bool { return hasDisplay(os.Environ()) }

// Info shows a message and waits for the user to acknowledge it.
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

// Ask prompts for one line of text and returns it trimmed.
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

// Choose presents a single-choice list and returns the chosen option's text. The
// first option is the default. The terminal fallback reads a 1-based number.
func (p *Prompter) Choose(title, prompt string, options []string) (string, error) {
	switch p.tool {
	case "zenity":
		args := []string{"--list", "--radiolist", "--title", title, "--text", prompt, "--hide-header", "--column", "", "--column", "Option"}
		for i, o := range options {
			state := "FALSE"
			if i == 0 {
				state = "TRUE"
			}
			args = append(args, state, o)
		}
		out, err := exec.Command("zenity", args...).Output()
		return strings.TrimSpace(string(out)), err
	case "kdialog":
		args := []string{"--title", title, "--radiolist", prompt}
		for i, o := range options {
			state := "off"
			if i == 0 {
				state = "on"
			}
			args = append(args, o, o, state)
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

// detectTool returns the graphical dialog tool to use, or "" for the terminal
// fallback: it needs both a display (an X11 or Wayland session) and one of the
// tools on PATH. environ is os.Environ()-shaped; look is exec.LookPath.
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

// hasDisplay reports whether a graphical session is present, i.e. DISPLAY (X11)
// or WAYLAND_DISPLAY is set to a non-empty value. A headless SSH box has neither.
func hasDisplay(environ []string) bool {
	return envValue(environ, "DISPLAY") != "" || envValue(environ, "WAYLAND_DISPLAY") != ""
}

// envValue returns the value of key in an os.Environ()-shaped slice, or "".
func envValue(environ []string, key string) string {
	prefix := key + "="
	for _, kv := range environ {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}
