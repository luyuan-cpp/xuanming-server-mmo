<#
.SYNOPSIS
    Currency-crash-window verification driver. Runs the 4 cases (A/B/C/D)
    described in docs/notes/currency-crash-window-verification.md and writes
    the results table to the same document's "执行结果" appendix.

.DESCRIPTION
    Each case is two robot legs separated by either kill -9 + restart (A/B/C)
    or a normal LeaveGame (D):

      Leg 1 (pre):   robot logs in → reads pre balance → GmAddCurrency 10000
                     → reads post balance → exits (no LeaveGame for A/B/C
                     so the variant lives only in scene memory; D uses the
                     normal logout path inside the robot scenario).
      <kill / wait>
      Leg 2 (post):  robot logs in → reads pre balance (= "what survived")
                     → exits.

    Per case we record:
      - leg1_pre, leg1_post, leg1_delta
      - wait_seconds (actual time between leg1 exit and kill)
      - leg2_pre  (== leg2_post since add_amount=0)
      - lost      = leg1_post - leg2_pre  (gold that disappeared due to kill)
      - expected  = "lost" or "preserved" depending on case
      - pass/fail

    This script does NOT launch gate, login, db, or other go services — those
    are assumed to already be running via tools/scripts/go_services.ps1 +
    tools/scripts/cpp_nodes.ps1. It DOES launch/restart the scene node since
    that's the unit under test.

.PARAMETER Cases
    Comma-separated list of cases to run: A, B, C, D. Default: all four.

.PARAMETER Account
    Account name to use for the robot. Default: robot_0001. Each case uses a
    different account so a failed case doesn't leak state into the next.

.PARAMETER NoReset
    By default the script wipes redis/mysql/kafka offsets before each case
    (per CLAUDE.md §9.6). Pass -NoReset to skip the wipe — only useful when
    iterating the script itself; never set for a real run.

.PARAMETER OutDir
    Where to write per-leg snapshot JSONs and the summary table.
    Default: robot/logs/currency-crash-window-<yyyyMMdd-HHmmss>

.PARAMETER ScenePort
    Scene metrics port to wait on after restart (default 9150). Used to know
    when the new scene process is actually ready to accept the post-leg robot.

.PARAMETER AllowWithRunningLeader
    Wipe even though a data_service route reconcile leader still holds the lease in the
    global DB (route_reconcile.lease_until_ms > DB clock, RouteStore.Mode=db_first).
    Without it every reset probes first, waits up to LeaseMs + 1s for a stale lease to
    expire, and refuses while a leader keeps renewing (see Assert-NoRunningRouteReconcileLeader).

.EXAMPLE
    pwsh tools/scripts/currency_crash_window.ps1
    Runs all four cases with default settings.

.EXAMPLE
    pwsh tools/scripts/currency_crash_window.ps1 -Cases C
    Run only case C (accelerated cross-period via SCENE_PLAYER_SAVE_INTERVAL_SECONDS=10).
#>
param(
    [string]$Cases = "A,B,C,D",
    [string]$Account = "robot_0001",
    [switch]$NoReset,
    [string]$OutDir,
    # Deprecated/unused: scene readiness is now detected via the scene process +
    # cpp_nodes.ps1's "STARTED SUCCESSFULLY" banner, not a fixed metrics port
    # (scene ports are assigned dynamically; 9150 is scene_manager, not scene).
    # Kept only so older invocations passing -ScenePort don't error.
    [int]$ScenePort = 9150,
    # Zone whose MySQL schema (zone_<Zone>_db) gets wiped between cases. Must
    # match the robot config's zone_id (robot.currency-crash.yaml -> zone_id: 1).
    [int]$Zone = 1,
    # data_service 全局库(go/data_service/etc/data_service.yaml 的 SnapshotMySQL.DBName):
    # 映射持久化的路由表与账号目录在这里(见 Clear-GlobalRouteTables):只清 player_store 不清账号目录,
    # 下一条用例登录会读到旧角色,认领时写权行已经没了(LOAD_AND_CLAIM_NO_OWNER)。
    [string]$GlobalDbName = "mmorpg_global",
    # 全局库里还有在任的映射对账 leader(db_first)时照样清库(见 Assert-NoRunningRouteReconcileLeader;D1 第 1b-3 批)。
    [switch]$AllowWithRunningLeader
)

$ResetData = -not $NoReset

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot  = Resolve-Path (Join-Path $ScriptDir "..\..")
$RobotDir  = Join-Path $RepoRoot "robot"
$RobotExe  = Join-Path $RobotDir "robot.exe"
$RobotCfg  = Join-Path $RobotDir "etc\robot.currency-crash.yaml"

if (-not $OutDir) {
    $ts     = (Get-Date).ToString("yyyyMMdd-HHmmss")
    $OutDir = Join-Path $RepoRoot "robot\logs\currency-crash-window-$ts"
}
$null = New-Item -ItemType Directory -Force -Path $OutDir

Write-Host "Out dir: $OutDir" -ForegroundColor Cyan

# ── Helpers ────────────────────���─────────────────────────────────────────

# 全局库里映射持久化的路由表(docs/design/player-route-mapping-durability-proposal.md §8.2,第 1a 批):
# 三张数据表 TRUNCATE,两张单行表原地复位(route_meta 只复位 writable;route_reconcile 清 leader / 租约 / 水位,
# 2026-10-12 G1a 从 route_meta 拆出)。与 stress_round19.ps1 的同名函数同一口径,改一处两处同改。
#
# route_meta / route_reconcile 不 TRUNCATE:本脚本在服务运行中清库,meta 行没了之后 db_first 的建角会一直 fail-closed
# (route_meta 缺行),直到下次 -migrate;第 1b 批的对账 leader 可能在跑,对账行没了它的租约 UPDATE 会落空。
# 原地复位不换 generation。
#
# 比 Invoke-DataReset 里 zone 表的清理更严格 —— SQL 失败就 throw,不 2>&1 | Out-Null 吞掉:Redis 已 FLUSHALL 而路由表还留着
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
        [Parameter(Mandatory = $true)][string]$MysqlPwd,
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
        throw "[reset] mysql client not on PATH and docker unavailable — cannot reach global DB $GlobalDbName"
    }
    $code = $LASTEXITCODE
    if ($code -ne 0) {
        $errText = ($raw | ForEach-Object { "$_" }) -join ' | '
        throw "[reset] global DB ${What} failed (mysql exit ${code}): $errText"
    }
    return @($raw | Where-Object { $_ -isnot [System.Management.Automation.ErrorRecord] } |
        ForEach-Object { "$_".Trim() } | Where-Object { $_ })
}

function Clear-GlobalRouteTables {
    param(
        [Parameter(Mandatory = $true)][string]$MysqlPwd,
        [bool]$HasDocker
    )
    # 库名要拼进 SQL:只认普通标识符,防的是参数里带引号 / 分号。
    if ($GlobalDbName -notmatch '^[A-Za-z0-9_]+$') { throw "[reset] GlobalDbName '$GlobalDbName' is not a plain identifier" }
    $names = (@($RouteDataTables) + @('route_meta', 'route_reconcile') | ForEach-Object { "'$_'" }) -join ','
    $existing = @(Invoke-GlobalDbSql -MysqlPwd $MysqlPwd -HasDocker $HasDocker -What 'route table lookup' -Sql (
        "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = '$GlobalDbName' AND TABLE_NAME IN ($names);"))
    $stmts = @()
    foreach ($t in $RouteDataTables) {
        if ($existing -contains $t) { $stmts += "TRUNCATE TABLE $t;" }
    }
    if ($existing -contains 'route_meta') { $stmts += $RouteMetaResetSql }
    if ($existing -contains 'route_reconcile') { $stmts += $RouteReconcileResetSql }
    if ($stmts.Count -eq 0) {
        Write-Host "[reset] no route tables in $GlobalDbName (migration never ran) - nothing to wipe" -ForegroundColor DarkYellow
        return
    }
    $sql = (@("USE $GlobalDbName;") + $stmts) -join "`n"
    Invoke-GlobalDbSql -MysqlPwd $MysqlPwd -HasDocker $HasDocker -What 'route table wipe' -Sql $sql | Out-Null
    Write-Host "[reset] global DB ${GlobalDbName}: $($stmts -join ' ')" -ForegroundColor Yellow
}

# ── 清库前探测映射对账 leader(D1 第 1b-3 批:docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §7 第 4 条、§13.4 第 5 条)──
# 与 stress_round19.ps1 里的同名常量与三个函数逐字相同,改一处两处同改(SQL 执行各用各的 Invoke-GlobalDbSql,经 -Query 传进来)。
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

function Invoke-DataReset {
    param([int]$Zone = 1)
    Write-Host "[reset] flushing redis / mysql / kafka offsets (zone $Zone)..." -ForegroundColor Yellow

    # Per CLAUDE.md §9.6: every kill-test login round must wipe redis/mysql/etcd
    # before the next round. We trim the wipe set to what currency persistence
    # actually depends on so this script stays usable outside stress contexts.
    #
    # This dev box runs infra (redis/mysql/kafka) inside docker containers and
    # the host redis-cli/mysql clients are NOT installed, so we detect the host
    # CLI first and fall back to `docker exec <container>` transparently.

    $hasDocker = [bool](Get-Command docker -ErrorAction SilentlyContinue)

    # MySQL 口令先解析:FLUSHALL 之前要先探测全局库里有没有在任的映射对账 leader(理由见 Assert-NoRunningRouteReconcileLeader 上方注释)。
    $mysqlPwd = $env:MYSQL_PASSWORD
    if (-not $mysqlPwd) { $mysqlPwd = "Mmorpg#2026db" }
    $null = Assert-NoRunningRouteReconcileLeader -DbName $GlobalDbName -AllowRunningLeader ([bool]$AllowWithRunningLeader) -Query {
        param([string]$Sql, [string]$What)
        Invoke-GlobalDbSql -Sql $Sql -What $What -MysqlPwd $mysqlPwd -HasDocker $hasDocker
    }

    # Redis full flush — clears the coordination keys (owner_epoch, location, save_barrier, handoff marks).
    if (Get-Command redis-cli -ErrorAction SilentlyContinue) {
        & redis-cli FLUSHALL | Out-Null
    } elseif ($hasDocker) {
        & docker exec redis redis-cli FLUSHALL | Out-Null
    } else {
        throw "[reset] redis-cli not on PATH and docker unavailable — cannot flush redis"
    }

    # MySQL: truncate the player tables in the zone-isolated schema (zone_<N>_db;
    # saves live in player_store,
    # see repo memory db-zone-isolation). Keep table definitions (DROP would force
    # a re-init dance). We touch only the tables this verification reads.
    # ($mysqlPwd 已在函数开头解析:对账 leader 的探测要先用它。)
    $dbName = "zone_${Zone}_db"
    $sql = @"
SET FOREIGN_KEY_CHECKS=0;
TRUNCATE TABLE player_store;
TRUNCATE TABLE player_centre_database;
TRUNCATE TABLE user_accounts;
TRUNCATE TABLE account_share_database;
SET FOREIGN_KEY_CHECKS=1;
"@
    if (Get-Command mysql -ErrorAction SilentlyContinue) {
        $sql | & mysql -uroot "-p$mysqlPwd" $dbName 2>&1 | Out-Null
    } elseif ($hasDocker) {
        $sql | & docker exec -i mysql mysql -uroot "-p$mysqlPwd" $dbName 2>&1 | Out-Null
    } else {
        throw "[reset] mysql client not on PATH and docker unavailable — cannot truncate $dbName"
    }

    # 全局库的路由表(fail-closed,理由见 Clear-GlobalRouteTables 上方注释)。
    Clear-GlobalRouteTables -MysqlPwd $mysqlPwd -HasDocker $hasDocker

    # Kafka offset reset — best-effort only. Player saves do not go through Kafka;
    # the groups left are the data_service audit consumers and match results, and
    # skipping their backlog keeps the previous case's audit rows out of this one.
    # Resetting also requires the consumer group to be idle, which it isn't while
    # data_service runs, so we treat failure as non-fatal.
    if ($hasDocker) {
        try {
            & docker exec kafka /opt/kafka/bin/kafka-consumer-groups.sh `
                --bootstrap-server kafka:29092 --all-groups --all-topics `
                --reset-offsets --to-latest --execute 2>&1 | Out-Null
        } catch {
            Write-Host "[reset] kafka offset reset skipped (non-fatal): $($_.Exception.Message)" -ForegroundColor DarkYellow
        }
    }
}

function Stop-SceneProcess {
    # Kill ALL scene.exe processes (case A/B/C want the abrupt-death path).
    # cpp_nodes.ps1 also has Stop, but it does graceful cleanup of the pid
    # file — we want kill -9 semantics, not graceful, so we Stop-Process -Force.
    $procs = Get-Process scene -ErrorAction SilentlyContinue
    if (-not $procs) {
        Write-Host "[kill] no scene process running" -ForegroundColor DarkYellow
        return
    }
    foreach ($p in $procs) {
        Write-Host "[kill] scene.exe PID=$($p.Id)" -ForegroundColor Magenta
        Stop-Process -Id $p.Id -Force
    }
    # Clean up the pid file so cpp_nodes.ps1 -Command status doesn't lie.
    $pidFile = Join-Path $RepoRoot "run\pids\cpp_nodes.pid.json"
    if (Test-Path $pidFile) {
        Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
    }
}

function Start-Scene {
    param([int]$SaveIntervalSec = 0)
    if ($SaveIntervalSec -gt 0) {
        $env:SCENE_PLAYER_SAVE_INTERVAL_SECONDS = "$SaveIntervalSec"
        Write-Host "[scene] starting with SCENE_PLAYER_SAVE_INTERVAL_SECONDS=$SaveIntervalSec" -ForegroundColor DarkCyan
    } else {
        # Clear so the next start uses the default 60s.
        Remove-Item env:SCENE_PLAYER_SAVE_INTERVAL_SECONDS -ErrorAction SilentlyContinue
        Write-Host "[scene] starting with default save interval (60s)" -ForegroundColor DarkCyan
    }
    $cppNodes = Join-Path $RepoRoot "tools\scripts\cpp_nodes.ps1"

    # cpp_nodes.ps1 launches scene.exe detached and blocks until it reads
    # "STARTED SUCCESSFULLY" from the scene log (or times out). That banner is
    # the real readiness signal — scene's TCP/gRPC ports are assigned
    # dynamically via the etcd allocator (e.g. 20000/50000), so there is NO
    # fixed port to probe. (Port 9150 is the Go scene_manager's metrics port,
    # NOT scene — probing it gave an instant false-positive that let the robot
    # leg race a not-yet-existent scene.)
    #
    # The first scene.exe launch of a session can fail with "Access is denied"
    # (Defender real-time scan / a briefly-held stdout log handle); cpp_nodes.ps1
    # swallows that Start-Process error, so we verify a scene process actually
    # came up and retry the launch a few times if it didn't.
    $maxAttempts = 4
    for ($attempt = 1; $attempt -le $maxAttempts; $attempt++) {
        # IMPORTANT: launch cpp_nodes fire-and-forget — do NOT block on it.
        # cpp_nodes.ps1 starts scene.exe via Start-Process with redirected std
        # handles (CreateProcess bInheritHandles=TRUE), so the detached scene
        # inherits and holds open every inheritable handle of the launcher,
        # including its redirected stdout. Any wait that depends on that handle
        # reaching EOF (a `| Out-Host` pipe, or `Start-Process -Wait` on a
        # file-redirected child) therefore blocks for scene's entire lifetime
        # and deadlocks the driver. Instead we fire cpp_nodes and poll for the
        # scene process + its "STARTED SUCCESSFULLY" readiness banner ourselves.
        $startLog = Join-Path $OutDir "scene-start-attempt$attempt.log"
        Start-Process -FilePath "pwsh" `
            -ArgumentList @('-NoProfile','-File', $cppNodes, '-Command','start','-Nodes','scene','-SceneCount','1') `
            -WorkingDirectory $RepoRoot `
            -RedirectStandardOutput $startLog `
            -RedirectStandardError  "$startLog.err" `
            -WindowStyle Hidden | Out-Null

        # Wait for scene to come up. cpp_nodes writes "STARTED SUCCESSFULLY" to
        # the scene node log once it's fully registered; that banner (plus a
        # live scene process) is the real readiness signal.
        $deadline = (Get-Date).AddSeconds(45)
        while ((Get-Date) -lt $deadline) {
            if (Get-Process scene -ErrorAction SilentlyContinue) {
                $sceneLog = Get-ChildItem (Join-Path $RepoRoot "run\logs\cpp_nodes\scene.*.log") `
                    -ErrorAction SilentlyContinue | Sort-Object LastWriteTime | Select-Object -Last 1
                if ($sceneLog -and (Select-String -Path $sceneLog.FullName -Pattern "STARTED SUCCESSFULLY" -Quiet -ErrorAction SilentlyContinue)) {
                    # Give scene_manager a moment to observe the new scene's etcd
                    # registration before the robot leg tries to enter the scene.
                    Start-Sleep -Seconds 3
                    return
                }
            }
            Start-Sleep -Milliseconds 500
        }
        Write-Host "[scene] launch attempt $attempt did not become ready in 45s; retrying..." -ForegroundColor Yellow
        Stop-SceneProcess
        Start-Sleep -Seconds 2
    }
    throw "scene failed to start after $maxAttempts attempts"
}

function Invoke-RobotLeg {
    param(
        [string]$Account,
        [int64]$AddAmount,
        [string]$OutputPath
    )

    if (-not (Test-Path $RobotExe)) {
        Write-Host "[robot] building robot.exe..." -ForegroundColor DarkGray
        Push-Location $RobotDir
        try { go build -o robot.exe . | Out-Host } finally { Pop-Location }
    }

    # Override the snapshot config via env so we can re-use one yaml for
    # both legs of every case. The robot reads `currency_crash:` from the
    # yaml; we patch the values per leg via a dedicated env-aware override
    # by writing a small per-leg yaml on disk.
    $legCfgPath = "$OutputPath.cfg.yaml"
    $cfgTxt = Get-Content $RobotCfg -Raw
    $cfgTxt = $cfgTxt -replace 'account: "robot_\d+"', ('account: "{0}"' -f $Account)
    $cfgTxt = $cfgTxt -replace 'add_amount: \d+',      ('add_amount: {0}' -f $AddAmount)
    # Force-LF the output path so embedded backslashes don't break yaml.
    $escapedPath = $OutputPath.Replace('\','/')
    $cfgTxt = $cfgTxt -replace 'output_path:.*',       ('output_path: "{0}"' -f $escapedPath)
    Set-Content -Path $legCfgPath -Value $cfgTxt -Encoding utf8

    Push-Location $RobotDir
    try {
        & $RobotExe -c $legCfgPath | Out-Host
        if ($LASTEXITCODE -ne 0) {
            throw "robot leg exited with code $LASTEXITCODE (account=$Account add=$AddAmount)"
        }
    } finally { Pop-Location }

    if (-not (Test-Path $OutputPath)) {
        throw "robot leg did not write snapshot at $OutputPath"
    }
    return Get-Content $OutputPath -Raw | ConvertFrom-Json
}

function Invoke-CrashCase {
    param(
        [string]$Name,
        [string]$Account,
        [int]$WaitSeconds,
        [int]$SaveIntervalSec,
        [bool]$Kill,
        [string]$Expected   # "lost" or "preserved"
    )

    Write-Host "`n========== CASE $Name ($Account, wait=$WaitSeconds`s, kill=$Kill, expect $Expected) ==========" `
        -ForegroundColor Cyan

    if ($ResetData) { Invoke-DataReset -Zone $Zone }

    # Make sure scene is fresh with the right save-interval setting.
    Stop-SceneProcess
    Start-Sleep -Milliseconds 500
    Start-Scene -SaveIntervalSec $SaveIntervalSec

    $legPre  = Join-Path $OutDir ("case-$Name`_leg_pre.json")
    $legPost = Join-Path $OutDir ("case-$Name`_leg_post.json")

    # Leg 1: add 10000 gold, exit (without LeaveGame for A/B/C).
    $snap1 = Invoke-RobotLeg -Account $Account -AddAmount 10000 -OutputPath $legPre

    # Wait for the configured window to open the kill opportunity.
    if ($WaitSeconds -gt 0) {
        Write-Host "[wait] sleeping $WaitSeconds`s before kill..." -ForegroundColor DarkGray
        Start-Sleep -Seconds $WaitSeconds
    }

    if ($Kill) {
        Stop-SceneProcess
        Start-Sleep -Seconds 1
        Start-Scene -SaveIntervalSec $SaveIntervalSec
    }

    # Leg 2: read-only re-snapshot.
    $snap2 = Invoke-RobotLeg -Account $Account -AddAmount 0 -OutputPath $legPost

    $lost     = [int64]$snap1.post_balance - [int64]$snap2.pre_balance
    $passed   = $false
    if ($Expected -eq "lost") {
        # We expect the +10000 to NOT survive, so leg2_pre should equal leg1_pre
        # (i.e. nothing got persisted). Allow any positive lost up to the full
        # delta — partial save is still a "lost" outcome.
        $passed = $lost -gt 0
    } else {
        # "preserved": leg2_pre should equal leg1_post (= leg1_pre + 10000).
        $passed = ($lost -eq 0) -and ([int64]$snap2.pre_balance -eq [int64]$snap1.post_balance)
    }

    return [pscustomobject]@{
        Case        = $Name
        Account     = $Account
        Leg1Pre     = [int64]$snap1.pre_balance
        Leg1Post    = [int64]$snap1.post_balance
        WaitSeconds = $WaitSeconds
        Kill        = $Kill
        Leg2Pre     = [int64]$snap2.pre_balance
        Lost        = $lost
        Expected    = $Expected
        Passed      = $passed
        PlayerID    = [uint64]$snap1.player_id
    }
}

# ── Cases ────────────────────────────────────────────────────────────────

# A: kill 5s after add → expect loss. Pins the period to 300s: the default 60s spreads players over
#    60 one-second slots, so about one run in twelve would save inside the 5s window and turn this red.
# B: wait 350s, well past the default 60s period → expect preserved (a save tick fired).
# C: same as B but with SCENE_PLAYER_SAVE_INTERVAL_SECONDS=10 + wait 15s.
# D: no kill, normal scenario in-process — separate path; reuse case A
#    accounts but Kill=$false so leg2 reads back the freshly-added gold.
$caseList = $Cases.Split(",") | ForEach-Object { $_.Trim().ToUpper() }
$results  = @()

foreach ($c in $caseList) {
    switch ($c) {
        "A" {
            $results += Invoke-CrashCase -Name "A" -Account "robot_0001" `
                -WaitSeconds 5 -SaveIntervalSec 300 -Kill $true -Expected "lost"
        }
        "B" {
            $results += Invoke-CrashCase -Name "B" -Account "robot_0002" `
                -WaitSeconds 350 -SaveIntervalSec 0 -Kill $true -Expected "preserved"
        }
        "C" {
            $results += Invoke-CrashCase -Name "C" -Account "robot_0003" `
                -WaitSeconds 15 -SaveIntervalSec 10 -Kill $true -Expected "preserved"
        }
        "D" {
            $results += Invoke-CrashCase -Name "D" -Account "robot_0004" `
                -WaitSeconds 1 -SaveIntervalSec 0 -Kill $false -Expected "preserved"
        }
        default {
            Write-Host "Unknown case: $c (expected A/B/C/D)" -ForegroundColor Red
        }
    }
}

# ── Summary ──────────────────────────────────────────────────────────────

Write-Host "`n========== SUMMARY ==========" -ForegroundColor Green
$results | Format-Table -AutoSize Case, Account, Leg1Pre, Leg1Post, WaitSeconds, Kill, Leg2Pre, Lost, Expected, Passed

$summaryPath = Join-Path $OutDir "summary.json"
$results | ConvertTo-Json -Depth 4 | Set-Content -Path $summaryPath -Encoding utf8
Write-Host "`nSummary JSON: $summaryPath" -ForegroundColor Cyan

# Markdown table fragment ready to paste into the verification doc.
$md = New-Object System.Text.StringBuilder
[void]$md.AppendLine("| Case | Account | Leg1 Pre | Leg1 Post | Wait (s) | Kill | Leg2 Pre | Lost | Expected | Pass |")
[void]$md.AppendLine("|------|---------|----------|-----------|----------|------|----------|------|----------|------|")
foreach ($r in $results) {
    $passMark = if ($r.Passed) { "✅" } else { "❌" }
    [void]$md.AppendLine("| $($r.Case) | $($r.Account) | $($r.Leg1Pre) | $($r.Leg1Post) | $($r.WaitSeconds) | $($r.Kill) | $($r.Leg2Pre) | $($r.Lost) | $($r.Expected) | $passMark |")
}
$mdPath = Join-Path $OutDir "summary.md"
Set-Content -Path $mdPath -Value $md.ToString() -Encoding utf8
Write-Host "Summary MD : $mdPath (paste into docs/notes/currency-crash-window-verification.md)" -ForegroundColor Cyan

# Exit non-zero on any failure so CI / babysitting scripts notice.
$failCount = ($results | Where-Object { -not $_.Passed }).Count
if ($failCount -gt 0) {
    Write-Host "`n$failCount case(s) failed." -ForegroundColor Red
    exit 1
}
Write-Host "`nAll cases passed." -ForegroundColor Green
