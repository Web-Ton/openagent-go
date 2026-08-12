# install.ps1 — one-step install for openagent-cli (Windows).
#
# Usage (run only if you trust this script source):
#   irm https://raw.githubusercontent.com/<repo>/master/scripts/install.ps1 | iex
#
# Downloads from Huawei Cloud OBS first (reachable behind GFW), falls back to
# GitHub on failure. OPENAGENT_MIRROR forces one source.
#
# Config (env vars):
#   OPENAGENT_CLI_NAME   binary name (default: openagent-cli)
#   OPENAGENT_VERSION    e.g. v1.2.3 (default: latest from OBS openagent/latest/)
#   REPO                 GitHub owner/name, used for fallback (default: yusheng-g/openagent-go)
#   OPENAGENT_MIRROR     force source: "obs" or "github" (default: try OBS then GitHub)
#   OBS_ENDPOINT         OBS bucket endpoint (default: https://twb.obs.cn-north-4.myhuaweicloud.com)
#   OBS_PREFIX           OBS object prefix (default: openagent)
#   SOUL_OVERWRITE       set to 1 to force-overwrite SOUL.md without prompting
#   HUWEICLOUDOPENAPI_OVERWRITE  set to 1 to force re-download the API snapshot
#
# NOTE on checksum scope: SHA256SUMS.txt ships in the same release as the
# binary, so this check defends against bit-flip / CDN corruption but NOT
# against a tampered release (attacker can replace both). For full supply-
# chain integrity, verify a maintainer GPG/cosign signature out-of-band.

$ErrorActionPreference = 'Stop'

# PS 5.1 on older Windows defaults to TLS 1.0; GitHub requires 1.2+. PS Core
# already defaults to 1.2 and lacks the type — guard with try/catch.
try {
    [Net.ServicePointManager]::SecurityProtocol = `
      [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch {}

$Name    = if ($env:OPENAGENT_CLI_NAME) { $env:OPENAGENT_CLI_NAME } else { 'openagent-cli' }
$Repo    = if ($env:REPO) { $env:REPO } else { 'yusheng-g/openagent-go' }
$InstallDir = Join-Path $env:LOCALAPPDATA "Programs\$Name"

# Resolve a tar executable that handles Windows drive-colon paths correctly.
# Windows 10 1803+ ships a libarchive bsdtar at System32\tar.exe which treats
# C:\... as a local path. A GNU tar from Git Bash (/usr/bin/tar) sitting earlier
# on PATH would parse the "C" in "C:\Users\...\Temp\...tar.gz" as a remote host
# and fail with "Cannot execute remote shell: No such file or directory".
# Prefer the system bsdtar explicitly; fall back to PATH tar.exe only if missing.
$TarExe = Join-Path $env:SystemRoot 'System32\tar.exe'
if (-not (Test-Path $TarExe)) { $TarExe = 'tar.exe' }

function Write-Info { param([string]$Msg) Write-Host "[install] $Msg" }
function Write-Warn { param([string]$Msg) Write-Host "[install] WARN: $Msg" -ForegroundColor Yellow }
function Write-Err  { param([string]$Msg) Write-Host "[install] $Msg" -ForegroundColor Red }

# ── skill install helper (defined early so the try block can call it) ──────────
function Print-SkillCommand {
    param([string[]]$Skills)
    $skillsStr = $Skills -join ' '
    Write-Info "  npx -y skills add -g https://gitcode.com/huaweicloud/huaweicloud-skills.git --skill $skillsStr -y"
}

function Install-HuaweiSkills {
    param([string]$SkillsFile)

    if (-not (Test-Path $SkillsFile)) {
        Write-Warn 'huawei-skills.txt missing; skipping skill install'
        return
    }

    $Skills = @(Get-Content $SkillsFile | Where-Object { $_ -ne '' })
    if ($Skills.Count -eq 0) { return }

    # Probe npx
    $npx = (Get-Command npx -ErrorAction SilentlyContinue)
    if (-not $npx) {
        Write-Warn 'npx not found; skipping Huawei Cloud skill install'
        Print-SkillCommand -Skills $Skills
        return
    }

    # Probe both endpoints with a short timeout.
    try {
        $null = Invoke-WebRequest -Uri 'https://gitcode.com' -Method Head -TimeoutSec 5 -UseBasicParsing
    } catch {
        Write-Warn 'gitcode.com unreachable; skipping Huawei Cloud skill install'
        Print-SkillCommand -Skills $Skills
        return
    }
    # Probe the npm registry npx will actually use (npm config get registry),
    # not a hardcoded one — users behind a mirror (npmmirror etc.) would have
    # registry.npmjs.org unreachable while npx still works fine via the mirror.
    $npmReg = (npm config get registry 2>$null).TrimEnd('/')
    if (-not $npmReg) { $npmReg = 'https://registry.npmjs.org' }
    try {
        $null = Invoke-WebRequest -Uri "$npmReg/skills" -Method Head -TimeoutSec 5 -UseBasicParsing
    } catch {
        Write-Warn "npm registry unreachable ($npmReg); skipping Huawei Cloud skill install"
        Print-SkillCommand -Skills $Skills
        return
    }

    Write-Info 'Installing Huawei Cloud skills...'
    # `npx -y` auto-confirms installing the skills package (no TTY in irm|iex);
    # -g forces global (user-level) scope so skills land in ~/.agents/skills/ regardless
    # of the caller's cwd — without -g, `skills add` auto-detects project scope when run
    # inside a git repo and installs under .\.agents\skills\ instead. The trailing -y
    # is passed to the skills tool itself (skip confirmation).
    $skillArgs = @('-y','skills','add','-g','https://gitcode.com/huaweicloud/huaweicloud-skills.git','--skill') + $Skills + @('-y')
    try {
        & npx @skillArgs
        if ($LASTEXITCODE -eq 0) {
            Write-Info 'Huawei Cloud skills installed'
        } else {
            Write-Warn "npx skills add failed (exit $LASTEXITCODE); you can retry manually:"
            Print-SkillCommand -Skills $Skills
        }
    } catch {
        Write-Warn "npx skills add threw: $_; you can retry manually:"
        Print-SkillCommand -Skills $Skills
    }
}

function Install-Soul {
    param([string]$SoulFile)

    if (-not (Test-Path $SoulFile)) {
        Write-Warn 'SOUL.md missing from archive; skipping profile install'
        return
    }

    $ProfileDir = Join-Path $env:USERPROFILE '.openagent\profile'
    $Dst = Join-Path $ProfileDir 'SOUL.md'

    # SOUL_OVERWRITE=1 → force overwrite, no prompt.
    $force = $env:SOUL_OVERWRITE -eq '1'

    if ((Test-Path $Dst) -and -not $force) {
        # Interactive? SupportsVirtualTerminal + stdout is a real terminal.
        $interactive = [Environment]::UserInteractive -and -not [Console]::IsOutputRedirected
        if ($interactive) {
            Write-Info "$Dst already exists."
            $ans = Read-Host "Overwrite? [y/N]"
            if ($ans -notmatch '^(y|yes)$') { return }   # skip on N/empty
        } else {
            Write-Warn "$Dst already exists; skipping (set SOUL_OVERWRITE=1 to force overwrite)"
            return
        }
    }

    $null = New-Item -ItemType Directory -Path $ProfileDir -Force
    try {
        Copy-Item -Path $SoulFile -Destination $Dst -Force
        Write-Info "SOUL.md installed to $Dst"
    } catch {
        Write-Warn "failed to copy SOUL.md to $Dst; profile not installed"
    }
}

function Install-System {
    param([string]$SystemFile)

    if (-not (Test-Path $SystemFile)) {
        Write-Warn 'SYSTEM.md missing from archive; skipping recipe install'
        return
    }

    # SYSTEM.md is a shared factual recipe, not user persona — always overwrite.
    # Keep a .bak of the previous version so local edits aren't silently destroyed.
    $ProfileDir = Join-Path $env:USERPROFILE '.openagent\profile'
    $Dst = Join-Path $ProfileDir 'SYSTEM.md'
    $null = New-Item -ItemType Directory -Path $ProfileDir -Force
    if (Test-Path $Dst) {
        try { Copy-Item -Path $Dst -Destination "$Dst.bak" -Force } catch {}
    }
    try {
        Copy-Item -Path $SystemFile -Destination $Dst -Force
        Write-Info "SYSTEM.md installed to $Dst"
    } catch {
        Write-Warn "failed to copy SYSTEM.md to $Dst; recipe not installed"
    }
}

function Install-HuaweiCloudOpenAPI {
    # OpenAPI snapshot: version-independent, ~22MB gzip, unpacks to
    # ~/.openagent/huaweicloudopenapi/. Mirror logic mirrors the binary: OBS primary,
    # GitHub fallback, OPENAGENT_MIRROR forces one. Idempotent: skip if dir exists
    # unless HUWEICLOUDOPENAPI_OVERWRITE=1. Soft-fail: never block the binary install.
    $SnapDir = Join-Path $env:USERPROFILE '.openagent\huaweicloudopenapi'
    if ((Test-Path $SnapDir) -and $env:HUWEICLOUDOPENAPI_OVERWRITE -ne '1') {
        Write-Info "$SnapDir already exists; skipping API snapshot (set HUWEICLOUDOPENAPI_OVERWRITE=1 to re-download)"
        return
    }

    $SnapTar = 'huaweicloudopenapi.tar.gz'
    $SnapSha = 'huaweicloudopenapi.tar.gz.sha256'
    $TarTmp  = Join-Path $TmpDir $SnapTar
    $ShaTmp  = Join-Path $TmpDir $SnapSha

    # Resolve snapshot source: OBS primary, GitHub fallback (unless MIRROR forces one).
    $SnapDl = $false
    if ($Mirror -ne 'github') {
        $obsBase = "$ObsEndpoint/$ObsPrefix/huaweicloudopenapi"
        $tarUrl  = "$obsBase/$SnapTar"
        $shaUrl  = "$obsBase/$SnapSha"
        Write-Info "Downloading API snapshot (OBS): $tarUrl"
        try {
            Invoke-WebRequest -Uri $tarUrl -OutFile $TarTmp -UseBasicParsing -TimeoutSec 120
            Invoke-WebRequest -Uri $shaUrl -OutFile $ShaTmp -UseBasicParsing -TimeoutSec 30
            $SnapDl = $true
            Write-Info 'Downloaded API snapshot from OBS'
        } catch {}
    }
    if (-not $SnapDl -and $Mirror -ne 'obs') {
        $ghBase  = "https://github.com/$Repo/releases/download/huaweicloudopenapi"
        $tarUrl  = "$ghBase/$SnapTar"
        $shaUrl  = "$ghBase/$SnapSha"
        Write-Info "Downloading API snapshot (GitHub): $tarUrl"
        try {
            Invoke-WebRequest -Uri $tarUrl -OutFile $TarTmp -UseBasicParsing -TimeoutSec 120
            Invoke-WebRequest -Uri $shaUrl -OutFile $ShaTmp -UseBasicParsing -TimeoutSec 30
            $SnapDl = $true
            Write-Info 'Downloaded API snapshot from GitHub'
        } catch {}
    }
    if (-not $SnapDl) {
        Write-Warn 'huaweicloudopenapi.tar.gz not found on OBS or GitHub; API search will be unavailable'
        return
    }

    # Verify SHA256 (sidecar downloaded above); soft-fail if mismatch.
    $expected = (Get-Content $ShaTmp -ErrorAction SilentlyContinue | Select-Object -First 1) -replace '\s.*',''
    if ($expected) {
        $actual = (Get-FileHash -Algorithm SHA256 $TarTmp).Hash.ToLower()
        if ($actual -ne $expected.ToLower()) {
            Write-Warn "huaweicloudopenapi.tar.gz checksum mismatch (expected $expected, got $actual); skipping"
            return
        }
        Write-Info 'API snapshot checksum OK'
    } else {
        Write-Warn 'huaweicloudopenapi.tar.gz.sha256 empty or unreadable; skipping integrity check'
    }

    # Reject path-traversal entries before unpacking (mirror install.sh guard).
    # Use $TarExe (system bsdtar) so a Git Bash GNU tar on PATH doesn't parse the
    # drive colon in $TarTmp as a remote-host indicator.
    try {
        $tarList = & $TarExe -tf $TarTmp 2>$null
        if ($LASTEXITCODE -ne 0) {
            Write-Warn "huaweicloudopenapi.tar.gz is unreadable (tar -tf failed); refusing to extract"
            return
        }
        $unsafe = $tarList | Where-Object { $_ -match '^\.\./|/\.\./|^/' }
        if ($unsafe) {
            Write-Warn "huaweicloudopenapi.tar.gz contains unsafe paths (.. or absolute); refusing to extract"
            return
        }
    } catch {
        Write-Warn "failed to inspect huaweicloudopenapi.tar.gz entries: $_; refusing to extract"
        return
    }

    # Extract. PowerShell 5.1+ has Expand-Archive (requires .zip; huaweicloudopenapi.tar.gz is
    # gzip tar, so shell out to tar — $TarExe, the system bsdtar, available on Win10 1803+).
    if (-not (Test-Path $SnapDir)) { $null = New-Item -ItemType Directory -Path $SnapDir -Force }
    else { Remove-Item -Recurse -Force $SnapDir; $null = New-Item -ItemType Directory -Path $SnapDir -Force }
    try {
        # bsdtar -xzf works on Windows 10 1803+. -C sets extraction dir.
        & $TarExe -xzf $TarTmp -C $SnapDir 2>$null
        if ($LASTEXITCODE -eq 0) {
            $n = (Get-ChildItem (Join-Path $SnapDir 'products') -Filter *.json -ErrorAction SilentlyContinue | Measure-Object).Count
            Write-Info "API snapshot installed to $SnapDir  ($n products)"
        } else {
            Write-Warn "tar failed (exit $LASTEXITCODE); API search will be unavailable"
            Remove-Item -Recurse -Force $SnapDir -ErrorAction SilentlyContinue
        }
    } catch {
        Write-Warn "failed to extract huaweicloudopenapi.tar.gz: $_; API search will be unavailable"
        Remove-Item -Recurse -Force $SnapDir -ErrorAction SilentlyContinue
    }
}

# ── download sources ───────────────────────────────────────────────────────────
# OBS is primary (users behind GFW can reach Huawei Cloud), GitHub is fallback.
# OPENAGENT_MIRROR=obs|github forces one source. OBS bucket is public-read.
$ObsEndpoint = if ($env:OBS_ENDPOINT) { $env:OBS_ENDPOINT } else { 'https://twb.obs.cn-north-4.myhuaweicloud.com' }
$ObsPrefix   = if ($env:OBS_PREFIX)   { $env:OBS_PREFIX }   else { 'openagent' }
$Mirror      = if ($env:OPENAGENT_MIRROR) { $env:OPENAGENT_MIRROR } else { '' }
if ($Mirror -and $Mirror -ne 'obs' -and $Mirror -ne 'github') {
    throw "OPENAGENT_MIRROR must be obs, github, or unset; got: $Mirror"
}

# ── detect arch (borrowed from upstream: Get-CimInstance Win32_Processor) ───────
# 0=x86, 5=ARM, 9=x86-64, 12=ARM64
$Arch = (Get-CimInstance Win32_Processor).Architecture
$ArchName = switch ($Arch) {
    9  { 'amd64' }
    12 { 'arm64' }
    default { throw "Unsupported architecture ($Arch). Download manually from https://github.com/$Repo/releases" }
}

$Version = $env:OPENAGENT_VERSION   # may be empty → resolve "latest" below

# ── resolve version + download (OBS primary, GitHub fallback) ──────────────────
$TmpDir = Join-Path $env:TEMP "openagent_install_$(Get-Random)"
New-Item -ItemType Directory -Path $TmpDir | Out-Null

function Download-From {
    param([string]$Base, [string]$Label, [string]$ArtifactName)
    $artifactUrl = "$Base/$ArtifactName"
    $sumsUrl     = "$Base/SHA256SUMS.txt"
    $zipPath     = Join-Path $TmpDir $ArtifactName
    Write-Info "Downloading $Label`: $artifactUrl"
    try {
        Invoke-WebRequest -Uri $artifactUrl -OutFile $zipPath -UseBasicParsing -TimeoutSec 60
    } catch { return $false }
    try {
        Invoke-WebRequest -Uri $sumsUrl -OutFile (Join-Path $TmpDir 'SHA256SUMS.txt') -UseBasicParsing -TimeoutSec 60
    } catch { return $false }
    return $true
}

function Resolve-GitHubLatest {
    Write-Info 'Fetching latest release version from GitHub...'
    try {
        $release = Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest"
        if ($release.tag_name) { return $release.tag_name }
    } catch {}
    return $null
}

$DownloadOk = $false

# --- OBS attempt (unless MIRROR=github) ---
if ($Mirror -ne 'github') {
    $obsPath = if ($Version) { "$ObsPrefix/$Version" } else { "$ObsPrefix/latest" }
    $obsBase = "$ObsEndpoint/$obsPath"

    if (-not $Version) {
        # Download latest/SHA256SUMS.txt to discover the real artifact name+version.
        try {
            Invoke-WebRequest -Uri "$obsBase/SHA256SUMS.txt" -OutFile (Join-Path $TmpDir 'SHA256SUMS.txt') -UseBasicParsing -TimeoutSec 60
            $sumsContent = Get-Content (Join-Path $TmpDir 'SHA256SUMS.txt') -ErrorAction SilentlyContinue
            foreach ($line in $sumsContent) {
                if ($line -match "${Name}_(v[0-9]+\.[0-9]+\.[0-9][^_\s]*)_windows_${ArchName}\.zip") {
                    $Version = $Matches[1]
                    break
                }
            }
            if ($Version) {
                Write-Info "Resolved latest version: $Version"
                $artifactName = "${Name}_${Version}_windows_${ArchName}.zip"
                $zipPath = Join-Path $TmpDir $artifactName
                try {
                    Invoke-WebRequest -Uri "$obsBase/$artifactName" -OutFile $zipPath -UseBasicParsing -TimeoutSec 60
                    $DownloadOk = $true
                    $DlBase = $obsBase
                    Write-Info 'Downloaded from OBS (latest)'
                } catch {}
            }
        } catch {}
    } else {
        $artifactName = "${Name}_${Version}_windows_${ArchName}.zip"
        if (Download-From -Base $obsBase -Label 'OBS' -ArtifactName $artifactName) {
            $DownloadOk = $true
            $DlBase = $obsBase
            Write-Info "Downloaded from OBS ($Version)"
        }
    }
    if (-not $DownloadOk -and $Mirror -eq 'obs') {
        throw "OPENAGENT_MIRROR=obs but OBS download failed. Check $ObsEndpoint/$ObsPrefix/ or set OPENAGENT_MIRROR=github"
    }
}

# --- GitHub fallback ---
if (-not $DownloadOk -and $Mirror -ne 'obs') {
    if (-not $Version) {
        $Version = Resolve-GitHubLatest
        if (-not $Version) {
            throw "failed to resolve latest version from GitHub (and OBS download also failed). Pass OPENAGENT_VERSION explicitly, or set OPENAGENT_MIRROR=obs/github."
        }
    }
    $artifactName = "${Name}_${Version}_windows_${ArchName}.zip"
    $ghBase = "https://github.com/$Repo/releases/download/$Version"
    Write-Warn 'OBS download unavailable; falling back to GitHub'
    if (-not (Download-From -Base $ghBase -Label 'GitHub' -ArtifactName $artifactName)) {
        throw "GitHub download also failed. Pass OPENAGENT_VERSION explicitly, or check network."
    }
    $DownloadOk = $true
    $DlBase = $ghBase
    Write-Info "Downloaded from GitHub ($Version)"
}

if (-not $DownloadOk) { throw 'no download source succeeded' }
if (-not $Version)    { throw 'version could not be determined' }
Write-Info "Installing $Name $Version from $Repo"

$Artifact   = "${Name}_${Version}_windows_${ArchName}.zip"
$ZipPath    = Join-Path $TmpDir $Artifact

try {
    # ── verify SHA256 (Get-FileHash) ───────────────────────────────────────────
    $SumsContent = Get-Content (Join-Path $TmpDir 'SHA256SUMS.txt')
    $Expected = $null
    # Escape $Artifact so '.' in the name matches literally, not as a regex wildcard.
    $ArtifactRe = [regex]::Escape($Artifact)
    foreach ($line in $SumsContent) {
        # SHA256SUMS.txt lines: "<hash>  <filename>" — match hash + exact filename.
        if ($line -match "^\s*([0-9a-fA-F]{64})\s+${ArtifactRe}\s*$") {
            $Expected = $Matches[1].ToLower()
            break
        }
    }
    if (-not $Expected) { throw "no checksum found for $Artifact in SHA256SUMS.txt" }

    $Actual = (Get-FileHash -Algorithm SHA256 $ZipPath).Hash.ToLower()
    if ($Actual -ne $Expected) {
        throw "checksum mismatch for ${Artifact}: expected ${Expected}, got ${Actual}"
    }
    Write-Info 'Checksum OK'

    # ── fetch top-level files (huawei-skills.txt, SOUL.md) ─────────────────────
    # These live alongside the archive, not inside it. Soft-fail.
    $SkillsFile = Join-Path $TmpDir 'huawei-skills.txt'
    try {
        Invoke-WebRequest -Uri "$DlBase/huawei-skills.txt" -OutFile $SkillsFile -UseBasicParsing -TimeoutSec 60
    } catch {
        Write-Warn "huawei-skills.txt not found at $DlBase/; skill install will be skipped"
    }
    $SoulFile = Join-Path $TmpDir 'SOUL.md'
    try {
        Invoke-WebRequest -Uri "$DlBase/SOUL.md" -OutFile $SoulFile -UseBasicParsing -TimeoutSec 60
    } catch {
        Write-Warn "SOUL.md not found at $DlBase/; profile will not be installed"
    }
    $SystemFile = Join-Path $TmpDir 'SYSTEM.md'
    try {
        Invoke-WebRequest -Uri "$DlBase/SYSTEM.md" -OutFile $SystemFile -UseBasicParsing -TimeoutSec 60
    } catch {
        Write-Warn "SYSTEM.md not found at $DlBase/; API search recipe will not be installed"
    }

    # ── extract ────────────────────────────────────────────────────────────────
    $ExtractDir = Join-Path $TmpDir 'extract'
    Expand-Archive -Path $ZipPath -DestinationPath $ExtractDir -Force

    $ExeName = "$Name.exe"
    $ExePath = Join-Path $ExtractDir $ExeName
    if (-not (Test-Path $ExePath)) { throw "archive does not contain $ExeName" }

    # ── install ────────────────────────────────────────────────────────────────
    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }
    Write-Info "Installing to $InstallDir ..."
    Copy-Item -Path $ExePath -Destination (Join-Path $InstallDir $ExeName) -Force

    # ── optional Huawei Cloud skills — INSIDE try, before finally deletes TmpDir ──
    try { Install-HuaweiSkills -SkillsFile $SkillsFile } catch { Write-Warn "skill install skipped: $_" }

    # ── SOUL.md profile — soft-coupled, never blocks install ──
    try { Install-Soul -SoulFile $SoulFile } catch { Write-Warn "SOUL.md install skipped: $_" }

    # ── SYSTEM.md recipe — always overwrite (factual recipe, not user data) ──
    try { Install-System -SystemFile $SystemFile } catch { Write-Warn "SYSTEM.md install skipped: $_" }

    # ── OpenAPI snapshot — soft-coupled, never blocks install ──
    try { Install-HuaweiCloudOpenAPI } catch { Write-Warn "API snapshot install skipped: $_" }
}
finally {
    Remove-Item -Recurse -Force $TmpDir -ErrorAction SilentlyContinue
}

# ── persist PATH for the user (borrowed from upstream) ─────────────────────────
$UserPath = [System.Environment]::GetEnvironmentVariable('PATH', 'User')
if ($UserPath -notlike "*$InstallDir*") {
    [System.Environment]::SetEnvironmentVariable('PATH', "$UserPath;$InstallDir", 'User')
    $env:PATH = "$env:PATH;$InstallDir"
    Write-Info "Added $InstallDir to your PATH."
}

# ── done (do not auto-start; just hint) ────────────────────────────────────────
Write-Info ''
Write-Info "$Name $Version installed to $InstallDir"
Write-Info ''
Write-Info "Run:  $Name serve"
Write-Info "Docs: https://github.com/$Repo"
