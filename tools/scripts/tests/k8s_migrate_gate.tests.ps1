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
        'Write-GoSvcMigrateJobDiagnostics', 'Add-GoSvcEnvEntries', 'Add-GoSvcCommandTopicEnv', 'Add-GoSvcTopicReplicationEnv')) {
        Restore-ProductionFunction $name
    }
    # Add-GoSvcEnvEntries:Add-GoSvcCommandTopicEnv / Add-GoSvcSecretEnv 共用的 env 注入助手,由 k8s_deploy 属主从两者中抽出。
    # 抽出之前部署脚本里还没有它,所以按"存在才恢复"处理;抽出之后调用方依赖它,若它缺席或改名,
    # 调用处会报"命令未找到",Add-GoSvcCommandTopicEnv 与 Apply-OneGoSvc 的用例随之变红,不会静默通过。
    if ($script:ProductionFunctions.ContainsKey('Add-GoSvcEnvEntries')) {
        Restore-ProductionFunction 'Add-GoSvcEnvEntries'
    }
    $script:DeploymentContent = $null
    $script:MigrationContent = $null
    $script:CommandTopicContractError = $null
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
    # friend 与 trade 一样是带 MigrateJob 的建表服务（port-decisions D-14），门禁走同一条链路。
    # 单独一条赋值而不是并进上面的字面量：两边都只做追加时，3-way 合并的冲突面最小。
    $script:GoSvcCatalogue['friend'] = @{
        ConfigMap = 'go-svc-friend-config'; Manifest = 'friend.yaml'; MigrateJob = 'friend-migrate.yaml'
        ConfigFile = 'friend.yaml'; ImageName = 'mmorpg-friend'; Port = 50400; Global = $true
    }

    # data-service 是第三个带 MigrateJob 的服务,也是第一个**不是全局池**的(每个 zone 一份,迁的是同一个全局库)。
    $script:GoSvcCatalogue['data-service'] = @{
        ConfigMap = 'go-svc-data-service-config'; Manifest = 'data-service.yaml'; MigrateJob = 'data-service-migrate.yaml'
        ConfigFile = 'data_service.yaml'; ImageName = 'mmorpg-data-service'; Port = 9000
    }

    # player-inbox(玩家收件箱,D-14 独占库 mmorpg_player_inbox)是第四个带 MigrateJob 的服务,另带运维口令的 Secret 注入
    # (docs/design/player-inbox-service.md §1.6)。口令相关的脚本级变量在 k8s_deploy.ps1 顶层赋值,AST 只抽函数,这里照抄一份;
    # 默认未注入(= dev 档没给 MMORPG_PLAYER_INBOX_ADMIN_TOKEN)。Apply-InjectedSecret 换成只记事件的桩,不碰集群。
    $script:GoSvcCatalogue['player-inbox'] = @{
        ConfigMap = 'go-svc-player-inbox-config'; Manifest = 'player-inbox.yaml'; MigrateJob = 'player-inbox-migrate.yaml'
        ConfigFile = 'player_inbox.yaml'; ImageName = 'mmorpg-player-inbox'; Port = 50950; Global = $true
    }
    $script:PlayerInboxAdminTokenSecretName = 'player-inbox-admin-token'
    $script:PlayerInboxAdminTokenSecretKey = 'admin-token'
    $script:PlayerInboxAdminTokenEnv = 'PLAYER_INBOX_ADMIN_TOKEN'
    $script:PlayerInboxAdminToken = ''
    # mail(邮件,D-14 独占库 mmorpg_mail)是第五个带 MigrateJob 的服务,另带运维令牌的 Secret 注入(docs/design/mail-system.md §7.2,
    # 照 player-inbox)。令牌相关的脚本级变量在 k8s_deploy.ps1 顶层赋值,AST 只抽函数,这里照抄一份;默认未注入。
    # 单独一条赋值(理由同上面 friend:两边都只做追加时,3-way 合并的冲突面最小)。
    $script:GoSvcCatalogue['mail'] = @{
        ConfigMap = 'go-svc-mail-config'; Manifest = 'mail.yaml'; MigrateJob = 'mail-migrate.yaml'
        ConfigFile = 'mail.yaml'; ImageName = 'mmorpg-mail'; Port = 50900; Global = $true
    }
    $script:GoSvcCatalogue['guild'] = @{
        ConfigMap = 'go-svc-guild-config'; Manifest = 'guild.yaml'; MigrateJob = 'guild-migrate.yaml'
        ConfigFile = 'guild.yaml'; ImageName = 'mmorpg-guild'; Port = 50300; Global = $true
    }
    $script:GoSvcCatalogue['match'] = @{
        ConfigMap = 'go-svc-match-config'; Manifest = 'match.yaml'; MigrateJob = 'match-migrate.yaml'
        ConfigFile = 'match_service.yaml'; ImageName = 'mmorpg-match'; Port = 50500; Global = $true
    }
    $script:MatchGlobalEnabled = $true
    Set-Item Function:script:Get-GlobalMatchDeploymentSettings {
        param($Profile, $TiDBHost, $Region)
        return [pscustomobject]@{ Enabled = $script:MatchGlobalEnabled }
    }
    # 本套只核迁移门禁，常驻三把密钥的注入由部署合同测试覆盖。
    Set-Item Function:script:Add-MatchAuthoritySecretEnv { param($ManifestContent) return $ManifestContent }
    $script:GrpcAllowLists = @{ guild = @{ PolicyName = 'guild-grpc-allowlist' } }
    $script:MailAdminTokenSecretName = 'mail-admin-auth'
    $script:MailAdminTokenSecretKey = 'token'
    $script:MailAdminTokenEnv = 'MMORPG_MAIL_ADMIN_TOKEN'
    $script:MailAdminToken = ''
    foreach ($name in @('Add-GoSvcSecretEnv', 'Get-InjectedSecretFingerprint')) {
        Restore-ProductionFunction $name
    }
    # 通用资产通道(-AssetOps,trade 是调用方之一):同样是 k8s_deploy.ps1 顶层赋值的脚本级变量,照抄一份;默认关(= 不传 -AssetOps)。
    $script:AssetOpsOn = $false
    $script:NetworkPoliciesOn = $false
    $script:AssetOpSecretKey = 'secret'
    $script:AssetOpCallers = [ordered]@{
        guild = [pscustomobject]@{ Env = 'MMORPG_ASSET_OP_SECRET_GUILD'; SecretName = 'asset-op-secret-guild'; HashAnnotation = 'mmorpg.io/asset-op-secret-guild-hash' }
        trade = [pscustomobject]@{ Env = 'MMORPG_ASSET_OP_SECRET_TRADE'; SecretName = 'asset-op-secret-trade'; HashAnnotation = 'mmorpg.io/asset-op-secret-trade-hash' }
    }
    $script:AssetOpSecretHashPlaceholder = 'PLACEHOLDER_ASSET_OP_SECRET_HASH'
    $script:AssetOpSecrets = @{}
    Set-Item Function:script:Apply-InjectedSecret {
        param($Namespace, $Name, $Key, $Value, $AppLabel)
        $script:Events.Add("apply:Secret:$Name")
    }

    Set-Item Function:script:Get-GoSvcMigrateJobWaitSeconds { return $script:BudgetSeconds }
    Set-Item Function:script:New-GoSvcConfigMapYaml {
        param($SvcName, $CurrentZoneId, $CurrentClusterId)
        return "kind: ConfigMap`nmetadata:`n  name: go-svc-trade-config"
    }
    Set-Item Function:script:Resolve-ImagePullPolicy { param($ImageRef) return 'IfNotPresent' }
    # 真实实现按 -KafkaBrokers 与档位算;这里固定成多 broker 的 3,证明注入值来自拓扑而不是写死的 1。
    Set-Item Function:script:Get-KafkaTopology { return [pscustomobject]@{ ReplicationFactor = 3 } }
    # 真实实现读 bin/etc/base_deploy_config.yaml;这里给一个仓库里不会出现的代号 7,证明注入值来自契约而不是写死。
    Set-Item Function:script:Get-GoSvcCommandTopicContract {
        if ($script:CommandTopicContractError) { throw $script:CommandTopicContractError }
        return [pscustomobject]@{ Partitions = 256; Generation = 7 }
    }
    Set-Item Function:script:Wait-ForDeploymentReady {
        param($Namespace, $DeploymentName)
        $script:Events.Add("ready:$DeploymentName")
    }
    Set-Item Function:script:Invoke-KubectlWithInputFile {
        [CmdletBinding()]
        param([string[]]$Args, [string]$InputContent)
        if ($InputContent -match '(?m)^kind: Job\s*$') {
            $script:MigrationContent = $InputContent
            $script:Events.Add('apply:Job')
            $script:JobApplied = $true
        } elseif ($InputContent -match '(?m)^kind: Deployment\s*$') {
            $script:Events.Add('apply:Deployment')
            $script:DeploymentContent = $InputContent
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

Write-Host '=== Migration Job admission regression tests (fully mocked kubectl) ==='

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

# friend（好友，D-14 独占库 mmorpg_friend）是第二个带 MigrateJob 的建表服务。用真 manifest 名跑一遍，
# 顺带钉住 friend.yaml / friend-migrate.yaml 确实在 deploy/k8s/manifests/go-svc/ 下，且占位符全部可替换
# （Apply-GoSvcMigrateJob 对残留占位 fail-closed）——缺文件时 Apply-OneGoSvc 会整条跳过，下面三条断言随即失败。
Test-Case 'friend 的迁移门禁与 trade 同链路：ConfigMap → Job → Deployment' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName friend -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:ConfigMap') -lt [Array]::IndexOf($events, 'apply:Job')) -Because 'friend-migrate Job 要读取先前创建的 ConfigMap'
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because 'staging/prod 的 Schema.AutoMigrate=false，表没建出来 friend 会拒启，迁移必须先于 Deployment'
    Assert-True -Condition ($events -contains 'query:Job') -Because '成功必须来自 Job 查询，不能因为是新服务就直接放行'
}

# data-service(全局库 mmorpg_global)走同一条门禁:staging/prod 启动期不建表、不种号段行,而 Store.Required=true
# 会让没有表的实例直接拒启,所以迁移 Job 必须先 Complete。用真 manifest 跑,顺带钉住两个文件都在、占位全部可替换。
Test-Case 'data-service 的迁移门禁:ConfigMap → Job → Deployment,且 Deployment 是多副本形态' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName 'data-service' -Namespace audit-zone -CurrentZoneId 1 -CurrentClusterId 0
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:ConfigMap') -lt [Array]::IndexOf($events, 'apply:Job')) -Because '迁移 Job 要读取先前创建的 ConfigMap'
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because '表与号段行没就位,Store.Required 会让 data-service 拒启'
    Assert-True -Condition ($events -contains 'query:Job') -Because '放行必须来自 Job 查询结果'
    Assert-Match -Text $script:DeploymentContent -Pattern '(?m)^  replicas: 2\s*$' -Because 'data-service 不再是单副本'
    Assert-Match -Text $script:DeploymentContent -Pattern 'name: data-service-pdb' -Because 'PDB 随主 manifest 一起 apply'
}

# player-inbox(玩家收件箱,全局库 mmorpg_player_inbox)走同一条门禁。用真 manifest 跑,顺带钉住两个文件都在、占位全部可替换
# (含 Pod 模板上的运维口令指纹占位)。未注入口令时:不注入 env、不建 Secret,指纹写 disabled(运维接口停用,fail-closed)。
Test-Case 'player-inbox 的迁移门禁与 trade 同链路,未注入运维口令时不建 Secret' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName 'player-inbox' -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:ConfigMap') -lt [Array]::IndexOf($events, 'apply:Job')) -Because 'player-inbox-migrate Job 要读取先前创建的 ConfigMap'
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because 'staging/prod 的 Schema.AutoMigrate=false,表没建出来 player-inbox 会拒启,迁移必须先于 Deployment'
    Assert-True -Condition ($events -contains 'query:Job') -Because '放行必须来自 Job 查询结果'
    Assert-True -Condition (-not ($events | Where-Object { $_ -like 'apply:Secret:*' })) -Because '未注入口令时不能建出一个空口令的 Secret'
    $content = $script:DeploymentContent
    Assert-NotMatch -Text $content -Pattern 'PLACEHOLDER_' -Because '镜像、拉取策略与口令指纹占位都必须被替换'
    Assert-Match -Text $content -Pattern 'mmorpg.io/player-inbox-admin-token-hash: "disabled"' -Because '未注入口令时指纹写 disabled,注入后模板必然变化、触发滚动'
    Assert-NotMatch -Text $content -Pattern 'name: PLAYER_INBOX_ADMIN_TOKEN' -Because '未注入口令时不能引用一个不存在的 Secret(Pod 会卡在 CreateContainerConfigError)'
    Assert-Match -Text $content -Pattern '(?m)^  replicas: 2\s*$' -Because 'MySQL 连接总账按两副本计'
    Assert-Match -Text $content -Pattern 'name: player-inbox-pdb' -Because 'PDB 随主 manifest 一起 apply'
}

Test-Case 'player-inbox 注入运维口令:先建 Secret 再 apply Deployment,env 走 secretKeyRef,口令值不进 manifest' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    $token = 'audit-only-player-inbox-admin-token-0123456789abcdef'
    $script:PlayerInboxAdminToken = $token
    Apply-OneGoSvc -SvcName 'player-inbox' -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    $secretAt = [Array]::IndexOf($events, 'apply:Secret:player-inbox-admin-token')
    Assert-True -Condition ($secretAt -ge 0) -Because '口令已解析时必须建 Secret'
    Assert-True -Condition ($secretAt -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because 'secretKeyRef 不是 optional,Secret 必须先于引用它的 Deployment'
    $content = $script:DeploymentContent
    Assert-Match -Text $content -Pattern '- name: PLAYER_INBOX_ADMIN_TOKEN\s+valueFrom:\s+secretKeyRef:\s+name: player-inbox-admin-token\s+key: admin-token' -Because '口令只经 Secret 进容器'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($content, 'name: PLAYER_INBOX_ADMIN_TOKEN').Count) -Because '只能注入一份'
    Assert-Match -Text $content -Pattern 'mmorpg.io/player-inbox-admin-token-hash: "[0-9a-f]{12}"' -Because '模板带口令指纹,轮换口令即滚动'
    Assert-True -Condition (-not $content.Contains($token)) -Because '口令值绝不能出现在 manifest 里'
    Assert-NotMatch -Text $content -Pattern 'PLACEHOLDER_' -Because '注入不能打乱原有占位替换'
}

# 现役邮件按生产者分别鉴权，DSN及密钥引用预置Secret；历史M1 AdminToken不再是注入契约。
# 保留正式建表先行、Secret引用必需且不泄露值的检查，不制造第二份旧口令。
Test-Case 'mail 的迁移门禁与 trade 同链路,只引用现役预置生产者Secret' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName 'mail' -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:ConfigMap') -lt [Array]::IndexOf($events, 'apply:Job')) -Because 'mail-migrate Job 要读取先前创建的 ConfigMap'
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because 'staging/prod 的 Schema.AutoMigrate=false,表没建出来 mail 会拒启,迁移必须先于 Deployment'
    Assert-True -Condition ($events -contains 'query:Job') -Because '放行必须来自 Job 查询结果'
    Assert-True -Condition (-not ($events | Where-Object { $_ -like 'apply:Secret:*' })) -Because '未注入令牌时不能建出一个空令牌的 Secret'
    Assert-True -Condition ([Array]::LastIndexOf($events, 'delete:Job') -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because '只替换本轮迁移Job，不删除运行实例仍引用的预置Secret'
    $content = $script:DeploymentContent
    Assert-NotMatch -Text $content -Pattern 'PLACEHOLDER_' -Because '镜像、拉取策略与令牌指纹占位都必须被替换'
    Assert-NotMatch -Text $content -Pattern 'MMORPG_MAIL_ADMIN_TOKEN|mail-admin-token-hash|mail-admin-auth' -Because '历史M1口令不得恢复成第二条授权路径'
    foreach ($producer in @('ADMIN', 'RANK', 'GUILD')) {
        $key = $producer.ToLowerInvariant()
        Assert-Match -Text $content -Pattern ("- name: MAIL_PRODUCER_SECRET_$producer\s+valueFrom:\s+secretKeyRef: \{name: domain-service-secrets, key: mail-producer-$key, optional: false\}") -Because '每个生产者只引用自己的必需密钥，缺失时拒启'
    }
    Assert-Match -Text $content -Pattern '(?m)^  replicas: 2\s*$' -Because 'MySQL / Redis 连接总账按两副本计'
    Assert-Match -Text $content -Pattern 'name: mail-pdb' -Because 'PDB 随主 manifest 一起 apply'
}

Test-Case 'mail 遗留运维令牌不覆盖现役生产者Secret,密钥值不进 manifest' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    $token = 'audit-only-mail-admin-token-0123456789abcdef'
    $script:MailAdminToken = $token
    Apply-OneGoSvc -SvcName 'mail' -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    Assert-True -Condition ($events -notcontains 'apply:Secret:mail-admin-auth') -Because '不创建已退出使用的历史M1口令Secret'
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because '预置Secret不能绕过正式迁移门禁'
    $content = $script:DeploymentContent
    Assert-Match -Text $content -Pattern '- name: MAIL_PRODUCER_SECRET_ADMIN\s+valueFrom:\s+secretKeyRef: \{name: domain-service-secrets, key: mail-producer-admin, optional: false\}' -Because '只从当前生产者Secret取管理签名密钥'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($content, 'name: MAIL_PRODUCER_SECRET_ADMIN').Count) -Because '管理生产者只能引用一份密钥'
    Assert-NotMatch -Text $content -Pattern 'MMORPG_MAIL_ADMIN_TOKEN|mail-admin-token-hash|mail-admin-auth' -Because '旧变量与指纹不能形成并行授权路径'
    Assert-True -Condition (-not $content.Contains($token)) -Because '令牌值绝不能出现在 manifest 里'
    Assert-NotMatch -Text $content -Pattern 'PLACEHOLDER_' -Because '注入不能打乱原有占位替换'
}


foreach ($profile in @('staging', 'prod')) {
    Test-Case "mail $profile 建表失败:不写令牌 Secret,不 apply Deployment" {
        Reset-MigrateFixture
        $script:ReleaseProfile = $profile
        $script:JobAbsentBeforeApply = $true
        $script:AfterApplyCondition = 'Failed'
        $script:MailAdminToken = 'audit-only-mail-admin-token-0123456789abcdef'
        $failure = Get-ThrownMessage { Apply-OneGoSvc -SvcName 'mail' -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0 }
        Assert-Match -Text $failure -Pattern 'mail-migrate.*failed' -Because '必须报告迁移失败,而不是其它模拟或参数错误'
        Assert-True -Condition ($script:Events -contains 'apply:Job') -Because '负向用例必须真正走到新 Job 的 apply'
        Assert-True -Condition ($script:Events -notcontains 'apply:Secret:mail-admin-auth') -Because '门禁失败时令牌 Secret 不得被覆盖(轮换令牌时旧 Pod 重启会读到新值)'
        Assert-True -Condition ($script:Events -notcontains 'apply:Deployment') -Because '失败后不得部署服务'
    }
}

# trade 是通用资产通道的调用方之一(-AssetOps):关着时指纹占位整行删掉、不注入密钥;打开时与 player-inbox 同一顺序(先 Secret 后 Deployment)。
Test-Case 'trade 资产通道关着:指纹占位整行删掉,不引用签名密钥,不建 Secret' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $content = $script:DeploymentContent
    # 清单注释会说明打开通道时的注解和环境变量;这里只检查生效字段,仍拒绝残留占位与任何签名密钥引用。
    $effectiveContent = (($content -split '\r?\n') | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
    Assert-NotMatch -Text $effectiveContent -Pattern '(?m)^[ \t]+["'']?mmorpg\.io/asset-op-secret-trade-hash["'']?[ \t]*:' -Because '关着时 Pod 模板不能保留密钥指纹注解,避免触发滚动'
    Assert-NotMatch -Text $effectiveContent -Pattern 'PLACEHOLDER_' -Because '生效清单不能残留未替换占位'
    Assert-NotMatch -Text $effectiveContent -Pattern '(?m)^[ \t]*-[ \t]+name:[ \t]*["'']?MMORPG_ASSET_OP_SECRET_TRADE["'']?[ \t]*(?:#.*)?$' -Because '关着时不能注入签名密钥环境变量'
    Assert-NotMatch -Text $effectiveContent -Pattern '(?m)^[ \t]+name:[ \t]*["'']?asset-op-secret-trade["'']?[ \t]*(?:#.*)?$' -Because '关着时不能引用一个不存在的签名密钥 Secret'
    Assert-True -Condition (-not ($script:Events | Where-Object { $_ -like 'apply:Secret:*' })) -Because '关着时不建 Secret'
}

Test-Case 'trade 资产通道打开:先建 Secret 再 apply Deployment,env 走 secretKeyRef,模板带指纹,密钥值不进 manifest' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    $secret = 'audit-only-asset-op-trade-secret-0123456789abcdef'
    $script:AssetOpsOn = $true
    $script:AssetOpSecrets = @{ trade = $secret }
    Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    $secretAt = [Array]::IndexOf($events, 'apply:Secret:asset-op-secret-trade')
    Assert-True -Condition ($secretAt -ge 0) -Because '打开时必须建 Secret'
    Assert-True -Condition ($secretAt -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because 'secretKeyRef 不是 optional,Secret 必须先于 Deployment'
    $content = $script:DeploymentContent
    Assert-Match -Text $content -Pattern '- name: MMORPG_ASSET_OP_SECRET_TRADE\s+valueFrom:\s+secretKeyRef:\s+name: asset-op-secret-trade\s+key: secret' -Because '签名密钥只经 Secret 进容器'
    Assert-Match -Text $content -Pattern 'mmorpg.io/asset-op-secret-trade-hash: "[0-9a-f]{12}"' -Because '模板带指纹,换密钥即滚动'
    Assert-True -Condition (-not $content.Contains($secret)) -Because '密钥值绝不能出现在 manifest 里'
    Assert-NotMatch -Text $content -Pattern 'PLACEHOLDER_' -Because '注入不能打乱原有占位替换'
}

Test-Case 'guild 资产off仅移除唯一inline资产env，SQL与Match签名保持，迁移仍先于Deployment' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName guild -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $content = $script:DeploymentContent
    Assert-NotMatch -Text $content -Pattern 'MMORPG_ASSET_OP_SECRET_GUILD|key: asset-guild|PLACEHOLDER_ASSET_OP_SECRET_HASH|mmorpg.io/asset-op-secret-guild-hash:' -Because 'off不能保留本域资产密钥或滚动指纹'
    foreach ($required in @('MMORPG_GUILD_SQL_DSN', 'MMORPG_MATCH_INTERNAL_SECRET_GUILD')) {
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($content, ('(?m)^\s*- name: ' + $required + '\s*$')).Count) -Because "off不能删除常驻必需$required"
    }
    Assert-True -Condition (-not ($script:Events | Where-Object { $_ -like 'apply:Secret:*' })) -Because 'off不创建可选资产Secret'
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because '资产off不能旁路正式SQL迁移门禁'
}

Test-Case 'guild 资产on保原inline引用改成本域Secret，先Secret后Deployment且保SQL' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    $secret = 'audit-only-asset-op-guild-secret-0123456789abcdef'
    $script:AssetOpsOn = $true
    $script:AssetOpSecrets = @{ guild = $secret }
    Apply-OneGoSvc -SvcName guild -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    $secretAt = [Array]::IndexOf($events, 'apply:Secret:asset-op-secret-guild')
    Assert-True -Condition ($secretAt -ge 0 -and $secretAt -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because '非optional Secret必须先存在'
    Assert-Match -Text $script:DeploymentContent -Pattern 'secretKeyRef: \{name: asset-op-secret-guild, key: secret, optional: false\}' -Because '保留inline实际形状并用本域Secret'
    Assert-Match -Text $script:DeploymentContent -Pattern 'mmorpg.io/asset-op-secret-guild-hash: "[0-9a-f]{12}"' -Because '注入指纹触发轮换'
    Assert-Match -Text $script:DeploymentContent -Pattern 'MMORPG_GUILD_SQL_DSN' -Because '资产on不能删除SQL配置'
    Assert-True -Condition (-not $script:DeploymentContent.Contains($secret)) -Because '密钥值不得进入manifest'
}

Test-Case 'guild 资产off原块缺失或重复在任何apply前拒绝，不能模糊删除其它env' {
    $original = [IO.File]::ReadAllText((Join-Path $script:RepoRoot 'deploy/k8s/manifests/go-svc/guild.yaml'))
    $block = [regex]::Match($original, '(?m)^(?<i>[ \t]*)- name: MMORPG_ASSET_OP_SECRET_GUILD[ \t]*\r?\n\k<i>  valueFrom:[ \t]*\r?\n\k<i>    secretKeyRef: \{name: domain-service-secrets, key: asset-guild, optional: false\}[ \t]*\r?\n').Value
    Assert-True -Condition ($block.Length -gt 0) -Because '负例先确认真实inline原块存在'
    $fixtureDir = Join-Path ([IO.Path]::GetTempPath()) ('guild-asset-contract-' + [guid]::NewGuid().ToString('N'))
    $fixturePath = Join-Path $fixtureDir 'guild.yaml'
    [IO.Directory]::CreateDirectory($fixtureDir) | Out-Null
    try {
        foreach ($candidate in @($original.Replace($block, ''), $original.Replace($block, $block + $block))) {
            Reset-MigrateFixture
            $script:GoSvcManifestsDir = $fixtureDir
            [IO.File]::WriteAllText($fixturePath, $candidate)
            $failure = Get-ThrownMessage { Apply-OneGoSvc -SvcName guild -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0 }
            Assert-Match -Text $failure -Pattern 'Guild AssetOps off 要求唯一的本域资产 env 原块' -Because '必须因确切块不唯一而拒绝，不能由其它错误喂饱断言'
            Assert-True -Condition (-not ($script:Events | Where-Object { $_ -like 'apply:*' })) -Because '渲染不确定时不得留下半截ConfigMap/Job/Deployment'
        }
    } finally {
        if (Test-Path -LiteralPath $fixturePath) { Remove-Item -LiteralPath $fixturePath -Force }
        [IO.Directory]::Delete($fixtureDir, $false)
    }
}

# 控制面命令 topic 契约(KAFKA_COMMAND_TOPIC_*)统一注入。背景:go/shared/kafkacmd 不配这两个变量就回落到 256 / 1,
# 而 C++ gate / scene 消费的是 base_deploy_config.yaml 的代号,不一致时 Go 发给 gate / scene 的命令静默丢失。
foreach ($svc in @('trade', 'friend', 'player-inbox', 'mail')) {
    Test-Case "$svc 的 Deployment 带上注入的命令 topic 契约,且只有一份" {
        Reset-MigrateFixture
        $script:JobAbsentBeforeApply = $true
        Apply-OneGoSvc -SvcName $svc -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
        $content = $script:DeploymentContent
        Assert-True -Condition ($null -ne $content) -Because '必须真正走到 Deployment 的 apply'
        Assert-Match -Text $content -Pattern '- name: KAFKA_COMMAND_TOPIC_PARTITIONS\s+value: "256"' -Because '分区数来自契约'
        Assert-Match -Text $content -Pattern '- name: KAFKA_COMMAND_TOPIC_GENERATION\s+value: "7"' -Because '代号来自契约,不是 kafkacmd 的编译期默认 1'
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($content, 'name: KAFKA_COMMAND_TOPIC_GENERATION').Count) -Because '重复的 env 名会让其中一份静默失效'
        Assert-Match -Text $content -Pattern '- name: KAFKA_TOPIC_REPLICATION_FACTOR\s+value: "3"' -Because 'topic 副本数来自 Kafka 拓扑,多 broker 集群上不能按 1 份建'
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($content, 'name: KAFKA_TOPIC_REPLICATION_FACTOR').Count) -Because '只能注入一份'
        Assert-NotMatch -Text $content -Pattern 'PLACEHOLDER_' -Because '注入不能打乱原有占位替换'
    }
}

Test-Case '契约读取失败时不做任何集群写操作' {
    Reset-MigrateFixture
    $script:CommandTopicContractError = 'contract unavailable'
    $failure = Get-ThrownMessage { Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0 }
    Assert-Match -Text $failure -Pattern 'contract unavailable' -Because '必须是契约错误本身,不是别的模拟故障'
    Assert-Equal -Expected 0 -Actual $script:Events.Count -Because '主 manifest 渲染在 ConfigMap / 迁移 Job 之前,渲染失败不能留下半截部署'
}

Test-Case 'Add-GoSvcCommandTopicEnv:沿用已有条目的缩进,插在 env 列表最前' {
    Reset-MigrateFixture
    $manifest = "kind: Deployment`nspec:`n  template:`n    spec:`n      containers:`n        - name: x`n          env:`n            # 注释行要跳过`n            - name: A`n              value: b`n          args: []`n"
    $out = Add-GoSvcCommandTopicEnv -SvcName x -ManifestContent $manifest -Partitions 256 -Generation 3
    Assert-Match -Text $out -Pattern '(?m)^          env:\n            # [^\n]*\n            - name: KAFKA_COMMAND_TOPIC_PARTITIONS\n              value: "256"\n            - name: KAFKA_COMMAND_TOPIC_GENERATION\n              value: "3"\n            # 注释行要跳过\n            - name: A' -Because '条目缩进必须与已有条目一致,否则 YAML 列表断裂'
    $compact = "containers:`n  - name: x`n    env:`n    - name: A`n      value: b`n"
    $compactOut = Add-GoSvcCommandTopicEnv -SvcName x -ManifestContent $compact -Partitions 256 -Generation 3
    Assert-Match -Text $compactOut -Pattern '(?m)^    env:\n    # [^\n]*\n    - name: KAFKA_COMMAND_TOPIC_PARTITIONS\n      value: "256"' -Because '与 env 同缩进的紧凑列表写法也要沿用原缩进'
    $crlf = "containers:`r`n  - name: x`r`n    env:`r`n      - name: A`r`n        value: b`r`n"
    $crlfOut = Add-GoSvcCommandTopicEnv -SvcName x -ManifestContent $crlf -Partitions 256 -Generation 3
    Assert-Equal -Expected 0 -Actual ([regex]::Matches($crlfOut, '(?<!\r)\n').Count) -Because 'CRLF 文件插入后不能混进裸 LF'
}

Test-Case 'Add-GoSvcCommandTopicEnv:manifest 形状不对或手写了变量一律拒绝' {
    Reset-MigrateFixture
    $handWritten = "containers:`n  - name: x`n    env:`n      - name: KAFKA_COMMAND_TOPIC_GENERATION`n        value: `"1`"`n"
    $failure = Get-ThrownMessage { Add-GoSvcCommandTopicEnv -SvcName x -ManifestContent $handWritten -Partitions 256 -Generation 2 }
    Assert-Match -Text $failure -Pattern 'KAFKA_COMMAND_TOPIC_\*' -Because '手写一份值就是第二份真相,下次换代号必漏改'
    $noEnv = "containers:`n  - name: x`n    args: []`n"
    $failure = Get-ThrownMessage { Add-GoSvcCommandTopicEnv -SvcName x -ManifestContent $noEnv -Partitions 256 -Generation 2 }
    Assert-Match -Text $failure -Pattern '恰好有 1 个 env' -Because '没有 env 段时不能悄悄不注入'
    $twoEnv = "containers:`n  - name: a`n    env:`n      - name: A`n        value: b`n  - name: c`n    env:`n      - name: C`n        value: d`n"
    $failure = Get-ThrownMessage { Add-GoSvcCommandTopicEnv -SvcName x -ManifestContent $twoEnv -Partitions 256 -Generation 2 }
    Assert-Match -Text $failure -Pattern '恰好有 1 个 env' -Because '多容器时不能猜注入目标'
    $notList = "containers:`n  - name: x`n    env:`n    args: []`n"
    $failure = Get-ThrownMessage { Add-GoSvcCommandTopicEnv -SvcName x -ManifestContent $notList -Partitions 256 -Generation 2 }
    Assert-Match -Text $failure -Pattern '不是列表项' -Because 'env 下不是列表时插入会产出非法 YAML'
}

Test-Case 'Add-GoSvcTopicReplicationEnv:与命令 topic 契约叠加注入,手写同名变量被拒' {
    Reset-MigrateFixture
    $manifest = "containers:`n  - name: x`n    env:`n      - name: A`n        value: b`n"
    $once = Add-GoSvcCommandTopicEnv -SvcName x -ManifestContent $manifest -Partitions 256 -Generation 2
    $twice = Add-GoSvcTopicReplicationEnv -SvcName x -ManifestContent $once -ReplicationFactor 3
    Assert-Match -Text $twice -Pattern '(?m)^    env:\n      # [^\n]*\n      - name: KAFKA_TOPIC_REPLICATION_FACTOR\n        value: "3"\n      # [^\n]*\n      - name: KAFKA_COMMAND_TOPIC_PARTITIONS\n        value: "256"\n      - name: KAFKA_COMMAND_TOPIC_GENERATION\n        value: "2"\n      - name: A' -Because '两组注入都落在同一个 env 列表里,缩进一致'
    $handWritten = "containers:`n  - name: x`n    env:`n      - name: KAFKA_TOPIC_REPLICATION_FACTOR`n        value: `"1`"`n"
    $failure = Get-ThrownMessage { Add-GoSvcTopicReplicationEnv -SvcName x -ManifestContent $handWritten -ReplicationFactor 3 }
    Assert-Match -Text $failure -Pattern 'KAFKA_TOPIC_REPLICATION_FACTOR' -Because '手写一份副本数就是第二份真相:broker 数变了它不会跟着变'
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
    $script:FixturePwshPath = (Get-Process -Id $PID).Path
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

Test-Case '全球match即使dev不带WaitReady也须先完成独立迁移Job' {
    Reset-MigrateFixture
    $script:ReleaseProfile = 'dev'
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName match -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:ConfigMap') -lt [Array]::IndexOf($events, 'apply:Job')) -Because '迁移先消费同一ConfigMap'
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Deployment')) -Because 'match常驻形态不自动DDL'
    Assert-True -Condition ($events -contains 'query:Job') -Because 'dev全球模式也必须核真实Job完成'
    Assert-Match -Text $script:MigrationContent -Pattern 'args:.*"-migrate"' -Because '运行正式迁移CLI'
    Assert-Match -Text $script:MigrationContent -Pattern 'MATCH_MIGRATION_USER' -Because '独立迁移账号'
    Assert-Match -Text $script:MigrationContent -Pattern 'key: match-migration-password' -Because '独立迁移口令'
    Assert-NotMatch -Text $script:MigrationContent -Pattern 'MATCH_PROBE_SECRET|RANK_PRODUCER_SECRET_MATCH|MMORPG_LOGIN_REDIS_PASSWORD' -Because '迁移不取得业务密钥'
    Assert-NotMatch -Text $script:DeploymentContent -Pattern 'MATCH_MIGRATION_USER|MATCH_MIGRATION_PASSWORD' -Because '常驻进程不得取得DDL凭据'
    $source = Get-Content -LiteralPath $script:DeployScript -Raw
    Assert-Match -Text $source -Pattern 'match\s+=.*MigrateJob = "match-migrate.yaml"' -Because '真实生产目录登记迁移Job，不能只有测试目录'
}

Test-Case 'match迁移失败不得部署常驻进程' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    $script:AfterApplyCondition = 'Failed'
    $failure = Get-ThrownMessage { Apply-OneGoSvc -SvcName match -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0 }
    Assert-Match -Text $failure -Pattern 'match-migrate.*failed' -Because '返回原迁移失败'
    Assert-True -Condition ($script:Events -contains 'apply:Job') -Because '确实运行迁移链'
    Assert-True -Condition ($script:Events -notcontains 'apply:Deployment') -Because '失败不能继续接流'
}

Test-Case '显式旧dev关闭全球SQL时不运行match迁移Job' {
    Reset-MigrateFixture
    $script:ReleaseProfile = 'dev'
    $script:MatchGlobalEnabled = $false
    Apply-GoSvcMigrateJob -SvcName match -Namespace audit-infra -SvcImage audit/match:test -PullPolicy IfNotPresent
    Assert-Equal 0 $script:Events.Count '兼容端口不能隐式建SQL表'
}

exit (Complete-TestRun -SuiteName 'k8s_migrate_gate')
