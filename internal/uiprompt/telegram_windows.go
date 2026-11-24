package uiprompt

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const tokenDialog = `
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
[Windows.Forms.Application]::EnableVisualStyles()
$form = New-Object Windows.Forms.Form
$form.Text = 'Poker Tracker - Connect Telegram'
$form.StartPosition = 'CenterScreen'
$form.ClientSize = '490,185'
$form.FormBorderStyle = 'FixedDialog'
$form.MaximizeBox = $false
$text = New-Object Windows.Forms.Label
$text.SetBounds(16,16,458,42)
$text.Text = "1. Open BotFather and send /newbot." + [Environment]::NewLine + "2. Follow its instructions and paste the token here."
$link = New-Object Windows.Forms.LinkLabel
$link.SetBounds(16,61,458,23)
$link.Text = 'Open BotFather in Telegram'
$link.Add_LinkClicked({ Start-Process 'https://t.me/BotFather' })
$token = New-Object Windows.Forms.TextBox
$token.SetBounds(16,93,458,25)
$token.UseSystemPasswordChar = $true
$connect = New-Object Windows.Forms.Button
$connect.Text = 'Connect'
$connect.SetBounds(302,139,82,28)
$connect.DialogResult = 'OK'
$cancel = New-Object Windows.Forms.Button
$cancel.Text = 'Close'
$cancel.SetBounds(392,139,82,28)
$cancel.DialogResult = 'Cancel'
$form.AcceptButton = $connect
$form.CancelButton = $cancel
$form.Controls.AddRange(@($text,$link,$token,$connect,$cancel))
if ($form.ShowDialog() -eq 'OK') { [Console]::Write($token.Text) }
`

func AskTelegramToken() (string, error) {
	cmd := exec.Command("powershell.exe", powerShellDialogArgs(tokenDialog)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	text, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("opening Telegram setup window: %w", err)
	}
	return strings.TrimSpace(string(text)), nil
}

func powerShellDialogArgs(script string) []string {
	units := utf16.Encode([]rune(script))
	data := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[i*2:], unit)
	}
	return []string{"-NoProfile", "-NonInteractive", "-STA", "-WindowStyle", "Hidden", "-EncodedCommand", base64.StdEncoding.EncodeToString(data)}
}

func TelegramSetupMessage(text string) error {
	message, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return err
	}
	title := windows.StringToUTF16Ptr("Poker Tracker - Connect Telegram")
	_, err = windows.MessageBox(0, message, title, windows.MB_OK|windows.MB_ICONINFORMATION)
	return err
}

func EnsureTelegramSetupPrivileges() (bool, error) {
	if windows.GetCurrentProcessToken().IsElevated() {
		return true, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	script := "$ErrorActionPreference='Stop'; $child=Start-Process -FilePath '" +
		strings.ReplaceAll(exe, "'", "''") +
		"' -ArgumentList '--connect-telegram' -Verb RunAs -PassThru; $child.WaitForExit(); exit $child.ExitCode"
	cmd := exec.Command("powershell.exe", powerShellDialogArgs(script)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("elevated Telegram setup did not finish: %w", err)
	}
	return false, nil
}

// CanPromptTelegram reports whether this process has an interactive session.
func CanPromptTelegram() bool {
	var session uint32
	return windows.ProcessIdToSessionId(uint32(os.Getpid()), &session) == nil && session != 0
}

func launchInDesktop(exe string, args ...string) error {
	var session uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &session); err != nil {
		return err
	}
	if session != 0 {
		cmd := exec.Command(exe, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		return err
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
	var active uint32
	for _, item := range unsafe.Slice(sessions, count) {
		if item.State == windows.WTSActive && item.SessionID != 0 {
			if active != 0 {
				return nil
			}
			active = item.SessionID
		}
	}
	if active == 0 {
		return nil
	}
	var user windows.Token
	if err := windows.WTSQueryUserToken(active, &user); err != nil {
		return err
	}
	defer user.Close()
	var env *uint16
	if err := windows.CreateEnvironmentBlock(&env, user, false); err != nil {
		return err
	}
	defer windows.DestroyEnvironmentBlock(env)
	command := windows.StringToUTF16Ptr(windows.ComposeCommandLine(append([]string{exe}, args...)))
	startup := windows.StartupInfo{Desktop: windows.StringToUTF16Ptr("winsta0\\default")}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	var proc windows.ProcessInformation
	if err := windows.CreateProcessAsUser(user, windows.StringToUTF16Ptr(exe), command, nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT, env, nil, &startup, &proc); err != nil {
		return err
	}
	windows.CloseHandle(proc.Thread)
	windows.CloseHandle(proc.Process)
	return nil
}

func ConfirmTelegramSetup(message string) (bool, error) {
	value, err := windows.UTF16PtrFromString(message)
	if err != nil {
		return false, err
	}
	title := windows.StringToUTF16Ptr("Poker Tracker - Connect Telegram")
	answer, err := windows.MessageBox(0, value, title, windows.MB_OKCANCEL|windows.MB_ICONINFORMATION)
	return answer == 1, err
}
