#Requires -Version 5.1
<#
.SYNOPSIS
  XianCun Cloud (idlemem) one-command local trial.

.DESCRIPTION
  Starts a complete local stack with zero manual steps:
    1. locate (or download) the server and agent binaries of a given release
    2. generate admin/node tokens and write a config file
    3. start the control plane and one agent, wait until both are ready
    4. create a demo tenant and a demo order
    5. print ready-to-paste redis-cli commands

  Everything runs inside -WorkDir (default: <repo>\quickstart). Logs, the sqlite-less
  buntdb file and pid files all live there, so the trial is fully disposable.

.PARAMETER WorkDir
  Working directory for binaries, config, db and logs. Created if missing.

.PARAMETER Api
  REST API listen address in host:port form. Default 127.0.0.1:8080.

.PARAMETER Gateway
  Redis protocol gateway listen address. Default 127.0.0.1:6379.

.PARAMETER Kv
  Agent KV listen address. Default 127.0.0.1:16380.

.PARAMETER Version
  Release tag to download binaries from when none are found locally. Default v0.1.1.

.PARAMETER BuildLocal
  Skip the download and use .\bin\idlemem-server(.exe) and .\bin\xiancun-agent(.exe).

.PARAMETER Stop
  Stop the processes started by an earlier run and exit.

.PARAMETER Force
  Kill leftover listeners on the configured ports before starting.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts\quickstart.ps1

.EXAMPLE
  pwsh -File scripts/quickstart.ps1 -Gateway 127.0.0.1:6380 -Force

.NOTES
  Requires Windows PowerShell 5.1+ or PowerShell 7+, and network access on first run
  (unless -BuildLocal is used). No Go toolchain and no Docker are required.
#>
[CmdletBinding()]
param(
  [string]$WorkDir = "",
  [string]$Api = "127.0.0.1:8080",
  [string]$Gateway = "127.0.0.1:6379",
  [string]$Kv = "127.0.0.1:16380",
  [string]$Version = "v0.1.1",
  [switch]$BuildLocal,
  [switch]$Stop,
  [switch]$Force
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

$repoRoot = Split-Path -Parent $PSScriptRoot
if (-not $WorkDir) { $WorkDir = Join-Path $repoRoot "quickstart" }
$WorkDir = [IO.Path]::GetFullPath($WorkDir)
$binDir = Join-Path $WorkDir "bin"
$logDir = Join-Path $WorkDir "log"
$serverExe = Join-Path $binDir "idlemem-server.exe"
$agentExe = Join-Path $binDir "xiancun-agent.exe"
$configPath = Join-Path $WorkDir "config.json"
$serverPidFile = Join-Path $WorkDir "server.pid"
$agentPidFile = Join-Path $WorkDir "agent.pid"
$serverLog = Join-Path $logDir "server.log"
$agentLog = Join-Path $logDir "agent.log"

$baseApi = "http://" + ($Api -replace "0\.0\.0\.0", "127.0.0.1")
$hostOnly, $apiPort = ($Api -split ":")[0], ($Api -split ":")[-1]
$gwPort = ($Gateway -split ":")[-1]
$kvPort = ($Kv -split ":")[-1]

function Write-Step([string]$Text) { Write-Host ("==> " + $Text) -ForegroundColor Cyan }
function Write-Info([string]$Text) { Write-Host ("    " + $Text) }
function Write-Warn2([string]$Text) { Write-Host ("[warn] " + $Text) -ForegroundColor Yellow }

function Stop-PidFile([string]$PidFile) {
  if (-not (Test-Path -LiteralPath $PidFile)) { return $false }
  $procId = (Get-Content -LiteralPath $PidFile -Raw).Trim()
  if ($procId -match '^\d+$') {
    try {
      Stop-Process -Id ([int]$procId) -Force -ErrorAction Stop
      Write-Info ("stopped pid " + $procId)
    } catch {
      Write-Info ("pid " + $procId + " not running")
    }
  }
  Remove-Item -LiteralPath $PidFile -Force -ErrorAction SilentlyContinue
  return $true
}

function Test-PortFree([int]$Port) {
  $listeners = Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue
  return ($null -eq $listeners -or $listeners.Count -eq 0)
}

function Wait-Healthz([int]$TimeoutSeconds) {
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  while ((Get-Date) -lt $deadline) {
    try {
      $r = Invoke-WebRequest -Uri ($baseApi + "/healthz") -TimeoutSec 3 -UseBasicParsing
      if ($r.StatusCode -eq 200) { return $true }
    } catch { }
    Start-Sleep -Milliseconds 500
  }
  return $false
}

function Invoke-Api([string]$Method, [string]$Path, $Body, [string]$Token) {
  $headers = @{ "Content-Type" = "application/json" }
  if ($Token) { $headers["Authorization"] = "Bearer " + $Token }
  $uri = $baseApi + $Path
  if ($null -eq $Body) {
    return Invoke-RestMethod -Method $Method -Uri $uri -Headers $headers -TimeoutSec 10
  }
  $json = ($Body | ConvertTo-Json -Depth 6 -Compress)
  return Invoke-RestMethod -Method $Method -Uri $uri -Headers $headers -Body $json -TimeoutSec 10
}

function Invoke-Download([string]$Uri, [string]$OutFile) {
  Write-Info ("downloading " + (Split-Path -Leaf $OutFile) + " ...")
  $tmp = $OutFile + ".part"
  Invoke-WebRequest -Uri $Uri -OutFile $tmp -TimeoutSec 300 -UseBasicParsing
  Move-Item -LiteralPath $tmp -Destination $OutFile -Force
}

if ($Stop) {
  Write-Step "stopping previous run"
  Stop-PidFile $agentPidFile | Out-Null
  Stop-PidFile $serverPidFile | Out-Null
  Write-Info "done"
  return
}

New-Item -ItemType Directory -Path $binDir -Force | Out-Null
New-Item -ItemType Directory -Path $logDir -Force | Out-Null

if ($Force) {
  foreach ($p in @($apiPort, $gwPort, $kvPort)) {
    $conns = Get-NetTCPConnection -State Listen -LocalPort ([int]$p) -ErrorAction SilentlyContinue
    foreach ($c in $conns) {
      if ($c.OwningProcess -and $c.OwningProcess -ne $PID) {
        Write-Warn2 ("port " + $p + " busy, killing pid " + $c.OwningProcess)
        try { Stop-Process -Id $c.OwningProcess -Force -ErrorAction Stop } catch { }
      }
    }
    Start-Sleep -Milliseconds 300
  }
}

Write-Step "resolving binaries"
$releaseBase = "https://github.com/renzhiyong1680-code/idlemem-server/releases/download/" + $Version
$verNo = $Version.TrimStart("v")

if ($BuildLocal) {
  $srcServer = Join-Path $repoRoot "bin\idlemem-server.exe"
  $srcAgent = Join-Path (Join-Path (Split-Path -Parent $repoRoot) "idlemem-core") "bin\xiancun-agent.exe"
  if (-not (Test-Path -LiteralPath $srcServer)) { throw ("missing " + $srcServer + "; run: go build -o bin\idlemem-server.exe ./cmd/server") }
  if (-not (Test-Path -LiteralPath $srcAgent)) { throw ("missing " + $srcAgent + "; run in idlemem-core: go build -o bin\xiancun-agent.exe ./cmd/xiancun-agent") }
  Copy-Item -LiteralPath $srcServer -Destination $serverExe -Force
  Copy-Item -LiteralPath $srcAgent -Destination $agentExe -Force
  Write-Info "using locally built binaries"
} else {
  if (-not (Test-Path -LiteralPath $serverExe)) {
    Invoke-Download ($releaseBase + "/server_" + $verNo + "_windows_amd64.exe") $serverExe
  }
  if (-not (Test-Path -LiteralPath $agentExe)) {
    Invoke-Download ($releaseBase + "/agent_" + $verNo + "_windows_amd64.exe") $agentExe
  }
  Write-Info ("binaries ready in " + $binDir)
}

Write-Step "generating tokens"
$tokenOut = (& $serverExe -gen-token) -join "`n"
$adminToken = ([regex]::Match($tokenOut, "admin_token:\s*([0-9a-f]+)")).Groups[1].Value
$nodeToken = ([regex]::Match($tokenOut, "node_token:\s*([0-9a-f]+)")).Groups[1].Value
if (-not $adminToken -or -not $nodeToken) { throw ("cannot parse -gen-token output: " + $tokenOut) }
Write-Info ("admin_token " + $adminToken.Substring(0, 8) + "... generated")

Write-Step "writing config"
$cfg = [ordered]@{
  api_addr                   = $Api
  gateway_addr               = $Gateway
  db_path                    = (Join-Path $WorkDir "idlemem-server.db")
  default_replicas           = 1
  customer_price_per_gb_hour = 0.0004
  supplier_price_per_gb_hour = 0.00032
  admin_token                = $adminToken
  node_token                 = $nodeToken
}
$cfgJson = ($cfg | ConvertTo-Json -Depth 4)
[IO.File]::WriteAllText($configPath, $cfgJson, (New-Object Text.UTF8Encoding($false)))
Write-Info $configPath

Write-Step "starting control plane"
if (-not (Test-PortFree ([int]$apiPort))) { throw ("port " + $apiPort + " is busy; re-run with -Force or pick another -Api") }
$srv = Start-Process -FilePath $serverExe -ArgumentList @("-config", $configPath) -WorkingDirectory $WorkDir `
  -RedirectStandardOutput $serverLog -RedirectStandardError ($serverLog + ".err") -WindowStyle Hidden -PassThru
Set-Content -LiteralPath $serverPidFile -Value $srv.Id -Encoding ascii
if (-not (Wait-Healthz 25)) { throw ("control plane did not become healthy; see " + $serverLog) }
Write-Info ("api " + $baseApi + " ready")

Write-Step "starting agent"
if (-not (Test-PortFree ([int]$kvPort))) { throw ("kv port " + $kvPort + " is busy; re-run with -Force or pick another -Kv") }
$agt = Start-Process -FilePath $agentExe -ArgumentList @("-server", $baseApi, "-kv", $Kv, "-node-token", $nodeToken) `
  -WorkingDirectory $WorkDir -RedirectStandardOutput $agentLog -RedirectStandardError ($agentLog + ".err") -WindowStyle Hidden -PassThru
Set-Content -LiteralPath $agentPidFile -Value $agt.Id -Encoding ascii

$nodeOnline = $false
for ($i = 0; $i -lt 40; $i++) {
  Start-Sleep -Milliseconds 700
  try {
    $nodesResp = Invoke-Api "GET" "/api/v1/nodes" $null $adminToken
    $nodes = @($nodesResp)
    if ($nodes.Count -gt 0 -and (@($nodes | Where-Object { $_.status -eq "online" }).Count -gt 0)) { $nodeOnline = $true; break }
  } catch { }
}
if (-not $nodeOnline) { Write-Warn2 ("agent started but no online node reported yet; see " + $agentLog) } else { Write-Info "agent online" }

Write-Step "creating demo tenant and order"
$tenant = Invoke-Api "POST" "/api/v1/tenants" @{ name = "demo-tenant"; quota_bytes = 10737418240 } $adminToken
Write-Info ("tenant " + $tenant.id)
$capacity = 2147483648
$order = $null
try {
  $order = Invoke-Api "POST" "/api/v1/orders" @{ tenant_id = $tenant.id; capacity_bytes = $capacity; replicas = 1; expire_hours = 720 } $adminToken
  Write-Info ("order " + $order.id + " capacity 2 GB")
} catch {
  Write-Warn2 ("order creation failed: " + $_.Exception.Message)
  Write-Warn2 "this usually means the agent reported less free memory than 2 GB; retry with a smaller capacity_bytes"
}

$summary = $null
try { $summary = Invoke-Api "GET" "/api/v1/summary" $null $adminToken } catch { }

Write-Host ""
Write-Host "XianCun Cloud local trial is up" -ForegroundColor Green
Write-Host ("  REST API   " + $baseApi)
Write-Host ("  gateway    " + $Gateway + "   (Redis protocol)")
Write-Host ("  agent KV   " + $Kv)
Write-Host ("  workdir    " + $WorkDir)
Write-Host ("  admin      " + $adminToken)
Write-Host ("  tenant     " + $tenant.name + "  token " + $tenant.token)
if ($summary) {
  Write-Host ("  nodes      " + $summary.nodes.online + " online / " + $summary.nodes.total + " total")
}
Write-Host ""
Write-Host "Try it with any Redis client:" -ForegroundColor Cyan
Write-Host ("  redis-cli -h " + $hostOnly + " -p " + $gwPort)
Write-Host ("  AUTH " + $tenant.token)
Write-Host "  SET hello world"
Write-Host "  GET hello"
Write-Host "  TTL hello"
Write-Host ""
Write-Host ("Stop everything:  powershell -ExecutionPolicy Bypass -File scripts\quickstart.ps1 -Stop -WorkDir " + $WorkDir)
