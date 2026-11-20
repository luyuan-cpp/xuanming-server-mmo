#requires -Version 7
<#
.SYNOPSIS
    内容安全服务告警的契约(docs/design/rename-and-content-safety.md §6.4、§6.7)。

.DESCRIPTION
    deploy/k8s/content-guard-alerts.yaml 的规则读 go/content_guard/internal/metrics/metrics.go 定义的指标,按 verdict / reason 的
    取值筛选。写错指标名、标签名或取值都不会报错,只会让告警永远不响。这里逐项对照代码里的权威来源。
    只读文本,不执行部署入口,不访问集群;规则语法由 promtool 另验。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/content_guard_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

# 每条规则:名字与规则体(到下一条规则或文件尾为止)。
function Get-AlertRules {
    param([Parameter(Mandatory = $true)][string]$Text)
    return @([regex]::Matches($Text, '(?ms)^        - alert:[ ]*(?<name>\S+)[ ]*\n(?<body>.*?)(?=^        - alert:|\z)') |
            ForEach-Object { [pscustomobject]@{ Name = $_.Groups['name'].Value; Body = $_.Groups['body'].Value } })
}

function Get-RuleExpr {
    param([Parameter(Mandatory = $true)][string]$Body)
    $m = [regex]::Match($Body, '(?ms)^          expr: \|\n(?<e>.*?)(?=^          for:)')
    if (-not $m.Success) { throw '规则里找不到 `expr: |` 段(或它后面不是 for:)' }
    return $m.Groups['e'].Value
}

$script:Prefix = 'content_guard_'
$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/content-guard-alerts.yaml'
$script:Metrics = Read-RepoText -RelativePath 'go/content_guard/internal/metrics/metrics.go'
$script:Server = Read-RepoText -RelativePath 'go/content_guard/internal/server/server.go'
$script:Watcher = Read-RepoText -RelativePath 'go/content_guard/internal/lexstore/watcher.go'
$script:Rules = @(Get-AlertRules -Text $script:Alerts)

Write-Host ''
Write-Host '=== content_guard_alerts_contract ==='

Test-Case '规则名单:go.content-guard 组恰好五条,各有 severity / summary / description' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^    - name:[ ]*go\.content-guard[ ]*$' -Because '组名'
    $expected = 'ContentGuardNoLexiconAnywhere,ContentGuardReplicaWithoutLexicon,ContentGuardLexiconLoadFailing,ContentGuardLexiconVersionSkew,ContentGuardRefusingChecks'
    Assert-Equal -Expected $expected -Actual (($script:Rules | ForEach-Object Name) -join ',') -Because '规则名单'
    foreach ($rule in $script:Rules) {
        foreach ($key in @('severity', 'summary', 'description')) {
            Assert-Match -Text $rule.Body -Pattern ('(?m)^            {0}:' -f $key) -Because "$($rule.Name) 要有 $key"
        }
    }
}

Test-Case '式子里的指标都在 metrics.go 里定义;分组 label 只用代码里有的或抓取时附加的' {
    $namespaces = @([regex]::Matches($script:Metrics, 'Namespace:\s*"(?<n>[^"]+)"') | ForEach-Object { $_.Groups['n'].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'content_guard' -Actual ($namespaces -join ',') -Because '夹具前提:全部指标同一个命名空间,指标全名 = content_guard_ + Name'
    foreach ($rule in $script:Rules) {
        $expr = Get-RuleExpr -Body $rule.Body
        $names = @([regex]::Matches($expr, '\bcontent_guard_[a-z_]+\b') | ForEach-Object Value | Sort-Object -Unique)
        Assert-True -Condition ($names.Count -gt 0) -Because "$($rule.Name) 至少读一个 content_guard_ 指标"
        foreach ($name in $names) {
            $short = $name.Substring($script:Prefix.Length)
            Assert-Match -Text $script:Metrics -Pattern ('Name:\s*"{0}"' -f [regex]::Escape($short)) -Because "$($rule.Name) 读的 $name 要在 metrics.go 里定义"
        }
        foreach ($by in [regex]::Matches($expr, '\bby \((?<labels>[^)]*)\)')) {
            foreach ($label in @($by.Groups['labels'].Value -split ',' | ForEach-Object { $_.Trim() })) {
                Assert-True -Condition ($label -in @('namespace', 'pod', 'reason')) -Because "$($rule.Name) 只按 namespace / pod / reason 分组;$label 不是这组指标的 label"
            }
        }
    }
    Assert-Match -Text $script:Metrics -Pattern '\[\]string\{"scene", "verdict", "category"\}' -Because '夹具前提:判定计数的 label 是 scene / verdict / category'
    Assert-Match -Text $script:Metrics -Pattern '\[\]string\{"reason"\}' -Because '夹具前提:加载失败计数的 label 只有 reason'
}

Test-Case 'verdict 与 reason 的取值来自代码' {
    $refusing = @($script:Rules | Where-Object Name -ceq 'ContentGuardRefusingChecks')[0]
    Assert-Match -Text (Get-RuleExpr -Body $refusing.Body) -Pattern 'content_guard_checks_total\{verdict="unavailable"\}' -Because '拒绝判定的规则按 verdict=unavailable 筛'
    Assert-Match -Text $script:Server -Pattern 'metrics\.ObserveCheck\(sceneName, "unavailable", "none"' -Because 'server.go 在没有词库而拒绝判定时记的就是 verdict=unavailable'
    # 告警文案与文件头点名的失败原因,每一个都得是 watcher.go 里的常量取值;代码里的取值也都得在文件头列出。
    $codeReasons = @([regex]::Matches($script:Watcher, '(?m)^\s*Failure[A-Za-z]+\s*=\s*"(?<r>[a-z_]+)"') | ForEach-Object { $_.Groups['r'].Value } | Sort-Object)
    Assert-Equal -Expected 'apply,corrupt,etcd,watch' -Actual ($codeReasons -join ',') -Because '夹具前提:lexstore 的加载失败原因是这四个'
    $header = [regex]::Match($script:Alerts, '(?m)^#\s+- content_guard_lexicon_load_failures_total\{reason\}:.*reason ∈ (?<list>[a-z_ /]+);')
    Assert-True -Condition $header.Success -Because '文件头要列出 reason 的全部取值'
    $documented = @($header.Groups['list'].Value -split '/' | ForEach-Object { $_.Trim() } | Where-Object { $_ } | Sort-Object)
    Assert-Equal -Expected ($codeReasons -join ',') -Actual ($documented -join ',') -Because '文件头列的 reason 取值与代码一致'
}

Test-Case '按副本的规则与全局的规则各管各的:版本为 0 的两条一条按 namespace、一条按 Pod' {
    $anywhere = @($script:Rules | Where-Object Name -ceq 'ContentGuardNoLexiconAnywhere')[0]
    $replica = @($script:Rules | Where-Object Name -ceq 'ContentGuardReplicaWithoutLexicon')[0]
    Assert-Match -Text (Get-RuleExpr -Body $anywhere.Body) -Pattern 'max by \(namespace\) \(content_guard_lexicon_version\) == 0' -Because '全部副本都没有词库:取最大值仍为 0'
    Assert-Match -Text (Get-RuleExpr -Body $replica.Body) -Pattern 'max by \(namespace, pod\) \(content_guard_lexicon_version\) == 0' -Because '单个副本没有词库'
    Assert-Match -Text $anywhere.Body -Pattern '(?m)^            severity: critical\s*$' -Because '全部副本没有词库 = 名字与聊天整体不可用'
    Assert-Match -Text $replica.Body -Pattern '(?m)^            severity: warning\s*$' -Because '单个副本没有词库只是少一份容量'
}

Test-Case '告警名在 deploy/k8s 的全部告警文件里各只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($rule in $script:Rules) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $rule.Name })).Count -Because "$($rule.Name) 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'content_guard_alerts_contract')
