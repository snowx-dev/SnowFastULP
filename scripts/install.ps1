param(
    [switch]$DryRun,
    [string]$Version = $env:SNOWFAST_VERSION,
    [string]$InstallDir = $env:SNOWFAST_INSTALL_DIR,
    [string]$ManifestUrl = $(if ($env:SNOWFAST_UPDATE_URL) { $env:SNOWFAST_UPDATE_URL } else { "https://sfu-update.snowx.dev/" }),
    [string]$ReleaseBase = $env:SNOWFAST_RELEASE_BASE,
    [string]$RepoOwner = $(if ($env:SNOWFAST_REPO_OWNER) { $env:SNOWFAST_REPO_OWNER } else { "snowx-dev" }),
    [string]$RepoName = $(if ($env:SNOWFAST_REPO_NAME) { $env:SNOWFAST_REPO_NAME } else { "SnowFastULP" }),
    # W11: config example must match the release being installed, not main.
    # $Ref is resolved after the version is known (release tag v<version> by
    # default; SNOWFAST_REF/SNOWFAST_RAW_BASE override).
    [string]$Ref = $env:SNOWFAST_REF,
    [string]$RawBase = $env:SNOWFAST_RAW_BASE,
    [string]$DocsUrl = $(if ($env:SNOWFAST_DOCS_URL) { $env:SNOWFAST_DOCS_URL } else { "https://snowfast.snowx.dev/docs" }),
    [int]$RequestTimeoutSec = $(if ($env:SNOWFAST_HTTP_TIMEOUT_SEC) { $env:SNOWFAST_HTTP_TIMEOUT_SEC } else { 30 })
)

$ErrorActionPreference = "Stop"
if ($RequestTimeoutSec -le 0) {
    throw "HTTP request timeout must be a positive number of seconds (got $RequestTimeoutSec)"
}

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
    $manifest = Invoke-RestMethod -Uri $ManifestUrl -Headers @{ "User-Agent" = "SnowFastULP-Installer" } -TimeoutSec $RequestTimeoutSec
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
    Invoke-WebRequest -Uri $Uri -OutFile $OutFile -UseBasicParsing -TimeoutSec $RequestTimeoutSec
}

# Returns the manifest entry for an asset, or $null if there is none. The
# assets map is advisory (url mirror support); W2 keeps checksums out of it.
function Get-ManifestAsset {
    param(
        [object]$Manifest,
        [string]$AssetName
    )
    if (-not $Manifest.PSObject.Properties["assets"]) {
        return $null
    }
    $property = $Manifest.assets.PSObject.Properties[$AssetName]
    if (-not $property) {
        return $null
    }
    return $property.Value
}

function Resolve-AssetUrl {
    param(
        [object]$Asset,
        [string]$Version,
        [string]$AssetName
    )
    if ($Asset -and $Asset.url) {
        return $Asset.url
    }
    # Same base the SHA256SUMS fetch uses (script-scope $releaseBase;
    # SNOWFAST_RELEASE_BASE overrides it for test hooks).
    return "$releaseBase/$AssetName"
}

# W2: checksums come from the release's SHA256SUMS artifact (`<64-hex>
# <whitespace> <name>` per line), never the manifest — a pinned version
# must never be validated against the LATEST manifest.
function Get-SumsChecksum {
    param(
        [string]$SumsPath,
        [string]$AssetName
    )
    foreach ($line in Get-Content -LiteralPath $SumsPath) {
        $parts = $line.Trim() -split '\s+' , 2
        if ($parts.Count -eq 2 -and $parts[1] -eq $AssetName -and $parts[0] -match '^[0-9a-fA-F]{64}$') {
            return $parts[0].ToLowerInvariant()
        }
    }
    return $null
}

function Assert-Checksum {
    param(
        [string]$ExpectedHash,
        [string]$AssetName,
        [string]$Path
    )
    if (-not $ExpectedHash) {
        throw "SHA256SUMS has no entry for $AssetName"
    }
    $expected = $ExpectedHash.ToLowerInvariant()
    if ($expected.Length -ne 64) {
        throw "checksum for $AssetName is not a SHA256 hex digest"
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
    # Test-only fault seam: force the replacement of one named command to
    # fail so the rollback path is exercised. Never set outside the tests.
    if ($env:SNOWFAST_TEST_FAIL_REPLACE_AT -and
        $Destination -like "*$env:SNOWFAST_TEST_FAIL_REPLACE_AT*") {
        throw "forced replacement failure for testing ($Destination)"
    }
    # Temp file next to the destination, cleaned up in finally — never a
    # predictable "$Destination.tmp.$PID" left behind on failure.
    $tmp = "$Destination.tmp.$PID.new"
    Copy-Item -Path $Source -Destination $tmp -Force
    try {
        Move-Item -Path $tmp -Destination $Destination -Force
    } catch {
        Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
        throw
    }
}

# H-16: the transaction journal. The catch-based rollback only runs while
# the installer process is alive; a crash, termination, or power loss after
# the first replacement bypasses it and left a mixed install whose snapshot
# the next run would take as its new baseline. The journal is written and
# flushed to disk BEFORE the first replacement (phase "replacing"), updated
# after each committed replacement (entry status "installed"), and set to
# phase "committed" once every replacement succeeded — so a later run can
# finish or roll back exactly this transaction. The journal lives inside the
# PID-named transaction dir, next to the backups it references.
function Write-TxJournal {
    param(
        [string]$Path,
        [object]$Journal
    )
    $json = $Journal | ConvertTo-Json -Depth 5
    # WriteAllText closes the file; the reopen+Flush($true) pushes the bytes
    # out of OS caches so a power loss cannot leave a journal claiming a
    # phase that never reached the disk.
    [System.IO.File]::WriteAllText($Path, $json)
    $fs = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
    try {
        $fs.Flush($true)
    } finally {
        $fs.Close()
    }
}

# H-16: startup recovery. Before a new transaction is opened, finish or roll
# back any transaction a previous installer run left behind in the install
# dir: phase "replacing" restores every previously-existing command from its
# backup and removes every newly-created one (an entry not yet marked
# "installed" may still hold a partially written binary — rolling it back is
# idempotent), phase "committed" only needs its dir removed. A transaction
# dir without a journal is from a crash before the first replacement (the
# journal precedes it), so it holds only backups and is removed outright.
# A recovery that cannot be completed keeps the dir (backups included) and
# fails loudly, exactly like the in-run rollback path.
function Recover-InstallTransactions {
    param([string]$InstallDir)

    $stale = @(Get-ChildItem -LiteralPath $InstallDir -Filter ".snowfast-install-txn-*" -Directory -Force -ErrorAction SilentlyContinue)
    foreach ($staleDir in $stale) {
        $dir = $staleDir.FullName
        $journalPath = Join-Path $dir "journal.json"
        if (-not (Test-Path -LiteralPath $journalPath)) {
            Write-Warn "removing interrupted-install transaction with no journal (nothing was replaced): $dir"
            Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
            continue
        }
        $journal = Get-Content -LiteralPath $journalPath -Raw | ConvertFrom-Json
        if ($journal.phase -eq "committed") {
            Write-Warn "finishing committed install transaction from an earlier run: $dir"
            Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
            continue
        }
        Write-Warn "recovering interrupted install (phase $($journal.phase)), rolling back to the pre-install state: $dir"
        $recoveryErrors = @()
        foreach ($entry in $journal.entries) {
            if ($entry.existed -and $entry.backup) {
                try {
                    Copy-Item -LiteralPath $entry.backup -Destination $entry.dest -Force
                } catch {
                    $recoveryErrors += "could not restore $($entry.command) from backup: $_"
                }
            } elseif (-not $entry.existed -and (Test-Path -LiteralPath $entry.dest)) {
                try {
                    Remove-Item -LiteralPath $entry.dest -Force
                } catch {
                    $recoveryErrors += "could not remove installed $($entry.command): $_"
                }
            }
        }
        if ($recoveryErrors.Count -gt 0) {
            throw "interrupted-install recovery incomplete, backups kept in $dir`: $($recoveryErrors -join '; ')"
        }
        Remove-Item -LiteralPath $dir -Recurse -Force
        Write-Ok "recovered interrupted install"
    }
}

function Add-UserPath {
    param([string]$Dir)

    # Tests can redirect this update to the child process and record calls;
    # normal installs continue to use the persistent user PATH.
    $scope = if ($env:SNOWFAST_TEST_PATH_SCOPE) { $env:SNOWFAST_TEST_PATH_SCOPE } else { "User" }
    $path = [Environment]::GetEnvironmentVariable("Path", $scope)
    if (Test-PathEntry -PathValue $path -Entry $Dir) {
        if ($env:SNOWFAST_TEST_PATH_RECORD) { Add-Content -LiteralPath $env:SNOWFAST_TEST_PATH_RECORD -Value $Dir }
        return "already configured"
    }

    $newPath = if ([string]::IsNullOrWhiteSpace($path)) {
        $Dir
    } else {
        "$path$([IO.Path]::PathSeparator)$Dir"
    }
    [Environment]::SetEnvironmentVariable("Path", $newPath, $scope)
    if ($env:SNOWFAST_TEST_PATH_RECORD) { Add-Content -LiteralPath $env:SNOWFAST_TEST_PATH_RECORD -Value $Dir }
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
$manifest = Get-UpdateManifest

if ($Version) {
    $resolvedVersion = Normalize-Version $Version
} else {
    $resolvedVersion = Normalize-Version $manifest.version
}

$releaseTag = "v$resolvedVersion"
if ($ReleaseBase) {
    # Test hooks: override the releases base; the tag segment stays composed.
    $releaseBase = $ReleaseBase.TrimEnd("/") + "/" + $releaseTag
} else {
    $releaseBase = "https://github.com/$RepoOwner/$RepoName/releases/download/$releaseTag"
}

# W11: config example must match the release being installed, not main.
# SNOWFAST_REF wins for users who explicitly want main or a branch;
# SNOWFAST_RAW_BASE overrides the raw base with the ref still composed
# (test hooks, mirrors SNOWFAST_RELEASE_BASE semantics).
if ($Ref) {
    $rawRef = $Ref
} else {
    $rawRef = $releaseTag
}
if ($RawBase) {
    $rawBase = $RawBase.TrimEnd("/") + "/" + $rawRef
} else {
    $rawBase = "https://raw.githubusercontent.com/$RepoOwner/$RepoName/$rawRef"
}

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

# W2: checksums come from the release's SHA256SUMS artifact, fetched
# after the dry-run block so --dry-run performs no extra downloads.

if ($DryRun) {
    Write-Section "Dry run"
    Write-Host "Checksums  : $releaseBase/SHA256SUMS"
    Write-Host "Would download:"
    foreach ($item in $assets) {
        $assetInfo = Get-ManifestAsset -Manifest $manifest -AssetName $item.Asset
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

    $sumsPath = Join-Path $tmpDir "SHA256SUMS"
    try {
        Download-File -Uri "$releaseBase/SHA256SUMS" -OutFile $sumsPath
    } catch {
        throw "could not download SHA256SUMS for tag $releaseTag (does release $releaseTag exist and ship the SHA256SUMS artifact?)"
    }

    foreach ($item in $assets) {
        $assetInfo = Get-ManifestAsset -Manifest $manifest -AssetName $item.Asset
        $assetPath = Join-Path $tmpDir $item.Asset
        $assetUrl = Resolve-AssetUrl -Asset $assetInfo -Version $resolvedVersion -AssetName $item.Asset
        Download-File -Uri $assetUrl -OutFile $assetPath
        $expected = Get-SumsChecksum -SumsPath $sumsPath -AssetName $item.Asset
        Assert-Checksum -ExpectedHash $expected -AssetName $item.Asset -Path $assetPath
        Write-Ok "verified $($item.Asset)"
    }

    # RR-6.7: the config example is optional polish, not the payload. Fetch
    # the pinned template BEFORE any binary is committed so an outage of the
    # raw host can never report the binary install as failed or skip PATH
    # setup. An existing config is preserved and never triggers a download.
    $configTemplatePath = $null
    $configStatus = "preserved existing"
    if (Test-Path -LiteralPath $configPath) {
        Write-Skip "config already exists: $configPath"
    } else {
        $configStatus = "not created (download failed)"
        $examplePath = Join-Path $tmpDir "config.toml.example"
        try {
            Download-File -Uri "$rawBase/config.toml.example" -OutFile $examplePath
            $configTemplatePath = $examplePath
        } catch {
            Write-Warn "could not download config template from $rawBase/config.toml.example; continuing without creating a config: $_"
        }
    }

    Write-Section "Installing commands"

    New-Item -ItemType Directory -Path $resolvedInstallDir -Force | Out-Null

    # Transactional install: snapshot every existing destination before
    # touching anything, then replace the three commands in deterministic
    # sfu,sfs,sfl order. On any replacement failure, restore the pre-run
    # set exactly: previously-existing destinations from their backups,
    # newly-created ones removed. Rollback errors are reported, never
    # swallowed — if restoration cannot be verified the transaction
    # directory (with the backups) is kept and the install fails loudly.
    $txEntries = @()
    # H-16: recover (finish or roll back) any transaction an interrupted
    # earlier run left behind, BEFORE this run opens its own transaction and
    # snapshots the install dir as its baseline. Must precede the txDir
    # creation below: recovery treats any journal-less txn dir as stale and
    # would otherwise delete this run's own freshly created dir.
    Recover-InstallTransactions -InstallDir $resolvedInstallDir
    $txDir = Join-Path $resolvedInstallDir ".snowfast-install-txn-$PID"
    New-Item -ItemType Directory -Path $txDir -Force | Out-Null
    $journalPath = Join-Path $txDir "journal.json"
    try {
        foreach ($item in $assets) {
            $dest = Join-Path $resolvedInstallDir $item.Command
            $entry = @{ Command = $item.Command; Dest = $dest; Existed = $false; Backup = $null }
            if (Test-Path -LiteralPath $dest) {
                $entry.Existed = $true
                $backup = Join-Path $txDir "$($item.Command).previous"
                Copy-Item -LiteralPath $dest -Destination $backup -Force
                $entry.Backup = $backup
            }
            $txEntries += $entry
        }
        # H-16: journal on disk and flushed BEFORE the first replacement, so
        # any crash after this point leaves a recoverable transaction.
        $journal = @{
            InstallDir = $resolvedInstallDir
            Version = $resolvedVersion
            Phase = "replacing"
            Entries = $txEntries
        }
        Write-TxJournal -Path $journalPath -Journal $journal
    } catch {
        # Nothing has been replaced yet; abandon the transaction cleanly.
        Remove-Item -LiteralPath $txDir -Recurse -Force -ErrorAction SilentlyContinue
        throw
    }

    try {
        foreach ($item in $assets) {
            $assetPath = Join-Path $tmpDir $item.Asset
            $dest = Join-Path $resolvedInstallDir $item.Command
            Install-Binary -Source $assetPath -Destination $dest
            Write-Ok "installed $($item.Command) -> $dest"
            # H-16: record the committed replacement so a later recovery
            # knows this command was installed after the journal was
            # written (an unmarked entry may hold a partial binary).
            ($txEntries | Where-Object { $_.Command -eq $item.Command }).Status = "installed"
            $journal.Entries = $txEntries
            Write-TxJournal -Path $journalPath -Journal $journal
        }
        # H-16: every replacement committed; a later run may finish the
        # transaction by simply removing the dir if the process dies here.
        $journal.Phase = "committed"
        Write-TxJournal -Path $journalPath -Journal $journal
    } catch {
        Write-Warn "install failed, rolling back to the previous binaries: $_"
        $rollbackErrors = @()
        foreach ($entry in $txEntries) {
            if ($entry.Existed -and $entry.Backup) {
                try {
                    Copy-Item -LiteralPath $entry.Backup -Destination $entry.Dest -Force
                } catch {
                    $rollbackErrors += "could not restore $($entry.Command) from backup: $_"
                }
            } elseif (-not $entry.Existed -and (Test-Path -LiteralPath $entry.Dest)) {
                try {
                    Remove-Item -LiteralPath $entry.Dest -Force
                } catch {
                    $rollbackErrors += "could not remove newly installed $($entry.Command): $_"
                }
            }
        }
        if ($rollbackErrors.Count -gt 0) {
            throw "rollback incomplete, backups kept in $txDir`: $($rollbackErrors -join '; ')"
        }
        Remove-Item -LiteralPath $txDir -Recurse -Force
        throw
    }

    Remove-Item -LiteralPath $txDir -Recurse -Force

    Write-Section "Writing config"

    # The template (if any) was fetched before the binary transaction. This
    # copy is deliberately POST-transaction: the binaries are already
    # verified and committed, so a local copy failure must not roll back
    # good binaries — it warns and continues, and PATH setup still runs.
    if (Test-Path -LiteralPath $configPath) {
        Write-Skip "config already exists: $configPath"
    } elseif ($configTemplatePath) {
        try {
            $configDir = Split-Path -Parent $configPath
            New-Item -ItemType Directory -Path $configDir -Force | Out-Null
            Copy-Item -Path $configTemplatePath -Destination $configPath -Force
            $configStatus = "created"
            Write-Ok "created config: $configPath"
        } catch {
            Write-Warn "could not install the downloaded config template to $configPath; continuing: $_"
            $configStatus = "not created (copy failed)"
        }
    } else {
        Write-Warn "config not created (download failed earlier): $configPath"
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
