param(
	[Parameter(Mandatory = $true)]
	[ValidateSet("zone-up", "zone-down", "zone-status", "all-up", "all-down", "all-status", "infra-up", "infra-down", "infra-status")]
	[string]$Command,

	[string]$ZoneName = "yesterday",
	[int]$ZoneId = 101,
	[string]$NamespacePrefix = "mmorpg-zone",
	[string]$InfraNamespace = "mmorpg-infra",

	[string]$ZonesConfigPath = "",

	# 留空 = 用 $NodeImageRepository + git 短 sha 组出**不可变** tag。
	# 以前这里硬编码 ":latest",而 latest 在 registry 上会被覆盖:新旧
	# Deployment revision 指向同一个 digest,`kubectl rollout undo`
	# (docs/ops/release-checklist.md §E.2 三级回滚)就退回同一个镜像,
	# 等于什么都没换。要显式发某个版本时直接传完整引用。
	[string]$NodeImage = "",
	[string]$NodeImageRepository = "ghcr.io/luyuancpp/mmorpg-node",
	# 发布档位。dev 允许占位密钥回落 + 可变 tag;staging/prod 一律要求
	# 从环境变量注入密钥、且 tag 必须不可变,查不到就 fail-closed。
	[ValidateSet("dev", "staging", "prod")]
	[string]$ReleaseProfile = "dev",
	# 留空 = 按 tag 是否可变自动推导(不可变 tag -> IfNotPresent,可变 tag -> Always)。
	# 以前无条件写死 IfNotPresent,配上 latest 就是"节点上有旧层就永远不拉新的"。
	[ValidateSet("", "Always", "IfNotPresent", "Never")]
	[string]$ImagePullPolicy = "",
	# 跳过发布预检。只给契约测试和"明知配置未就绪的演练"用,
	# staging/prod 真发布加这个开关等于把门禁拆了。
	[switch]$SkipPreflight,
	[ValidateSet("custom", "managed-cloud", "bare-metal")]
	[string]$OpsProfile = "custom",
	[ValidateSet("dev", "prod-like", "prod")]
	[string]$KafkaProfile = "prod",
	[int]$KafkaBrokerRetentionMs = 0,
	[int]$KafkaDbTaskRetentionMs = 0,
	[int]$KafkaRetentionCheckIntervalMs = 0,
	[long]$KafkaRetentionBytes = 0,
	[long]$KafkaSegmentBytes = 0,
	[string]$KafkaHeapOpts = "",
	[int]$CentreReplicas = 1,
	[int]$GateReplicas = 2,
	# Scene 角色拆分(见 docs/ops/scene-node-role-split.md):
	#   -SceneReplicas        legacy 单池,生成一个名为 scene 的 Deployment(SCENE_NODE_TYPE=0)
	#   -SceneWorldReplicas   拆分池 scene-world    (SCENE_NODE_TYPE=0)
	#   -SceneInstanceReplicas拆分池 scene-instance (SCENE_NODE_TYPE=1)
	# 兼容规则:只要 -SceneWorldReplicas / -SceneInstanceReplicas 任一 > 0(或 zones
	# 配置里出现 scene_world / scene_instance 任一键),就进入拆分模式,legacy scene 被忽略。
	# -1 表示"未指定",用于区分「显式写 0(缩到零副本)」和「压根没配」。
	[int]$SceneReplicas = 4,
	[int]$SceneWorldReplicas = -1,
	[int]$SceneInstanceReplicas = -1,
	# Scene Node 的编排方式。
	#   deployment - 普通 K8s Deployment(默认,行为与接 Agones 之前一致)
	#   agones     - agones.dev/v1 Fleet,由 Agones 管理进程生命周期
	# 刻意**不**做"检测集群装没装 Agones 就自动切换":自动切换会让同一条命令
	# 在两个集群上产出不同的工作负载类型,出事时无法从命令还原现场。
	[ValidateSet("deployment", "agones")]
	[string]$SceneOrchestrator = "deployment",
	# Agones GameServer 的优雅退出预算。Scene Node 收到 SIGTERM 后要把在场玩家
	# 全量存盘(main.cpp 的 exitAllPlayers),给够时间,否则会丢存档。
	[int]$SceneTerminationGracePeriodSeconds = 60,
	# Agones health 探测。periodSeconds 必须大于 C++ 侧的 health 心跳间隔
	# (LifecycleOptions::healthInterval,默认 2s),否则正常心跳也会被判失败。
	[int]$AgonesHealthPeriodSeconds = 10,
	[int]$AgonesHealthFailureThreshold = 3,
	[int]$AgonesHealthInitialDelaySeconds = 30,
	# 高密度容量:给每个 GameServer 挂一个 rooms Counter,SceneManager 通过
	# GameServerAllocation 原子预占名额。
	#
	# Counters and Lists 在 Agones 里是 **beta**,需要集群侧显式打开 FeatureGate
	# (CountsAndLists=true),所以这里默认关闭,由运维确认版本后再开。
	[switch]$AgonesHighDensity,
	# 每个 Scene Node 进程能承载多少个房间。
	# **没有默认值是有意的**:C++ Scene Node 是单 EventLoop,实际容量必须按
	# 帧耗时、AOI、玩家数和内存压测确定(压测口径见 CLAUDE.md §6)。
	# 开了 -AgonesHighDensity 却不给这个值,脚本会直接报错而不是替你猜一个。
	[int]$AgonesRoomCapacity = 0,
	# FleetAutoscaler:按剩余房间容量自动增减 Fleet replicas。
	# 需要 -AgonesHighDensity(依赖 rooms Counter)。
	[switch]$AgonesAutoscale,
	# 集群里始终保持的**空闲房间**数量。低于它就扩 Pod,高于上界就缩。
	# 这是缓冲区,不是容量:太小会让高峰期玩家等 Pod 调度(几十秒),
	# 太大是白烧钱。取值应当覆盖"一个调度周期内可能新增的房间数"。
	[int]$AgonesBufferRooms = 5,
	# Fleet 的 Pod 副本数下界 / 上界。
	#
	# 注意单位换算:Agones 的 Counter 策略里 minCapacity / maxCapacity 是**整个
	# Fleet 的总房间容量**,不是副本数。所以要乘 -AgonesRoomCapacity 才是要写进
	# YAML 的值。之前直接把副本数塞进容量字段,差了 RoomCapacity 倍
	# (MaxReplicas=8 + RoomCapacity=6 本该是 48 个房间的容量,却写成了 8,
	# 等于把 Fleet 钉死在 2 个 Pod)。
	#
	# maxCapacity 在 Agones 1.58 里是**必填且 >= 1**,所以 -AgonesAutoscale
	# 必须显式给 -AgonesMaxReplicas —— 不替运维猜上界(猜小了高峰期扩不上去,
	# 猜大了烧钱),与 -AgonesRoomCapacity 同一个口径。
	[int]$AgonesMinReplicas = 1,
	[int]$AgonesMaxReplicas = 0,
	# 与 C++ gRPC server 的默认值保持一致。传 0 时 Deployment / Fleet
	# 都不注入环境变量,由进程默认值接管；正数则两种编排写入同一个值。
	[int]$GrpcServerMaxPollers = 8,
	[ValidateSet("ClusterIP", "NodePort", "LoadBalancer")]
	[string]$GateServiceType = "NodePort",
	[int]$GateServicePort = 18000,

	[switch]$SkipInfra,
	[switch]$SkipGoSvc,
	[string]$GoSvcRegistry = "",
	# 同 NodeImage:留空 = git 短 sha,不再默认 latest。
	[string]$GoSvcTag = "",
	[switch]$SkipJavaSvc,
	[string]$JavaSvcRegistry = "",
	[string]$JavaSvcTag = "",
	[switch]$DryRun,
	[switch]$WaitReady,
	[int]$WaitTimeoutSeconds = 180,

	[string]$KubeContext = "",
	[string]$KubeConfig = ""
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")
$K8sRoot = Join-Path $RepoRoot "deploy\k8s"
$InfraManifestsDir = Join-Path $K8sRoot "manifests\infra"
$GoSvcManifestsDir = Join-Path $K8sRoot "manifests\go-svc"
$JavaSvcManifestsDir = Join-Path $K8sRoot "manifests\java-svc"

. (Join-Path $ScriptDir "lib\release_common.ps1")

# ─────────────────────────────────────────────────────────────────
# 不可变版本戳
# ─────────────────────────────────────────────────────────────────

# 留空的镜像参数一律回落到 git 短 sha(脏树带 -dirty 后缀)。
# 这样"构建出来的 tag"和"部署引用的 tag"在同一份工作树状态下必然相同,
# 而不同版本之间必然不同 —— 这是 rollout undo 能真的回滚的前提。
$script:ReleaseStamp = Get-GitReleaseStamp -RepoRoot $RepoRoot

function Resolve-ReleaseTag {
    param([Parameter(Mandatory = $true)][string]$Purpose)

    if (-not $script:ReleaseStamp.Ok) {
        throw "无法生成不可变镜像 tag($Purpose):$($script:ReleaseStamp.Reason)。请显式传入 tag / 完整镜像引用。"
    }
    return $script:ReleaseStamp.Tag
}

$script:NodeImageExplicit = -not [string]::IsNullOrWhiteSpace($NodeImage)
if (-not $script:NodeImageExplicit) {
	$NodeImage = "{0}:{1}" -f $NodeImageRepository, (Resolve-ReleaseTag -Purpose "NodeImage")
}

# 调用方显式指定了 NodeImage 却没指定 go/java tag 时,跟随 NodeImage 的 tag。
# 否则会出现"C++ 节点是版本 X、Go 服务是当前工作树版本"这种半新半旧的部署,
# 正是本轮要消灭的那类版本错配。
$script:FollowTag = Get-ImageTagFromRef -ImageRef $NodeImage
if ([string]::IsNullOrWhiteSpace($GoSvcTag)) {
	$GoSvcTag = if ($script:NodeImageExplicit -and -not [string]::IsNullOrWhiteSpace($script:FollowTag)) { $script:FollowTag } else { Resolve-ReleaseTag -Purpose "GoSvcTag" }
}
if ([string]::IsNullOrWhiteSpace($JavaSvcTag)) {
	$JavaSvcTag = if ($script:NodeImageExplicit -and -not [string]::IsNullOrWhiteSpace($script:FollowTag)) { $script:FollowTag } else { Resolve-ReleaseTag -Purpose "JavaSvcTag" }
}

if ([string]::IsNullOrWhiteSpace($ImagePullPolicy)) {
	$ImagePullPolicy = Resolve-ImagePullPolicy -ImageRef $NodeImage
}

<#
.SYNOPSIS
	staging/prod 路径显式拒绝可变 tag。
#>
function Assert-ImmutableReleaseImages {
	if ($ReleaseProfile -eq 'dev') { return }

	$refs = @($NodeImage)
	if (-not $SkipGoSvc -and -not [string]::IsNullOrWhiteSpace($GoSvcRegistry)) { $refs += "$GoSvcRegistry/mmorpg-*:$GoSvcTag" }
	if (-not $SkipJavaSvc -and -not [string]::IsNullOrWhiteSpace($JavaSvcRegistry)) { $refs += "$JavaSvcRegistry/mmorpg-*:$JavaSvcTag" }

	foreach ($ref in $refs) {
		$tag = Get-ImageTagFromRef -ImageRef $ref
		$chk = Test-ImmutableImageTag -Tag $tag -RejectDirty:($ReleaseProfile -eq 'prod')
		if (-not $chk.Ok) {
			throw "ReleaseProfile=$ReleaseProfile 拒绝该镜像引用 '$ref':$($chk.Reason)"
		}
	}
}

<#
.SYNOPSIS
	staging/prod 部署前跑发布预检,非 0 退出码直接阻断。

.DESCRIPTION
	"有检查器但没人调用"是最常见的失效模式,所以门禁挂在生成器入口而不是文档里。
#>
function Invoke-ReleasePreflight {
	if ($ReleaseProfile -eq 'dev') { return }
	if ($SkipPreflight) {
		Write-Warning "已通过 -SkipPreflight 跳过发布预检(ReleaseProfile=$ReleaseProfile)。真实发布不应该走到这里。"
		return
	}

	$preflight = Join-Path $ScriptDir "release_preflight.ps1"
	if (-not (Test-Path $preflight)) {
		throw "release_preflight.ps1 不存在: $preflight(fail-closed:预检脚本缺失不等于预检通过)"
	}

	$refs = @($NodeImage)
	& $preflight -ReleaseProfile $ReleaseProfile -ImageTag (Get-ImageTagFromRef -ImageRef $NodeImage) -ImageRef $refs
	if ($LASTEXITCODE -ne 0) {
		throw "release preflight 未通过(exit=$LASTEXITCODE),部署被阻断。修完配置再重跑,或用 -SkipPreflight 明确承担风险。"
	}
}

# ─────────────────────────────────────────────────────────────────
# 密钥注入(替代以前写死在生成器里的占位常量)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	解析要写进 ConfigMap 的密钥。只在写操作(*-up)前调用。

.DESCRIPTION
	以前这两处是生成器**自己**把占位串写进生产 ConfigMap:
	  GateTokenSecret: "change-me-in-production-use-a-strong-random-key"
	  gate.token-secret: change-me-in-production-use-a-strong-random-key
	也就是说,就算运维把仓库里 7 个文件全改对了,部署出来的还是公开常量。

	**必须是懒解析**:prod 档位下缺环境变量会 throw,如果在脚本顶层就解析,
	`zone-down` / `zone-status` 这些止血和排查命令也会被一起打死 —— 出事的时候
	连状态都看不了是灾难。
#>
function Initialize-InjectedSecrets {
	$script:GateTokenSecret = Resolve-InjectedSecret -EnvName "MMORPG_GATE_TOKEN_SECRET" `
		-DevFallback "change-me-in-production-use-a-strong-random-key" `
		-ReleaseProfile $ReleaseProfile -Purpose "Gate 连接令牌 HMAC 共享密钥" -MinLength 32

	# login 的内部调用方验签密钥(Secrets.InternalAuth)。生产模式恒强制验签且
	# 配置关不掉,缺这一项 login 直接拒绝启动 —— 以前这份 ConfigMap 根本没有它。
	#
	# dev 回落值刻意与 GateToken 那把**不同**:secrets.go 里有跨用途复用主密钥的
	# 检查,填成一样在生产会被判为复用而拒启,本地也会打 WARN。
	$script:InternalAuthSecret = Resolve-InjectedSecret -EnvName "MMORPG_INTERNAL_AUTH_SECRET" `
		-DevFallback "change-me-in-production-internal-auth-shared-key" `
		-ReleaseProfile $ReleaseProfile -Purpose "内部调用方身份声明验签密钥(callerauth)" -MinLength 32

	# db 服务连 MySQL 的凭据。生成器以前写死 root/root,而
	# deploy/k8s/manifests/infra/mysql.yaml 的 MYSQL_ROOT_PASSWORD 根本不是 root
	# —— 也就是说这份 ConfigMap 在真集群里连不上库。
	$script:MysqlUser = Resolve-InjectedSecret -EnvName "MMORPG_MYSQL_USER" `
		-DevFallback "root" -ReleaseProfile $ReleaseProfile -Purpose "MySQL 用户名" -MinLength 1
	$script:MysqlPassword = Resolve-InjectedSecret -EnvName "MMORPG_MYSQL_PASSWORD" `
		-DevFallback "Mmorpg#2026db" -ReleaseProfile $ReleaseProfile -Purpose "MySQL 密码" -MinLength 12
	$script:RedisPassword = Resolve-InjectedSecret -EnvName "MMORPG_REDIS_PASSWORD" `
		-DevFallback "" -ReleaseProfile $ReleaseProfile -Purpose "Redis 密码" -MinLength 12
	$script:GatewayDbUser = Resolve-InjectedSecret -EnvName "MMORPG_GATEWAY_DB_USER" `
		-DevFallback "root" -ReleaseProfile $ReleaseProfile -Purpose "Java Gateway 数据源用户名" -MinLength 1
	$script:GatewayDbPassword = Resolve-InjectedSecret -EnvName "MMORPG_GATEWAY_DB_PASSWORD" `
		-DevFallback "123456" -ReleaseProfile $ReleaseProfile -Purpose "Java Gateway 数据源密码" -MinLength 12
}

# ─────────────────────────────────────────────────────────────────
# 权威配置值(跨文件单一真相)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	从各服务 etc/*.yaml 读一个"契约关键值",查不到直接 throw。

.DESCRIPTION
	这些值以前在生成器里各写各的常数,于是漂移出过一堆压测期已知会炸的值
	(Locker.PlayerLockTTL 5 vs 120、Kafka.PartitionCnt 5 vs 10、
	 Database.MaxOpenConn 10 vs 60 …)。产物又不入库,漂移只能在线上炸出来。
	现在改成运行期从服务自己的 etc/*.yaml 取,单一真相在服务侧。
#>
function Get-AuthoritativeScalar {
	param(
		[Parameter(Mandatory = $true)][string]$RelativePath,
		[Parameter(Mandatory = $true)][string]$KeyPath
	)

	$full = Join-Path $RepoRoot ($RelativePath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
	$r = Get-YamlScalar -Path $full -KeyPath $KeyPath
	if (-not $r.Found) {
		throw "生成 ConfigMap 失败:$RelativePath 里查不到 $KeyPath。$($r.Reason)(fail-closed:不替你猜一个常数)"
	}
	return $r.Value
}

# Go micro-service catalogue: name → { configMapName, manifestFile, port, configFlag, configFileName }
$GoSvcCatalogue = @{
	db              = @{ ConfigMap = "go-svc-db-config";              Manifest = "db.yaml";              Port = 6000;  ConfigFlag = "-f";              ConfigFile = "db.yaml";                    ImageName = "mmorpg-db" }
	"data-service"  = @{ ConfigMap = "go-svc-data-service-config";    Manifest = "data-service.yaml";    Port = 9000;  ConfigFlag = "-f";              ConfigFile = "data_service.yaml";             ImageName = "mmorpg-data-service" }
	login           = @{ ConfigMap = "go-svc-login-config";           Manifest = "login.yaml";           Port = 50000; ConfigFlag = "-loginService";   ConfigFile = "login.yaml";                  ImageName = "mmorpg-login" }
	"player-locator"= @{ ConfigMap = "go-svc-player-locator-config";  Manifest = "player-locator.yaml";  Port = 50100; ConfigFlag = "-f";              ConfigFile = "player_locator.yaml";           ImageName = "mmorpg-player-locator" }
	"scene-manager" = @{ ConfigMap = "go-svc-scene-manager-config";   Manifest = "scene-manager.yaml";   Port = 60000; ConfigFlag = "-f";              ConfigFile = "scene_manager_service.yaml";    ImageName = "mmorpg-scene-manager" }
}

# Java service catalogue
$JavaSvcCatalogue = @{
	auth    = @{ ConfigMap = "java-svc-auth-config";    Manifest = "auth.yaml";    HttpPort = 5555; GrpcPort = 5556; ImageName = "mmorpg-auth" }
	gateway = @{ ConfigMap = "java-svc-gateway-config"; Manifest = "gateway.yaml"; HttpPort = 8081; GrpcPort = 0;    ImageName = "mmorpg-gateway" }
}

# 注意:OpsProfile 的副本数下限只作用于 legacy 单池参数 $SceneReplicas。
# 拆分池(scene_world / scene_instance)一律以调用方 / zones 配置写的值为准,
# 不做静默抬高 —— 拆分模式下副本比例是运维显式决策(world ≈ 1.2x instance),
# 被脚本改写会让"实际部署 != 配置文件"。
function Apply-OpsProfileDefaults {
	switch ($OpsProfile) {
		"managed-cloud" {
			$script:GateServiceType = "LoadBalancer"
			if ($CentreReplicas -lt 1) { $script:CentreReplicas = 1 }
			if ($GateReplicas -lt 2) { $script:GateReplicas = 2 }
			if ($SceneReplicas -lt 4) { $script:SceneReplicas = 4 }
		}
		"bare-metal" {
			$script:GateServiceType = "NodePort"
			if ($CentreReplicas -lt 1) { $script:CentreReplicas = 1 }
			if ($GateReplicas -lt 2) { $script:GateReplicas = 2 }
			if ($SceneReplicas -lt 4) { $script:SceneReplicas = 4 }
		}
		default {
		}
	}
}

function Apply-KafkaProfileDefaults {
	switch ($KafkaProfile) {
		"dev" {
			if ($KafkaBrokerRetentionMs -le 0) { $script:KafkaBrokerRetentionMs = 60000 }
			if ($KafkaDbTaskRetentionMs -le 0) { $script:KafkaDbTaskRetentionMs = 300000 }
			if ($KafkaRetentionCheckIntervalMs -le 0) { $script:KafkaRetentionCheckIntervalMs = 120000 }
			if ($KafkaRetentionBytes -le 0) { $script:KafkaRetentionBytes = 134217728 }
			if ($KafkaSegmentBytes -le 0) { $script:KafkaSegmentBytes = 16777216 }
			if ([string]::IsNullOrWhiteSpace($KafkaHeapOpts)) { $script:KafkaHeapOpts = "-Xms128m -Xmx256m" }
		}
		"prod-like" {
			if ($KafkaBrokerRetentionMs -le 0) { $script:KafkaBrokerRetentionMs = 300000 }
			if ($KafkaDbTaskRetentionMs -le 0) { $script:KafkaDbTaskRetentionMs = 600000 }
			if ($KafkaRetentionCheckIntervalMs -le 0) { $script:KafkaRetentionCheckIntervalMs = 120000 }
			if ($KafkaRetentionBytes -le 0) { $script:KafkaRetentionBytes = 536870912 }
			if ($KafkaSegmentBytes -le 0) { $script:KafkaSegmentBytes = 33554432 }
			if ([string]::IsNullOrWhiteSpace($KafkaHeapOpts)) { $script:KafkaHeapOpts = "-Xms512m -Xmx1g" }
		}
		default {
			if ($KafkaBrokerRetentionMs -le 0) { $script:KafkaBrokerRetentionMs = 300000 }
			if ($KafkaDbTaskRetentionMs -le 0) { $script:KafkaDbTaskRetentionMs = 900000 }
			if ($KafkaRetentionCheckIntervalMs -le 0) { $script:KafkaRetentionCheckIntervalMs = 120000 }
			if ($KafkaRetentionBytes -le 0) { $script:KafkaRetentionBytes = 536870912 }
			if ($KafkaSegmentBytes -le 0) { $script:KafkaSegmentBytes = 33554432 }
			if ([string]::IsNullOrWhiteSpace($KafkaHeapOpts)) { $script:KafkaHeapOpts = "-Xms512m -Xmx1g" }
		}
	}
}

function Show-ExposureProfileWarning {
	if ($OpsProfile -eq "custom" -and $GateServiceType -eq "LoadBalancer") {
		Write-Warning "Using OpsProfile=custom with GateServiceType=LoadBalancer. Ensure your cluster has a mature LB implementation; otherwise prefer NodePort + external L4 load balancer or use -OpsProfile bare-metal."
	}
	if ($GateServiceType -ne "ClusterIP") {
		# 暴露 Service 只是一半:客户端的 gate 地址不是从 Service 拿的,而是 login 从
		# etcd 读到 endpoint 后原样下发的(go/login internal/svc/servicecontext.go
		# CandidatesForZone)。那个 endpoint 是 POD_IP —— 集群内地址,只有 in-cluster 的
		# robot 连得上。所以 Service 暴露出去了,客户端拿到的仍是集群内地址。
		# 翻译要在 login 侧做,不在这个脚本里。
		Write-Warning "GateServiceType=$GateServiceType exposes the Service, but clients do not get gate's address from the Service: login hands out the etcd-registered endpoint, which is the cluster-internal POD_IP. External clients will still fail to connect until login is configured to translate node_id -> external address. See deploy/k8s/README.md."
	}
}

function Build-KubectlBaseArgs {
	$args = @()
	if (-not [string]::IsNullOrWhiteSpace($KubeContext)) {
		$args += @("--context", $KubeContext)
	}

	if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) {
		$args += @("--kubeconfig", $KubeConfig)
	}

	return ,$args
}

function Invoke-Kubectl {
	param(
		[Parameter(Mandatory = $true)]
		[string[]]$Args,
		[switch]$AllowFailure
	)

	$baseArgs = Build-KubectlBaseArgs
	$allArgs = @()
	$allArgs += $baseArgs
	$allArgs += $Args

	if ($DryRun) {
		Write-Host "[dry-run] kubectl $($allArgs -join ' ')"
		return
	}

	& kubectl @allArgs
	if (-not $AllowFailure -and $LASTEXITCODE -ne 0) {
		throw "kubectl failed: kubectl $($allArgs -join ' ')"
	}
}

function Invoke-KubectlWithInputFile {
	param(
		[Parameter(Mandatory = $true)]
		[string[]]$Args,

		[Parameter(Mandatory = $true)]
		[string]$InputContent
	)

	$sanitized = $InputContent -replace "`t", "    "

	# DryRun 下把真正会送进 kubectl 的 YAML 打出来。之前只打印临时文件路径,
	# 而临时文件在 finally 里就被删了 —— 等于 DryRun 无法验证任何生成结果。
	if ($DryRun) {
		$baseArgs = Build-KubectlBaseArgs
		$allArgs = @()
		$allArgs += $baseArgs
		$allArgs += $Args
		Write-Host "[dry-run] kubectl $($allArgs -join ' ') -f -"
		Write-Host "--- BEGIN MANIFEST ---"
		Write-Host $sanitized
		Write-Host "--- END MANIFEST ---"
		return
	}

	$tempFile = [System.IO.Path]::GetTempFileName()
	try {
		Set-Content -Path $tempFile -Value $sanitized -NoNewline -Encoding utf8NoBOM
		Invoke-Kubectl -Args ($Args + @("-f", $tempFile))
	}
	finally {
		Remove-Item -Path $tempFile -Force -ErrorAction SilentlyContinue
	}
}

function Get-ZoneNamespace {
	param([Parameter(Mandatory = $true)][string]$Name)
	return "{0}-{1}" -f $NamespacePrefix, $Name
}

function Ensure-KubectlAvailable {
	if ($DryRun) {
		return
	}

	$kubectl = Get-Command kubectl -ErrorAction SilentlyContinue
	if ($null -eq $kubectl) {
		throw "kubectl not found. Please install kubectl and configure cluster access first."
	}
}

function Ensure-Namespace {
	param([Parameter(Mandatory = $true)][string]$Namespace)

	$nsYaml = @"
apiVersion: v1
kind: Namespace
metadata:
  name: $Namespace
"@
	Invoke-KubectlWithInputFile -Args @("apply") -InputContent $nsYaml
}

function New-NodeConfigMapYaml {
	param(
		[Parameter(Mandatory = $true)][int]$CurrentZoneId,
		[Parameter(Mandatory = $true)][string]$ConfigName
	)

	# 这三个值以前要么写死、要么根本没生成,而这份 ConfigMap 是以 readOnly 整目录
	# 挂到 /app/bin/etc 的(见 New-NodeDeploymentYaml),会**完全遮蔽**镜像里那份
	# bin/etc/base_deploy_config.yaml。也就是说凡是这里没写的键,cpp 节点就当没配。
	#
	# NodeTTLSeconds 走权威取值而不是常数:仓库里那份是 180,注释记着
	# 2026-05-24 压测的结论 —— 60s 在 45k 开服浪涌下,keepalive 抖一帧就会误判
	# 租约过期并触发 kLeaseExpiredByEtcd FATAL 自杀(postmortem §A)。生成器写死
	# 60 等于把那次事故的修复悄悄退回去,而且再入屏障的推导也是按 180 写的
	# (docs/design/scene-owner-reentry-barrier.md §1)。
	$nodeTtlSeconds = Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Etcd.NodeTTLSeconds'
	$keepaliveInterval = Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Etcd.KeepaliveInterval'
	# gate 并发连接上限。0 = 不限,字段注释写明「生产必须配」。
	$gateMaxConnections = Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'GateMaxConnections'
	# gate 客户端令牌 HMAC 密钥。缺这一项时 gate 在 prod 运行模式下会 LOG_FATAL
	# 拒绝启动(cpp/nodes/gate/main.cpp::ValidateGateTokenSecretOrDie),而部署链
	# 从不设置 GATE_RUN_MODE,ResolveRunModeOnce 默认就是 prod —— 也就是说这一项
	# 缺失时 K8s 上的 gate 会直接 CrashLoopBackOff。
	$gateTokenSecret = $script:GateTokenSecret
	if ([string]::IsNullOrWhiteSpace($gateTokenSecret)) {
		# 与 Get-AuthoritativeScalar 同一条纪律:宁可在生成期炸,也不产出一份
		# 会让 gate 起不来的 ConfigMap。空串在这里是静默故障(要到 Pod
		# CrashLoopBackOff 才看得见),throw 是当场可读的错误。
		throw "生成 node ConfigMap 失败:GateTokenSecret 为空。请先调用 Initialize-InjectedSecrets(写操作路径会自动调用),或注入 MMORPG_GATE_TOKEN_SECRET。"
	}

	$baseDeployConfig = (@"
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  KeepaliveInterval: ${keepaliveInterval}
  NodeTTLSeconds: ${nodeTtlSeconds}
GateTokenSecret: "${gateTokenSecret}"
GateMaxConnections: ${gateMaxConnections}
TableDataDirectory: "../generated/generated_tables/"
DataRootDirectory: "/app/"
LogLevel: 1
HealthCheckInterval: 1
service_discovery_prefixes:
  - "SceneNodeService.rpc"
  - "GateNodeService.rpc"
  - "LoginNodeService.rpc"
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
  Topics:
    - "game-events"
  GroupID: "game-consumer-group"
  EnableAutoCommit: true
  AutoOffsetReset: "earliest"
"@) -replace "`t", "  "

	# SceneNodeType 在这里只是**文件基线**,gate / scene 共用同一份 ConfigMap。
	# 真正决定 scene pod 角色的是 Deployment 上的 SCENE_NODE_TYPE 环境变量:
	# cpp/libs/engine/config/config.cpp::readGameConfig 先读 yaml,再用 env 覆盖
	# (last-wins)。所以拆分模式不需要两份 ConfigMap,只需要两个 Deployment 各自
	# 带不同的 SCENE_NODE_TYPE。详见 docs/ops/scene-node-role-split.md §2。
	$gameConfig = @"
SceneNodeType: 0
ZoneId: $CurrentZoneId
zoneredis:
  host: "redis.${InfraNamespace}"
  port: 6379
  password: ""
  db: 0
  timeout: 3000
  max_connections: 100
  retry_interval: 1000
"@

	return @"
apiVersion: v1
kind: ConfigMap
metadata:
  name: $ConfigName
data:
  base_deploy_config.yaml: |
$($baseDeployConfig -split "`n" | ForEach-Object { "    $_" } | Out-String)
  game_config.yaml: |
$($gameConfig -split "`n" | ForEach-Object { "    $_" } | Out-String)
"@
}

function New-NodeDeploymentYaml {
	param(
		[Parameter(Mandatory = $true)][string]$NodeName,
		[Parameter(Mandatory = $true)][int]$Replicas,
		[Parameter(Mandatory = $true)][int]$RpcPort,
		[Parameter(Mandatory = $true)][string]$StartCommand,
		[Parameter(Mandatory = $true)][string]$ConfigMapName,
		# -1 = 不写 SCENE_NODE_TYPE(gate 等非 scene 角色)。
		[int]$SceneNodeType = -1
	)

	# 用数组逐行拼,最后 join 换行。
	# 旧写法是 `$block += @"..."@` 连续追加两个 here-string —— here-string 内容不含
	# 结尾换行,第二次追加会直接接在上一行尾部,生成非法 YAML。
	$extraEnvLines = @()
	if ($GrpcServerMaxPollers -gt 0) {
		$extraEnvLines += "`t`t`t- name: GRPC_SERVER_MAX_POLLERS"
		$extraEnvLines += "`t`t`t  value: `"$GrpcServerMaxPollers`""
	}
	if ($SceneNodeType -ge 0) {
		# 覆盖 ConfigMap 里的 SceneNodeType 基线,是角色拆分真正生效的那一步。
		$extraEnvLines += "`t`t`t- name: SCENE_NODE_TYPE"
		$extraEnvLines += "`t`t`t  value: `"$SceneNodeType`""
	}
	$grpcEnvBlock = $extraEnvLines -join "`n"

	return @"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $NodeName
spec:
  replicas: $Replicas
  selector:
	matchLabels:
	  app: $NodeName
  template:
	metadata:
	  labels:
		app: $NodeName
	spec:
	  containers:
		- name: $NodeName
		  image: $NodeImage
		  imagePullPolicy: $ImagePullPolicy
		  workingDir: /app/bin
		  command: ["/bin/sh", "-lc"]
		  args: ["$StartCommand"]
		  env:
			- name: POD_IP
			  valueFrom:
				fieldRef:
				  fieldPath: status.podIP
			- name: RPC_PORT
			  value: "$RpcPort"
			- name: NODE_PORT
			  value: "$RpcPort"
$grpcEnvBlock
		  volumeMounts:
			- name: node-config
			  mountPath: /app/bin/etc
			  readOnly: true
			- name: node-logs
			  mountPath: /app/bin/logs
		  ports:
			- containerPort: $RpcPort
			  name: rpc
	  volumes:
		- name: node-config
		  configMap:
			name: $ConfigMapName
		- name: node-logs
		  emptyDir: {}
"@
}

# 把镜像引用里的 tag 抽出来当 build 标签用。K8s label value 只允许
# 字母数字和 - _ .,且不超过 63 字符,所以其余字符一律替换成 '-'。
function Get-ImageBuildLabel {
	param([Parameter(Mandatory = $true)][string]$Image)

	$tag = "unknown"
	# 只看最后一个冒号后面的部分,并且要求它不含 '/',否则那是端口号不是 tag
	# (registry:5000/foo 这种)。
	$lastColon = $Image.LastIndexOf(':')
	if ($lastColon -ge 0) {
		$candidate = $Image.Substring($lastColon + 1)
		if ($candidate -notmatch '/' -and -not [string]::IsNullOrWhiteSpace($candidate)) {
			$tag = $candidate
		}
	}

	$sanitized = ($tag -replace '[^A-Za-z0-9._-]', '-')
	if ($sanitized.Length -gt 63) { $sanitized = $sanitized.Substring(0, 63) }
	$sanitized = $sanitized.Trim('-', '.', '_')
	if ([string]::IsNullOrWhiteSpace($sanitized)) { $sanitized = "unknown" }
	return $sanitized
}

<#
.SYNOPSIS
生成一个 Scene Node 的 agones.dev/v1 Fleet。

.DESCRIPTION
模型是 Agones 官方的 high-density GameServer:

    1 Agones GameServer = 1 个 Scene Node Pod / C++ 进程 = N 个动态创建的 ECS Scene 房间

**不是**一个 Scene 一个 GameServer。Scene 的创建/销毁/镜像共置/玩家路由仍然
全部由 Go SceneManager 负责,Agones 只负责进程级的 Ready / Allocated /
Unhealthy / Shutdown 和故障替换。

内部服务,不需要 UDP LB / HostPort / NodePort:portPolicy 用 None,
SceneManager 继续通过 etcd 里注册的 PodIP 找到 gRPC 地址。
#>
function New-SceneFleetYaml {
	param(
		[Parameter(Mandatory = $true)][string]$FleetName,
		[Parameter(Mandatory = $true)][int]$Replicas,
		[Parameter(Mandatory = $true)][int]$RpcPort,
		[Parameter(Mandatory = $true)][string]$StartCommand,
		[Parameter(Mandatory = $true)][string]$ConfigMapName,
		[Parameter(Mandatory = $true)][int]$SceneNodeType,
		[Parameter(Mandatory = $true)][string]$RoleLabel,
		[Parameter(Mandatory = $true)][string]$ZoneLabel,
		[Parameter(Mandatory = $true)][int]$ZoneIdLabel
	)

	$buildLabel = Get-ImageBuildLabel -Image $NodeImage

	$countersBlock = ""
	if ($AgonesHighDensity) {
		if ($AgonesRoomCapacity -le 0) {
			throw "-AgonesHighDensity requires -AgonesRoomCapacity <N>. 不要拍一个数字:每进程房间容量必须来自压测(帧耗时/AOI/玩家数/内存),见 docs/design/agones-scene-node-high-density.md §8。"
		}
		# rooms Counter。SceneManager 侧 internal/agones 的 CounterName 常量
		# 必须与这里同名,改一个就要改另一个。
		$countersBlock = @"

      counters:
        rooms:
          count: 0
          capacity: $AgonesRoomCapacity
"@
	}

	$grpcPollersEnv = ""
	if ($GrpcServerMaxPollers -gt 0) {
		$grpcPollersEnv = @"

                - name: GRPC_SERVER_MAX_POLLERS
                  value: "$GrpcServerMaxPollers"
"@
	}

	return @"
apiVersion: agones.dev/v1
kind: Fleet
metadata:
  name: $FleetName
  labels:
    app: $FleetName
    mmorpg.io/role: $RoleLabel
    mmorpg.io/zone: $ZoneLabel
    mmorpg.io/zone-id: "$ZoneIdLabel"
    mmorpg.io/build: $buildLabel
spec:
  replicas: $Replicas
  scheduling: Packed
  strategy:
    type: RollingUpdate
  template:
    metadata:
      labels:
        app: $FleetName
        mmorpg.io/role: $RoleLabel
        mmorpg.io/zone: $ZoneLabel
        mmorpg.io/zone-id: "$ZoneIdLabel"
        mmorpg.io/build: $buildLabel
    spec:
      ports:
        - name: rpc
          portPolicy: None
          containerPort: $RpcPort
          protocol: TCP
      health:
        disabled: false
        initialDelaySeconds: $AgonesHealthInitialDelaySeconds
        periodSeconds: $AgonesHealthPeriodSeconds
        failureThreshold: $AgonesHealthFailureThreshold$countersBlock
      template:
        metadata:
          labels:
            app: $FleetName
            mmorpg.io/role: $RoleLabel
            mmorpg.io/zone: $ZoneLabel
            mmorpg.io/zone-id: "$ZoneIdLabel"
            mmorpg.io/build: $buildLabel
        spec:
          terminationGracePeriodSeconds: $SceneTerminationGracePeriodSeconds
          containers:
            - name: $FleetName
              image: $NodeImage
              imagePullPolicy: $ImagePullPolicy
              workingDir: /app/bin
              command: ["/bin/sh", "-lc"]
              args: ["$StartCommand"]
              env:
                - name: POD_IP
                  valueFrom:
                    fieldRef:
                      fieldPath: status.podIP
                - name: RPC_PORT
                  value: "$RpcPort"
                - name: NODE_PORT
                  value: "$RpcPort"
                - name: SCENE_NODE_TYPE
                  value: "$SceneNodeType"
                - name: AGONES_ENABLED
                  value: "1"$grpcPollersEnv
              volumeMounts:
                - name: node-config
                  mountPath: /app/bin/etc
                  readOnly: true
                - name: node-logs
                  mountPath: /app/bin/logs
              ports:
                - containerPort: $RpcPort
                  name: rpc
          volumes:
            - name: node-config
              configMap:
                name: $ConfigMapName
            - name: node-logs
              emptyDir: {}
"@
}

<#
.SYNOPSIS
生成一个基于 rooms Counter 的 Agones FleetAutoscaler。

.DESCRIPTION
管的是**进程数(Pod)**,不是频道数。两件事分开:

  - 频道数(大世界一张图开几个频道)由 Go SceneManager 按玩家人数决定,
    见 docs/design/world-channel-autoscale.md。
  - 进程数由这里决定:房间总需求上来了就多开 Scene Node,空闲太多就回收。

用 Counter 策略而不是 Buffer 策略:高密度模型下"还剩几个 Ready 的
GameServer"没有意义(一个进程能装 N 个房间),真正该看的是"还剩几个空闲
**房间名额**"。

缩容风险:Agones 缩 Fleet 时会挑 Ready(未分配)的 GameServer 下手,
Allocated 的不会被动。但一个进程只要还有 1 个房间就是 Allocated,所以
缩容不会踢掉在玩的房间。空进程被回收是预期行为。
#>
function New-SceneFleetAutoscalerYaml {
	param(
		[Parameter(Mandatory = $true)][string]$FleetName,
		[Parameter(Mandatory = $true)][string]$RoleLabel,
		[Parameter(Mandatory = $true)][string]$ZoneLabel,
		[Parameter(Mandatory = $true)][int]$ZoneIdLabel
	)

	# 副本数 -> 总房间容量。Counter 策略的 min/maxCapacity 单位是"整个 Fleet 的
	# 房间总数",不是 Pod 数。实测(Agones 1.58,kubectl apply --dry-run=server):
	# 缺 maxCapacity 会被 CRD 校验直接拒掉 ——
	#   spec.policy.counter.maxCapacity: Invalid value: 0: should be >= 1
	if ($AgonesMaxReplicas -le 0) {
		throw "-AgonesAutoscale requires -AgonesMaxReplicas <N>. Agones 的 Counter 策略里 maxCapacity 必填(>=1),而且它是总房间容量而不是副本数;上界必须由运维显式给,脚本不替你猜。"
	}

	$minReplicas = $AgonesMinReplicas
	if ($minReplicas -lt 1) {
		# 每个大世界地图至少要有一个频道可落地,所以容量下界至少是一个进程。
		$minReplicas = 1
	}

	$minCapacity = $minReplicas * $AgonesRoomCapacity
	$maxCapacity = $AgonesMaxReplicas * $AgonesRoomCapacity
	if ($maxCapacity -lt $minCapacity) {
		throw "-AgonesMaxReplicas ($AgonesMaxReplicas) 小于 -AgonesMinReplicas ($minReplicas):maxCapacity($maxCapacity) < minCapacity($minCapacity),Agones 会拒绝。"
	}
	# bufferSize 必须留在容量上界之内,否则 autoscaler 永远满足不了缓冲区、
	# 会一直顶着 maxCapacity 扩容失败。
	if ($AgonesBufferRooms -ge $maxCapacity) {
		throw "-AgonesBufferRooms ($AgonesBufferRooms) 不小于总容量上界 ($maxCapacity = $AgonesMaxReplicas 副本 x $AgonesRoomCapacity 房间):缓冲区永远填不满,autoscaler 会一直顶在上界。"
	}

	return @"
apiVersion: autoscaling.agones.dev/v1
kind: FleetAutoscaler
metadata:
  name: $FleetName
  labels:
    app: $FleetName
    mmorpg.io/role: $RoleLabel
    mmorpg.io/zone: $ZoneLabel
    mmorpg.io/zone-id: "$ZoneIdLabel"
spec:
  fleetName: $FleetName
  policy:
    type: Counter
    counter:
      key: rooms
      bufferSize: $AgonesBufferRooms
      minCapacity: $minCapacity
      maxCapacity: $maxCapacity
"@
}

<#
.SYNOPSIS
生成 Agones SDK sidecar 在本 namespace 所需的 ServiceAccount + RoleBinding。

.DESCRIPTION
**没有这个,Agones 模式整套部署跑不起来。**

Agones 的 SDK sidecar 以 `agones-sdk` ServiceAccount 身份运行,而 Agones 的
helm 安装只在它自己那个 namespace(默认 `default`)里建了这套 RBAC。
zone namespace 是我们自己建的,里面没有 —— GameServer 控制器建 Pod 时会被
API server 拒掉,GameServer 直接进 Error:

    pods "scene-instance-xxxxx-yyyyy" is forbidden:
    error looking up service account mmorpg-zone-today/agones-sdk:
    serviceaccount "agones-sdk" not found

这一条**只有真集群能发现**:Fleet 对象本身完全合法,`kubectl apply
--dry-run=server` 一路绿灯,失败发生在控制器随后建 Pod 的时候。
(实测环境:Agones 1.58.0。)

ClusterRole `agones-sdk` 由 Agones 安装时创建,这里只做 namespace 级绑定,
不新建也不修改任何 ClusterRole —— 权限范围与 Agones 官方一致(events:
create/patch + gameservers:list),不额外放权。
#>
function New-AgonesSdkRbacYaml {
	param([Parameter(Mandatory = $true)][string]$Namespace)

	return @"
apiVersion: v1
kind: ServiceAccount
metadata:
  name: agones-sdk
  namespace: $Namespace
  labels:
    app: agones
    mmorpg.io/managed-by: k8s_deploy.ps1
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: agones-sdk-access
  namespace: $Namespace
  labels:
    app: agones
    mmorpg.io/managed-by: k8s_deploy.ps1
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: agones-sdk
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: User
    name: system:serviceaccount:$Namespace`:agones-sdk
"@
}

function New-GateServiceYaml {
	param(
		[Parameter(Mandatory = $true)][string]$ServiceName
	)

	return @"
apiVersion: v1
kind: Service
metadata:
  name: $ServiceName
spec:
  type: $GateServiceType
  selector:
    app: gate
  ports:
    - name: tcp-gate
      protocol: TCP
      port: $GateServicePort
      targetPort: rpc
"@
}

function Wait-ForDeploymentReady {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$DeploymentName
	)

	if ($DryRun) {
		Write-Host "[dry-run] kubectl rollout status deployment/$DeploymentName -n $Namespace --timeout ${WaitTimeoutSeconds}s"
		return
	}

	Invoke-Kubectl -Args @("rollout", "status", "deployment/$DeploymentName", "-n", $Namespace, "--timeout", ("{0}s" -f $WaitTimeoutSeconds)) -AllowFailure
	if ($LASTEXITCODE -eq 0) {
		return
	}

	Write-Host "Deployment not ready: namespace=$Namespace deployment=$DeploymentName"
	Invoke-Kubectl -Args @("get", "pods", "-n", $Namespace, "-o", "wide") -AllowFailure
	Invoke-Kubectl -Args @("describe", "deployment", $DeploymentName, "-n", $Namespace) -AllowFailure
	throw "Deployment rollout failed: namespace=$Namespace deployment=$DeploymentName"
}

function Wait-ForZoneReady {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		# 本 zone 实际生成的 scene Deployment 名字(legacy 单池 = @("scene"),
		# 拆分模式 = @("scene-world","scene-instance"))。副本数为 0 的池不等。
		[string[]]$SceneDeploymentNames = @("scene")
	)

	if (-not $WaitReady) {
		return
	}

	Write-Host "Waiting for zone workloads to become ready: namespace=$Namespace"
	Wait-ForDeploymentReady -Namespace $Namespace -DeploymentName "gate"

	if ($SceneOrchestrator -eq "agones") {
		# Fleet 不是 Deployment,`kubectl rollout status` 对它无效(会直接报
		# "no matches for kind")。这里不假装等过,而是把该看的命令打出来。
		# 真正的就绪判据是 Fleet 的 status.readyReplicas。
		foreach ($fleetName in $SceneDeploymentNames) {
			Write-Host "  [agones] scene fleet '$fleetName' readiness is NOT waited on by this script."
			Write-Host "           kubectl -n $Namespace get fleet $fleetName -o jsonpath='{.status.readyReplicas}'"
			Write-Host "           kubectl -n $Namespace get gameservers -l app=$fleetName"
		}
	}
	else {
		foreach ($sceneDeployment in $SceneDeploymentNames) {
			Wait-ForDeploymentReady -Namespace $Namespace -DeploymentName $sceneDeployment
		}
	}

	if (-not $SkipGoSvc -and -not [string]::IsNullOrWhiteSpace($GoSvcRegistry)) {
		foreach ($svcName in $GoSvcCatalogue.Keys) {
			Wait-ForDeploymentReady -Namespace $Namespace -DeploymentName $svcName
		}
	}
	if (-not $SkipJavaSvc -and -not [string]::IsNullOrWhiteSpace($JavaSvcRegistry)) {
		foreach ($svcName in $JavaSvcCatalogue.Keys) {
			Wait-ForDeploymentReady -Namespace $Namespace -DeploymentName $svcName
		}
	}
	Write-Host "Zone workloads are ready: namespace=$Namespace"
}

function New-GoSvcConfigMapYaml {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		[Parameter(Mandatory = $true)][int]$CurrentZoneId
	)

	$info = $GoSvcCatalogue[$SvcName]
	$configMapName = $info.ConfigMap
	$configFileName = $info.ConfigFile
	$dbTaskRetentionMs = $script:KafkaDbTaskRetentionMs

	# 契约关键值一律从服务自己的 etc/*.yaml 取,不在这里再写一份常数。
	$dbPartitionCnt    = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.PartitionCnt'
	$dbTopicGeneration = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.TopicGeneration'
	$dbSubShardCount   = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.SubShardCount'
	$dbMaxOpenConn     = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Database.MaxOpenConn'
	$dbMaxIdleConn     = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Database.MaxIdleConn'

	$loginPartitionCnt      = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Kafka.PartitionCnt'
	$loginInitialPartition  = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Kafka.InitialPartition'
	$loginTopicGeneration   = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Kafka.TopicGeneration'
	$loginSessionExpireMin  = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.SessionExpireMin'
	$loginMaxLoginDevices   = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.MaxLoginDevices'
	$loginNodeLeaseTTL      = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.LeaseTTL'
	$loginQueueShardCount   = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.QueueShardCount'
	$loginAccountLockTTL    = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Locker.AccountLockTTL'
	$loginPlayerLockTTL     = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Locker.PlayerLockTTL'

	$locatorNodeLeaseTTL    = Get-AuthoritativeScalar -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath 'Node.LeaseTTL'
	$locatorLeaseTTLSeconds = Get-AuthoritativeScalar -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath 'Lease.DefaultTTLSeconds'

	$mysqlUser = $script:MysqlUser
	$mysqlPassword = $script:MysqlPassword
	$redisPassword = $script:RedisPassword
	$gateTokenSecret = $script:GateTokenSecret
	$internalAuthSecret = $script:InternalAuthSecret

	# db 库名白名单。三个来源(DB_ALLOWED_DATABASES 环境变量 / AllowedDatabasesFile /
	# 本字段)全空时,db 在默认 strict 档下**拒绝启动**
	# (go/db/internal/config/config.go 的 AllowlistEnforcement)。以前这份 ConfigMap
	# 里没有 AllowedDatabases,Deployment 也没注入环境变量,所以 db 起不来。
	#
	# staging/prod 必须由运维显式注入,**绝不能**让生成器从 $CurrentZoneId 推导:
	# 这份白名单存在的全部意义,就是用一个与 ZoneId 无关的外部事实去校验
	# 「ZoneId 拼出来的库名」——ZoneId 填错一位会在生产实例上静默建库并写入玩家
	# 数据(config.go:44-48)。自己推自己等于零保护,还留下"已经防住了"的错觉。
	# 只有 dev 档才回落到推导值,为的是本地一键起栈。
	$dbAllowedDatabases = Resolve-InjectedSecret -EnvName "MMORPG_DB_ALLOWED_DATABASES" `
		-DevFallback "zone_${CurrentZoneId}_db" -ReleaseProfile $ReleaseProfile `
		-Purpose "db 库名白名单(逗号分隔,如 zone_1_db,zone_2_db)" -MinLength 3
	$dbAllowedList = @($dbAllowedDatabases -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ })
	if ($dbAllowedList.Count -eq 0) {
		throw "生成 db ConfigMap 失败:库名白名单解析后为空(MMORPG_DB_ALLOWED_DATABASES='$dbAllowedDatabases')。"
	}
	$dbAllowedDatabasesYaml = ($dbAllowedList | ForEach-Object { "      - `"$_`"" }) -join "`n"

	$svcConfig = switch ($SvcName) {
		"db" {
@"
Name: db.rpc
ListenOn: 0.0.0.0:6000
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: db.rpc
ZoneId: ${CurrentZoneId}
ServerConfig:
  JsonPath: "/app/data/mysql_database_table_list.json"
  Kafka:
    Brokers:
      - "kafka.${InfraNamespace}:9092"
    GroupID: "db_rpc_consumer_group"
    TopicGeneration: ${dbTopicGeneration}
    PartitionCnt: ${dbPartitionCnt}
    SubShardCount: ${dbSubShardCount}
    IsOfflineExpand: false
  Database:
    Hosts: "mysql.${InfraNamespace}:3306"
    User: "${mysqlUser}"
    Passwd: "${mysqlPassword}"
    MaxOpenConn: ${dbMaxOpenConn}
    MaxIdleConn: ${dbMaxIdleConn}
    Net: ""
    # 库名白名单(启动期硬断言)。刻意不写 AllowlistEnforcement:留空即 strict,
    # 也就是生产语义 —— 白名单一旦解析为空就拒启,不允许静默放行。
    AllowedDatabases:
${dbAllowedDatabasesYaml}
  RedisClient:
    Hosts: "redis.${InfraNamespace}:6379"
    DefaultTTLSeconds: 3600
    Password: "${redisPassword}"
    DB: 0
"@
		}
		"data-service" {
@"
Name: dataservice.rpc
ListenOn: 0.0.0.0:9000
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: dataservice.rpc
MappingRedis:
  Host: redis.${InfraNamespace}:6379
  Type: node
  DB: 15
Regions:
  - Id: 1
    Zones: [1]
    Redis:
      Addrs:
        - redis.${InfraNamespace}:6379
DevRedis:
  Host: redis.${InfraNamespace}:6379
  Type: node
  DB: 0
PlayerLockTTLSec: 3
"@
		}
		"login" {
@"
Name: login.rpc
ListenOn: 0.0.0.0:50000
Timeout: 100000
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: login.rpc
Node:
  ZoneId: ${CurrentZoneId}
  SessionExpireMin: ${loginSessionExpireMin}
  MaxLoginDevices: ${loginMaxLoginDevices}
  LeaseTTL: ${loginNodeLeaseTTL}
  QueueShardCount: ${loginQueueShardCount}
  MaxLoginDuration: 5m
  LogoutGraceTime: 5s
  RedisClient:
    Host: redis.${InfraNamespace}:6379
    Password: "${redisPassword}"
    DB: 0
    DefaultTTL: 24h
    DialTimeout: 3s
    ReadTimeout: 3s
    WriteTimeout: 3s
Snowflake:
  Epoch: 1721473263000
  NodeBits: 13
  StepBits: 9
Locker:
  AccountLockTTL: ${loginAccountLockTTL}
  PlayerLockTTL: ${loginPlayerLockTTL}
Account:
  MaxDevicesPerAccount: 3
  CacheExpire: 12h
Registry:
  Etcd:
    Hosts:
      - "etcd.${InfraNamespace}:2379"
    Key: loginservice.rpc
    DialTimeout: 5s
Timeouts:
  EtcdDialTimeout: 5s
  ServiceDiscoveryTimeout: 10s
  TaskWaitTimeout: 5s
  LoginTotalTimeout: 10s
  RoleCacheExpire: 24h
  TaskManagerCleanInterval: 5s
  TaskBatchExpireTime: 10s
PlayerLocatorRpc:
  Etcd:
    Hosts:
      - "etcd.${InfraNamespace}:2379"
    Key: playerlocator.rpc
  Timeout: 5000
  Middlewares:
    Breaker: false
SceneManagerRpc:
  Etcd:
    Hosts:
      - "etcd.${InfraNamespace}:2379"
    Key: scenemanagerservice.rpc
  Timeout: 5000
  Middlewares:
    Breaker: false
GateTokenSecret: "${gateTokenSecret}"
# Secrets.InternalAuth:内部调用方身份声明(x-session-detail-bin)的验签密钥。
#
# 生产模式下 EnforceInternalAuth 恒为 true(config/secrets.go,配置关不掉),
# 于是 ResolveSecrets 里 internal.validate(..., required=true) 会因为"未配置"
# 直接返回 error,login 拒绝启动。以前这份 ConfigMap 只有上面那个**已废弃**的
# 顶层 GateTokenSecret,没有 Secrets 段,所以生产 login 起不来。
#
# 必须是与 GateToken 不同的一把:secrets.go 里有跨用途复用主密钥的检查,
# 生产复用会直接拒绝启动(一处泄露不该牵连另一处)。
Secrets:
  InternalAuth:
    Value: "${internalAuthSecret}"
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
  GroupID: "db_rpc_consumer_group"
  TopicGeneration: ${loginTopicGeneration}
  PartitionCnt: ${loginPartitionCnt}
  InitialPartition: ${loginInitialPartition}
  DialTimeout: 10s
  ReadTimeout: 30s
  WriteTimeout: 10s
  RetryMax: 3
  RetryBackoff: 100ms
  ChannelBuffer: 1024
  SyncInterval: 30s
  StatsInterval: 5m
  CompressionType: 0
  Idempotent: true
  MaxOpenRequests: 1
  RetentionMs: ${dbTaskRetentionMs}
"@
		}
		"player-locator" {
@"
Name: playerlocator.rpc
ListenOn: 0.0.0.0:50100
Timeout: 10000
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: playerlocator.rpc
RedisClient:
  Host: redis.${InfraNamespace}:6379
  Password: "${redisPassword}"
  DB: 0
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
Node:
  ZoneId: ${CurrentZoneId}
  LeaseTTL: ${locatorNodeLeaseTTL}
Registry:
  Etcd:
    Hosts:
      - "etcd.${InfraNamespace}:2379"
    DialTimeout: 5s
Lease:
  DefaultTTLSeconds: ${locatorLeaseTTLSeconds}
  PollInterval: 1s
  BatchSize: 100
"@
		}
		"scene-manager" {
@"
Name: scenemanagerservice.rpc
ListenOn: 0.0.0.0:60000
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: scenemanagerservice.rpc
Redis:
  Host: redis.${InfraNamespace}:6379
  Type: node
  Key: scenemanagerservice
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
NodeID: "node-1"
# 跨 zone 重定向签发 gate 令牌要用,缺了 gate_redirect.go 直接返回
# "GateTokenSecret not configured"。以前这份 ConfigMap 压根没有这一项。
GateTokenSecret: "${gateTokenSecret}"
"@
		}
		default {
			throw "Unknown Go service: $SvcName"
		}
	}

	return @"
apiVersion: v1
kind: ConfigMap
metadata:
  name: $configMapName
data:
  ${configFileName}: |
$($svcConfig -split "`n" | ForEach-Object { "    $_" } | Out-String)
"@
}

function Apply-GoSvcManifests {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][int]$CurrentZoneId
	)

	if ($SkipGoSvc) { return }
	if ([string]::IsNullOrWhiteSpace($GoSvcRegistry)) {
		Write-Host "[skip] Go services: -GoSvcRegistry not set, skipping Go service deployment."
		return
	}

	Write-Host "Applying Go micro-service manifests to namespace $Namespace (registry=$GoSvcRegistry tag=$GoSvcTag)"

	foreach ($svcName in $GoSvcCatalogue.Keys) {
		$info = $GoSvcCatalogue[$svcName]
		$svcImage = "$GoSvcRegistry/$($info.ImageName):$GoSvcTag"

		# Apply ConfigMap
		$cmYaml = New-GoSvcConfigMapYaml -SvcName $svcName -CurrentZoneId $CurrentZoneId
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $cmYaml

		# Apply manifest with image placeholder replaced
		$manifestPath = Join-Path $GoSvcManifestsDir $info.Manifest
		if (-not (Test-Path $manifestPath)) {
			Write-Warning "Go service manifest not found: $manifestPath – skipping $svcName"
			continue
		}
		$svcPullPolicy = Resolve-ImagePullPolicy -ImageRef $svcImage
		$manifestContent = (Get-Content $manifestPath -Raw) -replace 'PLACEHOLDER_IMAGE', $svcImage
		$manifestContent = $manifestContent -replace 'PLACEHOLDER_PULL_POLICY', $svcPullPolicy
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $manifestContent

		Write-Host "  [applied] $svcName -> $svcImage (port $($info.Port) pullPolicy=$svcPullPolicy)"
	}
}

function New-JavaSvcConfigMapYaml {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName
	)

	$info = $JavaSvcCatalogue[$SvcName]
	$configMapName = $info.ConfigMap

	$gateTokenSecret = $script:GateTokenSecret
	$gatewayDbUser = $script:GatewayDbUser
	$gatewayDbPassword = $script:GatewayDbPassword

	$svcConfig = switch ($SvcName) {
		"auth" {
@"
server:
  port: 5555
spring:
  application:
    name: sa-token-auth
  cloud:
    nacos:
      server-addr: nacos:8848
  data:
    redis:
      host: redis.${InfraNamespace}
sa-token:
  is-read-cookie: false
grpc:
  server:
    port: 5556
"@
		}
		"gateway" {
@"
server:
  port: 8081
spring:
  application:
    name: gateway-node
  datasource:
    url: jdbc:mysql://mysql.${InfraNamespace}:3306/mmorpg?useSSL=false&allowPublicKeyRetrieval=true
    username: "${gatewayDbUser}"
    password: "${gatewayDbPassword}"
  data:
    redis:
      host: redis.${InfraNamespace}
      port: 6379
etcd:
  endpoints: http://etcd.${InfraNamespace}:2379
gate:
  token-secret: "${gateTokenSecret}"
zone:
  probe:
    interval-ms: 5000
"@
		}
		default {
			throw "Unknown Java service: $SvcName"
		}
	}

	return @"
apiVersion: v1
kind: ConfigMap
metadata:
  name: $configMapName
data:
  application.yaml: |
$($svcConfig -split "`n" | ForEach-Object { "    $_" } | Out-String)
"@
}

function Apply-JavaSvcManifests {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace
	)

	if ($SkipJavaSvc) { return }
	if ([string]::IsNullOrWhiteSpace($JavaSvcRegistry)) {
		Write-Host "[skip] Java services: -JavaSvcRegistry not set, skipping Java service deployment."
		return
	}

	Write-Host "Applying Java service manifests to namespace $Namespace (registry=$JavaSvcRegistry tag=$JavaSvcTag)"

	foreach ($svcName in $JavaSvcCatalogue.Keys) {
		$info = $JavaSvcCatalogue[$svcName]
		$svcImage = "$JavaSvcRegistry/$($info.ImageName):$JavaSvcTag"

		# Apply ConfigMap
		$cmYaml = New-JavaSvcConfigMapYaml -SvcName $svcName
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $cmYaml

		# Apply manifest with image placeholder replaced
		$manifestPath = Join-Path $JavaSvcManifestsDir $info.Manifest
		if (-not (Test-Path $manifestPath)) {
			Write-Warning "Java service manifest not found: $manifestPath – skipping $svcName"
			continue
		}
		$svcPullPolicy = Resolve-ImagePullPolicy -ImageRef $svcImage
		$manifestContent = (Get-Content $manifestPath -Raw) -replace 'PLACEHOLDER_IMAGE', $svcImage
		$manifestContent = $manifestContent -replace 'PLACEHOLDER_PULL_POLICY', $svcPullPolicy
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $manifestContent

		Write-Host "  [applied] $svcName -> $svcImage (http=$($info.HttpPort) grpc=$($info.GrpcPort) pullPolicy=$svcPullPolicy)"
	}
}

function Get-ZonesFromJson {
	param([Parameter(Mandatory = $true)][string]$Path)

	if (-not (Test-Path $Path)) {
		throw "zones config not found: $Path. You can copy deploy/k8s/zones.sample.json to this path and edit it."
	}

	$raw = Get-Content -Path $Path -Raw
	$extension = [System.IO.Path]::GetExtension($Path).ToLowerInvariant()
	$parsed = $null

	function Convert-ZonesYamlFallback {
		param([Parameter(Mandatory = $true)][string]$YamlText)

		$zones = @()
		$currentZone = $null

		$lines = $YamlText -split "`r?`n"
		foreach ($line in $lines) {
			$trimmed = $line.Trim()
			if ([string]::IsNullOrWhiteSpace($trimmed)) {
				continue
			}

			if ($trimmed.StartsWith("#")) {
				continue
			}

			if ($trimmed -eq "zones:") {
				continue
			}

			if ($trimmed -match "^-\s*name\s*:\s*(.+)$") {
				if ($null -ne $currentZone) {
					$zones += $currentZone
				}

				$currentZone = [ordered]@{
					name = $matches[1].Trim().Trim('"', "'")
					zoneId = $null
					replicas = [ordered]@{
						centre = $null
						gate = $null
						scene = $null
						scene_world = $null
						scene_instance = $null
					}
				}
				continue
			}

			if ($null -eq $currentZone) {
				continue
			}

			if ($trimmed -match "^zoneId\s*:\s*(\d+)$") {
				$currentZone.zoneId = [int]$matches[1]
				continue
			}

			# 长 key 必须排在 scene 前面,否则 "scene_world: 2" 会先命中 scene 分支。
			if ($trimmed -match "^(scene_world|scene_instance|centre|gate|scene)\s*:\s*(\d+)$") {
				$currentZone.replicas[$matches[1]] = [int]$matches[2]
				continue
			}
		}

		if ($null -ne $currentZone) {
			$zones += $currentZone
		}

		return [pscustomobject]@{ zones = $zones }
	}

	switch ($extension) {
		".json" {
			$parsed = $raw | ConvertFrom-Json
		}
		".yaml" {
			$yamlParser = Get-Command ConvertFrom-Yaml -ErrorAction SilentlyContinue
			if ($null -ne $yamlParser) {
				$parsed = $raw | ConvertFrom-Yaml
			}
			else {
				$parsed = Convert-ZonesYamlFallback -YamlText $raw
			}
		}
		".yml" {
			$yamlParser = Get-Command ConvertFrom-Yaml -ErrorAction SilentlyContinue
			if ($null -ne $yamlParser) {
				$parsed = $raw | ConvertFrom-Yaml
			}
			else {
				$parsed = Convert-ZonesYamlFallback -YamlText $raw
			}
		}
		default {
			throw "Unsupported zones config extension '$extension'. Use .json, .yaml, or .yml. path=$Path"
		}
	}

	if ($null -eq $parsed.zones -or $parsed.zones.Count -eq 0) {
		throw "zones config has no zones: $Path"
	}

	$result = @()
	foreach ($zone in $parsed.zones) {
		if ([string]::IsNullOrWhiteSpace($zone.name)) {
			throw "zone name is required in zones config: $Path"
		}

		if ($null -eq $zone.zoneId) {
			throw "zoneId is required for zone '$($zone.name)' in: $Path"
		}

		$zoneCentre = $CentreReplicas
		$zoneGate = $GateReplicas
		$zoneScene = $SceneReplicas
		# -1 = zones 配置里没写这个键,交给 Resolve-SceneDeploymentPlan 判定模式。
		$zoneSceneWorld = $SceneWorldReplicas
		$zoneSceneInstance = $SceneInstanceReplicas

		if ($null -ne $zone.replicas) {
			if ($null -ne $zone.replicas.centre) { $zoneCentre = [int]$zone.replicas.centre }
			if ($null -ne $zone.replicas.gate) { $zoneGate = [int]$zone.replicas.gate }
			if ($null -ne $zone.replicas.scene) { $zoneScene = [int]$zone.replicas.scene }
			if ($null -ne $zone.replicas.scene_world) { $zoneSceneWorld = [int]$zone.replicas.scene_world }
			if ($null -ne $zone.replicas.scene_instance) { $zoneSceneInstance = [int]$zone.replicas.scene_instance }
		}

		$sceneLegacyExplicit = ($null -ne $zone.replicas -and $null -ne $zone.replicas.scene)

		$result += [pscustomobject]@{
			name = [string]$zone.name
			zoneId = [int]$zone.zoneId
			centre = $zoneCentre
			gate = $zoneGate
			scene = $zoneScene
			scene_world = $zoneSceneWorld
			scene_instance = $zoneSceneInstance
			scene_legacy_explicit = $sceneLegacyExplicit
		}
	}

	return ,$result
}

# eSceneNodeType(proto common/base/config.proto,详见 docs/design/scene-creation-architecture.md
# "Node Role Separation")。这里只用到前两个;跨服角色 2/3 目前没有部署形态。
$SceneNodeTypeMainWorld = 0
$SceneNodeTypeInstance = 1

<#
.SYNOPSIS
决定一个 zone 要生成哪些 scene Deployment。

.DESCRIPTION
兼容规则(唯一权威,文档以此为准):

1. 只要 scene_world / scene_instance 任一被显式指定(>= 0),进入**拆分模式**:
     scene-world     replicas=scene_world     SCENE_NODE_TYPE=0
     scene-instance  replicas=scene_instance  SCENE_NODE_TYPE=1
   未指定的那一侧按 0 副本生成(保留 Deployment 便于后续 kubectl scale,
   同时对应 role-split runbook §3.4 的回滚动作"把 instance 池缩到 0")。
   此时 legacy 的 scene 键被**忽略**,不会再生成名为 scene 的 Deployment。
2. 否则进入 **legacy 单池模式**:生成一个名为 scene 的 Deployment,
   SCENE_NODE_TYPE=0(与 zones.sample.yaml 注释"所有 scene pod 都是 SceneNodeType=0"一致)。

两种模式互斥,永远不会同时产出 scene 和 scene-world/scene-instance。
#>
function Resolve-SceneDeploymentPlan {
	param(
		[Parameter(Mandatory = $true)][int]$LegacySceneReplicas,
		[Parameter(Mandatory = $true)][int]$WorldReplicas,
		[Parameter(Mandatory = $true)][int]$InstanceReplicas,
		[bool]$LegacyExplicit = $false,
		[string]$ZoneLabel = ""
	)

	$splitMode = ($WorldReplicas -ge 0 -or $InstanceReplicas -ge 0)

	if (-not $splitMode) {
		return ,@([pscustomobject]@{
			Name = "scene"
			Replicas = $LegacySceneReplicas
			SceneNodeType = $SceneNodeTypeMainWorld
			Role = "world (legacy single pool)"
			RoleLabel = "world"
		})
	}

	$world = if ($WorldReplicas -ge 0) { $WorldReplicas } else { 0 }
	$instance = if ($InstanceReplicas -ge 0) { $InstanceReplicas } else { 0 }

	if ($LegacyExplicit) {
		Write-Warning "zone ${ZoneLabel}: replicas.scene 与 scene_world/scene_instance 同时存在,拆分模式生效,legacy scene=$LegacySceneReplicas 被忽略。请从 zones 配置里删掉 scene 键。"
	}
	if ($world -le 0) {
		Write-Warning "zone ${ZoneLabel}: scene_world=0 —— 该 zone 没有主世界承载节点。StrictNodeTypeSeparation=true 时主世界场景创建会返回 ErrNoNodeForPurpose。"
	}
	if ($instance -le 0) {
		Write-Warning "zone ${ZoneLabel}: scene_instance=0 —— 该 zone 没有副本承载节点。StrictNodeTypeSeparation=true 时副本/战场创建会返回 ErrNoNodeForPurpose。"
	}

	return ,@(
		[pscustomobject]@{
			Name = "scene-world"
			Replicas = $world
			SceneNodeType = $SceneNodeTypeMainWorld
			Role = "world"
			RoleLabel = "world"
		},
		[pscustomobject]@{
			Name = "scene-instance"
			Replicas = $instance
			SceneNodeType = $SceneNodeTypeInstance
			Role = "instance"
			RoleLabel = "instance"
		}
	)
}

function Apply-Zone {
	param(
		[Parameter(Mandatory = $true)][string]$CurrentZoneName,
		[Parameter(Mandatory = $true)][int]$CurrentZoneId,
		[Parameter(Mandatory = $true)][int]$CurrentCentreReplicas,
		[Parameter(Mandatory = $true)][int]$CurrentGateReplicas,
		[Parameter(Mandatory = $true)][int]$CurrentSceneReplicas,
		[int]$CurrentSceneWorldReplicas = -1,
		[int]$CurrentSceneInstanceReplicas = -1,
		[bool]$CurrentSceneLegacyExplicit = $false
	)

	$namespace = Get-ZoneNamespace -Name $CurrentZoneName
	$scenePlan = Resolve-SceneDeploymentPlan `
		-LegacySceneReplicas $CurrentSceneReplicas `
		-WorldReplicas $CurrentSceneWorldReplicas `
		-InstanceReplicas $CurrentSceneInstanceReplicas `
		-LegacyExplicit $CurrentSceneLegacyExplicit `
		-ZoneLabel $CurrentZoneName

	$sceneSummary = ($scenePlan | ForEach-Object { "$($_.Name)=$($_.Replicas)(SCENE_NODE_TYPE=$($_.SceneNodeType))" }) -join " "

	$sceneKind = if ($SceneOrchestrator -eq "agones") { "agones.dev/v1 Fleet" } else { "apps/v1 Deployment" }

	Write-Host "Applying zone deployment: zone=$CurrentZoneName zone_id=$CurrentZoneId namespace=$namespace"
	Write-Host "Ops profile resolved: profile=$OpsProfile gate_service_type=$GateServiceType centre=$CurrentCentreReplicas gate=$CurrentGateReplicas"
	Write-Host "Scene orchestrator: $SceneOrchestrator -> $sceneKind"
	Write-Host "Scene pools resolved: $sceneSummary"

	Ensure-Namespace -Namespace $namespace

	$configMapName = "node-config"
	$gateServiceName = "gate-entry"
	# Agones 模式:必须先在本 namespace 里建 agones-sdk 的 SA + RoleBinding,
	# 否则 GameServer 控制器建 Pod 会被 API server 拒掉,GameServer 全进 Error。
	# 必须在 Fleet 之前 apply —— 反过来的话第一批 GameServer 会先失败一轮。
	if ($SceneOrchestrator -eq "agones") {
		$rbacYaml = New-AgonesSdkRbacYaml -Namespace $namespace
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $rbacYaml
	}

	$configMapYaml = New-NodeConfigMapYaml -CurrentZoneId $CurrentZoneId -ConfigName $configMapName
	Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $configMapYaml

	$gateYaml = New-NodeDeploymentYaml -NodeName "gate" -Replicas $CurrentGateReplicas -RpcPort 18000 -StartCommand "./gate" -ConfigMapName $configMapName
	Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $gateYaml

	foreach ($scenePool in $scenePlan) {
		if ($SceneOrchestrator -eq "agones") {
			$sceneYaml = New-SceneFleetYaml `
				-FleetName $scenePool.Name `
				-Replicas $scenePool.Replicas `
				-RpcPort 20000 `
				-StartCommand "./scene" `
				-ConfigMapName $configMapName `
				-SceneNodeType $scenePool.SceneNodeType `
				-RoleLabel $scenePool.RoleLabel `
				-ZoneLabel $CurrentZoneName `
				-ZoneIdLabel $CurrentZoneId
		}
		else {
			$sceneYaml = New-NodeDeploymentYaml `
				-NodeName $scenePool.Name `
				-Replicas $scenePool.Replicas `
				-RpcPort 20000 `
				-StartCommand "./scene" `
				-ConfigMapName $configMapName `
				-SceneNodeType $scenePool.SceneNodeType
		}
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $sceneYaml

		if ($SceneOrchestrator -eq "agones" -and $AgonesAutoscale) {
			if (-not $AgonesHighDensity) {
				throw "-AgonesAutoscale requires -AgonesHighDensity: the Counter policy scales on the rooms Counter, which only exists in high-density mode."
			}
			$autoscalerYaml = New-SceneFleetAutoscalerYaml `
				-FleetName $scenePool.Name `
				-RoleLabel $scenePool.RoleLabel `
				-ZoneLabel $CurrentZoneName `
				-ZoneIdLabel $CurrentZoneId
			Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $autoscalerYaml
		}
	}

	$gateServiceYaml = New-GateServiceYaml -ServiceName $gateServiceName
	Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $gateServiceYaml

	Apply-GoSvcManifests -Namespace $namespace -CurrentZoneId $CurrentZoneId

	Apply-JavaSvcManifests -Namespace $namespace

	$sceneDeploymentNames = @($scenePlan | Where-Object { $_.Replicas -gt 0 } | ForEach-Object { $_.Name })
	Wait-ForZoneReady -Namespace $namespace -SceneDeploymentNames $sceneDeploymentNames

	if ($scenePlan.Count -gt 1) {
		Write-Host "NOTE: 从 legacy 单池切到拆分模式时,旧的 'scene' 工作负载不会被 apply 自动删除。确认新池 Ready 后手动执行: kubectl -n $namespace delete deployment scene"
	}
	if ($SceneOrchestrator -eq "agones") {
		# 换编排方式会换 kind:apply 只会新建 Fleet,不会回收同名 Deployment,
		# 两者同时在线 = 同一个 zone 里有两套 scene 进程、两套容量语义。
		Write-Host "NOTE: 切到 Agones 编排后,同名的旧 Deployment 不会被自动删除。确认 Fleet 就绪后手动执行:"
		foreach ($scenePool in $scenePlan) {
			Write-Host "        kubectl -n $namespace delete deployment $($scenePool.Name) --ignore-not-found"
		}
	}

	Write-Host "Zone deployment applied: namespace=$namespace"
}

function Remove-Zone {
	param([Parameter(Mandatory = $true)][string]$CurrentZoneName)

	$namespace = Get-ZoneNamespace -Name $CurrentZoneName
	Write-Host "Deleting zone namespace: $namespace"
	Invoke-Kubectl -Args @("delete", "namespace", $namespace) -AllowFailure
}

function Show-ZoneStatus {
	param([Parameter(Mandatory = $true)][string]$CurrentZoneName)

	$namespace = Get-ZoneNamespace -Name $CurrentZoneName
	Write-Host "Zone status for namespace=$namespace"
	Invoke-Kubectl -Args @("get", "deploy,po,svc,cm", "-n", $namespace) -AllowFailure
}

function Apply-Infra {
	Write-Host "Deploying shared infrastructure to namespace $InfraNamespace"
	Write-Host "Kafka profile: $KafkaProfile (broker_retention_ms=$KafkaBrokerRetentionMs db_task_retention_ms=$KafkaDbTaskRetentionMs)"
	Ensure-Namespace -Namespace $InfraNamespace

	foreach ($manifest in @("etcd.yaml", "redis.yaml", "kafka.yaml", "mysql.yaml")) {
		$path = Join-Path $InfraManifestsDir $manifest
		if (-not (Test-Path $path)) {
			Write-Warning "Infra manifest not found: $path — skipping"
			continue
		}
		if ($manifest -eq "kafka.yaml") {
			$manifestContent = Get-Content -Path $path -Raw
			$manifestContent = $manifestContent.Replace("__KAFKA_LOG_RETENTION_MS__", [string]$KafkaBrokerRetentionMs)
			$manifestContent = $manifestContent.Replace("__KAFKA_LOG_RETENTION_CHECK_INTERVAL_MS__", [string]$KafkaRetentionCheckIntervalMs)
			$manifestContent = $manifestContent.Replace("__KAFKA_LOG_RETENTION_BYTES__", [string]$KafkaRetentionBytes)
			$manifestContent = $manifestContent.Replace("__KAFKA_LOG_SEGMENT_BYTES__", [string]$KafkaSegmentBytes)
			$manifestContent = $manifestContent.Replace("__KAFKA_HEAP_OPTS__", [string]$KafkaHeapOpts)
			Invoke-KubectlWithInputFile -Args @("apply", "-n", $InfraNamespace) -InputContent $manifestContent
		}
		else {
			Invoke-Kubectl -Args @("apply", "-n", $InfraNamespace, "-f", $path)
		}
	}

	Write-Host "Shared infrastructure deployed: namespace=$InfraNamespace"
}

function Remove-Infra {
	Write-Host "Deleting shared infrastructure namespace: $InfraNamespace"
	Invoke-Kubectl -Args @("delete", "namespace", $InfraNamespace) -AllowFailure
}

function Show-InfraStatus {
	Write-Host "Shared infrastructure status for namespace=$InfraNamespace"
	Invoke-Kubectl -Args @("get", "deploy,po,svc,cm", "-n", $InfraNamespace) -AllowFailure
}

function Resolve-ZonesConfigPath {
	if (-not [string]::IsNullOrWhiteSpace($ZonesConfigPath)) {
		return $ZonesConfigPath
	}

	return (Join-Path $K8sRoot "zones.json")
}

Ensure-KubectlAvailable
Apply-OpsProfileDefaults
Apply-KafkaProfileDefaults
Show-ExposureProfileWarning

Write-Host "Release: profile=$ReleaseProfile image=$NodeImage pullPolicy=$ImagePullPolicy go_tag=$GoSvcTag java_tag=$JavaSvcTag"
if ($script:ReleaseStamp.Ok -and $script:ReleaseStamp.Dirty) {
	Write-Warning "工作树是脏的(git status 非空),镜像 tag 带 -dirty 后缀。生产发布(-ReleaseProfile prod)会拒绝这种 tag。"
}

# 写操作才需要门禁;*-down / *-status 是止血和排查路径,不能被预检或密钥缺失挡住。
if ($Command -in @("zone-up", "all-up", "infra-up")) {
	Assert-ImmutableReleaseImages
	Invoke-ReleasePreflight
	Initialize-InjectedSecrets
}

switch ($Command) {
	"infra-up" {
		Apply-Infra
	}
	"infra-down" {
		Remove-Infra
	}
	"infra-status" {
		Show-InfraStatus
	}
	"zone-up" {
		Apply-Zone -CurrentZoneName $ZoneName -CurrentZoneId $ZoneId -CurrentCentreReplicas $CentreReplicas -CurrentGateReplicas $GateReplicas -CurrentSceneReplicas $SceneReplicas -CurrentSceneWorldReplicas $SceneWorldReplicas -CurrentSceneInstanceReplicas $SceneInstanceReplicas
	}
	"zone-down" {
		Remove-Zone -CurrentZoneName $ZoneName
	}
	"zone-status" {
		Show-ZoneStatus -CurrentZoneName $ZoneName
	}
	"all-up" {
		if (-not $SkipInfra) {
			Apply-Infra
		}
		$zonesPath = Resolve-ZonesConfigPath
		$zones = Get-ZonesFromJson -Path $zonesPath
		foreach ($zone in $zones) {
			Apply-Zone -CurrentZoneName $zone.name -CurrentZoneId $zone.zoneId -CurrentCentreReplicas $zone.centre -CurrentGateReplicas $zone.gate -CurrentSceneReplicas $zone.scene -CurrentSceneWorldReplicas $zone.scene_world -CurrentSceneInstanceReplicas $zone.scene_instance -CurrentSceneLegacyExplicit $zone.scene_legacy_explicit
		}
	}
	"all-down" {
		$zonesPath = Resolve-ZonesConfigPath
		$zones = Get-ZonesFromJson -Path $zonesPath
		foreach ($zone in $zones) {
			Remove-Zone -CurrentZoneName $zone.name
		}
		if (-not $SkipInfra) {
			Remove-Infra
		}
	}
	"all-status" {
		Show-InfraStatus
		$zonesPath = Resolve-ZonesConfigPath
		$zones = Get-ZonesFromJson -Path $zonesPath
		foreach ($zone in $zones) {
			Show-ZoneStatus -CurrentZoneName $zone.name
		}
	}
	default {
		throw "Unsupported command: $Command"
	}
}
