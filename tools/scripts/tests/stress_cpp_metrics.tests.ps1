#requires -Version 7
<#
.SYNOPSIS
    压测抓取与汇总的 C++ 节点指标、审计管道与登录排队三张表(2026-11-12)的契约测试。
.DESCRIPTION
    docs/stress/stress-gap-items-verification-20261111.md §4。全部在本进程里跑,不起任何服务、不访问网络之外的东西:
      - lib/cpp_metrics_ports.ps1 的端口规则(cpp_nodes.ps1 起进程与 stress_snap.ps1 抓取共用):固定样例与非法输入;
      - stress_snap.ps1 -CppMetricsBasePort 按 PID 文件推出的抓取地址(只断言 job.log 的「cpp endpoints:」行,
        不依赖端口上有没有进程在听);PID 文件缺失、Endpoints 键用 cpp_ 前缀都在抓取之前拒绝;
      - stress_summarize.ps1 的 $cppCommandTargets / $cppFanoutKinds / $cppReleaseStages 与 C++ 源码里写出的 label 取值、
        指标族名一致(C++ 侧没有 Go 那样的钉名测试,只能按源码文本对);
      - 用一组最小快照跑 stress_summarize.ps1,A1 / Q1 / N1 三张表的行按预期出数。
.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/stress_cpp_metrics.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$snapScript = Join-Path $repoRoot 'tools/scripts/stress_snap.ps1'
$summarizeScript = Join-Path $repoRoot 'tools/scripts/stress_summarize.ps1'
. (Join-Path $repoRoot 'tools/scripts/lib/cpp_metrics_ports.ps1')
$tmpRoot = Join-Path ([IO.Path]::GetTempPath()) ('stress_cpp_metrics_' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $tmpRoot
$pastStart = (Get-Date).AddMinutes(-5).ToString('yyyy-MM-dd HH:mm:ss')

function New-TempDir {
    $dir = Join-Path $tmpRoot ([guid]::NewGuid().ToString('N'))
    $null = New-Item -ItemType Directory -Path $dir
    return $dir
}

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return [IO.File]::ReadAllText((Join-Path $repoRoot $RelativePath)) -replace "`r`n", "`n"
}

# stress_summarize.ps1 里 `$<name> = @( … )` 的字符串数组(与 Go 钉名测试同一读法)。
function Get-SummarizeList {
    param([Parameter(Mandatory = $true)][string]$Name)
    $text = Read-RepoText 'tools/scripts/stress_summarize.ps1'
    $m = [regex]::Match($text, '(?s)\$' + [regex]::Escape($Name) + '\s*=\s*@\(([^)]*)\)')
    if (-not $m.Success) { throw "`$$Name list not found in stress_summarize.ps1" }
    return @([regex]::Matches($m.Groups[1].Value, "'([a-z_]+)'") | ForEach-Object { $_.Groups[1].Value })
}

try {
    Test-Case '端口规则:基准 + 1000 × zone + 节点偏移 + 序号;没有 zone 前缀算 0,没有序号算 1' {
        foreach ($case in @(
            @{ Key = 'gate'; Port = 19401 }
            @{ Key = 'gate_2'; Port = 19402 }
            @{ Key = 'scene_2'; Port = 19502 }
            @{ Key = 'battle_99'; Port = 19699 }
            @{ Key = 'z1_scene_1'; Port = 20501 }
            @{ Key = 'z2_battle'; Port = 21601 }
            @{ Key = 'z9_gate_99'; Port = 28499 }
        )) {
            Assert-Equal -Expected $case.Port -Actual (Get-CppMetricsPort -BasePort 19400 -Key $case.Key) -Because "实例键 $($case.Key)"
        }
    }

    Test-Case '端口规则:认不出的实例键、越界的序号与 zone、越界的基准一律报错,不猜' {
        foreach ($key in @('robot', 'Scene', 'gate-1', 'scene_0', 'scene_100', 'z10_gate', 'z1_', '')) {
            $thrown = $null
            try { $null = Get-CppMetricsPort -BasePort 19400 -Key $key } catch { $thrown = $_.Exception.Message }
            Assert-True -Condition ($null -ne $thrown) -Because "实例键 '$key' 必须被拒绝"
        }
        foreach ($base in @(80, 1023, 55001, 65000)) {
            $thrown = $null
            try { $null = Get-CppMetricsPort -BasePort $base -Key 'gate' } catch { $thrown = $_.Exception.Message }
            Assert-True -Condition ($null -ne $thrown) -Because "基准端口 $base 必须被拒绝"
        }
    }

    Test-Case 'stress_snap -CppMetricsBasePort:按 PID 文件的实例键推出 http://127.0.0.1:<端口>/metrics,-CppEndpoints 覆盖同名的' {
        $runDir = New-TempDir
        $pidFile = Join-Path (New-TempDir) 'cpp_nodes.pid.json'
        Set-Content -LiteralPath $pidFile -Value '{ "gate": 101, "scene_2": 102, "z1_scene_1": 103 }' -Encoding UTF8
        & $snapScript -RunDir $runDir -StartTime $pastStart -Stages '0' -Endpoints @{} -RedisTargets @{} `
            -CppMetricsBasePort 19400 -CppPidFile $pidFile -CppEndpoints @{ 'scene_2' = 'http://127.0.0.1:1/metrics' }
        $log = Get-Content -LiteralPath (Join-Path $runDir 'prom-snapshots/job.log') -Raw
        Assert-Match -Text $log -Pattern ([regex]::Escape('cpp endpoints: gate=http://127.0.0.1:19401/metrics scene_2=http://127.0.0.1:1/metrics z1_scene_1=http://127.0.0.1:20501/metrics')) -Because '按实例键排序,显式地址覆盖推出来的'
        Assert-Match -Text $log -Pattern 't0m cpp_gate (saved|FAIL)' -Because '每个实例每个时刻抓一次(端口上没有进程时记 FAIL、不中断)'
    }

    Test-Case 'stress_snap:PID 文件缺失、Endpoints 键用 cpp_ 前缀、-CppEndpoints 键认不出,都在抓取与建目录之前拒绝' {
        foreach ($case in @(
            @{ Label = 'PID 文件缺失'; Args = @{ CppMetricsBasePort = 19400; CppPidFile = (Join-Path $tmpRoot 'no-such.pid.json') } }
            @{ Label = 'Endpoints 键 cpp_x'; Args = @{ Endpoints = @{ 'cpp_x' = 'http://127.0.0.1:1/metrics' } } }
            @{ Label = 'CppEndpoints 键 robot'; Args = @{ CppEndpoints = @{ 'robot' = 'http://127.0.0.1:1/metrics' } } }
        )) {
            $runDir = Join-Path $tmpRoot ([guid]::NewGuid().ToString('N'))
            $params = @{ RunDir = $runDir; StartTime = $pastStart; Stages = '0'; RedisTargets = @{} }
            if (-not $case.Args.ContainsKey('Endpoints')) { $params['Endpoints'] = @{} }
            foreach ($k in $case.Args.Keys) { $params[$k] = $case.Args[$k] }
            $thrown = $null
            try { & $snapScript @params } catch { $thrown = $_.Exception.Message }
            Assert-True -Condition ($null -ne $thrown) -Because "$($case.Label) 必须报错"
            Assert-True -Condition (-not (Test-Path -LiteralPath (Join-Path $runDir 'prom-snapshots'))) -Because "$($case.Label):报错在建 prom-snapshots 目录之前"
        }
    }

    Test-Case 'stress_summarize 的 C++ label 取值与指标族名对得上 C++ 源码' {
        $commandStats = Read-RepoText 'cpp/libs/engine/core/node/system/node/node_command_consume_stats.h'
        $fanoutStats = Read-RepoText 'cpp/libs/engine/core/network/broadcast_fanout_stats.h'
        $ownership = Read-RepoText 'cpp/libs/services/scene/world/scene_ownership_reconciler.cpp'
        $summarize = Read-RepoText 'tools/scripts/stress_summarize.ps1'

        Assert-Equal -Expected 'own,foreign' -Actual ((Get-SummarizeList 'cppCommandTargets') -join ',') -Because 'N1 按 target 取 own / foreign'
        Assert-Match -Text $commandStats -Pattern 'BeginFamily\("node_command_consumed_total"' -Because '族名'
        foreach ($target in Get-SummarizeList 'cppCommandTargets') {
            Assert-Match -Text $commandStats -Pattern ([regex]::Escape("{""target"", ""$target""}")) -Because "C++ 写出 target=$target"
        }
        Assert-Match -Text $commandStats -Pattern ([regex]::Escape('{"topic", ')) -Because 'N1 按 topic 基名的 -cmd 后缀挑命令 topic'

        $kinds = Get-SummarizeList 'cppFanoutKinds'
        Assert-Match -Text $fanoutStats -Pattern ('(?s)kKindNames\{\s*' + (($kinds | ForEach-Object { '"' + $_ + '",' }) -join '\s*')) -Because 'kind 取值与 kKindNames 同序'
        Assert-Match -Text $fanoutStats -Pattern 'BeginFamily\("scene_broadcast_gate_fanout"' -Because '族名'

        foreach ($stage in Get-SummarizeList 'cppReleaseStages') {
            Assert-Match -Text $ownership -Pattern ([regex]::Escape("{{""stage"", ""$stage""}}")) -Because "C++ 写出 stage=$stage"
        }
        foreach ($family in @('scene_ownership_releases_total', 'scene_ownership_tracked_scenes', 'scene_ownership_releasing_scenes',
                'scene_ownership_release_deferred_scenes', 'scene_ownership_superseded')) {
            Assert-Match -Text $ownership -Pattern ('BeginFamily\("' + $family + '"') -Because "C++ 写出 $family"
            Assert-Match -Text $summarize -Pattern ("'" + $family + "'") -Because "N1 读 $family"
        }
        foreach ($family in @('node_command_consumed_total', 'scene_broadcast_gate_fanout')) {
            Assert-Match -Text $summarize -Pattern ("'" + $family + "'") -Because "N1 读 $family"
        }
        $snap = Read-RepoText 'tools/scripts/stress_snap.ps1'
        Assert-Match -Text $snap -Pattern ([regex]::Escape('"${tag}_cpp_${key}.txt"')) -Because 'stress_snap 写 t<N>m_cpp_<实例键>.txt'
        Assert-Match -Text $summarize -Pattern ([regex]::Escape("'^t\d+m_cpp_[a-z0-9_]+$'")) -Because 'stress_summarize 按同一个文件名形状认 C++ 快照'
    }

    Test-Case 'stress_summarize:最小快照下 A1 / Q1 / N1 按预期出数,C++ 快照不被当成 login 快照' {
        $runDir = New-TempDir
        Set-Content -LiteralPath (Join-Path $runDir 'robot.stderr') -Value '' -Encoding UTF8
        $snapDir = Join-Path $runDir 'prom-snapshots'
        $null = New-Item -ItemType Directory -Path $snapDir
        Set-Content -LiteralPath (Join-Path $snapDir 't5m_ds.txt') -Encoding UTF8 -Value @(
            'data_service_kafka_consumer_messages_total{consumer="transaction_log",outcome="inserted"} 10'
            'data_service_kafka_consumer_messages_total{consumer="transaction_log",outcome="duplicate"} 2'
            'data_service_kafka_consumer_messages_total{consumer="transaction_log",outcome="decode_error"} 1'
            'data_service_kafka_consumer_messages_total{consumer="transaction_log",outcome="invalid"} 0'
            'data_service_kafka_consumer_messages_total{consumer="transaction_log",outcome="oversize"} 0'
            'data_service_kafka_consumer_messages_total{consumer="transaction_log",outcome="db_error"} 0'
            'data_service_kafka_consumer_message_bytes_bucket{consumer="transaction_log",le="1024"} 13'
            'data_service_kafka_consumer_message_bytes_bucket{consumer="transaction_log",le="+Inf"} 13'
            'data_service_kafka_consumer_message_bytes_sum{consumer="transaction_log"} 2600'
            'data_service_kafka_consumer_message_bytes_count{consumer="transaction_log"} 13'
            'data_service_kafka_consumer_lag_max_partition_messages{consumer="transaction_log"} 7'
            'data_service_kafka_consumer_retention_gap_messages{consumer="transaction_log"} 0'
            'data_service_audit_retention_deleted_rows_total{table="transaction_log"} 42'
            'data_service_audit_retention_failures_total{table="transaction_log"} 0'
        )
        Set-Content -LiteralPath (Join-Path $snapDir 't5m_login.txt') -Encoding UTF8 -Value @(
            'login_queue_size{zone="1"} 300'
            'login_queue_size{zone="2"} 200'
            'login_queue_admitted_inflight{zone="1"} 5'
            'login_queue_capacity{zone="1"} 900'
            'login_queue_admit_total{zone="1"} 120'
            'login_queue_expired_total{zone="1",reason="admit_window_passed"} 3'
            'login_queue_wait_seconds_bucket{zone="1",le="10"} 100'
            'login_queue_wait_seconds_bucket{zone="1",le="+Inf"} 120'
            'login_queue_wait_seconds_sum{zone="1"} 900'
            'login_queue_wait_seconds_count{zone="1"} 120'
        )
        Set-Content -LiteralPath (Join-Path $snapDir 't5m_cpp_scene_1.txt') -Encoding UTF8 -Value @(
            'node_command_consumed_total{node_type="scene",topic="scene-cmd",target="own"} 40'
            'node_command_consumed_total{node_type="scene",topic="scene-cmd",target="foreign"} 120'
            'node_command_consumed_total{node_type="scene",topic="chat-world-bcast",target="own"} 999'
            'scene_broadcast_gate_fanout_bucket{kind="players",le="1"} 10'
            'scene_broadcast_gate_fanout_bucket{kind="players",le="2"} 30'
            'scene_broadcast_gate_fanout_bucket{kind="players",le="4"} 40'
            'scene_broadcast_gate_fanout_bucket{kind="players",le="+Inf"} 40'
            'scene_broadcast_gate_fanout_sum{kind="players"} 80'
            'scene_broadcast_gate_fanout_count{kind="players"} 40'
            'scene_ownership_releases_total{stage="drain_started"} 2'
            'scene_ownership_releases_total{stage="destroyed"} 1'
            'scene_ownership_tracked_scenes 12'
            'scene_ownership_releasing_scenes 1'
            'scene_ownership_release_deferred_scenes 0'
            'scene_ownership_superseded 0'
        )

        $out = & $summarizeScript -RunDir $runDir -KafkaContainer '' 6>&1 | Out-String
        Assert-Match -Text $out -Pattern 't5m_ds\s+\|\s+transaction_log\s+\|\s+13\s+10\s+2\s+1\s+\|\s+13\s+200\s+\S+\s+\|\s+7\s+0\s+\|\s+42\s+0' -Because 'A1:msgs 13 = 10 + 2 + 1;bad = decode_error;b_avg = 2600 / 13;lag_max、gap、del、del_err 原样'
        Assert-Match -Text $out -Pattern 't5m_ds\s+\|\s+player_snapshot\s+\|\s+n/a' -Because 'A1:这一条流整族没有序列打 n/a'
        Assert-Match -Text $out -Pattern 't5m_login\s+\|\s+500\s+5\s+900\s+\|\s+120\s+\|\s+0\s+0\s+0\s+0\s+3\s+0\s+\|\s+120\s+' -Because 'Q1:size 跨 zone 求和;expired 族在、缺的 reason 记 0;w_n = 120'
        Assert-Match -Text $out -Pattern 't5m_cpp_scene_1\s+\|\s+scene\s+\|\s+40\s+120\s+3\.0\s+\|\s+40\s+\S+\s+\S+\s+\|\s+2\s+1\s+\|\s+12\s+1\s+0\s+0' -Because 'N1:只数 -cmd 的 topic(广播 topic 的 999 不算);for/own = 3.0'

        # 没有任何 _login 快照时,不带已知后缀的 txt 才按 legacy 当 login 快照:C++ 快照不能被卷进去,只该出现在 N1 那一行。
        $cppOnly = New-TempDir
        Set-Content -LiteralPath (Join-Path $cppOnly 'robot.stderr') -Value '' -Encoding UTF8
        $null = New-Item -ItemType Directory -Path (Join-Path $cppOnly 'prom-snapshots')
        Copy-Item -LiteralPath (Join-Path $snapDir 't5m_cpp_scene_1.txt') -Destination (Join-Path $cppOnly 'prom-snapshots/t5m_cpp_scene_1.txt')
        $cppOut = & $summarizeScript -RunDir $cppOnly -KafkaContainer '' 6>&1 | Out-String
        $cppLines = @($cppOut -split "`n" | Where-Object { $_ -match 't5m_cpp_scene_1' })
        Assert-Equal -Expected 1 -Actual $cppLines.Count -Because "C++ 快照只该出现在 N1(实际:$($cppLines -join ' / '))"
    }
} finally {
    Remove-Item -LiteralPath $tmpRoot -Recurse -Force -ErrorAction SilentlyContinue
}

exit (Complete-TestRun -SuiteName 'stress_cpp_metrics')
