; Inno Setup script for the gpsync Windows installer.
;
; Built by .github/workflows/release.yml, which passes the version in:
;
;   ISCC.exe /DAppVersion=0.1.0 /O.\dist packaging\gpsync.iss
;
; It expects dist\gpsync.exe and dist\gpsync-tray.exe to exist already (the
; release workflow cross-compiles them on Linux first; `make dist` does the
; same locally).
;
; Installs per-user by default, so no administrator prompt is needed and the
; autostart entry it can create lands in the same user's registry.

#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif
; VersionInfoVersion goes into the PE version resource and must be numeric,
; so a pre-release tag like v0.2.0-rc1 cannot be used for it directly.
#ifndef NumericVersion
  #define NumericVersion "0.0.0"
#endif

#define AppName "gpsync"
#define AppPublisher "Gabriel Mizrahi"
#define AppURL "https://github.com/gmizrahi/gpsync"

[Setup]
AppId={{4C8F2E7A-1D3B-4A96-9C2E-7B5F3A8D6E10}
AppName={#AppName}
AppVersion={#AppVersion}
AppPublisher={#AppPublisher}
AppPublisherURL={#AppURL}
AppSupportURL={#AppURL}/issues
AppUpdatesURL={#AppURL}/releases
VersionInfoVersion={#NumericVersion}

; Per-user install: no UAC prompt, and autostart writes to this user's HKCU.
PrivilegesRequired=lowest
DefaultDirName={autopf}\{#AppName}
DefaultGroupName={#AppName}
DisableProgramGroupPage=yes
AllowNoIcons=yes

LicenseFile=..\LICENSE
OutputBaseFilename=gpsync-setup-{#AppVersion}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible

; Adding to PATH changes the environment for new processes.
ChangesEnvironment=yes

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "addtopath"; Description: "Add gpsync to PATH, so it can be run from any terminal"; GroupDescription: "Command line:"
Name: "autostart"; Description: "Start the tray app when I sign in"; GroupDescription: "Startup:"
Name: "desktopicon"; Description: "Create a desktop shortcut for the tray app"; GroupDescription: "Shortcuts:"; Flags: unchecked

[Files]
Source: "..\dist\gpsync.exe";      DestDir: "{app}"; Flags: ignoreversion
Source: "..\dist\gpsync-tray.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\README.md";            DestDir: "{app}"; Flags: ignoreversion isreadme
Source: "..\LICENSE";              DestDir: "{app}"; Flags: ignoreversion
Source: "..\CHANGELOG.md";         DestDir: "{app}"; Flags: ignoreversion
Source: "..\docs\*";               DestDir: "{app}\docs"; Flags: ignoreversion recursesubdirs

[Icons]
Name: "{group}\gpsync tray";       Filename: "{app}\gpsync-tray.exe"
Name: "{group}\gpsync on GitHub";  Filename: "{#AppURL}"
Name: "{group}\Uninstall gpsync";  Filename: "{uninstallexe}"
Name: "{autodesktop}\gpsync tray"; Filename: "{app}\gpsync-tray.exe"; Tasks: desktopicon

[Registry]
; Appending to the user's PATH. Inno only writes this when the check below
; confirms the directory is not already there, so repeated installs cannot
; grow PATH without bound.
Root: HKCU; Subkey: "Environment"; ValueType: expandsz; ValueName: "Path"; \
    ValueData: "{olddata};{app}"; Tasks: addtopath; Check: NeedsPathEntry(ExpandConstant('{app}'))

[Run]
; The tray app owns its own autostart entry (gpsync-tray --install-autostart
; writes HKCU\...\Run), so the installer delegates rather than writing a
; second, competing entry that could drift out of step with the tray's own
; "Starts with Windows" toggle.
Filename: "{app}\gpsync-tray.exe"; Parameters: "--install-autostart"; \
    Flags: runhidden; Tasks: autostart; StatusMsg: "Enabling start at sign-in..."
Filename: "{app}\gpsync-tray.exe"; Description: "Start the gpsync tray app now"; \
    Flags: postinstall nowait skipifsilent

[UninstallRun]
; Remove the autostart entry before the files go, or it would point at a
; binary that no longer exists and fail silently at every sign-in.
Filename: "{app}\gpsync-tray.exe"; Parameters: "--uninstall-autostart"; \
    Flags: runhidden; RunOnceId: "RemoveAutostart"

[Code]
// True when {app} is not already on the user's PATH. Without this, installing
// twice appends the same directory twice.
function NeedsPathEntry(Dir: string): Boolean;
var
  Path: string;
begin
  if not RegQueryStringValue(HKCU, 'Environment', 'Path', Path) then
  begin
    Result := True;
    exit;
  end;
  Result := Pos(';' + Uppercase(Dir) + ';', ';' + Uppercase(Path) + ';') = 0;
end;
