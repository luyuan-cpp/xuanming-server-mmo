#requires -Version 7
<#
.SYNOPSIS
    制品保留策略:只清理 <制品根>/snapshots/images 下的过期快照版本;releases/ 永不触碰。

.DESCRIPTION
    对标 A 仓 tools/scripts/artifacts_retention.ps1(去掉 UE 客户端包的流,B 只有镜像一条快照流)。

    规则:
      - 只看 snapshots/images 下名字形如 g<sha12>[-dirty-yyyyMMdd-HHmmss] 的目录;
        .tmp-* (发布中的 staging)、其它名字的目录(人工备份等)、符号链接一律跳过不删
      - 按目录 LastWriteTimeUtc 从新到旧排序(同一时刻按目录名序数兜底,结果可复现),保留最近 -KeepLast 个
      - snapshots/images/latest.json 指向的版本无论多旧都保留:它是 fetch_images.ps1 的默认拉取目标,
        删掉它等于让所有不带 -Version 的目标机拉取失败;latest.json 存在但读不懂时拒绝清理(fail-closed)
      - releases/ 下的发布版本是不可变永久制品,本脚本不读也不写
      - 默认 dry-run 只打印计划,加 -Force 才真删

    建议构建机排期周跑(Windows 计划任务 / cron)。

    退出码:0 = 成功(含无需清理);1 = 失败(错误文本以 [ERR ] 开头)。

.EXAMPLE
    pwsh -File tools/scripts/artifacts_retention.ps1                  # 预览
    pwsh -File tools/scripts/artifacts_retention.ps1 -KeepLast 5 -Force
#>
[CmdletBinding()]
param(
    # 保留最近的快照版本数,至少 1
    [int]$KeepLast = 10,

    # 真删;缺省 dry-run
    [switch]$Force,

    [string]$ArtifactRoot = ''
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'lib' 'artifacts_lib.ps1')

try {
    if ($KeepLast -lt 1) { throw "-KeepLast 至少为 1(实际 $KeepLast):快照全删会让 fetch_images.ps1 无版本可拉。" }

    $snapRoot = Get-ChannelRoot -Channel 'snapshot' -Override $ArtifactRoot
    $imagesRoot = Join-Path $snapRoot 'images'
    if (-not [System.IO.Directory]::Exists($imagesRoot)) {
        Write-Host "[ OK ] $imagesRoot 不存在,无快照需要清理(releases/ 永不触碰)。" -ForegroundColor Green
        exit 0
    }

    $protectedVersion = ''
    $latestPath = Join-Path $imagesRoot 'latest.json'
    if ([System.IO.File]::Exists($latestPath)) {
        try {
            $protectedVersion = [string](Get-Content -LiteralPath $latestPath -Raw | ConvertFrom-Json).version
        }
        catch {
            throw "latest.json 无法解析,为免误删 latest 指向的版本拒绝清理:$latestPath($($_.Exception.Message))"
        }
        if ([string]::IsNullOrWhiteSpace($protectedVersion)) {
            throw "latest.json 缺少 version 字段,为免误删拒绝清理:$latestPath"
        }
    }

    $candidates = New-Object System.Collections.Generic.List[System.IO.DirectoryInfo]
    foreach ($d in Get-ChildItem -LiteralPath $imagesRoot -Directory -Force) {
        if ($d.Name.StartsWith('.')) { continue }   # .tmp-* staging 等
        if (($d.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            Write-Host "[SKIP] 符号链接/联接点不处理:$($d.Name)" -ForegroundColor Yellow
            continue
        }
        if (-not (Test-SnapshotVersionName -Version $d.Name)) {
            Write-Host "[SKIP] 不是快照版本目录名,不处理:$($d.Name)" -ForegroundColor Yellow
            continue
        }
        $candidates.Add($d)
    }

    $sorted = @($candidates |
        Sort-Object -Property @{ Expression = { $_.LastWriteTimeUtc }; Descending = $true },
                              @{ Expression = { [string]$_.Name }; Descending = $false })
    # Sort-Object 的字符串次序依赖区域设置;名字只含 [0-9a-z-],各区域结果一致,够做兜底

    $toDelete = New-Object System.Collections.Generic.List[System.IO.DirectoryInfo]
    for ($i = $KeepLast; $i -lt $sorted.Count; $i++) {
        $d = $sorted[$i]
        if ($protectedVersion -and $d.Name -ceq $protectedVersion) {
            Write-Host "[KEEP] $($d.Name) 是 latest.json 指向的版本,保留" -ForegroundColor Cyan
            continue
        }
        $toDelete.Add($d)
    }

    if ($toDelete.Count -eq 0) {
        Write-Host "[ OK ] 快照共 $($sorted.Count) 个,保留最近 $KeepLast 个,无需清理(releases/ 永不触碰)。" -ForegroundColor Green
        exit 0
    }

    foreach ($d in $toDelete) {
        $rel = "snapshots/images/$($d.Name)"
        if ($Force) {
            Remove-Item -LiteralPath $d.FullName -Recurse -Force
            Write-Host "[DEL ] $rel" -ForegroundColor Red
        } else {
            Write-Host "[DRY ] 将删除:$rel(加 -Force 执行)" -ForegroundColor Cyan
        }
    }
    $verb = if ($Force) { '已删除' } else { '待删除(dry-run,未改动任何文件)' }
    Write-Host "[ OK ] $($toDelete.Count) 个过期快照$verb;保留最近 $KeepLast 个;releases/ 未触碰。" -ForegroundColor Green
    exit 0
}
catch {
    Write-Host "[ERR ] $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
