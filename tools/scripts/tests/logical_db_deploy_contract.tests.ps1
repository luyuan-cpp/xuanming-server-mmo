#requires -Version 7
<#
.SYNOPSIS
    逻辑库的部署侧契约测试(docs/design/horizontal-scale-logical-db.md §11 批 K1)。

.DESCRIPTION
    本文件钉住:
      1. 闸:-DirectoryLogicalShards(dev 缺省 1、staging 显式、prod 只能 64)和 -Region(dev 缺省 0、staging / prod 显式且不能用 0、上限 63);
      2. 纯函数:库名、每片池上限、data_service 名册核对、DbPlacement 计划;
      3. DryRun:K=64 渲染(片数、每片池、mysql-init 的片库、地域码)、DbPlacement 段的位置、MMORPG_DB_PLACEMENT 进 registrar init;
      4. 参与者清单和密钥名从 Go 源码核对,manifest 和本机开发密钥库都要对得上;
      5. 连接总账:K=64 规划档不超 TiDB 预算,K=1 规划档不超 mysql.yaml;
      6. 告警:指标名从 Go 源码核对;
      7. 旧键(AccountRoleSchema 等)从脚本和渲染产物里清零,包装入口把新参数转到底。
    子进程一律 -DryRun(不连集群);纯函数按 AST 从 k8s_deploy.ps1 抽出,在本进程里直接调用。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/logical_db_deploy_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"
. "$PSScriptRoot/../lib/role_fence_dev_secret.ps1"

Write-Host ''
Write-Host '=== logical database deployment contract tests ==='

$RepoRoot = Get-RepoRoot
$Infra = 'logical-infra'
$Zone = 'logical-test'
$ZoneNamespace = "mmorpg-zone-$Zone"
# 存档库清单夹具写占位值;Invoke-CapturedPwsh 按本次的地域码和 K 换成本地域的存档库(lib/deploy_capture.ps1 Add-PlayerStoreAllowlistFixture)。
# dev 缺省地域 0、K=1,换上的就是这一个;A17c 起清单里只有存档库(设计 player-data-tidb-scaling-plan.md §7.2)。
$AllowedDatabases = $PlayerStoreAllowlistFixture
$DevAllowedDatabases = 'player_store_1000000_db'
$AuthorityHost = 'tidb.invalid:4000'

$parseErrors = $null
$deployAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "部署脚本语法错误: $($parseErrors[0].Message)" }
$deployFunctions = @(
    'Resolve-DirectoryLogicalShards', 'Resolve-RegionCode', 'Resolve-IdIssuerRegion', 'Resolve-IdRegistryEtcd', 'Get-DataServiceWriterSchemas', 'Get-DataServiceRosterProblems', 'Get-SqlWriterRosterMinimumEntries',
    'Resolve-PlayerStoreShards', 'Assert-PlayerStoreAllowlist', 'Get-RoleProofStorageIds',
    'Get-DbPlacementPlan', 'New-DbPlacementYaml', 'ConvertTo-DbPlacementJson',
    'Get-AuthoritativeScalar', 'Get-MysqlConnectionPlan', 'Get-MysqlMaxConnectionsFromManifest', 'Get-ManifestDeploymentReplicas',
    'Get-AccountDirectoryPoolMaxOpen', 'Get-MysqlConnectionLedgerInputs', 'Get-MysqlConnectionLedger'
)
foreach ($node in $deployAst.FindAll({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($deployFunctions -contains $node.Name) { Set-Item -Path "Function:script:$($node.Name)" -Value $node.Body.GetScriptBlock() }
}
foreach ($name in $deployFunctions) {
    if (-not (Test-Path "Function:$name")) { throw "k8s_deploy.ps1 缺少函数 $name" }
}

$TempRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-logical-db-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $TempRoot
$CatalogPath = New-SqlWriterCatalogFixture -Root (Join-Path $TempRoot 'catalog') -Namespaces @($ZoneNamespace, $Infra)

function Invoke-LogicalDbDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '本套夹具只允许显式 -DryRun,禁止连接或写集群。' }
    $fixtureEnv = @{ MMORPG_DB_ALLOWED_DATABASES = $AllowedDatabases; MMORPG_SQL_K8S_BOOT_CATALOG = $CatalogPath }
    foreach ($key in $Env.Keys) { $fixtureEnv[$key] = $Env[$key] }
    return Invoke-CapturedPwsh -ScriptPath (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1') -Arguments $Arguments -Env $fixtureEnv
}

function Get-ConfigFlat {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$ConfigMap)
    return ConvertTo-FlatManifest -Block (Get-ConfigBlock -Output $Output -ConfigMap $ConfigMap)
}

function Get-ConfigBlock {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$ConfigMap)
    $found = @(Get-ManifestBlocks -Output $Output | Where-Object { $_ -cmatch '(?m)^kind: ConfigMap\s*$' -and $_ -cmatch ('(?m)^  name:\s*' + [regex]::Escape($ConfigMap) + '\s*$') })
    if ($found.Count -ne 1) { throw "DryRun 输出里 ConfigMap $ConfigMap 应当恰好 1 份,实际 $($found.Count) 份" }
    return $found[0]
}

function Get-DeploymentBlock {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$Name)
    # SQL 写者是 StatefulSet(槽位 = 序号,writer-roster-exhaustion-20261106.md §5.5),其余是 Deployment:按名字挑工作负载,两种都认。
    $found = @(Get-ManifestBlocks -Output $Output | Where-Object { $_ -cmatch '(?m)^kind: (?:Deployment|StatefulSet)\s*$' -and $_ -cmatch ('(?m)^  name: ' + [regex]::Escape($Name) + '\s*$') })
    if ($found.Count -ne 1) { throw "DryRun 输出里工作负载 $Name 应当恰好 1 份,实际 $($found.Count) 份" }
    return $found[0]
}

function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$RelativePath)
    return [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) $RelativePath))
}

# Go 源码里的名单:部署、手册和本机脚本都以它为准。
function Get-GoStringList {
    param([Parameter(Mandatory = $true)][string]$Source, [Parameter(Mandatory = $true)][string]$VarName)
    $match = [regex]::Match($Source, "(?m)^var\s+$VarName\s*=\s*\[\]string\{([^}]*)\}")
    if (-not $match.Success) { throw "Go 源码里找不到 $VarName" }
    return @([regex]::Matches($match.Groups[1].Value, '"([^"]+)"') | ForEach-Object { $_.Groups[1].Value })
}

$GuardSource = Read-RepoText 'go/shared/rolefence/guard.go'
$GoParticipants = @(Get-GoStringList -Source $GuardSource -VarName 'Participants')
$GoCallers = @(Get-GoStringList -Source $GuardSource -VarName 'Callers')
$RouteClientSource = Read-RepoText 'go/shared/accountdirectory/route_client.go'

# 参与者 → K8s Service 名和 gRPC 端口。端口再和各自的 manifest 核一遍。
$ParticipantServices = [ordered]@{
    guild = @{ Service = 'guild'; Port = 50300 }; trade = @{ Service = 'trade'; Port = 50800 }; mail = @{ Service = 'mail'; Port = 50900 }
    match = @{ Service = 'match'; Port = 50500 }; player_inbox = @{ Service = 'player-inbox'; Port = 50950 }; friend = @{ Service = 'friend'; Port = 50400 }
}

$ZoneArgs = @('-Command', 'zone-up', '-ZoneName', $Zone, '-ZoneId', '101', '-DryRun', '-InfraNamespace', $Infra,
    '-GlobalAuthorityTiDBHost', $AuthorityHost, '-GoSvcRegistry', 'registry.invalid/test', '-JavaSvcRegistry', 'registry.invalid/test')
# 全局服务正式准入要求 RF=3、minISR=2；仅夹具显式使用虚构 3 broker 拓扑。
$InfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-SkipJavaSvc', '-KafkaBrokers', '3', '-InfraNamespace', $Infra,
    '-GlobalAuthorityTiDBHost', $AuthorityHost, '-GoSvcRegistry', 'registry.invalid/test', '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab')
$InfraEnv = @{ MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345' }
# 同 k8s_deploy_contract.tests.ps1 的 $ProdEnv:走到逻辑库那一步之前要的密钥。玩家域集群的参数和口令由 deploy_capture 的夹具补。
$ProdEnv = @{
    MMORPG_GATE_TOKEN_SECRET        = 'contract-test-secret-0123456789abcdef0123'
    MMORPG_MYSQL_USER               = 'mmorpg_app'
    MMORPG_REDIS_PASSWORD           = 'contract-test-redis-pw-01'
    MMORPG_GATEWAY_DB_USER          = 'gateway_app'
    MMORPG_INTERNAL_AUTH_SIGNING_KEY = '9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60'
    MMORPG_INTERNAL_AUTH_PUBLIC_KEYS = 'd75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a'
    MMORPG_GATEWAY_ADMIN_API_KEY    = 'contract-test-gw-admin-key-0123456789abcdef'
    MMORPG_LOGIN_QUEUE_TOKEN_SECRET = 'contract-test-queue-token-0123456789abcdef'
    MMORPG_SPECTATE_TOKEN_SECRET    = 'contract-test-spectate-token-0123456789abcdef'
    MMORPG_BATTLE_TOKEN_SECRET      = 'contract-battle-ticket-0123456789abcdef012345'
    MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED = '1'
}
$ProdZoneArgs = $ZoneArgs + @('-ReleaseProfile', 'prod', '-SkipPreflight', '-NodeImage', 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab')
$StagingZoneArgs = $ZoneArgs + @('-ReleaseProfile', 'staging', '-SkipPreflight', '-NodeImage', 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab')

$devZoneRun = Invoke-LogicalDbDryRun -Arguments $ZoneArgs
# 同一个显式入口按 mysql 档部署(dev 才允许网关用 mysql 档,Resolve-GatewaySqlMode)。
$devMysqlZoneRun = Invoke-LogicalDbDryRun -Arguments ($ZoneArgs + @('-DatabaseBackend', 'mysql'))
$prodZoneRun = Invoke-LogicalDbDryRun -Arguments ($ProdZoneArgs + @('-DirectoryLogicalShards', '64', '-Region', '7')) -Env $ProdEnv
$devInfraRun = Invoke-LogicalDbDryRun -Arguments $InfraArgs -Env $InfraEnv
# 集群内单实例 MySQL(mysql.yaml、mysql-init-sql)只在 dev 档、入口就是它的 mysql 档部署(Test-InfraMysqlInUse)。mysql-init 的用例用这一形态。
$InClusterMysqlHost = "mysql.${Infra}:3306"
$InfraMysqlArgs = @($InfraArgs | ForEach-Object { if ($_ -ceq $AuthorityHost) { $InClusterMysqlHost } else { $_ } }) + @('-DatabaseBackend', 'mysql')
$devInfraMysqlRun = Invoke-LogicalDbDryRun -Arguments $InfraMysqlArgs -Env $InfraEnv
$shardedInfraMysqlRun = Invoke-LogicalDbDryRun -Arguments ($InfraMysqlArgs + @('-DirectoryLogicalShards', '64')) -Env $InfraEnv

# ──────────────────────────────────────────────
# 1. 闸(纯函数)
# ──────────────────────────────────────────────
Test-Case '-DirectoryLogicalShards:dev 缺省 1,staging 必须显式,prod 只能 64,不是 64 的约数拒绝' {
    Assert-Equal -Expected 1 -Actual (Resolve-DirectoryLogicalShards -Requested 0 -ReleaseProfileName 'dev') -Because 'dev 缺省 1'
    Assert-Equal -Expected 8 -Actual (Resolve-DirectoryLogicalShards -Requested 8 -ReleaseProfileName 'dev') -Because 'dev 也可以显式'
    Assert-Equal -Expected 16 -Actual (Resolve-DirectoryLogicalShards -Requested 16 -ReleaseProfileName 'staging') -Because 'staging 显式给什么用什么'
    Assert-Equal -Expected 64 -Actual (Resolve-DirectoryLogicalShards -Requested 64 -ReleaseProfileName 'prod') -Because 'prod 是 64'
    foreach ($profileName in @('staging', 'prod')) {
        Assert-Match -Text (Get-ThrownMessage { Resolve-DirectoryLogicalShards -Requested 0 -ReleaseProfileName $profileName }) -Pattern 'must pass -DirectoryLogicalShards explicitly' -Because "$profileName 不猜片数:上线后不能改"
    }
    Assert-Match -Text (Get-ThrownMessage { Resolve-DirectoryLogicalShards -Requested 16 -ReleaseProfileName 'prod' }) -Pattern 'must use -DirectoryLogicalShards 64' -Because 'prod 只能 64'
    Assert-Match -Text (Get-ThrownMessage { Resolve-DirectoryLogicalShards -Requested 3 -ReleaseProfileName 'dev' }) -Pattern 'divisor of 64' -Because '片数要是 64 的约数'
}

Test-Case '-Region:dev 缺省 0,staging 和 prod 必须显式且不能用 0,范围 0..63' {
    Assert-Equal -Expected 0 -Actual (Resolve-IdIssuerRegion -Requested -1 -ReleaseProfileName 'dev') -Because 'dev 缺省 0'
    Assert-Equal -Expected 5 -Actual (Resolve-IdIssuerRegion -Requested 5 -ReleaseProfileName 'dev') -Because 'dev 也可以显式'
    Assert-Equal -Expected 0 -Actual (Resolve-IdIssuerRegion -Requested 0 -ReleaseProfileName 'dev') -Because 'dev 可以显式写 0,0 不等于没给'
    Assert-Equal -Expected 1 -Actual (Resolve-IdIssuerRegion -Requested 1 -ReleaseProfileName 'prod') -Because '生产的下界'
    Assert-Equal -Expected 63 -Actual (Resolve-IdIssuerRegion -Requested 63 -ReleaseProfileName 'staging') -Because '上界'
    foreach ($profileName in @('staging', 'prod')) {
        Assert-Match -Text (Get-ThrownMessage { Resolve-IdIssuerRegion -Requested -1 -ReleaseProfileName $profileName }) -Pattern 'must pass -Region explicitly' -Because "$profileName 不猜地域码"
        Assert-Match -Text (Get-ThrownMessage { Resolve-IdIssuerRegion -Requested 0 -ReleaseProfileName $profileName }) -Pattern 'reserved for dev' -Because "$profileName 不能用 dev 的地域码,漏写也是 0"
    }
    Assert-Match -Text (Get-ThrownMessage { Resolve-IdIssuerRegion -Requested 64 -ReleaseProfileName 'dev' }) -Pattern 'out of range' -Because '超界'
}

Test-Case '-IdIssuerRegion:缺省等于 -Region;给了只换发号器地域码,规则与 -Region 相同;存档地域只认 -Region' {
    Assert-Equal -Expected 7 -Actual (Resolve-IdIssuerRegion -Requested 7 -ReleaseProfileName 'prod' -IssuerOverride -1) -Because '没给 -IdIssuerRegion 就是 -Region'
    Assert-Equal -Expected 9 -Actual (Resolve-IdIssuerRegion -Requested 7 -ReleaseProfileName 'prod' -IssuerOverride 9) -Because '换码之后发号器用新码'
    Assert-Equal -Expected 7 -Actual (Resolve-RegionCode -Requested 7 -ReleaseProfileName 'prod') -Because '存档地域只认 -Region,不跟着换码变'
    Assert-Match -Text (Get-ThrownMessage { Resolve-IdIssuerRegion -Requested 7 -ReleaseProfileName 'prod' -IssuerOverride 0 }) -Pattern 'cannot use -IdIssuerRegion 0' -Because '生产不能把发号器地域码换成 dev 的 0,报错点名 -IdIssuerRegion'
    Assert-Match -Text (Get-ThrownMessage { Resolve-IdIssuerRegion -Requested 7 -ReleaseProfileName 'dev' -IssuerOverride 64 }) -Pattern '-IdIssuerRegion 64 is out of range' -Because '超界'
}

Test-Case '-IdRegistryEtcd:dev 不写,staging 和 prod 必须给或者本集群托管,不能是本集群两套发号 etcd' {
    Assert-Equal -Expected 0 -Actual @(Resolve-IdRegistryEtcd -Requested '' -ReleaseProfileName 'dev' -Hosted $false -Namespace $Infra).Count -Because 'dev 回落到本地 etcd'
    Assert-Equal -Expected 'reg-a.invalid:2379,reg-b.invalid:2379' -Actual (@(Resolve-IdRegistryEtcd -Requested ' reg-a.invalid:2379, reg-b.invalid:2379 ' -ReleaseProfileName 'prod' -Hosted $false -Namespace $Infra) -join ',') -Because '逗号分隔,去空白'
    Assert-Equal -Expected "etcd-id-registry.${Infra}:2379" -Actual (@(Resolve-IdRegistryEtcd -Requested '' -ReleaseProfileName 'staging' -Hosted $true -Namespace $Infra) -join ',') -Because '托管登记处的集群取本集群的 Service'
    foreach ($profileName in @('staging', 'prod')) {
        Assert-Match -Text (Get-ThrownMessage { Resolve-IdRegistryEtcd -Requested '' -ReleaseProfileName $profileName -Hosted $false -Namespace $Infra }) -Pattern 'must pass -IdRegistryEtcd' -Because "$profileName 不猜登记处"
    }
    foreach ($local in @("etcd.${Infra}:2379", "etcd-id-secondary.${Infra}:2379")) {
        Assert-Match -Text (Get-ThrownMessage { Resolve-IdRegistryEtcd -Requested $local -ReleaseProfileName 'prod' -Hosted $false -Namespace $Infra }) -Pattern 'cannot be this cluster' -Because '登记处是第三份记录,不能和本地两份同一套'
    }
    Assert-Match -Text (Get-ThrownMessage { Resolve-IdRegistryEtcd -Requested 'http://reg.invalid:2379' -ReleaseProfileName 'prod' -Hosted $false -Namespace $Infra }) -Pattern 'must be host:port' -Because '不收 scheme 和凭据'
    Assert-Match -Text (Get-ThrownMessage { Resolve-IdRegistryEtcd -Requested 'reg.invalid:2379' -ReleaseProfileName 'dev' -Hosted $false -Namespace $Infra }) -Pattern 'staging and prod only' -Because 'dev 写了就是配错'
    Assert-Match -Text (Get-ThrownMessage { Resolve-IdRegistryEtcd -Requested '' -ReleaseProfileName 'dev' -Hosted $true -Namespace $Infra }) -Pattern 'staging and prod only' -Because 'dev 不托管登记处'
}

# 缺登记处的拒绝只在上一条用例里按函数测:空串参数要穿过两层子进程,不同 pwsh 版本处理不一样,不拿它当夹具。
Test-Case 'DryRun:prod 的 data-service ConfigMap 写出登记处;指到本集群发号 etcd、dev 写了都在出清单之前拒绝' {
    Assert-Match -Text $prodZoneRun.Output -Pattern 'RegistryEtcd: \["id-registry\.invalid:2379"\]' -Because 'deploy_capture 夹具补的 -IdRegistryEtcd 要进 IdSegment.Issuer'
    Assert-Match -Text $prodZoneRun.Output -Pattern 'Id issuer: region=7 registry=id-registry\.invalid:2379' -Because '入口打印本次登记处'
    Assert-NotMatch -Text $devZoneRun.Output -Pattern 'RegistryEtcd:' -Because 'dev 回落到本地 etcd,不写'
    $hosted = Invoke-LogicalDbDryRun -Arguments ($ProdZoneArgs + @('-DirectoryLogicalShards', '64', '-Region', '7', '-HostIdRegistry')) -Env $ProdEnv
    Assert-Equal -Expected 0 -Actual $hosted.ExitCode -Because "输出: $($hosted.Output)"
    Assert-Match -Text $hosted.Output -Pattern ('RegistryEtcd: \["etcd-id-registry\.' + [regex]::Escape($Infra) + ':2379"\]') -Because '托管登记处的集群指向本集群的 Service'
    foreach ($case in @(
            @{ Args = $ProdZoneArgs + @('-DirectoryLogicalShards', '64', '-Region', '7', '-IdRegistryEtcd', "etcd-id-secondary.${Infra}:2379"); Pattern = 'cannot be this cluster'; Why = 'prod 登记处指到本集群的副记录 etcd' }
            @{ Args = $ZoneArgs + @('-IdRegistryEtcd', 'reg.invalid:2379'); Pattern = 'staging and prod only'; Why = 'dev 写了登记处' }
        )) {
        $run = Invoke-LogicalDbDryRun -Arguments $case.Args -Env $ProdEnv
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "$($case.Why) 必须拒绝。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern $case.Pattern -Because "$($case.Why) 要点名"
        Assert-NotMatch -Text $run.Output -Pattern '--- BEGIN MANIFEST ---' -Because "$($case.Why):在任何清单生成之前拒绝"
    }
}

Test-Case '写路径入口:Assert-LogicalDbPreflight 排在任何集群写操作之前' {
    $writeGates = @($deployAst.EndBlock.Statements | Where-Object {
        $_ -is [Management.Automation.Language.IfStatementAst] -and
        $_.Clauses[0].Item1.Extent.Text -match '^\$Command\s+-in\s+@\(\s*"zone-up"\s*,\s*"all-up"\s*,\s*"infra-up"\s*\)$'
    })
    Assert-Equal -Expected 1 -Actual $writeGates.Count -Because '顶层应当恰好有一个写路径门禁块'
    $calls = @($writeGates[0].Clauses[0].Item2.FindAll({ param($a) $a -is [Management.Automation.Language.CommandAst] -and $a.GetCommandName() -eq 'Assert-LogicalDbPreflight' }, $true))
    Assert-Equal -Expected 1 -Actual $calls.Count -Because '门禁块里要调一次'
}

Test-Case 'DryRun:prod 缺 K、K 不是 64、缺地域码、地域码用 0 都拒绝;staging 同样要显式' {
    foreach ($case in @(
            @{ Args = $ProdZoneArgs + @('-DirectoryLogicalShards', '0', '-Region', '1'); Pattern = 'must pass -DirectoryLogicalShards explicitly'; Why = 'prod 缺 K' }
            @{ Args = $ProdZoneArgs + @('-DirectoryLogicalShards', '16', '-Region', '1'); Pattern = 'must use -DirectoryLogicalShards 64'; Why = 'prod 的 K 不是 64' }
            @{ Args = $ProdZoneArgs + @('-DirectoryLogicalShards', '64', '-Region', '-1'); Pattern = 'must pass -Region explicitly'; Why = 'prod 缺地域码' }
            @{ Args = $ProdZoneArgs + @('-DirectoryLogicalShards', '64', '-Region', '0'); Pattern = 'reserved for dev'; Why = 'prod 用了 dev 的地域码' }
            @{ Args = $StagingZoneArgs + @('-DirectoryLogicalShards', '0', '-Region', '1'); Pattern = 'must pass -DirectoryLogicalShards explicitly'; Why = 'staging 缺 K' }
            @{ Args = $StagingZoneArgs + @('-DirectoryLogicalShards', '16', '-Region', '-1'); Pattern = 'must pass -Region explicitly'; Why = 'staging 缺地域码' }
            @{ Args = $StagingZoneArgs + @('-DirectoryLogicalShards', '16', '-Region', '0'); Pattern = 'reserved for dev'; Why = 'staging 用了 dev 的地域码' }
        )) {
        $run = Invoke-LogicalDbDryRun -Arguments $case.Args -Env $ProdEnv
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "$($case.Why) 必须拒绝。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern $case.Pattern -Because "$($case.Why) 要点名"
        Assert-NotMatch -Text $run.Output -Pattern '--- BEGIN MANIFEST ---' -Because "$($case.Why):在任何清单生成之前拒绝"
    }
    $out = Invoke-LogicalDbDryRun -Arguments ($ZoneArgs + @('-Region', '64'))
    Assert-True -Condition ($out.ExitCode -ne 0) -Because '地域码超界在参数绑定处失败'
}

# ──────────────────────────────────────────────
# 2. 库名、每片池、名册、映射计划(纯函数)
# ──────────────────────────────────────────────
Test-Case '库名和 dirshard 同形:K=1 是基名,否则 <基名>_NN,流水库 <基名>_ledger' {
    Assert-Equal -Expected 'g' -Actual (@(Get-DirectoryDbNames -Base 'g' -LogicalShards 1) -join ',') -Because 'K=1 目录库就是基名'
    Assert-Equal -Expected 'g_00,g_01,g_02,g_03' -Actual (@(Get-DirectoryDbNames -Base 'g' -LogicalShards 4) -join ',') -Because '两位序号'
    $names = @(Get-DirectoryDbNames -Base 'g' -LogicalShards 64)
    Assert-Equal -Expected 64 -Actual $names.Count -Because '64 片'
    Assert-Equal -Expected 'g_00' -Actual $names[0] -Because '第 0 片'
    Assert-Equal -Expected 'g_63' -Actual $names[63] -Because '最后一片'
    Assert-Equal -Expected 'g_ledger' -Actual (Get-DirectoryLedgerDbName -Base 'g') -Because '流水库'
    $source = Read-RepoText 'go/shared/dirshard/dirshard.go'
    Assert-Match -Text $source -Pattern '%s_%02d' -Because 'Go 侧的片库名同一格式(go/shared/dirshard)'
    Assert-Match -Text $source -Pattern '_ledger' -Because 'Go 侧的流水库名同一后缀'
}

Test-Case '每片池上限:K=1 不动,K 大按片摊开,每片至少 2 条' {
    Assert-Equal -Expected 20 -Actual (Get-DirectoryShardPoolLimit -Base 20 -LogicalShards 1) -Because 'K=1 就是 yaml 的值'
    Assert-Equal -Expected 5 -Actual (Get-DirectoryShardPoolLimit -Base 20 -LogicalShards 4) -Because '20 / 4'
    Assert-Equal -Expected 3 -Actual (Get-DirectoryShardPoolLimit -Base 20 -LogicalShards 8) -Because '向上取整'
    Assert-Equal -Expected 2 -Actual (Get-DirectoryShardPoolLimit -Base 20 -LogicalShards 64) -Because '下限 2:事务之外还要留一条给同片的短读'
    Assert-Equal -Expected 2 -Actual (Get-DirectoryShardPoolLimit -Base 8 -LogicalShards 64) -Because '下限 2'
    Assert-Match -Text (Get-ThrownMessage { Get-DirectoryShardPoolLimit -Base 20 -LogicalShards 5 }) -Pattern 'divisor of 64' -Because '片数非法时不算'
}

Test-Case 'data_service 名册:恰好是片库、流水库和全部存档库,多一份少一份重复都点名' {
    $expected = Get-DataServiceWriterSchemas -Base 'g' -LogicalShards 2 -PlayerDatabases @('player_store_1000000_db', 'player_store_1000001_db')
    Assert-Equal -Expected 'g_00,g_01,g_ledger,player_store_1000000_db,player_store_1000001_db' -Actual (@($expected) -join ',') -Because '顺序和 config.WriterSchemas 一样'
    $single = Get-DataServiceWriterSchemas -Base 'g' -LogicalShards 1 -PlayerDatabases @('player_store_1000000_db')
    Assert-Equal -Expected 'g,g_ledger,player_store_1000000_db' -Actual (@($single) -join ',') -Because 'K=1 基名就是目录库,不会和流水库连成一个字符串'

    function New-Roster([string[]]$Schemas) {
        return [ordered]@{ entries = @([ordered]@{ requests = @($Schemas | ForEach-Object { [ordered]@{ authoritySchema = $_ } }) }) }
    }
    Assert-Equal -Expected 0 -Actual @(Get-DataServiceRosterProblems -Roster (New-Roster -Schemas $expected) -ExpectedSchemas $expected).Count -Because '恰好覆盖就过'
    $missing = @(Get-DataServiceRosterProblems -Roster (New-Roster -Schemas @('g_00', 'g_ledger', 'player_store_1000000_db', 'player_store_1000001_db')) -ExpectedSchemas $expected)
    Assert-Match -Text ($missing -join ';') -Pattern 'entry 0 is missing: g_01' -Because '少一片'
    $extra = @(Get-DataServiceRosterProblems -Roster (New-Roster -Schemas (@($expected) + 'player_store_1000002_db')) -ExpectedSchemas $expected)
    Assert-Match -Text ($extra -join ';') -Pattern 'entry 0 has extra: player_store_1000002_db' -Because '多一个库'
    $repeated = @(Get-DataServiceRosterProblems -Roster (New-Roster -Schemas (@($expected) + 'g_00')) -ExpectedSchemas $expected)
    Assert-Match -Text ($repeated -join ';') -Pattern 'entry 0 repeats: g_00' -Because '重复'
    Assert-Equal -Expected 0 -Actual @(Get-DataServiceRosterProblems -Roster ([ordered]@{ entries = @(@{}, @{}) }) -ExpectedSchemas $expected).Count -Because '空 entry 这里不核,真正的 registrar 会拒绝'
}

Test-Case 'DryRun:data-service 的名册和这次的 K、存档库清单对不上,在 apply 之前拒绝' {
    $root = Join-Path $TempRoot 'roster-check'
    $catalogFile = New-SqlWriterCatalogFixture -Root $root -Namespaces @($ZoneNamespace, $Infra)
    $catalog = Get-Content -LiteralPath $catalogFile -Raw | ConvertFrom-Json -AsHashtable
    $writeRoster = {
        param([string[]]$Schemas)
        $requests = @($Schemas | ForEach-Object { [ordered]@{ authoritySchema = $_; protectedSchemas = @($_) } })
        $rosterPath = Join-Path $root 'data-service-roster.json'
        $roster = [ordered]@{ format = 'SQL_WRITER_BOOT_ROSTER_V2'; entries = @(1..12 | ForEach-Object { [ordered]@{ requests = $requests } }) }
        [IO.File]::WriteAllText($rosterPath, (ConvertTo-Json -InputObject $roster -Depth 6 -Compress), [Text.UTF8Encoding]::new($false))
        $catalog["$ZoneNamespace/data-service"].rosterFile = $rosterPath
        $catalog["$ZoneNamespace/data-service"].rosterSha256 = (Get-FileHash -LiteralPath $rosterPath -Algorithm SHA256).Hash.ToLowerInvariant()
        [IO.File]::WriteAllText($catalogFile, (ConvertTo-Json -InputObject $catalog -Depth 8 -Compress), [Text.UTF8Encoding]::new($false))
    }
    # 少了流水库:G1 之前的旧名册就是这样。
    & $writeRoster @('mmorpg_global', 'player_store_1000000_db')
    $bad = Invoke-LogicalDbDryRun -Arguments $ZoneArgs -Env @{ MMORPG_SQL_K8S_BOOT_CATALOG = $catalogFile }
    Assert-True -Condition ($bad.ExitCode -ne 0) -Because "名册缺流水库必须拒绝。输出: $($bad.Output)"
    Assert-Match -Text $bad.Output -Pattern 'boot roster.*does not match' -Because '点名名册'
    Assert-Match -Text $bad.Output -Pattern 'mmorpg_global_ledger' -Because '点名缺哪个库'
    # 少了地域存档库:清单里有它(dev 地域 0、K=1),data_service 要给它发写资格。
    & $writeRoster @('mmorpg_global', 'mmorpg_global_ledger')
    $noStore = Invoke-LogicalDbDryRun -Arguments $ZoneArgs -Env @{ MMORPG_SQL_K8S_BOOT_CATALOG = $catalogFile }
    Assert-True -Condition ($noStore.ExitCode -ne 0) -Because "名册缺地域存档库必须拒绝。输出: $($noStore.Output)"
    Assert-Match -Text $noStore.Output -Pattern 'missing: player_store_1000000_db' -Because '点名缺哪个存档库'
    & $writeRoster @('mmorpg_global', 'mmorpg_global_ledger', 'player_store_1000000_db')
    $good = Invoke-LogicalDbDryRun -Arguments $ZoneArgs -Env @{ MMORPG_SQL_K8S_BOOT_CATALOG = $catalogFile }
    Assert-Equal -Expected 0 -Actual $good.ExitCode -Because "名册对得上就过。输出: $($good.Output)"
}

Test-Case 'DbPlacement 计划:没给集群清单时单集群 main;默认集群必须是权威入口;清单不带凭据' {
    $single = Get-DbPlacementPlan -SqlHost $AuthorityHost -ClustersText '' -DefaultCluster '' -EtcdHost 'etcd.x:2379'
    Assert-Equal -Expected 'main' -Actual $single.DefaultCluster -Because '单集群'
    Assert-Equal -Expected $AuthorityHost -Actual $single.SqlClusters['main'] -Because '地址就是权威入口'
    $two = Get-DbPlacementPlan -SqlHost $AuthorityHost -ClustersText "a=$AuthorityHost, b=tidb-b.invalid:4000" -DefaultCluster 'a' -EtcdHost 'etcd.x:2379'
    Assert-Equal -Expected 'a,b' -Actual (@($two.SqlClusters.Keys) -join ',') -Because '两个集群都进计划'
    $flat = (ConvertFrom-YamlToFlatMap -Text (New-DbPlacementYaml -Plan $two -Indent 0)).Scalars
    Assert-Equal -Expected 'a' -Actual $flat['DbPlacement.DefaultCluster'] -Because 'yaml 段'
    Assert-Equal -Expected 'tidb-b.invalid:4000' -Actual $flat['DbPlacement.SqlClusters.b.Hosts'] -Because 'yaml 段的地址'
    Assert-Equal -Expected 'etcd.x:2379' -Actual $flat['DbPlacement.EtcdHosts[0]'] -Because '映射在 etcd'
    $json = ConvertTo-DbPlacementJson -Plan $two | ConvertFrom-Json
    Assert-Equal -Expected 'a' -Actual $json.DefaultCluster -Because 'MMORPG_DB_PLACEMENT 同形状'
    Assert-Equal -Expected 'tidb-b.invalid:4000' -Actual $json.SqlClusters.b.Hosts -Because 'MMORPG_DB_PLACEMENT 同形状'
    Assert-Equal -Expected 'etcd.x:2379' -Actual (@($json.EtcdHosts) -join ',') -Because 'MMORPG_DB_PLACEMENT 同形状'
    foreach ($case in @(
            @{ Clusters = 'a=user:pw@tidb.invalid:4000'; Default = 'a'; Pattern = 'credentials do not belong here' }
            @{ Clusters = "a=$AuthorityHost"; Default = ''; Pattern = 'needs -SqlDefaultCluster' }
            @{ Clusters = "a=$AuthorityHost"; Default = 'b'; Pattern = 'is not in -SqlClusters' }
            @{ Clusters = 'a=other.invalid:4000'; Default = 'a'; Pattern = 'must point at -GlobalAuthorityTiDBHost' }
            @{ Clusters = ''; Default = 'a'; Pattern = 'needs -SqlClusters' }
            @{ Clusters = "a=$AuthorityHost,a=tidb-b.invalid:4000"; Default = 'a'; Pattern = 'twice' }
        )) {
        Assert-Match -Text (Get-ThrownMessage { Get-DbPlacementPlan -SqlHost $AuthorityHost -ClustersText $case.Clusters -DefaultCluster $case.Default -EtcdHost 'etcd.x:2379' }) -Pattern $case.Pattern -Because "拒绝: $($case.Clusters) / $($case.Default)"
    }
}

# ──────────────────────────────────────────────
# 3. DryRun 渲染
# ──────────────────────────────────────────────
Test-Case 'dev zone-up:K=1、地域 0,池就是服务 yaml 的值,入口打印本次形态' {
    Assert-Equal -Expected 0 -Actual $devZoneRun.ExitCode -Because "输出: $($devZoneRun.Output)"
    $flat = Get-ConfigFlat -Output $devZoneRun.Output -ConfigMap 'go-svc-data-service-config'
    $source = 'go/data_service/etc/data_service.yaml'
    Assert-Equal -Expected '1' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.Directory.LogicalShards') -Because 'dev 缺省 1'
    Assert-Equal -Expected '0' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.IdSegment.Issuer.Region') -Because 'dev 缺省 0'
    foreach ($key in @('PlayerName.MaxOpenConn', 'RouteStore.MaxOpenConn', 'AccountDirectory.MaxOpenConn', 'Directory.StoragePools.MaxOpenConn', 'Directory.StoragePools.MaxIdleConn', 'Directory.StoragePools.ConnMaxIdleSeconds')) {
        Assert-Equal -Expected (Get-EtcValue -RelativePath $source -KeyPath $key) -Actual (Get-FlatValue -Flat $flat -KeyPath "data.data_service.yaml.$key") -Because "K=1 时 $key 镜像服务 yaml"
    }
    Assert-Match -Text $devZoneRun.Output -Pattern 'Logical databases: directory_shards=1 directory=mmorpg_global ledger=mmorpg_global_ledger id_region=0' -Because '入口打印一行本次形态'
    Assert-Match -Text $devZoneRun.Output -Pattern 'MySQL connection ledger by logical db: logical_shards=1 limit=tidb_budget' -Because 'tidb 档 K=1 也对照 TiDB 预算:mmorpg 迁走之后这一档没有 MySQL'
    # mysql 档连外部主库($AuthorityHost)、dev 没给 -MysqlMaxConnections:仍按 mysql.yaml 的值比,但标明是假定值并另打一行说明。
    Assert-Match -Text $devMysqlZoneRun.Output -Pattern 'MySQL connection ledger by logical db: logical_shards=1 limit=mysql_manifest_assumed ' -Because '外部主库的上限脚本读不到,dev 不给时不能把 mysql.yaml 的值说成它的上限'
    Assert-Match -Text $devMysqlZoneRun.Output -Pattern 'MySQL connection ledger: the SQL entry is an external MySQL primary and -MysqlMaxConnections was not given' -Because '另打一行,点名该给的参数'
    Assert-NotMatch -Text $devZoneRun.Output -Pattern '-MysqlMaxConnections was not given' -Because 'tidb 档对照 TiDB 预算,不提这个参数'
}

Test-Case 'SQL 连接总账的上限跟着连接实际打到的服务器走:外部 MySQL 主库用 -MysqlMaxConnections,staging / prod 不给就拒绝,给了用不上也拒绝' {
    # 入口是集群内那台 MySQL(只在 dev):上限读 mysql.yaml,不用也不许给 -MysqlMaxConnections。
    $inClusterArgs = @($ZoneArgs | ForEach-Object { if ($_ -ceq $AuthorityHost) { $InClusterMysqlHost } else { $_ } }) + @('-DatabaseBackend', 'mysql')
    $inCluster = Invoke-LogicalDbDryRun -Arguments $inClusterArgs
    Assert-Equal -Expected 0 -Actual $inCluster.ExitCode -Because "dev 用集群内 MySQL。输出: $($inCluster.Output)"
    Assert-Match -Text $inCluster.Output -Pattern 'MySQL connection ledger by logical db: logical_shards=1 limit=mysql_manifest ' -Because '集群内 MySQL 的上限就是 mysql.yaml 写的值'
    $inClusterWithLimit = Invoke-LogicalDbDryRun -Arguments ($inClusterArgs + @('-MysqlMaxConnections', '3000'))
    Assert-True -Condition ($inClusterWithLimit.ExitCode -ne 0) -Because "入口是集群内 MySQL 时给了也不生效,要拒绝。输出: $($inClusterWithLimit.Output)"
    Assert-Match -Text $inClusterWithLimit.Output -Pattern 'this run has no external MySQL primary' -Because '说明为什么用不上'
    Assert-NotMatch -Text $inClusterWithLimit.Output -Pattern '--- BEGIN MANIFEST ---' -Because '在写路径入口拒绝'

    # 外部主库:给了就拿它当上限,打在总账两行里。
    $external = Invoke-LogicalDbDryRun -Arguments ($ZoneArgs + @('-DatabaseBackend', 'mysql', '-MysqlMaxConnections', '3000'))
    Assert-Equal -Expected 0 -Actual $external.ExitCode -Because "输出: $($external.Output)"
    Assert-Match -Text $external.Output -Pattern 'MySQL connection ledger: zones=\d+ steady=\d+ peak=\d+ max_connections=3000 ' -Because '上限是运维给的那台主库的 max_connections'
    Assert-Match -Text $external.Output -Pattern 'MySQL connection ledger by logical db: logical_shards=1 limit=mysql_external ' -Because '来源标成外部主库'
    Assert-NotMatch -Text $external.Output -Pattern '-MysqlMaxConnections was not given' -Because '给了就不再打假定值的说明'

    # staging / prod 连外部主库却不给上限:总账不替运维猜,在任何写操作之前拒绝。
    $prodNoLimit = Invoke-LogicalDbDryRun -Arguments ($ProdZoneArgs + @('-DirectoryLogicalShards', '64', '-Region', '7', '-DatabaseBackend', 'mysql')) -Env $ProdEnv
    Assert-True -Condition ($prodNoLimit.ExitCode -ne 0) -Because "prod 的 mysql 档连外部主库必须给 -MysqlMaxConnections。输出: $($prodNoLimit.Output)"
    Assert-Match -Text $prodNoLimit.Output -Pattern 'needs -MysqlMaxConnections' -Because '点名缺的参数'
    Assert-NotMatch -Text $prodNoLimit.Output -Pattern '--- BEGIN MANIFEST ---' -Because '在写路径入口拒绝'

    # tidb 档给了它:那一档对照 TiDB 预算,参数不生效,同样拒绝。
    $tidbWithLimit = Invoke-LogicalDbDryRun -Arguments ($ZoneArgs + @('-MysqlMaxConnections', '3000'))
    Assert-True -Condition ($tidbWithLimit.ExitCode -ne 0) -Because "tidb 档不认 -MysqlMaxConnections。输出: $($tidbWithLimit.Output)"
    Assert-Match -Text $tidbWithLimit.Output -Pattern '-MysqlMaxConnections only applies to -DatabaseBackend mysql' -Because '说明它只属于 mysql 档'
}

Test-Case 'prod zone-up K=64:片数、每片池、地域码进 ConfigMap,上限换 TiDB 预算,存档库池按存档库数摊开' {
    Assert-Equal -Expected 0 -Actual $prodZoneRun.ExitCode -Because "输出: $($prodZoneRun.Output)"
    $flat = Get-ConfigFlat -Output $prodZoneRun.Output -ConfigMap 'go-svc-data-service-config'
    $source = 'go/data_service/etc/data_service.yaml'
    Assert-Equal -Expected '64' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.Directory.LogicalShards') -Because 'K'
    Assert-Equal -Expected '7' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.IdSegment.Issuer.Region') -Because '-Region 写进 IdSegment.Issuer.Region'
    foreach ($key in @('PlayerName.MaxOpenConn', 'RouteStore.MaxOpenConn', 'AccountDirectory.MaxOpenConn')) {
        Assert-Equal -Expected '2' -Actual (Get-FlatValue -Flat $flat -KeyPath "data.data_service.yaml.$key") -Because "$key 每片一个池,K=64 时每片 2 条"
    }
    foreach ($key in @('PlayerName.MaxIdleConn', 'RouteStore.MaxIdleConn')) {
        Assert-True -Condition ([int](Get-FlatValue -Flat $flat -KeyPath "data.data_service.yaml.$key") -le 2) -Because "$key 不能超过每片上限,否则 Validate 拒启"
    }
    # 存档库的池按地域存档库数(夹具给 prod 补的 -PlayerStoreShards 64)摊开,和目录片数无关(设计 player-data-tidb-scaling-plan.md §7.2)。
    Assert-Equal -Expected ([string](Get-PlayerStorePoolLimit -Base ([int](Get-EtcValue -RelativePath $source -KeyPath 'Directory.StoragePools.MaxOpenConn')) -Shards 64)) -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.Directory.StoragePools.MaxOpenConn') -Because '存档库的池按存档库数摊开,和目录片数无关'
    Assert-Match -Text $prodZoneRun.Output -Pattern 'Logical databases: directory_shards=64 directory=mmorpg_global_00\.\.63 ledger=mmorpg_global_ledger id_region=7' -Because '入口打印一行本次形态'
    Assert-Match -Text $prodZoneRun.Output -Pattern 'MySQL connection ledger by logical db: logical_shards=64 limit=tidb_budget data_service_per_instance=\d+ directory_shard=\d+ ledger=\d+ player_store=\d+' -Because '按片、按逻辑库拆开'
}

Test-Case 'staging 可以用小一点的 K,同样进 ConfigMap' {
    $run = Invoke-LogicalDbDryRun -Arguments ($StagingZoneArgs + @('-DirectoryLogicalShards', '16', '-Region', '3')) -Env $ProdEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    $flat = Get-ConfigFlat -Output $run.Output -ConfigMap 'go-svc-data-service-config'
    Assert-Equal -Expected '16' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.Directory.LogicalShards') -Because 'K'
    Assert-Equal -Expected '3' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.IdSegment.Issuer.Region') -Because '地域码'
}

Test-Case 'mysql-init(dev 用集群内 MySQL):K=1 建基名和流水库,K=64 建全部片库和流水库,基名不再是库' {
    Assert-Equal -Expected 0 -Actual $devInfraMysqlRun.ExitCode -Because "输出: $($devInfraMysqlRun.Output)"
    Assert-Equal -Expected 0 -Actual $shardedInfraMysqlRun.ExitCode -Because "输出: $($shardedInfraMysqlRun.Output)"
    $single = Select-ManifestByName -Output $devInfraMysqlRun.Output -Name 'mysql-init-sql'
    Assert-Match -Text $single -Pattern ([regex]::Escape('CREATE DATABASE IF NOT EXISTS `mmorpg_global`;')) -Because 'K=1 目录库是基名'
    Assert-Match -Text $single -Pattern ([regex]::Escape('CREATE DATABASE IF NOT EXISTS `mmorpg_global_ledger`;')) -Because '流水库单独一个'
    Assert-NotMatch -Text $single -Pattern 'mmorpg_globalmmorpg_global' -Because '库名不能连成一个字符串'
    $sharded = Select-ManifestByName -Output $shardedInfraMysqlRun.Output -Name 'mysql-init-sql'
    foreach ($database in @('mmorpg_global_00', 'mmorpg_global_31', 'mmorpg_global_63', 'mmorpg_global_ledger')) {
        Assert-Match -Text $sharded -Pattern ([regex]::Escape("CREATE DATABASE IF NOT EXISTS ``$database``;")) -Because "要建 $database,-migrate 不建库"
        Assert-Match -Text $sharded -Pattern ([regex]::Escape("GRANT ALL PRIVILEGES ON ``$database``.*")) -Because "$database 和存档库同一口径授权"
    }
    Assert-NotMatch -Text $sharded -Pattern ([regex]::Escape('CREATE DATABASE IF NOT EXISTS `mmorpg_global`;')) -Because 'K>1 基名不是库'
    Assert-Equal -Expected 65 -Actual ([regex]::Matches($sharded, 'CREATE DATABASE IF NOT EXISTS `mmorpg_global_').Count) -Because '64 片加流水库'
    # 地域存档库(设计 player-data-tidb-scaling-plan.md §7.2):dev 缺省地域 0、K=1,预建的就是清单里那一个,与 Assert-PlayerStoreAllowlist 同一公式。
    foreach ($initSql in @($single, $sharded)) {
        Assert-Match -Text $initSql -Pattern ([regex]::Escape('CREATE DATABASE IF NOT EXISTS `player_store_1000000_db`;')) -Because 'dev 预建地域 0 的 0 号存档库,db-migrate 不建库'
        Assert-Match -Text $initSql -Pattern ([regex]::Escape('GRANT ALL PRIVILEGES ON `player_store_1000000_db`.*')) -Because '存档库预建之后授权'
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($initSql, 'CREATE DATABASE IF NOT EXISTS `player_store_').Count) -Because 'K=1 只有一个存档库'
        Assert-NotMatch -Text $initSql -Pattern 'CREATE DATABASE IF NOT EXISTS `?zone_' -Because 'zone 库已退役(A17c),mysql-init 一个都不建'
    }
}

Test-Case '集群内单实例 MySQL 只在 dev 的 mysql 档、入口就是它时部署;staging/prod 或 tidb 档把入口指到它都在写之前拒绝' {
    Assert-Equal -Expected 0 -Actual $devInfraRun.ExitCode -Because "输出: $($devInfraRun.Output)"
    Assert-NotMatch -Text $devInfraRun.Output -Pattern '(?m)^  name: mysql(-init-sql)?\s*$' -Because 'tidb 档的入口是外部 TiDB,不在集群里留一台没人用的单实例 MySQL'
    Assert-Match -Text $devInfraMysqlRun.Output -Pattern '(?m)^  name: mysql\s*$' -Because 'dev 的 mysql 档入口就是它,照旧部署'
    Assert-Match -Text $devInfraMysqlRun.Output -Pattern '(?m)^  name: mysql-init-sql\s*$' -Because 'initdb 脚本跟着它走'

    $prodArgs = @($ProdZoneArgs | ForEach-Object { if ($_ -ceq $AuthorityHost) { $InClusterMysqlHost } else { $_ } }) + @('-DirectoryLogicalShards', '64', '-Region', '7', '-DatabaseBackend', 'mysql', '-SkipJavaSvc')
    $prod = Invoke-LogicalDbDryRun -Arguments $prodArgs -Env $ProdEnv
    Assert-True -Condition ($prod.ExitCode -ne 0) -Because "prod 不能用 1 副本、没有主从的集群内 MySQL。输出: $($prod.Output)"
    Assert-Match -Text $prod.Output -Pattern 'is the in-cluster MySQL' -Because '点名原因'
    Assert-NotMatch -Text $prod.Output -Pattern '--- BEGIN MANIFEST ---' -Because '在写路径入口拒绝'

    $mismatch = Invoke-LogicalDbDryRun -Arguments @($ZoneArgs | ForEach-Object { if ($_ -ceq $AuthorityHost) { $InClusterMysqlHost } else { $_ } })
    Assert-True -Condition ($mismatch.ExitCode -ne 0) -Because "入口是 MySQL、档位却是 tidb,服务启动核对引擎必然失败。输出: $($mismatch.Output)"
    Assert-Match -Text $mismatch.Output -Pattern 'but -DatabaseBackend is tidb' -Because '点名缺 -DatabaseBackend mysql'
    Assert-NotMatch -Text $mismatch.Output -Pattern '--- BEGIN MANIFEST ---' -Because '在写路径入口拒绝'

    foreach ($manifest in @('data-service-migrate.yaml', 'db-migrate.yaml', 'friend-migrate.yaml', 'player-inbox-migrate.yaml', 'trade-migrate.yaml')) {
        $text = Read-RepoText "deploy/k8s/manifests/go-svc/$manifest"
        Assert-Match -Text $text -Pattern 'nc -z -w 3 "\$SQL_HOST" "\$SQL_PORT"' -Because "$manifest 等的是实际 SQL 入口"
        Assert-NotMatch -Text $text -Pattern 'mysql\.__INFRA_NAMESPACE__|\b3306\b' -Because "$manifest 不再写死集群内 MySQL"
    }
    $dbJob = @(Get-ManifestBlocks -Output $devZoneRun.Output | Where-Object { $_ -cmatch '(?m)^kind: Job\s*$' -and $_ -cmatch '(?m)^  name: db-migrate\s*$' })[0]
    Assert-Match -Text $dbJob -Pattern 'name: SQL_HOST\s+value: "tidb\.invalid"' -Because 'tidb 档等外部 TiDB'
    Assert-Match -Text $dbJob -Pattern 'name: SQL_PORT\s+value: "4000"' -Because '端口跟着入口'
}

Test-Case 'DbPlacement 段的位置和 Go 的配置结构一致' {
    $placements = @(
        @{ Output = $devInfraRun.Output; ConfigMap = 'go-svc-guild-config'; Key = 'data.guild.yaml.MySQL.DbPlacement' }
        @{ Output = $devInfraRun.Output; ConfigMap = 'go-svc-trade-config'; Key = 'data.trade.yaml.MySQL.DbPlacement' }
        @{ Output = $devInfraRun.Output; ConfigMap = 'go-svc-friend-config'; Key = 'data.friend.yaml.MySQL.DbPlacement' }
        @{ Output = $devInfraRun.Output; ConfigMap = 'go-svc-player-inbox-config'; Key = 'data.player_inbox.yaml.MySQL.DbPlacement' }
        @{ Output = $devInfraRun.Output; ConfigMap = 'go-svc-match-config'; Key = 'data.match_service.yaml.GlobalMatch.DbPlacement' }
        @{ Output = $devInfraRun.Output; ConfigMap = 'go-svc-chat-config'; Key = 'data.chat.yaml.DbPlacement' }
        @{ Output = $devInfraRun.Output; ConfigMap = 'go-svc-mail-config'; Key = 'data.mail.yaml.DbPlacement' }
        @{ Output = $devInfraRun.Output; ConfigMap = 'go-svc-rank-config'; Key = 'data.rank.yaml.DbPlacement' }
        @{ Output = $devZoneRun.Output; ConfigMap = 'go-svc-data-service-config'; Key = 'data.data_service.yaml.SnapshotMySQL.DbPlacement' }
        @{ Output = $devZoneRun.Output; ConfigMap = 'go-svc-db-config'; Key = 'data.db.yaml.DbPlacement' }
    )
    foreach ($placement in $placements) {
        $flat = Get-ConfigFlat -Output $placement.Output -ConfigMap $placement.ConfigMap
        Assert-Equal -Expected 'main' -Actual (Get-FlatValue -Flat $flat -KeyPath "$($placement.Key).DefaultCluster") -Because "$($placement.ConfigMap) 的默认集群"
        Assert-Equal -Expected $AuthorityHost -Actual (Get-FlatValue -Flat $flat -KeyPath "$($placement.Key).SqlClusters.main.Hosts") -Because "$($placement.ConfigMap) 的默认集群是权威入口"
        Assert-Equal -Expected "etcd.${Infra}:2379" -Actual (Get-FlatValue -Flat $flat -KeyPath "$($placement.Key).EtcdHosts[0]") -Because "$($placement.ConfigMap) 的映射读 infra 的 etcd"
    }
    # Go 侧就是这些位置(手册 docs/ops/db-move-runbook.md §2);配置结构变了这里要跟着改。
    foreach ($source in @('go/guild/internal/config/config.go', 'go/data_service/internal/config/config.go', 'go/db/internal/config/config.go', 'go/chat/internal/config/config.go')) {
        Assert-Match -Text (Read-RepoText $source) -Pattern 'DbPlacement\b' -Because "$source 要有 DbPlacement"
    }
}

Test-Case '-SqlClusters 进全部写者的 DbPlacement 段' {
    $run = Invoke-LogicalDbDryRun -Arguments ($ZoneArgs + @('-SqlClusters', "a=$AuthorityHost,b=tidb-b.invalid:4000", '-SqlDefaultCluster', 'a'))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    $flat = Get-ConfigFlat -Output $run.Output -ConfigMap 'go-svc-db-config'
    Assert-Equal -Expected 'a' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.db.yaml.DbPlacement.DefaultCluster') -Because '默认集群'
    Assert-Equal -Expected 'tidb-b.invalid:4000' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.db.yaml.DbPlacement.SqlClusters.b.Hosts') -Because '目标集群也要在清单里,翻转之后才连得上'
    $bad = Invoke-LogicalDbDryRun -Arguments ($ZoneArgs + @('-SqlClusters', 'a=other.invalid:4000', '-SqlDefaultCluster', 'a'))
    Assert-True -Condition ($bad.ExitCode -ne 0) -Because '默认集群不是权威入口要拒绝'
    Assert-Match -Text $bad.Output -Pattern 'must point at -GlobalAuthorityTiDBHost' -Because '点名'
    Assert-NotMatch -Text $bad.Output -Pattern '--- BEGIN MANIFEST ---' -Because '在写路径入口拒绝'
}

Test-Case 'MMORPG_DB_PLACEMENT 进全部 zone 写者的 registrar init,只进 init;login 和 gateway 的 mmorpg 同样按映射拨号' {
    # login、gateway 登记在 mmorpg 上,这个库 2026-11-05 起和其余逻辑库一样在 TiDB、按库映射连(docs/design/no-single-node-horizontal-scaling-20261001.md §6)。
    foreach ($service in @('data-service', 'db', 'login', 'gateway')) {
        $block = Get-DeploymentBlock -Output $devZoneRun.Output -Name $service
        $at = $block.IndexOf('- name: MMORPG_DB_PLACEMENT')
        Assert-True -Condition ($at -ge 0) -Because "$service 的 writer_authority 要按映射拨号"
        Assert-True -Condition ($at -lt $block.IndexOf('- name: MMORPG_SQL_REGISTRAR_DSN')) -Because "${service}:紧贴 registrar 的控制 DSN"
        Assert-True -Condition ($at -lt $block.IndexOf("`n      containers:")) -Because "${service}:常驻容器读 yaml 的 DbPlacement,不要这个变量"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, 'name: MMORPG_DB_PLACEMENT').Count) -Because "${service}:只注入一次"
        $value = [regex]::Match($block, "name: MMORPG_DB_PLACEMENT\s+value: '(?<json>[^'\r\n]+)'").Groups['json'].Value
        $json = $value | ConvertFrom-Json
        Assert-Equal -Expected 'main' -Actual $json.DefaultCluster -Because "${service}:值是 ConfigFromEnv 认的形状"
        Assert-Equal -Expected $AuthorityHost -Actual $json.SqlClusters.main.Hosts -Because "${service}:默认集群是权威入口"
        Assert-NotMatch -Text $value -Pattern '@tcp|password' -Because "${service}:只有地址,没有凭据"
    }
    # 网关的 init 由 Java 版的 registrar 注入(RuntimeKind java),锚点同一个 MMORPG_SQL_REGISTRAR_DSN;不能落进主容器。
    $gateway = Get-DeploymentBlock -Output $devZoneRun.Output -Name 'gateway'
    Assert-True -Condition ($gateway.IndexOf('- name: MMORPG_DB_PLACEMENT') -gt $gateway.IndexOf("`n      initContainers:")) -Because 'gateway:在 init 段里'
    Assert-Match -Text (Read-RepoText 'go/shared/dbplacement/placement.go') -Pattern 'EnvConfig = "MMORPG_DB_PLACEMENT"' -Because '变量名和 Go 一致'
}

Test-Case 'chat、mail、db 的内部 RPC 配置:chat 和 db 走 etcd 发现目录,mail 直连 guild Service' {
    $chat = Get-ConfigFlat -Output $devInfraRun.Output -ConfigMap 'go-svc-chat-config'
    Assert-Equal -Expected 'dataservice.rpc' -Actual (Get-FlatValue -Flat $chat -KeyPath 'data.chat.yaml.DataServiceRpc.Etcd.Key') -Because 'chat 读目录的 GetRoleFenceState'
    $mail = Get-ConfigFlat -Output $devInfraRun.Output -ConfigMap 'go-svc-mail-config'
    Assert-Equal -Expected "[guild.${Infra}:50300]" -Actual (Get-FlatValue -Flat $mail -KeyPath 'data.mail.yaml.GuildRpc.Endpoints') -Because 'guild 不写 Etcd.Key(D-13),直连 Service'
    Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $mail -KeyPath 'data.mail.yaml.GuildRpc.NonBlock') -Because 'guild 没起时邮件照常启动'
    $db = Get-ConfigFlat -Output $devZoneRun.Output -ConfigMap 'go-svc-db-config'
    Assert-Equal -Expected 'dataservice.rpc' -Actual (Get-FlatValue -Flat $db -KeyPath 'data.db.yaml.DataServiceRpc.Etcd.Key') -Because 'go/db 选库调 GetPlayerRoute'
    $guild = Get-ConfigFlat -Output $devInfraRun.Output -ConfigMap 'go-svc-guild-config'
    Assert-Equal -Expected 'matchservice.rpc' -Actual (Get-FlatValue -Flat $guild -KeyPath 'data.guild.yaml.MatchRpc.Etcd.Key') -Because 'guild 调 match'
}

# ──────────────────────────────────────────────
# 4. 参与者和密钥(从 Go 源码核对)
# ──────────────────────────────────────────────
Test-Case '参与者清单等于 go/shared/rolefence 的 Participants,地址是 infra 的 K8s Service' {
    Assert-Equal -Expected 6 -Actual $GoParticipants.Count -Because "Go 的名单(前提): $($GoParticipants -join ',')"
    Assert-Equal -Expected (($GoParticipants | Sort-Object) -join ',') -Actual (@($ParticipantServices.Keys | Sort-Object) -join ',') -Because '本文件的对照表要和 Go 一致,加参与者时先改这里'
    $config = Get-ConfigBlock -Output $devZoneRun.Output -ConfigMap 'go-svc-data-service-config'
    $rendered = @([regex]::Matches($config, '(?m)^\s+- Name: ([a-z_]+)\s*\r?\n\s+Endpoints: \["([^"]+)"\]') | ForEach-Object { [pscustomobject]@{ Name = $_.Groups[1].Value; Endpoint = $_.Groups[2].Value } })
    Assert-Equal -Expected (($GoParticipants | Sort-Object) -join ',') -Actual (@($rendered.Name | Sort-Object) -join ',') -Because '少配一个时过户和删角一律拒绝'
    foreach ($item in $rendered) {
        $service = $ParticipantServices[$item.Name]
        Assert-Equal -Expected "$($service.Service).${Infra}:$($service.Port)" -Actual $item.Endpoint -Because "$($item.Name) 在 infra namespace,data-service 在 zone namespace,要写全 Service 名"
        Assert-Match -Text (Read-RepoText "deploy/k8s/manifests/go-svc/$($service.Service).yaml") -Pattern "containerPort: $($service.Port)\b" -Because "$($item.Name) 的 manifest 监听这个端口"
    }
    $local = Read-RepoText 'go/data_service/etc/data_service.yaml'
    foreach ($participant in $GoParticipants) {
        Assert-Match -Text $local -Pattern "(?m)^\s+- Name: $participant\s*\r?\n\s+Endpoints: \[127\.0\.0\.1:$($ParticipantServices[$participant].Port)\]" -Because "本机配置同样列 $participant,端口和 go_services 一致"
    }
}

Test-Case '受 gRPC 准入保护的角色护栏参与者都放行 data-service,否则 -NetworkPolicies on 时协调者连不上' {
    $node = $deployAst.Find({ param($a) $a -is [Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -eq '$script:GrpcAllowLists' }, $false)
    Assert-True -Condition ($null -ne $node) -Because '要有放行名单'
    $lists = & ([scriptblock]::Create($node.Right.Extent.Text))
    # 参与者里有准入策略的那几个(player_inbox 没有客户端入口,不设准入)。2026-11-12 起 trade、mail、friend 也受保护。
    foreach ($name in @('guild', 'match', 'trade', 'mail', 'friend')) {
        Assert-True -Condition ($lists.Contains($name)) -Because "$name 收玩家会话身份,要有准入名单"
        Assert-True -Condition ($lists[$name].AllowedApps -contains 'data-service') -Because "$name 是参与者"
    }
    Assert-True -Condition ($lists['guild'].AllowedApps -contains 'mail') -Because 'mail 读玩家所在的帮'
}

function Get-ManifestRoleFenceSecrets {
    param([Parameter(Mandatory = $true)][string]$Service)
    $text = Read-RepoText "deploy/k8s/manifests/go-svc/$Service.yaml"
    return @([regex]::Matches($text, '(?m)^\s+- name: (MMORPG_ROLE_FENCE_SECRET_[A-Z_]+)\s*$') | ForEach-Object { $_.Groups[1].Value } | Sort-Object)
}

Test-Case 'manifest 的密钥名和 Go 一致,都引用 domain-service-secrets 且 optional: false' {
    Assert-Match -Text $GuardSource -Pattern 'CoordinatorSecretEnv = "MMORPG_ROLE_FENCE_SECRET_COORDINATOR"' -Because '协调者的密钥名(前提)'
    Assert-Match -Text $GuardSource -Pattern 'secretEnvPrefix = "MMORPG_ROLE_FENCE_SECRET_"' -Because '调用方密钥的前缀(前提)'
    Assert-Match -Text $RouteClientSource -Pattern 'RouteSecretEnv = "MMORPG_ROLE_FENCE_SECRET_DB"' -Because 'go/db 的密钥名(前提)'
    $callerEnvs = @($GoCallers | ForEach-Object { "MMORPG_ROLE_FENCE_SECRET_$($_.ToUpperInvariant())" })
    $all = @(@($callerEnvs) + 'MMORPG_ROLE_FENCE_SECRET_COORDINATOR' + 'MMORPG_ROLE_FENCE_SECRET_DB' | Sort-Object)
    Assert-Equal -Expected ($all -join ',') -Actual ((Get-ManifestRoleFenceSecrets -Service 'data-service') -join ',') -Because 'data-service 要全部九把'
    foreach ($participant in $GoParticipants) {
        $service = $ParticipantServices[$participant].Service
        $expected = @("MMORPG_ROLE_FENCE_SECRET_$($participant.ToUpperInvariant())", 'MMORPG_ROLE_FENCE_SECRET_COORDINATOR' | Sort-Object)
        Assert-Equal -Expected ($expected -join ',') -Actual ((Get-ManifestRoleFenceSecrets -Service $service) -join ',') -Because "$service 是参与者:自己那把加协调者那把"
    }
    Assert-Equal -Expected 'MMORPG_ROLE_FENCE_SECRET_CHAT' -Actual ((Get-ManifestRoleFenceSecrets -Service 'chat') -join ',') -Because 'chat 只读,不是参与者'
    Assert-Equal -Expected 'MMORPG_ROLE_FENCE_SECRET_DB' -Actual ((Get-ManifestRoleFenceSecrets -Service 'db') -join ',') -Because 'go/db 只调 GetPlayerRoute'
    foreach ($service in @('data-service', 'guild', 'trade', 'mail', 'match', 'player-inbox', 'friend', 'chat', 'db')) {
        $text = Read-RepoText "deploy/k8s/manifests/go-svc/$service.yaml"
        foreach ($envName in (Get-ManifestRoleFenceSecrets -Service $service)) {
            $secretKey = 'role-fence-' + $envName.Substring('MMORPG_ROLE_FENCE_SECRET_'.Length).ToLowerInvariant().Replace('_', '-')
            Assert-Match -Text $text -Pattern ("name: $envName\s+valueFrom:\s+secretKeyRef: \{name: domain-service-secrets, key: $secretKey, optional: false\}") -Because "$service 的 $envName"
        }
    }
    foreach ($service in @('guild', 'match')) {
        Assert-Match -Text (Read-RepoText "deploy/k8s/manifests/go-svc/$service.yaml") -Pattern 'name: MMORPG_MATCH_INTERNAL_SECRET_GUILD\s+valueFrom:\s+secretKeyRef: \{name: domain-service-secrets, key: match-internal-guild, optional: false\}' -Because "${service}:开局、占用、读终局都验这一把"
    }
}

Test-Case '本机开发密钥库的名单和 manifest 一致,go_services 接线,文件在被忽略的目录' {
    foreach ($service in @('data-service', 'guild', 'trade', 'mail', 'match', 'player-inbox', 'friend', 'chat', 'db')) {
        Assert-Equal -Expected ((Get-ManifestRoleFenceSecrets -Service $service) -join ',') -Actual (@(Get-RoleFenceDevSecretNames -Service $service | Sort-Object) -join ',') -Because "$service 本机和 K8s 同一套名字"
    }
    $launcher = Read-RepoText 'tools/scripts/go_services.ps1'
    Assert-Match -Text $launcher -Pattern 'lib\\role_fence_dev_secret\.ps1' -Because '顶层加载'
    Assert-Match -Text $launcher -Pattern 'Backup-RoleFenceDevSecrets -Service' -Because '起进程前备份'
    Assert-Match -Text $launcher -Pattern 'Initialize-RoleFenceDevSecrets -RepoRoot' -Because '注入本机开发值'
    Assert-Match -Text $launcher -Pattern 'Restore-RoleFenceDevSecrets' -Because 'finally 还原'
    $lib = Read-RepoText 'tools/scripts/lib/role_fence_dev_secret.ps1'
    Assert-Match -Text $lib -Pattern 'run/secrets/role-fence-dev\.env' -Because '放 run/secrets'
    Assert-NotMatch -Text $lib -Pattern 'Write-Host|Write-Output' -Because '不回显'
    $ignored = & git -C (Get-RepoRoot) check-ignore --quiet 'run/secrets/role-fence-dev.env' 2>$null
    Assert-Equal -Expected 0 -Actual $LASTEXITCODE -Because "run/secrets 被 git 忽略,真实密钥不进被跟踪的文件 $ignored"
}

# ──────────────────────────────────────────────
# 5. 连接总账
# ──────────────────────────────────────────────
Test-Case 'TiDB 档(目录 64 片、存档库 64 个)峰值不超 TiDB 预算,MySQL 档不超 mysql.yaml,按逻辑库拆开的账对得上' {
    $plan = Get-MysqlConnectionPlan
    $inputs = Get-MysqlConnectionLedgerInputs
    # MySQL 档:目录 1 片,存档库取规划档的小 K(设计 player-data-tidb-scaling-plan.md §7.2);mysql.yaml 的 max_connections 按这一档取值。
    $shape = @{ ZoneCount = $plan.ZoneCount; PlayerStoreShards = $plan.PlayerStoreShards; RolloutExtraPodsPerDeployment = $plan.RolloutExtraPodsPerDeployment; OpsReserve = $plan.OpsReserve }
    $single = Get-MysqlConnectionLedger -Inputs $inputs @shape
    Assert-True -Condition ($single.Peak -le $single.MaxConnections) -Because "MySQL 档规划峰值 $($single.Peak) 超过 mysql.yaml 的 $($single.MaxConnections)"
    Assert-Equal -Expected 1 -Actual $single.LogicalShards -Because '缺省 K=1'
    Assert-True -Condition (-not $plan.Contains('ExtraZoneStores')) -Because 'zone 库已退役(A17c),规划档没有 zone 库这一项'

    # TiDB 档:目录 64 片,存档库取 TidbPlayerStoreShards(生产初值 64),每库的池按 Get-PlayerStorePoolLimit 摊开。
    $tidbShape = @{} + $shape
    $tidbShape['PlayerStoreShards'] = $plan.TidbPlayerStoreShards
    $sharded = Get-MysqlConnectionLedger -Inputs $inputs @tidbShape -LogicalShards 64
    Assert-Equal -Expected 6 -Actual $sharded.DirectoryPerShard -Because 'K=64 时三个目录池每片各 2 条'
    $dbStorePool = Get-PlayerStorePoolLimit -Base ([int]$inputs.DbStoreMaxOpen) -Shards $plan.TidbPlayerStoreShards
    $dsStorePool = Get-PlayerStorePoolLimit -Base ([int]$inputs.DsStoragePoolMaxOpen) -Shards $plan.TidbPlayerStoreShards
    Assert-Equal -Expected "$dbStorePool/$dsStorePool" -Actual "$($sharded.DbStorePool)/$($sharded.DsStorePool)" -Because '每库存档池与 ConfigMap 同一条摊开规则'
    $expectedPerInstance = [int]$inputs.DsAuditStorePools * [int]$inputs.DsAuditMaxOpen + 64 * 6 + $plan.TidbPlayerStoreShards * $dsStorePool
    Assert-Equal -Expected $expectedPerInstance -Actual $sharded.DsPerInstance -Because 'ds 每实例 = 流水库池 + K × 每片 + 存档库数 × 每库存档池'
    Assert-Equal -Expected ($plan.TidbPlayerStoreShards * $dbStorePool) -Actual $sharded.DbPerInstance -Because 'db 每实例 = 存档库数 × 每库存档池(A17c 起 go/db 没有本区库)'
    Assert-True -Condition ($sharded.Peak -le $plan.TidbConnectionBudget) -Because "TiDB 档规划峰值 $($sharded.Peak) 超过 TiDB 预算 $($plan.TidbConnectionBudget),要收窄每片、每库上限或加 tidb-server"
    Assert-Equal -Expected ([int][Math]::Floor(($plan.TidbServers - 1) * $plan.TidbMaxConnectionsPerServer / $plan.TidbImbalanceFactor)) -Actual $plan.TidbConnectionBudget -Because '预算 = (N−1) × 每台上限 / 不均衡系数'

    foreach ($database in @('directory_shard', 'ledger', 'player_store', 'mmorpg', 'mmorpg_friend', 'mmorpg_trade', 'mmorpg_player_inbox', 'mmorpg_guild', 'mmorpg_mail', 'mmorpg_rank', 'mmorpg_chat', 'mmorpg_match')) {
        Assert-True -Condition ($sharded.PerLogicalDb.Contains($database) -and [int]$sharded.PerLogicalDb[$database] -gt 0) -Because "按逻辑库的账要有 $database"
    }
    Assert-True -Condition (-not $sharded.PerLogicalDb.Contains('zone_store')) -Because 'zone 库已退役(A17c),按逻辑库的账里没有 zone 主库'
    $dsInstances = $plan.ZoneCount * [int]$inputs.DsReplicas + $plan.RolloutExtraPodsPerDeployment
    $dbInstances = $plan.ZoneCount * [int]$inputs.DbReplicas + $plan.RolloutExtraPodsPerDeployment
    Assert-Equal -Expected ($dsInstances * 6) -Actual $sharded.PerLogicalDb['directory_shard'] -Because '每片的峰值 = data_service 实例数 × 每片连接'
    Assert-Equal -Expected ($dbInstances * $dbStorePool + $dsInstances * $dsStorePool) -Actual $sharded.PerLogicalDb['player_store'] -Because '每个地域存档库的峰值 = 全地域 go/db、data_service 实例数 × 每库存档池(搬库之前按它算目标集群的预算)'
    Assert-Match -Text (Get-ThrownMessage { Get-MysqlConnectionLedger -Inputs $inputs @shape -LogicalShards 5 }) -Pattern 'divisor of 64' -Because '片数非法拒绝'
}

# ──────────────────────────────────────────────
# 6. 告警
# ──────────────────────────────────────────────
Test-Case '告警:规则名单和形状,指标名与取值从 Go 源码核对,闸门冻结时长有规则' {
    $alerts = Read-RepoText 'deploy/k8s/logical-db-alerts.yaml'
    $names = @([regex]::Matches($alerts, '(?m)^\s+- alert: (\S+)') | ForEach-Object { $_.Groups[1].Value })
    Assert-Equal -Expected 'DbPlacementRejected,DbPlacementSyncFailing,DbPlacementVersionLagging,RoleFencePrepareStale,RoleFencePrepareUnreachable,RoleFencePushFailing,SqlWriterRegistrarFailing,SqlWriterSlotRetirerFailing,WriterGateFrozenTooLong' -Actual (($names | Sort-Object) -join ',') -Because '规则名单'
    # 缩容收尾 CronJob 的名字前缀与 k8s_deploy.ps1 起的名字一致,告警才对得上(docs/design/writer-roster-exhaustion-20261106.md §5.8)。
    Assert-Match -Text $alerts -Pattern 'kube_cronjob_status_last_successful_time\{cronjob=~"sql-writer-slot-retirer-\.\+"\}' -Because '缩容收尾一直失败要靠这条发现'
    Assert-Match -Text $alerts -Pattern 'kube_cronjob_created\{cronjob=~"sql-writer-slot-retirer-\.\+"\}' -Because '从没成功过的 CronJob 也要报'
    Assert-Match -Text (Read-RepoText 'tools/scripts/k8s_deploy.ps1') -Pattern '\$name = "sql-writer-slot-retirer-\$StatefulSet"' -Because '告警里的 CronJob 名前缀和部署脚本一致'
    Assert-Match -Text $alerts -Pattern 'kube_pod_init_container_status_restarts_total\{container="sql-writer-registrar"\}' -Because '回收 CronJob 不删 init 反复失败的 Pod,要靠这条发现,副本数比名册槽位多也在这里'
    Assert-Match -Text (Read-RepoText 'deploy/k8s/manifests/go-svc/data-service.yaml') -Pattern '(?m)^        - name: sql-writer-registrar\s*$' -Because '告警里的 init 容器名和清单一致'
    Assert-Match -Text $alerts -Pattern 'kube-state-metrics' -Because '抓取前提写明白,没有它这条是哑的'
    Assert-Equal -Expected $names.Count -Actual ([regex]::Matches($alerts, '(?m)^\s+for: \d+m\s*$').Count) -Because '每条都有 for'
    Assert-Equal -Expected $names.Count -Actual ([regex]::Matches($alerts, '(?m)^\s+severity: (critical|warning)\s*$').Count) -Because '每条都有 severity'
    Assert-Equal -Expected $names.Count -Actual ([regex]::Matches($alerts, '(?m)^\s+summary: ').Count) -Because '每条都有 summary'
    Assert-Equal -Expected $names.Count -Actual ([regex]::Matches($alerts, '(?m)^\s+description: ').Count) -Because '每条都有 description'
    Assert-Match -Text $alerts -Pattern '(?m)^kind: PrometheusRule\s*$' -Because '和别的告警文件同形'

    $placementSource = Read-RepoText 'go/shared/dbplacement/metrics.go'
    $fenceSource = Read-RepoText 'go/data_service/internal/metrics/role_fence.go'
    $gateSource = Read-RepoText 'go/shared/authoritysql/metrics.go'
    $subsystem = [regex]::Match((Read-RepoText 'go/data_service/internal/metrics/metrics.go'), 'const subsystem = "([^"]+)"').Groups[1].Value
    Assert-Equal -Expected 'data_service' -Actual $subsystem -Because '指标前缀(前提)'
    $known = @(@([regex]::Matches($placementSource + $gateSource, 'Name:\s+"([a-z_]+)"') | ForEach-Object { $_.Groups[1].Value }) + @([regex]::Matches($fenceSource, 'Name:\s+"([a-z_]+)"') | ForEach-Object { "${subsystem}_$($_.Groups[1].Value)" }))
    $used = @([regex]::Matches(($alerts -replace '(?m)^\s*#.*$', ''), '\b(db_placement_[a-z_]+|data_service_role_fence_[a-z_]+|writer_frozen_seconds)\b') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
    Assert-True -Condition ($used.Count -ge 5) -Because "规则确实用到这些指标(前提): $($used -join ',')"
    foreach ($metric in $used) { Assert-True -Condition ($metric -cin $known) -Because "$metric 不是代码里的指标,规则永远不响" }
    foreach ($value in @('unreachable', 'stale', 'failed')) {
        Assert-Match -Text $fenceSource -Pattern "= `"$value`"" -Because "result=`"$value`" 是代码里的取值"
        Assert-Match -Text $alerts -Pattern "result=`"$value`"" -Because "规则用 result=`"$value`""
    }
    Assert-True -Condition ('writer_frozen_seconds' -cin $used) -Because '冻结时长有规则,工具被杀靠它发现'
    Assert-Match -Text $alerts -Pattern 'db-move-runbook\.md §6' -Because '冻结告警指向处置步骤'
}

Test-Case 'staging、prod 的 schema Job 要求配置带 DbPlacement,dev 不要求' {
    $deploy = Read-RepoText 'tools/scripts/k8s_deploy.ps1'
    $function = [regex]::Match($deploy, '(?ms)^function Add-SchemaPermissionManifest \{.*?^\}')
    Assert-True -Condition $function.Success -Because '函数还在(前提)'
    $envName = [regex]::Match((Read-RepoText 'go/shared/dbplacement/placement.go'), 'const EnvRequired = "([A-Z_]+)"').Groups[1].Value
    Assert-Equal -Expected 'MMORPG_DB_PLACEMENT_REQUIRED' -Actual $envName -Because '环境变量名以 Go 为准'
    Assert-Match -Text $function.Value -Pattern ('if\(\$ReleaseProfile -cne ''dev''\)\{[^\r\n]*name: ' + $envName) -Because '只在非 dev 注入'
    Assert-Match -Text (Read-RepoText 'go/db/cmd/migrate/main.go') -Pattern 'dbplacement\.RequireForMigration\(' -Because 'go/db 的 -migrate 不走 ForMigration,要单独核'
}

# ──────────────────────────────────────────────
# 7. 旧键清零,包装入口转到底
# ──────────────────────────────────────────────
Test-Case '旧键从脚本、调试栈和渲染产物里清零' {
    $retired = 'AccountRoleSchema|MatchAuthoritySchema|GuildTrialSchema|RouteSchema|RoleTrade\w*Schema'
    foreach ($path in @('tools/scripts/k8s_deploy.ps1', 'tools/scripts/go_services.ps1', 'tools/scripts/lib/sql_boot_catalog_local.ps1', 'tools/scripts/prepare_docker_debug.py', 'deploy/debug/compose.game.yml')) {
        Assert-NotMatch -Text (Read-RepoText $path) -Pattern $retired -Because "$path 不再读写这些键"
    }
    foreach ($run in @($devZoneRun, $prodZoneRun, $devInfraRun)) {
        foreach ($block in (Get-ManifestBlocks -Output $run.Output)) {
            if ($block -cmatch '(?m)^kind: ConfigMap\s*$') { Assert-NotMatch -Text $block -Pattern $retired -Because 'ConfigMap 不再渲染这些键' }
        }
    }
    foreach ($service in @('chat', 'friend', 'guild', 'mail', 'match', 'player_inbox', 'trade', 'db', 'data_service')) {
        $file = if ($service -ceq 'match') { 'match_service.yaml' } else { "$service.yaml" }
        Assert-NotMatch -Text (Read-RepoText "go/$service/etc/$file") -Pattern '(?m)^\s*(AccountRoleSchema|MatchAuthoritySchema|GuildTrialSchema|RouteSchema):' -Because "$service 的配置没有这些键"
    }
}

Test-Case '包装入口:dev_tools 把逻辑库参数转到底,不粘滞' {
    $names = @('DirectoryLogicalShards', 'Region', 'IdIssuerRegion', 'PlayerStoreShards', 'SqlClusters', 'SqlDefaultCluster')
    foreach ($script in @('k8s_deploy.ps1', 'dev_tools.ps1')) {
        $ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) $script), [ref]$null, [ref]$null)
        $declared = @($ast.ParamBlock.Parameters | ForEach-Object { $_.Name.VariablePath.UserPath })
        foreach ($name in $names) { Assert-True -Condition ($declared -contains $name) -Because "$script 要有 -$name" }
    }
    $devTools = Read-RepoText 'tools/scripts/dev_tools.ps1'
    foreach ($name in $names) {
        Assert-Match -Text $devTools -Pattern "\`$args\.$name = \`$$name" -Because "dev_tools 转给 k8s_deploy:-$name"
    }
}

# ──────────────────────────────────────────────
# 8. 写者 Pod:私有目录、后端档位、地域码、回收
# ──────────────────────────────────────────────
$WriterConfigFiles = [ordered]@{ 'data-service' = 'data_service.yaml'; db = 'db.yaml'; login = 'login.yaml' }
$GlobalWriterConfigFiles = [ordered]@{ chat = 'chat.yaml'; friend = 'friend.yaml'; guild = 'guild.yaml'; mail = 'mail.yaml'; match = 'match_service.yaml'; 'player-inbox' = 'player_inbox.yaml'; rank = 'rank.yaml'; trade = 'trade.yaml' }

function Assert-WriterPrivatePaths {
    param([Parameter(Mandatory = $true)][string]$Block, [Parameter(Mandatory = $true)][string]$Service, [Parameter(Mandatory = $true)][string]$ConfigFile)
    $file = [regex]::Escape($ConfigFile)
    Assert-Match -Text $Block -Pattern 'umask 077; mkdir -p /run/sql-writer/grant/private /run/sql-writer/runtime/private/env;' -Because "${Service}:init 建私有子目录,挂载根过不了私有目录校验"
    Assert-Match -Text $Block -Pattern '"-boot-output","/run/sql-writer/grant/private/boot-grant\.json"' -Because "${Service}:grant 写进子目录"
    Assert-Match -Text $Block -Pattern ('"-runtime-config-output","/run/sql-writer/runtime/private/' + $file + '"') -Because "${Service}:选中配置写进子目录"
    Assert-Match -Text $Block -Pattern '"-runtime-env-output","/run/sql-writer/runtime/private/env"' -Because "${Service}:环境文件写进子目录"
    Assert-Match -Text $Block -Pattern 'name: MMORPG_SQL_WRITER_GRANT_FILE\s+value: /run/sql-writer/grant/private/boot-grant\.json' -Because "${Service}:主容器读子目录"
    Assert-Match -Text $Block -Pattern 'name: MMORPG_SQL_WRITER_CONSUME_DIR\s+value: /run/sql-writer/consume/private' -Because "${Service}:消费标记写子目录"
    Assert-Match -Text $Block -Pattern 'mmorpg\.io/sql-writer: "true"' -Because "${Service}:回收 CronJob 只处理带这个 label 的 Pod"
    Assert-NotMatch -Text $Block -Pattern ('/run/sql-writer/grant/boot-grant|/run/sql-writer/runtime/' + $file + '|/run/sql-writer/runtime/env|value: /run/sql-writer/consume\s') -Because "${Service}:不能再有写到挂载根的路径"
}

function Assert-GoWriterPrivatePaths {
    param([Parameter(Mandatory = $true)][string]$Block, [Parameter(Mandatory = $true)][string]$Service, [Parameter(Mandatory = $true)][string]$ConfigFile)
    Assert-WriterPrivatePaths -Block $Block -Service $Service -ConfigFile $ConfigFile
    $file = [regex]::Escape($ConfigFile)
    Assert-Match -Text $Block -Pattern '\(umask 077; mkdir -p /run/sql-writer/consume/private\);' -Because "${Service}:消费目录主容器自己建,umask 不带进服务进程"
    Assert-Match -Text $Block -Pattern 'if ls /run/sql-writer/consume/private/\*\.consumed >/dev/null 2>&1; then echo [^;]+; exit 78; fi;' -Because "${Service}:看到消费标记就退出 78,回收 CronJob 只删这种 Pod"
    Assert-Match -Text $Block -Pattern 'cat /run/sql-writer/grant/private/boot-grant\.json\.sha256' -Because "${Service}:摘要从子目录读"
    Assert-Match -Text $Block -Pattern ('"/app/etc/private/' + $file + '"') -Because "${Service}:-f 读子目录"
    Assert-NotMatch -Text $Block -Pattern ('"/app/etc/' + $file + '"|/app/etc/env/') -Because "${Service}:配置和环境文件不在挂载根"
}

Test-Case '写者的控制文件都在 emptyDir 挂载根下的私有子目录(挂载根过不了私有目录校验)' {
    foreach ($service in $WriterConfigFiles.Keys) {
        Assert-GoWriterPrivatePaths -Block (Get-DeploymentBlock -Output $devZoneRun.Output -Name $service) -Service $service -ConfigFile $WriterConfigFiles[$service]
    }
    foreach ($service in $GlobalWriterConfigFiles.Keys) {
        Assert-GoWriterPrivatePaths -Block (Get-DeploymentBlock -Output $devInfraRun.Output -Name $service) -Service $service -ConfigFile $GlobalWriterConfigFiles[$service]
    }
    $gateway = Get-DeploymentBlock -Output $devZoneRun.Output -Name 'gateway'
    Assert-WriterPrivatePaths -Block $gateway -Service 'gateway' -ConfigFile 'application-k8s.yaml'
    Assert-Match -Text $gateway -Pattern 'name: SPRING_CONFIG_ADDITIONAL_LOCATION\s+value: file:/app/config/private/' -Because 'Java 的选中配置在子目录,显式告诉 Spring'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($gateway, 'mmorpg\.io/sql-writer: "true"').Count) -Because 'label 只注入一次'
    $dockerfile = Read-RepoText 'deploy/k8s/Dockerfile.java-svc'
    Assert-Match -Text $dockerfile -Pattern '\(umask 077; mkdir -p \\"\$MMORPG_SQL_WRITER_CONSUME_DIR\\"\)' -Because 'Java 的消费目录由镜像入口建,不然 WriterAuthority 找不到私有目录'
    Assert-Match -Text $dockerfile -Pattern 'if ls \\"\$MMORPG_SQL_WRITER_CONSUME_DIR\\"/\*\.consumed >/dev/null 2>&1; then echo [^;]+; exit 78; fi;' -Because 'Java 读的时候就消费,任何一次重启都是 78'
    Assert-Match -Text $dockerfile -Pattern '\$MMORPG_SQL_WRITER_GRANT_FILE\.sha256' -Because '摘要跟着 grant 文件走,路径不写死'
    # 校验规则的前提:目录本身不能被组或其他人写,父目录靠 sticky 放行;Java 还要属主是自己。规则变了这里要重看。
    $marker = Read-RepoText 'go/shared/authoritysql/config_marker_other.go'
    Assert-Match -Text $marker -Pattern 'current == dir \|\| info\.Mode\(\)&os\.ModeSticky == 0' -Because 'sticky 的父目录放行,这是子目录方案成立的前提'
    Assert-Match -Text (Read-RepoText 'go/data_service/cmd/writer_authority/main.go') -Pattern 'CheckPrivateControlDirectory\(filepath\.Dir\(path\)\)' -Because 'registrar 的输出目录同样过这道校验'
    Assert-Match -Text (Read-RepoText 'java/authority_jdbc/src/main/java/com/game/authority/WriterAuthority.java') -Pattern 'value\.owner\(\)\.equals\(current\)' -Because 'Java 要求属主是自己,子目录由容器自己建才满足'
}

Test-Case 'registrar 带 MMORPG_SQL_BACKEND:全部写者跟 -DatabaseBackend(login、gateway 不再固定 mysql),只进 init' {
    Assert-Match -Text (Read-RepoText 'go/shared/authoritysql/backend.go') -Pattern 'MMORPG_SQL_BACKEND' -Because '变量名和 Go 一致'
    function Get-RegistrarBackend([string]$Block) {
        $found = [regex]::Matches($Block, 'name: MMORPG_SQL_BACKEND\s+value: "(?<v>[a-z]+)"')
        Assert-Equal -Expected 1 -Actual $found.Count -Because '只注入一次'
        Assert-True -Condition ($found[0].Index -lt $Block.IndexOf("`n      containers:")) -Because '只进 init,常驻进程的档位在自己的配置里'
        return $found[0].Groups['v'].Value
    }
    foreach ($service in @('data-service', 'db', 'login', 'gateway')) {
        Assert-Equal -Expected 'tidb' -Actual (Get-RegistrarBackend (Get-DeploymentBlock -Output $devZoneRun.Output -Name $service)) -Because "$service 跟 -DatabaseBackend,缺省 tidb(mmorpg 在 TiDB 上,§6)"
    }
    Assert-Equal -Expected 'tidb' -Actual (Get-RegistrarBackend (Get-DeploymentBlock -Output $devInfraRun.Output -Name 'guild')) -Because '全局服务同样跟 -DatabaseBackend'
    Assert-Equal -Expected 0 -Actual $devMysqlZoneRun.ExitCode -Because "输出: $($devMysqlZoneRun.Output)"
    foreach ($service in @('data-service', 'db', 'login', 'gateway')) {
        Assert-Equal -Expected 'mysql' -Actual (Get-RegistrarBackend (Get-DeploymentBlock -Output $devMysqlZoneRun.Output -Name $service)) -Because "mysql 档 $service 也是 mysql"
    }
}

# ──────────────────────────────────────────────
# 8b. mmorpg 在全球权威入口上(docs/design/no-single-node-horizontal-scaling-20261001.md §6)
# ──────────────────────────────────────────────
Test-Case '网关的 mmorpg 跟全球权威入口:档位写成 mmorpg.sql.backend,tidb 档核对 TiDB,mysql 档(含 prod)核对 MySQL 主库;不给入口在写之前拒绝' {
    $gatewaySource = Read-RepoText 'java/gateway_node/src/main/java/com/game/gateway/config/AuthorityDatabaseConfiguration.java'
    Assert-Match -Text $gatewaySource -Pattern 'AuthorityDataSource\.Backend\.resolve\(\s*environment\.getProperty\("mmorpg\.sql\.backend","tidb"\)' -Because '网关按 mmorpg.sql.backend 取档位(前提)'
    $authoritySource = Read-RepoText 'java/authority_jdbc/src/main/java/com/game/authority/AuthorityDataSource.java'
    Assert-Match -Text $authoritySource -Pattern 'case "mysql":\s+return MYSQL;' -Because 'mysql 档在任何运行模式下都按主库严格核对(规则只在 Backend.resolve)'
    Assert-Match -Text $authoritySource -Pattern 'Set\.of\("dev","test"\)\.contains\(runMode\)' -Because 'tidb 档关掉强核验仍只许 dev / test'
    foreach ($case in @(
            @{ Label = 'tidb 档'; Run = $devZoneRun; Backend = 'tidb'; RequireTidb = 'true' }
            @{ Label = 'dev 的 mysql 档'; Run = $devMysqlZoneRun; Backend = 'mysql'; RequireTidb = 'false' }
        )) {
        Assert-Equal -Expected 0 -Actual $case.Run.ExitCode -Because "$($case.Label) 输出: $($case.Run.Output)"
        $flat = Get-ConfigFlat -Output $case.Run.Output -ConfigMap 'java-svc-gateway-config'
        $url = Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.spring.datasource.url'
        Assert-True -Condition ($url.StartsWith("jdbc:mysql://$AuthorityHost/mmorpg?")) -Because "$($case.Label):网关连的就是 login 登记的那个 mmorpg,实际 $url"
        Assert-Equal -Expected $case.Backend -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.mmorpg.sql.backend') -Because "$($case.Label) 的核对档位"
        Assert-Equal -Expected $case.RequireTidb -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.mmorpg.sql.require-tidb') -Because "$($case.Label):require-tidb 只对 tidb 档有意义"
        Assert-Equal -Expected 'prod' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.mmorpg.run-mode') -Because "$($case.Label):不靠 dev 模式放宽核验"
    }
    $prodFlat = Get-ConfigFlat -Output $prodZoneRun.Output -ConfigMap 'java-svc-gateway-config'
    Assert-Equal -Expected 'tidb' -Actual (Get-FlatValue -Flat $prodFlat -KeyPath 'data.application.yaml.mmorpg.sql.backend') -Because 'prod 的 tidb 档'
    Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $prodFlat -KeyPath 'data.application.yaml.mmorpg.sql.require-tidb') -Because 'prod 的 tidb 档同样开强核验'

    # 不给入口:Go 服务那边早就所有档位都要显式入口;只部署网关(-SkipGoSvc)时同样拒绝,不回落 infra MySQL,否则 mmorpg 分成两份。
    $hostAt = [array]::IndexOf($ZoneArgs, '-GlobalAuthorityTiDBHost')
    Assert-True -Condition ($hostAt -gt 0 -and $ZoneArgs[$hostAt + 1] -ceq $AuthorityHost) -Because '夹具前提:$ZoneArgs 带入口'
    $noHostArgs = @($ZoneArgs[0..($hostAt - 1)]) + @($ZoneArgs[($hostAt + 2)..($ZoneArgs.Count - 1)]) + @('-SkipGoSvc')
    $noHost = Invoke-LogicalDbDryRun -Arguments $noHostArgs
    Assert-True -Condition ($noHost.ExitCode -ne 0) -Because "dev 不给入口也要拒绝。输出: $($noHost.Output)"
    Assert-Match -Text $noHost.Output -Pattern 'every release profile needs -GlobalAuthorityTiDBHost' -Because '点名缺的参数'
    Assert-NotMatch -Text $noHost.Output -Pattern '--- BEGIN MANIFEST ---' -Because '在写路径入口拒绝'

    # MySQL 主从省钱档在 prod 也能部署网关:Java 的 mysql 档逐条物理连接核对端是可写的 MySQL 主库。入口是外部主库,建表 Job 照跑。
    # prod 连外部主库要给那台主库的 max_connections(连接总账的上限,见上面「SQL 连接总账的上限」那条用例)。
    $prodMysql = Invoke-LogicalDbDryRun -Arguments ($ProdZoneArgs + @('-DirectoryLogicalShards', '64', '-Region', '7', '-DatabaseBackend', 'mysql', '-MysqlMaxConnections', '12000')) -Env $ProdEnv
    Assert-Equal -Expected 0 -Actual $prodMysql.ExitCode -Because "prod 的 mysql 档。输出: $($prodMysql.Output)"
    $prodMysqlFlat = Get-ConfigFlat -Output $prodMysql.Output -ConfigMap 'java-svc-gateway-config'
    Assert-Equal -Expected 'mysql' -Actual (Get-FlatValue -Flat $prodMysqlFlat -KeyPath 'data.application.yaml.mmorpg.sql.backend') -Because 'prod 的 mysql 档按主库核对'
    Assert-Equal -Expected 'prod' -Actual (Get-FlatValue -Flat $prodMysqlFlat -KeyPath 'data.application.yaml.mmorpg.run-mode') -Because '不靠 dev 模式放宽'
    $prodMysqlJob = @(Get-ManifestBlocks -Output $prodMysql.Output | Where-Object { $_ -cmatch '(?m)^kind: Job\s*$' -and $_ -cmatch '(?m)^  name: gateway-schema\s*$' })
    Assert-Equal -Expected 1 -Actual $prodMysqlJob.Count -Because '外部 MySQL 主库上同样要建表'
    Assert-Match -Text $prodMysqlJob[0] -Pattern 'name: MMORPG_SQL_BACKEND\s+value: "mysql"' -Because 'writer_authority 按 mysql 档连'
}

Test-Case '网关建表 Job:registrar 镜像跑 prepare-schema,DDL 就是网关的 schema.sql 加 login 的口令凭据表,先于网关 Deployment;只有入口是集群内 MySQL 时不渲染' {
    Assert-Match -Text (Read-RepoText 'go/data_service/cmd/writer_authority/main.go') -Pattern 'flags\.Var\(&ddlFiles, "ddl-file"' -Because 'writer_authority 有可重复的 -ddl-file(前提)'
    $jobs = @(Get-ManifestBlocks -Output $devZoneRun.Output | Where-Object { $_ -cmatch '(?m)^kind: Job\s*$' -and $_ -cmatch '(?m)^  name: gateway-schema\s*$' })
    Assert-Equal -Expected 1 -Actual $jobs.Count -Because 'tidb 档恰好一份'
    $job = $jobs[0]
    Assert-NotMatch -Text $job -Pattern 'PLACEHOLDER_' -Because '占位全部替换'
    Assert-Match -Text $job -Pattern ('(?m)^\s+image: ' + [regex]::Escape('fixture.invalid/registrar@sha256:' + ('a' * 64)) + '\s*$') -Because '镜像是 boot 目录 gateway 条目的 registrarImage,按摘要固定'
    Assert-Match -Text $job -Pattern 'args: \["-command", "prepare-schema", "-schema", "mmorpg", "-ddl-file", "/app/gateway-schema/schema\.sql", "-ddl-file", "/app/gateway-schema/login-password-auth\.sql", "-timeout", "60s"\]' -Because '闸门表和两个属主的业务表一次建完:先网关的区目录表,再 login 的口令凭据表'
    foreach ($key in @('dsn', 'principal')) {
        Assert-Match -Text $job -Pattern ('valueFrom: \{secretKeyRef: \{name: fixture-schema-permission, key: ' + $key + ', optional: false\}\}') -Because "schema 身份的 $key 来自 schemaPermissionSecret,不用网关常驻账号"
    }
    Assert-Match -Text $job -Pattern 'name: MMORPG_SQL_BACKEND\s+value: "tidb"' -Because 'TiDB 档'
    $placement = [regex]::Match($job, "name: MMORPG_DB_PLACEMENT\s+value: '(?<json>[^'\r\n]+)'").Groups['json'].Value | ConvertFrom-Json
    Assert-Equal -Expected $AuthorityHost -Actual $placement.SqlClusters.main.Hosts -Because '按库映射拨号,默认集群是权威入口'
    Assert-Match -Text $job -Pattern 'name: MMORPG_DB_PLACEMENT_REQUIRED\s+value: ""' -Because 'dev 不要求映射'
    $prodJob = @(Get-ManifestBlocks -Output $prodZoneRun.Output | Where-Object { $_ -cmatch '(?m)^kind: Job\s*$' -and $_ -cmatch '(?m)^  name: gateway-schema\s*$' })
    Assert-Equal -Expected 1 -Actual $prodJob.Count -Because 'prod 同样渲染'
    Assert-Match -Text $prodJob[0] -Pattern 'name: MMORPG_DB_PLACEMENT_REQUIRED\s+value: "1"' -Because 'staging、prod 要求映射,同其余 schema Job'
    Assert-Match -Text $job -Pattern 'mountPath: /app/gateway-schema, readOnly: true' -Because '-ddl-file 读挂载的 ConfigMap'
    Assert-Match -Text $job -Pattern 'configMap: \{name: gateway-schema-sql\}' -Because '挂的就是渲染出的 DDL ConfigMap'

    # ConfigMap 每个键的内容逐行等于它的来源文件:每张表的建表语句只有一份来源。键的先后就是 Job 里 -ddl-file 的先后。
    $configMap = (Get-ConfigBlock -Output $devZoneRun.Output -ConfigMap 'gateway-schema-sql') -replace "`r`n", "`n"
    $ddlSources = [ordered]@{
        'schema.sql'              = 'java/gateway_node/src/main/resources/schema.sql'
        'login-password-auth.sql' = 'go/login/model/password_auth_schema.sql'
    }
    $ddlKeys = @([regex]::Matches($configMap, '(?m)^  (?<key>[A-Za-z0-9._-]+): \|$') | ForEach-Object { $_.Groups['key'].Value })
    Assert-Equal -Expected (@($ddlSources.Keys) -join ',') -Actual ($ddlKeys -join ',') -Because 'ConfigMap 的键就是这两个 DDL 文件,先后与 Job 的 -ddl-file 相同'
    foreach ($key in $ddlSources.Keys) {
        $body = [regex]::Match($configMap, '(?ms)^  ' + [regex]::Escape($key) + ': \|\n(?<body>.*?)(?=\n  [A-Za-z0-9._-]+: \|\n|\z)').Groups['body'].Value
        $rendered = @($body.TrimEnd("`n") -split "`n" | ForEach-Object { $_ -replace '^    ', '' }) -join "`n"
        $expected = ((Read-RepoText $ddlSources[$key]) -replace "`r`n", "`n").TrimEnd("`n")
        Assert-Equal -Expected $expected -Actual $rendered -Because "DDL ConfigMap 的 $key 就是 $($ddlSources[$key])"
    }

    $applied = $devZoneRun.Output.IndexOf('[applied] gateway-schema -> fixture.invalid/registrar@sha256:')
    Assert-True -Condition ($applied -ge 0) -Because '打印镜像'
    Assert-True -Condition ($applied -lt $devZoneRun.Output.IndexOf('[applied] gateway -> ')) -Because 'Job 先于网关 Deployment:网关按 ddl-auto: validate 启动,表要先在'

    # mysql 档的入口是外部主库(这里是 $AuthorityHost):照样建表,writer_authority 按 mysql 档连。
    $mysqlJob = @(Get-ManifestBlocks -Output $devMysqlZoneRun.Output | Where-Object { $_ -cmatch '(?m)^kind: Job\s*$' -and $_ -cmatch '(?m)^  name: gateway-schema\s*$' })
    Assert-Equal -Expected 1 -Actual $mysqlJob.Count -Because '外部 MySQL 主库没有 mysql-init,建表要靠这个 Job'
    Assert-Match -Text $mysqlJob[0] -Pattern 'name: MMORPG_SQL_BACKEND\s+value: "mysql"' -Because '档位跟 -DatabaseBackend'
    # 入口就是集群内单实例 MySQL(只在 dev):mysql-init 的 gateway_tables.sql 建这个库,不跑 Job。
    $inCluster = Invoke-LogicalDbDryRun -Arguments (@($ZoneArgs | ForEach-Object { if ($_ -ceq $AuthorityHost) { $InClusterMysqlHost } else { $_ } }) + @('-DatabaseBackend', 'mysql'))
    Assert-Equal -Expected 0 -Actual $inCluster.ExitCode -Because "dev 用集群内 MySQL。输出: $($inCluster.Output)"
    Assert-NotMatch -Text $inCluster.Output -Pattern '(?m)^  name: gateway-schema(-sql)?\s*$' -Because '集群内 MySQL 由 mysql-init 建这个库'
    $inClusterFlat = Get-ConfigFlat -Output $inCluster.Output -ConfigMap 'java-svc-gateway-config'
    Assert-Equal -Expected 'mysql' -Actual (Get-FlatValue -Flat $inClusterFlat -KeyPath 'data.application.yaml.mmorpg.sql.backend') -Because '集群内 MySQL 同样按主库核对'
}

Test-Case 'mmorpg 的两条建表路径不漂移:gateway_tables.sql(本机与集群内开发 MySQL)原样包含建表 Job 执行的每一份 DDL 文件' {
    # 建表 Job 读的是各属主自己的文件;docker 的 initdb 只能吃一份自带 USE 的脚本,所以 gateway_tables.sql 里另有一份。
    # 两边靠人手同步时,改了一边忘了另一边不会有任何报错:本机 MySQL 与 K8s 上的表结构就此不同。这里机械钉住。
    # 比较前去掉整行注释、把连续空白压成一个空格;语句本身(含列注释)必须逐字相同。
    $normalize = {
        param([string]$Text)
        $kept = ($Text -replace "`r`n", "`n") -split "`n" | Where-Object { $_.Trim() -cnotmatch '^--' }
        return ((($kept -join "`n") -replace '\s+', ' ').Trim())
    }
    $init = & $normalize (Read-RepoText 'deploy/mysql-init/gateway_tables.sql')
    $deploy = Read-RepoText 'tools/scripts/k8s_deploy.ps1'
    $sources = @([regex]::Matches($deploy, "(?m)^\s*\[pscustomobject\]@\{ Key = '(?<key>[^']+)'; Source = '(?<source>[^']+)' \}\s*$") | ForEach-Object { $_.Groups['source'].Value })
    Assert-Equal -Expected 'java/gateway_node/src/main/resources/schema.sql,go/login/model/password_auth_schema.sql' -Actual ($sources -join ',') -Because 'Get-MmorpgSchemaDdlFiles 列出的就是这两份来源(加了属主要同步改这条用例与 gateway_tables.sql)'
    foreach ($source in $sources) {
        $ddl = & $normalize (Read-RepoText $source)
        Assert-True -Condition ($ddl.Length -gt 0) -Because "$source 不是空文件"
        Assert-True -Condition ($init.Contains($ddl)) -Because "deploy/mysql-init/gateway_tables.sql 必须原样包含 $source 的全部语句(先后相同),两边一起改"
    }
}

Test-Case '-Region 注入 data-service 主容器,所有档位都写;go/db 注入 POD_IP;dev 的 Schema.AutoMigrate 也是 false' {
    foreach ($case in @(@{ Run = $devZoneRun; Region = '0' }, @{ Run = $prodZoneRun; Region = '7' })) {
        $block = Get-DeploymentBlock -Output $case.Run.Output -Name 'data-service'
        $found = [regex]::Matches($block, 'name: MMORPG_ID_ISSUER_EXPECTED_REGION\s+value: "(?<v>\d+)"')
        Assert-Equal -Expected 1 -Actual $found.Count -Because '只注入一次'
        Assert-Equal -Expected $case.Region -Actual $found[0].Groups['v'].Value -Because '值就是这次的地域码'
        Assert-True -Condition ($found[0].Index -gt $block.IndexOf("`n      containers:")) -Because '进主容器,data_service 启动时比对'
    }
    Assert-Match -Text (Read-RepoText 'go/data_service/internal/svc/servicecontext.go') -Pattern 'envExpectedIdRegion = "MMORPG_ID_ISSUER_EXPECTED_REGION"' -Because '变量名和 Go 一致'
    Assert-Match -Text (Get-DeploymentBlock -Output $devZoneRun.Output -Name 'db') -Pattern 'name: POD_IP\s+valueFrom:\s+fieldRef:\s+fieldPath: status\.podIP' -Because 'go/db 写进 NodeInfo 的对外 IP 取 POD_IP,不注入时回落本机内网地址'
    $flat = Get-ConfigFlat -Output $devZoneRun.Output -ConfigMap 'go-svc-data-service-config'
    Assert-Equal -Expected 'false' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.Schema.AutoMigrate') -Because '常驻 data_service 见到 true 直接 panic,dev 也不例外'
}

Test-Case 'MMORPG_DB_ALLOWED_DATABASES 在写路径入口核:没给、不合规都在任何清单之前拒绝,dev 也一样' {
    $missing = Invoke-LogicalDbDryRun -Arguments $ZoneArgs -Env @{ MMORPG_DB_ALLOWED_DATABASES = '' }
    Assert-True -Condition ($missing.ExitCode -ne 0) -Because "dev 没给也要拒绝,没有回落值。输出: $($missing.Output)"
    Assert-Match -Text $missing.Output -Pattern 'MMORPG_DB_ALLOWED_DATABASES is not set' -Because '点名变量'
    Assert-NotMatch -Text $missing.Output -Pattern '--- BEGIN MANIFEST ---' -Because 'gate、scene 不能已经 apply 了才失败'
    $bad = Invoke-LogicalDbDryRun -Arguments $ZoneArgs -Env @{ MMORPG_DB_ALLOWED_DATABASES = 'mmorpg_global' }
    Assert-True -Condition ($bad.ExitCode -ne 0) -Because "不是存档库的名字要拒绝。输出: $($bad.Output)"
    Assert-NotMatch -Text $bad.Output -Pattern '--- BEGIN MANIFEST ---' -Because '同样在任何清单之前'
    # 存档库是别的地域的(地域码填错):dev 是地域 0,清单里却是地域 1 的库(设计 player-data-tidb-scaling-plan.md §7.2)。
    $wrongRegion = Invoke-LogicalDbDryRun -Arguments $ZoneArgs -Env @{ MMORPG_DB_ALLOWED_DATABASES = 'player_store_1001000_db' }
    Assert-True -Condition ($wrongRegion.ExitCode -ne 0) -Because "别的地域的存档库要拒绝。输出: $($wrongRegion.Output)"
    Assert-Match -Text $wrongRegion.Output -Pattern 'must list exactly the 1 player store databases of region 0 \(player_store_1000000_db\.\.player_store_1000000_db\)' -Because '点名本地域该有哪些库'
    Assert-Match -Text $wrongRegion.Output -Pattern 'missing 1: player_store_1000000_db; unexpected 1: player_store_1001000_db' -Because '点名少了哪个、多了哪个'
    Assert-NotMatch -Text $wrongRegion.Output -Pattern '--- BEGIN MANIFEST ---' -Because '同样在任何清单之前'
    # zone 库已退役(设计 §7.2 A17c):清单里还有 zone 库说明部署没跟上,点名拒绝。
    $zoneDb = Invoke-LogicalDbDryRun -Arguments $ZoneArgs -Env @{ MMORPG_DB_ALLOWED_DATABASES = 'zone_1_db,player_store_1000000_db' }
    Assert-True -Condition ($zoneDb.ExitCode -ne 0) -Because "清单里有 zone 库要拒绝。输出: $($zoneDb.Output)"
    Assert-Match -Text $zoneDb.Output -Pattern "names 'zone_1_db': zone databases are retired" -Because '点名是哪个 zone 库'
    Assert-NotMatch -Text $zoneDb.Output -Pattern '--- BEGIN MANIFEST ---' -Because '同样在任何清单之前'
    $infra = Invoke-LogicalDbDryRun -Arguments $InfraArgs -Env ($InfraEnv + @{ MMORPG_DB_ALLOWED_DATABASES = '' })
    Assert-Equal -Expected 0 -Actual $infra.ExitCode -Because "infra-up 不部署 db、data-service,不要这份清单。输出: $($infra.Output)"
}

Test-Case '存档库清单:恰好是本地域的 K 个库,zone 库拒绝;K 与地域码 dev 缺省 1 / 0,staging、prod 必须显式' {
    # 纯函数按 AST 抽出(本节点名单在文件头)。
    Assert-Equal -Expected 'player_store_1007000_db,player_store_1007001_db,player_store_1007002_db' -Actual ((Assert-PlayerStoreAllowlist -DatabaseNames 'player_store_1007002_db,player_store_1007000_db,player_store_1007001_db' -RegionCode 7 -Shards 3) -join ',') -Because '顺序无关,返回按片号排的本地域库名'
    foreach ($case in @(
            @{ Names = 'zone_1_db,player_store_1000000_db'; Region = 0; Shards = 1; Pattern = "names 'zone_1_db': zone databases are retired"; Why = 'zone 库已退役(A17c),清单里出现就拒绝并点名' }
            @{ Names = 'player_store_1007000_db,player_store_1007001_db'; Region = 7; Shards = 3; Pattern = 'missing 1: player_store_1007002_db'; Why = '少一个片' }
            @{ Names = 'player_store_1007000_db,player_store_1007001_db,player_store_1007002_db,player_store_1007003_db'; Region = 7; Shards = 3; Pattern = 'unexpected 1: player_store_1007003_db'; Why = 'K 填小了(清单多一个片)' }
            @{ Names = 'player_store_1000000_db'; Region = 7; Shards = 1; Pattern = 'unexpected 1: player_store_1000000_db'; Why = '地域码填错' }
        )) {
        Assert-Match -Text (Get-ThrownMessage { Assert-PlayerStoreAllowlist -DatabaseNames $case.Names -RegionCode $case.Region -Shards $case.Shards }) -Pattern ([regex]::Escape($case.Pattern)) -Because $case.Why
    }
    Assert-Match -Text (Get-ThrownMessage { Assert-PlayerStoreAllowlist -DatabaseNames 'player_store_1000000_db,player_store_1000000_db' -RegionCode 0 -Shards 1 }) -Pattern 'names storage 1000000 twice' -Because '库名形状与重复仍由 Get-RoleProofStorageIds 核'
    Assert-Equal -Expected 1 -Actual (Resolve-PlayerStoreShards -Requested 0 -ReleaseProfileName 'dev') -Because 'dev 缺省 1'
    Assert-Equal -Expected 64 -Actual (Resolve-PlayerStoreShards -Requested 64 -ReleaseProfileName 'prod') -Because '显式给就用它'
    foreach ($profileName in @('staging', 'prod')) {
        Assert-Match -Text (Get-ThrownMessage { Resolve-PlayerStoreShards -Requested 0 -ReleaseProfileName $profileName }) -Pattern 'must pass -PlayerStoreShards explicitly' -Because "$profileName 没有回落值"
    }
    Assert-Match -Text (Get-ThrownMessage { Resolve-PlayerStoreShards -Requested 1001 -ReleaseProfileName 'dev' }) -Pattern 'out of range' -Because 'K 上限 1000(placement.RegionStorageStride)'
}

Test-Case 'DryRun:login 的 Placement 按地域码和 K 渲染,存档库池按 K 摊开,MySQL 初始化预建本地域的存档库' {
    # dev:地域 0、K=1,login 只能落 player_store_1000000_db;两类存档库池就是服务 yaml 的 K=1 预算。
    $devLogin = Get-ConfigFlat -Output $devZoneRun.Output -ConfigMap 'go-svc-login-config'
    Assert-Equal -Expected '0/1' -Actual ('{0}/{1}' -f $devLogin.Scalars['data.login.yaml.Placement.Region'], $devLogin.Scalars['data.login.yaml.Placement.StorageShards']) -Because 'dev 缺省地域 0、K=1'
    Assert-Equal -Expected 0 -Actual @($devLogin.Scalars.Keys | Where-Object { $_ -like 'data.login.yaml.Placement.NewPlayerStorageId*' }).Count -Because '旧键 NewPlayerStorageId 已删,login 见到会拒启'
    Assert-Match -Text $devZoneRun.Output -Pattern 'Player store databases: region=0 shards=1 databases=player_store_1000000_db\.\.player_store_1000000_db' -Because '入口打印本次存档库'
    $devDb = Get-ConfigFlat -Output $devZoneRun.Output -ConfigMap 'go-svc-db-config'
    Assert-Equal -Expected (Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'Placement.StoreMaxOpenConn') -Actual $devDb.Scalars['data.db.yaml.Placement.StoreMaxOpenConn'] -Because 'K=1 时每库就是服务 yaml 的预算'

    # prod:地域 7、K=64(夹具补的 K),login 写同一对值;每库的池按 K 摊开,与连接总账同一条规则。
    $prodLogin = Get-ConfigFlat -Output $prodZoneRun.Output -ConfigMap 'go-svc-login-config'
    Assert-Equal -Expected '7/64' -Actual ('{0}/{1}' -f $prodLogin.Scalars['data.login.yaml.Placement.Region'], $prodLogin.Scalars['data.login.yaml.Placement.StorageShards']) -Because 'Region 与 data-service 的 IdSegment.Issuer.Region 同源,K 取 -PlayerStoreShards'
    $prodDb = Get-ConfigFlat -Output $prodZoneRun.Output -ConfigMap 'go-svc-db-config'
    $prodDs = Get-ConfigFlat -Output $prodZoneRun.Output -ConfigMap 'go-svc-data-service-config'
    $dbBudget = [int](Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'Placement.StoreMaxOpenConn')
    $dsBudget = [int](Get-AuthoritativeScalar -RelativePath 'go/data_service/etc/data_service.yaml' -KeyPath 'Directory.StoragePools.MaxOpenConn')
    Assert-Equal -Expected (Get-PlayerStorePoolLimit -Base $dbBudget -Shards 64) -Actual ([int]$prodDb.Scalars['data.db.yaml.Placement.StoreMaxOpenConn']) -Because 'go/db 每个存档库的池按 K 摊开'
    Assert-True -Condition ([int]$prodDb.Scalars['data.db.yaml.Placement.StoreMaxIdleConn'] -le [int]$prodDb.Scalars['data.db.yaml.Placement.StoreMaxOpenConn']) -Because '空闲数不超过摊开后的上限'
    Assert-Equal -Expected (Get-PlayerStorePoolLimit -Base $dsBudget -Shards 64) -Actual ([int]$prodDs.Scalars['data.data_service.yaml.Directory.StoragePools.MaxOpenConn']) -Because 'data_service 每个存档库的池按 K 摊开'
    Assert-Match -Text $prodZoneRun.Output -Pattern 'Player store databases: region=7 shards=64 databases=player_store_1007000_db\.\.player_store_1007063_db' -Because '入口打印本次存档库'

    # 缺 K:staging、prod 在任何清单之前拒绝。
    $noShards = Invoke-LogicalDbDryRun -Arguments ($ProdZoneArgs + @('-DirectoryLogicalShards', '64', '-Region', '7', '-PlayerStoreShards', '0')) -Env $ProdEnv
    Assert-True -Condition ($noShards.ExitCode -ne 0) -Because "prod 没给 -PlayerStoreShards 必须拒绝。输出: $($noShards.Output)"
    Assert-Match -Text $noShards.Output -Pattern 'must pass -PlayerStoreShards explicitly' -Because '点名参数'
    Assert-NotMatch -Text $noShards.Output -Pattern '--- BEGIN MANIFEST ---' -Because '在任何清单之前'
}

Test-Case 'DryRun:-IdIssuerRegion 只换发号器地域码,存档库和 login 的 Placement 仍按 -Region(换码之后老玩家的存档库不动)' {
    $run = Invoke-LogicalDbDryRun -Arguments ($ProdZoneArgs + @('-DirectoryLogicalShards', '64', '-Region', '7', '-IdIssuerRegion', '9')) -Env $ProdEnv
    Assert-True -Condition ($run.ExitCode -eq 0) -Because "换码之后照常部署。输出: $($run.Output)"
    $ds = Get-ConfigFlat -Output $run.Output -ConfigMap 'go-svc-data-service-config'
    Assert-Equal -Expected '9' -Actual (Get-FlatValue -Flat $ds -KeyPath 'data.data_service.yaml.IdSegment.Issuer.Region') -Because '发号器用新码'
    Assert-Match -Text (Get-DeploymentBlock -Output $run.Output -Name 'data-service') -Pattern 'name: MMORPG_ID_ISSUER_EXPECTED_REGION\s+value: "9"' -Because 'data_service 启动比对的是发号器地域码'
    $login = Get-ConfigFlat -Output $run.Output -ConfigMap 'go-svc-login-config'
    Assert-Equal -Expected '7' -Actual $login.Scalars['data.login.yaml.Placement.Region'] -Because '新号仍落本地域原来的存档库'
    Assert-Match -Text $run.Output -Pattern 'Player store databases: region=7 shards=64 databases=player_store_1007000_db\.\.player_store_1007063_db' -Because '存档库清单按 -Region 核'
    Assert-Match -Text $run.Output -Pattern 'Id issuer: region=9 ' -Because '入口打印发号器地域码'
}

Test-Case 'DryRun:staging/prod 的 data-service ConfigMap 写登记处双向 TLS 的三个证书路径并核对客户端证书 Secret,dev 不写' {
    $prod = Get-ConfigFlat -Output $prodZoneRun.Output -ConfigMap 'go-svc-data-service-config'
    foreach ($pair in @(@('CAFile', 'ca.crt'), @('CertFile', 'tls.crt'), @('KeyFile', 'tls.key'))) {
        Assert-Equal -Expected "/etc/mmorpg/id-registry-tls/$($pair[1])" -Actual (Get-FlatValue -Flat $prod -KeyPath "data.data_service.yaml.IdSegment.Issuer.RegistryTLS.$($pair[0])") `
            -Because "RegistryTLS.$($pair[0]) 指向 data-service.yaml 挂载的客户端证书"
    }
    Assert-Match -Text $prodZoneRun.Output -Pattern '\[dry-run\] would check that Secret id-registry-client-tls in namespace \S+ has ca\.crt, tls\.crt and tls\.key' -Because 'DryRun 打出将要核对的预置 Secret'
    $dev = Get-ConfigFlat -Output $devZoneRun.Output -ConfigMap 'go-svc-data-service-config'
    Assert-Equal -Expected 0 -Actual @($dev.Scalars.Keys | Where-Object { $_ -like 'data.data_service.yaml.IdSegment.Issuer.RegistryTLS*' }).Count -Because 'dev 的登记处在自己的 etcd 上,没有 TLS'
    Assert-NotMatch -Text $devZoneRun.Output -Pattern 'would check that Secret id-registry-client-tls' -Because 'dev 不核客户端证书'
}

Test-Case '回收 CronJob:zone 和 infra 各一份,先于写者,只有 pods 的三个动词,非 root,只认主容器的退出码 78' {
    foreach ($case in @(@{ Run = $devZoneRun; Namespace = $ZoneNamespace; Writer = 'data-service' }, @{ Run = $devInfraRun; Namespace = $Infra; Writer = 'guild' })) {
        $blocks = @(Get-ManifestBlocks -Output $case.Run.Output | Where-Object { $_ -cmatch '(?m)^kind: CronJob\s*$' -and $_ -cmatch '(?m)^  name: sql-writer-pod-reaper\s*$' })
        Assert-Equal -Expected 1 -Actual $blocks.Count -Because "$($case.Namespace) 一份"
        $block = $blocks[0]
        Assert-NotMatch -Text $block -Pattern 'PLACEHOLDER_' -Because '占位全部替换'
        Assert-Match -Text $block -Pattern ('(?m)^    namespace: ' + [regex]::Escape($case.Namespace) + '\s*$') -Because 'RoleBinding 的 subject 要写 namespace'
        Assert-True -Condition ($case.Run.Output.IndexOf('name: sql-writer-pod-reaper') -lt $case.Run.Output.IndexOf("[applied] $($case.Writer) ->")) -Because '先于写者落地'
        Assert-Match -Text $case.Run.Output -Pattern ('\[applied\] sql-writer-pod-reaper -> registry\.k8s\.io/kubectl:v\d+\.\d+\.\d+ \(namespace ' + [regex]::Escape($case.Namespace) + '\)') -Because '缺省官方镜像,钉具体版本'
    }
    $text = Read-RepoText 'deploy/k8s/manifests/go-svc/sql-writer-pod-reaper.yaml'
    Assert-Match -Text $text -Pattern 'schedule: "\* \* \* \* \*"' -Because '每分钟'
    Assert-Match -Text $text -Pattern 'concurrencyPolicy: Forbid' -Because '不并发'
    $deadline = [regex]::Match($text, '(?m)^\s+activeDeadlineSeconds: (\d+)\s*$')
    Assert-True -Condition ($deadline.Success -and [int]$deadline.Groups[1].Value -lt 60) -Because 'activeDeadlineSeconds 小于调度周期'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^\s+- apiGroups:').Count) -Because 'Role 只有一条规则'
    Assert-Match -Text $text -Pattern 'resources: \["pods"\]\s+verbs: \["get", "list", "delete"\]' -Because '只给 pods 的 get、list、delete'
    Assert-Match -Text $text -Pattern 'runAsNonRoot: true' -Because '有的集群禁 root 容器;官方镜像默认 root,要显式钉'
    Assert-Match -Text $text -Pattern 'runAsUser: 10001' -Because '非 root 的 uid'
    Assert-Match -Text $text -Pattern '--selector=mmorpg\.io/sql-writer=true' -Because '只处理带 label 的 Pod'
    Assert-Match -Text $text -Pattern 'if eq \(printf "%v" \.lastState\.terminated\.exitCode\) "78"' -Because '判据是主容器上一次以 78 退出:这一代的写资格已消费,删掉后 StatefulSet 按同序号重建、在槽位上换代。还没写过就退出的进程能原地接着起,不删;initContainerStatuses 不看,init 反复失败时删了也是同样原因再失败'
    Assert-NotMatch -Text $text -Pattern 'initContainerStatuses:?\}|range \.status\.initContainerStatuses' -Because 'init 的重启次数不算'
    Assert-Match -Text $text -Pattern 'if \.metadata\.deletionTimestamp' -Because '已经在删的跳过'
    Assert-Match -Text $text -Pattern 'deleting pod \{\{\$pod\.metadata\.name\}\} in namespace \{\{\$pod\.metadata\.namespace\}\}' -Because '删除前的日志写明 namespace 和 Pod 名'
    # 官方镜像没有 shell(v1.35.10 的镜像配置已核):只能用 /go-runner 和 /bin/kubectl。
    Assert-NotMatch -Text (($text -split "`n" | Where-Object { $_ -notmatch '^\s*#' }) -join "`n") -Pattern '/bin/sh|\bsh -c' -Because '镜像里没有 shell'
    Assert-Match -Text $text -Pattern 'command: \["/go-runner"\]' -Because 'stdout 写文件靠 go-runner'
    Assert-Match -Text $text -Pattern 'command: \["/bin/kubectl"\]' -Because '镜像里 kubectl 的路径'
    Assert-Match -Text $text -Pattern '--wait=false' -Because '不等 Pod 退干净,否则超过 activeDeadlineSeconds'
    Assert-Match -Text $text -Pattern '--ignore-not-found=true' -Because '别人先删了不算失败'
    Assert-Match -Text $text -Pattern '--redirect-stderr=false' -Because 'kubectl 的 stderr 不能混进要删除的清单'
    foreach ($service in @('scene-manager', 'player-locator')) {
        Assert-NotMatch -Text (Get-DeploymentBlock -Output $devZoneRun.Output -Name $service) -Pattern 'mmorpg\.io/sql-writer' -Because "$service 不是写者,不归它管"
    }
    $custom = Invoke-LogicalDbDryRun -Arguments ($ZoneArgs + @('-SqlWriterReaperImage', 'registry.invalid/mirror/kubectl:v1.35.10'))
    Assert-Equal -Expected 0 -Actual $custom.ExitCode -Because "输出: $($custom.Output)"
    Assert-Match -Text $custom.Output -Pattern 'image: registry\.invalid/mirror/kubectl:v1\.35\.10' -Because '镜像是部署参数'
    $devTools = Read-RepoText 'tools/scripts/dev_tools.ps1'
    foreach ($name in @('SqlSchemaOnly', 'SqlWriterReaperImage')) {
        Assert-Match -Text $devTools -Pattern "\`$args\.$name = " -Because "dev_tools 转给 k8s_deploy:-$name"
    }
}

Test-Case '名册按槽位:最少 副本数 个槽位;目录缺名册时 -SqlSchemaOnly 能跑 Job,不带这个开关在写之前拒绝' {
    Assert-Equal -Expected 2 -Actual (Get-SqlWriterRosterMinimumEntries -Replicas 2) -Because '两副本两个槽位:同一序号每次启动在槽位上换代,重启、回收、滚动都不消耗槽位'
    $root = Join-Path $TempRoot 'schema-only'
    $null = New-Item -ItemType Directory -Path $root
    $catalog = @{}
    foreach ($service in @('db', 'data-service', 'login', 'gateway')) { $catalog["$ZoneNamespace/$service"] = @{ schemaConfigSecret = 'fixture-schema-config'; schemaPermissionSecret = 'fixture-schema-permission' } }
    # 网关没有自己的迁移镜像,mmorpg 的建表 Job 跑 registrar 镜像里的 writer_authority:gateway 条目还要 registrarImage。
    # 镜像不依赖 generation,名册之前就能定。
    $gatewayRegistrarImage = 'fixture.invalid/registrar@sha256:' + ('c' * 64)
    $catalog["$ZoneNamespace/gateway"].registrarImage = $gatewayRegistrarImage
    $catalogFile = Join-Path $root 'catalog.json'
    [IO.File]::WriteAllText($catalogFile, (ConvertTo-Json -InputObject $catalog -Depth 4 -Compress), [Text.UTF8Encoding]::new($false))
    $schemaOnly = Invoke-LogicalDbDryRun -Arguments ($ZoneArgs + @('-SqlSchemaOnly')) -Env @{ MMORPG_SQL_K8S_BOOT_CATALOG = $catalogFile }
    Assert-Equal -Expected 0 -Actual $schemaOnly.ExitCode -Because "目录只有 schema Secret(gateway 另加 registrar 镜像)也能跑 Job。输出: $($schemaOnly.Output)"
    foreach ($job in @('db-migrate', 'data-service-migrate')) {
        $block = @(Get-ManifestBlocks -Output $schemaOnly.Output | Where-Object { $_ -cmatch '(?m)^kind: Job\s*$' -and $_ -cmatch ('(?m)^  name: ' + $job + '\s*$') })
        Assert-Equal -Expected 1 -Actual $block.Count -Because "$job 要渲染"
        Assert-Match -Text $block[0] -Pattern 'secretName: fixture-schema-config' -Because "$job 读 schema 账号的配置"
        Assert-Match -Text $block[0] -Pattern 'name: MMORPG_SQL_SCHEMA_PRINCIPAL\s+valueFrom: \{secretKeyRef: \{name: fixture-schema-permission, key: principal, optional: false\}\}' -Because "$job 的主体"
    }
    $gatewayJob = @(Get-ManifestBlocks -Output $schemaOnly.Output | Where-Object { $_ -cmatch '(?m)^kind: Job\s*$' -and $_ -cmatch '(?m)^  name: gateway-schema\s*$' })
    Assert-Equal -Expected 1 -Actual $gatewayJob.Count -Because 'tidb 档 mmorpg 的建表 Job 同样在名册之前跑:login、gateway 的名册要读它种下的 route_meta.generation'
    Assert-Match -Text $gatewayJob[0] -Pattern ('(?m)^\s+image: ' + [regex]::Escape($gatewayRegistrarImage) + '\s*$') -Because '镜像取 gateway 条目的 registrarImage'
    Assert-Match -Text $gatewayJob[0] -Pattern 'name: MMORPG_SQL_SCHEMA_PRINCIPAL\s+valueFrom: \{secretKeyRef: \{name: fixture-schema-permission, key: principal, optional: false\}\}' -Because 'gateway-schema 的主体'
    Assert-True -Condition ($schemaOnly.Output.IndexOf('[applied] gateway-schema ->') -gt $schemaOnly.Output.IndexOf('name: data-service-migrate')) -Because 'Go 服务的 Job 先跑,网关的建表 Job 随后'
    Assert-NotMatch -Text $schemaOnly.Output -Pattern '(?m)^kind: (?:Deployment|StatefulSet)\s*$' -Because 'gate、scene、写者都不起'

    # gateway 条目缺 registrar 镜像:先渲染网关的建表 Job,一个 Job 都还没跑就拒绝。
    $catalog["$ZoneNamespace/gateway"].Remove('registrarImage')
    $noImageFile = Join-Path $root 'catalog-no-gateway-image.json'
    [IO.File]::WriteAllText($noImageFile, (ConvertTo-Json -InputObject $catalog -Depth 4 -Compress), [Text.UTF8Encoding]::new($false))
    $noImage = Invoke-LogicalDbDryRun -Arguments ($ZoneArgs + @('-SqlSchemaOnly')) -Env @{ MMORPG_SQL_K8S_BOOT_CATALOG = $noImageFile }
    Assert-True -Condition ($noImage.ExitCode -ne 0) -Because "没有镜像跑不了 writer_authority。输出: $($noImage.Output)"
    Assert-Match -Text $noImage.Output -Pattern 'needs a registrarImage pinned by digest' -Because '点名缺什么'
    Assert-NotMatch -Text $noImage.Output -Pattern '--- BEGIN MANIFEST ---' -Because 'db-migrate、data-service-migrate 也还没 apply'
    $dbJob = @(Get-ManifestBlocks -Output $schemaOnly.Output | Where-Object { $_ -cmatch '(?m)^  name: db-migrate\s*$' })[0]
    Assert-Match -Text $dbJob -Pattern 'image: registry\.invalid/test/mmorpg-db-migrate:' -Because 'cmd/migrate 不在服务二进制里,单独的镜像'
    Assert-Match -Text $dbJob -Pattern ('name: DB_ALLOWED_DATABASES\s+value: "' + [regex]::Escape($DevAllowedDatabases) + '"') -Because '逐库迁全部存档库(本地域的存档库):data_service 的写资格覆盖它们'
    Assert-NotMatch -Text $dbJob -Pattern 'PLACEHOLDER_|__INFRA_NAMESPACE__' -Because '占位全部替换'

    $refused = Invoke-LogicalDbDryRun -Arguments $ZoneArgs -Env @{ MMORPG_SQL_K8S_BOOT_CATALOG = $catalogFile }
    Assert-True -Condition ($refused.ExitCode -ne 0) -Because "缺名册不能起进程。输出: $($refused.Output)"
    Assert-NotMatch -Text $refused.Output -Pattern '--- BEGIN MANIFEST ---' -Because 'gate、scene 不能已经 apply 了才失败'

    $wrongCommand = Invoke-LogicalDbDryRun -Arguments ($InfraArgs + @('-SqlSchemaOnly')) -Env $InfraEnv
    Assert-True -Condition ($wrongCommand.ExitCode -ne 0) -Because 'infra-up 不认这个开关'
    Assert-Match -Text $wrongCommand.Output -Pattern '-SqlSchemaOnly only works with zone-up and all-up' -Because '点名'

    # 名册比下限少:Apply-Zone 的第一步拒绝。
    $small = New-SqlWriterCatalogFixture -Root (Join-Path $root 'small') -Namespaces @($ZoneNamespace, $Infra)
    $smallCatalog = Get-Content -LiteralPath $small -Raw | ConvertFrom-Json -AsHashtable
    $smallRoster = Join-Path $root 'small-roster.json'
    [IO.File]::WriteAllText($smallRoster, '{"format":"SQL_WRITER_BOOT_ROSTER_V2","entries":[{}]}', [Text.UTF8Encoding]::new($false))
    foreach ($key in @($smallCatalog.Keys)) {
        $smallCatalog[$key].rosterFile = $smallRoster
        $smallCatalog[$key].rosterSha256 = (Get-FileHash -LiteralPath $smallRoster -Algorithm SHA256).Hash.ToLowerInvariant()
    }
    [IO.File]::WriteAllText($small, (ConvertTo-Json -InputObject $smallCatalog -Depth 6 -Compress), [Text.UTF8Encoding]::new($false))
    $tooSmall = Invoke-LogicalDbDryRun -Arguments $ZoneArgs -Env @{ MMORPG_SQL_K8S_BOOT_CATALOG = $small }
    Assert-True -Condition ($tooSmall.ExitCode -ne 0) -Because '槽位比副本少:多出来的序号选不到槽位'
    Assert-Match -Text $tooSmall.Output -Pattern '2 replicas need at least 2 slots' -Because '点名要多少个槽位'
    Assert-NotMatch -Text $tooSmall.Output -Pattern '--- BEGIN MANIFEST ---' -Because '在写之前拒绝'
}

Test-Case 'db-migrate 接进部署:目录登记,db 排第一,data-service 排第二,镜像目录有入口,退出码语义对得上 cmd/migrate' {
    $deploy = Read-RepoText 'tools/scripts/k8s_deploy.ps1'
    Assert-Match -Text $deploy -Pattern 'db\s+=\s+@\{[^\r\n]*MigrateJob = "db-migrate\.yaml"; MigrateImageName = "mmorpg-db-migrate"' -Because '目录登记 Job 和镜像'
    Assert-Match -Text (Read-RepoText 'tools/scripts/go_svc_image.ps1') -Pattern '"db-migrate"\s+=\s+@\{ Dir = "db";\s+Entry = "\./cmd/migrate";\s+ImageName = "mmorpg-db-migrate" \}' -Because '镜像入口'
    $job = Read-RepoText 'deploy/k8s/manifests/go-svc/db-migrate.yaml'
    Assert-Match -Text $job -Pattern 'values: \[1\]' -Because '1 是失败,FailJob;3 锁忙重试;4 在脚本里告警'
    Assert-Match -Text $job -Pattern '-command up -storage-id "\$storage_id"' -Because '按编号迁,和 placement.StoreDBName 同一套名字'
    Assert-Match -Text $job -Pattern '4\) echo "WARNING:' -Because '4 是迁移成功、有要人工过目的漂移,告警不中断'
    Assert-Match -Text $job -Pattern 'exit "\$code"' -Because '其余退出码原样退出,podFailurePolicy 按它判'
    Assert-NotMatch -Text (($job -split "`n" | Where-Object { $_ -notmatch '^\s*#' }) -join "`n") -Pattern '-create-database' -Because '库预先建好,不在这里建库'
    $migrateMain = Read-RepoText 'go/db/cmd/migrate/main.go'
    Assert-Match -Text $migrateMain -Pattern 'os\.Exit\(3\)' -Because '锁忙是 3'
    Assert-Match -Text $migrateMain -Pattern 'os\.Exit\(4\)' -Because '漂移是 4'
    Assert-Match -Text $migrateMain -Pattern 'EnsureCatalogSchema' -Because '闸门表由它建,这是 Job 排在 registrar 之前的理由'
    Assert-Match -Text $migrateMain -Pattern 'flag\.Uint64\("storage-id"' -Because '-storage-id 是 cmd/migrate 的参数'
    $node = $deployAst.Find({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Get-ZoneScopedGoSvcNames' }, $false)
    $GoSvcCatalogue = @{ login = @{}; 'data-service' = @{}; db = @{}; trade = @{ Global = $true }; 'scene-manager' = @{} }
    $names = @(& $node.Body.GetScriptBlock())
    Assert-Equal -Expected 'db,data-service,login,scene-manager' -Actual ($names -join ',') -Because 'db-migrate 先于 db、data-service 的 Deployment:data-service 的 registrar 要存档库上的闸门表'
}

Test-Case '手册:开新地域的手册写清地域码的约束,docs/README 登记,开服手册列预置清单' {
    $runbook = Read-RepoText 'docs/ops/open-new-region-runbook.md'
    foreach ($pattern in @('地域码一旦发号不能改', '不能和别的地域重复', '不进生产', '-Region', '-id-issuer-init', 'IdSegment\.Issuer\.Region')) {
        Assert-Match -Text $runbook -Pattern $pattern -Because "手册要写 $pattern"
    }
    Assert-Match -Text (Read-RepoText 'docs/README.md') -Pattern 'ops/open-new-region-runbook\.md' -Because '文档总索引登记'
    $openServer = Read-RepoText 'docs/ops/k8s-open-server-runbook.md'
    foreach ($key in @('role-fence-coordinator', 'role-fence-guild', 'role-fence-trade', 'role-fence-mail', 'role-fence-match', 'role-fence-player-inbox', 'role-fence-friend', 'role-fence-chat', 'role-fence-db', 'match-internal-guild', 'mail-producer-battle')) {
        Assert-True -Condition ($openServer.Contains($key)) -Because "预置清单要有 $key"
    }
    Assert-Match -Text $openServer -Pattern 'DirectoryLogicalShards = 64' -Because '部署参数示例'
    Assert-Match -Text $openServer -Pattern 'Region = ' -Because '部署参数示例'
    foreach ($pattern in @('-id-issuer-init', 'tidb_enable_check_constraint', 'render-schema-role', '-SqlSchemaOnly', 'db-migrate', 'sql-writer-pod-reaper', '槽位数 ≥ 副本数', 'SqlWriterRegistrarFailing', 'schemaPermissionSecret', 'MMORPG_ID_ISSUER_EXPECTED_REGION')) {
        Assert-Match -Text $openServer -Pattern $pattern -Because "开服手册要写 $pattern"
    }
    # 顺序:TiDB 开关在建库之前,发号器初始化在 data-service 就绪之后、放流量之前。
    Assert-True -Condition ($runbook.IndexOf('tidb_enable_check_constraint') -lt $runbook.IndexOf('建片库')) -Because '开新地域:开关在建库之前'
    Assert-Match -Text $runbook -Pattern '在第一个 zone 的 data-service 就绪之后、放流量之前' -Because '开新地域:发号器初始化的时机'
    Assert-Match -Text $openServer -Pattern '在第一个 zone 的 data-service 就绪之后、放流量之前' -Because '开服:发号器初始化的时机'
    foreach ($doc in @($runbook, $openServer, (Read-RepoText 'docs/ops/id-issuer-runbook.md'))) {
        Assert-Match -Text $doc -Pattern '/app/etc/private/data_service\.yaml -id-issuer-init' -Because '写者的配置在私有子目录,手册里的命令要跟着走'
        Assert-NotMatch -Text $doc -Pattern '/app/etc/data_service\.yaml -id-issuer-init' -Because '旧路径已经不存在'
    }
    Assert-Match -Text $runbook -Pattern 'exec statefulset/data-service -- grep -n ''Region:'' /app/etc/private/data_service\.yaml' -Because '核 Pod 里生效的配置,不核 ConfigMap 模板;data-service 是 StatefulSet(写者名册的槽位)'
    Assert-NotMatch -Text $runbook -Pattern 'get configmap go-svc-data-service-config' -Because 'ConfigMap 只是模板,核它看不出 Secret 抄错'
    Assert-NotMatch -Text (Read-RepoText 'deploy/k8s/README.md') -Pattern '靠 K8s 重启收敛|K8s 下一次重启就恢复' -Because '主容器重启过的写者 Pod 由回收 CronJob 换新,不是原地恢复'
}

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

exit (Complete-TestRun -SuiteName 'logical database deployment contract')
