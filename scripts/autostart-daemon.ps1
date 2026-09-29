<#
.SYNOPSIS
Manages the Solo Daemon Windows logon task.

.DESCRIPTION
Registers (or removes) a per-user Scheduled Task that starts the managed Solo
Daemon at logon, so a paired Windows Computer reconnects without a manual
launch. The task starts solo-daemon.exe directly, which reuses the credential
written by `solo daemon connect`; no enrollment token is needed after pairing.

The task runs with the registering user's token, without stored credentials, so
it only ever starts when that user logs on interactively.

.PARAMETER Enable
Register or update the logon task.

.PARAMETER Disable
Stop the task if it is running and delete it.

.PARAMETER Status
Report whether the task exists and whether the Daemon answers.

.PARAMETER Check
Report the task state as the process exit code: 0 when the task exists, 1 when
it does not. Intended for scripts and tests.

.EXAMPLE
powershell -File scripts/autostart-daemon.ps1 -Enable

.EXAMPLE
powershell -File scripts/autostart-daemon.ps1 -Status
#>
[CmdletBinding()]
param(
  [switch]$Enable,
  [switch]$Disable,
  [switch]$Status,
  [switch]$Check,
  # Directory holding solo.exe and solo-daemon.exe. Defaults to %USERPROFILE%\.solo\bin.
  [string]$BinDir = $(if ($env:SOLO_BIN_DIR) { $env:SOLO_BIN_DIR } else { '' }),
  # Daemon profile to start. The UI pairing command runs with
  # --profile <computer-id>, so 'auto' (the default) prefers the default profile
  # when it is paired and otherwise uses the most recently paired profile.
  [string]$Profile = 'auto',
  # Scheduled Task name.
  [string]$TaskName = 'Solo Daemon'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Test-WindowsHost {
  if (Test-Path Variable:IsWindows) { return [bool]$IsWindows }
  return ($env:OS -eq 'Windows_NT')
}

if (-not (Test-WindowsHost)) {
  throw 'solo: daemon autostart is only supported on Windows'
}

if (-not $BinDir) {
  $BinDir = Join-Path $env:USERPROFILE '.solo\bin'
}
$daemonPath = Join-Path $BinDir 'solo-daemon.exe'
$daemonPort = if ($env:DAEMON_PORT) { $env:DAEMON_PORT } else { '8081' }

function Get-PairedCredentialPath {
  # Mirrors cmd/solo: the default profile lives in .solo\daemon, every named
  # profile in .solo\daemons\<profile>.
  if ($Profile -ne 'auto') {
    if ($Profile -eq 'default') {
      return (Join-Path $env:USERPROFILE '.solo\daemon\credentials.json')
    }
    return (Join-Path $env:USERPROFILE ".solo\daemons\$Profile\credentials.json")
  }
  $default = Join-Path $env:USERPROFILE '.solo\daemon\credentials.json'
  if (Test-Path -LiteralPath $default -PathType Leaf) { return $default }
  $profilesRoot = Join-Path $env:USERPROFILE '.solo\daemons'
  if (-not (Test-Path -LiteralPath $profilesRoot -PathType Container)) { return $default }
  $newest = Get-ChildItem -LiteralPath $profilesRoot -Directory -ErrorAction SilentlyContinue |
    ForEach-Object {
      $candidate = Join-Path $_.FullName 'credentials.json'
      if (Test-Path -LiteralPath $candidate -PathType Leaf) { Get-Item -LiteralPath $candidate }
    } |
    Sort-Object LastWriteTime -Descending |
    Select-Object -First 1
  if ($newest) { return $newest.FullName }
  return $default
}

function Get-DaemonProfileArgument {
  # The Daemon finds its own state through SOLO_DAEMON_PROFILE; the CLI needs the
  # same profile so stop/status address the Daemon the task started.
  if ($Profile -ne 'auto') { return $Profile }
  $profileDir = Split-Path -Parent $resolvedCredentialPath
  if ((Split-Path -Leaf $profileDir) -eq 'daemon') { return 'default' }
  return (Split-Path -Leaf $profileDir)
}

function Get-DaemonPort {
  # A named profile records the port it started on; the default profile uses
  # DAEMON_PORT or 8081.
  if ($resolvedCredentialPath) {
    $portFile = Join-Path (Split-Path -Parent $resolvedCredentialPath) 'port'
    if (Test-Path -LiteralPath $portFile -PathType Leaf) {
      $recorded = (Get-Content -LiteralPath $portFile -Raw).Trim()
      if ($recorded) { return $recorded }
    }
  }
  return $daemonPort
}

function Test-TaskRegistered {
  # schtasks reports the state in the system language, so only its exit code is
  # a stable signal: 0 when the task exists, non-zero when it does not.
  $previous = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    $null = & schtasks.exe /query /tn $TaskName /fo list 2>&1
    return ($LASTEXITCODE -eq 0)
  } finally {
    $ErrorActionPreference = $previous
  }
}

function Test-DaemonHealth {
  try {
    $response = Invoke-WebRequest -Uri "http://127.0.0.1:$daemonPort/health" -UseBasicParsing -TimeoutSec 3
    return ($response.StatusCode -eq 200)
  } catch {
    return $false
  }
}

function Enable-Autostart {
  if (-not (Test-Path -LiteralPath $daemonPath -PathType Leaf)) {
    throw "solo: $daemonPath not found; install the Daemon first (scripts/install.ps1)"
  }
  $credentialPath = $resolvedCredentialPath
  if (-not (Test-Path -LiteralPath $credentialPath -PathType Leaf)) {
    throw "solo: no paired credential found (looked for $credentialPath); pair this Computer first (solo daemon connect)"
  }
  $daemonProfile = $resolvedProfile

  # schtasks applies its own quoting rules to /tr, which makes a nested
  # `powershell -Command "& 'path'"` fragile. Launch through a small batch file
  # in the Daemon's own directory so the task action stays one plain path.
  #
  # SOLO_DAEMON_STATE_DIR is what the Daemon uses to locate its state directory
  # and credential; sending only the profile would make it fall back to the
  # default profile, start unpaired, and bind the default port.
  $stateDir = Split-Path -Parent $credentialPath
  $launcherName = 'solo-daemon-launcher.cmd'
  $launcherPath = Join-Path $BinDir $launcherName
  $launcher = @(
    '@echo off',
    'rem Generated by scripts/autostart-daemon.ps1',
    "set SOLO_DAEMON_PROFILE=$daemonProfile",
    "set SOLO_DAEMON_STATE_DIR=$stateDir",
    "set SOLO_DAEMON_CREDENTIAL_FILE=$credentialPath",
    # /min rather than /b: /b keeps the child attached to cmd's console, which
    # kills it as soon as cmd exits (the task action returns immediately).
    'start "" /min "%~dp0solo-daemon.exe"'
  ) -join "`r`n"
  Set-Content -LiteralPath $launcherPath -Value $launcher -Encoding ASCII

  # at logon, run as the registering user only.
  $action = "cmd.exe /c `"$launcherPath`""
  $create = & schtasks.exe /create /f /tn $TaskName /sc onlogon /rl LIMITED /it /tr $action 2>&1
  if ($LASTEXITCODE -ne 0) {
    throw "solo: could not register the logon task: $create"
  }
  Write-Host "solo: registered the '$TaskName' logon task for $daemonPath (profile: $daemonProfile)"
  Write-Host 'solo: it starts at your next logon. Use -Status to inspect it or -Disable to remove it.'
}

function Disable-Autostart {
  if (-not (Test-TaskRegistered)) {
    Write-Host "solo: the '$TaskName' logon task is not registered"
    return
  }
  # /end first so a running Daemon is stopped before its task is removed.
  $previous = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    $null = & schtasks.exe /end /tn $TaskName 2>&1
    $delete = & schtasks.exe /delete /f /tn $TaskName 2>&1
    $deleted = ($LASTEXITCODE -eq 0)
  } finally {
    $ErrorActionPreference = $previous
  }
  if (-not $deleted) {
    throw "solo: could not delete the logon task: $delete"
  }
  $launcherPath = Join-Path $BinDir 'solo-daemon-launcher.cmd'
  Remove-Item -LiteralPath $launcherPath -Force -ErrorAction SilentlyContinue
  Write-Host "solo: removed the '$TaskName' logon task"
}

function Show-Status {
  if (Test-TaskRegistered) {
    Write-Host "solo: logon task '$TaskName' is registered"
    Write-Host "solo: run schtasks /query /tn `"$TaskName`" /fo list /v for its schedule and last result"
  } else {
    Write-Host "solo: logon task '$TaskName' is not registered"
  }
  $health = if (Test-DaemonHealth) { 'answering' } else { 'not answering' }
  Write-Host "solo: daemon on port $daemonPort is $health"
}

# Resolve the paired profile once, after the helpers above are defined. The path
# is made absolute so a later directory change in the caller cannot invalidate it.
$resolvedCredentialPath = Get-PairedCredentialPath
if ($resolvedCredentialPath) { $resolvedCredentialPath = [System.IO.Path]::GetFullPath($resolvedCredentialPath) }
$resolvedProfile = Get-DaemonProfileArgument
$daemonPort = Get-DaemonPort

if ($Check) {
  if (Test-TaskRegistered) { exit 0 } else { exit 1 }
}

$requested = @()
if ($Enable) { $requested += 'Enable' }
if ($Disable) { $requested += 'Disable' }
if ($Status) { $requested += 'Status' }
if ($requested.Count -eq 0) {
  throw 'solo: pass -Enable, -Disable, -Status, or -Check'
}
if ($requested.Count -gt 1) {
  throw "solo: pass only one of -Enable, -Disable, -Status (got: $($requested -join ', '))"
}

switch ($requested[0]) {
  'Enable' { Enable-Autostart }
  'Disable' { Disable-Autostart }
  'Status' { Show-Status }
}
