package uiprompt

import (
	"os"
	"path/filepath"
	"strings"
)

func ShowUpdateProgress(executable, requestID string, telegram bool) error {
	literal := func(value string) string {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	dir := filepath.Dir(executable)
	allowSetup := "$false"
	if telegram {
		allowSetup = "$true"
	}
	script := "$executable = " + literal(executable) + "\n" +
		"$statePath = " + literal(filepath.Join(dir, "requested_update.json")) + "\n" +
		"$credentialPath = " + literal(filepath.Join(dir, "credentials", "telegram.token")) + "\n" +
		"$requestID = " + literal(requestID) + "\n" +
		"$allowSetup = " + allowSetup + "\n" + updateDialog
	powerShell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	return launchInDesktop(powerShell, powerShellDialogArgs(script)...)
}

const updateDialog = `
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
[Windows.Forms.Application]::EnableVisualStyles()
$form = New-Object Windows.Forms.Form
$form.Text = 'Poker Tracker Update'
$form.StartPosition = 'CenterScreen'
$form.ClientSize = '470,180'
$form.FormBorderStyle = 'FixedDialog'
$form.MaximizeBox = $false
$title = New-Object Windows.Forms.Label
$title.SetBounds(16,16,438,24)
$text = New-Object Windows.Forms.Label
$text.SetBounds(16,44,438,42)
$progress = New-Object Windows.Forms.ProgressBar
$progress.SetBounds(16,92,438,19)
$progress.Style = 'Marquee'
$progress.MarqueeAnimationSpeed = 25
$connect = New-Object Windows.Forms.Button
$connect.Text = 'Connect Telegram'
$connect.SetBounds(238,133,126,28)
$connect.Visible = $false
$connect.Add_Click({ Start-Process -FilePath $executable -ArgumentList '--connect-telegram' })
$close = New-Object Windows.Forms.Button
$close.Text = 'Close'
$close.SetBounds(372,133,82,28)
$close.Add_Click({ $form.Close() })
$form.CancelButton = $close
$form.Controls.AddRange(@($title,$text,$progress,$connect,$close))

$refresh = {
    $job = $null
    try {
        if (Test-Path -LiteralPath $statePath) {
            $job = Get-Content -Raw -LiteralPath $statePath | ConvertFrom-Json
        }
    } catch { return }
    if ($requestID -and $job.request.id -ne $requestID) { return }
    $pendingSetup = $allowSetup -and !(Test-Path -LiteralPath $credentialPath)
    $connect.Visible = $pendingSetup -and (!$job -or $job.state -eq 'succeeded')
    $progress.Visible = $job -and $job.state -ne 'succeeded' -and $job.state -ne 'failed'
    $title.Text = if ($job) { 'Poker Tracker ' + $job.request.version } else { 'Telegram setup' }
    $text.Text = switch ($job.state) {
        'queued' { 'The update is queued.' }
        'waiting_for_pokerstars' { 'Close PokerStars to continue the update.' }
        'running' {
            if ($job.stage -eq 'download') { 'Downloading and verifying the installer.' }
            else { 'Installing the update and checking that Tracker restarts.' }
        }
        'succeeded' {
            if ($pendingSetup) { 'Tracker is updated. Connect your Telegram bot to finish.' }
            else { 'Tracker is updated and ready.' }
        }
        'failed' { 'The update failed. Check the Tracker log for details.' }
        default {
            if ($pendingSetup) { 'Connect your own Telegram bot to control Tracker from Telegram.' }
            else { 'Telegram is connected.' }
        }
    }
}
$timer = New-Object Windows.Forms.Timer
$timer.Interval = 500
$timer.Add_Tick($refresh)
$form.Add_Shown({ & $refresh; $timer.Start() })
$form.Add_FormClosed({ $timer.Dispose() })
[void]$form.ShowDialog()
`
