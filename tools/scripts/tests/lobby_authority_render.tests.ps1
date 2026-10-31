#requires -Version 7
# 只导入函数定义并渲染本地字符串；不执行部署入口、不调用kubectl、不使用真实密钥。
$ErrorActionPreference = 'Stop'
$RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
. (Join-Path $RepoRoot 'tools/scripts/lib/release_common.ps1')
. (Join-Path $RepoRoot 'tools/scripts/lib/k8s_client_entry.ps1')
$tokens = $null; $parseErrors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $RepoRoot 'tools/scripts/k8s_deploy.ps1'), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw '部署脚本语法错误' }
foreach ($definition in $ast.EndBlock.Statements | Where-Object { $_ -is [Management.Automation.Language.FunctionDefinitionAst] }) {
    . ([scriptblock]::Create($definition.Extent.Text))
}
# 只读取正式脚本的声明数据；不导入入口、密钥初始化或任何集群操作。
$catalog = @($ast.EndBlock.Statements | Where-Object { $_ -is [Management.Automation.Language.AssignmentStatementAst] -and $_.Left.Extent.Text -eq '$GoSvcCatalogue' })
$assetCallers = @($ast.EndBlock.Statements | Where-Object { $_ -is [Management.Automation.Language.AssignmentStatementAst] -and $_.Left.Extent.Text -eq '$script:AssetOpCallers' })
if ($catalog.Count -ne 1 -or $assetCallers.Count -ne 1) { throw '渲染夹具要求唯一服务目录和资产调用方声明' }
$GoSvcCatalogue = & ([scriptblock]::Create($catalog[0].Right.Extent.Text))
# ConfigMap 公共准备段会读取 trade 的 SecretEnv，即使当前只渲染 mail/rank，也必须装配真实声明。
$script:AssetOpCallers = & ([scriptblock]::Create($assetCallers[0].Right.Extent.Text))
$script:AssetOpsOn = $false; $script:AssetOpSecrets = @{}
$ReleaseProfile = 'dev'; $InfraNamespace = 'fixture-infra'
$GlobalDbName = 'fixture_global'
$GlobalAuthorityTiDBHost = 'fixture-global-tidb:4000'
$TradeTiDBHost = $GlobalAuthorityTiDBHost; $FriendTiDBHost = $GlobalAuthorityTiDBHost
$MatchTiDBHost = $GlobalAuthorityTiDBHost; $PhysicalRegion = 7
$DatabaseBackend = 'tidb'
$KafkaBrokers = 3; $OpsProfile = 'custom'
$script:MysqlUser = 'fixture'; $script:MysqlPassword = ''; $script:RedisPassword = ''
$script:GateTokenSecret = ''; $script:InternalAuthSecret = ''
$NodeImage = 'fixture.invalid/game:fixed'; $ImagePullPolicy = 'IfNotPresent'
$SnowflakeCacheDir = '/run/snowflake'; $GrpcServerMaxPollers = 0; $GateRouterMode = '1'
$RequireClientEndpoint = 'auto'; $ClientEntryMode = 'podip'; $NodeOpenFileLimitMode = ''
$script:CppLogSidecarEnabled = $false; $script:CppLogVolumeSizeLimit = '1Gi'; $script:CppLogPrunerCommand = ''
$AgonesHighDensity = $false

function Assert-Render([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }
Assert-Render ($script:AssetOpCallers -is [Collections.IDictionary] -and $script:AssetOpCallers.Contains('guild') -and $script:AssetOpCallers.Contains('trade')) '正式资产调用方声明未装配进渲染夹具'
function Read-RenderedConfig([string]$Service) {
    $manifest = New-GoSvcConfigMapYaml -SvcName $Service -CurrentZoneId 1 -CurrentClusterId 0
    $inside = $false; $body = [Collections.Generic.List[string]]::new()
    foreach ($line in ($manifest -split "`r?`n")) {
        if ($line -match '^  [a-z_]+\.yaml: \|$') { $inside = $true; continue }
        if ($inside -and $line -match '^    (.*)$') { $body.Add($Matches[1]) }
    }
    Assert-Render ($body.Count -gt 0) "没有渲染$Service配置"
    return (ConvertFrom-YamlToFlatMap -Text ($body -join "`n")).Scalars
}
foreach ($service in @('mail','rank')) {
    Assert-Render ([bool]$GoSvcCatalogue[$service].Global) "$service必须是全局池"
    Assert-Render (Test-Path -LiteralPath (Join-Path $RepoRoot "deploy/k8s/manifests/go-svc/$service-migrate.yaml")) "$service缺迁移Job"
    $values = Read-RenderedConfig $service
    Assert-Render ($values['TableDir'] -eq '/generated/tables') "$service配表路径必须匹配Docker镜像"
    Assert-Render ($values['Backend'] -eq 'tidb') "$service缺省是 tidb 档"
    Assert-Render ($values['DataServiceRpc.Etcd.Key'] -eq 'dataservice.rpc') "$service不能拨不存在的infra data-service Service"
}
$mail = Read-RenderedConfig 'mail'
Assert-Render ([bool]$GoSvcCatalogue['guild'].Global) '帮会必须进全局池'
$guild = Read-RenderedConfig 'guild'
Assert-Render ($guild['MySQL.DataSourceEnv'] -eq 'MMORPG_GUILD_SQL_DSN' -and [string]::IsNullOrEmpty($guild['MySQL.DataSource'])) '帮会必须从Secret读取同一全服SQL DSN'
Assert-Render ($guild['TableDir'] -eq '/generated/tables' -and $guild['DataServiceRpc.Etcd.Key'] -eq 'dataservice.rpc' -and $guild['MatchRpc.Etcd.Key'] -eq 'matchservice.rpc') '帮会配表或内部调用未接线'
Assert-Render ($guild['AccountRoleSchema'] -eq $GlobalDbName -and $guild['GlobalLoginSessions'] -eq 'true') '帮会角色与登录会话必须显式指向全服权威'
foreach ($key in @('MatchAuthoritySchema','Timeout','ApplicationQuotaPerMinute','AssetOp.Enabled','Activity.TrialResult.Topic','Activity.TrialResult.Partitions','IdSegment.Step','MySQL.MaxOpenConn','RedisClient.PoolSize','PlayerLocatorRedis.PoolSize','LoginRedis.PoolSize')) {
    $source = Get-YamlScalar -Path (Join-Path $RepoRoot 'go/guild/etc/guild.yaml') -KeyPath $key
    Assert-Render ($source.Found -and $guild[$key] -eq $source.Value) "帮会$key未镜像权威配置"
}
$ReleaseProfile = 'prod'; $guildProd = Read-RenderedConfig 'guild'; $ReleaseProfile = 'dev'
Assert-Render ($guildProd['Mode'] -eq 'pro' -and $guildProd['Schema.AutoMigrate'] -eq 'false' -and $guildProd['MySQL.Backend'] -eq 'tidb') '帮会生产实例缺省 tidb 档，禁开发模式或自行迁表'
Assert-Render ($guildProd['AssetOp.Enabled'] -eq 'false') '父快照围栏门禁未验收前，生产帮会资产通道不得自动启用'
$ReleaseProfile = 'prod'; $KafkaBrokers = 1; $rejected = $false
try { $null = Read-RenderedConfig 'guild' } catch { $rejected = $_.Exception.Message -match 'Kafka' }
$ReleaseProfile = 'dev'; $KafkaBrokers = 3
Assert-Render $rejected '全球帮会历练不得在单broker上部署'
foreach ($manifest in @('guild.yaml','guild-migrate.yaml')) {
    $guildManifest = Get-Content -LiteralPath (Join-Path $RepoRoot "deploy/k8s/manifests/go-svc/$manifest") -Raw
    Assert-Render ($guildManifest.Contains('key: guild-sql-dsn') -and $guildManifest.Contains('secretKeyRef:')) '帮会实例和迁移必须共享Secret DSN'
}
$guildManifest = Get-Content -LiteralPath (Join-Path $RepoRoot 'deploy/k8s/manifests/go-svc/guild.yaml') -Raw
Assert-Render ($guildManifest.Contains('name: POD_IP') -and $guildManifest.Contains('key: asset-guild')) '帮会缺可路由地址或资产签名引用'
$guildLedger = (Get-RedisConnectionLedgerInputs).Clients | Where-Object { $_.Name -eq 'guild' }
$guildPools = Get-RedisPoolMirror -Target 'guild'
$expectedPools = $guildPools['RedisClient.PoolSize'] + $guildPools['PlayerLocatorRedis.PoolSize'] + $guildPools['LoginRedis.PoolSize']
$guildSvcSource = Get-Content -LiteralPath (Join-Path $RepoRoot 'go/guild/internal/svc/servicecontext.go') -Raw
$syncPool = [regex]::Match($guildSvcSource, '(?m)^\s*(?:const\s+)?playerSyncRedisPoolSize\s*=\s*(\d+)\s*$')
Assert-Render ($syncPool.Success -and $guildLedger.Fixed -eq [int]$syncPool.Groups[1].Value) '帮会摘要专用池预算与实际源码不符'
Assert-Render ($guildLedger.ConfiguredPoolConns -eq $expectedPools -and $guildLedger.PubSub -eq 1) '帮会三个池与唤醒订阅未进入Redis连接总账'
Assert-Render ($mail['KafkaBrokers[0]'] -eq 'kafka.fixture-infra:9092' -or $mail['KafkaBrokers'] -match 'kafka\.fixture-infra:9092') '邮件提醒不能拨本地Kafka'
$trade = Read-RenderedConfig 'trade'
Assert-Render ($trade['MySQL.Host'] -eq $TradeTiDBHost) '交易没有使用显式全服TiDB入口'
Assert-Render ($trade['MySQL.Backend'] -eq 'tidb') '交易缺省 tidb 档'
Assert-Render ($trade.Contains('AssetOp.Enabled') -and $trade.Contains('Commerce.AllowInstances') -and $trade.Contains('Mall.GatewaySecretEnv')) '交易域配置未镜像'
$sourceAsset = Get-YamlScalar -Path (Join-Path $RepoRoot 'go/trade/etc/trade.yaml') -KeyPath 'AssetOp.Enabled'
Assert-Render ($trade['AssetOp.Enabled'] -eq $sourceAsset.Value) '资产开关与权威配置不一致'
$rejected = $false
try { $null = Get-TradeSqlSettings -Profile 'prod' -TiDBHost '' -DevelopmentHost 'fixture-mysql:3306' }
catch { $rejected = $_.Exception.Message -match 'TradeTiDBHost' }
Assert-Render $rejected '生产交易不得回落MySQL'
$match = Read-RenderedConfig 'match'
Assert-Render ($match['GlobalMatch.Enabled'] -eq 'true' -and $match['GlobalMatch.Backend'] -eq 'tidb') '全局匹配缺省 tidb 档'
Assert-Render ($match['GlobalMatch.Host'] -eq $MatchTiDBHost -and $match['GlobalMatch.User'] -eq $script:MysqlUser) '匹配未使用显式同服SQL入口'
# 省钱档:-DatabaseBackend mysql 一个参数切全部服务,不留写死 tidb 的 ConfigMap(方案 §9)。
$DatabaseBackend = 'mysql'
foreach ($check in @(
        @{ Service = 'mail'; Key = 'Backend' }, @{ Service = 'rank'; Key = 'Backend' }, @{ Service = 'guild'; Key = 'MySQL.Backend' },
        @{ Service = 'trade'; Key = 'MySQL.Backend' }, @{ Service = 'match'; Key = 'GlobalMatch.Backend' })) {
    $rendered = Read-RenderedConfig $check.Service
    Assert-Render ($rendered[$check.Key] -eq 'mysql') "$($check.Service) 的 $($check.Key) 没有跟 -DatabaseBackend mysql 走"
}
$DatabaseBackend = 'tidb'
Assert-Render ($match['GlobalMatch.RankEndpoint'] -eq 'rank.fixture-infra:51100') '匹配投榜误拨本地地址'
foreach ($key in @('GlobalMatch.GuildTrialSchema','GlobalMatch.CrossRegionWaitSeconds','GlobalMatch.HardLatencyMs','GlobalMatch.ProbeTTLSeconds','ResultTopic','RatingEnabled','RatingToleranceMaxWaitSeconds')) {
    $source = Get-YamlScalar -Path (Join-Path $RepoRoot 'go/match/etc/match_service.yaml') -KeyPath $key
    Assert-Render ($match[$key] -eq $source.Value) "匹配策略$key未镜像"
}
$matchManifest = Add-MatchAuthoritySecretEnv -ManifestContent (Get-Content (Join-Path $RepoRoot 'deploy/k8s/manifests/go-svc/match.yaml') -Raw)
foreach ($envName in @('MATCH_MYSQL_PASSWORD','MATCH_PROBE_SECRET','RANK_PRODUCER_SECRET_MATCH')) {
    Assert-Render ($matchManifest.Contains("name: $envName")) "匹配缺$envName环境引用"
}
$rejected = $false
try { $null = Get-GlobalMatchDeploymentSettings -Profile 'prod' -TiDBHost '' -Region 7 }
catch { $rejected = $_.Exception.Message -match 'MatchTiDBHost' }
Assert-Render $rejected '生产匹配不得退回旧Redis权威'
$rejected = $false
try { $null = Get-GlobalMatchDeploymentSettings -Profile 'dev' -TiDBHost $MatchTiDBHost -Region 0 }
catch { $rejected = $_.Exception.Message -match 'PhysicalRegion' }
Assert-Render $rejected '探测必须带真实机房身份'
$rejected = $false
try { Assert-GlobalMatchKafkaTopology -Topology (Get-KafkaTopologyFor -Brokers 1 -Namespace 'fixture') }
catch { $rejected = $_.Exception.Message -match 'Kafka' }
Assert-Render $rejected '单broker不能启用全球耐久结果提交'

$BattleReplicas = 2; $ClientPublicHost = ''
$AgonesHealthInitialDelaySeconds = 30; $AgonesHealthPeriodSeconds = 10; $AgonesHealthFailureThreshold = 3
foreach ($plan in @(@{Orchestrator='deployment'; Entry='podip'}, @{Orchestrator='deployment'; Entry='external'}, @{Orchestrator='agones'; Entry='external'})) {
    $BattleOrchestrator = $plan.Orchestrator; $ClientEntryMode = $plan.Entry
    $battle = (New-BattleWorkloadManifest).Yaml
    foreach ($envName in @('GLOBAL_MATCH_ENABLED','PHYSICAL_REGION','MATCH_PROBE_SECRET')) {
        Assert-Render ($battle.Contains("name: $envName")) "battle的$BattleOrchestrator/$ClientEntryMode缺$envName"
    }
    Assert-Render ($battle.Contains('match-probe') -and $battle.Contains('secretKeyRef:')) 'battle探测密钥必须来自Secret'
}

$scene = New-NodeDeploymentYaml -NodeName 'scene' -Replicas 1 -RpcPort 22000 -StartCommand './scene' -ConfigMapName 'fixture-scene' -SceneNodeType 0
$fleet = New-SceneFleetYaml -FleetName 'fixture-scene' -Replicas 1 -SceneNodeType 0 -RpcPort 22000 -StartCommand './scene' -ConfigMapName 'fixture-scene' -RoleLabel 'scene' -ZoneIdLabel 1 -ZoneLabel 'fixture'
$gate = New-NodeDeploymentYaml -NodeName 'gate' -Replicas 1 -RpcPort 18000 -StartCommand './gate' -ConfigMapName 'fixture-gate'
foreach ($caller in @('guild','trade','mail')) {
    $envName = 'MMORPG_ASSET_OP_SECRET_' + $caller.ToUpperInvariant()
    Assert-Render ($scene.Contains($envName) -and $fleet.Contains($envName)) "两种scene编排都必须引用$caller资产密钥"
    Assert-Render (-not $gate.Contains($envName)) 'gate不能获得scene资产签名密钥'
}
foreach ($caller in @('scene','trade')) {
    $envName = 'MMORPG_ACCOUNT_DIRECTORY_SECRET_' + $caller.ToUpperInvariant()
    Assert-Render ($scene.Contains($envName) -and $fleet.Contains($envName)) "两种scene编排都必须引用$caller目录验签密钥"
    Assert-Render ($scene.Contains("key: account-directory-$caller, optional: false") -and $fleet.Contains("key: account-directory-$caller, optional: false")) '目录验签密钥不得作为可缺省Secret'
    Assert-Render (-not $gate.Contains($envName)) 'gate不能获得scene目录验签密钥'
}
Write-Output 'PASS：全局域配置、TiDB入口、三种battle探测与两种scene密钥引用渲染通过（零集群操作）'
