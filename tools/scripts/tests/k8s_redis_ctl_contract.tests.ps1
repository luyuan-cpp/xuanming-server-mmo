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
      4. 纯函数:实例名、容量缺省(dev 128mb、release 1gb)与非法值、线上 ConfigMap 解析、切换判定。
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
        MMORPG_DB_ALLOWED_DATABASES = 'zone_1_db,zone_2_db,zone_101_db,zone_102_db'
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
$ctlFunctionNames = @('Get-RedisCtlInstanceName', 'Resolve-RedisCtlMemory', 'ConvertFrom-RedisCtlLiveYaml', 'Get-RedisCtlSwitchVerdict')
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
}

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

exit (Complete-TestRun -SuiteName "k8s redis ctl contract")
