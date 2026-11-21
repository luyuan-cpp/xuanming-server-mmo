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
        'Write-GoSvcMigrateJobDiagnostics', 'Add-GoSvcEnvEntries', 'Add-GoSvcCommandTopicEnv', 'Add-GoSvcTopicReplicationEnv',
        'Get-SqlRuntimeManifestRange', 'Add-SchemaPermissionManifest', 'Get-DbAllowedDatabaseNames', 'Apply-GoSvcManifests', 'Get-ZoneScopedGoSvcNames',
        'Test-InfraMysqlInUse', 'Get-SqlEntryEndpoint', 'Get-GlobalAuthoritySqlSettings', 'Get-FriendSqlSettings',
        # data-service 读集群身份的 RBAC 渲染(发号器地域码绑定,docs/handoff/global-id-issuer-registry-20261105.md §8.4):用真函数读真清单。
        'New-DataServiceIdentityRbacYaml')) {
        Restore-ProductionFunction $name
    }
    # 迁移 Job 的 init 容器等这次实际的 SQL 入口(Get-SqlEntryEndpoint),只有入口是集群内 MySQL 时才先等 deploy/mysql(Test-InfraMysqlInUse)。
    # 缺省给外部 TiDB 入口;用例要测集群内 MySQL 时自己改这三个变量。
    $script:GlobalAuthorityTiDBHost = 'tidb.invalid:4000'
    $script:FriendTiDBHost = ''
    $script:TradeTiDBHost = ''
    $script:MatchTiDBHost = ''
    $script:DatabaseBackend = 'tidb'
    # Add-GoSvcEnvEntries:Add-GoSvcCommandTopicEnv / Add-GoSvcSecretEnv 共用的 env 注入助手,由 k8s_deploy 属主从两者中抽出。
    # 抽出之前部署脚本里还没有它,所以按"存在才恢复"处理;抽出之后调用方依赖它,若它缺席或改名,
    # 调用处会报"命令未找到",Add-GoSvcCommandTopicEnv 与 Apply-OneGoSvc 的用例随之变红,不会静默通过。
    if ($script:ProductionFunctions.ContainsKey('Add-GoSvcEnvEntries')) {
        Restore-ProductionFunction 'Add-GoSvcEnvEntries'
    }
    # 发布轨道(docs/design/release-track-gray-canary.md):Apply-OneGoSvc 每个服务都调它。用真函数、按 stable 跑:stable 不写 env,
    # 清单原样返回,本套的断言不受影响;函数缺席或改名时 Apply-OneGoSvc 的用例报「命令未找到」变红。
    Restore-ProductionFunction 'Add-GoSvcReleaseTrackEnv'
    $script:ReleaseTrack = 'stable'
    $script:DeploymentContent = $null
    $script:MigrationContent = $null
    $script:IdentityRbacContent = $null
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
    # go/db 的迁移入口是单独的 cmd/migrate,Job 用单独的镜像(目录字段 MigrateImageName)。
    $script:GoSvcCatalogue['db'] = @{
        ConfigMap = 'go-svc-db-config'; Manifest = 'db.yaml'; MigrateJob = 'db-migrate.yaml'; MigrateImageName = 'mmorpg-db-migrate'
        ConfigFile = 'db.yaml'; ImageName = 'mmorpg-db'; Port = 6000
    }
    $script:SqlSchemaOnly = $false
    # 写 SQL 的服务表(k8s_deploy.ps1 顶层的 $script:SqlWriterGoSvcs,AST 只抽函数,这里照抄一份):Apply-OneGoSvc 按它决定调不调下面的写者 boot 桩。
    $script:SqlWriterGoSvcs = @('db', 'data-service', 'login', 'chat', 'friend', 'guild', 'mail', 'rank', 'payment', 'match', 'trade', 'player-inbox')
    # 本套只核迁移门禁，常驻三把密钥的注入由部署合同测试覆盖。
    Set-Item Function:script:Add-MatchAuthoritySecretEnv { param($ManifestContent) return $ManifestContent }
    # 写者启动授权和逻辑库同样不归本套：注入、名册核对在 k8s_deploy_contract、logical_db_deploy_contract。
    # 桩只替真函数做占位替换这一步(换成目录里的 Secret 名和摘要),清单才和真部署同形状,「占位必须全部替换」的断言继续管得住其余占位。
    Set-Item Function:script:Add-SqlWriterBootManifest { param($ManifestContent, $Namespace, $SvcName, $RuntimeKind) $script:Events.Add("render:boot:$SvcName"); return ($ManifestContent -creplace 'PLACEHOLDER_SQL_WRITER_[A-Z0-9_]+', 'fixture-sql-writer') }
    Set-Item Function:script:Add-SqlWriterPlacementEnv { param($ManifestContent) return $ManifestContent }
    # 缩容收尾(docs/design/writer-roster-exhaustion-20261106.md §5.9)也不归本套:渲染、落地与当场收尾只记事件,
    # 它们的拼装与顺序由 sql_writer_slot_retirer_contract.tests.ps1 守。线上副本数回 0:本套的用例都不是缩容。
    Set-Item Function:script:New-SqlWriterSlotRetirerManifest { param($Namespace, $StatefulSet) $script:Events.Add("render:slot-retirer:$StatefulSet"); return "fixture-slot-retirer-$StatefulSet" }
    Set-Item Function:script:Apply-SqlWriterSlotRetirer { param($Namespace, $StatefulSet, $Manifest) $script:Events.Add("apply:slot-retirer:$StatefulSet") }
    Set-Item Function:script:Get-SqlWriterLiveReplicas { param($Namespace, $StatefulSet) $script:Events.Add("query:live-replicas:$StatefulSet"); return 0 }
    Set-Item Function:script:Complete-SqlWriterScaleDown { param($Namespace, $StatefulSet, $LiveReplicas, $ManifestContent) $script:Events.Add("check:scale-down:$StatefulSet") }
    # 写者换成 StatefulSet 之前留下的同名 Deployment 由它收掉(k8s_deploy.ps1 Remove-LegacySqlWriterDeployment)。只记事件,不走 Invoke-Kubectl:那个桩把一切 delete 记成 delete:Job。
    Set-Item Function:script:Remove-LegacySqlWriterDeployment { param($Namespace, $Name) $script:Events.Add("delete:legacy-deployment:$Name") }
    Set-Item Function:script:Assert-DataServiceRosterSchemas { param($Namespace) $script:Events.Add('check:roster') }
    # 登记处双向 TLS 的预置 Secret 核对要读集群,这里只记事件(真函数的形状由 id_issuer_deploy_contract 核)。
    Set-Item Function:script:Assert-TlsSecretProvisioned { param($Namespace, $Name, $Purpose) $script:Events.Add("check:tls:$Name") }
    Set-Item Function:script:Add-IdIssuerExpectedRegionEnv { param($ManifestContent) return $ManifestContent }
    Set-Item Function:script:Add-PlayerInboxMatchSecretEnv { param($ManifestContent) return $ManifestContent }
    # schema Job 的注入用真函数,目录条目只有它要的两个 Secret:迁移 Job 在名册定下来之前就要跑。
    Set-Item Function:script:Get-SqlWriterCatalogEntry { param($Namespace, $SvcName) return [ordered]@{ schemaConfigSecret = 'fixture-schema-config'; schemaPermissionSecret = 'fixture-schema-permission' } }
    $script:GrpcAllowLists = @{ guild = @{ PolicyName = 'guild-grpc-allowlist' } }
    $script:MailAdminTokenSecretName = 'mail-admin-auth'
    $script:MailAdminTokenSecretKey = 'token'
    $script:MailAdminTokenEnv = 'MMORPG_MAIL_ADMIN_TOKEN'
    $script:MailAdminToken = ''
    $script:InternalAuthPublicKeys = 'd75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a'
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
        } elseif ($InputContent -match '(?m)^kind: (?:Deployment|StatefulSet)\s*$') {
            # 工作负载:SQL 写者是 StatefulSet(槽位 = 序号,writer-roster-exhaustion-20261106.md §5.5),其余是 Deployment。本套的服务都是写者。
            $script:Events.Add('apply:Workload')
            $script:DeploymentContent = $InputContent
        } elseif ($InputContent -match '(?m)^kind: ConfigMap\s*$') {
            $script:Events.Add('apply:ConfigMap')
        } elseif ($InputContent -match '(?m)^kind: ClusterRoleBinding\s*$') {
            # data-service 读集群身份的 RBAC:ServiceAccount、ClusterRole、ClusterRoleBinding 同一份清单。
            $script:Events.Add('apply:IdentityRbac')
            $script:IdentityRbacContent = $InputContent
        } elseif ($InputContent -match '(?m)^kind: RoleBinding\s*$' -and $InputContent -match '(?m)^  name: match-battle-pod-reader\s*$') {
            # match 查 battle Pod 的 RBAC:ServiceAccount、Role、RoleBinding 同一份清单(match-battle-pod-reader-rbac.yaml)。
            $script:Events.Add('apply:BattlePodReaderRbac')
            $script:BattlePodReaderRbacContent = $InputContent
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
        Assert-True -Condition ($script:Events -notcontains 'apply:Workload') -Because '失败后不得部署服务'
    }
}

Test-Case '成功 Job 才放行 Deployment，且 ConfigMap 与 Job 的顺序正确' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:ConfigMap') -lt [Array]::IndexOf($events, 'apply:Job')) -Because 'Job 要读取先前创建的 ConfigMap'
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because '迁移必须先于服务部署'
    Assert-True -Condition ($events -contains 'query:Job') -Because '成功必须来自 Job 查询，不能直接放行'
}

# docs/design/no-single-node-horizontal-scaling-20261001.md §6:tidb 档没有集群内 MySQL,init 容器等实际入口,脚本不等 deploy/mysql。
Test-Case 'wait-sql 等这次实际的 SQL 入口;只有入口是集群内 MySQL(dev)时才先等 deploy/mysql' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    Assert-Match -Text $script:MigrationContent -Pattern 'name: SQL_HOST\s+value: "tidb\.invalid"' -Because '主机取 -GlobalAuthorityTiDBHost'
    Assert-Match -Text $script:MigrationContent -Pattern 'name: SQL_PORT\s+value: "4000"' -Because '端口跟着入口,不写死 3306'
    Assert-NotMatch -Text $script:MigrationContent -Pattern 'mysql\.audit-infra|MYSQL_ADDR' -Because '外部 TiDB 时清单里没有集群内 MySQL 的地址'
    Assert-True -Condition ($script:Events -notcontains 'ready:mysql') -Because 'tidb 档不等一台没有部署的 MySQL'

    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    $script:ReleaseProfile = 'dev'
    $script:WaitReady = $true
    $script:GlobalAuthorityTiDBHost = 'mysql.audit-infra:3306'
    $script:DatabaseBackend = 'mysql'
    Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    Assert-Match -Text $script:MigrationContent -Pattern 'name: SQL_HOST\s+value: "mysql\.audit-infra"' -Because 'dev 用集群内 MySQL 时等的就是它'
    Assert-True -Condition ([Array]::IndexOf(@($script:Events), 'ready:mysql') -ge 0 -and [Array]::IndexOf(@($script:Events), 'ready:mysql') -lt [Array]::IndexOf(@($script:Events), 'apply:Job')) -Because '先等 deploy/mysql 就绪,再 apply Job'

    Reset-MigrateFixture
    $script:GlobalAuthorityTiDBHost = 'mysql.audit-infra:3306'
    $script:DatabaseBackend = 'mysql'
    $failure = Get-ThrownMessage { Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0 }
    Assert-Match -Text $failure -Pattern 'is the in-cluster MySQL' -Because 'prod 不许把入口指到集群内单实例 MySQL'
    Assert-True -Condition ($script:Events -notcontains 'apply:Job') -Because '拒绝发生在 apply 之前'
}

# 全局服务分两步(k8s_deploy.ps1 的 Apply-GlobalGoSvcManifests):第一步只有 ConfigMap 与迁移 Job,第二步只有工作负载。
Test-Case '-Stage Prepare 只做 ConfigMap 与迁移 Job,不碰 Deployment 与注入的 Secret;门禁失败同样中断' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    $script:AssetOpsOn = $true
    $script:AssetOpSecrets = @{ trade = 'contract-asset-op-secret-trade-0123456789abcdef'; guild = 'contract-asset-op-secret-guild-0123456789abcdef' }
    Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0 -Stage Prepare
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:ConfigMap') -ge 0 -and [Array]::IndexOf($events, 'apply:ConfigMap') -lt [Array]::IndexOf($events, 'apply:Job')) -Because 'Prepare 先 ConfigMap 再 Job'
    Assert-True -Condition ($events -notcontains 'apply:Workload') -Because 'Prepare 不起进程:data-service 就绪之前 trade 读不到合服映射'
    Assert-True -Condition (@($events | Where-Object { $_ -like 'apply:Secret:*' }).Count -eq 0) -Because '只有工作负载引用的 Secret 留给 Workload,迁移 Job 不读它'

    Reset-MigrateFixture
    $script:ReleaseProfile = 'prod'
    $script:JobAbsentBeforeApply = $true
    $script:AfterApplyCondition = 'Failed'
    $failure = Get-ThrownMessage { Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0 -Stage Prepare }
    Assert-Match -Text $failure -Pattern 'trade-migrate.*failed' -Because 'Prepare 阶段迁移失败同样中断发布,不留到第二步才发现'
}

Test-Case '-Stage Workload 只起进程:不重写 ConfigMap、不重跑迁移 Job;注入的 Secret 先于 Deployment' {
    Reset-MigrateFixture
    $script:AssetOpsOn = $true
    $script:AssetOpSecrets = @{ trade = 'contract-asset-op-secret-trade-0123456789abcdef'; guild = 'contract-asset-op-secret-guild-0123456789abcdef' }
    Apply-OneGoSvc -SvcName trade -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0 -Stage Workload
    $events = @($script:Events)
    Assert-True -Condition ($events -contains 'apply:Workload') -Because 'Workload 要 apply Deployment'
    foreach ($absent in @('apply:ConfigMap', 'apply:Job', 'delete:Job', 'query:Job')) {
        Assert-True -Condition ($events -notcontains $absent) -Because "Workload 不该有 $absent:ConfigMap 与 Job 第一步已经做过"
    }
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Secret:asset-op-secret-trade') -ge 0 -and [Array]::IndexOf($events, 'apply:Secret:asset-op-secret-trade') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because 'secretKeyRef 不是 optional,Secret 必须先于 Deployment'
}

# 全新集群的 data-service、db 走同一个 Prepare:名册要按 Job 种下的 generation 写,所以这一步不读名册。
foreach ($svc in @('data-service', 'db')) {
    Test-Case "$svc -Stage Prepare 只跑 ConfigMap 与迁移 Job,不渲染写者名册" {
        Reset-MigrateFixture
        $script:JobAbsentBeforeApply = $true
        Apply-OneGoSvc -SvcName $svc -Namespace audit-zone -CurrentZoneId 1 -CurrentClusterId 0 -Stage Prepare
        $events = @($script:Events)
        Assert-True -Condition ([Array]::IndexOf($events, 'apply:ConfigMap') -ge 0 -and [Array]::IndexOf($events, 'apply:ConfigMap') -lt [Array]::IndexOf($events, 'apply:Job')) -Because 'Prepare 先 ConfigMap 再 Job'
        Assert-True -Condition ($events -notcontains 'apply:Workload') -Because '名册没定之前不起进程'
        Assert-True -Condition ($events -notcontains 'apply:IdentityRbac') -Because '读集群身份的 RBAC 随工作负载走,Prepare 不起进程也就不 apply'
        Assert-True -Condition (@($events | Where-Object { $_ -like 'check:tls:*' }).Count -eq 0) -Because 'Prepare 不起进程,不核登记处客户端证书'
        Assert-True -Condition ($events -notcontains "render:boot:$svc") -Because '这一步还没有 generation,不能要求名册'
        Assert-True -Condition ($events -notcontains 'check:roster') -Because '同上'
    }
}

Test-Case 'data-service 重复部署:Job 再跑一遍(-migrate 对齐全的库返回 0),名册这时才核,然后才 apply Deployment' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName 'data-service' -Namespace audit-zone -CurrentZoneId 1 -CurrentClusterId 0
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'check:roster') -ge 0 -and [Array]::IndexOf($events, 'check:roster') -lt [Array]::IndexOf($events, 'apply:ConfigMap')) -Because '名册不对在写操作之前拦下'
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because 'Job 先于 Deployment'
}

Test-Case 'db 的迁移 Job:单独的镜像,库清单来自 MMORPG_DB_ALLOWED_DATABASES,排在 db Deployment 之前' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    $saved = [Environment]::GetEnvironmentVariable('MMORPG_DB_ALLOWED_DATABASES')
    try {
        [Environment]::SetEnvironmentVariable('MMORPG_DB_ALLOWED_DATABASES', 'player_store_1000000_db, player_store_1000001_db')
        Apply-OneGoSvc -SvcName db -Namespace audit-zone -CurrentZoneId 1 -CurrentClusterId 0
    } finally { [Environment]::SetEnvironmentVariable('MMORPG_DB_ALLOWED_DATABASES', $saved) }
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because 'db 的闸门表由这个 Job 建,registrar 要等它'
    $job = $script:MigrationContent
    Assert-Match -Text $job -Pattern 'image: registry\.invalid/audit/mmorpg-db-migrate:test' -Because 'cmd/migrate 不在服务二进制里'
    Assert-Match -Text $script:DeploymentContent -Pattern 'image: registry\.invalid/audit/mmorpg-db:test' -Because 'Deployment 仍是服务镜像'
    Assert-Match -Text $job -Pattern 'name: DB_ALLOWED_DATABASES\s+value: "player_store_1000000_db,player_store_1000001_db"' -Because '逐库迁全部存档库'
    Assert-Match -Text $job -Pattern 'secretName: fixture-schema-config' -Because '配置来自 schemaConfigSecret'
    Assert-NotMatch -Text $job -Pattern 'configMap:|PLACEHOLDER_|__INFRA_NAMESPACE__' -Because '占位全部替换,配置卷换成 Secret'
}

Test-Case 'Apply-GoSvcManifests -Stage Prepare 只跑带迁移 Job 的 zone 服务,db 在 data-service 之前' {
    Reset-MigrateFixture
    $script:SkipGoSvc = $false
    $script:Called = [Collections.Generic.List[string]]::new()
    $script:GoSvcCatalogue['login'] = @{ ConfigMap = 'go-svc-login-config'; Manifest = 'login.yaml' }
    Set-Item Function:script:Apply-OneGoSvc {
        param($SvcName, $Namespace, $CurrentZoneId, $CurrentClusterId, $Stage)
        $script:Called.Add("${SvcName}:$Stage")
    }
    Apply-GoSvcManifests -Namespace audit-zone -CurrentZoneId 1 -CurrentClusterId 0 -Stage Prepare
    Assert-Equal -Expected 'db:Prepare,data-service:Prepare' -Actual (@($script:Called) -join ',') -Because '-SqlSchemaOnly 只碰有迁移 Job 的服务;全局服务不在 zone 循环里'
    $script:Called.Clear()
    Apply-GoSvcManifests -Namespace audit-zone -CurrentZoneId 1 -CurrentClusterId 0
    Assert-Equal -Expected 'db:All,data-service:All,login:All' -Actual (@($script:Called) -join ',') -Because '正常部署全部 zone 服务,db-migrate 先于 data-service 的 registrar'
}

Test-Case 'Apply-GoSvcManifests -Only / -Except:zone-up 先单独发 db,后面那批跳过它(存档世代顺序,数据覆写审计 r2-ps#11)' {
    Reset-MigrateFixture
    $script:SkipGoSvc = $false
    $script:Called = [Collections.Generic.List[string]]::new()
    $script:GoSvcCatalogue['login'] = @{ ConfigMap = 'go-svc-login-config'; Manifest = 'login.yaml' }
    Set-Item Function:script:Apply-OneGoSvc {
        param($SvcName, $Namespace, $CurrentZoneId, $CurrentClusterId, $Stage)
        $script:Called.Add("${SvcName}:$Stage")
    }
    Apply-GoSvcManifests -Namespace audit-zone -CurrentZoneId 1 -CurrentClusterId 0 -Only @('db')
    Assert-Equal -Expected 'db:All' -Actual (@($script:Called) -join ',') -Because '-Only 只发点名的服务'
    $script:Called.Clear()
    Apply-GoSvcManifests -Namespace audit-zone -CurrentZoneId 1 -CurrentClusterId 0 -Except @('db')
    Assert-Equal -Expected 'data-service:All,login:All' -Actual (@($script:Called) -join ',') -Because '-Except 跳过已发的服务,其余照旧按 Get-ZoneScopedGoSvcNames 的先后'
    $script:Called.Clear()
    Apply-GoSvcManifests -Namespace audit-zone -CurrentZoneId 1 -CurrentClusterId 0 -Only @() -Except @()
    Assert-Equal -Expected 'db:All,data-service:All,login:All' -Actual (@($script:Called) -join ',') -Because '两个都空 = 全部,与不传相同'
}

Test-Case '-SqlSchemaOnly 恒等 Job 结束,dev 也等:下一步要读 generation' {
    Reset-MigrateFixture
    $script:ReleaseProfile = 'dev'
    $script:SqlSchemaOnly = $true
    $script:JobAbsentBeforeApply = $true
    $script:AfterApplyCondition = 'Failed'
    $failure = Get-ThrownMessage { Apply-OneGoSvc -SvcName 'data-service' -Namespace audit-zone -CurrentZoneId 1 -CurrentClusterId 0 -Stage Prepare }
    Assert-Match -Text $failure -Pattern 'data-service-migrate.*failed' -Because 'Job 失败要报出来,不能当它做完了去读 generation'
}

# 写者的 schema Job 有块式(trade、friend、player-inbox、data-service、db)和流式(guild、mail、rank、payment、match、chat)两种 configMap 写法,注入都要认。
foreach ($case in @(
        @{ Svc = 'trade'; File = 'trade-migrate.yaml'; ConfigMap = 'go-svc-trade-config' }
        @{ Svc = 'friend'; File = 'friend-migrate.yaml'; ConfigMap = 'go-svc-friend-config' }
        @{ Svc = 'player-inbox'; File = 'player-inbox-migrate.yaml'; ConfigMap = 'go-svc-player-inbox-config' }
        @{ Svc = 'data-service'; File = 'data-service-migrate.yaml'; ConfigMap = 'go-svc-data-service-config' }
        @{ Svc = 'db'; File = 'db-migrate.yaml'; ConfigMap = 'go-svc-db-config' }
        @{ Svc = 'guild'; File = 'guild-migrate.yaml'; ConfigMap = 'go-svc-guild-config' }
        @{ Svc = 'mail'; File = 'mail-migrate.yaml'; ConfigMap = 'go-svc-mail-config' }
        @{ Svc = 'rank'; File = 'rank-migrate.yaml'; ConfigMap = 'go-svc-rank-config' }
        @{ Svc = 'payment'; File = 'payment-migrate.yaml'; ConfigMap = 'go-svc-payment-config' }
        @{ Svc = 'match'; File = 'match-migrate.yaml'; ConfigMap = 'go-svc-match-config' }
        @{ Svc = 'chat'; File = 'chat-migrate.yaml'; ConfigMap = 'go-svc-chat-config' }
    )) {
    Test-Case "schema Job 注入:$($case.Svc) 的配置卷换成 schemaConfigSecret,带上 schema 主体" {
        Reset-MigrateFixture
        $text = [IO.File]::ReadAllText((Join-Path $script:GoSvcManifestsDir $case.File))
        Assert-True -Condition ($text.Contains($case.ConfigMap)) -Because '清单确实挂这个 ConfigMap(前提)'
        $rendered = Add-SchemaPermissionManifest -Content $text -Namespace audit-infra -SvcName $case.Svc
        Assert-Match -Text $rendered -Pattern '- name: svc-config\s+secret:\s+secretName: fixture-schema-config' -Because '配置卷换成 schema 账号那一份'
        # 清单头的注释会提到 ConfigMap 名,只看非注释行。
        $renderedNoComment = ($rendered -split "`r?`n" | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
        Assert-NotMatch -Text $renderedNoComment -Pattern ([regex]::Escape($case.ConfigMap)) -Because 'ConfigMap 里没有 schema 账号,不能留着'
        Assert-Match -Text $rendered -Pattern 'name: MMORPG_SQL_SCHEMA_PRINCIPAL\s+valueFrom: \{secretKeyRef: \{name: fixture-schema-permission, key: principal, optional: false\}\}' -Because 'LoadSchemaPermission 读它'
        Assert-NotMatch -Text $rendered -Pattern 'MMORPG_SQL_SCHEMA_DSN' -Because '没有程序读这个变量'
        Assert-Match -Text $rendered -Pattern 'name: MMORPG_DB_PLACEMENT_REQUIRED' -Because 'prod 的夹具要求配置带 DbPlacement'
    }
}

Test-Case 'schema Job 注入:两个 configMap 写法都找不到时拒绝,不猜' {
    Reset-MigrateFixture
    $text = [IO.File]::ReadAllText((Join-Path $script:GoSvcManifestsDir 'guild-migrate.yaml'))
    $broken = $text.Replace('configMap: {name: go-svc-guild-config}', 'emptyDir: {}')
    Assert-Match -Text (Get-ThrownMessage { Add-SchemaPermissionManifest -Content $broken -Namespace audit-infra -SvcName guild }) -Pattern 'schema Job必须唯一独立配置挂载' -Because '形状变了要报错'
}

foreach ($case in @(@{ Svc = 'guild'; File = 'guild-migrate.yaml'; Env = 'MMORPG_GUILD_SQL_DSN' }, @{ Svc = 'mail'; File = 'mail-migrate.yaml'; Env = 'MMORPG_MAIL_SQL_DSN' }, @{ Svc = 'rank'; File = 'rank-migrate.yaml'; Env = 'MMORPG_RANK_SQL_DSN' }, @{ Svc = 'payment'; File = 'payment-migrate.yaml'; Env = 'MMORPG_PAYMENT_SQL_DSN' })) {
    Test-Case "$($case.Svc) 的 Job:DSN 和主体出自同一个 schemaPermissionSecret" {
        Reset-MigrateFixture
        $text = [IO.File]::ReadAllText((Join-Path $script:GoSvcManifestsDir $case.File))
        Assert-NotMatch -Text $text -Pattern 'domain-service-secrets' -Because '常驻账号不是 schema 主体,Job 会报「权威SQL受限主体不匹配」'
        $rendered = Add-SchemaPermissionManifest -Content $text -Namespace audit-infra -SvcName $case.Svc
        Assert-Match -Text $rendered -Pattern ("name: $($case.Env)\s+valueFrom:\s+secretKeyRef: \{name: fixture-schema-permission, key: dsn, optional: false\}") -Because 'DSN 取 schemaPermissionSecret 的 dsn'
        Assert-NotMatch -Text $rendered -Pattern 'PLACEHOLDER_SCHEMA_PERMISSION_SECRET' -Because '占位要替换'
    }
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
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because 'staging/prod 的 Schema.AutoMigrate=false，表没建出来 friend 会拒启，迁移必须先于 Deployment'
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
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because '表与号段行没就位,Store.Required 会让 data-service 拒启'
    Assert-True -Condition ($events -contains 'query:Job') -Because '放行必须来自 Job 查询结果'
    Assert-Match -Text $script:DeploymentContent -Pattern '(?m)^  replicas: 2\s*$' -Because 'data-service 不再是单副本'
    Assert-Match -Text $script:DeploymentContent -Pattern 'name: data-service-pdb' -Because 'PDB 随主 manifest 一起 apply'
    # 发号器地域码绑定的部署身份(docs/handoff/global-id-issuer-registry-20261105.md §8.4):Pod 用 ServiceAccount data-service 读 kube-system 的 UID。
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:IdentityRbac') -ge 0 -and [Array]::IndexOf($events, 'apply:IdentityRbac') -lt [Array]::IndexOf($events, 'apply:Workload')) `
        -Because '读集群身份的 RBAC 先于 StatefulSet,否则第一次预留读身份拿到 403'
    Assert-Match -Text $script:IdentityRbacContent -Pattern '(?m)^  name: mmorpg-cluster-identity-audit-zone\s*$' -Because 'ClusterRoleBinding 名字带本 zone 命名空间'
    Assert-Match -Text $script:IdentityRbacContent -Pattern '(?m)^    namespace: audit-zone\s*$' -Because '只授权本 zone 的 data-service'
    Assert-Match -Text $script:DeploymentContent -Pattern '(?m)^      serviceAccountName: data-service\s*$' -Because 'Pod 用专用身份,不用 default'
    # 夹具是 prod:登记处双向 TLS 的客户端证书在本服务第一个写操作(ConfigMap)之前核对。
    Assert-True -Condition ([Array]::IndexOf($events, 'check:tls:id-registry-client-tls') -ge 0 -and [Array]::IndexOf($events, 'check:tls:id-registry-client-tls') -lt [Array]::IndexOf($events, 'apply:ConfigMap')) `
        -Because '证书缺了 data_service 装配不了发号器,要在写任何东西之前拒绝'
    Assert-Match -Text $script:DeploymentContent -Pattern 'name: id-registry-client-tls\s+secret: \{secretName: id-registry-client-tls, defaultMode: 288, optional: true\}' -Because '证书卷来自预置 Secret'
}

# player-inbox(玩家收件箱,全局库 mmorpg_player_inbox)走同一条门禁。用真 manifest 跑,顺带钉住两个文件都在、占位全部可替换
# (含 Pod 模板上的运维口令指纹占位)。未注入口令时:不注入 env、不建 Secret,指纹写 disabled(运维接口停用,fail-closed)。
Test-Case 'player-inbox 的迁移门禁与 trade 同链路,未注入运维口令时不建 Secret' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    Apply-OneGoSvc -SvcName 'player-inbox' -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:ConfigMap') -lt [Array]::IndexOf($events, 'apply:Job')) -Because 'player-inbox-migrate Job 要读取先前创建的 ConfigMap'
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because 'staging/prod 的 Schema.AutoMigrate=false,表没建出来 player-inbox 会拒启,迁移必须先于 Deployment'
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
    Assert-True -Condition ($secretAt -lt [Array]::IndexOf($events, 'apply:Workload')) -Because 'secretKeyRef 不是 optional,Secret 必须先于引用它的 Deployment'
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
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because 'staging/prod 的 Schema.AutoMigrate=false,表没建出来 mail 会拒启,迁移必须先于 Deployment'
    Assert-True -Condition ($events -contains 'query:Job') -Because '放行必须来自 Job 查询结果'
    # 验签方只拿明文公钥 env(MMORPG_INTERNAL_AUTH_PUBLIC_KEYS,gate-online-identity.md §10.1),不建 gate-identity-auth Secret;未注入令牌时不建任何 Secret。
    Assert-True -Condition (-not ($events | Where-Object { $_ -like 'apply:Secret:*' })) -Because '未注入令牌且验签方改用明文公钥后,不应建任何 Secret'
    Assert-True -Condition ([Array]::LastIndexOf($events, 'delete:Job') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because '只替换本轮迁移Job，不删除运行实例仍引用的预置Secret'
    $content = $script:DeploymentContent
    Assert-Match -Text $content -Pattern '- name: MMORPG_INTERNAL_AUTH_PUBLIC_KEYS\s+value:' -Because '验签方通过明文 env 注入 gate 公钥列表'
    Assert-NotMatch -Text $content -Pattern 'gate-identity-auth' -Because '验签方不引用 gate 签名私钥 Secret'
    Assert-NotMatch -Text $content -Pattern 'PLACEHOLDER_' -Because '镜像、拉取策略与令牌指纹占位都必须被替换'
    Assert-NotMatch -Text $content -Pattern 'MMORPG_MAIL_ADMIN_TOKEN|mail-admin-token-hash|mail-admin-auth' -Because '历史M1口令不得恢复成第二条授权路径'
    foreach ($producer in @('ADMIN', 'RANK', 'GUILD', 'BATTLE')) {
        $key = $producer.ToLowerInvariant()
        Assert-Match -Text $content -Pattern ("- name: MAIL_PRODUCER_SECRET_$producer\s+valueFrom:\s+secretKeyRef: \{name: domain-service-secrets, key: mail-producer-$key, optional: false\}") -Because '每个生产者只引用自己的必需密钥，缺失时拒启'
    }
    Assert-Match -Text $content -Pattern '(?m)^  replicas: 2\s*$' -Because 'MySQL / Redis 连接总账按两副本计'
    Assert-Match -Text $content -Pattern 'name: mail-pdb' -Because 'PDB 随主 manifest 一起 apply'
}

# 托管内部密钥表($script:ManagedDomainServiceSecrets)与 Go 服务目录($GoSvcCatalogue)都是脚本级赋值:按 AST 取右边的字面量求值,不执行脚本入口。
function Get-DeployScriptLiteral([string]$Left) {
    $assign = $deployAst.Find({ param($a) $a -is [Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -eq $Left }, $false)
    if ($null -eq $assign) { throw "部署脚本缺少 $Left" }
    return & ([scriptblock]::Create($assign.Right.Extent.Text))
}
function Get-ManagedDomainServiceSecretTable { return @(Get-DeployScriptLiteral '$script:ManagedDomainServiceSecrets') }

# 每把一个独特的合格值(超过 32 字节)。
function Get-ManagedSecretFixtureValue([string]$Key) { "$Key-contract-value-0123456789abcdef" }

function ConvertTo-FixtureBase64([string]$Value) { [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($Value)) }

# 一组键各取独特值;-Set 改几把($null = 去掉这把)。
function Get-ManagedSecretFixtureValues([string[]]$Keys, [hashtable]$Set = @{}) {
    $values = @{}
    foreach ($key in $Keys) { $values[$key] = Get-ManagedSecretFixtureValue $key }
    foreach ($key in $Set.Keys) { if ($null -eq $Set[$key]) { $values.Remove($key) } else { $values[$key] = $Set[$key] } }
    return $values
}

# Read-DomainServiceSecretEntries 的结果形状;-Missing = 这个 namespace 没有这个 Secret。用前先恢复 New-DomainServiceSecretEntry。
function New-ManagedSecretState([hashtable]$Values = @{}, [switch]$Missing) {
    $entries = [ordered]@{}
    if ($Missing) { return [pscustomobject]@{ Exists = $false; Entries = $entries } }
    foreach ($key in $Values.Keys) { $entries[$key] = New-DomainServiceSecretEntry -Base64 (ConvertTo-FixtureBase64 ([string]$Values[$key])) }
    return [pscustomobject]@{ Exists = $true; Entries = $entries }
}

Test-Case '托管内部密钥的写入计划:缺的从 infra 复制、沿用各 zone 一致的值或生成,已有的沿用;值不同、过短、两把同值、infra 没有而各 zone 不一致都记错且不带值;-Resync 改成统一值,只重启读到改写键的 Pod' {
    foreach ($name in @('Get-ManagedDomainServiceSecretPlan', 'New-DomainServiceSecretEntry')) { Restore-ProductionFunction $name }
    $table = Get-ManagedDomainServiceSecretTable
    $keys = @('account-directory-payment', 'account-directory-login', 'player-store-scene')
    $infraNs = 'mmorpg-infra'
    $zoneA = 'mmorpg-zone-a'
    $zoneB = 'mmorpg-zone-b'
    $script:GeneratedCount = 0
    $newValue = { $script:GeneratedCount++; "generated-$($script:GeneratedCount)-0123456789abcdef0123456789abcdef" }
    $plan = {
        param($Infra, [System.Collections.IDictionary]$Zones, [bool]$Resync = $false)
        Get-ManagedDomainServiceSecretPlan -Table $table -Keys $keys -InfraNamespace $infraNs -Infra $Infra -Zones $Zones `
            -ScopeZones @($zoneA) -ZoneKeys $keys -Resync $Resync -NewValue $newValue
    }

    # 全新集群:infra 与 zone 都没有这个 Secret。每把生成一次,infra 存规范值,本次部署的 zone 拿同一份;没部署的 zone 不碰。
    $fresh = & $plan (New-ManagedSecretState -Missing) ([ordered]@{ $zoneA = (New-ManagedSecretState -Missing); $zoneB = (New-ManagedSecretState -Missing) })
    Assert-Equal -Expected 0 -Actual $fresh.Errors.Count -Because "全新集群没有可冲突的值(实际:$($fresh.Errors -join ' | '))"
    Assert-Equal -Expected "$infraNs,$zoneA" -Actual (@($fresh.Writes.Keys) -join ',') -Because '只写 infra 与本次部署的 zone'
    foreach ($key in $keys) {
        Assert-True -Condition ($fresh.Writes[$infraNs][$key] -ceq $fresh.Writes[$zoneA][$key]) -Because "$key 在 infra 与 zone 里是同一个值"
    }
    Assert-Equal -Expected 3 -Actual @($keys | ForEach-Object { $fresh.Writes[$infraNs][$_] } | Select-Object -Unique).Count -Because '每把各生成一个值,互不相同'
    Assert-Equal -Expected 3 -Actual $script:GeneratedCount -Because '只生成缺的三把'
    Assert-Equal -Expected 0 -Actual $fresh.Restarts.Count -Because '只补缺不用重启:缺键的 Pod 本来就起不来,键到了 kubelet 自己重试'
    Assert-True -Condition (-not $fresh.Exists[$infraNs] -and -not $fresh.Exists[$zoneA]) -Because '没有这个 Secret 的 namespace 记下来,写入时 create'

    # infra 齐全:zone 缺的原样复制 infra 的原文;已有的不动;没部署的 zone 缺键不补。
    $infraFull = New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys)
    $copy = & $plan $infraFull ([ordered]@{
            $zoneA = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys -Set @{ 'account-directory-login' = $null }))
            $zoneB = (New-ManagedSecretState -Missing) })
    Assert-Equal -Expected 0 -Actual $copy.Errors.Count -Because "只缺不冲突(实际:$($copy.Errors -join ' | '))"
    Assert-Equal -Expected $zoneA -Actual (@($copy.Writes.Keys) -join ',') -Because 'infra 齐全不写;没部署的 zone 不碰'
    Assert-Equal -Expected 'account-directory-login' -Actual (@($copy.Writes[$zoneA].Keys) -join ',') -Because '只补缺的那把'
    Assert-Equal -Expected (ConvertTo-FixtureBase64 (Get-ManagedSecretFixtureValue 'account-directory-login')) -Actual $copy.Writes[$zoneA]['account-directory-login'] -Because '原样复制 infra 的原文'

    # infra 没有、各 zone 一致:沿用 zone 的值写进 infra,不生成。
    $script:GeneratedCount = 0
    $adopt = & $plan (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys -Set @{ 'player-store-scene' = $null })) ([ordered]@{
            $zoneA = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys))
            $zoneB = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys)) })
    Assert-Equal -Expected 0 -Actual $adopt.Errors.Count -Because "各 zone 一致就沿用(实际:$($adopt.Errors -join ' | '))"
    Assert-Equal -Expected (ConvertTo-FixtureBase64 (Get-ManagedSecretFixtureValue 'player-store-scene')) -Actual $adopt.Writes[$infraNs]['player-store-scene'] -Because 'infra 存的规范值就是各 zone 那个'
    Assert-Equal -Expected 0 -Actual $script:GeneratedCount -Because '沿用不生成'

    # 判不了的四种:报错点键名与 namespace,不带值;-Resync 改成统一值,只在改写了已有值的 namespace 重启读到这些键的工作负载;
    # 只改写了 zone 的共享键而 infra 没动时,把 infra 侧消费该键的服务记进 InfraManualRestarts(提醒运维换值时按需手工 rollout restart)。
    $cases = @(
        @{ Why = '没部署的 zone 值不同'; Infra = $infraFull
            Zones = [ordered]@{
                $zoneA = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys))
                $zoneB = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys -Set @{ 'account-directory-payment' = 'zone-b-payment-value-0123456789abcdef' })) }
            Pattern = 'account-directory-payment in namespace mmorpg-zone-b differs from the cluster-wide value'
            Restarts = @{ 'mmorpg-zone-b' = 'data-service' }; InfraManual = 'payment' }
        @{ Why = 'infra 那份过短'; Infra = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys -Set @{ 'account-directory-payment' = 'too-short' }))
            Zones = [ordered]@{ $zoneA = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys)) }
            Pattern = 'account-directory-payment in namespace mmorpg-infra is shorter than 32 bytes'
            Restarts = @{ 'mmorpg-infra' = 'payment,payment-canary'; 'mmorpg-zone-a' = 'data-service' }; InfraManual = '' }
        @{ Why = '两把同值'; Infra = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys -Set @{ 'player-store-scene' = (Get-ManagedSecretFixtureValue 'account-directory-login') }))
            Zones = [ordered]@{ $zoneA = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys -Set @{ 'player-store-scene' = (Get-ManagedSecretFixtureValue 'account-directory-login') })) }
            Pattern = 'account-directory-login and player-store-scene hold the same value'
            Restarts = @{ 'mmorpg-zone-a' = 'scene,scene-world,scene-instance,db' }; InfraManual = '' }
        @{ Why = 'infra 没有而各 zone 不一致'; Infra = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys -Set @{ 'account-directory-login' = $null }))
            Zones = [ordered]@{
                $zoneA = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys))
                $zoneB = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $keys -Set @{ 'account-directory-login' = 'zone-b-login-value-0123456789abcdef' })) }
            Pattern = 'account-directory-login is missing in namespace mmorpg-infra and the zone namespaces hold different or short values'
            Restarts = @{ 'mmorpg-zone-a' = 'login,data-service'; 'mmorpg-zone-b' = 'login,data-service' }; InfraManual = '' }
    )
    foreach ($case in $cases) {
        $refused = & $plan $case.Infra $case.Zones
        $errorText = $refused.Errors -join "`n"
        Assert-Equal -Expected 1 -Actual $refused.Errors.Count -Because "$($case.Why):记一条错(实际:$errorText)"
        Assert-Match -Text $errorText -Pattern $case.Pattern -Because "$($case.Why):点名键与 namespace"
        $leaked = @($keys | Where-Object { $errorText.Contains((Get-ManagedSecretFixtureValue $_)) }).Count -gt 0 -or $errorText.Contains('too-short') -or $errorText.Contains('zone-b-')
        Assert-True -Condition (-not $leaked) -Because "$($case.Why):报错里不带值"
        $resynced = & $plan $case.Infra $case.Zones $true
        Assert-Equal -Expected 0 -Actual $resynced.Errors.Count -Because "$($case.Why):-Resync 之后没有错(实际:$($resynced.Errors -join ' | '))"
        Assert-Equal -Expected (@($case.Restarts.Keys | Sort-Object) -join ',') -Actual (@($resynced.Restarts.Keys | Sort-Object) -join ',') -Because "$($case.Why):只在改写了已有值的 namespace 重启"
        foreach ($ns in $case.Restarts.Keys) {
            Assert-Equal -Expected $case.Restarts[$ns] -Actual (@($resynced.Restarts[$ns]) -join ',') -Because "$($case.Why):$ns 只重启读到改写键的工作负载(scene 是三种场景节点,infra 的服务带上金丝雀)"
        }
        Assert-Equal -Expected $case.InfraManual -Actual (@($resynced.InfraManualRestarts) -join ',') -Because "$($case.Why):InfraManualRestarts 只记 infra 未改写但 zone 改写了共享键的服务"
        foreach ($key in $keys) {
            $written = @($resynced.Writes.Keys | ForEach-Object { $resynced.Writes[$_][$key] } | Where-Object { $_ } | Select-Object -Unique)
            Assert-True -Condition ($written.Count -le 1) -Because "$($case.Why):$key 写出去的只有一个值"
        }
    }

    # 去重边界:同一个 infra 服务(mail)因另一把键(role-fence-mail)已进入 Restarts[infra],则 mail-producer-battle 只改了 zone 也不再重复进 InfraManualRestarts。
    $mailKeys = @('role-fence-mail', 'mail-producer-battle')
    $mailDedup = Get-ManagedDomainServiceSecretPlan -Table $table -Keys $mailKeys -InfraNamespace $infraNs `
        -Infra (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $mailKeys -Set @{ 'role-fence-mail' = 'too-short' })) `
        -Zones ([ordered]@{ $zoneA = (New-ManagedSecretState -Values (Get-ManagedSecretFixtureValues $mailKeys -Set @{ 'mail-producer-battle' = 'zone-a-mail-battle-0123456789abcdef' })) }) `
        -ScopeZones @($zoneA) -ZoneKeys $mailKeys -Resync $true -NewValue $newValue
    Assert-Equal -Expected 'mail,mail-canary' -Actual (@($mailDedup.Restarts[$infraNs]) -join ',') -Because 'infra 改写了 role-fence-mail,已自动重启 mail 与 mail-canary'
    Assert-Equal -Expected '' -Actual (@($mailDedup.InfraManualRestarts) -join ',') -Because 'mail 已在自动重启名单里,不再重复要求手工重启'
}

Test-Case '托管内部密钥的门禁:DryRun 只打一行、不连集群;真跑时先列 namespace 再逐个读 infra 与 <前缀>-* 的 Secret(不调 get secrets --all-namespaces、不读别的前缀),判不了就在任何写操作之前拒绝且不带值,判得了才存下计划;全新集群用脚本自己的生成器' {
    foreach ($name in @('Initialize-ManagedDomainServiceSecrets', 'Get-ManagedDomainServiceSecretPlan', 'Read-DomainServiceSecretEntries', 'New-DomainServiceSecretEntry',
            'Get-ZoneNamespace', 'Test-GlobalGoSvcDeployed', 'Test-ZoneDbDeployed', 'Resolve-ZonesConfigPath', 'Get-ZonesFromJson')) {
        Restore-ProductionFunction $name
    }
    $script:ManagedDomainServiceSecrets = Get-ManagedDomainServiceSecretTable
    $allKeys = @($script:ManagedDomainServiceSecrets | ForEach-Object { $_.Key })
    $sceneKeys = @($script:ManagedDomainServiceSecrets | Where-Object { $_.ZoneConsumers -contains 'scene' } | ForEach-Object { $_.Key })
    $infraKeys = @($script:ManagedDomainServiceSecrets | Where-Object { @($_.InfraConsumers).Count -gt 0 } | ForEach-Object { $_.Key })
    Assert-Equal -Expected 16 -Actual $allKeys.Count -Because 'zone 工作负载引用的 16 把'
    Assert-Equal -Expected 'mail-producer-battle,account-directory-trade,account-directory-scene,player-store-scene' -Actual ($sceneKeys -join ',') -Because 'scene 读的四把:只发 C++ 节点时也要维护'

    # 线上状态:namespace → 这个 Secret 的 data(键 → base64)。Read-DomainServiceSecretEntries 先列 namespace,再逐个按 namespace 读
    # domain-service-secrets;$script:SecretStubRawStdout 不为空时在读取目标 namespace 时原样当作 kubectl 的输出。
    $script:SecretStubCalls = [System.Collections.Generic.List[string]]::new()
    $script:SecretStubCluster = [ordered]@{}
    $script:SecretStubNsExitCode = 0
    $script:SecretStubExitCode = 0
    $script:SecretStubRawStdout = $null
    $savedListFunction = (Get-Command -Name Invoke-ClientEntryKubectl -CommandType Function -ErrorAction SilentlyContinue).ScriptBlock
    Set-Item -Path 'Function:script:Invoke-ClientEntryKubectl' -Value {
        param([string[]]$KubectlArgs, [AllowEmptyString()][string]$KubeContext, [AllowEmptyString()][string]$KubeConfig, [switch]$AllowFailure, [switch]$CaptureOutput)
        $line = $KubectlArgs -join ' '
        $script:SecretStubCalls.Add($line)
        if ($line -ceq 'get namespaces -o name') {
            $stdout = (@($script:SecretStubCluster.Keys | ForEach-Object { "namespace/$_" }) -join "`n")
            $stderr = if ($script:SecretStubNsExitCode -ne 0) { 'Error from server (Forbidden)' } else { '' }
            return [pscustomobject]@{ ExitCode = $script:SecretStubNsExitCode; Stdout = $stdout; Stderr = $stderr }
        }
        if ($line -cmatch '^get secret domain-service-secrets -n (\S+) --ignore-not-found -o json$') {
            $ns = $Matches[1]
            if ($script:SecretStubExitCode -ne 0) {
                return [pscustomobject]@{ ExitCode = $script:SecretStubExitCode; Stdout = ''; Stderr = 'Error from server (Forbidden)' }
            }
            if ($null -ne $script:SecretStubRawStdout) {
                return [pscustomobject]@{ ExitCode = 0; Stdout = $script:SecretStubRawStdout; Stderr = '' }
            }
            if (-not $script:SecretStubCluster.Contains($ns) -or $null -eq $script:SecretStubCluster[$ns]) {
                return [pscustomobject]@{ ExitCode = 0; Stdout = ''; Stderr = '' }
            }
            $json = ConvertTo-Json -InputObject ([ordered]@{ metadata = [ordered]@{ name = 'domain-service-secrets'; namespace = $ns }; data = $script:SecretStubCluster[$ns] }) -Depth 6 -Compress
            return [pscustomobject]@{ ExitCode = 0; Stdout = $json; Stderr = '' }
        }
        throw "Unexpected kubectl call in managed secret gate test: $line"
    }
    $secretData = {
        param([hashtable]$Set = @{}, [hashtable]$Raw = @{})
        $data = [ordered]@{ 'other-key' = 'eA==' }
        foreach ($key in $allKeys) { $data[$key] = ConvertTo-FixtureBase64 (Get-ManagedSecretFixtureValue $key) }
        foreach ($key in $Set.Keys) { $data[$key] = ConvertTo-FixtureBase64 ([string]$Set[$key]) }
        foreach ($key in $Raw.Keys) { $data[$key] = $Raw[$key] }
        return $data
    }
    $thrownBy = { param([scriptblock]$Action) try { & $Action 6>&1 | Out-Null; return '' } catch { return $_.Exception.Message } }
    # zones.json 只决定 all-up 部署哪些 zone;比对范围看集群。
    $zonesFile = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-managed-secrets-zones-' + [guid]::NewGuid().ToString('N') + '.json')
    Set-Content -LiteralPath $zonesFile -Encoding utf8 -Value '{"zones":[{"name":"alpha","zoneId":101,"replicas":{"centre":1,"gate":1,"scene":1}},{"name":"beta","zoneId":102,"replicas":{"centre":1,"gate":1,"scene":1}}]}'
    # Reset-MigrateFixture 与其它用例共用这些脚本变量,这里改完必须原样还原。
    $wiringNames = @('Command', 'ZoneName', 'InfraNamespace', 'NamespacePrefix', 'SkipInfra', 'SkipGoSvc', 'GoSvcRegistry', 'DryRun', 'ZonesConfigPath', 'ResyncDomainServiceSecrets')
    $wiringBefore = @{}
    foreach ($v in $wiringNames) { $wiringBefore[$v] = Get-Variable -Name $v -Scope Script -ValueOnly -ErrorAction SilentlyContinue }
    $script:NamespacePrefix = 'mmorpg-zone'
    $script:InfraNamespace = 'mmorpg-infra'
    $script:ZoneName = 'alpha'
    $script:SkipInfra = $false
    $script:SkipGoSvc = $false
    $script:GoSvcRegistry = 'registry.example'
    $script:ZonesConfigPath = $zonesFile
    $script:ResyncDomainServiceSecrets = [switch]$false
    try {
        $across = ', with one value per key across namespace mmorpg-infra and every mmorpg-zone-\* namespace'
        $script:DryRun = $true
        $script:Command = 'zone-up'
        $printed = (Initialize-ManagedDomainServiceSecrets 6>&1 | Out-String)
        Assert-Match -Text $printed -Pattern ('in namespace\(s\) mmorpg-infra, mmorpg-zone-alpha holds ' + [regex]::Escape($allKeys -join ', ') + $across) -Because 'zone-up 部署 zone 的 Go 服务:全表,写到 infra 与本 zone'
        $script:SkipGoSvc = $true
        $printed = (Initialize-ManagedDomainServiceSecrets 6>&1 | Out-String)
        Assert-Match -Text $printed -Pattern ('in namespace\(s\) mmorpg-infra, mmorpg-zone-alpha holds ' + [regex]::Escape($sceneKeys -join ', ') + $across) -Because '只发 C++ 节点:只维护 scene 读的四把'
        $script:SkipGoSvc = $false
        $script:Command = 'all-up'
        $printed = (Initialize-ManagedDomainServiceSecrets 6>&1 | Out-String)
        Assert-Match -Text $printed -Pattern ('in namespace\(s\) mmorpg-infra, mmorpg-zone-alpha, mmorpg-zone-beta holds ' + [regex]::Escape($allKeys -join ', ') + $across) -Because 'all-up:zones.json 里每个 zone 都要有全表'
        $script:Command = 'infra-up'
        $printed = (Initialize-ManagedDomainServiceSecrets 6>&1 | Out-String)
        Assert-Match -Text $printed -Pattern ('in namespace\(s\) mmorpg-infra holds ' + [regex]::Escape($infraKeys -join ', ') + $across) -Because 'infra-up:只维护 infra 侧有用者的那些'
        $script:SkipGoSvc = $true
        $printed = (Initialize-ManagedDomainServiceSecrets 6>&1 | Out-String)
        Assert-Equal -Expected '' -Actual ([string]$printed).Trim() -Because 'infra-up 不部署全局 Go 服务就什么都不维护'
        Assert-Equal -Expected 0 -Actual $script:SecretStubCalls.Count -Because 'DryRun 一次 kubectl 都不调'
        Assert-True -Condition ($null -eq $script:ManagedDomainServiceSecretPlan) -Because 'DryRun 不存计划'

        # 真跑:infra、本 zone、zones.json 里本次没部署的 beta、zones.json 也没列的 gamma 都一致,放行;另一套环境(别的前缀)连 get secret 都不调。
        $script:DryRun = $false
        $script:SkipGoSvc = $false
        $script:Command = 'zone-up'
        $script:SecretStubCluster = [ordered]@{
            'mmorpg-infra'             = (& $secretData)
            'mmorpg-zone-alpha'        = (& $secretData)
            'mmorpg-zone-beta'         = (& $secretData)
            'mmorpg-zone-gamma'        = (& $secretData)
            'mmorpg-stress-zone-alpha' = (& $secretData -Set @{ 'role-fence-guild' = 'stress-guild-value-0123456789abcdef' })
        }
        $printed = (Initialize-ManagedDomainServiceSecrets 6>&1 | Out-String)
        Assert-Match -Text $printed -Pattern 'agree across namespace mmorpg-infra and 3 zone namespace\(s\); to write: nothing' -Because 'alpha、beta、gamma 都比了(gamma 不在 zones.json 里),别的前缀不算'
        $expectedCalls = @(
            'get namespaces -o name'
            'get secret domain-service-secrets -n mmorpg-infra --ignore-not-found -o json'
            'get secret domain-service-secrets -n mmorpg-zone-alpha --ignore-not-found -o json'
            'get secret domain-service-secrets -n mmorpg-zone-beta --ignore-not-found -o json'
            'get secret domain-service-secrets -n mmorpg-zone-gamma --ignore-not-found -o json'
        ) -join '|'
        Assert-Equal -Expected $expectedCalls -Actual ($script:SecretStubCalls -join '|') -Because '先列 namespace 再逐个读本环境的 Secret:不要求全集群 list secrets,不碰其它前缀的 namespace'
        Assert-Equal -Expected 0 -Actual $script:ManagedDomainServiceSecretPlan.Writes.Count -Because '都齐、都一致:没有要写的'

        # 读失败(列 namespace 或读单个 Secret)、输出不是 JSON、base64 不合法:拒绝,点名 namespace 与键,不带值。
        $script:SecretStubCluster['mmorpg-zone-gamma'] = (& $secretData -Raw @{ 'account-directory-scene' = '%%%' })
        $message = & $thrownBy { Initialize-ManagedDomainServiceSecrets }
        Assert-Match -Text $message -Pattern 'The key account-directory-scene in namespace mmorpg-zone-gamma is not valid base64\. Nothing was written\.' -Because 'base64 不合法时拒绝并点名键'
        $script:SecretStubCluster['mmorpg-zone-gamma'] = (& $secretData)
        $script:SecretStubNsExitCode = 1
        $message = & $thrownBy { Initialize-ManagedDomainServiceSecrets }
        Assert-Match -Text $message -Pattern 'Cannot list namespaces to check Secret domain-service-secrets \(kubectl exit code 1\): Error from server \(Forbidden\)\. Nothing was written\.' -Because '列 namespace 失败时拒绝'
        $script:SecretStubNsExitCode = 0
        $script:SecretStubExitCode = 1
        $message = & $thrownBy { Initialize-ManagedDomainServiceSecrets }
        Assert-Match -Text $message -Pattern 'Cannot read Secret domain-service-secrets in namespace mmorpg-infra \(kubectl exit code 1\): Error from server \(Forbidden\)\. Nothing was written\.' -Because '读单个 namespace 的 Secret 失败时拒绝:读不到不等于没有'
        $script:SecretStubExitCode = 0
        $script:SecretStubRawStdout = '{"metadata":{"namespace":"mmorpg-infra"},"data":{"role-fence-guild":"c2VjcmV0LXZhbHVl'
        $message = & $thrownBy { Initialize-ManagedDomainServiceSecrets }
        Assert-Match -Text $message -Pattern 'The kubectl output for Secret domain-service-secrets in namespace mmorpg-infra is not valid JSON\. Nothing was written\.$' -Because '输出不是 JSON 时拒绝'
        Assert-True -Condition (-not $message.Contains('c2VjcmV0')) -Because '解析错误不带 Secret 的片段'
        $script:SecretStubRawStdout = $null

        # gamma(本次没部署、zones.json 也没列)的值不同:在任何写操作之前拒绝,点名键与 namespace、不带值,也不存计划;
        # -ResyncDomainServiceSecrets 放行并记下改写、重启与 infra 手工重启提示,另一套环境的 namespace 仍不碰。
        $script:SecretStubCluster['mmorpg-zone-gamma'] = (& $secretData -Set @{ 'role-fence-guild' = 'gamma-guild-value-0123456789abcdef' })
        $message = & $thrownBy { Initialize-ManagedDomainServiceSecrets }
        Assert-Match -Text $message -Pattern 'domain-service-secrets check failed \(1 problem\(s\)\); nothing was written\. Rerun with -ResyncDomainServiceSecrets' -Because '在任何写操作之前拒绝,并写明怎么修'
        Assert-Match -Text $message -Pattern 'role-fence-guild in namespace mmorpg-zone-gamma differs' -Because '点名键与 namespace'
        Assert-True -Condition (-not $message.Contains('gamma-guild-value') -and -not $message.Contains((Get-ManagedSecretFixtureValue 'role-fence-guild'))) -Because '报错里不带值'
        Assert-True -Condition ($null -eq $script:ManagedDomainServiceSecretPlan) -Because '拒绝时不存计划,后面的写入步骤什么都不做'
        $script:ResyncDomainServiceSecrets = [switch]$true
        $message = & $thrownBy { Initialize-ManagedDomainServiceSecrets }
        Assert-Equal -Expected '' -Actual $message -Because '-ResyncDomainServiceSecrets 放行'
        Assert-Equal -Expected 'mmorpg-zone-gamma' -Actual (@($script:ManagedDomainServiceSecretPlan.Writes.Keys) -join ',') -Because '只改 gamma;另一套环境的 namespace 不碰'
        Assert-Equal -Expected 'role-fence-guild' -Actual (@($script:ManagedDomainServiceSecretPlan.Writes['mmorpg-zone-gamma'].Keys) -join ',') -Because 'gamma 只改那一把'
        Assert-Equal -Expected 'data-service' -Actual (@($script:ManagedDomainServiceSecretPlan.Restarts['mmorpg-zone-gamma']) -join ',') -Because '改写了 role-fence-guild,重启读它的 data-service'
        Assert-Equal -Expected 'guild' -Actual (@($script:ManagedDomainServiceSecretPlan.InfraManualRestarts) -join ',') -Because 'infra 没改写、只改了 zone 的 role-fence-guild:记下 infra 侧读它的 guild'
        $script:ResyncDomainServiceSecrets = [switch]$false

        # 全新集群:哪个 namespace 都没有这个 Secret。用部署脚本自己的生成器:每把 32 字节随机数的 64 位小写十六进制,互不相同。
        $script:SecretStubCluster = [ordered]@{}
        $message = & $thrownBy { Initialize-ManagedDomainServiceSecrets }
        Assert-Equal -Expected '' -Actual $message -Because '全新集群:放行并生成'
        $fresh = $script:ManagedDomainServiceSecretPlan
        Assert-Equal -Expected 'mmorpg-infra,mmorpg-zone-alpha' -Actual (@($fresh.Writes.Keys | Sort-Object) -join ',') -Because '写 infra 与本次部署的 zone'
        $generated = @($allKeys | ForEach-Object { [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($fresh.Writes['mmorpg-infra'][$_])) })
        Assert-Equal -Expected 16 -Actual @($generated | Where-Object { $_ -cmatch '^[0-9a-f]{64}$' }).Count -Because '生成的是 32 字节随机数的 64 位小写十六进制'
        Assert-Equal -Expected 16 -Actual @($generated | Select-Object -Unique).Count -Because '每把各不相同'
        Assert-True -Condition (-not $fresh.Exists['mmorpg-infra'] -and -not $fresh.Exists['mmorpg-zone-alpha']) -Because '没有这个 Secret:写入时 create'
    }
    finally {
        if ($savedListFunction) { Set-Item -Path 'Function:script:Invoke-ClientEntryKubectl' -Value $savedListFunction } else { Remove-Item -Path 'Function:script:Invoke-ClientEntryKubectl' -ErrorAction SilentlyContinue }
        Remove-Item -LiteralPath $zonesFile -Force -ErrorAction SilentlyContinue
        $script:ManagedDomainServiceSecretPlan = $null
        $script:SecretStubRawStdout = $null
        $script:SecretStubNsExitCode = 0
        foreach ($v in $wiringNames) { Set-Variable -Name $v -Scope Script -Value $wiringBefore[$v] }
    }
}

Test-Case '托管内部密钥的写入:已有 Secret 用 merge patch 只增改计划里的键(值经临时文件,用完即删),没有的先建 namespace 再 create;改写过的 namespace 按 StatefulSet / Deployment 分别滚动重启(不强杀 Pod),Agones scene Fleet 告警提示 k8s_scene_roll.ps1;infra 未改写但 zone 改了共享键时告警提示手工 rollout restart;执行一次后清计划' {
    foreach ($name in @('Sync-ManagedDomainServiceSecrets', 'Get-WorkloadKind')) { Restore-ProductionFunction $name }
    $script:SyncKubectl = [System.Collections.Generic.List[string]]::new()
    $script:SyncPatchFiles = [System.Collections.Generic.List[string]]::new()
    $script:SyncPatchContents = [System.Collections.Generic.List[string]]::new()
    $script:SyncCreates = [System.Collections.Generic.List[object]]::new()
    $script:SyncNamespaces = [System.Collections.Generic.List[string]]::new()
    $script:SyncExistingWorkloads = @{}
    $stubbed = @('Invoke-Kubectl', 'Invoke-KubectlWithInputFile', 'Ensure-Namespace', 'Invoke-ClientEntryKubectl')
    $saved = @{}
    foreach ($name in $stubbed) { $saved[$name] = (Get-Command -Name $name -CommandType Function -ErrorAction SilentlyContinue).ScriptBlock }
    Set-Item -Path 'Function:script:Invoke-Kubectl' -Value {
        param([string[]]$Args, [switch]$AllowFailure)
        $script:SyncKubectl.Add($Args -join ' ')
        $at = [array]::IndexOf($Args, '--patch-file')
        if ($at -ge 0) {
            $script:SyncPatchFiles.Add($Args[$at + 1])
            $script:SyncPatchContents.Add([IO.File]::ReadAllText($Args[$at + 1]))
        }
    }
    Set-Item -Path 'Function:script:Invoke-KubectlWithInputFile' -Value {
        param([string[]]$Args, [string]$InputContent)
        $script:SyncCreates.Add([pscustomobject]@{ Args = ($Args -join ' '); Content = $InputContent })
    }
    Set-Item -Path 'Function:script:Ensure-Namespace' -Value { param([string]$Namespace) $script:SyncNamespaces.Add($Namespace) }
    Set-Item -Path 'Function:script:Invoke-ClientEntryKubectl' -Value {
        param([string[]]$KubectlArgs, [AllowEmptyString()][string]$KubeContext, [AllowEmptyString()][string]$KubeConfig, [switch]$AllowFailure, [switch]$CaptureOutput)
        $line = $KubectlArgs -join ' '
        if ($line -cmatch '^get (statefulset|deployment) -n (\S+) -o name$') {
            $lookup = "$($Matches[2]):$($Matches[1])"
            $names = @($script:SyncExistingWorkloads[$lookup])
            $stdout = ($names -join "`n")
            return [pscustomobject]@{ ExitCode = 0; Stdout = $stdout; Stderr = '' }
        }
        throw "Unexpected kubectl call in managed secret sync test: $line"
    }
    $savedDryRun = $script:DryRun
    $savedInfraNs = $script:InfraNamespace
    $savedSqlWriters = $script:SqlWriterGoSvcs
    $script:InfraNamespace = 'mmorpg-infra'
    $script:SqlWriterGoSvcs = @(Get-DeployScriptLiteral '$script:SqlWriterGoSvcs')
    try {
        # Deployment 形态的 scene-world / scene-instance + StatefulSet 的 login / data-service,附带 infra 手工重启提示。
        $script:SyncExistingWorkloads = @{
            'mmorpg-zone-a:statefulset' = @('statefulset.apps/login', 'statefulset.apps/data-service')
            'mmorpg-zone-a:deployment'  = @('deployment.apps/scene-world', 'deployment.apps/scene-instance')
        }
        $planFixture = [pscustomobject]@{
            Errors              = [string[]]@()
            Writes              = [ordered]@{ 'mmorpg-infra' = [ordered]@{ 'account-directory-payment' = 'cGF5bWVudA==' }; 'mmorpg-zone-a' = [ordered]@{ 'account-directory-login' = 'bG9naW4='; 'role-fence-db' = 'ZGI=' } }
            Restarts            = [ordered]@{ 'mmorpg-zone-a' = [System.Collections.Generic.List[string]]@('scene', 'scene-world', 'scene-instance', 'login', 'data-service') }
            InfraManualRestarts = [string[]]@('payment', 'trade')
            Exists              = [ordered]@{ 'mmorpg-infra' = $true; 'mmorpg-zone-a' = $false }
        }
        $script:ManagedDomainServiceSecretPlan = $planFixture
        $script:DryRun = $true
        Sync-ManagedDomainServiceSecrets 6>&1 3>&1 | Out-Null
        Assert-Equal -Expected 0 -Actual ($script:SyncKubectl.Count + $script:SyncCreates.Count + $script:SyncNamespaces.Count) -Because 'DryRun 什么都不写(门禁那一步已经打过说明)'

        $script:DryRun = $false
        $warnings = [System.Collections.Generic.List[string]]::new()
        Sync-ManagedDomainServiceSecrets 6>&1 3>&1 | ForEach-Object {
            if ($_ -is [System.Management.Automation.WarningRecord]) { $warnings.Add($_.Message) }
        }
        Assert-Match -Text $script:SyncKubectl[0] -Pattern '^patch secret domain-service-secrets -n mmorpg-infra --type merge --patch-file .+$' -Because '已有的 Secret 用 merge patch:只增改计划里的键,不删别的键'
        Assert-Equal -Expected '{"data":{"account-directory-payment":"cGF5bWVudA=="}}' -Actual $script:SyncPatchContents[0] -Because 'patch 里只有计划里的键'
        Assert-True -Condition (-not (Test-Path -LiteralPath $script:SyncPatchFiles[0])) -Because '临时文件用完即删'
        Assert-Equal -Expected 'mmorpg-zone-a' -Actual ($script:SyncNamespaces -join ',') -Because '没有 Secret 的 namespace 先建 namespace(新 zone 第一次部署)'
        Assert-Equal -Expected 'create -n mmorpg-zone-a' -Actual $script:SyncCreates[0].Args -Because '没有的 Secret 用 create'
        foreach ($line in @('kind: Secret', '  name: domain-service-secrets', '  account-directory-login: bG9naW4=', '  role-fence-db: ZGI=')) {
            Assert-True -Condition ($script:SyncCreates[0].Content -split "`n" -ccontains $line) -Because "create 的 Secret 里有这一行:$line"
        }
        Assert-Equal -Expected 'rollout restart -n mmorpg-zone-a statefulset/login statefulset/data-service' -Actual $script:SyncKubectl[1] -Because 'StatefulSet 先滚动重启(不强杀全部副本)'
        Assert-Equal -Expected 'rollout restart -n mmorpg-zone-a deployment/scene-world deployment/scene-instance' -Actual $script:SyncKubectl[2] -Because 'Deployment 只滚动实际存在的 scene-world / scene-instance,跳过不存在的 scene'
        Assert-Equal -Expected 3 -Actual $script:SyncKubectl.Count -Because '一个 patch、一个 StatefulSet rollout restart、一个 Deployment rollout restart;零 delete pod'
        Assert-Equal -Expected 1 -Actual $warnings.Count -Because 'Deployment 形态的 scene 已滚动,只打 infra 手工重启提示,不打 Agones Fleet 告警'
        Assert-Match -Text $warnings[0] -Pattern 'kubectl -n mmorpg-infra rollout restart statefulset/payment statefulset/trade' -Because '换值场景提醒运维手工重启 infra 侧读共享键的工作负载'

        # 一次运行只写一遍:再次调用是空操作(all-up 在 Apply-Infra 写过之后进 Apply-Zone 不重复写)。
        $script:SyncKubectl.Clear()
        Sync-ManagedDomainServiceSecrets 6>&1 3>&1 | Out-Null
        Assert-Equal -Expected 0 -Actual $script:SyncKubectl.Count -Because '执行一次后计划已清空,重复调用不写不重启'

        # Agones scene Fleet 形态:本 namespace 没有 scene* Deployment 时不强杀 GameServer Pod,改为告警提示 k8s_scene_roll.ps1 -Restart。
        $script:SyncExistingWorkloads = @{
            'mmorpg-zone-b:statefulset' = @('statefulset.apps/db')
            'mmorpg-zone-b:deployment'  = @('deployment.apps/scene-manager')
        }
        $script:ManagedDomainServiceSecretPlan = [pscustomobject]@{
            Errors              = [string[]]@()
            Writes              = [ordered]@{}
            Restarts            = [ordered]@{ 'mmorpg-zone-b' = [System.Collections.Generic.List[string]]@('scene', 'scene-world', 'scene-instance', 'db', 'scene-manager') }
            InfraManualRestarts = [string[]]@()
            Exists              = [ordered]@{ 'mmorpg-zone-b' = $true }
        }
        $fleetWarnings = [System.Collections.Generic.List[string]]::new()
        Sync-ManagedDomainServiceSecrets 6>&1 3>&1 | ForEach-Object {
            if ($_ -is [System.Management.Automation.WarningRecord]) { $fleetWarnings.Add($_.Message) }
        }
        Assert-Equal -Expected 'rollout restart -n mmorpg-zone-b statefulset/db|rollout restart -n mmorpg-zone-b deployment/scene-manager' -Actual ($script:SyncKubectl -join '|') -Because 'Agones Fleet 下只滚动 db 与 scene-manager,绝不 delete pod 强踢 GameServer'
        Assert-Equal -Expected 1 -Actual $fleetWarnings.Count -Because '没有 scene* Deployment 时打一条 Agones Fleet 排空替换告警'
        Assert-Match -Text $fleetWarnings[0] -Pattern 'k8s_scene_roll\.ps1 -Namespace mmorpg-zone-b -Fleet <scene\|scene-world\|scene-instance> -Restart' -Because '告警给出可直接执行的排空命令'
    }
    finally {
        foreach ($name in $stubbed) {
            if ($saved[$name]) { Set-Item -Path "Function:script:$name" -Value $saved[$name] } else { Remove-Item -Path "Function:script:$name" -ErrorAction SilentlyContinue }
        }
        $script:ManagedDomainServiceSecretPlan = $null
        $script:DryRun = $savedDryRun
        $script:InfraNamespace = $savedInfraNs
        $script:SqlWriterGoSvcs = $savedSqlWriters
    }
}

Test-Case '托管内部密钥的接入位置:只读门禁在写路径入口;写入不在 switch 之前抢写,而是落在 Apply-Infra 与 Apply-Zone 通过各自写前闸之后、任何工作负载之前' {
    $text = ([IO.File]::ReadAllText($script:DeployScript)) -replace "`r`n", "`n"
    $switchAt = $text.IndexOf("`nswitch (`$Command) {", [StringComparison]::Ordinal)
    $gateBlockAt = $text.IndexOf('if ($Command -in @("zone-up", "all-up", "infra-up")) {', [StringComparison]::Ordinal)
    $gateBlock = $text.Substring($gateBlockAt, $switchAt - $gateBlockAt)
    Assert-True -Condition ($gateBlock.Contains("`n`tInitialize-ManagedDomainServiceSecrets`n")) -Because '只读门禁在写路径入口的门禁块里'
    Assert-True -Condition (-not $gateBlock.Contains('Sync-ManagedDomainServiceSecrets')) -Because '写入不抢在 switch 之前:Apply-Infra / Apply-Zone 的写前闸被拒时不得动任何 Secret 或 Pod'

    $applyInfraBody = ($deployAst.Find({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Apply-Infra' }, $false)).Extent.Text -replace "`r`n", "`n"
    $infraGateAt = $applyInfraBody.IndexOf('Assert-WsEdgeTlsSecretPresent', [StringComparison]::Ordinal)
    $infraNsAt = $applyInfraBody.IndexOf('Ensure-Namespace -Namespace $InfraNamespace', [StringComparison]::Ordinal)
    $infraSyncAt = $applyInfraBody.IndexOf("`n`tSync-ManagedDomainServiceSecrets`n", [StringComparison]::Ordinal)
    $infraManifestAt = $applyInfraBody.IndexOf('$infraManifests = @("etcd.yaml")', [StringComparison]::Ordinal)
    Assert-True -Condition ($infraGateAt -gt 0 -and $infraNsAt -gt $infraGateAt -and $infraSyncAt -gt $infraNsAt -and $infraManifestAt -gt $infraSyncAt) -Because 'Apply-Infra:写前闸与 Ensure-Namespace 之后、infra 清单与全局服务之前写托管密钥'

    $applyZoneBody = ($deployAst.Find({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Apply-Zone' }, $false)).Extent.Text -replace "`r`n", "`n"
    $schemaNsAt = $applyZoneBody.IndexOf('Ensure-Namespace -Namespace $namespace', [StringComparison]::Ordinal)
    $schemaSyncAt = $applyZoneBody.IndexOf("`n`t`tSync-ManagedDomainServiceSecrets`n", [StringComparison]::Ordinal)
    $schemaApplyAt = $applyZoneBody.IndexOf('Apply-GoSvcManifests -Namespace $namespace', [StringComparison]::Ordinal)
    Assert-True -Condition ($schemaNsAt -gt 0 -and $schemaSyncAt -gt $schemaNsAt -and $schemaApplyAt -gt $schemaSyncAt) -Because 'Apply-Zone(-SqlSchemaOnly):Ensure-Namespace 之后、迁移 Job 之前写托管密钥'
    $zoneGateAt = $applyZoneBody.IndexOf('Remove-ClientEntryObsoleteResources', [StringComparison]::Ordinal)
    $zoneSyncAt = $applyZoneBody.IndexOf("`n`tSync-ManagedDomainServiceSecrets`n", [StringComparison]::Ordinal)
    $zoneCmAt = $applyZoneBody.IndexOf('New-NodeConfigMapYaml', [StringComparison]::Ordinal)
    Assert-True -Condition ($zoneGateAt -gt 0 -and $zoneSyncAt -gt $zoneGateAt -and $zoneCmAt -gt $zoneSyncAt) -Because 'Apply-Zone:名册、服务端预演与旧形态删除闸全部通过之后、工作负载之前写托管密钥'
}

Test-Case '托管内部密钥表与清单一致:恰好是 zone 工作负载以 optional: false 引用的全部键,每行的两侧用法与清单逐个对上' {
    Restore-ProductionFunction 'Get-SceneAccountDirectoryEnvLines'
    $table = Get-ManagedDomainServiceSecretTable
    $catalogue = Get-DeployScriptLiteral '$GoSvcCatalogue'
    $refsOf = {
        param([string]$Text)
        @([regex]::Matches($Text, 'secretKeyRef: \{name: domain-service-secrets, key: (?<k>[a-z0-9-]+), optional: false\}') | ForEach-Object { $_.Groups['k'].Value })
    }
    $manifestDir = Join-Path $script:RepoRoot 'deploy/k8s/manifests/go-svc'
    # 按 Go 服务目录分两侧:不带 Global 的随 zone 部署,再加 C++ scene(它的这几行由部署脚本生成,不在清单文件里);带 Global 的是全局池,在 infra。
    $zoneRefs = [ordered]@{}
    $infraRefs = [ordered]@{}
    foreach ($svc in @($catalogue.Keys | Sort-Object)) {
        $refs = @(& $refsOf ([IO.File]::ReadAllText((Join-Path $manifestDir $catalogue[$svc].Manifest))))
        if ($catalogue[$svc].Global) { $infraRefs[$svc] = $refs } else { $zoneRefs[$svc] = $refs }
    }
    $zoneRefs['scene'] = @(& $refsOf ((Get-SceneAccountDirectoryEnvLines -Indent '') -join "`n"))
    Assert-True -Condition ($zoneRefs.Contains('data-service') -and $infraRefs.Contains('payment')) -Because '服务目录取到了两侧的服务'
    $zoneKeys = @($zoneRefs.Values | ForEach-Object { $_ } | Sort-Object -Unique)
    Assert-Equal -Expected ($zoneKeys -join ',') -Actual (@($table | ForEach-Object { $_.Key } | Sort-Object) -join ',') `
        -Because 'zone 有多份副本、又被全局池跨 zone 调用:zone 引用的每把都要由部署维护;只在 infra 用的只有一份,不在表里'
    foreach ($row in $table) {
        $zoneUsers = @($zoneRefs.Keys | Where-Object { $zoneRefs[$_] -contains $row.Key } | Sort-Object)
        $infraUsers = @($infraRefs.Keys | Where-Object { $infraRefs[$_] -contains $row.Key } | Sort-Object)
        Assert-Equal -Expected ($zoneUsers -join ',') -Actual (@($row.ZoneConsumers | Sort-Object) -join ',') -Because "$($row.Key) 的 zone 侧用法与清单一致(决定只发 C++ 节点时维护不维护、resync 时重启谁)"
        Assert-Equal -Expected ($infraUsers -join ',') -Actual (@($row.InfraConsumers | Sort-Object) -join ',') -Because "$($row.Key) 的 infra 侧用法与清单一致"
    }
}

Test-Case 'mail 遗留运维令牌不覆盖现役生产者Secret,密钥值不进 manifest' {
    Reset-MigrateFixture
    $script:JobAbsentBeforeApply = $true
    $token = 'audit-only-mail-admin-token-0123456789abcdef'
    $script:MailAdminToken = $token
    Apply-OneGoSvc -SvcName 'mail' -Namespace audit-infra -CurrentZoneId 0 -CurrentClusterId 0
    $events = @($script:Events)
    Assert-True -Condition ($events -notcontains 'apply:Secret:mail-admin-auth') -Because '不创建已退出使用的历史M1口令Secret'
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because '预置Secret不能绕过正式迁移门禁'
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
        Assert-True -Condition ($script:Events -notcontains 'apply:Workload') -Because '失败后不得部署服务'
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
    # 资产通道关着时不建任何 Secret;内部身份验签改由明文公钥 env(MMORPG_INTERNAL_AUTH_PUBLIC_KEYS)注入(gate-online-identity.md §10.1)。
    Assert-True -Condition (-not ($script:Events | Where-Object { $_ -like 'apply:Secret:*' })) -Because '关着时不建资产签名或私钥 Secret'
    Assert-Match -Text $effectiveContent -Pattern '- name: MMORPG_INTERNAL_AUTH_PUBLIC_KEYS\s+value:' -Because '验签方通过明文 env 注入 gate 公钥列表'
    Assert-NotMatch -Text $effectiveContent -Pattern 'gate-identity-auth' -Because '验签方不引用 gate 签名私钥 Secret'
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
    Assert-True -Condition ($secretAt -lt [Array]::IndexOf($events, 'apply:Workload')) -Because 'secretKeyRef 不是 optional,Secret 必须先于 Deployment'
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
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($content, '(?m)^\s*- name: MMORPG_MATCH_INTERNAL_SECRET_GUILD\s*$').Count) -Because 'off不能删除常驻必需MMORPG_MATCH_INTERNAL_SECRET_GUILD'
    Assert-Match -Text $content -Pattern 'MMORPG_GUILD_SQL_DSN=\\"\$\(cat /app/etc/private/env/MMORPG_GUILD_SQL_DSN\)\\"' -Because 'off不能删除常驻必需的SQL连接串;它来自本boot选中的环境文件'
    Assert-True -Condition (-not ($script:Events | Where-Object { $_ -like 'apply:Secret:*' })) -Because 'off不创建可选资产或私钥Secret'
    Assert-Match -Text $content -Pattern '- name: MMORPG_INTERNAL_AUTH_PUBLIC_KEYS\s+value:' -Because '验签方通过明文 env 注入 gate 公钥列表'
    Assert-NotMatch -Text $content -Pattern 'gate-identity-auth' -Because '验签方不引用 gate 签名私钥 Secret'
    $events = @($script:Events)
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because '资产off不能旁路正式SQL迁移门禁'
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
    Assert-True -Condition ($secretAt -ge 0 -and $secretAt -lt [Array]::IndexOf($events, 'apply:Workload')) -Because '非optional Secret必须先存在'
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
    # 写者清单在契约之前渲染,桩记下的 render:boot 是本地动作,不算集群写操作。
    $clusterWrites = @($script:Events | Where-Object { $_ -notlike 'render:*' })
    Assert-Equal -Expected 0 -Actual $clusterWrites.Count -Because '主 manifest 渲染在 ConfigMap / 迁移 Job 之前,渲染失败不能留下半截部署'
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
    Assert-True -Condition ([Array]::IndexOf($events, 'apply:Job') -lt [Array]::IndexOf($events, 'apply:Workload')) -Because 'match常驻形态不自动DDL'
    Assert-True -Condition ($events -contains 'query:Job') -Because 'dev也必须核真实Job完成'
    Assert-Match -Text $script:MigrationContent -Pattern 'args:.*"-migrate"' -Because '运行正式迁移CLI'
    Assert-Match -Text $script:MigrationContent -Pattern 'MATCH_MIGRATION_USER' -Because '独立迁移账号'
    Assert-Match -Text $script:MigrationContent -Pattern 'key: match-migration-password' -Because '独立迁移口令'
    Assert-NotMatch -Text $script:MigrationContent -Pattern 'MATCH_PROBE_SECRET|RANK_PRODUCER_SECRET_MATCH|MMORPG_LOGIN_REDIS_PASSWORD' -Because '迁移不取得业务密钥'
    Assert-NotMatch -Text $script:DeploymentContent -Pattern 'MATCH_MIGRATION_USER|MATCH_MIGRATION_PASSWORD' -Because '常驻进程不得取得DDL凭据'
    # match 查 battle Pod 的身份与权限(battle-instance-loss-early-abort.md §4.2):Pod 用专用 ServiceAccount,RBAC 先于 StatefulSet 落地,
    # 权限只有本命名空间的 pods list;迁移 Job 不带这个身份。
    $rbacAt = [Array]::IndexOf($events, 'apply:BattlePodReaderRbac')
    Assert-True -Condition ($rbacAt -ge 0 -and $rbacAt -lt [Array]::IndexOf($events, 'apply:Workload')) -Because 'Pod 引用的 ServiceAccount 必须先于 StatefulSet 存在'
    Assert-Match -Text $script:DeploymentContent -Pattern '(?m)^      serviceAccountName: match\s*$' -Because '常驻进程用专用 ServiceAccount 查 battle Pod'
    Assert-NotMatch -Text $script:MigrationContent -Pattern 'serviceAccountName' -Because '迁移 Job 不需要查 Pod 的身份'
    Assert-Match -Text $script:BattlePodReaderRbacContent -Pattern '(?m)^kind: Role\s*$' -Because '权限只限本命名空间'
    Assert-NotMatch -Text $script:BattlePodReaderRbacContent -Pattern 'ClusterRole' -Because '不给集群级权限'
    Assert-Match -Text $script:BattlePodReaderRbacContent -Pattern 'resources: \["pods"\]\s+verbs: \["list"\]' -Because '只给 pods 的 list'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($script:BattlePodReaderRbacContent, '(?m)^\s+verbs:').Count) -Because '只有这一条规则'
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
    Assert-True -Condition ($script:Events -notcontains 'apply:Workload') -Because '失败不能继续接流'
}

# StatefulSet 滚动的等待预算(2026-11-20 独立评审):staging / prod 恒等 db 滚完再发 scene,预算按副本数 × 单副本耗时给,
# 不再一律 -WaitTimeoutSeconds;-WaitTimeoutSeconds 只当下限,显式传更大的照样生效。
Test-Case 'StatefulSet 滚动预算按副本数 x (宽限 + minReadySeconds + 30s) 给,-WaitTimeoutSeconds 只当下限' {
    Restore-ProductionFunction 'Get-StatefulSetRolloutWaitSeconds'
    Assert-Equal -Expected 190 -Actual (Get-StatefulSetRolloutWaitSeconds -Replicas 2 -TerminationGraceSeconds 45 -MinReadySeconds 20 -FloorSeconds 180) -Because 'db 清单:2 副本、宽限 45s、minReadySeconds 20'
    Assert-Equal -Expected 475 -Actual (Get-StatefulSetRolloutWaitSeconds -Replicas 5 -TerminationGraceSeconds 45 -MinReadySeconds 20 -FloorSeconds 180) -Because '副本越多预算越长,不会在发 scene 之前误判超时'
    Assert-Equal -Expected 180 -Actual (Get-StatefulSetRolloutWaitSeconds -Replicas 2 -TerminationGraceSeconds 30 -MinReadySeconds 0 -FloorSeconds 180) -Because '算出来更短时用 -WaitTimeoutSeconds'
    Assert-Equal -Expected 600 -Actual (Get-StatefulSetRolloutWaitSeconds -Replicas 2 -TerminationGraceSeconds 45 -MinReadySeconds 20 -FloorSeconds 600) -Because '显式传更大的 -WaitTimeoutSeconds 照样生效'
    Assert-Equal -Expected 180 -Actual (Get-StatefulSetRolloutWaitSeconds -Replicas 0 -TerminationGraceSeconds 45 -MinReadySeconds 20 -FloorSeconds 180) -Because '缩到 0 时只剩下限'
}

exit (Complete-TestRun -SuiteName 'k8s_migrate_gate')
