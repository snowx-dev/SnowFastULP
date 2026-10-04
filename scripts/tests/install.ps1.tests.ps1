# Test suite for scripts/install.ps1 (W8).
#
# Runs on windows-latest CI and anywhere pwsh exists (Linux/macOS included:
# the Windows-only surfaces it needs — [Environment] user PATH on Linux,
# Get-FileHash, Invoke-WebRequest — work under pwsh 7). Serves the shared
# fixtures via scripts/tests/server.py (python3 required, tests only) and
# points the installer at it via the SNOWFAST_* overrides.
#
# Usage:
#   pwsh -NoProfile -File scripts/tests/install.ps1.tests.ps1 [-InstallerPath <path>]
#
# Exit code 0 = all tests passed.
[CmdletBinding()]
param(
    [string]$InstallerPath = (Join-Path $PSScriptRoot ".." "install.ps1"),
    [int]$Port = 0
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$TestRoot = $PSScriptRoot
$FixtureDir = Join-Path $TestRoot "fixtures"
$ServerPy = Join-Path $TestRoot "server.py"

$script:Passed = 0
$script:Failed = 0
$script:Failures = @()

function Write-Pass([string]$Name) {
    $script:Passed++
    Write-Host "ok   - $Name"
}
function Write-Fail([string]$Name, [string]$Detail = "") {
    $script:Failed++
    $script:Failures += $Name
    Write-Host "FAIL - $Name"
    if ($Detail) { Write-Host $Detail }
}

# --- sandbox ---------------------------------------------------------------

$Sandbox = Join-Path ([IO.Path]::GetTempPath()) ("snowfast-installer-tests-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $Sandbox -Force | Out-Null
$PathRecord = Join-Path $Sandbox "path-calls.log"
$env:SNOWFAST_TEST_PATH_SCOPE = "Process"
$env:SNOWFAST_TEST_PATH_RECORD = $PathRecord

function New-CaseSandbox([string]$Case) {
    $dir = Join-Path $Sandbox $Case
    $homeDir = Join-Path $dir "home"
    New-Item -ItemType Directory -Path $homeDir -Force | Out-Null
    return @{
        Dir      = $dir
        Home     = $homeDir
        AppData  = Join-Path $homeDir "AppData\Roaming"
        LocalAppData = Join-Path $homeDir "AppData\Local"
        Bin      = Join-Path $homeDir "AppData\Local\SnowFast\bin"
        Config   = Join-Path $homeDir "AppData\Roaming\snowfast\config.toml"
    }
}

# --- server ----------------------------------------------------------------

function Find-FreePort {
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $listener.Start()
    $port = ($listener.LocalEndpoint -as [System.Net.IPEndPoint]).Port
    $listener.Stop()
    return $port
}

if ($Port -eq 0) {
    $Port = Find-FreePort
}
if ($IsWindows -or $PSVersionTable.PSVersion.Major -lt 6) {
    $python = "python"
} else {
    $python = "python3"
}
$serverProc = Start-Process -FilePath $python `
    -ArgumentList @($ServerPy, "$Port", $FixtureDir) `
    -PassThru `
    -RedirectStandardOutput (Join-Path $Sandbox "server.log") `
    -RedirectStandardError (Join-Path $Sandbox "server.err.log")

# Wait for readiness.
$ready = $false
$baseUrl = "http://127.0.0.1:$Port"
for ($i = 0; $i -lt 40; $i++) {
    try {
        Invoke-WebRequest -Uri "$baseUrl/manifest" -UseBasicParsing -TimeoutSec 2 | Out-Null
        $ready = $true
        break
    } catch {
        Start-Sleep -Milliseconds 250
    }
}
if (-not $ready) {
    Write-Host "test server failed to start; aborting"
    if (-not $serverProc.HasExited) { $serverProc.Kill() }
    exit 1
}
Write-Host "test server: $baseUrl (fixtures: $FixtureDir)"

$releaseBase = "$baseUrl/releases/download"
$rawBase = "$baseUrl/raw"

function Invoke-InstallerCase {
    param(
        [string]$Case,
        [string[]]$ExtraEnv = @(),
        [string[]]$ExtraArgs = @(),
        [scriptblock]$Prepare
    )
    $sb = New-CaseSandbox $Case
    if ($Prepare) { & $Prepare $sb }
    $env:SNOWFAST_UPDATE_URL = "$baseUrl/manifest"
    $env:SNOWFAST_RELEASE_BASE = $releaseBase
    $env:SNOWFAST_RAW_BASE = $rawBase
    $env:SNOWFAST_INSTALL_DIR = $sb.Bin
    $env:APPDATA = $sb.AppData
    $env:LOCALAPPDATA = $sb.LocalAppData
    $env:SNOWFAST_VERSION = ""
    $env:SNOWFAST_REF = ""
    $env:SNOWFAST_TEST_FAIL_REPLACE_AT = ""
    foreach ($kv in $ExtraEnv) {
        $k, $v = $kv -split "=", 2
        Set-Item -Path ("env:" + $k) -Value $v
    }
    $log = Join-Path $sb.Dir "out.log"
    $out = & pwsh -NoProfile -File $InstallerPath @ExtraArgs 2>&1 | Tee-Object -FilePath $log
    return @{
        Sandbox   = $sb
        ExitCode  = $LASTEXITCODE
        Output    = $out
        LogPath   = $log
    }
}

function Assert-BinariesInstalled {
    param($Result, [string]$Version, [string]$Case)
    $ok = $true
    foreach ($cmd in @("sfu.exe", "sfs.exe", "sfl.exe")) {
        $path = Join-Path $Result.Sandbox.Bin $cmd
        if (-not (Test-Path -LiteralPath $path)) {
            Write-Fail "${Case}: $cmd not installed" (Get-Content $Result.LogPath -Raw)
            return $false
        }
    }
    return $true
}

function Assert-SumsHashMatches {
    param($Result, [string]$Version, [string]$Case)
    $asset = "SnowFastULP-$Version-windows-amd64.exe"
    $sums = Get-Content -LiteralPath (Join-Path $FixtureDir "SHA256SUMS-$Version")
    $expected = ($sums | Where-Object { $_ -match [regex]::Escape($asset) }) -split "\s+" | Select-Object -First 1
    $actual = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $Result.Sandbox.Bin "sfu.exe")).Hash.ToLowerInvariant()
    if ($actual -ne $expected.ToLowerInvariant()) {
        Write-Fail "${Case}: sfu.exe hash does not match SHA256SUMS-$Version"
        return $false
    }
    return $true
}


# --- W7 rollback helpers ---------------------------------------------------

# Sentinel "old binary" content, shared by the Prepare hook and assertions.
$script:OldBytes = @{
    "sfu.exe" = "old-sfu-payload-for-rollback-tests`n"
    "sfs.exe" = "old-sfs-payload-for-rollback-tests`n"
    "sfl.exe" = "old-sfl-payload-for-rollback-tests`n"
}

function Write-OldBinaries($Sb) {
    New-Item -ItemType Directory -Path $Sb.Bin -Force | Out-Null
    foreach ($cmd in @("sfu.exe", "sfs.exe", "sfl.exe")) {
        [IO.File]::WriteAllText((Join-Path $Sb.Bin $cmd), $script:OldBytes[$cmd], [Text.UTF8Encoding]::new($false))
    }
}

# Every file left in the install dir that is not one of the expected command
# names: transaction dirs, .tmp/.new droppings, anything else.
function Get-BinLeftovers($Result, [string[]]$Expected) {
    if (-not (Test-Path -LiteralPath $Result.Sandbox.Bin)) { return @() }
    $items = Get-ChildItem -LiteralPath $Result.Sandbox.Bin -Force |
        Where-Object { $Expected -notcontains $_.Name }
    return @($items)
}

function Assert-ExactBinContent {
    param($Result, [string]$Case, [hashtable]$Wanted)
    $bad = @()
    foreach ($cmd in $Wanted.Keys) {
        $path = Join-Path $Result.Sandbox.Bin $cmd
        $actual = if (Test-Path -LiteralPath $path) { Get-Content -LiteralPath $path -Raw } else { $null }
        if ($actual -ne $Wanted[$cmd]) { $bad += "$cmd" }
    }
    return $bad
}

function Assert-HashesMatchFixture {
    param($Result, [string]$Version, [string]$Case)
    $pairs = @(
        @{ Asset = "SnowFastULP-$Version-windows-amd64.exe"; Command = "sfu.exe" },
        @{ Asset = "SnowFastSearch-$Version-windows-amd64.exe"; Command = "sfs.exe" },
        @{ Asset = "SnowFastLog-$Version-windows-amd64.exe"; Command = "sfl.exe" }
    )
    foreach ($p in $pairs) {
        $fixture = Join-Path (Join-Path $FixtureDir "assets") $p.Asset
        $expected = (Get-FileHash -Algorithm SHA256 -Path $fixture).Hash.ToLowerInvariant()
        $actual = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $Result.Sandbox.Bin $p.Command)).Hash.ToLowerInvariant()
        if ($actual -ne $expected) {
            Write-Fail "${Case}: $($p.Command) does not match fixture $($p.Asset)"
            return $false
        }
    }
    return $true
}

# --- tests -----------------------------------------------------------------

try {
    # 0. stalled manifest must fail within the configured timeout, before any
    # install directory or temporary download directory is created.
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $r = Invoke-InstallerCase -Case "requesttimeout" -ExtraEnv @(
        "SNOWFAST_UPDATE_URL=$baseUrl/stall",
        "SNOWFAST_HTTP_TIMEOUT_SEC=1"
    )
    $sw.Stop()
    $binCreated = Test-Path -LiteralPath $r.Sandbox.Bin
    $timedOut = ($r.Output -join "`n") -match "(?i)timeout|timed out|time-out"
    if ($r.ExitCode -ne 0 -and $sw.Elapsed.TotalSeconds -lt 8 -and -not $binCreated -and $timedOut) {
        Write-Pass "HTTP requests honor bounded timeout"
    } else {
        Write-Fail "HTTP requests honor bounded timeout" "exit=$($r.ExitCode), elapsed=$([math]::Round($sw.Elapsed.TotalSeconds, 2))s, binCreated=$binCreated"
    }

    # 1. latest install (pretty manifest)
    $r = Invoke-InstallerCase -Case "latest"
    if ($r.ExitCode -eq 0 -and (Assert-BinariesInstalled $r "0.2.0" "latest") -and
        (Assert-SumsHashMatches $r "0.2.0" "latest")) {
        Write-Pass "latest install"
    }

    # 2. pinned install via SNOWFAST_VERSION (W2 regression: pinned tag has
    #    its own SHA256SUMS; the LATEST manifest has no entry for it).
    $r = Invoke-InstallerCase -Case "pinned" -ExtraEnv @("SNOWFAST_VERSION=0.3.1")
    if ($r.ExitCode -eq 0 -and (Assert-BinariesInstalled $r "0.3.1" "pinned") -and
        (Assert-SumsHashMatches $r "0.3.1" "pinned")) {
        Write-Pass "pinned install via SNOWFAST_VERSION (W2)"
    } elseif ($r.ExitCode -ne 0) {
        Write-Fail "pinned install via SNOWFAST_VERSION (W2)" (Get-Content $r.LogPath -Raw)
    }

    # 3. dry run must not install anything
    $r = Invoke-InstallerCase -Case "dryrun" -ExtraArgs @("-DryRun")
    $binExists = Test-Path -LiteralPath (Join-Path $r.Sandbox.Bin "sfu.exe")
    if ($r.ExitCode -eq 0 -and -not $binExists -and
        ($r.Output -join "`n") -match "dry run complete") {
        Write-Pass "dry run makes no changes"
    } elseif ($r.ExitCode -ne 0) {
        Write-Fail "dry run makes no changes" (Get-Content $r.LogPath -Raw)
    } else {
        Write-Fail "dry run makes no changes" "sfu.exe exists after -DryRun"
    }

    # 4. checksum mismatch must fail loudly (W9 companion: no droppings)
    $sb = New-CaseSandbox "badsum"
    $mutated = Join-Path $sb.Dir "fixtures"
    Copy-Item -Path $FixtureDir -Destination $mutated -Recurse
    $victim = Join-Path $mutated "assets\SnowFastULP-0.3.1-windows-amd64.exe"
    Add-Content -LiteralPath $victim -Value "# corrupted"
    $badPort = Find-FreePort
    $badProc = Start-Process -FilePath $python `
        -ArgumentList @($ServerPy, "$badPort", $mutated) `
        -PassThru `
        -RedirectStandardOutput (Join-Path $sb.Dir "bad-server.log") `
        -RedirectStandardError (Join-Path $sb.Dir "bad-server.err.log")
    try {
        $env:SNOWFAST_UPDATE_URL = "http://127.0.0.1:$badPort/manifest"
        $env:SNOWFAST_RELEASE_BASE = "http://127.0.0.1:$badPort/releases/download"
        $env:SNOWFAST_RAW_BASE = "http://127.0.0.1:$badPort/raw"
        $env:SNOWFAST_INSTALL_DIR = $sb.Bin
        $env:APPDATA = $sb.AppData
        $env:LOCALAPPDATA = $sb.LocalAppData
        $env:SNOWFAST_VERSION = "0.3.1"
        $env:SNOWFAST_REF = ""
        $log = Join-Path $sb.Dir "out.log"
        & pwsh -NoProfile -File $InstallerPath 2>&1 | Out-File -FilePath $log
        $rc = $LASTEXITCODE
        $strays = Get-ChildItem -LiteralPath $sb.Bin -Filter "*.tmp.*" -ErrorAction SilentlyContinue
        if ($rc -ne 0 -and -not $strays) {
            Write-Pass "checksum mismatch fails cleanly"
        } elseif ($rc -eq 0) {
            Write-Fail "checksum mismatch fails cleanly" "installer exited 0 against corrupted asset"
        } else {
            Write-Fail "checksum mismatch fails cleanly" "temp droppings left in install dir"
        }
    } finally {
        if (-not $badProc.HasExited) { $badProc.Kill() }
    }

    # 4b. W7: all three commands exist; replacement of the second (sfs)
    #     is forced to fail. All destinations must keep their old bytes,
    #     and no transaction/temp leftovers may remain.
    $r = Invoke-InstallerCase -Case "rollbackallexisting" `
        -ExtraEnv @("SNOWFAST_TEST_FAIL_REPLACE_AT=sfs.exe") `
        -Prepare ${function:Write-OldBinaries}
    $log = Get-Content $r.LogPath -Raw
    if ($r.ExitCode -eq 0) {
        Write-Fail "rollback restores all existing binaries on failure (W7)" "installer exited 0 despite forced replacement failure"
    } else {
        $bad = @(Assert-ExactBinContent -Result $r -Case "rollbackallexisting" -Wanted $script:OldBytes)
        $leftovers = @(Get-BinLeftovers $r @("sfu.exe", "sfs.exe", "sfl.exe"))
        if ($bad.Count -eq 0 -and $leftovers.Count -eq 0 -and ($r.Output -join "`n") -match "rolling back") {
            Write-Pass "rollback restores all existing binaries on failure (W7)"
        } else {
            $detail = @()
            if ($bad.Count -gt 0) { $detail += "destinations changed: $($bad -join ', ')" }
            if ($leftovers.Count -gt 0) { $detail += "leftovers: $($leftovers.Name -join ', ')" }
            if (-not (($r.Output -join "`n") -match "rolling back")) { $detail += "no rollback message in output" }
            Write-Fail "rollback restores all existing binaries on failure (W7)" ($detail -join "; ")
        }
    }

    # 4c. W7: same install without the fault injection — success path
    #     overwrites all three and leaves no transaction leftovers.
    $r = Invoke-InstallerCase -Case "overwriteallexisting" `
        -Prepare ${function:Write-OldBinaries}
    if ($r.ExitCode -eq 0) {
        $leftovers = @(Get-BinLeftovers $r @("sfu.exe", "sfs.exe", "sfl.exe"))
        if ($leftovers.Count -eq 0 -and (Assert-HashesMatchFixture $r "0.2.0" "overwriteallexisting")) {
            Write-Pass "success path installs new bytes over existing binaries (W7)"
        } else {
            Write-Fail "success path installs new bytes over existing binaries (W7)" "leftovers: $($leftovers.Name -join ', ')"
        }
    } else {
        Write-Fail "success path installs new bytes over existing binaries (W7)" (Get-Content $r.LogPath -Raw)
    }

    # 4d. W7: one pre-existing command (sfu) and two new ones (sfs, sfl);
    #     failure at sfs. sfu must be restored, no new command may remain.
    $r = Invoke-InstallerCase -Case "rollbackonenew" `
        -ExtraEnv @("SNOWFAST_TEST_FAIL_REPLACE_AT=sfs.exe") `
        -Prepare {
            param($Sb)
            New-Item -ItemType Directory -Path $Sb.Bin -Force | Out-Null
            [IO.File]::WriteAllText((Join-Path $Sb.Bin "sfu.exe"), $script:OldBytes["sfu.exe"], [Text.UTF8Encoding]::new($false))
        }
    if ($r.ExitCode -eq 0) {
        Write-Fail "rollback leaves only pre-existing binaries (W7)" "installer exited 0 despite forced replacement failure"
    } else {
        $wanted = @{ "sfu.exe" = $script:OldBytes["sfu.exe"]; "sfs.exe" = $null; "sfl.exe" = $null }
        $bad = @(Assert-ExactBinContent -Result $r -Case "rollbackonenew" -Wanted $wanted)
        $leftovers = @(Get-BinLeftovers $r @("sfu.exe"))
        if ($bad.Count -eq 0 -and $leftovers.Count -eq 0) {
            Write-Pass "rollback leaves only pre-existing binaries (W7)"
        } else {
            $detail = @()
            if ($bad.Count -gt 0) { $detail += "unexpected destination state: $($bad -join ', ')" }
            if ($leftovers.Count -gt 0) { $detail += "leftovers: $($leftovers.Name -join ', ')" }
            Write-Fail "rollback leaves only pre-existing binaries (W7)" ($detail -join "; ")
        }
    }

    # 4d1. H-16: a previous installer run was terminated mid-transaction
    #     (power loss / kill after the first replacement). The next run must
    #     detect the stale transaction dir, roll the install dir back to the
    #     pre-run state (restore backed-up commands, remove newly-created
    #     ones), and then proceed with a clean install. A journal-less
    #     transaction dir (crash before the first replacement) must simply
    #     be removed, and a committed one finished. No stale dirs may remain.
    $r = Invoke-InstallerCase -Case "recoverinterrupted" -Prepare {
        param($Sb)
        Write-OldBinaries $Sb
        # Simulate the crash state: sfu was replaced with new junk bytes,
        # sfl was newly created, sfs untouched; backups sit in the stale
        # transaction dir alongside a phase-"replacing" journal.
        $txDir = Join-Path $Sb.Bin ".snowfast-install-txn-99999"
        New-Item -ItemType Directory -Path $txDir -Force | Out-Null
        [IO.File]::WriteAllText((Join-Path $txDir "sfu.exe.previous"), $script:OldBytes["sfu.exe"], [Text.UTF8Encoding]::new($false))
        [IO.File]::WriteAllText((Join-Path $txDir "sfs.exe.previous"), $script:OldBytes["sfs.exe"], [Text.UTF8Encoding]::new($false))
        [IO.File]::WriteAllText((Join-Path $Sb.Bin "sfu.exe"), "interrupted-new-sfu-payload`n", [Text.UTF8Encoding]::new($false))
        [IO.File]::WriteAllText((Join-Path $Sb.Bin "sfl.exe"), "interrupted-new-sfl-payload`n", [Text.UTF8Encoding]::new($false))
        $journal = [ordered]@{
            InstallDir = $Sb.Bin
            Version = "0.2.0"
            Phase = "replacing"
            Entries = @(
                [ordered]@{ Command = "sfu.exe"; Dest = (Join-Path $Sb.Bin "sfu.exe"); Existed = $true; Backup = (Join-Path $txDir "sfu.exe.previous") },
                [ordered]@{ Command = "sfs.exe"; Dest = (Join-Path $Sb.Bin "sfs.exe"); Existed = $true; Backup = (Join-Path $txDir "sfs.exe.previous") },
                [ordered]@{ Command = "sfl.exe"; Dest = (Join-Path $Sb.Bin "sfl.exe"); Existed = $false; Backup = $null }
            )
        }
        [IO.File]::WriteAllText((Join-Path $txDir "journal.json"), ($journal | ConvertTo-Json -Depth 5), [Text.UTF8Encoding]::new($false))
        # A second stale dir from a crash BEFORE the first replacement (no
        # journal): nothing was replaced, so nothing may be rolled back.
        New-Item -ItemType Directory -Path (Join-Path $Sb.Bin ".snowfast-install-txn-88888") -Force | Out-Null
    }
    if ($r.ExitCode -eq 0 -and (($r.Output -join "`n") -match "recovering interrupted install")) {
        $leftovers = @(Get-BinLeftovers $r @("sfu.exe", "sfs.exe", "sfl.exe"))
        $hashOk = Assert-HashesMatchFixture $r "0.2.0" "recoverinterrupted"
        if ($leftovers.Count -eq 0 -and $hashOk) {
            Write-Pass "next run recovers an interrupted install transaction (H-16)"
        } else {
            $detail = @()
            if ($leftovers.Count -gt 0) { $detail += "leftovers: $($leftovers.Name -join ', ')" }
            if (-not $hashOk) { $detail += "binaries do not match fixture after recovery" }
            Write-Fail "next run recovers an interrupted install transaction (H-16)" ($detail -join "; ")
        }
    } else {
        $detail = @()
        if ($r.ExitCode -ne 0) { $detail += "exit $($r.ExitCode)" }
        if (-not (($r.Output -join "`n") -match "recovering interrupted install")) { $detail += "no recovery message in output" }
        Write-Fail "next run recovers an interrupted install transaction (H-16)" ($detail -join "; " + (Get-Content $r.LogPath -Raw))
    }

    # 4e. P6-W7: raw host down on a fresh install. The binary install and
    #     PATH setup must still succeed; config stays absent with a warning.
    $sb = New-CaseSandbox "rawoutage"
    $mutated = Join-Path $sb.Dir "fixtures"
    Copy-Item -Path $FixtureDir -Destination $mutated -Recurse
    New-Item -ItemType File -Path (Join-Path $mutated "FAIL-RAW") | Out-Null
    $outagePort = Find-FreePort
    $outageProc = Start-Process -FilePath $python `
        -ArgumentList @($ServerPy, "$outagePort", $mutated) `
        -PassThru `
        -RedirectStandardOutput (Join-Path $sb.Dir "outage-server.log") `
        -RedirectStandardError (Join-Path $sb.Dir "outage-server.err.log")
    $pathCallsBefore = if (Test-Path -LiteralPath $PathRecord) { @(Get-Content -LiteralPath $PathRecord).Count } else { 0 }
    try {
        $env:SNOWFAST_UPDATE_URL = "http://127.0.0.1:$outagePort/manifest"
        $env:SNOWFAST_RELEASE_BASE = "http://127.0.0.1:$outagePort/releases/download"
        $env:SNOWFAST_RAW_BASE = "http://127.0.0.1:$outagePort/raw"
        $env:SNOWFAST_INSTALL_DIR = $sb.Bin
        $env:APPDATA = $sb.AppData
        $env:LOCALAPPDATA = $sb.LocalAppData
        $env:SNOWFAST_VERSION = ""
        $env:SNOWFAST_REF = ""
        $env:SNOWFAST_TEST_FAIL_REPLACE_AT = ""
        $log = Join-Path $sb.Dir "out.log"
        $out = & pwsh -NoProfile -File $InstallerPath 2>&1 | Tee-Object -FilePath $log
        $rc = $LASTEXITCODE
        $text = $out -join "`n"
        $binOk = (Test-Path -LiteralPath (Join-Path $sb.Bin "sfu.exe")) -and
            (Test-Path -LiteralPath (Join-Path $sb.Bin "sfs.exe")) -and
            (Test-Path -LiteralPath (Join-Path $sb.Bin "sfl.exe"))
        $configAbsent = -not (Test-Path -LiteralPath (Join-Path $sb.AppData "snowfast\config.toml"))
        $pathRecorded = (Test-Path -LiteralPath $PathRecord) -and (@(Get-Content -LiteralPath $PathRecord).Count -gt $pathCallsBefore)
        if ($rc -eq 0 -and $binOk -and $configAbsent -and $pathRecorded -and $text -match "could not download config template") {
            Write-Pass "raw outage does not fail install or skip PATH (P6-W7)"
        } else {
            $detail = @()
            if ($rc -ne 0) { $detail += "exit $rc" }
            if (-not $binOk) { $detail += "binaries missing" }
            if (-not $configAbsent) { $detail += "config exists" }
            if (-not $pathRecorded) { $detail += "PATH not recorded" }
            if (-not ($text -match "could not download config template")) { $detail += "no warning in output" }
            Write-Fail "raw outage does not fail install or skip PATH (P6-W7)" ($detail -join "; ")
        }
    } finally {
        if (-not $outageProc.HasExited) { $outageProc.Kill() }
    }

    # 4f. P6-W7: existing config is preserved and no raw fetch is attempted
    #     even when the raw host is down.
    $sb = New-CaseSandbox "rawoutage-existing"
    $mutated = Join-Path $sb.Dir "fixtures"
    Copy-Item -Path $FixtureDir -Destination $mutated -Recurse
    New-Item -ItemType File -Path (Join-Path $mutated "FAIL-RAW") | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $sb.AppData "snowfast") -Force | Out-Null
    $sentinel = "# my hand-tuned config`n"
    [IO.File]::WriteAllText((Join-Path $sb.AppData "snowfast\config.toml"), $sentinel, [Text.UTF8Encoding]::new($false))
    $existingPort = Find-FreePort
    $existingProc = Start-Process -FilePath $python `
        -ArgumentList @($ServerPy, "$existingPort", $mutated) `
        -PassThru `
        -RedirectStandardOutput (Join-Path $sb.Dir "existing-server.log") `
        -RedirectStandardError (Join-Path $sb.Dir "existing-server.err.log")
    try {
        $env:SNOWFAST_UPDATE_URL = "http://127.0.0.1:$existingPort/manifest"
        $env:SNOWFAST_RELEASE_BASE = "http://127.0.0.1:$existingPort/releases/download"
        $env:SNOWFAST_RAW_BASE = "http://127.0.0.1:$existingPort/raw"
        $env:SNOWFAST_INSTALL_DIR = $sb.Bin
        $env:APPDATA = $sb.AppData
        $env:LOCALAPPDATA = $sb.LocalAppData
        $env:SNOWFAST_VERSION = ""
        $env:SNOWFAST_REF = ""
        $env:SNOWFAST_TEST_FAIL_REPLACE_AT = ""
        $log = Join-Path $sb.Dir "out.log"
        $out = & pwsh -NoProfile -File $InstallerPath 2>&1 | Out-File -FilePath $log
        $rc = $LASTEXITCODE
        $text = (Get-Content $log -Raw)
        $configUnchanged = (Get-Content -LiteralPath (Join-Path $sb.AppData "snowfast\config.toml") -Raw) -eq $sentinel
        if ($rc -eq 0 -and $configUnchanged -and $text -notmatch "could not download config template" -and $text -match [regex]::Escape("config already exists")) {
            Write-Pass "existing config preserved; no download attempted (P6-W7)"
        } else {
            $detail = @()
            if ($rc -ne 0) { $detail += "exit $rc" }
            if (-not $configUnchanged) { $detail += "config modified" }
            if ($text -match "could not download config template") { $detail += "download attempted" }
            Write-Fail "existing config preserved; no download attempted (P6-W7)" ($detail -join "; ")
        }
    } finally {
        if (-not $existingProc.HasExited) { $existingProc.Kill() }
    }

    # 5. config example fetched from the pinned tag ref (W11), ref override respected
    $r = Invoke-InstallerCase -Case "refdefault" -ExtraEnv @("SNOWFAST_VERSION=0.3.1")
    if ($r.ExitCode -eq 0) {
        $config = Get-Content -LiteralPath (Join-Path $r.Sandbox.AppData "snowfast\config.toml") -Raw
        if ($config -match "served for ref: v0\.3\.1") {
            Write-Pass "config example from pinned tag ref (W11)"
        } else {
            Write-Fail "config example from pinned tag ref (W11)" "config body: $config"
        }
    } else {
        Write-Fail "config example from pinned tag ref (W11)" (Get-Content $r.LogPath -Raw)
    }

    $r = Invoke-InstallerCase -Case "refoverride" -ExtraEnv @("SNOWFAST_REF=some-branch")
    if ($r.ExitCode -eq 0) {
        $config = Get-Content -LiteralPath (Join-Path $r.Sandbox.AppData "snowfast\config.toml") -Raw
        if ($config -match "served for ref: some-branch") {
            Write-Pass "SNOWFAST_REF override respected"
        } else {
            Write-Fail "SNOWFAST_REF override respected" "config body: $config"
        }
    } else {
        Write-Fail "SNOWFAST_REF override respected" (Get-Content $r.LogPath -Raw)
    }
    if ((Test-Path -LiteralPath $PathRecord) -and ((Get-Content -LiteralPath $PathRecord | Where-Object { $_ }).Count -ge 1)) {
        Write-Pass "PATH updates recorded without registry mutation"
    } else {
        Write-Fail "PATH updates recorded without registry mutation" "no Add-UserPath calls recorded"
    }

} finally {
    $env:SNOWFAST_UPDATE_URL = ""
    $env:SNOWFAST_RELEASE_BASE = ""
    $env:SNOWFAST_RAW_BASE = ""
    $env:SNOWFAST_INSTALL_DIR = ""
    $env:SNOWFAST_VERSION = ""
    $env:SNOWFAST_HTTP_TIMEOUT_SEC = ""
    $env:SNOWFAST_REF = ""
    $env:SNOWFAST_TEST_FAIL_REPLACE_AT = ""
    $env:SNOWFAST_TEST_PATH_SCOPE = ""
    $env:SNOWFAST_TEST_PATH_RECORD = ""
    if (-not $serverProc.HasExited) { $serverProc.Kill() }
    Remove-Item -LiteralPath $Sandbox -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host ""
Write-Host "passed: $script:Passed  failed: $script:Failed"
if ($script:Failed -gt 0) {
    Write-Host "failed tests: $($script:Failures -join ', ')"
    exit 1
}
exit 0
