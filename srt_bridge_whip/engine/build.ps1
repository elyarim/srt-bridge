# Builds srt_bridge_aac.exe (Opus -> AAC for the WHIP input) with MSVC (Visual Studio 18) into ..\bin.
# Opus (third_party\opus) and the AAC encoder of fdk-aac (third_party\fdk-aac) are built once into
# static libraries under %LOCALAPPDATA%\srt_bridge_whip\obj; -RebuildLibs builds them again.
param([switch]$RebuildLibs, [string]$Exe = '')
$ErrorActionPreference = 'Stop'
$vs = 'C:\Program Files\Microsoft Visual Studio\18\Community'
$vcvars = Join-Path $vs 'VC\Auxiliary\Build\vcvars64.bat'
$cmake = Join-Path $vs 'Common7\IDE\CommonExtensions\Microsoft\CMake\CMake\bin\cmake.exe'
if (-not (Test-Path $vcvars)) { throw "vcvars64.bat not found: $vcvars" }
if (-not (Test-Path $cmake)) { throw "cmake.exe of Visual Studio not found: $cmake" }
$src = $PSScriptRoot
$fdk = Join-Path $src 'third_party\fdk-aac'
$opus = Join-Path $src 'third_party\opus'
$bin = Join-Path (Split-Path $src -Parent) 'bin'
$obj = Join-Path $env:LOCALAPPDATA 'srt_bridge_whip\obj'
$fdkObj = Join-Path $obj 'fdk'
$fdkLib = Join-Path $obj 'fdk-aac-enc.lib'
$opusBuild = Join-Path $obj 'opus'
$opusLib = Join-Path $opusBuild 'Release\opus.lib'
New-Item -ItemType Directory -Force $bin, $obj, $fdkObj | Out-Null
# vcvars64 asks for vswhere.exe, which is not there; the message is harmless and is kept out of the output.
$env_ = "`"$vcvars`" >nul 2>nul"

$parts = 'libAACenc', 'libSBRenc', 'libMpegTPEnc', 'libSACenc', 'libFDK', 'libSYS', 'libPCMutils', 'libArithCoding'
$fdkInc = ($parts | Where-Object { Test-Path (Join-Path $fdk "$_\include") } | ForEach-Object { "/I`"$fdk\$_\include`"" }) -join ' '
if ($RebuildLibs -or -not (Test-Path $fdkLib)) {
    Remove-Item "$fdkObj\*.obj" -ErrorAction SilentlyContinue
    $files = $parts | Where-Object { Test-Path (Join-Path $fdk "$_\src") } | ForEach-Object { Get-ChildItem (Join-Path $fdk "$_\src") -Filter *.cpp } | ForEach-Object { "`"$($_.FullName)`"" }
    $rsp = Join-Path $obj 'fdk.rsp'
    [IO.File]::WriteAllText($rsp, "/nologo /c /O2 /EHsc /MT /w /MP $fdkInc /Fo`"$fdkObj\\`" " + ($files -join ' '))
    cmd /c "$env_ && cl @`"$rsp`" >`"$obj\fdk-build.log`" && lib /nologo /OUT:`"$fdkLib`" `"$fdkObj\*.obj`""
    if ($LASTEXITCODE -ne 0) { throw "fdk-aac build failed, see $obj\fdk-build.log" }
}
if ($RebuildLibs -or -not (Test-Path $opusLib)) {
    if (Test-Path $opusBuild) { Remove-Item -Recurse -Force $opusBuild }
    & $cmake -S $opus -B $opusBuild -G 'Visual Studio 18 2026' -A x64 -DOPUS_STATIC_RUNTIME=ON -DOPUS_BUILD_PROGRAMS=OFF -DOPUS_BUILD_TESTING=OFF -DOPUS_DRED=OFF -DOPUS_OSCE=OFF -DBUILD_SHARED_LIBS=OFF *> (Join-Path $obj 'opus-configure.log')
    if ($LASTEXITCODE -ne 0) { throw "opus configure failed, see $obj\opus-configure.log" }
    & $cmake --build $opusBuild --config Release --target opus *> (Join-Path $obj 'opus-build.log')
    if ($LASTEXITCODE -ne 0) { throw "opus build failed, see $obj\opus-build.log" }
}

if (-not $Exe) { $Exe = Join-Path $bin 'srt_bridge_aac.exe' }
$cl = "cl /nologo /std:c++20 /O2 /EHsc /W3 /utf-8 /MT /DNOMINMAX $fdkInc /I`"$opus\include`" /Fo`"$obj\\`" /Fe`"$Exe`" `"$src\aac.cpp`" /link `"$fdkLib`" `"$opusLib`""
cmd /c "$env_ && $cl"
if ($LASTEXITCODE -ne 0) { throw 'compile failed' }
Get-Item $Exe | Select-Object Name, Length, LastWriteTime
