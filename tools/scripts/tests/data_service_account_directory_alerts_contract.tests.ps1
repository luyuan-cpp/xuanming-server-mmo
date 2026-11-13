<#
.SYNOPSIS
    账号目录告警与 data_service 源码的契约(docs/design/guild-phase2/03-names.md §3.4a、horizontal-scale-logical-db.md §5.2)。

.DESCRIPTION
    deploy/k8s/data-service-account-directory-alerts.yaml 的规则只在序列存在时才可能响,写错指标名、label 名或取值不会报错,只会永远不响。
    这里逐项对照 go/data_service 的源码:
      - 规则引用的每个指标都由 internal/metrics/account_directory.go 注册(Subsystem data_service + Name),按 label 聚合的 reason / step / result
        是那个指标声明的 label,result 的取值是那里的常量;
      - 计数序列在装配时预置(svc 调 metrics.PrimeAccountDirectory,取值来自 store 的封闭集合),第一次失败也能被 increase 看到;
      - 告警 description 让值班去日志里找的原文,确实是 data_service 打出的;
      - PrometheusRule 形状、severity / summary / description / for,四条告警名在 deploy/k8s 的告警文件里不重名。
    只读文本,不执行部署入口,不访问集群。规则语法由 promtool 另验。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/data_service_account_directory_alerts_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return ([IO.File]::ReadAllText((Join-Path $script:RepoRoot $RelativePath))) -replace "`r`n", "`n"
}

$script:Alerts = Read-RepoText -RelativePath 'deploy/k8s/data-service-account-directory-alerts.yaml'
$script:Metrics = Read-RepoText -RelativePath 'go/data_service/internal/metrics/account_directory.go'
$script:Events = Read-RepoText -RelativePath 'go/data_service/internal/svc/directory_events.go'
$script:RoleFence = Read-RepoText -RelativePath 'go/data_service/internal/svc/role_fence.go'
$script:StoreEvents = Read-RepoText -RelativePath 'go/data_service/internal/store/account_directory_events.go'
$script:RoleName = Read-RepoText -RelativePath 'go/data_service/internal/store/account_role_name.go'
$script:RoleRef = Read-RepoText -RelativePath 'go/data_service/internal/store/account_role_ref.go'
$script:AlertNames = @('DataServiceRoleNameBindFailed', 'DataServiceRemovedRoleNameReleaseFailed', 'DataServiceRoleRemovalResumeStuck', 'DataServiceRoleRefHeldForReview')

Write-Host '=== data_service_account_directory_alerts_contract ==='

Test-Case 'PrometheusRule 形状:kind、四条告警、都是 warning、summary / description 齐全;一次性失败 for 1m,后台持续失败 for 15m' {
    Assert-Match -Text $script:Alerts -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 owner-epoch-alerts.yaml 同一安装方式)'
    foreach ($name in $script:AlertNames) {
        Assert-Match -Text $script:Alerts -Pattern ('(?m)^\s+- alert:[ ]*{0}[ ]*$' -f $name) -Because "规则名 $name 固定,值班手册按名字查"
    }
    Assert-Equal -Expected 4 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+- alert:').Count) -Because '规则名单钉死:加规则要同步本用例'
    Assert-Equal -Expected 4 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+severity:[ ]*warning[ ]*$').Count) -Because '这几步失败都不改业务结局,都是 warning'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+for:[ ]*1m[ ]*$').Count) -Because '绑定失败、删角释放失败是一次性的事件,出现即报'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+for:[ ]*15m[ ]*$').Count) -Because '后台每分钟重试的(收尾、保留 ref)持续 15 分钟才报'
    Assert-Equal -Expected 4 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+summary:').Count) -Because '每条告警要有 summary'
    Assert-Equal -Expected 4 -Actual ([regex]::Matches($script:Alerts, '(?m)^\s+description:').Count) -Because '每条告警要有 description'
}

Test-Case '规则引用的指标都由 internal/metrics/account_directory.go 注册,按 label 聚合的 label 是指标声明的 label' {
    Assert-Match -Text $script:Metrics -Pattern 'Subsystem:\s*subsystem' -Because '指标名前缀来自 Subsystem(data_service)'
    $exprs = @([regex]::Matches($script:Alerts, '(?ms)expr:\s*\|\s*\n\s*(?<e>[^\n]+)') | ForEach-Object { $_.Groups['e'].Value })
    Assert-Equal -Expected 4 -Actual $exprs.Count -Because '每条规则一行表达式'
    $wanted = @{
        'data_service_account_role_name_bind_failures_total'     = 'reason'
        'data_service_account_role_removal_settle_failures_total' = 'step'
        'data_service_account_role_removal_resume_total'          = 'result'
        'data_service_account_role_ref_held'                      = ''
    }
    $used = @($exprs | ForEach-Object { [regex]::Matches($_, 'data_service_[a-z_]+') } | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-Equal -Expected (($wanted.Keys | Sort-Object) -join ',') -Actual ($used -join ',') -Because '规则只引用这四个指标'
    foreach ($metric in $wanted.Keys) {
        $suffix = $metric.Substring('data_service_'.Length)
        Assert-Match -Text $script:Metrics -Pattern ('Name:\s*"{0}"' -f [regex]::Escape($suffix)) -Because "$metric 必须由 account_directory.go 注册,否则规则永远不响"
        $label = $wanted[$metric]
        if ($label -ne '') {
            Assert-Match -Text $script:Metrics -Pattern ('(?s)Name:\s*"{0}".*?\}},\s*\[\]string\{{"{1}"\}}\)' -f [regex]::Escape($suffix), $label) -Because "$metric 的 label 是 $label"
        }
    }
    foreach ($expr in $exprs) {
        $by = [regex]::Match($expr, 'by \((?<l>[^)]*)\)').Groups['l'].Value
        Assert-Match -Text $by -Pattern '\bnamespace\b' -Because '按 namespace 分组(data_service 一个 zone 一个 namespace)'
    }
}

Test-Case 'result 的取值是 account_directory.go 的常量;计数序列在装配时按 store 的封闭集合预置' {
    $values = @([regex]::Matches($script:Alerts, 'result=~"(?<v>[^"]+)"') | ForEach-Object { $_.Groups['v'].Value -split '\|' } | Sort-Object -Unique)
    Assert-Equal -Expected 'failed,unsettled' -Actual ($values -join ',') -Because '后台收尾告警只看没做完的两种结局'
    foreach ($value in $values) {
        Assert-Match -Text $script:Metrics -Pattern ('RoleRemovalResume\w+\s*=\s*"{0}"' -f [regex]::Escape($value)) -Because "result=`"$value`" 必须是 account_directory.go 的常量"
    }
    Assert-Match -Text $script:Events -Pattern 'metrics\.PrimeAccountDirectory\(store\.NameBindFailureReasons\(\),\s*store\.RemovalSettleSteps\(\)\)' -Because '装配时预置,第一次失败也能被 increase 看到'
    Assert-Match -Text $script:StoreEvents -Pattern 'func NameBindFailureReasons\(\) \[\]string' -Because 'reason 的封闭集合在 store'
    Assert-Match -Text $script:StoreEvents -Pattern 'func RemovalSettleSteps\(\) \[\]string' -Because 'step 的封闭集合在 store'
    Assert-Match -Text $script:RoleFence -Pattern 'metrics\.ObserveRoleRemovalResume\(result\.Finished, result\.Abandoned, result\.Unsettled, result\.Failed\)' -Because '后台收尾每一轮都计'
    Assert-Match -Text $script:RoleFence -Pattern 'metrics\.SetRoleRefHeld\(result\.HeldTotal\)' -Because '保留 ref 的 gauge 由后台判陈旧每轮写'
}

Test-Case 'description 让值班去找的日志原文,确实是 data_service 打出的' {
    $logs = @(
        @{ Text = '[player-name] name of a committed role is not bound'; Source = $script:RoleName; File = 'store/account_role_name.go' }
        @{ Text = '[player-name] name of a removed role is not released yet'; Source = $script:RoleName; File = 'store/account_role_name.go' }
        @{ Text = '[account-role-removal] removal row of a settled role not dropped'; Source = $script:RoleName; File = 'store/account_role_name.go' }
        @{ Text = '[role-fence] resume removals'; Source = $script:RoleFence; File = 'svc/role_fence.go' }
        @{ Text = '[account-role-ref] stale create ref kept without a tombstone'; Source = $script:RoleRef; File = 'store/account_role_ref.go' }
    )
    foreach ($log in $logs) {
        Assert-True -Condition ($script:Alerts.Contains($log.Text)) -Because "告警文案引用 `"$($log.Text)`""
        Assert-True -Condition ($log.Source.Contains($log.Text)) -Because "`"$($log.Text)`" 必须是 $($log.File) 打出的原文,否则值班照着搜不到"
    }
    Assert-True -Condition ($script:Alerts.Contains('held_ms>0')) -Because '保留 ref 的查法按 held_ms 列'
    Assert-True -Condition ($script:RoleRef.Contains('held_ms>0')) -Because 'held_ms 是 account_role_ref 的列(SQL 里按它筛)'
}

Test-Case '告警名在 deploy/k8s 的全部告警文件里只出现一次' {
    $all = @(Get-ChildItem -LiteralPath (Join-Path $script:RepoRoot 'deploy/k8s') -Filter '*-alerts.yaml' -File | ForEach-Object {
            ([IO.File]::ReadAllText($_.FullName)) -replace "`r`n", "`n"
        } | ForEach-Object { [regex]::Matches($_, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') } | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in $script:AlertNames) {
        Assert-Equal -Expected 1 -Actual (@($all | Where-Object { $_ -ceq $name })).Count -Because "$name 在 Alertmanager 里按 alertname 分组,重名会互相掩盖"
    }
}

exit (Complete-TestRun -SuiteName 'data_service_account_directory_alerts_contract')
