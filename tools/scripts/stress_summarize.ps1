#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Summarise a stress-run into a single 2-D table for fast AI / human review.

.DESCRIPTION
    Walks a `robot/logs/stress-<name>-<ts>/` directory + the corresponding
    `prom-snapshots/` and produces the compact view we used in the
    2026-05-27/28 stress series:

      [robot stats per minute]
          time | conn | login_ok | enter_ok | enter_fail | recon_fb | max_login | msg/s

      [entergame_total snapshot at each prom-snapshot point]
          when | success | preload_failed | apply_failed | fail %
                 | preload{ok} avg | preload{fail} avg | apply avg

      [dataloader per-stage avg at each prom-snapshot point]
          when | cache_check{hit} | sub_cache_check | kafka_send | callback_wait{ok} | callback_wait{fail}

      [CreatePlayer stages:每个快照点的建角分阶段均值 + 名字孤儿计数;缺指标打印 n/a,
       name_orphan 在「有 stage 序列但无孤儿序列」时打印 0(规则见该段注释)]
          when | mint | name | register | account_write | name_orphan

      [optional: Kafka consumer lag (final)]
          partition | current | log_end | lag

    All numbers are derived, no raw histogram buckets — exactly what the
    AI needs to understand a run in ≤2 KB output instead of 30+ KB of
    prom dumps.

    Goal is "stop spending tokens parsing prom output by hand". After
    every stress run, call this once. The text it prints is the only
    artefact AI needs to read.

.PARAMETER RunDir
    The robot/logs/stress-* directory. Auto-discovers prom-snapshots/
    inside it.

.PARAMETER ProbePromUrl
    If passed, additionally fetches LIVE prometheus from this URL right
    now and treats it as a synthetic 'live' snapshot (named `t<duration>m_live`).
    Useful when the run is still going.

.PARAMETER KafkaContainer
    Docker container name (default 'kafka'). Set to '' to skip Kafka lag
    section.

.PARAMETER KafkaTopic
    Topic to inspect for consumer lag. Default 'db_task_zone_1'.

.PARAMETER KafkaConsumerGroup
    Consumer group to inspect. Default 'db_rpc_consumer_group'.

.EXAMPLE
    # Standard post-run summary
    pwsh tools/scripts/stress_summarize.ps1 -RunDir robot/logs/stress-1zone-25k-mysqlpool-20260528-095929

.EXAMPLE
    # Mid-run probe + Kafka lag
    pwsh tools/scripts/stress_summarize.ps1 `
        -RunDir robot/logs/stress-1zone-25k-mysqlpool-20260528-095929 `
        -ProbePromUrl http://127.0.0.1:9101/metrics
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$RunDir,

    [string]$ProbePromUrl = "",

    [string]$KafkaContainer = "kafka",
    [string]$KafkaTopic = "db_task_zone_1",
    [string]$KafkaConsumerGroup = "db_rpc_consumer_group"
)

$ErrorActionPreference = "Stop"

# ----- helpers ---------------------------------------------------------

function Format-Number {
    param([double]$N, [int]$Width = 6)
    if ([double]::IsNaN($N) -or [double]::IsInfinity($N)) { return "-" }
    return "{0,$Width}" -f [math]::Round($N).ToString("N0")
}

function Format-Avg {
    param([double]$Sum, [double]$Count, [string]$Suffix = "ms")
    if ($Count -le 0) { return "-" }
    $avg = ($Sum / $Count) * 1000.0  # convert sec → ms
    if ($avg -ge 10000) { return "{0,7:N1}s" -f ($avg / 1000.0) }
    return "{0,5:N1}$Suffix" -f $avg
}

# Parse one prometheus snapshot file → hashtable of metric -> value.
# Histograms: keep the `_count`, `_sum` raw; let caller compute avg.
function Read-PromSnapshot {
    param([string]$Path)

    $m = @{}
    foreach ($line in [System.IO.File]::ReadLines($Path)) {
        if ($line.StartsWith("#")) { continue }
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        # metric_name{label1="...",label2="..."} value [timestamp]
        if ($line -match '^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+([0-9.eE+-]+)') {
            $name = $matches[1]
            $labels = if ($matches[2]) { $matches[2] } else { "" }
            $value = [double]$matches[3]
            $key = "$name$labels"
            $m[$key] = $value
        }
    }
    return $m
}

# Pull labelled value or 0 if missing.
function Get-Metric {
    param([hashtable]$M, [string]$Key)
    if ($M.ContainsKey($Key)) { return [double]$M[$Key] }
    return 0.0
}

# 按正则把一组序列求和(跨全部 label 组合)。一条都没匹配到返回 $null ——
# 调用方据此区分「指标不存在」(打印 n/a)与「存在但为 0」。Get-Metric 把两者
# 都折成 0,对后加的指标不够用:旧版本服务的快照里没有这些序列,显示成 0 会被
# 读成「测过了,是 0」。
function Get-MetricSumOrNull {
    param([hashtable]$M, [string]$Pattern)
    $sum = 0.0
    $found = $false
    foreach ($k in $M.Keys) {
        if ($k -match $Pattern) {
            $sum += [double]$M[$k]
            $found = $true
        }
    }
    if ($found) { return $sum }
    return $null
}

# ----- 1. robot stats ---------------------------------------------------

$RunDirResolved = (Resolve-Path $RunDir).Path
Write-Host ""
Write-Host "Run directory: $RunDirResolved" -ForegroundColor Cyan

$stderrPath = Join-Path $RunDirResolved "robot.stderr"
if (-not (Test-Path $stderrPath)) {
    throw "robot.stderr not found in $RunDirResolved"
}

Write-Host ""
Write-Host "=== Robot per-minute stats =============================================" -ForegroundColor Yellow
Write-Host ""
"  time   conn        login_ok   enter_ok   enter_fail  recon_fb  max_login  msg/s" | Write-Host
"  ----   ----        --------   --------   ----------  --------  ---------  -----" | Write-Host

# Each line we want has form:
#   [stats 5m0s] conn=25184 login_ok=25119 ... enter_ok=25119 enter_fail=0 ...
#   recon_fb=0 ... max_login=151ms ... msg_sent=...(199/s)
$statsPattern = '\[stats (?<t>\d+m0s)\] conn=(?<conn>\d+) login_ok=(?<login_ok>\d+) login_fail=\d+ login_stuck=\d+ enter_ok=(?<enter_ok>\d+) enter_fail=(?<enter_fail>\d+) msg_sent=\d+\((?<mps>\d+)/s\) .*?max_login=(?<max_login>\d+)ms .*?recon_fb=(?<recon_fb>\d+)'

$lastStat = $null
$rowCount = 0
foreach ($line in [System.IO.File]::ReadLines($stderrPath)) {
    if ($line -match $statsPattern) {
        $r = $matches
        $line = "  {0,-6} {1,-11} {2,-10} {3,-10} {4,-11} {5,-9} {6,-10} {7}" -f `
            $r['t'], `
            (Format-Number $r['conn'] 11), `
            (Format-Number $r['login_ok']  10), `
            (Format-Number $r['enter_ok'] 10), `
            (Format-Number $r['enter_fail'] 11), `
            (Format-Number $r['recon_fb'] 9), `
            ($r['max_login'] + "ms"), `
            ($r['mps'] + "/s")
        Write-Host $line
        $lastStat = $r
        $rowCount++
    }
}
if ($rowCount -eq 0) {
    Write-Host "  (no [stats Xm0s] markers found — was the run shorter than 1 minute?)" -ForegroundColor Red
}

# ----- 2. snapshot summaries -------------------------------------------

$snapDir = Join-Path $RunDirResolved "prom-snapshots"
$snapshots = @()
$smSnapshots = @()
$dbSnapshots = @()

if (Test-Path $snapDir) {
    $allTxt = Get-ChildItem $snapDir -Filter "*.txt" | Sort-Object Name
    # New multi-endpoint layout (Round 16+) uses suffixes: t{N}m_login.txt,
    # t{N}m_sm.txt, t{N}m_db.txt. Old single-endpoint layout used bare
    # t{N}m.txt and only ever contained login metrics.
    $loginTxt = $allTxt | Where-Object { $_.BaseName -match '_login$' }
    $smTxt    = $allTxt | Where-Object { $_.BaseName -match '_sm$' }
    $dbTxt    = $allTxt | Where-Object { $_.BaseName -match '_db$' }
    if ($loginTxt.Count -gt 0) {
        $snapshots   += $loginTxt
        $smSnapshots += $smTxt
        $dbSnapshots += $dbTxt
    } else {
        # Legacy: bare t{N}m.txt files all hold login metrics.
        $snapshots += $allTxt
    }
}

if ($ProbePromUrl) {
    Write-Host ""
    Write-Host "Probing live prometheus: $ProbePromUrl" -ForegroundColor DarkCyan
    $tmp = New-TemporaryFile
    try {
        Invoke-WebRequest -Uri $ProbePromUrl -OutFile $tmp -TimeoutSec 5 -UseBasicParsing
        $snapshots += Get-Item $tmp
    } catch {
        Write-Host "  live probe failed: $($_.Exception.Message)" -ForegroundColor Red
    }
}

if ($snapshots.Count -eq 0) {
    Write-Host ""
    Write-Host "(no prom-snapshots/ directory and no -ProbePromUrl; skipping prom analysis)" -ForegroundColor DarkGray
} else {

    Write-Host ""
    Write-Host "=== EnterGame end-to-end (entergame_*) =================================" -ForegroundColor Yellow
    Write-Host ""
    "  snapshot                       success  preload_failed  apply_failed  fail%   preload{ok}avg  preload{fail}avg  apply avg" | Write-Host
    "  --------                       -------  --------------  ------------  -----   --------------  ----------------  ---------" | Write-Host

    foreach ($snap in $snapshots) {
        $m = Read-PromSnapshot $snap.FullName
        $success      = Get-Metric $m 'entergame_total{result="success"}'
        $preloadFail  = Get-Metric $m 'entergame_total{result="preload_failed"}'
        $applyFail    = Get-Metric $m 'entergame_total{result="apply_failed"}'
        $lockLost     = Get-Metric $m 'entergame_total{result="lock_lost"}'
        $total = $success + $preloadFail + $applyFail + $lockLost
        $failPct = if ($total -gt 0) { [math]::Round(($preloadFail + $applyFail + $lockLost) / $total * 100, 1) } else { 0.0 }

        $preloadOkAvg   = Format-Avg (Get-Metric $m 'entergame_preload_seconds_sum{result="success"}')        (Get-Metric $m 'entergame_preload_seconds_count{result="success"}')
        $preloadFailAvg = Format-Avg (Get-Metric $m 'entergame_preload_seconds_sum{result="preload_failed"}') (Get-Metric $m 'entergame_preload_seconds_count{result="preload_failed"}')
        $applyAvg       = Format-Avg (Get-Metric $m 'entergame_apply_seconds_sum{result="success"}')          (Get-Metric $m 'entergame_apply_seconds_count{result="success"}')

        $name = $snap.BaseName
        $line = "  {0,-30} {1,-8} {2,-15} {3,-13} {4,-7} {5,-15} {6,-17} {7}" -f `
            $name, `
            (Format-Number $success 8), `
            (Format-Number $preloadFail 15), `
            (Format-Number $applyFail 13), `
            ("{0:N1}%" -f $failPct), `
            $preloadOkAvg, `
            $preloadFailAvg, `
            $applyAvg
        Write-Host $line
    }

    Write-Host ""
    Write-Host "=== Dataloader per-stage avg (dataloader_preload_*) ====================" -ForegroundColor Yellow
    Write-Host ""
    "  snapshot                       cache_check  sub_cache  dispatcher  kafka_send  cb_wait{ok}  cb_wait{fail}" | Write-Host
    "  --------                       -----------  ---------  ----------  ----------  -----------  -------------" | Write-Host

    foreach ($snap in $snapshots) {
        $m = Read-PromSnapshot $snap.FullName

        # cache_check has hit/miss labels; combine for an overall avg.
        $ccSumHit    = Get-Metric $m 'dataloader_preload_cache_check_seconds_sum{hit="hit"}'
        $ccCountHit  = Get-Metric $m 'dataloader_preload_cache_check_seconds_count{hit="hit"}'
        $ccSumMiss   = Get-Metric $m 'dataloader_preload_cache_check_seconds_sum{hit="miss"}'
        $ccCountMiss = Get-Metric $m 'dataloader_preload_cache_check_seconds_count{hit="miss"}'
        $cacheAvg = Format-Avg ($ccSumHit + $ccSumMiss) ($ccCountHit + $ccCountMiss)

        # sub_cache_check has labels by `subs=N`; we collapse by walking all keys.
        $subSum = 0.0; $subCount = 0.0
        foreach ($k in $m.Keys) {
            if ($k -match '^dataloader_preload_sub_cache_check_seconds_sum\{subs="') { $subSum += $m[$k] }
            elseif ($k -match '^dataloader_preload_sub_cache_check_seconds_count\{subs="') { $subCount += $m[$k] }
        }
        $subAvg = Format-Avg $subSum $subCount

        $dispatcherAvg = Format-Avg `
            (Get-Metric $m 'dataloader_preload_dispatcher_register_seconds_sum') `
            (Get-Metric $m 'dataloader_preload_dispatcher_register_seconds_count')

        $kafkaAvg = Format-Avg `
            (Get-Metric $m 'dataloader_preload_kafka_send_seconds_sum{result="success"}') `
            (Get-Metric $m 'dataloader_preload_kafka_send_seconds_count{result="success"}')

        $cbOkAvg   = Format-Avg `
            (Get-Metric $m 'dataloader_preload_callback_wait_seconds_sum{result="success"}') `
            (Get-Metric $m 'dataloader_preload_callback_wait_seconds_count{result="success"}')
        $cbFailAvg = Format-Avg `
            (Get-Metric $m 'dataloader_preload_callback_wait_seconds_sum{result="failed"}') `
            (Get-Metric $m 'dataloader_preload_callback_wait_seconds_count{result="failed"}')

        $name = $snap.BaseName
        $line = "  {0,-30} {1,-12} {2,-10} {3,-11} {4,-11} {5,-12} {6}" -f `
            $name, $cacheAvg, $subAvg, $dispatcherAvg, $kafkaAvg, $cbOkAvg, $cbFailAvg
        Write-Host $line
    }

    # 建角分阶段耗时(帮会二期 B3a-2,docs/design/guild-phase2/03-names.md §3.11 / §3.20)。
    # 指标在 login(:9101)上,所以与上面两段共用 login 快照。stage 的四个取值由
    # 设计 §3.11 钉死,落码在 go/login/internal/logic/clientplayerlogin/metrics.go
    # —— 那边改名这里必须跟着改,否则整列静默变成 n/a:
    #   mint = 发 player_id,name = 名字占用(ReservePlayerName,含重试),
    #   register = RegisterPlayerZone,account_write = 围栏写账号 blob。
    # 验收口径(§3.20)看 name 段均值与 name_orphan(建角失败后名字仍被占住的次数,应为 0)。
    # 各 stage 列缺指标一律打印 n/a,不折成 0:B3a-2 之前的 login 没有这些序列;histogram 的
    # 某个 stage 在第一次观测前也不会出现 —— 两种都不是「测到 0」。
    #
    # name_orphan 列:正常情况下直接读序列。login 起服时由 PrimeCreatePlayerMetrics
    # (metrics.go;login.go startServer 在 zrpc.MustNewServer 之后调用)把该计数器预置成 0,
    # 所以新版 login 且采集已开的快照里这条序列恒存在,健康压测读到的就是 0。
    #
    # 下面的 $stageSeen 分支只是兜底,覆盖「预置被移除 / 调用时机被挪到 MustNewServer 之前
    # 而被 prometheus.Enabled() 开关丢弃」的情形:无 label 的 CounterVec 在第一次写入前
    # 连 HELP/TYPE 都不输出,那时照抄上面的规则会让验收口径「orphan=0」永远读成 n/a。
    # 规则:序列缺失、但同一快照里存在任一 login_create_player_stage_seconds_count 序列
    # → 打印 0;两者都缺 → 打印 n/a。依据(都在 login 的 metrics.go / createplayerlogic.go):
    #   1. 直方图与该计数器在同一个 var 块注册、同受 prometheus.Enabled() 开关控制,
    #      直方图有序列即证明「新版 login + 采集已开」;
    #   2. 每条孤儿路径都在 mint 段观测之后才可达,不存在「出过孤儿却没有直方图序列」。
    # 该例外已记入设计 03-names.md §3.18 的 B3a-2 工具清单(stress_summarize.ps1 那一条)。
    Write-Host ""
    Write-Host "=== CreatePlayer stages (login_create_player_stage_seconds) ============" -ForegroundColor Yellow
    Write-Host ""
    "  snapshot                       mint       name       register   account_write  name_orphan" | Write-Host
    "  --------                       ----       ----       --------   -------------  -----------" | Write-Host

    $createStages = @('mint','name','register','account_write')
    foreach ($snap in $snapshots) {
        $m = Read-PromSnapshot $snap.FullName
        $cells = @()
        foreach ($st in $createStages) {
            $sumV = Get-MetricSumOrNull $m "^login_create_player_stage_seconds_sum\{.*stage=`"$st`""
            $cntV = Get-MetricSumOrNull $m "^login_create_player_stage_seconds_count\{.*stage=`"$st`""
            $cell = if ($null -eq $sumV -or $null -eq $cntV) { "n/a" } else { Format-Avg $sumV $cntV }
            $cells += $cell
        }

        $orphanV = Get-MetricSumOrNull $m '^login_create_player_name_orphan_total(\{|$)'
        $stageSeen = $null -ne (Get-MetricSumOrNull $m '^login_create_player_stage_seconds_count\{')
        $orphanCell = if ($null -ne $orphanV) { "{0:N0}" -f $orphanV } elseif ($stageSeen) { "0" } else { "n/a" }

        $name = $snap.BaseName
        $line = "  {0,-30} {1,-10} {2,-10} {3,-10} {4,-14} {5}" -f `
            $name, $cells[0], $cells[1], $cells[2], $cells[3], $orphanCell
        Write-Host $line
    }
}

# ----- 2b. SceneManager EnterScene stage breakdown (Round 16+) ----------

if ($smSnapshots.Count -gt 0) {
    Write-Host ""
    Write-Host "=== SceneManager EnterScene stages (scene_manager_enter_scene_stage_seconds) ===" -ForegroundColor Yellow
    Write-Host ""
    "  snapshot                       dedup     scene_res reserve  upd_loc  route_gt rel_disp cross_zn" | Write-Host
    "  --------                       --------- --------- -------- -------- -------- -------- --------" | Write-Host

    $stages = @('dedup','scene_resolve','reserve','update_loc','route_gate','release_dispatch','cross_zone')
    foreach ($snap in $smSnapshots) {
        $m = Read-PromSnapshot $snap.FullName
        $cells = @()
        foreach ($st in $stages) {
            # Sum across all zone_id labels.
            $sumK = "scene_manager_enter_scene_stage_seconds_sum"
            $cntK = "scene_manager_enter_scene_stage_seconds_count"
            $sumV = 0.0; $cntV = 0.0
            foreach ($k in $m.Keys) {
                if ($k -match "^$sumK\{.*stage=`"$st`"") { $sumV += [double]$m[$k] }
                if ($k -match "^$cntK\{.*stage=`"$st`"") { $cntV += [double]$m[$k] }
            }
            $cells += (Format-Avg $sumV $cntV)
        }
        $name = $snap.BaseName
        $line = "  {0,-30} {1,-9} {2,-9} {3,-8} {4,-8} {5,-8} {6,-8} {7}" -f `
            $name, $cells[0], $cells[1], $cells[2], $cells[3], $cells[4], $cells[5], $cells[6]
        Write-Host $line
    }
}

# ----- 2bb. Dirty-save skip rate (cpp scene log, parsed not scraped) ----
#
# Why log-parsing instead of a Prometheus scrape: cpp scene processes don't
# expose a metrics HTTP endpoint today (only go SceneManager :9150 and go
# db :9160 do). The two atomic counters live in dirty_save_stats.h and emit
# one LOG_INFO line every 30 s from the RedisSystem snapshot timer. We pull
# the LATEST line from the most recent scene_*.log and surface the
# total / skipped / skip_pct triple.
#
# Stays silent when no [DirtySave] line is found — keeps legacy stress
# runs (pre-dirty-save-metric) clean and avoids a noisy "0 saves" line in
# very short smoke tests.

# Resolve repo root once from the script's own location ($PSScriptRoot is
# tools/scripts/, so two levels up is the repo root) — stress_summarize.ps1
# doesn't otherwise need it, but the dirty-save log scan below does.
$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot "..\..")
$cppLogDir = Join-Path $RepoRoot "run\logs\cpp_nodes"

# Only scan scene*.log files (gate logs don't run SavePlayerToRedis).
# The glob deliberately omits the underscore so it matches both legacy
# single-instance "scene.stdout.log" and multi-instance "scene_1.stdout.log".
$sceneLogs = @()
if (Test-Path $cppLogDir) {
    $sceneLogs = Get-ChildItem -Path $cppLogDir -Filter 'scene*.log' `
                 -ErrorAction SilentlyContinue | Sort-Object LastWriteTime -Descending
}

$dirtySaveLines = @()
foreach ($log in $sceneLogs) {
    # Tail-style scan: read the file once and keep all matches; counters
    # are monotonic so the LAST match per file is the cumulative total.
    #
    # Use FileShare.ReadWrite — the scene process holds an open write
    # handle on its own stdout log while it's running, and the default
    # File::ReadLines opens with FileShare.None which would throw
    # "being used by another process". Round 17 (2026-06-03) hit this
    # the first time we ran the parser against a still-running scene.
    $fs = $null
    $sr = $null
    try {
        $fs = [System.IO.FileStream]::new(
            $log.FullName,
            [System.IO.FileMode]::Open,
            [System.IO.FileAccess]::Read,
            [System.IO.FileShare]::ReadWrite)
        $sr = [System.IO.StreamReader]::new($fs)
        while (-not $sr.EndOfStream) {
            $line = $sr.ReadLine()
            if ($line -match '\[DirtySave\] total=(?<total>\d+) skipped=(?<skipped>\d+) skip_pct=(?<pct>[0-9.]+)%') {
                $dirtySaveLines += [pscustomobject]@{
                    Log     = $log.Name
                    Total   = [int64]$matches['total']
                    Skipped = [int64]$matches['skipped']
                    Pct     = [double]$matches['pct']
                }
            }
        }
    } finally {
        if ($sr) { $sr.Dispose() }
        if ($fs) { $fs.Dispose() }
    }
}

if ($dirtySaveLines.Count -gt 0) {
    Write-Host ""
    Write-Host "=== Dirty-save skip rate (cpp scene LOG_INFO [DirtySave]) ============" -ForegroundColor Yellow
    Write-Host ""
    "  scene_log                      total       skipped     skip_pct" | Write-Host
    "  ---------                      -----       -------     --------" | Write-Host

    # Group by log file and take the last (== highest, monotonic) sample
    # per scene instance. With one scene_1.log this is just one row; with
    # multi-scene runs each instance gets its own row.
    $byLog = $dirtySaveLines | Group-Object -Property Log
    foreach ($g in $byLog) {
        $latest = $g.Group | Select-Object -Last 1
        $line = "  {0,-30} {1,-11} {2,-11} {3,5:N1}%" -f `
            $g.Name, `
            (Format-Number $latest.Total 11), `
            (Format-Number $latest.Skipped 11), `
            $latest.Pct
        Write-Host $line
    }
}

# ----- 2c. DB per-task stage breakdown (Round 16+) ----------------------

if ($dbSnapshots.Count -gt 0) {
    Write-Host ""
    Write-Host "=== DB task stages (db_task_stage_seconds, db_task_result_total) ===" -ForegroundColor Yellow
    Write-Host ""
    "  snapshot                       read_total read_hdlr cache_wr publish  | write_total write_hdlr | ok      err" | Write-Host
    "  --------                       ---------- --------- -------- -------- | ----------- ---------- | ------- ----" | Write-Host

    foreach ($snap in $dbSnapshots) {
        $m = Read-PromSnapshot $snap.FullName

        $readTotalAvg = Format-Avg `
            (Get-Metric $m 'db_task_stage_seconds_sum{stage="op_total",op="read"}') `
            (Get-Metric $m 'db_task_stage_seconds_count{stage="op_total",op="read"}')
        $readHdlrAvg = Format-Avg `
            (Get-Metric $m 'db_task_stage_seconds_sum{stage="op_handler",op="read"}') `
            (Get-Metric $m 'db_task_stage_seconds_count{stage="op_handler",op="read"}')
        $readCacheAvg = Format-Avg `
            (Get-Metric $m 'db_task_stage_seconds_sum{stage="cache_write",op="read"}') `
            (Get-Metric $m 'db_task_stage_seconds_count{stage="cache_write",op="read"}')
        $publishAvg = Format-Avg `
            (Get-Metric $m 'db_task_stage_seconds_sum{stage="result_publish",op="read"}') `
            (Get-Metric $m 'db_task_stage_seconds_count{stage="result_publish",op="read"}')

        $writeTotalAvg = Format-Avg `
            (Get-Metric $m 'db_task_stage_seconds_sum{stage="op_total",op="write"}') `
            (Get-Metric $m 'db_task_stage_seconds_count{stage="op_total",op="write"}')
        $writeHdlrAvg = Format-Avg `
            (Get-Metric $m 'db_task_stage_seconds_sum{stage="op_handler",op="write"}') `
            (Get-Metric $m 'db_task_stage_seconds_count{stage="op_handler",op="write"}')

        $okCnt  = (Get-Metric $m 'db_task_result_total{op="read",result="ok"}') + (Get-Metric $m 'db_task_result_total{op="write",result="ok"}')
        $errCnt = (Get-Metric $m 'db_task_result_total{op="read",result="error"}') + (Get-Metric $m 'db_task_result_total{op="write",result="error"}')

        $name = $snap.BaseName
        $line = "  {0,-30} {1,-10} {2,-9} {3,-8} {4,-8} | {5,-11} {6,-10} | {7,-7} {8}" -f `
            $name, $readTotalAvg, $readHdlrAvg, $readCacheAvg, $publishAvg, $writeTotalAvg, $writeHdlrAvg, `
            (Format-Number $okCnt 7), (Format-Number $errCnt 4)
        Write-Host $line
    }
}

# ----- 3. Kafka lag -----------------------------------------------------

if ($KafkaContainer) {
    Write-Host ""
    Write-Host "=== Kafka consumer lag (live) ==========================================" -ForegroundColor Yellow
    Write-Host ""

    $cmd = "/opt/kafka/bin/kafka-consumer-groups.sh"
    $args = @(
        "exec", $KafkaContainer, $cmd,
        "--bootstrap-server", "localhost:9092",
        "--describe", "--group", $KafkaConsumerGroup
    )

    $raw = ""
    try {
        $raw = & docker @args 2>&1 | Out-String
    } catch {
        Write-Host "  docker exec failed: $($_.Exception.Message)" -ForegroundColor Red
        Write-Host ""
        return
    }

    $totalLag = 0
    $partRows = @()
    foreach ($line in ($raw -split "`r?`n")) {
        # GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG ...
        if ($line -match "^\S+\s+$KafkaTopic\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)") {
            $p = [int]$matches[1]
            $cur = [int]$matches[2]
            $end = [int]$matches[3]
            $lag = [int]$matches[4]
            $totalRows += 1
            $totalLag += $lag
            $partRows += [pscustomobject]@{ Part = $p; Cur = $cur; End = $end; Lag = $lag }
        }
    }

    if ($partRows.Count -eq 0) {
        Write-Host "  (no lag rows parsed — group $KafkaConsumerGroup empty? or container=$KafkaContainer down?)" -ForegroundColor DarkGray
    } else {
        "  partition    current      log_end       lag" | Write-Host
        "  ---------    -------      -------       ---" | Write-Host
        foreach ($r in ($partRows | Sort-Object Part)) {
            $line = "  {0,-12} {1,-12} {2,-13} {3}" -f $r.Part, $r.Cur.ToString("N0"), $r.End.ToString("N0"), $r.Lag.ToString("N0")
            Write-Host $line
        }
        Write-Host ""
        Write-Host ("  TOTAL LAG: {0}" -f $totalLag.ToString("N0")) -ForegroundColor Yellow
    }
}

Write-Host ""
