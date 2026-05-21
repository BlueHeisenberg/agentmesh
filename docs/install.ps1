# agentmesh installer (Windows) — downloads the latest signed release,
# verifies its sha256, installs the binary, adds the install dir to your
# user PATH, and registers agentmesh with one or more MCP harnesses.
#
#   iwr -useb https://blueheisenberg.github.io/agentmesh/install.ps1 | iex
#
# Env vars (set before piping into iex):
#   $env:VERSION         release tag       (default: latest)
#   $env:PREFIX          install dir       (default: $env:LOCALAPPDATA\Programs\agentmesh)
#   $env:NAME            node display name (default: $env:COMPUTERNAME)
#   $env:HARNESS         comma list:       claude,cursor,codex,antigravity,all,none
#                                          (default: interactive prompt; if not interactive: claude)
#   $env:SKIP_REGISTER   if set, skip all harness registration
#   $env:CLAUDE_CONFIG, $env:CURSOR_CONFIG, $env:CODEX_CONFIG, $env:ANTIGRAVITY_CONFIG
#                        override individual config paths

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

# Force TLS 1.2+ for GitHub downloads on older PowerShell (5.1 default).
[Net.ServicePointManager]::SecurityProtocol =
  [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

# Force UTF-8 console output so the ✓ / ✗ glyphs don't render as "?".
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch {}

# ---- pretty printing -----------------------------------------------------
# Glyph fallback: if the host is plain Windows PowerShell on a non-UTF-8
# code page, render with ASCII so output isn't garbled.
$utf8 = ([Console]::OutputEncoding.WebName -eq 'utf-8')
$G_OK   = if ($utf8) { '✓' } else { '+' }
$G_FAIL = if ($utf8) { '✗' } else { 'X' }

function Step    ($m) { Write-Host "::" -ForegroundColor DarkYellow -NoNewline; Write-Host " $m" }
function OK      ($m) { Write-Host $G_OK   -ForegroundColor Green       -NoNewline; Write-Host " $m" }
function Warn    ($m) { Write-Host "!"     -ForegroundColor DarkYellow  -NoNewline; Write-Host " $m" }
function Die     ($m) { Write-Host $G_FAIL -ForegroundColor Red         -NoNewline; Write-Host " $m"; exit 1 }
function Subtle  ($m) { Write-Host "    $m" -ForegroundColor DarkGray }

# ---- detect arch ---------------------------------------------------------
# Use the environment variable — RuntimeInformation::ProcessArchitecture is
# an enum that Windows PowerShell 5.1's `switch` doesn't string-coerce, which
# made every machine match the default branch. PROCESSOR_ARCHITEW6432 wins if
# we're running 32-bit PowerShell on 64-bit Windows.
$archEnv = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch -Regex ($archEnv) {
  '^(AMD64|EM64T|x64)$' { 'amd64'; break }
  '^ARM64$'             { Die "windows/arm64 builds aren't published yet — install via WSL or build from source." }
  '^x86$'               { Die "32-bit Windows isn't supported; please install 64-bit Windows." }
  default               { Die "unsupported arch: '$archEnv'" }
}

$repo    = if ($env:REPO)    { $env:REPO }    else { 'BlueHeisenberg/agentmesh' }
$version = if ($env:VERSION) { $env:VERSION } else { 'latest' }
$bin     = 'agentmesh.exe'

# ---- resolve version -----------------------------------------------------

if ($version -eq 'latest') {
  Step "fetching latest release tag for $repo"
  try {
    $rel = Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$repo/releases/latest"
    $version = $rel.tag_name
  } catch {
    Die "could not resolve latest release: $_"
  }
}
$verNoV = $version -replace '^v',''

# ---- install dir ---------------------------------------------------------

$prefix = if ($env:PREFIX) { $env:PREFIX } else { Join-Path $env:LOCALAPPDATA 'Programs\agentmesh' }
New-Item -ItemType Directory -Path $prefix -Force | Out-Null

# ---- download + verify ---------------------------------------------------

$tmp = New-Item -ItemType Directory -Path (Join-Path $env:TEMP "agentmesh-install-$(Get-Random)") -Force
try {
  $archive = "agentmesh_${verNoV}_windows_${arch}.zip"
  $base    = "https://github.com/$repo/releases/download/$version"

  Step "downloading $archive ($version)"
  Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive"       -OutFile (Join-Path $tmp $archive)
  Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt"  -OutFile (Join-Path $tmp "checksums.txt")

  Step "verifying sha256"
  $expected = (Get-Content (Join-Path $tmp "checksums.txt") |
    Where-Object { $_ -match " $([regex]::Escape($archive))$" }) -split '\s+' | Select-Object -First 1
  if (-not $expected) { Die "no checksum entry for $archive" }
  $actual = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash.ToLower()
  if ($expected -ne $actual) { Die "checksum mismatch — expected $expected, got $actual" }
  OK "checksum ok"

  Step "extracting"
  Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force

  $target = Join-Path $prefix $bin
  Step "installing to $target"
  # Try to overwrite even if the binary is in use by a running harness:
  # rename-then-replace works as long as no process holds an exclusive handle.
  if (Test-Path $target) {
    try { Remove-Item $target -Force } catch {
      Warn "couldn't remove existing $target ($_); did you stop your harness?"
      throw
    }
  }
  Move-Item -Path (Join-Path $tmp $bin) -Destination $target -Force
  OK "installed agentmesh $version"
}
finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

Subtle ""
& $target whoami 2>$null | ForEach-Object { Subtle $_ }
Subtle ""

# ---- add to user PATH ----------------------------------------------------

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$onPath   = ($userPath -split ';' | Where-Object { $_ -and ($_ -ieq $prefix) }).Count -gt 0
if (-not $onPath) {
  $newPath = if ([string]::IsNullOrEmpty($userPath)) { $prefix } else { "$userPath;$prefix" }
  [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
  OK "added $prefix to your user PATH (open a new terminal to use ``agentmesh``)"
}

# ===========================================================================
# Multi-harness registration
# ===========================================================================

if ($env:SKIP_REGISTER) {
  Step "skipping harness registration (SKIP_REGISTER set)"
  Write-Host ""
  Write-Host "Binary is installed. Wire it up at https://blueheisenberg.github.io/agentmesh/"
  return
}

$shortHost = if ($env:NAME) { $env:NAME } else { $env:COMPUTERNAME }
$binPath   = $target

$claudeCfg      = if ($env:CLAUDE_CONFIG)      { $env:CLAUDE_CONFIG }      else { Join-Path $env:USERPROFILE '.claude.json' }
$cursorCfg      = if ($env:CURSOR_CONFIG)      { $env:CURSOR_CONFIG }      else { Join-Path $env:USERPROFILE '.cursor\mcp.json' }
$codexCfg       = if ($env:CODEX_CONFIG)       { $env:CODEX_CONFIG }       else { Join-Path $env:USERPROFILE '.codex\config.toml' }
$antigravityCfg = if ($env:ANTIGRAVITY_CONFIG) { $env:ANTIGRAVITY_CONFIG } else { Join-Path $env:USERPROFILE '.gemini\antigravity\mcp_config.json' }

# Prompt for harness selection
$harness = $env:HARNESS
if (-not $harness) {
  if ([Environment]::UserInteractive -and $Host.UI.RawUI) {
    Write-Host ""
    Write-Host "Where should I register agentmesh?" -ForegroundColor White
    Write-Host "  " -NoNewline; Write-Host "1" -ForegroundColor DarkYellow -NoNewline; Write-Host ") claude code     " -NoNewline; Write-Host $claudeCfg -ForegroundColor DarkGray
    Write-Host "  " -NoNewline; Write-Host "2" -ForegroundColor DarkYellow -NoNewline; Write-Host ") cursor          " -NoNewline; Write-Host $cursorCfg -ForegroundColor DarkGray
    Write-Host "  " -NoNewline; Write-Host "3" -ForegroundColor DarkYellow -NoNewline; Write-Host ") chatgpt codex   " -NoNewline; Write-Host $codexCfg -ForegroundColor DarkGray
    Write-Host "  " -NoNewline; Write-Host "4" -ForegroundColor DarkYellow -NoNewline; Write-Host ") antigravity     " -NoNewline; Write-Host $antigravityCfg -ForegroundColor DarkGray
    Write-Host "  " -NoNewline; Write-Host "5" -ForegroundColor DarkYellow -NoNewline; Write-Host ") all of the above"
    Write-Host "  " -NoNewline; Write-Host "6" -ForegroundColor DarkYellow -NoNewline; Write-Host ") none — I'll do it manually"
    $choice = Read-Host "choose (comma-separated, e.g. 1,2) [default: 1]"
    if (-not $choice) { $choice = '1' }
    $picked = @()
    foreach ($c in ($choice -split ',')) {
      switch ($c.Trim()) {
        '1' { $picked += 'claude' }
        '2' { $picked += 'cursor' }
        '3' { $picked += 'codex'  }
        '4' { $picked += 'antigravity' }
        '5' { $picked = @('claude','cursor','codex','antigravity'); break }
        '6' { $picked = @('none'); break }
        default { Warn "ignoring invalid choice: $c" }
      }
    }
    $harness = ($picked -join ' ')
  } else {
    Step "non-interactive — defaulting to claude (override with `$env:HARNESS=...)"
    $harness = 'claude'
  }
}
$harness = ($harness -replace ',', ' ').Trim()

if ($harness -eq 'none' -or [string]::IsNullOrWhiteSpace($harness)) {
  Step "no harness registration requested"
  return
}

# Registration helpers -----------------------------------------------------

function Register-McpJson {
  param([string]$Kind, [string]$ConfigPath, [string]$BinPath, [string]$HostName)

  $dir = Split-Path -Parent $ConfigPath
  if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }

  $existed = Test-Path $ConfigPath
  $data = $null
  if ($existed) {
    try {
      $raw = Get-Content -Raw -Path $ConfigPath -Encoding UTF8
      if ($raw -and $raw.Trim()) { $data = $raw | ConvertFrom-Json }
    } catch {
      Warn "${Kind}: $ConfigPath isn't valid JSON — not touching it"; return
    }
  }
  if ($null -eq $data) { $data = New-Object PSObject }
  if ($data -isnot [PSObject]) { Warn "${Kind}: $ConfigPath has an unexpected shape — not touching it"; return }

  if (-not $data.PSObject.Properties.Match('mcpServers').Count) {
    $data | Add-Member -NotePropertyName 'mcpServers' -NotePropertyValue (New-Object PSObject)
  }
  $servers = $data.mcpServers

  $desired = [PSCustomObject]@{
    command = $BinPath
    args    = @('serve', "--name=$HostName")
  }

  $existing = $servers.PSObject.Properties.Match('agentmesh')
  if ($existing.Count -gt 0) {
    $cur = $servers.agentmesh
    if ($cur.command -eq $desired.command -and
        (Compare-Object $cur.args $desired.args -SyncWindow 0) -eq $null) {
      OK "${Kind}: already configured"
      return
    }
    $servers.agentmesh = $desired
  } else {
    $servers | Add-Member -NotePropertyName 'agentmesh' -NotePropertyValue $desired
  }

  if ($existed) { Copy-Item $ConfigPath "$ConfigPath.bak" -Force }
  $json = $data | ConvertTo-Json -Depth 20
  Set-Content -Path $ConfigPath -Value $json -Encoding UTF8
  if ($existed) { OK "${Kind}: registered in $ConfigPath (backup at $ConfigPath.bak)" }
  else          { OK "${Kind}: created $ConfigPath with agentmesh entry" }
}

function Register-CodexToml {
  param([string]$ConfigPath, [string]$BinPath, [string]$HostName)

  $dir = Split-Path -Parent $ConfigPath
  if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }

  $tomlPath = $BinPath.Replace('\','/')
  $section  = "`n[mcp_servers.agentmesh]`ncommand = `"$tomlPath`"`nargs = [`"serve`", `"--name=$HostName`"]`n"

  if (-not (Test-Path $ConfigPath)) {
    Set-Content -Path $ConfigPath -Value $section.TrimStart() -Encoding UTF8
    OK "codex: created $ConfigPath with agentmesh entry"
    return
  }
  $text = Get-Content -Raw -Path $ConfigPath -Encoding UTF8
  if ($text -match '\[mcp_servers\.agentmesh\]') {
    OK "codex: already configured"
    return
  }
  Copy-Item $ConfigPath "$ConfigPath.bak" -Force
  if (-not $text.EndsWith("`n")) { $section = "`n" + $section.TrimStart("`n") }
  Add-Content -Path $ConfigPath -Value $section -Encoding UTF8
  OK "codex: registered in $ConfigPath (backup at $ConfigPath.bak)"
}

Write-Host ""
foreach ($h in ($harness -split '\s+' | Where-Object { $_ })) {
  switch ($h) {
    'claude'      { Register-McpJson 'claude'      $claudeCfg      $binPath $shortHost }
    'cursor'      { Register-McpJson 'cursor'      $cursorCfg      $binPath $shortHost }
    'codex'       { Register-CodexToml             $codexCfg       $binPath $shortHost }
    'antigravity' { Register-McpJson 'antigravity' $antigravityCfg $binPath $shortHost }
    default       { Warn "unknown harness: $h (allowed: claude, cursor, codex, antigravity)" }
  }
}

Write-Host ""
Write-Host "You're done." -ForegroundColor White -NoNewline
Write-Host " Restart the harness(es) you registered with, then try " -NoNewline
Write-Host "mesh_whoami" -ForegroundColor DarkYellow -NoNewline
Write-Host "."
Subtle "Docs: https://blueheisenberg.github.io/agentmesh/"
