#requires -Version 7
<#
.SYNOPSIS
    资产指令重投按片分工的告警契约(docs/design/assetop-redelivery-sharding-20261110.md §12,缺口清单第 25 条)。

.DESCRIPTION
    deploy/k8s/asset-channel-alerts.yaml 的 asset-channel.work-shards 组只在序列存在时才可能响,写错指标名、label 或阈值
    不会报错,只会永远不响或一直响。这里逐项对照代码里的权威来源:
      - assetop_owned_shards、assetop_pending_oldest_age_seconds 由 go/shared/assetop/metrics.go 注册,service 是常量 label,
        assetop_owned_shards 没有可变 label(所以按 (namespace, service) 求和就是一个服务在一个 namespace 里负责的总片数);
      - 缺片阈值就是 go/shared/workshard 的 Shards;
      - 绑了分工的四个重投循环(guild / trade / mail / data_service 的回收)都拿到了 assetop.Metrics,否则那个服务没有序列、规则对它永远不响;
      - 规则名单、severity / for / summary / description,两条规则按同一组 label 聚合,告警名在 deploy/k8s 的告警文件里不重名。
    只读文本,不执行部署入口,不访问集群。规则语法由 promtool 另验。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/assetop_work_shard_alerts_contract.tests.ps1
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
    $m = [regex]::Match($Text, ('(?ms)^    - name:[ ]*{0}[ ]*\n(?<body>.*?)(?=^    - name:|^    # |\z)' -f [regex]::Escape($Group)))
    if (-not $m.Success) { throw "找不到告警组 $Group" }
    return $m.Groups['body'].Value
}

# 一组里的每条规则:名字与规则体(到下一条规则或组尾为止)。
function Get-AlertRules {
    param([Parameter(Mandatory = $true)][string]$GroupText)
    return @([regex]::Matches($GroupText, '(?ms)^        - alert:[ ]*(?<name>\S+)[ ]*\n(?<body>.*?)(?=^        - alert:|^        # |\z)') |
            ForEach-Object { [pscustomobject]@{ Name = $_.Groups['name'].Value; Body = $_.Groups['body'].Value } })
}

function Get-RuleExpr {
    param([Parameter(Mandatory = $true)][string]$Body)
    $m = [regex]::Match($Body, '(?ms)^          expr: \|\n(?<e>.*?)(?=^          for:)')
    if (-not $m.Success) { throw '规则里找不到 `expr: |` 段(或它后面不是 for:)' }
    return $m.Groups['e'].Value
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/asset-channel-alerts.yaml'
$script:MetricsGo = Read-RepoText -RelativePath 'go/shared/assetop/metrics.go'
$script:WorkshardGo = Read-RepoText -RelativePath 'go/shared/workshard/workshard.go'
$script:Group = Get-AlertGroupText -Text $script:Alerts -Group 'asset-channel.work-shards'
$script:Rules = @(Get-AlertRules -GroupText $script:Group)
$script:Names = @('AssetOpOwnedShardsMissing', 'AssetOpOwnedShardsMissingBacklogAging')

function Get-Rule {
    param([Parameter(Mandatory = $true)][string]$Name)
    $rule = @($script:Rules | Where-Object { $_.Name -ceq $Name })
    if ($rule.Count -ne 1) { throw "work-shards 组里找不到规则 $Name(或重复)" }
    return $rule[0]
}

Write-Host ''
Write-Host '=== assetop_work_shard_alerts_contract ==='

Test-Case 'PrometheusRule 形状;work-shards 组的规则名单、severity、for、summary、description 钉死' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与本文件其余规则同一安装方式)'
    Assert-Match -Text $script:Group -Pattern '(?m)^      interval:[ ]*60s[ ]*$' -Because '与 asset-channel.backlog 同一评估间隔'
    Assert-Equal -Expected ($script:Names -join ',') -Actual (($script:Rules | ForEach-Object { $_.Name }) -join ',') -Because '规则名单钉死(缺片 / 缺片持续且积压变老)'
    # 缺片 warning、持续 5 分钟(设计 §5 第 7 条给的式子);持续半小时以上升级为 critical。
    $expected = @{
        'AssetOpOwnedShardsMissing'             = @{ Severity = 'warning'; For = '5m' }
        'AssetOpOwnedShardsMissingBacklogAging' = @{ Severity = 'critical'; For = '30m' }
    }
    foreach ($name in $script:Names) {
        $body = (Get-Rule -Name $name).Body
        Assert-Match -Text $body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $expected[$name].Severity) -Because "$name 的 severity"
        Assert-Match -Text $body -Pattern ('(?m)^          for:[ ]*{0}[ ]*$' -f $expected[$name].For) -Because "$name 的 for"
        foreach ($key in @('summary', 'description')) {
            Assert-Match -Text $body -Pattern ('(?m)^            {0}:' -f $key) -Because "$name 要有 $key"
        }
    }
}

Test-Case '指标由 go/shared/assetop/metrics.go 注册;service 是常量 label,assetop_owned_shards 没有可变 label' {
    $referenced = @([regex]::Matches($script:Group, '\bassetop_[a-z0-9_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'assetop_owned_shards,assetop_pending_oldest_age_seconds' -Actual ($referenced -join ',') -Because '只引用负责片数与最老未决年龄两个 gauge'
    foreach ($name in $referenced) {
        Assert-Match -Text $script:MetricsGo -Pattern ('Name:\s*"{0}"' -f [regex]::Escape($name)) -Because "$name 必须由 go/shared/assetop/metrics.go 注册(改名后规则静默失效)"
    }
    Assert-Match -Text $script:MetricsGo -Pattern 'prometheus\.Labels\{"service": service\}' -Because 'service 是常量 label,告警按它区分服务'
    Assert-Match -Text $script:MetricsGo -Pattern '(?s)Name:\s*"assetop_owned_shards",.*?ConstLabels: labels,\s*\}, nil\)' -Because 'assetop_owned_shards 不带可变 label:按 (namespace, service) 求和才是一个服务负责的总片数'
}

Test-Case '缺片阈值就是 workshard.Shards;两条规则按同一组 label 聚合,升级规则复用同一个缺片条件' {
    $shards = [regex]::Match($script:WorkshardGo, '(?m)^const Shards = (?<n>\d+)\s*$')
    Assert-True -Condition $shards.Success -Because '夹具前提:能从 go/shared/workshard/workshard.go 读到 const Shards'
    $n = $shards.Groups['n'].Value
    $gap = 'sum by \(namespace, service\) \(assetop_owned_shards\) < {0}' -f $n
    $missing = Get-RuleExpr -Body (Get-Rule -Name 'AssetOpOwnedShardsMissing').Body
    Assert-Match -Text $missing -Pattern ('^\s*{0}\s*$' -f $gap) -Because "缺片的式子是 sum by (namespace, service) (assetop_owned_shards) < $n(片数改了,阈值跟着改)"
    $aging = Get-RuleExpr -Body (Get-Rule -Name 'AssetOpOwnedShardsMissingBacklogAging').Body
    Assert-Match -Text $aging -Pattern ('\({0}\)' -f $gap) -Because '升级规则的缺片条件与 AssetOpOwnedShardsMissing 逐字相同'
    Assert-Match -Text $aging -Pattern 'and on \(namespace, service\)' -Because '两边按同一组 label 匹配'
    Assert-Match -Text $aging -Pattern 'max by \(namespace, service\) \(assetop_pending_oldest_age_seconds\) > \d+' -Because '积压年龄取同一服务各副本的最大值(各副本读的是同一份 SQL)'
    $byClauses = @([regex]::Matches($script:Group, '(?:sum|max) by \((?<l>[^)]*)\)') | ForEach-Object { $_.Groups['l'].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'namespace, service' -Actual ($byClauses -join ';') -Because 'data_service 每个 zone namespace 一份、成员表只圈本 zone,必须带 namespace 聚合'
}

Test-Case '绑了分工的四个重投循环都拿到 assetop.Metrics(否则那个服务没有 assetop_owned_shards 序列)' {
    foreach ($path in @('go/guild/internal/svc/asset_op.go', 'go/trade/internal/reconcile/pipeline.go', 'go/mail/mail.go', 'go/data_service/internal/svc/recall_channel.go')) {
        $text = Read-RepoText -RelativePath $path
        Assert-Match -Text $text -Pattern '\.BindWorkOwner\(' -Because "$path 的重投循环要绑按片分工"
        $calls = @([regex]::Matches($text, '(?m)^.*assetop\.NewLoop\(.*$') | ForEach-Object { $_.Value })
        Assert-Equal -Expected 1 -Actual $calls.Count -Because "夹具前提:$path 里只有一处 assetop.NewLoop"
        Assert-NotMatch -Text $calls[0] -Pattern ',\s*nil,\s*(nil|now|time\.Now)\)\s*$' -Because "$path 传给 NewLoop 的 metrics 不能是 nil"
    }
    Assert-Match -Text (Read-RepoText -RelativePath 'go/mail/mail.go') -Pattern 'assetop\.NewMetrics\(prometheus\.DefaultRegisterer, assetOpCaller\)' -Because 'mail 的 assetop 指标注册在默认注册表上(domainhost 的 /metrics 只暴露它)'
}

Test-Case '两条告警的名字在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in $script:Names) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'assetop_work_shard_alerts_contract')
