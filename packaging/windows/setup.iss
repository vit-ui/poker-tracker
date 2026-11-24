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
#define CombinedUpdateRecoveryTaskName CombinedTaskName + " Update Recovery"
#define ServerUpdateRecoveryTaskName ServerTaskName + " Update Recovery"

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
Source: "tracker-amd64.exe"; DestDir: "{app}"; DestName: "tracker.exe"; Flags: ignoreversion; Check: InstallCombinedAmd64
Source: "tracker-arm64.exe"; DestDir: "{app}"; DestName: "tracker.exe"; Flags: ignoreversion; Check: InstallCombinedArm64
Source: "tracker-amd64.exe"; DestDir: "{tmp}"; Flags: dontcopy
Source: "tracker-arm64.exe"; DestDir: "{tmp}"; Flags: dontcopy
Source: "tracker-client-amd64.exe"; DestDir: "{app}"; DestName: "tracker.exe"; Flags: ignoreversion; Check: InstallClientAmd64
Source: "tracker-client-arm64.exe"; DestDir: "{app}"; DestName: "tracker.exe"; Flags: ignoreversion; Check: InstallClientArm64
Source: "tracker-server-windows-amd64.exe"; DestDir: "{app}"; DestName: "tracker-server.exe"; Flags: ignoreversion; Check: InstallServerAmd64
Source: "tracker-server-windows-arm64.exe"; DestDir: "{app}"; DestName: "tracker-server.exe"; Flags: ignoreversion; Check: InstallServerArm64
Source: "tracker-server-windows-amd64.exe"; DestDir: "{tmp}"; Flags: dontcopy
Source: "tracker-server-windows-arm64.exe"; DestDir: "{tmp}"; Flags: dontcopy

Source: "tracker-client-amd64.exe"; DestDir: "{tmp}"; Flags: dontcopy
Source: "tracker-client-arm64.exe"; DestDir: "{tmp}"; Flags: dontcopy

[UninstallDelete]
Type: dirifempty; Name: "{app}\credentials"
Type: dirifempty; Name: "{app}"

[UninstallRun]
Filename: "schtasks.exe"; Parameters: "/Delete /TN ""{#CombinedTaskName}"" /F"; Flags: runhidden; RunOnceId: "DeleteTask"
Filename: "schtasks.exe"; Parameters: "/Delete /TN ""{#ServerTaskName}"" /F"; Flags: runhidden; RunOnceId: "DeleteServerTask"

[Code]
var
  UpdateExtraction: Boolean;
  PageBackend: TInputOptionWizardPage;
  PageMethod: TInputOptionWizardPage;
  PageCredentials: TInputFileWizardPage;
  PageWaiting: TOutputMsgWizardPage;
  PageSheetName: TInputQueryWizardPage;
  PageScreenName: TInputQueryWizardPage;
  PageHandHistoryDir: TInputDirWizardPage;
  PageTournamentDir: TInputDirWizardPage;
  PageRole: TInputOptionWizardPage;
  PageUpgrade: TInputOptionWizardPage;
  ExistingRole: Integer;
  ExistingComplete: Boolean;
  PageConnection: TInputQueryWizardPage;
  PageTransfer: TOutputMsgWizardPage;
  ExistingCombined, ExistingRoleUnknown, IncompleteTransfer: Boolean;
  TransferStaged, TransferFinished: Boolean;
  TransferConnection: String;
  CombinedTaskSuspended, ServerTaskSuspended: Boolean;
  PageServerMethod: TInputOptionWizardPage;
  ServerUseTelegram: Boolean;
  UsingTelegram: Boolean;
  UsingSQLite: Boolean;
  TelegramSetupDone: Boolean;
  ExistingConfigJSON: String;
  BackendAlreadySet: Boolean;
  SheetAlreadySet: Boolean;
  ScreenNameAlreadySet: Boolean;
  HandHistoryAlreadySet: Boolean;
  TournamentAlreadySet: Boolean;
  CredentialsAlreadySet: Boolean;
  AllFieldsAlreadySet: Boolean;

function IsClientMode(): Boolean;
begin
  Result := Assigned(PageRole) and (PageRole.SelectedValueIndex = 1);
end;

function IsServerMode(): Boolean;
begin
  Result := Assigned(PageRole) and (PageRole.SelectedValueIndex = 2);
end;

function IsCombinedMode(): Boolean;
begin
  Result := not IsClientMode() and not IsServerMode();
end;

function KeepExistingSettings(): Boolean;
begin
  Result := (ExistingRole >= 0) and (PageUpgrade.SelectedValueIndex = 0) and
    (PageRole.SelectedValueIndex = ExistingRole) and not IncompleteTransfer;
end;

function UpgradeOnly(): Boolean;
begin
  Result := KeepExistingSettings() and ExistingComplete;
end;

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

function CombinedExeName(): String;
begin
  if IsARM64 then
    Result := 'tracker-arm64.exe'
  else
    Result := 'tracker-amd64.exe';
end;

function ReadFileAsString(const Path: String): String;
var
  Content: AnsiString;
begin
  if not LoadStringFromFile(Path, Content) then
    Result := ''
  else
    Result := Content;
end;

function JSONUnescape(const S: String): String;
var
  R: String;
begin
  R := S;
  StringChangeEx(R, '\"', '"', True);
  StringChangeEx(R, '\\', '\', True);
  Result := R;
end;

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

function JSONFieldRaw(const JSON, FieldName: String): String;
begin
  if JSONFieldIsSet(JSON, FieldName) then
    Result := JSONExtractRawValue(JSON, FieldName)
  else
    Result := 'null';
end;

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

function CurrentScreenName(): String;
begin
  if ScreenNameAlreadySet and KeepExistingSettings() then
    Result := JSONExtractStringValue(ExistingConfigJSON, 'pokerstars_screen_name')
  else
    Result := PageScreenName.Values[0];
end;

procedure ApplyTestAnswers();
var
  Role, ServerMethod, Backend, Creds, Sheet, Answer: String;
begin
  Role := ExpandConstant('{param:TESTROLE|}');
  if Role <> '' then PageUpgrade.SelectedValueIndex := 1;
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

procedure CancelTerminalUpdateRecovery();
var
  Scheduler, Folder, Tasks, Task: Variant;
  I: Integer;
begin
  Scheduler := CreateOleObject('Schedule.Service');
  Scheduler.Connect();
  Folder := Scheduler.GetFolder('\');
  Tasks := Folder.GetTasks(1);
  for I := Tasks.Count downto 1 do
  begin
    Task := Tasks.Item(I);
    if (Task.Name = '{#CombinedUpdateRecoveryTaskName}') or
       (Task.Name = '{#ServerUpdateRecoveryTaskName}') then
    begin
      Task.Enabled := False;
      Task.Stop(0);
      Folder.DeleteTask(Task.Name, 0);
    end;
  end;
  DeleteFile(ExpandConstant('{app}\update_recovery.json'));
  DeleteFile(ExpandConstant('{app}\update_recovery.json.tmp'));
  DeleteFile(ExpandConstant('{app}\.tracker-update-recovery.exe'));
  DeleteFile(ExpandConstant('{app}\.tracker-update-recovery.exe.tmp'));
end;

procedure StopInstallation();
var
  HelperPath, ErrorText, Details, LogPath: String;
  ResultCode, I: Integer;
  Started: Boolean;
  Output: TExecOutput;
begin
  CancelTerminalUpdateRecovery();
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
  ErrorText := '';
  Started := False;
  try
    Started := ExecAndCaptureOutput(HelperPath, '--stop-installation "' + ExpandConstant('{app}') + '"',
      '', SW_HIDE, ewWaitUntilTerminated, ResultCode, Output);
  except
    ErrorText := 'Could not run the tracker shutdown helper: ' + GetExceptionMessage();
  end;
  if Started and (ResultCode = 0) then Exit;
  if ErrorText = '' then
  begin
    if Started then
      ErrorText := 'Tracker shutdown failed (exit code ' + IntToStr(ResultCode) + ').'
    else
      ErrorText := 'Could not start the tracker shutdown helper.' + #13#10 +
        'Windows error ' + IntToStr(ResultCode) + ': ' + SysErrorMessage(ResultCode);
  end;
  Details := '';
  for I := 0 to GetArrayLength(Output.StdErr) - 1 do
    Details := Details + Output.StdErr[I] + #13#10;
  if Trim(Details) = '' then
    for I := 0 to GetArrayLength(Output.StdOut) - 1 do
      Details := Details + Output.StdOut[I] + #13#10;
  if Trim(Details) <> '' then
    ErrorText := ErrorText + #13#10 + Trim(Details)
  else if Started then
    ErrorText := ErrorText + #13#10 + 'The helper returned no error details.';
  if Output.Error then
    ErrorText := ErrorText + #13#10 + 'Some helper output could not be captured.';
  Log(ErrorText);
  LogPath := AddBackslash(GetTempDir()) + 'PokerTracker-shutdown-error.log';
  if SaveStringToFile(LogPath, ErrorText + #13#10 +
    'Installation folder: ' + ExpandConstant('{app}') + #13#10 +
    'Shutdown helper: ' + HelperPath + #13#10, False) then
    ErrorText := ErrorText + #13#10 + #13#10 + 'Details saved to: ' + LogPath
  else
    ErrorText := ErrorText + #13#10 + #13#10 + 'Could not save the shutdown error log to: ' + LogPath;
  RaiseException(ErrorText);
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

function InitializeSetup(): Boolean;
var
  Payload, OutputPath: String;
begin
  Payload := ExpandConstant('{param:UPDATEFILE|}');
  OutputPath := ExpandConstant('{param:UPDATEOUT|}');
  UpdateExtraction := (Payload <> '') or (OutputPath <> '');
  Result := not UpdateExtraction;
  if not UpdateExtraction then Exit;
  if ((Payload <> 'tracker-amd64.exe') and (Payload <> 'tracker-arm64.exe') and
      (Payload <> 'tracker-client-amd64.exe') and (Payload <> 'tracker-client-arm64.exe') and
      (Payload <> 'tracker-server-windows-amd64.exe') and (Payload <> 'tracker-server-windows-arm64.exe')) or
     (OutputPath = '') then
    RaiseException('Invalid update extraction request.');
  ExtractTemporaryFile(Payload);
  if not FileCopy(ExpandConstant('{tmp}\') + Payload, OutputPath, True) then
    RaiseException('Could not extract update payload.');
end;

procedure RejectExistingInstallation(const Message: String);
begin
  SuppressibleMsgBox(Message, mbError, MB_OK, IDOK);
  Abort;
end;

procedure InitializeWizard();
var
  MethodDescription, BackendDescription, RoleOutput, RoleText, TransferState, ExistingExe, ConfigFile, Helper: String;
  RoleResult: Integer;
begin
  ExistingRole := -1;
  ConfigFile := AddBackslash(WizardDirValue) + 'config.json';
  if FileExists(ConfigFile) then
  begin
    if IsARM64 then Helper := 'tracker-client-arm64.exe'
    else Helper := 'tracker-client-amd64.exe';
    ExtractTemporaryFile(Helper);
    Helper := ExpandConstant('{tmp}\') + Helper;
    if not Exec(Helper, '--validate-config "' + ConfigFile + '"', '', SW_HIDE, ewWaitUntilTerminated, RoleResult) or (RoleResult <> 0) then
      RejectExistingInstallation('Cannot read the existing configuration in ' + WizardDirValue + '. Repair it before running setup; no installation changes were made.');
  end;
  ExistingConfigJSON := ReadFileAsString(ConfigFile);
  RoleOutput := ExpandConstant('{tmp}\existing-tracker-role.txt');
  ExistingExe := AddBackslash(WizardDirValue) + 'tracker.exe';
  if FileExists(AddBackslash(WizardDirValue) + 'tracker-server.exe') then
  begin
    if FileExists(ExistingExe) then
      RejectExistingInstallation('Both tracker.exe and tracker-server.exe exist in ' + WizardDirValue + '. Resolve the conflicting installation roles before upgrading.');
    ExistingExe := AddBackslash(WizardDirValue) + 'tracker-server.exe';
  end;
  if FileExists(ExistingExe) then
  begin
    if Exec(ExistingExe, '--role --role-output "' + RoleOutput + '"', '', SW_HIDE, ewWaitUntilTerminated, RoleResult) and (RoleResult = 0) then
    begin
      RoleText := Trim(ReadFileAsString(RoleOutput));
      if RoleText = 'combined' then ExistingRole := 0
      else if RoleText = 'client' then ExistingRole := 1
      else if RoleText = 'server' then ExistingRole := 2;
      ExistingCombined := (RoleText = 'combined');
      ExistingRoleUnknown := (ExistingRole < 0) or
        ((ExistingRole = 2) <> (ExtractFileName(ExistingExe) = 'tracker-server.exe'));
    end
    else ExistingRoleUnknown := True;
    DeleteFile(RoleOutput);
    if ExistingRoleUnknown then
      RejectExistingInstallation('Cannot identify the existing executable role in ' + WizardDirValue + '. Setup has not changed the installation.');
  end;
  if (ExistingRole < 0) and FileExists(ConfigFile) then
    RejectExistingInstallation('An existing configuration was found without its executable in ' + WizardDirValue + '. Restore the executable before upgrading so setup can identify its role.');
  TransferState := ReadFileAsString(AddBackslash(WizardDirValue) + 'source_transfer.json');
  RoleText := JSONExtractStringValue(TransferState, 'phase');
  IncompleteTransfer := (RoleText = 'preparing') or (RoleText = 'prepared') or (RoleText = 'stopping');

  BackendAlreadySet := JSONFieldIsSet(ExistingConfigJSON, 'storage_backend');
  if BackendAlreadySet then
    UsingSQLite := JSONExtractStringValue(ExistingConfigJSON, 'storage_backend') = 'sqlite';
  SheetAlreadySet := JSONFieldIsSet(ExistingConfigJSON, 'google_sheet_name') or
                      JSONFieldIsSet(ExistingConfigJSON, 'google_sheet_id');
  ScreenNameAlreadySet := JSONExtractStringValue(ExistingConfigJSON, 'pokerstars_screen_name') <> '';
  HandHistoryAlreadySet := JSONExtractStringValue(ExistingConfigJSON, 'hand_history') <> '';
  TournamentAlreadySet := JSONExtractStringValue(ExistingConfigJSON, 'tournament_summary') <> '';
  CredentialsAlreadySet := FileExists(AddBackslash(WizardDirValue) + 'credentials\google_service_account.json');
  AllFieldsAlreadySet := BackendAlreadySet and ScreenNameAlreadySet and HandHistoryAlreadySet and
                          TournamentAlreadySet and (UsingSQLite or (SheetAlreadySet and CredentialsAlreadySet));
  if ExistingRole = 0 then ExistingComplete := AllFieldsAlreadySet
  else if ExistingRole = 1 then
    ExistingComplete := ScreenNameAlreadySet and HandHistoryAlreadySet and TournamentAlreadySet and
      (JSONExtractStringValue(ExistingConfigJSON, 'connection_string') <> '')
  else if ExistingRole = 2 then
    ExistingComplete := BackendAlreadySet and (UsingSQLite or (SheetAlreadySet and CredentialsAlreadySet)) and
      (JSONExtractStringValue(ExistingConfigJSON, 'pair_token') <> '') and
      FileExists(AddBackslash(WizardDirValue) + 'server_cert.pem') and
      FileExists(AddBackslash(WizardDirValue) + 'server_key.pem');

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
    BackendDescription := 'An existing installation was found without a storage backend recorded. Choose ' +
      'how Poker Tracker should store your tracked sessions going forward.';
  end;

  PageUpgrade := CreateInputOptionPage(wpWelcome, 'Existing installation',
    'Upgrade Poker Tracker',
    'Installation folder: ' + WizardDirValue + #13#10 +
    'Upgrade keeps your existing role, configuration and history. Missing settings will be requested if setup is incomplete.', True, False);
  PageUpgrade.Add('Upgrade existing installation (recommended)');
  PageUpgrade.Add('Change configuration or role');
  PageUpgrade.SelectedValueIndex := 0;

  PageRole := CreateInputOptionPage(PageUpgrade.ID,
    'Setup Type',
    'How will you run Poker Tracker?',
    'Run everything on this PC unless you are using the optional two-machine split. In the split, the poker PC runs "Connect to a server" and a separate always-on machine runs "Run a server on this PC".',
    True, False);
  PageRole.Add('Run everything on this PC (recommended)');
  PageRole.Add('Connect to a server (this PC is the poker PC)');
  PageRole.Add('Run a server on this PC (stores data, runs the bot)');
  PageRole.SelectedValueIndex := 0;
  if ExistingRole >= 0 then
  begin
    PageRole.SelectedValueIndex := ExistingRole;
    if ExistingRole = 0 then RoleText := 'All-local'
    else if ExistingRole = 1 then RoleText := 'Collector (connects to a server)'
    else RoleText := 'Server';
    PageUpgrade.SubCaptionLabel.Caption := 'Detected role: ' + RoleText + #13#10 +
      'Installation folder: ' + WizardDirValue + #13#10 +
      'Upgrade keeps your configuration and history. Missing settings will be requested if setup is incomplete.';
  end;

  PageConnection := CreateInputQueryPage(PageRole.ID,
    'Connect to a Server',
    'Paste the connection string your server printed',
    'Your server printed a line starting with tracker:// when you ran its setup. Paste the whole line.');
  PageConnection.Add('Connection string:', False);
  PageConnection.Values[0] := JSONExtractStringValue(ExistingConfigJSON, 'connection_string');

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
  if PageID = PageUpgrade.ID then
  begin Result := (ExistingRole < 0) or IncompleteTransfer; Exit; end;
  if PageID = PageRole.ID then
  begin Result := KeepExistingSettings(); Exit; end;
  if UpgradeOnly() then
  begin
    Result := (PageID = PageConnection.ID) or (PageID = PageServerMethod.ID) or
      (PageID = PageMethod.ID) or (PageID = PageBackend.ID) or
      (PageID = PageCredentials.ID) or (PageID = PageWaiting.ID) or
      (PageID = PageSheetName.ID) or (PageID = PageScreenName.ID) or
      (PageID = PageHandHistoryDir.ID) or (PageID = PageTournamentDir.ID) or
      (PageID = PageTransfer.ID);
    Exit;
  end;
  if PageID = PageTransfer.ID then begin Result := not NeedsTransfer(); Exit; end;
  if IsServerMode() then
  begin
    Result := (PageID = PageConnection.ID) or (PageID = PageMethod.ID) or
              (PageID = PageWaiting.ID) or (PageID = PageScreenName.ID) or
              (PageID = PageHandHistoryDir.ID) or (PageID = PageTournamentDir.ID);
    if (PageID = PageBackend.ID) and (BackendAlreadySet or ServerUseTelegram) then
      Result := True;
    if (PageID = PageCredentials.ID) and (UsingSQLite or CredentialsAlreadySet or ServerUseTelegram) then
      Result := True;
    if (PageID = PageSheetName.ID) and (UsingSQLite or SheetAlreadySet or ServerUseTelegram) then
      Result := True;
    Exit;
  end;
  if IsClientMode() then
  begin
    Result := (PageID = PageBackend.ID) or (PageID = PageMethod.ID) or
              (PageID = PageCredentials.ID) or (PageID = PageWaiting.ID) or
              (PageID = PageSheetName.ID) or (PageID = PageServerMethod.ID);
    if KeepExistingSettings() then
      Result := Result or
        ((PageID = PageConnection.ID) and (PageConnection.Values[0] <> '')) or
        ((PageID = PageScreenName.ID) and ScreenNameAlreadySet) or
        ((PageID = PageHandHistoryDir.ID) and HandHistoryAlreadySet) or
        ((PageID = PageTournamentDir.ID) and TournamentAlreadySet);
    Exit;
  end;
  if (PageID = PageConnection.ID) or (PageID = PageServerMethod.ID) then
  begin
    Result := True;
    Exit;
  end;
  if (PageID = PageBackend.ID) and (BackendAlreadySet or UsingTelegram) then
  begin
    Result := True;
    Exit;
  end;
  if (PageID = PageMethod.ID) and AllFieldsAlreadySet and KeepExistingSettings() then
  begin
    Result := True;
    Exit;
  end;
  if UsingTelegram then
  begin
    if (PageID = PageCredentials.ID) or (PageID = PageSheetName.ID) or (PageID = PageScreenName.ID) or
       (PageID = PageHandHistoryDir.ID) or (PageID = PageTournamentDir.ID) then
      Result := True;
  end
  else
  begin
    if PageID = PageWaiting.ID then
      Result := True
    else if (PageID = PageCredentials.ID) and (CredentialsAlreadySet or UsingSQLite) then
      Result := True
    else if (PageID = PageSheetName.ID) and (SheetAlreadySet or UsingSQLite) then
      Result := True
    else if (PageID = PageScreenName.ID) and ScreenNameAlreadySet and KeepExistingSettings() then
      Result := True
    else if (PageID = PageHandHistoryDir.ID) and HandHistoryAlreadySet and KeepExistingSettings() then
      Result := True
    else if (PageID = PageTournamentDir.ID) and TournamentAlreadySet and KeepExistingSettings() then
      Result := True;
  end;
end;

function JSONEscape(const S: String): String;
var
  R: String;
begin
  R := S;
  StringChangeEx(R, '\', '\\', True);
  StringChangeEx(R, '"', '\"', True);
  Result := R;
end;

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

function JSONStringOrNull(const S: String): String;
begin
  if S = '' then
    Result := 'null'
  else
    Result := '"' + JSONEscape(S) + '"';
end;

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

procedure PreInstallFiles();
begin
  StopInstallation();
  ForceDirectories(ExpandConstant('{app}'));
  if not UsingSQLite then
  begin
    ForceDirectories(ExpandConstant('{app}\credentials'));
    if not UsingTelegram and (PageCredentials.Values[0] <> '') then
      FileCopy(PageCredentials.Values[0], ExpandConstant('{app}\credentials\google_service_account.json'), False);
  end;

  ExtractTemporaryFile(CombinedExeName());
  FileCopy(ExpandConstant('{tmp}\' + CombinedExeName()), ExpandConstant('{app}\tracker.exe'), True);
end;

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

procedure WritePreSeedConfig();
begin
  if FileExists(ExpandConstant('{app}\config.json')) then
    Exit;
  if not SaveStringToFile(ExpandConstant('{app}\config.json'),
    BuildConfigJSON('', 'null', 'null', 'null', 'null', 'null', 'null', 'null',
      DefaultAlertsBlock(), DefaultHardstopBlock(), DefaultScheduleBlock(), 'null'), False) then
    RaiseException('Could not create this installation''s configuration.');
end;

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

procedure CurPageChanged(CurPageID: Integer);
begin
  if (CurPageID = PageWaiting.ID) and not TelegramSetupDone then
  begin
    WizardForm.Update;
    if RunTelegramSetup() then
    begin
      TelegramSetupDone := True;
      WizardForm.NextButton.OnClick(nil);
    end;
  end;
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  JoinedPath: String;
begin
  Result := True;

  if CurPageID = PageUpgrade.ID then
  begin
    if PageUpgrade.SelectedValueIndex = 0 then
    begin
      PageRole.SelectedValueIndex := ExistingRole;
      UsingTelegram := False;
      ServerUseTelegram := False;
    end;
  end
  else if CurPageID = PageBackend.ID then
    UsingSQLite := (PageBackend.SelectedValueIndex = 1)

  else if CurPageID = PageServerMethod.ID then
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
    PageSheetName.Values[0] := Trim(PageSheetName.Values[0]);
    if PageSheetName.Values[0] = '' then
    begin
      MsgBox('Enter your Google Sheet name.', mbError, MB_OK);
      Result := False;
    end;
  end

  else if CurPageID = PageScreenName.ID then
  begin
    PageScreenName.Values[0] := Trim(PageScreenName.Values[0]);
    if PageScreenName.Values[0] = '' then
    begin
      MsgBox('Enter your PokerStars screen name.', mbError, MB_OK);
      Result := False;
    end;
  end

  else if CurPageID = PageHandHistoryDir.ID then
  begin
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

function RunServerSetup(): Boolean;
var
  ExePath, ConnPath, Conn, SetupArgs, SheetInput, SheetName, SheetID: String;
  ResultCode: Integer;
begin
  Result := False;
  ExePath := ExpandConstant('{app}\tracker-server.exe');

  SetupArgs := '--setup';
  if not BackendAlreadySet then
  begin
    if ServerUseTelegram or UsingSQLite then
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
  begin
    MsgBox('Could not register the server boot task. Run tracker-server.exe --install from an elevated prompt.', mbError, MB_OK);
    Exit;
  end;
  if not Exec('schtasks.exe', '/Run /TN "{#ServerTaskName}"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
  begin
    MsgBox('Could not start the server boot task. Check Task Scheduler and tracker.log.', mbError, MB_OK);
    Exit;
  end;
  Result := True;

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

procedure FinishTelegramSetup(const ExeName: String);
var
  ExePath, PendingMessage: String;
  ResultCode: Integer;
begin
  ExePath := ExpandConstant('{app}\') + ExeName;
  PendingMessage := 'Tracker is installed. Telegram setup is pending or could not be checked.' + #13#10 +
    'Open ' + ExePath + ' to finish Connect Telegram.';
  if WizardSilent then
  begin
    if not Exec(ExePath, '--telegram-status', ExpandConstant('{app}'), SW_HIDE,
      ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
      Log(PendingMessage);
  end
  else if not Exec(ExePath, '--connect-telegram-if-needed', ExpandConstant('{app}'), SW_SHOW,
    ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    SuppressibleMsgBox(PendingMessage, mbInformation, MB_OK, IDOK);
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
    if UpgradeOnly() then
    begin
      if IsServerMode() then
      begin
        if not Exec(ExpandConstant('{app}\tracker-server.exe'), '--install', ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
          RaiseException('Could not register the upgraded server task.');
        if not Exec('schtasks.exe', '/Run /TN "{#ServerTaskName}"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
          RaiseException('Could not start the upgraded server task.');
      end
      else RegisterScheduledTask();
      if IsServerMode() then
        FinishTelegramSetup('tracker-server.exe')
      else if not IsClientMode() then
        FinishTelegramSetup('tracker.exe');
      Exit;
    end;
    ExistingConfigJSON := ReadFileAsString(ExpandConstant('{app}\config.json'));
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
    if IsServerMode() then
    begin
      if RunServerSetup() then
        FinishTelegramSetup('tracker-server.exe');
      Exit;
    end;
    PreInstallFiles();
    if UsingTelegram and not TelegramSetupDone then
    begin
      if not RunTelegramSetup() then
        RaiseException('Telegram setup did not finish. Existing installation files have been kept.');
      TelegramSetupDone := True;
    end;
    if not UsingTelegram then
    begin
      TelegramChatIDRaw := JSONFieldRaw(ExistingConfigJSON, 'telegram_chat_id');

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

      if ScreenNameAlreadySet and KeepExistingSettings() then
        ScreenNameRaw := JSONFieldRaw(ExistingConfigJSON, 'pokerstars_screen_name')
      else
        ScreenNameRaw := '"' + JSONEscape(PageScreenName.Values[0]) + '"';

      if HandHistoryAlreadySet and KeepExistingSettings() then
        HandHistoryRaw := JSONFieldRaw(ExistingConfigJSON, 'hand_history')
      else
        HandHistoryRaw := '"' + JSONEscape(PageHandHistoryDir.Values[0]) + '"';

      if TournamentAlreadySet and KeepExistingSettings() then
        TournamentRaw := JSONFieldRaw(ExistingConfigJSON, 'tournament_summary')
      else
        TournamentRaw := '"' + JSONEscape(PageTournamentDir.Values[0]) + '"';

      AlertsBlock := PreservedObjectOrDefault(ExistingConfigJSON, 'alerts', DefaultAlertsBlock());
      HardstopBlock := PreservedObjectOrDefault(ExistingConfigJSON, 'hardstop', DefaultHardstopBlock());
      ScheduleBlock := PreservedObjectOrDefault(ExistingConfigJSON, 'schedule', DefaultScheduleBlock());

      SaveStringToFile(ExpandConstant('{app}\config.json'),
        BuildConfigJSON(ExistingConfigJSON, TelegramChatIDRaw, StorageBackendRaw, SheetNameRaw, SheetIDRaw, ScreenNameRaw,
          HandHistoryRaw, TournamentRaw, AlertsBlock, HardstopBlock, ScheduleBlock,
          JSONFieldRaw(ExistingConfigJSON, 'connection_string')),
        False);
      if not Exec(ExpandConstant('{app}\tracker.exe'), '--resolve-storage', ExpandConstant('{app}'),
        SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
      begin
        if ExistingConfigJSON = '' then
          DeleteFile(ExpandConstant('{app}\config.json'))
        else
          SaveStringToFile(ExpandConstant('{app}\config.json'), ExistingConfigJSON, False);
        if not CredentialsAlreadySet then
          DeleteFile(ExpandConstant('{app}\credentials\google_service_account.json'));
        RaiseException('Storage setup is incomplete. Check the spreadsheet and service-account key, then rerun setup.');
      end;
      RegisterScheduledTask();
    end
    else
      RegisterScheduledTask();
    FinishTelegramSetup('tracker.exe');
  end;
end;

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
    DeleteFile(ExpandConstant('{app}\credentials\telegram.token'));
    DeleteFile(ExpandConstant('{app}\credentials\google_service_account.json'));
    DeletePlayerState('lockfile.json');
    DeletePlayerState('pending_tournaments.json');
    DeletePlayerState('sheets_fallback.json');
    DeletePlayerState('pending_cash.json');
    DelTree(ExpandConstant('{app}\pending-cash-history'), True, True, False);
    DeletePlayerState('hardstop_deliveries.json');
    DeletePlayerState('collector_settings.json');
    DeletePlayerState('collector_observation.json');
    DeletePlayerState('collector_clock.json');
    DeletePlayerState('update_startup.json');
    DeletePlayerState('requested_update.json');
    DeletePlayerState('collector_updates.json');
    DeletePlayerState('update_notices.json');
    DelTree(ExpandConstant('{app}\.tracker-previous-*'), False, True, False);
    DelTree(ExpandConstant('{app}\.tracker-update-*'), False, True, False);
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
    DelTree(ExpandConstant('{app}\storage-backups'), True, True, False);
    DeleteFile(ExpandConstant('{app}\server_cert.pem'));
    DeleteFile(ExpandConstant('{app}\server_key.pem'));
  end;
end;

procedure DeinitializeSetup();
var
  ResultCode: Integer;
begin
  if UpdateExtraction then Exit;
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
