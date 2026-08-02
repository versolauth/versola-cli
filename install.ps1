# Installs versola-cli for Windows, no administrator rights required.
#
# Usage:
#   iwr https://raw.githubusercontent.com/versolauth/versola-cli/main/install.ps1 -useb | iex
#
# To pin a version instead of installing the latest release:
#   $env:VERSOLA_VERSION = "v0.1.0"
#   iwr https://raw.githubusercontent.com/versolauth/versola-cli/main/install.ps1 -useb | iex

$ErrorActionPreference = "Stop"

# On any failure below, use "return" rather than "exit". This script is
# meant to be run via "iwr ... | iex", which evaluates it in the CURRENT
# PowerShell session rather than a child process -- "exit" there closes the
# user's whole terminal window, not just this script.

$Repo = "versolauth/versola-cli"
$InstallDir = "$env:LOCALAPPDATA\versola"
$BinName = "versola.exe"
$Asset = "versola-windows-amd64.exe"

# Only amd64 builds exist today. This only rejects 32-bit Windows -- it does
# NOT reject Windows on ARM, since Windows 11 on ARM runs x64 binaries via
# built-in emulation, so the published amd64 .exe works fine there too.
# Checking the OS via [Environment]::Is64BitOperatingSystem rather than
# $env:PROCESSOR_ARCHITECTURE -- that env var reflects the current
# PowerShell *process's* architecture, and under WOW64 (32-bit PowerShell on
# 64-bit Windows) it reports "x86" even though the OS itself is 64-bit.
if (-not [Environment]::Is64BitOperatingSystem) {
    Write-Error "Unsupported architecture: only 64-bit Windows is supported (amd64 builds only)"
    return
}

# Resolve version: $env:VERSOLA_VERSION if set, otherwise the latest release.
# Note: GitHub's "latest release" API excludes pre-releases, so this will
# fail to find anything until a non-pre-release build is published -- set
# $env:VERSOLA_VERSION explicitly until then.
$Version = $env:VERSOLA_VERSION
if (-not $Version) {
    try {
        $Release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
        $Version = $Release.tag_name
    } catch {
        $Version = $null
    }
    if (-not $Version) {
        Write-Error 'Could not determine the latest versola-cli version (no non-pre-release exists yet -- set $env:VERSOLA_VERSION explicitly, e.g. "v0.1.0-beta")'
        return
    }
}

Write-Host "Installing versola-cli $Version for windows/amd64..."

$BaseUrl = "https://github.com/$Repo/releases/download/$Version"
$TmpDir = Join-Path $env:TEMP "versola-install-$([guid]::NewGuid())"
New-Item -ItemType Directory -Path $TmpDir | Out-Null

try {
    $AssetPath = Join-Path $TmpDir $Asset
    Write-Host "Downloading $Asset..."
    try {
        Invoke-WebRequest -Uri "$BaseUrl/$Asset" -OutFile $AssetPath -UseBasicParsing
    } catch {
        Write-Error "Could not download $Asset for release $Version (check that it exists: $BaseUrl)"
        return
    }

    # Verify checksum if this release published one. Older releases (e.g.
    # v0.1.0-beta) didn't, so a missing checksums.txt (HTTP 404) is a soft
    # skip, not an error. A non-404 failure (network blip, proxy, DNS, etc.)
    # is NOT the same thing -- it means verification didn't happen, not that
    # it wasn't needed, so that case gets a distinct warning instead of
    # silently looking like the former.
    $ChecksumsPath = Join-Path $TmpDir "checksums.txt"
    $ChecksumsStatus = "ok"
    try {
        Invoke-WebRequest -Uri "$BaseUrl/checksums.txt" -OutFile $ChecksumsPath -UseBasicParsing
    } catch {
        $StatusCode = $null
        if ($_.Exception.Response) {
            $StatusCode = [int]$_.Exception.Response.StatusCode
        }
        if ($StatusCode -eq 404) {
            $ChecksumsStatus = "missing"
        } else {
            $ChecksumsStatus = "error"
            $ChecksumsError = $_.Exception.Message
        }
    }

    if ($ChecksumsStatus -eq "ok") {
        Write-Host "Verifying checksum..."
        # Exact filename match per line, not a substring regex search -- avoids
        # matching the wrong entry if two asset names happen to share a substring.
        # Also strips a leading "*" (sha256sum binary-mode marker) or "./"
        # (relative-path prefix) before comparing, so verification isn't
        # silently skipped just because checksums.txt used one of those
        # conventions.
        $Expected = $null
        foreach ($ChecksumLine in Get-Content $ChecksumsPath) {
            $Fields = $ChecksumLine -split '\s+', 2
            if ($Fields.Count -eq 2) {
                $Name = $Fields[1].Trim() -replace '^\*', '' -replace '^\./', ''
                if ($Name -eq $Asset) {
                    $Expected = $Fields[0]
                    break
                }
            }
        }
        if (-not $Expected) {
            Write-Warning "checksums.txt has no entry for $Asset, skipping verification"
        } else {
            $Actual = (Get-FileHash -Path $AssetPath -Algorithm SHA256).Hash.ToLower()
            if ($Expected.ToLower() -ne $Actual) {
                Write-Error "Checksum mismatch for $Asset`n  expected: $Expected`n  actual:   $Actual"
                return
            }
            Write-Host "Checksum OK."
        }
    } elseif ($ChecksumsStatus -eq "missing") {
        Write-Host "Note: $Version did not publish checksums.txt, skipping verification."
    } else {
        Write-Warning "Could not fetch checksums.txt ($ChecksumsError), skipping verification"
    }

    # Install.
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $DestPath = Join-Path $InstallDir $BinName
    Move-Item -Path $AssetPath -Destination $DestPath -Force

    Write-Host "Installed to $DestPath"

    # Make sure it's actually runnable as a bare command. Exact-match each PATH
    # entry rather than a substring check -- "-like *$InstallDir*" would also
    # match an unrelated folder that merely contains InstallDir as a substring
    # (e.g. "...\versola-old"), wrongly conclude it's already there, and skip
    # adding it.
    $UserPath = [Environment]::GetEnvironmentVariable("PATH", "User")
    $PathEntries = @()
    if ($UserPath) { $PathEntries = $UserPath -split ';' }
    $AlreadyOnPath = $PathEntries | Where-Object { $_.TrimEnd('\') -ieq $InstallDir.TrimEnd('\') }

    if (-not $AlreadyOnPath) {
        $NewPath = if ($UserPath) { "$UserPath;$InstallDir" } else { $InstallDir }
        [Environment]::SetEnvironmentVariable("PATH", $NewPath, "User")
        Write-Host ""
        Write-Host "Added $InstallDir to your PATH."
        Write-Host "Restart your terminal, then run 'versola doctor' to get started."
    } else {
        Write-Host ""
        Write-Host "Done. Run 'versola doctor' to get started."
    }
} finally {
    Remove-Item -Path $TmpDir -Recurse -Force -ErrorAction SilentlyContinue
}
