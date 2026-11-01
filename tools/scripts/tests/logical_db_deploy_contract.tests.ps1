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
$AllowedDatabases = 'zone_1_db,zone_2_db,zone_101_db,zone_102_db'
$AuthorityHost = 'tidb.invalid:4000'

$parseErrors = $null
$deployAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "部署脚本语法错误: $($parseErrors[0].Message)" }
$deployFunctions = @(
    'Resolve-DirectoryLogicalShards', 'Resolve-IdIssuerRegion', 'Get-DataServiceWriterSchemas', 'Get-DataServiceRosterProblems', 'Get-SqlWriterRosterMinimumEntries',
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
    $found = @(Get-ManifestBlocks -Output $Output | Where-Object { $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch ('(?m)^  name: ' + [regex]::Escape($Name) + '\s*$') })
    if ($found.Count -ne 1) { throw "DryRun 输出里 Deployment $Name 应当恰好 1 份,实际 $($found.Count) 份" }
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
$InfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-SkipJavaSvc', '-InfraNamespace', $Infra,
    '-GlobalAuthorityTiDBHost', $AuthorityHost, '-GoSvcRegistry', 'registry.invalid/test', '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab')
$InfraEnv = @{ MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345' }
# 同 k8s_deploy_contract.tests.ps1 的 $ProdEnv:走到逻辑库那一步之前要的密钥。玩家域集群的参数和口令由 deploy_capture 的夹具补。
$ProdEnv = @{
    MMORPG_GATE_TOKEN_SECRET        = 'contract-test-secret-0123456789abcdef0123'
    MMORPG_MYSQL_USER               = 'mmorpg_app'
    MMORPG_MYSQL_PASSWORD           = 'contract-test-mysql-pw-01'
    MMORPG_REDIS_PASSWORD           = 'contract-test-redis-pw-01'
    MMORPG_GATEWAY_DB_USER          = 'gateway_app'
    MMORPG_GATEWAY_DB_PASSWORD      = 'contract-test-gwdb-pw-01'
    MMORPG_INTERNAL_AUTH_SECRET     = 'contract-test-internal-auth-0123456789abcdef'
    MMORPG_GATEWAY_ADMIN_API_KEY    = 'contract-test-gw-admin-key-0123456789abcdef'
    MMORPG_LOGIN_QUEUE_TOKEN_SECRET = 'contract-test-queue-token-0123456789abcdef'
    MMORPG_SPECTATE_TOKEN_SECRET    = 'contract-test-spectate-token-0123456789abcdef'
    MMORPG_BATTLE_TOKEN_SECRET      = 'contract-battle-ticket-0123456789abcdef012345'
    MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED = '1'
}
$ProdZoneArgs = $ZoneArgs + @('-ReleaseProfile', 'prod', '-SkipPreflight', '-NodeImage', 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab')
$StagingZoneArgs = $ZoneArgs + @('-ReleaseProfile', 'staging', '-SkipPreflight', '-NodeImage', 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab')

$devZoneRun = Invoke-LogicalDbDryRun -Arguments $ZoneArgs
$prodZoneRun = Invoke-LogicalDbDryRun -Arguments ($ProdZoneArgs + @('-DirectoryLogicalShards', '64', '-Region', '7')) -Env $ProdEnv
$devInfraRun = Invoke-LogicalDbDryRun -Arguments $InfraArgs -Env $InfraEnv
$shardedInfraRun = Invoke-LogicalDbDryRun -Arguments ($InfraArgs + @('-DirectoryLogicalShards', '64')) -Env $InfraEnv

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
    $expected = Get-DataServiceWriterSchemas -Base 'g' -LogicalShards 2 -PlayerDatabases @('zone_1_db', 'zone_2_db')
    Assert-Equal -Expected 'g_00,g_01,g_ledger,zone_1_db,zone_2_db' -Actual (@($expected) -join ',') -Because '顺序和 config.WriterSchemas 一样'
    $single = Get-DataServiceWriterSchemas -Base 'g' -LogicalShards 1 -PlayerDatabases @('zone_1_db')
    Assert-Equal -Expected 'g,g_ledger,zone_1_db' -Actual (@($single) -join ',') -Because 'K=1 基名就是目录库,不会和流水库连成一个字符串'

    function New-Roster([string[]]$Schemas) {
        return [ordered]@{ entries = @([ordered]@{ requests = @($Schemas | ForEach-Object { [ordered]@{ authoritySchema = $_ } }) }) }
    }
    Assert-Equal -Expected 0 -Actual @(Get-DataServiceRosterProblems -Roster (New-Roster -Schemas $expected) -ExpectedSchemas $expected).Count -Because '恰好覆盖就过'
    $missing = @(Get-DataServiceRosterProblems -Roster (New-Roster -Schemas @('g_00', 'g_ledger', 'zone_1_db', 'zone_2_db')) -ExpectedSchemas $expected)
    Assert-Match -Text ($missing -join ';') -Pattern 'entry 0 is missing: g_01' -Because '少一片'
    $extra = @(Get-DataServiceRosterProblems -Roster (New-Roster -Schemas (@($expected) + 'zone_3_db')) -ExpectedSchemas $expected)
    Assert-Match -Text ($extra -join ';') -Pattern 'entry 0 has extra: zone_3_db' -Because '多一个库'
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
        $roster = [ordered]@{ format = 'SQL_WRITER_BOOT_ROSTER_V1'; entries = @(1..12 | ForEach-Object { [ordered]@{ requests = $requests } }) }
        [IO.File]::WriteAllText($rosterPath, (ConvertTo-Json -InputObject $roster -Depth 6 -Compress), [Text.UTF8Encoding]::new($false))
        $catalog["$ZoneNamespace/data-service"].rosterFile = $rosterPath
        $catalog["$ZoneNamespace/data-service"].rosterSha256 = (Get-FileHash -LiteralPath $rosterPath -Algorithm SHA256).Hash.ToLowerInvariant()
        [IO.File]::WriteAllText($catalogFile, (ConvertTo-Json -InputObject $catalog -Depth 8 -Compress), [Text.UTF8Encoding]::new($false))
    }
    # 少了流水库:G1 之前的旧名册就是这样。
    & $writeRoster @('mmorpg_global', 'zone_1_db', 'zone_2_db', 'zone_101_db', 'zone_102_db')
    $bad = Invoke-LogicalDbDryRun -Arguments $ZoneArgs -Env @{ MMORPG_SQL_K8S_BOOT_CATALOG = $catalogFile }
    Assert-True -Condition ($bad.ExitCode -ne 0) -Because "名册缺流水库必须拒绝。输出: $($bad.Output)"
    Assert-Match -Text $bad.Output -Pattern 'boot roster.*does not match' -Because '点名名册'
    Assert-Match -Text $bad.Output -Pattern 'mmorpg_global_ledger' -Because '点名缺哪个库'
    & $writeRoster @('mmorpg_global', 'mmorpg_global_ledger', 'zone_1_db', 'zone_2_db', 'zone_101_db', 'zone_102_db')
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
    Assert-Match -Text $devZoneRun.Output -Pattern 'MySQL connection ledger by logical db: logical_shards=1 limit=mysql_manifest' -Because 'K=1 按 mysql.yaml 从严'
}

Test-Case 'prod zone-up K=64:片数、每片池、地域码进 ConfigMap,上限换 TiDB 预算,存档库池不收窄' {
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
    Assert-Equal -Expected (Get-EtcValue -RelativePath $source -KeyPath 'Directory.StoragePools.MaxOpenConn') -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.Directory.StoragePools.MaxOpenConn') -Because '存档库的池按存档库算,和 K 无关'
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

Test-Case 'mysql-init:K=1 建基名和流水库,K=64 建全部片库和流水库,基名不再是库' {
    Assert-Equal -Expected 0 -Actual $devInfraRun.ExitCode -Because "输出: $($devInfraRun.Output)"
    Assert-Equal -Expected 0 -Actual $shardedInfraRun.ExitCode -Because "输出: $($shardedInfraRun.Output)"
    $single = Select-ManifestByName -Output $devInfraRun.Output -Name 'mysql-init-sql'
    Assert-Match -Text $single -Pattern ([regex]::Escape('CREATE DATABASE IF NOT EXISTS `mmorpg_global`;')) -Because 'K=1 目录库是基名'
    Assert-Match -Text $single -Pattern ([regex]::Escape('CREATE DATABASE IF NOT EXISTS `mmorpg_global_ledger`;')) -Because '流水库单独一个'
    Assert-NotMatch -Text $single -Pattern 'mmorpg_globalmmorpg_global' -Because '库名不能连成一个字符串'
    $sharded = Select-ManifestByName -Output $shardedInfraRun.Output -Name 'mysql-init-sql'
    foreach ($database in @('mmorpg_global_00', 'mmorpg_global_31', 'mmorpg_global_63', 'mmorpg_global_ledger')) {
        Assert-Match -Text $sharded -Pattern ([regex]::Escape("CREATE DATABASE IF NOT EXISTS ``$database``;")) -Because "要建 $database,-migrate 不建库"
        Assert-Match -Text $sharded -Pattern ([regex]::Escape("GRANT ALL PRIVILEGES ON ``$database``.*")) -Because "$database 和 zone 库同一口径授权"
    }
    Assert-NotMatch -Text $sharded -Pattern ([regex]::Escape('CREATE DATABASE IF NOT EXISTS `mmorpg_global`;')) -Because 'K>1 基名不是库'
    Assert-Equal -Expected 65 -Actual ([regex]::Matches($sharded, 'CREATE DATABASE IF NOT EXISTS `mmorpg_global_').Count) -Because '64 片加流水库'
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

Test-Case 'MMORPG_DB_PLACEMENT 进写者的 registrar init,只进 init;login 和 gateway 写区域 MySQL 上的 mmorpg,不进' {
    foreach ($service in @('data-service', 'db')) {
        $block = Get-DeploymentBlock -Output $devZoneRun.Output -Name $service
        $at = $block.IndexOf('- name: MMORPG_DB_PLACEMENT')
        Assert-True -Condition ($at -ge 0) -Because "$service 的 writer_authority 要按映射拨号"
        Assert-True -Condition ($at -lt $block.IndexOf('- name: MMORPG_SQL_REGISTRAR_DSN')) -Because "$service:紧贴 registrar 的控制 DSN"
        Assert-True -Condition ($at -lt $block.IndexOf("`n      containers:")) -Because "$service:常驻容器读 yaml 的 DbPlacement,不要这个变量"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, 'name: MMORPG_DB_PLACEMENT').Count) -Because "$service:只注入一次"
        $value = [regex]::Match($block, "name: MMORPG_DB_PLACEMENT\s+value: '(?<json>[^'\r\n]+)'").Groups['json'].Value
        $json = $value | ConvertFrom-Json
        Assert-Equal -Expected 'main' -Actual $json.DefaultCluster -Because "$service:值是 ConfigFromEnv 认的形状"
        Assert-Equal -Expected $AuthorityHost -Actual $json.SqlClusters.main.Hosts -Because "$service:默认集群是权威入口"
        Assert-NotMatch -Text $value -Pattern '@tcp|password' -Because "$service:只有地址,没有凭据"
    }
    foreach ($service in @('login', 'gateway')) {
        $block = Get-DeploymentBlock -Output $devZoneRun.Output -Name $service
        Assert-True -Condition ($block.Contains('MMORPG_SQL_REGISTRAR_DSN')) -Because "$service 同样有 registrar(本断言的前提)"
        Assert-True -Condition (-not $block.Contains('MMORPG_DB_PLACEMENT')) -Because "$service 的库不在 SqlClusters 里,按连接串的地址连(手册 §9)"
    }
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

Test-Case 'guild、match 的 gRPC 准入放行 data-service,否则 -NetworkPolicies on 时协调者连不上' {
    $node = $deployAst.Find({ param($a) $a -is [Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -eq '$script:GrpcAllowLists' }, $false)
    Assert-True -Condition ($null -ne $node) -Because '要有放行名单'
    $lists = & ([scriptblock]::Create($node.Right.Extent.Text))
    foreach ($name in @('guild', 'match')) {
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
        Assert-Match -Text (Read-RepoText "deploy/k8s/manifests/go-svc/$service.yaml") -Pattern 'name: MMORPG_MATCH_INTERNAL_SECRET_GUILD\s+valueFrom:\s+secretKeyRef: \{name: domain-service-secrets, key: match-internal-guild, optional: false\}' -Because "$service:开局、占用、读终局都验这一把"
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
Test-Case 'K=64 规划档峰值不超 TiDB 预算,K=1 规划档不超 mysql.yaml,按逻辑库拆开的账对得上' {
    $plan = Get-MysqlConnectionPlan
    $inputs = Get-MysqlConnectionLedgerInputs
    $shape = @{ ZoneCount = $plan.ZoneCount; ExtraZoneStores = $plan.ExtraZoneStores; NonZoneStores = $plan.NonZoneStores; RolloutExtraPodsPerDeployment = $plan.RolloutExtraPodsPerDeployment; OpsReserve = $plan.OpsReserve }
    $single = Get-MysqlConnectionLedger -Inputs $inputs @shape
    Assert-True -Condition ($single.Peak -le $single.MaxConnections) -Because "K=1 规划档峰值 $($single.Peak) 超过 mysql.yaml 的 $($single.MaxConnections)"
    Assert-Equal -Expected 1 -Actual $single.LogicalShards -Because '缺省 K=1'

    $sharded = Get-MysqlConnectionLedger -Inputs $inputs @shape -LogicalShards 64
    Assert-Equal -Expected 6 -Actual $sharded.DirectoryPerShard -Because 'K=64 时三个目录池每片各 2 条'
    $stores = $plan.ExtraZoneStores + 1 + $plan.NonZoneStores
    $expectedPerInstance = [int]$inputs.DsAuditStorePools * [int]$inputs.DsAuditMaxOpen + 64 * 6 + $stores * [int]$inputs.DsStoragePoolMaxOpen
    Assert-Equal -Expected $expectedPerInstance -Actual $sharded.DsPerInstance -Because 'ds 每实例 = 流水库池 + K × 每片 + 存档库数 × 存档库池'
    Assert-True -Condition ($sharded.Peak -le $plan.TidbConnectionBudget) -Because "K=64 规划档峰值 $($sharded.Peak) 超过 TiDB 预算 $($plan.TidbConnectionBudget),要收窄每片上限或加 tidb-server"
    Assert-Equal -Expected ([int][Math]::Floor(($plan.TidbServers - 1) * $plan.TidbMaxConnectionsPerServer / $plan.TidbImbalanceFactor)) -Actual $plan.TidbConnectionBudget -Because '预算 = (N−1) × 每台上限 / 不均衡系数'

    foreach ($database in @('directory_shard', 'ledger', 'player_store', 'mmorpg', 'mmorpg_friend', 'mmorpg_trade', 'mmorpg_player_inbox', 'mmorpg_guild', 'mmorpg_mail', 'mmorpg_rank', 'mmorpg_chat', 'mmorpg_match')) {
        Assert-True -Condition ($sharded.PerLogicalDb.Contains($database) -and [int]$sharded.PerLogicalDb[$database] -gt 0) -Because "按逻辑库的账要有 $database"
    }
    $dsInstances = $plan.ZoneCount * [int]$inputs.DsReplicas + $plan.RolloutExtraPodsPerDeployment
    Assert-Equal -Expected ($dsInstances * 6) -Actual $sharded.PerLogicalDb['directory_shard'] -Because '每片的峰值 = data_service 实例数 × 每片连接'
    Assert-Match -Text (Get-ThrownMessage { Get-MysqlConnectionLedger -Inputs $inputs @shape -LogicalShards 5 }) -Pattern 'divisor of 64' -Because '片数非法拒绝'
}

# ──────────────────────────────────────────────
# 6. 告警
# ──────────────────────────────────────────────
Test-Case '告警:规则名单和形状,指标名与取值从 Go 源码核对,闸门冻结时长有规则' {
    $alerts = Read-RepoText 'deploy/k8s/logical-db-alerts.yaml'
    $names = @([regex]::Matches($alerts, '(?m)^\s+- alert: (\S+)') | ForEach-Object { $_.Groups[1].Value })
    Assert-Equal -Expected 'DbPlacementRejected,DbPlacementSyncFailing,DbPlacementVersionLagging,RoleFencePrepareStale,RoleFencePrepareUnreachable,RoleFencePushFailing,SqlWriterRegistrarFailing,WriterGateFrozenTooLong' -Actual (($names | Sort-Object) -join ',') -Because '规则名单'
    Assert-Match -Text $alerts -Pattern 'kube_pod_init_container_status_restarts_total\{container="sql-writer-registrar"\}' -Because '回收 CronJob 不删 init 反复失败的 Pod,要靠这条发现,名册用完也在这里'
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
    $names = @('DirectoryLogicalShards', 'Region', 'SqlClusters', 'SqlDefaultCluster')
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

Test-Case 'registrar 带 MMORPG_SQL_BACKEND:login、gateway 固定 mysql,其余跟 -DatabaseBackend,只进 init' {
    Assert-Match -Text (Read-RepoText 'go/shared/authoritysql/backend.go') -Pattern 'MMORPG_SQL_BACKEND' -Because '变量名和 Go 一致'
    function Get-RegistrarBackend([string]$Block) {
        $found = [regex]::Matches($Block, 'name: MMORPG_SQL_BACKEND\s+value: "(?<v>[a-z]+)"')
        Assert-Equal -Expected 1 -Actual $found.Count -Because '只注入一次'
        Assert-True -Condition ($found[0].Index -lt $Block.IndexOf("`n      containers:")) -Because '只进 init,常驻进程的档位在自己的配置里'
        return $found[0].Groups['v'].Value
    }
    foreach ($service in @('data-service', 'db')) {
        Assert-Equal -Expected 'tidb' -Actual (Get-RegistrarBackend (Get-DeploymentBlock -Output $devZoneRun.Output -Name $service)) -Because "$service 跟 -DatabaseBackend,缺省 tidb"
    }
    foreach ($service in @('login', 'gateway')) {
        Assert-Equal -Expected 'mysql' -Actual (Get-RegistrarBackend (Get-DeploymentBlock -Output $devZoneRun.Output -Name $service)) -Because "$service 写区域 MySQL 上的 mmorpg"
    }
    Assert-Equal -Expected 'tidb' -Actual (Get-RegistrarBackend (Get-DeploymentBlock -Output $devInfraRun.Output -Name 'guild')) -Because '全局服务同样跟 -DatabaseBackend'
    $mysqlRun = Invoke-LogicalDbDryRun -Arguments ($ZoneArgs + @('-DatabaseBackend', 'mysql'))
    Assert-Equal -Expected 0 -Actual $mysqlRun.ExitCode -Because "输出: $($mysqlRun.Output)"
    foreach ($service in @('data-service', 'db', 'login', 'gateway')) {
        Assert-Equal -Expected 'mysql' -Actual (Get-RegistrarBackend (Get-DeploymentBlock -Output $mysqlRun.Output -Name $service)) -Because "mysql 档 $service 也是 mysql"
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
    $infra = Invoke-LogicalDbDryRun -Arguments $InfraArgs -Env ($InfraEnv + @{ MMORPG_DB_ALLOWED_DATABASES = '' })
    Assert-Equal -Expected 0 -Actual $infra.ExitCode -Because "infra-up 不部署 db、data-service,不要这份清单。输出: $($infra.Output)"
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
    Assert-Match -Text $text -Pattern 'if eq \(printf "%v" \.lastState\.terminated\.exitCode\) "78"' -Because '判据是主容器上一次以 78 退出:写资格已消费。还没写过就退出的进程能原地接着起,不删、不烧名册;initContainerStatuses 不看,init 反复失败时删了白烧名册'
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

Test-Case '名册要留余量:最少 副本数 × 6;目录缺名册时 -SqlSchemaOnly 能跑 Job,不带这个开关在写之前拒绝' {
    Assert-Equal -Expected 12 -Actual (Get-SqlWriterRosterMinimumEntries -Replicas 2) -Because '两副本:当前 2 + 一次滚动 2 + 每副本 4 条余量'
    $root = Join-Path $TempRoot 'schema-only'
    $null = New-Item -ItemType Directory -Path $root
    $catalog = @{}
    foreach ($service in @('db', 'data-service', 'login', 'gateway')) { $catalog["$ZoneNamespace/$service"] = @{ schemaConfigSecret = 'fixture-schema-config'; schemaPermissionSecret = 'fixture-schema-permission' } }
    $catalogFile = Join-Path $root 'catalog.json'
    [IO.File]::WriteAllText($catalogFile, (ConvertTo-Json -InputObject $catalog -Depth 4 -Compress), [Text.UTF8Encoding]::new($false))
    $schemaOnly = Invoke-LogicalDbDryRun -Arguments ($ZoneArgs + @('-SqlSchemaOnly')) -Env @{ MMORPG_SQL_K8S_BOOT_CATALOG = $catalogFile }
    Assert-Equal -Expected 0 -Actual $schemaOnly.ExitCode -Because "目录只有 schema Secret 也能跑 Job。输出: $($schemaOnly.Output)"
    foreach ($job in @('db-migrate', 'data-service-migrate')) {
        $block = @(Get-ManifestBlocks -Output $schemaOnly.Output | Where-Object { $_ -cmatch '(?m)^kind: Job\s*$' -and $_ -cmatch ('(?m)^  name: ' + $job + '\s*$') })
        Assert-Equal -Expected 1 -Actual $block.Count -Because "$job 要渲染"
        Assert-Match -Text $block[0] -Pattern 'secretName: fixture-schema-config' -Because "$job 读 schema 账号的配置"
        Assert-Match -Text $block[0] -Pattern 'name: MMORPG_SQL_SCHEMA_PRINCIPAL\s+valueFrom: \{secretKeyRef: \{name: fixture-schema-permission, key: principal, optional: false\}\}' -Because "$job 的主体"
    }
    Assert-NotMatch -Text $schemaOnly.Output -Pattern '(?m)^kind: Deployment\s*$' -Because 'gate、scene、写者都不起'
    $dbJob = @(Get-ManifestBlocks -Output $schemaOnly.Output | Where-Object { $_ -cmatch '(?m)^  name: db-migrate\s*$' })[0]
    Assert-Match -Text $dbJob -Pattern 'image: registry\.invalid/test/mmorpg-db-migrate:' -Because 'cmd/migrate 不在服务二进制里,单独的镜像'
    Assert-Match -Text $dbJob -Pattern ('name: DB_ALLOWED_DATABASES\s+value: "' + [regex]::Escape($AllowedDatabases) + '"') -Because '逐库迁全部存档库:data_service 的写资格覆盖它们'
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
    [IO.File]::WriteAllText($smallRoster, '{"format":"SQL_WRITER_BOOT_ROSTER_V1","entries":[{},{}]}', [Text.UTF8Encoding]::new($false))
    foreach ($key in @($smallCatalog.Keys)) {
        $smallCatalog[$key].rosterFile = $smallRoster
        $smallCatalog[$key].rosterSha256 = (Get-FileHash -LiteralPath $smallRoster -Algorithm SHA256).Hash.ToLowerInvariant()
    }
    [IO.File]::WriteAllText($small, (ConvertTo-Json -InputObject $smallCatalog -Depth 6 -Compress), [Text.UTF8Encoding]::new($false))
    $tooSmall = Invoke-LogicalDbDryRun -Arguments $ZoneArgs -Env @{ MMORPG_SQL_K8S_BOOT_CATALOG = $small }
    Assert-True -Condition ($tooSmall.ExitCode -ne 0) -Because '条数只够副本数不够:每次 Pod 启动都永久烧一条'
    Assert-Match -Text $tooSmall.Output -Pattern '2 replicas need at least 12 entries' -Because '点名要多少条'
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
    foreach ($key in @('role-fence-coordinator', 'role-fence-guild', 'role-fence-trade', 'role-fence-mail', 'role-fence-match', 'role-fence-player-inbox', 'role-fence-friend', 'role-fence-chat', 'role-fence-db', 'match-internal-guild')) {
        Assert-True -Condition ($openServer.Contains($key)) -Because "预置清单要有 $key"
    }
    Assert-Match -Text $openServer -Pattern 'DirectoryLogicalShards = 64' -Because '部署参数示例'
    Assert-Match -Text $openServer -Pattern 'Region = ' -Because '部署参数示例'
    foreach ($pattern in @('-id-issuer-init', 'tidb_enable_check_constraint', 'render-schema-role', '-SqlSchemaOnly', 'db-migrate', 'sql-writer-pod-reaper', '副本数 × 6', 'SqlWriterRegistrarFailing', 'schemaPermissionSecret', 'MMORPG_ID_ISSUER_EXPECTED_REGION')) {
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
    Assert-Match -Text $runbook -Pattern 'exec deploy/data-service -- grep -n ''Region:'' /app/etc/private/data_service\.yaml' -Because '核 Pod 里生效的配置,不核 ConfigMap 模板'
    Assert-NotMatch -Text $runbook -Pattern 'get configmap go-svc-data-service-config' -Because 'ConfigMap 只是模板,核它看不出 Secret 抄错'
    Assert-NotMatch -Text (Read-RepoText 'deploy/k8s/README.md') -Pattern '靠 K8s 重启收敛|K8s 下一次重启就恢复' -Because '主容器重启过的写者 Pod 由回收 CronJob 换新,不是原地恢复'
}

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

exit (Complete-TestRun -SuiteName 'logical database deployment contract')
