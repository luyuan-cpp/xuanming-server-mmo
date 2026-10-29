#requires -Version 7
<#
.SYNOPSIS
    k8s_deploy.ps1 的部署生成器契约测试。

.DESCRIPTION
    背景:k8s_deploy.ps1 1600+ 行、零测试,产物又不入库(每次 apply 现生成),
    于是它写死的常数和各服务 etc/*.yaml 之间静默漂移过一堆压测期已知会炸的值:
      - Locker.PlayerLockTTL       生成器 5   vs etc 120(30s 都被证明会丢锁)
      - Kafka.PartitionCnt         生成器 5   vs etc 10
      - Database.MaxOpenConn       生成器 10  vs etc 60(10 就是那个瓶颈)
      - Database.User/Passwd       生成器 root/root vs 集群 MYSQL_ROOT_PASSWORD

    所以本套测试的核心判据是**跨文件一致**:生成的 ConfigMap 值必须等于服务
    自己 etc/*.yaml 里的值,而不是"生成器里也有一份差不多的常数"。

    负向用例一律断言**错误文本**,不只断退出码 —— 退出码 1 可能来自任何地方
    (打错参数、脚本语法错),只断退出码的负向测试等于没测。

.EXAMPLE
    pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
#>

$ErrorActionPreference = "Stop"

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ""
Write-Host "=== k8s_deploy.ps1 deployment generator contract tests ==="

# 仅本套DryRun夹具补明确、虚构的外部SQL入口/独立库清单；显式参数与显式Env负例优先。
# 不改生产fallback，也不连接此主机；所有后续临时分支都仍运行正式部署脚本。
function Invoke-DeployDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '部署合同夹具只允许显式-DryRun，禁止连接或写集群。' }
    $fixtureEnv = @{ MMORPG_DB_ALLOWED_DATABASES = 'zone_1_db,zone_2_db,zone_101_db,zone_102_db' }
    foreach ($key in $Env.Keys) { $fixtureEnv[$key] = $Env[$key] }
    $fixtureArgs = @($Arguments)
    if (-not ($fixtureArgs | Where-Object { $_ -match '^-GlobalAuthorityTiDBHost(?::|$)' })) {
        $fixtureArgs += @('-GlobalAuthorityTiDBHost', 'tidb.invalid:4000')
    }
    # 仅部署渲染夹具：两条空entry检验有限容量；实际registrar仍必须校验完整正式请求，不能用本夹具注册。
    $fixtureRoot=Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-roster-render-'+[Guid]::NewGuid().ToString('N'))
    $null=[IO.Directory]::CreateDirectory($fixtureRoot)
    try {
        if(-not $fixtureEnv.ContainsKey('MMORPG_SQL_K8S_BOOT_CATALOG')){
            $rosterPath=Join-Path $fixtureRoot 'roster.json'
            [IO.File]::WriteAllText($rosterPath,'{"format":"SQL_WRITER_BOOT_ROSTER_V1","entries":[{},{}]}',[Text.UTF8Encoding]::new($false))
            $rosterSha=(Get-FileHash -LiteralPath $rosterPath -Algorithm SHA256).Hash.ToLowerInvariant()
            $zone='contract-test';$prefix='mmorpg-zone';$infra='mmorpg-infra'
            for($i=0;$i -lt $fixtureArgs.Count-1;$i++){
                switch($fixtureArgs[$i]){'-ZoneName'{$zone=$fixtureArgs[$i+1]};'-NamespacePrefix'{$prefix=$fixtureArgs[$i+1]};'-InfraNamespace'{$infra=$fixtureArgs[$i+1]}}
            }
            $catalog=@{}
            foreach($ns in @(($prefix+'-'+$zone),$infra)){
                foreach($svc in @('login','gateway','data-service','db','chat','friend','guild','mail','match','player-inbox','rank','trade')){
                    $catalog[$ns+'/'+$svc]=@{requestSecret='fixture-request';registrarSecret='fixture-registrar';registrarImage=('fixture.invalid/registrar@sha256:'+('a'*64));physicalAuthoritySha256=('b'*64);runtimeConfigSecret='fixture-runtime';replicas=2;rosterFile=$rosterPath;rosterSha256=$rosterSha;schemaConfigSecret='fixture-schema-config';schemaPermissionSecret='fixture-schema-permission'}
                }
            }
            $catalogPath=Join-Path $fixtureRoot 'catalog.json'
            [IO.File]::WriteAllText($catalogPath,($catalog | ConvertTo-Json -Depth 8 -Compress),[Text.UTF8Encoding]::new($false))
            $fixtureEnv.MMORPG_SQL_K8S_BOOT_CATALOG=$catalogPath
        }
        return Invoke-CapturedPwsh -ScriptPath (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1') -Arguments $fixtureArgs -Env $fixtureEnv
    } finally {
        $full=[IO.Path]::GetFullPath($fixtureRoot);$allowed=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\','/')+[IO.Path]::DirectorySeparatorChar
        if(-not $full.StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $full) -notlike 'mmorpg-roster-render-*'){throw '渲染夹具清理范围非法'}
        Remove-Item -LiteralPath $full -Recurse -Force
    }
}

# 一次 DryRun 供多个用例复用(子进程启动比断言贵得多)
$BaseArgs = @(
    "-Command", "zone-up",
    "-ZoneName", "contract-test",
    "-ZoneId", "101",
    "-DryRun",
    # 正式全球SQL契约的虚构入口；本套只有DryRun，不解析或连接此主机。
    "-GlobalAuthorityTiDBHost", "tidb.invalid:4000",
    "-GoSvcRegistry", "registry.invalid/test",
    "-JavaSvcRegistry", "registry.invalid/test"
)

$devRun = Invoke-DeployDryRun -Arguments $BaseArgs
if ($devRun.ExitCode -ne 0) {
    Write-Host $devRun.Output
    throw "基线 DryRun 自己就失败了(exit=$($devRun.ExitCode)),后续断言无意义"
}
$devOut = $devRun.Output

# Agones 渲染路径复用同一次 DryRun(Fleet 与 Deployment 是两套模板,sidecar / 日志保留
# 这类断言必须两边都过 —— 只改一边正是本仓踩过的坑)。
$agonesRun = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SceneOrchestrator', 'agones', '-SkipGoSvc', '-SkipJavaSvc'))
if ($agonesRun.ExitCode -ne 0) {
    Write-Host $agonesRun.Output
    throw "Agones DryRun 基线自己就失败了(exit=$($agonesRun.ExitCode)),后续断言无意义"
}
$agonesOut = $agonesRun.Output

# ─────────────────────────────────────────────────────────────────
# 0. 采集链路自检
# ─────────────────────────────────────────────────────────────────

Test-Case "自检:子进程输出的中文必须无损带回(否则后面断错误文本的用例全是假红)" {
    # 本套测试的负向用例断的是子进程打出来的中文错误文本。PowerShell 按
    # [Console]::OutputEncoding 解码原生进程输出,而 Windows 控制台默认码页是
    # ibm437/GBK 这类 OEM 码页 —— 中文会在编解码往返里被整体打成 '?',于是 6 条
    # 断中文的用例集体变红,红的却是编码链路而不是被测脚本。这个坑真踩过一次,
    # 排查成本远高于这条自检本身,所以在这里钉一条。
    # 兜底逻辑在 tests/lib/deploy_capture.ps1 的 Invoke-CapturedPwsh。
    #
    # 判据用"输出里还有没有汉字",不锚定具体措辞:生成器往 ConfigMap 里写的
    # 中文注释会随代码改,而"一个汉字都不剩"只可能是编码坏了。
    # 注意本条只能抓住"汉字被打成 '?'"这一类(ibm437 路径);GBK 误解码可能
    # 仍落在汉字区间,那种情况由下面各条断具体错误文本的用例兜住。
    Assert-Match -Text $devOut -Pattern '[一-龥]' -Because "捕获回来的子进程输出里一个汉字都没有 = 采集链路的编码坏了,不是被测脚本的问题"
}

# ─────────────────────────────────────────────────────────────────
# 1. 生成的 ConfigMap 必须与各服务 etc/*.yaml 跨文件一致
# ─────────────────────────────────────────────────────────────────

Test-Case "db ConfigMap 的 Kafka/Database 关键值 == go/db/etc/db.yaml" {
    $block = Select-ManifestByName -Output $devOut -Name "go-svc-db-config"
    Assert-True -Condition ($null -ne $block) -Because "DryRun 输出里应当有 go-svc-db-config"
    $flat = ConvertTo-FlatManifest -Block $block

    $pairs = @(
        @{ Gen = 'data.db.yaml.ServerConfig.Kafka.PartitionCnt';    Etc = 'ServerConfig.Kafka.PartitionCnt' }
        @{ Gen = 'data.db.yaml.ServerConfig.Kafka.TopicGeneration'; Etc = 'ServerConfig.Kafka.TopicGeneration' }
        @{ Gen = 'data.db.yaml.ServerConfig.Kafka.SubShardCount';   Etc = 'ServerConfig.Kafka.SubShardCount' }
        @{ Gen = 'data.db.yaml.ServerConfig.Database.MaxOpenConn';  Etc = 'ServerConfig.Database.MaxOpenConn' }
        @{ Gen = 'data.db.yaml.ServerConfig.Database.MaxIdleConn';  Etc = 'ServerConfig.Database.MaxIdleConn' }
    )
    foreach ($p in $pairs) {
        $expected = Get-EtcValue -RelativePath 'go/db/etc/db.yaml' -KeyPath $p.Etc
        $actual = Get-FlatValue -Flat $flat -KeyPath $p.Gen
        Assert-Equal -Expected $expected -Actual $actual -Because "$($p.Etc) 必须与 go/db/etc/db.yaml 一致(生成器不得自带常数)"
    }
}

Test-Case "data-service ConfigMap 的 Kafka 关键值 == go/data_service/etc/data_service.yaml" {
    # 审计 topic 的世代 / 分区数是不可变契约,player_snapshot 消费者的批量 / 并发旋钮与 lag 探针(G2a)决定落库路径:
    # 生成器一旦自带常数,集群跑的就不是仓库里评审过的那份(比如服务 yaml 仍是逐条,ConfigMap 却悄悄开了批量)。
    $block = Select-ManifestByName -Output $devOut -Name "go-svc-data-service-config"
    Assert-True -Condition ($null -ne $block) -Because "DryRun 输出里应当有 go-svc-data-service-config"
    $flat = ConvertTo-FlatManifest -Block $block

    $keys = @(
        'TopicGeneration', 'TransactionLogPartitions', 'SnapshotPartitions', 'RetentionMs',
        'SnapshotBatchMaxRows', 'SnapshotBatchMaxBytes', 'SnapshotFlushIntervalMs', 'SnapshotMaxInflightFlushes', 'LagProbeIntervalMs'
    )
    foreach ($k in $keys) {
        $expected = Get-EtcValue -RelativePath 'go/data_service/etc/data_service.yaml' -KeyPath "Kafka.$k"
        $actual = Get-FlatValue -Flat $flat -KeyPath "data.data_service.yaml.Kafka.$k"
        Assert-Equal -Expected $expected -Actual $actual -Because "Kafka.$k 必须与 go/data_service/etc/data_service.yaml 一致(生成器不得自带常数)"
    }
}

Test-Case "ConfigMap 镜像 MySQL 池参数 == 服务 yaml(db Placement、data-service PlayerName / RouteStore、gateway Hikari),login 不写 PasswordAuth" {
    # 背景(G2b,docs/handoff/launch-readiness-20261013/spec_G2b.md §4.1.4):所有 zone 的 go/db、data_service、Java gateway 共用同一台
    # MySQL,连接总账(k8s_deploy.ps1 的 Get-MysqlConnectionLedgerInputs)读的是服务自己的配置。这几组池参数以前不进 ConfigMap,
    # 集群里生效的是「Go 默认值 / jar 内那份恰好等于服务 yaml」:两边一漂移,总账算的就不是集群的账,而且不会有任何报错。
    # db 的额外落点库池按库所属家族分两个旋钮(ZoneStore* / NonZoneStore*),外加池内空闲连接的回收秒数,五个键都镜像。
    # 字段名用 PoolKeys 而不是 Keys:哈希表自带 Keys 属性,同名字段读起来有歧义。
    $mirrors = @(
        @{ ConfigMap = 'go-svc-db-config'; Prefix = 'data.db.yaml.'; Source = 'go/db/etc/db.yaml'
           PoolKeys = @('Placement.ZoneStoreMaxOpenConn', 'Placement.ZoneStoreMaxIdleConn', 'Placement.NonZoneStoreMaxOpenConn', 'Placement.NonZoneStoreMaxIdleConn', 'Placement.ExtraStoreConnMaxIdleSeconds') }
        @{ ConfigMap = 'go-svc-data-service-config'; Prefix = 'data.data_service.yaml.'; Source = 'go/data_service/etc/data_service.yaml'
           PoolKeys = @('PlayerName.MaxOpenConn', 'PlayerName.MaxIdleConn', 'RouteStore.MaxOpenConn', 'RouteStore.MaxIdleConn') }
        @{ ConfigMap = 'java-svc-gateway-config'; Prefix = 'data.application.yaml.'; Source = 'java/gateway_node/src/main/resources/application.yaml'
           PoolKeys = @('spring.datasource.hikari.maximum-pool-size', 'spring.datasource.hikari.minimum-idle', 'spring.datasource.hikari.connection-timeout') }
    )
    foreach ($mirror in $mirrors) {
        $block = Select-ManifestByName -Output $devOut -Name $mirror.ConfigMap
        Assert-True -Condition ($null -ne $block) -Because "DryRun 输出里应当有 $($mirror.ConfigMap)"
        $flat = ConvertTo-FlatManifest -Block $block
        foreach ($key in $mirror.PoolKeys) {
            $expected = Get-EtcValue -RelativePath $mirror.Source -KeyPath $key
            $actual = Get-FlatValue -Flat $flat -KeyPath ($mirror.Prefix + $key)
            Assert-Equal -Expected $expected -Actual $actual -Because "$($mirror.ConfigMap) 的 $key 必须与 $($mirror.Source) 一致(生成器不得自带常数;MySQL 连接总账读的是同一个键)"
        }
    }

    # login 的 PasswordAuth 自带一个 MySQL 连接池(go/login/etc/login.yaml 里 MaxOpenConns 20),总账没有登记它:前提是 K8s 的
    # login ConfigMap 不写这一段、默认不启用。一旦写出来,每个 login 实例就多一组不在账上的连接 —— 要启用,先在
    # Get-MysqlConnectionLedgerInputs / Get-MysqlConnectionLedger 里登记,再改这里。
    # DevPasswordAuth 是另一个顶层键(拍平后是 data.login.yaml.DevPasswordAuth...),不会被这条判据误中。
    $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-login-config')
    $passwordAuthKeys = @($loginFlat.Scalars.Keys | Where-Object { $_ -eq 'data.login.yaml.PasswordAuth' -or $_ -like 'data.login.yaml.PasswordAuth.*' })
    Assert-True -Condition ($passwordAuthKeys.Count -eq 0) -Because "login ConfigMap 不得写 PasswordAuth 段(实际出现:$($passwordAuthKeys -join ', ')):它的 MySQL 连接池不在连接总账里"
}

Test-Case "login ConfigMap 的 Timeout/Node/Locker/Kafka 关键值 == go/login/etc/login.yaml" {
    $block = Select-ManifestByName -Output $devOut -Name "go-svc-login-config"
    Assert-True -Condition ($null -ne $block) -Because "DryRun 输出里应当有 go-svc-login-config"
    $flat = ConvertTo-FlatManifest -Block $block

    # Timeout:C++ deadline 预算门禁(Assert-GrpcClientDeadlineBudget)核对的是 login.yaml,ConfigMap 必须是同一个值,
    # 否则门禁放行的不是集群里真正生效的那份(生成器以前在模板里写死 100000)。
    $pairs = @(
        @{ Gen = 'data.login.yaml.Timeout';                Etc = 'Timeout' }
        @{ Gen = 'data.login.yaml.Node.SessionExpireMin';  Etc = 'Node.SessionExpireMin' }
        @{ Gen = 'data.login.yaml.Node.MaxLoginDevices';   Etc = 'Node.MaxLoginDevices' }
        @{ Gen = 'data.login.yaml.Node.LeaseTTL';          Etc = 'Node.LeaseTTL' }
        @{ Gen = 'data.login.yaml.Node.QueueShardCount';   Etc = 'Node.QueueShardCount' }
        @{ Gen = 'data.login.yaml.Locker.AccountLockTTL';  Etc = 'Locker.AccountLockTTL' }
        @{ Gen = 'data.login.yaml.Locker.PlayerLockTTL';   Etc = 'Locker.PlayerLockTTL' }
        @{ Gen = 'data.login.yaml.Kafka.PartitionCnt';     Etc = 'Kafka.PartitionCnt' }
        @{ Gen = 'data.login.yaml.Kafka.InitialPartition'; Etc = 'Kafka.InitialPartition' }
        @{ Gen = 'data.login.yaml.Kafka.TopicGeneration';  Etc = 'Kafka.TopicGeneration' }
        # 混版本持久字段保护 C4 的模式:不镜像 = Go 读到空串按 warn,仓库切 enforce 时集群里悄悄不生效。
        @{ Gen = 'data.login.yaml.PersistSchemaRegressionMode'; Etc = 'PersistSchemaRegressionMode' }
    )
    foreach ($p in $pairs) {
        $expected = Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath $p.Etc
        $actual = Get-FlatValue -Flat $flat -KeyPath $p.Gen
        Assert-Equal -Expected $expected -Actual $actual -Because "$($p.Etc) 必须与 go/login/etc/login.yaml 一致"
    }
    $mode = Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.PersistSchemaRegressionMode'
    Assert-True -Condition ($mode -ceq 'warn' -or $mode -ceq 'enforce') -Because "login 只认 warn / enforce,其余值启动即拒(当前 '$mode')"
}

Test-Case "player-locator ConfigMap 的 LeaseTTL == go/player_locator/etc/player_locator.yaml" {
    $block = Select-ManifestByName -Output $devOut -Name "go-svc-player-locator-config"
    Assert-True -Condition ($null -ne $block) -Because "DryRun 输出里应当有 go-svc-player-locator-config"
    $flat = ConvertTo-FlatManifest -Block $block

    foreach ($key in @('Node.LeaseTTL', 'Lease.DefaultTTLSeconds')) {
        $expected = Get-EtcValue -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath $key
        $actual = Get-FlatValue -Flat $flat -KeyPath "data.player_locator.yaml.$key"
        Assert-Equal -Expected $expected -Actual $actual -Because "$key 必须与 player_locator.yaml 一致"
    }
}

Test-Case 'player-locator 指标端口:ConfigMap Prometheus 段 0.0.0.0:9190 与 manifest 9190 metrics 端口 + prometheus 注解一致' {
    # 背景(B0-1,2026-10-18):会话对账改为全服单 leader 后,leader 卡死 / 无 leader 只能靠 player_locator 的
    # reconcile 指标与 deploy/k8s/player-locator-alerts.yaml 发现。以前 ConfigMap 没有 Prometheus 段(go-zero 的
    # StartAgent 在 Host 为空时直接 return)、manifest 也没有 metrics 端口,集群里一条序列都抓不到。
    # 三处要一起对上:ConfigMap 让进程监听,容器端口声明它,注解让 Prometheus 来抓;只有注解没有监听时抓取目标是 down。
    $block = @(Get-ManifestBlocks -Output $devOut | Where-Object {
        $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch '(?m)^  name: player-locator\s*$'
    }) | Select-Object -First 1
    Assert-True -Condition ($null -ne $block) -Because 'DryRun 输出里应当有 player-locator 的 Deployment'
    # 按行首锚定:manifest 的注释里也提到 9190 与 prometheus.io/port,不能让注释把断言喂饱。
    Assert-Match -Text $block -Pattern '(?m)^[ ]+- containerPort: 9190\s+name: metrics\s*$' -Because '容器必须声明名为 metrics 的 9190 端口'
    Assert-Match -Text $block -Pattern '(?m)^[ ]+prometheus\.io/scrape: "true"\s*$' -Because 'Pod 模板必须带抓取注解,否则指标端口开了也没人抓'
    Assert-Match -Text $block -Pattern '(?m)^[ ]+prometheus\.io/port: "9190"\s*$' -Because '抓取端口注解必须与容器的 metrics 端口一致'
    Assert-Match -Text $block -Pattern '(?m)^[ ]+prometheus\.io/path: "/metrics"\s*$' -Because '抓取路径注解必须是 /metrics'

    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-player-locator-config')
    Assert-Equal -Expected '0.0.0.0' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.player_locator.yaml.Prometheus.Host') -Because 'Host 为空 go-zero 不起 /metrics;绑 127.0.0.1(dev yaml 的值)时 Pod 外抓不到'
    Assert-Equal -Expected '9190' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.player_locator.yaml.Prometheus.Port') -Because 'ConfigMap 的端口必须与 manifest 的 9190 一致'
    Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath 'Prometheus.Port') -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.player_locator.yaml.Prometheus.Port') -Because '端口分工与 dev yaml 一致(9190 = player_locator)'
    Assert-Equal -Expected '/metrics' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.player_locator.yaml.Prometheus.Path') -Because '路径必须与注解一致'
    # Reconcile 各项是全服单 leader 的参数,事实源是代码默认值;ConfigMap 不得单独写值(dev yaml 一侧由 go/player_locator/internal/config/config_test.go 守住)。
    foreach ($key in @('Lease.ReconcileIntervalSeconds', 'Lease.Reconcile.AuditIntervalSeconds', 'Lease.Reconcile.ScanCount', 'Lease.Reconcile.ScanPagesPerSecond', 'Lease.Reconcile.MaxTransitionsPerSecond', 'Lease.Reconcile.DeadGateMembersPerRound')) {
        Assert-True -Condition (-not $flat.Scalars.Contains("data.player_locator.yaml.$key")) -Because "$key 不得写进 ConfigMap:全服必须一致,事实源是代码默认值"
    }
}

Test-Case "zone 内 go-svc Deployment 统一注入控制面命令 topic 契约,值 == bin/etc/base_deploy_config.yaml" {
    # 背景:go/shared/kafkacmd 不配 KAFKA_COMMAND_TOPIC_* 就回落到 256 / 1,而 C++ gate / scene 与 kafka-topic-init
    # 用的是 base_deploy_config.yaml 的代号(当前 2)。两边不一致时 login 的会话绑定、顶号踢人、scene-manager 的换场景
    # 等 Go → gate / scene 命令落进没人消费的 topic,静默丢失;本机 start_game.ps1 会注入,所以只有 K8s 上才出事。
    $partitions = Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.CommandTopicPartitions'
    $generation = Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.CommandTopicGeneration'
    foreach ($name in @('login', 'player-locator', 'scene-manager', 'db', 'data-service')) {
        # 按「kind: Deployment + metadata 里的名字」挑块:ConfigMap 里 go-zero 的 Name: 字段也可能等于服务名。
        $block = @(Get-ManifestBlocks -Output $devOut | Where-Object {
            $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch "(?m)^  name: $([regex]::Escape($name))\s*$"
        }) | Select-Object -First 1
        Assert-True -Condition ($null -ne $block) -Because "DryRun 输出里应当有 $name 的 Deployment"
        Assert-Match -Text $block -Pattern "- name: KAFKA_COMMAND_TOPIC_PARTITIONS\s+value: `"$partitions`"" -Because "$name 的命令分区数必须与 C++ / topic 预建同一契约"
        Assert-Match -Text $block -Pattern "- name: KAFKA_COMMAND_TOPIC_GENERATION\s+value: `"$generation`"" -Because "$name 的命令代号必须与 C++ / topic 预建同一契约"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, 'name: KAFKA_COMMAND_TOPIC_GENERATION').Count) -Because '只能注入一份'
        # topic 副本数同样统一注入:dev 档是单 broker,所以是 1(多 broker 的取值见下面「Kafka 拓扑」一节)。
        Assert-Match -Text $block -Pattern '- name: KAFKA_TOPIC_REPLICATION_FACTOR\s+value: "1"' -Because "$name 建 topic 的副本数必须与 broker 数同源,不能各服务各写各的"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, 'name: KAFKA_TOPIC_REPLICATION_FACTOR').Count) -Because '只能注入一份'
    }
}

Test-Case "data-service 是多副本形态:2 副本滚动更新 + PDB + 反亲和,ConfigMap 要求 store 必备,迁移 Job 先于 Deployment" {
    # 背景(docs/design/no-single-node-horizontal-scaling-20261001.md §4):data-service 管发号段与审计落库,
    # 以前钉成 1 副本 + Recreate。多副本的两个服务侧前提是迁移互斥与 Store.Required,部署侧要一起到位。
    $block = @(Get-ManifestBlocks -Output $devOut | Where-Object {
        $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch '(?m)^  name: data-service\s*$'
    }) | Select-Object -First 1
    Assert-True -Condition ($null -ne $block) -Because 'DryRun 输出里应当有 data-service 的 Deployment'
    Assert-Match -Text $block -Pattern '(?m)^  replicas: 2\s*$' -Because '单副本 = 这个 zone 的发号与审计落库是单点'
    Assert-NotMatch -Text $block -Pattern 'type: Recreate' -Because 'Recreate 会在发布时出现一个实例都没有的窗口'
    Assert-Match -Text $block -Pattern 'type: RollingUpdate\s+rollingUpdate:\s+maxSurge: 1\s+maxUnavailable: 0' -Because '先起新的再停旧的'
    Assert-Match -Text $block -Pattern 'podAntiAffinity' -Because '两个副本不该落在同一个节点上'
    Assert-Match -Text $block -Pattern '(?s)kind: PodDisruptionBudget.*?name: data-service-pdb.*?minAvailable: 1' -Because 'PDB 必须写在主 manifest 里,独立文件不会被 apply'
    Assert-Match -Text $block -Pattern 'fieldPath: status.podIP' -Because 'C++ 约定注册仍要通告 Pod IP'
    Assert-Match -Text $block -Pattern 'containerPort: 9260\s+name: metrics' -Because '多副本后要能按 Pod 抓指标'

    $configMap = Select-ManifestByName -Output $devOut -Name 'go-svc-data-service-config'
    Assert-Match -Text $configMap -Pattern '(?m)^\s+Store:\s*\r?\n\s+Required: true\s*$' -Because 'store 装配不起来的实例不能进发现池'
    Assert-Match -Text $configMap -Pattern 'MetricsListenAddr: ":9260"' -Because '指标端口要与 manifest 一致'

    $job = Select-ManifestByName -Output $devOut -Name 'data-service-migrate'
    Assert-True -Condition ($null -ne $job) -Because 'staging/prod 启动期不建表、不种号段行,必须有迁移 Job'
    Assert-Match -Text $job -Pattern 'args: \["-f", "/app/etc/data_service.yaml", "-migrate"\]' -Because 'Job 必须用与服务同一份配置跑 -migrate'
    # (?-i):占位是全大写;断言助手默认不分大小写,不关掉会把无关的小写双下划线词也算进去。
    Assert-NotMatch -Text $job -Pattern '(?-i)__[A-Z][A-Z0-9_]*__|PLACEHOLDER_' -Because '占位必须全部替换'
    $jobIndex = $devOut.IndexOf('name: data-service-migrate')
    $deployIndex = $devOut.IndexOf('name: data-service-pdb')
    Assert-True -Condition ($jobIndex -ge 0 -and $deployIndex -gt $jobIndex) -Because '迁移 Job 必须先于 Deployment apply'
}


Test-Case "db 是多副本形态:2 副本滚动更新 + PDB + 反亲和" {
    # 背景(docs/design/no-single-node-horizontal-scaling-20261001.md §3):db 把存档任务从 Kafka 落到 MySQL,以前每个 zone 钉成单实例。
    # 重试收据按实例归属之后(go/db/internal/kafka/retry_ownership.go)可以多开;部署侧要一起到位。
    $block = @(Get-ManifestBlocks -Output $devOut | Where-Object {
        $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch '(?m)^  name: db\s*$'
    }) | Select-Object -First 1
    Assert-True -Condition ($null -ne $block) -Because 'DryRun 输出里应当有 db 的 Deployment'
    Assert-Match -Text $block -Pattern '(?m)^  replicas: 2\s*$' -Because '单副本 = 这个 zone 的存档落库是单点'
    Assert-Match -Text $block -Pattern 'type: RollingUpdate\s+rollingUpdate:\s+maxSurge: 1\s+maxUnavailable: 0' -Because '先起新的再停旧的,发布期间不出现零实例'
    Assert-Match -Text $block -Pattern 'podAntiAffinity' -Because '两个副本不该落在同一个节点上'
    Assert-Match -Text $block -Pattern '(?s)kind: PodDisruptionBudget.*?name: db-pdb.*?minAvailable: 1' -Because 'PDB 必须写在主 manifest 里,独立文件不会被 apply'
    Assert-Match -Text $block -Pattern 'terminationGracePeriodSeconds: 45' -Because '停机要留出把重试收据还回 ready 的时间'
    Assert-Match -Text $block -Pattern '(?m)^  minReadySeconds: 20\s*$' -Because '新 Pod Ready 后至少等完一次入组再停旧 Pod(G6)'
}

Test-Case 'db 指标端口:ConfigMap MetricsListenAddr ":9160" 与 manifest 9160 metrics 端口 + prometheus 注解一致' {
    # 背景(G2b):以前 K8s 上 db 的 ConfigMap 不写 MetricsListenAddr(缺省空串 = go/db 不起 /metrics 端口),manifest 也没有这个端口,
    # 连接池指标 mmorpg_sql_pool_*{sql_client="db"} 与 deploy/k8s/owner-epoch-alerts.yaml 的抓取前提在集群里都不成立。
    # 三处要一起对上:ConfigMap 让进程监听,容器端口声明它,注解让 Prometheus 来抓。只对上一部分时没有任何报错 ——
    # 没有序列的告警规则不触发,和「一切正常」长得一样(deploy/k8s/mysql-connection-alerts.yaml 文件头的抓取前提)。
    $block = @(Get-ManifestBlocks -Output $devOut | Where-Object {
        $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch '(?m)^  name: db\s*$'
    }) | Select-Object -First 1
    Assert-True -Condition ($null -ne $block) -Because 'DryRun 输出里应当有 db 的 Deployment'
    # 按行首锚定:manifest 的注释里也提到 9160 与 prometheus.io/port,不能让注释把断言喂饱。
    Assert-Match -Text $block -Pattern '(?m)^[ ]+- containerPort: 9160\s+name: metrics\s*$' -Because '容器必须声明名为 metrics 的 9160 端口(与 data-service 的 9260 同形)'
    Assert-Match -Text $block -Pattern '(?m)^[ ]+prometheus\.io/scrape: "true"\s*$' -Because 'Pod 模板必须带抓取注解,否则指标端口开了也没人抓'
    Assert-Match -Text $block -Pattern '(?m)^[ ]+prometheus\.io/port: "9160"\s*$' -Because '抓取端口注解必须与容器的 metrics 端口一致'
    Assert-Match -Text $block -Pattern '(?m)^[ ]+prometheus\.io/path: "/metrics"\s*$' -Because '抓取路径注解必须是 /metrics'

    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-db-config')
    Assert-Equal -Expected ':9160' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.db.yaml.MetricsListenAddr') -Because 'ConfigMap 不写(或写别的端口)时,manifest 声明的 9160 上没有监听,抓取恒失败'
}


Test-Case "login 与 db 生成产物里的 Kafka.PartitionCnt 必须彼此相等" {
    # db 侧启动门禁 fail-closed:两边不一致直接起不来
    $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-login-config")
    $dbFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-db-config")
    $loginVal = Get-FlatValue -Flat $loginFlat -KeyPath 'data.login.yaml.Kafka.PartitionCnt'
    $dbVal = Get-FlatValue -Flat $dbFlat -KeyPath 'data.db.yaml.ServerConfig.Kafka.PartitionCnt'
    Assert-Equal -Expected $loginVal -Actual $dbVal -Because "login/db 的 PartitionCnt 不一致时 db 启动门禁会 fail-closed"
}

Test-Case "scene-manager ConfigMap 必须带 GateTokenSecret(否则跨 zone 重定向签不了令牌)" {
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-scene-manager-config")
    $v = Get-FlatValue -Flat $flat -KeyPath 'data.scene_manager_service.yaml.GateTokenSecret'
    Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($v)) -Because "gate_redirect.go 在空值时直接返回 'GateTokenSecret not configured'"
}

Test-Case "scene-manager ConfigMap 的 Timeout / KafkaWriteTimeoutSeconds / HomeZoneLookupTimeoutMs == go/scene_manager/etc/scene_manager_service.yaml" {
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-scene-manager-config")
    foreach ($key in @('Timeout', 'KafkaWriteTimeoutSeconds', 'HomeZoneLookupTimeoutMs')) {
        $expected = Get-EtcValue -RelativePath 'go/scene_manager/etc/scene_manager_service.yaml' -KeyPath $key
        $actual = Get-FlatValue -Flat $flat -KeyPath "data.scene_manager_service.yaml.$key"
        Assert-Equal -Expected $expected -Actual $actual -Because "$key 必须与 scene_manager_service.yaml 一致(生成器不得自带常数)"
    }
}

# EnterScene 第一条腿在"归属查询 + Kafka 同步写"之外的 Redis 往返余量(毫秒):读 location、预占频道、铸造 / CAS Lua、
# 路由失败时的回滚 Lua。口径 = cross-zone-scene-travel.md §12.2,与 scene_manager_service.yaml 的 Timeout 注释
# (8000 = 1500 + 5000 + 1500)是同一个数;全仓只在这里做判定。
$SceneManagerEnterSceneMarginMs = 1500

# scene-manager 超时预算判定。$Scalars = ConvertFrom-YamlToFlatMap 的 Scalars;$KeyPrefix 对 ConfigMap 是
# 'data.scene_manager_service.yaml.',对服务 yaml 是 ''。返回违例文本数组,空 = 通过。
function Get-SceneManagerTimeoutBudgetViolations {
    param(
        [Parameter(Mandatory = $true)][System.Collections.IDictionary]$Scalars,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$KeyPrefix
    )
    $violations = New-Object System.Collections.Generic.List[string]

    # go-zero(zrpc/server.go)只在 Timeout > 0 时装超时拦截器并带上 MethodTimeouts;k8s_deploy.ps1 只镜像标量 Timeout、
    # 不搬 MethodTimeouts。用它只放宽 EnterScene 时本地生效、K8s 静默退回全局值 —— 真要用,先在生成器里整块镜像
    # (Get-AuthoritativeYamlBlock)再改这里。
    $methodKeys = @($Scalars.Keys | Where-Object { $_ -like "${KeyPrefix}MethodTimeouts*" })
    if ($methodKeys.Count -gt 0) {
        $violations.Add("出现 MethodTimeouts($($methodKeys -join ', ')):k8s_deploy.ps1 不镜像它,只许用全局 Timeout")
    }

    # 三项都要显式正整数:Timeout <= 0 = 不装超时拦截器,EnterScene 应答时刻无上界;另两项 <= 0 时运行期回落默认值
    # (svc/servicecontext.go、logic/home_zone.go),这里不另抄一份默认值去猜。
    $values = @{}
    foreach ($key in @('Timeout', 'KafkaWriteTimeoutSeconds', 'HomeZoneLookupTimeoutMs')) {
        $raw = $Scalars["$KeyPrefix$key"]
        $parsed = [long]0
        if ($null -eq $raw -or -not [long]::TryParse([string]$raw, [ref]$parsed) -or $parsed -le 0) {
            $violations.Add("$key='$raw' 必须是正整数")
            continue
        }
        $values[$key] = $parsed
    }
    if ($values.Count -eq 3) {
        $need = $values['HomeZoneLookupTimeoutMs'] + $values['KafkaWriteTimeoutSeconds'] * 1000 + $SceneManagerEnterSceneMarginMs
        if ($values['Timeout'] -lt $need) {
            $violations.Add(("Timeout={0} < HomeZoneLookupTimeoutMs({1}) + KafkaWriteTimeoutSeconds({2})*1000 + {3} = {4}" -f $values['Timeout'], $values['HomeZoneLookupTimeoutMs'], $values['KafkaWriteTimeoutSeconds'], $SceneManagerEnterSceneMarginMs, $need))
        }
    }
    return $violations.ToArray()
}

Test-Case "scene-manager 的 zrpc Timeout 必须为正、盖住归属查询 + Kafka 同步写 + 1500ms 余量且不用 MethodTimeouts(否则 EnterScene 的失败应答回不到 C++ 源 scene)" {
    # 服务 yaml 与 K8s 产物各算一遍:以后谁调大 KafkaWriteTimeoutSeconds / HomeZoneLookupTimeoutMs 而忘了同步调
    # Timeout,或改用 MethodTimeouts,这里点名失败(cross-zone-scene-travel.md §12.2)。
    $yamlPath = 'go/scene_manager/etc/scene_manager_service.yaml'
    $views = @(
        @{ Name = 'K8s ConfigMap go-svc-scene-manager-config'; Prefix = 'data.scene_manager_service.yaml.'
           Scalars = (ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-scene-manager-config")).Scalars }
        @{ Name = $yamlPath; Prefix = ''
           Scalars = (ConvertFrom-YamlToFlatMap -Text (Get-Content -LiteralPath (Join-Path (Get-RepoRoot) $yamlPath) -Raw)).Scalars }
    )
    foreach ($v in $views) {
        $violations = @(Get-SceneManagerTimeoutBudgetViolations -Scalars $v.Scalars -KeyPrefix $v.Prefix)
        Assert-True -Condition ($violations.Count -eq 0) -Because ("{0}:{1}" -f $v.Name, ($violations -join ';'))
    }
}

Test-Case "自检:scene-manager 超时预算判定对 5000 / 2000 / 7999 / 0 / 缺键 / MethodTimeouts 报违例、对 8000 通过(守卫不得静默放行)" {
    $pass = (ConvertFrom-YamlToFlatMap -Text "Timeout: 8000`nKafkaWriteTimeoutSeconds: 5`nHomeZoneLookupTimeoutMs: 1500").Scalars
    $got = @(Get-SceneManagerTimeoutBudgetViolations -Scalars $pass -KeyPrefix '')
    Assert-True -Condition ($got.Count -eq 0) -Because ("8000 = 1500 + 5000 + 1500 应通过,实际报:{0}" -f ($got -join ';'))
    $cases = @(
        @{ Why = '改前本地值 5000'; Text = "Timeout: 5000`nKafkaWriteTimeoutSeconds: 5`nHomeZoneLookupTimeoutMs: 1500" }
        @{ Why = '改前 K8s 落的 go-zero 默认 2000'; Text = "Timeout: 2000`nKafkaWriteTimeoutSeconds: 5`nHomeZoneLookupTimeoutMs: 1500" }
        @{ Why = '差 1ms(余量是 1500 不是 1000)'; Text = "Timeout: 7999`nKafkaWriteTimeoutSeconds: 5`nHomeZoneLookupTimeoutMs: 1500" }
        @{ Why = 'Timeout 0 = 不装超时拦截器'; Text = "Timeout: 0`nKafkaWriteTimeoutSeconds: 5`nHomeZoneLookupTimeoutMs: 1500" }
        @{ Why = '缺 Timeout 键(go-zero 会落默认 2000)'; Text = "KafkaWriteTimeoutSeconds: 5`nHomeZoneLookupTimeoutMs: 1500" }
        @{ Why = 'KafkaWriteTimeoutSeconds 0(运行期回落 5s,判定不猜)'; Text = "Timeout: 8000`nKafkaWriteTimeoutSeconds: 0`nHomeZoneLookupTimeoutMs: 1500" }
        @{ Why = 'MethodTimeouts 只放宽 EnterScene(K8s 不镜像)'; Text = "Timeout: 8000`nKafkaWriteTimeoutSeconds: 5`nHomeZoneLookupTimeoutMs: 1500`nMethodTimeouts:`n  - FullMethod: /scene_manager.SceneManager/EnterScene`n    Timeout: 8s" }
    )
    foreach ($c in $cases) {
        $got = @(Get-SceneManagerTimeoutBudgetViolations -Scalars (ConvertFrom-YamlToFlatMap -Text $c.Text).Scalars -KeyPrefix '')
        Assert-True -Condition ($got.Count -gt 0) -Because "$($c.Why) 必须报违例"
    }
    $prefixed = [ordered]@{ 'data.scene_manager_service.yaml.Timeout' = '7999'; 'data.scene_manager_service.yaml.KafkaWriteTimeoutSeconds' = '5'; 'data.scene_manager_service.yaml.HomeZoneLookupTimeoutMs' = '1500' }
    Assert-True -Condition (@(Get-SceneManagerTimeoutBudgetViolations -Scalars $prefixed -KeyPrefix 'data.scene_manager_service.yaml.').Count -gt 0) -Because "ConfigMap 前缀下的 7999 也必须报违例"
}

# login → scene-manager 的调用方超时必须 ≥ scene-manager 服务端 Timeout + 2000(上游比下游宽,与下面 C++ deadline 同一条规则)。
# 小了的话 scene-manager 还在落点 / 推路由时 login 就按失败收口、给客户端推 3023,服务端随后却可能已放行(结果未知)。
# 返回违例文本数组,空 = 通过。$LoginTimeout / $SceneManagerTimeout 是原样字符串(缺键为 $null)。
function Get-LoginSceneManagerRpcTimeoutViolations {
    param(
        [AllowNull()][object]$LoginTimeout,
        [AllowNull()][object]$SceneManagerTimeout
    )
    $login = [long]0
    $sm = [long]0
    if ($null -eq $LoginTimeout -or -not [long]::TryParse([string]$LoginTimeout, [ref]$login) -or $login -le 0) {
        return @("login SceneManagerRpc.Timeout='$LoginTimeout' 必须是正整数")
    }
    if ($null -eq $SceneManagerTimeout -or -not [long]::TryParse([string]$SceneManagerTimeout, [ref]$sm) -or $sm -le 0) {
        return @("scene-manager Timeout='$SceneManagerTimeout' 必须是正整数")
    }
    if ($login -lt $sm + 2000) {
        return @(("login SceneManagerRpc.Timeout={0} < scene-manager Timeout({1}) + 2000 = {2}" -f $login, $sm, ($sm + 2000)))
    }
    return @()
}

Test-Case "login 调 scene-manager 的 SceneManagerRpc.Timeout ≥ scene-manager Timeout + 2000(仓库 yaml 与 K8s 产物各算一遍)" {
    $root = Get-RepoRoot
    $loginYaml = (ConvertFrom-YamlToFlatMap -Text (Get-Content -LiteralPath (Join-Path $root 'go/login/etc/login.yaml') -Raw)).Scalars
    $smYaml = (ConvertFrom-YamlToFlatMap -Text (Get-Content -LiteralPath (Join-Path $root 'go/scene_manager/etc/scene_manager_service.yaml') -Raw)).Scalars
    $got = @(Get-LoginSceneManagerRpcTimeoutViolations -LoginTimeout $loginYaml['SceneManagerRpc.Timeout'] -SceneManagerTimeout $smYaml['Timeout'])
    Assert-True -Condition ($got.Count -eq 0) -Because ("仓库 yaml:{0}" -f ($got -join ';'))

    $loginFlat = (ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-login-config')).Scalars
    $smFlat = (ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-scene-manager-config')).Scalars
    $got = @(Get-LoginSceneManagerRpcTimeoutViolations -LoginTimeout $loginFlat['data.login.yaml.SceneManagerRpc.Timeout'] -SceneManagerTimeout $smFlat['data.scene_manager_service.yaml.Timeout'])
    Assert-True -Condition ($got.Count -eq 0) -Because ("K8s ConfigMap:{0}" -f ($got -join ';'))
    Assert-Equal -Expected ([string]$loginYaml['SceneManagerRpc.Timeout']) -Actual ([string]$loginFlat['data.login.yaml.SceneManagerRpc.Timeout']) -Because "login ConfigMap 的 SceneManagerRpc.Timeout 必须镜像 login.yaml(生成器不得自带常数)"
}

Test-Case "自检:login → scene-manager 超时判定对 5000 / 9999 / 0 / 缺键报违例、对 10000 通过(守卫不得静默放行)" {
    Assert-True -Condition (@(Get-LoginSceneManagerRpcTimeoutViolations -LoginTimeout '10000' -SceneManagerTimeout '8000').Count -eq 0) -Because "10000 = 8000 + 2000 应通过"
    foreach ($c in @(
            @{ Why = '改前的 5000'; Login = '5000'; Sm = '8000' }
            @{ Why = '差 1ms'; Login = '9999'; Sm = '8000' }
            @{ Why = 'login 0'; Login = '0'; Sm = '8000' }
            @{ Why = 'login 缺键'; Login = $null; Sm = '8000' }
            @{ Why = 'scene-manager 缺键'; Login = '10000'; Sm = $null }
        )) {
        Assert-True -Condition (@(Get-LoginSceneManagerRpcTimeoutViolations -LoginTimeout $c.Login -SceneManagerTimeout $c.Sm).Count -gt 0) -Because "$($c.Why) 必须报违例"
    }
}

# ─────────────────────────────────────────────────────────────────
# C++ gRPC 客户端 deadline 预算(docs/design/grpc-muduo-transport.md §16.3 / §16.4)
# ─────────────────────────────────────────────────────────────────
# 上面守的是 scene-manager 服务端 Timeout 盖住它自己的内部预算;这里守反方向:C++ deadline ≥ 目标 Go 服务的
# zrpc Timeout + 2000(上游比下游宽),以及 node ConfigMap 把 GrpcClient 块原样搬进集群。
# 判定规则只有一份,在 k8s_deploy.ps1:Get-GrpcClientDeadlineBudgetViolations(纯函数)+ Assert-GrpcClientDeadlineBudget
# (读文件、不满足即 throw,由写路径入口调用)。这里按 AST 把这组函数(名单见下)抽出来直接调用,不另抄一份判定,也不执行脚本入口
# (与 k8s_migrate_gate.tests.ps1 同一做法)。它们用到的 ConvertFrom-YamlToFlatMap 来自 deploy_capture.ps1 dot-source 的
# release_common.ps1;Assert 读 $RepoRoot,由各用例自己设。
$deadlineParseTokens = $null
$deadlineParseErrors = $null
$deadlineDeployAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$deadlineParseTokens, [ref]$deadlineParseErrors)
if ($deadlineParseErrors.Count -gt 0) { throw "k8s_deploy.ps1 语法错误: $($deadlineParseErrors.Message -join '; ')" }
$deadlineFunctionNames = @(
    'ConvertTo-ZrpcDurationMs', 'Get-ZrpcMethodTimeouts', 'Get-GrpcClientMethodsNotCalledFromCpp',
    'Get-GrpcClientDeadlineBudgetViolations', 'Assert-GrpcClientDeadlineBudget'
)
foreach ($deadlineFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($deadlineFunctionNames -contains $deadlineFnAst.Name) {
        Set-Item -Path "Function:script:$($deadlineFnAst.Name)" -Value $deadlineFnAst.Body.GetScriptBlock()
    }
}
foreach ($deadlineFnName in $deadlineFunctionNames) {
    if (-not (Test-Path "Function:$deadlineFnName")) { throw "k8s_deploy.ps1 缺少函数 $deadlineFnName(deadline 预算守卫被删了?后续断言无意义)" }
}

# ConfigMap 拍平后的键带 data.<文件名>. 前缀;去掉它才能与服务 yaml 用同一个判定函数、同一套键名。
function ConvertTo-UnprefixedScalars {
    param(
        [Parameter(Mandatory = $true)][System.Collections.IDictionary]$Scalars,
        [Parameter(Mandatory = $true)][string]$Prefix
    )
    $out = [ordered]@{}
    foreach ($key in $Scalars.Keys) {
        if (([string]$key).StartsWith($Prefix, [System.StringComparison]::Ordinal)) {
            $out[([string]$key).Substring($Prefix.Length)] = $Scalars[$key]
        }
    }
    return $out
}

# node ConfigMap 以 readOnly 整目录挂到 /app/bin/etc,完全遮蔽镜像里的 base_deploy_config.yaml:漏搬 GrpcClient 块 =
# K8s 上所有目标落回 C++ 内置默认 10000,号段 fetchTimeout / 换图在途 TTL 跟着偏离仓库口径,且不会有任何报错。
function Assert-GrpcClientBlockMirrored {
    param(
        [Parameter(Mandatory = $true)][string]$Output,
        [Parameter(Mandatory = $true)][string]$ConfigMapName
    )
    $authoritative = (ConvertFrom-YamlToFlatMap -Text (Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'bin/etc/base_deploy_config.yaml') -Raw)).Scalars
    $expectedKeys = @($authoritative.Keys | Where-Object { $_ -like 'GrpcClient.CallDeadlineMs.*' })
    Assert-True -Condition ($expectedKeys.Count -gt 0) -Because 'bin/etc/base_deploy_config.yaml 里应当有 GrpcClient.CallDeadlineMs(本断言的前提)'
    $generated = ConvertTo-UnprefixedScalars -Scalars (ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $Output -Name $ConfigMapName)).Scalars -Prefix 'data.base_deploy_config.yaml.'
    $generatedKeys = @($generated.Keys | Where-Object { $_ -like 'GrpcClient.CallDeadlineMs.*' })
    Assert-Equal -Expected ($expectedKeys -join ',') -Actual ($generatedKeys -join ',') -Because "$ConfigMapName 的 GrpcClient.CallDeadlineMs 键集合必须与权威文件逐项相同(整块搬运,不增不减)"
    foreach ($key in $expectedKeys) {
        Assert-Equal -Expected $authoritative[$key] -Actual $generated[$key] -Because "$ConfigMapName 的 $key 必须与 bin/etc/base_deploy_config.yaml 一致"
    }
}

Test-Case "C++ gRPC deadline 预算:仓库配置满足 deadline ≥ 目标 Go 服务 zrpc Timeout + 2000,六个目标(含 PlayerInbox)全部显式配置(部署门禁对当前配置放行)" {
    # 不满足时 Assert 抛出的文本就是失败原因,逐项点名是哪个目标、差多少。
    $RepoRoot = Get-RepoRoot
    Assert-GrpcClientDeadlineBudget | Out-Null
}

Test-Case "zone-up 写路径入口先过 deadline 预算门禁,之后才有任何 kubectl 写操作" {
    $okAt = $devOut.IndexOf('GrpcClient deadline budget OK')
    $firstKubectlAt = $devOut.IndexOf('[dry-run] kubectl')
    Assert-True -Condition ($okAt -ge 0) -Because 'zone-up DryRun 必须经过 Assert-GrpcClientDeadlineBudget(写路径入口门禁),否则守卫等于没挂'
    Assert-True -Condition ($firstKubectlAt -lt 0 -or $okAt -lt $firstKubectlAt) -Because '预算核对必须先于任何 kubectl 写操作,不留半截部署'
}

Test-Case "node ConfigMap 的 GrpcClient.CallDeadlineMs 逐项 == bin/etc/base_deploy_config.yaml(ConfigMap 遮蔽镜像,漏搬 = K8s 上全部落回默认 10000)" {
    Assert-GrpcClientBlockMirrored -Output $devOut -ConfigMapName 'node-config'
}

# ConfigMap 里某个内嵌文件(data: <文件名>: |)的原文,去掉块缩进。找不到即 throw(fail-closed)。
function Get-ConfigMapEmbeddedText {
    param(
        [Parameter(Mandatory = $true)][string]$Block,
        [Parameter(Mandatory = $true)][string]$FileName
    )
    $lines = ($Block -replace "`r`n", "`n") -split "`n"
    $start = -1
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match ('^  ' + [regex]::Escape($FileName) + ':\s*\|')) { $start = $i; break }
    }
    if ($start -lt 0) { throw "ConfigMap 里找不到内嵌文件 $FileName" }
    $out = New-Object System.Collections.Generic.List[string]
    for ($i = $start + 1; $i -lt $lines.Count; $i++) {
        $line = $lines[$i]
        if ($line.Length -eq 0 -or $line -match '^\s*$') { $out.Add(''); continue }
        if (-not $line.StartsWith('    ')) { break }
        $out.Add($line.Substring(4))
    }
    return ($out -join "`n")
}

Test-Case "K8s 生成物同样满足 deadline 预算:node-config 的 GrpcClient × zone 内 go-svc ConfigMap 实际写出的 Timeout(login 镜像 login.yaml、data-service 不写)" {
    # 部署门禁比对的是服务 yaml;这里再按集群里真正生效的那份核一遍不等式,防的是生成器与门禁悄悄分家:
    # scene-manager / login 的 Timeout 镜像被改回常数(login 以前就是模板里写死 100000),或 data-service 的 ConfigMap
    # 某天写出一个服务 yaml 里没有的 Timeout(它现在不写 = go-zero 默认 2000)。ConfigMap 键改名会让 Timeout 落回
    # 默认 2000、本条反而放行 —— 那一类由上面 login / scene-manager 的「ConfigMap 值 == 服务 yaml」逐键用例兜住(查不到键即失败)。
    # 全局服务(match / 路由服)不在 zone-up 产物里,它们的 ConfigMap Timeout 由 Get-AuthoritativeScalar 镜像服务 yaml,已被上面的门禁用例覆盖。
    $deployScalars = ConvertTo-UnprefixedScalars -Scalars (ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'node-config')).Scalars -Prefix 'data.base_deploy_config.yaml.'
    $targets = [ordered]@{}
    foreach ($t in @(
        @{ Target = 'SceneManagerNodeService'; ConfigMap = 'go-svc-scene-manager-config'; Prefix = 'data.scene_manager_service.yaml.'; File = 'scene_manager_service.yaml' }
        @{ Target = 'LoginNodeService';        ConfigMap = 'go-svc-login-config';         Prefix = 'data.login.yaml.';                File = 'login.yaml' }
        @{ Target = 'DataServiceNodeService';  ConfigMap = 'go-svc-data-service-config';  Prefix = 'data.data_service.yaml.';         File = 'data_service.yaml' }
    )) {
        $block = Select-ManifestByName -Output $devOut -Name $t.ConfigMap
        $flat = ConvertTo-FlatManifest -Block $block
        $targets[$t.Target] = @{ Source = "K8s ConfigMap $($t.ConfigMap)"; Scalars = (ConvertTo-UnprefixedScalars -Scalars $flat.Scalars -Prefix $t.Prefix) }
        # 2026-10-16 起 data-service 的 ConfigMap 镜像了三个 Rollback* 的 MethodTimeouts(回档帮会闸):与部署门禁同样逐条核对,
        # 解析的是 ConfigMap 里真正生效的那份文本,登记为「C++ 不调用」的方法才可以高于 deadline。
        $embedded = Get-ConfigMapEmbeddedText -Block $block -FileName $t.File
        if ($embedded -match '(?m)^MethodTimeouts\s*:') {
            $targets[$t.Target]['MethodTimeouts'] = @(Get-ZrpcMethodTimeouts -Text $embedded)
            $notCalled = Get-GrpcClientMethodsNotCalledFromCpp
            if ($notCalled.Contains($t.Target)) { $targets[$t.Target]['MethodsNotCalledFromCpp'] = @($notCalled[$t.Target]) }
        }
    }
    $violations = @(Get-GrpcClientDeadlineBudgetViolations -DeployScalars $deployScalars -Targets $targets)
    Assert-True -Condition ($violations.Count -eq 0) -Because ("K8s 生成物违例:{0}" -f ($violations -join ';'))
}

Test-Case "自检:deadline 预算判定对 修复前的 2500 / 差 1ms / 缺席 / 0 / 非数字 / 服务端 Timeout 0 / 非数字 报违例并点名目标,对等号边界通过(守卫不得静默放行)" {
    $check = {
        param([string]$DeployText, [string]$ServiceText)
        $targets = [ordered]@{
            DataServiceNodeService = @{ Source = 'fixture/data_service.yaml'; Scalars = (ConvertFrom-YamlToFlatMap -Text $ServiceText).Scalars }
        }
        return @(Get-GrpcClientDeadlineBudgetViolations -DeployScalars (ConvertFrom-YamlToFlatMap -Text $DeployText).Scalars -Targets $targets)
    }
    $deployWith = "GrpcClient:`n  CallDeadlineMs:`n    DataServiceNodeService: "

    $passes = @(
        @{ Why = '4000 + 未写 Timeout(go-zero 默认 2000,等号边界)'; Deploy = "${deployWith}4000"; Service = "Name: dataservice.rpc" }
        @{ Why = '10000 + Timeout 8000(等号边界)'; Deploy = "${deployWith}10000"; Service = "Timeout: 8000" }
    )
    foreach ($c in $passes) {
        $got = @(& $check $c.Deploy $c.Service)
        Assert-True -Condition ($got.Count -eq 0) -Because ("{0} 应通过,实际报:{1}" -f $c.Why, ($got -join ';'))
    }

    $violationCases = @(
        @{ Why = '修复前的仓库值 2500 + 未写 Timeout(2500 < 2000 + 2000)'; Deploy = "${deployWith}2500"; Service = "Name: dataservice.rpc" }
        @{ Why = '差 1ms(余量是 2000)'; Deploy = "${deployWith}9999"; Service = "Timeout: 8000" }
        @{ Why = 'deadline 缺席(C++ 落到内置默认,预算不能依赖隐式值)'; Deploy = "GrpcClient:`n  CallDeadlineMs:`n    EtcdNodeService: 5000"; Service = "Timeout: 1000" }
        @{ Why = 'deadline 0(C++ 忽略后按默认)'; Deploy = "${deployWith}0"; Service = "Timeout: 1000" }
        @{ Why = 'deadline 非数字'; Deploy = "${deployWith}4s"; Service = "Timeout: 1000" }
        @{ Why = '服务端 Timeout 0(go-zero 不装超时拦截器)'; Deploy = "${deployWith}10000"; Service = "Timeout: 0" }
        @{ Why = '服务端 Timeout 非数字'; Deploy = "${deployWith}10000"; Service = "Timeout: 8s" }
    )
    foreach ($c in $violationCases) {
        $got = @(& $check $c.Deploy $c.Service)
        Assert-True -Condition ($got.Count -gt 0) -Because "$($c.Why) 必须报违例"
        Assert-Match -Text $got[0] -Pattern '^DataServiceNodeService:' -Because "$($c.Why) 的违例必须点名是哪个目标"
    }
}

# go-zero 的 MethodTimeouts 能把个别方法的服务端超时单独放宽。门禁逐条核对:C++ 会调用的方法照样要满足
# 「deadline ≥ 该方法超时 + 2000」;只有登记为「C++ 不调用」的方法(Get-GrpcClientMethodsNotCalledFromCpp)可以更高。
# 2026-10-09 之前的口径是「出现 MethodTimeouts 就拒绝」,它与 data_service 回档三个 RPC 的 MethodTimeouts 撞在一起,
# 使所有写路径在入口被拒(docs/handoff/repo-layout-20261007.md §6.3)。
Test-Case "自检:MethodTimeouts 逐条核对,C++ 会调用的方法必须在预算内,登记为 C++ 不调用的方法才可以更高(守卫不得静默放行)" {
    $check = {
        param([string]$Deadline, [string]$ServiceText, [string[]]$NotCalledFromCpp = @(), [switch]$OmitParsedEntries)
        $target = @{
            Source                  = 'fixture/data_service.yaml'
            Scalars                 = (ConvertFrom-YamlToFlatMap -Text $ServiceText).Scalars
            MethodsNotCalledFromCpp = $NotCalledFromCpp
        }
        if (-not $OmitParsedEntries) { $target['MethodTimeouts'] = @(Get-ZrpcMethodTimeouts -Text $ServiceText) }
        $deployText = "GrpcClient:`n  CallDeadlineMs:`n    DataServiceNodeService: $Deadline"
        return @(Get-GrpcClientDeadlineBudgetViolations -DeployScalars (ConvertFrom-YamlToFlatMap -Text $deployText).Scalars -Targets ([ordered]@{ DataServiceNodeService = $target }))
    }
    $allocate = '/data_service.DataService/AllocateIdSegment'
    $rollback = '/data_service.DataService/RollbackPlayer'
    $block = {
        param([string[]]$Lines)
        return "MethodTimeouts:`n" + ($Lines -join "`n")
    }
    $entry = { param([string]$Method, [string]$Timeout) return "  - FullMethod: $Method`n    Timeout: $Timeout" }

    $passes = @(
        @{ Why = '等号边界:8s + 2000 = 10000'; Deadline = '10000'; Service = (& $block (& $entry $allocate '8s')) }
        @{ Why = '小数时长不多算:16.1s 恰好是 16100,加 2000 等于 18100'; Deadline = '18100'; Service = (& $block (& $entry $allocate '16.1s')) }
        @{ Why = '逐方法超时比全局默认还短'; Deadline = '4000'; Service = (& $block (& $entry $allocate '500ms')) }
        @{ Why = '登记为 C++ 不调用的方法可以远高于 deadline'; Deadline = '4000'; Service = (& $block (& $entry $rollback '14400s')); NotCalled = @($rollback) }
        @{ Why = '豁免只放行登记的那一条,另一条照常在预算内'; Deadline = '10000'; Service = (& $block @((& $entry $rollback '300s'), (& $entry $allocate '8s'))); NotCalled = @($rollback) }
        @{ Why = '显式空表 MethodTimeouts: []'; Deadline = '4000'; Service = 'MethodTimeouts: []' }
        @{ Why = '键名小写(go-zero 配置键不分大小写)且在预算内'; Deadline = '10000'; Service = "methodTimeouts:`n  - fullMethod: $allocate`n    timeout: 8s" }
    )
    foreach ($c in $passes) {
        $notCalled = if ($c.ContainsKey('NotCalled')) { $c.NotCalled } else { @() }
        $got = @(& $check $c.Deadline $c.Service $notCalled)
        Assert-True -Condition ($got.Count -eq 0) -Because ("{0} 应通过,实际报:{1}" -f $c.Why, ($got -join ';'))
    }

    $violationCases = @(
        @{ Why = '差 1ms(8s + 2000 > 9999)'; Deadline = '9999'; Service = (& $block (& $entry $allocate '8s')); Pattern = 'AllocateIdSegment 8s' }
        @{ Why = '回档方法没登记豁免就不放行,并提示登记到哪'; Deadline = '4000'; Service = (& $block (& $entry $rollback '300s')); Pattern = 'RollbackPlayer 300s.*Get-GrpcClientMethodsNotCalledFromCpp' }
        @{ Why = '登记的是别的方法,这一条不沾光'; Deadline = '4000'; Service = (& $block (& $entry $allocate '300s')); NotCalled = @($rollback); Pattern = 'AllocateIdSegment 300s' }
        @{ Why = '豁免逐字匹配,大小写不同不算(go-zero 按 FullMethod 原样查表)'; Deadline = '4000'; Service = (& $block (& $entry $rollback '300s')); NotCalled = @($rollback.ToLowerInvariant()); Pattern = 'RollbackPlayer 300s' }
        @{ Why = 'Timeout 是裸数字(go-zero 加载配置时直接报错,几乎一定是把毫秒写错了地方)'; Deadline = '10000'; Service = (& $block (& $entry $allocate '8000')); Pattern = "AllocateIdSegment 的 Timeout='8000' 解析不了" }
        @{ Why = 'Timeout 为 0'; Deadline = '10000'; Service = (& $block (& $entry $allocate '0s')); Pattern = '解析不了' }
        @{ Why = 'Timeout 单位大写(Go 不认)'; Deadline = '10000'; Service = (& $block (& $entry $allocate '8S')); Pattern = '解析不了' }
        @{ Why = 'FullMethod 不是 /包.服务/方法'; Deadline = '10000'; Service = (& $block (& $entry 'AllocateIdSegment' '1s')); Pattern = "FullMethod='AllocateIdSegment' 不是" }
        @{ Why = '键名小写同样被核到(否则换个大小写就绕过门禁)'; Deadline = '4000'; Service = "methodTimeouts:`n  - fullMethod: $allocate`n    timeout: 300s"; Pattern = 'AllocateIdSegment 300s' }
        @{ Why = '豁免不跳过全局 Timeout 的核对'; Deadline = '9999'; Service = ("Timeout: 8000`n" + (& $block (& $entry $rollback '300s'))); NotCalled = @($rollback); Pattern = 'Timeout 8000 \+ 2000 = 10000' }
        @{ Why = '豁免只免比大小,不免格式:豁免条目的超时写坏了也要报'; Deadline = '4000'; Service = (& $block (& $entry $rollback 'abc')); NotCalled = @($rollback); Pattern = "RollbackPlayer 的 Timeout='abc' 解析不了" }
        @{ Why = '豁免条目的超时是裸数字同样要报'; Deadline = '4000'; Service = (& $block (& $entry $rollback '3600')); NotCalled = @($rollback); Pattern = "RollbackPlayer 的 Timeout='3600' 解析不了" }
        @{ Why = '有这个键却一条也没有(键后什么都没写)'; Deadline = '4000'; Service = "MethodTimeouts:`nName: x"; Pattern = '有 MethodTimeouts 键却没有解析出任何条目' }
    )
    foreach ($c in $violationCases) {
        $notCalled = if ($c.ContainsKey('NotCalled')) { $c.NotCalled } else { @() }
        $got = @(& $check $c.Deadline $c.Service $notCalled)
        Assert-True -Condition ($got.Count -gt 0) -Because "$($c.Why) 必须报违例"
        Assert-Match -Text $got[0] -Pattern '^DataServiceNodeService:' -Because "$($c.Why) 的违例必须点名是哪个目标"
        Assert-Match -Text ($got -join "`n") -Pattern $c.Pattern -Because "$($c.Why) 的违例必须说清是哪一条、为什么"
    }

    # 服务 yaml 里明明有 MethodTimeouts,调用方却没把逐方法解析结果交进来:不能当作没有,必须拒绝。
    $got = @(& $check '10000' (& $block (& $entry $allocate '8s')) @() -OmitParsedEntries)
    Assert-True -Condition ($got.Count -gt 0) -Because '有 MethodTimeouts 却没给逐方法解析结果必须报违例'
    Assert-Match -Text $got[0] -Pattern '^DataServiceNodeService:.*没有拿到逐方法的解析结果' -Because '违例要说明缺的是逐方法解析结果'
    $got = @(& $check '10000' "methodTimeouts: [{FullMethod: $allocate, Timeout: 1h}]" @() -OmitParsedEntries)
    Assert-True -Condition ($got.Count -gt 0) -Because '键名小写时同样要认出「有 MethodTimeouts 却没给逐方法解析结果」(go-zero 的键不分大小写)'
    Assert-Match -Text $got[0] -Pattern '^DataServiceNodeService:.*没有拿到逐方法的解析结果' -Because '小写键的违例同样要说明缺的是逐方法解析结果'

    # 下面三种是「两套解析对不上」的情形,直接喂构造值(绕开 Get-ZrpcMethodTimeouts,它自己会先 throw)。
    $deployScalars = (ConvertFrom-YamlToFlatMap -Text "GrpcClient:`n  CallDeadlineMs:`n    DataServiceNodeService: 4000").Scalars
    $violationsFor = {
        param([string]$ServiceText, [object[]]$Entries)
        $target = @{
            Source                  = 'fixture/data_service.yaml'
            Scalars                 = (ConvertFrom-YamlToFlatMap -Text $ServiceText).Scalars
            MethodTimeouts          = $Entries
            MethodsNotCalledFromCpp = @($rollback)
        }
        return @(Get-GrpcClientDeadlineBudgetViolations -DeployScalars $deployScalars -Targets ([ordered]@{ DataServiceNodeService = $target }))
    }

    # 条数对不上(解析器漏读 / 调用方传错文件)。
    $got = @(& $violationsFor (& $block @((& $entry $rollback '300s'), (& $entry $allocate '300s'))) @(@{ FullMethod = $rollback; Timeout = '300s' }))
    Assert-True -Condition ($got.Count -gt 0) -Because '解析出 1 条而拍平表里有 2 条必须报违例(否则漏掉的那条 AllocateIdSegment 300s 就被放过了)'
    Assert-Match -Text $got[0] -Pattern '解析出 1 条,拍平表里数到 2 条' -Because '违例要给出两边的条数'

    # 交进来了逐方法条目,拍平表里却没有这个键:条目不能因此不被核对。
    $got = @(& $violationsFor 'Name: x' @(@{ FullMethod = $allocate; Timeout = '300s' }))
    Assert-True -Condition ($got.Count -gt 0) -Because '有逐方法条目而拍平表里没有 MethodTimeouts 键必须报违例(不能让这条 300s 不经核对)'
    Assert-Match -Text $got[0] -Pattern '解析出 1 条,拍平表里数到 0 条' -Because '违例要给出两边的条数'

    # 带引号的键 + 流式写法:拍平表认得出键、数不出条目,解析结果也是 0 条。0 == 0 不能算通过
    # (go-zero 走完整的 YAML 解析,这张表在服务端照常生效;评审时抓到的误放)。
    $flowText = "`"MethodTimeouts`": [{FullMethod: $allocate, Timeout: 1h}]"
    $got = @(& $violationsFor $flowText @())
    Assert-True -Condition ($got.Count -gt 0) -Because '有 MethodTimeouts 键、两边都数出 0 条、值又不是 [] 时必须报违例,不能当成空表放行'
    Assert-Match -Text $got[0] -Pattern '有 MethodTimeouts 键却没有解析出任何条目' -Because '违例要说明是有键却读不出条目'
}

Test-Case "自检:Get-ZrpcMethodTimeouts 读得出块式写法的每一条,读不懂的一律 throw(不拿读错的表去核预算)" {
    $text = @(
        'Name: dataservice.rpc'
        'MethodTimeouts:  # 逐方法超时'
        '  - FullMethod: /a.B/One   # 行尾注释'
        '    Timeout: 300s'
        ''
        '  # 两个键的先后顺序可以反过来,值可以带引号'
        '  - Timeout: "1m30s"'
        "    FullMethod: '/a.B/Two'"
        'Redis:'
        '  Timeout: 5s'
        '  FullMethod: /not.A/Method'
    ) -join "`n"
    foreach ($variant in @($text, ($text -replace "`n", "`r`n"))) {
        $got = @(Get-ZrpcMethodTimeouts -Text $variant)
        Assert-Equal -Expected 2 -Actual $got.Count -Because '应当读出两条(LF 与 CRLF 结果相同),块在下一个顶层键 Redis 处结束'
        Assert-Equal -Expected '/a.B/One' -Actual $got[0].FullMethod -Because '第一条的方法名'
        Assert-Equal -Expected '300s' -Actual $got[0].Timeout -Because '第一条的超时(行尾注释要去掉)'
        Assert-Equal -Expected '/a.B/Two' -Actual $got[1].FullMethod -Because '第二条的方法名(引号要去掉)'
        Assert-Equal -Expected '1m30s' -Actual $got[1].Timeout -Because '第二条的超时与方法名各归各位,不能串到别的条目上'
    }

    $lower = @(Get-ZrpcMethodTimeouts -Text "methodtimeouts:`n  - fullmethod: /a.B/One`n    timeout: 1s")
    Assert-Equal -Expected '/a.B/One=1s' -Actual ("{0}={1}" -f $lower[0].FullMethod, $lower[0].Timeout) -Because '键名不分大小写(与 go-zero 的配置加载一致)'
    Assert-Equal -Expected 0 -Actual @(Get-ZrpcMethodTimeouts -Text "Name: x`nTimeout: 8000").Count -Because '没有这个键 = 空'
    Assert-Equal -Expected 0 -Actual @(Get-ZrpcMethodTimeouts -Text 'MethodTimeouts: []').Count -Because '显式空表 = 空'
    Assert-Equal -Expected 0 -Actual @(Get-ZrpcMethodTimeouts -Text '').Count -Because '空文本 = 空'
    Assert-Equal -Expected 0 -Actual @(Get-ZrpcMethodTimeouts -Text "Name: x  # 注释里提到 MethodTimeouts 不算`n# MethodTimeouts: [{FullMethod: /a.B/One, Timeout: 1h}]").Count -Because '只在注释里出现不算'

    $one = "  - FullMethod: /a.B/One`n    Timeout: 1h"
    $malformed = @(
        @{ Why = '流式写法'; Text = 'MethodTimeouts: [{FullMethod: /a.B/One, Timeout: 1s}]'; Pattern = '只认块式写法' }
        @{ Why = '条目里有未知键'; Text = "MethodTimeouts:`n  - FullMethod: /a.B/One`n    Timeout: 1s`n    Retries: 3"; Pattern = '未知键 Retries' }
        @{ Why = '同一条目里键重复'; Text = "MethodTimeouts:`n  - FullMethod: /a.B/One`n    Timeout: 1s`n    Timeout: 2s"; Pattern = 'Timeout 写了两次' }
        @{ Why = '条目缺 Timeout'; Text = "MethodTimeouts:`n  - FullMethod: /a.B/One"; Pattern = '缺 FullMethod 或 Timeout' }
        @{ Why = '条目缺 FullMethod'; Text = "MethodTimeouts:`n  - Timeout: 1s"; Pattern = '缺 FullMethod 或 Timeout' }
        @{ Why = '条目不以 - 开头'; Text = "MethodTimeouts:`n  FullMethod: /a.B/One`n  Timeout: 1s"; Pattern = "必须以 '- ' 开头" }
        @{ Why = '列表项是标量'; Text = "MethodTimeouts:`n  - just-a-string"; Pattern = '看不懂的行' }
        @{ Why = '键只有名字没有值'; Text = "MethodTimeouts:`n  - FullMethod:`n    Timeout: 1s"; Pattern = '看不懂的行' }
        @{ Why = '顶层出现两次'; Text = "MethodTimeouts:`n$one`nName: x`nMethodTimeouts:`n$one"; Pattern = '不止一次' }
        # 以下几种这里都读不出来,必须拒绝而不能当作没有这张表。除「嵌在别的键下面」外,go-zero 都会让它们照常生效
        # (它走完整的 YAML 解析);嵌套的那种在服务端并不生效,但这里不去分辨。
        @{ Why = '列表项顶格写'; Text = "MethodTimeouts:`n- FullMethod: /a.B/One`n  Timeout: 1s`nNext: 1"; Pattern = '列表项要缩进书写' }
        @{ Why = '键带双引号 + 流式写法'; Text = "Name: x`n`"MethodTimeouts`": [{`"FullMethod`": `"/a.B/One`", `"Timeout`": `"1h`"}]"; Pattern = '提到了 MethodTimeouts' }
        @{ Why = '键带单引号 + 块式写法'; Text = "'MethodTimeouts':`n$one"; Pattern = '提到了 MethodTimeouts' }
        @{ Why = '键带引号 + 顶格列表'; Text = "`"MethodTimeouts`":`n- Timeout: 1h`n  FullMethod: /a.B/One"; Pattern = '提到了 MethodTimeouts' }
        @{ Why = '整份文档统一缩进'; Text = "  Name: x`n  MethodTimeouts: [{FullMethod: /a.B/One, Timeout: 1h}]"; Pattern = '提到了 MethodTimeouts' }
        @{ Why = '嵌在别的键下面'; Text = "Outer:`n  MethodTimeouts:`n    - FullMethod: /a.B/One`n      Timeout: 1h"; Pattern = '提到了 MethodTimeouts' }
        @{ Why = '标准写法之后又用带引号的键写了一遍(YAML 里后者覆盖前者)'; Text = "MethodTimeouts:`n$one`n`"MethodTimeouts`": [{FullMethod: /a.B/Two, Timeout: 2h}]"; Pattern = '提到了 MethodTimeouts' }
        @{ Why = '标准写法之后又用大小写不同的带引号键写了一遍(go-zero 的键不分大小写,两条会归并)'; Text = "MethodTimeouts:`n$one`n`"methodTimeouts`": [{FullMethod: /a.B/Two, Timeout: 2h}]"; Pattern = '提到了 MethodTimeouts' }
        @{ Why = '整份文档写成一行流式映射 / JSON(键在行中间)'; Text = '{"Name": "x", "MethodTimeouts": [{"FullMethod": "/a.B/One", "Timeout": "1h"}]}'; Pattern = '提到了 MethodTimeouts' }
        # yaml.v2 把单独的 CR 与 NEL / LS / PS 也当换行;这里只按 LF / CRLF 切行,注释后面跟一个这样的字符,后续内容会被当成注释漏读。
        @{ Why = '行尾是单独的 CR'; Text = "Name: x # c`rMethodTimeouts:`r  - FullMethod: /a.B/One`r    Timeout: 1h`r"; Pattern = '单独的 CR 或 NEL / LS / PS' }
        @{ Why = '注释后面跟 U+2028(LS)'; Text = "Name: x # c$([char]0x2028)MethodTimeouts:`n$one"; Pattern = '单独的 CR 或 NEL / LS / PS' }
        @{ Why = '注释后面跟 U+0085(NEL)'; Text = "Name: x # c$([char]0x85)MethodTimeouts:`n$one"; Pattern = '单独的 CR 或 NEL / LS / PS' }
        @{ Why = '块内条目之间用 U+2029(PS)分隔'; Text = "MethodTimeouts:`n  - FullMethod: /a.B/One`n    Timeout: 1h # c$([char]0x2029)  - FullMethod: /a.B/Two`n    Timeout: 2h"; Pattern = '单独的 CR 或 NEL / LS / PS' }
    )
    foreach ($c in $malformed) {
        $message = ''
        try { Get-ZrpcMethodTimeouts -Text $c.Text | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern $c.Pattern -Because "$($c.Why) 必须 throw 并说清原因,不能读出一份残缺的表"
    }
}

Test-Case "自检:ConvertTo-ZrpcDurationMs 认的是 Go 时长写法的子集,换算不多算,解析不了返回空(交给调用方报违例)" {
    $expected = [ordered]@{
        '300s'      = 300000
        '3600s'     = 3600000
        '14400s'    = 14400000
        '1m30s'     = 90000
        '1h2m3s4ms' = 3723004
        '500ms'     = 500
        '1.5s'      = 1500
        '16.1s'     = 16100
        '2.007s'    = 2007
        '2h'        = 7200000
        ' 8s '      = 8000
        '0.4ms'     = 1
    }
    foreach ($text in $expected.Keys) {
        Assert-Equal -Expected $expected[$text] -Actual (ConvertTo-ZrpcDurationMs -Text $text) -Because "'$text' 的毫秒数(小数不能多算,不足 1ms 向上取整)"
    }
    $fullWidthThree = [string][char]0xFF13
    foreach ($text in @('3000', '8S', '1m30S', '1H30m', '0s', '0ms', '-5s', '+5s', '.5s', '5.s', '1e3s', '1d', '10us', '5 s', 's', '', 'abc', '5s extra', "${fullWidthThree}s", "${fullWidthThree}s5s", "5s${fullWidthThree}ms", '99999999999999999999999999h')) {
        Assert-True -Condition ($null -eq (ConvertTo-ZrpcDurationMs -Text $text)) -Because "'$text' 不是带单位的正时长(单位只认小写的 ms / s / m / h,数字只认 0-9),必须返回空而不是抛异常"
    }
}

# 部署门禁读的全部文件。两条夹具用例都把它们拷到临时目录再改,不碰仓库。
$deadlineBudgetFixtureFiles = @(
    'bin/etc/base_deploy_config.yaml'
    'go/scene_manager/etc/scene_manager_service.yaml'
    'go/data_service/etc/data_service.yaml'
    'go/client_rpc_router/etc/client_rpc_router.yaml'
    'go/match/etc/match_service.yaml'
    'go/login/etc/login.yaml'
    # 第六个目标(玩家收件箱,docs/design/player-inbox-service.md §1.4):门禁同样读它的服务 yaml,缺了副本就是「找不到文件」。
    'go/player_inbox/etc/player_inbox.yaml'
)

Test-Case "负向:DataService deadline 退回修复前的 2500 时部署门禁必须 throw 并只点名该项(夹具是临时目录里的配置副本,不碰仓库)" {
    $tempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('grpc-deadline-budget-' + [guid]::NewGuid().ToString('N'))
    try {
        foreach ($rel in $deadlineBudgetFixtureFiles) {
            $dst = Join-Path $tempRoot $rel
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $dst) | Out-Null
            Copy-Item -LiteralPath (Join-Path (Get-RepoRoot) $rel) -Destination $dst
        }
        $deployCopy = Join-Path $tempRoot 'bin/etc/base_deploy_config.yaml'
        $original = Get-Content -LiteralPath $deployCopy -Raw
        $mutated = [regex]::Replace($original, '(?m)^(\s+DataServiceNodeService:\s*)\d+', '${1}2500')
        Assert-True -Condition ($mutated -ne $original) -Because '夹具必须真的把 DataServiceNodeService 改成 2500,否则下面的负向断言是空转'
        [System.IO.File]::WriteAllText($deployCopy, $mutated, [System.Text.UTF8Encoding]::new($false))

        $RepoRoot = $tempRoot
        $message = ''
        try { Assert-GrpcClientDeadlineBudget | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '拒绝部署' -Because '预算不成立必须拒绝部署,不能只打警告'
        Assert-Match -Text $message -Pattern 'DataServiceNodeService:C\+\+ deadline 2500 < .*go-zero 默认 2000.* = 4000' -Because '错误必须点名目标、实际值与按 go-zero 默认 2000 算出的下限 4000'
        Assert-NotMatch -Text $message -Pattern 'SceneManagerNodeService|ClientRpcRouterNodeService|MatchNodeService|LoginNodeService|PlayerInboxNodeService' -Because '其余五项仍满足,只点名不满足的那一项'
    }
    finally {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case "负向:给 C++ 会调用的 AllocateIdSegment 配一条超预算的 MethodTimeouts,或把这张表写坏,部署门禁都必须 throw(夹具是临时目录里的配置副本,不碰仓库)" {
    $tempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('grpc-deadline-method-timeouts-' + [guid]::NewGuid().ToString('N'))
    try {
        foreach ($rel in $deadlineBudgetFixtureFiles) {
            $dst = Join-Path $tempRoot $rel
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $dst) | Out-Null
            Copy-Item -LiteralPath (Join-Path (Get-RepoRoot) $rel) -Destination $dst
        }
        # 把副本里的 deadline 钉成 4000:本用例验的是判定逻辑,不该随仓库里这个值的调整变红。
        $deployCopy = Join-Path $tempRoot 'bin/etc/base_deploy_config.yaml'
        $deployText = Get-Content -LiteralPath $deployCopy -Raw
        Assert-Match -Text $deployText -Pattern '(?m)^\s+DataServiceNodeService:\s*\d+' -Because '夹具前提:base_deploy_config.yaml 里要有 DataServiceNodeService 的 deadline'
        [System.IO.File]::WriteAllText($deployCopy, [regex]::Replace($deployText, '(?m)^(\s+DataServiceNodeService:\s*)\d+', '${1}4000'), [System.Text.UTF8Encoding]::new($false))

        $serviceCopy = Join-Path $tempRoot 'go/data_service/etc/data_service.yaml'
        $original = Get-Content -LiteralPath $serviceCopy -Raw
        # 与解析器同样宽:键名不分大小写,键后可以带行尾注释。否则有人只给这一行加条注释,门禁照常放行、本用例却假红。
        $keyLine = '(?mi)^MethodTimeouts[ \t]*:[ \t]*(#[^\r\n]*)?\r?$'
        Assert-Match -Text $original -Pattern $keyLine -Because '夹具前提:data_service.yaml 里要有块式的 MethodTimeouts(回档三条),下面两处改动都插在它的第一行之后'
        $RepoRoot = $tempRoot

        # 前提:副本不改就该放行。否则下面几条负向断言分不清是谁造成的失败。
        Assert-GrpcClientDeadlineBudget | Out-Null

        # 通过行只列真正按登记表放行的条目:预算内的普通条目、以及与登记项只差大小写的方法(判定函数不豁免它,
        # 只是 1s + 2000 ≤ 4000 所以通过)都不能被写成 exempt,否则部署的人看到的豁免清单是错的。
        $withInBudget = [regex]::Replace($original, $keyLine, "MethodTimeouts:`n  - FullMethod: /data_service.DataService/AllocateIdSegment`n    Timeout: 1s`n  - FullMethod: /data_service.DataService/rollbackplayer`n    Timeout: 1s")
        [System.IO.File]::WriteAllText($serviceCopy, $withInBudget, [System.Text.UTF8Encoding]::new($false))
        $passLine = [string](Assert-GrpcClientDeadlineBudget 6>&1 | Out-String)
        Assert-Match -Text $passLine -Pattern 'GrpcClient deadline budget OK' -Because '两条预算内的条目不该让门禁拒绝(夹具前提)'
        Assert-Match -Text $passLine -Pattern 'RollbackPlayer=' -Because '登记过的回档条目仍要出现在豁免清单里'
        Assert-NotMatch -Text $passLine -Pattern 'AllocateIdSegment=' -Because '没登记的条目是经过 deadline 比较才通过的,不能出现在豁免清单里'
        Assert-True -Condition (-not ($passLine -cmatch 'rollbackplayer=')) -Because '与登记项只差大小写的方法没有被豁免,不能出现在豁免清单里(登记表逐字匹配)'

        $withAllocate = [regex]::Replace($original, $keyLine, "MethodTimeouts:`n  - FullMethod: /data_service.DataService/AllocateIdSegment`n    Timeout: 8s")
        [System.IO.File]::WriteAllText($serviceCopy, $withAllocate, [System.Text.UTF8Encoding]::new($false))
        $message = ''
        try { Assert-GrpcClientDeadlineBudget | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '拒绝部署' -Because 'C++ 会调用的方法被放宽到 deadline 之上必须拒绝部署'
        Assert-Match -Text $message -Pattern 'DataServiceNodeService:C\+\+ deadline 4000 < .*AllocateIdSegment 8s.*= 10000' -Because '错误必须点名目标、方法与按 8s + 2000 算出的下限'
        Assert-NotMatch -Text $message -Pattern 'Rollback(Player|Zone|All)' -Because '回档三条已登记为 C++ 不调用,不应被点名'
        Assert-NotMatch -Text $message -Pattern 'SceneManagerNodeService|ClientRpcRouterNodeService|MatchNodeService|LoginNodeService|PlayerInboxNodeService' -Because '其余五项仍满足,只点名不满足的那一项'

        $withUnknownKey = [regex]::Replace($original, $keyLine, "MethodTimeouts:`n  - FullMethod: /data_service.DataService/RollbackPlayer`n    Timeout: 300s`n    Retries: 3")
        [System.IO.File]::WriteAllText($serviceCopy, $withUnknownKey, [System.Text.UTF8Encoding]::new($false))
        $message = ''
        try { Assert-GrpcClientDeadlineBudget | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern 'go/data_service/etc/data_service\.yaml 的 MethodTimeouts 解析不了.*未知键 Retries' -Because '读不懂的 MethodTimeouts 必须拒绝部署并点名文件与原因,不能当作没有这张表'

        # 评审时抓到的误放:在文件末尾用带引号的键、流式写法再写一遍,YAML 里后者覆盖前者,go-zero 照常生效。
        $withQuotedFlow = $original.TrimEnd() + "`n`"MethodTimeouts`": [{FullMethod: /data_service.DataService/AllocateIdSegment, Timeout: 1h}]`n"
        [System.IO.File]::WriteAllText($serviceCopy, $withQuotedFlow, [System.Text.UTF8Encoding]::new($false))
        $message = ''
        try { Assert-GrpcClientDeadlineBudget | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern 'go/data_service/etc/data_service\.yaml 的 MethodTimeouts 解析不了.*提到了 MethodTimeouts' -Because '带引号的键 + 流式写法必须拒绝部署,不能因为两套解析都读不出条目就放行'
    }
    finally {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case "通过时回显按「C++ 不调用」放行的 MethodTimeouts 条目(部署的人看得到这次豁免了什么)" {
    $okLine = [string]($devOut -split "`r?`n" | Where-Object { $_ -match 'GrpcClient deadline budget OK' } | Select-Object -First 1)
    Assert-Match -Text $okLine -Pattern 'MethodTimeouts exempt \(never called from C\+\+\): ' -Because 'zone-up 的输出里,门禁通过的那一行必须列出被豁免的条目'
    $table = Get-GrpcClientMethodsNotCalledFromCpp
    foreach ($target in $table.Keys) {
        foreach ($fullMethod in @($table[$target])) {
            Assert-Match -Text $okLine -Pattern ([regex]::Escape("$target $fullMethod=")) -Because "$fullMethod 在服务 yaml 里配了 MethodTimeouts 且已登记,通过时必须点名它"
        }
    }
}

Test-Case "「C++ 不调用」表名副其实:表里的方法 C++ 既不直接调用、gate 也不会按消息号转发,且每一项都对应服务 yaml 里的一条 MethodTimeouts(没有过期项)" {
    # 表里的目标 → 它的服务 yaml。新增目标时在这里补一行(与 Assert-GrpcClientDeadlineBudget 的 $targetSources 同值)。
    $serviceYamlOf = @{ DataServiceNodeService = 'go/data_service/etc/data_service.yaml' }

    # 手写 C++ = cpp/ 下除生成物(cpp/generated,生成客户端自身的定义就在那里)与内嵌 muduo 之外的全部源码。
    $handWritten = @(Get-ChildItem -LiteralPath (Join-Path (Get-RepoRoot) 'cpp') -Recurse -File | Where-Object {
        @('.cpp', '.cc', '.h', '.hpp') -contains $_.Extension -and (($_.FullName -replace '\\', '/') -notmatch '/cpp/generated/|/muduo_windows/')
    })
    Assert-True -Condition ($handWritten.Count -gt 100) -Because "手写 C++ 源文件应当有几百个,实际只找到 $($handWritten.Count) 个(搜索范围写错了?)"

    # gate 直连模式不写任何符号:它拿客户端消息号查 gRpcMethodRegistry 再调 .sender 转发
    # (cpp/nodes/gate/handler/rpc/client_session_handler.cpp)。哪些消息号算客户端消息,由生成的 IsClientMessageId 决定。
    $registryText = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'cpp/generated/rpc/service_metadata/rpc_event_registry.cpp') -Raw
    $clientIdsFunction = [regex]::Match($registryText, '(?s)bool\s+IsClientMessageId\s*\([^)]*\)\s*\{(?<body>.*?)\r?\n\}')
    Assert-True -Condition $clientIdsFunction.Success -Because 'rpc_event_registry.cpp 里应当有 IsClientMessageId(生成器改了形状的话,本用例的「gate 会转发」判据要跟着改)'
    $clientMessageIds = @([regex]::Matches($clientIdsFunction.Groups['body'].Value, '\bcase\s+(?<id>[A-Za-z_][A-Za-z0-9_]*MessageId)\s*:') | ForEach-Object { $_.Groups['id'].Value })
    Assert-True -Condition ($clientMessageIds.Count -gt 20) -Because "IsClientMessageId 里应当列着上百个客户端消息号,实际只读到 $($clientMessageIds.Count) 个(解析写错了?)"

    $namesOf = {
        param([string]$FullMethod)
        $parts = [regex]::Match($FullMethod, '^/(?:[A-Za-z_][A-Za-z0-9_]*\.)*(?<service>[A-Za-z_][A-Za-z0-9_]*)/(?<method>[A-Za-z_][A-Za-z0-9_]*)$')
        if (-not $parts.Success) { throw "全方法名不是 /包.服务/方法 的形状: $FullMethod" }
        return $parts.Groups['service'].Value + $parts.Groups['method'].Value
    }
    # 直接调用:生成客户端的符号 Send<服务><方法> / Send<服务><方法>Erased,或它的消息号常量。
    $findDirectUses = {
        param([string]$FullMethod)
        $name = & $namesOf $FullMethod
        $symbol = '\b(?:Send' + $name + '(?:Erased)?|' + $name + 'MessageId)\b'
        return @($handWritten | Select-String -Pattern $symbol -CaseSensitive | ForEach-Object { "{0}:{1}" -f ($_.Path -replace '\\', '/'), $_.LineNumber })
    }
    $isForwardedByGate = { param([string]$FullMethod) return ($clientMessageIds -ccontains ((& $namesOf $FullMethod) + 'MessageId')) }

    # 正向对照:两种调用形态各拿一个已知会被调用的方法验一遍。搜不到它们,说明搜索本身坏了,下面的「搜不到」不能算证据。
    Assert-True -Condition (@(& $findDirectUses '/data_service.DataService/AllocateIdSegment').Count -gt 0) -Because '搜索必须能找到已知的直接调用 SendDataServiceAllocateIdSegment(scene 的号段领取),否则本用例的结论无效'
    Assert-True -Condition (& $isForwardedByGate '/loginpb.ClientPlayerLogin/Login') -Because 'Login 是 gate 直连模式按消息号转发的客户端消息,必须被判成「C++ 会调用」,否则本用例对这种调用形态是瞎的'
    Assert-True -Condition (-not (& $isForwardedByGate '/data_service.DataService/AllocateIdSegment')) -Because 'AllocateIdSegment 不是客户端消息,gate 不转发它(反向对照:判据不能把所有方法都判成会转发)'

    $table = Get-GrpcClientMethodsNotCalledFromCpp
    Assert-True -Condition ($table.Count -gt 0) -Because '表不应为空(回档三个 RPC 登记在这里;若它们的 MethodTimeouts 已删,连同本表一起清掉并改写本断言)'
    foreach ($target in $table.Keys) {
        Assert-True -Condition $serviceYamlOf.ContainsKey($target) -Because "本用例不认识目标 ${target}:在 `$serviceYamlOf 里补上它的服务 yaml"
        $configured = @(Get-ZrpcMethodTimeouts -Text (Get-Content -LiteralPath (Join-Path (Get-RepoRoot) $serviceYamlOf[$target]) -Raw) | ForEach-Object { [string]$_.FullMethod })
        foreach ($fullMethod in @($table[$target])) {
            $uses = @(& $findDirectUses $fullMethod)
            Assert-True -Condition ($uses.Count -eq 0) -Because ("{0} 登记为 C++ 不调用,但手写 C++ 里出现了它的生成客户端符号或消息号:{1}。C++ 一旦调用它,就必须满足 deadline ≥ 该方法超时 + 2000,不能再豁免" -f $fullMethod, ($uses -join ', '))
            Assert-True -Condition (-not (& $isForwardedByGate $fullMethod)) -Because "$fullMethod 登记为 C++ 不调用,但它的消息号列在 IsClientMessageId 里:gate 直连模式会按消息号把它转发给目标服务,不能豁免"
            Assert-True -Condition ($configured -ccontains $fullMethod) -Because "$fullMethod 登记在表里,但 $($serviceYamlOf[$target]) 的 MethodTimeouts 里没有这一条:过期的豁免要删掉,免得将来给它配长超时时无人复核"
        }
    }
}

Test-Case "node ConfigMap 必须带 GateTokenSecret(否则 gate 在 prod 运行模式下拒绝启动)" {
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "node-config")
    $v = Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.GateTokenSecret'
    Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($v)) -Because "ValidateGateTokenSecretOrDie 在空密钥 + prod 下 LOG_FATAL,而部署链从不设 GATE_RUN_MODE(默认即 prod)"
}

Test-Case "node ConfigMap 的 NodeTTLSeconds 必须与 bin/etc 权威值一致" {
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "node-config")
    $v = Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.Etcd.NodeTTLSeconds'
    $authoritative = Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Etcd.NodeTTLSeconds'
    Assert-Equal -Expected $authoritative -Actual $v -Because "生成器写死 60 会退回 2026-05-24 压测前的值,45k 浪涌下 keepalive 抖一帧就误判租约过期 FATAL"
}

Test-Case "node ConfigMap 必须带 GateMaxConnections(缺了等于 gate 无连接数上限)" {
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "node-config")
    $v = Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.GateMaxConnections'
    $authoritative = Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'GateMaxConnections'
    Assert-Equal -Expected $authoritative -Actual $v -Because "0/缺失 = 不限,而 gate 是唯一对公网开放的端口"
    Assert-True -Condition ([uint64]$v -gt 0) -Because "仓库默认/K8s 部署必须启用连接上限;0 只允许显式 dev/test 本地运行"
    Assert-True -Condition ([uint64]$v -le 131071) -Because "session id 低 17 位回绕;超过安全容量会让碰撞检查永久自旋"
}

Test-Case "node ConfigMap 的 AuditTopicGeneration 必须与 data-service 的 Kafka.TopicGeneration 一致" {
    # 审计 topic 的有效名 = `<基名>_g<世代号>`。C++ scene 是 transaction_log / player_snapshot
    # 唯一的生产者,data-service 是唯一的消费者,topic 名是两边唯一的会合点 —— 两边世代号分家
    # 不报任何错:生产者往一个没人消费的 topic 写,30 天保留期一到就静默没了。
    # 2026-09-09 前 C++ 侧后缀是编译期常量、这份 ConfigMap 刻意不写这一键;现在
    # config.cpp::readBaseDeployConfig 真读 AuditTopicGeneration,所以必须写,且必须等于消费者那份。
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "node-config")
    $v = Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.AuditTopicGeneration'
    $authoritative = Get-EtcValue -RelativePath 'go/data_service/etc/data_service.yaml' -KeyPath 'Kafka.TopicGeneration'
    Assert-Equal -Expected $authoritative -Actual $v -Because "生产者(C++)与消费者(data-service)的世代号不等 = 流水/快照写进没人消费的 topic,无任何报错"
}

Test-Case "node ConfigMap 的 DbTaskTopicGeneration 必须与 go/db、go/login 的 Kafka.TopicGeneration 三方一致" {
    # 玩家存盘 DBTask topic:C++ scene 按 home_zone 往 db_task_zone_{zone}[_g<N>] 写,go/db 是唯一的消费者,
    # login 也读写同一组 topic(player-storage-placement.md §7 / §12 A14)。三方世代号分家不报任何错 ——
    # 换代后 scene 仍写旧代 topic,存盘静默积压在已排空、没人消费的旧 topic 里。
    # config.cpp::readBaseDeployConfig 真读 DbTaskTopicGeneration,且 node ConfigMap 整目录遮蔽镜像里的
    # bin/etc,所以必须写,且必须等于消费者那份;同时钉住生成出来的 go-svc-db ConfigMap 那一份。
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "node-config")
    $v = Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.DbTaskTopicGeneration'
    $dbAuthoritative = Get-EtcValue -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.TopicGeneration'
    $loginAuthoritative = Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Kafka.TopicGeneration'
    Assert-Equal -Expected $dbAuthoritative -Actual $v -Because "生产者(C++ scene)与消费者(go/db)的 db_task 世代号不等 = 存盘写进没人消费的 topic,无任何报错"
    Assert-Equal -Expected $dbAuthoritative -Actual $loginAuthoritative -Because "login 与 db 读写同一组 db_task topic,换代必须同一次改"
    $dbFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-db-config")
    Assert-Equal -Expected $v -Actual (Get-FlatValue -Flat $dbFlat -KeyPath 'data.db.yaml.ServerConfig.Kafka.TopicGeneration') -Because "node 与 go-svc-db 两份 ConfigMap 必须从同一个键派生"
}

Test-Case "node ConfigMap 的 PersistSchemaRegressionMode == bin/etc/base_deploy_config.yaml,且只能是 warn / enforce" {
    # 混版本持久字段保护:scene 严格提交屏障 C3 的模式(docs/design/mixed-version-persist-field-protection.md §5.2)。
    # node ConfigMap 整目录遮蔽镜像里的 bin/etc:不写这一键 C++ 按 warn,仓库切到 enforce 时集群里悄悄不生效;
    # 写成两个字面量以外的值,config.cpp::readBaseDeployConfig 会拒绝启动(那比悄悄不生效更好,但应在生成期就拦下)。
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "node-config")
    $v = Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.PersistSchemaRegressionMode'
    $authoritative = Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'PersistSchemaRegressionMode'
    Assert-Equal -Expected $authoritative -Actual $v -Because "C3 模式必须从 bin/etc/base_deploy_config.yaml 镜像,不能留可漂移的副本"
    Assert-True -Condition ($v -ceq 'warn' -or $v -ceq 'enforce') -Because "C++ 只认 warn / enforce(当前 '$v')"
}

Test-Case "data-service 的 MappingRedis 不写 DB 键;go/db 读落点记录的 Redis 必须与它同址且为 DB 0" {
    # go-zero 的 RedisConf 没有 DB 字段:ConfigMap 里写 `DB: 15` 会被静默忽略,映射实际在 DB 0,
    # 留着只会让运维照它给 merge_zone 填 -mapping-redis-db 15(gap-fixes B6 / player-storage-placement.md §12 A12)。
    # go/db 按落点选库时 MGET player:placement / player:zone(§6.2):Placement.Redis 不写就复用 ServerConfig.RedisClient。
    # 两边不同址或不是 DB 0 = 读不到任何记录与 home_zone,全员按本 zone 选库,被钉到别处的玩家静默写错库。
    $dsFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-data-service-config")
    $mappingHost = Get-FlatValue -Flat $dsFlat -KeyPath 'data.data_service.yaml.MappingRedis.Host'
    Assert-True -Condition (-not $dsFlat.Scalars.Contains('data.data_service.yaml.MappingRedis.DB')) -Because "MappingRedis 的 DB 键对 go-zero 无效,写出来只会误导合服参数"

    $dbFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-db-config")
    # 显式写了 Placement.Redis 就以它为准(DB 缺省 0),否则取 RedisClient —— 与 go/db 的取值规则一致。
    if ($dbFlat.Scalars.Contains('data.db.yaml.Placement.Redis.Hosts')) {
        $placementHost = Get-FlatValue -Flat $dbFlat -KeyPath 'data.db.yaml.Placement.Redis.Hosts'
        $placementDb = if ($dbFlat.Scalars.Contains('data.db.yaml.Placement.Redis.DB')) { Get-FlatValue -Flat $dbFlat -KeyPath 'data.db.yaml.Placement.Redis.DB' } else { '0' }
    }
    else {
        $placementHost = Get-FlatValue -Flat $dbFlat -KeyPath 'data.db.yaml.ServerConfig.RedisClient.Hosts'
        $placementDb = Get-FlatValue -Flat $dbFlat -KeyPath 'data.db.yaml.ServerConfig.RedisClient.DB'
    }
    Assert-Equal -Expected $mappingHost -Actual $placementHost -Because "go/db 读落点记录的 Redis 必须是 data-service 的 MappingRedis 实例"
    Assert-Equal -Expected '0' -Actual $placementDb -Because "落点记录与 player:zone 恒在 DB 0"
}

Test-Case "data-service 的 DevRedis 必须与 C++ zoneredis、go/db RedisClient 同址且为 DB 0(账本读的就是写方那台)" {
    # 配了 DevRedis 时 Router.ClientForPlayer 不查映射,所有玩家一律返回这一个客户端(go/data_service/internal/routing/router.go 的 devClient 短路)。
    # data_service 在它上面读的账本都不是自己写的:PlayerAllData:<id> 由 C++ scene 经 zoneredis 存盘写入,
    # player:{id}:asset_sql_commit 由 go/db 经 RedisClient 在 SQL 提交后发布(读侧见 persisted_player_database.go / asset_sql_commit.go)。
    # 读写同库只靠这三段配置对齐,代码里没有交叉校验,DevRedis.DB 可配且不校验(internal/config/config.go DevRedisConfig)。
    # 改址或改库不报任何错:GET / HGET 全部 Nil → found=false,guild / trade / mail 把资产待办记 absent 一直退避,
    # player_inbox 对账一直 unknown 且对 absent 不告警 —— fail-closed,只丢活性,很难发现。
    # 删掉 DevRedis 段(改走 Regions 按 home_zone 路由)时这里先变红:那是另一条路由,要另写它的同址契约,不能静默放过。
    $dsFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-data-service-config")
    $devRedisHost = Get-FlatValue -Flat $dsFlat -KeyPath 'data.data_service.yaml.DevRedis.Host'
    # DevRedisConfig.DB 是 `json:",default=0"`:不写即 DB 0 —— 与 data_service 的取值规则一致。
    $devRedisDb = if ($dsFlat.Scalars.Contains('data.data_service.yaml.DevRedis.DB')) { Get-FlatValue -Flat $dsFlat -KeyPath 'data.data_service.yaml.DevRedis.DB' } else { '0' }

    $nodeFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "node-config")
    $zoneRedisAddr = '{0}:{1}' -f (Get-FlatValue -Flat $nodeFlat -KeyPath 'data.game_config.yaml.zoneredis.host'), (Get-FlatValue -Flat $nodeFlat -KeyPath 'data.game_config.yaml.zoneredis.port')
    $zoneRedisDb = Get-FlatValue -Flat $nodeFlat -KeyPath 'data.game_config.yaml.zoneredis.db'

    $dbFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-db-config")
    $dbRedisHost = Get-FlatValue -Flat $dbFlat -KeyPath 'data.db.yaml.ServerConfig.RedisClient.Hosts'
    $dbRedisDb = Get-FlatValue -Flat $dbFlat -KeyPath 'data.db.yaml.ServerConfig.RedisClient.DB'

    Assert-Equal -Expected $zoneRedisAddr -Actual $devRedisHost -Because "data_service 读 PlayerAllData 父键的 DevRedis 必须是 C++ scene 存盘写入的 zoneredis 实例(host:port)"
    Assert-Equal -Expected $dbRedisHost -Actual $devRedisHost -Because "data_service 读 asset_sql_commit 的 DevRedis 必须是 go/db 发布它的 RedisClient 实例"
    Assert-Equal -Expected '0' -Actual $zoneRedisDb -Because "C++ 不发 SELECT,PlayerAllData 恒写在 DB 0"
    Assert-Equal -Expected '0' -Actual $dbRedisDb -Because "go/db 在 RedisClient 的 DB 0 发布 asset_sql_commit"
    Assert-Equal -Expected '0' -Actual $devRedisDb -Because "DevRedis.DB 必须与两个写方同为 DB 0;换成别的库号读到的全是 Nil,账本读静默变成 absent"
}

Test-Case "login ConfigMap 必须带 Secrets.InternalAuth(否则生产 login 拒绝启动)" {
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-login-config")
    $v = Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Secrets.InternalAuth.Value'
    Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($v)) -Because "生产模式 EnforceInternalAuth 恒 true 且关不掉,未配置时 ResolveSecrets 直接返回 error"
}

Test-Case "db ConfigMap 必须带非空的库名白名单(否则 db 在 strict 档下拒启)" {
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-db-config")
    $v = Get-FlatValue -Flat $flat -KeyPath 'data.db.yaml.ServerConfig.Database.AllowedDatabases[0]'
    Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($v)) -Because "三个来源全空时 AllowlistEnforcement 默认 strict = 拒启"
}

# ─────────────────────────────────────────────────────────────────
# 2. 镜像不可变版本戳
# ─────────────────────────────────────────────────────────────────

Test-Case "生成的所有 image: 引用都不得是 latest 等可变 tag" {
    $imageLines = @([regex]::Matches($devOut, '(?m)^\s*image:\s*(?<ref>\S+)\s*$') | ForEach-Object { $_.Groups['ref'].Value })
    Assert-True -Condition ($imageLines.Count -gt 0) -Because "DryRun 输出里应当有 image: 行"
    foreach ($ref in $imageLines) {
        $tag = Get-ImageTagFromRef -ImageRef $ref
        $chk = Test-ImmutableImageTag -Tag $tag
        Assert-True -Condition $chk.Ok -Because "镜像引用 '$ref' 用了可变 tag,rollout undo 会退到同一个 digest($($chk.Reason))"
    }
}

Test-Case "显式传可变 tag 时 imagePullPolicy 必须是 Always" {
    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @("-NodeImage", "ghcr.io/luyuancpp/mmorpg-node:latest"))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "dev 档位允许可变 tag,只是要改 pullPolicy"
    Assert-Match -Text $run.Output -Pattern 'imagePullPolicy:\s*Always' -Because "可变 tag 配 IfNotPresent = 节点上有旧层就永远不拉新的"
}

Test-Case "不可变 tag 时 imagePullPolicy 是 IfNotPresent" {
    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @("-NodeImage", "ghcr.io/luyuancpp/mmorpg-node:0123456789ab"))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "基线 DryRun 应当成功"
    Assert-Match -Text $run.Output -Pattern 'imagePullPolicy:\s*IfNotPresent' -Because "内容不会变的 tag 没必要每次重拉"
}

# ─────────────────────────────────────────────────────────────────
# 3. prod 档位的密钥注入
# ─────────────────────────────────────────────────────────────────

$ProdEnv = @{
    MMORPG_GATE_TOKEN_SECRET  = 'contract-test-secret-0123456789abcdef0123'
    MMORPG_MYSQL_USER         = 'mmorpg_app'
    MMORPG_MYSQL_PASSWORD     = 'contract-test-mysql-pw-01'
    MMORPG_REDIS_PASSWORD     = 'contract-test-redis-pw-01'
    MMORPG_GATEWAY_DB_USER    = 'gateway_app'
    MMORPG_GATEWAY_DB_PASSWORD= 'contract-test-gwdb-pw-01'
    # login 的 Secrets.InternalAuth:生产模式恒强制验签且关不掉,缺了 login 拒启。
    # 刻意与 GATE_TOKEN_SECRET 不同 —— secrets.go 会拒绝跨用途复用主密钥。
    MMORPG_INTERNAL_AUTH_SECRET = 'contract-test-internal-auth-0123456789abcdef'
    # db 库名白名单:三个来源全空时 db 在默认 strict 档下拒启。
    MMORPG_DB_ALLOWED_DATABASES = 'zone_1_db,zone_2_db'
    # gateway 管理面口令(k8s-client-entry D91 纵深防御):非 dev 档缺失即拒绝部署;
    # 取 ≥32 位非占位串,且与上面各密钥都不同。
    MMORPG_GATEWAY_ADMIN_API_KEY = 'contract-test-gw-admin-key-0123456789abcdef'
    # login 登录排队令牌密钥(Secrets.QueueToken,G4b):staging / prod 缺省开排队,本次部署 login 时必填。
    # ≥32 位;与上面各值都不同(login 拒绝跨用途复用主密钥,部署侧对 GATE / INTERNAL_AUTH 两把同样拒绝);
    # 也不以 go/login/internal/config/secrets.go placeholderPrefixes 里的前缀开头(Go 侧按前缀判占位,比部署侧严)。
    MMORPG_LOGIN_QUEUE_TOKEN_SECRET = 'contract-test-queue-token-0123456789abcdef'
    # 登录排队合入门槛的测试接缝(2026-10-15,k8s_deploy.ps1 Resolve-LoginQueueSwitch):门槛未清($script:LoginQueueMergeGateCleared
    # 为 $false)时 staging / prod 的 auto 解析为关。设为 1 只在 -DryRun 下视为门槛已清,让下面依赖「排队开」的 DryRun
    # ($prodRun、$peakStagingRun、$peakAllUpRun,以及用例 10 / 11 由本表拷贝出的 $env2)照旧拿到排队开的产物;
    # 不带 -DryRun 时 k8s_deploy.ps1 见到它就拒绝,所以它放不开任何真实部署。本文件所有部署子进程都带 -DryRun。
    # 断「门槛未清」的用例 19 / 20 与不依赖排队的 $peakProdOffRun 在各自的拷贝里显式置空。
    MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED = '1'
}
$ProdArgs = $BaseArgs + @(
    "-ReleaseProfile", "prod",
    "-SkipPreflight",
    "-NodeImage", "ghcr.io/luyuancpp/mmorpg-node:0123456789ab"
)

$prodRun = Invoke-DeployDryRun -Arguments $ProdArgs -Env $ProdEnv

Test-Case "prod 档位 + 注入环境变量时生成器可以正常出产物" {
    Assert-Equal -Expected 0 -Actual $prodRun.ExitCode -Because "注入齐全时不该失败。输出: $($prodRun.Output)"
}

Test-Case 'data-service默认强SQL:dev/prod均db_first，Arm/read/db就绪门禁同开且服务源一致' {
    foreach ($case in @(@{ Label = 'dev'; Output = $devOut }, @{ Label = 'prod'; Output = $prodRun.Output })) {
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $case.Output -Name 'go-svc-data-service-config')
        Assert-Equal -Expected 'db_first' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.RouteStore.Mode') -Because "$($case.Label):全球SQL为唯一权威默认"
        foreach ($key in @('ReadRequiresReady', 'Reconcile.ArmReadyMarker')) {
            Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $flat -KeyPath "data.data_service.yaml.RouteStore.$key") -Because '默认即强制SQL就绪，不能静默回Redis'
        }
    }
    Assert-Equal -Expected 'db_first' -Actual (Get-EtcValue -RelativePath 'go/data_service/etc/data_service.yaml' -KeyPath 'RouteStore.Mode') -Because '本地与部署同为全球SQL'
}

Test-Case "prod 档位下 db 段不得出现 root/root 或空密码" {
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $prodRun.Output -Name "go-svc-db-config")
    $user = Get-FlatValue -Flat $flat -KeyPath 'data.db.yaml.ServerConfig.Database.User'
    $passwd = Get-FlatValue -Flat $flat -KeyPath 'data.db.yaml.ServerConfig.Database.Passwd'

    Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($passwd)) -Because "空密码 = 库随便连"
    Assert-True -Condition (-not (Test-PlaceholderSecret -Value $passwd)) -Because "prod 的 MySQL 密码不得是 'root' / '123456' 这类占位串(实际 '$passwd')"
    Assert-True -Condition (-not ($user -eq 'root' -and $passwd -eq 'root')) -Because "root/root 是生成器以前写死的值,在真集群上连都连不上"
    Assert-Equal -Expected $ProdEnv.MMORPG_MYSQL_PASSWORD -Actual $passwd -Because "密码必须来自环境变量注入"
}

Test-Case "prod 档位下所有 ConfigMap 都不得残留占位密钥串" {
    Assert-NotMatch -Text $prodRun.Output -Pattern 'change-me' -Because "生成器自己把占位常量写进生产 ConfigMap 是本轮要根除的问题"
    Assert-NotMatch -Text $prodRun.Output -Pattern 'password:\s*"?123456"?' -Because "Java Gateway 数据源密码以前写死 123456"
}

Test-Case "prod 档位下 login / scene-manager / gateway 的 GateTokenSecret 都来自同一个注入值" {
    foreach ($case in @(
        @{ Cm = 'go-svc-login-config';         Key = 'data.login.yaml.GateTokenSecret' }
        @{ Cm = 'go-svc-scene-manager-config'; Key = 'data.scene_manager_service.yaml.GateTokenSecret' }
        @{ Cm = 'java-svc-gateway-config';     Key = 'data.application.yaml.gate.token-secret' }
    )) {
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $prodRun.Output -Name $case.Cm)
        $v = Get-FlatValue -Flat $flat -KeyPath $case.Key
        Assert-Equal -Expected $ProdEnv.MMORPG_GATE_TOKEN_SECRET -Actual $v -Because "$($case.Cm) 的 gate 密钥必须来自 MMORPG_GATE_TOKEN_SECRET"
    }
}

Test-Case "login 的 go-zero Mode:dev 档必须写且 == go/login/etc/login.yaml,prod 档必须不写(保持 pro 门禁)" {
    # 背景(2026-09-03 kind B 档实跑):dev 档密钥是占位回落,login 的 secrets 门禁只在
    # Mode=dev/test 才降级为 WARN;ConfigMap 不写 Mode 就是默认 pro → login 起来即 panic
    # "HMAC 密钥配置不合格,拒绝启动"。修法是 dev 档写 Mode(值与 etc 权威文件一致),
    # 但 prod 档绝不能跟着写 —— 写了等于把生产的 fail-closed 门禁一起关掉。
    $devFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name "go-svc-login-config")
    $expected = Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Mode'
    $actual = Get-FlatValue -Flat $devFlat -KeyPath 'data.login.yaml.Mode'
    Assert-Equal -Expected $expected -Actual $actual -Because "dev 档 login ConfigMap 的 Mode 必须与 go/login/etc/login.yaml 一致(否则 login 在 K8s 上拒启)"

    $prodFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $prodRun.Output -Name "go-svc-login-config")
    Assert-True -Condition (-not $prodFlat.Scalars.Contains('data.login.yaml.Mode')) -Because "prod 档 login ConfigMap 不得写 Mode(默认 pro 才会对占位密钥 fail-closed)"
}

# ─────────────────────────────────────────────────────────────────
# 3a. C++ 节点的 zone Redis 口令(P0-A 第二批,方案 A)
# ─────────────────────────────────────────────────────────────────
# 设计:docs/handoff/global-singletons-p0-findings-20261014.md「P0-A」修复设计第 6、7 条与 8(c)。口令明文进 node-config /
# battle-node-config 的 game_config.yaml(zoneredis.password);挂 /app/bin/etc 的 C++ 工作负载 Pod 模板带只哈希口令的指纹注解
# mmorpg.io/zone-redis-auth-hash(C++ 只在启动时读配置,口令一变要靠它滚动)。dev 不注入口令时两者都与改动前逐字相同。
# 复用 $prodRun / $devOut / $agonesOut,另起 4 个 DryRun 子进程:换口令、external gate + Agones scene(dev 档注入口令)与两个负向;
# battle 的两个在下文 battle 段($battleInfraRun 之后)。生成期保护与指纹函数按 AST 抽出来直接调用(同上文 deadline 守卫)。
# 断言一律不回显口令值(比较用 -ceq / Contains,不用会把文本打进失败信息的 Assert-Equal / Assert-Match)。
$zoneRedisFunctionNames = @('Assert-RedisPasswordYamlSafe', 'Get-CppZoneRedisPassword', 'Get-CppZoneRedisAuthAnnotations', 'Get-InjectedSecretFingerprint')
foreach ($zoneRedisFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($zoneRedisFunctionNames -contains $zoneRedisFnAst.Name) {
        Set-Item -Path "Function:script:$($zoneRedisFnAst.Name)" -Value $zoneRedisFnAst.Body.GetScriptBlock()
    }
}
foreach ($zoneRedisFnName in $zoneRedisFunctionNames) {
    if (-not (Test-Path "Function:$zoneRedisFnName")) { throw "k8s_deploy.ps1 缺少函数 $zoneRedisFnName(zone Redis 口令的生成期保护 / 指纹被删了?后续断言无意义)" }
}
$ZoneRedisAuthHashKey = 'mmorpg.io/zone-redis-auth-hash'

# 期望的指纹:只用口令算(调抽出来的 Get-CppZoneRedisAuthAnnotations)。DryRun 里各工作负载的指纹等于它,就说明指纹输入只有口令,
# 与 node-config 的其余内容无关 —— 哈希整份 node-config 会让 P0-B 这类无关改动也滚动 gate(踢人)与 battle(作废在打的局)。
# 两个脚本级变量是被抽函数的输入,用完即删;$ReleaseProfile 按动态作用域给。
function Get-ExpectedZoneRedisAuthHash {
    param([Parameter(Mandatory = $true)][string]$Password)
    try {
        $script:RedisPassword = $Password
        $script:ZoneRedisAuthHashAnnotation = $ZoneRedisAuthHashKey
        $ReleaseProfile = 'prod'
        $annotations = Get-CppZoneRedisAuthAnnotations
        if ($annotations.Count -ne 1) { throw "非空口令应当恰好得到 1 条指纹注解,实际 $($annotations.Count) 条" }
        return [string]$annotations[$ZoneRedisAuthHashKey]
    }
    finally {
        Remove-Variable -Name RedisPassword, ZoneRedisAuthHashAnnotation -Scope Script -ErrorAction SilentlyContinue
    }
}

# 按 kind + metadata.name 取唯一一份 manifest(external 下 gate 的 StatefulSet 与 PDB 同名,Select-ManifestByName 只按名字取第一份)。
# 找不到、多于一份、或不挂 /app/bin/etc(不是 C++ 节点工作负载)一律 throw。
function Select-CppWorkloadBlock {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output,
        [Parameter(Mandatory = $true)][string]$Kind,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Label
    )
    $found = @(Get-ManifestBlocks -Output $Output | Where-Object {
        $_ -match ('(?m)^kind:[ ]*' + [regex]::Escape($Kind) + '[ ]*\r?$') -and $_ -match ('(?m)^  name:[ ]*' + [regex]::Escape($Name) + '[ ]*\r?$')
    })
    if ($found.Count -ne 1) { throw "${Label}:DryRun 输出里 kind=$Kind name=$Name 的 manifest 应恰好 1 份,实际 $($found.Count) 份" }
    if ($found[0] -notmatch 'mountPath: /app/bin/etc') { throw "${Label}:$Kind/$Name 不挂 /app/bin/etc,不是 C++ 节点工作负载(夹具前提)" }
    return $found[0]
}

# Pod 模板上的 zone Redis 口令指纹。没有该注解返回 $null;有则必须恰好 1 处、在某个 annotations: 段里(前面只能是同段的其它注解)、
# 值是加双引号的 12 位小写十六进制,否则 throw。
function Get-ZoneRedisAuthHash {
    param(
        [Parameter(Mandatory = $true)][string]$Block,
        [Parameter(Mandatory = $true)][string]$Label
    )
    $count = [regex]::Matches($Block, [regex]::Escape($ZoneRedisAuthHashKey)).Count
    if ($count -eq 0) { return $null }
    if ($count -ne 1) { throw "${Label}:zone Redis 口令指纹注解应恰好 1 处,实际 $count 处" }
    $pattern = '(?m)^(?<ind>[ ]+)annotations:[ ]*\r?\n(?:\k<ind>  [a-z0-9./-]+: "[^"\r\n]*"[ ]*\r?\n)*\k<ind>  ' + [regex]::Escape($ZoneRedisAuthHashKey) + ': "(?<hash>[0-9a-f]{12})"[ ]*\r?$'
    $m = [regex]::Match($Block, $pattern)
    if (-not $m.Success) { throw "${Label}:zone Redis 口令指纹必须在 Pod 模板的 annotations: 段里,值为加双引号的 12 位小写十六进制" }
    return $m.Groups['hash'].Value
}

# 换口令:只换 MMORPG_REDIS_PASSWORD,其余与 $prodRun 相同;-SkipGoSvc / -SkipJavaSvc:本次只看 C++ 节点。
$ZoneRedisRotatedPassword = 'contract-test-zone-redis-rotated-03'
$zoneRedisRotEnv = @{} + $ProdEnv
$zoneRedisRotEnv['MMORPG_REDIS_PASSWORD'] = $ZoneRedisRotatedPassword
$zoneRedisRotRun = Invoke-DeployDryRun -Arguments ($ProdArgs + @('-SkipGoSvc', '-SkipJavaSvc')) -Env $zoneRedisRotEnv
# dev 档注入口令(dev 档非空即采用,release_common.ps1 Resolve-InjectedSecret):gate 走 lib 的 StatefulSet、scene 走 Agones Fleet,
# 覆盖另外两套模板。参数同 k8s_client_entry_contract.tests.ps1 的 $ExternalArgs。
$ZoneRedisDevPassword = 'contract-test-zone-redis-dev-04'
$zoneRedisEntryRun = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SkipGoSvc', '-SkipJavaSvc', '-SceneOrchestrator', 'agones',
    '-ClientEntryMode', 'external', '-GateServiceType', 'NodePort', '-GateNodePortBase', '30000', '-ClientPublicHost', '127.0.0.1')) -Env @{ MMORPG_REDIS_PASSWORD = $ZoneRedisDevPassword }

Test-Case 'zone Redis 口令生成期保护:未解析 / 非 dev 档为空 / 含引号、反斜杠或控制字符一律 throw 且不回显值;dev 档为空写空串、不写指纹' {
    try {
        $script:ZoneRedisAuthHashAnnotation = $ZoneRedisAuthHashKey
        $ReleaseProfile = 'prod'
        $script:RedisPassword = $null
        $message = ''
        try { Get-CppZoneRedisPassword | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern 'zone Redis 口令未解析' -Because '没跑 Initialize-InjectedSecrets 就生成 C++ 配置必须 throw,不能把「没解析」当成「没口令」写出空串'

        $script:RedisPassword = ''
        $message = ''
        try { Get-CppZoneRedisPassword | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern ([regex]::Escape('ReleaseProfile=prod 要求 zone Redis 口令非空(MMORPG_REDIS_PASSWORD)')) -Because '非 dev 档空口令必须在生成期 throw(该档 Redis 开 requirepass,C++ 不带口令全线 NOAUTH);这是 Resolve-InjectedSecret 之后的纵深防御'

        foreach ($bad in @(
            @{ Why = '双引号'; Value = 'contract-zr"pw-0001' }
            @{ Why = '反斜杠'; Value = 'contract-zr\npw-0001' }
            @{ Why = '换行'; Value = "contract-zr`npw-0001" }
            @{ Why = '回车'; Value = "contract-zr`rpw-0001" }
            @{ Why = '制表符'; Value = "contract-zr`tpw-0001" }
        )) {
            $script:RedisPassword = $bad.Value
            # 两个入口:Get-CppZoneRedisPassword(生成 node-config 时)与 Assert-RedisPasswordYamlSafe(Initialize-InjectedSecrets 里、任何写操作之前)。
            foreach ($probe in @(
                @{ Name = 'Get-CppZoneRedisPassword'; Call = { Get-CppZoneRedisPassword | Out-Null } }
                @{ Name = 'Assert-RedisPasswordYamlSafe'; Call = { Assert-RedisPasswordYamlSafe -Password $script:RedisPassword } }
            )) {
                $message = ''
                try { & $probe.Call } catch { $message = $_.Exception.Message }
                Assert-Match -Text $message -Pattern '含双引号、反斜杠、换行或其它控制字符' -Because "$($bad.Why) / $($probe.Name):值要原样嵌进 YAML 双引号标量,会把 YAML 写坏或被转义静默改写,必须 throw"
                Assert-Match -Text $message -Pattern 'MMORPG_REDIS_PASSWORD' -Because "$($bad.Why) / $($probe.Name):报错要点名变量"
                Assert-True -Condition (-not $message.Contains($bad.Value)) -Because "$($bad.Why) / $($probe.Name):拒绝路径只点变量名,不回显口令值"
            }
        }

        $ReleaseProfile = 'dev'
        $script:RedisPassword = ''
        Assert-Equal -Expected '' -Actual (Get-CppZoneRedisPassword) -Because 'dev 档未注入:写空串,C++ 不发 AUTH,与改动前相同'
        Assert-Equal -Expected 0 -Actual (Get-CppZoneRedisAuthAnnotations).Count -Because 'dev 档无口令:不写指纹注解,Pod 模板与改动前逐字相同'
        $script:RedisPassword = 'contract-zr-pw-0001'
        $first = Get-CppZoneRedisAuthAnnotations
        Assert-Equal -Expected $ZoneRedisAuthHashKey -Actual (@($first.Keys) -join ',') -Because 'dev 档注入了口令同样要写指纹(dev 非空即采用,C++ 会发 AUTH)'
        Assert-Match -Text ([string]$first[$ZoneRedisAuthHashKey]) -Pattern '^[0-9a-f]{12}$' -Because '指纹是 12 位小写十六进制(Get-InjectedSecretFingerprint)'
        $script:RedisPassword = 'contract-zr-pw-0002'
        Assert-True -Condition ([string](Get-CppZoneRedisAuthAnnotations)[$ZoneRedisAuthHashKey] -ne [string]$first[$ZoneRedisAuthHashKey]) -Because '口令变了指纹必须变,否则换口令不会滚动 C++ Pod'
    }
    finally {
        Remove-Variable -Name RedisPassword, ZoneRedisAuthHashAnnotation -Scope Script -ErrorAction SilentlyContinue
    }
}

Test-Case 'zone Redis 口令(prod):node-config 的 zoneredis.password == MMORPG_REDIS_PASSWORD;gate / scene 带只哈希口令的指纹,与 sidecar 注解同在一个 annotations 段' {
    Assert-Equal -Expected 0 -Actual $prodRun.ExitCode -Because "prod 基线 DryRun 应当成功。输出: $($prodRun.Output)"
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $prodRun.Output -Name 'node-config')
    $password = Get-FlatValue -Flat $flat -KeyPath 'data.game_config.yaml.zoneredis.password'
    Assert-True -Condition ($password -ceq $ProdEnv.MMORPG_REDIS_PASSWORD) -Because 'C++ 的 zone Redis 口令必须来自 MMORPG_REDIS_PASSWORD(与 Go 服务 / gateway 的 ConfigMap 同一个值)'
    Assert-Equal -Expected '0' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.game_config.yaml.zoneredis.db') -Because 'C++ 不发 SELECT,库号钉在 0(跨运行时契约)'
    $expected = Get-ExpectedZoneRedisAuthHash -Password $ProdEnv.MMORPG_REDIS_PASSWORD
    foreach ($name in @('gate', 'scene')) {
        $block = Select-CppWorkloadBlock -Output $prodRun.Output -Kind 'Deployment' -Name $name -Label "prod $name"
        Assert-Equal -Expected $expected -Actual (Get-ZoneRedisAuthHash -Block $block -Label "prod $name") -Because "${name}:指纹必须只由口令决定(期望值只用口令算出)"
        Assert-Match -Text $block -Pattern '(?m)^      annotations:[ ]*\r?\n        mmorpg\.io/cpp-log-sidecar-config-hash: "[0-9a-f]{12}"[ ]*\r?\n        mmorpg\.io/zone-redis-auth-hash: "[0-9a-f]{12}"[ ]*\r?\n(?:        prometheus\.io/(?:scrape|port|path): "[^"]*"[ ]*\r?\n){3}      labels:' -Because "${name}:两条注解必须同在 Pod 模板的一个 annotations 段(sidecar 在前);重复的 annotations: 键各解析器行为不一"
        Assert-True -Condition (-not $block.Contains($ProdEnv.MMORPG_REDIS_PASSWORD)) -Because "${name}:工作负载里只有指纹,不出现口令明文"
    }
}

Test-Case 'C++ Prometheus 出口(-CppMetricsPort 缺省 9400):gate / scene 的 Deployment 与 scene Fleet 都带 MMORPG_METRICS_PORT、metrics 容器端口与 prometheus.io/* 抓取注解' {
    $cases = @(
        @{ Label = 'dev gate Deployment'; Block = (Select-CppWorkloadBlock -Output $devOut -Kind 'Deployment' -Name 'gate' -Label 'dev gate') },
        @{ Label = 'dev scene Deployment'; Block = (Select-CppWorkloadBlock -Output $devOut -Kind 'Deployment' -Name 'scene' -Label 'dev scene') },
        @{ Label = 'dev scene Fleet'; Block = (Select-CppWorkloadBlock -Output $agonesOut -Kind 'Fleet' -Name 'scene' -Label 'dev scene Fleet') }
    )
    foreach ($case in $cases) {
        $block = $case.Block -replace "`r`n", "`n"
        Assert-Match -Text $block -Pattern '- name: MMORPG_METRICS_PORT\s+value: "9400"' -Because "$($case.Label):进程按这个变量开 /metrics(cpp/libs/engine/core/metrics/metrics_http_exporter.h)"
        Assert-Match -Text $block -Pattern 'containerPort: 9400\s+name: metrics' -Because "$($case.Label):声明 metrics 容器端口"
        Assert-Match -Text $block -Pattern 'prometheus\.io/scrape: "true"' -Because "$($case.Label):与 Go 服务同一套抓取注解"
        Assert-Match -Text $block -Pattern 'prometheus\.io/port: "9400"' -Because "$($case.Label):抓取端口与 env 同值"
        Assert-Match -Text $block -Pattern 'prometheus\.io/path: "/metrics"' -Because "$($case.Label):出口只认 GET /metrics"
    }
}

Test-Case 'zone Redis 口令(dev 基线,不注入):node-config 写空串,C++ 工作负载不带指纹,注解段与改动前逐字相同' {
    # 跑测进程不设 MMORPG_REDIS_PASSWORD(CI 与本机约定同全文件的 dev 基线);设了的话下面会红,而不是静默放过。
    foreach ($case in @(@{ Label = 'dev Deployment'; Output = $devOut }, @{ Label = 'dev Agones'; Output = $agonesOut })) {
        $block = Select-ManifestByName -Output $case.Output -Name 'node-config'
        $flat = ConvertTo-FlatManifest -Block $block
        Assert-Equal -Expected '' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.game_config.yaml.zoneredis.password') -Because "$($case.Label):不注入口令时 C++ 不发 AUTH,与改动前相同"
        # 核对实际配置字段,不把 Windows DryRun 捕获时增加的空行当成配置变化。
        Assert-Equal -Expected 'redis.mmorpg-infra' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.game_config.yaml.zoneredis.host') -Because "$($case.Label):zone Redis 仍使用默认 infra namespace 的共享实例"
        Assert-Equal -Expected '6379' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.game_config.yaml.zoneredis.port') -Because "$($case.Label):zone Redis 端口保持 6379"
        Assert-Equal -Expected '0' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.game_config.yaml.zoneredis.db') -Because "$($case.Label):C++ 不发 SELECT,库号仍为 0"
        Assert-NotMatch -Text $case.Output -Pattern 'zone-redis-auth-hash' -Because "$($case.Label):口令为空不写指纹注解"
    }
    $gate = Select-CppWorkloadBlock -Output $devOut -Kind 'Deployment' -Name 'gate' -Label 'dev gate'
    Assert-Match -Text $gate -Pattern '(?m)^    metadata:[ ]*\r?\n      annotations:[ ]*\r?\n        mmorpg\.io/cpp-log-sidecar-config-hash: "[0-9a-f]{12}"[ ]*\r?\n(?:        prometheus\.io/(?:scrape|port|path): "[^"]*"[ ]*\r?\n){3}      labels:[ ]*\r?\n        app: gate[ ]*\r?$' -Because 'Deployment 模板的注解段改由 New-CppPodAnnotationBlock 渲染,dev 无口令时只有 sidecar 一条指纹,其后是 -CppMetricsPort 的三条抓取注解'
    $fleet = Select-CppWorkloadBlock -Output $agonesOut -Kind 'Fleet' -Name 'scene' -Label 'dev scene Fleet'
    Assert-Match -Text $fleet -Pattern '(?m)^        metadata:[ ]*\r?\n          annotations:[ ]*\r?\n            mmorpg\.io/cpp-log-sidecar-config-hash: "[0-9a-f]{12}"[ ]*\r?\n(?:            prometheus\.io/(?:scrape|port|path): "[^"]*"[ ]*\r?\n){3}          labels:[ ]*\r?\n            app: scene[ ]*\r?$' -Because 'Fleet 模板同上(Pod 模板 metadata 子键在第 10 列)'
}

Test-Case 'zone Redis 口令换了,gate / scene 的指纹跟着变(换口令自动滚动),sidecar 指纹不变,node-config 写新值' {
    Assert-Equal -Expected 0 -Actual $zoneRedisRotRun.ExitCode -Because "只换 Redis 口令的 prod DryRun 应当成功。输出: $($zoneRedisRotRun.Output)"
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $zoneRedisRotRun.Output -Name 'node-config')
    Assert-True -Condition ((Get-FlatValue -Flat $flat -KeyPath 'data.game_config.yaml.zoneredis.password') -ceq $ZoneRedisRotatedPassword) -Because 'node-config 里必须是换过的新口令(与指纹同一份输入)'
    $expected = Get-ExpectedZoneRedisAuthHash -Password $ZoneRedisRotatedPassword
    foreach ($name in @('gate', 'scene')) {
        $rotatedBlock = Select-CppWorkloadBlock -Output $zoneRedisRotRun.Output -Kind 'Deployment' -Name $name -Label "换口令 $name"
        $baselineBlock = Select-CppWorkloadBlock -Output $prodRun.Output -Kind 'Deployment' -Name $name -Label "prod $name"
        $rotated = Get-ZoneRedisAuthHash -Block $rotatedBlock -Label "换口令 $name"
        $baseline = Get-ZoneRedisAuthHash -Block $baselineBlock -Label "prod $name"
        Assert-Equal -Expected $expected -Actual $rotated -Because "${name}:换口令后的指纹必须是新口令算出的值"
        Assert-True -Condition ($rotated -ne $baseline) -Because "${name}:口令变了指纹必须变(两者都是 $baseline),否则只改 ConfigMap 不会滚动,C++ 一直拿旧口令"
        $sidecarPattern = 'mmorpg\.io/cpp-log-sidecar-config-hash: "(?<h>[0-9a-f]{12})"'
        Assert-Equal -Expected ([regex]::Match($baselineBlock, $sidecarPattern).Groups['h'].Value) -Actual ([regex]::Match($rotatedBlock, $sidecarPattern).Groups['h'].Value) -Because "${name}:sidecar 配置没变,它的指纹不该跟着口令变(两条注解各管各的输入)"
    }
}

Test-Case 'zone Redis 口令(dev 档注入):external gate StatefulSet 与 Agones scene Fleet 同样带指纹,node-config 写注入值' {
    Assert-Equal -Expected 0 -Actual $zoneRedisEntryRun.ExitCode -Because "external + Agones 的 dev DryRun 应当成功。输出: $($zoneRedisEntryRun.Output)"
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $zoneRedisEntryRun.Output -Name 'node-config')
    Assert-True -Condition ((Get-FlatValue -Flat $flat -KeyPath 'data.game_config.yaml.zoneredis.password') -ceq $ZoneRedisDevPassword) -Because 'dev 档注入了口令就写进 node-config(dev 非空即采用)'
    $expected = Get-ExpectedZoneRedisAuthHash -Password $ZoneRedisDevPassword
    foreach ($target in @(@{ Kind = 'StatefulSet'; Name = 'gate' }, @{ Kind = 'Fleet'; Name = 'scene' })) {
        $label = "dev 注入 $($target.Kind)/$($target.Name)"
        $block = Select-CppWorkloadBlock -Output $zoneRedisEntryRun.Output -Kind $target.Kind -Name $target.Name -Label $label
        Assert-Equal -Expected $expected -Actual (Get-ZoneRedisAuthHash -Block $block -Label $label) -Because "${label}:lib 模板与本脚本模板必须写同一个指纹"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, '(?m)^[ ]+annotations:[ ]*\r?$').Count) -Because "${label}:一个 Pod 模板只能有一个 annotations: 段"
    }
}

Test-Case '负向:prod 缺 MMORPG_REDIS_PASSWORD 在任何写操作之前拒绝并点名变量(C++ 不会拿空口令去连开了 requirepass 的 Redis)' {
    $env2 = @{} + $ProdEnv
    $env2['MMORPG_REDIS_PASSWORD'] = ''
    $run = Invoke-DeployDryRun -Arguments $ProdArgs -Env $env2
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "非 dev 档空口令必须阻断。输出: $($run.Output)"
    # 其余生产密钥都已给齐,拒绝只能来自这一把;CLI 路径上先拒的是 Resolve-InjectedSecret,生成期那道(Get-CppZoneRedisPassword)由上面的 AST 用例覆盖。
    Assert-Match -Text $run.Output -Pattern 'ReleaseProfile=prod 要求从环境变量 MMORPG_REDIS_PASSWORD' -Because '错误必须点名缺哪个环境变量,且说清是注入缺失'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '密钥解析(Initialize-InjectedSecrets)先于任何写操作,不留半截部署'
    Assert-NotMatch -Text $run.Output -Pattern 'zone-redis-auth-hash' -Because '被拒时不该渲染出任何 C++ 工作负载'
}

Test-Case '负向:MMORPG_REDIS_PASSWORD 含双引号时在任何写操作之前拒绝,只点变量名不回显值' {
    $badPassword = 'contract-test-zone"redis-05'
    $env2 = @{} + $ProdEnv
    $env2['MMORPG_REDIS_PASSWORD'] = $badPassword
    $run = Invoke-DeployDryRun -Arguments $ProdArgs -Env $env2
    Assert-True -Condition ($run.ExitCode -ne 0) -Because '口令含双引号必须阻断:它要原样写进多份 YAML 双引号标量'
    Assert-Match -Text $run.Output -Pattern '含双引号、反斜杠、换行或其它控制字符' -Because '错误要说清是字符集问题'
    Assert-Match -Text $run.Output -Pattern 'MMORPG_REDIS_PASSWORD' -Because '错误要点名变量'
    Assert-True -Condition (-not ([string]$run.Output).Contains($badPassword)) -Because '拒绝路径不回显口令值'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '字符集检查在 Initialize-InjectedSecrets 里,先于任何写操作'
}

# ─────────────────────────────────────────────────────────────────
# 3b. 削峰(G4b:docs/handoff/launch-readiness-20261013/spec_G4b.md §2、§6):gateway Bucket4j 限流 / login 登录排队
# ─────────────────────────────────────────────────────────────────
# k8s_deploy.ps1 按 -ReleaseProfile 生成:dev 两项都关(产物与 G4b 之前逐字相同),staging / prod 两项都开;阈值只有一份
# ($script:PeakShavingLaunchTier,全部待压测确认)。-GatewayRateLimit / -LoginQueue 取 auto|on|off(auto = 跟随档位),
# off 只用于止血与基线压测。复用上面的 $devOut(dev 缺省)与 $prodRun(prod 缺省),另起 11 个 DryRun 子进程:下面 6 个,
# 用例 10 一个、用例 11 两个、用例 19 / 20 各一个在用例里起。部署子进程一律带 -DryRun($BaseArgs / $ProdArgs 已带),不碰任何集群。
# 2026-10-15 起排队还要过合入门槛(k8s_deploy.ps1 的 $script:LoginQueueMergeGateCleared):未清时任何档位都不开排队。下面断「排队开」
# 的 DryRun 经 $ProdEnv 或显式给的 MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED=1(只在 -DryRun 生效的测试接缝)视为门槛已清;门槛本身见用例 19–22。
# 计划行与警告是 Show-PeakShavingPlan 打的英文原文;警告只断开头不含空格的「[PeakShaving]Xxx=yyy」一段
# (Write-Warning 可能在空格处折行,同下文 MySQL 连接总账、gateway 管理面口令两节的说明)。
$PeakNodeImage = 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab'
$peakStagingRun = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-ReleaseProfile', 'staging', '-SkipPreflight', '-NodeImage', $PeakNodeImage)) -Env $ProdEnv
# 两项都显式关,排队密钥置空:关排队时不该要求它。拷贝用 @{} + 再按键赋值(+ 合并同名键会抛异常)。
$peakProdOffEnv = @{} + $ProdEnv
$peakProdOffEnv['MMORPG_LOGIN_QUEUE_TOKEN_SECRET'] = ''
# 显式 off 不依赖排队开,不带合入门槛的测试接缝:顺带覆盖「门槛未清 + 显式 off」仍打原来的 LoginQueue=off 警告(用例 9)。
$peakProdOffEnv['MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED'] = ''
$peakProdOffRun = Invoke-DeployDryRun -Arguments ($ProdArgs + @('-GatewayRateLimit', 'off', '-LoginQueue', 'off')) -Env $peakProdOffEnv
# dev 档显式开两项(联调用法)。两个变量显式置空,结论不受跑测机器上残留的同名环境变量影响。
# -LoginQueue on 在门槛未清时直接拒绝,所以带上只在 -DryRun 生效的测试接缝(见 $ProdEnv 的说明)。
$peakDevOnRun = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-GatewayRateLimit', 'on', '-LoginQueue', 'on')) -Env @{ MMORPG_LOGIN_QUEUE_TOKEN_SECRET = ''; MMORPG_REDIS_PASSWORD = ''; MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED = '1' }
# 多 zone:zones.sample.json 的全部 zone。-ZonesConfigPath 给绝对路径(子进程的工作目录不保证是仓库根);
# -SkipInfra 不装配 battle 全局池(否则还要 battle 密钥),-SkipJavaSvc:本次只看 login。
$PeakAllUpArgs = @(
    '-Command', 'all-up', '-DryRun', '-SkipInfra', '-SkipJavaSvc',
    '-ZonesConfigPath', (Join-Path (Get-RepoRoot) 'deploy/k8s/zones.sample.json'),
    '-GoSvcRegistry', 'registry.invalid/test',
    '-ReleaseProfile', 'prod', '-SkipPreflight', '-NodeImage', $PeakNodeImage
)
$peakAllUpRun = Invoke-DeployDryRun -Arguments $PeakAllUpArgs -Env $ProdEnv
# 只换 Redis 口令(gateway 削峰指纹的输入之一);-SkipGoSvc:本次只看 gateway。
$PeakRotatedRedisPassword = 'contract-test-redis-pw-rotated-02'
$peakGwRedisRotEnv = @{} + $ProdEnv
$peakGwRedisRotEnv['MMORPG_REDIS_PASSWORD'] = $PeakRotatedRedisPassword
$peakGwRedisRotRun = Invoke-DeployDryRun -Arguments ($ProdArgs + @('-SkipGoSvc')) -Env $peakGwRedisRotEnv
# 只加可信代理网段(gateway 削峰指纹的另一份输入:rate-limit 段文本),密钥与 $prodRun 完全相同;-SkipGoSvc:本次只看 gateway。
# 没有 Ingress 时给可信代理不会被入口预检拒绝(lib/k8s_client_entry.ps1 只在给了 -GatewayIngressHost 却没给它时报错)。
$PeakTrustedProxies = '10.0.0.0/8'
$peakGwProxyRun = Invoke-DeployDryRun -Arguments ($ProdArgs + @('-SkipGoSvc', '-GatewayTrustedProxies', $PeakTrustedProxies)) -Env $ProdEnv

# 拍平后的 manifest 里某个路径本身及其全部子键(序数比较;'Queue' 不会误中 'QueueXxx')。
function Get-FlatSubtreeKeys {
    param(
        [Parameter(Mandatory = $true)]$Flat,
        [Parameter(Mandatory = $true)][string]$Path
    )
    return @($Flat.Scalars.Keys | ForEach-Object { [string]$_ } | Where-Object {
        $_ -ceq $Path -or $_.StartsWith("$Path.", [System.StringComparison]::Ordinal) -or $_.StartsWith("$Path[", [System.StringComparison]::Ordinal)
    })
}

# Pod 模板上的削峰指纹注解(mmorpg.io/peak-shaving-hash)。gateway:紧跟在口令指纹那一行之后(Add-PodTemplateAnnotation 把 -Key
# 恒排在 annotations: 的下一行,下文 Get-GatewayAdminApiKeyHash 依赖这个位置),顺带取出口令指纹;login:annotations: 的下一行。
# Select-ManifestByName 已把块内换行统一成 LF,所以按 \n 锚定(整段原始输出在 Windows 上是 CRLF)。
# 缺失 / 多于一处 / 位置不对 / 不是加双引号的 12 位小写十六进制,一律 throw,不返回半截结果。
function Get-PeakShavingAnnotation {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output,
        [Parameter(Mandatory = $true)][ValidateSet('gateway', 'login')][string]$Workload,
        [Parameter(Mandatory = $true)][string]$Label
    )
    $block = Select-ManifestByName -Output $Output -Name $Workload
    if ($null -eq $block) { throw "${Label}:DryRun 输出里没有 $Workload 的 manifest" }
    $count = [regex]::Matches($block, 'mmorpg\.io/peak-shaving-hash').Count
    if ($count -ne 1) { throw "${Label}:$Workload manifest 里削峰指纹注解应恰好 1 处,实际 $count 处" }
    $pattern = 'annotations:[ ]*\n[ ]+mmorpg\.io/peak-shaving-hash: "(?<peak>[0-9a-f]{12})"'
    if ($Workload -eq 'gateway') {
        $pattern = 'annotations:[ ]*\n[ ]+mmorpg\.io/gateway-admin-api-key-hash: "(?<admin>[0-9a-f]{12})"[ ]*\n[ ]+mmorpg\.io/peak-shaving-hash: "(?<peak>[0-9a-f]{12})"'
    }
    $m = [regex]::Match($block, $pattern)
    if (-not $m.Success) { throw "${Label}:$Workload 的削峰指纹必须在 Pod 模板 annotations 下(gateway 排在口令指纹之后),值为加双引号的 12 位小写十六进制" }
    return [pscustomobject]@{ Peak = $m.Groups['peak'].Value; AdminKey = $m.Groups['admin'].Value }
}

# 从 manifest 块里取出「<SectionKey>:」那一行连同其下缩进更深的所有行(去掉行尾空白与空行),用于逐字比对两次运行的同一段配置。
# 该键必须恰好出现一处且下面至少有一行,否则 throw(fail-closed:两段都是空的也「相等」,不能算通过)。
function Get-ManifestSectionText {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()][string]$Block,
        [Parameter(Mandatory = $true)][string]$SectionKey,
        [Parameter(Mandatory = $true)][string]$Label
    )
    if ([string]::IsNullOrWhiteSpace($Block)) { throw "${Label}:manifest 块为空" }
    $lines = @($Block -split "`r?`n" | ForEach-Object { $_.TrimEnd() } | Where-Object { $_ -ne '' })
    $keyPattern = '^[ ]*' + [regex]::Escape($SectionKey) + ':$'
    $starts = @(for ($i = 0; $i -lt $lines.Count; $i++) { if ($lines[$i] -cmatch $keyPattern) { $i } })
    if ($starts.Count -ne 1) { throw "${Label}:「${SectionKey}:」应恰好 1 处,实际 $($starts.Count) 处" }
    $keyIndent = $lines[$starts[0]].Length - $lines[$starts[0]].TrimStart(' ').Length
    $section = New-Object System.Collections.Generic.List[string]
    $section.Add($lines[$starts[0]])
    for ($i = $starts[0] + 1; $i -lt $lines.Count; $i++) {
        if (($lines[$i].Length - $lines[$i].TrimStart(' ').Length) -le $keyIndent) { break }
        $section.Add($lines[$i])
    }
    if ($section.Count -lt 2) { throw "${Label}:「${SectionKey}:」下面没有任何子行" }
    return ($section -join "`n")
}

# 常量表自检(用例 16):按 AST 从 k8s_deploy.ps1 抽出下面三个函数直接调用(与上面 deadline 守卫、下文 Kafka 拓扑同一做法)。
#   - Get-GatewayRateLimitSettings 只读 $script:GatewayRateLimitOn 与 $script:PeakShavingLaunchTier;
#   - New-LoginQueueYaml 读 $script:LoginQueueOn、$script:PeakShavingLaunchTier,并经 Get-AuthoritativeScalar 按 $RepoRoot 读
#     go/login/etc/login.yaml(Get-YamlScalar 来自 deploy_capture.ps1 dot-source 的 release_common.ps1)。
# 脚本级变量由用例在本脚本作用域里设好、用完即删;$RepoRoot 由用例自己设(下文 MySQL 节同样抽 Get-AuthoritativeScalar,重设无害)。
$peakFunctionNames = @('Get-GatewayRateLimitSettings', 'New-LoginQueueYaml', 'Get-AuthoritativeScalar')
foreach ($peakFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($peakFunctionNames -contains $peakFnAst.Name) {
        Set-Item -Path "Function:script:$($peakFnAst.Name)" -Value $peakFnAst.Body.GetScriptBlock()
    }
}
foreach ($peakFnName in $peakFunctionNames) {
    if (-not (Test-Path "Function:$peakFnName")) { throw "k8s_deploy.ps1 缺少函数 $peakFnName(削峰阈值 / 排队段的生成与自检被删了?后续断言无意义)" }
}

Test-Case '削峰 dev 档缺省关:gateway 不写 gate.rate-limit.enabled,login 不写 Queue / Secrets.QueueToken,两者无 peak-shaving 注解(产物与 G4b 前相同)' {
    # dev 档(auto)两项都关:落回 jar 内 / login.yaml 的 enabled: false,本地与 kind 联调不受削峰影响。
    # 基线 DryRun 没给 -GatewayTrustedProxies,所以 gateway 的 application.yaml 里连 rate-limit 键都不该有。
    $gatewayFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'java-svc-gateway-config')
    $rateLimitKeys = @(Get-FlatSubtreeKeys -Flat $gatewayFlat -Path 'data.application.yaml.gate.rate-limit')
    Assert-True -Condition ($rateLimitKeys.Count -eq 0) -Because "dev 档 gateway ConfigMap 不得写 gate.rate-limit(实际出现:$($rateLimitKeys -join ', '))"
    $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-login-config')
    $queueKeys = @(@(Get-FlatSubtreeKeys -Flat $loginFlat -Path 'data.login.yaml.Queue') + @(Get-FlatSubtreeKeys -Flat $loginFlat -Path 'data.login.yaml.Secrets.QueueToken'))
    Assert-True -Condition ($queueKeys.Count -eq 0) -Because "dev 档 login ConfigMap 不得写 Queue / Secrets.QueueToken(实际出现:$($queueKeys -join ', '))"
    Assert-NotMatch -Text $devOut -Pattern 'peak-shaving-hash' -Because 'dev 档 gateway / login 的 Pod 模板都不该带削峰指纹(关 = 不写注解,模板与 G4b 之前相同)'
    Assert-Match -Text $devOut -Pattern 'PeakShaving: gateway_rate_limit=off\(-GatewayRateLimit auto\) login_queue=off\(-LoginQueue auto\) profile=dev ' -Because '写路径入口要打出两个开关的解析结果:dev 档 auto = 关'
}

Test-Case '削峰 staging / prod 档开:gateway 阈值 == 开服档起步值,rate-limit: 只有一个' {
    # 负责人 2026-10-13 拍板的开服档起步值:zone 2000/s、突发 5000;IP 5/s、突发 20;账号冷却 5000ms;排队容量 5000/zone。
    # 全部待压测确认(L-14)。这张表就是被评审的那个决定:改 k8s_deploy.ps1 的 $script:PeakShavingLaunchTier 要同步改这里。
    $expected = [ordered]@{
        'enabled'             = 'true'
        'zone-default-rps'    = '2000'
        'zone-default-burst'  = '5000'
        'ip-rps'              = '5'
        'ip-burst'            = '20'
        'account-cooldown-ms' = '5000'
    }
    foreach ($case in @(@{ Label = 'staging'; Run = $peakStagingRun }, @{ Label = 'prod'; Run = $prodRun })) {
        Assert-Equal -Expected 0 -Actual $case.Run.ExitCode -Because "$($case.Label) 档 DryRun 应当成功。输出: $($case.Run.Output)"
        $block = Select-ManifestByName -Output $case.Run.Output -Name 'java-svc-gateway-config'
        $flat = ConvertTo-FlatManifest -Block $block
        foreach ($key in $expected.Keys) {
            Assert-Equal -Expected $expected[$key] -Actual (Get-FlatValue -Flat $flat -KeyPath "data.application.yaml.gate.rate-limit.$key") -Because "$($case.Label) 档 gate.rate-limit.$key 必须是开服档起步值"
        }
        # 阈值与 trusted-proxies 共用一个 rate-limit: 键:另起一个就是重复键,Spring 的 SnakeYAML 直接拒启。
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, '(?m)^[ ]+rate-limit:[ ]*\r?$').Count) -Because "$($case.Label) 档 java-svc-gateway-config 里 rate-limit: 只能有一个"
        # 与 release_preflight.ps1 F 节同一条规则(rps ≥ 1、burst ≥ rps),这里核的是集群里真正生效的那份值。
        foreach ($pair in @(@('zone-default-rps', 'zone-default-burst'), @('ip-rps', 'ip-burst'))) {
            $rps = [long](Get-FlatValue -Flat $flat -KeyPath "data.application.yaml.gate.rate-limit.$($pair[0])")
            $burst = [long](Get-FlatValue -Flat $flat -KeyPath "data.application.yaml.gate.rate-limit.$($pair[1])")
            Assert-True -Condition ($rps -ge 1 -and $burst -ge $rps) -Because "$($case.Label) 档 $($pair[0])=$rps / $($pair[1])=$burst 必须满足 rps ≥ 1 且 burst ≥ rps"
        }
        $planPattern = 'PeakShaving: gateway_rate_limit=on\(-GatewayRateLimit auto\) login_queue=on\(-LoginQueue auto\) profile=' + $case.Label +
            ' initial values \(pending stress-test validation\) zone=2000/s burst=5000 ip=5/s burst=20 cooldown=5000ms queue_capacity=5000/zone'
        Assert-Match -Text $case.Run.Output -Pattern $planPattern -Because "$($case.Label) 档 auto = 开,写路径入口要打出开关解析结果与起步值"
    }
}

Test-Case '削峰 staging / prod 档开:login Queue 段除三项外 == go/login/etc/login.yaml,锁键带本 zone,ZoneCapacityOverride 只有本 zone' {
    $loginYaml = 'go/login/etc/login.yaml'
    # Get-EtcValue 去掉引号后的基名。G1b 之后 login 把配置值当前缀、自己再拼一次 :zone:<id>;这里显式带 zone 防的是
    # rollout undo 退回 G1b 之前的镜像(见 k8s_deploy.ps1 New-LoginQueueYaml)。
    $lockKeyBase = Get-EtcValue -RelativePath $loginYaml -KeyPath 'Queue.DispatcherLockKey'
    foreach ($case in @(@{ Label = 'staging'; Run = $peakStagingRun }, @{ Label = 'prod'; Run = $prodRun })) {
        $block = Select-ManifestByName -Output $case.Run.Output -Name 'go-svc-login-config'
        $flat = ConvertTo-FlatManifest -Block $block
        Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Queue.Enabled') -Because "$($case.Label) 档 login 排队必须打开"
        foreach ($key in @('DispatchInterval', 'AdmitTTL', 'QueueEntryTTL', 'SoftCapMultiplier', 'DefaultRetryAfterMs', 'DispatcherLockTTL')) {
            Assert-Equal -Expected (Get-EtcValue -RelativePath $loginYaml -KeyPath "Queue.$key") -Actual (Get-FlatValue -Flat $flat -KeyPath "data.login.yaml.Queue.$key") -Because "$($case.Label) 档 Queue.$key 必须镜像 $loginYaml(生成器不得自带常数)"
        }
        $lockKey = Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Queue.DispatcherLockKey'
        Assert-True -Condition ($lockKey -ceq "${lockKeyBase}:zone:101") -Because "$($case.Label) 档锁键必须是 <login.yaml 基名>:zone:<本 zone>(期望 '${lockKeyBase}:zone:101',实际 '$lockKey')"
        $capacityKeys = @(Get-FlatSubtreeKeys -Flat $flat -Path 'data.login.yaml.Queue.ZoneCapacityOverride' | Where-Object { $_ -cne 'data.login.yaml.Queue.ZoneCapacityOverride' })
        Assert-Equal -Expected 'data.login.yaml.Queue.ZoneCapacityOverride.101' -Actual ($capacityKeys -join ',') -Because "$($case.Label) 档 ZoneCapacityOverride 只写本 zone(每个 zone 的 login 只出队本 zone)"
        Assert-Equal -Expected '5000' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Queue.ZoneCapacityOverride.101') -Because "$($case.Label) 档本 zone 的排队容量是开服档起步值 5000(同时在线上限,不是速率)"
        # ConfigMap 把 login.yaml 整体缩进 4 格嵌在 login.yaml: | 下;Queue 与 Secrets 都只能各有一个顶层键
        # (QueueToken 另起一个 Secrets: 就是重复键,后者覆盖前者,InternalAuth 会丢)。
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, '(?m)^ {4}Queue:[ ]*\r?$').Count) -Because "$($case.Label) 档 login.yaml 里顶层 Queue: 只能有一个"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, '(?m)^ {4}Secrets:[ ]*\r?$').Count) -Because "$($case.Label) 档 QueueToken 必须并进已有的 Secrets: 段"
    }
}

Test-Case '削峰写出的键名与服务自己的配置同名(拼错会被静默忽略)' {
    # Spring 对 gate.rate-limit 下不认识的键、go-zero 对 Queue 下不认识的键都不报错,只是不生效 —— 只能在这里逐键核对。
    $gatewayYaml = 'java/gateway_node/src/main/resources/application.yaml'
    $gatewayFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $prodRun.Output -Name 'java-svc-gateway-config')
    $rateLimitPrefix = 'data.application.yaml.gate.rate-limit.'
    $rateLimitKeys = @(Get-FlatSubtreeKeys -Flat $gatewayFlat -Path 'data.application.yaml.gate.rate-limit' |
        Where-Object { $_.StartsWith($rateLimitPrefix, [System.StringComparison]::Ordinal) } | ForEach-Object { $_.Substring($rateLimitPrefix.Length) })
    Assert-Equal -Expected 6 -Actual $rateLimitKeys.Count -Because "prod 基线(未给 -GatewayTrustedProxies)应恰好写出 6 个阈值键(实际:$($rateLimitKeys -join ', '))"
    foreach ($key in $rateLimitKeys) {
        $found = Get-YamlScalar -Path (Join-Path (Get-RepoRoot) $gatewayYaml) -KeyPath "gate.rate-limit.$key"
        Assert-True -Condition $found.Found -Because "gate.rate-limit.$key 在 $gatewayYaml 里查不到:键名拼错,Spring 静默忽略,限流不按写出的值生效"
    }

    $loginYaml = 'go/login/etc/login.yaml'
    $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $prodRun.Output -Name 'go-svc-login-config')
    $queuePrefix = 'data.login.yaml.Queue.'
    $queueKeys = @(Get-FlatSubtreeKeys -Flat $loginFlat -Path 'data.login.yaml.Queue' |
        Where-Object { $_.StartsWith($queuePrefix, [System.StringComparison]::Ordinal) } | ForEach-Object { $_.Substring($queuePrefix.Length) } |
        Where-Object { -not $_.Contains('.') })
    Assert-True -Condition ($queueKeys -ccontains 'ZoneCapacityOverride') -Because "prod 基线应当写出 Queue.ZoneCapacityOverride(实际 Queue 下的键:$($queueKeys -join ', '))"
    $configGo = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'go/login/internal/config/config.go') -Raw
    $queueConf = [regex]::Match($configGo, '(?s)type QueueConf struct \{(?<body>.*?)\r?\n\}')
    Assert-True -Condition $queueConf.Success -Because 'go/login/internal/config/config.go 里应当有 type QueueConf struct(本断言的前提)'
    foreach ($key in $queueKeys) {
        # go-zero 按 json tag 绑定:每个写出的键都必须是 QueueConf 某个字段的 tag 名(区分大小写)。
        Assert-True -Condition ($queueConf.Groups['body'].Value -cmatch ('json:"' + [regex]::Escape($key) + '[,"]')) -Because "Queue.$key 不是 go/login/internal/config/config.go QueueConf 的字段:拼错的键 login 静默忽略"
        # login.yaml 里 ZoneCapacityOverride 只有注释示例,其余每个键(Enabled、6 个镜像键、DispatcherLockKey)都必须读得到。
        if ($key -cne 'ZoneCapacityOverride') {
            $found = Get-YamlScalar -Path (Join-Path (Get-RepoRoot) $loginYaml) -KeyPath "Queue.$key"
            Assert-True -Condition $found.Found -Because "Queue.$key 在 $loginYaml 里查不到:生成器写了服务自己的配置里没有的键"
        }
    }
    Assert-True -Condition ($queueConf.Groups['body'].Value.Contains('json:"ZoneCapacityOverride,optional"')) -Because 'ZoneCapacityOverride 在 login.yaml 里只有注释示例,改核 Go 结构体的 json tag(optional:不配即走回落公式)'
}

Test-Case '「不写即关」的前提:jar 内 gate.rate-limit.enabled 与 login.yaml Queue.Enabled 仍为 false,两处 Go/Java 缺省也是 false' {
    # dev 档与 off 靠「不写」关闭削峰:ConfigMap 不写这两段,就落回 jar 内 application.yaml / login.yaml,再落回类型缺省值。
    # 这四处任何一处翻成 true,dev 档就会在没人注意的情况下开着削峰跑。
    Assert-Equal -Expected 'false' -Actual (Get-EtcValue -RelativePath 'java/gateway_node/src/main/resources/application.yaml' -KeyPath 'gate.rate-limit.enabled') -Because 'jar 内 gate.rate-limit.enabled 必须保持 false'
    Assert-Equal -Expected 'false' -Actual (Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Queue.Enabled') -Because 'go/login/etc/login.yaml 的 Queue.Enabled 必须保持 false'
    $propsText = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'java/gateway_node/src/main/java/com/game/gateway/ratelimit/RateLimitProperties.java') -Raw
    # 顶层字段缩进 4 格;嵌套类 WaveSchedule 里的同名字段缩进 8 格,不能拿它凑数。
    Assert-True -Condition ($propsText -cmatch '(?m)^ {4}private boolean enabled = false;') -Because 'RateLimitProperties 的总开关缺省必须是 false'
    $configGo = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'go/login/internal/config/config.go') -Raw
    $queueConf = [regex]::Match($configGo, '(?s)type QueueConf struct \{(?<body>.*?)\r?\n\}')
    Assert-True -Condition $queueConf.Success -Because 'go/login/internal/config/config.go 里应当有 type QueueConf struct(本断言的前提)'
    Assert-True -Condition ($queueConf.Groups['body'].Value -cmatch ('(?m)^\s*Enabled\s+bool\s+' + [regex]::Escape('`json:"Enabled,default=false"`'))) -Because 'QueueConf.Enabled 的 go-zero 缺省必须是 false'
}

Test-Case 'staging / prod 的 gateway 带 spring.data.redis.password 且 == MMORPG_REDIS_PASSWORD;dev 无 Redis 口令时不写' {
    # Bucket4j 的 Lettuce 客户端只从 spring.data.redis.password 取口令;release 档 Redis 带 --requirepass,不写口令 = 限流的 Redis
    # 调用过不了认证(见 k8s_deploy.ps1 New-JavaSvcConfigMapYaml)。只看口令是否非空,与限流开关无关。测试不回显口令值。
    foreach ($case in @(@{ Label = 'staging'; Run = $peakStagingRun }, @{ Label = 'prod'; Run = $prodRun })) {
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $case.Run.Output -Name 'java-svc-gateway-config')
        $password = Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.spring.data.redis.password'
        Assert-True -Condition ($password -ceq $ProdEnv.MMORPG_REDIS_PASSWORD) -Because "$($case.Label) 档 gateway 的 Redis 口令必须来自 MMORPG_REDIS_PASSWORD(与 go-svc ConfigMap 同一个值)"
    }
    Assert-Equal -Expected 0 -Actual $peakDevOnRun.ExitCode -Because "dev 档显式开削峰的 DryRun 应当成功。输出: $($peakDevOnRun.Output)"
    $devFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $peakDevOnRun.Output -Name 'java-svc-gateway-config')
    Assert-True -Condition (-not $devFlat.Scalars.Contains('data.application.yaml.spring.data.redis.password')) -Because 'dev 档没有 Redis 口令(MMORPG_REDIS_PASSWORD 为空)时不写 password 键,dev 产物逐字不变'
}

Test-Case 'login Secrets.QueueToken.Value == MMORPG_LOGIN_QUEUE_TOKEN_SECRET,InternalAuth 不受影响' {
    foreach ($case in @(@{ Label = 'staging'; Run = $peakStagingRun }, @{ Label = 'prod'; Run = $prodRun })) {
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $case.Run.Output -Name 'go-svc-login-config')
        $queueToken = Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Secrets.QueueToken.Value'
        Assert-True -Condition ($queueToken -ceq $ProdEnv.MMORPG_LOGIN_QUEUE_TOKEN_SECRET) -Because "$($case.Label) 档 Secrets.QueueToken 必须来自 MMORPG_LOGIN_QUEUE_TOKEN_SECRET(生产模式开排队缺它 login 拒启)"
        $internalAuth = Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Secrets.InternalAuth.Value'
        Assert-True -Condition ($internalAuth -ceq $ProdEnv.MMORPG_INTERNAL_AUTH_SECRET) -Because "$($case.Label) 档 Secrets.InternalAuth 仍须是 MMORPG_INTERNAL_AUTH_SECRET:QueueToken 并进同一个 Secrets: 段,不能把它挤掉"
    }
}

Test-Case 'peak-shaving 指纹:gateway 在口令指纹之后多一条、login 一条;staging 与 prod 同输入同指纹;off 时消失' {
    # ConfigMap 只在进程启动时读:只改 ConfigMap 时 Pod 模板不变、apply 是 no-op。指纹进模板,开关 / 阈值 / 排队密钥一变就滚动。
    $stagingGateway = Get-PeakShavingAnnotation -Output $peakStagingRun.Output -Workload 'gateway' -Label 'staging'
    $prodGateway = Get-PeakShavingAnnotation -Output $prodRun.Output -Workload 'gateway' -Label 'prod'
    $stagingLogin = Get-PeakShavingAnnotation -Output $peakStagingRun.Output -Workload 'login' -Label 'staging'
    $prodLogin = Get-PeakShavingAnnotation -Output $prodRun.Output -Workload 'login' -Label 'prod'
    # 指纹只由「写进 ConfigMap 的那段配置 + 相关密钥」决定:两次独立子进程、不同档位、同一输入,指纹必须相同,否则每次重跑都无故滚动。
    Assert-Equal -Expected $prodGateway.Peak -Actual $stagingGateway.Peak -Because 'gateway:staging 与 prod 的 rate-limit 段与 Redis 口令相同,指纹必须相同'
    Assert-Equal -Expected $prodLogin.Peak -Actual $stagingLogin.Peak -Because 'login:staging 与 prod 的 Queue 段与排队密钥相同,指纹必须相同'
    Assert-Equal -Expected 0 -Actual $peakProdOffRun.ExitCode -Because "下面断「off 时没有指纹」的前提是这次 DryRun 成功。输出: $($peakProdOffRun.Output)"
    foreach ($case in @(@{ Label = '-GatewayRateLimit off -LoginQueue off(prod)'; Output = $peakProdOffRun.Output }, @{ Label = 'dev 缺省'; Output = $devOut })) {
        Assert-NotMatch -Text $case.Output -Pattern 'peak-shaving-hash' -Because "$($case.Label):关 = 不写指纹注解(开 → 关时模板变化,同样自动滚动)"
    }
    # 排队密钥是 login 指纹的输入:dev 档未注入(空串)与 prod 注入的值不同,Queue 段相同,指纹必须不同(轮换排队密钥自动滚动 login)。
    $devOnLogin = Get-PeakShavingAnnotation -Output $peakDevOnRun.Output -Workload 'login' -Label 'dev -LoginQueue on'
    Assert-True -Condition ($devOnLogin.Peak -ne $prodLogin.Peak) -Because "排队密钥不同,login 指纹必须不同(两者都是 $($prodLogin.Peak))"
}

Test-Case '-GatewayRateLimit off -LoginQueue off(prod):两段不写、不要求排队密钥、打出关闭警告' {
    Assert-Equal -Expected 0 -Actual $peakProdOffRun.ExitCode -Because "止血 / 基线压测路径:排队密钥置空也必须能部署。输出: $($peakProdOffRun.Output)"
    $gatewayFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $peakProdOffRun.Output -Name 'java-svc-gateway-config')
    Assert-True -Condition (-not $gatewayFlat.Scalars.Contains('data.application.yaml.gate.rate-limit.enabled')) -Because 'off 不写 gate.rate-limit.enabled,落回 jar 内的 false'
    $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $peakProdOffRun.Output -Name 'go-svc-login-config')
    $queueKeys = @(@(Get-FlatSubtreeKeys -Flat $loginFlat -Path 'data.login.yaml.Queue') + @(Get-FlatSubtreeKeys -Flat $loginFlat -Path 'data.login.yaml.Secrets.QueueToken'))
    Assert-True -Condition ($queueKeys.Count -eq 0) -Because "off 不写 Queue / Secrets.QueueToken(实际出现:$($queueKeys -join ', '))"
    Assert-Match -Text $peakProdOffRun.Output -Pattern 'PeakShaving: gateway_rate_limit=off\(-GatewayRateLimit off\) login_queue=off\(-LoginQueue off\) profile=prod ' -Because '计划行要打出显式 off 的解析结果'
    Assert-Match -Text $peakProdOffRun.Output -Pattern '\[PeakShaving\]GatewayRateLimit=off' -Because '非 dev 档关限流必须告警(只用于止血 / 基线压测,参数不粘滞)'
    Assert-Match -Text $peakProdOffRun.Output -Pattern '\[PeakShaving\]LoginQueue=off' -Because '非 dev 档关排队必须告警'
    # $peakProdOffRun 不带合入门槛的测试接缝(门槛未清):显式 off 的结论与门槛无关,仍打上面那条,不打门槛警告。
    Assert-NotMatch -Text $peakProdOffRun.Output -Pattern '\[PeakShaving\]LoginQueueGate=closed' -Because '显式 -LoginQueue off 时维持原来的 LoginQueue=off 警告,不改打合入门槛警告'
    Assert-NotMatch -Text $peakProdOffRun.Output -Pattern '\[PeakShaving\]GatewayTrustedProxies=unset' -Because '限流关着时不打「未配置可信代理」警告'
    # 对照:prod 缺省(两项都开、未给 -GatewayTrustedProxies)不打两条关闭警告,只打可信代理那一条 —— 否则区分不出「关了才告警」。
    Assert-NotMatch -Text $prodRun.Output -Pattern '\[PeakShaving\](GatewayRateLimit|LoginQueue)=off' -Because '对照:两项都开时不该打关闭警告'
    Assert-NotMatch -Text $prodRun.Output -Pattern '\[PeakShaving\]LoginQueueGate=closed' -Because '对照:$prodRun 带测试接缝(视为门槛已清),排队按档位开,不该打合入门槛警告'
    Assert-Match -Text $prodRun.Output -Pattern '\[PeakShaving\]GatewayTrustedProxies=unset' -Because '对照:限流开、部署 gateway 却没给 -GatewayTrustedProxies 时要告警(IP 桶按 socket 对端分)'
}

Test-Case '负向:prod 缺 MMORPG_LOGIN_QUEUE_TOKEN_SECRET 在任何写操作之前拒绝并点名变量' {
    $env2 = @{} + $ProdEnv
    $env2['MMORPG_LOGIN_QUEUE_TOKEN_SECRET'] = ''
    $run = Invoke-DeployDryRun -Arguments $ProdArgs -Env $env2
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "prod 开排队、部署 login 却缺排队密钥必须阻断:login 生产模式拒启,部署出来只会 CrashLoopBackOff。输出: $($run.Output)"
    # 其余生产密钥都已给齐($ProdEnv 基线能出产物),拒绝只能来自这一把;断 Resolve-InjectedSecret「未设置」分支的原文开头。
    Assert-Match -Text $run.Output -Pattern 'ReleaseProfile=prod 要求从环境变量 MMORPG_LOGIN_QUEUE_TOKEN_SECRET' -Because '错误必须点名缺哪个环境变量,且说清是注入缺失而不是别的失败'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '密钥解析(Initialize-InjectedSecrets)先于任何写操作,不留半截部署'
    # 计划行排在密钥解析之前(k8s_deploy.ps1 写路径入口):被拒时输出里已有 login_queue=on 可对照。
    Assert-Match -Text $run.Output -Pattern 'login_queue=on\(-LoginQueue auto\)' -Because '被拒时操作者要能看出排队是按档位(auto)开的'
}

Test-Case '负向:排队密钥与 GATE / INTERNAL_AUTH 相同时拒绝,只点变量名不回显值' {
    foreach ($reused in @(
        @{ EnvName = 'MMORPG_GATE_TOKEN_SECRET'; SecretValue = $ProdEnv.MMORPG_GATE_TOKEN_SECRET }
        @{ EnvName = 'MMORPG_INTERNAL_AUTH_SECRET'; SecretValue = $ProdEnv.MMORPG_INTERNAL_AUTH_SECRET }
    )) {
        $env2 = @{} + $ProdEnv
        $env2['MMORPG_LOGIN_QUEUE_TOKEN_SECRET'] = $reused.SecretValue
        $run = Invoke-DeployDryRun -Arguments $ProdArgs -Env $env2
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "排队密钥复用 $($reused.EnvName) 必须阻断(login 拒绝跨用途复用主密钥)。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern "必须与 $($reused.EnvName) 不同" -Because "错误必须点名与哪一把重复($($reused.EnvName))"
        Assert-NotMatch -Text $run.Output -Pattern ([regex]::Escape($reused.SecretValue)) -Because "$($reused.EnvName):拒绝路径只点变量名,不回显密钥值"
        Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because "$($reused.EnvName):复用检查先于任何写操作"
    }
}

Test-Case 'dev -GatewayRateLimit on -LoginQueue on:两段写出,不写 Secrets.QueueToken,不要求注入,Mode 仍为 dev' {
    Assert-Equal -Expected 0 -Actual $peakDevOnRun.ExitCode -Because "dev 档联调:排队密钥未注入也不阻断。输出: $($peakDevOnRun.Output)"
    $gatewayFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $peakDevOnRun.Output -Name 'java-svc-gateway-config')
    Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $gatewayFlat -KeyPath 'data.application.yaml.gate.rate-limit.enabled') -Because '显式 on 在任何档位都写限流段'
    $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $peakDevOnRun.Output -Name 'go-svc-login-config')
    Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $loginFlat -KeyPath 'data.login.yaml.Queue.Enabled') -Because '显式 on 在任何档位都写排队段'
    $queueTokenKeys = @(Get-FlatSubtreeKeys -Flat $loginFlat -Path 'data.login.yaml.Secrets.QueueToken')
    Assert-True -Condition ($queueTokenKeys.Count -eq 0) -Because "dev 档排队密钥为空时不写 Secrets.QueueToken(login 宽松档回落 GateToken 密钥并告警)(实际出现:$($queueTokenKeys -join ', '))"
    # 回落只在宽松档(Mode dev / test)成立:dev 档照写 Mode,不写 QueueToken 的 login 才起得来。
    Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Mode') -Actual (Get-FlatValue -Flat $loginFlat -KeyPath 'data.login.yaml.Mode') -Because 'dev 档 login ConfigMap 的 Mode 必须仍与 go/login/etc/login.yaml 一致'
    Assert-Match -Text $peakDevOnRun.Output -Pattern 'PeakShaving: gateway_rate_limit=on\(-GatewayRateLimit on\) login_queue=on\(-LoginQueue on\) profile=dev ' -Because '计划行要打出显式 on 的解析结果'
}

Test-Case 'all-up 多 zone(prod):每个 zone 的 login 锁键与 ZoneCapacityOverride 只指向本 zone,两把锁键互不相同,login 削峰指纹也互不相同' {
    Assert-True -Condition ($PeakAllUpArgs -contains '-DryRun') -Because '夹具前提:all-up 必须是 DryRun(不带它会对本机当前 kubectl context 真执行 apply)'
    $expectedZoneIds = @((Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/zones.sample.json') -Raw | ConvertFrom-Json).zones | ForEach-Object { [string]$_.zoneId } | Sort-Object)
    Assert-True -Condition ($expectedZoneIds.Count -ge 2) -Because "夹具前提:deploy/k8s/zones.sample.json 至少有两个 zone(实际 $($expectedZoneIds.Count) 个)"
    Assert-Equal -Expected 0 -Actual $peakAllUpRun.ExitCode -Because "多 zone 的 prod all-up DryRun 应当成功。输出: $($peakAllUpRun.Output)"
    $lockKeyBase = Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Queue.DispatcherLockKey'
    $blocks = @(Get-ManifestBlocks -Output $peakAllUpRun.Output | Where-Object { $_ -match '(?m)^kind: ConfigMap\s*$' -and $_ -match '(?m)^  name:\s*go-svc-login-config\s*$' })
    Assert-Equal -Expected $expectedZoneIds.Count -Actual $blocks.Count -Because '每个 zone 各有一份 go-svc-login-config'
    $zoneIds = @()
    $lockKeys = @()
    foreach ($block in $blocks) {
        $flat = ConvertTo-FlatManifest -Block $block
        $zoneId = Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Node.ZoneId'
        $lockKey = Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Queue.DispatcherLockKey'
        Assert-True -Condition ($lockKey -ceq "${lockKeyBase}:zone:$zoneId") -Because "zone $zoneId 的锁键必须带本 zone(期望 '${lockKeyBase}:zone:$zoneId',实际 '$lockKey')"
        $capacityKeys = @(Get-FlatSubtreeKeys -Flat $flat -Path 'data.login.yaml.Queue.ZoneCapacityOverride' | Where-Object { $_ -cne 'data.login.yaml.Queue.ZoneCapacityOverride' })
        Assert-Equal -Expected "data.login.yaml.Queue.ZoneCapacityOverride.$zoneId" -Actual ($capacityKeys -join ',') -Because "zone $zoneId 的 ZoneCapacityOverride 只能有本 zone 一个键"
        $zoneIds += $zoneId
        $lockKeys += $lockKey
    }
    Assert-Equal -Expected ($expectedZoneIds -join ',') -Actual (@($zoneIds | Sort-Object) -join ',') -Because 'login ConfigMap 的 Node.ZoneId 与 zones.sample.json 的 zone 一一对应'
    Assert-Equal -Expected $expectedZoneIds.Count -Actual (@($lockKeys | Sort-Object -Unique).Count) -Because '各 zone 的锁键必须互不相同(同一把锁 = 全集群只有一个 leader,其它 zone 的队列不出队)'

    # login 指纹的输入里有配置文本(Queue 段),不只有排队密钥:各 zone 用同一把密钥,Queue 段只差 zone(锁键与 ZoneCapacityOverride),
    # 指纹必须各不相同。指纹若退化成只含密钥,改排队配置后 login 不会滚动,而用例 8 的「dev on 对 prod」(两边只差密钥)照样绿。
    # 不用 Get-PeakShavingAnnotation:它经 Select-ManifestByName 只取第一个同名块,拿不到第二个 zone 的 login。
    $loginDeployments = @(Get-ManifestBlocks -Output $peakAllUpRun.Output | Where-Object {
        $_ -match '(?m)^\s*kind:\s*Deployment\s*$' -and $_ -match '(?m)^\s*name:\s*login\s*$' -and $_.Contains('mmorpg.io/peak-shaving-hash')
    })
    Assert-Equal -Expected $expectedZoneIds.Count -Actual $loginDeployments.Count -Because '每个 zone 各有一份带削峰指纹的 login Deployment'
    $loginPeaks = @()
    foreach ($block in $loginDeployments) {
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, 'mmorpg\.io/peak-shaving-hash').Count) -Because 'login manifest 里削峰指纹注解应恰好 1 处'
        # Get-ManifestBlocks 已把块内换行统一成 LF,与 Get-PeakShavingAnnotation 同一个锚定写法。
        $peakMatch = [regex]::Match($block, 'annotations:[ ]*\n[ ]+mmorpg\.io/peak-shaving-hash: "(?<peak>[0-9a-f]{12})"')
        Assert-True -Condition $peakMatch.Success -Because 'login 的削峰指纹必须在 Pod 模板 annotations 的下一行,值为加双引号的 12 位小写十六进制'
        $loginPeaks += $peakMatch.Groups['peak'].Value
    }
    Assert-Equal -Expected $expectedZoneIds.Count -Actual (@($loginPeaks | Sort-Object -Unique).Count) -Because "同一把排队密钥下各 zone 的 Queue 段不同,login 指纹必须互不相同(实际:$($loginPeaks -join ', '))"
}

Test-Case 'staging 与 prod 的削峰产物逐字相同(档位只决定开关,不决定阈值)' {
    foreach ($section in @(
        @{ ConfigMap = 'java-svc-gateway-config'; SectionKey = 'rate-limit' }
        @{ ConfigMap = 'go-svc-login-config'; SectionKey = 'Queue' }
    )) {
        $stagingText = Get-ManifestSectionText -Block (Select-ManifestByName -Output $peakStagingRun.Output -Name $section.ConfigMap) -SectionKey $section.SectionKey -Label "staging $($section.ConfigMap)"
        $prodText = Get-ManifestSectionText -Block (Select-ManifestByName -Output $prodRun.Output -Name $section.ConfigMap) -SectionKey $section.SectionKey -Label "prod $($section.ConfigMap)"
        Assert-True -Condition ($stagingText -ceq $prodText) -Because "$($section.ConfigMap) 的 $($section.SectionKey): 段 staging 与 prod 必须逐字相同。staging:`n$stagingText`nprod:`n$prodText"
    }
}

Test-Case 'G4b-2 前置:G1b 与 G4b-G 已合入(DispatcherLockKey 是前缀,锁键按 zone 派生;在途占位是新键 ZSET)' {
    # 生成器给每个 zone 显式写 <基名>:zone:<id>,前提是 login 把配置值当前缀、只出队本 zone(G1b)。login 若改成「一个 leader
    # 覆盖全部 zone」,显式分 zone 键就变成 N 个 leader 同时出队所有 zone、超发准入 —— 这条只检查 G1b 的代码形状还在。
    # 2026-10-15 更正:此前这里与下面都写「钉成机械门禁」,不准确。本例只看 G1b / G4b-G 的代码形状,证明不了合入门槛
    # (G1b 多 zone 用例、G4b-G 由 Codex 跑绿,spec_G4b-G.md §8.4 真实 Redis 冒烟记进 PROGRESS);真正挡住打开排队的是
    # k8s_deploy.ps1 的 $script:LoginQueueMergeGateCleared(用例 19–22)。
    $dispatcherGo = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'go/login/internal/logic/pkg/loginqueue/dispatcher.go') -Raw
    Assert-True -Condition ($dispatcherGo -cmatch 'func ZoneDispatcherLockKey\(') -Because 'loginqueue.ZoneDispatcherLockKey 必须存在(G1b:锁键 = 配置前缀 + 本 zone)'
    Assert-True -Condition ($dispatcherGo.Contains('"%s:zone:%d"')) -Because '锁键派生格式必须是 <前缀>:zone:<id>(生成器显式写的键与排查时要查的键都按它推)'
    $svcGo = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'go/login/internal/svc/servicecontext.go') -Raw
    Assert-True -Condition (-not $svcGo.Contains('activeZones := func')) -Because 'G1b 之前「一个 leader 遍历 activeZones 出队」的实现必须已删除'
    # G4b-G:在途占位改成逐个到期的 admitted:zset:zone:{z}。G4b-2 一打开排队,G4b-G 之前的 SET 版本就把 Q1(快速通道占位被重复
    # 扣减、放一批停最长 60s)与 Q2(取走即撤占位、开区超发数倍容量)带上线 —— 下面两条只检查 G4b-G 的代码形状还在
    # (docs/handoff/launch-readiness-20261013/spec_G4b-G.md §5.5;不是对合入门槛的拦截,见上面 2026-10-15 更正)。
    $queueGo = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'go/login/internal/logic/pkg/loginqueue/queue.go') -Raw
    Assert-True -Condition ($queueGo.Contains('admitted:zset:zone:%d')) -Because '在途占位键必须是 admitted:zset:zone:<id>(G4b-G:ZSET,成员按分数逐个到期)'
    Assert-True -Condition (-not $queueGo.Contains('"admitted:zone:%d"')) -Because 'G4b-G 之前的 SET 键 "admitted:zone:%d" 必须已不再使用(新版本不读不写旧键)'
}

Test-Case '常量表自检:改坏 PeakShavingLaunchTier 时在任何写操作之前 throw' {
    # release_preflight.ps1 的「rps ≥ 1、burst ≥ rps」只查 jar 内 application.yaml;K8s 上真正生效的是 ConfigMap 里的值,
    # 同一条规则由 Get-GatewayRateLimitSettings 再守一次。每次取一份新的合法表,下面逐项改坏。
    $newTier = {
        [ordered]@{ ZoneDefaultRps = 2000; ZoneDefaultBurst = 5000; IpRps = 5; IpBurst = 20; AccountCooldownMs = 5000; LoginQueueZoneCapacity = 5000 }
    }
    try {
        $script:GatewayRateLimitOn = $true
        foreach ($bad in @(
            @{ Why = 'zone 突发小于速率'; Field = 'ZoneDefaultBurst'; NewValue = 1000; Expected = 'ZoneDefaultRps=2000 / ZoneDefaultBurst=1000 不满足 rps ≥ 1 且 burst ≥ rps' }
            @{ Why = 'IP 速率为 0'; Field = 'IpRps'; NewValue = 0; Expected = 'IpRps=0 / IpBurst=20 不满足 rps ≥ 1 且 burst ≥ rps' }
            @{ Why = '账号冷却为负'; Field = 'AccountCooldownMs'; NewValue = -1; Expected = 'AccountCooldownMs=-1 不能为负' }
        )) {
            $tier = & $newTier
            $tier[$bad.Field] = $bad.NewValue
            $script:PeakShavingLaunchTier = $tier
            $message = ''
            try { Get-GatewayRateLimitSettings | Out-Null } catch { $message = $_.Exception.Message }
            Assert-Match -Text $message -Pattern ([regex]::Escape($bad.Expected)) -Because "$($bad.Why) 必须 throw 并点名常量与值,不能把焊死(rps 0 = 全量排队)或自相矛盾的阈值写进 ConfigMap"
        }

        $script:PeakShavingLaunchTier = & $newTier
        $settings = Get-GatewayRateLimitSettings
        Assert-Equal -Expected 'enabled,zone-default-rps,zone-default-burst,ip-rps,ip-burst,account-cooldown-ms' -Actual (@($settings.Keys) -join ',') -Because '合法常量表不 throw,按固定顺序返回 6 个 Spring 属性名'
        $script:GatewayRateLimitOn = $false
        Assert-True -Condition ($null -eq (Get-GatewayRateLimitSettings)) -Because '限流关时返回 $null(什么都不写,落回 jar 内的 enabled: false)'

        # 排队容量是常量表的另一项(Go 侧 uint32),由 New-LoginQueueYaml 守。它还要读 go/login/etc/login.yaml 的 Queue.* 镜像键:
        # Get-AuthoritativeScalar 按 $RepoRoot 定位仓库(在 k8s_deploy.ps1 里是脚本级变量,这里按动态作用域给)。
        $RepoRoot = Get-RepoRoot
        $script:LoginQueueOn = $true
        foreach ($badCapacity in @(0, 4294967296)) {
            $tier = & $newTier
            $tier['LoginQueueZoneCapacity'] = $badCapacity
            $script:PeakShavingLaunchTier = $tier
            $message = ''
            try { New-LoginQueueYaml -CurrentZoneId 101 | Out-Null } catch { $message = $_.Exception.Message }
            Assert-Match -Text $message -Pattern ([regex]::Escape("LoginQueueZoneCapacity=$badCapacity 不在 1..4294967295")) -Because "排队容量 $badCapacity 越界必须 throw 并点名常量与值(0 = 本 zone 永远放不出人;超过 uint32 在 Go 侧解析失败)"
        }
        $script:PeakShavingLaunchTier = & $newTier
        $queueYaml = New-LoginQueueYaml -CurrentZoneId 101
        Assert-Match -Text $queueYaml -Pattern '(?m)^    "101": 5000\s*$' -Because '合法常量表不 throw,ZoneCapacityOverride 只写本 zone 的开服档容量'
        $script:LoginQueueOn = $false
        Assert-Equal -Expected '' -Actual (New-LoginQueueYaml -CurrentZoneId 101) -Because '排队关时返回空串(不写 Queue 段,落回 login.yaml 的 Enabled: false)'
    }
    finally {
        Remove-Variable -Name GatewayRateLimitOn, LoginQueueOn, PeakShavingLaunchTier -Scope Script -ErrorAction SilentlyContinue
    }

    # 「在任何写操作之前」:写路径入口的 Show-PeakShavingPlan(先于 Initialize-InjectedSecrets 与任何 kubectl 写操作)在本次部署
    # gateway 且限流开时先求一次 Get-GatewayRateLimitSettings,部署 login 且排队开时先求一次 New-LoginQueueYaml(容量越界、login.yaml
    # 缺 Queue 键都在这里 throw)。否则自检要等到生成 gateway / login ConfigMap 才跑,那时同一 zone 的 node ConfigMap / gate / scene
    # 已经 apply 过了,留下半截部署(spec_G4b §5 表头「部署期,都在任何 kubectl 写操作之前」)。
    $planFn = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Show-PeakShavingPlan' }, $true)
    Assert-True -Condition ($null -ne $planFn) -Because 'k8s_deploy.ps1 应当有 Show-PeakShavingPlan(写路径入口的削峰计划)'
    $selfCheckCall = $planFn.Body.Find({ param($a) $a -is [System.Management.Automation.Language.CommandAst] -and $a.GetCommandName() -eq 'Get-GatewayRateLimitSettings' }, $true)
    Assert-True -Condition ($null -ne $selfCheckCall) -Because 'Show-PeakShavingPlan 必须调用 Get-GatewayRateLimitSettings,把常量表自检提前到写路径入口'
    $queueSelfCheckCall = $planFn.Body.Find({ param($a) $a -is [System.Management.Automation.Language.CommandAst] -and $a.GetCommandName() -eq 'New-LoginQueueYaml' }, $true)
    Assert-True -Condition ($null -ne $queueSelfCheckCall) -Because 'Show-PeakShavingPlan 必须调用 New-LoginQueueYaml,把排队容量越界与 login.yaml 缺 Queue 键的检查提前到写路径入口'
    $planAt = $prodRun.Output.IndexOf('PeakShaving: ')
    $firstKubectlAt = $prodRun.Output.IndexOf('[dry-run] kubectl')
    Assert-True -Condition ($planAt -ge 0 -and ($firstKubectlAt -lt 0 -or $planAt -lt $firstKubectlAt)) -Because '削峰计划(含常量表自检)必须先于任何 kubectl 写操作打出'
}

Test-Case 'gateway 削峰指纹随 Redis 口令变化(轮换口令自动滚动 gateway)' {
    # Lettuce 只在启动时读 spring.data.redis.password:轮换 MMORPG_REDIS_PASSWORD 而 gateway 不滚动,旧口令被拒后限流一律 fail-open。
    # 所以口令是 gateway 削峰指纹的输入之一;管理面口令指纹只由管理面口令决定,不该跟着变。
    Assert-Equal -Expected 0 -Actual $peakGwRedisRotRun.ExitCode -Because "换 Redis 口令的 DryRun 应当成功。输出: $($peakGwRedisRotRun.Output)"
    $rotated = Get-PeakShavingAnnotation -Output $peakGwRedisRotRun.Output -Workload 'gateway' -Label '换 Redis 口令'
    $baseline = Get-PeakShavingAnnotation -Output $prodRun.Output -Workload 'gateway' -Label 'prod 基线'
    Assert-True -Condition ($rotated.Peak -ne $baseline.Peak) -Because "Redis 口令变了,gateway 削峰指纹必须变(两者都是 $($baseline.Peak))"
    Assert-Equal -Expected $baseline.AdminKey -Actual $rotated.AdminKey -Because '管理面口令没变,口令指纹必须不变(两条注解各管各的输入)'
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $peakGwRedisRotRun.Output -Name 'java-svc-gateway-config')
    Assert-True -Condition ((Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.spring.data.redis.password') -ceq $PeakRotatedRedisPassword) -Because 'ConfigMap 里的 Redis 口令必须是换过的新值(与指纹同一份输入)'
}

Test-Case 'gateway 削峰指纹随 rate-limit 段文本变化(只改 -GatewayTrustedProxies,密钥不变,也要自动滚动 gateway)' {
    # 指纹输入 = 整段 rate-limit 文本 + Redis 口令。上面几例只验证了「随口令变」「同输入同指纹」「关时消失」:指纹输入若退化成只含口令,
    # 它们照样全绿,而改阈值、改可信代理网段都不会再滚动 gateway(ConfigMap 只在启动时读)。这里密钥与 $prodRun 完全相同,只多一段代理网段。
    Assert-Equal -Expected 0 -Actual $peakGwProxyRun.ExitCode -Because "只加 -GatewayTrustedProxies 的 prod DryRun 应当成功。输出: $($peakGwProxyRun.Output)"
    $gatewayCm = Select-ManifestByName -Output $peakGwProxyRun.Output -Name 'java-svc-gateway-config'
    Assert-Match -Text $gatewayCm -Pattern ('(?m)^[ ]+trusted-proxies:[ ]*\r?\n[ ]+- "' + [regex]::Escape($PeakTrustedProxies) + '"') -Because '夹具前提:rate-limit 段里要真的多出这段可信代理网段,否则下面「指纹变了」无从谈起'
    $withProxies = Get-PeakShavingAnnotation -Output $peakGwProxyRun.Output -Workload 'gateway' -Label '加可信代理'
    $baseline = Get-PeakShavingAnnotation -Output $prodRun.Output -Workload 'gateway' -Label 'prod 基线'
    Assert-True -Condition ($withProxies.Peak -ne $baseline.Peak) -Because "rate-limit 段文本变了(多了 trusted-proxies),gateway 削峰指纹必须变(两者都是 $($baseline.Peak))"
    Assert-Equal -Expected $baseline.AdminKey -Actual $withProxies.AdminKey -Because '管理面口令没变,口令指纹必须不变(两条注解各管各的输入)'
}

# 登录排队合入门槛(用例 19–22,2026-10-15;docs/handoff/launch-readiness-20261013/spec_G4b.md §7「合入门槛」)。
# G4b-2 的 login 段已先于门槛入库,k8s_deploy.ps1 用常量 $script:LoginQueueMergeGateCleared 挡住打开排队;未清时 Resolve-LoginQueueSwitch
# 把 auto 一律解析为关、把 on 拒在任何写操作之前。测试接缝 MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED=1 只在 -DryRun 下视为门槛已清。
# 「不带 -DryRun + 接缝变量 → 拒绝」只在用例 21 里对纯函数断言:不带 -DryRun 起部署子进程,万一没拒绝就会对本机当前 kubectl context
# 真执行,所以本文件不起这种子进程。用例 19 / 20 断的是门槛未清时的产物,负责人把常量改为 $true 的同一提交里要随之改写这两例
# (届时 staging auto = 开、-LoginQueue on 放行);刻意不写「常量必须为 $false」的断言,那会把放开门槛本身挡在测试上。
# 用例 21 按 AST 抽出下面两个函数直接调用(与上文 deadline 守卫、常量表自检同一做法)。
$loginQueueGateFunctionNames = @('Resolve-PeakShavingSwitch', 'Resolve-LoginQueueSwitch')
foreach ($loginQueueGateFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($loginQueueGateFunctionNames -contains $loginQueueGateFnAst.Name) {
        Set-Item -Path "Function:script:$($loginQueueGateFnAst.Name)" -Value $loginQueueGateFnAst.Body.GetScriptBlock()
    }
}
foreach ($loginQueueGateFnName in $loginQueueGateFunctionNames) {
    if (-not (Test-Path "Function:$loginQueueGateFnName")) { throw "k8s_deploy.ps1 缺少函数 $loginQueueGateFnName(登录排队的合入门槛被删了?后续断言无意义)" }
}

Test-Case '合入门槛未清(不带测试接缝):staging zone-up 的 -LoginQueue auto 解析为关,打 LoginQueueGate=closed,login 不写 Queue 段、不要求排队密钥,限流照开' {
    # 用例 19。门槛清掉后本例要随常量一起改(见上方说明)。
    # 显式置空接缝变量:不受跑测机器上残留的同名环境变量影响。排队密钥也置空:门槛未清时排队关,不该要求它。
    $env2 = @{} + $ProdEnv
    $env2['MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED'] = ''
    $env2['MMORPG_LOGIN_QUEUE_TOKEN_SECRET'] = ''
    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-ReleaseProfile', 'staging', '-SkipPreflight', '-NodeImage', $PeakNodeImage)) -Env $env2
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "门槛未清时 staging 排队关,缺 MMORPG_LOGIN_QUEUE_TOKEN_SECRET 也必须能部署。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'PeakShaving: gateway_rate_limit=on\(-GatewayRateLimit auto\) login_queue=off\(-LoginQueue auto\) profile=staging ' -Because '计划行:限流仍按档位开,排队因门槛未清解析为关'
    Assert-Match -Text $run.Output -Pattern '\[PeakShaving\]LoginQueueGate=closed' -Because '门槛把本该开的排队关掉时,要用专门的警告点明原因与放开方法'
    Assert-NotMatch -Text $run.Output -Pattern '\[PeakShaving\]LoginQueue=off' -Because '门槛未清时不打 LoginQueue=off:它说的「不粘滞、下次部署按档位恢复」在门槛未清时不成立'
    $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $run.Output -Name 'go-svc-login-config')
    $queueKeys = @(@(Get-FlatSubtreeKeys -Flat $loginFlat -Path 'data.login.yaml.Queue') + @(Get-FlatSubtreeKeys -Flat $loginFlat -Path 'data.login.yaml.Secrets.QueueToken'))
    Assert-True -Condition ($queueKeys.Count -eq 0) -Because "门槛未清时 login ConfigMap 不得写 Queue / Secrets.QueueToken(实际出现:$($queueKeys -join ', '))"
    # 按「kind: Deployment + metadata 里的名字」挑 login 的 Deployment(同上文命令 topic 用例的挑法)。
    $loginDeployment = @(Get-ManifestBlocks -Output $run.Output | Where-Object {
        $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch '(?m)^  name: login\s*$'
    }) | Select-Object -First 1
    Assert-True -Condition ($null -ne $loginDeployment) -Because 'DryRun 输出里应当有 login 的 Deployment(本例断它不带削峰指纹的前提)'
    Assert-NotMatch -Text $loginDeployment -Pattern 'peak-shaving-hash' -Because '排队关 = login 的 Pod 模板不带削峰指纹'
    $gatewayFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $run.Output -Name 'java-svc-gateway-config')
    Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $gatewayFlat -KeyPath 'data.application.yaml.gate.rate-limit.enabled') -Because '合入门槛只管登录排队,gateway 限流仍按档位开'
    $null = Get-PeakShavingAnnotation -Output $run.Output -Workload 'gateway' -Label '门槛未清 staging'
}

Test-Case '合入门槛未清(不带测试接缝):prod -LoginQueue on 在参数解析后、任何 kubectl 之前拒绝并点名门槛' {
    # 用例 20。门槛清掉后本例要随常量一起改(见上方说明)。其余生产密钥都给齐:门槛已清时这次 DryRun 会成功,拒绝只能来自门槛。
    $env2 = @{} + $ProdEnv
    $env2['MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED'] = ''
    $run = Invoke-DeployDryRun -Arguments ($ProdArgs + @('-LoginQueue', 'on')) -Env $env2
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "门槛未清时 -LoginQueue on 必须拒绝。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern '-LoginQueue on 被拒绝' -Because '错误要说清是哪个参数值被拒'
    Assert-Match -Text $run.Output -Pattern '合入门槛未清' -Because '错误要说清拒绝原因是合入门槛'
    Assert-Match -Text $run.Output -Pattern 'LoginQueueMergeGateCleared' -Because '错误要点名放开门槛要改的常量'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl' -Because '门槛在参数解析后就拒绝,早于任何 kubectl(含只读)'
    # Release: 行与削峰计划行都在写路径入口才打,拒绝发生在它们之前 = 拒绝点在参数解析后(Resolve-LoginQueueSwitch 的调用处)。
    Assert-NotMatch -Text $run.Output -Pattern '(?m)^Release: profile=' -Because '拒绝必须早于 Release: 行(参数解析后、加载库之前)'
    Assert-NotMatch -Text $run.Output -Pattern '(?m)^PeakShaving: ' -Because '拒绝必须早于削峰计划行'
}

Test-Case '合入门槛纯函数:Resolve-LoginQueueSwitch 全组合(未清时 auto / off 恒关、on 拒绝;已清时与 Resolve-PeakShavingSwitch 逐格一致;测试接缝只在 DryRun 生效)' {
    # 用例 21。不依赖常量当前的值:常量以 -MergeGateCleared 参数传入。
    $profiles = @('dev', 'staging', 'prod')
    # 对照表本身先钉住:Resolve-PeakShavingSwitch 被改坏时,下面「与它一致」就失去意义。
    foreach ($profileName in $profiles) {
        Assert-Equal -Expected ($profileName -ne 'dev') -Actual (Resolve-PeakShavingSwitch -Value 'auto' -ReleaseProfileName $profileName) -Because "Resolve-PeakShavingSwitch:auto 在 $profileName 档应为 $($profileName -ne 'dev')(dev 关,其余开)"
        Assert-Equal -Expected $true -Actual (Resolve-PeakShavingSwitch -Value 'on' -ReleaseProfileName $profileName) -Because "Resolve-PeakShavingSwitch:on 在 $profileName 档应为开"
        Assert-Equal -Expected $false -Actual (Resolve-PeakShavingSwitch -Value 'off' -ReleaseProfileName $profileName) -Because "Resolve-PeakShavingSwitch:off 在 $profileName 档应为关"
    }

    # 门槛未清(常量 $false、不带接缝):DryRun 与否结论相同。'ON':ValidateSet 不分大小写,大写的 on 也必须被拒。
    foreach ($dryRunMode in @($false, $true)) {
        foreach ($profileName in $profiles) {
            foreach ($value in @('auto', 'off')) {
                $actual = Resolve-LoginQueueSwitch -Value $value -ReleaseProfileName $profileName -MergeGateCleared $false -DryRunMode $dryRunMode -TestOverride ''
                Assert-Equal -Expected $false -Actual $actual -Because "门槛未清时 -LoginQueue $value 在 $profileName 档必须解析为关(DryRun=$dryRunMode)"
            }
            foreach ($value in @('on', 'ON')) {
                $message = ''
                try { $null = Resolve-LoginQueueSwitch -Value $value -ReleaseProfileName $profileName -MergeGateCleared $false -DryRunMode $dryRunMode -TestOverride '' }
                catch { $message = $_.Exception.Message }
                Assert-Match -Text $message -Pattern '合入门槛未清' -Because "门槛未清时 -LoginQueue $value 在 $profileName 档必须 throw(DryRun=$dryRunMode)"
                Assert-Match -Text $message -Pattern 'LoginQueueMergeGateCleared' -Because '错误要点名放开门槛要改的常量'
                Assert-Match -Text $message -Pattern 'spec_G4b\.md §7' -Because '错误要给出三项门槛的出处'
                Assert-Match -Text $message -Pattern 'spec_G4b-G\.md §8\.4' -Because '错误要点名真实 Redis 冒烟那一项的出处'
            }
        }
    }

    # 门槛已清的三种来源,结论都与 Resolve-PeakShavingSwitch 逐格一致。
    foreach ($cleared in @(
        @{ Label = '常量 $true、非 DryRun'; MergeGateCleared = $true; DryRunMode = $false; TestOverride = '' }
        @{ Label = '常量 $true、DryRun'; MergeGateCleared = $true; DryRunMode = $true; TestOverride = '' }
        @{ Label = '常量 $false、DryRun + 接缝 1'; MergeGateCleared = $false; DryRunMode = $true; TestOverride = '1' }
    )) {
        foreach ($profileName in $profiles) {
            foreach ($value in @('auto', 'on', 'off', 'ON')) {
                $expected = Resolve-PeakShavingSwitch -Value $value -ReleaseProfileName $profileName
                $actual = Resolve-LoginQueueSwitch -Value $value -ReleaseProfileName $profileName -MergeGateCleared $cleared.MergeGateCleared -DryRunMode $cleared.DryRunMode -TestOverride $cleared.TestOverride
                Assert-Equal -Expected $expected -Actual $actual -Because "$($cleared.Label):-LoginQueue $value 在 $profileName 档必须与 Resolve-PeakShavingSwitch 一致"
            }
        }
    }

    # 接缝不带 -DryRun:只要设置了就 throw,常量为 $true 时也一样(fail-closed,它放不开真实部署)。
    foreach ($mergeGateCleared in @($false, $true)) {
        foreach ($override in @('1', '0', 'true')) {
            $message = ''
            try { $null = Resolve-LoginQueueSwitch -Value 'auto' -ReleaseProfileName 'prod' -MergeGateCleared $mergeGateCleared -DryRunMode $false -TestOverride $override }
            catch { $message = $_.Exception.Message }
            Assert-Match -Text $message -Pattern 'MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED' -Because "不带 -DryRun 时设置了接缝变量(值 '$override',常量 $mergeGateCleared)必须 throw 并点名变量"
            Assert-Match -Text $message -Pattern '只在 -DryRun 下生效' -Because '错误要说清接缝只在 -DryRun 下生效'
        }
    }
    # 接缝在 DryRun 下只接受 1,其它值不猜含义。
    foreach ($override in @('0', 'true', ' 1')) {
        $message = ''
        try { $null = Resolve-LoginQueueSwitch -Value 'auto' -ReleaseProfileName 'prod' -MergeGateCleared $false -DryRunMode $true -TestOverride $override }
        catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '只接受 1' -Because "DryRun 下接缝变量为 '$override' 必须 throw(只接受 1)"
    }
    # 未设置:子进程里没设的环境变量读出来是 $null,与空串同样视为未设置。
    Assert-Equal -Expected $false -Actual (Resolve-LoginQueueSwitch -Value 'auto' -ReleaseProfileName 'prod' -MergeGateCleared $false -DryRunMode $false -TestOverride $null) -Because '接缝变量未设置($null)且门槛未清时,prod auto 解析为关、不 throw'
}

Test-Case '合入门槛源码形状:$script:LoginQueueOn 只由 Resolve-LoginQueueSwitch 赋值,门槛常量只能是字面量,测试接缝只读一处且绑本脚本的 -DryRun' {
    # 用例 22。用例 21 证明了纯函数本身;这里证明脚本真的经它取值、门槛参数绑的是常量与 -DryRun,不留绕过的路。
    $assignmentsTo = {
        param([string]$Target)
        @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -ieq $Target }, $true))
    }
    $topLevel = @($deadlineDeployAst.EndBlock.Statements)

    # 外层再包 @():脚本块返回的数组经管道会被展开,只有一个元素时就不再是数组。
    $queueAssigns = @(& $assignmentsTo '$script:LoginQueueOn')
    Assert-Equal -Expected 1 -Actual $queueAssigns.Count -Because 'k8s_deploy.ps1 里 $script:LoginQueueOn 只能有一处赋值(多一处就可能绕过合入门槛)'
    $queueAssign = $queueAssigns[0]
    Assert-True -Condition ($topLevel -contains $queueAssign) -Because '$script:LoginQueueOn 必须在脚本顶层赋值(参数解析后算一次)'
    $calls = @($queueAssign.Right.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))
    Assert-True -Condition ($calls.Count -eq 1 -and $calls[0].GetCommandName() -eq 'Resolve-LoginQueueSwitch') -Because "`$script:LoginQueueOn 必须只由 Resolve-LoginQueueSwitch 算出(实际:$($queueAssign.Right.Extent.Text))"
    $callText = $calls[0].Extent.Text
    Assert-Match -Text $callText -Pattern '-MergeGateCleared[:\s]\s*\$script:LoginQueueMergeGateCleared(\s|$)' -Because '-MergeGateCleared 必须绑门槛常量 $script:LoginQueueMergeGateCleared'
    Assert-Match -Text $callText -Pattern '-DryRunMode[:\s]\s*\$DryRun\.IsPresent(\s|$)' -Because '-DryRunMode 必须绑本脚本的 -DryRun(测试接缝只在 DryRun 下生效的前提)'
    Assert-Match -Text $callText -Pattern '-TestOverride[:\s]\s*\$env:MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED(\s|$)' -Because '-TestOverride 必须读测试接缝变量本身'
    Assert-Match -Text $callText -Pattern '-Value[:\s]\s*\$LoginQueue(\s|$)' -Because '-Value 必须是 -LoginQueue 参数'

    # $DryRun 必须就是参数块里的 [switch]$DryRun,脚本里没有别处给它赋值。
    $dryRunParams = @($deadlineDeployAst.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq 'DryRun' })
    Assert-True -Condition ($dryRunParams.Count -eq 1 -and $dryRunParams[0].StaticType -eq [System.Management.Automation.SwitchParameter]) -Because 'k8s_deploy.ps1 的参数块里应当有 [switch]$DryRun'
    $dryRunAssigns = @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -match '(?i)\$(script:)?DryRun$' }, $true))
    Assert-Equal -Expected 0 -Actual $dryRunAssigns.Count -Because '脚本里不得给 $DryRun 赋值(否则接缝的生效条件就不再是调用方给的 -DryRun)'

    # 测试接缝变量只在上面那一处调用里读,也没有经 GetEnvironmentVariable / Env: 路径之类的字面量另读一次。
    $envReads = @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.VariableExpressionAst] -and $a.VariablePath.UserPath -ieq 'env:MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED' }, $true))
    Assert-Equal -Expected 1 -Actual $envReads.Count -Because '$env:MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED 只能读一处'
    Assert-True -Condition ($envReads[0].Extent.StartOffset -ge $calls[0].Extent.StartOffset -and $envReads[0].Extent.EndOffset -le $calls[0].Extent.EndOffset) -Because '接缝变量唯一的读取点必须在 Resolve-LoginQueueSwitch 的调用里'
    $nameLiterals = @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.StringConstantExpressionAst] -and $a.Value -imatch '^(env:)?MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED$' }, $true))
    Assert-Equal -Expected 0 -Actual $nameLiterals.Count -Because '不得以字符串字面量(GetEnvironmentVariable / Env: 路径)另读接缝变量'

    # 门槛常量:恰好一处、顶层、字面量 $true / $false,先于 $script:LoginQueueOn 的赋值。不断言它当前的值(放开门槛只改这一处)。
    $gateAssigns = @(& $assignmentsTo '$script:LoginQueueMergeGateCleared')
    Assert-Equal -Expected 1 -Actual $gateAssigns.Count -Because '$script:LoginQueueMergeGateCleared 只能有一处赋值'
    Assert-True -Condition ($topLevel -contains $gateAssigns[0]) -Because '门槛常量必须在脚本顶层赋值'
    Assert-Match -Text $gateAssigns[0].Right.Extent.Text -Pattern '^\$(true|false)$' -Because "门槛常量只能是字面量 `$true / `$false,不能改成读参数或环境变量(实际:$($gateAssigns[0].Right.Extent.Text))"
    Assert-True -Condition ($gateAssigns[0].Extent.StartOffset -lt $queueAssign.Extent.StartOffset) -Because '门槛常量必须先于 $script:LoginQueueOn 的赋值'

    # 早于任何写操作:排在顶层写路径门禁块 if ($Command -in @("zone-up", "all-up", "infra-up")) 之前(Show-PeakShavingPlan、密钥解析与 kubectl 都在它之后)。
    $writeGates = @($topLevel | Where-Object {
        $_ -is [System.Management.Automation.Language.IfStatementAst] -and
        $_.Clauses[0].Item1.Extent.Text -match '^\$Command\s+-in\s+@\(\s*"zone-up"\s*,\s*"all-up"\s*,\s*"infra-up"\s*\)$'
    })
    Assert-Equal -Expected 1 -Actual $writeGates.Count -Because 'k8s_deploy.ps1 顶层应当恰好有一个写路径门禁块'
    Assert-True -Condition ($queueAssign.Extent.StartOffset -lt $writeGates[0].Extent.StartOffset) -Because '$script:LoginQueueOn 的赋值(-LoginQueue on 的拒绝点)必须先于写路径门禁块'

    # 限流不受合入门槛影响:仍由 Resolve-PeakShavingSwitch 按档位算。
    $gatewayAssigns = @(& $assignmentsTo '$script:GatewayRateLimitOn')
    Assert-Equal -Expected 1 -Actual $gatewayAssigns.Count -Because '$script:GatewayRateLimitOn 只能有一处赋值'
    $gatewayCalls = @($gatewayAssigns[0].Right.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))
    Assert-True -Condition ($gatewayCalls.Count -eq 1 -and $gatewayCalls[0].GetCommandName() -eq 'Resolve-PeakShavingSwitch') -Because '$script:GatewayRateLimitOn 仍只由 Resolve-PeakShavingSwitch 算出(合入门槛只管登录排队)'
}

# ─────────────────────────────────────────────────────────────────
# 4. 负向用例 —— 断错误文本,不只断退出码
# ─────────────────────────────────────────────────────────────────

Test-Case "负向:prod 档位缺 MMORPG_GATE_TOKEN_SECRET 必须 fail-closed 并点名该变量" {
    $env2 = @{}
    foreach ($k in $ProdEnv.Keys) { $env2[$k] = $ProdEnv[$k] }
    $env2['MMORPG_GATE_TOKEN_SECRET'] = ''
    $run = Invoke-DeployDryRun -Arguments $ProdArgs -Env $env2

    Assert-True -Condition ($run.ExitCode -ne 0) -Because "缺密钥必须阻断"
    Assert-Match -Text $run.Output -Pattern 'MMORPG_GATE_TOKEN_SECRET' -Because "错误必须点名缺哪个环境变量,否则运维只能猜"
    Assert-Match -Text $run.Output -Pattern '要求从环境变量' -Because "错误文本要说清是注入缺失,不是别的失败"
}

Test-Case "负向:prod 档位环境变量仍是占位串时必须拒绝" {
    $env2 = @{}
    foreach ($k in $ProdEnv.Keys) { $env2[$k] = $ProdEnv[$k] }
    $env2['MMORPG_MYSQL_PASSWORD'] = 'change-me-in-production'
    $run = Invoke-DeployDryRun -Arguments $ProdArgs -Env $env2

    Assert-True -Condition ($run.ExitCode -ne 0) -Because "占位串必须阻断"
    Assert-Match -Text $run.Output -Pattern '仍是占位串' -Because "错误文本要指出是占位串问题"
    Assert-Match -Text $run.Output -Pattern 'MMORPG_MYSQL_PASSWORD' -Because "要点名是哪个变量"
}

Test-Case "负向:prod 档位密钥长度不达标时必须拒绝" {
    $env2 = @{}
    foreach ($k in $ProdEnv.Keys) { $env2[$k] = $ProdEnv[$k] }
    $env2['MMORPG_GATE_TOKEN_SECRET'] = 'short'
    $run = Invoke-DeployDryRun -Arguments $ProdArgs -Env $env2

    Assert-True -Condition ($run.ExitCode -ne 0) -Because "短密钥必须阻断"
    Assert-Match -Text $run.Output -Pattern '长度 5 <' -Because "错误文本要给出实际长度和门槛"
}

Test-Case "负向:prod 档位传 latest 镜像必须被拒并说明回滚失效的理由" {
    $args2 = $BaseArgs + @("-ReleaseProfile", "prod", "-SkipPreflight", "-NodeImage", "ghcr.io/luyuancpp/mmorpg-node:latest")
    $run = Invoke-DeployDryRun -Arguments $args2 -Env $ProdEnv

    Assert-True -Condition ($run.ExitCode -ne 0) -Because "prod 不许可变 tag"
    Assert-Match -Text $run.Output -Pattern '可变 tag' -Because "错误文本要点出可变 tag"
    Assert-Match -Text $run.Output -Pattern 'rollout undo' -Because "错误文本要说清后果是回滚失效"
}

Test-Case "负向:prod 档位不加 -SkipPreflight 时预检必须真的被调用并阻断" {
    # 这条测的是"检查器有没有调用方"。仓库当前配置里 GateTokenSecret 等仍是占位串,
    # 所以预检必然 FAIL —— 如果这里居然过了,说明门禁根本没挂上。
    $args2 = $BaseArgs + @("-ReleaseProfile", "prod", "-NodeImage", "ghcr.io/luyuancpp/mmorpg-node:0123456789ab")
    $run = Invoke-DeployDryRun -Arguments $args2 -Env $ProdEnv

    Assert-True -Condition ($run.ExitCode -ne 0) -Because "预检未过必须阻断部署"
    Assert-Match -Text $run.Output -Pattern 'release preflight' -Because "错误必须来自发布预检,而不是别的失败路径"
    Assert-Match -Text $run.Output -Pattern '部署被阻断' -Because "错误文本要说清部署被拦住了"
}

Test-Case "负向:zone-down / zone-status 不受发布门禁影响(止血路径不能被挡)" {
    $run = Invoke-DeployDryRun -Arguments @("-Command", "zone-status", "-ZoneName", "contract-test", "-DryRun", "-ReleaseProfile", "prod", "-NodeImage", "ghcr.io/luyuancpp/mmorpg-node:latest")
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "出事时看状态/关 zone 绝不能被发布预检挡住。输出: $($run.Output)"
}

# ─────────────────────────────────────────────────────────────────
# 5. 灾难回滚脚本
# ─────────────────────────────────────────────────────────────────

Test-Case "负向:k8s_zone_rollback 拒绝可变 tag 作为回滚目标" {
    $run = Invoke-ToolScript -ScriptName "k8s_zone_rollback.ps1" -Arguments @(
        "-ZoneName", "contract-test", "-ZoneId", "101",
        "-TargetTime", "2026-05-15T14:23:00Z",
        "-NodeImage", "ghcr.io/luyuancpp/mmorpg-node:latest"
    )
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "灾难回滚指向可变 tag = 数据回档了、代码没回"
    Assert-Match -Text $run.Output -Pattern '灾难回滚拒绝' -Because "错误文本要点明是回滚脚本自己拒的"
}

Test-Case "k8s_zone_rollback 接受不可变 tag(dry-run 走完全流程)" {
    $run = Invoke-ToolScript -ScriptName "k8s_zone_rollback.ps1" -Arguments @(
        "-ZoneName", "contract-test", "-ZoneId", "101",
        "-TargetTime", "2026-05-15T14:23:00Z",
        "-NodeImage", "ghcr.io/luyuancpp/mmorpg-node:0123456789ab"
    )
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "dry-run 不该失败。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'DRY-RUN' -Because "默认必须是 dry-run"
}

# battle 全局池与命令 topic 的跨文件部署契约。
Test-Case 'node ConfigMap 的 Kafka 命令 topic 世代和分区数必须与预建 Job 同源' {
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'node-config')
    foreach ($key in @('CommandTopicPartitions', 'CommandTopicGeneration')) {
        Assert-Equal -Expected (Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath "Kafka.$key") -Actual (Get-FlatValue -Flat $flat -KeyPath "data.base_deploy_config.yaml.Kafka.$key") -Because '挂载 CM 后的消费者必须使用预建 topic 的同一寻址契约'
    }
}
$BattleInfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-SkipGoSvc', '-SkipJavaSvc', '-WaitReady', '-InfraNamespace', 'contract-battle-infra', '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab')
$BattleTestEnv = @{ MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345' }
$battleInfraRun = Invoke-DeployDryRun -Arguments $BattleInfraArgs -Env $BattleTestEnv
# P0-B 批 2(docs/handoff/global-singletons-p0-findings-20261014.md):node ConfigMap 的 Kafka.GroupID 从 bin/etc 镜像,不再由
# 生成器自带常数;K8s 上同批关掉旧的一节点一 topic 消费路径。紧跟上一条命令 topic 用例,只是要等 $battleInfraRun 就位,
# 好同时核 battle-node-config —— battle 只生产不消费,旧二进制在 GroupID 非空时拿不到生产端 broker,出事的正是它。
# DisableLegacyPerNodeTopic 是 K8s 模板常数(本机 bin/etc 那份仍是 false、由运维显式翻),所以断言字面值 true,不与 bin/etc 比。
Test-Case 'node ConfigMap 的 Kafka.GroupID 从 bin/etc 镜像、不再写死共享组名,K8s 上关闭旧的一节点一 topic 路径(node-config 与 battle-node-config)' {
    $expectedGroupId = Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.GroupID'
    foreach ($target in @(
        @{ Name = 'node-config'; Output = $devOut },
        @{ Name = 'battle-node-config'; Output = $battleInfraRun.Output }
    )) {
        $block = Select-ManifestByName -Output $target.Output -Name $target.Name
        $flat = ConvertTo-FlatManifest -Block $block
        Assert-Equal -Expected $expectedGroupId -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.Kafka.GroupID') -Because "$($target.Name) 整目录遮蔽镜像里的 bin/etc,GroupID 必须等于 bin/etc 的权威值(允许空串)。以前写死共享组名:旧二进制的 battle 拿不到生产端 broker,gate / scene 的旧路径全服挤进同一个消费组"
        Assert-NotMatch -Text $block -Pattern 'game-consumer-group' -Because "$($target.Name) 不得再出现生成器自带的共享组名常数(2026-05-27 本机改空后模板一直没跟)"
        Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.Kafka.DisableLegacyPerNodeTopic') -Because "$($target.Name) 必须关掉旧路径:与 GroupID 镜像同批,否则旧路径换到新消费组后会从 earliest 重放旧 topic"
    }
}
# C++ 节点 LogLevel(2026-10-22):数值直接按 muduo Logger::LogLevel 枚举解释(0=TRACE 1=DEBUG 2=INFO 3=WARN 4=ERROR 5=FATAL,
# node.cpp 的 Node::InitLogSystem 直接 static_cast)。以前生成器写死 1,K8s 上 gate / scene / battle 全部带着 DEBUG 跑,
# 而 release_preflight.ps1 的 debug.cpp.loglevel 只查 bin/etc 那份(2),看不见集群里实际生效的值。现在两份 ConfigMap 都从
# bin/etc 镜像,结构校验在 ConvertTo-CppNodeLogLevel(按 AST 抽出来直接喂非法值,不另抄一份判定)。
foreach ($logLevelFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($logLevelFnAst.Name -eq 'ConvertTo-CppNodeLogLevel') {
        Set-Item -Path "Function:script:$($logLevelFnAst.Name)" -Value $logLevelFnAst.Body.GetScriptBlock()
    }
}
if (-not (Test-Path 'Function:ConvertTo-CppNodeLogLevel')) { throw 'k8s_deploy.ps1 缺少函数 ConvertTo-CppNodeLogLevel(LogLevel 结构校验被删了?后续断言无意义)' }
Test-Case 'node-config 与 battle-node-config 的 LogLevel 从 bin/etc 镜像、生成器不写死;仓库值 >= 2(muduo 2=INFO,与发布预检同一下界)' {
    $authoritative = Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'LogLevel'
    Assert-True -Condition ([int]$authoritative -ge 2) -Because "仓库默认值必须过 release_preflight.ps1 的 debug.cpp.loglevel(>= 2):1 = DEBUG、0 = TRACE(当前 '$authoritative')"
    foreach ($target in @(
        @{ Name = 'node-config'; Output = $devOut },
        @{ Name = 'battle-node-config'; Output = $battleInfraRun.Output }
    )) {
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $target.Output -Name $target.Name)
        Assert-Equal -Expected $authoritative -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.LogLevel') -Because "$($target.Name) 整目录遮蔽镜像里的 bin/etc,LogLevel 必须等于 bin/etc 的权威值(以前写死 1 = DEBUG)"
    }
}
Test-Case '自检:ConvertTo-CppNodeLogLevel 对 0..5 返回整数,对 6 / -1 / 空串 / INFO / 2.5 一律 throw 并给出 muduo 枚举对照(守卫不得静默放行)' {
    foreach ($ok in 0..5) {
        Assert-Equal -Expected $ok -Actual (ConvertTo-CppNodeLogLevel -Value "$ok") -Because "$ok 是 muduo 枚举项"
    }
    foreach ($bad in @('6', '-1', '', 'INFO', '2.5')) {
        $message = ''
        try { ConvertTo-CppNodeLogLevel -Value $bad | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '0\.\.5 的整数' -Because "'$bad' 不是 muduo 枚举项,生成期必须 throw"
        Assert-Match -Text $message -Pattern '1=DEBUG 2=INFO' -Because '错误文本要给出枚举对照,别让人再按「0=DEBUG」去猜'
    }
}
Test-Case 'release_preflight.ps1 的 debug.cpp.loglevel 查 bin/etc 的 LogLevel、下界 2(以前按「0=DEBUG」写成 1,恰好放过 DEBUG)' {
    # 预检的 PASS 行不打印下界,跑一遍只能看到 [PASS],钉不住下界;所以按 AST 静态核对这一次调用。
    $preflightTokens = $null
    $preflightErrors = $null
    $preflightAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'release_preflight.ps1'), [ref]$preflightTokens, [ref]$preflightErrors)
    Assert-True -Condition ($preflightErrors.Count -eq 0) -Because "release_preflight.ps1 必须能解析: $($preflightErrors.Message -join '; ')"
    $calls = @($preflightAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] -and $a.GetCommandName() -eq 'Test-NumericFloor' -and $a.Extent.Text -match "-Id\s+'debug\.cpp\.loglevel'" }, $true))
    Assert-Equal -Expected 1 -Actual $calls.Count -Because 'debug.cpp.loglevel 必须恰好有一处 Test-NumericFloor 调用'
    $text = $calls[0].Extent.Text
    Assert-Match -Text $text -Pattern "-RelativePath\s+'bin/etc/base_deploy_config\.yaml'\s+-KeyPath\s+'LogLevel'" -Because '预检必须查 node-config 镜像的同一个键,否则又是两份各说各话'
    Assert-Match -Text $text -Pattern '-Floor\s+2(\s|$)' -Because 'muduo 枚举 1 = DEBUG、0 = TRACE,生产下界是 2(INFO)'
}
Test-Case 'infra-up 默认部署一个 battle 并等待 rollout；zone-up 不重复部署全局池' {
    Assert-Equal -Expected 0 -Actual $battleInfraRun.ExitCode -Because 'battle 全局装配 DryRun 必须成功'
    $battle = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $battleInfraRun.Output -Name 'battle')
    Assert-Equal -Expected 1 -Actual (Get-FlatValue -Flat $battle -KeyPath 'spec.replicas') -Because '与本地默认一个 battle 一致'
    Assert-Equal -Expected 'Deployment' -Actual (Get-FlatValue -Flat $battle -KeyPath 'kind') -Because 'battle 由 infra 管理一个全局 Deployment'
    Assert-Match -Text $battleInfraRun.Output -Pattern 'rollout status deployment/battle -n contract-battle-infra' -Because 'WaitReady 必须包含 battle'
    Assert-True -Condition ($null -eq (Select-ManifestByName -Output $devOut -Name 'battle')) -Because 'zone-up 不得每区复制全局 battle 池'
}
Test-Case 'battle 部署保留 POD_IP 和独立 TCP/gRPC 端口，exec 传递退出信号' {
    $block = Select-ManifestByName -Output $battleInfraRun.Output -Name 'battle'
    Assert-Match -Text $block -Pattern 'args: \["mkdir -p /app/bin/logs/cpp_nodes && .*&& exec \./battle"\]' -Because 'shell 必须 exec，退出信号直接到 battle 主进程'
    Assert-Match -Text $block -Pattern 'fieldPath: status.podIP' -Because '发现必须通告实际 Pod IP'
    Assert-Match -Text $block -Pattern 'name: RPC_PORT\s+value: "20000"' -Because 'TCP 端口必须在非 gate 合法区间'
    Assert-Match -Text $block -Pattern 'name: NODE_PORT\s+value: "20000"' -Because '两种端口环境别名必须一致'
    Assert-Match -Text $block -Pattern 'containerPort: 50000\s+name: grpc' -Because 'gRPC=TCP+30000'
    Assert-Match -Text $block -Pattern 'startupProbe:\s+tcpSocket:\s+port: 20000' -Because '先等待客户端直连面监听'
    Assert-Match -Text $block -Pattern 'startupProbe:\s+tcpSocket:\s+port: 20000\s+periodSeconds: 2\s+failureThreshold: 150' -Because 'startup 预算 300s 必须越过 180s etcd 租约,否则同 Pod 重启撞旧注册时会被多杀几轮'
    Assert-Match -Text $block -Pattern 'readinessProbe:\s+tcpSocket:\s+port: 50000' -Because 'C++ 尚无 grpc.health.v1，必须使用可实现的 TCP 探针'
    Assert-NotMatch -Text $block -Pattern 'GATE_CLIENT_RPC_ROUTER|BATTLE_RUN_MODE' -Because '不能误注 gate 模式或关闭 battle 默认 prod 启动门禁'
}
# P0-A 第二批:battle 读 battle-node-config 的 zoneredis.password(与 zone 的 node-config 同一个生成器)。带口令的 infra-up 另起两个 DryRun:
# 默认 Deployment 形态(New-NodeDeploymentYaml)与 Agones Fleet 形态(lib New-BattleFleetYaml);口令按 dev 档注入(dev 非空即采用)。
# battle 工作负载在 Apply-Infra 开头、密钥解析之后渲染:若又挪回 Initialize-InjectedSecrets 之前,Get-CppZoneRedisPassword 对「未解析」
# fail-closed,这两个 DryRun 会直接失败。工具函数(Select-CppWorkloadBlock / Get-ZoneRedisAuthHash / Get-ExpectedZoneRedisAuthHash)见上文 3a 节。
$BattleZoneRedisPassword = 'contract-test-battle-zone-redis-06'
$battleZoneRedisEnv = $BattleTestEnv + @{ MMORPG_REDIS_PASSWORD = $BattleZoneRedisPassword }
$battleZoneRedisRun = Invoke-DeployDryRun -Arguments $BattleInfraArgs -Env $battleZoneRedisEnv
$battleZoneRedisFleetRun = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-BattleOrchestrator', 'agones', '-ClientEntryMode', 'external', '-ClientPublicHost', '127.0.0.1')) -Env $battleZoneRedisEnv
Test-Case 'battle:带口令的 infra-up 里 battle-node-config 的 zoneredis.password == 注入值,battle(Deployment / Agones Fleet)带口令指纹;不注入时空串且无指纹' {
    $expected = Get-ExpectedZoneRedisAuthHash -Password $BattleZoneRedisPassword
    foreach ($case in @(
        @{ Label = 'battle Deployment'; Run = $battleZoneRedisRun; Kind = 'Deployment' }
        @{ Label = 'battle Agones Fleet'; Run = $battleZoneRedisFleetRun; Kind = 'Fleet' }
    )) {
        Assert-Equal -Expected 0 -Actual $case.Run.ExitCode -Because "$($case.Label):带口令的 infra-up DryRun 应当成功(battle 工作负载渲染排在密钥解析之后)。输出: $($case.Run.Output)"
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $case.Run.Output -Name 'battle-node-config')
        Assert-True -Condition ((Get-FlatValue -Flat $flat -KeyPath 'data.game_config.yaml.zoneredis.password') -ceq $BattleZoneRedisPassword) -Because "$($case.Label):battle-node-config 的口令必须是注入值(与服务端 Secret redis-auth 同源)"
        $block = Select-CppWorkloadBlock -Output $case.Run.Output -Kind $case.Kind -Name 'battle' -Label $case.Label
        Assert-Equal -Expected $expected -Actual (Get-ZoneRedisAuthHash -Block $block -Label $case.Label) -Because "$($case.Label):Pod 模板必须带只由口令决定的指纹(换口令才会滚动 battle;C++ 只在启动时读配置)"
        Assert-True -Condition (-not $block.Contains($BattleZoneRedisPassword)) -Because "$($case.Label):工作负载里只有指纹,不出现口令明文"
    }
    Assert-Equal -Expected 0 -Actual $battleInfraRun.ExitCode -Because '对照:不注入口令的 battle 基线 DryRun 必须成功'
    $baseFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $battleInfraRun.Output -Name 'battle-node-config')
    Assert-Equal -Expected '' -Actual (Get-FlatValue -Flat $baseFlat -KeyPath 'data.game_config.yaml.zoneredis.password') -Because '对照:dev 不注入口令时 battle-node-config 写空串,battle 不发 AUTH,与改动前相同'
    Assert-NotMatch -Text $battleInfraRun.Output -Pattern 'zone-redis-auth-hash' -Because '对照:口令为空时 battle 不写指纹注解,Pod 模板与改动前逐字相同'
}
Test-Case 'gate 就绪探针只探唯一的监听口 18000,不探不存在的 gRPC 口,也不加会杀容器的探针' {
    $block = Select-ManifestByName -Output $devOut -Name 'gate'
    Assert-Match -Text $block -Pattern 'readinessProbe:\s+tcpSocket:\s+port: 18000\s+periodSeconds: 5\s+failureThreshold: 3' -Because '玩家连接与节点 RPC 共用 18000,它 listen 即已注册进 etcd'
    Assert-NotMatch -Text $block -Pattern 'port: 48000' -Because 'gate 不注册 gRPC 服务,RpcPort+30000 上没有监听,探它会恒失败'
    Assert-NotMatch -Text $block -Pattern 'livenessProbe|startupProbe' -Because 'tcpSocket 看不出 EventLoop 卡死;startup 预算给不准会在同 Pod 重启时多杀几轮'
}

# gate 客户端 RPC 路由模式(turn-based §22 D75):K8s 部署层默认 "1",C++ 进程默认值与单测不改(D-12)。
# 部署层默认值只允许存在于 k8s_deploy.ps1 一处;这里钉住「默认 "1"、只注入 gate、"0" 回退仍可生成、拼错在入口就拒、
# 包装入口留空不覆盖」。翻转之前这个开关零覆盖,默认值被悄悄改回去不会有任何报错。
Test-Case 'gate 默认以路由模式部署:GATE_CLIENT_RPC_ROUTER="1" 只注入 gate,scene(Deployment / Fleet)不带' {
    $gate = Select-ManifestByName -Output $devOut -Name 'gate'
    Assert-Match -Text $gate -Pattern 'name: GATE_CLIENT_RPC_ROUTER\s+value: "1"' -Because 'K8s 默认路由模式(D75);省略该变量 gate 按 C++ 默认落回直连,chat / friend / trade 全部不可达'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($gate, 'name: GATE_CLIENT_RPC_ROUTER').Count) -Because 'gate 只该注入一次,重复的 env 键以哪条为准不该交给 kubelet 决定'
    Assert-Match -Text (Select-ManifestByName -Output $agonesOut -Name 'gate') -Pattern 'name: GATE_CLIENT_RPC_ROUTER\s+value: "1"' -Because 'scene 编排方式不影响 gate 的默认模式'
    Assert-NotMatch -Text (Select-ManifestByName -Output $devOut -Name 'scene') -Pattern 'GATE_CLIENT_RPC_ROUTER' -Because 'scene 不读这个变量,注入只会让人误以为它也分模式'
    Assert-NotMatch -Text (Select-ManifestByName -Output $agonesOut -Name 'scene') -Pattern 'GATE_CLIENT_RPC_ROUTER' -Because 'Agones Fleet 模板同样不得注入'
}
Test-Case '-GateRouterMode 0 回退路径仍可生成,gate 显式写出 "0"' {
    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-GateRouterMode', '0', '-SkipGoSvc', '-SkipJavaSvc'))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "回退到直连模式的部署路径必须照样能生成。输出: $($run.Output)"
    Assert-Match -Text (Select-ManifestByName -Output $run.Output -Name 'gate') -Pattern 'name: GATE_CLIENT_RPC_ROUTER\s+value: "0"' -Because '"0" 也显式写出:kubectl 里一眼看出 gate 跑在直连模式,不靠猜 C++ 默认值'
}
Test-Case '负向:-GateRouterMode 只收 "0" / "1",拼错必须在入口被拒' {
    foreach ($bad in @('2', 'true')) {
        $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-GateRouterMode', $bad, '-SkipGoSvc', '-SkipJavaSvc'))
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "gate 侧除 1/true/on 外一律当关;'$bad' 必须在脚本入口被拒,而不是原样写进 env 让两边字面值分家"
        Assert-Match -Text $run.Output -Pattern 'GateRouterMode' -Because '不能把其他执行错误误判为参数校验成功'
        Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl apply' -Because '参数校验必须先于任何资源变更'
    }
}
Test-Case '发现前缀:node-config 含 ClientRpcRouterNodeService.rpc;battle-node-config 保留 BattleNodeService.rpc' {
    Assert-Match -Text (Select-ManifestByName -Output $devOut -Name 'node-config') -Pattern '- "ClientRpcRouterNodeService\.rpc"' -Because '默认路由模式下 gate 的依赖门等 ClientRpcRouter,发现不到它 gate 永远过不了依赖门,登录 / 匹配全部 no_target'
    Assert-Match -Text (Select-ManifestByName -Output $battleInfraRun.Output -Name 'battle-node-config') -Pattern '- "BattleNodeService\.rpc"' -Because 'gate 已不连 battle(turn-based §22 D66),但 battle 自己按这些前缀 watch 自身节点键做劫持检测与注册自检,不能当死前缀删掉'
}

Test-Case '玩家收件箱:node-config 与 bin/etc 都含 PlayerInboxNodeService.rpc,deadline 门禁核对了 PlayerInboxNodeService' {
    # scene 按这个前缀发现 player_inbox(docs/design/player-inbox-service.md §1.4)。两份前缀表任一漏写,收件箱在那一侧永远拉不到 ——
    # 弱依赖不报错(IB-7),只会表现为「GM 处置永不生效」。
    Assert-Match -Text (Select-ManifestByName -Output $devOut -Name 'node-config') -Pattern '- "PlayerInboxNodeService\.rpc"' -Because 'K8s 的 node ConfigMap 遮蔽镜像里的 bin/etc,前缀必须在生成器里也写出来'
    $authoritativeDeploy = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'bin/etc/base_deploy_config.yaml') -Raw
    Assert-Match -Text $authoritativeDeploy -Pattern '(?m)^\s*-\s*"PlayerInboxNodeService\.rpc"' -Because '本地栈读 bin/etc/base_deploy_config.yaml 的前缀表'
    $okLine = [string]($devOut -split "`r?`n" | Where-Object { $_ -match 'GrpcClient deadline budget OK' } | Select-Object -First 1)
    Assert-Match -Text $okLine -Pattern 'PlayerInboxNodeService' -Because 'Assert-GrpcClientDeadlineBudget 的目标表必须含 PlayerInboxNodeService(scene → player_inbox 的 deadline ≥ zrpc Timeout + 2000)'
    # 消费端开关(交接 player-inbox-handoff-20261015 §6 H1):node-config 的 player_inbox 段整段复制自 git 跟踪的 bin/etc/game_config.yaml,
    # 不由参数决定。不传 -PlayerInboxConsumer 时渲染值就是 yaml 的值 —— 以前钉的是「不传即 false」,那正是例行发布静默关掉消费的根因。
    # 键集合与值都逐项比对:periodic_fetch_minutes 跟着同一段走,以后往这一段加键也不必回来改模板。
    $authoritativeGame = (ConvertFrom-YamlToFlatMap -Text (Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'bin/etc/game_config.yaml') -Raw)).Scalars
    $expectedInboxKeys = @($authoritativeGame.Keys | Where-Object { $_ -like 'player_inbox.*' })
    Assert-True -Condition ($expectedInboxKeys -contains 'player_inbox.consumer_enabled') -Because 'bin/etc/game_config.yaml 必须显式写出 player_inbox.consumer_enabled(本断言与写前门禁的前提)'
    $nodeGame = ConvertTo-UnprefixedScalars -Scalars (ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'node-config')).Scalars -Prefix 'data.game_config.yaml.'
    $renderedInboxKeys = @($nodeGame.Keys | Where-Object { $_ -like 'player_inbox.*' })
    Assert-Equal -Expected ($expectedInboxKeys -join ',') -Actual ($renderedInboxKeys -join ',') -Because 'node-config 的 player_inbox 键集合必须与 bin/etc/game_config.yaml 逐项相同(整段复制,不增不减)'
    foreach ($key in $expectedInboxKeys) {
        Assert-Equal -Expected $authoritativeGame[$key] -Actual $nodeGame[$key] -Because "node-config 的 $key 必须与 bin/etc/game_config.yaml 一致(不传 -PlayerInboxConsumer 也一样,开关不由参数渲染)"
    }
}

Test-Case '玩家收件箱:-PlayerInboxConsumer 只核对、不改值 —— 与 yaml 一致时放行且渲染值仍取 yaml;不一致时在任何写操作之前拒绝并指向 yaml' {
    # 交接 §6 H1:任何路径都不能渲染出与 bin/etc/game_config.yaml 不同的值。参数保留下来只为把「我认为它开着 / 关着」写进发布命令。
    $yamlValue = Get-EtcValue -RelativePath 'bin/etc/game_config.yaml' -KeyPath 'player_inbox.consumer_enabled'
    Assert-True -Condition ($yamlValue -ceq 'true' -or $yamlValue -ceq 'false') -Because "前提:bin/etc/game_config.yaml 的 player_inbox.consumer_enabled 只能是小写 true / false(实际 '$yamlValue')"
    $same = if ($yamlValue -ceq 'true') { 'on' } else { 'off' }
    $other = if ($same -ceq 'on') { 'off' } else { 'on' }

    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-PlayerInboxConsumer', $same, '-SkipGoSvc', '-SkipJavaSvc'))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "与 yaml 一致的 -PlayerInboxConsumer $same 必须放行。输出: $($run.Output)"
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $run.Output -Name 'node-config')
    Assert-Equal -Expected $yamlValue -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.game_config.yaml.player_inbox.consumer_enabled') -Because '渲染值只取 yaml'

    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-PlayerInboxConsumer', $other, '-SkipGoSvc', '-SkipJavaSvc'))
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "与 yaml 不一致的 -PlayerInboxConsumer $other 必须拒绝部署,不能渲染出与 yaml 不同的值"
    # 子进程的错误输出可能按控制台宽度折行,这里只断消息开头;完整文本(yaml 实际值、「去改 yaml」)由下文 AST 用例断 Exception.Message。
    Assert-Match -Text $run.Output -Pattern "PlayerInboxConsumer $other 与 bin/etc/game_config\.yaml" -Because '错误必须点名参数值与权威文件'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '核对必须先于任何资源变更'
}

Test-Case '玩家收件箱写前门禁:zone-up 与 infra-up 的写路径入口都先过门禁,之后才有任何 kubectl 写操作' {
    foreach ($entry in @(@{ Name = 'zone-up'; Output = $devOut }, @{ Name = 'infra-up'; Output = $battleInfraRun.Output })) {
        $gateAt = $entry.Output.IndexOf('player-inbox consumer gate: ')
        $firstKubectlAt = $entry.Output.IndexOf('[dry-run] kubectl')
        Assert-True -Condition ($gateAt -ge 0) -Because "$($entry.Name) DryRun 必须经过 Assert-PlayerInboxConsumerGate(写路径入口门禁),否则守卫等于没挂"
        Assert-True -Condition ($firstKubectlAt -lt 0 -or $gateAt -lt $firstKubectlAt) -Because "$($entry.Name) 的门禁必须先于任何 kubectl 写操作,不留半截部署"
    }
    Assert-Match -Text $devOut -Pattern 'player-inbox consumer gate: consumer_enabled=(true|false) \(bin/etc/game_config\.yaml\) enqueue_types=\d+ \(go/player_inbox/etc/player_inbox\.yaml\) head=\S+ worktree=(clean|dirty|unknown) live=\S+ OK' -Because '放行时打一行门禁结果,写明两个值各取自哪份文件,以及 HEAD、两份 yaml 是否已提交、线上交叉核对的结果'
    # DryRun 不读集群:yaml 里消费端关着时 zone-up 本该读 infra 线上的白名单,DryRun 下只能写明跳过;开着时本来就不用读。
    $yamlConsumer = Get-EtcValue -RelativePath 'bin/etc/game_config.yaml' -KeyPath 'player_inbox.consumer_enabled'
    $expectedLive = if ($yamlConsumer -ceq 'false') { 'skipped\(dry-run\)' } else { 'not-needed' }
    Assert-Match -Text $devOut -Pattern "player-inbox consumer gate: [^\r\n]* live=$expectedLive OK" -Because "zone-up DryRun(consumer_enabled=${yamlConsumer})的线上核对必须是 ${expectedLive}:DryRun 不读集群"
}

# 收件箱写前门禁的判定按 AST 抽出直接调用(与上文 deadline 守卫同一做法):三份输入拷到临时目录再改,不碰仓库,也不执行脚本入口。
# 它们用到的 ConvertFrom-YamlToFlatMap / Get-YamlScalar 来自 deploy_capture.ps1 dot-source 的 release_common.ps1;$RepoRoot 由用例自己设。
# 线上交叉核对(Read-PlayerInboxGateLiveFacts)读集群只经 Invoke-ClientEntryKubectl,用例在自己的作用域里换成假函数,不碰真集群。
$inboxGateFunctionNames = @('Get-AuthoritativeScalar', 'Get-PlayerInboxConsumerEnabled', 'Get-PlayerInboxEnqueueTypes',
    'ConvertFrom-PlayerInboxEnqueueTypesText', 'Assert-PlayerInboxConsumerGate', 'Get-PlayerInboxGateSourceState',
    'Read-PlayerInboxGateLiveFacts', 'Test-GlobalGoSvcDeployed')
foreach ($inboxGateFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($inboxGateFunctionNames -contains $inboxGateFnAst.Name) {
        Set-Item -Path "Function:script:$($inboxGateFnAst.Name)" -Value $inboxGateFnAst.Body.GetScriptBlock()
    }
}
foreach ($inboxGateFnName in $inboxGateFunctionNames) {
    if (-not (Test-Path "Function:$inboxGateFnName")) { throw "k8s_deploy.ps1 缺少函数 $inboxGateFnName(收件箱写前门禁被删了?后续断言无意义)" }
}

# 门禁读的全部文件拷到临时目录,把消费开关那一行与 EnqueueTypes 那一行换成给定内容(空串 = 删掉这个键)。返回临时根目录,调用方负责删。
function New-PlayerInboxGateFixture {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$ConsumerLine,
        [Parameter(Mandatory = $true)][string]$EnqueueLine
    )
    $root = Join-Path ([System.IO.Path]::GetTempPath()) ('player-inbox-consumer-gate-' + [guid]::NewGuid().ToString('N'))
    foreach ($rel in @('bin/etc/game_config.yaml', 'go/player_inbox/etc/player_inbox.yaml', 'proto/player_inbox/player_inbox.proto')) {
        $dst = Join-Path $root $rel
        New-Item -ItemType Directory -Force -Path (Split-Path -Parent $dst) | Out-Null
        Copy-Item -LiteralPath (Join-Path (Get-RepoRoot) $rel) -Destination $dst
    }
    # 只换顶格缩进下的真键行:注释行(# 开头)里的同名词不算。恰好一处才改,否则夹具前提不成立。
    foreach ($edit in @(
            @{ File = 'bin/etc/game_config.yaml'; Pattern = '(?m)^([ \t]+)consumer_enabled:[^\r\n]*'; Line = $ConsumerLine }
            @{ File = 'go/player_inbox/etc/player_inbox.yaml'; Pattern = '(?m)^([ \t]+)EnqueueTypes:[^\r\n]*'; Line = $EnqueueLine }
        )) {
        $path = Join-Path $root $edit.File
        $text = Get-Content -LiteralPath $path -Raw
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, $edit.Pattern).Count) -Because "夹具前提:$($edit.File) 里恰好一处 /$($edit.Pattern)/"
        $line = $edit.Line
        $mutated = [regex]::Replace($text, $edit.Pattern, { param($m) $m.Groups[1].Value + $line }.GetNewClosure())
        [System.IO.File]::WriteAllText($path, $mutated, [System.Text.UTF8Encoding]::new($false))
    }
    return $root
}

Test-Case '玩家收件箱写前门禁:消费端关 + 入箱白名单非空时拒绝部署(点名两个文件、白名单与豁免开关),-AllowPlayerInboxConsumerOff 才放行;其余组合放行(夹具是临时目录里的配置副本,不碰仓库)' {
    $nonEmpty = 'EnqueueTypes: [PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN, PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN]'
    $cases = @(
        @{ Consumer = 'false'; Enqueue = $nonEmpty;           Allow = $false; Rejects = $true;  Why = '消费端关、白名单非空、未豁免' }
        @{ Consumer = 'false'; Enqueue = $nonEmpty;           Allow = $true;  Rejects = $false; Why = '显式 -AllowPlayerInboxConsumerOff' }
        @{ Consumer = 'false'; Enqueue = 'EnqueueTypes: []';  Allow = $false; Rejects = $false; Why = '上线第 3–4 步:消费端关、白名单空(回滚下限)' }
        @{ Consumer = 'true';  Enqueue = 'EnqueueTypes: []';  Allow = $false; Rejects = $false; Why = '上线第 5 步:消费端已开、白名单还空' }
        @{ Consumer = 'true';  Enqueue = $nonEmpty;           Allow = $false; Rejects = $false; Why = '上线第 6 步之后的常态' }
    )
    foreach ($case in $cases) {
        $root = New-PlayerInboxGateFixture -ConsumerLine "consumer_enabled: $($case.Consumer)" -EnqueueLine $case.Enqueue
        try {
            $RepoRoot = $root
            $message = ''
            $printed = ''
            try { $printed = [string](Assert-PlayerInboxConsumerGate -AllowConsumerOff $case.Allow 6>&1 3>&1 | Out-String -Width 4096) } catch { $message = $_.Exception.Message }
            if ($case.Rejects) {
                Assert-Match -Text $message -Pattern '^拒绝部署' -Because "$($case.Why):必须拒绝部署,不能只打警告"
                Assert-Match -Text $message -Pattern 'bin/etc/game_config\.yaml 的 player_inbox\.consumer_enabled=false' -Because '错误必须点名消费开关所在的文件与值'
                Assert-Match -Text $message -Pattern 'go/player_inbox/etc/player_inbox\.yaml 的 PlayerInbox\.EnqueueTypes 非空\(PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN, PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN\)' -Because '错误必须点名白名单所在的文件与内容'
                Assert-Match -Text $message -Pattern '-AllowPlayerInboxConsumerOff' -Because '错误必须写明豁免开关'
                continue
            }
            Assert-Equal -Expected '' -Actual $message -Because "$($case.Why) 必须放行"
            Assert-Match -Text $printed -Pattern "player-inbox consumer gate: consumer_enabled=$($case.Consumer) " -Because "$($case.Why):放行时打一行门禁结果"
            if ($case.Allow) {
                Assert-Match -Text $printed -Pattern 'AllowPlayerInboxConsumerOff 放行了「消费端关、入箱开」' -Because '豁免放行必须留下警告,说明新入箱的处置不会执行'
            }
            else {
                Assert-Match -Text $printed -Pattern ' OK' -Because "$($case.Why):正常放行打 OK"
            }
        }
        finally {
            Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

Test-Case '负向:收件箱消费开关只认显式写出的小写 true / false;-PlayerInboxConsumer 与 yaml 不一致时门禁拒绝(夹具是临时目录里的配置副本,不碰仓库)' {
    foreach ($bad in @(
            @{ Line = 'consumer_enabled: yes'; Pattern = "consumer_enabled='yes' 只能写小写 true 或 false"; Why = 'yaml-cpp 认 yes,门禁不替你猜' }
            @{ Line = 'consumer_enabled: True'; Pattern = "consumer_enabled='True' 只能写小写 true 或 false"; Why = '大小写不同同样拒绝' }
            @{ Line = ''; Pattern = '查不到 player_inbox\.consumer_enabled'; Why = 'C++ 缺键取 false,部署侧要求显式写出' }
        )) {
        $root = New-PlayerInboxGateFixture -ConsumerLine $bad.Line -EnqueueLine 'EnqueueTypes: []'
        try {
            $RepoRoot = $root
            $message = ''
            try { Assert-PlayerInboxConsumerGate 6>&1 | Out-Null } catch { $message = $_.Exception.Message }
            Assert-Match -Text $message -Pattern $bad.Pattern -Because "$($bad.Why)(夹具行 '$($bad.Line)')"
        }
        finally {
            Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    $root = New-PlayerInboxGateFixture -ConsumerLine 'consumer_enabled: true' -EnqueueLine 'EnqueueTypes: []'
    try {
        $RepoRoot = $root
        Assert-PlayerInboxConsumerGate -ExpectedConsumer 'on' 6>&1 | Out-Null
        $message = ''
        try { Assert-PlayerInboxConsumerGate -ExpectedConsumer 'off' 6>&1 | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '-PlayerInboxConsumer off 与 bin/etc/game_config\.yaml 的 player_inbox\.consumer_enabled=true 不一致,拒绝部署' -Because 'yaml 已开消费时,发布命令写 off 必须拒绝(防止以为关了、实际开着)'
        Assert-Match -Text $message -Pattern '改那份 yaml 并提交' -Because '错误必须告诉部署的人去改 yaml,而不是换个参数重跑'
    }
    finally {
        Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
    }
}

# 门禁的线上交叉核对与来源核对(交接 §6 H1 复核):zone 与 infra 分开部署、可以来自不同 commit,只比当前检出的两份 yaml 拦不住
# 「检出旧 commit 重跑 zone-up」与「同一提交里先跑 infra-up」。下面两条把 -LiveFacts / -SourceState 直接交给门禁,不依赖真集群与 git。
$inboxGateNonEmptyLine = 'EnqueueTypes: [PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN, PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN]'

Test-Case '玩家收件箱写前门禁:线上交叉核对 —— 本次渲染 false 而 infra 线上白名单非空、或本次渲染非空白名单而有 zone 线上没开消费,都在写操作之前拒绝(两份 yaml 本身不矛盾)' {
    # 场景一:开消费之后检出旧 commit 重跑 zone-up。那个 commit 里 consumer_enabled=false、EnqueueTypes=[],静态层放行;infra 线上白名单仍非空。
    $root = New-PlayerInboxGateFixture -ConsumerLine 'consumer_enabled: false' -EnqueueLine 'EnqueueTypes: []'
    try {
        $RepoRoot = $root
        $live = [pscustomobject]@{
            Note = 'infra_enqueue_types=1'; InfraSource = 'namespace contract-infra 的 ConfigMap go-svc-player-inbox-config'
            InfraEnqueueTypes = [string[]]@('PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN'); ZoneConsumers = $null
        }
        $message = ''
        try { Assert-PlayerInboxConsumerGate -LiveFacts $live 6>&1 3>&1 | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '^拒绝部署:本次把 consumer_enabled=false 渲染进 node-config,而线上 namespace contract-infra 的 ConfigMap go-svc-player-inbox-config 的 PlayerInbox\.EnqueueTypes 非空\(PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN\)' -Because '两份 yaml 不矛盾也要拒绝:另一侧线上已经开了入箱'
        Assert-Match -Text $message -Pattern '用当前 HEAD 的脚本配旧 -NodeImage,或 kubectl rollout undo' -Because '拒绝时写明开消费之后该怎么回滚 zone'
        Assert-Match -Text $message -Pattern 'k8s_zone_rollback\.ps1\)没有这个开关' -Because '包装入口没有豁免开关,拒绝文案必须写明'
        Assert-Match -Text $message -Pattern 'EnqueueTypes 改回 \[\] 并提交、重跑 infra-up\(正常关消费的顺序\),门禁随之放行' -Because '给出包装入口可执行的恢复路径'

        $printed = [string](Assert-PlayerInboxConsumerGate -LiveFacts $live -AllowConsumerOff $true 6>&1 3>&1 | Out-String -Width 4096)
        Assert-Match -Text $printed -Pattern 'AllowPlayerInboxConsumerOff 放行了「消费端关、入箱开」' -Because '豁免同样覆盖线上这一层,放行时留下警告'

        $live.InfraEnqueueTypes = [string[]]@()
        $live.Note = 'infra_enqueue_types=0'
        $printed = [string](Assert-PlayerInboxConsumerGate -LiveFacts $live 6>&1 3>&1 | Out-String -Width 4096)
        Assert-Match -Text $printed -Pattern 'live=infra_enqueue_types=0 OK' -Because 'infra 线上白名单已清空(正常关消费的顺序)时放行,门禁行写明线上核对结果'
    }
    finally {
        Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
    }

    # 场景二:开消费与填白名单在同一次提交里,先跑了 infra-up(或 all-up)。静态层放行;有 zone 线上还没开。
    $root = New-PlayerInboxGateFixture -ConsumerLine 'consumer_enabled: true' -EnqueueLine $inboxGateNonEmptyLine
    try {
        $RepoRoot = $root
        $live = [pscustomobject]@{
            Note = 'zones_consumer_enabled=1/3'; InfraSource = ''; InfraEnqueueTypes = $null
            ZoneConsumers = @(
                [pscustomobject]@{ Namespace = 'mmorpg-zone-a'; Value = 'true' }
                [pscustomobject]@{ Namespace = 'mmorpg-zone-b'; Value = 'false' }
                [pscustomobject]@{ Namespace = 'mmorpg-zone-c'; Value = '' }
            )
        }
        $message = ''
        try { Assert-PlayerInboxConsumerGate -LiveFacts $live 6>&1 3>&1 | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '^拒绝部署:本次把非空的 PlayerInbox\.EnqueueTypes\(PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN, PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN\)渲染进 player-inbox 的 ConfigMap' -Because '有 zone 线上没开消费时,不能先部署白名单'
        Assert-Match -Text $message -Pattern ':mmorpg-zone-b=false, mmorpg-zone-c=absent。' -Because '点名每个没开消费的 zone,缺键记 absent(C++ 缺键取 false)'
        Assert-NotMatch -Text $message -Pattern 'mmorpg-zone-a=' -Because '已开消费的 zone 不点名'
        Assert-Match -Text $message -Pattern 'all-up -SkipInfra' -Because 'all-up 先部署白名单、后部署 zone,拒绝文案写明不能一步完成'

        $live.ZoneConsumers = @([pscustomobject]@{ Namespace = 'mmorpg-zone-a'; Value = 'true' })
        $live.Note = 'zones_consumer_enabled=1/1'
        $printed = [string](Assert-PlayerInboxConsumerGate -LiveFacts $live 6>&1 3>&1 | Out-String -Width 4096)
        Assert-Match -Text $printed -Pattern 'live=zones_consumer_enabled=1/1 OK' -Because '全部 zone 线上都开了消费时放行'

        $live.ZoneConsumers = @()
        $live.Note = 'zones_consumer_enabled=0/0'
        $printed = [string](Assert-PlayerInboxConsumerGate -LiveFacts $live 6>&1 3>&1 | Out-String -Width 4096)
        Assert-Match -Text $printed -Pattern 'live=zones_consumer_enabled=0/0 OK' -Because '还没有任何 zone(新集群)时放行:之后的 zone-up 渲染 yaml 里的 true'
    }
    finally {
        Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
    }

    # 场景三:关消费的第一步(白名单改回 [])。本次不渲染非空白名单,zone 线上关着也不拒绝。
    $root = New-PlayerInboxGateFixture -ConsumerLine 'consumer_enabled: true' -EnqueueLine 'EnqueueTypes: []'
    try {
        $RepoRoot = $root
        $live = [pscustomobject]@{
            Note = 'not-needed'; InfraSource = ''; InfraEnqueueTypes = $null
            ZoneConsumers = @([pscustomobject]@{ Namespace = 'mmorpg-zone-b'; Value = 'false' })
        }
        $printed = [string](Assert-PlayerInboxConsumerGate -LiveFacts $live 6>&1 3>&1 | Out-String -Width 4096)
        Assert-Match -Text $printed -Pattern 'enqueue_types=0 [^\r\n]* OK' -Because '白名单为空时,zone 线上开没开都不影响放行'
    }
    finally {
        Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case '玩家收件箱写前门禁:两份 yaml 带未提交改动时,staging / prod 真部署拒绝、其余只告警;门禁行写明 HEAD 与工作区状态;-AllowPlayerInboxConsumerOff 不豁免这一条' {
    $root = New-PlayerInboxGateFixture -ConsumerLine 'consumer_enabled: true' -EnqueueLine 'EnqueueTypes: []'
    try {
        $RepoRoot = $root
        $dirty = [pscustomobject]@{ Ok = $true; Head = '0123456789ab'; DirtyFiles = [string[]]@('bin/etc/game_config.yaml'); Reason = '' }
        $message = ''
        try { Assert-PlayerInboxConsumerGate -SourceState $dirty -RequireCommittedSources $true -AllowConsumerOff $true 6>&1 3>&1 | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '^拒绝部署:bin/etc/game_config\.yaml 相对 HEAD 0123456789ab 有未提交改动' -Because '本机联调改了 consumer_enabled 却没提交,staging / prod 的真部署必须拒绝,豁免开关也不放行'
        Assert-Match -Text $message -Pattern '先提交或还原这两份文件再部署' -Because '拒绝文案给出出路'

        $printed = [string](Assert-PlayerInboxConsumerGate -SourceState $dirty 6>&1 3>&1 | Out-String -Width 4096)
        Assert-Match -Text $printed -Pattern '本次按工作区的值渲染' -Because 'dev 档与 DryRun 只告警'
        Assert-Match -Text $printed -Pattern 'head=0123456789ab worktree=dirty live=not-read OK' -Because '门禁行写明 HEAD 与工作区状态'

        $unknown = [pscustomobject]@{ Ok = $false; Head = ''; DirtyFiles = [string[]]@(); Reason = 'git 不在 PATH 里' }
        $message = ''
        try { Assert-PlayerInboxConsumerGate -SourceState $unknown -RequireCommittedSources $true 6>&1 3>&1 | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '^拒绝部署:判不出 bin/etc/game_config\.yaml / go/player_inbox/etc/player_inbox\.yaml 是否已提交\(git 不在 PATH 里\)' -Because '判不出来源时 staging / prod fail-closed'

        $clean = [pscustomobject]@{ Ok = $true; Head = '0123456789ab'; DirtyFiles = [string[]]@(); Reason = '' }
        $printed = [string](Assert-PlayerInboxConsumerGate -SourceState $clean -RequireCommittedSources $true 6>&1 3>&1 | Out-String -Width 4096)
        Assert-Match -Text $printed -Pattern 'head=0123456789ab worktree=clean live=not-read OK' -Because '两份 yaml 都已提交时放行'

        $message = ''
        try { Assert-PlayerInboxConsumerGate -RequireCommittedSources $true 6>&1 3>&1 | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '^内部错误' -Because '要求核对来源却没给 -SourceState,不能静默放行'
    }
    finally {
        Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
    }

    # Get-PlayerInboxGateSourceState 对仓库本身(只读 git):两份文件都被跟踪,HEAD 是 12 位短 sha;脏文件只可能是这两份之一。
    $RepoRoot = Get-RepoRoot
    $state = Get-PlayerInboxGateSourceState
    Assert-True -Condition ([bool]$state.Ok) -Because "仓库里两份 yaml 必须被 git 跟踪(Reason: $($state.Reason))"
    Assert-Match -Text $state.Head -Pattern '^[0-9a-f]{12,40}$' -Because 'Head 是至少 12 位的小写短 sha(--short=12 遇到前缀冲突会变长)'
    foreach ($file in @($state.DirtyFiles)) {
        Assert-True -Condition ($file -in @('bin/etc/game_config.yaml', 'go/player_inbox/etc/player_inbox.yaml')) -Because "DirtyFiles 只列这两份文件(实际 '$file')"
    }
}

# Read-PlayerInboxGateLiveFacts:在临时夹具上按给定命令 / 开关调用,读集群出口换成本函数作用域里的假 kubectl(按规则表第一条命中回应,
# 没命中即 throw —— DryRun 或「不需要读」时任何一次集群读都会让用例变红)。返回 { Facts; Calls; Error }。
function Invoke-PlayerInboxLiveFactsCase {
    param(
        [Parameter(Mandatory = $true)][string]$Root,
        [Parameter(Mandatory = $true)][string]$Command,
        [bool]$DryRun = $false,
        [bool]$SkipInfra = $false,
        [AllowEmptyString()][string]$GoSvcRegistry = 'registry.invalid/contract',
        [object[]]$Rules = @()
    )
    # 被测函数按动态作用域读这些脚本级参数(同名参数在 k8s_deploy.ps1 里的含义)。
    $RepoRoot = $Root
    $SkipGoSvc = $false
    $InfraNamespace = 'contract-infra'
    $NamespacePrefix = 'mmorpg-zone'
    $KubeContext = 'contract-inbox-ctx'
    $KubeConfig = ''
    $GoSvcCatalogue = @{ 'player-inbox' = @{ ConfigMap = 'go-svc-player-inbox-config'; ConfigFile = 'player_inbox.yaml' } }
    $calls = [System.Collections.Generic.List[string]]::new()
    function Invoke-ClientEntryKubectl {
        param(
            [Parameter(Mandatory = $true)][string[]]$KubectlArgs,
            [Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
            [Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig,
            [switch]$AllowFailure,
            [switch]$CaptureOutput
        )
        $line = $KubectlArgs -join ' '
        $calls.Add($line)
        foreach ($rule in $Rules) {
            if ($line -match $rule.Pattern) {
                return [pscustomobject]@{ ExitCode = [int]$rule.ExitCode; Stdout = [string]$rule.Stdout; Stderr = [string]$rule.Stderr }
            }
        }
        throw "假 kubectl:没有规则命中这次调用(本不该读集群?):kubectl $line"
    }
    $facts = $null
    $message = ''
    try { $facts = Read-PlayerInboxGateLiveFacts } catch { $message = $_.Exception.Message }
    return [pscustomobject]@{ Facts = $facts; Calls = @($calls); Error = $message }
}

Test-Case '玩家收件箱写前门禁:Read-PlayerInboxGateLiveFacts 按本次渲染哪一侧只读另一侧(DryRun 与不需要时不读集群;读不到 fail-closed;只认 -NamespacePrefix 下的 zone)' {
    $infraPattern = '^get configmap go-svc-player-inbox-config -n contract-infra --ignore-not-found -o json$'
    $zonesPattern = '^get configmap --all-namespaces --field-selector metadata\.name=node-config -o json$'
    $infraJson = @{
        apiVersion = 'v1'; kind = 'ConfigMap'; metadata = @{ name = 'go-svc-player-inbox-config'; namespace = 'contract-infra' }
        data = @{ 'player_inbox.yaml' = "Name: player_inbox`nPlayerInbox:`n  AdminTokenEnv: PLAYER_INBOX_ADMIN_TOKEN`n  EnqueueTypes:`n    - PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN`n" }
    } | ConvertTo-Json -Depth 6
    $zonesJson = @{
        items = @(
            @{ metadata = @{ name = 'node-config'; namespace = 'mmorpg-zone-a' }; data = @{ 'game_config.yaml' = "ZoneId: 1`nplayer_inbox:`n  consumer_enabled: true`n  periodic_fetch_minutes: 30`n" } }
            @{ metadata = @{ name = 'node-config'; namespace = 'mmorpg-zone-b' }; data = @{ 'game_config.yaml' = "ZoneId: 2`n" } }
            @{ metadata = @{ name = 'node-config'; namespace = 'other-app' }; data = @{ 'settings.yaml' = 'x: 1' } }
        )
    } | ConvertTo-Json -Depth 8
    $brokenZonesJson = @{
        items = @(@{ metadata = @{ name = 'node-config'; namespace = 'mmorpg-zone-a' }; data = @{ 'base_deploy_config.yaml' = 'x: 1' } })
    } | ConvertTo-Json -Depth 8

    $offRoot = New-PlayerInboxGateFixture -ConsumerLine 'consumer_enabled: false' -EnqueueLine 'EnqueueTypes: []'
    $onRoot = New-PlayerInboxGateFixture -ConsumerLine 'consumer_enabled: true' -EnqueueLine $inboxGateNonEmptyLine
    try {
        $r = Invoke-PlayerInboxLiveFactsCase -Root $offRoot -Command 'zone-up' -DryRun $true
        Assert-Equal -Expected '' -Actual $r.Error -Because 'DryRun 不应失败'
        Assert-Equal -Expected 0 -Actual $r.Calls.Count -Because 'DryRun 不读集群'
        Assert-Equal -Expected 'skipped(dry-run)' -Actual $r.Facts.Note -Because 'DryRun 写明跳过了线上核对'
        Assert-True -Condition ($null -eq $r.Facts.InfraEnqueueTypes -and $null -eq $r.Facts.ZoneConsumers) -Because 'DryRun 两侧都没读'

        $r = Invoke-PlayerInboxLiveFactsCase -Root $offRoot -Command 'zone-up' -Rules @(@{ Pattern = $infraPattern; ExitCode = 0; Stdout = $infraJson })
        Assert-Equal -Expected '' -Actual $r.Error -Because 'zone-up 渲染 false 时读 infra 线上的白名单'
        Assert-Equal -Expected 1 -Actual $r.Calls.Count -Because '只读 infra 那一份 ConfigMap'
        Assert-Equal -Expected 'PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN' -Actual (@($r.Facts.InfraEnqueueTypes) -join ',') -Because '线上白名单按块式列表解析'
        Assert-Equal -Expected 'infra_enqueue_types=1' -Actual $r.Facts.Note -Because 'Note 写明线上白名单项数'

        $r = Invoke-PlayerInboxLiveFactsCase -Root $offRoot -Command 'zone-up' -Rules @(@{ Pattern = $infraPattern; ExitCode = 0; Stdout = '' })
        Assert-True -Condition ($null -ne $r.Facts.InfraEnqueueTypes -and @($r.Facts.InfraEnqueueTypes).Count -eq 0) -Because 'ConfigMap 不存在(收件箱没部署)按空白名单算,但要记成「读过」'

        $r = Invoke-PlayerInboxLiveFactsCase -Root $offRoot -Command 'zone-up' -Rules @(@{ Pattern = $infraPattern; ExitCode = 1; Stdout = ''; Stderr = 'connection refused' })
        Assert-Match -Text $r.Error -Pattern '读取线上 namespace contract-infra 的 ConfigMap go-svc-player-inbox-config 失败\(kubectl 退出码 1\)' -Because '读不到不等于入箱关着:fail-closed'

        $r = Invoke-PlayerInboxLiveFactsCase -Root $offRoot -Command 'all-up'
        Assert-Equal -Expected 0 -Actual $r.Calls.Count -Because 'all-up 先部署全局 Go 服务时不读 infra:Apply-Infra 先写 yaml 的白名单,zone 才滚动'
        Assert-Equal -Expected 'not-needed' -Actual $r.Facts.Note -Because 'all-up 带全局 Go 服务、白名单为空时两侧都不用读'

        $r = Invoke-PlayerInboxLiveFactsCase -Root $offRoot -Command 'all-up' -SkipInfra $true -Rules @(@{ Pattern = $infraPattern; ExitCode = 0; Stdout = $infraJson })
        Assert-Equal -Expected 1 -Actual $r.Calls.Count -Because 'all-up -SkipInfra 不重写白名单,要读 infra 线上的值'

        $r = Invoke-PlayerInboxLiveFactsCase -Root $onRoot -Command 'zone-up'
        Assert-Equal -Expected 0 -Actual $r.Calls.Count -Because 'zone-up 渲染 true 时不用读集群'

        $r = Invoke-PlayerInboxLiveFactsCase -Root $onRoot -Command 'infra-up' -Rules @(@{ Pattern = $zonesPattern; ExitCode = 0; Stdout = $zonesJson })
        Assert-Equal -Expected '' -Actual $r.Error -Because 'infra-up 渲染非空白名单时读各 zone 线上的 node-config'
        Assert-Equal -Expected 'mmorpg-zone-a=true,mmorpg-zone-b=' -Actual ((@($r.Facts.ZoneConsumers) | ForEach-Object { "$($_.Namespace)=$($_.Value)" }) -join ',') -Because '只认 -NamespacePrefix 下的 namespace;缺键记空串'
        Assert-Equal -Expected 'zones_consumer_enabled=1/2' -Actual $r.Facts.Note -Because 'Note 写明开着的 zone 数'
        $RepoRoot = $onRoot
        $message = ''
        try { Assert-PlayerInboxConsumerGate -LiveFacts $r.Facts 6>&1 3>&1 | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern 'mmorpg-zone-b=absent' -Because '读到的事实交给门禁后,缺键的 zone 被拒绝并点名'

        $r = Invoke-PlayerInboxLiveFactsCase -Root $onRoot -Command 'infra-up' -GoSvcRegistry ''
        Assert-Equal -Expected 0 -Actual $r.Calls.Count -Because '没给 -GoSvcRegistry 就不部署白名单,不用读 zone'

        $r = Invoke-PlayerInboxLiveFactsCase -Root $onRoot -Command 'infra-up' -Rules @(@{ Pattern = $zonesPattern; ExitCode = 0; Stdout = $brokenZonesJson })
        Assert-Match -Text $r.Error -Pattern 'mmorpg-zone-a 的 ConfigMap node-config 没有 data\.game_config\.yaml' -Because 'zone 的 node-config 形状不对时 fail-closed'

        $r = Invoke-PlayerInboxLiveFactsCase -Root $onRoot -Command 'infra-up' -Rules @(@{ Pattern = $zonesPattern; ExitCode = 1; Stdout = ''; Stderr = 'forbidden' })
        Assert-Match -Text $r.Error -Pattern '读取各 zone 线上的 ConfigMap node-config 失败\(kubectl 退出码 1\)' -Because '列不出各 zone(例如没有跨 namespace 的读权限)时 fail-closed'
    }
    finally {
        Remove-Item -LiteralPath $offRoot -Recurse -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $onRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case 'LogQL 契约:player-inbox-alerts.yaml 的 scene 侧固定查询只用 C++ 日志 sidecar 真会打的流标签,统一写 {job="cpp_nodes", service="scene"}' {
    # C++ 业务日志只落文件,由 sidecar(Get-CppLogSidecarConfigAlloy)读文件送 Loki;流标签只有 external_labels、stage.labels、
    # stage.static_labels 三处给出的那些。用了别的标签(以前是 app=~"scene.*")的选择器匹配不到任何流,查询恒为空,值班会误判「没有问题」。
    # 只核对收件箱的告警文件;asset-channel-alerts.yaml / data-service-audit-alerts.yaml 有同样的 {app=...} 写法,改好之后加进来。
    $sidecar = Select-ManifestByName -Output $devOut -Name 'cpp-log-sidecar'
    Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($sidecar)) -Because '前提:zone-up DryRun 输出里有 sidecar 的 Alloy 配置'
    $labels = New-Object System.Collections.Generic.HashSet[string]
    $blocks = @([regex]::Matches($sidecar, '(?s)external_labels\s*=\s*\{(?<b>[^}]*)\}')) +
        @([regex]::Matches($sidecar, '(?s)stage\.(?:static_)?labels\s*\{\s*values\s*=\s*\{(?<b>[^}]*)\}'))
    foreach ($block in $blocks) {
        foreach ($m in [regex]::Matches($block.Groups['b'].Value, '(?<k>[A-Za-z_][A-Za-z0-9_]*)\s*=')) { [void]$labels.Add($m.Groups['k'].Value) }
    }
    foreach ($expected in @('job', 'service', 'zone', 'level')) {
        Assert-True -Condition ($labels.Contains($expected)) -Because "前提:sidecar 的流标签里有 ${expected}(实际:$(@($labels) -join ', '))"
    }
    Assert-True -Condition (-not $labels.Contains('app')) -Because 'sidecar 不打 app 标签;以后真加了它,本用例的前提就要重看'

    $alerts = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/player-inbox-alerts.yaml') -Raw
    $selectors = @([regex]::Matches($alerts, 'count_over_time\(\s*\{(?<sel>[^{}]*)\}'))
    Assert-True -Condition ($selectors.Count -ge 9) -Because "告警文件头的 scene 侧固定查询至少 9 条(实际 $($selectors.Count));少了说明写法变了、本用例抽不到"
    foreach ($selector in $selectors) {
        $text = $selector.Groups['sel'].Value
        foreach ($m in [regex]::Matches($text, '(?<k>[A-Za-z_][A-Za-z0-9_]*)\s*(?:=~|!~|!=|=)')) {
            Assert-True -Condition ($labels.Contains($m.Groups['k'].Value)) -Because "LogQL 选择器 {$text} 用了 sidecar 不打的标签 $($m.Groups['k'].Value)(sidecar 只有:$(@($labels) -join ', '))"
        }
        Assert-Match -Text $text -Pattern '^job="cpp_nodes", service="scene"' -Because "scene 侧选择器统一以 job=`"cpp_nodes`", service=`"scene`" 开头(实际 {$text})"
    }
}

# 包装入口的透传:只从 AST 取出 dev_tools.ps1 / k8s_image.ps1 里的透传函数(照 dev_tools_merge_zone_contract.tests.ps1),
# 把 $ScriptDir 指到临时目录里的假下游脚本 —— 不起 kubectl、不 build 镜像,只看下游收没收到、收到什么。
function Get-ToolScriptFunctionText {
    param(
        [Parameter(Mandatory = $true)][string]$ScriptName,
        [Parameter(Mandatory = $true)][string]$FunctionName
    )
    $path = Join-Path (Get-ToolsScriptsDir) $ScriptName
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($path, [ref]$null, [ref]$parseErrors)
    if ($parseErrors.Count) { throw "$ScriptName 解析失败: $($parseErrors | Out-String)" }
    $definition = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $FunctionName }, $true)
    if ($null -eq $definition) { throw "$ScriptName 里找不到函数 $FunctionName(契约测试的前提被改掉了)" }
    return $definition.Extent.Text
}
function Invoke-GateRouterModePassthrough {
    param(
        [Parameter(Mandatory = $true)][string]$SourceScript,
        [Parameter(Mandatory = $true)][string]$WrapperFunction,
        [Parameter(Mandatory = $true)][hashtable]$WrapperArgs,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Mode
    )
    $fakeDir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-gate-router-passthrough-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $fakeDir | Out-Null
    try {
        # 假下游刻意写成**简单脚本**(无 CmdletBinding / [Parameter]):包装函数 splat 过来的其余具名参数
        # 落进 $args,不会报"找不到参数";只把 GateRouterMode 的实收值打出来,没收到就是 <unset>。
        $fakeBody = 'param($GateRouterMode = "<unset>") "GateRouterMode=$GateRouterMode"'
        foreach ($downstream in @('k8s_deploy.ps1', 'k8s_image.ps1')) {
            Set-Content -LiteralPath (Join-Path $fakeDir $downstream) -Value $fakeBody -Encoding utf8
        }
        # 被测函数按动态作用域读调用方的 $ScriptDir / $GateRouterMode 等脚本级参数,这里放进本函数作用域;
        # 其余参数未定义即 $null,假下游不关心。k8s_image.ps1 的 Invoke-K8sDeploy 还要调 Get-ImageRef,给个替身。
        $ScriptDir = $fakeDir
        $GateRouterMode = $Mode
        function Get-ImageRef { return 'registry.invalid/test/mmorpg-node:0123456789ab' }
        . ([scriptblock]::Create((Get-ToolScriptFunctionText -ScriptName $SourceScript -FunctionName $WrapperFunction)))
        return (& $WrapperFunction @WrapperArgs | Out-String)
    }
    finally {
        Remove-Item -LiteralPath $fakeDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
Test-Case 'dev_tools / k8s_image 的 -GateRouterMode:留空不透传(由 k8s_deploy.ps1 默认值接管),显式给值才透传' {
    $wrappers = @(
        @{ Source = 'dev_tools.ps1'; Function = 'Invoke-K8sDeploy'; Args = @{ K8sCommand = 'zone-up' } },
        @{ Source = 'dev_tools.ps1'; Function = 'Invoke-K8sImage'; Args = @{ ImageCommand = 'release-zone' } },
        @{ Source = 'k8s_image.ps1'; Function = 'Invoke-K8sDeploy'; Args = @{ DeployCommand = 'zone-up' } }
    )
    foreach ($w in $wrappers) {
        $label = "$($w.Source) $($w.Function)"
        $unset = Invoke-GateRouterModePassthrough -SourceScript $w.Source -WrapperFunction $w.Function -WrapperArgs $w.Args -Mode ''
        Assert-Match -Text $unset -Pattern 'GateRouterMode=<unset>' -Because "$label 留空时不得透传:默认值只允许存在于 k8s_deploy.ps1 一处,透传空串还会被下游 ValidateSet 拒掉"
        $rollback = Invoke-GateRouterModePassthrough -SourceScript $w.Source -WrapperFunction $w.Function -WrapperArgs $w.Args -Mode '0'
        Assert-Match -Text $rollback -Pattern 'GateRouterMode=0' -Because "$label 必须能把回退值 0 透传下去,否则从包装入口无法回退到直连模式"
    }
    foreach ($target in @(@{ Name = 'dev_tools.ps1'; Command = 'help' }, @{ Name = 'k8s_image.ps1'; Command = 'list-refs' })) {
        $run = Invoke-ToolScript -ScriptName $target.Name -Arguments @('-Command', $target.Command, '-GateRouterMode', '2')
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "$($target.Name) 的 -GateRouterMode 只收 空 / 0 / 1,拼错必须在入口被拒"
        Assert-Match -Text $run.Output -Pattern 'GateRouterMode' -Because '不能把其他执行错误误判为参数校验成功'
    }
}
Test-Case 'scene(Deployment)就绪探针探 gRPC 口 50000 并声明该端口;Agones Fleet 不加任何 K8s 探针' {
    $block = Select-ManifestByName -Output $devOut -Name 'scene'
    Assert-Match -Text $block -Pattern 'kind: Deployment' -Because '必须检查 Deployment 模式的 scene'
    Assert-Match -Text $block -Pattern 'containerPort: 50000\s+name: grpc' -Because 'gRPC=TCP+30000,探针端口必须有对应声明'
    Assert-Match -Text $block -Pattern 'readinessProbe:\s+tcpSocket:\s+port: 50000\s+periodSeconds: 5\s+failureThreshold: 3' -Because 'C++ 尚无 grpc.health.v1,用实际监听的 gRPC 口'
    Assert-NotMatch -Text $block -Pattern 'livenessProbe|startupProbe' -Because '会杀容器的探针在没有启动耗时实测前不加'
    $fleet = Select-ManifestByName -Output $agonesOut -Name 'scene'
    Assert-Match -Text $fleet -Pattern 'kind: Fleet' -Because '必须检查真正的 Fleet'
    Assert-NotMatch -Text $fleet -Pattern 'livenessProbe|startupProbe|readinessProbe' -Because 'GameServer Pod 是 restartPolicy: Never,杀容器 = 销毁 GameServer;就绪交给 Agones SDK'
}
Test-Case 'node-config 与 battle-node-config 原样镜像 battle_rewards.json，Deployment 与 Agones 均不依赖被遮蔽的镜像配置' {
    $expected = ((Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'bin/etc/battle_rewards.json') -Raw -Encoding utf8) -replace "`r`n", "`n").TrimEnd()
    foreach ($case in @(
        @{ Label = 'zone Deployment'; Output = $devOut; Name = 'node-config' }
        @{ Label = 'zone Agones'; Output = $agonesOut; Name = 'node-config' }
        @{ Label = 'battle Deployment'; Output = $battleInfraRun.Output; Name = 'battle-node-config' }
        @{ Label = 'battle Agones'; Output = $battleZoneRedisFleetRun.Output; Name = 'battle-node-config' }
    )) {
        $block = Select-ManifestByName -Output $case.Output -Name $case.Name
        Assert-True -Condition ($null -ne $block) -Because "$($case.Label) 必须生成 $($case.Name)"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, '(?m)^  battle_rewards\.json:\s*\|').Count) -Because "$($case.Label) 的奖励配置只能出现一次"
        $actual = (Get-ConfigMapEmbeddedText -Block $block -FileName 'battle_rewards.json').TrimEnd()
        Assert-True -Condition ($actual -ceq $expected) -Because "$($case.Label) 的奖励 JSON 必须逐字等于 bin/etc/battle_rewards.json（仅归一化换行与块尾空行），不能另写一份额度或丢失策略配置"
        $null = $actual | ConvertFrom-Json -ErrorAction Stop
    }
}

Test-Case 'battle 配置必须满足独立票据密钥与权威连接上限' {
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $battleInfraRun.Output -Name 'battle-node-config')
    Assert-True -Condition ((Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.BattleTokenSecret') -ceq $BattleTestEnv.MMORPG_BATTLE_TOKEN_SECRET) -Because 'battle 密钥必须来自独立环境注入，测试不回显值'
    Assert-Equal -Expected (Get-EtcValue -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'BattleMaxConnections') -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.base_deploy_config.yaml.BattleMaxConnections') -Because '连接上限必须取权威配置'
    Assert-Match -Text (Select-ManifestByName -Output $battleInfraRun.Output -Name 'battle-node-config') -Pattern 'etcd.contract-battle-infra:2379' -Because 'battle 必须连接调用方指定的隔离 infra'
}
Test-Case 'battle-node-config 同样整块带 GrpcClient(battle 也是 C++ 节点,整目录挂载同样遮蔽镜像),且 infra-up 入口同样先过 deadline 预算门禁' {
    Assert-GrpcClientBlockMirrored -Output $battleInfraRun.Output -ConfigMapName 'battle-node-config'
    Assert-Match -Text $battleInfraRun.Output -Pattern 'GrpcClient deadline budget OK' -Because 'infra-up 与 zone-up 走同一个写路径门禁(Assert-GrpcClientDeadlineBudget)'
}
Test-Case 'BattleReplicas=0 不装配 battle，不删除现有池，也不要求 battle 密钥' {
    $env2 = @{} + $ProdEnv
    $env2['MMORPG_BATTLE_TOKEN_SECRET'] = ''
    $run = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-BattleReplicas','0','-ReleaseProfile','prod')) -Env $env2
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because '显式不装配时不能要求无关密钥'
    Assert-True -Condition ($null -eq (Select-ManifestByName -Output $run.Output -Name 'battle')) -Because '0 不生成 Deployment'
    Assert-True -Condition ($null -eq (Select-ManifestByName -Output $run.Output -Name 'battle-node-config')) -Because '0 不生成 battle 配置'
    Assert-NotMatch -Text $run.Output -Pattern 'delete (deployment|deploy) battle' -Because '不装配不等于销毁已运行战斗'
}
Test-Case '负向：prod battle 缺密钥必须在任何 apply 之前失败' {
    $env2 = @{} + $ProdEnv
    $env2['MMORPG_BATTLE_TOKEN_SECRET'] = ''
    $run = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-ReleaseProfile','prod')) -Env $env2
    Assert-True -Condition ($run.ExitCode -ne 0) -Because '缺 battle 密钥必须拒绝部署'
    Assert-Match -Text $run.Output -Pattern 'MMORPG_BATTLE_TOKEN_SECRET' -Because '失败必须准确点名新密钥'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl apply' -Because '密钥错误必须先于任何资源变更'
}
Test-Case '负向：battle 拒绝短密钥和与 gate 相同的密钥，即使发布档位是 dev' {
    foreach ($value in @('short', $BattleTestEnv.MMORPG_GATE_TOKEN_SECRET)) {
        $env2 = @{} + $BattleTestEnv
        $env2.MMORPG_BATTLE_TOKEN_SECRET = $value
        $run = Invoke-DeployDryRun -Arguments $BattleInfraArgs -Env $env2
        Assert-True -Condition ($run.ExitCode -ne 0) -Because 'K8s battle 运行时默认 prod，不能产出必然拒启的配置'
        Assert-Match -Text $run.Output -Pattern 'MMORPG_BATTLE_TOKEN_SECRET' -Because '错误必须点名配置键且不回显密钥值'
        Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl apply' -Because '失败必须发生于 apply 之前'
    }
}
Test-Case '负向：BattleReplicas 不能是负数' {
    $run = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-BattleReplicas','-1')) -Env $BattleTestEnv
    Assert-True -Condition ($run.ExitCode -ne 0) -Because '负数必须由入口参数校验拒绝'
    Assert-Match -Text $run.Output -Pattern 'BattleReplicas' -Because '不能把其他执行错误误判为参数校验成功'
}
Test-Case 'all-up 多区只装配一次 battle 全局池，并尊重副本参数' {
    $run = Invoke-DeployDryRun -Arguments @('-Command','all-up','-DryRun','-SkipPreflight','-SkipGoSvc','-SkipJavaSvc','-ZonesConfigPath',(Join-Path (Get-RepoRoot) 'deploy/k8s/zones.sample.json'),'-InfraNamespace','contract-battle-infra','-BattleReplicas','2','-NodeImage','registry.invalid/test/mmorpg-node:0123456789ab') -Env $BattleTestEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because '完整多区纯生成必须通过'
    $blocks = @(Get-ManifestBlocks -Output $run.Output | Where-Object { $_ -match '(?m)^\s*name:\s*battle\s*$' })
    Assert-Equal -Expected 1 -Actual $blocks.Count -Because 'battle 不按 zone 复制'
    $flat = ConvertTo-FlatManifest -Block $blocks[0]
    Assert-Equal -Expected 2 -Actual (Get-FlatValue -Flat $flat -KeyPath 'spec.replicas') -Because '显式副本数必须生效'
}

Test-Case 'Kafka 控制器必须经发布未就绪地址的 headless 自举，避免 Service readiness 死锁' {
    Assert-Match -Text $battleInfraRun.Output -Pattern 'name: KAFKA_CONTROLLER_QUORUM_VOTERS\s+value: "1@kafka-0\.kafka-headless\.contract-battle-infra\.svc\.cluster\.local:9093"' -Because '控制器地址不能使用只包含 Ready endpoint 的普通 Service'
    Assert-Match -Text $battleInfraRun.Output -Pattern '(?s)name: kafka-headless.*?publishNotReadyAddresses: true' -Because 'headless 必须在 broker 未 Ready 时发布 Pod 地址'
    Assert-NotMatch -Text $battleInfraRun.Output -Pattern 'value: "1@kafka:9093"' -Because '不得保留导致启动死锁的旧自举地址'
}
# ─────────────────────────────────────────────────────────────────
# Kafka 拓扑(docs/design/no-single-node-horizontal-scaling-20261001.md §2):broker 数决定的派生值
# (选举组成员表 / 副本数 / min.insync.replicas / PDB / podManagementPolicy)只在 k8s_deploy.ps1 的
# Get-KafkaTopologyFor 里算一次。纯函数按 AST 抽出来直接喂值(与上面 deadline 守卫同一做法),渲染结果用 DryRun 核对。
$kafkaTopologyFunctionNames = @('Get-KafkaTopologyFor', 'Get-KafkaTopology', 'Get-KafkaTopologyChangeVerdict')
foreach ($kafkaFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($kafkaTopologyFunctionNames -contains $kafkaFnAst.Name) {
        Set-Item -Path "Function:script:$($kafkaFnAst.Name)" -Value $kafkaFnAst.Body.GetScriptBlock()
    }
}
foreach ($kafkaFnName in $kafkaTopologyFunctionNames) {
    if (-not (Test-Path "Function:$kafkaFnName")) { throw "k8s_deploy.ps1 缺少函数 $kafkaFnName(Kafka 拓扑计算被删了?后续断言无意义)" }
}

Test-Case 'Kafka 拓扑:单 broker 与改造前逐字一致,≥3 个 broker 时选举组固定三票、副本数 3、min.insync 2' {
    $one = Get-KafkaTopologyFor -Brokers 1 -Namespace 'ns'
    Assert-Equal -Expected '1@kafka-0.kafka-headless.ns.svc.cluster.local:9093' -Actual $one.ControllerQuorumVoters -Because '单 broker 的成员表不能变:PVC 里的元数据日志是按它 format 的'
    Assert-Equal -Expected '1/1/1/1/OrderedReady' -Actual ('{0}/{1}/{2}/{3}/{4}' -f $one.ControllerCount, $one.ReplicationFactor, $one.MinInsyncReplicas, $one.PdbMinAvailable, $one.PodManagementPolicy) -Because 'OrderedReady 是缺省值,既有单 broker StatefulSet 的不可变字段不能被改到'

    $three = Get-KafkaTopologyFor -Brokers 3 -Namespace 'ns'
    Assert-Equal -Expected '1@kafka-0.kafka-headless.ns.svc.cluster.local:9093,2@kafka-1.kafka-headless.ns.svc.cluster.local:9093,3@kafka-2.kafka-headless.ns.svc.cluster.local:9093' -Actual $three.ControllerQuorumVoters -Because 'node.id = 序号 + 1,三票'
    Assert-Equal -Expected '3/3/2/2/Parallel' -Actual ('{0}/{1}/{2}/{3}/{4}' -f $three.ControllerCount, $three.ReplicationFactor, $three.MinInsyncReplicas, $three.PdbMinAvailable, $three.PodManagementPolicy) -Because '三份副本允许一份掉队;OrderedReady 会让选举组与 Pod 就绪互相等'

    $five = Get-KafkaTopologyFor -Brokers 5 -Namespace 'ns'
    Assert-Equal -Expected $three.ControllerQuorumVoters -Actual $five.ControllerQuorumVoters -Because '加 broker 不能改选举组:成员表变了就不是加副本,而是迁移选举组'
    Assert-Equal -Expected '3/3/2/4' -Actual ('{0}/{1}/{2}/{3}' -f $five.ControllerCount, $five.ReplicationFactor, $five.MinInsyncReplicas, $five.PdbMinAvailable) -Because '第 4 个起只当 broker;PDB 仍是一次最多驱逐一个'

    foreach ($bad in @(0, 2, -1)) {
        $thrown = ''
        try { Get-KafkaTopologyFor -Brokers $bad -Namespace 'ns' | Out-Null } catch { $thrown = $_.Exception.Message }
        Assert-Match -Text $thrown -Pattern '1 或 ≥3' -Because "broker 数 $bad 必须被拒:两票的选举组挂一个就没有多数派"
    }
}

Test-Case 'Kafka 拓扑:缺省 broker 数跟档位走(非 dev 发布档或非 custom 运维档 = 3),显式值优先' {
    $script:InfraNamespace = 'ns'
    foreach ($case in @(
        @{ Brokers = 0; Release = 'dev';     Ops = 'custom';        Want = 1 }
        @{ Brokers = 0; Release = 'staging'; Ops = 'custom';        Want = 3 }
        @{ Brokers = 0; Release = 'prod';    Ops = 'custom';        Want = 3 }
        @{ Brokers = 0; Release = 'dev';     Ops = 'managed-cloud'; Want = 3 }
        @{ Brokers = 0; Release = 'dev';     Ops = 'bare-metal';    Want = 3 }
        @{ Brokers = 1; Release = 'prod';    Ops = 'managed-cloud'; Want = 1 }
        @{ Brokers = 5; Release = 'dev';     Ops = 'custom';        Want = 5 }
    )) {
        $script:KafkaBrokers = $case.Brokers
        $script:ReleaseProfile = $case.Release
        $script:OpsProfile = $case.Ops
        Assert-Equal -Expected $case.Want -Actual (Get-KafkaTopology).Brokers -Because "KafkaBrokers=$($case.Brokers) release=$($case.Release) ops=$($case.Ops)"
    }
}

Test-Case 'Kafka broker 数变更:只放行「不变」与「已是多 broker 再加」,其余一律拒绝' {
    foreach ($case in @(
        @{ Live = $null; Want = 1; Verdict = 'absent' }
        @{ Live = $null; Want = 3; Verdict = 'absent' }
        @{ Live = 1; Want = 1; Verdict = 'unchanged' }
        @{ Live = 3; Want = 3; Verdict = 'unchanged' }
        @{ Live = 3; Want = 5; Verdict = 'scale-up' }
        @{ Live = 1; Want = 3; Verdict = 'refuse' }
        @{ Live = 3; Want = 1; Verdict = 'refuse' }
        @{ Live = 5; Want = 3; Verdict = 'refuse' }
        @{ Live = 0; Want = 1; Verdict = 'refuse' }
        @{ Live = 2; Want = 3; Verdict = 'refuse' }
    )) {
        $liveText = if ($null -eq $case.Live) { '<不存在>' } else { [string]$case.Live }
        Assert-Equal -Expected $case.Verdict -Actual (Get-KafkaTopologyChangeVerdict -LiveReplicas $case.Live -DesiredBrokers $case.Want) -Because "线上 $liveText → 目标 $($case.Want):1 ↔ ≥3 会改选举组成员表,缩容会摘掉仍持有副本的 broker"
    }
}

Test-Case 'dev 档 Kafka 清单仍是单 broker:副本数 1,占位全部替换' {
    $kafka = Select-ManifestByName -Output $battleInfraRun.Output -Name 'kafka'
    Assert-Match -Text $kafka -Pattern '(?m)^  replicas: 1\s*$' -Because 'dev 档默认单 broker'
    Assert-Match -Text $kafka -Pattern 'podManagementPolicy: OrderedReady' -Because '单 broker 必须渲染成缺省值,否则既有 StatefulSet apply 时不可变字段冲突'
    Assert-Match -Text $kafka -Pattern 'name: KAFKA_DEFAULT_REPLICATION_FACTOR\s+value: "1"' -Because '单 broker 只能有一份副本'
    Assert-Match -Text $kafka -Pattern 'name: KAFKA_MIN_INSYNC_REPLICAS\s+value: "1"' -Because '单 broker 的 min.insync 只能是 1'
    Assert-Match -Text $kafka -Pattern 'minAvailable: 1' -Because '单 broker 不允许自愿驱逐'
    Assert-NotMatch -Text $kafka -Pattern 'name: KAFKA_NODE_ID|name: KAFKA_ADVERTISED_LISTENERS' -Because '每个 Pod 不同的身份由启动脚本按序号导出,模板里写死就全体同号'
    $topicJob = Select-ManifestByName -Output $battleInfraRun.Output -Name 'kafka-topic-init'
    Assert-Match -Text $topicJob -Pattern 'name: REPLICATION_FACTOR\s+value: "1"' -Because '预建 topic 的副本数与 broker 数同源'
}

Test-Case '-KafkaBrokers 3:StatefulSet / 选举组 / 副本数 / PDB / 预建 Job 一起变' {
    $run = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-KafkaBrokers', '3')) -Env $BattleTestEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because '三 broker 的 DryRun 必须成功'
    Assert-Match -Text $run.Output -Pattern 'Kafka topology: brokers=3 controllers=3 replication_factor=3 min_insync_replicas=2' -Because '解析出的拓扑必须打出来,zone-up 与 infra-up 档位不一致时靠这行发现'
    $kafka = Select-ManifestByName -Output $run.Output -Name 'kafka'
    Assert-Match -Text $kafka -Pattern '(?m)^  replicas: 3\s*$' -Because '三个 broker'
    Assert-Match -Text $kafka -Pattern 'podManagementPolicy: Parallel' -Because 'OrderedReady 下 kafka-0 等多数派、kafka-1 等 kafka-0 Ready,互相等'
    Assert-Match -Text $kafka -Pattern 'name: KAFKA_CONTROLLER_QUORUM_VOTERS\s+value: "1@kafka-0\.kafka-headless\.contract-battle-infra\.svc\.cluster\.local:9093,2@kafka-1\.kafka-headless\.contract-battle-infra\.svc\.cluster\.local:9093,3@kafka-2\.kafka-headless\.contract-battle-infra\.svc\.cluster\.local:9093"' -Because '三票选举组,地址走发布未就绪地址的 headless'
    Assert-Match -Text $kafka -Pattern 'name: KAFKA_CONTROLLER_COUNT\s+value: "3"' -Because '启动脚本靠它决定谁兼任 controller、广播哪种地址'
    foreach ($name in @('KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR', 'KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR', 'KAFKA_DEFAULT_REPLICATION_FACTOR')) {
        Assert-Match -Text $kafka -Pattern ("name: $name\s+value: `"3`"") -Because "${name}:消费位点等内部 topic 只有一份时,倒一个 broker 照样全体消费者停摆"
    }
    Assert-Match -Text $kafka -Pattern 'name: KAFKA_MIN_INSYNC_REPLICAS\s+value: "2"' -Because '三份副本允许一份掉队,不允许只剩一份还照常收写'
    Assert-Match -Text $kafka -Pattern 'minAvailable: 2' -Because '一次最多自愿驱逐一个 broker'
    Assert-Match -Text $kafka -Pattern 'PLAINTEXT://\$\{POD_NAME\}\.kafka-headless\.contract-battle-infra\.svc\.cluster\.local:9092' -Because '多 broker 必须每个 Pod 广播自己的 DNS'
    # (?-i):占位是全大写。断言助手默认不分大小写,而 DryRun 输出里日志 sidecar 的 Alloy 配置有内置标签 __path__,
    # 不关掉大小写无关就会把它误判成未替换的占位(2026-10-08 实跑发现)。
    Assert-NotMatch -Text $run.Output -Pattern '(?-i)__[A-Z][A-Z0-9_]*__' -Because '占位必须全部替换'
    $topicJob = Select-ManifestByName -Output $run.Output -Name 'kafka-topic-init'
    Assert-Match -Text $topicJob -Pattern 'name: REPLICATION_FACTOR\s+value: "3"' -Because '命令 topic 与审计 topic 必须按三份建'
}

Test-Case '负向:-KafkaBrokers 2 在任何清单生成之前被拒' {
    $run = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-KafkaBrokers', '2')) -Env $BattleTestEnv
    Assert-True -Condition ($run.ExitCode -ne 0) -Because '两个 broker 的选举组没有容错,不能部署'
    Assert-Match -Text $run.Output -Pattern '1 或 ≥3' -Because '错误必须说明原因,而不是别的执行错误'
    Assert-NotMatch -Text $run.Output -Pattern 'BEGIN MANIFEST' -Because '参数非法时不能已经开始输出清单'
}

# etcd MVCC 压缩(P0-C 阶段 1b,docs/ops/etcd-maintenance-runbook.md):infra-up 必须把 etcd-maintenance.yaml 一起下发,
# 否则新环境漏 apply 就回到「历史只增不减、写满配额即 NOSPACE」。清单自身的形态与取值在 etcd_maintenance_contract.tests.ps1(静态,不跑部署脚本)。
Test-Case 'infra-up 产物里有 etcd-compact CronJob:namespace 占位已替换,标签不会被 etcd 的 selector 选中' {
    Assert-Equal -Expected 0 -Actual $battleInfraRun.ExitCode -Because "infra-up DryRun 必须成功。输出: $($battleInfraRun.Output)"
    $block = Select-ManifestByName -Output $battleInfraRun.Output -Name 'etcd-compact'
    Assert-True -Condition ($null -ne $block) -Because 'Apply-Infra 的 $infraManifests 必须带 etcd-maintenance.yaml'
    Assert-Match -Text $block -Pattern '(?m-i)^kind: CronJob\s*$' -Because 'etcd-compact 是 CronJob'
    Assert-Match -Text $block -Pattern 'http://etcd-0\.etcd-headless\.contract-battle-infra\.svc\.cluster\.local:2379' -Because '端点 FQDN 里的 __INFRA_NAMESPACE__ 必须替换成 -InfraNamespace(与 etcd.yaml 的广播地址同一条替换)'
    # (?-i):占位是全大写,理由同上面 -KafkaBrokers 3 用例。
    Assert-NotMatch -Text $block -Pattern '(?-i)__[A-Z][A-Z0-9_]*__' -Because '占位必须全部替换'
    Assert-NotMatch -Text $block -Pattern '(?m)^\s*app:\s*"?etcd"?\s*$' -Because 'app: etcd 会被 etcd 的 Service / PDB / StatefulSet 的 selector 选中'
}

# ─────────────────────────────────────────────────────────────────
# MySQL 连接总账(G2b:docs/handoff/launch-readiness-20261013/spec_G2b.md §2、§6.1)
# ─────────────────────────────────────────────────────────────────
# 所有 zone 的 go/db、data_service、Java gateway 与全局的 friend、trade 共用同一台 MySQL(deploy/k8s/manifests/infra/mysql.yaml)。
# 这笔账只有一份,在 k8s_deploy.ps1:Get-MysqlConnectionPlan(规划档)、Get-MysqlConnectionLedgerInputs(从权威文件读池上限 /
# 副本数 / max_connections)、Get-MysqlConnectionLedger(纯函数:稳态 / 滚动余量 / 峰值 / 余量)。这里按 AST 把它们连同依赖抽出来
# 直接调用(与上面 deadline 守卫、Kafka 拓扑同一做法),不另抄一份公式。读文件的那几个经 $RepoRoot 定位仓库,由调用方设好。
# 写路径入口的 Write-MysqlConnectionLedger 不在名单里:它依赖脚本参数(-Command / -ZoneId / -ZonesConfigPath),用 DryRun 输出核对。
# 连接池镜像门禁 Get-MysqlPoolMirror / Assert-MysqlPoolMirror 也在名单里:它们只经 Get-AuthoritativeScalar 与 $RepoRoot 读三份服务配置,
# 负向用例把 $RepoRoot 指向临时目录里的配置副本。
$mysqlLedgerFunctionNames = @(
    'Get-AuthoritativeScalar', 'Get-MysqlConnectionPlan', 'Get-MysqlMaxConnectionsFromManifest',
    'Get-ManifestDeploymentReplicas', 'Get-MysqlConnectionLedgerInputs', 'Get-MysqlConnectionLedger',
    'Get-MysqlPoolMirror', 'Assert-MysqlPoolMirror', 'Get-AccountDirectoryPoolMaxOpen'
)
foreach ($mysqlLedgerFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($mysqlLedgerFunctionNames -contains $mysqlLedgerFnAst.Name) {
        Set-Item -Path "Function:script:$($mysqlLedgerFnAst.Name)" -Value $mysqlLedgerFnAst.Body.GetScriptBlock()
    }
}
foreach ($mysqlLedgerFnName in $mysqlLedgerFunctionNames) {
    if (-not (Test-Path "Function:$mysqlLedgerFnName")) { throw "k8s_deploy.ps1 缺少函数 $mysqlLedgerFnName(MySQL 连接总账被删了?后续断言无意义)" }
}

# 用仓库权威文件的输入算一份总账。Z / E_z 由调用方给,其余形态参数(E_n / route store 是否 db_first / 滚动余量 / 运维余量)取规划档。
function Get-RepoMysqlConnectionLedger {
    param(
        [Parameter(Mandatory = $true)][System.Collections.IDictionary]$Plan,
        [Parameter(Mandatory = $true)][int]$ZoneCount,
        [Parameter(Mandatory = $true)][int]$ExtraZoneStores
    )
    # Get-MysqlConnectionLedgerInputs 及其依赖按动态作用域读这个变量(在 k8s_deploy.ps1 里它是脚本级变量)。
    $RepoRoot = Get-RepoRoot
    return Get-MysqlConnectionLedger -Inputs (Get-MysqlConnectionLedgerInputs) -ZoneCount $ZoneCount -ExtraZoneStores $ExtraZoneStores `
        -NonZoneStores $Plan.NonZoneStores -RouteStoreDbFirst $Plan.RouteStoreDbFirst `
        -RolloutExtraPodsPerDeployment $Plan.RolloutExtraPodsPerDeployment -OpsReserve $Plan.OpsReserve
}

# 把一份总账拍成一行 "键=值 键=值 …",断言失败时看分项用。
function Format-MysqlConnectionLedger {
    param([Parameter(Mandatory = $true)][System.Collections.IDictionary]$Ledger)
    return (@($Ledger.Keys | ForEach-Object { "$_=$($Ledger[$_])" }) -join ' ')
}

# DryRun 输出里「连这台 MySQL 的 ConfigMap」的名字(去重、排序)。一个 manifest 块可能是多文档(--- 分隔),逐文档看;
# 只看非注释行:模板注释里提到 MySQL 地址不算连它。地址认两种形状:Go 服务的 Host / Hosts "mysql.<ns>:3306"(与 MySQL 同
# namespace 时可以省掉 .<ns>,一并认)与 JDBC url。命中的文档读不出 metadata.name 就 throw(fail-closed:不能因为名字没读到而漏算一个)。
function Get-MysqlClientConfigMapNames {
    param([Parameter(Mandatory = $true)][string]$Output)

    $names = New-Object System.Collections.Generic.List[string]
    foreach ($block in (Get-ManifestBlocks -Output $Output)) {
        foreach ($doc in ($block -split '(?m)^---[ \t]*\r?$')) {
            if ($doc -notmatch '(?m)^kind:[ \t]*ConfigMap\s*$') { continue }
            $effective = @($doc -split "`n" | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
            if ($effective -notmatch 'mysql(?:\.[^\s:/]+)?:3306|tidb\.invalid:4000|jdbc:mysql://|DSNEnv: MMORPG_(MAIL|RANK)_SQL_DSN|DSNEnv: CHAT_AUTHORITY_DSN|DataSourceEnv: MMORPG_GUILD_SQL_DSN') { continue }
            # name 紧跟在 metadata: 之下(中间只允许注释行);生成器的 ConfigMap 都是这个形状。
            $name = [regex]::Match($doc, '(?m)^metadata:[ \t]*\r?\n(?:[ \t]*#[^\n]*\n)*[ ]{2}name:[ \t]*"?(?<name>[^\s"]+)"?\s*$')
            if (-not $name.Success) { throw "DryRun 输出里有一份连 MySQL 的 ConfigMap 读不出 metadata.name(形状变了?),无法核对它是否在连接总账里" }
            $names.Add($name.Groups['name'].Value)
        }
    }
    return @($names | Sort-Object -Unique)
}

Test-Case 'MySQL 连接总账:规划档峰值 ≤ mysql.yaml 的 max_connections' {
    # max_connections 是按规划档的峰值取的(mysql.yaml「连接上限」注释)。调大任何一个池、加副本、改规划档而不同步上限,
    # 都在这里点名失败,而不是等集群里出 1040。
    $plan = Get-MysqlConnectionPlan
    $ledger = Get-RepoMysqlConnectionLedger -Plan $plan -ZoneCount $plan.ZoneCount -ExtraZoneStores $plan.ExtraZoneStores
    Assert-True -Condition ($ledger.Peak -le $ledger.MaxConnections) -Because ("规划档(zones={0},每个 go/db 实例打开 {1} 个额外 zone 库)的峰值 {2} 超过 max_connections={3}:要么收紧池 / 规划档,要么同步抬高 deploy/k8s/manifests/infra/mysql.yaml 的 max_connections 与内存 limits。分项:{4}" -f $plan.ZoneCount, $plan.ExtraZoneStores, $ledger.Peak, $ledger.MaxConnections, (Format-MysqlConnectionLedger -Ledger $ledger))
}

Test-Case 'MySQL 连接总账:deploy/k8s/zones.json 的 zone 数(E_z=Z−1)峰值 ≤ max_connections' {
    # 仓库自带的 zones 配置(all-up 的默认输入)也必须装得下:Z 取 zones.json 的 zone 数,每个 go/db 实例按最坏情形打开其余
    # 全部 zone 的库(E_z = Z − 1),其余形态参数取规划档。往 zones.json 加 zone 加到超限时,这里先于部署失败。
    $zoneCount = @((Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/zones.json') -Raw | ConvertFrom-Json).zones).Count
    Assert-True -Condition ($zoneCount -gt 0) -Because "deploy/k8s/zones.json 里应当至少有一个 zone(本断言的前提),实际 $zoneCount 个"
    $plan = Get-MysqlConnectionPlan
    $ledger = Get-RepoMysqlConnectionLedger -Plan $plan -ZoneCount $zoneCount -ExtraZoneStores ($zoneCount - 1)
    Assert-True -Condition ($ledger.Peak -le $ledger.MaxConnections) -Because ("deploy/k8s/zones.json 的 {0} 个 zone(每个 go/db 实例打开其余 {1} 个 zone 的库)峰值 {2} 超过 max_connections={3}。分项:{4}" -f $zoneCount, ($zoneCount - 1), $ledger.Peak, $ledger.MaxConnections, (Format-MysqlConnectionLedger -Ledger $ledger))
}

Test-Case '自检:总账公式复现 2026-10-12 核算(历史Z=4 → 920、Z=2 → 500另加现役目录独立池),对旧 max_connections=500 判超限,输入非法即 throw' {
    $RepoRoot = Get-RepoRoot
    $directoryPool = Get-AccountDirectoryPoolMaxOpen
    # 合成输入 = 2026-10-12 手算那一版的口径(docs/PROGRESS.md「落点多库连接拓扑核算」):gateway 还是每副本 10 条,
    # max_connections 还是 500。不读仓库文件:本用例验的是公式本身,不该随服务配置的调整变红。每次取一份新的,下面有几处会改它。
    $newLegacyInputs = {
        [ordered]@{
            MaxConnections = 500
            DbReplicas = 2; DbHomeMaxOpen = 60; DbZoneStoreMaxOpen = 8; DbNonZoneStoreMaxOpen = 8
            DsReplicas = 2; DsAuditStorePools = 3; DsAuditMaxOpen = 5; DsPlayerNameMaxOpen = 20; DsDirectoryMaxOpen = $directoryPool; DsRouteMaxOpen = 20
            GatewayReplicas = 2; GatewayMaxPool = 10
            FriendReplicas = 2; FriendMaxOpen = 20; TradeReplicas = 2; TradeMaxOpen = 20
            # 2026-10-15 起全局池多了玩家收件箱(player-inbox,docs/design/player-inbox-service.md §10);当时的核算里没有它。
            PlayerInboxReplicas = 2; PlayerInboxMaxOpen = 20
            # 2026-10-16 起又多了帮会(guild 上 K8s,池上限同批补上);同样不在当时的核算里。
            GuildReplicas = 2; GuildMaxOpen = 20
            # 2026-10-16 邮件 mail 上 K8s(docs/design/mail-system.md §12.3);同样不在当时的核算里。
            MailReplicas = 2; MailMaxOpen = 20
            RankReplicas = 2; RankMaxOpen = 20
            ChatReplicas = 2; ChatMaxOpen = 16
        }
    }

    # 当时的名义值:不算额外落点库(E_z = E_n = 0)、route store 不计(F = 0)、不含滚动余量与运维余量。
    # 每 zone = 2 × (db 60 + ds 35 + gateway 10) = 210;全局 = friend 2 × 20 + trade 2 × 20 = 80。
    # 收件箱(2 × 20 = 40)是后加的全局池:断言按「2026-10-12 的核算 + 40」比,历史那份核算本身仍逐项复现。
    # 帮会(2 × 20 = 40)同理是 2026-10-16 后加的全局池,下面的断言一并加上;邮件(2 × 20 = 40)同日加入,同样一并加上。
    $inboxGlobal = 2 * 20 + 2 * 20 + 2 * 20 + 2 * 20 + 2 * 16
    $nominalShape = @{ ExtraZoneStores = 0; NonZoneStores = 0; RouteStoreDbFirst = $false; RolloutExtraPodsPerDeployment = 0; OpsReserve = 0 }
    foreach ($case in @(@{ Zones = 4; Steady = 920 }, @{ Zones = 2; Steady = 500 })) {
        $ledger = Get-MysqlConnectionLedger -Inputs (& $newLegacyInputs) -ZoneCount $case.Zones @nominalShape
        Assert-Equal -Expected ('60/{0}/10/{1}/{2}' -f (35 + $directoryPool), (210 + 2 * $directoryPool), (80 + $inboxGlobal)) -Actual ('{0}/{1}/{2}/{3}/{4}' -f $ledger.DbPerInstance, $ledger.DsPerInstance, $ledger.GatewayPerInstance, $ledger.PerZone, $ledger.Global) -Because "Z=$($case.Zones):db / ds / gateway 每实例、每 zone、全局(friend + trade + 收件箱) 五项"
        Assert-Equal -Expected ($case.Steady + $inboxGlobal + $case.Zones * 2 * $directoryPool) -Actual $ledger.Steady -Because "Z=$($case.Zones) 的稳态应为历史核算、现役全局池与每zone目录池总和"
        Assert-Equal -Expected ($case.Steady + $inboxGlobal + $case.Zones * 2 * $directoryPool) -Actual $ledger.Peak -Because '不含滚动余量与运维余量时,峰值 = 稳态'
        Assert-Equal -Expected (500 - $case.Steady - $inboxGlobal - $case.Zones * 2 * $directoryPool) -Actual $ledger.Headroom -Because '余量 = max_connections − 峰值(当时 Z=2 恰好为 0,加上收件箱后两者都为负)'
    }

    # 旧上限 500 对规划档必须判超限(峰值 > max_connections,余量为负)—— 这就是上限要跟着规划档走的原因。
    $plan = Get-MysqlConnectionPlan
    $planShape = @{
        ZoneCount = $plan.ZoneCount; ExtraZoneStores = $plan.ExtraZoneStores; NonZoneStores = $plan.NonZoneStores
        RouteStoreDbFirst = $plan.RouteStoreDbFirst; RolloutExtraPodsPerDeployment = $plan.RolloutExtraPodsPerDeployment; OpsReserve = $plan.OpsReserve
    }
    $overLimit = Get-MysqlConnectionLedger -Inputs (& $newLegacyInputs) @planShape
    Assert-True -Condition ($overLimit.Peak -gt 500 -and $overLimit.Headroom -lt 0) -Because ("规划档对旧上限 500 必须判超限,实际:{0}" -f (Format-MysqlConnectionLedger -Ledger $overLimit))

    # 滚动口径：R=2比R=0多2×所有域每实例池之和，额外含正规源读取的目录独立池。
    # 多出来的是「实例」(maxSurge 的新 Pod + 仍在优雅退出、连接还没释放的旧 Pod),不乘副本数,也不乘 zone 数。
    $rolloutShape = @{ ZoneCount = 4; ExtraZoneStores = 0; NonZoneStores = 0; RouteStoreDbFirst = $false; OpsReserve = 0 }
    $withoutRollout = Get-MysqlConnectionLedger -Inputs (& $newLegacyInputs) -RolloutExtraPodsPerDeployment 0 @rolloutShape
    $withRollout = Get-MysqlConnectionLedger -Inputs (& $newLegacyInputs) -RolloutExtraPodsPerDeployment 2 @rolloutShape
    Assert-Equal -Expected (2 * (241 + $directoryPool)) -Actual ($withRollout.Peak - $withoutRollout.Peak) -Because 'R=2 与 R=0 的峰值之差必须是 2 × (db + ds + gateway + friend + trade + 收件箱 + 帮会 + 邮件 每实例之和)'
    Assert-Equal -Expected (2 * (241 + $directoryPool)) -Actual $withRollout.RolloutSurge -Because '这个差值就是分项里的 RolloutSurge'
    Assert-Equal -Expected $withoutRollout.Steady -Actual $withRollout.Steady -Because '滚动余量不进稳态'

    # 逐项钉公式:副本数、池上限、形态参数各取互不相同的值,任何一项漏算、两个额外库旋钮接反、副本数错配,结果都对不上。
    #   db 每实例 = 60 + 2 × 7(zone 家族额外库)+ 1 × 11(非 zone 落点库)= 85
    #   ds 每实例 = 3 × 5 + 20 + 13(独立目录池) + 30(route store)= 78
    #   每zone=2×85+3×78+4×10=444；全局550含rank/chat；稳态3×444+550=1882。
    #   滚动255；峰值1882+255+7=2144；余量5000−2144=2856。
    $distinctInputs = [ordered]@{
        MaxConnections = 5000
        DbReplicas = 2; DbHomeMaxOpen = 60; DbZoneStoreMaxOpen = 7; DbNonZoneStoreMaxOpen = 11
        DsReplicas = 3; DsAuditStorePools = 3; DsAuditMaxOpen = 5; DsPlayerNameMaxOpen = 20; DsDirectoryMaxOpen = 13; DsRouteMaxOpen = 30
        GatewayReplicas = 4; GatewayMaxPool = 10
        FriendReplicas = 5; FriendMaxOpen = 20; TradeReplicas = 6; TradeMaxOpen = 40
        PlayerInboxReplicas = 7; PlayerInboxMaxOpen = 3
        GuildReplicas = 8; GuildMaxOpen = 2
        MailReplicas = 9; MailMaxOpen = 4
        RankReplicas = 10; RankMaxOpen = 6
        ChatReplicas = 11; ChatMaxOpen = 7
    }
    $distinct = Get-MysqlConnectionLedger -Inputs $distinctInputs -ZoneCount 3 -ExtraZoneStores 2 -NonZoneStores 1 -RouteStoreDbFirst $true -RolloutExtraPodsPerDeployment 1 -OpsReserve 7
    Assert-Equal -Expected '85/78/10/444/550/1882/255/7/2144/5000/2856' -Actual ('{0}/{1}/{2}/{3}/{4}/{5}/{6}/{7}/{8}/{9}/{10}' -f $distinct.DbPerInstance, $distinct.DsPerInstance, $distinct.GatewayPerInstance, $distinct.PerZone, $distinct.Global, $distinct.Steady, $distinct.RolloutSurge, $distinct.OpsReserve, $distinct.Peak, $distinct.MaxConnections, $distinct.Headroom) -Because '分项依次是 db / ds / gateway 每实例、每 zone、全局、稳态、滚动余量、运维余量、峰值、上限、余量'

    # 输入非法一律 throw 并点名是哪个键(fail-closed:不能拿一个猜出来的连接数去和上限比)。
    $missingKey = & $newLegacyInputs
    $missingKey.Remove('GatewayMaxPool')
    $zeroValue = & $newLegacyInputs
    $zeroValue['DbHomeMaxOpen'] = 0
    $notNumber = & $newLegacyInputs
    $notNumber['DsAuditMaxOpen'] = '5x'
    $zeroDirectory = & $newLegacyInputs
    $zeroDirectory['DsDirectoryMaxOpen'] = 0
    foreach ($bad in @(
        @{ Why = '缺键'; Key = 'GatewayMaxPool'; Inputs = $missingKey }
        @{ Why = '值为 0'; Key = 'DbHomeMaxOpen'; Inputs = $zeroValue }
        @{ Why = '值不是数字'; Key = 'DsAuditMaxOpen'; Inputs = $notNumber }
        @{ Why = '新增独立目录池不能为0'; Key = 'DsDirectoryMaxOpen'; Inputs = $zeroDirectory }
    )) {
        $message = ''
        try { Get-MysqlConnectionLedger -Inputs $bad.Inputs -ZoneCount 4 @nominalShape | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern ('MySQL 连接总账.*输入 {0}=' -f $bad.Key) -Because "$($bad.Why) 必须 throw 并点名 $($bad.Key),不能当成 0 条或某个默认值算下去"
    }

    # 形态参数越界同样 throw:滚动期间多出来的实例数不能是负数。
    $negativeRollout = @{} + $nominalShape
    $negativeRollout['RolloutExtraPodsPerDeployment'] = -1
    $message = ''
    try { Get-MysqlConnectionLedger -Inputs (& $newLegacyInputs) -ZoneCount 4 @negativeRollout | Out-Null } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'RolloutExtraPodsPerDeployment=-1\b.*不能为负' -Because 'R 传 -1 必须 throw 并点名参数与值,不能把滚动余量算成负数去抵稳态'
}

Test-Case '连 MySQL 的 ConfigMap 只有总账认识的那些(zone-up 三个、infra-up 五个:friend / trade / player-inbox / guild / mail)' {
    # 总账(Get-MysqlConnectionLedgerInputs)按服务逐个登记池上限。新增一个连这台 MySQL 的服务而不登记,它占的连接就不在账上,
    # 上面「峰值 ≤ max_connections」照样通过。这里把「谁在连」钉成名单:名单变了,先去总账加一项,再来改这里。
    $zoneExpected = @('go-svc-db-config', 'go-svc-data-service-config', 'java-svc-gateway-config' | Sort-Object) -join ','
    Assert-Equal -Expected $zoneExpected -Actual (@(Get-MysqlClientConfigMapNames -Output $devOut) -join ',') -Because 'zone-up 产物里连 MySQL 的 ConfigMap 必须恰好是 db / data-service / gateway 三个(总账的每 zone 部分只算了它们)'

    # 全局服务(friend / trade)只在 infra-up / all-up 的基础设施阶段生成(Apply-GlobalGoSvcManifests),zone-up 的产物里没有:
    # 只扫 zone-up 的话,新增一个连 MySQL 的全局服务能绕过总账而无人察觉。本文件其余 infra-up DryRun 都带 -SkipGoSvc,所以单独起一次:
    # 去掉 -SkipGoSvc、补上 -GoSvcRegistry;同时去掉 -WaitReady,免得 dev 档在 DryRun 里去走「等 trade-migrate / friend-migrate」那一段。
    $infraArgs = @($BattleInfraArgs | Where-Object { $_ -notin @('-SkipGoSvc', '-WaitReady') }) + @('-GoSvcRegistry', 'registry.invalid/test')
    Assert-True -Condition (($infraArgs -contains '-DryRun') -and -not ($infraArgs -contains '-SkipGoSvc')) -Because '夹具前提:这次 infra-up 必须仍是 DryRun(不碰任何集群),且不再跳过 Go 服务'
    $infraRun = Invoke-DeployDryRun -Arguments $infraArgs -Env $BattleTestEnv
    Assert-Equal -Expected 0 -Actual $infraRun.ExitCode -Because "带全局 Go 服务的 infra-up DryRun 必须成功。输出: $($infraRun.Output)"
    foreach ($authorityService in @('friend', 'guild', 'mail', 'chat')) {
        $authorityBlock = Select-ManifestByName -Output $infraRun.Output -Name "go-svc-$authorityService-config"
        $authorityFlat = ConvertTo-FlatManifest -Block $authorityBlock
        $authorityPrefix = "data.$authorityService.yaml."
        Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $authorityFlat -KeyPath ($authorityPrefix + 'GlobalLoginSessions')) -Because "$authorityService 必须声明真实全局登录态，不沿用本地占位false"
        Assert-Match -Text (Get-FlatValue -Flat $authorityFlat -KeyPath ($authorityPrefix + 'AccountRoleSchema')) -Pattern '^[a-z][a-z0-9_]{0,63}$' -Because "$authorityService 必须显式指定全局角色目录"
        Assert-Match -Text (Get-FlatValue -Flat $authorityFlat -KeyPath ($authorityPrefix + 'LoginRedis.Host')) -Pattern '^redis\..+:6379$' -Because "$authorityService 必须连接部署的全局login Redis，不能空地址"
        Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.RedisClient.DB') -Actual (Get-FlatValue -Flat $authorityFlat -KeyPath ($authorityPrefix + 'LoginRedis.DB')) -Because "$authorityService 原会话账号读取必须与login同DB"
    }
    $infraExpected = @('go-svc-friend-config', 'go-svc-trade-config', 'go-svc-player-inbox-config', 'go-svc-guild-config', 'go-svc-mail-config', 'go-svc-rank-config', 'go-svc-chat-config' | Sort-Object) -join ','
    Assert-Equal -Expected $infraExpected -Actual (@(Get-MysqlClientConfigMapNames -Output $infraRun.Output) -join ',') -Because 'infra-up 产物里连 MySQL 的 ConfigMap 必须恰好是 friend / trade / player-inbox / guild / mail 五个(总账的全局部分只算了它们)'
    $friendConfig = Select-ManifestByName -Output $infraRun.Output -Name 'go-svc-friend-config'
    Assert-Match -Text $friendConfig -Pattern 'RequireTiDB: true' -Because 'dev同样要求正式TiDB，不能回到MySQL权威'

    # 玩家收件箱的 ConfigMap 形状(docs/design/player-inbox-service.md §1.4、§1.6):运维口令只写**变量名**,口令值不进 ConfigMap;
    # 端口 / 指标端口与 manifest 一致;自己的 Etcd.Key 留空(D-13);入箱白名单显式写出(上线顺序 §11.7)。
    $inboxConfig = Select-ManifestByName -Output $infraRun.Output -Name 'go-svc-player-inbox-config'
    Assert-Match -Text $inboxConfig -Pattern 'AdminTokenEnv: PLAYER_INBOX_ADMIN_TOKEN' -Because 'ConfigMap 只写口令所在的环境变量名,值经 Secret player-inbox-admin-token 注入'
    Assert-NotMatch -Text $inboxConfig -Pattern '(?m)^\s+AdminToken\s*:' -Because '口令值绝不能写进 ConfigMap(dev 档同样不带口令)'
    Assert-Match -Text $inboxConfig -Pattern 'ListenOn: 0\.0\.0\.0:50950' -Because '端口 50950 与 manifests/go-svc/player-inbox.yaml、go_services.ps1 一致(D14)'
    Assert-Match -Text $inboxConfig -Pattern 'MetricsListenAddr: ":9270"' -Because '指标端口 9270 与 manifest 的 prometheus.io/port 一致(D14)'
    Assert-Match -Text $inboxConfig -Pattern 'Key: ""' -Because '全局服务自己的 go-zero 注册 Key 必须留空(D-13)'
    Assert-Match -Text $inboxConfig -Pattern 'Key: dataservice\.rpc' -Because '作为调用方经 dataservice.rpc 发现 data_service(玩家存在校验 / 对账读账本)'
    Assert-Match -Text $inboxConfig -Pattern 'EnqueueTypes:' -Because '入箱白名单必须显式写出(空也写 [])'
    Assert-Match -Text $inboxConfig -Pattern 'AutoMigrate: ' -Because '建表策略显式写出,staging/prod 固定 false'

    # 顶层 Middlewares 段(评审 L12):go-zero 的 Middlewares.Stat 缺省 true(整段缺席也回填),Go 侧 config.Validate 在 Stat 开着时
    # 要求 StatConf.IgnoreContentMethods 同时列出 FetchItems 与 AckItems,缺了 player-inbox 与 player-inbox-migrate Job 都拒启。
    # ConfigMap 里整段复制自服务 yaml:键集合与值逐项相同,两个方法名必须在顶层段(ConfigMap 内缩进 4 格)下出现。
    Assert-Match -Text $inboxConfig -Pattern '(?m)^    Middlewares:\s*$' -Because '必须有顶层 Middlewares 段(DataServiceRpc.Middlewares 是客户端的,不算)'
    foreach ($method in @('/player_inbox.PlayerInbox/FetchItems', '/player_inbox.PlayerInbox/AckItems')) {
        Assert-Match -Text $inboxConfig -Pattern ('(?m)^\s+-\s+' + [regex]::Escape($method) + '\s*$') -Because "IgnoreContentMethods 必须含 $method(否则 config.Validate 拒启)"
    }
    $authoritativeInbox = (ConvertFrom-YamlToFlatMap -Text (Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'go/player_inbox/etc/player_inbox.yaml') -Raw)).Scalars
    $expectedMiddlewareKeys = @($authoritativeInbox.Keys | Where-Object { $_ -like 'Middlewares.*' })
    Assert-True -Condition ($expectedMiddlewareKeys -contains 'Middlewares.StatConf.IgnoreContentMethods[0]') -Because 'go/player_inbox/etc/player_inbox.yaml 必须写出 Middlewares.StatConf.IgnoreContentMethods(本断言的前提)'
    $renderedInbox = ConvertTo-UnprefixedScalars -Scalars (ConvertTo-FlatManifest -Block $inboxConfig).Scalars -Prefix 'data.player_inbox.yaml.'
    $renderedMiddlewareKeys = @($renderedInbox.Keys | Where-Object { $_ -like 'Middlewares.*' })
    Assert-Equal -Expected ($expectedMiddlewareKeys -join ',') -Actual ($renderedMiddlewareKeys -join ',') -Because 'go-svc-player-inbox-config 的 Middlewares 键集合必须与服务 yaml 逐项相同(整段复制,不增不减)'
    foreach ($key in $expectedMiddlewareKeys) {
        Assert-Equal -Expected $authoritativeInbox[$key] -Actual $renderedInbox[$key] -Because "go-svc-player-inbox-config 的 $key 必须与 go/player_inbox/etc/player_inbox.yaml 一致"
    }
    # 迁移 Job 读的是同一份 ConfigMap(conf.MustLoad 同样跑 Validate),漏了它就是「服务起得来、Job 拒启」或反过来。
    $migrateManifest = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/go-svc/player-inbox-migrate.yaml') -Raw
    Assert-Match -Text $migrateManifest -Pattern 'configMap:\s+name: go-svc-player-inbox-config' -Because 'player-inbox-migrate Job 必须挂同一份 ConfigMap,Middlewares 段对它同样生效'
    # M2/M3 使用唯一的服务 YAML 配置源，不恢复旧 M1 的 token / Redis / MySQL 字段。
    $mailConfig = Select-ManifestByName -Output $infraRun.Output -Name 'go-svc-mail-config'
    Assert-True -Condition ($null -ne $mailConfig) -Because '全局池必须包含邮件服务'
    $mailFlat = ConvertTo-UnprefixedScalars -Scalars (ConvertTo-FlatManifest -Block $mailConfig).Scalars -Prefix 'data.mail.yaml.'
    foreach ($key in @('Name','Timeout','DSNEnv','RequireTiDB','MaxOpenConns','AutoMigrate','AssetSecretEnv',
        'Mail.MaxPersonalMailsPerPlayer','Mail.MaxActiveSystemMails','Mail.MaxPersonalMailTtlDays','Mail.MaxSystemMailTtlDays',
        'Sweep.Mode','Sweep.Interval','Sweep.RetentionDays','Sweep.BatchLimit')) {
        Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/mail/etc/mail.yaml' -KeyPath $key) -Actual ([string]$mailFlat[$key]).Trim('"') -Because "现役 mail 配置镜像同一事实源: $key"
    }
    Assert-Match -Text $mailConfig -Pattern 'DSNEnv: MMORPG_MAIL_SQL_DSN' -Because 'SQL 凭证只通过环境变量引用'
    Assert-NotMatch -Text $mailConfig -Pattern '(?m)^\s+AdminToken:' -Because '不恢复 M1 单一管理令牌'
}

Test-Case '部署写路径打印 MySQL connection ledger(zone-up DryRun)' {
    # 部署的人靠这一行看「这次部署之后连接预算还剩多少」。行里的数字用 AST 抽出的同一组函数重算后比对,不抄数字:
    # 调池 / 调规划档不需要来改本用例;打印的那份与契约测试钉住的那份分了家,才会在这里失败。
    $plan = Get-MysqlConnectionPlan
    $planLedger = Get-RepoMysqlConnectionLedger -Plan $plan -ZoneCount $plan.ZoneCount -ExtraZoneStores $plan.ExtraZoneStores
    # 基线 DryRun($BaseArgs)不带 -ZonesConfigPath,读的是 deploy/k8s/zones.json;Z = 其中的全部 zone ∪ 本次 -ZoneId(基线是 101)。
    # 现状:zones.json 是 101、102 两个 zone,101 已在其中,所以 Z = 2。E_z 取 Z − 1,其余形态参数取规划档。
    $zoneIds = @(@((Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/zones.json') -Raw | ConvertFrom-Json).zones | ForEach-Object { [int]$_.zoneId }) + @(101) | Sort-Object -Unique)
    $deployLedger = Get-RepoMysqlConnectionLedger -Plan $plan -ZoneCount $zoneIds.Count -ExtraZoneStores ($zoneIds.Count - 1)

    $ledgerLines = @($devOut -split "`r?`n" | Where-Object { $_ -match '^\s*MySQL connection ledger: zones=' })
    Assert-Equal -Expected 1 -Actual $ledgerLines.Count -Because 'zone-up 写路径入口必须恰好打印一行 MySQL connection ledger(Write-MysqlConnectionLedger)'
    $ledgerLine = [string]$ledgerLines[0]
    $deployPart = 'MySQL connection ledger: zones={0} steady={1} peak={2} max_connections={3} headroom={4};' -f $zoneIds.Count, $deployLedger.Steady, $deployLedger.Peak, $deployLedger.MaxConnections, $deployLedger.Headroom
    $planPart = '; plan zones={0} peak={1} headroom={2}' -f $plan.ZoneCount, $planLedger.Peak, $planLedger.Headroom
    Assert-Match -Text $ledgerLine -Pattern ('^\s*' + [regex]::Escape($deployPart)) -Because '本次部署那一份(zone 数 / 稳态 / 峰值 / 上限 / 余量)必须与总账函数按同一组输入算出的值一致'
    # 行尾锚定:zones 配置在场时不该带 zones_config=missing 的尾巴。
    Assert-Match -Text $ledgerLine -Pattern ([regex]::Escape($planPart) + '\s*$') -Because '规划档那一份(zone 数 / 峰值 / 余量)必须与 Get-MysqlConnectionPlan 算出的值一致,且行尾没有 zones_config=missing'

    $firstKubectlAt = $devOut.IndexOf('[dry-run] kubectl')
    Assert-True -Condition ($firstKubectlAt -lt 0 -or $devOut.IndexOf('MySQL connection ledger:') -lt $firstKubectlAt) -Because '总账在写路径入口打印,先于任何 kubectl 写操作'
}

Test-Case '负向:zones.10zones.yaml 时打 [MySQLConnectionLedger]OverLimit 警告且不阻断' {
    # 10 个 zone 的样例配置在单台 MySQL 上装不下(额外 zone 库的池随 zone 数平方增长)。超限只告警、不拒绝部署
    # (是否升级为部署门禁待拍板,见 Write-MysqlConnectionLedger 注释)。
    # -ZonesConfigPath 给绝对路径:子进程的工作目录不保证是仓库根(与上面 all-up 用例同一写法)。
    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-ZonesConfigPath', (Join-Path (Get-RepoRoot) 'deploy/k8s/zones.10zones.yaml'), '-SkipGoSvc', '-SkipJavaSvc'))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "连接总账超限只告警、不阻断部署。输出: $($run.Output)"
    # 只断警告开头不含空格的那一段:Write-Warning 若被 ConsoleHost 按窗口宽度折行,折点在空格处(先例见下面 gateway 管理面口令的占位警告)。
    Assert-Match -Text $run.Output -Pattern '\[MySQLConnectionLedger\]OverLimit:' -Because '峰值超过 max_connections 时必须明说,不能只留一个负的 headroom 让人自己看'
    # $BaseArgs 是 zone-up:Z = 配置里的 10 个 zone ∪ 本次 -ZoneId 101(不在配置里也并入:这个 zone 确实要部署)= 11。
    Assert-Match -Text $run.Output -Pattern 'MySQL connection ledger: zones=11 steady=\d+ peak=\d+ max_connections=\d+ headroom=-\d+;' -Because '本次部署那一份按 11 个 zone 算,余量为负'
    Assert-Match -Text $run.Output -Pattern '--- BEGIN MANIFEST ---' -Because '不阻断:告警之后清单照常生成'
    Assert-NotMatch -Text $devOut -Pattern '\[MySQLConnectionLedger\]OverLimit:' -Because '对照:仓库 zones.json 与规划档都在上限内,基线 DryRun 不该打这条警告(否则本条区分不出「超限才告警」)'
}

# 下面三例共用 zones.10zones.yaml 及其 zoneId。本套测试不带 YAML 解析器:按 Get-ZonesFromJson 的 YAML 回退解析的写法
# (键独占一行、行尾不带注释)读 zone 级 zoneId 行。-ZonesConfigPath 给绝对路径,理由同上。
# 文件读不到时得到空列表,由各例的夹具前提点名失败,不在脚本顶层 throw 掉整套测试。
$tenZonesConfigPath = Join-Path (Get-RepoRoot) 'deploy/k8s/zones.10zones.yaml'
$tenZonesRaw = [string](Get-Content -LiteralPath $tenZonesConfigPath -Raw -ErrorAction SilentlyContinue)
$tenZoneIds = @([regex]::Matches($tenZonesRaw, '(?m)^[ \t]+zoneId:[ \t]*(\d+)[ \t]*\r?$') | ForEach-Object { [int]$_.Groups[1].Value } | Sort-Object -Unique)

Test-Case 'all-up(zones.10zones.yaml):总账只数 zones 配置,zones=10 不并入 -ZoneId,峰值仍超限并打 [MySQLConnectionLedger]OverLimit' {
    # all-up / infra-up 不为 -ZoneId 部署 zone 级工作负载(db / data-service / gateway):-ZoneId 在这两条命令里用作全局 Go 服务与
    # battle 的 ZoneId、mysql-init-sql 预建库的输入,本身不产生每 zone 的 MySQL 连接,不论缺省值 101 还是显式传入都不并入。
    # 以前按 Get-InfraZoneIds 的口径把它并进来,zones 配置不含它时 Z 凭空多 1,可能打出虚假的 OverLimit
    # (见 Write-MysqlConnectionLedger 注释)。这里显式传一个不在配置里的 -ZoneId(取缺省值 101):
    # all-up 不论缺省还是显式都不并入它;显式传是为了不依赖脚本缺省值 —— 缺省值哪天改成配置里已有的 zone,不传就区分不出新旧口径。
    # 只部署 C++ 节点:-SkipInfra(不装配 battle 全局池,免得还要 battle 密钥)、-SkipGoSvc、-SkipJavaSvc;dev 档不走发布预检。
    Assert-Equal -Expected 10 -Actual $tenZoneIds.Count -Because "夹具前提:zones.10zones.yaml 应当有 10 个互不相同的 zoneId(实际 $($tenZoneIds -join ','))"
    Assert-True -Condition ($tenZoneIds -notcontains 101) -Because '夹具前提:zones.10zones.yaml 不含 101,否则并不并入 -ZoneId 结果相同,本例区分不出新旧口径'
    $plan = Get-MysqlConnectionPlan
    $deployLedger = Get-RepoMysqlConnectionLedger -Plan $plan -ZoneCount $tenZoneIds.Count -ExtraZoneStores ($tenZoneIds.Count - 1)
    Assert-True -Condition ($deployLedger.Peak -gt $deployLedger.MaxConnections) -Because ("夹具前提:10 个 zone 的峰值必须超过 max_connections,否则「仍打 OverLimit」无从谈起。分项:{0}" -f (Format-MysqlConnectionLedger -Ledger $deployLedger))

    $allUpArgs = @('-Command', 'all-up', '-DryRun', '-SkipInfra', '-SkipGoSvc', '-SkipJavaSvc', '-ZonesConfigPath', $tenZonesConfigPath, '-ZoneId', '101')
    Assert-True -Condition ($allUpArgs -contains '-DryRun') -Because '夹具前提:all-up 必须是 DryRun(不带它会对本机当前 kubectl context 真执行 apply)'
    $run = Invoke-DeployDryRun -Arguments $allUpArgs
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "连接总账超限只告警、不阻断 all-up。输出: $($run.Output)"
    $ledgerLines = @($run.Output -split "`r?`n" | Where-Object { $_ -match '^\s*MySQL connection ledger: zones=' })
    Assert-Equal -Expected 1 -Actual $ledgerLines.Count -Because 'all-up 写路径入口必须恰好打印一行 MySQL connection ledger'
    $deployPart = 'MySQL connection ledger: zones={0} steady={1} peak={2} max_connections={3} headroom={4};' -f $tenZoneIds.Count, $deployLedger.Steady, $deployLedger.Peak, $deployLedger.MaxConnections, $deployLedger.Headroom
    Assert-Match -Text ([string]$ledgerLines[0]) -Pattern ('^\s*' + [regex]::Escape($deployPart)) -Because 'all-up 本次部署那一份只数 zones 配置的 10 个 zone(E_z = 9),不是并入 -ZoneId 101 之后的 11 个'
    Assert-NotMatch -Text ([string]$ledgerLines[0]) -Pattern 'zones_config=missing' -Because 'zones 配置在场,行尾不该带缺席标记'
    # 只断警告开头不含空格的那一段(理由同上例)。
    Assert-Match -Text $run.Output -Pattern '\[MySQLConnectionLedger\]OverLimit:' -Because '10 个 zone 的峰值仍超过 max_connections,修口径不能顺手把真实超限的告警也去掉'
    Assert-Match -Text $run.Output -Pattern '--- BEGIN MANIFEST ---' -Because '不阻断:告警之后清单照常生成'
}

Test-Case '对照 zone-up(zones.10zones.yaml):-ZoneId 不在配置里也并入总账,zones=11' {
    # 与上例同一份 zones 配置,只换命令:zone-up 部署的就是 -ZoneId 那个 zone,不在配置里(例如新开的区还没登记)也要算进去。
    # 取 11 而不是缺省值 101:上面「负向:zones.10zones.yaml…」那例用的是 $BaseArgs 的 101(同样 zones=11),
    # 两例合起来说明并入的是本次 -ZoneId,不是认 101 这个值。
    $outsideZoneId = 11
    Assert-True -Condition ($tenZoneIds.Count -gt 0 -and $tenZoneIds -notcontains $outsideZoneId) -Because "夹具前提:zones.10zones.yaml 读得出 zoneId,且不含 $outsideZoneId(实际 $($tenZoneIds -join ','))"
    $plan = Get-MysqlConnectionPlan
    $zoneCount = $tenZoneIds.Count + 1
    $deployLedger = Get-RepoMysqlConnectionLedger -Plan $plan -ZoneCount $zoneCount -ExtraZoneStores ($zoneCount - 1)

    $zoneUpArgs = @('-Command', 'zone-up', '-ZoneName', 'contract-test', '-ZoneId', [string]$outsideZoneId, '-DryRun', '-SkipGoSvc', '-SkipJavaSvc', '-ZonesConfigPath', $tenZonesConfigPath)
    $run = Invoke-DeployDryRun -Arguments $zoneUpArgs
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "连接总账超限只告警、不阻断 zone-up。输出: $($run.Output)"
    $ledgerLines = @($run.Output -split "`r?`n" | Where-Object { $_ -match '^\s*MySQL connection ledger: zones=' })
    Assert-Equal -Expected 1 -Actual $ledgerLines.Count -Because 'zone-up 写路径入口必须恰好打印一行 MySQL connection ledger'
    $deployPart = 'MySQL connection ledger: zones={0} steady={1} peak={2} max_connections={3} headroom={4};' -f $zoneCount, $deployLedger.Steady, $deployLedger.Peak, $deployLedger.MaxConnections, $deployLedger.Headroom
    Assert-Match -Text ([string]$ledgerLines[0]) -Pattern ('^\s*' + [regex]::Escape($deployPart)) -Because "zone-up 本次部署那一份 = zones 配置的 $($tenZoneIds.Count) 个 zone ∪ -ZoneId $outsideZoneId = $zoneCount 个(E_z = $($zoneCount - 1))"
}

Test-Case 'infra-up(zones.10zones.yaml):总账只数 zones 配置,zones=10 不并入 -ZoneId;预建库照旧并入 -ZoneId' {
    # 与 all-up 例同一口径,但走 infra-up 那一支(all-up 例带 -SkipInfra,覆盖不到这里)。现有的 $battleInfraRun 读默认 zones.json(101、102),
    # 而 -ZoneId 缺省也是 101,并不并入结果都是 zones=2,区分不出新旧口径;所以换成不含 101 的 zones.10zones.yaml,并显式传 -ZoneId 101
    # (理由同 all-up 例)。复用 $BattleInfraArgs / $BattleTestEnv:infra-up 要装配 battle 全局池,需要 battle / gate 票据密钥。
    Assert-Equal -Expected 10 -Actual $tenZoneIds.Count -Because "夹具前提:zones.10zones.yaml 应当有 10 个互不相同的 zoneId(实际 $($tenZoneIds -join ','))"
    Assert-True -Condition ($tenZoneIds -notcontains 101) -Because '夹具前提:zones.10zones.yaml 不含 101,否则并不并入 -ZoneId 结果相同,本例区分不出新旧口径'
    Assert-True -Condition (($BattleInfraArgs -contains '-DryRun') -and -not ($BattleInfraArgs -contains '-ZoneId') -and -not ($BattleInfraArgs -contains '-ZonesConfigPath')) -Because '夹具前提:infra-up 必须是 DryRun(不带它会对本机当前 kubectl context 真执行 apply),且 $BattleInfraArgs 自己不带 -ZoneId / -ZonesConfigPath(重复传参会让子进程直接失败)'
    $plan = Get-MysqlConnectionPlan
    $deployLedger = Get-RepoMysqlConnectionLedger -Plan $plan -ZoneCount $tenZoneIds.Count -ExtraZoneStores ($tenZoneIds.Count - 1)
    Assert-True -Condition ($deployLedger.Peak -gt $deployLedger.MaxConnections) -Because ("夹具前提:10 个 zone 的峰值必须超过 max_connections,否则「仍打 OverLimit」无从谈起。分项:{0}" -f (Format-MysqlConnectionLedger -Ledger $deployLedger))

    $run = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-ZonesConfigPath', $tenZonesConfigPath, '-ZoneId', '101')) -Env $BattleTestEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "连接总账超限只告警、不阻断 infra-up。输出: $($run.Output)"
    $ledgerLines = @($run.Output -split "`r?`n" | Where-Object { $_ -match '^\s*MySQL connection ledger: zones=' })
    Assert-Equal -Expected 1 -Actual $ledgerLines.Count -Because 'infra-up 写路径入口必须恰好打印一行 MySQL connection ledger'
    $deployPart = 'MySQL connection ledger: zones={0} steady={1} peak={2} max_connections={3} headroom={4};' -f $tenZoneIds.Count, $deployLedger.Steady, $deployLedger.Peak, $deployLedger.MaxConnections, $deployLedger.Headroom
    Assert-Match -Text ([string]$ledgerLines[0]) -Pattern ('^\s*' + [regex]::Escape($deployPart)) -Because 'infra-up 本次部署那一份只数 zones 配置的 10 个 zone(E_z = 9),不是并入 -ZoneId 101 之后的 11 个'
    Assert-NotMatch -Text ([string]$ledgerLines[0]) -Pattern 'zones_config=missing' -Because 'zones 配置在场,行尾不该带缺席标记'
    # 只断警告开头不含空格的那一段(理由同上面「负向:zones.10zones.yaml…」例)。
    Assert-Match -Text $run.Output -Pattern '\[MySQLConnectionLedger\]OverLimit:' -Because '10 个 zone 的峰值仍超过 max_connections,修口径不能顺手把真实超限的告警也去掉'
    # 对照:mysql-init-sql 预建库的口径(Get-InfraZoneIds = zones 配置 ∪ -ZoneId)有意不同,照旧预建 -ZoneId 的库,留给之后的 zone-up -ZoneId。
    # 仓库静态的 deploy/mysql-init/00_init_zone_dbs.sql 只建 zone_1_db / zone_2_db,且 CREATE 里库名不带反引号,不会冒充这一行。
    $initSql = Select-ManifestByName -Output $run.Output -Name 'mysql-init-sql'
    Assert-Match -Text $initSql -Pattern ([regex]::Escape('CREATE DATABASE IF NOT EXISTS `zone_101_db`;')) -Because 'infra-up 的 mysql-init-sql 仍要预建 -ZoneId 101 的库:总账不并入 -ZoneId,不等于预建库也不并入'
}

Test-Case 'zones 配置缺席(zone-up DryRun):总账按 zones=1 算、行尾标 zones_config=missing,不打 Get-InfraZoneIds 的 WARNING' {
    # Write-MysqlConnectionLedger 的缺席分支(spec_G2b §5):Z 取 1(只有 -ZoneId),行尾带 zones_config=missing,且不调 Get-InfraZoneIds ——
    # 它那条 WARNING 讲的是「mysql-init-sql 只预建 -ZoneId 的库」,出现在 zone-up 的输出里是误导。
    # -ZonesConfigPath 给临时目录下一个不存在的绝对路径(只造路径、不建文件);zone-up 不在别处读 zones 配置。
    $missingZonesPath = Join-Path ([System.IO.Path]::GetTempPath()) ('zones-config-missing-' + [guid]::NewGuid().ToString('N') + '.json')
    Assert-True -Condition (-not (Test-Path -LiteralPath $missingZonesPath)) -Because '夹具前提:zones 配置路径必须不存在'
    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SkipGoSvc', '-SkipJavaSvc', '-ZonesConfigPath', $missingZonesPath))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "zones 配置缺席不该阻断 zone-up。输出: $($run.Output)"
    $ledgerLines = @($run.Output -split "`r?`n" | Where-Object { $_ -match '^\s*MySQL connection ledger: zones=' })
    Assert-Equal -Expected 1 -Actual $ledgerLines.Count -Because '写路径入口必须恰好打印一行 MySQL connection ledger'
    Assert-Match -Text ([string]$ledgerLines[0]) -Pattern 'MySQL connection ledger: zones=1 ' -Because 'zones 配置缺席时本次部署那一份只算 -ZoneId 一个 zone'
    Assert-Match -Text ([string]$ledgerLines[0]) -Pattern 'zones_config=missing\s*$' -Because '缺席要在行尾标出来,部署的人才知道这份账没算其它 zone'
    Assert-NotMatch -Text $run.Output -Pattern 'Zones configuration missing' -Because '缺席分支不调 Get-InfraZoneIds,不该出现它那条讲 mysql-init-sql 预建库的 WARNING'
}

# 连接池镜像门禁(Assert-MysqlPoolMirror)读的全部文件。夹具用例把它们拷到临时目录再逐项改坏,不碰仓库。
$mysqlPoolMirrorFixtureFiles = @(
    'go/db/etc/db.yaml'
    'go/data_service/etc/data_service.yaml'
    'java/gateway_node/src/main/resources/application.yaml'
)

# 夹具:把临时副本里某个键路径(与 release_common.ps1 ConvertFrom-YamlToFlatMap 同一套点分路径)的值改成 $Value,只改那一行。
# 路径按同一套缩进栈规则解析,改的就是 Get-AuthoritativeScalar 会读到的那一行;恰好一处才改,改完再用 Get-YamlScalar 读回核对。
# 任一步不成立就 throw:没改成的负向断言是空转。被改那一行的行尾注释随之丢掉,不影响判定。
function Set-FixtureYamlScalar {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$KeyPath,
        [Parameter(Mandatory = $true)][string]$Value
    )
    $lines = @([System.IO.File]::ReadAllText($Path) -split "`r?`n")
    $stack = New-Object System.Collections.Generic.List[object]
    $hits = New-Object System.Collections.Generic.List[int]
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ([string]::IsNullOrWhiteSpace($lines[$i])) { continue }
        $stripped = Remove-YamlInlineComment -Value ($lines[$i] -replace "`t", '    ')
        if ([string]::IsNullOrWhiteSpace($stripped)) { continue }
        $indent = $stripped.Length - $stripped.TrimStart(' ').Length
        $content = $stripped.Trim()
        if ($content.StartsWith('---') -or $content.StartsWith('...')) { continue }
        # 与解析器同序:序列元素只弹栈不入栈;既不是元素也不是「键:」的行不动栈。
        $isSequenceItem = $content.StartsWith('- ') -or $content -eq '-'
        if (-not $isSequenceItem -and -not [regex]::IsMatch($content, '^[^:]+:')) { continue }
        while ($stack.Count -gt 0 -and $stack[$stack.Count - 1].Indent -ge $indent) { $stack.RemoveAt($stack.Count - 1) }
        if ($isSequenceItem) { continue }
        $key = ConvertFrom-YamlScalarLiteral -Value ([regex]::Match($content, '^(?<k>[^:]+):').Groups['k'].Value)
        $stack.Add(@{ Indent = $indent; Key = $key })
        if ((@($stack | ForEach-Object { $_.Key }) -join '.') -ceq $KeyPath) { $hits.Add($i) }
    }
    if ($hits.Count -ne 1) { throw "夹具:$Path 里键路径 $KeyPath 应恰好 1 处,实际 $($hits.Count) 处" }
    $lines[$hits[0]] = [regex]::Replace($lines[$hits[0]], '^(?<head>[^:]+:).*$', ('${head} ' + $Value))
    [System.IO.File]::WriteAllText($Path, ($lines -join "`n"), [System.Text.UTF8Encoding]::new($false))
    $readBack = Get-YamlScalar -Path $Path -KeyPath $KeyPath
    if (-not $readBack.Found -or $readBack.Value -cne $Value) { throw "夹具:$Path 的 $KeyPath 没有改成 '$Value'(读回 '$($readBack.Value)')" }
}

Test-Case '负向:ConfigMap 要镜像的 MySQL 池参数不合格时,入口门禁 Assert-MysqlPoolMirror throw 并点名文件与键路径(夹具是临时目录里的配置副本,不碰仓库)' {
    # spec_G2b §5:池参数不合格要在任何 apply 之前 throw,不留半截部署。Go 服务的空闲数下限是 1(0 在 Go 侧 = 没配、取默认),
    # Hikari minimum-idle 的下限是 0;ExtraStoreConnMaxIdleSeconds 必须为正(Go 侧把 ≤0 当成没写)。每次只改一个键,其余保持仓库原值。
    # 两个「超过上限」的值按仓库当前上限 + 1 取:调池上限不需要来改本用例。
    $routeStoreMaxOpen = [int](Get-EtcValue -RelativePath 'go/data_service/etc/data_service.yaml' -KeyPath 'RouteStore.MaxOpenConn')
    $hikariMaxPool = [int](Get-EtcValue -RelativePath 'java/gateway_node/src/main/resources/application.yaml' -KeyPath 'spring.datasource.hikari.maximum-pool-size')
    $tempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('mysql-pool-mirror-' + [guid]::NewGuid().ToString('N'))
    $resetFixture = {
        foreach ($rel in $mysqlPoolMirrorFixtureFiles) {
            $dst = Join-Path $tempRoot $rel
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $dst) | Out-Null
            Copy-Item -LiteralPath (Join-Path (Get-RepoRoot) $rel) -Destination $dst -Force
        }
    }
    try {
        # Get-AuthoritativeScalar 按动态作用域读 $RepoRoot(在 k8s_deploy.ps1 里是脚本级变量)。
        $RepoRoot = $tempRoot
        # 前提:副本不改就该放行。否则下面几条负向断言分不清是谁造成的失败。
        & $resetFixture
        Assert-MysqlPoolMirror

        foreach ($bad in @(
            @{ Why = 'RouteStore 空闲数超过上限'; File = 'go/data_service/etc/data_service.yaml'; KeyPath = 'RouteStore.MaxIdleConn'; Value = [string]($routeStoreMaxOpen + 1) }
            @{ Why = 'PlayerName 上限为 0'; File = 'go/data_service/etc/data_service.yaml'; KeyPath = 'PlayerName.MaxOpenConn'; Value = '0' }
            @{ Why = 'zone 家族额外库空闲数为 0(Go 侧下限 1)'; File = 'go/db/etc/db.yaml'; KeyPath = 'Placement.ZoneStoreMaxIdleConn'; Value = '0' }
            @{ Why = '额外库空闲连接回收秒数为 0'; File = 'go/db/etc/db.yaml'; KeyPath = 'Placement.ExtraStoreConnMaxIdleSeconds'; Value = '0' }
            @{ Why = 'Hikari minimum-idle 为负(下限 0)'; File = 'java/gateway_node/src/main/resources/application.yaml'; KeyPath = 'spring.datasource.hikari.minimum-idle'; Value = '-1' }
            @{ Why = 'Hikari minimum-idle 超过 maximum-pool-size'; File = 'java/gateway_node/src/main/resources/application.yaml'; KeyPath = 'spring.datasource.hikari.minimum-idle'; Value = [string]($hikariMaxPool + 1) }
            # 2026-10-15:Hikari connection-timeout 下限 250(0 = 无限等,低于 250 拒启)。
            @{ Why = 'Hikari connection-timeout 为 0(Hikari 当成无限等)'; File = 'java/gateway_node/src/main/resources/application.yaml'; KeyPath = 'spring.datasource.hikari.connection-timeout'; Value = '0' }
            @{ Why = 'Hikari connection-timeout 低于 250'; File = 'java/gateway_node/src/main/resources/application.yaml'; KeyPath = 'spring.datasource.hikari.connection-timeout'; Value = '249' }
        )) {
            & $resetFixture
            Set-FixtureYamlScalar -Path (Join-Path $tempRoot $bad.File) -KeyPath $bad.KeyPath -Value $bad.Value
            $message = ''
            try { Assert-MysqlPoolMirror } catch { $message = $_.Exception.Message }
            Assert-Match -Text $message -Pattern 'MySQL 连接池镜像' -Because "$($bad.Why):入口门禁必须 throw(fail-closed),不能把非法池值镜像进 ConfigMap"
            Assert-Match -Text $message -Pattern ([regex]::Escape("$($bad.File) 的 $($bad.KeyPath)='$($bad.Value)'")) -Because "$($bad.Why):错误必须点名文件、键路径与值"
        }

        # 对照:Hikari minimum-idle=0 是合法值(不保留空闲连接),门禁放行并原样镜像 0。与上面 Go 服务的下限 1 一起,两个下限都钉住。
        & $resetFixture
        Set-FixtureYamlScalar -Path (Join-Path $tempRoot 'java/gateway_node/src/main/resources/application.yaml') -KeyPath 'spring.datasource.hikari.minimum-idle' -Value '0'
        Assert-MysqlPoolMirror
        $gatewayMirror = Get-MysqlPoolMirror -Target 'gateway'
        Assert-Equal -Expected 0 -Actual $gatewayMirror['spring.datasource.hikari.minimum-idle'] -Because 'Hikari minimum-idle=0 必须放行,并原样镜像 0'
    }
    finally {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case '写路径入口:Assert-MysqlPoolMirror 挂在 zone-up / all-up / infra-up 门禁块里,先于 switch ($Command)' {
    # 生成器取值时会再核一次,但那已经在 Apply-Zone 中途(node-config / gate / scene 都 apply 完了)。上面的负向用例只证明门禁函数会 throw,
    # 这里钉住它确实挂在写路径入口、且先于任何集群写操作(写操作都在 switch ($Command) 的分支里)。与 Write-MysqlConnectionLedger 的先后无关。
    $writeGates = @($deadlineDeployAst.EndBlock.Statements | Where-Object {
        $_ -is [System.Management.Automation.Language.IfStatementAst] -and
        $_.Clauses[0].Item1.Extent.Text -match '^\$Command\s+-in\s+@\(\s*"zone-up"\s*,\s*"all-up"\s*,\s*"infra-up"\s*\)$'
    })
    Assert-Equal -Expected 1 -Actual $writeGates.Count -Because 'k8s_deploy.ps1 顶层应当恰好有一个 if ($Command -in @("zone-up", "all-up", "infra-up")) 写路径门禁块'
    $mirrorCalls = @($writeGates[0].Clauses[0].Item2.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] -and $a.GetCommandName() -eq 'Assert-MysqlPoolMirror' }, $true))
    Assert-Equal -Expected 1 -Actual $mirrorCalls.Count -Because '写路径门禁块里必须调用一次 Assert-MysqlPoolMirror'
    $commandSwitches = @($deadlineDeployAst.EndBlock.Statements | Where-Object {
        $_ -is [System.Management.Automation.Language.SwitchStatementAst] -and $_.Condition.Extent.Text -match '^\(?\s*\$Command\s*\)?$'
    })
    Assert-Equal -Expected 1 -Actual $commandSwitches.Count -Because 'k8s_deploy.ps1 顶层应当恰好有一个 switch ($Command)'
    Assert-True -Condition ($mirrorCalls[0].Extent.StartOffset -lt $commandSwitches[0].Extent.StartOffset) -Because 'Assert-MysqlPoolMirror 必须先于 switch ($Command)(Apply-Infra / Apply-Zone 的集群写操作都在它的分支里)'
}

Test-Case 'mysql-connection-alerts.yaml:PrometheusRule 形状,额外库数阈值与 Get-MysqlConnectionPlan 同源' {
    $alertsRelativePath = 'deploy/k8s/mysql-connection-alerts.yaml'
    $alertsPath = Join-Path (Get-RepoRoot) $alertsRelativePath
    Assert-True -Condition (Test-Path -LiteralPath $alertsPath) -Because "$alertsRelativePath 应当存在(MySQL 连接预算与连接池的告警规则)"
    # 只看非注释行,统一成 LF 后按行锚定:文件头与各条规则上方的注释里也会提到规则名、severity 与阈值,不能让注释把断言喂饱。
    $effective = @((Get-Content -LiteralPath $alertsPath -Raw) -split "`r?`n" | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
    Assert-Match -Text $effective -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 owner-epoch-alerts.yaml 同一安装方式)'
    Assert-Match -Text $effective -Pattern '(?m)^metadata:[ ]*\n[ ]+name:[ ]*mysql-connection-alerts[ ]*$' -Because 'metadata.name 必须是 mysql-connection-alerts'

    $alerts = [regex]::Matches($effective, '(?m)^[ ]*- alert:[ ]*(?<name>\S+)')
    Assert-Equal -Expected 5 -Actual $alerts.Count -Because ("应当恰好 5 条规则(MySQL 服务端侧 2 条 + 客户端连接池侧 3 条),实际:{0}" -f (@($alerts | ForEach-Object { $_.Groups['name'].Value }) -join ', '))
    for ($i = 0; $i -lt $alerts.Count; $i++) {
        $bodyEnd = if ($i + 1 -lt $alerts.Count) { $alerts[$i + 1].Index } else { $effective.Length }
        $body = $effective.Substring($alerts[$i].Index, $bodyEnd - $alerts[$i].Index)
        Assert-Match -Text $body -Pattern '(?m)^[ ]+severity:[ ]*\S+' -Because "规则 $($alerts[$i].Groups['name'].Value) 必须带 labels.severity(严重级约定见文件头)"
    }

    # DbExtraStorePoolsBeyondPlan 的两个阈值就是规划档的额外库数:规划档改了而阈值没跟(或反过来),这条告警要么恒响、要么该响不响。
    # 提取依赖告警文件「每个阈值单独占一行,形如 pool="extra_zone"}) > 3」的写法(文件里该规则上方的注释写明了这条约定)。
    # 把表达式改写成别的形状会让本条变红 —— 有意为之:要么保持这个形状,要么同步改这里的提取方式。
    $plan = Get-MysqlConnectionPlan
    foreach ($threshold in @(
        @{ Pool = 'extra_zone'; PlanKey = 'ExtraZoneStores' }
        @{ Pool = 'extra_non_zone'; PlanKey = 'NonZoneStores' }
    )) {
        $thresholdPattern = 'pool="' + [regex]::Escape($threshold.Pool) + '"\}\) > (?<n>\d+)'
        $found = [regex]::Matches($effective, $thresholdPattern)
        Assert-Equal -Expected 1 -Actual $found.Count -Because "$alertsRelativePath 里 pool=`"$($threshold.Pool)`" 的阈值应当恰好出现 1 处(形如 pool=`"$($threshold.Pool)`"}) > N,单独占一行),实际 $($found.Count) 处"
        Assert-Equal -Expected $plan[$threshold.PlanKey] -Actual $found[0].Groups['n'].Value -Because "pool=`"$($threshold.Pool)`" 的阈值必须等于 Get-MysqlConnectionPlan 的 $($threshold.PlanKey):改规划档必须同改告警阈值(反之亦然),并重算 max_connections"
    }
}

# ─────────────────────────────────────────────────────────────────
# Redis 连接总账与重启闸(P0-E 第 2 批:docs/handoff/global-singletons-p0-findings-20261014.md「## P0-E」修复设计五)
# ─────────────────────────────────────────────────────────────────
# 全服共连一个共享 Redis(deploy/k8s/manifests/infra/redis.yaml)。MAXCLIENTS 本身的静态契约(恰好定义一次、两条 exec 都带、
# compose 同值、启动时打印 fd 限值)在 redis_maxclients_contract.tests.ps1;这里钉 k8s_deploy.ps1 里的那笔账与重启闸。
# 纯函数按 AST 抽出来直接调用(它们依赖的 Get-MysqlConnectionPlan / Get-ManifestDeploymentReplicas 已由上面 MySQL 一节抽出),
# Assert-RedisCoreRestartConfirmed 读集群的那一步(Get-LiveRedisCoreTemplateDigest)在用例里给替身;写路径打印与 DryRun 产物用
# $devOut(zone-up)/ $battleInfraRun(infra-up)核对;包装入口的透传照上面 GateRouterMode 的做法。全部不碰任何集群。
$redisLedgerFunctionNames = @(
    'Get-RedisConnectionPlan', 'Get-RedisMaxClientsFromManifest', 'Get-RedisPoolMirror', 'Assert-DbOwnerEpochRedisDeployment', 'Get-RedisConnectionLedgerInputs', 'Get-RedisConnectionLedger',
    'Get-RedisPodTemplateDigest', 'Get-RedisCoreRestartVerdict', 'Assert-RedisCoreRestartConfirmed', 'Get-LiveRedisCoreTemplateDigest',
    # 没有 sts 与旧 Deployment 时再查孤儿 redis-<n> Pod(2026-10-19,半截的 1 → 3 切换不再被当成全新集群)。
    'Get-LiveRedisOrphanPodOrdinals'
)
foreach ($redisLedgerFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($redisLedgerFunctionNames -contains $redisLedgerFnAst.Name) {
        Set-Item -Path "Function:script:$($redisLedgerFnAst.Name)" -Value $redisLedgerFnAst.Body.GetScriptBlock()
    }
}
foreach ($redisLedgerFnName in $redisLedgerFunctionNames) {
    if (-not (Test-Path "Function:$redisLedgerFnName")) { throw "k8s_deploy.ps1 缺少函数 $redisLedgerFnName(Redis 连接总账 / 重启闸被删了?后续断言无意义)" }
}

# k8s_deploy.ps1 参数块里某个 [int] 参数的缺省值(按 AST 读,不抄数字)。找不到或没有缺省值即 throw。
function Get-K8sDeployIntParamDefault {
    param([Parameter(Mandatory = $true)][string]$Name)
    $found = @($deadlineDeployAst.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq $Name })
    if ($found.Count -ne 1 -or $null -eq $found[0].DefaultValue) { throw "k8s_deploy.ps1 的参数块里找不到带缺省值的 -$Name" }
    return [int]$found[0].DefaultValue.Extent.Text
}

# 用仓库权威文件的输入按规划档算一份 Redis 总账(Z、C++ 台数、滚动余量、运维余量、告警比例全部取 Get-RedisConnectionPlan)。
function Get-RepoRedisPlanLedger {
    # Get-RedisConnectionLedgerInputs 及其依赖按动态作用域读这个变量(在 k8s_deploy.ps1 里它是脚本级变量)。
    $RepoRoot = Get-RepoRoot
    $plan = Get-RedisConnectionPlan
    # 规划档按 RouteStore.Mode=db_first 记 D1 对账 leader 的独立池(Get-RedisConnectionPlan 的 RouteStoreDbFirst,与 MySQL 规划档同源)。
    return Get-RedisConnectionLedger -Inputs (Get-RedisConnectionLedgerInputs -RouteStoreDbFirst ([bool]$plan.RouteStoreDbFirst)) -ZoneCount $plan.ZoneCount `
        -CppZoneProcesses ($plan.ZoneCount * ($plan.GatePerZone + $plan.ScenePerZone)) -BattleProcesses $plan.BattleProcesses `
        -RolloutExtraPodsPerDeployment $plan.RolloutExtraPodsPerDeployment -OpsReserve $plan.OpsReserve -WarnRatio $plan.WarnRatio
}

# DryRun 输出里 kind: StatefulSet、metadata.name 为 redis 的那一份文档(redis.yaml 是多文档,Select-ManifestByName 只按名字取第一块)。
function Select-RedisStatefulSetDocument {
    param([Parameter(Mandatory = $true)][string]$Output)
    $docs = @(foreach ($block in (Get-ManifestBlocks -Output $Output)) {
            foreach ($doc in (($block -replace "`r`n", "`n") -split '(?m)^---[ \t]*$')) {
                if ($doc -cmatch '(?m)^kind:[ \t]*StatefulSet[ \t]*$' -and $doc -cmatch '(?m)^  name:[ \t]*redis[ \t]*$') { $doc }
            }
        })
    if ($docs.Count -ne 1) { throw "DryRun 输出里 StatefulSet redis 应当恰好 1 份,实际 $($docs.Count) 份" }
    return $docs[0]
}

# 各分项拍成一行,断言失败时看分项用。
function Format-RedisConnectionLedger {
    param([Parameter(Mandatory = $true)][System.Collections.IDictionary]$Ledger)
    return (@($Ledger.Keys | ForEach-Object { "$_=$($Ledger[$_])" }) -join ' ')
}

Test-Case 'Redis 连接总账:规划档峰值 ≤ 80% × redis.yaml 的 MAXCLIENTS;规划 zone 数与滚动余量与 MySQL 总账同源,C++ 台数等于参数缺省值' {
    $plan = Get-RedisConnectionPlan
    $mysqlPlan = Get-MysqlConnectionPlan
    Assert-Equal -Expected $mysqlPlan.ZoneCount -Actual $plan.ZoneCount -Because '规划 zone 数只有一个来源(Get-MysqlConnectionPlan),不写第二个 4'
    Assert-Equal -Expected $mysqlPlan.RolloutExtraPodsPerDeployment -Actual $plan.RolloutExtraPodsPerDeployment -Because '滚动余量与 MySQL 总账同一来源'
    Assert-Equal -Expected (Get-K8sDeployIntParamDefault -Name 'GateReplicas') -Actual $plan.GatePerZone -Because '规划档每 zone gate 台数取 -GateReplicas 的缺省值'
    Assert-Equal -Expected (Get-K8sDeployIntParamDefault -Name 'SceneReplicas') -Actual $plan.ScenePerZone -Because '规划档每 zone scene 台数取 -SceneReplicas 的缺省值'
    Assert-Equal -Expected (Get-K8sDeployIntParamDefault -Name 'BattleReplicas') -Actual $plan.BattleProcesses -Because '规划档 battle 进程数取 -BattleReplicas 的缺省值'
    Assert-Equal -Expected 0.8 -Actual $plan.WarnRatio -Because '告警水位是 80%(与负责人定的口径一致;改它要同改 README 与告警文案)'

    $ledger = Get-RepoRedisPlanLedger
    $line80 = [Math]::Floor($ledger.MaxClients * 0.8)
    Assert-Equal -Expected $line80 -Actual $ledger.WarnAt -Because '总账的告警水位必须就是 ⌊0.8 × maxclients⌋'
    Assert-True -Condition ($ledger.Peak -le $line80) -Because ("规划档(zones={0})峰值 {1} 超过 80% × maxclients = {2}:要么收紧连接(显式 PoolSize、减副本),要么在维护窗口抬高 redis.yaml 的 MAXCLIENTS(先在线 CONFIG SET)。分项:{3}" -f $plan.ZoneCount, $ledger.Peak, $line80, (Format-RedisConnectionLedger -Ledger $ledger))
}

Test-Case 'Redis 总账输入:客户端表的副本数就是各 manifest 的 replicas;显式 PoolSize 的行等于服务 yaml 的值;有缺省池的 Go 服务 manifest 的 CPU limit ≤ 表里的 GOMAXPROCS(按 2 记的前提)' {
    $RepoRoot = Get-RepoRoot
    $inputs = Get-RedisConnectionLedgerInputs
    Assert-Equal -Expected 10 -Actual $inputs.GoPoolSizePerProc -Because 'go-redis v9 的 PoolSize 缺省 = 10 × GOMAXPROCS(只用于还没显式池上限的 DefaultPools)'
    Assert-Equal -Expected 1 -Actual $inputs.CppConnectionsPerProcess -Because 'C++ 每进程 1 条(node.cpp 唯一的 tlsRedis.Connect)'
    $names = @($inputs.Clients | ForEach-Object { $_.Name })
    foreach ($expectedName in @('login', 'db', 'data-service', 'player-locator', 'scene-manager', 'gateway', 'match', 'chat', 'friend', 'trade', 'guild', 'player-inbox')) {
        Assert-True -Condition ($names -contains $expectedName) -Because "客户端表里必须有 $expectedName(它连着共享 Redis;不连了要先改代码再删这一行)"
    }
    # player-inbox(go/player_inbox/internal/svc/servicecontext.go openCoreRedis):全局一份、核心 Redis 一个 go-redis 池(CoreRedis.PoolSize,
    # 池上限的出处在下面 $explicitPools 里核)、没有 Pub/Sub;围栏上界、在线提醒与选主锁共用这一个客户端。改成多个池或加订阅时这里先变红,逼着同改总账那一行。
    $inbox = @($inputs.Clients | Where-Object { $_.Name -eq 'player-inbox' })
    Assert-Equal -Expected 1 -Actual $inbox.Count -Because '客户端表里 player-inbox 只能有一行'
    Assert-Equal -Expected 'global/0/0/0' -Actual ('{0}/{1}/{2}/{3}' -f $inbox[0].Scope, $inbox[0].DefaultPools, $inbox[0].PubSub, $inbox[0].Fixed) `
        -Because 'player-inbox 每副本 = 1 个显式 PoolSize 的 go-redis 池,没有缺省大小的池、Pub/Sub 与固定连接,按全局计'

    # 显式 PoolSize 的 go-redis 直连池(P0-E 第 3 批):ConfiguredPoolConns 必须等于服务 yaml 里那个键的值(这里独立读,不经被测函数),
    # 且是正整数;这几个服务剩下的缺省大小池只有 data-service 的 go-zero MappingRedis。换键名、拆成多个池、或删掉显式值时这里先变红。
    $explicitPools = @(
        @{ Name = 'login'; Source = 'go/login/etc/login.yaml'; KeyPath = 'Node.RedisClient.PoolSize'; DefaultPools = 0 }
        @{ Name = 'db'; Source = 'go/db/etc/db.yaml'; KeyPath = 'ServerConfig.RedisClient.PoolSize'; DefaultPools = 0 }
        @{ Name = 'data-service'; Source = 'go/data_service/etc/data_service.yaml'; KeyPath = 'DevRedis.PoolSize'; DefaultPools = 1 }
        @{ Name = 'player-locator'; Source = 'go/player_locator/etc/player_locator.yaml'; KeyPath = 'RedisClient.PoolSize'; DefaultPools = 0 }
        @{ Name = 'trade'; Source = 'go/trade/etc/trade.yaml'; KeyPath = 'SharedRedis.PoolSize'; DefaultPools = 0 }
        @{ Name = 'guild'; Source = 'go/guild/etc/guild.yaml'; KeyPath = @('RedisClient.PoolSize', 'PlayerLocatorRedis.PoolSize', 'MergeMarkerRedis.PoolSize', 'LoginRedis.PoolSize'); DefaultPools = 0 }
        @{ Name = 'friend'; Source = 'go/friend/etc/friend.yaml'; KeyPath = 'LoginRedis.PoolSize'; DefaultPools = 1 }
        @{ Name = 'match'; Source = 'go/match/etc/match_service.yaml'; KeyPath = 'LoginRedis.PoolSize'; DefaultPools = 1 }
        @{ Name = 'chat'; Source = 'go/chat/etc/chat.yaml'; KeyPath = 'LoginRedis.PoolSize'; DefaultPools = 0 }
        @{ Name = 'player-inbox'; Source = 'go/player_inbox/etc/player_inbox.yaml'; KeyPath = 'CoreRedis.PoolSize'; DefaultPools = 0 }
    )
    $explicitNames = @($explicitPools | ForEach-Object { $_.Name })
    foreach ($pool in $explicitPools) {
        $row = @($inputs.Clients | Where-Object { $_.Name -eq $pool.Name })
        Assert-Equal -Expected 1 -Actual $row.Count -Because "客户端表里 $($pool.Name) 只能有一行"
        $expected = 0
        foreach ($poolKey in @($pool.KeyPath)) {
            $raw = Get-EtcValue -RelativePath $pool.Source -KeyPath $poolKey
            $poolValue = 0
            Assert-True -Condition ([int]::TryParse([string]$raw, [ref]$poolValue) -and $poolValue -gt 0) -Because "$($pool.Source) 的 $poolKey='$raw' 必须显式写正整数(0 / 不写 = go-redis 缺省 10 × GOMAXPROCS,随 CPU limit 漂)"
            $expected += $poolValue
        }
        Assert-Equal -Expected $expected -Actual $row[0].ConfiguredPoolConns -Because "$($pool.Name) 的显式池上限必须读自 $($pool.Source) 的 $($pool.KeyPath)(ConfigMap 镜像同一个键)"
        Assert-Equal -Expected $pool.DefaultPools -Actual $row[0].DefaultPools -Because "$($pool.Name) 剩下的缺省大小池个数(data-service 只剩 go-zero 的 MappingRedis)"
    }
    foreach ($client in $inputs.Clients) {
        if ($explicitNames -contains $client.Name) { continue }
        Assert-Equal -Expected 0 -Actual $client.ConfiguredPoolConns -Because "$($client.Name) 没有 PoolMirror,ConfiguredPoolConns 只能是 0(显式池上限必须有服务 yaml 出处)"
    }

    foreach ($client in $inputs.Clients) {
        $manifestText = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) $client.Manifest)) -replace "`r`n", "`n"
        $replicas = [regex]::Matches($manifestText, '(?m)^  replicas:[ \t]*(\d+)[ \t]*$')
        Assert-Equal -Expected 1 -Actual $replicas.Count -Because "$($client.Manifest) 里两格缩进的 replicas 行应恰好 1 处"
        Assert-Equal -Expected $replicas[0].Groups[1].Value -Actual $client.Replicas -Because "$($client.Name) 的副本数必须读自 $($client.Manifest)"
        if ($client.DefaultPools -le 0) { continue }

        # G(GOMAXPROCS)按 2 记的前提:Go 1.25+ 容器感知按 CPU limit 向上取整、下限 2(外部常识,待 S6 实测)。limit 超过 G 核,
        # 缺省池这一列就低估了池上限 —— 在这里变红,逼着重算总账(或给该服务显式 PoolSize,见上面的 $explicitPools)。
        # 显式 PoolSize 的池不随 CPU limit 变,不在这里核。
        $lines = $manifestText -split "`n"
        $cpuLimits = New-Object System.Collections.Generic.List[double]
        for ($i = 0; $i -lt $lines.Count; $i++) {
            $limitsLine = [regex]::Match($lines[$i], '^(?<indent>[ ]+)limits:[ \t]*$')
            if (-not $limitsLine.Success) { continue }
            for ($j = $i + 1; $j -lt $lines.Count; $j++) {
                if ($lines[$j].Trim().Length -eq 0) { continue }
                if (($lines[$j].Length - $lines[$j].TrimStart().Length) -le $limitsLine.Groups['indent'].Value.Length) { break }
                $cpu = [regex]::Match($lines[$j], '^[ ]+cpu:[ \t]*"?(?<v>[0-9]+(?:\.[0-9]+)?)(?<m>m?)"?[ \t]*$')
                if ($cpu.Success) {
                    $cores = [double]::Parse($cpu.Groups['v'].Value, [Globalization.CultureInfo]::InvariantCulture)
                    if ($cpu.Groups['m'].Value -eq 'm') { $cores = $cores / 1000 }
                    $cpuLimits.Add($cores)
                }
            }
        }
        Assert-Equal -Expected 1 -Actual $cpuLimits.Count -Because "$($client.Manifest) 应当恰好有 1 个 CPU limit(没有 limit 时 GOMAXPROCS = 节点核数,总账无从谈起)"
        Assert-True -Condition ($cpuLimits[0] -le $client.GoMaxProcs) -Because ("{0} 的 CPU limit {1} 核超过了总账里记的 GOMAXPROCS={2}:池上限 = 10 × GOMAXPROCS 会跟着变大,重算 Get-RedisConnectionLedgerInputs 的这一行" -f $client.Name, $cpuLimits[0], $client.GoMaxProcs)
    }
}

Test-Case '自检:Redis 总账公式逐项可复现,滚动余量只按「每副本之和」计,输入非法即 throw 并点名' {
    # 合成输入:每一项取互不相同的值,任何一项漏算、zone / global 接反、显式池 / PubSub / Fixed 漏加,结果都对不上。不读仓库文件。
    #   zoneA(只有显式池,GOMAXPROCS 写 0 也合法)每副本 = 20 + 1 = 21;zoneB = 2 × 10 × 3 = 60;zoneJ(非 Go)= 2;
    #   globalA = 1 × 10 × 1 = 10;globalB(显式池 + 缺省池混合)= 10 + 1 × 10 × 1 + 1 = 21
    #   每 zone = 2 × 21 + 3 × 60 + 4 × 2 = 230;全局 = 5 × 10 + 2 × 21 + battle 3 × 1 = 95
    #   稳态 = 3 × 230 + C++ 20 × 1 + 95 = 805;滚动余量 = 2 × (21 + 60 + 2 + 10 + 21 + 3 × 1) = 234
    #   峰值 = 805 + 234 + 运维 7 = 1046;告警水位 = ⌊1000 × 0.8⌋ = 800;余量 = 1000 − 1046 = −46
    $newInputs = {
        [ordered]@{
            MaxClients = 1000; GoPoolSizePerProc = 10; CppConnectionsPerProcess = 1
            Clients = @(
                [ordered]@{ Name = 'zoneA'; Scope = 'zone'; Replicas = 2; ConfiguredPoolConns = 20; DefaultPools = 0; GoMaxProcs = 0; PubSub = 1; Fixed = 0 }
                [ordered]@{ Name = 'zoneB'; Scope = 'zone'; Replicas = 3; ConfiguredPoolConns = 0; DefaultPools = 2; GoMaxProcs = 3; PubSub = 0; Fixed = 0 }
                [ordered]@{ Name = 'zoneJ'; Scope = 'zone'; Replicas = 4; ConfiguredPoolConns = 0; DefaultPools = 0; GoMaxProcs = 0; PubSub = 0; Fixed = 2 }
                [ordered]@{ Name = 'globalA'; Scope = 'global'; Replicas = 5; ConfiguredPoolConns = 0; DefaultPools = 1; GoMaxProcs = 1; PubSub = 0; Fixed = 0 }
                [ordered]@{ Name = 'globalB'; Scope = 'global'; Replicas = 2; ConfiguredPoolConns = 10; DefaultPools = 1; GoMaxProcs = 1; PubSub = 1; Fixed = 0 }
            )
        }
    }
    $shape = @{ ZoneCount = 3; CppZoneProcesses = 20; BattleProcesses = 3; RolloutExtraPodsPerDeployment = 2; OpsReserve = 7; WarnRatio = 0.8 }
    $ledger = Get-RedisConnectionLedger -Inputs (& $newInputs) @shape
    Assert-Equal -Expected '230/95/805/234/7/1046/1000/800/-46' -Actual ('{0}/{1}/{2}/{3}/{4}/{5}/{6}/{7}/{8}' -f $ledger.PerZone, $ledger.Global, $ledger.Steady, $ledger.RolloutSurge, $ledger.OpsReserve, $ledger.Peak, $ledger.MaxClients, $ledger.WarnAt, $ledger.Headroom) -Because '分项依次是 每 zone、全局、稳态、滚动余量、运维余量、峰值、上限、告警水位、余量'

    $noRollout = @{} + $shape
    $noRollout['RolloutExtraPodsPerDeployment'] = 0
    $withoutRollout = Get-RedisConnectionLedger -Inputs (& $newInputs) @noRollout
    Assert-Equal -Expected 234 -Actual ($ledger.Peak - $withoutRollout.Peak) -Because 'R=2 与 R=0 的峰值之差 = 2 × (每副本之和 + 3 个 C++ 工作负载);不乘副本数,也不乘 zone 数'
    Assert-Equal -Expected $withoutRollout.Steady -Actual $ledger.Steady -Because '滚动余量不进稳态'

    $missingKey = & $newInputs
    $missingKey.Remove('MaxClients')
    $negativeReplicas = & $newInputs
    $negativeReplicas.Clients[0]['Replicas'] = -1
    $badScope = & $newInputs
    $badScope.Clients[1]['Scope'] = 'Zone'
    $zeroConnections = & $newInputs
    $zeroConnections.Clients[2]['Fixed'] = 0
    $poolWithoutProcs = & $newInputs
    $poolWithoutProcs.Clients[3]['GoMaxProcs'] = 0
    $negativeConfiguredPool = & $newInputs
    $negativeConfiguredPool.Clients[0]['ConfiguredPoolConns'] = -1
    $missingConfiguredPool = & $newInputs
    $missingConfiguredPool.Clients[4].Remove('ConfiguredPoolConns')
    $noClients = & $newInputs
    $noClients['Clients'] = @()
    foreach ($bad in @(
        @{ Why = '缺 MaxClients'; Inputs = $missingKey; Pattern = 'Redis 连接总账.*输入 MaxClients=' }
        @{ Why = '副本数为负'; Inputs = $negativeReplicas; Pattern = 'Redis 连接总账.*客户端 zoneA 的 Replicas=' }
        @{ Why = 'Scope 大小写不对'; Inputs = $badScope; Pattern = "Redis 连接总账.*客户端 zoneB 的 Scope='Zone'" }
        @{ Why = '每副本 0 条'; Inputs = $zeroConnections; Pattern = 'Redis 连接总账.*客户端 zoneJ 每副本 0 条连接' }
        @{ Why = '有缺省大小的 Go 池却没有 GOMAXPROCS'; Inputs = $poolWithoutProcs; Pattern = 'Redis 连接总账.*客户端 globalA .*GoMaxProcs 必须为正' }
        @{ Why = '显式池上限为负'; Inputs = $negativeConfiguredPool; Pattern = 'Redis 连接总账.*客户端 zoneA 的 ConfiguredPoolConns=' }
        @{ Why = '缺 ConfiguredPoolConns(不能当成 0)'; Inputs = $missingConfiguredPool; Pattern = 'Redis 连接总账.*客户端 globalB 的 ConfiguredPoolConns=' }
        @{ Why = '客户端表为空'; Inputs = $noClients; Pattern = 'Redis 连接总账.*Clients 为空' }
    )) {
        $message = ''
        try { Get-RedisConnectionLedger -Inputs $bad.Inputs @shape | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern $bad.Pattern -Because "$($bad.Why) 必须 throw 并点名,不能当成 0 条或某个默认值算下去"
    }
    $badRatio = @{} + $shape
    $badRatio['WarnRatio'] = 1.5
    $message = ''
    try { Get-RedisConnectionLedger -Inputs (& $newInputs) @badRatio | Out-Null } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'WarnRatio=1\.5 必须在 \(0, 1\] 内' -Because '告警比例越界必须 throw'
}

Test-Case 'ConfigMap 镜像 go-redis 池上限 PoolSize == 服务 yaml(zone-up:login / db / data-service / player-locator;infra-up:trade / player-inbox),Redis 连接总账读同一个键' {
    # P0-E 第 3 批:这几个 go-redis 直连客户端以前不设 PoolSize,池上限 = 10 × GOMAXPROCS,跟着 CPU limit 漂,总账只能按 G=2 估。
    # 现在服务 yaml 写显式值,ConfigMap 镜像同一个键(Get-RedisPoolMirror),总账读同一个键。镜像漏写时 go-zero 按 0 加载 =
    # 回到缺省池,集群跑的就不是账上的数,而且没有任何报错。
    $zoneMirrors = @(
        @{ ConfigMap = 'go-svc-login-config'; Prefix = 'data.login.yaml.'; Source = 'go/login/etc/login.yaml'; KeyPath = 'Node.RedisClient.PoolSize' }
        @{ ConfigMap = 'go-svc-db-config'; Prefix = 'data.db.yaml.'; Source = 'go/db/etc/db.yaml'; KeyPath = 'ServerConfig.RedisClient.PoolSize' }
        @{ ConfigMap = 'go-svc-data-service-config'; Prefix = 'data.data_service.yaml.'; Source = 'go/data_service/etc/data_service.yaml'; KeyPath = 'DevRedis.PoolSize' }
        @{ ConfigMap = 'go-svc-player-locator-config'; Prefix = 'data.player_locator.yaml.'; Source = 'go/player_locator/etc/player_locator.yaml'; KeyPath = 'RedisClient.PoolSize' }
    )
    foreach ($mirror in $zoneMirrors) {
        $block = Select-ManifestByName -Output $devOut -Name $mirror.ConfigMap
        Assert-True -Condition ($null -ne $block) -Because "DryRun 输出里应当有 $($mirror.ConfigMap)"
        $flat = ConvertTo-FlatManifest -Block $block
        $expected = Get-EtcValue -RelativePath $mirror.Source -KeyPath $mirror.KeyPath
        $actual = Get-FlatValue -Flat $flat -KeyPath ($mirror.Prefix + $mirror.KeyPath)
        Assert-Equal -Expected $expected -Actual $actual -Because "$($mirror.ConfigMap) 的 $($mirror.KeyPath) 必须与 $($mirror.Source) 一致(生成器不得自带常数;Redis 连接总账读的是同一个键)"
    }

    # trade 是全局服务,只在 infra-up / all-up 的基础设施阶段生成:做法同上面「连 MySQL 的 ConfigMap 只有总账认识的那些」那次 infra-up
    # (去掉 -SkipGoSvc / -WaitReady、补 -GoSvcRegistry,仍是 DryRun)。
    $infraArgs = @($BattleInfraArgs | Where-Object { $_ -notin @('-SkipGoSvc', '-WaitReady') }) + @('-GoSvcRegistry', 'registry.invalid/test')
    Assert-True -Condition (($infraArgs -contains '-DryRun') -and -not ($infraArgs -contains '-SkipGoSvc')) -Because '夹具前提:这次 infra-up 必须仍是 DryRun(不碰任何集群),且不再跳过 Go 服务'
    $infraRun = Invoke-DeployDryRun -Arguments $infraArgs -Env $BattleTestEnv
    Assert-Equal -Expected 0 -Actual $infraRun.ExitCode -Because "带全局 Go 服务的 infra-up DryRun 必须成功。输出: $($infraRun.Output)"
    $tradeBlock = Select-ManifestByName -Output $infraRun.Output -Name 'go-svc-trade-config'
    Assert-True -Condition ($null -ne $tradeBlock) -Because 'infra-up DryRun 输出里应当有 go-svc-trade-config'
    $tradeFlat = ConvertTo-FlatManifest -Block $tradeBlock
    Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/trade/etc/trade.yaml' -KeyPath 'SharedRedis.PoolSize') `
        -Actual (Get-FlatValue -Flat $tradeFlat -KeyPath 'data.trade.yaml.SharedRedis.PoolSize') `
        -Because 'go-svc-trade-config 的 SharedRedis.PoolSize 必须与 go/trade/etc/trade.yaml 一致(Redis 连接总账读的是同一个键)'
    $inboxBlock = Select-ManifestByName -Output $infraRun.Output -Name 'go-svc-player-inbox-config'
    Assert-True -Condition ($null -ne $inboxBlock) -Because 'infra-up DryRun 输出里应当有 go-svc-player-inbox-config'
    $inboxFlat = ConvertTo-FlatManifest -Block $inboxBlock
    Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/player_inbox/etc/player_inbox.yaml' -KeyPath 'CoreRedis.PoolSize') `
        -Actual (Get-FlatValue -Flat $inboxFlat -KeyPath 'data.player_inbox.yaml.CoreRedis.PoolSize') `
        -Because 'go-svc-player-inbox-config 的 CoreRedis.PoolSize 必须与 go/player_inbox/etc/player_inbox.yaml 一致(Redis 连接总账读的是同一个键)'
    $chatFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $infraRun.Output -Name 'go-svc-chat-config')
    Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/chat/etc/chat.yaml' -KeyPath 'LoginRedis.PoolSize') `
        -Actual (Get-FlatValue -Flat $chatFlat -KeyPath 'data.chat.yaml.LoginRedis.PoolSize') `
        -Because 'Chat 唯一登录原件只读池与其服务 YAML、Redis 总账使用同一个显式上限'
}

# go-redis 池上限镜像(Get-RedisPoolMirror)读的服务配置。夹具用例把它们拷到临时目录再逐项改坏,不碰仓库。
$redisPoolMirrorFixtureFiles = @(
    'go/match/etc/match_service.yaml'
    'go/login/etc/login.yaml'
    'go/db/etc/db.yaml'
    'go/data_service/etc/data_service.yaml'
    'go/player_locator/etc/player_locator.yaml'
    'go/trade/etc/trade.yaml'
    'go/guild/etc/guild.yaml'
    'go/friend/etc/friend.yaml'
    'go/chat/etc/chat.yaml'
    'go/player_inbox/etc/player_inbox.yaml'
)

Test-Case '负向:服务 yaml 的 go-redis PoolSize 不是正整数时 Get-RedisPoolMirror throw 并点名文件与键路径,总账输入同样 throw(夹具是临时目录里的副本,不碰仓库)' {
    # 0 在 Go 侧 = go-redis 缺省 10 × GOMAXPROCS(负数拒启):镜像一个 0 进 ConfigMap,集群里生效的就不是写出来的值,总账也不是集群的账。
    # 总账输入(Get-RedisConnectionLedgerInputs)由写路径入口的 Write-RedisConnectionLedger 先于任何集群写操作调用,它 throw 就不留半截部署。
    $tempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('redis-pool-mirror-' + [guid]::NewGuid().ToString('N'))
    $resetFixture = {
        foreach ($rel in $redisPoolMirrorFixtureFiles) {
            $dst = Join-Path $tempRoot $rel
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $dst) | Out-Null
            Copy-Item -LiteralPath (Join-Path (Get-RepoRoot) $rel) -Destination $dst -Force
        }
    }
    try {
        # 总账输入还要读各 manifest 的副本数与 redis.yaml 的 MAXCLIENTS:整棵 deploy/k8s/manifests 拷一次(夹具只改服务 yaml)。
        $k8sDir = Join-Path $tempRoot 'deploy/k8s'
        New-Item -ItemType Directory -Force -Path $k8sDir | Out-Null
        Copy-Item -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/manifests') -Destination $k8sDir -Recurse -Force
        # Get-AuthoritativeScalar / Get-ManifestDeploymentReplicas 按动态作用域读 $RepoRoot(在 k8s_deploy.ps1 里是脚本级变量)。
        $RepoRoot = $tempRoot
        # 前提:副本不改就该放行,且每个目标恰好一个正整数池上限。否则下面几条负向断言分不清是谁造成的失败。
        & $resetFixture
        foreach ($target in @('login', 'db', 'data-service', 'player-locator', 'trade', 'guild', 'friend', 'match', 'chat', 'player-inbox')) {
            $values = Get-RedisPoolMirror -Target $target
            $expectedPoolCount = if ($target -eq 'guild') { 4 } else { 1 }
            Assert-Equal -Expected $expectedPoolCount -Actual $values.Count -Because "前提:$target 的显式池数量必须与真实客户端匹配"
            if ($target -eq 'guild') {
                Assert-Equal -Expected 'LoginRedis.PoolSize,MergeMarkerRedis.PoolSize,PlayerLocatorRedis.PoolSize,RedisClient.PoolSize' `
                    -Actual (@($values.Keys | Sort-Object) -join ',') -Because 'Guild 的四池逐键绑定当前服务客户端，不能只把计数改大'
            }
            foreach ($poolSize in $values.Values) { Assert-True -Condition ($poolSize -gt 0) -Because "前提:仓库里 $target 的 PoolSize 是正整数" }
        }
        [void](Get-RedisConnectionLedgerInputs)

        foreach ($bad in @(
            @{ Why = 'login 写 0(= go-redis 缺省池)'; Target = 'login'; File = 'go/login/etc/login.yaml'; KeyPath = 'Node.RedisClient.PoolSize'; Value = '0' }
            @{ Why = 'db 写负数'; Target = 'db'; File = 'go/db/etc/db.yaml'; KeyPath = 'ServerConfig.RedisClient.PoolSize'; Value = '-1' }
            @{ Why = 'data-service 写非数字'; Target = 'data-service'; File = 'go/data_service/etc/data_service.yaml'; KeyPath = 'DevRedis.PoolSize'; Value = 'twenty' }
            @{ Why = 'player-locator 写小数'; Target = 'player-locator'; File = 'go/player_locator/etc/player_locator.yaml'; KeyPath = 'RedisClient.PoolSize'; Value = '20.5' }
            @{ Why = 'trade 写 0'; Target = 'trade'; File = 'go/trade/etc/trade.yaml'; KeyPath = 'SharedRedis.PoolSize'; Value = '0' }
            @{ Why = 'guild 全局池写0'; Target = 'guild'; File = 'go/guild/etc/guild.yaml'; KeyPath = 'RedisClient.PoolSize'; Value = '0' }
            @{ Why = 'guild 在线池写负数'; Target = 'guild'; File = 'go/guild/etc/guild.yaml'; KeyPath = 'PlayerLocatorRedis.PoolSize'; Value = '-1' }
            @{ Why = 'guild 合服标记池写非数字'; Target = 'guild'; File = 'go/guild/etc/guild.yaml'; KeyPath = 'MergeMarkerRedis.PoolSize'; Value = 'twenty' }
            @{ Why = 'guild 登录原件池写0'; Target = 'guild'; File = 'go/guild/etc/guild.yaml'; KeyPath = 'LoginRedis.PoolSize'; Value = '0' }
            @{ Why = 'friend 登录原件池写负数'; Target = 'friend'; File = 'go/friend/etc/friend.yaml'; KeyPath = 'LoginRedis.PoolSize'; Value = '-1' }
            @{ Why = 'match 登录原件池写0'; Target = 'match'; File = 'go/match/etc/match_service.yaml'; KeyPath = 'LoginRedis.PoolSize'; Value = '0' }
            @{ Why = 'chat 登录原件池写0'; Target = 'chat'; File = 'go/chat/etc/chat.yaml'; KeyPath = 'LoginRedis.PoolSize'; Value = '0' }
            @{ Why = 'player-inbox 写 0'; Target = 'player-inbox'; File = 'go/player_inbox/etc/player_inbox.yaml'; KeyPath = 'CoreRedis.PoolSize'; Value = '0' }
        )) {
            & $resetFixture
            Set-FixtureYamlScalar -Path (Join-Path $tempRoot $bad.File) -KeyPath $bad.KeyPath -Value $bad.Value
            $message = ''
            try { Get-RedisPoolMirror -Target $bad.Target | Out-Null } catch { $message = $_.Exception.Message }
            Assert-Match -Text $message -Pattern 'Redis 连接池镜像' -Because "$($bad.Why):必须 throw(fail-closed),不能把非法池值镜像进 ConfigMap"
            Assert-Match -Text $message -Pattern ([regex]::Escape("$($bad.File) 的 $($bad.KeyPath)='$($bad.Value)'")) -Because "$($bad.Why):错误必须点名文件、键路径与值"
            $message = ''
            try { Get-RedisConnectionLedgerInputs | Out-Null } catch { $message = $_.Exception.Message }
            Assert-Match -Text $message -Pattern 'Redis 连接池镜像' -Because "$($bad.Why):总账输入同样必须 throw(写路径入口先于任何集群写操作读它)"
        }
    }
    finally {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case '部署写路径打印 Redis connection ledger:zone-up 的数字可按同一组函数重算,zones= 与 MySQL 那一行相同,先于任何 kubectl 写操作;infra-up 同样打印一行' {
    $zones = @((Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/zones.json') -Raw | ConvertFrom-Json).zones)
    $gateDefault = Get-K8sDeployIntParamDefault -Name 'GateReplicas'
    $sceneDefault = Get-K8sDeployIntParamDefault -Name 'SceneReplicas'
    $battleDefault = Get-K8sDeployIntParamDefault -Name 'BattleReplicas'
    # 基线 DryRun($BaseArgs)是 zone-up -ZoneId 101、读 deploy/k8s/zones.json:Z = 其中全部 zone ∪ 101;101 按命令行(缺省)台数,
    # 其余 zone 按 zones.json 的 replicas(没写的键落回缺省)。zones.json 只用 legacy 单池,拆分池的键出现时本例要跟着改。
    $cppByZone = @{}
    foreach ($zone in $zones) {
        $replicaKeys = if ($null -ne $zone.replicas) { @($zone.replicas.PSObject.Properties.Name) } else { @() }
        Assert-True -Condition (-not ($replicaKeys -contains 'scene_world' -or $replicaKeys -contains 'scene_instance')) -Because '夹具前提:deploy/k8s/zones.json 不用 scene 拆分池'
        $gate = if ($replicaKeys -contains 'gate') { [int]$zone.replicas.gate } else { $gateDefault }
        $scene = if ($replicaKeys -contains 'scene') { [int]$zone.replicas.scene } else { $sceneDefault }
        $cppByZone[[int]$zone.zoneId] = $gate + $scene
    }
    $cppByZone[101] = $gateDefault + $sceneDefault
    $cppTotal = 0
    foreach ($processes in $cppByZone.Values) { $cppTotal += $processes }

    $RepoRoot = Get-RepoRoot
    $plan = Get-RedisConnectionPlan
    $deployLedger = Get-RedisConnectionLedger -Inputs (Get-RedisConnectionLedgerInputs -RouteStoreDbFirst $true) -ZoneCount $cppByZone.Count -CppZoneProcesses $cppTotal `
        -BattleProcesses $battleDefault -RolloutExtraPodsPerDeployment $plan.RolloutExtraPodsPerDeployment -OpsReserve $plan.OpsReserve -WarnRatio $plan.WarnRatio
    $planLedger = Get-RepoRedisPlanLedger

    $ledgerLines = @($devOut -split "`r?`n" | Where-Object { $_ -match '^\s*Redis connection ledger: zones=' })
    Assert-Equal -Expected 1 -Actual $ledgerLines.Count -Because 'zone-up 写路径入口必须恰好打印一行 Redis connection ledger(Write-RedisConnectionLedger)'
    $ledgerLine = [string]$ledgerLines[0]
    $deployPart = 'Redis connection ledger: zones={0} steady={1} peak={2} maxclients={3} warn_at={4} headroom={5};' -f $cppByZone.Count, $deployLedger.Steady, $deployLedger.Peak, $deployLedger.MaxClients, $deployLedger.WarnAt, $deployLedger.Headroom
    Assert-Match -Text $ledgerLine -Pattern ('^\s*' + [regex]::Escape($deployPart)) -Because ("本次部署那一份必须与总账函数按同一组输入算出的值一致。分项:{0}" -f (Format-RedisConnectionLedger -Ledger $deployLedger))
    $planPart = '; plan zones={0} peak={1} headroom={2}' -f $plan.ZoneCount, $planLedger.Peak, $planLedger.Headroom
    Assert-Match -Text $ledgerLine -Pattern ([regex]::Escape($planPart) + '\s*$') -Because '规划档那一份必须与 Get-RedisConnectionPlan 算出的值一致,且 zones 配置在场时行尾没有 zones_config=missing'
    Assert-NotMatch -Text $devOut -Pattern '\[RedisConnectionLedger\]OverWarnRatio:' -Because '仓库 zones.json 与规划档都在 80% 水位以内,基线 DryRun 不该打这条警告'

    $firstKubectlAt = $devOut.IndexOf('[dry-run] kubectl')
    Assert-True -Condition ($firstKubectlAt -lt 0 -or $devOut.IndexOf('Redis connection ledger:') -lt $firstKubectlAt) -Because '总账在写路径入口打印,先于任何 kubectl 写操作'

    # zone 集合与 MySQL 那一份同一口径(Write-RedisConnectionLedger 的注释写明两处一起改):两行的 zones= 必须相等,zone-up 与 infra-up 都核。
    foreach ($run in @(@{ Label = 'zone-up'; Output = $devOut }, @{ Label = 'infra-up'; Output = $battleInfraRun.Output })) {
        $redisLines = @($run.Output -split "`r?`n" | Where-Object { $_ -match '^\s*Redis connection ledger: zones=' })
        $mysqlLines = @($run.Output -split "`r?`n" | Where-Object { $_ -match '^\s*MySQL connection ledger: zones=' })
        Assert-Equal -Expected 1 -Actual $redisLines.Count -Because "$($run.Label) 写路径入口必须恰好打印一行 Redis connection ledger"
        Assert-Equal -Expected 1 -Actual $mysqlLines.Count -Because "$($run.Label) 写路径入口必须恰好打印一行 MySQL connection ledger(对照前提)"
        $redisZones = [regex]::Match([string]$redisLines[0], 'zones=(\d+) ').Groups[1].Value
        $mysqlZones = [regex]::Match([string]$mysqlLines[0], 'zones=(\d+) ').Groups[1].Value
        Assert-Equal -Expected $mysqlZones -Actual $redisZones -Because "$($run.Label):Redis 与 MySQL 两份总账数的是同一批 zone"
    }
}

Test-Case '重启闸纯判定:线上不存在 → absent(放行),只剩旧 Deployment → legacy-deployment(要确认),摘要相同 → unchanged(放行),摘要不同或线上没有注解 → restart(要确认)' {
    foreach ($case in @(
        @{ Exists = $false; Live = ''; Desired = 'abcdef0123456789'; Verdict = 'absent' }
        @{ Exists = $false; Live = '0000000000000000'; Desired = 'abcdef0123456789'; Verdict = 'absent' }
        @{ Exists = $false; Legacy = $false; Live = ''; Desired = 'abcdef0123456789'; Verdict = 'absent' }
        @{ Exists = $false; Legacy = $true; Live = ''; Desired = 'abcdef0123456789'; Verdict = 'legacy-deployment' }
        # 有 sts 时旧 Deployment 那一项不参与判定(不该同时出现;出现了也按 sts 的摘要判)。
        @{ Exists = $true; Legacy = $true; Live = 'abcdef0123456789'; Desired = 'abcdef0123456789'; Verdict = 'unchanged' }
        @{ Exists = $true; Live = 'abcdef0123456789'; Desired = 'abcdef0123456789'; Verdict = 'unchanged' }
        @{ Exists = $true; Live = ' abcdef0123456789 '; Desired = 'abcdef0123456789'; Verdict = 'unchanged' }
        @{ Exists = $true; Live = '0000000000000000'; Desired = 'abcdef0123456789'; Verdict = 'restart' }
        @{ Exists = $true; Live = 'ABCDEF0123456789'; Desired = 'abcdef0123456789'; Verdict = 'restart' }
        @{ Exists = $true; Live = ''; Desired = 'abcdef0123456789'; Verdict = 'restart' }
        @{ Exists = $true; Live = $null; Desired = 'abcdef0123456789'; Verdict = 'restart' }
    )) {
        $liveText = if ($null -eq $case.Live) { '<null>' } else { "'$($case.Live)'" }
        # 不带 Legacy 键的行:不传 -LegacyDeployment,同时钉住它的缺省值(旧调用方不受影响)。
        $verdict = if ($case.ContainsKey('Legacy')) {
            Get-RedisCoreRestartVerdict -LiveExists $case.Exists -LiveDigest $case.Live -DesiredDigest $case.Desired -LegacyDeployment $case.Legacy
        }
        else {
            Get-RedisCoreRestartVerdict -LiveExists $case.Exists -LiveDigest $case.Live -DesiredDigest $case.Desired
        }
        Assert-Equal -Expected $case.Verdict -Actual $verdict -Because "线上存在=$($case.Exists) 旧 Deployment=$($case.Legacy) 注解=$liveText → 本次 $($case.Desired):没有注解按「要确认」处理(P0-E 之前部署的实例,第一次 apply 一定重启);只剩旧 Deployment 时 apply 会删它(停机、数据不迁移),同样要确认"
    }
    # 孤儿 Pod(2026-10-19):没有 sts、没有旧 Deployment,但 redis-<n> Pod 仍在服务 → orphan-pods(要确认);旧 Deployment 优先;有 sts 时不看。
    Assert-Equal -Expected 'orphan-pods' -Actual (Get-RedisCoreRestartVerdict -LiveExists $false -LiveDigest '' -DesiredDigest 'abcdef0123456789' -OrphanPodCount 1) -Because '1 → 3 切换在 orphan 删除之后失败、重跑时不能当成全新集群放行'
    Assert-Equal -Expected 'legacy-deployment' -Actual (Get-RedisCoreRestartVerdict -LiveExists $false -LiveDigest '' -DesiredDigest 'abcdef0123456789' -LegacyDeployment $true -OrphanPodCount 1) -Because '旧 Deployment 的切换走它自己的分支'
    Assert-Equal -Expected 'absent' -Actual (Get-RedisCoreRestartVerdict -LiveExists $false -LiveDigest '' -DesiredDigest 'abcdef0123456789' -OrphanPodCount 0) -Because '三样都没有才是全新集群'
    Assert-Equal -Expected 'unchanged' -Actual (Get-RedisCoreRestartVerdict -LiveExists $true -LiveDigest 'abcdef0123456789' -DesiredDigest 'abcdef0123456789' -OrphanPodCount 3) -Because '有 sts 时孤儿计数不参与判定'
}

Test-Case '负向:重启闸要确认时 throw,错误写明「apply 会重启 redis-0,全服登录中断并重放 AOF」并点名 -ConfirmRedisCoreRestart;给了开关只告警放行;DryRun 不查集群' {
    # Assert-RedisCoreRestartConfirmed 按动态作用域读 $DryRun / $InfraNamespace / $ConfirmRedisCoreRestart,读集群那一步
    # (Get-LiveRedisCoreTemplateDigest)在这里给替身:返回 $stubLive,并记查询次数。不起 kubectl。
    $InfraNamespace = 'contract-redis-gate'
    $desired = 'abcdef0123456789'
    $queries = [ref]0
    function Get-LiveRedisCoreTemplateDigest {
        param([string]$Namespace)
        if ($DryRun) { throw 'DryRun 下不该查集群' }
        $queries.Value = $queries.Value + 1
        return $stubLive
    }

    $DryRun = $false
    $ConfirmRedisCoreRestart = $false
    foreach ($case in @(
        @{ Why = '摘要不同'; Live = [pscustomobject]@{ Exists = $true; Digest = '0000000000000000' }; ShowsLive = '0000000000000000' }
        @{ Why = '线上没有注解'; Live = [pscustomobject]@{ Exists = $true; Digest = '' }; ShowsLive = '<none>' }
    )) {
        $stubLive = $case.Live
        $message = ''
        try { Assert-RedisCoreRestartConfirmed -DesiredDigest $desired } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern 'apply 会重启 redis-0,全服登录中断并重放 AOF' -Because "$($case.Why):拒绝文本必须说清后果"
        Assert-Match -Text $message -Pattern '-ConfirmRedisCoreRestart' -Because "$($case.Why):拒绝文本必须点名确认开关"
        Assert-Match -Text $message -Pattern '本次未做任何写操作' -Because "$($case.Why):闸在 Apply-Infra 的任何写操作之前"
        Assert-Match -Text $message -Pattern ([regex]::Escape("namespace=$InfraNamespace")) -Because "$($case.Why):拒绝文本必须点名 namespace"
        Assert-Match -Text $message -Pattern ([regex]::Escape($desired) + '[^\n]*' + [regex]::Escape($case.ShowsLive)) -Because "$($case.Why):拒绝文本必须给出本次与线上两个摘要"
    }

    # 给了开关:放行,只打 [RedisCoreRestart]Confirmed 告警。
    $ConfirmRedisCoreRestart = $true
    $stubLive = [pscustomobject]@{ Exists = $true; Digest = '0000000000000000' }
    $warning = (Assert-RedisCoreRestartConfirmed -DesiredDigest $desired 3>&1 | Out-String)
    Assert-Match -Text $warning -Pattern '\[RedisCoreRestart\]Confirmed:' -Because '给了 -ConfirmRedisCoreRestart 时放行,但要留一条告警说明这次会重启 redis-0'

    # 不需要确认的两种:没给开关也放行。
    $ConfirmRedisCoreRestart = $false
    foreach ($stub in @([pscustomobject]@{ Exists = $true; Digest = $desired }, [pscustomobject]@{ Exists = $false; Digest = '' })) {
        $stubLive = $stub
        Assert-RedisCoreRestartConfirmed -DesiredDigest $desired
    }
    Assert-Equal -Expected 5 -Actual $queries.Value -Because '非 DryRun 下每次过闸都要读一次线上现状(两次拒绝 + 一次带开关放行 + 两次无需确认)'

    # DryRun 不查集群:替身在 DryRun 下被调用就 throw,这里必须平安返回、查询次数不变。
    $DryRun = $true
    $stubLive = [pscustomobject]@{ Exists = $true; Digest = '0000000000000000' }
    Assert-RedisCoreRestartConfirmed -DesiredDigest $desired
    Assert-Equal -Expected 5 -Actual $queries.Value -Because 'DryRun 不连集群,重启闸只打一行说明'
}

Test-Case '负向:线上只剩旧 Deployment redis(2026-09-19 之前的形态)时重启闸拒绝,写明停机切换、数据不迁移并点名 -ConfirmRedisCoreRestart;给了开关只告警放行' {
    # 以前这种集群被判成「全新集群」放行,随后 Apply-Infra 无确认地 delete deployment redis(emptyDir,数据清空)。
    $InfraNamespace = 'contract-redis-gate'
    $desired = 'abcdef0123456789'
    $DryRun = $false
    function Get-LiveRedisCoreTemplateDigest {
        param([string]$Namespace)
        return [pscustomobject]@{ Exists = $false; Digest = ''; LegacyDeployment = $true }
    }

    $ConfirmRedisCoreRestart = $false
    $message = ''
    try { Assert-RedisCoreRestartConfirmed -DesiredDigest $desired } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern '还有旧的 deployment/redis' -Because '拒绝文本必须点明线上是旧 Deployment,不是「摘要不同」'
    Assert-Match -Text $message -Pattern '不迁移' -Because '切 StatefulSet 时 emptyDir 里的数据不迁移,拒绝文本必须说清'
    Assert-Match -Text $message -Pattern '本次未做任何写操作' -Because '闸在 Apply-Infra 的任何写操作(含 delete deployment redis)之前'
    Assert-Match -Text $message -Pattern '-ConfirmRedisCoreRestart' -Because '拒绝文本必须点名确认开关'
    Assert-Match -Text $message -Pattern ([regex]::Escape("namespace=$InfraNamespace")) -Because '拒绝文本必须点名 namespace'

    $ConfirmRedisCoreRestart = $true
    $warning = (Assert-RedisCoreRestartConfirmed -DesiredDigest $desired 3>&1 | Out-String)
    Assert-Match -Text $warning -Pattern '\[RedisCoreRestart\]Confirmed:[^\n]*legacy deployment/redis' -Because '给了开关放行,但要留一条告警说明这次会删旧 Deployment'
}

Test-Case '重启闸读线上(Get-LiveRedisCoreTemplateDigest):kubectl 失败一律 throw(fail-closed);没有 sts 时再查旧 Deployment;有 sts 时读出摘要注解、不查 Deployment' {
    # 被测函数按 AST 抽出(见本节开头),它读 $script:RedisPodTemplateDigestAnnotation:键名同样按 AST 取,不抄字面量。
    $annotationAssign = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -eq '$script:RedisPodTemplateDigestAnnotation' }, $true)
    Assert-True -Condition ($null -ne $annotationAssign) -Because 'k8s_deploy.ps1 里应当有 $script:RedisPodTemplateDigestAnnotation'
    $script:RedisPodTemplateDigestAnnotation = $annotationAssign.Right.Extent.Text.Trim().Trim("'", '"')
    $digest = 'abcdef0123456789'
    $stsJson = '{"metadata":{"name":"redis","annotations":{"' + $script:RedisPodTemplateDigestAnnotation + '":"' + $digest + '"}}}'

    # 假 kubectl:函数优先于外部程序,被测函数里的 `& kubectl` 解析到这里(按动态作用域)。按资源种类回应 $fake 里的
    # { Exit; Out },退出码经 $global:LASTEXITCODE 交回(被测函数读它)。每次调用记一行参数。
    function Build-KubectlBaseArgs { return , @() }
    $kubectlCalls = [System.Collections.Generic.List[string]]::new()
    function kubectl {
        $line = $args -join ' '
        $kubectlCalls.Add($line)
        $reply = if ($line -match '\bstatefulset\b') { $fake.Sts } elseif ($line -match '\bdeployment\b') { $fake.Deploy } elseif ($line -match '\bget pods\b') { $fake.Pods } else { throw "假 kubectl:意料之外的调用 kubectl $line" }
        $global:LASTEXITCODE = [int]$reply.Exit
        return $reply.Out
    }
    try {
        # 查 sts 失败:throw,不再往下查(查不到不等于不存在)。
        $fake = @{ Sts = @{ Exit = 1; Out = '' }; Deploy = @{ Exit = 0; Out = '' } }
        $kubectlCalls.Clear(); $message = ''
        try { Get-LiveRedisCoreTemplateDigest -Namespace 'contract-ns' | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '查询线上 StatefulSet redis 失败' -Because 'kubectl 失败按 fail-closed 处理;当成「不存在」会把要重启误判成全新集群放行'
        Assert-Equal -Expected 1 -Actual $kubectlCalls.Count -Because '查 sts 失败就停,不再查 Deployment'
        Assert-Match -Text $kubectlCalls[0] -Pattern 'get statefulset redis -n contract-ns --ignore-not-found -o json' -Because '读的是指定 namespace 的 sts/redis,不存在时输出为空而不是报错'

        # 没有 sts,查 Deployment 失败:同样 throw。
        $fake = @{ Sts = @{ Exit = 0; Out = '' }; Deploy = @{ Exit = 1; Out = '' } }
        $kubectlCalls.Clear(); $message = ''
        try { Get-LiveRedisCoreTemplateDigest -Namespace 'contract-ns' | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '查询线上 Deployment redis 失败' -Because '查旧 Deployment 失败同样 fail-closed'
        Assert-Match -Text ($kubectlCalls -join "`n") -Pattern 'get deployment redis -n contract-ns --ignore-not-found' -Because '没有 sts 时要再查旧 Deployment'

        # 没有 sts,有旧 Deployment → LegacyDeployment。
        $fake = @{ Sts = @{ Exit = 0; Out = '' }; Deploy = @{ Exit = 0; Out = 'deployment.apps/redis' } }
        $live = Get-LiveRedisCoreTemplateDigest -Namespace 'contract-ns'
        Assert-True -Condition (-not $live.Exists -and $live.LegacyDeployment) -Because '线上只剩旧 Deployment redis 时必须报 LegacyDeployment,不能当成全新集群'

        Assert-NotMatch -Text ($kubectlCalls -join "`n") -Pattern 'get pods' -Because '只剩旧 Deployment 时不查孤儿 Pod(它的 Pod 名字不是 redis-<n>,切换走 legacy 分支)'

        # 三样都没有 → 全新集群。
        $fake = @{ Sts = @{ Exit = 0; Out = '' }; Deploy = @{ Exit = 0; Out = '' }; Pods = @{ Exit = 0; Out = '' } }
        $kubectlCalls.Clear()
        $live = Get-LiveRedisCoreTemplateDigest -Namespace 'contract-ns'
        Assert-True -Condition (-not $live.Exists -and -not $live.LegacyDeployment -and @($live.OrphanPods).Count -eq 0) -Because '三样都没有才是全新集群'
        Assert-Match -Text ($kubectlCalls -join "`n") -Pattern 'get pods -n contract-ns -l app=redis --ignore-not-found -o name' -Because '没有 sts 与旧 Deployment 时要再查孤儿 redis Pod'

        # 没有 sts 与旧 Deployment,但有孤儿 redis-0(1 → 3 切换在 orphan 删除之后失败):报出孤儿序号;代理 Pod 与旧 Deployment 形态的名字不计。
        $fake = @{ Sts = @{ Exit = 0; Out = '' }; Deploy = @{ Exit = 0; Out = '' }; Pods = @{ Exit = 0; Out = "pod/redis-0`npod/redis-7d9f8c-abcde" } }
        $live = Get-LiveRedisCoreTemplateDigest -Namespace 'contract-ns'
        Assert-Equal -Expected '0' -Actual (@($live.OrphanPods) -join ',') -Because '只认 StatefulSet 形态的 redis-<n>'

        # 查孤儿 Pod 失败:同样 throw。
        $fake = @{ Sts = @{ Exit = 0; Out = '' }; Deploy = @{ Exit = 0; Out = '' }; Pods = @{ Exit = 1; Out = '' } }
        $message = ''
        try { Get-LiveRedisCoreTemplateDigest -Namespace 'contract-ns' | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '查询线上 redis Pod 失败' -Because '查不到不等于没有孤儿 Pod(fail-closed)'

        # 有 sts:读出注解里的摘要,不查 Deployment。
        $fake = @{ Sts = @{ Exit = 0; Out = $stsJson }; Deploy = @{ Exit = 1; Out = '' } }
        $kubectlCalls.Clear()
        $live = Get-LiveRedisCoreTemplateDigest -Namespace 'contract-ns'
        Assert-True -Condition ($live.Exists -and -not $live.LegacyDeployment) -Because '有 sts/redis 时存在、不是旧形态'
        Assert-Equal -Expected $digest -Actual $live.Digest -Because '摘要取自 sts 自己 metadata 上的注解'
        Assert-Equal -Expected 1 -Actual $kubectlCalls.Count -Because '有 sts 时不必再查 Deployment'
    }
    finally {
        # 假 kubectl 改过全局退出码;复位,免得影响后面按 $LASTEXITCODE 判断的用例。
        $global:LASTEXITCODE = 0
    }
}

Test-Case 'Redis Pod 模板摘要只跟 Pod 模板走:改 YAML 注释、模板外的对象、换行风格都不变;改 MAXCLIENTS 或启动薄壳里的一行 shell 注释就变;没有 StatefulSet redis 即 throw' {
    $base = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/infra/redis.yaml')) -replace "`r`n", "`n"
    $baseDigest = Get-RedisPodTemplateDigest -ManifestContent $base
    Assert-Match -Text $baseDigest -Pattern '^[0-9a-f]{16}$' -Because '摘要是 16 位小写十六进制'

    # 不该变:Pod 模板里加一行 YAML 注释(apply 不会重启,不该要求确认)。
    $withYamlComment = $base.Replace("`n          env:`n", "`n          # contract test: yaml comment only`n          env:`n")
    Assert-True -Condition ($withYamlComment -ne $base) -Because '夹具前提:redis.yaml 里有一行 10 格缩进的 env:(容器的 env 段)'
    Assert-Equal -Expected $baseDigest -Actual (Get-RedisPodTemplateDigest -ManifestContent $withYamlComment) -Because '只改 YAML 注释不改 Pod 模板'
    # 不该变:模板外的对象(PDB)与 StatefulSet 自己的注解。
    $outside = $base.Replace('minAvailable: 1', 'minAvailable: 0').Replace('"__REDIS_POD_TEMPLATE_DIGEST__"', '"0123456789abcdef"')
    Assert-True -Condition ($outside -ne $base) -Because '夹具前提:redis.yaml 里有 PDB 的 minAvailable: 1 与摘要注解占位'
    Assert-Equal -Expected $baseDigest -Actual (Get-RedisPodTemplateDigest -ManifestContent $outside) -Because 'PDB 与 StatefulSet 自己的 metadata 不在 Pod 模板里,改它们不重启 redis-0'
    # 不该变:CRLF 与 LF。
    Assert-Equal -Expected $baseDigest -Actual (Get-RedisPodTemplateDigest -ManifestContent ($base -replace "`n", "`r`n")) -Because '工作树的换行风格不该让摘要变'

    # 该变:启动薄壳里加一行 shell 注释(它在字面块里,是 Pod 模板的一部分,改了照样滚动)、MAXCLIENTS 改值。
    $maxLine = [regex]::Match($base, '(?m)^(?<indent>[ ]+)MAXCLIENTS=(?<n>\d+)$')
    Assert-True -Condition $maxLine.Success -Because '夹具前提:redis.yaml 的启动薄壳里有 MAXCLIENTS=<数字> 一行'
    $withShellComment = $base.Insert($maxLine.Index + $maxLine.Length, "`n" + $maxLine.Groups['indent'].Value + '# contract test: shell comment')
    Assert-True -Condition ((Get-RedisPodTemplateDigest -ManifestContent $withShellComment) -ne $baseDigest) -Because '字面块里的 shell 注释是 Pod 模板的一部分:改了 apply 会重启 redis-0,摘要必须变'
    $bumped = $base.Substring(0, $maxLine.Index) + $maxLine.Groups['indent'].Value + 'MAXCLIENTS=' + ([long]$maxLine.Groups['n'].Value + 1) + $base.Substring($maxLine.Index + $maxLine.Length)
    Assert-True -Condition ((Get-RedisPodTemplateDigest -ManifestContent $bumped) -ne $baseDigest) -Because '改 MAXCLIENTS 就是改 Pod 模板,摘要必须变'

    # throw:清单里没有 StatefulSet redis。
    $noStatefulSet = @(($base -split '(?m)^---[ \t]*$') | Where-Object { $_ -notmatch '(?m)^kind:[ \t]*StatefulSet[ \t]*$' }) -join "---`n"
    $message = ''
    try { Get-RedisPodTemplateDigest -ManifestContent $noStatefulSet | Out-Null } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'Redis Pod 模板摘要.*实际 0 份' -Because '找不到 StatefulSet redis 必须 throw,不能拿一个空摘要去比'
}

Test-Case 'infra-up DryRun:sts/redis 自己的 metadata 带 16 位模板摘要注解(不在 Pod 模板里),值 == 按产物与按仓库文件重算;重启闸与 maxclients 核对在 DryRun 下只打说明' {
    Assert-Equal -Expected 0 -Actual $battleInfraRun.ExitCode -Because "infra-up DryRun 必须成功。输出: $($battleInfraRun.Output)"
    $annotationAssign = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -eq '$script:RedisPodTemplateDigestAnnotation' }, $true)
    Assert-True -Condition ($null -ne $annotationAssign) -Because 'k8s_deploy.ps1 里应当有 $script:RedisPodTemplateDigestAnnotation(重启闸读线上注解用的键)'
    $annotationKey = $annotationAssign.Right.Extent.Text.Trim().Trim("'", '"')

    $doc = Select-RedisStatefulSetDocument -Output $battleInfraRun.Output
    Assert-NotMatch -Text $doc -Pattern '(?-i)__[A-Z][A-Z0-9_]*__' -Because '占位必须全部替换(摘要注解占位由 Apply-Infra 填)'
    $annotation = [regex]::Match($doc, '(?m)^  annotations:[ \t]*\n    ' + [regex]::Escape($annotationKey) + ':[ \t]*"(?<digest>[0-9a-f]{16})"[ \t]*$')
    Assert-True -Condition $annotation.Success -Because "StatefulSet redis 自己的 metadata(两格缩进的 annotations:)下必须有 ${annotationKey}: `"<16 位十六进制>`""
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($doc, [regex]::Escape($annotationKey)).Count) -Because '摘要注解只能在 StatefulSet 自己的 metadata 上;写进 Pod 模板会让每次改摘要都滚动 redis-0'
    $digest = $annotation.Groups['digest'].Value
    Assert-Equal -Expected (Get-RedisPodTemplateDigest -ManifestContent $doc) -Actual $digest -Because '注解里的摘要必须等于对 apply 出去的那份 Pod 模板重算的结果'
    $fileText = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/infra/redis.yaml')).Replace('__INFRA_NAMESPACE__', 'contract-battle-infra')
    Assert-Equal -Expected (Get-RedisPodTemplateDigest -ManifestContent $fileText) -Actual $digest -Because 'Apply-Infra 开头的重启闸与写进注解的摘要出自同一个文件、同一种渲染'

    Assert-Match -Text $battleInfraRun.Output -Pattern ('Redis restart gate: skipped in DryRun \(cluster not queried\); rendered pod template digest=' + $digest + ' confirm_redis_core_restart=False') -Because 'DryRun 不查集群,重启闸只打一行说明(带本次摘要与收到的开关值;这次没给开关)'
    $RepoRoot = Get-RepoRoot
    Assert-Match -Text $battleInfraRun.Output -Pattern ('\[dry-run\] redis maxclients check skipped \(manifest MAXCLIENTS=' + (Get-RedisMaxClientsFromManifest) + '\)') -Because '-WaitReady 的部署后核对在 DryRun 下只打说明,不 exec 进 Pod'
    $gateAt = $battleInfraRun.Output.IndexOf('Redis restart gate:')
    $firstWrite = [regex]::Match($battleInfraRun.Output, '\[dry-run\] kubectl [^\r\n]*\b(?:apply|delete|create|patch|replace)\b')
    Assert-True -Condition ($gateAt -ge 0 -and (-not $firstWrite.Success -or $gateAt -lt $firstWrite.Index)) -Because '重启闸必须先于 Apply-Infra 的任何写操作(Ensure-Namespace、删旧 Deployment、建 Secret、apply)'
}

Test-Case 'Apply-Infra 只读一次 redis.yaml:过闸算摘要、循环里 apply、-WaitReady 部署后核对的 MAXCLIENTS 都用同一份内容(AST 契约)' {
    # DryRun 产物测不出「读了两次」:跑测期间文件不变,两次读的结果相同。这里按代码形状钉住修复(2026-10-16 审查 P0-E E3):
    # 以前过闸读一次、循环里又 Get-Content 一次,两次之间文件被改时闸按旧内容放行而实际 apply 新模板。
    $applyInfra = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Apply-Infra' }, $true)
    Assert-True -Condition ($null -ne $applyInfra) -Because 'k8s_deploy.ps1 里应当有 Apply-Infra'
    $commands = @($applyInfra.Body.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))
    $redisReads = @($commands | Where-Object { $_.GetCommandName() -eq 'Get-Content' -and $_.Extent.Text -match '\$redisManifestPath\b' })
    Assert-Equal -Expected 1 -Actual $redisReads.Count -Because 'Apply-Infra 里只能有一处 Get-Content 读 redis.yaml($redisManifestPath)'
    $loopUsesRendered = $applyInfra.Body.Find({ param($a) $a -is [System.Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -eq '$manifestContent' -and $a.Right.Extent.Text.Trim() -eq '$redisManifestRendered' }, $true)
    Assert-True -Condition ($null -ne $loopUsesRendered) -Because '循环里 redis.yaml 分支必须直接用过闸那一份($manifestContent = $redisManifestRendered),不重读文件'
    # 修复前的第二次读就是循环里的 `Get-Content -Path $path`。只数 $redisManifestPath 看不见它,所以再钉住:循环里每一处读 $path 的
    # Get-Content 都必须落在「redis.yaml 且已有 $redisManifestRendered」那个 if 的 else 分支里,redis.yaml 走不到它。
    $loopReads = @($commands | Where-Object { $_.GetCommandName() -eq 'Get-Content' -and $_.Extent.Text -match '\$path\b' })
    Assert-True -Condition ($loopReads.Count -ge 1) -Because '夹具前提:循环里其余 infra 清单仍按 $path 读文件'
    foreach ($read in $loopReads) {
        $guarded = $false
        for ($node = $read.Parent; $null -ne $node -and -not [object]::ReferenceEquals($node, $applyInfra); $node = $node.Parent) {
            if ($node -isnot [System.Management.Automation.Language.IfStatementAst] -or $null -eq $node.ElseClause) { continue }
            $condition = $node.Clauses[0].Item1.Extent.Text
            $inElse = $read.Extent.StartOffset -ge $node.ElseClause.Extent.StartOffset -and $read.Extent.EndOffset -le $node.ElseClause.Extent.EndOffset
            # 2026-10-19 起分支条件是「所选 Redis 清单」($manifest -eq $redisTopology.Manifest,redis.yaml 或 redis-ha.yaml)。
            if ($inElse -and ($condition -match 'redis\.yaml' -or $condition -match '\$redisTopology\.Manifest') -and $condition -match '\$redisManifestRendered') { $guarded = $true; break }
        }
        Assert-True -Condition $guarded -Because "循环里的 $($read.Extent.Text) 必须只在「不是 redis.yaml 或开头没读到它」的 else 分支里执行,否则 redis.yaml 会被重读"
    }
    $digestFromRendered = @($commands | Where-Object { $_.GetCommandName() -eq 'Get-RedisPodTemplateDigest' -and $_.Extent.Text -match '-ManifestContent\s+\$redisManifestRendered\b' })
    Assert-Equal -Expected 1 -Actual $digestFromRendered.Count -Because '重启闸的摘要必须对同一份 $redisManifestRendered 计算'
    $maxFromRendered = @($commands | Where-Object { $_.GetCommandName() -eq 'Get-RedisMaxClientsFromManifest' -and $_.Extent.Text -match '-ManifestContent\s+\$redisManifestRendered\b' })
    Assert-True -Condition ($maxFromRendered.Count -ge 1) -Because '-WaitReady 部署后核对的期望 MAXCLIENTS 必须取自同一份内容,不重新读盘'
}

# 包装入口透传 -ConfirmRedisCoreRestart:照上面 Invoke-GateRouterModePassthrough 的做法,只从 AST 取出透传函数,
# 把 $ScriptDir 指到临时目录里的假下游脚本 —— 不起 kubectl、不 build 镜像,只看下游收没收到这个开关。
function Invoke-ConfirmRedisCoreRestartPassthrough {
    param(
        [Parameter(Mandatory = $true)][string]$SourceScript,
        [Parameter(Mandatory = $true)][string]$WrapperFunction,
        [Parameter(Mandatory = $true)][hashtable]$WrapperArgs,
        [Parameter(Mandatory = $true)][bool]$Confirm
    )
    $fakeDir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-redis-restart-passthrough-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $fakeDir | Out-Null
    try {
        # 简单脚本(无 CmdletBinding / [Parameter]):包装函数 splat 过来的其余具名参数落进 $args,只把开关收没收到打出来。
        $fakeBody = 'param([switch]$ConfirmRedisCoreRestart) "ConfirmRedisCoreRestart=$($ConfirmRedisCoreRestart.IsPresent)"'
        foreach ($downstream in @('k8s_deploy.ps1', 'k8s_image.ps1')) {
            Set-Content -LiteralPath (Join-Path $fakeDir $downstream) -Value $fakeBody -Encoding utf8
        }
        # 被测函数按动态作用域读 $ScriptDir / $ConfirmRedisCoreRestart 等脚本级参数,这里放进本函数作用域;其余参数未定义即 $null。
        $ScriptDir = $fakeDir
        $ConfirmRedisCoreRestart = $Confirm
        function Get-ImageRef { return 'registry.invalid/test/mmorpg-node:0123456789ab' }
        . ([scriptblock]::Create((Get-ToolScriptFunctionText -ScriptName $SourceScript -FunctionName $WrapperFunction)))
        return (& $WrapperFunction @WrapperArgs | Out-String)
    }
    finally {
        Remove-Item -LiteralPath $fakeDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case 'dev_tools / k8s_image 的 -ConfirmRedisCoreRestart:不给不透传,给了才透传(经包装入口的 infra-up / release-all 也能确认重启 redis-0)' {
    $wrappers = @(
        @{ Source = 'dev_tools.ps1'; Function = 'Invoke-K8sDeploy'; Args = @{ K8sCommand = 'infra-up' } },
        @{ Source = 'dev_tools.ps1'; Function = 'Invoke-K8sImage'; Args = @{ ImageCommand = 'release-all' } },
        @{ Source = 'k8s_image.ps1'; Function = 'Invoke-K8sDeploy'; Args = @{ DeployCommand = 'all-up' } }
    )
    foreach ($w in $wrappers) {
        $label = "$($w.Source) $($w.Function)"
        $absent = Invoke-ConfirmRedisCoreRestartPassthrough -SourceScript $w.Source -WrapperFunction $w.Function -WrapperArgs $w.Args -Confirm $false
        Assert-Match -Text $absent -Pattern 'ConfirmRedisCoreRestart=False' -Because "$label 没给开关时不得替运维确认重启"
        $present = Invoke-ConfirmRedisCoreRestartPassthrough -SourceScript $w.Source -WrapperFunction $w.Function -WrapperArgs $w.Args -Confirm $true
        Assert-Match -Text $present -Pattern 'ConfirmRedisCoreRestart=True' -Because "$label 必须把开关透传下去,否则从文档入口无法确认重启,模板一变 infra-up 就只能被拒"
    }
}

# ─────────────────────────────────────────────────────────────────
# 共享 Redis 的高可用形态(-RedisReplicas;docs/design/no-single-node-horizontal-scaling-20261001.md §5)
# ─────────────────────────────────────────────────────────────────
# redis-ha.yaml = 1 主 2 从 + 每个 redis Pod 一个哨兵 + 2 个 haproxy 入口代理,Service redis 指向代理。2026-10-08 在本机 kind 上
# 实测过故障转移 / 滚动 / 冷启动 / 原地切换,实测里踩到的坑(haproxy 3.0.29 的僵尸会话、expect 不带 min-recv 每次否定结论等 2 秒、
# 哨兵不加引号回写口令)这里逐条钉住。纯函数按 AST 抽出直接调用;DryRun 产物用 $BattleInfraArgs 起的 infra-up 核对,不碰任何集群。
$redisHaFunctionNames = @(
    'Get-RedisTopologyFor', 'Get-RedisTopologyChangeVerdict', 'Get-RedisHaLimits', 'Get-RedisHaScriptsDigest', 'Assert-RedisPasswordSentinelSafe',
    # 2026-10-19 接入补齐:zone 写路径跟随线上、WAITAOF 副本份数与拓扑同源(读脚本参数的几个按动态作用域在用例里给值)。
    'Get-MappingRedisTopologyFor', 'Get-RedisZoneTopologyDecision', 'Get-RedisTopology', 'Get-MappingRedisTopology',
    'Resolve-RedisTopologyFromLive', 'Get-RouteStoreWaitAOFNumReplicas', 'Get-RedisReplicasFromOrphanOrdinals',
    # 拒绝文本按线上读数的来源写(sts / 孤儿 Pod),副本数不是 1 / 3 时不给传不进去的参数建议。
    'Format-RedisLiveShape', 'Get-RedisLiveShapeManualFix', 'Assert-RedisTopologyChangeIsSafe'
)
foreach ($redisHaFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($redisHaFunctionNames -contains $redisHaFnAst.Name) {
        Set-Item -Path "Function:script:$($redisHaFnAst.Name)" -Value $redisHaFnAst.Body.GetScriptBlock()
    }
}
foreach ($redisHaFnName in $redisHaFunctionNames) {
    if (-not (Test-Path "Function:$redisHaFnName")) { throw "k8s_deploy.ps1 缺少函数 $redisHaFnName(共享 Redis 高可用接入被删了?后续断言无意义)" }
}
$redisHaManifestPath = Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/infra/redis-ha.yaml'
$redisHaManifestText = [IO.File]::ReadAllText($redisHaManifestPath) -replace "`r`n", "`n"
$redisHaRun = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-RedisReplicas', '3')) -Env $BattleTestEnv

Test-Case 'Redis 拓扑:只认 1 / 3;线上 1 → 目标 3 是 upgrade,3 → 1 与线上 0 / 2 一律拒绝' {
    $single = Get-RedisTopologyFor -Replicas 1
    Assert-Equal -Expected 'redis.yaml' -Actual $single.Manifest -Because '1 = 单实例清单'
    Assert-True -Condition (-not $single.HighAvailability) -Because '1 不是高可用'
    $ha = Get-RedisTopologyFor -Replicas 3
    Assert-Equal -Expected 'redis-ha.yaml' -Actual $ha.Manifest -Because '3 = 高可用清单'
    Assert-Equal -Expected 'Parallel' -Actual $ha.PodManagementPolicy -Because '冷启动时上一任主可能不是 redis-0,按序创建会互相等死'
    foreach ($bad in @(0, 2, 4, 5)) {
        $threw = $false
        try { $null = Get-RedisTopologyFor -Replicas $bad } catch { $threw = $true }
        Assert-True -Condition $threw -Because "副本数 $bad 必须拒绝(两个节点的哨兵多数派挂一个就没了;更多副本没有收益)"
    }
    $cases = @(
        @{ Live = $null; Desired = 3; Expected = 'absent' },
        @{ Live = 1; Desired = 1; Expected = 'unchanged' },
        @{ Live = 3; Desired = 3; Expected = 'unchanged' },
        @{ Live = 1; Desired = 3; Expected = 'upgrade' },
        @{ Live = 3; Desired = 1; Expected = 'refuse' },
        @{ Live = 0; Desired = 1; Expected = 'refuse' },
        @{ Live = 2; Desired = 3; Expected = 'refuse' }
    )
    foreach ($c in $cases) {
        Assert-Equal -Expected $c.Expected -Actual (Get-RedisTopologyChangeVerdict -LiveReplicas $c.Live -DesiredReplicas $c.Desired) -Because "线上 $($c.Live) → 目标 $($c.Desired)"
    }
}

Test-Case 'redis-ha.yaml 连接上限:MAXCLIENTS 与 redis.yaml 同值;代理全部副本(含 maxSurge)+ 预留 ≤ MAXCLIENTS;规划档峰值 ≤ 80% × 单个代理的 maxconn' {
    $RepoRoot = Get-RepoRoot
    $limits = Get-RedisHaLimits -ManifestContent $redisHaManifestText -SingleInstanceMaxClients (Get-RedisMaxClientsFromManifest)
    Assert-True -Condition ($limits.ProxyTotal + $limits.Reserved -le $limits.MaxClients) -Because '代理必须比 Redis 先到线:Redis 到线时代理的健康检查连接也被拒,判下线后会踢掉全部会话'
    # 一个代理重启时,它的客户端全部重连到另一个代理:单个代理要能独自扛住峰值。
    $ledger = Get-RepoRedisPlanLedger
    $line80 = [Math]::Floor(0.8 * $limits.ProxyMaxConn)
    Assert-True -Condition ($ledger.Peak -le $line80) -Because ("规划档峰值 {0} 超过 80% × PROXY_MAXCONN = {1}:另一个代理重启期间,剩下的那个会先到线" -f $ledger.Peak, $line80)
    $threw = $false
    try { $null = Get-RedisHaLimits -ManifestContent $redisHaManifestText -SingleInstanceMaxClients ($limits.MaxClients + 1) } catch { $threw = $true }
    Assert-True -Condition $threw -Because '两份清单的 MAXCLIENTS 不同时必须拒绝:Redis 连接总账只从 redis.yaml 读'
    $tooMany = [regex]::Replace($redisHaManifestText, '(?m)^([ \t]*PROXY_MAXCONN=)\d+', { param($m) $m.Groups[1].Value + [string]($limits.MaxClients) })
    $threw = $false
    try { $null = Get-RedisHaLimits -ManifestContent $tooMany -SingleInstanceMaxClients $limits.MaxClients } catch { $threw = $true }
    Assert-True -Condition $threw -Because '代理上限加起来超过 MAXCLIENTS 必须拒绝'
}

Test-Case 'redis-ha.yaml 钉住实测教训:haproxy 用 3.2(3.0.29 有僵尸会话)、每条 tcp-check expect 带 min-recv、redis-server 带 --maxclients、StatefulSet 是 Parallel' {
    Assert-Match -Text $redisHaManifestText -Pattern '(?m)^\s+image: haproxy:3\.2-' -Because 'haproxy 3.0.29 在后端被判下线时留下客户端侧不关闭的会话(长连接客户端卡死),3.2 没有'
    Assert-NotMatch -Text $redisHaManifestText -Pattern 'haproxy:3\.0' -Because '不得退回 3.0'
    $expects = [regex]::Matches($redisHaManifestText, 'tcp-check expect[^\n"]*')
    Assert-True -Condition ($expects.Count -ge 4) -Because '夹具前提:代理脚本里有认证 / PONG / 哨兵答主 / role:master 四类 expect'
    foreach ($e in $expects) {
        Assert-Match -Text $e.Value -Pattern 'min-recv \d+' -Because "「$($e.Value)」不带 min-recv 时不匹配要等到检查超时(2 秒)才下结论,切主后代理晚 2 秒才改道"
    }
    Assert-Match -Text $redisHaManifestText -Pattern '--maxclients "\$MAXCLIENTS"' -Because '每个 redis-server 都要显式 --maxclients'
    Assert-Match -Text $redisHaManifestText -Pattern '(?m)^  podManagementPolicy: Parallel\s*$' -Because '冷启动时上一任主可能不是 redis-0'
    Assert-Match -Text $redisHaManifestText -Pattern '\*\[\[:space:\]\\"\\''\\\\\]\*\)' -Because '哨兵启动脚本要拒绝含空白 / 引号 / 反斜杠的口令(否则下次重启读不回自己的配置)'
}

Test-Case '口令字符集(高可用形态):空白与单引号拒绝且不回显;dev 空口令与常规口令放行' {
    foreach ($ok in @('', 'contract-test-redis-pw-01', 'p@%^:,~#$!&*()=+.')) {
        $threw = $false
        try { Assert-RedisPasswordSentinelSafe -Password $ok } catch { $threw = $true }
        Assert-True -Condition (-not $threw) -Because "「$ok」应当放行"
    }
    foreach ($bad in @('with space-0123456', "quote'pw-0123456", "tab`tpw-0123456")) {
        $message = ''
        try { Assert-RedisPasswordSentinelSafe -Password $bad } catch { $message = $_.Exception.Message }
        Assert-True -Condition ($message.Length -gt 0) -Because '含空白 / 单引号必须拒绝'
        Assert-True -Condition (-not $message.Contains($bad)) -Because '报错不得回显口令'
    }
    $env2 = @{} + $BattleTestEnv
    $env2['MMORPG_REDIS_PASSWORD'] = "contract'pw-0123456"
    $run = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-RedisReplicas', '3')) -Env $env2
    Assert-True -Condition ($run.ExitCode -ne 0) -Because '高可用形态下含单引号的口令必须拒绝部署'
    Assert-Match -Text $run.Output -Pattern 'MMORPG_REDIS_PASSWORD' -Because '报错要点名变量'
    Assert-True -Condition (-not $run.Output.Contains("contract'pw-0123456")) -Because '报错不得回显口令'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl apply' -Because '必须在任何写操作之前拒绝'
}

Test-Case 'infra-up DryRun(-RedisReplicas 3):apply redis-ha.yaml;sts/redis 3 副本、摘要注解 == 按仓库文件重算;代理带脚本摘要、Service 指向代理、等代理就绪' {
    Assert-Equal -Expected 0 -Actual $redisHaRun.ExitCode -Because "高可用形态的 infra-up DryRun 必须成功。输出: $($redisHaRun.Output)"
    Assert-Match -Text $redisHaRun.Output -Pattern 'Redis topology: replicas=3 manifest=redis-ha\.yaml high_availability=true' -Because '入口要打出本次的 Redis 形态'
    Assert-Match -Text $redisHaRun.Output -Pattern 'Redis HA limits: maxclients=\d+ proxy_maxconn=\d+ proxies=\d+\+\d+ reserved=\d+' -Because '写路径要打出连接上限核对'
    $sts = Select-RedisStatefulSetDocument -Output $redisHaRun.Output
    Assert-Match -Text $sts -Pattern '(?m)^  replicas: 3\s*$' -Because '高可用形态是 3 个 redis Pod'
    $rendered = $redisHaManifestText.Replace('__INFRA_NAMESPACE__', 'contract-battle-infra')
    $expectedDigest = Get-RedisPodTemplateDigest -ManifestContent $rendered
    Assert-Match -Text $sts -Pattern ('mmorpg\.io/redis-pod-template-digest: "' + $expectedDigest + '"') -Because '重启闸摘要必须按高可用清单算,并写进 sts/redis 自己的注解'
    $scriptsDigest = Get-RedisHaScriptsDigest -ManifestContent $rendered
    Assert-Match -Text $redisHaRun.Output -Pattern ('mmorpg\.io/redis-ha-scripts-digest: "' + $scriptsDigest + '"') -Because '脚本一改代理就要滚动'
    Assert-Match -Text $redisHaRun.Output -Pattern '(?s)name: redis\r?\n(?:(?!\n---).)*?selector:\s+app: redis-proxy' -Because 'Service redis 必须指向代理'
    Assert-Match -Text $redisHaRun.Output -Pattern 'rollout status deployment/redis-proxy -n contract-battle-infra' -Because '客户端连的是代理:代理没就绪等于 Redis 没就绪'
    Assert-NotMatch -Text $redisHaRun.Output -Pattern '(?-i)__REDIS_[A-Z_]+__' -Because '占位必须全部替换'
}

Test-Case 'dev 基线(不传 -RedisReplicas)仍是单实例;prod 档缺省就是高可用' {
    Assert-Match -Text $battleInfraRun.Output -Pattern 'Redis topology: replicas=1 manifest=redis\.yaml high_availability=false' -Because 'dev 档缺省单实例'
    Assert-NotMatch -Text $battleInfraRun.Output -Pattern 'redis-proxy' -Because '单实例形态不出现代理'
    $run = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-ReleaseProfile', 'prod', '-BattleReplicas', '0')) -Env $ProdEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "prod 档 infra-up DryRun 必须成功。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'Redis topology: replicas=3 manifest=redis-ha\.yaml high_availability=true' -Because '非 dev 档缺省高可用(与 Kafka 同一判据)'
    $explicit = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-ReleaseProfile', 'prod', '-BattleReplicas', '0', '-RedisReplicas', '1')) -Env $ProdEnv
    Assert-Match -Text $explicit.Output -Pattern 'Redis topology: replicas=1 manifest=redis\.yaml' -Because '显式 -RedisReplicas 1 覆盖档位缺省'
    $bad = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-RedisReplicas', '2')) -Env $BattleTestEnv
    Assert-True -Condition ($bad.ExitCode -ne 0) -Because '-RedisReplicas 2 必须拒绝'
    Assert-NotMatch -Text $bad.Output -Pattern '\[dry-run\] kubectl apply' -Because '参数错误必须先于任何写操作'
}

Test-Case 'zone 写路径的 Redis 拓扑判定:没给 -RedisReplicas 跟随线上 1 / 3;显式值与线上不同、线上 0 / 2 拒绝;线上没有 sts 照本次解析' {
    Assert-Equal -Expected 'single' -Actual (Get-MappingRedisTopologyFor -Topology (Get-RedisTopologyFor -Replicas 1)) -Because '1 = 单实例(WAITAOF 不等副本)'
    Assert-Equal -Expected 'ha' -Actual (Get-MappingRedisTopologyFor -Topology (Get-RedisTopologyFor -Replicas 3)) -Because '3 = 高可用(WAITAOF 等 1 个副本)'
    $cases = @(
        @{ Live = $null; Requested = 0; Resolved = 3; Expected = 'resolved'; Why = 'infra 还没部署,不替 infra-up 判断' },
        @{ Live = 3; Requested = 0; Resolved = 3; Expected = 'resolved'; Why = '线上与档位缺省一致' },
        @{ Live = 1; Requested = 1; Resolved = 1; Expected = 'resolved'; Why = '显式值与线上一致' },
        @{ Live = 1; Requested = 0; Resolved = 3; Expected = 'live'; Why = 'staging / prod 还在单实例上:没给参数就跟随线上,zone 回滚 Step 6 照常可用' },
        @{ Live = 3; Requested = 0; Resolved = 1; Expected = 'live'; Why = 'dev 档上手工切过高可用:跟随线上,不把 WAITAOF 降回 0' },
        @{ Live = 1; Requested = 3; Resolved = 3; Expected = 'refuse'; Why = '显式 3 而线上 1:WAITAOF 写 1 没有副本可等,go/db 拒启' },
        @{ Live = 3; Requested = 1; Resolved = 1; Expected = 'refuse'; Why = '显式 1 而线上 3:WAITAOF 写 0,切主时静默丢已确认写入' },
        @{ Live = 2; Requested = 0; Resolved = 3; Expected = 'refuse'; Why = '线上 2 个副本不是本脚本产生的形态,不猜' },
        @{ Live = 0; Requested = 0; Resolved = 1; Expected = 'refuse'; Why = '线上 0 个副本不是本脚本产生的形态,不猜' }
    )
    foreach ($c in $cases) {
        Assert-Equal -Expected $c.Expected -Actual (Get-RedisZoneTopologyDecision -LiveReplicas $c.Live -RequestedReplicas $c.Requested -ResolvedReplicas $c.Resolved) -Because "线上 $($c.Live) / 参数 $($c.Requested) / 解析 $($c.Resolved):$($c.Why)"
    }
}

Test-Case 'Resolve-RedisTopologyFromLive:prod 档没给参数遇到线上单实例 → 改按单实例渲染 WAITAOF 0 并打一行;显式 3 → 拒绝且不改状态;线上没有 sts → 不变' {
    # 读集群那一步(Get-LiveStatefulSetReplicas)在这里给替身,其余按动态作用域读本用例里的同名变量。不起 kubectl。
    $InfraNamespace = 'contract-redis-follow'
    $Command = 'zone-up'
    $ReleaseProfile = 'prod'
    $OpsProfile = 'custom'
    $stubLive = [pscustomobject]@{ Replicas = 1; Source = 'sts'; Ordinals = @() }
    # 读集群的 Get-LiveRedisReplicas(一次读数:sts 副本数,sts 不在时按孤儿 Pod 推算)给替身。
    function Get-LiveRedisReplicas { return $stubLive }
    try {
        $script:RedisReplicasFollowedLive = $null
        $RedisReplicas = 0
        Assert-Equal -Expected 'ha' -Actual (Get-MappingRedisTopology) -Because '夹具前提:prod 档缺省高可用'
        $out = (Resolve-RedisTopologyFromLive 6>&1 | Out-String)
        Assert-Match -Text $out -Pattern 'Redis topology: following live statefulset/redis replicas=1 \(profile default was 3; -RedisReplicas not given\)[^\r\n]*waitaof_replicas=0' -Because '跟随线上要打一行,带上档位缺省与 WAITAOF 副本份数'
        Assert-Equal -Expected 1 -Actual $script:RedisReplicasFollowedLive -Because '跟随的线上值记在脚本级变量里,供随后的渲染使用'
        Assert-Equal -Expected 'single' -Actual (Get-MappingRedisTopology) -Because '跟随线上之后 data-service / go/db 的 WAITAOF 按单实例渲染'
        Assert-Equal -Expected 0 -Actual (Get-RouteStoreWaitAOFNumReplicas -Topology (Get-MappingRedisTopology)) -Because '单实例没有副本可等'

        $script:RedisReplicasFollowedLive = $null
        $RedisReplicas = 3
        $message = ''
        try { Resolve-RedisTopologyFromLive 6>$null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '形态与线上不一致' -Because '显式值与线上不同必须拒绝'
        Assert-Match -Text $message -Pattern '本次未做任何写操作' -Because '拒绝发生在写路径入口,早于任何写操作'
        Assert-True -Condition ($null -eq $script:RedisReplicasFollowedLive) -Because '拒绝时不得记下跟随值'

        Assert-Match -Text $message -Pattern '去掉 -RedisReplicas\(跟随线上\)或传 -RedisReplicas 1' -Because '线上是 1 / 3 时给出能执行的处置'

        $RedisReplicas = 0
        $stubLive = [pscustomobject]@{ Replicas = $null; Source = 'none'; Ordinals = @() }
        Resolve-RedisTopologyFromLive 6>$null
        Assert-True -Condition ($null -eq $script:RedisReplicasFollowedLive) -Because '线上没有 sts/redis 也没有孤儿 Pod 时照本次解析,不跟随'
        Assert-Equal -Expected 'ha' -Actual (Get-MappingRedisTopology) -Because '没有跟随时仍按档位缺省'

        # 孤儿 Pod 读数:序号连续(只剩 redis-0)时跟随并写明来源;缺号时拒绝,不说「sts/redis 有 N 个副本」,不建议传不进去的 -RedisReplicas。
        $stubLive = [pscustomobject]@{ Replicas = 1; Source = 'orphan-pods'; Ordinals = @(0) }
        $out = (Resolve-RedisTopologyFromLive 6>&1 | Out-String)
        Assert-Match -Text $out -Pattern 'following live orphan redis pods redis-0 \(statefulset/redis absent\) replicas=1' -Because '跟随孤儿 Pod 时写明来源'
        $script:RedisReplicasFollowedLive = $null
        $stubLive = [pscustomobject]@{ Replicas = -1; Source = 'orphan-pods'; Ordinals = @(1, 2) }
        $message = ''
        try { Resolve-RedisTopologyFromLive 6>$null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '没有 statefulset/redis,但带 app=redis 的孤儿 Pod redis-1, redis-2 仍在' -Because '点名孤儿 Pod'
        Assert-Match -Text $message -Pattern '序号不连续' -Because '说明推算不出形态'
        Assert-NotMatch -Text $message -Pattern '-RedisReplicas -1|传 -RedisReplicas|sts/redis 有' -Because '参数只认 0 / 1 / 3,不能建议传线上值;sts 不在时不说它有几个副本'
        Assert-Match -Text $message -Pattern '只剩 redis-0\(单实例\)或 redis-0\.\.2 齐全' -Because '给出人工整理的处置'
    }
    finally {
        $script:RedisReplicasFollowedLive = $null
    }
}

Test-Case '拓扑变更闸(Assert-RedisTopologyChangeIsSafe)的拒绝文本按读数来源写:sts 3 → 1 建议传 -RedisReplicas 3;3 个孤儿 → 1 提示回退第 2 步没删干净;孤儿缺号与 sts 2 个副本只给人工整理;孤儿 redis-0 → 3 判 upgrade' {
    $InfraNamespace = 'contract-redis-topology'
    $single = Get-RedisTopologyFor -Replicas 1
    $ha = Get-RedisTopologyFor -Replicas 3
    function Get-LiveRedisReplicas { return $stubLive }
    $refusal = {
        param($Topology)
        $m = ''
        try { $null = Assert-RedisTopologyChangeIsSafe -Topology $Topology 3>$null } catch { $m = $_.Exception.Message }
        return $m
    }

    $stubLive = [pscustomobject]@{ Replicas = 3; Source = 'sts'; Ordinals = @() }
    $message = & $refusal $single
    Assert-Match -Text $message -Pattern '线上statefulset/redis 有 3 个副本 → 目标 1 个' -Because 'sts 在时照实写副本数'
    Assert-Match -Text $message -Pattern '显式传 -RedisReplicas 3' -Because '线上是合法形态时给出「不改形态」的参数'

    $stubLive = [pscustomobject]@{ Replicas = 3; Source = 'orphan-pods'; Ordinals = @(0, 1, 2) }
    $message = & $refusal $single
    Assert-Match -Text $message -Pattern '孤儿 Pod redis-0, redis-1, redis-2 仍在' -Because '点名孤儿 Pod'
    Assert-Match -Text $message -Pattern '第 2 步还没删干净 pod/redis-1、pod/redis-2' -Because '3 个孤儿而目标 1 多半是回退做到一半'
    Assert-NotMatch -Text $message -Pattern '传 -RedisReplicas 3' -Because '照这个参数重跑会把高可用对象建回来,与回退目标相反'

    foreach ($stub in @(
            [pscustomobject]@{ Replicas = -1; Source = 'orphan-pods'; Ordinals = @(0, 2) },
            [pscustomobject]@{ Replicas = 2; Source = 'orphan-pods'; Ordinals = @(0, 1) },
            [pscustomobject]@{ Replicas = 2; Source = 'sts'; Ordinals = @() }
        )) {
        $stubLive = $stub
        $message = & $refusal $ha
        Assert-True -Condition ($message.Length -gt 0) -Because "线上 $($stub.Source) $($stub.Replicas) 必须拒绝"
        Assert-NotMatch -Text $message -Pattern '传 -RedisReplicas' -Because "线上 $($stub.Source) $($stub.Replicas):-RedisReplicas 只认 0 / 1 / 3,不能建议传线上值"
        Assert-Match -Text $message -Pattern 'kubectl scale 回 1 或 3' -Because '只给人工整理的处置'
    }

    $stubLive = [pscustomobject]@{ Replicas = 1; Source = 'orphan-pods'; Ordinals = @(0) }
    Assert-Equal -Expected 'upgrade' -Actual (Assert-RedisTopologyChangeIsSafe -Topology $ha 3>$null) -Because '1 → 3 在 orphan 删除之后失败,重跑仍按 upgrade'
}

Test-Case '孤儿 redis Pod 推算线上副本数:0..n-1 连续 → n;缺号 → -1(拒绝);没有 → $null;配上变更判定:半截 1 → 3 重跑仍是 upgrade,只剩 redis-0 回到单实例是 unchanged,3 个孤儿回到单实例照样拒绝' {
    Assert-True -Condition ($null -eq (Get-RedisReplicasFromOrphanOrdinals -Ordinals @())) -Because '没有孤儿 Pod = 真的不存在'
    Assert-Equal -Expected 1 -Actual (Get-RedisReplicasFromOrphanOrdinals -Ordinals @(0)) -Because '只剩 redis-0'
    Assert-Equal -Expected 3 -Actual (Get-RedisReplicasFromOrphanOrdinals -Ordinals @(2, 0, 1)) -Because '序号连续,顺序无关'
    Assert-Equal -Expected -1 -Actual (Get-RedisReplicasFromOrphanOrdinals -Ordinals @(1)) -Because '只剩 redis-1:不是本脚本产生的形态'
    Assert-Equal -Expected -1 -Actual (Get-RedisReplicasFromOrphanOrdinals -Ordinals @(0, 2)) -Because '中间缺号:不猜'
    Assert-Equal -Expected 'upgrade' -Actual (Get-RedisTopologyChangeVerdict -LiveReplicas (Get-RedisReplicasFromOrphanOrdinals -Ordinals @(0)) -DesiredReplicas 3) -Because '1 → 3 在 orphan 删除之后失败,重跑仍按 upgrade(orphan 删除带 --ignore-not-found,再做一次是空操作)'
    Assert-Equal -Expected 'unchanged' -Actual (Get-RedisTopologyChangeVerdict -LiveReplicas (Get-RedisReplicasFromOrphanOrdinals -Ordinals @(0)) -DesiredReplicas 1) -Because 'README 高可用 → 单实例回退的第 3 步:只剩 redis-0'
    Assert-Equal -Expected 'refuse' -Actual (Get-RedisTopologyChangeVerdict -LiveReplicas (Get-RedisReplicasFromOrphanOrdinals -Ordinals @(0, 1, 2)) -DesiredReplicas 1) -Because '先 orphan 再 apply 单实例,不能绕过 3 → 1 的拒绝'
    Assert-Equal -Expected 'refuse' -Actual (Get-RedisTopologyChangeVerdict -LiveReplicas (Get-RedisReplicasFromOrphanOrdinals -Ordinals @(0, 2)) -DesiredReplicas 3) -Because '序号认不出时拒绝'
    $live = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Get-LiveRedisReplicas' }, $true)
    Assert-True -Condition ($null -ne $live -and $live.Body.Extent.Text -match 'Get-LiveRedisOrphanPodOrdinals' -and $live.Body.Extent.Text -match 'Get-RedisReplicasFromOrphanOrdinals') -Because 'sts 不在时按孤儿 Pod 推算'
    foreach ($caller in @('Assert-RedisTopologyChangeIsSafe', 'Resolve-RedisTopologyFromLive')) {
        $fn = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq $caller }, $true)
        Assert-True -Condition ($null -ne $fn -and $fn.Body.Extent.Text -match '\bGet-LiveRedisReplicas\b' -and $fn.Body.Extent.Text -notmatch 'Get-LiveStatefulSetReplicas') -Because "$caller 必须经 Get-LiveRedisReplicas 读线上(半截切换不能当成全新集群)"
    }
}

Test-Case '重启闸 orphan-pods:没有 sts 与旧 Deployment 但孤儿 redis Pod 仍在服务时拒绝、点名 Pod 与 -ConfirmRedisCoreRestart;给了开关只告警放行' {
    $InfraNamespace = 'contract-redis-gate-orphan'
    $stubLive = [pscustomobject]@{ Exists = $false; Digest = ''; LegacyDeployment = $false; OrphanPods = @(0) }
    function Get-LiveRedisCoreTemplateDigest {
        param([string]$Namespace)
        return $stubLive
    }
    $DryRun = $false
    $ConfirmRedisCoreRestart = $false
    $message = ''
    try { Assert-RedisCoreRestartConfirmed -DesiredDigest 'abcdef0123456789' } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'Pod redis-0 仍在服务' -Because '拒绝文本要点名孤儿 Pod'
    Assert-Match -Text $message -Pattern '-ConfirmRedisCoreRestart' -Because '拒绝文本必须点名确认开关'
    Assert-Match -Text $message -Pattern '本次未做任何写操作' -Because '闸在 Apply-Infra 的任何写操作之前'
    $ConfirmRedisCoreRestart = $true
    $warning = (Assert-RedisCoreRestartConfirmed -DesiredDigest 'abcdef0123456789' 3>&1 | Out-String)
    Assert-Match -Text $warning -Pattern '\[RedisCoreRestart\]Confirmed:[^\n]*orphan pods redis-0' -Because '给了开关放行,告警写明收养孤儿 Pod'
}

Test-Case '重启闸 -RollingHandover(线上与本次都是高可用):拒绝文本写「逐个滚动重启 3 个 redis Pod」并点名 -ConfirmRedisCoreRestart;给了开关只告警放行;不传时仍是单实例文本' {
    $InfraNamespace = 'contract-redis-gate-ha'
    $desired = 'abcdef0123456789'
    $stubLive = [pscustomobject]@{ Exists = $true; Digest = '0000000000000000' }
    function Get-LiveRedisCoreTemplateDigest {
        param([string]$Namespace)
        return $stubLive
    }
    $DryRun = $false
    $ConfirmRedisCoreRestart = $false
    $message = ''
    try { Assert-RedisCoreRestartConfirmed -DesiredDigest $desired -RollingHandover } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern '逐个滚动重启 3 个 redis Pod' -Because '高可用形态下模板变化 = 逐个滚动,每个先交接主,不是「重启 redis-0、重放 AOF」'
    Assert-NotMatch -Text $message -Pattern '重放 AOF' -Because '高可用形态的拒绝文本不能沿用单实例的后果描述'
    Assert-Match -Text $message -Pattern '-ConfirmRedisCoreRestart' -Because '拒绝文本必须点名确认开关'
    Assert-Match -Text $message -Pattern '本次未做任何写操作' -Because '闸在 Apply-Infra 的任何写操作之前'
    $message = ''
    try { Assert-RedisCoreRestartConfirmed -DesiredDigest $desired } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'apply 会重启 redis-0,全服登录中断并重放 AOF' -Because '不传 -RollingHandover(单实例、1 → 3 切换)仍是 redis-0 带着全服重启'
    $ConfirmRedisCoreRestart = $true
    $warning = (Assert-RedisCoreRestartConfirmed -DesiredDigest $desired -RollingHandover 3>&1 | Out-String)
    Assert-Match -Text $warning -Pattern '\[RedisCoreRestart\]Confirmed:[^\n]*rolls the 3 redis pods one at a time' -Because '给了开关放行,告警写明是逐个滚动'
}

Test-Case 'Apply-Infra 的共享 Redis 顺序(AST):拓扑变更闸 → 高可用前置 → 重启闸(高可用且线上不变时 -RollingHandover)→ 第一个写操作;orphan 删除只在 upgrade 分支;部署后核对 -Nodes 取拓扑副本数' {
    $applyInfra = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Apply-Infra' }, $true)
    $commands = @($applyInfra.Body.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))
    $firstOf = {
        param([string]$Name)
        $found = @($commands | Where-Object { $_.GetCommandName() -eq $Name } | Sort-Object { $_.Extent.StartOffset })
        if ($found.Count -eq 0) { throw "Apply-Infra 里找不到 $Name" }
        return $found[0]
    }
    $topologyGate = & $firstOf 'Assert-RedisTopologyChangeIsSafe'
    $sentinelSafe = & $firstOf 'Assert-RedisPasswordSentinelSafe'
    $haLimits = & $firstOf 'Get-RedisHaLimits'
    $restartGate = & $firstOf 'Assert-RedisCoreRestartConfirmed'
    $ensureNs = & $firstOf 'Ensure-Namespace'
    Assert-True -Condition ($topologyGate.Extent.StartOffset -lt $sentinelSafe.Extent.StartOffset -and $sentinelSafe.Extent.StartOffset -lt $restartGate.Extent.StartOffset -and $haLimits.Extent.StartOffset -lt $restartGate.Extent.StartOffset) -Because '拓扑变更闸、口令字符集、连接上限不等式都要先于重启闸'
    Assert-True -Condition ($restartGate.Extent.StartOffset -lt $ensureNs.Extent.StartOffset) -Because '三道检查都要先于第一个写操作(Ensure-Namespace)'
    Assert-Match -Text $restartGate.Extent.Text -Pattern '-RollingHandover:\(\$redisTopology\.HighAvailability -and \$redisTopologyVerdict -eq ''unchanged''\)' -Because '只有线上与本次都是高可用时才换成滚动交接的说法;1 → 3 仍是 redis-0 重启'
    $orphan = @($commands | Where-Object { $_.GetCommandName() -eq 'Invoke-Kubectl' -and $_.Extent.Text -match '--cascade=orphan' })
    Assert-Equal -Expected 1 -Actual $orphan.Count -Because 'Apply-Infra 里只有一处 orphan 删除'
    $guarded = $false
    for ($node = $orphan[0].Parent; $null -ne $node; $node = $node.Parent) {
        if ($node -is [System.Management.Automation.Language.IfStatementAst] -and $node.Clauses[0].Item1.Extent.Text -match '\$redisTopologyVerdict -eq ''upgrade''') { $guarded = $true; break }
    }
    Assert-True -Condition $guarded -Because 'orphan 删除 sts/redis 只能在 1 → 3(upgrade)分支里'
    Assert-True -Condition ($orphan[0].Extent.StartOffset -gt $restartGate.Extent.StartOffset) -Because '重启闸必须在 orphan 删除之前读线上摘要,否则切换会落进「全新集群」的放行分支'
    $liveChecks = @($commands | Where-Object { $_.GetCommandName() -eq 'Test-RedisLiveMaxClients' })
    Assert-True -Condition ($liveChecks.Count -ge 1) -Because '夹具前提:-WaitReady 时有部署后核对'
    foreach ($check in $liveChecks) {
        Assert-Match -Text $check.Extent.Text -Pattern '-Nodes \$redisTopology\.Replicas' -Because '高可用形态每个节点都要核 maxclients(任何一个从都可能变成主)'
    }
}

Test-Case '写路径门禁(AST):zone 写路径部署 zone Go 服务时先 Resolve-RedisTopologyFromLive,再打开关计划行与 Redis 总账;不经 Get-RedisTopology 之外的地方定拓扑' {
    $gates = @($deadlineDeployAst.EndBlock.Statements | Where-Object { $_ -is [System.Management.Automation.Language.IfStatementAst] -and $_.Clauses[0].Item1.Extent.Text -match '^\$Command\s+-in\s+@\(\s*"zone-up"\s*,\s*"all-up"\s*,\s*"infra-up"\s*\)$' })
    Assert-Equal -Expected 1 -Actual $gates.Count -Because '夹具前提:恰好一个写路径门禁块'
    $gate = $gates[0]
    $calls = @($gate.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))
    $resolve = @($calls | Where-Object { $_.GetCommandName() -eq 'Resolve-RedisTopologyFromLive' })
    Assert-Equal -Expected 1 -Actual $resolve.Count -Because '写路径门禁里恰好一次 Resolve-RedisTopologyFromLive'
    $parentIf = $resolve[0].Parent
    while ($null -ne $parentIf -and $parentIf -isnot [System.Management.Automation.Language.IfStatementAst]) { $parentIf = $parentIf.Parent }
    Assert-True -Condition ($null -ne $parentIf -and $parentIf.Clauses[0].Item1.Extent.Text -match 'Test-ZoneDbDeployed' -and $parentIf.Clauses[0].Item1.Extent.Text -match 'SkipInfra') -Because '只在 zone-up / all-up -SkipInfra 且部署 zone Go 服务(渲染 WAITAOF)时查线上'
    foreach ($later in @('Assert-RouteSwitchPreflight', 'Write-RedisConnectionLedger')) {
        $c = @($calls | Where-Object { $_.GetCommandName() -eq $later })
        Assert-True -Condition ($c.Count -ge 1 -and $resolve[0].Extent.StartOffset -lt $c[0].Extent.StartOffset) -Because "跟随线上必须先于 $later(它打印 / 计算的拓扑要是跟随之后的)"
    }
    $topologyFn = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Get-RedisTopology' }, $true)
    Assert-Match -Text $topologyFn.Body.Extent.Text -Pattern '\$script:RedisReplicasFollowedLive' -Because 'Get-RedisTopology 要认跟随线上记下的值'
}

Test-Case 'prod 档 zone-up DryRun(缺省高可用):打 Redis proxy ledger 一行(单个代理扛全部客户端的水位),不越线不告警;dev 档单实例不打' {
    $RepoRoot = Get-RepoRoot
    $limits = Get-RedisHaLimits -ManifestContent $redisHaManifestText -SingleInstanceMaxClients (Get-RedisMaxClientsFromManifest)
    $warnAt = [int][Math]::Floor($limits.ProxyMaxConn * (Get-RedisConnectionPlan).WarnRatio)
    Assert-Match -Text $prodRun.Output -Pattern ('Redis proxy ledger: proxy_maxconn=' + $limits.ProxyMaxConn + ' proxies=' + $limits.ProxyReplicas + '\+' + $limits.ProxyMaxSurge + ' warn_at=' + $warnAt + ' ') -Because '高可用形态的总账要再按单个代理的 PROXY_MAXCONN 算一次水位'
    Assert-NotMatch -Text $prodRun.Output -Pattern '\[RedisConnectionLedger\]OverProxyWarnRatio:' -Because '规划档与本次部署都在单个代理的 80% 水位以内'
    Assert-NotMatch -Text $devOut -Pattern 'Redis proxy ledger:' -Because 'dev 档缺省单实例,没有代理'
    Assert-Match -Text $prodRun.Output -Pattern 'Redis topology: replicas=3 manifest=redis-ha\.yaml high_availability=true waitaof_replicas=1' -Because '入口一行带上 WAITAOF 副本份数'
}

# 包装入口透传 -RedisReplicas:做法同上面 -ConfirmRedisCoreRestart 的透传用例,只从 AST 取出透传函数,下游换成只打印参数的假脚本。
function Invoke-RedisReplicasPassthrough {
    param(
        [Parameter(Mandatory = $true)][string]$SourceScript,
        [Parameter(Mandatory = $true)][string]$WrapperFunction,
        [Parameter(Mandatory = $true)][hashtable]$WrapperArgs,
        [Parameter(Mandatory = $true)][int]$Replicas
    )
    $fakeDir = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-redis-replicas-passthrough-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $fakeDir | Out-Null
    try {
        $fakeBody = 'param([int]$RedisReplicas = -1) "RedisReplicas=$RedisReplicas"'
        foreach ($downstream in @('k8s_deploy.ps1', 'k8s_image.ps1')) {
            Set-Content -LiteralPath (Join-Path $fakeDir $downstream) -Value $fakeBody -Encoding utf8
        }
        $ScriptDir = $fakeDir
        $RedisReplicas = $Replicas
        function Get-ImageRef { return 'registry.invalid/test/mmorpg-node:0123456789ab' }
        . ([scriptblock]::Create((Get-ToolScriptFunctionText -ScriptName $SourceScript -FunctionName $WrapperFunction)))
        return (& $WrapperFunction @WrapperArgs | Out-String)
    }
    finally {
        Remove-Item -LiteralPath $fakeDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case 'dev_tools / k8s_image 的 -RedisReplicas:0 不透传(下游按档位 / 跟随线上),非 0 原样透传' {
    $wrappers = @(
        @{ Source = 'dev_tools.ps1'; Function = 'Invoke-K8sDeploy'; Args = @{ K8sCommand = 'infra-up' } },
        @{ Source = 'dev_tools.ps1'; Function = 'Invoke-K8sDeploy'; Args = @{ K8sCommand = 'zone-up' } },
        @{ Source = 'dev_tools.ps1'; Function = 'Invoke-K8sImage'; Args = @{ ImageCommand = 'release-all' } },
        @{ Source = 'k8s_image.ps1'; Function = 'Invoke-K8sDeploy'; Args = @{ DeployCommand = 'all-up' } }
    )
    foreach ($w in $wrappers) {
        $label = "$($w.Source) $($w.Function) $(@($w.Args.Values) -join ' ')"
        $absent = Invoke-RedisReplicasPassthrough -SourceScript $w.Source -WrapperFunction $w.Function -WrapperArgs $w.Args -Replicas 0
        Assert-Match -Text $absent -Pattern 'RedisReplicas=-1' -Because "$label 没给时不得替运维定形态(0 也不传,下游按档位或跟随线上)"
        $present = Invoke-RedisReplicasPassthrough -SourceScript $w.Source -WrapperFunction $w.Function -WrapperArgs $w.Args -Replicas 3
        Assert-Match -Text $present -Pattern 'RedisReplicas=3' -Because "$label 必须把 -RedisReplicas 透传下去,否则从文档入口无法在 dev 档试高可用或做 1 → 3 切换"
    }
}

# ─────────────────────────────────────────────────────────────────
# 帮会 guild 上 K8s(K8s 上线批 BK8s = docs/design/guild-phase2/90-consistency.md G-05,2026-10-16)
# ─────────────────────────────────────────────────────────────────
# guild 是全局池(infra namespace 一份),ConfigMap 按 go/guild/etc/guild.yaml 生成,AssetOp / Activity 两大块原样搬运;
# data-service 同批接上回档帮会闸(GuildInternalRpc 按 Service 名直连 guild + 三个 Rollback* 的 MethodTimeouts)。
# 带全局 Go 服务的 infra-up DryRun 单独起一次(本文件其余 infra-up 都带 -SkipGoSvc)。
$guildInfraArgs = @($BattleInfraArgs | Where-Object { $_ -notin @('-SkipGoSvc', '-WaitReady') }) + @('-GoSvcRegistry', 'registry.invalid/test')
$guildInfraRun = Invoke-DeployDryRun -Arguments $guildInfraArgs -Env $BattleTestEnv
$guildYamlText = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'go/guild/etc/guild.yaml')) -replace "`r`n", "`n"

# 文本里某个顶层键的整块(键行 + 其后所有缩进行,去掉块尾空行)。找不到即 throw。
function Get-TopLevelYamlBlockText {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Key)
    $lines = ($Text -replace "`r`n", "`n") -split "`n"
    $start = -1
    for ($i = 0; $i -lt $lines.Count; $i++) { if ($lines[$i] -match ('^' + [regex]::Escape($Key) + '\s*:')) { $start = $i; break } }
    if ($start -lt 0) { throw "找不到顶层块 ${Key}:" }
    $out = New-Object System.Collections.Generic.List[string]
    $out.Add($lines[$start].TrimEnd())
    for ($i = $start + 1; $i -lt $lines.Count; $i++) {
        $line = $lines[$i]
        if ([string]::IsNullOrWhiteSpace($line)) { $out.Add(''); continue }
        if ($line -notmatch '^\s') { break }
        $out.Add($line.TrimEnd())
    }
    while ($out.Count -gt 0 -and [string]::IsNullOrWhiteSpace($out[$out.Count - 1])) { $out.RemoveAt($out.Count - 1) }
    return ($out -join "`n")
}

Test-Case 'guild 部署形态:全局池 2 副本滚动 + PDB + 反亲和 + POD_IP;资产密钥默认不注入;迁移 Job 先于 Deployment' {
    Assert-Equal -Expected 0 -Actual $guildInfraRun.ExitCode -Because "带全局 Go 服务的 infra-up DryRun 必须成功。输出: $($guildInfraRun.Output)"
    $block = @(Get-ManifestBlocks -Output $guildInfraRun.Output | Where-Object {
        $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch '(?m)^  name: guild\s*$'
    }) | Select-Object -First 1
    Assert-True -Condition ($null -ne $block) -Because 'infra-up 产物里应当有 guild 的 Deployment'
    Assert-Match -Text $block -Pattern '(?m)^  replicas: 2\s*$' -Because '单副本 = 全服帮会功能是单点'
    Assert-Match -Text $block -Pattern 'type: RollingUpdate\s+rollingUpdate:\s+maxSurge: 1\s+maxUnavailable: 0' -Because '先起新的再停旧的'
    Assert-Match -Text $block -Pattern 'podAntiAffinity' -Because '两个副本不该落在同一个节点上'
    Assert-Match -Text $block -Pattern '(?s)kind: PodDisruptionBudget.*?name: guild-pdb.*?minAvailable: 1' -Because 'PDB 必须写在主 manifest 里'
    Assert-Match -Text $block -Pattern 'name: POD_IP\s+valueFrom:\s+fieldRef:\s+fieldPath: status\.podIP' -Because 'ListenOn 是 0.0.0.0,注册进 etcd 的必须是 Pod IP(go/guild/guild.go advertisedHost)'
    Assert-NotMatch -Text $block -Pattern '(?m)^\s*- name: MMORPG_ASSET_OP_SECRET_GUILD' -Because '资产密钥只在 -AssetOps on 时由脚本注入(Add-GoSvcSecretEnv),manifest 不手写,默认关着不引用 Secret'
    Assert-Match -Text $block -Pattern 'containerPort: 9220\s+name: metrics' -Because 'metrics 端口与 ConfigMap 的 Prometheus.Port 一致'
    $jobIndex = $guildInfraRun.Output.IndexOf('name: guild-migrate')
    $deployIndex = $guildInfraRun.Output.IndexOf('name: guild-pdb')
    Assert-True -Condition ($jobIndex -ge 0 -and $deployIndex -gt $jobIndex) -Because '建表 Job 必须先于 Deployment apply'
    $job = Select-ManifestByName -Output $guildInfraRun.Output -Name 'guild-migrate'
    Assert-Match -Text $job -Pattern 'args: \["-f", "/app/etc/guild\.yaml", "-migrate"\]' -Because 'Job 必须用与服务同一份配置跑 -migrate'
    Assert-Match -Text $job -Pattern 'name: go-svc-guild-config' -Because 'Job 挂的是 guild 自己的 ConfigMap'
    Assert-NotMatch -Text $job -Pattern '(?-i)__[A-Z][A-Z0-9_]*__|PLACEHOLDER_' -Because '占位必须全部替换'
}

Test-Case 'guild ConfigMap 镜像 go/guild/etc/guild.yaml;AssetOp / Activity 整块原样(只 AssetOp.Enabled 改成 false);不写 go-zero Etcd 段' {
    $cm = Select-ManifestByName -Output $guildInfraRun.Output -Name 'go-svc-guild-config'
    Assert-True -Condition ($null -ne $cm) -Because 'infra-up 产物里应当有 go-svc-guild-config'
    $embedded = Get-ConfigMapEmbeddedText -Block $cm -FileName 'guild.yaml'
    $flat = ConvertTo-UnprefixedScalars -Scalars (ConvertTo-FlatManifest -Block $cm).Scalars -Prefix 'data.guild.yaml.'
    foreach ($key in @('Name', 'Timeout', 'ApplicationQuotaPerMinute', 'RedisClient.DB', 'MySQL.MaxOpenConn', 'MySQL.MaxIdleConn', 'Node.LeaseTTL',
            'Cache.DefaultTTL', 'Cache.MaxMembers', 'DataServiceRpc.Timeout', 'MatchRpc.Timeout', 'IdSegment.Step', 'IdSegment.MinStep', 'IdSegment.MaxStep')) {
        Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/guild/etc/guild.yaml' -KeyPath $key) -Actual ([string]$flat[$key]).Trim('"') -Because "ConfigMap 的 $key 必须等于服务 yaml(单一真相)"
    }
    Assert-Equal -Expected 'false' -Actual ([string]$flat['IdSegment.FallbackToSnowflake']) -Because '号段失败即建帮失败,不回退 snowflake'
    $expectedAssetOp = (Get-TopLevelYamlBlockText -Text $guildYamlText -Key 'AssetOp') -replace '(?m)^  Enabled:.*$', '  Enabled: false'
    Assert-Equal -Expected $expectedAssetOp -Actual (Get-TopLevelYamlBlockText -Text $embedded -Key 'AssetOp') -Because 'AssetOp 整块原样搬运,只改第一层 Enabled(资产通道打开要先过 08 §8.3 的六条门禁)'
    Assert-Equal -Expected (Get-TopLevelYamlBlockText -Text $guildYamlText -Key 'Activity') -Actual (Get-TopLevelYamlBlockText -Text $embedded -Key 'Activity') -Because 'Activity 整块原样搬运'
    Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/match/etc/match_service.yaml' -KeyPath 'ResultTopicPartitions') -Actual (Get-EtcValue -RelativePath 'go/guild/etc/guild.yaml' -KeyPath 'Activity.TrialResult.Partitions') -Because '历练结果分区数必须等于 match 的结果 topic 分区数,否则结算漏读分区'
    Assert-NotMatch -Text $embedded -Pattern '(?m)^Etcd\s*:' -Because 'guild 刻意不写 go-zero 的 Etcd 段(D-13),写了会被当成按 zone 切片的发现键'
    Assert-Match -Text $embedded -Pattern '(?m)^PlayerLocatorRedis:\s*\n\s+Host: "redis\.contract-battle-infra:6379"\s*\n\s+Password: "[^"]*"\s*\n\s+DB: 0\s*$' -Because 'PlayerLocatorRedis 必须是共享 Redis 的 DB 0(scene / battle 写在线、位置、历练结果的同一实例)'
    Assert-Match -Text $embedded -Pattern '(?m)^MergeMarkerRedis:\s*\n\s+Host: "redis\.contract-battle-infra:6379"\s*\n\s+Pass: "[^"]*"\s*\n\s+DB: 0\s*$' -Because '合服闸门读 data-service 的 MappingRedis(共享 Redis DB 0)'
    Assert-Match -Text $embedded -Pattern 'DataSource: "[^"]*@tcp\(mysql\.contract-battle-infra:3306\)/mmorpg_guild\?' -Because 'DSN 库名必须是独占库 mmorpg_guild'
    # 配置注释会解释为何禁用此参数;约束只作用于实际连接串。
    Assert-NotMatch -Text (Get-FlatValue -Flat (ConvertTo-FlatManifest -Block $cm) -KeyPath 'data.guild.yaml.MySQL.DataSource') -Pattern 'clientFoundRows' -Because 'guild config.Validate 拒绝 DSN 中的 clientFoundRows'
    Assert-Match -Text $embedded -Pattern '(?m)^Prometheus:\s*\n\s+Host: 0\.0\.0\.0\s*\n\s+Port: 9220\s*$' -Because 'metrics 绑 0.0.0.0、端口与清单一致'
    Assert-Equal -Expected '9220' -Actual (Get-EtcValue -RelativePath 'go/guild/etc/guild.yaml' -KeyPath 'Prometheus.Port') -Because '服务 yaml 的 Prometheus.Port 与清单的 9220 一致'
    Assert-Match -Text $embedded -Pattern '(?m)^SnowflakeCacheDir: ""\s*$' -Because '容器里没有持久目录,不缓存 snowflake 槽位'
    $schemaDev = Get-EtcValue -RelativePath 'go/guild/etc/guild.yaml' -KeyPath 'Schema.AutoMigrate'
    Assert-Equal -Expected $schemaDev -Actual ([string]$flat['Schema.AutoMigrate']) -Because 'dev 档镜像服务 yaml 的建表策略'
}

Test-Case 'data-service ConfigMap 接上回档帮会闸:GuildInternalRpc 按 Service 名直连 guild,Timeout / 时钟余量 / 检查预算 / MethodTimeouts 与服务 yaml 相同' {
    $cm = Select-ManifestByName -Output $devOut -Name 'go-svc-data-service-config'
    $embedded = Get-ConfigMapEmbeddedText -Block $cm -FileName 'data_service.yaml'
    Assert-Match -Text $embedded -Pattern '(?m)^GuildInternalRpc:\s*\n\s+Endpoints:\s*\n\s+- "guild\.mmorpg-infra:50300"\s*$' -Because 'guild 不写 go-zero Etcd.Key,只能按 Endpoints 直连;guild 是全局池,在 infra namespace'
    $flat = ConvertTo-UnprefixedScalars -Scalars (ConvertTo-FlatManifest -Block $cm).Scalars -Prefix 'data.data_service.yaml.'
    foreach ($key in @('GuildInternalRpc.Timeout', 'GuildClockSkewMarginMs', 'GuildCheckBudgetSeconds')) {
        Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/data_service/etc/data_service.yaml' -KeyPath $key) -Actual ([string]$flat[$key]) -Because "ConfigMap 的 $key 必须等于服务 yaml"
    }
    Assert-Equal -Expected 'true' -Actual ([string]$flat['GuildInternalRpc.NonBlock']) -Because 'guild 不在线不得阻塞 data-service 启动'
    $serviceText = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'go/data_service/etc/data_service.yaml'))
    $expected = @(Get-ZrpcMethodTimeouts -Text $serviceText | ForEach-Object { "$($_.FullMethod)=$($_.Timeout)" }) -join ';'
    $actual = @(Get-ZrpcMethodTimeouts -Text $embedded | ForEach-Object { "$($_.FullMethod)=$($_.Timeout)" }) -join ';'
    Assert-True -Condition ($expected.Length -gt 0) -Because '夹具前提:服务 yaml 里有三个 Rollback* 的 MethodTimeouts'
    Assert-Equal -Expected $expected -Actual $actual -Because '回档方法的服务端超时必须与服务 yaml 一致,否则回档被 go-zero 默认 2000ms 截断'
}

Test-Case 'go_svc_image.ps1 与 k8s_deploy.ps1 的 guild 镜像名一致(否则发布的镜像与部署拉的镜像对不上)' {
    $imageScript = [IO.File]::ReadAllText((Join-Path (Get-ToolsScriptsDir) 'go_svc_image.ps1'))
    $deployScript = [IO.File]::ReadAllText((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'))
    $imageName = [regex]::Match($imageScript, '(?m)^\s*guild\s*=\s*@\{[^}]*ImageName\s*=\s*"(?<n>[^"]+)"')
    $deployName = [regex]::Match($deployScript, '(?m)^\s*guild\s*=\s*@\{[^}]*ImageName\s*=\s*"(?<n>[^"]+)"')
    Assert-True -Condition ($imageName.Success -and $deployName.Success) -Because '两个脚本的服务表里都应当有 guild 条目'
    Assert-Equal -Expected $deployName.Groups['n'].Value -Actual $imageName.Groups['n'].Value -Because 'ImageName 必须一致'
}

# ─────────────────────────────────────────────────────────────────
# 邮件 mail 上 K8s(M1d,docs/design/mail-system.md §7.2 / §12.3,2026-10-16)
# ─────────────────────────────────────────────────────────────────
# mail 是全局池(infra namespace 一份),带 mail-migrate Job 与运维令牌 Secret 注入(照 player-inbox)。
# 复用上面带全局 Go 服务的 infra-up DryRun:$BattleTestEnv 不带 MMORPG_MAIL_ADMIN_TOKEN,但子进程会继承父进程环境,
# 跑测试的 shell 若恰好 export 了它,「停用」分支就不成立 —— 所以停用断言单独起一次、显式把变量置空。
Test-Case 'mail 现役部署:全局双副本、滚动冗余、PDB及跨节点偏好' {
    $manifest = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/go-svc/mail.yaml') -Raw
    foreach ($pattern in @('replicas: 2','maxSurge: 1','maxUnavailable: 0','podAntiAffinity:','minAvailable: 1','name: POD_IP','path: /healthz')) {
        Assert-Match -Text $manifest -Pattern $pattern -Because "现役邮件部署必须保留冗余及存活契约: $pattern"
    }
    Assert-NotMatch -Text $manifest -Pattern 'MMORPG_MAIL_ADMIN_TOKEN|PLACEHOLDER_MAIL_ADMIN_TOKEN_HASH' -Because '已由生产者分用途签名替代M1 token'
}

Test-Case 'mail 现役签名:各生产者与资产密钥只引用必需Secret' {
    $manifest = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/go-svc/mail.yaml') -Raw
    foreach ($name in @('MAIL_PRODUCER_SECRET_ADMIN','MAIL_PRODUCER_SECRET_RANK','MAIL_PRODUCER_SECRET_GUILD','MMORPG_ASSET_OP_SECRET_MAIL')) {
        Assert-Match -Text $manifest -Pattern ('name: '+$name+'\s+valueFrom:\s+secretKeyRef: \{name: domain-service-secrets, key: [^,]+, optional: false\}') -Because '缺密钥时容器拒绝启动，不能静默降级或把值写入ConfigMap'
    }
}

# 非 dev 档:上面三条都跑 dev($guildInfraArgs 不带 -ReleaseProfile),其余非 dev 的 infra-up 都带 -SkipGoSvc,mail 段一次都不执行。
# 这里单独起 staging 的带全局 Go 服务 infra-up,钉住三条只在非 dev 档成立的安全属性:
#   - 未设令牌**不挡** infra-up(先判空再调 Resolve-InjectedSecret;后者在非 dev 档对未设置的变量直接 throw —— §12.3 偏差 2 修掉的缺陷);
#   - ConfigMap 固定 Mode: pro、Schema.AutoMigrate: false(pro 才拒 ROBOT 发件方与附件,M-9;建表只走 mail-migrate Job,D-14);
#   - 令牌与其它密钥同值、或是占位串:在任何写操作之前拒绝。
$mailStagingArgs = $guildInfraArgs + @('-ReleaseProfile', 'staging', '-BattleReplicas', '0', '-FriendTiDBHost', 'tidb.invalid:4000')
$mailStagingEnv = @{} + $ProdEnv
$mailStagingEnv['MMORPG_BATTLE_TOKEN_SECRET'] = $BattleTestEnv['MMORPG_BATTLE_TOKEN_SECRET']

Test-Case 'mail 非dev运行门禁:生产模式、TiDB和禁止自动迁移由部署统一覆写' {
    $scriptText = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'tools/scripts/k8s_deploy.ps1') -Raw
    Assert-Match -Text $scriptText -Pattern 'AutoMigrate:\.\*\$.*AutoMigrate: false' -Because '非dev不能由源码dev配置打开自动DDL'
    Assert-Match -Text $scriptText -Pattern 'RequireTiDB:\.\*\$.*RequireTiDB: true' -Because '共享环境保持SQL权威TiDB门禁'
}

Test-Case 'mail 迁移作业只读取结构迁移DSN，不加载producer或资产密钥' {
    $manifest = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/go-svc/mail-migrate.yaml') -Raw
    Assert-Match -Text $manifest -Pattern 'name: MMORPG_MAIL_SQL_DSN' -Because '迁移与常驻进程使用相同DSN'
    Assert-Match -Text $manifest -Pattern '"-migrate"' -Because '仅运行迁移入口'
    Assert-Match -Text $manifest -Pattern 'values: \[1, 4\]' -Because '不可重试迁移错误立即FailJob'
    Assert-NotMatch -Text $manifest -Pattern 'name: MAIL_PRODUCER|name: MMORPG_ASSET_OP' -Because '迁移不需要发件或资产权限'
}

Test-Case 'go_svc_image.ps1 与 k8s_deploy.ps1 的 mail 镜像名一致(否则发布的镜像与部署拉的镜像对不上)' {
    $imageScript = [IO.File]::ReadAllText((Join-Path (Get-ToolsScriptsDir) 'go_svc_image.ps1'))
    $deployScript = [IO.File]::ReadAllText((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'))
    $imageName = [regex]::Match($imageScript, '(?m)^\s*mail\s*=\s*@\{[^}]*ImageName\s*=\s*"(?<n>[^"]+)"')
    $deployName = [regex]::Match($deployScript, '(?m)^\s*mail\s*=\s*@\{[^}]*ImageName\s*=\s*"(?<n>[^"]+)"')
    Assert-True -Condition ($imageName.Success -and $deployName.Success) -Because '两个脚本的服务表里都应当有 mail 条目'
    Assert-Equal -Expected 'mmorpg-mail' -Actual $deployName.Groups['n'].Value -Because 'k8s_deploy.ps1 的 mail 镜像名'
    Assert-Equal -Expected $deployName.Groups['n'].Value -Actual $imageName.Groups['n'].Value -Because 'ImageName 必须一致'
}

Test-Case 'mail-alerts.yaml:MailSweepStalled 阈值 = 3 × go/mail/etc/mail.yaml 的 Sweep.Interval;规则引用的 mail_* 指标都由 go/mail/internal/metrics 注册' {
    $repo = Get-RepoRoot
    $alerts = [IO.File]::ReadAllText((Join-Path $repo 'deploy/k8s/mail-alerts.yaml')) -replace "`r`n", "`n"
    $svcYaml = [IO.File]::ReadAllText((Join-Path $repo 'go/mail/etc/mail.yaml')) -replace "`r`n", "`n"
    $metricsGo = [IO.File]::ReadAllText((Join-Path $repo 'go/mail/internal/metrics/metrics.go'))

    # 阈值与 sweep 周期只靠注释联动:Interval 调大(比如 30m)告警会常亮,调小会变迟钝。ConfigMap 的 Sweep.Interval 镜像服务 yaml,所以以服务 yaml 为准。
    $interval = [regex]::Match((Get-TopLevelYamlBlockText -Text $svcYaml -Key 'Sweep'), '(?m)^\s+Interval:\s*(?<n>\d+)(?<u>[smh])\s*$')
    Assert-True -Condition $interval.Success -Because 'go/mail/etc/mail.yaml 的 Sweep.Interval 应当写成 <整数><s|m|h>(换了写法要同步本用例)'
    $intervalSeconds = [int]$interval.Groups['n'].Value * (@{ s = 1; m = 60; h = 3600 }[$interval.Groups['u'].Value])
    $threshold = [regex]::Match($alerts, 'time\(\) - max\(mail_sweep_last_run_unixtime\) > (?<t>\d+)')
    Assert-True -Condition $threshold.Success -Because 'MailSweepStalled 的表达式形状变了,要同步本用例'
    Assert-Equal -Expected (3 * $intervalSeconds) -Actual ([int]$threshold.Groups['t'].Value) -Because 'MailSweepStalled 阈值必须等于 3 × Sweep.Interval(秒)'

    # 指标名只靠人工核对:改名后告警永远不触发、也不报错。从 metrics.go 拼出全名(subsystem + "_" + Name),逐个核对规则里引用的 mail_* 名字。
    $subsystem = [regex]::Match($metricsGo, '(?m)^const subsystem = "(?<s>[a-z_]+)"')
    Assert-True -Condition $subsystem.Success -Because 'go/mail/internal/metrics/metrics.go 应当有 const subsystem = "..."(换了写法要同步本用例)'
    $registered = @([regex]::Matches($metricsGo, '(?m)^\s*Name:\s*"(?<n>[a-z0-9_]+)"') | ForEach-Object { $subsystem.Groups['s'].Value + '_' + $_.Groups['n'].Value })
    $referenced = @([regex]::Matches($alerts, '\bmail_[a-z0-9_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-True -Condition ($referenced.Count -ge 3) -Because '夹具前提:告警文件至少引用 sweep 两个指标与列表截断计数器'
    foreach ($name in $referenced) {
        Assert-True -Condition ($registered -contains $name) -Because "告警引用的指标 $name 必须由 go/mail/internal/metrics 注册(改名后告警会静默失效)"
    }
}

# ─────────────────────────────────────────────────────────────────
# 排行榜评分快照开关(-MatchRatingSnapshotPublish;docs/design/leaderboard-system.md §2.4 / §5.4,L-20)
# ─────────────────────────────────────────────────────────────────
# 开关是排行榜上线的唯一闸门:K8s 上由部署参数给值、不镜像 go/match/etc/match_service.yaml;topic 三项是与 rank 共享的契约,从服务 yaml 取。
Test-Case 'match ConfigMap:RatingSnapshotPublishEnabled 由 -MatchRatingSnapshotPublish 给值(缺省 false、不镜像服务 yaml),快照 topic 三项 == 服务 yaml' {
    Assert-Equal -Expected 0 -Actual $guildInfraRun.ExitCode -Because "带全局 Go 服务的 infra-up DryRun 必须成功。输出: $($guildInfraRun.Output)"
    $cm = Select-ManifestByName -Output $guildInfraRun.Output -Name 'go-svc-match-config'
    Assert-True -Condition ($null -ne $cm) -Because 'infra-up 产物里应当有 go-svc-match-config'
    $flat = ConvertTo-UnprefixedScalars -Scalars (ConvertTo-FlatManifest -Block $cm).Scalars -Prefix 'data.match_service.yaml.'
    Assert-Equal -Expected 'false' -Actual ([string]$flat['RatingSnapshotPublishEnabled']) -Because '不传 -MatchRatingSnapshotPublish 时快照发布必须关着(rank 就绪之前不能发)'
    foreach ($key in @('RatingSnapshotTopic', 'RatingSnapshotTopicPartitions', 'RatingSnapshotRetentionHours')) {
        Assert-Equal -Expected (Get-EtcValue -RelativePath 'go/match/etc/match_service.yaml' -KeyPath $key) -Actual ([string]$flat[$key]) -Because "ConfigMap 的 $key 必须等于服务 yaml(分区数是与 rank 共享的不可变契约,保留期只由 match 声明)"
    }
    Assert-Match -Text $guildInfraRun.Output -Pattern '(?m)^MatchRatingSnapshotPublish: 0 ' -Because '部署输出要打一行本次取值(不粘滞,「榜为什么停更」先对照它)'
    $matchDeploy = @(Get-ManifestBlocks -Output $guildInfraRun.Output | Where-Object {
        $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch '(?m)^  name: match\s*$'
    }) | Select-Object -First 1
    Assert-True -Condition ($null -ne $matchDeploy) -Because 'infra-up 产物里应当有 match 的 Deployment'
    Assert-Match -Text $matchDeploy -Pattern 'mmorpg\.io/rating-snapshot-publish: "0"' -Because '开关值写进 Pod 模板注解:翻值必须改变模板、让 match 滚动(ConfigMap 只在启动时读)'
    Assert-NotMatch -Text $matchDeploy -Pattern 'PLACEHOLDER_' -Because '占位必须全部替换'

    $on = Invoke-DeployDryRun -Arguments ($guildInfraArgs + @('-MatchRatingSnapshotPublish', '1')) -Env $BattleTestEnv
    Assert-Equal -Expected 0 -Actual $on.ExitCode -Because "-MatchRatingSnapshotPublish 1 的 infra-up DryRun 必须成功。输出: $($on.Output)"
    $onCm = Select-ManifestByName -Output $on.Output -Name 'go-svc-match-config'
    $onFlat = ConvertTo-UnprefixedScalars -Scalars (ConvertTo-FlatManifest -Block $onCm).Scalars -Prefix 'data.match_service.yaml.'
    Assert-Equal -Expected 'true' -Actual ([string]$onFlat['RatingSnapshotPublishEnabled']) -Because '参数为 1 时 ConfigMap 写 true,与服务 yaml 仓内的 false 无关(不镜像)'
    Assert-Match -Text $on.Output -Pattern '(?m)^MatchRatingSnapshotPublish: 1 ' -Because '部署输出打出本次取值'
    $onDeploy = @(Get-ManifestBlocks -Output $on.Output | Where-Object {
        $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch '(?m)^  name: match\s*$'
    }) | Select-Object -First 1
    Assert-Match -Text $onDeploy -Pattern 'mmorpg\.io/rating-snapshot-publish: "1"' -Because '开关为 1 时模板注解随之变化,0 → 1 / 1 → 0 都会滚动 match'

    # 本次不部署全局 Go 服务(zone-up)却显式传了开关:没有效果,必须告警而不是静默接受。
    $zoneRun = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-MatchRatingSnapshotPublish', '1'))
    Assert-Equal -Expected 0 -Actual $zoneRun.ExitCode -Because "zone-up DryRun 必须成功。输出: $($zoneRun.Output)"
    Assert-Match -Text $zoneRun.Output -Pattern 'MatchRatingSnapshotPublish 1 has no effect' -Because 'zone-up 不部署 match,显式传开关要告警'

    $bad = Invoke-DeployDryRun -Arguments ($guildInfraArgs + @('-MatchRatingSnapshotPublish', 'on')) -Env $BattleTestEnv
    Assert-True -Condition ($bad.ExitCode -ne 0) -Because '-MatchRatingSnapshotPublish 只接受 0 / 1'
    Assert-NotMatch -Text $bad.Output -Pattern '\[dry-run\] kubectl apply' -Because '参数错误必须先于任何写操作'
}

# ─────────────────────────────────────────────────────────────────
# 排行榜 rank 上 K8s(R2b,docs/design/leaderboard-system.md §5.4 / §12.6)
# ─────────────────────────────────────────────────────────────────
# rank 是全局 SQL 池，正规迁移 Job 先于服务；历史 redis-rank 不再自动部署。
# 本节断言不写端口字面量:gRPC / metrics 端口一律从 go/rank/etc/rank.yaml 取,与 manifest、ConfigMap 三处互相核对(契约 §7)。
$rankYamlPath = 'go/rank/etc/rank.yaml'
$rankGrpcPort = [regex]::Match([string](Get-EtcValue -RelativePath $rankYamlPath -KeyPath 'ListenOn'), ':(?<p>\d+)\s*$').Groups['p'].Value
$rankMetricsPort = [regex]::Match([string](Get-EtcValue -RelativePath $rankYamlPath -KeyPath 'MetricsListenAddr'), ':(?<p>\d+)"?\s*$').Groups['p'].Value

# DryRun 把一个 manifest 文件整份打成一个块(rank.yaml = Service + Deployment + PDB,redis-rank.yaml = 两个 Service + StatefulSet)。
# 按 --- 切成单份文档,再按 kind + 顶层 metadata.name 精确取(照 Select-RedisStatefulSetDocument):只按块筛选时,
# 块里别的文档带着同一个名字就能把断言带过去(删掉 ClusterIP Service redis-rank,headless 那份照样让"有 Service"成立)。
# 恰好 1 份才返回;-AllowMissing 时 0 份返回 $null;其余一律 throw。
function Select-RankManifestDocument {
    param(
        [Parameter(Mandatory = $true)][string]$Output,
        [Parameter(Mandatory = $true)][string]$Kind,
        [Parameter(Mandatory = $true)][string]$Name,
        [switch]$AllowMissing
    )
    $kindPattern = '(?m)^kind:[ \t]*{0}[ \t]*$' -f [regex]::Escape($Kind)
    $namePattern = '(?m)^  name:[ \t]*{0}[ \t]*$' -f [regex]::Escape($Name)
    $docs = @(foreach ($block in (Get-ManifestBlocks -Output $Output)) {
            foreach ($doc in (($block -replace "`r`n", "`n") -split '(?m)^---[ \t]*$')) {
                if ($doc -cmatch $kindPattern -and $doc -cmatch $namePattern) { $doc }
            }
        })
    if ($docs.Count -eq 0 -and $AllowMissing) { return $null }
    if ($docs.Count -ne 1) { throw "DryRun 输出里 $Kind $Name 应当恰好 1 份,实际 $($docs.Count) 份" }
    return $docs[0]
}

# 同一种切分,返回某个 kind 的全部单份文档(用来断言"不存在"时按内容筛,不靠名字)。
function Get-RankManifestDocumentsOfKind {
    param(
        [Parameter(Mandatory = $true)][string]$Output,
        [Parameter(Mandatory = $true)][string]$Kind
    )
    $kindPattern = '(?m)^kind:[ \t]*{0}[ \t]*$' -f [regex]::Escape($Kind)
    return @(foreach ($block in (Get-ManifestBlocks -Output $Output)) {
            foreach ($doc in (($block -replace "`r`n", "`n") -split '(?m)^---[ \t]*$')) {
                if ($doc -cmatch $kindPattern) { $doc }
            }
        })
}

Test-Case 'rank 部署形态:全局池 2 副本滚动 + PDB + 反亲和 + POD_IP;正规SQL迁移且gRPC / metrics端口同源' {
    Assert-True -Condition ($rankGrpcPort -match '^\d+$' -and $rankMetricsPort -match '^\d+$') -Because "夹具前提:$rankYamlPath 的 ListenOn / MetricsListenAddr 能解析出端口"
    Assert-Equal -Expected 0 -Actual $guildInfraRun.ExitCode -Because "带全局 Go 服务的 infra-up DryRun 必须成功。输出: $($guildInfraRun.Output)"
    $deploy = Select-RankManifestDocument -Output $guildInfraRun.Output -Kind 'Deployment' -Name 'rank'
    $svc = Select-RankManifestDocument -Output $guildInfraRun.Output -Kind 'Service' -Name 'rank'
    $pdb = Select-RankManifestDocument -Output $guildInfraRun.Output -Kind 'PodDisruptionBudget' -Name 'rank-pdb'
    Assert-Match -Text $deploy -Pattern '(?m)^  replicas: 2\s*$' -Because '单副本 = 全服排行榜是单点'
    Assert-Match -Text $deploy -Pattern 'type: RollingUpdate\s+rollingUpdate:\s+maxSurge: 1\s+maxUnavailable: 0' -Because '先起新的再停旧的'
    Assert-Match -Text $deploy -Pattern 'podAntiAffinity' -Because '两个副本不该落在同一个节点上'
    Assert-Match -Text $pdb -Pattern '(?m)^  minAvailable: 1\s*$' -Because 'PDB 必须写在主 manifest 里,至少留一个副本'
    Assert-Match -Text $deploy -Pattern 'name: POD_IP\s+valueFrom:\s+fieldRef:\s+fieldPath: status\.podIP' -Because 'ListenOn 是 0.0.0.0,注册进 etcd 的必须是 Pod IP(go/rank/rank.go advertisedHost)'
    Assert-Match -Text $deploy -Pattern ('containerPort: {0}\s+name: grpc' -f $rankGrpcPort) -Because 'gRPC 容器端口 == 服务 yaml 的 ListenOn'
    Assert-Match -Text $deploy -Pattern ('readinessProbe:\s+grpc:\s+port: {0}\s' -f $rankGrpcPort) -Because 'readiness 探针端口 == 服务 yaml 的 ListenOn'
    Assert-Match -Text $deploy -Pattern ('livenessProbe:\s+grpc:\s+port: {0}\s' -f $rankGrpcPort) -Because 'liveness 探针端口 == 服务 yaml 的 ListenOn'
    Assert-Match -Text $svc -Pattern ('(?m)^\s+port: {0}\s*\n\s+targetPort: {0}\s*$' -f $rankGrpcPort) -Because 'Service rank 的 port / targetPort == 服务 yaml 的 ListenOn'
    Assert-Match -Text $deploy -Pattern ('containerPort: {0}\s+name: metrics' -f $rankMetricsPort) -Because 'metrics 容器端口 == 服务 yaml 的 MetricsListenAddr'
    Assert-Match -Text $deploy -Pattern ('prometheus\.io/port: "{0}"' -f $rankMetricsPort) -Because '抓取注解 == 服务 yaml 的 MetricsListenAddr'
    Assert-Match -Text $deploy -Pattern 'image: registry\.invalid/test/mmorpg-rank:' -Because '镜像名 mmorpg-rank(与 go_svc_image.ps1 一致)'
    Assert-Match -Text $deploy -Pattern '- name: KAFKA_TOPIC_REPLICATION_FACTOR' -Because 'rank 可能先于 match 建出快照 topic,副本数必须拿部署级契约(脚本注入)'
    foreach ($doc in @($deploy, $svc, $pdb)) {
        Assert-NotMatch -Text $doc -Pattern '(?-i)__[A-Z][A-Z0-9_]*__|PLACEHOLDER_' -Because '占位必须全部替换'
    }
    $migration = Select-RankManifestDocument -Output $guildInfraRun.Output -Kind 'Job' -Name 'rank-migrate'
    Assert-Match -Text $migration -Pattern '"-migrate"' -Because 'SQL正规Job必须实际渲染'
    Assert-Match -Text $migration -Pattern 'name: MMORPG_RANK_SQL_DSN' -Because '与常驻进程共用本域SQL凭证引用'
}

Test-Case 'rank ConfigMap现役源:SQL权威、邮件奖励及分用途producer' {
    $source = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'go/rank/etc/rank.yaml') -Raw
    foreach ($pattern in @('DSNEnv: MMORPG_RANK_SQL_DSN','MaxOpenConns:','RequireTiDB:','MailRpc:','Producers:')) {
        Assert-Match -Text $source -Pattern $pattern -Because "SQL通用榜必须提供现役配置: $pattern"
    }
    Assert-NotMatch -Text $source -Pattern '(?m)^RankRedis:|(?m)^RatingConsumerGroup:' -Because '不能同时启动R2b旧评分写者'
}

Test-Case 'rank 迁移与常驻进程共享SQL凭证，目录登记迁移门禁' {
    $manifest = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/go-svc/rank-migrate.yaml') -Raw
    Assert-Match -Text $manifest -Pattern 'name: MMORPG_RANK_SQL_DSN' -Because '迁移只使用本域权威DSN'
    Assert-Match -Text $manifest -Pattern '"-migrate"' -Because '部署先迁结构再启动'
    $scriptText = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'tools/scripts/k8s_deploy.ps1') -Raw
    Assert-Match -Text $scriptText -Pattern 'rank = .*MigrateJob = "rank-migrate.yaml"' -Because 'rank必须经过统一迁移完成门禁'
}

Test-Case 'R2b Redis清单保留历史，现役SQL排行不自动部署旧写榜存储' {
    $legacy = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/infra/redis-rank.yaml') -Raw
    Assert-Match -Text $legacy -Pattern 'noeviction' -Because '保留旧运维设计供历史追溯'
    $scriptText = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'tools/scripts/k8s_deploy.ps1') -Raw
    $manifestLine = [regex]::Match($scriptText,'(?m)^\s*\$infraManifests = @\([^\n]+').Value
    Assert-NotMatch -Text $manifestLine -Pattern 'redis-rank.yaml' -Because '现役SQL链不再启动第二套运行时'
}

Test-Case 'go_svc_image.ps1 与 k8s_deploy.ps1 的 rank 镜像名一致(否则发布的镜像与部署拉的镜像对不上)' {
    $imageScript = [IO.File]::ReadAllText((Join-Path (Get-ToolsScriptsDir) 'go_svc_image.ps1'))
    $deployScript = [IO.File]::ReadAllText((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'))
    $imageName = [regex]::Match($imageScript, '(?m)^\s*rank\s*=\s*@\{[^}]*ImageName\s*=\s*"(?<n>[^"]+)"')
    $deployName = [regex]::Match($deployScript, '(?m)^\s*rank\s*=\s*@\{[^}]*ImageName\s*=\s*"(?<n>[^"]+)"')
    Assert-True -Condition ($imageName.Success -and $deployName.Success) -Because '两个脚本的服务表里都应当有 rank 条目'
    Assert-Equal -Expected 'mmorpg-rank' -Actual $deployName.Groups['n'].Value -Because 'k8s_deploy.ps1 的 rank 镜像名'
    Assert-Equal -Expected $deployName.Groups['n'].Value -Actual $imageName.Groups['n'].Value -Because 'ImageName 必须一致'
    $deployPort = [regex]::Match($deployScript, '(?m)^\s*rank\s*=\s*@\{[^}]*Port\s*=\s*(?<p>\d+)')
    $localScript = [IO.File]::ReadAllText((Join-Path (Get-ToolsScriptsDir) 'go_services.ps1'))
    $localPort = [regex]::Match($localScript, '(?m)^\s*rank\s*=\s*@\{[^}]*Port\s*=\s*(?<p>\d+)')
    Assert-Equal -Expected $rankGrpcPort -Actual $deployPort.Groups['p'].Value -Because 'k8s_deploy.ps1 目录里的 rank 端口 == 服务 yaml 的 ListenOn'
    Assert-Equal -Expected $rankGrpcPort -Actual $localPort.Groups['p'].Value -Because 'go_services.ps1 目录里的 rank 端口 == 服务 yaml 的 ListenOn'
}

Test-Case 'rank SQL指标与告警同源:确认提交与回放分开，奖励真实延迟及失败保旧，无Redis消费者真相' {
    $repo = Get-RepoRoot
    $alerts = [IO.File]::ReadAllText((Join-Path $repo 'deploy/k8s/rank-alerts.yaml')) -replace "`r`n", "`n"
    foreach ($svc in @(@{ Prefix = 'rank'; Path = 'go/rank/internal/metrics/metrics.go' }, @{ Prefix = 'match'; Path = 'go/match/internal/metrics/metrics.go' })) {
        $metricsGo = [IO.File]::ReadAllText((Join-Path $repo $svc.Path))
        $subsystem = [regex]::Match($metricsGo, '(?m)^const subsystem = "(?<s>[a-z_]+)"')
        Assert-True -Condition ($subsystem.Success -and $subsystem.Groups['s'].Value -ceq $svc.Prefix) -Because '指标前缀必须与实际注册源一致'
        $registered = @([regex]::Matches($metricsGo, '(?m)^\s*Name:\s*"(?<n>[a-z0-9_]+)"') | ForEach-Object { $svc.Prefix + '_' + $_.Groups['n'].Value })
        $referenced = @([regex]::Matches($alerts, ('\b{0}_[a-z0-9_]+' -f $svc.Prefix)) | ForEach-Object { $_.Value } | Sort-Object -Unique)
        Assert-True -Condition ($referenced.Count -ge 1) -Because "告警必须实际引用 $($svc.Prefix) 指标"
        foreach ($name in $referenced) {
            Assert-True -Condition ($registered -contains $name) -Because "告警指标$name必须有实际注册者"
        }
    }
    Assert-NotMatch -Text $alerts -Pattern 'rank_last_consume_unixtime|rank_board_size|rank_read_total|out_of_range|malformed_version|gen_reset|RankConsumerStalled|RankBoardShrunk' -Because '现役SQL链不能告警已删除Redis消费者/版本表'
    $rankMetricsGo = [IO.File]::ReadAllText((Join-Path $repo 'go/rank/internal/metrics/metrics.go'))
    $outcomeOf = {
        param([string]$Const)
        $value = [regex]::Match($rankMetricsGo, ('(?m)^\s*{0}\s*=\s*"(?<v>[a-z_]+)"' -f $Const))
        Assert-True -Condition $value.Success -Because "实际指标必须定义$Const"
        return $value.Groups['v'].Value
    }
    $rule = {
        param([string]$Name)
        $m = [regex]::Match($alerts, ('(?m)^\s+- alert: {0}\s*\n(?<body>(?:^\s{{10,}}.*\n?)+)' -f [regex]::Escape($Name)))
        Assert-True -Condition $m.Success -Because "告警规则$Name必须存在"
        $body = $m.Groups['body'].Value
        $expr = [regex]::Match($body, '(?m)^\s+expr: \|\s*\n(?<e>(?:^\s{12,}\S.*\n?)+)').Groups['e'].Value
        $for = [regex]::Match($body, '(?m)^\s+for:\s*(?<f>\S+)\s*$').Groups['f'].Value
        Assert-True -Condition ($expr.Trim().Length -gt 0 -and $for.Length -gt 0) -Because "规则$Name必须有真实表达式和持续时间"
        return [pscustomobject]@{ Expr = $expr; For = $for }
    }
    $failing = & $rule 'RankWriteFailing'
    $errorOutcome = & $outcomeOf 'WriteError'
    Assert-Match -Text $failing.Expr -Pattern ('rank_write_total\{{outcome="{0}"\}}' -f [regex]::Escape($errorOutcome)) -Because '写失败只看最终error'
    Assert-Equal -Expected '10m' -Actual $failing.For -Because '持续失败才报，不把一次抖动当事故'
    Assert-NotMatch -Text $failing.Expr -Pattern 'replay|rejected|outcome=~' -Because '幂等回放和业务拒绝不能进入SQL故障告警'
    foreach ($name in @('RankRewardScanFailing', 'RankRewardDispatchFailing')) {
        $item = & $rule $name
        Assert-Equal -Expected '10m' -Actual $item.For -Because "$name必须持续失败才报"
    }
    $scan = & $rule 'RankRewardScanFailing'
    Assert-Match -Text $scan.Expr -Pattern 'rank_reward_scan_total\{outcome="error"\}' -Because '查询错误独立告警，不能依赖旧Gauge判断可用'
    $dispatch = & $rule 'RankRewardDispatchFailing'
    foreach ($const in @('RewardError', 'RewardUnknown', 'RewardRejected')) {
        Assert-Match -Text $dispatch.Expr -Pattern ('outcome=~"[^"]*\b{0}\b[^"]*"' -f [regex]::Escape((& $outcomeOf $const))) -Because "投递失败必须包括$const"
    }
    Assert-NotMatch -Text $dispatch.Expr -Pattern 'delivered|busy' -Because '成功及被其它副本抢走不是投递故障'
    $delay = & $rule 'RankRewardOverdue'
    Assert-Match -Text $delay.Expr -Pattern 'max\(rank_reward_due_delay_seconds\) > 900' -Because '多副本真实到期延迟按max聚合'
    Assert-Equal -Expected '5m' -Actual $delay.For -Because '到期15分钟并持续5分钟才报'
    $invalid = & $rule 'RankRewardInvalidPayload'
    Assert-Equal -Expected '0m' -Actual $invalid.For -Because '坏持久原件出现一次即需人工调查'
    $parts = @($invalid.Expr -split '(?m)^[ \t]+or[ \t]*$' | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    Assert-Equal -Expected 2 -Actual $parts.Count -Because '坏原件覆盖increase与首次抓取前增长两个窗口'
    foreach ($part in $parts) {
        Assert-Match -Text $part -Pattern ('rank_reward_dispatch_total\{{outcome="{0}"\}}' -f [regex]::Escape((& $outcomeOf 'RewardInvalid'))) -Because '两个窗口均只能选择invalid，不得告警所有成功请求'
    }
    Assert-Match -Text $invalid.Expr -Pattern 'and on\(instance\) \(time\(\) - process_start_time_seconds\) < 900' -Because '覆盖进程首次抓取前已增长'
    Assert-Match -Text $rankMetricsGo -Pattern 'dueDelay: prometheus.NewGaugeVec' -Because '首次成功扫描前不能发布伪造的0'
    $publisher = & $rule 'MatchRatingSnapshotPublishFailing'
    Assert-Match -Text $publisher.Expr -Pattern 'match_rating_snapshot_publish_total\{outcome="error"\}' -Because '仍实际运行的match发布入口继续告警'
    Assert-Equal -Expected '10m' -Actual $publisher.For -Because '保留现役发布失败门禁'
}

# ─────────────────────────────────────────────────────────────────
# 资产通道开关 + gRPC 口准入(BK8s 第二块:-AssetOps / -NetworkPolicies;docs/design/guild-phase2/90-consistency.md G-05,2026-10-16)
# ─────────────────────────────────────────────────────────────────
# 默认两个开关都关,产物与开关引入之前相同(只多「不粘滞」的删除行);打开时 guild / trade / scene 拿签名密钥、三份 NetworkPolicy 落地。
# 纯函数按 AST 抽出直接调用;DryRun 产物各起一次 zone-up(Deployment / Agones)与带全局 Go 服务的 infra-up,两个开关一起打开。
$assetOpFunctionNames = @('Assert-AssetOpSecretShape', 'Get-AssetOpConsistencyVerdict', 'Get-ManifestContainerPorts', 'New-GrpcAllowListNetworkPolicyYaml', 'Get-InjectedSecretFingerprint')
foreach ($assetOpFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($assetOpFunctionNames -contains $assetOpFnAst.Name) {
        Set-Item -Path "Function:script:$($assetOpFnAst.Name)" -Value $assetOpFnAst.Body.GetScriptBlock()
    }
}
foreach ($assetOpFnName in $assetOpFunctionNames) {
    if (-not (Test-Path "Function:$assetOpFnName")) { throw "k8s_deploy.ps1 缺少函数 $assetOpFnName(资产通道开关被删了?后续断言无意义)" }
}
$assetOpGuildSecret = 'contract-asset-op-guild-0123456789abcdef0123'
$assetOpTradeSecret = 'contract-asset-op-trade-0123456789abcdef0123'
$assetOpEnv = @{ MMORPG_ASSET_OP_SECRET_GUILD = $assetOpGuildSecret; MMORPG_ASSET_OP_SECRET_TRADE = $assetOpTradeSecret }
$assetOpBoth = @('-AssetOps', 'on', '-NetworkPolicies', 'on')
$assetZoneRun = Invoke-DeployDryRun -Arguments ($BaseArgs + $assetOpBoth) -Env $assetOpEnv
$assetAgonesRun = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SceneOrchestrator', 'agones', '-SkipGoSvc', '-SkipJavaSvc') + $assetOpBoth) -Env $assetOpEnv
$assetInfraEnv = @{}
foreach ($k in $BattleTestEnv.Keys) { $assetInfraEnv[$k] = $BattleTestEnv[$k] }
foreach ($k in $assetOpEnv.Keys) { $assetInfraEnv[$k] = $assetOpEnv[$k] }
$assetInfraRun = Invoke-DeployDryRun -Arguments ($guildInfraArgs + $assetOpBoth) -Env $assetInfraEnv
$tradeYamlTextForAssetOp = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'go/trade/etc/trade.yaml')) -replace "`r`n", "`n"

# 工作负载清单在 DryRun 输出里的位置(kind 行 + metadata.name 行唯一定位;找不到返回 -1)。按块开头的前几十个字符找不行:各 Deployment 开头相同。
function Get-WorkloadOffset {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$Kind, [Parameter(Mandatory = $true)][string]$Name)
    $m = [regex]::Match(($Output -replace "`r`n", "`n"), ('(?m)^kind: ' + $Kind + '[ \t]*\nmetadata:[ \t]*\n  name: ' + [regex]::Escape($Name) + '[ \t]*$'))
    if ($m.Success) { return $m.Index }
    return -1
}

# 去掉空行后比较(DryRun 采集里 CRLF here-string 的行间可能多出空行,与内容无关)。
function ConvertTo-NonBlankLines {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    return (@(($Text -replace "`r`n", "`n") -split "`n" | Where-Object { $_.Trim().Length -gt 0 } | ForEach-Object { $_.TrimEnd() }) -join "`n")
}

# NetworkPolicy 块 → 目标 app、对所有来源开放的端口、gRPC 口与放行名单(只认本脚本生成的两条 ingress 形状)。
function Get-GrpcAllowListShape {
    param([Parameter(Mandatory = $true)][string]$Block)
    $text = $Block -replace "`r`n", "`n"
    $target = [regex]::Match($text, '(?s)podSelector:\s*\n\s+matchExpressions:\s*\n\s+- key: app\s*\n\s+operator: In\s*\n\s+values: \[(?<v>[^\]]*)\]')
    $fromIndex = $text.IndexOf('    - from:')
    $ingressIndex = $text.IndexOf('  ingress:')
    $openText = if ($fromIndex -gt $ingressIndex -and $ingressIndex -ge 0) { $text.Substring($ingressIndex, $fromIndex - $ingressIndex) } else { '' }
    $open = @([regex]::Matches($openText, 'port: (\d+)') | ForEach-Object { [int]$_.Groups[1].Value } | Sort-Object)
    $grpc = [regex]::Match($text.Substring([Math]::Max(0, $fromIndex)), '(?s)values: \[(?<v>[^\]]*)\]\s*\n\s+ports:\s*\n\s+- protocol: TCP\s*\n\s+port: (?<p>\d+)')
    return [pscustomobject]@{
        TargetApps  = if ($target.Success) { @($target.Groups['v'].Value -split ',\s*') } else { @() }
        OpenPorts   = $open
        GrpcPort    = if ($grpc.Success) { [int]$grpc.Groups['p'].Value } else { 0 }
        AllowedApps = if ($grpc.Success) { @($grpc.Groups['v'].Value -split ',\s*') } else { @() }
    }
}

Test-Case '资产通道默认关:scene / guild / trade 不带签名密钥与指纹,不生成 NetworkPolicy,只删以前留下的同名 Secret 与策略;trade 的 AssetOp 仍是两行' {
    foreach ($out in @($devOut, $agonesOut, $guildInfraRun.Output)) {
        Assert-Match -Text $out -Pattern 'AssetOps: off NetworkPolicies: off' -Because '入口要打出两个开关的取值'
        Assert-NotMatch -Text $out -Pattern '(?m)^kind: NetworkPolicy' -Because '默认不生成准入策略'
        # DryRun 的 Secret 创建只有 apply 记录(不打印密钥 manifest);注释和删除旧 Secret 的记录不算创建。
        Assert-NotMatch -Text $out -Pattern '(?m)^\[dry-run\] kubectl apply\b[^\r\n]*# Secret asset-op-secret-' -Because '默认不建签名密钥 Secret'
    }
    $scene = Select-ManifestByName -Output $devOut -Name 'scene'
    $fleet = Select-ManifestByName -Output $agonesOut -Name 'scene'
    foreach ($block in @($scene, $fleet)) {
        Assert-NotMatch -Text $block -Pattern 'MMORPG_ASSET_OP_SECRET_(?:GUILD|TRADE)\b|asset-op-secret-(?:guild|trade)\b' -Because 'AssetOps off 只禁用 Guild/Trade 两条可选资产通道；必需的 MAIL、目录和SQL签名不受此开关控制'
    }
    Assert-Match -Text $devOut -Pattern 'kubectl delete networkpolicy scene-grpc-allowlist -n mmorpg-zone-contract-test --ignore-not-found' -Because '开关不粘滞:以前开过留下的策略要删'
    foreach ($secretName in @('asset-op-secret-guild', 'asset-op-secret-trade')) {
        Assert-Match -Text $devOut -Pattern ('kubectl delete secret ' + $secretName + ' -n mmorpg-zone-contract-test --ignore-not-found') -Because 'zone 里的验签密钥同样不粘滞'
        Assert-Match -Text $guildInfraRun.Output -Pattern ('kubectl delete secret ' + $secretName + ' -n contract-battle-infra --ignore-not-found') -Because 'infra 里的签名密钥同样不粘滞'
    }
    foreach ($policy in @('guild-grpc-allowlist', 'match-grpc-allowlist')) {
        Assert-Match -Text $guildInfraRun.Output -Pattern ('kubectl delete networkpolicy ' + $policy + ' -n contract-battle-infra --ignore-not-found') -Because 'infra 的准入策略同样不粘滞'
    }
    foreach ($svc in @('guild', 'trade')) {
        $deploy = @(Get-ManifestBlocks -Output $guildInfraRun.Output | Where-Object { $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch ('(?m)^  name: ' + $svc + '\s*$') }) | Select-Object -First 1
        Assert-True -Condition ($null -ne $deploy) -Because "infra-up 产物里应当有 $svc 的 Deployment"
        Assert-NotMatch -Text $deploy -Pattern '(?m)^\s*- name: MMORPG_ASSET_OP_SECRET_' -Because "$svc 关着时不注入签名密钥(manifest 也不许手写,Add-GoSvcSecretEnv 会拒)"
        Assert-NotMatch -Text $deploy -Pattern '(?m)^\s+mmorpg\.io/asset-op-secret-\w+-hash:|PLACEHOLDER_' -Because "$svc 关着时指纹占位整行删掉,Pod 模板与开关引入之前相同"
    }
    $tradeCm = Select-ManifestByName -Output $guildInfraRun.Output -Name 'go-svc-trade-config'
    $tradeEmbedded = Get-ConfigMapEmbeddedText -Block $tradeCm -FileName 'trade.yaml'
    Assert-Equal -Expected "AssetOp:`n  Enabled: false`n  SecretEnv: MMORPG_ASSET_OP_SECRET_TRADE" -Actual (ConvertTo-NonBlankLines -Text (Get-TopLevelYamlBlockText -Text $tradeEmbedded -Key 'AssetOp')) -Because 'trade 关着时只写 Enabled 与 SecretEnv 两行'
}

Test-Case 'Assert-AssetOpSecretShape:未设置 / 含空白 / 短于 32 字节一律拒绝,报错点名变量、不回显值' {
    $cases = @(
        @{ Value = ''; Pattern = '未设置' }
        @{ Value = 'contract asset op 0123456789abcdef0123456'; Pattern = '空白' }
        @{ Value = "contract-asset-op-0123456789abcdef0123456`t"; Pattern = '空白' }
        @{ Value = ('x' * 31); Pattern = '< 32' }
    )
    foreach ($case in $cases) {
        $message = ''
        try { Assert-AssetOpSecretShape -EnvName 'MMORPG_ASSET_OP_SECRET_GUILD' -Value $case.Value } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern $case.Pattern -Because "形状不对必须拒绝:$($case.Pattern)"
        Assert-Match -Text $message -Pattern 'MMORPG_ASSET_OP_SECRET_GUILD' -Because '报错要点名变量'
        if ($case.Value.Length -gt 0) { Assert-True -Condition (-not $message.Contains($case.Value.Trim())) -Because '报错不得回显密钥' }
    }
    Assert-AssetOpSecretShape -EnvName 'MMORPG_ASSET_OP_SECRET_GUILD' -Value ('x' * 32)
}

Test-Case 'Get-AssetOpConsistencyVerdict:调用方没开恒放行;开着时 scene 必须拿同一把' {
    Assert-Equal -Expected 'ok' -Actual (Get-AssetOpConsistencyVerdict -CallerHash $null -SceneHash $null) -Because '两边都没开'
    Assert-Equal -Expected 'ok' -Actual (Get-AssetOpConsistencyVerdict -CallerHash '' -SceneHash 'abcdef012345') -Because '打开顺序:先给 scene 密钥,调用方再开'
    Assert-Equal -Expected 'scene-off-while-caller-on' -Actual (Get-AssetOpConsistencyVerdict -CallerHash 'abcdef012345' -SceneHash '') -Because '调用方开着而 scene 关:该 zone 的资产 RPC 全部被拒'
    Assert-Equal -Expected 'key-mismatch' -Actual (Get-AssetOpConsistencyVerdict -CallerHash 'abcdef012345' -SceneHash 'abcdef012346') -Because '密钥不是同一把'
    Assert-Equal -Expected 'ok' -Actual (Get-AssetOpConsistencyVerdict -CallerHash 'abcdef012345' -SceneHash 'abcdef012345') -Because '同一把密钥'
}

Test-Case 'New-GrpcAllowListNetworkPolicyYaml:逐个列端口(不用 endPort);gRPC 口不得同时对所有来源开放' {
    $yaml = New-GrpcAllowListNetworkPolicyYaml -PolicyName 'p' -TargetApps @('a') -GrpcPort 50300 -OpenTcpPorts @(9220, 9220) -AllowedApps @('x', 'y')
    Assert-NotMatch -Text $yaml -Pattern 'endPort' -Because '不支持 endPort 的网络插件只认起始端口,会把其余端口全拦掉'
    $shape = Get-GrpcAllowListShape -Block $yaml
    Assert-Equal -Expected '9220' -Actual ($shape.OpenPorts -join ',') -Because '开放端口去重'
    Assert-Equal -Expected 50300 -Actual $shape.GrpcPort -Because 'gRPC 口单独一条'
    Assert-Equal -Expected 'x,y' -Actual ($shape.AllowedApps -join ',') -Because '放行名单原样写出'
    Assert-Match -Text $yaml -Pattern '- namespaceSelector: \{\}\s*\n\s+podSelector:' -Because '调用方分布在 infra 与各 zone namespace,两个选择器写在同一项里 = 任意 namespace 的这些 Pod'
    $message = ''
    try { $null = New-GrpcAllowListNetworkPolicyYaml -PolicyName 'p' -TargetApps @('a') -GrpcPort 50300 -OpenTcpPorts @(50300) -AllowedApps @('x') } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern '形同虚设' -Because 'gRPC 口出现在开放端口里必须拒绝'
    $ports = Get-ManifestContainerPorts -ManifestContent "ports:`n  - containerPort: 50300`n    name: grpc`n  - name: rpc`n    containerPort: 20000`n`t`t- containerPort: 9220`r`n"
    Assert-Equal -Expected '9220,20000,50300' -Actual ($ports -join ',') -Because 'containerPort 不论写在列表项首行还是第二行、制表符缩进或 CRLF 都要认出来'
}

Test-Case '-AssetOps on 的组合与密钥校验都在任何写操作之前拒绝,报错不回显密钥' {
    $noPolicy = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-AssetOps', 'on')) -Env $assetOpEnv
    Assert-True -Condition ($noPolicy.ExitCode -ne 0) -Because '资产通道打开而 scene 的 gRPC 口不设防必须拒绝'
    Assert-Match -Text $noPolicy.Output -Pattern '-NetworkPolicies on' -Because '报错要说明缺哪个开关'
    $missing = Invoke-DeployDryRun -Arguments ($BaseArgs + $assetOpBoth)
    Assert-True -Condition ($missing.ExitCode -ne 0) -Because '没注入签名密钥必须拒绝(各档位都没有回落值)'
    Assert-Match -Text $missing.Output -Pattern 'MMORPG_ASSET_OP_SECRET_GUILD' -Because '报错要点名变量'
    $sameValue = 'contract-asset-op-same-0123456789abcdef01234'
    $same = Invoke-DeployDryRun -Arguments ($BaseArgs + $assetOpBoth) -Env @{ MMORPG_ASSET_OP_SECRET_GUILD = $sameValue; MMORPG_ASSET_OP_SECRET_TRADE = $sameValue }
    Assert-True -Condition ($same.ExitCode -ne 0) -Because '两个调用方共用一把密钥必须拒绝'
    Assert-Match -Text $same.Output -Pattern 'MMORPG_ASSET_OP_SECRET_TRADE' -Because '报错点名两个变量'
    $reuse = Invoke-DeployDryRun -Arguments ($BaseArgs + $assetOpBoth) -Env @{ MMORPG_GATE_TOKEN_SECRET = $assetOpGuildSecret; MMORPG_ASSET_OP_SECRET_GUILD = $assetOpGuildSecret; MMORPG_ASSET_OP_SECRET_TRADE = $assetOpTradeSecret }
    Assert-True -Condition ($reuse.ExitCode -ne 0) -Because '与其它口令复用必须拒绝'
    Assert-Match -Text $reuse.Output -Pattern 'MMORPG_GATE_TOKEN_SECRET' -Because '报错点名被复用的那一把'
    foreach ($run in @($noPolicy, $missing, $same, $reuse)) {
        Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '组合校验与密钥解析先于任何写操作,不留半截部署'
    }
    foreach ($run in @($same, $reuse)) {
        Assert-True -Condition (-not $run.Output.Contains('0123456789abcdef0123')) -Because '报错不得回显密钥'
    }
}

Test-Case 'zone-up -AssetOps on:scene(Deployment 与 Fleet)经 secretKeyRef 拿两把验签密钥、带指纹;Secret 与准入策略先于 scene;gate 不带' {
    Assert-Equal -Expected 0 -Actual $assetZoneRun.ExitCode -Because "zone-up 打开两个开关必须成功。输出: $($assetZoneRun.Output)"
    Assert-Equal -Expected 0 -Actual $assetAgonesRun.ExitCode -Because "Agones 形态打开两个开关必须成功。输出: $($assetAgonesRun.Output)"
    $guildHash = Get-InjectedSecretFingerprint -SecretName 'asset-op-secret-guild' -Value $assetOpGuildSecret
    $tradeHash = Get-InjectedSecretFingerprint -SecretName 'asset-op-secret-trade' -Value $assetOpTradeSecret
    foreach ($pair in @(@{ Run = $assetZoneRun; Kind = 'Deployment' }, @{ Run = $assetAgonesRun; Kind = 'Fleet' })) {
        $out = $pair.Run.Output -replace "`r`n", "`n"
        $scene = Select-ManifestByName -Output $out -Name 'scene'
        $sceneAt = Get-WorkloadOffset -Output $out -Kind $pair.Kind -Name 'scene'
        Assert-True -Condition ($sceneAt -ge 0) -Because '夹具前提:找得到 scene 工作负载'
        Assert-Match -Text $scene -Pattern ('(?m)^kind: ' + $pair.Kind) -Because "要核对的是 scene 的 $($pair.Kind)"
        foreach ($caller in @(@{ Env = 'MMORPG_ASSET_OP_SECRET_GUILD'; Secret = 'asset-op-secret-guild'; Hash = $guildHash }, @{ Env = 'MMORPG_ASSET_OP_SECRET_TRADE'; Secret = 'asset-op-secret-trade'; Hash = $tradeHash })) {
            Assert-Match -Text $scene -Pattern ('- name: ' + $caller.Env + '\s+valueFrom:\s+secretKeyRef:\s+name: ' + $caller.Secret + '\s+key: secret\s*\r?\n') -Because "scene 验签要 $($caller.Env),只经 Secret 注入、不写 optional"
            Assert-Match -Text $scene -Pattern ('mmorpg\.io/' + $caller.Secret + '-hash: "' + $caller.Hash + '"') -Because '指纹与调用方同一算法、同一域分隔,换密钥即滚动、跨命令可核对'
            $secretAt = $out.IndexOf("# Secret $($caller.Secret) (key secret; value hidden)")
            Assert-True -Condition ($secretAt -ge 0 -and $secretAt -lt $sceneAt) -Because 'secretKeyRef 不是 optional,Secret 必须先于 scene 落地'
        }
        $policyAt = $out.IndexOf('name: scene-grpc-allowlist')
        Assert-True -Condition ($policyAt -ge 0 -and $policyAt -lt $sceneAt) -Because '准入策略先到,新 scene Pod 从第一秒起受保护'
        Assert-True -Condition (-not $out.Contains($assetOpGuildSecret) -and -not $out.Contains($assetOpTradeSecret)) -Because '密钥值绝不进产物'
        Assert-NotMatch -Text $out -Pattern 'kubectl delete (secret asset-op-secret-|networkpolicy scene-grpc-allowlist)' -Because '开着时不删自己要用的 Secret 与策略'
    }
    $gate = Select-ManifestByName -Output $assetZoneRun.Output -Name 'gate'
    Assert-NotMatch -Text $gate -Pattern 'MMORPG_ASSET_OP_SECRET_|asset-op-secret' -Because '只有 scene 验签,gate 不该拿资产密钥'
}

Test-Case 'scene 的 gRPC 口准入:目标 = 三种 scene 池,放行名单逐字钉住,开放端口覆盖 scene 清单(Deployment 与 Fleet)声明的其它全部端口' {
    $policy = Select-ManifestByName -Output $assetZoneRun.Output -Name 'scene-grpc-allowlist'
    Assert-True -Condition ($null -ne $policy) -Because '打开时 zone 里要有 scene-grpc-allowlist'
    $shape = Get-GrpcAllowListShape -Block $policy
    Assert-Equal -Expected 'scene,scene-world,scene-instance' -Actual ($shape.TargetApps -join ',') -Because 'legacy 单池与拆分两池都要被选中'
    Assert-Equal -Expected 'scene-manager,match,player-inbox,guild,trade,mail' -Actual ($shape.AllowedApps -join ',') -Because '调用方名单(k8s_deploy.ps1 $script:GrpcAllowLists 注释里的调用点);增删要同时改这里'
    $scene = Select-ManifestByName -Output $devOut -Name 'scene'
    $grpcDeclared = [regex]::Match($scene, 'containerPort: (?<p>\d+)\s+name: grpc')
    Assert-True -Condition $grpcDeclared.Success -Because '夹具前提:scene Deployment 声明了 grpc 口'
    Assert-Equal -Expected ([int]$grpcDeclared.Groups['p'].Value) -Actual $shape.GrpcPort -Because '受保护的必须是 scene 真正监听的 gRPC 口'
    $fleet = Select-ManifestByName -Output $agonesOut -Name 'scene'
    $declared = @(Get-ManifestContainerPorts -ManifestContent ($scene + "`n" + $fleet) | Where-Object { $_ -ne $shape.GrpcPort })
    Assert-Equal -Expected ($declared -join ',') -Actual ($shape.OpenPorts -join ',') -Because '清单里声明的其它端口(节点口、日志 sidecar)都得对所有来源开放,漏一个就断(gate ↔ scene 走节点口)'
}

Test-Case 'infra-up -AssetOps on:guild / trade 各自只拿自己那把密钥、带指纹、ConfigMap 打开通道(trade 整块搬运);guild / match 的准入策略名单逐字钉住、开放端口覆盖清单' {
    Assert-Equal -Expected 0 -Actual $assetInfraRun.ExitCode -Because "infra-up 打开两个开关必须成功。输出: $($assetInfraRun.Output)"
    $out = $assetInfraRun.Output -replace "`r`n", "`n"
    $specs = @(
        @{ Svc = 'guild'; Env = 'MMORPG_ASSET_OP_SECRET_GUILD'; Other = 'MMORPG_ASSET_OP_SECRET_TRADE'; Secret = 'asset-op-secret-guild'; Value = $assetOpGuildSecret }
        @{ Svc = 'trade'; Env = 'MMORPG_ASSET_OP_SECRET_TRADE'; Other = 'MMORPG_ASSET_OP_SECRET_GUILD'; Secret = 'asset-op-secret-trade'; Value = $assetOpTradeSecret }
    )
    foreach ($spec in $specs) {
        $deploy = @(Get-ManifestBlocks -Output $out | Where-Object { $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch ('(?m)^  name: ' + $spec.Svc + '\s*$') }) | Select-Object -First 1
        Assert-True -Condition ($null -ne $deploy) -Because "infra-up 产物里应当有 $($spec.Svc) 的 Deployment"
        $envPattern = if ($spec.Svc -eq 'guild') {
            '- name: ' + $spec.Env + '\s+valueFrom:\s+secretKeyRef: \{name: ' + $spec.Secret + ', key: secret, optional: false\}'
        } else {
            '- name: ' + $spec.Env + '\s+valueFrom:\s+secretKeyRef:\s+name: ' + $spec.Secret + '\s+key: secret\s*\r?\n'
        }
        Assert-Match -Text $deploy -Pattern $envPattern -Because "$($spec.Svc) 签名密钥只经本域Secret注入，inline显式optional:false与block缺省同样必须存在"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($deploy, 'name: ' + $spec.Env).Count) -Because '只能注入一份'
        Assert-NotMatch -Text $deploy -Pattern ('(?m)^\s*- name: ' + $spec.Other) -Because '每个调用方只拿自己那把(不变量 I6)'
        $hash = Get-InjectedSecretFingerprint -SecretName $spec.Secret -Value $spec.Value
        Assert-Match -Text $deploy -Pattern ('mmorpg\.io/' + $spec.Secret + '-hash: "' + $hash + '"') -Because '指纹与 scene 一侧同一算法,跨命令核对靠它'
        Assert-NotMatch -Text $deploy -Pattern 'PLACEHOLDER_' -Because '占位必须全部替换'
        $secretAt = $out.IndexOf("# Secret $($spec.Secret) (key secret; value hidden)")
        Assert-True -Condition ($secretAt -ge 0 -and $secretAt -lt (Get-WorkloadOffset -Output $out -Kind 'Deployment' -Name $spec.Svc)) -Because 'Secret 必须先于引用它的 Deployment'
    }
    Assert-True -Condition (-not $out.Contains($assetOpGuildSecret) -and -not $out.Contains($assetOpTradeSecret)) -Because '密钥值绝不进产物'
    $guildEmbedded = Get-ConfigMapEmbeddedText -Block (Select-ManifestByName -Output $out -Name 'go-svc-guild-config') -FileName 'guild.yaml'
    $expectedGuild = (Get-TopLevelYamlBlockText -Text $guildYamlText -Key 'AssetOp') -replace '(?m)^  Enabled:.*$', '  Enabled: true'
    Assert-Equal -Expected (ConvertTo-NonBlankLines -Text $expectedGuild) -Actual (ConvertTo-NonBlankLines -Text (Get-TopLevelYamlBlockText -Text $guildEmbedded -Key 'AssetOp')) -Because 'guild 打开:整块原样,第一层 Enabled 改 true(Wake 子块的 Enabled 不动)'
    $tradeEmbedded = Get-ConfigMapEmbeddedText -Block (Select-ManifestByName -Output $out -Name 'go-svc-trade-config') -FileName 'trade.yaml'
    $expectedTrade = (Get-TopLevelYamlBlockText -Text $tradeYamlTextForAssetOp -Key 'AssetOp') -replace '(?m)^  Enabled:.*$', '  Enabled: true'
    Assert-Equal -Expected (ConvertTo-NonBlankLines -Text $expectedTrade) -Actual (ConvertTo-NonBlankLines -Text (Get-TopLevelYamlBlockText -Text $tradeEmbedded -Key 'AssetOp')) -Because 'trade 打开:落地恢复与 Cleanup* 键连同 Enabled=true 一起进 ConfigMap(go/trade/etc/trade.yaml 注释)'
    Assert-Match -Text $tradeEmbedded -Pattern '(?m)^  SecretEnv: MMORPG_ASSET_OP_SECRET_TRADE\s*$' -Because 'SecretEnv 必须是部署侧注入的变量名'
    $policies = @(
        @{ Name = 'guild-grpc-allowlist'; Svc = 'guild'; Allowed = 'client-rpc-router,data-service'; Port = 50300 }
        @{ Name = 'match-grpc-allowlist'; Svc = 'match'; Allowed = 'client-rpc-router,guild,gate'; Port = 50500 }
    )
    foreach ($p in $policies) {
        $policy = Select-ManifestByName -Output $out -Name $p.Name
        Assert-True -Condition ($null -ne $policy) -Because "打开时 infra 里要有 $($p.Name)"
        $shape = Get-GrpcAllowListShape -Block $policy
        Assert-Equal -Expected $p.Svc -Actual ($shape.TargetApps -join ',') -Because '策略只选中本服务'
        Assert-Equal -Expected $p.Allowed -Actual ($shape.AllowedApps -join ',') -Because "$($p.Svc) 的 gRPC 调用方名单;增删要同时改这里"
        Assert-Equal -Expected $p.Port -Actual $shape.GrpcPort -Because 'gRPC 口取 $GoSvcCatalogue 的 Port'
        $manifest = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) "deploy/k8s/manifests/go-svc/$($p.Svc).yaml"))
        $declared = @(Get-ManifestContainerPorts -ManifestContent $manifest | Where-Object { $_ -ne $p.Port })
        Assert-Equal -Expected ($declared -join ',') -Actual ($shape.OpenPorts -join ',') -Because '清单里声明的其它端口(metrics)要对所有来源开放'
        $policyAt = $out.IndexOf("name: $($p.Name)")
        $deployAt = Get-WorkloadOffset -Output $out -Kind 'Deployment' -Name $p.Svc
        Assert-True -Condition ($policyAt -ge 0 -and $deployAt -ge 0 -and $policyAt -lt $deployAt) -Because '策略先于 Deployment'
    }
}

function Get-TestPodTemplateAppLabel([string]$ManifestText) {
    $text = $ManifestText -replace "`r`n", "`n"
    $metadata = [regex]::Matches($text, '(?m)^  template:[ \t]*\n    metadata:[ \t]*\n(?<body>(?:^[ ]{6,}.*\n?)+)')
    if ($metadata.Count -ne 1) { throw '夹具必须恰有一份 spec.template.metadata，不能拿 Service 或顶层标签证明 Pod' }
    $labels = [regex]::Matches($metadata[0].Groups['body'].Value, '(?m)^      labels:[ \t]*(?<inline>\{[^\r\n]*\})?[ \t]*\n(?<block>(?:^[ ]{8,}.*\n?)*)')
    if ($labels.Count -ne 1) { throw 'Pod 模板 labels 块缺失、重复或格式未知' }
    if ($labels[0].Groups['inline'].Success) {
        $map = $labels[0].Groups['inline'].Value.Trim('{}')
        $apps = [regex]::Matches($map, '(?:^|,)[ \t]*app:[ \t]*(?<value>[a-z][a-z0-9-]*)[ \t]*(?=,|$)')
    } else {
        $apps = [regex]::Matches($labels[0].Groups['block'].Value, '(?m)^        app:[ \t]*(?<value>[a-z][a-z0-9-]*)[ \t]*$')
    }
    if ($apps.Count -ne 1) { throw 'Pod labels.app 必须恰有一个明确标量，缺失或重复均拒绝' }
    return $apps[0].Groups['value'].Value
}

Test-Case 'gRPC 准入名单里的每个 app 标签都对应一个真实工作负载(写错一个字母 = 那个调用方被拦)' {
    $blockLabels = "spec:`n  template:`n    metadata:`n      labels:`n        app: guild`n        tier: go-svc`n    spec:`n      containers: []`n"
    $inlineLabels = $blockLabels.Replace("labels:`n        app: guild`n        tier: go-svc", 'labels: {app: guild, tier: go-svc}')
    foreach ($fixture in @($blockLabels, $inlineLabels)) {
        Assert-Equal -Expected 'guild' -Actual (Get-TestPodTemplateAppLabel $fixture) -Because 'block 与 inline 两种正式 YAML 标签写法同义'
        Assert-True -Condition ((Get-TestPodTemplateAppLabel ($fixture.Replace('app: guild', 'app: other'))) -cne 'guild') -Because '错误 app 不能被别处 guild 文本冒充'
    }
    foreach ($badFixture in @(
        "metadata:`n  labels: {app: guild}`nspec:`n  ports: []`n",
        $inlineLabels.Replace('app: guild, tier:', 'app: guild, app: other, tier:'),
        $blockLabels.Replace("        app: guild", "        app: guild`n        app: other"),
        $blockLabels.Replace('        app: guild', '        # app: guild')
    )) {
        $rejected = $false
        try { $null = Get-TestPodTemplateAppLabel $badFixture } catch { $rejected = $true }
        Assert-True -Condition $rejected -Because '顶层标签、重复键或注释中的 app 都不能充当 Pod 准入标签'
    }
    $knownCpp = @('gate', 'scene', 'scene-world', 'scene-instance')
    $deployScript = [IO.File]::ReadAllText((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'))
    $tableText = [regex]::Match($deployScript, '(?s)\$script:GrpcAllowLists = \[ordered\]@\{(?<t>.*?)\r?\n\}').Groups['t'].Value
    Assert-True -Condition ($tableText.Length -gt 0) -Because '夹具前提:找得到 $script:GrpcAllowLists'
    $apps = @([regex]::Matches($tableText, "'([a-z][a-z-]*)'") | ForEach-Object { $_.Groups[1].Value } | Where-Object { $_ -notmatch 'allowlist$' } | Sort-Object -Unique)
    Assert-True -Condition ($apps.Count -ge 8) -Because '夹具前提:名单解析出来了'
    foreach ($app in $apps) {
        if ($knownCpp -contains $app) { continue }
        $manifestPath = Join-Path (Get-RepoRoot) "deploy/k8s/manifests/go-svc/$app.yaml"
        Assert-True -Condition (Test-Path $manifestPath) -Because "名单里的 $app 没有对应的 go-svc manifest"
        Assert-Equal -Expected $app -Actual (Get-TestPodTemplateAppLabel ([IO.File]::ReadAllText($manifestPath))) -Because "$app 的 Pod 模板 labels.app 必须精确一致，支持 block / inline YAML"
    }
}

Test-Case '普通 gate/scene Deployment 必须先创建被 emptyDir 遮蔽的日志父目录' {
    foreach ($name in @('gate','scene')) {
        $block = Select-ManifestByName -Output $devOut -Name $name
        Assert-Match -Text $block -Pattern ('args: \["mkdir -p /app/bin/logs/cpp_nodes && .*&& \./' + $name + '"\]') -Because 'Node 构造器在初始化前就打开 logs/cpp_nodes/<role>，父目录必须已经存在'
    }
}
Test-Case 'Agones Scene Fleet 同样必须在进程启动前建立日志父目录' {
    $fleet = Select-ManifestByName -Output $agonesOut -Name 'scene'
    Assert-Match -Text $fleet -Pattern 'kind: Fleet' -Because '必须检查真正的 Fleet 而非普通 Deployment'
    Assert-Match -Text $fleet -Pattern 'args: \["mkdir -p /app/bin/logs/cpp_nodes && .*&& \./scene"\]' -Because 'Fleet 的 emptyDir 与 Deployment 同样遮蔽镜像目录'
}

# ─────────────────────────────────────────────────────────────────
# C++ 日志 sidecar(见 docs/ops/grafana-loki-local-logs.md §6)
# ─────────────────────────────────────────────────────────────────

Test-Case '日志保留:业务容器必须自带清理循环,且 node-logs 有 sizeLimit 兜底' {
    foreach ($name in @('gate','scene')) {
        $block = Select-ManifestByName -Output $devOut -Name $name
        # muduo 每 8MiB 滚一个新文件且从不删旧文件;没有这两道闸,长跑节点会把节点盘写满,
        # 而本机 kind 的 evictionHard 全是 0%,盘满的表现是 MySQL/Kafka/etcd 一起 ENOSPC。
        Assert-Match -Text $block -Pattern 'ls -1t /app/bin/logs/cpp_nodes/\*\.log .*tail -n \+9 .*rm -f' -Because '业务容器里必须有"只留最近若干个日志文件"的清理循环'
        Assert-Match -Text $block -Pattern 'name: node-logs\s+emptyDir: \{ sizeLimit: \S+ \}' -Because 'emptyDir 不封顶 = 一个 Pod 能把整个节点盘写满'
    }
}

Test-Case 'sidecar 必须是 initContainers 里的原生 sidecar(restartPolicy: Always)' {
    # 普通容器形态下:Agones 给 GameServer Pod 写死 restartPolicy: Never,sidecar OOM 后永不重启
    # 且 Agones 只看游戏容器不会报错;Pod 终止时它又与业务容器同时收 SIGTERM,drain 期日志整段丢。
    foreach ($pair in @(@{ Out = $devOut; Name = 'gate' }, @{ Out = $agonesOut; Name = 'scene' })) {
        $block = Select-ManifestByName -Output $pair.Out -Name $pair.Name
        Assert-Match -Text $block -Pattern '(?s)initContainers:\s+- name: log-sidecar.*?restartPolicy: Always' -Because 'sidecar 必须放 initContainers 并显式声明 restartPolicy: Always'
        # 只能有一份:普通容器带 restartPolicy 要 API server 开了 ContainerRestartRules 门控才收,
        # 1.28~1.33 会报 containers[N].restartPolicy: Forbidden,整份清单 apply 不上去。
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, '- name: log-sidecar').Count) -Because 'sidecar 容器片段只应出现在 initContainers 一处'
        Assert-Match -Text $block -Pattern 'runAsNonRoot: true' -Because '观测容器没有理由把 root 塞回业务 Pod'
        # 值带引号(全数字哈希不加引号会被 YAML 解析成数字、API server 拒收),两种写法都认。
        Assert-Match -Text $block -Pattern 'mmorpg\.io/cpp-log-sidecar-config-hash: "?[0-9a-f]{12}"?' -Because '配置只改 ConfigMap 不会让 Alloy 重读,必须靠 pod 模板注解触发滚动'
    }
}

Test-Case 'k8s label value 契约:生成的标签值不能是裸 - (YAML 非法 + label 校验器拒)' {
    # 真实事故:battle 路径曾用 "-" 当 zone 占位,渲染出 `mmorpg.io/zone: -`,
    # YAML 把行尾裸 - 当块序列项,infra-up / all-up 在这里整体中断。
    # 注意 kubectl apply --dry-run=client 查不出 label value 非法(它只按 schema 校验),
    # 所以这条必须在生成期断。
    foreach ($out in @($devOut, $agonesOut, $battleInfraRun.Output)) {
        Assert-NotMatch -Text $out -Pattern '(?m)^\s+[A-Za-z0-9][-A-Za-z0-9_./]*:\s+-\s*$' -Because '标签/字段值为裸 - 既是非法 YAML 也是非法 label value'
    }
}

Test-Case 'battle 是全局池:它的 sidecar ConfigMap 不能带 zone 标签,zone 的必须带且加引号' {
    $zoneCm = Select-ManifestByName -Output $devOut -Name 'cpp-log-sidecar'
    Assert-Match -Text $zoneCm -Pattern 'mmorpg\.io/zone: "contract-test"' -Because 'zone 标签值要加引号,否则将来纯数字 zone 名会被 kubectl 当数字拒掉'
    $battleCm = Select-ManifestByName -Output $battleInfraRun.Output -Name 'cpp-log-sidecar'
    Assert-NotMatch -Text $battleCm -Pattern 'mmorpg\.io/zone' -Because 'battle 不属于任何 zone;写个占位值会被 {mmorpg.io/zone=<真 zone>} 的选择器误选'
}

Test-Case '-NoCppLogSidecar 必须产出与加这个功能之前一致的清单' {
    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-NoCppLogSidecar','-SkipGoSvc','-SkipJavaSvc'))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because '关掉 sidecar 的路径必须照样能生成'
    Assert-NotMatch -Text $run.Output -Pattern 'log-sidecar' -Because '关掉后不能残留任何 sidecar 容器/卷'
    Assert-NotMatch -Text $run.Output -Pattern 'initContainers' -Because '关掉后不能留下空的 initContainers 键(空列表 = null,API server 拒)'
    Assert-NotMatch -Text $run.Output -Pattern 'cpp-log-sidecar-config-hash' -Because '关掉后 pod 模板不该带 sidecar 配置注解'
}

Test-Case 'Java gateway 冷启动必须有独立 startupProbe 预算并保留就绪与存活探针' {
    $block = Select-ManifestByName -Output $devOut -Name 'gateway'
    # 2026-10-15:三个探针改打独立管理端口 8082(端口与 ConfigMap 的一致性见下一个用例)。
    Assert-Match -Text $block -Pattern 'startupProbe:\s+httpGet:\s+path: /actuator/health/readiness\s+port: 8082\s+periodSeconds: 10\s+timeoutSeconds: 5\s+failureThreshold: 60' -Because 'Spring/JPA 初始化期间不能被 liveness 的短失败预算反复杀死'
    Assert-Match -Text $block -Pattern 'readinessProbe:\s+httpGet:\s+path: /actuator/health/readiness\s+port: 8082\s+initialDelaySeconds: 15\s+periodSeconds: 10' -Because '冷启动后仍必须保留 HTTP readiness 门禁'
    Assert-Match -Text $block -Pattern 'livenessProbe:\s+httpGet:\s+path: /actuator/health/liveness\s+port: 8082\s+initialDelaySeconds: 30\s+periodSeconds: 20' -Because 'startupProbe 不能取代运行期存活检查'
    # liveness 不能落到聚合的 /actuator/health:那会把 DB / Redis 等外部依赖的抖动算进存活,依赖一抖就重启全部副本。
    Assert-NotMatch -Text $block -Pattern 'path: /actuator/health\s' -Because '探针必须走 readiness / liveness 分组,不能用聚合健康端点'
}
Test-Case 'Java gateway 探针走独立管理端口:ConfigMap 的 management.server.port == 三个探针的端口,且不等于业务端口 server.port' {
    # 2026-10-15:探针原先与业务共用 8081 的 Tomcat 线程池,MySQL 慢 / 不可达、Redis 卡住或被不限流的端点刷时,业务线程
    # 耗尽,liveness 跟着超时,两个副本被一起杀。管理端口让 actuator 用独立的连接器与线程池。两边任一处漂移,探针打到
    # 没有 actuator 的端口上全部失败(startupProbe 600s 后杀容器),所以按 DryRun 产物比对,不各自写死。
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'java-svc-gateway-config')
    $managementPort = Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.management.server.port'
    $serverPort = Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.server.port'
    Assert-Equal -Expected '8082' -Actual $managementPort -Because 'gateway ConfigMap 必须把 actuator 放到独立管理端口 8082'
    Assert-True -Condition ($managementPort -cne $serverPort) -Because "管理端口不能等于业务端口 server.port=$serverPort(相等就退回共用业务线程池)"
    $block = Select-ManifestByName -Output $devOut -Name 'gateway'
    $probePorts = @([regex]::Matches($block, '(?<probe>startupProbe|readinessProbe|livenessProbe):\s+httpGet:\s+path: \S+\s+port: (?<port>\d+)') | ForEach-Object { "$($_.Groups['probe'].Value)=$($_.Groups['port'].Value)" })
    Assert-Equal -Expected 'startupProbe,readinessProbe,livenessProbe' -Actual (@($probePorts | ForEach-Object { ($_ -split '=')[0] }) -join ',') -Because 'gateway Deployment 应当恰好有 startup / readiness / liveness 三个 httpGet 探针'
    foreach ($probePort in $probePorts) {
        Assert-Equal -Expected $managementPort -Actual (($probePort -split '=')[1]) -Because "gateway 的 $(($probePort -split '=')[0]) 必须打 ConfigMap 的 management.server.port"
    }
    Assert-Match -Text $block -Pattern ('containerPort: {0}\s+name: management' -f $managementPort) -Because '管理端口要在容器 ports 里声明(名 management),与探针端口一致'
}
Test-Case 'Java gateway JDBC 连接级超时:jar 内 URL 带正整数 connectTimeout / socketTimeout,ConfigMap 的 URL 原样镜像' {
    # 2026-10-16:Hikari connection-timeout 只限取连接;没有 socketTimeout 时,已取到连接后的查询在半开 TCP / MDL 排队上
    # 可以挂到内核重传放弃(约 15 分钟)。ServerListService / AssignGateService 的合并查询只有查库方自己能摘在途 future,
    # 查库方挂住期间该副本上同类请求全部 3s 后回 5xx。修复前红:两份 URL 都不带这两个参数。
    $paramsOf = {
        param([string]$Url)
        $params = @{}
        $query = ($Url -split '\?', 2) | Select-Object -Skip 1 -First 1
        foreach ($pair in @("$query" -split '&')) {
            $kv = $pair -split '=', 2
            if ($kv.Count -eq 2) { $params[$kv[0]] = $kv[1] }
        }
        return $params
    }
    $jarParams = & $paramsOf (Get-EtcValue -RelativePath 'java/gateway_node/src/main/resources/application.yaml' -KeyPath 'spring.datasource.url')
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'java-svc-gateway-config')
    $cmParams = & $paramsOf (Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.spring.datasource.url')
    foreach ($key in @('connectTimeout', 'socketTimeout')) {
        Assert-Match -Text "$($jarParams[$key])" -Pattern '^[1-9][0-9]*$' -Because "jar 内 spring.datasource.url 必须带正整数的 $key(单一真相)"
        Assert-Equal -Expected $jarParams[$key] -Actual $cmParams[$key] -Because "gateway ConfigMap 的 JDBC URL 的 $key 必须与 jar 内一致(生成器不得自带常数)"
    }
}
Test-Case 'Java gateway 必须显式限制 JVM 堆并为 native 内存保留容器预算' {
    $block = Select-ManifestByName -Output $devOut -Name 'gateway'
    Assert-Match -Text $block -Pattern 'name: JAVA_TOOL_OPTIONS\s+value: "-Xms64m -Xmx384m"' -Because '不能依赖本机 JRE 对 cgroup 内存限制的自动识别'
    Assert-Match -Text $block -Pattern 'requests:\s+cpu: 200m\s+memory: 512Mi' -Because '调度请求必须覆盖已观测的启动期常驻内存'
    Assert-Match -Text $block -Pattern 'limits:\s+cpu: "1"\s+memory: 1Gi' -Because '堆外内存包括 metaspace、线程栈与直接缓冲区，不能把最大堆等同容器内存'
}

# ─────────────────────────────────────────────────────────────────
# gateway 管理面口令(admin.api-key)只经 Secret 进容器(k8s-client-entry D91 纵深防御)
# ─────────────────────────────────────────────────────────────────
# 链路:环境变量 MMORPG_GATEWAY_ADMIN_API_KEY → zone namespace 的 Secret gateway-admin-api-key(key api-key)→ gateway 容器
# env ADMIN_APIKEY(secretKeyRef)→ 挂载的 application.yaml 里 admin.api-key: "${ADMIN_APIKEY}"。实现在 k8s_deploy.ps1 的
# Initialize-InjectedSecrets / New-JavaSvcConfigMapYaml / Apply-JavaSvcManifests / Apply-InjectedSecret / Add-PodTemplateAnnotation /
# Get-InjectedSecretFingerprint;这里只断 DryRun 的可观察输出。非 DryRun(假 kubectl)下缺失 / 占位 / 过短的整句报错与"零写操作"
# 由 k8s_client_entry_contract.tests.ps1 钉住,本节补 DryRun 路径与产物形状。
# 两次 dev 运行都显式给出该变量(空串 = 未设置,回落占位值),结论不受跑测机器上残留的同名环境变量影响;
# 都带 -SkipGoSvc:本节只看 Java 服务的产物。
$AdminKeyDevArgs = $BaseArgs + @('-SkipGoSvc')
$adminDevPlaceholderRun = Invoke-DeployDryRun -Arguments $AdminKeyDevArgs -Env @{ MMORPG_GATEWAY_ADMIN_API_KEY = '' }
$adminDevSameKeyRun = Invoke-DeployDryRun -Arguments $AdminKeyDevArgs -Env @{ MMORPG_GATEWAY_ADMIN_API_KEY = $ProdEnv.MMORPG_GATEWAY_ADMIN_API_KEY }

# 取 gateway Deployment Pod 模板上的口令指纹注解值。形状不对(缺失 / 多于一处 / 不在 template.metadata.annotations 下 /
# 没加双引号 / 不是 12 位小写十六进制)一律 throw,不返回半截结果。
function Get-GatewayAdminApiKeyHash {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output,
        [Parameter(Mandatory = $true)][string]$Label
    )
    $block = Select-ManifestByName -Output $Output -Name 'gateway'
    if ($null -eq $block) { throw "${Label}:DryRun 输出里没有 gateway 的 manifest" }
    $count = [regex]::Matches($block, 'mmorpg\.io/gateway-admin-api-key-hash').Count
    if ($count -ne 1) { throw "${Label}:gateway manifest 里口令指纹注解应恰好 1 处,实际 $count 处" }
    # Add-PodTemplateAnnotation 紧跟 template: 下的 metadata: 插入(前面可有一行生成说明注释)。值必须带双引号:
    # 12 位十六进制可能全是数字或形如 1e5…,不加引号会被 YAML 当成数字,而注解值只能是字符串,kubectl 拒收。
    # Get-ManifestBlocks 已把块内换行统一成 LF,所以这里按 \n 逐行锚定。
    $m = [regex]::Match($block, '(?m)^[ ]+template:[ ]*\n[ ]+metadata:[ ]*\n(?:[ ]*#[^\n]*\n)*[ ]+annotations:[ ]*\n[ ]+mmorpg\.io/gateway-admin-api-key-hash: "(?<hash>[0-9a-f]{12})"[ ]*$')
    if (-not $m.Success) { throw "${Label}:口令指纹注解必须在 Pod 模板 metadata.annotations 下,值为加双引号的 12 位小写十六进制" }
    return $m.Groups['hash'].Value
}

Test-Case '负向:prod 档位部署 gateway 却缺 MMORPG_GATEWAY_ADMIN_API_KEY 必须在任何写操作之前拒绝并点名该变量' {
    $env2 = @{} + $ProdEnv
    $env2['MMORPG_GATEWAY_ADMIN_API_KEY'] = ''
    $run = Invoke-DeployDryRun -Arguments $ProdArgs -Env $env2

    Assert-True -Condition ($run.ExitCode -ne 0) -Because "缺管理面口令必须阻断:否则 /admin/** 用 git 里公开的 change-me-in-production 鉴权。输出: $($run.Output)"
    # 其余生产密钥都已给齐($ProdEnv 基线能出产物),拒绝只能来自这一把;断 Resolve-InjectedSecret"未设置"分支的原文开头。
    Assert-Match -Text $run.Output -Pattern 'ReleaseProfile=prod 要求从环境变量 MMORPG_GATEWAY_ADMIN_API_KEY' -Because '错误必须点名缺哪个环境变量,且说清是注入缺失而不是别的失败'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '口令解析(Initialize-InjectedSecrets)先于任何写操作,不留半截部署'
    Assert-NotMatch -Text $run.Output -Pattern '# Secret gateway-admin-api-key' -Because '拒绝之后不得再写管理面口令 Secret'
}

Test-Case 'dev 档未设 MMORPG_GATEWAY_ADMIN_API_KEY:回落占位值并打警告,Secret gateway-admin-api-key 只打隐值的 dry-run 行,gateway 经 secretKeyRef 注入 ADMIN_APIKEY' {
    $run = $adminDevPlaceholderRun
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "dev 档允许回落占位值,不能拒绝(本地栈要能一键起)。输出: $($run.Output)"
    # 只断警告里不含空格的一段:Write-Warning 若被 ConsoleHost 按窗口宽度折行(重定向下是否折行未实测),折点在空格处;这一段连同前缀
    # "WARNING: gateway " 止于第 64 显示列(中文按 2 列),≥ 约 65 列的控制台不会把它拆开。旧写法连到"未设置或仍是占位串",
    # 而其后直到 "/admin/**" 是一整段无空格 token(止于约第 113 列),窗口不足约 114 列就会在"未设置"前折断。
    Assert-Match -Text $run.Output -Pattern 'admin password is a placeholder \(MMORPG_GATEWAY_ADMIN_API_KEY' -Because '回落占位值必须明说:此时 /admin/** 用公开常量鉴权,只允许本地 dev'
    # DryRun 下 Apply-InjectedSecret 只打一行隐去值的意图,不走会把整份 YAML(含明文 stringData)打出来的 Invoke-KubectlWithInputFile。
    Assert-Match -Text $run.Output -Pattern '(?m)^\[dry-run\] kubectl apply -n \S+ -f -\s+# Secret gateway-admin-api-key \(key api-key; value hidden\)\s*$' -Because 'Secret 的 dry-run 行要点名 Secret 名与 key,且声明值已隐去'

    $gateway = Select-ManifestByName -Output $run.Output -Name 'gateway'
    Assert-True -Condition ($null -ne $gateway) -Because 'DryRun 输出里应当有 gateway 的 manifest'
    # 不写 optional(= false):Secret 缺失时 Pod 卡在 CreateContainerConfigError,而不是带着空口令起来。
    Assert-Match -Text $gateway -Pattern '- name: ADMIN_APIKEY\s+valueFrom:\s+secretKeyRef:\s+name: gateway-admin-api-key\s+key: api-key[ ]*\n(?![ ]+optional:)' -Because 'ADMIN_APIKEY 必须经 secretKeyRef 取 Secret gateway-admin-api-key 的 api-key,且不得 optional'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($gateway, 'name: ADMIN_APIKEY').Count) -Because 'ADMIN_APIKEY 只能注入一份,重复的 env 键以哪条为准不该交给 kubelet 决定'
    Assert-NotMatch -Text $gateway -Pattern 'name: ADMIN_APIKEY\s+value:' -Because '口令不得以明文 value 写进 Deployment'
}

Test-Case 'gateway ConfigMap 的 admin.api-key 只写 ${ADMIN_APIKEY} 引用(dev / prod 同形),DryRun 输出里没有口令明文' {
    # 显式写引用而不靠 Spring 宽松绑定,设计意图是:env 注入一旦被删 / 改名,占位符解析不了 gateway 直接拒启(fail-closed),
    # 而不是静默回落到 jar 内公开的 change-me-in-production。
    # (Spring 运行期行为未验证:外部 ./config 覆盖 jar 内配置、嵌套占位符解析不了即拒启,都只按 Spring Boot 文档核对过,
    # 见 ingress2d 批 fix:deploy-hardening 的 RISKS;待部署后对 /admin 实测。本用例只断 ConfigMap 文本。)
    foreach ($case in @(@{ Label = 'dev'; Output = $devOut }, @{ Label = 'prod'; Output = $prodRun.Output })) {
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $case.Output -Name 'java-svc-gateway-config')
        # 用 -ceq 而不是 Assert-Equal:后者按 -ne 比较、不区分大小写,生成器模板漂成 ${admin_apikey} 也会放过;
        # 这里钉的是生成器写出的字面量,与 manifest 注入的 env 名 ADMIN_APIKEY 逐字一致。
        $apiKeyRef = Get-FlatValue -Flat $flat -KeyPath 'data.application.yaml.admin.api-key'
        Assert-True -Condition ($apiKeyRef -ceq '${ADMIN_APIKEY}') -Because "$($case.Label) 档 java-svc-gateway-config 的 admin.api-key 必须逐字(区分大小写)是对容器 env ADMIN_APIKEY 的引用,不得写口令值(实际 '$apiKeyRef')"
    }
    $plain = [regex]::Escape($ProdEnv.MMORPG_GATEWAY_ADMIN_API_KEY)
    foreach ($case in @(@{ Label = 'prod 基线'; Output = $prodRun.Output }, @{ Label = 'dev 注入同一口令'; Output = $adminDevSameKeyRun.Output })) {
        Assert-NotMatch -Text $case.Output -Pattern $plain -Because "$($case.Label):口令只经 Secret 进容器,ConfigMap / Secret 意图行 / 指纹注解都不得带明文"
    }
}

Test-Case 'gateway Pod 模板带口令指纹注解(加引号的 12 位十六进制):同一口令两次独立运行指纹相同,换口令指纹必变(轮换自动滚动 gateway)' {
    foreach ($run in @($adminDevPlaceholderRun, $adminDevSameKeyRun)) {
        Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "dev 档 DryRun 应当成功。输出: $($run.Output)"
    }
    $placeholderHash = Get-GatewayAdminApiKeyHash -Output $adminDevPlaceholderRun.Output -Label 'dev 占位口令'
    $devHash = Get-GatewayAdminApiKeyHash -Output $adminDevSameKeyRun.Output -Label 'dev 注入与 prod 基线相同的口令'
    $prodHash = Get-GatewayAdminApiKeyHash -Output $prodRun.Output -Label 'prod 基线'
    # 指纹只由口令(加 Secret 名做域分隔)决定:两次独立子进程、不同档位,同一口令必须得到同一指纹 ——
    # 否则每次重跑 zone-up 都无故滚动 gateway。
    Assert-Equal -Expected $prodHash -Actual $devHash -Because '同一口令的指纹必须稳定'
    # secretKeyRef 只在容器启动时读:只改 Secret 时 Pod 模板逐字节不变,apply 是 no-op,旧口令一直有效、新口令被拒。
    Assert-True -Condition ($placeholderHash -ne $devHash) -Because "换口令指纹必须变,否则轮换不会触发滚动(两者都是 $devHash)"
    Assert-NotMatch -Text $adminDevSameKeyRun.Output -Pattern 'admin password is a placeholder' -Because '对照:非占位口令不该打占位警告'
}

Test-Case 'gateway 写入顺序 ConfigMap → Secret → Deployment,Secret 与 Deployment 同 namespace(secretKeyRef 非 optional,Secret 必须先落地)' {
    $out = $adminDevPlaceholderRun.Output
    $cm = [regex]::Match($out, '(?m)^  name: java-svc-gateway-config\s*$')
    $secrets = [regex]::Matches($out, '(?m)^\[dry-run\] kubectl apply -n (?<ns>\S+) -f -\s+# Secret gateway-admin-api-key \(')
    $deployEnv = [regex]::Match($out, '(?m)^[ ]+- name: ADMIN_APIKEY\s*$')
    Assert-True -Condition $cm.Success -Because 'DryRun 输出里应当有 java-svc-gateway-config 的 metadata.name'
    Assert-Equal -Expected 1 -Actual $secrets.Count -Because '单 zone 的 zone-up 只写一次管理面口令 Secret'
    Assert-True -Condition $deployEnv.Success -Because 'DryRun 输出里应当有注入 ADMIN_APIKEY 的 gateway Deployment'
    Assert-True -Condition ($cm.Index -lt $secrets[0].Index) -Because 'ConfigMap 先于 Secret(Apply-JavaSvcManifests 注释约定的写序 ConfigMap → Secret → Deployment;Apply-OneGoSvc 的 login 口令 Secret 反而先于 ConfigMap,两者只共享"Secret 先于 Deployment")'
    Assert-True -Condition ($secrets[0].Index -lt $deployEnv.Index) -Because 'Secret 必须先于引用它的 Deployment:反过来新 Pod 会卡在 CreateContainerConfigError'

    # Deployment 所在的 apply 行 = 它之前最近的一条带 manifest 的 dry-run apply 行(Secret 意图行以注释结尾,不会被这条模式选中)。
    $deployApply = @([regex]::Matches($out, '(?m)^\[dry-run\] kubectl [^\r\n]*apply -n (?<ns>\S+) -f -\s*$') | Where-Object { $_.Index -lt $deployEnv.Index }) | Select-Object -Last 1
    Assert-True -Condition ($null -ne $deployApply) -Because 'gateway Deployment 之前应当有它自己的 dry-run apply 行'
    Assert-Equal -Expected $deployApply.Groups['ns'].Value -Actual $secrets[0].Groups['ns'].Value -Because 'secretKeyRef 只能引用同 namespace 的 Secret'
}

Test-Case 'Java 服务缺 manifest(目录里的 auth)时连 ConfigMap 也不 apply:DryRun 不含 java-svc-auth-config' {
    $authManifest = Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/java-svc/auth.yaml'
    Assert-True -Condition (-not (Test-Path -LiteralPath $authManifest)) -Because '本条前提:auth 在 Java 服务目录里但没有 manifest。有人补上 auth.yaml 后,本条应改为断言它的 ConfigMap 与 Deployment 成对出现'
    Assert-True -Condition ($null -ne (Select-ManifestByName -Output $devOut -Name 'java-svc-gateway-config')) -Because '对照:Java 服务路径确实走到了(gateway 的 ConfigMap 在),auth 的缺席才有意义'
    foreach ($case in @(@{ Label = 'dev'; Output = $devOut }, @{ Label = 'prod'; Output = $prodRun.Output })) {
        Assert-NotMatch -Text $case.Output -Pattern 'java-svc-auth-config' -Because "$($case.Label):没有 Deployment 消费的孤儿 ConfigMap 不该 apply(与 Apply-OneGoSvc 同口径)"
    }
}

# ─────────────────────────────────────────────────────────────────
# login 第三方登录(微信 / QQ):AuthProviders 渲染与微信 AppSecret 注入
# ─────────────────────────────────────────────────────────────────
# 链路:-LoginWeChatAppId → login ConfigMap 的 AuthProviders.WeChat(AppId + AppSecretEnv: LOGIN_WECHAT_APP_SECRET);环境变量
# MMORPG_LOGIN_WECHAT_APP_SECRET → zone namespace 的 Secret login-oauth(key wechat-app-secret)→ login 容器 env LOGIN_WECHAT_APP_SECRET
# (secretKeyRef)+ Pod 模板指纹注解 mmorpg.io/login-oauth-secret-hash。-LoginQQAppId 只写 AuthProviders.QQ.AppId,不建 Secret。
# Go 侧契约见 go/login/internal/config/config.go WeChatAuthConf / QQAuthConf 与 auth_providers.go(不展开 ${VAR}、生产只认 AppSecretEnv、
# Endpoint 生产只认 https)。实现在 k8s_deploy.ps1 的 Get-LoginAuthProviderParamErrors / New-LoginAuthProvidersYaml / Assert-LoginAuthProviderParams /
# Resolve-LoginWeChatAppSecret / Apply-OneGoSvc。纯函数按 AST 抽出来直接调(与上文 New-LoginQueueYaml 同一做法);
# Get-InjectedSecretFingerprint 一并抽出,用来核对注解值就是 login-oauth 这把密钥的指纹。
# 端到端另起 4 个 DryRun 子进程,都带 -SkipJavaSvc(本节只看 login);基线不配第三方登录的断言复用 $devOut。
$LoginAuthFunctionNames = @('Get-LoginAuthProviderParamErrors', 'New-LoginAuthProvidersYaml', 'Resolve-LoginWeChatAppSecret', 'Get-InjectedSecretFingerprint')
foreach ($loginAuthFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($LoginAuthFunctionNames -contains $loginAuthFnAst.Name) {
        Set-Item -Path "Function:script:$($loginAuthFnAst.Name)" -Value $loginAuthFnAst.Body.GetScriptBlock()
    }
}
foreach ($loginAuthFnName in $LoginAuthFunctionNames) {
    if (-not (Test-Path "Function:$loginAuthFnName")) { throw "k8s_deploy.ps1 缺少函数 $loginAuthFnName(第三方登录的渲染 / 校验 / 密钥解析被删了?后续断言无意义)" }
}

$LoginWeChatTestAppId = 'wx0123456789abcdef'
$LoginQQTestAppId = '101234567'
# ≥16 位、非占位、不含首尾空白与 ${,与 $ProdEnv 里的各密钥都不同;用来断言它不出现在任何输出里。
$LoginWeChatTestSecret = 'contract-wechat-app-secret-0123456789ab'
$LoginWeChatSandbox = 'http://login-oauth-mock.contract:18090'
$LoginAuthStagingImage = 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab'
$LoginAuthDevArgs = $BaseArgs + @('-SkipJavaSvc')
# dev:微信(含 AllowOpenId 与沙箱 http Endpoint)+ QQ。
$loginAuthDevRun = Invoke-DeployDryRun -Arguments ($LoginAuthDevArgs + @(
        '-LoginWeChatAppId', $LoginWeChatTestAppId, '-LoginWeChatEndpoint', $LoginWeChatSandbox, '-LoginWeChatAllowOpenId',
        '-LoginQQAppId', $LoginQQTestAppId)) -Env @{ MMORPG_LOGIN_WECHAT_APP_SECRET = $LoginWeChatTestSecret }
# dev:只配 QQ。微信密钥变量照样给值,证明不配微信时它既不被读进 Secret、也不出现在输出里。
$loginAuthQQOnlyRun = Invoke-DeployDryRun -Arguments ($LoginAuthDevArgs + @('-LoginQQAppId', $LoginQQTestAppId)) -Env @{ MMORPG_LOGIN_WECHAT_APP_SECRET = $LoginWeChatTestSecret }
# 负向:dev 档给了微信 AppId 却没设密钥(dev 也没有回落值)。
$loginAuthNoSecretRun = Invoke-DeployDryRun -Arguments ($LoginAuthDevArgs + @('-LoginWeChatAppId', $LoginWeChatTestAppId)) -Env @{ MMORPG_LOGIN_WECHAT_APP_SECRET = '' }
# 负向:staging 档给了 http Endpoint。密钥都给齐,拒绝只能来自参数门禁。
$loginAuthStagingEnv = @{} + $ProdEnv
$loginAuthStagingEnv['MMORPG_LOGIN_WECHAT_APP_SECRET'] = $LoginWeChatTestSecret
$loginAuthStagingHttpRun = Invoke-DeployDryRun -Arguments ($LoginAuthDevArgs + @(
        '-ReleaseProfile', 'staging', '-SkipPreflight', '-NodeImage', $LoginAuthStagingImage,
        '-LoginWeChatAppId', $LoginWeChatTestAppId, '-LoginWeChatEndpoint', $LoginWeChatSandbox)) -Env $loginAuthStagingEnv

# 按 kind + metadata.name 取 login 的 manifest 块(login.yaml 是 Service + Deployment 的多文档,整份是一个块)。找不到 / 多于一份即 throw。
function Select-LoginDeploymentBlock {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output,
        [Parameter(Mandatory = $true)][string]$Label
    )
    $found = @(Get-ManifestBlocks -Output $Output | Where-Object { $_ -cmatch '(?m)^kind: Deployment[ ]*$' -and $_ -cmatch '(?m)^  name: login[ ]*$' })
    if ($found.Count -ne 1) { throw "${Label}:DryRun 输出里 login Deployment 的 manifest 应恰好 1 份,实际 $($found.Count) 份" }
    return $found[0]
}

Test-Case 'login 第三方登录不配(基线):ConfigMap 没有 AuthProviders 段,Deployment 不引用 login-oauth、没有指纹注解,残留的 login-oauth Secret 照删' {
    $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-login-config')
    $authKeys = @(Get-FlatSubtreeKeys -Flat $loginFlat -Path 'data.login.yaml.AuthProviders')
    Assert-True -Condition ($authKeys.Count -eq 0) -Because "两个 AppId 都不给 = 不写 AuthProviders,产物与接入前逐字相同(实际出现:$($authKeys -join ', '))"
    Assert-NotMatch -Text $devOut -Pattern 'LOGIN_WECHAT_APP_SECRET|login-oauth-secret-hash|# Secret login-oauth ' -Because '不配微信:不注入 env、不带指纹注解、不建 Secret'
    Assert-Match -Text $devOut -Pattern '(?m)^\[dry-run\] kubectl delete secret login-oauth -n \S+ --ignore-not-found\s*$' -Because '不粘滞:上次开过微信登录、这次没给,要删掉残留 Secret'
    Assert-Match -Text $devOut -Pattern 'login_wechat=False login_qq=False' -Because '写路径入口的摘要行要写明两个 provider 都没开(只展示是否启用)'
}

Test-Case '纯函数 New-LoginAuthProvidersYaml:都不配返回空串;只配微信只写 AppId + AppSecretEnv(不写 AppSecret / RequireUnionId / Endpoint);AllowOpenId 写 RequireUnionId: false;QQ 只写 AppId,没有任何密钥键' {
    foreach ($profileName in @('dev', 'staging', 'prod')) {
        Assert-Equal -Expected '' -Actual (New-LoginAuthProvidersYaml -ReleaseProfile $profileName -WeChatAppSecretEnv 'LOGIN_WECHAT_APP_SECRET') -Because "$profileName 档两个 AppId 都为空必须返回空串(login 模板不写 AuthProviders)"
    }

    $wechatOnly = ConvertFrom-YamlToFlatMap -Text (New-LoginAuthProvidersYaml -WeChatAppId $LoginWeChatTestAppId -ReleaseProfile 'prod' -WeChatAppSecretEnv 'LOGIN_WECHAT_APP_SECRET')
    Assert-Equal -Expected 'AuthProviders,AuthProviders.WeChat,AuthProviders.WeChat.AppId,AuthProviders.WeChat.AppSecretEnv' `
        -Actual ((Get-FlatSubtreeKeys -Flat $wechatOnly -Path 'AuthProviders') -join ',') -Because '只配微信:只写 AppId 与 AppSecretEnv;从不写内联 AppSecret(生产拒启),RequireUnionId / Endpoint 不写 = login 缺省值'
    Assert-True -Condition ($wechatOnly.Scalars['AuthProviders.WeChat.AppId'] -ceq $LoginWeChatTestAppId) -Because 'AppId 原样写出'
    Assert-True -Condition ($wechatOnly.Scalars['AuthProviders.WeChat.AppSecretEnv'] -ceq 'LOGIN_WECHAT_APP_SECRET') -Because 'AppSecretEnv 写的是容器环境变量名,不是密钥'

    $openId = ConvertFrom-YamlToFlatMap -Text (New-LoginAuthProvidersYaml -WeChatAppId $LoginWeChatTestAppId -WeChatAllowOpenId $true `
            -QQAppId $LoginQQTestAppId -QQAllowOpenId $true -ReleaseProfile 'staging' -WeChatAppSecretEnv 'LOGIN_WECHAT_APP_SECRET')
    Assert-Equal -Expected 'false' -Actual $openId.Scalars['AuthProviders.WeChat.RequireUnionId'] -Because '-LoginWeChatAllowOpenId 写 RequireUnionId: false'
    Assert-Equal -Expected 'false' -Actual $openId.Scalars['AuthProviders.QQ.RequireUnionId'] -Because '-LoginQQAllowOpenId 写 RequireUnionId: false'

    $qqOnlyText = New-LoginAuthProvidersYaml -QQAppId $LoginQQTestAppId -ReleaseProfile 'prod' -WeChatAppSecretEnv 'LOGIN_WECHAT_APP_SECRET'
    $qqOnly = ConvertFrom-YamlToFlatMap -Text $qqOnlyText
    Assert-Equal -Expected 'AuthProviders,AuthProviders.QQ,AuthProviders.QQ.AppId' -Actual ((Get-FlatSubtreeKeys -Flat $qqOnly -Path 'AuthProviders') -join ',') `
        -Because 'QQ 只写 AppId:provider 只用 access_token 调 /oauth2.0/me,不需要 AppKey,也不写 RequestUnionId(缺省 true)'
    # 只看非注释行:片段开头的说明注释会提到微信 AppSecret,那不是键。
    $qqOnlyKeyLines = (@($qqOnlyText -split "`n" | Where-Object { $_ -notmatch '^\s*#' }) -join "`n")
    Assert-NotMatch -Text $qqOnlyKeyLines -Pattern 'AppSecret|AppKey|WeChat' -Because '只配 QQ 时不出现任何密钥键,也不出现微信段'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($qqOnlyText, '(?m)^AuthProviders:[ ]*$').Count) -Because '片段里顶层 AuthProviders: 恰好一个'
}

Test-Case '纯函数 Get-LoginAuthProviderParamErrors:非法 AppId、staging / prod 的 http Endpoint、带查询串 / 用户信息 / 片段 / ${ / 换行的 Endpoint、有 Endpoint 无 AppId 都报错并点名参数;合法组合放行;New-LoginAuthProvidersYaml 对同样输入 throw' {
    $rejects = @(
        @{ Why = 'AppId 含空格'; Args = @{ WeChatAppId = 'wx 0123'; ReleaseProfile = 'dev' }; Pattern = '-LoginWeChatAppId 只允许 \[A-Za-z0-9_\.-\]' }
        @{ Why = 'AppId 含双引号'; Args = @{ WeChatAppId = 'wx"0123'; ReleaseProfile = 'dev' }; Pattern = '-LoginWeChatAppId 只允许' }
        @{ Why = 'AppId 结尾带换行($ 会放过,必须 \z)'; Args = @{ WeChatAppId = "wx0123`n"; ReleaseProfile = 'dev' }; Pattern = '-LoginWeChatAppId 只允许' }
        @{ Why = 'QQ AppId 含 ${'; Args = @{ QQAppId = '1012${QQ_APP_ID}'; ReleaseProfile = 'dev' }; Pattern = '-LoginQQAppId 只允许' }
        @{ Why = 'staging 档微信 http Endpoint'; Args = @{ WeChatAppId = 'wx0123'; WeChatEndpoint = 'http://mock.contract:18090'; ReleaseProfile = 'staging' }; Pattern = '-LoginWeChatEndpoint 在 -ReleaseProfile staging 只接受 https://' }
        @{ Why = 'prod 档 QQ http Endpoint'; Args = @{ QQAppId = '101234567'; QQEndpoint = 'http://mock.contract:18091'; ReleaseProfile = 'prod' }; Pattern = '-LoginQQEndpoint 在 -ReleaseProfile prod 只接受 https://' }
        @{ Why = 'Endpoint 带查询串'; Args = @{ WeChatAppId = 'wx0123'; WeChatEndpoint = 'https://mock.contract/sns?appid=1'; ReleaseProfile = 'dev' }; Pattern = '-LoginWeChatEndpoint 须是 https://主机' }
        @{ Why = 'Endpoint 带用户信息'; Args = @{ WeChatAppId = 'wx0123'; WeChatEndpoint = 'https://ops:hunter2pw@mock.contract'; ReleaseProfile = 'dev' }; Pattern = '-LoginWeChatEndpoint 须是 https://主机'; NoEcho = 'hunter2pw' }
        @{ Why = 'Endpoint 带片段'; Args = @{ WeChatAppId = 'wx0123'; WeChatEndpoint = 'https://mock.contract/path#frag'; ReleaseProfile = 'dev' }; Pattern = '-LoginWeChatEndpoint 须是 https://主机' }
        @{ Why = 'Endpoint 端口越界'; Args = @{ WeChatAppId = 'wx0123'; WeChatEndpoint = 'https://mock.contract:70000'; ReleaseProfile = 'dev' }; Pattern = '-LoginWeChatEndpoint 须是 https://主机' }
        @{ Why = 'Endpoint scheme 大写'; Args = @{ WeChatAppId = 'wx0123'; WeChatEndpoint = 'HTTPS://mock.contract'; ReleaseProfile = 'dev' }; Pattern = '-LoginWeChatEndpoint 须是 https://主机' }
        @{ Why = 'Endpoint 结尾带换行'; Args = @{ WeChatAppId = 'wx0123'; WeChatEndpoint = "https://mock.contract`n"; ReleaseProfile = 'dev' }; Pattern = '-LoginWeChatEndpoint 须是 https://主机' }
        @{ Why = 'Endpoint 含 ${'; Args = @{ WeChatAppId = 'wx0123'; WeChatEndpoint = 'https://${WECHAT_MOCK_HOST}'; ReleaseProfile = 'dev' }; Pattern = '-LoginWeChatEndpoint 含未展开的 \$\{\.\.\.\}' }
        @{ Why = '有微信 Endpoint 无 AppId'; Args = @{ WeChatEndpoint = 'https://mock.contract'; ReleaseProfile = 'dev' }; Pattern = '给了 -LoginWeChatEndpoint 却没给 -LoginWeChatAppId' }
        @{ Why = '有 QQ Endpoint 无 AppId'; Args = @{ QQEndpoint = 'https://mock.contract'; ReleaseProfile = 'prod' }; Pattern = '给了 -LoginQQEndpoint 却没给 -LoginQQAppId' }
    )
    foreach ($case in $rejects) {
        $fnArgs = $case.Args
        $errors = @(Get-LoginAuthProviderParamErrors @fnArgs)
        Assert-True -Condition ($errors.Count -ge 1) -Because "$($case.Why):必须报错(fail-closed),不能放行"
        Assert-Match -Text ($errors -join "`n") -Pattern $case.Pattern -Because "$($case.Why):错误文本必须点名参数与原因"
        $message = ''
        try { New-LoginAuthProvidersYaml @fnArgs -WeChatAppSecretEnv 'LOGIN_WECHAT_APP_SECRET' | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern '^生成 login ConfigMap 的 AuthProviders 段失败:' -Because "$($case.Why):生成器用同一份规则纵深防御,必须 throw"
        if ($case.NoEcho) {
            Assert-NotMatch -Text (($errors -join "`n") + "`n" + $message) -Pattern ([regex]::Escape($case.NoEcho)) -Because "$($case.Why):报错不回显 Endpoint 的值(用户信息里可能有口令)"
        }
    }

    $passes = @(
        @{ Why = '全空(不配第三方登录)'; Args = @{ ReleaseProfile = 'prod' } }
        @{ Why = 'dev 档 http 沙箱'; Args = @{ WeChatAppId = 'wx0123'; WeChatEndpoint = 'http://127.0.0.1:18090'; QQAppId = '101234567'; QQEndpoint = 'http://login-oauth-mock.contract:18091'; ReleaseProfile = 'dev' } }
        @{ Why = 'staging 档 https 带端口与路径'; Args = @{ WeChatAppId = 'wx_App.01-a'; WeChatEndpoint = 'https://oauth-mock.example.com:8443/sandbox/v1'; ReleaseProfile = 'staging' } }
        @{ Why = 'prod 档只配 AppId'; Args = @{ WeChatAppId = 'wx0123'; QQAppId = '101234567'; ReleaseProfile = 'prod' } }
    )
    foreach ($case in $passes) {
        $fnArgs = $case.Args
        $errors = @(Get-LoginAuthProviderParamErrors @fnArgs)
        Assert-Equal -Expected 0 -Actual $errors.Count -Because "$($case.Why):合法组合必须放行(实际报错:$($errors -join ' | '))"
    }
}

Test-Case '纯函数 Resolve-LoginWeChatAppSecret:任何档位缺失即 throw(dev 也没有回落值);staging 短于 16 位、prod 占位串、首尾空白、${ 都拒绝且不回显值;合法值原样返回' {
    $envName = 'MMORPG_LOGIN_WECHAT_APP_SECRET'
    $saved = [System.Environment]::GetEnvironmentVariable($envName)
    try {
        $cases = @(
            @{ Why = 'dev 未设置'; Profile = 'dev'; Value = ''; Pattern = '-LoginWeChatAppId 需要环境变量 MMORPG_LOGIN_WECHAT_APP_SECRET' }
            @{ Why = 'staging 未设置'; Profile = 'staging'; Value = ''; Pattern = 'ReleaseProfile=staging 要求从环境变量 MMORPG_LOGIN_WECHAT_APP_SECRET' }
            @{ Why = 'staging 短于 16 位'; Profile = 'staging'; Value = 'wechat-short-01'; Pattern = 'MMORPG_LOGIN_WECHAT_APP_SECRET.*长度 15 < 要求的 16'; NoEcho = $true }
            @{ Why = 'prod 占位串'; Profile = 'prod'; Value = 'change-me-in-production'; Pattern = 'MMORPG_LOGIN_WECHAT_APP_SECRET.*仍是占位串' }
            @{ Why = 'dev 首尾空白'; Profile = 'dev'; Value = "$LoginWeChatTestSecret "; Pattern = 'MMORPG_LOGIN_WECHAT_APP_SECRET 的值含首尾空白'; NoEcho = $true }
            @{ Why = 'prod 首尾空白'; Profile = 'prod'; Value = " $LoginWeChatTestSecret"; Pattern = 'MMORPG_LOGIN_WECHAT_APP_SECRET 的值含首尾空白'; NoEcho = $true }
            @{ Why = 'dev 含 ${'; Profile = 'dev'; Value = 'contract-${WECHAT_APP_SECRET}-0123'; Pattern = 'MMORPG_LOGIN_WECHAT_APP_SECRET 的值含未展开的' }
        )
        foreach ($case in $cases) {
            [System.Environment]::SetEnvironmentVariable($envName, $case.Value)
            $message = ''
            try { Resolve-LoginWeChatAppSecret -ReleaseProfile $case.Profile | Out-Null } catch { $message = $_.Exception.Message }
            Assert-Match -Text $message -Pattern $case.Pattern -Because "$($case.Why):必须在写操作之前 throw 并点名环境变量"
            if ($case.NoEcho) {
                Assert-NotMatch -Text $message -Pattern ([regex]::Escape($case.Value.Trim())) -Because "$($case.Why):报错不回显密钥"
            }
        }
        foreach ($profileName in @('dev', 'staging', 'prod')) {
            [System.Environment]::SetEnvironmentVariable($envName, $LoginWeChatTestSecret)
            $got = Resolve-LoginWeChatAppSecret -ReleaseProfile $profileName
            Assert-True -Condition ($got -ceq $LoginWeChatTestSecret) -Because "$profileName 档合法密钥必须原样返回(不 trim、不改写)"
        }
    }
    finally {
        [System.Environment]::SetEnvironmentVariable($envName, $saved)
    }
}

Test-Case '端到端 dev 档配微信 + QQ:ConfigMap 写 AuthProviders(微信 AppId / AppSecretEnv / RequireUnionId false / 沙箱 Endpoint,QQ 只写 AppId),AppSecret 原文不进输出,经 Secret login-oauth 的 secretKeyRef 注入,Pod 模板带它的指纹' {
    $run = $loginAuthDevRun
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "dev 档配微信 + QQ 并给了密钥必须成功。输出: $($run.Output)"
    $out = $run.Output

    $cmBlock = Select-ManifestByName -Output $out -Name 'go-svc-login-config'
    $flat = ConvertTo-FlatManifest -Block $cmBlock
    $expected = [ordered]@{
        'data.login.yaml.AuthProviders.WeChat.AppId'          = $LoginWeChatTestAppId
        'data.login.yaml.AuthProviders.WeChat.AppSecretEnv'   = 'LOGIN_WECHAT_APP_SECRET'
        'data.login.yaml.AuthProviders.WeChat.RequireUnionId' = 'false'
        'data.login.yaml.AuthProviders.WeChat.Endpoint'       = $LoginWeChatSandbox
        'data.login.yaml.AuthProviders.QQ.AppId'              = $LoginQQTestAppId
    }
    foreach ($key in $expected.Keys) {
        $actual = Get-FlatValue -Flat $flat -KeyPath $key
        Assert-True -Condition ($actual -ceq $expected[$key]) -Because "$key 期望 '$($expected[$key])',实际 '$actual'"
    }
    $leafKeys = @(Get-FlatSubtreeKeys -Flat $flat -Path 'data.login.yaml.AuthProviders' | Where-Object { $flat.Scalars[$_] -ne '' })
    Assert-Equal -Expected ($expected.Keys -join ',') -Actual ($leafKeys -join ',') -Because 'AuthProviders 下只有这五个值:不写 AppSecret / AppKey(密钥不进 ConfigMap),QQ 不写 RequireUnionId(没给 -LoginQQAllowOpenId)'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($cmBlock, '(?m)^ {4}AuthProviders:[ ]*\r?$').Count) -Because 'login.yaml 里顶层 AuthProviders: 只能有一个(重复的顶层键各解析器行为不一)'
    # http Endpoint 只在 login 的 Mode=dev / test 放行:同一份 ConfigMap 必须写着 Mode: dev。
    Assert-Equal -Expected 'dev' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.Mode') -Because 'dev 档 http 沙箱 Endpoint 依赖 login 的 Mode=dev'
    Assert-NotMatch -Text $out -Pattern ([regex]::Escape($LoginWeChatTestSecret)) -Because 'AppSecret 只经 Secret 进容器:ConfigMap、Secret 意图行、指纹注解、日志里都不得有原文'

    Assert-Match -Text $out -Pattern '(?m)^\[dry-run\] kubectl apply -n \S+ -f -\s+# Secret login-oauth \(key wechat-app-secret; value hidden\)\s*$' -Because 'Secret 的 dry-run 行要点名 Secret 名与 key,且声明值已隐去'
    Assert-NotMatch -Text $out -Pattern 'kubectl delete secret login-oauth' -Because '开着微信登录时不删自己要用的 Secret'

    $deployment = Select-LoginDeploymentBlock -Output $out -Label 'dev 微信 + QQ'
    Assert-Match -Text $deployment -Pattern '- name: LOGIN_WECHAT_APP_SECRET\s+valueFrom:\s+secretKeyRef:\s+name: login-oauth\s+key: wechat-app-secret[ ]*\n(?![ ]+optional:)' -Because 'LOGIN_WECHAT_APP_SECRET 必须经 secretKeyRef 取 Secret login-oauth 的 wechat-app-secret,且不得 optional'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($deployment, 'name: LOGIN_WECHAT_APP_SECRET').Count) -Because 'LOGIN_WECHAT_APP_SECRET 只能注入一份'
    Assert-NotMatch -Text $deployment -Pattern 'name: LOGIN_WECHAT_APP_SECRET\s+value:' -Because 'AppSecret 不得以明文 value 写进 Deployment'
    # dev 档排队关:微信指纹自己排在 annotations: 的下一行。值必须是 login-oauth 这把密钥的指纹,只轮换密钥时模板才会变、login 才会滚动。
    $hash = [regex]::Match($deployment, '(?m)^[ ]+template:[ ]*\n[ ]+metadata:[ ]*\n(?:[ ]*#[^\n]*\n)*[ ]+annotations:[ ]*\n[ ]+mmorpg\.io/login-oauth-secret-hash: "(?<hash>[0-9a-f]{12})"[ ]*$')
    Assert-True -Condition $hash.Success -Because '密钥指纹注解必须在 Pod 模板 metadata.annotations 下,值为加双引号的 12 位小写十六进制'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($deployment, 'mmorpg\.io/login-oauth-secret-hash').Count) -Because '密钥指纹注解只能有一处'
    Assert-Equal -Expected (Get-InjectedSecretFingerprint -SecretName 'login-oauth' -Value $LoginWeChatTestSecret) -Actual $hash.Groups['hash'].Value -Because '指纹输入只有 login-oauth 这把密钥(带 Secret 名做域分隔):换密钥指纹必变,同一密钥重跑不变'

    # 写序:Secret 先于引用它的 Deployment,且同 namespace(secretKeyRef 只能引用同 namespace 的 Secret)。
    $secretLine = [regex]::Match($out, '(?m)^\[dry-run\] kubectl apply -n (?<ns>\S+) -f -\s+# Secret login-oauth \(')
    $envLine = [regex]::Match($out, '(?m)^[ ]+- name: LOGIN_WECHAT_APP_SECRET\s*$')
    Assert-True -Condition ($secretLine.Success -and $envLine.Success -and $secretLine.Index -lt $envLine.Index) -Because 'Secret login-oauth 必须先于引用它的 login Deployment 落地,反过来新 Pod 会卡在 CreateContainerConfigError'
    $deployApply = @([regex]::Matches($out, '(?m)^\[dry-run\] kubectl [^\r\n]*apply -n (?<ns>\S+) -f -\s*$') | Where-Object { $_.Index -lt $envLine.Index }) | Select-Object -Last 1
    Assert-True -Condition ($null -ne $deployApply) -Because 'login Deployment 之前应当有它自己的 dry-run apply 行'
    Assert-Equal -Expected $deployApply.Groups['ns'].Value -Actual $secretLine.Groups['ns'].Value -Because 'Secret 与 login Deployment 必须同 namespace'

    Assert-Match -Text $out -Pattern 'login_wechat=True login_qq=True' -Because '写路径入口的摘要行要写明两个 provider 都开了(只展示是否启用,不展示密钥)'
    Assert-NotMatch -Text $out -Pattern 'LoginWeChatAllowOpenId \(ReleaseProfile=' -Because 'dev 档给 -LoginWeChatAllowOpenId 不打 openid 分裂警告(只对非 dev 档)'
}

Test-Case '端到端 dev 档只配 QQ:只写 AuthProviders.QQ.AppId,不建 Secret、不注入 env、不带指纹注解;微信密钥变量即使有值也不被读取,残留的 login-oauth 照删' {
    $run = $loginAuthQQOnlyRun
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "只配 QQ 必须成功(QQ 不需要任何密钥)。输出: $($run.Output)"
    $out = $run.Output
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $out -Name 'go-svc-login-config')
    Assert-Equal -Expected 'data.login.yaml.AuthProviders,data.login.yaml.AuthProviders.QQ,data.login.yaml.AuthProviders.QQ.AppId' `
        -Actual ((Get-FlatSubtreeKeys -Flat $flat -Path 'data.login.yaml.AuthProviders') -join ',') -Because '只配 QQ:AuthProviders 下只有 QQ.AppId,没有微信段、没有任何密钥键'
    Assert-True -Condition ((Get-FlatValue -Flat $flat -KeyPath 'data.login.yaml.AuthProviders.QQ.AppId') -ceq $LoginQQTestAppId) -Because 'QQ AppId 原样写出'
    Assert-NotMatch -Text $out -Pattern '# Secret login-oauth |LOGIN_WECHAT_APP_SECRET|login-oauth-secret-hash' -Because 'QQ 不建 Secret、不注入 env、不带指纹注解'
    Assert-NotMatch -Text $out -Pattern ([regex]::Escape($LoginWeChatTestSecret)) -Because '不配微信时不读微信密钥变量,原文更不能出现'
    Assert-Match -Text $out -Pattern '(?m)^\[dry-run\] kubectl delete secret login-oauth -n \S+ --ignore-not-found\s*$' -Because '没给 -LoginWeChatAppId:删掉上次留下的 login-oauth Secret'
    Assert-Match -Text $out -Pattern 'login_wechat=False login_qq=True' -Because '摘要行只展示 QQ 开了'
}

Test-Case '端到端负向:dev 档给了 -LoginWeChatAppId 却没设 MMORPG_LOGIN_WECHAT_APP_SECRET,在任何 kubectl 写操作之前拒绝(没有回落值)' {
    $run = $loginAuthNoSecretRun
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "缺微信 AppSecret 必须阻断:login 对缺失的 AppSecretEnv 拒启。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern '-LoginWeChatAppId 需要环境变量 MMORPG_LOGIN_WECHAT_APP_SECRET' -Because '错误必须点名缺哪个环境变量'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '密钥解析(Initialize-InjectedSecrets)先于任何写操作,不留半截部署'
}

Test-Case '端到端负向:staging 档 -LoginWeChatEndpoint 给 http://,在密钥解析与任何 kubectl 写操作之前拒绝,报的是参数原因' {
    $run = $loginAuthStagingHttpRun
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "staging 档 http Endpoint 必须拒绝:login 生产模式只认 https,部署出来只会拒启。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'login 第三方登录参数校验失败' -Because '拒绝来自写路径入口的参数门禁(Assert-LoginAuthProviderParams)'
    Assert-Match -Text $run.Output -Pattern '-LoginWeChatEndpoint 在 -ReleaseProfile staging 只接受 https://' -Because '错误必须点名参数、档位与原因'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '参数门禁先于任何写操作'
    Assert-NotMatch -Text $run.Output -Pattern 'Client entry: mode=' -Because '参数门禁排在入口摘要行之前,拒绝后不再往下走'
}

Test-Case '第三方登录源码形状:写路径入口经 Assert-ClientEntryDeployPreflight 调一次参数门禁,密钥解析只在 Initialize-InjectedSecrets、只在部署 login 且给了 -LoginWeChatAppId 时' {
    $preflightFn = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Assert-ClientEntryDeployPreflight' }, $false)
    Assert-True -Condition ($null -ne $preflightFn) -Because '要有 Assert-ClientEntryDeployPreflight'
    $gateCalls = @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] -and $a.GetCommandName() -eq 'Assert-LoginAuthProviderParams' }, $true))
    Assert-Equal -Expected 1 -Actual $gateCalls.Count -Because '整个脚本只调一次 Assert-LoginAuthProviderParams'
    Assert-True -Condition ($gateCalls[0].Extent.StartOffset -gt $preflightFn.Extent.StartOffset -and $gateCalls[0].Extent.EndOffset -lt $preflightFn.Extent.EndOffset) -Because '参数门禁在 Assert-ClientEntryDeployPreflight 里(写路径入口、密钥解析之前,与 -LoginDevPasswordAuth 同处)'

    $resolveCalls = @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] -and $a.GetCommandName() -eq 'Resolve-LoginWeChatAppSecret' }, $true))
    Assert-Equal -Expected 1 -Actual $resolveCalls.Count -Because '微信 AppSecret 只在一处解析'
    $initFn = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Initialize-InjectedSecrets' }, $false)
    Assert-True -Condition ($resolveCalls[0].Extent.StartOffset -gt $initFn.Extent.StartOffset -and $resolveCalls[0].Extent.EndOffset -lt $initFn.Extent.EndOffset) -Because '解析在 Initialize-InjectedSecrets 里(写路径入口懒解析,*-down / *-status 不被它挡住)'
    $guard = $resolveCalls[0].Parent
    while ($null -ne $guard -and $guard -isnot [System.Management.Automation.Language.IfStatementAst]) { $guard = $guard.Parent }
    Assert-True -Condition ($null -ne $guard) -Because '解析必须包在 if 里'
    $condition = $guard.Clauses[0].Item1.Extent.Text
    Assert-True -Condition ($condition.Contains('$LoginWeChatAppId') -and $condition.Contains('Test-ZoneLoginDeployed')) -Because "只在部署 login 且给了 -LoginWeChatAppId 时解析(实际条件:$condition)"
}
# ─────────────────────────────────────────────────────────────────
# 路由服预算链(docs/design/client-request-ordering.md §5.2 不变式 I1)
# ─────────────────────────────────────────────────────────────────
# 路由目标 Go 服务的 zrpc Timeout + 1000 ≤ 路由服 ForwardTimeoutMs。判定只有一份:k8s_deploy.ps1 的
# Get-RouterForwardBudgetViolations(纯函数)+ Assert-RouterForwardBudget(读文件、写路径入口调用)。
# 照上面 deadline 预算的做法按 AST 抽出来直接调用,不另抄一份判定,也不执行脚本入口。
$routerBudgetFunctionNames = @('Get-RouterForwardBudgetViolations', 'Assert-RouterForwardBudget')
foreach ($routerBudgetFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($routerBudgetFunctionNames -contains $routerBudgetFnAst.Name) {
        Set-Item -Path "Function:script:$($routerBudgetFnAst.Name)" -Value $routerBudgetFnAst.Body.GetScriptBlock()
    }
}
foreach ($routerBudgetFnName in $routerBudgetFunctionNames) {
    if (-not (Test-Path "Function:$routerBudgetFnName")) { throw "k8s_deploy.ps1 缺少函数 $routerBudgetFnName(路由服预算守卫被删了?后续断言无意义)" }
}

Test-Case "路由服预算链:仓库配置满足 目标 Go 服务 zrpc Timeout + 1000 ≤ 路由服 ForwardTimeoutMs(部署门禁对当前配置放行)" {
    # 不满足时 Assert 抛出的文本就是失败原因,逐项点名是哪个目标、差多少。
    $RepoRoot = Get-RepoRoot
    Assert-RouterForwardBudget | Out-Null
}

Test-Case "zone-up 写路径入口先过路由服预算门禁,之后才有任何 kubectl 写操作" {
    $okAt = $devOut.IndexOf('Router forward budget OK')
    $firstKubectlAt = $devOut.IndexOf('[dry-run] kubectl')
    Assert-True -Condition ($okAt -ge 0) -Because 'zone-up DryRun 必须经过 Assert-RouterForwardBudget(写路径入口门禁),否则守卫等于没挂'
    Assert-True -Condition ($firstKubectlAt -lt 0 -or $okAt -lt $firstKubectlAt) -Because '预算核对必须先于任何 kubectl 写操作,不留半截部署'
}

Test-Case "自检:路由服预算判定对 修复前 login 的 10000 / 差 1ms / Timeout 0 / 非数字 / MethodTimeouts / ForwardTimeoutMs 缺席或 0 报违例,对等号边界与未写 Timeout 通过(守卫不得静默放行)" {
    $check = {
        param([string]$ForwardRaw, [string]$ServiceText)
        $targets = [ordered]@{
            LoginNodeService = @{ Source = 'fixture/login.yaml'; Scalars = (ConvertFrom-YamlToFlatMap -Text $ServiceText).Scalars }
        }
        return @(Get-RouterForwardBudgetViolations -ForwardTimeoutMsRaw $ForwardRaw -Targets $targets)
    }

    $passes = @(
        @{ Why = 'ForwardTimeoutMs 5000 + Timeout 4000(等号边界)'; Forward = '5000'; Service = "Timeout: 4000" }
        @{ Why = 'ForwardTimeoutMs 5000 + 未写 Timeout(go-zero 默认 2000)'; Forward = '5000'; Service = "Name: login.rpc" }
    )
    foreach ($c in $passes) {
        $got = @(& $check $c.Forward $c.Service)
        Assert-True -Condition ($got.Count -eq 0) -Because ("{0} 应通过,实际报:{1}" -f $c.Why, ($got -join ';'))
    }

    $targetViolations = @(
        @{ Why = '修复前的 login 值 10000(路由服 5000 先回超时,handler 继续提交)'; Forward = '5000'; Service = "Timeout: 10000" }
        @{ Why = '修复前的 match 值 5000(与 ForwardTimeoutMs 相等,没有余量)'; Forward = '5000'; Service = "Timeout: 5000" }
        @{ Why = '差 1ms(余量是 1000)'; Forward = '5000'; Service = "Timeout: 4001" }
        @{ Why = '服务端 Timeout 0(go-zero 不装超时拦截器)'; Forward = '5000'; Service = "Timeout: 0" }
        @{ Why = '服务端 Timeout 非数字'; Forward = '5000'; Service = "Timeout: 4s" }
        @{ Why = '出现 MethodTimeouts(不逐方法核对,宁拒不放)'; Forward = '5000'; Service = "Timeout: 3000`nMethodTimeouts:`n  - FullMethod: /loginpb.ClientPlayerLogin/CreatePlayer`n    Timeout: 9s" }
    )
    foreach ($c in $targetViolations) {
        $got = @(& $check $c.Forward $c.Service)
        Assert-True -Condition ($got.Count -gt 0) -Because "$($c.Why) 必须报违例"
        Assert-Match -Text $got[0] -Pattern '^LoginNodeService:' -Because "$($c.Why) 的违例必须点名是哪个目标"
    }

    $forwardViolations = @(
        @{ Why = 'ForwardTimeoutMs 缺席'; Forward = '' }
        @{ Why = 'ForwardTimeoutMs 0'; Forward = '0' }
        @{ Why = 'ForwardTimeoutMs 非数字'; Forward = '5s' }
    )
    foreach ($c in $forwardViolations) {
        $got = @(& $check $c.Forward "Timeout: 1000")
        Assert-True -Condition ($got.Count -gt 0) -Because "$($c.Why) 必须报违例"
        Assert-Match -Text $got[0] -Pattern 'ForwardTimeoutMs' -Because "$($c.Why) 的违例必须点名 ForwardTimeoutMs"
    }
}

$friendSqlAst = $deadlineDeployAst.Find({ param($a)
    $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Get-FriendSqlSettings'
}, $false)
if ($null -eq $friendSqlAst) { throw '好友权威 SQL 配置门禁缺失' }
Set-Item -Path Function:script:Get-FriendSqlSettings -Value $friendSqlAst.Body.GetScriptBlock()

Test-Case '全服好友开发可用 MySQL，显式 TiDB 入口始终启用后端验证' {
    $local = Get-FriendSqlSettings -Profile 'dev' -TiDBHost '' -DevelopmentHost 'mysql.test:3306'
    Assert-Equal -Expected 'mysql.test:3306' -Actual $local.Host -Because '开发默认沿用独立测试 MySQL'
    Assert-Equal -Expected 'false' -Actual $local.RequireTiDB -Because '开发不得误称已使用 TiDB'
    foreach ($profile in @('dev', 'staging', 'prod')) {
        $settings = Get-FriendSqlSettings -Profile $profile -TiDBHost 'tidb.global.test:4000' -DevelopmentHost 'mysql.test:3306'
        Assert-Equal -Expected 'tidb.global.test:4000' -Actual $settings.Host -Because '所有副本的显式权威入口必须原样保留'
        Assert-Equal -Expected 'true' -Actual $settings.RequireTiDB -Because '指定 TiDB 必须同时启用实际后端身份验证'
    }
}

Test-Case '全服好友生产缺 TiDB 入口或入口非法时拒绝生成' {
    foreach ($profile in @('staging', 'prod')) {
        $rejected = $false
        try { $null = Get-FriendSqlSettings -Profile $profile -TiDBHost '' -DevelopmentHost 'mysql.test:3306' }
        catch { $rejected = $_.Exception.Message -match 'FriendTiDBHost' }
        Assert-True -Condition $rejected -Because '生产不得悄悄落回 MySQL'
    }
    foreach ($hostValue in @('tidb', 'tidb:0', 'tidb:65536', 'user:pass@tidb:4000', 'tidb:4000/db', "tidb:4000`nMode: dev")) {
        $rejected = $false
        try { $null = Get-FriendSqlSettings -Profile 'prod' -TiDBHost $hostValue -DevelopmentHost 'mysql.test:3306' }
        catch { $rejected = $_.Exception.Message -match 'FriendTiDBHost' }
        Assert-True -Condition $rejected -Because '不能把非法地址、凭据或额外配置写入 ConfigMap'
    }
}

# ─────────────────────────────────────────────────────────────────
# ③ db 消费组按 zone 拆(S2;docs/design/db-consumer-group-per-zone.md §2 / §4,
#    规格:docs/handoff/godb-pipeline-impl-20261015/S2-consumer-group-per-zone-addendum.md §2 / §3 / §12)
# ─────────────────────────────────────────────────────────────────
# go-svc-db-config 的组名键只有四种形状:final(G) 只写 GroupID;handoff(X←Y) 只写 ConsumerGroup + GroupHandoffFrom
# (不写 GroupID:交接闸之前的 go/db 二进制在 MustLoad 拒启,这就是能力证明)。G、X、Y ∈ {L, Z}:L = go/db/etc/db.yaml 的
# ServerConfig.Kafka.GroupID,Z = L + "_z" + ZoneId。复用 $devOut(基线 DryRun,缺省 keep),另起 5 个 DryRun 子进程:
# zone、legacy 两次预览,all-up / zone-down 两次入口拒绝,一次多 zone all-up(keep)。决策表、形状解析、读集群的两个函数与写路径入口函数按 AST 从
# k8s_deploy.ps1 抽出来直接调用(与上文 deadline 守卫同一做法);读集群的唯一出口 Invoke-ClientEntryKubectl 由本节的同名假函数
# 顶替(函数先于外部命令解析,被抽函数按动态作用域找到它),没有规则命中的调用一律 throw,不碰任何集群。
# DryRun 告警只断不含空格的片段(DbConsumerGroup=keep、dryrun-preview):Write-Warning 可能在空格处折行。
# 前置:deploy/k8s/manifests/go-svc/db.yaml 的 Pod 模板带 PLACEHOLDER_DB_CONSUMER_GROUP / PLACEHOLDER_DB_GROUP_HANDOFF_FROM 两个占位
# (缺占位时 Apply-OneGoSvc 对 db 拒绝部署,文件头的基线 DryRun 会先失败)。
. (Join-Path (Get-ToolsScriptsDir) 'lib' 'kafka_consumer_groups.ps1')
$DbGroupLegacy = Get-EtcValue -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.GroupID'
$DbGroupZone = "${DbGroupLegacy}_z101"
$DbGroupTopic = Get-DbTaskTopicName -ZoneId 101 -Generation ([int](Get-EtcValue -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.TopicGeneration'))
$DbGroupPartitions = [int](Get-EtcValue -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.PartitionCnt')
$DbGroupComment = '    # 消费组由 k8s_deploy.ps1 按 -DbConsumerGroup 与 Kafka 位点决定(docs/design/db-consumer-group-per-zone.md)'
$DbGroupDescribeHeader = 'GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG CONSUMER-ID HOST CLIENT-ID'
$DbGroupExecShell = "KAFKA_HEAP_OPTS='-Xms16m -Xmx64m' /opt/kafka/bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 --describe --group"

# ConvertFrom-LiveDbConsumerGroup 2026-10-18 起在 lib/kafka_consumer_groups.ps1(上面已 dot-source,回档第 0 步 f 共用):AST 里找不到它,
# 下面的存在性检查由 lib 的定义满足;留在名单里,是为了它被删掉或改名时这里照样变红。
$dbGroupFunctionNames = @(
    'Assert-DbConsumerGroupName', 'ConvertFrom-LiveDbConsumerGroup', 'Test-DbConsumerGroupNeedsKafkaFacts', 'Resolve-DbConsumerGroupPlan',
    'New-DbConsumerGroupYaml', 'Get-DbConsumerGroupPlan', 'Read-LiveDbConfigMapYaml', 'Read-DbGroupTopicFacts',
    # 显式交接渲染交接形状之前的线上 db 交接闸能力证明(R7 复核 [0])。
    'Assert-LiveDbPodsHaveHandoffGate',
    'Initialize-DbConsumerGroupPlans', 'Test-ZoneDbDeployed', 'Get-AuthoritativeScalar', 'Get-ZoneNamespace',
    # all-up 分支取 zone 列表用(与 switch ($Command) 里的 Apply-Zone 同一来源)。
    'Get-ZonesFromJson', 'Resolve-ZonesConfigPath'
)
foreach ($dbGroupFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($dbGroupFunctionNames -contains $dbGroupFnAst.Name) {
        Set-Item -Path "Function:script:$($dbGroupFnAst.Name)" -Value $dbGroupFnAst.Body.GetScriptBlock()
    }
}
foreach ($dbGroupFnName in $dbGroupFunctionNames) {
    if (-not (Test-Path "Function:$dbGroupFnName")) { throw "k8s_deploy.ps1 缺少函数 ${dbGroupFnName}(db 消费组的决策 / 读集群被删了?后续断言无意义)" }
}

# 读集群出口的假函数:按规则表(Pattern 对 KubectlArgs 以空格拼接的串)第一条命中回应 { ExitCode; Stdout; Stderr },
# 没命中即 throw —— 本节任何意料之外的集群读(例如 DryRun 下读了集群)都要变红。每次调用记下参数与集群选择参数。
$script:DbGroupKubectlRules = @()
$script:DbGroupKubectlCalls = [System.Collections.Generic.List[object]]::new()
function Invoke-ClientEntryKubectl {
    param(
        [Parameter(Mandatory = $true)][string[]]$KubectlArgs,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig,
        [switch]$AllowFailure,
        [switch]$CaptureOutput
    )
    $line = $KubectlArgs -join ' '
    $script:DbGroupKubectlCalls.Add([pscustomobject]@{
            Args = @($KubectlArgs); Line = $line; KubeContext = $KubeContext; KubeConfig = $KubeConfig
            AllowFailure = $AllowFailure.IsPresent; CaptureOutput = $CaptureOutput.IsPresent
        })
    foreach ($rule in $script:DbGroupKubectlRules) {
        if ($line -match $rule.Pattern) {
            return [pscustomobject]@{ ExitCode = [int]$rule.ExitCode; Stdout = [string]$rule.Stdout; Stderr = [string]$rule.Stderr }
        }
    }
    throw "假 kubectl:没有规则命中这次调用(本不该读集群?):kubectl $line"
}

# 被抽函数读的脚本级变量一次给全(同名参数在 k8s_deploy.ps1 里的含义),规则表与调用记录清空。
function Reset-DbGroupFixture {
    param([object[]]$Rules = @())
    $script:DbGroupKubectlRules = @($Rules)
    $script:DbGroupKubectlCalls.Clear()
    $script:InfraNamespace = 'contract-infra'
    $script:NamespacePrefix = 'mmorpg-zone'
    $script:KubeContext = 'contract-db-group-ctx'
    $script:KubeConfig = ''
    $script:RepoRoot = Get-RepoRoot
    $script:GoSvcCatalogue = @{ db = @{ ConfigMap = 'go-svc-db-config'; ConfigFile = 'db.yaml' } }
    $script:Command = 'zone-up'
    $script:ZoneName = 'contract-test'
    $script:ZoneId = 101
    $script:SkipGoSvc = $false
    $script:GoSvcRegistry = 'registry.invalid/test'
    $script:DryRun = $false
    $script:DbConsumerGroup = 'keep'
    $script:DbConsumerGroupPlans = @{}
    # all-up 用例才用到:-SkipInfra 决定是否探测 infra namespace,-ZonesConfigPath 给 zone 列表(用例里显式设置)。
    $script:SkipInfra = $false
    $script:ZonesConfigPath = ''
}

# 一次 kafka-0 里 describe 的回应。missing 走 stderr(证明 stdout / stderr 合并后才分类),exec-failed 是 kubectl exec 本身失败。
function New-DbGroupDescribeRule {
    param(
        [Parameter(Mandatory = $true)][string]$Group,
        [Parameter(Mandatory = $true)][ValidateSet('offsets', 'no-offsets', 'missing', 'exec-failed', 'error-line')][string]$State
    )
    $pattern = '^exec .* --group ' + [regex]::Escape($Group) + '$'
    switch ($State) {
        'offsets' { return @{ Pattern = $pattern; ExitCode = 0; Stdout = "`n$DbGroupDescribeHeader`n$Group $DbGroupTopic 0 42 50 8 - - -`n$Group $DbGroupTopic 1 7 7 0 - - -"; Stderr = '' } }
        'no-offsets' { return @{ Pattern = $pattern; ExitCode = 0; Stdout = "`n$DbGroupDescribeHeader`n$Group other_topic 0 5 5 0 - - -`n$Group $DbGroupTopic 0 - 50 - - - -"; Stderr = '' } }
        'missing' { return @{ Pattern = $pattern; ExitCode = 0; Stdout = ''; Stderr = "Error: Consumer group '$Group' does not exist." } }
        'exec-failed' { return @{ Pattern = $pattern; ExitCode = 1; Stdout = ''; Stderr = 'Error from server (NotFound): pods "kafka-0" not found' } }
        'error-line' { return @{ Pattern = $pattern; ExitCode = 0; Stdout = 'Error: Executing consumer group command failed due to org.apache.kafka.common.errors.TimeoutException: Timed out waiting for a node assignment.'; Stderr = '' } }
    }
}

# 线上 go-svc-db-config 的回应:-DbYaml 为空 = ConfigMap 不存在(--ignore-not-found 无输出);-ExitCode 非 0 = 读失败。
# -Namespace 缺省是 zone-up 夹具的 zone namespace;all-up 用例按 zone 各给一条。
function New-DbGroupConfigMapRule {
    param([AllowEmptyString()][string]$DbYaml = '', [int]$ExitCode = 0, [string]$RawStdout = $null, [string]$Namespace = 'mmorpg-zone-contract-test')
    $stdout = ''
    if ($PSBoundParameters.ContainsKey('RawStdout')) { $stdout = $RawStdout }
    elseif (-not [string]::IsNullOrEmpty($DbYaml)) {
        $stdout = [ordered]@{ apiVersion = 'v1'; kind = 'ConfigMap'; metadata = @{ name = 'go-svc-db-config' }; data = @{ 'db.yaml' = $DbYaml } } | ConvertTo-Json -Depth 5
    }
    $stderr = if ($ExitCode -ne 0) { 'Unable to connect to the server: dial tcp 10.0.0.1:6443: i/o timeout' } else { '' }
    return @{ Pattern = ('^get configmap go-svc-db-config -n ' + [regex]::Escape($Namespace) + ' --ignore-not-found -o json$'); ExitCode = $ExitCode; Stdout = $stdout; Stderr = $stderr }
}

# all-up(不带 -SkipInfra)对 infra namespace 的只读探测:-Exists = 已存在(输出 namespace/<名>);缺省 = 不存在(无输出);
# -ExitCode 非 0 = 探测失败。
function New-DbGroupInfraNamespaceRule {
    param([switch]$Exists, [int]$ExitCode = 0)
    $stdout = if ($Exists -and $ExitCode -eq 0) { 'namespace/contract-infra' } else { '' }
    $stderr = if ($ExitCode -ne 0) { 'Error from server (Forbidden): namespaces "contract-infra" is forbidden' } else { '' }
    return @{ Pattern = '^get namespace contract-infra --ignore-not-found -o name$'; ExitCode = $ExitCode; Stdout = $stdout; Stderr = $stderr }
}

# 本 zone 的 db Deployment 与 Pod(Assert-LiveDbPodsHaveHandoffGate 的第一次只读调用,R7 复核 [0])。
# -Pods 每项 @{ Name; Phase(缺省 Running); Deleting = $true 即带 deletionTimestamp };-NoDeployment = db Deployment 不存在;-ExitCode 非 0 = 读失败。
function New-DbGroupPodsRule {
    param([object[]]$Pods = @(), [int]$Replicas = 2, [switch]$NoDeployment, [int]$ExitCode = 0, [string]$Namespace = 'mmorpg-zone-contract-test')
    $items = [System.Collections.Generic.List[object]]::new()
    if (-not $NoDeployment) {
        $items.Add([ordered]@{ apiVersion = 'apps/v1'; kind = 'Deployment'; metadata = [ordered]@{ name = 'db'; labels = @{ app = 'db' } }; spec = [ordered]@{ replicas = $Replicas } })
    }
    foreach ($pod in $Pods) {
        $metadata = [ordered]@{ name = $pod.Name; labels = @{ app = 'db' } }
        if ($pod.Deleting) { $metadata.deletionTimestamp = '2026-10-17T03:00:00Z' }
        $phase = if ($pod.Phase) { $pod.Phase } else { 'Running' }
        $items.Add([ordered]@{ apiVersion = 'v1'; kind = 'Pod'; metadata = $metadata; status = [ordered]@{ phase = $phase } })
    }
    $stdout = if ($ExitCode -eq 0) { [ordered]@{ apiVersion = 'v1'; kind = 'List'; items = @($items) } | ConvertTo-Json -Depth 8 } else { '' }
    $stderr = if ($ExitCode -ne 0) { 'Error from server (Forbidden): pods is forbidden: User "deployer" cannot list resource "pods"' } else { '' }
    return @{ Pattern = ('^get deployments,pods -l app=db -n ' + [regex]::Escape($Namespace) + ' -o json --request-timeout=15s$'); ExitCode = $ExitCode; Stdout = $stdout; Stderr = $stderr }
}

# 经 API server 代理抓一个 db Pod 的 /metrics:gate = 带交接闸的二进制(以 0 预建 db_consumer_group_handoff_gate_total);
# no-gate = 交接闸之前的二进制;forbidden = 部署账号缺 pods/proxy 权限。
function New-DbGroupMetricsRule {
    param(
        [Parameter(Mandatory = $true)][string]$Pod,
        [Parameter(Mandatory = $true)][ValidateSet('gate', 'no-gate', 'forbidden')][string]$State,
        [string]$Namespace = 'mmorpg-zone-contract-test'
    )
    $pattern = '^get --raw ' + [regex]::Escape("/api/v1/namespaces/$Namespace/pods/${Pod}:9160/proxy/metrics") + ' --request-timeout=15s$'
    switch ($State) {
        'gate' { return @{ Pattern = $pattern; ExitCode = 0; Stderr = ''; Stdout = "# HELP db_consumer_group_handoff_gate_total Group handoff gate verdicts.`n# TYPE db_consumer_group_handoff_gate_total counter`ndb_consumer_group_handoff_gate_total{outcome=`"no_peer_offsets`"} 0`ndb_consumer_group_handoff_gate_total{outcome=`"peer_released`"} 0" } }
        'no-gate' { return @{ Pattern = $pattern; ExitCode = 0; Stderr = ''; Stdout = "# TYPE db_task_result_total counter`ndb_task_result_total{result=`"ok`"} 12" } }
        'forbidden' { return @{ Pattern = $pattern; ExitCode = 1; Stdout = ''; Stderr = "Error from server (Forbidden): pods `"$Pod`" is forbidden: User `"deployer`" cannot get resource `"pods/proxy`"" } }
    }
}

# 一份 db.yaml 文本,ServerConfig.Kafka 下的组名键由调用方给(各行已带 4 格缩进)。
function New-DbGroupLiveYaml {
    param([Parameter(Mandatory = $true)][AllowEmptyCollection()][string[]]$KafkaLines)
    return (@('Name: db.rpc', 'ZoneId: 101', 'ServerConfig:', '  Kafka:', '    Brokers:', '      - "kafka.contract-infra:9092"') + $KafkaLines +
        @('    TopicGeneration: 1', '    PartitionCnt: 10', '  Database:', '    MaxOpenConn: 60')) -join "`n"
}

function Format-DbGroupPlan {
    param([Parameter(Mandatory = $true)]$Plan)
    return ('{0}:{1}<-{2}' -f $Plan.Shape, $Plan.Group, $Plan.HandoffFrom)
}

# DryRun 产物里 db 的 ConfigMap(原文 + 拍平)与 Deployment。
function Get-DbGroupRendering {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$Label)
    $block = Select-ManifestByName -Output $Output -Name 'go-svc-db-config'
    if ($null -eq $block) { throw "${Label}:DryRun 输出里没有 go-svc-db-config" }
    $deployment = @(Get-ManifestBlocks -Output $Output | Where-Object {
            $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch '(?m)^  name: db\s*$'
        }) | Select-Object -First 1
    if ($null -eq $deployment) { throw "${Label}:DryRun 输出里没有 db 的 Deployment" }
    return [pscustomobject]@{ Block = $block; Flat = (ConvertTo-FlatManifest -Block $block); Deployment = $deployment }
}

# db Pod 模板上的两条消费组注解:各恰好一处(按行首锚定,注释里提到注解名不算),值等于计划;不残留任何占位。
function Assert-DbGroupAnnotations {
    param(
        [Parameter(Mandatory = $true)]$Rendering,
        [Parameter(Mandatory = $true)][string]$Group,
        [Parameter(Mandatory = $true)][string]$HandoffFrom,
        [Parameter(Mandatory = $true)][string]$Label
    )
    foreach ($pair in @(@{ Key = 'mmorpg.io/db-consumer-group'; Value = $Group }, @{ Key = 'mmorpg.io/db-group-handoff-from'; Value = $HandoffFrom })) {
        $keyPattern = '(?m)^[ ]+' + [regex]::Escape($pair.Key) + ':'
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($Rendering.Deployment, $keyPattern).Count) -Because "${Label}:Pod 模板上 $($pair.Key) 必须恰好一处"
        Assert-Match -Text $Rendering.Deployment -Pattern ($keyPattern + ' "' + [regex]::Escape($pair.Value) + '"\s*$') -Because "${Label}:$($pair.Key) 必须等于本 zone 的消费组计划($($pair.Value)),组名或交接对方组一变 db 才会滚动"
    }
    Assert-NotMatch -Text $Rendering.Deployment -Pattern 'PLACEHOLDER_' -Because "${Label}:db Deployment 不得残留占位"
}

$DbGroupZoneRun = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SkipJavaSvc', '-DbConsumerGroup', 'Zone'))
$DbGroupLegacyRun = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SkipJavaSvc', '-DbConsumerGroup', 'legacy'))
$DbGroupAllUpRun = Invoke-DeployDryRun -Arguments @('-Command', 'all-up', '-DryRun', '-GoSvcRegistry', 'registry.invalid/test', '-DbConsumerGroup', 'zone')
$DbGroupZoneDownRun = Invoke-DeployDryRun -Arguments @('-Command', 'zone-down', '-ZoneName', 'contract-test', '-DryRun', '-DbConsumerGroup', 'legacy')
# 多 zone:zones.sample.json 的全部 zone(至少两个,zoneId 各不相同;下面的用例先核对这个前提)。-ZonesConfigPath 给绝对路径
# (子进程的工作目录不保证是仓库根);-SkipInfra 不装配 battle 全局池,-SkipJavaSvc:本次只看 db。
$DbGroupSampleZonesPath = Join-Path (Get-RepoRoot) 'deploy/k8s/zones.sample.json'
$DbGroupSampleZones = @((Get-Content -LiteralPath $DbGroupSampleZonesPath -Raw | ConvertFrom-Json).zones)
$DbGroupMultiZoneRun = Invoke-DeployDryRun -Arguments @('-Command', 'all-up', '-DryRun', '-SkipInfra', '-SkipJavaSvc', '-ZonesConfigPath', $DbGroupSampleZonesPath, '-GoSvcRegistry', 'registry.invalid/test')

Test-Case '③ 基线 DryRun(keep):db ConfigMap 是 final(Z),只有 GroupID=<db.yaml 的 GroupID>_z101,没有 ConsumerGroup / GroupHandoffFrom;注解 handoff-from 为 none' {
    $r = Get-DbGroupRendering -Output $devOut -Label 'keep DryRun'
    Assert-Equal -Expected 'db_rpc_consumer_group_z101' -Actual (Get-FlatValue -Flat $r.Flat -KeyPath 'data.db.yaml.ServerConfig.Kafka.GroupID') -Because 'DryRun 不读集群,keep 预览最终形状 final(Z)(修订稿 §2 DryRun 第一行)'
    Assert-Equal -Expected $DbGroupZone -Actual (Get-FlatValue -Flat $r.Flat -KeyPath 'data.db.yaml.ServerConfig.Kafka.GroupID') -Because '跨文件:按 zone 的组名 = go/db/etc/db.yaml 的 ServerConfig.Kafka.GroupID + _z<ZoneId>(与 go_services.ps1 / 回档 / merge_zone 同一推导)'
    foreach ($key in @('ConsumerGroup', 'GroupHandoffFrom')) {
        Assert-True -Condition (-not $r.Flat.Scalars.Contains("data.db.yaml.ServerConfig.Kafka.$key")) -Because "最终形状不写 $key(写了就成了交接形状,交接闸之前的二进制会拒启)"
    }
    # 注释行紧跟在 GroupID 上方:替换的是模板里原来那一行,组名键只此一处。中间只容许空行:CRLF 的 here-string 经 ConfigMap 包装
    # 与子进程输出捕获之后,行间可能多出空行(与本节无关的既有现象),不能因此误红。
    Assert-Match -Text $r.Block -Pattern ('(?m)^[ ]+' + [regex]::Escape($DbGroupComment.Trim()) + '[ \t\r]*\n(?:[ \t\r]*\n)*[ ]+GroupID: "' + [regex]::Escape($DbGroupZone) + '"[ \t\r]*$') -Because '组名键前面先一行说明注释(New-DbConsumerGroupYaml)'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($r.Block, '(?m)^[ ]+GroupID:').Count) -Because 'db ConfigMap 里 GroupID 只能出现一次'
    Assert-Match -Text $devOut -Pattern 'DbConsumerGroup=keep' -Because 'DryRun 加 keep 必须告警:这次没读集群,真跑时按线上 ConfigMap 与 Kafka 位点决定'
    Assert-Match -Text $devOut -Pattern 'dryrun-preview' -Because '告警与计划行都要点明 dryrun-preview'
    $planLine = "DB consumer group: zone=contract-test\(101\) mode=keep shape=final group=$([regex]::Escape($DbGroupZone)) handoff_from=none source=dryrun-preview topic=$([regex]::Escape($DbGroupTopic))"
    Assert-Match -Text $devOut -Pattern $planLine -Because '写路径入口每个 zone 打一行消费组计划'
    $planAt = $devOut.IndexOf('DB consumer group:')
    $firstKubectlAt = $devOut.IndexOf('[dry-run] kubectl')
    Assert-True -Condition ($planAt -ge 0 -and ($firstKubectlAt -lt 0 -or $planAt -lt $firstKubectlAt)) -Because '消费组计划在写路径入口算好,先于任何 kubectl 写操作(F2 / F4 的拒绝不留半截部署)'
    Assert-DbGroupAnnotations -Rendering $r -Group $DbGroupZone -HandoffFrom 'none' -Label 'keep DryRun'
}

Test-Case '③ 基线 DryRun:login ConfigMap 的 GroupID 仍是共享组名(死配置,值不随切组变化),上方带一行说明注释' {
    $login = Select-ManifestByName -Output $devOut -Name 'go-svc-login-config'
    Assert-Equal -Expected $DbGroupLegacy -Actual (Get-FlatValue -Flat (ConvertTo-FlatManifest -Block $login) -KeyPath 'data.login.yaml.Kafka.GroupID') -Because 'login 只生产不消费、代码不读,本轮只把它改成 optional,值不动;删除留给 Step 7'
    Assert-Match -Text $login -Pattern '(?m)^[ ]+#[^\n]*死配置[^\n]*\n(?:[ \t\r]*\n)*[ ]+GroupID: "db_rpc_consumer_group"[ \t\r]*$' -Because 'GroupID 上方要写明它是死配置、不是 db 的消费组(中间只容许空行)'
}

Test-Case '③ -DbConsumerGroup zone 的 DryRun(参数大小写不敏感):handoff(Z←L) —— ConsumerGroup=Z、GroupHandoffFrom=L、不写 GroupID;注解随之变' {
    Assert-Equal -Expected 0 -Actual $DbGroupZoneRun.ExitCode -Because "zone 模式的 DryRun 必须成功。输出: $($DbGroupZoneRun.Output)"
    $r = Get-DbGroupRendering -Output $DbGroupZoneRun.Output -Label 'zone DryRun'
    Assert-Equal -Expected $DbGroupZone -Actual (Get-FlatValue -Flat $r.Flat -KeyPath 'data.db.yaml.ServerConfig.Kafka.ConsumerGroup') -Because '交接形状的组名键是 ConsumerGroup'
    Assert-Equal -Expected $DbGroupLegacy -Actual (Get-FlatValue -Flat $r.Flat -KeyPath 'data.db.yaml.ServerConfig.Kafka.GroupHandoffFrom') -Because '正向交接的对方组是 legacy 组'
    Assert-True -Condition (-not $r.Flat.Scalars.Contains('data.db.yaml.ServerConfig.Kafka.GroupID')) -Because '交接形状不写 GroupID:交接闸之前的二进制因缺必填键拒启(能力证明)'
    Assert-Match -Text $DbGroupZoneRun.Output -Pattern 'DbConsumerGroup=zone' -Because 'DryRun 预览要告警'
    Assert-Match -Text $DbGroupZoneRun.Output -Pattern "mode=zone shape=handoff group=$([regex]::Escape($DbGroupZone)) handoff_from=$([regex]::Escape($DbGroupLegacy)) source=dryrun-preview" -Because '计划行(模式规范成小写)'
    Assert-DbGroupAnnotations -Rendering $r -Group $DbGroupZone -HandoffFrom $DbGroupLegacy -Label 'zone DryRun'
}

Test-Case '③ -DbConsumerGroup legacy 的 DryRun:handoff(L←Z) —— ConsumerGroup=L、GroupHandoffFrom=Z、不写 GroupID' {
    Assert-Equal -Expected 0 -Actual $DbGroupLegacyRun.ExitCode -Because "legacy 模式的 DryRun 必须成功。输出: $($DbGroupLegacyRun.Output)"
    $r = Get-DbGroupRendering -Output $DbGroupLegacyRun.Output -Label 'legacy DryRun'
    Assert-Equal -Expected $DbGroupLegacy -Actual (Get-FlatValue -Flat $r.Flat -KeyPath 'data.db.yaml.ServerConfig.Kafka.ConsumerGroup') -Because '反向交接回共享组'
    Assert-Equal -Expected $DbGroupZone -Actual (Get-FlatValue -Flat $r.Flat -KeyPath 'data.db.yaml.ServerConfig.Kafka.GroupHandoffFrom') -Because '反向交接的对方组是 zone 组'
    Assert-True -Condition (-not $r.Flat.Scalars.Contains('data.db.yaml.ServerConfig.Kafka.GroupID')) -Because '交接形状不写 GroupID'
    Assert-Match -Text $DbGroupLegacyRun.Output -Pattern 'DbConsumerGroup=legacy' -Because 'DryRun 预览要告警'
    Assert-DbGroupAnnotations -Rendering $r -Group $DbGroupLegacy -HandoffFrom $DbGroupZone -Label 'legacy DryRun'
}

Test-Case '③ 多 zone all-up DryRun(keep):每个 zone 一行计划、一份 go-svc-db-config,GroupID 是 L_z<本 zone 的 ZoneId>;db 注解按 zone 各自的组' {
    $sampleIds = @($DbGroupSampleZones | ForEach-Object { [int]$_.zoneId })
    Assert-True -Condition ($sampleIds.Count -ge 2 -and @($sampleIds | Sort-Object -Unique).Count -eq $sampleIds.Count) -Because "夹具前提:zones.sample.json 至少两个 zone 且 zoneId 各不相同(实际: $($sampleIds -join ','))"
    Assert-Equal -Expected 0 -Actual $DbGroupMultiZoneRun.ExitCode -Because "多 zone all-up DryRun 必须成功。输出: $($DbGroupMultiZoneRun.Output)"
    $out = $DbGroupMultiZoneRun.Output
    $blocks = @(Get-ManifestBlocks -Output $out | Where-Object { $_ -cmatch '(?m)^kind: ConfigMap\s*$' -and $_ -cmatch '(?m)^  name: go-svc-db-config\s*$' })
    Assert-Equal -Expected $sampleIds.Count -Actual $blocks.Count -Because '每个 zone 恰好一份 go-svc-db-config'
    $groupByZone = @{}
    foreach ($block in $blocks) {
        $flat = ConvertTo-FlatManifest -Block $block
        $groupByZone[[int](Get-FlatValue -Flat $flat -KeyPath 'data.db.yaml.ZoneId')] = Get-FlatValue -Flat $flat -KeyPath 'data.db.yaml.ServerConfig.Kafka.GroupID'
        Assert-True -Condition (-not $flat.Scalars.Contains('data.db.yaml.ServerConfig.Kafka.ConsumerGroup')) -Because 'keep 的 DryRun 预览是最终形状,不写 ConsumerGroup'
    }
    $generation = [int](Get-EtcValue -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.TopicGeneration')
    foreach ($zone in $DbGroupSampleZones) {
        $zoneId = [int]$zone.zoneId
        $want = "${DbGroupLegacy}_z$zoneId"
        Assert-Equal -Expected $want -Actual $groupByZone[$zoneId] -Because "zone $zoneId 的 ConfigMap(按 data.db.yaml.ZoneId 认 zone)GroupID 必须是本 zone 的组:计划键与 Apply-Zone 传的 zoneId 一致,不串 zone"
        $topic = Get-DbTaskTopicName -ZoneId $zoneId -Generation $generation
        Assert-Match -Text $out -Pattern ("DB consumer group: zone=$([regex]::Escape([string]$zone.name))\($zoneId\) mode=keep shape=final group=$([regex]::Escape($want)) handoff_from=none source=dryrun-preview topic=$([regex]::Escape($topic))") -Because "zone $zoneId 打一行计划,topic 按本 zone 推导"
    }
    $annotationGroups = @(Get-ManifestBlocks -Output $out | Where-Object { $_ -cmatch '(?m)^kind: Deployment\s*$' -and $_ -cmatch '(?m)^  name: db\s*$' } | ForEach-Object {
            $m = [regex]::Match($_, '(?m)^[ ]+mmorpg\.io/db-consumer-group: "(?<g>[^"]*)"')
            if ($m.Success) { $m.Groups['g'].Value } else { '<missing>' }
        } | Sort-Object)
    $expectedGroups = @($sampleIds | ForEach-Object { "${DbGroupLegacy}_z$_" } | Sort-Object)
    Assert-Equal -Expected ($expectedGroups -join ',') -Actual ($annotationGroups -join ',') -Because '每个 zone 的 db Deployment 注解 db-consumer-group 是本 zone 的组(与同 zone 的 ConfigMap 同一份计划)'
}

Test-Case '③ 负向:-DbConsumerGroup zone|legacy 只许配 zone-up —— all-up / zone-down 在入口拒绝,报错点名原因,不渲染任何清单' {
    foreach ($case in @(@{ Label = 'all-up + zone'; Run = $DbGroupAllUpRun; Mode = 'zone'; Command = 'all-up' }, @{ Label = 'zone-down + legacy'; Run = $DbGroupZoneDownRun; Mode = 'legacy'; Command = 'zone-down' })) {
        Assert-True -Condition ($case.Run.ExitCode -ne 0) -Because "$($case.Label) 必须被拒。输出: $($case.Run.Output)"
        Assert-Match -Text $case.Run.Output -Pattern '只允许与 -Command zone-up 一起用' -Because "$($case.Label):错误文本要写明只许 zone-up"
        Assert-Match -Text $case.Run.Output -Pattern ([regex]::Escape("-DbConsumerGroup $($case.Mode)")) -Because "$($case.Label):错误文本要点名本次的模式"
        Assert-NotMatch -Text $case.Run.Output -Pattern 'BEGIN MANIFEST|DB consumer group:' -Because "$($case.Label):入口拒绝早于任何渲染与集群操作"
    }
}

Test-Case '③ 写路径入口:-DbConsumerGroup 的入口拒绝早于 Ensure-KubectlAvailable;Initialize-DbConsumerGroupPlans 紧跟 Assert-ClientEntryDeployPreflight,先于 switch ($Command)' {
    $top = @($deadlineDeployAst.EndBlock.Statements)
    $rejectAt = -1
    $ensureAt = -1
    $switchAt = -1
    for ($i = 0; $i -lt $top.Count; $i++) {
        $s = $top[$i]
        if ($s -is [System.Management.Automation.Language.IfStatementAst] -and $s.Clauses[0].Item1.Extent.Text -match '^\$DbConsumerGroup\s+-ne\s+"keep"\s+-and\s+\$Command\s+-ne\s+"zone-up"$') { $rejectAt = $i }
        if ($s.Extent.Text.Trim() -ceq 'Ensure-KubectlAvailable' -and $ensureAt -lt 0) { $ensureAt = $i }
        if ($s -is [System.Management.Automation.Language.SwitchStatementAst] -and $s.Condition.Extent.Text -match '^\(?\s*\$Command\s*\)?$') { $switchAt = $i }
    }
    Assert-True -Condition ($rejectAt -ge 0) -Because '顶层要有 if ($DbConsumerGroup -ne "keep" -and $Command -ne "zone-up") 入口拒绝'
    Assert-True -Condition ($ensureAt -gt $rejectAt) -Because '入口拒绝必须早于 Ensure-KubectlAvailable 与任何 kubectl'
    $writeGates = @($top | Where-Object {
            $_ -is [System.Management.Automation.Language.IfStatementAst] -and
            $_.Clauses[0].Item1.Extent.Text -match '^\$Command\s+-in\s+@\(\s*"zone-up"\s*,\s*"all-up"\s*,\s*"infra-up"\s*\)$'
        })
    Assert-Equal -Expected 1 -Actual $writeGates.Count -Because '顶层恰好一个写路径门禁块'
    $statements = @($writeGates[0].Clauses[0].Item2.Statements)
    $assertAt = -1
    for ($i = 0; $i -lt $statements.Count; $i++) {
        if ($statements[$i].Extent.Text.Trim() -ceq 'Assert-ClientEntryDeployPreflight') { $assertAt = $i }
    }
    Assert-True -Condition ($assertAt -ge 0 -and $assertAt + 1 -lt $statements.Count) -Because '写路径门禁块里要有 Assert-ClientEntryDeployPreflight'
    Assert-Equal -Expected 'Initialize-DbConsumerGroupPlans' -Actual $statements[$assertAt + 1].Extent.Text.Trim() -Because '消费组计划紧跟集群外入口预检(修订稿 §3.4),早于密钥解析'
    Assert-True -Condition ($switchAt -ge 0 -and $writeGates[0].Extent.StartOffset -lt $top[$switchAt].Extent.StartOffset) -Because '写路径门禁块先于 switch ($Command)(Apply-Infra / Apply-Zone 的写操作都在它的分支里)'
}

Test-Case '③ 读集群只在写路径入口、只在非 DryRun:Read-LiveDbConfigMapYaml / Read-DbGroupTopicFacts / Assert-LiveDbPodsHaveHandoffGate 只由 Initialize-DbConsumerGroupPlans 调用,且包在 if (-not $DryRun) 里' {
    $initAst = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Initialize-DbConsumerGroupPlans' }, $false)
    Assert-True -Condition ($null -ne $initAst) -Because '要有 Initialize-DbConsumerGroupPlans'
    foreach ($readerName in @('Read-LiveDbConfigMapYaml', 'Read-DbGroupTopicFacts', 'Assert-LiveDbPodsHaveHandoffGate')) {
        $allCalls = @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] -and $a.GetCommandName() -eq $readerName }, $true))
        Assert-Equal -Expected 1 -Actual $allCalls.Count -Because "$readerName 全脚本只能有一处调用(Invoke-ClientEntryKubectl 不看 DryRun,判断集中在写路径入口)"
        $call = $allCalls[0]
        Assert-True -Condition ($call.Extent.StartOffset -gt $initAst.Extent.StartOffset -and $call.Extent.EndOffset -lt $initAst.Extent.EndOffset) -Because "$readerName 只能在 Initialize-DbConsumerGroupPlans 里调用"
        $guarded = $false
        $parent = $call.Parent
        while ($null -ne $parent -and -not [object]::ReferenceEquals($parent, $initAst)) {
            if ($parent -is [System.Management.Automation.Language.IfStatementAst]) {
                foreach ($clause in $parent.Clauses) {
                    if ($clause.Item1.Extent.Text -match '^-not\s+\$DryRun$' -and $call.Extent.StartOffset -ge $clause.Item2.Extent.StartOffset -and $call.Extent.EndOffset -le $clause.Item2.Extent.EndOffset) {
                        $guarded = $true
                    }
                }
            }
            $parent = $parent.Parent
        }
        Assert-True -Condition $guarded -Because "$readerName 必须包在 if (-not `$DryRun) 里:DryRun 不碰集群"
    }
}

Test-Case '③ Apply-OneGoSvc:db 注解块(服务名判断在最外层)先于本函数任何集群写操作,替换两个占位、对方组为空写 none' {
    $applyAst = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Apply-OneGoSvc' }, $false)
    Assert-True -Condition ($null -ne $applyAst) -Because '要有 Apply-OneGoSvc'
    $dbBlocks = @($applyAst.Body.FindAll({ param($a)
                $a -is [System.Management.Automation.Language.IfStatementAst] -and $a.Clauses[0].Item1.Extent.Text -ceq "`$SvcName -eq 'db'" -and
                $a.Extent.Text.Contains('Get-DbConsumerGroupPlan')
            }, $true))
    Assert-Equal -Expected 1 -Actual $dbBlocks.Count -Because "Apply-OneGoSvc 里恰好一个 if (`$SvcName -eq 'db') 块取消费组计划(k8s_migrate_gate.tests.ps1 按 AST 抽本函数跑别的服务,服务名判断必须在最外层)"
    $blockText = $dbBlocks[0].Extent.Text
    foreach ($needle in @('PLACEHOLDER_DB_CONSUMER_GROUP', 'PLACEHOLDER_DB_GROUP_HANDOFF_FROM', "'none'", 'PLACEHOLDER_DB_')) {
        Assert-True -Condition $blockText.Contains($needle) -Because "db 注解块要处理 $needle"
    }
    $writeNames = @('Invoke-KubectlWithInputFile', 'Invoke-Kubectl', 'Apply-InjectedSecret', 'Apply-LoginDevPasswordSecret', 'Apply-LoginOAuthSecret', 'Apply-GoSvcMigrateJob')
    $writes = @($applyAst.Body.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] -and $writeNames -contains $a.GetCommandName() }, $true))
    Assert-True -Condition ($writes.Count -gt 0) -Because 'Apply-OneGoSvc 里应当有集群写操作(取写操作的逻辑空转了?)'
    $firstWrite = ($writes | ForEach-Object { $_.Extent.StartOffset } | Measure-Object -Minimum).Minimum
    Assert-True -Condition ($dbBlocks[0].Extent.EndOffset -lt $firstWrite) -Because 'db 注解在任何集群写操作之前渲染完,缺占位的 throw 不留半截部署'
}

Test-Case '③ deploy/k8s/manifests/go-svc/db.yaml 的 Pod 模板带两条消费组注解占位(Apply-OneGoSvc 缺占位即拒绝部署 db)' {
    $manifest = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/go-svc/db.yaml') -Raw
    foreach ($pair in @(@{ Key = 'mmorpg.io/db-consumer-group'; Placeholder = 'PLACEHOLDER_DB_CONSUMER_GROUP' }, @{ Key = 'mmorpg.io/db-group-handoff-from'; Placeholder = 'PLACEHOLDER_DB_GROUP_HANDOFF_FROM' })) {
        Assert-Match -Text $manifest -Pattern ('(?m)^[ ]+' + [regex]::Escape($pair.Key) + ': "' + $pair.Placeholder + '"[ \t\r]*$') -Because "db.yaml 的 Pod 模板要有 $($pair.Key): `"$($pair.Placeholder)`"(只改 ConfigMap 时靠它滚动 db)"
    }
}

Test-Case '③ ConvertFrom-LiveDbConsumerGroup:四种合法形状(含生成器注释行、CRLF、行尾注释、被注释掉的同名键)' {
    $L = $DbGroupLegacy
    $Z = $DbGroupZone
    $legal = @(
        @{ Label = 'final(L) 旧 ConfigMap 原样'; Lines = @("    GroupID: `"$L`""); Want = "final:$L<-" }
        @{ Label = 'final(Z) 带生成器注释行与行尾注释'; Lines = @($DbGroupComment, "    GroupID: `"$Z`" # zone group"); Want = "final:$Z<-" }
        @{ Label = 'handoff(Z←L)'; Lines = @($DbGroupComment, "    ConsumerGroup: `"$Z`"", "    GroupHandoffFrom: `"$L`""); Want = "handoff:$Z<-$L" }
        @{ Label = 'handoff(L←Z)'; Lines = @("    ConsumerGroup: `"$L`"", "    GroupHandoffFrom: `"$Z`""); Want = "handoff:$L<-$Z" }
        @{ Label = 'final(L) 下方有 db.yaml 那样被注释掉的交接键'; Lines = @("    GroupID: `"$L`"", "    # ConsumerGroup: `"$Z`"", "    # GroupHandoffFrom: `"$L`""); Want = "final:$L<-" }
        @{ Label = 'final(Z) 单引号 / 裸值同样认'; Lines = @("    GroupID: '$Z'"); Want = "final:$Z<-" }
        @{ Label = 'final(Z) 裸值'; Lines = @("    GroupID: $Z"); Want = "final:$Z<-" }
    )
    foreach ($case in $legal) {
        foreach ($newline in @("`n", "`r`n")) {
            $yaml = (New-DbGroupLiveYaml -KafkaLines $case.Lines) -replace "`n", $newline
            $live = ConvertFrom-LiveDbConsumerGroup -DbYaml $yaml -LegacyGroup $L -ZoneGroup $Z
            Assert-Equal -Expected $case.Want -Actual (Format-DbGroupPlan $live) -Because "$($case.Label)(行尾 $(if ($newline -eq "`n") { 'LF' } else { 'CRLF' }))"
        }
    }
}

Test-Case '③ ConvertFrom-LiveDbConsumerGroup:四种之外一律 throw(GroupID+交接键、只有 ConsumerGroup、只有对方组、三键全无、未知组名、重复键、X=Y、空值)' {
    $L = $DbGroupLegacy
    $Z = $DbGroupZone
    $illegal = @(
        @{ Label = 'GroupID 配 GroupHandoffFrom(go/db 拒启)'; Lines = @("    GroupID: `"$Z`"", "    GroupHandoffFrom: `"$L`""); Pattern = 'GroupID 配 GroupHandoffFrom' }
        @{ Label = 'GroupID 与 ConsumerGroup 同时出现'; Lines = @("    GroupID: `"$L`"", "    ConsumerGroup: `"$Z`""); Pattern = '同时写了 GroupID 与 ConsumerGroup' }
        @{ Label = '只有 ConsumerGroup、没有 GroupHandoffFrom'; Lines = @("    ConsumerGroup: `"$Z`""); Pattern = '只有 ConsumerGroup' }
        @{ Label = '只有 GroupHandoffFrom'; Lines = @("    GroupHandoffFrom: `"$L`""); Pattern = '只有 GroupHandoffFrom' }
        @{ Label = '三个键都没有'; Lines = @(); Pattern = '一个都没有' }
        @{ Label = '未知组名'; Lines = @('    GroupID: "db_rpc_consumer_group_manual_test"'); Pattern = '不猜它的归属' }
        @{ Label = 'GroupID 出现两次'; Lines = @("    GroupID: `"$L`"", "    GroupID: `"$Z`""); Pattern = '出现了 2 次' }
        @{ Label = '交接给自己(X = Y)'; Lines = @("    ConsumerGroup: `"$Z`"", "    GroupHandoffFrom: `"$Z`""); Pattern = '不是 .+ 之一' }
        @{ Label = '交接对方是未知组'; Lines = @("    ConsumerGroup: `"$Z`"", '    GroupHandoffFrom: "someone_else"'); Pattern = '不是 .+ 之一' }
        @{ Label = '空值'; Lines = @('    GroupID: ""'); Pattern = '不是合法的消费组名' }
        @{ Label = '含非法字符'; Lines = @('    GroupID: "db rpc"'); Pattern = '不是合法的消费组名' }
    )
    foreach ($case in $illegal) {
        $message = ''
        try { $null = ConvertFrom-LiveDbConsumerGroup -DbYaml (New-DbGroupLiveYaml -KafkaLines $case.Lines) -LegacyGroup $L -ZoneGroup $Z }
        catch { $message = $_.Exception.Message }
        Assert-True -Condition ($message -ne '') -Because "$($case.Label):必须 throw(fail-closed,不把未知形状当成已知)"
        Assert-Match -Text $message -Pattern $case.Pattern -Because "$($case.Label):错误文本要点明哪里不合法"
        Assert-Match -Text $message -Pattern '合法形状只有四种' -Because "$($case.Label):错误文本要列出四种合法形状"
    }
    $empty = ''
    try { $null = ConvertFrom-LiveDbConsumerGroup -DbYaml '' -LegacyGroup $L -ZoneGroup $Z } catch { $empty = $_.Exception.Message }
    Assert-Match -Text $empty -Pattern '一个都没有' -Because '空文本同样拒绝'
}

Test-Case '③ ConvertFrom-LiveDbConsumerGroup:按路径只认 ServerConfig.Kafka 的直接子键 —— 别的段的同名键忽略、不算重复;组名键只在别的段即拒绝;Kafka 段写两遍算重复' {
    $L = $DbGroupLegacy
    $Z = $DbGroupZone
    $base = New-DbGroupLiveYaml -KafkaLines @("    GroupID: `"$L`"")
    # New-DbGroupLiveYaml 以 '  Database:' / '    MaxOpenConn: 60' 收尾:追加的 4 格缩进行落在 ServerConfig.Database 下,与 Kafka 子键同缩进。
    $ignored = @(
        @{ Label = 'ServerConfig.Database 下的同名键(4 格缩进)'; Yaml = $base + "`n    GroupID: `"$Z`"`n    ConsumerGroup: `"$Z`"`n    GroupHandoffFrom: `"$Z`"" }
        @{ Label = '顶层同名键'; Yaml = $base + "`nGroupID: `"someone_else`"`nConsumerGroup: `"$Z`"" }
        @{ Label = 'Kafka 下更深一层的同名键'; Yaml = (New-DbGroupLiveYaml -KafkaLines @("    GroupID: `"$L`"", '    Retry:', "      GroupID: `"$Z`"", "      GroupHandoffFrom: `"$Z`"")) }
    )
    foreach ($case in $ignored) {
        foreach ($newline in @("`n", "`r`n")) {
            $live = ConvertFrom-LiveDbConsumerGroup -DbYaml ($case.Yaml -replace "`n", $newline) -LegacyGroup $L -ZoneGroup $Z
            Assert-Equal -Expected "final:$L<-" -Actual (Format-DbGroupPlan $live) -Because "$($case.Label):go/db 只读 ServerConfig.Kafka 下的三个键,别处的同名键不改变形状,也不触发「出现了 2 次」或「同时写了」的拒绝"
        }
    }

    # 组名键被手改写到别的段(顶层与 Database 下各一处):go/db 实际读不到,不能当成有效形状。
    $misplaced = @('Name: db.rpc', 'ZoneId: 101', "GroupID: `"$L`"", 'ServerConfig:', '  Kafka:', '    Brokers:', '      - "kafka.contract-infra:9092"',
        '    PartitionCnt: 10', '  Database:', "    GroupID: `"$L`"", '    MaxOpenConn: 60') -join "`n"
    $message = ''
    try { $null = ConvertFrom-LiveDbConsumerGroup -DbYaml $misplaced -LegacyGroup $L -ZoneGroup $Z } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'ServerConfig\.Kafka 下 .*一个都没有' -Because '组名键只在别的段时必须拒绝(fail-closed),文案点明要的是 ServerConfig.Kafka 下的键'

    # 同一路径两处才算重复:Kafka 段写了两遍,两处都是 ServerConfig.Kafka.GroupID。扁平表会静默取后一处,所以必须另数。
    $twice = @('ServerConfig:', '  Kafka:', "    GroupID: `"$L`"", '  Database:', '    MaxOpenConn: 60', '  Kafka:', "    GroupID: `"$Z`"") -join "`n"
    $message = ''
    try { $null = ConvertFrom-LiveDbConsumerGroup -DbYaml $twice -LegacyGroup $L -ZoneGroup $Z } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'ServerConfig\.Kafka\.GroupID 出现了 2 次' -Because 'Kafka 段重复时同一路径出现两次,不猜哪一处生效'
}

Test-Case '③ New-DbConsumerGroupYaml:先一行注释,final 一行 GroupID、handoff 两行;非四种形状即 throw;渲染结果能被 ConvertFrom-LiveDbConsumerGroup 原样读回' {
    $L = $DbGroupLegacy
    $Z = $DbGroupZone
    $cases = @(
        @{ Plan = [pscustomobject]@{ Shape = 'final'; Group = $L; HandoffFrom = '' }; Want = @($DbGroupComment, "    GroupID: `"$L`"") }
        @{ Plan = [pscustomobject]@{ Shape = 'final'; Group = $Z; HandoffFrom = '' }; Want = @($DbGroupComment, "    GroupID: `"$Z`"") }
        @{ Plan = [pscustomobject]@{ Shape = 'handoff'; Group = $Z; HandoffFrom = $L }; Want = @($DbGroupComment, "    ConsumerGroup: `"$Z`"", "    GroupHandoffFrom: `"$L`"") }
        @{ Plan = [pscustomobject]@{ Shape = 'handoff'; Group = $L; HandoffFrom = $Z }; Want = @($DbGroupComment, "    ConsumerGroup: `"$L`"", "    GroupHandoffFrom: `"$Z`"") }
    )
    foreach ($case in $cases) {
        $yaml = New-DbConsumerGroupYaml -Plan $case.Plan
        Assert-True -Condition ($yaml -ceq ($case.Want -join "`n")) -Because "$(Format-DbGroupPlan $case.Plan) 的渲染必须逐字等于期望。实际: $yaml"
        $back = ConvertFrom-LiveDbConsumerGroup -DbYaml (New-DbGroupLiveYaml -KafkaLines ($yaml -split "`n")) -LegacyGroup $L -ZoneGroup $Z
        Assert-Equal -Expected (Format-DbGroupPlan $case.Plan) -Actual (Format-DbGroupPlan $back) -Because '生成器写出的形状必须能被 keep 原样读回'
    }
    foreach ($bad in @(
            [pscustomobject]@{ Shape = 'bogus'; Group = $Z; HandoffFrom = '' }
            [pscustomobject]@{ Shape = 'final'; Group = $Z; HandoffFrom = $L }
            [pscustomobject]@{ Shape = 'handoff'; Group = $Z; HandoffFrom = $Z }
            [pscustomobject]@{ Shape = 'handoff'; Group = $Z; HandoffFrom = '' }
            [pscustomobject]@{ Shape = 'final'; Group = 'bad"name'; HandoffFrom = '' }
        )) {
        $message = ''
        try { $null = New-DbConsumerGroupYaml -Plan $bad } catch { $message = $_.Exception.Message }
        Assert-True -Condition ($message -ne '') -Because "$(Format-DbGroupPlan $bad) 不是四种形状之一,必须 throw、不渲染半截配置"
    }
    $script:DbConsumerGroupPlans = @{}
    $missing = ''
    try { $null = Get-DbConsumerGroupPlan -CurrentZoneId 101 } catch { $missing = $_.Exception.Message }
    Assert-Match -Text $missing -Pattern '内部错误' -Because '没算过计划就生成 db ConfigMap 必须 throw,不回落任何默认组'
}

Test-Case '③ Test-DbConsumerGroupNeedsKafkaFacts:DryRun 从不读;keep 只在线上没有 ConfigMap 或是交接形状时读;显式模式非 DryRun 一律读' {
    $final = [pscustomobject]@{ Shape = 'final'; Group = $DbGroupLegacy; HandoffFrom = '' }
    $handoff = [pscustomobject]@{ Shape = 'handoff'; Group = $DbGroupZone; HandoffFrom = $DbGroupLegacy }
    foreach ($case in @(
            @{ Mode = 'keep'; DryRun = $true; Live = $null; Want = $false }
            @{ Mode = 'zone'; DryRun = $true; Live = $null; Want = $false }
            @{ Mode = 'legacy'; DryRun = $true; Live = $handoff; Want = $false }
            @{ Mode = 'keep'; DryRun = $false; Live = $final; Want = $false }
            @{ Mode = 'keep'; DryRun = $false; Live = $handoff; Want = $true }
            @{ Mode = 'keep'; DryRun = $false; Live = $null; Want = $true }
            @{ Mode = 'zone'; DryRun = $false; Live = $final; Want = $true }
            @{ Mode = 'legacy'; DryRun = $false; Live = $null; Want = $true }
        )) {
        $liveText = if ($null -eq $case.Live) { 'null' } else { $case.Live.Shape }
        Assert-Equal -Expected $case.Want -Actual (Test-DbConsumerGroupNeedsKafkaFacts -Mode $case.Mode -IsDryRun $case.DryRun -Live $case.Live) -Because "mode=$($case.Mode) dryrun=$($case.DryRun) live=$liveText"
    }
}

Test-Case '③ 决策表(Resolve-DbConsumerGroupPlan):修订稿 §2 每一行(含回档行);F2 / F4 拒绝附收尾命令;Kafka 事实缺失或读不到一律 fail-closed' {
    $L = $DbGroupLegacy
    $Z = $DbGroupZone
    $finalL = [pscustomobject]@{ Shape = 'final'; Group = $L; HandoffFrom = '' }
    $finalZ = [pscustomobject]@{ Shape = 'final'; Group = $Z; HandoffFrom = '' }
    $zFromL = [pscustomobject]@{ Shape = 'handoff'; Group = $Z; HandoffFrom = $L }
    $lFromZ = [pscustomobject]@{ Shape = 'handoff'; Group = $L; HandoffFrom = $Z }
    $both = @{ ZoneHasOffsets = $true; LegacyHasOffsets = $true }
    $zOnly = @{ ZoneHasOffsets = $true; LegacyHasOffsets = $false }
    $lOnly = @{ ZoneHasOffsets = $false; LegacyHasOffsets = $true }
    $none = @{ ZoneHasOffsets = $false; LegacyHasOffsets = $false }
    $finalizeZ = "-Group $Z -RetireGroup $L -Partitions $DbGroupPartitions -FinalizeHandoff"
    $finalizeL = "-Group $L -RetireGroup $Z -Partitions $DbGroupPartitions -FinalizeHandoff"
    $rows = @(
        # DryRun:不读集群,只看模式。
        @{ Label = 'DryRun keep'; Mode = 'keep'; DryRun = $true; Live = $finalL; Want = "final:$Z<-"; Source = 'dryrun-preview'; Warning = @('DbConsumerGroup=keep', 'dryrun-preview') }
        @{ Label = 'DryRun zone'; Mode = 'zone'; DryRun = $true; Want = "handoff:$Z<-$L"; Source = 'dryrun-preview'; Warning = @('DbConsumerGroup=zone', 'dryrun-preview') }
        @{ Label = 'DryRun legacy'; Mode = 'legacy'; DryRun = $true; Want = "handoff:$L<-$Z"; Source = 'dryrun-preview'; Warning = @('DbConsumerGroup=legacy', 'dryrun-preview') }
        # keep:线上最终形状原样保留,不需要 Kafka 事实。
        @{ Label = 'keep + final(L)'; Mode = 'keep'; Live = $finalL; Want = "final:$L<-"; Source = 'live-configmap' }
        @{ Label = 'keep + final(Z)'; Mode = 'keep'; Live = $finalZ; Want = "final:$Z<-"; Source = 'live-configmap' }
        # keep:线上交接形状。
        @{ Label = 'keep + handoff(Z←L) + 读不到 Kafka'; Mode = 'keep'; Live = $zFromL; FactsError = 'kafka-0 unreachable'; Want = "handoff:$Z<-$L"; Source = 'live-configmap'; Warning = @('unreadable', 'kafka-0 unreachable') }
        @{ Label = 'keep + handoff(Z←L) + L 已收尾'; Mode = 'keep'; Live = $zFromL; Facts = $zOnly; Want = "final:$Z<-"; Source = 'handoff-finalized'; Notice = @('handoff finalized', 'GroupHandoffFrom') }
        @{ Label = 'keep + handoff(Z←L) + L 仍有位点'; Mode = 'keep'; Live = $zFromL; Facts = $both; Want = "handoff:$Z<-$L"; Source = 'live-configmap'; Warning = @($finalizeZ, '-BootstrapServer kafka.contract-infra:9092', "-Topic $DbGroupTopic") }
        @{ Label = 'keep + handoff(L←Z) + Z 已收尾'; Mode = 'keep'; Live = $lFromZ; Facts = $lOnly; Want = "final:$L<-"; Source = 'handoff-finalized'; Notice = @('handoff finalized') }
        @{ Label = 'keep + handoff(L←Z) + Z 仍有位点'; Mode = 'keep'; Live = $lFromZ; Facts = $both; Want = "handoff:$L<-$Z"; Source = 'live-configmap'; Warning = @($finalizeL) }
        # keep:全新 namespace,以 Kafka 为准,只渲染最终形状。
        @{ Label = 'keep + 全新 namespace + 两组都有位点(F4)'; Mode = 'keep'; Facts = $both; Want = 'throw'; Throws = @('全新 namespace', $finalizeZ, $finalizeL, '尚未做任何写操作') }
        @{ Label = 'keep + 全新 namespace + 只有 Z'; Mode = 'keep'; Facts = $zOnly; Want = "final:$Z<-"; Source = 'fresh-namespace-kafka' }
        @{ Label = 'keep + 全新 namespace + 只有 L(保留反向决定)'; Mode = 'keep'; Facts = $lOnly; Want = "final:$L<-"; Source = 'fresh-namespace-kafka'; Notice = @('no implicit group switch') }
        @{ Label = 'keep + 全新 namespace + 都没有(新 zone)'; Mode = 'keep'; Facts = $none; Want = "final:$Z<-"; Source = 'fresh-namespace-kafka' }
        @{ Label = 'keep + 全新 namespace + 读不到 Kafka'; Mode = 'keep'; FactsError = 'boom'; Want = 'throw'; Throws = @('读不到 Kafka 位点', 'boom', 'pods/exec') }
        # 显式 zone(X = Z,Y = L)。
        @{ Label = 'zone + 两组都有 + 线上已是 handoff(Z←L)(幂等重跑)'; Mode = 'zone'; Live = $zFromL; Facts = $both; Want = "handoff:$Z<-$L"; Source = 'explicit-handoff'; Warning = @($finalizeZ) }
        @{ Label = 'zone + 两组都有 + 线上 final(L)(F2)'; Mode = 'zone'; Live = $finalL; Facts = $both; Want = 'throw'; Throws = @('-DbConsumerGroup zone 被拒绝', $finalizeL, '尚未做任何写操作') }
        @{ Label = 'zone + 两组都有 + 线上 final(Z)(F2)'; Mode = 'zone'; Live = $finalZ; Facts = $both; Want = 'throw'; Throws = @('-DbConsumerGroup zone 被拒绝', $finalizeZ) }
        @{ Label = 'zone + 两组都有 + 线上没有(F2,两种写法)'; Mode = 'zone'; Facts = $both; Want = 'throw'; Throws = @('线上没有 go-svc-db-config', $finalizeZ, $finalizeL) }
        @{ Label = 'zone + 只有 Z + 线上没有(回档 Step 6)'; Mode = 'zone'; Facts = $zOnly; Want = "final:$Z<-"; Source = 'explicit-already-authoritative' }
        @{ Label = 'zone + 只有 Z + 线上 final(L)'; Mode = 'zone'; Live = $finalL; Facts = $zOnly; Want = "final:$Z<-"; Source = 'explicit-already-authoritative' }
        @{ Label = 'zone + 只有 L(正向交接)'; Mode = 'zone'; Live = $finalL; Facts = $lOnly; Want = "handoff:$Z<-$L"; Source = 'explicit-handoff'; Notice = @($finalizeZ) }
        @{ Label = 'zone + 都没有'; Mode = 'zone'; Facts = $none; Want = "final:$Z<-"; Source = 'explicit-nothing-to-inherit' }
        @{ Label = 'zone + 读不到 Kafka'; Mode = 'zone'; Live = $finalL; FactsError = 'forbidden'; Want = 'throw'; Throws = @('读不到 Kafka 位点', 'forbidden') }
        # 显式 legacy(X = L,Y = Z)。
        @{ Label = 'legacy + 两组都有 + 线上已是 handoff(L←Z)(幂等重跑)'; Mode = 'legacy'; Live = $lFromZ; Facts = $both; Want = "handoff:$L<-$Z"; Source = 'explicit-handoff'; Warning = @($finalizeL) }
        @{ Label = 'legacy + 两组都有 + 线上 final(Z)(F2:正向没收尾就反向)'; Mode = 'legacy'; Live = $finalZ; Facts = $both; Want = 'throw'; Throws = @('-DbConsumerGroup legacy 被拒绝', $finalizeZ) }
        @{ Label = 'legacy + 两组都有 + 线上 handoff(Z←L)(方向相反,不是幂等重跑)'; Mode = 'legacy'; Live = $zFromL; Facts = $both; Want = 'throw'; Throws = @('-DbConsumerGroup legacy 被拒绝', $finalizeZ) }
        @{ Label = 'legacy + 只有 L'; Mode = 'legacy'; Live = $finalL; Facts = $lOnly; Want = "final:$L<-"; Source = 'explicit-already-authoritative' }
        @{ Label = 'legacy + 只有 Z(反向交接)'; Mode = 'legacy'; Live = $finalZ; Facts = $zOnly; Want = "handoff:$L<-$Z"; Source = 'explicit-handoff'; Notice = @($finalizeL) }
        @{ Label = 'legacy + 都没有'; Mode = 'legacy'; Facts = $none; Want = "final:$L<-"; Source = 'explicit-nothing-to-inherit' }
        # Kafka 事实缺字段 / 不是布尔:不按「没有位点」猜。
        @{ Label = 'zone + 事实缺 LegacyHasOffsets'; Mode = 'zone'; Facts = @{ ZoneHasOffsets = $true }; Want = 'throw'; Throws = @('内部错误', 'LegacyHasOffsets') }
        @{ Label = 'keep + 全新 namespace + 事实不是布尔'; Mode = 'keep'; Facts = @{ ZoneHasOffsets = 'yes'; LegacyHasOffsets = $false }; Want = 'throw'; Throws = @('内部错误', 'ZoneHasOffsets') }
        @{ Label = 'keep + 全新 namespace + 没有事实'; Mode = 'keep'; Want = 'throw'; Throws = @('内部错误') }
    )
    foreach ($row in $rows) {
        $planArgs = @{
            Mode = $row.Mode; LegacyGroup = $L; ZoneGroup = $Z; IsDryRun = [bool]$row.DryRun
            Live = $row.Live; Facts = $row.Facts; FactsError = [string]$row.FactsError
            Topic = $DbGroupTopic; Partitions = $DbGroupPartitions; BootstrapServer = 'kafka.contract-infra:9092'
        }
        if ($row.Want -ceq 'throw') {
            $message = ''
            try { $null = Resolve-DbConsumerGroupPlan @planArgs } catch { $message = $_.Exception.Message }
            Assert-True -Condition ($message -ne '') -Because "$($row.Label):必须在任何写操作之前拒绝"
            foreach ($needle in @($row.Throws)) {
                Assert-True -Condition $message.Contains($needle) -Because "$($row.Label):拒绝文案要含「$needle」。实际: $message"
            }
            continue
        }
        $plan = Resolve-DbConsumerGroupPlan @planArgs
        Assert-Equal -Expected $row.Want -Actual (Format-DbGroupPlan $plan) -Because "$($row.Label) 的形状"
        Assert-Equal -Expected $row.Source -Actual $plan.Source -Because "$($row.Label) 的 Source"
        foreach ($field in @('Notice', 'Warning')) {
            $actual = [string]$plan.$field
            if ($row.ContainsKey($field)) {
                foreach ($needle in @($row[$field])) {
                    Assert-True -Condition $actual.Contains($needle) -Because "$($row.Label):$field 要含「$needle」。实际: $actual"
                }
            }
            else {
                Assert-Equal -Expected '' -Actual $actual -Because "$($row.Label):不该有 $field"
            }
        }
    }

    # 回档行(修订稿 §2「只有 X 有」与 §12):Step 5 收尾后 Live=null、Zc=true、Lc=false → final(Z),渲染恰好是注释行加一行 GroupID,
    # 交接闸之前的旧镜像也能加载(config_test 的 TestPreHandoffBinaryLoadsFinalZoneShape 用同一行)。
    $rollback = Resolve-DbConsumerGroupPlan -Mode 'zone' -LegacyGroup $L -ZoneGroup $Z -IsDryRun $false -Live $null -Facts $zOnly -Topic $DbGroupTopic -Partitions $DbGroupPartitions
    $rollbackYaml = New-DbConsumerGroupYaml -Plan $rollback
    Assert-True -Condition ($rollbackYaml -ceq ($DbGroupComment + "`n" + '    GroupID: "db_rpc_consumer_group_z101"')) -Because "回档 Step 6 的 db ConfigMap 必须恰好是注释行加 ``    GroupID: `"db_rpc_consumer_group_z101`"``。实际: $rollbackYaml"
}

Test-Case '③ Read-DbGroupTopicFacts:exec 进 kafka-0 的 kafka 容器(小堆 KAFKA_HEAP_OPTS、localhost:9092、--group <组>),stdout / stderr 合并后分类;读不到一律 throw' {
    Reset-DbGroupFixture -Rules @((New-DbGroupDescribeRule -Group $DbGroupZone -State 'offsets'), (New-DbGroupDescribeRule -Group $DbGroupLegacy -State 'missing'))
    $facts = Read-DbGroupTopicFacts -Topic $DbGroupTopic -ZoneGroup $DbGroupZone -LegacyGroup $DbGroupLegacy
    Assert-True -Condition ($facts.ZoneHasOffsets -eq $true) -Because 'zone 组在本 topic 上有数值 CURRENT-OFFSET'
    Assert-True -Condition ($facts.LegacyHasOffsets -eq $false) -Because 'legacy 组不存在(文案在 stderr)= 没有位点'
    Assert-Equal -Expected 2 -Actual $script:DbGroupKubectlCalls.Count -Because '两组各 describe 一次'
    $expectedArgs = @('exec', '-n', 'contract-infra', 'kafka-0', '-c', 'kafka', '--', '/bin/sh', '-c', "$DbGroupExecShell $DbGroupZone")
    Assert-Equal -Expected ($expectedArgs -join '|') -Actual ($script:DbGroupKubectlCalls[0].Args -join '|') -Because '命令形状:infra namespace 的 kafka-0、kafka 容器、/bin/sh -c 里覆盖 KAFKA_HEAP_OPTS(容器 env 的大堆会被 CLI 继承,把 broker 撑到 OOMKilled)'
    Assert-Equal -Expected 'contract-db-group-ctx' -Actual $script:DbGroupKubectlCalls[0].KubeContext -Because '显式带上 -KubeContext,不落到本机默认 context'
    Assert-True -Condition ($script:DbGroupKubectlCalls[0].CaptureOutput -and $script:DbGroupKubectlCalls[0].AllowFailure) -Because '捕获输出、退出码交给分类函数判'

    Reset-DbGroupFixture -Rules @((New-DbGroupDescribeRule -Group $DbGroupZone -State 'no-offsets'), (New-DbGroupDescribeRule -Group $DbGroupLegacy -State 'offsets'))
    $facts = Read-DbGroupTopicFacts -Topic $DbGroupTopic -ZoneGroup $DbGroupZone -LegacyGroup $DbGroupLegacy
    Assert-True -Condition ($facts.ZoneHasOffsets -eq $false -and $facts.LegacyHasOffsets -eq $true) -Because '本 topic 只有 "-" 行(别的 topic 有位点也不算)= 没有位点'

    foreach ($state in @('exec-failed', 'error-line')) {
        Reset-DbGroupFixture -Rules @((New-DbGroupDescribeRule -Group $DbGroupZone -State $state), (New-DbGroupDescribeRule -Group $DbGroupLegacy -State 'missing'))
        $message = ''
        try { $null = Read-DbGroupTopicFacts -Topic $DbGroupTopic -ZoneGroup $DbGroupZone -LegacyGroup $DbGroupLegacy } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern 'kafka-consumer-groups --describe 失败' -Because "${state}:读不到必须 throw,不当成「没有位点」"
    }
}

Test-Case '③ Read-LiveDbConfigMapYaml:不存在返回 $null;kubectl 失败、输出不是 JSON、缺 data.db.yaml 一律 throw(读不到不等于没有)' {
    $text = New-DbGroupLiveYaml -KafkaLines @("    GroupID: `"$DbGroupLegacy`"")
    Reset-DbGroupFixture -Rules @(New-DbGroupConfigMapRule -DbYaml $text)
    Assert-Equal -Expected $text -Actual (Read-LiveDbConfigMapYaml -Namespace 'mmorpg-zone-contract-test') -Because '返回 data.db.yaml 原文'
    Assert-Equal -Expected 'get|configmap|go-svc-db-config|-n|mmorpg-zone-contract-test|--ignore-not-found|-o|json' -Actual ($script:DbGroupKubectlCalls[0].Args -join '|') -Because '只读,--ignore-not-found 区分「不存在」与「读失败」'

    Reset-DbGroupFixture -Rules @(New-DbGroupConfigMapRule -DbYaml '')
    Assert-True -Condition ($null -eq (Read-LiveDbConfigMapYaml -Namespace 'mmorpg-zone-contract-test')) -Because 'ConfigMap 不存在(含 namespace 已删)返回 $null'

    foreach ($case in @(
            @{ Label = 'kubectl 失败'; Rule = (New-DbGroupConfigMapRule -ExitCode 1); Pattern = '读取线上 ConfigMap go-svc-db-config 失败' }
            @{ Label = '输出不是 JSON'; Rule = (New-DbGroupConfigMapRule -RawStdout 'not json'); Pattern = '不是合法 JSON' }
            @{ Label = '缺 data.db.yaml'; Rule = (New-DbGroupConfigMapRule -RawStdout '{"apiVersion":"v1","kind":"ConfigMap","data":{"other.yaml":"x"}}'); Pattern = '没有 data\.db\.yaml' }
        )) {
        Reset-DbGroupFixture -Rules @($case.Rule)
        $message = ''
        try { $null = Read-LiveDbConfigMapYaml -Namespace 'mmorpg-zone-contract-test' } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern $case.Pattern -Because "$($case.Label):必须 throw"
        Assert-Match -Text $message -Pattern '尚未做任何写操作' -Because "$($case.Label):报错要说明还没有写集群"
    }
}

Test-Case '③ Initialize-DbConsumerGroupPlans(非 DryRun,假 kubectl):keep + 线上最终形状只读 ConfigMap、不 exec;每个 zone 打一行计划' {
    Reset-DbGroupFixture -Rules @(New-DbGroupConfigMapRule -DbYaml (New-DbGroupLiveYaml -KafkaLines @("    GroupID: `"$DbGroupLegacy`"")))
    $out = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String
    Assert-Equal -Expected "final:$DbGroupLegacy<-" -Actual (Format-DbGroupPlan $script:DbConsumerGroupPlans[101]) -Because '线上 final(L) 原样保留'
    Assert-Equal -Expected 'live-configmap' -Actual $script:DbConsumerGroupPlans[101].Source -Because 'Source 记明来自线上 ConfigMap'
    Assert-Equal -Expected 1 -Actual $script:DbGroupKubectlCalls.Count -Because '最终形状不读 Kafka(日常发布不依赖 kafka-0 的 exec 权限)'
    Assert-Match -Text $out -Pattern ("DB consumer group: zone=contract-test\(101\) mode=keep shape=final group=$([regex]::Escape($DbGroupLegacy)) handoff_from=none source=live-configmap topic=$([regex]::Escape($DbGroupTopic))") -Because '计划行'
}

Test-Case '③ Initialize-DbConsumerGroupPlans:keep + 线上交接形状 —— 对方组已收尾即落到最终形状;读不到 Kafka 保持线上形状并告警' {
    $handoffYaml = New-DbGroupLiveYaml -KafkaLines @($DbGroupComment, "    ConsumerGroup: `"$DbGroupZone`"", "    GroupHandoffFrom: `"$DbGroupLegacy`"")
    Reset-DbGroupFixture -Rules @((New-DbGroupConfigMapRule -DbYaml $handoffYaml), (New-DbGroupDescribeRule -Group $DbGroupZone -State 'offsets'), (New-DbGroupDescribeRule -Group $DbGroupLegacy -State 'missing'))
    $out = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String
    Assert-Equal -Expected "final:$DbGroupZone<-" -Actual (Format-DbGroupPlan $script:DbConsumerGroupPlans[101]) -Because 'legacy 组在本 topic 上已没有位点(FinalizeHandoff 删过),去掉 GroupHandoffFrom'
    Assert-Equal -Expected 'handoff-finalized' -Actual $script:DbConsumerGroupPlans[101].Source -Because 'Source=handoff-finalized'
    Assert-Match -Text $out -Pattern 'handoff finalized' -Because 'Notice 要打出来(db 将滚动一次)'
    Assert-Equal -Expected 3 -Actual $script:DbGroupKubectlCalls.Count -Because '读一次 ConfigMap、两组各 describe 一次'

    Reset-DbGroupFixture -Rules @((New-DbGroupConfigMapRule -DbYaml $handoffYaml), (New-DbGroupDescribeRule -Group $DbGroupZone -State 'exec-failed'))
    $out = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String
    Assert-Equal -Expected "handoff:$DbGroupZone<-$DbGroupLegacy" -Actual (Format-DbGroupPlan $script:DbConsumerGroupPlans[101]) -Because '读不到 Kafka 时原样保留线上交接形状(永远安全),不 throw'
    Assert-Match -Text $out -Pattern 'unreadable' -Because '要告警说明这次没能判断收尾'
}

Test-Case '③ Initialize-DbConsumerGroupPlans:显式 zone + 全新 namespace —— 回档 Step 6 落到 final(Z);读不到 Kafka 在写操作之前拒绝;ConfigMap 读失败同样拒绝' {
    Reset-DbGroupFixture -Rules @((New-DbGroupConfigMapRule -DbYaml ''), (New-DbGroupDescribeRule -Group $DbGroupZone -State 'offsets'), (New-DbGroupDescribeRule -Group $DbGroupLegacy -State 'no-offsets'))
    $script:DbConsumerGroup = 'zone'
    $null = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String
    $plan = Get-DbConsumerGroupPlan -CurrentZoneId 101
    Assert-Equal -Expected "final:$DbGroupZone<-" -Actual (Format-DbGroupPlan $plan) -Because 'Step 5 收尾后只有 zone 组有位点 → final(Z),任何镜像都能起'
    Assert-Equal -Expected 'explicit-already-authoritative' -Actual $plan.Source -Because '回档行的 Source'
    Assert-True -Condition ((New-DbConsumerGroupYaml -Plan $plan) -ceq ($DbGroupComment + "`n" + "    GroupID: `"$DbGroupZone`"")) -Because 'ConfigMap 里恰好注释行加一行 GroupID'

    Reset-DbGroupFixture -Rules @((New-DbGroupConfigMapRule -DbYaml ''), (New-DbGroupDescribeRule -Group $DbGroupZone -State 'exec-failed'))
    $script:DbConsumerGroup = 'zone'
    $message = ''
    try { $null = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern '读不到 Kafka 位点,无法判定 db 消费组归属' -Because '显式模式读不到 Kafka 必须 throw(此时还没有任何写操作)'
    Assert-Match -Text $message -Pattern 'pods/exec' -Because '报错提示检查 kafka-0 与 exec 权限'

    Reset-DbGroupFixture -Rules @(New-DbGroupConfigMapRule -ExitCode 1)
    $message = ''
    try { $null = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern '读取线上 ConfigMap go-svc-db-config 失败' -Because 'keep 读不到 ConfigMap 不能当成全新 namespace'
    Assert-Equal -Expected 1 -Actual $script:DbGroupKubectlCalls.Count -Because 'ConfigMap 读失败就停,不再 exec 进 kafka-0'

    Reset-DbGroupFixture -Rules @(New-DbGroupConfigMapRule -DbYaml (New-DbGroupLiveYaml -KafkaLines @("    GroupID: `"$DbGroupZone`"", "    GroupHandoffFrom: `"$DbGroupLegacy`"")))
    $message = ''
    try { $null = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'GroupID 配 GroupHandoffFrom' -Because '线上形状不合法即拒绝'
    Assert-Equal -Expected 1 -Actual $script:DbGroupKubectlCalls.Count -Because '形状不合法时不再读 Kafka'
}

Test-Case '③ R7 [0] 显式交接渲染交接形状之前,线上每个 db Pod 的 /metrics 都要有 db_consumer_group_handoff_gate_total;证明不了即在写操作之前拒绝' {
    # go-svc-db-config 整目录挂载、进程只在启动时读:旧 ReplicaSet 的 Pod 一旦重启就读到交接形状,交接闸之前的二进制在 MustLoad 拒启。
    $finalL = New-DbGroupLiveYaml -KafkaLines @("    GroupID: `"$DbGroupLegacy`"")
    $forward = @((New-DbGroupConfigMapRule -DbYaml $finalL), (New-DbGroupDescribeRule -Group $DbGroupZone -State 'missing'), (New-DbGroupDescribeRule -Group $DbGroupLegacy -State 'offsets'))
    $pods = @(@{ Name = 'db-a' }, @{ Name = 'db-b' }, @{ Name = 'db-old'; Deleting = $true }, @{ Name = 'db-evicted'; Phase = 'Failed' })
    $runInit = {
        param([object[]]$Rules, [string]$Mode)
        Reset-DbGroupFixture -Rules $Rules
        $script:DbConsumerGroup = $Mode
        $message = ''
        $out = ''
        try { $out = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String } catch { $message = $_.Exception.Message }
        return [pscustomobject]@{ Message = $message; Output = $out }
    }

    # 1. 两个运行中的 Pod 都带交接闸:放行,渲染 handoff(Z←L)。在删除 / 已终结的 Pod 不抓(没有它们的规则,抓了就 throw)。
    $ok = & $runInit ($forward + @((New-DbGroupPodsRule -Pods $pods), (New-DbGroupMetricsRule -Pod 'db-a' -State 'gate'), (New-DbGroupMetricsRule -Pod 'db-b' -State 'gate'))) 'zone'
    Assert-Equal -Expected '' -Actual $ok.Message -Because '每个 db Pod 都带交接闸时放行'
    Assert-Equal -Expected "handoff:$DbGroupZone<-$DbGroupLegacy" -Actual (Format-DbGroupPlan (Get-DbConsumerGroupPlan -CurrentZoneId 101)) -Because '正向交接照常渲染交接形状'
    $scrapes = @($script:DbGroupKubectlCalls | Where-Object { $_.Line -match '^get --raw ' })
    Assert-Equal -Expected 2 -Actual $scrapes.Count -Because '只抓运行中的两个 Pod'
    Assert-True -Condition (@($scrapes | Where-Object { $_.KubeContext -ne 'contract-db-group-ctx' -or -not $_.CaptureOutput -or -not $_.AllowFailure }).Count -eq 0) -Because '显式带上 -KubeContext,捕获输出、退出码自己判'

    # 2. 有一个 Pod 是交接闸之前的二进制:拒绝,不留计划,给出收回命令(zone → legacy,最终形状 GroupID=L)。
    $old = & $runInit ($forward + @((New-DbGroupPodsRule -Pods $pods), (New-DbGroupMetricsRule -Pod 'db-a' -State 'gate'), (New-DbGroupMetricsRule -Pod 'db-b' -State 'no-gate'))) 'zone'
    Assert-Match -Text $old.Message -Pattern 'Pod db-b:/metrics 里没有 db_consumer_group_handoff_gate_total' -Because '点名缺交接闸的 Pod'
    Assert-Match -Text $old.Message -Pattern '尚未做任何写操作' -Because '拒绝早于任何写操作'
    Assert-Match -Text $old.Message -Pattern ([regex]::Escape("-DbConsumerGroup legacy 收回到最终形状 GroupID=$DbGroupLegacy")) -Because '要给出交接 rollout 卡住后的收回办法'
    Assert-Equal -Expected 0 -Actual $script:DbConsumerGroupPlans.Count -Because '拒绝时本 zone 不留计划'

    # 3. 缺 pods/proxy 权限、4. 列 Pod 失败、5. 副本数 > 0 却没有可抓的 Pod:一律拒绝(fail-closed)。
    $proxy = & $runInit ($forward + @((New-DbGroupPodsRule -Pods @(@{ Name = 'db-a' })), (New-DbGroupMetricsRule -Pod 'db-a' -State 'forbidden'))) 'zone'
    Assert-Match -Text $proxy.Message -Pattern 'pods/proxy' -Because '抓不到 /metrics 时点名 pods/proxy 权限'
    $list = & $runInit ($forward + @(New-DbGroupPodsRule -ExitCode 1)) 'zone'
    Assert-Match -Text $list.Message -Pattern '读不到本 zone 的 db Deployment / Pod' -Because '读不到不等于没有'
    $rebuilding = & $runInit ($forward + @(New-DbGroupPodsRule -Replicas 2 -Pods @(@{ Name = 'db-gone'; Deleting = $true }))) 'zone'
    Assert-Match -Text $rebuilding.Message -Pattern '副本数是 2、却没有一个可检查的 db Pod' -Because '旧 ReplicaSet 正在拉起的 Pod 判不出带不带交接闸'

    # 6. 反向交接同一规则,收回命令是 -DbConsumerGroup zone(最终形状 GroupID=Z)。
    $finalZ = New-DbGroupLiveYaml -KafkaLines @("    GroupID: `"$DbGroupZone`"")
    $reverse = @((New-DbGroupConfigMapRule -DbYaml $finalZ), (New-DbGroupDescribeRule -Group $DbGroupZone -State 'offsets'), (New-DbGroupDescribeRule -Group $DbGroupLegacy -State 'missing'))
    $back = & $runInit ($reverse + @((New-DbGroupPodsRule -Pods @(@{ Name = 'db-a' })), (New-DbGroupMetricsRule -Pod 'db-a' -State 'no-gate'))) 'legacy'
    Assert-Match -Text $back.Message -Pattern ([regex]::Escape("-DbConsumerGroup zone 收回到最终形状 GroupID=$DbGroupZone")) -Because '反向交接的收回方向相反'

    # 7. namespace 里没有 db 进程(全新 namespace,Deployment 不存在):没有会重启进交接形状的旧 Pod,放行并告警。
    $fresh = @((New-DbGroupConfigMapRule -DbYaml ''), (New-DbGroupDescribeRule -Group $DbGroupZone -State 'missing'), (New-DbGroupDescribeRule -Group $DbGroupLegacy -State 'offsets'))
    $none = & $runInit ($fresh + @(New-DbGroupPodsRule -NoDeployment)) 'zone'
    Assert-Equal -Expected '' -Actual $none.Message -Because '没有旧进程可保护时不拒绝'
    Assert-Equal -Expected "handoff:$DbGroupZone<-$DbGroupLegacy" -Actual (Format-DbGroupPlan (Get-DbConsumerGroupPlan -CurrentZoneId 101)) -Because '全新 namespace + 只有 L 有位点 → handoff(Z←L)'
    # Write-Warning 经 Out-String 可能在空格处折行,先把空白折叠再匹配。
    Assert-Match -Text ($none.Output -replace '\s+', ' ') -Pattern 'has no running db process' -Because '要告警:新 Pod 不带交接闸时 rollout 卡住,以及怎么收回'

    # 8. 回档 Step 6(只有 Z 有位点 → final(Z))与 keep:不渲染交接形状,不做能力校验,一次 Pod 读取都没有。
    $rollback = & $runInit @((New-DbGroupConfigMapRule -DbYaml ''), (New-DbGroupDescribeRule -Group $DbGroupZone -State 'offsets'), (New-DbGroupDescribeRule -Group $DbGroupLegacy -State 'no-offsets')) 'zone'
    Assert-Equal -Expected '' -Actual $rollback.Message -Because '回档行照常通过'
    Assert-Equal -Expected 0 -Actual @($script:DbGroupKubectlCalls | Where-Object { $_.Line -match '^get (deployments,pods|--raw) ' }).Count -Because '最终形状任何二进制都能加载,不需要能力证明'
}

Test-Case '③ Initialize-DbConsumerGroupPlans:DryRun 一次集群都不读;不部署 db 时 keep 没有计划,显式模式在任何写操作之前拒绝' {
    Reset-DbGroupFixture
    $script:DryRun = $true
    $script:DbConsumerGroup = 'legacy'
    $out = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String
    Assert-Equal -Expected 0 -Actual $script:DbGroupKubectlCalls.Count -Because 'DryRun 不碰集群(Invoke-ClientEntryKubectl 不看 DryRun,判断在调用方)'
    Assert-Equal -Expected "handoff:$DbGroupLegacy<-$DbGroupZone" -Actual (Format-DbGroupPlan $script:DbConsumerGroupPlans[101]) -Because 'DryRun legacy 预览 handoff(L←Z)'
    Assert-Match -Text $out -Pattern 'dryrun-preview' -Because 'DryRun 要告警'

    Reset-DbGroupFixture
    $script:SkipGoSvc = $true
    $out = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String
    Assert-Equal -Expected 0 -Actual $script:DbConsumerGroupPlans.Count -Because 'keep + -SkipGoSvc:不部署 db,不算计划,也不拒绝'
    Assert-Equal -Expected 0 -Actual $script:DbGroupKubectlCalls.Count -Because '不部署 db 就不读集群'

    # 显式切组配在不部署 db 的运行上:原来只告警「no effect」、C++ 节点照常重部署、退出码 0,运维会以为切过了(修订稿 §14 步骤 3 的金丝雀命令
    # 不带 -GoSvcRegistry 就会踩中)。DryRun 也拒绝:这是参数组合错误,预览同样要红。
    foreach ($case in @(
            @{ Label = 'zone + -SkipGoSvc'; Mode = 'zone'; SkipGoSvc = $true; Registry = 'registry.invalid/test'; DryRun = $false }
            @{ Label = 'legacy + 没给 -GoSvcRegistry'; Mode = 'legacy'; SkipGoSvc = $false; Registry = ''; DryRun = $false }
            @{ Label = 'zone + 没给 -GoSvcRegistry + DryRun'; Mode = 'zone'; SkipGoSvc = $false; Registry = ''; DryRun = $true }
        )) {
        Reset-DbGroupFixture
        $script:DbConsumerGroup = $case.Mode
        $script:SkipGoSvc = $case.SkipGoSvc
        $script:GoSvcRegistry = $case.Registry
        $script:DryRun = $case.DryRun
        $message = ''
        try { $null = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern ([regex]::Escape("-DbConsumerGroup $($case.Mode) 需要本次部署 db")) -Because "$($case.Label):必须 throw 并点名模式,不能只告警后照常部署"
        Assert-Match -Text $message -Pattern '尚未做任何写操作' -Because "$($case.Label):报错要说明还没有写集群"
        Assert-Equal -Expected 0 -Actual $script:DbGroupKubectlCalls.Count -Because "$($case.Label):拒绝前不读集群"
        Assert-Equal -Expected 0 -Actual $script:DbConsumerGroupPlans.Count -Because "$($case.Label):不留计划"
    }
}

Test-Case '③ Initialize-DbConsumerGroupPlans(all-up,-SkipInfra,假 kubectl):zones 配置里每个 zone 各读一次自己的 ConfigMap、各算一份计划,计划键是各自的 zoneId' {
    $sampleZones = @($DbGroupSampleZones)
    Assert-True -Condition ($sampleZones.Count -ge 2) -Because '夹具前提:zones.sample.json 至少两个 zone'
    $rules = @($sampleZones | ForEach-Object {
            New-DbGroupConfigMapRule -Namespace "mmorpg-zone-$($_.name)" -DbYaml (New-DbGroupLiveYaml -KafkaLines @("    GroupID: `"$DbGroupLegacy`""))
        })
    Reset-DbGroupFixture -Rules $rules
    $script:Command = 'all-up'
    $script:SkipInfra = $true
    $script:ZonesConfigPath = $DbGroupSampleZonesPath
    $out = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String
    $wantKeys = @($sampleZones | ForEach-Object { [int]$_.zoneId } | Sort-Object)
    Assert-Equal -Expected ($wantKeys -join ',') -Actual (@($script:DbConsumerGroupPlans.Keys | ForEach-Object { [int]$_ } | Sort-Object) -join ',') -Because '计划键恰好是 zones 配置里的各个 zoneId([int],与 Apply-Zone 传入的 $zone.zoneId 一致)'
    $generation = [int](Get-EtcValue -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.TopicGeneration')
    foreach ($zone in $sampleZones) {
        $zoneId = [int]$zone.zoneId
        Assert-Equal -Expected "final:$DbGroupLegacy<-" -Actual (Format-DbGroupPlan (Get-DbConsumerGroupPlan -CurrentZoneId $zoneId)) -Because "zone ${zoneId}:线上 final(L) 原样保留"
        $topic = Get-DbTaskTopicName -ZoneId $zoneId -Generation $generation
        Assert-Match -Text $out -Pattern ("DB consumer group: zone=$([regex]::Escape([string]$zone.name))\($zoneId\) mode=keep shape=final group=$([regex]::Escape($DbGroupLegacy)) handoff_from=none source=live-configmap topic=$([regex]::Escape($topic))") -Because "zone $zoneId 一行计划,topic 按本 zone 推导"
    }
    $configMapReads = @($script:DbGroupKubectlCalls | Where-Object { $_.Line -match '^get configmap ' })
    Assert-Equal -Expected $sampleZones.Count -Actual $configMapReads.Count -Because '每个 zone 读一次自己 namespace 的 ConfigMap'
    Assert-Equal -Expected $sampleZones.Count -Actual $script:DbGroupKubectlCalls.Count -Because '最终形状不 exec;-SkipInfra 不探测 infra namespace'
}

Test-Case '③ Initialize-DbConsumerGroupPlans(all-up,不带 -SkipInfra):infra namespace 不存在 = 本次新建空 Kafka,不 exec、按两组都没有位点判定;探测失败即拒绝;namespace 在则照旧 exec' {
    $sampleZones = @($DbGroupSampleZones)
    Assert-True -Condition ($sampleZones.Count -ge 2) -Because '夹具前提:zones.sample.json 至少两个 zone'
    $first = $sampleZones[0]
    $second = $sampleZones[1]
    $firstZ = "${DbGroupLegacy}_z$([int]$first.zoneId)"
    $secondZ = "${DbGroupLegacy}_z$([int]$second.zoneId)"
    # 第一个 zone 没有 ConfigMap(全新);第二个 zone 线上是未收尾的交接形状(infra-down 之后 zone namespace 还在的情形);其余 zone 没有 ConfigMap。
    $configMapRules = @($sampleZones | ForEach-Object {
            if ([int]$_.zoneId -eq [int]$second.zoneId) {
                New-DbGroupConfigMapRule -Namespace "mmorpg-zone-$($_.name)" -DbYaml (New-DbGroupLiveYaml -KafkaLines @("    ConsumerGroup: `"$secondZ`"", "    GroupHandoffFrom: `"$DbGroupLegacy`""))
            }
            else {
                New-DbGroupConfigMapRule -Namespace "mmorpg-zone-$($_.name)" -DbYaml ''
            }
        })
    Reset-DbGroupFixture -Rules (@(New-DbGroupInfraNamespaceRule) + $configMapRules)
    $script:Command = 'all-up'
    $script:ZonesConfigPath = $DbGroupSampleZonesPath
    $out = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String
    $firstPlan = Get-DbConsumerGroupPlan -CurrentZoneId ([int]$first.zoneId)
    Assert-Equal -Expected "final:$firstZ<-" -Actual (Format-DbGroupPlan $firstPlan) -Because '全新 zone + 新建的空 Kafka → final(Z)'
    Assert-Equal -Expected 'fresh-namespace-kafka' -Actual $firstPlan.Source -Because 'Source 与「全新 namespace、两组都没有位点」同一行'
    $secondPlan = Get-DbConsumerGroupPlan -CurrentZoneId ([int]$second.zoneId)
    Assert-Equal -Expected "final:$secondZ<-" -Actual (Format-DbGroupPlan $secondPlan) -Because '线上 handoff(Z←L) + 新建的空 Kafka:对方组不可能有位点,收尾成 final(Z)'
    Assert-Equal -Expected 'handoff-finalized' -Actual $secondPlan.Source -Because '走 keep + 交接形状 + 对方组无位点那一行'
    Assert-Equal -Expected 0 -Actual @($script:DbGroupKubectlCalls | Where-Object { $_.Line -match '^exec ' }).Count -Because 'infra namespace 不存在时不 exec 进 kafka-0(它还没建)'
    Assert-Equal -Expected 'get|namespace|contract-infra|--ignore-not-found|-o|name' -Actual ($script:DbGroupKubectlCalls[0].Args -join '|') -Because '先只读探测 infra namespace(--ignore-not-found 区分「不存在」与「读失败」)'
    Assert-Equal -Expected 'contract-db-group-ctx' -Actual $script:DbGroupKubectlCalls[0].KubeContext -Because '探测同样显式带 -KubeContext'
    Assert-Equal -Expected (1 + $sampleZones.Count) -Actual $script:DbGroupKubectlCalls.Count -Because '一次探测 + 每个 zone 一次 ConfigMap'
    Assert-Match -Text $out -Pattern 'creates Kafka from scratch' -Because '要打一行说明这次按空 Kafka 判定'

    # 探测失败(退出码 1):读不到不等于没有,不按全新判定,也不再往下读。
    Reset-DbGroupFixture -Rules (@(New-DbGroupInfraNamespaceRule -ExitCode 1) + $configMapRules)
    $script:Command = 'all-up'
    $script:ZonesConfigPath = $DbGroupSampleZonesPath
    $message = ''
    try { $null = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern '读取 infra namespace contract-infra 失败' -Because '探测失败必须 throw(fail-closed)'
    Assert-Match -Text $message -Pattern '尚未做任何写操作' -Because '报错要说明还没有写集群'
    Assert-Equal -Expected 1 -Actual $script:DbGroupKubectlCalls.Count -Because '探测失败就停,不再读 ConfigMap、不 exec'

    # namespace 已在(例如上一次 Apply-Infra 半途失败、kafka-0 还没起来):照旧 exec,读不到由决策表拒绝,文案给出新集群的两步走法。
    Reset-DbGroupFixture -Rules (@((New-DbGroupInfraNamespaceRule -Exists), (New-DbGroupDescribeRule -Group $firstZ -State 'exec-failed')) + $configMapRules)
    $script:Command = 'all-up'
    $script:ZonesConfigPath = $DbGroupSampleZonesPath
    $message = ''
    try { $null = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern '读不到 Kafka 位点,无法判定 db 消费组归属' -Because 'infra namespace 在就不能假定 Kafka 是空的(PVC 可能还在)'
    Assert-Match -Text $message -Pattern 'infra-up -WaitReady,再跑 all-up -SkipInfra' -Because 'keep + 全新 namespace 读不到 Kafka 时,提示新集群先 infra-up -WaitReady 再 all-up -SkipInfra'
    Assert-True -Condition (@($script:DbGroupKubectlCalls | Where-Object { $_.Line -match '^exec ' }).Count -ge 1) -Because 'namespace 在时确实去 exec 了'

    # -SkipInfra 或 DryRun 都不探测(前者不建 Kafka,后者不读集群)。
    Reset-DbGroupFixture
    $script:Command = 'all-up'
    $script:ZonesConfigPath = $DbGroupSampleZonesPath
    $script:DryRun = $true
    $null = Initialize-DbConsumerGroupPlans 6>&1 3>&1 | Out-String
    Assert-Equal -Expected 0 -Actual $script:DbGroupKubectlCalls.Count -Because 'DryRun 的 all-up 一次集群都不读(含 infra namespace 探测)'
}

Test-Case '③ -DbConsumerGroup 只经 dev_tools.ps1::Invoke-K8sDeploy 透传(给了才传);k8s-image-* / k8s-release-* 与 k8s-zone-rollback 不透传,显式给了即拒绝' {
    $readFunction = {
        param([string]$RelativePath, [string]$FunctionName)
        $parseErrors = $null
        $ast = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) $RelativePath), [ref]$null, [ref]$parseErrors)
        if ($parseErrors.Count -gt 0) { throw "$RelativePath 解析失败:$($parseErrors[0].Message)" }
        $fnAst = $ast.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq $FunctionName }, $true)
        if ($null -eq $fnAst) { throw "$RelativePath 里找不到函数 $FunctionName" }
        return $fnAst.Extent.Text
    }
    $devDeploy = & $readFunction 'dev_tools.ps1' 'Invoke-K8sDeploy'
    Assert-Match -Text $devDeploy -Pattern 'if \(-not \[string\]::IsNullOrWhiteSpace\(\$DbConsumerGroup\)\)\s*\{\s*\$args\.DbConsumerGroup = \$DbConsumerGroup\s*\}' -Because 'dev_tools 的 k8s-zone-up 等入口留空不透传、给了才透传(回档 Step 6 经它传 zone)'
    Assert-NotMatch -Text (& $readFunction 'k8s_image.ps1' 'Invoke-K8sDeploy') -Pattern 'DbConsumerGroup' -Because 'k8s_image.ps1::Invoke-K8sDeploy 不透传 -DbConsumerGroup:release 流程等于 keep,切组只许显式的单 zone zone-up(原规格 F4)'
    $devImage = & $readFunction 'dev_tools.ps1' 'Invoke-K8sImage'
    Assert-NotMatch -Text $devImage -Pattern '\$args(\.|\[\s*[''"])DbConsumerGroup|DbConsumerGroup\s*=' -Because 'dev_tools.ps1::Invoke-K8sImage 不把 -DbConsumerGroup 转给 k8s_image.ps1(它不部署 db)'
    Assert-Match -Text $devImage -Pattern 'if \(-not \[string\]::IsNullOrWhiteSpace\(\$DbConsumerGroup\)\)\s*\{\s*\$notForwarded \+= ''-DbConsumerGroup''\s*\}' -Because 'dev_tools.ps1::Invoke-K8sImage 显式给了 -DbConsumerGroup 要拒绝,不静默吞掉(同 -LoginDevPasswordAuth 等)'

    # 行为:按 AST 取出 Invoke-K8sImage 的函数体直接调用(不起子进程,不碰 docker / 集群)。$ScriptDir 指向不存在的目录:
    # 守卫一旦失效,函数在「k8s_image.ps1 not found」处停下,也不会真的执行发布。
    $parseErrors = $null
    $devAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'dev_tools.ps1'), [ref]$null, [ref]$parseErrors)
    $imageBody = $devAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Invoke-K8sImage' }, $true).Body.GetScriptBlock()
    $ScriptDir = Join-Path ([System.IO.Path]::GetTempPath()) ('db-group-no-such-dir-' + [guid]::NewGuid().ToString('N'))
    $AgonesHighDensity = $false; $AgonesAutoscale = $false; $LoginDevPasswordAuth = $false
    $GatewayIngressHost = ''; $GatewayIngressClassName = ''; $GatewayIngressTlsSecret = ''; $GatewayTrustedProxies = ''
    foreach ($case in @(@{ Value = 'zone'; Rejected = $true }, @{ Value = 'keep'; Rejected = $true }, @{ Value = ''; Rejected = $false })) {
        $DbConsumerGroup = $case.Value
        $message = ''
        try { & $imageBody -ImageCommand 'release-zone' } catch { $message = $_.Exception.Message }
        if ($case.Rejected) {
            Assert-Match -Text $message -Pattern 'k8s-image-\* / k8s-release-\* 不支持 -DbConsumerGroup' -Because "k8s-release-zone 给 -DbConsumerGroup '$($case.Value)' 必须在任何副作用之前拒绝并点名参数(给 keep 也不生效:k8s_image 不部署 db)"
        }
        else {
            Assert-NotMatch -Text $message -Pattern 'DbConsumerGroup' -Because '留空不拒绝(函数走到找 k8s_image.ps1 那一步才停)'
        }
    }

    # k8s-zone-rollback:回档 Step 6 固定传 zone,外层给的 -DbConsumerGroup 不生效 —— 拒绝必须排在调用回档脚本之前。
    $rollbackClause = $null
    foreach ($switchAst in $devAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.SwitchStatementAst] }, $true)) {
        foreach ($clause in $switchAst.Clauses) {
            if ($clause.Item1 -is [System.Management.Automation.Language.StringConstantExpressionAst] -and $clause.Item1.Value -ceq 'k8s-zone-rollback') { $rollbackClause = $clause.Item2.Extent.Text }
        }
    }
    Assert-True -Condition ($null -ne $rollbackClause) -Because 'dev_tools.ps1 要有 k8s-zone-rollback 分支'
    $guardAt = $rollbackClause.IndexOf('if (-not [string]::IsNullOrWhiteSpace($DbConsumerGroup))')
    # 按调用表达式找(分支开头的注释里也提到 k8s_zone_rollback.ps1)。
    $invokeAt = $rollbackClause.IndexOf('(Join-Path $ScriptDir "k8s_zone_rollback.ps1")')
    Assert-True -Condition ($guardAt -ge 0 -and $invokeAt -gt $guardAt) -Because 'k8s-zone-rollback 分支在调用 k8s_zone_rollback.ps1 之前拒绝非空的 -DbConsumerGroup'
    Assert-Match -Text $rollbackClause.Substring([Math]::Max($guardAt, 0)) -Pattern '^if \(-not \[string\]::IsNullOrWhiteSpace\(\$DbConsumerGroup\)\)\s*\{\s*throw "k8s-zone-rollback 不支持 -DbConsumerGroup' -Because '拒绝文案点名参数'
}

Remove-Item -Path Function:Invoke-ClientEntryKubectl -ErrorAction SilentlyContinue
foreach ($dbGroupVar in @('InfraNamespace', 'NamespacePrefix', 'KubeContext', 'KubeConfig', 'GoSvcCatalogue', 'Command', 'ZoneName', 'ZoneId', 'SkipGoSvc', 'GoSvcRegistry', 'DryRun', 'DbConsumerGroup', 'DbConsumerGroupPlans', 'SkipInfra', 'ZonesConfigPath')) {
    Remove-Variable -Name $dbGroupVar -Scope Script -ErrorAction SilentlyContinue
}

# ─────────────────────────────────────────────────────────────────
# S5b 保留期与 Kafka 盘预算(规格:docs/handoff/godb-pipeline-impl-20261015/S5-dirty-snapshot-retention-spec.md
#    B-10 / B-11 / B-15 / B-16 / B-17;deploy/k8s/README.md「Kafka 磁盘预算与 db_task retention.bytes」)
# ─────────────────────────────────────────────────────────────────
# db_task 的 topic 级 retention.bytes 与每个 broker 的 PVC 在同一个公式里对账(k8s_deploy.ps1 的 Get-KafkaDiskPlan):
# 新集群按公式建盘,已有集群沿用线上 PVC、盘不够就按盘收缩 retention.bytes 并告警,连 2 个 segment 都放不下才 throw;
# 显式 -KafkaStorageGi 与线上不同即拒绝(volumeClaimTemplates 不可变)。生效值同时渲染进 db 与 login 两个 ConfigMap(B-15)。
# 纯函数与胶水函数按 AST 抽出来直接调用(与上文 Kafka 拓扑、③ 同一做法);读集群的唯一出口是 & kubectl,由本节的同名假函数顶替
# (函数先于外部命令解析),只在测 Get-LiveKafkaStorageGi / Get-LiveKafkaPvcMinGi / Get-LiveZoneNamespaceNames / Resolve-KafkaDiskPlan
# 的非 DryRun 路径时用到;假函数按「get pvc」「get namespace」与其余(StatefulSet)分三路回应。
# 缺省值表(负责人 2026-10-13 拍板:prod 2h / 3 MiB/s,prod-like 2h / 1 MiB/s,dev 1h / 256 KiB/s;其它预留 4 / 2 / 1 GiB)
# 以 db_task 10 个分区、审计 6 + 3 个分区、zones.json 两个 zone 为前提,下面第一条用例先核对这个前提。
$S5bFunctionNames = @(
    'Get-KafkaDiskBudgetConstants', 'Get-KafkaDbTaskRetentionBytesTarget', 'Get-KafkaDiskPlan', 'Get-KafkaStorageChangeVerdict',
    'Get-LiveKafkaStorageGi', 'Get-LiveKafkaPvcMinGi', 'Get-LiveZoneNamespaceNames', 'Resolve-KafkaDiskPlan', 'Write-KafkaDiskLedger', 'Assert-KafkaStorageChangeIsSafe',
    # Resolve-KafkaDiskPlan 的依赖(与 k8s_deploy.ps1 里同一份函数体)。
    'Build-KubectlBaseArgs', 'Get-KafkaTopologyFor', 'Get-KafkaTopology', 'Get-AuthoritativeScalar', 'Get-ZonesFromJson', 'Resolve-ZonesConfigPath'
)
foreach ($s5bFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($S5bFunctionNames -contains $s5bFnAst.Name) {
        Set-Item -Path "Function:script:$($s5bFnAst.Name)" -Value $s5bFnAst.Body.GetScriptBlock()
    }
}
foreach ($s5bFnName in $S5bFunctionNames) {
    if (-not (Test-Path "Function:$s5bFnName")) { throw "k8s_deploy.ps1 缺少函数 ${s5bFnName}(Kafka 盘预算被删了?后续断言无意义)" }
}

$S5bGiB = [decimal]1073741824
$S5bDbTaskPartitions = [int](Get-EtcValue -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.PartitionCnt')
$S5bAuditPartitions = [int](Get-EtcValue -RelativePath 'go/data_service/etc/data_service.yaml' -KeyPath 'Kafka.TransactionLogPartitions') +
    [int](Get-EtcValue -RelativePath 'go/data_service/etc/data_service.yaml' -KeyPath 'Kafka.SnapshotPartitions')
# 三个档位的缺省输入(与 Apply-KafkaProfileDefaults 同值;改档位缺省值时这张表、README 与本节的期望值一起改)。
$S5bProfiles = @{
    'prod'      = @{ DesignBytesPerSec = 3145728; BufferTargetMs = 7200000; RetentionMs = 86400000; SegmentBytes = 33554432; BrokerRetentionBytes = 536870912; ReserveGi = 4 }
    'prod-like' = @{ DesignBytesPerSec = 1048576; BufferTargetMs = 7200000; RetentionMs = 21600000; SegmentBytes = 33554432; BrokerRetentionBytes = 536870912; ReserveGi = 2 }
    'dev'       = @{ DesignBytesPerSec = 262144;  BufferTargetMs = 3600000; RetentionMs = 3600000;  SegmentBytes = 16777216; BrokerRetentionBytes = 134217728; ReserveGi = 1 }
}

# 按档位缺省值调 Get-KafkaDiskPlan;-Overrides 覆盖任意一个参数(例如 LiveStorageGi / ExplicitStorageGi / ZoneCount)。
function New-S5bDiskPlan {
    param(
        # 不叫 -Profile:$PROFILE 是自动变量。
        [ValidateSet('prod', 'prod-like', 'dev')][string]$Tier = 'prod',
        [hashtable]$Overrides = @{}
    )
    $d = $S5bProfiles[$Tier]
    $target = Get-KafkaDbTaskRetentionBytesTarget -DesignBytesPerSec $d.DesignBytesPerSec -BufferTargetMs $d.BufferTargetMs `
        -RetentionMs $d.RetentionMs -Partitions $S5bDbTaskPartitions -SegmentBytes $d.SegmentBytes
    $p = @{
        Brokers = 3; ReplicationFactor = 3; ZoneCount = 2; DbTaskPartitions = $S5bDbTaskPartitions
        DbTaskRetentionBytesTarget = $target; SegmentBytes = $d.SegmentBytes; BrokerRetentionBytes = $d.BrokerRetentionBytes
        AuditPartitions = $S5bAuditPartitions; OtherTopicsReserveGi = $d.ReserveGi; LiveStorageGi = $null; LivePvcGi = $null; ExplicitStorageGi = 0
        DesignBytesPerSec = $d.DesignBytesPerSec; DbTaskRetentionMs = $d.RetentionMs
    }
    foreach ($k in $Overrides.Keys) { $p[$k] = $Overrides[$k] }
    return Get-KafkaDiskPlan @p
}

# 规格的保证(B-17):生效的 retention.bytes 下,1.3 × Σ(各 topic 每分区上限 + segment)≤ 实际 PVC。
# 每个 broker 按「持有每个分区各一份副本」的最坏情形算(3 → N 扩容后、reassign 之前老 broker 就是这样),不乘 RF ÷ broker 数。
function Assert-S5bPlanFitsDisk {
    param([Parameter(Mandatory = $true)]$Plan, [Parameter(Mandatory = $true)][string]$Label)
    $seg = [decimal]$Plan.SegmentBytes
    $used = [decimal]1.3 * (
        [decimal]$Plan.ZoneCount * [decimal]$Plan.DbTaskPartitions * ([decimal]$Plan.DbTaskRetentionBytes + $seg) +
        [decimal]$Plan.AuditPartitions * ([decimal]$Plan.BrokerRetentionBytes + $seg) +
        [decimal]$Plan.OtherTopicsReserveGi * $S5bGiB)
    Assert-True -Condition ($used -le [decimal]$Plan.EffectiveGi * $S5bGiB) -Because "${Label}:按生效的 retention.bytes=$($Plan.DbTaskRetentionBytes) 算,最坏的 broker 要 $used 字节(含 1.3 余量),超过了 $($Plan.EffectiveGi)Gi 的盘 —— 收缩没有把 Σ 压到 PVC ÷ 1.3 以内"
}

# DryRun 输出里的账本行(到紧随其后的 note 行为止;Write-Host 折行时 token 里没有空格,仍能断言)。
function Get-S5bLedgerText {
    param([Parameter(Mandatory = $true)][string]$Output)
    $m = [regex]::Match($Output, '(?s)Kafka disk ledger: brokers=.*?(?=Kafka disk ledger note:)')
    if (-not $m.Success) { throw "DryRun 输出里没有「Kafka disk ledger: brokers=…」一行(写路径入口没打盘预算?)" }
    return $m.Value
}

# 读集群出口的假 kubectl:「get pvc」回应 $script:S5bKubectlPvcReply,「get namespace」回应 $script:S5bKubectlNamespaceReply,
# 其余(StatefulSet 模板)回应 $script:S5bKubectlReply;记下每次调用的参数。
$script:S5bKubectlReply = @{ ExitCode = 0; Stdout = '' }
$script:S5bKubectlPvcReply = @{ ExitCode = 0; Stdout = '' }
$script:S5bKubectlNamespaceReply = @{ ExitCode = 0; Stdout = '' }
$script:S5bKubectlCalls = [System.Collections.Generic.List[string]]::new()
function kubectl {
    $joined = ($args -join ' ')
    $script:S5bKubectlCalls.Add($joined)
    $reply = $script:S5bKubectlReply
    if ($joined -match '(^| )get pvc( |$)') { $reply = $script:S5bKubectlPvcReply }
    elseif ($joined -match '(^| )get namespace( |$)') { $reply = $script:S5bKubectlNamespaceReply }
    $global:LASTEXITCODE = [int]$reply.ExitCode
    return [string]$reply.Stdout
}

# 被抽函数读的脚本级变量一次给全(取值 = prod 档缺省、dev 发布档单 broker、zones.json 两个 zone、DryRun)。
# 三路假 kubectl 缺省都是「成功、无输出」:没有 StatefulSet、没有 PVC、没有 zone namespace。
function Reset-S5bFixture {
    param(
        [hashtable]$Reply = @{ ExitCode = 0; Stdout = '' },
        [hashtable]$PvcReply = @{ ExitCode = 0; Stdout = '' },
        [hashtable]$NamespaceReply = @{ ExitCode = 0; Stdout = '' }
    )
    $script:S5bKubectlReply = $Reply
    $script:S5bKubectlPvcReply = $PvcReply
    $script:S5bKubectlNamespaceReply = $NamespaceReply
    $script:S5bKubectlCalls.Clear()
    $script:NamespacePrefix = 'contract-zone'
    $script:ZoneName = 'yesterday'
    $script:RepoRoot = Get-RepoRoot
    $script:KafkaBrokers = 0
    $script:ReleaseProfile = 'dev'
    $script:OpsProfile = 'custom'
    $script:InfraNamespace = 'contract-infra'
    $script:KubeContext = ''
    $script:KubeConfig = ''
    $script:ZoneId = 101
    $script:ZonesConfigPath = Join-Path (Get-RepoRoot) 'deploy/k8s/zones.json'
    $script:DryRun = $true
    $script:KafkaSegmentBytes = [long]33554432
    $script:KafkaRetentionBytes = [long]536870912
    $script:KafkaDbTaskRetentionMs = 86400000
    $script:KafkaDbTaskDesignBytesPerSec = [long]3145728
    $script:KafkaDbTaskBufferTargetMs = [long]7200000
    $script:KafkaOtherTopicsReserveGi = 4
    $script:KafkaStorageGi = 0
    $script:KafkaDbTaskRetentionBytes = [long]0
    $script:KafkaDiskPlan = $null
}

Test-Case 'S5b 夹具前提:db_task 10 个分区、审计 6 + 3 个分区、zones.json 恰好两个 zone(缺省值表按这组数算;变了就重算 README 与本节期望值)' {
    Assert-Equal -Expected 10 -Actual $S5bDbTaskPartitions -Because 'go/db/etc/db.yaml 的 ServerConfig.Kafka.PartitionCnt'
    Assert-Equal -Expected 9 -Actual $S5bAuditPartitions -Because 'data_service.yaml 的 TransactionLogPartitions + SnapshotPartitions'
    $zoneIds = @((Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/zones.json') -Raw | ConvertFrom-Json).zones | ForEach-Object { [int]$_.zoneId } | Sort-Object -Unique)
    Assert-Equal -Expected '101,102' -Actual ($zoneIds -join ',') -Because 'DryRun 用例的 zone 数 = zones.json ∪ -ZoneId 101 = 2'
}

Test-Case 'S5b 目标 retention.bytes(Get-KafkaDbTaskRetentionBytesTarget):三个档位的缺省值,T 取 min(缓冲目标, retention.ms),非正输入即 throw' {
    foreach ($case in @(
        @{ Profile = 'prod'; Want = 2785017856 }
        @{ Profile = 'prod-like'; Want = 973078528 }
        @{ Profile = 'dev'; Want = 150994944 }
    )) {
        $d = $S5bProfiles[$case.Profile]
        $got = Get-KafkaDbTaskRetentionBytesTarget -DesignBytesPerSec $d.DesignBytesPerSec -BufferTargetMs $d.BufferTargetMs -RetentionMs $d.RetentionMs -Partitions $S5bDbTaskPartitions -SegmentBytes $d.SegmentBytes
        Assert-Equal -Expected $case.Want -Actual $got -Because "$($case.Profile) 档:roundUp(R × T × 1.2 ÷ P, 64MiB) + segment(负责人拍板的缺省值表)"
        Assert-Equal -Expected 0 -Actual (([long]$got - $d.SegmentBytes) % 67108864) -Because "$($case.Profile) 档:去掉一个 segment 后是 64MiB 的整数倍"
    }
    $dev = $S5bProfiles['dev']
    $longBuffer = Get-KafkaDbTaskRetentionBytesTarget -DesignBytesPerSec $dev.DesignBytesPerSec -BufferTargetMs 7200000 -RetentionMs 3600000 -Partitions $S5bDbTaskPartitions -SegmentBytes $dev.SegmentBytes
    Assert-Equal -Expected 150994944 -Actual $longBuffer -Because '缓冲目标 2h 但 retention.ms 只有 1h:按 1h 算(时间先到先删,多留只占盘)'
    foreach ($bad in @(@{ Partitions = 0; BufferTargetMs = 3600000 }, @{ Partitions = 10; BufferTargetMs = 0 })) {
        $thrown = ''
        try { $null = Get-KafkaDbTaskRetentionBytesTarget -DesignBytesPerSec 262144 -BufferTargetMs $bad.BufferTargetMs -RetentionMs 3600000 -Partitions $bad.Partitions -SegmentBytes 16777216 } catch { $thrown = $_.Exception.Message }
        Assert-Match -Text $thrown -Pattern '必须全部为正' -Because "partitions=$($bad.Partitions) buffer=$($bad.BufferTargetMs):不替你猜一个保留量"
    }
}

Test-Case 'S5b 盘预算(Get-KafkaDiskPlan):新集群按公式建盘 —— prod 3 broker / RF3 与 1 broker / RF1 都是 80Gi,prod-like 40Gi,dev 取下限 20Gi;不收缩' {
    foreach ($case in @(
        @{ Label = 'prod 3 broker RF3'; Profile = 'prod'; Overrides = @{}; Gi = 80; Target = 2785017856 }
        @{ Label = 'prod 1 broker RF1'; Profile = 'prod'; Overrides = @{ Brokers = 1; ReplicationFactor = 1 }; Gi = 80; Target = 2785017856 }
        @{ Label = 'prod-like'; Profile = 'prod-like'; Overrides = @{}; Gi = 40; Target = 973078528 }
        @{ Label = 'dev'; Profile = 'dev'; Overrides = @{ Brokers = 1; ReplicationFactor = 1 }; Gi = 20; Target = 150994944 }
    )) {
        $plan = New-S5bDiskPlan -Tier $case.Profile -Overrides $case.Overrides
        Assert-Equal -Expected $case.Target -Actual $plan.TargetRetentionBytes -Because "$($case.Label):目标值"
        Assert-Equal -Expected $case.Gi -Actual $plan.RequiredGi -Because "$($case.Label):1.3 × RF × Σ ÷ broker 数,取到 10Gi、下限 20Gi"
        Assert-Equal -Expected 'computed' -Actual $plan.Source -Because "$($case.Label):线上没有、也没显式给"
        Assert-Equal -Expected $case.Gi -Actual $plan.EffectiveGi -Because "$($case.Label):新集群直接按公式建盘"
        Assert-Equal -Expected $case.Target -Actual $plan.DbTaskRetentionBytes -Because "$($case.Label):按公式建出来的盘装得下目标值,不得收缩(哪怕一个 64MiB 粒度)"
        Assert-True -Condition (-not $plan.FitLimited) -Because "$($case.Label):FitLimited 为假"
        Assert-S5bPlanFitsDisk -Plan $plan -Label $case.Label
    }
    $prod = New-S5bDiskPlan -Tier 'prod'
    Assert-True -Condition ([double]$prod.ImpliedBufferSec -ge 7200) -Because "prod 新集群按设计速率至少缓冲 2 小时(实际 $($prod.ImpliedBufferSec)s)"
}

Test-Case 'S5b 盘预算:已有集群沿用线上 PVC,盘不够就按盘收缩并标 FitLimited —— prod、线上 20Gi、2 个 zone → 268435456,约 622s;显式更大的目标同样收缩' {
    $plan = New-S5bDiskPlan -Tier 'prod' -Overrides @{ LiveStorageGi = 20 }
    Assert-Equal -Expected 20 -Actual $plan.EffectiveGi -Because '自动模式沿用线上大小(volumeClaimTemplates 不可变,渲染值 = 线上值才不会 apply 冲突)'
    Assert-Equal -Expected 'live' -Actual $plan.Source -Because 'pvc_source=live'
    Assert-Equal -Expected 80 -Actual $plan.RequiredGi -Because '需求照算,FitLimited 告警里给出要扩到多大'
    Assert-Equal -Expected 2785017856 -Actual $plan.TargetRetentionBytes -Because '目标值不变'
    Assert-Equal -Expected 268435456 -Actual $plan.DbTaskRetentionBytes -Because '按盘收缩到 floor(capacity, 64MiB)(B-17:不会写爆盘)'
    Assert-True -Condition $plan.FitLimited -Because 'FitLimited 为真'
    Assert-True -Condition ([Math]::Abs([double]$plan.ImpliedBufferSec - 622) -le 1) -Because "按设计速率约 622s 的缓冲(实际 $($plan.ImpliedBufferSec))"
    Assert-S5bPlanFitsDisk -Plan $plan -Label 'prod 线上 20Gi'

    $explicitTarget = New-S5bDiskPlan -Tier 'prod' -Overrides @{ LiveStorageGi = 20; DbTaskRetentionBytesTarget = [long]10770972672 }
    Assert-Equal -Expected 10770972672 -Actual $explicitTarget.TargetRetentionBytes -Because '显式的 -KafkaDbTaskRetentionBytes 只是目标'
    Assert-Equal -Expected 268435456 -Actual $explicitTarget.DbTaskRetentionBytes -Because '显式目标大于可容纳值时同样按盘收缩'
    Assert-True -Condition $explicitTarget.FitLimited -Because '显式目标放不下同样标 FitLimited'
    Assert-S5bPlanFitsDisk -Plan $explicitTarget -Label '显式目标 + 线上 20Gi'

    $explicitDisk = New-S5bDiskPlan -Tier 'prod' -Overrides @{ ExplicitStorageGi = 40 }
    Assert-Equal -Expected 'explicit/40' -Actual ('{0}/{1}' -f $explicitDisk.Source, $explicitDisk.EffectiveGi) -Because '显式 -KafkaStorageGi 优先于公式(线上没有时)'
    Assert-Equal -Expected 1140850688 -Actual $explicitDisk.DbTaskRetentionBytes -Because '显式的盘小于需求:按 40Gi 收缩'
    Assert-True -Condition $explicitDisk.FitLimited -Because '显式的盘装不下目标值同样标 FitLimited'
    Assert-S5bPlanFitsDisk -Plan $explicitDisk -Label '显式 40Gi'
}

Test-Case 'S5b 盘预算(R5a 复核):每个 broker 按「每个分区各一份副本」取上界,5 broker / RF3 / 线上 20Gi 与 3 broker 同为 268435456;线上模板与最小实际 PVC 取小,StatefulSet 不在时按 PVC(live-pvc)' {
    # [1] 3 → N 扩容是放行的,kafka-reassign-partitions 之前老的 3 个 broker 仍持有全部 db_task 与审计分区:
    #     按 RF ÷ broker 数均摊会算成 805306368,老 broker 上约 20×(805306368+33554432)+9428795392 ≈ 26.2GB,超过 20Gi。
    $three = New-S5bDiskPlan -Tier 'prod' -Overrides @{ LiveStorageGi = 20 }
    $five = New-S5bDiskPlan -Tier 'prod' -Overrides @{ Brokers = 5; ReplicationFactor = 3; LiveStorageGi = 20 }
    Assert-Equal -Expected 268435456 -Actual $five.DbTaskRetentionBytes -Because '5 broker、RF 3、线上 20Gi:不按 RF ÷ broker 数均摊(否则是 805306368,写爆还没 reassign 的老 broker)'
    Assert-Equal -Expected $three.DbTaskRetentionBytes -Actual $five.DbTaskRetentionBytes -Because '5 broker 与 3 broker 的生效值相同'
    Assert-Equal -Expected $three.RequiredGi -Actual $five.RequiredGi -Because 'RequiredGi 同样不随 broker 数变小'
    Assert-Equal -Expected '5/3' -Actual ('{0}/{1}' -f $five.Brokers, $five.ReplicationFactor) -Because 'broker 数与 RF 仍原样带回,账本行照打'
    Assert-S5bPlanFitsDisk -Plan $five -Label '5 broker 线上 20Gi'
    Assert-Equal -Expected 80 -Actual (New-S5bDiskPlan -Tier 'prod' -Overrides @{ Brokers = 5; ReplicationFactor = 3 }).RequiredGi -Because '新集群 5 broker 同样按上界建 80Gi'

    # [0] 线上盘 = min(StatefulSet 模板, 最小的实际 PVC);StatefulSet 不在、PVC 在时按 PVC,不按公式值。
    $pvcOnly = New-S5bDiskPlan -Tier 'prod' -Overrides @{ LivePvcGi = 20 }
    Assert-Equal -Expected 'live-pvc/20/20/268435456' -Actual ('{0}/{1}/{2}/{3}' -f $pvcOnly.Source, $pvcOnly.EffectiveGi, $pvcOnly.StorageTemplateGi, $pvcOnly.DbTaskRetentionBytes) -Because '--cascade=orphan 之后 StatefulSet 不在:按最小的实际 PVC 算并以它为模板,不按公式值 80Gi(B-17)'
    Assert-S5bPlanFitsDisk -Plan $pvcOnly -Label 'StatefulSet 不在、PVC 20Gi'
    $templateBigger = New-S5bDiskPlan -Tier 'prod' -Overrides @{ LiveStorageGi = 80; LivePvcGi = 20 }
    Assert-Equal -Expected 'live/20/80/268435456' -Actual ('{0}/{1}/{2}/{3}' -f $templateBigger.Source, $templateBigger.EffectiveGi, $templateBigger.StorageTemplateGi, $templateBigger.DbTaskRetentionBytes) -Because '模板 80Gi、有一块 PVC 只有 20Gi:retention.bytes 按 20Gi 算,kafka.yaml 照旧渲染线上模板 80Gi(不可变字段,不同即 apply 被拒)'
    Assert-S5bPlanFitsDisk -Plan $templateBigger -Label '模板 80Gi、实际 20Gi'
    $pvcBigger = New-S5bDiskPlan -Tier 'prod' -Overrides @{ LiveStorageGi = 20; LivePvcGi = 80 }
    Assert-Equal -Expected 'live/20/20/268435456' -Actual ('{0}/{1}/{2}/{3}' -f $pvcBigger.Source, $pvcBigger.EffectiveGi, $pvcBigger.StorageTemplateGi, $pvcBigger.DbTaskRetentionBytes) -Because '只 patch 了 PVC、没重建 StatefulSet:按旧模板(更小的)算,方向保守'
    $explicitWithPvc = New-S5bDiskPlan -Tier 'prod' -Overrides @{ LivePvcGi = 80; ExplicitStorageGi = 80 }
    Assert-Equal -Expected 'explicit/80/80' -Actual ('{0}/{1}/{2}' -f $explicitWithPvc.Source, $explicitWithPvc.EffectiveGi, $explicitWithPvc.StorageTemplateGi) -Because '显式值优先(与线上是否相符由 Assert-KafkaStorageChangeIsSafe 判)'
    $thrown = ''
    try { $null = New-S5bDiskPlan -Tier 'prod' -Overrides @{ LivePvcGi = 0 } } catch { $thrown = $_.Exception.Message }
    Assert-Match -Text $thrown -Pattern 'fail-closed' -Because '最小实际 PVC 为 0:不替你猜一块盘'
}

Test-Case 'S5b 盘预算:连 2 个 segment 都放不下即 throw(线上 5Gi、prod、3 个 zone),错误写明「放不下」与所需的 Gi;非法输入一律 throw' {
    $thrown = ''
    try { $null = New-S5bDiskPlan -Tier 'prod' -Overrides @{ LiveStorageGi = 5; ZoneCount = 3 } } catch { $thrown = $_.Exception.Message }
    Assert-Match -Text $thrown -Pattern '放不下' -Because '每分区缓冲约等于零时不能只告警'
    Assert-Match -Text $thrown -Pattern '-KafkaStorageGi 120' -Because '错误里给出按目标值所需的盘(3 个 zone 需要 120Gi)与扩盘后的重跑参数'
    foreach ($bad in @(
        @{ Label = 'RF 大于 broker 数'; Overrides = @{ Brokers = 1; ReplicationFactor = 3 } }
        @{ Label = 'zone 数为 0'; Overrides = @{ ZoneCount = 0 } }
        @{ Label = '线上 0Gi'; Overrides = @{ LiveStorageGi = 0 } }
        @{ Label = '目标值为 0'; Overrides = @{ DbTaskRetentionBytesTarget = [long]0 } }
    )) {
        $thrown = ''
        try { $null = New-S5bDiskPlan -Tier 'prod' -Overrides $bad.Overrides } catch { $thrown = $_.Exception.Message }
        Assert-Match -Text $thrown -Pattern 'fail-closed' -Because "$($bad.Label):不替你猜一块盘"
    }
}

Test-Case 'S5b PVC 变更判定(Get-KafkaStorageChangeVerdict):absent / auto / unchanged 放行,扩、缩与「显式值大于最小的实际 PVC」一律拒绝' {
    foreach ($case in @(
        @{ Live = $null; Pvc = $null; Explicit = 0; Verdict = 'absent' }
        @{ Live = $null; Pvc = $null; Explicit = 80; Verdict = 'absent' }
        @{ Live = 20; Pvc = $null; Explicit = 0; Verdict = 'auto' }
        @{ Live = 20; Pvc = $null; Explicit = 20; Verdict = 'unchanged' }
        @{ Live = 20; Pvc = $null; Explicit = 80; Verdict = 'expand-refused' }
        @{ Live = 80; Pvc = $null; Explicit = 20; Verdict = 'shrink-refused' }
        # --cascade=orphan 之后:StatefulSet 不在、PVC 在,判定不再是 absent(R5a 复核 [0])。
        @{ Live = $null; Pvc = 20; Explicit = 0; Verdict = 'auto' }
        @{ Live = $null; Pvc = 20; Explicit = 20; Verdict = 'unchanged' }
        @{ Live = $null; Pvc = 20; Explicit = 80; Verdict = 'pvc-not-expanded' }
        @{ Live = $null; Pvc = 80; Explicit = 40; Verdict = 'shrink-refused' }
        # StatefulSet 在:先按模板判(不等即 apply 被拒),再按最小的实际 PVC 判。
        @{ Live = 20; Pvc = 80; Explicit = 20; Verdict = 'unchanged' }
        @{ Live = 20; Pvc = 80; Explicit = 80; Verdict = 'expand-refused' }
        @{ Live = 80; Pvc = 20; Explicit = 80; Verdict = 'pvc-not-expanded' }
        @{ Live = 80; Pvc = 20; Explicit = 0; Verdict = 'auto' }
    )) {
        $liveText = if ($null -eq $case.Live) { '<不存在>' } else { "$($case.Live)Gi" }
        $pvcText = if ($null -eq $case.Pvc) { '<没有>' } else { "$($case.Pvc)Gi" }
        Assert-Equal -Expected $case.Verdict -Actual (Get-KafkaStorageChangeVerdict -LiveStorageGi $case.Live -LivePvcGi $case.Pvc -ExplicitStorageGi $case.Explicit) -Because "模板 $liveText、最小 PVC $pvcText、显式 $($case.Explicit):volumeClaimTemplates 不可变、PVC 不能缩、不按不存在的盘算"
    }
}

Test-Case 'S5b Get-LiveKafkaStorageGi(假 kubectl):DryRun 不查;不存在返回 $null;只认 kafka:<整数>Gi,别的单位、没有大小、kubectl 失败一律 throw' {
    Reset-S5bFixture
    Assert-True -Condition ($null -eq (Get-LiveKafkaStorageGi)) -Because 'DryRun 一律当线上没有'
    Assert-Equal -Expected 0 -Actual $script:S5bKubectlCalls.Count -Because 'DryRun 一次集群都不读'

    Reset-S5bFixture -Reply @{ ExitCode = 0; Stdout = 'kafka:20Gi' }
    $script:DryRun = $false
    Assert-Equal -Expected 20 -Actual (Get-LiveKafkaStorageGi) -Because '读出线上 volumeClaimTemplates 的大小'
    Assert-Match -Text ([string]$script:S5bKubectlCalls[0]) -Pattern '^get statefulset kafka -n contract-infra --ignore-not-found -o jsonpath=\{\.metadata\.name\}:\{\.spec\.volumeClaimTemplates\[0\]\.spec\.resources\.requests\.storage\}$' -Because '名字与大小一起取,两种「空」分得开'

    Reset-S5bFixture -Reply @{ ExitCode = 0; Stdout = '' }
    $script:DryRun = $false
    Assert-True -Condition ($null -eq (Get-LiveKafkaStorageGi)) -Because 'StatefulSet 不存在(--ignore-not-found 无输出)返回 $null'

    foreach ($bad in @(
        @{ Label = '别的单位'; Reply = @{ ExitCode = 0; Stdout = 'kafka:1Ti' }; Pattern = '只认 Gi' }
        @{ Label = '没有大小'; Reply = @{ ExitCode = 0; Stdout = 'kafka:' }; Pattern = '只认 Gi' }
        @{ Label = 'kubectl 失败'; Reply = @{ ExitCode = 1; Stdout = '' }; Pattern = 'fail-closed' }
    )) {
        Reset-S5bFixture -Reply $bad.Reply
        $script:DryRun = $false
        $thrown = ''
        try { $null = Get-LiveKafkaStorageGi } catch { $thrown = $_.Exception.Message }
        Assert-Match -Text $thrown -Pattern $bad.Pattern -Because "$($bad.Label):不知道线上盘多大,就算不出不会写爆盘的 retention.bytes"
    }
}

Test-Case 'S5b Get-LiveKafkaPvcMinGi(假 kubectl):DryRun 不查;只数 kafka-data-kafka-<序号>、取最小的实际容量;没绑定的取 requests;别的单位、列数不对、kubectl 失败一律 throw' {
    Reset-S5bFixture
    Assert-True -Condition ($null -eq (Get-LiveKafkaPvcMinGi)) -Because 'DryRun 一律当没有'
    Assert-Equal -Expected 0 -Actual $script:S5bKubectlCalls.Count -Because 'DryRun 一次集群都不读'

    # kafka-data-kafka-1 停在扩容途中(requests 已是 80Gi、实际仍是 20Gi);别的 PVC 与名字不完全匹配的不算。
    Reset-S5bFixture -PvcReply @{ ExitCode = 0; Stdout = "kafka-data-kafka-0   80Gi   80Gi`nkafka-data-kafka-1   20Gi   80Gi`nkafka-data-kafka-2   80Gi   80Gi`nmysql-data   5Gi   5Gi`nkafka-data-kafka-old   1Gi   1Gi" }
    $script:DryRun = $false
    Assert-Equal -Expected 20 -Actual (Get-LiveKafkaPvcMinGi) -Because '取最小的 status.capacity,不读 requests(patch 之后立刻就变,扩容没做完时比实际盘大)'
    Assert-Match -Text ([string]$script:S5bKubectlCalls[0]) -Pattern '^get pvc -n contract-infra --no-headers -o custom-columns=NAME:\.metadata\.name,CAPACITY:\.status\.capacity\.storage,REQUEST:\.spec\.resources\.requests\.storage$' -Because '名字、实际容量、申请量一起取'

    Reset-S5bFixture -PvcReply @{ ExitCode = 0; Stdout = "kafka-data-kafka-0   20Gi   20Gi`nkafka-data-kafka-3   <none>   80Gi" }
    $script:DryRun = $false
    Assert-Equal -Expected 20 -Actual (Get-LiveKafkaPvcMinGi) -Because '还没绑定的 PVC 取它的 requests(绑定时至少这么大、盘上还没数据),不拦下部署'

    Reset-S5bFixture -PvcReply @{ ExitCode = 0; Stdout = '' }
    $script:DryRun = $false
    Assert-True -Condition ($null -eq (Get-LiveKafkaPvcMinGi)) -Because '一块都没有返回 $null'

    foreach ($bad in @(
        @{ Label = '别的单位'; Reply = @{ ExitCode = 0; Stdout = 'kafka-data-kafka-0   20G   20Gi' }; Pattern = '只认 Gi' }
        @{ Label = '列数不对'; Reply = @{ ExitCode = 0; Stdout = 'kafka-data-kafka-0   20Gi' }; Pattern = '三列' }
        @{ Label = 'kubectl 失败'; Reply = @{ ExitCode = 1; Stdout = '' }; Pattern = 'fail-closed' }
    )) {
        Reset-S5bFixture -PvcReply $bad.Reply
        $script:DryRun = $false
        $thrown = ''
        try { $null = Get-LiveKafkaPvcMinGi } catch { $thrown = $_.Exception.Message }
        Assert-Match -Text $thrown -Pattern $bad.Pattern -Because "$($bad.Label):不知道每块盘实际多大,就算不出不会写爆盘的 retention.bytes"
    }
}

Test-Case 'S5b Get-LiveZoneNamespaceNames(假 kubectl):DryRun 返回 $null;只认「前缀-」开头的 namespace、去掉前缀、排除 infra;空列表返回空数组;kubectl 失败 throw' {
    Reset-S5bFixture
    Assert-True -Condition ($null -eq (Get-LiveZoneNamespaceNames)) -Because 'DryRun 不读,返回 $null(账本行印 unread)'
    Assert-Equal -Expected 0 -Actual $script:S5bKubectlCalls.Count -Because 'DryRun 一次集群都不读'

    Reset-S5bFixture -NamespaceReply @{ ExitCode = 0; Stdout = "default`nkube-system`ncontract-infra`ncontract-zone-yesterday`ncontract-zone-today`ncontract-zone-z103`ncontract-zonex`ncontract-zone-" }
    $script:DryRun = $false
    $names = Get-LiveZoneNamespaceNames
    Assert-Equal -Expected 'today,yesterday,z103' -Actual ((@($names) | Sort-Object) -join ',') -Because '只要「contract-zone-」开头且前缀后非空的 namespace'
    Assert-Match -Text ([string]$script:S5bKubectlCalls[0]) -Pattern '^get namespace --no-headers -o custom-columns=NAME:\.metadata\.name$' -Because '列 namespace 名'

    Reset-S5bFixture -NamespaceReply @{ ExitCode = 0; Stdout = "contract-zone-infra`ncontract-zone-yesterday" }
    $script:DryRun = $false
    $script:InfraNamespace = 'contract-zone-infra'
    $infraFilteredNames = Get-LiveZoneNamespaceNames
    Assert-Equal -Expected 'yesterday' -Actual (@($infraFilteredNames) -join ',') -Because 'infra namespace 恰好带同一前缀时不算 zone'

    Reset-S5bFixture -NamespaceReply @{ ExitCode = 0; Stdout = "default`nkube-system" }
    $script:DryRun = $false
    $empty = Get-LiveZoneNamespaceNames
    Assert-True -Condition ($null -ne $empty -and @($empty).Count -eq 0) -Because '读了但没有 zone namespace:空数组,不是 $null(否则账本行误印 unread)'

    Reset-S5bFixture -NamespaceReply @{ ExitCode = 1; Stdout = '' }
    $script:DryRun = $false
    $thrown = ''
    try { $null = Get-LiveZoneNamespaceNames } catch { $thrown = $_.Exception.Message }
    Assert-Match -Text $thrown -Pattern 'fail-closed' -Because '读不到不等于没有'
}

Test-Case 'S5b Resolve-KafkaDiskPlan:按 prod 档与 zones.json 解出 80Gi / 2785017856 并缓存;线上 20Gi 时按盘收缩;-KafkaDbTaskRetentionBytes 负数或不到 2 个 segment 即拒绝' {
    Reset-S5bFixture
    $plan = Resolve-KafkaDiskPlan
    Assert-Equal -Expected '1/1/2' -Actual ('{0}/{1}/{2}' -f $plan.Brokers, $plan.ReplicationFactor, $plan.ZoneCount) -Because 'dev 发布档单 broker;zone = zones.json ∪ -ZoneId'
    Assert-Equal -Expected 'computed/80/2785017856' -Actual ('{0}/{1}/{2}' -f $plan.Source, $plan.EffectiveGi, $plan.DbTaskRetentionBytes) -Because 'DryRun 按公式建盘、不收缩'
    Assert-Equal -Expected $S5bAuditPartitions -Actual $plan.AuditPartitions -Because '审计分区数取自 data_service.yaml'
    Assert-True -Condition (-not $plan.ZonesConfigMissing) -Because 'zones 配置在场'
    Assert-True -Condition ([object]::ReferenceEquals($plan, $script:KafkaDiskPlan)) -Because '结果缓存进 $script:KafkaDiskPlan,kafka.yaml 与两个 ConfigMap 读同一份'

    Reset-S5bFixture
    $script:ZonesConfigPath = Join-Path ([System.IO.Path]::GetTempPath()) ('s5b-zones-missing-' + [guid]::NewGuid().ToString('N') + '.json')
    $missing = Resolve-KafkaDiskPlan 3>&1 | Where-Object { $_ -isnot [System.Management.Automation.WarningRecord] }
    Assert-Equal -Expected 1 -Actual @($missing)[0].ZoneCount -Because 'zones 配置缺席时只数 -ZoneId'
    Assert-True -Condition @($missing)[0].ZonesConfigMissing -Because '缺席要标出来(账本行尾 zones_config=missing)'
    # R5a 复核 [2]:缺席不能只在行尾标一个 token,要告警(dev 档);告警措辞不得撞上 Get-InfraZoneIds 那条讲预建库的 WARNING。
    $missingLines = @(Write-KafkaDiskLedger -Plan @($missing)[0] 6>&1 3>&1 | ForEach-Object { [string]$_ })
    Assert-Match -Text ($missingLines -join "`n") -Pattern '\[KafkaDiskLedger\]ZonesConfigMissing:' -Because 'zones 配置缺席时 Z 不可信,要打告警'
    Assert-NotMatch -Text ($missingLines -join "`n") -Pattern 'Zones configuration missing' -Because '与 Get-InfraZoneIds 的预建库告警区分开(MySQL 总账用例断言 zone-up 输出里没有那一条)'
    Assert-Match -Text ([string]@($missingLines | Where-Object { $_ -like 'Kafka disk ledger: brokers=*' })[0]) -Pattern 'live_extra_zones=unread zones_config=missing$' -Because 'DryRun 没读线上 zone;行尾仍标 zones_config=missing'

    Reset-S5bFixture -Reply @{ ExitCode = 0; Stdout = 'kafka:20Gi' }
    $script:DryRun = $false
    $live = Resolve-KafkaDiskPlan
    Assert-Equal -Expected 'live/20/268435456' -Actual ('{0}/{1}/{2}' -f $live.Source, $live.EffectiveGi, $live.DbTaskRetentionBytes) -Because '已有集群沿用线上 20Gi,按盘收缩'

    foreach ($bad in @(@{ Value = [long]-1; Pattern = '必须 ≥ 0' }, @{ Value = [long]1000; Pattern = '不到 2 个 segment' })) {
        Reset-S5bFixture
        $script:KafkaDbTaskRetentionBytes = $bad.Value
        $thrown = ''
        try { $null = Resolve-KafkaDiskPlan } catch { $thrown = $_.Exception.Message }
        Assert-Match -Text $thrown -Pattern $bad.Pattern -Because "-KafkaDbTaskRetentionBytes $($bad.Value) 必须在写路径入口被拒"
    }
}

Test-Case 'S5b Assert-KafkaStorageChangeIsSafe 与 Write-KafkaDiskLedger:扩盘拒绝附 runbook、缩盘拒绝;账本行字段齐全,收缩时打 FitLimited 告警' {
    Reset-S5bFixture -Reply @{ ExitCode = 0; Stdout = 'kafka:20Gi' }
    $script:DryRun = $false
    $script:KafkaStorageGi = 80
    $expand = Resolve-KafkaDiskPlan
    $thrown = ''
    try { Assert-KafkaStorageChangeIsSafe -Plan $expand } catch { $thrown = $_.Exception.Message }
    foreach ($fragment in @('volumeClaimTemplates 不可变', 'patch pvc kafka-data-kafka-<i>', 'delete statefulset kafka --cascade=orphan', '-KafkaStorageGi 80', 'allowVolumeExpansion')) {
        Assert-Match -Text $thrown -Pattern ([regex]::Escape($fragment)) -Because "expand-refused 的错误要带完整 runbook(缺「$fragment」)"
    }

    Reset-S5bFixture -Reply @{ ExitCode = 0; Stdout = 'kafka:80Gi' }
    $script:DryRun = $false
    $script:KafkaStorageGi = 40
    $thrown = ''
    try { Assert-KafkaStorageChangeIsSafe -Plan (Resolve-KafkaDiskPlan) } catch { $thrown = $_.Exception.Message }
    Assert-Match -Text $thrown -Pattern 'PVC 不能缩' -Because 'shrink-refused'

    foreach ($ok in @(@{ Live = 'kafka:20Gi'; Explicit = 0 }, @{ Live = 'kafka:20Gi'; Explicit = 20 }, @{ Live = ''; Explicit = 80 })) {
        Reset-S5bFixture -Reply @{ ExitCode = 0; Stdout = $ok.Live }
        $script:DryRun = $false
        $script:KafkaStorageGi = $ok.Explicit
        Assert-KafkaStorageChangeIsSafe -Plan (Resolve-KafkaDiskPlan)
    }

    Reset-S5bFixture -Reply @{ ExitCode = 0; Stdout = 'kafka:20Gi' }
    $script:DryRun = $false
    $lines = @(Write-KafkaDiskLedger -Plan (Resolve-KafkaDiskPlan) 6>&1 3>&1 | ForEach-Object { [string]$_ })
    $ledger = @($lines | Where-Object { $_ -like 'Kafka disk ledger: brokers=*' })
    Assert-Equal -Expected 1 -Actual $ledger.Count -Because '恰好一行账本'
    $want = 'Kafka disk ledger: brokers=1 rf=1 zones=2 db_task_partitions=10 segment_bytes=33554432 db_task_retention_bytes=268435456 target_retention_bytes=2785017856 pvc_gib=20 pvc_source=live required_gib=80 implied_db_task_buffer_min=10 db_task_retention_ms_min=1440 design_bytes_per_sec=3145728 audit_partitions=9 other_topics_reserve_gib=4 live_sts_gib=20 live_pvc_min_gib=none live_extra_zones=0'
    Assert-Equal -Expected $want -Actual $ledger[0] -Because '账本行字段与顺序(README「Kafka 磁盘预算」一节照抄这一行;改字段要两边一起改)'
    Assert-True -Condition (@($lines | Where-Object { $_ -like 'Kafka disk ledger note:*' }).Count -eq 1) -Because '提醒 retention.bytes 是 topic 级设置、先部署的 zone 在下次 zone-up 前保留旧值'
    Assert-Match -Text ($lines -join "`n") -Pattern '\[KafkaDiskLedger\]FitLimited:' -Because '按盘收缩要告警(只告警不阻断)'
    # R5a 复核 [3]:收缩后的值低于 topic 现值时,发布瞬间 Kafka 就删掉超出部分的未消费积压,告警要求先看 lag。
    Assert-Match -Text ($lines -join "`n") -Pattern ([regex]::Escape('confirm the db consumer lag is ~0 for every partition before rolling db / login')) -Because 'FitLimited 告警要提醒发布前确认 db 消费 lag 为 0'
    Assert-Match -Text ($lines -join "`n") -Pattern ([regex]::Escape('the broker default 536870912')) -Because '告警里给出 broker 缺省值,便于判断是否低于 topic 现值'
    Assert-Match -Text ($lines -join "`n") -Pattern '\[KafkaDiskLedger\]BufferBelowRetention:' -Because '按设计速率约 10 分钟的缓冲短于 24 小时的 retention.ms'
    Assert-NotMatch -Text ($lines -join "`n") -Pattern '\[KafkaDiskLedger\](ZonesConfigMissing|LiveZonesNotInConfig):' -Because 'zones 配置在场、线上没有配置外的 zone,不该告警'
}

Test-Case 'S5b R5a 复核 [0]:--cascade=orphan 之后(StatefulSet 不在、PVC 20Gi)不带参数按 live-pvc/20 算,显式 80 被拒;模板 80Gi 而 PVC 20Gi 时按 20Gi 算、渲染模板仍是 80Gi' {
    Reset-S5bFixture -Reply @{ ExitCode = 0; Stdout = '' } -PvcReply @{ ExitCode = 0; Stdout = "kafka-data-kafka-0   20Gi   20Gi" }
    $script:DryRun = $false
    $orphan = Resolve-KafkaDiskPlan
    Assert-Equal -Expected 'live-pvc/20/20/268435456' -Actual ('{0}/{1}/{2}/{3}' -f $orphan.Source, $orphan.EffectiveGi, $orphan.StorageTemplateGi, $orphan.DbTaskRetentionBytes) -Because 'StatefulSet 不在、PVC 20Gi、不带 -KafkaStorageGi:按实际盘 20Gi 算,不按公式值 80Gi(否则 retention.bytes 2785017856 写爆 20Gi 的盘)'
    Assert-KafkaStorageChangeIsSafe -Plan $orphan

    Reset-S5bFixture -Reply @{ ExitCode = 0; Stdout = '' } -PvcReply @{ ExitCode = 0; Stdout = "kafka-data-kafka-0   20Gi   80Gi" }
    $script:DryRun = $false
    $script:KafkaStorageGi = 80
    $thrown = ''
    try { Assert-KafkaStorageChangeIsSafe -Plan (Resolve-KafkaDiskPlan) } catch { $thrown = $_.Exception.Message }
    foreach ($fragment in @('20Gi', '先完成', '第 ② 步', 'CAPACITY', 'FileSystemResizePending')) {
        Assert-Match -Text $thrown -Pattern ([regex]::Escape($fragment)) -Because "StatefulSet 不在 + PVC 实际 20Gi + 显式 80 必须拒绝(pvc-not-expanded),错误要带「$fragment」"
    }

    Reset-S5bFixture -Reply @{ ExitCode = 0; Stdout = '' } -PvcReply @{ ExitCode = 0; Stdout = "kafka-data-kafka-0   80Gi   80Gi`nkafka-data-kafka-1   80Gi   80Gi" }
    $script:DryRun = $false
    $script:KafkaStorageGi = 80
    $adopt = Resolve-KafkaDiskPlan
    Assert-KafkaStorageChangeIsSafe -Plan $adopt
    Assert-Equal -Expected 'explicit/80/80' -Actual ('{0}/{1}/{2}' -f $adopt.Source, $adopt.EffectiveGi, $adopt.StorageTemplateGi) -Because 'runbook 第 ④ 步:每块都已扩到 80Gi,带 -KafkaStorageGi 80 放行'

    Reset-S5bFixture -Reply @{ ExitCode = 0; Stdout = 'kafka:80Gi' } -PvcReply @{ ExitCode = 0; Stdout = "kafka-data-kafka-0   20Gi   80Gi" }
    $script:DryRun = $false
    $stale = Resolve-KafkaDiskPlan
    Assert-Equal -Expected 'live/20/80/268435456' -Actual ('{0}/{1}/{2}/{3}' -f $stale.Source, $stale.EffectiveGi, $stale.StorageTemplateGi, $stale.DbTaskRetentionBytes) -Because '模板 80Gi、实际 20Gi:retention.bytes 按 20Gi 算,kafka.yaml 渲染线上模板 80Gi(不可变字段)'
    Assert-KafkaStorageChangeIsSafe -Plan $stale
    $staleLedger = @(Write-KafkaDiskLedger -Plan $stale 6>&1 3>&1 | ForEach-Object { [string]$_ } | Where-Object { $_ -like 'Kafka disk ledger: brokers=*' })
    Assert-Match -Text ([string]$staleLedger[0]) -Pattern 'pvc_gib=20 pvc_source=live .* live_sts_gib=80 live_pvc_min_gib=20 ' -Because '账本行同时给出模板与最小实际 PVC'
}

Test-Case 'S5b R5a 复核 [2]:线上 zone namespace 并入 Z(配置外的 z103 多算一个并告警);zones 配置缺席时非 dev 档拒绝、dev 档按线上 zone 数;列 namespace 失败 throw' {
    Reset-S5bFixture -NamespaceReply @{ ExitCode = 0; Stdout = "contract-infra`ncontract-zone-yesterday`ncontract-zone-today`ncontract-zone-z103" }
    $script:DryRun = $false
    $extra = Resolve-KafkaDiskPlan
    Assert-Equal -Expected 3 -Actual $extra.ZoneCount -Because 'zones.json 只有 101 / 102,线上还有 zone-up -ZoneId 单独部署的 z103:Z = 2 + 1'
    Assert-Equal -Expected 'z103' -Actual (@($extra.LiveExtraZoneNames) -join ',') -Because '配置里的 yesterday / today 不重复算'
    $twoZones = New-S5bDiskPlan -Tier 'prod' -Overrides @{ ZoneCount = 2; LiveStorageGi = 80 }
    $threeZones = New-S5bDiskPlan -Tier 'prod' -Overrides @{ ZoneCount = 3; LiveStorageGi = 80 }
    Assert-True -Condition ($threeZones.DbTaskRetentionBytes -lt $twoZones.DbTaskRetentionBytes) -Because '多一个 zone,同一块盘上每分区可容纳值要变小(否则 2 个 zone 拿到不收缩的 2785017856,1.3 余量被吃光)'
    $extraLines = @(Write-KafkaDiskLedger -Plan $extra 6>&1 3>&1 | ForEach-Object { [string]$_ })
    Assert-Match -Text ($extraLines -join "`n") -Pattern '\[KafkaDiskLedger\]LiveZonesNotInConfig:' -Because '线上有配置外的 zone 要告警'
    Assert-Match -Text ([string]@($extraLines | Where-Object { $_ -like 'Kafka disk ledger: brokers=*' })[0]) -Pattern '(?<![\w])zones=3 .* live_extra_zones=1$' -Because '账本行 zones 已含线上配置外的 zone'

    Reset-S5bFixture -NamespaceReply @{ ExitCode = 0; Stdout = "contract-zone-yesterday`ncontract-zone-today" }
    $script:DryRun = $false
    $script:ZonesConfigPath = Join-Path ([System.IO.Path]::GetTempPath()) ('s5b-zones-missing-' + [guid]::NewGuid().ToString('N') + '.json')
    $missingLive = Resolve-KafkaDiskPlan
    Assert-Equal -Expected 2 -Actual $missingLive.ZoneCount -Because 'zones 配置缺席(dev 档):-ZoneId 101(-ZoneName yesterday)+ 线上的 today'

    Reset-S5bFixture
    $script:ZonesConfigPath = Join-Path ([System.IO.Path]::GetTempPath()) ('s5b-zones-missing-' + [guid]::NewGuid().ToString('N') + '.json')
    foreach ($profileName in @('staging', 'prod')) {
        $script:ReleaseProfile = $profileName
        $thrown = ''
        try { $null = Resolve-KafkaDiskPlan } catch { $thrown = $_.Exception.Message }
        Assert-Match -Text $thrown -Pattern 'Z 不可信' -Because "-ReleaseProfile ${profileName}:zones 配置缺席时 retention.bytes 可能超量,拒绝部署"
        Assert-Match -Text $thrown -Pattern '尚未做任何写操作' -Because '拒绝早于任何写操作'
    }

    Reset-S5bFixture -NamespaceReply @{ ExitCode = 1; Stdout = '' }
    $script:DryRun = $false
    $thrown = ''
    try { $null = Resolve-KafkaDiskPlan } catch { $thrown = $_.Exception.Message }
    Assert-Match -Text $thrown -Pattern 'fail-closed' -Because '数不清线上 zone 就不算盘预算'
}

Test-Case 'S5b zone-up DryRun:db 与 login 两个 ConfigMap 的 RetentionMs / RetentionBytes 同值(86400000 / 2785017856),账本行 pvc_source=computed required_gib=80' {
    $dbFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-db-config')
    $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-login-config')
    foreach ($key in @('RetentionMs', 'RetentionBytes')) {
        $dbVal = Get-FlatValue -Flat $dbFlat -KeyPath "data.db.yaml.ServerConfig.Kafka.$key"
        $loginVal = Get-FlatValue -Flat $loginFlat -KeyPath "data.login.yaml.Kafka.$key"
        Assert-Equal -Expected $loginVal -Actual $dbVal -Because "两个服务启动时都对同一个 db_task topic 设置 $key,值一不同就随重启顺序横跳(B-15)"
    }
    Assert-Equal -Expected '86400000' -Actual (Get-FlatValue -Flat $dbFlat -KeyPath 'data.db.yaml.ServerConfig.Kafka.RetentionMs') -Because 'prod 档 db_task retention.ms 24h(以前 db 不写、只靠 Go 默认值碰巧相等)'
    Assert-Equal -Expected '2785017856' -Actual (Get-FlatValue -Flat $dbFlat -KeyPath 'data.db.yaml.ServerConfig.Kafka.RetentionBytes') -Because 'prod 档按公式建盘时不收缩'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($devOut, 'Kafka disk ledger: brokers=').Count) -Because '写路径入口恰好打一行盘预算'
    $ledger = Get-S5bLedgerText -Output $devOut
    foreach ($token in @('pvc_source=computed', 'required_gib=80', 'pvc_gib=80', 'zones=2', 'db_task_retention_bytes=2785017856')) {
        Assert-Match -Text $ledger -Pattern ('(?<![\w])' + [regex]::Escape($token) + '(?![\w])') -Because "账本行要有 $token"
    }
    Assert-NotMatch -Text $devOut -Pattern '\[KafkaDiskLedger\]FitLimited:' -Because '按公式建盘不会收缩'
    Assert-Match -Text $devOut -Pattern '\[KafkaDiskLedger\]BufferBelowRetention:' -Because 'prod 档 retention.bytes 按 2h 算,先于 24h 的 retention.ms 生效,要告警'
}

Test-Case 'S5b infra-up DryRun:kafka 清单的 PVC 是 80Gi(prod 档),-KafkaProfile dev 是 20Gi;没有残留的 __KAFKA_STORAGE_SIZE__' {
    Assert-Equal -Expected 0 -Actual $battleInfraRun.ExitCode -Because "infra-up DryRun 必须成功。输出: $($battleInfraRun.Output)"
    $kafka = Select-ManifestByName -Output $battleInfraRun.Output -Name 'kafka'
    Assert-Match -Text $kafka -Pattern '(?m)^\s+storage: 80Gi\s*$' -Because 'prod 档、2 个 zone、单 broker:RequiredGi 80'
    Assert-NotMatch -Text $battleInfraRun.Output -Pattern '__KAFKA_STORAGE_SIZE__' -Because '占位必须替换'

    $devRunS5b = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-KafkaProfile', 'dev')) -Env $BattleTestEnv
    Assert-Equal -Expected 0 -Actual $devRunS5b.ExitCode -Because "-KafkaProfile dev 的 infra-up DryRun 必须成功。输出: $($devRunS5b.Output)"
    Assert-Match -Text (Select-ManifestByName -Output $devRunS5b.Output -Name 'kafka') -Pattern '(?m)^\s+storage: 20Gi\s*$' -Because 'dev 档按公式约 7Gi,取下限 20Gi'
    Assert-NotMatch -Text $devRunS5b.Output -Pattern '__KAFKA_STORAGE_SIZE__' -Because '占位必须替换'
    $ledger = Get-S5bLedgerText -Output $devRunS5b.Output
    foreach ($token in @('segment_bytes=16777216', 'db_task_retention_bytes=150994944', 'required_gib=20', 'pvc_source=computed')) {
        Assert-Match -Text $ledger -Pattern ('(?<![\w])' + [regex]::Escape($token) + '(?![\w])') -Because "dev 档账本行要有 $token"
    }
    Assert-NotMatch -Text $devRunS5b.Output -Pattern '\[KafkaDiskLedger\]BufferBelowRetention:' -Because 'dev 档缓冲目标 = retention.ms = 1h,按设计速率约 71 分钟,不该告警'

    $kafkaYaml = Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/infra/kafka.yaml') -Raw
    Assert-Match -Text $kafkaYaml -Pattern '(?m)^\s+storage: __KAFKA_STORAGE_SIZE__\s*$' -Because 'kafka.yaml 的 PVC 大小只能来自 Get-KafkaDiskPlan'
    Assert-NotMatch -Text $kafkaYaml -Pattern '(?m)^\s+storage: \d+Gi' -Because '不得再写死尺寸'
}

Test-Case 'S5b zone-up DryRun -KafkaStorageGi 40:显式的盘装不下目标值 → 两个 ConfigMap 同为收缩后的 1140850688,账本行 pvc_source=explicit 并打 FitLimited' {
    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SkipJavaSvc', '-KafkaStorageGi', '40'))
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "盘不够只告警、不阻断。输出: $($run.Output)"
    $dbVal = Get-FlatValue -Flat (ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $run.Output -Name 'go-svc-db-config')) -KeyPath 'data.db.yaml.ServerConfig.Kafka.RetentionBytes'
    $loginVal = Get-FlatValue -Flat (ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $run.Output -Name 'go-svc-login-config')) -KeyPath 'data.login.yaml.Kafka.RetentionBytes'
    Assert-Equal -Expected '1140850688' -Actual $dbVal -Because 'db 渲染的是按 40Gi 收缩后的值'
    Assert-Equal -Expected $dbVal -Actual $loginVal -Because 'login 与 db 同值'
    $ledger = Get-S5bLedgerText -Output $run.Output
    foreach ($token in @('pvc_source=explicit', 'pvc_gib=40', 'required_gib=80', 'db_task_retention_bytes=1140850688')) {
        Assert-Match -Text $ledger -Pattern ('(?<![\w])' + [regex]::Escape($token) + '(?![\w])') -Because "账本行要有 $token"
    }
    Assert-Match -Text $run.Output -Pattern '\[KafkaDiskLedger\]FitLimited:' -Because '按盘收缩要告警'
}

Test-Case 'S5b 负向:-KafkaDbTaskRetentionBytes -1 在任何 apply 之前拒绝(错误含「必须 ≥ 0」);-KafkaStorageGi -1 被参数校验拒绝' {
    # 冒号写法:pwsh -File 下 "-1" 单独作为一个参数时可能被当成参数名,冒号把值钉在参数上(about_Pwsh 的 -File 一节)。
    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SkipJavaSvc', '-KafkaDbTaskRetentionBytes:-1'))
    Assert-True -Condition ($run.ExitCode -ne 0) -Because 'Kafka 里 -1 = 不限,磁盘无界'
    Assert-Match -Text $run.Output -Pattern '必须 ≥ 0' -Because "错误文本要点名原因,而不是别的执行错误。输出: $($run.Output)"
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl apply' -Because '拒绝必须先于任何资源变更'

    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SkipJavaSvc', '-KafkaStorageGi:-1'))
    Assert-True -Condition ($run.ExitCode -ne 0) -Because '负的 PVC 大小必须由入口参数校验拒绝'
    Assert-Match -Text $run.Output -Pattern 'KafkaStorageGi' -Because '不能把其他执行错误误判为参数校验成功'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl apply' -Because '参数校验必须先于任何资源变更'
}

Test-Case 'S5b 写路径门禁块:Write-MysqlConnectionLedger 之后依次 Resolve-KafkaDiskPlan → Write-KafkaDiskLedger → Assert-KafkaStorageChangeIsSafe;Apply-Infra 替换 __KAFKA_STORAGE_SIZE__' {
    $writeGates = @($deadlineDeployAst.EndBlock.Statements | Where-Object {
            $_ -is [System.Management.Automation.Language.IfStatementAst] -and
            $_.Clauses[0].Item1.Extent.Text -match '^\$Command\s+-in\s+@\(\s*"zone-up"\s*,\s*"all-up"\s*,\s*"infra-up"\s*\)$'
        })
    Assert-Equal -Expected 1 -Actual $writeGates.Count -Because '顶层恰好一个写路径门禁块'
    $statements = @($writeGates[0].Clauses[0].Item2.Statements | ForEach-Object { $_.Extent.Text.Trim() })
    $at = @{}
    foreach ($pair in @(
        @{ Name = 'mysql'; Pattern = '^Write-MysqlConnectionLedger$' }
        @{ Name = 'resolve'; Pattern = '^\$null = Resolve-KafkaDiskPlan$' }
        @{ Name = 'ledger'; Pattern = '^Write-KafkaDiskLedger -Plan \$script:KafkaDiskPlan$' }
        @{ Name = 'assert'; Pattern = '^Assert-KafkaStorageChangeIsSafe -Plan \$script:KafkaDiskPlan$' }
    )) {
        $at[$pair.Name] = -1
        for ($i = 0; $i -lt $statements.Count; $i++) { if ($statements[$i] -match $pair.Pattern) { $at[$pair.Name] = $i; break } }
        Assert-True -Condition ($at[$pair.Name] -ge 0) -Because "写路径门禁块里要有 /$($pair.Pattern)/"
    }
    Assert-True -Condition ($at.mysql -lt $at.resolve -and $at.resolve -lt $at.ledger -and $at.ledger -lt $at.assert) -Because '先算、再打账本、最后判盘变更;全部在门禁块里,早于 switch ($Command) 的任何写操作'
    $applyInfra = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Apply-Infra' }, $true)
    Assert-Match -Text $applyInfra.Extent.Text -Pattern ([regex]::Escape('.Replace("__KAFKA_STORAGE_SIZE__"')) -Because 'Apply-Infra 的 kafka.yaml 分支替换 PVC 大小占位'
    Assert-Match -Text $applyInfra.Extent.Text -Pattern ([regex]::Escape('$infraDiskPlan.StorageTemplateGi')) -Because '渲染的是线上模板(StorageTemplateGi),不是按实际盘取小的 EffectiveGi:模板不可变,不同即 apply 被拒'
    Assert-NotMatch -Text $applyInfra.Extent.Text -Pattern ([regex]::Escape('$infraDiskPlan.EffectiveGi')) -Because 'EffectiveGi 只用于算 retention.bytes'
}

Remove-Item -Path Function:kubectl -ErrorAction SilentlyContinue
foreach ($s5bVar in @('RepoRoot', 'KafkaBrokers', 'ReleaseProfile', 'OpsProfile', 'InfraNamespace', 'KubeContext', 'KubeConfig', 'ZoneId', 'ZonesConfigPath', 'DryRun',
        'KafkaSegmentBytes', 'KafkaRetentionBytes', 'KafkaDbTaskRetentionMs', 'KafkaDbTaskDesignBytesPerSec', 'KafkaDbTaskBufferTargetMs',
        'KafkaOtherTopicsReserveGi', 'KafkaStorageGi', 'KafkaDbTaskRetentionBytes', 'KafkaDiskPlan', 'S5bKubectlReply', 'S5bKubectlCalls',
        'S5bKubectlPvcReply', 'S5bKubectlNamespaceReply', 'NamespacePrefix', 'ZoneName')) {
    Remove-Variable -Name $s5bVar -Scope Script -ErrorAction SilentlyContinue
}

# ─────────────────────────────────────────────────────────────────
# ④ 重试只交给分区属主 + WAITAOF(S1;规格:docs/handoff/godb-pipeline-impl-20261015/S1-retry-owner-waitaof-spec.md
#    「[modify] tools/scripts/k8s_deploy.ps1」「[modify] tools/scripts/tests/k8s_deploy_contract.tests.ps1」与修订稿 §8;
#    设计:docs/design/db-retry-owner-routing.md §7 / §8)
# ─────────────────────────────────────────────────────────────────
# 部署侧只有三件事:db ConfigMap 原样镜像 go/db/etc/db.yaml 的 Kafka.RetryRouting 与 RedisClient.WaitAOF* 三项;
# 写路径入口的副本数门禁(manifest 的 replicas 与同一份 manifest 里 HPA 的 maxReplicas ≤ PartitionCnt);
# Redis 连接总账 db 行计入 WAITAOF 专用客户端的 2 条连接。门禁函数按 AST 抽出直接调用(与上文 MySQL 总账同一做法),
# 依赖再抽一次,本节不依赖前文各节的执行顺序;读文件的函数经 $RepoRoot 定位仓库,由各用例自己设(S5b 一节收尾时删了脚本级的 $RepoRoot)。
$S1FunctionNames = @(
    'Test-DbReplicaPartitionBudget', 'Assert-DbReplicasWithinPartitionContract',
    # 依赖(与 k8s_deploy.ps1 里同一份函数体)。
    'Get-AuthoritativeScalar', 'Get-ManifestDeploymentReplicas',
    'Get-RedisPoolMirror', 'Get-RedisMaxClientsFromManifest', 'Get-RedisConnectionLedgerInputs'
)
foreach ($s1FnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($S1FunctionNames -contains $s1FnAst.Name) {
        Set-Item -Path "Function:script:$($s1FnAst.Name)" -Value $s1FnAst.Body.GetScriptBlock()
    }
}
foreach ($s1FnName in $S1FunctionNames) {
    if (-not (Test-Path "Function:$s1FnName")) { throw "k8s_deploy.ps1 缺少函数 ${s1FnName}(go/db 副本数门禁 / Redis 总账被删了?后续断言无意义)" }
}

$S1DbYaml = 'go/db/etc/db.yaml'
$S1DbManifest = 'deploy/k8s/manifests/go-svc/db.yaml'
$S1PartitionCnt = [int](Get-EtcValue -RelativePath $S1DbYaml -KeyPath 'ServerConfig.Kafka.PartitionCnt')

# 夹具:把临时副本 db manifest 里 Deployment 顶层的 replicas(两格缩进,恰好一处)改成 $Replicas,改完读回核对;不成立即 throw(没改成的负向断言是空转)。
function Set-S1FixtureDbReplicas {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][int]$Replicas)
    $text = [System.IO.File]::ReadAllText($Path)
    $pattern = '(?m)^  replicas:[ \t]*\d+'
    if ([regex]::Matches($text, $pattern).Count -ne 1) { throw "夹具:$Path 里两格缩进的 replicas 行应恰好 1 处" }
    [System.IO.File]::WriteAllText($Path, [regex]::Replace($text, $pattern, "  replicas: $Replicas"), [System.Text.UTF8Encoding]::new($false))
    if ([regex]::Match([System.IO.File]::ReadAllText($Path), '(?m)^  replicas:[ \t]*(\d+)').Groups[1].Value -ne [string]$Replicas) { throw "夹具:$Path 的 replicas 没有改成 $Replicas" }
}

# 夹具:在临时副本 db manifest 末尾追加一份 HPA 文档。$MaxReplicas 为 $null 时不写 maxReplicas 行(负向用例)。
function Add-S1FixtureDbHpa {
    param([Parameter(Mandatory = $true)][string]$Path, [AllowNull()]$MaxReplicas)
    $hpa = @(
        '---'
        'apiVersion: autoscaling/v2'
        'kind: HorizontalPodAutoscaler'
        'metadata:'
        '  name: db'
        'spec:'
        '  scaleTargetRef:'
        '    apiVersion: apps/v1'
        '    kind: Deployment'
        '    name: db'
        '  minReplicas: 1'
    )
    if ($null -ne $MaxReplicas) { $hpa += "  maxReplicas: $MaxReplicas" }
    $text = [System.IO.File]::ReadAllText($Path).TrimEnd() + "`n" + ($hpa -join "`n") + "`n"
    [System.IO.File]::WriteAllText($Path, $text, [System.Text.UTF8Encoding]::new($false))
}

Test-Case '④ zone-up DryRun:db ConfigMap 的 Kafka.RetryRouting 与 RedisClient.WaitAOFNumLocal / WaitAOFNumReplicas / WaitAOFTimeoutMs == go/db/etc/db.yaml(生成器不得自带常数)' {
    # 切 owner(rollout 第 4 步)、redis-ha 接入时把 NumReplicas 改 1,都只改 db.yaml:ConfigMap 必须跟着走,不得留一份生成器常数。
    # 四个键在 db.yaml 里必须显式写出(Get-AuthoritativeScalar 查不到即 throw,基线 DryRun 就会失败):这里先核前提再比对。
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-db-config')
    # Fencing 两个滚动开关与 RequireTiDB 的实际后端门禁同一口径镜像,一并核对。
    foreach ($keyPath in @('ServerConfig.Kafka.RetryRouting', 'ServerConfig.RedisClient.WaitAOFNumLocal', 'ServerConfig.RedisClient.WaitAOFTimeoutMs', 'ServerConfig.Fencing.Mode', 'ServerConfig.Fencing.EpochOverride', 'ServerConfig.Database.RequireTiDB')) {
        $expected = Get-EtcValue -RelativePath $S1DbYaml -KeyPath $keyPath
        Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($expected)) -Because "前提:$S1DbYaml 必须显式写出 $keyPath 的值(不写 = go/db 代码默认值,ConfigMap 镜像不出来)"
        $actual = Get-FlatValue -Flat $flat -KeyPath ('data.db.yaml.' + $keyPath)
        Assert-Equal -Expected $expected -Actual $actual -Because "go-svc-db-config 的 $keyPath 必须与 $S1DbYaml 一致(生成器不得自带常数)"
    }
    # WaitAOFNumReplicas 例外(2026-10-19 redis-ha 接入):按共享 Redis 的形态渲染(与 data-service 同一个 Get-RouteStoreWaitAOFNumReplicas),
    # 不镜像 db.yaml —— db.yaml 那一项给本机 compose 单实例用,必须保持 0(写 1 本机 go/db 启动探针等不到副本而拒启),但仍须显式写出。
    Assert-Equal -Expected '0' -Actual (Get-EtcValue -RelativePath $S1DbYaml -KeyPath 'ServerConfig.RedisClient.WaitAOFNumReplicas') -Because "$S1DbYaml 的 WaitAOFNumReplicas 给本机单实例用,保持 0"
    Assert-Equal -Expected '0' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.db.yaml.ServerConfig.RedisClient.WaitAOFNumReplicas') -Because 'dev 档缺省单实例(redis.yaml),db 的 WAITAOF 不等副本'
    $prodDbFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $prodRun.Output -Name 'go-svc-db-config')
    Assert-Equal -Expected '1' -Actual (Get-FlatValue -Flat $prodDbFlat -KeyPath 'data.db.yaml.ServerConfig.RedisClient.WaitAOFNumReplicas') -Because 'prod 档缺省高可用(redis-ha.yaml),db 的 WAITAOF 等 1 个副本(与 data-service 同源)'
}

Test-Case '围栏部署:OwnerEpochRedis 缺省复用主池;显式独立池未登记总账前拒绝部署' {
    $RepoRoot = Get-RepoRoot
    Assert-DbOwnerEpochRedisDeployment
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-db-config')
    Assert-True -Condition (-not $flat.Scalars.Contains('data.db.yaml.ServerConfig.OwnerEpochRedis')) `
        -Because '源配置未拆池时ConfigMap也不能额外开池'

    $tempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('db-owner-redis-contract-' + [guid]::NewGuid().ToString('N'))
    $fixturePath = Join-Path $tempRoot 'go/db/etc/db.yaml'
    try {
        New-Item -ItemType Directory -Force -Path (Split-Path -Parent $fixturePath) | Out-Null
        [System.IO.File]::WriteAllText($fixturePath, "ServerConfig:`n  OwnerEpochRedis:`n    Hosts: core-redis.invalid:6379`n    DB: 0`n    PoolSize: 8`n", [System.Text.UTF8Encoding]::new($false))
        $RepoRoot = $tempRoot
        $message = ''
        try { Assert-DbOwnerEpochRedisDeployment } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern 'OwnerEpochRedis.*Redis 连接总账.*MAXCLIENTS' `
            -Because '显式独立池必须拒绝并指出尚未登记的预算,不能静默丢配置或低估连接'
    }
    finally {
        $RepoRoot = Get-RepoRoot
        Remove-Item -LiteralPath $fixturePath -Force -ErrorAction SilentlyContinue
        foreach ($directory in @('go/db/etc', 'go/db', 'go', '')) {
            Remove-Item -LiteralPath (Join-Path $tempRoot $directory) -Force -ErrorAction SilentlyContinue
        }
    }
}

Test-Case '④ Test-DbReplicaPartitionBudget 负向:replicas 11 > PartitionCnt 10 即 throw,文本含「replicas ≤ PartitionCnt」、点名两份文件与热备;HPA 用同一判定,文本点名 maxReplicas' {
    $message = ''
    try { Test-DbReplicaPartitionBudget -Replicas 11 -PartitionCnt 10 } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern 'replicas ≤ PartitionCnt' -Because '副本数超过分区数必须 throw,并写明不变量'
    Assert-Match -Text $message -Pattern ([regex]::Escape('go/db 副本数门禁:deploy/k8s/manifests/go-svc/db.yaml 的 replicas=11')) -Because '错误必须点名 manifest 与副本数'
    Assert-Match -Text $message -Pattern ([regex]::Escape('go/db/etc/db.yaml 的 ServerConfig.Kafka.PartitionCnt=10')) -Because '错误必须点名分区数出处'
    Assert-Match -Text $message -Pattern '热备' -Because '错误要说明多出的副本只做热备(拿不到分区)'

    $message = ''
    try { Test-DbReplicaPartitionBudget -Replicas 11 -PartitionCnt 10 -ReplicasSource 'HorizontalPodAutoscaler 的 maxReplicas' } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern ([regex]::Escape('HorizontalPodAutoscaler 的 maxReplicas=11')) -Because 'HPA 越界时错误点名 maxReplicas,而不是 replicas'
    Assert-Match -Text $message -Pattern 'replicas ≤ PartitionCnt' -Because 'HPA 与 Deployment 是同一条不变量'
}

Test-Case '④ Test-DbReplicaPartitionBudget 正向与参数校验:10 == 10、1 < 10 放行且不往管道输出任何东西;PartitionCnt 0 / -1 即 throw(fail-closed)' {
    foreach ($replicas in @(10, 1)) {
        $out = @(Test-DbReplicaPartitionBudget -Replicas $replicas -PartitionCnt 10)
        Assert-Equal -Expected 0 -Actual $out.Count -Because "replicas=$replicas ≤ PartitionCnt=10 必须放行;门禁在写路径入口调用,放行时不得往输出里漏一个 True"
    }
    foreach ($partitionCnt in @(0, -1)) {
        $message = ''
        try { Test-DbReplicaPartitionBudget -Replicas 0 -PartitionCnt $partitionCnt } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern ([regex]::Escape("ServerConfig.Kafka.PartitionCnt=$partitionCnt 必须是正整数")) -Because "PartitionCnt=${partitionCnt}:分区数不明就判不了副本数是否越界,必须 throw(副本数 0 也不放行)"
    }
}

Test-Case '④ 仓库现状:db manifest 的副本数 ≤ go/db/etc/db.yaml 的 PartitionCnt(独立读两份文件核对),Assert-DbReplicasWithinPartitionContract 对仓库放行' {
    $RepoRoot = Get-RepoRoot
    $manifestText = [System.IO.File]::ReadAllText((Join-Path $RepoRoot $S1DbManifest)) -replace "`r`n", "`n"
    $replicas = [regex]::Matches($manifestText, '(?m)^  replicas:[ \t]*(\d+)[ \t]*$')
    Assert-Equal -Expected 1 -Actual $replicas.Count -Because "$S1DbManifest 里两格缩进的 replicas 行应恰好 1 处"
    Assert-True -Condition ([int]$replicas[0].Groups[1].Value -le $S1PartitionCnt) -Because ("{0} 的 replicas={1} 超过 {2} 的 PartitionCnt={3}:多出的副本只能做热备" -f $S1DbManifest, $replicas[0].Groups[1].Value, $S1DbYaml, $S1PartitionCnt)
    $out = @(Assert-DbReplicasWithinPartitionContract)
    Assert-Equal -Expected 0 -Actual $out.Count -Because '仓库现状必须放行,且门禁不往输出里写东西'
}

Test-Case '④ 负向(夹具):replicas 超过 PartitionCnt、PartitionCnt 为 0 或非整数、HPA maxReplicas 越界或缺失、manifest 缺席时 Assert-DbReplicasWithinPartitionContract throw 并点名;HPA 在上界内放行(夹具是临时目录里的副本,不碰仓库)' {
    $tempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('db-replica-gate-' + [guid]::NewGuid().ToString('N'))
    $fixtureManifest = Join-Path $tempRoot $S1DbManifest
    $resetFixture = {
        foreach ($rel in @($S1DbYaml, $S1DbManifest)) {
            $dst = Join-Path $tempRoot $rel
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $dst) | Out-Null
            Copy-Item -LiteralPath (Join-Path (Get-RepoRoot) $rel) -Destination $dst -Force
        }
    }
    $over = $S1PartitionCnt + 1
    try {
        # Get-AuthoritativeScalar / Get-ManifestDeploymentReplicas 按动态作用域读 $RepoRoot(在 k8s_deploy.ps1 里是脚本级变量)。
        $RepoRoot = $tempRoot
        # 前提:副本不改就该放行。否则下面几条负向断言分不清是谁造成的失败。
        & $resetFixture
        Assert-DbReplicasWithinPartitionContract

        $cases = @(
            @{ Why = "replicas=$over 超过 PartitionCnt=$S1PartitionCnt"; Arrange = { Set-S1FixtureDbReplicas -Path $fixtureManifest -Replicas $over }
               Patterns = @('replicas ≤ PartitionCnt', [regex]::Escape("db.yaml 的 replicas=$over 超过")) }
            @{ Why = 'PartitionCnt=0'; Arrange = { Set-FixtureYamlScalar -Path (Join-Path $tempRoot $S1DbYaml) -KeyPath 'ServerConfig.Kafka.PartitionCnt' -Value '0' }
               Patterns = @([regex]::Escape('ServerConfig.Kafka.PartitionCnt=0 必须是正整数')) }
            @{ Why = 'PartitionCnt 不是整数'; Arrange = { Set-FixtureYamlScalar -Path (Join-Path $tempRoot $S1DbYaml) -KeyPath 'ServerConfig.Kafka.PartitionCnt' -Value 'ten' }
               Patterns = @([regex]::Escape("go/db 副本数门禁:go/db/etc/db.yaml 的 ServerConfig.Kafka.PartitionCnt='ten' 不是整数")) }
            @{ Why = "HPA maxReplicas=$over 超过 PartitionCnt"; Arrange = { Add-S1FixtureDbHpa -Path $fixtureManifest -MaxReplicas $over }
               Patterns = @('replicas ≤ PartitionCnt', [regex]::Escape("HorizontalPodAutoscaler 的 maxReplicas=$over 超过")) }
            @{ Why = 'HPA 没写 maxReplicas'; Arrange = { Add-S1FixtureDbHpa -Path $fixtureManifest -MaxReplicas $null }
               Patterns = @([regex]::Escape('HorizontalPodAutoscaler 两格缩进的 maxReplicas 行应恰好出现 1 次,实际 0 次')) }
            @{ Why = 'manifest 缺席'; Arrange = { Remove-Item -LiteralPath $fixtureManifest -Force }
               Patterns = @([regex]::Escape("go/db 副本数门禁:找不到 $S1DbManifest")) }
        )
        foreach ($case in $cases) {
            & $resetFixture
            & $case.Arrange
            $message = ''
            try { Assert-DbReplicasWithinPartitionContract } catch { $message = $_.Exception.Message }
            Assert-Match -Text $message -Pattern '^go/db 副本数门禁:' -Because "$($case.Why):必须以本门禁的名义 throw(fail-closed),不能放行或报别的执行错误"
            foreach ($pattern in $case.Patterns) {
                Assert-Match -Text $message -Pattern $pattern -Because "$($case.Why):错误文本必须点名原因"
            }
        }

        # 对照:HPA 的 maxReplicas 等于 PartitionCnt 放行(上界是 ≤,不是 <);注释里提到 HPA 不算 HPA 文档。
        & $resetFixture
        Add-S1FixtureDbHpa -Path $fixtureManifest -MaxReplicas $S1PartitionCnt
        Assert-DbReplicasWithinPartitionContract
    }
    finally {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-Case '④ 写路径入口:Assert-DbReplicasWithinPartitionContract 在 zone-up / all-up / infra-up 门禁块里恰好调用一次,排在 Write-MysqlConnectionLedger 之后、switch ($Command) 之前' {
    # 规格:门禁静态比较仓库的两份文件,先于任何集群写操作(写操作都在 switch ($Command) 的分支里)。基线 zone-up DryRun 成功即证明仓库现状放行。
    $writeGates = @($deadlineDeployAst.EndBlock.Statements | Where-Object {
            $_ -is [System.Management.Automation.Language.IfStatementAst] -and
            $_.Clauses[0].Item1.Extent.Text -match '^\$Command\s+-in\s+@\(\s*"zone-up"\s*,\s*"all-up"\s*,\s*"infra-up"\s*\)$'
        })
    Assert-Equal -Expected 1 -Actual $writeGates.Count -Because '顶层恰好一个写路径门禁块'
    $gateCalls = @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] -and $a.GetCommandName() -eq 'Assert-DbReplicasWithinPartitionContract' }, $true))
    Assert-Equal -Expected 1 -Actual $gateCalls.Count -Because '整个脚本只在写路径门禁块里调用一次 Assert-DbReplicasWithinPartitionContract'
    $statements = @($writeGates[0].Clauses[0].Item2.Statements | ForEach-Object { $_.Extent.Text.Trim() })
    $mysqlAt = [Array]::IndexOf($statements, 'Write-MysqlConnectionLedger')
    $gateAt = [Array]::IndexOf($statements, 'Assert-DbReplicasWithinPartitionContract')
    Assert-True -Condition ($mysqlAt -ge 0 -and $gateAt -gt $mysqlAt) -Because "门禁块的直接语句里要有 Assert-DbReplicasWithinPartitionContract,且排在 Write-MysqlConnectionLedger 之后(实际下标 mysql=$mysqlAt gate=$gateAt)"
    $commandSwitches = @($deadlineDeployAst.EndBlock.Statements | Where-Object {
            $_ -is [System.Management.Automation.Language.SwitchStatementAst] -and $_.Condition.Extent.Text -match '^\(?\s*\$Command\s*\)?$'
        })
    Assert-Equal -Expected 1 -Actual $commandSwitches.Count -Because '顶层恰好一个 switch ($Command)'
    Assert-True -Condition ($gateCalls[0].Extent.StartOffset -lt $commandSwitches[0].Extent.StartOffset) -Because '门禁必须先于 switch ($Command)(Apply-Infra / Apply-Zone 的集群写操作都在它的分支里)'
}

Test-Case '④ Redis 连接总账:db 行 Fixed == go/db/internal/kafka/durable_enqueue.go 的 durableClientPoolSize(WAITAOF 专用客户端,不占 PoolSize 主池,按开启计)' {
    # 专用客户端在 db.yaml 两项 WAITAOF 都写 0 时不建;总账按开启计(仓库 db.yaml 缺省开启),偏保守。源码改了池大小,这里先变红,逼着同改总账那一行。
    $RepoRoot = Get-RepoRoot
    $dbRow = @((Get-RedisConnectionLedgerInputs).Clients | Where-Object { $_.Name -eq 'db' })
    Assert-Equal -Expected 1 -Actual $dbRow.Count -Because 'Redis 连接总账里 db 只能有一行'
    $source = [System.IO.File]::ReadAllText((Join-Path $RepoRoot 'go/db/internal/kafka/durable_enqueue.go'))
    $poolSize = [regex]::Matches($source, '(?m)^\s*(?:const\s+)?durableClientPoolSize\s*=\s*(\d+)\s*$')
    Assert-Equal -Expected 1 -Actual $poolSize.Count -Because 'durable_enqueue.go 里 durableClientPoolSize 应恰好定义一次(字面量整数)'
    Assert-Equal -Expected ([int]$poolSize[0].Groups[1].Value) -Actual $dbRow[0].Fixed -Because 'db 行的 Fixed 就是 WAITAOF 专用客户端的池大小'
    Assert-Equal -Expected 'zone/0/0/db' -Actual ('{0}/{1}/{2}/{3}' -f $dbRow[0].Scope, $dbRow[0].DefaultPools, $dbRow[0].PubSub, $dbRow[0].PoolMirror) -Because 'db 行其余形状不变:按 zone 计、主池由 ServerConfig.RedisClient.PoolSize 镜像、没有缺省池与 Pub/Sub'
}

Test-Case 'G3b Redis 连接总账:scene-manager 行 Fixed == go/scene_manager/internal/svc/owner_epoch_mint.go 的 ownerEpochMintPoolSize(铸造专用客户端,不占 go-zero 缺省池)' {
    # 源码改了池大小,这里先变红,逼着同改总账那一行(owner_epoch_mint.go 头注释的契约)。
    $RepoRoot = Get-RepoRoot
    $smRow = @((Get-RedisConnectionLedgerInputs).Clients | Where-Object { $_.Name -eq 'scene-manager' })
    Assert-Equal -Expected 1 -Actual $smRow.Count -Because 'Redis 连接总账里 scene-manager 只能有一行'
    $source = [System.IO.File]::ReadAllText((Join-Path $RepoRoot 'go/scene_manager/internal/svc/owner_epoch_mint.go'))
    $poolSize = [regex]::Matches($source, '(?m)^\s*(?:const\s+)?ownerEpochMintPoolSize\s*=\s*(\d+)\s*$')
    Assert-Equal -Expected 1 -Actual $poolSize.Count -Because 'owner_epoch_mint.go 里 ownerEpochMintPoolSize 应恰好定义一次(字面量整数)'
    Assert-Equal -Expected ([int]$poolSize[0].Groups[1].Value) -Actual $smRow[0].Fixed -Because 'scene-manager 行的 Fixed 就是铸造专用客户端的池上限'
    Assert-Equal -Expected 'zone/1/0' -Actual ('{0}/{1}/{2}' -f $smRow[0].Scope, $smRow[0].DefaultPools, $smRow[0].PubSub) -Because 'scene-manager 行其余形状不变:按 zone 计、一个 go-zero 缺省池、没有 Pub/Sub'
}

Test-Case 'G3b zone-up DryRun:scene-manager ConfigMap 的 OwnerEpochMint.WaitAOFNumLocal / WaitAOFTimeoutMs == scene_manager_service.yaml,WaitAOFNumReplicas 按共享 Redis 形态(dev 单实例 0、prod 高可用 1)' {
    # 铸造 owner_epoch 之后的落盘确认(docs/design/zone-redis-regression-guard-20261022.md §4.3.2)。高可用形态下 NumReplicas 若停在 0,
    # 主节点宕机切主时被提升的副本上可能没有这次铸造,持有者缓存着一个 Redis 里已经不存在的 epoch,下一次存盘被拒、按废黜销毁。
    $yamlPath = 'go/scene_manager/etc/scene_manager_service.yaml'
    $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'go-svc-scene-manager-config')
    foreach ($key in @('OwnerEpochMint.WaitAOFNumLocal', 'OwnerEpochMint.WaitAOFTimeoutMs')) {
        $expected = Get-EtcValue -RelativePath $yamlPath -KeyPath $key
        Assert-True -Condition (-not [string]::IsNullOrWhiteSpace($expected)) -Because "前提:$yamlPath 必须显式写出 $key(不写 = Go 代码默认值,ConfigMap 镜像不出来)"
        Assert-Equal -Expected $expected -Actual (Get-FlatValue -Flat $flat -KeyPath "data.scene_manager_service.yaml.$key") -Because "$key 必须与 $yamlPath 一致(生成器不得自带常数)"
    }
    Assert-Equal -Expected '0' -Actual (Get-EtcValue -RelativePath $yamlPath -KeyPath 'OwnerEpochMint.WaitAOFNumReplicas') -Because "$yamlPath 的 WaitAOFNumReplicas 给本机 compose 单实例用,保持 0(写 1 本机启动探针等不到副本而拒启)"
    Assert-Equal -Expected '0' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.scene_manager_service.yaml.OwnerEpochMint.WaitAOFNumReplicas') -Because 'dev 档缺省单实例(redis.yaml),铸造的 WAITAOF 不等副本'
    $prodFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $prodRun.Output -Name 'go-svc-scene-manager-config')
    Assert-Equal -Expected '1' -Actual (Get-FlatValue -Flat $prodFlat -KeyPath 'data.scene_manager_service.yaml.OwnerEpochMint.WaitAOFNumReplicas') -Because 'prod 档缺省高可用(redis-ha.yaml),铸造的 WAITAOF 等 1 个副本(与 db、data-service 同源)'
}

# ─────────────────────────────────────────────────────────────────
# D1 玩家归属与落点映射持久化的部署载体(第 1b-3 批:docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §7、§13.4)
# ─────────────────────────────────────────────────────────────────
# 缺省产物用基线 $devOut / $prodRun / $peakAllUpRun;开关打开的产物另起 DryRun(all-up 多 zone、infra-up 带全局 Go 服务);
# 组合门禁的负向用例断错误文本;粘滞核对与读集群按 AST 抽出,给 Invoke-ClientEntryKubectl 替身(只在用例函数里定义,不影响别的节)。
# 全部不碰任何集群。
$routeSwitchFunctionNames = @(
    'Get-RouteStoreWaitAOFNumReplicas', 'Get-RouteStoreDeploySettings', 'Get-RouteSwitchParamErrors', 'ConvertFrom-RouteSwitchLiveYaml',
    'Get-RouteSwitchDrift', 'Get-RouteSwitchKeepArgs', 'Read-LiveRouteSwitchFacts', 'Assert-RouteSwitchClusterState',
    'Get-AuthoritativeScalar', 'Get-ZoneNamespace', 'Get-RedisConnectionPlan', 'Get-MysqlConnectionPlan', 'Get-RedisConnectionLedgerInputs'
)
foreach ($routeSwitchFnAst in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($routeSwitchFunctionNames -contains $routeSwitchFnAst.Name) {
        Set-Item -Path "Function:script:$($routeSwitchFnAst.Name)" -Value $routeSwitchFnAst.Body.GetScriptBlock()
    }
}
foreach ($routeSwitchFnName in $routeSwitchFunctionNames) {
    if (-not (Test-Path "Function:$routeSwitchFnName")) { throw "k8s_deploy.ps1 缺少函数 ${routeSwitchFnName}(D1 部署载体被删了?后续断言无意义)" }
}

# 全部开关打开(Mode 故意写成大小写混排:参数绑定后必须规范成 data_service 认的小写 db_first)。guild 的开关只在部署全局 Go 服务时生效,
# 这里的 all-up 带 -SkipInfra,单独由下面 infra-up 的用例测。
$RouteSwitchOnArgs = @('-RouteStoreMode', 'DB_First', '-RouteReconcileArm', '-RouteReadRequiresReady', '-DbPlacementRequireRouteReady')
$routeAllUpOnRun = Invoke-DeployDryRun -Arguments ($PeakAllUpArgs + $RouteSwitchOnArgs) -Env $ProdEnv

# DryRun 产物里某个名字的 ConfigMap 全部拍平(all-up 每个 zone 一份;Select-ManifestByName 只取第一块)。
function Get-RouteSwitchConfigMapFlats {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$Name)
    return @(Get-ManifestBlocks -Output $Output | Where-Object { $_ -match '(?m)^kind: ConfigMap\s*$' -and $_ -match ('(?m)^  name:\s*' + [regex]::Escape($Name) + '\s*$') } |
        ForEach-Object { ConvertTo-FlatManifest -Block $_ })
}

# 写路径打印的 Redis 总账一行拆成数字(部署那一份的 zones / steady / peak,与规划那一份的 peak)。
function Get-RouteSwitchRedisLedgerNumbers {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$Label)
    $lines = @($Output -split "`r?`n" | Where-Object { $_ -match '^\s*Redis connection ledger: zones=' })
    if ($lines.Count -ne 1) { throw "${Label}:应当恰好一行 Redis connection ledger(实际 $($lines.Count) 行)" }
    $m = [regex]::Match([string]$lines[0], 'zones=(?<z>\d+) steady=(?<s>\d+) peak=(?<p>\d+) .*; plan zones=\d+ peak=(?<pp>\d+) ')
    if (-not $m.Success) { throw "${Label}:Redis connection ledger 一行认不出:$($lines[0])" }
    return [pscustomobject]@{ Zones = [int]$m.Groups['z'].Value; Steady = [int]$m.Groups['s'].Value; Peak = [int]$m.Groups['p'].Value; PlanPeak = [int]$m.Groups['pp'].Value }
}

Test-Case 'D1 1b-3 缺省产物(dev / prod):RouteStore 段 db_first、read/Arm true、FenceShards == TiDB源值且在 [1, MaxRouteFenceShards]、WaitAOF 1 / 0 / 服务 yaml;Reconcile 段只写 ArmReadyMarker;db Placement.RequireRouteReady true' {
    $routeGo = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'go/shared/placement/route.go'))
    $maxMatch = [regex]::Match($routeGo, 'MaxRouteFenceShards\s+uint32\s*=\s*(\d+)')
    Assert-True -Condition $maxMatch.Success -Because 'go/shared/placement/route.go 里应当有 MaxRouteFenceShards uint32 = <N>(围栏持有者的契约)'
    $maxShards = [int]$maxMatch.Groups[1].Value
    Assert-Equal -Expected 64 -Actual $maxShards -Because 'MaxRouteFenceShards 是 64(k8s_deploy.ps1 Get-RouteStoreDeploySettings 的上界照抄它,改了要同改)'
    $tidbShards = [int](Get-EtcValue -RelativePath 'go/data_service/etc/data_service.yaml' -KeyPath 'RouteStore.FenceShards')
    Assert-Equal -Expected 64 -Actual $tidbShards -Because '当前全球TiDB围栏源为64，与完整merge分片契约一致'
    $waitTimeout = Get-EtcValue -RelativePath 'go/data_service/etc/data_service.yaml' -KeyPath 'RouteStore.WaitAOFTimeoutMs'
    # WaitAOFNumReplicas 按共享 Redis 形态:dev 档缺省单实例 0,prod 档缺省高可用 1(2026-10-19 redis-ha 接入)。
    foreach ($case in @(@{ Label = 'dev'; Output = $devOut; Replicas = '0' }, @{ Label = 'prod'; Output = $prodRun.Output; Replicas = '1' })) {
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $case.Output -Name 'go-svc-data-service-config')
        $p = 'data.data_service.yaml.RouteStore.'
        Assert-Equal -Expected 'db_first' -Actual (Get-FlatValue -Flat $flat -KeyPath "${p}Mode") -Because "$($case.Label):不带开关即强SQL默认"
        Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $flat -KeyPath "${p}ReadRequiresReady") -Because "$($case.Label):读方门禁缺省开"
        Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $flat -KeyPath "${p}Reconcile.ArmReadyMarker") -Because "$($case.Label):就绪标记缺省挂"
        $reconcileKeys = @(Get-FlatSubtreeKeys -Flat $flat -Path "${p}Reconcile" | Where-Object { $_ -cne "${p}Reconcile" })
        Assert-Equal -Expected "${p}Reconcile.ArmReadyMarker" -Actual ($reconcileKeys -join ',') -Because "$($case.Label):Reconcile 段只写 ArmReadyMarker,其余取代码缺省(RedisPoolSize 4 计入 Redis 总账)"
        $fence = [int](Get-FlatValue -Flat $flat -KeyPath "${p}FenceShards")
        Assert-True -Condition ($fence -ge 1 -and $fence -le $maxShards) -Because "$($case.Label):FenceShards=$fence 必须在 [1, $maxShards]"
        Assert-Equal -Expected $tidbShards -Actual $fence -Because "$($case.Label):全球TiDB分片数来自正规YAML唯一源"
        Assert-Equal -Expected 'tidb.invalid:4000' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.SnapshotMySQL.Host') -Because '同一明确虚构TiDB入口，不回落zone MySQL'
        Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.data_service.yaml.SnapshotMySQL.RequireTiDB') -Because '部署声明强制TiDB，实际身份由服务启动核验'
        Assert-Equal -Expected '1' -Actual (Get-FlatValue -Flat $flat -KeyPath "${p}WaitAOFNumLocal") -Because "$($case.Label):redis.yaml 开着 AOF,本地落盘确认 1"
        Assert-Equal -Expected $case.Replicas -Actual (Get-FlatValue -Flat $flat -KeyPath "${p}WaitAOFNumReplicas") -Because "$($case.Label):单实例 redis.yaml 写 0,高可用 redis-ha.yaml 写 1"
        Assert-Equal -Expected $waitTimeout -Actual (Get-FlatValue -Flat $flat -KeyPath "${p}WaitAOFTimeoutMs") -Because "$($case.Label):WAITAOF 超时镜像服务 yaml"
        $dbFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $case.Output -Name 'go-svc-db-config')
        Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $dbFlat -KeyPath 'data.db.yaml.Placement.RequireRouteReady') -Because "$($case.Label):go/db 读方门禁缺省开"
    }
    Assert-Match -Text $devOut -Pattern ('Route switches: mode=db_first arm=true read_requires_ready=true db_require_route_ready=true guild_require_route_ready=true fence_shards=' + $tidbShards + '\(tidb-required\) waitaof=1/0/' + $waitTimeout + 'ms redis_topology=single ') -Because '写路径入口打一行开关计划'
    Assert-Match -Text $prodRun.Output -Pattern ('Route switches: [^\r\n]*waitaof=1/1/' + $waitTimeout + 'ms redis_topology=ha ') -Because 'prod 档缺省高可用,开关计划行的拓扑与副本份数随之变'
    Assert-Match -Text $devOut -Pattern '\[dry-run\] Skipping D1 route switch sticky check' -Because 'DryRun 不读集群,粘滞核对只打一行说明'
}

Test-Case 'D1 1b-3 Arm 全服单值:all-up 多 zone 时每个 zone 的 data-service ConfigMap 取值相同(带开关全部 db_first / true,不带全部缺省);db 同理;-RouteStoreMode 规范成小写' {
    Assert-True -Condition ($PeakAllUpArgs -contains '-DryRun') -Because '夹具前提:all-up 必须是 DryRun'
    Assert-Equal -Expected 0 -Actual $routeAllUpOnRun.ExitCode -Because "开关全开的 all-up DryRun 应当成功(组合合法)。输出: $($routeAllUpOnRun.Output)"
    $expectedZones = @((Get-Content -LiteralPath (Join-Path (Get-RepoRoot) 'deploy/k8s/zones.sample.json') -Raw | ConvertFrom-Json).zones).Count
    Assert-True -Condition ($expectedZones -ge 2) -Because '夹具前提:zones.sample.json 至少两个 zone,才谈得上「全部 zone 同一取值」'
    foreach ($case in @(
            @{ Label = '开关全开'; Output = $routeAllUpOnRun.Output; Mode = 'db_first'; Bool = 'true' }
            @{ Label = '缺省'; Output = $peakAllUpRun.Output; Mode = 'db_first'; Bool = 'true' }
        )) {
        $dsFlats = @(Get-RouteSwitchConfigMapFlats -Output $case.Output -Name 'go-svc-data-service-config')
        Assert-Equal -Expected $expectedZones -Actual $dsFlats.Count -Because "$($case.Label):每个 zone 一份 go-svc-data-service-config"
        $tuples = @($dsFlats | ForEach-Object {
                '{0}/{1}/{2}' -f (Get-FlatValue -Flat $_ -KeyPath 'data.data_service.yaml.RouteStore.Mode'),
                (Get-FlatValue -Flat $_ -KeyPath 'data.data_service.yaml.RouteStore.Reconcile.ArmReadyMarker'),
                (Get-FlatValue -Flat $_ -KeyPath 'data.data_service.yaml.RouteStore.ReadRequiresReady')
            } | Sort-Object -Unique)
        Assert-Equal -Expected 1 -Actual $tuples.Count -Because "$($case.Label):全部 zone 的 Mode / Arm / ReadRequiresReady 必须是同一组值(实际 $($tuples -join ' | '))"
        Assert-Equal -Expected "$($case.Mode)/$($case.Bool)/$($case.Bool)" -Actual $tuples[0] -Because "$($case.Label):取值来自参数(Mode 规范成小写)"
        $dbFlats = @(Get-RouteSwitchConfigMapFlats -Output $case.Output -Name 'go-svc-db-config')
        Assert-Equal -Expected $expectedZones -Actual $dbFlats.Count -Because "$($case.Label):每个 zone 一份 go-svc-db-config"
        $dbValues = @($dbFlats | ForEach-Object { Get-FlatValue -Flat $_ -KeyPath 'data.db.yaml.Placement.RequireRouteReady' } | Sort-Object -Unique)
        Assert-Equal -Expected $case.Bool -Actual ($dbValues -join ',') -Because "$($case.Label):全部 zone 的 Placement.RequireRouteReady 同一取值"
    }
    Assert-Match -Text $routeAllUpOnRun.Output -Pattern 'Route switches: mode=db_first arm=true read_requires_ready=true db_require_route_ready=true ' -Because '计划行打出规范化之后的取值'
    Assert-NotMatch -Text $routeAllUpOnRun.Output -Pattern '\[RouteSwitch\]NoEffect:-RouteStoreMode' -Because '本次部署 zone 的 Go 服务,这几个开关有效果'
}

Test-Case 'D1 1b-3 Redis 连接总账在 db_first 时包含 leader 的独立池:data-service 行 Fixed redis 0 / db_first 4;all-up 部署行 steady 多 zones × 副本 × 4、峰值再多滚动余量;规划行恒按 db_first 计(与 MySQL 规划档同源)' {
    $RepoRoot = Get-RepoRoot
    $redisRows = @((Get-RedisConnectionLedgerInputs -RouteStoreDbFirst $false).Clients)
    $dbFirstRows = @((Get-RedisConnectionLedgerInputs -RouteStoreDbFirst $true).Clients)
    $redisDs = @($redisRows | Where-Object { $_.Name -eq 'data-service' })
    $dbFirstDs = @($dbFirstRows | Where-Object { $_.Name -eq 'data-service' })
    Assert-Equal -Expected 1 -Actual $dbFirstDs.Count -Because '客户端表里 data-service 只能有一行'
    Assert-Equal -Expected 0 -Actual $redisDs[0].Fixed -Because '纯历史redis规划输入不装配leader，不能作为真实up默认'
    Assert-Equal -Expected 4 -Actual $dbFirstDs[0].Fixed -Because 'db_first 时计 leader 的独立 go-redis 池 = Reconcile.RedisPoolSize 的代码缺省 4(ConfigMap 不写它)'
    foreach ($row in $redisRows) {
        if ($row.Name -eq 'data-service') { continue }
        $other = @($dbFirstRows | Where-Object { $_.Name -eq $row.Name })
        Assert-Equal -Expected ('{0}/{1}/{2}' -f $row.ConfiguredPoolConns, $row.Fixed, $row.DefaultPools) -Actual ('{0}/{1}/{2}' -f $other[0].ConfiguredPoolConns, $other[0].Fixed, $other[0].DefaultPools) -Because "$($row.Name) 行与 RouteStore.Mode 无关"
    }
    $poolKey = Get-YamlScalar -Path (Join-Path (Get-RepoRoot) 'go/data_service/etc/data_service.yaml') -KeyPath 'RouteStore.Reconcile.RedisPoolSize'
    if ($poolKey.Found) {
        Assert-Equal -Expected '4' -Actual ([string]$poolKey.Value) -Because '服务 yaml 写了 Reconcile.RedisPoolSize 时必须等于总账按的代码缺省 4(ConfigMap 不镜像它)'
    }
    Assert-Equal -Expected 'True/True' -Actual ('{0}/{1}' -f [bool](Get-RedisConnectionPlan).RouteStoreDbFirst, [bool](Get-MysqlConnectionPlan).RouteStoreDbFirst) -Because 'Redis 规划档按 db_first 计,取自 MySQL 规划档同一个 RouteStoreDbFirst'

    $off = Get-RouteSwitchRedisLedgerNumbers -Output $peakAllUpRun.Output -Label '缺省 all-up'
    $on = Get-RouteSwitchRedisLedgerNumbers -Output $routeAllUpOnRun.Output -Label 'db_first all-up'
    $dsReplicas = [int]$redisDs[0].Replicas
    $rollout = [int](Get-RedisConnectionPlan).RolloutExtraPodsPerDeployment
    Assert-Equal -Expected $off.Zones -Actual $on.Zones -Because '两次 all-up 数的是同一批 zone'
    Assert-Equal -Expected 0 -Actual ($on.Steady - $off.Steady) -Because '缺省与显式db_first均含leader池，稳态总账必须相同'
    Assert-Equal -Expected 0 -Actual ($on.Peak - $off.Peak) -Because '缺省与显式强SQL峰值均含相同滚动余量'
    Assert-Equal -Expected $off.PlanPeak -Actual $on.PlanPeak -Because '规划档与本次参数无关(恒按 db_first 计)'
}

Test-Case 'D1 1b-3 组合门禁:Get-RouteSwitchParamErrors 逐项;DryRun 下 Arm 配 redis 在任何 kubectl 写操作之前拒绝并点名' {
    foreach ($case in @(
            @{ Mode = 'redis'; Arm = $false; Ready = $false; Db = $false; Guild = $false; Count = 0; Pattern = '' }
            @{ Mode = 'db_first'; Arm = $false; Ready = $false; Db = $false; Guild = $false; Count = 0; Pattern = '' }
            @{ Mode = 'db_first'; Arm = $true; Ready = $false; Db = $false; Guild = $false; Count = 0; Pattern = '' }
            @{ Mode = 'db_first'; Arm = $true; Ready = $true; Db = $true; Guild = $true; Count = 0; Pattern = '' }
            @{ Mode = 'redis'; Arm = $true; Ready = $false; Db = $false; Guild = $false; Count = 1; Pattern = '-RouteReconcileArm 要求 -RouteStoreMode db_first' }
            @{ Mode = 'redis'; Arm = $false; Ready = $false; Db = $true; Guild = $false; Count = 2; Pattern = '-DbPlacementRequireRouteReady 要求 -RouteStoreMode db_first[\s\S]*-DbPlacementRequireRouteReady 要求同时给 -RouteReconcileArm' }
            @{ Mode = 'db_first'; Arm = $false; Ready = $true; Db = $false; Guild = $true; Count = 1; Pattern = '-RouteReadRequiresReady / -GuildRequireRouteReady 要求同时给 -RouteReconcileArm' }
            @{ Mode = 'redis'; Arm = $true; Ready = $true; Db = $true; Guild = $true; Count = 1; Pattern = '-RouteReconcileArm / -RouteReadRequiresReady / -DbPlacementRequireRouteReady / -GuildRequireRouteReady 要求 -RouteStoreMode db_first' }
        )) {
        $errors = @(Get-RouteSwitchParamErrors -Mode $case.Mode -ArmReadyMarker $case.Arm -ReadRequiresReady $case.Ready -DbRequireRouteReady $case.Db -GuildRequireRouteReady $case.Guild)
        $label = "Mode=$($case.Mode) Arm=$($case.Arm) Ready=$($case.Ready) Db=$($case.Db) Guild=$($case.Guild)"
        Assert-Equal -Expected $case.Count -Actual $errors.Count -Because "${label}:错误条数($($errors -join ' | '))"
        if ($case.Pattern) { Assert-Match -Text ($errors -join "`n") -Pattern $case.Pattern -Because "${label}:点名哪个开关、缺什么" }
    }
    $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-RouteStoreMode', 'redis'))
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "Arm 配 redis 模式必须拒绝(data_service 会拒启)。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'D1 映射开关的参数组合不合法,未做任何写操作' -Because '拒绝来自写路径入口的 Assert-RouteSwitchPreflight'
    Assert-Match -Text $run.Output -Pattern '要求 -RouteStoreMode db_first' -Because '错误点名不安全模式'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '组合门禁先于任何写操作'
    $calls = @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] -and $a.GetCommandName() -eq 'Assert-RouteSwitchPreflight' }, $true))
    Assert-Equal -Expected 1 -Actual $calls.Count -Because '整个脚本只在写路径门禁块里调用一次 Assert-RouteSwitchPreflight'
    $commandSwitches = @($deadlineDeployAst.EndBlock.Statements | Where-Object { $_ -is [System.Management.Automation.Language.SwitchStatementAst] -and $_.Condition.Extent.Text -match '^\(?\s*\$Command\s*\)?$' })
    Assert-True -Condition ($commandSwitches.Count -eq 1 -and $calls[0].Extent.StartOffset -lt $commandSwitches[0].Extent.StartOffset) -Because 'Assert-RouteSwitchPreflight 必须先于 switch ($Command)(写操作都在它的分支里)'
}

Test-Case 'D1 1b-3 mapping Redis 拓扑:与 Apply-Infra 选清单同源(Get-RedisTopology),不另存常量;single → WaitAOFNumReplicas 0,ha → 1,其余 throw' {
    # 2026-10-19 redis-ha 接入:以前的 $script:MappingRedisTopology 常量删掉,改由 Get-MappingRedisTopology 每次从 Get-RedisTopology 现取。
    $topologyAssign = @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -eq '$script:MappingRedisTopology' }, $true))
    Assert-Equal -Expected 0 -Actual $topologyAssign.Count -Because '不得再有 $script:MappingRedisTopology 常量:拓扑只从 Get-RedisTopology 推出,与 Apply-Infra 选清单同一处'
    $mappingFn = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Get-MappingRedisTopology' }, $true)
    Assert-True -Condition ($null -ne $mappingFn -and $mappingFn.Body.Extent.Text -match 'Get-RedisTopology\b') -Because 'Get-MappingRedisTopology 必须从 Get-RedisTopology 推出'
    # 只看 `=` 赋值(初值表);`+=` 追加的是 loki.yaml 这类条件项,与 Redis 拓扑无关。
    $infraAssign = @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.AssignmentStatementAst] -and $a.Left.Extent.Text -eq '$infraManifests' -and $a.Operator -eq [System.Management.Automation.Language.TokenKind]::Equals }, $true))
    Assert-Equal -Expected 1 -Actual $infraAssign.Count -Because 'Apply-Infra 里 $infraManifests 的初值表只能有一处'
    $manifests = $infraAssign[0].Right.Extent.Text
    Assert-True -Condition ($manifests -match '\$redisTopology\.Manifest' -and $manifests -notmatch 'redis(-ha)?\.yaml') -Because "Apply-Infra 的共享 Redis 清单只能取自 `$redisTopology.Manifest(实际 $manifests)"
    $applyInfra = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Apply-Infra' }, $true)
    Assert-Match -Text $applyInfra.Body.Extent.Text -Pattern '\$redisTopology\s*=\s*Get-RedisTopology\b' -Because 'Apply-Infra 的拓扑与 WAITAOF 渲染同一个来源'
    Assert-Equal -Expected 0 -Actual (Get-RouteStoreWaitAOFNumReplicas -Topology 'single') -Because '单实例没有副本可等'
    Assert-Equal -Expected 1 -Actual (Get-RouteStoreWaitAOFNumReplicas -Topology 'ha') -Because 'redis-ha 等一个副本落盘(哨兵切主时被提升的副本上要有这次写)'
    $message = ''
    try { Get-RouteStoreWaitAOFNumReplicas -Topology 'Single' | Out-Null } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern '不是 single / ha' -Because '拓扑名区分大小写,认不出就拒绝渲染'
    $RepoRoot = Get-RepoRoot
    $ha = Get-RouteStoreDeploySettings -Mode 'db_first' -ArmReadyMarker $true -ReadRequiresReady $false -Topology 'ha'
    Assert-Equal -Expected 'db_first/true/false/1/1' -Actual ('{0}/{1}/{2}/{3}/{4}' -f $ha.Mode, $ha.ArmReadyMarker, $ha.ReadRequiresReady, $ha.WaitAOFNumLocal, $ha.WaitAOFNumReplicas) -Because 'Get-RouteStoreDeploySettings 原样带出开关并按拓扑取副本份数'
    $message = ''
    try { Get-RouteStoreDeploySettings -Mode 'DB_FIRST' -ArmReadyMarker $false -ReadRequiresReady $false -Topology 'single' | Out-Null } catch { $message = $_.Exception.Message }
    Assert-Match -Text $message -Pattern "RouteStore\.Mode 'DB_FIRST' 不是 redis / db_first" -Because 'Mode 区分大小写(data_service 同样区分),没规范化就拒绝渲染'
}

Test-Case 'D1 1b-3 guild:infra-up 带全局 Go 服务时 MergeMarkerRedis.RequireRouteReady 缺省及显式均true(且只在该段改写一行);zone 级开关在 infra-up 上告警无效果' {
    $infraArgs = @($BattleInfraArgs | Where-Object { $_ -notin @('-SkipGoSvc', '-WaitReady') }) + @('-GoSvcRegistry', 'registry.invalid/test')
    Assert-True -Condition (($infraArgs -contains '-DryRun') -and -not ($infraArgs -contains '-SkipGoSvc')) -Because '夹具前提:infra-up 仍是 DryRun,且部署全局 Go 服务'
    $defaultRun = Invoke-DeployDryRun -Arguments $infraArgs -Env $BattleTestEnv
    $onRun = Invoke-DeployDryRun -Arguments ($infraArgs + @('-RouteStoreMode', 'db_first', '-RouteReconcileArm', '-GuildRequireRouteReady')) -Env $BattleTestEnv
    foreach ($case in @(@{ Label = '缺省'; Run = $defaultRun; Expected = 'true' }, @{ Label = '-GuildRequireRouteReady'; Run = $onRun; Expected = 'true' })) {
        Assert-Equal -Expected 0 -Actual $case.Run.ExitCode -Because "$($case.Label) 的 infra-up DryRun 应当成功。输出: $($case.Run.Output)"
        # 按 kind 过滤:guild 的 Deployment / 迁移 Job 也引用这个 ConfigMap 名,Select-ManifestByName 只按名字取第一块。
        $blocks = @(Get-ManifestBlocks -Output $case.Run.Output | Where-Object { $_ -match '(?m)^kind: ConfigMap\s*$' -and $_ -match '(?m)^  name:\s*go-svc-guild-config\s*$' })
        Assert-Equal -Expected 1 -Actual $blocks.Count -Because "$($case.Label):infra-up 带全局 Go 服务时应当恰好生成一份 go-svc-guild-config"
        $block = $blocks[0]
        $flat = ConvertTo-FlatManifest -Block $block
        Assert-Equal -Expected $case.Expected -Actual (Get-FlatValue -Flat $flat -KeyPath 'data.guild.yaml.MergeMarkerRedis.RequireRouteReady') -Because "$($case.Label):guild 读方门禁取参数"
        Assert-Equal -Expected 1 -Actual ([regex]::Matches($block, '(?m)^\s+RequireRouteReady:').Count) -Because "$($case.Label):guild ConfigMap 里 RequireRouteReady 只出现在 MergeMarkerRedis 段一处"
    }
    Assert-Match -Text $onRun.Output -Pattern '\[RouteSwitch\]NoEffect:-RouteStoreMode' -Because 'infra-up 不部署 zone 的 Go 服务,zone 级开关要告警「无效果」'
    Assert-NotMatch -Text $onRun.Output -Pattern '\[RouteSwitch\]NoEffect:-GuildRequireRouteReady' -Because 'infra-up 部署全局 Go 服务,guild 开关有效果'
    Assert-Match -Text $onRun.Output -Pattern 'guild_require_route_ready=true ' -Because '计划行打出 guild 开关'
}

# 粘滞核对夹具:被抽函数读的脚本级变量一次给全,Invoke-ClientEntryKubectl 换成按规则表回应的假函数(只活在本函数里)。
# 返回 { Message(throw 的文本,没 throw 为空);Output(Write-Host / Write-Warning 合并);Calls(kubectl 参数串)}。
function Invoke-RouteSwitchClusterCase {
    param(
        [object[]]$Rules = @(),
        [string]$Mode = 'redis', [bool]$Arm = $false, [bool]$Ready = $false, [bool]$DbRequire = $false, [bool]$GuildRequire = $false,
        [bool]$Confirm = $false, [string[]]$ZoneNames = @('contract-test'), [bool]$DeploysZoneGoSvc = $true, [bool]$DeploysGuild = $false
    )
    $RouteStoreMode = $Mode
    $RouteReconcileArm = $Arm
    $RouteReadRequiresReady = $Ready
    $DbPlacementRequireRouteReady = $DbRequire
    $GuildRequireRouteReady = $GuildRequire
    $ConfirmRouteSwitchChange = $Confirm
    $NamespacePrefix = 'mmorpg-zone'
    $InfraNamespace = 'contract-infra'
    $KubeContext = 'contract-route-ctx'
    $KubeConfig = ''
    $GoSvcCatalogue = @{
        'data-service' = @{ ConfigMap = 'go-svc-data-service-config'; ConfigFile = 'data_service.yaml' }
        db             = @{ ConfigMap = 'go-svc-db-config'; ConfigFile = 'db.yaml' }
        guild          = @{ ConfigMap = 'go-svc-guild-config'; ConfigFile = 'guild.yaml' }
    }
    $calls = [System.Collections.Generic.List[string]]::new()
    function Invoke-ClientEntryKubectl {
        param(
            [Parameter(Mandatory = $true)][string[]]$KubectlArgs,
            [Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
            [Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig,
            [switch]$AllowFailure,
            [switch]$CaptureOutput
        )
        $line = $KubectlArgs -join ' '
        $calls.Add($line)
        if ($KubeContext -cne 'contract-route-ctx') { throw "假 kubectl:没带脚本的 -KubeContext(实际 '$KubeContext'),会落到本机默认 context" }
        foreach ($rule in $Rules) {
            if ($line -match $rule.Pattern) { return [pscustomobject]@{ ExitCode = [int]$rule.ExitCode; Stdout = [string]$rule.Stdout; Stderr = [string]$rule.Stderr } }
        }
        throw "假 kubectl:没有规则命中这次调用:kubectl $line"
    }
    $message = ''
    $output = ''
    try {
        $output = (Assert-RouteSwitchClusterState -ZoneNames $ZoneNames -DeploysZoneGoSvc $DeploysZoneGoSvc -DeploysGuild $DeploysGuild 3>&1 6>&1 | Out-String -Width 4096)
    }
    catch {
        $message = $_.Exception.Message
    }
    return [pscustomobject]@{ Message = $message; Output = $output; Calls = @($calls) }
}

# 一份 data-service 的 data_service.yaml(只写本节关心的键;参数留空 = 不写那个键,看缺键按缺省)。
function New-RouteSwitchDataServiceYaml {
    param([string]$Mode = '', [string]$Arm = '', [string]$Ready = '')
    $lines = @('Name: dataservice.rpc', 'RouteStore:')
    if ($Mode) { $lines += "  Mode: $Mode" }
    $lines += '  MaxOpenConn: 20'
    if ($Ready) { $lines += "  ReadRequiresReady: $Ready" }
    if ($Arm) { $lines += '  Reconcile:', "    ArmReadyMarker: $Arm" }
    $lines += 'MetricsListenAddr: ":9260"'
    return ($lines -join "`n")
}

# kubectl get configmap -o json 的一个 ConfigMap 对象;-NoData = 没有服务 yaml 那个数据键(不是本脚本生成的形状)。
function New-RouteSwitchConfigMap {
    param([string]$Name, [string]$Namespace, [string]$ConfigFile, [AllowEmptyString()][string]$Yaml = '', [switch]$NoData)
    $configMap = [ordered]@{ apiVersion = 'v1'; kind = 'ConfigMap'; metadata = [ordered]@{ name = $Name; namespace = $Namespace } }
    if (-not $NoData) { $configMap['data'] = @{ $ConfigFile = $Yaml } }
    return $configMap
}

# 全部 namespace 的 data-service ConfigMap 列表的回应。
function New-RouteSwitchDataServiceListRule {
    param([object[]]$Items = @(), [int]$ExitCode = 0)
    $stdout = if ($ExitCode -eq 0) { [ordered]@{ apiVersion = 'v1'; kind = 'List'; items = @($Items) } | ConvertTo-Json -Depth 8 } else { '' }
    $stderr = if ($ExitCode -ne 0) { 'Error from server (Forbidden): configmaps is forbidden: User "deployer" cannot list resource "configmaps" at the cluster scope' } else { '' }
    return @{ Pattern = '^get configmap --all-namespaces --field-selector metadata\.name=go-svc-data-service-config -o json$'; ExitCode = $ExitCode; Stdout = $stdout; Stderr = $stderr }
}

# 一个 namespace 里一份 ConfigMap 的回应;-Absent = 不存在(--ignore-not-found 无输出)。
function New-RouteSwitchOneRule {
    param([string]$Name, [string]$Namespace, [string]$ConfigFile, [AllowEmptyString()][string]$Yaml = '', [switch]$Absent)
    $stdout = if ($Absent) { '' } else { (New-RouteSwitchConfigMap -Name $Name -Namespace $Namespace -ConfigFile $ConfigFile -Yaml $Yaml) | ConvertTo-Json -Depth 8 }
    return @{ Pattern = ('^get configmap ' + [regex]::Escape($Name) + ' -n ' + [regex]::Escape($Namespace) + ' --ignore-not-found -o json$'); ExitCode = 0; Stdout = $stdout; Stderr = '' }
}

Test-Case 'D1 1b-3 粘滞核对(纯函数):线上 yaml 缺键按代码缺省、认不出的写法带 ? 记下;比对逐键;照传现状的参数串' {
    $live = ConvertFrom-RouteSwitchLiveYaml -Scope 'data-service' -Text (New-RouteSwitchDataServiceYaml)
    Assert-Equal -Expected 'redis/false/false' -Actual ('{0}/{1}/{2}' -f $live.RouteStoreMode, $live.RouteReconcileArm, $live.RouteReadRequiresReady) -Because '本批之前的 ConfigMap 不写这几个键:按 data_service 的缺省(redis / false / false)'
    $live = ConvertFrom-RouteSwitchLiveYaml -Scope 'data-service' -Text (New-RouteSwitchDataServiceYaml -Mode 'db_first' -Arm 'true' -Ready 'false')
    Assert-Equal -Expected 'db_first/true/false' -Actual ('{0}/{1}/{2}' -f $live.RouteStoreMode, $live.RouteReconcileArm, $live.RouteReadRequiresReady) -Because 'Reconcile 是 RouteStore 下的嵌套段'
    Assert-Equal -Expected '-RouteStoreMode db_first -RouteReconcileArm:$true -RouteReadRequiresReady:$false' -Actual (Get-RouteSwitchKeepArgs -Live $live) -Because '新默认true下必须显式保留false，省略会改变真实live语义'
    Assert-Equal -Expected '-DbPlacementRequireRouteReady:$false -GuildRequireRouteReady:$true' -Actual (Get-RouteSwitchKeepArgs -Live ([ordered]@{ DbPlacementRequireRouteReady = 'false'; GuildRequireRouteReady = 'true' })) -Because '所有bool均显式复现，不因新默认替旧live补权'
    $weird = ConvertFrom-RouteSwitchLiveYaml -Scope 'data-service' -Text (New-RouteSwitchDataServiceYaml -Mode 'DB_FIRST' -Arm 'True')
    Assert-Equal -Expected 'DB_FIRST?/True?' -Actual ('{0}/{1}' -f $weird.RouteStoreMode, $weird.RouteReconcileArm) -Because '认不出的写法原样带 ? 记下(data_service 区分大小写,看不懂不等于没开)'
    Assert-True -Condition ($null -eq (Get-RouteSwitchKeepArgs -Live $weird)) -Because '线上取值认不出时给不出照传参数串'
    $db = ConvertFrom-RouteSwitchLiveYaml -Scope 'db' -Text "Placement:`n  ZoneStoreMaxOpenConn: 8`n  RequireRouteReady: true"
    Assert-Equal -Expected 'true' -Actual $db.DbPlacementRequireRouteReady -Because 'db 的开关在 Placement 段'
    Assert-Equal -Expected 'false' -Actual (ConvertFrom-RouteSwitchLiveYaml -Scope 'guild' -Text "MergeMarkerRedis:`n  Host: redis:6379").GuildRequireRouteReady -Because 'guild 缺键按 false'
    $drifts = @(Get-RouteSwitchDrift -Live $live -Desired ([ordered]@{ RouteStoreMode = 'redis'; RouteReconcileArm = 'true'; RouteReadRequiresReady = 'false'; Unrelated = 'x' }))
    Assert-Equal -Expected '-RouteStoreMode 线上 db_first,本次 redis' -Actual ($drifts -join ' | ') -Because '只报不一致的键;-Desired 里多出来的键不比'
}

Test-Case 'D1 1b-3 粘滞核对:读全部 zone 的 data-service(含本 zone 已被删掉重建的回档场景)与本 zone 的 db;不一致且没给 -ConfirmRouteSwitchChange 即拒绝并给出照传参数,给了只告警;一致放行' {
    $dbFirstYaml = New-RouteSwitchDataServiceYaml -Mode 'db_first' -Arm 'true'
    $items = @(
        New-RouteSwitchConfigMap -Name 'go-svc-data-service-config' -Namespace 'mmorpg-zone-z2' -ConfigFile 'data_service.yaml' -Yaml $dbFirstYaml
        # 别的前缀部署的 namespace 不认(与 Read-PlayerInboxGateLiveFacts 同一口径)。
        New-RouteSwitchConfigMap -Name 'go-svc-data-service-config' -Namespace 'other-zone-x' -ConfigFile 'data_service.yaml' -Yaml (New-RouteSwitchDataServiceYaml -Mode 'redis')
    )
    # 本 zone(contract-test)的 namespace 不在:zone 回档 Step 1 删掉了它,Step 6 重建 —— 只读它自己就核对不出来。
    $rules = @(
        (New-RouteSwitchDataServiceListRule -Items $items)
        (New-RouteSwitchOneRule -Name 'go-svc-db-config' -Namespace 'mmorpg-zone-contract-test' -ConfigFile 'db.yaml' -Absent)
    )
    $refused = Invoke-RouteSwitchClusterCase -Rules $rules
    Assert-Match -Text $refused.Message -Pattern 'D1 映射开关的线上现状与本次参数不一致\(1 处\),未做任何写操作' -Because '缺省参数部署到已是 db_first 的集群:拒绝(防止静默降回 redis)'
    Assert-Match -Text $refused.Message -Pattern 'data-service\(namespace=mmorpg-zone-z2\)' -Because '点名是哪个 zone 的现状'
    Assert-Match -Text $refused.Message -Pattern '-RouteStoreMode 线上 db_first,本次 redis' -Because '逐键列出不一致'
    Assert-Match -Text $refused.Message -Pattern '-RouteReconcileArm 线上 true,本次 false' -Because '逐键列出不一致'
    Assert-Match -Text $refused.Message -Pattern '线上现状对应的参数是 -RouteStoreMode db_first -RouteReconcileArm:\$true' -Because '给出线上现状对应的参数串(布尔值显式写出)'
    Assert-NotMatch -Text $refused.Message -Pattern '要保持现状请照传|布尔开关不给 = false' -Because '全球默认 true 之后「不给 = false」不成立,入口闸也不许照传 redis / false 保持现状(review OPS-9)'
    Assert-Match -Text $refused.Message -Pattern '-ConfirmRouteSwitchChange' -Because '点名确认开关'
    Assert-NotMatch -Text $refused.Message -Pattern 'other-zone-x' -Because '只认 -NamespacePrefix 下的 zone'
    Assert-Equal -Expected 2 -Actual $refused.Calls.Count -Because '读一次全部 namespace 的 data-service,再读本 zone 的 db'

    $same = Invoke-RouteSwitchClusterCase -Rules $rules -Mode 'db_first' -Arm $true
    Assert-Equal -Expected '' -Actual $same.Message -Because '照传现状:放行'
    Assert-Match -Text $same.Output -Pattern 'Route switch live state: data_service_zones=1 db_zones=0 guild=0 drifts_refused=0 ' -Because '放行时打一行现状摘要'

    $confirmed = Invoke-RouteSwitchClusterCase -Rules $rules -Confirm $true
    Assert-Equal -Expected '' -Actual $confirmed.Message -Because '给了 -ConfirmRouteSwitchChange:放行'
    Assert-Match -Text $confirmed.Output -Pattern '\[RouteSwitch\]Confirmed:-ConfirmRouteSwitchChange supplied; allowed: data-service\(namespace=mmorpg-zone-z2\)' -Because '放行但留一条告警'

    $dbRules = @(
        (New-RouteSwitchDataServiceListRule -Items @())
        (New-RouteSwitchOneRule -Name 'go-svc-db-config' -Namespace 'mmorpg-zone-contract-test' -ConfigFile 'db.yaml' -Yaml "Placement:`n  RequireRouteReady: true")
    )
    $dbRefused = Invoke-RouteSwitchClusterCase -Rules $dbRules
    Assert-Match -Text $dbRefused.Message -Pattern 'db\(namespace=mmorpg-zone-contract-test\):-DbPlacementRequireRouteReady 线上 true,本次 false' -Because 'db 的读方开关逐 zone 粘滞:漏传会把已打开的门禁静默关掉'
    Assert-Match -Text $dbRefused.Message -Pattern '线上现状对应的参数是 -DbPlacementRequireRouteReady:\$true' -Because '给出线上现状对应的参数'

    $weirdRules = @((New-RouteSwitchDataServiceListRule -Items @(New-RouteSwitchConfigMap -Name 'go-svc-data-service-config' -Namespace 'mmorpg-zone-z2' -ConfigFile 'data_service.yaml' -Yaml (New-RouteSwitchDataServiceYaml -Mode 'DB_FIRST'))), (New-RouteSwitchOneRule -Name 'go-svc-db-config' -Namespace 'mmorpg-zone-contract-test' -ConfigFile 'db.yaml' -Absent))
    $weirdRefused = Invoke-RouteSwitchClusterCase -Rules $weirdRules
    Assert-Match -Text $weirdRefused.Message -Pattern '线上取值认不出' -Because '认不出的线上写法 fail-closed'
}

Test-Case 'D1 1b-3 粘滞核对 fail-closed:列 ConfigMap 失败、ConfigMap 没有服务 yaml 数据键都拒绝;guild 只在部署全局服务时读,infra-up 打开 guild 开关时另核全服 data-service 的 Mode / Arm(不比 ReadRequiresReady)' {
    $failed = Invoke-RouteSwitchClusterCase -Rules @((New-RouteSwitchDataServiceListRule -ExitCode 1))
    Assert-Match -Text $failed.Message -Pattern '读取各 zone 线上的 ConfigMap go-svc-data-service-config 失败' -Because 'kubectl 失败按 fail-closed 处理'
    Assert-Match -Text $failed.Message -Pattern '尚未做任何写操作' -Because '在任何写操作之前'

    $noData = Invoke-RouteSwitchClusterCase -Rules @((New-RouteSwitchDataServiceListRule -Items @(New-RouteSwitchConfigMap -Name 'go-svc-data-service-config' -Namespace 'mmorpg-zone-z2' -ConfigFile 'data_service.yaml' -NoData)))
    Assert-Match -Text $noData.Message -Pattern '不是本脚本生成的形状' -Because 'ConfigMap 在而数据键不在:判不出,拒绝'

    $guildRules = @((New-RouteSwitchOneRule -Name 'go-svc-guild-config' -Namespace 'contract-infra' -ConfigFile 'guild.yaml' -Yaml "MergeMarkerRedis:`n  RequireRouteReady: true"))
    $guildRefused = Invoke-RouteSwitchClusterCase -Rules $guildRules -ZoneNames @() -DeploysZoneGoSvc $false -DeploysGuild $true
    Assert-Match -Text $guildRefused.Message -Pattern 'guild\(namespace=contract-infra\):-GuildRequireRouteReady 线上 true,本次 false' -Because 'infra-up 漏传 guild 开关会把已打开的门禁关掉'
    Assert-Equal -Expected 1 -Actual $guildRefused.Calls.Count -Because 'guild 开关不打开时 infra-up 只读 guild 自己,不读 data-service'

    $dsRedisReady = New-RouteSwitchDataServiceYaml -Mode 'db_first' -Arm 'true' -Ready 'true'
    $preconditionRules = @(
        (New-RouteSwitchOneRule -Name 'go-svc-guild-config' -Namespace 'contract-infra' -ConfigFile 'guild.yaml' -Absent)
        (New-RouteSwitchDataServiceListRule -Items @(New-RouteSwitchConfigMap -Name 'go-svc-data-service-config' -Namespace 'mmorpg-zone-z2' -ConfigFile 'data_service.yaml' -Yaml $dsRedisReady))
    )
    $ok = Invoke-RouteSwitchClusterCase -Rules $preconditionRules -Mode 'db_first' -Arm $true -GuildRequire $true -ZoneNames @() -DeploysZoneGoSvc $false -DeploysGuild $true
    Assert-Equal -Expected '' -Actual $ok.Message -Because '全服 data-service 已是 db_first + Arm=true:放行;线上 ReadRequiresReady=true 与本次不同也不比(infra-up 不渲染 data-service)'
    Assert-Equal -Expected 2 -Actual $ok.Calls.Count -Because 'infra-up 打开 guild 开关时另读全部 zone 的 data-service'
    $notReady = @(
        (New-RouteSwitchOneRule -Name 'go-svc-guild-config' -Namespace 'contract-infra' -ConfigFile 'guild.yaml' -Absent)
        (New-RouteSwitchDataServiceListRule -Items @(New-RouteSwitchConfigMap -Name 'go-svc-data-service-config' -Namespace 'mmorpg-zone-z2' -ConfigFile 'data_service.yaml' -Yaml (New-RouteSwitchDataServiceYaml)))
    )
    $refused = Invoke-RouteSwitchClusterCase -Rules $notReady -Mode 'db_first' -Arm $true -GuildRequire $true -ZoneNames @() -DeploysZoneGoSvc $false -DeploysGuild $true
    Assert-Match -Text $refused.Message -Pattern '-RouteStoreMode 线上 redis,本次 db_first' -Because 'guild 读方的前提是全服真的已 db_first:参数里声明了不算数'
}

# review OPS-2(a) / B3-2:-ConfirmRouteSwitchChange 只放行「本次要改的值」;读方前提(本次渲染了读方开关时,本次不部署的 zone 的
# data-service 必须已是 db_first + Arm=true)不受它豁免。2026-10-21 起 k8s_deploy 的 D1 缺省是 db_first / 全 true(全球默认),
# zone-up / all-up / infra-up 每次都会渲染读方,所以这条前提在每次真实部署上都生效:不能误拒已切好的集群,也不能放过半切的集群。
Test-Case 'D1 读方前提不受 -ConfirmRouteSwitchChange 豁免:本次不部署的 zone 必须已是 db_first + Arm=true;本次部署的 zone 自己、已切好的集群、全新集群都放行' {
    $ds = {
        param([string]$Namespace, [string]$Mode = '', [string]$Arm = '', [string]$Ready = '')
        New-RouteSwitchConfigMap -Name 'go-svc-data-service-config' -Namespace $Namespace -ConfigFile 'data_service.yaml' -Yaml (New-RouteSwitchDataServiceYaml -Mode $Mode -Arm $Arm -Ready $Ready)
    }
    $dbAbsent = New-RouteSwitchOneRule -Name 'go-svc-db-config' -Namespace 'mmorpg-zone-contract-test' -ConfigFile 'db.yaml' -Absent
    # 全球默认:zone-up 渲染 db_first + Arm + 两个读方。
    $on = @{ Mode = 'db_first'; Arm = $true; Ready = $true; DbRequire = $true }

    # (a) infra-up 第一次打开 guild:guild 线上显式 false(必有漂移、必须带 Confirm),而 zone 的 data-service 还是 redis。
    $guildFirstOpen = @(
        (New-RouteSwitchOneRule -Name 'go-svc-guild-config' -Namespace 'contract-infra' -ConfigFile 'guild.yaml' -Yaml "MergeMarkerRedis:`n  RequireRouteReady: false")
        (New-RouteSwitchDataServiceListRule -Items @((& $ds 'mmorpg-zone-z2')))
    )
    $r = Invoke-RouteSwitchClusterCase -Rules $guildFirstOpen -Mode 'db_first' -Arm $true -GuildRequire $true -Confirm $true -ZoneNames @() -DeploysZoneGoSvc $false -DeploysGuild $true
    Assert-Match -Text $r.Message -Pattern 'D1 读方开关的前提不满足\(1 个 zone\),-ConfirmRouteSwitchChange 不豁免这一条' -Because '以前前提核对与 guild 自己的漂移走同一个 Confirm 分支,第一次打开 guild 时必带 Confirm,前提也就跟着被放行'
    Assert-Match -Text $r.Message -Pattern 'data-service\(namespace=mmorpg-zone-z2\):[^\n]*线上 RouteStore\.Mode=redis、Reconcile\.ArmReadyMarker=false[^\n]*本次要打开 -GuildRequireRouteReady' -Because '点名哪个 zone、线上是什么、本次要开哪个读方'
    Assert-NotMatch -Text $r.Message -Pattern 'guild\(namespace=contract-infra\)' -Because 'guild 自己由 false 改 true 是本次要改的值,带了 Confirm 就不进拒绝清单'
    $r = Invoke-RouteSwitchClusterCase -Rules $guildFirstOpen -Mode 'db_first' -Arm $true -GuildRequire $true -ZoneNames @() -DeploysZoneGoSvc $false -DeploysGuild $true
    Assert-Match -Text $r.Message -Pattern '另有线上现状与本次参数不一致\(1 处\):[\s\S]*guild\(namespace=contract-infra\):-GuildRequireRouteReady 线上 false,本次 true' -Because '不带 Confirm 时前提与漂移一次报全'

    # (b) zone-up 一个 zone,其它 zone 一个是 db_first 但 Arm=false、一个还是 redis:带 Confirm 也拒绝,提示改用 all-up。
    $halfSwitched = @(
        (New-RouteSwitchDataServiceListRule -Items @((& $ds 'mmorpg-zone-z2' 'db_first' 'false'), (& $ds 'mmorpg-zone-z3' 'redis')))
        $dbAbsent
    )
    $r = Invoke-RouteSwitchClusterCase -Rules $halfSwitched @on -Confirm $true
    Assert-Match -Text $r.Message -Pattern 'D1 读方开关的前提不满足\(2 个 zone\)' -Because 'z2 的副本当选 leader 就撤标记(M4),本 zone 的 go/db 随即暂停存盘消费;z3 不挂标记'
    Assert-Match -Text $r.Message -Pattern 'data-service\(namespace=mmorpg-zone-z2\):[^\n]*-RouteReconcileArm 线上 false,本次 true[^\n]*本次要打开 -RouteReadRequiresReady / -DbPlacementRequireRouteReady' -Because '点名 Arm 未开的 zone'
    Assert-Match -Text $r.Message -Pattern 'data-service\(namespace=mmorpg-zone-z3\):[^\n]*线上 RouteStore\.Mode=redis' -Because '点名仍是 redis 的 zone'
    Assert-Match -Text $r.Message -Pattern '改用 all-up 一次部署全部 zone' -Because '多 zone 首次切换只能一次切全部 zone'

    # (c) all-up 覆盖全部 zone:每个 zone 都是本次渲染的,线上旧值只是本次要改的值,Confirm 放行。
    $allZones = @(
        (New-RouteSwitchDataServiceListRule -Items @((& $ds 'mmorpg-zone-z2' 'db_first' 'false')))
        $dbAbsent
        (New-RouteSwitchOneRule -Name 'go-svc-db-config' -Namespace 'mmorpg-zone-z2' -ConfigFile 'db.yaml' -Absent)
    )
    $r = Invoke-RouteSwitchClusterCase -Rules $allZones @on -Confirm $true -ZoneNames @('contract-test', 'z2')
    Assert-Equal -Expected '' -Actual $r.Message -Because 'all-up 一次渲染全部 zone:不违反前提'
    Assert-Match -Text $r.Output -Pattern 'precondition_refused=0' -Because '摘要行带前提拒绝数'
    Assert-Match -Text $r.Output -Pattern '\[RouteSwitch\]Confirmed:[^\r\n]*data-service\(namespace=mmorpg-zone-z2\)' -Because '本次要改的值仍按漂移告警'

    # (d) 单 zone 升级:唯一的 data-service 就是本次部署的 zone,不算前提;漂移照常要 Confirm。
    $selfOnly = @((New-RouteSwitchDataServiceListRule -Items @((& $ds 'mmorpg-zone-contract-test'))), $dbAbsent)
    $r = Invoke-RouteSwitchClusterCase -Rules $selfOnly @on -Confirm $true
    Assert-Equal -Expected '' -Actual $r.Message -Because '本次部署的 zone 自己的旧值是本次要改的值'
    $r = Invoke-RouteSwitchClusterCase -Rules $selfOnly @on
    Assert-Match -Text $r.Message -Pattern 'D1 映射开关的线上现状与本次参数不一致' -Because '没带 Confirm 按漂移拒绝'
    Assert-NotMatch -Text $r.Message -Pattern '前提不满足' -Because '不是前提问题'

    # (e) 已切好的集群(全部 db_first / true):不带 Confirm 直接放行,不误拒。
    $switched = @((New-RouteSwitchDataServiceListRule -Items @((& $ds 'mmorpg-zone-z2' 'db_first' 'true' 'true'))), $dbAbsent)
    $r = Invoke-RouteSwitchClusterCase -Rules $switched @on
    Assert-Equal -Expected '' -Actual $r.Message -Because '全球默认部署到已切好的集群必须放行'
    Assert-Match -Text $r.Output -Pattern 'drifts_refused=0 [^\r\n]*precondition_refused=0' -Because '没有漂移、没有前提问题'

    # (f) 全新集群 infra-up 先于任何 zone:一个 data-service 都没有,不算违反(guild 在第一个 zone 挂标记之前 fail-closed,不写错)。
    $fresh = @(
        (New-RouteSwitchOneRule -Name 'go-svc-guild-config' -Namespace 'contract-infra' -ConfigFile 'guild.yaml' -Absent)
        (New-RouteSwitchDataServiceListRule -Items @())
    )
    $r = Invoke-RouteSwitchClusterCase -Rules $fresh -Mode 'db_first' -Arm $true -GuildRequire $true -ZoneNames @() -DeploysZoneGoSvc $false -DeploysGuild $true
    Assert-Equal -Expected '' -Actual $r.Message -Because '全新集群先 infra-up 再 zone-up 是常规流程,不能误拒'
}

Test-Case 'D1 调用点与参数一致:k8s_deploy.ps1 对本节函数的每次调用只用被调函数 param 块声明过的参数名' {
    # 2026-10-20 Read-LiveRouteSwitchFacts 的 -DbZoneNames 改成 -ReadDb 而调用点没跟上:每次非 DryRun 部署 Go 服务都在参数绑定处 throw,
    # DryRun 不走粘滞核对,产物用例全绿。这里按 AST 钉住调用点与 param 块逐字一致(PowerShell 的参数名缩写也不许用)。
    $d1Functions = @('Get-RouteStoreWaitAOFNumReplicas', 'Get-RouteStoreDeploySettings', 'Get-RouteSwitchParamErrors', 'ConvertFrom-RouteSwitchLiveYaml',
        'Get-RouteSwitchDrift', 'Get-RouteSwitchKeepArgs', 'Read-LiveRouteSwitchFacts', 'Assert-RouteSwitchClusterState')
    $declared = @{}
    foreach ($fn in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true)) {
        if ($d1Functions -notcontains $fn.Name) { continue }
        $names = @()
        if ($null -ne $fn.Body.ParamBlock) { $names = @($fn.Body.ParamBlock.Parameters | ForEach-Object { $_.Name.VariablePath.UserPath }) }
        $declared[$fn.Name] = $names
    }
    Assert-Equal -Expected $d1Functions.Count -Actual $declared.Count -Because '夹具前提:本节函数都在 k8s_deploy.ps1 里'
    $checked = 0
    foreach ($call in $deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true)) {
        $name = $call.GetCommandName()
        if ([string]::IsNullOrEmpty($name) -or -not $declared.ContainsKey($name)) { continue }
        foreach ($element in @($call.CommandElements | Where-Object { $_ -is [System.Management.Automation.Language.CommandParameterAst] })) {
            Assert-True -Condition ($declared[$name] -contains $element.ParameterName) -Because "第 $($call.Extent.StartLineNumber) 行调用 $name 给了 -$($element.ParameterName),它的 param 块没有这个参数(声明的是 $($declared[$name] -join ' / '))"
            $checked++
        }
    }
    Assert-True -Condition ($checked -ge 10) -Because "夹具前提:应当核到本节函数的全部命名参数(实际 $checked 个)"
    $readCalls = @($deadlineDeployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] -and $a.GetCommandName() -eq 'Read-LiveRouteSwitchFacts' }, $true))
    Assert-Equal -Expected 1 -Actual $readCalls.Count -Because '线上现状只在 Assert-RouteSwitchClusterState 里读一次'
}

Test-Case 'D1 1b-3 mapping Redis 启动参数:redis.yaml 的两条 exec、redis-ha.yaml 的 redis-server、docker-compose 的 redis 都显式 --enable-debug-command no' {
    $redisYaml = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/infra/redis.yaml')) -replace "`r`n", "`n"
    $execLines = @([regex]::Matches($redisYaml, '(?m)^[ \t]*exec redis-server [^\n]*$') | ForEach-Object { $_.Value })
    Assert-True -Condition ($execLines.Count -ge 2) -Because "redis.yaml 有口令、无口令两条 exec redis-server(实际 $($execLines.Count) 条)"
    foreach ($line in $execLines) {
        Assert-Match -Text $line -Pattern '--enable-debug-command[ \t]+no(?:[ \t]|$)' -Because "每一条 exec 都要关掉 DEBUG(DEBUG RELOAD 在进程内替换数据而 run_id / replid 不变,读方的身份判据认不出,规格 §13.1 第 5 条):$($line.Trim())"
    }
    $haYaml = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'deploy/k8s/manifests/infra/redis-ha.yaml')) -replace "`r`n", "`n"
    $haStart = [regex]::Match($haYaml, '(?ms)^[ \t]*set -- redis-server \\\n(?<args>(?:[^\n]*\\\n)*[^\n]*)$')
    Assert-True -Condition $haStart.Success -Because 'redis-ha.yaml 的启动脚本里应当有 set -- redis-server \ 续行的参数表'
    Assert-Match -Text $haStart.Groups['args'].Value -Pattern '--enable-debug-command no' -Because 'redis-ha 的每个节点同样关掉 DEBUG(被提升的副本也会成为 mapping Redis)'
    $compose = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'deploy/docker-compose.yml')) -replace "`r`n", "`n"
    $redisService = [regex]::Match($compose, '(?ms)^  redis:\n(?<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\z)')
    Assert-True -Condition $redisService.Success -Because 'docker-compose.yml 里应当有 redis 服务'
    Assert-Match -Text $redisService.Groups['body'].Value -Pattern '(?m)^\s+--enable-debug-command no\s*$' -Because '本机 compose 的 redis 同样是 mapping Redis'
}

# ── 告警引用的指标名与标签取值从 Go 源码核对(D1 映射与落点告警;照 mail-alerts / rank-alerts 用例读 metrics.go 的做法)──
# 改名后告警表达式恒为空、不报错,与「一切正常」长得一样;手写名单拦不住(review B3-6 / OPS-7)。读到的 Go 文件同步列在
# .github/workflows/deploy-config-tests.yml 的 paths 里,只改这些 Go 文件也会重跑本用例。

# 一个 metrics 包里注册的指标全名:`const subsystem = "..."` + 每个 `Name: "..."`(prometheus.*Opts,包内都写 Subsystem: subsystem)。
# 读目录下全部非 _test.go 的 .go 文件(go/db 的指标分在 metrics.go / pipeline.go / task_result_notify.go 里)。换了写法要同步本函数。
function Get-GoSubsystemMetricNames {
    param([Parameter(Mandatory = $true)][string]$RelativeDir)
    $dir = Join-Path (Get-RepoRoot) $RelativeDir
    $sources = @(Get-ChildItem -LiteralPath $dir -Filter '*.go' -File | Where-Object { $_.Name -notlike '*_test.go' } | Sort-Object Name |
            ForEach-Object { [IO.File]::ReadAllText($_.FullName) })
    $subsystems = @($sources | ForEach-Object { [regex]::Matches($_, '(?m)^const subsystem = "(?<s>[a-z_]+)"') } | ForEach-Object { $_.Groups['s'].Value } | Sort-Object -Unique)
    if ($subsystems.Count -ne 1) { throw "$RelativeDir 里应当恰好一个 const subsystem = `"...`"(实际 $($subsystems -join ',')),换了写法要同步 Get-GoSubsystemMetricNames" }
    return @($sources | ForEach-Object { [regex]::Matches($_, '(?m)^\s*Name:\s*"(?<n>[a-z0-9_]+)"') } | ForEach-Object { $subsystems[0] + '_' + $_.Groups['n'].Value } | Sort-Object -Unique)
}

# 一个 Go 文件里以 -Prefix 开头的字符串常量的取值(`<Prefix>Xxx = "value"`,const 块里对齐写法也认)。
function Get-GoPrefixedStringConsts {
    param([Parameter(Mandatory = $true)][string]$RelativePath, [Parameter(Mandatory = $true)][string]$Prefix)
    $source = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) $RelativePath))
    $values = @([regex]::Matches($source, ('(?m)^\s*{0}[A-Za-z0-9]*\s*=\s*"(?<v>[a-z0-9_]+)"' -f [regex]::Escape($Prefix))) | ForEach-Object { $_.Groups['v'].Value } | Sort-Object -Unique)
    if ($values.Count -eq 0) { throw "$RelativePath 里找不到以 $Prefix 开头的字符串常量(改名了?要同步本用例)" }
    return $values
}

# 三个读方共用的 verdict 取值:shared/placement 的 7 个名字(routeReadyVerdictNames 数组字面量)加 script_error(data_service 的常量)。
function Get-RouteReadyVerdictLabelValues {
    $source = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'go/shared/placement/route_ready.go'))
    $table = [regex]::Match($source, '(?s)routeReadyVerdictNames\s*=\s*\[routeReadyVerdictCount\]string\{(?<body>[^}]*)\}')
    if (-not $table.Success) { throw 'go/shared/placement/route_ready.go 里找不到 routeReadyVerdictNames 的数组字面量(换了写法要同步本用例)' }
    $names = @([regex]::Matches($table.Groups['body'].Value, '"(?<v>[a-z_]+)"') | ForEach-Object { $_.Groups['v'].Value })
    return @($names + @(Get-GoPrefixedStringConsts -RelativePath 'go/data_service/internal/metrics/metrics.go' -Prefix 'RouteReadyVerdictScriptError'))
}

# 告警文件里全部 expr 块拼成一段文本(`expr: |` 到同一规则的 `for:` 之前)。
function Get-AlertExprText {
    param([Parameter(Mandatory = $true)][string]$Text)
    return (@([regex]::Matches($Text, '(?ms)^          expr: \|\n(?<e>.*?)(?=^          for:)') | ForEach-Object { $_.Groups['e'].Value }) -join "`n")
}

# 表达式里 <Metric>{...} 选择器中 <Label> 的每个匹配都要落在 -Allowed 里:= / != 的字面量必须是其中之一;=~ / !~ 是纯 | 分隔的字面量时
# 逐个必须是其中之一,否则(如 freeze_.*)至少要匹配到一个取值。写错取值的表达式不报错,只会永远不响(或 unless / != 永远不过滤)。
function Assert-AlertLabelValues {
    param(
        [Parameter(Mandatory = $true)][string]$Exprs,
        [Parameter(Mandatory = $true)][string]$Metric,
        [Parameter(Mandatory = $true)][string]$Label,
        [Parameter(Mandatory = $true)][string[]]$Allowed,
        [Parameter(Mandatory = $true)][string]$Source
    )
    $selectors = @([regex]::Matches($Exprs, ('\b{0}\{{(?<sel>[^}}]*)\}}' -f [regex]::Escape($Metric))) | ForEach-Object { $_.Groups['sel'].Value })
    foreach ($selector in $selectors) {
        foreach ($m in [regex]::Matches($selector, ('\b{0}\s*(?<op>=~|!~|!=|=)\s*"(?<v>[^"]*)"' -f [regex]::Escape($Label)))) {
            $op = $m.Groups['op'].Value
            $value = $m.Groups['v'].Value
            $matcher = '{0}{{{1}{2}"{3}"}}' -f $Metric, $Label, $op, $value
            $allowedText = $Allowed -join ' | '
            if ($op -in @('=', '!=') -or $value -match '^[a-z0-9_]+(\|[a-z0-9_]+)*$') {
                foreach ($literal in ($value -split '\|')) {
                    Assert-True -Condition ($Allowed -ccontains $literal) -Because ('{0}:取值 {1} 必须是 {2} 定义的常量之一({3})' -f $matcher, $literal, $Source, $allowedText)
                }
            }
            else {
                $hits = @($Allowed | Where-Object { $_ -cmatch ('^(?:' + $value + ')$') })
                Assert-True -Condition ($hits.Count -gt 0) -Because ('{0}:正则至少要匹配到 {1} 的一个取值({2})' -f $matcher, $Source, $allowedText)
            }
        }
    }
}

Test-Case 'D1 1b-3 告警 data-service-route-alerts.yaml:PrometheusRule 形状、interval 30s、每条规则有 severity / summary / description;规格点名的规则都在;NoLeader 兜住缺序列,ReadyMissing 用 max(arm_enabled);抓取前提写在文件头' {
    $path = 'deploy/k8s/data-service-route-alerts.yaml'
    $text = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) $path)) -replace "`r`n", "`n"
    Assert-Match -Text $text -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 owner-epoch-alerts.yaml 同一安装方式)'
    $intervals = @([regex]::Matches($text, '(?m)^      interval:[ ]*(\S+)[ ]*$') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
    Assert-Equal -Expected '30s' -Actual ($intervals -join ',') -Because '每个组 interval: 30s'
    $alertNames = @([regex]::Matches($text, '(?m)^        - alert:[ ]*(\S+)[ ]*$') | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in @('DataServiceRouteNoLeader', 'DataServiceRouteMultiLeader', 'DataServiceRouteArmMixed', 'DataServiceRouteReadyMissing', 'DataServiceRouteWritesFrozen',
            'DataServiceRouteRollbackOrCacheAhead', 'DataServiceRouteArmingRefused', 'DataServiceRouteForeignFence', 'DataServiceRoutePushConflicts',
            'DataServiceRouteCacheVerifyStale', 'GoDbRouteGateClosed', 'DataServiceRouteReadSQLErrors', 'GuildRouteNotReady',
            # 规格 §7 第 2 条的 RouteNotReadyDeferrals、leader 让出租约;评审补的读方持续不可信(A2-6)与 W1 发布 / 登记失败(B3-9)。
            'GoDbRouteNotReadyDeferrals', 'DataServiceRouteLeaderRedisUnreachable', 'DataServiceRouteReadNotTrusted',
            'DataServiceRoutePublishUnconfirmed', 'DataServiceRouteRegisterFailures')) {
        Assert-True -Condition ($alertNames -contains $name) -Because "规格 §7 / §13.4 点名(或评审补上)的告警 $name 必须在"
    }
    Assert-Equal -Expected $alertNames.Count -Actual (@($alertNames | Sort-Object -Unique).Count) -Because '告警名不重复'
    $rules = @(($text -split '(?m)^        - alert:') | Select-Object -Skip 1)
    Assert-Equal -Expected $alertNames.Count -Actual $rules.Count -Because '按 - alert: 切出的规则块数等于告警数'
    foreach ($rule in $rules) {
        $ruleName = ($rule -split "`n")[0].Trim()
        Assert-Match -Text $rule -Pattern '(?m)^            severity:[ ]*(critical|warning|info)[ ]*$' -Because "$ruleName 要有 severity(critical / warning / info)"
        Assert-Match -Text $rule -Pattern '(?m)^            summary:' -Because "$ruleName 要有 summary"
        Assert-Match -Text $rule -Pattern '(?m)^            description:' -Because "$ruleName 要有 description"
        Assert-Match -Text $rule -Pattern '(?m)^          for:[ ]*\d+[ms][ ]*$' -Because "$ruleName 要写 for"
    }
    $noLeader = $rules | Where-Object { $_ -match '^\s*DataServiceRouteNoLeader\b' }
    Assert-Match -Text $noLeader -Pattern ([regex]::Escape('(sum(data_service_route_reconcile_leader) or vector(0)) == 0 and sum(data_service_route_reconcile_assembled) > 0')) -Because 'NoLeader 用 or vector(0) 兜住「一个序列都没有」,并按装配过滤(规格 §13.4 第 2 条)'
    $readyMissing = $rules | Where-Object { $_ -match '^\s*DataServiceRouteReadyMissing\b' }
    Assert-Match -Text $readyMissing -Pattern ([regex]::Escape('max(data_service_route_reconcile_arm_enabled) == 1 and (max(data_service_route_ready_marker) or vector(0)) == 0')) -Because 'ReadyMissing 用 max(arm_enabled) 作闸,ready_marker 缺序列按不在'
    # 指标名从源码拼全名(不再手写名单):data_service / go/db 读 metrics 包的 subsystem + Name,guild 读 merge_fence.go 的 Namespace / Subsystem / Name。
    $registered = @(Get-GoSubsystemMetricNames -RelativeDir 'go/data_service/internal/metrics') + @(Get-GoSubsystemMetricNames -RelativeDir 'go/db/internal/metrics')
    $guildSource = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'go/guild/internal/logic/merge_fence.go'))
    $guildMetric = [regex]::Match($guildSource, 'guildRouteReadyVerdictTotal\s*=\s*metric\.NewCounterVec\(&metric\.CounterVecOpts\{\s*Namespace:\s*"(?<ns>[a-z_]+)",\s*Subsystem:\s*"(?<ss>[a-z_]+)",\s*Name:\s*"(?<n>[a-z_]+)"')
    Assert-True -Condition $guildMetric.Success -Because 'go/guild/internal/logic/merge_fence.go 应当用 metric.NewCounterVec 定义 guildRouteReadyVerdictTotal(Namespace / Subsystem / Name 依次写出;换了写法要同步本用例)'
    $registered += ('{0}_{1}_{2}' -f $guildMetric.Groups['ns'].Value, $guildMetric.Groups['ss'].Value, $guildMetric.Groups['n'].Value)
    $exprs = Get-AlertExprText -Text $text
    $referenced = @([regex]::Matches($exprs, '\b(?:data_service|db|guild)_[a-z0-9_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Assert-True -Condition ($referenced -contains 'db_route_ready_gate' -and $referenced -contains 'guild_route_ready_verdict_total') -Because '夹具前提:表达式里有 go/db 与 guild 的读方指标'
    foreach ($metric in $referenced) {
        Assert-True -Condition ($registered -contains $metric) -Because "表达式里的 $metric 必须由 data_service / go/db / guild 的代码注册(名字逐字一致,改名后告警会静默失效)"
    }
    # db_route_ready_gate 必须是不带 label 的 Gauge:规格 §13.2 原文写过 {state},真加成 one-hot 的 GaugeVec 时 min == 0 会恒为真。
    $dbMetricsGo = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'go/db/internal/metrics/metrics.go'))
    Assert-Match -Text $dbMetricsGo -Pattern 'routeReadyGate\s*=\s*prometheus\.NewGauge\(prometheus\.GaugeOpts\{\s*Subsystem:\s*subsystem,\s*Name:\s*"route_ready_gate"' -Because 'GoDbRouteGateClosed 按 min by (namespace) (db_route_ready_gate) == 0 判,指标必须是不带 label 的 NewGauge'
    # 标签取值从常量核对(写错取值的表达式不报错、只会永远不响)。
    $dsMetrics = 'go/data_service/internal/metrics/metrics.go'
    $dbMetrics = 'go/db/internal/metrics/metrics.go'
    $verdicts = Get-RouteReadyVerdictLabelValues
    foreach ($check in @(
            @{ Metric = 'data_service_route_reconcile_event_total'; Label = 'event'; Path = $dsMetrics; Prefix = 'RouteReconcileEvent' }
            @{ Metric = 'data_service_route_reconcile_push_total'; Label = 'result'; Path = $dsMetrics; Prefix = 'RouteReconcilePush' }
            @{ Metric = 'data_service_route_reconcile_push_total'; Label = 'phase'; Path = $dsMetrics; Prefix = 'RouteReconcilePhase' }
            @{ Metric = 'data_service_route_reconcile_tick_total'; Label = 'result'; Path = $dsMetrics; Prefix = 'RouteReconcileTick' }
            @{ Metric = 'data_service_route_reconcile_mirror_total'; Label = 'result'; Path = $dsMetrics; Prefix = 'RouteReconcileMirror' }
            @{ Metric = 'data_service_route_publish_total'; Label = 'result'; Path = $dsMetrics; Prefix = 'RoutePublish' }
            @{ Metric = 'data_service_route_register_total'; Label = 'outcome'; Path = $dsMetrics; Prefix = 'RouteRegister' }
            @{ Metric = 'data_service_route_read_total'; Label = 'result'; Path = $dsMetrics; Prefix = 'RouteReadResult' }
            @{ Metric = 'data_service_route_read_total'; Label = 'path'; Path = $dsMetrics; Prefix = 'RouteReadPath' }
            @{ Metric = 'db_placement_guard_total'; Label = 'outcome'; Path = $dbMetrics; Prefix = 'Placement' }
        )) {
        Assert-AlertLabelValues -Exprs $exprs -Metric $check.Metric -Label $check.Label -Allowed (Get-GoPrefixedStringConsts -RelativePath $check.Path -Prefix $check.Prefix) -Source "$($check.Path) 的 $($check.Prefix)*"
    }
    foreach ($metric in @('data_service_route_ready_verdict_total', 'db_route_ready_verdict_total', 'guild_route_ready_verdict_total', 'data_service_route_ready_selfcheck')) {
        Assert-AlertLabelValues -Exprs $exprs -Metric $metric -Label 'verdict' -Allowed $verdicts -Source 'shared/placement 的 verdict 名加 script_error'
    }
    foreach ($check in @(
            @{ Rule = 'DataServiceRouteReadNotTrusted'; Pattern = 'data_service_route_ready_verdict_total\{verdict!="trusted"\}' }
            @{ Rule = 'DataServiceRoutePublishUnconfirmed'; Pattern = 'data_service_route_publish_total\{result=~"not_durable\|error"\}' }
            @{ Rule = 'DataServiceRouteRegisterFailures'; Pattern = 'data_service_route_register_total\{outcome=~"store_error\|precheck_error"\}' }
        )) {
        $body = $rules | Where-Object { $_ -match ('^\s*' + $check.Rule + '\b') }
        Assert-Match -Text $body -Pattern $check.Pattern -Because "$($check.Rule) 的选择器(评审 A2-6 / B3-9 定的口径)"
    }
    Assert-Match -Text $text -Pattern 'ServiceMonitor' -Because '文件头写明抓取前提:data-service 只有 Prometheus 注解、没有 ServiceMonitor,接入方式是 P2 门禁一项'
}

Test-Case '落点告警 player-placement-alerts.yaml:PrometheusRule 形状;设计 §13 / 种子 §9 要求的规则都在;指标名与 outcome / result / reason 取值都由 go/db、scene_manager 源码定义;出现即报的四条带「新序列」分支' {
    # 设计 docs/design/player-storage-placement.md §13 / §14(09-28 只写了告警建议)与 new-player-first-enter-no-cold-read.md §9 第 3 条:
    # 打开 login 的 NewPlayerSeed 之前,missing_required 与 db_placement_store_open_total{result=~"error|rejected"} 两类告警必须就位。
    $text = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'deploy/k8s/player-placement-alerts.yaml')) -replace "`r`n", "`n"
    Assert-Match -Text $text -Pattern '(?m)^kind:[ ]*PrometheusRule[ ]*$' -Because '必须是 PrometheusRule(与 owner-epoch-alerts.yaml 同一安装方式)'
    $intervals = @([regex]::Matches($text, '(?m)^      interval:[ ]*(\S+)[ ]*$') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
    Assert-Equal -Expected '30s' -Actual ($intervals -join ',') -Because '每个组 interval: 30s'
    $alertNames = @([regex]::Matches($text, '(?m)^        - alert:[ ]*(\S+)[ ]*$') | ForEach-Object { $_.Groups[1].Value })
    foreach ($name in @('DbPlacementStaleTopic', 'DbPlacementMissingRequired', 'DbPlacementStoreRejected', 'DbPlacementStoreOpenErrors',
            'DbPlacementFrozenStuck', 'DbPlacementRecheckMoved', 'SceneManagerHomeZoneMerging')) {
        Assert-True -Condition ($alertNames -contains $name) -Because "设计 §13 / §14 与种子 §9 要求的告警 $name 必须在"
    }
    Assert-Equal -Expected $alertNames.Count -Actual (@($alertNames | Sort-Object -Unique).Count) -Because '告警名不重复'
    $rules = @(($text -split '(?m)^        - alert:') | Select-Object -Skip 1)
    Assert-Equal -Expected $alertNames.Count -Actual $rules.Count -Because '按 - alert: 切出的规则块数等于告警数'
    foreach ($rule in $rules) {
        $ruleName = ($rule -split "`n")[0].Trim()
        Assert-Match -Text $rule -Pattern '(?m)^            severity:[ ]*(critical|warning|info)[ ]*$' -Because "$ruleName 要有 severity(critical / warning / info)"
        Assert-Match -Text $rule -Pattern '(?m)^            summary:' -Because "$ruleName 要有 summary"
        Assert-Match -Text $rule -Pattern '(?m)^            description:' -Because "$ruleName 要有 description"
        Assert-Match -Text $rule -Pattern '(?m)^          for:[ ]*\d+[ms][ ]*$' -Because "$ruleName 要写 for"
    }
    # 同名告警在 Alertmanager 里按 alertname 分组,会与别的文件的规则混在一起、互相掩盖:落点规则名不得与相邻几份告警文件重名。
    foreach ($other in @('deploy/k8s/db-task-retry-alerts.yaml', 'deploy/k8s/scene-manager-alerts.yaml', 'deploy/k8s/data-service-route-alerts.yaml')) {
        $otherText = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) $other)) -replace "`r`n", "`n"
        $otherNames = @([regex]::Matches($otherText, '(?m)^\s+- alert:[ ]*(\S+)[ ]*$') | ForEach-Object { $_.Groups[1].Value })
        foreach ($name in $alertNames) {
            Assert-True -Condition (-not ($otherNames -contains $name)) -Because "落点告警 $name 不得与 $other 里的规则重名"
        }
    }

    # 指标名从源码拼全名(go/db 与 scene_manager 的 metrics 包),逐个核对表达式引用的名字。
    $registered = @(Get-GoSubsystemMetricNames -RelativeDir 'go/db/internal/metrics') + @(Get-GoSubsystemMetricNames -RelativeDir 'go/scene_manager/internal/metrics')
    $exprs = Get-AlertExprText -Text $text
    $referenced = @([regex]::Matches($exprs, '\b(?:db|scene_manager)_[a-z0-9_]+') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    foreach ($expected in @('db_placement_guard_total', 'db_placement_store_open_total', 'scene_manager_enter_scene_rejected_total')) {
        Assert-True -Condition ($referenced -contains $expected) -Because "夹具前提:表达式里引用 $expected"
    }
    foreach ($metric in $referenced) {
        Assert-True -Condition ($registered -contains $metric) -Because "表达式里的 $metric 必须由 go/db / scene_manager 的 metrics 包注册(名字逐字一致,改名后告警会静默失效)"
    }
    $dbMetrics = 'go/db/internal/metrics/metrics.go'
    Assert-AlertLabelValues -Exprs $exprs -Metric 'db_placement_guard_total' -Label 'outcome' -Allowed (Get-GoPrefixedStringConsts -RelativePath $dbMetrics -Prefix 'Placement') -Source "$dbMetrics 的 Placement*"
    Assert-AlertLabelValues -Exprs $exprs -Metric 'db_placement_guard_total' -Label 'op' -Allowed @('read', 'write') -Source 'CountPlacementGuard 的 op(read / write)'
    Assert-AlertLabelValues -Exprs $exprs -Metric 'db_placement_store_open_total' -Label 'result' -Allowed (Get-GoPrefixedStringConsts -RelativePath $dbMetrics -Prefix 'StoreOpen') -Source "$dbMetrics 的 StoreOpen*"
    # scene_manager 的 reason:home_zone_merging 是 home_zone.go 的常量,必须同时列在 metrics.go 的 Help 里(Help 是 reason 的权威清单)。
    $homeZoneGo = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'go/scene_manager/internal/logic/home_zone.go'))
    $merging = [regex]::Match($homeZoneGo, '(?m)^\s*rejectReasonHomeZoneMerging\s*=\s*"(?<v>[a-z_]+)"')
    Assert-True -Condition $merging.Success -Because 'go/scene_manager/internal/logic/home_zone.go 应当定义 rejectReasonHomeZoneMerging(改名了要同步本用例)'
    Assert-AlertLabelValues -Exprs $exprs -Metric 'scene_manager_enter_scene_rejected_total' -Label 'reason' -Allowed @($merging.Groups['v'].Value) -Source 'home_zone.go 的 rejectReasonHomeZoneMerging'
    $smMetricsGo = [IO.File]::ReadAllText((Join-Path (Get-RepoRoot) 'go/scene_manager/internal/metrics/metrics.go'))
    Assert-Match -Text $smMetricsGo -Pattern ('EnterScene rejections by reason \([^)]*\b{0}\b' -f [regex]::Escape($merging.Groups['v'].Value)) -Because 'enter_scene_rejected_total 的 Help 必须列出 home_zone_merging'

    # 规则口径(评审 B3-4 定的选择器;种子 §9 的两类硬门禁)。
    $ruleOf = { param([string]$Name) $rules | Where-Object { $_ -match ('^\s*' + $Name + '\b') } }
    foreach ($check in @(
            @{ Rule = 'DbPlacementStaleTopic'; Pattern = 'db_placement_guard_total\{outcome="stale_topic"\}'; Severity = 'critical' }
            @{ Rule = 'DbPlacementMissingRequired'; Pattern = 'db_placement_guard_total\{outcome="missing_required"\}'; Severity = 'critical' }
            @{ Rule = 'DbPlacementStoreRejected'; Pattern = 'db_placement_store_open_total\{result="rejected"\}'; Severity = 'critical' }
            @{ Rule = 'DbPlacementStoreOpenErrors'; Pattern = 'db_placement_store_open_total\{result="error"\}'; Severity = 'warning' }
            @{ Rule = 'DbPlacementFrozenStuck'; Pattern = 'db_placement_guard_total\{op="write", outcome="frozen_deferred"\}'; Severity = 'warning' }
            @{ Rule = 'DbPlacementRecheckMoved'; Pattern = 'db_placement_guard_total\{outcome="recheck_moved"\}'; Severity = 'warning' }
            @{ Rule = 'SceneManagerHomeZoneMerging'; Pattern = 'scene_manager_enter_scene_rejected_total\{reason="home_zone_merging"\}'; Severity = 'warning' }
        )) {
        $body = & $ruleOf $check.Rule
        Assert-Match -Text $body -Pattern $check.Pattern -Because "$($check.Rule) 的选择器"
        Assert-Match -Text $body -Pattern ('(?m)^            severity:[ ]*' + $check.Severity + '[ ]*$') -Because "$($check.Rule) 的级别是 $($check.Severity)"
    }
    # MissingRequired 不按 op 过滤:写进死信与读失败(冷登录失败,读不进死信,别处没有告警)都要报。
    Assert-NotMatch -Text (& $ruleOf 'DbPlacementMissingRequired') -Pattern 'outcome="missing_required", op=|op="[a-z]+", outcome="missing_required"' -Because 'MissingRequired 要同时覆盖 op=read(读失败只有这一条告警)'
    # go/db 的两个计数器启动时没有以 0 预置:出现即报的四条要有 `unless … offset 10m` 第二支(带 > 0),漏掉「从无到 1」的第一次就是漏报。
    foreach ($name in @('DbPlacementStaleTopic', 'DbPlacementMissingRequired', 'DbPlacementStoreRejected', 'DbPlacementStoreOpenErrors')) {
        $body = & $ruleOf $name
        Assert-Match -Text $body -Pattern '(?s)\bor\b.*\bunless\b[^\n]*\boffset 10m\b.*\)\s*>\s*0' -Because "$name 要用 unless … offset 10m 把 10 分钟内新出现的序列也算进来,并以 > 0 过滤(以后预置 0 时不误报)"
    }
    Assert-Match -Text $text -Pattern 'NewPlayerSeed' -Because '文件头写明它是打开 NewPlayerSeed 的前置(种子 §9 第 3 条)'
}

# ─────────────────────────────────────────────────────────────────
# B0-4 部署侧(2026-10-20):NODE_OPEN_FILE_LIMIT_MODE 接线、C++ Deployment 的 maxUnavailable 0、-WaitReady 失败时捞拒启行
# ─────────────────────────────────────────────────────────────────
# 进程侧是 cpp/libs/engine/core/node/system/node/open_file_limit.h:软限抬到硬限后仍不够就缺省拒启,NODE_OPEN_FILE_LIMIT_MODE=warn 豁免
# (负责人 2026-10-17 拍板)。生成侧钉住:不传参数时五套 C++ 模板一个字都不写;给了值就写进本次部署的全部 C++ 工作负载,
# 每个只写一次、值原样小写;大写或拼错在入口就拒(C++ 只认小写,大写会被进程静默当成 enforce)。五套模板 = 本脚本的
# Deployment(gate / scene / battle)与 scene Fleet,加 lib 的 gate StatefulSet / battle Fleet / battle hostPort Deployment ——
# 只改其中几套正是本仓踩过的坑。默认值复用上文的 $devOut / $agonesOut / $zoneRedisEntryRun / $battleInfraRun / $battleZoneRedisFleetRun。
$OpenFileLimitEnvName = 'NODE_OPEN_FILE_LIMIT_MODE'
$oflZoneWarnRun = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SkipGoSvc', '-SkipJavaSvc', '-NodeOpenFileLimitMode', 'warn'))
$oflZoneEntryEnforceRun = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SkipGoSvc', '-SkipJavaSvc', '-SceneOrchestrator', 'agones',
    '-ClientEntryMode', 'external', '-GateServiceType', 'NodePort', '-GateNodePortBase', '30000', '-ClientPublicHost', '127.0.0.1', '-NodeOpenFileLimitMode', 'enforce'))
$oflBattleWarnRun = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-NodeOpenFileLimitMode', 'warn')) -Env $BattleTestEnv
# external + 缺省 -BattleOrchestrator deployment = battle 的 hostPort 形态(lib New-BattleHostPortDeploymentYaml)。
$oflBattleHostPortWarnRun = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-ClientEntryMode', 'external', '-NodeOpenFileLimitMode', 'warn')) -Env $BattleTestEnv
$oflBattleFleetEnforceRun = Invoke-DeployDryRun -Arguments ($BattleInfraArgs + @('-BattleOrchestrator', 'agones', '-ClientEntryMode', 'external',
    '-ClientPublicHost', '127.0.0.1', '-NodeOpenFileLimitMode', 'enforce')) -Env $BattleTestEnv

# DryRun 输出里的 C++ 工作负载块:只有 C++ 模板挂 /app/bin/etc(Go / Java 清单与 infra 都不挂)。返回 { Key = kind/name; Block }。
function Get-CppWorkloadBlocks {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Output)
    foreach ($block in (Get-ManifestBlocks -Output $Output)) {
        if ($block -notmatch 'mountPath: /app/bin/etc') { continue }
        $kind = [regex]::Match($block, '(?m)^kind:[ ]*(?<v>\S+)[ ]*$').Groups['v'].Value
        $name = [regex]::Match($block, '(?m)^  name:[ ]*(?<v>\S+)[ ]*$').Groups['v'].Value
        [pscustomobject]@{ Key = "$kind/$name"; Block = $block }
    }
}

# 工作负载里 NODE_OPEN_FILE_LIMIT_MODE 的取值:没有返回 $null;必须按「- name / value」成对写出且最多一处,否则 throw
# (重复的 env 键以哪条为准不该交给 kubelet;只写名字不写值会被当成空串,进程按 enforce)。
function Get-OpenFileLimitModeEnvValue {
    param(
        [Parameter(Mandatory = $true)][string]$Block,
        [Parameter(Mandatory = $true)][string]$Label
    )
    $pairs = [regex]::Matches($Block, '(?m)^[ ]+- name: ' + $OpenFileLimitEnvName + '[ ]*\n[ ]+value: "(?<v>[^"\n]*)"[ ]*$')
    $mentions = [regex]::Matches($Block, [regex]::Escape($OpenFileLimitEnvName)).Count
    if ($mentions -ne $pairs.Count) { throw "${Label}:$OpenFileLimitEnvName 出现 $mentions 处,按「- name / value」成对写出的只有 $($pairs.Count) 处" }
    if ($pairs.Count -gt 1) { throw "${Label}:$OpenFileLimitEnvName 应最多写一处,实际 $($pairs.Count) 处" }
    if ($pairs.Count -eq 0) { return $null }
    return $pairs[0].Groups['v'].Value
}

# 一次 DryRun 的全部 C++ 工作负载都带(或都不带)这个变量、取值一致,并且恰好覆盖预期的那几套模板。
function Assert-OpenFileLimitModeRendered {
    param(
        [Parameter(Mandatory = $true)]$Run,
        [Parameter(Mandatory = $true)][string]$Label,
        [Parameter(Mandatory = $true)][string[]]$ExpectedWorkloads,
        [AllowNull()][string]$ExpectedValue
    )
    Assert-Equal -Expected 0 -Actual $Run.ExitCode -Because "${Label}:DryRun 应当成功。输出: $($Run.Output)"
    $workloads = @(Get-CppWorkloadBlocks -Output $Run.Output)
    Assert-Equal -Expected ((@($ExpectedWorkloads) | Sort-Object) -join ',') -Actual ((@($workloads | ForEach-Object { $_.Key }) | Sort-Object -Unique) -join ',') -Because "${Label}:这次 DryRun 要覆盖的 C++ 模板(夹具前提)"
    foreach ($workload in $workloads) {
        $actual = Get-OpenFileLimitModeEnvValue -Block $workload.Block -Label "$Label $($workload.Key)"
        if ([string]::IsNullOrEmpty($ExpectedValue)) {
            Assert-True -Condition ($null -eq $actual) -Because "${Label} $($workload.Key):不传 -NodeOpenFileLimitMode 时不得写 $OpenFileLimitEnvName(进程按 enforce,模板与引入开关之前逐字相同)"
        }
        else {
            Assert-True -Condition ($actual -ceq $ExpectedValue) -Because "${Label} $($workload.Key):$OpenFileLimitEnvName 必须原样写成 '$ExpectedValue'(实际 '$actual');漏了这一套模板,运维以为全开了豁免,这一种形态仍会拒启"
        }
    }
    foreach ($block in (Get-ManifestBlocks -Output $Run.Output)) {
        if ($block -match 'mountPath: /app/bin/etc') { continue }
        Assert-NotMatch -Text $block -Pattern $OpenFileLimitEnvName -Because "${Label}:只有 C++ 进程读这个变量,其它工作负载不写"
    }
}

Test-Case 'B0-4:不传 -NodeOpenFileLimitMode 时五套 C++ 模板都不写 NODE_OPEN_FILE_LIMIT_MODE,也不打豁免告警' {
    foreach ($case in @(
        @{ Label = 'zone-up 缺省'; Run = $devRun; Expected = @('Deployment/gate', 'Deployment/scene') }
        @{ Label = 'zone-up Agones scene'; Run = $agonesRun; Expected = @('Deployment/gate', 'Fleet/scene') }
        @{ Label = 'zone-up external gate + Agones scene'; Run = $zoneRedisEntryRun; Expected = @('StatefulSet/gate', 'Fleet/scene') }
        @{ Label = 'infra-up battle Deployment'; Run = $battleInfraRun; Expected = @('Deployment/battle') }
        @{ Label = 'infra-up battle Agones Fleet'; Run = $battleZoneRedisFleetRun; Expected = @('Fleet/battle') }
    )) {
        Assert-OpenFileLimitModeRendered -Run $case.Run -Label $case.Label -ExpectedWorkloads $case.Expected -ExpectedValue $null
        Assert-NotMatch -Text $case.Run.Output -Pattern '-NodeOpenFileLimitMode warn:' -Because "$($case.Label):没开豁免就不该打豁免告警"
    }
}

Test-Case 'B0-4:-NodeOpenFileLimitMode warn / enforce 写进本次部署的全部 C++ 工作负载(五套模板),每个恰好一次、取值原样;只有 warn 打告警' {
    foreach ($case in @(
        @{ Label = 'zone-up warn'; Run = $oflZoneWarnRun; Expected = @('Deployment/gate', 'Deployment/scene'); Value = 'warn' }
        @{ Label = 'zone-up external gate + Agones scene enforce'; Run = $oflZoneEntryEnforceRun; Expected = @('StatefulSet/gate', 'Fleet/scene'); Value = 'enforce' }
        @{ Label = 'infra-up battle Deployment warn'; Run = $oflBattleWarnRun; Expected = @('Deployment/battle'); Value = 'warn' }
        @{ Label = 'infra-up battle hostPort warn'; Run = $oflBattleHostPortWarnRun; Expected = @('Deployment/battle'); Value = 'warn' }
        @{ Label = 'infra-up battle Agones Fleet enforce'; Run = $oflBattleFleetEnforceRun; Expected = @('Fleet/battle'); Value = 'enforce' }
    )) {
        Assert-OpenFileLimitModeRendered -Run $case.Run -Label $case.Label -ExpectedWorkloads $case.Expected -ExpectedValue $case.Value
        if ($case.Value -ceq 'warn') {
            Assert-Match -Text $case.Run.Output -Pattern '-NodeOpenFileLimitMode warn:' -Because "$($case.Label):warn 只作短时应急,每次部署都要打一行告警,免得被当成常态配置"
        }
        else {
            Assert-NotMatch -Text $case.Run.Output -Pattern '-NodeOpenFileLimitMode warn:' -Because "$($case.Label):enforce 不是豁免,不打豁免告警"
        }
    }
    $hostPort = Select-CppWorkloadBlock -Output $oflBattleHostPortWarnRun.Output -Kind 'Deployment' -Name 'battle' -Label 'infra-up battle hostPort warn'
    Assert-Match -Text $hostPort -Pattern 'hostPort: 20000' -Because '夹具前提:external + deployment 渲染的是 hostPort 形态,不是 New-NodeDeploymentYaml 的 battle'
}

Test-Case '负向:-NodeOpenFileLimitMode 只收小写 enforce / warn,大写与其它值在入口被拒、先于任何资源变更' {
    foreach ($bad in @('WARN', 'off')) {
        $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-SkipGoSvc', '-SkipJavaSvc', '-NodeOpenFileLimitMode', $bad))
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "'$bad' 必须在脚本入口被拒:C++ 只认小写 enforce / warn,其余值按 enforce 处理,原样写进 env 等于运维以为开了豁免、进程照样拒启"
        Assert-Match -Text $run.Output -Pattern 'NodeOpenFileLimitMode' -Because '不能把其他执行错误误判为参数校验成功'
        Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl' -Because '参数校验必须先于任何资源变更'
    }
}

Test-Case 'B0-4:C++ Deployment(gate / scene / battle)显式 maxUnavailable 0、maxSurge 留缺省;battle hostPort 仍是 maxSurge 0 / maxUnavailable 1' {
    # scene 缺省 4 副本,K8s 缺省 25% 向下取整 = 先缩 1 台旧 scene;新 Pod 因 nofile 硬限不够拒启时,这份容量一直缺到回滚。
    foreach ($case in @(
        @{ Label = 'gate'; Output = $devOut; Name = 'gate' }
        @{ Label = 'scene'; Output = $devOut; Name = 'scene' }
        @{ Label = 'battle'; Output = $battleInfraRun.Output; Name = 'battle' }
    )) {
        $block = Select-CppWorkloadBlock -Output $case.Output -Kind 'Deployment' -Name $case.Name -Label $case.Label
        Assert-Match -Text $block -Pattern '(?m)^  strategy:\n    type: RollingUpdate\n    rollingUpdate:\n      maxUnavailable: 0[ ]*$' -Because "$($case.Label):新 Pod 就绪之前不缩旧 Pod,B0-4 拒启只让滚动停住"
        Assert-NotMatch -Text $block -Pattern 'maxSurge' -Because "$($case.Label):maxSurge 留 K8s 缺省 25%(至少 1 个新 Pod),滚动节奏与改动前相同"
    }
    $hostPort = Select-CppWorkloadBlock -Output $oflBattleHostPortWarnRun.Output -Kind 'Deployment' -Name 'battle' -Label 'battle hostPort'
    Assert-Match -Text $hostPort -Pattern '(?m)^  strategy:\n    type: RollingUpdate\n    rollingUpdate:\n      maxSurge: 0\n      maxUnavailable: 1[ ]*$' -Because 'hostPort 独占同一节点端口(D86):新 Pod 不能与旧 Pod 抢端口,保持先删后起'
}

# -WaitReady 失败诊断:Write-CppOpenFileLimitRefusals 按 AST 抽出直接调用(同上文各守卫),用假 kubectl 喂回包,不碰集群。
$oflDiagFnAst = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Write-CppOpenFileLimitRefusals' }, $true)
if ($null -eq $oflDiagFnAst) { throw 'k8s_deploy.ps1 缺少函数 Write-CppOpenFileLimitRefusals(-WaitReady 的 B0-4 拒启诊断被删了?后续断言无意义)' }
Set-Item -Path 'Function:script:Write-CppOpenFileLimitRefusals' -Value $oflDiagFnAst.Body.GetScriptBlock()

Test-Case 'B0-4 -WaitReady 失败诊断:只读未就绪 Pod,先 --previous、没有上一个实例才读当前实例,只打印拒启行;kubectl 失败只告警不 throw,DryRun 不碰集群' {
    # 假 kubectl:函数优先于外部程序,被测函数里的 `& kubectl` 解析到这里(按动态作用域)。按命令回应 $fake,记下每次调用的参数。
    function Build-KubectlBaseArgs { return , @() }
    $calls = [System.Collections.Generic.List[string]]::new()
    function kubectl {
        $line = $args -join ' '
        $calls.Add($line)
        $reply = $null
        if ($line -match '^get pods ') { $reply = $fake.Pods }
        elseif ($line -match '^logs (?<pod>\S+) .*--previous$') { $reply = $fake.Previous[$Matches['pod']] }
        elseif ($line -match '^logs (?<pod>\S+) ') { $reply = $fake.Current[$Matches['pod']] }
        if ($null -eq $reply) { $reply = @{ Exit = 1; Out = @() } }
        $global:LASTEXITCODE = [int]$reply.Exit
        return $reply.Out
    }
    $refusal = 'FATAL open file limit config: role=gate soft=1024 hard=1024 required=24096 (GateMaxConnections=20000 + reserve 4096) shortfall=23072 raise=none'
    # gate-ok 已就绪(旧 Pod,不读);gate-crash 在 CrashLoop(读 --previous);gate-fresh 刚崩第一次、没有上一个实例(改读当前实例);
    # gate-init 还没有容器状态(Init 卡住,两次都读不到,跳过)。
    $podsJson = '{"items":[' +
        '{"metadata":{"name":"gate-ok"},"status":{"containerStatuses":[{"name":"gate","ready":true,"restartCount":0}]}},' +
        '{"metadata":{"name":"gate-crash"},"status":{"containerStatuses":[{"name":"gate","ready":false,"restartCount":4}]}},' +
        '{"metadata":{"name":"gate-fresh"},"status":{"containerStatuses":[{"name":"gate","ready":false,"restartCount":0}]}},' +
        '{"metadata":{"name":"gate-init"},"status":{}}]}'
    $DryRun = $false
    try {
        $fake = @{
            Pods     = @{ Exit = 0; Out = $podsJson }
            Previous = @{ 'gate-crash' = @{ Exit = 0; Out = @('[open_file_limit] role=gate verdict=insufficient mode=enforce required=24096', $refusal) } }
            Current  = @{ 'gate-fresh' = @{ Exit = 0; Out = @('starting', $refusal) } }
        }
        $calls.Clear()
        $text = (Write-CppOpenFileLimitRefusals -Namespace 'contract-ns' -WorkloadName 'gate' 6>&1 3>&1 | Out-String)
        Assert-Match -Text $text -Pattern ([regex]::Escape("pod=gate-crash (previous): $refusal")) -Because 'CrashLoop 的 Pod 拒启行在上一个容器实例里'
        Assert-Match -Text $text -Pattern ([regex]::Escape("pod=gate-fresh (current): $refusal")) -Because '没有上一个实例(Agones GameServer 不重启、或刚崩第一次)时读当前实例'
        Assert-NotMatch -Text $text -Pattern 'verdict=insufficient' -Because '只打印拒启行,启动行与其它日志不往部署输出里倒'
        Assert-Match -Text $text -Pattern 'refused to start' -Because '命中后要给出处置方向(调低准入上限 / 改运行时 LimitNOFILE / 短时 warn)'
        $joined = $calls -join "`n"
        Assert-Match -Text $joined -Pattern '(?m)^get pods -n contract-ns -l app=gate -o json$' -Because '按工作负载标签列 Pod'
        Assert-NotMatch -Text $joined -Pattern 'logs gate-ok ' -Because '已就绪的旧 Pod 不读日志,失败路径上的耗时只花在可疑 Pod 上'
        Assert-Match -Text $joined -Pattern '(?m)^logs gate-crash -n contract-ns -c gate --tail 200 --previous$' -Because '指定容器名(原生 sidecar 也在 Pod 里)、只拉尾部、先读上一个实例'
        Assert-Equal -Expected 1 -Actual (@($calls | Where-Object { $_ -like 'logs gate-crash *' }).Count) -Because '上一个实例读到了就不再读当前实例'
        Assert-Equal -Expected 2 -Actual (@($calls | Where-Object { $_ -like 'logs gate-init *' }).Count) -Because '两次都读不到就跳过,不 throw'

        # 都读到了但没有拒启行:明确说「排除了」,运维不必再去翻。
        $fake.Previous = @{ 'gate-crash' = @{ Exit = 0; Out = @('connect etcd failed') } }
        $fake.Current = @{ 'gate-fresh' = @{ Exit = 0; Out = @('starting') } }
        $text = (Write-CppOpenFileLimitRefusals -Namespace 'contract-ns' -WorkloadName 'gate' 6>&1 3>&1 | Out-String)
        Assert-Match -Text $text -Pattern ([regex]::Escape("no 'FATAL open file limit' line in the logs of 3 not-ready pod(s) (app=gate, 3 not ready of 4)")) -Because '没命中也要打一行,说明查过哪些 Pod'

        # 未就绪 Pod 很多时最多读 10 个。
        $manyPods = @(1..12 | ForEach-Object { '{"metadata":{"name":"gate-' + $_ + '"},"status":{"containerStatuses":[{"name":"gate","ready":false}]}}' })
        $fake = @{ Pods = @{ Exit = 0; Out = ('{"items":[' + ($manyPods -join ',') + ']}') }; Previous = @{}; Current = @{} }
        $calls.Clear()
        $text = (Write-CppOpenFileLimitRefusals -Namespace 'contract-ns' -WorkloadName 'gate' 6>&1 3>&1 | Out-String)
        Assert-Equal -Expected 10 -Actual (@($calls | Where-Object { $_ -like 'logs * --previous' }).Count) -Because '失败路径上最多读 10 个 Pod'
        Assert-Match -Text $text -Pattern ([regex]::Escape('of 10 not-ready pod(s) (app=gate, 12 not ready of 12)')) -Because '截断要在输出里看得出来'

        # 列 Pod 失败、回包不是 JSON:只告警、不 throw(调用方随后照常抛等待超时)。
        foreach ($broken in @(
            @{ Why = 'kubectl get pods 失败'; Pods = @{ Exit = 1; Out = '' }; Pattern = 'open file limit check skipped' }
            @{ Why = '回包不是 JSON'; Pods = @{ Exit = 0; Out = 'not json' }; Pattern = 'open file limit check failed' }
        )) {
            $fake = @{ Pods = $broken.Pods; Previous = @{}; Current = @{} }
            $calls.Clear()
            $message = ''
            $text = ''
            try { $text = (Write-CppOpenFileLimitRefusals -Namespace 'contract-ns' -WorkloadName 'gate' 6>&1 3>&1 | Out-String) } catch { $message = $_.Exception.Message }
            Assert-Equal -Expected '' -Actual $message -Because "$($broken.Why):诊断是尽力而为,不能 throw 把原本的等待失败盖掉"
            Assert-Match -Text $text -Pattern $broken.Pattern -Because "$($broken.Why):要告警说明没查成"
            Assert-Equal -Expected 1 -Actual $calls.Count -Because "$($broken.Why):列不出 Pod 就不读日志"
        }

        # DryRun 不碰集群。
        $DryRun = $true
        $calls.Clear()
        Write-CppOpenFileLimitRefusals -Namespace 'contract-ns' -WorkloadName 'gate' 6>&1 3>&1 | Out-Null
        Assert-Equal -Expected 0 -Actual $calls.Count -Because 'DryRun 下不调 kubectl'
    }
    finally {
        # 假 kubectl 改过全局退出码;复位,免得影响后面按 $LASTEXITCODE 判断的用例。
        $global:LASTEXITCODE = 0
    }
}

Test-Case 'B0-4 -WaitReady 接线:gate / scene / battle 的五个等待点失败时都捞拒启行(Deployment 带 -CppNode,StatefulSet / Fleet 在 catch 里捞完原样抛出),Go / Java 服务不捞' {
    $cppTargets = @('"gate"', '$sceneDeployment', "'battle'")
    $deploymentSites = 0
    $wrappedSites = 0
    foreach ($fnName in @('Wait-ForZoneReady', 'Apply-BattlePool')) {
        $fn = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq $fnName }, $true)
        Assert-True -Condition ($null -ne $fn) -Because "k8s_deploy.ps1 里应当有 $fnName"
        foreach ($cmd in @($fn.Body.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))) {
            $commandName = $cmd.GetCommandName()
            $elements = @($cmd.CommandElements)
            if ($commandName -eq 'Wait-ForDeploymentReady') {
                $target = ''
                for ($i = 1; $i -lt $elements.Count - 1; $i++) {
                    if ($elements[$i] -is [System.Management.Automation.Language.CommandParameterAst] -and $elements[$i].ParameterName -eq 'DeploymentName') { $target = $elements[$i + 1].Extent.Text }
                }
                $hasSwitch = @($elements | Where-Object { $_ -is [System.Management.Automation.Language.CommandParameterAst] -and $_.ParameterName -eq 'CppNode' }).Count -eq 1
                if ($cppTargets -contains $target) {
                    Assert-True -Condition $hasSwitch -Because "$fnName 等 C++ Deployment $target 必须带 -CppNode,否则 B0-4 拒启时运维只看到 rollout 超时"
                    $deploymentSites++
                }
                else {
                    Assert-True -Condition (-not $hasSwitch) -Because "$fnName 等的 $target 不是 C++ 节点,不该去捞 C++ 的拒启行"
                }
            }
            elseif ($commandName -in @('Wait-ForGateStatefulSetReady', 'Wait-ForFleetReady')) {
                $try = $cmd.Parent
                while ($null -ne $try -and $try -isnot [System.Management.Automation.Language.TryStatementAst]) { $try = $try.Parent }
                Assert-True -Condition ($null -ne $try) -Because "$fnName 的 $commandName 必须包在 try 里:库函数超时即 throw,诊断只能在 catch 里做"
                $catchText = (@($try.CatchClauses | ForEach-Object { $_.Body.Extent.Text })) -join "`n"
                Assert-Match -Text $catchText -Pattern 'Write-CppOpenFileLimitRefusals -Namespace' -Because "$fnName 的 $commandName 失败时要捞拒启行"
                Assert-Match -Text $catchText -Pattern '(?m)^\s*throw\s*$' -Because "$fnName 的 $commandName:诊断之后必须原样抛出,不能把等待失败吞掉"
                $wrappedSites++
            }
        }
    }
    Assert-Equal -Expected 3 -Actual $deploymentSites -Because 'gate Deployment、scene 池、battle Deployment(含 hostPort 形态)三个调用点'
    Assert-Equal -Expected 2 -Actual $wrappedSites -Because 'external gate StatefulSet 与 battle Agones Fleet 两个调用点'
    $waitFn = $deadlineDeployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Wait-ForDeploymentReady' }, $true)
    Assert-Match -Text $waitFn.Body.Extent.Text -Pattern '(?s)if \(\$CppNode\) \{\s*Write-CppOpenFileLimitRefusals -Namespace \$Namespace -WorkloadName \$DeploymentName\s*\}\s*throw "Deployment rollout failed' -Because 'Wait-ForDeploymentReady 在失败分支、throw 之前捞拒启行'
}

Test-Case 'B0-4 lib 纵深防御:Get-ClientEntryNodeBaseEnv 只写小写 enforce / warn,大写或其它值在渲染时 throw;留空或 PodCommon 缺这个成员时不写' {
    . (Join-Path (Get-ToolsScriptsDir) 'lib' 'k8s_client_entry.ps1')
    $common = @{ Image = 'registry.invalid/test/mmorpg-node:0123456789ab'; ImagePullPolicy = 'IfNotPresent'; SnowflakeCacheDir = '/app/snowflake-cache'; LogVolumeSizeLimit = '2Gi' }
    $baseNames = 'POD_IP,RPC_PORT,SNOWFLAKE_CACHE_DIR'
    $entries = Get-ClientEntryNodeBaseEnv -PodCommon (New-ClientEntryPodCommon @common) -RpcPort 20000
    Assert-Equal -Expected $baseNames -Actual (@($entries | ForEach-Object { $_.Name }) -join ',') -Because '不传 -OpenFileLimitMode:不写,三个生成器与引入开关之前逐字相同'
    $entries = Get-ClientEntryNodeBaseEnv -PodCommon ([pscustomobject]@{ SnowflakeCacheDir = '/app/snowflake-cache' }) -RpcPort 20000
    Assert-Equal -Expected $baseNames -Actual (@($entries | ForEach-Object { $_.Name }) -join ',') -Because '手拼的 PodCommon 没有这个成员 = 不写'
    foreach ($good in @('enforce', 'warn')) {
        $entries = Get-ClientEntryNodeBaseEnv -PodCommon (New-ClientEntryPodCommon @common -OpenFileLimitMode $good) -RpcPort 20000
        $mode = @($entries | Where-Object { $_.Name -eq $OpenFileLimitEnvName })
        Assert-Equal -Expected 1 -Actual $mode.Count -Because "${good}:写且只写一次"
        Assert-True -Condition ([string]$mode[0].Value -ceq $good) -Because "${good}:值原样写出(实际 '$($mode[0].Value)')"
    }
    foreach ($bad in @('WARN', 'Enforce', 'off', ' warn')) {
        $message = ''
        try { Get-ClientEntryNodeBaseEnv -PodCommon (New-ClientEntryPodCommon @common -OpenFileLimitMode $bad) -RpcPort 20000 | Out-Null } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern 'OpenFileLimitMode 只收小写' -Because "'$bad' 必须在渲染时 throw:C++ 只认小写,其余值会被进程静默当成 enforce"
    }
}

Test-Case '有限roster模板绑定真实PodUID且normal不持registrar或其他boot配置' {
    . "$PSScriptRoot/../maintenance_drain.ps1"
    $source=Join-Path $PSScriptRoot '../k8s_deploy.ps1'
    $tk=$null;$errors=$null;$ast=[Management.Automation.Language.Parser]::ParseFile($source,[ref]$tk,[ref]$errors)
    if($errors.Count){throw '部署源解析失败，不能用其它断言掩盖'}
    foreach($name in @('Get-SqlRuntimeManifestRange','Get-SqlWriterDeploymentEntry','Add-SqlWriterBootManifest')){
        $definition=$ast.Find({param($n)$n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name},$true)
        Set-Item -Path "Function:script:$name" -Value $definition.Body.GetScriptBlock()
    }
    $taskRoot=Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-sql-deploy-'+[Guid]::NewGuid().ToString('N'))
    $null=[IO.Directory]::CreateDirectory($taskRoot)
    $savedCatalog=[Environment]::GetEnvironmentVariable('MMORPG_SQL_K8S_BOOT_CATALOG','Process')
    $savedRepo=$RepoRoot
    try {
        $RepoRoot=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
        $template=Get-Content -LiteralPath (Join-Path $RepoRoot 'deploy/k8s/manifests/go-svc/data-service.yaml') -Raw
        # 容量来自正式roster原件；每个Pod只能由init领取一个未消费的entry。
        $single=[regex]::Replace($template,'(?m)^  replicas:\s*\d+\s*$','  replicas: 1')
        $roster=Join-Path $taskRoot 'roster.json';$null=Write-MaintenanceImmutableJson -Path $roster -Value @{format='SQL_WRITER_BOOT_ROSTER_V1';entries=@(@{},@{})}
        $entry=@{requestSecret='fixture-request';registrarSecret='fixture-registrar';registrarImage=('fixture.invalid/registrar@sha256:'+('a'*64));physicalAuthoritySha256=('b'*64);runtimeConfigSecret='fixture-runtime';replicas=1;rosterFile=$roster;rosterSha256=(Get-FileHash -LiteralPath $roster -Algorithm SHA256).Hash.ToLowerInvariant()}
        $catalog=Join-Path $taskRoot 'catalog.json';$null=Write-MaintenanceImmutableJson -Path $catalog -Value @{'fixture/data-service'=$entry}
        [Environment]::SetEnvironmentVariable('MMORPG_SQL_K8S_BOOT_CATALOG',$catalog,'Process')
        $rendered=Add-SqlWriterBootManifest -ManifestContent $single -Namespace fixture -SvcName data-service
        Assert-True -Condition ($rendered -notmatch 'PLACEHOLDER_SQL_WRITER_') -Because '真实boot占位全部必须固定'
        Assert-Match -Text $rendered -Pattern 'fieldPath: metadata.uid' -Because '部署身份必须来自真实PodUID'
        Assert-Match -Text $rendered -Pattern '"-roster-sha256"' -Because 'init必须再次核已批准有限roster原件'
        $range=Get-SqlRuntimeManifestRange -ManifestContent $rendered -SvcName data-service
        $normal=$rendered.Substring($range.Start,$range.End-$range.Start)
        Assert-True -Condition ($normal -notmatch 'MMORPG_SQL_REGISTRAR_DSN|MMORPG_SQL_OPERATOR_PRINCIPAL|name: sql-writer-runtime-input|name: sql-writer-request') -Because 'init控制能力和其他boot完整凭据不泄漏给normal容器'
        Assert-Match -Text $normal -Pattern 'readOnly: true' -Because 'grant源对normal只读'
        $multi=[regex]::Replace($single,'(?m)^  replicas: 1\s*$','  replicas: 2')
        $message='';try{$null=Add-SqlWriterBootManifest -ManifestContent $multi -Namespace fixture -SvcName data-service}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '副本集合不符' -Because '真实副本数必须与已批准部署输入相符'
        $entry.replicas=2
        $multiCatalog=Join-Path $taskRoot 'multi-catalog.json';$null=Write-MaintenanceImmutableJson -Path $multiCatalog -Value @{'fixture/data-service'=$entry}
        [Environment]::SetEnvironmentVariable('MMORPG_SQL_K8S_BOOT_CATALOG',$multiCatalog,'Process')
        $renderedMulti=Add-SqlWriterBootManifest -ManifestContent $multi -Namespace fixture -SvcName data-service
        Assert-Match -Text $renderedMulti -Pattern '(?m)^  replicas: 2\s*$' -Because '两个独立entry允许两个Pod，不得退化为强制单副本'
        $entry.replicas=3
        $tooManyCatalog=Join-Path $taskRoot 'too-many-catalog.json';$null=Write-MaintenanceImmutableJson -Path $tooManyCatalog -Value @{'fixture/data-service'=$entry}
        [Environment]::SetEnvironmentVariable('MMORPG_SQL_K8S_BOOT_CATALOG',$tooManyCatalog,'Process')
        $message='';try{$null=Get-SqlWriterDeploymentEntry -Namespace fixture -SvcName data-service}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '有限roster不能覆盖' -Because '容量不足必须在渲染前拒绝，不能由重启临时补entry'
    } finally {
        $RepoRoot=$savedRepo
        [Environment]::SetEnvironmentVariable('MMORPG_SQL_K8S_BOOT_CATALOG',$savedCatalog,'Process')
        $full=[IO.Path]::GetFullPath($taskRoot);$allowed=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\','/')+[IO.Path]::DirectorySeparatorChar
        if(-not $full.StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $full) -notlike 'mmorpg-sql-deploy-*'){throw '夹具清理范围非法'}
        Remove-Item -LiteralPath $full -Recurse -Force
    }
}

exit (Complete-TestRun -SuiteName "k8s_deploy contract")
