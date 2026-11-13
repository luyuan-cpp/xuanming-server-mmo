<#
.SYNOPSIS
    区服策略发布、本区角色上限守卫与「新玩家还有没有区可进」的告警,与 Java / Go 源码的契约(docs/design/zone-full-handling.md §5、§6)。

.DESCRIPTION
    deploy/k8s/gateway-zone-policy-alerts.yaml 的规则只在序列存在时才可能响,写错指标名或 outcome 不会报错,只会永远不响。
    这里逐项对照源码:
      - 规则引用的每个指标,都由 Micrometer 以对应的名字注册(Prometheus 名 = Micrometer 名把 . 换成 _,计数器再加 _total),
        规则里用到的 outcome 取值确实注册了;文件头登记的两个看板 gauge 也真的存在;
      - 告警 description 让值班去日志里找的原文,确实是网关、login、data_service 打出的;
      - PrometheusRule 形状、severity / summary / description / for,三条告警名在 deploy/k8s 的告警文件里不重名。
    只读文本,不执行部署入口,不访问集群。规则语法由 promtool 另验。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/gateway_zone_policy_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/gateway-zone-policy-alerts.yaml'
$script:JavaRoot = 'java/gateway_node/src/main/java/com/game/gateway'
$script:Publisher = Read-RepoText -RelativePath "$($script:JavaRoot)/service/ZonePolicyPublisher.java"
$script:Guard = Read-RepoText -RelativePath "$($script:JavaRoot)/service/RoleCapacityGuard.java"
$script:NewPlayers = Read-RepoText -RelativePath "$($script:JavaRoot)/service/NewPlayerCapacityMetrics.java"
$script:LoginLogic = Read-RepoText -RelativePath 'go/login/internal/logic/loginpregate/getzonerolecountlogic.go'
$script:DataServer = Read-RepoText -RelativePath 'go/data_service/internal/server/dataserviceserver.go'
$script:AlertNames = @('GatewayZonePolicyPublishFailing', 'GatewayRoleCapacityCheckFailing', 'GatewayNoZoneAcceptsNewCharacters')

Write-Host '=== gateway_zone_policy_alerts_contract ==='

Test-Case 'PrometheusRule 形状:kind、三条告警、severity、summary、description、for 都在' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 owner-epoch-alerts.yaml 同一安装方式)'
    foreach ($name in $script:AlertNames) {
        Assert-Match -Text $script:Alerts -Pattern ('(?m)^\s+- alert:[ ]*{0}[ ]*$' -f $name) -Because "规则名 $name 固定,值班手册按名字查"
    }
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+severity:[ ]*warning[ ]*$').Count) -Because '发布与守卫两条是 warning:策略停在旧值、不自动关创角都不挡登录,下个工作日处理'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+severity:[ ]*critical[ ]*$').Count) -Because '没有区能建角是 critical:新玩家全部进不来,等不到下个工作日'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+for:[ ]*10m[ ]*$').Count) -Because '发布与守卫两条持续 10 分钟才响,盖过一次 etcd 或 login 的滚动重启'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+for:[ ]*5m[ ]*$').Count) -Because '没有区能建角持续 5 分钟才响,盖过「先关老区、再开新区」的操作间隙'
    Assert-Equal -Expected 3 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+summary:').Count) -Because '每条告警要有 summary'
    Assert-Equal -Expected 3 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+description:').Count) -Because '每条告警要有 description'
}

Test-Case '规则引用的指标与 outcome 都是 Java 用 Micrometer 注册的;文件头登记的看板 gauge 也存在' {
    $counters = @(
        @{ Prometheus = 'gateway_zone_policy_publish_total';  Micrometer = 'gateway.zone_policy.publish';   Source = $script:Publisher; File = 'ZonePolicyPublisher.java' }
        @{ Prometheus = 'gateway_role_capacity_check_total';  Micrometer = 'gateway.role_capacity.check';   Source = $script:Guard;     File = 'RoleCapacityGuard.java' }
    )
    $ruleGauges = @(
        @{ Prometheus = 'gateway_zones_open';                  Micrometer = 'gateway.zones.open' }
        @{ Prometheus = 'gateway_zones_accepting_new_players'; Micrometer = 'gateway.zones.accepting_new_players' }
    )
    # 一条 expr 可以引用不止一个指标:逐行取 expr,再取行内全部 gateway_ 开头的名字。
    $referenced = @([regex]::Matches($script:Alerts, '(?m)^\s+expr:.*$') | ForEach-Object { [regex]::Matches($_.Value, '\bgateway_[a-z_]+\b') } |
            ForEach-Object { $_.Value } | Sort-Object -Unique)
    $expected = @(@($counters | ForEach-Object { $_.Prometheus }) + @($ruleGauges | ForEach-Object { $_.Prometheus }) | Sort-Object)
    Assert-Equal -Expected ($expected -join ',') -Actual ($referenced -join ',') -Because '规则只引用两个计数器与两个区数 gauge'
    foreach ($gauge in $ruleGauges) {
        Assert-Equal -Expected $gauge.Prometheus -Actual ($gauge.Micrometer -replace '\.', '_') -Because '两个名字的对应关系(Micrometer 的 Prometheus 命名约定)'
        Assert-Match -Text $script:NewPlayers -Pattern ('Gauge\.builder\("{0}"' -f [regex]::Escape($gauge.Micrometer)) -Because "NewPlayerCapacityMetrics 要以 $($gauge.Micrometer) 注册 gauge,否则规则永远不响"
    }
    foreach ($counter in $counters) {
        Assert-Equal -Expected $counter.Prometheus -Actual (($counter.Micrometer -replace '\.', '_') + '_total') -Because '两个名字的对应关系(Micrometer 的 Prometheus 命名约定,计数器加 _total)'
        Assert-Match -Text $counter.Source -Pattern ('Counter\.builder\("{0}"\)' -f [regex]::Escape($counter.Micrometer)) -Because "$($counter.File) 要以 $($counter.Micrometer) 注册计数器,否则规则永远不响"
        Assert-Match -Text $counter.Source -Pattern 'ERROR\("error"\)' -Because "$($counter.File) 的 outcome 封闭集合里要有 error(规则只看它)"
        Assert-Match -Text $counter.Source -Pattern '\.tag\("outcome", outcome\.label\)' -Because "$($counter.File) 按 outcome 标签逐个预置"
    }
    foreach ($gauge in @('gateway.zone.role_count', 'gateway.zone.role_capacity')) {
        Assert-Match -Text $script:Alerts -Pattern ([regex]::Escape($gauge -replace '\.', '_')) -Because "文件头登记了 $gauge"
        Assert-Match -Text $script:Guard -Pattern ('Gauge\.builder\("{0}"' -f [regex]::Escape($gauge)) -Because "RoleCapacityGuard 注册 $gauge"
    }
}

Test-Case '告警 description 让值班去找的日志原文都是代码打出的' {
    $phrases = @(
        @{ Text = 'Zone policy publication failed for zone'; Source = $script:Publisher;  File = 'ZonePolicyPublisher.java' }
        @{ Text = 'Role capacity check failed for zone';     Source = $script:Guard;      File = 'RoleCapacityGuard.java' }
        @{ Text = 'GetZoneRoleCount zone=';                  Source = $script:LoginLogic; File = 'getzonerolecountlogic.go' }
        @{ Text = '[DataRoute] CountZoneRoles';              Source = $script:DataServer; File = 'dataserviceserver.go' }
        @{ Text = 'reached its role capacity';               Source = $script:Guard;      File = 'RoleCapacityGuard.java' }
    )
    foreach ($phrase in $phrases) {
        Assert-Match -Text $script:Alerts -Pattern ([regex]::Escape($phrase.Text)) -Because "告警里引用了「$($phrase.Text)」"
        Assert-Match -Text $phrase.Source -Pattern ([regex]::Escape($phrase.Text)) -Because "$($phrase.File) 要打出「$($phrase.Text)」"
    }
}

Test-Case '三条告警名在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in $script:AlertNames) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'gateway_zone_policy_alerts_contract')
