#requires -Version 7
<#
.SYNOPSIS
    发布轨道告警的契约测试(docs/design/release-track-gray-canary.md §7 批 3a)。

.DESCRIPTION
    deploy/k8s/release-track-alerts.yaml 的三条规则只在序列存在时才可能响,写错指标名、label 名或取值不会报错,只会永远不响。
    这里逐项对照代码里的权威来源:
      - cpp_node_info 由 C++ node.cpp 输出,带 node_type / zone_id / release_track 三个 label;node_type 的 3 / 4 是 node.proto 的
        SceneNodeService / GateNodeService;
      - client_rpc_router_forward_by_track_total 由路由服 metrics.go 注册(subsystem + Name),label 是 release_track、outcome;
        规则里的 outcome 取值都是 metrics.go 的 Outcome* 常量,release_track 取值都是 shared/releasetrack 的轨道名;
      - client_rpc_router_forward_by_track_seconds 由同一个 metrics.go 注册,label 只有 release_track,分桶与 forward_seconds 共用;
        延迟规则两边都取 p99、都按 le 聚合,只比较 stable 与 canary(设计 §2.8 判据 3);
      - PrometheusRule 形状、规则名单、每条规则的 severity / summary / description / runbook_url / for,runbook 文件存在,
        告警名在 deploy/k8s 的告警文件里不重名。
    只读文本,不执行部署入口,不访问集群。规则语法由 promtool 另验(见 docs/PROGRESS.md 对应条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/release_track_alerts_contract.tests.ps1
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

# 一组里的每条规则:名字与规则体(到下一条规则或组尾为止)。
function Get-AlertRules {
    param([Parameter(Mandatory = $true)][string]$GroupText)
    return @([regex]::Matches($GroupText, '(?ms)^        - alert:[ ]*(?<name>\S+)[ ]*\n(?<body>.*?)(?=^        - alert:|\z)') |
            ForEach-Object { [pscustomobject]@{ Name = $_.Groups['name'].Value; Body = $_.Groups['body'].Value } })
}

# 按名字取一组里的一条规则体;找不到就失败(规则改名后用例不能静默跳过)。
function Get-AlertRuleBody {
    param([Parameter(Mandatory = $true)][string]$Group, [Parameter(Mandatory = $true)][string]$Name)
    $rule = @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group $Group) | Where-Object { $_.Name -ceq $Name })
    if ($rule.Count -ne 1) { throw "告警组 $Group 里找不到唯一的规则 $Name" }
    return $rule[0].Body
}

function Get-RuleExpr {
    param([Parameter(Mandatory = $true)][string]$Body)
    $m = [regex]::Match($Body, '(?ms)^          expr: \|\n(?<e>.*?)(?=^          for:)')
    if (-not $m.Success) { throw '规则里找不到 `expr: |` 段(或它后面不是 for:)' }
    return $m.Groups['e'].Value
}

# expr 里 <Metric>{...} 选择器中某个 label 的字面量(=~ 的 | 分隔逐个拆开)。
function Get-LabelLiterals {
    param([Parameter(Mandatory = $true)][string]$Expr, [Parameter(Mandatory = $true)][string]$Metric, [Parameter(Mandatory = $true)][string]$Label)
    $literals = New-Object System.Collections.Generic.List[string]
    foreach ($sel in [regex]::Matches($Expr, ('\b{0}\{{(?<sel>[^}}]*)\}}' -f [regex]::Escape($Metric)))) {
        foreach ($m in [regex]::Matches($sel.Groups['sel'].Value, ('\b{0}\s*(=~|=)\s*"(?<v>[^"]*)"' -f [regex]::Escape($Label)))) {
            foreach ($v in ($m.Groups['v'].Value -split '\|')) { $literals.Add($v) }
        }
    }
    return @($literals | Sort-Object -Unique)
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/release-track-alerts.yaml'
$script:NodeCpp = Read-RepoText -RelativePath 'cpp/libs/engine/core/node/system/node/node.cpp'
$script:RouterMetrics = Read-RepoText -RelativePath 'go/client_rpc_router/internal/metrics/metrics.go'
$script:ReleaseTrackGo = Read-RepoText -RelativePath 'go/shared/releasetrack/releasetrack.go'
$script:NodeProto = Read-RepoText -RelativePath 'proto/common/base/node.proto'
$script:Outcomes = @([regex]::Matches($script:RouterMetrics, '(?m)^\s*Outcome[A-Za-z]+\s*=\s*"(?<v>[a-z_]+)"') | ForEach-Object { $_.Groups['v'].Value } | Sort-Object -Unique)
$script:Tracks = @([regex]::Matches($script:ReleaseTrackGo, '(?m)^\s*(stable|canary)Name\s*=\s*"(?<v>[a-z]+)"') | ForEach-Object { $_.Groups['v'].Value } | Sort-Object -Unique)

Write-Host ''
Write-Host '=== release_track_alerts_contract ==='

Test-Case '夹具:路由服的 Outcome* 常量与轨道名读得出来' {
    Assert-True -Condition ($script:Outcomes.Count -ge 10) -Because '夹具前提:metrics.go 的 Outcome* 常量写法变了要同步本用例'
    Assert-Equal -Expected 'canary,stable' -Actual ($script:Tracks -join ',') -Because '轨道名的权威来源是 go/shared/releasetrack(stableName / canaryName)'
}

Test-Case 'release-track-alerts.yaml:PrometheusRule 形状,两组的规则名单钉死,每条的 severity / summary / description / runbook_url / for 齐全' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 owner-epoch-alerts.yaml 同一安装方式)'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^  name:[ ]*release-track-alerts[ ]*$' -Because 'metadata.name'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    # 组名 → 按文件里的顺序排的 (规则名, severity)。
    $expected = [ordered]@{
        'release-track.zone'   = @(@{ Name = 'ReleaseTrackZoneMixedTooLong'; Severity = 'warning' })
        'release-track.canary' = @(@{ Name = 'ReleaseTrackCanaryForwardErrorsHigh'; Severity = 'critical' }, @{ Name = 'ReleaseTrackCanaryForwardLatencyHigh'; Severity = 'critical' })
    }
    foreach ($group in $expected.Keys) {
        $text = Get-AlertGroupText -Text $script:Alerts -Group $group
        Assert-Match -Text $text -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because "$group 的评估间隔"
        $rules = @(Get-AlertRules -GroupText $text)
        Assert-Equal -Expected (($expected[$group] | ForEach-Object { $_.Name }) -join ',') -Actual (($rules | ForEach-Object { $_.Name }) -join ',') -Because "$group 的规则名单钉死"
        for ($i = 0; $i -lt $rules.Count; $i++) {
            $rule = $rules[$i]
            Assert-Match -Text $rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $expected[$group][$i].Severity) -Because "$($rule.Name) 的 severity"
            foreach ($key in @('summary', 'description', 'runbook_url')) {
                Assert-Match -Text $rule.Body -Pattern ('(?m)^            {0}:' -f $key) -Because "$($rule.Name) 要有 $key"
            }
            Assert-Match -Text $rule.Body -Pattern '(?m)^          for:[ ]*\d+[mh][ ]*$' -Because "$($rule.Name) 要写 for"
            $runbook = [regex]::Match($rule.Body, '(?m)^            runbook_url:[ ]*(?<p>\S+)[ ]*$').Groups['p'].Value
            Assert-True -Condition (Test-Path -LiteralPath (Join-Path $script:RepoRoot $runbook)) -Because "$($rule.Name) 的 runbook_url 指向仓库里真实存在的手册($runbook)"
        }
    }
}

Test-Case 'ReleaseTrackZoneMixedTooLong:cpp_node_info 由 node.cpp 输出、带三个 label;node_type 3 / 4 是 scene / gate,release_track 取值是轨道名' {
    Assert-Match -Text $script:NodeCpp -Pattern 'BeginFamily\("cpp_node_info"' -Because 'cpp_node_info 必须由 C++ 节点输出(改名后规则静默失效)'
    foreach ($label in @('node_type', 'zone_id', 'release_track')) {
        Assert-Match -Text $script:NodeCpp -Pattern ('\{{"{0}",' -f $label) -Because "cpp_node_info 要带 $label label"
    }
    $expr = Get-RuleExpr -Body (Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group 'release-track.zone'))[0].Body
    $sceneType = [regex]::Match($script:NodeProto, '(?m)^\s*SceneNodeService\s*=\s*(?<n>\d+)\s*;').Groups['n'].Value
    $gateType = [regex]::Match($script:NodeProto, '(?m)^\s*GateNodeService\s*=\s*(?<n>\d+)\s*;').Groups['n'].Value
    Assert-True -Condition ($sceneType -and $gateType) -Because '夹具前提:node.proto 里读得出 SceneNodeService、GateNodeService 的枚举值'
    Assert-Equal -Expected ((@($sceneType, $gateType) | Sort-Object) -join ',') -Actual ((Get-LabelLiterals -Expr $expr -Metric 'cpp_node_info' -Label 'node_type') -join ',') -Because 'node_type 的取值要和 node.proto 的 scene / gate 枚举值一致'
    Assert-Equal -Expected ($script:Tracks -join ',') -Actual ((Get-LabelLiterals -Expr $expr -Metric 'cpp_node_info' -Label 'release_track') -join ',') -Because '只数已解析出轨道的节点(空串是启动前的占位)'
    Assert-Match -Text $expr -Pattern 'count by \(zone_id\)' -Because '按 zone 判混轨'
}

Test-Case 'ReleaseTrackCanaryForwardErrorsHigh:指标由路由服 metrics.go 注册,outcome 取值都是 Outcome* 常量,只比较 stable 与 canary' {
    $subsystem = [regex]::Match($script:RouterMetrics, '(?m)^const subsystem = "(?<s>[a-z_]+)"').Groups['s'].Value
    Assert-Equal -Expected 'client_rpc_router' -Actual $subsystem -Because '指标全名的前缀'
    Assert-Match -Text $script:RouterMetrics -Pattern '(?s)Name:\s*"forward_by_track_total",.*?\}, \[\]string\{"release_track", "outcome"\}\)' -Because 'forward_by_track_total 要注册,label 是 release_track、outcome'
    $expr = Get-RuleExpr -Body (Get-AlertRuleBody -Group 'release-track.canary' -Name 'ReleaseTrackCanaryForwardErrorsHigh')
    $referenced = @([regex]::Matches($expr, '\bclient_rpc_router_[a-z0-9_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'client_rpc_router_forward_by_track_total' -Actual ($referenced -join ',') -Because '只引用按轨道分的转发计数'
    foreach ($outcome in (Get-LabelLiterals -Expr $expr -Metric 'client_rpc_router_forward_by_track_total' -Label 'outcome')) {
        Assert-True -Condition ($script:Outcomes -ccontains $outcome) -Because "outcome=$outcome 必须是 metrics.go 的 Outcome* 常量"
    }
    Assert-Equal -Expected ($script:Tracks -join ',') -Actual ((Get-LabelLiterals -Expr $expr -Metric 'client_rpc_router_forward_by_track_total' -Label 'release_track') -join ',') -Because '两条轨道都要出现,unknown 不参与比较'
    $failureSelectors = @([regex]::Matches($expr, 'outcome=~"(?<v>[^"]*)"') | ForEach-Object { $_.Groups['v'].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 1 -Actual $failureSelectors.Count -Because 'canary 与 stable 两边的失败口径要完全相同,否则比较没有意义'
}

Test-Case 'ReleaseTrackCanaryForwardLatencyHigh:直方图由路由服 metrics.go 注册、与 forward_seconds 共用分桶,两边都取 p99、按 le 聚合,只比较 stable 与 canary' {
    Assert-Match -Text $script:RouterMetrics -Pattern '(?s)Name:\s*"forward_by_track_seconds",.*?Buckets:\s*forwardLatencyBuckets,\s*\}, \[\]string\{"release_track"\}\)' -Because 'forward_by_track_seconds 要注册,label 只有 release_track,分桶用共用的 forwardLatencyBuckets'
    Assert-Match -Text $script:RouterMetrics -Pattern '(?s)Name:\s*"forward_seconds",.*?Buckets:\s*forwardLatencyBuckets,' -Because 'forward_seconds 与按轨道的那份共用分桶,同一次 Forward 落进同一个桶'
    Assert-Match -Text $script:RouterMetrics -Pattern 'forwardByTrackSeconds\.WithLabelValues\(trackLabel\)\.Observe\(' -Because 'ObserveForward 要按会话轨道记耗时'
    $expr = Get-RuleExpr -Body (Get-AlertRuleBody -Group 'release-track.canary' -Name 'ReleaseTrackCanaryForwardLatencyHigh')
    $referenced = @([regex]::Matches($expr, '\bclient_rpc_router_[a-z0-9_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'client_rpc_router_forward_by_track_seconds_bucket,client_rpc_router_forward_by_track_seconds_count' -Actual ($referenced -join ',') -Because '只引用按轨道分的耗时直方图'
    Assert-Equal -Expected ($script:Tracks -join ',') -Actual ((Get-LabelLiterals -Expr $expr -Metric 'client_rpc_router_forward_by_track_seconds_bucket' -Label 'release_track') -join ',') -Because 'p99 两条轨道都要出现,unknown 不参与比较'
    Assert-Equal -Expected 'canary' -Actual ((Get-LabelLiterals -Expr $expr -Metric 'client_rpc_router_forward_by_track_seconds_count' -Label 'release_track') -join ',') -Because '最低样本量只看 canary'
    $quantiles = @([regex]::Matches($expr, 'histogram_quantile\((?<q>[0-9.]+),') | ForEach-Object { $_.Groups['q'].Value })
    Assert-Equal -Expected 4 -Actual $quantiles.Count -Because '倍数与绝对差两项判据各比一次 canary 与 stable'
    Assert-Equal -Expected '0.99' -Actual (($quantiles | Sort-Object -Unique) -join ',') -Because '两边比较的分位数要相同'
    Assert-Equal -Expected 4 -Actual ([regex]::Matches($expr, 'sum by \(le\) \(rate\(client_rpc_router_forward_by_track_seconds_bucket\{')).Count -Because '每个分位数都先按 le 汇总全部路由服实例'
}

Test-Case '三条告警的名字在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in @('ReleaseTrackZoneMixedTooLong', 'ReleaseTrackCanaryForwardErrorsHigh', 'ReleaseTrackCanaryForwardLatencyHigh')) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'release_track_alerts_contract')
