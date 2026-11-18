# Builds everything into .\bin: srt-bridge-output.exe (window) and srt_bridge_decoder.exe (one per signal).
$ErrorActionPreference = 'Stop'
$go = 'C:\Fastvideo_dev\work\go-runtime\go\bin\go.exe'
if (-not (Test-Path $go)) { $go = 'go' }
$env:GOTOOLCHAIN = 'local'
$env:CGO_ENABLED = '0'
& (Join-Path $PSScriptRoot 'engine\build.ps1')
Push-Location $PSScriptRoot
try {
    # The icon (as in the bridge): only rebuilt when windres is there; the .syso stays in the folder.
    $windres = Get-ChildItem "$env:LOCALAPPDATA\Microsoft\WinGet\Packages" -Recurse -Filter windres.exe -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($windres) {
        & $windres.FullName -O coff -o rsrc_windows_amd64.syso app.rc
        if ($LASTEXITCODE -ne 0) { throw 'windres failed' }
    } elseif (-not (Test-Path rsrc_windows_amd64.syso)) {
        Write-Warning 'windres not found: the EXE gets no icon'
    }
    & $go vet .
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
    & $go build -trimpath -ldflags '-H windowsgui -s -w' -o bin\srt-bridge-output.exe .
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
    Get-ChildItem bin | Select-Object Name, Length, LastWriteTime
} finally {
    Pop-Location
}
