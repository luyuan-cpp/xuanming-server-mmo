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

# ─────────────────────────────────────────────────────────────────
# C++ gRPC 客户端 deadline 预算(docs/design/grpc-client-deadline-failure-callback.md §4.2 / §4.4)
# ─────────────────────────────────────────────────────────────────
# 上面守的是 scene-manager 服务端 Timeout 盖住它自己的内部预算;这里守反方向:C++ deadline ≥ 目标 Go 服务的
# zrpc Timeout + 2000(上游比下游宽),以及 node ConfigMap 把 GrpcClient 块原样搬进集群。
# 判定规则只有一份,在 k8s_deploy.ps1:Get-GrpcClientDeadlineBudgetViolations(纯函数)+ Assert-GrpcClientDeadlineBudget
# (读文件、不满足即 throw,由写路径入口调用)。这里按 AST 把两个函数抽出来直接调用,不另抄一份判定,也不执行脚本入口
# (与 k8s_migrate_gate.tests.ps1 同一做法)。它们用到的 ConvertFrom-YamlToFlatMap 来自 deploy_capture.ps1 dot-source 的
# release_common.ps1;Assert 读 $RepoRoot,由各用例自己设。
$deadlineParseTokens = $null
$deadlineParseErrors = $null
$deadlineDeployAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$deadlineParseTokens, [ref]$deadlineParseErrors)
if ($deadlineParseErrors.Count -gt 0) { throw "k8s_deploy.ps1 语法错误: $($deadlineParseErrors.Message -join '; ')" }
$deadlineFunctionNames = @('Get-GrpcClientDeadlineBudgetViolations', 'Assert-GrpcClientDeadlineBudget')
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

Test-Case "C++ gRPC deadline 预算:仓库配置满足 deadline ≥ 目标 Go 服务 zrpc Timeout + 2000,五个目标全部显式配置(部署门禁对当前配置放行)" {
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

Test-Case "K8s 生成物同样满足 deadline 预算:node-config 的 GrpcClient × zone 内 go-svc ConfigMap 实际写出的 Timeout(login 镜像 login.yaml、data-service 不写)" {
    # 部署门禁比对的是服务 yaml;这里再按集群里真正生效的那份核一遍不等式,防的是生成器与门禁悄悄分家:
    # scene-manager / login 的 Timeout 镜像被改回常数(login 以前就是模板里写死 100000),或 data-service 的 ConfigMap
    # 某天写出一个服务 yaml 里没有的 Timeout(它现在不写 = go-zero 默认 2000)。ConfigMap 键改名会让 Timeout 落回
    # 默认 2000、本条反而放行 —— 那一类由上面 login / scene-manager 的「ConfigMap 值 == 服务 yaml」逐键用例兜住(查不到键即失败)。
    # 全局服务(match / 路由服)不在 zone-up 产物里,它们的 ConfigMap Timeout 由 Get-AuthoritativeScalar 镜像服务 yaml,已被上面的门禁用例覆盖。
    $deployScalars = ConvertTo-UnprefixedScalars -Scalars (ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name 'node-config')).Scalars -Prefix 'data.base_deploy_config.yaml.'
    $targets = [ordered]@{}
    foreach ($t in @(
        @{ Target = 'SceneManagerNodeService'; ConfigMap = 'go-svc-scene-manager-config'; Prefix = 'data.scene_manager_service.yaml.' }
        @{ Target = 'LoginNodeService';        ConfigMap = 'go-svc-login-config';         Prefix = 'data.login.yaml.' }
        @{ Target = 'DataServiceNodeService';  ConfigMap = 'go-svc-data-service-config';  Prefix = 'data.data_service.yaml.' }
    )) {
        $flat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $devOut -Name $t.ConfigMap)
        $targets[$t.Target] = @{ Source = "K8s ConfigMap $($t.ConfigMap)"; Scalars = (ConvertTo-UnprefixedScalars -Scalars $flat.Scalars -Prefix $t.Prefix) }
    }
    $violations = @(Get-GrpcClientDeadlineBudgetViolations -DeployScalars $deployScalars -Targets $targets)
    Assert-True -Condition ($violations.Count -eq 0) -Because ("K8s 生成物违例:{0}" -f ($violations -join ';'))
}

Test-Case "自检:deadline 预算判定对 修复前的 2500 / 差 1ms / 缺席 / 0 / 非数字 / 服务端 Timeout 0 / 非数字 / MethodTimeouts 报违例并点名目标,对等号边界通过(守卫不得静默放行)" {
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
        @{ Why = 'MethodTimeouts(只按全局 Timeout 核对)'; Deploy = "${deployWith}10000"; Service = "Timeout: 2000`nMethodTimeouts:`n  - FullMethod: /dataservice.DataService/AllocateIdSegment`n    Timeout: 8s" }
    )
    foreach ($c in $violationCases) {
        $got = @(& $check $c.Deploy $c.Service)
        Assert-True -Condition ($got.Count -gt 0) -Because "$($c.Why) 必须报违例"
        Assert-Match -Text $got[0] -Pattern '^DataServiceNodeService:' -Because "$($c.Why) 的违例必须点名是哪个目标"
    }
}

Test-Case "负向:DataService deadline 退回修复前的 2500 时部署门禁必须 throw 并只点名该项(夹具是临时目录里的配置副本,不碰仓库)" {
    $tempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('grpc-deadline-budget-' + [guid]::NewGuid().ToString('N'))
    try {
        foreach ($rel in @(
            'bin/etc/base_deploy_config.yaml'
            'go/scene_manager/etc/scene_manager_service.yaml'
            'go/data_service/etc/data_service.yaml'
            'go/client_rpc_router/etc/client_rpc_router.yaml'
            'go/match/etc/match_service.yaml'
            'go/login/etc/login.yaml'
        )) {
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
        Assert-NotMatch -Text $message -Pattern 'SceneManagerNodeService|ClientRpcRouterNodeService|MatchNodeService|LoginNodeService' -Because '其余四项仍满足,只点名不满足的那一项'
    }
    finally {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
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
        Assert-Match -Text $block -Pattern 'mmorpg\.io/cpp-log-sidecar-config-hash: [0-9a-f]{12}' -Because '配置只改 ConfigMap 不会让 Alloy 重读,必须靠 pod 模板注解触发滚动'
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
    Assert-Match -Text $block -Pattern 'startupProbe:\s+httpGet:\s+path: /actuator/health/readiness\s+port: 8081\s+periodSeconds: 10\s+timeoutSeconds: 5\s+failureThreshold: 60' -Because 'Spring/JPA 初始化期间不能被 liveness 的短失败预算反复杀死'
    Assert-Match -Text $block -Pattern 'readinessProbe:\s+httpGet:\s+path: /actuator/health/readiness\s+port: 8081\s+initialDelaySeconds: 15\s+periodSeconds: 10' -Because '冷启动后仍必须保留 HTTP readiness 门禁'
    Assert-Match -Text $block -Pattern 'livenessProbe:\s+httpGet:\s+path: /actuator/health/liveness\s+port: 8081\s+initialDelaySeconds: 30\s+periodSeconds: 20' -Because 'startupProbe 不能取代运行期存活检查'
    # liveness 不能落到聚合的 /actuator/health:那会把 DB / Redis 等外部依赖的抖动算进存活,依赖一抖就重启全部副本。
    Assert-NotMatch -Text $block -Pattern 'path: /actuator/health\s' -Because '探针必须走 readiness / liveness 分组,不能用聚合健康端点'
}
Test-Case 'Java gateway 必须显式限制 JVM 堆并为 native 内存保留容器预算' {
    $block = Select-ManifestByName -Output $devOut -Name 'gateway'
    Assert-Match -Text $block -Pattern 'name: JAVA_TOOL_OPTIONS\s+value: "-Xms64m -Xmx384m"' -Because '不能依赖本机 JRE 对 cgroup 内存限制的自动识别'
    Assert-Match -Text $block -Pattern 'requests:\s+cpu: 200m\s+memory: 512Mi' -Because '调度请求必须覆盖已观测的启动期常驻内存'
    Assert-Match -Text $block -Pattern 'limits:\s+cpu: "1"\s+memory: 1Gi' -Because '堆外内存包括 metaspace、线程栈与直接缓冲区，不能把最大堆等同容器内存'
}
exit (Complete-TestRun -SuiteName "k8s_deploy contract")
