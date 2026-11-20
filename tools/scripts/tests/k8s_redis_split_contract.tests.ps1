#requires -Version 7
<#
.SYNOPSIS
    Redis 运维命令(redis-split-status、redis-wipe)与按用途拆实例退役的部署侧契约测试。

.DESCRIPTION
    按用途、按 zone 组拆出的单主实例(G4a 第 1a 批的排队实例 redis-queue-g<N>、缺口第 5 条的控制域实例 redis-ctl-g<N>)、它们的模板
    redis-purpose*.yaml,以及 -RedisSplit / -ConfirmRedisSplitSwitch / -RedisQueueMaxMemory / -RedisCtlMaxMemory / -ConfirmRedisCtlSwitch
    五个参数随缺口第 34 条第 6b 批退役(docs/design/redis-cluster-all-domains-20261110.md §10):控制域全部在控制端点
    (staging / prod 是 Redis Cluster redis-control,dev 是核心单实例),玩家域在 redis-player。文件名沿用(deploy/k8s/README.md 与交接单引用它)。
    本文件钉住:
      1. 退役:五个参数在 k8s_deploy / k8s_image / dev_tools 三个参数块里都没有,也没有脚本再读它们;给了就在参数绑定处失败、早于任何写操作;
         两份模板已删;缺省 zone-up / infra-up 的产物里没有 redis-queue-g / redis-ctl-g,login 不写 Node.QueueRedis;
      2. redis-wipe:准入(namespace 规则 + 确认)与 exec 输出解析;DryRun 只打计划;kubectl 替身下只清名单里实际存在的 StatefulSet
         (核心 redis、redis-match-cluster、两套 Redis Cluster),名单外的(含没清理的 redis-queue-g<N>)不碰,主不在序号 0 也清得到;
      3. redis-split-status:DryRun 只打计划;线上事实注入时两套集群各打一行,没部署打 <absent> 不算失败,集群的问题进失败列表;
      4. Read-RedisClusterLiveStatus / ConvertFrom-RedisClusterInfoText 的判定。
    子进程一律 -DryRun(不连集群);纯函数按 AST 从 k8s_deploy.ps1 抽出,在本进程里直接调用;读集群的函数在用例里换成替身。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/k8s_redis_split_contract.tests.ps1
#>

$ErrorActionPreference = "Stop"

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ""
Write-Host "=== k8s_deploy.ps1 Redis ops commands and split retirement contract tests ==="

$TempRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-redis-split-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $TempRoot | Out-Null

# 与 k8s_deploy_contract.tests.ps1 同一套 DryRun 夹具:虚构的全局 SQL 入口与库名白名单,只用于 DryRun,不解析也不连接。
function Invoke-SplitDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '本套夹具只允许显式 -DryRun,禁止连接或写集群。' }
    $fixtureEnv = @{
        MMORPG_DB_ALLOWED_DATABASES = $PlayerStoreAllowlistFixture
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
# infra-up:与 k8s_deploy_contract.tests.ps1 的 $BattleInfraArgs 同形(不部署 Go / Java 服务)。
$InfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-SkipGoSvc', '-SkipJavaSvc', '-WaitReady', '-InfraNamespace', 'split-infra',
    '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab')
$InfraEnv = @{ MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345' }

$defaultZoneRun = Invoke-SplitDryRun -Arguments $ZoneArgs
$defaultInfraRun = Invoke-SplitDryRun -Arguments $InfraArgs -Env $InfraEnv

# ─────────────────────────────────────────────────────────────────
# 1. 退役:参数、模板与产物
# ─────────────────────────────────────────────────────────────────

$RetiredSplitParams = @('RedisSplit', 'ConfirmRedisSplitSwitch', 'RedisQueueMaxMemory', 'RedisCtlMaxMemory', 'ConfirmRedisCtlSwitch')

Test-Case '退役:-RedisSplit 等五个参数在 k8s_deploy / k8s_image / dev_tools 的参数块里都没有,也没有脚本再读它们;给了就在参数绑定处失败,早于任何写操作' {
    foreach ($scriptName in @('k8s_deploy.ps1', 'k8s_image.ps1', 'dev_tools.ps1')) {
        $ast = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) $scriptName), [ref]$null, [ref]$null)
        $params = @($ast.ParamBlock.Parameters | ForEach-Object { $_.Name.VariablePath.UserPath })
        foreach ($name in $RetiredSplitParams) {
            Assert-True -Condition ($params -notcontains $name) -Because "$scriptName 的参数块不该再有 -$name"
        }
        # 变量(含 $script: 限定)、转发时写的 -参数名、按名字取 $PSBoundParameters 的字符串键,三种写法都算残留。
        $reads = @($ast.FindAll({ param($a)
                    ($a -is [System.Management.Automation.Language.VariableExpressionAst] -and $RetiredSplitParams -contains $a.VariablePath.UnqualifiedPath) -or
                    ($a -is [System.Management.Automation.Language.CommandParameterAst] -and $RetiredSplitParams -contains $a.ParameterName) -or
                    ($a -is [System.Management.Automation.Language.StringConstantExpressionAst] -and $RetiredSplitParams -contains $a.Value)
                }, $true))
        Assert-Equal -Expected 0 -Actual $reads.Count -Because "$scriptName 不再读、不再透传这几个参数(残留:$(@($reads | ForEach-Object { $_.Extent.Text }) -join ', '))"
    }
    $run = Invoke-SplitDryRun -Arguments ($ZoneArgs + @('-RedisSplit', 'queue'))
    Assert-True -Condition ($run.ExitCode -ne 0) -Because "已删的参数不能被静默接受。输出: $($run.Output)"
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl [^\r\n]*\b(apply|create|delete|patch|replace)\b' -Because '参数绑定失败先于任何写操作'
    foreach ($template in @('redis-purpose.yaml', 'redis-purpose-ha.yaml')) {
        Assert-True -Condition (-not (Test-Path -LiteralPath (Join-Path (Get-RepoRoot) "deploy/k8s/manifests/infra/$template"))) -Because "$template 随按用途拆实例退役"
    }
}

Test-Case '缺省 zone-up / infra-up DryRun:产物里没有 redis-queue-g / redis-ctl-g;login 不写 Node.QueueRedis;dev 没开控制集群时 login 的 ControlRedis 与 gateway 都指向核心' {
    foreach ($case in @(@{ Label = 'zone-up'; Run = $defaultZoneRun }, @{ Label = 'infra-up'; Run = $defaultInfraRun })) {
        Assert-Equal -Expected 0 -Actual $case.Run.ExitCode -Because "$($case.Label) DryRun 必须成功。输出: $($case.Run.Output)"
        # 整行注释除外:清单与部署说明的注释可能提到退役的实例名。
        $effective = @(($case.Run.Output -split "`r?`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
        Assert-NotMatch -Text $effective -Pattern 'redis-(queue|ctl)-g' -Because "$($case.Label):按用途拆出的实例已退役,不渲染、不操作"
    }
    $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $defaultZoneRun.Output -Name 'go-svc-login-config')
    $queueKeys = @($loginFlat.Scalars.Keys | Where-Object { ([string]$_).StartsWith('data.login.yaml.Node.QueueRedis', [System.StringComparison]::Ordinal) })
    Assert-Equal -Expected 0 -Actual $queueKeys.Count -Because '排队、准入与放行锁和 gate 标记同在控制域(键带 {q:<zone>}),不再单独拆实例'
    Assert-Equal -Expected "redis.${Infra}:6379" -Actual (Get-FlatValue -Flat $loginFlat -KeyPath 'data.login.yaml.ControlRedis.Addrs[0]') -Because 'dev 没开控制集群,控制端点是核心单实例'
    $gatewayFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $defaultZoneRun.Output -Name 'java-svc-gateway-config')
    Assert-Equal -Expected "redis.$Infra" -Actual (Get-FlatValue -Flat $gatewayFlat -KeyPath 'data.application.yaml.spring.data.redis.host') -Because 'gateway 的限流在控制域;dev 是核心单实例'
}

# ─────────────────────────────────────────────────────────────────
# 2. 纯函数与替身(按 AST 从 k8s_deploy.ps1 抽出)
# ─────────────────────────────────────────────────────────────────

$deployParseErrors = $null
$deployAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$deployParseErrors)
if ($deployParseErrors.Count -gt 0) { throw "k8s_deploy.ps1 解析失败:$($deployParseErrors[0].Message)" }
$splitFunctionNames = @(
    'Get-RedisWipeRefusal', 'ConvertFrom-RedisWipeProbe', 'Invoke-RedisWipe', 'Show-RedisSplitStatus',
    # 两套 Redis Cluster 的名字取自集群描述,线上状态走 Invoke-ClientEntryKubectl 替身。
    'Get-RedisCluster', 'Get-RedisClusters', 'ConvertFrom-RedisClusterInfoText', 'Read-RedisClusterLiveStatus'
)
foreach ($fnAst in $deployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($splitFunctionNames -contains $fnAst.Name) {
        Set-Item -Path "Function:script:$($fnAst.Name)" -Value $fnAst.Body.GetScriptBlock()
    }
}
foreach ($fnName in $splitFunctionNames) {
    if (-not (Test-Path "Function:$fnName")) { throw "k8s_deploy.ps1 缺少函数 ${fnName}(Redis 运维命令被删了?后续断言无意义)" }
}
# 被抽出的函数读的脚本级常量,取 k8s_deploy.ps1 里的同一条赋值,不在测试里另写一份。
$deployTopLevelAssignments = @($deployAst.FindAll({ param($a) $a -is [System.Management.Automation.Language.AssignmentStatementAst] }, $false))
foreach ($constantName in @('RedisWipeNamespacePattern')) {
    $constantAssignment = @($deployTopLevelAssignments | Where-Object { $_.Left.Extent.Text -ceq "`$script:$constantName" })
    if ($constantAssignment.Count -ne 1) { throw "k8s_deploy.ps1 顶层 `$script:$constantName 的赋值应当恰好 1 处,实际 $($constantAssignment.Count) 处" }
    . ([scriptblock]::Create($constantAssignment[0].Extent.Text))
}

# 读集群的替身:调用记进 $script:KubectlCalls,回复由 $script:KubectlResponder 给。
$script:KubectlCalls = @()
$script:KubectlResponder = $null
function Invoke-ClientEntryKubectl {
    param(
        [Parameter(Mandatory = $true)][string[]]$KubectlArgs,
        [AllowEmptyString()][string]$KubeContext,
        [AllowEmptyString()][string]$KubeConfig,
        [switch]$AllowFailure,
        [switch]$CaptureOutput
    )
    $joined = $KubectlArgs -join ' '
    $script:KubectlCalls += $joined
    return (& $script:KubectlResponder $joined)
}
function New-KubectlReply {
    param([int]$ExitCode = 0, [string]$Stdout = '', [string]$Stderr = '')
    return [pscustomobject]@{ ExitCode = $ExitCode; Stdout = $Stdout; Stderr = $Stderr }
}

# ─────────────────────────────────────────────────────────────────
# 3. redis-wipe
# ─────────────────────────────────────────────────────────────────

Test-Case 'redis-wipe 准入:namespace 必须匹配 ^mmorpg-stress-(区分大小写)且带 -ConfirmWipeAllRedis;exec 输出解析' {
    $pattern = '^mmorpg-stress-'
    Assert-Equal -Expected $pattern -Actual $script:RedisWipeNamespacePattern -Because '规则写死在 k8s_deploy.ps1'
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

Test-Case 'redis-wipe DryRun:非压测 namespace 或缺确认即拒绝;两条都满足只打计划,不 exec' {
    $refused = Invoke-SplitDryRun -Arguments @('-Command', 'redis-wipe', '-DryRun', '-InfraNamespace', 'mmorpg-infra', '-ConfirmWipeAllRedis')
    Assert-True -Condition ($refused.ExitCode -ne 0) -Because "非压测 namespace 必须拒绝。输出: $($refused.Output)"
    Assert-Match -Text $refused.Output -Pattern '拒绝 redis-wipe' -Because '错误写明拒绝原因'
    $noConfirm = Invoke-SplitDryRun -Arguments @('-Command', 'redis-wipe', '-DryRun', '-InfraNamespace', 'mmorpg-stress-contract')
    Assert-True -Condition ($noConfirm.ExitCode -ne 0) -Because "缺 -ConfirmWipeAllRedis 必须拒绝。输出: $($noConfirm.Output)"
    Assert-Match -Text $noConfirm.Output -Pattern '没给 -ConfirmWipeAllRedis' -Because '错误点名缺确认'
    $plan = Invoke-SplitDryRun -Arguments @('-Command', 'redis-wipe', '-DryRun', '-InfraNamespace', 'mmorpg-stress-contract', '-ConfirmWipeAllRedis')
    Assert-Equal -Expected 0 -Actual $plan.ExitCode -Because "准入满足时 DryRun 成功。输出: $($plan.Output)"
    Assert-Match -Text $plan.Output -Pattern '\[dry-run\] redis-wipe would list StatefulSets redis / redis-match-cluster / redis-player / redis-control in namespace mmorpg-stress-contract' `
        -Because 'DryRun 只打计划;核心、match 集群与两套 Redis Cluster(玩家域、控制)的每个主节点都要清'
    Assert-NotMatch -Text $plan.Output -Pattern 'redis-(queue|ctl)-g' -Because '退役的实例不在清单里'
    Assert-NotMatch -Text $plan.Output -Pattern '\[dry-run\] kubectl ' -Because 'DryRun 不发任何 kubectl'
}

Test-Case 'redis-wipe(kubectl 替身):只清名单里实际存在的 StatefulSet,逐个 Pod exec -c redis;集群的主不在序号 0 也清得到,从只报 DBSIZE;名单外的(含没清理的 redis-queue-g0)不碰' {
    # 失败分支是 exit 1,在本进程里跑会结束整个测试:这里只走全部通过的路径,判定逻辑由「redis-wipe 准入」与下面的断言覆盖。
    $InfraNamespace = 'mmorpg-stress-wipe'; $KubeContext = ''; $KubeConfig = ''; $DryRun = $false; $ConfirmWipeAllRedis = $true
    $script:KubectlCalls = @()
    $script:KubectlResponder = {
        param([string]$Joined)
        if ($Joined -like 'get statefulset -n *') {
            return New-KubectlReply -Stdout ('{"items":[{"metadata":{"name":"redis"},"spec":{"replicas":1}},{"metadata":{"name":"redis-player"},"spec":{"replicas":6}},' +
                '{"metadata":{"name":"redis-queue-g0"},"spec":{"replicas":3}},{"metadata":{"name":"kafka"},"spec":{"replicas":3}}]}')
        }
        if ($Joined -like '*pod/redis-0 *' -or $Joined -like '*pod/redis-player-1 *' -or $Joined -like '*pod/redis-player-3 *' -or $Joined -like '*pod/redis-player-5 *') {
            return New-KubectlReply -Stdout "role=master`nflushall=OK`ndbsize=0"
        }
        return New-KubectlReply -Stdout "role=slave`ndbsize=5"
    }
    # 逐条取消息文本,不过 Out-String:它可能按控制台宽度折行,断言就落不到一行上。
    $printed = (@(Invoke-RedisWipe 6>&1) | ForEach-Object { $_.ToString() }) -join "`n"
    Assert-Match -Text $printed -Pattern 'Redis wipe: namespace=mmorpg-stress-wipe instances=redisx1,redis-playerx6(\r?\n|$)' -Because '核心 1 个 Pod、玩家域集群 6 个节点;redis-queue-g0 与 kafka 不在名单里'
    Assert-Match -Text $printed -Pattern '\[wiped\] redis-player-1 role=master flushall=OK dbsize=0' -Because '集群的主可能不在序号 0 上,逐主节点 FLUSHALL'
    Assert-Match -Text $printed -Pattern '\[checked\] redis-player-0 role=slave dbsize=5' -Because '从的 DBSIZE 只打印,不作判据(复制有延迟)'
    Assert-Match -Text $printed -Pattern 'Redis wipe OK: every primary of 2 instance\(s\)' -Because '全部主节点通过'
    $execs = @($script:KubectlCalls | Where-Object { $_ -like 'exec *' })
    Assert-Equal -Expected 7 -Actual $execs.Count -Because '核心 1 个 + 集群 6 个 Pod 各 exec 一次'
    foreach ($pod in @('redis-0') + @(0..5 | ForEach-Object { "redis-player-$_" })) {
        Assert-Equal -Expected 1 -Actual @($execs | Where-Object { $_ -like "exec -n mmorpg-stress-wipe pod/$pod -c redis -- *" }).Count -Because "pod/$pod 要显式 -c redis(Pod 里还有 exporter)"
    }
    Assert-Equal -Expected 0 -Actual @($script:KubectlCalls | Where-Object { $_ -match 'redis-queue-g0|kafka' }).Count -Because '名单外的 StatefulSet 不 exec'
}

# ─────────────────────────────────────────────────────────────────
# 4. redis-split-status
# ─────────────────────────────────────────────────────────────────

Test-Case 'redis-split-status DryRun:只打计划(两套 Redis Cluster 的 StatefulSet 与 CLUSTER INFO),不连集群,不再逐 zone 读 ConfigMap' {
    $run = Invoke-SplitDryRun -Arguments @('-Command', 'redis-split-status', '-DryRun')
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "DryRun 必须成功。输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern '\[dry-run\] would read statefulset redis-player / redis-control in namespace \S+ and CLUSTER INFO from pod <name>-0; absent is not a failure' `
        -Because '两套集群只打计划;没部署不算失败'
    Assert-NotMatch -Text $run.Output -Pattern 'redis-(queue|ctl)-g|would read ConfigMaps' -Because '按 zone 组分的实例已退役,不再逐 zone 读 ConfigMap'
    Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl ' -Because 'DryRun 不发任何 kubectl'
}

Test-Case 'Show-RedisSplitStatus(线上事实注入):两套集群各打一行;没部署打 <absent> 不算失败;只读两套集群;集群的问题进失败列表' {
    # 失败分支是 exit 1,在本进程里跑会结束整个测试:这里走全部正常的路径,问题判定由下面 Read-RedisClusterLiveStatus 的用例覆盖,接线用函数体断言钉住。
    $InfraNamespace = 'ns'; $DryRun = $false; $KubeContext = ''; $KubeConfig = ''
    $script:KubectlCalls = @()
    $script:KubectlResponder = {
        param([string]$Joined)
        if ($Joined -like 'get statefulset redis-player -n ns --ignore-not-found -o json') { return New-KubectlReply -Stdout '{"spec":{"replicas":6},"status":{"readyReplicas":6}}' }
        if ($Joined -like 'exec -n ns pod/redis-player-0 -c redis -- *') {
            return New-KubectlReply -Stdout "cluster_state:ok`r`ncluster_slots_assigned:16384`r`ncluster_slots_ok:16384`r`ncluster_known_nodes:6`r`n"
        }
        if ($Joined -like 'get statefulset redis-control -n ns --ignore-not-found -o json') { return New-KubectlReply }
        throw "kubectl 替身没想到的调用:$Joined"
    }
    $printed = (@(Show-RedisSplitStatus 6>&1) | ForEach-Object { $_.ToString() }) -join "`n"
    Assert-Match -Text $printed -Pattern 'Redis split status: OK' -Because '玩家域集群健康,控制集群没部署'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($printed, 'cluster=redis-player namespace=ns ready=6/6 state=ok slots_ok=16384 known_nodes=6').Count) -Because '每套集群打一行'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($printed, 'cluster=redis-control namespace=ns <absent>').Count) -Because '没部署的集群打 <absent>,不算失败'
    Assert-Equal -Expected 3 -Actual $script:KubectlCalls.Count -Because '每套集群一次 get,在的那套再 exec 一次 CLUSTER INFO;不读任何 zone 的对象'
    $body = ($deployAst.Find({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq 'Show-RedisSplitStatus' }, $true)).Body.Extent.Text
    Assert-Match -Text $body -Pattern 'foreach \(\$problem in \$clusterStatus\.Problems\) \{ \$failures\.Add' -Because '集群的问题要进失败列表(退出码 1),不能只打印'
}

Test-Case 'Read-RedisClusterLiveStatus / ConvertFrom-RedisClusterInfoText(kubectl 替身):没部署不算问题;没就绪、读不出 CLUSTER INFO、state 不是 ok、slot 不全、节点数对不上都算问题;kubectl 失败与非 JSON 一律 throw' {
    $KubeContext = ''; $KubeConfig = ''
    $cluster = Get-RedisCluster -Kind 'control'
    $healthyInfo = "cluster_state:ok`ncluster_slots_assigned:16384`ncluster_slots_ok:16384`ncluster_known_nodes:6`n"
    $script:ClusterCase = $null
    $script:KubectlResponder = {
        param([string]$Joined)
        if ($Joined -like 'get statefulset redis-control -n infra-ns --ignore-not-found -o json') { return $script:ClusterCase.StatefulSet }
        if ($Joined -like 'exec -n infra-ns pod/redis-control-0 -c redis -- sh -c *') { return $script:ClusterCase.Exec }
        throw "kubectl 替身没想到的调用:$Joined"
    }

    $script:ClusterCase = @{ StatefulSet = (New-KubectlReply); Exec = $null }
    $absent = Read-RedisClusterLiveStatus -Cluster $cluster -Namespace 'infra-ns'
    Assert-Equal -Expected 'False/0' -Actual ('{0}/{1}' -f $absent.Present, @($absent.Problems).Count) -Because '--ignore-not-found 的空输出 = 没部署,不算问题(建不建由部署参数决定)'

    $script:ClusterCase = @{ StatefulSet = (New-KubectlReply -Stdout '{"spec":{"replicas":6},"status":{"readyReplicas":6}}'); Exec = (New-KubectlReply -Stdout $healthyInfo) }
    $healthy = Read-RedisClusterLiveStatus -Cluster $cluster -Namespace 'infra-ns'
    Assert-Equal -Expected 'True/6/6/ok/16384/6/0' -Actual ('{0}/{1}/{2}/{3}/{4}/{5}/{6}' -f $healthy.Present, $healthy.Replicas, $healthy.Ready, $healthy.Info.State, $healthy.Info.SlotsOk, $healthy.Info.KnownNodes, @($healthy.Problems).Count) `
        -Because '全部就绪、16384 个 slot 可用、6 个节点都认识'

    $readySts = '{"spec":{"replicas":6},"status":{"readyReplicas":6}}'
    foreach ($case in @(
            @{ Label = 'Pod 没全部就绪'; Sts = '{"spec":{"replicas":6},"status":{"readyReplicas":5}}'; Exec = (New-KubectlReply -Stdout $healthyInfo); Pattern = 'statefulset/redis-control ready 5/6' },
            @{ Label = 'status 里还没有 readyReplicas'; Sts = '{"spec":{"replicas":6},"status":{}}'; Exec = (New-KubectlReply -Stdout $healthyInfo); Pattern = 'ready 0/6' },
            @{ Label = 'exec 失败'; Sts = $readySts; Exec = (New-KubectlReply -ExitCode 1 -Stderr 'container not found'); Pattern = 'CLUSTER INFO not readable \(exit code 1\): container not found' },
            @{ Label = '输出认不出(口令错)'; Sts = $readySts; Exec = (New-KubectlReply -Stdout 'NOAUTH Authentication required.'); Pattern = 'CLUSTER INFO not readable' },
            @{ Label = 'cluster_state:fail'; Sts = $readySts; Exec = (New-KubectlReply -Stdout ($healthyInfo -replace 'cluster_state:ok', 'cluster_state:fail')); Pattern = "cluster_state is 'fail'" },
            @{ Label = '一片没有可用的主(full-coverage no 时 state 仍是 ok)'; Sts = $readySts; Exec = (New-KubectlReply -Stdout ($healthyInfo -replace 'cluster_slots_ok:16384', 'cluster_slots_ok:10923')); Pattern = 'cluster_slots_ok=10923, expected 16384' },
            @{ Label = '扩容的新节点还没入群'; Sts = '{"spec":{"replicas":9},"status":{"readyReplicas":9}}'; Exec = (New-KubectlReply -Stdout $healthyInfo); Pattern = 'cluster_known_nodes=6 but the StatefulSet has 9 replicas' }
        )) {
        $script:ClusterCase = @{ StatefulSet = (New-KubectlReply -Stdout $case.Sts); Exec = $case.Exec }
        $r = Read-RedisClusterLiveStatus -Cluster $cluster -Namespace 'infra-ns'
        Assert-True -Condition $r.Present -Because "$($case.Label):StatefulSet 在"
        Assert-Match -Text (@($r.Problems) -join ' | ') -Pattern $case.Pattern -Because "$($case.Label) 必须算问题"
    }

    foreach ($case in @(@{ Reply = (New-KubectlReply -ExitCode 1 -Stderr 'forbidden'); Pattern = 'forbidden' }, @{ Reply = (New-KubectlReply -Stdout 'not json'); Pattern = '不是合法 JSON' })) {
        $script:ClusterCase = @{ StatefulSet = $case.Reply; Exec = $null }
        $message = ''
        try { $null = Read-RedisClusterLiveStatus -Cluster $cluster -Namespace 'infra-ns' } catch { $message = $_.Exception.Message }
        Assert-Match -Text $message -Pattern $case.Pattern -Because '读不到不等于没部署,不能当成集群不存在'
    }
    $unparsed = ConvertFrom-RedisClusterInfoText -Text $null
    Assert-Equal -Expected 'False/-1' -Actual ('{0}/{1}' -f $unparsed.Parsed, $unparsed.SlotsOk) -Because '空输出认不出,数字字段是 -1 而不是 0(0 会被当成真读到的值)'
}

Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue

exit (Complete-TestRun -SuiteName "k8s redis split contract")
