# Builds srt-bridge.exe and the setup dist\SRT-Bridge-Setup-<version>.exe (+ .sha256).
# The version comes from main.go (const version).
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
$iscc = 'C:\Program Files\Inno Setup 7\ISCC.exe'
if (-not (Test-Path $iscc)) { $iscc = 'C:\Program Files (x86)\Inno Setup 6\ISCC.exe' }
if (-not (Test-Path $iscc)) { throw 'Inno Setup (ISCC.exe) not found' }

$mainGo = Join-Path $root 'main.go'
$line = Select-String -Path $mainGo -Pattern 'const version = "([0-9.]+)"' | Select-Object -First 1
if (-not $line) { throw 'version not found in main.go' }
$version = $line.Matches[0].Groups[1].Value

# Every setup gets its own version (Peter's rule): if a setup with this version
# was built already, the last number in main.go goes up by one first.
if (Test-Path (Join-Path $root "dist\SRT-Bridge-Setup-$version.exe")) {
    $parts = $version.Split('.')
    $parts[-1] = [string]([int]$parts[-1] + 1)
    $new = $parts -join '.'
    # main.go is plain ASCII; read and written as UTF-8 without BOM, never with Set-Content.
    $text = [IO.File]::ReadAllText($mainGo)
    $text = $text.Replace("const version = `"$version`"", "const version = `"$new`"")
    [IO.File]::WriteAllText($mainGo, $text, (New-Object Text.UTF8Encoding $false))
    Write-Host "version $version -> $new"
    $version = $new
}

# Built into installer\payload, not over srt-bridge.exe: that one may be running.
# The icon comes from rsrc_windows_amd64.syso in the project folder (build.ps1 makes it).
$go = 'C:\Fastvideo_dev\work\go-runtime\go\bin\go.exe'
if (-not (Test-Path $go)) { $go = 'go' }
$env:GOTOOLCHAIN = 'local'
$env:CGO_ENABLED = '0'
Push-Location $root
try {
    & $go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
    & $go build -trimpath -ldflags '-H windowsgui -s -w' -o (Join-Path $PSScriptRoot 'payload\srt-bridge.exe') .
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
} finally {
    Pop-Location
}

# SRT Bridge Output is part of this setup (since 0.4.0): built by its own
# build.ps1 into its bin folder, then copied into payload\output. Only the
# program files: its config and log in bin belong to the development copy.
$outRoot = Join-Path (Split-Path $root -Parent) 'srt_bridge_output'
& (Join-Path $outRoot 'build.ps1') | Out-Host
$payloadOut = Join-Path $PSScriptRoot 'payload\output'
if (Test-Path $payloadOut) { Remove-Item $payloadOut -Recurse -Force }
New-Item -ItemType Directory -Force $payloadOut | Out-Null
foreach ($f in 'srt-bridge-output.exe', 'srt_bridge_decoder.exe', 'libomt.dll', 'libvmx.dll') {
    Copy-Item (Join-Path $outRoot "bin\$f") $payloadOut -ErrorAction Stop
}
# fdk-aac (sound decoder) asks for its licence text next to the program.
Copy-Item (Join-Path $outRoot 'engine\third_party\fdk-aac\NOTICE') (Join-Path $payloadOut 'fdk-aac-NOTICE.txt') -ErrorAction Stop
# libomt/libvmx (MIT) ask for their licence text with every copy.
Copy-Item (Join-Path $outRoot 'engine\third_party\omt\LICENSE.txt') (Join-Path $payloadOut 'omt-LICENSE.txt') -ErrorAction Stop

& $iscc "/DMyAppVersion=$version" (Join-Path $PSScriptRoot 'srt_bridge.iss')
if ($LASTEXITCODE -ne 0) { throw 'ISCC failed' }

$setup = Join-Path $root "dist\SRT-Bridge-Setup-$version.exe"
$hash = (Get-FileHash $setup -Algorithm SHA256).Hash.ToLower()
# Written with .NET: no BOM, no code page surprises.
[IO.File]::WriteAllText("$setup.sha256", "$hash  SRT-Bridge-Setup-$version.exe`n")
Get-Item $setup | Select-Object Name, Length, LastWriteTime
