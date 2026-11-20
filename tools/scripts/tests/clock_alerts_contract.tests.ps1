#requires -Version 7
<#
.SYNOPSIS
    节点时钟告警与启动时钟核对的契约测试(2026-11-19)。

.DESCRIPTION
    deploy/k8s/clock-alerts.yaml 比的是 node_exporter 报的节点时间和 Prometheus 的抓取时刻。这里钉的都是「写错了不报错、
    只会永远不响或者和启动门槛对不上」的东西:
      - PrometheusRule 形状、规则名单、每条规则的 severity / summary / description / runbook_url / for;
      - 两条规则都用 node_time_seconds 减 timestamp(node_time_seconds);
      - critical 的门槛和三种语言的启动拒启门槛是同一个 30 秒,开关是同一个 MMORPG_CLOCK_CHECK_MODE;
      - 每个 Go 服务的 main 都调了 clockcheck.MustPass,C++ Node 与镜像目录里的每个 Java 服务都接了核对;
      - 告警名在 deploy/k8s 的全部告警文件里不重名。
    只读文本,不执行部署入口,不访问任何集群。规则语法由 promtool 另验(见 docs/PROGRESS.md 对应条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/clock_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

# 一组里的每条规则:名字与规则体(到下一条规则、规则前注释或文件尾为止)。
function Get-AlertRules {
    param([Parameter(Mandatory = $true)][string]$Text)
    return @([regex]::Matches($Text, '(?ms)^        - alert:[ ]*(?<name>\S+)[ ]*\n(?<body>.*?)(?=^        - alert:|^        #|\z)') |
            ForEach-Object { [pscustomobject]@{ Name = $_.Groups['name'].Value; Body = $_.Groups['body'].Value } })
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/clock-alerts.yaml'
$script:GoCheck = Read-RepoText -RelativePath 'go/shared/clockcheck/clockcheck.go'
$script:CppCheck = Read-RepoText -RelativePath 'cpp/libs/engine/core/node/system/node/clock_check.h'
$script:NodeSource = Read-RepoText -RelativePath 'cpp/libs/engine/core/node/system/node/node.cpp'
$script:JavaCheck = Read-RepoText -RelativePath 'java/server_startup/src/main/java/com/game/startup/ClockCheck.java'
$script:JavaMains = [ordered]@{
    'gateway' = 'java/gateway_node/src/main/java/com/game/gateway/GatewayNodeApplication.java'
    'mall'    = 'java/mall_game/src/main/java/com/game/mall/MallGameApplication.java'
}

$script:ExpectedRules = [ordered]@{
    'NodeClockOffsetDrifting' = @{ Severity = 'warning'; Threshold = '1' }
    'NodeClockOffsetTooLarge' = @{ Severity = 'critical'; Threshold = '30' }
}

Test-Case 'clock-alerts.yaml:PrometheusRule 形状,规则名单与每条规则的字段' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与其余告警文件同一安装方式)'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^  name:[ ]*clock-alerts[ ]*$' -Because 'metadata.name'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    - name:[ ]*node\.clock[ ]*\n      interval:[ ]*30s[ ]*$' -Because '组名与评估间隔'
    $rules = @(Get-AlertRules -Text $script:Alerts)
    Assert-Equal -Expected (($script:ExpectedRules.Keys | Sort-Object) -join ',') -Actual ((@($rules | ForEach-Object { $_.Name }) | Sort-Object) -join ',') -Because '规则名单钉死'
    foreach ($rule in $rules) {
        $want = $script:ExpectedRules[$rule.Name]
        Assert-Match -Text $rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $want.Severity) -Because "$($rule.Name) 的 severity 应为 $($want.Severity)"
        Assert-Match -Text $rule.Body -Pattern ('(?m)^            abs\(node_time_seconds - timestamp\(node_time_seconds\)\) > {0}[ ]*$' -f $want.Threshold) `
            -Because "$($rule.Name) 比节点时间和抓取时刻,门槛 $($want.Threshold) 秒(用 time() 会混进规则评估的延迟)"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            summary:' -Because "$($rule.Name) 要有 summary"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            description:' -Because "$($rule.Name) 要有 description"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            runbook_url:[ ]*docs/ops/clock-sync-runbook\.md[ ]*$' -Because "$($rule.Name) 指向时钟运维手册"
        Assert-Match -Text $rule.Body -Pattern '(?m)^          for:[ ]*\d+[mh][ ]*$' -Because "$($rule.Name) 要写 for"
    }
    Assert-True -Condition (Test-Path (Join-Path $script:RepoRoot 'docs/ops/clock-sync-runbook.md')) -Because 'runbook_url 指向的手册要存在'
}

Test-Case '启动拒启门槛:三种语言都是 30 秒、同一个开关,critical 告警的门槛和它相同' {
    Assert-Match -Text $script:GoCheck -Pattern '(?m)^\s*MaxOffset = 30 \* time\.Second$' -Because 'Go 的拒启门槛'
    Assert-Match -Text $script:GoCheck -Pattern 'ModeEnv = "MMORPG_CLOCK_CHECK_MODE"' -Because 'Go 的开关名'
    Assert-Match -Text $script:CppCheck -Pattern 'kMaxOffsetMillis = 30 \* 1000;' -Because 'C++ 的拒启门槛'
    Assert-Match -Text $script:CppCheck -Pattern 'kModeEnv = "MMORPG_CLOCK_CHECK_MODE";' -Because 'C++ 的开关名'
    Assert-Match -Text $script:JavaCheck -Pattern 'MAX_OFFSET_MILLIS = 30_000;' -Because 'Java 的拒启门槛'
    Assert-Match -Text $script:JavaCheck -Pattern 'MODE_ENV = "MMORPG_CLOCK_CHECK_MODE";' -Because 'Java 的开关名'
}

Test-Case '接线:每个 Go 服务的 main、C++ Node 启动、两个 Java 服务的 main 都调了核对' {
    $mains = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'go') -Filter '*.go' -File -Recurse |
            Where-Object { $_.Name -notlike '*_test.go' } |
            Where-Object { ([IO.File]::ReadAllText($_.FullName)) -match 'fmt\.Println\(buildinfo\.StartupLine\("' })
    Assert-True -Condition ($mains.Count -ge 15) -Because '打版本行的 Go 服务入口'
    foreach ($main in $mains) {
        $text = ([IO.File]::ReadAllText($main.FullName)) -replace "`r`n", "`n"
        $service = [regex]::Match($text, 'fmt\.Println\(buildinfo\.StartupLine\("(?<s>[a-z_]+)"\)\)').Groups['s'].Value
        Assert-Match -Text $text -Pattern ('clockcheck\.MustPass\("{0}"\)' -f [regex]::Escape($service)) -Because "$($main.Name) 启动时要核对时钟"
    }
    Assert-Match -Text $script:NodeSource -Pattern '(?m)^\s*EnsureClockOrDie\(\);$' -Because 'C++ Node 启动时要核对时钟'
    foreach ($service in $script:JavaMains.Keys) {
        $main = Read-RepoText -RelativePath $script:JavaMains[$service]
        Assert-Match -Text $main -Pattern 'import com\.game\.startup\.ClockCheck;' -Because "Java $service 用共用模块 server_startup 里的核对"
        Assert-Match -Text $main -Pattern ('ClockCheck\.mustPass\("{0}"\);\s*\n\s*SpringApplication\.run' -f $service) -Because "Java $service 在 Spring 起来之前核对时钟"
    }
    $catalogue = Read-RepoText -RelativePath 'tools/scripts/java_svc_image.ps1'
    $deployed = @([regex]::Matches($catalogue, '(?m)^\s+(?<name>[a-z]+)\s+= @\{ Dir = "') | ForEach-Object { $_.Groups['name'].Value } | Sort-Object)
    Assert-Equal -Expected (($script:JavaMains.Keys | Sort-Object) -join ',') -Actual ($deployed -join ',') -Because '镜像目录里的每个 Java 服务都要接核对;新加服务要同步这里'
}

Test-Case '本文件钉的每条告警的名字在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in @($script:ExpectedRules.Keys)) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'clock_alerts_contract')
