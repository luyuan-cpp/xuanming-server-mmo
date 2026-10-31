#requires -Version 7
<#
.SYNOPSIS
    玩家域 Redis Cluster 的容量基准:逐档加压,按 INFO 求差出每个主节点的 ops 与主线程 CPU,判定是否真的压到了饱和。

.DESCRIPTION
    口径见 docs/design/redis-player-store-cluster.md §14.3 第 1、2 项。容量结论只认这样的证据:每个主的主线程 CPU 接近 100%,总 ops 进入平台期,
    错误率约等于 0。副本数、代理数、连接数都不是证据。没压到饱和时本脚本判 NOT SATURATED、退出码 2,这一轮的数字只说明压测端不够大。

    负载是 redis-benchmark --cluster 跑一段 Lua:写 location、写会话 hash、铸一次代次。键带 {tag},redis-benchmark 把它换成当前连接所在主节点的 slot,
    所以三个键同槽,客户端均摊到每个主节点。键名前缀 capbench:,全部带 600 秒 TTL。
    这段 Lua 只是玩家域写路径的近似,不是线上流量的回放。端到端结论走 AGENTS.md §6 的压测流程(§14.3 第 5 项),本脚本不替代它。

    读数全部来自服务端 INFO 求差(total_commands_processed、cmdstat_eval、used_cpu_*_main_thread、total_error_replies),不读 redis-benchmark 的输出:
    它被 timeout 杀掉时没有汇总行,而且压测端看到的数字说明不了服务端有没有饱和。
    预热段不计入:连接爬升期的 ops 没有意义。

    压测端与主节点抢 CPU 时结论不成立。K8s 上压测端是单独的 Pod,与 redis-player 反亲和(preferred),落在同一台机器时告警并记进结果;
    本机 docker 必然同机,只能用来验证脚本本身和做相对比较。

    写入保护:K8s 的 namespace 必须匹配 ^mmorpg-stress-(同 k8s_deploy.ps1 的 redis-wipe)。

    退出码:0 = 已饱和,结果可作容量证据;2 = 没饱和,不能作证据;1 = 失败(拓扑中途变化、节点重启、压测端提前退出等)。

    结果落在 -OutDir:capacity.txt(与控制台相同的表)、capacity.json、每档每节点的 INFO 原文。

.EXAMPLE
    # 本机开发集群(deploy/compose.redis-cluster.yml),只验证脚本
    pwsh -File tools/scripts/redis_cluster_capacity.ps1 -Target docker-cluster:redis-player-cluster-0:7100 -ClientSteps 50,200 -StageSeconds 30

.EXAMPLE
    # K8s 压测环境
    pwsh -File tools/scripts/redis_cluster_capacity.ps1 -Target k8s-cluster:mmorpg-stress-a/redis-player -ClientSteps 100,200,400,800,1600 -StageSeconds 120 -KubeContext my-cluster

.EXAMPLE
    # 只看将要执行什么,不连任何东西
    pwsh -File tools/scripts/redis_cluster_capacity.ps1 -Target k8s-cluster:mmorpg-stress-a/redis-player -DryRun
#>
param(
    # docker-cluster:<container>:<port> 或 k8s-cluster:<namespace>/<statefulset>,与 stress_snap.ps1 的 -RedisTargets 同形。
    [Parameter(Mandatory = $true)][string]$Target,
    # 逐档的总连接数(整个集群,redis-benchmark 均摊到各主节点),逗号分隔,同 stress_snap.ps1 的 -Stages。必须递增,至少两档:一档看不出平台期。
    [string]$ClientSteps = '100,200,400,800',
    [ValidateRange(10, 3600)][int]$StageSeconds = 60,
    [ValidateRange(0, 600)][int]$WarmupSeconds = 15,
    [ValidateRange(1, 64)][int]$Threads = 4,
    [ValidateRange(1, 1000)][int]$Pipeline = 1,
    [ValidateRange(1000, 100000000)][int]$KeySpace = 100000,
    # 判饱和的阈值:每个主的主线程 CPU 不低于它,且最后一档总 ops 比上一档增长低于 -PlateauGrowthPercent。
    [ValidateRange(1, 100)][double]$SaturatedCpuPercent = 90,
    [ValidateRange(0.1, 100)][double]$PlateauGrowthPercent = 5,
    # 压测端镜像,缺省与集群同一个(deploy/k8s/manifests/infra/redis-player-cluster.yaml)。
    [string]$Image = 'redis:8.10.2@sha256:6f81e8915c60b065a524e6967e0ad1c639ba6efa84d669f823683ea04d9150ee',
    [string]$OutDir = '',
    [string]$DockerContext = 'desktop-linux',
    [string]$KubeContext = '',
    # 口令所在的 Secret(同 namespace),压测端 Pod 经它拿 REDIS_PASSWORD。没开认证时 optional 不影响。
    [string]$AuthSecret = 'redis-player-auth',
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

# ----- 纯函数(tests/redis_player_cluster_contract.tests.ps1 按 AST 抽出来测) -----------------------------

function ConvertFrom-CapacityTarget {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Spec)
    $dns = '[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?'
    if ($Spec -cmatch '^docker-cluster:(?<container>[A-Za-z0-9][A-Za-z0-9_.-]*):(?<port>\d{1,5})$') {
        $port = [int]$Matches['port']
        if ($port -lt 1 -or $port -gt 65535) { throw "Target '$Spec': port $port out of range (1..65535)" }
        return [pscustomobject]@{ Kind = 'docker'; Container = $Matches['container']; Port = $port }
    }
    if ($Spec -cmatch "^k8s-cluster:(?<ns>$dns)/(?<sts>$dns)$") {
        # 先取出来:下面的 -cnotmatch 命中时会改写 $Matches。
        $namespace = $Matches['ns']
        $statefulSet = $Matches['sts']
        # 这个脚本往集群里写键、把主线程压满,只许进压测环境。规则同 k8s_deploy.ps1 的 redis-wipe。
        if ($namespace -cnotmatch '^mmorpg-stress-') {
            throw "Target '$Spec': namespace must match ^mmorpg-stress- (this tool writes keys and saturates the masters)"
        }
        return [pscustomobject]@{ Kind = 'k8s'; Namespace = $namespace; StatefulSet = $statefulSet; Port = 6379 }
    }
    throw "Target '$Spec' is invalid: expected docker-cluster:<container>:<port> or k8s-cluster:<namespace>/<statefulset>"
}

function ConvertFrom-CapacitySteps {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    $steps = @(foreach ($token in ($Text -split ',')) {
            $value = 0
            if (-not [int]::TryParse($token.Trim(), [ref]$value) -or $value -lt 1) { throw "-ClientSteps value '$token' must be a positive integer" }
            $value
        })
    if ($steps.Count -lt 2) { throw '-ClientSteps needs at least two steps: one step cannot show a plateau' }
    for ($i = 1; $i -lt $steps.Count; $i++) {
        if ($steps[$i] -le $steps[$i - 1]) { throw '-ClientSteps must be strictly increasing' }
    }
    return , $steps
}

# CLUSTER NODES 文本 → 持有 slot 的主节点。形状不对或一个都没有即 throw:不把「没读到」当成「没有节点」。
function ConvertFrom-ClusterNodesText {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    $masters = @(
        foreach ($line in ($Text -split "`r?`n")) {
            $fields = @($line -split '\s+' | Where-Object { $_ -ne '' })
            if ($fields.Count -lt 9) { continue }
            if (@($fields[2] -split ',') -cnotcontains 'master') { continue }
            if ($fields[0] -cnotmatch '^[0-9a-f]{40}$' -or $fields[1] -cnotmatch '^(?<host>[0-9A-Fa-f.:]+):(?<port>\d{1,5})@') { continue }
            $endpointHost = $Matches['host']
            $endpointPort = [int]$Matches['port']
            $slots = 0
            foreach ($token in $fields[8..($fields.Count - 1)]) {
                if ($token -cmatch '^(?<low>\d+)-(?<high>\d+)$') { $slots += [int]$Matches['high'] - [int]$Matches['low'] + 1 }
                elseif ($token -cmatch '^\d+$') { $slots++ }
            }
            [pscustomobject]@{ NodeId = $fields[0]; Host = $endpointHost; Port = $endpointPort; Slots = $slots }
        }
    )
    if ($masters.Count -eq 0) { throw "CLUSTER NODES has no master holding slots (text length $($Text.Length))" }
    $total = ($masters | Measure-Object -Property Slots -Sum).Sum
    if ($total -ne 16384) { throw "CLUSTER NODES masters cover $total slots, expected 16384: fix the cluster before measuring capacity" }
    return , $masters
}

# 拓扑指纹:节点 ID 与它的 slot 数。一档之内变了,求差就没有意义。
function Get-CapacityTopologyFingerprint {
    param([Parameter(Mandatory = $true)][object[]]$Masters)
    return (@($Masters | Sort-Object NodeId | ForEach-Object { "$($_.NodeId)=$($_.Slots)" }) -join ';')
}

# INFO 文本 → 字段表。cmdstat_* 一行展开成 cmdstat_eval.calls 这样的键。
function ConvertFrom-RedisInfoText {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    if ($Text -cmatch '(?m)^\s*(\(error\)\s*)?(ERR|NOAUTH|WRONGPASS|NOPERM|LOADING|MASTERDOWN)\b') {
        throw "Redis returned an error: $(($Text.Trim() -split "`r?`n")[0])"
    }
    $fields = @{}
    foreach ($line in ($Text -split "`r?`n")) {
        if ($line -cnotmatch '^(?<k>[a-z][a-zA-Z0-9_|.]*):(?<v>.*)$') { continue }
        $key = $Matches['k']
        $value = $Matches['v']
        $fields[$key] = $value
        if ($key -clike 'cmdstat_*') {
            foreach ($pair in ($value -split ',')) {
                $parts = $pair -split '=', 2
                if ($parts.Count -eq 2) { $fields["$key.$($parts[0])"] = $parts[1] }
            }
        }
    }
    foreach ($required in @('run_id', 'total_commands_processed', 'used_cpu_user_main_thread', 'used_cpu_sys_main_thread')) {
        if (-not $fields.ContainsKey($required)) { throw "INFO has no $required (text length $($Text.Length)): this tool needs server, stats and cpu sections from Redis 6.2+" }
    }
    return $fields
}

# 一个主节点在一档里的读数。run_id 变了说明节点重启过,计数器归零,这一档作废(throw)。
function Get-CapacityMasterRow {
    param(
        [Parameter(Mandatory = $true)]$Master,
        [Parameter(Mandatory = $true)][hashtable]$Before,
        [Parameter(Mandatory = $true)][hashtable]$After,
        [Parameter(Mandatory = $true)][double]$Seconds
    )
    if ($Seconds -le 0) { throw "non-positive window $Seconds for node $($Master.NodeId)" }
    if ($Before['run_id'] -cne $After['run_id']) { throw "node $($Master.NodeId) restarted during the stage (run_id changed)" }
    $delta = {
        param([string]$Key)
        $a = 0.0
        $b = 0.0
        [void][double]::TryParse([string]$Before[$Key], [System.Globalization.NumberStyles]::Float, [System.Globalization.CultureInfo]::InvariantCulture, [ref]$a)
        [void][double]::TryParse([string]$After[$Key], [System.Globalization.NumberStyles]::Float, [System.Globalization.CultureInfo]::InvariantCulture, [ref]$b)
        return ($b - $a)
    }
    $commands = & $delta 'total_commands_processed'
    $cpu = (& $delta 'used_cpu_user_main_thread') + (& $delta 'used_cpu_sys_main_thread')
    $memory = 0.0
    [void][double]::TryParse([string]$After['used_memory'], [ref]$memory)
    $clients = 0
    [void][int]::TryParse([string]$After['connected_clients'], [ref]$clients)
    return [pscustomobject]@{
        NodeId         = $Master.NodeId
        Slots          = $Master.Slots
        OpsPerSec      = [math]::Round($commands / $Seconds, 1)
        EvalPerSec     = [math]::Round((& $delta 'cmdstat_eval.calls') / $Seconds, 1)
        MainCpuPercent = [math]::Round($cpu / $Seconds * 100, 1)
        UsedMemoryMb   = [math]::Round($memory / 1MB, 1)
        Clients        = $clients
        ErrorReplies   = [long](& $delta 'total_error_replies')
        Commands       = [long]$commands
    }
}

<#
.SYNOPSIS
    判定是否压到饱和(纯函数)。-Stages 每项 { Clients; Masters = 行的数组 },按加压顺序。

.DESCRIPTION
    三条都成立才算:最后一档每个主的主线程 CPU 不低于阈值;最后一档总 ops 比上一档增长低于阈值(平台期);错误回复不超过命令数的千分之一
    (OOM 写满、大量 MOVED 也会让 ops 停涨,那不是 CPU 饱和)。主线程没满但 ops 不涨,说明瓶颈在压测端或网络,不是 Redis。
#>
function Get-CapacityVerdict {
    param(
        [Parameter(Mandatory = $true)][object[]]$Stages,
        [Parameter(Mandatory = $true)][double]$CpuThresholdPercent,
        [Parameter(Mandatory = $true)][double]$PlateauGrowthPercent
    )
    $reasons = [System.Collections.Generic.List[string]]::new()
    $last = $Stages[$Stages.Count - 1]
    $lastOps = ($last.Masters | Measure-Object -Property OpsPerSec -Sum).Sum
    $minCpu = ($last.Masters | Measure-Object -Property MainCpuPercent -Minimum).Minimum
    $growth = $null
    if ($Stages.Count -lt 2) {
        $reasons.Add('only one stage: a plateau needs at least two')
    }
    else {
        $previousOps = ($Stages[$Stages.Count - 2].Masters | Measure-Object -Property OpsPerSec -Sum).Sum
        if ($previousOps -le 0) {
            $reasons.Add('previous stage has zero ops')
        }
        else {
            $growth = [math]::Round(($lastOps - $previousOps) / $previousOps * 100, 1)
            if ($growth -ge $PlateauGrowthPercent) { $reasons.Add("total ops still growing: +$growth% at the last step (need < $PlateauGrowthPercent%); add a higher -ClientSteps value") }
        }
    }
    if ($minCpu -lt $CpuThresholdPercent) {
        $cold = @($last.Masters | Where-Object { $_.MainCpuPercent -lt $CpuThresholdPercent } | ForEach-Object { '{0}={1}%' -f $_.NodeId.Substring(0, 8), $_.MainCpuPercent })
        $reasons.Add("main thread not saturated on $($cold -join ', ') (need >= $CpuThresholdPercent%); the load generator or the network is the bottleneck, or the slots are unevenly spread")
    }
    $commands = ($last.Masters | Measure-Object -Property Commands -Sum).Sum
    $errors = ($last.Masters | Measure-Object -Property ErrorReplies -Sum).Sum
    if ($errors -gt 0 -and ($commands -le 0 -or ($errors / $commands) -gt 0.001)) {
        $reasons.Add("$errors error replies in the last stage (over 0.1% of commands): OOM or redirects stop ops from growing too, so this is not CPU saturation")
    }
    $perMaster = @($last.Masters | ForEach-Object { $_.OpsPerSec })
    return [pscustomobject]@{
        Saturated           = ($reasons.Count -eq 0)
        Reasons             = @($reasons)
        TotalOpsPerSec      = [math]::Round($lastOps, 1)
        LastStepGrowthPct   = $growth
        MinMainCpuPercent   = $minCpu
        MinOpsPerMaster     = ($perMaster | Measure-Object -Minimum).Minimum
        MaxOpsPerMaster     = ($perMaster | Measure-Object -Maximum).Maximum
        Masters             = @($last.Masters).Count
    }
}

# redis-benchmark 的参数(不含口令,口令由外层 sh 从环境变量加)。Lua 单行:写 location、写会话 hash、铸代次,全部带 TTL,跑完不用清。
function New-CapacityBenchmarkArguments {
    param(
        [Parameter(Mandatory = $true)][string]$SeedHost,
        [Parameter(Mandatory = $true)][int]$SeedPort,
        [Parameter(Mandatory = $true)][int]$Clients,
        [Parameter(Mandatory = $true)][int]$Threads,
        [Parameter(Mandatory = $true)][int]$Pipeline,
        [Parameter(Mandatory = $true)][int]$KeySpace
    )
    $lua = "redis.call('SET',KEYS[1],ARGV[1],'EX',600) redis.call('HSET',KEYS[2],'node',ARGV[1],'gate',ARGV[1]) redis.call('EXPIRE',KEYS[2],600) local e=redis.call('INCR',KEYS[3]) redis.call('EXPIRE',KEYS[3],600) return e"
    # -n 取接近 int32 上限:结束时间由外层 timeout 决定,不靠请求数。
    return @(
        '--cluster', '-h', $SeedHost, '-p', [string]$SeedPort, '-c', [string]$Clients, '--threads', [string]$Threads, '-P', [string]$Pipeline,
        '-r', [string]$KeySpace, '-n', '2000000000', '-q',
        'EVAL', $lua, '3', 'capbench:{tag}:__rand_int__:location', 'capbench:{tag}:__rand_int__:session', 'capbench:{tag}:__rand_int__:epoch',
        '0123456789abcdef0123456789abcdef'
    )
}

# ----- 外部进程 ---------------------------------------------------------------------------------------------

# 同 stress_snap.ps1 的 Invoke-BoundedProcess(那边是入口脚本内的函数,无法 dot-source)。
function Invoke-CapacityProcess {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string[]]$ArgumentList,
        [Parameter(Mandatory = $true)][int]$TimeoutSec
    )
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
        if (-not $process.WaitForExit($TimeoutSec * 1000)) {
            try { $process.Kill($true) } catch { }
            $null = $process.WaitForExit(1000)
            throw "$FilePath did not finish within ${TimeoutSec}s"
        }
        $null = [System.Threading.Tasks.Task]::WaitAll([System.Threading.Tasks.Task[]]@($stdout, $stderr), 1000)
        return [pscustomobject]@{ ExitCode = $process.ExitCode; Output = [string]$stdout.Result; ErrorOutput = [string]$stderr.Result }
    }
    finally {
        $process.Dispose()
    }
}

function Get-CapacityKubectlBase {
    if ([string]::IsNullOrEmpty($KubeContext)) { return @() }
    return @('--context', $KubeContext)
}

function Get-CapacityDockerBase {
    if ([string]::IsNullOrEmpty($DockerContext)) { return @() }
    return @('--context', $DockerContext)
}

# 跑一条 redis-cli,取 stdout。K8s:exec 进 statefulset/<sts> 的 -c redis,口令在 Pod 内由 REDIS_PASSWORD 转成 REDISCLI_AUTH,host / port 走 sh 的位置参数。
function Invoke-CapacityRedisCli {
    param(
        [Parameter(Mandatory = $true)]$TargetSpec,
        [Parameter(Mandatory = $true)][string]$NodeHost,
        [Parameter(Mandatory = $true)][int]$NodePort,
        [Parameter(Mandatory = $true)][string[]]$Command
    )
    if ($TargetSpec.Kind -eq 'docker') {
        $file = 'docker'
        $argv = @(Get-CapacityDockerBase) + @('exec', $TargetSpec.Container, 'redis-cli', '-h', $NodeHost, '-p', [string]$NodePort) + $Command
    }
    else {
        $file = 'kubectl'
        $shell = 'h=$1; p=$2; shift 2; if [ -n "$REDIS_PASSWORD" ]; then REDISCLI_AUTH=$REDIS_PASSWORD; export REDISCLI_AUTH; fi; exec redis-cli -h "$h" -p "$p" "$@"'
        $argv = @(Get-CapacityKubectlBase) + @('--request-timeout=30s', 'exec', '-n', $TargetSpec.Namespace, "statefulset/$($TargetSpec.StatefulSet)", '-c', 'redis', '--', 'sh', '-c', $shell, 'sh', $NodeHost, [string]$NodePort) + $Command
    }
    $result = Invoke-CapacityProcess -FilePath $file -ArgumentList $argv -TimeoutSec 40
    if ($result.ExitCode -ne 0) { throw "redis-cli $($Command -join ' ') on ${NodeHost}:$NodePort failed (exit $($result.ExitCode)): $($result.ErrorOutput.Trim())" }
    return $result.Output
}

function Get-CapacityMasters {
    param([Parameter(Mandatory = $true)]$TargetSpec)
    return ConvertFrom-ClusterNodesText -Text (Invoke-CapacityRedisCli -TargetSpec $TargetSpec -NodeHost '127.0.0.1' -NodePort $TargetSpec.Port -Command @('CLUSTER', 'NODES'))
}

# 读全部主节点的 INFO。每个节点各记各的时刻,求差用自己的时间窗:逐个读有先后。
function Read-CapacitySnapshot {
    param(
        [Parameter(Mandatory = $true)]$TargetSpec,
        [Parameter(Mandatory = $true)][object[]]$Masters,
        [Parameter(Mandatory = $true)][System.Diagnostics.Stopwatch]$Clock,
        [Parameter(Mandatory = $true)][string]$RawPrefix
    )
    $snapshot = @{}
    foreach ($master in $Masters) {
        $at = $Clock.Elapsed.TotalSeconds
        $text = Invoke-CapacityRedisCli -TargetSpec $TargetSpec -NodeHost $master.Host -NodePort $master.Port -Command @('INFO', 'server', 'stats', 'cpu', 'clients', 'memory', 'commandstats')
        [System.IO.File]::WriteAllText("${RawPrefix}_$($master.NodeId.Substring(0, 8)).txt", $text)
        $snapshot[$master.NodeId] = @{ At = $at; Fields = (ConvertFrom-RedisInfoText -Text $text) }
    }
    return $snapshot
}

# 压测端跑在 sh 里:timeout 决定结束时间(被杀是正常结局,退出码 124),有口令时在容器内部加 -a。
$script:BenchmarkShell = 'seconds=$1; shift; if [ -n "$REDIS_PASSWORD" ]; then set -- -a "$REDIS_PASSWORD" "$@"; fi; exec timeout -s TERM "$seconds" redis-benchmark "$@"'
$script:BenchPodName = 'redis-capacity-bench'

function Get-CapacityBenchmarkCommand {
    param(
        [Parameter(Mandatory = $true)]$TargetSpec,
        [Parameter(Mandatory = $true)][int]$Seconds,
        [Parameter(Mandatory = $true)][string[]]$BenchmarkArguments
    )
    if ($TargetSpec.Kind -eq 'docker') {
        # 共用节点的网络命名空间:开发集群宣告的 127.0.0.1:710N 只在这里面通。
        return [pscustomobject]@{
            File = 'docker'
            Args = @(Get-CapacityDockerBase) + @('run', '--rm', '--network', "container:$($TargetSpec.Container)", '--entrypoint', 'sh', $Image, '-c', $script:BenchmarkShell, 'sh', [string]$Seconds) + $BenchmarkArguments
        }
    }
    return [pscustomobject]@{
        File = 'kubectl'
        Args = @(Get-CapacityKubectlBase) + @('exec', '-n', $TargetSpec.Namespace, "pod/$($script:BenchPodName)", '--', 'sh', '-c', $script:BenchmarkShell, 'sh', [string]$Seconds) + $BenchmarkArguments
    }
}

# 起压测端,不等它结束。输出读进异步任务,管道不会写满。
function Start-CapacityBenchmark {
    param([Parameter(Mandatory = $true)]$Command)
    $process = [System.Diagnostics.Process]::new()
    $process.StartInfo.FileName = $Command.File
    $process.StartInfo.UseShellExecute = $false
    $process.StartInfo.CreateNoWindow = $true
    $process.StartInfo.RedirectStandardOutput = $true
    $process.StartInfo.RedirectStandardError = $true
    foreach ($arg in $Command.Args) { $process.StartInfo.ArgumentList.Add([string]$arg) }
    $null = $process.Start()
    return [pscustomobject]@{ Process = $process; Stdout = $process.StandardOutput.ReadToEndAsync(); Stderr = $process.StandardError.ReadToEndAsync() }
}

# 等压测端结束。timeout 杀掉 = 124(kubectl exec 原样转发,docker run 同理)= 全程在压;其它退出码说明它提前挂了,这一档作废。
function Wait-CapacityBenchmark {
    param([Parameter(Mandatory = $true)]$Bench, [Parameter(Mandatory = $true)][int]$TimeoutSec)
    try {
        if (-not $Bench.Process.WaitForExit($TimeoutSec * 1000)) {
            try { $Bench.Process.Kill($true) } catch { }
            throw "load generator did not stop within ${TimeoutSec}s"
        }
        $code = $Bench.Process.ExitCode
        if ($code -ne 124) {
            $null = [System.Threading.Tasks.Task]::WaitAll([System.Threading.Tasks.Task[]]@($Bench.Stdout, $Bench.Stderr), 1000)
            $tail = (([string]$Bench.Stdout.Result) + ([string]$Bench.Stderr.Result)).Trim()
            if ($tail.Length -gt 800) { $tail = $tail.Substring($tail.Length - 800) }
            throw "load generator exited with $code before the stage ended (expected 124 from timeout): $tail"
        }
    }
    finally {
        $Bench.Process.Dispose()
    }
}

# K8s 压测端 Pod:和 redis-player 反亲和(preferred,单节点的 kind 上也能起来),口令走 Secret,不上命令行。
function New-CapacityBenchPodManifest {
    param([Parameter(Mandatory = $true)][string]$Namespace)
    return @"
apiVersion: v1
kind: Pod
metadata:
  name: $($script:BenchPodName)
  namespace: $Namespace
  labels:
    app: redis-capacity-bench
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  affinity:
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            labelSelector:
              matchLabels:
                app: redis-player
            topologyKey: kubernetes.io/hostname
  containers:
    - name: bench
      image: $Image
      command: ["sleep", "86400"]
      env:
        - name: REDIS_PASSWORD
          valueFrom:
            secretKeyRef:
              name: $AuthSecret
              key: password
              optional: true
      resources:
        requests:
          cpu: "$Threads"
          memory: 256Mi
"@
}

function Get-CapacityKubectlText {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [int]$TimeoutSec = 60)
    $result = Invoke-CapacityProcess -FilePath 'kubectl' -ArgumentList (@(Get-CapacityKubectlBase) + $Arguments) -TimeoutSec $TimeoutSec
    if ($result.ExitCode -ne 0) { throw "kubectl $($Arguments -join ' ') failed (exit $($result.ExitCode)): $($result.ErrorOutput.Trim())" }
    return $result.Output
}

# 建压测端 Pod 并等它就绪。返回它是否与某个集群节点同机。
function Start-CapacityBenchPod {
    param([Parameter(Mandatory = $true)]$TargetSpec, [Parameter(Mandatory = $true)][string]$ManifestPath)
    $null = Get-CapacityKubectlText -Arguments @('delete', 'pod', $script:BenchPodName, '-n', $TargetSpec.Namespace, '--ignore-not-found', '--wait=true') -TimeoutSec 90
    $null = Get-CapacityKubectlText -Arguments @('apply', '-f', $ManifestPath)
    $null = Get-CapacityKubectlText -Arguments @('wait', '--for=condition=Ready', "pod/$($script:BenchPodName)", '-n', $TargetSpec.Namespace, '--timeout=180s') -TimeoutSec 190
    $benchNode = (Get-CapacityKubectlText -Arguments @('get', 'pod', $script:BenchPodName, '-n', $TargetSpec.Namespace, '-o', 'jsonpath={.spec.nodeName}')).Trim()
    $redisNodes = @((Get-CapacityKubectlText -Arguments @('get', 'pods', '-n', $TargetSpec.Namespace, '-l', "app=$($TargetSpec.StatefulSet)", '-o', 'jsonpath={.items[*].spec.nodeName}')) -split '\s+' | Where-Object { $_ })
    return ($redisNodes -contains $benchNode)
}

# ----- 主流程 -----------------------------------------------------------------------------------------------

$targetSpec = ConvertFrom-CapacityTarget -Spec $Target
$steps = ConvertFrom-CapacitySteps -Text $ClientSteps
$tailSeconds = 20

if ($DryRun) {
    Write-Host "[dry-run] target=$Target kind=$($targetSpec.Kind) steps=$($steps -join ',') stage=${StageSeconds}s warmup=${WarmupSeconds}s threads=$Threads pipeline=$Pipeline keyspace=$KeySpace"
    if ($targetSpec.Kind -eq 'k8s') {
        Write-Host "[dry-run] would apply pod/$($script:BenchPodName) in namespace $($targetSpec.Namespace):"
        Write-Host (New-CapacityBenchPodManifest -Namespace $targetSpec.Namespace)
    }
    foreach ($clients in $steps) {
        $benchArgs = New-CapacityBenchmarkArguments -SeedHost '<first master>' -SeedPort $targetSpec.Port -Clients $clients -Threads $Threads -Pipeline $Pipeline -KeySpace $KeySpace
        $command = Get-CapacityBenchmarkCommand -TargetSpec $targetSpec -Seconds ($WarmupSeconds + $StageSeconds + $tailSeconds) -BenchmarkArguments $benchArgs
        Write-Host "[dry-run] step clients=$clients`: $($command.File) $($command.Args -join ' ')"
    }
    Write-Host '[dry-run] nothing was executed.'
    exit 0
}

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
if ([string]::IsNullOrEmpty($OutDir)) { $OutDir = Join-Path $repoRoot ('build/redis-capacity-' + (Get-Date -Format 'yyyyMMdd-HHmmss')) }
$null = New-Item -ItemType Directory -Force -Path $OutDir
$reportPath = Join-Path $OutDir 'capacity.txt'
function Write-CapacityLine {
    param([string]$Line = '')
    Write-Host $Line
    Add-Content -LiteralPath $reportPath -Encoding utf8 -Value $Line
}

$colocated = ($targetSpec.Kind -eq 'docker')
$stages = [System.Collections.Generic.List[object]]::new()
$clock = [System.Diagnostics.Stopwatch]::StartNew()
try {
    if ($targetSpec.Kind -eq 'k8s') {
        $manifestPath = Join-Path $OutDir 'bench-pod.yaml'
        [System.IO.File]::WriteAllText($manifestPath, (New-CapacityBenchPodManifest -Namespace $targetSpec.Namespace))
        $colocated = Start-CapacityBenchPod -TargetSpec $targetSpec -ManifestPath $manifestPath
    }
    if ($colocated) {
        Write-Warning 'The load generator shares a host with cluster nodes: it competes with the masters for CPU, so the numbers are only good for relative comparison.'
    }
    Write-CapacityLine "target=$Target steps=$($steps -join ',') stage=${StageSeconds}s warmup=${WarmupSeconds}s threads=$Threads pipeline=$Pipeline keyspace=$KeySpace colocated=$colocated"
    Write-CapacityLine ('{0,7} {1,-8} {2,6} {3,10} {4,10} {5,8} {6,9} {7,8} {8,8}' -f 'clients', 'node', 'slots', 'ops/s', 'eval/s', 'main_cpu%', 'mem_mb', 'conns', 'errors')

    $stageIndex = 0
    foreach ($clients in $steps) {
        $stageIndex++
        $masters = Get-CapacityMasters -TargetSpec $targetSpec
        $fingerprint = Get-CapacityTopologyFingerprint -Masters $masters
        $benchArgs = New-CapacityBenchmarkArguments -SeedHost $masters[0].Host -SeedPort $masters[0].Port -Clients $clients -Threads $Threads -Pipeline $Pipeline -KeySpace $KeySpace
        $benchSeconds = $WarmupSeconds + $StageSeconds + $tailSeconds
        $bench = Start-CapacityBenchmark -Command (Get-CapacityBenchmarkCommand -TargetSpec $targetSpec -Seconds $benchSeconds -BenchmarkArguments $benchArgs)
        Start-Sleep -Seconds $WarmupSeconds
        if ($bench.Process.HasExited) { Wait-CapacityBenchmark -Bench $bench -TimeoutSec 1 }
        $before = Read-CapacitySnapshot -TargetSpec $targetSpec -Masters $masters -Clock $clock -RawPrefix (Join-Path $OutDir "stage${stageIndex}_before")
        Start-Sleep -Seconds $StageSeconds
        $after = Read-CapacitySnapshot -TargetSpec $targetSpec -Masters $masters -Clock $clock -RawPrefix (Join-Path $OutDir "stage${stageIndex}_after")
        $stillRunning = -not $bench.Process.HasExited
        $topologyAfter = Get-CapacityTopologyFingerprint -Masters (Get-CapacityMasters -TargetSpec $targetSpec)
        Wait-CapacityBenchmark -Bench $bench -TimeoutSec ($tailSeconds + 30)
        if (-not $stillRunning) { throw "load generator stopped before the measurement window ended (clients=$clients): the numbers would mix idle time" }
        if ($topologyAfter -cne $fingerprint) { throw "cluster topology changed during the stage (clients=$clients): the deltas are meaningless" }

        $rows = @(foreach ($master in $masters) {
                Get-CapacityMasterRow -Master $master -Before $before[$master.NodeId].Fields -After $after[$master.NodeId].Fields -Seconds ($after[$master.NodeId].At - $before[$master.NodeId].At)
            })
        $stages.Add([pscustomobject]@{ Clients = $clients; Masters = $rows })
        foreach ($row in $rows) {
            Write-CapacityLine ('{0,7} {1,-8} {2,6} {3,10} {4,10} {5,8} {6,9} {7,8} {8,8}' -f $clients, $row.NodeId.Substring(0, 8), $row.Slots, $row.OpsPerSec, $row.EvalPerSec, $row.MainCpuPercent, $row.UsedMemoryMb, $row.Clients, $row.ErrorReplies)
        }
        $totalOps = [math]::Round(($rows | Measure-Object -Property OpsPerSec -Sum).Sum, 1)
        $totalEval = [math]::Round(($rows | Measure-Object -Property EvalPerSec -Sum).Sum, 1)
        Write-CapacityLine ('{0,7} {1,-8} {2,6} {3,10} {4,10}' -f $clients, 'total', '', $totalOps, $totalEval)
    }
}
finally {
    if ($targetSpec.Kind -eq 'k8s') {
        try { $null = Get-CapacityKubectlText -Arguments @('delete', 'pod', $script:BenchPodName, '-n', $targetSpec.Namespace, '--ignore-not-found', '--wait=false') }
        catch { Write-Warning "failed to delete pod/$($script:BenchPodName): $($_.Exception.Message)" }
    }
}

$verdict = Get-CapacityVerdict -Stages $stages.ToArray() -CpuThresholdPercent $SaturatedCpuPercent -PlateauGrowthPercent $PlateauGrowthPercent
Write-CapacityLine ''
Write-CapacityLine ('per_master_ops min={0} max={1} masters={2} total_ops={3} min_main_cpu={4}% last_step_growth={5}%' -f $verdict.MinOpsPerMaster, $verdict.MaxOpsPerMaster, $verdict.Masters, $verdict.TotalOpsPerSec, $verdict.MinMainCpuPercent, $verdict.LastStepGrowthPct)
[System.IO.File]::WriteAllText((Join-Path $OutDir 'capacity.json'), (@{ Target = $Target; Colocated = $colocated; Verdict = $verdict; Stages = $stages.ToArray() } | ConvertTo-Json -Depth 6))
if ($verdict.Saturated -and -not $colocated) {
    Write-CapacityLine 'VERDICT SATURATED: main threads are full and ops have plateaued. These per-master numbers are capacity evidence (docs/design/redis-player-store-cluster.md section 14.3).'
    Write-CapacityLine "results: $OutDir"
    exit 0
}
if ($verdict.Saturated) {
    Write-CapacityLine 'VERDICT SATURATED, COLOCATED: the masters are full, but the load generator shared their host. Not capacity evidence; rerun with a separate load generator.'
}
else {
    Write-CapacityLine 'VERDICT NOT SATURATED: these numbers are NOT capacity evidence.'
    foreach ($reason in $verdict.Reasons) { Write-CapacityLine "  - $reason" }
}
Write-CapacityLine "results: $OutDir"
exit 2
