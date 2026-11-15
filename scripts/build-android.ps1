<#
.SYNOPSIS
Build the Go core of the Android app (mobile/rocketcore) into
android/app/libs/rocketcore.aar with gomobile bind, then run Gradle in
android/ when the app project is there.

.DESCRIPTION
Needs Go, gomobile and gobind (go install golang.org/x/mobile/cmd/gomobile
and .../gobind, then gomobile init), and an Android SDK with an NDK.
ANDROID_HOME defaults to %LOCALAPPDATA%\Android\Sdk (ANDROID_SDK_ROOT is also
read); ANDROID_NDK_HOME defaults to the newest NDK under ANDROID_HOME\ndk.
The environment is set for this script only.

.EXAMPLE
pwsh scripts/build-android.ps1                    # AAR, then gradlew assembleDebug
pwsh scripts/build-android.ps1 -AarOnly
pwsh scripts/build-android.ps1 -GradleTasks testDebugUnitTest,lintDebug,assembleDebug
#>
[CmdletBinding()]
param(
    [switch]$AarOnly,
    [string[]]$GradleTasks = @('assembleDebug')
)
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

function Invoke-Native {
    param([string]$Exe, [string[]]$Arguments)
    & $Exe @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$Exe $($Arguments -join ' ') failed (exit $LASTEXITCODE)" }
}

$sdk = $env:ANDROID_HOME
if (-not $sdk) { $sdk = $env:ANDROID_SDK_ROOT }
if (-not $sdk) { $sdk = Join-Path $env:LOCALAPPDATA 'Android\Sdk' }
if (-not (Test-Path $sdk -PathType Container)) { throw "Android SDK not found at $sdk (set ANDROID_HOME)" }
$env:ANDROID_HOME = $sdk

$ndk = $env:ANDROID_NDK_HOME
if (-not $ndk) {
    $ndkDir = Join-Path $sdk 'ndk'
    if (Test-Path $ndkDir) {
        $newest = Get-ChildItem $ndkDir -Directory |
            Sort-Object { try { [version]$_.Name } catch { [version]'0.0' } } |
            Select-Object -Last 1
        if ($newest) { $ndk = $newest.FullName }
    }
}
if (-not $ndk -or -not (Test-Path $ndk -PathType Container)) { throw "Android NDK not found (set ANDROID_NDK_HOME)" }
$env:ANDROID_NDK_HOME = $ndk

# gomobile runs gobind from PATH; both live in GOPATH/bin after go install.
$goBin = Join-Path (go env GOPATH) 'bin'
$env:PATH = "$goBin$([IO.Path]::PathSeparator)$env:PATH"
if (-not (Get-Command gomobile -ErrorAction SilentlyContinue)) { throw 'gomobile not found (go install golang.org/x/mobile/cmd/gomobile@latest; gomobile init)' }
if (-not (Get-Command gobind -ErrorAction SilentlyContinue)) { throw 'gobind not found (go install golang.org/x/mobile/cmd/gobind@latest)' }

$aar = 'android/app/libs/rocketcore.aar'
New-Item -ItemType Directory -Force (Split-Path $aar) | Out-Null
Write-Host "gomobile bind -> $aar (NDK $(Split-Path $ndk -Leaf))"
Invoke-Native gomobile @(
    'bind',
    '-target=android/arm64,android/arm,android/amd64',
    '-androidapi', '26',
    '-javapkg', 'io.github.donaldsteele.rocket.core',
    '-trimpath', '-ldflags=-s -w',
    '-o', $aar,
    './mobile/rocketcore'
)

if ($AarOnly) { return }
$gradlew = if ($IsWindows -or $env:OS -eq 'Windows_NT') { 'gradlew.bat' } else { './gradlew' }
if (-not (Test-Path (Join-Path 'android' $gradlew.TrimStart('./')))) {
    Write-Host 'android/ has no Gradle project yet; built the AAR only.'
    return
}
Push-Location android
try {
    Invoke-Native $gradlew $GradleTasks
} finally {
    Pop-Location
}
