#requires -Version 7
<#
.SYNOPSIS
    战斗结算收尾告警的契约测试(2026-11-27,原 LogQL 值班查询改 PromQL)。

.DESCRIPTION
    deploy/k8s/battle-settlement-alerts.yaml 的规则读两处 C++ 导出的指标:
      - battle 的 battle_sql_outcome_stuck_rooms / battle_rooms(cpp/nodes/battle/logic/battle_room_manager.cpp 的 CollectMetrics,
        battle main 登记);
      - scene 的 scene_battle_freeze_events_total{event}(cpp/libs/services/scene/battle/system/player_battle.cpp 的
        WriteBattleFreezeMetrics,scene main 登记;event 取 battle_freeze_hold_rules.h 的 kFreezeHoldEventNames)。
    这里钉的都是「写错了不报错、只会永远不响」的东西:指标名与 event 取值都由 C++ 导出;每个计数点与同名日志一一对应
    (计数口径与原来的 LogQL 查询相同);采集回调有人登记;规则形状、名单、字段;按低基数 label 分组;告警名全局不重名;
    asset-channel-alerts.yaml 文件头不再保留这四条 LogQL(Loki ruler 接线之后会与本文件重复告警)。
    只读文本,不执行部署入口,不访问任何集群。规则语法由 promtool 另验(见 docs/PROGRESS.md 对应条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/battle_settlement_alerts_contract.tests.ps1
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

# 一段文本里某个子串(按字面)出现的次数。
function Get-LiteralCount {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Literal)
    return [regex]::Matches($Text, [regex]::Escape($Literal)).Count
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/battle-settlement-alerts.yaml'
$script:AssetAlerts = Read-RepoText -RelativePath 'deploy/k8s/asset-channel-alerts.yaml'
$script:Rules = Read-RepoText -RelativePath 'cpp/libs/services/scene/battle/system/battle_freeze_hold_rules.h'
$script:PlayerBattle = Read-RepoText -RelativePath 'cpp/libs/services/scene/battle/system/player_battle.cpp'
$script:RoomManager = Read-RepoText -RelativePath 'cpp/nodes/battle/logic/battle_room_manager.cpp'
$script:SceneMain = Read-RepoText -RelativePath 'cpp/nodes/scene/main.cpp'
$script:BattleMain = Read-RepoText -RelativePath 'cpp/nodes/battle/main.cpp'

# C++ 导出的指标族名(BeginFamily 的名字参数)。
$script:SceneFamilies = @([regex]::Matches($script:PlayerBattle, 'BeginFamily\("(?<n>scene_[a-z_]+)"') | ForEach-Object { $_.Groups['n'].Value } | Sort-Object -Unique)
$script:BattleFamilies = @([regex]::Matches($script:RoomManager, 'BeginFamily\("(?<n>battle_[a-z_]+)"') | ForEach-Object { $_.Groups['n'].Value } | Sort-Object -Unique)
# kFreezeHoldEventNames 的取值(event 标签)。
$script:FreezeEvents = @(
    [regex]::Matches([regex]::Match($script:Rules, '(?s)kFreezeHoldEventNames\[\]\s*=\s*\{(?<body>[^}]*)\}').Groups['body'].Value, '"(?<v>[a-z_]+)"') |
        ForEach-Object { $_.Groups['v'].Value })

$script:ExpectedGroups = [ordered]@{
    'battle.settlement-rooms' = [ordered]@{ 'BattleSqlOutcomeRoomsStuck' = 'critical' }
    'scene.battle-freeze'     = [ordered]@{
        'SceneBattleFreezeHoldCapReached' = 'warning'
        'SceneBattleItemHoldOverdue'      = 'warning'
        'SceneJianghuDuelFeeOverdue'      = 'warning'
    }
}
# scene 规则读的 event 取值,与它取代的那条 LogQL 的日志名。
$script:SceneRuleEvents = [ordered]@{
    'SceneBattleFreezeHoldCapReached' = @{ Event = 'cap_reached'; Log = 'battle_freeze_hold_cap_reached' }
    'SceneBattleItemHoldOverdue'      = @{ Event = 'item_hold_overdue'; Log = 'battle_item_hold_overdue' }
    'SceneJianghuDuelFeeOverdue'      = @{ Event = 'duel_fee_overdue'; Log = 'jianghu_duel_fee_overdue' }
}

Write-Host ''
Write-Host '=== battle_settlement_alerts_contract ==='

Test-Case '夹具:C++ 导出的指标族与 event 名表;采集回调由 scene / battle main 登记' {
    Assert-Equal -Expected 'held_complete,held_unknown,cap_reached,query_lost,held_pending,prepare_expired,expired_released,item_hold_overdue,duel_fee_overdue' `
        -Actual ($script:FreezeEvents -join ',') -Because 'kFreezeHoldEventNames 的取值与顺序(告警按名字读,改名要同步本文件与告警)'
    Assert-Match -Text $script:Rules -Pattern 'static_assert\(std::size\(kFreezeHoldEventNames\) == static_cast<std::size_t>\(FreezeHoldEvent::kCount\)' -Because '名表与枚举同长'
    Assert-Equal -Expected 'scene_battle_freeze_events_total' -Actual ($script:SceneFamilies -join ',') -Because 'player_battle.cpp 导出的指标族'
    Assert-Match -Text $script:PlayerBattle -Pattern '(?s)WriteBattleFreezeMetrics\(prometheus_text::Writer& writer\).*?battle_freeze_hold::kFreezeHoldEventNames\[i\]' -Because 'event 标签取自名表'
    foreach ($family in @('battle_rooms', 'battle_sql_outcome_stuck_rooms')) {
        Assert-True -Condition ($script:BattleFamilies -ccontains $family) -Because "battle_room_manager.cpp 要导出 $family"
    }
    Assert-Match -Text $script:SceneMain -Pattern '(?m)^\s*PlayerBattleSystem::RegisterMetrics\(\);' -Because 'scene main 不登记采集回调,指标不存在、告警永远不响'
    Assert-Match -Text $script:BattleMain -Pattern '(?m)^\s*BattleRoomManager::Instance\(\)\.RegisterMetrics\(\);' -Because 'battle main 不登记采集回调,指标不存在、告警永远不响'
}

Test-Case '计数口径:每处同名日志紧跟一次计数(与原来按日志行数的 LogQL 同一口径)' {
    $pairs = @(
        @{ Log = 'battle_freeze_hold_cap_reached'; Enum = 'kCapReached' },
        @{ Log = 'battle_item_hold_overdue'; Enum = 'kItemHoldOverdue' },
        @{ Log = 'jianghu_duel_fee_overdue'; Enum = 'kDuelFeeOverdue' },
        @{ Log = 'battle_prepare_expired'; Enum = 'kPrepareExpired' },
        @{ Log = 'battle_freeze_expired'; Enum = 'kExpiredReleased' },
        @{ Log = 'battle_terminal_query_lost'; Enum = 'kQueryLost' }
    )
    foreach ($pair in $pairs) {
        $logs = Get-LiteralCount -Text $script:PlayerBattle -Literal ('[PlayerBattle] metric={0} player_id=' -f $pair.Log)
        $counts = Get-LiteralCount -Text $script:PlayerBattle -Literal ('CountFreezeHoldEvent(battle_freeze_hold::FreezeHoldEvent::{0});' -f $pair.Enum)
        Assert-True -Condition ($logs -gt 0) -Because "前提:player_battle.cpp 里有 metric=$($pair.Log) 的日志"
        Assert-Equal -Expected $logs -Actual $counts -Because "metric=$($pair.Log) 每打一处都要计一次 $($pair.Enum)"
        Assert-Match -Text $script:PlayerBattle -Pattern ('(?s)metric={0} player_id=.{{0,700}}?CountFreezeHoldEvent\(battle_freeze_hold::FreezeHoldEvent::{1}\);' -f [regex]::Escape($pair.Log), $pair.Enum) `
            -Because "$($pair.Enum) 与 metric=$($pair.Log) 在同一分支里"
    }
    Assert-Match -Text $script:RoomManager -Pattern '(?s)void BattleRoomManager::CollectMetrics\(.*?OutcomeStuck\(\*room\)' -Because '滞留房间数与滞留提醒行同一判据(OutcomeStuck)'
    Assert-Match -Text $script:RoomManager -Pattern '(?s)void BattleRoomManager::ReportStuckOutcomeRooms\(\).*?OutcomeStuck\(\*room\)' -Because '滞留提醒行也按 OutcomeStuck 数'
}

Test-Case 'battle-settlement-alerts.yaml:PrometheusRule 形状、两组的规则名单与每条规则的字段' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^  name:[ ]*battle-settlement-alerts[ ]*$' -Because 'metadata.name'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    Assert-Equal -Expected (@($script:ExpectedGroups.Keys) -join ',') -Actual ((@([regex]::Matches($script:Alerts, '(?m)^    - name:[ ]*(\S+)[ ]*$') | ForEach-Object { $_.Groups[1].Value })) -join ',') -Because '组名单'
    foreach ($groupName in $script:ExpectedGroups.Keys) {
        $group = Get-AlertGroupText -Text $script:Alerts -Group $groupName
        Assert-Match -Text $group -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because "$groupName 的评估间隔"
        $expected = $script:ExpectedGroups[$groupName]
        $rules = @(Get-AlertRules -GroupText $group)
        Assert-Equal -Expected (@($expected.Keys) -join ',') -Actual ((@($rules | ForEach-Object { $_.Name })) -join ',') -Because "$groupName 的规则名单钉死"
        foreach ($rule in $rules) {
            $severity = $expected[$rule.Name]
            Assert-Match -Text $rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $severity) -Because "$($rule.Name) 的 severity 应为 $severity"
            foreach ($key in @('summary', 'description', 'runbook_url')) {
                Assert-Match -Text $rule.Body -Pattern ('(?m)^            {0}:' -f $key) -Because "$($rule.Name) 要有 $key"
            }
            $runbook = [regex]::Match($rule.Body, '(?m)^            runbook_url:[ ]*(\S+)[ ]*$').Groups[1].Value
            Assert-True -Condition (Test-Path -LiteralPath (Join-Path $script:RepoRoot $runbook)) -Because "$($rule.Name) 的 runbook_url 指向仓库里存在的文档($runbook)"
        }
    }
}

Test-Case '式子:指标都由 C++ 导出;只按低基数 label 分组;scene 规则读对的 event,并带新序列一支' {
    foreach ($groupName in $script:ExpectedGroups.Keys) {
        foreach ($rule in (Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group $groupName))) {
            $expr = Get-RuleExpr -Body $rule.Body
            $referenced = @([regex]::Matches($expr, '\b(?:scene|battle)_[a-z_]+\b') | ForEach-Object { $_.Value } | Sort-Object -Unique)
            Assert-True -Condition ($referenced.Count -gt 0) -Because "$($rule.Name) 至少引用一个指标"
            foreach ($metric in $referenced) {
                Assert-True -Condition (($script:SceneFamilies + $script:BattleFamilies) -ccontains $metric) -Because "$($rule.Name):$metric 必须由 C++ 导出(改名后告警静默失效)"
            }
            foreach ($by in [regex]::Matches($expr, '\bby \((?<labels>[^)]*)\)')) {
                foreach ($label in @($by.Groups['labels'].Value -split ',' | ForEach-Object { $_.Trim() })) {
                    Assert-True -Condition (@('namespace', 'instance') -contains $label) -Because "$($rule.Name) 不按 $label 分组(只许低基数标签)"
                }
            }
            foreach ($m in [regex]::Matches($expr, 'event="(?<v>[a-z_]+)"')) {
                Assert-True -Condition ($script:FreezeEvents -contains $m.Groups['v'].Value) -Because "$($rule.Name) 里的 event=$($m.Groups['v'].Value) 要是 kFreezeHoldEventNames 的取值"
            }
        }
    }
    $stuck = Get-RuleExpr -Body (@(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group 'battle.settlement-rooms'))[0]).Body
    Assert-Match -Text $stuck -Pattern 'max by \(namespace, instance\) \(battle_sql_outcome_stuck_rooms\) > 0' -Because '按台报:instance 就是要排空的那台 GameServer'
    foreach ($rule in (Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group 'scene.battle-freeze'))) {
        $expr = Get-RuleExpr -Body $rule.Body
        $event = $script:SceneRuleEvents[$rule.Name].Event
        $series = [regex]::Escape(('scene_battle_freeze_events_total{{event="{0}"}}' -f $event))
        Assert-Match -Text $expr -Pattern ('increase\({0}\[10m\]\)' -f $series) -Because "$($rule.Name) 读 $event 的 10 分钟增量(与原 LogQL 的 10 分钟窗口相同)"
        Assert-Match -Text $expr -Pattern ('(?s)\bor\b.*{0}\s+unless {0} offset 10m' -f $series) -Because "$($rule.Name) 带新序列一支:首次被抓取之前就计了数时 increase 看不到"
        Assert-Match -Text $rule.Body -Pattern '(?m)^          for:[ ]*0m[ ]*$' -Because "$($rule.Name) 健康时恒 0,出现就报"
        Assert-Match -Text $rule.Body -Pattern ('metric={0}' -f [regex]::Escape($script:SceneRuleEvents[$rule.Name].Log)) -Because "$($rule.Name) 的 description 写出按局排查用的日志名"
    }
}

Test-Case 'asset-channel-alerts.yaml 文件头不再保留已改成本文件规则的四条 LogQL(ruler 接线后会重复告警)' {
    foreach ($log in @('battle_personal_settlement_staging_exhausted_rooms', 'battle_freeze_hold_cap_reached', 'battle_item_hold_overdue', 'jianghu_duel_fee_overdue')) {
        Assert-NotMatch -Text $script:AssetAlerts -Pattern ('\|= "metric={0}"' -f [regex]::Escape($log)) -Because "metric=$log 已由 battle-settlement-alerts.yaml 的 PromQL 规则覆盖"
    }
    Assert-Match -Text $script:AssetAlerts -Pattern 'battle-settlement-alerts\.yaml' -Because '文件头写明这几条去了哪里'
}

Test-Case '本文件的每条告警的名字在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($groupName in $script:ExpectedGroups.Keys) {
        foreach ($name in @($script:ExpectedGroups[$groupName].Keys)) {
            Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
        }
    }
}

exit (Complete-TestRun -SuiteName 'battle_settlement_alerts_contract')
