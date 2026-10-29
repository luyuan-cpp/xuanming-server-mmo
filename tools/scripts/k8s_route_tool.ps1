#requires -Version 7
<#
.SYNOPSIS
    在 K8s 上以一次性 Job 跑 data_service 的映射运维工具 route-tool(import-from-redis / audit / freeze / reconcile-from-cache /
    unfreeze / tidb-prepare / rewarm),等它结束,把 stdout / stderr 存进报告目录,按容器退出码原样返回。

.DESCRIPTION
    D1 第 1b-3 批(docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §7 第 3 条、§13.4 第 3 条)。
    生产上 route-tool 并在 data_service 主程序里:`/app/service -f /app/etc/data_service.yaml -route-tool <子命令> [参数]`
    (go/data_service/data_service.go;子命令与退出码见 go/data_service/internal/routetoolcli/cli.go)。
    不 kubectl exec 进在役 Pod:data-service 容器 limit 512Mi,import / audit 的已见集合是 O(N),可能把在役进程一起 OOM 掉
    (exec 只限 -dry-run 或小库,runbook §9.4)。本脚本改为渲染 deploy/k8s/manifests/go-svc/data-service-route-tool.yaml:
    同一镜像(缺省取在役 deployment/data-service 的镜像)、同一 ConfigMap(go-svc-data-service-config)、资源单独设。

    流程:
      1. 入口校验(任何 kubectl 之前):-RouteToolArgs 第一项必须是已知子命令;不许带 -f / -route-tool / -migrate / -version
         (这几项由本脚本固定);不许有换行等控制字符;只有一项且含空白 = 把整条命令行塞成了一个字符串,拒绝(不替你按空格切)。
      2. 只读核对:zone namespace 存在;**全部 namespace** 里没有在途的 route-tool Job(同一个全局库只许一个在跑);
         没给 -Image 时读 deployment/data-service 的镜像与拉取策略。
      3. 渲染 Job(名字 data-service-route-tool-<运行号>,每次新名字、不删上一次的),写进报告目录的 manifest.yaml,kubectl apply。
      4. 有界轮询 Job 终态(预算 = -TimeoutSeconds + 120s;Job 自己的 activeDeadlineSeconds = -TimeoutSeconds,到点由 K8s 杀掉)。
      5. 取 Pod 的容器退出码与日志:日志按带运行号的标记切回 stdout.txt / stderr.txt(Job 模板里 sh 包了一层,见模板注释),
         再写 summary.txt。控制台只打 stdout(统计行)与 stderr 末尾若干行。

    -DryRun 只做第 1 步与渲染,把 Job 清单打印出来(夹在 --- BEGIN MANIFEST --- / --- END MANIFEST --- 之间,与 k8s_deploy.ps1 同一标记),
    不调用 kubectl、不建报告目录。没给 -Image 时清单里的镜像写成说明文字(真跑时从 deployment/data-service 读)。

    退出码:route-tool 容器的退出码原样返回 —— 0 干净 / 1 完成但需要人工处理 / 2 失败 / 3 有生效的合服围栏。
    本脚本自己失败(参数不合法、kubectl 失败、namespace 不存在、已有在途 Job、等不到终态、容器退出码不在 0..3、取不到退出码)一律 2,
    控制台与 summary.txt 写明是哪一种;等不到终态时 Job 可能还在跑,不要立刻再起一次。
    未预料的异常由入口兜成 2(pwsh -File 下未捕获异常的退出码是 1,会与「完成但需要人工处理」撞号);Job 结束之后写报告
    (stdout.txt / stderr.txt / summary.txt)失败只告警,退出码仍按容器退出码,不让写盘失败盖掉 0..3。
    例外:参数绑定失败(pwsh -File 下缺 -ZoneName / -RouteToolArgs、-ZoneName 不合格式)发生在脚本运行之前,退出码是 pwsh 的 1;
    经 dev_tools.ps1 -Command k8s-route-tool 调用时由包装层兜成 2。

    口令不进输出:本脚本不读 ConfigMap 内容(口令在里面),清单里只有 ConfigMap 的名字;route-tool 的 stderr 只带主机与库名。
    所有 kubectl 调用都有 HTTP 超时与进程级截止;kind 上务必显式传 -KubeContext kind-mmorpg。

.EXAMPLE
    # 预览清单(不碰集群)
    pwsh -NoProfile -Command "& ./tools/scripts/k8s_route_tool.ps1 -ZoneName z1 -RouteToolArgs @('audit') -Image ghcr.io/luyuancpp/mmorpg-data-service:0123456789ab -DryRun"

.EXAMPLE
    # 冻结期审计(全局库回档后的恢复流程第 3 步,runbook §9.4)
    pwsh -NoProfile -Command "& ./tools/scripts/k8s_route_tool.ps1 -ZoneName z1 -RouteToolArgs @('audit', '-frozen-recovery', '-timeout=60m') -TimeoutSeconds 4200 -KubeContext kind-mmorpg"
    子命令参数含 '-' 开头的项,用 -Command 形式传数组;`pwsh -File` 下逗号列表会被当成一个字符串。
#>
param(
    # zone 名,与 k8s_deploy.ps1 -ZoneName 同义:namespace = <NamespacePrefix>-<ZoneName>。
    # route-tool 读写的是全服共用的全局库与 mapping Redis,哪个 zone 都一样;选 zone 只决定用哪份 ConfigMap、在哪个 namespace 起 Job。
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')]
    [string]$ZoneName,

    # route-tool 的子命令与参数,原样跟在 `-f /app/etc/data_service.yaml -route-tool` 之后。第一项是子命令。
    [Parameter(Mandatory = $true)]
    [AllowEmptyCollection()]
    [AllowEmptyString()]
    [string[]]$RouteToolArgs,

    # 报告目录:manifest.yaml / stdout.txt / stderr.txt / summary.txt。留空 = <仓库>/run/route-tool/<ZoneName>-<运行号>(run/ 不入库)。
    [string]$ReportDir = "",

    # Job 的 activeDeadlineSeconds,同时是本脚本等终态的预算(再加 120s 余量)。要大于子命令自己的 -timeout(缺省 30m)加启动开销,
    # 否则 K8s 会在 route-tool 自己超时之前把它杀掉。
    [ValidateRange(60, 86400)]
    [int]$TimeoutSeconds = 2400,

    # 镜像引用。留空 = 取在役 deployment/data-service 的镜像与拉取策略(同一二进制、同一份配置);给了就按它,拉取策略按 tag 是否可变推导。
    [string]$Image = "",

    [switch]$DryRun,

    # 与 k8s_deploy.ps1 -NamespacePrefix 同义。
    [string]$NamespacePrefix = "mmorpg-zone",

    # 与 k8s_deploy.ps1 的 Build-KubectlBaseArgs 同语义:非空才带 --context / --kubeconfig。
    [string]$KubeContext = "",
    [string]$KubeConfig = ""
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = [IO.Path]::GetFullPath((Join-Path $ScriptDir '../..'))
# Resolve-ImagePullPolicy:与 k8s_deploy.ps1 同一份推导规则。路径逐级 Join-Path:契约测试也跑在 Linux 上,不写反斜杠字面量。
. (Join-Path (Join-Path $ScriptDir 'lib') 'release_common.ps1')

# 本脚本与 Job 模板、data_service 入口之间的全部字面量契约,集中在一处;tools/scripts/tests/k8s_route_tool.tests.ps1 逐项钉住。
function Get-RouteToolContract {
    return [pscustomobject]@{
        # Job 模板(相对仓库根)与它里面的占位。
        JobTemplate             = 'deploy/k8s/manifests/go-svc/data-service-route-tool.yaml'
        # Job 名 = 前缀 + '-' + 运行号;Pod 标签 app=<JobAppLabel>(查在途 Job 用)。
        JobNamePrefix           = 'data-service-route-tool'
        JobAppLabel             = 'data-service-route-tool'
        ContainerName           = 'route-tool'
        # 在役服务:k8s_deploy.ps1 $GoSvcCatalogue."data-service" 与 manifests/go-svc/data-service.yaml。
        DeploymentName          = 'data-service'
        DeploymentContainerName = 'data-service'
        ConfigMapName           = 'go-svc-data-service-config'
        # 与 data-service.yaml 的 args 前两项相同,再接 -route-tool(data_service.go 的 flag)。
        FixedArgs               = @('-f', '/app/etc/data_service.yaml', '-route-tool')
        # routetoolcli/cli.go 的 sub* 常量(第 1a / 1b-1 批)加第 1b-2 批的 rewarm。新增子命令时先在 cli.go 加,再加到这里
        # (测试钉住 cli.go 里的每个子命令都在这张表里)。
        Subcommands             = @('import-from-redis', 'audit', 'tidb-prepare', 'freeze', 'reconcile-from-cache', 'unfreeze', 'rewarm')
        # 由本脚本固定的入口参数(Go flag 认 -x 与 --x、-x=v):出现在 -RouteToolArgs 里一律拒绝。
        ReservedFlags           = @('f', 'route-tool', 'migrate', 'version')
        # route-tool 的退出码(cli.go usage):原样返回。本脚本自己的失败也用 Failed。
        ExitClean               = 0
        ExitNeedsAttention      = 1
        ExitFailed              = 2
        ExitFenced              = 3
        # 标记行(Job 模板的 sh 包装打印;{0} = 运行号)。
        StdoutBeginFormat       = '@@ROUTE_TOOL_STDOUT_BEGIN {0}@@'
        StdoutEndFormat         = '@@ROUTE_TOOL_STDOUT_END {0}@@'
        # 单次 kubectl 调用的截止;取日志单独放宽(大库的 stderr 明细可能有几 MB)。
        CallTimeoutSeconds      = 30
        LogsTimeoutSeconds      = 180
        PollIntervalSeconds     = 5
        ProgressIntervalSeconds = 60
        # 等终态的预算 = -TimeoutSeconds + 本值:activeDeadlineSeconds 到点之后,Job 控制器还要一点时间把 Job 判成 Failed。
        StatusGraceSeconds      = 120
        # 控制台打印 stderr 的末尾行数(全文在 stderr.txt)。
        StderrTailLines         = 20
    }
}

# ─── 参数校验与渲染(纯函数) ───

# -RouteToolArgs 的形状校验,返回错误文本数组(空 = 合法)。只看形状,子命令自己的参数由 route-tool 解析(非法时它以 2 退出)。
function Get-RouteToolArgErrors {
    param([AllowNull()][AllowEmptyCollection()][string[]]$Arguments)
    $contract = Get-RouteToolContract
    $errors = New-Object System.Collections.Generic.List[string]
    $items = @($Arguments | ForEach-Object { [string]$_ })
    if ($items.Count -eq 0) {
        $errors.Add("-RouteToolArgs 为空:第一项必须是子命令($($contract.Subcommands -join ' / '))")
        return [string[]]$errors.ToArray()
    }
    # 只有一项时它就是子命令,子命令名里没有空白与逗号:出现了就是整条命令行被塞成了一项(pwsh -File 下逗号列表就是这样)。
    # 多项时逗号是合法值(tidb-prepare -zones=1,2),不在此列。
    if ($items.Count -eq 1 -and $items[0] -match '[\s,]') {
        $errors.Add("-RouteToolArgs 只有一项且含空白或逗号('$($items[0])'):看起来把整条命令行塞成了一个字符串(pwsh -File 下逗号列表就是这样)。请用 -Command 形式逐项传数组,例如 @('audit', '-frozen-recovery');本脚本不替你按空格 / 逗号切分")
        return [string[]]$errors.ToArray()
    }
    for ($i = 0; $i -lt $items.Count; $i++) {
        $item = $items[$i]
        if ([string]::IsNullOrEmpty($item)) {
            $errors.Add("-RouteToolArgs 第 $($i + 1) 项是空串")
            continue
        }
        if ($item -match '[\x00-\x1F\x7F]') {
            $errors.Add("-RouteToolArgs 第 $($i + 1) 项含换行 / 制表符等控制字符")
            continue
        }
        $flag = [regex]::Match($item, '^--?(?<name>[^=]+)(?:=.*)?$')
        if ($flag.Success -and ($contract.ReservedFlags -ccontains $flag.Groups['name'].Value)) {
            $errors.Add("-RouteToolArgs 第 $($i + 1) 项 '$item' 是入口参数:-f / -route-tool / -migrate / -version 由本脚本固定(-f $($contract.FixedArgs[1]) -route-tool),不能再传")
        }
    }
    $sub = $items[0]
    if (-not ($contract.Subcommands -ccontains $sub)) {
        $errors.Add("-RouteToolArgs 第一项 '$sub' 不是已知子命令;可选:$($contract.Subcommands -join ' / ')")
    }
    return [string[]]$errors.ToArray()
}

# 运行号:yyyyMMdd-HHmmss-<4 位十六进制>,只含小写字母、数字与 '-'(进 Job 名与标签值)。时间与随机数显式传入,便于测试。
function New-RouteToolRunId {
    param(
        [Parameter(Mandatory = $true)][datetime]$Now,
        [Parameter(Mandatory = $true)][ValidateRange(0, 65535)][int]$Random
    )
    return ('{0}-{1:x4}' -f $Now.ToString('yyyyMMdd-HHmmss', [Globalization.CultureInfo]::InvariantCulture), $Random)
}

# 容器 args:固定前缀 + -RouteToolArgs,渲染成 JSON 数组(JSON 字符串是合法的 YAML 双引号标量,引号 / 反斜杠 / 非 ASCII 都逐项转义)。
function ConvertTo-RouteToolArgsYaml {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    $contract = Get-RouteToolContract
    $all = [string[]](@($contract.FixedArgs) + @($Arguments))
    return (ConvertTo-Json -InputObject $all -Compress)
}

# 渲染 Job 清单。任何占位没替换就 throw(与 k8s_deploy.ps1 的 Apply-GoSvcMigrateJob 同一条 fail-closed)。
function New-RouteToolJobManifest {
    param(
        [Parameter(Mandatory = $true)][string]$TemplateText,
        [Parameter(Mandatory = $true)][string]$RunId,
        [Parameter(Mandatory = $true)][string]$Image,
        [Parameter(Mandatory = $true)][string]$PullPolicy,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][int]$TimeoutSeconds
    )
    $contract = Get-RouteToolContract
    if ($RunId -notmatch '^[0-9]{8}-[0-9]{6}-[0-9a-f]{4}$') { throw "运行号 '$RunId' 形状不对(应为 yyyyMMdd-HHmmss-xxxx)" }
    if ($PullPolicy -cnotin @('Always', 'IfNotPresent', 'Never')) { throw "拉取策略 '$PullPolicy' 不是 Always / IfNotPresent / Never" }
    if ([string]::IsNullOrWhiteSpace($Image) -or $Image -match '[\x00-\x1F\x7F]') { throw "镜像引用为空或含控制字符" }
    $argErrors = @(Get-RouteToolArgErrors -Arguments $Arguments)
    if ($argErrors.Count -gt 0) { throw ("route-tool 参数不合法:" + ($argErrors -join ';')) }

    $text = $TemplateText -replace "`r`n", "`n"
    $text = $text.Replace('PLACEHOLDER_JOB_NAME', "$($contract.JobNamePrefix)-$RunId")
    $text = $text.Replace('PLACEHOLDER_RUN_ID', $RunId)
    # 镜像写成 JSON 字符串:说明文字(DryRun 没给 -Image 时)含空格与括号,裸写进 YAML 会被误解析。
    $text = $text.Replace('PLACEHOLDER_IMAGE', (ConvertTo-Json -InputObject $Image -Compress))
    $text = $text.Replace('PLACEHOLDER_PULL_POLICY', $PullPolicy)
    $text = $text.Replace('PLACEHOLDER_ACTIVE_DEADLINE_SECONDS', [string]$TimeoutSeconds)
    $text = $text.Replace('PLACEHOLDER_ROUTE_TOOL_ARGS', (ConvertTo-RouteToolArgsYaml -Arguments $Arguments))
    $leftover = @([regex]::Matches($text, '(__[A-Z][A-Z0-9_]*__|PLACEHOLDER_[A-Z_]+)') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    if ($leftover.Count -gt 0) {
        throw "Job 模板 $($contract.JobTemplate) 里有未替换的占位:$($leftover -join ', ')。请在 New-RouteToolJobManifest 里补对应的替换。"
    }
    return $text
}

# 把容器日志切回 stdout / stderr:两行标记(带运行号)之间是 stdout,其余是 stderr。标记由 Job 模板的 sh 包装打印:
# BEGIN 行之后是 stdout 原文,再一个换行,再 END 行;所以 stdout = BEGIN 行之后、"\n" + END 标记之前的全部文本(末尾有没有换行都原样还原)。
# 找不到成对的标记(进程被杀、包装没跑到那一步)时 MarkersFound=$false,整段日志都算 stderr。
function Split-RouteToolLog {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [Parameter(Mandatory = $true)][string]$RunId
    )
    $contract = Get-RouteToolContract
    $normalized = $Text -replace "`r`n", "`n"
    $begin = ($contract.StdoutBeginFormat -f $RunId) + "`n"
    $end = "`n" + ($contract.StdoutEndFormat -f $RunId)
    $beginAt = $normalized.LastIndexOf($begin, [StringComparison]::Ordinal)
    if ($beginAt -lt 0) {
        return [pscustomobject]@{ Stdout = ''; Stderr = $normalized; MarkersFound = $false }
    }
    $bodyAt = $beginAt + $begin.Length
    # stdout 为空时 BEGIN 行之后紧跟 "\n" + END:endAt == bodyAt,截出空串。
    $endAt = $normalized.IndexOf($end, $bodyAt, [StringComparison]::Ordinal)
    if ($endAt -lt 0) {
        return [pscustomobject]@{ Stdout = ''; Stderr = $normalized; MarkersFound = $false }
    }
    $stdout = $normalized.Substring($bodyAt, $endAt - $bodyAt)
    $afterEnd = $endAt + $end.Length
    $tail = if ($afterEnd -lt $normalized.Length) { $normalized.Substring($afterEnd).TrimStart("`n") } else { '' }
    $stderr = $normalized.Substring(0, $beginAt) + $tail
    return [pscustomobject]@{ Stdout = $stdout; Stderr = $stderr; MarkersFound = $true }
}

# 容器退出码 → 本脚本退出码。0..3 原样返回;取不到($null)或其它值(OOMKilled 137、找不到二进制 127、被信号杀掉)一律 2。
function Resolve-RouteToolExitCode {
    param([AllowNull()]$ContainerExitCode)
    $contract = Get-RouteToolContract
    if ($null -eq $ContainerExitCode) {
        return [pscustomobject]@{ ExitCode = $contract.ExitFailed; Verdict = 'no-exit-code' }
    }
    $code = [int]$ContainerExitCode
    switch ($code) {
        0 { return [pscustomobject]@{ ExitCode = 0; Verdict = 'clean' } }
        1 { return [pscustomobject]@{ ExitCode = 1; Verdict = 'needs-attention' } }
        2 { return [pscustomobject]@{ ExitCode = 2; Verdict = 'failed' } }
        3 { return [pscustomobject]@{ ExitCode = 3; Verdict = 'merge-fence-active' } }
    }
    return [pscustomobject]@{ ExitCode = $contract.ExitFailed; Verdict = "unexpected-exit-code-$code" }
}

# Job 的 status.conditions → running / complete / failed(SuccessCriteriaMet 与 FailureTarget 是 1.31 起的提前终态)。纯函数。
function Get-RouteToolJobPhase {
    param([Parameter(Mandatory = $true)]$Job)
    $trueTypes = @($Job.status.conditions | Where-Object { $_.status -eq 'True' } | ForEach-Object { [string]$_.type })
    if ($trueTypes -contains 'Failed' -or $trueTypes -contains 'FailureTarget') { return 'failed' }
    if ($trueTypes -contains 'Complete' -or $trueTypes -contains 'SuccessCriteriaMet') { return 'complete' }
    return 'running'
}

# ─── kubectl 调用面 ───

function Build-KubectlBaseArgs {
    $baseArgs = @()
    if (-not [string]::IsNullOrWhiteSpace($KubeContext)) { $baseArgs += @('--context', $KubeContext) }
    if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) { $baseArgs += @('--kubeconfig', $KubeConfig) }
    return , $baseArgs
}

# 起外部进程并在截止时硬杀(与 k8s_gate_drain.ps1 的 Invoke-GateDrainProcess 同一做法;那边是入口脚本,不能 dot-source 复用)。
# 参数逐项进 ArgumentList,不经 shell 拼接;结果不经 $LASTEXITCODE 跨函数传递。
function Invoke-RouteToolProcess {
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
            # 只杀本次 kubectl 及其子进程(凭证插件等),不碰集群里的 Job。
            try { $process.Kill($true) } catch { }
            $null = $process.WaitForExit(1000)
            return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = "process did not exit within ${timeoutMs}ms"; TimedOut = $true }
        }
        if (-not [System.Threading.Tasks.Task]::WaitAll([System.Threading.Tasks.Task[]]@($stdout, $stderr), 1000)) {
            return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = 'process output pipes did not close'; TimedOut = $true }
        }
        return [pscustomobject]@{ ExitCode = $process.ExitCode; Output = $stdout.Result; ErrorOutput = $stderr.Result; TimedOut = $false }
    } catch {
        return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = $_.Exception.Message; TimedOut = $false }
    } finally {
        $process.Dispose()
    }
}

function Invoke-RouteToolKubectl {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [ValidateRange(0.001, 2147483)][double]$TimeoutSeconds = 30
    )
    $kubectl = Get-Command kubectl -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $kubectl) {
        return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = 'kubectl not found'; TimedOut = $false }
    }
    $timeoutMs = [int][Math]::Ceiling($TimeoutSeconds * 1000)
    $allArgs = @((Build-KubectlBaseArgs)) + @("--request-timeout=${timeoutMs}ms") + $Arguments
    return Invoke-RouteToolProcess -FilePath $kubectl.Source -ArgumentList $allArgs -TimeoutSeconds $TimeoutSeconds
}

# 错误输出只留末尾一段:够定位,又不把整屏诊断灌进异常消息。
function Get-RouteToolErrorTail {
    param([AllowNull()][AllowEmptyString()][string]$Text)
    if ([string]::IsNullOrWhiteSpace($Text)) { return '<no error output>' }
    $trimmed = $Text.Trim()
    if ($trimmed.Length -le 400) { return $trimmed }
    return '...' + $trimmed.Substring($trimmed.Length - 400)
}

# 必须成功的调用:失败或超时直接 throw,返回 stdout。
function Invoke-RouteToolKubectlChecked {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][string]$What,
        [ValidateRange(0.001, 2147483)][double]$TimeoutSeconds = 30
    )
    $result = Invoke-RouteToolKubectl -Arguments $Arguments -TimeoutSeconds $TimeoutSeconds
    if ($result.TimedOut -or $result.ExitCode -ne 0) {
        throw "$What 失败(exit=$($result.ExitCode),timedOut=$($result.TimedOut)):$(Get-RouteToolErrorTail $result.ErrorOutput)"
    }
    return [string]$result.Output
}

# ─── 读集群 ───

# 在役 deployment/data-service 的镜像与拉取策略。读不到、容器不是恰好一个 data-service 一律 throw(不替你猜镜像)。
function Get-LiveDataServiceImage {
    param([Parameter(Mandatory = $true)][string]$Namespace)
    $contract = Get-RouteToolContract
    $text = Invoke-RouteToolKubectlChecked -Arguments @('get', 'deployment', $contract.DeploymentName, '-n', $Namespace, '-o', 'json') `
        -What "读取 namespace $Namespace 的 deployment/$($contract.DeploymentName)"
    $deployment = $text | ConvertFrom-Json -ErrorAction Stop
    $containers = @($deployment.spec.template.spec.containers | Where-Object { $_.name -eq $contract.DeploymentContainerName })
    if ($containers.Count -ne 1 -or [string]::IsNullOrWhiteSpace([string]$containers[0].image)) {
        throw "namespace $Namespace 的 deployment/$($contract.DeploymentName) 里名为 $($contract.DeploymentContainerName) 的容器应恰好一个且带 image(实际 $($containers.Count) 个):不替你猜镜像,请显式传 -Image"
    }
    $policy = [string]$containers[0].imagePullPolicy
    if ($policy -cnotin @('Always', 'IfNotPresent', 'Never')) { $policy = Resolve-ImagePullPolicy -ImageRef ([string]$containers[0].image) }
    return [pscustomobject]@{ Image = [string]$containers[0].image; PullPolicy = $policy }
}

# 全部 namespace 里还没到终态的 route-tool Job(同一个全局库只许一个在跑)。返回 "<namespace>/<name>" 数组。
function Get-ActiveRouteToolJobs {
    $contract = Get-RouteToolContract
    $text = Invoke-RouteToolKubectlChecked -Arguments @('get', 'jobs', '--all-namespaces', '-l', "app=$($contract.JobAppLabel)", '-o', 'json') `
        -What '列出全部 namespace 的 route-tool Job'
    $list = $text | ConvertFrom-Json -ErrorAction Stop
    $active = New-Object System.Collections.Generic.List[string]
    foreach ($job in @($list.items)) {
        if ($null -eq $job) { continue }
        if ((Get-RouteToolJobPhase -Job $job) -eq 'running') { $active.Add("$($job.metadata.namespace)/$($job.metadata.name)") }
    }
    return [string[]]$active.ToArray()
}

# Job 当前状态:Phase(running / complete / failed / absent)、Uid、终态原因。查询失败返回 Phase=unknown(由调用方计入重试)。
function Get-RouteToolJobState {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$JobName
    )
    $result = Invoke-RouteToolKubectl -Arguments @('get', 'job', $JobName, '-n', $Namespace, '--ignore-not-found', '-o', 'json')
    if ($result.TimedOut -or $result.ExitCode -ne 0) {
        return [pscustomobject]@{ Phase = 'unknown'; Uid = ''; Reason = (Get-RouteToolErrorTail $result.ErrorOutput) }
    }
    if ([string]::IsNullOrWhiteSpace([string]$result.Output)) { return [pscustomobject]@{ Phase = 'absent'; Uid = ''; Reason = '' } }
    try { $job = [string]$result.Output | ConvertFrom-Json -ErrorAction Stop }
    catch { return [pscustomobject]@{ Phase = 'unknown'; Uid = ''; Reason = 'job json not parseable' } }
    $reasons = @($job.status.conditions | Where-Object { $_.status -eq 'True' } | ForEach-Object { "$($_.type)/$($_.reason)" })
    return [pscustomobject]@{ Phase = (Get-RouteToolJobPhase -Job $job); Uid = [string]$job.metadata.uid; Reason = ($reasons -join ',') }
}

# Job 的 Pod(按 ownerReferences 的 uid 认,不只看 job-name 标签)与 route-tool 容器的退出码。没有 Pod / 没终止时 ExitCode=$null。
function Get-RouteToolPodResult {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$JobName,
        [Parameter(Mandatory = $true)][string]$JobUid
    )
    $contract = Get-RouteToolContract
    $text = Invoke-RouteToolKubectlChecked -Arguments @('get', 'pods', '-n', $Namespace, '-l', "job-name=$JobName", '-o', 'json') `
        -What "读取 Job $JobName 的 Pod"
    $pods = @(($text | ConvertFrom-Json -ErrorAction Stop).items | Where-Object {
            $null -ne $_ -and @($_.metadata.ownerReferences | Where-Object { $_.kind -eq 'Job' -and $_.uid -eq $JobUid }).Count -gt 0
        })
    if ($pods.Count -eq 0) { return [pscustomobject]@{ PodName = ''; ExitCode = $null; Reason = 'no pod owned by the job' } }
    # backoffLimit 0:正常只有一个 Pod;万一有多个,取最后创建的那个。
    $pod = @($pods | Sort-Object { [string]$_.metadata.creationTimestamp })[-1]
    $status = @($pod.status.containerStatuses | Where-Object { $_.name -eq $contract.ContainerName })
    if ($status.Count -ne 1 -or $null -eq $status[0].state.terminated) {
        return [pscustomobject]@{ PodName = [string]$pod.metadata.name; ExitCode = $null; Reason = "container $($contract.ContainerName) has not terminated (pod phase $($pod.status.phase))" }
    }
    $terminated = $status[0].state.terminated
    return [pscustomobject]@{ PodName = [string]$pod.metadata.name; ExitCode = [int]$terminated.exitCode; Reason = [string]$terminated.reason }
}

# ─── 主流程 ───

function Write-RouteToolSummary {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][System.Collections.IDictionary]$Fields)
    $lines = foreach ($key in $Fields.Keys) { '{0}: {1}' -f $key, $Fields[$key] }
    [IO.File]::WriteAllText($Path, (($lines -join "`n") + "`n"), [Text.UTF8Encoding]::new($false))
}

# Job apply 之后的落盘一律经这两个函数:写不了(磁盘满、目录没权限)只告警,不 throw。到这一步 Job 已经建了、可能已经跑完,
# 退出码必须仍按容器退出码(或本脚本已判出的 2)返回;让写盘异常冒到入口,会把真实的 0..3 盖成 2,而且 done 一行打不出来。
function Save-RouteToolSummary {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][System.Collections.IDictionary]$Fields)
    try {
        Write-RouteToolSummary -Path $Path -Fields $Fields
    }
    catch {
        Write-Warning "[route-tool] could not write ${Path}: $($_.Exception.Message); the exit code still follows the container exit code (summary: verdict=$($Fields['verdict']) exit_code=$($Fields['exit_code']) note=$($Fields['note']))"
    }
}

# 返回 $true = 写成。
function Save-RouteToolReportText {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    try {
        [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false))
        return $true
    }
    catch {
        Write-Warning "[route-tool] could not write ${Path}: $($_.Exception.Message); the exit code still follows the container exit code"
        return $false
    }
}

function Invoke-RouteTool {
    $contract = Get-RouteToolContract
    $argErrors = @(Get-RouteToolArgErrors -Arguments $RouteToolArgs)
    if ($argErrors.Count -gt 0) {
        foreach ($e in $argErrors) { Write-Host "[route-tool] invalid argument: $e" }
        return $contract.ExitFailed
    }
    $namespace = "{0}-{1}" -f $NamespacePrefix, $ZoneName
    $runId = New-RouteToolRunId -Now (Get-Date) -Random (Get-Random -Minimum 0 -Maximum 65536)
    $jobName = "$($contract.JobNamePrefix)-$runId"
    $templatePath = Join-Path $RepoRoot $contract.JobTemplate
    try {
        if (-not (Test-Path -LiteralPath $templatePath)) { throw "找不到 Job 模板 $($contract.JobTemplate)" }
        $templateText = [IO.File]::ReadAllText($templatePath)
    }
    catch {
        Write-Host "[route-tool] cannot read the Job template, nothing was created: $($_.Exception.Message)"
        return $contract.ExitFailed
    }
    $subcommand = $RouteToolArgs[0]

    if ($DryRun) {
        try {
            $jobImage = if ([string]::IsNullOrWhiteSpace($Image)) { "(resolved at run time from deployment/$($contract.DeploymentName) in $namespace)" } else { $Image }
            $jobPullPolicy = if ([string]::IsNullOrWhiteSpace($Image)) { 'IfNotPresent' } else { Resolve-ImagePullPolicy -ImageRef $Image }
            $manifest = New-RouteToolJobManifest -TemplateText $templateText -RunId $runId -Image $jobImage -PullPolicy $jobPullPolicy `
                -Arguments $RouteToolArgs -TimeoutSeconds $TimeoutSeconds
        }
        catch {
            Write-Host "[route-tool] cannot render the Job manifest, nothing was created: $($_.Exception.Message)"
            return $contract.ExitFailed
        }
        Write-Host "[dry-run] route-tool job=$jobName namespace=$namespace subcommand=$subcommand timeout=${TimeoutSeconds}s (kubectl is not called, no report directory is created)"
        if ([string]::IsNullOrWhiteSpace($Image)) {
            Write-Host "[dry-run] -Image not given: a real run reads image and imagePullPolicy from deployment/$($contract.DeploymentName); the rendered imagePullPolicy is a placeholder"
        }
        Write-Host '--- BEGIN MANIFEST ---'
        Write-Host $manifest
        Write-Host '--- END MANIFEST ---'
        return $contract.ExitClean
    }

    if ($null -eq (Get-Command kubectl -CommandType Application -ErrorAction SilentlyContinue)) {
        Write-Host '[route-tool] kubectl not found; install it and configure cluster access first'
        return $contract.ExitFailed
    }

    try {
        # 只读核对:namespace 在;没有在途的 route-tool Job;镜像。
        $null = Invoke-RouteToolKubectlChecked -Arguments @('get', 'namespace', $namespace, '-o', 'name') -What "确认 namespace $namespace 存在"
        $active = @(Get-ActiveRouteToolJobs)
        if ($active.Count -gt 0) {
            Write-Host "[route-tool] refusing to start: route-tool job(s) still running: $($active -join ', '). They all work on the same global database; wait for them or delete them after checking their logs (kubectl -n <ns> logs job/<name>)."
            return $contract.ExitFailed
        }
        if ([string]::IsNullOrWhiteSpace($Image)) {
            $live = Get-LiveDataServiceImage -Namespace $namespace
            $jobImage = $live.Image
            $jobPullPolicy = $live.PullPolicy
        }
        else {
            $jobImage = $Image
            $jobPullPolicy = Resolve-ImagePullPolicy -ImageRef $Image
        }
        $manifest = New-RouteToolJobManifest -TemplateText $templateText -RunId $runId -Image $jobImage -PullPolicy $jobPullPolicy `
            -Arguments $RouteToolArgs -TimeoutSeconds $TimeoutSeconds
    }
    catch {
        Write-Host "[route-tool] preflight failed, nothing was created: $($_.Exception.Message)"
        return $contract.ExitFailed
    }

    $report = if ([string]::IsNullOrWhiteSpace($ReportDir)) { Join-Path $RepoRoot (Join-Path 'run/route-tool' "$ZoneName-$runId") } else { $ReportDir }
    $manifestPath = Join-Path $report 'manifest.yaml'
    # apply 读的就是这份文件:建不了目录或写不了清单时什么都还没建,按本脚本自己的失败(2)返回。
    try {
        $null = New-Item -ItemType Directory -Force -Path $report
        [IO.File]::WriteAllText($manifestPath, $manifest, [Text.UTF8Encoding]::new($false))
    }
    catch {
        Write-Host "[route-tool] cannot write the report directory $report, nothing was created: $($_.Exception.Message)"
        return $contract.ExitFailed
    }
    $summary = [ordered]@{
        run_id = $runId; zone = $ZoneName; namespace = $namespace; job = $jobName; image = $jobImage
        subcommand = $subcommand; args = ($RouteToolArgs -join ' '); timeout_seconds = $TimeoutSeconds
        started_at = (Get-Date).ToString('o'); job_phase = ''; job_reason = ''; pod = ''; container_exit_code = ''
        verdict = ''; exit_code = ''; note = ''
    }
    $summaryPath = Join-Path $report 'summary.txt'
    Write-Host "[route-tool] run=$runId job=$jobName namespace=$namespace subcommand=$subcommand image=$jobImage report=$report"

    try {
        $null = Invoke-RouteToolKubectlChecked -Arguments @('apply', '-n', $namespace, '-f', $manifestPath) -What "apply Job $jobName"
    }
    catch {
        $summary.note = "apply failed: $($_.Exception.Message)"; $summary.exit_code = $contract.ExitFailed
        Save-RouteToolSummary -Path $summaryPath -Fields $summary
        Write-Host "[route-tool] $($summary.note)"
        return $contract.ExitFailed
    }

    # 有界轮询终态。单调时钟;查询失败只计入,不中断(下一轮再查),预算到了才判超时。
    $budget = [System.Diagnostics.Stopwatch]::StartNew()
    $budgetSeconds = $TimeoutSeconds + $contract.StatusGraceSeconds
    $lastProgress = 0
    $state = $null
    while ($true) {
        $state = Get-RouteToolJobState -Namespace $namespace -JobName $jobName
        if ($state.Phase -in @('complete', 'failed')) { break }
        if ($state.Phase -eq 'absent') { break }
        $elapsed = $budget.Elapsed.TotalSeconds
        if ($elapsed -ge $budgetSeconds) { break }
        if ($elapsed - $lastProgress -ge $contract.ProgressIntervalSeconds) {
            Write-Host ("[route-tool] waiting for job {0}: phase={1} elapsed={2:N0}s budget={3}s" -f $jobName, $state.Phase, $elapsed, $budgetSeconds)
            $lastProgress = $elapsed
        }
        Start-Sleep -Seconds ([Math]::Max(1, [Math]::Min($contract.PollIntervalSeconds, [int][Math]::Ceiling($budgetSeconds - $elapsed))))
    }
    $summary.job_phase = $state.Phase
    $summary.job_reason = $state.Reason
    if ($state.Phase -notin @('complete', 'failed')) {
        $summary.note = if ($state.Phase -eq 'absent') { 'job disappeared before reaching a terminal state' } else { "job did not reach a terminal state within ${budgetSeconds}s; it may still be running: do not start another run, check kubectl -n $namespace logs job/$jobName" }
        $summary.exit_code = $contract.ExitFailed
        Save-RouteToolSummary -Path $summaryPath -Fields $summary
        Write-Host "[route-tool] $($summary.note)"
        return $contract.ExitFailed
    }

    $pod = $null
    try {
        $pod = Get-RouteToolPodResult -Namespace $namespace -JobName $jobName -JobUid $state.Uid
    }
    catch {
        $pod = [pscustomobject]@{ PodName = ''; ExitCode = $null; Reason = $_.Exception.Message }
    }
    $summary.pod = $pod.PodName
    $summary.container_exit_code = if ($null -eq $pod.ExitCode) { '' } else { [string]$pod.ExitCode }

    # 日志:取不到只告警,判定仍按容器退出码。
    if (-not [string]::IsNullOrEmpty($pod.PodName)) {
        $logs = Invoke-RouteToolKubectl -Arguments @('logs', $pod.PodName, '-n', $namespace, '-c', $contract.ContainerName) -TimeoutSeconds $contract.LogsTimeoutSeconds
        if ($logs.TimedOut -or $logs.ExitCode -ne 0) {
            Write-Warning "[route-tool] could not fetch logs of pod $($pod.PodName): $(Get-RouteToolErrorTail $logs.ErrorOutput)"
            $summary.note = 'logs not collected'
        }
        else {
            $split = Split-RouteToolLog -Text ([string]$logs.Output) -RunId $runId
            $stdoutSaved = Save-RouteToolReportText -Path (Join-Path $report 'stdout.txt') -Text $split.Stdout
            $stderrSaved = Save-RouteToolReportText -Path (Join-Path $report 'stderr.txt') -Text $split.Stderr
            if (-not $split.MarkersFound) {
                $summary.note = 'stdout markers not found (process killed before the wrapper printed them); whole log saved as stderr.txt'
            }
            if (-not ($stdoutSaved -and $stderrSaved)) {
                $summary.note = (@($summary.note, 'logs not saved to the report directory (see warnings above)') | Where-Object { $_ }) -join '; '
            }
            if (-not [string]::IsNullOrWhiteSpace($split.Stdout)) { Write-Host "[route-tool] stdout: $($split.Stdout.TrimEnd())" }
            $tail = @(($split.Stderr.TrimEnd() -split "`n") | Select-Object -Last $contract.StderrTailLines)
            if ($tail.Count -gt 0 -and -not [string]::IsNullOrWhiteSpace($tail -join '')) {
                Write-Host "[route-tool] stderr (last $($tail.Count) lines; full text in stderr.txt):"
                foreach ($line in $tail) { Write-Host "  $line" }
            }
        }
    }

    $verdict = Resolve-RouteToolExitCode -ContainerExitCode $pod.ExitCode
    $summary.verdict = $verdict.Verdict
    $summary.exit_code = $verdict.ExitCode
    if ($null -eq $pod.ExitCode -and [string]::IsNullOrEmpty($summary.note)) { $summary.note = "no container exit code: $($pod.Reason)" }
    Save-RouteToolSummary -Path $summaryPath -Fields $summary
    Write-Host ("[route-tool] done: job_phase={0} container_exit={1} verdict={2} exit={3} report={4}" -f $state.Phase, $summary.container_exit_code, $verdict.Verdict, $verdict.ExitCode, $report)
    return $verdict.ExitCode
}

# ─── 入口 ───
# 本脚本自己的失败一律 2(文件头「退出码」)。Invoke-RouteTool 在 apply 前后的已知失败点都自己返回 2、Job 之后的落盘失败只告警;
# 这里兜的是没预料到的异常:pwsh -File 下未捕获异常的退出码是 1,与 route-tool 的「1 = 完成但需要人工处理」撞号。
try {
    $exitCode = Invoke-RouteTool
}
catch {
    Write-Host "[route-tool] internal error (exit 2): $($_.Exception.Message). If a 'run=... job=...' line was printed above, the Job may already exist: check it (kubectl get jobs --all-namespaces -l app=data-service-route-tool) before starting another run."
    $exitCode = (Get-RouteToolContract).ExitFailed
}
exit ([int]$exitCode)
