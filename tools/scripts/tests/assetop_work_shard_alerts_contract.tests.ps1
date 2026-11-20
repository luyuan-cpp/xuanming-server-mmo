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
    同一文件的 asset-channel.wake-shards 组(落地唤醒订阅按片分工,docs/design/redis-cluster-all-domains-20261110.md §4.6)
    也在这里核对:
      - assetop_wake_subscribed_shards 由 go/shared/assetop/metrics.go 注册,只带常量 service label;
      - 缺片阈值就是 go/shared/assetopwake 的 ShardCount;
      - 按 (service) 聚合、不带 namespace,前提是 data_service 的唤醒成员表跨全部 zone(recall_channel.go 的 recallWakeMembersPrefix
        不带 zone);那张表改回按 zone 圈的话,这条规则要跟着改回 (namespace, service);
      - 三个订阅方(guild / trade / data_service)都绑了片的负责者,并把 assetop.Metrics 交给 assetopwake.New,否则没有序列。
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
$script:WakeGo = Read-RepoText -RelativePath 'go/shared/assetopwake/wake.go'
$script:WakeGroup = Get-AlertGroupText -Text $script:Alerts -Group 'asset-channel.wake-shards'
$script:WakeRules = @(Get-AlertRules -GroupText $script:WakeGroup)
$script:WakeNames = @('AssetOpWakeShardsUnsubscribed')
# 起落地唤醒订阅的三个服务;每个文件里只有一处 assetopwake.New。
$script:WakeSubscribers = @('go/guild/internal/svc/asset_op.go', 'go/trade/internal/svc/assetchannel.go', 'go/data_service/internal/svc/recall_channel.go')

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

Test-Case 'wake-shards 组:规则名单、severity、for、summary、description 钉死' {
    Assert-Match -Text $script:WakeGroup -Pattern '(?m)^      interval:[ ]*60s[ ]*$' -Because '与 work-shards 组同一评估间隔'
    Assert-Equal -Expected ($script:WakeNames -join ',') -Actual (($script:WakeRules | ForEach-Object { $_.Name }) -join ',') -Because '规则名单钉死(唤醒缺片)'
    $body = $script:WakeRules[0].Body
    # 缺片 warning、持续 5 分钟,与 AssetOpOwnedShardsMissing 同一口径:换主、重订的抖动在几秒到几十秒内收敛。
    Assert-Match -Text $body -Pattern '(?m)^            severity:[ ]*warning[ ]*$' -Because 'AssetOpWakeShardsUnsubscribed 的 severity'
    Assert-Match -Text $body -Pattern '(?m)^          for:[ ]*5m[ ]*$' -Because 'AssetOpWakeShardsUnsubscribed 的 for'
    foreach ($key in @('summary', 'description')) {
        Assert-Match -Text $body -Pattern ('(?m)^            {0}:' -f $key) -Because "AssetOpWakeShardsUnsubscribed 要有 $key"
    }
}

Test-Case '唤醒缺片:指标由 metrics.go 注册、不带可变 label,阈值是 assetopwake.ShardCount,按 (service) 聚合' {
    $referenced = @([regex]::Matches($script:WakeGroup, '\bassetop_[a-z0-9_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'assetop_wake_subscribed_shards' -Actual ($referenced -join ',') -Because '只引用已订上的唤醒片数一个 gauge'
    Assert-Match -Text $script:MetricsGo -Pattern '(?s)Name:\s*"assetop_wake_subscribed_shards",.*?ConstLabels: labels,\s*\}, nil\)' -Because 'assetop_wake_subscribed_shards 由 go/shared/assetop/metrics.go 注册且不带可变 label:按 service 求和才是一个服务已订上的总片数'
    $count = [regex]::Match($script:WakeGo, '(?m)^\s*ShardCount\s*=\s*(?<n>\d+)\s*$')
    Assert-True -Condition $count.Success -Because '夹具前提:能从 go/shared/assetopwake/wake.go 读到 ShardCount'
    $expr = [regex]::Match($script:WakeRules[0].Body, '(?ms)^          expr: \|\n(?<e>.*?)(?=^          for:)').Groups['e'].Value
    Assert-Match -Text $expr -Pattern ('^\s*sum by \(service\) \(assetop_wake_subscribed_shards\) < {0}\s*$' -f $count.Groups['n'].Value) -Because '缺片的式子是 sum by (service) (assetop_wake_subscribed_shards) < ShardCount(片数改了,阈值跟着改)'
    # 不带 namespace 的前提:data_service 的唤醒成员表跨全部 zone。改回按 zone 圈,每个 zone namespace 都会各订满 64 片,
    # 按 service 求和变成 64 × zone 数,缺片就看不出来了。
    $recall = Read-RepoText -RelativePath 'go/data_service/internal/svc/recall_channel.go'
    Assert-Match -Text $recall -Pattern '(?s)func recallWakeMembersPrefix\(\) string \{\s*return base\.ENodeType_name\[int32\(base\.ENodeType_DataServiceNodeService\)\] \+ "\.rpc/"\s*\}' -Because 'data_service 的唤醒成员表前缀不带 zone(全部 zone 的 stable data_service 一张表)'
    Assert-Match -Text $recall -Pattern 'workshard\.NewMembership\(recallWakeMembersPrefix\(\)\)' -Because '唤醒订阅用的就是这张跨 zone 的成员表'
}

Test-Case '三个订阅方都绑了片的负责者,并把 assetop.Metrics 交给 assetopwake.New(否则没有 assetop_wake_subscribed_shards 序列)' {
    foreach ($path in $script:WakeSubscribers) {
        $text = Read-RepoText -RelativePath $path
        Assert-Match -Text $text -Pattern '\.BindShardOwner\(' -Because "$path 的唤醒订阅要只订本副本负责的片"
        $calls = @([regex]::Matches($text, 'assetopwake\.New\('))
        Assert-Equal -Expected 1 -Actual $calls.Count -Because "夹具前提:$path 里只有一处 assetopwake.New"
        # 最后一个实参(counter)到收尾的右括号为止;中间的实参可能跨行(guild 的 Config 字面量)。
        $last = [regex]::Match($text.Substring($calls[0].Index), '(?s)^assetopwake\.New\(.*?,\s*(?<counter>\w+)\)')
        Assert-True -Condition $last.Success -Because "夹具前提:能读到 $path 里 assetopwake.New 的最后一个实参"
        Assert-True -Condition ($last.Groups['counter'].Value -cne 'nil') -Because "$path 传给 assetopwake.New 的 counter 不能是 nil"
    }
}

Test-Case '各条告警的名字在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in @($script:Names + $script:WakeNames)) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'assetop_work_shard_alerts_contract')
