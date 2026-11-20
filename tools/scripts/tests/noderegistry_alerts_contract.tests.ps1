#requires -Version 7
<#
.SYNOPSIS
    Go 节点注册告警的契约(docs/design/infra-reconnect-overview.md §4.1 R4、R17)。

.DESCRIPTION
    deploy/k8s/noderegistry-alerts.yaml 的规则读 go/shared/noderegistry/metrics.go 定义的指标。写错指标名、按 node_id / uuid
    分组(高基数,AGENTS.md §9)、把 for 缩到盖不住一次正常重注册,都不会报错,只会让告警永远不响或乱响。这里逐项对照代码里的
    权威来源。只读文本,不执行部署入口,不访问集群;规则语法由 promtool 另验。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/noderegistry_alerts_contract.tests.ps1
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

# Go 时长常量(`name = N * time.Second`)的秒数。
function Get-GoSeconds {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Name)
    $m = [regex]::Match($Text, ('(?m)^\s*{0}\s*=\s*(?<n>\d+) \* time\.Second' -f [regex]::Escape($Name)))
    if (-not $m.Success) { throw "registry.go 里找不到常量 $Name" }
    return [int]$m.Groups['n'].Value
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/noderegistry-alerts.yaml'
$script:Metrics = Read-RepoText -RelativePath 'go/shared/noderegistry/metrics.go'
$script:Registry = Read-RepoText -RelativePath 'go/shared/noderegistry/registry.go'
$script:Rules = @(Get-AlertRules -Text $script:Alerts)

Write-Host ''
Write-Host '=== noderegistry_alerts_contract ==='

Test-Case '规则名单:go.noderegistry 组恰好两条,各有 severity / summary / description' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    - name:[ ]*go\.noderegistry[ ]*$' -Because '组名'
    Assert-Equal -Expected 'NodeRegistryInstanceUnregistered,NodeRegistryLeaseFlapping' -Actual (($script:Rules | ForEach-Object Name) -join ',') -Because '规则名单'
    foreach ($rule in $script:Rules) {
        foreach ($key in @('severity', 'summary', 'description')) {
            Assert-Match -Text $rule.Body -Pattern ('(?m)^            {0}:' -f $key) -Because "$($rule.Name) 要有 $key"
        }
    }
}

Test-Case '式子里的指标都在 metrics.go 里定义;分组 label 只有 namespace / pod / prefix' {
    foreach ($rule in $script:Rules) {
        $expr = Get-RuleExpr -Body $rule.Body
        $names = @([regex]::Matches($expr, '\bnoderegistry_[a-z_]+\b') | ForEach-Object Value | Sort-Object -Unique)
        Assert-True -Condition ($names.Count -gt 0) -Because "$($rule.Name) 至少读一个 noderegistry_ 指标"
        foreach ($name in $names) {
            Assert-Match -Text $script:Metrics -Pattern ('Name:\s*"{0}"' -f [regex]::Escape($name)) -Because "$($rule.Name) 读的 $name 要在 metrics.go 里定义"
        }
        foreach ($by in [regex]::Matches($expr, '\bby \((?<labels>[^)]*)\)')) {
            $labels = @($by.Groups['labels'].Value -split ',' | ForEach-Object { $_.Trim() })
            Assert-Equal -Expected 'namespace,pod,prefix' -Actual ($labels -join ',') -Because "$($rule.Name) 按副本、按前缀判;不按 node_id / uuid 分组"
        }
    }
    Assert-Match -Text $script:Metrics -Pattern '\}, \[\]string\{"prefix"\}\)' -Because '夹具前提:计数器与 gauge 的 label 只有 prefix'
}

Test-Case '未注册的 for 盖得住一次失租后的正常重注册' {
    $rule = @($script:Rules | Where-Object Name -ceq 'NodeRegistryInstanceUnregistered')[0]
    Assert-Match -Text (Get-RuleExpr -Body $rule.Body) -Pattern 'max by \(namespace, pod, prefix\) \(noderegistry_registered\) < 1' -Because '按副本判 gauge 为 0'
    $for = [regex]::Match($rule.Body, '(?m)^          for:[ ]*(?<m>\d+)m[ ]*$')
    Assert-True -Condition $for.Success -Because 'for 以分钟写'
    # 失租之后:第一轮重注册失败时最长等 backoffMax × 1.2(抖动上界),每轮 etcd 调用另有 reRegisterOpTimeout;
    # for 至少容下两轮最长的失败加重试,否则 etcd 闪断一次也会报。
    $backoffMax = Get-GoSeconds -Text $script:Registry -Name 'backoffMax'
    $opTimeout = Get-GoSeconds -Text $script:Registry -Name 'reRegisterOpTimeout'
    $minSeconds = 2 * ($backoffMax * 1.2 + $opTimeout)
    Assert-True -Condition ([int]$for.Groups['m'].Value * 60 -ge $minSeconds) -Because "for $($for.Groups['m'].Value)m 应不小于 2 × (backoffMax × 1.2 + reRegisterOpTimeout) = $minSeconds 秒"
}

Test-Case '告警名在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($rule in $script:Rules) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $rule.Name })).Count -Because "$($rule.Name) 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'noderegistry_alerts_contract')
