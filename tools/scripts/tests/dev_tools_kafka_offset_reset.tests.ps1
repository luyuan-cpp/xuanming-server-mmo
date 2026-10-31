#requires -Version 7
<#
.SYNOPSIS
    dev_tools.ps1 -Command kafka-offset-reset 的参数透传契约测试(2026-10-15)。

.DESCRIPTION
    背景:以前这个分支用数组 splatting 调 kafka_offset_reset.ps1(`$kArgs = @("-BootstrapServer", ...)` 再 `@kArgs`),
    PowerShell 把数组元素当位置参数依次绑定,"-Topic" 后面的 topic 名撞上 [int]$Partitions 直接类型转换失败 ——
    重置路径从来跑不通。旧的契约测试只做源码断言,测不出来。

    本套在隔离目录里真跑 dev_tools.ps1 入口(夹具做法同 dev_tools_proto_gen_exit.tests.ps1:只拷 dev_tools.ps1 与
    lib/release_common.ps1,镜像 tag 显式给出,不依赖 git),旁边放一个假的 kafka_offset_reset.ps1:与真脚本同名同类型地声明全部参数、
    关掉位置绑定,把收到的具名参数逐行写进记录文件。断言:
      - 每个参数都按名字绑定到位(BootstrapServer / Topic / Group / ToDatetime / ToLatest / Partitions / Apply …);
      - 留空 / 缺省的参数不透传(字符串非空才放、switch 打开才放、数值偏离缺省才放);
      - 下游以非 0 退出时 dev_tools.ps1 同样失败,不把失败吞成成功。
    不运行任何 Kafka CLI。

.EXAMPLE
    pwsh -File tools/scripts/tests/dev_tools_kafka_offset_reset.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ""
Write-Host "=== dev_tools.ps1 kafka-offset-reset argument forwarding contract tests ==="

$fixture = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-dev-tools-kafka-reset-' + [guid]::NewGuid().ToString('N'))
$fixtureScriptDir = Join-Path $fixture 'tools' 'scripts'
$recordPath = Join-Path $fixture 'kafka_offset_reset.bound.txt'

# 假 kafka_offset_reset.ps1:参数名与类型同真脚本([int]$Partitions 等),位置绑定关闭 —— 任何错位都会直接报错而不是静默记错账。
$fakeKafkaReset = @'
[CmdletBinding(PositionalBinding = $false)]
param(
    [Parameter(Mandatory = $true)][string]$BootstrapServer,
    [string]$Group,
    [string]$Topic,
    [string]$ToDatetime,
    [switch]$ToEarliest,
    [switch]$ToLatest,
    [switch]$DeleteAndRecreateTopic,
    [int]$Partitions,
    [int]$ReplicationFactor,
    [string]$KafkaBin,
    [switch]$Apply
)
$boundParameters = $PSBoundParameters
$lines = @($boundParameters.Keys | Sort-Object | ForEach-Object { "$_=$($boundParameters[$_])" })
Set-Content -LiteralPath $env:MMORPG_FAKE_KAFKA_RESET_RECORD -Encoding utf8 -Value $lines
$code = [int]$env:MMORPG_FAKE_KAFKA_RESET_EXIT
if ($code -ne 0) {
    Write-Host "fake kafka_offset_reset failing with exit code $code"
    exit $code
}
exit 0
'@

<#
.SYNOPSIS
    以子进程真跑一次夹具里的 dev_tools.ps1 -Command kafka-offset-reset,返回 @{ ExitCode; Output; Called; Bound }。
    Bound 是假下游收到的具名参数(参数名 → 字符串值);没被调到时 Called = $false、Bound 为空表。
#>
function Invoke-DevToolsKafkaReset {
    param(
        [string[]]$Arguments = @(),
        [int]$FakeExit = 0
    )

    if (Test-Path -LiteralPath $recordPath) { Remove-Item -LiteralPath $recordPath -Force }
    # 镜像 tag 显式给出:夹具目录不是 git 仓库,留空会在入口因拿不到版本戳而失败(与本测试无关)。
    $baseArguments = @('-Command', 'kafka-offset-reset', '-ImageTag', 'fixture', '-GoSvcTag', 'fixture', '-JavaSvcTag', 'fixture', '-NodeImage', 'fixture:local')
    $run = Invoke-CapturedPwsh -ScriptPath (Join-Path $fixtureScriptDir 'dev_tools.ps1') -Arguments ($baseArguments + $Arguments) `
        -Env @{ MMORPG_FAKE_KAFKA_RESET_RECORD = $recordPath; MMORPG_FAKE_KAFKA_RESET_EXIT = [string]$FakeExit }
    $bound = @{}
    $called = Test-Path -LiteralPath $recordPath
    if ($called) {
        foreach ($line in @(Get-Content -LiteralPath $recordPath -Encoding utf8)) {
            $separator = $line.IndexOf('=')
            if ($separator -gt 0) { $bound[$line.Substring(0, $separator)] = $line.Substring($separator + 1) }
        }
    }
    return [pscustomobject]@{ ExitCode = $run.ExitCode; Output = $run.Output; Called = $called; Bound = $bound }
}

# 断言假下游收到的参数恰好是 $Expected(参数名 → 值),不多不少。
function Assert-BoundExactly {
    param(
        [Parameter(Mandatory = $true)]$Run,
        [Parameter(Mandatory = $true)][hashtable]$Expected,
        [Parameter(Mandatory = $true)][string]$Label
    )

    Assert-True -Condition $Run.Called -Because "${Label}:必须真正调到 kafka_offset_reset.ps1。输出: $($Run.Output)"
    foreach ($name in $Expected.Keys) {
        Assert-True -Condition $Run.Bound.ContainsKey($name) -Because "${Label}:-$name 必须按名字传给 kafka_offset_reset.ps1(实际收到: $(@($Run.Bound.Keys | Sort-Object) -join ', '))"
        Assert-Equal -Expected $Expected[$name] -Actual $Run.Bound[$name] -Because "${Label}:-$name 的值"
    }
    $extra = @($Run.Bound.Keys | Where-Object { -not $Expected.ContainsKey($_) } | Sort-Object)
    Assert-Equal -Expected 0 -Actual $extra.Count -Because "${Label}:留空 / 缺省的参数不得透传(多出: $($extra -join ', '))"
}

$exitCode = 1
try {
    New-Item -ItemType Directory -Path (Join-Path $fixtureScriptDir 'lib') -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path (Get-ToolsScriptsDir) 'dev_tools.ps1') -Destination $fixtureScriptDir
    Copy-Item -LiteralPath (Join-Path (Get-ToolsScriptsDir) 'lib' 'release_common.ps1') -Destination (Join-Path $fixtureScriptDir 'lib')
    Set-Content -LiteralPath (Join-Path $fixtureScriptDir 'kafka_offset_reset.ps1') -Encoding utf8 -Value $fakeKafkaReset

    Test-Case '拨到日志末端:-KafkaGroup / -KafkaToLatest / -KafkaPartitions / -KafkaApply 全部按名字绑定' {
        $run = Invoke-DevToolsKafkaReset -Arguments @(
            '-KafkaBootstrapServer', 'kafka.infra:9092', '-KafkaTopic', 'transaction_log_topic_g1',
            '-KafkaGroup', 'data_service-transaction-log', '-KafkaToLatest', '-KafkaPartitions', '6', '-KafkaApply')
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "假下游成功时 dev_tools.ps1 不该失败。输出: $($run.Output)"
        Assert-BoundExactly -Run $run -Label '拨到日志末端' -Expected @{
            BootstrapServer = 'kafka.infra:9092'; Topic = 'transaction_log_topic_g1'; Group = 'data_service-transaction-log'
            ToLatest = 'True'; Partitions = '6'; Apply = 'True'
        }
    }

    Test-Case '按时间重置:不带 -KafkaApply 时不透传 Apply' {
        $run = Invoke-DevToolsKafkaReset -Arguments @(
            '-KafkaBootstrapServer', '127.0.0.1:9092', '-KafkaTopic', 'player_snapshot_topic_g1',
            '-KafkaGroup', 'data_service-player-snapshot', '-KafkaToDatetime', '2026-05-15T03:17:00Z', '-KafkaPartitions', '3')
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
        Assert-BoundExactly -Run $run -Label '按时间重置' -Expected @{
            BootstrapServer = '127.0.0.1:9092'; Topic = 'player_snapshot_topic_g1'; Group = 'data_service-player-snapshot'
            ToDatetime = '2026-05-15T03:17:00Z'; Partitions = '3'
        }
    }

    Test-Case '删 topic 重建:-KafkaDeleteAndRecreate / -KafkaReplicationFactor / -KafkaBin 按名字绑定,不带 Group' {
        $run = Invoke-DevToolsKafkaReset -Arguments @(
            '-KafkaTopic', 'contract_recreate_topic', '-KafkaDeleteAndRecreate', '-KafkaPartitions', '5',
            '-KafkaReplicationFactor', '3', '-KafkaBin', '/opt/kafka/bin')
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
        Assert-BoundExactly -Run $run -Label '删 topic 重建' -Expected @{
            BootstrapServer = 'kafka:9092'; Topic = 'contract_recreate_topic'; DeleteAndRecreateTopic = 'True'
            Partitions = '5'; ReplicationFactor = '3'; KafkaBin = '/opt/kafka/bin'
        }
    }

    Test-Case '删 topic 重建:显式 -KafkaReplicationFactor 1 也透传(以前缺省 1 且只在 ≠ 1 时透传,单 broker 档传不下去)' {
        $run = Invoke-DevToolsKafkaReset -Arguments @(
            '-KafkaTopic', 'contract_recreate_topic', '-KafkaDeleteAndRecreate', '-KafkaPartitions', '5', '-KafkaReplicationFactor', '1')
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
        Assert-BoundExactly -Run $run -Label '副本数 1' -Expected @{
            BootstrapServer = 'kafka:9092'; Topic = 'contract_recreate_topic'; DeleteAndRecreateTopic = 'True'
            Partitions = '5'; ReplicationFactor = '1'
        }
    }

    Test-Case '缺省值不透传:只给 topic 与 -KafkaToEarliest 时,下游只收到 BootstrapServer(dev_tools 缺省)/ Topic / ToEarliest' {
        $run = Invoke-DevToolsKafkaReset -Arguments @('-KafkaTopic', 'match-results', '-KafkaToEarliest')
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
        Assert-BoundExactly -Run $run -Label '缺省值' -Expected @{ BootstrapServer = 'kafka:9092'; Topic = 'match-results'; ToEarliest = 'True' }
    }

    Test-Case '下游以非 0 退出:dev_tools.ps1 同样失败,错误带上退出码' {
        $run = Invoke-DevToolsKafkaReset -FakeExit 7 -Arguments @('-KafkaTopic', 'match-results', '-KafkaGroup', 'g', '-KafkaToLatest')
        Assert-True -Condition $run.Called -Because "必须真正调到假下游。输出: $($run.Output)"
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "kafka_offset_reset.ps1 以 7 退出时包装入口必须失败。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern 'exit code 7' -Because '失败诊断要带上下游的退出码'
    }

    $exitCode = Complete-TestRun -SuiteName 'dev_tools kafka-offset-reset 参数透传'
}
finally {
    Remove-Item -LiteralPath $fixture -Recurse -Force -ErrorAction SilentlyContinue
}
exit $exitCode
