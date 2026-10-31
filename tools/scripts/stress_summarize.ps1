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
          when | success | apply_failed | lock_lost | fail % | total{ok} avg | apply avg | pool_saturated
          (pool_saturated = PreloadPool 满、入场链没启动的次数;不计入 fail %;缺序列打印 n/a)

      [CreatePlayer stages:每个快照点的建角分阶段均值 + 名字孤儿计数;缺指标打印 n/a,
       name_orphan 在「有 stage 序列但无孤儿序列」时打印 0(规则见该段注释)]
          when | mint | name | register | account_write | name_orphan

      [L1 Login outbound RPC(login :9101 的 go-zero rpc_client_requests_*,2026-10-23 起):登录链路调
       data_service 的 5 个方法各自的次数 / 累计均值 / p99(桶插值)/ 非 OK 码数;缺序列打印 n/a]
          when | bgz_n bgz_avg bgz_p99 bgz_err | bpn_* | ghz_* | rad_* | lar_*

      [L2 Login gate directory reads(login_gate_directory_reads_total):gate 目录读镜像还是回落直读 etcd]
          when | cache etcd_unsynced etcd_stale | cache%

      [S1 SceneManager home_zone / role_lookup(*_sm):归属查询结果分布 + 两个新 stage 的 n / 均值 / p99]
          when | mapped unmapped unconfig error | hz_n hz_avg hz_p99 | rl_n rl_avg rl_p99

      [D1 data_service home-zone reads(*_ds,:9260):GetPlayerHomeZone / BatchGetPlayerHomeZone 服务端耗时与结果、
       每批 id 数、读方门禁回源计数]
          when | h_ok h_nf h_err h_avg h_p99 | b_ok b_nf b_err b_avg b_p99 | ids/b | rr_cache rr_sql rr_sqlerr

      [D2 data_service account directory RPC(*_ds):每个快照 × 每个账号目录 RPC 一行]
          when | rpc | n avg p99 | nf rej err

      [P1 Player store save(*_db 快照,:9160):db_player_store_seconds{op="save"} 的 n / 均值 / p99,
       db_player_store_requests_total 每个结局的次数;本 op 没有序列打印 n/a,有序列时缺的结局打 0]
          when | n avg p99 | committed equal | stale not_hold unclaimed older | no_owner invalid regressed too_new too_large | unknown unavail denied

      [P2 Player store claim(同上,op="claim")]
          when | n avg p99 | claimed | stale not_hold no_owner invalid too_new | unknown unavail denied

      [P3 Player store row bytes(db_player_store_row_bytes,已提交的行):每组 n / 均值 / p99,单位字节]
          when | hot_* | asset_* | growth_* | profile_* | legacy1_*

      [P4 DB pools and placement(db_redis_pool_*、mmorpg_sql_pool_*、db_placement_*、db_store_cooldown_stores)]
          when | rpool_wait rpool_tmo | sql_in_use sql_wait sql_wait_s | open_stores open_err cooldown

      [R1 Redis instance load(stress_snap.ps1 抓的 t<N>m_redis_<name>.txt,INFO 相邻两份求差 + 首末整体窗口)]
          target | window | secs ops/s cpu% main% in_KB/s out_KB/s | clients used_MB mem% | rej_conn err_rep oom aof_dfs | note

      [R2 Redis top commands(commandstats 求差 + latencystats 累计百分位)]
          target | window | cmd | calls/s usec/call | p50 p99 p99.9 | failed rejected

      [M1 Mapping read path at a glance:按 t<N>m 把 login / ds / sm / db / redis 的关键均值连成一行]
          tag | l_bgz l_rad | ds_batch ds_home ds_dir | sm_hz sm_rl | db_claim db_save | rd_ops/s rd_main%

    新表在缺文件时打印标题加一行「(无数据 no data: …)」,缺序列打印 n/a,退出码不变。

      [optional: Kafka consumer lag (live,data_service 的两条审计落库消费组)]
          group | topic | partition | current | log_end | lag

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

.PARAMETER KafkaConsumerGroups
    Consumer groups to inspect for lag. Default: data_service 的两条审计落库消费组
    (go/data_service/etc/data_service.yaml 的 Kafka.TransactionLogConsumerGroup / SnapshotConsumerGroup)。
    玩家存盘不经 Kafka,go/db 没有消费组;控制面命令 topic 是 assign 分区、不进消费组,这里看不到。

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
    [string[]]$KafkaConsumerGroups = @("data_service-transaction-log", "data_service-player-snapshot")
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

# 下面几个函数放在脚本前部:PowerShell 脚本里的函数不提升,后面各段都要调用它们。

# 秒 → 毫秒文本;≥10s 改用秒(同 Format-Avg)。
function Format-AvgCell {
    param($AvgSeconds, [int]$Decimals = 1)
    if ($null -eq $AvgSeconds) { return "n/a" }
    if ([double]::IsNaN([double]$AvgSeconds)) { return "-" }
    $ms = [double]$AvgSeconds * 1000.0
    if ($ms -ge 10000) { return "{0:N1}s" -f ($ms / 1000.0) }
    return "{0:N$Decimals}ms" -f $ms
}

# 计数文本;$null(缺序列)→ n/a。
function Format-CountCell {
    param($Value)
    if ($null -eq $Value) { return "n/a" }
    return "{0:N0}" -f [double]$Value
}

# 百分比文本;分子或分母缺序列 → n/a,分母为 0 → "-"(同 Format-Avg 的「没有样本」)。
function Format-RatioCell {
    param($Numerator, $Denominator)
    if ($null -eq $Numerator -or $null -eq $Denominator) { return "n/a" }
    if ([double]$Denominator -le 0) { return "-" }
    return "{0:N1}%" -f (100.0 * [double]$Numerator / [double]$Denominator)
}

# 小数文本;$null → n/a,NaN → "-"(没有样本 / 分母为 0)。
function Format-DecimalCell {
    param($Value, [int]$Decimals = 1, [string]$Suffix = "")
    if ($null -eq $Value) { return "n/a" }
    if ([double]::IsNaN([double]$Value)) { return "-" }
    return ("{0:N$Decimals}" -f [double]$Value) + $Suffix
}

# 几个可能为 $null 的数相加;任一为 $null 结果就是 $null(→ n/a),不把缺序列折成 0。
# $Values 不标类型:传数组字面量或字典的 .Values 都行,含 $null 的元素逐个遍历到。
function Get-SumOrNull {
    param($Values)
    $sum = 0.0
    foreach ($v in $Values) {
        if ($null -eq $v) { return $null }
        $sum += [double]$v
    }
    return $sum
}

# ----- 按 label 过滤的序列读取(2026-10-23,L-18 观测批的新表用) ------------
#
# 按 label 名取值,与 /metrics 输出时的 label 顺序无关,没列出的 label(scene_manager 的 zone_id、histogram 的 le 以外)跨值求和。

# 拆出一条序列 key("name{a=\"x\",b=\"y\"}" 或 "name")的 label 字典;不属于 $Name 这一族返回 $null。
# name_sum / name_count / name_bucket 各算一族。
function Get-SeriesLabels {
    param([string]$Key, [string]$Name)
    if ($Key -ceq $Name) { return @{} }
    if (-not $Key.StartsWith($Name + '{', [System.StringComparison]::Ordinal)) { return $null }
    $labels = @{}
    foreach ($pair in [regex]::Matches($Key.Substring($Name.Length), '([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"')) {
        $labels[$pair.Groups[1].Value] = $pair.Groups[2].Value
    }
    return $labels
}

function Test-LabelsMatch {
    param([hashtable]$Labels, [hashtable]$Match)
    foreach ($k in $Match.Keys) {
        if (-not $Labels.ContainsKey($k) -or $Labels[$k] -cne [string]$Match[$k]) { return $false }
    }
    return $true
}

# 一族序列按 $Match 过滤后求和。一条都没匹配到返回 $null(→ n/a),区分「指标不存在」与「存在但为 0」。
function Get-SeriesSumOrNull {
    param([hashtable]$M, [string]$Name, [hashtable]$Match = @{})
    $sum = 0.0
    $found = $false
    foreach ($k in $M.Keys) {
        $labels = Get-SeriesLabels -Key $k -Name $Name
        if ($null -eq $labels -or -not (Test-LabelsMatch $labels $Match)) { continue }
        $sum += [double]$M[$k]
        $found = $true
    }
    if ($found) { return $sum }
    return $null
}

# 一族 histogram 按 $Match 过滤、跨其余 label 求和。没有 _count 序列返回 $null(→ n/a)。
# Buckets 是按 le 升序的 { Le; Cum }(累计数,跨 label 逐 le 相加);没有 _bucket 序列时为空数组。
function Get-HistogramOrNull {
    param([hashtable]$M, [string]$Name, [hashtable]$Match = @{})
    $count = Get-SeriesSumOrNull $M "${Name}_count" $Match
    if ($null -eq $count) { return $null }
    $sum = Get-SeriesSumOrNull $M "${Name}_sum" $Match
    $byLe = @{}
    foreach ($k in $M.Keys) {
        $labels = Get-SeriesLabels -Key $k -Name "${Name}_bucket"
        if ($null -eq $labels -or -not $labels.ContainsKey('le')) { continue }
        if (-not (Test-LabelsMatch $labels $Match)) { continue }
        $le = if ($labels['le'] -eq '+Inf') { [double]::PositiveInfinity }
              else { [double]::Parse($labels['le'], [System.Globalization.CultureInfo]::InvariantCulture) }
        if (-not $byLe.ContainsKey($le)) { $byLe[$le] = 0.0 }
        $byLe[$le] += [double]$M[$k]
    }
    $buckets = @($byLe.Keys | Sort-Object | ForEach-Object { [pscustomobject]@{ Le = [double]$_; Cum = [double]$byLe[$_] } })
    return [pscustomobject]@{
        Count   = [double]$count
        Sum     = $(if ($null -eq $sum) { 0.0 } else { [double]$sum })
        Buckets = $buckets
    }
}

# 分位数,同 PromQL histogram_quantile:在累计桶上线性插值,第一个桶的下界取 0。
# 返回 $null(没有桶)、NaN(总数为 0),或 { Value; Overflow }:目标秩落在 +Inf 桶时 Value 取最高的有限上界、
# Overflow=$true(打印时加 '>',表示「至少这么大」)。值是累计口径(进程启动以来),桶越粗插值越不准。
function Get-HistogramQuantile {
    param($Hist, [double]$Q)
    if ($null -eq $Hist -or $Hist.Buckets.Count -eq 0) { return $null }
    $total = $Hist.Buckets[-1].Cum
    if ($total -le 0) { return [double]::NaN }
    $rank = $Q * $total
    $prevLe = 0.0
    $prevCum = 0.0
    foreach ($b in $Hist.Buckets) {
        if ($b.Cum -ge $rank) {
            if ([double]::IsPositiveInfinity($b.Le)) { return [pscustomobject]@{ Value = $prevLe; Overflow = $true } }
            if ($b.Cum -le $prevCum) { return [pscustomobject]@{ Value = $b.Le; Overflow = $false } }
            return [pscustomobject]@{ Value = $prevLe + ($b.Le - $prevLe) * ($rank - $prevCum) / ($b.Cum - $prevCum); Overflow = $false }
        }
        $prevLe = $b.Le
        $prevCum = $b.Cum
    }
    return [pscustomobject]@{ Value = $prevLe; Overflow = $true }
}

# histogram 的「n / 均值 / p99」三格。$UnitScale 把观测值换算成秒:秒级 histogram 传 1,go-zero 的 *_ms 传 0.001。
# 缺序列 → 三格 n/a;count 为 0 → n 打 0、均值与 p99 打 "-";有 count 没有 bucket → p99 打 n/a。
function Get-HistogramCells {
    param($Hist, [double]$UnitScale = 1.0, [int]$Decimals = 1)
    if ($null -eq $Hist) { return @('n/a', 'n/a', 'n/a') }
    $n = Format-CountCell $Hist.Count
    if ($Hist.Count -le 0) { return @($n, '-', '-') }
    $avg = Format-AvgCell ($Hist.Sum / $Hist.Count * $UnitScale) $Decimals
    $q = Get-HistogramQuantile -Hist $Hist -Q 0.99
    $p99 = if ($null -eq $q) { 'n/a' }
           elseif ($q -is [double]) { '-' }
           else { $(if ($q.Overflow) { '>' } else { '' }) + (Format-AvgCell ($q.Value * $UnitScale) $Decimals) }
    return @($n, $avg, $p99)
}

# 只要均值一格(M1 总览用)。
function Get-HistogramAvgCell {
    param($Hist, [double]$UnitScale = 1.0, [int]$Decimals = 1)
    return (Get-HistogramCells $Hist $UnitScale $Decimals)[1]
}

# 字节 histogram 的「n / 均值 / p99」三格,缺序列与没有样本的规则同 Get-HistogramCells。
function Get-HistogramByteCells {
    param($Hist)
    if ($null -eq $Hist) { return @('n/a', 'n/a', 'n/a') }
    $n = Format-CountCell $Hist.Count
    if ($Hist.Count -le 0) { return @($n, '-', '-') }
    $q = Get-HistogramQuantile -Hist $Hist -Q 0.99
    $p99 = if ($null -eq $q) { 'n/a' }
           elseif ($q -is [double]) { '-' }
           else { $(if ($q.Overflow) { '>' } else { '' }) + (Format-DecimalCell $q.Value 0) }
    return @($n, (Format-DecimalCell ($Hist.Sum / $Hist.Count) 0), $p99)
}

# 「族内存在规则」:没有预建的 counter(scene_manager_home_zone_lookup_total、login_gate_directory_reads_total、
# db_player_store_requests_total)
# 在第一次事件之前整族一条序列都没有。整族都没有 → 每个取值 $null(→ n/a:分不清旧版本与零事件,如实标 n/a);
# 族内有任一序列 → 缺的取值记 0(同一进程的同一个计数器,有一条序列即证明已注册、采集已开,缺的取值只是
# 还没发生过)。先例是 CreatePlayer 段 name_orphan 的 $stageSeen 兜底。返回按 $Values 顺序的 取值 → 数。
function Get-FamilyCountsOrNull {
    param([hashtable]$M, [string]$Name, [string]$Label, [string[]]$Values, [hashtable]$Match = @{})
    $familySeen = $null -ne (Get-SeriesSumOrNull $M $Name $Match)
    $counts = [ordered]@{}
    foreach ($v in $Values) {
        if (-not $familySeen) { $counts[$v] = $null; continue }
        $one = $Match.Clone()
        $one[$Label] = $v
        $c = Get-SeriesSumOrNull $M $Name $one
        $counts[$v] = if ($null -eq $c) { 0.0 } else { $c }
    }
    return $counts
}

# 按段打印「无数据」行。带 ASCII 的 no data,方便脚本与测试在任意控制台编码下匹配。
function Write-NoData {
    param([string]$Reason)
    Write-Host "  (无数据 no data: $Reason)" -ForegroundColor DarkGray
}

# ----- Redis INFO 快照(stress_snap.ps1 -RedisTargets 写的 t<N>m_redis_<name>.txt) --------------

# "calls=21,usec=175,usec_per_call=8.33" → @{ calls = 21; usec = 175; usec_per_call = 8.33 }。
function ConvertFrom-RedisKvList {
    param([string]$Text)
    $h = @{}
    foreach ($part in $Text.Split(',')) {
        $j = $part.IndexOf('=')
        if ($j -le 0) { continue }
        $d = 0.0
        if ([double]::TryParse($part.Substring($j + 1), [System.Globalization.NumberStyles]::Float,
                [System.Globalization.CultureInfo]::InvariantCulture, [ref]$d)) {
            $h[$part.Substring(0, $j)] = $d
        }
    }
    return $h
}

# 解析一份 INFO 快照。首行 '# stress_snap …' 是抓取元信息,跳过;'# Xxx' 是 section 头,记进 Sections
# (区分「section 没抓」与「section 在、该字段只是没出现」,如 errorstat_OOM 只在发生过 OOM 后才有);
# cmdstat_<cmd> / latency_percentiles_usec_<cmd> / errorstat_<ERR> 的 k=v 列表分别进 Cmd / Lat / Err,其余进 Fields。
function Read-RedisInfoSnapshot {
    param([System.IO.FileInfo]$File)
    if (-not ($File.BaseName -match '^t(?<n>\d+)m_redis_(?<name>[a-z][a-z0-9-]*)$')) { return $null }
    $snap = [pscustomobject]@{
        Tag      = [int]$Matches['n']
        Name     = $Matches['name']
        Fields   = @{}
        Cmd      = @{}
        Lat      = @{}
        Err      = @{}
        Sections = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    }
    foreach ($raw in [System.IO.File]::ReadLines($File.FullName)) {
        $line = $raw.Trim()
        if ($line.Length -eq 0 -or $line.StartsWith('# stress_snap ')) { continue }
        if ($line.StartsWith('#')) { $null = $snap.Sections.Add($line.TrimStart('#').Trim()); continue }
        $i = $line.IndexOf(':')
        if ($i -le 0) { continue }
        $key = $line.Substring(0, $i)
        $value = $line.Substring($i + 1)
        if ($key.StartsWith('cmdstat_')) { $snap.Cmd[$key.Substring('cmdstat_'.Length)] = ConvertFrom-RedisKvList $value }
        elseif ($key.StartsWith('latency_percentiles_usec_')) { $snap.Lat[$key.Substring('latency_percentiles_usec_'.Length)] = ConvertFrom-RedisKvList $value }
        elseif ($key.StartsWith('errorstat_')) { $snap.Err[$key.Substring('errorstat_'.Length)] = ConvertFrom-RedisKvList $value }
        else { $snap.Fields[$key] = $value }
    }
    return $snap
}

function Get-RedisNumberOrNull {
    param($Snap, [string]$Key)
    if (-not $Snap.Fields.ContainsKey($Key)) { return $null }
    $d = 0.0
    if ([double]::TryParse($Snap.Fields[$Key], [System.Globalization.NumberStyles]::Float,
            [System.Globalization.CultureInfo]::InvariantCulture, [ref]$d)) { return $d }
    return $null
}

# 若干累计字段之和在窗口内的增量。任一字段在任一端缺失返回 $null(→ n/a);$Prev 为 $null 时以 0 为起点(自实例启动)。
function Get-RedisDeltaOrNull {
    param($Prev, $Cur, [string[]]$Keys)
    $total = 0.0
    foreach ($k in $Keys) {
        $c = Get-RedisNumberOrNull $Cur $k
        if ($null -eq $c) { return $null }
        $p = 0.0
        if ($null -ne $Prev) {
            $p = Get-RedisNumberOrNull $Prev $k
            if ($null -eq $p) { return $null }
        }
        $total += $c - $p
    }
    return $total
}

# errorstat_<Type> 的累计次数。Errorstats section 没抓 → $null;抓了但没有这一行 → 0(该类错误还没发生过)。
function Get-RedisErrorCountOrNull {
    param($Snap, [string]$Type)
    if (-not $Snap.Sections.Contains('Errorstats')) { return $null }
    if ($Snap.Err.ContainsKey($Type) -and $Snap.Err[$Type].ContainsKey('count')) { return [double]$Snap.Err[$Type]['count'] }
    return 0.0
}

# 一个窗口的实例负载(R1 / M1 共用)。口径:
#   Δt = Δuptime_in_seconds(Redis 自己的时钟,不受采集时刻漂移影响)。run_id 变了或 Δuptime ≤ 0 = 重启 / 切主,
#   Restart=$true,增量列一律不出数(不把两个实例的计数相减)。$Prev 为 $null = 单份快照,以实例启动为起点
#   (Δx = 累计值,Δt = uptime)。
#   ops/s = Δtotal_commands_processed / Δt;cpu% = Δ(used_cpu_sys + used_cpu_user) / Δt × 100;
#   main% = Δ(used_cpu_sys_main_thread + used_cpu_user_main_thread) / Δt × 100,是单线程是否饱和的判据,
#   老版本没有这两个字段时为 $null(→ n/a);网络 KB/s 用 Δtotal_net_input_bytes / Δtotal_net_output_bytes;
#   rej_conn / err_rep / oom / aof_dfs 是窗口内增量;clients / used_MB / mem% 取窗口末的瞬时值,maxmemory=0 时 mem% 为 NaN(→ "-")。
function Get-RedisWindow {
    param($Prev, $Cur)
    $curUp = Get-RedisNumberOrNull $Cur 'uptime_in_seconds'
    $win = [ordered]@{ Restart = $false; Secs = $null; Ops = $null; Cpu = $null; Main = $null; InKBs = $null; OutKBs = $null
        RejConn = $null; ErrRep = $null; Oom = $null; AofDfs = $null; Clients = $null; UsedMB = $null; MemPct = $null }

    $win.Clients = Get-RedisNumberOrNull $Cur 'connected_clients'
    $used = Get-RedisNumberOrNull $Cur 'used_memory'
    if ($null -ne $used) { $win.UsedMB = $used / 1048576.0 }
    $maxMem = Get-RedisNumberOrNull $Cur 'maxmemory'
    if ($null -ne $used -and $null -ne $maxMem) { $win.MemPct = if ($maxMem -le 0) { [double]::NaN } else { 100.0 * $used / $maxMem } }

    if ($null -eq $curUp) { $win.Restart = $true; return [pscustomobject]$win }
    $secs = $curUp
    if ($null -ne $Prev) {
        $prevUp = Get-RedisNumberOrNull $Prev 'uptime_in_seconds'
        if ($null -eq $prevUp -or $Prev.Fields['run_id'] -cne $Cur.Fields['run_id'] -or $curUp -le $prevUp) {
            $win.Restart = $true
            return [pscustomobject]$win
        }
        $secs = $curUp - $prevUp
    }
    if ($secs -le 0) { $win.Restart = $true; return [pscustomobject]$win }
    $win.Secs = $secs

    $perSec = { param($d) if ($null -eq $d) { $null } else { $d / $secs } }
    $win.Ops = & $perSec (Get-RedisDeltaOrNull $Prev $Cur @('total_commands_processed'))
    $cpu = Get-RedisDeltaOrNull $Prev $Cur @('used_cpu_sys', 'used_cpu_user')
    if ($null -ne $cpu) { $win.Cpu = 100.0 * $cpu / $secs }
    $main = Get-RedisDeltaOrNull $Prev $Cur @('used_cpu_sys_main_thread', 'used_cpu_user_main_thread')
    if ($null -ne $main) { $win.Main = 100.0 * $main / $secs }
    $in = & $perSec (Get-RedisDeltaOrNull $Prev $Cur @('total_net_input_bytes'))
    if ($null -ne $in) { $win.InKBs = $in / 1024.0 }
    $out = & $perSec (Get-RedisDeltaOrNull $Prev $Cur @('total_net_output_bytes'))
    if ($null -ne $out) { $win.OutKBs = $out / 1024.0 }
    $win.RejConn = Get-RedisDeltaOrNull $Prev $Cur @('rejected_connections')
    $win.ErrRep = Get-RedisDeltaOrNull $Prev $Cur @('total_error_replies')
    $win.AofDfs = Get-RedisDeltaOrNull $Prev $Cur @('aof_delayed_fsync')
    $oomCur = Get-RedisErrorCountOrNull $Cur 'OOM'
    $oomPrev = if ($null -eq $Prev) { 0.0 } else { Get-RedisErrorCountOrNull $Prev 'OOM' }
    if ($null -ne $oomCur -and $null -ne $oomPrev) { $win.Oom = $oomCur - $oomPrev }
    return [pscustomobject]$win
}

# ----- 跨语言钉名的取值数组(2026-10-23,L-18 观测批) -----------------------
#
# 下面的数组与 Go 侧常量逐项对应,Go 的钉名测试按 `$名字 = @( … )` 读出这些数组再比对(如
# go/data_service/internal/metrics 的 TestLoginReadNamesMatchStressSummarize):两边不一致时整列会静默变成 n/a,
# 只有钉名测试能先红。改名或加取值要同改 Go 常量、本数组与 test_stress_summarize.ps1;括号里不写注释。
#
# $loginRpcMethods:login 调 data_service 的 gRPC 方法全名(go-zero rpc_client_requests_* 的 method label),
#   与 dspb.DataService_<Method>_FullMethodName 逐项对应;$loginRpcColumns 是 L1 / M1 的列名前缀,与之同序:
#   bgz = BatchGetPlayerHomeZone、bpn = BatchGetPlayerName、ghz = GetPlayerHomeZone(只在 RedirectOnEnterEnabled
#   打开时调用,通常没有序列)、rad = ReadAccountDirectory、lar = LookupAccountRole(EnterGame 每次 3 次)。
$loginRpcMethods = @('/data_service.DataService/BatchGetPlayerHomeZone', '/data_service.DataService/BatchGetPlayerName', '/data_service.DataService/GetPlayerHomeZone', '/data_service.DataService/ReadAccountDirectory', '/data_service.DataService/LookupAccountRole')
$loginRpcColumns = @('bgz', 'bpn', 'ghz', 'rad', 'lar')
# $gateDirectorySources:login_gate_directory_reads_total 的 source,同 go/login/internal/logic/pkg/node/node_cache.go
#   的 readSourceCache / readSourceUnsynced / readSourceStale。
$gateDirectorySources = @('cache', 'etcd_unsynced', 'etcd_stale')
# $smEnterSceneStages:scene_manager_enter_scene_stage_seconds 的全部 stage,同 go/scene_manager/internal/metrics
#   的 EnterSceneStage* 常量(前 7 个是原 SM stages 表的列,列序不动;home_zone / role_lookup 由 S1 表出)。
#   $smLookupStages 声明哪几个进 S1 表,必须是 $smEnterSceneStages 的子集。
$smEnterSceneStages = @('dedup', 'scene_resolve', 'reserve', 'update_loc', 'route_gate', 'release_dispatch', 'cross_zone', 'home_zone', 'role_lookup')
$smLookupStages = @('home_zone', 'role_lookup')
# $homeZoneLookupOutcomes:scene_manager_home_zone_lookup_total 的 outcome(go/scene_manager/internal/metrics 的 Help)。
$homeZoneLookupOutcomes = @('mapped', 'unmapped', 'unconfigured', 'error')
# $homeZoneReadRpcs / $homeZoneReadResults:data_service_home_zone_read_seconds 的 rpc / result 取值
#   (home = GetPlayerHomeZone,batch = BatchGetPlayerHomeZone;not_found 只在 home 出现,两种 rpc 都预建全部组合)。
$homeZoneReadRpcs = @('home', 'batch')
$homeZoneReadResults = @('ok', 'not_found', 'error')
# $accountDirectoryRpcs:data_service_account_directory_rpc_seconds 的 rpc 取值。规则:
#   go/data_service/internal/server/account_directory.go 里的每个账号目录 RPC,方法名去掉 AccountDirectory /
#   AccountRole / AccountRoles 前后缀后转小写,按该文件中的方法顺序:ReadAccountDirectory → read、CreateAccountRole →
#   create、LookupAccountRole → lookup、RepairAccountRole → repair、RemoveAccountRoles → remove。角色交易
#   (account_role_trade.go)与入场登记(account_role_admission.go)不在该文件,不计入。
# $accountDirectoryResults:result 取值。rejected = 鉴权失败或业务拒绝(InvalidArgument / PermissionDenied /
#   ResourceExhausted / FailedPrecondition),error = 依赖故障(Unavailable),not_found = NotFound。
$accountDirectoryRpcs = @('read', 'create', 'lookup', 'repair', 'remove')
$accountDirectoryResults = @('ok', 'not_found', 'rejected', 'error')
# $redisAlwaysCommands:R2 在 Δcalls 前 8 名之外必列的命令(有就列),对应 mapping 读路径与核心读写。
$redisAlwaysCommands = @('mget', 'evalsha', 'eval', 'get', 'set')
# $playerStoreSaveColumns / $playerStoreClaimColumns:db_player_store_requests_total 的 result → 列名。result 取
#   proto/player_store/player_store.proto 的 SavePlayerResult / LoadAndClaimResult 枚举名,加 go/db/internal/metrics 的
#   unknown_outcome / unavailable / denied(请求没走到判定就结束)。枚举加值要同改这里与 test_stress_summarize.ps1。
$playerStoreSaveColumns = [ordered]@{
    committed = 'SAVE_PLAYER_COMMITTED'; equal = 'SAVE_PLAYER_EQUAL'
    stale = 'SAVE_PLAYER_STALE_OWNER'; not_hold = 'SAVE_PLAYER_NOT_HOLDER'; unclaimed = 'SAVE_PLAYER_UNCLAIMED'; older = 'SAVE_PLAYER_OLDER_SEQ'
    no_owner = 'SAVE_PLAYER_NO_OWNER'; invalid = 'SAVE_PLAYER_INVALID'; regressed = 'SAVE_PLAYER_SCHEMA_REGRESSED'; too_new = 'SAVE_PLAYER_SCHEMA_TOO_NEW'; too_large = 'SAVE_PLAYER_TOO_LARGE'
    unknown = 'unknown_outcome'; unavail = 'unavailable'; denied = 'denied'
}
$playerStoreClaimColumns = [ordered]@{
    claimed = 'LOAD_AND_CLAIM_CLAIMED'
    stale = 'LOAD_AND_CLAIM_STALE_OWNER'; not_hold = 'LOAD_AND_CLAIM_NOT_HOLDER'; no_owner = 'LOAD_AND_CLAIM_NO_OWNER'; invalid = 'LOAD_AND_CLAIM_INVALID'; too_new = 'LOAD_AND_CLAIM_SCHEMA_TOO_NEW'
    unknown = 'unknown_outcome'; unavail = 'unavailable'; denied = 'denied'
}
# $playerStoreRowKinds:db_player_store_row_bytes 的 kind(PlayerStoreKind 枚举名,写权行不带 payload,不在这里)→ 列名前缀。
$playerStoreRowKinds = [ordered]@{ hot = 'PLAYER_STORE_KIND_HOT'; asset = 'PLAYER_STORE_KIND_ASSET'; growth = 'PLAYER_STORE_KIND_GROWTH'; profile = 'PLAYER_STORE_KIND_PROFILE'; legacy1 = 'PLAYER_STORE_KIND_LEGACY1' }

if ($loginRpcMethods.Count -ne $loginRpcColumns.Count) {
    throw "`$loginRpcMethods 与 `$loginRpcColumns 长度不一致"
}
foreach ($st in $smLookupStages) {
    if ($smEnterSceneStages -cnotcontains $st) { throw "`$smLookupStages 的 $st 不在 `$smEnterSceneStages 里" }
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
$dsSnapshots = @()
$redisSnapshots = @()

if (Test-Path $snapDir) {
    $allTxt = Get-ChildItem $snapDir -Filter "*.txt" | Sort-Object Name
    # Redis INFO 快照(stress_snap.ps1 -RedisTargets 写的 t{N}m_redis_<name>.txt)先剔出去,其余才是 prom 快照。
    # 它们按分钟数数值排序:Name 字典序里 t10m 排在 t2m 前面,求差会把窗口倒过来。
    $redisPattern = '^t\d+m_redis_[a-z][a-z0-9-]*$'
    $redisSnapshots = @($allTxt | Where-Object { $_.BaseName -match $redisPattern } |
        ForEach-Object { Read-RedisInfoSnapshot $_ } | Sort-Object Tag, Name)
    $promTxt = @($allTxt | Where-Object { $_.BaseName -notmatch $redisPattern })
    # New multi-endpoint layout (Round 16+) uses suffixes: t{N}m_login.txt,
    # t{N}m_sm.txt, t{N}m_db.txt, t{N}m_ds.txt (data_service, 2026-10-23+).
    # sm / db / ds 一律按后缀归组(2026-10-23 起;此前只在存在 _login 时才认,login 全部抓取失败的目录会把
    # sm / db 文件当成 login 快照)。Old single-endpoint layout used bare t{N}m.txt and only ever contained
    # login metrics:没有任何 _login 时,只把不带已知后缀的 txt 当 login 快照,纯 legacy 目录的行为不变。
    $loginTxt    = @($promTxt | Where-Object { $_.BaseName -match '_login$' })
    $smSnapshots = @($promTxt | Where-Object { $_.BaseName -match '_sm$' })
    $dbSnapshots = @($promTxt | Where-Object { $_.BaseName -match '_db$' })
    $dsSnapshots = @($promTxt | Where-Object { $_.BaseName -match '_ds$' })
    if ($loginTxt.Count -gt 0) {
        $snapshots += $loginTxt
    } else {
        # Legacy: bare t{N}m.txt files all hold login metrics.
        $snapshots += @($promTxt | Where-Object { $_.BaseName -notmatch '_(login|sm|db|ds)$' })
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
    "  snapshot                       success  apply_failed  lock_lost  fail%   total{ok}avg  apply avg  pool_saturated" | Write-Host
    "  --------                       -------  ------------  ---------  -----   ------------  ---------  --------------" | Write-Host

    foreach ($snap in $snapshots) {
        $m = Read-PromSnapshot $snap.FullName
        $success      = Get-Metric $m 'entergame_total{result="success"}'
        $applyFail    = Get-Metric $m 'entergame_total{result="apply_failed"}'
        $lockLost     = Get-Metric $m 'entergame_total{result="lock_lost"}'
        $total = $success + $applyFail + $lockLost
        $failPct = if ($total -gt 0) { [math]::Round(($applyFail + $lockLost) / $total * 100, 1) } else { 0.0 }

        # pool_saturated = PreloadPool 满、入场链根本没启动(EnterGame 当场回 kLoginInProgress)的次数。
        # 它**不是**链的终态:只在 entergame_total 里计数,不进任何耗时直方图,所以不计入上面的
        # $total 与 fail%,单列一列放在最后。入场链整段跑在 PreloadPool 的 worker 上(配置键名是预加载时代留下的),
        # 这一列有数就该调大 login 的 PreloadPool.Size。
        # 缺序列打印 n/a 而不是 0:login 起服时由 PrimeEnterGameMetrics(clientplayerlogin/metrics.go)
        # 把这条序列预置成 0,所以带这个取值的 login 的快照里它恒存在,健康压测读到的就是 0;
        # 读到 n/a = 这个 login 不导出该取值(2026-10-13 之前的版本 / 预置没跑 / 采集没开),不是「测到 0」。
        $poolSatV = Get-MetricSumOrNull $m '^entergame_total\{.*result="pool_saturated"'
        $poolSatCell = if ($null -ne $poolSatV) { "{0:N0}" -f $poolSatV } else { "n/a" }

        $totalOkAvg = Format-Avg (Get-Metric $m 'entergame_total_seconds_sum{result="success"}') (Get-Metric $m 'entergame_total_seconds_count{result="success"}')
        $applyAvg   = Format-Avg (Get-Metric $m 'entergame_apply_seconds_sum{result="success"}') (Get-Metric $m 'entergame_apply_seconds_count{result="success"}')

        $name = $snap.BaseName
        $line = "  {0,-30} {1,-8} {2,-13} {3,-10} {4,-7} {5,-13} {6,-10} {7}" -f `
            $name, `
            (Format-Number $success 8), `
            (Format-Number $applyFail 13), `
            (Format-Number $lockLost 10), `
            ("{0:N1}%" -f $failPct), `
            $totalOkAvg, `
            $applyAvg, `
            $poolSatCell
        Write-Host $line
    }

    # 建角分阶段耗时(帮会二期 B3a-2,docs/design/guild-phase2/03-names.md §3.11 / §3.20)。
    # 指标在 login(:9101)上,所以与上面共用 login 快照。stage 的四个取值落码在
    # go/login/internal/logic/clientplayerlogin/metrics.go(由设计 §3.11 钉死)
    # —— 那边改名或加取值这里必须跟着改,否则整列静默变成 n/a,或者新取值根本没有列:
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

# ----- 2a. Login 出站 RPC / gate 目录(L-18 观测批,login :9101) ----
#
# 与上面两张 login 表共用 login 快照($snapshots),但放在那个 if / else 之外:一份 login 快照都没有时也打印标题与
# 「无数据」行,复盘时能分清「没抓到」和「表没了」。

# L1:go-zero 客户端拦截器的 rpc_client_requests_duration_ms{method}(毫秒,桶 1,2,5,10,25,50,…)与
#     rpc_client_requests_code_total{code,method}(code 是 gRPC 数字码,0 = OK)。每个方法四列:
#       _n    _count(累计调用次数);_avg 累计均值;_p99 由桶线性插值(桶粗,只看量级;落在 +Inf 桶打 '>最高上界')
#       _err  code≠0 的次数(含 NotFound 等业务码,不全是故障)
#     这是客户端侧耗时,含网络、熔断与选点,不能与 D1 / D2 的服务端耗时直接相减。只有 login 配置带 Prometheus 段
#     时 go-zero 才出这些序列(本机 login.yaml 有;K8s 由 k8s_deploy.ps1 的 login ConfigMap 写入,2026-10-23 起),
#     缺序列打印 n/a。方法与列名见 $loginRpcMethods。
$l1Fmt = '  {0,-30}'
$l1Header = @('snapshot')
$l1Rule = @('--------')
for ($i = 0; $i -lt $loginRpcColumns.Count; $i++) {
    $base = 1 + 4 * $i
    $l1Fmt += (' | {{{0},-6}} {{{1},-8}} {{{2},-8}} {{{3},-5}}' -f $base, ($base + 1), ($base + 2), ($base + 3))
    foreach ($suffix in @('n', 'avg', 'p99', 'err')) {
        $title = "$($loginRpcColumns[$i])_$suffix"
        $l1Header += $title
        $l1Rule += ('-' * $title.Length)
    }
}
$l1Legend = for ($i = 0; $i -lt $loginRpcColumns.Count; $i++) { "$($loginRpcColumns[$i])=$($loginRpcMethods[$i] -replace '^.*/', '')" }
Write-Host ""
Write-Host "=== L1 Login outbound RPC (rpc_client_requests_duration_ms / _code_total, login :9101) ===" -ForegroundColor Yellow
Write-Host "  $($l1Legend -join ' ')"
Write-Host ""
if ($snapshots.Count -eq 0) {
    Write-NoData "没有 login 快照"
} else {
    Write-Host ($l1Fmt -f $l1Header)
    Write-Host ($l1Fmt -f $l1Rule)
    foreach ($snap in $snapshots) {
        $m = Read-PromSnapshot $snap.FullName
        $cells = @($snap.BaseName)
        foreach ($method in $loginRpcMethods) {
            $cells += Get-HistogramCells (Get-HistogramOrNull $m 'rpc_client_requests_duration_ms' @{ method = $method }) 0.001 1
            $codeAll = Get-SeriesSumOrNull $m 'rpc_client_requests_code_total' @{ method = $method }
            $codeOk = Get-SeriesSumOrNull $m 'rpc_client_requests_code_total' @{ method = $method; code = '0' }
            if ($null -eq $codeOk) { $codeOk = 0.0 }
            $cells += $(if ($null -eq $codeAll) { 'n/a' } else { Format-CountCell ($codeAll - $codeOk) })
        }
        Write-Host ($l1Fmt -f $cells)
    }
}

# L2:login_gate_directory_reads_total{source}(go-zero 计数器,没有预建,第一次读之前整族没有序列 → 族内存在规则,
#     见 Get-FamilyCountsOrNull)。cache% = cache / 全部,稳态应接近 100%;etcd_stale 持续增长说明 etcd 不可达或
#     watch 反复卡住,登录又回到了逐请求打 etcd(L-18 第 4 步)。
$l2Fmt = "  {0,-30} | {1,-8} {2,-13} {3,-10} | {4}"
Write-Host ""
Write-Host "=== L2 Login gate directory reads (login_gate_directory_reads_total) ===" -ForegroundColor Yellow
Write-Host ""
if ($snapshots.Count -eq 0) {
    Write-NoData "没有 login 快照"
} else {
    Write-Host ($l2Fmt -f (@('snapshot') + $gateDirectorySources + @('cache%')))
    Write-Host ($l2Fmt -f (@('--------') + @($gateDirectorySources | ForEach-Object { '-' * $_.Length }) + @('------')))
    foreach ($snap in $snapshots) {
        $m = Read-PromSnapshot $snap.FullName
        $counts = Get-FamilyCountsOrNull $m 'login_gate_directory_reads_total' 'source' $gateDirectorySources
        $cells = @($snap.BaseName) + @($gateDirectorySources | ForEach-Object { Format-CountCell $counts[$_] })
        $cells += Format-RatioCell $counts[$gateDirectorySources[0]] (Get-SumOrNull $counts.Values)
        Write-Host ($l2Fmt -f $cells)
    }
}

# ----- 2b. SceneManager EnterScene stage breakdown (Round 16+) ----------

if ($smSnapshots.Count -gt 0) {
    Write-Host ""
    Write-Host "=== SceneManager EnterScene stages (scene_manager_enter_scene_stage_seconds) ===" -ForegroundColor Yellow
    Write-Host ""
    "  snapshot                       dedup     scene_res reserve  upd_loc  route_gt rel_disp cross_zn" | Write-Host
    "  --------                       --------- --------- -------- -------- -------- -------- --------" | Write-Host

    # 原有 7 列(dedup … cross_zone),列序与 prev-summary 对齐、不动;home_zone / role_lookup 进下面的 S1 表。
    $stages = @($smEnterSceneStages | Where-Object { $smLookupStages -cnotcontains $_ })
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

# S1:scene_manager 的两次外部查询。左半是 home_zone_lookup_total{outcome}(没有预建,族内存在规则见 Get-FamilyCountsOrNull);
#     右半是 enter_scene_stage_seconds 的 $smLookupStages 两个 stage,跨 zone_id 求和:hz = data_service GetPlayerHomeZone
#     的单次往返,rl = LookupAccountRole(全局 TiDB 点查;zone_id 恒为 0,样本含 LeaveScene)。旧版 sm 没有这两个 stage → n/a。
$s1Fmt = "  {0,-30} | {1,-7} {2,-9} {3,-9} {4,-6} | {5,-7} {6,-9} {7,-9} | {8,-7} {9,-9} {10}"
Write-Host ""
Write-Host "=== S1 SceneManager home_zone / role_lookup (home_zone_lookup_total, enter_scene_stage_seconds) ===" -ForegroundColor Yellow
Write-Host ""
if ($smSnapshots.Count -eq 0) {
    Write-NoData "没有 *_sm 快照"
} else {
    Write-Host ($s1Fmt -f @('snapshot', 'mapped', 'unmapped', 'unconfig', 'error', 'hz_n', 'hz_avg', 'hz_p99', 'rl_n', 'rl_avg', 'rl_p99'))
    Write-Host ($s1Fmt -f @('--------', '------', '--------', '--------', '-----', '----', '------', '------', '----', '------', '------'))
    foreach ($snap in $smSnapshots) {
        $m = Read-PromSnapshot $snap.FullName
        $counts = Get-FamilyCountsOrNull $m 'scene_manager_home_zone_lookup_total' 'outcome' $homeZoneLookupOutcomes
        $cells = @($snap.BaseName) + @($homeZoneLookupOutcomes | ForEach-Object { Format-CountCell $counts[$_] })
        foreach ($st in $smLookupStages) {
            $cells += Get-HistogramCells (Get-HistogramOrNull $m 'scene_manager_enter_scene_stage_seconds' @{ stage = $st }) 1.0 1
        }
        Write-Host ($s1Fmt -f $cells)
    }
}

# D1:data_service 归属读(*_ds,:9260)。h_* = GetPlayerHomeZone(进场 / 换图,带合服围栏),b_* = BatchGetPlayerHomeZone
#     (登录角色列表)。ok / nf / err 是 data_service_home_zone_read_seconds 按 result 的次数;avg / p99 跨 result 合并,
#     是服务端 handler 耗时,与 L1 的 login 客户端耗时之差约等于网络与排队。ids/b = home_zone_batch_ids 的均值(每批几个角色)。
#     rr_* = data_service_route_read_total 在 path ∈ {home_fence, batch} 上按 result 求和(只在读方门禁打开时预建):
#     rr_cache = 副本命中,rr_sql = 回源库(sql + not_found_sql),rr_sqlerr = 回源失败。门禁关着或旧版 data_service 打 n/a。
$d1Fmt = "  {0,-30} | {1,-6} {2,-5} {3,-6} {4,-8} {5,-8} | {6,-6} {7,-5} {8,-6} {9,-8} {10,-8} | {11,-5} | {12,-8} {13,-7} {14}"
Write-Host ""
Write-Host "=== D1 data_service home-zone reads (data_service_home_zone_read_seconds / _batch_ids / _route_read_total, :9260) ===" -ForegroundColor Yellow
Write-Host ""
if ($dsSnapshots.Count -eq 0) {
    Write-NoData "没有 *_ds 快照(stress_snap.ps1 缺省抓 data_service :9260)"
} else {
    Write-Host ($d1Fmt -f @('snapshot', 'h_ok', 'h_nf', 'h_err', 'h_avg', 'h_p99', 'b_ok', 'b_nf', 'b_err', 'b_avg', 'b_p99', 'ids/b', 'rr_cache', 'rr_sql', 'rr_sqlerr'))
    Write-Host ($d1Fmt -f @('--------', '----', '----', '-----', '-----', '-----', '----', '----', '-----', '-----', '-----', '-----', '--------', '------', '---------'))
    foreach ($snap in $dsSnapshots) {
        $m = Read-PromSnapshot $snap.FullName
        $cells = @($snap.BaseName)
        foreach ($rpc in $homeZoneReadRpcs) {
            foreach ($result in $homeZoneReadResults) {
                $cells += Format-CountCell (Get-SeriesSumOrNull $m 'data_service_home_zone_read_seconds_count' @{ rpc = $rpc; result = $result })
            }
            $hc = Get-HistogramCells (Get-HistogramOrNull $m 'data_service_home_zone_read_seconds' @{ rpc = $rpc }) 1.0 1
            $cells += @($hc[1], $hc[2])
        }
        $ids = Get-HistogramOrNull $m 'data_service_home_zone_batch_ids'
        $cells += $(if ($null -eq $ids) { 'n/a' } elseif ($ids.Count -le 0) { '-' } else { Format-DecimalCell ($ids.Sum / $ids.Count) 1 })
        # route_read_total 不是每个 path × result 组合都存在(batch 没有逐 id 的缺席结论,没有 not_found_sql),
        # 所以按「族存在规则」取:整族没有序列 → n/a;有 → 缺的组合按 0 计。
        $rrSeen = $null -ne (Get-SeriesSumOrNull $m 'data_service_route_read_total')
        $rr = @{}
        foreach ($res in @('cache', 'sql', 'not_found_sql', 'sql_error')) {
            if (-not $rrSeen) { $rr[$res] = $null; continue }
            $sum = 0.0
            foreach ($path in @('home_fence', 'batch')) {
                $v = Get-SeriesSumOrNull $m 'data_service_route_read_total' @{ path = $path; result = $res }
                if ($null -ne $v) { $sum += $v }
            }
            $rr[$res] = $sum
        }
        $cells += @(
            (Format-CountCell $rr['cache']),
            (Format-CountCell (Get-SumOrNull @($rr['sql'], $rr['not_found_sql']))),
            (Format-CountCell $rr['sql_error']))
        Write-Host ($d1Fmt -f $cells)
    }
}

# D2:data_service 账号目录 RPC(data_service_account_directory_rpc_seconds{rpc,result},每个快照 × 每个 rpc 一行)。
#     n / avg / p99 跨 result 合并;nf / rej / err 是 $accountDirectoryResults 里三种非 OK 结局的次数(rejected = 明确拒绝,
#     error = 依赖故障)。read 是每次 Login 的悲观事务,lookup 是 EnterGame / scene_manager 的点查,二者是登录链路落到
#     全局 TiDB 的主要负载;看 err 与 p99 判断目录池(AccountDirectory.MaxOpenConn)是否开始排队。
$d2NonOk = @($accountDirectoryResults | Where-Object { $_ -cne 'ok' })
$d2Fmt = "  {0,-30} | {1,-7} | {2,-7} {3,-9} {4,-9} | {5,-5} {6,-5} {7}"
Write-Host ""
Write-Host "=== D2 data_service account directory RPC (data_service_account_directory_rpc_seconds, :9260) ===" -ForegroundColor Yellow
Write-Host ""
if ($dsSnapshots.Count -eq 0) {
    Write-NoData "没有 *_ds 快照"
} else {
    Write-Host ($d2Fmt -f @('snapshot', 'rpc', 'n', 'avg', 'p99', 'nf', 'rej', 'err'))
    Write-Host ($d2Fmt -f @('--------', '---', '-', '---', '---', '--', '---', '---'))
    foreach ($snap in $dsSnapshots) {
        $m = Read-PromSnapshot $snap.FullName
        foreach ($rpc in $accountDirectoryRpcs) {
            $cells = @($snap.BaseName, $rpc)
            $cells += Get-HistogramCells (Get-HistogramOrNull $m 'data_service_account_directory_rpc_seconds' @{ rpc = $rpc }) 1.0 1
            foreach ($result in $d2NonOk) {
                $cells += Format-CountCell (Get-SeriesSumOrNull $m 'data_service_account_directory_rpc_seconds_count' @{ rpc = $rpc; result = $result })
            }
            Write-Host ($d2Fmt -f $cells)
        }
    }
}

# ----- 2bb. Dirty-save skip rate (cpp scene log, parsed not scraped) ----
#
# Why log-parsing instead of a Prometheus scrape: cpp scene processes don't
# expose a metrics HTTP endpoint today (only go SceneManager :9150 and go
# db :9160 do). The two atomic counters live in dirty_save_stats.h and emit
# one LOG_INFO line from the scene's 30 s counter snapshot timer. We pull
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

# ----- 2bc. 节点间 RPC0 调用结局(cpp gate / scene 日志的 [RpcChannel] summary) ----
#
# C++ 节点没有 Prometheus 出口,RpcChannelStats(cpp/libs/engine/core/network/rpc_channel_stats.h)每 30s、
# 计数有变化时打一行 `[RpcChannel] summary k=v ...`,计数是进程级、单调累计的,所以每个日志文件取最后一行。
# 字段名是与 FormatRpcChannelStats 之间的契约(docs/design/muduo-rpc-channel.md §12),改名要两边一起改。
# 节点日志实际落在 bin\logs\cpp_nodes(start_game.ps1),旧路径 run\logs\cpp_nodes 也一并扫。
# 一行都没有时保持沉默(老版本二进制没有这行)。

$rpcChannelFields = @('started', 'ok', 'not_sent', 'remote_rejected', 'remote_failed', 'reply_dropped', 'timeout',
                      'disconnected', 'channel_closed', 'invalid_response', 'late_reply', 'legacy_id0_reply',
                      'max_in_flight', 'handler_failed', 'reply_abandoned')
$rpcChannelLatest = [ordered]@{}
foreach ($dir in @((Join-Path $RepoRoot "bin\logs\cpp_nodes"), $cppLogDir)) {
    if (-not (Test-Path $dir)) { continue }
    $nodeLogs = @(Get-ChildItem -Path $dir -Filter 'gate*.log' -ErrorAction SilentlyContinue) +
                @(Get-ChildItem -Path $dir -Filter 'scene*.log' -ErrorAction SilentlyContinue)
    foreach ($log in $nodeLogs) {
        # 与上面 DirtySave 同理:节点进程运行中持有写句柄,必须以 FileShare.ReadWrite 打开。
        $fs = $null
        $sr = $null
        $last = $null
        try {
            $fs = [System.IO.FileStream]::new(
                $log.FullName,
                [System.IO.FileMode]::Open,
                [System.IO.FileAccess]::Read,
                [System.IO.FileShare]::ReadWrite)
            $sr = [System.IO.StreamReader]::new($fs)
            while (-not $sr.EndOfStream) {
                $line = $sr.ReadLine()
                $idx = $line.IndexOf('[RpcChannel] summary ')
                if ($idx -ge 0) { $last = $line.Substring($idx + '[RpcChannel] summary '.Length) }
            }
        } finally {
            if ($sr) { $sr.Dispose() }
            if ($fs) { $fs.Dispose() }
        }
        if ($null -ne $last) {
            $values = @{}
            foreach ($pair in ($last -split '\s+')) {
                if ($pair -match '^(?<k>[a-z0-9_]+)=(?<v>\d+)$') { $values[$matches['k']] = [int64]$matches['v'] }
            }
            $rpcChannelLatest["$($log.Directory.Name)/$($log.Name)"] = $values
        }
    }
}

if ($rpcChannelLatest.Count -gt 0) {
    Write-Host ""
    Write-Host "=== RPC0 call outcomes (cpp gate/scene LOG_INFO [RpcChannel] summary, cumulative) ===" -ForegroundColor Yellow
    Write-Host ""
    foreach ($entry in $rpcChannelLatest.GetEnumerator()) {
        $cells = foreach ($field in $rpcChannelFields) {
            $v = if ($entry.Value.ContainsKey($field)) { $entry.Value[$field] } else { 'n/a' }
            "$field=$v"
        }
        Write-Host ("  {0}" -f $entry.Key)
        Write-Host ("    {0}" -f ($cells -join ' '))
    }
    Write-Host "  (after a same-zone cutover timeout / late_reply / legacy_id0_reply should stay 0)"
}

# ----- 2c. 存盘接口(go/db 的 PlayerStore gRPC,*_db 快照,:9160) ----------
#
# go/db 只剩这一件事(docs/design/player-save-direct-commit.md),下面四张表就是 DB 的对比表。指标来源
# go/db/internal/metrics/metrics.go 与 go/shared/sqlpoolstats,那边改名或加取值,这里与 test_stress_summarize.ps1
# 必须同改,否则整列静默变成 n/a。

# 一个 op 的 n / avg / p99(db_player_store_seconds,服务端耗时,含写权闸的语句;p99 由桶线性插值,桶粗,只看量级),
# 后面跟 $Columns 里每个结局的次数(db_player_store_requests_total)。这个计数器没有预建,按族内存在规则:
# 本 op 一条序列都没有 → n/a(还没有这类请求,或不导出它的 db);有任一序列 → 缺的结局打 0。
function Get-PlayerStoreOpCells {
    param([hashtable]$M, [string]$Op, $Columns)
    $results = @($Columns.Values)
    $counts = Get-FamilyCountsOrNull $M 'db_player_store_requests_total' 'result' $results @{ op = $Op }
    $cells = @(Get-HistogramCells (Get-HistogramOrNull $M 'db_player_store_seconds' @{ op = $Op }) 1.0 1)
    $cells += @($results | ForEach-Object { Format-CountCell $counts[$_] })
    return $cells
}

$dbNoSnapshot = 'no *_db snapshots (stress_snap.ps1 scrapes db :9160 by default)'

# P1:存盘。committed 与 equal 都算提交成功(equal = 同序号重发,幂等)。stale / not_hold / unclaimed / older 是写权与序号的拒绝,
#     持续增长说明有人还在写已被接管的玩家。no_owner / invalid / regressed / too_new / too_large 正常为 0:too_new 是发布顺序错了,
#     too_large 是 payload 超预算。unknown = 提交结局未知(scene 用同一序号重发),unavail = 落点或库打不开,denied = 调用方鉴权失败。
$p1Fmt = "  {0,-30} | {1,-7} {2,-9} {3,-9} | {4,-9} {5,-6} | {6,-6} {7,-8} {8,-9} {9,-6} | {10,-8} {11,-7} {12,-9} {13,-7} {14,-9} | {15,-7} {16,-7} {17}"
$p1Columns = @('n', 'avg', 'p99') + @($playerStoreSaveColumns.Keys)
Write-Host ""
Write-Host "=== P1 Player store save (db_player_store_seconds, db_player_store_requests_total, :9160) ===" -ForegroundColor Yellow
Write-Host ""
if ($dbSnapshots.Count -eq 0) {
    Write-NoData $dbNoSnapshot
} else {
    Write-Host ($p1Fmt -f (@('snapshot') + $p1Columns))
    Write-Host ($p1Fmt -f (@('--------') + @($p1Columns | ForEach-Object { '-' * $_.Length })))
    foreach ($snap in $dbSnapshots) {
        $m = Read-PromSnapshot $snap.FullName
        Write-Host ($p1Fmt -f (@($snap.BaseName) + @(Get-PlayerStoreOpCells $m 'save' $playerStoreSaveColumns)))
    }
}

# P2:认领(进场读档)。claimed 之外:stale = 路由过期,not_hold = 同代次被别的实例持有,no_owner = 角色不存在或未建完,
#     too_new = 库里的世代比 scene 新;unknown / unavail / denied 同 P1。
$p2Fmt = "  {0,-30} | {1,-7} {2,-9} {3,-9} | {4,-8} | {5,-6} {6,-8} {7,-8} {8,-7} {9,-7} | {10,-7} {11,-7} {12}"
$p2Columns = @('n', 'avg', 'p99') + @($playerStoreClaimColumns.Keys)
Write-Host ""
Write-Host "=== P2 Player store claim (db_player_store_seconds, db_player_store_requests_total, :9160) ===" -ForegroundColor Yellow
Write-Host ""
if ($dbSnapshots.Count -eq 0) {
    Write-NoData $dbNoSnapshot
} else {
    Write-Host ($p2Fmt -f (@('snapshot') + $p2Columns))
    Write-Host ($p2Fmt -f (@('--------') + @($p2Columns | ForEach-Object { '-' * $_.Length })))
    foreach ($snap in $dbSnapshots) {
        $m = Read-PromSnapshot $snap.FullName
        Write-Host ($p2Fmt -f (@($snap.BaseName) + @(Get-PlayerStoreOpCells $m 'claim' $playerStoreClaimColumns)))
    }
}

# P3:已提交行的 payload 大小(db_player_store_row_bytes{kind},字节,只记 SAVE_PLAYER_COMMITTED 里带的行)。每组三列 n / avg / p99;
#     某组没有序列打 n/a。n 的比例就是写放大的分布:只写有变化的行,hot 应远多于其它组。
$p3Fmt = '  {0,-30}'
$p3Header = @('snapshot')
$p3Rule = @('--------')
$p3Index = 0
foreach ($col in $playerStoreRowKinds.Keys) {
    $base = 1 + 3 * $p3Index
    $p3Fmt += (' | {{{0},-9}} {{{1},-11}} {{{2},-11}}' -f $base, ($base + 1), ($base + 2))
    foreach ($suffix in @('n', 'avg', 'p99')) {
        $title = "${col}_$suffix"
        $p3Header += $title
        $p3Rule += ('-' * $title.Length)
    }
    $p3Index++
}
Write-Host ""
Write-Host "=== P3 Player store row bytes (db_player_store_row_bytes, committed rows, bytes) ===" -ForegroundColor Yellow
Write-Host ""
if ($dbSnapshots.Count -eq 0) {
    Write-NoData $dbNoSnapshot
} else {
    Write-Host ($p3Fmt -f $p3Header)
    Write-Host ($p3Fmt -f $p3Rule)
    foreach ($snap in $dbSnapshots) {
        $m = Read-PromSnapshot $snap.FullName
        $cells = @($snap.BaseName)
        foreach ($kind in $playerStoreRowKinds.Values) {
            $cells += Get-HistogramByteCells (Get-HistogramOrNull $m 'db_player_store_row_bytes' @{ kind = $kind })
        }
        Write-Host ($p3Fmt -f $cells)
    }
}

# P4:连接池与落点。rpool_wait / rpool_tmo = go-redis 连接池等待次数 / 取连接超时次数(db_redis_pool_wait_total / timeouts_total,
#     跨 redis_client 求和;placement 与 main 是同一个 client 时只导出 main 一份,不会重复计数)。
#     sql_in_use = 此刻在用的 SQL 连接数,sql_wait / sql_wait_s = 等连接的累计次数与秒数(mmorpg_sql_pool_*{sql_client="db"},跨 pool 求和),
#     存盘慢先看这两列。open_stores = 常驻的落点库个数,open_err = 按需打开失败或被拒的累计次数(db_placement_store_open_total 的
#     error + rejected),cooldown = 此刻在库级冷却期内的库个数(db_store_cooldown_stores,跨 pool 求和)。
$p4Fmt = "  {0,-30} | {1,-10} {2,-9} | {3,-10} {4,-8} {5,-10} | {6,-11} {7,-8} {8}"
Write-Host ""
Write-Host "=== P4 DB pools and placement (db_redis_pool_*, mmorpg_sql_pool_*, db_placement_*, db_store_cooldown_stores) ===" -ForegroundColor Yellow
Write-Host ""
if ($dbSnapshots.Count -eq 0) {
    Write-NoData $dbNoSnapshot
} else {
    Write-Host ($p4Fmt -f @('snapshot', 'rpool_wait', 'rpool_tmo', 'sql_in_use', 'sql_wait', 'sql_wait_s', 'open_stores', 'open_err', 'cooldown'))
    Write-Host ($p4Fmt -f @('--------', '----------', '---------', '----------', '--------', '----------', '-----------', '--------', '--------'))
    foreach ($snap in $dbSnapshots) {
        $m = Read-PromSnapshot $snap.FullName
        $sqlMatch = @{ sql_client = 'db' }
        $openErr = Get-SumOrNull @(
            (Get-SeriesSumOrNull $m 'db_placement_store_open_total' @{ result = 'error' }),
            (Get-SeriesSumOrNull $m 'db_placement_store_open_total' @{ result = 'rejected' }))
        Write-Host ($p4Fmt -f @($snap.BaseName,
            (Format-CountCell (Get-SeriesSumOrNull $m 'db_redis_pool_wait_total')),
            (Format-CountCell (Get-SeriesSumOrNull $m 'db_redis_pool_timeouts_total')),
            (Format-CountCell (Get-SeriesSumOrNull $m 'mmorpg_sql_pool_in_use_connections' $sqlMatch)),
            (Format-CountCell (Get-SeriesSumOrNull $m 'mmorpg_sql_pool_wait_count_total' $sqlMatch)),
            (Format-DecimalCell (Get-SeriesSumOrNull $m 'mmorpg_sql_pool_wait_duration_seconds_total' $sqlMatch) 1),
            (Format-CountCell (Get-SeriesSumOrNull $m 'db_placement_open_stores')),
            (Format-CountCell $openErr),
            (Format-CountCell (Get-SeriesSumOrNull $m 'db_store_cooldown_stores'))))
    }
}

# ----- 2h. Redis INFO 快照(R1 / R2)与映射读路径总览(M1),2026-10-23 -----------

# 按 target 分组(组内已按分钟数数值排序),并找出「同一时刻 run_id 相同」的 target:同一个实例被抓了两份
# (G4a 之前 mapping 与核心就是同一台),后出现的只标 same_as=<先出现的 target>,不重复出数。
$redisByTarget = [ordered]@{}
$redisSameAs = @{}
$redisFirstByTagRun = @{}
foreach ($s in $redisSnapshots) {
    if (-not $redisByTarget.Contains($s.Name)) { $redisByTarget[$s.Name] = [System.Collections.Generic.List[object]]::new() }
    $redisByTarget[$s.Name].Add($s)
    $rid = $s.Fields['run_id']
    if (-not $rid) { continue }
    $k = "$($s.Tag)|$rid"
    if ($redisFirstByTagRun.ContainsKey($k)) { $redisSameAs["$($s.Tag)|$($s.Name)"] = $redisFirstByTagRun[$k] }
    else { $redisFirstByTagRun[$k] = $s.Name }
}

# R1 一行的 16 格;$Win 来自 Get-RedisWindow。重启窗口只出瞬时值,增量列打 "-"。
function Get-RedisLoadCells {
    param([string]$Target, [string]$Window, $Win, [string]$Note)
    $clients = Format-CountCell $Win.Clients
    $usedMB = Format-DecimalCell $Win.UsedMB 1
    $memPct = Format-DecimalCell $Win.MemPct 1 '%'
    if ($Win.Restart) {
        $n = @($Note, 'restart') | Where-Object { $_ }
        return @($Target, $Window, '-', '-', '-', '-', '-', '-', $clients, $usedMB, $memPct, '-', '-', '-', '-', ($n -join ' '))
    }
    return @($Target, $Window,
        (Format-DecimalCell $Win.Secs 0), (Format-DecimalCell $Win.Ops 0), (Format-DecimalCell $Win.Cpu 1 '%'), (Format-DecimalCell $Win.Main 1 '%'),
        (Format-DecimalCell $Win.InKBs 1), (Format-DecimalCell $Win.OutKBs 1),
        $clients, $usedMB, $memPct,
        (Format-CountCell $Win.RejConn), (Format-CountCell $Win.ErrRep), (Format-CountCell $Win.Oom), (Format-CountCell $Win.AofDfs),
        $Note)
}

function Get-RedisSameAsCells {
    param([string]$Target, [string]$Window, [string]$SameAs, [int]$Width)
    return @($Target, $Window) + @(1..($Width - 3) | ForEach-Object { '' }) + @("same_as=$SameAs")
}

# R1:每个 target 相邻两份一行,再加一行首→末整体窗口(overall);只有一份时打一行「since start」。口径见 Get-RedisWindow:
#     ops/s、cpu%、main%(单线程饱和判据,接近 100% 就是瓶颈)、网络 KB/s 按窗口求差;clients / used_MB / mem% 取窗口末瞬时值。
$r1Fmt = "  {0,-10} | {1,-11} | {2,-6} {3,-9} {4,-7} {5,-7} {6,-8} {7,-8} | {8,-7} {9,-8} {10,-6} | {11,-8} {12,-7} {13,-4} {14,-7} | {15}"
Write-Host ""
Write-Host "=== R1 Redis instance load (INFO snapshots t<N>m_redis_<name>.txt) ===" -ForegroundColor Yellow
Write-Host ""
if ($redisSnapshots.Count -eq 0) {
    Write-NoData "没有 t<N>m_redis_<name>.txt(stress_snap.ps1 -RedisTargets)"
} else {
    Write-Host ($r1Fmt -f @('target', 'window', 'secs', 'ops/s', 'cpu%', 'main%', 'in_KB/s', 'out_KB/s', 'clients', 'used_MB', 'mem%', 'rej_conn', 'err_rep', 'oom', 'aof_dfs', 'note'))
    Write-Host ($r1Fmt -f @('------', '------', '----', '-----', '----', '-----', '-------', '--------', '-------', '-------', '----', '--------', '-------', '---', '-------', '----'))
    foreach ($name in $redisByTarget.Keys) {
        $list = $redisByTarget[$name]
        if ($list.Count -eq 1) {
            $s = $list[0]
            $same = $redisSameAs["$($s.Tag)|$name"]
            if ($same) { Write-Host ($r1Fmt -f (Get-RedisSameAsCells $name "t$($s.Tag)m" $same 16)); continue }
            Write-Host ($r1Fmt -f (Get-RedisLoadCells $name "t$($s.Tag)m" (Get-RedisWindow $null $s) '(single snapshot, since start)'))
            continue
        }
        for ($i = 1; $i -lt $list.Count; $i++) {
            $p = $list[$i - 1]
            $c = $list[$i]
            $window = "t$($p.Tag)m-t$($c.Tag)m"
            $same = $redisSameAs["$($c.Tag)|$name"]
            if ($same) { Write-Host ($r1Fmt -f (Get-RedisSameAsCells $name $window $same 16)); continue }
            Write-Host ($r1Fmt -f (Get-RedisLoadCells $name $window (Get-RedisWindow $p $c) ''))
        }
        if ($list.Count -gt 2) {
            $p = $list[0]
            $c = $list[$list.Count - 1]
            $window = "t$($p.Tag)m-t$($c.Tag)m"
            $same = $redisSameAs["$($c.Tag)|$name"]
            if ($same) { Write-Host ($r1Fmt -f (Get-RedisSameAsCells $name $window $same 16)); continue }
            Write-Host ($r1Fmt -f (Get-RedisLoadCells $name $window (Get-RedisWindow $p $c) 'overall'))
        }
    }
}

# cmdstat 的 k=v 字段取值,缺失记 0(该命令在窗口起点之前还没被调用过)。
function Get-RedisCmdField {
    param($Snap, [string]$Cmd, [string]$Field)
    if ($null -eq $Snap -or -not $Snap.Cmd.ContainsKey($Cmd)) { return 0.0 }
    $kv = $Snap.Cmd[$Cmd]
    if ($kv.ContainsKey($Field)) { return [double]$kv[$Field] }
    return 0.0
}

# R2:Redis 热点命令。每个 target 取首→末整体窗口(只有一份时自实例启动):Δcalls 前 8 名,外加 $redisAlwaysCommands 里
#     出现过的。calls/s、usec/call 是窗口内 Redis 自身执行耗时(不含网络与排队);p50 / p99 / p99.9 取最后一份的
#     latency_percentiles_usec(自实例启动或上次 CONFIG RESETSTAT 的累计口径,单位 µs);failed / rejected 是窗口内
#     Δfailed_calls / Δrejected_calls。窗口跨了重启时整组打 restart,不出数。
$r2Fmt = "  {0,-10} | {1,-11} | {2,-18} | {3,-9} {4,-9} | {5,-8} {6,-8} {7,-8} | {8,-7} {9}"
Write-Host ""
Write-Host "=== R2 Redis top commands (commandstats delta, latencystats cumulative) ===" -ForegroundColor Yellow
Write-Host ""
if ($redisSnapshots.Count -eq 0) {
    Write-NoData "没有 t<N>m_redis_<name>.txt(stress_snap.ps1 -RedisTargets)"
} else {
    Write-Host ($r2Fmt -f @('target', 'window', 'cmd', 'calls/s', 'usec/call', 'p50', 'p99', 'p99.9', 'failed', 'rejected'))
    Write-Host ($r2Fmt -f @('------', '------', '---', '-------', '---------', '---', '---', '-----', '------', '--------'))
    foreach ($name in $redisByTarget.Keys) {
        $list = $redisByTarget[$name]
        $last = $list[$list.Count - 1]
        $first = if ($list.Count -gt 1) { $list[0] } else { $null }
        $window = if ($null -eq $first) { "t$($last.Tag)m" } else { "t$($first.Tag)m-t$($last.Tag)m" }
        $same = $redisSameAs["$($last.Tag)|$name"]
        if ($same) { Write-Host ($r2Fmt -f (Get-RedisSameAsCells $name $window $same 10)); continue }
        $win = Get-RedisWindow $first $last
        if ($win.Restart -or $null -eq $win.Secs) {
            Write-Host ($r2Fmt -f @($name, $window, '-', '-', '-', '-', '-', '-', '-', 'restart'))
            continue
        }
        if (-not $last.Sections.Contains('Commandstats')) {
            Write-Host ($r2Fmt -f @($name, $window, 'n/a', 'n/a', 'n/a', 'n/a', 'n/a', 'n/a', 'n/a', '(commandstats not captured)'))
            continue
        }
        $rows = foreach ($cmd in $last.Cmd.Keys) {
            [pscustomobject]@{
                Cmd      = $cmd
                Calls    = (Get-RedisCmdField $last $cmd 'calls') - (Get-RedisCmdField $first $cmd 'calls')
                Usec     = (Get-RedisCmdField $last $cmd 'usec') - (Get-RedisCmdField $first $cmd 'usec')
                Failed   = (Get-RedisCmdField $last $cmd 'failed_calls') - (Get-RedisCmdField $first $cmd 'failed_calls')
                Rejected = (Get-RedisCmdField $last $cmd 'rejected_calls') - (Get-RedisCmdField $first $cmd 'rejected_calls')
            }
        }
        $top = @($rows | Where-Object { $_.Calls -gt 0 } | Sort-Object -Property @{ Expression = 'Calls'; Descending = $true }, @{ Expression = 'Cmd'; Descending = $false } | Select-Object -First 8)
        $topNames = @($top | ForEach-Object { $_.Cmd })
        $extra = @($rows | Where-Object { $redisAlwaysCommands -ccontains $_.Cmd -and $topNames -cnotcontains $_.Cmd } | Sort-Object Cmd)
        if (($top.Count + $extra.Count) -eq 0) {
            Write-Host ($r2Fmt -f @($name, $window, '-', '-', '-', '-', '-', '-', '-', '(no commands in window)'))
            continue
        }
        foreach ($r in @($top) + @($extra)) {
            $lat = if ($last.Lat.ContainsKey($r.Cmd)) { $last.Lat[$r.Cmd] } else { @{} }
            $pct = foreach ($q in @('p50', 'p99', 'p99.9')) {
                if ($lat.ContainsKey($q)) { Format-DecimalCell $lat[$q] 1 } else { 'n/a' }
            }
            $usecPerCall = if ($r.Calls -gt 0) { Format-DecimalCell ($r.Usec / $r.Calls) 2 } else { '-' }
            Write-Host ($r2Fmt -f @($name, $window, $r.Cmd, (Format-DecimalCell ($r.Calls / $win.Secs) 1), $usecPerCall,
                $pct[0], $pct[1], $pct[2], (Format-CountCell $r.Failed), (Format-CountCell $r.Rejected)))
        }
    }
}

# M1:映射读路径总览,按 t<N>m 把各端关键均值连成一行(prom 是进程启动以来的累计均值;Redis 取该时刻与同一 target
#     上一份快照之间的窗口,只有一份时自实例启动)。缺哪份快照哪列打 n/a。
#     l_bgz / l_rad = login 客户端 BatchGetPlayerHomeZone / ReadAccountDirectory;ds_batch / ds_home / ds_dir = data_service
#     服务端批量归属读 / 单查 / ReadAccountDirectory;sm_hz / sm_rl = scene_manager 两个查询 stage;db_claim / db_save =
#     go/db 存盘接口认领 / 存盘的服务端耗时(db_player_store_seconds{op});rd = Redis target,mapping 优先,没有就 core,
#     再没有就第一个。G4a 之前 mapping 与核心是同一个实例,rd 的数是两者合计。L-18 第 5 步「是不是瓶颈」先看这一张。
$m1ByTag = @{}
foreach ($pair in @(@('login', $snapshots), @('sm', $smSnapshots), @('ds', $dsSnapshots), @('db', $dbSnapshots))) {
    foreach ($f in $pair[1]) {
        if ($f.BaseName -match '^t(\d+)m(_|$)') {
            $t = [int]$Matches[1]
            if (-not $m1ByTag.ContainsKey($t)) { $m1ByTag[$t] = @{} }
            $m1ByTag[$t][$pair[0]] = $f
        }
    }
}
$m1Redis = if ($redisByTarget.Contains('mapping')) { 'mapping' } elseif ($redisByTarget.Contains('core')) { 'core' }
           elseif ($redisByTarget.Count -gt 0) { @($redisByTarget.Keys)[0] } else { $null }
$m1RedisByTag = @{}
if ($null -ne $m1Redis) {
    $list = $redisByTarget[$m1Redis]
    for ($i = 0; $i -lt $list.Count; $i++) {
        $prev = if ($i -gt 0) { $list[$i - 1] } else { $null }
        $m1RedisByTag[$list[$i].Tag] = Get-RedisWindow $prev $list[$i]
        if (-not $m1ByTag.ContainsKey($list[$i].Tag)) { $m1ByTag[$list[$i].Tag] = @{} }
    }
}
$m1Bgz = $loginRpcMethods[[array]::IndexOf($loginRpcColumns, 'bgz')]
$m1Rad = $loginRpcMethods[[array]::IndexOf($loginRpcColumns, 'rad')]
$m1Fmt = "  {0,-6} | {1,-8} {2,-8} | {3,-8} {4,-8} {5,-8} | {6,-8} {7,-8} | {8,-10} {9,-10} | {10,-8} {11}"
Write-Host ""
Write-Host "=== M1 Mapping read path at a glance (avg per tag; rd = Redis $(if ($m1Redis) { $m1Redis } else { 'n/a' })) ===" -ForegroundColor Yellow
Write-Host ""
if ($m1ByTag.Count -eq 0) {
    Write-NoData "没有任何 t<N>m_* 快照"
} else {
    Write-Host ($m1Fmt -f @('tag', 'l_bgz', 'l_rad', 'ds_batch', 'ds_home', 'ds_dir', 'sm_hz', 'sm_rl', 'db_claim', 'db_save', 'rd_ops/s', 'rd_main%'))
    Write-Host ($m1Fmt -f @('---', '-----', '-----', '--------', '-------', '------', '-----', '-----', '--------', '-------', '--------', '--------'))
    foreach ($t in @($m1ByTag.Keys | Sort-Object)) {
        $row = $m1ByTag[$t]
        $m1MOf = { param($kind) if ($row.ContainsKey($kind)) { Read-PromSnapshot $row[$kind].FullName } else { $null } }
        $ml = & $m1MOf 'login'
        $md = & $m1MOf 'ds'
        $ms = & $m1MOf 'sm'
        $mb = & $m1MOf 'db'
        $m1Avg = { param($m, $name, $match, $scale) if ($null -eq $m) { 'n/a' } else { Get-HistogramAvgCell (Get-HistogramOrNull $m $name $match) $scale 1 } }
        $cells = @("t${t}m",
            (& $m1Avg $ml 'rpc_client_requests_duration_ms' @{ method = $m1Bgz } 0.001),
            (& $m1Avg $ml 'rpc_client_requests_duration_ms' @{ method = $m1Rad } 0.001),
            (& $m1Avg $md 'data_service_home_zone_read_seconds' @{ rpc = 'batch' } 1.0),
            (& $m1Avg $md 'data_service_home_zone_read_seconds' @{ rpc = 'home' } 1.0),
            (& $m1Avg $md 'data_service_account_directory_rpc_seconds' @{ rpc = 'read' } 1.0),
            (& $m1Avg $ms 'scene_manager_enter_scene_stage_seconds' @{ stage = 'home_zone' } 1.0),
            (& $m1Avg $ms 'scene_manager_enter_scene_stage_seconds' @{ stage = 'role_lookup' } 1.0),
            (& $m1Avg $mb 'db_player_store_seconds' @{ op = 'claim' } 1.0),
            (& $m1Avg $mb 'db_player_store_seconds' @{ op = 'save' } 1.0))
        $rw = $m1RedisByTag[$t]
        if ($null -eq $rw) { $cells += @('n/a', 'n/a') }
        elseif ($rw.Restart) { $cells += @('restart', 'restart') }
        else { $cells += @((Format-DecimalCell $rw.Ops 0), (Format-DecimalCell $rw.Main 1 '%')) }
        Write-Host ($m1Fmt -f $cells)
    }
}

# ----- 3. Kafka lag -----------------------------------------------------
#
# 玩家存盘不经 Kafka,go/db 没有消费组。还走消费组的是 data_service 的两条审计落库(交易流水、玩家快照),
# 组名见 -KafkaConsumerGroups。控制面命令 topic(gate-cmd / scene-cmd)是 assign 分区、不进消费组,这里看不到。

if ($KafkaContainer) {
    Write-Host ""
    Write-Host "=== Kafka consumer lag (live) ==========================================" -ForegroundColor Yellow
    Write-Host ""

    $cmd = "/opt/kafka/bin/kafka-consumer-groups.sh"
    $lagFmt = "  {0,-30} {1,-26} {2,-10} {3,-12} {4,-13} {5}"
    $lagHeaderPrinted = $false
    foreach ($group in $KafkaConsumerGroups) {
        $dockerArgs = @(
            "exec", $KafkaContainer, $cmd,
            "--bootstrap-server", "localhost:9092",
            "--describe", "--group", $group
        )

        $raw = ""
        try {
            $raw = & docker @dockerArgs 2>&1 | Out-String
        } catch {
            Write-Host "  docker exec failed: $($_.Exception.Message)" -ForegroundColor Red
            break
        }

        $partRows = @()
        foreach ($line in ($raw -split "`r?`n")) {
            # GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG ...
            if ($line -match '^\S+\s+(\S+)\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)') {
                $partRows += [pscustomobject]@{ Topic = $matches[1]; Part = [int]$matches[2]; Cur = [long]$matches[3]; End = [long]$matches[4]; Lag = [long]$matches[5] }
            }
        }

        if ($partRows.Count -eq 0) {
            Write-Host "  (no lag rows parsed: group $group empty, or container=$KafkaContainer down?)" -ForegroundColor DarkGray
            continue
        }
        if (-not $lagHeaderPrinted) {
            Write-Host ($lagFmt -f 'group', 'topic', 'partition', 'current', 'log_end', 'lag')
            Write-Host ($lagFmt -f '-----', '-----', '---------', '-------', '-------', '---')
            $lagHeaderPrinted = $true
        }
        $totalLag = [long]0
        foreach ($r in ($partRows | Sort-Object Topic, Part)) {
            $totalLag += $r.Lag
            Write-Host ($lagFmt -f $group, $r.Topic, $r.Part, $r.Cur.ToString("N0"), $r.End.ToString("N0"), $r.Lag.ToString("N0"))
        }
        Write-Host ("  TOTAL LAG ({0}): {1}" -f $group, $totalLag.ToString("N0")) -ForegroundColor Yellow
    }
}

Write-Host ""
