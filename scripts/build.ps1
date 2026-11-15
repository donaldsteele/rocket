<#
.SYNOPSIS
Cross-compile rocket for Windows, macOS and Linux (amd64 + arm64) and the
tray app for Windows and Linux (amd64 + arm64), zip each target with
README.md, HOW-TO-OPEN.txt and the license files into dist/, build the
signed Android app as dist/rocket-android.apk, and write dist/SHA256SUMS.

.DESCRIPTION
The APK needs ANDROID_HOME (or ANDROID_SDK_ROOT), the gomobile toolchain (see
scripts/build-android.ps1) and the release keystore properties file
($env:ROCKET_KEYSTORE_PROPS, else ~/.rocket-signing/keystore.properties).
Without the SDK or the keystore it is skipped with a warning; with -Release
(or RELEASE=1) that is an error. Only Gradle reads the keystore; this script
just checks that the properties file exists.

.EXAMPLE
pwsh scripts/build.ps1                  # version defaults to git describe
pwsh scripts/build.ps1 -Version 1.2.0
pwsh scripts/build.ps1 -Version 1.2.0 -Release   # the APK is required
#>
[CmdletBinding()]
param(
    [string]$Version = $env:VERSION,
    [switch]$Release = ($env:RELEASE -eq '1')
)
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

if (-not $Version) {
    $Version = (git describe --tags --always --dirty 2>$null)
    if ($LASTEXITCODE -ne 0 -or -not $Version) { $Version = 'dev' }
}
# rocket version prints "rocket 1.2.0", not "rocket v1.2.0".
$Version = $Version -replace '^v', ''
$Dist = 'dist'
$Targets = 'windows/amd64', 'windows/arm64', 'darwin/amd64', 'darwin/arm64', 'linux/amd64', 'linux/arm64'
# The macOS tray needs cgo, so it is not cross-compiled (build it on a Mac).
$TrayTargets = 'windows/amd64', 'windows/arm64', 'linux/amd64', 'linux/arm64'
$LdPkg = 'github.com/donaldsteele/rocket/internal/buildinfo.Version'

$Docs = 'README.md', 'HOW-TO-OPEN.txt', 'LICENSE', 'NOTICE', 'THIRD_PARTY_LICENSES.txt'
foreach ($f in $Docs) {
    if (-not (Test-Path $f -PathType Leaf)) { throw "missing $f" }
}

function Invoke-Native {
    param([string]$Exe, [string[]]$Arguments)
    & $Exe @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$Exe $($Arguments -join ' ') failed (exit $LASTEXITCODE)" }
}

# The Android APK is built when the SDK and the keystore properties are there.
# Decide up front, so -Release fails before the long desktop build.
$AndroidSdk = if ($env:ANDROID_HOME) { $env:ANDROID_HOME } else { $env:ANDROID_SDK_ROOT }
$KeystoreProps = if ($env:ROCKET_KEYSTORE_PROPS) { $env:ROCKET_KEYSTORE_PROPS } else {
    Join-Path $HOME '.rocket-signing/keystore.properties'
}
$ApkSkip = $null
if (-not $AndroidSdk -or -not (Test-Path $AndroidSdk -PathType Container)) {
    $ApkSkip = 'ANDROID_HOME is not set to an Android SDK'
} elseif (-not (Test-Path $KeystoreProps -PathType Leaf)) {
    $ApkSkip = 'no keystore properties file (set ROCKET_KEYSTORE_PROPS or create ~/.rocket-signing/keystore.properties)'
}
if ($ApkSkip -and $Release) { throw "-Release needs rocket-android.apk, but $ApkSkip" }

# The SHA-256 digest of the release signing certificate (docs/security.md).
# Kept in a file so build.sh and build.ps1 check the same one.
$ApkCertFile = Join-Path $PSScriptRoot '../android/release-cert.sha256'

# Fails unless apk is signed by exactly the release certificate: apksigner
# verify alone accepts any key, and a wrong ROCKET_KEYSTORE_PROPS would
# otherwise ship an APK that cannot update installs of the real one.
function Assert-ApkCert {
    param([string]$Apksigner, [string]$Apk)
    $expected = ((Get-Content $ApkCertFile -Raw) -replace '[:\s]', '').ToLowerInvariant()
    if ($expected -notmatch '^[0-9a-f]{64}$') { throw "$ApkCertFile does not hold a SHA-256 digest" }
    $certs = & $Apksigner verify --print-certs $Apk
    if ($LASTEXITCODE -ne 0) { throw "apksigner rejected $Apk (exit $LASTEXITCODE)" }
    $digests = @($certs | ForEach-Object {
            if ($_ -match '^Signer #\d+ certificate SHA-256 digest:\s*([0-9A-Fa-f:]+)') {
                ($Matches[1] -replace ':', '').ToLowerInvariant()
            }
        } | Sort-Object -Unique)
    if ($digests.Count -ne 1 -or $digests[0] -ne $expected) {
        $got = if ($digests.Count) { $digests -join ', ' } else { 'no certificate' }
        throw "$Apk is signed with $got, not the release certificate $expected ($ApkCertFile). Check ROCKET_KEYSTORE_PROPS."
    }
    Write-Host "signer certificate matches $ApkCertFile"
}

# Build the signed release APK into $Dist/rocket-android.apk, or warn and skip
# when the SDK or the keystore is missing.
function Build-Apk {
    if ($ApkSkip) {
        Write-Warning "skipping rocket-android.apk: $ApkSkip."
        return
    }
    $sdk = $AndroidSdk
    $out = 'android/app/build/outputs/apk/release'
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $out
    Write-Host 'building rocket-android.apk (signed release)'
    # A child pwsh, so the SDK, NDK and PATH changes stay out of this session.
    $pwsh = (Get-Process -Id $PID).Path
    Invoke-Native $pwsh @('-NoProfile', '-File', (Join-Path $PSScriptRoot 'build-android.ps1'),
        '-GradleTasks', 'assembleRelease')
    # Gradle names an APK it could not sign app-release-unsigned.apk.
    $apk = Join-Path $out 'app-release.apk'
    if (-not (Test-Path $apk -PathType Leaf)) { throw "no signed APK in $out" }
    $bt = Get-ChildItem (Join-Path $sdk 'build-tools') -Directory -ErrorAction SilentlyContinue |
        Sort-Object { try { [version]$_.Name } catch { [version]'0.0' } } | Select-Object -Last 1
    $apksigner = $null
    if ($bt) {
        $apksigner = @('apksigner.bat', 'apksigner') | ForEach-Object { Join-Path $bt.FullName $_ } |
            Where-Object { Test-Path $_ -PathType Leaf } | Select-Object -First 1
    }
    if (-not $apksigner) { throw "apksigner not found under $sdk/build-tools; cannot check the APK's signer." }
    Assert-ApkCert $apksigner $apk
    Copy-Item $apk (Join-Path $Dist 'rocket-android.apk')
    Write-Host "wrote $Dist/rocket-android.apk"
}

$stage = Join-Path ([IO.Path]::GetTempPath()) ("rocket-build-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $stage | Out-Null
$saved = @{ CGO_ENABLED = $env:CGO_ENABLED; GOOS = $env:GOOS; GOARCH = $env:GOARCH }
try {
    New-Item -ItemType Directory -Force -Path $Dist | Out-Null
    Get-ChildItem -Path $Dist -Filter 'rocket-*.zip' | Remove-Item -Force
    Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $Dist 'SHA256SUMS'), (Join-Path $Dist 'rocket-android.apk')

    # The license file must list exactly the modules being shipped.
    $env:CGO_ENABLED = $null; $env:GOOS = $null; $env:GOARCH = $null
    Invoke-Native go @('run', './tools/licenses', '-check')

    # tools/mkzip (pure Go) puts files at the zip root and keeps the binaries
    # executable for macOS and Linux, matching scripts/build.sh exactly.
    $mkzip = Join-Path $stage 'mkzip.exe'
    Invoke-Native go @('build', '-o', $mkzip, './tools/mkzip')
    Write-Host "rocket version: $Version"

    # Version info, manifest and icon for rocket.exe (cmd/rocket/winres). go
    # build links the generated rsrc_windows_*.syso into the Windows targets only.
    Invoke-Native go @('tool', 'go-winres', 'make', '--in', 'cmd/rocket/winres/winres.json',
        '--out', 'cmd/rocket/rsrc', '--arch', 'amd64,arm64',
        '--product-version', $Version, '--file-version', $Version)

    foreach ($target in $Targets) {
        $goos, $goarch = $target -split '/'
        $exe = if ($goos -eq 'windows') { '.exe' } else { '' }

        $pkg = Join-Path $stage "$goos-$goarch"
        New-Item -ItemType Directory -Path $pkg | Out-Null
        Write-Host "building $goos/$goarch"
        $env:CGO_ENABLED = '0'; $env:GOOS = $goos; $env:GOARCH = $goarch
        Invoke-Native go @('build', '-trimpath', '-ldflags', "-s -w -X $LdPkg=$Version",
            '-o', (Join-Path $pkg "rocket$exe"), './cmd/rocket')

        if ($TrayTargets -contains $target) {
            Write-Host "building tray $goos/$goarch"
            $trayFlags = "-s -w -X main.version=$Version"
            if ($goos -eq 'windows') { $trayFlags += ' -H=windowsgui' }
            Push-Location tray
            try {
                Invoke-Native go @('build', '-trimpath', '-ldflags', $trayFlags,
                    '-o', (Join-Path $pkg "rocket-tray$exe"), '.')
            }
            finally { Pop-Location }
        }
        $env:CGO_ENABLED = $null; $env:GOOS = $null; $env:GOARCH = $null
        Copy-Item $Docs -Destination $pkg

        $zip = Join-Path $Dist "rocket-$goos-$goarch.zip"
        $files = Get-ChildItem -File $pkg | ForEach-Object FullName
        Invoke-Native $mkzip (@('-o', $zip, '-x', 'rocket', '-x', 'rocket.exe',
                '-x', 'rocket-tray', '-x', 'rocket-tray.exe') + $files)
        Write-Host "wrote $zip"
        Remove-Item -Recurse -Force $pkg
    }

    Build-Apk

    # Same format as sha256sum: "<lowercase hash>  <name>", LF line endings.
    # The zips first, then the APK when there is one (scripts/build.sh matches).
    $sumFiles = @(Get-ChildItem -Path $Dist -Filter 'rocket-*.zip' | Sort-Object Name)
    $sumFiles += @(Get-ChildItem -Path $Dist -Filter 'rocket-android.apk')
    $sums = $sumFiles | ForEach-Object {
        (Get-FileHash -Algorithm SHA256 $_.FullName).Hash.ToLowerInvariant() + '  ' + $_.Name
    }
    $sumsPath = Join-Path (Resolve-Path $Dist) 'SHA256SUMS'
    [IO.File]::WriteAllText($sumsPath, (($sums -join "`n") + "`n"))
    Write-Host "wrote $Dist/SHA256SUMS"
}
finally {
    foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $stage
    Remove-Item -Force -ErrorAction SilentlyContinue cmd/rocket/rsrc_windows_*.syso
}
