; SRT Bridge setup. Built by installer\build-setup.ps1, which passes the version
; from main.go: ISCC /DMyAppVersion=0.3.5 srt_bridge.iss

#ifndef MyAppVersion
  #error Build with build-setup.ps1 (it passes MyAppVersion)
#endif

#define MyAppName "SRT Bridge"
#define MyAppExe "srt-bridge.exe"
#define DefaultPort "21101"

[Setup]
AppId={{4726D871-576E-4A0D-98C2-67EE8D1BEA3F}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
VersionInfoVersion={#MyAppVersion}.0
AppVerName={#MyAppName} {#MyAppVersion}
AppPublisher=Peter Aellig
; The program only reads its folder. Settings and log go to the places Windows
; has for them: %APPDATA%\SRTBridge\config.json, %LOCALAPPDATA%\SRTBridge\srt-bridge.log.
DefaultDirName={autopf}\SRT Bridge
DisableProgramGroupPage=yes
PrivilegesRequired=admin
; Only for testing without administrator rights: setup /CURRENTUSER
PrivilegesRequiredOverridesAllowed=commandline
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
OutputDir=..\dist
OutputBaseFilename=SRT-Bridge-Setup-{#MyAppVersion}
SetupIconFile=..\logo\srt_bridge.ico
UninstallDisplayIcon={app}\{#MyAppExe}
UninstallDisplayName={#MyAppName}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
; A running bridge is closed before its files are replaced (and before uninstalling).
CloseApplications=yes
RestartApplications=no

[Languages]
Name: "en"; MessagesFile: "compiler:Default.isl"
Name: "de"; MessagesFile: "compiler:Languages\German.isl"

[CustomMessages]
en.TaskDesktop=Desktop shortcut
de.TaskDesktop=Verknüpfung auf dem Desktop
en.TaskFirewall=Windows Firewall: allow incoming SRT on UDP {#DefaultPort}
de.TaskFirewall=Windows-Firewall: eingehendes SRT auf UDP {#DefaultPort} erlauben
en.GroupOther=Other:
de.GroupOther=Weiteres:
en.StartNow=Start SRT Bridge
de.StartNow=SRT Bridge starten

[Tasks]
Name: "desktopicon"; Description: "{cm:TaskDesktop}"; GroupDescription: "{cm:GroupOther}"
; Only on a first installation: an existing configuration may use another port,
; and its rule is set with the button in Setup.
Name: "firewall"; Description: "{cm:TaskFirewall}"; GroupDescription: "{cm:GroupOther}"; Check: IsFirstInstall

[Files]
Source: "payload\srt-bridge.exe"; DestDir: "{app}"; Flags: ignoreversion
; SRT Bridge Output belongs to the bridge since 0.4.0: the bridge finds it in {app}\output
; and starts it from its tab «Outputs». Its settings go to %APPDATA%\SRTBridge as well.
Source: "payload\output\*"; DestDir: "{app}\output"; Flags: ignoreversion
; The WHIP input (since 0.5.0): srt-bridge-whip and its sound converter; the bridge
; finds them in {app}\whip and starts them while WHIP is switched on in Setup.
Source: "payload\whip\*"; DestDir: "{app}\whip"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\{#MyAppName}"; Filename: "{app}\{#MyAppExe}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExe}"; Tasks: desktopicon

[Run]
; The same rule name the bridge uses for its own button, so both find each other.
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall delete rule name=""SRT Bridge"""; Flags: runhidden; Tasks: firewall
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall add rule name=""SRT Bridge"" dir=in action=allow protocol=UDP localport={#DefaultPort} profile=any"; Flags: runhidden; Tasks: firewall
Filename: "{app}\{#MyAppExe}"; Description: "{cm:StartNow}"; Flags: nowait postinstall skipifsilent runasoriginaluser

[UninstallRun]
; Bridge Output and its decoders end before their files go.
Filename: "{sys}\taskkill.exe"; Parameters: "/F /IM srt-bridge-output.exe"; Flags: runhidden; RunOnceId: "KillOutput"
Filename: "{sys}\taskkill.exe"; Parameters: "/F /IM srt_bridge_decoder.exe"; Flags: runhidden; RunOnceId: "KillDecoder"
Filename: "{sys}\taskkill.exe"; Parameters: "/F /IM srt-bridge-whip.exe"; Flags: runhidden; RunOnceId: "KillWhip"
Filename: "{sys}\taskkill.exe"; Parameters: "/F /IM srt_bridge_aac.exe"; Flags: runhidden; RunOnceId: "KillWhipAac"
; «Start with Windows» of the user who uninstalls (set in the bridge under Setup).
Filename: "{sys}\reg.exe"; Parameters: "delete ""HKCU\Software\Microsoft\Windows\CurrentVersion\Run"" /v ""SRT Bridge"" /f"; Flags: runhidden; RunOnceId: "DelAutostart"
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall delete rule name=""SRT Bridge"""; Flags: runhidden; RunOnceId: "DelFirewallRule"
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall delete rule name=""SRT Bridge (remote)"""; Flags: runhidden; RunOnceId: "DelFirewallRuleRemote"
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall delete rule name=""SRT Bridge (WHIP)"""; Flags: runhidden; RunOnceId: "DelFirewallRuleWhip"

; The settings and the log in %APPDATA% / %LOCALAPPDATA% stay after uninstalling:
; they hold the signals, keys and passphrases, and a new installation picks them up again.

[Code]
// A first installation: this user has no settings yet (an existing configuration
// may use another port; its rule is set with the button next to the port).
function IsFirstInstall(): Boolean;
begin
  Result := not FileExists(ExpandConstant('{userappdata}\SRTBridge\config.json'));
end;
