# Builds srt_bridge_decoder.exe with MSVC (Visual Studio 18) into ..\bin, next to libomt.dll and libvmx.dll.
# The AAC decoder fdk-aac (third_party\fdk-aac, decoder parts only) is built once into a static library and linked in.
param([switch]$RebuildFdk, [string]$Exe = '')
$ErrorActionPreference = 'Stop'
$vcvars = 'C:\Program Files\Microsoft Visual Studio\18\Community\VC\Auxiliary\Build\vcvars64.bat'
if (-not (Test-Path $vcvars)) { throw "vcvars64.bat not found: $vcvars" }
$src = $PSScriptRoot
$omt = Join-Path $src 'third_party\omt\Libraries\Winx64'
$fdk = Join-Path $src 'third_party\fdk-aac'
$bin = Join-Path (Split-Path $src -Parent) 'bin'
$obj = Join-Path $env:LOCALAPPDATA 'srt_bridge_output\obj'
$fdkObj = Join-Path $obj 'fdk'
$fdkLib = Join-Path $obj 'fdk-aac.lib'
New-Item -ItemType Directory -Force $bin, $obj, $fdkObj | Out-Null
# vcvars64 asks for vswhere.exe, which is not there; the message is harmless and is kept out of the output.
$env_ = "`"$vcvars`" >nul 2>nul"

$parts = 'libAACdec', 'libArithCoding', 'libDRCdec', 'libFDK', 'libMpegTPDec', 'libPCMutils', 'libSACdec', 'libSBRdec', 'libSYS'
$fdkInc = ($parts | ForEach-Object { "/I`"$fdk\$_\include`"" }) -join ' '
if ($RebuildFdk -or -not (Test-Path $fdkLib)) {
    $files = $parts | ForEach-Object { Get-ChildItem (Join-Path $fdk "$_\src") -Filter *.cpp } | ForEach-Object { "`"$($_.FullName)`"" }
    $rsp = Join-Path $obj 'fdk.rsp'
    [IO.File]::WriteAllText($rsp, "/nologo /c /O2 /EHsc /MT /w /MP $fdkInc /Fo`"$fdkObj\\`" " + ($files -join ' '))
    cmd /c "$env_ && cl @`"$rsp`" >`"$obj\fdk-build.log`" && lib /nologo /OUT:`"$fdkLib`" `"$fdkObj\*.obj`""
    if ($LASTEXITCODE -ne 0) { throw "fdk-aac build failed, see $obj\fdk-build.log" }
}

# SDI: the DeckLink interfaces come from the type library of Blackmagic Desktop Video (as in SDI_converter2's CMake).
# Desktop Video 14 installs into "Blackmagic Desktop Video", newer versions into "Desktop Video". Only needed to build;
# the decoder runs without Desktop Video and then finds no SDI port.
$dlDll = 'C:\Program Files\Blackmagic Design\Desktop Video\DeckLinkAPI64.dll', 'C:\Program Files\Blackmagic Design\Blackmagic Desktop Video\DeckLinkAPI64.dll' |
    Where-Object { Test-Path $_ } | Select-Object -First 1
if (-not $dlDll) { throw 'DeckLinkAPI64.dll not found: install Blackmagic Desktop Video to build' }
# Desktop Video 14.3+ keeps the old API as *_v14_2_1; older drivers only know the plain names.
$hasV14 = [Text.Encoding]::ASCII.GetString([IO.File]::ReadAllBytes($dlDll)).Contains('IDeckLinkInput_v14_2_1')
$importH = @(
    '#pragma once',
    '// Written by build.ps1: the DeckLink type library lives in a different folder per Desktop Video version.',
    "#import `"$($dlDll -replace '\\', '/')`" raw_interfaces_only raw_native_types no_namespace named_guids",
    $(if ($hasV14) { '#define SDI_DECKLINK_V14_2_1 1' } else { '' }),
    '#ifndef SDI_DECKLINK_V14_2_1',
    'using IDeckLinkOutput_v14_2_1 = IDeckLinkOutput;',
    'using IDeckLinkVideoOutputCallback_v14_2_1 = IDeckLinkVideoOutputCallback;',
    'using IDeckLinkVideoFrame_v14_2_1 = IDeckLinkVideoFrame;',
    'using IDeckLinkMutableVideoFrame_v14_2_1 = IDeckLinkMutableVideoFrame;',
    '#endif',
    'using BMDDisplayMode = _BMDDisplayMode;',
    'using BMDDeckLinkAttributeID = _BMDDeckLinkAttributeID;'
) -join "`r`n"
[IO.File]::WriteAllText((Join-Path $obj 'decklink_import.h'), $importH + "`r`n")

# -Exe builds to another file, e.g. for a test while the decoder in bin is running.
if (-not $Exe) { $Exe = Join-Path $bin 'srt_bridge_decoder.exe' }
$cl = "cl /nologo /std:c++20 /O2 /EHsc /W3 /utf-8 /MT /DNOMINMAX /I`"$omt`" /I`"$obj`" $fdkInc /Fo`"$obj\\`" /Fe`"$Exe`" `"$src\main.cpp`" /link /LIBPATH:`"$omt`" libomt.lib `"$fdkLib`" mfplat.lib mfuuid.lib mf.lib ole32.lib oleaut32.lib ws2_32.lib"
cmd /c "$env_ && $cl"
if ($LASTEXITCODE -ne 0) { throw 'compile failed' }
# Only when they differ: a running decoder holds the DLLs open.
foreach ($dll in 'libomt.dll', 'libvmx.dll') {
    $from = Get-Item (Join-Path $omt $dll); $to = Join-Path $bin $dll
    if (-not (Test-Path $to) -or (Get-Item $to).Length -ne $from.Length -or (Get-Item $to).LastWriteTime -ne $from.LastWriteTime) { Copy-Item $from.FullName $bin -Force }
}
Get-Item $Exe | Select-Object Name, Length, LastWriteTime
