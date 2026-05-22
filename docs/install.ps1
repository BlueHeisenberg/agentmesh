# agentmesh installer (Windows).
#
# Default behaviour:
#   - If the binary or any harness config is already present, just refresh the
#     binary in place and exit.
#   - Otherwise, install the binary and show a checkbox menu (arrow keys,
#     space to toggle, enter to confirm) for harness selection.
#
#   iwr -useb https://blueheisenberg.github.io/agentmesh/install.ps1 | iex
#
# Env vars (set before piping into iex):
#   $env:VERSION         release tag       (default: latest)
#   $env:PREFIX          install dir       (default: $env:LOCALAPPDATA\Programs\agentmesh)
#   $env:NAME            explicit node name (default: derived from CWD + git branch)
#   $env:HARNESS         comma list: claude,cursor,codex,antigravity,all,none
#   $env:RECONFIGURE     if set, force the harness picker even when already configured
#   $env:SKIP_REGISTER   if set, never touch any harness config
#   $env:CLAUDE_CONFIG, $env:CURSOR_CONFIG, $env:CODEX_CONFIG, $env:ANTIGRAVITY_CONFIG
#                        override individual config paths

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

# Force TLS 1.2+ for GitHub downloads on older PowerShell (5.1 default).
[Net.ServicePointManager]::SecurityProtocol =
  [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

# ---- printing (ASCII-only) ------------------------------------------------

function Step    ($m) { Write-Host "::"   -ForegroundColor DarkYellow -NoNewline; Write-Host " $m" }
function OK      ($m) { Write-Host "[ok]" -ForegroundColor Green      -NoNewline; Write-Host " $m" }
function Warn    ($m) { Write-Host "[!] " -ForegroundColor DarkYellow -NoNewline; Write-Host $m }
function Die     ($m) { Write-Host "[x] " -ForegroundColor Red        -NoNewline; Write-Host $m; exit 1 }
function Subtle  ($m) { Write-Host "    $m" -ForegroundColor DarkGray }

# ---- detect arch ----------------------------------------------------------

$archEnv = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch -Regex ($archEnv) {
  '^(AMD64|EM64T|x64)$' { 'amd64'; break }
  '^ARM64$'             { Die "windows/arm64 builds aren't published yet - install via WSL or build from source." }
  '^x86$'               { Die "32-bit Windows isn't supported; please install 64-bit Windows." }
  default               { Die "unsupported arch: '$archEnv'" }
}

$repo    = if ($env:REPO)    { $env:REPO }    else { 'BlueHeisenberg/agentmesh' }
$version = if ($env:VERSION) { $env:VERSION } else { 'latest' }
$bin     = 'agentmesh.exe'

# ---- resolve version ------------------------------------------------------

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

# ---- known harness config paths ------------------------------------------

$claudeCfg      = if ($env:CLAUDE_CONFIG)      { $env:CLAUDE_CONFIG }      else { Join-Path $env:USERPROFILE '.claude.json' }
$cursorCfg      = if ($env:CURSOR_CONFIG)      { $env:CURSOR_CONFIG }      else { Join-Path $env:USERPROFILE '.cursor\mcp.json' }
$codexCfg       = if ($env:CODEX_CONFIG)       { $env:CODEX_CONFIG }       else { Join-Path $env:USERPROFILE '.codex\config.toml' }
$antigravityCfg = if ($env:ANTIGRAVITY_CONFIG) { $env:ANTIGRAVITY_CONFIG } else { Join-Path $env:USERPROFILE '.gemini\antigravity\mcp_config.json' }

function Test-AgentmeshIn($path, $kind) {
  if (-not (Test-Path $path)) { return $false }
  $text = Get-Content -Raw -Path $path -Encoding UTF8 -ErrorAction SilentlyContinue
  if (-not $text) { return $false }
  if ($kind -eq 'codex') {
    return $text -match '\[mcp_servers\.agentmesh\]'
  } else {
    return $text -match '"agentmesh"'
  }
}

$existing = @()
if (Test-AgentmeshIn $claudeCfg      'json')  { $existing += 'claude' }
if (Test-AgentmeshIn $cursorCfg      'json')  { $existing += 'cursor' }
if (Test-AgentmeshIn $codexCfg       'codex') { $existing += 'codex' }
if (Test-AgentmeshIn $antigravityCfg 'json')  { $existing += 'antigravity' }

# ---- install dir ----------------------------------------------------------

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
  if ($expected -ne $actual) { Die "checksum mismatch: expected $expected, got $actual" }
  OK "checksum ok"

  Step "extracting"
  Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force

  $target = Join-Path $prefix $bin
  Step "installing to $target"
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

# ---- add to user PATH -----------------------------------------------------

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$onPath   = ($userPath -split ';' | Where-Object { $_ -and ($_ -ieq $prefix) }).Count -gt 0
if (-not $onPath) {
  $newPath = if ([string]::IsNullOrEmpty($userPath)) { $prefix } else { "$userPath;$prefix" }
  [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
  OK "added $prefix to your user PATH (open a new terminal to use ``agentmesh``)"
}

# ===========================================================================
# Harness registration
# ===========================================================================

if ($env:SKIP_REGISTER) {
  Step "skipping harness registration (SKIP_REGISTER set)"
  return
}

# Fast path: already configured, no $env:RECONFIGURE -> just exit.
if ($existing.Count -gt 0 -and -not $env:RECONFIGURE -and -not $env:HARNESS) {
  OK ("agentmesh already configured for: " + ($existing -join ', '))
  Subtle "re-run with `$env:RECONFIGURE=1; ... | iex to add/change harnesses"
  Write-Host ""
  Write-Host "You're done." -ForegroundColor White -NoNewline
  Write-Host " Restart the harness(es) above to pick up v$verNoV."
  return
}

# ---- checkbox menu --------------------------------------------------------

function Show-CheckboxMenu {
  param(
    [string]$Prompt,
    [string[]]$Labels,
    [bool[]]$InitialChecked
  )
  $n = $Labels.Length
  $checked = New-Object 'bool[]' $n
  for ($i = 0; $i -lt $n; $i++) { $checked[$i] = $InitialChecked[$i] }
  $cursor = 0

  function Draw($prompt, $labels, $checked, $cursor) {
    Write-Host $prompt -ForegroundColor White
    for ($i = 0; $i -lt $labels.Length; $i++) {
      $arrow = if ($i -eq $cursor) { "> " } else { "  " }
      $mark  = if ($checked[$i])   { "[x]" } else { "[ ]" }
      $line  = "$arrow$mark $($labels[$i])"
      if ($i -eq $cursor) {
        Write-Host $line -ForegroundColor DarkYellow
      } else {
        Write-Host $line
      }
    }
    Write-Host "  arrows: move | space: toggle | enter: confirm | q: cancel" -ForegroundColor DarkGray
  }

  # Initial draw.
  [Console]::CursorVisible = $false
  $startY = [Console]::CursorTop
  Draw $Prompt $Labels $checked $cursor

  while ($true) {
    $k = [Console]::ReadKey($true)
    switch ($k.Key) {
      'UpArrow'   { if ($cursor -gt 0)     { $cursor-- } }
      'DownArrow' { if ($cursor -lt $n - 1){ $cursor++ } }
      'Spacebar'  { $checked[$cursor] = -not $checked[$cursor] }
      'Enter'     {
        [Console]::CursorVisible = $true
        $sel = @()
        for ($i = 0; $i -lt $n; $i++) { if ($checked[$i]) { $sel += $i } }
        return ,$sel
      }
      'Q' {
        [Console]::CursorVisible = $true
        return ,@()
      }
    }
    # Re-render: rewind to start, redraw.
    [Console]::SetCursorPosition(0, $startY)
    Draw $Prompt $Labels $checked $cursor
  }
}

# Default-check the harnesses already configured. If nothing is configured,
# pre-check claude (the most common case).
$initial = @(
  ($existing -contains 'claude'),
  ($existing -contains 'cursor'),
  ($existing -contains 'codex'),
  ($existing -contains 'antigravity')
)
if (-not ($initial -contains $true)) { $initial[0] = $true }

$harness = $env:HARNESS
if (-not $harness) {
  if ([Environment]::UserInteractive -and $Host.UI.RawUI) {
    Write-Host ""
    $picks = Show-CheckboxMenu `
      -Prompt "Select harness(es) to register agentmesh with:" `
      -Labels @(
        "claude code     $claudeCfg",
        "cursor          $cursorCfg",
        "chatgpt codex   $codexCfg",
        "antigravity     $antigravityCfg"
      ) `
      -InitialChecked $initial

    $names = @('claude','cursor','codex','antigravity')
    $picked = @()
    foreach ($i in $picks) { $picked += $names[$i] }
    $harness = ($picked -join ' ')
  } else {
    Step "non-interactive - defaulting to claude (override with `$env:HARNESS=...)"
    $harness = 'claude'
  }
}
$harness = ($harness -replace ',', ' ').Trim()
switch -Regex ($harness) {
  '\ball\b'  { $harness = 'claude cursor codex antigravity' }
  '\bnone\b' { $harness = '' }
}

if ([string]::IsNullOrWhiteSpace($harness)) {
  Step "no harness selected - binary installed, no MCP registration done."
  Subtle "re-run with `$env:RECONFIGURE=1; ... | iex to pick later."
  return
}

# ---- Registration helpers (PowerShell-native, no Python) -----------------

function Register-McpJson {
  param([string]$Kind, [string]$ConfigPath, [string]$BinPath, [string]$NameOverride)

  $dir = Split-Path -Parent $ConfigPath
  if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }

  $existed = Test-Path $ConfigPath
  $data = $null
  if ($existed) {
    try {
      $raw = Get-Content -Raw -Path $ConfigPath -Encoding UTF8
      if ($raw -and $raw.Trim()) { $data = $raw | ConvertFrom-Json }
    } catch {
      Warn "${Kind}: $ConfigPath isn't valid JSON - not touching it"; return
    }
  }
  if ($null -eq $data) { $data = New-Object PSObject }
  if (-not $data.PSObject.Properties.Match('mcpServers').Count) {
    $data | Add-Member -NotePropertyName 'mcpServers' -NotePropertyValue (New-Object PSObject)
  }
  $servers = $data.mcpServers

  $args = @('serve')
  if ($NameOverride) { $args += "--name=$NameOverride" }
  $desired = [PSCustomObject]@{ command = $BinPath; args = $args }

  $present = $servers.PSObject.Properties.Match('agentmesh').Count -gt 0
  if ($present) {
    $cur = $servers.agentmesh
    $sameCmd  = ($cur.command -eq $desired.command)
    $sameArgs = ((Compare-Object $cur.args $desired.args -SyncWindow 0) -eq $null)
    if ($sameCmd -and $sameArgs) {
      OK "${Kind}: already configured"; return
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
  param([string]$ConfigPath, [string]$BinPath, [string]$NameOverride)

  $dir = Split-Path -Parent $ConfigPath
  if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }

  $tomlBin = $BinPath.Replace('\','/')
  $argsToml = '"serve"'
  if ($NameOverride) { $argsToml = "`"serve`", `"--name=$NameOverride`"" }
  $section = "`n[mcp_servers.agentmesh]`ncommand = `"$tomlBin`"`nargs = [$argsToml]`n"

  if (-not (Test-Path $ConfigPath)) {
    Set-Content -Path $ConfigPath -Value $section.TrimStart() -Encoding UTF8
    OK "codex: created $ConfigPath with agentmesh entry"
    return
  }
  $text = Get-Content -Raw -Path $ConfigPath -Encoding UTF8
  if ($text -match '\[mcp_servers\.agentmesh\]') {
    OK "codex: already configured"; return
  }
  Copy-Item $ConfigPath "$ConfigPath.bak" -Force
  if (-not $text.EndsWith("`n")) { $section = "`n" + $section.TrimStart("`n") }
  Add-Content -Path $ConfigPath -Value $section -Encoding UTF8
  OK "codex: registered in $ConfigPath (backup at $ConfigPath.bak)"
}

$binPath  = $target
$nameOver = $env:NAME

Write-Host ""
foreach ($h in ($harness -split '\s+' | Where-Object { $_ })) {
  switch ($h) {
    'claude'      { Register-McpJson 'claude'      $claudeCfg      $binPath $nameOver }
    'cursor'      { Register-McpJson 'cursor'      $cursorCfg      $binPath $nameOver }
    'codex'       { Register-CodexToml             $codexCfg       $binPath $nameOver }
    'antigravity' { Register-McpJson 'antigravity' $antigravityCfg $binPath $nameOver }
    default       { Warn "unknown harness: $h (allowed: claude, cursor, codex, antigravity)" }
  }
}

Write-Host ""
Write-Host "You're done." -ForegroundColor White -NoNewline
Write-Host " Restart the harness(es) you registered with, then try " -NoNewline
Write-Host "mesh_whoami" -ForegroundColor DarkYellow -NoNewline
Write-Host "."
Subtle "Each session starts loopback-only. Have the agent call mesh_open_lan to expose it to the LAN."
Subtle "Docs: https://blueheisenberg.github.io/agentmesh/"
