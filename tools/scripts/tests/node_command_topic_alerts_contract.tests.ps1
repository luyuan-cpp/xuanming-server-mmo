<#
.SYNOPSIS
    命令 topic 读放大告警与 C++ 计数的契约(docs/design/connection-fanout-subsetting-20261106.md §3.5)。

.DESCRIPTION
    deploy/k8s/node-command-topic-alerts.yaml 的规则只在序列存在时才可能响,写错指标名、label 名或取值不会报错,只会永远不响。
    这里逐项对照 C++ 侧的权威来源 cpp/libs/engine/core/node/system/node/node_command_consume_stats.h:
      - 指标族名 node_command_consumed_total;
      - 规则里 target 的取值(own / foreign)与按 label 聚合的 node_type、topic,都是采集回调真的写出的;
      - PrometheusRule 形状、severity / summary / description / for,告警名在 deploy/k8s 的告警文件里不重名。
    只读文本,不执行部署入口,不访问集群。规则语法由 promtool 另验。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/node_command_topic_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/node-command-topic-alerts.yaml'
$script:Stats = Read-RepoText -RelativePath 'cpp/libs/engine/core/node/system/node/node_command_consume_stats.h'
$script:AlertName = 'NodeCommandTopicReadAmplificationHigh'

Write-Host '=== node_command_topic_alerts_contract ==='

Test-Case 'PrometheusRule 形状:kind、名字、severity、summary、description、for 10m 都在' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 owner-epoch-alerts.yaml 同一安装方式)'
    Assert-Match -Text $script:Alerts -Pattern ('(?m)^\s+- alert:[ ]*{0}[ ]*$' -f $script:AlertName) -Because '规则名固定,值班手册按名字查'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^\s+severity:[ ]*warning[ ]*$' -Because '读放大是容量趋势,下个工作日处理'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^\s+for:[ ]*10m[ ]*$' -Because '设计稿 §3.5:持续 10 分钟'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^\s+summary:' -Because '告警要有 summary'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^\s+description:' -Because '告警要有 description'
}

Test-Case '规则用的指标名与 target 取值都是 C++ 采集回调写出的' {
    Assert-Match -Text $script:Stats -Pattern '"node_command_consumed_total"' -Because 'C++ 侧的指标族名'
    $metricNames = @([regex]::Matches($script:Alerts, '(?<m>[a-z_]+)\{target="') | ForEach-Object { $_.Groups['m'].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'node_command_consumed_total' -Actual ($metricNames -join ',') -Because '规则只引用这一个指标,名字与 C++ 一致'
    $targets = @([regex]::Matches($script:Alerts, 'target="(?<v>[^"]+)"') | ForEach-Object { $_.Groups['v'].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'foreign,own' -Actual ($targets -join ',') -Because '规则要同时用到 own 与 foreign'
    foreach ($value in $targets) {
        Assert-Match -Text $script:Stats -Pattern ('\{{"target", "{0}"\}}' -f [regex]::Escape($value)) -Because "target=`"$value`" 必须是 C++ 写出的取值,否则规则永远不响"
    }
}

Test-Case '按 label 聚合的 node_type、topic 是 C++ 写出的 label' {
    $byClauses = @([regex]::Matches($script:Alerts, 'sum by \((?<l>[^)]*)\)') | ForEach-Object { $_.Groups['l'].Value })
    Assert-True -Condition ($byClauses.Count -eq 3) -Because "三处 sum by 的 label 集合要一致(实际 $($byClauses.Count) 处)"
    Assert-Equal -Expected 1 -Actual (@($byClauses | Sort-Object -Unique)).Count -Because '比值两边按同一组 label 聚合,否则 and / 比较会对不上'
    foreach ($label in @('node_type', 'topic')) {
        Assert-Match -Text $byClauses[0] -Pattern ('\b{0}\b' -f $label) -Because "按 $label 聚合"
        Assert-Match -Text $script:Stats -Pattern ('\{{"{0}", ' -f $label) -Because "$label 必须是 C++ 写出的 label"
    }
}

Test-Case '告警名在 deploy/k8s 的全部告警文件里只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $script:AlertName })).Count -Because "$($script:AlertName) 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
}

exit (Complete-TestRun -SuiteName 'node_command_topic_alerts_contract')
