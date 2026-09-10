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
Write-Host "=== k8s_deploy.ps1 部署生成器契约测试 ==="

# 一次 DryRun 供多个用例复用(子进程启动比断言贵得多)
$BaseArgs = @(
    "-Command", "zone-up",
    "-ZoneName", "contract-test",
    "-ZoneId", "101",
    "-DryRun",
    "-GoSvcRegistry", "registry.invalid/test",
    "-JavaSvcRegistry", "registry.invalid/test"
)

$devRun = Invoke-DeployDryRun -Arguments $BaseArgs
if ($devRun.ExitCode -ne 0) {
    Write-Host $devRun.Output
    throw "基线 DryRun 自己就失败了(exit=$($devRun.ExitCode)),后续断言无意义"
}
$devOut = $devRun.Output

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

Test-Case "login ConfigMap 的 Node/Locker/Kafka 关键值 == go/login/etc/login.yaml" {
    $block = Select-ManifestByName -Output $devOut -Name "go-svc-login-config"
    Assert-True -Condition ($null -ne $block) -Because "DryRun 输出里应当有 go-svc-login-config"
    $flat = ConvertTo-FlatManifest -Block $block

    $pairs = @(
        @{ Gen = 'data.login.yaml.Node.SessionExpireMin';  Etc = 'Node.SessionExpireMin' }
        @{ Gen = 'data.login.yaml.Node.MaxLoginDevices';   Etc = 'Node.MaxLoginDevices' }
        @{ Gen = 'data.login.yaml.Node.LeaseTTL';          Etc = 'Node.LeaseTTL' }
        @{ Gen = 'data.login.yaml.Node.QueueShardCount';   Etc = 'Node.QueueShardCount' }
        @{ Gen = 'data.login.yaml.Locker.AccountLockTTL';  Etc = 'Locker.AccountLockTTL' }
        @{ Gen = 'data.login.yaml.Locker.PlayerLockTTL';   Etc = 'Locker.PlayerLockTTL' }
        @{ Gen = 'data.login.yaml.Kafka.PartitionCnt';     Etc = 'Kafka.PartitionCnt' }
        @{ Gen = 'data.login.yaml.Kafka.InitialPartition'; Etc = 'Kafka.InitialPartition' }
        @{ Gen = 'data.login.yaml.Kafka.TopicGeneration';  Etc = 'Kafka.TopicGeneration' }
    )
    foreach ($p in $pairs) {
        $expected = Get-EtcValue -RelativePath 'go/login/etc/login.yaml' -KeyPath $p.Etc
        $actual = Get-FlatValue -Flat $flat -KeyPath $p.Gen
        Assert-Equal -Expected $expected -Actual $actual -Because "$($p.Etc) 必须与 go/login/etc/login.yaml 一致"
    }
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

exit (Complete-TestRun -SuiteName "k8s_deploy contract")
