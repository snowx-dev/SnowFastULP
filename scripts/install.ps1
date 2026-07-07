param(
    [switch]$DryRun,
    [string]$Version = $env:SNOWFAST_VERSION,
    [string]$InstallDir = $env:SNOWFAST_INSTALL_DIR,
    [string]$ManifestUrl = $(if ($env:SNOWFAST_UPDATE_URL) { $env:SNOWFAST_UPDATE_URL } else { "https://sfu-update.snowx.dev/" }),
    [string]$RepoOwner = $(if ($env:SNOWFAST_REPO_OWNER) { $env:SNOWFAST_REPO_OWNER } else { "snowx-dev" }),
    [string]$RepoName = $(if ($env:SNOWFAST_REPO_NAME) { $env:SNOWFAST_REPO_NAME } else { "SnowFastULP" }),
    [string]$Ref = $(if ($env:SNOWFAST_REF) { $env:SNOWFAST_REF } else { "main" }),
    [string]$DocsUrl = $(if ($env:SNOWFAST_DOCS_URL) { $env:SNOWFAST_DOCS_URL } else { "https://snowfast.snowx.dev/docs" })
)

$ErrorActionPreference = "Stop"

function Write-Section {
    param([string]$Message)
    Write-Host ""
    Write-Host "==> $Message" -ForegroundColor Cyan
}

function Write-Ok {
    param([string]$Message)
    Write-Host "[ok] $Message" -ForegroundColor Green
}

function Write-Skip {
    param([string]$Message)
    Write-Host "[skip] $Message" -ForegroundColor Yellow
}

function Write-Warn {
    param([string]$Message)
    Write-Host "[warn] $Message" -ForegroundColor Yellow
}

function Normalize-Version {
    param([string]$Value)
    return $Value.TrimStart("v")
}

function Get-UpdateManifest {
    $manifest = Invoke-RestMethod -Uri $ManifestUrl -Headers @{ "User-Agent" = "SnowFastULP-Installer" }
    if (-not $manifest.version) {
        throw "update manifest has no version"
    }
    return $manifest
}

function Resolve-InstallDir {
    if ($InstallDir) {
        return $InstallDir
    }
    $localAppData = $env:LOCALAPPDATA
    if (-not $localAppData) {
        $localAppData = Join-Path $HOME "AppData\Local"
    }
    return Join-Path $localAppData "SnowFast\bin"
}

function Resolve-ConfigPath {
    $appData = $env:APPDATA
    if (-not $appData) {
        $appData = Join-Path $HOME "AppData\Roaming"
    }
    return Join-Path $appData "snowfast\config.toml"
}

function Test-PathEntry {
    param(
        [string]$PathValue,
        [string]$Entry
    )
    $separator = [IO.Path]::PathSeparator
    return ($PathValue -split [regex]::Escape([string]$separator) | Where-Object { $_.TrimEnd("\") -ieq $Entry.TrimEnd("\") }).Count -gt 0
}

function Download-File {
    param(
        [string]$Uri,
        [string]$OutFile
    )
    Invoke-WebRequest -Uri $Uri -OutFile $OutFile -UseBasicParsing
}

function Get-ManifestAsset {
    param(
        [object]$Manifest,
        [string]$AssetName
    )
    $property = $Manifest.assets.PSObject.Properties[$AssetName]
    if (-not $property) {
        throw "update manifest missing asset $AssetName"
    }
    return $property.Value
}

function Resolve-AssetUrl {
    param(
        [object]$Asset,
        [string]$Version,
        [string]$AssetName
    )
    if ($Asset.url) {
        return $Asset.url
    }
    return "https://github.com/$RepoOwner/$RepoName/releases/download/v$Version/$AssetName"
}

function Assert-Checksum {
    param(
        [string]$ExpectedHash,
        [string]$AssetName,
        [string]$Path
    )
    $expected = $ExpectedHash.ToLowerInvariant()
    if ($expected.Length -ne 64) {
        throw "manifest checksum for $AssetName is not a SHA256 hex digest"
    }
    $actual = (Get-FileHash -Algorithm SHA256 -Path $Path).Hash.ToLowerInvariant()
    if ($expected -ne $actual) {
        throw "checksum mismatch for $AssetName"
    }
}

function Install-Binary {
    param(
        [string]$Source,
        [string]$Destination
    )
    $tmp = "$Destination.tmp.$PID"
    Copy-Item -Path $Source -Destination $tmp -Force
    Move-Item -Path $tmp -Destination $Destination -Force
}

function Add-UserPath {
    param([string]$Dir)

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (Test-PathEntry -PathValue $userPath -Entry $Dir) {
        return "already configured"
    }

    $newPath = if ([string]::IsNullOrWhiteSpace($userPath)) {
        $Dir
    } else {
        "$userPath$([IO.Path]::PathSeparator)$Dir"
    }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    $env:Path = "$env:Path$([IO.Path]::PathSeparator)$Dir"
    return "updated user PATH"
}

Write-Section "SnowFastULP Windows installer"

if (-not [Environment]::Is64BitOperatingSystem) {
    throw "unsupported platform: Windows 64-bit is required"
}

$platform = "windows-amd64"
$resolvedInstallDir = Resolve-InstallDir
$configPath = Resolve-ConfigPath
$rawBase = "https://raw.githubusercontent.com/$RepoOwner/$RepoName/$Ref"
$manifest = Get-UpdateManifest

if ($Version) {
    $resolvedVersion = Normalize-Version $Version
} else {
    $resolvedVersion = Normalize-Version $manifest.version
}

$releaseTag = "v$resolvedVersion"
$releaseBase = "https://github.com/$RepoOwner/$RepoName/releases/download/$releaseTag"

Write-Host "Repository : $RepoOwner/$RepoName"
Write-Host "Version    : $resolvedVersion"
Write-Host "Platform   : $platform"
Write-Host "Install dir: $resolvedInstallDir"
Write-Host "Config     : $configPath"
Write-Host "Manifest   : $ManifestUrl"

$assets = @(
    @{ Asset = "SnowFastULP-$resolvedVersion-$platform.exe"; Command = "sfu.exe" },
    @{ Asset = "SnowFastSearch-$resolvedVersion-$platform.exe"; Command = "sfs.exe" },
    @{ Asset = "SnowFastLog-$resolvedVersion-$platform.exe"; Command = "sfl.exe" }
)

if ($DryRun) {
    Write-Section "Dry run"
    Write-Host "Would download:"
    foreach ($item in $assets) {
        $assetInfo = Get-ManifestAsset -Manifest $manifest -AssetName $item.Asset
        if (-not $assetInfo.sha256) {
            throw "update manifest missing checksum for $($item.Asset)"
        }
        Write-Host "  $(Resolve-AssetUrl -Asset $assetInfo -Version $resolvedVersion -AssetName $item.Asset)"
    }
    Write-Host "Would install:"
    foreach ($item in $assets) {
        Write-Host "  $(Join-Path $resolvedInstallDir $item.Command)"
    }
    Write-Host "Would create config if missing:"
    Write-Host "  $configPath"
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (Test-PathEntry -PathValue $userPath -Entry $resolvedInstallDir) {
        Write-Host "User PATH already contains install dir."
    } else {
        Write-Host "Would add install dir to the user PATH."
    }
    Write-Host ""
    Write-Ok "dry run complete"
    exit 0
}

$tmpDir = Join-Path ([IO.Path]::GetTempPath()) "snowfast-install-$PID"
New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

try {
    Write-Section "Downloading release assets"

    foreach ($item in $assets) {
        $assetInfo = Get-ManifestAsset -Manifest $manifest -AssetName $item.Asset
        $assetPath = Join-Path $tmpDir $item.Asset
        $assetUrl = Resolve-AssetUrl -Asset $assetInfo -Version $resolvedVersion -AssetName $item.Asset
        Download-File -Uri $assetUrl -OutFile $assetPath
        Assert-Checksum -ExpectedHash $assetInfo.sha256 -AssetName $item.Asset -Path $assetPath
        Write-Ok "verified $($item.Asset)"
    }

    Write-Section "Installing commands"

    New-Item -ItemType Directory -Path $resolvedInstallDir -Force | Out-Null
    foreach ($item in $assets) {
        $assetPath = Join-Path $tmpDir $item.Asset
        $dest = Join-Path $resolvedInstallDir $item.Command
        Install-Binary -Source $assetPath -Destination $dest
        Write-Ok "installed $($item.Command) -> $dest"
    }

    Write-Section "Writing config"

    $configStatus = "preserved existing"
    if (Test-Path -LiteralPath $configPath) {
        Write-Skip "config already exists: $configPath"
    } else {
        $configDir = Split-Path -Parent $configPath
        New-Item -ItemType Directory -Path $configDir -Force | Out-Null
        $examplePath = Join-Path $tmpDir "config.toml.example"
        Download-File -Uri "$rawBase/config.toml.example" -OutFile $examplePath
        Copy-Item -Path $examplePath -Destination $configPath -Force
        $configStatus = "created"
        Write-Ok "created config: $configPath"
    }

    Write-Section "Checking PATH"

    $pathStatus = Add-UserPath -Dir $resolvedInstallDir
    if ($pathStatus -eq "already configured") {
        Write-Ok "$resolvedInstallDir is already on the user PATH"
    } else {
        Write-Ok "added $resolvedInstallDir to the user PATH"
        Write-Warn "open a new terminal before running sfu, sfs, or sfl"
    }

    Write-Section "Installed"

    Write-Host "Commands:"
    Write-Host "  sfu  clean and deduplicate ULP/LPU text dumps"
    Write-Host "  sfs  search plain .txt dumps or compressed .zst libraries"
    Write-Host "  sfl  extract stealer logs into ULP lines or a library"
    Write-Host ""
    Write-Host "Docs:"
    Write-Host "  $DocsUrl"
    Write-Host ""
    Write-Host "Config:"
    Write-Host "  $configPath ($configStatus)"
    Write-Host ""
    Write-Host "Install dir:"
    Write-Host "  $resolvedInstallDir ($pathStatus)"
    Write-Host ""
    Write-Host "Try:"
    Write-Host "  sfu --version"
    Write-Host "  sfs --version"
    Write-Host "  sfl --version"
} finally {
    Remove-Item -LiteralPath $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
}
