# kaal installer (Windows) — fetches the prebuilt release binary for this
# platform from GitHub Releases via irm (Invoke-RestMethod), the same
# `kaal-windows-<arch>.exe` asset `kaal update`'s release-fetch path looks up.
#
# Usage:
#   irm https://raw.githubusercontent.com/shivamnarkar47/kaal/main/install.ps1 | iex
#
# Overrides:
#   $env:KAAL_VERSION  release tag to fetch (default: latest)
#   $env:INSTALL_DIR   install directory   (default: $env:LOCALAPPDATA\kaal)
$ErrorActionPreference = 'Stop'

$repo = 'shivamnarkar47/kaal'
$version = if ($env:KAAL_VERSION) { $env:KAAL_VERSION } else { 'latest' }

switch ([System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture) {
	'X64'   { $arch = 'amd64' }
	'Arm64' { $arch = 'arm64' }
	default { throw "kaal: unsupported architecture: $($_.ToString())" }
}

if ($version -eq 'latest') {
	$url = "https://github.com/$repo/releases/latest/download/kaal-windows-$arch.exe"
} else {
	$url = "https://github.com/$repo/releases/download/$version/kaal-windows-$arch.exe"
}

$dest = if ($env:INSTALL_DIR) {
	$env:INSTALL_DIR
} elseif ($env:LOCALAPPDATA) {
	Join-Path $env:LOCALAPPDATA 'kaal'
} else {
	Join-Path $env:USERPROFILE '.local\bin'
}
New-Item -ItemType Directory -Force -Path $dest | Out-Null
$exe = Join-Path $dest 'kaal.exe'
$tmp = Join-Path $dest ('.kaal-install-' + [guid]::NewGuid().ToString('N') + '.exe')

Write-Host "kaal: fetching $url"
Invoke-RestMethod -Uri $url -OutFile $tmp
# The downloaded file must answer --version like a kaal before we install it.
$probe = & $tmp --version 2>&1
if ($LASTEXITCODE -ne 0) {
	Remove-Item $tmp -Force
	throw "kaal: downloaded binary failed a --version probe: $probe"
}
Move-Item -Force $tmp $exe

# Add the install dir to the user PATH so 'kaal' works in new terminals.
$binDir = Split-Path -Parent $exe
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not $userPath) {
	$userPath = $binDir
} elseif ($userPath -notlike "*$binDir*") {
	$userPath = "$userPath;$binDir"
}
if ($userPath -ne [Environment]::GetEnvironmentVariable('Path', 'User')) {
	[Environment]::SetEnvironmentVariable('Path', $userPath, 'User')
	Write-Host "kaal: added $binDir to your user PATH (open a new terminal)."
}

Write-Host "kaal installed at $exe"
Write-Host "Run 'kaal' to start; 'kaal update' self-updates the binary."
