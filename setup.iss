; Poker Tracker installer. Compiled ONCE by .github/workflows/release.yml (a
; single iscc run, no per-arch define) into ONE installer that carries every
; role's binary for BOTH CPU architectures (x64 and ARM64) and installs the
; matching one at runtime. ArchitecturesInstallIn64BitMode below covers x64 +
; arm64 (native 64-bit Program Files / registry view on both), and each [Files]
; entry is gated by an arch Check (IsARM64, via the Install*Amd64/Install*Arm64
; wrappers in [Code]) combined with the role Check — so the player never picks
; an architecture, the installer detects it. An x64 installer also runs on ARM64
; Windows via the OS's built-in x64 emulation, so one build serves both.
; Runs with /DAppVersion=<tag, "v" stripped>; the #ifndef lets a local manual
; compile work without it.
#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif

#ifndef InstallationID
  #define InstallationID ""
#endif
#if InstallationID != ""
  #define InstallationSuffix "-" + InstallationID
#else
  #define InstallationSuffix ""
#endif
#define CombinedTaskName "Poker Tracker" + InstallationSuffix
#define ServerTaskName "PokerTrackerServer" + InstallationSuffix

[Setup]
AppId={{5D7656F7-7CCB-4B00-8694-954DDE2986AE}{#InstallationSuffix}
AppName=Poker Tracker{#InstallationSuffix}
AppVersion={#AppVersion}
DefaultDirName=C:\poker-tracker{#InstallationSuffix}
DisableDirPage=yes
DefaultGroupName=Poker Tracker{#InstallationSuffix}
DisableProgramGroupPage=yes
DisableWelcomePage=no
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible arm64
ArchitecturesInstallIn64BitMode=x64compatible arm64
OutputBaseFilename=PokerTracker-Setup-{#AppVersion}
Compression=lzma
SolidCompression=yes
SetupIconFile=icon.ico
UninstallDisplayIcon={app}\tracker.exe

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Files]
; Every entry is gated by role AND architecture: the six Install* Check wrappers
; ([Code]) each combine one role (IsCombinedMode/IsClientMode/IsServerMode) with
; IsARM64, so exactly one Source per role installs and it matches the machine's
; CPU. DestName is the fixed "tracker.exe" for combined and client — an external
; .bat launcher (poker-tracker-plan.md, not in this repo) hardcodes
; C:\poker-tracker\tracker.exe. Process stopping checks the full executable
; path. The server role uses tracker-server.exe and a separate scheduled task.
;
; All-local (combined) build — installed unless the player chose connect-to-server
; or run-a-server on the role page. Both arches; the matching one wins.
Source: "tracker-amd64.exe"; DestDir: "{app}"; DestName: "tracker.exe"; Flags: ignoreversion; Check: InstallCombinedAmd64
Source: "tracker-arm64.exe"; DestDir: "{app}"; DestName: "tracker.exe"; Flags: ignoreversion; Check: InstallCombinedArm64
; Second copy, flagged dontcopy, exists only so ExtractTemporaryFile can pull
; tracker.exe's bytes out during the wizard (Telegram setup), before the real
; install step above has copied anything to {app} yet. PreInstallFiles then
; copies this {tmp} extraction onward to {app} (via CombinedExeName) and runs it
; from there — not from {tmp} directly — since tracker.exe resolves its own
; config/credentials paths from its own executable location. Also used as the
; transfer helper before a combined installation changes to the client role.
Source: "tracker-amd64.exe"; DestDir: "{tmp}"; Flags: dontcopy
Source: "tracker-arm64.exe"; DestDir: "{tmp}"; Flags: dontcopy
; Split client build — installed as the same fixed tracker.exe name when the
; player chose connect-to-server. It provides --serve and --check-lock; the
; server owns storage and the bot.
Source: "tracker-client-amd64.exe"; DestDir: "{app}"; DestName: "tracker.exe"; Flags: ignoreversion; Check: InstallClientAmd64
Source: "tracker-client-arm64.exe"; DestDir: "{app}"; DestName: "tracker.exe"; Flags: ignoreversion; Check: InstallClientArm64
; Split server build — installed as tracker-server.exe when the player chose
; run-a-server-on-this-PC. It stores data, runs the bot, and enforces the rules;
; CurStepChanged runs its --setup/--install and shows the connection string.
Source: "tracker-server-windows-amd64.exe"; DestDir: "{app}"; DestName: "tracker-server.exe"; Flags: ignoreversion; Check: InstallServerAmd64
Source: "tracker-server-windows-arm64.exe"; DestDir: "{app}"; DestName: "tracker-server.exe"; Flags: ignoreversion; Check: InstallServerArm64

; The collector is also a temporary, network-free process-stop helper for setup.
Source: "tracker-client-amd64.exe"; DestDir: "{tmp}"; Flags: dontcopy
Source: "tracker-client-arm64.exe"; DestDir: "{tmp}"; Flags: dontcopy

[UninstallDelete]
; tracker.exe itself is removed automatically (tracked via [Files] above).
; credentials was written by [Code] at install time, so Inno doesn't know
; about it unless listed. config.json, lockfile.json, and
; pending_tournaments.json are deliberately NOT listed here — a fixed AppId
; run of a new Setup.exe installs over the top without ever invoking this
; uninstaller, so those three survive an upgrade regardless; the guard in
; CurUninstallStepChanged below only matters for an actual uninstall
; (Control Panel / manually running unins000.exe), where it distinguishes a
; real removal from a silent one (e.g. a future chained old-version
; uninstall before install) and deletes them only for the former. {app}
; itself is removed last, once it's empty.
Type: files; Name: "{app}\credentials\google_service_account.json"
Type: dirifempty; Name: "{app}\credentials"
Type: dirifempty; Name: "{app}"

[UninstallRun]
Filename: "schtasks.exe"; Parameters: "/Delete /TN ""{#CombinedTaskName}"" /F"; Flags: runhidden; RunOnceId: "DeleteTask"
Filename: "schtasks.exe"; Parameters: "/Delete /TN ""{#ServerTaskName}"" /F"; Flags: runhidden; RunOnceId: "DeleteServerTask"

[Code]
var
  PageBackend: TInputOptionWizardPage;
  PageMethod: TInputOptionWizardPage;
  PageCredentials: TInputFileWizardPage;
  PageWaiting: TOutputMsgWizardPage;
  PageSheetName: TInputQueryWizardPage;
  PageScreenName: TInputQueryWizardPage;
  PageHandHistoryDir: TInputDirWizardPage;
  PageTournamentDir: TInputDirWizardPage;
  // Split (v3.7+): the role choice (all-local vs connect-to-server vs
  // run-a-server), the connection-string page (connect-to-server only), and the
  // server storage-method page (run-a-server only). The all-local pages above
  // are unchanged in identity; only their order relative to PageMethod moved.
  PageRole: TInputOptionWizardPage;
  PageConnection: TInputQueryWizardPage;
  PageTransfer: TOutputMsgWizardPage;
  ExistingCombined, ExistingRoleUnknown, IncompleteTransfer: Boolean;
  TransferStaged, TransferFinished: Boolean;
  TransferConnection: String;
  CombinedTaskSuspended, ServerTaskSuspended: Boolean;
  // PageServerMethod asks the run-a-server install the same thing the Linux
  // server's chooseBackend does: configure storage from Telegram (recommended,
  // defaults SQLite) vs set it here now. ServerUseTelegram is its answer, read
  // by ShouldSkipPage (skip the storage pages when deferring to Telegram) and
  // RunServerSetup (pass --backend sqlite when deferring). Shown only in server
  // mode; false everywhere else.
  PageServerMethod: TInputOptionWizardPage;
  ServerUseTelegram: Boolean;
  UsingTelegram: Boolean;
  // Storage backend choice (PageBackend, first page of the wizard, shown
  // regardless of Telegram vs manual). False = Google Sheets (default,
  // matches PageBackend.SelectedValueIndex = 0), True = local sqlite file.
  // Initialized from ExistingConfigJSON in InitializeWizard when
  // BackendAlreadySet, so every downstream read of this var (ShouldSkipPage,
  // PreInstallFiles, CurStepChanged) is correct whether the value came from
  // this run's page or a prior install, without needing a separate
  // "which source" branch at each call site.
  UsingSQLite: Boolean;
  // Guards against CurPageChanged's auto-start and NextButtonClick's manual
  // path both running RunTelegramSetup for the same visit to PageWaiting —
  // see CurPageChanged and the PageWaiting branch of NextButtonClick.
  TelegramSetupDone: Boolean;
  // Raw text of {app}\config.json, read at startup and refreshed after stopping
  // the previous process before writing — '' means
  // no existing install (fresh install: behave exactly as before). The
  // four config-field booleans are computed from it once and drive both
  // ShouldSkipPage (skip a page whose field is already configured) and
  // CurStepChanged (splice the existing raw value back in for any field
  // whose page was skipped, instead of rebuilding config.json from
  // scratch — see BuildConfigJSON). Manual setup path only; the Telegram
  // path already preserves these correctly on the Go side (bot.RunSetup
  // loads the existing config and only mutates the fields it collects).
  ExistingConfigJSON: String;
  BackendAlreadySet: Boolean;
  SheetAlreadySet: Boolean;
  ScreenNameAlreadySet: Boolean;
  HandHistoryAlreadySet: Boolean;
  TournamentAlreadySet: Boolean;
  // Same idea but not a config.json field — whether the credentials file
  // itself already exists at the destination, checked directly rather
  // than derived from ExistingConfigJSON (config.json doesn't record
  // this). See ShouldSkipPage and the guarded FileCopy in PreInstallFiles.
  CredentialsAlreadySet: Boolean;
  // True only when all five of the above are — i.e. there is genuinely
  // nothing left to configure either way. Drives skipping PageMethod
  // itself; see InitializeWizard and ShouldSkipPage.
  AllFieldsAlreadySet: Boolean;

// IsClientMode reports whether the player chose "Connect to a server" on the
// role page (PageRole index 1) — install the split client instead of the
// all-local combined build. Guarded with Assigned so it's safe to call from a
// [Files] Check before the page exists (it never actually is — files copy
// after the wizard — but the guard keeps it total).
function IsClientMode(): Boolean;
begin
  Result := Assigned(PageRole) and (PageRole.SelectedValueIndex = 1);
end;

// IsServerMode reports whether the player chose "Run a server on this PC"
// (PageRole index 2) — install the split server (tracker-server.exe).
function IsServerMode(): Boolean;
begin
  Result := Assigned(PageRole) and (PageRole.SelectedValueIndex = 2);
end;

// IsCombinedMode is the all-local default — neither of the split roles.
function IsCombinedMode(): Boolean;
begin
  Result := not IsClientMode() and not IsServerMode();
end;

// The six role×arch Check wrappers gate the [Files] entries: each pairs one role
// with IsARM64 (an Inno built-in) so exactly one Source per role installs and it
// matches the machine's CPU. Kept as named functions (not inline Check
// expressions) so each [Files] line reads as a single, self-describing Check.
function InstallCombinedAmd64(): Boolean;
begin
  Result := IsCombinedMode() and not IsARM64;
end;
function InstallCombinedArm64(): Boolean;
begin
  Result := IsCombinedMode() and IsARM64;
end;
function InstallClientAmd64(): Boolean;
begin
  Result := IsClientMode() and not IsARM64;
end;
function InstallClientArm64(): Boolean;
begin
  Result := IsClientMode() and IsARM64;
end;
function InstallServerAmd64(): Boolean;
begin
  Result := IsServerMode() and not IsARM64;
end;
function InstallServerArm64(): Boolean;
begin
  Result := IsServerMode() and IsARM64;
end;

// CombinedExeName is the arch-matching combined binary's source filename, used
// by PreInstallFiles to ExtractTemporaryFile / copy the right one to tracker.exe.
function CombinedExeName(): String;
begin
  if IsARM64 then
    Result := 'tracker-arm64.exe'
  else
    Result := 'tracker-amd64.exe';
end;

// ReadFileAsString loads Path into a single raw String, or returns '' if
// the file doesn't exist (the normal case on a fresh install — not an
// error). Uses LoadStringFromFile rather than the line-splitting
// LoadStringsFromFile so it doesn't have to reconcile this script's own
// CRLF output with config.SaveConfig's LF-only json.MarshalIndent output
// (Go, internal/config/config.go) — the whole-file string is scanned for
// JSON tokens directly, so line endings never matter.
function ReadFileAsString(const Path: String): String;
var
  Content: AnsiString;
begin
  if not LoadStringFromFile(Path, Content) then
    Result := ''
  else
    Result := Content;
end;

// JSONUnescape reverses JSONEscape (below) — backslash first, then quote,
// the exact opposite order JSONEscape applies them, so the round trip is
// lossless.
function JSONUnescape(const S: String): String;
var
  R: String;
begin
  R := S;
  StringChangeEx(R, '\"', '"', True);
  StringChangeEx(R, '\\', '\', True);
  Result := R;
end;

// JSONFieldIsSet reports whether FieldName's value in JSON is anything
// other than the literal null. Marker includes the closing quote and
// colon, so it can't false-match a longer key name. Whitespace after the
// colon is skipped since this script's own JSON and Go's
// json.MarshalIndent output (verified: both use exactly one space) don't
// need to match byte-for-byte for this to work either way.
function JSONFieldIsSet(const JSON, FieldName: String): Boolean;
var
  Marker: String;
  I: Integer;
begin
  Result := False;
  Marker := '"' + FieldName + '":';
  I := Pos(Marker, JSON);
  if I = 0 then
    Exit;
  I := I + Length(Marker);
  while (I <= Length(JSON)) and ((JSON[I] = ' ') or (JSON[I] = #13) or (JSON[I] = #10) or (JSON[I] = #9)) do
    I := I + 1;
  Result := Copy(JSON, I, 4) <> 'null';
end;

// JSONExtractRawValue returns FieldName's value in JSON exactly as it
// appears — quotes and escaping intact — so it can be spliced straight
// into a newly built document without an unescape/re-escape round trip.
// The closing quote is found by counting consecutive backslashes
// immediately before each candidate '"': an odd count means that quote is
// itself escaped (part of the string), even (including zero) means it's
// the real close. A naive "quote not preceded by a single backslash" test
// gets this wrong for a value ending in an escaped backslash, e.g. a
// Windows path with a trailing separator ("C:\HandHistory\" encodes as
// "C:\\HandHistory\\" — the real closing quote IS preceded by a
// backslash there). Returns '' if the field is null, absent, or not a
// string.
function JSONExtractRawValue(const JSON, FieldName: String): String;
var
  Marker: String;
  I, EndI, K, BackslashCount: Integer;
  Closed: Boolean;
begin
  Result := '';
  Marker := '"' + FieldName + '":';
  I := Pos(Marker, JSON);
  if I = 0 then
    Exit;
  I := I + Length(Marker);
  while (I <= Length(JSON)) and ((JSON[I] = ' ') or (JSON[I] = #13) or (JSON[I] = #10) or (JSON[I] = #9)) do
    I := I + 1;
  if (I > Length(JSON)) or (JSON[I] <> '"') then
    Exit;
  EndI := I + 1;
  Closed := False;
  while (EndI <= Length(JSON)) and not Closed do
  begin
    if JSON[EndI] = '"' then
    begin
      BackslashCount := 0;
      K := EndI - 1;
      while (K >= I) and (JSON[K] = '\') do
      begin
        BackslashCount := BackslashCount + 1;
        K := K - 1;
      end;
      if (BackslashCount mod 2) = 0 then
        Closed := True
      else
        EndI := EndI + 1;
    end
    else
      EndI := EndI + 1;
  end;
  if not Closed then
    Exit;
  Result := Copy(JSON, I, EndI - I + 1);
end;

// JSONExtractStringValue is JSONExtractRawValue with the surrounding
// quotes stripped and escaping reversed — the plain value, for contexts
// outside JSON (see CurrentScreenName).
function JSONExtractStringValue(const JSON, FieldName: String): String;
var
  Raw: String;
begin
  Raw := JSONExtractRawValue(JSON, FieldName);
  if Length(Raw) >= 2 then
    Result := JSONUnescape(Copy(Raw, 2, Length(Raw) - 2))
  else
    Result := '';
end;

// JSONFieldRaw returns FieldName's existing value ready to splice into a
// rebuilt config.json: its raw string literal, or the bare word null,
// whichever JSON already has.
function JSONFieldRaw(const JSON, FieldName: String): String;
begin
  if JSONFieldIsSet(JSON, FieldName) then
    Result := JSONExtractRawValue(JSON, FieldName)
  else
    Result := 'null';
end;

// JSONExtractObjectRaw returns FieldName's whole nested object verbatim,
// braces included. Used to carry alerts/hardstop/schedule through a
// rewrite untouched: the wizard never asks about any field inside them,
// so byte-for-byte preservation is simpler and safer than reconstructing
// them field by field. Brace-depth counting alone is only safe because
// none of these three objects contain string values that could hide a
// stray brace — true here (every leaf is a number or null) but not a
// general-purpose JSON object extractor. Returns '' if FieldName isn't
// present as an object (in particular: JSON = '', meaning no existing
// install).
function JSONExtractObjectRaw(const JSON, FieldName: String): String;
var
  Marker: String;
  StartI, I, Depth: Integer;
begin
  Result := '';
  Marker := '"' + FieldName + '":';
  I := Pos(Marker, JSON);
  if I = 0 then
    Exit;
  I := I + Length(Marker);
  while (I <= Length(JSON)) and (JSON[I] <> '{') do
    I := I + 1;
  if I > Length(JSON) then
    Exit;
  StartI := I;
  Depth := 0;
  while I <= Length(JSON) do
  begin
    if JSON[I] = '{' then
      Depth := Depth + 1
    else if JSON[I] = '}' then
    begin
      Depth := Depth - 1;
      if Depth = 0 then
      begin
        Result := Copy(JSON, StartI, I - StartI + 1);
        Exit;
      end;
    end;
    I := I + 1;
  end;
end;

// PreservedObjectOrDefault carries an existing alerts/hardstop/schedule
// block through unchanged, or falls back to DefaultBlock when JSON has none
// (JSONExtractObjectRaw returns '' — either JSON = '', a fresh install, or
// FieldName genuinely isn't present as an object). JSON is a parameter
// rather than reading the global ExistingConfigJSON directly so this can
// also serve WritePreSeedConfig, which rebuilds from a live re-read of
// config.json rather than the wizard-startup snapshot — see its own doc
// comment for why that distinction matters there.
function PreservedObjectOrDefault(const JSON, FieldName, DefaultBlock: String): String;
var
  Existing: String;
begin
  Existing := JSONExtractObjectRaw(JSON, FieldName);
  if Existing <> '' then
    Result := Existing
  else
    Result := DefaultBlock;
end;

// CurrentScreenName is the screen name to use both for path-join
// validation (NextButtonClick, PageHandHistoryDir/PageTournamentDir) and
// for the final config.json: the page's own Values[0] if it was shown
// this run, or the preserved value if ScreenNameAlreadySet skipped it.
// Needed because HandHistoryDir/TournamentDir can still be shown (their
// own field still unset) even when the screen name page itself was
// skipped (already set) — the two don't skip together.
function CurrentScreenName(): String;
begin
  if ScreenNameAlreadySet and not IsClientMode() then
    Result := JSONExtractStringValue(ExistingConfigJSON, 'pokerstars_screen_name')
  else
    Result := PageScreenName.Values[0];
end;

// ApplyTestAnswers presets the wizard's answers from command-line params so a
// local, unattended /VERYSILENT run can drive the storage flow without a human
// clicking through pages. It is inert in every real install: real installs
// never pass TESTROLE/TESTSERVERMETHOD/TESTBACKEND, so the ExpandConstant
// defaults ('') leave every SelectedValueIndex and derived var untouched.
//
// It sets both the page SelectedValueIndex (read by the [Files] Check wrappers
// and RunServerSetup via IsServerMode) AND the booleans NextButtonClick would
// normally derive (ServerUseTelegram/UsingSQLite/UsingTelegram) — silent mode
// skips page display, so those page-transition handlers never fire and the
// booleans must be set here to match. Exercised only by the gitignored
// localtest/windows-installer-check.sh; see CLAUDE.md.
procedure ApplyTestAnswers();
var
  Role, ServerMethod, Backend, Creds, Sheet, Answer: String;
begin
  Role := ExpandConstant('{param:TESTROLE|}');
  if Role = 'client' then PageRole.SelectedValueIndex := 1
  else if Role = 'server' then PageRole.SelectedValueIndex := 2
  else if Role = 'combined' then PageRole.SelectedValueIndex := 0;

  Answer := ExpandConstant('{param:TESTMETHOD|}');
  if Answer = 'telegram' then
  begin
    PageMethod.SelectedValueIndex := 0;
    UsingTelegram := True;
  end
  else if Answer = 'manual' then
  begin
    PageMethod.SelectedValueIndex := 1;
    UsingTelegram := False;
  end;

  ServerMethod := ExpandConstant('{param:TESTSERVERMETHOD|}');
  if ServerMethod = 'telegram' then
  begin
    PageServerMethod.SelectedValueIndex := 0;
    ServerUseTelegram := True;
  end
  else if ServerMethod = 'here' then
  begin
    PageServerMethod.SelectedValueIndex := 1;
    ServerUseTelegram := False;
  end;

  Backend := ExpandConstant('{param:TESTBACKEND|}');
  if Backend = 'sheets' then
  begin
    PageBackend.SelectedValueIndex := 0;
    UsingSQLite := False;
  end
  else if Backend = 'sqlite' then
  begin
    PageBackend.SelectedValueIndex := 1;
    UsingSQLite := True;
  end;

  // For the "set storage here -> Sheets" path the credentials and sheet-name
  // pages are shown and validated (FileExists + non-empty); a silent test must
  // supply both or the wizard rightly refuses to proceed. TESTCREDS is a path
  // to any existing file, TESTSHEET any non-empty name.
  Creds := ExpandConstant('{param:TESTCREDS|}');
  if Creds <> '' then PageCredentials.Values[0] := Creds;
  Sheet := ExpandConstant('{param:TESTSHEET|}');
  if Sheet <> '' then PageSheetName.Values[0] := Sheet;
  Answer := ExpandConstant('{param:TESTCONNECTION|}');
  if Answer <> '' then PageConnection.Values[0] := Answer;
  Answer := ExpandConstant('{param:TESTSCREENNAME|}');
  if Answer <> '' then PageScreenName.Values[0] := Answer;
  Answer := ExpandConstant('{param:TESTHANDHISTORY|}');
  if Answer <> '' then PageHandHistoryDir.Values[0] := Answer;
  Answer := ExpandConstant('{param:TESTTOURNAMENT|}');
  if Answer <> '' then PageTournamentDir.Values[0] := Answer;
end;

// Disable triggers and failure recovery before asking the owner to exit.
// Enumeration distinguishes an absent task from scheduler/access failures.
procedure SuspendScheduledRecovery();
var
  Scheduler, Tasks, Task: Variant;
  I: Integer;
begin
  Scheduler := CreateOleObject('Schedule.Service');
  Scheduler.Connect();
  Tasks := Scheduler.GetFolder('\').GetTasks(1);
  for I := 1 to Tasks.Count do
  begin
    Task := Tasks.Item(I);
    if Task.Enabled then
    begin
      if (Task.Name = '{#CombinedTaskName}') and not CombinedTaskSuspended then
      begin
        Task.Enabled := False;
        CombinedTaskSuspended := True;
      end
      else if (Task.Name = '{#ServerTaskName}') and not ServerTaskSuspended then
      begin
        Task.Enabled := False;
        ServerTaskSuspended := True;
      end;
    end;
  end;
end;

procedure RestoreScheduledRecovery(const TaskName: String; var Suspended: Boolean);
var
  ResultCode: Integer;
begin
  if not Suspended then Exit;
  if not Exec('schtasks.exe', '/Change /TN "' + TaskName + '" /ENABLE',
    '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
  begin
    SuppressibleMsgBox('Could not restore scheduled recovery for "' + TaskName + '". Enable it in Task Scheduler.', mbError, MB_OK, IDOK);
    Exit;
  end;
  Suspended := False;
  if not Exec('schtasks.exe', '/Run /TN "' + TaskName + '"',
    '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    SuppressibleMsgBox('Could not restart "' + TaskName + '". Run it in Task Scheduler.', mbError, MB_OK, IDOK);
end;

procedure StopInstallation();
var
  HelperPath: String;
  ResultCode: Integer;
begin
  SuspendScheduledRecovery();
  if IsUninstaller then
  begin
    HelperPath := ExpandConstant('{app}\tracker.exe');
    if not FileExists(HelperPath) then
      HelperPath := ExpandConstant('{app}\tracker-server.exe');
    if not FileExists(HelperPath) then
      Exit;
  end
  else
  begin
    if IsARM64 then
      HelperPath := 'tracker-client-arm64.exe'
    else
      HelperPath := 'tracker-client-amd64.exe';
    ExtractTemporaryFile(HelperPath);
    HelperPath := ExpandConstant('{tmp}\') + HelperPath;
  end;
  if not Exec(HelperPath, '--stop-installation "' + ExpandConstant('{app}') + '"',
    '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    RaiseException('Could not stop this installation safely. Close its tracker process and try again.');
end;

function TransferHelperPath(): String;
begin
  Result := ExpandConstant('{app}\tracker-transfer.exe');
  ExtractTemporaryFile(CombinedExeName());
  if not FileCopy(ExpandConstant('{tmp}\' + CombinedExeName()), Result, False) then
    RaiseException('Could not prepare the transfer helper. Existing installation files were kept.');
end;

function RunTransferStep(const Arguments: String): Boolean;
var
  ResultCode: Integer;
  Details: String;
begin
  Result := Exec(TransferHelperPath(), Arguments, ExpandConstant('{app}'),
    SW_HIDE, ewWaitUntilTerminated, ResultCode) and (ResultCode = 0);
  if not Result then
  begin
    Details := ReadFileAsString(ExpandConstant('{app}\transfer-error.txt'));
    SuppressibleMsgBox('Transfer could not finish. Existing data and protection files were kept.' +
      '' + #13#10 + #13#10 + Details + #13#10 + #13#10 +
      'Retry this installer with the same server connection string to resolve the saved transfer.',
      mbError, MB_OK, IDOK);
  end;
end;

function NeedsTransfer(): Boolean;
begin
  Result := IsClientMode() and (ExistingCombined or IncompleteTransfer);
end;

function StageTransfer(): Boolean;
var
  ResultCode: Integer;
begin
  Result := True;
  if IsClientMode() and ExistingRoleUnknown then
  begin
    SuppressibleMsgBox('This existing executable cannot identify its role. Upgrade it in its current role with this version first, then run Connect to a server.', mbError, MB_OK, IDOK);
    Result := False;
    Exit;
  end;
  if not NeedsTransfer() or TransferStaged then Exit;
  TransferConnection := Trim(PageConnection.Values[0]);
  Result := RunTransferStep('--transfer-stage "' + TransferConnection + '"');
  TransferStaged := Result;
  if not Result then
  begin
    if JSONExtractStringValue(ReadFileAsString(ExpandConstant('{app}\source_transfer.json')), 'phase') = 'aborted' then
    begin
      Exec('schtasks.exe', '/Change /TN "{#CombinedTaskName}" /ENABLE', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
      Exec('schtasks.exe', '/Run /TN "{#CombinedTaskName}"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
    end;
  end;
  if Result then
  begin
    if not Exec('schtasks.exe', '/Change /TN "{#CombinedTaskName}" /DISABLE', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    begin
      RunTransferStep('--transfer-rollback');
      Exec('schtasks.exe', '/Change /TN "{#CombinedTaskName}" /ENABLE', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
      Exec('schtasks.exe', '/Run /TN "{#CombinedTaskName}"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
      Result := False;
    end
    else
      Result := RunTransferStep('--transfer-stage "' + TransferConnection + '"');
  end;
end;

procedure InitializeWizard();
var
  MethodDescription, BackendDescription, RoleOutput, RoleText, TransferState: String;
  RoleResult: Integer;
begin
  // WizardDirValue is available during initialization and includes /DIR and
  // the directory restored from this installation identity's uninstall entry.
  ExistingConfigJSON := ReadFileAsString(AddBackslash(WizardDirValue) + 'config.json');
  RoleOutput := ExpandConstant('{tmp}\existing-tracker-role.txt');
  if FileExists(AddBackslash(WizardDirValue) + 'tracker.exe') then
  begin
    if Exec(AddBackslash(WizardDirValue) + 'tracker.exe', '--role --role-output "' + RoleOutput + '"', '', SW_HIDE, ewWaitUntilTerminated, RoleResult) and (RoleResult = 0) then
    begin
      RoleText := Trim(ReadFileAsString(RoleOutput));
      ExistingCombined := (RoleText = 'combined');
      ExistingRoleUnknown := (RoleText <> 'combined') and (RoleText <> 'client');
    end
    else ExistingRoleUnknown := True;
    DeleteFile(RoleOutput);
  end;
  TransferState := ReadFileAsString(AddBackslash(WizardDirValue) + 'source_transfer.json');
  RoleText := JSONExtractStringValue(TransferState, 'phase');
  IncompleteTransfer := (RoleText = 'preparing') or (RoleText = 'prepared') or (RoleText = 'stopping');

  BackendAlreadySet := JSONFieldIsSet(ExistingConfigJSON, 'storage_backend');
  // Seeds UsingSQLite from the existing value when the page that would
  // normally set it (PageBackend, below) is about to be skipped — see the
  // doc comment on the UsingSQLite var declaration.
  if BackendAlreadySet then
    UsingSQLite := JSONExtractStringValue(ExistingConfigJSON, 'storage_backend') = 'sqlite';
  SheetAlreadySet := JSONFieldIsSet(ExistingConfigJSON, 'google_sheet_name') or
                      JSONFieldIsSet(ExistingConfigJSON, 'google_sheet_id');
  ScreenNameAlreadySet := JSONFieldIsSet(ExistingConfigJSON, 'pokerstars_screen_name');
  HandHistoryAlreadySet := JSONFieldIsSet(ExistingConfigJSON, 'hand_history');
  TournamentAlreadySet := JSONFieldIsSet(ExistingConfigJSON, 'tournament_summary');
  CredentialsAlreadySet := FileExists(AddBackslash(WizardDirValue) + 'credentials\google_service_account.json');
  // On sqlite, Sheet/Credentials never get set by this wizard (there's no
  // Google step for that backend at all — ShouldSkipPage), so requiring
  // them here would keep AllFieldsAlreadySet permanently False for a fully
  // configured sqlite install, showing PageMethod pointlessly on every
  // rerun. UsingSQLite is already correct at this point (set above from
  // BackendAlreadySet, or PageBackend's own default False otherwise).
  AllFieldsAlreadySet := BackendAlreadySet and ScreenNameAlreadySet and HandHistoryAlreadySet and
                          TournamentAlreadySet and (UsingSQLite or (SheetAlreadySet and CredentialsAlreadySet));

  // The two cases that DO show this page get their own wording — nothing
  // (fresh) vs something-but-not-everything (partial) — so it's never the
  // same generic text regardless of which is true.
  if ExistingConfigJSON = '' then
  begin
    MethodDescription := 'No existing installation was found — this will set up Poker Tracker from scratch.';
    BackendDescription := 'Google Sheets keeps a live spreadsheet you can open in a browser, but needs a ' +
      'one-time Google Cloud service account setup. Local file stores everything in a single file on this ' +
      'PC — no Google account, nothing to share, nothing to configure.';
  end
  else
  begin
    MethodDescription := 'An existing installation was found, but some settings are still missing. ' +
      'Whatever''s already configured — sheet, screen name, folders, credentials — will be kept as-is; ' +
      'you''ll only be asked about what''s still missing.';
    // Skipped entirely when BackendAlreadySet (see ShouldSkipPage), so this
    // text is only ever shown to a player upgrading an install that
    // predates the backend field.
    BackendDescription := 'An existing installation was found without a storage backend recorded. Choose ' +
      'how Poker Tracker should store your tracked sessions going forward.';
  end;

  // Anchored at wpSelectDir, not wpWelcome — {app} only becomes valid for
  // ExpandConstant once the wizard's internal position passes wpSelectDir,
  // regardless of DisableDirPage hiding that page from view (it hides the
  // page, not its position in the underlying sequence). Every page in this
  // chain sat before wpSelectDir when anchored at wpWelcome, so the first
  // ExpandConstant('{app}') call anywhere (PreInstallFiles, triggered by
  // the "Complete setup in Telegram" page's Next click) threw "An attempt
  // was made to expand the 'app' constant before it was initialized" —
  // and since that happens before RunTelegramSetup ever reaches the Exec
  // call that launches tracker.exe --setup, the Telegram flow never
  // actually started, which is why nothing ever responded. Visually
  // nothing changes: wpSelectDir is still never shown, so Welcome is still
  // immediately followed by this page either way.
  //
  // PageMethod (Telegram vs manual) now comes first — before PageBackend — so
  // UsingTelegram is known by the time the wizard reaches the backend page. That
  // is what lets ShouldSkipPage skip PageBackend in Telegram mode and hand the
  // storage-backend question to bot.RunSetup (which asks it whenever the field
  // is null); the manual path still answers it on PageBackend here. Both are
  // anchored at/after wpSelectDir (PageMethod at wpSelectDir, PageBackend one
  // page later) so ExpandConstant('{app}') is valid by the time either is
  // reached — see the wpSelectDir note above. PageCredentials follows
  // PageBackend (not PageMethod) so the storage pages stay contiguous.
  //
  // Role choice, shown first: all-local (both roles in one process, the default)
  // vs connect-to-server (this PC is the poker PC) vs run-a-server (a separate
  // always-on machine). Anchored at wpWelcome — it collects no {app}-dependent
  // value, so it needn't sit past wpSelectDir like the pages below.
  // ShouldSkipPage hides the pages that don't apply to the chosen role.
  PageRole := CreateInputOptionPage(wpWelcome,
    'Setup Type',
    'How will you run Poker Tracker?',
    'Run everything on this PC unless you are using the optional two-machine split. In the split, the poker PC runs "Connect to a server" and a separate always-on machine runs "Run a server on this PC".',
    True, False);
  PageRole.Add('Run everything on this PC (recommended)');
  PageRole.Add('Connect to a server (this PC is the poker PC)');
  PageRole.Add('Run a server on this PC (stores data, runs the bot)');
  PageRole.SelectedValueIndex := 0;

  PageConnection := CreateInputQueryPage(PageRole.ID,
    'Connect to a Server',
    'Paste the connection string your server printed',
    'Your server printed a line starting with tracker:// when you ran its setup. Paste the whole line.');
  PageConnection.Add('Connection string:', False);
  PageConnection.Values[0] := JSONExtractStringValue(ExistingConfigJSON, 'connection_string');

  // Run-a-server only (ShouldSkipPage): the same choice the Linux server offers
  // in chooseBackend — defer storage/alerts/rules to Telegram (defaults SQLite,
  // switchable later via /storage) or set the storage backend here now. Anchored
  // after PageConnection so it sits early, before the wpSelectDir storage pages.
  PageServerMethod := CreateInputOptionPage(PageConnection.ID,
    'Server Setup',
    'How do you want to configure storage, alerts, and rules?',
    'Everything can be set from Telegram once your poker PC is paired — the simplest path. It stores data in a local file (SQLite) you can switch to Google Sheets anytime with /storage. Or set the storage backend here now.',
    True, False);
  PageServerMethod.Add('From Telegram on my phone (recommended)');
  PageServerMethod.Add('Set storage here now');
  PageServerMethod.SelectedValueIndex := 0;

  PageMethod := CreateInputOptionPage(wpSelectDir,
    'Setup Method',
    'Would you like to set up via Telegram? (Recommended)',
    MethodDescription,
    True, False);
  PageMethod.Add('Yes, use Telegram');
  PageMethod.Add('No, set up manually');
  PageMethod.SelectedValueIndex := 0;

  PageBackend := CreateInputOptionPage(PageMethod.ID,
    'Storage Backend',
    'How do you want to store your tracked poker sessions?',
    BackendDescription,
    True, False);
  PageBackend.Add('Google Sheets');
  PageBackend.Add('Local file (no Google account needed)');
  PageBackend.SelectedValueIndex := 0;

  // Manual setup only — skipped entirely for Telegram (see ShouldSkipPage),
  // since --setup has its own optional upload-or-skip step for this file.
  // Anchored after PageBackend (which now precedes it) so the storage pages
  // stay contiguous: Method -> Backend -> Credentials -> ... -> SheetName.
  PageCredentials := CreateInputFilePage(PageBackend.ID,
    'Google Service Account',
    'Locate your service account credentials file',
    'Poker Tracker needs this file to read and write your Google Sheet.');
  PageCredentials.Add('Select your google_service_account.json file', 'JSON files|*.json|All files|*.*', '.json');

  PageWaiting := CreateOutputMsgPage(PageCredentials.ID,
    'Telegram Setup',
    'Complete setup in Telegram',
    'Go to Telegram and send a message to your bot to begin. The buttons on this window will not work until you finish there.');

  PageSheetName := CreateInputQueryPage(PageWaiting.ID,
    'Google Sheet',
    'Enter your Google Sheet name, or paste its full URL',
    'Create a blank sheet and share it with the service account before continuing.');
  PageSheetName.Add('Sheet name or URL:', False);

  PageScreenName := CreateInputQueryPage(PageSheetName.ID,
    'PokerStars Screen Name',
    'Enter your PokerStars screen name',
    'Exact capitalisation matters.');
  PageScreenName.Add('PokerStars screen name:', False);
  PageScreenName.Values[0] := JSONExtractStringValue(ExistingConfigJSON, 'pokerstars_screen_name');

  PageHandHistoryDir := CreateInputDirPage(PageScreenName.ID,
    'Hand History Folder',
    'Select the base Hand History folder',
    'Do NOT browse into your screen name''s subfolder underneath it — that part is added automatically.',
    False, '');
  PageHandHistoryDir.Add('Parent HandHistory folder:');
  PageHandHistoryDir.Values[0] := JSONExtractStringValue(ExistingConfigJSON, 'hand_history');

  PageTournamentDir := CreateInputDirPage(PageHandHistoryDir.ID,
    'Tournament Summary Folder',
    'Select the base TournamentSummaries folder',
    'Do NOT browse into your screen name''s subfolder underneath it — that part is added automatically.',
    False, '');
  PageTournamentDir.Add('Parent TournamentSummaries folder:');
  PageTournamentDir.Values[0] := JSONExtractStringValue(ExistingConfigJSON, 'tournament_summary');

  // Inert unless the local installer test passes TEST* params; see ApplyTestAnswers.
  PageTransfer := CreateOutputMsgPage(PageTournamentDir.ID,
    'Move this PC to your server', 'Transfer existing history before switching roles',
    'Close PokerStars before continuing. Your existing sessions, bankroll, rules and unanswered tournament questions will be copied to the configured empty server and verified before this PC switches to collection only.' +
    '' + #13#10 + #13#10 + 'The server must have no collector connected, no pending work, and the same saved Telegram chat (or no saved chat). Google credentials stay on their own machine. Your original history and protection files stay on this PC.' +
    '' + #13#10 + #13#10 + 'If interrupted, rerun this installer with the same connection string. Transfer starts when you click Install.');
  ApplyTestAnswers();
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := False;
  if PageID = PageTransfer.ID then begin Result := not NeedsTransfer(); Exit; end;
  // Run-a-server mode shows PageServerMethod (configure from Telegram vs set
  // storage here) and, when the player sets storage here, the storage backend
  // page (+ credentials + sheet name for Sheets) — exactly the all-local storage
  // pages. The certificate, pairing token, and connection string are handled by
  // tracker-server --setup after install (RunServerSetup). It never asks the
  // screen name, folders, all-local Telegram-method, or connection-string pages:
  // the collector owns the screen name/folders, and the server *generates* the
  // connection string rather than consuming one.
  if IsServerMode() then
  begin
    Result := (PageID = PageConnection.ID) or (PageID = PageMethod.ID) or
              (PageID = PageWaiting.ID) or (PageID = PageScreenName.ID) or
              (PageID = PageHandHistoryDir.ID) or (PageID = PageTournamentDir.ID);
    // Storage pages: skipped when the player chose to configure from Telegram
    // (ServerUseTelegram — RunServerSetup then records the SQLite default and the
    // bot owns storage/alerts/rules), or when a prior install already recorded a
    // backend (don't silently switch — that's /storage's migration).
    if (PageID = PageBackend.ID) and (BackendAlreadySet or ServerUseTelegram) then
      Result := True;
    // Sheets-only pages: shown only when Sheets is the choice, storage is being
    // set here (not deferred to Telegram), and the value isn't already in place.
    if (PageID = PageCredentials.ID) and (UsingSQLite or CredentialsAlreadySet or ServerUseTelegram) then
      Result := True;
    if (PageID = PageSheetName.ID) and (UsingSQLite or SheetAlreadySet or ServerUseTelegram) then
      Result := True;
    Exit;
  end;
  // Connect-to-server mode shows only the connection string, screen name, and
  // the two folder pages; every all-local / Google / Telegram page is skipped.
  if IsClientMode() then
  begin
    Result := (PageID = PageBackend.ID) or (PageID = PageMethod.ID) or
              (PageID = PageCredentials.ID) or (PageID = PageWaiting.ID) or
              (PageID = PageSheetName.ID) or (PageID = PageServerMethod.ID);
    Exit;
  end;
  // All-local mode never shows the connection-string or run-a-server pages.
  if (PageID = PageConnection.ID) or (PageID = PageServerMethod.ID) then
  begin
    Result := True;
    Exit;
  end;
  // Backend page: skipped when a prior install already recorded a backend, or
  // when the player chose Telegram — PageMethod now precedes this page, so
  // UsingTelegram is set, and bot.RunSetup owns the backend question in that
  // path (asks it when the field is null). The manual path answers it here.
  if (PageID = PageBackend.ID) and (BackendAlreadySet or UsingTelegram) then
  begin
    Result := True;
    Exit;
  end;
  // Checked before the UsingTelegram branch below since UsingTelegram
  // isn't meaningfully set yet when this page itself is what's being
  // asked about — there's nothing for either path to ask when every
  // field is already configured, so there's no method choice to make.
  if (PageID = PageMethod.ID) and AllFieldsAlreadySet then
  begin
    Result := True;
    Exit;
  end;
  if UsingTelegram then
  begin
    // PageCredentials is skipped for Telegram too — bot.RunSetup's own
    // Step 1.5 (send the file as a Telegram attachment, or skip if
    // already in place) covers it; asking again here via a native
    // browse dialog would just be redundant. Also unconditionally correct
    // on sqlite — bot.RunSetup skips its own Step 1.5/Step 2 for that
    // backend already, so there's nothing for this native page to redo
    // either way.
    if (PageID = PageCredentials.ID) or (PageID = PageSheetName.ID) or (PageID = PageScreenName.ID) or
       (PageID = PageHandHistoryDir.ID) or (PageID = PageTournamentDir.ID) then
      Result := True;
  end
  else
  begin
    // Also skip whichever of these already has a value in an existing
    // config.json (ExistingConfigJSON, computed in InitializeWizard) — a
    // rerun over a complete install then shows none of them, matching
    // CurStepChanged's decision below to preserve rather than re-collect
    // the same fields. PageCredentials isn't a config.json field but
    // follows the same rule: skip it once the file's already in place.
    //
    // PageCredentials and PageSheetName are additionally skipped whenever
    // UsingSQLite — a local-file install has no Google Sheet and nothing
    // to authenticate to, so neither question applies regardless of
    // whether either field happens to be set in an existing config.
    if PageID = PageWaiting.ID then
      Result := True
    else if (PageID = PageCredentials.ID) and (CredentialsAlreadySet or UsingSQLite) then
      Result := True
    else if (PageID = PageSheetName.ID) and (SheetAlreadySet or UsingSQLite) then
      Result := True
    else if (PageID = PageScreenName.ID) and ScreenNameAlreadySet then
      Result := True
    else if (PageID = PageHandHistoryDir.ID) and HandHistoryAlreadySet then
      Result := True
    else if (PageID = PageTournamentDir.ID) and TournamentAlreadySet then
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

// ExtractSheetIDFromURL mirrors bot.extractSheetIDFromURL (Go) — returns
// the spreadsheet ID from a pasted Google Sheets URL, or '' if Input
// doesn't contain one. Doesn't require a regex engine (Pascal Script has
// none): "spreadsheets/d/" is an unambiguous marker, and the ID is
// whatever follows it up to the next path/query/fragment separator.
function ExtractSheetIDFromURL(const Input: String): String;
var
  Marker: String;
  MarkerPos, IDStart, IDEnd: Integer;
begin
  Result := '';
  Marker := 'spreadsheets/d/';
  MarkerPos := Pos(Marker, Input);
  if MarkerPos = 0 then
    Exit;
  IDStart := MarkerPos + Length(Marker);
  IDEnd := IDStart;
  while (IDEnd <= Length(Input)) and (Input[IDEnd] <> '/') and (Input[IDEnd] <> '?') and (Input[IDEnd] <> '#') do
    IDEnd := IDEnd + 1;
  Result := Copy(Input, IDStart, IDEnd - IDStart);
end;

// JSONStringOrNull renders S as a JSON string, or the literal null if S
// is empty — used for google_sheet_id/google_sheet_name, which are
// pointer fields in config.Config (see internal/config/config.go): empty
// must produce null, not an empty string, or config.LoadConfig would
// treat "not configured" as "configured to an empty string" instead.
function JSONStringOrNull(const S: String): String;
begin
  if S = '' then
    Result := 'null'
  else
    Result := '"' + JSONEscape(S) + '"';
end;

// BuildConfigJSON mirrors config.Config's JSON shape exactly (see
// internal/config/config.go), but — unlike the old version of this
// function — never invents null for a field this run didn't touch. Every
// parameter is already a ready-to-splice JSON fragment (a quoted string,
// the word null, or a whole "{...}" object), resolved by the caller
// (CurStepChanged, WritePreSeedConfig) from either this run's freshly
// collected wizard values or, for anything whose page was skipped as
// already-set, the matching raw fragment out of ExistingConfigJSON. This is
// what makes a rerun over a complete install a no-op write: every fragment
// passed in is just the existing file's own content, copied back unchanged.
// Threshold/schedule objects, notifications, and pairing identity are preserved
// from Existing; no wizard page collects these operational preferences.
// StorageBackendRaw sits second, matching config.Config's field order
// (internal/config/config.go: telegram_chat_id, storage_backend,
// google_sheet_id, google_sheet_name, pokerstars_screen_name).
function BuildConfigJSON(const Existing, TelegramChatIDRaw, StorageBackendRaw, SheetNameRaw, SheetIDRaw, ScreenNameRaw,
  HandHistoryRaw, TournamentRaw, AlertsBlock, HardstopBlock, ScheduleBlock, ConnectionStringRaw: String): String;
begin
  Result :=
    '{' + #13#10 +
    '  "setup": {' + #13#10 +
    '    "telegram_chat_id": ' + TelegramChatIDRaw + ',' + #13#10 +
    '    "storage_backend": ' + StorageBackendRaw + ',' + #13#10 +
    '    "google_sheet_id": ' + SheetIDRaw + ',' + #13#10 +
    '    "google_sheet_name": ' + SheetNameRaw + ',' + #13#10 +
    '    "pokerstars_screen_name": ' + ScreenNameRaw + ',' + #13#10 +
    '    "connection_string": ' + ConnectionStringRaw + ',' + #13#10 +
    '    "pair_token": ' + JSONFieldRaw(Existing, 'pair_token') + #13#10 +
    '  },' + #13#10 +
    '  "paths": {' + #13#10 +
    '    "hand_history": ' + HandHistoryRaw + ',' + #13#10 +
    '    "tournament_summary": ' + TournamentRaw + #13#10 +
    '  },' + #13#10 +
    '  "alerts": ' + AlertsBlock + ',' + #13#10 +
    '  "hardstop": ' + HardstopBlock + ',' + #13#10 +
    '  "schedule": ' + ScheduleBlock + ',' + #13#10 +
    '  "notifications": ' + PreservedObjectOrDefault(Existing, 'notifications', '{}') + #13#10 +
    '}' + #13#10;
end;

// DefaultAlertsBlock/DefaultHardstopBlock/DefaultScheduleBlock are the
// all-null fallback fragments for a wizard page that never asks about
// these three objects — used both by PreservedObjectOrDefault (manual
// path, CurStepChanged) and by WritePreSeedConfig (Telegram path's partial
// pre-seed), so there's exactly one literal copy of each block rather than
// two that could drift apart.
function DefaultAlertsBlock(): String;
begin
  Result :=
    '{' + #13#10 +
    '    "downswing_buyins": null,' + #13#10 +
    '    "losing_streak_count": null,' + #13#10 +
    '    "daily_loss_cents": null,' + #13#10 +
    '    "spingo_daily_loss_cents": null,' + #13#10 +
    '    "session_loss_buyins": null' + #13#10 +
    '  }';
end;

function DefaultHardstopBlock(): String;
begin
  Result :=
    '{' + #13#10 +
    '    "session_loss_buyins": null,' + #13#10 +
    '    "consecutive_losses": null,' + #13#10 +
    '    "daily_loss_cents": null,' + #13#10 +
    '    "spingo_daily_loss_cents": null,' + #13#10 +
    '    "duration_hours": null,' + #13#10 +
    '    "downswing_buyins": null' + #13#10 +
    '  }';
end;

function DefaultScheduleBlock(): String;
begin
  Result :=
    '{' + #13#10 +
    '    "day_start_hour": null' + #13#10 +
    '  }';
end;

// Collector upgrades retain the complete existing configuration while replacing
// only fields collected by this wizard. Fresh installs leave server fields null.
function BuildClientConfigJSON(const Existing, ConnectionStringRaw, ScreenNameRaw, HandHistoryRaw, TournamentRaw: String): String;
begin
  Result := BuildConfigJSON(Existing,
    JSONFieldRaw(Existing, 'telegram_chat_id'),
    JSONFieldRaw(Existing, 'storage_backend'),
    JSONFieldRaw(Existing, 'google_sheet_name'),
    JSONFieldRaw(Existing, 'google_sheet_id'), ScreenNameRaw, HandHistoryRaw, TournamentRaw,
    PreservedObjectOrDefault(Existing, 'alerts', DefaultAlertsBlock()),
    PreservedObjectOrDefault(Existing, 'hardstop', DefaultHardstopBlock()),
    PreservedObjectOrDefault(Existing, 'schedule', DefaultScheduleBlock()), ConnectionStringRaw);
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

function XMLEscape(const S: String): String;
begin
  Result := S;
  StringChangeEx(Result, '&', '&amp;', True);
  StringChangeEx(Result, '<', '&lt;', True);
  StringChangeEx(Result, '>', '&gt;', True);
end;

// BuildTaskXML mirrors internal/installation.WindowsTaskXML, so
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
    '    <BootTrigger>' + #13#10 +
    '      <Enabled>true</Enabled>' + #13#10 +
    '      <Delay>PT30S</Delay>' + #13#10 +
    '    </BootTrigger>' + #13#10 +
    '    <TimeTrigger>' + #13#10 +
    '      <Repetition><Interval>PT1M</Interval><StopAtDurationEnd>false</StopAtDurationEnd></Repetition>' + #13#10 +
    '      <StartBoundary>' + GetDateTimeString('yyyy-mm-dd', '-', ':') + 'T' + GetDateTimeString('hh:nn:ss', '-', ':') + '</StartBoundary>' + #13#10 +
    '      <Enabled>true</Enabled>' + #13#10 +
    '    </TimeTrigger>' + #13#10 +
    '  </Triggers>' + #13#10 +
    '  <Principals>' + #13#10 +
    '    <Principal id="Author">' + #13#10 +
    '      <UserId>S-1-5-18</UserId>' + #13#10 +
    '      <RunLevel>HighestAvailable</RunLevel>' + #13#10 +
    '    </Principal>' + #13#10 +
    '  </Principals>' + #13#10 +
    '  <Settings>' + #13#10 +
    '    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>' + #13#10 +
    '    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>' + #13#10 +
    '    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>' + #13#10 +
    '    <AllowHardTerminate>true</AllowHardTerminate>' + #13#10 +
    '    <StartWhenAvailable>true</StartWhenAvailable>' + #13#10 +
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
    '    <RestartOnFailure><Interval>PT1M</Interval><Count>255</Count></RestartOnFailure>' + #13#10 +
    '  </Settings>' + #13#10 +
    '  <Actions Context="Author">' + #13#10 +
    '    <Exec>' + #13#10 +
    '      <Command>' + XMLEscape(ExePath) + '</Command>' + #13#10 +
    '      <Arguments>--serve</Arguments>' + #13#10 +
    '      <WorkingDirectory>' + XMLEscape(WorkingDir) + '</WorkingDirectory>' + #13#10 +
    '    </Exec>' + #13#10 +
    '  </Actions>' + #13#10 +
    '</Task>';
end;

// PreInstallFiles creates {app} and writes the credentials copy, plus a
// copy of tracker.exe itself — both needed before tracker.exe --setup can
// run (Telegram path, called mid-wizard) and again, harmlessly idempotent,
// during the real install step (both paths).
//
// tracker.exe resolves its three deployment paths (service account,
// config, lockfile) via os.Executable() — wherever the running binary's
// own file actually is, not the process's working directory (see
// cmd/tracker/main.go). That's correct for a normally-installed,
// long-running --serve, but it means RunTelegramSetup running --setup
// from a {tmp}-extracted copy would resolve those paths to {tmp}, not
// {app} — WorkingDirectory doesn't matter to os.Executable() at all. So
// tracker.exe has to actually *be* at {app} before it's ever run for
// setup, not just at {tmp} where ExtractTemporaryFile puts it. The real
// [Files]-driven copy to {app} doesn't happen until the Install step,
// which is after this — too late for RunTelegramSetup, called mid-wizard.
procedure PreInstallFiles();
begin
  StopInstallation();
  ForceDirectories(ExpandConstant('{app}'));
  // Sqlite installs never read or write a credentials file — no directory
  // for it either (CLAUDE.md: "the credentials file may not exist on a
  // sqlite-backend install"). UsingSQLite is already correct here whether
  // this run's PageBackend set it or it was seeded from an existing config.
  if not UsingSQLite then
  begin
    ForceDirectories(ExpandConstant('{app}\credentials'));
    // Manual path only — PageCredentials is skipped entirely for Telegram
    // (see ShouldSkipPage), so Values[0] would be empty here; that flow's
    // credentials come from bot.RunSetup's own upload-or-skip step instead.
    // On the manual path itself, PageCredentials is also skipped when
    // CredentialsAlreadySet — Values[0] is empty there too, and correctly
    // so: the file already exists at the destination, so there's nothing
    // to copy and no risk of FileCopy overwriting it with an empty source.
    if not UsingTelegram and (PageCredentials.Values[0] <> '') then
      FileCopy(PageCredentials.Values[0], ExpandConstant('{app}\credentials\google_service_account.json'), False);
  end;

  ExtractTemporaryFile(CombinedExeName());
  FileCopy(ExpandConstant('{tmp}\' + CombinedExeName()), ExpandConstant('{app}\tracker.exe'), True);
end;

// RegisterScheduledTask is the manual-setup path's equivalent of what
// tracker.exe --setup already does for itself on the Telegram path — see
// runSetup in cmd/tracker/main.go. Not fatal if either step fails: the
// rest of the install has already succeeded, so this only warns. Also
// starts the task immediately (schtasks /Run) the same way runSetup does
// — without this, the manual path left the player with a registered but
// not-yet-running service until the next logon/reboot, unlike the
// Telegram path, which starts it right away.
procedure RegisterScheduledTask();
var
  XMLPath, ExePath: String;
  ResultCode: Integer;
begin
  ExePath := ExpandConstant('{app}\tracker.exe');
  XMLPath := ExpandConstant('{tmp}\poker-tracker-task.xml');
  SaveStringToFile(XMLPath, UTF16Bytes(BuildTaskXML(ExePath, ExpandConstant('{app}'))), False);
  if not Exec('schtasks.exe', '/Create /TN "{#CombinedTaskName}" /XML "' + XMLPath + '" /F',
     '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    MsgBox('Could not register the Task Scheduler entry. You can set it up manually — see CLAUDE.md''s Windows Task Scheduler Setup section.', mbError, MB_OK);
  DeleteFile(XMLPath);

  if not Exec('schtasks.exe', '/Run /TN "{#CombinedTaskName}"',
     '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    MsgBox('Could not start the tracker service. Open Task Scheduler and run "Poker Tracker" manually, or reboot.', mbError, MB_OK);
end;

function RegisterTransferTask(): Boolean;
var
  XMLPath: String;
  ResultCode: Integer;
begin
  XMLPath := ExpandConstant('{tmp}\transfer-collector-task.xml');
  Result := SaveStringToFile(XMLPath, UTF16Bytes(BuildTaskXML(ExpandConstant('{app}\tracker.exe'), ExpandConstant('{app}'))), False);
  if Result then
    Result := Exec('schtasks.exe', '/Create /TN "{#CombinedTaskName}" /XML "' + XMLPath + '" /F', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) and (ResultCode = 0);
  DeleteFile(XMLPath);
end;

// WritePreSeedConfig runs on the Telegram path (RunTelegramSetup) right before
// tracker.exe --setup. It deliberately does NOT force storage_backend: with
// PageMethod now ahead of PageBackend, a Telegram install skips the native
// backend page (ShouldSkipPage) and bot.RunSetup asks the backend itself — it
// prompts whenever the field is null (internal/bot/setup.go). So this leaves
// storage_backend null on a fresh config and preserves whatever is on disk for
// an existing one. Existing bytes are left intact; the Go setup owns field
// updates and reads the latest progress on every retry. Only a fresh install
// needs a default stub before setup begins.
procedure WritePreSeedConfig();
begin
  if FileExists(ExpandConstant('{app}\config.json')) then
    Exit;
  if not SaveStringToFile(ExpandConstant('{app}\config.json'),
    BuildConfigJSON('', 'null', 'null', 'null', 'null', 'null', 'null', 'null',
      DefaultAlertsBlock(), DefaultHardstopBlock(), DefaultScheduleBlock(), 'null'), False) then
    RaiseException('Could not create this installation''s configuration.');
end;

// RunTelegramSetup runs tracker.exe --setup from {app} — PreInstallFiles
// has already copied it there, precisely so it resolves its own
// deployment paths against the real install directory rather than {tmp}
// — see PreInstallFiles' doc comment for why running it from {tmp} was
// wrong. The installer owns task registration/startup so recovery cannot
// reopen tracker.exe before the final file replacement.
// Release builds use -H=windowsgui, so tracker.exe never creates a console
// regardless of ShowCmd — SW_HIDE here just states that honestly; all of
// --setup's progress is visible through the Telegram conversation itself.
function RunTelegramSetup(): Boolean;
var
  ResultCode: Integer;
begin
  PreInstallFiles();
  WritePreSeedConfig();
  if not Exec(ExpandConstant('{app}\tracker.exe'), '--setup --installer-owns-task', ExpandConstant('{app}'),
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

// CurPageChanged fires after a page has actually been displayed (unlike
// NextButtonClick, which fires when the player tries to leave the
// *previous* page). PageWaiting's own text tells the player to go to
// Telegram and send a message "to begin" — before this hook existed, that
// was false: RunTelegramSetup only ran once NextButtonClick(PageWaiting.ID)
// fired, i.e. only after the player clicked Next on this same page, so any
// message sent on arrival just sat queued server-side until that click
// eventually happened. Running it here instead means --setup genuinely is
// listening by the time the page's own instructions say it is. On success,
// WizardForm.NextButton.OnClick(nil) is the documented way to simulate the
// player clicking Next themselves, advancing the wizard automatically
// instead of leaving them staring at a page that already finished its job.
procedure CurPageChanged(CurPageID: Integer);
begin
  if (CurPageID = PageWaiting.ID) and not TelegramSetupDone then
  begin
    // Displaying a page only queues its repaint (WM_PAINT), dispatched the
    // next time the message loop runs. RunTelegramSetup's Exec call blocks
    // that loop for the whole Telegram conversation, so without forcing
    // the paint now, the page would sit blank the entire time instead of
    // showing the "this window may look frozen" text above — worse than
    // before, when the block only started after a Next click on an
    // already-painted page.
    WizardForm.Update;
    if RunTelegramSetup() then
    begin
      TelegramSetupDone := True;
      WizardForm.NextButton.OnClick(nil);
    end;
    // RunTelegramSetup already showed a MsgBox on failure. Leave
    // TelegramSetupDone False and stay on the page — NextButtonClick's
    // PageWaiting branch becomes the player's manual retry.
  end;
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  JoinedPath: String;
begin
  Result := True;

  if CurPageID = PageBackend.ID then
    UsingSQLite := (PageBackend.SelectedValueIndex = 1)

  else if CurPageID = PageServerMethod.ID then
    // Index 0 = "From Telegram (recommended)": defer storage to Telegram, so
    // ShouldSkipPage hides the storage pages and RunServerSetup records SQLite.
    ServerUseTelegram := (PageServerMethod.SelectedValueIndex = 0)

  else if CurPageID = PageMethod.ID then
    UsingTelegram := (PageMethod.SelectedValueIndex = 0)

  else if CurPageID = wpReady then
  begin
    if not StageTransfer() then begin Result := False; Exit; end;
    StopInstallation();
  end

  else if CurPageID = PageConnection.ID then
  begin
    PageConnection.Values[0] := Trim(PageConnection.Values[0]);
    if (PageConnection.Values[0] = '') or (Copy(PageConnection.Values[0], 1, 10) <> 'tracker://') then
    begin
      MsgBox('Paste the full connection string your server printed — it starts with tracker://', mbError, MB_OK);
      Result := False;
    end;
  end

  else if CurPageID = PageCredentials.ID then
  begin
    if not FileExists(PageCredentials.Values[0]) then
    begin
      MsgBox('Select a valid google_service_account.json file.', mbError, MB_OK);
      Result := False;
    end;
  end

  else if CurPageID = PageWaiting.ID then
  begin
    // Normal path: CurPageChanged already ran RunTelegramSetup when this
    // page appeared and simulated this very click on success, so
    // TelegramSetupDone is already True here — just let the click through.
    // Manual-retry path: CurPageChanged's automatic attempt failed (its
    // own MsgBox already told the player), so TelegramSetupDone is still
    // False; a real click here means the player is retrying by hand.
    if TelegramSetupDone then
      Result := True
    else
    begin
      Result := RunTelegramSetup();
      if Result then
        TelegramSetupDone := True;
    end;
  end

  else if CurPageID = PageSheetName.ID then
  begin
    // Written back trimmed, matching resolveSheetInput's TrimSpace (Go) —
    // otherwise a trailing space would pass this check but still end up
    // saved untrimmed in CurStepChanged, unlike the Telegram flow.
    PageSheetName.Values[0] := Trim(PageSheetName.Values[0]);
    if PageSheetName.Values[0] = '' then
    begin
      MsgBox('Enter your Google Sheet name.', mbError, MB_OK);
      Result := False;
    end;
  end

  else if CurPageID = PageScreenName.ID then
  begin
    // Matches bot.RunSetup's screen name step exactly: trimmed before
    // both the emptiness check and being saved/used to join paths below.
    PageScreenName.Values[0] := Trim(PageScreenName.Values[0]);
    if PageScreenName.Values[0] = '' then
    begin
      MsgBox('Enter your PokerStars screen name.', mbError, MB_OK);
      Result := False;
    end;
  end

  else if CurPageID = PageHandHistoryDir.ID then
  begin
    // Matches bot.handlePathsReply exactly: DirExists on the base folder
    // alone isn't enough — any real, unrelated folder would pass that
    // check. The base only matters joined with the screen name, since
    // that's the actual folder PokerStars writes into.
    PageHandHistoryDir.Values[0] := Trim(PageHandHistoryDir.Values[0]);
    JoinedPath := AddBackslash(PageHandHistoryDir.Values[0]) + CurrentScreenName();
    if not DirExists(JoinedPath) then
    begin
      MsgBox('That path doesn''t exist: ' + JoinedPath + #13#10 + 'Select the folder again.', mbError, MB_OK);
      Result := False;
    end;
  end

  else if CurPageID = PageTournamentDir.ID then
  begin
    PageTournamentDir.Values[0] := Trim(PageTournamentDir.Values[0]);
    JoinedPath := AddBackslash(PageTournamentDir.Values[0]) + CurrentScreenName();
    if not DirExists(JoinedPath) then
    begin
      MsgBox('That path doesn''t exist: ' + JoinedPath + #13#10 + 'Select the folder again.', mbError, MB_OK);
      Result := False;
    end;
  end;
end;

// RunServerSetup finishes a run-a-server install: tracker-server.exe --setup
// generates the TLS certificate + pairing token and records the storage backend
// (passed as --backend/--sheet-name/--sheet-id, with the service-account key
// copied for Sheets, since the hidden-console --setup can't prompt). When the
// player chose "Configure from Telegram" on PageServerMethod, that's --backend
// sqlite — the working default they configure from Telegram after pairing; when
// they chose "Set storage here", it's their PageBackend selection. --install
// registers the PokerTrackerServer boot task; schtasks /Run starts it now; then
// --pair's connection string (printed to stdout, captured via a cmd redirect) is
// shown so the player can pair their poker PC. Alerts and rules are set later
// from Telegram.
procedure RunServerSetup();
var
  ExePath, ConnPath, Conn, SetupArgs, SheetInput, SheetName, SheetID: String;
  ResultCode: Integer;
begin
  ExePath := ExpandConstant('{app}\tracker-server.exe');

  // Pass the storage choice the wizard collected to --setup, which owns
  // config.json and preserves the pairing token. Only when this run actually
  // asked — a re-install with a backend already recorded keeps it untouched,
  // regardless of the PageServerMethod answer.
  SetupArgs := '--setup';
  if not BackendAlreadySet then
  begin
    if ServerUseTelegram or UsingSQLite then
      // "Configure from Telegram" defers storage/alerts/rules to the bot and
      // records the working SQLite default now (switchable later via /storage);
      // an explicit "set storage here -> Local file" also lands on SQLite.
      SetupArgs := SetupArgs + ' --backend sqlite'
    else
    begin
      SetupArgs := SetupArgs + ' --backend sheets';
      SheetInput := PageSheetName.Values[0];
      SheetID := ExtractSheetIDFromURL(SheetInput);
      if SheetID <> '' then
        SheetName := ''
      else
        SheetName := SheetInput;
      if SheetName <> '' then
        SetupArgs := SetupArgs + ' --sheet-name "' + SheetName + '"';
      if SheetID <> '' then
        SetupArgs := SetupArgs + ' --sheet-id "' + SheetID + '"';
      // Copy the service-account key so the server can reach the sheet at runtime.
      ForceDirectories(ExpandConstant('{app}\credentials'));
      if PageCredentials.Values[0] <> '' then
        FileCopy(PageCredentials.Values[0], ExpandConstant('{app}\credentials\google_service_account.json'), False);
    end;
  end;

  if (not Exec(ExePath, SetupArgs, ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ResultCode)) or (ResultCode <> 0) then
  begin
    MsgBox('The server did not finish first-time setup. Open a command prompt in ' +
      ExpandConstant('{app}') + ' and run: tracker-server.exe --setup', mbError, MB_OK);
    Exit;
  end;
  if (not Exec(ExePath, '--install', ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ResultCode)) or (ResultCode <> 0) then
    MsgBox('Could not register the server boot task. Run tracker-server.exe --install from an elevated prompt.', mbError, MB_OK)
  else
    Exec('schtasks.exe', '/Run /TN "{#ServerTaskName}"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);

  ConnPath := ExpandConstant('{tmp}\server-conn.txt');
  if Exec(ExpandConstant('{cmd}'), '/C ""' + ExePath + '" --pair > "' + ConnPath + '""',
     ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ResultCode) then
  begin
    Conn := ReadFileAsString(ConnPath);
    DeleteFile(ConnPath);
  end;
  if Conn <> '' then
    SuppressibleMsgBox('Your Poker Tracker server is installed and running.' + #13#10 + #13#10 +
      'Pair your poker PC by choosing "Connect to a server" in its installer and pasting this:'
      + #13#10 + #13#10 + Conn, mbInformation, MB_OK, IDOK)
  else
    SuppressibleMsgBox('Your Poker Tracker server is installed and running. To get the pairing line, run ' +
      'tracker-server.exe --pair in ' + ExpandConstant('{app}') + '.', mbInformation, MB_OK, IDOK);
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  SheetInput, SheetName, SheetID: String;
  TelegramChatIDRaw, StorageBackendRaw, SheetNameRaw, SheetIDRaw, ScreenNameRaw: String;
  HandHistoryRaw, TournamentRaw, AlertsBlock, HardstopBlock, ScheduleBlock: String;
  ResultCode: Integer;
begin
  if CurStep = ssInstall then
  begin
    if not StageTransfer() then RaiseException('Transfer did not finish; the original installation was kept.');
    StopInstallation();
  end;
  if CurStep = ssPostInstall then
  begin
    ExistingConfigJSON := ReadFileAsString(ExpandConstant('{app}\config.json'));
    // Connect-to-server: the client binary is already installed as tracker.exe
    // ([Files] Check: IsClientMode). Write its config (connection string +
    // paths + screen name; everything the server owns stays null) and register
    // the same scheduled task, which runs the client's own --serve. No
    // PreInstallFiles (no combined binary to extract, no credentials) and no
    // Telegram setup.
    if IsClientMode() then
    begin
      SaveStringToFile(ExpandConstant('{app}\config.json'),
        BuildClientConfigJSON(ExistingConfigJSON,
          '"' + JSONEscape(PageConnection.Values[0]) + '"',
          '"' + JSONEscape(PageScreenName.Values[0]) + '"',
          '"' + JSONEscape(PageHandHistoryDir.Values[0]) + '"',
          '"' + JSONEscape(PageTournamentDir.Values[0]) + '"'),
        False);
      if TransferStaged then
      begin
        if not RegisterTransferTask() then
          RaiseException('Collector task registration failed; retry transfer to recover.');
        if not Exec('schtasks.exe', '/Run /TN "{#CombinedTaskName}"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
          RaiseException('Collector start failed; retry this transfer.');
        if not RunTransferStep('--transfer-activate') then
          RaiseException('Destination activation is unconfirmed; retry this transfer.');
        if not RunTransferStep('--transfer-complete') then
          RaiseException('Transfer completion is unconfirmed; retry this transfer.');
        TransferFinished := True;
      end
      else RegisterScheduledTask();
      Exit;
    end;
    // Run-a-server: the server binary is installed as tracker-server.exe
    // ([Files] Check: InstallServer*). Finish its setup, register + start its
    // boot task, and show the connection string. No combined binary, no
    // Telegram wizard, no credentials.
    if IsServerMode() then
    begin
      RunServerSetup();
      Exit;
    end;
    // Idempotent for the Telegram path (already done once in
    // RunTelegramSetup); the only time it matters is manual setup, where
    // this is the first and only time it runs.
    PreInstallFiles();
    if UsingTelegram and not TelegramSetupDone then
    begin
      if not RunTelegramSetup() then
        RaiseException('Telegram setup did not finish. Existing installation files have been kept.');
      TelegramSetupDone := True;
    end;
    if not UsingTelegram then
    begin
      // Never asked by this wizard — always carried over from the
      // existing file if there is one (a fresh chat ID is auto-detected
      // at runtime by --serve; see CLAUDE.md).
      TelegramChatIDRaw := JSONFieldRaw(ExistingConfigJSON, 'telegram_chat_id');

      // BackendAlreadySet preserves the existing value verbatim; otherwise
      // UsingSQLite (set by NextButtonClick(PageBackend.ID) above) is
      // this run's own answer.
      if BackendAlreadySet then
        StorageBackendRaw := JSONFieldRaw(ExistingConfigJSON, 'storage_backend')
      else if UsingSQLite then
        StorageBackendRaw := '"sqlite"'
      else
        StorageBackendRaw := '"sheets"';

      if SheetAlreadySet then
      begin
        SheetNameRaw := JSONFieldRaw(ExistingConfigJSON, 'google_sheet_name');
        SheetIDRaw := JSONFieldRaw(ExistingConfigJSON, 'google_sheet_id');
      end
      else
      begin
        SheetInput := PageSheetName.Values[0];
        SheetID := ExtractSheetIDFromURL(SheetInput);
        if SheetID <> '' then
          SheetName := ''
        else
          SheetName := SheetInput;
        SheetNameRaw := JSONStringOrNull(SheetName);
        SheetIDRaw := JSONStringOrNull(SheetID);
      end;

      if ScreenNameAlreadySet then
        ScreenNameRaw := JSONFieldRaw(ExistingConfigJSON, 'pokerstars_screen_name')
      else
        ScreenNameRaw := '"' + JSONEscape(PageScreenName.Values[0]) + '"';

      if HandHistoryAlreadySet then
        HandHistoryRaw := JSONFieldRaw(ExistingConfigJSON, 'hand_history')
      else
        HandHistoryRaw := '"' + JSONEscape(PageHandHistoryDir.Values[0]) + '"';

      if TournamentAlreadySet then
        TournamentRaw := JSONFieldRaw(ExistingConfigJSON, 'tournament_summary')
      else
        TournamentRaw := '"' + JSONEscape(PageTournamentDir.Values[0]) + '"';

      // Never asked by this wizard at all — always preserved verbatim if
      // an existing config has them, else the same null block as before.
      AlertsBlock := PreservedObjectOrDefault(ExistingConfigJSON, 'alerts', DefaultAlertsBlock());
      HardstopBlock := PreservedObjectOrDefault(ExistingConfigJSON, 'hardstop', DefaultHardstopBlock());
      ScheduleBlock := PreservedObjectOrDefault(ExistingConfigJSON, 'schedule', DefaultScheduleBlock());

      SaveStringToFile(ExpandConstant('{app}\config.json'),
        BuildConfigJSON(ExistingConfigJSON, TelegramChatIDRaw, StorageBackendRaw, SheetNameRaw, SheetIDRaw, ScreenNameRaw,
          HandHistoryRaw, TournamentRaw, AlertsBlock, HardstopBlock, ScheduleBlock,
          JSONFieldRaw(ExistingConfigJSON, 'connection_string')),
        False);
      RegisterScheduledTask();
    end
    else
      RegisterScheduledTask();
  end;
end;

// Stop before deleting any files, including on silent uninstall. Player data
// keeps the existing manual-versus-silent uninstall policy.
procedure DeletePlayerState(const Name: String);
begin
  DeleteFile(ExpandConstant('{app}\') + Name);
  DeleteFile(ExpandConstant('{app}\') + Name + '.tmp');
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usUninstall then
    StopInstallation();
  if (CurUninstallStep = usUninstall) and not UninstallSilent() then
  begin
    DeletePlayerState('config.json');
    DeletePlayerState('lockfile.json');
    DeletePlayerState('pending_tournaments.json');
    DeletePlayerState('sheets_fallback.json');
    DeletePlayerState('pending_cash.json');
    DeletePlayerState('hardstop_deliveries.json');
    DeletePlayerState('collector_settings.json');
    DeletePlayerState('collector_observation.json');
    DeletePlayerState('collector_clock.json');
    DeletePlayerState('collector_closures.json');
    DeletePlayerState('closure_notifications.json');
    DeletePlayerState('source_transfer.json');
    DeletePlayerState('source_transfer_request.json');
    DeletePlayerState('deployment_transfer.json');
    DeleteFile(ExpandConstant('{app}\deployment_transfer.data'));
    DeleteFile(ExpandConstant('{app}\transfer-error.txt'));
    DeleteFile(ExpandConstant('{app}\tracker-transfer.exe'));
    DelTree(ExpandConstant('{app}\tracker-source-*.exe'), False, True, False);
    DeleteFile(ExpandConstant('{app}\tracker.db'));
    // Server role's TLS material (absent on combined/client installs).
    DeleteFile(ExpandConstant('{app}\server_cert.pem'));
    DeleteFile(ExpandConstant('{app}\server_key.pem'));
  end;
end;

procedure DeinitializeSetup();
var
  ResultCode: Integer;
begin
  if TransferStaged and not TransferFinished then
  begin
    if Exec('schtasks.exe', '/Change /TN "{#CombinedTaskName}" /DISABLE', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) and (ResultCode = 0) then
    begin
      RunTransferStep('--transfer-rollback');
      Exec('schtasks.exe', '/Change /TN "{#CombinedTaskName}" /ENABLE', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
      Exec('schtasks.exe', '/Run /TN "{#CombinedTaskName}"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
    end;
  end;
  if not TransferStaged or TransferFinished then
  begin
    RestoreScheduledRecovery('{#CombinedTaskName}', CombinedTaskSuspended);
    RestoreScheduledRecovery('{#ServerTaskName}', ServerTaskSuspended);
  end;
end;

function GetCustomSetupExitCode(): Integer;
begin
  Result := 0;
  if TransferStaged and not TransferFinished then Result := 1;
end;
