#requires -Version 7
<#
.SYNOPSIS
    控制域按 zone 组分实例(缺口第 5 条)与 scene_manager 发现键按 zone(缺口第 4 条)的部署侧契约测试。

.DESCRIPTION
    设计:docs/design/control-plane-zone-scoping-20261105.md §3.6、§4.5、§4.6。本文件钉住:
      1. zone-up DryRun:scene-manager 的 Redis.Host = 本 zone 组的 redis-ctl-g<N>,CoreRedis.Host = 核心 redis.<infra>;
         scene-manager 服务端与 login 客户端的 go-zero 发现键都是 scenemanagerservice.rpc.z<zone>;
         C++ node-config 的 scene_control_redis 指向 redis-ctl-g<N>,zoneredis 仍是核心;
      2. zones 配置的 redisQueueGroup: 2 → redis-ctl-g2(组号与排队实例同一个字段);
      3. infra-up DryRun 不带 -RedisSplit 也渲染 redis-ctl-g0:不带 app: redis、noeviction + maxmemory、没有未替换的占位;
      4. 纯函数:实例名、容量缺省(dev 128mb、release 1gb)与非法值、线上 ConfigMap 解析、切换判定;
      5. 没建玩家域集群时 scene-manager 的 PlayerRedis 不写地址,只能回落到 CoreRedis 的核心(Go 侧 EffectivePlayerRedis,config_test.go 钉住);
      6. 控制域实例的连接总账:写路径每组一行,按组合计 zone 与 scene 进程,超水位只告警。
    子进程一律 -DryRun(不连集群);纯函数按 AST 从 k8s_deploy.ps1 抽出,在本进程里直接调用。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/k8s_redis_ctl_contract.tests.ps1
#>

$ErrorActionPreference = "Stop"

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ""
Write-Host "=== k8s_deploy.ps1 control-domain Redis per zone group contract tests ==="

$TempRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-redis-ctl-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $TempRoot | Out-Null

# 与 k8s_redis_split_contract.tests.ps1 同一套 DryRun 夹具。
function Invoke-CtlDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '本套夹具只允许显式 -DryRun,禁止连接或写集群。' }
    $fixtureEnv = @{
        MMORPG_DB_ALLOWED_DATABASES = $PlayerStoreAllowlistFixture
        MMORPG_SQL_K8S_BOOT_CATALOG = (New-SqlWriterCatalogFixture -Root (Join-Path $TempRoot 'sql-writer-catalog') -Namespaces @('mmorpg-zone-ctl-test', 'mmorpg-infra', 'ctl-infra'))
    }
    foreach ($key in $Env.Keys) { $fixtureEnv[$key] = $Env[$key] }
    $fixtureArgs = @($Arguments)
    if (-not ($fixtureArgs | Where-Object { $_ -match '^-GlobalAuthorityTiDBHost(?::|$)' })) {
        $fixtureArgs += @('-GlobalAuthorityTiDBHost', 'tidb.invalid:4000')
    }
    return Invoke-CapturedPwsh -ScriptPath (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1') -Arguments $fixtureArgs -Env $fixtureEnv
}

function New-CtlZonesJson {
    param([Parameter(Mandatory = $true)][string]$Name, [AllowNull()]$Group)
    $zone = [ordered]@{ name = 'ctl-test'; zoneId = 101 }
    if ($null -ne $Group) { $zone['redisQueueGroup'] = $Group }
    $path = Join-Path $TempRoot "$Name.json"
    Set-Content -LiteralPath $path -Encoding utf8 -Value (@{ zones = @($zone) } | ConvertTo-Json -Depth 5)
    return $path
}

function Get-CtlConfigValue {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$ConfigMap, [Parameter(Mandatory = $true)][string]$KeyPath)
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $Output -Name $ConfigMap)
    return Get-FlatValue -Flat $flat -KeyPath $KeyPath
}

$Infra = 'mmorpg-infra'
$ZoneArgs = @(
    '-Command', 'zone-up',
    '-ZoneName', 'ctl-test',
    '-ZoneId', '101',
    '-DryRun',
    '-GoSvcRegistry', 'registry.invalid/test',
    '-JavaSvcRegistry', 'registry.invalid/test'
)
$InfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-SkipGoSvc', '-SkipJavaSvc', '-InfraNamespace', 'ctl-infra',
    '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab')
$InfraEnv = @{ MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345' }

$group0Zones = New-CtlZonesJson -Name 'group0' -Group $null
$group2Zones = New-CtlZonesJson -Name 'group2' -Group 2
$zoneRun = Invoke-CtlDryRun -Arguments ($ZoneArgs + @('-ZonesConfigPath', $group0Zones))
$zoneGroup2Run = Invoke-CtlDryRun -Arguments ($ZoneArgs + @('-ZonesConfigPath', $group2Zones))
$infraRun = Invoke-CtlDryRun -Arguments ($InfraArgs + @('-ZonesConfigPath', $group0Zones)) -Env $InfraEnv

# ─────────────────────────────────────────────────────────────────
# 1. zone-up:scene-manager、login、C++ node-config
# ─────────────────────────────────────────────────────────────────

Test-Case 'zone-up DryRun:scene-manager 控制域 = redis-ctl-g0,CoreRedis = 核心;发现键按 zone 加 .z<zone>' {
    Assert-Equal -Expected 0 -Actual $zoneRun.ExitCode -Because "zone-up DryRun 必须成功。输出: $($zoneRun.Output)"
    $out = $zoneRun.Output
    Assert-Equal -Expected "redis-ctl-g0.${Infra}:6379" -Actual (Get-CtlConfigValue -Output $out -ConfigMap 'go-svc-scene-manager-config' -KeyPath 'data.scene_manager_service.yaml.Redis.Host') `
        -Because 'scene_manager 的控制域在本 zone 所属组的控制域实例'
    Assert-Equal -Expected "redis.${Infra}:6379" -Actual (Get-CtlConfigValue -Output $out -ConfigMap 'go-svc-scene-manager-config' -KeyPath 'data.scene_manager_service.yaml.CoreRedis.Host') `
        -Because 'gate 排空标记留在核心,scene_manager 经 CoreRedis 读'
    Assert-Equal -Expected 'scenemanagerservice.rpc.z101' -Actual (Get-CtlConfigValue -Output $out -ConfigMap 'go-svc-scene-manager-config' -KeyPath 'data.scene_manager_service.yaml.Etcd.Key') `
        -Because 'scene_manager 服务端的 go-zero 键按 zone'
    Assert-Equal -Expected 'scenemanagerservice.rpc.z101' -Actual (Get-CtlConfigValue -Output $out -ConfigMap 'go-svc-login-config' -KeyPath 'data.login.yaml.SceneManagerRpc.Etcd.Key') `
        -Because 'login 只连本 zone 的 scene_manager'
    Assert-Equal -Expected '101' -Actual (Get-CtlConfigValue -Output $out -ConfigMap 'go-svc-scene-manager-config' -KeyPath 'data.scene_manager_service.yaml.ZoneId') `
        -Because 'scene_manager 的 ZoneId 必须是本 zone(锁、watch、编排都按它)'
}

Test-Case 'zone-up DryRun without a player Redis cluster: scene-manager PlayerRedis has no Addrs, so it relies on falling back to CoreRedis (the core), not to the control instance' {
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $zoneRun.Output -Name 'go-svc-scene-manager-config')
    $addrKeys = @($flat.Scalars.Keys | Where-Object { $_ -like 'data.scene_manager_service.yaml.PlayerRedis.Addrs*' })
    Assert-Equal -Expected 0 -Actual $addrKeys.Count -Because 'without a cluster only the pool size is rendered; the address comes from EffectivePlayerRedis (go/scene_manager/internal/config/config.go)'
    Assert-True -Condition ($flat.Scalars.Contains('data.scene_manager_service.yaml.PlayerRedis.PoolSizePerNode')) -Because 'the PlayerRedis section itself is rendered'
    Assert-Equal -Expected "redis.${Infra}:6379" -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.scene_manager_service.yaml.CoreRedis.Host') `
        -Because 'the player domain falls back to CoreRedis, which must be the same core instance every other service and the C++ scene use'
    Assert-Equal -Expected "redis.$Infra" -Actual (Get-CtlConfigValue -Output $zoneRun.Output -ConfigMap 'node-config' -KeyPath 'data.game_config.yaml.zoneredis.host') `
        -Because 'C++ scene without a player_redis section uses zoneredis (the core) for the player domain'
}

Test-Case 'write path prints one ctl connection ledger line per group: scene-manager control pool plus one connection per scene process' {
    $out = $zoneRun.Output
    $m = [regex]::Match($out, 'Redis ctl connection ledger: instance=redis-ctl-g0 zones=1 scene_processes=(?<scene>\d+) steady=(?<steady>\d+) ')
    Assert-True -Condition $m.Success -Because "zone-up prints the ctl ledger line. Output: $out"
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($out, 'Redis ctl connection ledger:').Count) -Because 'one group, one line'
    $smManifest = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/go-svc/scene-manager.yaml') -Raw
    $smReplicas = [int]([regex]::Match($smManifest, '(?m)^\s+replicas:\s*(\d+)').Groups[1].Value)
    # 每个 scene-manager 副本一个 go-zero 缺省池 = 10 × GOMAXPROCS(按 2 计)= 20 条;每个 scene 进程 1 条。
    Assert-Equal -Expected ($smReplicas * 20 + [int]$m.Groups['scene'].Value) -Actual ([int]$m.Groups['steady'].Value) -Because 'steady = scene-manager replicas x 20 + scene processes'
    Assert-Match -Text $zoneGroup2Run.Output -Pattern 'Redis ctl connection ledger: instance=redis-ctl-g2 zones=1 ' -Because 'the group follows redisQueueGroup'
    Assert-Match -Text $infraRun.Output -Pattern 'Redis ctl connection ledger: instance=redis-ctl-g0 zones=1 ' -Because 'infra-up prints it too (zones from the zones config)'
    Assert-Match -Text $out -Pattern 'Redis connection ledger: zones=1 ' -Because 'the core ledger line is unchanged and has a different prefix'
}

Test-Case 'zone-up DryRun:C++ node-config 的 scene_control_redis 指向 redis-ctl-g0,zoneredis 仍是核心' {
    $out = $zoneRun.Output
    Assert-Equal -Expected "redis-ctl-g0.$Infra" -Actual (Get-CtlConfigValue -Output $out -ConfigMap 'node-config' -KeyPath 'data.game_config.yaml.scene_control_redis.host') `
        -Because 'scene 读的分线目录与神秘商人在本 zone 的控制域实例'
    Assert-Equal -Expected '6379' -Actual (Get-CtlConfigValue -Output $out -ConfigMap 'node-config' -KeyPath 'data.game_config.yaml.scene_control_redis.port') -Because '端口'
    Assert-Equal -Expected '0' -Actual (Get-CtlConfigValue -Output $out -ConfigMap 'node-config' -KeyPath 'data.game_config.yaml.scene_control_redis.db') -Because '库号只认 0'
    Assert-Equal -Expected "redis.$Infra" -Actual (Get-CtlConfigValue -Output $out -ConfigMap 'node-config' -KeyPath 'data.game_config.yaml.zoneredis.host') `
        -Because '落点唤醒 PUBLISH 的订阅者 guild / trade 只连核心'
}

Test-Case 'zones 配置 redisQueueGroup: 2 → scene-manager 与 node-config 都指向 redis-ctl-g2' {
    Assert-Equal -Expected 0 -Actual $zoneGroup2Run.ExitCode -Because "zone-up DryRun 必须成功。输出: $($zoneGroup2Run.Output)"
    $out = $zoneGroup2Run.Output
    Assert-Equal -Expected "redis-ctl-g2.${Infra}:6379" -Actual (Get-CtlConfigValue -Output $out -ConfigMap 'go-svc-scene-manager-config' -KeyPath 'data.scene_manager_service.yaml.Redis.Host') -Because '组号与排队实例同一个字段'
    Assert-Equal -Expected "redis-ctl-g2.$Infra" -Actual (Get-CtlConfigValue -Output $out -ConfigMap 'node-config' -KeyPath 'data.game_config.yaml.scene_control_redis.host') -Because 'scene 与 scene_manager 必须是同一个实例'
}

# ─────────────────────────────────────────────────────────────────
# 2. infra-up:控制域实例不受 -RedisSplit 控制
# ─────────────────────────────────────────────────────────────────

Test-Case 'infra-up DryRun(不带 -RedisSplit):渲染 redis-ctl-g0,noeviction + maxmemory,不带 app: redis,没有未替换的占位' {
    Assert-Equal -Expected 0 -Actual $infraRun.ExitCode -Because "infra-up DryRun 必须成功。输出: $($infraRun.Output)"
    $out = $infraRun.Output
    Assert-Match -Text $out -Pattern '(?m)^\s*name:\s*redis-ctl-g0\s*$' -Because '控制域实例每次 infra 部署都 apply'
    Assert-Match -Text $out -Pattern 'Redis ctl instances: redis-ctl-g0 maxmemory=128mb' -Because 'dev 档缺省 128mb,并打出来'
    $sts = @(Get-ManifestBlocks -Output $out | Where-Object { $_ -match '(?m)^kind: StatefulSet\s*$' -and $_ -match '(?m)^  name:\s*redis-ctl-g0\s*$' })
    Assert-Equal -Expected 1 -Actual $sts.Count -Because '恰好一份 redis-ctl-g0 StatefulSet'
    Assert-Match -Text $sts[0] -Pattern 'noeviction' -Because '承载状态的实例一律 noeviction'
    Assert-Match -Text $sts[0] -Pattern '--maxmemory' -Because '显式 maxmemory'
    Assert-NotMatch -Text $sts[0] -Pattern '(?m)^\s*app:\s*redis\s*$' -Because '带上 app: redis 会被挂到核心 Service 后面'
    Assert-NotMatch -Text $sts[0] -Pattern '__[A-Z_]+__' -Because '没有未替换的占位'
}

# ─────────────────────────────────────────────────────────────────
# 3. 纯函数(按 AST 从 k8s_deploy.ps1 抽出)
# ─────────────────────────────────────────────────────────────────

$deployParseErrors = $null
$deployAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$deployParseErrors)
if ($deployParseErrors.Count -gt 0) { throw "k8s_deploy.ps1 解析失败:$($deployParseErrors[0].Message)" }
$ctlFunctionNames = @('Get-RedisCtlInstanceName', 'Resolve-RedisCtlMemory', 'ConvertFrom-RedisCtlLiveYaml', 'Get-RedisCtlSwitchVerdict',
    'Write-RedisCtlConnectionLedger', 'Get-RedisConnectionLedger')
foreach ($fnAst in $deployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($ctlFunctionNames -contains $fnAst.Name) {
        Set-Item -Path "Function:script:$($fnAst.Name)" -Value $fnAst.Body.GetScriptBlock()
    }
}
foreach ($fnName in $ctlFunctionNames) {
    if (-not (Test-Path "Function:$fnName")) { throw "k8s_deploy.ps1 缺少函数 ${fnName}(控制域实例的接入被删了?后续断言无意义)" }
}

Test-Case 'Get-RedisCtlInstanceName:按组取 redis-ctl-g<N>,负组号拒绝' {
    Assert-Equal -Expected 'redis-ctl-g0' -Actual (Get-RedisCtlInstanceName -Group 0) -Because '缺省组'
    Assert-Equal -Expected 'redis-ctl-g7' -Actual (Get-RedisCtlInstanceName -Group 7) -Because '按组号'
    $threw = $false
    try { Get-RedisCtlInstanceName -Group -1 | Out-Null } catch { $threw = $true }
    Assert-True -Condition $threw -Because '负组号必须拒绝'
}

Test-Case 'Resolve-RedisCtlMemory:dev 缺省 128mb,release 缺省 1gb,显式值原样(容器 2 倍),非法值拒绝' {
    Assert-Equal -Expected '128mb/256Mi' -Actual ((Resolve-RedisCtlMemory -Value '' -ReleaseProfileName 'dev' -OpsProfileName 'custom') | ForEach-Object { "$($_.MaxMemory)/$($_.ContainerMemory)" }) -Because 'dev 档'
    Assert-Equal -Expected '1gb/2Gi' -Actual ((Resolve-RedisCtlMemory -Value '' -ReleaseProfileName 'prod' -OpsProfileName 'custom') | ForEach-Object { "$($_.MaxMemory)/$($_.ContainerMemory)" }) -Because 'release 档缺省每组 1gb'
    Assert-Equal -Expected '512mb/1024Mi' -Actual ((Resolve-RedisCtlMemory -Value '512MB' -ReleaseProfileName 'prod' -OpsProfileName 'custom') | ForEach-Object { "$($_.MaxMemory)/$($_.ContainerMemory)" }) -Because '显式值规范成小写'
    foreach ($bad in @('1g', '0mb', '-1gb', 'lots')) {
        $threw = $false
        try { Resolve-RedisCtlMemory -Value $bad -ReleaseProfileName 'dev' -OpsProfileName 'custom' | Out-Null } catch { $threw = $true; Assert-Match -Text $_.Exception.Message -Pattern '-RedisCtlMaxMemory' -Because '错误点名参数' }
        Assert-True -Condition $threw -Because "非法值 '$bad' 必须拒绝"
    }
}

Test-Case 'ConvertFrom-RedisCtlLiveYaml + Get-RedisCtlSwitchVerdict:不存在放行,同址不动,换址要确认' {
    $absent = ConvertFrom-RedisCtlLiveYaml -SceneManagerYaml $null
    Assert-Equal -Expected 'absent' -Actual (Get-RedisCtlSwitchVerdict -Live $absent -DesiredHostPort 'redis-ctl-g0.ns:6379') -Because '首次部署'
    $yaml = "Name: scenemanagerservice.rpc`nEtcd:`n  Hosts:`n    - `"etcd.ns:2379`"`n  Key: scenemanagerservice.rpc.z101`nRedis:`n  Host: redis.ns:6379`n  Type: node`n  Pass: `"`"`nCoreRedis:`n  Host: redis.ns:6379`n"
    $live = ConvertFrom-RedisCtlLiveYaml -SceneManagerYaml $yaml
    Assert-Equal -Expected 'redis.ns:6379' -Actual $live.HostPort -Because '只读顶层 Redis 段的 Host,不读 Etcd.Hosts 与 CoreRedis'
    Assert-Equal -Expected 'refuse' -Actual (Get-RedisCtlSwitchVerdict -Live $live -DesiredHostPort 'redis-ctl-g0.ns:6379') -Because '线上指向核心:新实例是空的,要确认'
    Assert-Equal -Expected 'switch' -Actual (Get-RedisCtlSwitchVerdict -Live $live -DesiredHostPort 'redis-ctl-g0.ns:6379' -Confirmed $true) -Because '带了确认放行'
    $same = ConvertFrom-RedisCtlLiveYaml -SceneManagerYaml ($yaml -replace 'Redis:\n  Host: redis\.ns:6379', "Redis:`n  Host: redis-ctl-g0.ns:6379")
    Assert-Equal -Expected 'unchanged' -Actual (Get-RedisCtlSwitchVerdict -Live $same -DesiredHostPort 'redis-ctl-g0.ns:6379') -Because '同址不动'
    $unknown = ConvertFrom-RedisCtlLiveYaml -SceneManagerYaml 'Name: x'
    Assert-Equal -Expected 'refuse' -Actual (Get-RedisCtlSwitchVerdict -Live $unknown -DesiredHostPort 'redis-ctl-g0.ns:6379') -Because '认不出线上地址按地址不同拒绝(fail-closed)'
    $emptyData = ConvertFrom-RedisCtlLiveYaml -SceneManagerYaml ''
    Assert-Equal -Expected 'True/refuse' -Actual ('{0}/{1}' -f $emptyData.Present, (Get-RedisCtlSwitchVerdict -Live $emptyData -DesiredHostPort 'redis-ctl-g0.ns:6379')) `
        -Because 'a ConfigMap without the data key is present but unreadable; only $null means absent'
}

# 总账(Write-RedisCtlConnectionLedger):算式是 Get-RedisConnectionLedger 的同一份函数体;客户端输入、组的划分与 Redis 形态换成替身。
$script:CtlGroupByZone = @{}
$script:CtlLedgerMaxClients = 10000
function Get-ZoneRedisCtlTopology {
    param([Parameter(Mandatory = $true)][int]$CurrentZoneId)
    $name = 'redis-ctl-g{0}' -f $script:CtlGroupByZone[$CurrentZoneId]
    return [pscustomobject]@{ Service = $name; Host = "$name.ns"; Port = 6379; HostPort = "$name.ns:6379" }
}
function Get-RedisCtlConnectionLedgerInputs {
    return [ordered]@{
        MaxClients = $script:CtlLedgerMaxClients; GoPoolSizePerProc = 10; CppConnectionsPerProcess = 1
        Clients    = @([ordered]@{ Name = 'scene-manager'; Scope = 'zone'; Replicas = 2; ConfiguredPoolConns = 0; DefaultPools = 1; GoMaxProcs = 2; PubSub = 0; Fixed = 0 })
    }
}
function Get-RedisTopology { return $null }
function Get-RedisQueueShapeFor { param([AllowNull()]$Topology) return [pscustomobject]@{ HighAvailability = $false } }
function Invoke-CtlLedger {
    param([Parameter(Mandatory = $true)][hashtable]$SceneByZone)
    $scene = [System.Collections.Generic.Dictionary[int, int]]::new()
    foreach ($key in $SceneByZone.Keys) { $scene[[int]$key] = [int]$SceneByZone[$key] }
    $plan = [ordered]@{ ZoneCount = 10; ScenePerZone = 4; RolloutExtraPodsPerDeployment = 1; OpsReserve = 100; WarnRatio = 0.8 }
    return (@(Write-RedisCtlConnectionLedger -SceneProcessesByZone $scene -LedgerPlan $plan 6>&1 3>&1) | ForEach-Object { $_.ToString() }) -join "`n"
}

Test-Case 'Write-RedisCtlConnectionLedger: one line per group, zones and scene processes summed per group, plan puts every zone in one group' {
    $script:CtlGroupByZone = @{ 101 = 0; 102 = 0; 103 = 1 }
    $script:CtlLedgerMaxClients = 10000
    $printed = Invoke-CtlLedger -SceneByZone @{ 101 = 3; 102 = 5; 103 = 2 }
    # 每 zone = 2 副本 × 1 个缺省池 × 10 × GOMAXPROCS 2 = 40;滚动余量 = 1 × (20 + 1 个 C++ 工作负载 × 1) = 21;峰值 = 稳态 + 21 + 100。
    Assert-Match -Text $printed -Pattern 'Redis ctl connection ledger: instance=redis-ctl-g0 zones=2 scene_processes=8 steady=88 peak=209 maxclients=10000 warn_at=8000 headroom=9791; plan zones=10 peak=561 headroom=9439 ' `
        -Because 'group 0: two zones, 3 + 5 scene processes; plan: 10 zones x 40 + 40 scene processes'
    Assert-Match -Text $printed -Pattern 'Redis ctl connection ledger: instance=redis-ctl-g1 zones=1 scene_processes=2 steady=42 peak=163 ' -Because 'group 1: one zone'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($printed, 'Redis ctl connection ledger:').Count) -Because 'one line per group'
    Assert-NotMatch -Text $printed -Pattern 'OverWarnRatio|proxy ledger' -Because 'far below maxclients; single-instance shape has no proxy line'
    Assert-Equal -Expected '' -Actual (Invoke-CtlLedger -SceneByZone @{}) -Because 'no zones, no line'
}

Test-Case 'Write-RedisCtlConnectionLedger: above the warn ratio it warns (per group and plan) without throwing' {
    $script:CtlGroupByZone = @{ 101 = 0; 102 = 0; 103 = 1 }
    $script:CtlLedgerMaxClients = 200
    $printed = Invoke-CtlLedger -SceneByZone @{ 101 = 3; 102 = 5; 103 = 2 }
    Assert-Match -Text $printed -Pattern '\[RedisCtlConnectionLedger\]OverWarnRatio:redis-ctl-g0 zones=2 peak 209; redis-ctl-g1 zones=1 peak 163; plan zones=10 in one group peak 561; maxclients=200 warn_at=160 ' `
        -Because 'warn_at = floor(200 x 0.8) = 160; every ledger above it is named'
    Assert-Match -Text $printed -Pattern 'Deployment continues: warning only, not blocking' -Because 'the ledger never blocks a deployment'
}

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

exit (Complete-TestRun -SuiteName "k8s redis ctl contract")
