<#
.SYNOPSIS
Verifies scripts/install.ps1 end to end on Windows without network access.

.DESCRIPTION
Builds real Windows binaries, packs the release archive the way GoReleaser
would (zip + checksums.txt), then installs it through scripts/install.ps1 using
a filesystem release base URL. It asserts that:

  * both binaries install and run with the built version,
  * the version lookup path resolves the latest release tag,
  * a tampered archive is rejected by the checksum check,
  * the user PATH gains the install directory exactly once.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$Installer = Join-Path $Root 'scripts/install.ps1'
$Version = '0.0.0-test'
$Arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$Asset = "solo_${Version}_windows_${Arch}.zip"

$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("solo-release-test-" + [guid]::NewGuid().ToString('N'))
$ReleaseDir = Join-Path $TempDir 'release'
$PayloadDir = Join-Path $TempDir 'payload'
$InstallDir = Join-Path $TempDir 'install'

function Assert-True([bool]$Condition, [string]$Message) {
  if (-not $Condition) { throw "release install test: $Message" }
}

function Get-FileUrl([string]$Path) {
  return 'file:///' + ($Path -replace '\\', '/')
}

# Builds the Windows payloads. Uses the native Go toolchain when present and
# falls back to Go inside WSL, so the test also runs on a Windows host whose
# Go lives in a Linux distribution.
function Build-WindowsPayload([string]$OutputDir) {
  $ldflags = "-s -w -X github.com/solo-ai/solo/pkg/version.Version=$Version -X github.com/solo-ai/solo/pkg/version.Commit=test -X github.com/solo-ai/solo/pkg/version.Date=test"
  if (Get-Command go -ErrorAction SilentlyContinue) {
    Push-Location $Root
    try {
      $env:GOOS = 'windows'
      $env:GOARCH = $Arch
      $env:CGO_ENABLED = '0'
      & go build -ldflags $ldflags -o (Join-Path $OutputDir 'solo.exe') ./cmd/solo
      Assert-True ($LASTEXITCODE -eq 0) 'go build ./cmd/solo failed'
      & go build -ldflags $ldflags -o (Join-Path $OutputDir 'solo-daemon.exe') ./cmd/daemon
      Assert-True ($LASTEXITCODE -eq 0) 'go build ./cmd/daemon failed'
    } finally {
      Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
      Pop-Location
    }
    return
  }

  if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) {
    throw 'release install test: neither a native go toolchain nor wsl.exe is available to build the payloads'
  }
  $repo = $Root -replace '\\', '/'
  $wslRepo = "/mnt/$($repo.Substring(0, 1).ToLower())$($repo.Substring(2))"
  # wslpath needs forward slashes; PowerShell would otherwise strip the
  # backslashes while passing the argument to wsl.exe.
  $outputDirForWsl = $OutputDir -replace '\\', '/'
  $wslOut = (& wsl.exe -d Ubuntu -- wslpath -u $outputDirForWsl | Out-String).Trim()
  $script = "set -euo pipefail; cd '$wslRepo'; export CGO_ENABLED=0 GOPROXY=https://goproxy.cn,direct; " +
  "GOOS=windows GOARCH=$Arch go build -ldflags '$ldflags' -o '$wslOut/solo.exe' ./cmd/solo; " +
  "GOOS=windows GOARCH=$Arch go build -ldflags '$ldflags' -o '$wslOut/solo-daemon.exe' ./cmd/daemon"
  & wsl.exe -d Ubuntu -- bash -c $script
  Assert-True ($LASTEXITCODE -eq 0) 'building the payloads through WSL failed'
}

New-Item -ItemType Directory -Path $ReleaseDir, $PayloadDir -Force | Out-Null

try {
  Write-Host "release install test: building $Version for windows/$Arch"
  Build-WindowsPayload $PayloadDir

  Compress-Archive -Path (Join-Path $PayloadDir 'solo.exe'), (Join-Path $PayloadDir 'solo-daemon.exe') `
    -DestinationPath (Join-Path $ReleaseDir $Asset) -Force
  $hash = (Get-FileHash -LiteralPath (Join-Path $ReleaseDir $Asset) -Algorithm SHA256).Hash.ToLowerInvariant()
  Set-Content -LiteralPath (Join-Path $ReleaseDir 'checksums.txt') -Value "$hash  $Asset"

  Write-Host 'release install test: installing from a filesystem release'
  & $Installer -Version $Version -ReleaseBaseUrl (Get-FileUrl $ReleaseDir) -BinDir $InstallDir -NoPathUpdate
  Assert-True (Test-Path -LiteralPath (Join-Path $InstallDir 'solo.exe')) 'installer did not install solo.exe'
  Assert-True (Test-Path -LiteralPath (Join-Path $InstallDir 'solo-daemon.exe')) 'installer did not install solo-daemon.exe'

  $versionOutput = & (Join-Path $InstallDir 'solo.exe') version
  Assert-True ($LASTEXITCODE -eq 0) 'installed solo.exe failed to run'
  Assert-True (($versionOutput -join "`n") -match [regex]::Escape($Version)) "installed solo.exe did not report $Version"

  Write-Host 'release install test: resolving the latest version without -Version'
  $mockDir = Join-Path $TempDir 'mock'
  New-Item -ItemType Directory -Path $mockDir -Force | Out-Null
  $mockTmp = Join-Path $TempDir 'mock-tmp'
  New-Item -ItemType Directory -Path $mockTmp -Force | Out-Null
  $mockLatest = Join-Path $mockDir 'latest.json'
  Set-Content -LiteralPath $mockLatest -Value ('{"tag_name":"v' + $Version + '"}')
  $previousLatestUrl = $env:SOLO_LATEST_VERSION_URL
  $env:SOLO_LATEST_VERSION_URL = Get-FileUrl $mockLatest
  $env:TEMP = $mockTmp
  $env:TMP = $mockTmp
  try {
    & $Installer -ReleaseBaseUrl (Get-FileUrl $ReleaseDir) -BinDir (Join-Path $TempDir 'install-latest') -NoPathUpdate
  } finally {
    $env:TEMP = $TempDir
    $env:TMP = $TempDir
    if ($previousLatestUrl) { $env:SOLO_LATEST_VERSION_URL = $previousLatestUrl } else { Remove-Item Env:SOLO_LATEST_VERSION_URL -ErrorAction SilentlyContinue }
  }
  Assert-True (Test-Path -LiteralPath (Join-Path $TempDir 'install-latest/solo.exe')) 'latest-version install did not produce solo.exe'

  Write-Host 'release install test: rejecting a tampered archive'
  $tampered = Join-Path $TempDir 'tampered'
  New-Item -ItemType Directory -Path $tampered -Force | Out-Null
  Copy-Item -LiteralPath (Join-Path $ReleaseDir $Asset) -Destination (Join-Path $tampered $Asset)
  Copy-Item -LiteralPath (Join-Path $ReleaseDir 'checksums.txt') -Destination (Join-Path $tampered 'checksums.txt')
  Add-Content -LiteralPath (Join-Path $tampered $Asset) -Value 'tampered'
  $rejected = $false
  try {
    & $Installer -Version $Version -ReleaseBaseUrl (Get-FileUrl $tampered) `
      -BinDir (Join-Path $TempDir 'install-tampered') -NoPathUpdate
  } catch {
    $rejected = $_.Exception.Message -match 'checksum verification failed'
  }
  Assert-True $rejected 'installer accepted an archive whose checksum does not match'

  Write-Host 'release install test: user PATH gains the install directory once'
  $originalUserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  try {
    & $Installer -Version $Version -ReleaseBaseUrl (Get-FileUrl $ReleaseDir) -BinDir $InstallDir
    & $Installer -Version $Version -ReleaseBaseUrl (Get-FileUrl $ReleaseDir) -BinDir $InstallDir
    $updatedUserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $matches = @($updatedUserPath -split ';' | Where-Object { $_ -eq $InstallDir })
    Assert-True ($matches.Count -eq 1) "expected the install directory once in the user PATH, found $($matches.Count)"
  } finally {
    [Environment]::SetEnvironmentVariable('Path', $originalUserPath, 'User')
  }

  Write-Host 'Release archive, checksum verification, version lookup, PATH update, and PowerShell installer verified.'
} finally {
  Remove-Item -LiteralPath $TempDir -Recurse -Force -ErrorAction SilentlyContinue
}
