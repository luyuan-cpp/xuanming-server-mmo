#requires -Version 7
<#
.SYNOPSIS
    C++ 节点依赖连接、节点间 RPC 与 Kafka 生产告警的契约(docs/design/infra-reconnect-overview.md §1.8、§4.1 R17)。

.DESCRIPTION
    deploy/k8s/cpp-node-dependency-alerts.yaml 的规则读三处 C++ 源码导出的指标:
    cpp/libs/engine/core/metrics/dependency_link_metrics.h(cpp_node_dependency_*)、
    cpp/libs/engine/core/network/rpc_channel_stats.cpp(cpp_node_rpc_*,2026-11-27)、
    cpp/libs/engine/core/node/system/node/node.cpp(cpp_node_kafka_producer_*,2026-11-27)。
    写错指标名、dependency / outcome 取值,按高基数标签分组,都不会报错,只会让告警永远不响或乱响。这里逐项对照 C++ 源码。
    只读文本,不执行部署入口,不访问集群;规则语法由 promtool 另验。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/cpp_node_dependency_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

# 每条规则:名字与规则体(到下一条规则或文件尾为止)。
function Get-AlertRules {
    param([Parameter(Mandatory = $true)][string]$Text)
    return @([regex]::Matches($Text, '(?ms)^        - alert:[ ]*(?<name>\S+)[ ]*\n(?<body>.*?)(?=^        - alert:|\z)') |
            ForEach-Object { [pscustomobject]@{ Name = $_.Groups['name'].Value; Body = $_.Groups['body'].Value } })
}

function Get-RuleExpr {
    param([Parameter(Mandatory = $true)][string]$Body)
    $m = [regex]::Match($Body, '(?ms)^          expr: \|\n(?<e>.*?)(?=^          for:)')
    if (-not $m.Success) { throw '规则里找不到 `expr: |` 段(或它后面不是 for:)' }
    return $m.Groups['e'].Value
}

# C++ 名字表(inline constexpr std::string_view kXxxNames[] = { ... };)里的取值。
function Get-CppNameTable {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Name)
    $m = [regex]::Match($Text, ('(?s){0}\[\]\s*=\s*\{{(?<body>[^}}]*)\}}' -f [regex]::Escape($Name)))
    if (-not $m.Success) { throw "dependency_link_metrics.h 里找不到 $Name" }
    return @([regex]::Matches($m.Groups['body'].Value, '"(?<v>[a-z_]+)"') | ForEach-Object { $_.Groups['v'].Value })
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/cpp-node-dependency-alerts.yaml'
$script:Header = Read-RepoText -RelativePath 'cpp/libs/engine/core/metrics/dependency_link_metrics.h'
# 导出指标的全部源码:规则里的指标名要以字符串字面量出现在其中一处(BeginFamily / WriteCounter / WriteGauge 的名字参数)。
$script:MetricSources = @(
    $script:Header,
    (Read-RepoText -RelativePath 'cpp/libs/engine/core/network/rpc_channel_stats.cpp'),
    (Read-RepoText -RelativePath 'cpp/libs/engine/core/node/system/node/node.cpp')
) -join "`n"
$script:Rules = @(Get-AlertRules -Text $script:Alerts)
$script:Dependencies = Get-CppNameTable -Text $script:Header -Name 'kDependencyNames'
$script:Reasons = Get-CppNameTable -Text $script:Header -Name 'kFailureReasonNames'
# RPC 结局名表(rpc_status.h:inline constexpr std::array<const char*, N> kRpcStatusCodeNames{ ... };)。
$script:RpcStatus = Read-RepoText -RelativePath 'cpp/libs/engine/core/network/rpc_status.h'
$script:RpcOutcomes = @(
    [regex]::Matches([regex]::Match($script:RpcStatus, '(?s)kRpcStatusCodeNames\{(?<body>[^}]*)\}').Groups['body'].Value, '"(?<v>[a-z_]+)"') |
        ForEach-Object { $_.Groups['v'].Value })

Write-Host ''
Write-Host '=== cpp_node_dependency_alerts_contract ==='

Test-Case '夹具前提:C++ 名字表与枚举一一对应' {
    Assert-Equal -Expected 'redis_player,redis_control,redis_other,etcd,grpc,node_link' -Actual ($script:Dependencies -join ',') -Because 'dependency 取值'
    Assert-Equal -Expected 'connect,auth,handshake' -Actual ($script:Reasons -join ',') -Because 'reason 取值'
    Assert-Match -Text $script:Header -Pattern 'static_assert\(std::size\(kDependencyNames\) == static_cast<size_t>\(Dependency::kCount\)\)' -Because '名字表与 Dependency 枚举同长'
    Assert-Equal -Expected 'ok,not_sent,remote_rejected,remote_failed,reply_dropped,timeout,disconnected,channel_closed,invalid_response' -Actual ($script:RpcOutcomes -join ',') -Because 'RPC outcome 取值'
}

Test-Case '规则名单:cpp-node.dependencies 两条、cpp-node.calls 三条,各有 severity / summary / description' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    - name:[ ]*cpp-node\.dependencies[ ]*$' -Because '组名'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    - name:[ ]*cpp-node\.calls[ ]*$' -Because '组名'
    Assert-Equal -Expected 'CppNodeDependencyConnectFailing,CppNodeDependencyFlapping,CppNodeRpcCallFailureRatioHigh,CppNodeKafkaProduceFailing,CppNodeKafkaProducerFatal' -Actual (($script:Rules | ForEach-Object Name) -join ',') -Because '规则名单'
    foreach ($rule in $script:Rules) {
        foreach ($key in @('severity', 'summary', 'description')) {
            Assert-Match -Text $rule.Body -Pattern ('(?m)^            {0}:' -f $key) -Because "$($rule.Name) 要有 $key"
        }
    }
}

Test-Case '式子里的指标都由 C++ 导出;分组 label 只有低基数的 namespace / instance / dependency / reason;dependency / outcome 取值都存在' {
    foreach ($rule in $script:Rules) {
        $expr = Get-RuleExpr -Body $rule.Body
        $names = @([regex]::Matches($expr, '\bcpp_node_[a-z_]+\b') | ForEach-Object Value | Sort-Object -Unique)
        Assert-True -Condition ($names.Count -gt 0) -Because "$($rule.Name) 至少读一个 cpp_node_ 指标"
        foreach ($name in $names) {
            Assert-Match -Text $script:MetricSources -Pattern ('"{0}"' -f [regex]::Escape($name)) -Because "$($rule.Name) 读的 $name 要在 C++ 源码里导出"
        }
        foreach ($m in [regex]::Matches($expr, 'outcome(=~|!~|=|!=)"(?<v>[a-z_|]+)"')) {
            foreach ($value in @($m.Groups['v'].Value -split '\|')) {
                Assert-True -Condition ($script:RpcOutcomes -contains $value) -Because "$($rule.Name) 里的 outcome=$value 要是 rpc_status.h 的取值"
            }
        }
        foreach ($by in [regex]::Matches($expr, '\bby \((?<labels>[^)]*)\)')) {
            foreach ($label in @($by.Groups['labels'].Value -split ',' | ForEach-Object { $_.Trim() })) {
                Assert-True -Condition (@('namespace', 'instance', 'dependency', 'reason') -contains $label) -Because "$($rule.Name) 不按 $label 分组(只许低基数标签)"
            }
        }
        foreach ($m in [regex]::Matches($expr, 'dependency!?="(?<v>[a-z_]+)"')) {
            Assert-True -Condition ($script:Dependencies -contains $m.Groups['v'].Value) -Because "$($rule.Name) 里的 dependency=$($m.Groups['v'].Value) 要是 C++ 导出的取值"
        }
    }
    $flapping = @($script:Rules | Where-Object Name -ceq 'CppNodeDependencyFlapping')[0]
    Assert-Match -Text (Get-RuleExpr -Body $flapping.Body) -Pattern 'dependency!="node_link"' -Because '对端滚动重启必然让节点链路断一次,反复断开不报 node_link'

    $ratio = Get-RuleExpr -Body (@($script:Rules | Where-Object Name -ceq 'CppNodeRpcCallFailureRatioHigh')[0]).Body
    $failing = [regex]::Match($ratio, 'outcome=~"(?<v>[a-z_|]+)"').Groups['v'].Value -split '\|'
    Assert-Equal -Expected 'not_sent,remote_failed,reply_dropped,timeout,disconnected,invalid_response' -Actual ($failing -join ',') -Because '分子只算没正常完成的结局:remote_rejected 含热关停的人为拒绝,channel_closed 是本端关机'
    Assert-Match -Text $ratio -Pattern '\(sum by \(namespace, instance\) \(rate\(cpp_node_rpc_client_calls_total\[10m\]\)\) > 1\)' -Because '分母只留每秒至少 1 次调用的节点,免得一两次失败就是 100%'
}

Test-Case '告警名在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($rule in $script:Rules) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $rule.Name })).Count -Because "$($rule.Name) 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'cpp_node_dependency_alerts_contract')
