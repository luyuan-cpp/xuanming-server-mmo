#requires -Version 7
<#
.SYNOPSIS
    审计发件箱告警的契约测试(docs/design/player-audit-outbox.md §5.6、§9;断线重连 R13)。

.DESCRIPTION
    deploy/k8s/audit-outbox-alerts.yaml 的规则只在序列存在时才可能响:写错指标名、label、阈值或把按副本的规则写成聚合,
    都不会报错,只会让告警永远不响或乱响。这里逐项对照代码里的权威来源:
      - go/db 的指标名(go/db/internal/metrics/audit_outbox.go,前缀 db 来自 metrics.go 的 subsystem);
      - 转发循环的活性接线与阈值依据(go/db/internal/auditoutbox/relay.go、sql.go,go/db/internal/config/audit_outbox.go);
      - go/db 跳过原因的取值(go/db/internal/playerstore/audit_outbox.go 的 AuditSkip*),告警描述要逐个讲到;
      - scene 侧丢弃计数(cpp/libs/modules/audit/audit_outbox_stats.h 的 DropReason 与 kDropReasonNames)、导出它的采集回调
        (cpp/libs/services/scene/player/system/player_save_metrics.cpp)与登记(cpp/nodes/scene/main.cpp),每种原因都真的有地方计;
      - PrometheusRule 形状、规则名单、severity / for / summary / description / runbook_url,告警名在 deploy/k8s 的告警文件里不重名。
    只读文本,不执行部署入口,不访问集群。规则语法由 promtool 另验(见 docs/PROGRESS.md 对应条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/audit_outbox_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

# 告警文件里某一组的文本:从 `    - name: <组名>` 到下一个组、下一段四格缩进的组注释或文件尾。
function Get-AlertGroupText {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Group)
    $m = [regex]::Match($Text, ('(?ms)^    - name:[ ]*{0}[ ]*\n(?<body>.*?)(?=^    - name:|^    # |\z)' -f [regex]::Escape($Group)))
    if (-not $m.Success) { throw "找不到告警组 $Group" }
    return $m.Groups['body'].Value
}

# 一组里的每条规则:名字与规则体(到下一条规则、下一段八格缩进的规则注释或组尾为止)。
function Get-AlertRules {
    param([Parameter(Mandatory = $true)][string]$GroupText)
    return @([regex]::Matches($GroupText, '(?ms)^        - alert:[ ]*(?<name>\S+)[ ]*\n(?<body>.*?)(?=^        - alert:|^        # |\z)') |
            ForEach-Object { [pscustomobject]@{ Name = $_.Groups['name'].Value; Body = $_.Groups['body'].Value } })
}

function Get-Rule {
    param([Parameter(Mandatory = $true)][object[]]$Rules, [Parameter(Mandatory = $true)][string]$Name)
    $rule = @($Rules | Where-Object { $_.Name -ceq $Name })
    if ($rule.Count -ne 1) { throw "找不到规则 $Name(或重复)" }
    return $rule[0]
}

function Get-RuleExpr {
    param([Parameter(Mandatory = $true)][string]$Body)
    $m = [regex]::Match($Body, '(?ms)^          expr: \|\n(?<e>.*?)(?=^          for:)')
    if (-not $m.Success) { throw '规则里找不到 `expr: |` 段(或它后面不是 for:)' }
    return $m.Groups['e'].Value.Trim()
}

# 从源码里取 `<名字> = <n> * time.Second` 的 n(秒)。取不到即 throw:写法变了要同步本用例,而不是让断言在空值上通过。
function Get-GoSeconds {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Name)
    $m = [regex]::Match($Text, ('\b{0}\s*=\s*(?<n>\d+)\s*\*\s*time\.Second\b' -f [regex]::Escape($Name)))
    if (-not $m.Success) { throw "源码里找不到 $Name = <n> * time.Second" }
    return [int]$m.Groups['n'].Value
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/audit-outbox-alerts.yaml'
$script:GoMetrics = Read-RepoText -RelativePath 'go/db/internal/metrics/audit_outbox.go'
$script:GoMetricsMain = Read-RepoText -RelativePath 'go/db/internal/metrics/metrics.go'
$script:Relay = Read-RepoText -RelativePath 'go/db/internal/auditoutbox/relay.go'
$script:RelaySql = Read-RepoText -RelativePath 'go/db/internal/auditoutbox/sql.go'
$script:RelayConfig = Read-RepoText -RelativePath 'go/db/internal/config/audit_outbox.go'
$script:StoreOutbox = Read-RepoText -RelativePath 'go/db/internal/playerstore/audit_outbox.go'
$script:Stats = Read-RepoText -RelativePath 'cpp/libs/modules/audit/audit_outbox_stats.h'
$script:Collector = Read-RepoText -RelativePath 'cpp/libs/services/scene/player/system/player_save_metrics.cpp'
$script:SceneMain = Read-RepoText -RelativePath 'cpp/nodes/scene/main.cpp'
$script:StoreClient = Read-RepoText -RelativePath 'cpp/libs/services/scene/player/system/player_store_client.cpp'

# 组名 → 规则名 → @(severity, for)。增删规则要同步这里与设计稿 §9。
$script:ExpectedGroups = [ordered]@{
    'db.audit-outbox'          = [ordered]@{
        'AuditOutboxForwardingLagging' = @('warning', '5m')
        'AuditOutboxForwardingStalled' = @('critical', '5m')
        'AuditOutboxRecordsSkipped'    = @('warning', '0m')
        'AuditOutboxRelayDisabled'     = @('critical', '10m')
        'AuditOutboxUnitsUnowned'      = @('warning', '5m')
    }
    'db.audit-outbox-liveness' = [ordered]@{
        'AuditOutboxRelayLoopStalled' = @('warning', '5m')
    }
    'scene.audit-outbox'       = [ordered]@{
        'SceneAuditRecordsDropped' = @('warning', '0m')
        'SceneAuditStoreTooOld'    = @('warning', '0m')
    }
}

function Get-GroupRules {
    param([Parameter(Mandatory = $true)][string]$Group)
    return @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:Alerts -Group $Group))
}

# scene 侧丢弃原因:枚举项(不含 kCount)与名字表,两者按下标一一对应(C++ 的 static_assert 守着条数)。
$script:DropReasonItems = @(
    [regex]::Matches([regex]::Match($script:Stats, '(?s)enum class DropReason : uint8_t\s*\{(?<b>.*?)\};').Groups['b'].Value, '(?m)^\s*(?<k>k[A-Za-z]+),') |
        ForEach-Object { $_.Groups['k'].Value } | Where-Object { $_ -cne 'kCount' })
$script:DropReasonNames = @(
    [regex]::Matches([regex]::Match($script:Stats, '(?s)kDropReasonNames\[\] = \{(?<b>.*?)\};').Groups['b'].Value, '"(?<n>[a-z_]+)"') |
        ForEach-Object { $_.Groups['n'].Value })

Write-Host ''
Write-Host '=== audit_outbox_alerts_contract ==='

Test-Case 'PrometheusRule 形状;三个组的规则名单、severity、for、summary、description、runbook_url' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 player-store-alerts.yaml 同一安装方式)'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^  name:[ ]*audit-outbox-alerts[ ]*$' -Because 'metadata.name'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    $groups = @([regex]::Matches($script:Alerts, '(?m)^    - name:[ ]*(\S+)[ ]*$') | ForEach-Object { $_.Groups[1].Value })
    Assert-Equal -Expected (@($script:ExpectedGroups.Keys) -join ',') -Actual ($groups -join ',') -Because '组名单与顺序钉死'
    foreach ($group in $script:ExpectedGroups.Keys) {
        Assert-Match -Text (Get-AlertGroupText -Text $script:Alerts -Group $group) -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because "$group 的评估间隔"
        $rules = Get-GroupRules -Group $group
        $expected = $script:ExpectedGroups[$group]
        Assert-Equal -Expected (@($expected.Keys) -join ',') -Actual (@($rules | ForEach-Object { $_.Name }) -join ',') -Because "$group 的规则名单钉死"
        foreach ($rule in $rules) {
            $severity, $for = $expected[$rule.Name]
            Assert-Match -Text $rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $severity) -Because "$($rule.Name) 的 severity 应为 $severity"
            Assert-Match -Text $rule.Body -Pattern ('(?m)^          for:[ ]*{0}[ ]*$' -f [regex]::Escape($for)) -Because "$($rule.Name) 的 for 应为 $for"
            foreach ($key in @('summary', 'description')) {
                Assert-Match -Text $rule.Body -Pattern ('(?m)^            {0}:' -f $key) -Because "$($rule.Name) 要有 $key"
            }
            Assert-Match -Text $rule.Body -Pattern '(?m)^            runbook_url:[ ]*docs/design/player-audit-outbox\.md[ ]*$' -Because "$($rule.Name) 指向审计发件箱设计稿"
        }
    }
}

Test-Case '文件里出现的每个 db_audit_outbox_* 都是 go/db 注册的指标(含描述里点名的)' {
    Assert-Match -Text $script:GoMetricsMain -Pattern '(?m)^const subsystem = "db"' -Because 'go/db 指标名前缀'
    $defined = @([regex]::Matches($script:GoMetrics, 'Subsystem:\s*subsystem,\s*Name:\s*"(?<n>audit_outbox_[a-z_]+)"') | ForEach-Object { 'db_' + $_.Groups['n'].Value })
    Assert-True -Condition ($defined.Count -ge 9) -Because "夹具前提:audit_outbox.go 里至少有 9 个指标定义(实际 $($defined.Count)),写法变了要同步本用例"
    $referenced = @([regex]::Matches($script:Alerts, '\bdb_audit_outbox_[a-z_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-True -Condition ($referenced.Count -gt 0) -Because '告警至少引用一个 db_audit_outbox_* 指标'
    foreach ($metric in $referenced) {
        Assert-True -Condition ($defined -ccontains $metric) -Because "$metric 必须由 go/db/internal/metrics/audit_outbox.go 定义(改名后告警静默失效、描述误导值班)"
    }
    # 注册:全部指标经 registerAuditOutbox 挂进默认注册表,活性 gauge 经 Collector()。
    Assert-Match -Text $script:GoMetrics -Pattern '(?s)prometheus\.MustRegister\(.*?auditOutboxRelayLive\.Collector\(\)\)' -Because '活性 gauge 要挂进注册表'
}

Test-Case '转发类规则:年龄按 kind 取最大、两级阈值;跳过按 increase 读;全停与覆盖按全集群聚合' {
    $rules = Get-GroupRules -Group 'db.audit-outbox'
    $lagging = Get-RuleExpr -Body (Get-Rule -Rules $rules -Name 'AuditOutboxForwardingLagging').Body
    $stalled = Get-RuleExpr -Body (Get-Rule -Rules $rules -Name 'AuditOutboxForwardingStalled').Body
    Assert-Equal -Expected 'max by (kind) (db_audit_outbox_oldest_pending_age_seconds) > 300' -Actual $lagging -Because 'warning:最老未投记录超过 5 分钟(设计 §9)'
    Assert-Equal -Expected 'max by (kind) (db_audit_outbox_oldest_pending_age_seconds) > 1800' -Actual $stalled -Because 'critical:超过 30 分钟(设计 §9)'
    $skipped = Get-RuleExpr -Body (Get-Rule -Rules $rules -Name 'AuditOutboxRecordsSkipped').Body
    Assert-Equal -Expected 'sum by (namespace, reason) (increase(db_audit_outbox_records_skipped_total[10m])) > 0' -Actual $skipped -Because '跳过健康时恒 0,出现就报;按 zone 与原因分'
    Assert-Equal -Expected 'max(db_audit_outbox_relay_enabled) == 0' -Actual (Get-RuleExpr -Body (Get-Rule -Rules $rules -Name 'AuditOutboxRelayDisabled').Body) `
        -Because '全部副本都没在转发才报:转发跨 zone 分工,不按 namespace 分;canary 恒 0 被 stable 的 1 盖住'
    Assert-Equal -Expected 'sum(db_audit_outbox_relay_owned_units) < max(db_audit_outbox_relay_units)' -Actual (Get-RuleExpr -Body (Get-Rule -Rules $rules -Name 'AuditOutboxUnitsUnowned').Body) `
        -Because '各副本负责的单位数之和少于单位总数 = 有单位没人投'
    # 年龄 gauge 的 kind 取值、跳过原因的取值由 Go 预置(没有序列时 increase 抓不到第一次)。
    Assert-Match -Text $script:GoMetrics -Pattern 'auditOutboxOldestAge\.WithLabelValues\(kind\)\.Set\(0\)' -Because '年龄 gauge 按 kind 预置为 0'
    Assert-Match -Text $script:GoMetrics -Pattern 'auditOutboxSkipped\.WithLabelValues\(reason\)\.Add\(0\)' -Because '跳过计数按原因预置为 0'
    # 跳过原因:描述要逐个讲到,值班才知道每一种去查什么。
    $reasons = @([regex]::Matches($script:StoreOutbox, '(?m)^\s*AuditSkip[A-Za-z]+\s*=\s*"(?<r>[a-z_]+)"') | ForEach-Object { $_.Groups['r'].Value })
    Assert-True -Condition ($reasons.Count -ge 4) -Because "夹具前提:playerstore 至少有 4 个跳过原因(实际 $($reasons.Count))"
    $skippedBody = (Get-Rule -Rules $rules -Name 'AuditOutboxRecordsSkipped').Body
    foreach ($reason in $reasons) {
        Assert-Match -Text $skippedBody -Pattern ('\b{0} =' -f [regex]::Escape($reason)) -Because "AuditOutboxRecordsSkipped 的描述要讲到跳过原因 $reason"
    }
}

Test-Case '活性规则:按副本逐条判 time() - gauge > 秒;阈值不小于两次写时刻最长间隔的 5 倍,与 Go 常量同源' {
    $rule = Get-Rule -Rules (Get-GroupRules -Group 'db.audit-outbox-liveness') -Name 'AuditOutboxRelayLoopStalled'
    $expr = Get-RuleExpr -Body $rule.Body
    $m = [regex]::Match($expr, '^time\(\) - db_audit_outbox_relay_last_run_unixtime > (?<s>\d+)$')
    Assert-True -Condition $m.Success -Because "式子只能是 time() - db_audit_outbox_relay_last_run_unixtime > 秒,不聚合(一个副本停住时别的副本照常推进,聚合会盖住它),实际: $expr"
    $threshold = [int]$m.Groups['s'].Value

    # 一轮最长 = passBudget + 在途的一批(读、删各一条语句,投一批)+ 这个单位的两次探测、取锁(各一条语句)与放锁;
    # 两轮之间最长等待 = max(退避上限, IntervalMs 上限)。
    $passBudget = Get-GoSeconds -Text $script:Relay -Name 'passBudget'
    $statement = Get-GoSeconds -Text $script:Relay -Name 'statementTimeout'
    $produce = Get-GoSeconds -Text $script:Relay -Name 'produceTimeout'
    $backoffMax = Get-GoSeconds -Text $script:Relay -Name 'backoffMax'
    $release = Get-GoSeconds -Text $script:RelaySql -Name 'releaseTimeout'
    $interval = [regex]::Match($script:RelayConfig, '\bMaxAuditOutboxIntervalMs\s*=\s*(?<ms>\d+)')
    Assert-True -Condition $interval.Success -Because '夹具前提:能从 go/db/internal/config/audit_outbox.go 读到 MaxAuditOutboxIntervalMs'
    $intervalMaxSec = [math]::Ceiling([int]$interval.Groups['ms'].Value / 1000)
    $passMax = $passBudget + 5 * $statement + $produce + $release
    $gap = $passMax + [math]::Max($backoffMax, $intervalMaxSec)
    Assert-True -Condition ($threshold -ge 5 * $gap) -Because "活性阈值 $threshold 秒应不小于 5 × 两次写时刻最长间隔($gap 秒 = 一轮最长 $passMax + 最长等待 $([math]::Max($backoffMax, $intervalMaxSec)))"
}

Test-Case '活性接线:looplive 定义与注册;Run 起来时 Mark、每一轮经 Round、Stop 后紧跟 return、不 defer;覆盖 gauge 每轮写' {
    Assert-Match -Text $script:GoMetrics -Pattern 'auditOutboxRelayLive = looplive\.New\(prometheus\.GaugeOpts\{\s*Subsystem:\s*subsystem,\s*Name:\s*"audit_outbox_relay_last_run_unixtime",' -Because '活性 gauge 用 looplive 定义,名字与告警一致'
    Assert-Match -Text $script:GoMetrics -Pattern 'func AuditOutboxRelayLive\(\) \*looplive\.Gauge' -Because '转发循环经这个函数拿活性 gauge'
    Assert-Match -Text $script:Relay -Pattern 'live := metrics\.AuditOutboxRelayLive\(\)' -Because 'Run 拿的是注册过的那个 gauge'
    Assert-Match -Text $script:Relay -Pattern 'live\.Mark\(time\.Now\(\)\)' -Because '循环起来时先写一次(首轮之前可能等一拍)'
    Assert-Match -Text $script:Relay -Pattern 'live\.Round\(relayPoint, func\(\) \{ result = r\.Pass\(ctx\) \}\)' -Because '每一轮都经 Round 跑:panic 兜住、正常返回才写时刻'
    Assert-Match -Text $script:Relay -Pattern 'live\.Stop\(\)\s*\n\s*return' -Because '有意停止时先删序列再 return'
    Assert-Equal -Expected ([regex]::Matches($script:Relay, 'live\.Stop\(\)').Count) -Actual ([regex]::Matches($script:Relay, 'live\.Stop\(\)\s*\n\s*return').Count) -Because '每一处 Stop 后面都紧跟 return'
    Assert-NotMatch -Text $script:Relay -Pattern 'defer\s+live\.(Mark|Stop)\(' -Because 'Mark / Stop 都不能 defer:defer 在 panic 展开时也执行,会记错时刻或删掉循环异常退出的证据'
    Assert-Match -Text $script:Relay -Pattern '\brelayPoint\s*=\s*"db\.audit_outbox_relay"' -Because 'safego 点位名(告警描述按它指路)'
    Assert-Match -Text $script:Relay -Pattern 'metrics\.SetAuditOutboxRelayUnits\(' -Because '覆盖 gauge(owned_units / units)每一轮写一次'
}

Test-Case 'scene 侧:丢弃原因的枚举与名字一一对应、每种原因都有地方计;两个计数由采集回调导出、采集回调由 scene main 登记' {
    Assert-True -Condition ($script:DropReasonItems.Count -ge 5) -Because "夹具前提:能读到 DropReason 的枚举项(实际 $($script:DropReasonItems.Count))"
    Assert-Equal -Expected $script:DropReasonItems.Count -Actual $script:DropReasonNames.Count -Because '枚举项与名字表条数相同(C++ static_assert 同样守着)'
    # 每种原因都要在某处真的计数,否则那一种丢弃永远看不见。
    $sources = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'cpp/libs/modules'), (Join-Path $script:RepoRoot 'cpp/libs/services/scene') -Recurse -File |
            Where-Object { $_.Extension -in @('.cpp', '.h') } | ForEach-Object { [IO.File]::ReadAllText($_.FullName) })
    $counted = @($sources | ForEach-Object { [regex]::Matches($_, 'IncDropped\((?:audit_outbox_stats::)?DropReason::(?<k>k[A-Za-z]+)\)') } | ForEach-Object { $_.Groups['k'].Value } | Sort-Object -Unique)
    foreach ($item in $script:DropReasonItems) {
        Assert-True -Condition ($counted -ccontains $item) -Because "DropReason::$item 没有任何地方调 IncDropped:这一种丢弃不会出现在指标里"
    }
    Assert-Match -Text $script:StoreClient -Pattern 'audit_outbox_stats::IncStoreTooOld\(\);' -Because 'go/db 没写发件箱时存盘状态机计数'
    # 采集回调:两个指标族、counter 类型、reason label 取自名字表。
    Assert-Match -Text $script:Collector -Pattern '(?s)BeginFamily\("scene_audit_outbox_records_dropped_total",.*?prometheus_text::MetricType::kCounter\);\s*for \(std::size_t i = 0; i < audit_outbox_stats::kDropReasonCount; \+\+i\)\s*\{\s*writer\.Sample\(\{\{"reason", audit_outbox_stats::kDropReasonNames\[i\]\}\},' `
        -Because '丢弃计数按原因逐个导出,每种原因启动即以 0 导出'
    Assert-Match -Text $script:Collector -Pattern '(?s)BeginFamily\("scene_audit_outbox_store_too_old_total",.*?prometheus_text::MetricType::kCounter\);\s*writer\.Sample\(\{\}, audit_outbox_stats::ReadStoreTooOld\(\)\);' `
        -Because 'store_too_old 计数导出'
    Assert-Match -Text $script:SceneMain -Pattern '(?m)^\s*RegisterPlayerSaveMetrics\(\);' -Because 'scene main 要登记存盘采集回调,否则指标不存在、告警永远不响'
    # 规则引用的 scene 指标都由采集回调导出;描述逐个讲到每种原因。
    $families = @([regex]::Matches($script:Collector, 'BeginFamily\("(?<n>scene_audit_outbox_[a-z_]+)"') | ForEach-Object { $_.Groups['n'].Value })
    foreach ($metric in @([regex]::Matches($script:Alerts, '\bscene_audit_outbox_[a-z_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)) {
        Assert-True -Condition ($families -ccontains $metric) -Because "$metric 必须由 player_save_metrics.cpp 导出"
    }
    $rules = Get-GroupRules -Group 'scene.audit-outbox'
    Assert-Equal -Expected 'sum by (namespace, reason) (increase(scene_audit_outbox_records_dropped_total[10m])) > 0' `
        -Actual (Get-RuleExpr -Body (Get-Rule -Rules $rules -Name 'SceneAuditRecordsDropped').Body) -Because '丢弃健康时恒 0,出现就报'
    Assert-Equal -Expected 'sum by (namespace) (increase(scene_audit_outbox_store_too_old_total[10m])) > 0' `
        -Actual (Get-RuleExpr -Body (Get-Rule -Rules $rules -Name 'SceneAuditStoreTooOld').Body) -Because '混版本出现就报'
    $droppedBody = (Get-Rule -Rules $rules -Name 'SceneAuditRecordsDropped').Body
    foreach ($name in $script:DropReasonNames) {
        Assert-Match -Text $droppedBody -Pattern ('\b{0} =' -f [regex]::Escape($name)) -Because "SceneAuditRecordsDropped 的描述要讲到原因 $name"
    }
}

Test-Case '本文件钉的每条告警的名字在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($group in $script:ExpectedGroups.Keys) {
        foreach ($name in @($script:ExpectedGroups[$group].Keys)) {
            Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
        }
    }
}

exit (Complete-TestRun -SuiteName 'audit_outbox_alerts_contract')
