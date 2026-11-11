#requires -Version 7
<#
.SYNOPSIS
    按片分工的周期循环的活性告警契约(docs/design/assetop-redelivery-sharding-20261110.md §14.3、§14.8,缺口清单第 25 条)。

.DESCRIPTION
    六个循环各有一个「最近一次跑完一轮的时刻」gauge:资产指令重投(guild / trade / mail / data_service 回收共用 assetop)、
    GM 回收派发、mail 过期清理、guild / trade 终态清理、match 终局发件箱。口径都一样:循环起来时写一次,每跑完一轮写一次
    (这一轮成败都写,panic 的那一轮不写),循环退出时删掉序列;告警按副本逐条判 time() - gauge > 阈值,不聚合。
    写错指标名、把规则写成 max() 聚合、在开头用 defer 写时刻、阈值小于一轮最长耗时,都不会报错,只会让告警永远不响或乱响。
    这里逐项对照代码里的权威来源。只读文本,不执行部署入口,不访问集群;规则语法由 promtool 另验。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/loop_liveness_alerts_contract.tests.ps1
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
    return $m.Groups['e'].Value
}

# 一条活性规则的式子里每一项 time() - <指标> > <秒>:回 @{ 指标名 = 秒 }。
function Get-StallTerms {
    param([Parameter(Mandatory = $true)][string]$Expr)
    $terms = [ordered]@{}
    foreach ($m in [regex]::Matches($Expr, 'time\(\) - (?<metric>[a-z0-9_]+) > (?<seconds>\d+)')) {
        $terms[$m.Groups['metric'].Value] = [int]$m.Groups['seconds'].Value
    }
    return $terms
}

# 共同的规则形状:severity warning、for 5m、有 summary / description;式子只由 time() - gauge > 秒(可用 or 并列)组成,不聚合。
function Assert-StallRuleShape {
    param([Parameter(Mandatory = $true)][object]$Rule, [Parameter(Mandatory = $true)][string[]]$Metrics)
    $body = $Rule.Body
    Assert-Match -Text $body -Pattern '(?m)^            severity:[ ]*warning[ ]*$' -Because "$($Rule.Name):片没人做只是延迟,不丢,取 warning"
    Assert-Match -Text $body -Pattern '(?m)^          for:[ ]*5m[ ]*$' -Because "$($Rule.Name):for 5m 防一次陈旧抓取"
    foreach ($key in @('summary', 'description')) {
        Assert-Match -Text $body -Pattern ('(?m)^            {0}:' -f $key) -Because "$($Rule.Name) 要有 $key"
    }
    $expr = Get-RuleExpr -Body $body
    Assert-NotMatch -Text $expr -Pattern '\b(sum|max|min|avg|count|topk|bottomk|quantile)\b' -Because "$($Rule.Name) 要按副本逐条判:分片之后一个副本卡住,别的副本照常推进,聚合会把它盖住"
    $rest = [regex]::Replace($expr, 'time\(\) - [a-z0-9_]+ > \d+', '')
    Assert-Match -Text $rest -Pattern '^[\s()]*(or[\s()]*)*$' -Because "$($Rule.Name) 的式子只能是 time() - gauge > 秒(可用 or 并列),实际: $expr"
    $terms = Get-StallTerms -Expr $expr
    Assert-Equal -Expected ($Metrics -join ',') -Actual (@($terms.Keys) -join ',') -Because "$($Rule.Name) 判的是哪些活性 gauge"
    return $terms
}

# Go 源里一个 GaugeVec 定义:不带可变 label(`}, nil)` 结尾)。$Pattern 从变量名一直写到 Name 那一行。
function Assert-NoLabelGaugeVec {
    param([Parameter(Mandatory = $true)][string]$Source, [Parameter(Mandatory = $true)][string]$Pattern, [Parameter(Mandatory = $true)][string]$Because)
    Assert-Match -Text $Source -Pattern ('(?s){0}[^}}]*\}}, nil\)' -f $Pattern) -Because $Because
}

$script:AssetAlerts = Read-RepoText -RelativePath 'deploy/k8s/asset-channel-alerts.yaml'
$script:MailAlerts = Read-RepoText -RelativePath 'deploy/k8s/mail-alerts.yaml'
$script:MatchAlerts = Read-RepoText -RelativePath 'deploy/k8s/match-outcome-outbox-alerts.yaml'
$script:LivenessRules = @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:AssetAlerts -Group 'asset-channel.loop-liveness'))
$script:MailRules = @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:MailAlerts -Group 'mail.sweep'))
$script:MatchRules = @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:MatchAlerts -Group 'match.outcome-outbox-liveness'))

Write-Host ''
Write-Host '=== loop_liveness_alerts_contract ==='

Test-Case '规则名单:asset-channel.loop-liveness 三条、mail 一条、match 一条;形状一致(warning、for 5m、按副本判)' {
    Assert-Equal -Expected 'AssetOpRedeliveryLoopStalled,RecallDispatchLoopStalled,AssetOpCleanupLoopStalled' -Actual (($script:LivenessRules | ForEach-Object { $_.Name }) -join ',') -Because 'asset-channel.loop-liveness 组的规则名单钉死'
    Assert-Match -Text (Get-AlertGroupText -Text $script:AssetAlerts -Group 'asset-channel.loop-liveness') -Pattern '(?m)^      interval:[ ]*60s[ ]*$' -Because '与本文件其余组同一评估间隔'
    Assert-Equal -Expected 'MatchOutcomeOutboxLoopStalled' -Actual (($script:MatchRules | ForEach-Object { $_.Name }) -join ',') -Because 'match.outcome-outbox-liveness 组只有一条'
    $null = Assert-StallRuleShape -Rule (Get-Rule -Rules $script:LivenessRules -Name 'AssetOpRedeliveryLoopStalled') -Metrics @('assetop_loop_last_run_unixtime')
    $null = Assert-StallRuleShape -Rule (Get-Rule -Rules $script:LivenessRules -Name 'RecallDispatchLoopStalled') -Metrics @('data_service_recall_dispatch_last_run_unixtime')
    $null = Assert-StallRuleShape -Rule (Get-Rule -Rules $script:LivenessRules -Name 'AssetOpCleanupLoopStalled') -Metrics @('guild_asset_cleanup_last_run_unixtime', 'trade_assetop_cleanup_last_run_unixtime')
    $null = Assert-StallRuleShape -Rule (Get-Rule -Rules $script:MailRules -Name 'MailSweepStalled') -Metrics @('mail_sweep_last_run_unixtime')
    $null = Assert-StallRuleShape -Rule (Get-Rule -Rules $script:MatchRules -Name 'MatchOutcomeOutboxLoopStalled') -Metrics @('match_outcome_outbox_last_run_unixtime')
}

Test-Case '六个活性 gauge 都是不带可变 label 的 GaugeVec,退出时 Reset 删序列;名字与告警里的一致' {
    $assetopMetrics = Read-RepoText -RelativePath 'go/shared/assetop/metrics.go'
    Assert-NoLabelGaugeVec -Source $assetopMetrics -Pattern 'loopLastRun: f\.NewGaugeVec\(prometheus\.GaugeOpts\{\s*Name:\s*"assetop_loop_last_run_unixtime",' -Because 'assetop_loop_last_run_unixtime 只带 service 常量 label'
    Assert-Match -Text $assetopMetrics -Pattern '(?s)func \(m \*Metrics\) clearLoopRun\(\) \{.*?m\.loopLastRun\.Reset\(\)' -Because '重投循环退出时删序列'

    $dsMetrics = Read-RepoText -RelativePath 'go/data_service/internal/metrics/metrics.go'
    Assert-Match -Text $dsMetrics -Pattern '(?m)^const subsystem = "data_service"' -Because 'data_service 指标名前缀'
    $dsRecall = Read-RepoText -RelativePath 'go/data_service/internal/metrics/recall.go'
    Assert-NoLabelGaugeVec -Source $dsRecall -Pattern 'recallDispatchLastRun = promauto\.NewGaugeVec\(prometheus\.GaugeOpts\{\s*Subsystem:\s*subsystem,\s*Name:\s*"recall_dispatch_last_run_unixtime",' -Because 'data_service_recall_dispatch_last_run_unixtime 不带可变 label'
    Assert-Match -Text $dsRecall -Pattern 'recallDispatchLastRun\.Reset\(\)' -Because '派发循环退出时删序列'

    $mailMetrics = Read-RepoText -RelativePath 'go/mail/internal/metrics/metrics.go'
    Assert-Match -Text $mailMetrics -Pattern '(?m)^const subsystem = "mail"' -Because 'mail 指标名前缀'
    Assert-NoLabelGaugeVec -Source $mailMetrics -Pattern 'sweepLastRunUnixtime = prometheus\.NewGaugeVec\(prometheus\.GaugeOpts\{\s*Subsystem:\s*subsystem,\s*Name:\s*"sweep_last_run_unixtime",' -Because 'mail_sweep_last_run_unixtime 不带可变 label,也不预建(canary 不跑清理,不能报 0)'
    Assert-Match -Text $mailMetrics -Pattern 'sweepLastRunUnixtime\.Reset\(\)' -Because '清理循环退出时删序列'

    $guildStore = Read-RepoText -RelativePath 'go/guild/internal/data/asset_store.go'
    Assert-Match -Text (Read-RepoText -RelativePath 'go/guild/internal/data/guild_manage_repo.go') -Pattern 'const guildMetricNamespace = "guild"' -Because 'guild 指标名前缀'
    Assert-NoLabelGaugeVec -Source $guildStore -Pattern 'guildAssetCleanupLastRun = promauto\.NewGaugeVec\(prometheus\.GaugeOpts\{\s*Namespace:\s*guildMetricNamespace,\s*Subsystem:\s*"asset",\s*Name:\s*"cleanup_last_run_unixtime",' -Because 'guild_asset_cleanup_last_run_unixtime 不带可变 label(用 Prometheus 客户端:go-zero 的 metric 删不了序列)'
    Assert-Match -Text $guildStore -Pattern 'guildAssetCleanupLastRun\.Reset\(\)' -Because 'guild 清理循环退出时删序列'

    $tradeRepo = Read-RepoText -RelativePath 'go/trade/internal/data/asset_op_repo.go'
    Assert-NoLabelGaugeVec -Source $tradeRepo -Pattern 'cleanupLastRun = promauto\.With\(prometheus\.DefaultRegisterer\)\.NewGaugeVec\(prometheus\.GaugeOpts\{\s*Subsystem:\s*"trade",\s*Name:\s*"assetop_cleanup_last_run_unixtime",' -Because 'trade_assetop_cleanup_last_run_unixtime 不带可变 label'
    Assert-Match -Text $tradeRepo -Pattern 'cleanupLastRun\.Reset\(\)' -Because 'trade 清理循环退出时删序列'

    $matchOutcome = Read-RepoText -RelativePath 'go/match/internal/metrics/outcome.go'
    Assert-NoLabelGaugeVec -Source $matchOutcome -Pattern 'outcomeOutboxLastRun = prometheus\.NewGaugeVec\(prometheus\.GaugeOpts\{\s*Subsystem:\s*subsystem,\s*Name:\s*"outcome_outbox_last_run_unixtime",' -Because 'match_outcome_outbox_last_run_unixtime 不带可变 label'
    Assert-Match -Text $matchOutcome -Pattern 'outcomeOutboxLastRun\.Reset\(\)' -Because '发件箱循环退出时删序列'
    Assert-Match -Text $matchOutcome -Pattern '(?s)func outcomeCollectors\(\) \[\]prometheus\.Collector \{.*?outcomeOutboxLastRun.*?\}' -Because '发件箱活性 gauge 要挂进 register()'
}

Test-Case '每个循环起来时写、退出时删、每轮跑完在正常走完的地方写(不用 defer:defer 在开头就求值 time.Now(),panic 时也照样执行)' {
    $loops = @(
        @{ Path = 'go/shared/assetop/reconcile.go'; Start = 'l\.metrics\.markLoopRun\(l\.now\(\)\)\s*\n\s*defer l\.metrics\.clearLoopRun\(\)'; Round = 'l\.Tick\(ctx\)\s*\n\s*l\.metrics\.markLoopRun\(l\.now\(\)\)'; Mark = 'markLoopRun' }
        @{ Path = 'go/data_service/internal/svc/recall_channel.go'; Start = 'metrics\.MarkRecallDispatchRun\(time\.Now\(\)\)\s*\n\s*defer metrics\.ClearRecallDispatchRun\(\)'; Round = 'r\.dispatchRound\(ctx\)\s*\n\s*metrics\.MarkRecallDispatchRun\(time\.Now\(\)\)'; Mark = 'MarkRecallDispatchRun' }
        @{ Path = 'go/mail/internal/maintenance/sweep.go'; Start = 'metrics\.MarkSweepRun\(time\.Now\(\)\)\s*\n\s*defer metrics\.ClearSweepRun\(\)'; Round = 'err := sweepOwned\([^\n]*\)\s*\n\s*metrics\.MarkSweepRun\(time\.Now\(\)\)'; Mark = 'MarkSweepRun' }
        @{ Path = 'go/guild/internal/data/asset_store.go'; Start = 'MarkCleanupRun\(time\.Now\(\)\)\s*\n\s*defer ClearCleanupRun\(\)'; Round = 'logx\.Errorf\("\[GuildAsset\] Cleanup failed: %v", err\)\s*\n\s*\}\s*\n\s*MarkCleanupRun\(time\.Now\(\)\)'; Mark = 'MarkCleanupRun' }
        @{ Path = 'go/guild/guild.go'; Start = 'data\.MarkCleanupRun\(time\.Now\(\)\)\s*\n\s*defer data\.ClearCleanupRun\(\)'; Round = 'logx\.Errorf\("guild cleanup failed: %v", err\)\s*\n\s*\}\s*\n[^\n]*\n\s*data\.MarkCleanupRun\(time\.Now\(\)\)'; Mark = 'MarkCleanupRun' }
        @{ Path = 'go/trade/internal/data/asset_op_repo.go'; Start = 'MarkCleanupRun\(time\.Now\(\)\)\s*\n\s*defer ClearCleanupRun\(\)'; Round = 'Terminal asset op cleanup deleted[^\n]*\n\s*\}\s*\n\s*MarkCleanupRun\(time\.Now\(\)\)'; Mark = 'MarkCleanupRun' }
        @{ Path = 'go/trade/internal/svc/assetchannel.go'; Start = 'data\.MarkCleanupRun\(time\.Now\(\)\)\s*\n\s*defer data\.ClearCleanupRun\(\)'; Round = 'logx\.Errorf\("trade cleanup failed: %v", err\)\s*\n\s*\}\s*\n\s*data\.MarkCleanupRun\(time\.Now\(\)\)'; Mark = 'MarkCleanupRun' }
        @{ Path = 'go/match/internal/svc/servicecontext.go'; Start = 'metrics\.MarkOutcomeOutboxRun\(time\.Now\(\)\)\s*\n\s*defer metrics\.ClearOutcomeOutboxRun\(\)'; Round = 'cancel\(\)\s*\n\s*metrics\.MarkOutcomeOutboxRun\(time\.Now\(\)\)'; Mark = 'MarkOutcomeOutboxRun' }
    )
    foreach ($loop in $loops) {
        $text = Read-RepoText -RelativePath $loop.Path
        Assert-Match -Text $text -Pattern $loop.Start -Because "$($loop.Path):循环起来时先写一次(首轮之前可能有随机延迟),退出时删序列"
        Assert-Match -Text $text -Pattern $loop.Round -Because "$($loop.Path):每跑完一轮,在这一轮正常走完的地方写时刻"
        Assert-NotMatch -Text $text -Pattern ('defer\s+[A-Za-z0-9_.]*{0}\(' -f $loop.Mark) -Because "$($loop.Path):不能 defer 写时刻(会记成开始时刻,panic 时也会写)"
    }
}

Test-Case '阈值大于一轮最长耗时的若干倍,与代码常量、服务配置同源' {
    # 重投:Tick 等全部 worker 跑完才返回,一轮最长约 Interval + ceil(Batch / Workers) × OpBudget(DefaultLoopConfig)。
    $reconcile = Read-RepoText -RelativePath 'go/shared/assetop/reconcile.go'
    $defaults = [regex]::Match($reconcile, '(?ms)^func DefaultLoopConfig\(\) LoopConfig \{(?<b>.*?)^\}').Groups['b'].Value
    $interval = [regex]::Match($defaults, 'Interval:\s+(?<n>\d+) \* time\.Second,')
    $batch = [regex]::Match($defaults, 'Batch:\s+(?<n>\d+),')
    $workers = [regex]::Match($defaults, 'Workers:\s+(?<n>\d+),')
    $budget = [regex]::Match($defaults, 'OpBudget:\s+(?<n>\d+) \* time\.Millisecond,')
    Assert-True -Condition ($interval.Success -and $batch.Success -and $workers.Success -and $budget.Success) -Because '夹具前提:能从 DefaultLoopConfig 读到 Interval / Batch / Workers / OpBudget(换了写法要同步本用例)'
    $roundSec = [double]$interval.Groups['n'].Value + [math]::Ceiling([double]$batch.Groups['n'].Value / [double]$workers.Groups['n'].Value) * [double]$budget.Groups['n'].Value / 1000
    $terms = Get-StallTerms -Expr (Get-RuleExpr -Body (Get-Rule -Rules $script:LivenessRules -Name 'AssetOpRedeliveryLoopStalled').Body)
    Assert-True -Condition ($terms['assetop_loop_last_run_unixtime'] -ge 5 * $roundSec) -Because "重投阈值 $($terms['assetop_loop_last_run_unixtime']) 秒应不小于 5 × 默认最长一轮($roundSec 秒)"

    # 派发:间隔 + 一轮总预算(recallDispatchRoundLimit)。
    $recall = Read-RepoText -RelativePath 'go/data_service/internal/svc/recall_channel.go'
    $dispatchInterval = [regex]::Match($recall, 'recallDispatchInterval\s*=\s*(?:(?<n>\d+)\s*\*\s*)?time\.Second')
    $roundLimit = [regex]::Match($recall, 'recallDispatchRoundLimit\s*=\s*(?<n>\d+)\s*\*\s*time\.Second')
    Assert-True -Condition ($dispatchInterval.Success -and $roundLimit.Success) -Because '夹具前提:能读到 recallDispatchInterval 与 recallDispatchRoundLimit'
    $dispatchIntervalSec = if ($dispatchInterval.Groups['n'].Success) { [int]$dispatchInterval.Groups['n'].Value } else { 1 }
    $dispatchRoundSec = $dispatchIntervalSec + [int]$roundLimit.Groups['n'].Value
    $terms = Get-StallTerms -Expr (Get-RuleExpr -Body (Get-Rule -Rules $script:LivenessRules -Name 'RecallDispatchLoopStalled').Body)
    Assert-True -Condition ($terms['data_service_recall_dispatch_last_run_unixtime'] -ge 5 * $dispatchRoundSec) -Because "派发阈值应不小于 5 × (间隔 + 一轮总预算) = $(5 * $dispatchRoundSec) 秒"

    # 终态清理:阈值 = 3 × CleanupIntervalMinutes。服务 yaml 写了就以它为准,没写取代码缺省;部署脚本不许另行覆盖。
    $cleanupSeconds = foreach ($svc in @('guild', 'trade')) {
        $default = [regex]::Match((Read-RepoText -RelativePath "go/$svc/internal/config/config.go"), 'CleanupIntervalMinutes\s+int\s+`json:",default=(?<n>\d+)"`')
        Assert-True -Condition $default.Success -Because "夹具前提:能从 go/$svc/internal/config/config.go 读到 CleanupIntervalMinutes 的缺省值"
        $minutes = [int]$default.Groups['n'].Value
        $override = [regex]::Match((Read-RepoText -RelativePath "go/$svc/etc/$svc.yaml"), '(?m)^\s+CleanupIntervalMinutes:\s*(?<n>\d+)\s*$')
        if ($override.Success) { $minutes = [int]$override.Groups['n'].Value }
        3 * 60 * $minutes
    }
    Assert-NotMatch -Text (Read-RepoText -RelativePath 'tools/scripts/k8s_deploy.ps1') -Pattern 'CleanupIntervalMinutes' -Because '部署脚本不另设清理间隔:阈值只跟服务 yaml / 代码缺省联动'
    $terms = Get-StallTerms -Expr (Get-RuleExpr -Body (Get-Rule -Rules $script:LivenessRules -Name 'AssetOpCleanupLoopStalled').Body)
    Assert-Equal -Expected $cleanupSeconds[0] -Actual $terms['guild_asset_cleanup_last_run_unixtime'] -Because 'guild 清理阈值 = 3 × CleanupIntervalMinutes(秒)'
    Assert-Equal -Expected $cleanupSeconds[1] -Actual $terms['trade_assetop_cleanup_last_run_unixtime'] -Because 'trade 清理阈值 = 3 × CleanupIntervalMinutes(秒)'

    # match 发件箱:一轮总预算(servicecontext.go 的 WithTimeout)+ 缺省间隔(GlobalMatch.OutcomeOutbox.IntervalMs)。
    $matchSvc = Read-RepoText -RelativePath 'go/match/internal/svc/servicecontext.go'
    $matchBudget = [regex]::Match($matchSvc, 'context\.WithTimeout\(context\.WithoutCancel\(pubctx\), (?<s>\d+)\*time\.Second\)')
    $matchInterval = [regex]::Match((Read-RepoText -RelativePath 'go/match/internal/config/config.go'), '\bIntervalMs\s+int\s+`json:",default=(?<ms>\d+)"`')
    Assert-True -Condition ($matchBudget.Success -and $matchInterval.Success) -Because '夹具前提:能读到发件箱一轮的总预算与缺省间隔'
    $matchRoundSec = [int]$matchBudget.Groups['s'].Value + [int]$matchInterval.Groups['ms'].Value / 1000
    $terms = Get-StallTerms -Expr (Get-RuleExpr -Body (Get-Rule -Rules $script:MatchRules -Name 'MatchOutcomeOutboxLoopStalled').Body)
    Assert-True -Condition ($terms['match_outcome_outbox_last_run_unixtime'] -ge 5 * $matchRoundSec) -Because "发件箱阈值应不小于 5 × (一轮总预算 + 缺省间隔) = $(5 * $matchRoundSec) 秒"
}

Test-Case '活性告警名在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in @('AssetOpRedeliveryLoopStalled', 'RecallDispatchLoopStalled', 'AssetOpCleanupLoopStalled', 'MailSweepStalled', 'MatchOutcomeOutboxLoopStalled')) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'loop_liveness_alerts_contract')
