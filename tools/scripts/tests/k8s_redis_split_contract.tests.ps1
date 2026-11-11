#requires -Version 7
<#
.SYNOPSIS
    共享 Redis 按用途拆实例(G4a 第 1a 批:queue)的部署侧契约测试。

.DESCRIPTION
    设计:docs/design/shared-redis-split-by-purpose.md §25.3(2026-10-23 拍板记录)、§3.2.1;部署说明见 deploy/k8s/README.md
    「共享 Redis 按用途拆实例」。本文件钉住:
      1. 不带 -RedisSplit(缺省 auto,DryRun 按 none)时 zone-up / infra-up 不配置 QueueRedis、不渲染或操作 redis-queue-g 实例,
         infra-up 共享模板的整行说明注释除外;gateway 的 spring.data.redis.host 仍是核心 redis.<infra>;
      2. -RedisSplit queue:login 整段写 Node.QueueRedis(Host = redis-queue-g0.<infra>:6379,其余字段与 RedisClient 逐项同值),
         gateway 指向 redis-queue-g0.<infra>;C++ zoneredis、data-service NameCacheRedis 仍是核心,go/db 不连 Redis;
      3. zones 配置的 redisQueueGroup: 2 → redis-queue-g2;非法值(负数、小数、字符串)在任何写操作之前 throw;
      4. infra-up -RedisSplit queue 渲染 redis-queue-g0:不带 app: redis、noeviction + maxmemory、MAXCLIENTS 只定义一次、
         --enable-debug-command no、redis-exporter sidecar、没有未替换的占位,且排在核心 Redis 之后;
      5. release 档要 apply queue 实例而没给 -RedisQueueMaxMemory → 拒绝;
      6. 切换闸的判定(Get-RedisPurposeSwitchVerdict)与入口闸(Assert-RedisPurposeSwitchIsSafe,线上事实注入,不连集群);
      7. redis-wipe 的准入(namespace 规则 + 确认)与 exec 输出解析;dev_tools.ps1 的透传口径;
      8. 高可用形态(-RedisReplicas 3,redis-purpose-ha.yaml;§25.3 第 18 条子项 1 与 2026-11-01 落地说明):两种形态的渲染与同名对象、生效行与 redis-ha.yaml
         逐行同源、对象名不与核心撞、地址不随形态变、连接上限与代理总账、形态变更闸与 orphan 删除、redis-split-status 读实例状态、redis-wipe 找主。
    子进程一律 -DryRun(不连集群);纯函数按 AST 从 k8s_deploy.ps1 抽出,在本进程里直接调用;读写集群的函数在用例里换成替身。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/k8s_redis_split_contract.tests.ps1
#>

$ErrorActionPreference = "Stop"

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ""
Write-Host "=== k8s_deploy.ps1 shared Redis split (G4a batch 1a) contract tests ==="

# 与 k8s_deploy_contract.tests.ps1 同一套 DryRun 夹具:虚构的全局 SQL 入口与库名白名单,只用于 DryRun,不解析也不连接。
function Invoke-SplitDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '本套夹具只允许显式 -DryRun,禁止连接或写集群。' }
    $fixtureEnv = @{
        MMORPG_DB_ALLOWED_DATABASES = 'zone_1_db,zone_2_db,zone_101_db,zone_102_db'
        # zone-up 渲染 SQL 写者要写者计划。
        MMORPG_SQL_K8S_BOOT_CATALOG = (New-SqlWriterCatalogFixture -Root (Join-Path $TempRoot 'sql-writer-catalog') -Namespaces @('mmorpg-zone-split-test', 'mmorpg-infra', 'split-infra'))
    }
    foreach ($key in $Env.Keys) { $fixtureEnv[$key] = $Env[$key] }
    $fixtureArgs = @($Arguments)
    if (-not ($fixtureArgs | Where-Object { $_ -match '^-GlobalAuthorityTiDBHost(?::|$)' })) {
        $fixtureArgs += @('-GlobalAuthorityTiDBHost', 'tidb.invalid:4000')
    }
    return Invoke-CapturedPwsh -ScriptPath (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1') -Arguments $fixtureArgs -Env $fixtureEnv
}

# zone-up:部署 Go 与 Java 服务(login / gateway 才会渲染),infra namespace 用缺省 mmorpg-infra。
$ZoneArgs = @(
    '-Command', 'zone-up',
    '-ZoneName', 'split-test',
    '-ZoneId', '101',
    '-DryRun',
    '-GoSvcRegistry', 'registry.invalid/test',
    '-JavaSvcRegistry', 'registry.invalid/test'
)
$Infra = 'mmorpg-infra'
# infra-up:与 k8s_deploy_contract.tests.ps1 的 $BattleInfraArgs 同形(不部署 Go / Java 服务,带 -WaitReady 看部署后核对的 DryRun 行)。
$InfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-SkipGoSvc', '-SkipJavaSvc', '-WaitReady', '-InfraNamespace', 'split-infra',
    '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab')
$InfraEnv = @{ MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345' }

# 临时 zones 配置:只放 zoneId 101 一个 zone,redisQueueGroup 由用例给。
$TempRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-redis-split-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $TempRoot | Out-Null
function New-TempZonesJson {
    param([Parameter(Mandatory = $true)][string]$Name, [AllowNull()]$Group, [switch]$RawGroup)
    $zone = [ordered]@{ name = 'split-test'; zoneId = 101 }
    if ($null -ne $Group) { $zone['redisQueueGroup'] = $Group }
    $path = Join-Path $TempRoot "$Name.json"
    Set-Content -LiteralPath $path -Encoding utf8 -Value (@{ zones = @($zone) } | ConvertTo-Json -Depth 5)
    return $path
}

# 取 ConfigMap 拍平后的一个值;块缺席或键缺席都 throw(fail-closed)。
function Get-ConfigValue {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$ConfigMap, [Parameter(Mandatory = $true)][string]$KeyPath)
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $Output -Name $ConfigMap)
    return Get-FlatValue -Flat $flat -KeyPath $KeyPath
}

$defaultZoneRun = Invoke-SplitDryRun -Arguments $ZoneArgs
$queueZoneRun = Invoke-SplitDryRun -Arguments ($ZoneArgs + @('-RedisSplit', 'queue'))
$defaultInfraRun = Invoke-SplitDryRun -Arguments $InfraArgs -Env $InfraEnv
$queueInfraRun = Invoke-SplitDryRun -Arguments ($InfraArgs + @('-RedisSplit', 'queue')) -Env $InfraEnv
# 高可用形态(第 8 节):dev 档显式给 3,不依赖 release 档的密钥;prod 缺省就是 3 的那条在第 8 节单独跑。
$queueHaInfraRun = Invoke-SplitDryRun -Arguments ($InfraArgs + @('-RedisSplit', 'queue', '-RedisReplicas', '3')) -Env $InfraEnv
$queueHaZoneRun = Invoke-SplitDryRun -Arguments ($ZoneArgs + @('-RedisSplit', 'queue', '-RedisReplicas', '3'))

# ─────────────────────────────────────────────────────────────────
# 1. 缺省(不带 -RedisSplit):产物与拆分前一致
# ─────────────────────────────────────────────────────────────────

Test-Case '缺省 zone-up DryRun:login 没有 QueueRedis、gateway 指向核心 redis.<infra>,整份输出没有 redis-queue-g 字样' {
    Assert-Equal -Expected 0 -Actual $defaultZoneRun.ExitCode -Because "缺省 zone-up DryRun 必须成功。输出: $($defaultZoneRun.Output)"
    $out = $defaultZoneRun.Output
    $login = Select-ManifestByName -Output $out -Name 'go-svc-login-config'
    Assert-True -Condition ($null -ne $login) -Because 'DryRun 输出里应当有 go-svc-login-config'
    Assert-NotMatch -Text $login -Pattern 'QueueRedis' -Because '不拆时 login 不写 Node.QueueRedis(整段不写 = 与 RedisClient 共用同一个 client)'
    Assert-Equal -Expected "redis.${Infra}:6379" -Actual (Get-ConfigValue -Output $out -ConfigMap 'go-svc-login-config' -KeyPath 'data.login.yaml.Node.RedisClient.Host') -Because 'login 的 RedisClient 恒为核心'
    Assert-Equal -Expected "redis.$Infra" -Actual (Get-ConfigValue -Output $out -ConfigMap 'java-svc-gateway-config' -KeyPath 'data.application.yaml.spring.data.redis.host') -Because '不拆时 gateway 的限流 Redis 仍是核心'
    Assert-NotMatch -Text $out -Pattern 'redis-queue-g' -Because '不带 -RedisSplit 时任何产物与打印都不该出现排队实例'
    Assert-NotMatch -Text $out -Pattern 'Redis queue connection ledger' -Because '不拆时不打 queue 实例的总账'
    Assert-Match -Text $out -Pattern 'Redis split: zone=split-test zone_id=101 requested=auto resolved=none ' -Because 'DryRun 下 auto 按 none 渲染,并把将要写的地址打出来'
}

Test-Case '缺省 infra-up DryRun:不渲染或操作 queue 实例,共享模板的整行说明注释不算部署' {
    Assert-Equal -Expected 0 -Actual $defaultInfraRun.ExitCode -Because "缺省 infra-up DryRun 必须成功。输出: $($defaultInfraRun.Output)"
    # 控制域实例复用 redis-purpose.yaml,模板头说明包含 redis-queue-g<N>;只排除整行注释,实际资源和操作仍须没有 queue 实例。
    Assert-NotMatch -Text $defaultInfraRun.Output -Pattern '(?m)^(?![ \t]*#)[^\r\n]*redis-queue-g' -Because 'infra-up 的 auto 按 none:不 apply 也不删除 queue 实例'
    Assert-Match -Text $defaultInfraRun.Output -Pattern 'mmorpg\.io/redis-pod-template-digest' -Because '对照:核心 Redis 照常渲染'
}

# ─────────────────────────────────────────────────────────────────
# 2. -RedisSplit queue:login / gateway 走排队实例,其余恒为核心
# ─────────────────────────────────────────────────────────────────

Test-Case '-RedisSplit queue:login 整段写 QueueRedis(Host = redis-queue-g0,其余与 RedisClient 逐项同值,PoolSize 镜像 login.yaml)' {
    Assert-Equal -Expected 0 -Actual $queueZoneRun.ExitCode -Because "-RedisSplit queue 的 zone-up DryRun 必须成功。输出: $($queueZoneRun.Output)"
    $out = $queueZoneRun.Output
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $out -Name 'go-svc-login-config')
    Assert-Equal -Expected "redis-queue-g0.${Infra}:6379" -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Node.QueueRedis.Host') -Because '排队实例按 zone 组取地址,缺省组 0'
    Assert-Equal -Expected "redis.${Infra}:6379" -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Node.RedisClient.Host') -Because 'RedisClient 仍是核心(会话、锁、gate 排空标记都在核心)'
    # login 的 QueueRedis 是指针 + optional,段一出现内层字段全部必填:缺一个 login 就拒启,所以逐项核对。
    foreach ($field in @('Password', 'DB', 'DefaultTTL', 'DialTimeout', 'ReadTimeout', 'WriteTimeout', 'PoolSize')) {
        Assert-Equal -Expected (Get-FlatValue -Flat $flat -KeyPath "data.login.yaml.Node.RedisClient.$field") -Actual (Get-FlatValue -Flat $flat -KeyPath "data.login.yaml.Node.QueueRedis.$field") `
            -Because "QueueRedis.$field 必须与 RedisClient.$field 同值"
    }
    Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.RedisClient.PoolSize') -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Node.QueueRedis.PoolSize') `
        -Because 'QueueRedis 的池上限镜像 go/login/etc/login.yaml 的 Node.RedisClient.PoolSize(连接总账按同一个键记)'
    Assert-Equal -Expected '0' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Node.QueueRedis.DB') -Because '排队实例用 DB 0'
}

Test-Case '-RedisSplit queue:gateway 的 spring.data.redis.host = redis-queue-g0.<infra>,端口不变' {
    $out = $queueZoneRun.Output
    Assert-Equal -Expected "redis-queue-g0.$Infra" -Actual (Get-ConfigValue -Output $out -ConfigMap 'java-svc-gateway-config' -KeyPath 'data.application.yaml.spring.data.redis.host') -Because 'gateway 的 Redis 只用于 Lua 限流(RedisRateLimitStore),跟着排队实例走'
    Assert-Equal -Expected '6379' -Actual (Get-ConfigValue -Output $out -ConfigMap 'java-svc-gateway-config' -KeyPath 'data.application.yaml.spring.data.redis.port') -Because '端口行不变'
}

Test-Case '-RedisSplit queue:C++ zoneredis、data-service NameCacheRedis 仍是核心 redis.<infra>:6379' {
    $out = $queueZoneRun.Output
    $zoneRedis = '{0}:{1}' -f (Get-ConfigValue -Output $out -ConfigMap 'node-config' -KeyPath 'data.game_config.yaml.zoneredis.host'), (Get-ConfigValue -Output $out -ConfigMap 'node-config' -KeyPath 'data.game_config.yaml.zoneredis.port')
    Assert-Equal -Expected "redis.${Infra}:6379" -Actual $zoneRedis -Because 'C++ 的归属与会话协调恒在核心,永不指向拆出去的实例'
    Assert-Equal -Expected "redis.${Infra}:6379" -Actual (Get-ConfigValue -Output $out -ConfigMap 'go-svc-data-service-config' -KeyPath 'data.data_service.yaml.NameCacheRedis.Host') -Because 'data-service 的名字读缓存恒在核心'
}

Test-Case '-RedisSplit queue:核心总账去掉 gateway,另打 queue 实例的总账一行;DryRun 打出 resolved=queue' {
    $out = $queueZoneRun.Output
    $coreLines = @($out -split "`r?`n" | Where-Object { $_ -match '^\s*Redis connection ledger: zones=' })
    Assert-Equal -Expected 1 -Actual $coreLines.Count -Because '核心总账仍恰好一行(前缀与 queue 那行不同)'
    Assert-Match -Text $out -Pattern '(?m)^\s*Redis queue connection ledger: instance=redis-queue-g0 zones=\d+ steady=\d+ peak=\d+ maxclients=10000 ' -Because 'queue 实例的总账按 redis-purpose.yaml 的 MAXCLIENTS 记'
    Assert-Match -Text $out -Pattern 'Redis split: zone=split-test zone_id=101 requested=queue resolved=queue queue_redis=redis-queue-g0\.mmorpg-infra:6379 ' -Because '入口打出本 zone 将要写的排队地址'
    $default = [regex]::Match($defaultZoneRun.Output, '(?m)^\s*Redis connection ledger: zones=(\d+) steady=(\d+) ')
    $split = [regex]::Match($out, '(?m)^\s*Redis connection ledger: zones=(\d+) steady=(\d+) ')
    Assert-True -Condition ($default.Success -and $split.Success) -Because '两次运行都要有核心总账行'
    Assert-Equal -Expected $default.Groups[1].Value -Actual $split.Groups[1].Value -Because '两次数的是同一批 zone'
    Assert-True -Condition ([int]$split.Groups[2].Value -lt [int]$default.Groups[2].Value) -Because 'gateway 改连排队实例,核心稳态连接数必须变少'
}

# ─────────────────────────────────────────────────────────────────
# 3. zones 配置的 redisQueueGroup
# ─────────────────────────────────────────────────────────────────

Test-Case 'zones 配置 redisQueueGroup: 2 → login / gateway 都指向 redis-queue-g2' {
    $zonesPath = New-TempZonesJson -Name 'group2' -Group 2
    $run = Invoke-SplitDryRun -Arguments ($ZoneArgs + @('-RedisSplit', 'queue', '-ZonesConfigPath', $zonesPath))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "组 2 的 zone-up DryRun 必须成功。输出: $($run.Output)"
    Assert-Equal -Expected "redis-queue-g2.${Infra}:6379" -Actual (Get-ConfigValue -Output $run.Output -ConfigMap 'go-svc-login-config' -KeyPath 'data.login.yaml.Node.QueueRedis.Host') -Because 'login 按 zones 配置的组取实例'
    Assert-Equal -Expected "redis-queue-g2.$Infra" -Actual (Get-ConfigValue -Output $run.Output -ConfigMap 'java-svc-gateway-config' -KeyPath 'data.application.yaml.spring.data.redis.host') -Because 'gateway 与 login 必须是同一个组实例'
    Assert-NotMatch -Text $run.Output -Pattern 'redis-queue-g0' -Because '组 2 的 zone 不该出现 g0'
}

foreach ($bad in @(
        @{ Label = '负数'; Value = -1 },
        @{ Label = '小数'; Value = 1.5 },
        @{ Label = '非数字字符串'; Value = 'abc' }
    )) {
    Test-Case "zones 配置 redisQueueGroup 非法($($bad.Label))→ 在任何写操作之前 throw 并点名" {
        $zonesPath = New-TempZonesJson -Name ('bad-' + [guid]::NewGuid().ToString('N')) -Group $bad.Value
        $run = Invoke-SplitDryRun -Arguments ($ZoneArgs + @('-RedisSplit', 'queue', '-ZonesConfigPath', $zonesPath))
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "$($bad.Label) 必须拒绝。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern "zone 'split-test' 的 redisQueueGroup=.* 不合法" -Because '错误必须点名 zone 与字段'
        Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because 'zones 配置校验先于任何写操作'
    }
}

# ─────────────────────────────────────────────────────────────────
# 4. infra-up -RedisSplit queue:redis-purpose.yaml 的渲染
# ─────────────────────────────────────────────────────────────────

Test-Case 'infra-up -RedisSplit queue:渲染 redis-queue-g0(不带 app: redis、noeviction + maxmemory、MAXCLIENTS 一次、debug 关、exporter、无未替换占位),排在核心之后' {
    Assert-Equal -Expected 0 -Actual $queueInfraRun.ExitCode -Because "infra-up -RedisSplit queue DryRun 必须成功(dev 档缺省 128mb)。输出: $($queueInfraRun.Output)"
    $out = $queueInfraRun.Output
    $block = Select-ManifestByName -Output $out -Name 'redis-queue-g0'
    Assert-True -Condition ($null -ne $block) -Because 'DryRun 输出里应当有 redis-queue-g0 的清单'
    Assert-NotMatch -Text $block -Pattern '(?-i)__[A-Z][A-Z0-9_]*__' -Because '占位必须全部替换'
    Assert-NotMatch -Text $block -Pattern '(?m)^\s*app:\s*redis\s*$' -Because '带 app: redis 的 Pod 会被挂到核心 Service redis 后面'
    $docs = @(($block -replace "`r`n", "`n") -split '(?m)^---[ \t]*$')
    $sts = @($docs | Where-Object { $_ -cmatch '(?m)^kind:[ \t]*StatefulSet[ \t]*$' })
    Assert-Equal -Expected 1 -Actual $sts.Count -Because '恰好一个 StatefulSet'
    $sts = $sts[0]
    Assert-Match -Text $sts -Pattern '(?m)^  name: redis-queue-g0\s*$' -Because 'StatefulSet 与 Service 同名(运维脚本按 pod/<实例名>-<序号> exec)'
    Assert-Match -Text $sts -Pattern '(?m)^  replicas: 1\s*$' -Because 'dev 档缺省 -RedisReplicas 1 = 单实例(高可用形态见第 8 节)'
    Assert-Match -Text $sts -Pattern '(?m)^\s+app: redis-queue-g0\s*$' -Because '标签与 selector 用实例名'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($sts, '(?m)^[ \t]*MAXCLIENTS=10000[ \t]*$').Count) -Because 'MAXCLIENTS 只定义一次'
    Assert-Match -Text $sts -Pattern 'redis fd limit:' -Because '启动前打印 fd 限值(同 redis.yaml)'
    # 只看 exec 那两行:模板的注释里也写了这些参数。
    $redisExecLines = @($sts -split "`n" | Where-Object { $_ -match '^\s*exec redis-server ' })
    Assert-Equal -Expected 2 -Actual $redisExecLines.Count -Because '有口令 / 无口令各一条 exec'
    foreach ($flag in @('--appendonly yes', '--appendfsync everysec', '--maxclients "$MAXCLIENTS"', '--enable-debug-command no', '--maxmemory 128mb', '--maxmemory-policy noeviction')) {
        foreach ($line in $redisExecLines) {
            Assert-True -Condition ($line.Contains($flag)) -Because "有口令 / 无口令两条 exec 都要带 $flag"
        }
    }
    Assert-Match -Text $sts -Pattern '--requirepass "\$REDIS_PASSWORD"' -Because '有口令分支 --requirepass'
    Assert-Match -Text $sts -Pattern '(?m)^\s+- name: REDIS_PASSWORD\s*$' -Because '口令环境变量名必须叫 REDIS_PASSWORD(运维脚本 exec 进来读它)'
    Assert-Match -Text $sts -Pattern 'name: redis-auth' -Because '口令来自可选 Secret redis-auth'
    Assert-Match -Text $sts -Pattern '(?m)^\s+- name: redis\s*$' -Because '容器名必须叫 redis'
    Assert-Match -Text $sts -Pattern '(?m)^\s+image: redis:7\.2\s*$' -Because '镜像与核心一致'
    Assert-Match -Text $sts -Pattern '(?m)^\s+- name: redis-exporter\s*$' -Because '带 redis-exporter sidecar(同 redis.yaml)'
    Assert-Match -Text $sts -Pattern 'kubectl\.kubernetes\.io/default-container: redis' -Because 'default-container 指向 redis'
    Assert-Match -Text $sts -Pattern 'prometheus\.io/port: "9121"' -Because '抓取注解指向 exporter'
    Assert-Match -Text $sts -Pattern '(?m)^\s+requests:\s*\n\s+cpu: 250m\s*\n\s+memory: 256Mi\s*\n\s+limits:\s*\n\s+memory: 256Mi\s*$' -Because 'requests 与 limits 的内存都是 2 × maxmemory'
    Assert-Match -Text $sts -Pattern '(?m)^\s+storage: 8Gi\s*$' -Because 'PVC 大小是脚本常量'
    foreach ($kind in @('Service', 'PodDisruptionBudget')) {
        Assert-True -Condition (@($docs | Where-Object { $_ -cmatch "(?m)^kind:[ \t]*$kind[ \t]*$" }).Count -ge 1) -Because "清单里要有 $kind"
    }
    Assert-Match -Text $block -Pattern '(?m)^  name: redis-queue-g0-headless\s*$' -Because 'governing service'
    Assert-Match -Text $block -Pattern '(?m)^  name: redis-queue-g0-pdb\s*$' -Because 'PDB'

    $coreAt = $out.IndexOf('mmorpg.io/redis-pod-template-digest')
    $queueAt = $out.IndexOf('name: redis-queue-g0')
    Assert-True -Condition ($coreAt -ge 0 -and $queueAt -gt $coreAt) -Because 'queue 实例排在核心 Redis 之后(Secret redis-auth 已在核心分支建好或删掉)'
    Assert-Match -Text $out -Pattern 'Redis queue instances: redis-queue-g0 maxmemory=128mb memory=256Mi storage=8Gi' -Because '写路径入口打出本次要 apply 的实例与容量'
    Assert-Match -Text $out -Pattern '\[dry-run\] kubectl rollout status statefulset/redis-queue-g0 ' -Because '-WaitReady 逐个等 queue 实例'
    Assert-Match -Text $out -Pattern '\[dry-run\] redis maxclients check skipped \(manifest MAXCLIENTS=10000\) statefulset=redis-queue-g0' -Because 'queue 实例同样核对 maxclients(DryRun 只打说明)'
}

Test-Case 'release 档 infra-up -RedisSplit queue 不给 -RedisQueueMaxMemory → 在任何写操作之前拒绝' {
    $run = Invoke-SplitDryRun -Arguments ($InfraArgs + @('-RedisSplit', 'queue', '-ReleaseProfile', 'prod')) -Env $InfraEnv
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "release 档缺容量必须拒绝。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern '必须显式给 -RedisQueueMaxMemory' -Because '错误必须点名缺哪个参数'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '拒绝先于任何写操作'
}

# ─────────────────────────────────────────────────────────────────
# 5. 纯函数(按 AST 从 k8s_deploy.ps1 抽出)
# ─────────────────────────────────────────────────────────────────

$deployParseErrors = $null
$deployAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$deployParseErrors)
if ($deployParseErrors.Count -gt 0) { throw "k8s_deploy.ps1 解析失败:$($deployParseErrors[0].Message)" }
$splitFunctionNames = @(
    'Get-RedisPurposeTopologyFor', 'Resolve-RedisQueueMemory', 'ConvertFrom-RedisPurposeLiveYaml', 'Get-RedisPurposeSwitchVerdict',
    'Format-RedisPurposeLiveArgs', 'Get-RedisWipeRefusal', 'ConvertFrom-RedisWipeProbe',
    # 入口闸与它读参数 / zones 配置的依赖(读集群的 Read-RedisPurposeLiveFacts 在用例里换成注入线上事实的替身)。
    'Assert-RedisPurposeSwitchIsSafe', 'Get-RedisPurposeTopology', 'Get-ZoneRedisQueueGroup', 'Resolve-ZonesConfigPath', 'Get-ZonesFromJson',
    'Test-ZoneLoginDeployed', 'Test-ZoneGatewayDeployed', 'Get-ZoneNamespace',
    # 高可用形态(第 8 节):形态、对象名、上限核对、渲染、形态变更闸、apply、读线上。读写集群的 Get-LiveStatefulSetReplicas / Invoke-Kubectl /
    # Invoke-KubectlWithInputFile / Invoke-ClientEntryKubectl 在第 8 节换成替身,其余是 k8s_deploy.ps1 的同一份函数体。
    'Get-RedisTopologyFor', 'Get-RedisTopology', 'Get-RedisTopologyChangeVerdict', 'Get-RedisMaxClientsFromManifest', 'Get-RedisHaLimits', 'Get-RedisHaScriptsDigest',
    'Assert-RedisPasswordSentinelSafe', 'Assert-InfraManifestPlaceholdersReplaced', 'Get-MysqlConnectionPlan', 'Get-RedisConnectionPlan',
    'Get-RedisQueueShapeFor', 'Get-RedisQueueHaObjectNames', 'Get-RedisQueueHaLimitsFor', 'ConvertTo-RedisQueueInstanceManifest', 'Get-RedisQueueGroupsToApply',
    'Assert-RedisQueueTopologyChangeIsSafe', 'New-RedisQueueInstanceManifests', 'Apply-RedisQueueInstances',
    'ConvertFrom-RedisQueueInstanceAddress', 'ConvertFrom-RedisQueueInstanceLive', 'Read-RedisQueueInstanceLive', 'Get-RedisSplitRolloutState', 'Show-RedisSplitStatus', 'Invoke-RedisWipe',
    # redis-split-status 的控制域一段(缺口第 5 条):读线上 ConfigMap 与 redis-ctl-g<N>,走同一个 Invoke-ClientEntryKubectl 替身。
    'Get-RedisCtlInstanceName', 'Get-ZoneRedisCtlTopology', 'Read-LiveConfigMapData', 'ConvertFrom-RedisCtlLiveYaml', 'ConvertFrom-RedisCtlNodeConfigYaml',
    'Get-RedisCtlZoneProblems', 'Get-RedisCtlZoneStatus',
    # 总账的告警路径(第 8 节):客户端输入 Get-RedisQueueConnectionLedgerInputs 换成替身,算式与判定是同一份函数体。
    'Get-RedisConnectionLedger', 'Write-RedisQueueConnectionLedger'
)
foreach ($fnAst in $deployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($splitFunctionNames -contains $fnAst.Name) {
        Set-Item -Path "Function:script:$($fnAst.Name)" -Value $fnAst.Body.GetScriptBlock()
    }
}
foreach ($fnName in $splitFunctionNames) {
    if (-not (Test-Path "Function:$fnName")) { throw "k8s_deploy.ps1 缺少函数 ${fnName}(按用途拆实例的接入被删了?后续断言无意义)" }
}
# 被抽出的函数读的脚本级常量,取 k8s_deploy.ps1 里的同一条赋值,不在测试里另写一份。
$deployTopLevelAssignments = @($deployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.AssignmentStatementAst] }, $false))
foreach ($constantName in @('RedisHaScriptsDigestPlaceholder', 'RedisQueueStorage', 'RedisWipeNamespacePattern', 'RedisSplitStatusRolloutTimeoutSeconds')) {
    $constantAssignment = @($deployTopLevelAssignments | Where-Object { $_.Left.Extent.Text -ceq "`$script:$constantName" })
    if ($constantAssignment.Count -ne 1) { throw "k8s_deploy.ps1 顶层 `$script:$constantName 的赋值应当恰好 1 处,实际 $($constantAssignment.Count) 处" }
    . ([scriptblock]::Create($constantAssignment[0].Extent.Text))
}
# redis-split-status 的控制域一段按 $GoSvcCatalogue 取 scene-manager 的 ConfigMap 名与配置文件名:同样取 k8s_deploy.ps1 里那一条赋值(纯字面量)。
$catalogueAssignment = @($deployTopLevelAssignments | Where-Object { $_.Left.Extent.Text -ceq '$GoSvcCatalogue' })
if ($catalogueAssignment.Count -ne 1) { throw "k8s_deploy.ps1 top-level `$GoSvcCatalogue should be assigned exactly once, found $($catalogueAssignment.Count)" }
. ([scriptblock]::Create($catalogueAssignment[0].Extent.Text))

Test-Case 'Get-RedisPurposeTopologyFor:none 时 Queue 就是 Core;queue 时按组取 redis-queue-g<N>;负组号与未知档位拒绝' {
    $none = Get-RedisPurposeTopologyFor -Split 'none' -InfraNamespace 'ns' -QueueGroup 3
    Assert-Equal -Expected 'redis.ns:6379' -Actual $none.Core.HostPort -Because 'Core 恒为 redis'
    Assert-Equal -Expected $none.Core.HostPort -Actual $none.Queue.HostPort -Because 'none 时排队地址 = 核心'
    Assert-True -Condition (-not $none.QueueSplit) -Because 'none 不拆'
    $queue = Get-RedisPurposeTopologyFor -Split 'queue' -InfraNamespace 'ns' -QueueGroup 3
    Assert-Equal -Expected 'redis-queue-g3/redis-queue-g3.ns/6379/redis-queue-g3.ns:6379' -Actual ('{0}/{1}/{2}/{3}' -f $queue.Queue.Service, $queue.Queue.Host, $queue.Queue.Port, $queue.Queue.HostPort) -Because 'Host 与 Port 分开给(gateway 用 host / port 两个键)'
    Assert-Equal -Expected 'redis.ns:6379' -Actual $queue.Core.HostPort -Because '拆分不动核心'
    foreach ($badCall in @({ Get-RedisPurposeTopologyFor -Split 'queue' -InfraNamespace 'ns' -QueueGroup -1 }, { Get-RedisPurposeTopologyFor -Split 'mapping' -InfraNamespace 'ns' -QueueGroup 0 })) {
        $threw = $false
        try { & $badCall | Out-Null } catch { $threw = $true }
        Assert-True -Condition $threw -Because "非法输入必须拒绝:$badCall"
    }
}

Test-Case 'Resolve-RedisQueueMemory:dev 缺省 128mb;显式值换算 2 倍容器内存;release 档缺值与非法写法拒绝' {
    $dev = Resolve-RedisQueueMemory -Value '' -ReleaseProfileName 'dev' -OpsProfileName 'custom'
    Assert-Equal -Expected '128mb/256Mi' -Actual ('{0}/{1}' -f $dev.MaxMemory, $dev.ContainerMemory) -Because 'dev 档缺省 128mb'
    foreach ($case in @(@{ In = '1gb'; Out = '1gb/2Gi' }, @{ In = '512MB'; Out = '512mb/1024Mi' }, @{ In = ' 64kb '; Out = '64kb/128Ki' })) {
        $r = Resolve-RedisQueueMemory -Value $case.In -ReleaseProfileName 'prod' -OpsProfileName 'custom'
        Assert-Equal -Expected $case.Out -Actual ('{0}/{1}' -f $r.MaxMemory, $r.ContainerMemory) -Because "'$($case.In)' 的换算"
    }
    foreach ($case in @(@{ Release = 'prod'; Ops = 'custom' }, @{ Release = 'staging'; Ops = 'custom' }, @{ Release = 'dev'; Ops = 'managed-cloud' })) {
        $message = ''
        try { Resolve-RedisQueueMemory -Value '' -ReleaseProfileName $case.Release -OpsProfileName $case.Ops | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '必须显式给 -RedisQueueMaxMemory' -Because "release 档($($case.Release)/$($case.Ops))缺值必须拒绝"
    }
    foreach ($bad in @('1g', '0mb', '1.5gb', 'abc', '-1mb')) {
        $message = ''
        try { Resolve-RedisQueueMemory -Value $bad -ReleaseProfileName 'dev' -OpsProfileName 'custom' | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '不合法' -Because "'$bad' 必须拒绝(只认 <正整数><kb|mb|gb>)"
    }
}

# 线上 ConfigMap 里的服务配置文本(只含判定读的键)。
function New-LiveLoginYaml {
    param([string]$QueueHost = '', [string]$QueueEnabled = '')
    $lines = @('Node:', '  ZoneId: 101', '  RedisClient:', '    Host: redis.ns:6379', '    Password: ""')
    if ($QueueHost) { $lines += @('  QueueRedis:', "    Host: $QueueHost", '    Password: ""') }
    if ($QueueEnabled) { $lines += @('Queue:', "  Enabled: $QueueEnabled") }
    return ($lines -join "`n")
}
function New-LiveGatewayYaml {
    param([Parameter(Mandatory = $true)][string]$RedisHost)
    return (@('spring:', '  data:', '    redis:', "      host: $RedisHost", '      port: 6379') -join "`n")
}

Test-Case 'ConvertFrom-RedisPurposeLiveYaml:线上档位、实际排队地址、组号与 Queue.Enabled(认不出按开着)' {
    $none = ConvertFrom-RedisPurposeLiveYaml -LoginYaml (New-LiveLoginYaml) -GatewayYaml (New-LiveGatewayYaml -RedisHost 'redis.ns')
    Assert-Equal -Expected 'none/redis.ns:6379/redis.ns/False' -Actual ('{0}/{1}/{2}/{3}' -f $none.LiveSplit, $none.LoginQueueHostPort, $none.GatewayHost, $none.QueueEnabled) -Because '没有 QueueRedis = 与核心共用'
    $queue = ConvertFrom-RedisPurposeLiveYaml -LoginYaml (New-LiveLoginYaml -QueueHost 'redis-queue-g4.ns:6379' -QueueEnabled 'false') -GatewayYaml $null
    Assert-Equal -Expected 'queue/redis-queue-g4.ns:6379/4/False/False' -Actual ('{0}/{1}/{2}/{3}/{4}' -f $queue.LiveSplit, $queue.LoginQueueHostPort, $queue.LiveQueueGroup, $queue.QueueEnabled, $queue.GatewayPresent) -Because '有 QueueRedis 即 queue,组号取自 Host'
    foreach ($case in @(@{ Text = 'true'; Expected = $true }, @{ Text = 'True'; Expected = $true }, @{ Text = 'maybe'; Expected = $true }, @{ Text = 'false'; Expected = $false })) {
        $live = ConvertFrom-RedisPurposeLiveYaml -LoginYaml (New-LiveLoginYaml -QueueEnabled $case.Text) -GatewayYaml $null
        Assert-Equal -Expected $case.Expected -Actual $live.QueueEnabled -Because "Queue.Enabled: $($case.Text)"
    }
    $absent = ConvertFrom-RedisPurposeLiveYaml -LoginYaml $null -GatewayYaml $null
    Assert-True -Condition (-not $absent.LoginPresent -and -not $absent.GatewayPresent -and $absent.LiveSplit -eq 'none') -Because 'ConfigMap 不存在 = 没有现状,auto 跟随时按 none'
}

Test-Case 'Get-RedisPurposeSwitchVerdict:地址要变没确认拒绝;线上排队开着拒绝(确认不豁免);一致放行;不部署的一侧不参与' {
    $liveNone = ConvertFrom-RedisPurposeLiveYaml -LoginYaml (New-LiveLoginYaml) -GatewayYaml (New-LiveGatewayYaml -RedisHost 'redis.ns')
    $liveNoneQueueOn = ConvertFrom-RedisPurposeLiveYaml -LoginYaml (New-LiveLoginYaml -QueueEnabled 'true') -GatewayYaml (New-LiveGatewayYaml -RedisHost 'redis.ns')
    $liveQueue = ConvertFrom-RedisPurposeLiveYaml -LoginYaml (New-LiveLoginYaml -QueueHost 'redis-queue-g0.ns:6379') -GatewayYaml (New-LiveGatewayYaml -RedisHost 'redis-queue-g0.ns')
    $liveAbsent = ConvertFrom-RedisPurposeLiveYaml -LoginYaml $null -GatewayYaml $null
    $desiredNone = Get-RedisPurposeTopologyFor -Split 'none' -InfraNamespace 'ns' -QueueGroup 0
    $desiredQueue = Get-RedisPurposeTopologyFor -Split 'queue' -InfraNamespace 'ns' -QueueGroup 0
    $desiredQueue1 = Get-RedisPurposeTopologyFor -Split 'queue' -InfraNamespace 'ns' -QueueGroup 1
    $cases = @(
        @{ Label = 'none → queue 未确认'; Live = $liveNone; Desired = $desiredQueue; Login = $true; Gateway = $true; Confirmed = $false; Verdict = 'refuse'; Changes = 2 },
        @{ Label = 'none → queue 已确认、排队关'; Live = $liveNone; Desired = $desiredQueue; Login = $true; Gateway = $true; Confirmed = $true; Verdict = 'switch'; Changes = 2 },
        @{ Label = 'none → queue 已确认、排队开'; Live = $liveNoneQueueOn; Desired = $desiredQueue; Login = $true; Gateway = $true; Confirmed = $true; Verdict = 'refuse'; Changes = 2 },
        @{ Label = 'queue → none(漏带 -RedisSplit)未确认'; Live = $liveQueue; Desired = $desiredNone; Login = $true; Gateway = $true; Confirmed = $false; Verdict = 'refuse'; Changes = 2 },
        @{ Label = 'queue g0 → g1(换组)未确认'; Live = $liveQueue; Desired = $desiredQueue1; Login = $true; Gateway = $true; Confirmed = $false; Verdict = 'refuse'; Changes = 2 },
        @{ Label = 'queue 一致'; Live = $liveQueue; Desired = $desiredQueue; Login = $true; Gateway = $true; Confirmed = $false; Verdict = 'unchanged'; Changes = 0 },
        @{ Label = 'none 一致'; Live = $liveNone; Desired = $desiredNone; Login = $true; Gateway = $true; Confirmed = $false; Verdict = 'unchanged'; Changes = 0 },
        @{ Label = '线上没有 ConfigMap'; Live = $liveAbsent; Desired = $desiredQueue; Login = $true; Gateway = $true; Confirmed = $false; Verdict = 'absent'; Changes = 0 },
        @{ Label = '只部署 gateway:login 的差异不参与'; Live = $liveNone; Desired = $desiredQueue; Login = $false; Gateway = $true; Confirmed = $false; Verdict = 'refuse'; Changes = 1 },
        @{ Label = '两侧都不部署'; Live = $liveNone; Desired = $desiredQueue; Login = $false; Gateway = $false; Confirmed = $false; Verdict = 'unchanged'; Changes = 0 }
    )
    foreach ($c in $cases) {
        $r = Get-RedisPurposeSwitchVerdict -Live $c.Live -Desired $c.Desired -DeploysLogin $c.Login -DeploysGateway $c.Gateway -Confirmed $c.Confirmed
        Assert-Equal -Expected $c.Verdict -Actual $r.Verdict -Because "$($c.Label) 的判定(变化:$($r.Changes -join ' | '))"
        Assert-Equal -Expected $c.Changes -Actual @($r.Changes).Count -Because "$($c.Label) 的变化条数"
    }
}

# 入口闸:读集群的 Read-RedisPurposeLiveFacts 换成注入线上事实的替身(本节的 $script:SplitLive),其余是 k8s_deploy.ps1 的同一份函数体。
$SplitZonesPath = New-TempZonesJson -Name 'gate' -Group 0
function Read-RedisPurposeLiveFacts {
    param([Parameter(Mandatory = $true)][string]$Namespace)
    $script:SplitReadNamespaces += $Namespace
    return $script:SplitLive
}
function Invoke-SplitGate {
    param([Parameter(Mandatory = $true)][string]$Requested, [Parameter(Mandatory = $true)]$Live, [switch]$Confirm)
    # 被测函数按动态作用域读这些脚本级参数(同名参数在 k8s_deploy.ps1 里的含义)。
    $Command = 'zone-up'; $ZoneName = 'split-test'; $ZoneId = 101; $DryRun = $false
    $SkipGoSvc = $false; $GoSvcRegistry = 'registry.invalid/test'; $SkipJavaSvc = $false; $JavaSvcRegistry = 'registry.invalid/test'
    $NamespacePrefix = 'mmorpg-zone'; $InfraNamespace = 'ns'; $ZonesConfigPath = $SplitZonesPath
    $CentreReplicas = 1; $GateReplicas = 2; $SceneReplicas = 4; $SceneWorldReplicas = -1; $SceneInstanceReplicas = -1; $GateNodePortBase = 30000
    $RedisSplit = $Requested
    $ConfirmRedisSplitSwitch = [bool]$Confirm
    $script:RedisSplitByZone = @{}
    $script:SplitLive = $Live
    $script:SplitReadNamespaces = @()
    $message = ''
    $warnings = ''
    try { $warnings = (Assert-RedisPurposeSwitchIsSafe 3>&1 | Out-String) } catch { $message = $_.Exception.Message }
    return [pscustomobject]@{ Error = $message; Warnings = $warnings; Followed = $script:RedisSplitByZone; Reads = @($script:SplitReadNamespaces) }
}

Test-Case 'Assert-RedisPurposeSwitchIsSafe(注入线上事实):未确认的切换与排队开着都拒绝并写明现状;auto 跟随线上;确认后放行并告警' {
    $liveNone = ConvertFrom-RedisPurposeLiveYaml -LoginYaml (New-LiveLoginYaml) -GatewayYaml (New-LiveGatewayYaml -RedisHost 'redis.ns')
    $liveNoneQueueOn = ConvertFrom-RedisPurposeLiveYaml -LoginYaml (New-LiveLoginYaml -QueueEnabled 'true') -GatewayYaml (New-LiveGatewayYaml -RedisHost 'redis.ns')
    $liveQueue = ConvertFrom-RedisPurposeLiveYaml -LoginYaml (New-LiveLoginYaml -QueueHost 'redis-queue-g0.ns:6379') -GatewayYaml (New-LiveGatewayYaml -RedisHost 'redis-queue-g0.ns')

    $r = Invoke-SplitGate -Requested 'queue' -Live $liveNone
    Assert-Match -Text $r.Error -Pattern '本次没给 -ConfirmRedisSplitSwitch' -Because '地址要变而没确认必须拒绝'
    Assert-Match -Text $r.Error -Pattern '线上现状对应的参数是 -RedisSplit none' -Because '报错写明照传现状的参数'
    Assert-Equal -Expected 'mmorpg-zone-split-test' -Actual ($r.Reads -join ',') -Because '只读本次部署的 zone 的 namespace'

    $r = Invoke-SplitGate -Requested 'queue' -Live $liveNoneQueueOn -Confirm
    Assert-Match -Text $r.Error -Pattern 'Queue\.Enabled 为 true' -Because '排队开着时确认也不豁免'

    $r = Invoke-SplitGate -Requested 'auto' -Live $liveQueue
    Assert-Equal -Expected '' -Actual $r.Error -Because 'auto 跟随线上,与线上一致即放行'
    Assert-Equal -Expected 'queue' -Actual $r.Followed[101] -Because 'auto 把线上档位记进 $script:RedisSplitByZone,供生成器与总账使用'

    $r = Invoke-SplitGate -Requested 'none' -Live $liveQueue
    Assert-Match -Text $r.Error -Pattern '线上现状对应的参数是 -RedisSplit queue,且 zones 配置里该 zone 的 redisQueueGroup 为 0' -Because '漏带 -RedisSplit queue 的例行部署被拦下并写明现状'

    $r = Invoke-SplitGate -Requested 'queue' -Live $liveNone -Confirm
    Assert-Equal -Expected '' -Actual $r.Error -Because '确认且排队关着时放行'
    Assert-Match -Text $r.Warnings -Pattern '\[RedisSplit\]Confirmed:' -Because '放行的切换必须告警'
}

Test-Case 'redis-wipe 准入:namespace 必须匹配 ^mmorpg-stress-(区分大小写)且带 -ConfirmWipeAllRedis;exec 输出解析' {
    $pattern = '^mmorpg-stress-'
    Assert-Match -Text (Get-RedisWipeRefusal -Namespace 'mmorpg-infra' -Confirmed $true -Pattern $pattern) -Pattern '不匹配压测环境的命名规则' -Because '非压测 namespace 拒绝'
    Assert-Match -Text (Get-RedisWipeRefusal -Namespace 'MMORPG-STRESS-a' -Confirmed $true -Pattern $pattern) -Pattern '不匹配压测环境的命名规则' -Because '大小写不同也拒绝'
    Assert-Match -Text (Get-RedisWipeRefusal -Namespace 'mmorpg-stress-a' -Confirmed $false -Pattern $pattern) -Pattern '没给 -ConfirmWipeAllRedis' -Because '缺确认拒绝'
    Assert-Equal -Expected '' -Actual (Get-RedisWipeRefusal -Namespace 'mmorpg-stress-a' -Confirmed $true -Pattern $pattern) -Because '两条都满足才放行'
    $master = ConvertFrom-RedisWipeProbe -Output "role=master`nflushall=OK`ndbsize=0`n"
    Assert-Equal -Expected 'master/OK/0/True' -Actual ('{0}/{1}/{2}/{3}' -f $master.Role, $master.FlushReply, $master.DbSize, $master.Parsed) -Because '主节点的三行都要认出'
    $replica = ConvertFrom-RedisWipeProbe -Output "role=slave`r`ndbsize=12"
    Assert-Equal -Expected 'slave/12/True' -Actual ('{0}/{1}/{2}' -f $replica.Role, $replica.DbSize, $replica.Parsed) -Because '从节点只报 DBSIZE'
    Assert-True -Condition (-not (ConvertFrom-RedisWipeProbe -Output '').Parsed -and -not (ConvertFrom-RedisWipeProbe -Output "role=master`ndbsize=x").Parsed) -Because '认不出的输出按失败处理'
}

# ─────────────────────────────────────────────────────────────────
# 6. 命令入口(DryRun):redis-wipe 的准入在子进程里同样先判;redis-split-status 只打计划
# ─────────────────────────────────────────────────────────────────

Test-Case 'redis-wipe DryRun:非压测 namespace 或缺确认即拒绝;两条都满足只打计划,不 exec' {
    $refused = Invoke-SplitDryRun -Arguments @('-Command', 'redis-wipe', '-DryRun', '-InfraNamespace', 'mmorpg-infra', '-ConfirmWipeAllRedis')
    Assert-True -Condition ($refused.ExitCode -ne 0) -Because "非压测 namespace 必须拒绝。输出: $($refused.Output)"
    Assert-Match -Text $refused.Output -Pattern '拒绝 redis-wipe' -Because '错误写明拒绝原因'
    $noConfirm = Invoke-SplitDryRun -Arguments @('-Command', 'redis-wipe', '-DryRun', '-InfraNamespace', 'mmorpg-stress-contract')
    Assert-True -Condition ($noConfirm.ExitCode -ne 0) -Because "缺 -ConfirmWipeAllRedis 必须拒绝。输出: $($noConfirm.Output)"
    Assert-Match -Text $noConfirm.Output -Pattern '没给 -ConfirmWipeAllRedis' -Because '错误点名缺确认'
    $plan = Invoke-SplitDryRun -Arguments @('-Command', 'redis-wipe', '-DryRun', '-InfraNamespace', 'mmorpg-stress-contract', '-ConfirmWipeAllRedis')
    Assert-Equal -Expected 0 -Actual $plan.ExitCode -Because "准入满足时 DryRun 成功。输出: $($plan.Output)"
    Assert-Match -Text $plan.Output -Pattern '\[dry-run\] redis-wipe would list StatefulSets redis / redis-queue-g<N> / redis-ctl-g<N> / redis-match-cluster / redis-player ' -Because 'DryRun 只打计划;控制域实例与玩家域集群的每个主节点也要清'
    Assert-NotMatch -Text $plan.Output -Pattern '\[dry-run\] kubectl ' -Because 'DryRun 不发任何 kubectl'
}

Test-Case 'redis-split-status DryRun:按 zones 配置逐 zone 打出将要读的 ConfigMap 与 rollout,不连集群' {
    $run = Invoke-SplitDryRun -Arguments @('-Command', 'redis-split-status', '-DryRun', '-ZonesConfigPath', $SplitZonesPath)
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "DryRun 必须成功。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern '\[dry-run\] would read ConfigMaps go-svc-login-config / java-svc-gateway-config and rollout status of statefulset/login / statefulset/gateway in namespace mmorpg-zone-split-test' -Because '对每个 zone 打出要读的对象;login、gateway 是 SQL 写者,线上是 StatefulSet'
    Assert-Match -Text $run.Output -Pattern '\[dry-run\] would also read statefulset, proxy deployment and service of every redis-queue-g<N> those ConfigMaps point at' -Because '实例的形态与就绪状态同样只打计划'
    Assert-Match -Text $run.Output -Pattern '\[dry-run\] would read ConfigMaps go-svc-scene-manager-config / node-config and rollout status of deployment/scene-manager in namespace mmorpg-zone-split-test, expecting control Redis redis-ctl-g0\.[a-z0-9-]+:6379' `
        -Because 'the control domain is checked for every zone, independent of -RedisSplit'
    Assert-Match -Text $run.Output -Pattern '\[dry-run\] would also read statefulset, proxy deployment and service of the redis-ctl-g<N> of every zone group' -Because 'control instances are only planned in DryRun'
}

# ─────────────────────────────────────────────────────────────────
# 7. dev_tools.ps1 的透传口径(进程内,k8s_deploy.ps1 由临时目录里的假脚本顶替)
# ─────────────────────────────────────────────────────────────────

$devToolsAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'dev_tools.ps1'), [ref]$null, [ref]$null)
function Invoke-DevToolsDeployWith {
    param([Parameter(Mandatory = $true)][hashtable]$Variables)
    $fakeDir = Join-Path $TempRoot ('devtools-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $fakeDir | Out-Null
    Set-Content -LiteralPath (Join-Path $fakeDir 'k8s_deploy.ps1') -Encoding utf8 -Value @(
        'param($RedisSplit = "<unset>", [switch]$ConfirmRedisSplitSwitch, $RedisQueueMaxMemory = "<unset>", [switch]$ConfirmWipeAllRedis)',
        '"RedisSplit=$RedisSplit"',
        '"ConfirmRedisSplitSwitch=$($PSBoundParameters.ContainsKey(''ConfirmRedisSplitSwitch''))"',
        '"RedisQueueMaxMemory=$RedisQueueMaxMemory"',
        '"ConfirmWipeAllRedis=$($PSBoundParameters.ContainsKey(''ConfirmWipeAllRedis''))"'
    )
    $ScriptDir = $fakeDir
    foreach ($variableName in $Variables.Keys) { Set-Variable -Name $variableName -Value $Variables[$variableName] }
    $definition = $devToolsAst.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Invoke-K8sDeploy' }, $true)
    . ([scriptblock]::Create($definition.Extent.Text))
    return (Invoke-K8sDeploy -K8sCommand 'zone-up' | Out-String)
}

Test-Case 'dev_tools.ps1 Invoke-K8sDeploy:-RedisSplit auto 不透传,none / queue 原样透传;确认开关与容量给了才透传' {
    $auto = Invoke-DevToolsDeployWith -Variables @{ RedisSplit = 'auto' }
    Assert-Match -Text $auto -Pattern 'RedisSplit=<unset>' -Because 'auto 是 k8s_deploy.ps1 的缺省,不透传'
    Assert-Match -Text $auto -Pattern 'ConfirmRedisSplitSwitch=False' -Because '没给确认就不透传'
    Assert-Match -Text $auto -Pattern 'RedisQueueMaxMemory=<unset>' -Because '容量留空不透传'
    Assert-Match -Text $auto -Pattern 'ConfirmWipeAllRedis=False' -Because '没给 wipe 确认就不透传'
    $queue = Invoke-DevToolsDeployWith -Variables @{ RedisSplit = 'queue'; ConfirmRedisSplitSwitch = $true; RedisQueueMaxMemory = '1gb'; ConfirmWipeAllRedis = $true }
    Assert-Match -Text $queue -Pattern 'RedisSplit=queue' -Because 'queue 原样透传'
    Assert-Match -Text $queue -Pattern 'ConfirmRedisSplitSwitch=True' -Because '确认开关透传'
    Assert-Match -Text $queue -Pattern 'RedisQueueMaxMemory=1gb' -Because '容量透传'
    Assert-Match -Text $queue -Pattern 'ConfirmWipeAllRedis=True' -Because 'wipe 确认透传'
    $none = Invoke-DevToolsDeployWith -Variables @{ RedisSplit = 'none' }
    Assert-Match -Text $none -Pattern 'RedisSplit=none' -Because 'none 也要透传(显式回到不拆,由 k8s_deploy 的切换闸把关)'
}

# ─────────────────────────────────────────────────────────────────
# 8. 高可用形态(-RedisReplicas 3,redis-purpose-ha.yaml;§25.3 第 18 条子项 1 与 2026-11-01 落地说明)
# ─────────────────────────────────────────────────────────────────

$InfraManifestDir = Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/infra'
function Read-InfraManifestText {
    param([Parameter(Mandatory = $true)][string]$Name)
    return ([IO.File]::ReadAllText((Join-Path $InfraManifestDir $Name)) -replace "`r`n", "`n")
}
$queueHaTemplate = Read-InfraManifestText -Name 'redis-purpose-ha.yaml'
$queueSingleTemplate = Read-InfraManifestText -Name 'redis-purpose.yaml'
$coreHaManifest = Read-InfraManifestText -Name 'redis-ha.yaml'
$coreSingleManifest = Read-InfraManifestText -Name 'redis.yaml'

# 清单文本切成文档(模板原文与 DryRun 打出来的块都一样切)。
function Split-ManifestDocs {
    param([Parameter(Mandatory = $true)][string]$Text)
    return @(($Text -replace "`r`n", "`n") -split '(?m)^---[ \t]*$')
}

# 恰好一份 kind + metadata.name 的文档,否则 throw(fail-closed:断言不能落在空文本上)。
function Select-ManifestDoc {
    param(
        [Parameter(Mandatory = $true)][string]$Text,
        [Parameter(Mandatory = $true)][string]$Kind,
        [Parameter(Mandatory = $true)][string]$Name
    )
    $kindPattern = '(?m)^kind:[ \t]*' + $Kind + '[ \t]*$'
    $namePattern = '(?m)^metadata:[ \t]*\n  name:[ \t]*' + [regex]::Escape($Name) + '[ \t]*$'
    $found = @(Split-ManifestDocs -Text $Text | Where-Object { $_ -cmatch $kindPattern -and $_ -cmatch $namePattern })
    if ($found.Count -ne 1) { throw "清单里 $Kind/$Name 应当恰好 1 份,实际 $($found.Count) 份" }
    return $found[0]
}

# 生效行:去掉空行与整行注释。
function Get-EffectiveLines {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    return @(($Text -replace "`r`n", "`n") -split "`n" | Where-Object { $_.Trim() -ne '' -and -not $_.TrimStart().StartsWith('#') })
}

# 清单里每份文档的 <kind>/<metadata.name>。
function Get-ManifestObjectKeys {
    param([Parameter(Mandatory = $true)][string]$Text)
    $keys = foreach ($doc in (Split-ManifestDocs -Text $Text)) {
        $kind = [regex]::Match($doc, '(?m)^kind:[ \t]*(\S+)[ \t]*$')
        $name = [regex]::Match($doc, '(?m)^metadata:[ \t]*\n  name:[ \t]*(\S+)[ \t]*$')
        if ($kind.Success -and $name.Success) { '{0}/{1}' -f $kind.Groups[1].Value, $name.Groups[1].Value }
    }
    return @($keys)
}

# 多个 zone 的 zones 配置,组号由 -Groups 逐个给;gateNodePortBase 显式写(多 zone 的 all-up 预检要求)。
function New-TempGroupedZonesJson {
    param([Parameter(Mandatory = $true)][string]$Name, [Parameter(Mandatory = $true)][int[]]$Groups)
    $zones = for ($i = 0; $i -lt $Groups.Count; $i++) {
        [ordered]@{ name = "split-$i"; zoneId = 101 + $i; gateNodePortBase = 30000 + 10 * $i; redisQueueGroup = $Groups[$i] }
    }
    $path = Join-Path $TempRoot "$Name.json"
    Set-Content -LiteralPath $path -Encoding utf8 -Value (@{ zones = @($zones) } | ConvertTo-Json -Depth 5)
    return $path
}
$TwoGroupsZonesPath = New-TempGroupedZonesJson -Name 'two-groups' -Groups @(0, 1)

# 读写集群的替身:线上 StatefulSet 副本数取 $script:QueueLive,写操作记进 $script:QueueKubectlCalls,Invoke-ClientEntryKubectl 的回复由 $script:KubectlResponder 给。
$script:QueueLive = @{}
$script:QueueKubectlCalls = @()
$script:KubectlResponder = $null
function Get-LiveStatefulSetReplicas {
    param([Parameter(Mandatory = $true)][string]$Namespace, [Parameter(Mandatory = $true)][string]$Name)
    if ($script:QueueLive.ContainsKey($Name)) { return $script:QueueLive[$Name] }
    return $null
}
function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Args, [switch]$AllowFailure)
    $script:QueueKubectlCalls += ('kubectl ' + ($Args -join ' '))
}
function Invoke-KubectlWithInputFile {
    param([Parameter(Mandatory = $true)][string[]]$Args, [Parameter(Mandatory = $true)][string]$InputContent)
    $script:QueueKubectlCalls += ('kubectl ' + ($Args -join ' ') + ' <<' + $InputContent)
}
function Invoke-ClientEntryKubectl {
    param(
        [Parameter(Mandatory = $true)][string[]]$KubectlArgs,
        [AllowEmptyString()][string]$KubeContext,
        [AllowEmptyString()][string]$KubeConfig,
        [switch]$AllowFailure,
        [switch]$CaptureOutput
    )
    $joined = $KubectlArgs -join ' '
    $script:QueueKubectlCalls += $joined
    return (& $script:KubectlResponder $joined)
}
function New-KubectlReply {
    param([int]$ExitCode = 0, [string]$Stdout = '', [string]$Stderr = '')
    return [pscustomobject]@{ ExitCode = $ExitCode; Stdout = $Stdout; Stderr = $Stderr }
}

Test-Case 'Get-RedisQueueShapeFor / Get-RedisQueueHaObjectNames:形态跟核心的拓扑走,1 = redis-purpose.yaml,3 = redis-purpose-ha.yaml;对象名按实例派生' {
    $single = Get-RedisQueueShapeFor -Topology (Get-RedisTopologyFor -Replicas 1)
    Assert-Equal -Expected '1/False/redis-purpose.yaml' -Actual ('{0}/{1}/{2}' -f $single.Replicas, $single.HighAvailability, $single.Template) -Because '单实例形态'
    $ha = Get-RedisQueueShapeFor -Topology (Get-RedisTopologyFor -Replicas 3)
    Assert-Equal -Expected '3/True/redis-purpose-ha.yaml' -Actual ('{0}/{1}/{2}' -f $ha.Replicas, $ha.HighAvailability, $ha.Template) -Because '高可用形态'
    $names = Get-RedisQueueHaObjectNames -InstanceName 'redis-queue-g5'
    Assert-Equal -Expected 'redis-queue-g5-proxy/redis-queue-g5-proxy-pdb/redis-queue-g5-ha-scripts' -Actual ('{0}/{1}/{2}' -f $names.Proxy, $names.ProxyPdb, $names.ScriptsConfigMap) -Because '对象名只在这一处拼,与模板是同一套约定'
}

Test-Case 'infra-up -RedisSplit queue -RedisReplicas 3:渲染高可用 redis-queue-g0,对象名按实例派生,Service 指向代理,noeviction + maxmemory,无未替换占位,排在核心之后,-WaitReady 等实例与代理' {
    Assert-Equal -Expected 0 -Actual $queueHaInfraRun.ExitCode -Because "高可用形态的 infra-up DryRun 必须成功。输出: $($queueHaInfraRun.Output)"
    $out = $queueHaInfraRun.Output
    Assert-Match -Text $out -Pattern 'Redis topology: replicas=3 manifest=redis-ha\.yaml high_availability=true' -Because '核心与 queue 同一个 -RedisReplicas'
    Assert-Match -Text $out -Pattern 'Redis queue instances: redis-queue-g0 maxmemory=128mb memory=256Mi storage=8Gi replicas=3 high_availability=true \(manifests/infra/redis-purpose-ha\.yaml' -Because '入口打出本次的形态与模板'
    Assert-Match -Text $out -Pattern 'Redis queue HA limits: instance=redis-queue-g0 maxclients=10000 proxy_maxconn=3000 proxies=2\+1 reserved=1000 ' -Because '写路径打出核对过的连接上限(同核心的 Redis HA limits)'
    Assert-NotMatch -Text $out -Pattern '\[dry-run\] kubectl [^\r\n]*delete statefulset redis-queue-g0' -Because 'DryRun 不查线上,一律当全新,不做 orphan 删除'

    $block = Select-ManifestByName -Output $out -Name 'redis-queue-g0'
    Assert-True -Condition ($null -ne $block) -Because 'DryRun 输出里应当有 redis-queue-g0 的清单'
    Assert-NotMatch -Text $block -Pattern '(?-i)__[A-Z][A-Z0-9_]*__' -Because '占位必须全部替换'

    $service = Select-ManifestDoc -Text $block -Kind 'Service' -Name 'redis-queue-g0'
    Assert-Match -Text $service -Pattern '(?m)^  selector:\s*\n    app: redis-queue-g0-proxy\s*$' -Because '客户端入口是同名 Service,高可用时它指向代理'
    Assert-Match -Text $service -Pattern '(?m)^\s+port: 6379\s*$' -Because '端口与单实例相同'
    $headless = Select-ManifestDoc -Text $block -Kind 'Service' -Name 'redis-queue-g0-headless'
    foreach ($pattern in @('(?m)^  clusterIP: None\s*$', '(?m)^  publishNotReadyAddresses: true\s*$', '(?m)^    app: redis-queue-g0\s*$', '(?m)^\s+port: 26379\s*$')) {
        Assert-Match -Text $headless -Pattern $pattern -Because 'headless Service 给复制、哨兵与代理的健康检查寻址,从要在主还没就绪时就能解析到它'
    }
    Assert-Match -Text (Select-ManifestDoc -Text $block -Kind 'PodDisruptionBudget' -Name 'redis-queue-g0-pdb') -Pattern '(?m)^  maxUnavailable: 1\s*$' -Because '一次自愿驱逐最多带走一个 redis Pod'
    Assert-Match -Text (Select-ManifestDoc -Text $block -Kind 'PodDisruptionBudget' -Name 'redis-queue-g0-proxy-pdb') -Pattern '(?m)^  minAvailable: 1\s*$' -Because '至少留一个代理'

    $sts = Select-ManifestDoc -Text $block -Kind 'StatefulSet' -Name 'redis-queue-g0'
    foreach ($pattern in @(
            '(?m)^  serviceName: redis-queue-g0-headless\s*$',
            '(?m)^  replicas: 3\s*$',
            '(?m)^  podManagementPolicy: Parallel\s*$',
            '(?m)^      app: redis-queue-g0\s*$',
            '(?m)^            name: redis-queue-g0-ha-scripts\s*$',
            '(?m)^\s+image: redis:7\.2\s*$',
            '(?m)^\s+- name: redis\s*$',
            '(?m)^\s+- name: sentinel\s*$',
            '(?m)^\s+- name: redis-exporter\s*$',
            '(?m)^\s+storage: 8Gi\s*$',
            '(?m)^\s+requests:\s*\n\s+cpu: 250m\s*\n\s+memory: 256Mi\s*\n\s+limits:\s*\n\s+memory: 256Mi\s*$'
        )) {
        Assert-Match -Text $sts -Pattern $pattern -Because 'StatefulSet 的结构、容器名(redis-wipe 与 maxclients 核对按 -c redis 进来)、卷与容量'
    }
    foreach ($assignment in @('MAXCLIENTS=10000', 'MAXMEMORY=128mb')) {
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($sts, '(?m)^[ \t]*' + [regex]::Escape($assignment) + '[ \t]*$').Count) -Because "$assignment 只在 Pod 模板里定义一次(改它 = 滚动重启并逐个交接主,不是等下一次意外重启才悄悄生效)"
    }
    Assert-NotMatch -Text $sts -Pattern 'redis-pod-template-digest' -Because 'queue 实例不走核心的重启闸(排队数据带 TTL)'

    $scripts = Select-ManifestDoc -Text $block -Kind 'ConfigMap' -Name 'redis-queue-g0-ha-scripts'
    foreach ($line in @(
            'MASTER_NAME=redis-queue-g0',
            'echo "redis-queue-g0-$1.redis-queue-g0-headless.${NS}.svc.${DOMAIN}"',
            "grep '^redis-queue-g0-[0-9]'",
            'tcp-check send SENTINEL\ get-master-addr-by-name\ redis-queue-g0\r\n',
            'tcp-check expect min-recv 32 string redis-queue-g0-${i}.redis-queue-g0-headless.',
            '--appendfsync everysec',
            '--maxclients "$MAXCLIENTS"',
            '--maxmemory "$MAXMEMORY"',
            '--maxmemory-policy noeviction'
        )) {
        Assert-True -Condition ($scripts.Contains($line)) -Because "脚本 ConfigMap 里应当有:$line"
    }

    $proxy = Select-ManifestDoc -Text $block -Kind 'Deployment' -Name 'redis-queue-g0-proxy'
    foreach ($pattern in @('(?m)^  replicas: 2\s*$', '(?m)^\s+maxSurge: 1\s*$', '(?m)^      app: redis-queue-g0-proxy\s*$', '(?m)^\s+image: haproxy:3\.2-', '(?m)^            name: redis-queue-g0-ha-scripts\s*$')) {
        Assert-Match -Text $proxy -Pattern $pattern -Because '代理的副本数、滚动参数、标签、镜像与脚本卷'
    }
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($proxy, '(?m)^[ \t]*PROXY_MAXCONN=3000[ \t]*$').Count) -Because 'PROXY_MAXCONN 只在代理的 Pod 模板里定义一次'
    $digest = Get-RedisHaScriptsDigest -ManifestContent $block -ConfigMapName 'redis-queue-g0-ha-scripts' -ManifestLabel 'redis-purpose-ha.yaml'
    Assert-Match -Text $proxy -Pattern ('mmorpg\.io/redis-ha-scripts-digest: "' + $digest + '"') -Because '脚本一改代理就要滚动:注解是这份 ConfigMap 的摘要'

    $coreAt = $out.IndexOf('mmorpg.io/redis-pod-template-digest')
    $queueAt = $out.IndexOf('name: redis-queue-g0')
    Assert-True -Condition ($coreAt -ge 0 -and $queueAt -gt $coreAt) -Because 'queue 实例排在核心 Redis 之后(Secret redis-auth 已在核心分支建好或删掉)'
    $stsWaitAt = $out.IndexOf('[dry-run] kubectl rollout status statefulset/redis-queue-g0 ')
    $proxyWaitAt = $out.IndexOf('[dry-run] kubectl rollout status deployment/redis-queue-g0-proxy ')
    $checkAt = $out.IndexOf('[dry-run] redis maxclients check skipped (manifest MAXCLIENTS=10000) statefulset=redis-queue-g0')
    Assert-True -Condition ($stsWaitAt -ge 0 -and $proxyWaitAt -gt $stsWaitAt -and $checkAt -gt $proxyWaitAt) -Because '-WaitReady 先等 redis 节点,再等代理(客户端连的是代理),最后核 maxclients'
}

Test-Case 'redis-purpose-ha.yaml 与 redis-ha.yaml 逐行同源:生效行只允许文件头列出的差别(改 redis-ha.yaml 要同步改这里)' {
    # 文件头「与 redis-ha.yaml 的差别」逐条写成表。Core / Queue 是连续的生效行(模板的实例名先归一成 redis),两边各自恰好出现一次,换掉之后其余逐行相等。
    # 连接上限的数值本来就不同,归一成 <n> 不比,取值由 DryRun 用例与上限用例钉住。写死核心名字的错误不会被归一掩盖:下一条用例核对渲染后没有核心的名字。
    $differences = @(
        @{ Why = '哨兵 master 名取实例名'; Core = @('    MASTER_NAME=mymaster'); Queue = @('    MASTER_NAME=redis') },
        @{ Why = '代理问哨兵用的 master 名'; Core = @('        echo ''  tcp-check send SENTINEL\ get-master-addr-by-name\ mymaster\r\n'''); Queue = @('        echo ''  tcp-check send SENTINEL\ get-master-addr-by-name\ redis\r\n''') },
        @{ Why = 'maxmemory 由 Pod 模板注入'; Core = @(); Queue = @('    : "${MAXMEMORY:?MAXMEMORY 未注入(应由 Pod 模板的 command 设置)}"') },
        @{ Why = 'redis-server 多 --appendfsync'; Core = @(); Queue = @('      --appendfsync everysec \') },
        @{ Why = 'redis-server 多 --maxmemory 与淘汰策略'; Core = @(); Queue = @('      --maxmemory "$MAXMEMORY" \', '      --maxmemory-policy noeviction \') },
        @{ Why = 'StatefulSet 上没有重启闸注解'; Core = @('  annotations:', '    mmorpg.io/redis-pod-template-digest: "__REDIS_POD_TEMPLATE_DIGEST__"'); Queue = @() },
        @{ Why = 'MAXMEMORY 写在 Pod 模板里'; Core = @('              export MAXCLIENTS'); Queue = @('              MAXMEMORY=__REDIS_MAXMEMORY__', '              export MAXCLIENTS MAXMEMORY') },
        @{ Why = 'redis 容器写 limits,内存按 maxmemory 渲染'; Core = @('              memory: 512Mi'); Queue = @('              memory: __REDIS_MEM__', '            limits:', '              memory: __REDIS_MEM__') },
        @{ Why = 'PVC 大小来自脚本常量'; Core = @('            storage: 8Gi'); Queue = @('            storage: __REDIS_STORAGE__') }
    )
    $normalize = {
        param([string]$Text)
        $lines = @(Get-EffectiveLines -Text $Text | ForEach-Object { $_ -replace '^(\s*(?:MAXCLIENTS|PROXY_MAXCONN)=)\d+\s*$', '${1}<n>' })
        return ("`n" + ($lines -join "`n") + "`n")
    }
    $normalized = @{ Core = (& $normalize $coreHaManifest); Queue = (& $normalize ($queueHaTemplate.Replace('__REDIS_NAME__', 'redis'))) }
    foreach ($difference in $differences) {
        foreach ($side in @('Core', 'Queue')) {
            $lines = @($difference[$side])
            if ($lines.Count -eq 0) { continue }
            $needle = "`n" + ($lines -join "`n") + "`n"
            $hits = [regex]::Matches($normalized[$side], [regex]::Escape($needle)).Count
            Assert-Equal -Expected 1 -Actual $hits -Because "差别表「$($difference.Why)」的 $side 一侧应当恰好出现一次(清单改了就同步改这张表与文件头):$($lines -join ' | ')"
            $normalized[$side] = $normalized[$side].Replace($needle, "`n")
        }
    }
    $coreLines = $normalized.Core -split "`n"
    $queueLines = $normalized.Queue -split "`n"
    for ($i = 0; $i -lt [Math]::Max($coreLines.Count, $queueLines.Count); $i++) {
        if ($coreLines[$i] -cne $queueLines[$i]) {
            throw "断言失败: redis-purpose-ha.yaml 与 redis-ha.yaml 的生效行不同步(归一后第 $i 行):核心 '$($coreLines[$i])';queue '$($queueLines[$i])'。文件头没列出的差别不允许。"
        }
    }
}

Test-Case '高可用模板没有写死核心的名字;所有对象名由实例名派生,不与核心重名,两组之间也不重名' {
    $rendered = $queueHaTemplate.Replace('__REDIS_NAME__', 'redis-queue-g7')
    $effective = (Get-EffectiveLines -Text $rendered) -join "`n"
    foreach ($case in @(
            @{ Pattern = 'mymaster'; Why = '哨兵 master 名必须取实例名' },
            @{ Pattern = 'redis-headless'; Why = '节点要按本实例的 headless Service 寻址,写死就连到核心的节点上' },
            @{ Pattern = 'redis-proxy'; Why = '代理对象名必须按实例派生' },
            @{ Pattern = 'redis-ha-scripts(?!-digest)'; Why = '脚本 ConfigMap 必须按实例派生(注解键 mmorpg.io/redis-ha-scripts-digest 与核心同名,不算)' },
            @{ Pattern = '(?<![\w-])redis-[0-9]'; Why = 'Pod 名前缀必须取实例名(哨兵答案的过滤、代理的期望字符串)' },
            @{ Pattern = '(?m)^\s*app:\s*redis(-proxy)?\s*$'; Why = '带核心标签的 Pod 会被挂到核心 Service redis 后面' }
        )) {
        Assert-NotMatch -Text $effective -Pattern $case.Pattern -Because $case.Why
    }
    $appValues = @([regex]::Matches($rendered, '(?m)^\s*app:[ \t]*(\S+)[ \t]*$') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'redis-queue-g7,redis-queue-g7-proxy' -Actual ($appValues -join ',') -Because '标签只有实例名与实例名-proxy 两种'

    $keys = @(Get-ManifestObjectKeys -Text $rendered)
    $expected = @('ConfigMap/redis-queue-g7-ha-scripts', 'Deployment/redis-queue-g7-proxy', 'PodDisruptionBudget/redis-queue-g7-pdb', 'PodDisruptionBudget/redis-queue-g7-proxy-pdb', 'Service/redis-queue-g7', 'Service/redis-queue-g7-headless', 'StatefulSet/redis-queue-g7')
    Assert-Equal -Expected (($expected | Sort-Object) -join ',') -Actual (($keys | Sort-Object) -join ',') -Because '高可用实例的对象清单'
    $names = Get-RedisQueueHaObjectNames -InstanceName 'redis-queue-g7'
    foreach ($key in @("Deployment/$($names.Proxy)", "PodDisruptionBudget/$($names.ProxyPdb)", "ConfigMap/$($names.ScriptsConfigMap)")) {
        Assert-True -Condition ($keys -ccontains $key) -Because "脚本里拼的名字($key)必须与模板一致:-WaitReady、redis-split-status 与拒绝文本都用它"
    }
    $coreKeys = @(Get-ManifestObjectKeys -Text $coreHaManifest) + @(Get-ManifestObjectKeys -Text $coreSingleManifest)
    Assert-True -Condition ($coreKeys -ccontains 'StatefulSet/redis' -and $coreKeys -ccontains 'Deployment/redis-proxy') -Because '夹具前提:读得出核心的对象名'
    $otherKeys = @(Get-ManifestObjectKeys -Text ($queueHaTemplate.Replace('__REDIS_NAME__', 'redis-queue-g8')))
    foreach ($key in $keys) {
        Assert-True -Condition ($coreKeys -cnotcontains $key) -Because "$key 与核心重名,apply 会改掉核心的对象"
        Assert-True -Condition ($otherKeys -cnotcontains $key) -Because "$key 在两个组之间重名"
    }
}

Test-Case '单实例形态(dev 档缺省,或显式 -RedisReplicas 1):Service 直接选 redis Pod,没有代理、脚本与哨兵;显式 1 的产物与缺省逐字相同' {
    $out = $queueInfraRun.Output
    Assert-Match -Text $out -Pattern 'Redis queue instances: redis-queue-g0 maxmemory=128mb memory=256Mi storage=8Gi replicas=1 high_availability=false \(manifests/infra/redis-purpose\.yaml' -Because '入口打出本次的形态与模板'
    $block = Select-ManifestByName -Output $out -Name 'redis-queue-g0'
    Assert-Match -Text (Select-ManifestDoc -Text $block -Kind 'Service' -Name 'redis-queue-g0') -Pattern '(?m)^  selector:\s*\n    app: redis-queue-g0\s*$' -Because '单实例时同名 Service 直接选 redis Pod'
    Assert-NotMatch -Text ((Get-EffectiveLines -Text $block) -join "`n") -Pattern 'redis-queue-g0-proxy|ha-scripts|sentinel' -Because '单实例形态没有代理、脚本 ConfigMap 与哨兵'
    Assert-NotMatch -Text $out -Pattern 'rollout status deployment/redis-queue-g0-proxy' -Because '单实例没有代理可等'
    Assert-NotMatch -Text $out -Pattern 'Redis queue HA limits|Redis queue proxy ledger' -Because '单实例不打代理相关的核对'
    $explicit = Invoke-SplitDryRun -Arguments ($InfraArgs + @('-RedisSplit', 'queue', '-RedisReplicas', '1')) -Env $InfraEnv
    Assert-Equal -Expected 0 -Actual $explicit.ExitCode -Because "显式 -RedisReplicas 1 必须成功。输出: $($explicit.Output)"
    Assert-Equal -Expected $block -Actual (Select-ManifestByName -Output $explicit.Output -Name 'redis-queue-g0') -Because '显式 1 与 dev 档缺省是同一份产物'
}

# 与 k8s_deploy_contract.tests.ps1 的 $ProdEnv 同形的 prod 档夹具(都是虚构值,只用于 DryRun)。
$SplitProdEnv = @{
    MMORPG_GATE_TOKEN_SECRET        = 'contract-test-secret-0123456789abcdef0123'
    MMORPG_MYSQL_USER               = 'mmorpg_app'
    MMORPG_MYSQL_PASSWORD           = 'contract-test-mysql-pw-01'
    MMORPG_REDIS_PASSWORD           = 'contract-test-redis-pw-01'
    MMORPG_GATEWAY_DB_USER          = 'gateway_app'
    MMORPG_GATEWAY_DB_PASSWORD      = 'contract-test-gwdb-pw-01'
    MMORPG_INTERNAL_AUTH_SECRET     = 'contract-test-internal-auth-0123456789abcdef'
    MMORPG_DB_ALLOWED_DATABASES     = 'zone_1_db,zone_2_db'
    MMORPG_GATEWAY_ADMIN_API_KEY    = 'contract-test-gw-admin-key-0123456789abcdef'
    MMORPG_LOGIN_QUEUE_TOKEN_SECRET = 'contract-test-queue-token-0123456789abcdef'
    MMORPG_SPECTATE_TOKEN_SECRET    = 'contract-test-spectate-token-0123456789abcdef'
}

Test-Case 'release 档:prod 缺省就是高可用(核心与 queue 同一条档位规则),容量按 -RedisQueueMaxMemory 渲染;显式 -RedisReplicas 1 两者一起回单实例' {
    $prodArgs = $InfraArgs + @('-ReleaseProfile', 'prod', '-BattleReplicas', '0', '-RedisSplit', 'queue', '-RedisQueueMaxMemory', '1gb')
    $run = Invoke-SplitDryRun -Arguments $prodArgs -Env $SplitProdEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "prod 档 infra-up DryRun 必须成功。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'Redis topology: replicas=3 manifest=redis-ha\.yaml high_availability=true' -Because '非 dev 档缺省高可用'
    Assert-Match -Text $run.Output -Pattern 'Redis queue instances: redis-queue-g0 maxmemory=1gb memory=2Gi storage=8Gi replicas=3 high_availability=true \(manifests/infra/redis-purpose-ha\.yaml' -Because 'queue 与核心同一条档位规则'
    $sts = Select-ManifestDoc -Text (Select-ManifestByName -Output $run.Output -Name 'redis-queue-g0') -Kind 'StatefulSet' -Name 'redis-queue-g0'
    Assert-Match -Text $sts -Pattern '(?m)^\s+MAXMEMORY=1gb\s*$' -Because 'maxmemory 写在 Pod 模板里'
    Assert-Match -Text $sts -Pattern '(?m)^\s+requests:\s*\n\s+cpu: 250m\s*\n\s+memory: 2Gi\s*\n\s+limits:\s*\n\s+memory: 2Gi\s*$' -Because '容器内存 = 2 × maxmemory'
    $single = Invoke-SplitDryRun -Arguments ($prodArgs + @('-RedisReplicas', '1')) -Env $SplitProdEnv
    Assert-Equal -Expected 0 -Actual $single.ExitCode -Because "prod 档显式单实例的 DryRun 必须成功。输出: $($single.Output)"
    Assert-Match -Text $single.Output -Pattern 'Redis topology: replicas=1 manifest=redis\.yaml' -Because '显式 -RedisReplicas 1 覆盖档位缺省'
    Assert-Match -Text $single.Output -Pattern 'Redis queue instances: redis-queue-g0 maxmemory=1gb memory=2Gi storage=8Gi replicas=1 high_availability=false \(manifests/infra/redis-purpose\.yaml' -Because 'queue 跟着回单实例'
}

Test-Case 'zones 配置出现多个组:每组各渲染一份高可用实例,脚本摘要各不相同,逐个等就绪;all-up 同样先 apply 实例再部署 zone' {
    $run = Invoke-SplitDryRun -Arguments ($InfraArgs + @('-RedisSplit', 'queue', '-RedisReplicas', '3', '-ZonesConfigPath', $TwoGroupsZonesPath)) -Env $InfraEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "两组的 infra-up DryRun 必须成功。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'Redis queue instances: redis-queue-g0,redis-queue-g1 maxmemory=128mb ' -Because '每个出现过的组一份实例'
    $digests = foreach ($instance in @('redis-queue-g0', 'redis-queue-g1')) {
        $block = Select-ManifestByName -Output $run.Output -Name $instance
        Assert-True -Condition ($null -ne $block) -Because "DryRun 输出里应当有 $instance 的清单"
        $digest = Get-RedisHaScriptsDigest -ManifestContent $block -ConfigMapName "$instance-ha-scripts" -ManifestLabel 'redis-purpose-ha.yaml'
        Assert-Match -Text (Select-ManifestDoc -Text $block -Kind 'Deployment' -Name "$instance-proxy") -Pattern ('mmorpg\.io/redis-ha-scripts-digest: "' + $digest + '"') -Because "$instance 的代理带自己那份脚本的摘要"
        Assert-Match -Text $run.Output -Pattern "rollout status statefulset/$instance " -Because "-WaitReady 等 $instance"
        Assert-Match -Text $run.Output -Pattern "rollout status deployment/$instance-proxy " -Because "-WaitReady 等 $instance 的代理"
        Assert-Match -Text $run.Output -Pattern "Redis queue HA limits: instance=$instance " -Because "$instance 的连接上限逐个核对"
        Assert-Match -Text $run.Output -Pattern "Redis queue proxy ledger: instance=$instance " -Because "$instance 的代理水位逐个打"
        $digest
    }
    Assert-True -Condition (@($digests | Sort-Object -Unique).Count -eq 2) -Because '脚本里写着实例名,两组的摘要不能相同'

    $allUp = Invoke-SplitDryRun -Arguments @('-Command', 'all-up', '-DryRun', '-SkipPreflight', '-SkipGoSvc', '-SkipJavaSvc', '-WaitReady', '-InfraNamespace', 'split-infra',
        '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab', '-ZonesConfigPath', $TwoGroupsZonesPath, '-RedisSplit', 'queue', '-RedisReplicas', '3') -Env $InfraEnv
    Assert-Equal -Expected 0 -Actual $allUp.ExitCode -Because "带 infra 的 all-up DryRun 必须成功。输出: $($allUp.Output)"
    $queueAt = $allUp.Output.IndexOf('name: redis-queue-g1')
    $zoneAt = $allUp.Output.IndexOf('name: node-config')
    Assert-True -Condition ($queueAt -ge 0 -and $zoneAt -gt $queueAt) -Because 'all-up 先 Apply-Infra(含 queue 实例)再逐 zone 部署'
}

Test-Case '地址不随形态变:单实例与高可用下 login / gateway 的排队地址、核心地址、Redis split 与 queue 总账都逐字相同' {
    Assert-Equal -Expected 0 -Actual $queueHaZoneRun.ExitCode -Because "高可用形态的 zone-up DryRun 必须成功。输出: $($queueHaZoneRun.Output)"
    foreach ($key in @(
            @{ ConfigMap = 'go-svc-login-config'; Path = 'data.login.yaml.Node.QueueRedis.Host'; Expected = "redis-queue-g0.${Infra}:6379" },
            @{ ConfigMap = 'go-svc-login-config'; Path = 'data.login.yaml.Node.RedisClient.Host'; Expected = "redis.${Infra}:6379" },
            @{ ConfigMap = 'java-svc-gateway-config'; Path = 'data.application.yaml.spring.data.redis.host'; Expected = "redis-queue-g0.$Infra" },
            @{ ConfigMap = 'java-svc-gateway-config'; Path = 'data.application.yaml.spring.data.redis.port'; Expected = '6379' }
        )) {
        foreach ($run in @($queueZoneRun, $queueHaZoneRun)) {
            Assert-Equal -Expected $key.Expected -Actual (Get-ConfigValue -Output $run.Output -ConfigMap $key.ConfigMap -KeyPath $key.Path) -Because "$($key.ConfigMap) 的 $($key.Path) 不随形态变(换形态不触发 -ConfirmRedisSplitSwitch)"
        }
    }
    foreach ($linePattern in @('Redis split: zone=split-test [^\r\n]*', 'Redis queue connection ledger: instance=redis-queue-g0 [^\r\n]*')) {
        $singleLine = [regex]::Match($queueZoneRun.Output, $linePattern).Value
        Assert-True -Condition ($singleLine -ne '') -Because "夹具前提:输出里有 /$linePattern/"
        Assert-Equal -Expected $singleLine -Actual ([regex]::Match($queueHaZoneRun.Output, $linePattern).Value) -Because '两种形态共用同一个地址与同一份账'
    }
}

Test-Case '两种形态可以原地切换:同名 Service / headless / PDB / StatefulSet,同一个 governing service、selector、卷模板与 MAXCLIENTS' {
    $name = 'redis-queue-g0'
    $single = $queueSingleTemplate.Replace('__REDIS_NAME__', $name)
    $ha = $queueHaTemplate.Replace('__REDIS_NAME__', $name)
    $haKeys = @(Get-ManifestObjectKeys -Text $ha)
    foreach ($key in @(Get-ManifestObjectKeys -Text $single)) {
        Assert-True -Condition ($haKeys -ccontains $key) -Because "单实例的 $key 在高可用模板里必须同名:apply 才是原地改,而不是多出一个对象"
    }
    $singleSts = Select-ManifestDoc -Text $single -Kind 'StatefulSet' -Name $name
    $haSts = Select-ManifestDoc -Text $ha -Kind 'StatefulSet' -Name $name
    foreach ($pattern in @("(?m)^  serviceName: $name-headless\s*$", "(?m)^    matchLabels:\s*\n      app: $name\s*$", '(?m)^        - name: redis\s*$')) {
        Assert-Match -Text $singleSts -Pattern $pattern -Because '单实例一侧:新 StatefulSet 要按同名同标签接管孤儿 Pod'
        Assert-Match -Text $haSts -Pattern $pattern -Because '高可用一侧:新 StatefulSet 要按同名同标签接管孤儿 Pod'
    }
    Assert-NotMatch -Text $singleSts -Pattern 'podManagementPolicy' -Because '单实例是缺省的 OrderedReady'
    Assert-Match -Text $haSts -Pattern '(?m)^  podManagementPolicy: Parallel\s*$' -Because '这个字段创建后不可改,所以 1 → 3 要先 orphan 删除旧 StatefulSet'
    $volumeClaims = {
        param([string]$Doc)
        return ((Get-EffectiveLines -Text ($Doc.Substring($Doc.IndexOf('  volumeClaimTemplates:')))) -join "`n")
    }
    Assert-Equal -Expected (& $volumeClaims $singleSts) -Actual (& $volumeClaims $haSts) -Because '卷模板相同,<实例名>-0 才接着用原来那块 PVC'
    Assert-Equal -Expected (Get-RedisMaxClientsFromManifest -ManifestContent $single) -Actual (Get-RedisMaxClientsFromManifest -ManifestContent $ha) -Because '连接总账只读 redis-purpose.yaml,两种形态的 MAXCLIENTS 必须同值'
}

Test-Case 'Get-RedisQueueHaLimitsFor:代理全部副本(含 maxSurge)+ 预留 ≤ MAXCLIENTS,且 MAXCLIENTS 与 redis-purpose.yaml 同值;不成立与代理名对不上都拒绝' {
    $RepoRoot = Get-RepoRoot
    $singleMax = Get-RedisMaxClientsFromManifest -RelativePath 'deploy/k8s/manifests/infra/redis-purpose.yaml'
    $rendered = $queueHaTemplate.Replace('__REDIS_NAME__', 'redis-queue-g3')
    $limits = Get-RedisQueueHaLimitsFor -ManifestContent $rendered -InstanceName 'redis-queue-g3' -SingleInstanceMaxClients $singleMax
    Assert-Equal -Expected "$singleMax/2/1/1000" -Actual ('{0}/{1}/{2}/{3}' -f $limits.MaxClients, $limits.ProxyReplicas, $limits.ProxyMaxSurge, $limits.Reserved) -Because '上限读自模板'
    Assert-True -Condition ($limits.ProxyTotal + $limits.Reserved -le $limits.MaxClients) -Because '代理必须比 Redis 先到线:Redis 到线时代理的健康检查也被拒,判下线后会踢掉全部会话'

    $message = ''
    try { $null = Get-RedisQueueHaLimitsFor -ManifestContent $rendered -InstanceName 'redis-queue-g3' -SingleInstanceMaxClients ($singleMax + 1) } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'redis-purpose-ha\.yaml 的 MAXCLIENTS=\d+ 与 redis-purpose\.yaml 的 \d+ 不同' -Because '两种形态共用一份账,MAXCLIENTS 不同必须拒绝,并点名两个模板'

    $tooMany = $rendered -replace '(?m)^([ \t]*PROXY_MAXCONN=)\d+', ('${1}' + $limits.MaxClients)
    $message = ''
    try { $null = Get-RedisQueueHaLimitsFor -ManifestContent $tooMany -InstanceName 'redis-queue-g3' -SingleInstanceMaxClients $singleMax } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'PROXY_MAXCONN=\d+.*超过 MAXCLIENTS=\d+' -Because '代理上限加起来超过 MAXCLIENTS 必须拒绝'

    $message = ''
    try { $null = Get-RedisQueueHaLimitsFor -ManifestContent $rendered -InstanceName 'redis-queue-g4' -SingleInstanceMaxClients $singleMax } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'redis-queue-g4-proxy' -Because '代理名按实例派生,模板里找不到就拒绝,不悄悄跳过核对'
}

Test-Case '总账:高可用形态每组多打一行 Redis queue proxy ledger(单个代理扛全部客户端的水位),规划档不越线不告警;单实例不打;核心的代理总账照打' {
    $out = $queueHaZoneRun.Output
    $m = [regex]::Match($out, 'Redis queue proxy ledger: instance=redis-queue-g0 proxy_maxconn=(?<maxconn>\d+) proxies=(?<replicas>\d+)\+(?<surge>\d+) warn_at=(?<warn>\d+) [^\r\n]*deploy peak=(?<deploy>\d+) plan peak=(?<plan>\d+)')
    Assert-True -Condition $m.Success -Because "高可用形态要打代理水位。输出: $out"
    $RepoRoot = Get-RepoRoot
    $limits = Get-RedisQueueHaLimitsFor -ManifestContent ($queueHaTemplate.Replace('__REDIS_NAME__', 'redis-queue-g0')) -InstanceName 'redis-queue-g0' -SingleInstanceMaxClients (Get-RedisMaxClientsFromManifest -RelativePath 'deploy/k8s/manifests/infra/redis-purpose.yaml')
    Assert-Equal -Expected ('{0}/{1}/{2}' -f $limits.ProxyMaxConn, $limits.ProxyReplicas, $limits.ProxyMaxSurge) -Actual ('{0}/{1}/{2}' -f $m.Groups['maxconn'].Value, $m.Groups['replicas'].Value, $m.Groups['surge'].Value) -Because '总账读的是模板里的上限'
    $warnAt = [int][Math]::Floor($limits.ProxyMaxConn * (Get-RedisConnectionPlan).WarnRatio)
    Assert-Equal -Expected $warnAt -Actual $m.Groups['warn'].Value -Because '水位 = WarnRatio × 单个代理的 PROXY_MAXCONN'
    Assert-True -Condition ([int]$m.Groups['deploy'].Value -le $warnAt -and [int]$m.Groups['plan'].Value -le $warnAt) -Because ('规划档或本次峰值超过单个代理水位 {0}:另一个代理重启期间,剩下的那个会先到线' -f $warnAt)
    Assert-NotMatch -Text $out -Pattern '\[RedisQueueConnectionLedger\]OverProxyWarnRatio:' -Because '都在水位以内,不告警'
    Assert-Match -Text $out -Pattern 'Redis proxy ledger: proxy_maxconn=' -Because '核心的代理总账照打,不被 queue 那行顶掉'
    foreach ($run in @($queueZoneRun, $defaultZoneRun)) {
        Assert-NotMatch -Text $run.Output -Pattern 'Redis queue proxy ledger:' -Because '单实例或不拆时没有 queue 的代理'
    }
}

# 总账的告警路径:客户端输入换成替身,把池上限调到单个代理扛不下、MAXCLIENTS 的水位还没到。算式与判定是 k8s_deploy.ps1 的同一份函数体。
function Get-RedisQueueConnectionLedgerInputs {
    return [ordered]@{
        MaxClients               = Get-RedisMaxClientsFromManifest -ManifestContent $queueSingleTemplate
        GoPoolSizePerProc        = 10
        CppConnectionsPerProcess = 1
        Clients                  = @([ordered]@{ Name = 'login'; Scope = 'zone'; Replicas = 2; ConfiguredPoolConns = 1500; DefaultPools = 0; GoMaxProcs = 0; PubSub = 0; Fixed = 0 })
    }
}

Test-Case '总账告警路径:单个代理扛不下时告警、不阻断,本次与规划档分开点名;MAXCLIENTS 水位没到不打那一条;单实例不看代理水位' {
    $RepoRoot = Get-RepoRoot
    $plan = [ordered]@{ ZoneCount = 1; RolloutExtraPodsPerDeployment = 2; OpsReserve = 50; WarnRatio = 0.8 }
    $topologyByZone = @{ 101 = (Get-RedisPurposeTopologyFor -Split 'queue' -InfraNamespace 'ns' -QueueGroup 0) }
    $runLedger = {
        param([int]$Replicas)
        $RedisReplicas = $Replicas
        return ((@(Write-RedisQueueConnectionLedger -TopologyByZone $topologyByZone -LedgerPlan $plan 3>&1 6>&1) | ForEach-Object { $_.ToString() }) -join "`n")
    }
    # 1 个 zone:稳态 2 × 1500 = 3000,滚动余量 2 × 1500 = 3000,运维 50,峰值 6050。单个代理水位 3000 × 0.8 = 2400,MAXCLIENTS 水位 8000。
    $ha = & $runLedger 3
    Assert-Match -Text $ha -Pattern 'Redis queue proxy ledger: instance=redis-queue-g0 proxy_maxconn=3000 proxies=2\+1 warn_at=2400 [^\r\n]*deploy peak=6050 plan peak=6050' -Because '高可用每组打代理水位'
    Assert-Match -Text $ha -Pattern '\[RedisQueueConnectionLedger\]OverProxyWarnRatio:redis-queue-g0 zones=1 peak 6050; plan zones=1 in one group peak 6050; proxy_maxconn=3000 warn_at=2400 ' -Because '超过单个代理的水位要告警,本次与规划档都点名'
    Assert-NotMatch -Text $ha -Pattern '\[RedisQueueConnectionLedger\]OverWarnRatio:' -Because '峰值没到 MAXCLIENTS 的水位,不打那一条'
    $single = & $runLedger 1
    Assert-Match -Text $single -Pattern 'Redis queue connection ledger: instance=redis-queue-g0 zones=1 steady=3000 peak=6050 maxclients=10000 warn_at=8000 headroom=3950' -Because '两种形态共用同一份账'
    Assert-NotMatch -Text $single -Pattern 'Redis queue proxy ledger|OverProxyWarnRatio' -Because '单实例没有代理'
}

Test-Case 'ConvertTo-RedisQueueInstanceManifest:替换全部占位;脚本一改摘要就变;多出没人替换的占位拒绝;高可用缺 redis-purpose.yaml 的上限拒绝' {
    $haShape = Get-RedisQueueShapeFor -Topology (Get-RedisTopologyFor -Replicas 3)
    $singleShape = Get-RedisQueueShapeFor -Topology (Get-RedisTopologyFor -Replicas 1)
    $memory = [pscustomobject]@{ MaxMemory = '1gb'; ContainerMemory = '2Gi' }
    $singleMax = Get-RedisMaxClientsFromManifest -ManifestContent $queueSingleTemplate
    $render = {
        param([string]$Template, $Shape)
        return ConvertTo-RedisQueueInstanceManifest -Template $Template -Shape $Shape -InstanceName 'redis-queue-g0' -InfraNamespace 'ns' -Memory $memory -Storage '8Gi' -SingleInstanceMaxClients $singleMax
    }
    $ha = & $render $queueHaTemplate $haShape
    Assert-Equal -Expected 'redis-queue-g0/3/True/redis-purpose-ha.yaml/1gb' -Actual ('{0}/{1}/{2}/{3}/{4}' -f $ha.Name, $ha.Replicas, $ha.HighAvailability, $ha.Template, $ha.MaxMemory) -Because '高可用的渲染结果'
    Assert-Equal -Expected $singleMax -Actual $ha.HaLimits.MaxClients -Because '高可用时带上核对过的上限'
    Assert-NotMatch -Text $ha.Content -Pattern '(?-i)__[A-Z][A-Z0-9_]*__' -Because '占位全部替换'
    $single = & $render $queueSingleTemplate $singleShape
    Assert-Equal -Expected 'redis-queue-g0/1/False/redis-purpose.yaml' -Actual ('{0}/{1}/{2}/{3}' -f $single.Name, $single.Replicas, $single.HighAvailability, $single.Template) -Because '单实例的渲染结果'
    Assert-True -Condition ($null -eq $single.HaLimits) -Because '单实例没有代理上限'
    Assert-NotMatch -Text $single.Content -Pattern '(?-i)__[A-Z][A-Z0-9_]*__' -Because '占位全部替换'

    $digestOf = { param([string]$Content) return [regex]::Match($Content, 'mmorpg\.io/redis-ha-scripts-digest: "([0-9a-f]{16})"').Groups[1].Value }
    $changed = & $render ($queueHaTemplate.Replace('hard-stop-after 10s', 'hard-stop-after 11s')) $haShape
    Assert-True -Condition ((& $digestOf $ha.Content) -ne '' -and (& $digestOf $ha.Content) -ne (& $digestOf $changed.Content)) -Because '脚本一改,代理 Pod 模板上的摘要就变,Deployment 才会滚动'

    $message = ''
    try { $null = & $render ($queueHaTemplate + "`n# __REDIS_UNKNOWN__`n") $haShape } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern '未替换的占位:__REDIS_UNKNOWN__' -Because '模板多了没人替换的占位必须拒绝 apply'

    $message = ''
    try { $null = ConvertTo-RedisQueueInstanceManifest -Template $queueHaTemplate -Shape $haShape -InstanceName 'redis-queue-g0' -InfraNamespace 'ns' -Memory $memory -Storage '8Gi' } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'MAXCLIENTS' -Because '高可用必须拿到 redis-purpose.yaml 的 MAXCLIENTS 才能核对,不给就拒绝'
}

# 形态变更闸:线上副本数由 Get-LiveStatefulSetReplicas 的替身给;告警与返回值分开取。
function Invoke-QueueTopologyGate {
    param([AllowNull()]$Live, [Parameter(Mandatory = $true)][int]$Desired)
    $InfraNamespace = 'ns'
    $script:QueueLive = @{}
    if ($null -ne $Live) { $script:QueueLive['redis-queue-g0'] = $Live }
    $records = @()
    $message = ''
    try { $records = @(Assert-RedisQueueTopologyChangeIsSafe -InstanceName 'redis-queue-g0' -DesiredReplicas $Desired 3>&1) } catch { $message = $_.Exception.Message }
    return [pscustomobject]@{
        Verdict  = (@($records | Where-Object { $_ -isnot [System.Management.Automation.WarningRecord] }) -join ',')
        Warnings = (@($records | Where-Object { $_ -is [System.Management.Automation.WarningRecord] } | ForEach-Object { $_.Message }) -join "`n")
        Error    = $message
    }
}

Test-Case '形态变更闸:全新与不变放行;1 → 3 告警后判 upgrade;3 → 1 与线上 0 / 2 在任何写操作之前拒绝,写明手工步骤' {
    foreach ($case in @(
            @{ Live = $null; Desired = 1; Verdict = 'absent' },
            @{ Live = $null; Desired = 3; Verdict = 'absent' },
            @{ Live = 1; Desired = 1; Verdict = 'unchanged' },
            @{ Live = 3; Desired = 3; Verdict = 'unchanged' }
        )) {
        $r = Invoke-QueueTopologyGate -Live $case.Live -Desired $case.Desired
        Assert-Equal -Expected $case.Verdict -Actual $r.Verdict -Because "线上 $($case.Live) → 目标 $($case.Desired)"
        Assert-Equal -Expected '' -Actual $r.Warnings -Because "线上 $($case.Live) → 目标 $($case.Desired) 不告警"
    }
    $upgrade = Invoke-QueueTopologyGate -Live 1 -Desired 3
    Assert-Equal -Expected 'upgrade' -Actual $upgrade.Verdict -Because '1 → 3 原地切高可用'
    Assert-Match -Text $upgrade.Warnings -Pattern '\[RedisQueueTopology\]Upgrade:' -Because '放行的形态变更必须告警'
    Assert-NotMatch -Text $upgrade.Warnings -Pattern 're-run with -RedisReplicas 1' -Because '告警之后本次照样继续;核心是 3 时传 1 只会被核心的闸拒绝,不能这样建议'

    $downgrade = Invoke-QueueTopologyGate -Live 3 -Desired 1
    foreach ($pattern in @('拒绝变更排队 Redis 实例 redis-queue-g0', '本次未做任何写操作', '-LoginQueue off', 'statefulset/redis-queue-g0', 'deployment/redis-queue-g0-proxy', 'pdb/redis-queue-g0-proxy-pdb', 'configmap/redis-queue-g0-ha-scripts', 'redis-data-redis-queue-g0-0', '带 -RedisReplicas 1 重跑', '显式传 -RedisReplicas 3')) {
        Assert-Match -Text $downgrade.Error -Pattern $pattern -Because "3 → 1 的拒绝文本要写明:$pattern"
    }
    foreach ($case in @(@{ Live = 0; Desired = 1 }, @{ Live = 2; Desired = 3 })) {
        $r = Invoke-QueueTopologyGate -Live $case.Live -Desired $case.Desired
        Assert-Match -Text $r.Error -Pattern '拒绝变更排队 Redis 实例' -Because "线上 $($case.Live) 是本脚本从不产生的副本数"
        Assert-Match -Text $r.Error -Pattern 'scale statefulset/redis-queue-g0' -Because '只给手工整理的步骤,不给传不进去的参数建议'
        Assert-NotMatch -Text $r.Error -Pattern '显式传 -RedisReplicas' -Because '参数只认 0 / 1 / 3,不能建议传线上值'
    }
}

Test-Case 'Apply-RedisQueueInstances:只有 upgrade 先 orphan 删除旧 StatefulSet 再 apply;其余只 apply;不删 Service、PVC 与别的对象' {
    $InfraNamespace = 'ns'
    $script:QueueKubectlCalls = @()
    $newManifest = {
        param([string]$Name, [string]$Verdict)
        return [pscustomobject]@{ Name = $Name; Replicas = 3; HighAvailability = $true; Template = 'redis-purpose-ha.yaml'; MaxMemory = '1gb'; Content = "content-of-$Name"; Verdict = $Verdict }
    }
    Apply-RedisQueueInstances -Manifests @((& $newManifest 'redis-queue-g0' 'upgrade'), (& $newManifest 'redis-queue-g1' 'unchanged'), (& $newManifest 'redis-queue-g2' 'absent'))
    $expected = @(
        'kubectl delete statefulset redis-queue-g0 -n ns --cascade=orphan --ignore-not-found',
        'kubectl apply -n ns <<content-of-redis-queue-g0',
        'kubectl apply -n ns <<content-of-redis-queue-g1',
        'kubectl apply -n ns <<content-of-redis-queue-g2'
    )
    Assert-Equal -Expected ($expected -join ' | ') -Actual ($script:QueueKubectlCalls -join ' | ') -Because 'podManagementPolicy 创建后不可改,所以 upgrade 要先只删对象、留下 Pod 与 PVC,紧接着 apply'
}

# 渲染入口:被测函数按动态作用域读这些脚本级参数(同名参数在 k8s_deploy.ps1 里的含义)。
function Invoke-QueueManifestRender {
    param(
        [Parameter(Mandatory = $true)][int]$Replicas,
        [hashtable]$Live = @{},
        [string]$ManifestsDir = $InfraManifestDir,
        [string]$Password = ''
    )
    $RedisSplit = 'queue'; $RedisReplicas = $Replicas; $ReleaseProfile = 'dev'; $OpsProfile = 'custom'; $RedisQueueMaxMemory = ''
    $InfraNamespace = 'ns'; $ZonesConfigPath = $TwoGroupsZonesPath; $InfraManifestsDir = $ManifestsDir; $RepoRoot = Get-RepoRoot
    $CentreReplicas = 1; $GateReplicas = 2; $SceneReplicas = 4; $SceneWorldReplicas = -1; $SceneInstanceReplicas = -1; $GateNodePortBase = 30000
    $script:RedisPassword = $Password
    $script:RedisQueueMemory = $null
    $script:QueueLive = $Live
    $script:QueueKubectlCalls = @()
    $records = @()
    $message = ''
    try { $records = @(New-RedisQueueInstanceManifests 3>&1 6>$null) } catch { $message = $_.Exception.Message }
    return [pscustomobject]@{
        Manifests = @($records | Where-Object { $_ -isnot [System.Management.Automation.WarningRecord] })
        Warnings  = (@($records | Where-Object { $_ -is [System.Management.Automation.WarningRecord] } | ForEach-Object { $_.Message }) -join "`n")
        Error     = $message
        Writes    = @($script:QueueKubectlCalls)
    }
}

Test-Case 'New-RedisQueueInstanceManifests:每组各判一次形态;任何一组被拒就整体拒绝,不写集群;模板缺席不退回单实例;高可用另查口令字符集' {
    $mixed = Invoke-QueueManifestRender -Replicas 3 -Live @{ 'redis-queue-g0' = 1 }
    Assert-Equal -Expected '' -Actual $mixed.Error -Because '1 → 3 与全新都放行'
    Assert-Equal -Expected 'redis-queue-g0=upgrade,redis-queue-g1=absent' -Actual ((@($mixed.Manifests | ForEach-Object { "$($_.Name)=$($_.Verdict)" })) -join ',') -Because '形态变更按实例判'
    Assert-Match -Text $mixed.Warnings -Pattern '\[RedisQueueTopology\]Upgrade:queue instance redis-queue-g0 ' -Because '只对要切的实例告警'
    Assert-Equal -Expected 0 -Actual $mixed.Writes.Count -Because '渲染阶段不写集群'

    $refused = Invoke-QueueManifestRender -Replicas 1 -Live @{ 'redis-queue-g0' = 1; 'redis-queue-g1' = 3 }
    Assert-Match -Text $refused.Error -Pattern '拒绝变更排队 Redis 实例 redis-queue-g1' -Because '任何一组被拒,整次渲染失败'
    Assert-Equal -Expected 0 -Actual $refused.Writes.Count -Because '拒绝发生在任何写操作之前'

    $emptyDir = Join-Path $TempRoot 'no-templates'
    New-Item -ItemType Directory -Path $emptyDir -Force | Out-Null
    foreach ($case in @(@{ Replicas = 3; Template = 'redis-purpose-ha\.yaml' }, @{ Replicas = 1; Template = 'redis-purpose\.yaml' })) {
        $missing = Invoke-QueueManifestRender -Replicas $case.Replicas -ManifestsDir $emptyDir
        Assert-Match -Text $missing.Error -Pattern ('找不到 deploy/k8s/manifests/infra/' + $case.Template) -Because '模板缺席必须拒绝,高可用不悄悄退回单实例'
    }

    $spaced = 'with space-0123456'
    $badPassword = Invoke-QueueManifestRender -Replicas 3 -Password $spaced
    Assert-Match -Text $badPassword.Error -Pattern 'MMORPG_REDIS_PASSWORD' -Because '哨兵把口令原样写回配置文件,含空白的口令高可用形态要拒绝并点名变量'
    Assert-True -Condition (-not $badPassword.Error.Contains($spaced)) -Because '报错不得回显口令'
    Assert-Equal -Expected '' -Actual (Invoke-QueueManifestRender -Replicas 1 -Password $spaced).Error -Because '单实例没有哨兵,这条限制不适用'
    $script:RedisPassword = ''
}

Test-Case 'Apply-Infra 的 queue 实例顺序(AST):渲染与形态变更闸先于第一个写操作;apply 排在 infra 清单循环之后;等待只在 -WaitReady 分支' {
    $applyInfra = $deployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Apply-Infra' }, $true)
    $commands = @($applyInfra.Body.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))
    $firstOf = {
        param([string]$Name)
        $found = @($commands | Where-Object { $_.GetCommandName() -eq $Name } | Sort-Object { $_.Extent.StartOffset })
        if ($found.Count -eq 0) { throw "Apply-Infra 里找不到 $Name" }
        return $found[0]
    }
    $render = & $firstOf 'New-RedisQueueInstanceManifests'
    $ensureNs = & $firstOf 'Ensure-Namespace'
    $apply = & $firstOf 'Apply-RedisQueueInstances'
    $wait = & $firstOf 'Wait-RedisQueueInstancesReady'
    Assert-True -Condition ($render.Extent.StartOffset -lt $ensureNs.Extent.StartOffset) -Because '模板缺席、容量不合法、上限不等式、形态变更闸都要先于第一个写操作(Ensure-Namespace)'
    $infraLoop = $applyInfra.Body.Find({ param($a) $a -is [System.Management.Automation.Language.ForEachStatementAst] -and $a.Condition.Extent.Text -eq '$infraManifests' }, $true)
    Assert-True -Condition ($null -ne $infraLoop -and $apply.Extent.StartOffset -gt $infraLoop.Extent.EndOffset) -Because 'apply 排在核心 Redis 之后:口令 Secret redis-auth 已在核心分支建好或删掉'
    $guarded = $false
    for ($node = $wait.Parent; $null -ne $node; $node = $node.Parent) {
        if ($node -is [System.Management.Automation.Language.IfStatementAst] -and $node.Clauses[0].Item1.Extent.Text -eq '$WaitReady') { $guarded = $true; break }
    }
    Assert-True -Condition $guarded -Because '等待只在 -WaitReady 分支'
}

Test-Case 'redis-wipe(高可用 queue 实例,kubectl 替身):逐个 Pod exec -c redis,主不在序号 0 也清得到,从只报 DBSIZE;不碰列表外的 StatefulSet' {
    # 失败分支是 exit 1,在本进程里跑会结束整个测试:这里只走全部通过的路径,判定逻辑由「redis-wipe 准入」与下面的断言覆盖。
    $InfraNamespace = 'mmorpg-stress-ha'; $KubeContext = ''; $KubeConfig = ''; $DryRun = $false; $ConfirmWipeAllRedis = $true
    $script:QueueKubectlCalls = @()
    $script:KubectlResponder = {
        param([string]$Joined)
        if ($Joined -like 'get statefulset -n *') {
            return New-KubectlReply -Stdout '{"items":[{"metadata":{"name":"redis-queue-g0"},"spec":{"replicas":3}},{"metadata":{"name":"kafka"},"spec":{"replicas":3}}]}'
        }
        if ($Joined -like '*pod/redis-queue-g0-2 *') { return New-KubectlReply -Stdout "role=master`nflushall=OK`ndbsize=0" }
        return New-KubectlReply -Stdout "role=slave`ndbsize=5"
    }
    # 逐条取消息文本,不过 Out-String:它可能按控制台宽度折行,断言就落不到一行上。
    $printed = (@(Invoke-RedisWipe 6>&1) | ForEach-Object { $_.ToString() }) -join "`n"
    Assert-Match -Text $printed -Pattern 'Redis wipe: namespace=mmorpg-stress-ha instances=redis-queue-g0x3' -Because '高可用实例 3 个 Pod,kafka 不在名单里'
    Assert-Match -Text $printed -Pattern '\[wiped\] redis-queue-g0-2 role=master flushall=OK dbsize=0' -Because '主可能不在序号 0 上'
    Assert-Match -Text $printed -Pattern '\[checked\] redis-queue-g0-0 role=slave dbsize=5' -Because '从的 DBSIZE 只打印,不作判据(复制有延迟)'
    Assert-Match -Text $printed -Pattern 'Redis wipe OK' -Because '全部主节点通过'
    $execs = @($script:QueueKubectlCalls | Where-Object { $_ -like 'exec *' })
    Assert-Equal -Expected 3 -Actual $execs.Count -Because '3 个 redis Pod 各 exec 一次'
    foreach ($ordinal in 0..2) {
        Assert-True -Condition (@($execs | Where-Object { $_ -like "exec -n mmorpg-stress-ha pod/redis-queue-g0-$ordinal -c redis -- *" }).Count -eq 1) -Because "pod/redis-queue-g0-$ordinal 要显式 -c redis(Pod 里还有哨兵与 exporter)"
    }
}

Test-Case 'ConvertFrom-RedisQueueInstanceAddress:认 login 的 host:port 与 gateway 的 host;核心地址、空串与人工改过的地址不算 queue 实例' {
    foreach ($case in @(@{ In = 'redis-queue-g0.mmorpg-infra:6379'; Out = 'redis-queue-g0/mmorpg-infra' }, @{ In = ' redis-queue-g12.split-infra '; Out = 'redis-queue-g12/split-infra' })) {
        $target = ConvertFrom-RedisQueueInstanceAddress -Address $case.In
        Assert-Equal -Expected $case.Out -Actual ('{0}/{1}' -f $target.Name, $target.Namespace) -Because "'$($case.In)' 的实例与 namespace"
    }
    foreach ($bad in @('redis.mmorpg-infra:6379', '', 'redis-queue-g0', 'redis-queue-gx.ns:6379', 'redis-queue-g0-proxy.ns:6379')) {
        Assert-True -Condition ($null -eq (ConvertFrom-RedisQueueInstanceAddress -Address $bad)) -Because "'$bad' 后面没有可查的 queue 实例"
    }
    Assert-True -Condition ($null -eq (ConvertFrom-RedisQueueInstanceAddress -Address $null)) -Because '$null 同样没有'
}

# kubectl get -o json 解析出来的对象的最小替身(只含判定读的字段)。
function New-LiveWorkload {
    param([int]$Replicas, [AllowNull()]$Ready)
    $status = if ($null -eq $Ready) { [pscustomobject]@{} } else { [pscustomobject]@{ readyReplicas = $Ready } }
    return [pscustomobject]@{ spec = [pscustomobject]@{ replicas = $Replicas }; status = $status }
}
function New-LiveService {
    param([string]$App)
    return [pscustomobject]@{ spec = [pscustomobject]@{ selector = [pscustomobject]@{ app = $App } } }
}

Test-Case 'ConvertFrom-RedisQueueInstanceLive:形态与客户端那条路上的每一段;单实例不要求代理;实例不存在、没就绪、Service 与形态对不上都算问题' {
    $name = 'redis-queue-g0'
    $proxyName = "$name-proxy"
    $healthyHa = ConvertFrom-RedisQueueInstanceLive -InstanceName $name -StatefulSet (New-LiveWorkload -Replicas 3 -Ready 3) -Proxy (New-LiveWorkload -Replicas 2 -Ready 2) -Service (New-LiveService -App $proxyName)
    Assert-Equal -Expected "ha/3/3/2/2/$proxyName/0" -Actual ('{0}/{1}/{2}/{3}/{4}/{5}/{6}' -f $healthyHa.Shape, $healthyHa.Replicas, $healthyHa.ReadyReplicas, $healthyHa.ProxyReplicas, $healthyHa.ProxyReadyReplicas, $healthyHa.ServiceSelector, @($healthyHa.Problems).Count) -Because '高可用形态全部就绪'
    $healthySingle = ConvertFrom-RedisQueueInstanceLive -InstanceName $name -StatefulSet (New-LiveWorkload -Replicas 1 -Ready 1) -Proxy $null -Service (New-LiveService -App $name)
    Assert-Equal -Expected 'single/0' -Actual ('{0}/{1}' -f $healthySingle.Shape, @($healthySingle.Problems).Count) -Because '单实例不要求代理'

    foreach ($case in @(
            @{ Label = '没有 StatefulSet'; Sts = $null; Proxy = $null; Service = (New-LiveService -App $name); Shape = 'absent'; Pattern = "statefulset/$name is missing" },
            @{ Label = '没有 Service'; Sts = (New-LiveWorkload -Replicas 1 -Ready 1); Proxy = $null; Service = $null; Shape = 'single'; Pattern = "service/$name is missing" },
            @{ Label = '副本数不是 1 / 3'; Sts = (New-LiveWorkload -Replicas 2 -Ready 2); Proxy = $null; Service = (New-LiveService -App $name); Shape = 'unknown'; Pattern = 'has 2 replicas' },
            @{ Label = 'redis Pod 没全部就绪'; Sts = (New-LiveWorkload -Replicas 3 -Ready 2); Proxy = (New-LiveWorkload -Replicas 2 -Ready 2); Service = (New-LiveService -App $proxyName); Shape = 'ha'; Pattern = 'ready 2/3' },
            @{ Label = 'status 里还没有 readyReplicas'; Sts = (New-LiveWorkload -Replicas 1 -Ready $null); Proxy = $null; Service = (New-LiveService -App $name); Shape = 'single'; Pattern = 'ready 0/1' },
            @{ Label = '高可用没有代理'; Sts = (New-LiveWorkload -Replicas 3 -Ready 3); Proxy = $null; Service = (New-LiveService -App $proxyName); Shape = 'ha'; Pattern = "deployment/$proxyName is missing" },
            @{ Label = '代理没全部就绪'; Sts = (New-LiveWorkload -Replicas 3 -Ready 3); Proxy = (New-LiveWorkload -Replicas 2 -Ready 1); Service = (New-LiveService -App $proxyName); Shape = 'ha'; Pattern = "deployment/$proxyName ready 1/2" },
            @{ Label = '高可用而 Service 还选 redis Pod'; Sts = (New-LiveWorkload -Replicas 3 -Ready 3); Proxy = (New-LiveWorkload -Replicas 2 -Ready 2); Service = (New-LiveService -App $name); Shape = 'ha'; Pattern = "should select app=$proxyName" },
            @{ Label = '单实例而 Service 还选代理'; Sts = (New-LiveWorkload -Replicas 1 -Ready 1); Proxy = $null; Service = (New-LiveService -App $proxyName); Shape = 'single'; Pattern = "should select app=$name" }
        )) {
        $r = ConvertFrom-RedisQueueInstanceLive -InstanceName $name -StatefulSet $case.Sts -Proxy $case.Proxy -Service $case.Service
        Assert-Equal -Expected $case.Shape -Actual $r.Shape -Because "$($case.Label) 的形态"
        Assert-Match -Text (@($r.Problems) -join ' | ') -Pattern $case.Pattern -Because "$($case.Label) 必须算问题"
    }
}

Test-Case 'Read-RedisQueueInstanceLive(kubectl 替身):按实例读三个对象;不存在 = $null;kubectl 失败与非 JSON 一律 throw(读不到不等于不存在)' {
    $KubeContext = ''; $KubeConfig = ''
    $script:QueueKubectlCalls = @()
    $script:KubectlResponder = {
        param([string]$Joined)
        if ($Joined -like 'get statefulset redis-queue-g1 -n infra-ns *') { return New-KubectlReply -Stdout '{"spec":{"replicas":3},"status":{"readyReplicas":3}}' }
        if ($Joined -like 'get deployment redis-queue-g1-proxy -n infra-ns *') { return New-KubectlReply -Stdout '{"spec":{"replicas":2},"status":{"readyReplicas":2}}' }
        if ($Joined -like 'get service redis-queue-g1 -n infra-ns *') { return New-KubectlReply -Stdout '{"spec":{"selector":{"app":"redis-queue-g1-proxy"}}}' }
        throw "kubectl 替身没想到的调用:$Joined"
    }
    $live = Read-RedisQueueInstanceLive -InstanceName 'redis-queue-g1' -Namespace 'infra-ns'
    Assert-Equal -Expected 'ha/0' -Actual ('{0}/{1}' -f $live.Shape, @($live.Problems).Count) -Because '三个对象都在且就绪'
    Assert-Equal -Expected 3 -Actual $script:QueueKubectlCalls.Count -Because '每个对象读一次'

    $script:KubectlResponder = { param([string]$Joined) return New-KubectlReply }
    $absent = Read-RedisQueueInstanceLive -InstanceName 'redis-queue-g1' -Namespace 'infra-ns'
    Assert-Equal -Expected 'absent' -Actual $absent.Shape -Because '--ignore-not-found 的空输出 = 不存在'
    Assert-True -Condition (@($absent.Problems).Count -ge 2) -Because '实例与入口都不在'

    foreach ($case in @(@{ Reply = (New-KubectlReply -ExitCode 1 -Stderr 'forbidden'); Pattern = 'forbidden' }, @{ Reply = (New-KubectlReply -Stdout 'not json'); Pattern = '不是合法 JSON' })) {
        $script:CannedKubectlReply = $case.Reply
        $script:KubectlResponder = { param([string]$Joined) return $script:CannedKubectlReply }
        $message = ''
        try { $null = Read-RedisQueueInstanceLive -InstanceName 'redis-queue-g1' -Namespace 'infra-ns' } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern $case.Pattern -Because '读不到不等于不存在,不能当成实例没部署'
    }
}

Test-Case 'Show-RedisSplitStatus(线上事实注入):打印 zone 实际指向的实例的形态与就绪状态,多个 zone 同组只查一次;实例的问题进失败列表' {
    # 失败分支是 exit 1,在本进程里跑会结束整个测试:这里走全部正常的路径,问题判定由上面的纯函数用例覆盖,接线用函数体断言钉住。
    $ZonesConfigPath = New-TempGroupedZonesJson -Name 'same-group' -Groups @(0, 0)
    $NamespacePrefix = 'mmorpg-zone'; $InfraNamespace = 'ns'; $DryRun = $false; $KubeContext = ''; $KubeConfig = ''
    $CentreReplicas = 1; $GateReplicas = 2; $SceneReplicas = 4; $SceneWorldReplicas = -1; $SceneInstanceReplicas = -1; $GateNodePortBase = 30000
    $script:SplitLive = ConvertFrom-RedisPurposeLiveYaml -LoginYaml (New-LiveLoginYaml -QueueHost 'redis-queue-g0.infra-ns:6379') -GatewayYaml (New-LiveGatewayYaml -RedisHost 'redis-queue-g0.infra-ns')
    $script:SplitReadNamespaces = @()
    $script:QueueKubectlCalls = @()
    $script:KubectlResponder = {
        param([string]$Joined)
        if ($Joined -like 'get statefulset redis-queue-g0 -n infra-ns *') { return New-KubectlReply -Stdout '{"spec":{"replicas":3},"status":{"readyReplicas":3}}' }
        if ($Joined -like 'get deployment redis-queue-g0-proxy -n infra-ns *') { return New-KubectlReply -Stdout '{"spec":{"replicas":2},"status":{"readyReplicas":2}}' }
        if ($Joined -like 'get service redis-queue-g0 -n infra-ns *') { return New-KubectlReply -Stdout '{"spec":{"selector":{"app":"redis-queue-g0-proxy"}}}' }
        # 控制域(缺口第 5 条):两个 zone 都在组 0,scene-manager 与 node-config 都指向 redis-ctl-g0.ns(本次 -InfraNamespace 是 ns)。
        if ($Joined -like 'get configmap go-svc-scene-manager-config -n mmorpg-zone-split-* -o json') {
            return New-KubectlReply -Stdout ((@{ data = @{ 'scene_manager_service.yaml' = "Redis:`n  Host: redis-ctl-g0.ns:6379`n  Type: node`nCoreRedis:`n  Host: redis.ns:6379`n" } }) | ConvertTo-Json -Depth 5)
        }
        if ($Joined -like 'get configmap node-config -n mmorpg-zone-split-* -o json') {
            return New-KubectlReply -Stdout ((@{ data = @{ 'game_config.yaml' = "zoneredis:`n  host: `"redis.ns`"`n  port: 6379`nscene_control_redis:`n  host: `"redis-ctl-g0.ns`"`n  port: 6379`n  db: 0`n" } }) | ConvertTo-Json -Depth 5)
        }
        if ($Joined -like 'get statefulset redis-ctl-g0 -n ns *') { return New-KubectlReply -Stdout '{"spec":{"replicas":1},"status":{"readyReplicas":1}}' }
        if ($Joined -like 'get deployment redis-ctl-g0-proxy -n ns *') { return New-KubectlReply }
        if ($Joined -like 'get service redis-ctl-g0 -n ns *') { return New-KubectlReply -Stdout '{"spec":{"selector":{"app":"redis-ctl-g0"}}}' }
        if ($Joined -like 'get deployment * -o name') { return New-KubectlReply -Stdout 'deployment.apps/x' }
        # login、gateway 是 SQL 写者,按 StatefulSet 查(Get-RedisSplitRolloutState -Kind statefulset)。
        if ($Joined -like 'get statefulset * -o name') { return New-KubectlReply -Stdout 'statefulset.apps/x' }
        if ($Joined -like 'rollout status *') { return New-KubectlReply }
        throw "kubectl 替身没想到的调用:$Joined"
    }
    $printed = (@(Show-RedisSplitStatus 6>&1) | ForEach-Object { $_.ToString() }) -join "`n"
    Assert-Match -Text $printed -Pattern 'Redis split status: OK' -Because '地址一致、滚完、实例就绪'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($printed, 'instance=redis-queue-g0 namespace=infra-ns shape=ha ready=3/3 proxy=2/2 service_selector=redis-queue-g0-proxy').Count) -Because '两个 zone 同组,实例只查、只打一次;namespace 取自线上地址,不取本次参数'
    Assert-Equal -Expected 'mmorpg-zone-split-0,mmorpg-zone-split-1' -Actual ($script:SplitReadNamespaces -join ',') -Because '每个 zone 各读一次 ConfigMap'
    Assert-Equal -Expected 2 -Actual ([regex]::Matches($printed, 'ctl_expected=redis-ctl-g0\.ns:6379 scene-manager\.Redis=redis-ctl-g0\.ns:6379 node-config\.scene_control_redis=redis-ctl-g0\.ns rollout scene-manager=complete').Count) `
        -Because 'control domain: one line per zone, scene-manager and node-config both on the group instance'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($printed, 'instance=redis-ctl-g0 namespace=ns shape=single ready=1/1 proxy=<none> service_selector=redis-ctl-g0').Count) `
        -Because 'control instance of a shared group is checked and printed once'

    $body = ($deployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Show-RedisSplitStatus' }, $true)).Body.Extent.Text
    Assert-Match -Text $body -Pattern 'foreach \(\$problem in \$instance\.Problems\) \{ \$failures\.Add' -Because '实例的问题要进失败列表(退出码 1),不能只打印'
    Assert-Match -Text $body -Pattern 'foreach \(\$address in @\(\$live\.LoginQueueHostPort, \$live\.GatewayHost\)\)' -Because 'login 与 gateway 分开取:切换没做完时两者可能指向不同实例'
    Assert-Match -Text $body -Pattern 'foreach \(\$problem in \(Get-RedisCtlZoneStatus [^\r\n]*\)\) \{\s*\$failures\.Add\(\$problem\)' -Because 'control domain problems must reach the failure list (exit code 1), not only be printed'
}

Test-Case 'Get-RedisCtlZoneProblems / ConvertFrom-RedisCtlNodeConfigYaml: wrong group, missing scene_control_redis section and unfinished rollout are problems; absent ConfigMaps are not' {
    $expected = [pscustomobject]@{ Service = 'redis-ctl-g1'; Host = 'redis-ctl-g1.ns'; Port = 6379; HostPort = 'redis-ctl-g1.ns:6379' }
    $smOk = ConvertFrom-RedisCtlLiveYaml -SceneManagerYaml "Redis:`n  Host: redis-ctl-g1.ns:6379`n"
    $nodeOk = ConvertFrom-RedisCtlNodeConfigYaml -GameConfigYaml "zoneredis:`n  host: `"redis.ns`"`nscene_control_redis:`n  host: `"redis-ctl-g1.ns`"`n  port: 6379`n"
    Assert-Equal -Expected 'redis-ctl-g1.ns' -Actual $nodeOk.Host -Because 'reads scene_control_redis.host, not zoneredis.host'
    Assert-Equal -Expected 0 -Actual @(Get-RedisCtlZoneProblems -SceneManager $smOk -NodeConfig $nodeOk -Expected $expected -SceneManagerRollout 'complete').Count -Because 'both on the group instance, rollout complete'
    $absentSm = ConvertFrom-RedisCtlLiveYaml -SceneManagerYaml $null
    $absentNode = ConvertFrom-RedisCtlNodeConfigYaml -GameConfigYaml $null
    Assert-Equal -Expected 'False/False' -Actual ('{0}/{1}' -f $absentSm.Present, $absentNode.Present) -Because '$null means the ConfigMap does not exist (not coerced to an empty string)'
    Assert-Equal -Expected 0 -Actual @(Get-RedisCtlZoneProblems -SceneManager $absentSm -NodeConfig $absentNode -Expected $expected -SceneManagerRollout 'absent').Count -Because 'zone not deployed is not a problem'
    foreach ($case in @(
            @{ Label = 'scene-manager on another group'; Sm = (ConvertFrom-RedisCtlLiveYaml -SceneManagerYaml "Redis:`n  Host: redis-ctl-g0.ns:6379`n"); Node = $nodeOk; Rollout = 'complete'; Pattern = "scene-manager control Redis is 'redis-ctl-g0\.ns:6379'" },
            @{ Label = 'scene-manager on the core Redis'; Sm = (ConvertFrom-RedisCtlLiveYaml -SceneManagerYaml "Redis:`n  Host: redis.ns:6379`n"); Node = $nodeOk; Rollout = 'complete'; Pattern = "scene-manager control Redis is 'redis\.ns:6379'" },
            @{ Label = 'node-config without scene_control_redis (C++ copies zoneredis)'; Sm = $smOk; Node = (ConvertFrom-RedisCtlNodeConfigYaml -GameConfigYaml "zoneredis:`n  host: `"redis.ns`"`n"); Rollout = 'complete'; Pattern = "scene_control_redis\.host is ''" },
            @{ Label = 'rollout not finished'; Sm = $smOk; Node = $nodeOk; Rollout = 'incomplete'; Pattern = 'deployment/scene-manager rollout is incomplete' },
            @{ Label = 'rollout query failed'; Sm = $smOk; Node = $nodeOk; Rollout = 'error'; Pattern = 'deployment/scene-manager rollout is error' }
        )) {
        $problems = @(Get-RedisCtlZoneProblems -SceneManager $case.Sm -NodeConfig $case.Node -Expected $expected -SceneManagerRollout $case.Rollout)
        Assert-Equal -Expected 1 -Actual $problems.Count -Because "$($case.Label): exactly one problem"
        Assert-Match -Text ($problems -join ' | ') -Pattern $case.Pattern -Because "$($case.Label) must be reported"
    }
}

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

exit (Complete-TestRun -SuiteName "k8s redis split contract")
