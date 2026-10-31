<#
.SYNOPSIS
    Smoke test for tools/scripts/stress_summarize.ps1 — focuses on the
    new section 2bb (Dirty-save skip rate) added 2026-06-03 but also
    exercises the legacy login-stats parser so a regression there can't
    sneak through unnoticed.

.DESCRIPTION
    The real stress_summarize parses three sources:
      * RunDir/robot.stderr           → "[stats Xm0s]" lines
      * RunDir/prom-snapshots/*.txt   → SceneManager + DB Prom snapshots
      * run/logs/cpp_nodes/scene_*.log → "[DirtySave]" lines (new)

    This script fabricates minimal versions of all three under a temp
    directory, runs stress_summarize.ps1 against them, and asserts the
    section 2bb table contains the correct numbers parsed from the
    fake [DirtySave] lines.

    The Kafka block of stress_summarize is skipped by passing
    -KafkaContainer "" — we don't have a kafka container in the test
    sandbox and the flag explicitly silences that section.

.NOTES
    Test failures print a red FAIL line and exit 1 so this can wire
    into CI later. Success exits 0 with a green PASS line.
#>
param(
    [switch]$KeepArtifacts # leave the temp dir intact for debugging
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot  = Resolve-Path (Join-Path $ScriptDir "..\..")
$Summarize = Join-Path $ScriptDir "stress_summarize.ps1"

if (-not (Test-Path $Summarize)) {
    throw "stress_summarize.ps1 not found at $Summarize"
}

# ── Fabricate inputs ────────────────────────────────────────────────────

# 1. A throwaway RunDir with the bare minimum the parser expects.
$tmpRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("stress-summarize-test-" + [guid]::NewGuid().ToString("N"))
$runDir  = Join-Path $tmpRoot "run"
$null = New-Item -ItemType Directory -Force -Path $runDir
$null = New-Item -ItemType Directory -Force -Path (Join-Path $runDir "prom-snapshots")

# robot.stderr with one [stats Xm0s] line so the legacy parser has
# something to chew on.
$fakeStderr = @"
[stats 5m0s] conn=1000 login_ok=995 login_fail=0 login_stuck=0 enter_ok=995 enter_fail=0 msg_sent=12345(40/s) entergame_total=ok=995 fail=0 max_login=82ms recon_fb=0
"@
Set-Content -Path (Join-Path $runDir "robot.stderr") -Value $fakeStderr -Encoding utf8

$snapDirForTest = Join-Path $runDir "prom-snapshots"

# 1a. login 快照(2026-10-13,G3 评审修复):后加的指标造「有序列」与「缺序列」两种快照。
#     entergame_total{result="pool_saturated"}:PreloadPool 满、入场链没启动的次数。它不是链的终态,
#     不计入 fail% 的分母;序列缺失(不导出该取值的旧 login)必须打印 n/a,不能折成 0。
#     t18m = 新版 login;t5m = 不导出 pool_saturated 的旧 login。
#     数值都取小于 1000 的整数:Format-Number 用 N0,千分位分隔符随区域设置变。
$fakeLoginNew = @"
# HELP entergame_total EnterGame async chain terminal-state count.
# TYPE entergame_total counter
entergame_total{result="success"} 900
entergame_total{result="apply_failed"} 90
entergame_total{result="lock_lost"} 10
entergame_total{result="pool_saturated"} 7
# TYPE entergame_total_seconds histogram
entergame_total_seconds_sum{result="success"} 0.45
entergame_total_seconds_count{result="success"} 900
# TYPE entergame_apply_seconds histogram
entergame_apply_seconds_sum{result="success"} 0.18
entergame_apply_seconds_count{result="success"} 900
# TYPE login_create_player_stage_seconds histogram
login_create_player_stage_seconds_sum{stage="mint"} 0.1
login_create_player_stage_seconds_count{stage="mint"} 100
login_create_player_stage_seconds_sum{stage="name"} 0.2
login_create_player_stage_seconds_count{stage="name"} 100
login_create_player_stage_seconds_sum{stage="register"} 0.3
login_create_player_stage_seconds_count{stage="register"} 100
login_create_player_stage_seconds_sum{stage="account_write"} 0.5
login_create_player_stage_seconds_count{stage="account_write"} 100
# TYPE login_create_player_name_orphan_total counter
login_create_player_name_orphan_total 0
"@
Set-Content -Path (Join-Path $snapDirForTest "t18m_login.txt") -Value $fakeLoginNew -Encoding utf8
$fakeLoginOld = @"
# TYPE entergame_total counter
entergame_total{result="success"} 500
# TYPE login_create_player_stage_seconds histogram
login_create_player_stage_seconds_sum{stage="mint"} 0.05
login_create_player_stage_seconds_count{stage="mint"} 50
login_create_player_stage_seconds_sum{stage="name"} 0.1
login_create_player_stage_seconds_count{stage="name"} 50
login_create_player_stage_seconds_sum{stage="register"} 0.15
login_create_player_stage_seconds_count{stage="register"} 50
login_create_player_stage_seconds_sum{stage="account_write"} 0.25
login_create_player_stage_seconds_count{stage="account_write"} 50
"@
Set-Content -Path (Join-Path $snapDirForTest "t5m_login.txt") -Value $fakeLoginOld -Encoding utf8

# 1b. go/db 快照。t18m_db 是还没有存盘流量的 db:带 label 的计数器与 histogram 第一次写入之前不输出序列,
#     P1 / P2 / P3 必须打印 n/a(不能折成 0);P4 的落点库 gauge 照常出数,其余 n/a。
$fakeDbSnapshotIdle = @"
# TYPE db_placement_open_stores gauge
db_placement_open_stores 1
"@
Set-Content -Path (Join-Path $snapDirForTest "t18m_db.txt") -Value $fakeDbSnapshotIdle -Encoding utf8

# 1c. t20m_db 是有存盘流量的 db。取值约定(列串位、求和口径写错都会变红):
#     - save:结局 80 / 5 / 4 / 3 / 2 / 1 / 6 / 7 互不相同,合计 108 次;sum 0.216s → 2.0ms;
#       桶 le=0.002:60、le=0.005:108 → p99 插值到 4.9ms。
#     - claim:结局 15 / 2 / 3,合计 20 次;sum 0.2s → 10.0ms;桶 le=0.01:10、le=0.025:20 → p99 插值到 24.7ms。
#     - denied 只出现在 claim 上:按 op 过滤写错就会串到 save 那张表。
#     - row_bytes:HOT 100 行,sum 12800 → 128 字节,桶 le=256:100 → p99 插值到 253;ASSET 有 count 没有桶 → p99 n/a;
#       其余组没有序列 → n/a。
#     - db_redis_pool_wait_seconds_total 不能被当成 wait_total 加进 rpool_wait;只有 redis_client="main" 一份
#       (placement 与 main 共用 client 时 go/db 只导出 main),rpool_wait 必须是 5 而不是翻倍。
#     - sql_in_use 跨 pool 求和 3 + 1 = 4;open_err = error 6 + rejected 0;cooldown 跨 pool 求和 0 + 1 = 1。
$fakeDbSnapshotStore = @"
# TYPE db_player_store_seconds histogram
db_player_store_seconds_bucket{op="save",le="0.001"} 10
db_player_store_seconds_bucket{op="save",le="0.002"} 60
db_player_store_seconds_bucket{op="save",le="0.005"} 108
db_player_store_seconds_bucket{op="save",le="+Inf"} 108
db_player_store_seconds_sum{op="save"} 0.216
db_player_store_seconds_count{op="save"} 108
db_player_store_seconds_bucket{op="claim",le="0.005"} 5
db_player_store_seconds_bucket{op="claim",le="0.01"} 10
db_player_store_seconds_bucket{op="claim",le="0.025"} 20
db_player_store_seconds_bucket{op="claim",le="+Inf"} 20
db_player_store_seconds_sum{op="claim"} 0.2
db_player_store_seconds_count{op="claim"} 20
# TYPE db_player_store_requests_total counter
db_player_store_requests_total{op="save",result="SAVE_PLAYER_COMMITTED"} 80
db_player_store_requests_total{op="save",result="SAVE_PLAYER_EQUAL"} 5
db_player_store_requests_total{op="save",result="SAVE_PLAYER_STALE_OWNER"} 4
db_player_store_requests_total{op="save",result="SAVE_PLAYER_NOT_HOLDER"} 3
db_player_store_requests_total{op="save",result="SAVE_PLAYER_OLDER_SEQ"} 2
db_player_store_requests_total{op="save",result="SAVE_PLAYER_TOO_LARGE"} 1
db_player_store_requests_total{op="save",result="unknown_outcome"} 6
db_player_store_requests_total{op="save",result="unavailable"} 7
db_player_store_requests_total{op="claim",result="LOAD_AND_CLAIM_CLAIMED"} 15
db_player_store_requests_total{op="claim",result="LOAD_AND_CLAIM_NO_OWNER"} 2
db_player_store_requests_total{op="claim",result="denied"} 3
# TYPE db_player_store_row_bytes histogram
db_player_store_row_bytes_bucket{kind="PLAYER_STORE_KIND_HOT",le="256"} 100
db_player_store_row_bytes_bucket{kind="PLAYER_STORE_KIND_HOT",le="+Inf"} 100
db_player_store_row_bytes_sum{kind="PLAYER_STORE_KIND_HOT"} 12800
db_player_store_row_bytes_count{kind="PLAYER_STORE_KIND_HOT"} 100
db_player_store_row_bytes_sum{kind="PLAYER_STORE_KIND_ASSET"} 3000
db_player_store_row_bytes_count{kind="PLAYER_STORE_KIND_ASSET"} 10
# TYPE db_redis_pool_wait_total counter
db_redis_pool_hits_total{redis_client="main"} 900
db_redis_pool_wait_total{redis_client="main"} 5
db_redis_pool_wait_seconds_total{redis_client="main"} 0.5
db_redis_pool_timeouts_total{redis_client="main"} 1
db_redis_pool_conns{redis_client="main"} 12
# TYPE mmorpg_sql_pool_in_use_connections gauge
mmorpg_sql_pool_in_use_connections{pool="home",sql_client="db"} 3
mmorpg_sql_pool_in_use_connections{pool="extra_zone",sql_client="db"} 1
mmorpg_sql_pool_wait_count_total{pool="home",sql_client="db"} 9
mmorpg_sql_pool_wait_duration_seconds_total{pool="home",sql_client="db"} 1.5
# TYPE db_placement_open_stores gauge
db_placement_open_stores 2
db_placement_store_open_total{result="error"} 6
db_placement_store_open_total{result="ok"} 8
db_placement_store_open_total{result="rejected"} 0
db_store_cooldown_stores{pool="extra_zone"} 1
db_store_cooldown_stores{pool="home"} 0
"@
Set-Content -Path (Join-Path $snapDirForTest "t20m_db.txt") -Value $fakeDbSnapshotStore -Encoding utf8

# 2. The dirty-save scan walks run/logs/cpp_nodes under the *real*
#    repo root (not RunDir) — that's the production layout. We back up
#    any existing scene_*.log under that path, drop our fake in place,
#    and restore at the end.
$realCppLogDir = Join-Path $RepoRoot "run\logs\cpp_nodes"
$null = New-Item -ItemType Directory -Force -Path $realCppLogDir
$backupDir = Join-Path $tmpRoot "real_cpp_logs_backup"
$null = New-Item -ItemType Directory -Force -Path $backupDir

# Backup ALL existing scene*.log so the test directory only contains
# our fabricated fixture. The glob deliberately matches both the legacy
# single-instance "scene.stdout.log" and the multi-instance form
# "scene_1.log", because real and fake both need to be cleared.
$preExistingLogs = Get-ChildItem -Path $realCppLogDir -Filter 'scene*.log' -ErrorAction SilentlyContinue
foreach ($log in $preExistingLogs) {
    Move-Item -Path $log.FullName -Destination $backupDir
}

# Three monotonic [DirtySave] samples — what 90 s of scene runtime
# would look like with two 30 s snapshot ticks. The parser keeps the
# LAST sample per log file (highest, monotonic) so the assertion below
# is against the third row.
$fakeSceneLog = @"
2026-06-03 09:00:00 INFO  [PlayerSave] Player 1001 saved
2026-06-03 09:00:30 INFO  [DirtySave] total=120 skipped=80 skip_pct=66.6%
2026-06-03 09:01:00 INFO  [DirtySave] total=240 skipped=170 skip_pct=70.8%
2026-06-03 09:01:30 INFO  [DirtySave] total=360 skipped=270 skip_pct=75.0%
2026-06-03 09:02:00 INFO  [PlayerSave] Player 1042 saved
"@
$fakeLogPath = Join-Path $realCppLogDir "scene_1.log"
Set-Content -Path $fakeLogPath -Value $fakeSceneLog -Encoding utf8

# 1e. L-18 观测批(2026-10-23)的新表:每张表各造「新版有序列」与「旧版缺序列 → n/a」两种快照。
#     取值都小于 1000、各列互不相同,串列就红。
#     - t18m_login 追加 L1 / L2 的序列;t5m_login 不加(旧版 login → 两张表全 n/a)。
#       L1:BatchGetPlayerHomeZone 100 次,sum 300ms → avg 3.0ms;桶 le=2:40、le=5:99 → p99 插值到 5.0ms;code≠0 两次。
#       L2:cache 90、etcd_stale 10、没有 etcd_unsynced → 那列按族内存在规则打 0,cache% 90.0%。
Add-Content -Path (Join-Path $snapDirForTest "t18m_login.txt") -Encoding utf8 -Value @"
# TYPE rpc_client_requests_duration_ms histogram
rpc_client_requests_duration_ms_bucket{method="/data_service.DataService/BatchGetPlayerHomeZone",le="1"} 10
rpc_client_requests_duration_ms_bucket{method="/data_service.DataService/BatchGetPlayerHomeZone",le="2"} 40
rpc_client_requests_duration_ms_bucket{method="/data_service.DataService/BatchGetPlayerHomeZone",le="5"} 99
rpc_client_requests_duration_ms_bucket{method="/data_service.DataService/BatchGetPlayerHomeZone",le="10"} 100
rpc_client_requests_duration_ms_bucket{method="/data_service.DataService/BatchGetPlayerHomeZone",le="+Inf"} 100
rpc_client_requests_duration_ms_sum{method="/data_service.DataService/BatchGetPlayerHomeZone"} 300
rpc_client_requests_duration_ms_count{method="/data_service.DataService/BatchGetPlayerHomeZone"} 100
# TYPE rpc_client_requests_code_total counter
rpc_client_requests_code_total{code="0",method="/data_service.DataService/BatchGetPlayerHomeZone"} 98
rpc_client_requests_code_total{code="14",method="/data_service.DataService/BatchGetPlayerHomeZone"} 2
# TYPE login_gate_directory_reads_total counter
login_gate_directory_reads_total{source="cache"} 90
login_gate_directory_reads_total{source="etcd_stale"} 10
"@

#     - S1:t18m_sm 有 home_zone stage(sum 0.2s / 100 次 → 2.0ms;桶 le=0.002:60、le=0.005:99 → p99 5.0ms)与
#       mapped 50 / error 2(unmapped、unconfigured 按族内存在规则打 0),没有 role_lookup stage → rl 三格 n/a;
#       t5m_sm 是旧版 sm,只有原 7 个 stage 之一 → S1 全 n/a。
$fakeSmNew = @"
# TYPE scene_manager_home_zone_lookup_total counter
scene_manager_home_zone_lookup_total{outcome="mapped",zone_id="1"} 50
scene_manager_home_zone_lookup_total{outcome="error",zone_id="1"} 2
# TYPE scene_manager_enter_scene_stage_seconds histogram
scene_manager_enter_scene_stage_seconds_bucket{stage="home_zone",zone_id="1",le="0.001"} 10
scene_manager_enter_scene_stage_seconds_bucket{stage="home_zone",zone_id="1",le="0.002"} 60
scene_manager_enter_scene_stage_seconds_bucket{stage="home_zone",zone_id="1",le="0.005"} 99
scene_manager_enter_scene_stage_seconds_bucket{stage="home_zone",zone_id="1",le="0.01"} 100
scene_manager_enter_scene_stage_seconds_bucket{stage="home_zone",zone_id="1",le="+Inf"} 100
scene_manager_enter_scene_stage_seconds_sum{stage="home_zone",zone_id="1"} 0.2
scene_manager_enter_scene_stage_seconds_count{stage="home_zone",zone_id="1"} 100
"@
Set-Content -Path (Join-Path $snapDirForTest "t18m_sm.txt") -Value $fakeSmNew -Encoding utf8
$fakeSmOld = @"
# TYPE scene_manager_enter_scene_stage_seconds histogram
scene_manager_enter_scene_stage_seconds_sum{stage="dedup",zone_id="1"} 0.01
scene_manager_enter_scene_stage_seconds_count{stage="dedup",zone_id="1"} 10
"@
Set-Content -Path (Join-Path $snapDirForTest "t5m_sm.txt") -Value $fakeSmOld -Encoding utf8

#     - D1 / D2:t18m_ds 是新版 data_service。
#       home:ok 40 / not_found 5 / error 1,sum 合计 0.1s / 46 次 → avg 2.2ms(没有桶 → p99 n/a);
#       batch:ok 100(sum 0.3s → 3.0ms),not_found / error 为 0;batch_ids sum 300 / 100 → 3.0。
#       route_read_total:home_fence cache 30 / sql 3 / not_found_sql 1 / sql_error 1,batch cache 90 / sql 8
#       (batch 没有 not_found_sql 组合)→ rr_cache 120、rr_sql 12、rr_sqlerr 1。
#       D2:read ok 200 + rejected 3 + error 2 = 205 次,sum 2.05s → 10.0ms;lookup ok 300,sum 0.3s → 1.0ms;
#       其余三个 rpc 没有序列 → n/a。t5m_ds 是旧版 data_service → D1 / D2 全 n/a。
$fakeDsNew = @"
# TYPE data_service_home_zone_read_seconds histogram
data_service_home_zone_read_seconds_sum{result="ok",rpc="home"} 0.08
data_service_home_zone_read_seconds_count{result="ok",rpc="home"} 40
data_service_home_zone_read_seconds_sum{result="not_found",rpc="home"} 0.01
data_service_home_zone_read_seconds_count{result="not_found",rpc="home"} 5
data_service_home_zone_read_seconds_sum{result="error",rpc="home"} 0.01
data_service_home_zone_read_seconds_count{result="error",rpc="home"} 1
data_service_home_zone_read_seconds_sum{result="ok",rpc="batch"} 0.3
data_service_home_zone_read_seconds_count{result="ok",rpc="batch"} 100
data_service_home_zone_read_seconds_sum{result="not_found",rpc="batch"} 0
data_service_home_zone_read_seconds_count{result="not_found",rpc="batch"} 0
data_service_home_zone_read_seconds_sum{result="error",rpc="batch"} 0
data_service_home_zone_read_seconds_count{result="error",rpc="batch"} 0
# TYPE data_service_home_zone_batch_ids histogram
data_service_home_zone_batch_ids_sum 300
data_service_home_zone_batch_ids_count 100
# TYPE data_service_route_read_total counter
data_service_route_read_total{path="home_fence",result="cache"} 30
data_service_route_read_total{path="home_fence",result="sql"} 3
data_service_route_read_total{path="home_fence",result="not_found_sql"} 1
data_service_route_read_total{path="home_fence",result="sql_error"} 1
data_service_route_read_total{path="batch",result="cache"} 90
data_service_route_read_total{path="batch",result="sql"} 8
data_service_route_read_total{path="batch",result="sql_error"} 0
# TYPE data_service_account_directory_rpc_seconds histogram
data_service_account_directory_rpc_seconds_sum{result="ok",rpc="read"} 1.5
data_service_account_directory_rpc_seconds_count{result="ok",rpc="read"} 200
data_service_account_directory_rpc_seconds_sum{result="not_found",rpc="read"} 0
data_service_account_directory_rpc_seconds_count{result="not_found",rpc="read"} 0
data_service_account_directory_rpc_seconds_sum{result="rejected",rpc="read"} 0.05
data_service_account_directory_rpc_seconds_count{result="rejected",rpc="read"} 3
data_service_account_directory_rpc_seconds_sum{result="error",rpc="read"} 0.5
data_service_account_directory_rpc_seconds_count{result="error",rpc="read"} 2
data_service_account_directory_rpc_seconds_sum{result="ok",rpc="lookup"} 0.3
data_service_account_directory_rpc_seconds_count{result="ok",rpc="lookup"} 300
"@
Set-Content -Path (Join-Path $snapDirForTest "t18m_ds.txt") -Value $fakeDsNew -Encoding utf8
$fakeDsOld = @"
# TYPE data_service_route_ready_verdict_total counter
data_service_route_ready_verdict_total{verdict="trusted"} 1
"@
Set-Content -Path (Join-Path $snapDirForTest "t5m_ds.txt") -Value $fakeDsOld -Encoding utf8

#     - R1 / R2:core 在 t2m、t5m 各一份 INFO 快照,uptime 100 → 280 → 窗口 180s。
#       total_commands_processed 1000 → 19000 → ops/s 100;cpu(sys+user)1.0 → 2.8 → 1.0%;main 0.5 → 1.4 → 0.5%;
#       net in / out 各 0 → 184320 字节 → 1.0 KB/s;clients 7;used 2 MiB → 2.0;maxmemory 0 → mem% "-";
#       error replies 1 → 3 → 2;Errorstats 段在、没有 OOM → oom 0。
#       commandstats:get 500 → 9100 次(Δ8600 → 47.8/s,usec 1000 → 18200 → 2.00 µs/call),
#       mget 100 → 1900 次(Δ1800 → 10.0/s,usec 500 → 9500 → 5.00 µs/call,累计 p50 / p99 / p99.9 = 4.0 / 12.0 / 20.0)。
#     - mapping 在同两个时刻抓的是同一实例(同 run_id)→ R1 / R2 只打 same_as=core;M1 的 rd 取 mapping,数与 core 相同。
function New-FakeRedisInfo {
    param([int]$Uptime, [int]$Cmds, [string]$CpuSys, [string]$CpuUser, [string]$MainSys, [string]$MainUser,
          [int]$NetBytes, [int]$ErrReplies, [int]$GetCalls, [int]$GetUsec, [int]$MgetCalls, [int]$MgetUsec)
    return @"
# Server
redis_version:7.2.4
run_id:fixturerunid0000000000000000000000000001
uptime_in_seconds:$Uptime
# Clients
connected_clients:7
# Memory
used_memory:2097152
maxmemory:0
# Stats
total_commands_processed:$Cmds
total_net_input_bytes:$NetBytes
total_net_output_bytes:$NetBytes
rejected_connections:0
total_error_replies:$ErrReplies
# Persistence
aof_delayed_fsync:0
# CPU
used_cpu_sys:$CpuSys
used_cpu_user:$CpuUser
used_cpu_sys_main_thread:$MainSys
used_cpu_user_main_thread:$MainUser
# Commandstats
cmdstat_get:calls=$GetCalls,usec=$GetUsec,usec_per_call=2.00,rejected_calls=0,failed_calls=0
cmdstat_mget:calls=$MgetCalls,usec=$MgetUsec,usec_per_call=5.00,rejected_calls=0,failed_calls=0
# Errorstats
errorstat_ERR:count=3
# Latencystats
latency_percentiles_usec_mget:p50=4.001,p99=12.003,p99.9=20.001
latency_percentiles_usec_get:p50=1.003,p99=3.007,p99.9=5.001
"@
}
foreach ($target in @('core', 'mapping')) {
    Set-Content -Path (Join-Path $snapDirForTest "t2m_redis_$target.txt") -Encoding utf8 -Value (
        "# stress_snap target=$target spec=docker:redis captured_at=2026-10-23T00:02:00Z`n" +
        (New-FakeRedisInfo -Uptime 100 -Cmds 1000 -CpuSys '0.6' -CpuUser '0.4' -MainSys '0.3' -MainUser '0.2' -NetBytes 0 -ErrReplies 1 -GetCalls 500 -GetUsec 1000 -MgetCalls 100 -MgetUsec 500))
    Set-Content -Path (Join-Path $snapDirForTest "t5m_redis_$target.txt") -Encoding utf8 -Value (
        "# stress_snap target=$target spec=docker:redis captured_at=2026-10-23T00:05:00Z`n" +
        (New-FakeRedisInfo -Uptime 280 -Cmds 19000 -CpuSys '1.6' -CpuUser '1.2' -MainSys '0.8' -MainUser '0.6' -NetBytes 184320 -ErrReplies 3 -GetCalls 9100 -GetUsec 18200 -MgetCalls 1900 -MgetUsec 9500))
}

# ── Run stress_summarize and capture output ─────────────────────────────

Write-Host "Running stress_summarize against fabricated inputs..." -ForegroundColor Cyan
$outputLines = & pwsh -NoProfile -File $Summarize -RunDir $runDir -KafkaContainer "" 2>&1 |
                  ForEach-Object { $_.ToString() }

# ── Restore real logs before any assertion can throw ────────────────────

Remove-Item $fakeLogPath -Force -ErrorAction SilentlyContinue
foreach ($log in (Get-ChildItem -Path $backupDir -ErrorAction SilentlyContinue)) {
    Move-Item -Path $log.FullName -Destination $realCppLogDir
}

# ── Assertions ──────────────────────────────────────────────────────────

function Assert-Contains {
    param(
        [string[]]$Haystack,
        [string]$Needle,
        [string]$Reason
    )
    if (-not ($Haystack -match [regex]::Escape($Needle))) {
        Write-Host "FAIL: expected to find $Needle ($Reason)" -ForegroundColor Red
        Write-Host "── stress_summarize output ──" -ForegroundColor DarkGray
        $Haystack | ForEach-Object { Write-Host "  $_" -ForegroundColor DarkGray }
        if (-not $KeepArtifacts) { Remove-Item -Recurse -Force $tmpRoot -ErrorAction SilentlyContinue }
        exit 1
    }
    Write-Host "  ok: $Needle" -ForegroundColor DarkGreen
}

Write-Host ""
Write-Host "Assertions:" -ForegroundColor Cyan

# Section 2bb header must show up.
Assert-Contains -Haystack $outputLines `
    -Needle "Dirty-save skip rate" `
    -Reason "section 2bb header missing — was the parser introduced?"

# The LAST monotonic sample (total=360 skipped=270 → 75.0%) must appear.
# Format-Number renders 360 / 270 with comma thousands separators in
# fields padded to width 11 ("        360" / "        270"). Match the
# numbers loosely so a future formatter tweak doesn't break the test.
Assert-Contains -Haystack $outputLines `
    -Needle "scene_1.log" `
    -Reason "scene_1.log row missing in section 2bb"

Assert-Contains -Haystack $outputLines `
    -Needle "360" `
    -Reason "expected total=360 (latest sample) — parser may be reading the first instead of last sample"

Assert-Contains -Haystack $outputLines `
    -Needle "270" `
    -Reason "expected skipped=270 (latest sample)"

Assert-Contains -Haystack $outputLines `
    -Needle "75.0" `
    -Reason "expected skip_pct=75.0 (latest sample)"

# Earlier samples must NOT appear as the displayed row — that would
# mean the parser kept the FIRST match instead of the LAST.
$rendered = ($outputLines -join "`n")
if ($rendered -match "scene_1\.log\s+120\s+80") {
    Write-Host "FAIL: parser surfaced the FIRST sample (120/80) instead of the LAST (360/270)" -ForegroundColor Red
    if (-not $KeepArtifacts) { Remove-Item -Recurse -Force $tmpRoot -ErrorAction SilentlyContinue }
    exit 1
}
Write-Host "  ok: parser kept last (monotonic) sample" -ForegroundColor DarkGreen

# Legacy section sanity: the fake [stats 5m0s] line must show in
# section 1 so we know we didn't accidentally break the existing path.
Assert-Contains -Haystack $outputLines `
    -Needle "5m0s" `
    -Reason "robot stats parser regressed — fake [stats 5m0s] line did not produce a row"

# login 两个段(EnterGame / CreatePlayer)的行都以同一个快照名开头,只按行首匹配分不清是哪一段,
# 所以先按段标题定位,再取该段里指定快照的那一行。段标题只做子串匹配,不锚行首。
function Get-SectionRow {
    param(
        [string[]]$Lines,
        [string]$SectionMarker,
        [string]$SnapshotName
    )
    $inSection = $false
    foreach ($l in $Lines) {
        if ($l.Contains('=== ')) {
            $inSection = $l.Contains($SectionMarker)
            continue
        }
        if ($inSection -and $l -match ('^\s*' + [regex]::Escape($SnapshotName) + '\s')) {
            return $l
        }
    }
    return $null
}

function Assert-SectionRow {
    param(
        [string[]]$Lines,
        [string]$SectionMarker,
        [string]$SnapshotName,
        [string]$Pattern,
        [string]$Reason
    )
    $row = Get-SectionRow -Lines $Lines -SectionMarker $SectionMarker -SnapshotName $SnapshotName
    if (-not $row -or $row -notmatch $Pattern) {
        Write-Host "FAIL: $SectionMarker row for $SnapshotName does not match $Pattern ($Reason)" -ForegroundColor Red
        Write-Host "  got: $row" -ForegroundColor Red
        Write-Host "── stress_summarize output ──" -ForegroundColor DarkGray
        $Lines | ForEach-Object { Write-Host "  $_" -ForegroundColor DarkGray }
        if (-not $KeepArtifacts) { Remove-Item -Recurse -Force $tmpRoot -ErrorAction SilentlyContinue }
        exit 1
    }
    Write-Host "  ok: $SectionMarker / $SnapshotName" -ForegroundColor DarkGreen
}

function Assert-HeaderMatches {
    param(
        [string[]]$Lines,
        [string]$Pattern,
        [string]$Reason
    )
    if (-not ($Lines -match $Pattern)) {
        Write-Host "FAIL: no header line matches $Pattern ($Reason)" -ForegroundColor Red
        Write-Host "── stress_summarize output ──" -ForegroundColor DarkGray
        $Lines | ForEach-Object { Write-Host "  $_" -ForegroundColor DarkGray }
        if (-not $KeepArtifacts) { Remove-Item -Recurse -Force $tmpRoot -ErrorAction SilentlyContinue }
        exit 1
    }
    Write-Host "  ok: header $Pattern" -ForegroundColor DarkGreen
}

# EnterGame 表:pool_saturated 是最后一列。
Assert-HeaderMatches -Lines $outputLines -Pattern 'success\s+apply_failed\s+lock_lost\s+fail%\s+total\{ok\}avg\s+apply avg\s+pool_saturated\s*$' `
    -Reason "EnterGame table must keep its column order and end with the pool_saturated column"
# t18m:success 900 / apply_failed 90 / lock_lost 10 → fail% = 100/1000 = 10.0%。
# pool_saturated 的 7 不进分母:进了分母就不是 10.0%(100/1007 = 9.9%,算成失败则是 107/1007 = 10.6%)。
# 整条链 0.45s / 900 = 0.5ms,apply 段 0.18s / 900 = 0.2ms。
Assert-SectionRow -Lines $outputLines -SectionMarker 'EnterGame end-to-end' -SnapshotName 't18m_login' `
    -Pattern '^\s*t18m_login\s+900\s+90\s+10\s+10\.0%\s+0\.5ms\s+0\.2ms\s+7\s*$' `
    -Reason "pool_saturated must print the counter value as the last column and stay out of fail%"
# t5m:不导出 pool_saturated 的 login → n/a,不能折成 0;没有耗时序列 → "-"。
Assert-SectionRow -Lines $outputLines -SectionMarker 'EnterGame end-to-end' -SnapshotName 't5m_login' `
    -Pattern '^\s*t5m_login\s+500\s+0\s+0\s+0\.0%\s+-\s+-\s+n/a\s*$' `
    -Reason "a login snapshot without the pool_saturated series must print n/a, not 0"

# CreatePlayer 表:四个阶段按持锁顺序。
Assert-HeaderMatches -Lines $outputLines -Pattern 'mint\s+name\s+register\s+account_write\s+name_orphan\s*$' `
    -Reason "CreatePlayer stages table must list the four stages in lock order"
# t18m:四个阶段均值依次 1.0 / 2.0 / 3.0 / 5.0 ms,孤儿 0。四个数互不相同,列串位即红。
Assert-SectionRow -Lines $outputLines -SectionMarker 'CreatePlayer stages' -SnapshotName 't18m_login' `
    -Pattern '^\s*t18m_login\s+1\.0ms\s+2\.0ms\s+3\.0ms\s+5\.0ms\s+0\s*$' `
    -Reason "each stage avg must land in its own column"
# t5m:孤儿序列缺失但有 stage 序列 → 0(既有规则)。
Assert-SectionRow -Lines $outputLines -SectionMarker 'CreatePlayer stages' -SnapshotName 't5m_login' `
    -Pattern '^\s*t5m_login\s+1\.0ms\s+2\.0ms\s+3\.0ms\s+5\.0ms\s+0\s*$' `
    -Reason "a missing orphan series next to stage series must print 0"

# 存盘接口各段的行都以 t18m_db / t20m_db 开头(P1 / P2 / P3 / P4 各一行),一律按段标题定位。
# P1 存盘:n avg p99 | committed equal | stale not_hold unclaimed older | no_owner invalid regressed too_new too_large | unknown unavail denied。
Assert-HeaderMatches -Lines $outputLines `
    -Pattern 'n\s+avg\s+p99\s+\|\s+committed\s+equal\s+\|\s+stale\s+not_hold\s+unclaimed\s+older\s+\|\s+no_owner\s+invalid\s+regressed\s+too_new\s+too_large\s+\|\s+unknown\s+unavail\s+denied\s*$' `
    -Reason "P1 Player store save table must keep its column order"
Assert-SectionRow -Lines $outputLines -SectionMarker 'P1 Player store save' -SnapshotName 't20m_db' `
    -Pattern '^\s*t20m_db\s+\|\s+108\s+2\.0ms\s+4\.9ms\s+\|\s+80\s+5\s+\|\s+4\s+3\s+0\s+2\s+\|\s+0\s+0\s+0\s+0\s+1\s+\|\s+6\s+7\s+0\s*$' `
    -Reason "latency and each save outcome must land in their own columns; outcomes without a series print 0"
Assert-SectionRow -Lines $outputLines -SectionMarker 'P1 Player store save' -SnapshotName 't18m_db' `
    -Pattern '^\s*t18m_db\s+\|(\s+n/a){3}\s+\|(\s+n/a){2}\s+\|(\s+n/a){4}\s+\|(\s+n/a){5}\s+\|(\s+n/a){3}\s*$' `
    -Reason "a db without any save series must print n/a, not 0"

# P2 认领:n avg p99 | claimed | stale not_hold no_owner invalid too_new | unknown unavail denied。
Assert-HeaderMatches -Lines $outputLines `
    -Pattern 'n\s+avg\s+p99\s+\|\s+claimed\s+\|\s+stale\s+not_hold\s+no_owner\s+invalid\s+too_new\s+\|\s+unknown\s+unavail\s+denied\s*$' `
    -Reason "P2 Player store claim table must keep its column order"
Assert-SectionRow -Lines $outputLines -SectionMarker 'P2 Player store claim' -SnapshotName 't20m_db' `
    -Pattern '^\s*t20m_db\s+\|\s+20\s+10\.0ms\s+24\.7ms\s+\|\s+15\s+\|\s+0\s+0\s+2\s+0\s+0\s+\|\s+0\s+0\s+3\s*$' `
    -Reason "claim rows must read op=claim only: save outcomes must not leak in"
Assert-SectionRow -Lines $outputLines -SectionMarker 'P2 Player store claim' -SnapshotName 't18m_db' `
    -Pattern '^\s*t18m_db\s+\|(\s+n/a){3}\s+\|\s+n/a\s+\|(\s+n/a){5}\s+\|(\s+n/a){3}\s*$' `
    -Reason "a db without any claim series must print n/a, not 0"

# P3 行大小:五组,每组 n avg p99,单位字节。
Assert-HeaderMatches -Lines $outputLines -Pattern 'hot_n\s+hot_avg\s+hot_p99\s+\|\s+asset_n\s+asset_avg\s+asset_p99\s+\|.*legacy1_p99\s*$' `
    -Reason "P3 Player store row bytes table must list the five kinds"
Assert-SectionRow -Lines $outputLines -SectionMarker 'P3 Player store row bytes' -SnapshotName 't20m_db' `
    -Pattern '^\s*t20m_db\s+\|\s+100\s+128\s+253\s+\|\s+10\s+300\s+n/a\s+\|(\s+n/a){3}\s+\|(\s+n/a){3}\s+\|(\s+n/a){3}\s*$' `
    -Reason "per-kind n / avg bytes / interpolated p99; no buckets print p99 n/a; missing kinds print n/a"
Assert-SectionRow -Lines $outputLines -SectionMarker 'P3 Player store row bytes' -SnapshotName 't18m_db' `
    -Pattern '^\s*t18m_db(\s+\|(\s+n/a){3}){5}\s*$' `
    -Reason "a db without committed rows must print n/a"

# P4 连接池与落点:rpool_wait 只有 main 一份 = 5(不翻倍,也没把 wait_seconds_total 的 0.5 加进来)。
Assert-HeaderMatches -Lines $outputLines `
    -Pattern 'rpool_wait\s+rpool_tmo\s+\|\s+sql_in_use\s+sql_wait\s+sql_wait_s\s+\|\s+open_stores\s+open_err\s+cooldown\s*$' `
    -Reason "P4 DB pools table must keep its column order"
Assert-SectionRow -Lines $outputLines -SectionMarker 'P4 DB pools' -SnapshotName 't20m_db' `
    -Pattern '^\s*t20m_db\s+\|\s+5\s+1\s+\|\s+4\s+9\s+1\.5\s+\|\s+2\s+6\s+1\s*$' `
    -Reason "redis pool counters, SQL pool sums across pools and placement counters must land in their own columns"
Assert-SectionRow -Lines $outputLines -SectionMarker 'P4 DB pools' -SnapshotName 't18m_db' `
    -Pattern '^\s*t18m_db\s+\|\s+n/a\s+n/a\s+\|(\s+n/a){3}\s+\|\s+1\s+n/a\s+n/a\s*$' `
    -Reason "the gauge that is present prints its value, missing series print n/a"

# ── L-18 观测批的新表(造数见 1e) ────────────────────────────────────────

# 一段里任意一行匹配(同一快照在 D2 / R2 里有多行,Get-SectionRow 只取第一行)。
function Assert-SectionAnyRow {
    param([string[]]$Lines, [string]$SectionMarker, [string]$Pattern, [string]$Reason)
    $inSection = $false
    foreach ($l in $Lines) {
        if ($l.Contains('=== ')) { $inSection = $l.Contains($SectionMarker); continue }
        if ($inSection -and $l -match $Pattern) { Write-Host "  ok: $SectionMarker any-row $Pattern" -ForegroundColor DarkGreen; return }
    }
    Write-Host "FAIL: no $SectionMarker row matches $Pattern ($Reason)" -ForegroundColor Red
    Write-Host "── stress_summarize output ──" -ForegroundColor DarkGray
    $Lines | ForEach-Object { Write-Host "  $_" -ForegroundColor DarkGray }
    if (-not $KeepArtifacts) { Remove-Item -Recurse -Force $tmpRoot -ErrorAction SilentlyContinue }
    exit 1
}

Assert-HeaderMatches -Lines $outputLines -Pattern 'bgz_n\s+bgz_avg\s+bgz_p99\s+bgz_err\s+\|.*lar_err\s*$' `
    -Reason "L1 header must list the five login→data_service methods, four columns each"
Assert-SectionRow -Lines $outputLines -SectionMarker 'L1 Login outbound RPC' -SnapshotName 't18m_login' `
    -Pattern '^\s*t18m_login\s+\|\s+100\s+3\.0ms\s+5\.0ms\s+2\s+\|\s+n/a\s+n/a\s+n/a\s+n/a\s+\|' `
    -Reason "BatchGetPlayerHomeZone: n=100, avg 3.0ms, p99 interpolated to 5.0ms, err = non-zero codes; missing methods print n/a"
Assert-SectionRow -Lines $outputLines -SectionMarker 'L1 Login outbound RPC' -SnapshotName 't5m_login' `
    -Pattern '^\s*t5m_login(\s+\|(\s+n/a){4}){5}\s*$' `
    -Reason "an old login without rpc_client_* series must print n/a everywhere"

Assert-SectionRow -Lines $outputLines -SectionMarker 'L2 Login gate directory' -SnapshotName 't18m_login' `
    -Pattern '^\s*t18m_login\s+\|\s+90\s+0\s+10\s+\|\s+90\.0%\s*$' `
    -Reason "family-present rule: missing etcd_unsynced prints 0; cache% = 90/100"
Assert-SectionRow -Lines $outputLines -SectionMarker 'L2 Login gate directory' -SnapshotName 't5m_login' `
    -Pattern '^\s*t5m_login\s+\|\s+n/a\s+n/a\s+n/a\s+\|\s+n/a\s*$' `
    -Reason "no series at all prints n/a"

Assert-SectionRow -Lines $outputLines -SectionMarker 'S1 SceneManager' -SnapshotName 't18m_sm' `
    -Pattern '^\s*t18m_sm\s+\|\s+50\s+0\s+0\s+2\s+\|\s+100\s+2\.0ms\s+5\.0ms\s+\|\s+n/a\s+n/a\s+n/a\s*$' `
    -Reason "home_zone stage n/avg/p99 summed across zone_id; role_lookup absent prints n/a"
Assert-SectionRow -Lines $outputLines -SectionMarker 'S1 SceneManager' -SnapshotName 't5m_sm' `
    -Pattern '^\s*t5m_sm\s+\|(\s+n/a){4}\s+\|(\s+n/a){3}\s+\|(\s+n/a){3}\s*$' `
    -Reason "an old scene_manager without the new stages prints n/a"

Assert-SectionRow -Lines $outputLines -SectionMarker 'D1 data_service home-zone' -SnapshotName 't18m_ds' `
    -Pattern '^\s*t18m_ds\s+\|\s+40\s+5\s+1\s+2\.2ms\s+n/a\s+\|\s+100\s+0\s+0\s+3\.0ms\s+n/a\s+\|\s+3\.0\s+\|\s+120\s+12\s+1\s*$' `
    -Reason "per-result counts, merged avg, no buckets → p99 n/a; route_read sums both paths, batch has no not_found_sql"
Assert-SectionRow -Lines $outputLines -SectionMarker 'D1 data_service home-zone' -SnapshotName 't5m_ds' `
    -Pattern '^\s*t5m_ds(\s+\|(\s+n/a){5}){2}\s+\|\s+n/a\s+\|(\s+n/a){3}\s*$' `
    -Reason "an old data_service prints n/a everywhere"

Assert-SectionRow -Lines $outputLines -SectionMarker 'D2 data_service account directory' -SnapshotName 't18m_ds' `
    -Pattern '^\s*t18m_ds\s+\|\s+read\s+\|\s+205\s+10\.0ms\s+n/a\s+\|\s+0\s+3\s+2\s*$' `
    -Reason "read row comes first: n merges all results, nf/rej/err per result"
Assert-SectionAnyRow -Lines $outputLines -SectionMarker 'D2 data_service account directory' `
    -Pattern '^\s*t18m_ds\s+\|\s+lookup\s+\|\s+300\s+1\.0ms\s+n/a\s+\|\s+n/a\s+n/a\s+n/a\s*$' `
    -Reason "lookup has only ok series → non-ok counts n/a"
Assert-SectionAnyRow -Lines $outputLines -SectionMarker 'D2 data_service account directory' `
    -Pattern '^\s*t18m_ds\s+\|\s+create\s+\|(\s+n/a){3}\s+\|(\s+n/a){3}\s*$' `
    -Reason "a rpc without series prints n/a"

Assert-SectionRow -Lines $outputLines -SectionMarker 'R1 Redis instance load' -SnapshotName 'core' `
    -Pattern '^\s*core\s+\|\s+t2m-t5m\s+\|\s+180\s+100\s+1\.0%\s+0\.5%\s+1\.0\s+1\.0\s+\|\s+7\s+2\.0\s+-\s+\|\s+0\s+2\s+0\s+0\s+\|\s*$' `
    -Reason "window by Δuptime; ops/s, cpu%, main%, KB/s; maxmemory 0 → mem% '-'"
Assert-SectionRow -Lines $outputLines -SectionMarker 'R1 Redis instance load' -SnapshotName 'mapping' `
    -Pattern '^\s*mapping\s+\|\s+t2m-t5m\s+\|.*same_as=core\s*$' `
    -Reason "the same instance captured twice is printed once"

Assert-SectionRow -Lines $outputLines -SectionMarker 'R2 Redis top commands' -SnapshotName 'core' `
    -Pattern '^\s*core\s+\|\s+t2m-t5m\s+\|\s+get\s+\|\s+47\.8\s+2\.00\s+\|\s+1\.0\s+3\.0\s+5\.0\s+\|\s+0\s+0\s*$' `
    -Reason "top command by Δcalls first"
Assert-SectionAnyRow -Lines $outputLines -SectionMarker 'R2 Redis top commands' `
    -Pattern '^\s*core\s+\|\s+t2m-t5m\s+\|\s+mget\s+\|\s+10\.0\s+5\.00\s+\|\s+4\.0\s+12\.0\s+20\.0\s+\|\s+0\s+0\s*$' `
    -Reason "mget: Δcalls/s, Δusec per call, cumulative latency percentiles"

Assert-SectionRow -Lines $outputLines -SectionMarker 'M1 Mapping read path' -SnapshotName 't18m' `
    -Pattern '^\s*t18m\s+\|\s+3\.0ms\s+n/a\s+\|\s+3\.0ms\s+2\.2ms\s+10\.0ms\s+\|\s+2\.0ms\s+n/a\s+\|\s+n/a\s+n/a\s+\|\s+n/a\s+n/a\s*$' `
    -Reason "M1 joins login / ds / sm / db by tag; no Redis snapshot at t18m"
Assert-SectionRow -Lines $outputLines -SectionMarker 'M1 Mapping read path' -SnapshotName 't5m' `
    -Pattern '^\s*t5m(\s+\|\s+n/a\s+n/a){1}\s+\|(\s+n/a){3}\s+\|(\s+n/a){2}\s+\|(\s+n/a){2}\s+\|\s+100\s+0\.5%\s*$' `
    -Reason "rd uses the mapping target window ending at t5m"
Assert-SectionRow -Lines $outputLines -SectionMarker 'M1 Mapping read path' -SnapshotName 't20m' `
    -Pattern '^\s*t20m\s+\|\s+n/a\s+n/a\s+\|(\s+n/a){3}\s+\|(\s+n/a){2}\s+\|\s+10\.0ms\s+2\.0ms\s+\|\s+n/a\s+n/a\s*$' `
    -Reason "db_claim / db_save come from db_player_store_seconds{op}"

# ── Cleanup ─────────────────────────────────────────────────────────────

if (-not $KeepArtifacts) {
    Remove-Item -Recurse -Force $tmpRoot -ErrorAction SilentlyContinue
} else {
    Write-Host "`nArtifacts kept under $tmpRoot" -ForegroundColor DarkGray
}

Write-Host ""
Write-Host "PASS" -ForegroundColor Green
