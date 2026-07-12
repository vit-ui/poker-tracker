; Poker Tracker installer. Compiled by .github/workflows/release.yml, which
; runs iscc with /DAppVersion=<tag, "v" stripped> — see the #ifndef fallback
; below for local/manual compiles that don't pass that define.
#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif

[Setup]
AppName=Poker Tracker
AppVersion={#AppVersion}
DefaultDirName=C:\poker-tracker
DisableDirPage=yes
DefaultGroupName=Poker Tracker
DisableProgramGroupPage=yes
DisableWelcomePage=no
PrivilegesRequired=admin
OutputBaseFilename=Setup
Compression=lzma
SolidCompression=yes

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Files]
Source: "tracker.exe"; DestDir: "{app}"; Flags: ignoreversion
; Second copy, flagged dontcopy, exists only so ExtractTemporaryFile can pull
; tracker.exe out to {tmp} and run it *during* the wizard (Telegram setup) —
; before the real install step below has copied anything to {app} yet.
Source: "tracker.exe"; DestDir: "{tmp}"; Flags: dontcopy

[UninstallDelete]
; tracker.exe itself is removed automatically (tracked via [Files] above).
; Everything else here was written by [Code] or by the running service at
; runtime, so Inno doesn't know about it unless listed — including
; config.json, lockfile.json, and pending_tournaments.json (all three are
; config.go's responsibility per CLAUDE.md, same directory). This is a full
; uninstall: nothing is left behind for a reinstall to pick back up. {app}
; itself is removed last, once it's empty.
Type: files; Name: "{app}\credentials\google_service_account.json"
Type: files; Name: "{app}\config.json"
Type: files; Name: "{app}\lockfile.json"
Type: files; Name: "{app}\pending_tournaments.json"
Type: dirifempty; Name: "{app}\credentials"
Type: dirifempty; Name: "{app}"

[UninstallRun]
; Must run before file deletion below, or a running --serve process holds
; tracker.exe open and the uninstaller can't remove it. [UninstallRun]
; entries execute before files are deleted by default, and top-to-bottom
; within the section, so this only needs to be listed first.
Filename: "taskkill.exe"; Parameters: "/IM tracker.exe /F"; Flags: runhidden; RunOnceId: "KillTracker"
Filename: "schtasks.exe"; Parameters: "/Delete /TN ""Poker Tracker"" /F"; Flags: runhidden; RunOnceId: "DeleteTask"

[Code]
var
  PageMethod: TInputOptionWizardPage;
  PageCredentials: TInputFileWizardPage;
  PageWaiting: TOutputMsgWizardPage;
  PageSheetName: TInputQueryWizardPage;
  PageScreenName: TInputQueryWizardPage;
  PageHandHistoryDir: TInputDirWizardPage;
  PageTournamentDir: TInputDirWizardPage;
  UsingTelegram: Boolean;

procedure InitializeWizard();
begin
  PageMethod := CreateInputOptionPage(wpWelcome,
    'Setup Method',
    'Would you like to set up via Telegram? (Recommended)',
    '',
    True, False);
  PageMethod.Add('Yes, use Telegram');
  PageMethod.Add('No, set up manually');
  PageMethod.SelectedValueIndex := 0;

  PageCredentials := CreateInputFilePage(PageMethod.ID,
    'Google Service Account',
    'Locate your service account credentials file',
    'Poker Tracker needs this file to read and write your Google Sheet — required either way, Telegram or manual setup.');
  PageCredentials.Add('Select your google_service_account.json file', 'JSON files|*.json|All files|*.*', '.json');

  PageWaiting := CreateOutputMsgPage(PageCredentials.ID,
    'Telegram Setup',
    'Complete setup in Telegram',
    'Complete setup in Telegram. Send any message to your bot to begin. The installer will continue automatically when done.');

  PageSheetName := CreateInputQueryPage(PageWaiting.ID,
    'Google Sheet',
    'Enter your Google Sheet name',
    'Create a blank sheet and share it with the service account before continuing.');
  PageSheetName.Add('Google Sheet name:', False);

  PageScreenName := CreateInputQueryPage(PageSheetName.ID,
    'PokerStars Screen Name',
    'Enter your PokerStars screen name',
    'Exact capitalisation matters.');
  PageScreenName.Add('PokerStars screen name:', False);

  PageHandHistoryDir := CreateInputDirPage(PageScreenName.ID,
    'Hand History Folder',
    'Select the base Hand History folder',
    'Select the parent HandHistory folder — do NOT browse into your screen name''s subfolder underneath it, that part is added automatically.',
    False, '');
  PageHandHistoryDir.Add('Select the parent HandHistory folder — do NOT browse into your screen name''s subfolder underneath it, that part is added automatically:');

  PageTournamentDir := CreateInputDirPage(PageHandHistoryDir.ID,
    'Tournament Summary Folder',
    'Select the base TournamentSummaries folder',
    'Select the parent TournamentSummaries folder — do NOT browse into your screen name''s subfolder underneath it, that part is added automatically.',
    False, '');
  PageTournamentDir.Add('Select the parent TournamentSummaries folder — do NOT browse into your screen name''s subfolder underneath it, that part is added automatically:');
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := False;
  if UsingTelegram then
  begin
    if (PageID = PageSheetName.ID) or (PageID = PageScreenName.ID) or
       (PageID = PageHandHistoryDir.ID) or (PageID = PageTournamentDir.ID) then
      Result := True;
  end
  else
  begin
    if PageID = PageWaiting.ID then
      Result := True;
  end;
end;

// JSONEscape escapes backslashes and double quotes so Windows paths and
// free-text fields (sheet name, screen name) can be embedded safely as
// JSON string values.
function JSONEscape(const S: String): String;
var
  R: String;
begin
  R := S;
  StringChangeEx(R, '\', '\\', True);
  StringChangeEx(R, '"', '\"', True);
  Result := R;
end;

// BuildConfigJSON mirrors config.Config's JSON shape exactly (see
// internal/config/config.go) — only the four fields collected by manual
// setup are non-null; everything else (chat ID, sheet ID, alerts, hard
// stop, schedule) stays null, configured later via the bot or self-healed
// by bot.EnsureSheetID on first --serve launch.
function BuildConfigJSON(const SheetName, ScreenName, HandHistoryPath, TournamentPath: String): String;
begin
  Result :=
    '{' + #13#10 +
    '  "setup": {' + #13#10 +
    '    "telegram_chat_id": null,' + #13#10 +
    '    "google_sheet_id": null,' + #13#10 +
    '    "google_sheet_name": "' + JSONEscape(SheetName) + '",' + #13#10 +
    '    "pokerstars_screen_name": "' + JSONEscape(ScreenName) + '"' + #13#10 +
    '  },' + #13#10 +
    '  "paths": {' + #13#10 +
    '    "hand_history": "' + JSONEscape(HandHistoryPath) + '",' + #13#10 +
    '    "tournament_summary": "' + JSONEscape(TournamentPath) + '"' + #13#10 +
    '  },' + #13#10 +
    '  "alerts": {' + #13#10 +
    '    "downswing_buyins": null,' + #13#10 +
    '    "losing_streak_count": null,' + #13#10 +
    '    "daily_loss_usd": null,' + #13#10 +
    '    "spingo_daily_loss_usd": null,' + #13#10 +
    '    "session_loss_buyins": null' + #13#10 +
    '  },' + #13#10 +
    '  "hardstop": {' + #13#10 +
    '    "session_loss_buyins": null,' + #13#10 +
    '    "consecutive_losses": null,' + #13#10 +
    '    "daily_loss_usd": null,' + #13#10 +
    '    "spingo_daily_loss_usd": null,' + #13#10 +
    '    "duration_hours": null,' + #13#10 +
    '    "downswing_buyins": null' + #13#10 +
    '  },' + #13#10 +
    '  "schedule": {' + #13#10 +
    '    "day_start_hour": null' + #13#10 +
    '  }' + #13#10 +
    '}' + #13#10;
end;

// UTF16Bytes converts S to raw UTF-16LE bytes with a leading byte-order
// mark, packed into an AnsiString (one Pascal Char per byte). Every value
// written (BOM bytes, low/high byte of each UTF-16 code unit) is within
// 0-255, so this round-trips through AnsiString without lossy conversion.
// schtasks /Create /XML rejects a plain UTF-8/ANSI file — it requires this
// encoding, same as bot.utf16File in the Go codebase (main.go).
function UTF16Bytes(const S: String): AnsiString;
var
  I: Integer;
  C: Longint;
  Buf: AnsiString;
begin
  Buf := Chr($FF) + Chr($FE);
  for I := 1 to Length(S) do
  begin
    C := Ord(S[I]);
    Buf := Buf + Chr(C and $FF) + Chr((C shr 8) and $FF);
  end;
  Result := Buf;
end;

// BuildTaskXML mirrors taskSchedulerXML in cmd/tracker/main.go exactly, so
// a task registered by manual setup behaves identically to one registered
// by tracker.exe --setup (Telegram path) — same battery/time-limit/
// multiple-instance settings either way. WorkingDirectory matters less
// than it used to now that tracker.exe resolves its own deployment paths
// from its executable's location rather than cwd (see main.go) — but a
// real Task Scheduler bug was caused by exactly this being missing
// (defaults to %SystemRoot%\System32, silently pointing relative paths at
// the wrong place), so it's set here too rather than relying solely on
// that fix.
function BuildTaskXML(const ExePath, WorkingDir: String): String;
begin
  Result :=
    '<?xml version="1.0" encoding="UTF-16"?>' + #13#10 +
    '<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">' + #13#10 +
    '  <Triggers>' + #13#10 +
    '    <LogonTrigger>' + #13#10 +
    '      <Enabled>true</Enabled>' + #13#10 +
    '    </LogonTrigger>' + #13#10 +
    '  </Triggers>' + #13#10 +
    '  <Principals>' + #13#10 +
    '    <Principal id="Author">' + #13#10 +
    '      <GroupId>S-1-5-32-544</GroupId>' + #13#10 +
    '      <RunLevel>HighestAvailable</RunLevel>' + #13#10 +
    '    </Principal>' + #13#10 +
    '  </Principals>' + #13#10 +
    '  <Settings>' + #13#10 +
    '    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>' + #13#10 +
    '    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>' + #13#10 +
    '    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>' + #13#10 +
    '    <AllowHardTerminate>true</AllowHardTerminate>' + #13#10 +
    '    <StartWhenAvailable>false</StartWhenAvailable>' + #13#10 +
    '    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>' + #13#10 +
    '    <IdleSettings>' + #13#10 +
    '      <StopOnIdleEnd>true</StopOnIdleEnd>' + #13#10 +
    '      <RestartOnIdle>false</RestartOnIdle>' + #13#10 +
    '    </IdleSettings>' + #13#10 +
    '    <AllowStartOnDemand>true</AllowStartOnDemand>' + #13#10 +
    '    <Enabled>true</Enabled>' + #13#10 +
    '    <Hidden>false</Hidden>' + #13#10 +
    '    <RunOnlyIfIdle>false</RunOnlyIfIdle>' + #13#10 +
    '    <WakeToRun>false</WakeToRun>' + #13#10 +
    '    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>' + #13#10 +
    '    <Priority>7</Priority>' + #13#10 +
    '  </Settings>' + #13#10 +
    '  <Actions Context="Author">' + #13#10 +
    '    <Exec>' + #13#10 +
    '      <Command>' + ExePath + '</Command>' + #13#10 +
    '      <Arguments>--serve</Arguments>' + #13#10 +
    '      <WorkingDirectory>' + WorkingDir + '</WorkingDirectory>' + #13#10 +
    '    </Exec>' + #13#10 +
    '  </Actions>' + #13#10 +
    '</Task>';
end;

// PreInstallFiles creates {app} and writes the credentials copy — needed
// before tracker.exe --setup can run (Telegram path, called mid-wizard)
// and again, harmlessly idempotent, during the real install step (both
// paths). tracker.exe resolves its three deployment paths (service
// account, config, lockfile) relative to its own executable location at
// runtime — see cmd/tracker/main.go — so there's no .env to write here
// anymore, and no dependency on this process's own working directory
// either.
procedure PreInstallFiles();
begin
  ForceDirectories(ExpandConstant('{app}'));
  ForceDirectories(ExpandConstant('{app}\credentials'));
  FileCopy(PageCredentials.Values[0], ExpandConstant('{app}\credentials\google_service_account.json'), False);
end;

// RegisterScheduledTask is the manual-setup path's equivalent of what
// tracker.exe --setup already does for itself on the Telegram path — see
// runSetup in cmd/tracker/main.go. Not fatal if it fails: the rest of the
// install has already succeeded, so this only warns.
procedure RegisterScheduledTask();
var
  XMLPath, ExePath: String;
  ResultCode: Integer;
begin
  ExePath := ExpandConstant('{app}\tracker.exe');
  XMLPath := ExpandConstant('{tmp}\poker-tracker-task.xml');
  SaveStringToFile(XMLPath, UTF16Bytes(BuildTaskXML(ExePath, ExpandConstant('{app}'))), False);
  if not Exec('schtasks.exe', '/Create /TN "Poker Tracker" /XML "' + XMLPath + '" /F',
     '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    MsgBox('Could not register the Task Scheduler entry. You can set it up manually — see CLAUDE.md''s Windows Task Scheduler Setup section.', mbError, MB_OK);
  DeleteFile(XMLPath);
end;

// RunTelegramSetup extracts tracker.exe to {tmp} (the real {app} copy
// hasn't been installed yet at this point in the wizard) and runs
// --setup, which handles config.json, the Task Scheduler entry, and
// starting the service itself — see bot.RunSetup and runSetup in main.go.
// Release builds use -H=windowsgui, so tracker.exe never creates a console
// regardless of ShowCmd — SW_HIDE here just states that honestly; all of
// --setup's progress is visible through the Telegram conversation itself.
function RunTelegramSetup(): Boolean;
var
  ResultCode: Integer;
begin
  PreInstallFiles();
  ExtractTemporaryFile('tracker.exe');
  if not Exec(ExpandConstant('{tmp}\tracker.exe'), '--setup', ExpandConstant('{app}'),
     SW_HIDE, ewWaitUntilTerminated, ResultCode) then
  begin
    MsgBox('Could not start tracker.exe --setup.', mbError, MB_OK);
    Result := False;
    Exit;
  end;
  if ResultCode <> 0 then
  begin
    MsgBox('Setup did not complete successfully (exit code ' + IntToStr(ResultCode) + '). Please try again.', mbError, MB_OK);
    Result := False;
    Exit;
  end;
  Result := True;
end;

function NextButtonClick(CurPageID: Integer): Boolean;
begin
  Result := True;

  if CurPageID = PageMethod.ID then
    UsingTelegram := (PageMethod.SelectedValueIndex = 0)

  else if CurPageID = PageCredentials.ID then
  begin
    if not FileExists(PageCredentials.Values[0]) then
    begin
      MsgBox('Select a valid google_service_account.json file.', mbError, MB_OK);
      Result := False;
    end;
  end

  else if CurPageID = PageWaiting.ID then
    Result := RunTelegramSetup()

  else if CurPageID = PageSheetName.ID then
  begin
    if Trim(PageSheetName.Values[0]) = '' then
    begin
      MsgBox('Enter your Google Sheet name.', mbError, MB_OK);
      Result := False;
    end;
  end

  else if CurPageID = PageScreenName.ID then
  begin
    if Trim(PageScreenName.Values[0]) = '' then
    begin
      MsgBox('Enter your PokerStars screen name.', mbError, MB_OK);
      Result := False;
    end;
  end

  else if CurPageID = PageHandHistoryDir.ID then
  begin
    if not DirExists(PageHandHistoryDir.Values[0]) then
    begin
      MsgBox('Select a valid Hand History folder.', mbError, MB_OK);
      Result := False;
    end;
  end

  else if CurPageID = PageTournamentDir.ID then
  begin
    if not DirExists(PageTournamentDir.Values[0]) then
    begin
      MsgBox('Select a valid Tournament Summary folder.', mbError, MB_OK);
      Result := False;
    end;
  end;
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then
  begin
    // Idempotent for the Telegram path (already done once in
    // RunTelegramSetup); the only time it matters is manual setup, where
    // this is the first and only time it runs.
    PreInstallFiles();
    if not UsingTelegram then
    begin
      SaveStringToFile(ExpandConstant('{app}\config.json'),
        BuildConfigJSON(PageSheetName.Values[0], PageScreenName.Values[0],
          PageHandHistoryDir.Values[0], PageTournamentDir.Values[0]),
        False);
      RegisterScheduledTask();
    end;
  end;
end;
