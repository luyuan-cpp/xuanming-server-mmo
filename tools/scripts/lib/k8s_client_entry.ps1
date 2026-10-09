#requires -Version 7
<#
.SYNOPSIS
	集群外客户端入口(D76–D93)的部署生成器库:gate StatefulSet + 每序号 Service、battle Fleet /
	hostPort Deployment、gateway Ingress、preflight 与 Fleet 就绪等待。

.DESCRIPTION
	由 tools/scripts/k8s_deploy.ps1 dot-source(接入由对齐包完成),契约测试
	tools/scripts/tests/k8s_client_entry_contract.tests.ps1 直接 dot-source 本文件。
	设计与模式矩阵见 docs/design/k8s-client-entry.md(ingress_final D80 / D81 / D86–D91)。

	约定:
	  1. **不读任何脚本级变量**。k8s_deploy.ps1 的 $NodeImage / $GrpcServerMaxPollers / 日志 sidecar
	     等一律经参数传入(公共容器参数先用 New-ClientEntryPodCommon 打包),本文件可以单独 dot-source 测试。
	  2. 生成函数是纯函数:只拼 YAML 字符串,不调 kubectl、不写文件、不打日志;返回值一律是 LF 换行(本文件按
	     .gitattributes 以 CRLF 检出,here-string 会带 CR,返回前经 ConvertTo-ClientEntryLf 统一)。YAML 一律用**空格**缩进
	     (不依赖 Invoke-KubectlWithInputFile 的制表符换算),列位置与 New-NodeDeploymentYaml(换算后)
	     和 New-SceneFleetYaml 一致:apps/v1 的 pod spec 键在第 6 列、列表项第 8 列;Fleet 的在第 10 / 12 列。
	  3. **kubectl 调用约定**:凡是碰集群的函数都把 -KubeContext / -KubeConfig 声明为**必填**
	     ([Parameter(Mandatory)][AllowEmptyString()]):每个调用点都必须显式写出这两个参数(可以是空串,语义同
	     k8s_deploy.ps1 的 Build-KubectlBaseArgs:非空才追加 --context / --kubeconfig),漏传一处就不会悄悄
	     落到本机默认 current-context(属于别的项目,2b §5 要求 fail-closed 指定集群)。支持 -DryRun。
	     测试可以定义同名 function kubectl 覆盖可执行文件(PowerShell 解析顺序:函数优先)。
	  4. D92:新生成器只用 CLIENT_ENDPOINT_* / HOST_IP / POD_NAME,绝不复用 D92 列出的两个集群内身份 env。
	     C++ 的 TryResolveNodePortFromEnv 以 RPC_PORT 优先,新模板只写 RPC_PORT,行为与旧模板一致。
	  5. 跨语言 / 跨脚本字符串契约集中在 Get-ClientEntryContract(含 CLIENT_ENDPOINT_* env 名与 SOURCE 取值、gate 生成器
	     内部 env 名),本文件与 k8s_deploy.ps1 只引用、不另写字面量;改名必须与 C++ 常量同改。
	  6. **不设 StrictMode**:dot-source 时顶层 Set-StrictMode 会改掉调用方作用域的严格模式(同 artifacts_lib.ps1)。
	     本库代码不依赖宽松语义:kubectl JSON 里可能缺席的字段、调用方传入的 zone 记录一律经 Get-ClientEntryMember 读取。
	  7. **K8s 枚举取值区分大小写**:PowerShell 的 ValidateSet / -in / -eq 都不分大小写、也不改写取值,
	     写进 YAML 的枚举(Service type、externalTrafficPolicy、imagePullPolicy)先经
	     ConvertTo-ClientEntryCanonicalName 规范成标准写法。
	  8. **模式参数不粘滞**:-ClientEntryMode / -BattleOrchestrator 漏传就落回默认值 podip / deployment。
	     删除"另一种形态"时,凡是会踢人的条目(KicksPlayers)在集群里确实存在、调用方又没给
	     -AllowDisruptiveSwitch,Remove-ClientEntryObsoleteResources 一律 throw、一个都不删(AGENTS §11.3 fail-closed)。

	导出函数(参数均为显式传参;[] 内为可选参数与默认值):

	  契约与工具
	    Get-ClientEntryContract                     → 跨语言常量表(ordered hashtable)
	    ConvertTo-ClientEntryCanonicalName -Value -Allowed                                → 标准大小写写法 | $null
	    Get-ClientEntryKubectlBaseArgs  -KubeContext -KubeConfig                          → string[]
	    Invoke-ClientEntryKubectl       -KubectlArgs -KubeContext -KubeConfig [-AllowFailure] [-CaptureOutput]
	                                    → CaptureOutput 时返回 { ExitCode; Stdout; Stderr }

	  规则与计划(纯函数)
	    Resolve-GateClientEndpointPlan  -ServiceType NodePort|LoadBalancer -Replicas -ServicePort
	                                    [-NodePortBase 0] [-ClientHostTemplate ''] [-ClientPublicHost ''] [-ZoneName '']
	    Get-GateClientEndpointShellPrefix                                                 → POSIX sh 片段
	    Test-GateEntryServiceWanted     -ClientEntryMode -GateReplicas                    → bool(D90)
	    Get-ClientEntryRequireClientEndpoint -ClientEntryMode                             → 'true' | 'false'
	    Resolve-BattleFleetHealth       -InitialDelaySeconds -PeriodSeconds -FailureThreshold
	                                    → { InitialDelaySeconds; PeriodSeconds; FailureThreshold; Raised;
	                                        RequestedInitialDelaySeconds; StartupWorstSeconds }

	  生成器(纯函数,返回 YAML 字符串)
	    New-ClientEntryPodCommon        -Image -ImagePullPolicy -SnowflakeCacheDir -LogVolumeSizeLimit
	                                    [-LogPrunerCommand ''] [-GrpcServerMaxPollers 0]
	                                    [-SidecarContainerYaml ''] [-SidecarVolumeYaml ''] [-SidecarConfigHash '']
	    New-GateStatefulSetYaml         -PodCommon -Plan -RpcPort -StartCommand -ConfigMapName -GateRouterMode
	                                    [-TerminationGracePeriodSeconds 30]
	    New-GateOrdinalServicesYaml     -Plan [-ExternalTrafficPolicy Local]              → gate-<i> 用 --- 连接
	    New-GateHeadlessServiceYaml     -ServicePort
	    New-GatePdbYaml
	    New-BattleFleetYaml             -PodCommon -Replicas -RpcPort -StartCommand -ConfigMapName
	                                    -ClientEntryMode -Health -BuildLabel [-ClientPublicHost '']
	                                    [-TerminationGracePeriodSeconds 30]
	    New-BattleHostPortDeploymentYaml -PodCommon -Replicas -RpcPort -StartCommand -ConfigMapName
	                                    [-ClientPublicHost ''] [-TerminationGracePeriodSeconds 30]
	    New-GatewayIngressYaml          -IngressHost [-ZoneName ''] [-IngressClassName nginx] [-TlsSecret '']
	                                    [-ServiceName gateway] [-ServicePort 8081]         → 只路由 /api(Prefix)
	    New-GatewayRateLimitYaml        -TrustedProxies [-Indent '  ']                    → 挂在 gate: 下的片段
	    New-LoginDevPasswordAuthYaml                                                      → login.yaml 顶层片段

	  preflight
	    Test-ClientEntryPreflight       -ClientEntryMode -GateServiceType [-Zones @()] [-GateClientHostTemplate '']
	                                    [-ClientPublicHost ''] [-GateExternalTrafficPolicy Local]
	                                    [-DeploysBattle $false] [-BattleOrchestrator deployment]
	                                    [-AgonesFleetCrdPresent $null] [-DeploysGateway $false]
	                                    [-GatewayIngressHost ''] [-GatewayIngressTlsSecret '']
	                                    [-GatewayTrustedProxies ''] [-LoginDevPasswordAuth $false]
	                                    [-ReleaseProfile dev]
	                                    → { Errors = string[]; Warnings = string[] }
	                                    -Zones 每项:{ Name; GateReplicas; GateNodePortBase; GateNodePortBaseExplicit }
	    Assert-ClientEntryPreflightResult -Result                                        → Write-Warning + throw
	    Test-AgonesFleetCrdPresent      -KubeContext -KubeConfig                          → bool
	                                    (get --raw /apis/agones.dev/v1:NotFound = 未装;其余 kubectl 失败 throw)

	  集群操作(-KubeContext / -KubeConfig 必填,可为空串;均支持 -DryRun)
	    Wait-ForFleetReady              -Namespace -FleetName -ExpectedReplicas -KubeContext -KubeConfig
	                                    [-TimeoutSeconds 300] [-PollIntervalSeconds 5]
	                                    → 判据见 Measure-FleetRolloutReadiness(只认当前模板的 GameServerSet)
	    Wait-ForGateStatefulSetReady    -Namespace -ExpectedReplicas -KubeContext -KubeConfig [-StatefulSetName gate]
	                                    [-TimeoutSeconds 300] [-PollIntervalSeconds 5]    → 替代 Wait-ForStatefulSetReady
	                                    (后者走 rollout status,对 OnDelete 的 StatefulSet 直接报错)
	    Get-GateObsoleteResources       -ClientEntryMode -GateReplicas                    → 待删资源清单(纯函数)
	    Get-BattleObsoleteResources     -BattleOrchestrator                               → 待删资源清单(纯函数)
	    Find-ClientEntryObsoleteResources -Namespace -Resources -KubeContext -KubeConfig [-AgonesFleetCrdPresent $null]
	                                    [-KicksPlayersOnly]                               → 只探测不删:{ Present; Refusals }
	    Remove-ClientEntryObsoleteResources -Namespace -Resources -KubeContext -KubeConfig [-AllowDisruptiveSwitch]
	                                    → 会踢人的条目存在且未确认时 throw(一个都不删)
	    Remove-StaleGateOrdinalServices -Namespace -Replicas -KubeContext -KubeConfig

	  内部辅助(接入方不要直接依赖,签名可能调整):ConvertTo-ClientEntryLf、ConvertTo-ClientEntryYamlString、Add-ClientEntryIndent、
	    Get-ClientEntryMember、Test-ClientEntryHostName、ConvertTo-ClientEntryCidrList、Get-ClientEntryInvalidCidrs、
	    Get-ClientEntryEnvYaml、Get-ClientEntryNodeBaseEnv、Get-ClientEntryStartScript、Get-ClientEntryVolumeMountYaml、
	    Get-ClientEntryPodVolumeYaml、Get-ClientEntrySidecarBlocks、Measure-FleetRolloutReadiness(纯函数,契约测试可直测)、
	    ConvertTo-ClientEntryUtcTime、Get-ClientEntryStringMapText、New-ClientEntryObsoleteItem、
	    Get-ClientEntryObsoleteTarget、Wait-ForClientEntryCondition。

	  调用顺序(接入参考):Apply-OpsProfileDefaults 之后、任何集群写操作之前跑 preflight(GateServiceType
	  可能已被 OpsProfile 改写);先删另一种形态(Remove-ClientEntryObsoleteResources,k8s_deploy.ps1 暴露同名
	  -AllowDisruptiveSwitch 透传),再按 RBAC → ConfigMap → headless → StatefulSet / Fleet → 每序号 Service → PDB
	  的顺序 apply。
	  契约测试检查 D92 时要按整词匹配两个身份 env 名:GATE_NODE_PORT_BASE 是合法的生成器内部 env,会被子串匹配误伤。

	gateway Ingress 的归属(2c §2 决定):Ingress 与 gateway Deployment 同处 —— 由 zone-up(Apply-Zone 里的
	Apply-JavaSvcManifests,zone namespace)生成;只在本次确实部署 Java 服务时生成。infra-up 不部署 gateway,
	在 infra-up 上传 -GatewayIngressHost 只会得到"被忽略"警告。Ingress 只路由玩家面 /api,
	/admin 与 /actuator 永不出集群(见 New-GatewayIngressYaml)。
#>

# ─────────────────────────────────────────────────────────────────
# 0. 跨语言契约与小工具
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	跨语言 / 跨脚本的字符串契约。改任何一项都必须同改右侧注释里的对端。
#>
function Get-ClientEntryContract {
	return [ordered]@{
		# battle main.cpp kAgonesDrainLabelKey;运维手动 `kubectl label gs` 用同一个键(D83)。
		DrainLabelKey              = 'mmorpg.io/drain'
		# agones_client_endpoint_source.h agones::kClientPortName(D81)。
		AgonesClientPortName       = 'client'
		# 每序号 Service 的标签(ingress_final §3「标签与注解」)。
		GateOrdinalLabelKey        = 'mmorpg.io/gate-ordinal'
		GateHeadlessServiceName    = 'gate-headless'
		GatePdbName                = 'gate'
		# podip 单副本入口(D90);k8s_deploy.ps1 Apply-Zone 里的 $gateServiceName。
		GateEntryServiceName       = 'gate-entry'
		# k8s_deploy.ps1 New-AgonesSdkRbacYaml 建的 ServiceAccount。
		AgonesSdkServiceAccount    = 'agones-sdk'
		AgonesApiGroup             = 'agones.dev'
		AgonesApiVersion           = 'v1'
		# 发现文档 /apis/agones.dev/v1 里 Fleet 的资源名(复数、不带组)。
		AgonesFleetPlural          = 'fleets'
		AgonesFleetResource        = 'fleets.agones.dev'
		AgonesGameServerSetResource = 'gameserversets.agones.dev'
		# Agones 写在 GameServerSet / GameServer 上的所属 Fleet 标签(agonesv1.FleetNameLabel)。
		AgonesFleetNameLabelKey    = 'agones.dev/fleet'
		ExternalDnsHostnameKey     = 'external-dns.alpha.kubernetes.io/hostname'
		# gateway 玩家面接口全在 /api 下(java/gateway_node controller 的 @RequestMapping("/api"));
		# /admin/**(管理面)与 /actuator/**(探针)只在集群内访问,Ingress 不路由(D91)。
		GatewayIngressPathPrefix   = '/api'
		# K8s 默认 service-node-port-range 与推荐的静态子段(D88)。
		NodePortMin                = 30000
		NodePortMax                = 32767
		RecommendedNodePortMax     = 30085
		# node_allocator.cpp kGrpcPortOffset:非 gate 节点 gRPC 端口 = TCP 端口 + 30000。
		GrpcPortOffset             = 30000
		# go/login/etc/login.yaml DevPasswordAuth 段(2b §7)。
		LoginDevPasswordSecretEnv  = 'LOGIN_DEV_PASSWORD_SHARED_SECRET'
		LoginDevPasswordPrefixes   = @('robot_', 'dev_')
		# C++ client_endpoint.h kEnvSource / kEnvHost / kEnvPort / kEnvRequired(D79)。
		ClientEndpointSourceEnv    = 'CLIENT_ENDPOINT_SOURCE'
		ClientEndpointHostEnv      = 'CLIENT_ENDPOINT_HOST'
		ClientEndpointPortEnv      = 'CLIENT_ENDPOINT_PORT'
		ClientEndpointRequiredEnv  = 'CLIENT_ENDPOINT_REQUIRED'
		# CLIENT_ENDPOINT_SOURCE 的取值:C++ client_endpoint.cpp 的来源名表(kNone / kStatic / kAgones)。
		# k8s_deploy.ps1 Get-BattleEntryDrift 读集群现状时按同一组取值判 external(agones / static)与 podip(none / 缺省)。
		ClientEndpointSourceAgones = 'agones'
		ClientEndpointSourceStatic = 'static'
		ClientEndpointSourceNone   = 'none'
		# battle hostPort 形态没给 -ClientPublicHost 时 CLIENT_ENDPOINT_HOST 的值:交给 kubelet 做依赖展开的 $(HOST_IP)。
		# Get-BattleEntryDrift 把它当作"没有主机覆盖"。
		BattleHostPortDefaultClientHost = '$(HOST_IP)'
		# gate 生成器内部 env(C++ 不读,只给启动 shell 前缀算地址)。Get-GateClientEndpointShellPrefix 的 shell 片段
		# 按字面量引用这些名字,渲染时逐个自检(改名漏改片段即 throw);k8s_deploy.ps1 Get-GateAddressDrift 读集群里
		# StatefulSet 模板的这几项比对地址来源。
		GateClientHostTemplateEnv  = 'GATE_CLIENT_HOST_TEMPLATE'
		GateClientPublicHostEnv    = 'CLIENT_PUBLIC_HOST'
		GateClientPortModeEnv      = 'GATE_CLIENT_PORT_MODE'
		GateNodePortBaseEnv        = 'GATE_NODE_PORT_BASE'
		GateClientPortEnv          = 'GATE_CLIENT_PORT'
		# GATE_CLIENT_PORT_MODE 的取值(Resolve-GateClientEndpointPlan 的 PortMode):nodeport = 基址 + 序号;service = Service 端口。
		GateClientPortModeNodePort = 'nodeport'
		GateClientPortModeService  = 'service'
	}
}

# 生成器返回前统一换成 LF。本文件受 .gitattributes 的 *.ps1 eol=crlf 约束,here-string 的换行随文件是 CRLF,
# 又与 -join "`n" 拼的片段混在一起;调用方(k8s_deploy.ps1、契约测试的 (?m)^...$ 锚定)一律按 LF 处理。
function ConvertTo-ClientEntryLf {
	param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
	return ($Text -replace "`r`n", "`n")
}

# YAML 双引号标量。env 值、label 值一律加引号:纯数字不加引号会被解析成数字,API server 直接拒。
function ConvertTo-ClientEntryYamlString {
	param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value)
	return '"' + $Value.Replace('\', '\\').Replace('"', '\"') + '"'
}

<#
.SYNOPSIS
	把大小写不敏感的枚举取值规范成 Allowed 里的标准写法;不在 Allowed 内返回 $null。

.DESCRIPTION
	PowerShell 的 ValidateSet / -in / -eq 都不分大小写,也不会改写取值:-GateServiceType nodeport 能过参数校验,
	原样写进 YAML 就是 `type: nodeport`,API server 按大小写校验枚举,直接拒收。凡是要写进 YAML 的枚举
	(Service type、externalTrafficPolicy、imagePullPolicy)都先经这里。k8s_deploy.ps1 参数绑定后也可以用它
	一次规范化,之后再传给本库与自己的模板。
#>
function ConvertTo-ClientEntryCanonicalName {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value,
		[Parameter(Mandatory = $true)][string[]]$Allowed
	)
	foreach ($candidate in $Allowed) {
		if ([string]::Equals($candidate, $Value, [System.StringComparison]::OrdinalIgnoreCase)) { return $candidate }
	}
	return $null
}

<#
.SYNOPSIS
	严格模式安全地按路径取成员:IDictionary 按键取,其余按 PSObject 属性取;任一级缺失返回 $null。

.DESCRIPTION
	kubectl JSON 里取值为 0 的字段(StatefulSet status.readyReplicas 带 omitempty)、刚建出来还没有 status 的对象、
	调用方用 hashtable 或 pscustomobject 传入的 zone 记录,都可能缺某个成员;调用方开了 StrictMode 时直接点号访问
	会抛异常。缺席不是错误的地方一律经这里读。注意 PowerShell 会展开返回的数组:取列表时用 @() 包一层。
#>
function Get-ClientEntryMember {
	param(
		[AllowNull()]$Object,
		[Parameter(Mandatory = $true)][string[]]$Path
	)
	$current = $Object
	foreach ($name in $Path) {
		if ($null -eq $current) { return $null }
		if ($current -is [System.Collections.IDictionary]) {
			$current = if ($current.Contains($name)) { $current[$name] } else { $null }
			continue
		}
		$property = $current.PSObject.Properties[$name]
		$current = if ($null -eq $property) { $null } else { $property.Value }
	}
	return $current
}

# 重新缩进多行文本:先去掉所有非空行共同的前导空格,再给每个非空行加 Indent;空行保持为空,避免尾随空白。
# 调用方渲染 sidecar 片段时用什么缩进都行(New-CppLogSidecarContainerYaml 的 -ItemIndent 是必填、不收空串)。
function Add-ClientEntryIndent {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$Indent
	)
	$rawLines = @($Text -split "`r?`n")
	$common = $null
	foreach ($line in $rawLines) {
		if ([string]::IsNullOrWhiteSpace($line)) { continue }
		$lead = $line.Length - $line.TrimStart(' ').Length
		if ($null -eq $common -or $lead -lt $common) { $common = $lead }
	}
	if ($null -eq $common) { $common = 0 }
	$lines = foreach ($line in $rawLines) {
		if ([string]::IsNullOrWhiteSpace($line)) { '' } else { "$Indent$($line.Substring($common))" }
	}
	return (@($lines) -join "`n")
}

# 裸主机名:IPv4 字面量或 DNS 名,不带 scheme / 端口 / 路径 / 空白。
# 与 C++ client_endpoint::ParseSettings 对 CLIENT_ENDPOINT_HOST 的要求同口径(那边不合法即 LOG_FATAL)。
function Test-ClientEntryHostName {
	param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value)
	if ($Value.Length -gt 253) { return $false }
	return ($Value -cmatch '^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$')
}

# 逗号分隔的 CIDR 串 → 去空白、去空项后的列表。校验与渲染共用这一份拆分规则。
function ConvertTo-ClientEntryCidrList {
	param([AllowEmptyString()][string]$CommaSeparated = '')
	$items = @($CommaSeparated -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne '' })
	return ,$items
}

# 返回不合法的 CIDR 项。gateway 的 ClientIpResolver 遇到非法项只打 ERROR 日志并**静默跳过**,
# 漏掉的网段会让整个 Ingress 的流量共用一个限流桶,所以在部署前就拒(fail-closed)。
# 口径同 Java Cidr.parse:IPv4 点分四段或 IPv6,可选 /前缀位数(v4 0..32,v6 0..128);不带前缀 = 单个地址。
function Get-ClientEntryInvalidCidrs {
	param([AllowEmptyString()][string]$CommaSeparated = '')
	$invalid = @()
	foreach ($spec in (ConvertTo-ClientEntryCidrList -CommaSeparated $CommaSeparated)) {
		$addrPart = $spec
		$bitsText = $null
		$slash = $spec.IndexOf('/')
		if ($slash -ge 0) {
			$addrPart = $spec.Substring(0, $slash)
			$bitsText = $spec.Substring($slash + 1)
		}
		$isV4 = $addrPart -match '^\d{1,3}(\.\d{1,3}){3}$'
		$isV6 = $addrPart.Contains(':')
		$ip = $null
		if (-not ($isV4 -or $isV6) -or -not [System.Net.IPAddress]::TryParse($addrPart, [ref]$ip)) {
			$invalid += $spec
			continue
		}
		if ($null -ne $bitsText) {
			$maxBits = if ($isV6) { 128 } else { 32 }
			$bits = 0
			if (-not [int]::TryParse($bitsText, [ref]$bits) -or $bits -lt 0 -or $bits -gt $maxBits) {
				$invalid += $spec
			}
		}
	}
	return ,$invalid
}

# ─────────────────────────────────────────────────────────────────
# 1. kubectl 调用约定(-KubeContext / -KubeConfig)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	与 k8s_deploy.ps1 Build-KubectlBaseArgs 同语义,但显式传参:非空才追加 --context / --kubeconfig。
	两个参数必填(可为空串):空串 = 调用方明确选择"用 kubectl 默认值",而不是漏传(约定 3)。
#>
function Get-ClientEntryKubectlBaseArgs {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig
	)
	$base = @()
	if (-not [string]::IsNullOrWhiteSpace($KubeContext)) { $base += @('--context', $KubeContext) }
	if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) { $base += @('--kubeconfig', $KubeConfig) }
	return ,$base
}

<#
.SYNOPSIS
	本库唯一的 kubectl 出口。不处理 DryRun:要不要真的执行由调用方(各集群操作函数)决定。

.DESCRIPTION
	-CaptureOutput:stdout / stderr 分开收集并返回 { ExitCode; Stdout; Stderr },给需要解析输出的调用方
	(Fleet 就绪、CRD 探测、资源存在性)。否则输出直接进主机,不污染调用方的返回值。
	非 0 退出默认 throw;-AllowFailure 时由调用方看 ExitCode / $LASTEXITCODE。
	测试用同名 function kubectl 模拟时,须自行设置 $global:LASTEXITCODE。
#>
function Invoke-ClientEntryKubectl {
	param(
		[Parameter(Mandatory = $true)][string[]]$KubectlArgs,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig,
		[switch]$AllowFailure,
		[switch]$CaptureOutput
	)

	$baseArgs = Get-ClientEntryKubectlBaseArgs -KubeContext $KubeContext -KubeConfig $KubeConfig
	$allArgs = @()
	$allArgs += $baseArgs
	$allArgs += $KubectlArgs
	# 函数内局部覆盖:调用方(k8s_deploy.ps1)是 Stop,kubectl 往 stderr 打的提示不能被当成终止错误,
	# 成败只看退出码。
	$ErrorActionPreference = 'Continue'

	if (-not $CaptureOutput) {
		& kubectl @allArgs | Out-Host
		$exitCode = $LASTEXITCODE
		if (-not $AllowFailure -and $exitCode -ne 0) {
			throw "kubectl failed (exit $exitCode): kubectl $($allArgs -join ' ')"
		}
		return
	}

	$stdout = [System.Collections.Generic.List[string]]::new()
	$stderr = [System.Collections.Generic.List[string]]::new()
	foreach ($line in (& kubectl @allArgs 2>&1)) {
		if ($line -is [System.Management.Automation.ErrorRecord]) { $stderr.Add($line.ToString()) }
		else { $stdout.Add([string]$line) }
	}
	$exitCode = $LASTEXITCODE
	if (-not $AllowFailure -and $exitCode -ne 0) {
		throw "kubectl failed (exit $exitCode): kubectl $($allArgs -join ' ')`n$($stderr -join "`n")"
	}
	return [pscustomobject]@{ ExitCode = $exitCode; Stdout = ($stdout -join "`n"); Stderr = ($stderr -join "`n") }
}

# ─────────────────────────────────────────────────────────────────
# 2. 规则与计划(纯函数)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	一个 zone 的 gate 客户端地址计划(D88)。StatefulSet 的生成器内部 env 与每序号 Service 都从它取值,
	保证"gate 自报的端口"与"Service 暴露的端口"只有一份真相。

.DESCRIPTION
	主机优先级:模板(生成期渲染 {zone},shell 里渲染 {ordinal})> -ClientPublicHost > 运行期 HOST_IP。
	端口:NodePort = NodePortBase + 序号;LoadBalancer = ServicePort(18000)。
	Ordinals[i].ClientHost 为空串 = 运行期取 HOST_IP(status.hostIP),生成期无从得知。
	范围 / 重叠 / 模板合法性由 Test-ClientEntryPreflight 负责,这里只做会让结果失真的最小校验。
	返回的 ServiceType 一律是标准写法 NodePort / LoadBalancer(ValidateSet 不分大小写,nodeport 也能进来,
	原样写进 `type:` 会被 API server 拒收,约定 7)。
#>
function Resolve-GateClientEndpointPlan {
	param(
		[Parameter(Mandatory = $true)][ValidateSet('NodePort', 'LoadBalancer')][string]$ServiceType,
		[Parameter(Mandatory = $true)][ValidateRange(0, 65535)][int]$Replicas,
		[Parameter(Mandatory = $true)][ValidateRange(1, 65535)][int]$ServicePort,
		[int]$NodePortBase = 0,
		[AllowEmptyString()][string]$ClientHostTemplate = '',
		[AllowEmptyString()][string]$ClientPublicHost = '',
		[AllowEmptyString()][string]$ZoneName = ''
	)

	# 另起变量:给带 ValidateSet 的参数变量重新赋值会再跑一遍校验,语义不直观。
	$canonicalType = ConvertTo-ClientEntryCanonicalName -Value $ServiceType -Allowed @('NodePort', 'LoadBalancer')
	if ($ClientHostTemplate.Contains('{zone}') -and [string]::IsNullOrWhiteSpace($ZoneName)) {
		throw "-GateClientHostTemplate 含 {zone},但没有给 -ZoneName,无法渲染。"
	}
	if ($canonicalType -ceq 'NodePort' -and $NodePortBase -le 0) {
		throw "NodePort 形态必须给 -NodePortBase(每个 zone 显式配置,D88)。"
	}
	$hostTemplate = $ClientHostTemplate.Replace('{zone}', $ZoneName)
	$contract = Get-ClientEntryContract
	$isNodePort = ($canonicalType -ceq 'NodePort')
	$portMode = if ($isNodePort) { $contract.GateClientPortModeNodePort } else { $contract.GateClientPortModeService }

	$ordinals = @()
	for ($i = 0; $i -lt $Replicas; $i++) {
		$clientHost = if ($hostTemplate) { $hostTemplate.Replace('{ordinal}', [string]$i) } else { $ClientPublicHost }
		$ordinals += [pscustomobject]@{
			Ordinal             = $i
			# StatefulSet 名固定为 gate,Pod 名即 gate-<序号>;每序号 Service 与 Pod 同名(ingress_final §3)。
			PodName             = "gate-$i"
			ServiceName         = "gate-$i"
			NodePort            = if ($isNodePort) { $NodePortBase + $i } else { 0 }
			ClientHost          = $clientHost
			ClientPort          = if ($isNodePort) { $NodePortBase + $i } else { $ServicePort }
			# LoadBalancer 的地址要等云厂商分配,只能靠 DNS 模板 + external-dns 把名字指过去(ingress_final §3)。
			ExternalDnsHostname = if ($canonicalType -ceq 'LoadBalancer' -and $hostTemplate) { $clientHost } else { '' }
		}
	}

	return [pscustomobject]@{
		ServiceType  = $canonicalType
		PortMode     = $portMode
		NodePortBase = if ($isNodePort) { $NodePortBase } else { 0 }
		ServicePort  = $ServicePort
		# service 模式下 gate 自报的端口;nodeport 模式下为 0(由 shell 按序号算)。
		ClientPort   = if ($isNodePort) { 0 } else { $ServicePort }
		HostTemplate = $hostTemplate
		PublicHost   = $ClientPublicHost
		Ordinals     = $ordinals
	}
}

<#
.SYNOPSIS
	gate StatefulSet 启动 shell 的前缀片段(D88):由 POD_NAME 的序号算出 CLIENT_ENDPOINT_HOST / PORT 并 export。

.DESCRIPTION
	POSIX sh,dash 下也能跑。任何一步算不出来都 exit 64,Pod 进 CrashLoop 而不是带着错地址发布到 etcd
	(C++ 侧 static 来源缺 HOST / PORT 同样 LOG_FATAL,两道闸)。
	HOST / PORT **总会被 export**:kubectl set env 注入空的 CLIENT_ENDPOINT_HOST 会被这里覆盖,
	所以负向验证不能照 ingress_final §5.2 原 6f 的写法(见 2b §11)。
	kubelet 会先对 args 做 $(VAR) 展开:片段里不得出现 `$$`,`$(` 之后到第一个 `)` 之间也不得恰好是某个
	容器 env 名 —— 现有的 $(printf ...) 与 $((...)) 都不命中,kubelet 原样保留。
	sed 里的模板只含 [A-Za-z0-9.-] 与 {ordinal}(preflight 保证),不会破坏 s/// 表达式。
	片段里的 env 名与端口模式取值按字面量写(可读),定义在 Get-ClientEntryContract;返回前逐个自检片段确实引用了
	契约里的名字,改名漏改片段就 throw(渲染期、任何集群写操作之前),不会带着读不到的变量发布到集群。
	返回 LF 换行:片段进容器 shell,行尾的 CR 会被 sh 当成命令的一部分。
#>
function Get-GateClientEndpointShellPrefix {
	$contract = Get-ClientEntryContract
	# 单引号 here-string:$ 与反引号全部按字面量。不要改成双引号。
	$text = @'
ORD="${POD_NAME##*-}"; case "$ORD" in ''|*[!0-9]*) echo "gate: bad ordinal from POD_NAME=$POD_NAME" >&2; exit 64;; esac
if [ -n "$GATE_CLIENT_HOST_TEMPLATE" ]; then CLIENT_ENDPOINT_HOST=$(printf '%s' "$GATE_CLIENT_HOST_TEMPLATE" | sed "s/{ordinal}/$ORD/g")
elif [ -n "$CLIENT_PUBLIC_HOST" ]; then CLIENT_ENDPOINT_HOST="$CLIENT_PUBLIC_HOST"; else CLIENT_ENDPOINT_HOST="$HOST_IP"; fi
if [ "$GATE_CLIENT_PORT_MODE" = nodeport ]; then case "$GATE_NODE_PORT_BASE" in ''|*[!0-9]*) echo "gate: bad GATE_NODE_PORT_BASE=$GATE_NODE_PORT_BASE" >&2; exit 64;; esac; fi
if [ "$GATE_CLIENT_PORT_MODE" = nodeport ]; then CLIENT_ENDPOINT_PORT=$((GATE_NODE_PORT_BASE + ORD)); else CLIENT_ENDPOINT_PORT="$GATE_CLIENT_PORT"; fi
export CLIENT_ENDPOINT_HOST CLIENT_ENDPOINT_PORT
echo "gate: client endpoint $CLIENT_ENDPOINT_HOST:$CLIENT_ENDPOINT_PORT (ordinal $ORD)" >&2
'@
	# 带引号的 "$NAME" 精确到整名:裸 $GATE_CLIENT_PORT 会被 $GATE_CLIENT_PORT_MODE 的子串误判为已引用。
	$quotedVars = @($contract.GateClientHostTemplateEnv, $contract.GateClientPublicHostEnv, $contract.GateClientPortModeEnv,
		$contract.GateNodePortBaseEnv, $contract.GateClientPortEnv) | ForEach-Object { '"$' + $_ + '"' }
	$referenced = @($quotedVars) + @(
		('export ' + $contract.ClientEndpointHostEnv + ' ' + $contract.ClientEndpointPortEnv),
		('" = ' + $contract.GateClientPortModeNodePort + ' ]')
	)
	foreach ($needle in $referenced) {
		if (-not $text.Contains($needle)) {
			throw "Get-GateClientEndpointShellPrefix: shell 片段没有引用契约里的 '$needle'(Get-ClientEntryContract 改名后须同改片段)。"
		}
	}
	return (ConvertTo-ClientEntryLf -Text $text)
}

<#
.SYNOPSIS
	D90:单一 gate-entry Service 只在 podip 且 gate 副本数恰为 1 时生成。
	副本数 ≥2 时 Service 随机分流,约一半票据会被 token_gate_node_mismatch 拒绝;external 模式由每序号 Service 取代。
#>
function Test-GateEntryServiceWanted {
	param(
		[Parameter(Mandatory = $true)][ValidateSet('podip', 'external')][string]$ClientEntryMode,
		[Parameter(Mandatory = $true)][int]$GateReplicas
	)
	return ($ClientEntryMode -eq 'podip' -and $GateReplicas -eq 1)
}

<#
.SYNOPSIS
	login / scene_manager ConfigMap 的 RequireClientEndpoint 取值(ingress_final §3):external → true,podip → false。
	注意上线顺序(ingress_final §6 第 3 批):确认所有 gate 都已自报 clientEndpoint 之后再切 external 的 true。
#>
function Get-ClientEntryRequireClientEndpoint {
	param([Parameter(Mandatory = $true)][ValidateSet('podip', 'external')][string]$ClientEntryMode)
	if ($ClientEntryMode -eq 'external') { return 'true' }
	return 'false'
}

<#
.SYNOPSIS
	battle Fleet 的 Agones health 参数:复用 scene 的 -AgonesHealth* 三个参数,但把 initialDelaySeconds
	抬到能覆盖 battle 启动最坏耗时的下限(2c 补充第 1 条)。

.DESCRIPTION
	预算推导(从容器启动到进程发出第一次 /health):
	  a. 构造期 InitClientEndpoint:Agones Fetch 总预算 60s
	     (agones_client_endpoint_source.h ClientEndpointSourceOptions.totalBudget,单调时钟截止)。
	  b. 读表 + etcd 注册 + 端口 CAS 退避:K8s 上未实测,留 20s 余量。Agones Pod restartPolicy Never,
	     重建即换 PodIP,不会撞同 IP 旧注册的 180s 租约。
	  c. SetAfterStart 才启动 lifecycle worker,先跑完 RunInitialReady 才发 /health:
	     30 次、退避 200ms 翻倍封顶 2s,合计约 53s(sidecar 立即拒连的口径;sidecar 可连却不响应时它自己
	     也收不到 /health,判 Unhealthy 替换正是期望行为)。
	  d. 第一次 /health 再等一个 healthInterval(LifecycleOptions.healthInterval 2s)。
	  合计 T = 60 + 20 + 53 + 2 = 135s。
	Agones sidecar 从自身启动起计时:healthLastUpdated = 启动 + initialDelaySeconds,每 periodSeconds 检查一次,
	超时即失败计数 +1,满 failureThreshold 判 Unhealthy。最早判定时刻约为
	initialDelaySeconds + periodSeconds × failureThreshold,因此要求
	    initialDelaySeconds ≥ T − periodSeconds × failureThreshold。
	scene 默认 30 / 10 / 3 只覆盖到 60s,不够;按推导 battle 取 max(传入值, 135 − 10×3 = 105)。
	sidecar 早于业务容器启动,业务镜像**首次拉取**的耗时也会吃掉这段预算,不在推导内:生产须预拉镜像
	(与 scene Fleet 同一前提)。
	periodSeconds 必须大于 C++ healthInterval(2s),否则正常心跳也会被判失败 —— 不满足直接 throw。
#>
function Resolve-BattleFleetHealth {
	param(
		[Parameter(Mandatory = $true)][ValidateRange(0, 3600)][int]$InitialDelaySeconds,
		[Parameter(Mandatory = $true)][int]$PeriodSeconds,
		[Parameter(Mandatory = $true)][int]$FailureThreshold
	)

	$fetchBudgetSeconds = 60
	$initAllowanceSeconds = 20
	$readyRetrySeconds = 53
	$healthIntervalSeconds = 2
	$startupWorstSeconds = $fetchBudgetSeconds + $initAllowanceSeconds + $readyRetrySeconds + $healthIntervalSeconds

	if ($PeriodSeconds -le $healthIntervalSeconds) {
		throw "battle Fleet health periodSeconds=$PeriodSeconds 必须大于 C++ LifecycleOptions.healthInterval(${healthIntervalSeconds}s),否则正常心跳也会被判失败。"
	}
	if ($FailureThreshold -lt 1) {
		throw "battle Fleet health failureThreshold=$FailureThreshold 必须 ≥1。"
	}

	$required = [Math]::Max(0, $startupWorstSeconds - $PeriodSeconds * $FailureThreshold)
	$effective = [Math]::Max($InitialDelaySeconds, $required)
	return [pscustomobject]@{
		InitialDelaySeconds          = $effective
		PeriodSeconds                = $PeriodSeconds
		FailureThreshold             = $FailureThreshold
		# true = 传入值不够,已抬到推导下限;调用方应打一行说明(不静默改写运维给的值)。
		Raised                       = ($effective -ne $InitialDelaySeconds)
		RequestedInitialDelaySeconds = $InitialDelaySeconds
		StartupWorstSeconds          = $startupWorstSeconds
	}
}

# ─────────────────────────────────────────────────────────────────
# 3. Pod 公共部分(容器镜像、卷、日志 sidecar)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	打包 C++ 节点 Pod 的公共参数,避免每个生成器各接十个参数、各读一遍 k8s_deploy.ps1 的脚本变量。

.DESCRIPTION
	k8s_deploy.ps1 接入示例(sidecar 片段用任意缩进渲染,本库去掉公共前导空格后按各模板的列位置重新缩进;
	-ItemIndent 是必填且不收空串,传两个空格即可):
	    $podCommon = New-ClientEntryPodCommon -Image $NodeImage -ImagePullPolicy $ImagePullPolicy `
	        -SnowflakeCacheDir $SnowflakeCacheDir -LogVolumeSizeLimit $CppLogVolumeSizeLimit `
	        -LogPrunerCommand $CppLogPrunerCommand -GrpcServerMaxPollers $GrpcServerMaxPollers `
	        -SidecarContainerYaml (New-CppLogSidecarContainerYaml -ItemIndent '  ' -CurrentZoneName <zone|global> -AsNativeSidecar) `
	        -SidecarVolumeYaml (New-CppLogSidecarVolumeYaml -ItemIndent '  ') `
	        -SidecarConfigHash (Get-CppLogSidecarConfigHash)
	关闭 sidecar(-NoCppLogSidecar)时三个 Sidecar* 参数都留空。三者必须同时给或同时不给。
	业务日志卷名固定 node-logs:与 New-NodeDeploymentYaml 以及 sidecar 片段里的 $script:CppLogVolumeName 同名。
#>
function New-ClientEntryPodCommon {
	param(
		[Parameter(Mandatory = $true)][string]$Image,
		[Parameter(Mandatory = $true)][ValidateSet('Always', 'IfNotPresent', 'Never')][string]$ImagePullPolicy,
		[Parameter(Mandatory = $true)][string]$SnowflakeCacheDir,
		[Parameter(Mandatory = $true)][string]$LogVolumeSizeLimit,
		# 形如 "(while true; ...; done &) && ",拼在 mkdir 与启动命令之间;不得含双引号。
		[AllowEmptyString()][string]$LogPrunerCommand = '',
		# 0 = 不注入 GRPC_SERVER_MAX_POLLERS,由进程默认值接管(同 k8s_deploy.ps1 口径)。
		[ValidateRange(0, 1024)][int]$GrpcServerMaxPollers = 0,
		[AllowEmptyString()][string]$SidecarContainerYaml = '',
		[AllowEmptyString()][string]$SidecarVolumeYaml = '',
		[AllowEmptyString()][string]$SidecarConfigHash = ''
	)

	$given = @($SidecarContainerYaml, $SidecarVolumeYaml, $SidecarConfigHash | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
	if ($given.Count -ne 0 -and $given.Count -ne 3) {
		throw "New-ClientEntryPodCommon: -SidecarContainerYaml / -SidecarVolumeYaml / -SidecarConfigHash 必须同时给或同时留空。"
	}
	if ($LogPrunerCommand.Contains('"')) {
		throw "New-ClientEntryPodCommon: -LogPrunerCommand 不得含双引号(它要进 YAML 流式标量)。"
	}

	return [pscustomobject]@{
		Image                = $Image
		# 规范大小写(约定 7):imagePullPolicy 是区分大小写的 K8s 枚举。
		ImagePullPolicy      = ConvertTo-ClientEntryCanonicalName -Value $ImagePullPolicy -Allowed @('Always', 'IfNotPresent', 'Never')
		SnowflakeCacheDir    = $SnowflakeCacheDir
		LogVolumeSizeLimit   = $LogVolumeSizeLimit
		LogPrunerCommand     = $LogPrunerCommand
		GrpcServerMaxPollers = $GrpcServerMaxPollers
		SidecarEnabled       = ($given.Count -eq 3)
		SidecarContainerYaml = $SidecarContainerYaml
		SidecarVolumeYaml    = $SidecarVolumeYaml
		SidecarConfigHash    = $SidecarConfigHash
	}
}

# env 列表。每项 @{ Name; Value } 或 @{ Name; FieldPath }(downward API)。值一律加引号。
# 顺序有意义:K8s 的 $(VAR) 依赖展开只认排在前面的 env(battle hostPort 的 CLIENT_ENDPOINT_HOST 依赖 HOST_IP)。
function Get-ClientEntryEnvYaml {
	param(
		[Parameter(Mandatory = $true)][object[]]$Entries,
		[Parameter(Mandatory = $true)][string]$ItemIndent
	)
	$keyIndent = $ItemIndent + '  '
	$lines = @()
	foreach ($entry in $Entries) {
		$lines += "$ItemIndent- name: $($entry.Name)"
		if (-not [string]::IsNullOrEmpty($entry.FieldPath)) {
			$lines += "${keyIndent}valueFrom:"
			$lines += "${keyIndent}  fieldRef:"
			$lines += "${keyIndent}    fieldPath: $($entry.FieldPath)"
		}
		else {
			$lines += "${keyIndent}value: $(ConvertTo-ClientEntryYamlString -Value ([string]$entry.Value))"
		}
	}
	return ($lines -join "`n")
}

# C++ 节点共有的 env:集群内身份(POD_IP + RPC_PORT)、gRPC poller 上限、发号槽缓存目录。
# 与 New-NodeDeploymentYaml 同口径,只是不写 D92 禁用的第二个端口变量(RPC_PORT 优先,行为不变)。
function Get-ClientEntryNodeBaseEnv {
	param(
		[Parameter(Mandatory = $true)]$PodCommon,
		[Parameter(Mandatory = $true)][int]$RpcPort
	)
	$entries = @(
		@{ Name = 'POD_IP'; FieldPath = 'status.podIP' },
		@{ Name = 'RPC_PORT'; Value = [string]$RpcPort }
	)
	if ($PodCommon.GrpcServerMaxPollers -gt 0) {
		$entries += @{ Name = 'GRPC_SERVER_MAX_POLLERS'; Value = [string]$PodCommon.GrpcServerMaxPollers }
	}
	$entries += @{ Name = 'SNOWFLAKE_CACHE_DIR'; Value = $PodCommon.SnowflakeCacheDir }
	return ,$entries
}

# 启动脚本主体:建日志目录 → 日志清理循环 → 启动命令(与 New-NodeDeploymentYaml 的 args 同形)。
function Get-ClientEntryStartScript {
	param(
		[Parameter(Mandatory = $true)]$PodCommon,
		[Parameter(Mandatory = $true)][string]$StartCommand
	)
	return "mkdir -p /app/bin/logs/cpp_nodes && $($PodCommon.LogPrunerCommand)$StartCommand"
}

function Get-ClientEntryVolumeMountYaml {
	param(
		[Parameter(Mandatory = $true)]$PodCommon,
		[Parameter(Mandatory = $true)][string]$ItemIndent
	)
	$k = $ItemIndent + '  '
	return (@(
		"$ItemIndent- name: node-config", "${k}mountPath: /app/bin/etc", "${k}readOnly: true",
		"$ItemIndent- name: node-logs", "${k}mountPath: /app/bin/logs",
		"$ItemIndent- name: snowflake-cache", "${k}mountPath: $($PodCommon.SnowflakeCacheDir)"
	) -join "`n")
}

function Get-ClientEntryPodVolumeYaml {
	param(
		[Parameter(Mandatory = $true)]$PodCommon,
		[Parameter(Mandatory = $true)][string]$ConfigMapName,
		[Parameter(Mandatory = $true)][string]$ItemIndent
	)
	$k = $ItemIndent + '  '
	$text = (@(
		"$ItemIndent- name: node-config", "${k}configMap:", "${k}  name: $ConfigMapName",
		"$ItemIndent- name: node-logs", "${k}emptyDir: { sizeLimit: $($PodCommon.LogVolumeSizeLimit) }",
		"$ItemIndent- name: snowflake-cache", "${k}emptyDir: { sizeLimit: 64Mi }"
	) -join "`n")
	if ($PodCommon.SidecarEnabled) {
		$text += "`n" + (Add-ClientEntryIndent -Text $PodCommon.SidecarVolumeYaml -Indent $ItemIndent)
	}
	return $text
}

# 日志 sidecar 的两个片段。KeyIndent = Pod 模板里 metadata / spec 子键所在的列
# (apps/v1 为 6 个空格,Fleet 为 10 个空格)。sidecar 放 initContainers + restartPolicy: Always(原生 sidecar),
# 理由见 k8s_deploy.ps1 New-CppLogSidecarContainerYaml。哈希加引号:12 位十六进制可能全是数字或形如 1e5,
# 不加引号会被解析成数字,API server 拒收。
function Get-ClientEntrySidecarBlocks {
	param(
		[Parameter(Mandatory = $true)]$PodCommon,
		[Parameter(Mandatory = $true)][string]$KeyIndent
	)
	if (-not $PodCommon.SidecarEnabled) {
		return [pscustomobject]@{ InitBlock = ''; AnnotationBlock = '' }
	}
	$hash = ConvertTo-ClientEntryYamlString -Value $PodCommon.SidecarConfigHash
	return [pscustomobject]@{
		InitBlock       = "`n${KeyIndent}initContainers:`n" + (Add-ClientEntryIndent -Text $PodCommon.SidecarContainerYaml -Indent ($KeyIndent + '  '))
		AnnotationBlock = "`n${KeyIndent}annotations:`n${KeyIndent}  mmorpg.io/cpp-log-sidecar-config-hash: $hash"
	}
}

# ─────────────────────────────────────────────────────────────────
# 4. gate(external):StatefulSet + 每序号 Service + headless + PDB(D87 / D88 / D89)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	gate StatefulSet(external 模式)。容器定义沿用 New-NodeDeploymentYaml 的 gate 分支:
	POD_IP、RPC_PORT、GATE_CLIENT_RPC_ROUTER、卷、日志 sidecar、tcpSocket 就绪探针。

.DESCRIPTION
	与 Deployment 版的差别:
	  - podManagementPolicy: Parallel —— 序号只用来算地址,不需要按序启动。
	  - updateStrategy: OnDelete —— gate 有长连接,滚动一律走 k8s_gate_drain.ps1(标 draining → 等 drained → 删 Pod);
	    apply 新模板不会自动重建任何 Pod。
	  - 追加 POD_NAME / HOST_IP / CLIENT_ENDPOINT_SOURCE=static / CLIENT_ENDPOINT_REQUIRED=1 与生成器内部 env
	    (GATE_CLIENT_HOST_TEMPLATE / CLIENT_PUBLIC_HOST / GATE_CLIENT_PORT_MODE / GATE_NODE_PORT_BASE | GATE_CLIENT_PORT,
	    C++ 不读),启动 shell 前缀由它们算出 CLIENT_ENDPOINT_HOST / PORT。
	  - args 改用块标量:地址片段含双引号,塞进流式标量要层层转义。
	副本数取自 Plan.Ordinals 的个数,与每序号 Service 同源。
#>
function New-GateStatefulSetYaml {
	param(
		[Parameter(Mandatory = $true)]$PodCommon,
		# Resolve-GateClientEndpointPlan 的返回值。
		[Parameter(Mandatory = $true)]$Plan,
		[Parameter(Mandatory = $true)][ValidateRange(1, 65535)][int]$RpcPort,
		[Parameter(Mandatory = $true)][string]$StartCommand,
		[Parameter(Mandatory = $true)][string]$ConfigMapName,
		# 原样写进 GATE_CLIENT_RPC_ROUTER(k8s_deploy.ps1 -GateRouterMode,D75 默认 "1")。
		[Parameter(Mandatory = $true)][ValidateSet('0', '1')][string]$GateRouterMode,
		[ValidateRange(0, 3600)][int]$TerminationGracePeriodSeconds = 30
	)

	$contract = Get-ClientEntryContract
	$replicas = @($Plan.Ordinals).Count

	$envEntries = Get-ClientEntryNodeBaseEnv -PodCommon $PodCommon -RpcPort $RpcPort
	$envEntries += @(
		@{ Name = 'GATE_CLIENT_RPC_ROUTER'; Value = $GateRouterMode },
		@{ Name = 'POD_NAME'; FieldPath = 'metadata.name' },
		@{ Name = 'HOST_IP'; FieldPath = 'status.hostIP' },
		@{ Name = $contract.ClientEndpointSourceEnv; Value = $contract.ClientEndpointSourceStatic },
		@{ Name = $contract.ClientEndpointRequiredEnv; Value = '1' },
		@{ Name = $contract.GateClientHostTemplateEnv; Value = $Plan.HostTemplate },
		@{ Name = $contract.GateClientPublicHostEnv; Value = $Plan.PublicHost },
		@{ Name = $contract.GateClientPortModeEnv; Value = $Plan.PortMode }
	)
	if ($Plan.PortMode -eq $contract.GateClientPortModeNodePort) {
		$envEntries += @{ Name = $contract.GateNodePortBaseEnv; Value = [string]$Plan.NodePortBase }
	}
	else {
		$envEntries += @{ Name = $contract.GateClientPortEnv; Value = [string]$Plan.ClientPort }
	}

	$startScript = (Get-GateClientEndpointShellPrefix).TrimEnd() + "`n" + (Get-ClientEntryStartScript -PodCommon $PodCommon -StartCommand $StartCommand)
	$argsBlock = Add-ClientEntryIndent -Text $startScript -Indent '              '
	$envBlock = Get-ClientEntryEnvYaml -Entries $envEntries -ItemIndent '            '
	$mountBlock = Get-ClientEntryVolumeMountYaml -PodCommon $PodCommon -ItemIndent '            '
	$volumeBlock = Get-ClientEntryPodVolumeYaml -PodCommon $PodCommon -ConfigMapName $ConfigMapName -ItemIndent '        '
	$sidecar = Get-ClientEntrySidecarBlocks -PodCommon $PodCommon -KeyIndent '      '

	# 就绪探针同 Deployment 版(推导见 New-NodeDeploymentYaml):gate 只有 RpcPort 一个监听口,不加 startup / liveness。
	return ConvertTo-ClientEntryLf -Text @"
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: gate
spec:
  serviceName: $($contract.GateHeadlessServiceName)
  replicas: $replicas
  podManagementPolicy: Parallel
  updateStrategy:
    type: OnDelete
  selector:
    matchLabels:
      app: gate
  template:
    metadata:$($sidecar.AnnotationBlock)
      labels:
        app: gate
    spec:
      terminationGracePeriodSeconds: $TerminationGracePeriodSeconds$($sidecar.InitBlock)
      containers:
        - name: gate
          image: $($PodCommon.Image)
          imagePullPolicy: $($PodCommon.ImagePullPolicy)
          workingDir: /app/bin
          command: ["/bin/sh", "-lc"]
          args:
            - |
$argsBlock
          env:
$envBlock
          volumeMounts:
$mountBlock
          ports:
            - containerPort: $RpcPort
              name: rpc
          readinessProbe:
            tcpSocket:
              port: $RpcPort
            periodSeconds: 5
            failureThreshold: 3
      volumes:
$volumeBlock
"@
}

<#
.SYNOPSIS
	每序号 Service gate-<i>,多份用 "---" 连接;副本数为 0 时返回空串(调用方跳过 apply)。

.DESCRIPTION
	selector 带 statefulset.kubernetes.io/pod-name,只命中同序号的 Pod,所以 gate 自报的 nodePort
	一定落到自己身上 —— 这是票据不被 token_gate_node_mismatch 拒绝的前提(D90 的反面)。
	externalTrafficPolicy 默认 Local(D89):保留客户端源 IP;Cluster 会 SNAT,preflight 打警告。
	NodePort 形态写死 nodePort = base + 序号;LoadBalancer 不写 nodePort(由集群分配,客户端不用它),
	配了模板时加 external-dns 注解。
	gRPC(18000 + 30000)在 gate 上本来就没有监听,这里也绝不暴露。
#>
function New-GateOrdinalServicesYaml {
	param(
		[Parameter(Mandatory = $true)]$Plan,
		[ValidateSet('Local', 'Cluster')][string]$ExternalTrafficPolicy = 'Local'
	)

	$contract = Get-ClientEntryContract
	# 规范大小写(约定 7):Service.spec.type 与 externalTrafficPolicy 都是区分大小写的 K8s 枚举。
	$serviceType = ConvertTo-ClientEntryCanonicalName -Value ([string]$Plan.ServiceType) -Allowed @('NodePort', 'LoadBalancer')
	if ($null -eq $serviceType) {
		throw "New-GateOrdinalServicesYaml: Plan.ServiceType '$($Plan.ServiceType)' 不是 NodePort / LoadBalancer(请用 Resolve-GateClientEndpointPlan 生成 Plan)。"
	}
	$trafficPolicy = ConvertTo-ClientEntryCanonicalName -Value $ExternalTrafficPolicy -Allowed @('Local', 'Cluster')
	$docs = @()
	foreach ($ordinal in @($Plan.Ordinals)) {
		$annotationBlock = ''
		if (-not [string]::IsNullOrEmpty($ordinal.ExternalDnsHostname)) {
			$annotationBlock = "`n  annotations:`n    $($contract.ExternalDnsHostnameKey): $(ConvertTo-ClientEntryYamlString -Value $ordinal.ExternalDnsHostname)"
		}
		$nodePortLine = ''
		if ($Plan.PortMode -eq $contract.GateClientPortModeNodePort) {
			$nodePortLine = "`n      nodePort: $($ordinal.NodePort)"
		}
		$docs += @"
apiVersion: v1
kind: Service
metadata:
  name: $($ordinal.ServiceName)
  labels:
    app: gate
    $($contract.GateOrdinalLabelKey): "$($ordinal.Ordinal)"$annotationBlock
spec:
  type: $serviceType
  externalTrafficPolicy: $trafficPolicy
  selector:
    app: gate
    statefulset.kubernetes.io/pod-name: $($ordinal.PodName)
  ports:
    - name: tcp-gate
      protocol: TCP
      port: $($Plan.ServicePort)
      targetPort: rpc$nodePortLine
"@
	}
	return (ConvertTo-ClientEntryLf -Text ($docs -join "`n---`n"))
}

<#
.SYNOPSIS
	StatefulSet 要求的 governing Service(spec.serviceName)。只给 Pod 提供稳定 DNS,客户端与 login 都不用它。
#>
function New-GateHeadlessServiceYaml {
	param([Parameter(Mandatory = $true)][ValidateRange(1, 65535)][int]$ServicePort)

	$contract = Get-ClientEntryContract
	return ConvertTo-ClientEntryLf -Text @"
apiVersion: v1
kind: Service
metadata:
  name: $($contract.GateHeadlessServiceName)
  labels:
    app: gate
spec:
  clusterIP: None
  selector:
    app: gate
  ports:
    - name: tcp-gate
      protocol: TCP
      port: $ServicePort
      targetPort: rpc
"@
}

<#
.SYNOPSIS
	gate PDB,maxUnavailable: 0(D87)。

.DESCRIPTION
	代价(ingress_final §7):它会挡住 kubectl drain 与 cluster-autoscaler 对 gate 所在节点的驱逐 ——
	这是有意的:驱逐 = 整台 gate 踢人。维护节点前先用 k8s_gate_drain.ps1 排空并删 Pod。
	切回 podip(Deployment)时必须删掉它(Get-GateObsoleteResources),否则它会挡住 Deployment 版 gate 的驱逐。
#>
function New-GatePdbYaml {
	$contract = Get-ClientEntryContract
	return ConvertTo-ClientEntryLf -Text @"
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: $($contract.GatePdbName)
  labels:
    app: gate
spec:
  maxUnavailable: 0
  selector:
    matchLabels:
      app: gate
"@
}

# ─────────────────────────────────────────────────────────────────
# 5. battle:Agones Fleet(D81–D84)与 hostPort Deployment(D86)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	battle 的 agones.dev/v1 Fleet(infra namespace,-BattleOrchestrator agones)。

.DESCRIPTION
	- spec.ports 只列 name: client、portPolicy: Dynamic、containerPort = RpcPort(20000)。Agones 会把这个端口
	  注入容器,所以容器自己的 ports **只写 gRPC(RpcPort + 30000)** —— 重复声明 20000 会触发端口重复校验;
	  gRPC 永不进 hostPort / nodePort / Agones ports,也不开 hostNetwork(D81)。
	- allocationOverflow 打排空标签(D83):滚动或缩容时旧的 Allocated 实例被打上 mmorpg.io/drain=true,
	  进程读到后拒绝新房间、打完现有房间回 Ready,由 Fleet 回收。
	- eviction.safe: Never:在打的房间不被驱逐(代价:挡 kubectl drain,按手册先打排空标签)。
	- health 取 Resolve-BattleFleetHealth 的结果(预算推导见该函数)。
	- 不加任何 K8s 探针:Agones 给 GameServer Pod 写死 restartPolicy: Never,会杀容器的探针等于销毁整个
	  GameServer;就绪交给 SDK 的 Ready / Health(同 New-SceneFleetYaml)。
	- env:AGONES_ENABLED=1;CLIENT_ENDPOINT_SOURCE = agones(external)| none(podip)。
	  external 时 CLIENT_ENDPOINT_REQUIRED=1,给了 -ClientPublicHost 才写 CLIENT_ENDPOINT_HOST(覆盖
	  Agones status.address,kind 填 127.0.0.1);**永不写 CLIENT_ENDPOINT_PORT**(agones 来源下设置即致命)。
	  podip 时 HOST / PORT / REQUIRED 一个都不写(SOURCE=none 时带了会打常态 WARN,2b §3)。
	- Pod 显式 serviceAccountName: agones-sdk(RBAC 由 New-AgonesSdkRbacYaml 在同一 namespace 先建)。
#>
function New-BattleFleetYaml {
	param(
		[Parameter(Mandatory = $true)]$PodCommon,
		[Parameter(Mandatory = $true)][ValidateRange(0, 65535)][int]$Replicas,
		# C++ 非 gate TCP 合法区间 20000..35535,gRPC = TCP + 30000 不越界。
		[Parameter(Mandatory = $true)][ValidateRange(20000, 35535)][int]$RpcPort,
		[Parameter(Mandatory = $true)][string]$StartCommand,
		[Parameter(Mandatory = $true)][string]$ConfigMapName,
		[Parameter(Mandatory = $true)][ValidateSet('podip', 'external')][string]$ClientEntryMode,
		# Resolve-BattleFleetHealth 的返回值。
		[Parameter(Mandatory = $true)]$Health,
		# k8s_deploy.ps1 Get-ImageBuildLabel -Image $NodeImage 的结果。
		[Parameter(Mandatory = $true)][string]$BuildLabel,
		[AllowEmptyString()][string]$ClientPublicHost = '',
		[ValidateRange(0, 3600)][int]$TerminationGracePeriodSeconds = 30
	)

	$contract = Get-ClientEntryContract
	$grpcPort = $RpcPort + $contract.GrpcPortOffset
	$buildValue = ConvertTo-ClientEntryYamlString -Value $BuildLabel

	$envEntries = Get-ClientEntryNodeBaseEnv -PodCommon $PodCommon -RpcPort $RpcPort
	$envEntries += @{ Name = 'AGONES_ENABLED'; Value = '1' }
	if ($ClientEntryMode -eq 'external') {
		$envEntries += @{ Name = $contract.ClientEndpointSourceEnv; Value = $contract.ClientEndpointSourceAgones }
		if (-not [string]::IsNullOrWhiteSpace($ClientPublicHost)) {
			$envEntries += @{ Name = $contract.ClientEndpointHostEnv; Value = $ClientPublicHost }
		}
		$envEntries += @{ Name = $contract.ClientEndpointRequiredEnv; Value = '1' }
	}
	else {
		$envEntries += @{ Name = $contract.ClientEndpointSourceEnv; Value = $contract.ClientEndpointSourceNone }
	}

	$argsValue = ConvertTo-ClientEntryYamlString -Value (Get-ClientEntryStartScript -PodCommon $PodCommon -StartCommand $StartCommand)
	$envBlock = Get-ClientEntryEnvYaml -Entries $envEntries -ItemIndent '                '
	$mountBlock = Get-ClientEntryVolumeMountYaml -PodCommon $PodCommon -ItemIndent '                '
	$volumeBlock = Get-ClientEntryPodVolumeYaml -PodCommon $PodCommon -ConfigMapName $ConfigMapName -ItemIndent '            '
	$sidecar = Get-ClientEntrySidecarBlocks -PodCommon $PodCommon -KeyIndent '          '

	# container: battle 始终显式写出:原生 sidecar 在 initContainers 里,但将来再加普通容器时不写这行 Fleet 会被拒。
	return ConvertTo-ClientEntryLf -Text @"
apiVersion: agones.dev/v1
kind: Fleet
metadata:
  name: battle
  labels:
    app: battle
    mmorpg.io/role: battle
    mmorpg.io/build: $buildValue
spec:
  replicas: $Replicas
  scheduling: Packed
  strategy:
    type: RollingUpdate
  allocationOverflow:
    labels:
      $($contract.DrainLabelKey): "true"
  template:
    metadata:
      labels:
        app: battle
        mmorpg.io/role: battle
        mmorpg.io/build: $buildValue
    spec:
      container: battle
      ports:
        - name: $($contract.AgonesClientPortName)
          portPolicy: Dynamic
          containerPort: $RpcPort
          protocol: TCP
      health:
        disabled: false
        initialDelaySeconds: $($Health.InitialDelaySeconds)
        periodSeconds: $($Health.PeriodSeconds)
        failureThreshold: $($Health.FailureThreshold)
      eviction:
        safe: Never
      template:
        metadata:$($sidecar.AnnotationBlock)
          labels:
            app: battle
            mmorpg.io/role: battle
            mmorpg.io/build: $buildValue
        spec:
          serviceAccountName: $($contract.AgonesSdkServiceAccount)
          terminationGracePeriodSeconds: $TerminationGracePeriodSeconds$($sidecar.InitBlock)
          containers:
            - name: battle
              image: $($PodCommon.Image)
              imagePullPolicy: $($PodCommon.ImagePullPolicy)
              workingDir: /app/bin
              command: ["/bin/sh", "-lc"]
              args: [$argsValue]
              env:
$envBlock
              volumeMounts:
$mountBlock
              ports:
                - containerPort: $grpcPort
                  name: grpc
          volumes:
$volumeBlock
"@
}

<#
.SYNOPSIS
	battle 的 Deployment + hostPort 形态(external + -BattleOrchestrator deployment),**只用于验证与回退**(D86)。

.DESCRIPTION
	- 只有客户端口 rpc(RpcPort = 20000)带 hostPort = RpcPort;gRPC(RpcPort + 30000)只写 containerPort,
	  永不进 hostPort。
	- 同一节点只能跑 1 个副本(hostPort 独占),且没有忙碌保护:滚动时在打的局直接作废。所以
	  strategy 取 maxSurge 0 / maxUnavailable 1 —— 新 Pod 不可能与旧 Pod 抢同一节点的 hostPort 而卡 Pending。
	  preflight 对 external + deployment 打警告。
	- CLIENT_ENDPOINT_SOURCE=static,端口 = hostPort;主机取 -ClientPublicHost,留空则取 $(HOST_IP)
	  (kubelet 依赖展开,HOST_IP 必须排在它之前)。REQUIRED=1:拿不到地址就拒签票,不回落 PodIP。
	- 探针沿用 New-NodeDeploymentYaml 的 battle 分支(startup 探客户端口 2s × 150,readiness 探 gRPC 口)。
#>
function New-BattleHostPortDeploymentYaml {
	param(
		[Parameter(Mandatory = $true)]$PodCommon,
		[Parameter(Mandatory = $true)][ValidateRange(0, 65535)][int]$Replicas,
		[Parameter(Mandatory = $true)][ValidateRange(20000, 35535)][int]$RpcPort,
		[Parameter(Mandatory = $true)][string]$StartCommand,
		[Parameter(Mandatory = $true)][string]$ConfigMapName,
		[AllowEmptyString()][string]$ClientPublicHost = '',
		[ValidateRange(0, 3600)][int]$TerminationGracePeriodSeconds = 30
	)

	$contract = Get-ClientEntryContract
	$grpcPort = $RpcPort + $contract.GrpcPortOffset
	# 缺省值 $(HOST_IP) 是交给 kubelet 的依赖展开,不是 PowerShell 子表达式(契约里用单引号定义)。
	$clientHost = if ([string]::IsNullOrWhiteSpace($ClientPublicHost)) { $contract.BattleHostPortDefaultClientHost } else { $ClientPublicHost }

	$envEntries = Get-ClientEntryNodeBaseEnv -PodCommon $PodCommon -RpcPort $RpcPort
	$envEntries += @(
		@{ Name = 'HOST_IP'; FieldPath = 'status.hostIP' },
		@{ Name = $contract.ClientEndpointSourceEnv; Value = $contract.ClientEndpointSourceStatic },
		@{ Name = $contract.ClientEndpointHostEnv; Value = $clientHost },
		@{ Name = $contract.ClientEndpointPortEnv; Value = [string]$RpcPort },
		@{ Name = $contract.ClientEndpointRequiredEnv; Value = '1' }
	)

	$argsValue = ConvertTo-ClientEntryYamlString -Value (Get-ClientEntryStartScript -PodCommon $PodCommon -StartCommand $StartCommand)
	$envBlock = Get-ClientEntryEnvYaml -Entries $envEntries -ItemIndent '            '
	$mountBlock = Get-ClientEntryVolumeMountYaml -PodCommon $PodCommon -ItemIndent '            '
	$volumeBlock = Get-ClientEntryPodVolumeYaml -PodCommon $PodCommon -ConfigMapName $ConfigMapName -ItemIndent '        '
	$sidecar = Get-ClientEntrySidecarBlocks -PodCommon $PodCommon -KeyIndent '      '

	return ConvertTo-ClientEntryLf -Text @"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: battle
spec:
  replicas: $Replicas
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 0
      maxUnavailable: 1
  selector:
    matchLabels:
      app: battle
  template:
    metadata:$($sidecar.AnnotationBlock)
      labels:
        app: battle
    spec:
      terminationGracePeriodSeconds: $TerminationGracePeriodSeconds$($sidecar.InitBlock)
      containers:
        - name: battle
          image: $($PodCommon.Image)
          imagePullPolicy: $($PodCommon.ImagePullPolicy)
          workingDir: /app/bin
          command: ["/bin/sh", "-lc"]
          args: [$argsValue]
          env:
$envBlock
          volumeMounts:
$mountBlock
          ports:
            - containerPort: $RpcPort
              hostPort: $RpcPort
              name: rpc
            - containerPort: $grpcPort
              name: grpc
          startupProbe:
            tcpSocket:
              port: $RpcPort
            periodSeconds: 2
            failureThreshold: 150
          readinessProbe:
            tcpSocket:
              port: $grpcPort
            periodSeconds: 5
      volumes:
$volumeBlock
"@
}

# ─────────────────────────────────────────────────────────────────
# 6. gateway HTTP 入口(D91)与 login 开发认证片段(2b §7)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	gateway 的 networking.k8s.io/v1 Ingress,名 gateway,后端 gateway:8081。

.DESCRIPTION
	归属:与 gateway Deployment 同处,由 zone-up 在 zone namespace 里 apply(Ingress 只能引用同 namespace 的
	Service)。-IngressHost 可含 {zone},生成期渲染:多个 zone 各自部署 gateway 时,同一个 host 的 Ingress
	会被 ingress-nginx 准入拒绝(preflight 对此打警告)。
	启用 Ingress 必须同时给 gateway 配 gate.rate-limit.trusted-proxies(New-GatewayRateLimitYaml),
	否则所有玩家共用 Ingress controller 的那一个限流桶 —— preflight 对此报错。

	**只路由 /api(pathType Prefix)**:gateway:8081 上同时挂着 /admin/zones、/admin/whitelist、
	/admin/announcements 管理面和 /actuator 探针;把 "/" 整个放出集群等于把管理面交给公网,
	而管理面的唯一防线是一个 X-Admin-Key,AdminApiKeyFilter 又只按原始 URI 前缀判断。玩家面接口全在 /api 下
	(D91 验收的 /api/server-list、robot 的 /api/assign-gate),Prefix 按路径段匹配,/apix 之类不会命中。
	/admin 与 /actuator 只在集群内经 Service 访问。k8s_deploy.ps1 经 Secret 注入 admin.api-key(dev 档允许占位并告警,
	其它档缺密钥拒绝部署)与 Java 属主的 Filter 路径规范化都是纵深防御项,不能替代这里的收窄。
#>
function New-GatewayIngressYaml {
	param(
		[Parameter(Mandatory = $true)][string]$IngressHost,
		[AllowEmptyString()][string]$ZoneName = '',
		[AllowEmptyString()][string]$IngressClassName = 'nginx',
		[AllowEmptyString()][string]$TlsSecret = '',
		[string]$ServiceName = 'gateway',
		[ValidateRange(1, 65535)][int]$ServicePort = 8081
	)

	if ($IngressHost.Contains('{zone}') -and [string]::IsNullOrWhiteSpace($ZoneName)) {
		throw "-GatewayIngressHost 含 {zone},但没有给 -ZoneName,无法渲染。"
	}
	$contract = Get-ClientEntryContract
	$renderedHost = $IngressHost.Replace('{zone}', $ZoneName)
	$classLine = if ([string]::IsNullOrWhiteSpace($IngressClassName)) { '' } else { "`n  ingressClassName: $IngressClassName" }
	$tlsBlock = ''
	if (-not [string]::IsNullOrWhiteSpace($TlsSecret)) {
		$tlsBlock = "`n  tls:`n    - hosts:`n        - $renderedHost`n      secretName: $TlsSecret"
	}

	return ConvertTo-ClientEntryLf -Text @"
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: gateway
  labels:
    app: gateway
spec:$classLine$tlsBlock
  rules:
    - host: $renderedHost
      http:
        paths:
          - path: $($contract.GatewayIngressPathPrefix)
            pathType: Prefix
            backend:
              service:
                name: $ServiceName
                port:
                  number: $ServicePort
"@
}

<#
.SYNOPSIS
	gateway application.yaml 里 gate: 键**下面**的 rate-limit 片段(Spring 属性 gate.rate-limit.trusted-proxies)。

.DESCRIPTION
	java-svc-gateway-config 的承载形式是 application.yaml(k8s_deploy.ps1 New-JavaSvcConfigMapYaml),
	所以只写这一处,不再另设 GATE_RATELIMIT_TRUSTEDPROXIES env(ingress_final §3 二选一)。
	现有 gateway 配置已经有一个 gate: 键(token-secret),本片段必须接在它下面,不能再起一个 gate:
	(重复键在 YAML 里是非法 / 后者覆盖前者)。-Indent 是 gate: 子键的缩进。
	列表为空时返回空串:Java 侧未配置 = 只信 socket 对端地址(fail-closed)。
#>
function New-GatewayRateLimitYaml {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$TrustedProxies,
		[string]$Indent = '  '
	)

	$cidrs = ConvertTo-ClientEntryCidrList -CommaSeparated $TrustedProxies
	if ($cidrs.Count -eq 0) { return '' }
	$invalid = Get-ClientEntryInvalidCidrs -CommaSeparated $TrustedProxies
	if ($invalid.Count -gt 0) {
		throw "-GatewayTrustedProxies 含非法 CIDR:$($invalid -join ', ')"
	}
	$lines = @("${Indent}rate-limit:", "${Indent}  trusted-proxies:")
	foreach ($cidr in $cidrs) {
		$lines += "${Indent}    - $(ConvertTo-ClientEntryYamlString -Value $cidr)"
	}
	return (ConvertTo-ClientEntryLf -Text ($lines -join "`n"))
}

<#
.SYNOPSIS
	login.yaml 顶层的 DevPasswordAuth 段(-LoginDevPasswordAuth,只允许 -ReleaseProfile dev)。

.DESCRIPTION
	键名核对自 go/login/internal/config/config.go DevPasswordAuthConf 与 go/login/etc/login.yaml。
	生效还有两个前提,由接入方负责,缺一个 login 就启动 panic(fail-closed):
	  1. 同一份配置的 go-zero Mode 必须是 dev 或 test(auth_init.go validateDevelopmentPasswordMode);
	  2. login 容器 env 里有 LOGIN_DEV_PASSWORD_SHARED_SECRET(经 Secret 注入;共享密钥绝不进 ConfigMap)。
	与 PasswordAuth 互斥(auth_init.go),接入方不得同时写 PasswordAuth.Enabled: true。
#>
function New-LoginDevPasswordAuthYaml {
	$contract = Get-ClientEntryContract
	$lines = @(
		'DevPasswordAuth:',
		'  Enabled: true',
		"  SharedSecretEnv: $(ConvertTo-ClientEntryYamlString -Value $contract.LoginDevPasswordSecretEnv)",
		'  AllowedAccountPrefixes:'
	)
	foreach ($prefix in $contract.LoginDevPasswordPrefixes) {
		$lines += "    - $(ConvertTo-ClientEntryYamlString -Value $prefix)"
	}
	return (ConvertTo-ClientEntryLf -Text ($lines -join "`n"))
}

# ─────────────────────────────────────────────────────────────────
# 7. preflight(错误与警告文本被契约测试断言,改措辞要同步改测试)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	集群外入口的部署前校验。纯函数:集群状态(Agones CRD 是否存在)由调用方探测后传入。

.DESCRIPTION
	只收集、不抛:返回 { Errors; Warnings },由 Assert-ClientEntryPreflightResult 统一输出与拒绝,
	一次把所有问题报全。必须在任何集群写操作之前调用(与 Assert-GrpcClientDeadlineBudget 同一位置)。
	-Zones:本次要部署的 zone(infra-up 传空数组 = 跳过 gate 校验)。每项 { Name; GateReplicas;
	  GateNodePortBase; GateNodePortBaseExplicit }。单 zone 路径(zone-up)把 -GateNodePortBase 当作显式值;
	  all-up 按 zones 配置里是否写了 gateNodePortBase 判定。注意:分多次 zone-up 部署的 zone 之间的
	  nodePort 重叠这里看不到,只能靠集群拒绝重复的 nodePort(apply 失败)。
	-DeploysBattle:本次会生成 battle(infra-up 且 BattleReplicas > 0)。
	-AgonesFleetCrdPresent:Test-AgonesFleetCrdPresent 的结果;$null = 未探测(DryRun 跳过,ingress_final WP9)。
	-DeploysGateway:本次会部署 gateway(zone-up 且未 -SkipJavaSvc、给了 -JavaSvcRegistry)。
#>
function Test-ClientEntryPreflight {
	param(
		[Parameter(Mandatory = $true)][ValidateSet('podip', 'external')][string]$ClientEntryMode,
		[Parameter(Mandatory = $true)][string]$GateServiceType,
		[AllowEmptyCollection()][object[]]$Zones = @(),
		[AllowEmptyString()][string]$GateClientHostTemplate = '',
		[AllowEmptyString()][string]$ClientPublicHost = '',
		[ValidateSet('Local', 'Cluster')][string]$GateExternalTrafficPolicy = 'Local',
		[bool]$DeploysBattle = $false,
		[ValidateSet('deployment', 'agones')][string]$BattleOrchestrator = 'deployment',
		[AllowNull()][object]$AgonesFleetCrdPresent = $null,
		[bool]$DeploysGateway = $false,
		[AllowEmptyString()][string]$GatewayIngressHost = '',
		[AllowEmptyString()][string]$GatewayIngressTlsSecret = '',
		[AllowEmptyString()][string]$GatewayTrustedProxies = '',
		[bool]$LoginDevPasswordAuth = $false,
		[ValidateSet('dev', 'staging', 'prod')][string]$ReleaseProfile = 'dev'
	)

	$contract = Get-ClientEntryContract
	$errors = [System.Collections.Generic.List[string]]::new()
	$warnings = [System.Collections.Generic.List[string]]::new()
	$external = ($ClientEntryMode -eq 'external')
	$zoneList = @($Zones | Where-Object { $null -ne $_ })

	# ── 地址参数的形状:两种模式都校验(podip 下不生效,但拼错了也该在切 external 之前发现)。
	if (-not [string]::IsNullOrEmpty($ClientPublicHost) -and -not (Test-ClientEntryHostName -Value $ClientPublicHost)) {
		$errors.Add("-ClientPublicHost 只能是裸主机名或 IPv4 字面量,不能带 scheme / 端口 / 路径(当前 '$ClientPublicHost')。")
	}
	if (-not [string]::IsNullOrEmpty($GateClientHostTemplate)) {
		$sample = $GateClientHostTemplate.Replace('{zone}', 'z').Replace('{ordinal}', '0')
		if ($sample.Contains('{') -or $sample.Contains('}') -or -not (Test-ClientEntryHostName -Value $sample)) {
			$errors.Add("-GateClientHostTemplate 只能含字母、数字、'.'、'-' 与 {zone} / {ordinal} 占位,不能带 scheme / 端口 / 路径(当前 '$GateClientHostTemplate')。")
		}
	}
	if (-not $external -and (-not [string]::IsNullOrEmpty($ClientPublicHost) -or -not [string]::IsNullOrEmpty($GateClientHostTemplate))) {
		$warnings.Add("-ClientEntryMode podip 下 -ClientPublicHost / -GateClientHostTemplate 不生效(客户端直接拿 PodIP);要对集群外开放请加 -ClientEntryMode external。")
	}

	# zone 记录可以是 hashtable 或 pscustomobject;经 Get-ClientEntryMember 读,缺成员按 $null(约定 6)。
	$zoneRecords = @(foreach ($zone in $zoneList) {
		[pscustomobject]@{
			Name     = [string](Get-ClientEntryMember -Object $zone -Path 'Name')
			Replicas = [int](Get-ClientEntryMember -Object $zone -Path 'GateReplicas')
			Base     = [int](Get-ClientEntryMember -Object $zone -Path 'GateNodePortBase')
			Explicit = [bool](Get-ClientEntryMember -Object $zone -Path 'GateNodePortBaseExplicit')
		}
	})

	# ── GateServiceType 的大小写(约定 7):只要本次部署 zone 就校验,两种模式都会把它写进 Service 的 type:
	#    (external 的每序号 Service、podip 单副本的 gate-entry)。ValidateSet / -notin 都不分大小写,
	#    nodeport 能一路放行到 apply 才被 API server 拒收 —— 那时另一种形态的 gate 可能已经删了。
	$canonicalServiceType = ConvertTo-ClientEntryCanonicalName -Value $GateServiceType -Allowed @('ClusterIP', 'NodePort', 'LoadBalancer')
	if ($zoneRecords.Count -gt 0 -and $null -ne $canonicalServiceType -and $canonicalServiceType -cne $GateServiceType) {
		$errors.Add("-GateServiceType 必须按 K8s 的大小写写成 $canonicalServiceType(当前 '$GateServiceType'):PowerShell 参数校验不分大小写,API server 分,原样写进 Service 的 type 会被拒收。")
	}

	# ── gate(external,只在本次部署 zone 时校验)。以下比较一律用规范化后的取值、区分大小写。
	if ($external -and $zoneRecords.Count -gt 0) {
		if ($canonicalServiceType -cnotin @('NodePort', 'LoadBalancer')) {
			$errors.Add("-ClientEntryMode external 要求 -GateServiceType 为 NodePort 或 LoadBalancer(当前 $GateServiceType)。")
		}
		elseif ($canonicalServiceType -ceq 'LoadBalancer' -and [string]::IsNullOrEmpty($GateClientHostTemplate)) {
			$errors.Add("-GateServiceType LoadBalancer 需要 -GateClientHostTemplate:LB 地址由云厂商在 Service 建好后才分配,gate 启动时拿不到,只能由 DNS 模板给出(D88)。")
		}

		if (-not [string]::IsNullOrEmpty($GateClientHostTemplate) -and -not $GateClientHostTemplate.Contains('{ordinal}')) {
			foreach ($zone in $zoneRecords) {
				if ($zone.Replicas -gt 1) {
					$errors.Add("zone $($zone.Name):gate 副本数 $($zone.Replicas) > 1,-GateClientHostTemplate 必须含 {ordinal},否则所有 gate 自报同一个地址。")
				}
			}
		}

		if ($canonicalServiceType -ceq 'NodePort') {
			if ($zoneRecords.Count -gt 1) {
				$missing = @($zoneRecords | Where-Object { -not $_.Explicit } | ForEach-Object { $_.Name })
				if ($missing.Count -gt 0) {
					$errors.Add("多 zone 部署且 -ClientEntryMode external + NodePort 时,每个 zone 必须在 zones 配置里显式写 gateNodePortBase(缺:$($missing -join ', '))。")
				}
			}

			$ranges = @()
			foreach ($zone in $zoneRecords) {
				$replicas = $zone.Replicas
				if ($replicas -le 0) { continue }
				$low = $zone.Base
				$high = $low + $replicas - 1
				$ranges += [pscustomobject]@{ Name = $zone.Name; Low = $low; High = $high }
				if ($low -lt $contract.NodePortMin -or $high -gt $contract.NodePortMax) {
					$errors.Add("zone $($zone.Name):gate nodePort 段 [$low-$high] 越界:gateNodePortBase 必须 ≥$($contract.NodePortMin) 且 gateNodePortBase + 副本数 - 1 ≤$($contract.NodePortMax)。")
				}
				elseif ($high -gt $contract.RecommendedNodePortMax) {
					$warnings.Add("zone $($zone.Name):gate nodePort 段 [$low-$high] 超出推荐静态子段 $($contract.NodePortMin)-$($contract.RecommendedNodePortMax),可能与集群动态分配的 nodePort 冲突(D88)。")
				}
			}
			for ($i = 0; $i -lt $ranges.Count; $i++) {
				for ($j = $i + 1; $j -lt $ranges.Count; $j++) {
					$a = $ranges[$i]
					$b = $ranges[$j]
					if ($a.Low -le $b.High -and $b.Low -le $a.High) {
						$errors.Add("zone $($a.Name) 与 zone $($b.Name) 的 gate nodePort 段重叠:[$($a.Low)-$($a.High)] 与 [$($b.Low)-$($b.High)]。")
					}
				}
			}
		}

		if ($GateExternalTrafficPolicy -eq 'Cluster') {
			$warnings.Add("-GateExternalTrafficPolicy Cluster 会对入站连接做 SNAT:gate 看到的是节点 IP 而不是玩家真实 IP,G9 按源 IP 限流不可用(D89)。")
		}
	}

	# ── battle(只在本次部署 battle 池时校验)。
	if ($DeploysBattle) {
		if ($external -and $BattleOrchestrator -eq 'deployment') {
			$warnings.Add("-ClientEntryMode external 配 -BattleOrchestrator deployment:battle 以 hostPort 运行,每个节点只能跑 1 个副本且没有忙碌保护,滚动时在打的局作废;只用于验证与回退,生产请用 -BattleOrchestrator agones(D86)。")
		}
		if ($BattleOrchestrator -eq 'agones' -and $AgonesFleetCrdPresent -eq $false) {
			$errors.Add("-BattleOrchestrator agones 需要集群已安装 Agones:找不到 CRD $($contract.AgonesFleetResource)。")
		}
	}

	# ── gateway Ingress(D91)。
	$trustedProxyList = ConvertTo-ClientEntryCidrList -CommaSeparated $GatewayTrustedProxies
	$invalidCidrs = Get-ClientEntryInvalidCidrs -CommaSeparated $GatewayTrustedProxies
	if ($invalidCidrs.Count -gt 0) {
		$errors.Add("-GatewayTrustedProxies 含非法 CIDR:$($invalidCidrs -join ', ')(gateway 会静默跳过非法项,漏掉的网段让全体玩家共用一个限流桶)。")
	}
	if (-not [string]::IsNullOrWhiteSpace($GatewayIngressHost)) {
		if ($trustedProxyList.Count -eq 0) {
			$errors.Add("配置了 -GatewayIngressHost 却没有 -GatewayTrustedProxies:gateway 会把 Ingress controller 的地址当成所有玩家的来源 IP,全体玩家共用一个限流桶(D91)。")
		}
		$sampleHost = $GatewayIngressHost.Replace('{zone}', 'z')
		$parsedIp = $null
		if ($sampleHost.Contains('{') -or -not (Test-ClientEntryHostName -Value $sampleHost) -or [System.Net.IPAddress]::TryParse($sampleHost, [ref]$parsedIp)) {
			$errors.Add("-GatewayIngressHost 必须是 DNS 名(可含 {zone}),不能是 IP、不能带 scheme / 端口 / 路径(当前 '$GatewayIngressHost')。")
		}
		if (-not $DeploysGateway) {
			$warnings.Add("给了 -GatewayIngressHost,但本次不部署 gateway(Ingress 只随 zone-up 的 Java 服务生成,需要 -JavaSvcRegistry 且不带 -SkipJavaSvc),本次不会生成 Ingress。")
		}
		elseif ($zoneList.Count -gt 1 -and -not $GatewayIngressHost.Contains('{zone}')) {
			$warnings.Add("多个 zone 会各生成一个 host 相同的 gateway Ingress,ingress-nginx 准入会拒绝后到者;请在 -GatewayIngressHost 里用 {zone} 区分。")
		}
	}
	elseif ($external -and $DeploysGateway) {
		$warnings.Add("-ClientEntryMode external 但未配置 -GatewayIngressHost:gateway 的 HTTP 入口(/api/assign-gate 等)没有集群外入口(D91)。")
	}
	if (-not [string]::IsNullOrWhiteSpace($GatewayIngressTlsSecret) -and [string]::IsNullOrWhiteSpace($GatewayIngressHost)) {
		$warnings.Add("给了 -GatewayIngressTlsSecret 但没有 -GatewayIngressHost,不会生成 Ingress,TLS 设置被忽略。")
	}

	# ── login 开发口令认证(2b §7;与 release_preflight.ps1 的 debug.login.devpassword 同一门禁)。
	if ($LoginDevPasswordAuth -and $ReleaseProfile -ne 'dev') {
		$errors.Add("-LoginDevPasswordAuth 只允许 -ReleaseProfile dev(当前 $ReleaseProfile):DevPasswordAuth 绕过账号库口令体系。")
	}

	return [pscustomobject]@{
		Errors   = [string[]]$errors.ToArray()
		Warnings = [string[]]$warnings.ToArray()
	}
}

<#
.SYNOPSIS
	输出 preflight 结果:警告逐条 Write-Warning,有错误就一次性 throw(全部列出)。
#>
function Assert-ClientEntryPreflightResult {
	param([Parameter(Mandatory = $true)]$Result)

	foreach ($warning in @($Result.Warnings)) {
		Write-Warning "[client-entry] $warning"
	}
	$errorList = @($Result.Errors)
	if ($errorList.Count -gt 0) {
		throw ("集群外入口预检失败($($errorList.Count) 项):`n  - " + ($errorList -join "`n  - "))
	}
}

<#
.SYNOPSIS
	探测集群是否注册了 Agones Fleet 资源(fleets.agones.dev)。

.DESCRIPTION
	只读 agones.dev 这一个组的发现文档:`kubectl get --raw /apis/agones.dev/v1`。
	  - 成功:资源列表里有 fleets 即 $true,没有即 $false;
	  - 服务端回 NotFound(组 / 版本未注册)= 没装,$false;
	  - 其余失败(集群不可达、凭据错、Forbidden、返回体不是 JSON)一律 throw:查不到不等于"没装",
	    既不能当 false 也不能当 true 放行(fail-closed)。
	不用 `kubectl api-resources`:它会拉**全部**组的发现,集群里任何一个无关的聚合 APIService 不可用
	(常见的是 metrics-server)就以非 0 退出,在没装 Agones 的默认路径上把 infra-up 整个阻断。agones.dev 是
	CRD,由 apiserver 进程内提供,只读它自己的组就不受别的组影响。
	也不用 `get crd`:要集群级 CRD 读权限,按 namespace 授权的部署账号会被拒;/apis/* 的发现接口
	由内置 system:discovery 授给所有已认证用户。DryRun 由调用方跳过,不调本函数。
#>
function Test-AgonesFleetCrdPresent {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig
	)

	$contract = Get-ClientEntryContract
	$discoveryPath = "/apis/$($contract.AgonesApiGroup)/$($contract.AgonesApiVersion)"
	$result = Invoke-ClientEntryKubectl -KubectlArgs @('get', '--raw', $discoveryPath) `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
	if ($result.ExitCode -ne 0) {
		# kubectl 把服务端 404 打成 "Error from server (NotFound): the server could not find the requested resource"。
		# 区分大小写只认这个 reason,Forbidden / 连接失败等都不命中。
		if ($result.Stderr -cmatch '\(NotFound\)') {
			return $false
		}
		throw "无法确认集群是否安装了 Agones(kubectl get --raw $discoveryPath 失败,exit $($result.ExitCode)):$($result.Stderr)"
	}

	$document = $null
	try { $document = $result.Stdout | ConvertFrom-Json -ErrorAction Stop }
	catch { throw "无法确认集群是否安装了 Agones:$discoveryPath 的返回不是 JSON($($_.Exception.Message))。" }
	if ($null -eq $document) {
		throw "无法确认集群是否安装了 Agones:$discoveryPath 返回为空。"
	}
	$resourceNames = @(@(Get-ClientEntryMember -Object $document -Path 'resources') |
		ForEach-Object { [string](Get-ClientEntryMember -Object $_ -Path 'name') })
	return ($resourceNames -ccontains $contract.AgonesFleetPlural)
}

# ─────────────────────────────────────────────────────────────────
# 8. 集群操作:Fleet 就绪等待、模式切换清理(-KubeContext / -KubeConfig 必填;均支持 -DryRun)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	内部辅助:有界轮询,直到 Probe 判定完成。

.DESCRIPTION
	Probe:param($ProbeArgs) → { Done = <bool>; State = <string> }。Probe 自己调 kubectl(带 -AllowFailure),
	瞬时失败返回 Done=$false 并把原因写进 State,不中断轮询;Probe 抛异常属于逻辑错误,直接向上抛。
	输入经 ProbeArgs 显式传入,不用 GetNewClosure:闭包绑定到动态模块,看不到 dot-source 进脚本作用域的本库函数。
	截止时间用单调时钟(Stopwatch,AGENTS §11.3),睡眠截到剩余预算。
	超时后逐条执行 DiagnosticArgs(每项一组 kubectl 参数)打出现场,再 throw。
#>
function Wait-ForClientEntryCondition {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$Kind,
		[Parameter(Mandatory = $true)][string]$Name,
		[Parameter(Mandatory = $true)][scriptblock]$Probe,
		[Parameter(Mandatory = $true)][hashtable]$ProbeArgs,
		[Parameter(Mandatory = $true)][object[]]$DiagnosticArgs,
		[Parameter(Mandatory = $true)][int]$TimeoutSeconds,
		[Parameter(Mandatory = $true)][int]$PollIntervalSeconds,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig
	)

	$stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
	$budget = [TimeSpan]::FromSeconds($TimeoutSeconds)
	$lastState = 'not polled'
	while ($true) {
		$verdict = & $Probe $ProbeArgs
		$lastState = [string](Get-ClientEntryMember -Object $verdict -Path 'State')
		if ([bool](Get-ClientEntryMember -Object $verdict -Path 'Done')) {
			Write-Host "$Kind ready: namespace=$Namespace name=$Name $lastState"
			return
		}

		$remaining = $budget - $stopwatch.Elapsed
		if ($remaining -le [TimeSpan]::Zero) { break }
		Start-Sleep -Milliseconds ([int][Math]::Min($PollIntervalSeconds * 1000, $remaining.TotalMilliseconds))
	}

	Write-Host "$Kind not ready: namespace=$Namespace name=$Name ($lastState)"
	foreach ($diagnostic in $DiagnosticArgs) {
		Invoke-ClientEntryKubectl -KubectlArgs $diagnostic -KubeContext $KubeContext -KubeConfig $KubeConfig -AllowFailure
	}
	throw "$Kind not ready after ${TimeoutSeconds}s: namespace=$Namespace name=$Name ($lastState)"
}

# 内部辅助:kubectl JSON 里的 creationTimestamp(ConvertFrom-Json 可能已转成 DateTime,也可能仍是 RFC3339 串)→ UTC;
# 读不出返回 $null。
function ConvertTo-ClientEntryUtcTime {
	param([AllowNull()]$Value)
	if ($null -eq $Value) { return $null }
	if ($Value -is [datetime]) { return $Value.ToUniversalTime() }
	$parsed = [datetime]::MinValue
	$styles = [System.Globalization.DateTimeStyles]::AssumeUniversal -bor [System.Globalization.DateTimeStyles]::AdjustToUniversal
	if ([datetime]::TryParse([string]$Value, [System.Globalization.CultureInfo]::InvariantCulture, $styles, [ref]$parsed)) {
		return $parsed
	}
	return $null
}

# 内部辅助:string→string 映射(labels / annotations)的规范化文本,供相等比较;缺席 = 空映射。
function Get-ClientEntryStringMapText {
	param([AllowNull()]$Map)
	if ($null -eq $Map) { return '' }
	$pairs = if ($Map -is [System.Collections.IDictionary]) {
		foreach ($key in $Map.Keys) { "$key=$($Map[$key])" }
	}
	else {
		foreach ($property in $Map.PSObject.Properties) { "$($property.Name)=$($property.Value)" }
	}
	return ((@($pairs) | Sort-Object -CaseSensitive) -join "`n")
}

<#
.SYNOPSIS
	Fleet 滚动就绪判据(纯函数,契约测试可直接喂 JSON 对象):只认当前模板的 GameServerSet 上的实例,
	外加旧 GameServerSet 上仍在打的 Allocated 实例。返回 { Done; State }。

.DESCRIPTION
	Fleet.status 的 ready / allocated 是**全部** GameServerSet 的合计:换镜像或改模板触发滚动时,旧 GameServerSet
	的 Ready 实例、以及被 allocationOverflow 打上排空标签的 Allocated 实例都会让合计立刻达标,
	-WaitReady 会在新版本一个都没就绪时返回成功。所以逐个 GameServerSet 判定,全部满足才 Done:
	  1. 当前 = creationTimestamp 最新的那个(Agones 每换一次模板建一个新的);最新的时间戳并列时无法判定,继续等。
	  2. 它的模板 labels / annotations 与 Fleet 当前模板一致。apply 之后 Fleet 控制器还没建出新 GameServerSet 时,
	     "最新"的仍是旧模板那个,不能拿它交差。本库 Fleet 模板带 mmorpg.io/build(随镜像变)与 sidecar 配置哈希,
	     换镜像 / 改日志配置都会反映在这里。**残留**:只改了 env 等 spec 字段的那次 apply,控制器建出新
	     GameServerSet 之前的那一瞬(通常亚秒级)这条挡不住。
	  3. 当前 GameServerSet 自己扩容到位:readyReplicas + allocatedReplicas ≥ spec.replicas。
	  4. 当前 GameServerSet 的 ready + allocated,加上旧 GameServerSet 上仍在打的 allocated(排空中,打完即回收),
	     ≥ ExpectedReplicas。旧 GameServerSet 的 Ready 实例一律不计。
	allocated 计入的理由:战斗进行中的实例是 Allocated,在线环境重跑 -WaitReady 只看 Ready 会把"正在打"误判成
	没就绪、超时报失败。没有 GameServerSet、时间戳读不出等无法判定的情况一律 Done=$false,由调用方等到超时。
#>
function Measure-FleetRolloutReadiness {
	param(
		[Parameter(Mandatory = $true)]$Fleet,
		[Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$GameServerSets,
		[Parameter(Mandatory = $true)][int]$ExpectedReplicas
	)

	$sets = @($GameServerSets | Where-Object { $null -ne $_ })
	if ($sets.Count -eq 0) {
		return [pscustomobject]@{ Done = $false; State = "no GameServerSet yet expected=$ExpectedReplicas" }
	}
	$dated = @()
	foreach ($set in $sets) {
		$setName = [string](Get-ClientEntryMember -Object $set -Path 'metadata', 'name')
		$created = ConvertTo-ClientEntryUtcTime -Value (Get-ClientEntryMember -Object $set -Path 'metadata', 'creationTimestamp')
		if ($null -eq $created) {
			return [pscustomobject]@{ Done = $false; State = "GameServerSet '$setName' has no readable creationTimestamp" }
		}
		$dated += [pscustomobject]@{ Set = $set; Name = $setName; Created = $created }
	}
	$ordered = @($dated | Sort-Object -Property Created -Descending)
	$current = $ordered[0]
	if ($ordered.Count -gt 1 -and $ordered[1].Created -eq $current.Created) {
		return [pscustomobject]@{ Done = $false; State = "ambiguous current GameServerSet: '$($current.Name)' and '$($ordered[1].Name)' share creationTimestamp" }
	}

	$templatePath = @('spec', 'template', 'metadata')
	$fleetLabels = Get-ClientEntryStringMapText -Map (Get-ClientEntryMember -Object $Fleet -Path ($templatePath + 'labels'))
	$fleetAnnotations = Get-ClientEntryStringMapText -Map (Get-ClientEntryMember -Object $Fleet -Path ($templatePath + 'annotations'))
	$setLabels = Get-ClientEntryStringMapText -Map (Get-ClientEntryMember -Object $current.Set -Path ($templatePath + 'labels'))
	$setAnnotations = Get-ClientEntryStringMapText -Map (Get-ClientEntryMember -Object $current.Set -Path ($templatePath + 'annotations'))
	if ($fleetLabels -cne $setLabels -or $fleetAnnotations -cne $setAnnotations) {
		return [pscustomobject]@{ Done = $false; State = "newest GameServerSet '$($current.Name)' does not carry the current Fleet template yet" }
	}

	$ready = [int](Get-ClientEntryMember -Object $current.Set -Path 'status', 'readyReplicas')
	$allocated = [int](Get-ClientEntryMember -Object $current.Set -Path 'status', 'allocatedReplicas')
	$target = [int](Get-ClientEntryMember -Object $current.Set -Path 'spec', 'replicas')
	$oldAllocated = 0
	foreach ($old in @($ordered | Select-Object -Skip 1)) {
		$oldAllocated += [int](Get-ClientEntryMember -Object $old.Set -Path 'status', 'allocatedReplicas')
	}
	$done = (($ready + $allocated) -ge $target) -and (($ready + $allocated + $oldAllocated) -ge $ExpectedReplicas)
	return [pscustomobject]@{
		Done  = $done
		State = "current=$($current.Name) ready=$ready allocated=$allocated target=$target old_sets=$($ordered.Count - 1) old_allocated=$oldAllocated expected=$ExpectedReplicas"
	}
}

<#
.SYNOPSIS
	有界轮询 Fleet 与它的 GameServerSet,直到 Measure-FleetRolloutReadiness 判定就绪,截止 TimeoutSeconds(默认 300s)。

.DESCRIPTION
	`kubectl rollout status` 对 Fleet 无效(不是 apps/v1),所以自己轮询。判据见 Measure-FleetRolloutReadiness:
	不看 Fleet.status 的合计(滚动时旧 GameServerSet 的实例会让它提前达标)。
#>
function Wait-ForFleetReady {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$FleetName,
		[Parameter(Mandatory = $true)][ValidateRange(0, 65535)][int]$ExpectedReplicas,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig,
		[ValidateRange(1, 3600)][int]$TimeoutSeconds = 300,
		[ValidateRange(1, 60)][int]$PollIntervalSeconds = 5,
		[switch]$DryRun
	)

	if ($DryRun) {
		Write-Host "[dry-run] wait fleet/$FleetName -n ${Namespace}: poll current-template GameServerSet ready + allocated (+ draining allocated) >= $ExpectedReplicas, timeout ${TimeoutSeconds}s"
		return
	}
	if ($ExpectedReplicas -eq 0) {
		Write-Host "Fleet $FleetName expects 0 replicas, nothing to wait for (namespace=$Namespace)."
		return
	}

	$contract = Get-ClientEntryContract
	$probeArgs = @{
		Namespace        = $Namespace
		FleetName        = $FleetName
		ExpectedReplicas = $ExpectedReplicas
		KubeContext      = $KubeContext
		KubeConfig       = $KubeConfig
		SetResource      = $contract.AgonesGameServerSetResource
		FleetLabel       = "$($contract.AgonesFleetNameLabelKey)=$FleetName"
	}
	$probe = {
		param($p)
		$fleetResult = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'fleet', $p.FleetName, '-n', $p.Namespace, '-o', 'json') `
			-KubeContext $p.KubeContext -KubeConfig $p.KubeConfig -CaptureOutput -AllowFailure
		if ($fleetResult.ExitCode -ne 0) {
			return [pscustomobject]@{ Done = $false; State = "kubectl get fleet failed (exit $($fleetResult.ExitCode)): $($fleetResult.Stderr)" }
		}
		$setsResult = Invoke-ClientEntryKubectl -KubectlArgs @('get', $p.SetResource, '-n', $p.Namespace, '-l', $p.FleetLabel, '-o', 'json') `
			-KubeContext $p.KubeContext -KubeConfig $p.KubeConfig -CaptureOutput -AllowFailure
		if ($setsResult.ExitCode -ne 0) {
			return [pscustomobject]@{ Done = $false; State = "kubectl get $($p.SetResource) failed (exit $($setsResult.ExitCode)): $($setsResult.Stderr)" }
		}
		$fleet = $null
		$setList = $null
		try {
			$fleet = $fleetResult.Stdout | ConvertFrom-Json -ErrorAction Stop
			$setList = $setsResult.Stdout | ConvertFrom-Json -ErrorAction Stop
		}
		catch {
			return [pscustomobject]@{ Done = $false; State = "unparsable fleet / GameServerSet json: $($_.Exception.Message)" }
		}
		if ($null -eq $fleet) {
			return [pscustomobject]@{ Done = $false; State = 'kubectl get fleet returned empty output' }
		}
		$sets =@(@(Get-ClientEntryMember -Object $setList -Path 'items') | Where-Object { $null -ne $_ })
		return (Measure-FleetRolloutReadiness -Fleet $fleet -GameServerSets $sets -ExpectedReplicas $p.ExpectedReplicas)
	}
	$diagnostics = @(
		@('get', 'fleet', $FleetName, '-n', $Namespace, '-o', 'wide'),
		@('get', $contract.AgonesGameServerSetResource, '-n', $Namespace, '-l', $probeArgs.FleetLabel, '-o', 'wide'),
		@('get', 'gameservers', '-n', $Namespace, '-l', $probeArgs.FleetLabel, '-o', 'wide'),
		@('describe', 'fleet', $FleetName, '-n', $Namespace)
	)
	Wait-ForClientEntryCondition -Namespace $Namespace -Kind 'fleet' -Name $FleetName `
		-Probe $probe -ProbeArgs $probeArgs -DiagnosticArgs $diagnostics -TimeoutSeconds $TimeoutSeconds -PollIntervalSeconds $PollIntervalSeconds `
		-KubeContext $KubeContext -KubeConfig $KubeConfig
}

<#
.SYNOPSIS
	有界轮询 gate StatefulSet 的 .status.readyReplicas ≥ ExpectedReplicas。

.DESCRIPTION
	**不能用 k8s_deploy.ps1 的 Wait-ForStatefulSetReady**:它走 `kubectl rollout status statefulset/...`,
	而 kubectl 对 updateStrategy: OnDelete 的 StatefulSet 直接报错
	("rollout status is only available for RollingUpdate strategy type"),-WaitReady 会立刻失败。
	OnDelete 下 apply 新模板不会重建现有 Pod,这里等到的是"现存 Pod 已就绪";新模板何时生效由
	k8s_gate_drain.ps1 逐个排空删 Pod 决定。就绪 = tcpSocket 探针通 = 已发布进 etcd(见 New-NodeDeploymentYaml)。
#>
function Wait-ForGateStatefulSetReady {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][ValidateRange(0, 65535)][int]$ExpectedReplicas,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig,
		[string]$StatefulSetName = 'gate',
		[ValidateRange(1, 3600)][int]$TimeoutSeconds = 300,
		[ValidateRange(1, 60)][int]$PollIntervalSeconds = 5,
		[switch]$DryRun
	)

	if ($DryRun) {
		Write-Host "[dry-run] wait statefulset/$StatefulSetName -n ${Namespace}: poll .status.readyReplicas >= $ExpectedReplicas, timeout ${TimeoutSeconds}s"
		return
	}
	if ($ExpectedReplicas -eq 0) {
		Write-Host "StatefulSet $StatefulSetName expects 0 replicas, nothing to wait for (namespace=$Namespace)."
		return
	}

	$probeArgs = @{
		Namespace        = $Namespace
		Name             = $StatefulSetName
		ExpectedReplicas = $ExpectedReplicas
		KubeContext      = $KubeContext
		KubeConfig       = $KubeConfig
	}
	$probe = {
		param($p)
		$result = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'statefulset', $p.Name, '-n', $p.Namespace, '-o', 'json') `
			-KubeContext $p.KubeContext -KubeConfig $p.KubeConfig -CaptureOutput -AllowFailure
		if ($result.ExitCode -ne 0) {
			return [pscustomobject]@{ Done = $false; State = "kubectl get statefulset failed (exit $($result.ExitCode)): $($result.Stderr)" }
		}
		$sts = $null
		try { $sts = $result.Stdout | ConvertFrom-Json -ErrorAction Stop }
		catch { return [pscustomobject]@{ Done = $false; State = "unparsable statefulset json: $($_.Exception.Message)" } }
		# status.readyReplicas 带 omitempty:0 个就绪时字段缺席,按 0 计(约定 6)。
		$ready = [int](Get-ClientEntryMember -Object $sts -Path 'status', 'readyReplicas')
		return [pscustomobject]@{ Done = ($ready -ge $p.ExpectedReplicas); State = "ready=$ready expected=$($p.ExpectedReplicas)" }
	}
	$diagnostics = @(
		@('get', 'pods', '-n', $Namespace, '-l', "app=$StatefulSetName", '-o', 'wide'),
		@('describe', 'statefulset', $StatefulSetName, '-n', $Namespace)
	)
	Wait-ForClientEntryCondition -Namespace $Namespace -Kind 'statefulset' -Name $StatefulSetName `
		-Probe $probe -ProbeArgs $probeArgs -DiagnosticArgs $diagnostics -TimeoutSeconds $TimeoutSeconds -PollIntervalSeconds $PollIntervalSeconds `
		-KubeContext $KubeContext -KubeConfig $KubeConfig
}

# 待删资源条目。Kind 用 kubectl 认的资源名;Selector 非空时按标签删(Name 忽略);
# ApiGroup 非空 = 该资源依赖 CRD,集群没装时直接跳过;KicksPlayers = 删它会断开在线玩家 / 作废在打的局。
# KicksPlayers 的条目必须给 CurrentShape(集群现状的叫法,如 "statefulset gate")与 KeepHint(保持现状要显式传的
# 参数,如 "-ClientEntryMode external"):Remove-ClientEntryObsoleteResources 拒绝删除时原样写进报错文本。
function New-ClientEntryObsoleteItem {
	param(
		[Parameter(Mandatory = $true)][string]$Kind,
		[AllowEmptyString()][string]$Name = '',
		[AllowEmptyString()][string]$Selector = '',
		[AllowEmptyString()][string]$ApiGroup = '',
		[Parameter(Mandatory = $true)][string]$Reason,
		[bool]$KicksPlayers = $false,
		[AllowEmptyString()][string]$CurrentShape = '',
		[AllowEmptyString()][string]$KeepHint = ''
	)
	if ($KicksPlayers -and ([string]::IsNullOrWhiteSpace($CurrentShape) -or [string]::IsNullOrWhiteSpace($KeepHint))) {
		throw "New-ClientEntryObsoleteItem: KicksPlayers 的条目必须给 -CurrentShape 与 -KeepHint($Kind $Name)。"
	}
	return [pscustomobject]@{
		Kind = $Kind; Name = $Name; Selector = $Selector; ApiGroup = $ApiGroup; Reason = $Reason
		KicksPlayers = $KicksPlayers; CurrentShape = $CurrentShape; KeepHint = $KeepHint
	}
}

# 内部辅助:条目 → kubectl 目标参数(按名或按标签)。
function Get-ClientEntryObsoleteTarget {
	param([Parameter(Mandatory = $true)]$Resource)
	if ([string]::IsNullOrEmpty($Resource.Selector)) { return ,@($Resource.Kind, $Resource.Name) }
	return ,@($Resource.Kind, '-l', $Resource.Selector)
}

<#
.SYNOPSIS
	切换 -ClientEntryMode 时 gate 侧要删除的"另一种形态"(zone namespace)。纯函数,只给清单。

.DESCRIPTION
	kind 不同、名字相同的工作负载 apply 不会互相回收(etcd / kafka 迁 StatefulSet 时同一个坑):
	gate Deployment 与 gate StatefulSet 同时在线 = 两套 gate 同时注册、同时收玩家。
	切换本身就是整台踢人(ingress_final §6 第 2 批的维护窗口):KicksPlayers 的条目只有调用方显式给了
	-AllowDisruptiveSwitch 才会被 Remove-ClientEntryObsoleteResources 删除。
#>
function Get-GateObsoleteResources {
	param(
		[Parameter(Mandatory = $true)][ValidateSet('podip', 'external')][string]$ClientEntryMode,
		[Parameter(Mandatory = $true)][ValidateRange(0, 65535)][int]$GateReplicas
	)

	$contract = Get-ClientEntryContract
	$items = @()
	if ($ClientEntryMode -eq 'external') {
		$items += New-ClientEntryObsoleteItem -Kind 'deployment' -Name 'gate' -KicksPlayers $true `
			-CurrentShape 'deployment gate' -KeepHint '-ClientEntryMode podip' `
			-Reason 'gate 从 Deployment 切到 StatefulSet(-ClientEntryMode external):旧 Deployment 上的玩家全部断线,需要重新登录。'
		$items += New-ClientEntryObsoleteItem -Kind 'service' -Name $contract.GateEntryServiceName `
			-Reason 'external 模式由每序号 Service 取代单一 gate-entry(D90),删除残留。'
		return ,$items
	}

	$items += New-ClientEntryObsoleteItem -Kind 'statefulset' -Name 'gate' -KicksPlayers $true `
		-CurrentShape 'statefulset gate' -KeepHint '-ClientEntryMode external' `
		-Reason 'gate 从 StatefulSet 切回 Deployment(-ClientEntryMode podip):StatefulSet 上的玩家全部断线,需要重新登录。'
	$items += New-ClientEntryObsoleteItem -Kind 'service' -Selector $contract.GateOrdinalLabelKey `
		-Reason 'podip 模式没有每序号 Service,删除残留(否则继续占着 nodePort)。'
	$items += New-ClientEntryObsoleteItem -Kind 'service' -Name $contract.GateHeadlessServiceName `
		-Reason 'podip 模式没有 gate StatefulSet,删除它的 headless Service。'
	$items += New-ClientEntryObsoleteItem -Kind 'poddisruptionbudget' -Name $contract.GatePdbName `
		-Reason 'maxUnavailable: 0 的 gate PDB 会挡住 Deployment 版 gate 的驱逐与节点维护,podip 模式删除。'
	if (-not (Test-GateEntryServiceWanted -ClientEntryMode $ClientEntryMode -GateReplicas $GateReplicas)) {
		$items += New-ClientEntryObsoleteItem -Kind 'service' -Name $contract.GateEntryServiceName `
			-Reason "gate 副本数为 $GateReplicas(≠1):单一 gate-entry 会把约一半票据分到别的 gate 被 token_gate_node_mismatch 拒绝(D90),删除残留。"
	}
	return ,$items
}

<#
.SYNOPSIS
	切换 -BattleOrchestrator 时 battle 侧要删除的另一种形态(infra namespace)。纯函数。
	-BattleReplicas 0 = "不装配、不删除已部署的池",调用方此时不应调用本函数。
#>
function Get-BattleObsoleteResources {
	param([Parameter(Mandatory = $true)][ValidateSet('deployment', 'agones')][string]$BattleOrchestrator)

	$contract = Get-ClientEntryContract
	if ($BattleOrchestrator -eq 'agones') {
		return ,@(New-ClientEntryObsoleteItem -Kind 'deployment' -Name 'battle' -KicksPlayers $true `
			-CurrentShape 'deployment battle' -KeepHint '-BattleOrchestrator deployment' `
			-Reason 'battle 从 Deployment 切到 Agones Fleet:旧 Deployment 上在打的战斗全部作废(先公告,ingress_final §6 第 2 批)。')
	}
	return ,@(New-ClientEntryObsoleteItem -Kind $contract.AgonesFleetResource -Name 'battle' -ApiGroup $contract.AgonesApiGroup -KicksPlayers $true `
		-CurrentShape 'fleet battle' -KeepHint '-BattleOrchestrator agones' `
		-Reason 'battle 从 Agones Fleet 切回 Deployment:Fleet 上在打的战斗全部作废。')
}

<#
.SYNOPSIS
	只探测、不删除:Get-GateObsoleteResources / Get-BattleObsoleteResources 清单里哪些条目在集群里确实存在,
	以及其中会踢人(KicksPlayers)的条目的拒绝文本。

.DESCRIPTION
	返回 { Present = @({ Resource; Names }); Refusals = string[] }(都可能为空)。Refusals 与 -AllowDisruptiveSwitch 无关,
	由调用方决定拒绝还是放行 —— 这是"会踢人"判据与拒绝文本的唯一一份:Remove-ClientEntryObsoleteResources 用它做删除前的闸,
	k8s_deploy.ps1 的集群现状预检(Assert-ClientEntryClusterState)在任何写操作之前用它把 battle 与各 zone 一次判完。
	判据:agones.dev 条目先看 CRD 是否注册(-AgonesFleetCrdPresent 为 $null 时遇到才探测一次;没装就不可能有残留,跳过),
	再逐条 `get --ignore-not-found -o name`。任何 kubectl 失败都 throw(查不到不等于不存在,fail-closed)。
	-KicksPlayersOnly:只探测会踢人的条目(预检只要拒绝文本,不为残留清理多打 get)。
#>
function Find-ClientEntryObsoleteResources {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Resources,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig,
		# Test-AgonesFleetCrdPresent 的结果;$null = 未探测。
		[AllowNull()][object]$AgonesFleetCrdPresent = $null,
		[switch]$KicksPlayersOnly
	)

	$contract = Get-ClientEntryContract
	$agonesPresent = $AgonesFleetCrdPresent
	$present = @()
	foreach ($resource in @($Resources | Where-Object { $null -ne $_ })) {
		if ($KicksPlayersOnly -and -not $resource.KicksPlayers) { continue }
		if ($resource.ApiGroup -eq $contract.AgonesApiGroup) {
			if ($null -eq $agonesPresent) {
				$agonesPresent = Test-AgonesFleetCrdPresent -KubeContext $KubeContext -KubeConfig $KubeConfig
			}
			if (-not $agonesPresent) { continue }
		}
		$target = Get-ClientEntryObsoleteTarget -Resource $resource
		$found = Invoke-ClientEntryKubectl -KubectlArgs (@('get') + $target + @('-n', $Namespace, '--ignore-not-found', '-o', 'name')) `
			-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput
		$names = @($found.Stdout -split "`r?`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne '' })
		if ($names.Count -eq 0) { continue }
		$present += [pscustomobject]@{ Resource = $resource; Names = $names }
	}

	$refusals = @(foreach ($entry in @($present | Where-Object { $_.Resource.KicksPlayers })) {
		"当前集群是 $($entry.Resource.CurrentShape)($($entry.Names -join ', '),namespace=$Namespace),本次参数会删除它:$($entry.Resource.Reason)" +
			" 要保持现状请显式传 $($entry.Resource.KeepHint);确认要切换请加 -AllowDisruptiveSwitch。"
	})
	return [pscustomobject]@{
		Present  = $present
		Refusals = [string[]]$refusals
	}
}

<#
.SYNOPSIS
	按 Get-GateObsoleteResources / Get-BattleObsoleteResources 的清单删除确实存在的资源,删前逐条 Write-Warning。

.DESCRIPTION
	两趟:先逐条 `get --ignore-not-found -o name` 探测,再统一删除,只对真的删到东西的条目告警(常态重复部署不刷屏)。
	**会踢人的闸(AGENTS §11.3 fail-closed)**:-ClientEntryMode / -BattleOrchestrator 不粘滞,漏传就落回默认值
	podip / deployment。在 external 或 agones 的环境里重跑时漏传一个参数,清单就会指向正在服务的 gate StatefulSet
	或 battle Fleet —— 整个 zone 踢下线、在打的局作废,podip 下集群外还根本连不上。所以第一趟探测后,只要有
	KicksPlayers 的条目在集群里确实存在、而调用方没给 -AllowDisruptiveSwitch,就 throw 并一个都不删
	(不留"删了 gate-entry、没删 gate"的半切换状态)。报错逐条写明"当前集群是 <CurrentShape>"与保持现状要
	显式传的参数(KeepHint),契约测试断言这段文本。确认要切换的维护窗口里,调用方显式加 -AllowDisruptiveSwitch。
	ApiGroup 为 agones.dev 的条目:集群没装 Agones 就不可能有残留,直接跳过(否则 kubectl 报
	"the server doesn't have a resource type")。任何 kubectl 失败都 throw(fail-closed):
	旧工作负载删不掉就继续部署,会得到两套 gate / battle 同时在线。
	DryRun 只打印将执行的删除命令,不探测集群;会踢人的条目额外注明"真实执行需要 -AllowDisruptiveSwitch"。
#>
function Remove-ClientEntryObsoleteResources {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Resources,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig,
		# 确认本次就是要切换形态、接受踢人 / 作废在打的局。k8s_deploy.ps1 暴露同名参数原样透传,不得默认打开。
		[switch]$AllowDisruptiveSwitch,
		[switch]$DryRun
	)

	if ($DryRun) {
		foreach ($resource in $Resources) {
			$target = Get-ClientEntryObsoleteTarget -Resource $resource
			$guard = if ($resource.KicksPlayers -and -not $AllowDisruptiveSwitch) { '   [若集群里存在:未给 -AllowDisruptiveSwitch,真实执行会拒绝并中止]' } else { '' }
			Write-Host "[dry-run] kubectl delete $($target -join ' ') -n $Namespace --ignore-not-found   # $($resource.Reason)$guard"
		}
		return
	}

	# 第一趟:只探测,不删(判据与拒绝文本见 Find-ClientEntryObsoleteResources)。
	$probe = Find-ClientEntryObsoleteResources -Namespace $Namespace -Resources $Resources -KubeContext $KubeContext -KubeConfig $KubeConfig

	# 闸:会踢人的条目确实存在且未确认 → 全部列出后拒绝,一个都不删。
	if ($probe.Refusals.Count -gt 0 -and -not $AllowDisruptiveSwitch) {
		throw ("拒绝删除正在服务的工作负载(模式参数不粘滞,漏传会落回默认值 podip / deployment),未删除任何资源:`n  - " + ($probe.Refusals -join "`n  - "))
	}

	# 第二趟:删。
	foreach ($entry in @($probe.Present)) {
		Write-Warning "[client-entry] $($entry.Resource.Reason) 删除:$($entry.Names -join ', ')(namespace=$Namespace)"
		Invoke-ClientEntryKubectl -KubectlArgs (@('delete') + $entry.Names + @('-n', $Namespace, '--ignore-not-found')) `
			-KubeContext $KubeContext -KubeConfig $KubeConfig
	}
}

<#
.SYNOPSIS
	external 模式下 gate 缩容后,删除序号 ≥ Replicas 的每序号 Service。

.DESCRIPTION
	StatefulSet 缩容会删掉高序号 Pod,但 apply 不会回收不再生成的 Service:它继续占着自己的 nodePort,
	之后别的 zone 用到这个端口时 apply 会被集群拒绝。只删标签值能解析成整数且 ≥ Replicas 的,
	认不出的标签值只告警不删。DryRun 只打印意图。
#>
function Remove-StaleGateOrdinalServices {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][ValidateRange(0, 65535)][int]$Replicas,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeContext,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$KubeConfig,
		[switch]$DryRun
	)

	$contract = Get-ClientEntryContract
	$labelKey = $contract.GateOrdinalLabelKey
	if ($DryRun) {
		Write-Host "[dry-run] kubectl delete service -l $labelKey -n $Namespace   # 只删序号 >= $Replicas 的每序号 Service"
		return
	}

	$result = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'service', '-n', $Namespace, '-l', $labelKey, '-o', 'json') `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput
	$list = $result.Stdout | ConvertFrom-Json
	$stale = @()
	foreach ($item in @(@(Get-ClientEntryMember -Object $list -Path 'items') | Where-Object { $null -ne $_ })) {
		$serviceName = [string](Get-ClientEntryMember -Object $item -Path 'metadata', 'name')
		$rawOrdinal = [string](Get-ClientEntryMember -Object $item -Path 'metadata', 'labels', $labelKey)
		$ordinal = 0
		if (-not [int]::TryParse($rawOrdinal, [ref]$ordinal)) {
			Write-Warning "[client-entry] service/$serviceName 的 $labelKey 标签值 '$rawOrdinal' 不是整数,不删除,请人工确认。"
			continue
		}
		if ($ordinal -ge $Replicas) {
			$stale += "service/$serviceName"
		}
	}
	if ($stale.Count -eq 0) { return }

	Write-Warning "[client-entry] gate 副本数为 $Replicas,删除多余的每序号 Service:$($stale -join ', ')(namespace=$Namespace)"
	Invoke-ClientEntryKubectl -KubectlArgs (@('delete') + $stale + @('-n', $Namespace, '--ignore-not-found')) `
		-KubeContext $KubeContext -KubeConfig $KubeConfig
}
