# Builds srt-bridge.exe (no console window). Go is not in the PATH on this PC.
$ErrorActionPreference = 'Stop'
$go = 'C:\Fastvideo_dev\work\go-runtime\go\bin\go.exe'
if (-not (Test-Path $go)) { $go = 'go' }
$env:GOTOOLCHAIN = 'local'
$env:CGO_ENABLED = '0'
Push-Location $PSScriptRoot
try {
    # The icon: only rebuilt when windres is there; the .syso stays in the folder.
    $windres = Get-ChildItem "$env:LOCALAPPDATA\Microsoft\WinGet\Packages" -Recurse -Filter windres.exe -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($windres) {
        & $windres.FullName -O coff -o rsrc_windows_amd64.syso app.rc
        if ($LASTEXITCODE -ne 0) { throw 'windres failed' }
    } elseif (-not (Test-Path rsrc_windows_amd64.syso)) {
        Write-Warning 'windres not found: the EXE gets no icon'
    }
    & $go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
    & $go build -trimpath -ldflags '-H windowsgui -s -w' -o srt-bridge.exe .
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
    Get-Item srt-bridge.exe | Select-Object Name, Length, LastWriteTime
} finally {
    Pop-Location
}
