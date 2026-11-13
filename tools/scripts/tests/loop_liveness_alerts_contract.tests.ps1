#requires -Version 7
<#
.SYNOPSIS
    按片分工的周期循环的活性告警契约(docs/design/assetop-redelivery-sharding-20261110.md §14.3、§14.8、§14.10,缺口清单第 25 条)。

.DESCRIPTION
    每个循环各有一个「最近一次跑完一轮的时刻」gauge:资产指令重投(guild / trade / mail / data_service 回收共用 assetop)、
    GM 回收派发、mail 过期清理、guild / trade 终态清理、match 终局发件箱,以及 match 经 StartSourceLoop / StartRelayLoop
    起的循环与投榜循环(一族 match_loop_last_run_unixtime,point 区分循环)。口径只有一份,在 go/shared/looplive:
    循环起来时 Mark 一次;每一轮经 Round 跑 —— 正常返回(成败都算)才写时刻,panic 被 safego 兜住、不写、循环接着跑;
    循环有意停止时 Stop 删掉序列。告警按副本逐条判 time() - gauge > 阈值,不聚合。兜住的 panic 由 go-background-alerts.yaml
    的通用规则对全部 Go 服务的后台点位报。
    写错指标名或点位名、把规则写成 max() 聚合、用 defer 写时刻或删序列、阈值小于一轮最长耗时,都不会报错,只会让告警永远不响
    或乱响。这里逐项对照代码里的权威来源。只读文本,不执行部署入口,不访问集群;规则语法由 promtool 另验。

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

# 一条停摆规则的式子里每一项 time() - <指标> > <秒>:回 @{ 指标名 = 秒 }。
function Get-StallTerms {
    param([Parameter(Mandatory = $true)][string]$Expr)
    $terms = [ordered]@{}
    foreach ($m in [regex]::Matches($Expr, 'time\(\) - (?<metric>[a-z0-9_]+) > (?<seconds>\d+)')) {
        $terms[$m.Groups['metric'].Value] = [int]$m.Groups['seconds'].Value
    }
    return $terms
}

# 停摆规则共同的形状:severity warning、for 5m、有 summary / description;式子只由 time() - gauge > 秒(可用 or 并列)组成,不聚合。
function Assert-StallRuleShape {
    param([Parameter(Mandatory = $true)][object]$Rule, [Parameter(Mandatory = $true)][string[]]$Metrics)
    $body = $Rule.Body
    Assert-Match -Text $body -Pattern '(?m)^            severity:[ ]*warning[ ]*$' -Because "$($Rule.Name):片没人做只是延迟,不丢,取 warning"
    Assert-Match -Text $body -Pattern '(?m)^          for:[ ]*5m[ ]*$' -Because "$($Rule.Name):for 5m 防一次陈旧抓取"
    foreach ($key in @('summary', 'description')) {
        Assert-Match -Text $body -Pattern ('(?m)^            {0}:' -f $key) -Because "$($Rule.Name) 要有 $key"
    }
    $expr = Get-RuleExpr -Body $body
    Assert-NotMatch -Text $expr -Pattern '\b(sum|max|min|avg|count|topk|bottomk|quantile)\b' -Because "$($Rule.Name) 要按副本逐条判:分片之后一个副本停住,别的副本照常推进,聚合会把它盖住"
    $rest = [regex]::Replace($expr, 'time\(\) - [a-z0-9_]+ > \d+', '')
    Assert-Match -Text $rest -Pattern '^[\s()]*(or[\s()]*)*$' -Because "$($Rule.Name) 的式子只能是 time() - gauge > 秒(可用 or 并列),实际: $expr"
    $terms = Get-StallTerms -Expr $expr
    Assert-Equal -Expected ($Metrics -join ',') -Actual (@($terms.Keys) -join ',') -Because "$($Rule.Name) 判的是哪些活性 gauge"
}

$script:AssetAlerts = Read-RepoText -RelativePath 'deploy/k8s/asset-channel-alerts.yaml'
$script:MailAlerts = Read-RepoText -RelativePath 'deploy/k8s/mail-alerts.yaml'
$script:MatchAlerts = Read-RepoText -RelativePath 'deploy/k8s/match-outcome-outbox-alerts.yaml'
$script:BackgroundAlerts = Read-RepoText -RelativePath 'deploy/k8s/go-background-alerts.yaml'
$script:LivenessRules = @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:AssetAlerts -Group 'asset-channel.loop-liveness'))
$script:MailRules = @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:MailAlerts -Group 'mail.sweep'))
$script:MatchRules = @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:MatchAlerts -Group 'match.outcome-outbox-liveness'))
$script:BackgroundRules = @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:BackgroundAlerts -Group 'go.background-panic'))
$script:MatchPoolAlerts = Read-RepoText -RelativePath 'deploy/k8s/match-alerts.yaml'
$script:MatchLoopRules = @(Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:MatchPoolAlerts -Group 'match.loop-liveness'))

# 各个活性 gauge:在哪个文件里怎么定义(到 Name 那一行为止的正则)、指标全名、循环在哪个文件、gauge 在循环里怎么称呼、
# 每一轮的 safego 点位名(源码里的字面量所在文件与常量写法)。match 的一族里点位很多,点位名单在节拍用例里另行钉死。
$script:Loops = @(
    @{ Metric = 'assetop_loop_last_run_unixtime'
       Define = @{ Path = 'go/shared/assetop/metrics.go'; Pattern = 'loopLive: looplive\.NewRegistered\(reg, prometheus\.GaugeOpts\{\s*Name:\s*"assetop_loop_last_run_unixtime",' }
       Sites = @(@{ Path = 'go/shared/assetop/reconcile.go'; Live = 'live'; Round = 'live\.Round\("assetop\.reconcile_tick", func\(\) \{ l\.Tick\(ctx\) \}\)' })
       Point = @{ Name = 'assetop.reconcile_tick'; Path = 'go/shared/assetop/reconcile.go'; Pattern = 'live\.Round\("assetop\.reconcile_tick",' } }
    @{ Metric = 'data_service_recall_dispatch_last_run_unixtime'
       Define = @{ Path = 'go/data_service/internal/metrics/recall.go'; Pattern = 'recallDispatchLive = looplive\.NewRegistered\(prometheus\.DefaultRegisterer, prometheus\.GaugeOpts\{\s*Subsystem:\s*subsystem,\s*Name:\s*"recall_dispatch_last_run_unixtime",' }
       Sites = @(@{ Path = 'go/data_service/internal/svc/recall_channel.go'; Live = 'metrics\.RecallDispatchLive\(\)'; Round = 'metrics\.RecallDispatchLive\(\)\.Round\(recallDispatchPoint, func\(\) \{ r\.dispatchRound\(ctx\) \}\)' })
       Point = @{ Name = 'data_service.recall.dispatch'; Path = 'go/data_service/internal/svc/recall_channel.go'; Pattern = 'recallDispatchPoint\s*=\s*"data_service\.recall\.dispatch"' } }
    @{ Metric = 'mail_sweep_last_run_unixtime'
       Define = @{ Path = 'go/mail/internal/metrics/metrics.go'; Pattern = 'SweepLive = looplive\.New\(prometheus\.GaugeOpts\{\s*Subsystem:\s*subsystem,\s*Name:\s*"sweep_last_run_unixtime",' }
       Sites = @(@{ Path = 'go/mail/internal/maintenance/sweep.go'; Live = 'metrics\.SweepLive'; Round = 'metrics\.SweepLive\.Round\(sweepPoint, func\(\) \{ sweepRound\(ctx, repo, c, owns, root\) \}\)' })
       Point = @{ Name = 'mail.expired_sweep'; Path = 'go/mail/internal/maintenance/sweep.go'; Pattern = 'const sweepPoint = "mail\.expired_sweep"' } }
    @{ Metric = 'guild_asset_cleanup_last_run_unixtime'
       Define = @{ Path = 'go/guild/internal/data/asset_store.go'; Pattern = 'CleanupLive = looplive\.NewRegistered\(prometheus\.DefaultRegisterer, prometheus\.GaugeOpts\{\s*Namespace:\s*guildMetricNamespace,\s*Subsystem:\s*"asset",\s*Name:\s*"cleanup_last_run_unixtime",' }
       Sites = @(
           @{ Path = 'go/guild/internal/data/asset_store.go'; Live = 'CleanupLive'; Round = 'CleanupLive\.Round\(CleanupPoint, func\(\) \{' }
           @{ Path = 'go/guild/guild.go'; Live = 'data\.CleanupLive'; Round = 'data\.CleanupLive\.Round\(data\.CleanupPoint, func\(\) \{' })
       Point = @{ Name = 'guild.asset_op_cleanup'; Path = 'go/guild/internal/data/asset_store.go'; Pattern = 'const CleanupPoint = "guild\.asset_op_cleanup"' } }
    @{ Metric = 'trade_assetop_cleanup_last_run_unixtime'
       Define = @{ Path = 'go/trade/internal/data/asset_op_repo.go'; Pattern = 'CleanupLive = looplive\.NewRegistered\(prometheus\.DefaultRegisterer, prometheus\.GaugeOpts\{\s*Subsystem:\s*"trade",\s*Name:\s*"assetop_cleanup_last_run_unixtime",' }
       Sites = @(
           @{ Path = 'go/trade/internal/data/asset_op_repo.go'; Live = 'CleanupLive'; Round = 'CleanupLive\.Round\(cleanupPoint, func\(\) \{' }
           @{ Path = 'go/trade/internal/svc/assetchannel.go'; Live = 'data\.CleanupLive'; Round = 'data\.CleanupLive\.Round\(assetOpCleanupPoint, func\(\) \{ sourcedCleanupRound\(ctx, owner, clean\) \}\)' })
       Point = @{ Name = 'trade.asset_op_cleanup'; Path = 'go/trade/internal/svc/assetchannel.go'; Pattern = 'assetOpCleanupPoint\s*=\s*"trade\.asset_op_cleanup"' } }
    @{ Metric = 'match_outcome_outbox_last_run_unixtime'
       Define = @{ Path = 'go/match/internal/metrics/outcome.go'; Pattern = 'OutcomeOutboxLive = looplive\.New\(prometheus\.GaugeOpts\{\s*Subsystem:\s*subsystem,\s*Name:\s*"outcome_outbox_last_run_unixtime",' }
       Sites = @(@{ Path = 'go/match/internal/svc/outcome_outbox_loop.go'; Live = 'live'; Round = 'live\.Round\(outcomeOutboxPoint, func\(\) \{' })
       Point = @{ Name = 'match.outcome_outbox'; Path = 'go/match/internal/svc/outcome_outbox_loop.go'; Pattern = 'outcomeOutboxPoint\s*=\s*"match\.outcome_outbox"' } }
    @{ Metric = 'match_loop_last_run_unixtime'
       Define = @{ Path = 'go/match/internal/metrics/loops.go'; Pattern = 'LoopLive = looplive\.NewFamily\(prometheus\.GaugeOpts\{\s*Subsystem:\s*subsystem,\s*Name:\s*"loop_last_run_unixtime",' }
       Sites = @(
           @{ Path = 'go/match/internal/svc/servicecontext.go'; Live = 'live'; Round = 'if !live\.Round\(point, func\(\) \{ defer finish\(\); round\(context\.WithoutCancel\(ctx\)\) \}\) \{\s*sc\.RootAdmission\.Hold\(\)' }
           @{ Path = 'go/match/internal/svc/servicecontext.go'; Live = 'live'; Round = 'if !live\.Round\(point, func\(\) \{ round\(context\.WithoutCancel\(ctx\)\) \}\) \{\s*sc\.HoldRelay\(drainproof\.ErrHold\)' }
           @{ Path = 'go/match/internal/authority/rank_publisher.go'; Live = 'live'; Round = 'live\.Round\(rankPublishPoint, func\(\) \{' })
       Point = @{ Name = 'match.rank_publish'; Path = 'go/match/internal/authority/rank_publisher.go'; Pattern = 'const rankPublishPoint = "match\.rank_publish"' } }
)

Write-Host ''
Write-Host '=== loop_liveness_alerts_contract ==='

Test-Case '规则名单:asset-channel.loop-liveness 三条、mail 一条、match 发件箱一条、match 后台循环一条;停摆规则形状一致(warning、for 5m、按副本判)' {
    Assert-Equal -Expected 'AssetOpRedeliveryLoopStalled,RecallDispatchLoopStalled,AssetOpCleanupLoopStalled' -Actual (($script:LivenessRules | ForEach-Object { $_.Name }) -join ',') -Because 'asset-channel.loop-liveness 组的规则名单钉死'
    Assert-Match -Text (Get-AlertGroupText -Text $script:AssetAlerts -Group 'asset-channel.loop-liveness') -Pattern '(?m)^      interval:[ ]*60s[ ]*$' -Because '与本文件其余组同一评估间隔'
    Assert-Equal -Expected 'MatchOutcomeOutboxLoopStalled' -Actual (($script:MatchRules | ForEach-Object { $_.Name }) -join ',') -Because 'match.outcome-outbox-liveness 组只有一条'
    Assert-Equal -Expected 'MatchBackgroundLoopStalled' -Actual (($script:MatchLoopRules | ForEach-Object { $_.Name }) -join ',') -Because 'match.loop-liveness 组只有一条:一族 gauge 一条规则,按点位逐条判'
    Assert-Match -Text (Get-AlertGroupText -Text $script:MatchPoolAlerts -Group 'match.loop-liveness') -Pattern '(?m)^      interval:[ ]*60s[ ]*$' -Because '与其余活性组同一评估间隔'
    Assert-StallRuleShape -Rule (Get-Rule -Rules $script:MatchLoopRules -Name 'MatchBackgroundLoopStalled') -Metrics @('match_loop_last_run_unixtime')
    Assert-StallRuleShape -Rule (Get-Rule -Rules $script:LivenessRules -Name 'AssetOpRedeliveryLoopStalled') -Metrics @('assetop_loop_last_run_unixtime')
    Assert-StallRuleShape -Rule (Get-Rule -Rules $script:LivenessRules -Name 'RecallDispatchLoopStalled') -Metrics @('data_service_recall_dispatch_last_run_unixtime')
    Assert-StallRuleShape -Rule (Get-Rule -Rules $script:LivenessRules -Name 'AssetOpCleanupLoopStalled') -Metrics @('guild_asset_cleanup_last_run_unixtime', 'trade_assetop_cleanup_last_run_unixtime')
    Assert-StallRuleShape -Rule (Get-Rule -Rules $script:MailRules -Name 'MailSweepStalled') -Metrics @('mail_sweep_last_run_unixtime')
    Assert-StallRuleShape -Rule (Get-Rule -Rules $script:MatchRules -Name 'MatchOutcomeOutboxLoopStalled') -Metrics @('match_outcome_outbox_last_run_unixtime')
    # 停摆规则判的 gauge 合起来正好是上表里的那些,一个不多一个不少。
    $alerted = @($script:LivenessRules + $script:MailRules + $script:MatchRules + $script:MatchLoopRules | ForEach-Object { (Get-StallTerms -Expr (Get-RuleExpr -Body $_.Body)).Keys } | Sort-Object -Unique)
    Assert-Equal -Expected (@($script:Loops | ForEach-Object { $_.Metric } | Sort-Object) -join ',') -Actual ($alerted -join ',') -Because '每个活性 gauge 各有一条停摆规则'
}

Test-Case '活性口径只有一份(go/shared/looplive):GaugeVec 不预建序列;Round 在 safego 里跑、正常返回才写;Stop 删序列(族里只删自己那条)' {
    $looplive = Read-RepoText -RelativePath 'go/shared/looplive/looplive.go'
    Assert-Match -Text $looplive -Pattern 'prometheus\.NewGaugeVec\(opts, nil\)' -Because 'GaugeVec 不带可变 label:第一次 Mark 之前、Stop 之后都没有序列(Gauge 一注册就报 0,会让没起循环的副本被判成停摆)'
    Assert-Match -Text $looplive -Pattern 'prometheus\.NewGaugeVec\(opts, \[\]string\{label\}\)' -Because '一族只带一个区分循环的 label'
    Assert-Match -Text $looplive -Pattern '(?s)func \(g \*Gauge\) Round\(point string, round func\(\)\) bool \{\s*if !safego\.Run\(point, round\) \{\s*return false\s*\}\s*g\.Mark\(time\.Now\(\)\)\s*return true\s*\}' -Because 'Round:panic 的一轮被 safego 兜住、不写时刻;正常返回才写'
    Assert-Match -Text $looplive -Pattern '(?s)func \(g \*Gauge\) Stop\(\) \{.*?g\.vec\.Reset\(\).*?g\.vec\.DeleteLabelValues\(g\.value\)' -Because 'Stop 删掉序列:单独的 gauge 清空,族里的只删自己那一条(别的循环照常在报)'
    # match 的一族:label 名是 point(与 safego_panic_total 同名,值都取循环的点位名),挂进 register()。
    Assert-Match -Text (Read-RepoText -RelativePath 'go/match/internal/metrics/loops.go') -Pattern '(?s)LoopLive = looplive\.NewFamily\(.*?\}, "point"\)' -Because 'match 的一族按 point 区分循环'
    Assert-Match -Text (Read-RepoText -RelativePath 'go/match/internal/metrics/loops.go') -Pattern 'LoopLive\.Collector\(\)' -Because 'loopCollectors 要带上这一族'
    Assert-Match -Text (Read-RepoText -RelativePath 'go/match/internal/metrics/metrics.go') -Pattern 'prometheus\.MustRegister\(loopCollectors\(\)\.\.\.\)' -Because 'match 的一族要挂进 register()'
    $matchContext = Read-RepoText -RelativePath 'go/match/internal/svc/servicecontext.go'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($matchContext, 'live := metrics\.LoopLive\.For\(point\)').Count) -Because 'StartSourceLoop 与 StartRelayLoop 各从一族里取自己点位的那一条'
    Assert-Match -Text (Read-RepoText -RelativePath 'go/match/internal/authority/rank_publisher.go') -Pattern 'live := metrics\.LoopLive\.For\(rankPublishPoint\)' -Because '投榜循环用自己的点位名'
    foreach ($loop in $script:Loops) {
        $source = Read-RepoText -RelativePath $loop.Define.Path
        Assert-Match -Text $source -Pattern $loop.Define.Pattern -Because "$($loop.Metric) 必须用 looplive 定义,名字与告警里的一致(改名后告警静默失效)"
    }
    # 指标名前缀的来源。
    Assert-Match -Text (Read-RepoText -RelativePath 'go/data_service/internal/metrics/metrics.go') -Pattern '(?m)^const subsystem = "data_service"' -Because 'data_service 指标名前缀'
    Assert-Match -Text (Read-RepoText -RelativePath 'go/mail/internal/metrics/metrics.go') -Pattern '(?m)^const subsystem = "mail"' -Because 'mail 指标名前缀'
    Assert-Match -Text (Read-RepoText -RelativePath 'go/guild/internal/data/guild_manage_repo.go') -Pattern 'const guildMetricNamespace = "guild"' -Because 'guild 指标名前缀'
    # 手工注册的两个要真的挂进注册表。
    Assert-Match -Text (Read-RepoText -RelativePath 'go/mail/internal/metrics/metrics.go') -Pattern 'SweepLive\.Collector\(\),' -Because 'mail 的活性 gauge 要挂进 Register()'
    Assert-Match -Text (Read-RepoText -RelativePath 'go/match/internal/metrics/outcome.go') -Pattern '(?s)func outcomeCollectors\(\) \[\]prometheus\.Collector \{.*?OutcomeOutboxLive\.Collector\(\).*?\}' -Because 'match 的活性 gauge 要挂进 register()'
}

Test-Case '每处循环:起来时 Mark;每一轮经 Round;Stop 只写在有意停止的 return 之前;不 defer 写时刻、不 defer 删序列' {
    foreach ($loop in $script:Loops) {
        foreach ($site in $loop.Sites) {
            $text = Read-RepoText -RelativePath $site.Path
            Assert-Match -Text $text -Pattern ('{0}\.Mark\(time\.Now\(\)\)' -f $site.Live) -Because "$($site.Path):循环起来时先写一次(首轮之前可能有随机延迟)"
            Assert-Match -Text $text -Pattern $site.Round -Because "$($site.Path):每一轮都经 Round 跑(panic 兜住、正常返回才写时刻)"
            Assert-Match -Text $text -Pattern ('{0}\.Stop\(\)\s*\n\s*return' -f $site.Live) -Because "$($site.Path):有意停止时先删序列再 return"
            Assert-NotMatch -Text $text -Pattern ('defer\s+{0}\.(Mark|Stop)\(' -f $site.Live) -Because "$($site.Path):Mark / Stop 都不能 defer(defer 在 panic 展开时也执行:会记错时刻,或把循环异常退出的证据删掉)"
            # Stop 的每一处后面都紧跟 return:它不是「清一下」,是「这个循环到此为止」。
            $stops = [regex]::Matches($text, ('{0}\.Stop\(\)' -f $site.Live)).Count
            $stopsThenReturn = [regex]::Matches($text, ('{0}\.Stop\(\)\s*\n\s*return' -f $site.Live)).Count
            Assert-Equal -Expected $stops -Actual $stopsThenReturn -Because "$($site.Path):每一处 Stop 后面都应紧跟 return"
        }
    }
}

Test-Case '兜住的 panic 有人报:通用规则按副本、按点位判 safego_panic_total 大于 0;各循环的点位名是代码常量' {
    Assert-Match -Text $script:BackgroundAlerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与其余告警文件同一安装方式)'
    Assert-Equal -Expected 'GoBackgroundPanicRecovered' -Actual (($script:BackgroundRules | ForEach-Object { $_.Name }) -join ',') -Because 'go.background-panic 组只有一条'
    $rule = Get-Rule -Rules $script:BackgroundRules -Name 'GoBackgroundPanicRecovered'
    Assert-Match -Text $rule.Body -Pattern '(?m)^            severity:[ ]*warning[ ]*$' -Because '兜住的 panic 不打断服务,取 warning'
    Assert-Match -Text $rule.Body -Pattern '(?m)^          for:[ ]*0m[ ]*$' -Because '第一次 panic 就要报'
    $expr = Get-RuleExpr -Body $rule.Body
    Assert-Match -Text $expr -Pattern '^\s*max by \(namespace, pod, point\) \(safego_panic_total\) > 0\s*$' -Because '对全部点位按副本、按点位判计数大于 0(不列点位名单:名单会漏掉新加的循环)'
    Assert-NotMatch -Text $expr -Pattern '\b(increase|rate|delta)\b' -Because '计数序列在第一次 panic 时才出现,increase() 看不到第一次;进程重启才清零,与 HOLD 同寿命'
    $safego = Read-RepoText -RelativePath 'go/shared/safego/safego.go'
    Assert-Match -Text $safego -Pattern '(?s)Name:\s*"safego_panic_total".*?\[\]string\{"point"\}' -Because '计数由 shared/safego 注册,label 是 point'
    # 各循环的点位名是代码里的常量(停摆告警的描述按它指路);match 那一族的点位名单在节拍用例里。
    foreach ($loop in $script:Loops) {
        Assert-Match -Text (Read-RepoText -RelativePath $loop.Point.Path) -Pattern $loop.Point.Pattern -Because "点位 $($loop.Point.Name) 必须是 $($loop.Point.Path) 里的常量"
    }
    # trade 的清理有两处循环(带 / 不带排空 SOURCE),两个常量必须同值。
    Assert-Match -Text (Read-RepoText -RelativePath 'go/trade/internal/data/asset_op_repo.go') -Pattern 'const cleanupPoint = "trade\.asset_op_cleanup"' -Because 'trade 两处清理循环用同一个点位名'
    # match 的撮合(StartSourceLoop)、成局推进等(StartRelayLoop)与投榜(RunRankPublisher)每一轮都经 Round(上一个用例):
    # 一轮 panic 只丢这一轮,不许让循环退出(进程还在成员表里,它的片从此没人做),也不许一路抛出去打死进程(片换主后挨个打死)。
    Assert-NotMatch -Text (Read-RepoText -RelativePath 'go/match/internal/authority/rank_publisher.go') -Pattern 'safego\.Run\(' -Because '投榜的每一轮只经 live.Round 兜住,不另包一层'
}

Test-Case '停摆阈值大于一轮最长耗时的若干倍,与代码常量、服务配置同源' {
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

    # match 发件箱:一轮总预算(outcomeOutboxRoundBudget)+ 缺省间隔(GlobalMatch.OutcomeOutbox.IntervalMs)。
    $matchLoop = Read-RepoText -RelativePath 'go/match/internal/svc/outcome_outbox_loop.go'
    $matchBudget = [regex]::Match($matchLoop, 'outcomeOutboxRoundBudget\s*=\s*(?<s>\d+)\s*\*\s*time\.Second')
    $matchInterval = [regex]::Match((Read-RepoText -RelativePath 'go/match/internal/config/config.go'), '\bIntervalMs\s+int\s+`json:",default=(?<ms>\d+)"`')
    Assert-True -Condition ($matchBudget.Success -and $matchInterval.Success) -Because '夹具前提:能读到发件箱一轮的总预算与缺省间隔'
    $matchRoundSec = [int]$matchBudget.Groups['s'].Value + [int]$matchInterval.Groups['ms'].Value / 1000
    $terms = Get-StallTerms -Expr (Get-RuleExpr -Body (Get-Rule -Rules $script:MatchRules -Name 'MatchOutcomeOutboxLoopStalled').Body)
    Assert-True -Condition ($terms['match_outcome_outbox_last_run_unixtime'] -ge 5 * $matchRoundSec) -Because "发件箱阈值应不小于 5 × (一轮总预算 + 缺省间隔) = $(5 * $matchRoundSec) 秒"
    # mail 清理的阈值与 Sweep.Interval 的联动在 k8s_deploy_contract.tests.ps1 的 mail 告警用例里。
}

Test-Case 'match 后台循环一族:点位名单钉死;每个调用点的间隔都登记过、来源逐项核对;阈值不小于 3 × (最长间隔 + 最长一轮预算)' {
    $matchSources = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'go/match/internal') -Filter '*.go' -File -Recurse |
            Where-Object { $_.Name -notlike '*_test.go' } |
            ForEach-Object { ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n" })
    $calls = @($matchSources | ForEach-Object {
            [regex]::Matches($_, '\.Start(?:Relay|Source)Loop\(\s*\w+,\s*(?<point>"[^"]+"|\w+),\s*(?<interval>[^,]+?),') |
                ForEach-Object { [pscustomobject]@{ Point = $_.Groups['point'].Value; Interval = $_.Groups['interval'].Value.Trim() } }
        })
    Assert-True -Condition ($calls.Count -ge 1) -Because '夹具前提:能在 go/match/internal 里找到 StartSourceLoop / StartRelayLoop 的调用点'

    # 点位名单:调用点里写死的点位、整队 SQL 恢复经 start(状态, 点位) 传进去的四个、投榜。新加循环要在这里登记,并按下面的节拍表核对阈值。
    $team = Read-RepoText -RelativePath 'go/match/internal/team/sql_recovery.go'
    $points = @($calls | Where-Object { $_.Point.StartsWith('"') } | ForEach-Object { $_.Point.Trim('"') }) +
        @([regex]::Matches($team, '\bstart\(\d+,\s*"(?<p>[^"]+)"\)') | ForEach-Object { $_.Groups['p'].Value }) + @('match.rank_publish')
    Assert-Equal -Expected 1 -Actual @($calls | Where-Object { -not $_.Point.StartsWith('"') }).Count -Because '只有整队 SQL 恢复的 start(状态, 点位) 用变量传点位名'
    $expectedPoints = @('match.battle_instance_loss', 'match.global', 'match.global.expired.audit', 'match.global.outcome.recover', 'match.global.pool',
        'match.global.ready.scan', 'match.global.spectate.cleanup', 'match.global.spectator_intent.purge', 'match.rank_publish',
        'match.team.sql.completed', 'match.team.sql.forming', 'match.team.sql.idle', 'match.team.sql.prepared', 'match.team.sql.queued')
    Assert-Equal -Expected (@($expectedPoints | Sort-Object) -join ',') -Actual (@($points | Sort-Object) -join ',') -Because 'match_loop_last_run_unixtime 的点位名单(label 值是这十四个常量,不放实体 id)'

    # 节拍表:调用点上的间隔写法 → 秒。下面逐项核对每个名字的定义。
    $matchYaml = Read-RepoText -RelativePath 'go/match/etc/match_service.yaml'
    $matcherMs = [regex]::Match($matchYaml, '(?m)^MatcherIntervalMs:\s*(?<ms>\d+)\s*$')
    if (-not $matcherMs.Success) {
        $matcherMs = [regex]::Match((Read-RepoText -RelativePath 'go/match/internal/config/config.go'), 'MatcherIntervalMs\s+int64\s+`json:",default=(?<ms>\d+)"`')
    }
    Assert-True -Condition $matcherMs.Success -Because '夹具前提:能从服务 yaml 或代码缺省读到 MatcherIntervalMs'
    $intervalSeconds = @{
        '500*time.Millisecond'         = 0.5
        'time.Second'                  = 1
        'globalOutcomeRecoverInterval' = 1
        'globalExpiredAuditInterval'   = 60
        'cleanupEvery'                 = 60
        'purgeEvery'                   = 60
        'interval'                     = [int]$matcherMs.Groups['ms'].Value / 1000
    }
    foreach ($call in $calls) {
        Assert-True -Condition ($intervalSeconds.ContainsKey($call.Interval)) -Because "调用点 $($call.Point) 的间隔写法 $($call.Interval) 没登记:先确认它加一轮预算远小于 MatchBackgroundLoopStalled 的阈值,再登记到节拍表"
    }
    Assert-Match -Text (Read-RepoText -RelativePath 'go/match/internal/logic/global_outcome_recover.go') -Pattern 'const globalOutcomeRecoverInterval = authority\.ExpiredOutcomeRecoverTick' -Because '到期终局恢复的间隔来源'
    Assert-Match -Text (Read-RepoText -RelativePath 'go/match/internal/authority/terminal_abort.go') -Pattern 'const ExpiredOutcomeRecoverTick = time\.Second' -Because '到期终局恢复 1 秒一拍'
    Assert-Match -Text (Read-RepoText -RelativePath 'go/match/internal/logic/global_expired_audit.go') -Pattern 'const globalExpiredAuditInterval = time\.Minute' -Because '滞留成局巡检 1 分钟一拍'
    $matcher = Read-RepoText -RelativePath 'go/match/internal/logic/matcher.go'
    Assert-Match -Text $matcher -Pattern 'startGlobalSpectateMaintenance\(ctx, svcCtx, globalSpectateCleanupInterval, globalSpectatorIntentPurgeInterval\)' -Because '观战维护的两个间隔来源'
    $spectator = Read-RepoText -RelativePath 'go/match/internal/logic/global_spectator.go'
    Assert-Match -Text $spectator -Pattern 'globalSpectateCleanupInterval\s*=\s*time\.Minute' -Because '观战索引清理 1 分钟一拍'
    Assert-Match -Text $spectator -Pattern 'globalSpectatorIntentPurgeInterval\s*=\s*time\.Minute' -Because '观战意图保留期清理 1 分钟一拍'
    Assert-Match -Text (Read-RepoText -RelativePath 'go/match/internal/logic/global_pool_matcher.go') -Pattern 'interval := time\.Duration\(s\.Config\.MatcherIntervalMs\) \* time\.Millisecond' -Because '按池撮合的间隔来源'
    $rank = Read-RepoText -RelativePath 'go/match/internal/authority/rank_publisher.go'
    $rankTick = [regex]::Match($rank, 'time\.NewTicker\((?<s>\d+) \* time\.Second\)')
    $rankBudget = [regex]::Match($rank, 'rankPublishRoundTimeout\s*=\s*(?<s>\d+) \* time\.Second')
    Assert-True -Condition ($rankTick.Success -and $rankBudget.Success) -Because '夹具前提:能读到投榜的节拍与一轮预算'

    # 一轮预算:现有最长的是全球撮合一轮的 30 秒(其余 3~5 秒)。
    Assert-Match -Text (Read-RepoText -RelativePath 'go/match/internal/logic/global_match.go') -Pattern 'context\.WithTimeout\(ctx, 30\*time\.Second\)' -Because '全球撮合一轮的预算 30 秒'
    $longestSec = [math]::Max((@($intervalSeconds.Values) | Measure-Object -Maximum).Maximum, [double]$rankTick.Groups['s'].Value) + [math]::Max(30, [double]$rankBudget.Groups['s'].Value)
    $terms = Get-StallTerms -Expr (Get-RuleExpr -Body (Get-Rule -Rules $script:MatchLoopRules -Name 'MatchBackgroundLoopStalled').Body)
    Assert-True -Condition ($terms['match_loop_last_run_unixtime'] -ge 3 * $longestSec) -Because "match 后台循环阈值 $($terms['match_loop_last_run_unixtime']) 秒应不小于 3 × (最长间隔 + 最长一轮预算) = $(3 * $longestSec) 秒"
}

Test-Case '活性与 panic 告警名在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in @('AssetOpRedeliveryLoopStalled', 'RecallDispatchLoopStalled', 'AssetOpCleanupLoopStalled', 'MailSweepStalled', 'MatchOutcomeOutboxLoopStalled', 'MatchBackgroundLoopStalled', 'GoBackgroundPanicRecovered')) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'loop_liveness_alerts_contract')
