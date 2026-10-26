<#
.SYNOPSIS
  Installs (or removes) wslc-go: wslc-compose.exe on the user PATH, and a
  `wslc` PowerShell alias pointing at the wrapper so `wslc compose ...` works
  while every other subcommand still reaches the real wslc CLI.

.EXAMPLE
  irm https://raw.githubusercontent.com/gjuchault/wslc-compose-go/main/install.ps1 | iex

.EXAMPLE
  & ([scriptblock]::Create((irm https://raw.githubusercontent.com/gjuchault/wslc-compose-go/main/install.ps1))) -Version v0.3.0
#>
param(
    [string]$Version = 'latest',
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA 'Programs\wslc-go'),
    [switch]$NoAlias,
    [switch]$Uninstall
)

$ErrorActionPreference = 'Stop'
$repo = 'gjuchault/wslc-compose-go'
$beginMarker = '# >>> wslc-go >>>'
$endMarker = '# <<< wslc-go <<<'

# The machine PATH comes before the user PATH, so a wslc.exe on the user PATH
# would be shadowed by C:\Program Files\WSL\wslc.exe: an alias wins over both.
# Both Windows PowerShell 5.1 and PowerShell 7 profiles are updated.
$documents = [Environment]::GetFolderPath('MyDocuments')
$profiles = @(
    (Join-Path $documents 'WindowsPowerShell\profile.ps1'),
    (Join-Path $documents 'PowerShell\profile.ps1')
)

function Remove-ProfileBlock([string]$path) {
    if (-not (Test-Path $path)) { return }
    $content = Get-Content -Raw $path
    $pattern = '(?s)\r?\n?' + [regex]::Escape($beginMarker) + '.*?' + [regex]::Escape($endMarker) + '\r?\n?'
    $updated = [regex]::Replace($content, $pattern, [Environment]::NewLine)
    if ($updated -ne $content) { Set-Content -Path $path -Value $updated.TrimEnd() -Encoding utf8 }
}

function Set-UserPath([string[]]$entries) {
    [Environment]::SetEnvironmentVariable('Path', ($entries -join ';'), 'User')
}

$userPath = @([Environment]::GetEnvironmentVariable('Path', 'User') -split ';' | Where-Object { $_ })

if ($Uninstall) {
    $profiles | ForEach-Object { Remove-ProfileBlock $_ }
    Set-UserPath ($userPath | Where-Object { $_.TrimEnd('\') -ne $InstallDir.TrimEnd('\') })
    if (Test-Path $InstallDir) { Remove-Item -Recurse -Force $InstallDir }
    Remove-Item -Force Alias:wslc -ErrorAction SilentlyContinue
    Write-Host 'wslc-go uninstalled. Open a new terminal to refresh PATH.'
    return
}

$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$asset = "wslc-go_windows_$arch.zip"
$url = if ($Version -eq 'latest') {
    "https://github.com/$repo/releases/latest/download/$asset"
} else {
    "https://github.com/$repo/releases/download/$Version/$asset"
}

[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Downloading $url"
    Invoke-WebRequest -Uri $url -OutFile (Join-Path $tmp $asset) -UseBasicParsing
    Expand-Archive -Path (Join-Path $tmp $asset) -DestinationPath $tmp
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item -Force -Path (Join-Path $tmp "wslc-go_windows_$arch\*") -Destination $InstallDir
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

if ($userPath -notcontains $InstallDir) {
    Set-UserPath ($userPath + $InstallDir)
}
if (($env:Path -split ';') -notcontains $InstallDir) { $env:Path += ";$InstallDir" }

if (-not $NoAlias) {
    $wrapper = Join-Path $InstallDir 'wslc.exe'
    $block = "$beginMarker`r`nSet-Alias -Name wslc -Value '$wrapper'`r`n$endMarker"
    foreach ($path in $profiles) {
        Remove-ProfileBlock $path
        New-Item -ItemType Directory -Force -Path (Split-Path $path) | Out-Null
        Add-Content -Path $path -Value "`r`n$block" -Encoding utf8
    }
    Set-Alias -Name wslc -Value $wrapper -Scope Global
    if ((Get-ExecutionPolicy) -in 'Restricted', 'AllSigned') {
        Write-Warning ("Your execution policy blocks profile scripts, so the wslc alias won't load in new terminals. " +
            'Run: Set-ExecutionPolicy -Scope CurrentUser RemoteSigned')
    }
}

Write-Host "wslc-go installed to $InstallDir"
& (Join-Path $InstallDir 'wslc-compose.exe') --version
