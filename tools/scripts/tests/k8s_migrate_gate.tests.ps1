#requires -Version 7
<#
.SYNOPSIS
    迁移 Job 的发布门禁、删除安全与有界查询回归测试。
.DESCRIPTION
    只从 k8s_deploy.ps1 的 AST 提取函数，不执行脚本入口。
    kubectl 调用均替换为内存模拟，进程截止用临时 pwsh 夹具验证；不访问集群、服务或数据库。
.EXAMPLE
    pwsh -File tools/scripts/tests/k8s_migrate_gate.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$script:DeployScript = Join-Path $script:RepoRoot 'tools/scripts/k8s_deploy.ps1'
$parseTokens = $null
$parseErrors = $null
$deployAst = [Management.Automation.Language.Parser]::ParseFile($script:DeployScript, [ref]$parseTokens, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "部署脚本语法错误: $($parseErrors.Message -join '; ')" }
$script:ProductionFunctions = @{}
foreach ($node in $deployAst.FindAll({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    $script:ProductionFunctions[$node.Name] = $node.Body.GetScriptBlock()
}

function Restore-ProductionFunction {
    param([string]$Name)
    if (-not $script:ProductionFunctions.ContainsKey($Name)) { throw "部署脚本缺少函数 $Name" }
    Set-Item -Path "Function:script:$Name" -Value $script:ProductionFunctions[$Name]
}

function New-MockResult {
    param([string]$Output = '', [int]$ExitCode = 0, [bool]$TimedOut = $false)
    [pscustomobject]@{ ExitCode = $ExitCode; Output = $Output; ErrorOutput = ''; TimedOut = $TimedOut }
}

function New-MockJob {
    param([string]$Condition = 'Complete', [string]$Uid = 'current-job-uid')
    @{ metadata = @{ name = 'trade-migrate'; uid = $Uid }; status = @{
        active = 0; conditions = @(@{ type = $Condition; status = 'True' })
    } } | ConvertTo-Json -Depth 8 -Compress
}

function New-MockPod {
    param([string]$Phase, [string]$Uid = 'current-job-uid', [bool]$Terminating = $false)
    $metadata = @{ name = 'trade-migrate-pod'; ownerReferences = @(@{ kind = 'Job'; name = 'trade-migrate'; uid = $Uid; controller = $true }) }
    if ($Terminating) { $metadata.deletionTimestamp = '2026-09-16T00:00:00Z' }
    @{ metadata = $metadata; status = @{ phase = $Phase } }
}

function Reset-MigrateFixture {
    foreach ($name in @('Apply-OneGoSvc', 'Apply-GoSvcMigrateJob', 'Get-GoSvcMigrateJobState',
        'Wait-GoSvcMigrateJobSettled', 'Assert-GoSvcMigrateJobNotInFlight', 'Wait-ForGoSvcMigrateJob',
        'Write-GoSvcMigrateJobDiagnostics')) {
        Restore-ProductionFunction $name
    }
    $script:Calls = [Collections.Generic.List[object]]::new()
    $script:Events = [Collections.Generic.List[string]]::new()
    $script:JobJson = New-MockJob
    $script:PodsJson = @{ items = @() } | ConvertTo-Json -Compress
    $script:PodsExitCode = 0
    $script:JobExitCode = 0
    $script:JobApplied = $false
    $script:JobAbsentBeforeApply = $false
    $script:AfterApplyCondition = 'Complete'
    $script:QueryDelayMs = 0
    $script:BudgetSeconds = 1
    $script:WaitTimeoutSeconds = 1
    $script:GoSvcMigrateJobMinWaitSeconds = 0
    $script:ReleaseProfile = 'prod'
    $script:WaitReady = $false
    $script:DryRun = $false
    $script:InfraNamespace = 'audit-infra'
    $script:GoSvcRegistry = 'registry.invalid/audit'
    $script:GoSvcTag = 'test'
    $script:GoSvcManifestsDir = Join-Path $script:RepoRoot 'deploy/k8s/manifests/go-svc'
    $script:GoSvcCatalogue = @{ trade = @{
        ConfigMap = 'go-svc-trade-config'; Manifest = 'trade.yaml'; MigrateJob = 'trade-migrate.yaml'
        ConfigFile = 'trade.yaml'; ImageName = 'mmorpg-trade'; Port = 50800; Global = $true
    } }

    Set-Item Function:script:Get-GoSvcMigrateJobWaitSeconds { return $script:BudgetSeconds }
    Set-Item Function:script:New-GoSvcConfigMapYaml {
        param($SvcName, $CurrentZoneId, $CurrentClusterId)
        return "kind: ConfigMap`nmetadata:`n  name: go-svc-trade-config"
    }
    Set-Item Function:script:Resolve-ImagePullPolicy { param($ImageRef) return 'IfNotPresent' }
    Set-Item Function:script:Wait-ForDeploymentReady {
        param($Namespace, $DeploymentName)
        $script:Events.Add("ready:$DeploymentName")
    }
    Set-Item Function:script:Invoke-KubectlWithInputFile {
        [CmdletBinding()]
        param([string[]]$Args, [string]$InputContent)
        if ($InputContent -match '(?m)^kind: Job\s*$') {
            $script:Events.Add('apply:Job')
            $script:JobApplied = $true
        } elseif ($InputContent -match '(?m)^kind: Deployment\s*$') {
            $script:Events.Add('apply:Deployment')
        } elseif ($InputContent -match '(?m)^kind: ConfigMap\s*$') {
            $script:Events.Add('apply:ConfigMap')
        } else { throw '未识别的模拟 apply 内容' }
    }
    Set-Item Function:script:Invoke-Kubectl {
        [CmdletBinding()]
        param([string[]]$Args, [switch]$AllowFailure)
        if ($Args[0] -eq 'delete') { $script:Events.Add('delete:Job'); return }
        throw "迁移查询或诊断走了无界 kubectl 调用面: $($Args -join ' ')"
    }
    # 最后一层防护:意外绕过封装也只能失败，不会连接真实集群。
    Set-Item Function:script:kubectl { throw '测试禁止执行真实 kubectl' }
    Set-Item Function:script:Invoke-GoSvcMigrateKubectl {
        [CmdletBinding()]
        param([string[]]$Args, [double]$TimeoutSeconds = 10)
        $script:Calls.Add([pscustomobject]@{ Args = @($Args); TimeoutSeconds = $TimeoutSeconds })
        if ($script:QueryDelayMs -gt 0) { Microsoft.PowerShell.Utility\Start-Sleep -Milliseconds $script:QueryDelayMs }
        if ($Args[0] -eq 'get' -and $Args[1] -eq 'job') {
            $script:Events.Add('query:Job')
            if ($script:JobAbsentBeforeApply -and -not $script:JobApplied) { return New-MockResult }
            if ($script:JobApplied) { return New-MockResult -Output (New-MockJob -Condition $script:AfterApplyCondition) }
            return New-MockResult -Output $script:JobJson -ExitCode $script:JobExitCode
        }
        if ($Args[0] -eq 'get' -and $Args[1] -eq 'pods' -and $Args -contains 'json') {
            $script:Events.Add('query:Pods')
            return New-MockResult -Output $script:PodsJson -ExitCode $script:PodsExitCode
        }
        $script:Events.Add('diagnostic')
        return New-MockResult -ExitCode 1
    }
}

function Get-ThrownMessage {
    param([scriptblock]$Action)
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

Write-Host '=== 迁移 Job 门禁回归（完全模拟 kubectl） ==='

foreach ($profile in @('staging', 'prod')) {
    Test-Case "$profile 不带 WaitReady，失败 Job 仍阻止 Deployment" {
        Reset-MigrateFixture
        $script:ReleaseProfile = $profile
        $script:JobAbsentBeforeApply = $true
        $script:AfterApplyCondition = 'Failed'
        $failure = Get-ThrownMessage { Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0 }
        Assert-Match -Text $failure -Pattern 'trade-migrate.*failed' -Because '必须报告迁移失败，而不是其它模拟或参数错误'
        Assert-True -Condition ($script:Events -contains 'apply:Job') -Because '负向用例必须真正走到新 Job 的 apply'
        Assert-True -Condition ($script:Events -notcontains 'apply:Deployment') -Because '失败后不得部署服务'
    }
}

Test-Case '成功 Job 才放行 Deployment，且 ConfigMap 与 Job 的顺序正确' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:ConfigMap') -lt [Array]::IndexOf($events, 'apply:Job')) -Because 'Job 要读取先前创建的 ConfigMap'
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because '迁移必须先于服务部署'
    Assert-True -Condition ($events -contains 'query:Job') -Because '成功必须来自 Job 查询，不能直接放行'
}

foreach ($condition in @('Complete', 'Failed', 'FailureTarget')) {
    foreach ($phase in @('Running', 'Unknown', 'Pending')) {
        Test-Case "$condition 且同 UID Pod=$phase 时禁止删除旧 Job" {
            Reset-MigrateFixture
            $script:ReleaseProfile = 'dev'
            $script:JobJson = New-MockJob -Condition $condition
            $script:PodsJson = @{ items = @(New-MockPod -Phase $phase -Terminating $true) } | ConvertTo-Json -Depth 8 -Compress
            $state = Get-GoSvcMigrateJobState -Namespace audit-infra -JobName trade-migrate -TimeoutSeconds 1 -RequirePodsTerminal
            Assert-Equal -Expected running -Actual $state -Because '同 UID 的非终态 Pod 必须被实际查出，不能凭 Job 条件放行'
            # 让第一轮查询耗尽小预算，避免测试真的等生产环境的五分钟。
            $script:BudgetSeconds = 0.001
            $script:QueryDelayMs = 2
            $failure = Get-ThrownMessage { Apply-GoSvcMigrateJob -SvcName trade -Namespace audit-infra -SvcImage audit/trade:test -PullPolicy IfNotPresent }
            Assert-Match -Text $failure -Pattern 'trade-migrate.*(未结束|在途|未删除|未安全结束)' -Because '应保留旧 Job 并明确中断原因'
            Assert-True -Condition ($script:Events -notcontains 'delete:Job') -Because 'Job 条件和 active=0 不能证明 Pod 已退出'
            Assert-True -Condition ($script:Events -notcontains 'apply:Job') -Because '旧 Pod 未终止时也不能创建替代 Job'
        }
    }
}

Test-Case 'Pod 查询失败时删除检查 fail-closed' {
    Reset-MigrateFixture
    $script:PodsExitCode = 1
    $state = Get-GoSvcMigrateJobState -Namespace audit-infra -JobName trade-migrate -TimeoutSeconds 1 -RequirePodsTerminal
    Assert-Equal -Expected unknown -Actual $state -Because '查不到 Pod 不能宣称旧迁移已结束'
    $script:ReleaseProfile = 'dev'
    $script:BudgetSeconds = 0.001
    $script:QueryDelayMs = 2
    $failure = Get-ThrownMessage { Apply-GoSvcMigrateJob -SvcName trade -Namespace audit-infra -SvcImage audit/trade:test -PullPolicy IfNotPresent }
    Assert-Match -Text $failure -Pattern 'trade-migrate.*(未结束|在途|未删除|未安全结束)' -Because '无法确认 Pod 退出时须中断发布'
    Assert-True -Condition ($script:Events -notcontains 'delete:Job') -Because 'Pod 查询失败不能变成删除许可'
}

Test-Case 'Pod JSON 无效时删除检查 fail-closed' {
    Reset-MigrateFixture
    $script:PodsJson = '{invalid'
    $state = Get-GoSvcMigrateJobState -Namespace audit-infra -JobName trade-migrate -TimeoutSeconds 1 -RequirePodsTerminal
    Assert-Equal -Expected unknown -Actual $state -Because '解析失败不能变成安全终态'
}

Test-Case '同名旧 UID 的 Running Pod 不属于当前 Job' {
    Reset-MigrateFixture
    $script:PodsJson = @{ items = @(
        (New-MockPod -Phase Running -Uid 'previous-job-uid'),
        (New-MockPod -Phase Succeeded)
    ) } | ConvertTo-Json -Depth 8 -Compress
    $state = Get-GoSvcMigrateJobState -Namespace audit-infra -JobName trade-migrate -TimeoutSeconds 1 -RequirePodsTerminal
    Assert-Equal -Expected complete -Actual $state -Because '关联必须按 owner UID，不能只比较 job-name 标签'
}

Test-Case '同 UID 但 owner kind 不是 Job 的 Pod 不误关联' {
    Reset-MigrateFixture
    $pod = New-MockPod -Phase Running
    $pod.metadata.ownerReferences[0].kind = 'ReplicaSet'
    $script:PodsJson = @{ items = @($pod) } | ConvertTo-Json -Depth 8 -Compress
    $state = Get-GoSvcMigrateJobState -Namespace audit-infra -JobName trade-migrate -TimeoutSeconds 1 -RequirePodsTerminal
    Assert-Equal -Expected complete -Actual $state -Because 'owner 必须同时匹配 Job kind 与 UID'
}

Test-Case '同 UID Pod 全部退出后允许删除重建' {
    Reset-MigrateFixture
    $script:ReleaseProfile = 'dev'
    $script:PodsJson = @{ items = @((New-MockPod -Phase Succeeded), (New-MockPod -Phase Failed)) } | ConvertTo-Json -Depth 8 -Compress
    Apply-GoSvcMigrateJob -SvcName trade -Namespace audit-infra -SvcImage audit/trade:test -PullPolicy IfNotPresent
    Assert-True -Condition ($script:Events -contains 'delete:Job') -Because '终止检查不能永久禁止正常的迁移重跑'
    Assert-True -Condition ($script:Events -contains 'apply:Job') -Because '安全删除后应创建新 Job'
}

Test-Case '失败发布快速结束，不必等终止中的 Pod' {
    Reset-MigrateFixture
    $script:JobJson = New-MockJob -Condition FailureTarget
    $script:PodsJson = @{ items = @(New-MockPod -Phase Running -Terminating $true) } | ConvertTo-Json -Depth 8 -Compress
    $state = Get-GoSvcMigrateJobState -Namespace audit-infra -JobName trade-migrate -TimeoutSeconds 1
    Assert-Equal -Expected failed -Actual $state -Because '发布失败判定与允许删除旧 Job 是两种不同门禁'
}

Test-Case 'Job 和 Pod 共享一次查询的剩余预算' {
    Reset-MigrateFixture
    $script:QueryDelayMs = 20
    Get-GoSvcMigrateJobState -Namespace audit-infra -JobName trade-migrate -TimeoutSeconds 0.2 -RequirePodsTerminal | Out-Null
    Assert-Equal -Expected 2 -Actual $script:Calls.Count -Because '本用例应覆盖 Job 与 Pod 两次查询'
    Assert-True -Condition ($script:Calls[0].TimeoutSeconds -le 0.2 -and $script:Calls[0].TimeoutSeconds -gt 0) -Because '第一条查询不能超出调用预算'
    Assert-True -Condition ($script:Calls[1].TimeoutSeconds -lt $script:Calls[0].TimeoutSeconds -and $script:Calls[1].TimeoutSeconds -gt 0) -Because '第二条查询必须扣除第一条已耗时间'
}

Test-Case '轮询每条查询不超过剩余预算，预算耗尽后不再发请求' {
    Reset-MigrateFixture
    $script:JobJson = New-MockJob -Condition Running
    $script:BudgetSeconds = 0.02
    $script:QueryDelayMs = 25
    $state = Wait-GoSvcMigrateJobSettled -Namespace audit-infra -JobName trade-migrate
    Assert-True -Condition ($state -in @('running', 'unknown')) -Because '耗尽预算仍未完成，应返回未决状态'
    Assert-Equal -Expected 1 -Actual $script:Calls.Count -Because '第一次请求已耗尽预算，不能再 sleep 后追加请求'
    Assert-True -Condition ($script:Calls[0].TimeoutSeconds -gt 0 -and $script:Calls[0].TimeoutSeconds -le 0.02) -Because '小于十秒的剩余预算必须继续传到进程封装'
}

Test-Case '诊断调用全部有正数上限，诊断失败不抛出新故障' {
    Reset-MigrateFixture
    Write-GoSvcMigrateJobDiagnostics -Namespace audit-infra -JobName trade-migrate
    Assert-True -Condition ($script:Calls.Count -gt 0) -Because '应实际尝试输出诊断'
    foreach ($call in $script:Calls) {
        Assert-True -Condition ($call.TimeoutSeconds -gt 0 -and $call.TimeoutSeconds -le 10) -Because '每项诊断都必须有界'
    }
}

Test-Case '诊断意外抛错仍保留发布失败的原始原因' {
    Reset-MigrateFixture
    $script:JobJson = New-MockJob -Condition Failed
    Set-Item Function:script:Write-GoSvcMigrateJobDiagnostics { param($Namespace, $JobName) throw 'DIAGNOSTIC_BROKEN' }
    $failure = Get-ThrownMessage { Wait-ForGoSvcMigrateJob -Namespace audit-infra -JobName trade-migrate -SvcName trade }
    Assert-Match -Text $failure -Pattern 'trade-migrate.*failed' -Because '最终错误必须仍然点名失败的迁移 Job'
    Assert-NotMatch -Text $failure -Pattern '^DIAGNOSTIC_BROKEN$' -Because '诊断是尽力输出，不应覆盖发布失败'
}

# 最后一项只运行临时 pwsh 夹具来模拟阻塞的 CLI,不查找或执行真实 kubectl。
Test-Case '查询进程保留参数边界并在 CLI 阻塞时硬截止' {
    Reset-MigrateFixture
    Restore-ProductionFunction 'Invoke-GoSvcMigrateKubectl'
    $script:FixturePwshPath = [Environment]::ProcessPath
    $fixture = Join-Path ([IO.Path]::GetTempPath()) ('d14-kubectl-fixture-' + [guid]::NewGuid().ToString('N') + '.ps1')
    [IO.File]::WriteAllText($fixture, @'
if ($args[0] -eq 'sleep') { Start-Sleep -Seconds 30 }
ConvertTo-Json -InputObject @($args) -Compress
[Console]::Error.WriteLine('fixture-stderr')
exit 7
'@, [Text.UTF8Encoding]::new($false))
    Set-Item Function:script:Build-KubectlBaseArgs { return ,@() }
    Set-Item Function:script:Get-Command {
        [CmdletBinding()]
        param($Name, $CommandType)
        if ($Name -ne 'kubectl') { throw '夹具只允许替代 kubectl 查询' }
        [pscustomobject]@{ Source = $script:FixturePwshPath }
    }
    try {
        $result = Invoke-GoSvcMigrateKubectl -Args @('-NoProfile', '-File', $fixture, 'value with spaces', 'quote"inside') -TimeoutSeconds 10
        Assert-Equal -Expected 7 -Actual $result.ExitCode -Because '正常退出必须保留 CLI 自身退出码'
        $seen = $result.Output | ConvertFrom-Json
        Assert-Equal -Expected 'value with spaces' -Actual $seen[0] -Because '不能把一个带空格参数拼成多个参数'
        Assert-Equal -Expected 'quote"inside' -Actual $seen[1] -Because '参数内引号必须逐字传递'
        Assert-Equal -Expected '--request-timeout=10000ms' -Actual $seen[2] -Because '每次请求必须带有界 HTTP 超时'
        Assert-Match -Text $result.ErrorOutput -Pattern 'fixture-stderr' -Because 'stderr 应当独立捕获'

        $elapsed = [Diagnostics.Stopwatch]::StartNew()
        $result = Invoke-GoSvcMigrateKubectl -Args @('-NoProfile', '-File', $fixture, 'sleep') -TimeoutSeconds 0.2
        Assert-True -Condition $result.TimedOut -Because 'CLI 阻塞必须被整个进程的截止打断'
        Assert-True -Condition ($result.ExitCode -ne 0) -Because '超时不能伪装成功'
        Assert-True -Condition ($elapsed.Elapsed.TotalSeconds -lt 5) -Because '30s 阻塞夹具不得突破有界清理时间'
    } finally {
        Remove-Item Function:script:Get-Command -ErrorAction SilentlyContinue
        Restore-ProductionFunction 'Build-KubectlBaseArgs'
        Remove-Item -LiteralPath $fixture -Force
    }
}

exit (Complete-TestRun -SuiteName 'k8s_migrate_gate')
