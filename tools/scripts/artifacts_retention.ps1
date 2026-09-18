#requires -Version 7
<#
.SYNOPSIS
    制品保留策略:只清理 <制品根>/snapshots/images 下的过期快照版本;releases/ 永不触碰。

.DESCRIPTION
    对标 A 仓 tools/scripts/artifacts_retention.ps1(去掉 UE 客户端包的流,B 只有镜像一条快照流)。

    规则:
      - 只看 snapshots/images 下名字形如 g<sha12>[-dirty-yyyyMMdd-HHmmss] 的目录;
        .tmp-* (发布中的 staging)、其它名字的目录(人工备份等)、符号链接一律跳过不删
      - 最后修改早于 24 小时的 .tmp-* 打印 WARN(仍不删):publish 被 Ctrl+C / CI 取消 / 强杀时会留下
        GB 级半截 staging,脚本分不清它与共享盘上别的机器正在进行的发布,只能提示人工确认
      - 按目录 LastWriteTimeUtc 从新到旧排序(同一时刻按目录名序数兜底,结果可复现),保留最近 -KeepLast 个
      - snapshots/images/latest.json 指向的版本无论多旧都保留:它是 fetch_images.ps1 的默认拉取目标,
        删掉它等于让所有不带 -Version 的目标机拉取失败;latest.json 存在但读不懂时拒绝清理(fail-closed)
      - releases/ 下的发布版本是不可变永久制品,本脚本不读也不写
      - 默认 dry-run 只打印计划,加 -Force 才真删
      - 删除先同目录 rename 成 .deleting-<名>-<guid>(原子下线),再递归删:直接删到一半失败
        (Windows 上 tar 正被共享读取 / 杀毒扫描)会留下名字合法、内容残缺的"版本"。
        单个版本下线或删除失败只记下来继续处理下一个,最后以非 0 退出汇总;
        上次没删完的 .deleting-* 残留在下一次 -Force 运行时清掉
      - 制品根必须已存在,不会被创建:计划任务没带上 MMORPG_ARTIFACT_ROOT 或共享盘没挂上时报错退出,
        而不是在错误位置建个空目录再"无快照需要清理"地假绿

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

    $snapRoot = Get-ChannelRoot -Channel 'snapshot' -Override $ArtifactRoot -MustExist
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

    # 上次中断留下的 .deleting-*:以 . 开头,上面的候选枚举一律跳过,不专门处理就永久占着磁盘
    $residues = @(Get-ChildItem -LiteralPath $imagesRoot -Directory -Force | Where-Object {
            $_.Name.StartsWith('.deleting-', [System.StringComparison]::Ordinal) -and
            ($_.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -eq 0
        })

    # 疑似遗留的 staging 只提示不删(见文件头):不提示的话排期清理一直报"无需清理"的绿,
    # 要等磁盘写满、发布失败才被发现。判据只看目录自身修改时间,所以措辞是"疑似"
    $staleStagingCutoff = [DateTime]::UtcNow.AddHours(-24)
    $staleStagings = @(Get-ChildItem -LiteralPath $imagesRoot -Directory -Force | Where-Object {
            $_.Name.StartsWith('.tmp-', [System.StringComparison]::Ordinal) -and
            ($_.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -eq 0 -and
            $_.LastWriteTimeUtc -lt $staleStagingCutoff
        })
    foreach ($d in $staleStagings) {
        $mtime = $d.LastWriteTimeUtc.ToString("yyyy-MM-dd'T'HH':'mm':'ss'Z'", [System.Globalization.CultureInfo]::InvariantCulture)
        Write-Host "[WARN] 疑似被中断发布遗留的 staging(最后修改 $mtime,不自动删除;确认没有发布在进行后手工删除):snapshots/images/$($d.Name)" -ForegroundColor Yellow
    }
    $stagingNote = if ($staleStagings.Count -gt 0) { ";另有 $($staleStagings.Count) 个疑似遗留 staging 待人工确认(见上方 WARN)" } else { '' }

    if ($toDelete.Count -eq 0 -and $residues.Count -eq 0) {
        Write-Host "[ OK ] 快照共 $($sorted.Count) 个,保留最近 $KeepLast 个,无需清理(releases/ 永不触碰)$stagingNote。" -ForegroundColor Green
        exit 0
    }

    $failures = New-Object System.Collections.Generic.List[string]
    foreach ($d in $residues) {
        $rel = "snapshots/images/$($d.Name)"
        if (-not $Force) {
            Write-Host "[DRY ] 将清理上次未删完的残留:$rel(加 -Force 执行)" -ForegroundColor Cyan
            continue
        }
        try {
            Remove-Item -LiteralPath $d.FullName -Recurse -Force
            Write-Host "[DEL ] 残留 $rel" -ForegroundColor Red
        }
        catch {
            $failures.Add("残留 $rel 删除失败:$($_.Exception.Message)")
            Write-Host "[WARN] 残留 $rel 删除失败,继续处理其它目录" -ForegroundColor Yellow
        }
    }

    foreach ($d in $toDelete) {
        $rel = "snapshots/images/$($d.Name)"
        if (-not $Force) {
            Write-Host "[DRY ] 将删除:$rel(加 -Force 执行)" -ForegroundColor Cyan
            continue
        }
        $trash = Join-Path $imagesRoot (".deleting-" + $d.Name + "-" + [guid]::NewGuid().ToString('N'))
        try {
            [System.IO.Directory]::Move($d.FullName, $trash)
        }
        catch {
            $failures.Add("$rel 下线失败(可能有文件正被占用),本次未删:$($_.Exception.Message)")
            Write-Host "[WARN] $rel 下线失败,跳过" -ForegroundColor Yellow
            continue
        }
        try {
            Remove-Item -LiteralPath $trash -Recurse -Force
            Write-Host "[DEL ] $rel" -ForegroundColor Red
        }
        catch {
            $failures.Add("$rel 已下线但删除未完成,下次 -Force 运行会继续清理 $(Split-Path -Leaf $trash):$($_.Exception.Message)")
            Write-Host "[WARN] $rel 已下线但删除未完成" -ForegroundColor Yellow
        }
    }

    if ($failures.Count -gt 0) {
        throw "快照清理未全部完成($($failures.Count) 项):`n$($failures -join "`n")"
    }
    $verb = if ($Force) { '已删除' } else { '待删除(dry-run,未改动任何文件)' }
    $residueNote = if ($residues.Count -gt 0) { "(另有上次未删完的残留 $($residues.Count) 个)" } else { '' }
    Write-Host "[ OK ] $($toDelete.Count) 个过期快照$verb$residueNote;保留最近 $KeepLast 个;releases/ 未触碰$stagingNote。" -ForegroundColor Green
    exit 0
}
catch {
    Write-Host "[ERR ] $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
