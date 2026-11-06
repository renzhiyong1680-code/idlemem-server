# 闲存云发布打包：交叉编译 + 版本注入 + SHA256 校验和。
#
# 用法：
#   .\scripts\release.ps1                          # 版本取 git describe，缺省 0.1.0-dev
#   .\scripts\release.ps1 -Version 0.2.0           # 指定版本
#   .\scripts\release.ps1 -Version 0.2.0 -Only server,cli
#
# 产物：
#   dist\<version>\<name>_<version>_<os>_<arch>[.exe]
#   dist\<version>\SHA256SUMS
#
# 说明：托盘程序（xiancun-tray）依赖 CGO 与平台 GUI，不在交叉编译范围内，
#      需要在目标平台上单独构建（见 idlemem-desktop/deploy/README.md）。

param(
    [string]$Version = "",
    [string[]]$Only = @("agent", "server", "cli"),
    [string]$OutRoot = ""
)

$ErrorActionPreference = "Stop"

$Root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$Go = "C:\Program Files\Go\bin\go.exe"
if (-not (Test-Path -LiteralPath $Go)) {
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if (-not $cmd) { throw "go not found; install Go or edit the path in this script" }
    $Go = $cmd.Source
}

if (-not $Version) {
    Push-Location $Root
    try {
        $desc = & git describe --tags --always 2>$null
        if ($LASTEXITCODE -eq 0 -and $desc) {
            $Version = ($desc -replace "^v", "").Trim()
        } else {
            $Version = "0.1.0-dev"
        }
    } finally { Pop-Location }
}

if (-not $OutRoot) { $OutRoot = Join-Path $Root "dist" }
$OutDir = Join-Path $OutRoot $Version
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

# 目标平台（Linux 服务器 + Windows 节点 + Apple Silicon / Intel Mac）
$Targets = @(
    @{ OS = "linux";   Arch = "amd64" },
    @{ OS = "linux";   Arch = "arm64" },
    @{ OS = "windows"; Arch = "amd64" },
    @{ OS = "darwin";  Arch = "amd64" },
    @{ OS = "darwin";  Arch = "arm64" }
)

$Components = @(
    @{ Name = "agent";  Dir = "../idlemem-core"; Pkg = "./cmd/xiancun-agent" },
    @{ Name = "server"; Dir = ".";               Pkg = "./cmd/server" },
    @{ Name = "cli";    Dir = ".";               Pkg = "./cmd/xiancun-cli" }
)

$env:GOPROXY = "off"          # 依赖已在本地 module cache，避免网络抖动
$env:CGO_ENABLED = "0"        # 纯 Go 交叉编译
$env:GOFLAGS = "-trimpath"

$Built = @()
foreach ($comp in $Components) {
    if ($Only -notcontains $comp.Name) { continue }
    $pkgDir = Join-Path $Root $comp.Dir
    if (-not (Test-Path -LiteralPath $pkgDir)) { throw "missing repo: $pkgDir" }

    foreach ($t in $Targets) {
        $outName = "$($comp.Name)_${Version}_$($t.OS)_$($t.Arch)"
        if ($t.OS -eq "windows") { $outName += ".exe" }
        $outPath = Join-Path $OutDir $outName

        $env:GOOS = $t.OS
        $env:GOARCH = $t.Arch
        Push-Location $pkgDir
        try {
            $ldflags = "-s -w -X main.version=$Version"
            & $Go build -ldflags $ldflags -o $outPath $comp.Pkg
            if ($LASTEXITCODE -ne 0) { throw "build failed: $($comp.Name)/$($t.OS)/$($t.Arch)" }
        } finally { Pop-Location }

        $size = (Get-Item -LiteralPath $outPath).Length
        "{0,-8} {1,-8} {2,-8} {3,10:N0} bytes" -f $comp.Name, $t.OS, $t.Arch, $size | Write-Host
        $Built += $outPath
    }
}

# 校验和（sha256，格式与 sha256sum 输出一致，便于 Linux 侧核对）
$sumPath = Join-Path $OutDir "SHA256SUMS"
$lines = foreach ($f in $Built) {
    $h = (Get-FileHash -LiteralPath $f -Algorithm SHA256).Hash.ToLower()
    "$h  $(Split-Path -Leaf $f)"
}
Set-Content -LiteralPath $sumPath -Value $lines -Encoding ascii

Write-Host ""
Write-Host "dist: $OutDir"
Write-Host "files: $($Built.Count)"
Write-Host "checksums: $sumPath"
Write-Host ""
Write-Host "verify on the target host:  sha256sum -c SHA256SUMS"
