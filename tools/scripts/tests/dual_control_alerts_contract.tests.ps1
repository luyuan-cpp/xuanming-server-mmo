#requires -Version 7
<#
.SYNOPSIS
    双人复核「不带批准记录被接受」告警的契约测试(2026-11-26,docs/design/admin-identity-and-dual-control.md §11.4)。

.DESCRIPTION
    deploy/k8s/dual-control-alerts.yaml 每个资源服务一条规则,数各自的「不带批准记录而被接受」计数。这里钉的都是写错了不报错、
    只会永远不响的东西:
      - PrometheusRule 形状、规则名单、每条规则的 severity / summary / description / runbook_url;
      - 每条规则数的指标名,在对应服务的源码里确实注册过(改名要两边一起改);
      - trade 的告警名与 trade 配置注释里写的同名;
      - 告警名在 deploy/k8s 的全部告警文件里不重名。
    只读文本,不执行部署入口,不访问任何集群。规则语法由 promtool 另验(见 docs/PROGRESS.md 对应条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/dual_control_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

# 目录下(递归、不含测试文件)有没有哪份源码含这段文本。
function Test-SourceContains {
    param([Parameter(Mandatory = $true)][string]$RelativeDir, [Parameter(Mandatory = $true)][string]$Text)
    foreach ($file in Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot $RelativeDir) -File -Recurse -Include '*.go', '*.java') {
        if ($file.Name -like '*_test.go' -or $file.FullName -match '[\\/]src[\\/]test[\\/]') { continue }
        if (([IO.File]::ReadAllText($file.FullName)).Contains($Text)) { return $true }
    }
    return $false
}

function Get-AlertRules {
    param([Parameter(Mandatory = $true)][string]$Text)
    return @([regex]::Matches($Text, '(?ms)^        - alert:[ ]*(?<name>\S+)[ ]*\n(?<body>.*?)(?=^        - alert:|^        #|\z)') |
            ForEach-Object { [pscustomobject]@{ Name = $_.Groups['name'].Value; Body = $_.Groups['body'].Value } })
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/dual-control-alerts.yaml'

# 规则名 → 指标名、源码目录、源码里能找到的注册片段(Go 是 Subsystem/Namespace + Name 拆开写的,所以找去掉服务前缀的那一段)。
$script:ExpectedRules = [ordered]@{
    'TradeManualResolutionUnapprovedExecute' = @{ Metric = 'trade_manual_resolution_unapproved_execute_total'; Dir = 'go/trade'; Fragment = 'manual_resolution_unapproved_execute_total' }
    'MallAdminCommandUnapproved'             = @{ Metric = 'mall_admin_unapproved_command_total'; Dir = 'java/mall_game/src/main'; Fragment = 'mall.admin.unapproved.command' }
    'MailAdminCommandUnapproved'             = @{ Metric = 'mail_admin_unapproved_command_total'; Dir = 'go/mail'; Fragment = 'admin_unapproved_command_total' }
    'LoginAdminCommandUnapproved'            = @{ Metric = 'login_admin_unapproved_command_total'; Dir = 'go/login'; Fragment = 'admin_unapproved_command_total' }
    'DataServiceAdminCommandUnapproved'      = @{ Metric = 'data_service_admin_unapproved_command_total'; Dir = 'go/data_service'; Fragment = 'admin_unapproved_command_total' }
    'PlayerInboxAdminCommandUnapproved'      = @{ Metric = 'player_inbox_admin_unapproved_command_total'; Dir = 'go/player_inbox'; Fragment = 'admin_unapproved_command_total' }
    'RankAdminCommandUnapproved'             = @{ Metric = 'rank_admin_unapproved_command_total'; Dir = 'go/rank'; Fragment = 'admin_unapproved_command_total' }
    'MatchAdminCommandUnapproved'            = @{ Metric = 'match_admin_unapproved_command_total'; Dir = 'go/match'; Fragment = 'admin_unapproved_command_total' }
}

Test-Case 'dual-control-alerts.yaml:PrometheusRule 形状,规则名单与每条规则的字段' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与其余告警文件同一安装方式)'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^  name:[ ]*dual-control-alerts[ ]*$' -Because 'metadata.name'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    - name:[ ]*admin\.dual-control[ ]*\n      interval:[ ]*30s[ ]*$' -Because '组名与评估间隔'
    $rules = @(Get-AlertRules -Text $script:Alerts)
    Assert-Equal -Expected (($script:ExpectedRules.Keys | Sort-Object) -join ',') -Actual ((@($rules | ForEach-Object { $_.Name }) | Sort-Object) -join ',') -Because '规则名单钉死;新接入的资源服务要同步这里'
    foreach ($rule in $rules) {
        $want = $script:ExpectedRules[$rule.Name]
        Assert-Match -Text $rule.Body -Pattern ('(?m)^            sum\(increase\({0}\[5m\]\)\) > 0[ ]*$' -f [regex]::Escape($want.Metric)) `
            -Because "$($rule.Name) 数 $($want.Metric) 在 5 分钟内的增长;计数在集群里应当恒为 0"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            severity:[ ]*critical[ ]*$' -Because "$($rule.Name) 是应急出口被真的用了,要叫醒人"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            summary:' -Because "$($rule.Name) 要有 summary"
        Assert-Match -Text $rule.Body -Pattern '(?m)^            description:' -Because "$($rule.Name) 要有 description"
        $runbook = [regex]::Match($rule.Body, '(?m)^            runbook_url:[ ]*(?<path>\S+)[ ]*$').Groups['path'].Value
        Assert-True -Condition ($runbook -and (Test-Path (Join-Path $script:RepoRoot $runbook))) -Because "$($rule.Name) 的 runbook_url 指向的文档要存在"
    }
}

Test-Case '每条规则数的指标在对应服务的源码里注册过' {
    foreach ($name in $script:ExpectedRules.Keys) {
        $want = $script:ExpectedRules[$name]
        Assert-True -Condition (Test-SourceContains -RelativeDir $want.Dir -Text $want.Fragment) -Because "$name 数的 $($want.Metric) 要在 $($want.Dir) 里注册($($want.Fragment))"
    }
}

Test-Case 'trade 的告警名与 trade 配置注释一致' {
    $config = Read-RepoText -RelativePath 'go/trade/internal/config/config.go'
    Assert-Match -Text $config -Pattern 'TradeManualResolutionUnapprovedExecute' -Because 'trade 配置注释点名的告警要真的存在'
}

Test-Case '本文件钉的每条告警的名字在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in @($script:ExpectedRules.Keys)) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'dual_control_alerts_contract')
