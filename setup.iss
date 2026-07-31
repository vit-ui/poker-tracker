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

[Setup]
AppId={{5D7656F7-7CCB-4B00-8694-954DDE2986AE}
AppName=Poker Tracker
AppVersion={#AppVersion}
DefaultDirName=C:\poker-tracker
DisableDirPage=yes
DefaultGroupName=Poker Tracker
DisableProgramGroupPage=yes
DisableWelcomePage=no
PrivilegesRequired=admin
ArchitecturesAllowed=x64 arm64
ArchitecturesInstallIn64BitMode=x64 arm64
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
; C:\poker-tracker\tracker.exe, and main.go's self-taskkill, the Task Scheduler
; XML, and CurUninstallStepChanged's taskkill all assume it. The server role runs
; as its own tracker-server.exe (its own PokerTrackerServer task, see below).
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
; config/credentials paths from its own executable location. Combined only.
Source: "tracker-amd64.exe"; DestDir: "{tmp}"; Flags: dontcopy; Check: InstallCombinedAmd64
Source: "tracker-arm64.exe"; DestDir: "{tmp}"; Flags: dontcopy; Check: InstallCombinedArm64
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
; Must run before file deletion below, or a running --serve process holds
; tracker.exe open and the uninstaller can't remove it. [UninstallRun]
; entries execute before files are deleted by default, and top-to-bottom
; within the section, so this only needs to be listed first.
Filename: "taskkill.exe"; Parameters: "/IM tracker.exe /F"; Flags: runhidden; RunOnceId: "KillTracker"
Filename: "schtasks.exe"; Parameters: "/Delete /TN ""Poker Tracker"" /F"; Flags: runhidden; RunOnceId: "DeleteTask"
; Server-role cleanup — its own exe name and task. Harmless no-ops on a combined
; or client install (nothing by that name exists); a failed schtasks /Delete is
; ignored, same as the combined task's entry above.
Filename: "taskkill.exe"; Parameters: "/IM tracker-server.exe /F"; Flags: runhidden; RunOnceId: "KillServer"
Filename: "schtasks.exe"; Parameters: "/Delete /TN ""PokerTrackerServer"" /F"; Flags: runhidden; RunOnceId: "DeleteServerTask"

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
  // Split (v3.7+): the role choice (all-local vs connect-to-server) and the
  // connection-string page shown only in connect-to-server mode. All the
  // pages above are the all-local flow, unchanged.
  PageRole: TInputOptionWizardPage;
  PageConnection: TInputQueryWizardPage;
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
  // Raw text of {app}\config.json, read once at wizard startup — '' means
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
  if ScreenNameAlreadySet then
    Result := JSONExtractStringValue(ExistingConfigJSON, 'pokerstars_screen_name')
  else
    Result := PageScreenName.Values[0];
end;

procedure InitializeWizard();
var
  MethodDescription, BackendDescription: String;
begin
  // Read before {app} is valid for ExpandConstant (see the comment on the
  // page-anchoring below) — hardcoded to match DefaultDirName ([Setup]
  // above) rather than expanded, since this runs before the wizard's
  // internal position has passed wpSelectDir. Done first, before any page
  // is created, so ExistingConfigJSON is available to build MethodDescription
  // below and to ShouldSkipPage/CurStepChanged once the wizard is running.
  ExistingConfigJSON := ReadFileAsString('C:\poker-tracker\config.json');
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
  CredentialsAlreadySet := FileExists('C:\poker-tracker\credentials\google_service_account.json');
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
  // PageBackend goes first — before PageMethod (Telegram vs manual) — since
  // the choice applies identically to both paths: it decides whether the
  // Telegram conversation's own backend question (bot.RunSetup) is
  // pre-answered via WritePreSeedConfig, and whether the manual path's
  // PageCredentials/PageSheetName pages are shown at all (ShouldSkipPage).
  // PageMethod anchors off PageBackend.ID rather than wpSelectDir directly,
  // so it inherits the same {app}-validity position one page later.
  // Role choice, shown first: all-local (both roles in one process, the
  // default, everything below) vs connect-to-server (this PC is the poker PC
  // and a separate always-on machine runs the server). Anchored at wpWelcome —
  // it collects no {app}-dependent value, so it needn't sit past wpSelectDir
  // like the pages below. In client mode ShouldSkipPage hides every all-local /
  // Google / Telegram page and shows PageConnection + screen name + folders.
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

  PageBackend := CreateInputOptionPage(wpSelectDir,
    'Storage Backend',
    'How do you want to store your tracked poker sessions?',
    BackendDescription,
    True, False);
  PageBackend.Add('Google Sheets');
  PageBackend.Add('Local file (no Google account needed)');
  PageBackend.SelectedValueIndex := 0;

  PageMethod := CreateInputOptionPage(PageBackend.ID,
    'Setup Method',
    'Would you like to set up via Telegram? (Recommended)',
    MethodDescription,
    True, False);
  PageMethod.Add('Yes, use Telegram');
  PageMethod.Add('No, set up manually');
  PageMethod.SelectedValueIndex := 0;

  // Manual setup only — skipped entirely for Telegram (see ShouldSkipPage),
  // since --setup has its own optional upload-or-skip step for this file.
  PageCredentials := CreateInputFilePage(PageMethod.ID,
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

  PageHandHistoryDir := CreateInputDirPage(PageScreenName.ID,
    'Hand History Folder',
    'Select the base Hand History folder',
    'Do NOT browse into your screen name''s subfolder underneath it — that part is added automatically.',
    False, '');
  PageHandHistoryDir.Add('Parent HandHistory folder:');

  PageTournamentDir := CreateInputDirPage(PageHandHistoryDir.ID,
    'Tournament Summary Folder',
    'Select the base TournamentSummaries folder',
    'Do NOT browse into your screen name''s subfolder underneath it — that part is added automatically.',
    False, '');
  PageTournamentDir.Add('Parent TournamentSummaries folder:');
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := False;
  // Run-a-server mode asks nothing in the wizard — the certificate, pairing
  // token, storage default, and the connection string are all handled by
  // tracker-server --setup after install (CurStepChanged). Skip every page.
  if IsServerMode() then
  begin
    Result := (PageID = PageConnection.ID) or (PageID = PageBackend.ID) or
              (PageID = PageMethod.ID) or (PageID = PageCredentials.ID) or
              (PageID = PageWaiting.ID) or (PageID = PageSheetName.ID) or
              (PageID = PageScreenName.ID) or (PageID = PageHandHistoryDir.ID) or
              (PageID = PageTournamentDir.ID);
    Exit;
  end;
  // Connect-to-server mode shows only the connection string, screen name, and
  // the two folder pages; every all-local / Google / Telegram page is skipped.
  if IsClientMode() then
  begin
    Result := (PageID = PageBackend.ID) or (PageID = PageMethod.ID) or
              (PageID = PageCredentials.ID) or (PageID = PageWaiting.ID) or
              (PageID = PageSheetName.ID);
    Exit;
  end;
  // All-local mode never shows the connection-string page.
  if PageID = PageConnection.ID then
  begin
    Result := True;
    Exit;
  end;
  // Checked first and unconditionally — the backend choice doesn't depend
  // on Telegram vs manual (UsingTelegram isn't set yet at this point
  // anyway), only on whether a prior install already recorded one.
  if (PageID = PageBackend.ID) and BackendAlreadySet then
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
// AlertsBlock/HardstopBlock/ScheduleBlock are always preserved-or-default
// this way, never collected — no wizard page asks about them.
// StorageBackendRaw sits second, matching config.Config's field order
// (internal/config/config.go: telegram_chat_id, storage_backend,
// google_sheet_id, google_sheet_name, pokerstars_screen_name).
function BuildConfigJSON(const TelegramChatIDRaw, StorageBackendRaw, SheetNameRaw, SheetIDRaw, ScreenNameRaw,
  HandHistoryRaw, TournamentRaw, AlertsBlock, HardstopBlock, ScheduleBlock: String): String;
begin
  Result :=
    '{' + #13#10 +
    '  "setup": {' + #13#10 +
    '    "telegram_chat_id": ' + TelegramChatIDRaw + ',' + #13#10 +
    '    "storage_backend": ' + StorageBackendRaw + ',' + #13#10 +
    '    "google_sheet_id": ' + SheetIDRaw + ',' + #13#10 +
    '    "google_sheet_name": ' + SheetNameRaw + ',' + #13#10 +
    '    "pokerstars_screen_name": ' + ScreenNameRaw + #13#10 +
    '  },' + #13#10 +
    '  "paths": {' + #13#10 +
    '    "hand_history": ' + HandHistoryRaw + ',' + #13#10 +
    '    "tournament_summary": ' + TournamentRaw + #13#10 +
    '  },' + #13#10 +
    '  "alerts": ' + AlertsBlock + ',' + #13#10 +
    '  "hardstop": ' + HardstopBlock + ',' + #13#10 +
    '  "schedule": ' + ScheduleBlock + #13#10 +
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
    '    "daily_loss_usd": null,' + #13#10 +
    '    "spingo_daily_loss_usd": null,' + #13#10 +
    '    "session_loss_buyins": null' + #13#10 +
    '  }';
end;

function DefaultHardstopBlock(): String;
begin
  Result :=
    '{' + #13#10 +
    '    "session_loss_buyins": null,' + #13#10 +
    '    "consecutive_losses": null,' + #13#10 +
    '    "daily_loss_usd": null,' + #13#10 +
    '    "spingo_daily_loss_usd": null,' + #13#10 +
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

// BuildClientConfigJSON writes the split client's config.json: the connection
// string, screen name, and the two base folders it needs, with everything the
// server owns (telegram, storage backend, sheet, pair token) left null. Alerts,
// hard stop, and schedule are the same all-null default blocks — the client
// enforces the locks the server decides but configures none of them itself.
function BuildClientConfigJSON(const ConnectionStringRaw, ScreenNameRaw, HandHistoryRaw, TournamentRaw: String): String;
begin
  Result :=
    '{' + #13#10 +
    '  "setup": {' + #13#10 +
    '    "telegram_chat_id": null,' + #13#10 +
    '    "storage_backend": null,' + #13#10 +
    '    "google_sheet_id": null,' + #13#10 +
    '    "google_sheet_name": null,' + #13#10 +
    '    "pokerstars_screen_name": ' + ScreenNameRaw + ',' + #13#10 +
    '    "connection_string": ' + ConnectionStringRaw + ',' + #13#10 +
    '    "pair_token": null' + #13#10 +
    '  },' + #13#10 +
    '  "paths": {' + #13#10 +
    '    "hand_history": ' + HandHistoryRaw + ',' + #13#10 +
    '    "tournament_summary": ' + TournamentRaw + #13#10 +
    '  },' + #13#10 +
    '  "alerts": ' + DefaultAlertsBlock() + ',' + #13#10 +
    '  "hardstop": ' + DefaultHardstopBlock() + ',' + #13#10 +
    '  "schedule": ' + DefaultScheduleBlock() + #13#10 +
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
    '    <BootTrigger>' + #13#10 +
    '      <Enabled>true</Enabled>' + #13#10 +
    '      <Delay>PT30S</Delay>' + #13#10 +
    '    </BootTrigger>' + #13#10 +
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
var
  ResultCode: Integer;
begin
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

  // Kill any previously-running tracker.exe before this install's task
  // gets (re)started below — MultipleInstancesPolicy=IgnoreNew
  // (BuildTaskXML) means schtasks /Run silently no-ops if an old
  // instance is still alive, leaving it running with stale config
  // forever instead of picking up what this install just wrote. Safe to
  // run unconditionally: this installer process is never itself named
  // tracker.exe (unlike cmd/tracker/main.go's runSetup, which needs a
  // PID filter to avoid killing itself). Result ignored — "nothing to
  // kill" is the expected, common case on a first install.
  Exec('taskkill.exe', '/F /IM tracker.exe', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
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
  if not Exec('schtasks.exe', '/Create /TN "Poker Tracker" /XML "' + XMLPath + '" /F',
     '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    MsgBox('Could not register the Task Scheduler entry. You can set it up manually — see CLAUDE.md''s Windows Task Scheduler Setup section.', mbError, MB_OK);
  DeleteFile(XMLPath);

  if not Exec('schtasks.exe', '/Run /TN "Poker Tracker"',
     '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    MsgBox('Could not start the tracker service. Open Task Scheduler and run "Poker Tracker" manually, or reboot.', mbError, MB_OK);
end;

// WritePreSeedConfig ensures {app}\config.json has setup.storage_backend
// set to PageBackend's choice before --setup ever runs. This is what lets
// bot.RunSetup's own backend question (internal/bot/bot.go, "Step 1.5") see
// the field already set and skip its inline keyboard entirely — the player
// answers the storage-backend question once, on this native page, not a
// second time inside the Telegram conversation. Two cases:
//
// - config.json doesn't exist yet (fresh install): write the same
//   all-null-except-backend stub as before.
// - config.json already exists: rebuild it field by field via the same
//   JSONFieldRaw/PreservedObjectOrDefault/BuildConfigJSON combination
//   CurStepChanged's manual path uses, splicing every field back byte-for-
//   byte except storage_backend, which is always set to Backend. Covers a
//   real upgrade (a working v1.x sheet/screen-name/paths setup, or a v2.0
//   config missing only the backend field) without touching anything the
//   player already configured, and also a retried RunTelegramSetup
//   (CurPageChanged's automatic attempt failed, the player clicks Next on
//   PageWaiting again) — by then --setup's first, failed run may already
//   have written real progress (chat ID, sheet, screen name).
//
// Reads config.json fresh off disk (Existing) rather than the global
// ExistingConfigJSON, which is a one-time snapshot taken before the wizard
// even started and would miss exactly the retried-setup case above.
procedure WritePreSeedConfig();
var
  Backend, Existing: String;
  TelegramChatIDRaw, SheetNameRaw, SheetIDRaw, ScreenNameRaw: String;
  HandHistoryRaw, TournamentRaw, AlertsBlock, HardstopBlock, ScheduleBlock: String;
begin
  if UsingSQLite then
    Backend := '"sqlite"'
  else
    Backend := '"sheets"';

  Existing := ReadFileAsString(ExpandConstant('{app}\config.json'));
  if Existing = '' then
  begin
    SaveStringToFile(ExpandConstant('{app}\config.json'),
      BuildConfigJSON('null', Backend, 'null', 'null', 'null', 'null', 'null',
        DefaultAlertsBlock(), DefaultHardstopBlock(), DefaultScheduleBlock()),
      False);
    Exit;
  end;

  TelegramChatIDRaw := JSONFieldRaw(Existing, 'telegram_chat_id');
  SheetNameRaw := JSONFieldRaw(Existing, 'google_sheet_name');
  SheetIDRaw := JSONFieldRaw(Existing, 'google_sheet_id');
  ScreenNameRaw := JSONFieldRaw(Existing, 'pokerstars_screen_name');
  HandHistoryRaw := JSONFieldRaw(Existing, 'hand_history');
  TournamentRaw := JSONFieldRaw(Existing, 'tournament_summary');
  AlertsBlock := PreservedObjectOrDefault(Existing, 'alerts', DefaultAlertsBlock());
  HardstopBlock := PreservedObjectOrDefault(Existing, 'hardstop', DefaultHardstopBlock());
  ScheduleBlock := PreservedObjectOrDefault(Existing, 'schedule', DefaultScheduleBlock());

  SaveStringToFile(ExpandConstant('{app}\config.json'),
    BuildConfigJSON(TelegramChatIDRaw, Backend, SheetNameRaw, SheetIDRaw, ScreenNameRaw,
      HandHistoryRaw, TournamentRaw, AlertsBlock, HardstopBlock, ScheduleBlock),
    False);
end;

// RunTelegramSetup runs tracker.exe --setup from {app} — PreInstallFiles
// has already copied it there, precisely so it resolves its own
// deployment paths against the real install directory rather than {tmp}
// — see PreInstallFiles' doc comment for why running it from {tmp} was
// wrong. --setup handles config.json, the Task Scheduler entry, and
// starting the service itself — see bot.RunSetup and runSetup in main.go.
// Release builds use -H=windowsgui, so tracker.exe never creates a console
// regardless of ShowCmd — SW_HIDE here just states that honestly; all of
// --setup's progress is visible through the Telegram conversation itself.
function RunTelegramSetup(): Boolean;
var
  ResultCode: Integer;
begin
  PreInstallFiles();
  WritePreSeedConfig();
  if not Exec(ExpandConstant('{app}\tracker.exe'), '--setup', ExpandConstant('{app}'),
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
  ResultCode: Integer;
begin
  Result := True;

  if CurPageID = PageBackend.ID then
    UsingSQLite := (PageBackend.SelectedValueIndex = 1)

  else if CurPageID = PageMethod.ID then
    UsingTelegram := (PageMethod.SelectedValueIndex = 0)

  else if CurPageID = wpReady then
    // Leaving the Ready page means the player just clicked Install. This
    // runs before Inno's own Restart-Manager-based "close applications"
    // scan (part of the Preparing-to-install step right after this),
    // which is what actually produces "Setup was unable to automatically
    // close all applications" if it finds tracker.exe still running —
    // e.g. the fresh --serve that tracker.exe --setup's own last step
    // started via schtasks /Run during PageWaiting, well before this
    // point. A taskkill placed later (CurStepChanged's ssInstall, tried
    // before) runs after that scan already happened, too late to prevent
    // the prompt. Safe to run unconditionally, same as PreInstallFiles'
    // own taskkill — this installer process is never itself named
    // tracker.exe.
    Exec('taskkill.exe', '/F /IM tracker.exe', '', SW_HIDE, ewWaitUntilTerminated, ResultCode)

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
// generates the TLS certificate + pairing token and writes a sqlite-default
// config; --install registers the PokerTrackerServer boot task; schtasks /Run
// starts it now; then --pair's connection string (printed to stdout, captured
// via cmd redirect) is shown so the player can pair their poker PC. The server
// takes no wizard input — storage, alerts, and rules are set later from Telegram.
procedure RunServerSetup();
var
  ExePath, ConnPath, Conn: String;
  ResultCode: Integer;
begin
  ExePath := ExpandConstant('{app}\tracker-server.exe');
  if (not Exec(ExePath, '--setup', ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ResultCode)) or (ResultCode <> 0) then
  begin
    MsgBox('The server did not finish first-time setup. Open a command prompt in ' +
      ExpandConstant('{app}') + ' and run: tracker-server.exe --setup', mbError, MB_OK);
    Exit;
  end;
  if (not Exec(ExePath, '--install', ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ResultCode)) or (ResultCode <> 0) then
    MsgBox('Could not register the server boot task. Run tracker-server.exe --install from an elevated prompt.', mbError, MB_OK)
  else
    Exec('schtasks.exe', '/Run /TN "PokerTrackerServer"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);

  ConnPath := ExpandConstant('{tmp}\server-conn.txt');
  if Exec(ExpandConstant('{cmd}'), '/C ""' + ExePath + '" --pair > "' + ConnPath + '""',
     ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ResultCode) then
  begin
    Conn := ReadFileAsString(ConnPath);
    DeleteFile(ConnPath);
  end;
  if Conn <> '' then
    MsgBox('Your Poker Tracker server is installed and running.' + #13#10 + #13#10 +
      'Pair your poker PC by choosing "Connect to a server" in its installer and pasting this:' +
      #13#10 + #13#10 + Conn, mbInformation, MB_OK)
  else
    MsgBox('Your Poker Tracker server is installed and running. To get the pairing line, run ' +
      'tracker-server.exe --pair in ' + ExpandConstant('{app}') + '.', mbInformation, MB_OK);
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  SheetInput, SheetName, SheetID: String;
  TelegramChatIDRaw, StorageBackendRaw, SheetNameRaw, SheetIDRaw, ScreenNameRaw: String;
  HandHistoryRaw, TournamentRaw, AlertsBlock, HardstopBlock, ScheduleBlock: String;
  ResultCode: Integer;
begin
  if CurStep = ssPostInstall then
  begin
    // Connect-to-server: the client binary is already installed as tracker.exe
    // ([Files] Check: IsClientMode). Write its config (connection string +
    // paths + screen name; everything the server owns stays null) and register
    // the same scheduled task, which runs the client's own --serve. No
    // PreInstallFiles (no combined binary to extract, no credentials) and no
    // Telegram setup.
    if IsClientMode() then
    begin
      SaveStringToFile(ExpandConstant('{app}\config.json'),
        BuildClientConfigJSON(
          '"' + JSONEscape(PageConnection.Values[0]) + '"',
          '"' + JSONEscape(PageScreenName.Values[0]) + '"',
          '"' + JSONEscape(PageHandHistoryDir.Values[0]) + '"',
          '"' + JSONEscape(PageTournamentDir.Values[0]) + '"'),
        False);
      RegisterScheduledTask();
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
        BuildConfigJSON(TelegramChatIDRaw, StorageBackendRaw, SheetNameRaw, SheetIDRaw, ScreenNameRaw,
          HandHistoryRaw, TournamentRaw, AlertsBlock, HardstopBlock, ScheduleBlock),
        False);
      RegisterScheduledTask();
    end
    else
    begin
      // The Telegram path's task already exists — tracker.exe --setup
      // registered it itself (see runSetup in cmd/tracker/main.go) and
      // started it running. But NextButtonClick(wpReady) then killed
      // that process (to avoid Inno's "unable to close applications"
      // prompt during file copy), so by this point the task is
      // registered but not running. Only a restart is needed, not the
      // full RegisterScheduledTask the manual path uses above.
      if not Exec('schtasks.exe', '/Run /TN "Poker Tracker"',
         '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
        MsgBox('Could not start the tracker service. Open Task Scheduler and run "Poker Tracker" manually, or reboot.', mbError, MB_OK);
    end;
  end;
end;

// CurUninstallStepChanged deletes the player-data files not covered by
// [UninstallDelete] — see the comment there. UninstallSilent() is False
// for a normal, interactive Control Panel uninstall and True when the
// uninstaller is invoked with /SILENT or /VERYSILENT, which is how a
// chained "remove the old version before installing the new one" step
// would run it were one ever added — this keeps that hypothetical future
// path from wiping settings too, not just today's plain in-place upgrade
// (which never runs this uninstaller at all). tracker.db (sqlite backend
// only — DeleteFile on a nonexistent path is a silent no-op on the sheets
// backend) and sheets_fallback.json (the mirror case: sheets backend only,
// holding writes that failed against the Sheets API for later replay)
// follow the exact same rule as the three JSON files: deleted on a real
// manual uninstall, preserved across an in-place upgrade.
//
// The explicit taskkill below is required, not redundant with
// [UninstallRun]'s own taskkill entry: CurUninstallStepChanged(usUninstall)
// fires before [UninstallRun] executes, not after — a real gap found live
// (tracker.db survived a manual uninstall while the three JSON files
// didn't). config.json/lockfile.json/pending_tournaments.json only ever
// hold a file handle open for the length of one config.go read or write
// (SaveConfig does an atomic tmp-then-rename), so they're never actually
// locked when this runs regardless of process state. tracker.db is
// different: a live --serve process keeps its sqlite connection — and the
// file handle under it — open for the process's entire lifetime, so
// DeleteFile silently fails against it if tracker.exe hasn't been killed
// yet. Same pattern as PreInstallFiles' and NextButtonClick(wpReady)'s own
// inline taskkill calls elsewhere in this file — this installer process is
// never itself named tracker.exe, so it's safe to run unconditionally.
procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  ResultCode: Integer;
begin
  if (CurUninstallStep = usUninstall) and not UninstallSilent() then
  begin
    Exec('taskkill.exe', '/F /IM tracker.exe', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
    // Also stop the server role's process (its own exe name); no-op otherwise.
    Exec('taskkill.exe', '/F /IM tracker-server.exe', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
    DeleteFile(ExpandConstant('{app}\config.json'));
    DeleteFile(ExpandConstant('{app}\lockfile.json'));
    DeleteFile(ExpandConstant('{app}\pending_tournaments.json'));
    DeleteFile(ExpandConstant('{app}\sheets_fallback.json'));
    DeleteFile(ExpandConstant('{app}\tracker.db'));
    // Server role's TLS material (absent on combined/client installs).
    DeleteFile(ExpandConstant('{app}\server_cert.pem'));
    DeleteFile(ExpandConstant('{app}\server_key.pem'));
  end;
end;
