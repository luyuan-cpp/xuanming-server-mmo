#requires -Version 7
<#
.SYNOPSIS
    场景归属对账告警的契约测试(缺口清单第 33 条,2026-11-10)。

.DESCRIPTION
    deploy/k8s/scene-ownership-alerts.yaml 的规则读 scene 节点的 scene_ownership_* 指标
    (cpp/libs/services/scene/world/scene_ownership_reconciler.cpp 的 CollectSceneOwnership)。这里钉的都是「写错了不报错、只会永远不响」的东西:
      - 规则引用的每个指标名都由 C++ 采集回调的 BeginFamily 导出;
      - stage 取值是采集回调里写死的样本标签,outcome 取值是 scene_ownership_reconciler.h 的 kSweepOutcomeNames;
      - PrometheusRule 形状、规则名单、每条规则的 severity / summary / description / runbook_url / for;
      - 告警名在 deploy/k8s 的全部告警文件里不重名。
    只读文本,不执行部署入口,不访问任何集群。规则语法由 promtool 另验(见 docs/PROGRESS.md 对应条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/scene_ownership_alerts_contract.tests.ps1
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

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/scene-ownership-alerts.yaml'
$script:Collector = Read-RepoText -RelativePath 'cpp/libs/services/scene/world/scene_ownership_reconciler.cpp'
$script:Header = Read-RepoText -RelativePath 'cpp/libs/services/scene/world/scene_ownership_reconciler.h'

# C++ 导出的指标族名、stage 样本标签与 kSweepOutcomeNames。
$script:Families = @([regex]::Matches($script:Collector, 'BeginFamily\("(?<n>scene_ownership_[a-z_]+)"') | ForEach-Object { $_.Groups['n'].Value } | Sort-Object -Unique)
$script:Stages = @([regex]::Matches($script:Collector, '\{"stage",\s*"(?<v>[a-z_]+)"\}') | ForEach-Object { $_.Groups['v'].Value } | Sort-Object -Unique)
$outcomeTable = [regex]::Match($script:Header, '(?s)kSweepOutcomeNames\[\]\s*=\s*\{(?<body>.*?)\};')
if (-not $outcomeTable.Success) { throw '找不到 kSweepOutcomeNames' }
$script:Outcomes = @([regex]::Matches($outcomeTable.Groups['body'].Value, '"(?<v>[a-z_]+)"') | ForEach-Object { $_.Groups['v'].Value } | Sort-Object -Unique)

$script:ExpectedRules = [ordered]@{
    'SceneOwnershipUnownedScenesReleased' = 'warning'
    'SceneOwnershipMassRelease'           = 'critical'
    'SceneOwnershipNodeIdTakenOver'       = 'critical'
    'SceneOwnershipReleaseBacklog'        = 'warning'
    'SceneOwnershipReconcileBlind'        = 'warning'
}

Test-Case '夹具:C++ 导出的指标族、stage 与 outcome 取值' {
    Assert-Equal -Expected 'scene_ownership_observations_total,scene_ownership_release_deferred_scenes,scene_ownership_releases_total,scene_ownership_releasing_scenes,scene_ownership_superseded,scene_ownership_sweeps_total,scene_ownership_tracked_scenes' `
        -Actual ($script:Families -join ',') -Because '采集回调的指标族;增删指标要同步告警与本用例'
    Assert-Equal -Expected 'destroyed,drain_started' -Actual ($script:Stages -join ',') -Because 'releases_total 的 stage 取值'
    Assert-Equal -Expected 'no_scenes,previous_in_flight,redis_unavailable,send_failed,sent' -Actual ($script:Outcomes -join ',') -Because 'kSweepOutcomeNames'
}

Test-Case 'scene-ownership-alerts.yaml:PrometheusRule 形状,scene.ownership 组的规则名单与每条规则的字段' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 owner-epoch-alerts.yaml 同一安装方式)'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^  name:[ ]*scene-ownership-alerts[ ]*$' -Because 'metadata.name'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    $group = Get-AlertGroupText -Text $script:Alerts -Group 'scene.ownership'
    Assert-Match -Text $group -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because '评估间隔'
    $rules = @(Get-AlertRules -GroupText $group)
    Assert-Equal -Expected (($script:ExpectedRules.Keys | Sort-Object) -join ',') -Actual ((@($rules | ForEach-Object { $_.Name }) | Sort-Object) -join ',') -Because '规则名单钉死'
    foreach ($rule in $rules) {
        $severity = $script:ExpectedRules[$rule.Name]
        Assert-Match -Text $rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $severity) -Because "$($rule.Name) 的 severity 应为 $severity"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            summary:' -Because "$($rule.Name) 要有 summary"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            description:' -Because "$($rule.Name) 要有 description"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            runbook_url:[ ]*docs/ops/scene-node-role-split\.md[ ]*$' -Because "$($rule.Name) 指向 scene 节点运维手册"
        Assert-Match -Text $rule.Body -Pattern '(?m)^          for:[ ]*\d+[ms][ ]*$' -Because "$($rule.Name) 要写 for"
    }
}

Test-Case 'scene-ownership-alerts.yaml:引用的指标都由 C++ 导出,stage / outcome 取值都在 C++ 的取值表里' {
    foreach ($rule in (Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group 'scene.ownership'))) {
        $expr = Get-RuleExpr -Body $rule.Body
        $referenced = @([regex]::Matches($expr, '\bscene_ownership_[a-z_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
        Assert-True -Condition ($referenced.Count -gt 0) -Because "$($rule.Name) 至少引用一个 scene_ownership_* 指标"
        foreach ($metric in $referenced) {
            Assert-True -Condition ($script:Families -ccontains $metric) -Because "$($rule.Name):$metric 必须由 scene_ownership_reconciler.cpp 导出(改名后告警静默失效)"
        }
        foreach ($m in [regex]::Matches($expr, '\bstage\s*=\s*"(?<v>[^"]*)"')) {
            Assert-True -Condition ($script:Stages -ccontains $m.Groups['v'].Value) -Because "$($rule.Name):stage=$($m.Groups['v'].Value) 必须是 C++ 导出的取值"
        }
        foreach ($m in [regex]::Matches($expr, '\boutcome\s*=~\s*"(?<v>[^"]*)"')) {
            foreach ($v in ($m.Groups['v'].Value -split '\|')) {
                Assert-True -Condition ($script:Outcomes -ccontains $v) -Because "$($rule.Name):outcome=$v 必须是 kSweepOutcomeNames 之一"
            }
        }
    }
}

Test-Case '五条告警的名字在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in $script:ExpectedRules.Keys) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'scene_ownership_alerts_contract')
