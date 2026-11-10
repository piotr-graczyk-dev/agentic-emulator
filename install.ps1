# Installs agemu on Windows (x86-64; Android only) from the latest GitHub release:
#
#   irm https://raw.githubusercontent.com/piotr-graczyk-dev/agentic-emulator/main/install.ps1 | iex
#
# $env:AGEMU_VERSION = "v0.2.0" picks a release, $env:AGEMU_PREFIX another place (default
# %LOCALAPPDATA%\Programs\agemu, binaries in its bin folder, which is added to the user's PATH).
# It checks the download against the release's SHA256SUMS, restarts a running agemu daemon on the
# new build (devices keep running) and runs `agemu doctor`.
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue" # Invoke-WebRequest is many times slower with the bar

function Die($msg) { Write-Host "install.ps1: $msg" -ForegroundColor Red; exit 1 }

$repo = "piotr-graczyk-dev/agentic-emulator"
$prefix = if ($env:AGEMU_PREFIX) { $env:AGEMU_PREFIX } else { Join-Path $env:LOCALAPPDATA "Programs\agemu" }
$version = if ($env:AGEMU_VERSION) { $env:AGEMU_VERSION } else { "latest" }

$arch = $env:PROCESSOR_ARCHITECTURE
if ($env:PROCESSOR_ARCHITEW6432) { $arch = $env:PROCESSOR_ARCHITEW6432 }
if ($arch -ne "AMD64") { Die "agemu runs on x86-64 Windows, not $arch" }

if ($version -eq "latest") {
	# The latest release's page redirects to .../releases/tag/<version>.
	try {
		$resp = Invoke-WebRequest -Uri "https://github.com/$repo/releases/latest" -Method Head -UseBasicParsing -MaximumRedirection 5
		$url = $resp.BaseResponse.ResponseUri.AbsoluteUri
		if (-not $url) { $url = $resp.BaseResponse.RequestMessage.RequestUri.AbsoluteUri }
	} catch { Die "no release found at https://github.com/$repo/releases (or GitHub is unreachable)" }
	$version = $url.Split("/")[-1]
	if (-not $version.StartsWith("v")) { Die "no release found at https://github.com/$repo/releases" }
}

$name = "agemu-$version-windows-amd64"
$base = if ($env:AGEMU_DOWNLOAD_BASE) { $env:AGEMU_DOWNLOAD_BASE } else { "https://github.com/$repo/releases/download/$version" }
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("agemu-install-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
	Write-Host "Downloading agemu $version"
	$zip = Join-Path $tmp "$name.zip"
	$sums = Join-Path $tmp "SHA256SUMS"
	try { Invoke-WebRequest -Uri "$base/$name.zip" -OutFile $zip -UseBasicParsing } catch { Die "no $name.zip in release $version" }
	try { Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile $sums -UseBasicParsing } catch { Die "no SHA256SUMS in release $version" }
	$line = Get-Content $sums | Where-Object { $_ -match "\s$([regex]::Escape("$name.zip"))$" } | Select-Object -First 1
	if (-not $line) { Die "SHA256SUMS has no entry for $name.zip" }
	$want = ($line -split "\s+")[0].ToLower()
	$got = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLower()
	if ($got -ne $want) { Die "checksum mismatch" }
	Expand-Archive -Path $zip -DestinationPath $tmp -Force

	$bin = Join-Path $prefix "bin"
	New-Item -ItemType Directory -Force -Path $bin | Out-Null
	# Windows cannot overwrite a program that runs (a running daemon, say), but it can rename it.
	# Copy the new files next to their targets first, then rename each old one away and move the
	# new one into place; leftovers from an earlier install are removed when nothing runs them.
	# Each displaced file gets its own name: one from an earlier install may still run (an MCP
	# session, or a daemon whose restart waits for a preparation).
	foreach ($b in "agemu.exe", "agemud.exe") {
		Get-ChildItem -Path $bin -Filter "$b.old-*" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
		Copy-Item -Force (Join-Path $tmp "$name\$b") (Join-Path $bin ".$b.new")
	}
	$stamp = [DateTime]::UtcNow.ToString("yyyyMMddHHmmssfff")
	foreach ($b in "agemu.exe", "agemud.exe") {
		$dst = Join-Path $bin $b
		if (Test-Path $dst) { Move-Item -Force $dst "$dst.old-$stamp" }
		Move-Item -Force (Join-Path $bin ".$b.new") $dst
		Remove-Item -Force -ErrorAction SilentlyContinue "$dst.old-$stamp"
	}
	Write-Host "Installed agemu.exe and agemud.exe $version to $bin"
	& (Join-Path $bin "agemu.exe") daemon restart --if-running

	$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
	if (-not $userPath) { $userPath = "" }
	if (-not (($userPath -split ";") -contains $bin)) {
		[Environment]::SetEnvironmentVariable("Path", (($userPath.TrimEnd(";") + ";" + $bin).TrimStart(";")), "User")
		$env:Path = "$env:Path;$bin"
		Write-Host ""
		Write-Host "Added $bin to your PATH; open a new terminal to use agemu there."
	}
	Write-Host ""
	& (Join-Path $bin "agemu.exe") doctor
} finally {
	Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $tmp
}
