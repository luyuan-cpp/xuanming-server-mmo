#requires -Version 7
<#
.SYNOPSIS
    两套 Redis Cluster(玩家域 redis-player、控制 redis-control)的部署侧契约测试(玩家域设计 docs/design/redis-player-store-cluster.md §13 第 10 批;
    控制集群 docs/design/redis-cluster-all-domains-20261110.md §10 第 6a 批)。

.DESCRIPTION
    设计 §5.1、§5.6、§6、§7、§8;运维手册 docs/ops/redis-player-cluster-runbook.md(两套共用);部署说明见 deploy/k8s/README.md「玩家域 Redis Cluster」。
    两套集群同一份模板(deploy/k8s/manifests/infra/redis-cluster.yaml)、同一套部署函数,差别只在 Get-RedisCluster 的描述。本文件钉住:
      1. 清单模板:版本钉摘要、AOF everysec、noeviction、宣告 Service IP、OnDelete、建群 Job 只在全部空白时建群并核对 16384 个 slot;
      2. 纯函数:集群描述、拓扑、容量、变更判定、清单渲染(两套集群对象名互不重叠)、配置段形状;
      3. 连接总账:每主节点 = Σ(每服务池上限 × 副本数) + scene 进程数,和主节点数无关;控制域那一份去掉这些池;控制集群接上服务前没有客户端;
      4. DryRun:dev 缺省两套都不建;-PlayerRedisMasters 3 时清单、ConfigMap、Secret、环境变量、WAITAOF 都到位,口令不进 ConfigMap;
         -ControlRedisMasters 3 时只部署集群本身(清单、Secret、建群 Job、总账一行),还没有服务连它;staging / prod 缺参数在任何写操作之前拒绝;
      5. 告警:组与规则名单、形状,两套集群一条对一条;scene_manager 成员对账的指标名与取值从 Go 源码核对;
      6. compose 开发集群:3 主 3 从、同一个镜像摘要、一次性建群容器;
      7. 容量基准 redis_cluster_capacity.ps1 的判定:没压满就不算容量证据,节点重启或拓扑变化作废。
    子进程一律 -DryRun(不连集群);纯函数按 AST 从 k8s_deploy.ps1 抽出,在本进程里直接调用。不起容器。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/redis_player_cluster_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ''
Write-Host '=== Redis Cluster (player / control) deployment contract tests ==='

$RepoRoot = Get-RepoRoot
$Infra = 'cluster-infra'
$Seeds = @(0..2 | ForEach-Object { "redis-player-$_.${Infra}:6379" })
$ControlSeeds = @(0..2 | ForEach-Object { "redis-control-$_.${Infra}:6379" })
# 虚构值,只用于确认它不进任何 ConfigMap。
$ClusterPassword = 'contract-player-redis-secret-01'
$ControlClusterPassword = 'contract-control-redis-secret-01'
$RedisImagePattern = 'redis:8\.10\.\d+@sha256:[0-9a-f]{64}'
# 集群的 K8s 对象。服务 yaml 的注释里有 docs/design/redis-player-store-cluster.md,不能只匹配 redis-player。
$ClusterObjectPattern = 'redis-player-(\d|auth|cluster-init|cluster-config|headless|pdb)'
$ControlClusterObjectPattern = 'redis-control-(\d|auth|cluster-init|cluster-config|headless|pdb)'

$TemplateText = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/k8s/manifests/infra/redis-cluster.yaml')) -replace "`r`n", "`n"
$ComposeText = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/compose.redis-cluster.yml')) -replace "`r`n", "`n"

$TempRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-redis-player-cluster-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $TempRoot
$CatalogPath = New-SqlWriterCatalogFixture -Root (Join-Path $TempRoot 'catalog') -Namespaces @('mmorpg-zone-cluster-test', $Infra)

function Invoke-ClusterDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '本套夹具只允许显式 -DryRun,禁止连接或写集群。' }
    $fixtureEnv = @{ MMORPG_DB_ALLOWED_DATABASES = $PlayerStoreAllowlistFixture; MMORPG_SQL_K8S_BOOT_CATALOG = $CatalogPath }
    foreach ($key in $Env.Keys) { $fixtureEnv[$key] = $Env[$key] }
    return Invoke-CapturedPwsh -ScriptPath (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1') -Arguments $Arguments -Env $fixtureEnv
}

function Get-ConfigFlat {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$ConfigMap)
    $found = @(Get-ManifestBlocks -Output $Output | Where-Object { $_ -cmatch '(?m)^kind: ConfigMap\s*$' -and $_ -cmatch ('(?m)^  name:\s*' + [regex]::Escape($ConfigMap) + '\s*$') })
    if ($found.Count -ne 1) { throw "DryRun 输出里 ConfigMap $ConfigMap 应当恰好 1 份,实际 $($found.Count) 份" }
    return ConvertTo-FlatManifest -Block $found[0]
}

function Get-WorkloadBlock {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$Kind, [Parameter(Mandatory = $true)][string]$Name)
    $found = @(Get-ManifestBlocks -Output $Output | Where-Object { $_ -cmatch "(?m)^kind: $Kind\s*$" -and $_ -cmatch ('(?m)^  name: ' + [regex]::Escape($Name) + '\s*$') })
    if ($found.Count -ne 1) { throw "DryRun 输出里 $Kind $Name 应当恰好 1 份,实际 $($found.Count) 份" }
    return $found[0]
}

# SQL 写者都是 StatefulSet:Pod 序号就是写者名册的槽位(docs/design/writer-roster-exhaustion-20261106.md §5.5);其余 Go 服务是 Deployment。
# 名单同 k8s_deploy.ps1 的 $script:SqlWriterGoSvcs。
function Get-ServiceWorkloadKind {
    param([Parameter(Mandatory = $true)][string]$Name)
    if (@('db', 'data-service', 'login', 'chat', 'friend', 'guild', 'mail', 'rank', 'match', 'trade', 'player-inbox') -ccontains $Name) { return 'StatefulSet' }
    return 'Deployment'
}

function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

# 服务 → ConfigMap 里配置文件的键前缀。
$ServiceConfigs = [ordered]@{
    login            = 'data.login.yaml.'
    'data-service'   = 'data.data_service.yaml.'
    'player-locator' = 'data.player_locator.yaml.'
    'scene-manager'  = 'data.scene_manager_service.yaml.'
    match            = 'data.match_service.yaml.'
    chat             = 'data.chat.yaml.'
    friend           = 'data.friend.yaml.'
    guild            = 'data.guild.yaml.'
    trade            = 'data.trade.yaml.'
    'player-inbox'   = 'data.player_inbox.yaml.'
    mail             = 'data.mail.yaml.'
}
$ZoneServices = @('login', 'data-service', 'player-locator', 'scene-manager')
# 连玩家域的全局服务。chat 不连 Redis,不在这里。
$GlobalServices = @('match', 'friend', 'guild', 'trade', 'player-inbox', 'mail')
# 只认 gate 签名的会话身份原件、不读登录态的全局服务:ConfigMap 里不该有 GlobalLoginSessions / LoginRedis(trade 的曾在 Rmb 下)。
$SignedIdentityServices = @('match', 'chat', 'friend', 'guild', 'mail', 'trade')

# ConfigMap 里登录态段的残留键(顶层与 Rmb 下都查),给上面这几个服务做反向断言用。
function Get-LoginSessionKeys {
    param([Parameter(Mandatory = $true)]$Flat, [Parameter(Mandatory = $true)][string]$Prefix)
    return @($Flat.Scalars.Keys | ForEach-Object { [string]$_ } | Where-Object {
            $_.StartsWith($Prefix, [System.StringComparison]::Ordinal) -and $_.Substring($Prefix.Length) -cmatch '^(?:Rmb\.)?(?:GlobalLoginSessions|LoginRedis)(?:$|[.\[])'
        })
}

$ZoneArgs = @('-Command', 'zone-up', '-ZoneName', 'cluster-test', '-ZoneId', '101', '-DryRun', '-InfraNamespace', $Infra,
    '-GlobalAuthorityTiDBHost', 'tidb.invalid:4000', '-GoSvcRegistry', 'registry.invalid/test', '-JavaSvcRegistry', 'registry.invalid/test')
# 保持玩家域形态为本套变量；全局服务另显式满足正式 Kafka RF=3、minISR=2 准入。
$InfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-SkipJavaSvc', '-KafkaBrokers', '3', '-InfraNamespace', $Infra,
    '-GlobalAuthorityTiDBHost', 'tidb.invalid:4000', '-GoSvcRegistry', 'registry.invalid/test', '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab')
$InfraEnv = @{ MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345' }
$ClusterEnv = @{ MMORPG_PLAYER_REDIS_PASSWORD = $ClusterPassword }
$ClusterInfraEnv = @{} + $InfraEnv + $ClusterEnv
$ControlClusterEnv = @{ MMORPG_CONTROL_REDIS_PASSWORD = $ControlClusterPassword }
$ControlClusterInfraEnv = @{} + $InfraEnv + $ControlClusterEnv
# 同 k8s_deploy_contract.tests.ps1 的 $ProdEnv:走到玩家域口令那一步之前要的密钥。
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
}
$ProdZoneArgs = $ZoneArgs + @('-ReleaseProfile', 'prod', '-SkipPreflight', '-NodeImage', 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab')
# 不部署 Go 服务:这里只测入口闸,全局服务在 prod 还要各自的 SQL 入口与机房号,与本文件无关。
$ProdInfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-SkipGoSvc', '-SkipJavaSvc', '-InfraNamespace', $Infra,
    '-GlobalAuthorityTiDBHost', 'tidb.invalid:4000', '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab', '-ReleaseProfile', 'prod')

$devZoneRun = Invoke-ClusterDryRun -Arguments $ZoneArgs
$devInfraRun = Invoke-ClusterDryRun -Arguments $InfraArgs -Env $InfraEnv
$clusterZoneRun = Invoke-ClusterDryRun -Arguments ($ZoneArgs + @('-PlayerRedisMasters', '3')) -Env $ClusterEnv
$clusterInfraRun = Invoke-ClusterDryRun -Arguments ($InfraArgs + @('-PlayerRedisMasters', '3')) -Env $ClusterInfraEnv
# 控制集群(缺口第 34 条第 6a 批):只开它一套,和两套都开。
$controlZoneRun = Invoke-ClusterDryRun -Arguments ($ZoneArgs + @('-ControlRedisMasters', '3')) -Env $ControlClusterEnv
$controlInfraRun = Invoke-ClusterDryRun -Arguments ($InfraArgs + @('-ControlRedisMasters', '3')) -Env $ControlClusterInfraEnv
$bothInfraRun = Invoke-ClusterDryRun -Arguments ($InfraArgs + @('-PlayerRedisMasters', '3', '-ControlRedisMasters', '3')) -Env (@{} + $ClusterInfraEnv + $ControlClusterEnv)

# ─────────────────────────────────────────────────────────────────
# 1. 清单模板(静态)
# ─────────────────────────────────────────────────────────────────

Test-Case '清单模板:redis 节点与建群 Job 同一个镜像,钉 8.10.x 与摘要;redis-match-cluster 不再写 C++ 不支持集群' {
    $images = @([regex]::Matches($TemplateText, '(?m)^\s+image:\s*(redis:\S+)\s*$') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 1 -Actual $images.Count -Because '节点与建群 Job 必须同一个镜像'
    Assert-Match -Text $images[0] -Pattern "^$RedisImagePattern$" -Because '扩缩容要 8.4 起的原子迁移;只钉标签时内容可被重推'
    $matchCluster = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/k8s/manifests/infra/redis-match-cluster.yaml'))
    Assert-NotMatch -Text $matchCluster -Pattern 'hiredis 不支持' -Because 'C++ 已有自研路由层访问 Redis Cluster,旧说法会把后来人带偏'
    $matchImages = @([regex]::Matches($matchCluster, '(?m)^\s+image:\s*(redis:\S+)\s*$') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
    Assert-Equal -Expected $images[0] -Actual ($matchImages -join ',') -Because 'match 私有集群与两套集群同一个镜像摘要(缺口第 34 条第 6a 批从 7.2 升上来):节点、等待与建群三处一起改'
    # 两套集群的全部对象名都从 __REDIS_CLUSTER_NAME__ 派生;写死一个名字,两套集群就会抢同一个对象。手册路径不是对象名,不算。
    $codeLines = @(($TemplateText -split "`n") | Where-Object { $_ -notmatch '^\s*#' })
    $hardCoded = @($codeLines | Where-Object { $_ -cmatch 'redis-(player|control)(?!-cluster-runbook\.md)' })
    Assert-Equal -Expected 0 -Actual $hardCoded.Count -Because "模板不写死集群名(命中:$($hardCoded -join ' | '))"
    Assert-True -Condition (-not (Test-Path -LiteralPath (Join-Path $RepoRoot 'deploy/k8s/manifests/infra/redis-player-cluster.yaml'))) -Because '玩家域旧模板已并进 redis-cluster.yaml,留着就是第二份真相'
}

Test-Case '清单模板:redis.conf 的持久化、淘汰与集群参数(设计 §7)' {
    foreach ($line in @('cluster-enabled yes', 'appendonly yes', 'appendfsync everysec', 'save ""', 'maxmemory-policy noeviction', 'cluster-require-full-coverage no',
            'cluster-allow-reads-when-down no', 'cluster-allow-replica-migration yes', 'cluster-preferred-endpoint-type ip', 'enable-debug-command no')) {
        Assert-Match -Text $TemplateText -Pattern ('(?m)^\s+' + [regex]::Escape($line) + '\s*$') -Because "redis.conf 必须有 $line"
    }
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($TemplateText, '(?m)^[ \t]*MAXCLIENTS=(\d+)[ \t]*$').Count) -Because 'MAXCLIENTS 只定义一次(总账从这一行读)'
    Assert-Match -Text $TemplateText -Pattern 'redis fd limit:' -Because '启动时打印 fd 限值(同 redis.yaml)'
}

Test-Case '清单模板:宣告 Service IP 与端口(C++ 路由层只认 IP 字面量,§5.6);解析不到就退出;OnDelete' {
    foreach ($flag in @('--cluster-announce-ip "$announce_ip"', '--cluster-announce-port 6379', '--cluster-announce-bus-port 16379')) {
        Assert-Match -Text $TemplateText -Pattern ([regex]::Escape($flag)) -Because "启动参数要有 $flag"
    }
    Assert-Match -Text $TemplateText -Pattern 'getent hosts "\$service_host"' -Because '宣告的是本 Pod 对应 Service 的 IP'
    Assert-NotMatch -Text $TemplateText -Pattern 'status\.podIP' -Because '不宣告 Pod IP:Pod 重建就换地址,nodes.conf 与客户端拓扑都失效'
    Assert-Match -Text $TemplateText -Pattern 'refusing to start without an announce address' -Because '解析不到不能带错地址起来'
    Assert-Match -Text $TemplateText -Pattern '(?m)^  updateStrategy:\s*\n    type: OnDelete\s*$' -Because 'apply 改了模板不重启节点,滚动按手册先切主'
    Assert-Match -Text $TemplateText -Pattern '--requirepass "\$REDIS_PASSWORD" --masterauth "\$REDIS_PASSWORD"' -Because '设了口令时副本连主也要认证,否则副本永远同步不上'
    Assert-Match -Text $TemplateText -Pattern '(?m)^        - name: redis\s*$' -Because '容器名必须叫 redis(运维脚本按 -c redis exec)'
    Assert-Match -Text $TemplateText -Pattern '(?m)^        - name: redis-exporter\s*$' -Because '带 exporter sidecar,告警才有数据'
    Assert-Match -Text $TemplateText -Pattern 'prometheus\.io/port: "9121"' -Because '抓取注解指向 exporter'
    $exporterPattern = '(?m)^\s+image:\s*(oliver006/redis_exporter:\S+)\s*$'
    $controlManifest = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/k8s/manifests/infra/redis.yaml'))
    Assert-Equal -Expected ([regex]::Match($controlManifest, $exporterPattern).Groups[1].Value) -Actual ([regex]::Match($TemplateText, $exporterPattern).Groups[1].Value) -Because 'exporter 与控制域同一版本,告警的指标名才是同一份'
    Assert-Match -Text $TemplateText -Pattern 'kubectl\.kubernetes\.io/default-container: redis' -Because 'default-container 指向 redis'
}

Test-Case '清单模板:建群 Job 只在全部节点空白时建群,核对 cluster_state、16384 个 slot,口令走环境变量' {
    Assert-Match -Text $TemplateText -Pattern 'redis-cli --cluster create \$seeds --cluster-replicas "\$REPLICAS_PER_MASTER" --cluster-yes' -Because '建群'
    Assert-Match -Text $TemplateText -Pattern 'if \[ "\$known" = "1" \] && \[ "\$assigned" = "0" \]; then blank=' -Because '空白 = 只认识自己且没有 slot'
    Assert-Match -Text $TemplateText -Pattern 'if \[ -z "\$joined" \]; then' -Because '只有一个节点都没入群才建;否则只核对,重跑幂等,不碰已有集群'
    Assert-Match -Text $TemplateText -Pattern 'cluster_state\)" != "ok"' -Because '核对 cluster_state'
    Assert-Match -Text $TemplateText -Pattern 'cluster_slots_assigned\)" != "16384"' -Because '核对 16384 个 slot 已分配'
    Assert-Match -Text $TemplateText -Pattern 'cluster_slots_ok\)" != "16384"' -Because '核对 16384 个 slot 可用'
    Assert-Match -Text $TemplateText -Pattern 'redis-cli --cluster check "\$first:6379"' -Because '节点之间对 slot 配置一致'
    Assert-Match -Text $TemplateText -Pattern '- name: REDISCLI_AUTH\s+valueFrom:\s+secretKeyRef:\s+name: __REDIS_CLUSTER_AUTH_SECRET__' -Because '口令不上命令行;Secret 名随集群渲染'
    Assert-NotMatch -Text $TemplateText -Pattern '--cluster fix' -Because '建群 Job 不跑 fix:可能把缺的 slot 分给空节点,旧数据回不来'
}

# ─────────────────────────────────────────────────────────────────
# 2. 纯函数(按 AST 从 k8s_deploy.ps1 抽出)
# ─────────────────────────────────────────────────────────────────

$deployParseErrors = $null
$deployAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$deployParseErrors)
if ($deployParseErrors.Count -gt 0) { throw "k8s_deploy.ps1 解析失败:$($deployParseErrors[0].Message)" }
$pureFunctionNames = @(
    'Get-RedisCluster', 'Get-RedisClusters', 'Get-RedisClusterTopologyFor', 'Resolve-RedisClusterCapacity', 'Get-RedisClusterChangeVerdict', 'ConvertTo-RedisClusterManifest',
    'Assert-InfraManifestPlaceholdersReplaced', 'New-PlayerRedisConfigYaml', 'New-NodePlayerRedisBlock',
    # 连接总账与它读文件的依赖($RepoRoot 按动态作用域读)。
    'Get-AuthoritativeScalar', 'Get-ManifestDeploymentReplicas', 'Get-RedisMaxClientsFromManifest', 'Get-RedisPoolMirror',
    'Get-RedisPlayerPoolMirror', 'Get-RedisConnectionLedgerInputs', 'Get-RedisClusterConnectionLedgerInputs', 'Get-RedisConnectionLedger'
)
foreach ($fnAst in $deployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($pureFunctionNames -contains $fnAst.Name) {
        Set-Item -Path "Function:script:$($fnAst.Name)" -Value $fnAst.Body.GetScriptBlock()
    }
}
foreach ($fnName in $pureFunctionNames) {
    if (-not (Test-Path "Function:$fnName")) { throw "k8s_deploy.ps1 缺少函数 ${fnName}(Redis Cluster 的接入被删了?后续断言无意义)" }
}
$PlayerCluster = Get-RedisCluster -Kind 'player'
$ControlCluster = Get-RedisCluster -Kind 'control'

Test-Case 'Get-RedisCluster / Get-RedisClusters:两套集群的名字、Secret、口令变量、参数前缀互不相同;顺序固定玩家域在前;未知种类拒绝' {
    Assert-Equal -Expected 'redis-player/redis-player-auth/password/MMORPG_PLAYER_REDIS_PASSWORD/-PlayerRedis' `
        -Actual ('{0}/{1}/{2}/{3}/{4}' -f $PlayerCluster.Name, $PlayerCluster.AuthSecretName, $PlayerCluster.AuthSecretKey, $PlayerCluster.PasswordEnv, $PlayerCluster.ParamPrefix) -Because '玩家域的描述'
    Assert-Equal -Expected 'redis-control/redis-control-auth/password/MMORPG_CONTROL_REDIS_PASSWORD/-ControlRedis' `
        -Actual ('{0}/{1}/{2}/{3}/{4}' -f $ControlCluster.Name, $ControlCluster.AuthSecretName, $ControlCluster.AuthSecretKey, $ControlCluster.PasswordEnv, $ControlCluster.ParamPrefix) -Because '控制集群的描述'
    Assert-Equal -Expected 'player,control' -Actual (@(Get-RedisClusters | ForEach-Object { $_.Kind }) -join ',') -Because 'apply、等就绪、总账都按这个顺序'
    foreach ($field in @('Name', 'AuthSecretName', 'PasswordEnv', 'ParamPrefix', 'LogTitle', 'LedgerTitle', 'WarningTag', 'LedgerWarningTag')) {
        Assert-True -Condition ([string]$PlayerCluster.$field -cne [string]$ControlCluster.$field) -Because "两套集群的 $field 不能相同,否则对象、口令或告警前缀互相覆盖"
    }
    Assert-Match -Text (Get-ThrownMessage { Get-RedisCluster -Kind 'queue' }) -Pattern '未知的 Redis Cluster 种类' -Because '拼错种类不能悄悄落到某一套集群上'
}

Test-Case 'Get-RedisClusterTopologyFor:dev 的 0 不建集群;staging / prod 的 0 拒绝并点名本集群的参数;1、2 任何档位都拒绝;节点数、种子、min-replicas-to-write' {
    foreach ($cluster in @($PlayerCluster, $ControlCluster)) {
        $prefix = $cluster.ParamPrefix
        $none = Get-RedisClusterTopologyFor -Cluster $cluster -Masters 0 -ReplicasPerMaster 1 -ReleaseProfileName 'dev' -InfraNamespace 'ns'
        Assert-True -Condition (-not $none.Enabled) -Because "$($cluster.Name):dev 缺省不建集群"
        Assert-Equal -Expected 0 -Actual $none.Nodes -Because "$($cluster.Name):不建集群时节点数 0(变更判定读它)"
        foreach ($releaseProfile in @('staging', 'prod')) {
            $message = Get-ThrownMessage { Get-RedisClusterTopologyFor -Cluster $cluster -Masters 0 -ReplicasPerMaster 1 -ReleaseProfileName $releaseProfile -InfraNamespace 'ns' }
            Assert-Match -Text $message -Pattern ([regex]::Escape("必须显式给 ${prefix}Masters")) -Because "$($cluster.Name):$releaseProfile 不替运维猜主节点数"
        }
        foreach ($masters in @(1, 2)) {
            foreach ($releaseProfile in @('dev', 'prod')) {
                $message = Get-ThrownMessage { Get-RedisClusterTopologyFor -Cluster $cluster -Masters $masters -ReplicasPerMaster 1 -ReleaseProfileName $releaseProfile -InfraNamespace 'ns' }
                Assert-Match -Text $message -Pattern ([regex]::Escape("${prefix}Masters $masters 不合法")) -Because "$($cluster.Name):$releaseProfile 的 $masters 个主选不出新主"
            }
        }
        $one = Get-RedisClusterTopologyFor -Cluster $cluster -Masters 3 -ReplicasPerMaster 1 -ReleaseProfileName 'prod' -InfraNamespace 'ns'
        Assert-Equal -Expected '6/0' -Actual ('{0}/{1}' -f $one.Nodes, $one.MinReplicasToWrite) -Because '每主 1 副本:副本一重启就整片拒写不可接受,min-replicas-to-write 取 0'
        $two = Get-RedisClusterTopologyFor -Cluster $cluster -Masters 3 -ReplicasPerMaster 2 -ReleaseProfileName 'prod' -InfraNamespace 'ns'
        Assert-Equal -Expected '9/1' -Actual ('{0}/{1}' -f $two.Nodes, $two.MinReplicasToWrite) -Because '每主 2 副本取 1'
        $large = Get-RedisClusterTopologyFor -Cluster $cluster -Masters 16 -ReplicasPerMaster 1 -ReleaseProfileName 'prod' -InfraNamespace 'ns'
        $expectedSeeds = @(0..2 | ForEach-Object { '{0}-{1}.ns:6379' -f $cluster.Name, $_ }) -join ','
        Assert-Equal -Expected $expectedSeeds -Actual ($large.Seeds -join ',') -Because '种子固定序号 0、1、2,不随主节点数变:扩容不用重发 zone'
    }
}

Test-Case 'Resolve-RedisClusterCapacity:非 dev 缺容量拒绝并点名本集群的参数;写法不对拒绝;容器内存 = 2 × maxmemory' {
    foreach ($cluster in @($PlayerCluster, $ControlCluster)) {
        $prefix = $cluster.ParamPrefix
        $message = Get-ThrownMessage { Resolve-RedisClusterCapacity -Cluster $cluster -MaxMemory '' -Storage '' -ReleaseProfileName 'staging' }
        Assert-Match -Text $message -Pattern ([regex]::Escape("${prefix}MaxMemory")) -Because "$($cluster.Name):点名缺的参数"
        Assert-Match -Text $message -Pattern ([regex]::Escape("${prefix}Storage")) -Because "$($cluster.Name):点名缺的参数"
        $dev = Resolve-RedisClusterCapacity -Cluster $cluster -MaxMemory '' -Storage '' -ReleaseProfileName 'dev'
        Assert-Equal -Expected '256mb/512Mi/2Gi' -Actual ('{0}/{1}/{2}' -f $dev.MaxMemory, $dev.ContainerMemory, $dev.Storage) -Because 'dev 取小值'
        $prod = Resolve-RedisClusterCapacity -Cluster $cluster -MaxMemory '8GB' -Storage '64Gi' -ReleaseProfileName 'prod'
        Assert-Equal -Expected '8gb/16Gi/64Gi' -Actual ('{0}/{1}/{2}' -f $prod.MaxMemory, $prod.ContainerMemory, $prod.Storage) -Because 'AOF 重写 fork 时写时复制最坏翻倍,容器内存按 2 倍'
        foreach ($bad in @(@{ MaxMemory = '8'; Storage = '64Gi' }, @{ MaxMemory = '0gb'; Storage = '64Gi' }, @{ MaxMemory = '8gb'; Storage = '64gb' }, @{ MaxMemory = '8gb'; Storage = '64' })) {
            $message = Get-ThrownMessage { Resolve-RedisClusterCapacity -Cluster $cluster -MaxMemory $bad.MaxMemory -Storage $bad.Storage -ReleaseProfileName 'dev' }
            Assert-Match -Text $message -Pattern '不合法' -Because "$($bad.MaxMemory) / $($bad.Storage) 必须拒绝"
        }
    }
}

Test-Case 'Get-RedisClusterChangeVerdict:真值表(缩容、下线、缺集群都不放行)' {
    foreach ($case in @(
            @{ Live = $null; Desired = 0; Infra = $false; Verdict = 'none' }
            @{ Live = $null; Desired = 0; Infra = $true; Verdict = 'none' }
            @{ Live = $null; Desired = 6; Infra = $true; Verdict = 'create' }
            @{ Live = $null; Desired = 6; Infra = $false; Verdict = 'missing' }
            @{ Live = 6; Desired = 0; Infra = $false; Verdict = 'disable' }
            @{ Live = 6; Desired = 0; Infra = $true; Verdict = 'disable' }
            @{ Live = 6; Desired = 6; Infra = $true; Verdict = 'unchanged' }
            @{ Live = 6; Desired = 12; Infra = $false; Verdict = 'unchanged' }
            @{ Live = 6; Desired = 12; Infra = $true; Verdict = 'scale-up' }
            @{ Live = 12; Desired = 6; Infra = $true; Verdict = 'shrink' }
        )) {
        $actual = Get-RedisClusterChangeVerdict -LiveNodes $case.Live -DesiredNodes $case.Desired -DeploysInfra $case.Infra
        Assert-Equal -Expected $case.Verdict -Actual $actual -Because "线上 $($case.Live) → 目标 $($case.Desired),apply=$($case.Infra)"
    }
}

Test-Case 'ConvertTo-RedisClusterManifest:每个节点一个 ClusterIP Service,占位全部替换;模板被改坏就 throw' {
    $topology = Get-RedisClusterTopologyFor -Cluster $PlayerCluster -Masters 3 -ReplicasPerMaster 2 -ReleaseProfileName 'prod' -InfraNamespace 'ns'
    $capacity = Resolve-RedisClusterCapacity -Cluster $PlayerCluster -MaxMemory '8gb' -Storage '64Gi' -ReleaseProfileName 'prod'
    $manifest = ConvertTo-RedisClusterManifest -Template $TemplateText -Cluster $PlayerCluster -Topology $topology -Capacity $capacity -InfraNamespace 'ns'
    Assert-Equal -Expected 'redis-player/redis-player-cluster-init/9/3' -Actual ('{0}/{1}/{2}/{3}' -f $manifest.Name, $manifest.InitJob, $manifest.Nodes, $manifest.Masters) -Because '摘要字段'
    $content = $manifest.Content
    Assert-NotMatch -Text $content -Pattern '__[A-Z][A-Z0-9_]*__' -Because '占位全部替换'
    $perPod = @([regex]::Matches($content, '(?m)^  name: redis-player-(\d+)\s*$') | ForEach-Object { [int]$_.Groups[1].Value } | Sort-Object)
    Assert-Equal -Expected '0,1,2,3,4,5,6,7,8' -Actual ($perPod -join ',') -Because '序号连续,每个节点一个 Service'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($content, '(?m)^  clusterIP: None\s*$').Count) -Because '只有 governing service 是 headless,每 Pod 的 Service 必须有 ClusterIP'
    Assert-Equal -Expected 9 -Actual ([regex]::Matches($content, 'statefulset\.kubernetes\.io/pod-name: redis-player-\d+').Count) -Because '每个 Service 只选自己那个 Pod'
    Assert-Match -Text $content -Pattern '(?m)^  replicas: 9\s*$' -Because 'StatefulSet 副本数 = 节点总数'
    Assert-Match -Text $content -Pattern '(?m)^    min-replicas-to-write 1\s*$' -Because '每主 2 副本取 1'
    Assert-Match -Text $content -Pattern '(?m)^    maxmemory 8gb\s*$' -Because 'maxmemory'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($content, '(?m)^\s+memory: 16Gi\s*$').Count) -Because '内存 requests = limits = 2 × maxmemory'
    Assert-Match -Text $content -Pattern '(?m)^\s+storage: 64Gi\s*$' -Because 'PVC'
    Assert-Match -Text $content -Pattern '- name: REPLICAS_PER_MASTER\s+value: "2"' -Because '建群 Job 的每主副本数'
    Assert-Match -Text $content -Pattern '- name: NODES\s+value: "9"' -Because '建群 Job 等多少个节点'
    # 文档之间的分隔还在:每 Pod 的 Service 不能粘成一份。
    $docs = @($content -split '(?m)^---[ \t]*$' | Where-Object { $_ -cmatch '(?m)^kind:' })
    Assert-Equal -Expected 9 -Actual @($docs | Where-Object { $_ -cmatch 'mmorpg\.io/redis-player-node: "true"' }).Count -Because '每 Pod 的 Service 各是一份文档'
    Assert-Match -Text $content -Pattern '- name: REDISCLI_AUTH\s+valueFrom:\s+secretKeyRef:\s+name: redis-player-auth' -Because '建群 Job 用本集群的口令 Secret'
    $broken = $TemplateText.Replace('__REDIS_CLUSTER_ORDINAL__', '0')
    Assert-True -Condition ($broken -cne $TemplateText) -Because '夹具前提:模板里有序号占位'
    $message = Get-ThrownMessage { ConvertTo-RedisClusterManifest -Template $broken -Cluster $PlayerCluster -Topology $topology -Capacity $capacity -InfraNamespace 'ns' }
    Assert-Match -Text $message -Pattern '带序号占位的文档应恰好 1 份' -Because '模板没有序号占位就不能展开,不能悄悄只渲染一个 Service'
}

Test-Case 'ConvertTo-RedisClusterManifest:控制集群的对象名、标签、口令 Secret 全部带 redis-control 前缀,与玩家域那份没有一个对象同名' {
    $capacity = Resolve-RedisClusterCapacity -Cluster $ControlCluster -MaxMemory '4gb' -Storage '32Gi' -ReleaseProfileName 'prod'
    $control = ConvertTo-RedisClusterManifest -Template $TemplateText -Cluster $ControlCluster -Capacity $capacity -InfraNamespace 'ns' `
        -Topology (Get-RedisClusterTopologyFor -Cluster $ControlCluster -Masters 3 -ReplicasPerMaster 1 -ReleaseProfileName 'prod' -InfraNamespace 'ns')
    $player = ConvertTo-RedisClusterManifest -Template $TemplateText -Cluster $PlayerCluster -Capacity $capacity -InfraNamespace 'ns' `
        -Topology (Get-RedisClusterTopologyFor -Cluster $PlayerCluster -Masters 3 -ReplicasPerMaster 1 -ReleaseProfileName 'prod' -InfraNamespace 'ns')
    Assert-Equal -Expected 'redis-control/redis-control-cluster-init/6/3' -Actual ('{0}/{1}/{2}/{3}' -f $control.Name, $control.InitJob, $control.Nodes, $control.Masters) -Because '摘要字段'
    $content = $control.Content
    Assert-NotMatch -Text $content -Pattern '__[A-Z][A-Z0-9_]*__' -Because '占位全部替换'
    # 模板头注释同时介绍两套集群,只看非注释行;手册路径两套共用,不是对象名。
    $code = (@(($content -split "`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n") -replace 'redis-player-cluster-runbook\.md', ''
    Assert-NotMatch -Text $code -Pattern 'redis-player' -Because '控制集群的产物不引用玩家域的任何对象'
    foreach ($name in @('redis-control-cluster-config', 'redis-control-headless', 'redis-control-pdb', 'redis-control', 'redis-control-cluster-init')) {
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($content, ('(?m)^  name: ' + [regex]::Escape($name) + '\s*$')).Count) -Because "对象 $name 恰好一份"
    }
    Assert-Equal -Expected 6 -Actual ([regex]::Matches($content, 'mmorpg\.io/redis-control-node: "true"').Count) -Because '每 Pod 一个 Service,标签带本集群名'
    Assert-Equal -Expected 3 -Actual ([regex]::Matches($content, 'name: redis-control-auth').Count) -Because '节点、exporter 与建群 Job 三处都引用本集群的口令 Secret'
    Assert-Match -Text $content -Pattern 'redis cluster redis-control verified: 16384 slots covered' -Because '建群 Job 的日志写明是哪套集群'
    $objectNames = { param($Text) @([regex]::Matches($Text, '(?m)^  name: (\S+)\s*$') | ForEach-Object { $_.Groups[1].Value }) }
    $shared = @(& $objectNames $control.Content | Where-Object { (& $objectNames $player.Content) -ccontains $_ })
    Assert-Equal -Expected 0 -Actual $shared.Count -Because "两套集群在同一个 namespace 里,对象名不能重叠(重叠:$($shared -join ', '))"
}

Test-Case 'New-PlayerRedisConfigYaml / New-NodePlayerRedisBlock:集群写种子、Cluster、PasswordEnv;没开集群只写池上限;C++ 段不写口令' {
    $cluster = Get-RedisClusterTopologyFor -Cluster $PlayerCluster -Masters 3 -ReplicasPerMaster 1 -ReleaseProfileName 'prod' -InfraNamespace 'ns'
    $none = Get-RedisClusterTopologyFor -Cluster $PlayerCluster -Masters 0 -ReplicasPerMaster 1 -ReleaseProfileName 'dev' -InfraNamespace 'ns'
    $flat = (ConvertFrom-YamlToFlatMap -Text (New-PlayerRedisConfigYaml -Section 'PlayerRedis' -Topology $cluster -PoolSizePerNode 20 -ClusterPasswordEnv 'PLAYER_REDIS_PASSWORD')).Scalars
    Assert-Equal -Expected 'redis-player-0.ns:6379' -Actual $flat['PlayerRedis.Addrs[0]'] -Because '种子'
    Assert-Equal -Expected 'true' -Actual $flat['PlayerRedis.Cluster'] -Because 'Cluster'
    Assert-Equal -Expected 'PLAYER_REDIS_PASSWORD' -Actual $flat['PlayerRedis.PasswordEnv'] -Because '只写变量名'
    Assert-Equal -Expected '20' -Actual $flat['PlayerRedis.PoolSizePerNode'] -Because '池上限'
    Assert-Equal -Expected "PlayerRedis:`n  PoolSizePerNode: 20" -Actual (New-PlayerRedisConfigYaml -Section 'PlayerRedis' -Topology $none -PoolSizePerNode 20) -Because '没开集群只写池上限,地址由服务指向现有实例'
    Assert-Equal -Expected "LoginRedis:`n  PoolSizePerNode: 20" -Actual (New-PlayerRedisConfigYaml -Section 'LoginRedis' -Topology $none -PoolSizePerNode 20) -Because 'login 自己的登录域段同形:没开集群只写池上限,地址落到它的 Node.RedisClient'
    Assert-Equal -Expected '' -Actual (New-NodePlayerRedisBlock -Topology $none) -Because '没开集群 node-config 不写这一段(缺失 = standalone,沿用 zoneredis)'
    $node = (ConvertFrom-YamlToFlatMap -Text (New-NodePlayerRedisBlock -Topology $cluster)).Scalars
    Assert-Equal -Expected 'cluster' -Actual $node['player_redis.mode'] -Because 'mode'
    Assert-Equal -Expected 'redis-player-2.ns:6379' -Actual $node['player_redis.seeds[2]'] -Because '种子'
    Assert-True -Condition (-not $node.Contains('player_redis.password')) -Because 'scene 经 Secret 拿口令,不进 ConfigMap'
}

# ─────────────────────────────────────────────────────────────────
# 3. 连接总账
# ─────────────────────────────────────────────────────────────────

Test-Case '玩家域总账输入:每个服务的每节点连接 = PlayerRedis 池 + login 自己的 LoginRedis 池 + 代码里写死的池;maxclients 读集群模板' {
    $inputs = Get-RedisClusterConnectionLedgerInputs -Cluster $PlayerCluster
    Assert-Equal -Expected 40000 -Actual $inputs.MaxClients -Because '读 redis-cluster.yaml 的 MAXCLIENTS(两套集群同一个值)'
    Assert-Equal -Expected 1 -Actual $inputs.CppConnectionsPerProcess -Because '每个 scene 进程每个主节点 1 条'
    $player = { param($Path) [int](Get-EtcValue -RelativePath $Path -KeyPath 'PlayerRedis.PoolSizePerNode') }
    $mintSource = [IO.File]::ReadAllText((Join-Path $RepoRoot 'go/scene_manager/internal/svc/owner_epoch_mint.go'))
    $mintPool = [int][regex]::Match($mintSource, '(?m)^\s*(?:const\s+)?ownerEpochMintPoolSize\s*=\s*(\d+)\s*$').Groups[1].Value
    $syncSource = [IO.File]::ReadAllText((Join-Path $RepoRoot 'go/guild/internal/svc/servicecontext.go'))
    $syncPool = [int][regex]::Match($syncSource, '(?m)^\s*(?:const\s+)?playerSyncRedisPoolSize\s*=\s*(\d+)\s*$').Groups[1].Value
    Assert-True -Condition ($mintPool -gt 0 -and $syncPool -gt 0) -Because "写死的池上限要从源码读到(实际 $mintPool / $syncPool),写法变了同步本用例"
    # 登录域只有 login 连;match、friend、guild、trade、mail 只认 gate 签名的会话身份原件,只剩玩家域句柄;chat 不连 Redis,不在表里。
    $expected = [ordered]@{
        login            = (& $player 'go/login/etc/login.yaml') + [int](Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath 'LoginRedis.PoolSizePerNode')
        # GM 回收读玩家位置键(go/data_service/internal/config/config.go 的 PlayerRedis),连接按打开计。
        'data-service'   = (& $player 'go/data_service/etc/data_service.yaml')
        'player-locator' = (& $player 'go/player_locator/etc/player_locator.yaml')
        'scene-manager'  = (& $player 'go/scene_manager/etc/scene_manager_service.yaml') + $mintPool
        match            = (& $player 'go/match/etc/match_service.yaml')
        friend           = (& $player 'go/friend/etc/friend.yaml')
        guild            = (& $player 'go/guild/etc/guild.yaml') + $syncPool
        trade            = (& $player 'go/trade/etc/trade.yaml')
        'player-inbox'   = (& $player 'go/player_inbox/etc/player_inbox.yaml')
        mail             = (& $player 'go/mail/etc/mail.yaml')
    }
    Assert-Equal -Expected (@($expected.Keys | Sort-Object) -join ',') -Actual (@($inputs.Clients | ForEach-Object { $_.Name } | Sort-Object) -join ',') -Because '连玩家域的服务名单;新增一个连玩家域的服务 = 总账加一行'
    foreach ($client in $inputs.Clients) {
        $perReplica = [int]$client.ConfiguredPoolConns + [int]$client.Fixed
        Assert-Equal -Expected $expected[$client.Name] -Actual $perReplica -Because "$($client.Name) 每副本每节点的连接数"
        Assert-Equal -Expected 0 -Actual ([int]$client.DefaultPools + [int]$client.PubSub) -Because "$($client.Name) 在玩家域没有缺省池和 Pub/Sub"
        $expectedScope = if ($client.Name -in $ZoneServices) { 'zone' } else { 'global' }
        Assert-Equal -Expected $expectedScope -Actual $client.Scope -Because "$($client.Name) 的 Scope"
    }
}

Test-Case '控制集群总账输入:缺口第 34 条第 6b 批接上服务之前没有客户端(返回空表,写路径只打一行说明);maxclients 同一个模板' {
    $inputs = Get-RedisClusterConnectionLedgerInputs -Cluster $ControlCluster
    Assert-Equal -Expected 0 -Actual @($inputs.Clients).Count -Because '还没有服务连控制集群;接上一个服务 = 这里加一行,并删掉本用例'
    Assert-Equal -Expected 40000 -Actual $inputs.MaxClients -Because '读 redis-cluster.yaml 的 MAXCLIENTS'
    Assert-Match -Text (Get-ThrownMessage { Get-RedisConnectionLedger -Inputs $inputs -ZoneCount 1 -CppZoneProcesses 0 -BattleProcesses 0 -RolloutExtraPodsPerDeployment 0 -OpsReserve 0 -WarnRatio 0.8 }) `
        -Pattern '输入 Clients 为空' -Because '空表不能算出 0 条连接冒充结论,所以写路径对空表只打说明行(Write-RedisClusterConnectionLedger)'
    $body = ($deployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Write-RedisClusterConnectionLedger' }, $true)).Body.Extent.Text
    Assert-Match -Text $body -Pattern 'if \(@\(\$inputs\.Clients\)\.Count -eq 0\)' -Because '空表先拦下,再调总账函数'
    Assert-Match -Text $body -Pattern 'no clients wired yet' -Because '说明行'
}

Test-Case '玩家域总账算式:每节点 = Z × Σ(zone 服务) + Σ(全局服务) + scene 进程数;gate、battle 不计' {
    $inputs = Get-RedisClusterConnectionLedgerInputs -Cluster $PlayerCluster
    $perZone = 0
    $globalClients = 0
    foreach ($client in $inputs.Clients) {
        $total = [int]$client.Replicas * ([int]$client.ConfiguredPoolConns + [int]$client.Fixed)
        if ($client.Scope -eq 'zone') { $perZone += $total } else { $globalClients += $total }
    }
    $ledger = Get-RedisConnectionLedger -Inputs $inputs -ZoneCount 3 -CppZoneProcesses 12 -BattleProcesses 0 -RolloutExtraPodsPerDeployment 0 -OpsReserve 0 -WarnRatio 0.8 -CppSurgeWorkloads 1
    Assert-Equal -Expected (3 * $perZone + $globalClients + 12) -Actual $ledger.Steady -Because '公式见 docs/design/redis-player-store-cluster.md §6.1'
    Assert-Equal -Expected 32000 -Actual $ledger.WarnAt -Because '80% 水位,与核心总账同一纪律'
}

Test-Case '控制域总账:没建集群时这些池记在控制域;建了集群就搬走,只剩玩家域句柄的 mail / match 不留行;chat 不连 Redis,两份都没有它' {
    $unsplit = Get-RedisConnectionLedgerInputs
    $split = Get-RedisConnectionLedgerInputs -PlayerDomainSplit $true
    # 行是字典:脚本块返回时单行会被展开,字典的 .Count 和 [0] 都不是想要的,调用处要包 @()。
    $rowOf = { param($Inputs, $Name) $Inputs.Clients | Where-Object { $_.Name -eq $Name } }
    # K8s 的 match 私有池始终指向 redis-match-cluster;身份回退删除后,它与 mail 在核心上都只剩玩家域池。
    foreach ($service in @('mail', 'match')) {
        $unsplitRows = @(& $rowOf $unsplit $service)
        Assert-Equal -Expected 1 -Actual $unsplitRows.Count -Because "没建集群时 $service 的玩家域句柄连这台 Redis"
        $source = if ($service -eq 'match') { 'go/match/etc/match_service.yaml' } else { 'go/mail/etc/mail.yaml' }
        $playerPool = [int](Get-EtcValue -RelativePath $source -KeyPath 'PlayerRedis.PoolSizePerNode')
        Assert-Equal -Expected $playerPool -Actual $unsplitRows[0].ConfiguredPoolConns -Because "$service 在核心上的显式连接只有玩家域池"
        Assert-Equal -Expected '0/0/0/0' -Actual ('{0}/{1}/{2}/{3}' -f $unsplitRows[0].DefaultPools, $unsplitRows[0].GoMaxProcs, $unsplitRows[0].PubSub, $unsplitRows[0].Fixed) `
            -Because "$service 不再有独立的控制域池或订阅,不能沿用删除登录身份回退前的连接数"
        Assert-Equal -Expected 0 -Actual @(& $rowOf $split $service).Count -Because "建了集群 $service 不再连这台 Redis,留 0 条的行总账函数会拒绝"
    }
    foreach ($inputs in @($unsplit, $split)) {
        Assert-Equal -Expected 0 -Actual @(& $rowOf $inputs 'chat').Count -Because 'chat 只认 gate 签名的会话身份原件、聊天权威在 SQL,不连 Redis'
    }
    $loginPool = [int](Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.RedisClient.PoolSize')
    Assert-Equal -Expected $loginPool -Actual @(& $rowOf $split 'login')[0].ConfiguredPoolConns -Because '建了集群 login 只剩 Node.RedisClient'
    $smSplit = @(& $rowOf $split 'scene-manager')[0]
    Assert-Equal -Expected '0/0/1' -Actual ('{0}/{1}/{2}' -f $smSplit.ConfiguredPoolConns, $smSplit.Fixed, $smSplit.DefaultPools) -Because '铸造通道搬到玩家域,控制域只剩 go-zero 缺省池'
    $guildSplit = @(& $rowOf $split 'guild')[0]
    $guildControl = [int](Get-EtcValue -RelativePath 'go/guild/etc/guild.yaml' -KeyPath 'RedisClient.PoolSize') + [int](Get-EtcValue -RelativePath 'go/guild/etc/guild.yaml' -KeyPath 'PlayerLocatorRedis.PoolSize')
    # 入帮申请配额专用池(go/guild/internal/logic/application_quota.go 的 ApplicationQuotaRedisPoolSize)连核心库,拆了玩家域也留下。
    $quotaSource = [IO.File]::ReadAllText((Join-Path $RepoRoot 'go/guild/internal/logic/application_quota.go'))
    $quotaPool = [int][regex]::Match($quotaSource, '(?m)^\s*(?:const\s+)?ApplicationQuotaRedisPoolSize\s*=\s*(\d+)\s*$').Groups[1].Value
    Assert-True -Condition ($quotaPool -gt 0) -Because "写死的配额池上限要从源码读到(实际 $quotaPool),写法变了同步本用例"
    Assert-Equal -Expected "$guildControl/$quotaPool" -Actual ('{0}/{1}' -f $guildSplit.ConfiguredPoolConns, $guildSplit.Fixed) -Because 'guild 的两个控制域池与申请配额池留下,玩家域句柄、摘要池搬走'
    $plan = @{ ZoneCount = 1; CppZoneProcesses = 6; BattleProcesses = 1; RolloutExtraPodsPerDeployment = 2; OpsReserve = 50; WarnRatio = 0.8 }
    Assert-True -Condition ((Get-RedisConnectionLedger -Inputs $split @plan).Steady -lt (Get-RedisConnectionLedger -Inputs $unsplit @plan).Steady) -Because '搬走之后控制域的账变小,而且没有 0 条的行让总账函数拒绝'
}

Test-Case '负向:PlayerRedis.PoolSizePerNode 不是正整数时 throw 并点名;登录域池只有 login 有' {
    $root = Join-Path $TempRoot 'pool-mirror'
    $relative = 'go/player_locator/etc/player_locator.yaml'
    $target = Join-Path $root $relative
    $null = New-Item -ItemType Directory -Force -Path (Split-Path -Parent $target)
    $source = [IO.File]::ReadAllText((Join-Path $RepoRoot $relative))
    $broken = [regex]::Replace($source, '(?m)^(PlayerRedis:\r?\n  PoolSizePerNode:)[ ]*\d+', '$1 0')
    Assert-True -Condition ($broken -cne $source) -Because '夹具前提:player_locator.yaml 的 PlayerRedis 段第一行是 PoolSizePerNode'
    [IO.File]::WriteAllText($target, $broken)
    $saved = $RepoRoot
    try {
        $RepoRoot = $root
        $message = Get-ThrownMessage { Get-RedisPlayerPoolMirror -Target 'player-locator' }
    }
    finally { $RepoRoot = $saved }
    Assert-Match -Text $message -Pattern 'Redis 连接池镜像' -Because 'fail-closed:每节点池上限决定玩家域每个主节点的连接数'
    Assert-Match -Text $message -Pattern ([regex]::Escape("$relative 的 PlayerRedis.PoolSizePerNode='0'")) -Because '点名文件、键路径与值'
    Assert-Equal -Expected ([int](Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath 'LoginRedis.PoolSizePerNode')) -Actual (Get-RedisPlayerPoolMirror -Target 'login').Login -Because 'login 自己的登录域池镜像服务 yaml'
    foreach ($service in @('match', 'friend', 'guild', 'trade', 'mail')) {
        Assert-Equal -Expected 0 -Actual (Get-RedisPlayerPoolMirror -Target $service).Login -Because "$service 只认 gate 签名的会话身份原件,没有登录域池"
    }
    Assert-True -Condition ((Get-ThrownMessage { Get-RedisPlayerPoolMirror -Target 'chat' }) -ne '') -Because 'chat 不连 Redis,不是玩家域镜像的目标'
}

# ─────────────────────────────────────────────────────────────────
# 4. DryRun
# ─────────────────────────────────────────────────────────────────

Test-Case 'dev 缺省 zone-up:产物没有 redis-player,node-config 没有 player_redis,Go 服务的 PlayerRedis 只写池上限' {
    Assert-Equal -Expected 0 -Actual $devZoneRun.ExitCode -Because "dev 缺省 zone-up DryRun 必须成功。输出: $($devZoneRun.Output)"
    $out = $devZoneRun.Output
    Assert-NotMatch -Text $out -Pattern $ClusterObjectPattern -Because 'dev 缺省不建集群,任何产物都不引用它'
    Assert-NotMatch -Text $out -Pattern 'PLAYER_REDIS_PASSWORD' -Because '没开集群不注入口令'
    Assert-NotMatch -Text $out -Pattern 'Redis player connection ledger' -Because '没开集群不打玩家域总账'
    $node = Get-ConfigFlat -Output $out -ConfigMap 'node-config'
    Assert-Equal -Expected 0 -Actual @($node.Scalars.Keys | Where-Object { $_ -like 'data.game_config.yaml.player_redis*' }).Count -Because '整段缺失 = standalone,沿用 zoneredis'
    foreach ($service in $ZoneServices) {
        $flat = Get-ConfigFlat -Output $out -ConfigMap "go-svc-$service-config"
        $prefix = $ServiceConfigs[$service] + 'PlayerRedis.'
        Assert-True -Condition ($flat.Scalars.Contains($prefix + 'PoolSizePerNode')) -Because "$service 要写池上限(总账读同一个键)"
        Assert-True -Condition (-not $flat.Scalars.Contains($prefix + 'Addrs[0]')) -Because "$service 没开集群时不写地址,由 OrDefault 指向现有实例"
    }
}

Test-Case 'dev 缺省 infra-up:不渲染集群;只认签名会话身份的六个全局服务不渲染 GlobalLoginSessions / LoginRedis;trade 的 Rmb 段镜像服务 yaml' {
    Assert-Equal -Expected 0 -Actual $devInfraRun.ExitCode -Because "dev 缺省 infra-up DryRun 必须成功。输出: $($devInfraRun.Output)"
    $out = $devInfraRun.Output
    Assert-NotMatch -Text $out -Pattern $ClusterObjectPattern -Because '没开集群不 apply 也不删'
    foreach ($service in $SignedIdentityServices) {
        $leftover = @(Get-LoginSessionKeys -Flat (Get-ConfigFlat -Output $out -ConfigMap "go-svc-$service-config") -Prefix $ServiceConfigs[$service])
        Assert-Equal -Expected 0 -Actual $leftover.Count -Because "$service 只认 gate 签名的会话身份原件,不该渲染登录态段(残留:$($leftover -join ', '))"
    }
    $trade = Get-ConfigFlat -Output $out -ConfigMap 'go-svc-trade-config'
    Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/trade/etc/trade.yaml' -KeyPath 'Rmb.Enabled') -Actual (Get-FlatValue -Flat $trade -KeyPath 'data.trade.yaml.Rmb.Enabled') -Because 'Rmb 段整段镜像服务 yaml'
}

Test-Case '-PlayerRedisMasters 3 的 infra-up:清单、Secret、建群 Job 都渲染,排在全局 Go 服务之前,占位全部替换,口令不进产物' {
    Assert-Equal -Expected 0 -Actual $clusterInfraRun.ExitCode -Because "infra-up DryRun 必须成功。输出: $($clusterInfraRun.Output)"
    $out = $clusterInfraRun.Output
    Assert-Match -Text $out -Pattern 'Redis player cluster: masters=3 replicas_per_master=1 nodes=6 maxmemory=256mb memory=512Mi storage=2Gi min_replicas_to_write=0' -Because '入口一行把拓扑和容量打出来'
    Assert-Match -Text $out -Pattern ("\[dry-run\] kubectl apply -n $Infra -f -\s+# Secret redis-player-auth \(key password; value hidden\)") -Because 'infra namespace 的 Secret 先于清单'
    Assert-Match -Text $out -Pattern ("delete job redis-player-cluster-init -n $Infra --ignore-not-found") -Because 'Job 的 template 不可变,重跑先删'
    $block = Get-WorkloadBlock -Output $out -Kind 'StatefulSet' -Name 'redis-player'
    Assert-Match -Text $block -Pattern '(?m)^  replicas: 6\s*$' -Because '3 主每主 1 副本 = 6 个节点'
    Assert-NotMatch -Text $block -Pattern '__[A-Z][A-Z0-9_]*__' -Because '占位全部替换'
    $perPod = @([regex]::Matches($block, '(?m)^  name: redis-player-(\d+)\s*$') | ForEach-Object { [int]$_.Groups[1].Value } | Sort-Object)
    Assert-Equal -Expected '0,1,2,3,4,5' -Actual ($perPod -join ',') -Because '每个节点一个 Service'
    Assert-Match -Text $block -Pattern '(?m)^  name: redis-player-cluster-init\s*$' -Because '建群 Job'
    Assert-True -Condition ($out.IndexOf('name: redis-player-cluster-config') -lt $out.IndexOf('name: go-svc-friend-config')) -Because '全局 Go 服务连集群,集群清单排在前面'
    Assert-Match -Text $out -Pattern 'Redis player connection ledger: per_master zones=\d+ scene_processes=\d+ steady=\d+ peak=\d+ maxclients=40000 warn_at=32000 ' -Because '写路径入口打印玩家域总账'
    Assert-NotMatch -Text $out -Pattern '\[RedisPlayerConnectionLedger\]OverWarnRatio:' -Because '仓库的规划档在 80% 水位以内'
    Assert-NotMatch -Text $out -Pattern ([regex]::Escape($ClusterPassword)) -Because '口令不进任何产物与打印'
}

Test-Case '-PlayerRedisMasters 3 的 infra-up:全局服务的 PlayerRedis 写种子与 PasswordEnv,容器有口令环境变量;登录态段不渲染,chat 不拿口令' {
    $out = $clusterInfraRun.Output
    foreach ($service in $GlobalServices) {
        $flat = Get-ConfigFlat -Output $out -ConfigMap "go-svc-$service-config"
        $prefix = $ServiceConfigs[$service] + 'PlayerRedis.'
        foreach ($i in 0..2) {
            Assert-Equal -Expected $Seeds[$i] -Actual (Get-FlatValue -Flat $flat -KeyPath ($prefix + "Addrs[$i]")) -Because "$service 的 PlayerRedis 种子 $i"
        }
        Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $flat -KeyPath ($prefix + 'Cluster')) -Because "$service 的 PlayerRedis"
        Assert-Equal -Expected 'PLAYER_REDIS_PASSWORD' -Actual (Get-FlatValue -Flat $flat -KeyPath ($prefix + 'PasswordEnv')) -Because "$service 的 PlayerRedis 只写变量名"
        Assert-True -Condition (-not $flat.Scalars.Contains($prefix + 'Password')) -Because "$service 的 PlayerRedis 不写口令值"
        $workload = Get-WorkloadBlock -Output $out -Kind (Get-ServiceWorkloadKind -Name $service) -Name $service
        Assert-Match -Text $workload -Pattern '- name: PLAYER_REDIS_PASSWORD\s+valueFrom:\s+secretKeyRef:\s+name: redis-player-auth\s+key: password' -Because "$service 容器要有口令:没有的话只有一屏 NOAUTH"
    }
    foreach ($service in $SignedIdentityServices) {
        $leftover = @(Get-LoginSessionKeys -Flat (Get-ConfigFlat -Output $out -ConfigMap "go-svc-$service-config") -Prefix $ServiceConfigs[$service])
        Assert-Equal -Expected 0 -Actual $leftover.Count -Because "$service 开了集群也不渲染登录态段(残留:$($leftover -join ', '))"
    }
    $chatFlat = Get-ConfigFlat -Output $out -ConfigMap 'go-svc-chat-config'
    Assert-Equal -Expected 0 -Actual @($chatFlat.Scalars.Keys | Where-Object { ([string]$_).StartsWith($ServiceConfigs['chat'] + 'PlayerRedis', [System.StringComparison]::Ordinal) }).Count -Because 'chat 没有玩家域段'
    Assert-NotMatch -Text (Get-WorkloadBlock -Output $out -Kind (Get-ServiceWorkloadKind -Name 'chat') -Name 'chat') -Pattern 'PLAYER_REDIS_PASSWORD' -Because 'chat 不连 Redis,容器不拿玩家域口令'
}

Test-Case '-PlayerRedisMasters 3 的 zone-up:node-config 写 player_redis,scene 有口令与配置指纹,gate 没有;zone 的 Go 服务写种子;WAITAOF 取 1' {
    Assert-Equal -Expected 0 -Actual $clusterZoneRun.ExitCode -Because "zone-up DryRun 必须成功。输出: $($clusterZoneRun.Output)"
    $out = $clusterZoneRun.Output
    $node = Get-ConfigFlat -Output $out -ConfigMap 'node-config'
    Assert-Equal -Expected 'cluster' -Actual (Get-FlatValue -Flat $node -KeyPath 'data.game_config.yaml.player_redis.mode') -Because 'mode'
    foreach ($i in 0..2) {
        Assert-Equal -Expected $Seeds[$i] -Actual (Get-FlatValue -Flat $node -KeyPath "data.game_config.yaml.player_redis.seeds[$i]") -Because "种子 $i"
    }
    Assert-True -Condition (-not $node.Scalars.Contains('data.game_config.yaml.player_redis.password')) -Because '口令经 Secret,不进 ConfigMap'
    Assert-Match -Text $out -Pattern 'Redis player cluster: masters=3 seeds=' -Because 'zone-up 只渲染客户端,打一行说明'
    Assert-Match -Text $out -Pattern 'kubectl apply -n mmorpg-zone-cluster-test -f -\s+# Secret redis-player-auth' -Because 'Secret 不能跨 namespace 引用,zone 里也要一份'
    $scene = Get-WorkloadBlock -Output $out -Kind 'Deployment' -Name 'scene'
    Assert-Match -Text $scene -Pattern 'name: PLAYER_REDIS_PASSWORD\s+valueFrom:\s+secretKeyRef: \{name: redis-player-auth, key: password, optional: false\}' -Because 'scene 经 Secret 拿口令'
    Assert-Match -Text $scene -Pattern 'mmorpg\.io/player-redis-config-hash: "[0-9a-f]{12}"' -Because 'C++ 只在启动时读,种子或口令变了要靠指纹滚动'
    $gate = Get-WorkloadBlock -Output $out -Kind 'Deployment' -Name 'gate'
    Assert-NotMatch -Text $gate -Pattern 'PLAYER_REDIS_PASSWORD|player-redis-config-hash' -Because 'gate 不连玩家域'
    foreach ($service in $ZoneServices) {
        $flat = Get-ConfigFlat -Output $out -ConfigMap "go-svc-$service-config"
        Assert-Equal -Expected $Seeds[0] -Actual (Get-FlatValue -Flat $flat -KeyPath ($ServiceConfigs[$service] + 'PlayerRedis.Addrs[0]')) -Because "$service 的种子"
        Assert-Equal -Expected 'PLAYER_REDIS_PASSWORD' -Actual (Get-FlatValue -Flat $flat -KeyPath ($ServiceConfigs[$service] + 'PlayerRedis.PasswordEnv')) -Because "$service 只写变量名"
        Assert-Match -Text (Get-WorkloadBlock -Output $out -Kind (Get-ServiceWorkloadKind -Name $service) -Name $service) -Pattern '- name: PLAYER_REDIS_PASSWORD\s+valueFrom:' -Because "$service 容器要有口令"
    }
    $sceneManager = Get-ConfigFlat -Output $out -ConfigMap 'go-svc-scene-manager-config'
    Assert-Equal -Expected '1' -Actual (Get-FlatValue -Flat $sceneManager -KeyPath 'data.scene_manager_service.yaml.OwnerEpochMint.WaitAOFNumReplicas') -Because '铸造的键在集群上,每主至少 1 副本'
    Assert-NotMatch -Text $out -Pattern ([regex]::Escape($ClusterPassword)) -Because '口令不进任何产物与打印'
}

Test-Case 'dev 档集群不注入口令:Go 不写 PasswordEnv,容器没有口令环境变量;集群无口令启动' {
    $run = Invoke-ClusterDryRun -Arguments ($ZoneArgs + @('-PlayerRedisMasters', '3'))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "dev 的集群口令可选。输出: $($run.Output)"
    Assert-NotMatch -Text $run.Output -Pattern 'PLAYER_REDIS_PASSWORD' -Because '没有口令就没有环境变量,也不写 PasswordEnv'
    $flat = Get-ConfigFlat -Output $run.Output -ConfigMap 'go-svc-login-config'
    Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.PlayerRedis.Cluster') -Because '集群照开'
    Assert-Match -Text $run.Output -Pattern 'delete secret redis-player-auth' -Because '口令撤掉时删掉残留,否则下一次重启还会带着旧口令'
}

Test-Case '每主 2 副本:节点数 ×1.5,min-replicas-to-write 取 1' {
    $run = Invoke-ClusterDryRun -Arguments ($InfraArgs + @('-PlayerRedisMasters', '4', '-PlayerRedisReplicasPerMaster', '2', '-PlayerRedisMaxMemory', '2gb', '-PlayerRedisStorage', '16Gi')) -Env $ClusterInfraEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'Redis player cluster: masters=4 replicas_per_master=2 nodes=12 maxmemory=2gb memory=4Gi storage=16Gi min_replicas_to_write=1' -Because '入口一行'
    Assert-Match -Text (Get-WorkloadBlock -Output $run.Output -Kind 'StatefulSet' -Name 'redis-player') -Pattern '(?m)^  replicas: 12\s*$' -Because '4 × 3'
}

Test-Case '加主节点不增加每节点连接:-PlayerRedisMasters 3 与 6 的 steady 相同' {
    $run = Invoke-ClusterDryRun -Arguments ($InfraArgs + @('-PlayerRedisMasters', '6')) -Env $ClusterInfraEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    $steady = { param($Text) [regex]::Match($Text, 'Redis player connection ledger: per_master zones=\d+ scene_processes=\d+ steady=(\d+) ').Groups[1].Value }
    $three = & $steady $clusterInfraRun.Output
    Assert-True -Condition ($three -match '^\d+$') -Because '总账行要能读到 steady'
    Assert-Equal -Expected $three -Actual (& $steady $run.Output) -Because '每节点连接只取决于客户端进程数,加主节点没有用(§6.3)'
}

Test-Case '-ControlRedisMasters 3 的 infra-up:控制集群的清单、Secret、建群 Job 都渲染,占位全部替换;只开它时玩家域不建;口令不进产物' {
    Assert-Equal -Expected 0 -Actual $controlInfraRun.ExitCode -Because "infra-up DryRun 必须成功。输出: $($controlInfraRun.Output)"
    $out = $controlInfraRun.Output
    Assert-Match -Text $out -Pattern 'Redis control cluster: masters=3 replicas_per_master=1 nodes=6 maxmemory=256mb memory=512Mi storage=2Gi min_replicas_to_write=0 \(manifests/infra/redis-cluster\.yaml, name=redis-control\)' `
        -Because '入口一行把拓扑和容量打出来,点名是哪套集群'
    Assert-Match -Text $out -Pattern ("\[dry-run\] kubectl apply -n $Infra -f -\s+# Secret redis-control-auth \(key password; value hidden\)") -Because 'infra namespace 的 Secret 先于清单'
    Assert-Match -Text $out -Pattern ("delete job redis-control-cluster-init -n $Infra --ignore-not-found") -Because 'Job 的 template 不可变,重跑先删'
    $block = Get-WorkloadBlock -Output $out -Kind 'StatefulSet' -Name 'redis-control'
    Assert-Match -Text $block -Pattern '(?m)^  replicas: 6\s*$' -Because '3 主每主 1 副本 = 6 个节点'
    Assert-NotMatch -Text $block -Pattern '__[A-Z][A-Z0-9_]*__' -Because '占位全部替换'
    $perPod = @([regex]::Matches($block, '(?m)^  name: redis-control-(\d+)\s*$') | ForEach-Object { [int]$_.Groups[1].Value } | Sort-Object)
    Assert-Equal -Expected '0,1,2,3,4,5' -Actual ($perPod -join ',') -Because '每个节点一个 Service'
    Assert-Match -Text $out -Pattern 'Redis control connection ledger: per_master ' -Because '写路径入口打印控制集群的总账一行'
    Assert-NotMatch -Text $out -Pattern $ClusterObjectPattern -Because '只开控制集群时玩家域照旧不建'
    Assert-NotMatch -Text $out -Pattern 'Redis player (cluster|connection ledger):' -Because '只开控制集群时不打玩家域那两行'
    Assert-NotMatch -Text $out -Pattern ([regex]::Escape($ControlClusterPassword)) -Because '口令不进任何产物与打印'
}

Test-Case '-ControlRedisMasters 3 的 zone-up:不 apply 集群,只打一行种子说明;dev 缺省 zone-up 与 infra-up 都不引用控制集群' {
    Assert-Equal -Expected 0 -Actual $controlZoneRun.ExitCode -Because "zone-up DryRun 必须成功。输出: $($controlZoneRun.Output)"
    Assert-Match -Text $controlZoneRun.Output -Pattern ('Redis control cluster: masters=3 seeds=' + [regex]::Escape($ControlSeeds -join ',') + ' \(this run only renders clients') `
        -Because 'zone-up 只渲染客户端;种子固定序号 0、1、2'
    $clusterBlocks = @(Get-ManifestBlocks -Output $controlZoneRun.Output | Where-Object { $_ -cmatch '(?m)^kind: StatefulSet\s*$' -and $_ -cmatch '(?m)^  name: redis-control\s*$' })
    Assert-Equal -Expected 0 -Actual $clusterBlocks.Count -Because 'zone-up 不 apply 集群'
    Assert-NotMatch -Text $controlZoneRun.Output -Pattern 'delete job redis-control-cluster-init' -Because 'zone-up 不动建群 Job'
    Assert-NotMatch -Text $controlZoneRun.Output -Pattern ([regex]::Escape($ControlClusterPassword)) -Because '口令不进任何产物与打印'
    foreach ($run in @($devZoneRun, $devInfraRun)) {
        Assert-NotMatch -Text $run.Output -Pattern $ControlClusterObjectPattern -Because 'dev 缺省不建控制集群,任何产物都不引用它'
        Assert-NotMatch -Text $run.Output -Pattern 'Redis control (cluster|connection ledger):' -Because 'dev 缺省不打控制集群那两行'
    }
}

Test-Case '两套都开的 infra-up:各一套对象互不重名,玩家域先 apply,两套都排在全局 Go 服务之前;两份总账各一行' {
    Assert-Equal -Expected 0 -Actual $bothInfraRun.ExitCode -Because "infra-up DryRun 必须成功。输出: $($bothInfraRun.Output)"
    $out = $bothInfraRun.Output
    foreach ($name in @('redis-player', 'redis-control')) {
        $null = Get-WorkloadBlock -Output $out -Kind 'StatefulSet' -Name $name
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($out, ('(?m)^  name: ' + [regex]::Escape("$name-cluster-config") + '\s*$')).Count) -Because "$name 的 ConfigMap 恰好一份"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($out, ('# Secret ' + [regex]::Escape("$name-auth") + ' \(key password; value hidden\)')).Count) -Because "$name 的口令 Secret 在 infra namespace 恰好同步一次"
    }
    $playerAt = $out.IndexOf('name: redis-player-cluster-config')
    $controlAt = $out.IndexOf('name: redis-control-cluster-config')
    $friendAt = $out.IndexOf('name: go-svc-friend-config')
    Assert-True -Condition ($playerAt -ge 0 -and $controlAt -gt $playerAt) -Because '按 Get-RedisClusters 的顺序 apply:玩家域在前'
    Assert-True -Condition ($friendAt -gt $controlAt) -Because '全局 Go 服务连这些集群,两套集群的清单都排在它们前面'
    Assert-Match -Text $out -Pattern 'Redis player connection ledger: per_master zones=' -Because '玩家域总账一行'
    Assert-Match -Text $out -Pattern 'Redis control connection ledger: per_master ' -Because '控制集群总账一行'
    foreach ($password in @($ClusterPassword, $ControlClusterPassword)) {
        Assert-NotMatch -Text $out -Pattern ([regex]::Escape($password)) -Because '口令不进任何产物与打印'
    }
}

foreach ($case in @(
        @{ Label = 'staging 没给 -PlayerRedisMasters'; Args = ($ProdZoneArgs -replace '^prod$', 'staging') + @('-PlayerRedisMasters', '0'); Pattern = '-ReleaseProfile staging 必须显式给 -PlayerRedisMasters' }
        @{ Label = 'prod 没给 -PlayerRedisMasters'; Args = $ProdInfraArgs + @('-PlayerRedisMasters', '0'); Pattern = '-ReleaseProfile prod 必须显式给 -PlayerRedisMasters' }
        @{ Label = 'prod 只给 2 个主'; Args = $ProdZoneArgs + @('-PlayerRedisMasters', '2'); Pattern = '-PlayerRedisMasters 2 不合法' }
        @{ Label = 'prod 的 infra-up 缺容量'; Args = $ProdInfraArgs + @('-PlayerRedisMasters', '3'); Pattern = '必须显式给 -PlayerRedisMaxMemory' }
        # 控制集群同一套闸(Add-PlayerRedisDeployFixture 给另一套补齐参数,红绿只由本集群的闸决定)。
        @{ Label = 'staging 没给 -ControlRedisMasters'; Args = ($ProdZoneArgs -replace '^prod$', 'staging') + @('-ControlRedisMasters', '0'); Pattern = '-ReleaseProfile staging 必须显式给 -ControlRedisMasters' }
        @{ Label = 'prod 没给 -ControlRedisMasters'; Args = $ProdInfraArgs + @('-ControlRedisMasters', '0'); Pattern = '-ReleaseProfile prod 必须显式给 -ControlRedisMasters' }
        @{ Label = 'prod 只给 2 个控制主'; Args = $ProdZoneArgs + @('-ControlRedisMasters', '2'); Pattern = '-ControlRedisMasters 2 不合法' }
        @{ Label = 'prod 的 infra-up 缺控制集群容量'; Args = $ProdInfraArgs + @('-ControlRedisMasters', '3'); Pattern = '必须显式给 -ControlRedisMaxMemory' }
    )) {
    Test-Case "负向:$($case.Label),在任何写操作之前拒绝" {
        $run = Invoke-ClusterDryRun -Arguments $case.Args -Env $ProdEnv
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "必须拒绝。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern $case.Pattern -Because '错误写明原因'
        Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '拓扑判定先于任何写操作'
    }
}

Test-Case '负向:prod 开了集群缺 MMORPG_PLAYER_REDIS_PASSWORD,在任何写操作之前拒绝并点名变量;给了就能出产物' {
    $missing = Invoke-ClusterDryRun -Arguments ($ProdZoneArgs + @('-PlayerRedisMasters', '3')) -Env (@{} + $ProdEnv + @{ MMORPG_PLAYER_REDIS_PASSWORD = '' })
    Assert-True -Condition ($missing.ExitCode -ne 0) -Because "必须拒绝。输出: $($missing.Output)"
    Assert-Match -Text $missing.Output -Pattern 'ReleaseProfile=prod 要求从环境变量 MMORPG_PLAYER_REDIS_PASSWORD' -Because '点名缺哪个环境变量'
    Assert-NotMatch -Text $missing.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '密钥解析先于任何写操作'
    $given = Invoke-ClusterDryRun -Arguments ($ProdZoneArgs + @('-PlayerRedisMasters', '3')) -Env (@{} + $ProdEnv + $ClusterEnv)
    Assert-Equal -Expected 0 -Actual $given.ExitCode -Because "注入齐全时不该失败。输出: $($given.Output)"
    Assert-Match -Text (Get-WorkloadBlock -Output $given.Output -Kind 'Deployment' -Name 'scene') -Pattern 'name: PLAYER_REDIS_PASSWORD' -Because 'prod 的 scene 带口令'
    Assert-NotMatch -Text $given.Output -Pattern ([regex]::Escape($ClusterPassword)) -Because '口令不进任何产物与打印'
}

Test-Case '负向:prod 开了控制集群缺 MMORPG_CONTROL_REDIS_PASSWORD,在任何写操作之前拒绝并点名变量' {
    $missing = Invoke-ClusterDryRun -Arguments ($ProdZoneArgs + @('-ControlRedisMasters', '3')) -Env (@{} + $ProdEnv + @{ MMORPG_CONTROL_REDIS_PASSWORD = '' })
    Assert-True -Condition ($missing.ExitCode -ne 0) -Because "必须拒绝。输出: $($missing.Output)"
    Assert-Match -Text $missing.Output -Pattern 'ReleaseProfile=prod 要求从环境变量 MMORPG_CONTROL_REDIS_PASSWORD' -Because '点名缺哪个环境变量'
    Assert-NotMatch -Text $missing.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '密钥解析先于任何写操作'
}

Test-Case '写路径门禁(AST):两套集群的拓扑预检排在密钥解析与连接总账之前;控制域总账按拓扑算' {
    $gates = @($deployAst.EndBlock.Statements | Where-Object { $_ -is [System.Management.Automation.Language.IfStatementAst] -and $_.Clauses[0].Item1.Extent.Text -match '^\$Command\s+-in\s+@\(' -and $_.Extent.Text -match 'Assert-RedisClusterPreflight' })
    Assert-Equal -Expected 1 -Actual $gates.Count -Because '夹具前提:恰好一个写路径门禁块调它'
    Assert-Match -Text $gates[0].Extent.Text -Pattern 'foreach \(\$redisCluster in Get-RedisClusters\) \{ Assert-RedisClusterPreflight -Cluster \$redisCluster \}' -Because '两套集群都过闸,不能只查一套'
    $calls = @($gates[0].FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))
    $firstOf = { param([string]$Name) @($calls | Where-Object { $_.GetCommandName() -eq $Name } | Sort-Object { $_.Extent.StartOffset })[0].Extent.StartOffset }
    Assert-True -Condition ((& $firstOf 'Assert-RedisClusterPreflight') -lt (& $firstOf 'Initialize-InjectedSecrets')) -Because '报的是拓扑原因而不是缺环境变量'
    Assert-True -Condition ((& $firstOf 'Initialize-InjectedSecrets') -lt (& $firstOf 'Write-RedisConnectionLedger')) -Because '总账按解析后的拓扑记'
}

# ─────────────────────────────────────────────────────────────────
# 5. 告警
# ─────────────────────────────────────────────────────────────────

Test-Case 'redis-player-alerts.yaml:PrometheusRule 形状、单独的组、规则名单钉死;只看本集群,没有玩家维度 label;没有并进控制域的组' {
    $text = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/k8s/redis-player-alerts.yaml')) -replace "`r`n", "`n"
    Assert-Match -Text $text -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '安装方式与 redis-alerts.yaml 相同'
    $groups = @([regex]::Matches($text, '(?m)^    - name:[ ]*(\S+)[ ]*$') | ForEach-Object { $_.Groups[1].Value })
    Assert-Equal -Expected 'redis.player-domain' -Actual ($groups -join ',') -Because '单独一组'
    $names = @([regex]::Matches($text, '(?m)^        - alert:[ ]*(\S+)[ ]*$') | ForEach-Object { $_.Groups[1].Value } | Sort-Object)
    $expected = @('RedisPlayerNodeDown', 'RedisPlayerClusterStateNotOk', 'RedisPlayerSlotsNotCovered', 'RedisPlayerNodesDisagree', 'RedisPlayerReplicaLinkDown',
        'RedisPlayerMemoryNearMax', 'RedisPlayerOomRejected', 'RedisPlayerConnectionsRejected', 'RedisPlayerConnectionsNearMax', 'RedisPlayerAofWriteFailed' | Sort-Object)
    Assert-Equal -Expected ($expected -join ',') -Actual ($names -join ',') -Because '节点宕机、cluster_state、slot 覆盖、内存、连接数、AOF 都要有'
    $rules = @(($text -split '(?m)^        - alert:') | Select-Object -Skip 1)
    foreach ($rule in $rules) {
        $ruleName = ($rule -split "`n")[0].Trim()
        Assert-Match -Text $rule -Pattern '(?m)^            severity:[ ]*(critical|warning)[ ]*$' -Because "$ruleName 要有 severity"
        Assert-Match -Text $rule -Pattern '(?m)^            summary:' -Because "$ruleName 要有 summary"
        Assert-Match -Text $rule -Pattern '(?m)^            description:' -Because "$ruleName 要有 description"
        Assert-Match -Text $rule -Pattern '(?m)^          for:[ ]*\d+[ms][ ]*$' -Because "$ruleName 要写 for"
        Assert-Match -Text $rule -Pattern 'pod=~"redis-player-\[0-9\]\+"' -Because "$ruleName 只看本集群,不能把控制域的节点算进来"
    }
    Assert-Match -Text ($rules | Where-Object { $_ -match '^\s*RedisPlayerSlotsNotCovered\b' }) -Pattern 'redis_cluster_slots_ok' -Because 'full-coverage no 时一片全挂 cluster_state 还是 ok,要单独盯 slot'
    Assert-NotMatch -Text $text -Pattern 'player_id' -Because '不用玩家维度 label(AGENTS §9)'
    $control = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/k8s/redis-alerts.yaml'))
    Assert-Match -Text $control -Pattern 'redis\.control-domain' -Because '控制域的组不动'
    Assert-NotMatch -Text $control -Pattern '(?m)^\s+- alert:\s*RedisPlayer' -Because '玩家域规则不并进控制域文件'
}

Test-Case 'redis-control-alerts.yaml:与玩家域那份一条对一条(表达式只差 pod 选择器,for 与 severity 相同);对象名与组名不重名;只看本集群' {
    $playerText = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/k8s/redis-player-alerts.yaml')) -replace "`r`n", "`n"
    $controlText = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/k8s/redis-control-alerts.yaml')) -replace "`r`n", "`n"
    Assert-Match -Text $controlText -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '安装方式与玩家域那份相同'
    Assert-Match -Text $controlText -Pattern '(?m)^  name:[ ]*redis-control-alerts[ ]*$' -Because 'PrometheusRule 对象名不能与玩家域那份相同,否则同一个 namespace 里 apply 互相覆盖'
    $groups = @([regex]::Matches($controlText, '(?m)^    - name:[ ]*(\S+)[ ]*$') | ForEach-Object { $_.Groups[1].Value })
    Assert-Equal -Expected 'redis.control-cluster' -Actual ($groups -join ',') -Because '单独一组,不与单实例控制域的 redis.control-domain 重名'
    # 规则名去掉前缀后作键;表达式取 expr 块到 for 之前。
    $parse = {
        param([string]$Text, [string]$Prefix)
        $map = [ordered]@{}
        foreach ($rule in @(($Text -split '(?m)^        - alert:') | Select-Object -Skip 1)) {
            $name = ($rule -split "`n")[0].Trim()
            if (-not $name.StartsWith($Prefix, [System.StringComparison]::Ordinal)) { throw "规则 $name 没有前缀 $Prefix" }
            $map[$name.Substring($Prefix.Length)] = [pscustomobject]@{
                Rule     = $rule
                Expr     = [regex]::Match($rule, '(?ms)^          expr: \|\n(.*?)^          for:').Groups[1].Value
                For      = [regex]::Match($rule, '(?m)^          for:[ ]*(\S+)').Groups[1].Value
                Severity = [regex]::Match($rule, '(?m)^            severity:[ ]*(\S+)').Groups[1].Value
            }
        }
        return $map
    }
    $player = & $parse $playerText 'RedisPlayer'
    $controlRules = & $parse $controlText 'RedisControl'
    Assert-Equal -Expected (@($player.Keys) -join ',') -Actual (@($controlRules.Keys) -join ',') -Because '两套集群的规则一条对一条,顺序相同'
    foreach ($suffix in @($player.Keys)) {
        $rule = $controlRules[$suffix]
        Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($rule.Expr)) -Because "RedisControl$suffix 要能读到表达式(夹具前提)"
        Assert-Equal -Expected ($player[$suffix].Expr -replace 'redis-player-', 'redis-control-') -Actual $rule.Expr -Because "RedisControl$suffix 的表达式与玩家域那条只差 pod 选择器"
        Assert-Equal -Expected $player[$suffix].For -Actual $rule.For -Because "RedisControl$suffix 的 for 与玩家域相同"
        Assert-Equal -Expected $player[$suffix].Severity -Actual $rule.Severity -Because "RedisControl$suffix 的 severity 与玩家域相同"
        Assert-Match -Text $rule.Rule -Pattern '(?m)^            summary:' -Because "RedisControl$suffix 要有 summary"
        Assert-Match -Text $rule.Rule -Pattern '(?m)^            description:' -Because "RedisControl$suffix 要有 description"
        Assert-Match -Text $rule.Expr -Pattern 'pod=~"redis-control-\[0-9\]\+"' -Because "RedisControl$suffix 只看本集群"
        # 手册两套共用,不算引用玩家域。
        Assert-NotMatch -Text ($rule.Rule -replace 'redis-player-cluster-runbook\.md', '') -Pattern 'redis-player' -Because "RedisControl$suffix 不能引用玩家域的 Pod 或 Secret"
    }
    Assert-NotMatch -Text $controlText -Pattern 'player_id' -Because '不用玩家维度 label(AGENTS §9)'
    $core = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/k8s/redis-alerts.yaml'))
    Assert-NotMatch -Text $core -Pattern '(?m)^\s+- alert:\s*RedisControl' -Because '控制集群的规则不并进单实例控制域文件'
}

Test-Case 'scene-manager-alerts.yaml 的成员对账规则:指标名与 outcome 取值从 Go 源码核对' {
    $text = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/k8s/scene-manager-alerts.yaml')) -replace "`r`n", "`n"
    Assert-Match -Text $text -Pattern '(?m)^    - name: scene-manager\.scene-member-reconcile\s*$' -Because '成员对账一组'
    foreach ($alert in @('SceneManagerSceneMemberReconcileErrors', 'SceneManagerSceneMemberReconcileRemoving', 'SceneManagerSceneMemberReconcileStalled')) {
        Assert-Match -Text $text -Pattern ('(?m)^        - alert:[ ]*' + $alert + '\s*$') -Because "要有 $alert"
    }
    $source = [IO.File]::ReadAllText((Join-Path $RepoRoot 'go/scene_manager/internal/metrics/metrics.go'))
    $subsystem = [regex]::Match($source, '(?m)^const subsystem = "(?<s>[a-z_]+)"').Groups['s'].Value
    Assert-Equal -Expected 'scene_manager' -Actual $subsystem -Because '指标全名的前缀'
    $registered = @([regex]::Matches($source, '(?m)^\s*Name:\s*"(?<n>[a-z0-9_]+)"') | ForEach-Object { $subsystem + '_' + $_.Groups['n'].Value })
    $used = @([regex]::Matches($text, '\bscene_manager_scene_member_reconcile_[a-z0-9_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'scene_manager_scene_member_reconcile_members_total,scene_manager_scene_member_reconcile_passes_total' -Actual ($used -join ',') -Because '规则引用这两个指标'
    foreach ($metric in $used) {
        Assert-True -Condition ($registered -ccontains $metric) -Because "$metric 必须由代码注册(改名后告警静默失效)"
    }
    $outcomes = @([regex]::Matches($source, '(?m)^\s*SceneMemberReconcile[A-Za-z]+\s*=\s*"(?<v>[a-z]+)"') | ForEach-Object { $_.Groups['v'].Value })
    foreach ($selector in [regex]::Matches($text, 'scene_member_reconcile_members_total\{outcome="(?<v>[a-z]+)"\}')) {
        Assert-True -Condition ($outcomes -ccontains $selector.Groups['v'].Value) -Because "outcome=$($selector.Groups['v'].Value) 必须是 SceneMemberReconcile* 常量之一(写错取值的规则不报错,只会永远不响)"
    }
}

Test-Case 'player-locator-alerts.yaml:分片未覆盖的规则在(指标名核对在 go/player_locator 的 TestAlertRulesReferenceRegisteredMetrics)' {
    $text = [IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/k8s/player-locator-alerts.yaml')) -replace "`r`n", "`n"
    Assert-Match -Text $text -Pattern '(?m)^        - alert:[ ]*PlayerLocatorSlotSegmentsUncovered\s*$' -Because '分片对账要能发现没人负责的 slot 段'
}

# ─────────────────────────────────────────────────────────────────
# 6. compose 开发集群
# ─────────────────────────────────────────────────────────────────

Test-Case 'compose.redis-cluster.yml:3 主 3 从、同一个镜像摘要、独立项目名与端口段;每个节点的参数齐全;一次性容器建群并核对' {
    Assert-Match -Text $ComposeText -Pattern '(?m)^name: mmorpg-redis-player-cluster\s*$' -Because '独立项目,down 不误伤主 compose'
    $templateImage = [regex]::Match($TemplateText, '(?m)^\s+image:\s*(redis:\S+)\s*$').Groups[1].Value
    $images = @([regex]::Matches($ComposeText, '(?m)^\s+image:\s*(redis:\S+)\s*$') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
    Assert-Equal -Expected $templateImage -Actual ($images -join ',') -Because '本机验证和线上同一个镜像'
    foreach ($i in 0..5) {
        $port = 7100 + $i
        Assert-Match -Text $ComposeText -Pattern ("(?m)^  redis-player-cluster-${i}:\s*$") -Because "节点 $i"
        Assert-Match -Text $ComposeText -Pattern ("--port, `"$port`", --cluster-enabled, `"yes`"") -Because "节点 $i 的端口"
        Assert-Match -Text $ComposeText -Pattern ("--cluster-announce-ip, 127\.0\.0\.1, --cluster-announce-port, `"$port`", --cluster-announce-bus-port, `"1$port`"") -Because "节点 $i 宣告宿主机可达的地址"
        Assert-Match -Text $ComposeText -Pattern ("127\.0\.0\.1:${port}:$port") -Because "端口 $port 只绑回环"
    }
    Assert-Equal -Expected 6 -Actual ([regex]::Matches($ComposeText, '--appendfsync, everysec').Count) -Because '每个节点 AOF everysec'
    Assert-Equal -Expected 6 -Actual ([regex]::Matches($ComposeText, '--maxmemory-policy, noeviction').Count) -Because '每个节点 noeviction'
    Assert-Equal -Expected 6 -Actual ([regex]::Matches($ComposeText, '--cluster-require-full-coverage, "no"').Count) -Because '与线上同一口径'
    Assert-Match -Text $ComposeText -Pattern 'redis-cli --cluster create \$\$nodes --cluster-replicas 1 --cluster-yes' -Because '建群'
    Assert-Match -Text $ComposeText -Pattern 'cluster_slots_ok\)" != "16384"' -Because '核对 slot'
    Assert-Match -Text $ComposeText -Pattern 'redis-cli --cluster check 127\.0\.0\.1:7100' -Because '核对节点之间一致'
    Assert-Match -Text $ComposeText -Pattern 'restart: "no"' -Because '建群容器一次性'
}

# ─────────────────────────────────────────────────────────────────
# 7. 容量基准的判定(按 AST 抽纯函数;不起压测端)
# ─────────────────────────────────────────────────────────────────

$capacityScript = Join-Path (Get-ToolsScriptsDir) 'redis_cluster_capacity.ps1'
$capacityParseErrors = $null
$capacityAst = [System.Management.Automation.Language.Parser]::ParseFile($capacityScript, [ref]$null, [ref]$capacityParseErrors)
if ($capacityParseErrors.Count -gt 0) { throw "redis_cluster_capacity.ps1 解析失败:$($capacityParseErrors[0].Message)" }
$capacityFunctionNames = @('ConvertFrom-CapacityTarget', 'ConvertFrom-CapacitySteps', 'ConvertFrom-ClusterNodesText', 'Get-CapacityTopologyFingerprint',
    'ConvertFrom-RedisInfoText', 'Get-CapacityMasterRow', 'Get-CapacityVerdict', 'New-CapacityBenchmarkArguments')
foreach ($fnAst in $capacityAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($capacityFunctionNames -contains $fnAst.Name) {
        Set-Item -Path "Function:script:$($fnAst.Name)" -Value $fnAst.Body.GetScriptBlock()
    }
}
foreach ($fnName in $capacityFunctionNames) {
    if (-not (Test-Path "Function:$fnName")) { throw "redis_cluster_capacity.ps1 缺少函数 $fnName" }
}

function New-CapacityRow {
    param([string]$Node, [double]$Ops, [double]$Cpu, [long]$Errors = 0, [long]$Commands = 1000000)
    return [pscustomobject]@{ NodeId = ($Node * 40).Substring(0, 40); Slots = 5461; OpsPerSec = $Ops; EvalPerSec = 0; MainCpuPercent = $Cpu; UsedMemoryMb = 1; Clients = 1; ErrorReplies = $Errors; Commands = $Commands }
}

Test-Case '容量判定:主线程满且 ops 进入平台期才算证据;CPU 没满、ops 还在涨、错误多、只有一档都不算' {
    $stage = { param($Clients, [object[]]$Rows) [pscustomobject]@{ Clients = $Clients; Masters = $Rows } }
    $low = & $stage 100 @((New-CapacityRow 'a' 90000 95), (New-CapacityRow 'b' 90000 95), (New-CapacityRow 'c' 90000 95))
    $high = & $stage 200 @((New-CapacityRow 'a' 91000 99), (New-CapacityRow 'b' 91000 98), (New-CapacityRow 'c' 91000 97))
    $ok = Get-CapacityVerdict -Stages @($low, $high) -CpuThresholdPercent 90 -PlateauGrowthPercent 5
    Assert-True -Condition $ok.Saturated -Because "满足全部条件。原因: $($ok.Reasons -join '; ')"
    Assert-Equal -Expected 273000 -Actual $ok.TotalOpsPerSec -Because '三个主相加'
    Assert-Equal -Expected 97 -Actual $ok.MinMainCpuPercent -Because '取最低的主:一个主没满就不算饱和'
    Assert-Equal -Expected 3 -Actual $ok.Masters -Because '主节点数只是记录,不参与判定'

    $cold = & $stage 200 @((New-CapacityRow 'a' 91000 99), (New-CapacityRow 'b' 91000 40), (New-CapacityRow 'c' 91000 97))
    $coldVerdict = Get-CapacityVerdict -Stages @($low, $cold) -CpuThresholdPercent 90 -PlateauGrowthPercent 5
    Assert-True -Condition (-not $coldVerdict.Saturated) -Because 'ops 不涨但主线程没满:瓶颈在压测端或网络,不是 Redis'
    Assert-Match -Text ($coldVerdict.Reasons -join ';') -Pattern 'main thread not saturated on bbbbbbbb=40%' -Because '点名没满的节点'

    $growing = & $stage 200 @((New-CapacityRow 'a' 120000 99), (New-CapacityRow 'b' 120000 99), (New-CapacityRow 'c' 120000 99))
    $growingVerdict = Get-CapacityVerdict -Stages @($low, $growing) -CpuThresholdPercent 90 -PlateauGrowthPercent 5
    Assert-True -Condition (-not $growingVerdict.Saturated) -Because 'ops 还在涨,还没到顶'
    Assert-Match -Text ($growingVerdict.Reasons -join ';') -Pattern 'still growing' -Because '说清原因'

    $failing = & $stage 200 @((New-CapacityRow 'a' 91000 99 -Errors 5000), (New-CapacityRow 'b' 91000 99), (New-CapacityRow 'c' 91000 99))
    $failingVerdict = Get-CapacityVerdict -Stages @($low, $failing) -CpuThresholdPercent 90 -PlateauGrowthPercent 5
    Assert-True -Condition (-not $failingVerdict.Saturated) -Because 'OOM 或重定向也会让 ops 停涨'
    Assert-Match -Text ($failingVerdict.Reasons -join ';') -Pattern 'error replies' -Because '说清原因'

    $single = Get-CapacityVerdict -Stages @($high) -CpuThresholdPercent 90 -PlateauGrowthPercent 5
    Assert-True -Condition (-not $single.Saturated) -Because '一档看不出平台期'
}

Test-Case '容量基准:INFO 求差出每主节点 ops 与主线程 CPU;节点重启这一档作废' {
    $nodeId = 'a' * 40
    $master = [pscustomobject]@{ NodeId = $nodeId; Host = '127.0.0.1'; Port = 7100; Slots = 5461 }
    $info = {
        param($RunId, $Commands, $User, $Sys, $Eval, $Errors)
        ConvertFrom-RedisInfoText -Text "# Server`r`nrun_id:$RunId`r`n# Stats`r`ntotal_commands_processed:$Commands`r`ntotal_error_replies:$Errors`r`n# CPU`r`nused_cpu_user_main_thread:$User`r`nused_cpu_sys_main_thread:$Sys`r`n# Clients`r`nconnected_clients:7`r`n# Memory`r`nused_memory:1048576`r`n# Commandstats`r`ncmdstat_eval:calls=$Eval,usec=1,usec_per_call=0.50`r`n"
    }
    $row = Get-CapacityMasterRow -Master $master -Before (& $info 'r1' 1000 10.0 5.0 100 0) -After (& $info 'r1' 601000 50.0 15.0 100100 3) -Seconds 60
    Assert-Equal -Expected 10000 -Actual $row.OpsPerSec -Because '求差除以窗口'
    Assert-Equal -Expected 1666.7 -Actual $row.EvalPerSec -Because 'cmdstat_eval 的 calls 求差'
    Assert-Equal -Expected 83.3 -Actual $row.MainCpuPercent -Because '主线程 user + sys 求差除以窗口'
    Assert-Equal -Expected 3 -Actual $row.ErrorReplies -Because '错误回复求差'
    Assert-Equal -Expected 7 -Actual $row.Clients -Because '连接数取后一次'
    Assert-Equal -Expected 1 -Actual $row.UsedMemoryMb -Because '内存取后一次'
    Assert-Match -Text (Get-ThrownMessage { Get-CapacityMasterRow -Master $master -Before (& $info 'r1' 1 1 1 1 0) -After (& $info 'r2' 2 2 2 2 0) -Seconds 60 }) -Pattern 'restarted during the stage' -Because 'run_id 变了计数器已归零,差值没有意义'
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-RedisInfoText -Text 'NOAUTH Authentication required.' }) -Pattern 'Redis returned an error' -Because '错误回复不当成读数'
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-RedisInfoText -Text "run_id:x`ntotal_commands_processed:1" }) -Pattern 'used_cpu_user_main_thread' -Because '缺主线程 CPU 就判不了饱和'
}

Test-Case '容量基准:目标与档位校验;K8s 只许压测 namespace;CLUSTER NODES 要覆盖 16384 个 slot;负载的键带 {tag}' {
    Assert-Equal -Expected 'docker/redis-player-cluster-0/7100' -Actual ('{0}/{1}/{2}' -f (ConvertFrom-CapacityTarget -Spec 'docker-cluster:redis-player-cluster-0:7100').Kind, (ConvertFrom-CapacityTarget -Spec 'docker-cluster:redis-player-cluster-0:7100').Container, (ConvertFrom-CapacityTarget -Spec 'docker-cluster:redis-player-cluster-0:7100').Port) -Because '本机开发集群'
    Assert-Equal -Expected 'mmorpg-stress-a' -Actual (ConvertFrom-CapacityTarget -Spec 'k8s-cluster:mmorpg-stress-a/redis-player').Namespace -Because '压测 namespace'
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-CapacityTarget -Spec 'k8s-cluster:mmorpg-infra/redis-player' }) -Pattern '\^mmorpg-stress-' -Because '它往集群里写键、把主线程压满,不许进别的环境'
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-CapacityTarget -Spec 'redis:6379' }) -Pattern 'is invalid' -Because '形状不对'
    Assert-Equal -Expected '100,200' -Actual ((ConvertFrom-CapacitySteps -Text '100, 200') -join ',') -Because '逗号分隔'
    foreach ($bad in @('100', '200,100', '100,100', '0,100', 'x,100')) {
        Assert-True -Condition ((Get-ThrownMessage { ConvertFrom-CapacitySteps -Text $bad }) -ne '') -Because "'$bad' 必须拒绝:要至少两档且递增"
    }
    $node = { param($Id, $Addr, $Flags, $Slots) "$($Id * 40) $Addr $Flags - 0 0 1 connected $Slots" }
    $nodes = ((& $node 'a' '10.0.0.1:6379@16379' 'myself,master' '0-5460'), (& $node 'b' '10.0.0.2:6379@16379' 'master' '5461-10922'), (& $node 'c' '10.0.0.3:6379@16379' 'master' '10923-16382 16383'), ("$('d' * 40) 10.0.0.4:6379@16379 slave $('a' * 40) 0 0 1 connected")) -join "`n"
    $masters = ConvertFrom-ClusterNodesText -Text $nodes
    Assert-Equal -Expected 3 -Actual $masters.Count -Because '副本不算'
    Assert-Equal -Expected '5461,5462,5461' -Actual (($masters | ForEach-Object { $_.Slots }) -join ',') -Because '按 slot 段数'
    Assert-Equal -Expected '10.0.0.2:6379' -Actual ('{0}:{1}' -f $masters[1].Host, $masters[1].Port) -Because '端点取宣告的地址'
    $partial = (& $node 'a' '10.0.0.1:6379@16379' 'master' '0-5460')
    Assert-Match -Text (Get-ThrownMessage { ConvertFrom-ClusterNodesText -Text $partial }) -Pattern 'expected 16384' -Because 'slot 没覆盖全时测出来的数不算'
    Assert-NotMatch -Text (Get-CapacityTopologyFingerprint -Masters $masters) -Pattern '10\.0\.0' -Because '指纹只认节点与 slot,不认地址'
    $argv = New-CapacityBenchmarkArguments -SeedHost '10.0.0.1' -SeedPort 6379 -Clients 200 -Threads 4 -Pipeline 1 -KeySpace 100000
    Assert-True -Condition ($argv -ccontains '--cluster') -Because '必须走集群模式'
    $keys = @($argv | Where-Object { $_ -clike 'capbench:*' })
    Assert-Equal -Expected 3 -Actual $keys.Count -Because '脚本声明 3 个键'
    foreach ($key in $keys) {
        Assert-Match -Text $key -Pattern '\{tag\}' -Because '{tag} 由 redis-benchmark 换成当前节点的 slot,否则 CROSSSLOT'
        Assert-Match -Text $key -Pattern '__rand_int__' -Because '配 -r 打散键'
    }
    $scriptAt = [array]::IndexOf($argv, 'EVAL') + 1
    Assert-Equal -Expected 3 -Actual ([regex]::Matches($argv[$scriptAt], 'KEYS\[(\d)\]') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique | Measure-Object).Count -Because 'Lua 用到 3 个键,与声明一致'
    Assert-Equal -Expected 3 -Actual ([regex]::Matches($argv[$scriptAt], "'EX',600|'EXPIRE',KEYS").Count) -Because '每个键都带 TTL,跑完不用清'
}

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

exit (Complete-TestRun -SuiteName 'redis player cluster contract')
