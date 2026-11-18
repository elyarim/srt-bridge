# Builds everything into .\bin: srt-bridge-whip.exe (Go, Pion) and srt_bridge_aac.exe (MSVC, libopus + fdk-aac),
# and bin\third_party_licenses.txt with the licences of the Go modules that are compiled in.
$ErrorActionPreference = 'Stop'
$go = 'C:\Fastvideo_dev\work\go-runtime\go\bin\go.exe'
if (-not (Test-Path $go)) { $go = 'go' }
$env:GOTOOLCHAIN = 'local'
$env:CGO_ENABLED = '0'
& (Join-Path $PSScriptRoot 'engine\build.ps1') | Out-Host
Push-Location $PSScriptRoot
try {
    & $go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
    & $go build -trimpath -ldflags '-H windowsgui -s -w' -o bin\srt-bridge-whip.exe .
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
    # Licences of every module in the program (Pion and what it uses), one file next to it.
    $mods = & $go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}{{end}}' . | Sort-Object -Unique
    $sb = New-Object Text.StringBuilder
    [void]$sb.AppendLine('Third-party Go modules compiled into srt-bridge-whip.exe')
    foreach ($m in $mods) {
        if (-not $m) { continue }
        $path, $ver, $dir = $m.Split('|')
        $lic = Get-ChildItem $dir -File | Where-Object { $_.Name -match '^(LICENSE|LICENCE|COPYING)' } | Select-Object -First 1
        [void]$sb.AppendLine('')
        [void]$sb.AppendLine('=' * 72)
        [void]$sb.AppendLine("$path $ver")
        [void]$sb.AppendLine('=' * 72)
        if ($lic) { [void]$sb.AppendLine([IO.File]::ReadAllText($lic.FullName)) } else { [void]$sb.AppendLine('(no licence file in the module)') }
    }
    [IO.File]::WriteAllText((Join-Path $PSScriptRoot 'bin\third_party_licenses.txt'), $sb.ToString(), (New-Object Text.UTF8Encoding $false))
    Get-ChildItem bin -Filter *.exe | Select-Object Name, Length, LastWriteTime
} finally {
    Pop-Location
}
