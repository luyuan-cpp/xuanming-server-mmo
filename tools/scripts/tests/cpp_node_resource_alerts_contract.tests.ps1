#requires -Version 7
<#
.SYNOPSIS
    C++ 节点 fd 用量与 accept 降级告警的契约测试(2026-11-12)。

.DESCRIPTION
    deploy/k8s/cpp-node-resource-alerts.yaml 的规则读 C++ 节点 Prometheus 出口的指标
    (cpp/libs/engine/core/node/system/node/process_fd_metrics.h 的 Write)。这里钉的都是「写错了不报错、只会永远不响」的东西:
      - 规则引用的每个指标名都由 process_fd_metrics.h 的 BeginFamily 导出;
      - reason 取值是 Write 里写死的样本标签;
      - 指标页确实接进了出口(node.cpp 的 RenderProcessFdMetrics 在抓取回调里);
      - PrometheusRule 形状、规则名单、每条规则的 severity / summary / description / runbook_url / for;
      - 告警名在 deploy/k8s 的全部告警文件里不重名。
    只读文本,不执行部署入口,不访问任何集群。规则语法由 promtool 另验(见 docs/PROGRESS.md 对应条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/cpp_node_resource_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

# 告警文件里某一组的文本:从 `    - name: <组名>` 到下一个组或文件尾。
function Get-AlertGroupText {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Group)
    $m = [regex]::Match($Text, ('(?ms)^    - name:[ ]*{0}[ ]*\n(?<body>.*?)(?=^    - name:|\z)' -f [regex]::Escape($Group)))
    if (-not $m.Success) { throw "找不到告警组 $Group" }
    return $m.Groups['body'].Value
}

# 一组里的每条规则:名字与规则体(到下一条规则、规则前注释或组尾为止)。
function Get-AlertRules {
    param([Parameter(Mandatory = $true)][string]$GroupText)
    return @([regex]::Matches($GroupText, '(?ms)^        - alert:[ ]*(?<name>\S+)[ ]*\n(?<body>.*?)(?=^        - alert:|^        #|\z)') |
            ForEach-Object { [pscustomobject]@{ Name = $_.Groups['name'].Value; Body = $_.Groups['body'].Value } })
}

function Get-RuleExpr {
    param([Parameter(Mandatory = $true)][string]$Body)
    $m = [regex]::Match($Body, '(?ms)^          expr: \|\n(?<e>.*?)(?=^          for:)')
    if (-not $m.Success) { throw '规则里找不到 `expr: |` 段(或它后面不是 for:)' }
    return $m.Groups['e'].Value
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/cpp-node-resource-alerts.yaml'
$script:MetricsHeader = Read-RepoText -RelativePath 'cpp/libs/engine/core/node/system/node/process_fd_metrics.h'
$script:NodeSource = Read-RepoText -RelativePath 'cpp/libs/engine/core/node/system/node/node.cpp'

# C++ 导出的指标族名与 reason 样本标签。
$script:Families = @([regex]::Matches($script:MetricsHeader, 'BeginFamily\("(?<n>[a-z_]+)"') | ForEach-Object { $_.Groups['n'].Value } | Sort-Object -Unique)
$script:Reasons = @([regex]::Matches($script:MetricsHeader, '\{"reason",\s*"(?<v>[a-z_]+)"\}') | ForEach-Object { $_.Groups['v'].Value } | Sort-Object -Unique)

$script:ExpectedRules = [ordered]@{
    'ProcessOpenFilesNearLimit'               = 'warning'
    'CppNodeAcceptResourceExhausted'          = 'warning'
    'CppNodeAcceptResourceExhaustedSustained' = 'critical'
    'CppNodeOpenFileLimitExempted'            = 'warning'
}

Test-Case '夹具:process_fd_metrics.h 导出的指标族与 reason 取值' {
    Assert-Equal -Expected 'cpp_node_accept_exhausted_total,cpp_node_accept_paused_total,cpp_node_open_file_limit_exempted,cpp_node_open_file_limit_required,process_max_fds,process_open_fds' `
        -Actual ($script:Families -join ',') -Because 'Write 的指标族;增删指标要同步告警与本用例'
    Assert-Equal -Expected 'descriptors,memory' -Actual ($script:Reasons -join ',') -Because 'cpp_node_accept_exhausted_total 的 reason 取值'
}

Test-Case 'node.cpp:指标页接进了出口的抓取回调,门槛结果与 Acceptor 计数都有来源' {
    Assert-Match -Text $script:NodeSource -Pattern 'RenderNodeIdentityMetrics\(GetNodeInfo\(\)\) \+ RenderProcessFdMetrics\(\) \+ GlobalMetricsRegistry\(\)\.Render\(\)' `
        -Because '抓取回调里要拼上 RenderProcessFdMetrics,否则这些序列根本不存在'
    Assert-Match -Text $script:NodeSource -Pattern 'muduo::net::Acceptor::exhaustionCounts\(\)' -Because 'accept 计数读自 muduo Acceptor'
    Assert-Match -Text $script:NodeSource -Pattern 'gOpenFileLimitCheckResult = \{true, required, true\};' -Because '走豁免启动时要记下 exempted,CppNodeOpenFileLimitExempted 才会响'
    Assert-Match -Text $script:NodeSource -Pattern 'gOpenFileLimitCheckResult = \{true, required, false\};' -Because '正常通过时以 0 导出 exempted'
}

Test-Case 'cpp-node-resource-alerts.yaml:PrometheusRule 形状,cpp-node.resources 组的规则名单与每条规则的字段' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与其余告警文件同一安装方式)'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^  name:[ ]*cpp-node-resource-alerts[ ]*$' -Because 'metadata.name'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    $group = Get-AlertGroupText -Text $script:Alerts -Group 'cpp-node.resources'
    Assert-Match -Text $group -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because '评估间隔'
    $rules = @(Get-AlertRules -GroupText $group)
    Assert-Equal -Expected (($script:ExpectedRules.Keys | Sort-Object) -join ',') -Actual ((@($rules | ForEach-Object { $_.Name }) | Sort-Object) -join ',') -Because '规则名单钉死'
    foreach ($rule in $rules) {
        $severity = $script:ExpectedRules[$rule.Name]
        Assert-Match -Text $rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $severity) -Because "$($rule.Name) 的 severity 应为 $severity"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            summary:' -Because "$($rule.Name) 要有 summary"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            description:' -Because "$($rule.Name) 要有 description"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            runbook_url:[ ]*docs/ops/gate-kernel-tuning-runbook\.md[ ]*$' -Because "$($rule.Name) 指向 fd 上限的运维手册"
        Assert-Match -Text $rule.Body -Pattern '(?m)^          for:[ ]*\d+[mh][ ]*$' -Because "$($rule.Name) 要写 for"
    }
}

Test-Case 'cpp-node-resource-alerts.yaml:引用的指标都由 C++ 导出,reason 取值都在导出的取值里' {
    foreach ($rule in (Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group 'cpp-node.resources'))) {
        $expr = Get-RuleExpr -Body $rule.Body
        $referenced = @([regex]::Matches($expr, '\b(?:process|cpp_node)_[a-z_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
        Assert-True -Condition ($referenced.Count -gt 0) -Because "$($rule.Name) 至少引用一个 process_* 或 cpp_node_* 指标"
        foreach ($metric in $referenced) {
            Assert-True -Condition ($script:Families -ccontains $metric) -Because "$($rule.Name):$metric 必须由 process_fd_metrics.h 导出(改名后告警静默失效)"
        }
        foreach ($m in [regex]::Matches($expr, '\breason\s*=~?\s*"(?<v>[^"]*)"')) {
            foreach ($v in ($m.Groups['v'].Value -split '\|')) {
                Assert-True -Condition ($script:Reasons -ccontains $v) -Because "$($rule.Name):reason=$v 必须是 C++ 导出的取值"
            }
        }
    }
}

Test-Case '本文件钉的每条告警的名字在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in @($script:ExpectedRules.Keys)) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'cpp_node_resource_alerts_contract')
