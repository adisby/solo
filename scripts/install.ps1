<#
.SYNOPSIS
Installs the Solo CLI and Daemon on Windows, optionally pairing this Computer.

.DESCRIPTION
Downloads the checksummed Windows release archive for this machine's
architecture, verifies its SHA256 against the release checksums.txt, extracts
both binaries, and adds the install directory to the user PATH.

With -Connect it then runs `solo daemon connect`, which pairs this Computer
with the Solo server and starts the Daemon in the background.

.PARAMETER Connect
Pair this Computer after installation. Requires -Server, -ComputerId and
-Token (or the matching SOLO_* environment variables).

.EXAMPLE
irm https://raw.githubusercontent.com/solo-agent/solo/master/scripts/install.ps1 | iex

.EXAMPLE
$env:SOLO_VERSION = '1.1.0'
.\install.ps1 connect -Server 'https://solo.example.com' -ComputerId '<uuid>' -Token '<token>'
#>
[CmdletBinding()]
param(
  # Pair this Computer after installing.
  [switch]$Connect,
  # Solo Server base URL, for example https://solo.example.com.
  [string]$Server = $(if ($env:SOLO_SERVER) { $env:SOLO_SERVER } else { '' }),
  # Computer ID from the Solo UI (a UUID).
  [string]$ComputerId = $(if ($env:SOLO_COMPUTER_ID) { $env:SOLO_COMPUTER_ID } else { '' }),
  # One-time enrollment token from the Solo UI.
  [string]$Token = $(if ($env:SOLO_ENROLLMENT_TOKEN) { $env:SOLO_ENROLLMENT_TOKEN } else { '' }),
  # Release version to install, for example 1.1.0 or v1.1.0. Defaults to the latest release.
  [string]$Version = $(if ($env:SOLO_VERSION) { $env:SOLO_VERSION } else { '' }),
  # Release download base URL. Defaults to the GitHub release for -Version.
  [string]$ReleaseBaseUrl = $(if ($env:SOLO_RELEASE_BASE_URL) { $env:SOLO_RELEASE_BASE_URL } else { '' }),
  # Installation directory. Defaults to %USERPROFILE%\.solo\bin.
  [string]$BinDir = $(if ($env:SOLO_BIN_DIR) { $env:SOLO_BIN_DIR } else { '' }),
  # Do not modify the user PATH.
  [switch]$NoPathUpdate,
  # Also register the Windows logon task so the Daemon starts automatically.
  [switch]$Autostart
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Repo = 'solo-agent/solo'

function Write-Step([string]$Message) {
  Write-Host "solo installer: $Message"
}

function Stop-Install([string]$Message) {
  throw "solo installer: $Message"
}

function Get-ReleaseBaseUrl([string]$RequestedVersion) {
  # A local directory or file URL keeps CI and offline installs working.
  $base = if ($ReleaseBaseUrl) { $ReleaseBaseUrl.TrimEnd('/') } else { "https://github.com/$Repo/releases/download/v$RequestedVersion" }
  if ($base.StartsWith('file://')) {
    $localPath = $base.Substring('file://'.Length)
    if ($localPath -match '^/[A-Za-z]:') { $localPath = $localPath.Substring(1) }
    return ([System.Uri]::UnescapeDataString($localPath)).Replace('\', '/')
  }
  return $base
}

function Resolve-LatestVersion {
  # The override keeps the version lookup testable without GitHub access.
  if (Test-Path Env:SOLO_LATEST_VERSION_URL) {
    $release = Invoke-RestMethod -Uri $env:SOLO_LATEST_VERSION_URL -Headers @{ 'User-Agent' = 'solo-installer' }
    if ($release.tag_name) { return ($release.tag_name -replace '^v', '') }
    Stop-Install 'the latest-version endpoint returned no tag_name'
  }
  try {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ 'User-Agent' = 'solo-installer' }
    if ($release.tag_name) { return ($release.tag_name -replace '^v', '') }
  } catch {
    Write-Verbose "GitHub API lookup failed, falling back to the releases/latest redirect: $($_.Exception.Message)"
  }
  $params = @{ Uri = "https://github.com/$Repo/releases/latest"; MaximumRedirection = 0 }
  if ((Get-Command Invoke-WebRequest).Parameters.ContainsKey('SkipHttpErrorCheck')) {
    $params['SkipHttpErrorCheck'] = $true
  }
  $response = Invoke-WebRequest @params
  $location = $response.Headers.Location
  if (-not $location) { Stop-Install 'could not determine the latest release version; pass -Version' }
  $tag = ($location.ToString().TrimEnd('/') -split '/')[-1]
  if ($tag -notmatch '^v?[A-Za-z0-9._-]+$') { Stop-Install "unexpected latest-release redirect: $location" }
  return ($tag -replace '^v', '')
}

function Copy-ReleaseAsset([string]$Url, [string]$Destination) {
  if ($Url -notmatch '^[A-Za-z][A-Za-z0-9+.-]*://' -or $Url.StartsWith('file://')) {
    $source = if ($Url.StartsWith('file://')) { ([System.Uri]$Url).LocalPath } else { $Url }
    if (-not (Test-Path -LiteralPath $source)) { Stop-Install "file not found: $source" }
    Copy-Item -LiteralPath $source -Destination $Destination -Force
    return
  }
  Invoke-WebRequest -Uri $Url -OutFile $Destination -UseBasicParsing
}

$processorArchitecture = if (Test-Path Env:PROCESSOR_ARCHITECTURE) { $env:PROCESSOR_ARCHITECTURE } else { '' }
if ($processorArchitecture -notmatch 'AMD64|ARM64') {
  Stop-Install "unsupported processor architecture: $processorArchitecture"
}
$Arch = if ($processorArchitecture -eq 'ARM64') { 'arm64' } else { 'amd64' }

if (-not $BinDir) {
  $BinDir = Join-Path $env:USERPROFILE '.solo\bin'
}

if (-not $Version) {
  $Version = Resolve-LatestVersion
}
$AssetVersion = $Version -replace '^v', ''
if ($AssetVersion -notmatch '^[A-Za-z0-9._-]+$') {
  Stop-Install "invalid release version: $Version"
}

$Asset = "solo_${AssetVersion}_windows_${Arch}.zip"
$BaseUrl = Get-ReleaseBaseUrl $AssetVersion
$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("solo-install-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null

try {
  Write-Step "downloading Solo $AssetVersion for windows/$Arch..."
  Copy-ReleaseAsset "$BaseUrl/$Asset" (Join-Path $TempDir $Asset)
  Copy-ReleaseAsset "$BaseUrl/checksums.txt" (Join-Path $TempDir 'checksums.txt')

  $checksums = Get-Content -LiteralPath (Join-Path $TempDir 'checksums.txt')
  $expected = $null
  foreach ($line in $checksums) {
    $fields = $line -split '\s+'
    if ($fields.Count -ge 2 -and $fields[-1].TrimStart('*') -eq $Asset) { $expected = $fields[0]; break }
  }
  if (-not $expected) { Stop-Install "release checksum not found for $Asset" }

  $actual = (Get-FileHash -LiteralPath (Join-Path $TempDir $Asset) -Algorithm SHA256).Hash
  if ($actual -ne $expected.ToUpperInvariant()) {
    Stop-Install "checksum verification failed for $Asset"
  }

  Expand-Archive -LiteralPath (Join-Path $TempDir $Asset) -DestinationPath $TempDir -Force

  foreach ($binary in @('solo.exe', 'solo-daemon.exe')) {
    if (-not (Test-Path -LiteralPath (Join-Path $TempDir $binary) -PathType Leaf)) {
      Stop-Install "release is missing $binary"
    }
  }

  New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
  Copy-Item -LiteralPath (Join-Path $TempDir 'solo.exe') -Destination (Join-Path $BinDir 'solo.exe') -Force
  Copy-Item -LiteralPath (Join-Path $TempDir 'solo-daemon.exe') -Destination (Join-Path $BinDir 'solo-daemon.exe') -Force

  if (-not $NoPathUpdate) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $entries = @()
    if ($userPath) { $entries = $userPath -split ';' | Where-Object { $_ } }
    if ($entries -notcontains $BinDir) {
      $updated = (@($entries) + $BinDir) -join ';'
      [Environment]::SetEnvironmentVariable('Path', $updated, 'User')
      $env:Path = "$BinDir;$env:Path"
      Write-Step "added $BinDir to the user PATH"
    }
  }

  Write-Step "installed solo and solo-daemon $AssetVersion in $BinDir"
} finally {
  Remove-Item -LiteralPath $TempDir -Recurse -Force -ErrorAction SilentlyContinue
}

if ($Connect) {
  if (-not $Server) { Stop-Install 'connect requires -Server' }
  if (-not $ComputerId) { Stop-Install 'connect requires -ComputerId' }
  if (-not $Token) { Stop-Install 'connect requires -Token' }

  $solo = Join-Path $BinDir 'solo.exe'
  & $solo daemon connect --server $Server --computer-id $ComputerId --token $Token --profile $ComputerId
  if ($LASTEXITCODE -ne 0) {
    Stop-Install "solo daemon connect failed with exit code $LASTEXITCODE"
  }

  if ($Autostart) {
    # Pairing wrote the credential the logon task needs, so register it now.
    & (Join-Path $PSScriptRoot 'autostart-daemon.ps1') -Enable -BinDir $BinDir
  }
}
