<#
.SYNOPSIS
    网关区服健康告警与 Java 指标的契约(docs/design/gateway-zone-health-publish-20261111.md,服务器缺口清单第 29 条)。

.DESCRIPTION
    deploy/k8s/gateway-zone-health-alerts.yaml 的规则只在序列存在时才可能响,写错指标名不会报错,只会永远不响。
    这里逐项对照 java/gateway_node 里注册指标的源码:
      - 规则引用的每个指标,都由 Micrometer 以对应的名字注册(Prometheus 名 = Micrometer 名把 . 换成 _);
      - 日志关键字(告警 description 让值班去日志里找的那几句)确实是代码打出的;
      - PrometheusRule 形状、severity / summary / description / for,两条告警名在 deploy/k8s 的告警文件里不重名。
    只读文本,不执行部署入口,不访问集群。规则语法由 promtool 另验。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/gateway_zone_health_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/gateway-zone-health-alerts.yaml'
$script:JavaRoot = 'java/gateway_node/src/main/java/com/game/gateway'
$script:Probe = Read-RepoText -RelativePath "$($script:JavaRoot)/service/ZoneHealthProbeService.java"
$script:Publisher = Read-RepoText -RelativePath "$($script:JavaRoot)/service/ZoneHealthPublisher.java"
$script:Mirror = Read-RepoText -RelativePath "$($script:JavaRoot)/etcd/EtcdPrefixMirror.java"
$script:Watcher = Read-RepoText -RelativePath "$($script:JavaRoot)/etcd/ZoneHealthWatcher.java"
$script:AlertNames = @('GatewayZoneHealthUnpublished', 'GatewayZoneHealthStale')

Write-Host '=== gateway_zone_health_alerts_contract ==='

Test-Case 'PrometheusRule 形状:kind、两条告警、severity、summary、description、for 5m 都在' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 owner-epoch-alerts.yaml 同一安装方式)'
    foreach ($name in $script:AlertNames) {
        Assert-Match -Text $script:Alerts -Pattern ('(?m)^\s+- alert:[ ]*{0}[ ]*$' -f $name) -Because "规则名 $name 固定,值班手册按名字查"
    }
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+severity:[ ]*warning[ ]*$').Count) -Because '两条都是 warning:列表退化成 UNKNOWN 不挡登录,下个工作日处理'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+for:[ ]*5m[ ]*$').Count) -Because '两条都持续 5 分钟才响,盖过一次滚动重启'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+summary:').Count) -Because '每条告警要有 summary'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+description:').Count) -Because '每条告警要有 description'
}

Test-Case '规则引用的指标都是 Java 用 Micrometer 注册的(Prometheus 名 = Micrometer 名把 . 换成 _)' {
    $metrics = @(
        @{ Prometheus = 'gateway_zone_health_unpublished_zones';    Micrometer = 'gateway.zone_health.unpublished_zones';    Source = $script:Probe;     File = 'ZoneHealthProbeService.java' }
        @{ Prometheus = 'gateway_zone_health_snapshot_age_seconds'; Micrometer = 'gateway.zone_health.snapshot_age_seconds'; Source = $script:Probe;     File = 'ZoneHealthProbeService.java' }
    )
    $referenced = @([regex]::Matches($script:Alerts, '(?m)^\s+expr:.*?\b(?<m>gateway_[a-z_]+)\b') | ForEach-Object { $_.Groups['m'].Value } | Sort-Object -Unique)
    Assert-Equal -Expected (@($metrics | ForEach-Object { $_.Prometheus } | Sort-Object) -join ',') -Actual ($referenced -join ',') -Because '规则只引用这两个指标'
    foreach ($metric in $metrics) {
        Assert-Equal -Expected $metric.Prometheus -Actual ($metric.Micrometer -replace '\.', '_') -Because '两个名字的对应关系(Micrometer 的 Prometheus 命名约定)'
        Assert-Match -Text $metric.Source -Pattern ('Gauge\.builder\("{0}"' -f [regex]::Escape($metric.Micrometer)) -Because "$($metric.File) 要以 $($metric.Micrometer) 注册 gauge,否则规则永远不响"
    }
    # 文件头列出的计数器(看板用)也要真的存在,免得文档与代码漂移。
    Assert-Match -Text $script:Alerts -Pattern 'gateway_zone_health_publish_total\{outcome\}' -Because '文件头登记了发布计数器'
    Assert-Match -Text $script:Publisher -Pattern 'Counter\.builder\("gateway\.zone_health\.publish"\)' -Because 'ZoneHealthPublisher 注册发布计数器'
    foreach ($outcome in @('written', 'withdrawn', 'failed')) {
        Assert-Match -Text $script:Alerts -Pattern ([regex]::Escape($outcome)) -Because "文件头列出 outcome=$outcome"
        Assert-Match -Text $script:Publisher -Pattern ('outcomeCounter\(meterRegistry, "{0}"\)' -f $outcome) -Because "ZoneHealthPublisher 注册 outcome=$outcome"
    }
}

Test-Case '告警 description 让值班去找的日志原文都是代码打出的' {
    $phrases = @(
        @{ Text = 'Zones with a registered login but no health record'; Source = $script:Probe; File = 'ZoneHealthProbeService.java' }
        @{ Text = 'health publisher could not';                          Source = $script:Publisher; File = 'ZoneHealthPublisher.java' }
        @{ Text = 'health record withdrawn';                             Source = $script:Publisher; File = 'ZoneHealthPublisher.java' }
        @{ Text = 'Zone health probe failed';                            Source = $script:Probe; File = 'ZoneHealthProbeService.java' }
    )
    foreach ($phrase in $phrases) {
        Assert-Match -Text $script:Alerts -Pattern ([regex]::Escape($phrase.Text)) -Because "告警里引用了「$($phrase.Text)」"
        Assert-Match -Text $phrase.Source -Pattern ([regex]::Escape($phrase.Text)) -Because "$($phrase.File) 要打出「$($phrase.Text)」"
    }
    # "Failed to parse zone health" = EtcdPrefixMirror 的解码失败日志 + ZoneHealthWatcher 给的记录叫法。
    Assert-Match -Text $script:Alerts -Pattern 'Failed to parse zone health' -Because '告警里引用了坏记录的日志'
    Assert-Match -Text $script:Mirror -Pattern 'Failed to parse \{\} from key=' -Because 'EtcdPrefixMirror 的解码失败日志'
    Assert-Match -Text $script:Watcher -Pattern '"zone health"' -Because 'ZoneHealthWatcher 的记录叫法是 zone health'
}

Test-Case '两条告警名在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in $script:AlertNames) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'gateway_zone_health_alerts_contract')
