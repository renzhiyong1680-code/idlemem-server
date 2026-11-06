# 闲存云控制面日常巡检
#
# 每天（或部署后）跑一次，把"节点掉线、订单快到期、租户停服、备份过期"
# 这类需要人处理的事情一次性摊开，而不是等客户投诉才发现。
#
# 用法：
#   .\healthcheck.ps1 -Server http://127.0.0.1:8080 -AdminToken $env:XIANCUN_ADMIN_TOKEN
#   .\healthcheck.ps1 -Server http://127.0.0.1:8080 -BackupDir D:\backup
#
# 退出码：0 = 全部正常；1 = 有需关注项（WARN）；2 = 有失败项（FAIL）
#
# 对应 Linux 上可以直接用 curl 做同样的事，docker/less 环境也能跑：
#   curl -s $S/healthz
#   curl -s -H "X-Admin-Token: $T" $S/api/v1/summary
#   curl -s -H "X-Admin-Token: $T" $S/api/v1/orders
#   curl -s -H "X-Admin-Token: $T" $S/api/v1/tenants
#   curl -s $S/metrics

param(
    [string]$Server = "http://127.0.0.1:8080",
    [string]$AdminToken = $env:XIANCUN_ADMIN_TOKEN,
    [int]$ExpireWarnHours = 24,
    [string]$BackupDir = ""
)

$script:warn = 0
$script:fail = 0

function Write-Item {
    param([string]$Level, [string]$Name, [string]$Detail)
    if ($Level -eq "FAIL") { $script:fail++ }
    if ($Level -eq "WARN") { $script:warn++ }
    $tag = switch ($Level) { "OK" { "[ OK ]" } "WARN" { "[WARN]" } default { "[FAIL]" } }
    Write-Host ("$tag " + $Name)
    Write-Host ("       " + $Detail)
}

function Get-Headers {
    $h = @{}
    if ($AdminToken) { $h["X-Admin-Token"] = $AdminToken }
    return $h
}

function Invoke-Raw {
    param([string]$Path)
    $url = $Server.TrimEnd('/') + $Path
    return Invoke-WebRequest -Uri $url -Headers (Get-Headers) -UseBasicParsing -TimeoutSec 15
}

function Invoke-Api {
    param([string]$Path)
    $resp = Invoke-Raw $Path
    $bytes = $resp.RawContentStream.ToArray()
    $text = [Text.Encoding]::UTF8.GetString($bytes)
    return ($text | ConvertFrom-Json)
}

function Format-GB {
    param([double]$Bytes)
    return ([math]::Round($Bytes / 1073741824.0, 1).ToString() + " GB")
}

Write-Host ""
Write-Host ("闲存云巡检报告 " + (Get-Date -Format "yyyy-MM-dd HH:mm:ss"))
Write-Host ("控制面 " + $Server)
Write-Host ""

# 1. 控制面存活。这一项挂了，后面都不用查。
try {
    $h = Invoke-Raw "/healthz"
    if ($h.StatusCode -ge 200 -and $h.StatusCode -lt 300) {
        Write-Item "OK" "控制面存活" ("/healthz 返回 " + $h.StatusCode)
    } else {
        Write-Item "FAIL" "控制面存活" ("/healthz 返回 " + $h.StatusCode)
    }
} catch {
    Write-Item "FAIL" "控制面存活" $_.Exception.Message
    Write-Host ""
    Write-Host ("结论：" + $script:fail + " 项失败。控制面不可用，先查进程与日志。")
    exit 2
}

# 2. 节点在线率。
try {
    $sum = Invoke-Api "/api/v1/summary"
    $n = $sum.nodes
    if ($n.total -eq 0) {
        Write-Item "WARN" "节点在线" "还没有节点注册，数据面没有可用承载"
    } else {
        $rate = [math]::Round(100.0 * $n.online / $n.total, 1)
        $level = "OK"
        if ($n.online -eq 0) {
            $level = "FAIL"
        } elseif ($rate -lt 50) {
            $level = "WARN"
        }
        $detail = "{0}/{1} online（{2}%），容量 {3}，承载 {4}" -f $n.online, $n.total, $rate, (Format-GB $n.capacity), (Format-GB $n.used)
        if ($n.suspended -gt 0) { $detail = $detail + ("；" + $n.suspended + " 个节点因本机高负载暂停") }
        Write-Item $level "节点在线" $detail
    }
} catch {
    Write-Item "FAIL" "节点在线" $_.Exception.Message
}

# 3. 订单到期。
try {
    $orders = Invoke-Api "/api/v1/orders"
    $active = 0
    $soon = @()
    foreach ($o in $orders) {
        if ($o.status -ne "active") { continue }
        $active++
        $exp = [datetime]::Parse($o.expire_at)
        if ($exp.Year -lt 2000) { continue }
        $hours = [math]::Round(($exp.ToUniversalTime() - [datetime]::UtcNow).TotalHours, 1)
        if ($hours -le $ExpireWarnHours) {
            $soon += ("{0}（租户 {1}，剩 {2} h）" -f $o.id, $o.tenant_id, $hours)
        }
    }
    if ($soon.Count -gt 0) {
        Write-Item "WARN" "订单到期" ("{0} 张活跃订单将在 {1} 小时内到期：{2}" -f $soon.Count, $ExpireWarnHours, ($soon -join "；"))
    } else {
        Write-Item "OK" "订单到期" ("{0} 张活跃订单，{1} 小时内无到期" -f $active, $ExpireWarnHours)
    }
} catch {
    Write-Item "FAIL" "订单到期" $_.Exception.Message
}

# 4. 租户状态。
try {
    $tenants = Invoke-Api "/api/v1/tenants"
    $susp = @($tenants | Where-Object { $_.status -eq "suspended" })
    if ($susp.Count -gt 0) {
        $names = ($susp | ForEach-Object { $_.name }) -join "、"
        Write-Item "WARN" "租户状态" ("{0} 个租户共 {1} 个，其中 {2} 个已停服：{3}" -f $susp.Count, @($tenants).Count, $susp.Count, $names)
    } else {
        Write-Item "OK" "租户状态" ("{0} 个租户，无停服" -f @($tenants).Count)
    }
} catch {
    Write-Item "FAIL" "租户状态" $_.Exception.Message
}

# 5. 计量指标是否在产出（/metrics 有 xiancun_ 前缀的样本行）。
try {
    $m = Invoke-Raw "/metrics"
    $bytes = $m.RawContentStream.ToArray()
    $text = [Text.Encoding]::UTF8.GetString($bytes)
    $lines = @($text -split "`n" | Where-Object { $_ -match "^xiancun_" })
    if ($lines.Count -gt 0) {
        Write-Item "OK" "计量指标" ("/metrics 输出 " + $lines.Count + " 条 xiancun_ 样本")
    } else {
        Write-Item "FAIL" "计量指标" "/metrics 没有任何 xiancun_ 样本，监控会瞎"
    }
} catch {
    Write-Item "FAIL" "计量指标" $_.Exception.Message
}

# 6. 备份新鲜度（可选，需要 -BackupDir）。
if ($BackupDir -eq "") {
    Write-Item "WARN" "备份新鲜度" "未指定 -BackupDir，跳过检查（建议每天 backup export 一次）"
} elseif (-not (Test-Path -LiteralPath $BackupDir -PathType Container)) {
    Write-Item "FAIL" "备份新鲜度" ("备份目录不存在：" + $BackupDir)
} else {
    $latest = Get-ChildItem -LiteralPath $BackupDir -Filter "*.json" -File -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if ($null -eq $latest) {
        Write-Item "WARN" "备份新鲜度" "备份目录里没有 json 备份"
    } else {
        $age = [math]::Round(((Get-Date) - $latest.LastWriteTime).TotalHours, 1)
        if ($age -gt 26) {
            Write-Item "WARN" "备份新鲜度" ("最近备份 " + $latest.Name + " 已是 " + $age + " 小时前")
        } else {
            Write-Item "OK" "备份新鲜度" ("最近备份 " + $latest.Name + "（" + $age + " 小时前）")
        }
    }
}

Write-Host ""
if ($script:fail -gt 0) {
    Write-Host ("结论：" + $script:fail + " 项失败、" + $script:warn + " 项需关注")
    exit 2
}
if ($script:warn -gt 0) {
    Write-Host ("结论：" + $script:warn + " 项需关注")
    exit 1
}
Write-Host "结论：全部正常"
exit 0
