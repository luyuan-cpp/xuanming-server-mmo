#requires -Version 7
<#
.SYNOPSIS
    scene 侧版本错位与资产通道归属告警的契约测试(2026-11-12 扣还计数;2026-11-27 补资产通道两条)。

.DESCRIPTION
    deploy/k8s/persist-version-skew-alerts.yaml 的规则读 scene 节点的 scene_currency_clawback_version_skew_total
    (cpp/libs/services/scene/player/system/currency_metrics.cpp 的 CollectCurrencyClawback,计数在
    cpp/libs/modules/currency/system/currency_clawback_stats.h)与 scene_asset_op_version_skew_total{where};
    deploy/k8s/owner-epoch-alerts.yaml 的 owner-epoch.scene 组读 scene_asset_op_owner_epoch_unknown_total
    (两者都在 cpp/libs/services/scene/player/system/asset_op_system.cpp 的 PlayerAssetOpSystem::WriteMetrics,
    原来是 asset-channel-alerts.yaml 与 cross-zone-failure-test-runbook.md 里的 LogQL 值班查询)。
    这里钉的都是「写错了不报错、只会永远不响」的东西:
      - 规则引用的每个指标名都由 C++ 采集回调的 BeginFamily 导出,采集回调由 scene main 登记;
      - 计数在 CurrencySystem::AddCurrency 跳过补缴的那一支里自增,与 where=currency_clawback 的 WARN 同一处;
        资产通道每处 where=asset_* 日志与 owner_epoch unknown 日志前各计一次,where 取值与日志同名;
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
$script:OwnerEpochAlerts = Read-RepoText -RelativePath 'deploy/k8s/owner-epoch-alerts.yaml'
$script:Collector = Read-RepoText -RelativePath 'cpp/libs/services/scene/player/system/currency_metrics.cpp'
$script:Stats = Read-RepoText -RelativePath 'cpp/libs/modules/currency/system/currency_clawback_stats.h'
$script:Currency = Read-RepoText -RelativePath 'cpp/libs/modules/currency/system/currency_system.cpp'
$script:AssetOp = Read-RepoText -RelativePath 'cpp/libs/services/scene/player/system/asset_op_system.cpp'
$script:SceneMain = Read-RepoText -RelativePath 'cpp/nodes/scene/main.cpp'

# C++ 导出的指标族名。
$script:Families = @([regex]::Matches($script:Collector, 'BeginFamily\("(?<n>scene_[a-z_]+)"') | ForEach-Object { $_.Groups['n'].Value } | Sort-Object -Unique)
$script:AssetOpFamilies = @([regex]::Matches($script:AssetOp, 'BeginFamily\("(?<n>scene_[a-z_]+)"') | ForEach-Object { $_.Groups['n'].Value } | Sort-Object -Unique)
# 资产通道版本错位的 where 取值(asset_op_system.cpp 的 kVersionSkewWhereNames)。
$script:SkewWheres = @(
    [regex]::Matches([regex]::Match($script:AssetOp, '(?s)kVersionSkewWhereNames\[\]\s*=\s*\{(?<body>[^}]*)\}').Groups['body'].Value, '"(?<v>[a-z_]+)"') |
        ForEach-Object { $_.Groups['v'].Value })

# 每条规则:所在文件与组、级别、读的计数族、按什么分组、runbook。
$script:ExpectedRules = [ordered]@{
    'SceneCurrencyClawbackVersionSkew' = @{ File = 'persist'; Group = 'scene.persist-version-skew'; Severity = 'warning'; Series = 'scene_currency_clawback_version_skew_total'; By = 'namespace'; Runbook = 'docs/design/mixed-version-persist-field-protection.md' }
    'SceneAssetOpVersionSkew'          = @{ File = 'persist'; Group = 'scene.persist-version-skew'; Severity = 'warning'; Series = 'scene_asset_op_version_skew_total'; By = 'namespace, where'; Runbook = 'docs/design/mixed-version-persist-field-protection.md' }
    'SceneAssetOpOwnerEpochUnknown'    = @{ File = 'owner'; Group = 'owner-epoch.scene'; Severity = 'warning'; Series = 'scene_asset_op_owner_epoch_unknown_total'; By = 'namespace'; Runbook = 'docs/ops/cross-zone-failure-test-runbook.md' }
}

function Get-RuleByName {
    param([Parameter(Mandatory = $true)][string]$Name)
    $spec = $script:ExpectedRules[$Name]
    $text = if ($spec.File -eq 'persist') { $script:Alerts } else { $script:OwnerEpochAlerts }
    return @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $text -Group $spec.Group) | Where-Object Name -ceq $Name)[0]
}

Test-Case '夹具:C++ 导出的指标族;计数与同名日志同一处;采集回调由 scene main 登记' {
    Assert-Equal -Expected 'scene_currency_clawback_version_skew_total' -Actual ($script:Families -join ',') -Because '采集回调的指标族;增删指标要同步告警与本用例'
    Assert-Match -Text $script:Collector -Pattern 'currency_clawback_stats::ReadVersionSkewSkipped\(\)' -Because '导出的是补缴因版本错位被跳过的计数'
    Assert-Match -Text $script:Stats -Pattern 'inline void IncVersionSkewSkipped\(\)' -Because '计数的写入口'
    # 自增与 WARN 在同一个分支里:从 `if (clawbackSkippedForVersionSkew)` 到 where=currency_clawback 那一行之间没有别的块。
    Assert-Match -Text $script:Currency -Pattern '(?s)if \(clawbackSkippedForVersionSkew\)\s*\{[^}]*currency_clawback_stats::IncVersionSkewSkipped\(\);[^}]*where=currency_clawback' `
        -Because 'AddCurrency 跳过补缴时计数与 WARN 同一处,计数口径与日志口径一致'
    Assert-Match -Text $script:SceneMain -Pattern '(?m)^\s*RegisterCurrencyMetrics\(\);' -Because 'scene main 要登记采集回调,否则指标不存在、告警永远不响'

    # 2026-11-27:资产通道的版本错位与归属未铸造被挡(asset_op_system.cpp 的 PlayerAssetOpSystem::WriteMetrics)。
    Assert-Equal -Expected 'scene_asset_op_owner_epoch_unknown_total,scene_asset_op_version_skew_total' -Actual ($script:AssetOpFamilies -join ',') -Because 'asset_op_system.cpp 导出的指标族'
    Assert-Equal -Expected 'asset_debit,asset_recall_debt,asset_recall,asset_request,asset_currency,asset_config' -Actual ($script:SkewWheres -join ',') -Because 'where 取值与日志 metric=persist_version_skew where=asset_* 同名'
    Assert-Match -Text $script:AssetOp -Pattern 'static_assert\(std::size\(kVersionSkewWhereNames\) == static_cast<std::size_t>\(VersionSkewWhere::kCount\)' -Because '名表与枚举同长'
    foreach ($where in $script:SkewWheres) {
        $logs = [regex]::Matches($script:AssetOp, ('LOG_WARN << "\[AssetOp\] metric=persist_version_skew where={0}[ "]' -f [regex]::Escape($where))).Count
        $counted = [regex]::Matches($script:AssetOp, ('CountVersionSkew\(VersionSkewWhere::k[A-Za-z]+\);\s*\n\s*LOG_WARN << "\[AssetOp\] metric=persist_version_skew where={0}[ "]' -f [regex]::Escape($where))).Count
        Assert-True -Condition ($logs -gt 0) -Because "前提:有 where=$where 的日志"
        Assert-Equal -Expected $logs -Actual $counted -Because "where=$where 的日志前面都计一次"
    }
    Assert-Match -Text $script:AssetOp -Pattern 'gBlockedOwnerEpochUnknown\.fetch_add\(1, std::memory_order_relaxed\);\s*\n\s*LOG_WARN << "\[AssetOp\] blocked: owner_epoch unknown' -Because '归属未铸造被挡与日志同一处计数'
    Assert-Match -Text $script:SceneMain -Pattern '(?m)^\s*PlayerAssetOpSystem::RegisterMetrics\(\);' -Because 'scene main 要登记资产通道的采集回调'
}

# 2026-11-19 评审补:同一批的腾位通知接线。少了这一行不报错,收件箱的背包满延后只会退化成按退避等(最长约 60 秒)。
Test-Case 'scene main:收件箱初始化之后装背包腾位通知的订阅方' {
    Assert-Match -Text $script:SceneMain -Pattern '(?s)PlayerInboxSystem::Initialize\(\);.{0,400}?BagService::SetSpaceFreedListener\(&PlayerInboxSystem::OnBagSpaceFreed\);' `
        -Because '订阅方装在 PlayerInboxSystem::Initialize 之后(bag-rule-policy-layering.md §6.6「腾位唤醒」)'
}

Test-Case '两份告警文件:PrometheusRule 形状,组与规则名单,每条规则的字段' {
    foreach ($pair in @(@{ Text = $script:Alerts; Name = 'persist-version-skew-alerts' }, @{ Text = $script:OwnerEpochAlerts; Name = 'owner-epoch-alerts' })) {
        Assert-Match -Text $pair.Text -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because "$($pair.Name) 必须是 PrometheusRule"
        Assert-Match -Text $pair.Text -Pattern ('(?m)^  name:[ ]*{0}[ ]*$' -f [regex]::Escape($pair.Name)) -Because 'metadata.name'
        Assert-Match -Text $pair.Text -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    }
    $persistRules = @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group 'scene.persist-version-skew') | ForEach-Object { $_.Name })
    Assert-Equal -Expected 'SceneCurrencyClawbackVersionSkew,SceneAssetOpVersionSkew' -Actual ($persistRules -join ',') -Because 'scene.persist-version-skew 组的规则名单钉死'
    Assert-Match -Text (Get-AlertGroupText -Text $script:Alerts -Group 'scene.persist-version-skew') -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because '评估间隔'
    $ownerGroup = Get-AlertGroupText -Text $script:OwnerEpochAlerts -Group 'owner-epoch.scene'
    Assert-Match -Text $ownerGroup -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because 'owner-epoch.scene 组的评估间隔'
    Assert-Equal -Expected 'SceneAssetOpOwnerEpochUnknown' -Actual ((@(Get-AlertRules -GroupText $ownerGroup | ForEach-Object { $_.Name })) -join ',') -Because 'owner-epoch.scene 组的规则名单钉死'
    foreach ($name in $script:ExpectedRules.Keys) {
        $spec = $script:ExpectedRules[$name]
        $rule = Get-RuleByName -Name $name
        Assert-True -Condition ($null -ne $rule) -Because "$name 在 $($spec.Group) 组里"
        Assert-Match -Text $rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $spec.Severity) -Because "$name 的 severity 应为 $($spec.Severity)"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            summary:' -Because "$name 要有 summary"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            description:' -Because "$name 要有 description"
        Assert-Match -Text $rule.Body -Pattern ('(?m)^            runbook_url:[ ]*{0}[ ]*$' -f [regex]::Escape($spec.Runbook)) -Because "$name 的 runbook_url"
        Assert-Match -Text $rule.Body -Pattern '(?m)^          for:[ ]*0m[ ]*$' -Because "$name 健康时恒 0(或只在发布窗口出现),出现就报"
    }
}

Test-Case '式子:引用的指标都由 C++ 导出,按 increase 读计数并带新序列一支,只按低基数标签分组' {
    foreach ($name in $script:ExpectedRules.Keys) {
        $spec = $script:ExpectedRules[$name]
        $expr = Get-RuleExpr -Body (Get-RuleByName -Name $name).Body
        $referenced = @([regex]::Matches($expr, '\bscene_[a-z_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
        Assert-Equal -Expected $spec.Series -Actual ($referenced -join ',') -Because "$name 只读 $($spec.Series)"
        Assert-True -Condition (($script:Families + $script:AssetOpFamilies) -ccontains $spec.Series) -Because "${name}:$($spec.Series) 必须由 C++ 导出(改名后告警静默失效)"
        $series = [regex]::Escape($spec.Series)
        Assert-Match -Text $expr -Pattern ('sum by \({0}\) \(increase\({1}\[10m\]\)\) > 0' -f [regex]::Escape($spec.By), $series) -Because "$name 读计数的 10 分钟增量,按 ($($spec.By)) 分组"
        # 回滚出来的旧 Pod 可能在第一次被抓取之前就计了数:首个样本已不是 0,increase 看不到,要靠「新序列」这一支。
        Assert-Match -Text $expr -Pattern ('(?s)\bor\b.*{0}\s+unless {0} offset 10m' -f $series) -Because "$name 带新序列一支,窗口与 increase 相同"
        foreach ($by in [regex]::Matches($expr, '\bby \((?<labels>[^)]*)\)')) {
            foreach ($label in @($by.Groups['labels'].Value -split ',' | ForEach-Object { $_.Trim() })) {
                Assert-True -Condition (@('namespace', 'where') -contains $label) -Because "$name 不按 $label 分组(只许低基数标签)"
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

exit (Complete-TestRun -SuiteName 'persist_version_skew_alerts_contract')
