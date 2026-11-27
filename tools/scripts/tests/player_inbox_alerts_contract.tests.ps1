#requires -Version 7
<#
.SYNOPSIS
    scene 侧收件箱告警的契约测试(2026-11-27,player-inbox-alerts.yaml 文件头的 9 条 LogQL 值班查询改 PromQL)。

.DESCRIPTION
    deploy/k8s/player-inbox-alerts.yaml 的 scene.player-inbox 组读 scene 节点导出的收件箱指标:
    cpp/libs/services/scene/player/system/player_inbox_system.cpp 的 PlayerInboxSystem::WriteMetrics,由同文件的
    PlayerInboxSystem::Initialize 登记(scene main 启动时调一次)。标签取值来自两份名表:player_inbox_rules.h 的
    kConsumerEventNames / kLeaveAckResultNames,player_inbox_runtime_comp.h 的 kPlayerInboxDeferReasonNames / kPlayerInboxHaltReasonNames。
    这里钉的都是「写错了不报错、只会永远不响」的东西:
      - 规则引用的指标名都由 C++ 导出,标签取值都在名表里,枚举与名表同序;采集回调在提前返回之前登记;
      - 每处 metric=inbox_* 日志都有去处(gauge 或计数),计数紧跟同名日志自增(口径就是原来 LogQL 数的日志行数);
      - 规则名单、级别、字段、形状(计数带新序列一支,逐玩家状态带 10 分钟 max_over_time),只按低基数 label 分组;
      - 告警名在 deploy/k8s 全部告警文件里不重名;文件头不再保留已转换的 LogQL(Loki ruler 接线之后会重复告警);
      - runbook 的告警处置表列着每条新规则。
    本文件名以收件箱服务的指标前缀开头,不能写进告警文件(go/player_inbox 的 TestAlertRulesReferenceRegisteredMetrics
    会把它当成指标名),只登记在 docs/ops/player-inbox-runbook.md §15。
    只读文本,不执行部署入口,不访问任何集群。规则语法由 promtool 另验(见 runbook §15)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/player_inbox_alerts_contract.tests.ps1
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

# C++ 名字表(`kXxxNames[] = { ... };`)里的取值,按声明顺序。
function Get-CppNameTable {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Name)
    $m = [regex]::Match($Text, ('(?s)' + [regex]::Escape($Name) + '\[\]\s*=\s*\{(?<body>[^}]*)\}'))
    if (-not $m.Success) { throw "找不到名表 $Name" }
    return @([regex]::Matches($m.Groups['body'].Value, '"(?<v>[a-z_]+)"') | ForEach-Object { $_.Groups['v'].Value })
}

# C++ 枚举(`enum class Xxx : uint8_t { kA, kB, ..., kCount }`)的枚举项,去掉 kCount,按声明顺序。
function Get-CppEnumItems {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Name)
    $m = [regex]::Match($Text, ('(?s)enum class ' + [regex]::Escape($Name) + '\s*:\s*uint8_t\s*\{(?<body>[^}]*)\}'))
    if (-not $m.Success) { throw "找不到枚举 $Name" }
    $body = [regex]::Replace($m.Groups['body'].Value, '//[^\n]*', '')
    return @([regex]::Matches($body, '\b(?<v>k[A-Z][A-Za-z0-9]*)\b') | ForEach-Object { $_.Groups['v'].Value } | Where-Object { $_ -cne 'kCount' })
}

# 名表取值 → 枚举项名:delivered_undecided → kDeliveredUndecided。
function ConvertTo-EnumItem {
    param([Parameter(Mandatory = $true)][string]$Value)
    return 'k' + ((@($Value -split '_') | ForEach-Object { $_.Substring(0, 1).ToUpperInvariant() + $_.Substring(1) }) -join '')
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/player-inbox-alerts.yaml'
$script:AlertsHeader = ($script:Alerts -split "(?m)^---[ ]*$", 2)[0]
$script:System = Read-RepoText -RelativePath 'cpp/libs/services/scene/player/system/player_inbox_system.cpp'
$script:Rules = Read-RepoText -RelativePath 'cpp/libs/services/scene/player/system/player_inbox_rules.h'
$script:RuntimeComp = Read-RepoText -RelativePath 'cpp/libs/services/scene/player/comp/player_inbox_runtime_comp.h'
$script:Loader = Read-RepoText -RelativePath 'cpp/libs/services/scene/player/system/player_database_loader.cpp'
$script:SceneMain = Read-RepoText -RelativePath 'cpp/nodes/scene/main.cpp'
$script:Runbook = Read-RepoText -RelativePath 'docs/ops/player-inbox-runbook.md'

$script:WriteMetricsBody = [regex]::Match($script:System, '(?s)void PlayerInboxSystem::WriteMetrics\(prometheus_text::Writer &writer\)\s*\{(?<body>.*?)\n\}').Groups['body'].Value
$script:Families = @([regex]::Matches($script:System, 'BeginFamily\("(?<n>scene_[a-z_]+)"') | ForEach-Object { $_.Groups['n'].Value } | Sort-Object -Unique)
$script:ConsumerEvents = Get-CppNameTable -Text $script:Rules -Name 'kConsumerEventNames'
$script:LeaveAckResults = Get-CppNameTable -Text $script:Rules -Name 'kLeaveAckResultNames'
# 两张原因表的下标 0 是 none(不是原因,不导出)。
$script:DeferReasons = @(Get-CppNameTable -Text $script:RuntimeComp -Name 'kPlayerInboxDeferReasonNames' | Select-Object -Skip 1)
$script:HaltReasons = @(Get-CppNameTable -Text $script:RuntimeComp -Name 'kPlayerInboxHaltReasonNames' | Select-Object -Skip 1)

# scene 侧每一种 metric=inbox_<键> 日志去了哪里(导出的指标族)。新加一种日志就要在这里决定它的去处。
$script:LogKeyExports = [ordered]@{
    'consumer_disabled'        = 'scene_player_inbox_consumer_enabled'
    'registry_broken'          = 'scene_player_inbox_registry_broken'
    'halted'                   = 'scene_player_inbox_halted_player_domains'
    'ledger_invalid'           = 'scene_player_inbox_ledger_invalid_players'
    'unknown_type'             = 'scene_player_inbox_events_total'
    'replay_mismatch'          = 'scene_player_inbox_events_total'
    'ack_anomaly'              = 'scene_player_inbox_events_total'
    'battle_duplicate_request' = 'scene_player_inbox_events_total'
    'stale_owner'              = 'scene_player_inbox_events_total'
    'deferred'                 = 'scene_player_inbox_deferral_starts_total'
    'leave_ack'                = 'scene_player_inbox_leave_ack_total'
}

# scene.player-inbox 组的规则:级别沿用原 LogQL;Log 是它取代的那条查询的日志键,description 要写出它(按人排查的入口)。
$script:SceneRules = [ordered]@{
    'ScenePlayerInboxReplayMismatch'   = @{ Severity = 'critical'; Log = 'replay_mismatch'; Event = 'replay_mismatch' }
    'ScenePlayerInboxUnknownType'      = @{ Severity = 'warning'; Log = 'unknown_type'; Event = 'unknown_type' }
    'ScenePlayerInboxAckAnomaly'       = @{ Severity = 'warning'; Log = 'ack_anomaly'; Event = 'ack_anomaly' }
    'ScenePlayerInboxDomainHalted'     = @{ Severity = 'warning'; Log = 'halted'; Expr = 'sum by (namespace, reason) (max_over_time(scene_player_inbox_halted_player_domains[10m])) > 0' }
    'ScenePlayerInboxLedgerInvalid'    = @{ Severity = 'warning'; Log = 'ledger_invalid'; Expr = 'sum by (namespace) (max_over_time(scene_player_inbox_ledger_invalid_players[10m])) > 0' }
    'ScenePlayerInboxRegistryBroken'   = @{ Severity = 'warning'; Log = 'registry_broken'; Expr = 'sum by (namespace) (scene_player_inbox_registry_broken) > 0' }
    'ScenePlayerInboxConsumerDisabled' = @{ Severity = 'warning'; Log = 'consumer_disabled'; Expr = 'count by (namespace) (scene_player_inbox_consumer_enabled == 0) > 0' }
}

Write-Host ''
Write-Host '=== player_inbox_alerts_contract ==='

Test-Case '夹具:C++ 导出的指标族与名表,枚举与名表同序;采集回调在 Initialize 提前返回之前登记' {
    Assert-Equal -Expected 'scene_player_inbox_consumer_enabled,scene_player_inbox_deferral_starts_total,scene_player_inbox_events_total,scene_player_inbox_halted_player_domains,scene_player_inbox_leave_ack_total,scene_player_inbox_ledger_invalid_players,scene_player_inbox_registry_broken' `
        -Actual ($script:Families -join ',') -Because 'player_inbox_system.cpp 导出的指标族;增删指标要同步告警与本用例'
    Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($script:WriteMetricsBody)) -Because '前提:找得到 PlayerInboxSystem::WriteMetrics 的函数体'
    Assert-Equal -Expected 'unknown_type,replay_mismatch,ack_anomaly,battle_duplicate_request,stale_owner' -Actual ($script:ConsumerEvents -join ',') `
        -Because 'kConsumerEventNames 的取值与顺序(告警按名字读)'
    Assert-Equal -Expected 'sent,not_sent,not_durable,fetch_inflight,ack_inflight,delivered_undecided,decided_prefix_queued' -Actual ($script:LeaveAckResults -join ',') `
        -Because 'kLeaveAckResultNames 的取值与顺序'
    foreach ($table in @(
            @{ Enum = 'ConsumerEvent'; Names = $script:ConsumerEvents; Table = 'kConsumerEventNames' },
            @{ Enum = 'LeaveAckResult'; Names = $script:LeaveAckResults; Table = 'kLeaveAckResultNames' })) {
        $expected = @($table.Names | ForEach-Object { ConvertTo-EnumItem -Value $_ }) -join ','
        Assert-Equal -Expected $expected -Actual ((Get-CppEnumItems -Text $script:Rules -Name $table.Enum) -join ',') `
            -Because "$($table.Enum) 的枚举项与 $($table.Table) 同序(static_assert 只管长度,错位会把计数记到别的名字上)"
        Assert-Match -Text $script:Rules -Pattern ('static_assert\(std::size\({0}\) == static_cast<std::size_t>\({1}::kCount\)' -f $table.Table, $table.Enum) -Because "$($table.Table) 与枚举同长"
    }
    Assert-True -Condition ($script:DeferReasons.Count -gt 0 -and $script:HaltReasons.Count -gt 0) -Because '前提:读得到两张原因表'
    Assert-Match -Text $script:System -Pattern '(?s)void PlayerInboxSystem::Initialize\(\)\s*\{\s*(?://[^\n]*\n\s*)*if \(!GlobalMetricsRegistry\(\)\.Register\("scene_player_inbox", &PlayerInboxSystem::WriteMetrics\)\)' `
        -Because 'Initialize 第一件事是登记采集回调:开关关着时它会提前返回,而开关状态本身就是要导出的信号'
    Assert-Match -Text $script:SceneMain -Pattern '(?m)^\s*PlayerInboxSystem::Initialize\(\);' -Because 'scene main 调 Initialize,采集回调才会登记(指标不存在时告警永远不响)'
}

Test-Case 'WriteMetrics:标签取自名表,原因表跳过下标 0 的 none;gauge 读的是与日志同一份状态' {
    $body = $script:WriteMetricsBody
    Assert-Match -Text $body -Pattern '\{\{"event", player_inbox_rules::kConsumerEventNames\[i\]\}\}, tlsConsumerEventCounts\[i\]' -Because 'event 取自 kConsumerEventNames,值是同下标的计数'
    Assert-Match -Text $body -Pattern '\{\{"result", player_inbox_rules::kLeaveAckResultNames\[i\]\}\}, tlsLeaveAckCounts\[i\]' -Because 'result 取自 kLeaveAckResultNames'
    Assert-Match -Text $body -Pattern '(?s)for \(std::size_t i = 1; i < tlsDeferralStartCounts\.size\(\); \+\+i\)\s*\{\s*writer\.Sample\(\{\{"reason", kPlayerInboxDeferReasonNames\[i\]\}\}' -Because 'deferral 的 reason 取自 kPlayerInboxDeferReasonNames,从 1 起(none 不导出)'
    Assert-Match -Text $body -Pattern '(?s)for \(std::size_t i = 1; i < halted\.size\(\); \+\+i\)\s*\{\s*writer\.Sample\(\{\{"reason", kPlayerInboxHaltReasonNames\[i\]\}\}' -Because '停用的 reason 取自 kPlayerInboxHaltReasonNames,从 1 起'
    Assert-Match -Text $body -Pattern 'writer\.Sample\(\{\}, ConfiguredConsumerEnabled\(\) \? 1U : 0U\);' -Because '开关读 ConfiguredConsumerEnabled(与 metric=inbox_consumer_disabled 同一个判据,不含注册表自检)'
    Assert-Match -Text $script:System -Pattern '(?s)if \(ConfiguredConsumerEnabled\(\)\)\s*\{\s*tlsNextConsumerDisabledLogMs = 0;\s*\}\s*else if \(TakeNodeRelogTurn\(tlsNextConsumerDisabledLogMs, nowMs\)\)\s*\{\s*LogConsumerDisabled\(\);' `
        -Because '前提:日志也只按 ConfiguredConsumerEnabled 判'
    Assert-Match -Text $body -Pattern 'writer\.Sample\(\{\}, tlsRegistryBroken \? 1U : 0U\);' -Because '注册表自检读 tlsRegistryBroken(与 metric=inbox_registry_broken 同一个标志)'
    Assert-Match -Text $body -Pattern '(?s)view<PlayerInboxRuntimeComp>\(\)\.each\(\).*?if \(state\.halted\)\s*\{\s*IncrementCount\(halted, state\.haltReason\);' -Because '停用按运行时组件里每个域的 halted / haltReason 现数'
    Assert-Match -Text $body -Pattern 'view<PlayerInboxLedgerInvalidComp>\(\)' -Because '账本无效数加载时挂的损坏标签'
    Assert-Match -Text $body -Pattern 'view<PlayerInboxRuntimeComp>\(entt::exclude<PlayerInboxLedgerComp, PlayerInboxLedgerInvalidComp>\)' -Because '另数已落地却缺账本组件的(与前置闸同口径),有标签的不重复数'
    Assert-Match -Text $script:System -Pattern '(?s)if \(tlsEcs\.actorRegistry\.any_of<PlayerInboxLedgerInvalidComp>\(player\) \|\|\s*!tlsEcs\.actorRegistry\.all_of<PlayerInboxLedgerComp>\(player\)\)\s*\{\s*return PlayerInboxDeferReason::kLedgerInvalid;' `
        -Because '前提:前置闸判账本无效的正是这两种情形'
    Assert-Match -Text $script:Loader -Pattern '(?s)"\[PlayerInbox\] metric=inbox_ledger_invalid source=loader player_id=".{0,300}?emplace_or_replace<PlayerInboxLedgerInvalidComp>\(player\);' `
        -Because '加载时打 source=loader 的同一分支挂损坏标签,gauge 才看得到'
}

Test-Case '计数口径:每种 metric=inbox_* 日志都有去处;计数紧跟同名日志自增,每处日志一次' {
    $keys = @(@($script:System, $script:Loader) | ForEach-Object { [regex]::Matches($_, '"\[PlayerInbox\] metric=inbox_(?<k>[a-z_]+)') } |
            ForEach-Object { $_.Groups['k'].Value } | Sort-Object -Unique)
    Assert-Equal -Expected ((@($script:LogKeyExports.Keys) | Sort-Object) -join ',') -Actual ($keys -join ',') `
        -Because 'scene 的 metric=inbox_* 日志键与本用例的去处表一一对应;新加一种日志要决定导出成什么(并补告警或看板)'
    foreach ($key in $script:LogKeyExports.Keys) {
        Assert-True -Condition ($script:Families -ccontains $script:LogKeyExports[$key]) -Because "metric=inbox_$key 的去处 $($script:LogKeyExports[$key]) 要由 C++ 导出"
    }

    # 一次性事件:同名日志的每一处后面(下一条 metric= 日志之前)紧跟一次对应枚举的计数。
    foreach ($event in $script:ConsumerEvents) {
        $enum = ConvertTo-EnumItem -Value $event
        $logLiteral = '"[PlayerInbox] metric=inbox_{0} ' -f $event
        $countLiteral = 'IncrementCount(tlsConsumerEventCounts, ConsumerEvent::{0});' -f $enum
        $logs = Get-LiteralCount -Text $script:System -Literal $logLiteral
        Assert-True -Condition ($logs -gt 0) -Because "前提:player_inbox_system.cpp 里有 metric=inbox_$event 的日志"
        Assert-Equal -Expected $logs -Actual (Get-LiteralCount -Text $script:System -Literal $countLiteral) -Because "metric=inbox_$event 每打一处计一次 $enum"
        $paired = [regex]::Matches($script:System, ('(?s)' + [regex]::Escape($logLiteral) + '(?:(?!"\[PlayerInbox\] metric=).){0,800}?' + [regex]::Escape($countLiteral))).Count
        Assert-Equal -Expected $logs -Actual $paired -Because "每处 metric=inbox_$event 之后、下一条 metric= 日志之前都有它的 $enum 计数"
    }

    # 延后:原因变化时打的那一行(同一原因连续延后只打 DEBUG、不计)。
    $deferLiteral = '"[PlayerInbox] metric=inbox_deferred reason="'
    Assert-Equal -Expected 1 -Actual (Get-LiteralCount -Text $script:System -Literal $deferLiteral) -Because 'metric=inbox_deferred 只在 ScheduleRetry 原因变化时打一处'
    Assert-Match -Text $script:System -Pattern ('(?s)if \(reasonChanged\)\s*\{\s*LOG_INFO << ' + [regex]::Escape($deferLiteral) + '[^;]*;\s*IncrementCount\(tlsDeferralStartCounts, reason\);\s*\}') `
        -Because '延后计数与 metric=inbox_deferred 在同一个 reasonChanged 分支里'

    # 离开补确认:日志里写死的 result 取值(含 sent / not_sent 的三元式)正好是名表,每处紧跟同一取值的计数。
    $literalResults = @([regex]::Matches($script:System, '"\[PlayerInbox\] metric=inbox_leave_ack result=(?<v>[a-z_]+) ') | ForEach-Object { $_.Groups['v'].Value })
    $ternary = '"[PlayerInbox] metric=inbox_leave_ack result=" << (sent ? "sent" : "not_sent")'
    Assert-Equal -Expected 1 -Actual (Get-LiteralCount -Text $script:System -Literal $ternary) -Because '补发结局 sent / not_sent 那一行'
    Assert-Equal -Expected (($script:LeaveAckResults | Sort-Object) -join ',') -Actual ((@($literalResults + @('sent', 'not_sent')) | Sort-Object) -join ',') `
        -Because 'metric=inbox_leave_ack 的 result 取值与 kLeaveAckResultNames 一一对应,每个取值只有一处日志'
    foreach ($result in $literalResults) {
        $logLiteral = '"[PlayerInbox] metric=inbox_leave_ack result={0} ' -f $result
        $countLiteral = 'IncrementCount(tlsLeaveAckCounts, LeaveAckResult::{0});' -f (ConvertTo-EnumItem -Value $result)
        Assert-Match -Text $script:System -Pattern ('(?s)' + [regex]::Escape($logLiteral) + '(?:(?!"\[PlayerInbox\] metric=).){0,800}?' + [regex]::Escape($countLiteral)) `
            -Because "result=$result 的日志之后紧跟同一取值的计数"
    }
    Assert-Match -Text $script:System -Pattern ('(?s)' + [regex]::Escape($ternary) + '(?:(?!"\[PlayerInbox\] metric=).){0,400}?IncrementCount\(tlsLeaveAckCounts, sent \? LeaveAckResult::kSent : LeaveAckResult::kNotSent\);') `
        -Because 'sent / not_sent 按同一个 sent 判据计数'
}

Test-Case 'player-inbox-alerts.yaml:scene.player-inbox 组的规则名单、级别与字段' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    $group = Get-AlertGroupText -Text $script:Alerts -Group 'scene.player-inbox'
    Assert-Match -Text $group -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because 'scene 组的评估间隔'
    $rules = @(Get-AlertRules -GroupText $group)
    Assert-Equal -Expected (@($script:SceneRules.Keys) -join ',') -Actual ((@($rules | ForEach-Object { $_.Name })) -join ',') -Because 'scene.player-inbox 的规则名单钉死'
    foreach ($rule in $rules) {
        $spec = $script:SceneRules[$rule.Name]
        Assert-Match -Text $rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $spec.Severity) -Because "$($rule.Name) 的级别沿用原 LogQL($($spec.Severity))"
        Assert-Match -Text $rule.Body -Pattern '(?m)^          for:[ ]*0m[ ]*$' -Because "$($rule.Name) 与原查询一样出现就报"
        foreach ($key in @('summary', 'description', 'runbook_url')) {
            Assert-Match -Text $rule.Body -Pattern ('(?m)^            {0}:' -f $key) -Because "$($rule.Name) 要有 $key"
        }
        $runbook = [regex]::Match($rule.Body, '(?m)^            runbook_url:[ ]*(\S+)[ ]*$').Groups[1].Value
        Assert-True -Condition (Test-Path -LiteralPath (Join-Path $script:RepoRoot $runbook)) -Because "$($rule.Name) 的 runbook_url 指向仓库里存在的文档($runbook)"
        Assert-Match -Text $rule.Body -Pattern ('metric=inbox_{0}\b' -f $spec.Log) -Because "$($rule.Name) 的 description 写出按人排查用的日志键 metric=inbox_$($spec.Log)"
    }
}

Test-Case '式子:指标都由 C++ 导出、标签取值在名表里、只按低基数 label 分组;计数带新序列一支,逐玩家状态带 10 分钟窗口' {
    $labelValues = @{ 'event' = $script:ConsumerEvents; 'result' = $script:LeaveAckResults; 'reason' = @($script:HaltReasons + $script:DeferReasons) }
    $group = Get-AlertGroupText -Text $script:Alerts -Group 'scene.player-inbox'
    foreach ($rule in (Get-AlertRules -GroupText $group)) {
        $spec = $script:SceneRules[$rule.Name]
        $expr = Get-RuleExpr -Body $rule.Body
        $referenced = @([regex]::Matches($expr, '\bscene_[a-z_]+\b') | ForEach-Object { $_.Value } | Sort-Object -Unique)
        Assert-True -Condition ($referenced.Count -gt 0) -Because "$($rule.Name) 至少引用一个 scene 指标"
        foreach ($metric in $referenced) {
            Assert-True -Condition ($script:Families -ccontains $metric) -Because "$($rule.Name):$metric 必须由 WriteMetrics 导出(改名后告警静默失效)"
        }
        foreach ($by in [regex]::Matches($expr, '\bby \((?<labels>[^)]*)\)')) {
            foreach ($label in @($by.Groups['labels'].Value -split ',' | ForEach-Object { $_.Trim() })) {
                Assert-True -Condition (@('namespace', 'instance', 'reason') -ccontains $label) -Because "$($rule.Name) 不按 $label 分组(只许 namespace / instance 与封闭名表 reason)"
            }
        }
        Assert-NotMatch -Text $expr -Pattern '(?i)player_id|session_id|battle_id' -Because "$($rule.Name) 不碰高基数标签"
        foreach ($m in [regex]::Matches($expr, '\b(?<label>event|result|reason)="(?<v>[a-z_]+)"')) {
            Assert-True -Condition ($labelValues[$m.Groups['label'].Value] -ccontains $m.Groups['v'].Value) -Because "$($rule.Name) 里的 $($m.Value) 要是名表里的取值"
        }
        if ($spec.ContainsKey('Event')) {
            $series = [regex]::Escape(('scene_player_inbox_events_total{{event="{0}"}}' -f $spec.Event))
            Assert-Match -Text $expr -Pattern ('sum by \(namespace\) \(increase\({0}\[10m\]\)\) > 0' -f $series) -Because "$($rule.Name) 读 $($spec.Event) 的 10 分钟增量(与原 LogQL 的 10 分钟窗口相同)"
            Assert-Match -Text $expr -Pattern ('(?s)\bor\b\s*sum by \(namespace\) \(\s*{0}\s+unless {0} offset 10m\s*\) > 0' -f $series) -Because "$($rule.Name) 带新序列一支:首次被抓取之前就计了数时 increase 看不到"
        }
        else {
            Assert-Equal -Expected $spec.Expr -Actual $expr.Trim() -Because "$($rule.Name) 的式子(状态类 gauge;逐玩家状态带 max_over_time 10 分钟,保留原查询的窗口口径)"
        }
    }
}

Test-Case '文件头:已转换的 9 条 LogQL 不再保留,info 查询改写成 PromQL 看板查询,写明每条去了哪里' {
    $converted = @('unknown_type', 'halted', 'replay_mismatch', 'ack_anomaly', 'ledger_invalid', 'registry_broken', 'consumer_disabled', 'deferred', 'leave_ack')
    foreach ($key in $converted) {
        Assert-NotMatch -Text $script:Alerts -Pattern ('\|= "metric=inbox_{0}"' -f $key) -Because "metric=inbox_$key 已改成 PromQL,LogQL 留着的话 ruler 接线之后会重复告警"
    }
    $selectors = [regex]::Matches($script:Alerts, '\{(?<sel>[^{}]*)\}\s*\|[=~]').Count
    Assert-Equal -Expected 0 -Actual $selectors -Because '文件里不再有 LogQL 选择器(要加回值班查询时,同步改本断言与 k8s_deploy_contract「LogQL 契约」的 MinSelectors)'
    Assert-NotMatch -Text $script:AlertsHeader -Pattern 'scene 侧没有 Prometheus 端点' -Because 'scene 有 Prometheus 出口(MMORPG_METRICS_PORT),旧说法已不成立'
    foreach ($rule in $script:SceneRules.Keys) {
        $log = $script:SceneRules[$rule].Log
        Assert-Match -Text $script:AlertsHeader -Pattern ('metric=inbox_{0}\s+→\s+{1}\b' -f $log, $rule) -Because "文件头写明 metric=inbox_$log 改成了 $rule"
    }
    Assert-Match -Text $script:AlertsHeader -Pattern ([regex]::Escape('sum by (namespace, reason) (increase(scene_player_inbox_deferral_starts_total[10m]))')) -Because '延后原因分布的看板查询'
    $leave = [regex]::Match($script:AlertsHeader, 'sum by \(namespace, result\) \(increase\(scene_player_inbox_leave_ack_total\{result=~"(?<v>[a-z_|]+)"\}\[1h\]\)\)')
    Assert-True -Condition $leave.Success -Because '离开补确认残余的看板查询(1 小时窗口,与原查询相同)'
    $residues = @($leave.Groups['v'].Value -split '\|')
    Assert-Equal -Expected 'ack_inflight,delivered_undecided,fetch_inflight,not_durable,not_sent' -Actual (($residues | Sort-Object) -join ',') -Because '残余只算这五种,sent 与 decided_prefix_queued 不算'
    foreach ($value in $residues) {
        Assert-True -Condition ($script:LeaveAckResults -ccontains $value) -Because "看板查询里的 result=$value 要是 kLeaveAckResultNames 的取值"
    }
    Assert-NotMatch -Text $script:Alerts -Pattern 'player_inbox_alerts_contract' -Because '本用例的文件名不能写进告警文件:Go 侧守卫会把它当成收件箱服务的指标名'
}

Test-Case '告警名在 deploy/k8s 的全部告警文件里各只出现一次;runbook 的处置表列着每条 scene 规则' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in @($script:SceneRules.Keys)) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
        Assert-Match -Text $script:Runbook -Pattern ('\|\s*`{0}`\s*\|' -f $name) -Because "docs/ops/player-inbox-runbook.md 的告警处置表要有 $name 一行"
    }
    Assert-Match -Text $script:Runbook -Pattern 'player_inbox_alerts_contract\.tests\.ps1' -Because 'runbook §15 登记本用例(告警文件里不能写它的名字)'
}

exit (Complete-TestRun -SuiteName 'player_inbox_alerts_contract')
