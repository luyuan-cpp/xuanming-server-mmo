#requires -Version 7
<#
.SYNOPSIS
    Disaster-recovery rollback for a single zone — orchestrates
    `k8s-zone-down` → MySQL PITR (manual) → Redis FLUSHDB → Kafka offset reset
    → `k8s-zone-up`.

.DESCRIPTION
    This script is the executable form of the SOP in
    docs/design/zone_data_rollback.md §3 "整 Zone 灾难恢复级回档".

    It will NOT do the destructive steps without -Apply. Default is dry-run.

    What it does (in order):
      1. `k8s-zone-down -ZoneName <zone>` — stop all writers
      2. Wait for Kafka consumer LAG to drain (in-flight writes settle)
      3. ⚠️ MySQL PITR is NOT automated — print runbook reference and pause
         for confirmation. Operator runs PITR manually, hits ENTER to continue.
      4. Redis FLUSHDB on the zone's data Redis (cache rebuilds from MySQL)
      5. Kafka offset reset on db_task_zone_<id> (avoids replaying post-target writes)
      6. `k8s-zone-up -ZoneName <zone> -ZoneId <id>` — restart zone
      7. Print verification checklist

    Why MySQL is manual:
      Picking the right binlog cut point and validating against shadow DB is
      a judgement call no script can safely make. See
      docs/ops/mysql-backup-pitr-runbook.md §4.1 for the procedure.

.EXAMPLE
    # Preview what would happen
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z"

.EXAMPLE
    # Run the destructive flow
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z" `
        -RedisHost zone-redis.mmorpg-zone-test1 `
        -KafkaBootstrap kafka:9092 `
        -Apply
#>
param(
    [Parameter(Mandatory = $true)]
    [string]$ZoneName,

    [Parameter(Mandatory = $true)]
    [int]$ZoneId,

    [Parameter(Mandatory = $true)]
    [string]$TargetTime,    # ISO 8601 UTC, e.g. "2026-05-15T14:23:00Z"

    # Zone-local Redis (cache that will be FLUSHDB'd).
    [string]$RedisHost = "",
    [string]$RedisPort = "6379",
    [string]$RedisPassword = "",
    [int]$RedisDB = 0,

    # Kafka cluster bootstrap.
    [string]$KafkaBootstrap = "kafka:9092",
    # 留空 = 按 -ZoneId 推导成 db_task_zone_<id>（go/db 与 go/login 两侧都是这么拼的:
    # go/db/internal/config.go DbTaskTopic / go/login/internal/config/config.go）。
    # 旧默认值 "db_task_topic" 是个**全仓不存在的 topic** —— offset reset 会「成功」地
    # 重置一个空 topic，回滚后 go/db 照样重放目标时间点之后的写入。
    # go/db 的 Kafka.TopicGeneration > 1 时真实名字带 _g<gen> 后缀，这里推不出来,
    # 必须显式传 -KafkaTopic。
    [string]$KafkaTopic = "",
    [string]$KafkaGroup = "db_rpc_consumer_group",

    # Zone restart args.
    #
    # 必填且必须是不可变 tag。以前这里默认 "…:latest" —— 灾难回滚脚本自己
    # 默认了一个可变 tag,等于"回档到某个时间点"时把 zone 拉回了**当前**
    # 镜像,而不是那个时间点在跑的版本。回滚必须由操作者显式指定回到哪一版。
    [Parameter(Mandatory = $true)]
    [string]$NodeImage,
    [string]$NamespacePrefix = "mmorpg-zone",

    # Skip the manual MySQL prompt (use only when PITR was done OOB earlier).
    [switch]$SkipMySqlPause,

    # Drain budget for Kafka consumer LAG before restart.
    [int]$KafkaDrainTimeoutSec = 300,

    [switch]$Apply
)

$ErrorActionPreference = "Stop"

# topic 名必须与 go/db 实际消费的一致,否则第 5 步会「成功」重置一个不存在的 topic。
if ($KafkaTopic -eq "") { $KafkaTopic = "db_task_zone_$ZoneId" }
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")

. (Join-Path $ScriptDir "lib\release_common.ps1")

# 回滚目标必须是不可变 tag,否则"回到旧版本"根本没发生。
$rollbackTag = Get-ImageTagFromRef -ImageRef $NodeImage
$rollbackChk = Test-ImmutableImageTag -Tag $rollbackTag
if (-not $rollbackChk.Ok) {
    throw "灾难回滚拒绝该 -NodeImage '$NodeImage':$($rollbackChk.Reason)。请传回滚目标版本的 git sha tag 或 repo@sha256:… digest。"
}

$verb = if ($Apply) { "APPLY" } else { "DRY-RUN" }
Write-Host "" -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host " k8s-zone-rollback ($verb)" -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host "  zone        : $ZoneName (id=$ZoneId)"
Write-Host "  target time : $TargetTime"
Write-Host "  redis       : $(if ($RedisHost) { "$RedisHost`:$RedisPort/db$RedisDB" } else { '(skipped — no -RedisHost)' })"
Write-Host "  kafka       : $KafkaBootstrap topic=$KafkaTopic group=$KafkaGroup"
Write-Host ""

if (-not $Apply) {
    Write-Host "DRY-RUN mode. The actions below would be executed in order." -ForegroundColor Yellow
    Write-Host "Re-run with -Apply to actually perform the rollback." -ForegroundColor Yellow
    Write-Host ""
}

# ── Step 1: stop the zone ────────────────────────────────────────
Write-Host "── Step 1: k8s-zone-down ──" -ForegroundColor Cyan
$downCmd = "& '$ScriptDir/dev_tools.ps1' -Command k8s-zone-down -ZoneName '$ZoneName' -NamespacePrefix '$NamespacePrefix'"
Write-Host "  $downCmd"
if ($Apply) {
    & "$ScriptDir/dev_tools.ps1" -Command k8s-zone-down -ZoneName $ZoneName -NamespacePrefix $NamespacePrefix
    if ($LASTEXITCODE -ne 0) { throw "k8s-zone-down failed" }
    Write-Host "  zone $ZoneName stopped" -ForegroundColor Green
}
Write-Host ""

# ── Step 2: drain Kafka consumer LAG ─────────────────────────────
Write-Host "── Step 2: drain Kafka consumer LAG (timeout ${KafkaDrainTimeoutSec}s) ──" -ForegroundColor Cyan
$describeCmd = "kafka-consumer-groups.sh --bootstrap-server $KafkaBootstrap --describe --group $KafkaGroup"
Write-Host "  $describeCmd"
if ($Apply) {
    $deadline = (Get-Date).AddSeconds($KafkaDrainTimeoutSec)
    while ((Get-Date) -lt $deadline) {
        $out = & kafka-consumer-groups.sh --bootstrap-server $KafkaBootstrap --describe --group $KafkaGroup 2>&1
        # LAG column is field 5 in default output. Sum non-zero values.
        $hasLag = $false
        foreach ($line in $out) {
            if ($line -match "^\S+\s+\S+\s+\d+\s+\d+\s+\d+\s+(\d+)") {
                if ([int]$matches[1] -gt 0) { $hasLag = $true; break }
            }
        }
        if (-not $hasLag) {
            Write-Host "  consumer LAG drained" -ForegroundColor Green
            break
        }
        Write-Host "  ... waiting for LAG to drain"
        Start-Sleep -Seconds 5
    }
    if ((Get-Date) -ge $deadline) {
        Write-Warning "consumer LAG did not drain within ${KafkaDrainTimeoutSec}s — proceeding anyway"
    }
}
Write-Host ""

# ── Step 3: MySQL PITR (manual) ──────────────────────────────────
Write-Host "── Step 3: MySQL Point-in-Time Recovery ──" -ForegroundColor Cyan
Write-Host "  ⚠️ This step is NOT automated." -ForegroundColor Yellow
Write-Host "  See docs/ops/mysql-backup-pitr-runbook.md §4.1 for the procedure."
Write-Host "  Target time: $TargetTime"
Write-Host ""
Write-Host "  Typical command (verify before running):"
Write-Host "    mysqlbinlog --stop-datetime=`"$($TargetTime.Replace('T',' ').TrimEnd('Z'))`" \\"
Write-Host "      /backup/binlog/mysql-bin.* | mysql -h shadow-host -u root -p game"
Write-Host ""

if ($Apply -and -not $SkipMySqlPause) {
    Write-Host "  When MySQL PITR is complete and verified, press ENTER to continue."
    Write-Host "  Press CTRL+C to abort." -ForegroundColor Yellow
    [void](Read-Host)
}

# ── Step 4: Redis FLUSHDB ────────────────────────────────────────
Write-Host "── Step 4: Redis FLUSHDB (zone cache) ──" -ForegroundColor Cyan
if ($RedisHost -eq "") {
    Write-Host "  skipped — pass -RedisHost to enable" -ForegroundColor Yellow
} else {
    $redisArgs = @("-h", $RedisHost, "-p", $RedisPort, "-n", $RedisDB)
    if ($RedisPassword -ne "") { $redisArgs += @("-a", $RedisPassword) }
    Write-Host "  redis-cli $($redisArgs -join ' ') FLUSHDB"
    if ($Apply) {
        & redis-cli @redisArgs FLUSHDB
        if ($LASTEXITCODE -ne 0) { throw "redis FLUSHDB failed" }
        Write-Host "  Redis db $RedisDB flushed" -ForegroundColor Green
    }
}
Write-Host ""

# ── Step 5: Kafka offset reset ───────────────────────────────────
Write-Host "── Step 5: Kafka offset reset ($KafkaTopic) ──" -ForegroundColor Cyan
# 必须用 hashtable splatting。之前用的是数组 splatting —— PowerShell 会把数组
# 元素当**位置参数**依次绑定,于是 "-BootstrapServer" 这个字符串本身被绑到
# BootstrapServer、"kafka:9092" 绑到 Group、…… 最后 "-Group" 撞上 [int]$Partitions
# 直接类型转换失败。结果是这个灾难恢复脚本连 dry-run 都跑不到第 6 步。
$kArgs = @{
    BootstrapServer = $KafkaBootstrap
    Topic           = $KafkaTopic
    Group           = $KafkaGroup
    ToDatetime      = $TargetTime
}
if ($Apply) { $kArgs.Apply = $true }
Write-Host ("  kafka_offset_reset.ps1 " + (($kArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { "-$($_.Key) $($_.Value)" }) -join ' '))
if ($Apply) {
    & "$ScriptDir/kafka_offset_reset.ps1" @kArgs
    if ($LASTEXITCODE -ne 0) { throw "kafka_offset_reset.ps1 failed" }
}
Write-Host ""

# ── Step 6: k8s-zone-up ──────────────────────────────────────────
Write-Host "── Step 6: k8s-zone-up ──" -ForegroundColor Cyan
# 同 Step 5:必须 hashtable splatting,数组 splatting 会按位置绑定,
# "-Command" 这个字符串会直接撞上 dev_tools.ps1 的 ValidateSet。
$upArgs = @{
    Command         = "k8s-zone-up"
    ZoneName        = $ZoneName
    ZoneId          = $ZoneId
    NodeImage       = $NodeImage
    NamespacePrefix = $NamespacePrefix
    WaitReady       = $true
}
Write-Host ("  dev_tools.ps1 " + (($upArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { "-$($_.Key) $($_.Value)" }) -join ' '))
if ($Apply) {
    & "$ScriptDir/dev_tools.ps1" @upArgs
    if ($LASTEXITCODE -ne 0) { throw "k8s-zone-up failed" }
    Write-Host "  zone $ZoneName up" -ForegroundColor Green
}
Write-Host ""

# ── Step 7: verification checklist ───────────────────────────────
Write-Host "── Step 7: post-rollback verification ──" -ForegroundColor Cyan
Write-Host "  Run these manually:"
Write-Host "    1. kubectl get pods -n ${NamespacePrefix}-${ZoneName}"
Write-Host "    2. Robot smoke login: pwsh -File tools/scripts/dev_tools.ps1 -Command dev-robot-zones -Zones $ZoneId"
Write-Host "    3. Spot-check player data:"
Write-Host "       redis-cli -h $RedisHost -p $RedisPort -n $RedisDB GET 'player:{<known-pid>}:player_database'"
Write-Host "    4. Tail scene/gate logs for replay errors (first 5 minutes)"
Write-Host ""
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host " Rollback complete. " -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
