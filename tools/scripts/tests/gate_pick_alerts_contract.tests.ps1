#requires -Version 7
<#
.SYNOPSIS
    挑 gate 回落告警的契约测试(缺口清单第 7 条,2026-11-05;11-06 排空并入分档)。

.DESCRIPTION
    login 的三处挑 gate 与 scene_manager 的跨 zone 重定向给候选分档:没有完全合格的 gate 时退到连得上的(排空中或到水位),
    全部停写才退到全部,退档只计 fallback、不拒绝。这两个计数的告警在 deploy/k8s/login-alerts.yaml(login.gate-pick 组)
    与 deploy/k8s/scene-manager-alerts.yaml(scene-manager.gate-redirect 组)。这里钉的都是「写错了不报错、只会永远不响」的东西:
      - 规则引用的指标名必须由代码注册(go-zero 的 Namespace / Subsystem / Name 三段,scene_manager 的 subsystem + Name);
      - reason 取值必须是 go/shared/gatepick/eligibility.go 的 Reason* 常量,login 两条规则合起来覆盖全部取值;
      - 两个计数都不预置 0 值序列,每条规则都要带「新序列」分支(unless … offset),否则第一次回落 increase() 看不见;
      - PrometheusRule 形状、每条规则有 severity / summary / description / for,告警名在 deploy/k8s 的告警文件里不重名。
    只读文本,不执行部署入口,不访问任何集群。规则语法由 promtool 另验(见 docs/PROGRESS.md 对应条目)。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/gate_pick_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

# 告警文件里某一组的文本:从 `    - name: <组名>` 到下一个组、组前注释或文件尾。
function Get-AlertGroupText {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Group)
    $m = [regex]::Match($Text, ('(?ms)^    - name:[ ]*{0}[ ]*\n(?<body>.*?)(?=^    - name:|^    #|\z)' -f [regex]::Escape($Group)))
    if (-not $m.Success) { throw "找不到告警组 $Group" }
    return $m.Groups['body'].Value
}

# 一组里的每条规则:名字与规则体(到下一条规则、规则前注释、组尾为止)。
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

function Assert-RuleShape {
    param([Parameter(Mandatory = $true)]$Rule, [Parameter(Mandatory = $true)][string]$Severity)
    Assert-Match -Text $Rule.Body -Pattern ('(?m)^            severity:[ ]*{0}[ ]*$' -f $Severity) -Because "$($Rule.Name) 的 severity 应为 $Severity"
    Assert-Match -Text $Rule.Body -Pattern '(?m)^            summary:' -Because "$($Rule.Name) 要有 summary"
    Assert-Match -Text $Rule.Body -Pattern '(?m)^            description:' -Because "$($Rule.Name) 要有 description"
    Assert-Match -Text $Rule.Body -Pattern '(?m)^            runbook_url:' -Because "$($Rule.Name) 要有 runbook_url"
    Assert-Match -Text $Rule.Body -Pattern '(?m)^          for:[ ]*\d+[ms][ ]*$' -Because "$($Rule.Name) 要写 for"
    $expr = Get-RuleExpr -Body $Rule.Body
    Assert-Match -Text $expr -Pattern '\bincrease\(' -Because "$($Rule.Name) 的第一支按 increase() 判"
    Assert-Match -Text $expr -Pattern '\bunless\b[^\n]*\boffset 10m\b' -Because "$($Rule.Name) 要带「新序列」分支:计数不预置 0 值序列,第一次回落 increase() 看不见"
}

# expr 里 <Metric>{...} 选择器中 reason 的字面量(| 分隔的逐个拆开)。
function Get-ReasonLiterals {
    param([Parameter(Mandatory = $true)][string]$Expr, [Parameter(Mandatory = $true)][string]$Metric)
    $literals = New-Object System.Collections.Generic.List[string]
    foreach ($sel in [regex]::Matches($Expr, ('\b{0}\{{(?<sel>[^}}]*)\}}' -f [regex]::Escape($Metric)))) {
        foreach ($m in [regex]::Matches($sel.Groups['sel'].Value, '\breason\s*(=~|=)\s*"(?<v>[^"]*)"')) {
            foreach ($v in ($m.Groups['v'].Value -split '\|')) { $literals.Add($v) }
        }
    }
    return @($literals | Sort-Object -Unique)
}

$script:LoginAlerts = Read-RepoText -RelativePath 'deploy/k8s/login-alerts.yaml'
$script:SceneManagerAlerts = Read-RepoText -RelativePath 'deploy/k8s/scene-manager-alerts.yaml'
$script:GatepickSource = Read-RepoText -RelativePath 'go/shared/gatepick/eligibility.go'
$script:Reasons = @([regex]::Matches($script:GatepickSource, '(?m)^\s*Reason[A-Za-z]+\s*=\s*"(?<v>[a-z]+)"') | ForEach-Object { $_.Groups['v'].Value } | Sort-Object -Unique)

Test-Case '夹具:gatepick 的 Reason* 常量是 draining / full / mixed / stale' {
    Assert-Equal -Expected 'draining,full,mixed,stale' -Actual ($script:Reasons -join ',') -Because 'reason 取值的权威来源;增删取值要同步两份告警与本用例'
}

Test-Case 'login-alerts.yaml:PrometheusRule 形状,login.gate-pick 组恰好两条规则,severity / summary / description / for / 新序列分支齐全' {
    Assert-Match -Text $script:LoginAlerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 owner-epoch-alerts.yaml 同一安装方式)'
    Assert-Match -Text $script:LoginAlerts -Pattern '(?m)^  name:[ ]*login-alerts[ ]*$' -Because 'metadata.name'
    Assert-Match -Text $script:LoginAlerts -Pattern '(?m)^    role:[ ]*alert-rules[ ]*$' -Because '与其余告警文件同一组选择标签'
    $group = Get-AlertGroupText -Text $script:LoginAlerts -Group 'login.gate-pick'
    Assert-Match -Text $group -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because '评估间隔'
    $rules = Get-AlertRules -GroupText $group
    Assert-Equal -Expected 'LoginGatePickFallbackDegraded,LoginGatePickFallbackStale' -Actual ((@($rules | ForEach-Object { $_.Name }) | Sort-Object) -join ',') -Because '规则名单钉死'
    foreach ($rule in $rules) {
        $severity = if ($rule.Name -eq 'LoginGatePickFallbackStale') { 'critical' } else { 'warning' }
        Assert-RuleShape -Rule $rule -Severity $severity
    }
}

Test-Case 'login-alerts.yaml:引用的指标由 loginqueue 的 go-zero 计数注册;reason 取值是 gatepick 常量,两条规则合起来覆盖全部取值' {
    $dir = Join-Path $script:RepoRoot 'go/login/internal/logic/pkg/loginqueue'
    $sources = @(Get-ChildItem -LiteralPath $dir -Filter '*.go' -File | Where-Object { $_.Name -notlike '*_test.go' } |
            ForEach-Object { [IO.File]::ReadAllText($_.FullName) })
    $registered = @($sources | ForEach-Object {
            [regex]::Matches($_, '(?s)Namespace:\s*"(?<ns>[a-z_]+)",\s*Subsystem:\s*"(?<sub>[a-z_]*)",\s*Name:\s*"(?<n>[a-z0-9_]+)"')
        } | ForEach-Object {
            if ($_.Groups['sub'].Value) { '{0}_{1}_{2}' -f $_.Groups['ns'].Value, $_.Groups['sub'].Value, $_.Groups['n'].Value }
            else { '{0}_{1}' -f $_.Groups['ns'].Value, $_.Groups['n'].Value }
        } | Sort-Object -Unique)
    Assert-True -Condition ($registered -ccontains 'login_gate_pick_fallback_total') -Because '夹具前提:gate_eligibility.go 注册 login_gate_pick_fallback_total(三段写法变了要同步本用例)'

    $covered = New-Object System.Collections.Generic.List[string]
    foreach ($rule in (Get-AlertRules -GroupText (Get-AlertGroupText -Text $script:LoginAlerts -Group 'login.gate-pick'))) {
        $expr = Get-RuleExpr -Body $rule.Body
        $referenced = @([regex]::Matches($expr, '\blogin_[a-z0-9_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
        Assert-Equal -Expected 'login_gate_pick_fallback_total' -Actual ($referenced -join ',') -Because "$($rule.Name) 只引用回落计数"
        foreach ($metric in $referenced) {
            Assert-True -Condition ($registered -ccontains $metric) -Because "$($rule.Name):$metric 必须由代码注册(改名后告警静默失效)"
        }
        $literals = @(Get-ReasonLiterals -Expr $expr -Metric 'login_gate_pick_fallback_total')
        Assert-True -Condition ($literals.Count -gt 0) -Because "$($rule.Name) 要按 reason 选择(两条规则分了 severity)"
        foreach ($literal in $literals) {
            Assert-True -Condition ($script:Reasons -ccontains $literal) -Because "$($rule.Name):reason=$literal 必须是 gatepick.Reason* 常量之一"
            $covered.Add($literal)
        }
        # 两支(increase 与新序列)要选同一批 reason,否则某个取值的第一次回落会漏。
        $selectors = @([regex]::Matches($expr, '\blogin_gate_pick_fallback_total\{(?<sel>[^}]*)\}') | ForEach-Object { $_.Groups['sel'].Value } | Sort-Object -Unique)
        Assert-Equal -Expected 1 -Actual $selectors.Count -Because "$($rule.Name) 的各个选择器要完全相同"
    }
    Assert-Equal -Expected ($script:Reasons -join ',') -Actual ((@($covered) | Sort-Object -Unique) -join ',') -Because '每个回落原因都要有一条规则管'
    Assert-Equal -Expected $covered.Count -Actual (@($covered | Sort-Object -Unique)).Count -Because '同一个原因不能被两条规则重复管(会双报)'
}

Test-Case 'scene-manager-alerts.yaml:scene-manager.gate-redirect 组的 SceneManagerGateRedirectFallback 形状齐全,指标由 metrics.go 注册' {
    $group = Get-AlertGroupText -Text $script:SceneManagerAlerts -Group 'scene-manager.gate-redirect'
    Assert-Match -Text $group -Pattern '(?m)^      interval:[ ]*30s[ ]*$' -Because '评估间隔'
    $rules = @(Get-AlertRules -GroupText $group)
    Assert-Equal -Expected 'SceneManagerGateRedirectFallback' -Actual ((@($rules | ForEach-Object { $_.Name }) | Sort-Object) -join ',') -Because '规则名单钉死'
    Assert-RuleShape -Rule $rules[0] -Severity 'warning'

    $source = Read-RepoText -RelativePath 'go/scene_manager/internal/metrics/metrics.go'
    $subsystem = [regex]::Match($source, '(?m)^const subsystem = "(?<s>[a-z_]+)"').Groups['s'].Value
    Assert-Equal -Expected 'scene_manager' -Actual $subsystem -Because '指标全名的前缀'
    $registered = @([regex]::Matches($source, '(?m)^\s*Name:\s*"(?<n>[a-z0-9_]+)"') | ForEach-Object { $subsystem + '_' + $_.Groups['n'].Value })
    $expr = Get-RuleExpr -Body $rules[0].Body
    $referenced = @([regex]::Matches($expr, '\bscene_manager_[a-z0-9_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'scene_manager_gate_redirect_fallback_total' -Actual ($referenced -join ',') -Because '只引用重定向回落计数'
    Assert-True -Condition ($registered -ccontains 'scene_manager_gate_redirect_fallback_total') -Because 'scene_manager_gate_redirect_fallback_total 必须由 metrics.go 注册(改名后告警静默失效)'
    Assert-Match -Text $expr -Pattern '\bsum by \(zone_id, reason\)' -Because '按目标 zone 与原因分开报'
    foreach ($literal in (Get-ReasonLiterals -Expr $expr -Metric 'scene_manager_gate_redirect_fallback_total')) {
        Assert-True -Condition ($script:Reasons -ccontains $literal) -Because "reason=$literal 必须是 gatepick.Reason* 常量之一"
    }
}

Test-Case '三条回落告警的名字在 deploy/k8s 的全部告警文件里各只出现一次' {
    $names = @('LoginGatePickFallbackStale', 'LoginGatePickFallbackDegraded', 'SceneManagerGateRedirectFallback')
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in $names) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'gate_pick_alerts_contract')
