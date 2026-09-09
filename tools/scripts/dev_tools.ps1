param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("help", "pbgen-build", "pbgen-run", "proto-gen-build", "proto-gen-run", "tree", "naming-audit", "naming-apply", "third-party-grpc-build", "iwyu-run", "k8s-infra-up", "k8s-infra-down", "k8s-infra-status", "k8s-zone-up", "k8s-zone-down", "k8s-zone-status", "k8s-zone-rollback", "k8s-all-up", "k8s-all-down", "k8s-all-status", "k8s-build-all", "k8s-exposure-preflight", "k8s-stage-runtime", "k8s-image-preflight", "k8s-build-image", "k8s-push-image", "k8s-release-zone", "k8s-release-all", "go-svc-start", "go-svc-start-exe", "go-svc-stop", "go-svc-status", "go-svc-list", "go-svc-build", "go-svc-build-images", "go-svc-push-images", "java-svc-build-image", "java-svc-push-image", "cpp-node-start", "cpp-node-stop", "cpp-node-status", "cpp-node-list", "dev-start", "dev-start-exe", "dev-start-zones", "dev-stop", "dev-status", "dev-robot-zones", "merge-zone", "merge-zone-audit", "merge-zone-unmerge", "kafka-offset-reset", "git-stats")]
    [string]$Command,

    [string]$ConfigPath = "",

    [int]$StatsYear = (Get-Date).Year,
    [int]$StatsMonth = (Get-Date).Month,
    [string]$StatsAuthor = "",

    [switch]$EnablePprof,
    [switch]$UseBinary,
    [switch]$UseGoRun,

    [ValidateSet("snake", "kebab")]
    [string]$Style = "snake",

    [int]$MaxChanges = 0,
    [int]$Jobs = 0,

    [string]$ZoneName = "yesterday",
    [int]$ZoneId = 101,
    [string]$NamespacePrefix = "mmorpg-zone",
    [string]$InfraNamespace = "mmorpg-infra",
    [string]$ZonesConfigPath = "",
    # 留空 = 由 k8s_deploy.ps1 / k8s_image.ps1 用 git 短 sha 组出不可变 tag。
    # 以前这里默认 "…:latest",导致 build 出来的和部署引用的可能是两个不同产物,
    # 而 rollout undo 又退不回去(见 tools/scripts/lib/release_common.ps1 注释)。
    [string]$NodeImage = "",
    [ValidateSet("custom", "managed-cloud", "bare-metal")]
    [string]$OpsProfile = "custom",
    # 发布档位,透传给 k8s_image.ps1 / k8s_deploy.ps1。
    [ValidateSet("dev", "staging", "prod")]
    [string]$ReleaseProfile = "dev",
    [switch]$AllowDirty,
    [string]$ImageRepository = "ghcr.io/luyuancpp/mmorpg-node",
    [string]$ImageTag = "",
    [string]$RuntimeRoot = "deploy/k8s/runtime/linux",
    [string]$DockerfilePath = "deploy/k8s/Dockerfile.runtime",
    [string]$BinarySourceRoot = "",
    [string]$ZoneInfoSource = "bin/zoneinfo",
    [string]$TableSource = "generated/tables",
    [int]$CentreReplicas = 1,
    [int]$GateReplicas = 2,
    # Scene 角色拆分,-1 = 未指定(走 legacy 单池)。见 k8s_deploy.ps1 Resolve-SceneDeploymentPlan。
    [int]$SceneReplicas = 4,
    [int]$SceneWorldReplicas = -1,
    [int]$SceneInstanceReplicas = -1,
    # Scene Node 编排方式:deployment(默认) | agones。见 docs/design/agones-scene-node-high-density.md。
    [ValidateSet("deployment", "agones")]
    [string]$SceneOrchestrator = "deployment",
    # Agones 高密度 / FleetAutoscaler 开关。
    # 这些以前只存在于 k8s_deploy.ps1,dev_tools.ps1 没透传 —— 而 dev_tools.ps1
    # 才是文档里的入口,等于这些功能从文档路径**完全不可达**。
    [switch]$AgonesHighDensity,
    [int]$AgonesRoomCapacity = 0,
    [switch]$AgonesAutoscale,
    [int]$AgonesBufferRooms = 5,
    [int]$AgonesMinReplicas = 1,
    [int]$AgonesMaxReplicas = 0,
    [ValidateSet("ClusterIP", "NodePort", "LoadBalancer")]
    [string]$GateServiceType = "NodePort",
    [int]$GateServicePort = 18000,
    [switch]$SkipInfra,
    [switch]$SkipGoSvc,
    [string]$GoSvcRegistry = "ghcr.io/luyuancpp",
    # 同 NodeImage:留空 = git 短 sha。
    [string]$GoSvcTag = "",
    [switch]$SkipJavaSvc,
    [string]$JavaSvcRegistry = "ghcr.io/luyuancpp",
    [string]$JavaSvcTag = "",
    [bool]$BuildRelease = $true,
    [bool]$BuildDebug = $true,
    # iwyu-run
    [string[]]$NodePath = @("cpp/nodes/scene"),
    [ValidateSet("auto", "iwyu", "clang-tidy")]
    [string]$IwyuTool = "auto",
    [switch]$FixIncludes,
    [switch]$ChangedOnly,
    [string]$DiffBase = "",
    [switch]$Clean,
    [switch]$SkipToolCheck,
    [switch]$DryRun,
    [switch]$WaitReady,
    [int]$WaitTimeoutSeconds = 180,
    [string]$KubeContext = "",
    [string]$KubeConfig = "",
    # go-svc-* commands
    [string[]]$GoServices = @(),
    # Per-service instance counts for local multi-open of Go services. Accepts:
    #   * hashtable (in-session use): -GoCounts @{ login = 2; scene_manager = 2 }
    #   * string list (pwsh -File):   -GoCounts login=2,scene_manager=2
    # Cap = 32 per service. 'db' is single-instance only.
    [object]$GoCounts = @{},
    [int]$GoPortStride = 1,
    # Disable tier-staged Go service startup (parallel launch like before).
    [switch]$NoTier,
    # Per-tier readiness budget (seconds). Soft TCP LISTEN probe per instance.
    [int]$TierReadySeconds = 10,
    # cpp-node-* / dev-* commands
    [string[]]$CppNodes = @(),
    [int]$GateCount = 1,
    [int]$SceneCount = 1,
    # 回合制战斗节点实例数。battle 是全局池(不分 zone),
    # dev-start-zones 只在第一个 zone 起 $BattleCount 份,其余 zone 传 0。
    [int]$BattleCount = 1,
    # Address the C++ nodes advertise into etcd (forwarded to cpp_nodes.ps1 as
    # -NodeIp -> NODE_IP). Empty = that script's default, which detects this
    # machine's physical-NIC IPv4 and needs no configuration. Also accepts an
    # explicit address, 'loopback', or 'engine'.
    [string]$NodeIp = "",
    [switch]$UseVSGenerator,

    # Local multi-zone stress launch (dev-start-zones).
    # Accepts comma-separated ints, e.g. -Zones "1,2" or -Zones 1,2.
    # For dev-start-zones, defaults to 1,2 when empty. Forwarded to
    # go_services.ps1 / cpp_nodes.ps1 as -Zone <N> for each zone.
    [int[]]$Zones = @(),
    # Single-zone override forwarded to underlying scripts when callers want
    # to launch one specific zone (e.g. dev_tools.ps1 ... -Command go-svc-start -Zone 2).
    [int]$Zone = 0,
    # Port offset between zones (forwarded to go_services.ps1).
    [int]$ZonePortShift = 1000,
    # ── merge-zone / merge-zone-audit ─────────────────────────────
    # 合服跨了四个 Redis DB。DB 号猜错不会报错,只会**静默无效**。所以每个 DB
    # 都是独立参数,默认值取自各服务 yaml。
    #   mapping DB 0   player:zone / lock:player / merge:in_progress
    #                  **一定是 0**:data_service 用 go-zero 的 MustNewRedis,
    #                  而 go-zero v1.10.0 的 RedisConf 没有 DB 字段,yaml 里写
    #                  `DB: 15` 会被静默忽略(那行 inert 的键已从 yaml 删除)。
    #                  传 -MergeMappingRedisDB 15 = fence 与 remap 一起指向
    #                  data_service 从不碰的库 = 审计恒绿、合服报成功却零改动。
    #   guild   DB 2   guild_rank:zone / guild:v2 / guild_rank:maintenance_lock
    #   friend  DB 3   friend:online
    #   login   DB 0   player_merge_notice / player:session / kafka:retry|dead
    [int]$MergeSourceZone = 0,
    [int]$MergeTargetZone = 0,
    [string]$MergeMySqlDsn = "root:@tcp(127.0.0.1:3306)/mmorpg?charset=utf8mb4&parseTime=true&loc=Local",
    [string]$MergeRedisAddr = "127.0.0.1:6379",
    [string]$MergeRedisPassword = "",
    [int]$MergeRedisDB = 2,
    # -1 = 从 go/data_service/etc/data_service.yaml 的 MappingRedis 现读(见 Get-MergeMappingRedis)。
    [string]$MergeMappingRedisAddr = "",
    [int]$MergeMappingRedisDB = -1,
    [string]$MergeMappingRedisPassword = "",
    [string]$MergeNoticeRedisAddr = "",
    [int]$MergeNoticeRedisDB = 0,
    [string]$MergeFriendRedisAddr = "",
    [int]$MergeFriendRedisDB = 3,
    [string]$MergeSceneRedisAddr = "",
    [int]$MergeSceneRedisDB = 0,
    # 多 Redis 集群部署才需要的 player:{id}:* blob 拷贝。
    [switch]$MergeMigratePlayerBlobs,
    [string]$MergeSourceDataRedis = "",
    [string]$MergeTargetDataRedis = "",
    [string]$MergeDataRedisPassword = "",
    [int]$MergeSourceDataRedisDB = 0,
    [int]$MergeTargetDataRedisDB = 0,
    # 合服后清掉源区在 scene_manager Redis 里的热状态(location / scene / 频道 / 节点)。
    [switch]$MergeClearSourceHotState,
    # 存量玩家 player:zone 回填。**第一次合服之前每个 zone 都要跑一遍。**
    [int]$MergeBackfillZone = 0,
    # 清单 / 复核 / 撤销。
    [string]$MergeManifestPath = "",
    [int]$MergeExpectedSrcPlayers = -1,
    [switch]$MergeAllowEmptySource,
    [string]$MergeTableListJson = "",
    # Kafka 积压门禁:有 CLI 就真查,没有就必须显式声明(不允许静默跳过)。
    # GroupID / TopicGeneration 必须与 go/db/etc/db.yaml 一致 —— 查错 topic
    # 等于「查了一个不存在的 topic,lag 恒 0」,门禁形同虚设。默认取 db.yaml 的值。
    [string]$MergeKafkaConsumerGroupsCmd = "",
    [string]$MergeKafkaBootstrap = "127.0.0.1:9092",
    [string]$MergeKafkaGroup = "",
    [int]$MergeKafkaTopicGeneration = -1,
    [switch]$MergeAssumeKafkaDrained,
    # merge-zone-audit only — switches to post-merge verification mode that
    # asserts source-zone state has been emptied. See merge-zone-runbook.md §5.
    [switch]$VerifyMerged,

    # ── kafka-offset-reset ────────────────────────────────────────
    # See tools/scripts/kafka_offset_reset.ps1 for full docs.
    # Used by zone rollback (docs/design/zone_data_rollback.md §3 step 4).
    [string]$KafkaBootstrapServer = "kafka:9092",
    [string]$KafkaGroup = "",
    [string]$KafkaTopic = "",
    [string]$KafkaToDatetime = "",      # ISO 8601 UTC, e.g. "2026-05-15T03:17:00Z"
    [switch]$KafkaToEarliest,
    [switch]$KafkaToLatest,
    [switch]$KafkaDeleteAndRecreate,
    [int]$KafkaPartitions = 0,
    [int]$KafkaReplicationFactor = 1,
    [string]$KafkaBin = "",
    [switch]$KafkaApply,

    # ── k8s-zone-rollback ─────────────────────────────────────────
    # See tools/scripts/k8s_zone_rollback.ps1 for full docs.
    # Wraps docs/design/zone_data_rollback.md §3 5-step disaster recovery.
    [string]$RollbackTargetTime = "",       # ISO 8601 UTC
    [string]$RollbackRedisHost = "",
    [string]$RollbackRedisPort = "6379",
    [string]$RollbackRedisPassword = "",
    [int]$RollbackRedisDB = 0,
    # 留空 = 由 k8s_zone_rollback.ps1 按 -ZoneId 推导 db_task_zone_<id>。
    # 旧默认 "db_task_topic" 全仓不存在,会静默重置一个空 topic。
    [string]$RollbackKafkaTopic = "",
    [string]$RollbackKafkaGroup = "db_rpc_consumer_group",
    [switch]$RollbackSkipMySqlPause,
    [int]$RollbackKafkaDrainTimeoutSec = 300,
    [switch]$RollbackApply
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")
$ProtoGenDir = Join-Path $RepoRoot "tools\proto_generator\protogen"
$ProtoGenEnablePprofEnvVar = "PROTOGEN_ENABLE_PPROF"
$LegacyProtoGenEnablePprofEnvVar = "PBGEN_ENABLE_PPROF"

. (Join-Path $ScriptDir "lib\release_common.ps1")

# ─────────────────────────────────────────────────────────────────
# 镜像版本戳:留空的镜像参数一律回落到 git 短 sha,不再默认 latest
# ─────────────────────────────────────────────────────────────────
#
# 这样 `k8s-build-all` 打出来的 tag 和 `k8s-zone-up` 引用的 tag 在同一份
# 工作树状态下必然相同;版本之间必然不同,rollout undo 才真的换 digest。
$script:ReleaseStamp = Get-GitReleaseStamp -RepoRoot $RepoRoot

function Resolve-DevImageTag {
    param([Parameter(Mandatory = $true)][string]$Purpose)

    if (-not $script:ReleaseStamp.Ok) {
        throw "无法生成不可变镜像 tag($Purpose):$($script:ReleaseStamp.Reason)。请显式传入 tag。"
    }
    if ($script:ReleaseStamp.Dirty -and $ReleaseProfile -ne 'dev' -and -not $AllowDirty) {
        throw "工作树是脏的,ReleaseProfile=$ReleaseProfile 下拒绝自动生成 tag($Purpose)。提交/清理工作树,或显式加 -AllowDirty / -ImageTag。"
    }
    return $script:ReleaseStamp.Tag
}

# 调用方有没有显式指定镜像 —— 灾难回滚必须显式指定,不能拿当前工作树的 sha 顶上
$script:NodeImageExplicit = $PSBoundParameters.ContainsKey('NodeImage') -and (-not [string]::IsNullOrWhiteSpace($NodeImage))

if ([string]::IsNullOrWhiteSpace($ImageTag)) { $ImageTag = Resolve-DevImageTag -Purpose "ImageTag" }
if ([string]::IsNullOrWhiteSpace($GoSvcTag)) { $GoSvcTag = Resolve-DevImageTag -Purpose "GoSvcTag" }
if ([string]::IsNullOrWhiteSpace($JavaSvcTag)) { $JavaSvcTag = Resolve-DevImageTag -Purpose "JavaSvcTag" }
if ([string]::IsNullOrWhiteSpace($NodeImage)) { $NodeImage = "{0}:{1}" -f $ImageRepository, $ImageTag }

function Invoke-ProtoGenBuild {
    Push-Location $ProtoGenDir
    try {
        $legacyProtoGenExePath = Join-Path $ProtoGenDir "pbgen.exe"
        $primaryProtoGenExePath = Join-Path $ProtoGenDir "proto-gen.exe"
        $staleCmdExePath = Join-Path $ProtoGenDir "cmd.exe"

        go build -v -o $legacyProtoGenExePath ./cmd
        Copy-Item -Path $legacyProtoGenExePath -Destination $primaryProtoGenExePath -Force

        if (Test-Path $staleCmdExePath) {
            Remove-Item $staleCmdExePath -Force -ErrorAction SilentlyContinue
        }
    }
    finally {
        Pop-Location
    }
}

function Invoke-ProtoGenRun {
    Push-Location $ProtoGenDir
    $previousConfigPath = [Environment]::GetEnvironmentVariable("PROTO_GEN_CONFIG_PATH", "Process")
    $previousEnablePprof = [Environment]::GetEnvironmentVariable($ProtoGenEnablePprofEnvVar, "Process")
    $previousLegacyEnablePprof = [Environment]::GetEnvironmentVariable($LegacyProtoGenEnablePprofEnvVar, "Process")
    try {
        if ($UseBinary -and $UseGoRun) {
            throw "UseBinary and UseGoRun cannot be enabled at the same time."
        }

        if ([string]::IsNullOrWhiteSpace($ConfigPath)) {
            [Environment]::SetEnvironmentVariable("PROTO_GEN_CONFIG_PATH", (Join-Path $ProtoGenDir "etc\proto_gen.yaml"), "Process")
        }
        else {
            $resolvedConfigPath = $ConfigPath
            if (-not [System.IO.Path]::IsPathRooted($resolvedConfigPath)) {
                $resolvedConfigPath = Join-Path $RepoRoot $resolvedConfigPath
            }

            $resolvedConfigPath = (Resolve-Path $resolvedConfigPath).Path
            [Environment]::SetEnvironmentVariable("PROTO_GEN_CONFIG_PATH", $resolvedConfigPath, "Process")
        }

        if ($EnablePprof) {
            [Environment]::SetEnvironmentVariable($ProtoGenEnablePprofEnvVar, "1", "Process")
        }
        else {
            [Environment]::SetEnvironmentVariable($ProtoGenEnablePprofEnvVar, $null, "Process")
            [Environment]::SetEnvironmentVariable($LegacyProtoGenEnablePprofEnvVar, $null, "Process")
        }

        $primaryProtoGenExePath = Join-Path $ProtoGenDir "proto-gen.exe"
        $legacyProtoGenExePath = Join-Path $ProtoGenDir "pbgen.exe"

        if ($UseGoRun) {
            go run ./cmd
            return
        }

        if ($UseBinary) {
            if (Test-Path $primaryProtoGenExePath) {
                & $primaryProtoGenExePath
                return
            }

            if (Test-Path $legacyProtoGenExePath) {
                & $legacyProtoGenExePath
                return
            }

            throw "proto-gen binary not found. Expected $primaryProtoGenExePath or $legacyProtoGenExePath. Please run proto-gen-build first."
        }

        # Default strategy: prefer prebuilt proto-gen binary for faster startup, fall back to the legacy name, then go run.
        if (Test-Path $primaryProtoGenExePath) {
            & $primaryProtoGenExePath
        }
        elseif (Test-Path $legacyProtoGenExePath) {
            & $legacyProtoGenExePath
        }
        else {
            go run ./cmd
        }
    }
    finally {
        if ($null -ne $previousConfigPath) {
            [Environment]::SetEnvironmentVariable("PROTO_GEN_CONFIG_PATH", $previousConfigPath, "Process")
        }
        else {
            [Environment]::SetEnvironmentVariable("PROTO_GEN_CONFIG_PATH", $null, "Process")
        }

        if ($null -ne $previousEnablePprof) {
            [Environment]::SetEnvironmentVariable($ProtoGenEnablePprofEnvVar, $previousEnablePprof, "Process")
        }
        else {
            [Environment]::SetEnvironmentVariable($ProtoGenEnablePprofEnvVar, $null, "Process")
        }

        if ($null -ne $previousLegacyEnablePprof) {
            [Environment]::SetEnvironmentVariable($LegacyProtoGenEnablePprofEnvVar, $previousLegacyEnablePprof, "Process")
        }
        else {
            [Environment]::SetEnvironmentVariable($LegacyProtoGenEnablePprofEnvVar, $null, "Process")
        }

        Pop-Location
    }
}

function Invoke-Tree {
    $TreeScript = Join-Path $ScriptDir "tree.ps1"
    if (-not (Test-Path $TreeScript)) {
        throw "tree.ps1 not found: $TreeScript"
    }

    & $TreeScript
}

function Invoke-NamingAudit {
    $scriptPath = Join-Path $ScriptDir "normalize_names.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "normalize_names.ps1 not found: $scriptPath"
    }

    & $scriptPath -Mode audit -Style $Style -MaxChanges $MaxChanges
}

function Invoke-NamingApply {
    $scriptPath = Join-Path $ScriptDir "normalize_names.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "normalize_names.ps1 not found: $scriptPath"
    }

    & $scriptPath -Mode apply -Style $Style -MaxChanges $MaxChanges -Confirm:$false
}

function Invoke-ThirdPartyGrpcBuild {
    $scriptPath = Join-Path $ScriptDir "third_party\build_grpc.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "third_party/build_grpc.ps1 not found: $scriptPath"
    }

    $bound = $script:PSBoundParameters
    $args = @{}
    if ($bound.ContainsKey('BuildRelease')) {
        $args.BuildRelease = $BuildRelease
    }
    if ($bound.ContainsKey('BuildDebug')) {
        $args.BuildDebug = $BuildDebug
    }
    if ($bound.ContainsKey('Jobs')) {
        $args.Jobs = $Jobs
    }
    if ($Clean) {
        $args.Clean = $true
    }
    if ($SkipToolCheck) {
        $args.SkipToolCheck = $true
    }
    if ($UseVSGenerator) {
        $args.UseVSGenerator = $true
    }

    & $scriptPath @args
}

function Invoke-IwyuRun {
    $scriptPath = Join-Path $ScriptDir "iwyu_run.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "iwyu_run.ps1 not found: $scriptPath"
    }

    $args = @{
        NodePath = $NodePath
        Tool     = $IwyuTool
    }
    if ($FixIncludes) { $args.Fix = $true }
    if ($ChangedOnly) { $args.ChangedOnly = $true }
    if (-not [string]::IsNullOrWhiteSpace($DiffBase)) { $args.DiffBase = $DiffBase }

    & $scriptPath @args
}

function Invoke-K8sDeploy {
    param(
        [Parameter(Mandatory = $true)]
        [string]$K8sCommand
    )

    $scriptPath = Join-Path $ScriptDir "k8s_deploy.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "k8s_deploy.ps1 not found: $scriptPath"
    }

    $args = @{
        Command = $K8sCommand
        ZoneName = $ZoneName
        ZoneId = $ZoneId
        NamespacePrefix = $NamespacePrefix
        InfraNamespace = $InfraNamespace
        NodeImage = $NodeImage
        ReleaseProfile = $ReleaseProfile
        OpsProfile = $OpsProfile
        CentreReplicas = $CentreReplicas
        GateReplicas = $GateReplicas
        SceneReplicas = $SceneReplicas
        SceneWorldReplicas = $SceneWorldReplicas
        SceneInstanceReplicas = $SceneInstanceReplicas
        SceneOrchestrator = $SceneOrchestrator
        AgonesRoomCapacity = $AgonesRoomCapacity
        AgonesBufferRooms = $AgonesBufferRooms
        AgonesMinReplicas = $AgonesMinReplicas
        AgonesMaxReplicas = $AgonesMaxReplicas
        GateServiceType = $GateServiceType
        GateServicePort = $GateServicePort
        WaitTimeoutSeconds = $WaitTimeoutSeconds
    }

    if (-not [string]::IsNullOrWhiteSpace($ZonesConfigPath)) {
        $args.ZonesConfigPath = $ZonesConfigPath
    }

    if (-not [string]::IsNullOrWhiteSpace($KubeContext)) {
        $args.KubeContext = $KubeContext
    }

    if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) {
        $args.KubeConfig = $KubeConfig
    }

    if ($SkipInfra) {
        $args.SkipInfra = $true
    }

    if ($SkipGoSvc) {
        $args.SkipGoSvc = $true
    }

    if (-not [string]::IsNullOrWhiteSpace($GoSvcRegistry)) {
        $args.GoSvcRegistry = $GoSvcRegistry
        $args.GoSvcTag = $GoSvcTag
    }

    if ($SkipJavaSvc) {
        $args.SkipJavaSvc = $true
    }

    if (-not [string]::IsNullOrWhiteSpace($JavaSvcRegistry)) {
        $args.JavaSvcRegistry = $JavaSvcRegistry
        $args.JavaSvcTag = $JavaSvcTag
    }

    # switch 只在被指定时传下去。无条件传 $false 会让 k8s_deploy.ps1 里
    # "开了高密度却没给容量就报错" 那些判定失去意义。
    if ($AgonesHighDensity) {
        $args.AgonesHighDensity = $true
    }

    if ($AgonesAutoscale) {
        $args.AgonesAutoscale = $true
    }

    if ($DryRun) {
        $args.DryRun = $true
    }

    if ($WaitReady) {
        $args.WaitReady = $true
    }

    & $scriptPath @args
}

function Invoke-K8sBuildAll {
    Write-Host "=== Building C++ node image ===" -ForegroundColor Cyan
    $cppDockerfile = Join-Path $RepoRoot "deploy\k8s\Dockerfile.cpp"
    if ($DryRun) {
        Write-Host "[dry-run] docker build -f $cppDockerfile -t $NodeImage $RepoRoot"
    } else {
        & docker build -f $cppDockerfile -t $NodeImage $RepoRoot
        if ($LASTEXITCODE -ne 0) { throw "C++ node image build failed" }
    }

    Write-Host "`n=== Building Go service images ===" -ForegroundColor Cyan
    & (Join-Path $ScriptDir "go_svc_image.ps1") -Command build-all -Registry $GoSvcRegistry -Tag $GoSvcTag -DryRun:$DryRun

    Write-Host "`n=== Building Java service image ===" -ForegroundColor Cyan
    & (Join-Path $ScriptDir "java_svc_image.ps1") -Command build -Registry $JavaSvcRegistry -Tag $JavaSvcTag -DryRun:$DryRun

    Write-Host "`nAll images built successfully." -ForegroundColor Green
}

function Invoke-K8sExposurePreflight {
    $scriptPath = Join-Path $ScriptDir "k8s_deploy.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "k8s_deploy.ps1 not found: $scriptPath"
    }

    function Invoke-PreflightCase {
        param(
            [Parameter(Mandatory = $true)][string]$Title,
            [Parameter(Mandatory = $true)][string]$CaseZoneName,
            [Parameter(Mandatory = $true)][int]$CaseZoneId,
            [Parameter(Mandatory = $true)][string]$CaseOpsProfile,
            [Parameter(Mandatory = $true)][string]$ExpectedServiceType,
            [bool]$ExpectWarning = $false,
            [string]$CaseGateServiceType = ""
        )

        $caseArgs = @{
            Command = "zone-up"
            ZoneName = $CaseZoneName
            ZoneId = $CaseZoneId
            NamespacePrefix = $NamespacePrefix
            InfraNamespace = $InfraNamespace
            NodeImage = $NodeImage
            OpsProfile = $CaseOpsProfile
            CentreReplicas = $CentreReplicas
            GateReplicas = $GateReplicas
            SceneReplicas = $SceneReplicas
            GateServicePort = $GateServicePort
            WaitTimeoutSeconds = $WaitTimeoutSeconds
            DryRun = $true
        }

        if (-not [string]::IsNullOrWhiteSpace($CaseGateServiceType)) {
            $caseArgs.GateServiceType = $CaseGateServiceType
        }
        if (-not [string]::IsNullOrWhiteSpace($KubeContext)) {
            $caseArgs.KubeContext = $KubeContext
        }
        if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) {
            $caseArgs.KubeConfig = $KubeConfig
        }

        Write-Host "--- $Title ---" -ForegroundColor Cyan
        $output = & $scriptPath @caseArgs 2>&1
        $text = ($output | Out-String)
        $output | ForEach-Object { Write-Host $_ }

        $expectedMarker = "Ops profile resolved: profile=$CaseOpsProfile gate_service_type=$ExpectedServiceType"
        if ($text -notmatch [regex]::Escape($expectedMarker)) {
            throw "Preflight case '$Title' failed: expected marker not found -> $expectedMarker"
        }

        $warningMarker = "Using OpsProfile=custom with GateServiceType=LoadBalancer"
        if ($ExpectWarning) {
            if ($text -notmatch [regex]::Escape($warningMarker)) {
                throw "Preflight case '$Title' failed: expected warning not found."
            }
        }
        else {
            if ($text -match [regex]::Escape($warningMarker)) {
                throw "Preflight case '$Title' failed: unexpected warning found."
            }
        }

        Write-Host "PASS: $Title" -ForegroundColor Green
    }

    Invoke-PreflightCase -Title "CASE1 custom default" -CaseZoneName "preflight-a" -CaseZoneId 201 -CaseOpsProfile "custom" -ExpectedServiceType "NodePort"
    Invoke-PreflightCase -Title "CASE2 managed-cloud" -CaseZoneName "preflight-b" -CaseZoneId 202 -CaseOpsProfile "managed-cloud" -ExpectedServiceType "LoadBalancer"
    Invoke-PreflightCase -Title "CASE3 custom + LoadBalancer" -CaseZoneName "preflight-c" -CaseZoneId 203 -CaseOpsProfile "custom" -ExpectedServiceType "LoadBalancer" -CaseGateServiceType "LoadBalancer" -ExpectWarning $true

    Write-Host "k8s exposure preflight passed." -ForegroundColor Green
}

function Invoke-K8sImage {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ImageCommand
    )

    $scriptPath = Join-Path $ScriptDir "k8s_image.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "k8s_image.ps1 not found: $scriptPath"
    }

    $args = @{
        Command = $ImageCommand
        RuntimeRoot = $RuntimeRoot
        DockerfilePath = $DockerfilePath
        ImageRepository = $ImageRepository
        ImageTag = $ImageTag
        ReleaseProfile = $ReleaseProfile
        ZoneName = $ZoneName
        ZoneId = $ZoneId
        NamespacePrefix = $NamespacePrefix
        OpsProfile = $OpsProfile
        CentreReplicas = $CentreReplicas
        GateReplicas = $GateReplicas
        SceneReplicas = $SceneReplicas
        SceneWorldReplicas = $SceneWorldReplicas
        SceneInstanceReplicas = $SceneInstanceReplicas
        SceneOrchestrator = $SceneOrchestrator
        AgonesRoomCapacity = $AgonesRoomCapacity
        AgonesBufferRooms = $AgonesBufferRooms
        AgonesMinReplicas = $AgonesMinReplicas
        AgonesMaxReplicas = $AgonesMaxReplicas
        GateServiceType = $GateServiceType
        GateServicePort = $GateServicePort
        WaitTimeoutSeconds = $WaitTimeoutSeconds
    }

    if (-not [string]::IsNullOrWhiteSpace($ZonesConfigPath)) {
        $args.ZonesConfigPath = $ZonesConfigPath
    }
    if (-not [string]::IsNullOrWhiteSpace($KubeContext)) {
        $args.KubeContext = $KubeContext
    }
    if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) {
        $args.KubeConfig = $KubeConfig
    }
    if ($SkipInfra) {
        $args.SkipInfra = $true
    }
    if ($WaitReady) {
        $args.WaitReady = $true
    }
    if ($DryRun) {
        $args.DryRun = $true
    }

    & $scriptPath @args
}

function Invoke-K8sStageRuntime {
    $scriptPath = Join-Path $ScriptDir "k8s_stage_runtime.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "k8s_stage_runtime.ps1 not found: $scriptPath"
    }

    if ([string]::IsNullOrWhiteSpace($BinarySourceRoot)) {
        throw "-BinarySourceRoot is required for k8s-stage-runtime"
    }

    & $scriptPath `
        -BinarySourceRoot $BinarySourceRoot `
        -RuntimeRoot $RuntimeRoot `
        -ZoneInfoSource $ZoneInfoSource `
        -TableSource $TableSource
}

function Invoke-GitStats {
    $scriptPath = Join-Path $ScriptDir "git_stats.ps1"
    if (-not (Test-Path $scriptPath)) {
        throw "git_stats.ps1 not found: $scriptPath"
    }

    $statsArgs = @{
        Year  = $StatsYear
        Month = $StatsMonth
    }
    if (-not [string]::IsNullOrWhiteSpace($StatsAuthor)) {
        $statsArgs.Author = $StatsAuthor
    }

    & $scriptPath @statsArgs
}

# Get-MergeMappingRedis 从 go/data_service/etc/data_service.yaml 的 MappingRedis
# 段现读 Host / DB / Password,作为 -MergeMappingRedis* 的默认值。
#
# 为什么现读而不是写死:mapping Redis 的 DB 号(今天是 15)是合服里最容易
# 出错、且出错**完全静默**的一个参数 —— 扫错库 = 一个玩家都匹配不到 =
# 「合服成功,0 人被迁移」。写死会在 yaml 改动后悄悄失配;现读至少与部署同源。
# 解析用最小行扫描(不引 powershell-yaml 模块,那是外部依赖):
# 找到 `MappingRedis:` 段,读它缩进块里的 Host / DB / Password。
function Get-MergeMappingRedis {
    $result = @{ Host = ""; DB = -1; Password = "" }
    $yaml = Join-Path $ScriptDir "..\..\go\data_service\etc\data_service.yaml"
    if (-not (Test-Path $yaml)) { return $result }
    $inBlock = $false
    foreach ($line in (Get-Content -LiteralPath $yaml)) {
        if ($line -match '^\s*#') { continue }
        if ($line -match '^MappingRedis:\s*$') { $inBlock = $true; continue }
        if ($inBlock) {
            # 顶格的下一个键 = MappingRedis 段结束。
            if ($line -match '^\S') { break }
            if ($line -match '^\s+Host:\s*(\S+)\s*$')     { $result.Host = $matches[1].Trim('"').Trim("'") }
            if ($line -match '^\s+DB:\s*(\d+)\s*$')       { $result.DB = [int]$matches[1] }
            if ($line -match '^\s+Password:\s*(.*)$')     { $result.Password = $matches[1].Trim().Trim('"').Trim("'") }
        }
    }
    return $result
}

# Get-MergeZoneArgs 组装 merge_zone 的公共 flag。merge-zone 与 merge-zone-audit
# **必须**用同一份,否则审计查的库和合服写的库会不一致 —— 那正是旧版的状态
# (两个入口都只转发 -redis-addr/-redis-db,mapping / friend / login 三个库
# 全走默认值,审计因此恒绿而合服恒空转)。
#
# 另一件它统一处理的事:**路径**。tools/merge_zone 是独立 go module,所以
# `go run` 必须在 tools/merge_zone 里跑(在仓库根跑会得到
# "cannot find main module" —— 旧的 `go run ./tools/merge_zone` 从来没跑通过)。
# 一旦切了工作目录,所有相对路径参数都必须先转成绝对路径。
# Get-MergeDbKafka 现读 go/db/etc/db.yaml 的 Kafka 段,给合服的积压门禁用。
# 为什么必须现读:门禁查的是 db_task_zone_<src>[_g<gen>] 的 consumer lag。GroupID
# 或 TopicGeneration 与 go/db 实际用的不一致时,查的是一个**不存在的 topic** ——
# lag 恒 0,门禁静默放行,于是在 go/db 还没消费完的时候就开始搬数据。
# 解析不到就返回 GroupID="" / TopicGeneration=-1,由调用方决定是回落到工具默认值
# 还是不转发(这里选择不转发,让工具用它自己文档化的默认值)。
function Get-MergeDbKafka {
    $result = @{ GroupID = ""; TopicGeneration = -1 }
    $yaml = Join-Path $ScriptDir "..\..\go\db\etc\db.yaml"
    if (-not (Test-Path $yaml)) { return $result }
    foreach ($line in (Get-Content -LiteralPath $yaml)) {
        if ($line -match '^\s*#') { continue }
        if ($line -match '^\s+GroupID:\s*(\S+)') { $result.GroupID = $matches[1].Trim('"').Trim("'") }
        if ($line -match '^\s+TopicGeneration:\s*(\d+)') { $result.TopicGeneration = [int]$matches[1] }
    }
    return $result
}

function Get-MergeZoneArgs {
    $repoRoot = (Resolve-Path (Join-Path $ScriptDir "..\..")).Path
    $mapping = Get-MergeMappingRedis
    $mapAddr = if ($MergeMappingRedisAddr -ne "") { $MergeMappingRedisAddr }
               elseif ($mapping.Host -ne "")      { $mapping.Host }
               else                               { $MergeRedisAddr }
    # 兜底是 **0**,不是 15:data_service 用 go-zero 的 redis.MustNewRedis(MappingRedis),
    # 而 go-zero 的 RedisConf(v1.10.0)没有 DB 字段 —— yaml 里就算写了 `DB: 15` 也会被
    # 静默忽略,映射永远落在 DB 0。data_service.yaml 里那行 inert 的 DB 已经删除,所以
    # 上面的解析现在恒为 -1。兜底若写 15,fence 与 remap 会一起指向 data_service 从不碰
    # 的库:审计恒绿、合服报告成功却一个 key 都没改。
    $mapDB = if ($MergeMappingRedisDB -ge 0) { $MergeMappingRedisDB }
             elseif ($mapping.DB -ge 0)      { $mapping.DB }
             else                            { 0 }
    $mapPwd = if ($MergeMappingRedisPassword -ne "") { $MergeMappingRedisPassword } else { $mapping.Password }

    $a = @(
        "-mysql-dsn", $MergeMySqlDsn,
        "-redis-addr", $MergeRedisAddr,
        "-redis-db", $MergeRedisDB,
        "-mapping-redis-addr", $mapAddr,
        "-mapping-redis-db", $mapDB,
        "-notice-redis-db", $MergeNoticeRedisDB,
        "-friend-redis-db", $MergeFriendRedisDB,
        "-scene-redis-db", $MergeSceneRedisDB
    )

    # Kafka 积压门禁的三个参数必须一起转发。漏转 -kafka-group / -kafka-topic-generation
    # 时工具会去查 db_task_zone_<src> 与默认 group,在 TopicGeneration != 1 或改过
    # GroupID 的环境上那是一个**不存在的 topic**:lag 查出来恒 0,门禁静默放行。
    $kGroup = if ($MergeKafkaGroup -ne "") { $MergeKafkaGroup } else { (Get-MergeDbKafka).GroupID }
    $kGen = if ($MergeKafkaTopicGeneration -ge 0) { $MergeKafkaTopicGeneration } else { (Get-MergeDbKafka).TopicGeneration }
    # 只在这里转发 group / generation:CLI 路径与 -assume-kafka-drained 由
    # merge-zone 命令块单独追加(审计不跑积压门禁,给了也无害但没必要)。
    if ($kGroup -ne "") { $a += @("-kafka-group", $kGroup) }
    if ($kGen -ge 0) { $a += @("-kafka-topic-generation", $kGen) }
    if ($MergeRedisPassword -ne "")      { $a += @("-redis-password", $MergeRedisPassword) }
    if ($mapPwd -ne "")                  { $a += @("-mapping-redis-password", $mapPwd) }
    if ($MergeNoticeRedisAddr -ne "")    { $a += @("-notice-redis-addr", $MergeNoticeRedisAddr) }
    if ($MergeFriendRedisAddr -ne "")    { $a += @("-friend-redis-addr", $MergeFriendRedisAddr) }
    if ($MergeSceneRedisAddr -ne "")     { $a += @("-scene-redis-addr", $MergeSceneRedisAddr) }
    # 建表清单:go run 在 tools/merge_zone 里跑,相对路径会指错地方,一律转绝对。
    $tableList = if ($MergeTableListJson -ne "") { $MergeTableListJson }
                 else { Join-Path $repoRoot "generated\data\mysql_database_table_list.json" }
    $a += @("-table-list-json", (Convert-Path -LiteralPath $tableList -ErrorAction SilentlyContinue) ?? $tableList)
    if ($MergeSourceDataRedis -ne "")    { $a += @("-source-data-redis", $MergeSourceDataRedis, "-source-data-redis-db", $MergeSourceDataRedisDB) }
    if ($MergeTargetDataRedis -ne "")    { $a += @("-target-data-redis", $MergeTargetDataRedis, "-target-data-redis-db", $MergeTargetDataRedisDB) }
    if ($MergeDataRedisPassword -ne "")  { $a += @("-data-redis-password", $MergeDataRedisPassword) }
    if ($MergeExpectedSrcPlayers -ge 0)  { $a += @("-expected-src-players", $MergeExpectedSrcPlayers) }
    Write-Host "merge_zone: mapping=$mapAddr db=$mapDB | guild=$MergeRedisAddr db=$MergeRedisDB | login db=$MergeNoticeRedisDB | friend db=$MergeFriendRedisDB" -ForegroundColor DarkGray
    return $a
}

function Invoke-Help {
    @"
dev_tools.ps1 command help

Primary proto generator commands:
    -Command proto-gen-build
    -Command proto-gen-run

Compatibility aliases (legacy):
    -Command pbgen-build
    -Command pbgen-run

Useful proto-gen flags:
    -EnablePprof   Enable pprof wait mode (PROTOGEN_ENABLE_PPROF=1)
    -UseBinary     Force running proto-gen binary (proto-gen.exe, fallback: pbgen.exe)
    -UseGoRun      Force running go run ./cmd

Go micro-service commands (local dev):
    -Command go-svc-start [-GoServices login,db,...] [-GoCounts @{login=2;scene_manager=2}] [-GoPortStride N]
    -Command go-svc-start-exe [-GoServices login,db,...] [-GoCounts @{login=2}] [-GoPortStride N]
    -Command go-svc-stop  [-GoServices login,...]
    -Command go-svc-status
    -Command go-svc-list

Go micro-service build commands (local binary):
    -Command go-svc-build [-GoServices login,db,...]
        Build native binaries for selected (or all) Go services -> bin\go_services\

Go micro-service Docker image commands:
    -Command go-svc-build-images [-GoSvcRegistry <registry> -GoSvcTag <tag>]
    -Command go-svc-push-images  [-GoSvcRegistry <registry> -GoSvcTag <tag>]

Java service Docker image commands:
    -Command java-svc-build-image [-JavaSvcRegistry <registry> -JavaSvcTag <tag>]
    -Command java-svc-push-image  [-JavaSvcRegistry <registry> -JavaSvcTag <tag>]

C++ node commands (local dev):
    -Command cpp-node-start [-CppNodes gate,scene,battle] [-GateCount N] [-SceneCount N] [-BattleCount N] [-NodeIp <ip>|loopback|engine]
    -Command cpp-node-stop  [-CppNodes gate,...]
    -Command cpp-node-status
    -Command cpp-node-list

Unified dev commands (C++ nodes + Go services):
    -Command dev-start  [-GateCount N] [-SceneCount N] [-BattleCount N] [-GoCounts @{login=2}]
    -Command dev-start-exe  [-GateCount N] [-SceneCount N] [-BattleCount N] [-GoCounts @{login=2}]
    -Command dev-stop
    -Command dev-status

Other common commands:
    -Command git-stats [-StatsYear <year> -StatsMonth <month> -StatsAuthor <author>]
    -Command tree
    -Command naming-audit
    -Command naming-apply
    -Command third-party-grpc-build
    -Command iwyu-run
    -Command k8s-build-all
        Build all Docker images (C++ node + Go services + Java auth) before deployment.
    -Command k8s-infra-up | k8s-infra-down | k8s-infra-status
        Manage shared infrastructure (etcd/redis/kafka/mysql) in the mmorpg-infra namespace.
    -Command k8s-zone-up | k8s-zone-down | k8s-zone-status
    -Command k8s-all-up | k8s-all-down | k8s-all-status
    -Command k8s-exposure-preflight
    -Command k8s-stage-runtime
    -Command k8s-image-preflight | k8s-build-image | k8s-push-image | k8s-release-zone | k8s-release-all

Zone merge (合服) commands — full SOP: docs/ops/merge-zone-runbook.md
    -Command merge-zone -MergeBackfillZone <id> [-DryRun]
        PREREQUISITE, run once per existing zone BEFORE the first ever merge:
        seeds player:zone:{id} (SET NX) from zone_<id>_db.player_database.
        Players without that mapping are silently left behind by a merge.
    -Command merge-zone-audit -MergeSourceZone <s> -MergeTargetZone <d>
        Read-only pre-merge gates (scene nodes / online / locks / kafka queues).
        Exit 0=clean, 1=blocking finding, 2=the audit itself could not run.
    -Command merge-zone -MergeSourceZone <s> -MergeTargetZone <d> [-DryRun]
        Player rows (zone_<s>_db -> zone_<d>_db) + guild MySQL/ZSET/cache +
        player:zone remap + post-merge notice flags. Writes a JSON manifest
        BEFORE the first write (-MergeManifestPath).
        -DryRun first, ALWAYS; record players_in_source and pass it back as
        -MergeExpectedSrcPlayers on the real run.
    -Command merge-zone-audit ... -VerifyMerged -MergeExpectedSrcPlayers <n>
        Post-merge assertions (runbook Step 5).
    -Command merge-zone-unmerge -MergeManifestPath <file> [-DryRun]
        Reverse one run, object by object. Target-zone natives are untouched.

Proto-gen naming docs:
    tools/docs/proto_gen_naming_audit.md
    tools/docs/proto_gen_naming_migration.md
"@ | Write-Output
}

switch ($Command) {
    "help" { Invoke-Help }
    "pbgen-build" { Invoke-ProtoGenBuild }
    "pbgen-run" { Invoke-ProtoGenRun }
    "proto-gen-build" { Invoke-ProtoGenBuild }
    "proto-gen-run" { Invoke-ProtoGenRun }
    "tree" { Invoke-Tree }
    "naming-audit" { Invoke-NamingAudit }
    "naming-apply" { Invoke-NamingApply }
    "third-party-grpc-build" { Invoke-ThirdPartyGrpcBuild }
    "iwyu-run"              { Invoke-IwyuRun }
    "k8s-infra-up" { Invoke-K8sDeploy -K8sCommand "infra-up" }
    "k8s-infra-down" { Invoke-K8sDeploy -K8sCommand "infra-down" }
    "k8s-infra-status" { Invoke-K8sDeploy -K8sCommand "infra-status" }
    "k8s-zone-up" { Invoke-K8sDeploy -K8sCommand "zone-up" }
    "k8s-zone-down" { Invoke-K8sDeploy -K8sCommand "zone-down" }
    "k8s-zone-status" { Invoke-K8sDeploy -K8sCommand "zone-status" }
    "k8s-all-up" { Invoke-K8sDeploy -K8sCommand "all-up" }
    "k8s-all-down" { Invoke-K8sDeploy -K8sCommand "all-down" }
    "k8s-all-status" { Invoke-K8sDeploy -K8sCommand "all-status" }
    "k8s-build-all" { Invoke-K8sBuildAll }
    "k8s-exposure-preflight" { Invoke-K8sExposurePreflight }
    "k8s-stage-runtime" { Invoke-K8sStageRuntime }
    "k8s-image-preflight" { Invoke-K8sImage -ImageCommand "preflight" }
    "k8s-build-image" { Invoke-K8sImage -ImageCommand "build-image" }
    "k8s-push-image" { Invoke-K8sImage -ImageCommand "push-image" }
    "k8s-release-zone" { Invoke-K8sImage -ImageCommand "release-zone" }
    "k8s-release-all" { Invoke-K8sImage -ImageCommand "release-all" }
    "go-svc-start"  { & (Join-Path $ScriptDir "go_services.ps1") -Command start  -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds -Zone $Zone -ZonePortShift $ZonePortShift }
    "go-svc-start-exe"  { & (Join-Path $ScriptDir "go_services.ps1") -Command start-exe  -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds -Zone $Zone -ZonePortShift $ZonePortShift }
    "go-svc-stop"   { & (Join-Path $ScriptDir "go_services.ps1") -Command stop   -Services $GoServices }
    "go-svc-status" { & (Join-Path $ScriptDir "go_services.ps1") -Command status }
    "go-svc-list"   { & (Join-Path $ScriptDir "go_services.ps1") -Command list   }
    "go-svc-build"        { & (Join-Path $ScriptDir "go_services.ps1") -Command build  -Services $GoServices }
    "go-svc-build-images" { & (Join-Path $ScriptDir "go_svc_image.ps1") -Command build-all -Registry $GoSvcRegistry -Tag $GoSvcTag -DryRun:$DryRun }
    "go-svc-push-images"  { & (Join-Path $ScriptDir "go_svc_image.ps1") -Command push-all  -Registry $GoSvcRegistry -Tag $GoSvcTag -DryRun:$DryRun }
    "java-svc-build-image" { & (Join-Path $ScriptDir "java_svc_image.ps1") -Command build -Registry $JavaSvcRegistry -Tag $JavaSvcTag -DryRun:$DryRun }
    "java-svc-push-image"  { & (Join-Path $ScriptDir "java_svc_image.ps1") -Command push  -Registry $JavaSvcRegistry -Tag $JavaSvcTag -DryRun:$DryRun }
    "cpp-node-start"  { & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command start  -Nodes $CppNodes -GateCount $GateCount -SceneCount $SceneCount -BattleCount $BattleCount -Zone $Zone -NodeIp $NodeIp }
    "cpp-node-stop"   { & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command stop   -Nodes $CppNodes }
    "cpp-node-status" { & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command status }
    "cpp-node-list"   { & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command list   }
    "dev-start" {
        Write-Host "=== Starting Go services ===" -ForegroundColor Cyan
        & (Join-Path $ScriptDir "go_services.ps1") -Command start -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds
        Write-Host "`n=== Starting C++ nodes ==="  -ForegroundColor Cyan
        & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command start -Nodes $CppNodes -GateCount $GateCount -SceneCount $SceneCount -BattleCount $BattleCount -NodeIp $NodeIp
    }
    "dev-start-exe" {
        Write-Host "=== Starting Go services (exe) ===" -ForegroundColor Cyan
        & (Join-Path $ScriptDir "go_services.ps1") -Command start-exe -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds
        Write-Host "`n=== Starting C++ nodes ==="  -ForegroundColor Cyan
        & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command start -Nodes $CppNodes -GateCount $GateCount -SceneCount $SceneCount -BattleCount $BattleCount -NodeIp $NodeIp
    }
    "dev-start-zones" {
        # Multi-zone local stress launch. Each zone gets its own go services
        # (zone-prefixed instance keys + derived yaml with ZoneId/port rewritten)
        # and its own gate/scene processes (ZONE_ID env var override).
        # Shared infra (etcd/Redis/Kafka/MySQL) is reused; per-zone DB schemas
        # (zone_<N>_db) must be pre-created via deploy/mysql-init/00_init_zone_dbs.sql.
        $zoneList = if ($Zones.Count -gt 0) { $Zones } else { @(1, 2) }
        Write-Host "=== Multi-zone launch: zones [$($zoneList -join ',')] ===" -ForegroundColor Cyan
        foreach ($z in $zoneList) {
            Write-Host "`n>>> Zone ${z}: starting Go services (exe) ..." -ForegroundColor Cyan
            & (Join-Path $ScriptDir "go_services.ps1") -Command start-exe -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds -Zone $z -ZonePortShift $ZonePortShift
            Write-Host "`n>>> Zone ${z}: starting C++ nodes ..." -ForegroundColor Cyan
            # battle 是全局池(非 zone-scoped,所有 zone 的 gate 都能发现),
            # 只在第一个 zone 起 $BattleCount 份,其余 zone 传 0 避免重复起。
            $zoneBattleCount = if ($z -eq $zoneList[0]) { $BattleCount } else { 0 }
            & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command start -Nodes $CppNodes -GateCount $GateCount -SceneCount $SceneCount -BattleCount $zoneBattleCount -Zone $z -NodeIp $NodeIp
        }
        Write-Host "`nAll zones launched. Use 'dev status' to inspect." -ForegroundColor Green
    }
    "dev-stop" {
        Write-Host "=== Stopping Go services ===" -ForegroundColor Cyan
        & (Join-Path $ScriptDir "go_services.ps1") -Command stop -Services $GoServices
        Write-Host "`n=== Stopping C++ nodes ==="  -ForegroundColor Cyan
        & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command stop -Nodes $CppNodes
    }
    "dev-status" {
        Write-Host "=== C++ nodes ==="  -ForegroundColor Cyan
        & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command status
        Write-Host "`n=== Go services ===" -ForegroundColor Cyan
        & (Join-Path $ScriptDir "go_services.ps1") -Command status
    }
    "merge-zone" {
        # Drives tools/merge_zone/main.go directly via `go run`. The earlier
        # implementation invoked a `merge_zone.ps1` wrapper that never
        # actually existed in the repo — we route to `go run` to remove the
        # phantom dependency. See docs/ops/merge-zone-runbook.md §5.
        #
        # 三种子模式共用这一个命令:
        #   -MergeBackfillZone <id>   存量 player:zone 回填(合服前置,每个 zone 各跑一次)
        #   -MergeManifestPath + -Unmerge 语义由 -Command merge-zone-unmerge 承担
        #   其余                       正常合服
        #
        # ⚠️ tools/merge_zone 是**独立 go module**:`go run` 必须在那个目录里跑。
        # 旧代码在仓库根跑 `go run ./tools/merge_zone`,结果恒为
        # "go: cannot find main module" —— 这个入口从来没有真正执行过一次合服。
        $repoRoot = (Resolve-Path (Join-Path $ScriptDir "..\..")).Path
        $mergeDir = Join-Path $repoRoot "tools\merge_zone"
        # 清单必须落在仓库根(而不是 go run 的工作目录),而且路径要绝对 —— 它是
        # 撤销与复核的唯一凭据,不能因为切了目录就找不着。
        $manifest = if ($MergeManifestPath -ne "") { [System.IO.Path]::GetFullPath($MergeManifestPath, $PWD.Path) }
                    else { Join-Path $repoRoot ("merge_{0}_to_{1}_{2}.json" -f $MergeSourceZone, $MergeTargetZone, (Get-Date).ToUniversalTime().ToString("yyyyMMddTHHmmssZ")) }
        Push-Location $mergeDir
        try {
            if ($MergeBackfillZone -gt 0) {
                $bfArgs = @("run", ".", "-backfill-home-zone", "-zone", $MergeBackfillZone)
                $bfArgs += Get-MergeZoneArgs
                if ($DryRun) { $bfArgs += "-dry-run" } else { $bfArgs += "-apply" }
                & go @bfArgs
                return
            }
            if ($MergeSourceZone -le 0 -or $MergeTargetZone -le 0) {
                throw "merge-zone requires -MergeSourceZone <id> and -MergeTargetZone <id> (or -MergeBackfillZone <id> for the backfill mode)"
            }
            $mergeGoArgs = @(
                "run", ".",
                "-source-zone", $MergeSourceZone,
                "-target-zone", $MergeTargetZone,
                "-manifest-path", $manifest
            )
            $mergeGoArgs += Get-MergeZoneArgs
            if ($MergeMigratePlayerBlobs)         { $mergeGoArgs += "-migrate-player-blobs" }
            if ($MergeClearSourceHotState)        { $mergeGoArgs += "-clear-source-hot-state" }
            if ($MergeAllowEmptySource)           { $mergeGoArgs += "-allow-empty-source" }
            if ($MergeAssumeKafkaDrained)         { $mergeGoArgs += "-assume-kafka-drained" }
            if ($MergeKafkaConsumerGroupsCmd -ne "") {
                $mergeGoArgs += @("-kafka-consumer-groups-cmd", $MergeKafkaConsumerGroupsCmd, "-kafka-bootstrap", $MergeKafkaBootstrap)
            }
            if ($DryRun) { $mergeGoArgs += "-dry-run" } else { $mergeGoArgs += "-apply" }
            & go @mergeGoArgs
        }
        finally { Pop-Location }
    }
    "merge-zone-unmerge" {
        # 按清单逐对象撤销一次合服(目标区原住民不受影响)。
        # 只在「刚合完、还没开服」的窗口里用;开服之后请走备份还原。
        if ($MergeManifestPath -eq "") {
            throw "merge-zone-unmerge requires -MergeManifestPath <the manifest written by the original run>"
        }
        $repoRoot = (Resolve-Path (Join-Path $ScriptDir "..\..")).Path
        $manifest = [System.IO.Path]::GetFullPath($MergeManifestPath, $PWD.Path)
        if (-not (Test-Path -LiteralPath $manifest)) { throw "manifest not found: $manifest" }
        $unArgs = @("run", ".", "-mode", "unmerge", "-manifest-path", $manifest)
        $unArgs += Get-MergeZoneArgs
        if ($DryRun) { $unArgs += "-dry-run" } else { $unArgs += "-apply" }
        Push-Location (Join-Path $repoRoot "tools\merge_zone")
        try { & go @unArgs }
        finally { Pop-Location }
    }
    "merge-zone-audit" {
        # Read-only inspection — see tools/merge_zone/audit_resources.go and
        # docs/ops/merge-zone-runbook.md §4.2. Reports per-resource counts +
        # conflicts; never writes. Safe to run any time, even on a live zone.
        # T-1 day uses default mode; post-merge verification uses -VerifyMerged.
        #
        # 退出码:0=干净 / 1=有 block 级发现(不许合服)/ 2=审计自己没跑成
        # (连不上某个库),结论不可信 —— 不要把 2 当成 1 处理。
        if ($MergeSourceZone -le 0 -or $MergeTargetZone -le 0) {
            throw "merge-zone-audit requires -MergeSourceZone <id> and -MergeTargetZone <id>"
        }
        $repoRoot = (Resolve-Path (Join-Path $ScriptDir "..\..")).Path
        $auditArgs = @(
            "run", ".",
            "-mode", "audit",
            "-source-zone", $MergeSourceZone,
            "-target-zone", $MergeTargetZone
        )
        $auditArgs += Get-MergeZoneArgs
        if ($VerifyMerged) { $auditArgs += "-verify-merged" }
        Push-Location (Join-Path $repoRoot "tools\merge_zone")
        try { & go @auditArgs }
        finally { Pop-Location }
    }
    "kafka-offset-reset" {
        # Wraps kafka_offset_reset.ps1 — see docs/design/zone_data_rollback.md §3 step 4.
        # Used during zone rollback to reset db_task_zone_<id> offsets before restarting a zone.
        $kArgs = @("-BootstrapServer", $KafkaBootstrapServer, "-Topic", $KafkaTopic)
        if ($KafkaGroup -ne "")               { $kArgs += @("-Group", $KafkaGroup) }
        if ($KafkaToDatetime -ne "")          { $kArgs += @("-ToDatetime", $KafkaToDatetime) }
        if ($KafkaToEarliest)                 { $kArgs += "-ToEarliest" }
        if ($KafkaToLatest)                   { $kArgs += "-ToLatest" }
        if ($KafkaDeleteAndRecreate)          { $kArgs += "-DeleteAndRecreateTopic" }
        if ($KafkaPartitions -gt 0)           { $kArgs += @("-Partitions", $KafkaPartitions) }
        if ($KafkaReplicationFactor -ne 1)    { $kArgs += @("-ReplicationFactor", $KafkaReplicationFactor) }
        if ($KafkaBin -ne "")                 { $kArgs += @("-KafkaBin", $KafkaBin) }
        if ($KafkaApply)                      { $kArgs += "-Apply" }
        & (Join-Path $ScriptDir "kafka_offset_reset.ps1") @kArgs
    }
    "k8s-zone-rollback" {
        # Wraps k8s_zone_rollback.ps1 — see docs/design/zone_data_rollback.md §3.
        # Orchestrates: zone-down → Kafka drain → MySQL PITR (manual) → Redis FLUSHDB
        #             → Kafka offset reset → zone-up → verification checklist.
        if ($RollbackTargetTime -eq "") {
            throw "k8s-zone-rollback requires -RollbackTargetTime (ISO 8601 UTC, e.g. '2026-05-15T14:23:00Z')"
        }
        # 回滚目标版本必须显式给。默认的 git 短 sha 是**当前工作树**的版本,
        # 拿它回滚等于"把数据回档到过去、把代码留在现在",比不回滚更危险。
        if (-not $script:NodeImageExplicit) {
            throw "k8s-zone-rollback requires an explicit -NodeImage (回滚目标版本的镜像引用,如 ghcr.io/luyuancpp/mmorpg-node:<回滚目标 sha>)。默认值是当前工作树的 sha,不是回滚目标。"
        }
        # 必须 hashtable splatting:数组 splatting 会按**位置**绑定,
        # "-ZoneName" 这个字符串本身会被绑到 ZoneName、"$ZoneName" 绑到 [int]$ZoneId,
        # 于是灾难回滚脚本连第一步都进不去。
        $rbArgs = @{
            ZoneName             = $ZoneName
            ZoneId               = $ZoneId
            TargetTime           = $RollbackTargetTime
            KafkaBootstrap       = $KafkaBootstrapServer
            KafkaTopic           = $RollbackKafkaTopic
            KafkaGroup           = $RollbackKafkaGroup
            NodeImage            = $NodeImage
            NamespacePrefix      = $NamespacePrefix
            KafkaDrainTimeoutSec = $RollbackKafkaDrainTimeoutSec
        }
        if ($RollbackRedisHost -ne "")     { $rbArgs.RedisHost = $RollbackRedisHost }
        if ($RollbackRedisPort -ne "6379") { $rbArgs.RedisPort = $RollbackRedisPort }
        if ($RollbackRedisPassword -ne "") { $rbArgs.RedisPassword = $RollbackRedisPassword }
        if ($RollbackRedisDB -ne 0)        { $rbArgs.RedisDB = $RollbackRedisDB }
        if ($RollbackSkipMySqlPause)       { $rbArgs.SkipMySqlPause = $true }
        if ($RollbackApply)                { $rbArgs.Apply = $true }
        & (Join-Path $ScriptDir "k8s_zone_rollback.ps1") @rbArgs
    }
    "dev-robot-zones" {
        # Per-zone parallel robot launch. Builds robot once, then for each
        # requested zone derives a yaml at run/etc/robot/robot.z<N>.yaml with
        # zone_id rewritten and account_fmt prefixed by 'z<N>_' to avoid
        # account-name collisions across zones. Each robot is launched in its
        # own console window so stress output stays separable.
        $zoneList = if ($Zones.Count -gt 0) { $Zones } else { @(1, 2) }
        $robotDir = Join-Path $RepoRoot "robot"
        $robotExe = Join-Path $robotDir "robot.exe"
        $baseYaml = Join-Path $robotDir "etc\robot.yaml"
        if (-not (Test-Path $baseYaml)) { throw "Base robot yaml not found: $baseYaml" }

        Write-Host "=== Building robot ===" -ForegroundColor Cyan
        Push-Location $robotDir
        try {
            go build -o robot.exe .
            if ($LASTEXITCODE -ne 0) { throw "robot build failed" }
        } finally { Pop-Location }

        $derivedDir = Join-Path $RepoRoot "run\etc\robot"
        if (-not (Test-Path $derivedDir)) { New-Item -ItemType Directory -Path $derivedDir -Force | Out-Null }

        $baseContent = Get-Content $baseYaml -Raw
        $zoneRegex    = [regex]::new('(?m)^(?<lead>zone_id:\s*)\d+\s*(?<tail>(#.*)?)$')
        $accountRegex = [regex]::new('(?m)^(?<lead>account_fmt:\s*")(?<fmt>[^"\r\n]+)(?<tail>".*)$')

        foreach ($z in $zoneList) {
            $content = $baseContent
            if ($zoneRegex.IsMatch($content)) {
                $content = $zoneRegex.Replace(
                    $content,
                    { param($m) "$($m.Groups['lead'].Value)$z $($m.Groups['tail'].Value)".TrimEnd() },
                    1
                )
            } else {
                Write-Warning "No 'zone_id:' line found in $baseYaml; zone $z derived yaml may not target the right zone."
            }
            if ($accountRegex.IsMatch($content)) {
                $content = $accountRegex.Replace(
                    $content,
                    { param($m) "$($m.Groups['lead'].Value)z${z}_$($m.Groups['fmt'].Value)$($m.Groups['tail'].Value)" },
                    1
                )
            }
            $derivedYaml = Join-Path $derivedDir "robot.z${z}.yaml"
            Set-Content -Path $derivedYaml -Value $content -Encoding UTF8 -NoNewline
            Write-Host "[zone $z] cfg -> $derivedYaml" -ForegroundColor DarkGray

            # Launch in a new console window so each zone's stress output is
            # visible side-by-side. -WindowStyle Normal so it actually pops up.
            $title = "robot-z$z"
            Start-Process -FilePath "cmd.exe" `
                -ArgumentList @("/k", "title $title && `"$robotExe`" -c `"$derivedYaml`"") `
                -WorkingDirectory $robotDir `
                -WindowStyle Normal | Out-Null
            Write-Host "[zone $z] robot launched (window title: $title)" -ForegroundColor Green
        }
    }
    "git-stats" { Invoke-GitStats }
    default { throw "Unsupported command: $Command" }
}
