# stress_snap.ps1 — periodic multi-endpoint Prometheus snapshot for stress runs.
#
# Per AGENTS.md §6.2, every stress round must capture prom snapshots at
# ramp-end / steady-mid / steady-end and store them under the run dir's
# prom-snapshots/ folder for stress_summarize.ps1 to consume.
#
# Round 16+ scrapes these endpoints one after another at every stage:
#   :9101  login            (entergame_*, dataloader_preload_*, rpc_client_requests_*)
#   :9150  scene_manager    (scene_manager_enter_scene_stage_seconds, …)
#   :9160  db               (db_task_stage_seconds, db_task_result_total)
#   :9260  data_service     (data_service_home_zone_read_seconds, data_service_account_directory_rpc_seconds,
#                            data_service_route_read_total;2026-10-23 起,L-18 观测批)
#
# Each snapshot is written as `t{N}m_<service>.txt` so stress_summarize.ps1
# can route it by suffix (_login / _sm / _db / _ds).
# 某个端点没起(例如本机没开 data_service)只在 job.log 记一行 FAIL,不中断。
# 多 zone 本机启动(go_services.ps1 -Zone N)时各服务端口按该脚本的规则位移,四个端口都要经 -Endpoints 手传。
#
# Redis INFO 快照(L-18 / G4a 第 1a 批,命名同 docs/design/shared-redis-split-by-purpose.md §6.4):
#   每个时刻 HTTP 抓完后,再对 -RedisTargets 里的每个实例抓一次 `redis-cli INFO <sections>`,
#   存成 `t{N}m_redis_<name>.txt`,首行是 `# stress_snap target=<name> spec=<spec> captured_at=<ISO8601>`,
#   之后是 INFO 原文。stress_summarize.ps1 用两次快照之差出 ops/s、CPU、命令级耗时。
#   本机与 K8s 都没有 redis_exporter,所以走 INFO 文本快照;G4a 拆分之前 mapping 与核心是同一个实例。
#
# 参数:
#   -RedisTargets       hashtable,name → spec,缺省 @{ core = 'docker:redis' };传 @{} 关闭 Redis 抓取。
#                       name 必须匹配 ^[a-z][a-z0-9-]{0,31}$ 且不得为 login / sm / db / ds
#                       (否则 t5m_redis_db.txt 的 BaseName 以 _db 结尾,会被汇总脚本当成 db 快照)。
#                       spec 两种:
#                         'docker:<container>'   docker --context <DockerContext> exec <container> redis-cli INFO …
#                                                (本机没装 redis-cli,用 compose 容器自带的;compose 的 redis 无口令)
#                         'k8s:<namespace>/<statefulset>[@<host>:<port>]'
#                                                kubectl exec 进该 StatefulSet 的 redis 容器,用 -h <host> -p <port> 连
#                                                (host 缺省 = statefulset 名,port 缺省 6379;高可用形态下 host 写 Service,
#                                                经 haproxy 落到当前主)。口令取 Pod 自己的 REDIS_PASSWORD,经 REDISCLI_AUTH
#                                                传给 redis-cli,不上本机命令行、不进日志(同 k8s_gate_drain.ps1)。
#                       本地要单列 mapping 时传 @{ core = 'docker:redis'; mapping = 'docker:redis' }:同一实例抓两份,
#                       汇总脚本按 run_id 识别成 same_as,不重复计数。
#   -DockerContext      docker 形态用的 --context,缺省 'desktop-linux'(同 start_game.ps1);传 '' 用当前 context。
#   -RedisInfoSections  INFO 的 section 列表(多 section 需 Redis 7.0+;K8s 7.2 与本机 8.x 都满足),必须含 server
#                       (汇总求差靠它的 run_id / uptime_in_seconds)。不用 INFO all:8.x 多出 keysizes 等 section,
#                       文件变大、形状随版本漂移。
#   -RedisInfoProvider  测试接缝:非空时改调 `& $RedisInfoProvider <name> <spec>` 取 INFO 文本,不起 docker / kubectl。
#                       只供进程内调用(`& stress_snap.ps1 …`),pwsh -File 传不进 scriptblock。
#   每次 INFO 抓取有 10s 进程级超时(超时即杀掉本次 docker / kubectl 进程),失败只在 job.log 记一行 FAIL、
#   不写文件、不中断后面的 target 与时刻。建议 L-14 的 -Stages 带上 0,t0m 作 Redis 求差基线。
#
# Usage:
#   pwsh tools/scripts/stress_snap.ps1 `
#       -RunDir robot/logs/stress-45k-z1-r16-20260601-120000 `
#       -StartTime '2026-06-01 12:00:00' `
#       -Stages '0,2,5,10,15,18'
#
# StartTime is the wall-clock moment when the robot ramp begins (use the
# value printed by the stress launcher). Stages is a comma-separated list
# of minute offsets from StartTime to capture.

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$RunDir,

    [Parameter(Mandatory = $true)]
    [string]$StartTime,

    [string]$Stages = '2,5,10,15,18',

    [hashtable]$Endpoints = @{
        'login' = 'http://127.0.0.1:9101/metrics'
        'sm'    = 'http://127.0.0.1:9150/metrics'
        'db'    = 'http://127.0.0.1:9160/metrics'
        'ds'    = 'http://127.0.0.1:9260/metrics'
    },

    [hashtable]$RedisTargets = @{ 'core' = 'docker:redis' },

    [string]$DockerContext = 'desktop-linux',

    [string[]]$RedisInfoSections = @('server', 'clients', 'memory', 'persistence', 'stats', 'cpu',
        'commandstats', 'errorstats', 'latencystats', 'keyspace'),

    [scriptblock]$RedisInfoProvider = $null
)

$ErrorActionPreference = 'Stop'

# 单次 INFO 抓取(一个 docker / kubectl 进程)的上限,与 HTTP 抓取的 -TimeoutSec 10 同档。
$RedisInfoTimeoutSec = 10

# 汇总脚本按 BaseName 后缀分流 prom 快照,这些名字做 Redis target 会撞后缀。
$ReservedRedisTargetNames = @('login', 'sm', 'db', 'ds')

# ----- Redis target 校验与命令拼装 ---------------------------------------------

function Assert-RedisTargetName {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Name)
    # -cmatch:大小写敏感。Bad_Name 这类写法必须拒绝,文件名分流正则只认小写。
    if ($Name -cnotmatch '^[a-z][a-z0-9-]{0,31}$') {
        throw "Redis target 名 '$Name' 非法:必须匹配 ^[a-z][a-z0-9-]{0,31}$(小写字母开头,只含小写字母、数字、连字符)"
    }
    if ($ReservedRedisTargetNames -ccontains $Name) {
        throw "Redis target 名 '$Name' 与 prom 快照后缀冲突(保留名 $($ReservedRedisTargetNames -join ' / ')),stress_summarize.ps1 会把 t<N>m_redis_$Name.txt 当成 _$Name 快照"
    }
}

# 解析 spec,非法即 throw。返回 Kind = docker | k8s 的对象;k8s 的各字段按 DNS-1123 / 主机名规则校验,
# 它们会作为独立参数传给 kubectl 与 sh 的位置参数,不拼进任何脚本文本。
function ConvertFrom-RedisTargetSpec {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Spec)
    if ($Spec -cmatch '^docker:(?<container>[A-Za-z0-9][A-Za-z0-9_.-]*)$') {
        return [pscustomobject]@{ Kind = 'docker'; Container = $Matches['container'] }
    }
    $dns = '[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?'
    $hostPattern = '[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?'
    if ($Spec -cmatch "^k8s:(?<ns>$dns)/(?<sts>$dns)(?:@(?<host>$hostPattern):(?<port>\d{1,5}))?$") {
        $redisHost = if ($Matches['host']) { $Matches['host'] } else { $Matches['sts'] }
        $port = if ($Matches['port']) { [int]$Matches['port'] } else { 6379 }
        if ($port -lt 1 -or $port -gt 65535) { throw "Redis spec '$Spec' 端口 $port 越界(1..65535)" }
        return [pscustomobject]@{
            Kind        = 'k8s'
            Namespace   = $Matches['ns']
            StatefulSet = $Matches['sts']
            Host        = $redisHost
            Port        = $port
        }
    }
    throw "Redis spec '$Spec' 非法:只支持 'docker:<container>' 与 'k8s:<namespace>/<statefulset>[@<host>:<port>]'"
}

# 返回 { FilePath; ArgumentList }。参数逐项进 ArgumentList,不经本机 shell 拼接。
# k8s 形态仿 k8s_gate_drain.ps1 的 Invoke-GateDrainRedis:exec statefulset/<sts> 的 -c redis 容器(高可用形态的
# Pod 另有哨兵容器;Service 指向的 haproxy 镜像里没有 redis-cli),口令只在 Pod 内由 REDIS_PASSWORD 转成
# REDISCLI_AUTH;为空时不设(设成空串 redis-cli 会发 AUTH "" 并报错)。host / port 经 sh 的位置参数传入。
function Get-RedisInfoCommand {
    param(
        [Parameter(Mandatory = $true)]$Target,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$DockerContext,
        [Parameter(Mandatory = $true)][string[]]$Sections,
        [Parameter(Mandatory = $true)][int]$TimeoutSec
    )
    if ($Target.Kind -eq 'docker') {
        $argv = @()
        if ($DockerContext) { $argv += @('--context', $DockerContext) }
        $argv += @('exec', $Target.Container, 'redis-cli', 'INFO') + $Sections
        return [pscustomobject]@{ FilePath = 'docker'; ArgumentList = $argv }
    }
    $shell = 'h=$1; p=$2; shift 2; if [ -n "$REDIS_PASSWORD" ]; then REDISCLI_AUTH=$REDIS_PASSWORD; export REDISCLI_AUTH; fi; exec redis-cli -h "$h" -p "$p" "$@"'
    $argv = @("--request-timeout=${TimeoutSec}s", 'exec', '-n', $Target.Namespace, "statefulset/$($Target.StatefulSet)",
        '-c', 'redis', '--', 'sh', '-c', $shell, 'sh', $Target.Host, [string]$Target.Port, 'INFO') + $Sections
    return [pscustomobject]@{ FilePath = 'kubectl'; ArgumentList = $argv }
}

# 起外部进程并在截止时硬杀(Process + WaitForExit;Invoke-WebRequest 式的 -TimeoutSec 管不住原生命令)。
# 与 k8s_gate_drain.ps1 的 Invoke-GateDrainProcess 同一做法(那边是入口脚本内的函数,无法 dot-source 复用)。
function Invoke-BoundedProcess {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][string[]]$ArgumentList,
        [Parameter(Mandatory = $true)][int]$TimeoutSec
    )
    $timeoutMs = $TimeoutSec * 1000
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
            try { $process.Kill($true) } catch { }
            $null = $process.WaitForExit(1000)
            return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = "超过 ${TimeoutSec}s 未结束,已终止"; TimedOut = $true }
        }
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

# 取一个 target 的 INFO 文本;任何失败都 throw,由调用方记 FAIL。
# redis-cli 在非终端下把错误回复(NOAUTH / ERR …)直接打到 stdout,退出码不一定非 0,所以还要看文本形状:
# 至少一行 key:value,且不是错误回复。
function Get-RedisInfoText {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Spec,
        [Parameter(Mandatory = $true)]$Target
    )
    if ($null -ne $RedisInfoProvider) {
        $text = (@(& $RedisInfoProvider $Name $Spec) | ForEach-Object { [string]$_ }) -join "`n"
    } else {
        $cmd = Get-RedisInfoCommand -Target $Target -DockerContext $DockerContext -Sections $RedisInfoSections -TimeoutSec $RedisInfoTimeoutSec
        $exe = Get-Command $cmd.FilePath -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($null -eq $exe) { throw "找不到 $($cmd.FilePath)" }
        $result = Invoke-BoundedProcess -FilePath $exe.Source -ArgumentList $cmd.ArgumentList -TimeoutSec $RedisInfoTimeoutSec
        if ($result.TimedOut) { throw $result.ErrorOutput }
        if ($result.ExitCode -ne 0) {
            $tail = ([string]$result.ErrorOutput).Trim()
            if ($tail.Length -gt 300) { $tail = $tail.Substring($tail.Length - 300) }
            throw "exit=$($result.ExitCode): $tail"
        }
        $text = [string]$result.Output
    }
    # 大小写敏感:错误回复是大写前缀,而 INFO 里有 loading:0 这类小写同名字段。
    if ($text -cmatch '(?m)^\s*(\(error\)\s*)?(ERR|NOAUTH|WRONGPASS|NOPERM|LOADING|MASTERDOWN)\b') {
        throw "Redis 回了错误: $(($text.Trim() -split "`r?`n")[0])"
    }
    if ($text -cnotmatch '(?m)^[a-z][a-z0-9_]*:') {
        throw "回复里没有任何 INFO 字段(长度 $($text.Length))"
    }
    return $text
}

# ----- 参数校验:全部在任何抓取与落盘之前完成 ---------------------------------

foreach ($svc in $Endpoints.Keys) {
    if ([string]$svc -like 'redis_*') {
        throw "Endpoints 键 '$svc' 以 redis_ 开头,t<N>m_$svc.txt 会被 stress_summarize.ps1 当成 Redis INFO 快照"
    }
}
foreach ($section in $RedisInfoSections) {
    if ($section -cnotmatch '^[a-z]+$') { throw "RedisInfoSections 里的 '$section' 非法:只许小写字母" }
}
if ($RedisTargets.Count -gt 0 -and -not ($RedisInfoSections | Where-Object { $_ -in @('server', 'default', 'all', 'everything') })) {
    throw "RedisInfoSections 必须含 server(或 default / all / everything):汇总脚本按 run_id / uptime_in_seconds 求差"
}
$redisTargetList = @(
    foreach ($name in ($RedisTargets.Keys | ForEach-Object { [string]$_ } | Sort-Object)) {
        Assert-RedisTargetName -Name $name
        $spec = [string]$RedisTargets[$name]
        [pscustomobject]@{ Name = $name; Spec = $spec; Target = (ConvertFrom-RedisTargetSpec -Spec $spec) }
    }
)

# ----- 抓取 -------------------------------------------------------------------

$snapDir = Join-Path $RunDir 'prom-snapshots'
if (-not (Test-Path $snapDir)) {
    New-Item -ItemType Directory -Path $snapDir | Out-Null
}

$logFile = Join-Path $snapDir 'job.log'
$start   = [datetime]$StartTime

function Write-JobLog($msg) {
    $ts = (Get-Date -Format 'yyyy-MM-dd HH:mm:ss')
    "$ts $msg" | Out-File -Append -FilePath $logFile -Encoding UTF8
}

Write-JobLog "stress_snap started: runDir=$RunDir start=$StartTime stages=$Stages"
Write-JobLog "endpoints: $($Endpoints.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" } | Join-String -Separator ' ')"
Write-JobLog "redis targets: $(($redisTargetList | ForEach-Object { "$($_.Name)=$($_.Spec)" }) -join ' ') sections=$($RedisInfoSections -join ',')$(if ($null -ne $RedisInfoProvider) { ' provider=injected' })"

$stageList = $Stages.Split(',') | ForEach-Object { [int]$_.Trim() }

foreach ($mins in $stageList) {
    $tag = "t${mins}m"
    $tgt = $start.AddMinutes($mins)
    $wait = ($tgt - (Get-Date)).TotalSeconds
    if ($wait -gt 0) {
        Write-JobLog "$tag waiting $([int]$wait)s until $($tgt.ToString('HH:mm:ss'))"
        Start-Sleep -Seconds ([int]$wait)
    }

    foreach ($svc in $Endpoints.Keys) {
        $url = $Endpoints[$svc]
        $outFile = Join-Path $snapDir "${tag}_${svc}.txt"
        try {
            $c = (Invoke-WebRequest -Uri $url -TimeoutSec 10 -UseBasicParsing).Content
            Set-Content -Path $outFile -Value $c -Encoding UTF8
            Write-JobLog "$tag $svc saved size=$($c.Length) ($url)"
        } catch {
            Write-JobLog "$tag $svc FAIL ($url): $($_.Exception.Message)"
        }
    }

    foreach ($rt in $redisTargetList) {
        $outFile = Join-Path $snapDir "${tag}_redis_$($rt.Name).txt"
        try {
            $text = Get-RedisInfoText -Name $rt.Name -Spec $rt.Spec -Target $rt.Target
            $header = "# stress_snap target=$($rt.Name) spec=$($rt.Spec) captured_at=$((Get-Date).ToString('o'))"
            Set-Content -Path $outFile -Value ($header + "`n" + $text) -Encoding UTF8
            Write-JobLog "$tag redis_$($rt.Name) saved size=$($text.Length) ($($rt.Spec))"
        } catch {
            Write-JobLog "$tag redis_$($rt.Name) FAIL ($($rt.Spec)): $($_.Exception.Message)"
        }
    }
}

Write-JobLog "stress_snap finished: all stages captured"
