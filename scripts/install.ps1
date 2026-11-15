<#
.SYNOPSIS
  Install, update or uninstall FastRouter for the current user. No admin needed.

.DESCRIPTION
  Copies fastrouter.exe to %LOCALAPPDATA%\FastRouter (or -InstallDir).
  Optionally adds that folder to the *user* PATH, registers a per-user logon
  shortcut (fastrouter service install) and starts it.
  Your config and data in %USERPROFILE%\.fastrouter are never touched.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\install.ps1 -AddToPath -Startup
.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\install.ps1 -Uninstall
#>
[CmdletBinding()]
param(
    # fastrouter .exe to install. Default: newest fastrouter*.exe next to this script or in ..\dist
    [string]$Source,
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA 'FastRouter'),
    # Add InstallDir to the user PATH
    [switch]$AddToPath,
    # Start FastRouter at logon (fastrouter service install)
    [switch]$Startup,
    # Start FastRouter now, minimized
    [switch]$Run,
    [switch]$Uninstall,
    # On install/update or -Uninstall: stop a running FastRouter from InstallDir instead of aborting
    [switch]$Force
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$exe = Join-Path $InstallDir 'fastrouter.exe'

function Get-UserPath {
    $key = Get-Item -LiteralPath 'HKCU:\Environment'
    # DoNotExpand keeps entries like %USERPROFILE%\bin intact.
    return [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
}

function Set-UserPath([string]$value) {
    Set-ItemProperty -LiteralPath 'HKCU:\Environment' -Name 'Path' -Value $value -Type ExpandString
    # Any user-scope SetEnvironmentVariable broadcasts WM_SETTINGCHANGE so new shells see the change.
    [Environment]::SetEnvironmentVariable('FASTROUTER_INSTALL_TMP', $null, 'User')
}

function Get-PathEntries([string]$path) {
    return @($path -split ';' | Where-Object { $_ -ne '' })
}

function Test-SameDir([string]$a, [string]$b) {
    return ([Environment]::ExpandEnvironmentVariables($a).TrimEnd('\') -ieq $b.TrimEnd('\'))
}

function Stop-Installed {
    $running = @(Get-Process -Name 'fastrouter' -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -and (Test-SameDir $_.Path $exe) })
    if ($running.Count -eq 0) { return }
    if (-not $Force) {
        throw "FastRouter is running from $exe (pid $($running.Id -join ', ')). Stop it first, or re-run with -Force."
    }
    $running | Stop-Process -Force
    Start-Sleep -Milliseconds 500
}

if ($Uninstall) {
    Stop-Installed
    if (Test-Path -LiteralPath $exe) {
        & $exe service uninstall
    }
    $entries = Get-PathEntries (Get-UserPath)
    $kept = @($entries | Where-Object { -not (Test-SameDir $_ $InstallDir) })
    if ($kept.Count -ne $entries.Count) {
        Set-UserPath ($kept -join ';')
        Write-Host "Removed $InstallDir from your user PATH."
    }
    if (Test-Path -LiteralPath $InstallDir) {
        Remove-Item -LiteralPath $InstallDir -Recurse -Force
        Write-Host "Removed $InstallDir."
    }
    Write-Host "Uninstalled. Your config and data in $(Join-Path $HOME '.fastrouter') were left in place."
    return
}

if (-not $Source) {
    $candidates = @(@(
        Get-ChildItem -LiteralPath $PSScriptRoot -Filter 'fastrouter*.exe' -File -ErrorAction SilentlyContinue
        Get-ChildItem -LiteralPath (Join-Path $PSScriptRoot '..\dist') -Filter 'fastrouter*windows_amd64.exe' -File -ErrorAction SilentlyContinue
    ) | Sort-Object LastWriteTime -Descending)
    if ($candidates.Count -eq 0) { throw 'No fastrouter*.exe found next to this script or in ..\dist. Pass -Source <path>.' }
    $Source = $candidates[0].FullName
}
if (-not (Test-Path -LiteralPath $Source -PathType Leaf)) { throw "Source not found: $Source" }

Stop-Installed
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item -LiteralPath $Source -Destination $exe -Force
Write-Host "Installed $Source -> $exe"
& $exe version

if ($AddToPath) {
    $current = Get-UserPath
    $entries = Get-PathEntries $current
    if (@($entries | Where-Object { Test-SameDir $_ $InstallDir }).Count -eq 0) {
        Set-UserPath ((@($entries) + $InstallDir) -join ';')
        Write-Host "Added $InstallDir to your user PATH (open a new terminal to use 'fastrouter')."
    } else {
        Write-Host "$InstallDir is already on your user PATH."
    }
}

if ($Startup) {
    & $exe service install
    if ($LASTEXITCODE -ne 0) { throw "fastrouter service install failed (exit $LASTEXITCODE)" }
}

if ($Run) {
    Start-Process -FilePath $exe -ArgumentList 'serve' -WorkingDirectory $InstallDir -WindowStyle Minimized
    Write-Host 'Started FastRouter (minimized) on http://127.0.0.1:20128'
}
