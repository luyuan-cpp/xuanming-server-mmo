<#
.SYNOPSIS
  One-click Round 19 login-stress orchestrator (1 zone x 45k), verifying the two
  R2 gate-token-expiry fixes (gateTokenTTL 5min->10min + robot retry accounting).

.DESCRIPTION
  Drives the full AGENTS.md §6.2 clean-stress protocol in phases:

    plan       (default) print exactly what each phase WILL do, touch nothing.
    baseline   verify prev-summary.txt (R18) exists as the comparison baseline.
    wipe       redis FLUSHALL + mysql truncate(zone_1_db) + etcd del --prefix
               + kafka offset reset + prune old robot/cpp logs.   [DESTRUCTIVE]
               Refuses (before FLUSHALL) while a data_service route reconcile leader
               still holds the lease in the global DB (RouteStore.Mode=db_first);
               -AllowWithRunningLeader overrides. The plan prints the probe result.
    build      go-svc-build (Go only — NEVER compiles C++, uses existing gate.exe).
    start      dev-start-exe (Go services + cpp gate/scene from prebuilt exe).
    run        launch the 45k robot + background stress_snap (prom snapshots).
    summarize  run stress_summarize.ps1 and print the R18-vs-R19 comparison table.
    all        baseline -> wipe -> build -> start -> run -> (wait) -> summarize.

  SAFETY:
    * Default phase is 'plan' — nothing happens without -Execute.
    * The destructive 'wipe' additionally requires -WipeData (double gate). It
      DESTROYS whatever stack is currently up (incl. the currency-crash task).
    * This script NEVER triggers C++ compilation (repo memory cpp-build-policy).
      gate.exe/scene.exe must already be built.

.EXAMPLE
  # 1) Dry-run the whole plan (safe, prints steps only):
  pwsh tools/scripts/stress_round19.ps1 -Phase all

  # 2) Actually run end-to-end (will wipe data!):
  pwsh tools/scripts/stress_round19.ps1 -Phase all -Execute -WipeData

  # 3) Run phases one at a time:
  pwsh tools/scripts/stress_round19.ps1 -Phase wipe  -Execute -WipeData
  pwsh tools/scripts/stress_round19.ps1 -Phase build -Execute
  pwsh tools/scripts/stress_round19.ps1 -Phase start -Execute
  pwsh tools/scripts/stress_round19.ps1 -Phase run   -Execute
  # ...wait ~18 min...
  pwsh tools/scripts/stress_round19.ps1 -Phase summarize -Execute -RunDir robot/logs/stress-round19-<ts>
#>
[CmdletBinding()]
param(
    [ValidateSet('plan','baseline','wipe','build','start','run','summarize','all')]
    [string]$Phase = 'plan',

    # Master safety switch — without it every phase is a dry-run print.
    [switch]$Execute,

    # Second gate required specifically for the destructive 'wipe' phase.
    [switch]$WipeData,

    [int]$Zone = 1,
    [string]$RobotConfig = 'robot/etc/robot.stress-3zone-z1.yaml',  # 45k, profile=stress (R18 config)
    [string]$PrevSummary = 'docs/stress/prev-summary.txt',          # R18 baseline
    [string]$Stages = '2,5,10,15,18',
    [int]$SteadyMinutes = 18,   # how long 'all' waits before summarize
    [string]$RunDir = '',       # reused by 'summarize'; auto-created by 'run'/'all'

    # mysql tables to truncate in zone_<N>_db (same set as currency_crash_window.ps1).
    # 玩家存档只在 player_store;player_database / player_database_1 只是内存形状,不建表,写进来 TRUNCATE 会在第一条就中断。
    [string[]]$MySqlTables = @(
        'player_store','player_centre_database',
        'user_accounts','account_share_database'
    ),

    # data_service 全局库(go/data_service/etc/data_service.yaml 的 SnapshotMySQL.DBName):
    # 映射持久化的路由表与账号目录在这里(见 Clear-GlobalRouteTables):只清 player_store 不清账号目录,
    # 下一轮登录会读到旧角色,认领时写权行已经没了(LOAD_AND_CLAIM_NO_OWNER)。
    [string]$GlobalDbName = 'mmorpg_global',

    # 全局库里还有在任的映射对账 leader(RouteStore.Mode=db_first 的 data_service 在跑)时照样清库。
    # 不给时 wipe 在 FLUSHALL 之前探测并拒绝(见 Assert-NoRunningRouteReconcileLeader;D1 第 1b-3 批)。
    [switch]$AllowWithRunningLeader
)

$ErrorActionPreference = 'Stop'
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot  = (Resolve-Path (Join-Path $ScriptDir '..\..')).Path
$DevTools  = Join-Path $ScriptDir 'dev_tools.ps1'
$SnapPs1   = Join-Path $ScriptDir 'stress_snap.ps1'
$SummPs1   = Join-Path $ScriptDir 'stress_summarize.ps1'

function Hdr($t)  { Write-Host "`n=== $t ===" -ForegroundColor Cyan }
function Plan($t) { Write-Host "  [plan] $t" -ForegroundColor DarkGray }
function Do1($t)  { Write-Host "  [exec] $t" -ForegroundColor Green }
function Warn($t) { Write-Host "  [warn] $t" -ForegroundColor Yellow }

# Resolve the mysql root password the same way the rest of the repo does.
$MysqlPwd = if ($env:MYSQL_PASSWORD) { $env:MYSQL_PASSWORD } else { 'Mmorpg#2026db' }
$DbName   = "zone_${Zone}_db"

# ────────────────────────────────────────────────────────────────────────────
function Invoke-Baseline {
    Hdr 'Phase: baseline (verify R18 comparison baseline exists)'
    $p = Join-Path $RepoRoot $PrevSummary
    if (Test-Path $p) {
        Do1 "prev-summary present: $PrevSummary ($((Get-Item $p).Length) bytes)"
    } else {
        Warn "MISSING baseline: $PrevSummary"
        Warn "CLAUDE.md §6.2: prev-summary.txt must exist before the next round."
        Warn "Store the R18 stress_summarize output there first, or you must NOT proceed."
        if ($Execute) { throw "baseline missing — refusing to continue (CLAUDE §6.2)" }
    }
}

# ────────────────────────────────────────────────────────────────────────────
# 全局库里映射持久化的路由表(docs/design/player-route-mapping-durability-proposal.md §8.2,第 1a 批):
# 三张数据表 TRUNCATE,两张单行表原地复位(route_meta 只复位 writable;route_reconcile 清 leader / 租约 / 水位,
# 2026-10-12 G1a 从 route_meta 拆出)。与 currency_crash_window.ps1 的同名函数同一口径,改一处两处同改。
#
# route_meta / route_reconcile 不 TRUNCATE:清库可能发生在服务运行中,meta 行没了之后 db_first 的建角会一直 fail-closed
# (route_meta 缺行),直到下次 -migrate;第 1b 批的对账 leader 可能在跑,对账行没了它的租约 UPDATE 会落空。
# 原地复位不换 generation。
#
# 比 Invoke-Wipe 里 zone 表的清理更严格 —— SQL 失败就 throw,不 2>&1 | Out-Null 吞掉:Redis 已 FLUSHALL 而路由表还留着
# 上一轮的行,db_first 下就是「库有行、缓存全无」,同一个 id 再注册会落进 RowMissing / 冲突 / 已存在分支,
# audit 全是 db_only_unpublished —— 等于拿上一轮的映射跑这一轮,而且没有任何报错。
# 只处理已存在的表:没跑过迁移的库里一张都没有,是合法状态(跑过 G1a 之前迁移的库没有 route_reconcile,同样合法)。
#
# 本脚本只连本机 mysql 容器。TiDB 环境的清库按 docs/design/tidb-phase1-and-launch-readiness.md §3.5 做:
# TiDB 上 TRUNCATE 会换表 ID、丢掉预切与 table attributes,清完要重跑 `route_tool -f <yaml> tidb-prepare -zones=… -dry-run=false`。
# 既有全量本地 reset 同时清全局角色目录；Redis清空不再删除SQL归属。仅原显式wipe门禁允许执行。
$RouteDataTables        = @('account_role_import_receipt', 'account_role_trade_holder', 'account_role_admission', 'account_role_transfer_receipt', 'account_role_trade', 'account_role', 'account_directory', 'player_route', 'zone_merged_into')
$RouteMetaResetSql      = 'UPDATE route_meta SET writable=1 WHERE id=1;'
$RouteReconcileResetSql = 'UPDATE route_reconcile SET leader=NULL, lease_until_ms=NULL, watermark_ms=0, cache_verified_ms=0 WHERE id=1;'

# 在全局库所在的 MySQL 上执行一段 SQL(不指定默认库,语句自己带库名或 USE),非 0 退出码即 throw。
# 返回 stdout 的非空行(-N -B:不带列名、tab 分隔);stderr(含口令警告)不进返回值。
function Invoke-GlobalDbSql {
    param(
        [Parameter(Mandatory = $true)][string]$Sql,
        [Parameter(Mandatory = $true)][string]$What,
        [bool]$HasDocker
    )
    # 函数内局部改回 Continue:脚本级是 Stop,而 mysql 每次都往 stderr 写口令警告。Windows PowerShell 5.1 /
    # pwsh < 7.2 下 Stop + `2>&1` 会把这行 stderr 当 NativeCommandError 直接抛出,清库在查表那一步就中断 ——
    # 成败就取决于宿主版本了。这里只认 $LASTEXITCODE(下面非 0 即 throw),stderr 只是收进 $raw 供报错引用。
    # 赋值只作用于本函数作用域,不影响脚本其它部分的 Stop。
    $ErrorActionPreference = 'Continue'
    $mysqlArgs = @('-uroot', "-p$MysqlPwd", '-N', '-B')
    if (Get-Command mysql -ErrorAction SilentlyContinue) {
        $raw = @($Sql | & mysql @mysqlArgs 2>&1)
    } elseif ($HasDocker) {
        $raw = @($Sql | & docker exec -i mysql mysql @mysqlArgs 2>&1)
    } else {
        throw 'mysql client not on PATH and docker unavailable'
    }
    $code = $LASTEXITCODE
    if ($code -ne 0) {
        $errText = ($raw | ForEach-Object { "$_" }) -join ' | '
        throw "global DB ${What} failed (mysql exit ${code}): $errText"
    }
    return @($raw | Where-Object { $_ -isnot [System.Management.Automation.ErrorRecord] } |
        ForEach-Object { "$_".Trim() } | Where-Object { $_ })
}

function Clear-GlobalRouteTables {
    param([bool]$HasDocker)
    # 库名要拼进 SQL:只认普通标识符,防的是参数里带引号 / 分号。
    if ($GlobalDbName -notmatch '^[A-Za-z0-9_]+$') { throw "GlobalDbName '$GlobalDbName' is not a plain identifier" }
    $names = (@($RouteDataTables) + @('route_meta', 'route_reconcile') | ForEach-Object { "'$_'" }) -join ','
    $existing = @(Invoke-GlobalDbSql -HasDocker $HasDocker -What 'route table lookup' -Sql (
        "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = '$GlobalDbName' AND TABLE_NAME IN ($names);"))
    $stmts = @()
    foreach ($t in $RouteDataTables) {
        if ($existing -contains $t) { $stmts += "TRUNCATE TABLE $t;" }
    }
    if ($existing -contains 'route_meta') { $stmts += $RouteMetaResetSql }
    if ($existing -contains 'route_reconcile') { $stmts += $RouteReconcileResetSql }
    if ($stmts.Count -eq 0) {
        Warn "no route tables in $GlobalDbName (migration never ran) - nothing to wipe"
        return
    }
    $sql = (@("USE $GlobalDbName;") + $stmts) -join "`n"
    Invoke-GlobalDbSql -HasDocker $HasDocker -What 'route table wipe' -Sql $sql | Out-Null
    Do1 "global DB ${GlobalDbName}: $($stmts -join ' ')"
}

# ── 清库前探测映射对账 leader(D1 第 1b-3 批:docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §7 第 4 条、§13.4 第 5 条)──
# 与 currency_crash_window.ps1 里的同名常量与三个函数逐字相同,改一处两处同改(SQL 执行各用各的 Invoke-GlobalDbSql,经 -Query 传进来)。
# 为什么:RouteStore.Mode=db_first 时 data_service 的对账 leader 每拍把全局库的路由行推回 mapping Redis,并按水位键 / 世代判回档。
# 在它运行时清库:FLUSHALL 之后 leader 看到就绪标记没了,立刻全量预热,把马上要 TRUNCATE 的行推回刚清空的 Redis;随后 route_reconcile
# 被复位(watermark_ms=0)而 Redis 里的水位键还在 —— leader 判成全局库回档、冻结建角(writable=0)。两种都不报错,压测却在拿脏数据跑。
# 所以在 FLUSHALL 之前探测全局库:
#   - route_reconcile 表不存在(没跑过迁移、G1a 之前的库)= 没有 leader;
#   - lease_until_ms 为 NULL 或 ≤ 库时钟 = 没有 leader(redis 模式从不写它;leader 正常退出会释放租约);
#   - 租约未到期:可能是刚被杀掉的 leader 留下的(自然过期最长 LeaseMs),先等到期,最长 LeaseMs + 1s(缺省 11s);
#     到时仍未到期 = 有在任 leader 在续约,拒绝清库(先停 data_service,或切回 redis 模式);-AllowWithRunningLeader 放行。
# 判据用库时钟(与 data_service 的 dbNowMsSQL 同一表达式),不用本机时钟:两边的钟可以差出几秒。探测读不到(SQL 失败)即 throw,不当成没有 leader。
$RouteReconcileLeaseMs = 10000   # data_service RouteStore.Reconcile.LeaseMs 的缺省值;本机 yaml 改了它要同改这里
$RouteReconcileDbNowMsSql = 'CAST(UNIX_TIMESTAMP(NOW(3)) * 1000 AS UNSIGNED)'

# 探一次。-Query:执行一段 SQL、返回非空输出行(-N -B,tab 分隔)的脚本块,参数 (Sql, What),失败由它 throw。
# 返回 [pscustomobject]@{ TableExists; Leader; LeaseUntilMs; DbNowMs; Held; RemainingMs }。
function Get-RouteReconcileLeaderProbe {
    param(
        [Parameter(Mandatory = $true)][scriptblock]$Query,
        [Parameter(Mandatory = $true)][string]$DbName
    )
    # 库名要拼进 SQL:只认普通标识符。
    if ($DbName -notmatch '^[A-Za-z0-9_]+$') { throw "[route-leader] GlobalDbName '$DbName' is not a plain identifier" }
    $probe = [pscustomobject]@{ TableExists = $false; Leader = ''; LeaseUntilMs = [uint64]0; DbNowMs = [uint64]0; Held = $false; RemainingMs = [uint64]0 }
    $exists = @(& $Query "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = '$DbName' AND TABLE_NAME = 'route_reconcile';" 'route leader probe (table lookup)')
    if ($exists.Count -ne 1 -or $exists[0] -notmatch '^\d+$') {
        throw "[route-leader] unexpected reply to the route_reconcile table lookup: '$($exists -join ' | ')'"
    }
    if ([int]$exists[0] -eq 0) { return $probe }
    $probe.TableExists = $true
    # 行缺失(id=1 没种)按 0 / 空串:等于没有 leader。leader 放最后:为空时 -B 输出的行尾 tab 会被修剪掉。
    $sql = "SELECT $RouteReconcileDbNowMsSql, IFNULL((SELECT lease_until_ms FROM $DbName.route_reconcile WHERE id = 1), 0), " +
        "IFNULL((SELECT leader FROM $DbName.route_reconcile WHERE id = 1), '');"
    $rows = @(& $Query $sql 'route leader probe')
    if ($rows.Count -ne 1) { throw "[route-leader] expected exactly one row from the lease probe, got $($rows.Count): '$($rows -join ' | ')'" }
    $fields = @($rows[0] -split "`t")
    if ($fields.Count -lt 2 -or $fields[0] -notmatch '^\d+$' -or $fields[1] -notmatch '^\d+$') {
        throw "[route-leader] unexpected lease probe row: '$($rows[0])'"
    }
    $probe.DbNowMs = [uint64]$fields[0]
    $probe.LeaseUntilMs = [uint64]$fields[1]
    if ($fields.Count -ge 3) { $probe.Leader = $fields[2] }
    $probe.Held = $probe.LeaseUntilMs -gt $probe.DbNowMs
    if ($probe.Held) { $probe.RemainingMs = $probe.LeaseUntilMs - $probe.DbNowMs }
    return $probe
}

function Format-RouteReconcileLeaderProbe {
    param([Parameter(Mandatory = $true)]$Probe)
    if (-not $Probe.TableExists) { return 'route_reconcile table absent (no leader)' }
    if (-not $Probe.Held) { return "no running leader (lease_until_ms=$($Probe.LeaseUntilMs) db_now_ms=$($Probe.DbNowMs))" }
    return "leader '$($Probe.Leader)' holds the lease for another $($Probe.RemainingMs)ms (lease_until_ms=$($Probe.LeaseUntilMs) db_now_ms=$($Probe.DbNowMs))"
}

# 清库前的闸:没有 leader 立即放行;租约未到期就等到期(总共最长 LeaseMs + 1s)再看;仍有 leader 就 throw,-AllowRunningLeader 只告警放行。
function Assert-NoRunningRouteReconcileLeader {
    param(
        [Parameter(Mandatory = $true)][scriptblock]$Query,
        [Parameter(Mandatory = $true)][string]$DbName,
        [bool]$AllowRunningLeader = $false,
        [int]$LeaseMs = $RouteReconcileLeaseMs
    )
    $budgetMs = $LeaseMs + 1000
    $clock = [System.Diagnostics.Stopwatch]::StartNew()
    while ($true) {
        $probe = Get-RouteReconcileLeaderProbe -Query $Query -DbName $DbName
        Write-Host "[route-leader] $(Format-RouteReconcileLeaderProbe -Probe $probe)"
        if (-not $probe.Held) { return $probe }
        $leftMs = [double]($budgetMs - $clock.ElapsedMilliseconds)
        if ($leftMs -le 0) { break }
        # 等到租约到期再看(最多 1s 一次),总共不超过 LeaseMs + 1s;单调时钟,不按「次数 × sleep」算。
        $sleepMs = [Math]::Max(100.0, [Math]::Min([Math]::Min(1000.0, $leftMs), [double]$probe.RemainingMs + 100.0))
        Start-Sleep -Milliseconds ([int]$sleepMs)
    }
    $text = ("a data_service route reconcile leader is running against global DB ${DbName}: $(Format-RouteReconcileLeaderProbe -Probe $probe). " +
        "Wiping now lets it push rows back into the flushed Redis, or freeze route writers (writable=0) as if the DB had been rolled back. " +
        "Stop data_service (or switch RouteStore.Mode back to redis) first, or pass -AllowWithRunningLeader.")
    if ($AllowRunningLeader) {
        Write-Warning "[route-leader] -AllowWithRunningLeader supplied; continuing although $text"
        return $probe
    }
    throw "[route-leader] refusing to wipe: $text"
}

# ────────────────────────────────────────────────────────────────────────────
function Invoke-Wipe {
    Hdr 'Phase: wipe  [DESTRUCTIVE]  redis + mysql + etcd + kafka + logs'
    $hasDocker = [bool](Get-Command docker -ErrorAction SilentlyContinue)

    if (-not $Execute -or -not $WipeData) {
        # 计划里打印对账 leader 的探测结果(只读 SELECT,不等租约、不拒绝);真清库时同一份探测在 FLUSHALL 之前做,读不到即拒绝。
        try {
            $probe = Get-RouteReconcileLeaderProbe -DbName $GlobalDbName -Query {
                param([string]$Sql, [string]$What)
                Invoke-GlobalDbSql -Sql $Sql -What $What -HasDocker $hasDocker
            }
            Plan "leader: route reconcile probe in $GlobalDbName (read-only, now): $(Format-RouteReconcileLeaderProbe -Probe $probe)"
        }
        catch {
            Plan "leader: route reconcile probe in $GlobalDbName unavailable now ($($_.Exception.Message)); the real wipe refuses to run when it cannot probe"
        }
        Plan "leader: before FLUSHALL, wait up to $($RouteReconcileLeaseMs + 1000)ms for a stale lease, then refuse while route_reconcile.lease_until_ms > DB clock (-AllowWithRunningLeader overrides; given: $([bool]$AllowWithRunningLeader))"
        Plan 'redis : docker exec redis redis-cli FLUSHALL'
        Plan "mysql : TRUNCATE $($MySqlTables -join ', ') in $DbName"
        Plan "mysql : TRUNCATE $($RouteDataTables -join ', ') in $GlobalDbName (only tables that exist; fail-closed)"
        Plan "mysql : route_meta(id=1) writable reset in place in ${GlobalDbName}: $RouteMetaResetSql"
        Plan "mysql : route_reconcile(id=1) reset in place in ${GlobalDbName} (if the table exists): $RouteReconcileResetSql"
        Plan 'etcd  : docker exec etcd etcdctl del "" --prefix'
        Plan 'kafka : reset all consumer groups --to-latest (best-effort)'
        Plan 'logs  : prune robot/logs/stress-* (keep newest 1), bin/log/*, run/logs/*'
        if ($Execute -and -not $WipeData) {
            Warn 'wipe is gated behind BOTH -Execute AND -WipeData. Add -WipeData to actually wipe.'
        }
        return
    }

    Warn 'About to DESTROY all runtime data (redis/mysql/etcd/kafka) — this also'
    Warn 'tears down the stack currently up for the currency-crash task.'

    # 映射对账 leader 防护(理由见 Assert-NoRunningRouteReconcileLeader 上方注释):探测在 FLUSHALL 之前,有在任 leader 就拒绝。
    Do1 "route reconcile leader probe in $GlobalDbName (before FLUSHALL)"
    $null = Assert-NoRunningRouteReconcileLeader -DbName $GlobalDbName -AllowRunningLeader ([bool]$AllowWithRunningLeader) -Query {
        param([string]$Sql, [string]$What)
        Invoke-GlobalDbSql -Sql $Sql -What $What -HasDocker $hasDocker
    }

    # redis
    Do1 'redis FLUSHALL'
    if (Get-Command redis-cli -ErrorAction SilentlyContinue) { & redis-cli FLUSHALL | Out-Null }
    elseif ($hasDocker) { & docker exec redis redis-cli FLUSHALL | Out-Null }
    else { throw 'redis-cli not on PATH and docker unavailable' }

    # mysql
    Do1 "mysql TRUNCATE in $DbName"
    $sqlBody = ($MySqlTables | ForEach-Object { "TRUNCATE TABLE $_;" }) -join "`n"
    $sql = "SET FOREIGN_KEY_CHECKS=0;`n$sqlBody`nSET FOREIGN_KEY_CHECKS=1;"
    if (Get-Command mysql -ErrorAction SilentlyContinue) {
        $sql | & mysql -uroot "-p$MysqlPwd" $DbName 2>&1 | Out-Null
    } elseif ($hasDocker) {
        $sql | & docker exec -i mysql mysql -uroot "-p$MysqlPwd" $DbName 2>&1 | Out-Null
    } else { throw 'mysql client not on PATH and docker unavailable' }

    # 全局库的路由表(fail-closed,理由见 Clear-GlobalRouteTables 上方注释)
    Do1 "mysql route tables in $GlobalDbName"
    Clear-GlobalRouteTables -HasDocker $hasDocker

    # etcd — delete all keys (node-id allocs, service discovery, locks)
    Do1 'etcd del "" --prefix'
    if ($hasDocker) {
        try { & docker exec etcd etcdctl del "" --prefix 2>&1 | Out-Null }
        catch { Warn "etcd wipe skipped (non-fatal): $($_.Exception.Message)" }
    } else { Warn 'docker unavailable — skipped etcd wipe' }

    # kafka offsets — best-effort (group must be idle; non-fatal otherwise)
    Do1 'kafka reset all consumer-group offsets --to-latest'
    if ($hasDocker) {
        try {
            & docker exec kafka /opt/kafka/bin/kafka-consumer-groups.sh `
                --bootstrap-server kafka:29092 --all-groups --all-topics `
                --reset-offsets --to-latest --execute 2>&1 | Out-Null
        } catch { Warn "kafka offset reset skipped (non-fatal): $($_.Exception.Message)" }
    }

    # logs — keep newest stress dir for diffing, prune the rest
    Do1 'prune old logs'
    $stressGlob = Join-Path $RepoRoot 'robot\logs\stress-*'
    Get-ChildItem $stressGlob -Directory -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTime -Descending | Select-Object -Skip 1 |
        ForEach-Object { Remove-Item $_.FullName -Recurse -Force -ErrorAction SilentlyContinue }
    foreach ($d in @('bin\log','run\logs')) {
        $p = Join-Path $RepoRoot $d
        if (Test-Path $p) { Get-ChildItem $p -File -Recurse -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue }
    }
}

# ────────────────────────────────────────────────────────────────────────────
function Invoke-Build {
    Hdr 'Phase: build (Go services ONLY — never compiles C++)'
    if (-not $Execute) {
        Plan "pwsh $DevTools -Command go-svc-build"
        Plan 'NOTE: gate.exe/scene.exe must already be built (C++ build is manual).'
        return
    }
    Do1 'go-svc-build (login/db/scene_manager/data_service/player_locator)'
    & pwsh -NoProfile -File $DevTools -Command go-svc-build
    if ($LASTEXITCODE -ne 0) { throw 'go-svc-build failed' }
}

# ────────────────────────────────────────────────────────────────────────────
function Invoke-Start {
    Hdr 'Phase: start (dev-start-exe: Go services + cpp gate/scene from prebuilt exe)'
    if (-not $Execute) {
        Plan "pwsh $DevTools -Command dev-start-exe"
        Plan 'Also ensure Java sa-token(18080)+gateway-node(8081) are up (started separately).'
        return
    }
    Do1 'dev-start-exe'
    & pwsh -NoProfile -File $DevTools -Command dev-start-exe
}

# ────────────────────────────────────────────────────────────────────────────
function New-RunDir {
    $ts = Get-Date -Format 'yyyyMMdd-HHmmss'
    $rd = "robot/logs/stress-round19-$ts"
    New-Item -ItemType Directory -Path (Join-Path $RepoRoot $rd) -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $RepoRoot "$rd/prom-snapshots") -Force | Out-Null
    return $rd
}

function Invoke-Run {
    Hdr 'Phase: run (launch 45k robot + background stress_snap)'
    $robotDir = Join-Path $RepoRoot 'robot'
    $robotExe = Join-Path $robotDir 'robot.exe'
    $cfgFull  = Join-Path $RepoRoot $RobotConfig

    if (-not $Execute) {
        Plan "build robot.exe if missing: (cd robot; go build -o robot.exe .)"
        Plan "robot.exe -c $RobotConfig   (45k, profile=stress, fire-and-forget)"
        Plan "stress_snap.ps1 -RunDir robot/logs/stress-round19-<ts> -StartTime <now> -Stages $Stages  (detached process)"
        return
    }
    if (-not (Test-Path $cfgFull)) { throw "robot config not found: $RobotConfig" }

    if (-not (Test-Path $robotExe)) {
        Do1 'building robot.exe'
        Push-Location $robotDir
        try { & go build -o robot.exe . ; if ($LASTEXITCODE -ne 0) { throw 'robot build failed' } }
        finally { Pop-Location }
    }

    $rd = if ($RunDir) { $RunDir } else { New-RunDir }
    $rdFull = Join-Path $RepoRoot $rd
    $script:RunDir = $rd
    $startTime = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'

    # Robot: fire-and-forget detached (avoid handle-inheritance pipe deadlocks).
    Do1 "launch robot ($RobotConfig) -> $rd"
    Start-Process -FilePath $robotExe `
        -ArgumentList @('-c', $cfgFull) `
        -WorkingDirectory $robotDir `
        -RedirectStandardOutput (Join-Path $rdFull 'robot.stdout') `
        -RedirectStandardError  (Join-Path $rdFull 'robot.stderr') `
        -WindowStyle Hidden | Out-Null

    # Snapshots: detached process so it survives this orchestrator exiting.
    # (Start-Job dies with the parent pwsh process, which would silently drop
    #  every prom snapshot — must be a real detached Start-Process instead.
    #  Use -Command (not -File): the -File form exited immediately under
    #  -WindowStyle Hidden, dropping every snapshot.)
    Do1 "start detached stress_snap (stages=$Stages, start=$startTime)"
    $snapCmd  = "& '$SnapPs1' -RunDir '$rd' -StartTime '$startTime' -Stages '$Stages'"
    $snapProc = Start-Process -FilePath 'pwsh' `
        -ArgumentList @('-NoProfile', '-Command', $snapCmd) `
        -WorkingDirectory $RepoRoot `
        -WindowStyle Hidden -PassThru
    Write-Host "  snap PID = $($snapProc.Id)" -ForegroundColor DarkGray

    Write-Host "  RunDir   = $rd"          -ForegroundColor Green
    Write-Host "  StartTime= $startTime"   -ForegroundColor Green
    Write-Host "  tail: Get-Content $rd/robot.stderr -Wait" -ForegroundColor DarkGray
}

# ────────────────────────────────────────────────────────────────────────────
function Invoke-Summarize {
    Hdr 'Phase: summarize (R18 vs R19 comparison table)'
    if (-not $RunDir) { throw 'summarize requires -RunDir (the round19 run dir)' }
    if (-not $Execute) {
        Plan "pwsh $SummPs1 -RunDir $RunDir"
        Plan "Then 2D-compare against $PrevSummary (R18). PASS = token expired -> ~0,"
        Plan 'login_fail/gave-up down sharply, pipeline enter_fail=0/player store unknown=0/audit Kafka lag=0.'
        return
    }
    Do1 "stress_summarize -RunDir $RunDir"
    & pwsh -NoProfile -File $SummPs1 -RunDir $RunDir
    Write-Host "`nCompare the table above against baseline: $PrevSummary" -ForegroundColor Yellow
    Write-Host 'PASS criteria: token expired -> ~0; login_fail & gave-up down sharply;' -ForegroundColor Yellow
    Write-Host '              pipeline stays enter_fail=0 / player store unknown=0 / audit Kafka lag=0.' -ForegroundColor Yellow
    Write-Host 'Do NOT claim "fixed" without this comparison table (CLAUDE §6).'          -ForegroundColor Yellow
}

# ────────────────────────────────────────────────────────────────────────────
Write-Host "Round 19 orchestrator — phase=$Phase  Execute=$Execute  WipeData=$WipeData" -ForegroundColor Magenta
if (-not $Execute) { Write-Host '(dry-run: nothing will be changed; add -Execute to act)' -ForegroundColor DarkYellow }

switch ($Phase) {
    'plan'      { Invoke-Baseline; Invoke-Wipe; Invoke-Build; Invoke-Start; Invoke-Run; if (-not $RunDir) { $script:RunDir = 'robot/logs/stress-round19-<ts>' }; Invoke-Summarize }
    'baseline'  { Invoke-Baseline }
    'wipe'      { Invoke-Wipe }
    'build'     { Invoke-Build }
    'start'     { Invoke-Start }
    'run'       { Invoke-Run }
    'summarize' { Invoke-Summarize }
    'all' {
        Invoke-Baseline
        Invoke-Wipe
        Invoke-Build
        Invoke-Start
        if ($Execute) { Write-Host "`n[all] waiting 20s for services to settle..." -ForegroundColor DarkGray; Start-Sleep -Seconds 20 }
        Invoke-Run
        if ($Execute) {
            Write-Host "`n[all] robot running; waiting $SteadyMinutes min for steady-state before summarize..." -ForegroundColor Cyan
            Start-Sleep -Seconds ($SteadyMinutes * 60)
            Invoke-Summarize
        } else {
            if (-not $RunDir) { $script:RunDir = 'robot/logs/stress-round19-<ts>' }
            Invoke-Summarize
        }
    }
}

Write-Host "`nDone (phase=$Phase)." -ForegroundColor Magenta
