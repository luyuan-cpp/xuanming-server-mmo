#requires -Version 7
<#
.SYNOPSIS
    排空一台 gate(external 形态的 gate StatefulSet),可选在排空完成后删 Pod 让 STS 重建。

.DESCRIPTION
    集群外入口 D87:gate 是 StatefulSet(updateStrategy OnDelete、PDB maxUnavailable 0),
    滚动一律走本脚本 —— 标 gate:<id>:draining → 等 gate:<id>:drained → 删 Pod(-DeletePod)。

    本脚本不做缩容。排空后直接 scale StatefulSet,两个标记会留到 TTL 到期;node_id 全局唯一、按最小
    空闲号复用,login 的 Redis 跨 zone 共用,期间之后拿到同一 node_id 的 gate(可能在任意 zone)分不到
    玩家。缩容后的清标记步骤见运维手册(docs/design/k8s-client-entry.md)。

    流程:
      1. 读 zone namespace 里的 login ConfigMap(go-svc-login-config / login.yaml),取 zone_id、
         login 连的 Redis(Node.RedisClient)与 etcd(Registry.Etcd.Hosts,缺省 Etcd.Hosts)、
         RequireClientEndpoint,以及 GateDrain 判定参数(不写该块 = login 结构体默认值 Interval 5s /
         Deadline 25m)。「Redis 与 etcd 在哪」只认这份 ConfigMap,不在本脚本另写一份地址。
      2. 入口校验(任何写操作之前):-DrainTtlSeconds 与 -WaitTimeoutSeconds 都必须大于 login 的
         GateDrain.Deadline,并留出足够的 drained 观察窗口(见 Get-RequiredDrainedWindowSeconds)。
      3. 取 Pod gate-<Ordinal> 的 podIP;在 etcd 的 gate 前缀下找 endpoint.ip == podIP 的 NodeInfo,
         得到 node_id;0 条或多条都报错(多条 = 同 IP 上还挂着旧进程的陈旧记录,等租约过期再来)。
         再按 login 下发候选集的口径确认本 zone 还有别的 gate(Select-GateDrainPeers):login 在候选
         全部 draining 时会忽略标记照常分配,单副本 zone 标了也挡不住新玩家。
      4. 取 Redis 服务器时间(TIME),一次原子 EVAL:同 zone 其它候选全部在排空 → 什么都不写并拒绝;
         否则 DEL 残留的 drained,再 SET gate:<id>:draining <整数 Unix 秒> NX EX <ttl>。已有标记沿用
         (不重置期限),但必须带 TTL,值必须是 [Redis TIME - MaxDrainTtlSeconds, Redis TIME] 内的整数 Unix 秒
         (本脚本写的标记 TTL 不超过 -DrainTtlSeconds 的上限 MaxDrainTtlSeconds;更早的值不是本脚本写的,
         login 会立刻按 deadline 放行),否则拒绝。
         写完立即复核 Pod UID 与 etcd 里的 node_id/uuid,变了就撤回本次写入的标记并中止。
      5. 有界轮询 gate:<id>:drained(由 login 的排空判定循环写,值 below_threshold | deadline,
         TTL 跟随 draining 剩余 TTL)。draining 中途消失 = gate 已重新接客,中止且不删 Pod;
         draining 的值被改写(不再等于第 4 步确认的值)= login 的判定不再对应本次排空,同样中止且不删 Pod。
         每隔几轮复核被排空进程的 etcd 记录还在不在:进程没了(Pod 被替换 / 重启换了 node_id)就撤回
         本次写入的标记(值比对后再删,原子)并中止 —— 不撤回会挡住之后复用该 node_id 的 gate。
         reason=deadline 时打醒目警告:这台 gate 上还有玩家,删 Pod 会让他们断线重连。
      6. 只有带 -DeletePod 才删 Pod:删前复核 Pod UID 与 etcd 里的进程身份没变;删后等旧进程的
         etcd 记录消失再清掉两个标记(新 gate 常会复用同一 node_id,不清就在 TTL 内分不到玩家),
         最后等新 Pod 与 STS 就绪。

    -DryRun 只做只读定位(ConfigMap / Pod / etcd),把将要执行的写操作打印出来,不访问 Redis、不删 Pod。

    退出码:0 = 完成(含 -DryRun,以及未带 -DeletePod 到 drained 为止);1 = 出错中止(输出里说明标记现状);
    3 = 结束时留下了需要人工清理的排空标记(输出里有 DEL 指引;删 Pod 后新 Pod 是否就绪也在输出里)。

    依赖:etcdctl 在 kubectl exec 进 login 所连 etcd Service 背后的 Pod 后,按 login 配置里的 Service 地址连回同一个
    Service(hairpin);redis-cli 在 kubectl exec 进与 Redis Service 同名的 StatefulSet(statefulset/redis 的 redis 容器)后,
    同样按 Service 地址连 —— 单实例形态下 Service 背后就是这个 Pod(hairpin),高可用形态(redis-ha.yaml)下 Service 指向
    redis-proxy、经代理落到主,不涉及 hairpin。读写失败的方向都是 fail-closed(读不到就不写、不删)。

    所有 kubectl 调用都有 HTTP 超时与进程级截止;轮询共用单调时钟的总预算。
    kind 上务必显式传 -KubeContext kind-mmorpg(本机默认 current-context 是别的集群)。

.EXAMPLE
    # 排空 zone e2e 的 gate-1,排空完成后删 Pod 并等重建
    pwsh -File tools/scripts/k8s_gate_drain.ps1 -ZoneName e2e -Ordinal 1 -DeletePod -KubeContext kind-mmorpg

.EXAMPLE
    # 只看会发生什么(只读定位 + 打印计划)
    pwsh -File tools/scripts/k8s_gate_drain.ps1 -ZoneName e2e -Ordinal 1 -DryRun -KubeContext kind-mmorpg
#>
param(
    # zone 名,与 k8s_deploy.ps1 -ZoneName 同义:namespace = <NamespacePrefix>-<ZoneName>。
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')]
    [string]$ZoneName,

    # gate StatefulSet 的序号:目标 Pod = gate-<Ordinal>。
    [Parameter(Mandatory = $true)]
    [ValidateRange(0, 9999)]
    [int]$Ordinal,

    # gate:<id>:draining 的 TTL(秒)。到期标记自动消失、gate 重新接客 —— 脚本中途挂了也不会让容量永久蒸发。
    # 必须大于 login 的 GateDrain.Deadline(默认 25m = 1500s)并留出观察窗口:期限不早于 TTL 时
    # draining 先过期,drained 永远等不到。入口校验。
    # 上限 86400 与 Get-GateDrainContract.MaxDrainTtlSeconds 必须相等(属性参数只能写常量,由测试钉住)。
    [ValidateRange(1, 86400)]
    [int]$DrainTtlSeconds = 1800,

    # 等 drained 的总预算(秒)。同样必须大于 GateDrain.Deadline 并留出观察窗口,否则脚本会在
    # login 按期限放行之前先超时。入口校验。
    [ValidateRange(1, 86400)]
    [int]$WaitTimeoutSeconds = 1800,

    # -DeletePod 之后等新 Pod 与 STS 就绪的预算(秒)。
    [ValidateRange(30, 3600)]
    [int]$ReadyTimeoutSeconds = 300,

    [switch]$DeletePod,
    [switch]$DryRun,

    # 与 k8s_deploy.ps1 -NamespacePrefix 同义。
    [string]$NamespacePrefix = "mmorpg-zone",

    # 与 k8s_deploy.ps1 的 Build-KubectlBaseArgs 同语义:非空才带 --context / --kubeconfig。
    [string]$KubeContext = "",
    [string]$KubeConfig = ""
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
# ConvertFrom-YamlToFlatMap:解析 login ConfigMap 里的 login.yaml。
. (Join-Path $ScriptDir "lib\release_common.ps1")

# 本脚本与 login / C++ 节点 / 部署生成器之间的全部字面量契约,集中在一处;
# tools/scripts/tests/k8s_gate_drain.tests.ps1 逐项钉住它们与源头一致。
function Get-GateDrainContract {
    return [pscustomobject]@{
        # etcd 发现前缀与 login GateWatcher 相同:node.BuildRpcPrefix(GetRpcPrefix(GateNodeService), zone, 4)
        # = GateNodeService.rpc/zone/<zone>/node_type/4/;C++ 的键在其下 node_id/<id>(EtcdManager::MakeNodeEtcdKey)。
        # 4 = proto/common/base/node.proto 的 eNodeType.GateNodeService。
        GateServiceName               = 'GateNodeService'
        GateNodeType                  = 4
        # k8s_deploy.ps1 生成的 gate StatefulSet 名(Pod 名 gate-<序号>)。
        GateStatefulSet               = 'gate'
        # k8s_deploy.ps1 $GoSvcCatalogue.login 的 ConfigMap 名与数据键(ConfigFile)。
        LoginConfigMap                = 'go-svc-login-config'
        LoginConfigKey                = 'login.yaml'
        # go/login/internal/logic/pkg/loginqueue:GateDrainingKeyFmt / GateDrainedKeyFmt 与两个判定理由。
        DrainingKeyFormat             = 'gate:{0}:draining'
        DrainedKeyFormat              = 'gate:{0}:drained'
        DrainedReasonBelowThreshold   = 'below_threshold'
        DrainedReasonDeadline         = 'deadline'
        # go/login/internal/config/config.go GateDrainConf 的 default 标签。k8s_deploy.ps1 生成的
        # login ConfigMap 不写 GateDrain 块,集群上生效的就是这两个值。
        DefaultMonitorIntervalSeconds = 5
        DefaultDeadlineSeconds        = 1500
        # 同一文件 GateDrainMigrateConf 的 default 标签(排空迁移,docs/design/gate-drain-session-migration.md)。
        # 本脚本只据此打印与估算迁移时长,不改变排空流程与预算校验。
        DefaultMigrateEnabled           = $true
        DefaultMigrateRatePerSecond     = 20
        DefaultMigrateStartAfterSeconds = 0
        # go/shared/gatepick/eligibility.go 的 FullPercent:gate 人数到自报连接上限的这个百分比就算到水位,
        # login 不再往它身上分人或迁人。本脚本只用它估算其余 gate 还能接多少人(Get-GateDrainPeerHeadroom),
        # 测试钉住与源头一致。
        GateFullPercent                 = 90
        # 轮询:一轮 = 至多 CallTimeoutSeconds 的查询 + PollIntervalSeconds 的睡眠。
        PollIntervalSeconds           = 5
        CallTimeoutSeconds            = 10
        # drained 观察窗口的下限(秒),见 Get-RequiredDrainedWindowSeconds。
        MinDrainedWindowSeconds       = 60
        # 删 Pod 后等旧 gate 的 etcd 记录消失的上限:覆盖 bin/etc/base_deploy_config.yaml 的
        # Etcd.NodeTTLSeconds(180,进程被强杀、租约自然过期的最坏情况)再留 60s。
        # 改 NodeTTLSeconds 时同步改这里(测试钉住它必须更大)。
        StaleRecordWaitSeconds        = 240
        # 旧记录在 etcd 里消失之后、清排空标记之前再等这么久(2026-10-18):login 读 gate 目录用的是进程内
        # list-watch 镜像,watch 卡住时镜像最多晚 go/login/internal/logic/pkg/node/node_cache.go 的
        # cacheStaleAfter(15s)才放弃旧记录;标记清早了,那段时间里 login 会把玩家分到已删 Pod 的陈旧记录上。
        # 必须大于 cacheStaleAfter(测试钉住),改 cacheStaleAfter 时同步改这里。
        LoginMirrorSettleSeconds      = 20
        # 删 Pod 的等待 = 该 Pod 自己的 terminationGracePeriodSeconds + 本余量。
        DeleteGraceMarginSeconds      = 60
        ProgressIntervalSeconds       = 30
        # 等 drained 期间每隔几轮复核一次被排空进程的 etcd 记录(一轮至多 CallTimeout + PollInterval = 15s)。
        IdentityRecheckEveryRounds    = 6
        # 退出码:脚本结束时留下了需要人工清理的排空标记。
        ExitCodeLeftoverMarks         = 3
        # 本脚本写 draining 的 TTL 上限(秒),必须与 -DrainTtlSeconds 的 ValidateRange 上限相等 ——
        # 属性参数只能写常量,两处由 tools/scripts/tests/k8s_gate_drain.tests.ps1 钉住,不许只改一处。
        # 用途:还活着的、由本脚本写下的 draining,值一定不早于 Redis TIME - 本值;更早的值(例如手工
        # SET 成 1)login 会算出巨大的已排空时长、立刻按 deadline 放行,沿用前必须拒绝。
        MaxDrainTtlSeconds            = 86400
        # 标记脚本(原子)。KEYS[1] = 本台 draining,KEYS[2] = 本台 drained,KEYS[3..] = 同 zone 其它候选的
        # draining;ARGV[1] = 标记秒,ARGV[2] = TTL。其它候选全部在排空(或一台都没有)→ 返回
        # NoUndrainedPeerReply、什么都不写;否则 DEL 残留 drained,再 SET draining NX EX(已存在返回 nil)。
        # 检查与写入同在一个脚本里,两次并发排空不会都通过检查、把整个 zone 标满。login 用单机 Redis,
        # 不涉及跨 slot。
        MarkScript                    = "local free=0 for i=3,#KEYS do if redis.call('EXISTS',KEYS[i])==0 then free=free+1 end end if free==0 then return 'NO_UNDRAINED_PEER' end redis.call('DEL',KEYS[2]) return redis.call('SET',KEYS[1],ARGV[1],'NX','EX',ARGV[2])"
        NoUndrainedPeerReply          = 'NO_UNDRAINED_PEER'
        # 撤回脚本(原子):draining 的值仍等于 ARGV[1](本次写入的标记秒)才 DEL draining 与 drained,
        # 返回删除数;否则返回 -1、什么都不动 —— 别人在此期间重写的标记不归本次运行处理。
        ReleaseScript                 = "if redis.call('GET',KEYS[1])==ARGV[1] then return redis.call('DEL',KEYS[1],KEYS[2]) end return -1"
    }
}

# ─── kubectl 调用面 ───

# 与 k8s_deploy.ps1 的 Build-KubectlBaseArgs 同语义:非空才带 --context / --kubeconfig。
# 留空 = 落到 kubeconfig 的 current-context。
function Build-KubectlBaseArgs {
    $baseArgs = @()
    if (-not [string]::IsNullOrWhiteSpace($KubeContext)) { $baseArgs += @('--context', $KubeContext) }
    if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) { $baseArgs += @('--kubeconfig', $KubeConfig) }
    return ,$baseArgs
}

# 全局参数必须放在子命令之前:exec 的 `--` 之后全部归容器里的命令,追加在末尾会被当成
# etcdctl / redis-cli 的参数(k8s_deploy.ps1 的迁移查询追加在末尾,因为那边从不 exec)。
function Get-GateDrainKubectlArgs {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][ValidateRange(0.001, 2147483)][double]$TimeoutSeconds
    )
    $timeoutMs = [int][Math]::Ceiling($TimeoutSeconds * 1000)
    return ,(@((Build-KubectlBaseArgs)) + @("--request-timeout=${timeoutMs}ms") + $Arguments)
}

# 起外部进程并在截止时硬杀。只加 --request-timeout 限不住凭证插件等请求之外的等待,外层 Stopwatch
# 也打断不了阻塞的原生命令,所以用 Process + WaitForExit;参数逐项进 ArgumentList,不经 shell 拼接。
# 与 k8s_deploy.ps1 的 Invoke-GoSvcMigrateKubectl 同一做法(那边是入口脚本内的函数,无法 dot-source 复用)。
# 结果不经 $LASTEXITCODE 跨函数传递。
function Invoke-GateDrainProcess {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][string[]]$ArgumentList,
        [Parameter(Mandatory = $true)][ValidateRange(0.001, 2147483)][double]$TimeoutSeconds
    )
    $timeoutMs = [int][Math]::Ceiling($TimeoutSeconds * 1000)
    $process = [System.Diagnostics.Process]::new()
    try {
        $process.StartInfo.FileName = $FilePath
        $process.StartInfo.UseShellExecute = $false
        $process.StartInfo.CreateNoWindow = $true
        $process.StartInfo.RedirectStandardOutput = $true
        $process.StartInfo.RedirectStandardError = $true
        $process.StartInfo.StandardOutputEncoding = [System.Text.Encoding]::UTF8
        $process.StartInfo.StandardErrorEncoding = [System.Text.Encoding]::UTF8
        foreach ($arg in $ArgumentList) { $process.StartInfo.ArgumentList.Add([string]$arg) }
        $null = $process.Start()
        $stdout = $process.StandardOutput.ReadToEndAsync()
        $stderr = $process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit($timeoutMs)) {
            # 只杀本次调用及其子进程(凭证插件等),不触碰集群工作负载;清理最多再等 1s。
            try { $process.Kill($true) } catch { }
            $null = $process.WaitForExit(1000)
            return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = "进程超过 ${timeoutMs}ms 未结束"; TimedOut = $true }
        }
        # 继承了管道的后台子进程也不能让收尾无限阻塞。
        if (-not [System.Threading.Tasks.Task]::WaitAll([System.Threading.Tasks.Task[]]@($stdout, $stderr), 1000)) {
            return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = '进程输出管道未关闭'; TimedOut = $true }
        }
        return [pscustomobject]@{ ExitCode = $process.ExitCode; Output = $stdout.Result; ErrorOutput = $stderr.Result; TimedOut = $false }
    } catch {
        return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = $_.Exception.Message; TimedOut = $false }
    } finally {
        $process.Dispose()
    }
}

# 一次有界的 kubectl 调用。找不到 kubectl 也按失败结果返回,由调用方决定 throw 还是计入重试。
function Invoke-GateDrainKubectl {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [ValidateRange(0.001, 2147483)][double]$TimeoutSeconds = 10
    )
    $kubectl = Get-Command kubectl -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $kubectl) {
        return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = '找不到 kubectl'; TimedOut = $false }
    }
    $allArgs = Get-GateDrainKubectlArgs -Arguments $Arguments -TimeoutSeconds $TimeoutSeconds
    return Invoke-GateDrainProcess -FilePath $kubectl.Source -ArgumentList $allArgs -TimeoutSeconds $TimeoutSeconds
}

# 错误输出只留末尾一段:够定位,又不把整屏 kubectl 诊断灌进异常消息。
function Get-GateDrainErrorTail {
    param([AllowNull()][AllowEmptyString()][string]$Text)
    if ([string]::IsNullOrWhiteSpace($Text)) { return '<无错误输出>' }
    $trimmed = $Text.Trim()
    if ($trimmed.Length -le 400) { return $trimmed }
    return '...' + $trimmed.Substring($trimmed.Length - 400)
}

# 必须成功的调用:失败或超时直接 throw,返回 stdout。
function Invoke-GateDrainKubectlChecked {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [ValidateRange(0.001, 2147483)][double]$TimeoutSeconds = 10,
        [Parameter(Mandatory = $true)][string]$What
    )
    $result = Invoke-GateDrainKubectl -Arguments $Arguments -TimeoutSeconds $TimeoutSeconds
    if ($result.TimedOut -or $result.ExitCode -ne 0) {
        throw "$What 失败(exit=$($result.ExitCode),timedOut=$($result.TimedOut)):$(Get-GateDrainErrorTail $result.ErrorOutput)"
    }
    return [string]$result.Output
}

# ─── 配置解析 ───

# 解析 Go time.Duration 字面量(go-zero 配置里的 5s / 25m / 1h30m / 0),返回秒。
# 只接受 Go 能解析的形状:除 "0" 外每段都必须带单位;解析不了直接 throw,不猜。
function ConvertFrom-GoDurationSeconds {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [Parameter(Mandatory = $true)][string]$Name
    )
    $trimmed = $Text.Trim()
    if ($trimmed -in @('0', '+0', '-0')) { return [double]0 }
    $whole = [regex]::Match($trimmed, '^(?<sign>[-+]?)(?<body>(?:(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|ms|s|m|h))+)$')
    if (-not $whole.Success) { throw "$Name='$Text' 不是合法的 Go duration(形如 5s / 25m / 1h30m)" }
    $unitSeconds = @{ ns = 1e-9; us = 1e-6; ms = 1e-3; s = 1.0; m = 60.0; h = 3600.0 }
    $total = 0.0
    foreach ($part in [regex]::Matches($whole.Groups['body'].Value, '(?<n>\d+(?:\.\d*)?|\.\d+)(?<u>ns|us|ms|s|m|h)')) {
        $number = [double]::Parse($part.Groups['n'].Value, [Globalization.CultureInfo]::InvariantCulture)
        $total += $number * $unitSeconds[$part.Groups['u'].Value]
    }
    if ($whole.Groups['sign'].Value -eq '-') { $total = -$total }
    return $total
}

# 把 login 配置里的集群内地址 <service>.<namespace>[.svc[.cluster.local]]:<port> 拆开。
# 本脚本 exec 进 etcd Service 背后的 Pod 跑 etcdctl、exec 进与 Redis Service 同名的 StatefulSet 跑 redis-cli,所以只接受这种形状;
# IP、集群外域名、不带 namespace 的短名一律拒绝 —— 推不出 Service 就不猜。
function Split-ClusterServiceHost {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$HostPort,
        [Parameter(Mandatory = $true)][string]$Name
    )
    $label = '[a-z0-9](?:[-a-z0-9]*[a-z0-9])?'
    $pattern = "^(?:http://)?(?<host>(?<svc>$label)\.(?<ns>$label)(?:\.svc(?:\.cluster\.local)?)?):(?<port>\d{1,5})$"
    $match = [regex]::Match($HostPort.Trim(), $pattern)
    if (-not $match.Success) {
        throw "$Name='$HostPort' 不是 <service>.<namespace>[.svc[.cluster.local]]:<port> 形式,推不出要 exec 的 Service(本脚本只支持集群内 Service 地址)"
    }
    $port = [int]$match.Groups['port'].Value
    if ($port -lt 1 -or $port -gt 65535) { throw "$Name='$HostPort' 端口越界" }
    return [pscustomobject]@{
        Endpoint  = $HostPort.Trim()
        Host      = $match.Groups['host'].Value
        Service   = $match.Groups['svc'].Value
        Namespace = $match.Groups['ns'].Value
        Port      = $port
    }
}

# 从 login.yaml 文本取排空要用的定位与判定参数。键名对照 go/login/internal/config/config.go:
# Node.ZoneId;Node.RedisClient.{Host,DB}(排空判定循环用的就是这个 Redis 句柄);
# Registry.Etcd.Hosts,缺省 Etcd.Hosts(同 login/internal/logic/pkg/etcd.NewClient 的取法,GateWatcher 用它);
# GateDrain.{Interval,Deadline,DrainedBelowPlayers},缺键取结构体 default(见 Get-GateDrainContract)。
function ConvertFrom-LoginDrainConfig {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [Parameter(Mandatory = $true)][string]$Source
    )
    $contract = Get-GateDrainContract
    $scalars = (ConvertFrom-YamlToFlatMap -Text $Text).Scalars

    $zoneText = [string]$scalars['Node.ZoneId']
    if ($zoneText -notmatch '^\d{1,10}$' -or [uint64]$zoneText -gt [uint32]::MaxValue) {
        throw "$Source 缺少或非法的 Node.ZoneId='$zoneText'"
    }
    $redisHost = [string]$scalars['Node.RedisClient.Host']
    if ([string]::IsNullOrWhiteSpace($redisHost)) { throw "$Source 缺少 Node.RedisClient.Host" }
    $redisDbText = [string]$scalars['Node.RedisClient.DB']
    if ($redisDbText -notmatch '^\d{1,5}$') { throw "$Source 缺少或非法的 Node.RedisClient.DB='$redisDbText'(login 该字段必填,不替它猜 0)" }
    $etcdHost = [string]$scalars['Registry.Etcd.Hosts[0]']
    if ([string]::IsNullOrWhiteSpace($etcdHost)) { $etcdHost = [string]$scalars['Etcd.Hosts[0]'] }
    if ([string]::IsNullOrWhiteSpace($etcdHost)) { throw "$Source 缺少 Registry.Etcd.Hosts 与 Etcd.Hosts" }

    $intervalSeconds = [double]$contract.DefaultMonitorIntervalSeconds
    $intervalText = [string]$scalars['GateDrain.Interval']
    if (-not [string]::IsNullOrWhiteSpace($intervalText)) {
        $intervalSeconds = ConvertFrom-GoDurationSeconds -Text $intervalText -Name "$Source GateDrain.Interval"
    }
    if ($intervalSeconds -le 0) {
        throw "$Source 的 GateDrain.Interval='$intervalText':login 的排空判定循环关闭,gate:{id}:drained 永远不会出现,本脚本只能等到超时。先恢复该配置"
    }
    $deadlineSeconds = [long]$contract.DefaultDeadlineSeconds
    $deadlineText = [string]$scalars['GateDrain.Deadline']
    if (-not [string]::IsNullOrWhiteSpace($deadlineText)) {
        $parsed = ConvertFrom-GoDurationSeconds -Text $deadlineText -Name "$Source GateDrain.Deadline"
        # 与 login 的 gateDrainPolicy 同口径:正数向上取整到秒,<=0 一律 0(永不因超时放行)。
        $deadlineSeconds = if ($parsed -gt 0) { [long][Math]::Ceiling($parsed) } else { [long]0 }
    }
    $drainedBelow = [long]0
    $drainedBelowText = [string]$scalars['GateDrain.DrainedBelowPlayers']
    if (-not [string]::IsNullOrWhiteSpace($drainedBelowText)) {
        if ($drainedBelowText -notmatch '^\d{1,10}$') { throw "$Source 的 GateDrain.DrainedBelowPlayers='$drainedBelowText' 非法" }
        $drainedBelow = [long]$drainedBelowText
    }
    # 决定 login 候选集里「没自报客户端地址的 gate」算不算数(见 Select-GateDrainPeers);缺省同 login 的 default=false。
    $requireText = ([string]$scalars['RequireClientEndpoint']).Trim()
    $requireClientEndpoint = $false
    if ($requireText -ieq 'true') { $requireClientEndpoint = $true }
    elseif (-not [string]::IsNullOrEmpty($requireText) -and $requireText -ine 'false') {
        throw "$Source 的 RequireClientEndpoint='$requireText' 不是 true / false"
    }
    # 排空迁移 GateDrain.Migrate.{Enabled,RatePerSecond,StartAfter},缺键取结构体 default。只用于打印与估算。
    $migrateEnabled = [bool]$contract.DefaultMigrateEnabled
    $migrateEnabledText = ([string]$scalars['GateDrain.Migrate.Enabled']).Trim()
    if ($migrateEnabledText -ieq 'true') { $migrateEnabled = $true }
    elseif ($migrateEnabledText -ieq 'false') { $migrateEnabled = $false }
    elseif (-not [string]::IsNullOrEmpty($migrateEnabledText)) {
        throw "$Source GateDrain.Migrate.Enabled='$migrateEnabledText' is not true / false"
    }
    $migrateRate = [long]$contract.DefaultMigrateRatePerSecond
    $migrateRateText = [string]$scalars['GateDrain.Migrate.RatePerSecond']
    if (-not [string]::IsNullOrWhiteSpace($migrateRateText)) {
        if ($migrateRateText -notmatch '^\d{1,10}$' -or [uint64]$migrateRateText -gt [uint32]::MaxValue) {
            throw "$Source GateDrain.Migrate.RatePerSecond='$migrateRateText' is not an integer in [0, 4294967295]"
        }
        $migrateRate = [long]$migrateRateText
    }
    $migrateStartAfter = [long]$contract.DefaultMigrateStartAfterSeconds
    $migrateStartText = [string]$scalars['GateDrain.Migrate.StartAfter']
    if (-not [string]::IsNullOrWhiteSpace($migrateStartText)) {
        $startParsed = ConvertFrom-GoDurationSeconds -Text $migrateStartText -Name "$Source GateDrain.Migrate.StartAfter"
        # 与 login 的 gateDrainMigratePolicy 同口径:正数向上取整到秒,<=0 一律 0(立即开始)。
        $migrateStartAfter = if ($startParsed -gt 0) { [long][Math]::Ceiling($startParsed) } else { [long]0 }
    }

    return [pscustomobject]@{
        Source                 = $Source
        ZoneId                 = [uint32]$zoneText
        RequireClientEndpoint  = $requireClientEndpoint
        Redis                  = Split-ClusterServiceHost -HostPort $redisHost -Name "$Source Node.RedisClient.Host"
        RedisDb                = [int]$redisDbText
        Etcd                   = Split-ClusterServiceHost -HostPort $etcdHost -Name "$Source etcd Hosts[0]"
        MonitorIntervalSeconds = $intervalSeconds
        DeadlineSeconds        = $deadlineSeconds
        DrainedBelowPlayers    = $drainedBelow
        MigrateEnabled           = $migrateEnabled
        MigrateRatePerSecond     = $migrateRate
        MigrateStartAfterSeconds = $migrateStartAfter
    }
}

# 排空迁移的一行说明(docs/design/gate-drain-session-migration.md):login 开了迁移时,排空中的 gate 按限速把已登录的
# 连接经 msg 124 迁到同 zone 其他 gate,通常远早于 Deadline 就会 drained。纯函数,只估算、只打印,不改变流程与预算校验。
function Format-GateDrainMigrationLine {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)][long]$Online
    )
    if (-not $Login.MigrateEnabled -or $Login.MigrateRatePerSecond -le 0) {
        return ("[gate-drain] migration: off (GateDrain.Migrate.Enabled={0} RatePerSecond={1}); players leave on their own, " +
            "and whoever is still online at the deadline is disconnected when the Pod is deleted") -f `
            $Login.MigrateEnabled, $Login.MigrateRatePerSecond
    }
    $estimateSeconds = $Login.MigrateStartAfterSeconds + [long][Math]::Ceiling($Online / [double]$Login.MigrateRatePerSecond)
    return ("[gate-drain] migration: on, {0}/s starting {1}s after the mark; {2} online now -> about {3}s to move them " +
        "(login refreshes the lease every GateDrain.Interval; the gate redirects logged-in connections with msg 124)") -f `
        $Login.MigrateRatePerSecond, $Login.MigrateStartAfterSeconds, $Online, $estimateSeconds
}

# 同 zone 其余候选到水位为止还能接多少人(docs/design/gate-drain-session-migration.md §6)。纯函数,只看 etcd 记录里
# gate 自报的在线数与连接上限,口径同 login 的 gatepick:水位 = ceil(max_connections × GateFullPercent / 100),
# 已到水位的记 0。有候选没声明上限(max_connections 为 0:dev 下不设上限的 gate、旧版 gate)时无从封顶,Bounded = $false。
# 估的是此刻的余量,有两处看不到:其余候选里已经在排空的不接新人,它们的标记在 Redis 里;停写(卡住)的 gate 要比较
# 两次读取才认得出。两者都会让余量偏大,提示文案里写明。只用于提示,不改变排空流程。
function Get-GateDrainPeerHeadroom {
    param([Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Peers)
    $contract = Get-GateDrainContract
    $bounded = $true
    $headroom = [long]0
    $peerCount = 0
    foreach ($peer in $Peers) {
        if ($null -eq $peer) { continue }
        $peerCount++
        $maxConnections = [long]$peer.MaxConnections
        if ($maxConnections -le 0) { $bounded = $false; continue }
        # 整数算 ceil(max × 百分比 / 100):先凑成 100 的整倍数再除,不经过浮点。
        $scaled = $maxConnections * [long]$contract.GateFullPercent + 99
        $watermark = [long](($scaled - ($scaled % 100)) / 100)
        $free = $watermark - [long]$peer.PlayerCount
        if ($free -gt 0) { $headroom += $free }
    }
    return [pscustomobject]@{ Bounded = $bounded; Headroom = $headroom; PeerCount = $peerCount }
}

# 容量估算的一行说明。纯函数。Short = $true 表示这台 gate 上的人其余 gate 装不下,调用方用 Write-Warning 打出来:
# 多出来的人迁不走(login 不往到水位的 gate 迁人,也不回落),只能留到期限、随删 Pod 断线。
function Format-GateDrainCapacityLine {
    param(
        [Parameter(Mandatory = $true)]$Capacity,
        [Parameter(Mandatory = $true)][long]$Online
    )
    $contract = Get-GateDrainContract
    if (-not $Capacity.Bounded) {
        return [pscustomobject]@{
            Short = $false
            Text  = ("[gate-drain] peer capacity: not estimated; at least one of the {0} other gate(s) in this zone does not declare " +
                "max_connections, so there is no watermark to count against") -f $Capacity.PeerCount
        }
    }
    if ($Capacity.Headroom -ge $Online) {
        return [pscustomobject]@{
            Short = $false
            Text  = ("[gate-drain] peer capacity: {0} online here; the {1} other gate(s) can take about {2} more before their {3}% " +
                "watermark, so everyone fits (gates that are already draining or stalled are not subtracted)") -f `
                $Online, $Capacity.PeerCount, $Capacity.Headroom, $contract.GateFullPercent
        }
    }
    return [pscustomobject]@{
        Short = $true
        Text  = ("[gate-drain] peer capacity is SHORT: {0} online here, but the {1} other gate(s) can take only about {2} more before " +
            "their {3}% watermark. About {4} player(s) will find no migration target, stay on this gate, and be disconnected when the " +
            "Pod is deleted after the deadline. Scale out the gate StatefulSet before draining, or accept that. Gates that are already " +
            "draining or stalled are not subtracted, so the real shortfall can be larger") -f `
            $Online, $Capacity.PeerCount, $Capacity.Headroom, $contract.GateFullPercent, ($Online - $Capacity.Headroom)
    }
}

# 读 zone namespace 里 login 的 ConfigMap。它是 login 进程实际加载的配置,
# Redis / etcd 的位置与排空判定参数都以它为准。
function Get-LoginDrainContext {
    param([Parameter(Mandatory = $true)][string]$ZoneNamespace)
    $contract = Get-GateDrainContract
    $source = "$ZoneNamespace/$($contract.LoginConfigMap)"
    $json = Invoke-GateDrainKubectlChecked -Arguments @('get', 'configmap', $contract.LoginConfigMap, '-n', $ZoneNamespace, '-o', 'json') `
        -TimeoutSeconds $contract.CallTimeoutSeconds -What "读取 login ConfigMap $source"
    try { $configMap = $json | ConvertFrom-Json -ErrorAction Stop } catch { throw "login ConfigMap $source 的查询结果不是合法 JSON" }
    $entry = if ($null -ne $configMap.data) { $configMap.data.PSObject.Properties[$contract.LoginConfigKey] } else { $null }
    if ($null -eq $entry -or [string]::IsNullOrWhiteSpace([string]$entry.Value)) {
        throw "login ConfigMap $source 没有数据键 $($contract.LoginConfigKey)"
    }
    return ConvertFrom-LoginDrainConfig -Text ([string]$entry.Value) -Source $source
}

# ─── 入口校验 ───

# drained 最早在 Deadline 之后的第一个判定周期写入,一直活到 draining 的 TTL 到期;脚本一轮 = 至多
# CallTimeoutSeconds 的查询 + PollIntervalSeconds 的睡眠。窗口至少容下两轮才不会漏看,再与下限取大。
function Get-RequiredDrainedWindowSeconds {
    param([Parameter(Mandatory = $true)][double]$MonitorIntervalSeconds)
    $contract = Get-GateDrainContract
    $twoRounds = 2 * ($MonitorIntervalSeconds + $contract.PollIntervalSeconds + $contract.CallTimeoutSeconds)
    return [Math]::Max([double]$contract.MinDrainedWindowSeconds, $twoRounds)
}

# TTL 与等待预算都必须大于 login 的 GateDrain.Deadline 并留出观察窗口。纯函数,在任何写操作之前调用。
function Assert-DrainBudget {
    param(
        [Parameter(Mandatory = $true)][int]$DrainTtlSeconds,
        [Parameter(Mandatory = $true)][int]$WaitTimeoutSeconds,
        [Parameter(Mandatory = $true)][long]$DeadlineSeconds,
        [Parameter(Mandatory = $true)][double]$MonitorIntervalSeconds
    )
    if ($MonitorIntervalSeconds -le 0) { throw "login GateDrain.Interval<=0:排空判定循环关闭,drained 永远不会出现" }
    if ($DeadlineSeconds -le 0) {
        Write-Warning "login GateDrain.Deadline=0: drain completes only when online players fall below the threshold; otherwise it waits until the draining TTL expires and gate resumes admission. The deadline does not allow completion."
        return
    }
    $window = Get-RequiredDrainedWindowSeconds -MonitorIntervalSeconds $MonitorIntervalSeconds
    $minimum = [long][Math]::Ceiling($DeadlineSeconds + $window)
    $violations = [System.Collections.Generic.List[string]]::new()
    if ($DrainTtlSeconds -lt $minimum) {
        $violations.Add("-DrainTtlSeconds=$DrainTtlSeconds 必须 >= GateDrain.Deadline(${DeadlineSeconds}s)+ drained 观察窗口(${window}s)= ${minimum}s:期限不早于 TTL 时 draining 先过期,gate 重新接客,drained 永远等不到")
    }
    if ($WaitTimeoutSeconds -lt $minimum) {
        $violations.Add("-WaitTimeoutSeconds=$WaitTimeoutSeconds 必须 >= ${minimum}s:否则脚本在 login 按期限放行之前就先超时")
    }
    if ($violations.Count -gt 0) { throw ("排空预算不合格:" + ($violations -join ';')) }
}

# ─── 定位:Pod → etcd NodeInfo ───

# 取 gate-<Ordinal>。只处理归 StatefulSet/gate 管的、未在终止、已有 podIP 的 Pod。
function Get-GateStatefulSetPod {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][int]$Ordinal
    )
    $contract = Get-GateDrainContract
    $name = "$($contract.GateStatefulSet)-$Ordinal"
    $json = Invoke-GateDrainKubectlChecked -Arguments @('get', 'pod', $name, '-n', $Namespace, '--ignore-not-found', '-o', 'json') `
        -TimeoutSeconds $contract.CallTimeoutSeconds -What "查询 Pod $Namespace/$name"
    if ([string]::IsNullOrWhiteSpace($json)) {
        throw "Pod $Namespace/$name 不存在。本脚本只处理 external 形态的 gate StatefulSet(Pod 名 $($contract.GateStatefulSet)-<序号>);podip 形态的 gate 是 Deployment,不走本脚本"
    }
    try { $pod = $json | ConvertFrom-Json -ErrorAction Stop } catch { throw "Pod $Namespace/$name 的查询结果不是合法 JSON" }
    $owners = @($pod.metadata.ownerReferences | Where-Object {
            $null -ne $_ -and $_.kind -eq 'StatefulSet' -and $_.name -eq $contract.GateStatefulSet -and $_.controller -eq $true })
    if ($owners.Count -ne 1) { throw "Pod $Namespace/$name 不归 StatefulSet/$($contract.GateStatefulSet) 管,拒绝处理(删了不会按序号重建)" }
    if ($null -ne $pod.metadata.deletionTimestamp) { throw "Pod $Namespace/$name 正在终止,没有可排空的对象" }
    $podIp = [string]$pod.status.podIP
    if ([string]::IsNullOrWhiteSpace($podIp)) { throw "Pod $Namespace/$name 还没有 podIP(未调度或未启动)" }
    $uid = [string]$pod.metadata.uid
    if ([string]::IsNullOrWhiteSpace($uid)) { throw "Pod $Namespace/$name 缺少 metadata.uid" }
    # K8s 的缺省宽限期是 30s;显式写了就以 Pod 自己的为准。
    $grace = 30
    if ($null -ne $pod.spec -and $null -ne $pod.spec.terminationGracePeriodSeconds) { $grace = [int]$pod.spec.terminationGracePeriodSeconds }
    return [pscustomobject]@{ Name = $name; Namespace = $Namespace; Ordinal = $Ordinal; Uid = $uid; PodIp = $podIp; GraceSeconds = $grace }
}

# login GateWatcher 看的 gate 前缀(node.BuildRpcPrefix 同形,末尾带 /)。
function Get-GateEtcdPrefix {
    param([Parameter(Mandatory = $true)][uint32]$ZoneId)
    $contract = Get-GateDrainContract
    return '{0}.rpc/zone/{1}/node_type/{2}/' -f $contract.GateServiceName, $ZoneId, $contract.GateNodeType
}

# NodeInfo JSON 的字段名兼容两种写法:C++ MessageToJsonString 默认的 lowerCamelCase 与 proto 原名。
# protojson 省略零值字段,查不到返回 $null,由调用方决定缺省。
function Get-GateNodeInfoField {
    param([AllowNull()]$Object, [Parameter(Mandatory = $true)][string[]]$Names)
    if ($null -eq $Object) { return $null }
    foreach ($name in $Names) {
        $property = $Object.PSObject.Properties[$name]
        if ($null -ne $property -and $null -ne $property.Value) { return $property.Value }
    }
    return $null
}

# 解析 `etcdctl get <prefix> --prefix --write-out=json`(键和值都是 base64)。
# 只认 <prefix>node_id/<数字> 的键;值里的 node_id(省略 = 0)必须与键尾一致,
# 不一致说明记录损坏 —— 标错 node_id 等于排空了别的 gate,fail-closed。
function ConvertFrom-GateEtcdRecords {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Json,
        [Parameter(Mandatory = $true)][string]$Prefix
    )
    try { $reply = $Json | ConvertFrom-Json -ErrorAction Stop } catch { throw "etcdctl 输出不是合法 JSON" }
    if ($null -eq $reply -or $null -eq $reply.PSObject.Properties['header']) {
        throw "etcdctl 输出缺少 header,不是 get --write-out=json 的结果"
    }
    $keyPattern = '^' + [regex]::Escape($Prefix) + 'node_id/(?<id>\d{1,10})$'
    $records = [System.Collections.Generic.List[object]]::new()
    foreach ($kv in @($reply.kvs)) {
        if ($null -eq $kv) { continue }
        try {
            $key = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String([string]$kv.key))
            $valueText = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String([string]$kv.value))
        } catch { throw "etcd 记录的键或值不是合法 base64" }
        $keyMatch = [regex]::Match($key, $keyPattern)
        if (-not $keyMatch.Success) { continue }
        $keyId = [uint64]$keyMatch.Groups['id'].Value
        try { $info = $valueText | ConvertFrom-Json -ErrorAction Stop } catch { throw "etcd 记录 $key 的值不是合法 NodeInfo JSON" }
        $valueIdRaw = Get-GateNodeInfoField -Object $info -Names @('nodeId', 'node_id')
        $valueId = if ($null -eq $valueIdRaw) { [uint64]0 } else { [uint64]$valueIdRaw }
        if ($valueId -ne $keyId -or $keyId -gt [uint32]::MaxValue) {
            throw "etcd 记录 $key 的 node_id=$valueId 与键不一致,记录损坏,拒绝据此标记"
        }
        $endpoint = Get-GateNodeInfoField -Object $info -Names @('endpoint')
        $clientEndpoint = Get-GateNodeInfoField -Object $info -Names @('clientEndpoint', 'client_endpoint')
        $players = Get-GateNodeInfoField -Object $info -Names @('playerCount', 'player_count')
        # gate 自报的客户端连接上限(NodeInfo.max_connections);省略 = 0 = 没声明上限。只用于容量估算。
        $maxConnections = Get-GateNodeInfoField -Object $info -Names @('maxConnections', 'max_connections')
        $endpointPort = Get-GateNodeInfoField -Object $endpoint -Names @('port')
        $clientPort = Get-GateNodeInfoField -Object $clientEndpoint -Names @('port')
        # protojson 把 uint64 写成字符串,[uint64] 两种都接。
        $launchTime = Get-GateNodeInfoField -Object $info -Names @('launchTime', 'launch_time')
        $zoneId = Get-GateNodeInfoField -Object $info -Names @('zoneId', 'zone_id')
        $records.Add([pscustomobject]@{
                Key          = $key
                NodeId       = [uint32]$keyId
                HasEndpoint  = $null -ne $endpoint
                Ip           = [string](Get-GateNodeInfoField -Object $endpoint -Names @('ip'))
                EndpointPort = if ($null -eq $endpointPort) { [uint32]0 } else { [uint32]$endpointPort }
                ClientIp     = [string](Get-GateNodeInfoField -Object $clientEndpoint -Names @('ip'))
                ClientPort   = if ($null -eq $clientPort) { [uint32]0 } else { [uint32]$clientPort }
                LaunchTime   = if ($null -eq $launchTime) { [uint64]0 } else { [uint64]$launchTime }
                ZoneId       = if ($null -eq $zoneId) { [uint32]0 } else { [uint32]$zoneId }
                Uuid         = [string](Get-GateNodeInfoField -Object $info -Names @('nodeUuid', 'node_uuid'))
                PlayerCount  = if ($null -eq $players) { [uint32]0 } else { [uint32]$players }
                MaxConnections = if ($null -eq $maxConnections) { [uint32]0 } else { [uint32]$maxConnections }
            })
    }
    return ,$records.ToArray()
}

# etcd:exec 进 Service 背后的 Pod 再按 Service 地址连回(hairpin)失败时附在错误后面的提示。
# 刻意不自动回落 127.0.0.1:回落后连的是 exec 恰好落到的那个 Pod,多副本时不一定是 login 用的那一个。
function Get-GateDrainHairpinHint {
    param([Parameter(Mandatory = $true)]$Target)
    return "(若是连接失败 / 超时:本脚本 exec 进 svc/$($Target.Service) 后按 login 配置里的 $($Target.Endpoint) 连回同一 Service,集群的 CNI / kube-proxy 须支持 hairpin)"
}

# Redis 命令失败时附在错误后面的提示。两种形态的原因不同,静态写全,不为了选一句再多查一次集群(错误路径保持简单;RBAC 也不一定允许读 Deployment):
# 单实例形态下 exec 落在 Service redis 背后的同一个 Pod,要 hairpin;高可用形态下 Service redis 指向 redis-proxy,不涉及 hairpin,
# 连接被立即关闭多半是代理此刻没有可路由的主(切主中、哨兵没有多数)或代理没就绪。排查命令引用 README(带口令时要 REDISCLI_AUTH),不在这里内联。
function Get-GateDrainRedisFailureHint {
    param([Parameter(Mandatory = $true)]$Login)
    return ("(若是连接失败 / 被关闭 / 超时:本脚本 exec 进 $(Get-GateDrainRedisExecTarget -Login $Login) 的 redis 容器后按 login 配置里的 $($Login.Redis.Endpoint) 连。" +
        "单实例形态下 Service redis 背后就是这个 Pod,集群的 CNI / kube-proxy 须支持 hairpin;" +
        "高可用形态下 Service redis 指向 redis-proxy,不涉及 hairpin,连接被立即关闭多半是代理此刻没有可路由的主(切主中或哨兵没有多数)或代理没就绪," +
        "按 deploy/k8s/README.md「共享 Redis:高可用形态 → 运维」查代理的 show servers state 与三个哨兵的 get-master-addr-by-name;exec 本身失败先看 statefulset/redis 的 Pod 状态)")
}

# exec 进 login 所连 etcd 的 Service 背后的 Pod,用它自带的 etcdctl 按 login 配置里的同一地址读 gate 前缀。
function Get-GateEtcdRecords {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [ValidateRange(0.001, 2147483)][double]$TimeoutSeconds = 10
    )
    $prefix = Get-GateEtcdPrefix -ZoneId $Login.ZoneId
    $arguments = @('exec', '-n', $Login.Etcd.Namespace, "svc/$($Login.Etcd.Service)", '--',
        'etcdctl', "--endpoints=$($Login.Etcd.Endpoint)", '--dial-timeout=3s', '--command-timeout=5s',
        'get', $prefix, '--prefix', '--write-out=json')
    try {
        $json = Invoke-GateDrainKubectlChecked -Arguments $arguments -TimeoutSeconds $TimeoutSeconds -What "读取 etcd 前缀 $prefix"
    } catch {
        throw "$($_.Exception.Message) $(Get-GateDrainHairpinHint -Target $Login.Etcd)"
    }
    return ,(ConvertFrom-GateEtcdRecords -Json $json -Prefix $prefix)
}

# podIP 必须恰好对上一条记录。0 条 = gate 还没注册完或前缀不对;多条 = 同 IP 上还挂着旧进程的陈旧记录。
function Select-GateNodeRecord {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Records,
        [Parameter(Mandatory = $true)][string]$PodIp,
        [Parameter(Mandatory = $true)][string]$Prefix
    )
    $matched = @($Records | Where-Object { $null -ne $_ -and $_.Ip -eq $PodIp })
    if ($matched.Count -eq 0) {
        throw "etcd 前缀 $Prefix 下没有 endpoint.ip == $PodIp 的 gate 记录(共 $($Records.Count) 条):gate 还没注册完,或 zone_id 不对。拿不到 node_id 就无法标记,拒绝继续"
    }
    if ($matched.Count -gt 1) {
        $ids = ($matched | ForEach-Object { $_.NodeId }) -join ','
        throw "etcd 前缀 $Prefix 下有 $($matched.Count) 条 endpoint.ip == $PodIp 的记录(node_id=$ids):同 IP 上还挂着旧进程的陈旧记录,等其租约过期(Etcd.NodeTTLSeconds)后重试"
    }
    return $matched[0]
}

# login 把这条记录下发给客户端时用的地址;不进候选集返回 $null。
# 镜像 shared/clientendpoint.Select 与 buildGateCandidates 的前置过滤:缺 endpoint 或不属本 zone 的跳过;
# client_endpoint 可用(host 非空且 port 在 1..65535)就用它;RequireClientEndpoint=true 时不回落;
# 否则回落 endpoint,endpoint 也不可用则跳过。
function Get-GateClientFacingAddress {
    param(
        [Parameter(Mandatory = $true)]$Record,
        [Parameter(Mandatory = $true)]$Login
    )
    if (-not $Record.HasEndpoint) { return $null }
    if ($Login.ZoneId -ne 0 -and $Record.ZoneId -ne $Login.ZoneId) { return $null }
    if (-not [string]::IsNullOrEmpty($Record.ClientIp) -and $Record.ClientPort -ge 1 -and $Record.ClientPort -le 65535) {
        return "$($Record.ClientIp):$($Record.ClientPort)"
    }
    if ($Login.RequireClientEndpoint) { return $null }
    if (-not [string]::IsNullOrEmpty($Record.Ip) -and $Record.EndpointPort -ge 1 -and $Record.EndpointPort -le 65535) {
        return "$($Record.Ip):$($Record.EndpointPort)"
    }
    return $null
}

# 本 zone 里除目标之外、login 会下发的 gate(候选集口径同 login 的 buildGateCandidates:选地址 →
# 按地址去重、只留 launch_time 最大者,见 shared/clientendpoint.DedupeNewest)。一台都没有就拒绝:
# login 挑 gate 在候选全部 draining 时退到「连得上的」那一档照常分配(shared/gatepick.Eligible),标了挡不住新玩家,之后带
# -DeletePod 删 Pod 会断掉排空期间新分进来的全部玩家。与目标同地址的记录(目标的陈旧影子)不算。
# 这里只看 etcd;「其它候选是不是也在排空」由标记脚本在写入时原子判定(见 MarkScript)。
function Select-GateDrainPeers {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Records,
        [Parameter(Mandatory = $true)]$Target,
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)][string]$Prefix
    )
    $targetAddress = Get-GateClientFacingAddress -Record $Target -Login $Login
    $winners = [ordered]@{}
    foreach ($record in $Records) {
        if ($null -eq $record) { continue }
        $address = Get-GateClientFacingAddress -Record $record -Login $Login
        if ($null -eq $address) { continue }
        # 严格大于才替换:launch_time 相同时先出现者胜,与 DedupeNewest 一致。
        if (-not $winners.Contains($address) -or $record.LaunchTime -gt $winners[$address].LaunchTime) { $winners[$address] = $record }
    }
    $peers = @(foreach ($entry in $winners.GetEnumerator()) {
            if ($entry.Key -ne $targetAddress -and $entry.Value.NodeId -ne $Target.NodeId) { $entry.Value }
        })
    if ($peers.Count -eq 0) {
        throw ("etcd 前缀 $Prefix 下除 node_id=$($Target.NodeId) 外没有别的可分配 gate(按 login 候选集口径:同 zone、" +
            "有可下发地址 [RequireClientEndpoint=$($Login.RequireClientEndpoint)]、按地址去重)。login 在候选全部排空时会忽略标记照常分配," +
            "排空挡不住新玩家,删 Pod 会断掉排空期间分进来的所有玩家。先扩容 gate StatefulSet 并等新 gate 注册后再排空;拒绝继续")
    }
    return ,$peers
}

# ─── Redis(login 所用的那个)───

# 用哪个 Pod 里的 redis-cli:login 所连 Redis Service 同名的 StatefulSet(共享 Redis 的两种形态都叫 redis)里任一 Pod 的 redis 容器。
# 不 exec svc/<Service>:高可用形态(redis-ha.yaml)下 Service redis 指向 haproxy 入口代理,代理镜像里没有 redis-cli。
# 连哪台仍由 -h 决定(login 配置里的 Service 地址):高可用形态下经代理落到当前的主,单实例就是它自己 —— 与 login 写的是同一处,
# 不随 exec 落到哪个 Pod(可能是从)而变。
function Get-GateDrainRedisExecTarget {
    param([Parameter(Mandatory = $true)]$Login)
    return "statefulset/$($Login.Redis.Service)"
}

# exec 进 login 所连共享 Redis 的节点 Pod(Get-GateDrainRedisExecTarget),用它自带的 redis-cli 连 login 配置里的同一 host:port/DB。
# 密码取该 Pod 自己的 REDIS_PASSWORD(Secret redis-auth,与 requirepass 同源),经 REDISCLI_AUTH 传给 redis-cli:
# 不经本机命令行、不进日志;为空时不设(设成空串 redis-cli 会发 AUTH "" 并报错)。
# 参数经 sh 的位置参数传入,不拼进脚本文本。--no-raw 让 nil 与空串可区分。-c redis:高可用形态的节点 Pod 另有哨兵容器。
function Invoke-GateDrainRedis {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)][string[]]$Command,
        [ValidateRange(0.001, 2147483)][double]$TimeoutSeconds = 10
    )
    $shell = 'h=$1; p=$2; d=$3; shift 3; if [ -n "$REDIS_PASSWORD" ]; then REDISCLI_AUTH=$REDIS_PASSWORD; export REDISCLI_AUTH; fi; exec redis-cli --no-raw -h "$h" -p "$p" -n "$d" "$@"'
    $arguments = @('exec', '-n', $Login.Redis.Namespace, (Get-GateDrainRedisExecTarget -Login $Login), '-c', 'redis', '--',
        'sh', '-c', $shell, 'sh', $Login.Redis.Host, [string]$Login.Redis.Port, [string]$Login.RedisDb) + $Command
    return Invoke-GateDrainKubectl -Arguments $arguments -TimeoutSeconds $TimeoutSeconds
}

# 必须成功的 Redis 命令:进程失败、超时、或回复是 (error) 都 throw。返回原始回复文本。
function Invoke-GateDrainRedisChecked {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)][string[]]$Command,
        [Parameter(Mandatory = $true)][string]$What
    )
    $contract = Get-GateDrainContract
    $result = Invoke-GateDrainRedis -Login $Login -Command $Command -TimeoutSeconds $contract.CallTimeoutSeconds
    if ($result.TimedOut -or $result.ExitCode -ne 0) {
        throw "$What 失败(exit=$($result.ExitCode),timedOut=$($result.TimedOut)):$(Get-GateDrainErrorTail $result.ErrorOutput) $(Get-GateDrainRedisFailureHint -Login $Login)"
    }
    $text = [string]$result.Output
    if ($text -match '(?m)^\s*(\d+\)\s*)?\(error\)') { throw "$What 失败:$(Get-GateDrainErrorTail $text)" }
    return $text
}

# 解析 redis-cli --no-raw 的单个回复值:(nil) → $null;"..." → 字符串;(integer) n → [long];
# OK 之类的状态回复 → 原文;(error) → throw。本脚本的值域只有数字与判定理由,带转义的字符串一律拒绝。
function ConvertFrom-RedisCliValue {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Line)
    $text = $Line.Trim()
    if ($text -eq '(nil)') { return $null }
    if ($text -match '^\(error\)') { throw "Redis 返回错误:$text" }
    $integer = [regex]::Match($text, '^\(integer\) (?<n>-?\d{1,19})$')
    if ($integer.Success) { return [long]$integer.Groups['n'].Value }
    $quoted = [regex]::Match($text, '^"(?<v>[^"\\]*)"$')
    if ($quoted.Success) { return $quoted.Groups['v'].Value }
    if ($text -match '^[A-Za-z]+$') { return $text }
    throw "无法识别的 redis-cli 回复:'$text'"
}

# 单条回复:恰好一行非空。
function ConvertFrom-RedisCliSingle {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    $lines = @($Text -split "`r?`n" | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    if ($lines.Count -ne 1) { throw "期望 1 行 redis-cli 回复,实际 $($lines.Count) 行:'$(Get-GateDrainErrorTail $Text)'" }
    return ConvertFrom-RedisCliValue -Line $lines[0]
}

# 多条回复(MGET / TIME):每行 "<序号>) <值>",序号从 1 连续,条数必须等于 ExpectedCount。
function ConvertFrom-RedisCliArray {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [Parameter(Mandatory = $true)][int]$ExpectedCount
    )
    $values = [System.Collections.Generic.List[object]]::new()
    foreach ($line in ($Text -split "`r?`n")) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        $item = [regex]::Match($line.Trim(), '^(?<i>\d{1,9})\) (?<v>.*)$')
        if (-not $item.Success -or [int]$item.Groups['i'].Value -ne $values.Count + 1) {
            throw "无法识别的 redis-cli 多条回复:'$(Get-GateDrainErrorTail $Text)'"
        }
        $values.Add((ConvertFrom-RedisCliValue -Line $item.Groups['v'].Value))
    }
    if ($values.Count -ne $ExpectedCount) { throw "redis-cli 回复 $($values.Count) 条,期望 $ExpectedCount 条" }
    return ,$values.ToArray()
}

function Get-GateDrainKeys {
    param([Parameter(Mandatory = $true)][uint32]$NodeId)
    $contract = Get-GateDrainContract
    return [pscustomobject]@{
        Draining = $contract.DrainingKeyFormat -f $NodeId
        Drained  = $contract.DrainedKeyFormat -f $NodeId
    }
}

# draining 的值必须是 [NotBeforeUnix, NotAfterUnix] 内(且为正)、int64 范围内的整数 Unix 秒。login 用
# strconv.ParseInt 读它并忽略错误:非数字或 "0" 按 0 算,等于「已经等了 50 多年」,下一个判定周期就按
# deadline 放行 —— 替人做了断线决定;偏小的正整数(例如手工 SET 成 1)同理;溢出 int64 被夹到 MaxInt64、
# 晚于现在的值被当成「刚标记」,期限都不再按实际经过的时间走。下界由调用方按 TTL 上限给出
# (见 Get-GateDrainContract.MaxDrainTtlSeconds)。
function Test-GateDrainMarkValue {
    param(
        [AllowNull()][AllowEmptyString()][string]$Value,
        [long]$NotBeforeUnix = 0,
        [long]$NotAfterUnix = [long]::MaxValue
    )
    $seconds = [long]0
    if ($Value -notmatch '^\d{1,19}$' -or -not [long]::TryParse($Value, [ref]$seconds)) { return $false }
    return ($seconds -gt 0 -and $seconds -ge $NotBeforeUnix -and $seconds -le $NotAfterUnix)
}

# 标记排空。顺序:
#   1. TIME:取 Redis 服务器时间作标记值 —— login 拿集群内的时钟去比,不受运维机器时钟漂移影响。只读。
#   2. 一次 EVAL MarkScript(原子):PeerNodeIds(Select-GateDrainPeers 选出的同 zone 其它候选)全部在排空
#      → 什么都不写,拒绝;否则 DEL 残留的 drained,再 SET draining NX EX。残留来源:login 读到 draining 的剩余
#      TTL 之后、SET drained 之前 draining 被删(清标记 / 运维取消),或只删了 draining,而该 node_id 当时不在
#      login 快照里、没被顺手清掉 —— 这两种残留的寿命都不超过原 draining(login 拿不到正的剩余 TTL 时不写
#      drained,不再回落成固定时长,见 gatedrain_monitor.go 同段注释)。draining 已存在时,login 下一个判定
#      周期按现状重写 drained。
#   3. 已有标记就沿用、不重置期限;但必须带 TTL(否则脚本中止后这台 gate 永久被排除),值必须是
#      [Redis TIME - MaxDrainTtlSeconds, Redis TIME] 内的整数 Unix 秒(见 Test-GateDrainMarkValue):
#      本脚本写的标记 TTL 不超过 MaxDrainTtlSeconds,还活着就一定不早于这个下界。
# 返回 Value = draining 的值;Created = 是否本次写入(只有本次写入的才可由本次运行撤回)。
function Set-GateDrainingMark {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)][uint32]$NodeId,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][uint32[]]$PeerNodeIds,
        [Parameter(Mandatory = $true)][int]$TtlSeconds
    )
    $contract = Get-GateDrainContract
    $keys = Get-GateDrainKeys -NodeId $NodeId
    $time = ConvertFrom-RedisCliArray -Text (Invoke-GateDrainRedisChecked -Login $Login -Command @('TIME') -What '读取 Redis 服务器时间') -ExpectedCount 2
    $now = [string]$time[0]
    if (-not (Test-GateDrainMarkValue $now)) { throw "Redis TIME 返回的秒数 '$now' 不是正整数" }

    $peerKeys = @($PeerNodeIds | ForEach-Object { (Get-GateDrainKeys -NodeId $_).Draining })
    $command = @('EVAL', $contract.MarkScript, [string](2 + $peerKeys.Count), $keys.Draining, $keys.Drained) + $peerKeys + @($now, [string]$TtlSeconds)
    $reply = ConvertFrom-RedisCliSingle -Text (Invoke-GateDrainRedisChecked -Login $Login -Command $command -What "写入 $($keys.Draining)")
    if ($reply -is [string] -and $reply -eq $contract.NoUndrainedPeerReply) {
        throw ("同 zone 其它候选 gate(node_id=$($PeerNodeIds -join ','))全部在排空:login 在候选全部排空时会忽略标记照常分配," +
            "再标这台也挡不住新玩家。未做任何写入;等其它排空结束(或取消其中一台)后重跑")
    }
    if ($reply -is [string] -and $reply -eq 'OK') {
        Write-Host "[gate-drain] Marked $($keys.Draining)=$now (TTL ${TtlSeconds}s): another gate in this zone is still accepting players; login will stop assigning new players to this gate"
        return [pscustomobject]@{ MarkedAt = [long]$now; Value = $now; Created = $true }
    }
    if ($null -ne $reply) { throw "写入 $($keys.Draining) 得到意外回复 '$reply'" }

    $existing = ConvertFrom-RedisCliSingle -Text (Invoke-GateDrainRedisChecked -Login $Login -Command @('GET', $keys.Draining) -What "读取已有的 $($keys.Draining)")
    if ($null -eq $existing) { throw "$($keys.Draining) 在 SET NX 未成功后又消失了(恰好过期或被并发取消),状态不确定,请重跑" }
    $ttl = ConvertFrom-RedisCliSingle -Text (Invoke-GateDrainRedisChecked -Login $Login -Command @('TTL', $keys.Draining) -What "读取已有 $($keys.Draining) 的 TTL")
    if ($ttl -isnot [long]) { throw "读取 $($keys.Draining) 的 TTL 得到意外回复 '$ttl'" }
    if ($ttl -le 0) {
        throw "已有的 $($keys.Draining) TTL=$ttl(-1 = 永不过期,-2 = 已消失):沿用它,脚本中止后这台 gate 会被永久排除。先人工 DEL $($keys.Draining) $($keys.Drained) 再重跑"
    }
    $notBefore = [long]$now - [long]$contract.MaxDrainTtlSeconds
    if (-not (Test-GateDrainMarkValue ([string]$existing) -NotBeforeUnix $notBefore -NotAfterUnix ([long]$now))) {
        throw ("已有的 $($keys.Draining)='$existing' 不是 (0, Redis TIME=$now] 内、且不早于 TIME-$($contract.MaxDrainTtlSeconds)=$notBefore 的整数 Unix 秒:" +
            "login 按它算出的排空时长不可信 —— 0 / 非数字,以及早于 TIME-$($contract.MaxDrainTtlSeconds) 的值(例如手工 SET 成 1)," +
            "login 会立刻按 deadline 放行,带 -DeletePod 时就是直接断线。本脚本写的标记 TTL 不超过 $($contract.MaxDrainTtlSeconds)s," +
            "这样的值不是本脚本写的,不沿用。先人工 DEL $($keys.Draining) $($keys.Drained) 再重跑")
    }
    Write-Warning "Existing drain mark $($keys.Draining)=$existing (elapsed $([long]$now - [long]$existing)s, remaining TTL ${ttl}s); reusing it without resetting the deadline"
    return [pscustomobject]@{ MarkedAt = [long]$existing; Value = [string]$existing; Created = $false }
}

# 撤回本次写入的 draining(连同 login 可能已据它写下的 drained)。EVAL ReleaseScript 原子比对:值仍等于
# 本次写入值才删,别人在此期间重写过的标记不动。返回 $true = 已撤回;$false = 值已不是本次写入的。
function Remove-OwnGateDrainMark {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)][uint32]$NodeId,
        [Parameter(Mandatory = $true)][string]$MarkValue
    )
    $contract = Get-GateDrainContract
    $keys = Get-GateDrainKeys -NodeId $NodeId
    $reply = ConvertFrom-RedisCliSingle -Text (Invoke-GateDrainRedisChecked -Login $Login `
            -Command @('EVAL', $contract.ReleaseScript, '2', $keys.Draining, $keys.Drained, $MarkValue) -What "撤回 $($keys.Draining)")
    if ($reply -isnot [long]) { throw "撤回 $($keys.Draining) 得到意外回复 '$reply'" }
    return ($reply -ge 1)
}

# 有界轮询 drained。每轮一次 MGET 同时看 draining 与 drained:
#   draining 消失 → gate 已重新接客(被取消 / TTL 到期),throw,绝不删 Pod;
#   draining 的值 ≠ MarkValue(Set-GateDrainingMark 写入或校验后沿用的值)→ 被改写过,login 的期限已不按本次
#   排空计算(例如被手工 SET 成 1 会立刻按 deadline 放行),throw,绝不删 Pod。按值比对与 ReleaseScript 同一口径,
#   比每轮重复做区间检查更严:MarkValue 本身已校验过,相等即合法。别人 SET 成恰好相同的值无法区分,但那时
#   login 算出的期限也与本次相同;
#   drained ∈ {below_threshold, deadline} → 返回;其它值 → throw;
#   查询失败 / 回复无法识别 → 只告警计数、继续等,绝不当成 drained。
# drained 的 TTL 跟随 draining 剩余 TTL,默认只剩约 5 分钟窗口;PollIntervalSeconds 必须明显短于它,
# 入口校验(Assert-DrainBudget)已保证窗口至少容下两轮查询 + 睡眠。
# 每 IdentityRecheckEveryRounds 轮复核一次 Record 对应的进程是否还在 etcd(Test-GateRecordGone):进程没了,
# login 的判定循环只评估快照里的 node_id,drained 永远不会来,标记还会挡住之后复用该 node_id 的 gate ——
# 返回 IdentityLost=$true,由调用方撤回标记并中止。查询失败算「未知」,继续等。
# 进程租约短暂丢失、重注册之前恰好被复核看到,也会判成消失:方向是中止并撤回(gate 重新接客),重跑即可。
function Wait-GateDrained {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)][uint32]$NodeId,
        [Parameter(Mandatory = $true)]$Record,
        # Set-GateDrainingMark 返回的 Value;轮询期间 draining 必须一直等于它。
        [Parameter(Mandatory = $true)][ValidatePattern('^\d{1,19}$')][string]$MarkValue,
        [Parameter(Mandatory = $true)][ValidateRange(0.001, 2147483)][double]$TimeoutSeconds,
        [Parameter(Mandatory = $true)][ValidateRange(0.001, 3600)][double]$PollIntervalSeconds
    )
    $contract = Get-GateDrainContract
    $keys = Get-GateDrainKeys -NodeId $NodeId
    $reasons = @($contract.DrainedReasonBelowThreshold, $contract.DrainedReasonDeadline)
    $budget = [System.Diagnostics.Stopwatch]::StartNew()
    $failures = 0
    $rounds = 0
    $nextProgress = [double]$contract.ProgressIntervalSeconds
    while ($true) {
        $remaining = $TimeoutSeconds - $budget.Elapsed.TotalSeconds
        if ($remaining -lt 0.001) { break }
        $result = Invoke-GateDrainRedis -Login $Login -Command @('MGET', $keys.Draining, $keys.Drained) `
            -TimeoutSeconds ([Math]::Min([double]$contract.CallTimeoutSeconds, $remaining))
        $values = $null
        $problem = Get-GateDrainErrorTail $result.ErrorOutput
        if (-not $result.TimedOut -and $result.ExitCode -eq 0) {
            try { $values = ConvertFrom-RedisCliArray -Text ([string]$result.Output) -ExpectedCount 2 } catch { $problem = $_.Exception.Message }
        }
        if ($null -eq $values) {
            $failures++
            Write-Warning "Failed to query $($keys.Drained) (failure $failures; continuing to wait without treating it as drained): $problem"
        } else {
            if ($null -eq $values[0]) {
                throw "$($keys.Draining) 已不存在(被取消或 TTL 到期):这台 gate 已重新参与分配,中止,不删 Pod"
            }
            if ([string]$values[0] -cne $MarkValue) {
                throw ("$($keys.Draining) 已被改写:当前值 '$($values[0])' ≠ 本次确认的 '$MarkValue'(被人工 SET / 重新标记," +
                    "或不是整数 Unix 秒),login 的排空期限不再按本次标记计算,判定不可信,中止,不删 Pod。" +
                    "确认无人在排空这台 gate 后,人工 DEL $($keys.Draining) $($keys.Drained) 再重跑")
            }
            if ($null -ne $values[1]) {
                if ([string]$values[1] -notin $reasons) { throw "未知的 $($keys.Drained) 值 '$($values[1])',拒绝据此删 Pod" }
                return [pscustomobject]@{ Reason = [string]$values[1]; MarkedAt = [long]$values[0]; WaitedSeconds = $budget.Elapsed.TotalSeconds; IdentityLost = $false }
            }
        }
        $rounds++
        if ($rounds % $contract.IdentityRecheckEveryRounds -eq 0) {
            $remaining = $TimeoutSeconds - $budget.Elapsed.TotalSeconds
            if ($remaining -lt 0.001) { break }
            $gone = Test-GateRecordGone -Login $Login -Record $Record -TimeoutSeconds ([Math]::Min([double]$contract.CallTimeoutSeconds, $remaining))
            if ($gone -eq $true) {
                return [pscustomobject]@{ Reason = $null; MarkedAt = [long]0; WaitedSeconds = $budget.Elapsed.TotalSeconds; IdentityLost = $true }
            }
        }
        if ($budget.Elapsed.TotalSeconds -ge $nextProgress) {
            Write-Host ("[gate-drain] Waiting for {0}: elapsed {1:N0}s / {2}s" -f $keys.Drained, $budget.Elapsed.TotalSeconds, $TimeoutSeconds)
            $nextProgress += $contract.ProgressIntervalSeconds
        }
        $remaining = $TimeoutSeconds - $budget.Elapsed.TotalSeconds
        if ($remaining -lt 0.001) { break }
        Start-Sleep -Milliseconds ([int][Math]::Max(1, [Math]::Min($PollIntervalSeconds * 1000, [Math]::Floor($remaining * 1000))))
    }
    throw ("等待 {0} 超过 {1}s 仍未出现,不删 Pod。draining 标记保留到 TTL 到期(期间这台 gate 不接新玩家);" +
        "要取消,在 login 的 Redis({2} DB {3})上 DEL {4} {0}。另请确认 login 的排空判定循环在跑(日志前缀 [GateDrain])") -f `
        $keys.Drained, $TimeoutSeconds, $Login.Redis.Endpoint, $Login.RedisDb, $keys.Draining
}

# reason=deadline:期限到了还有人。删 Pod 会让他们断线,靠客户端重连 login 改派到别的 gate。
# 在线数取 etcd 里 gate 自报的 player_count,尽力而为:读不到也照样告警。
function Write-GateDeadlineWarning {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)]$Pod,
        [Parameter(Mandatory = $true)]$Record
    )
    $online = 'unknown'
    try {
        $current = @((Get-GateEtcdRecords -Login $Login) | Where-Object { $_.NodeId -eq $Record.NodeId -and $_.Uuid -eq $Record.Uuid })
        if ($current.Count -eq 1) { $online = [string]$current[0].PlayerCount }
    } catch {
        # 在线数只是告警里的补充信息,读取失败不能吞掉告警本身。
        $online = "unknown(read failed: $($_.Exception.Message))"
    }
    $banner = '!' * 78
    Write-Host $banner -ForegroundColor Red
    Write-Host "!! gate $($Pod.Name) (node_id=$($Record.NodeId)) was marked drained by DEADLINE: players remain online after the deadline (etcd reports $online)." -ForegroundColor Red
    Write-Host "!! Deleting this Pod will disconnect those players; clients must reconnect to login for assignment to another gate." -ForegroundColor Red
    Write-Host "!! Disconnect timing is an operations decision: without -DeletePod the script stops here; with it, deletion continues." -ForegroundColor Red
    Write-Host $banner -ForegroundColor Red
    Write-Warning "gate:$($Record.NodeId):drained=deadline; players remain online ($online); deleting the Pod will disconnect them"
}

# 复核 Pod 还是定位时那一个(UID),etcd 里该 podIP 还是同一进程(node_id + uuid)。标记写完立即复核一次
# (定位与 SET 之间 gate 重启换了 node_id,标记就落在不在快照里的号上),删 Pod 前再复核一次。
# 任何一项变了都说明 gate 重启过:新进程可能换了 node_id、已经在接客,删它会断掉没排空过的玩家。
# 残留竞态:复核与 delete 之间 Pod 被替换(kubectl delete 没有 UID 前置条件),窗口是一次 API 往返。
function Assert-GateUnchanged {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)]$Pod,
        [Parameter(Mandatory = $true)]$Record
    )
    $current = Get-GateStatefulSetPod -Namespace $Pod.Namespace -Ordinal $Pod.Ordinal
    if ($current.Uid -ne $Pod.Uid) {
        throw "Pod $($Pod.Name) 已被替换(uid $($Pod.Uid) → $($current.Uid)),新 Pod 没有排空过,中止(不删 Pod)。重新运行本脚本"
    }
    $prefix = Get-GateEtcdPrefix -ZoneId $Login.ZoneId
    $now = Select-GateNodeRecord -Records (Get-GateEtcdRecords -Login $Login) -PodIp $current.PodIp -Prefix $prefix
    if ($now.NodeId -ne $Record.NodeId -or $now.Uuid -ne $Record.Uuid) {
        throw "gate 进程重启过(node_id/uuid $($Record.NodeId)/$($Record.Uuid) → $($now.NodeId)/$($now.Uuid)):排空标记打在旧 node_id 上,新进程可能已在接客,中止(不删 Pod)。重新运行本脚本"
    }
}

# 旧进程的 etcd 记录是否已消失:按 uuid 认进程(新 gate 常复用同一 node_id,甚至同一 podIP);
# 缺 uuid 时退回 node_id + ip。查询失败返回 $null(未知),由调用方继续等。
function Test-GateRecordGone {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)]$Record,
        [ValidateRange(0.001, 2147483)][double]$TimeoutSeconds = 10
    )
    try { $records = Get-GateEtcdRecords -Login $Login -TimeoutSeconds $TimeoutSeconds } catch { return $null }
    $byUuid = -not [string]::IsNullOrEmpty($Record.Uuid)
    $still = @($records | Where-Object {
            if ($byUuid) { $_.Uuid -eq $Record.Uuid } else { $_.NodeId -eq $Record.NodeId -and $_.Ip -eq $Record.Ip }
        })
    return ($still.Count -eq 0)
}

# 有界等待旧记录消失。优雅退出时 gate 会撤租约、记录立刻消失;被强杀则等租约自然过期。
function Wait-GateRecordGone {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)]$Record,
        [Parameter(Mandatory = $true)][double]$TimeoutSeconds,
        [Parameter(Mandatory = $true)][double]$PollIntervalSeconds
    )
    $budget = [System.Diagnostics.Stopwatch]::StartNew()
    while ($budget.Elapsed.TotalSeconds -lt $TimeoutSeconds) {
        if ((Test-GateRecordGone -Login $Login -Record $Record) -eq $true) { return $true }
        $remaining = $TimeoutSeconds - $budget.Elapsed.TotalSeconds
        if ($remaining -lt 0.001) { break }
        Start-Sleep -Milliseconds ([int][Math]::Max(1, [Math]::Min($PollIntervalSeconds * 1000, [Math]::Floor($remaining * 1000))))
    }
    return $false
}

# 清掉两个标记。只在旧进程的 etcd 记录消失、并再等过 LoginMirrorSettleSeconds 之后调用:
# 清早了,login 会把玩家分到已删 Pod 的陈旧记录上。
function Clear-GateDrainMarks {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)][uint32]$NodeId
    )
    $keys = Get-GateDrainKeys -NodeId $NodeId
    $deleted = ConvertFrom-RedisCliSingle -Text (Invoke-GateDrainRedisChecked -Login $Login -Command @('DEL', $keys.Draining, $keys.Drained) -What "清除 $($keys.Draining) / $($keys.Drained)")
    if ($deleted -isnot [long]) { throw "清除排空标记得到意外回复 '$deleted'" }
    Write-Host "[gate-drain] Removed $($keys.Draining) / $($keys.Drained)"
}

# 等新 Pod(UID 与旧的不同、未在终止、Ready)与 StatefulSet(readyReplicas >= replicas)就绪。
# gate 是 OnDelete 策略,`kubectl rollout status` 对它不可用,所以直接看 Pod 与 STS 状态。
function Wait-GateStatefulSetReady {
    param(
        [Parameter(Mandatory = $true)]$Pod,
        [Parameter(Mandatory = $true)][double]$TimeoutSeconds,
        [Parameter(Mandatory = $true)][double]$PollIntervalSeconds
    )
    $contract = Get-GateDrainContract
    $budget = [System.Diagnostics.Stopwatch]::StartNew()
    $last = '尚未查询'
    while ($true) {
        $remaining = $TimeoutSeconds - $budget.Elapsed.TotalSeconds
        if ($remaining -lt 0.001) { break }
        $callTimeout = [Math]::Min([double]$contract.CallTimeoutSeconds, $remaining)
        try {
            $podJson = Invoke-GateDrainKubectlChecked -Arguments @('get', 'pod', $Pod.Name, '-n', $Pod.Namespace, '--ignore-not-found', '-o', 'json') `
                -TimeoutSeconds $callTimeout -What "查询新 Pod $($Pod.Name)"
            $podReady = $false
            if ([string]::IsNullOrWhiteSpace($podJson)) { $last = '新 Pod 尚未创建' }
            else {
                $newPod = $podJson | ConvertFrom-Json -ErrorAction Stop
                $readyTrue = @($newPod.status.conditions | Where-Object { $null -ne $_ -and $_.type -eq 'Ready' -and $_.status -eq 'True' }).Count -gt 0
                if ([string]$newPod.metadata.uid -eq $Pod.Uid) { $last = '旧 Pod 仍在' }
                elseif ($null -ne $newPod.metadata.deletionTimestamp) { $last = '新 Pod 正在终止' }
                elseif (-not $readyTrue) { $last = "新 Pod 未就绪(phase=$($newPod.status.phase))" }
                else { $podReady = $true }
            }
            if ($podReady) {
                $remaining = $TimeoutSeconds - $budget.Elapsed.TotalSeconds
                if ($remaining -lt 0.001) { break }
                $stsJson = Invoke-GateDrainKubectlChecked -Arguments @('get', 'statefulset', $contract.GateStatefulSet, '-n', $Pod.Namespace, '-o', 'json') `
                    -TimeoutSeconds ([Math]::Min([double]$contract.CallTimeoutSeconds, $remaining)) -What "查询 StatefulSet/$($contract.GateStatefulSet)"
                $sts = $stsJson | ConvertFrom-Json -ErrorAction Stop
                $ready = [int]$sts.status.readyReplicas
                $wanted = [int]$sts.spec.replicas
                if ($ready -ge $wanted) {
                    Write-Host "[gate-drain] New Pod $($Pod.Name) is ready; StatefulSet/$($contract.GateStatefulSet) readyReplicas=$ready/$wanted"
                    return
                }
                $last = "StatefulSet readyReplicas=$ready/$wanted"
            }
        } catch {
            # 查询失败只说明状态未知,继续等到预算耗尽,不当成就绪。
            $last = "查询失败:$($_.Exception.Message)"
        }
        $remaining = $TimeoutSeconds - $budget.Elapsed.TotalSeconds
        if ($remaining -lt 0.001) { break }
        Start-Sleep -Milliseconds ([int][Math]::Max(1, [Math]::Min($PollIntervalSeconds * 1000, [Math]::Floor($remaining * 1000))))
    }
    throw "等新 Pod $($Pod.Namespace)/$($Pod.Name) 与 StatefulSet/$($contract.GateStatefulSet) 就绪超过 ${TimeoutSeconds}s(最后状态:$last)"
}

# 删 Pod → 等旧进程的 etcd 记录消失 → 再等 login 镜像跟上(LoginMirrorSettleSeconds)→ 清标记 → 等新 Pod 与 STS 就绪。
# 标记必须清:新 gate 常会复用同一 node_id,不清就在 draining 的 TTL 内分不到新玩家。
# 旧记录迟迟不消失或清除失败时不清(见 Clear-GateDrainMarks),返回 MarksCleared=$false。
# 就绪超时不在这里 throw,而是带在 ReadyError 里返回:标记没清时,主流程要先报残留标记与人工 DEL 指引
# (退出码 3 优先),不能让一个就绪超时把它盖掉。
function Invoke-GatePodReplacement {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)]$Pod,
        [Parameter(Mandatory = $true)]$Record,
        [Parameter(Mandatory = $true)][int]$ReadyTimeoutSeconds
    )
    $contract = Get-GateDrainContract
    Assert-GateUnchanged -Login $Login -Pod $Pod -Record $Record

    $deleteTimeout = $Pod.GraceSeconds + $contract.DeleteGraceMarginSeconds
    Write-Host "[gate-drain] Deleting Pod $($Pod.Namespace)/$($Pod.Name) (waiting up to ${deleteTimeout}s)"
    $null = Invoke-GateDrainKubectlChecked -Arguments @('delete', 'pod', $Pod.Name, '-n', $Pod.Namespace, '--wait=true', "--timeout=${deleteTimeout}s") `
        -TimeoutSeconds ($deleteTimeout + 15) -What "删除 Pod $($Pod.Namespace)/$($Pod.Name)"

    $marksCleared = $false
    if (Wait-GateRecordGone -Login $Login -Record $Record -TimeoutSeconds $contract.StaleRecordWaitSeconds -PollIntervalSeconds $contract.PollIntervalSeconds) {
        # etcd 里没了不等于每个 login 的镜像都没了,见契约 LoginMirrorSettleSeconds。
        Write-Host "[gate-drain] Old record gone; waiting $($contract.LoginMirrorSettleSeconds)s for login gate-directory mirrors before clearing drain marks"
        Start-Sleep -Seconds $contract.LoginMirrorSettleSeconds
        try {
            Clear-GateDrainMarks -Login $Login -NodeId $Record.NodeId
            $marksCleared = $true
        } catch {
            Write-Warning "Failed to clear drain marks: $($_.Exception.Message)"
        }
    } else {
        Write-Warning ("Old gate (node_id={0}) etcd record did not disappear within {1}s; preserving drain marks. Clearing them early would let login assign players to the deleted Pod's stale record" -f `
                $Record.NodeId, $contract.StaleRecordWaitSeconds)
    }

    $readyError = $null
    try {
        Wait-GateStatefulSetReady -Pod $Pod -TimeoutSeconds $ReadyTimeoutSeconds -PollIntervalSeconds $contract.PollIntervalSeconds
    } catch {
        $readyError = $_.Exception.Message
    }
    return [pscustomobject]@{ MarksCleared = $marksCleared; ReadyError = $readyError }
}

# 残留排空标记的人工清理指引。node_id 全局唯一、按最小空闲号复用,login 的 Redis 跨 zone 共用:
# 不清的话,之后拿到这个 node_id 的 gate(可能在任意 zone)在 TTL 内分不到新玩家。
function Write-GateLeftoverMarkGuidance {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)][uint32]$NodeId,
        [Parameter(Mandatory = $true)][string]$Situation
    )
    $keys = Get-GateDrainKeys -NodeId $NodeId
    # -f 的优先级高于 +,拼接必须先括起来,否则只有后半句被格式化。
    Write-Warning (("{0}: drain marks {1} / {2} remain; any gate that later receives node_id={3} (in any zone) cannot receive new players until the TTL expires." +
            "After confirming the old node_id={3} process has disappeared from etcd, manually DEL {1} {2} in login Redis ({4} DB {5})") -f `
            $Situation, $keys.Draining, $keys.Drained, $NodeId, $Login.Redis.Endpoint, $Login.RedisDb)
}

# 排空中止时处理本次的标记:只撤回本次写入的(Remove-OwnGateDrainMark 值比对),沿用来的不动。
# 返回 $true = 无残留;$false = 有残留,已打印人工清理指引。
function Undo-GateDrainMark {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)][uint32]$NodeId,
        [Parameter(Mandatory = $true)]$Mark
    )
    if ($Mark.Created) {
        try {
            if (Remove-OwnGateDrainMark -Login $Login -NodeId $NodeId -MarkValue $Mark.Value) {
                Write-Host "[gate-drain] Reverted gate:${NodeId}:draining written by this run"
                return $true
            }
            Write-Warning "gate:${NodeId}:draining no longer equals $($Mark.Value) written by this run (changed by another run or operator); skipping deletion"
        } catch {
            Write-Warning "Failed to revert gate:${NodeId}:draining written by this run: $($_.Exception.Message)"
        }
    } else {
        Write-Warning "gate:${NodeId}:draining is a reused mark from an earlier run; skipping deletion"
    }
    Write-GateLeftoverMarkGuidance -Login $Login -NodeId $NodeId -Situation 'Drain aborted'
    return $false
}

# -DryRun:只读定位已经做完,这里只把后续写操作打印出来。
function Write-GateDrainDryRunPlan {
    param(
        [Parameter(Mandatory = $true)]$Login,
        [Parameter(Mandatory = $true)]$Pod,
        [Parameter(Mandatory = $true)]$Record,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][uint32[]]$PeerNodeIds,
        [Parameter(Mandatory = $true)][int]$DrainTtlSeconds,
        [Parameter(Mandatory = $true)][int]$WaitTimeoutSeconds,
        [switch]$WillDeletePod
    )
    $contract = Get-GateDrainContract
    $keys = Get-GateDrainKeys -NodeId $Record.NodeId
    $peerKeys = @($PeerNodeIds | ForEach-Object { (Get-GateDrainKeys -NodeId $_).Draining }) -join ' '
    Write-Host "[dry-run] The following Redis operations will not execute (Redis $($Login.Redis.Endpoint) DB $($Login.RedisDb), via $(Get-GateDrainRedisExecTarget -Login $Login) -c redis exec):"
    Write-Host "[dry-run]   TIME; EVAL (atomic): DEL $($keys.Drained) only if at least one key in $peerKeys is absent,"
    Write-Host "[dry-run]     SET $($keys.Draining) <Redis TIME seconds> NX EX $DrainTtlSeconds; otherwise write nothing and reject"
    Write-Host "[dry-run]   Recheck Pod uid=$($Pod.Uid) and etcd process node_id/uuid=$($Record.NodeId)/$($Record.Uuid); revert this run's mark if either changed"
    Write-Host "[dry-run]   MGET $($keys.Draining) $($keys.Drained) every $($contract.PollIntervalSeconds)s, up to ${WaitTimeoutSeconds}s; recheck the etcd process every $($contract.IdentityRecheckEveryRounds) rounds"
    if ($WillDeletePod) {
        Write-Host "[dry-run]   Recheck Pod uid=$($Pod.Uid) and etcd process uuid=$($Record.Uuid), then kubectl delete pod $($Pod.Name) -n $($Pod.Namespace)"
        Write-Host "[dry-run]   Wait for the old process's etcd record to disappear (up to $($contract.StaleRecordWaitSeconds)s), then DEL $($keys.Draining) $($keys.Drained)"
        Write-Host "[dry-run]   Wait for the new Pod and StatefulSet/$($contract.GateStatefulSet) to become ready"
    } else {
        Write-Host "[dry-run]   (no -DeletePod: stop when drained appears)"
    }
}

# ─── 主流程 ───

# 整个排空流程,返回退出码(0 / Get-GateDrainContract.ExitCodeLeftoverMarks);出错中止一律 throw(退出码 1)。
# 参数与脚本 param 同义。-KubeContext / -KubeConfig 不经这里:它们是连接设置,由 Build-KubectlBaseArgs
# 直接读脚本作用域。收成函数是为了让测试用同一套内存模拟驱动完整顺序(预算校验与 -DryRun 在任何写之前)。
function Invoke-GateDrain {
    param(
        [Parameter(Mandatory = $true)][string]$ZoneName,
        [Parameter(Mandatory = $true)][int]$Ordinal,
        [Parameter(Mandatory = $true)][int]$DrainTtlSeconds,
        [Parameter(Mandatory = $true)][int]$WaitTimeoutSeconds,
        [Parameter(Mandatory = $true)][int]$ReadyTimeoutSeconds,
        [Parameter(Mandatory = $true)][string]$NamespacePrefix,
        [switch]$DeletePod,
        [switch]$DryRun
    )
    $contract = Get-GateDrainContract
    $zoneNamespace = "$NamespacePrefix-$ZoneName"
    $contextText = if ([string]::IsNullOrWhiteSpace($KubeContext)) { '<kubeconfig current-context>' } else { $KubeContext }
    Write-Host ("[gate-drain] zone={0} namespace={1} pod={2}-{3} context={4} dryRun={5} deletePod={6}" -f `
            $ZoneName, $zoneNamespace, $contract.GateStatefulSet, $Ordinal, $contextText, [bool]$DryRun, [bool]$DeletePod)

    # 1-2. login 配置 + 入口校验:都在任何写操作之前。
    $login = Get-LoginDrainContext -ZoneNamespace $zoneNamespace
    Write-Host ("[gate-drain] login({0}):zone_id={1} redis={2} DB {3} etcd={4} RequireClientEndpoint={5} GateDrain.Interval={6}s Deadline={7}s DrainedBelowPlayers={8}" -f `
            $login.Source, $login.ZoneId, $login.Redis.Endpoint, $login.RedisDb, $login.Etcd.Endpoint, $login.RequireClientEndpoint,
        $login.MonitorIntervalSeconds, $login.DeadlineSeconds, $login.DrainedBelowPlayers)
    Assert-DrainBudget -DrainTtlSeconds $DrainTtlSeconds -WaitTimeoutSeconds $WaitTimeoutSeconds `
        -DeadlineSeconds $login.DeadlineSeconds -MonitorIntervalSeconds $login.MonitorIntervalSeconds

    # 3. Pod → podIP → etcd 里唯一的 NodeInfo → node_id;本 zone 还得有别的可分配 gate。
    $pod = Get-GateStatefulSetPod -Namespace $zoneNamespace -Ordinal $Ordinal
    $prefix = Get-GateEtcdPrefix -ZoneId $login.ZoneId
    $records = Get-GateEtcdRecords -Login $login
    $record = Select-GateNodeRecord -Records $records -PodIp $pod.PodIp -Prefix $prefix
    $peers = Select-GateDrainPeers -Records $records -Target $record -Login $login -Prefix $prefix
    $peerIds = [uint32[]]@($peers | ForEach-Object { $_.NodeId })
    Write-Host ("[gate-drain] {0} podIP={1} uid={2} -> node_id={3} uuid={4} online(etcd-reported)={5}; other candidate node_id values in this zone={6}" -f `
            $pod.Name, $pod.PodIp, $pod.Uid, $record.NodeId, $record.Uuid, $record.PlayerCount, ($peerIds -join ','))
    Write-Host (Format-GateDrainMigrationLine -Login $login -Online ([long]$record.PlayerCount))
    # 其余 gate 装不装得下这台的人:只读 etcd 的估算,在任何写之前打出来(-DryRun 也打),装不下时是一条 WARNING。
    $capacity = Format-GateDrainCapacityLine -Capacity (Get-GateDrainPeerHeadroom -Peers $peers) -Online ([long]$record.PlayerCount)
    if ($capacity.Short) { Write-Warning $capacity.Text } else { Write-Host $capacity.Text }

    if ($DryRun) {
        Write-GateDrainDryRunPlan -Login $login -Pod $pod -Record $record -PeerNodeIds $peerIds -DrainTtlSeconds $DrainTtlSeconds `
            -WaitTimeoutSeconds $WaitTimeoutSeconds -WillDeletePod:$DeletePod
        return 0
    }

    # 4. 标记 draining(与「其它候选未全在排空」原子判定),写完立即复核进程身份。
    $mark = Set-GateDrainingMark -Login $login -NodeId $record.NodeId -PeerNodeIds $peerIds -TtlSeconds $DrainTtlSeconds
    try {
        Assert-GateUnchanged -Login $login -Pod $pod -Record $record
    } catch {
        $reason = "标记后复核不通过:$($_.Exception.Message)"
        if (Undo-GateDrainMark -Login $login -NodeId $record.NodeId -Mark $mark) { throw $reason }
        Write-Warning $reason
        return $contract.ExitCodeLeftoverMarks
    }

    # 5. 等 drained;期间被排空的进程没了就撤回标记并中止。
    $drained = Wait-GateDrained -Login $login -NodeId $record.NodeId -Record $record -MarkValue $mark.Value `
        -TimeoutSeconds $WaitTimeoutSeconds -PollIntervalSeconds $contract.PollIntervalSeconds
    if ($drained.IdentityLost) {
        $reason = ("被排空的 gate 进程(node_id={0} uuid={1})已从 etcd 消失:Pod 被替换或进程重启过,标记已不代表这台 gate," +
            "login 也不会再为它写 drained。中止(不删 Pod);确认新进程注册后重新运行本脚本") -f $record.NodeId, $record.Uuid
        if (Undo-GateDrainMark -Login $login -NodeId $record.NodeId -Mark $mark) { throw $reason }
        Write-Warning $reason
        return $contract.ExitCodeLeftoverMarks
    }
    if ($drained.Reason -eq $contract.DrainedReasonDeadline) {
        Write-GateDeadlineWarning -Login $login -Pod $pod -Record $record
    } else {
        Write-Host ("[gate-drain] gate:{0}:drained={1} (waited {2:N0}s): online players are below the threshold; the Pod can be deleted safely" -f `
                $record.NodeId, $drained.Reason, $drained.WaitedSeconds)
    }

    if (-not $DeletePod) {
        Write-Host ("[gate-drain] No -DeletePod; stopping here. The draining mark remains until its TTL expires; rerun with the same arguments plus -DeletePod after confirmation" +
            "(reuses the existing mark without resetting the deadline). The script deletes the Pod, clears marks, and waits for replacement. Do not scale down the StatefulSet instead: this script does not clear marks after scale-down," +
            "and leftover marks prevent gates that later reuse this node_id from receiving players until the TTL expires (see the operations guide).")
        return 0
    }

    # 6. 删 Pod、清标记、等重建。标记没清时先报残留与人工指引(退出码 3 优先),再报就绪超时。
    $replacement = Invoke-GatePodReplacement -Login $login -Pod $pod -Record $record -ReadyTimeoutSeconds $ReadyTimeoutSeconds
    if (-not $replacement.MarksCleared) {
        Write-GateLeftoverMarkGuidance -Login $login -NodeId $record.NodeId -Situation "Pod $($pod.Name) deleted, but drain marks remain"
        if ($null -ne $replacement.ReadyError) { Write-Warning "The new Pod is also not ready: $($replacement.ReadyError)" }
        return $contract.ExitCodeLeftoverMarks
    }
    if ($null -ne $replacement.ReadyError) { throw "排空标记已清除,但$($replacement.ReadyError)" }
    Write-Host "[gate-drain] Complete: $($pod.Name) was replaced and is ready; drain marks cleared"
    return 0
}

# ─── 入口 ───

if ($null -eq (Get-Command kubectl -CommandType Application -ErrorAction SilentlyContinue)) {
    throw "找不到 kubectl。请先安装并配置集群访问"
}
$exitCode = Invoke-GateDrain -ZoneName $ZoneName -Ordinal $Ordinal -DrainTtlSeconds $DrainTtlSeconds -WaitTimeoutSeconds $WaitTimeoutSeconds `
    -ReadyTimeoutSeconds $ReadyTimeoutSeconds -NamespacePrefix $NamespacePrefix -DeletePod:$DeletePod -DryRun:$DryRun
exit ([int]$exitCode)
