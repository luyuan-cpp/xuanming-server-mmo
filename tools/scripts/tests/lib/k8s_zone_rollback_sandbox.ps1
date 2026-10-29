#requires -Version 7
<#
.SYNOPSIS
    k8s_zone_rollback.ps1 的进程内沙箱(契约测试共用):每一步都落在假下游上,不碰任何集群、Redis、Kafka、MySQL。

.DESCRIPTION
    k8s_zone_rollback_gate_router_mode.tests.ps1 与 k8s_zone_rollback_discard.tests.ps1 都 dot-source 本文件,沙箱只有这一份。
    调用方须先 dot-source test_harness.ps1 与 deploy_capture.ps1(Get-ToolsScriptsDir / Get-RepoRoot / Get-YamlScalar)。

    2026-10-18 起(回档丢弃语义,负责人 2026-10-17 裁决)沙箱另提供:假回档工具 zone_rollback_discard(.ps1,记 argv、按 -mode 返回可配置的退出码)、
    假 kafka-get-offsets.sh(两次读数可配置是否相等)、假 kafka-consumer-groups.sh 对 --state 的回应与 Step 5 之后 zone 组的 describe(LAG 可配置)、
    假 kubectl 对第 0 步 f 部署快照与 Step 1 之后 namespace 检查的回应、假 Start-Sleep(只记账不等),以及按调用顺序记下全部下游的时间线。
    同日回档复核修正版另加:假工具的 -version 与 -expect-run-id 核对、假 kubectl 对 redis-0 INFO server 的回应、Step 4a 之后才生效的
    namespace / --state 回应(模拟 PITR 期间写者回来),以及 New-RollbackDeploySnapshotReply 的 -GroupLines(final(Z) / 交接形状)与 -DbImage。
    2026-10-22(B0-4 第 0 步 nofile 核对)另加:假 dev_tools.ps1 的 -NodeOpenFileLimitMode、假 k8s_deploy.ps1 的同名默认值、
    假 kubectl 对 Agones 探测与 scene Fleet 读取的回应;沙箱缺省显式传 -NodeOpenFileLimitMode enforce(见该参数)。
#>

Set-StrictMode -Off

. (Join-Path (Get-ToolsScriptsDir) 'lib' 'kafka_consumer_groups.ps1')

$script:RollbackSandboxNamespace = 'mmorpg-zone-contract-test'
$script:RollbackSandboxTargetTime = '2026-05-15T14:23:00Z'
$script:RollbackSandboxDsnEnv = 'MMORPG_CONTRACT_ROLLBACK_DSN'
$script:RollbackSandboxDbImage = 'ghcr.io/luyuancpp/mmorpg-db:0123456789ab'
$script:RollbackSandboxLiveRedis = 'redis.mmorpg-infra.svc.cluster.local:6379'
# 第 0 步 f 经 kubectl exec 读到的 go/db Redis run_id(回档复核 [4]),与假工具缺省「看到」的 run_id 相同。
$script:RollbackSandboxRedisRunId = 'aa11bb22cc33dd44ee55ff6677889900aabbccdd'
# 假工具 -version 缺省报的构建提交:以沙箱 db 镜像的 tag 0123456789ab 开头(回档复核 [5])。
$script:RollbackSandboxToolCommit = '0123456789ab' + ('0' * 28)

<#
.SYNOPSIS
    第 0 步 f 那次 kubectl(部署快照)的假回应:go-svc-db-config / go-svc-data-service-config 与 db / login Deployment 合成一个 List。
    db.yaml 里带一个假口令,用来断言回档脚本从不读它、不落盘。
#>
function New-RollbackDeploySnapshotReply {
    param(
        # 三项都可为空串:沙箱 db.yaml 缺键的用例(被测脚本在入口即拒)也要能生成快照。
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$GroupBase,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$TopicGeneration,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$PartitionCnt,
        [string]$ZoneId = '101',
        [string]$FencingMode = 'legacy',
        [string]$RedisDB = '0',
        [string]$MappingHost = $script:RollbackSandboxLiveRedis,
        [string[]]$ExtraDbYaml = @(),
        # ServerConfig.Kafka 下的组名键(4 格缩进的整行),缺省 = final(L) 一行 GroupID: "<GroupBase>"。
        # final(Z) / 交接形状 / 第三个组名的用例自己给(k8s_deploy.ps1 只写四种形状,回档复核 [0])。
        [string[]]$GroupLines = $null,
        [string]$DbImage = $script:RollbackSandboxDbImage
    )

    if ($null -eq $GroupLines) { $GroupLines = @("    GroupID: `"$GroupBase`"") }
    $dbYaml = @(
        "ZoneId: $ZoneId",
        'ServerConfig:',
        '  Kafka:') + $GroupLines + @(
        "    TopicGeneration: $TopicGeneration",
        "    PartitionCnt: $PartitionCnt",
        '  RedisClient:',
        "    Hosts: `"$($script:RollbackSandboxLiveRedis)`"",
        '    Password: "contract-live-secret-never-read"',
        "    DB: $RedisDB",
        '  Fencing:',
        "    Mode: $FencingMode"
    ) + $ExtraDbYaml
    $dataServiceYaml = @('MappingRedis:', "  Host: $MappingHost", '  Type: node')
    $items = @(
        [ordered]@{ apiVersion = 'v1'; kind = 'ConfigMap'; metadata = [ordered]@{ name = 'go-svc-db-config' }; data = [ordered]@{ 'db.yaml' = ($dbYaml -join "`n") } },
        [ordered]@{ apiVersion = 'v1'; kind = 'ConfigMap'; metadata = [ordered]@{ name = 'go-svc-data-service-config' }; data = [ordered]@{ 'data_service.yaml' = ($dataServiceYaml -join "`n") } },
        [ordered]@{ apiVersion = 'apps/v1'; kind = 'Deployment'; metadata = [ordered]@{ name = 'db' }; spec = [ordered]@{ template = [ordered]@{ spec = [ordered]@{ containers = @([ordered]@{ name = 'db'; image = $DbImage }) } } } },
        [ordered]@{ apiVersion = 'apps/v1'; kind = 'Deployment'; metadata = [ordered]@{ name = 'login' }; spec = [ordered]@{ template = [ordered]@{ spec = [ordered]@{ containers = @([ordered]@{ name = 'login'; image = 'ghcr.io/luyuancpp/mmorpg-login:0123456789ab' }) } } } }
    )
    $list = [ordered]@{ apiVersion = 'v1'; kind = 'List'; items = $items; metadata = [ordered]@{ resourceVersion = '' } }
    return @{ ExitCode = 0; Output = ($list | ConvertTo-Json -Depth 30) }
}

<#
.SYNOPSIS
    第 0 步 a 读 login ConfigMap(go-svc-login-config)那次 kubectl 的假回应:一份最小的 login.yaml,AuthProviders 段由调用方给
    (空串 = 没开第三方登录,即缺省回应)。-Absent 回「不存在」(--ignore-not-found 下无输出、退出码 0)。
    login.yaml 里另带一个假口令,用来断言回档脚本不把 ConfigMap 里的其它值打进输出或存档。
#>
function New-RollbackLoginConfigMapReply {
    param(
        [AllowEmptyString()][string]$AuthProvidersYaml = '',
        [switch]$Absent
    )

    if ($Absent) { return @{ ExitCode = 0; Output = '' } }
    $loginYaml = @('Name: login.rpc', 'ListenOn: 0.0.0.0:50000', 'Secrets:', '  InternalAuth:', '    Value: "contract-login-secret-never-read"',
        'RequireClientEndpoint: false')
    if (-not [string]::IsNullOrEmpty($AuthProvidersYaml)) { $loginYaml += @($AuthProvidersYaml -split "`r?`n") }
    $loginYaml += @('Kafka:', '  Brokers:', '    - "kafka.contract-infra:9092"')
    $configMap = [ordered]@{ apiVersion = 'v1'; kind = 'ConfigMap'; metadata = [ordered]@{ name = 'go-svc-login-config' }; data = [ordered]@{ 'login.yaml' = ($loginYaml -join "`n") } }
    return @{ ExitCode = 0; Output = ($configMap | ConvertTo-Json -Depth 10) }
}

<#
.SYNOPSIS
    在进程内的沙箱里跑一遍 k8s_zone_rollback.ps1,返回执行结果与每个假下游的记账。

.DESCRIPTION
    沙箱是 <tmp>/<guid>/tools/scripts(被测脚本、真 lib/release_common.ps1、lib/k8s_client_entry.ps1、lib/kafka_consumer_groups.ps1 与
    假的 dev_tools.ps1 / kafka_offset_reset.ps1 / k8s_deploy.ps1 / zone_rollback_discard.ps1),外加 <tmp>/<guid>/go/db/etc/db.yaml(缺省拷仓库那份):
    被测脚本在入口按 <脚本目录>/../.. 读它推导消费组名与分区数,读不到就拒绝。报告目录落在 <tmp>/<guid>/run/logs/zone-rollback/<rid>/。
    kubectl、kafka-consumer-groups.sh、kafka-get-offsets.sh 与 Start-Sleep 用本函数作用域里的同名函数顶替(函数先于外部命令与 cmdlet 解析,
    被测脚本在子作用域里按动态作用域找到它们)。于是即使哪道检查有 bug 没拦住,-Apply 的每一步也只落在假下游上:
      第 0 步静态预检 / Step 1 / Step 6 → 假 dev_tools.ps1(记下命令与收到的 GateRouterMode,另记一行全部具名参数;
        带 -DryRun 的调用记成 "<命令>/dry-run",与 Step 6 的真实调用区分开);
      第 0 步 c / d / e、Step 1b、Step 5 之后的切点 → 假 kafka-consumer-groups.sh(缺省对每个组回「组不存在」,--state 缺省回 Empty,
        Step 5 -Apply 之后对 zone 组回每个分区 CURRENT == LOG-END 的表);第 0 步 d 的预演与 Step 5 → 假 kafka_offset_reset.ps1;
      第 0 步 f、1b、4a、4b、5b 的回档工具 → 假 zone_rollback_discard.ps1;Step 1b 的静默窗口 → 假 kafka-get-offsets.sh 与假 Start-Sleep;
      Step 3 → -SkipMySqlPause(配 -PitrRestoredTo)。
    DevToolsCalls 按调用顺序;DevToolsBound 每次调用一行 "<命令> 参数=值;…"(按参数名排序,不含 Command 与 DryRun);
    KafkaResetBound 每次调用一行 "参数=值;…"(按参数名排序);DiscardCalls 每次调用一行 "<模式> <argv 以空格拼接>";
    Timeline 是全部下游按调用先后的统一记录("dev_tools:<命令>"、"reset:apply|dry-run"、"tool:<模式>|version"、"kafka-cg:<组>[:state]"、
    "get-offsets"、"kubectl:entry|exec|snapshot|namespace|redis|login-auth|agones|fleet"、"sleep")。

.PARAMETER ExtraArgs
    额外传给被测脚本的具名参数;值为 $null 的键表示从缺省参数里去掉它(如 @{ RedisHost = $null } 测 E3)。
    缺省参数:ZoneName / ZoneId / TargetTime / NodeImage / SkipMySqlPause,以及 -Apply 所需的 RedisHost / MySqlDsnEnv / RollbackDiscardTool /
    BarrierProtocol legacy / PitrRestoredTo(= TargetTime)。

.PARAMETER DiscardExit
    假回档工具各模式的退出码,如 @{ inventory = 6 };没写的模式退出 0。

.PARAMETER KafkaStateOutput
    假 kafka-consumer-groups.sh 对 --state 的回应:@{ <组名> = @{ Lines; ExitCode } };没写的组回一行 Empty。

.PARAMETER PostResetLag
    Step 5 -Apply 之后第 i 次 describe zone 组时分区 0 的 LAG(其余分区恒为 0);次数超出数组时沿用最后一个值。缺省 @(0)。

.PARAMETER LeoChanges
    两次 kafka-get-offsets 之间分区 0 的日志末端前进一条(模拟停服后仍有生产者在写)。

.PARAMETER SnapshotReply
    第 0 步 f 部署快照那次 kubectl 的回应 @{ ExitCode; Output };缺省 = New-RollbackDeploySnapshotReply 按沙箱 db.yaml 生成的一致快照。

.PARAMETER NamespaceReply
    Step 1 之后 namespace 检查的回应 @{ ExitCode; Output };缺省 = 已不存在(无输出、退出码 0)。

.PARAMETER EnvVars
    跑被测脚本期间设置的环境变量(跑完还原);缺省设置 -MySqlDsnEnv 指名的那一个。值为 $null 表示确保它未设置。

.PARAMETER SeedReportFiles
    跑之前写进报告目录的文件 @{ 文件名 = 内容 };需同时用 ExtraArgs 给定 RollbackId。

.PARAMETER RedisInfoReply
    第 0 步 f 读 go/db Redis run_id 那次 kubectl exec(redis-0)的回应 @{ ExitCode; Output };缺省 = INFO server 带 $script:RollbackSandboxRedisRunId。
    这次调用单独记账(RedisExecCalls,时间线 "kubectl:redis"),不进 KubectlCalls。

.PARAMETER ToolRedisRunId
    假工具在 -redis 上「看到」的 run_id;缺省 = $script:RollbackSandboxRedisRunId。argv 里的 -expect-run-id 与它不同时,假工具对任何模式都以 3 退出
    (模拟契约:每个模式先核对实例身份)。

.PARAMETER ToolVersionLines / ToolVersionExit
    假工具 -version 的输出行与退出码;缺省 = commit=$script:RollbackSandboxToolCommit、dirty=false、退出 0。-version 调用单独记账
    (VersionCalls,时间线 "tool:version"),不进 DiscardCalls / DiscardEnv。

.PARAMETER LoginConfigMapReply / LoginOAuthSecretReply
    第 0 步 a 的 login 第三方登录防误关读的两次 kubectl:login ConfigMap(-o json)与 Secret login-oauth(-o name)的回应 @{ ExitCode; Output }
    (或 @{ Throw = '…' } 模拟 kubectl 不可用)。缺省 = ConfigMap 在、没有 AuthProviders,Secret 不在:没开第三方登录,核对放行,
    已有用例不受这道检查影响。两次调用单独记账(LoginAuthCalls,时间线 "kubectl:login-auth"),不进 KubectlCalls。

.PARAMETER NamespaceReplyAfterInventory / KafkaStateOutputAfterInventory
    Step 4a 盘点之后(假工具记下 inventory 之后)的 namespace 检查回应与 --state 回应(@{ <组名> = @{ Lines; ExitCode } }),
    用来模拟 PITR 期间 namespace 被重建、zone 组又活跃(回档复核 [8]);缺省同 NamespaceReply / KafkaStateOutput。

.PARAMETER NodeOpenFileLimitMode
    传给被测脚本的 -NodeOpenFileLimitMode(B0-4,2026-10-22)。缺省 'enforce' = 显式给值,第 0 步 a 不核对 nofile 模式:
    这道核对在留空时要读集群(gate / scene 与 Agones Fleet),已有用例关心的是别的步骤,不该因它多读集群或被拒。
    测这道核对的用例传空串(留空,不传给被测脚本)。

.PARAMETER DeployOpenFileLimitDefault
    假 k8s_deploy.ps1 的 -NodeOpenFileLimitMode 默认值(被测脚本按语法树读它);缺省空串(同真脚本:不写 env,进程按 enforce)。
    'missing' = 假脚本不声明这个参数,被测脚本读不到默认值。-DeployDefault real 时用真脚本,本参数不起作用。

.PARAMETER AgonesDiscoveryReply / SceneFleetReply
    第 0 步 a nofile 核对读的两次 kubectl(lib Test-AgonesFleetCrdPresent 的 get --raw /apis/agones.dev/v1,与 Get-ClusterSceneFleets 的
    get fleets.agones.dev …)的回应 @{ ExitCode; Stdout; Stderr }(Stderr 以 ErrorRecord 写出,同真 kubectl 的 stderr),或 @{ Throw = '…' }。
    探测缺省 = 组未注册(NotFound,没装 Agones),于是不读 Fleet;Fleet 缺省 = 本用例不该读。两次调用单独记账(AgonesCalls,时间线
    "kubectl:agones" / "kubectl:fleet"),不进 KubectlCalls。
#>
function Invoke-RollbackInSandbox {
    param(
        [Parameter(Mandatory = $true)][ValidateSet('0', '1', 'real')][string]$DeployDefault,
        [ValidateSet('podip', 'external')][string]$DeployEntryDefault = 'podip',
        [hashtable]$KubectlReply = @{ Throw = '本用例不该读集群' },
        [AllowEmptyString()][string]$GateRouterMode = '',
        [hashtable]$ExtraArgs = @{},
        [ValidateSet('none', 'throw', 'exit')][string]$DryRunFailure = 'none',
        [string]$DryRunFailMessage = 'k8s_deploy.ps1 静态预检失败(假)',
        [hashtable]$KafkaCliOutput = @{},
        [int]$KafkaResetDryRunExit = 0,
        [int]$KafkaResetApplyExit = 0,
        [hashtable]$KafkaExecOutput = @{},
        [AllowEmptyString()][string]$DevToolsInfraNamespaceDefault = 'contract-infra',
        [string[]]$DbYamlContent = $null,
        [switch]$OmitDbYaml,
        [hashtable]$DiscardExit = @{},
        [hashtable]$KafkaStateOutput = @{},
        [int[]]$PostResetLag = @(0),
        [switch]$LeoChanges,
        [hashtable]$SnapshotReply = $null,
        [hashtable]$NamespaceReply = @{ ExitCode = 0; Output = '' },
        [hashtable]$EnvVars = $null,
        [hashtable]$SeedReportFiles = @{},
        [hashtable]$RedisInfoReply = $null,
        [string]$ToolRedisRunId = $script:RollbackSandboxRedisRunId,
        [string[]]$ToolVersionLines = @("commit=$($script:RollbackSandboxToolCommit)", 'dirty=false'),
        [int]$ToolVersionExit = 0,
        [hashtable]$NamespaceReplyAfterInventory = $null,
        [hashtable]$KafkaStateOutputAfterInventory = @{},
        [hashtable]$LoginConfigMapReply = $null,
        [hashtable]$LoginOAuthSecretReply = @{ ExitCode = 0; Output = '' },
        # 取值 '' / enforce / warn;不加 ValidateSet,空串由用例显式传入(被测脚本自己的 ValidateSet 才是要测的入口校验)。
        [AllowEmptyString()][string]$NodeOpenFileLimitMode = 'enforce',
        # 取值 '' / enforce / warn / missing。
        [AllowEmptyString()][string]$DeployOpenFileLimitDefault = '',
        [hashtable]$AgonesDiscoveryReply = @{ ExitCode = 1; Stderr = 'Error from server (NotFound): the server could not find the requested resource' },
        [hashtable]$SceneFleetReply = @{ Throw = '本用例不该读 scene Fleet' },
        [switch]$Apply
    )

    # 沙箱是一棵最小的仓库树:被测脚本按 <脚本目录>/../.. 找 go/db/etc/db.yaml 与 run/logs。
    $sandboxRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-rollback-preflight-' + [guid]::NewGuid().ToString('N'))
    $sandboxDir = Join-Path $sandboxRoot 'tools' 'scripts'
    New-Item -ItemType Directory -Path (Join-Path $sandboxDir 'lib') -Force | Out-Null
    if ($null -eq $EnvVars) { $EnvVars = @{ $script:RollbackSandboxDsnEnv = 'contract:dsn@tcp(mysql.contract:3306)/zone_101_db' } }
    $savedEnv = @{}
    try {
        $toolsDir = Get-ToolsScriptsDir
        Copy-Item -LiteralPath (Join-Path $toolsDir 'k8s_zone_rollback.ps1') -Destination $sandboxDir
        Copy-Item -LiteralPath (Join-Path $toolsDir 'lib' 'release_common.ps1') -Destination (Join-Path $sandboxDir 'lib')
        # 被测脚本在 $ErrorActionPreference = "Stop" 下 dot-source 它(取 Get-ClientEntryContract 与 kubectl 出口);不拷则启动即终止。
        Copy-Item -LiteralPath (Join-Path $toolsDir 'lib' 'k8s_client_entry.ps1') -Destination (Join-Path $sandboxDir 'lib')
        # 同上:describe / --state / get-offsets 的解析与切点核对都在它里面。
        Copy-Item -LiteralPath (Join-Path $toolsDir 'lib' 'kafka_consumer_groups.ps1') -Destination (Join-Path $sandboxDir 'lib')
        $dbYamlPath = Join-Path $sandboxRoot 'go' 'db' 'etc' 'db.yaml'
        if (-not $OmitDbYaml) {
            New-Item -ItemType Directory -Path (Split-Path -Parent $dbYamlPath) -Force | Out-Null
            if ($null -ne $DbYamlContent) {
                Set-Content -LiteralPath $dbYamlPath -Encoding utf8 -Value $DbYamlContent
            }
            else {
                Copy-Item -LiteralPath (Join-Path (Get-RepoRoot) 'go' 'db' 'etc' 'db.yaml') -Destination $dbYamlPath
            }
        }
        # 假下游要知道 zone 组名、topic 与分区数,取沙箱 db.yaml 的同一组键(被测脚本同样现读,测试不抄常量)。
        $groupBase = [string](Get-YamlScalar -Path $dbYamlPath -KeyPath 'ServerConfig.Kafka.GroupID').Value
        $generationText = [string](Get-YamlScalar -Path $dbYamlPath -KeyPath 'ServerConfig.Kafka.TopicGeneration').Value
        $partitionText = [string](Get-YamlScalar -Path $dbYamlPath -KeyPath 'ServerConfig.Kafka.PartitionCnt').Value
        $partitionCount = 0
        [void][int]::TryParse($partitionText, [ref]$partitionCount)
        $generation = 0
        [void][int]::TryParse($generationText, [ref]$generation)
        $sandboxTopic = Get-DbTaskTopicName -ZoneId 101 -Generation $generation
        $sandboxZoneGroup = if ($ExtraArgs.ContainsKey('KafkaGroup') -and $ExtraArgs.KafkaGroup) { [string]$ExtraArgs.KafkaGroup } else { "${groupBase}_z101" }
        if ($null -eq $SnapshotReply) {
            $SnapshotReply = New-RollbackDeploySnapshotReply -GroupBase $groupBase -TopicGeneration $generationText -PartitionCnt $partitionText
        }

        if ($DeployDefault -eq 'real') {
            Copy-Item -LiteralPath (Join-Path $toolsDir 'k8s_deploy.ps1') -Destination $sandboxDir
        }
        else {
            $paramLines = @(
                '    [ValidateSet("0", "1")]',
                ('    [string]$GateRouterMode = "{0}",' -f $DeployDefault),
                '    [ValidateSet("podip", "external")]',
                ('    [string]$ClientEntryMode = "{0}"' -f $DeployEntryDefault)
            )
            # -NodeOpenFileLimitMode 的默认值同真脚本写成字符串字面常量;'missing' 时不声明,模拟读不到默认值。
            if ($DeployOpenFileLimitDefault -cne 'missing') {
                $paramLines[-1] += ','
                $paramLines += @('    [ValidateSet("enforce", "warn", IgnoreCase = $false)]', ('    [string]$NodeOpenFileLimitMode = "{0}"' -f $DeployOpenFileLimitDefault))
            }
            Set-Content -LiteralPath (Join-Path $sandboxDir 'k8s_deploy.ps1') -Encoding utf8 -Value (
                @('param(') + $paramLines + @(')', 'throw "假 k8s_deploy.ps1 只供预检读默认值,不该被执行"'))
        }
        $timeline = Join-Path $sandboxRoot 'timeline.txt'
        $timelineLiteral = $timeline.Replace("'", "''")
        # 假 dev_tools.ps1 声明第 0 步静态预检 / Step 1 / Step 6 可能传的全部参数并关掉位置绑定:简单脚本里未声明的参数名会让
        # 它后面的值按位置落到别的参数上,记账就错了;以后被测脚本多传一个参数,这里直接报「找不到参数」而不是静默错位。
        $devToolsRecord = Join-Path $sandboxDir 'dev_tools.calls.txt'
        $devToolsBound = Join-Path $sandboxDir 'dev_tools.bound.txt'
        $fakeDevTools = @'
[CmdletBinding(PositionalBinding = $false)]
param($Command, $ZoneName, $ZoneId, $NodeImage, $NamespacePrefix, [switch]$WaitReady, $GateRouterMode = "<unset>", $ReleaseProfile,
    $ClientEntryMode, $ClientPublicHost, $GateClientHostTemplate, $GateNodePortBase, $GateExternalTrafficPolicy, $GateServiceType,
    $RequireClientEndpoint, [switch]$LoginDevPasswordAuth, [switch]$AllowDisruptiveSwitch, $GatewayIngressHost, $GatewayIngressClassName,
    $GatewayIngressTlsSecret, $GatewayTrustedProxies, $KubeContext, $KubeConfig, $DbConsumerGroup,
    $LoginWeChatAppId, $LoginWeChatEndpoint, [switch]$LoginWeChatAllowOpenId, $LoginQQAppId, $LoginQQEndpoint, [switch]$LoginQQAllowOpenId,
    [switch]$ConfirmLoginAuthProviderChange, $NodeOpenFileLimitMode, __INFRA_PARAM__[switch]$DryRun)
$label = if ($DryRun) { "$Command/dry-run" } else { $Command }
Add-Content -LiteralPath '__CALLS__' -Encoding utf8 -Value "$label GateRouterMode=$GateRouterMode"
Add-Content -LiteralPath '__TIMELINE__' -Encoding utf8 -Value "dev_tools:$label"
$boundParameters = $PSBoundParameters
$bound = @($boundParameters.Keys | Where-Object { $_ -notin @('Command', 'DryRun') } | Sort-Object | ForEach-Object { "$_=$($boundParameters[$_])" }) -join ';'
Add-Content -LiteralPath '__BOUND__' -Encoding utf8 -Value "$label $bound"
if ($DryRun) { __DRYRUN_FAIL__ }
exit 0
'@
        $failMessage = $DryRunFailMessage.Replace("'", "''")
        $dryRunFailCode = switch ($DryRunFailure) {
            'throw' { "throw '$failMessage'" }
            'exit' { "Write-Host '$failMessage'; exit 3" }
            default { '' }
        }
        # 第 0 步 e 按语法树读 dev_tools.ps1 的 -InfraNamespace 默认值(Step 6 不传它,就用这个默认值);被调时不传它,不影响记账。
        $infraParam = if ([string]::IsNullOrEmpty($DevToolsInfraNamespaceDefault)) { '$InfraNamespace, ' } else { ('$InfraNamespace = "{0}", ' -f $DevToolsInfraNamespaceDefault) }
        $fakeDevTools = $fakeDevTools.Replace('__CALLS__', $devToolsRecord.Replace("'", "''")).Replace('__BOUND__', $devToolsBound.Replace("'", "''")).Replace('__TIMELINE__', $timelineLiteral).Replace('__DRYRUN_FAIL__', $dryRunFailCode).Replace('__INFRA_PARAM__', $infraParam)
        Set-Content -LiteralPath (Join-Path $sandboxDir 'dev_tools.ps1') -Encoding utf8 -Value $fakeDevTools
        # 假 kafka_offset_reset.ps1 同样声明全部参数并关掉位置绑定:数组 splatting 之类的错位会直接报错,而不是静默记错账。
        # -Apply 成功时写一个标记文件:之后假 kafka-consumer-groups.sh 对 zone 组回「已拨到末端」的表(Step 5 切点与 5b)。
        $kafkaResetBound = Join-Path $sandboxDir 'kafka_offset_reset.bound.txt'
        $resetMarker = Join-Path $sandboxRoot 'reset.applied'
        $fakeKafkaReset = @'
[CmdletBinding(PositionalBinding = $false)]
param($BootstrapServer, $Group, $RetireGroup, $Topic, $ToDatetime, [switch]$ToEarliest, [switch]$ToLatest, [switch]$DeleteAndRecreateTopic,
    [switch]$FinalizeHandoff, $Partitions, $ReplicationFactor, $KafkaBin, [switch]$Apply)
$boundParameters = $PSBoundParameters
$bound = @($boundParameters.Keys | Sort-Object | ForEach-Object { "$_=$($boundParameters[$_])" }) -join ';'
Add-Content -LiteralPath '__RESET__' -Encoding utf8 -Value $bound
Add-Content -LiteralPath '__TIMELINE__' -Encoding utf8 -Value $(if ($Apply) { 'reset:apply' } else { 'reset:dry-run' })
if (-not $Apply -and __DRYRUN_EXIT__ -ne 0) { Write-Host 'fake kafka_offset_reset: incomplete reset plan'; exit __DRYRUN_EXIT__ }
if ($Apply -and __APPLY_EXIT__ -ne 0) { Write-Host 'fake kafka_offset_reset: verification failed'; exit __APPLY_EXIT__ }
if ($Apply) { Set-Content -LiteralPath '__MARKER__' -Value 'applied' }
exit 0
'@
        $fakeKafkaReset = $fakeKafkaReset.Replace('__RESET__', $kafkaResetBound.Replace("'", "''")).Replace('__TIMELINE__', $timelineLiteral).Replace('__MARKER__', $resetMarker.Replace("'", "''")).Replace('__DRYRUN_EXIT__', [string]$KafkaResetDryRunExit).Replace('__APPLY_EXIT__', [string]$KafkaResetApplyExit)
        Set-Content -LiteralPath (Join-Path $sandboxDir 'kafka_offset_reset.ps1') -Encoding utf8 -Value $fakeKafkaReset
        # 假回档工具:不声明参数块,argv 原样落进 $args;记下 "<模式> <argv>",以及 argv 指名的两个环境变量在工具进程里有没有值(只记有无,不记值)。
        $discardCalls = Join-Path $sandboxRoot 'discard.calls.txt'
        $discardEnv = Join-Path $sandboxRoot 'discard.env.txt'
        $exitEntries = @($DiscardExit.Keys | ForEach-Object { "'$_' = $([int]$DiscardExit[$_])" }) -join '; '
        # 只带 -version 时按 -ToolVersionLines / -ToolVersionExit 回应构建身份(单独记账,不进 DiscardCalls);其余调用若带 -expect-run-id
        # 且与 -ToolRedisRunId 不同,任何模式都以 3 退出(契约:每个模式先核对 -redis 的 run_id)。
        $discardVersions = Join-Path $sandboxRoot 'discard.version.txt'
        $fakeDiscard = @'
$argList = @($args | ForEach-Object { "$_" })
if ($argList.Count -eq 1 -and $argList[0] -ceq '-version') {
    Add-Content -LiteralPath '__VERSIONS__' -Encoding utf8 -Value '-version'
    Add-Content -LiteralPath '__TIMELINE__' -Encoding utf8 -Value 'tool:version'
    @(__VERSION_LINES__) | ForEach-Object { Write-Output $_ }
    exit __VERSION_EXIT__
}
$modeIndex = [array]::IndexOf($argList, '-mode')
$mode = if ($modeIndex -ge 0 -and $modeIndex + 1 -lt $argList.Count) { $argList[$modeIndex + 1] } else { '?' }
Add-Content -LiteralPath '__CALLS__' -Encoding utf8 -Value ("$mode " + ($argList -join ' '))
Add-Content -LiteralPath '__TIMELINE__' -Encoding utf8 -Value "tool:$mode"
foreach ($flag in @('-redis-password-env', '-mysql-dsn-env')) {
    $at = [array]::IndexOf($argList, $flag)
    if ($at -ge 0 -and $at + 1 -lt $argList.Count) {
        $name = $argList[$at + 1]
        $state = if ([string]::IsNullOrEmpty([System.Environment]::GetEnvironmentVariable($name))) { 'unset' } else { 'set' }
        Add-Content -LiteralPath '__ENV__' -Encoding utf8 -Value "$mode $name=$state"
    }
}
$runIdAt = [array]::IndexOf($argList, '-expect-run-id')
if ($runIdAt -ge 0 -and ($runIdAt + 1 -ge $argList.Count -or $argList[$runIdAt + 1] -cne '__TOOL_RUN_ID__')) {
    Write-Host "fake zone_rollback_discard: mode=$mode run_id mismatch (-redis reports __TOOL_RUN_ID__) exit=3"
    exit 3
}
$codes = @{ __CODES__ }
$code = if ($codes.ContainsKey($mode)) { $codes[$mode] } else { 0 }
Write-Host "fake zone_rollback_discard: mode=$mode exit=$code"
exit $code
'@
        $versionLinesLiteral = (@($ToolVersionLines | ForEach-Object { "'" + ([string]$_).Replace("'", "''") + "'" }) -join ', ')
        $fakeDiscard = $fakeDiscard.Replace('__CALLS__', $discardCalls.Replace("'", "''")).Replace('__ENV__', $discardEnv.Replace("'", "''")).Replace('__TIMELINE__', $timelineLiteral).Replace('__CODES__', $exitEntries)
        $fakeDiscard = $fakeDiscard.Replace('__VERSIONS__', $discardVersions.Replace("'", "''")).Replace('__VERSION_LINES__', $versionLinesLiteral).Replace('__VERSION_EXIT__', [string]$ToolVersionExit).Replace('__TOOL_RUN_ID__', $ToolRedisRunId.Replace("'", "''"))
        $discardToolPath = Join-Path $sandboxDir 'zone_rollback_discard.ps1'
        Set-Content -LiteralPath $discardToolPath -Encoding utf8 -Value $fakeDiscard

        $fakeKubectlCalls = [System.Collections.Generic.List[string]]::new()
        $fakeKafkaExecCalls = [System.Collections.Generic.List[string]]::new()
        $fakeSnapshotCalls = [System.Collections.Generic.List[string]]::new()
        $fakeNamespaceCalls = [System.Collections.Generic.List[string]]::new()
        $fakeRedisExecCalls = [System.Collections.Generic.List[string]]::new()
        $fakeLoginAuthCalls = [System.Collections.Generic.List[string]]::new()
        $fakeAgonesCalls = [System.Collections.Generic.List[string]]::new()
        if ($null -eq $LoginConfigMapReply) { $LoginConfigMapReply = New-RollbackLoginConfigMapReply }
        if ($null -eq $RedisInfoReply) {
            $RedisInfoReply = @{ ExitCode = 0; Output = @('# Server', 'redis_version:7.2.4', "run_id:$($script:RollbackSandboxRedisRunId)", 'tcp_port:6379') }
        }
        # Step 4a 盘点之后(假工具已记下 inventory 调用)才生效的回应:模拟 PITR 期间写者回来(回档复核 [8])。
        $afterInventory = {
            (Test-Path -LiteralPath $discardCalls) -and (@(Get-Content -LiteralPath $discardCalls -Encoding utf8 | Where-Object { $_ -like 'inventory *' }).Count -gt 0)
        }
        function kubectl {
            $kubectlArgList = @($args | ForEach-Object { "$_" })
            # 第 0 步 f:exec 进 infra 的 redis-0 读 go/db Redis 的 INFO server(run_id)。单独记账,先于 kafka-0 的 exec 判断。
            if ($kubectlArgList -ccontains 'pod/redis-0') {
                $fakeRedisExecCalls.Add(($kubectlArgList -join ' '))
                Add-Content -LiteralPath $timeline -Encoding utf8 -Value 'kubectl:redis'
                $global:LASTEXITCODE = $RedisInfoReply.ExitCode
                return $RedisInfoReply.Output
            }
            # 第 0 步 e:exec 进 kafka-0 跑 describe。单独记账;缺省回与假 kafka-consumer-groups.sh 同一份事实(同一个 Kafka)。
            if ($kubectlArgList -ccontains 'exec') {
                $fakeKafkaExecCalls.Add(($kubectlArgList -join ' '))
                Add-Content -LiteralPath $timeline -Encoding utf8 -Value 'kubectl:exec'
                $execReply = if ($KafkaExecOutput.ContainsKey('Lines')) { $KafkaExecOutput } elseif ($KafkaCliOutput.ContainsKey('Lines')) { $KafkaCliOutput } else { $null }
                if ($null -ne $execReply) {
                    $global:LASTEXITCODE = if ($execReply.ContainsKey('ExitCode')) { $execReply.ExitCode } else { 0 }
                    return $execReply.Lines
                }
                $execGroup = [regex]::Match($kubectlArgList[-1], '--group (\S+)\s*$').Groups[1].Value
                $global:LASTEXITCODE = 0
                return "Error: Consumer group '$execGroup' does not exist."
            }
            # 第 0 步 f:部署快照(go-svc ConfigMap 与 Deployment)。
            if ($kubectlArgList -ccontains 'configmap/go-svc-db-config') {
                $fakeSnapshotCalls.Add(($kubectlArgList -join ' '))
                Add-Content -LiteralPath $timeline -Encoding utf8 -Value 'kubectl:snapshot'
                $global:LASTEXITCODE = $SnapshotReply.ExitCode
                return $SnapshotReply.Output
            }
            # 第 0 步 a:login 第三方登录防误关读 login ConfigMap 与 Secret login-oauth 是否存在(lib Get-LoginAuthProviderLiveState)。
            # 单独记账,不进 KubectlCalls(入口预检那一次 kubectl 的计数断言不受影响)。
            if ($kubectlArgList -ccontains 'go-svc-login-config' -or $kubectlArgList -ccontains 'login-oauth') {
                $fakeLoginAuthCalls.Add(($kubectlArgList -join ' '))
                Add-Content -LiteralPath $timeline -Encoding utf8 -Value 'kubectl:login-auth'
                $loginAuthReply = if ($kubectlArgList -ccontains 'go-svc-login-config') { $LoginConfigMapReply } else { $LoginOAuthSecretReply }
                if ($loginAuthReply.ContainsKey('Throw')) {
                    throw [System.Management.Automation.CommandNotFoundException]::new($loginAuthReply.Throw)
                }
                $global:LASTEXITCODE = $loginAuthReply.ExitCode
                return $loginAuthReply.Output
            }
            # 第 0 步 a:nofile 核对的 Agones 探测(lib Test-AgonesFleetCrdPresent)与 scene Fleet 读取(Get-ClusterSceneFleets),
            # 都经 lib Invoke-ClientEntryKubectl(--context / --kubeconfig 在前)。单独记账,不进 KubectlCalls。
            $isAgonesProbe = $kubectlArgList -ccontains '--raw'
            if ($isAgonesProbe -or $kubectlArgList -ccontains 'fleets.agones.dev') {
                $fakeAgonesCalls.Add(($kubectlArgList -join ' '))
                Add-Content -LiteralPath $timeline -Encoding utf8 -Value $(if ($isAgonesProbe) { 'kubectl:agones' } else { 'kubectl:fleet' })
                $agonesReply = if ($isAgonesProbe) { $AgonesDiscoveryReply } else { $SceneFleetReply }
                if ($agonesReply.ContainsKey('Throw')) {
                    throw [System.Management.Automation.CommandNotFoundException]::new($agonesReply.Throw)
                }
                # 同 k8s_client_entry_contract.tests.ps1 的 CRD 探测替身:stderr 以 ErrorRecord 写出,Invoke-ClientEntryKubectl 按类型分开收。
                if ($agonesReply.Stderr) { Write-Error -Message $agonesReply.Stderr -ErrorAction Continue }
                $global:LASTEXITCODE = $agonesReply.ExitCode
                if ($agonesReply.Stdout) { return $agonesReply.Stdout }
                return
            }
            # Step 1 之后与 Step 5 之前:namespace 是否已不存在。
            if ($kubectlArgList -ccontains 'namespace') {
                $fakeNamespaceCalls.Add(($kubectlArgList -join ' '))
                Add-Content -LiteralPath $timeline -Encoding utf8 -Value 'kubectl:namespace'
                $namespaceAnswer = if ($null -ne $NamespaceReplyAfterInventory -and (& $afterInventory)) { $NamespaceReplyAfterInventory } else { $NamespaceReply }
                $global:LASTEXITCODE = $namespaceAnswer.ExitCode
                return $namespaceAnswer.Output
            }
            $fakeKubectlCalls.Add(($args -join ' '))
            Add-Content -LiteralPath $timeline -Encoding utf8 -Value 'kubectl:entry'
            if ($KubectlReply.ContainsKey('Throw')) {
                throw [System.Management.Automation.CommandNotFoundException]::new($KubectlReply.Throw)
            }
            $global:LASTEXITCODE = $KubectlReply.ExitCode
            return $KubectlReply.Output
        }
        $fakeKafkaCliCalls = [System.Collections.Generic.List[string]]::new()
        $postResetDescribes = [System.Collections.Generic.List[int]]::new()
        Set-Item -Path 'Function:kafka-consumer-groups.sh' -Value {
            $argList = @($args | ForEach-Object { "$_" })
            $fakeKafkaCliCalls.Add(($argList -join ' '))
            $groupIndex = [array]::IndexOf($argList, '--group')
            $groupName = if ($groupIndex -ge 0 -and $groupIndex + 1 -lt $argList.Count) { $argList[$groupIndex + 1] } else { '?' }
            $isState = $argList -ccontains '--state'
            Add-Content -LiteralPath $timeline -Encoding utf8 -Value ("kafka-cg:$groupName" + $(if ($isState) { ':state' } else { '' }))
            if ($KafkaCliOutput.ContainsKey('Throw')) {
                throw [System.Management.Automation.CommandNotFoundException]::new($KafkaCliOutput.Throw)
            }
            if ($isState) {
                if ($KafkaStateOutputAfterInventory.ContainsKey($groupName) -and (& $afterInventory)) {
                    $laterReply = $KafkaStateOutputAfterInventory[$groupName]
                    $global:LASTEXITCODE = if ($laterReply.ContainsKey('ExitCode')) { $laterReply.ExitCode } else { 0 }
                    return $laterReply.Lines
                }
                if ($KafkaStateOutput.ContainsKey($groupName)) {
                    $stateReply = $KafkaStateOutput[$groupName]
                    $global:LASTEXITCODE = if ($stateReply.ContainsKey('ExitCode')) { $stateReply.ExitCode } else { 0 }
                    return $stateReply.Lines
                }
                $global:LASTEXITCODE = 0
                return @('GROUP                       COORDINATOR (ID)          ASSIGNMENT-STRATEGY  STATE           #MEMBERS',
                    "$groupName  kafka-0.kafka:9092 (0)  Empty  0")
            }
            # Step 5 -Apply 之后对 zone 组:每个分区都已拨到日志末端;-PostResetLag 让第 i 次的分区 0 落后若干条。
            if ((Test-Path -LiteralPath $resetMarker) -and $groupName -ceq $sandboxZoneGroup) {
                $callIndex = $postResetDescribes.Count
                $postResetDescribes.Add($callIndex)
                $lag = if ($PostResetLag.Count -eq 0) { 0 } elseif ($callIndex -lt $PostResetLag.Count) { $PostResetLag[$callIndex] } else { $PostResetLag[-1] }
                $rows = @('GROUP  TOPIC  PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG  CONSUMER-ID  HOST  CLIENT-ID')
                for ($p = 0; $p -lt $partitionCount; $p++) {
                    $current = 1000 + $p
                    $partitionLag = if ($p -eq 0) { $lag } else { 0 }
                    $rows += "$groupName  $sandboxTopic  $p  $current  $($current + $partitionLag)  $partitionLag  -  -  -"
                }
                $global:LASTEXITCODE = 0
                return $rows
            }
            if ($KafkaCliOutput.ContainsKey('Lines')) {
                $global:LASTEXITCODE = if ($KafkaCliOutput.ContainsKey('ExitCode')) { $KafkaCliOutput.ExitCode } else { 0 }
                return $KafkaCliOutput.Lines
            }
            $global:LASTEXITCODE = 0
            return "Error: Consumer group '$groupName' does not exist."
        }
        $fakeLeoCalls = [System.Collections.Generic.List[string]]::new()
        Set-Item -Path 'Function:kafka-get-offsets.sh' -Value {
            $argList = @($args | ForEach-Object { "$_" })
            $fakeLeoCalls.Add(($argList -join ' '))
            Add-Content -LiteralPath $timeline -Encoding utf8 -Value 'get-offsets'
            $topicIndex = [array]::IndexOf($argList, '--topic')
            $topicName = if ($topicIndex -ge 0 -and $topicIndex + 1 -lt $argList.Count) { $argList[$topicIndex + 1] } else { '?' }
            $bump = if ($LeoChanges -and $fakeLeoCalls.Count -ge 2) { 1 } else { 0 }
            $global:LASTEXITCODE = 0
            return @(for ($p = 0; $p -lt $partitionCount; $p++) { "${topicName}:${p}:$(1000 + $p + $(if ($p -eq 0) { $bump } else { 0 }))" })
        }
        $fakeSleepCalls = [System.Collections.Generic.List[string]]::new()
        function Start-Sleep {
            $fakeSleepCalls.Add((@($args | ForEach-Object { "$_" }) -join ' '))
            Add-Content -LiteralPath $timeline -Encoding utf8 -Value 'sleep'
        }
        # 回档不再直接碰 Redis(写 Redis 只经回档工具):被测脚本一旦调了 redis-cli,这里记账并抛出,绝不落到本机真的 redis-cli 上。
        $fakeRedisCliCalls = [System.Collections.Generic.List[string]]::new()
        function redis-cli {
            $fakeRedisCliCalls.Add((@($args | ForEach-Object { "$_" }) -join ' '))
            Add-Content -LiteralPath $timeline -Encoding utf8 -Value 'redis-cli'
            throw '回档脚本不该直接调用 redis-cli(假)'
        }

        $rollbackArgs = @{
            ZoneName            = 'contract-test'
            ZoneId              = 101
            TargetTime          = $script:RollbackSandboxTargetTime
            NodeImage           = 'ghcr.io/luyuancpp/mmorpg-node:0123456789ab'
            SkipMySqlPause      = $true
            PitrRestoredTo      = $script:RollbackSandboxTargetTime
            RedisHost           = '127.0.0.1'
            MySqlDsnEnv         = $script:RollbackSandboxDsnEnv
            RollbackDiscardTool = $discardToolPath
            BarrierProtocol     = 'legacy'
        }
        if ($GateRouterMode -ne '') { $rollbackArgs.GateRouterMode = $GateRouterMode }
        if ($NodeOpenFileLimitMode -ne '') { $rollbackArgs.NodeOpenFileLimitMode = $NodeOpenFileLimitMode }
        foreach ($key in $ExtraArgs.Keys) {
            if ($null -eq $ExtraArgs[$key]) { $rollbackArgs.Remove($key) } else { $rollbackArgs[$key] = $ExtraArgs[$key] }
        }
        if ($Apply) { $rollbackArgs.Apply = $true }
        if ($rollbackArgs.ContainsKey('RollbackId') -and $SeedReportFiles.Count -gt 0) {
            $seedDir = Join-Path $sandboxRoot 'run' 'logs' 'zone-rollback' ([string]$rollbackArgs.RollbackId)
            New-Item -ItemType Directory -Path $seedDir -Force | Out-Null
            foreach ($name in $SeedReportFiles.Keys) { Set-Content -LiteralPath (Join-Path $seedDir $name) -Encoding utf8 -Value $SeedReportFiles[$name] }
        }
        foreach ($name in $EnvVars.Keys) {
            $savedEnv[$name] = [System.Environment]::GetEnvironmentVariable($name)
            [System.Environment]::SetEnvironmentVariable($name, $EnvVars[$name])
        }

        $captured = [System.Text.StringBuilder]::new()
        $errorText = $null
        try {
            & (Join-Path $sandboxDir 'k8s_zone_rollback.ps1') @rollbackArgs *>&1 | ForEach-Object { [void]$captured.AppendLine("$_") }
        }
        catch {
            $errorText = $_.Exception.Message
        }
        $reportFiles = @{}
        $reportRoot = Join-Path $sandboxRoot 'run' 'logs' 'zone-rollback'
        if (Test-Path -LiteralPath $reportRoot) {
            foreach ($file in @(Get-ChildItem -LiteralPath $reportRoot -Recurse -File)) {
                $reportFiles[$file.Name] = Get-Content -LiteralPath $file.FullName -Raw -Encoding utf8
            }
        }
        return [pscustomobject]@{
            Error              = $errorText
            Output             = $captured.ToString()
            KubectlCalls       = @($fakeKubectlCalls)
            DevToolsCalls      = @(if (Test-Path -LiteralPath $devToolsRecord) { Get-Content -LiteralPath $devToolsRecord -Encoding utf8 })
            DevToolsBound      = @(if (Test-Path -LiteralPath $devToolsBound) { Get-Content -LiteralPath $devToolsBound -Encoding utf8 })
            KafkaCliCalls      = @($fakeKafkaCliCalls)
            KafkaExecCalls     = @($fakeKafkaExecCalls)
            KafkaResetBound    = @(if (Test-Path -LiteralPath $kafkaResetBound) { Get-Content -LiteralPath $kafkaResetBound -Encoding utf8 })
            DiscardCalls       = @(if (Test-Path -LiteralPath $discardCalls) { Get-Content -LiteralPath $discardCalls -Encoding utf8 })
            DiscardEnv         = @(if (Test-Path -LiteralPath $discardEnv) { Get-Content -LiteralPath $discardEnv -Encoding utf8 })
            SnapshotCalls      = @($fakeSnapshotCalls)
            NamespaceCalls     = @($fakeNamespaceCalls)
            RedisExecCalls     = @($fakeRedisExecCalls)
            LoginAuthCalls     = @($fakeLoginAuthCalls)
            AgonesCalls        = @($fakeAgonesCalls)
            VersionCalls       = @(if (Test-Path -LiteralPath $discardVersions) { Get-Content -LiteralPath $discardVersions -Encoding utf8 })
            LeoCalls           = @($fakeLeoCalls)
            SleepCalls         = @($fakeSleepCalls)
            RedisCliCalls      = @($fakeRedisCliCalls)
            PostResetDescribes = $postResetDescribes.Count
            Timeline           = @(if (Test-Path -LiteralPath $timeline) { Get-Content -LiteralPath $timeline -Encoding utf8 })
            ReportFiles        = $reportFiles
            DiscardToolPath    = $discardToolPath
            Topic              = $sandboxTopic
            ZoneGroup          = $sandboxZoneGroup
        }
    }
    finally {
        foreach ($name in $savedEnv.Keys) { [System.Environment]::SetEnvironmentVariable($name, $savedEnv[$name]) }
        Remove-Item -LiteralPath $sandboxRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

<#
.SYNOPSIS
    时间线里各事件第一次出现的位置;没有出现记为 -1。用于断言「A 早于 B」。
#>
function Get-TimelineIndex {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][string[]]$Timeline,
        [Parameter(Mandatory = $true)][string]$Event
    )

    return [array]::IndexOf($Timeline, $Event)
}
