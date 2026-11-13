#requires -Version 7
<#
.SYNOPSIS
    在部署好的集群上实测 gRPC 口准入(NetworkPolicy)是否真的拦得住,以及声明的 Pod 间加密是否在每个节点上启用
    (docs/design/gate-online-identity.md §10.2、§10.3)。

.DESCRIPTION
    契约测试只证明 k8s_deploy.ps1 生成的策略对象写对了。拦不拦得住取决于集群的网络插件:不执行 NetworkPolicy 的插件
    (例如 kind 默认的 kindnet)照样接受策略对象,却什么都不拦。本脚本在集群上直接连一遍:

      1. 读出 k8s_deploy.ps1 生成的全部 gRPC 口准入策略(标签 mmorpg.io/managed-by=k8s_deploy.ps1、名字以 -grpc-allowlist 结尾),
         逐份解析出目标工作负载、gRPC 端口与放行名单。形状与生成器对不上就判失败,不猜。
      2. 每份策略在它选中的每种工作负载里挑一个 Running、Ready、有 IP 的 Pod 当靶子。
      3. 在独立的探测 namespace 里,为放行名单里出现过的每个 app 标签各起一个探测 Pod,再起一个不在任何名单里的
         (app=netpol-probe-unlisted)。策略按 app 标签放行、不限 namespace,所以探测 Pod 与真实调用方受同样的对待;
         放在独立 namespace 里,不会被任何 Service 选中,也不会混进业务流量。
      4. 先做两个对照,证明探测工具本身可靠:从探测 Pod 连 API server 必须连得上;连本机一个没人监听的口必须连不上。
      5. 逐份策略、逐个靶子:名单里的每个调用方都必须连得上 gRPC 口;名单之外的那一个必须连不上。
         名单里一个都连不上时判「不确定」(靶子没在监听,或策略写错),不拿「连不上」当成拦住了。
      6. 给了 -PodNetworkEncryption 时,另外只读核对 Pod 间加密:与部署前检同一份核对(lib/pod_network_encryption.ps1),
         网络插件的 WireGuard 开关打开,并且每个节点都有 WireGuard 公钥。
      7. 写报告(manifest.yaml / results.txt / summary.txt),删掉探测 namespace(-KeepProbeNamespace 保留)。

    对集群的写只有一处:在自己的探测 namespace 里建 Pod。其余全是读。探测 namespace 已存在而不是本脚本建的,拒绝运行。
    探测 Pod 带着真实调用方的 app 标签,跨 namespace 按 app 标签选 Pod 的运维操作(例如 kubectl get pods -A -l app=gate)会看到它们:
    在生产集群上跑之前确认没有这类操作在进行。

    -DryRun 只读集群、打印计划(策略、靶子、要做的连接),不建任何对象、不做连接;给了 -PodNetworkEncryption 时照样做加密核对(只读)。

    退出码:0 = 全部符合;2 = 有不符合、判不了,或脚本自己失败。参数绑定失败(pwsh -File 下缺 -KubeContext 等)是 pwsh 的 1。

.EXAMPLE
    # kind 上(装了 Calico 并打开 WireGuard,见 deploy/k8s/kind-netpol-config.yaml 文件头)
    pwsh -NoProfile -File tools/scripts/k8s_netpol_probe.ps1 -KubeContext kind-mmorpg-netpol -KubeConfig "$env:TEMP\kind-mmorpg-netpol.kubeconfig" -PodNetworkEncryption calico-wireguard
#>
param(
    # 必须显式给:本脚本会在集群里建 Pod,不允许悄悄落到本机默认 context 上。
    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$KubeContext,

    [string]$KubeConfig = "",

    # 与 k8s_deploy.ps1 -PodNetworkEncryption 同义。none = 不核对加密。
    [ValidateSet('none', 'cilium-wireguard', 'calico-wireguard')]
    [string]$PodNetworkEncryption = 'none',

    # 探测 Pod 所在的 namespace。本脚本建、本脚本删;已存在而不是本脚本建的就拒绝。
    [ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')]
    [string]$ProbeNamespace = 'mmorpg-netpol-probe',

    # 探测镜像:要有 sh 与支持 -z / -w 的 nc。缺省与 deploy/k8s/manifests/infra/mysql.yaml 同一个镜像。
    [string]$Image = 'busybox:1.36',

    # 单次连接的超时(秒)。被策略拦下的连接是丢包,要等满这个时间才回。
    [ValidateRange(1, 30)]
    [int]$ConnectTimeoutSeconds = 3,

    # 报告目录。留空 = <仓库>/run/netpol-probe/<运行号>(run/ 不入库)。
    [string]$ReportDir = "",

    [switch]$KeepProbeNamespace,

    [switch]$DryRun
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = [IO.Path]::GetFullPath((Join-Path $ScriptDir '../..'))
# Pod 间加密的核对项与判据,与 k8s_deploy.ps1 的部署前检共用。路径逐级 Join-Path:契约测试也跑在 Linux 上。
. (Join-Path (Join-Path $ScriptDir 'lib') 'pod_network_encryption.ps1')

# 本脚本与 k8s_deploy.ps1 生成的策略、探测 Pod 之间的全部字面量契约,集中在一处;tools/scripts/tests/k8s_netpol_probe.tests.ps1 逐项钉住。
function Get-NetpolProbeContract {
    return [pscustomobject]@{
        # k8s_deploy.ps1 New-GrpcAllowListNetworkPolicyYaml 给策略打的标签,与策略名的后缀。
        PolicySelector    = 'mmorpg.io/managed-by=k8s_deploy.ps1'
        PolicyNameSuffix  = '-grpc-allowlist'
        # 探测 namespace 与探测 Pod 上的标签:mmorpg.io/managed-by 认出「是本脚本建的」,mmorpg.io/netpol-probe 选出全部探测 Pod。
        ManagedByKey      = 'mmorpg.io/managed-by'
        ManagedByValue    = 'k8s_netpol_probe.ps1'
        ProbeLabelKey     = 'mmorpg.io/netpol-probe'
        # 名单之外的调用方。任何放行名单都不该出现这个标签。
        UnlistedApp       = 'netpol-probe-unlisted'
        ContainerName     = 'probe'
        PodNamePrefix     = 'probe-'
        # 探测命令把 nc 的退出码打成这一行(sh 包装,见 Get-NetpolProbeCommand)。
        ResultMarker      = '@@NETPOL_PROBE_EXIT='
        ExitPass          = 0
        ExitFailed        = 2
        # 单次 kubectl 调用的截止;exec 另加连接超时。
        CallTimeoutSeconds = 30
        ReadyTimeoutSeconds = 180
        NamespaceGoneTimeoutSeconds = 180
    }
}

# ─── 解析与计划(纯函数) ───

# 取一个 matchExpressions 选择器里 key=app、operator=In 的取值;形状不对回错误文本。
function Get-NetpolAppSelectorValues {
    param([AllowNull()]$Selector, [Parameter(Mandatory = $true)][string]$Where)
    if ($null -eq $Selector) { return [pscustomobject]@{ Values = @(); Error = "$Where has no podSelector" } }
    $expressions = @($Selector.matchExpressions)
    if ($expressions.Count -ne 1 -or $null -eq $expressions[0]) {
        return [pscustomobject]@{ Values = @(); Error = "$Where should have exactly one matchExpressions entry" }
    }
    $e = $expressions[0]
    if ([string]$e.key -cne 'app' -or [string]$e.operator -cne 'In') {
        return [pscustomobject]@{ Values = @(); Error = "$Where should select 'app In (...)', got '$($e.key) $($e.operator)'" }
    }
    $values = @($e.values | ForEach-Object { [string]$_ } | Where-Object { $_ })
    if ($values.Count -eq 0) { return [pscustomobject]@{ Values = @(); Error = "$Where selects no app" } }
    return [pscustomobject]@{ Values = [string[]]$values; Error = '' }
}

# 把一份策略(kubectl -o json 的一个 item)解析成 { Namespace; Name; TargetApps; GrpcPort; AllowedApps; Error }。
# 只认 k8s_deploy.ps1 生成的形状:选中目标的 app In (...),恰好一条带 from 的入站规则(任意 namespace + app In (...),一个 TCP 端口),
# 其余入站规则不带 from(对所有来源开放的其它端口,这里不测)。对不上时 Error 非空,不猜。
function ConvertFrom-NetpolAllowListPolicy {
    param([Parameter(Mandatory = $true)]$Policy)
    $ns = [string]$Policy.metadata.namespace
    $name = [string]$Policy.metadata.name
    $result = [pscustomobject]@{ Namespace = $ns; Name = $name; TargetApps = [string[]]@(); GrpcPort = 0; AllowedApps = [string[]]@(); Error = '' }
    $where = "policy $ns/$name"

    $target = Get-NetpolAppSelectorValues -Selector $Policy.spec.podSelector -Where "$where podSelector"
    if ($target.Error) { $result.Error = $target.Error; return $result }
    $result.TargetApps = $target.Values

    $restricted = @(@($Policy.spec.ingress) | Where-Object { $null -ne $_ -and $null -ne $_.PSObject.Properties['from'] -and @($_.from).Count -gt 0 })
    if ($restricted.Count -ne 1) {
        $result.Error = "$where should have exactly one ingress rule with 'from' (the gRPC rule), got $($restricted.Count)"
        return $result
    }
    $rule = $restricted[0]
    $peers = @($rule.from)
    if ($peers.Count -ne 1 -or $null -eq $peers[0].PSObject.Properties['namespaceSelector']) {
        $result.Error = "$where gRPC rule should have exactly one peer with a namespaceSelector"
        return $result
    }
    $allowed = Get-NetpolAppSelectorValues -Selector $peers[0].podSelector -Where "$where gRPC rule peer"
    if ($allowed.Error) { $result.Error = $allowed.Error; return $result }
    $result.AllowedApps = $allowed.Values

    $ports = @($rule.ports)
    if ($ports.Count -ne 1 -or [string]$ports[0].protocol -cne 'TCP') {
        $result.Error = "$where gRPC rule should open exactly one TCP port"
        return $result
    }
    $port = 0
    if (-not [int]::TryParse([string]$ports[0].port, [ref]$port) -or $port -lt 1 -or $port -gt 65535) {
        $result.Error = "$where gRPC rule port '$($ports[0].port)' is not a port number"
        return $result
    }
    $result.GrpcPort = $port
    return $result
}

# 一种工作负载的靶子:namespace 里第一个(按名字)Running、Ready、没在删除、有 IPv4 地址的 Pod。没有回 $null。
function Select-NetpolProbeTargetPod {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Pods,
        [Parameter(Mandatory = $true)][string]$App
    )
    $candidates = @($Pods | Where-Object {
            $null -ne $_ -and [string]$_.metadata.labels.app -ceq $App -and $null -eq $_.metadata.deletionTimestamp -and
            [string]$_.status.phase -ceq 'Running' -and [string]$_.status.podIP -match '^\d{1,3}(\.\d{1,3}){3}$' -and
            @($_.status.conditions | Where-Object { [string]$_.type -ceq 'Ready' -and [string]$_.status -ceq 'True' }).Count -gt 0
        } | Sort-Object { [string]$_.metadata.name })
    if ($candidates.Count -eq 0) { return $null }
    return [pscustomobject]@{ App = $App; Pod = [string]$candidates[0].metadata.name; Ip = [string]$candidates[0].status.podIP }
}

# 由解析好的策略与各 namespace 的 Pod 列表排出全部连接检查。PodsByNamespace:namespace → Pod 对象数组。
# 回 { Checks; Problems(排计划时就发现的问题,算失败); Skipped(没部署的目标,只提示); Callers(要起探测 Pod 的 app 标签) }。
#
# 目标工作负载一个 Pod 都没有 = 没部署,跳过并提示:放行名单两侧都带着金丝雀标签(<名字>-canary),没起金丝雀副本时它们就是这样。
# 有 Pod 却没有一个 Running 且 Ready = 判不了,算失败。一份策略的目标全都没部署也算失败:这份策略一次都没测到。
function New-NetpolProbePlan {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Policies,
        [Parameter(Mandatory = $true)][hashtable]$PodsByNamespace
    )
    $contract = Get-NetpolProbeContract
    $checks = [System.Collections.Generic.List[object]]::new()
    $problems = [System.Collections.Generic.List[string]]::new()
    $skipped = [System.Collections.Generic.List[string]]::new()
    $callers = [System.Collections.Generic.SortedSet[string]]::new([StringComparer]::Ordinal)
    $null = $callers.Add($contract.UnlistedApp)
    if ($Policies.Count -eq 0) {
        $problems.Add("no gRPC allowlist policies found (label $($contract.PolicySelector), name *$($contract.PolicyNameSuffix)): deploy with -NetworkPolicies on first")
    }
    foreach ($policy in $Policies) {
        $id = "$($policy.Namespace)/$($policy.Name)"
        if ($policy.Error) { $problems.Add($policy.Error); continue }
        if ($policy.AllowedApps -ccontains $contract.UnlistedApp) {
            $problems.Add("policy $id allows '$($contract.UnlistedApp)', the label this probe reserves for the unlisted caller")
            continue
        }
        $pods = @()
        if ($PodsByNamespace.ContainsKey($policy.Namespace)) { $pods = @($PodsByNamespace[$policy.Namespace]) }
        $probed = 0
        $unprobable = 0
        foreach ($app in $policy.TargetApps) {
            $deployed = @($pods | Where-Object { $null -ne $_ -and [string]$_.metadata.labels.app -ceq $app }).Count
            if ($deployed -eq 0) {
                $skipped.Add("policy ${id}: no pod with app=$app in namespace $($policy.Namespace) (not deployed), skipped")
                continue
            }
            $target = Select-NetpolProbeTargetPod -Pods $pods -App $app
            if ($null -eq $target) {
                $problems.Add("policy ${id}: $deployed pod(s) with app=$app in namespace $($policy.Namespace) but none is running and ready, so it cannot be probed")
                $unprobable++
                continue
            }
            $probed++
            foreach ($caller in $policy.AllowedApps) {
                $null = $callers.Add($caller)
                $checks.Add([pscustomobject]@{ Policy = $id; TargetApp = $app; TargetPod = $target.Pod; Ip = $target.Ip; Port = $policy.GrpcPort; Caller = $caller; Expect = 'connect' })
            }
            $checks.Add([pscustomobject]@{ Policy = $id; TargetApp = $app; TargetPod = $target.Pod; Ip = $target.Ip; Port = $policy.GrpcPort; Caller = $contract.UnlistedApp; Expect = 'blocked' })
        }
        # 目标全都没部署:每一种都只记了「跳过」,这份策略一次都没测到,要算失败(有目标判不了时上面已经记过失败)。
        if ($probed -eq 0 -and $unprobable -eq 0) {
            $problems.Add("policy ${id}: none of its target workloads ($($policy.TargetApps -join ', ')) has a pod in namespace $($policy.Namespace), so the policy was not probed at all")
        }
    }
    return [pscustomobject]@{ Checks = $checks.ToArray(); Problems = [string[]]$problems.ToArray(); Skipped = [string[]]$skipped.ToArray(); Callers = [string[]]@($callers) }
}

# 探测 Pod 的名字:前缀 + app 标签。app 标签只含小写字母、数字与 '-',拼出来仍是合法的 DNS-1123 标签。
function Get-NetpolProbePodName {
    param([Parameter(Mandatory = $true)][string]$App)
    $contract = Get-NetpolProbeContract
    if ($App -cnotmatch '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') { throw "app label '$App' is not a DNS-1123 label" }
    $name = $contract.PodNamePrefix + $App
    if ($name.Length -gt 63) { throw "probe pod name '$name' is longer than 63 characters" }
    return $name
}

# 探测 namespace 与探测 Pod 的清单。Pod 按受限的 Pod 安全标准写(非 root、只读根文件系统、去掉全部能力),
# 不挂服务账号令牌;只跑 sleep,连接由 kubectl exec 发起。
function New-NetpolProbeManifest {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$Image,
        [Parameter(Mandatory = $true)][string[]]$Callers
    )
    $contract = Get-NetpolProbeContract
    if ([string]::IsNullOrWhiteSpace($Image) -or $Image -match '[\s\x00-\x1F\x7F]') { throw "probe image reference is empty or contains whitespace / control characters" }
    $docs = [System.Collections.Generic.List[string]]::new()
    $docs.Add((@(
                'apiVersion: v1',
                'kind: Namespace',
                'metadata:',
                "  name: $Namespace",
                '  labels:',
                "    $($contract.ManagedByKey): $($contract.ManagedByValue)"
            ) -join "`n"))
    foreach ($caller in $Callers) {
        $docs.Add((@(
                    'apiVersion: v1',
                    'kind: Pod',
                    'metadata:',
                    "  name: $(Get-NetpolProbePodName -App $caller)",
                    "  namespace: $Namespace",
                    '  labels:',
                    "    app: $caller",
                    "    $($contract.ManagedByKey): $($contract.ManagedByValue)",
                    "    $($contract.ProbeLabelKey): `"1`"",
                    'spec:',
                    '  automountServiceAccountToken: false',
                    '  restartPolicy: Never',
                    '  terminationGracePeriodSeconds: 0',
                    '  securityContext:',
                    '    runAsNonRoot: true',
                    '    runAsUser: 65534',
                    '    runAsGroup: 65534',
                    '    seccompProfile:',
                    '      type: RuntimeDefault',
                    '  containers:',
                    "    - name: $($contract.ContainerName)",
                    "      image: $(ConvertTo-Json -InputObject $Image -Compress)",
                    '      imagePullPolicy: IfNotPresent',
                    '      command: ["sleep", "3600"]',
                    '      securityContext:',
                    '        allowPrivilegeEscalation: false',
                    '        readOnlyRootFilesystem: true',
                    '        capabilities:',
                    '          drop: ["ALL"]',
                    '      resources:',
                    '        requests:',
                    '          cpu: 10m',
                    '          memory: 16Mi',
                    '        limits:',
                    '          cpu: 100m',
                    '          memory: 32Mi'
                ) -join "`n"))
    }
    return ($docs -join "`n---`n") + "`n"
}

# 探测命令:nc 只做 TCP 握手(-z),-w 秒没连上就放弃;退出码打成标记行。kubectl exec 自己失败时没有标记行,据此与「连不上」分开。
# Target 为 'api-server' / 'closed-port' 时是两个对照;否则 Ip / Port 必须是合法的 IPv4 与端口(拼进 sh -c,不接受任意文本)。
function Get-NetpolProbeCommand {
    param(
        [Parameter(Mandatory = $true)][ValidateRange(1, 30)][int]$TimeoutSeconds,
        [string]$Ip = '',
        [int]$Port = 0,
        [ValidateSet('pod', 'api-server', 'closed-port')][string]$Target = 'pod'
    )
    $contract = Get-NetpolProbeContract
    $endpoint = switch ($Target) {
        # 每个 Pod 都有这两个环境变量(不挂令牌也有),指向 API server 的 Service。
        'api-server' { '"$KUBERNETES_SERVICE_HOST" "$KUBERNETES_SERVICE_PORT"' }
        # 本机 1 号口没人监听:nc 必须报连不上。
        'closed-port' { '127.0.0.1 1' }
        default {
            if ($Ip -notmatch '^\d{1,3}(\.\d{1,3}){3}$') { throw "target IP '$Ip' is not an IPv4 address" }
            if ($Port -lt 1 -or $Port -gt 65535) { throw "target port $Port is out of range" }
            "$Ip $Port"
        }
    }
    return [string[]]@('sh', '-c', ('nc -z -w {0} {1} 2>&1; echo "{2}$?"' -f $TimeoutSeconds, $endpoint, $contract.ResultMarker))
}

# 从 kubectl exec 的输出取结果:connected / not-connected;没有标记行(exec 本身失败、输出被截断)是 error。
function Get-NetpolProbeOutcome {
    param([AllowNull()][AllowEmptyString()][string]$Output)
    $contract = Get-NetpolProbeContract
    $found = @([regex]::Matches([string]$Output, [regex]::Escape($contract.ResultMarker) + '(?<code>\d+)'))
    if ($found.Count -eq 0) { return 'error' }
    if ($found[$found.Count - 1].Groups['code'].Value -eq '0') { return 'connected' }
    return 'not-connected'
}

# 逐条判定。Results:检查 + Outcome(connected / not-connected / error)+ Detail。名单之外那一条的「连不上」只有在同一个靶子上
# 至少一个名单里的调用方连上了才算拦住了:否则靶子可能根本没在监听,判不确定。
# 回 { Rows(每条检查一行); Failures(失败行与计划阶段的问题); ExitCode }。
function Resolve-NetpolProbeVerdict {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Results,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][string[]]$PlanProblems
    )
    $contract = Get-NetpolProbeContract
    $reachable = @{}
    foreach ($r in $Results) {
        if ($r.Expect -ceq 'connect' -and $r.Outcome -ceq 'connected') { $reachable["$($r.Policy)|$($r.TargetPod)"] = $true }
    }
    $rows = [System.Collections.Generic.List[object]]::new()
    $failures = [System.Collections.Generic.List[string]]::new()
    foreach ($problem in $PlanProblems) { $failures.Add($problem) }
    foreach ($r in $Results) {
        $where = "$($r.Policy) $($r.Caller) -> $($r.TargetPod) $($r.Ip):$($r.Port)"
        $verdict = 'PASS'
        $reason = ''
        if ($r.Outcome -ceq 'error') {
            $verdict = 'FAIL'; $reason = 'probe did not report a result (kubectl exec failed)'
        }
        elseif ($r.Expect -ceq 'connect' -and $r.Outcome -cne 'connected') {
            $verdict = 'FAIL'; $reason = 'listed caller could not connect (policy blocks it, or the target is not listening)'
        }
        elseif ($r.Expect -ceq 'blocked' -and $r.Outcome -ceq 'connected') {
            $verdict = 'FAIL'; $reason = 'unlisted caller connected: the network plugin does not enforce NetworkPolicy, or the policy is not applied'
        }
        elseif ($r.Expect -ceq 'blocked' -and -not $reachable.ContainsKey("$($r.Policy)|$($r.TargetPod)")) {
            $verdict = 'FAIL'; $reason = 'inconclusive: no listed caller reached this target either, so "not connected" does not prove the policy blocks'
        }
        $rows.Add([pscustomobject]@{ Verdict = $verdict; Policy = $r.Policy; Caller = $r.Caller; Expect = $r.Expect; Outcome = $r.Outcome; Target = "$($r.TargetPod) $($r.Ip):$($r.Port)"; Reason = $reason; Detail = [string]$r.Detail })
        if ($verdict -ceq 'FAIL') { $failures.Add("$where expected $($r.Expect), got $($r.Outcome): $reason") }
    }
    $exit = if ($failures.Count -eq 0) { $contract.ExitPass } else { $contract.ExitFailed }
    return [pscustomobject]@{ Rows = $rows.ToArray(); Failures = [string[]]$failures.ToArray(); ExitCode = $exit }
}

# 两个对照:连 API server 必须连上;连没人监听的口必须连不上。回问题文本(空 = 探测工具可靠)。
function Test-NetpolProbeControls {
    param(
        [Parameter(Mandatory = $true)][string]$ApiServerOutcome,
        [Parameter(Mandatory = $true)][string]$ClosedPortOutcome
    )
    $problems = [System.Collections.Generic.List[string]]::new()
    if ($ApiServerOutcome -cne 'connected') {
        $problems.Add("control failed: the probe pod could not open a TCP connection to the API server ($ApiServerOutcome); check the probe image (needs sh and nc with -z / -w) and egress from the probe namespace")
    }
    if ($ClosedPortOutcome -cne 'not-connected') {
        $problems.Add("control failed: connecting to a closed local port reported '$ClosedPortOutcome'; the probe tool cannot tell blocked from allowed")
    }
    return [string[]]$problems.ToArray()
}

# 运行号:yyyyMMdd-HHmmss-<4 位十六进制>,进报告目录名。时间与随机数显式传入,便于测试。
function New-NetpolProbeRunId {
    param(
        [Parameter(Mandatory = $true)][datetime]$Now,
        [Parameter(Mandatory = $true)][ValidateRange(0, 65535)][int]$Random
    )
    return ('{0}-{1:x4}' -f $Now.ToString('yyyyMMdd-HHmmss', [Globalization.CultureInfo]::InvariantCulture), $Random)
}

# ─── kubectl 调用面 ───

# 起外部进程并在截止时硬杀(与 k8s_route_tool.ps1 的 Invoke-RouteToolProcess 同一做法;那边是入口脚本,不能 dot-source 复用)。
# 参数逐项进 ArgumentList,不经 shell 拼接。
function Invoke-NetpolProbeProcess {
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
            try { $process.Kill($true) } catch { }
            $null = $process.WaitForExit(1000)
            return [pscustomobject]@{ ExitCode = -1; Stdout = ''; Stderr = "process did not exit within ${timeoutMs}ms"; TimedOut = $true }
        }
        if (-not [System.Threading.Tasks.Task]::WaitAll([System.Threading.Tasks.Task[]]@($stdout, $stderr), 1000)) {
            return [pscustomobject]@{ ExitCode = -1; Stdout = ''; Stderr = 'process output pipes did not close'; TimedOut = $true }
        }
        return [pscustomobject]@{ ExitCode = $process.ExitCode; Stdout = $stdout.Result; Stderr = $stderr.Result; TimedOut = $false }
    }
    catch {
        return [pscustomobject]@{ ExitCode = -1; Stdout = ''; Stderr = $_.Exception.Message; TimedOut = $false }
    }
    finally {
        $process.Dispose()
    }
}

function Invoke-NetpolProbeKubectl {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [ValidateRange(0.001, 2147483)][double]$TimeoutSeconds = 30
    )
    $kubectl = Get-Command kubectl -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $kubectl) {
        return [pscustomobject]@{ ExitCode = -1; Stdout = ''; Stderr = 'kubectl not found'; TimedOut = $false }
    }
    $timeoutMs = [int][Math]::Ceiling($TimeoutSeconds * 1000)
    $base = @('--context', $KubeContext)
    if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) { $base += @('--kubeconfig', $KubeConfig) }
    $allArgs = $base + @("--request-timeout=${timeoutMs}ms") + $Arguments
    return Invoke-NetpolProbeProcess -FilePath $kubectl.Source -ArgumentList $allArgs -TimeoutSeconds ($TimeoutSeconds + 5)
}

# 必须成功的 kubectl 调用:失败就 throw,带上 stderr 的末尾。
function Invoke-NetpolProbeKubectlChecked {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [ValidateRange(0.001, 2147483)][double]$TimeoutSeconds = 30,
        [Parameter(Mandatory = $true)][string]$What
    )
    $result = Invoke-NetpolProbeKubectl -Arguments $Arguments -TimeoutSeconds $TimeoutSeconds
    if ($result.ExitCode -ne 0) {
        $tail = (([string]$result.Stderr).Trim() -split "`n" | Select-Object -Last 5) -join ' | '
        throw "kubectl failed while trying to ${What} (exit $($result.ExitCode)): $tail"
    }
    return $result
}

# 在探测 Pod 里跑一次探测命令。
function Invoke-NetpolProbeExec {
    param(
        [Parameter(Mandatory = $true)][string]$Caller,
        [Parameter(Mandatory = $true)][string[]]$Command
    )
    $contract = Get-NetpolProbeContract
    $arguments = @('exec', '-n', $ProbeNamespace, (Get-NetpolProbePodName -App $Caller), '-c', $contract.ContainerName, '--') + $Command
    $result = Invoke-NetpolProbeKubectl -Arguments $arguments -TimeoutSeconds ($contract.CallTimeoutSeconds + $ConnectTimeoutSeconds)
    $text = ([string]$result.Stdout + "`n" + [string]$result.Stderr).Trim()
    return [pscustomobject]@{ Outcome = (Get-NetpolProbeOutcome -Output ([string]$result.Stdout)); Detail = ($text -replace '\s+', ' ') }
}

# 探测 namespace 不存在就直接用;是本脚本上次留下的就先删掉、等它消失;别人的就拒绝。
function Initialize-NetpolProbeNamespace {
    $contract = Get-NetpolProbeContract
    $existing = Invoke-NetpolProbeKubectl -Arguments @('get', 'namespace', $ProbeNamespace, '-o', 'json', '--ignore-not-found')
    if ($existing.ExitCode -ne 0) { throw "could not check namespace ${ProbeNamespace}: $(([string]$existing.Stderr).Trim())" }
    if ([string]::IsNullOrWhiteSpace($existing.Stdout)) { return }
    $ns = $existing.Stdout | ConvertFrom-Json
    $owner = $null
    if ($null -ne $ns.metadata.labels) { $owner = $ns.metadata.labels.PSObject.Properties[$contract.ManagedByKey] }
    if ($null -eq $owner -or [string]$owner.Value -cne $contract.ManagedByValue) {
        throw "namespace $ProbeNamespace already exists and was not created by this probe (label $($contract.ManagedByKey)=$($contract.ManagedByValue) missing); pass another -ProbeNamespace"
    }
    Write-Host "[netpol-probe] removing namespace $ProbeNamespace left by an earlier run"
    $null = Invoke-NetpolProbeKubectlChecked -Arguments @('delete', 'namespace', $ProbeNamespace, '--wait=true', "--timeout=$($contract.NamespaceGoneTimeoutSeconds)s") `
        -TimeoutSeconds ($contract.NamespaceGoneTimeoutSeconds + 10) -What "delete the leftover namespace $ProbeNamespace"
}

function Save-NetpolProbeText {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    try { [IO.File]::WriteAllText($Path, $Text, [System.Text.UTF8Encoding]::new($false)) }
    catch { Write-Warning "[netpol-probe] could not write ${Path}: $($_.Exception.Message)" }
}

function Format-NetpolProbeRows {
    param([Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Rows)
    $lines = [System.Collections.Generic.List[string]]::new()
    $lines.Add("verdict`tpolicy`tcaller`texpect`toutcome`ttarget`treason`tdetail")
    foreach ($row in $Rows) {
        $lines.Add(("{0}`t{1}`t{2}`t{3}`t{4}`t{5}`t{6}`t{7}" -f $row.Verdict, $row.Policy, $row.Caller, $row.Expect, $row.Outcome, $row.Target, $row.Reason, $row.Detail))
    }
    return ($lines -join "`n") + "`n"
}

# ─── 主流程 ───

function Invoke-NetpolProbe {
    $contract = Get-NetpolProbeContract
    $runId = New-NetpolProbeRunId -Now (Get-Date) -Random (Get-Random -Minimum 0 -Maximum 65536)
    $report = if ([string]::IsNullOrWhiteSpace($ReportDir)) { Join-Path (Join-Path (Join-Path $RepoRoot 'run') 'netpol-probe') $runId } else { $ReportDir }
    Write-Host "[netpol-probe] run=$runId context=$KubeContext probe_namespace=$ProbeNamespace dry_run=$([bool]$DryRun)"

    # 1. 策略。
    $raw = Invoke-NetpolProbeKubectlChecked -Arguments @('get', 'networkpolicies', '--all-namespaces', '-l', $contract.PolicySelector, '-o', 'json') -What 'list the gRPC allowlist policies'
    $items = ConvertFrom-KubeListJson -Json ([string]$raw.Stdout) -What 'network policy list'
    $policies = @($items | Where-Object { ([string]$_.metadata.name).EndsWith($contract.PolicyNameSuffix, [StringComparison]::Ordinal) } |
            Sort-Object { "$($_.metadata.namespace)/$($_.metadata.name)" } | ForEach-Object { ConvertFrom-NetpolAllowListPolicy -Policy $_ })

    # 2. 靶子。每个 namespace 取一次 Pod 列表。
    $podsByNamespace = @{}
    foreach ($ns in @($policies | Where-Object { -not $_.Error } | ForEach-Object { $_.Namespace } | Sort-Object -Unique)) {
        $pods = Invoke-NetpolProbeKubectlChecked -Arguments @('get', 'pods', '-n', $ns, '-o', 'json') -What "list the pods in namespace $ns"
        $podsByNamespace[$ns] = ConvertFrom-KubeListJson -Json ([string]$pods.Stdout) -What "pod list of namespace $ns"
    }
    $plan = New-NetpolProbePlan -Policies $policies -PodsByNamespace $podsByNamespace
    foreach ($policy in $policies) {
        if ($policy.Error) { continue }
        Write-Host ("[netpol-probe] policy {0}/{1}: targets [{2}] grpc port {3} allows [{4}]" -f $policy.Namespace, $policy.Name, ($policy.TargetApps -join ', '), $policy.GrpcPort, ($policy.AllowedApps -join ', '))
    }
    Write-Host "[netpol-probe] planned $($plan.Checks.Count) connection checks from $($plan.Callers.Count) probe pods"
    foreach ($note in $plan.Skipped) { Write-Host "[netpol-probe] note: $note" }
    foreach ($problem in $plan.Problems) { Write-Host "[netpol-probe] PLAN PROBLEM: $problem" }

    # 3. 加密(只读)。
    $encryptionProblems = @()
    if ($PodNetworkEncryption -cne 'none') {
        $encryptionProblems = @(Get-PodNetworkEncryptionClusterProblems -Mode $PodNetworkEncryption -InvokeKubectl {
                param([string[]]$KubectlArgs)
                Invoke-NetpolProbeKubectl -Arguments $KubectlArgs
            })
        if ($encryptionProblems.Count -eq 0) { Write-Host "[netpol-probe] encryption ${PodNetworkEncryption}: switch on, every node has a WireGuard public key" }
        foreach ($problem in $encryptionProblems) { Write-Host "[netpol-probe] ENCRYPTION PROBLEM: $problem" }
    }

    if ($DryRun) {
        foreach ($check in $plan.Checks) {
            Write-Host ("[netpol-probe] [dry-run] {0}: {1} -> {2} {3}:{4} expect {5}" -f $check.Policy, $check.Caller, $check.TargetPod, $check.Ip, $check.Port, $check.Expect)
        }
        $ok = $plan.Problems.Count -eq 0 -and $encryptionProblems.Count -eq 0
        Write-Host "[netpol-probe] dry run done: nothing was created; plan_problems=$($plan.Problems.Count) encryption_problems=$($encryptionProblems.Count)"
        return $(if ($ok) { $contract.ExitPass } else { $contract.ExitFailed })
    }

    # 4. 探测 Pod。
    $null = New-Item -ItemType Directory -Force -Path $report
    $manifestPath = Join-Path $report 'manifest.yaml'
    Save-NetpolProbeText -Path $manifestPath -Text (New-NetpolProbeManifest -Namespace $ProbeNamespace -Image $Image -Callers $plan.Callers)
    $results = [System.Collections.Generic.List[object]]::new()
    $controlProblems = @()
    # 只删自己建的:探测 namespace 是别人的时 Initialize 会拒绝,这时 finally 不能去删它。
    $ownsNamespace = $false
    try {
        Initialize-NetpolProbeNamespace
        $ownsNamespace = $true
        $null = Invoke-NetpolProbeKubectlChecked -Arguments @('apply', '-f', $manifestPath) -What 'create the probe pods'
        $null = Invoke-NetpolProbeKubectlChecked -Arguments @('wait', '--for=condition=Ready', 'pod', '-l', "$($contract.ProbeLabelKey)=1", '-n', $ProbeNamespace, "--timeout=$($contract.ReadyTimeoutSeconds)s") `
            -TimeoutSeconds ($contract.ReadyTimeoutSeconds + 10) -What 'wait for the probe pods to become ready'

        # 5. 对照,从名单之外的那个探测 Pod 发起。
        $api = Invoke-NetpolProbeExec -Caller $contract.UnlistedApp -Command (Get-NetpolProbeCommand -TimeoutSeconds $ConnectTimeoutSeconds -Target 'api-server')
        $closed = Invoke-NetpolProbeExec -Caller $contract.UnlistedApp -Command (Get-NetpolProbeCommand -TimeoutSeconds $ConnectTimeoutSeconds -Target 'closed-port')
        $controlProblems = @(Test-NetpolProbeControls -ApiServerOutcome $api.Outcome -ClosedPortOutcome $closed.Outcome)
        foreach ($problem in $controlProblems) { Write-Host "[netpol-probe] $problem (api: $($api.Detail); closed: $($closed.Detail))" }

        # 6. 连接检查。对照不过时探测结果不可信,不再做。
        if ($controlProblems.Count -eq 0) {
            foreach ($check in $plan.Checks) {
                $probe = Invoke-NetpolProbeExec -Caller $check.Caller -Command (Get-NetpolProbeCommand -TimeoutSeconds $ConnectTimeoutSeconds -Ip $check.Ip -Port $check.Port)
                $results.Add([pscustomobject]@{ Policy = $check.Policy; TargetApp = $check.TargetApp; TargetPod = $check.TargetPod; Ip = $check.Ip; Port = $check.Port; Caller = $check.Caller; Expect = $check.Expect; Outcome = $probe.Outcome; Detail = $probe.Detail })
                Write-Host ("[netpol-probe] {0}: {1} -> {2} {3}:{4} expect {5}, {6}" -f $check.Policy, $check.Caller, $check.TargetPod, $check.Ip, $check.Port, $check.Expect, $probe.Outcome)
            }
        }
    }
    finally {
        if (-not $ownsNamespace) {
            # 没走到建 namespace 那一步(别人的 namespace、读不到集群):什么都没建,也就什么都不删。
        }
        elseif ($KeepProbeNamespace) {
            Write-Host "[netpol-probe] keeping namespace $ProbeNamespace (-KeepProbeNamespace)"
        }
        else {
            $gone = Invoke-NetpolProbeKubectl -Arguments @('delete', 'namespace', $ProbeNamespace, '--wait=false', '--ignore-not-found')
            if ($gone.ExitCode -ne 0) { Write-Warning "[netpol-probe] could not delete namespace ${ProbeNamespace}: $(([string]$gone.Stderr).Trim())" }
        }
    }

    # 7. 判定与报告。
    $verdict = Resolve-NetpolProbeVerdict -Results $results.ToArray() -PlanProblems ([string[]]@($plan.Problems + $controlProblems + $encryptionProblems))
    Save-NetpolProbeText -Path (Join-Path $report 'results.txt') -Text (Format-NetpolProbeRows -Rows $verdict.Rows)
    $summary = @(
        "run=$runId",
        "context=$KubeContext",
        "pod_network_encryption=$PodNetworkEncryption",
        "policies=$(@($policies).Count)",
        "checks=$(@($verdict.Rows).Count)",
        "failures=$($verdict.Failures.Count)",
        "exit=$($verdict.ExitCode)"
    ) + @($verdict.Failures | ForEach-Object { "failure: $_" })
    Save-NetpolProbeText -Path (Join-Path $report 'summary.txt') -Text (($summary -join "`n") + "`n")
    foreach ($failure in $verdict.Failures) { Write-Host "[netpol-probe] FAIL: $failure" }
    Write-Host ("[netpol-probe] done: checks={0} failures={1} exit={2} report={3}" -f @($verdict.Rows).Count, $verdict.Failures.Count, $verdict.ExitCode, $report)
    return $verdict.ExitCode
}

# ─── 入口 ───
# 本脚本自己的失败一律 2(文件头「退出码」):pwsh -File 下未捕获异常的退出码是 1,这里兜住。
try {
    $exitCode = Invoke-NetpolProbe
}
catch {
    Write-Host "[netpol-probe] error (exit 2): $($_.Exception.Message). If probe pods were created, delete them with: kubectl --context $KubeContext delete namespace $ProbeNamespace"
    $exitCode = (Get-NetpolProbeContract).ExitFailed
}
exit ([int]$exitCode)
