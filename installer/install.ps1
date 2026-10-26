# DYLARIS installer for Windows.
#
# Runs the Linux installer (install.sh) inside WSL2: it sets up WSL with
# Ubuntu if needed, then downloads, verifies and starts install.sh there, which
# installs Docker Engine inside WSL and asks the same questions as on Linux.
# If Docker Desktop with WSL integration is already installed, install.sh finds
# Docker working and skips installing it.
#
# Run in an elevated PowerShell:
#   powershell -ExecutionPolicy Bypass -File install.ps1
#
# Fine for trying DYLARIS and for small private servers. For a server people
# play on around the clock, a Linux machine is the better home.

#Requires -Version 5.1
$ErrorActionPreference = 'Stop'
$Distro = 'Ubuntu-24.04'
$Base = 'https://github.com/dylaris-dev/platform/releases/download/installer-latest'

function Say($m)  { Write-Host $m }
function Ok($m)   { Write-Host "ok  $m" -ForegroundColor Green }
function Warn($m) { Write-Host "!!  $m" -ForegroundColor Yellow }
function Die($m)  { Write-Host "xx  $m" -ForegroundColor Red; exit 1 }
function Ask-YesNo($q, $def = 'y') {
    $hint = if ($def -eq 'y') { 'Y/n' } else { 'y/N' }
    $a = Read-Host "$q [$hint]"
    if ([string]::IsNullOrWhiteSpace($a)) { $a = $def }
    return $a.ToLower().StartsWith('y')
}

Say 'DYLARIS installer for Windows (runs the Linux installer inside WSL2)'

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Die 'Run this in an elevated PowerShell (right-click > Run as administrator).'
}
$build = [Environment]::OSVersion.Version.Build
if ($build -lt 19041) { Die "Windows build $build is too old for WSL2; Windows 10 2004 (19041) or newer is needed." }
Ok "Windows build $build"

# 1. WSL with an Ubuntu distribution.
$wslOk = $true
try { $null = & wsl.exe --status 2>$null; if ($LASTEXITCODE -ne 0) { $wslOk = $false } } catch { $wslOk = $false }
$distros = @()
if ($wslOk) { $distros = (& wsl.exe -l -q 2>$null) -replace "`0", '' | Where-Object { $_ -match '\S' } | ForEach-Object { $_.Trim() } }
if (-not ($distros -contains $Distro)) {
    if (-not (Ask-YesNo "WSL2 with $Distro is needed. Install it now?")) { Die 'Stopped; nothing was changed.' }
    & wsl.exe --install -d $Distro
    if ($LASTEXITCODE -ne 0) { Die 'wsl --install failed. Run it by hand: wsl --install -d Ubuntu-24.04' }
    Warn "WSL and $Distro were installed. Restart Windows if asked, open '$Distro' once from the Start menu to create your Linux user, then run this installer again."
    exit 0
}
Ok "WSL2 with $Distro"

# 2. systemd inside WSL: Docker Engine runs as a systemd service there.
$conf = & wsl.exe -d $Distro -u root -- cat /etc/wsl.conf 2>$null
if (-not ($conf -match 'systemd\s*=\s*true')) {
    & wsl.exe -d $Distro -u root -- sh -c "printf '[boot]\nsystemd=true\n' >> /etc/wsl.conf"
    & wsl.exe --terminate $Distro | Out-Null
    Ok 'Switched systemd on inside WSL (needed for Docker)'
}

# 3. Mirrored networking, so players on the network reach the servers.
$wslconfig = Join-Path $env:USERPROFILE '.wslconfig'
$current = if (Test-Path $wslconfig) { Get-Content $wslconfig -Raw } else { '' }
if ($current -notmatch 'networkingMode\s*=\s*mirrored') {
    Say ''
    Say 'By default WSL is only reachable from this PC. With mirrored networking (Windows 11 22H2+)'
    Say 'DYLARIS listens on this PC''s own network address, so other machines and players can connect.'
    if (Ask-YesNo 'Switch WSL to mirrored networking?') {
        if ($current -match '\[wsl2\]') { $current = $current -replace '\[wsl2\]', "[wsl2]`r`nnetworkingMode=mirrored" }
        else { $current = $current + "`r`n[wsl2]`r`nnetworkingMode=mirrored`r`n" }
        Set-Content -Path $wslconfig -Value $current -Encoding ascii
        & wsl.exe --shutdown
        Ok "Wrote $wslconfig (WSL restarted)"
        Warn 'Windows Firewall: allow inbound TCP 25443, 25500 and 25600-25699 if other machines should connect.'
    }
}

# 4. Download, verify and run install.sh inside WSL as root.
$script = @"
set -e
cd /root
curl -fsSLO $Base/install.sh
curl -fsSLO $Base/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
exec bash install.sh
"@
$script = $script -replace "`r", ''   # bash chokes on Windows line endings
Say ''
Say "Starting the installer inside $Distro..."
& wsl.exe -d $Distro -u root -- bash -c $script
if ($LASTEXITCODE -ne 0) { Die "The installer stopped (exit $LASTEXITCODE). Run it again; it keeps what it already set up." }
Say ''
Ok 'Done. DYLARIS runs inside WSL; it starts with WSL. To keep it running, leave a WSL session open or enable WSL autostart.'
