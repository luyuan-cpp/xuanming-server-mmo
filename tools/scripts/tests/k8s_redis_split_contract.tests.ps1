#requires -Version 7
<#
.SYNOPSIS
    共享 Redis 按用途拆实例(G4a 第 1a 批:queue)的部署侧契约测试。

.DESCRIPTION
    设计:docs/design/shared-redis-split-by-purpose.md §25.3(2026-10-23 拍板记录)、§3.2.1;部署说明见 deploy/k8s/README.md
    「共享 Redis 按用途拆实例」。本文件钉住:
      1. 不带 -RedisSplit(缺省 auto,DryRun 按 none)时 zone-up / infra-up 的产物里没有 QueueRedis、没有 redis-queue-g 字样,
         gateway 的 spring.data.redis.host 仍是核心 redis.<infra>;
      2. -RedisSplit queue:login 整段写 Node.QueueRedis(Host = redis-queue-g0.<infra>:6379,其余字段与 RedisClient 逐项同值),
         gateway 指向 redis-queue-g0.<infra>;C++ zoneredis、go/db RedisClient、data-service DevRedis 仍是核心;
      3. zones 配置的 redisQueueGroup: 2 → redis-queue-g2;非法值(负数、小数、字符串)在任何写操作之前 throw;
      4. infra-up -RedisSplit queue 渲染 redis-queue-g0:不带 app: redis、noeviction + maxmemory、MAXCLIENTS 只定义一次、
         --enable-debug-command no、redis-exporter sidecar、没有未替换的占位,且排在核心 Redis 之后;
      5. release 档要 apply queue 实例而没给 -RedisQueueMaxMemory → 拒绝;
      6. 切换闸的判定(Get-RedisPurposeSwitchVerdict)与入口闸(Assert-RedisPurposeSwitchIsSafe,线上事实注入,不连集群);
      7. redis-wipe 的准入(namespace 规则 + 确认)与 exec 输出解析;dev_tools.ps1 的透传口径。
    子进程一律 -DryRun(不连集群);纯函数按 AST 从 k8s_deploy.ps1 抽出,在本进程里直接调用。

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
    $fixtureEnv = @{ MMORPG_DB_ALLOWED_DATABASES = 'zone_1_db,zone_2_db,zone_101_db,zone_102_db' }
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

Test-Case '缺省 infra-up DryRun:不渲染任何 queue 实例,也没有 redis-queue-g 字样' {
    Assert-Equal -Expected 0 -Actual $defaultInfraRun.ExitCode -Because "缺省 infra-up DryRun 必须成功。输出: $($defaultInfraRun.Output)"
    Assert-NotMatch -Text $defaultInfraRun.Output -Pattern 'redis-queue-g' -Because 'infra-up 的 auto 按 none:不 apply 也不删除 queue 实例'
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
    Assert-Equal -Expected "redis-queue-g0.$Infra" -Actual (Get-ConfigValue -Output $out -ConfigMap 'java-svc-gateway-config' -KeyPath 'data.application.yaml.spring.data.redis.host') -Because 'gateway 的 Redis 只用于 Bucket4j 限流,跟着排队实例走'
    Assert-Equal -Expected '6379' -Actual (Get-ConfigValue -Output $out -ConfigMap 'java-svc-gateway-config' -KeyPath 'data.application.yaml.spring.data.redis.port') -Because '端口行不变'
}

Test-Case '-RedisSplit queue:C++ zoneredis、go/db RedisClient、data-service DevRedis 仍是核心 redis.<infra>:6379' {
    $out = $queueZoneRun.Output
    $zoneRedis = '{0}:{1}' -f (Get-ConfigValue -Output $out -ConfigMap 'node-config' -KeyPath 'data.game_config.yaml.zoneredis.host'), (Get-ConfigValue -Output $out -ConfigMap 'node-config' -KeyPath 'data.game_config.yaml.zoneredis.port')
    Assert-Equal -Expected "redis.${Infra}:6379" -Actual $zoneRedis -Because 'C++ 存盘与会话恒在核心,永不指向拆出去的实例'
    Assert-Equal -Expected "redis.${Infra}:6379" -Actual (Get-ConfigValue -Output $out -ConfigMap 'go-svc-db-config' -KeyPath 'data.db.yaml.ServerConfig.RedisClient.Hosts') -Because 'go/db 的 RedisClient(存盘控制面、屏障 ACK)恒在核心'
    Assert-Equal -Expected "redis.${Infra}:6379" -Actual (Get-ConfigValue -Output $out -ConfigMap 'go-svc-data-service-config' -KeyPath 'data.data_service.yaml.DevRedis.Host') -Because 'data-service 读的账本由 C++ / go/db 写在核心'
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
    Assert-Match -Text $sts -Pattern '(?m)^  replicas: 1\s*$' -Because '本批所有档位都是单实例'
    Assert-Match -Text $sts -Pattern '(?m)^\s+app: redis-queue-g0\s*$' -Because '标签与 selector 用实例名'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($sts, '(?m)^[ \t]*MAXCLIENTS=10000[ \t]*$').Count) -Because 'MAXCLIENTS 只定义一次'
    Assert-Match -Text $sts -Pattern 'redis fd limit:' -Because '启动前打印 fd 限值(同 redis.yaml)'
    foreach ($flag in @('--appendonly yes', '--appendfsync everysec', '--maxclients "$MAXCLIENTS"', '--enable-debug-command no', '--maxmemory 128mb', '--maxmemory-policy noeviction')) {
        Assert-Equal -Expected 2 -Actual ([regex]::Matches($sts, [regex]::Escape($flag)).Count) -Because "有口令 / 无口令两条 exec 都要带 $flag"
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
    'Test-ZoneLoginDeployed', 'Test-ZoneGatewayDeployed', 'Get-ZoneNamespace'
)
foreach ($fnAst in $deployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($splitFunctionNames -contains $fnAst.Name) {
        Set-Item -Path "Function:script:$($fnAst.Name)" -Value $fnAst.Body.GetScriptBlock()
    }
}
foreach ($fnName in $splitFunctionNames) {
    if (-not (Test-Path "Function:$fnName")) { throw "k8s_deploy.ps1 缺少函数 ${fnName}(按用途拆实例的接入被删了?后续断言无意义)" }
}

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
    Assert-Match -Text $plan.Output -Pattern '\[dry-run\] redis-wipe would list StatefulSets redis / redis-queue-g<N> / redis-match-cluster' -Because 'DryRun 只打计划'
    Assert-NotMatch -Text $plan.Output -Pattern '\[dry-run\] kubectl ' -Because 'DryRun 不发任何 kubectl'
}

Test-Case 'redis-split-status DryRun:按 zones 配置逐 zone 打出将要读的 ConfigMap 与 rollout,不连集群' {
    $run = Invoke-SplitDryRun -Arguments @('-Command', 'redis-split-status', '-DryRun', '-ZonesConfigPath', $SplitZonesPath)
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "DryRun 必须成功。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern '\[dry-run\] would read ConfigMaps go-svc-login-config / java-svc-gateway-config and rollout status of deployment/login / deployment/gateway in namespace mmorpg-zone-split-test' -Because '对每个 zone 打出要读的对象'
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

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

exit (Complete-TestRun -SuiteName "k8s redis split contract")
