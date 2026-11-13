#requires -Version 7
<#
.SYNOPSIS
    scene 侧版本错位告警的契约测试(2026-11-12,数据审计遗留·扣还计数)。

.DESCRIPTION
    deploy/k8s/persist-version-skew-alerts.yaml 的规则读 scene 节点的 scene_currency_clawback_version_skew_total
    (cpp/libs/services/scene/player/system/currency_metrics.cpp 的 CollectCurrencyClawback,计数在
    cpp/libs/modules/currency/system/currency_clawback_stats.h)。这里钉的都是「写错了不报错、只会永远不响」的东西:
      - 规则引用的每个指标名都由 C++ 采集回调的 BeginFamily 导出,采集回调由 scene main 登记;
      - 计数在 CurrencySystem::AddCurrency 跳过补缴的那一支里自增,与 where=currency_clawback 的 WARN 同一处;
      - PrometheusRule 形状、规则名单、每条规则的 severity / summary / description / runbook_url / for;
      - 告警名在 deploy/k8s 的全部告警文件里不重名。
    只读文本,不执行部署入口,不访问任何集群。规则语法由 promtool 另验(见 docs/PROGRESS.md 对应条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/persist_version_skew_alerts_contract.tests.ps1
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

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/persist-version-skew-alerts.yaml'
$script:Collector = Read-RepoText -RelativePath 'cpp/libs/services/scene/player/system/currency_metrics.cpp'
$script:Stats = Read-RepoText -RelativePath 'cpp/libs/modules/currency/system/currency_clawback_stats.h'
$script:Currency = Read-RepoText -RelativePath 'cpp/libs/modules/currency/system/currency_system.cpp'
$script:SceneMain = Read-RepoText -RelativePath 'cpp/nodes/scene/main.cpp'

# C++ 导出的指标族名。
$script:Families = @([regex]::Matches($script:Collector, 'BeginFamily\("(?<n>scene_[a-z_]+)"') | ForEach-Object { $_.Groups['n'].Value } | Sort-Object -Unique)

$script:ExpectedRules = [ordered]@{
    'SceneCurrencyClawbackVersionSkew' = 'warning'
}

Test-Case '夹具:C++ 导出的指标族;计数在跳过补缴的那一支自增;采集回调由 scene main 登记' {
    Assert-Equal -Expected 'scene_currency_clawback_version_skew_total' -Actual ($script:Families -join ',') -Because '采集回调的指标族;增删指标要同步告警与本用例'
    Assert-Match -Text $script:Collector -Pattern 'currency_clawback_stats::ReadVersionSkewSkipped\(\)' -Because '导出的是补缴因版本错位被跳过的计数'
    Assert-Match -Text $script:Stats -Pattern 'inline void IncVersionSkewSkipped\(\)' -Because '计数的写入口'
    # 自增与 WARN 在同一个分支里:从 `if (clawbackSkippedForVersionSkew)` 到 where=currency_clawback 那一行之间没有别的块。
    Assert-Match -Text $script:Currency -Pattern '(?s)if \(clawbackSkippedForVersionSkew\)\s*\{[^}]*currency_clawback_stats::IncVersionSkewSkipped\(\);[^}]*where=currency_clawback' `
        -Because 'AddCurrency 跳过补缴时计数与 WARN 同一处,计数口径与日志口径一致'
    Assert-Match -Text $script:SceneMain -Pattern '(?m)^\s*RegisterCurrencyMetrics\(\);' -Because 'scene main 要登记采集回调,否则指标不存在、告警永远不响'
}

Test-Case 'persist-version-skew-alerts.yaml:PrometheusRule 形状,scene.persist-version-skew 组的规则名单与每条规则的字段' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 owner-epoch-alerts.yaml 同一安装方式)'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^  name:[ ]*persist-version-skew-alerts[ ]*$' -Because 'metadata.name'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    $group = Get-AlertGroupText -Text $script:Alerts -Group 'scene.persist-version-skew'
    Assert-Match -Text $group -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because '评估间隔'
    $rules = @(Get-AlertRules -GroupText $group)
    Assert-Equal -Expected (($script:ExpectedRules.Keys | Sort-Object) -join ',') -Actual ((@($rules | ForEach-Object { $_.Name }) | Sort-Object) -join ',') -Because '规则名单钉死'
    foreach ($rule in $rules) {
        $severity = $script:ExpectedRules[$rule.Name]
        Assert-Match -Text $rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $severity) -Because "$($rule.Name) 的 severity 应为 $severity"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            summary:' -Because "$($rule.Name) 要有 summary"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            description:' -Because "$($rule.Name) 要有 description"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            runbook_url:[ ]*docs/design/mixed-version-persist-field-protection\.md[ ]*$' -Because "$($rule.Name) 指向混版本保护设计 §4.4"
        Assert-Match -Text $rule.Body -Pattern '(?m)^          for:[ ]*0m[ ]*$' -Because "$($rule.Name) 健康时恒 0,出现就报"
    }
}

Test-Case 'persist-version-skew-alerts.yaml:引用的指标都由 C++ 导出,按 increase 读计数' {
    foreach ($rule in (Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group 'scene.persist-version-skew'))) {
        $expr = Get-RuleExpr -Body $rule.Body
        $referenced = @([regex]::Matches($expr, '\bscene_[a-z_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
        Assert-True -Condition ($referenced.Count -gt 0) -Because "$($rule.Name) 至少引用一个 scene_* 指标"
        foreach ($metric in $referenced) {
            Assert-True -Condition ($script:Families -ccontains $metric) -Because "$($rule.Name):$metric 必须由 currency_metrics.cpp 导出(改名后告警静默失效)"
        }
        Assert-Match -Text $expr -Pattern 'increase\(scene_currency_clawback_version_skew_total\[10m\]\)' -Because "$($rule.Name) 读计数的 10 分钟增量"
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

exit (Complete-TestRun -SuiteName 'persist_version_skew_alerts_contract')
