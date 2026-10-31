param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("help", "pbgen-build", "pbgen-run", "proto-gen-build", "proto-gen-run", "tree", "naming-audit", "naming-apply", "third-party-lock-check", "third-party-zlib-build", "third-party-protobuf-build", "third-party-boringssl-build", "no-raw-pointer-setup", "iwyu-run", "k8s-infra-up", "k8s-infra-down", "k8s-infra-status", "k8s-redis-split-status", "k8s-redis-wipe", "k8s-zone-up", "k8s-zone-down", "k8s-zone-status", "k8s-zone-rollback", "k8s-all-up", "k8s-all-down", "k8s-all-status", "k8s-build-all", "k8s-exposure-preflight", "k8s-stage-runtime", "k8s-image-preflight", "k8s-build-image", "k8s-push-image", "k8s-release-zone", "k8s-release-all", "k8s-route-tool", "go-svc-start", "go-svc-start-exe", "go-svc-stop", "go-svc-status", "go-svc-list", "go-svc-build", "go-svc-migrate", "sql-boot-catalog-local", "go-svc-build-images", "go-svc-push-images", "java-svc-build-image", "java-svc-push-image", "cpp-node-start", "cpp-node-stop", "cpp-node-status", "cpp-node-list", "dev-start", "dev-start-exe", "dev-start-zones", "dev-stop", "dev-status", "dev-robot-zones", "merge-zone", "merge-zone-audit", "merge-zone-unmerge", "kafka-offset-reset", "redis-wipe-local", "git-stats")]
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
    # 第三方固定版本检查与隔离构建；留空时沿用对应脚本默认值。
    [switch]$ApplyPatches,
    [string]$BuildRoot = "",
    [string]$ZlibInstallRoot = "",
    [ValidateSet("Debug", "Release")]
    [string]$Configuration = "Debug",
    [ValidateSet("x64", "x86")]
    [string]$Architecture = "x64",
    [string]$PreferredMsvcVersion = "14.51",

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
    # 发布档位,透传给 k8s_image.ps1 / k8s_deploy.ps1;k8s-zone-rollback 只在显式给了时转给它的 Step 6(staging / prod zone 回滚必须显式传)。
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
    # C++ 日志 sidecar(k8s-infra-up / k8s-zone-up / k8s-all-up / k8s-image-*),透传给 k8s_deploy.ps1。
    # infra-up 也算一条:battle 是全局池,它那份 sidecar 由 infra-up 路径创建(Apply-BattlePool),
    # 漏写会让人以为 infra-up 上关不掉。
    # 两个字符串一律默认留空 = 不覆盖 k8s_deploy.ps1 的默认值:sidecar 镜像钉死版本
    # (v1.19.2 有读取位置回归)只允许存在于 k8s_deploy.ps1 一处,抄第二份迟早漂移,
    # 到时候"文档说钉在 v1.10.0、实际部署的是别的版本"很难查。
    [switch]$NoCppLogSidecar,
    [string]$CppLogSidecarImage = "",
    [string]$LokiPushUrl = "",
    # 默认留空 = 不覆盖下游默认值(k8s_deploy.ps1 默认 NodePort;-OpsProfile managed-cloud / bare-metal 时
    # k8s_deploy.ps1 的 Apply-OpsProfileDefaults 还会强制改写),非空才透传。写法同 -GateRouterMode:默认值只留在
    # k8s_deploy.ps1 一处,k8s_image.ps1 同口径。以前这里写死 "NodePort" 并无条件透传,是第二份默认值。
    # k8s-exposure-preflight 的三个用例自带 CaseGateServiceType,不读本参数。
    [ValidateSet("", "ClusterIP", "NodePort", "LoadBalancer")]
    [string]$GateServiceType = "",
    [int]$GateServicePort = 18000,
    # gate 客户端 RPC 路由模式(k8s-zone-up / k8s-all-up / k8s-release-* 等),透传给 k8s_deploy.ps1 / k8s_image.ps1。
    # 默认留空 = 不覆盖下游默认值(k8s_deploy.ps1 默认 "1",turn-based §22 D75):默认值只允许存在于
    # k8s_deploy.ps1 一处,与 CppLogSidecarImage 同一理由。非空才透传,用于从本入口回退到 "0"
    # (回退后果与前置见 k8s_deploy.ps1 -GateRouterMode 参数注释)。
    [ValidateSet("", "0", "1")]
    [string]$GateRouterMode = "",
    # B0-4 运维豁免开关(NODE_OPEN_FILE_LIMIT_MODE),透传给 k8s_deploy.ps1 -NodeOpenFileLimitMode(k8s-infra-up / k8s-zone-up /
    # k8s-all-up)、k8s_image.ps1(k8s-release-*)与 k8s_zone_rollback.ps1(k8s-zone-rollback,由它的 Step 6 转给 k8s-zone-up)。
    # 取值语义、覆盖范围(zone-up 的 gate / scene,infra-up 的 battle)与 warn 的代价见 k8s_deploy.ps1 同名参数注释。
    # 写法同 -GateRouterMode:默认留空 = 不透传,由 k8s_deploy.ps1 接管(那边留空 = 不写 env、进程按 enforce),默认值只留在那一处。
    # 与下游同一 ValidateSet,同样区分大小写(C++ 只认小写):大写或拼错在本入口就拒,不拖到 k8s_image.ps1 推完镜像、
    # 或回滚已停服之后才由下游拒。⚠ 不粘滞:以 warn 运行的环境经本入口重新部署却不传,会落回 enforce,硬限仍不够就再次拒启。
    [ValidateSet("", "enforce", "warn", IgnoreCase = $false)]
    [string]$NodeOpenFileLimitMode = "",
    # 集群外客户端入口,透传给 k8s_deploy.ps1(k8s-infra-up / k8s-zone-up / k8s-all-up)、k8s_image.ps1(k8s-release-*)
    # 或 k8s_zone_rollback.ps1(k8s-zone-rollback,由它的 Step 6 转给 k8s-zone-up),语义见 k8s_deploy.ps1 同名参数与
    # docs/design/k8s-client-entry.md(集群外入口 D80 / D88 / D89 / D91)。
    # 写法同 -GateRouterMode:一律默认留空 = 不覆盖下游默认值(ClientEntryMode=podip、RequireClientEndpoint=auto、
    # BattleOrchestrator=deployment、GateNodePortBase=30000、GateExternalTrafficPolicy=Local、GatewayIngressClassName=nginx
    # 都只留在 k8s_deploy.ps1 一处),非空才透传;枚举型保留 ValidateSet,拼错在本入口就拒,不等到下游。
    # 生效范围:battle 是全局池,只在 infra-up 路径部署,所以 -BattleOrchestrator 只对 k8s-infra-up / k8s-all-up /
    # k8s-release-all(均不带 -SkipInfra)生效。gateway Ingress 由 zone-up 随 gateway 生成(k8s-zone-up、k8s-all-up 的各 zone,
    # 需部署 Java 服务),k8s-infra-up 上给 -Gateway* 只会得到下游的"被忽略"警告。-Gateway* 与 -LoginDevPasswordAuth
    # 落在 Go / Java 服务上,而 k8s_image.ps1 不部署 Go / Java 服务,所以 k8s-image-* / k8s-release-* 显式给了会被
    # Invoke-K8sImage 拒绝;-RequireClientEndpoint 例外地照常透传(见它的参数注释)。
    # 模式不粘滞:k8s_deploy.ps1 每次按本次参数重新生成 gate / battle 工作负载,不从集群读回旧值。以 external 运行的
    # infra / zone,之后每次经本入口重新部署都必须再传同一组参数;漏传时 k8s_deploy.ps1 按集群现状在任何写操作之前拒绝
    # (确要切换形态或改地址,加 -AllowDisruptiveSwitch)。
    # k8s-zone-rollback 转发 -GateRouterMode、本组的 gate / Ingress 参数、-GateServiceType、-RequireClientEndpoint、
    # -LoginDevPasswordAuth、第三方登录的 -Login* 与 -ConfirmLoginAuthProviderChange(见参数块末尾)、
    # -AllowDisruptiveSwitch、显式给了的 -ReleaseProfile 与 -KubeContext / -KubeConfig
    # (不转发 -BattleOrchestrator:zone 回滚不碰 battle 全局池)。它的 Step 1 删掉整个 zone namespace,k8s_deploy.ps1 的
    # 集群现状预检在 Step 6 通常无从触发,改由 k8s_zone_rollback.ps1 在停服之前自行核对,并把 Step 6 的参数先 -DryRun 一遍(见该脚本第 0 步);
    # 以 external 运行的 zone 回滚时照传上一次部署的同一组值。
    [ValidateSet("", "podip", "external")]
    [string]$ClientEntryMode = "",
    [string]$ClientPublicHost = "",
    [string]$GateClientHostTemplate = "",
    # -1 = 未指定(不透传,由 k8s_deploy.ps1 默认值 / zones.json 每个 zone 的 gateNodePortBase 决定);其余值原样透传,由下游校验。
    [int]$GateNodePortBase = -1,
    [ValidateSet("", "Local", "Cluster")]
    [string]$GateExternalTrafficPolicy = "",
    [ValidateSet("", "deployment", "agones")]
    [string]$BattleOrchestrator = "",
    [string]$GatewayIngressHost = "",
    [string]$GatewayIngressClassName = "",
    [string]$GatewayIngressTlsSecret = "",
    # 逗号分隔的 CIDR,原样透传。
    [string]$GatewayTrustedProxies = "",
    # 集群内 login 的开发口令认证(DevPasswordAuth),只允许 -ReleaseProfile dev,由 k8s_deploy.ps1 校验;
    # switch 只在被指定时透传,共享密钥走环境变量 / Secret,不经参数。不粘滞:k8s-zone-rollback 同样转发给它的 Step 6,
    # dev zone 依赖开发口令登录时回滚要照传,漏传则 Step 6 生成的 login 没有任何认证配置。
    [switch]$LoginDevPasswordAuth,
    # login / scene_manager ConfigMap 的 RequireClientEndpoint(auto | true | false),透传给 k8s_deploy.ps1(k8s-zone-up /
    # k8s-all-up)、k8s_image.ps1(k8s-release-*)与 k8s-zone-rollback 的 Step 6;取值口径与上线窗口(逐个 zone 切 external 时
    # 已切 zone 传 false)见 k8s_deploy.ps1 同名参数注释。留空不透传 = 下游默认 auto(跟随 -ClientEntryMode)。
    # k8s-release-* 不部署 login / scene_manager,经那条路只参与 k8s_deploy.ps1 的组合预检、不改写 ConfigMap,由 k8s_image.ps1 告警说明。
    [ValidateSet("", "auto", "true", "false")]
    [string]$RequireClientEndpoint = "",
    # 确认本次就是要切换入口形态(-ClientEntryMode / -BattleOrchestrator 换值)或改 gate / battle 的客户端地址来源(会踢人、
    # 在打的战斗作废),透传给 k8s_deploy.ps1 / k8s_image.ps1 / k8s_zone_rollback.ps1 同名参数;switch 只在被指定时透传,
    # 只在维护窗口里显式加。k8s-zone-rollback 下它只转给 Step 6:Step 1 已删掉整个 zone namespace,这道闸通常无从触发,
    # 只在 namespace 没删净时由 k8s_deploy.ps1 按集群现状把关;它不豁免回滚脚本第 0 步的停服前核对(见 k8s_zone_rollback.ps1 同名参数)。
    [switch]$AllowDisruptiveSwitch,
    # 确认本次 infra-up 可以重启共享 Redis(redis-0,全服登录中断并重放 AOF),透传给 k8s_deploy.ps1 / k8s_image.ps1 同名参数,
    # 语义见 k8s_deploy.ps1 参数注释(P0-E 重启闸);switch 只在被指定时透传,只在维护窗口里显式加。
    # 只对会 apply infra 的入口有意义:k8s-infra-up、k8s-all-up、k8s-release-all(均不带 -SkipInfra);k8s-zone-rollback 不 infra-up,不转发。
    [switch]$ConfirmRedisCoreRestart,
    # 共享 Redis 的形态(1 = 单实例 redis.yaml,3 = 高可用 redis-ha.yaml;0 = 不透传,由 k8s_deploy.ps1 按档位取),透传给
    # k8s_deploy.ps1 / k8s_image.ps1 同名参数,语义见 k8s_deploy.ps1 参数注释。非 0 才透传。zone 入口(k8s-zone-up 等)通常不用传:
    # 它决定 db / data-service 的 WAITAOF 副本份数,k8s_deploy.ps1 的 zone-up 没收到时跟随线上 sts/redis,收到而与线上不同即拒绝。
    [ValidateRange(0, 3)]
    [int]$RedisReplicas = 0,
    # 共享 Redis 按用途拆实例(G4a 第 1a 批),透传给 k8s_deploy.ps1 / k8s_image.ps1 同名参数,语义见 k8s_deploy.ps1 参数注释。
    #   -RedisSplit:auto(缺省)= 不透传,由 k8s_deploy.ps1 解析(所有档位不拆;zone 写路径跟随线上);none / queue 才透传。
    #     k8s-zone-rollback 同样只转发 none / queue:回档 Step 1 删掉 namespace 后 auto 读不到线上,以 queue 运行的 zone 回档要显式给 queue。
    #   -ConfirmRedisSplitSwitch:确认本次就是要改 login / gateway 的排队 Redis 地址,switch 只在被指定时透传(k8s-zone-rollback 不支持)。
    #   -RedisQueueMaxMemory:queue 实例的 maxmemory(如 1gb),非空才透传;只对会 apply infra 的入口有意义(k8s-zone-rollback 不支持)。
    [ValidateSet("auto", "none", "queue")]
    [string]$RedisSplit = "auto",
    [switch]$ConfirmRedisSplitSwitch,
    [string]$RedisQueueMaxMemory = "",
    # k8s-redis-wipe 的显式确认(压测前清空部署拓扑里的每个 Redis 实例,AGENTS.md §6.2),透传给 k8s_deploy.ps1 -Command redis-wipe;
    # 那边还要求 -InfraNamespace 匹配 ^mmorpg-stress-。switch 只在被指定时透传。
    [switch]$ConfirmWipeAllRedis,
    # 玩家域 Redis Cluster(docs/design/redis-player-store-cluster.md),透传给 k8s_deploy.ps1 / k8s_image.ps1 同名参数,语义见 k8s_deploy.ps1 参数注释。
    # 主节点数与副本数 0 = 不透传;容量非空才透传,只对会 apply infra 的入口有意义。不粘滞:staging / prod 的 zone-up、all-up、infra-up 每次都要给主节点数。
    # k8s-zone-rollback 转发前两个(Step 6 的 zone-up 要),容量给了即拒绝(回档不 infra-up)。
    [ValidateRange(0, 512)]
    [int]$PlayerRedisMasters = 0,
    [ValidateRange(0, 2)]
    [int]$PlayerRedisReplicasPerMaster = 0,
    [string]$PlayerRedisMaxMemory = "",
    [string]$PlayerRedisStorage = "",
    # k8s-zone-rollback:线上玩家域是 Redis Cluster 时必填的种子地址与口令环境变量名,转给 k8s_zone_rollback.ps1 的 -PlayerRedisSeeds / -PlayerRedisPasswordEnv。
    [string]$RollbackPlayerRedisSeeds = "",
    [string]$RollbackPlayerRedisPasswordEnv = "",
    [switch]$SkipInfra,
    [switch]$SkipGoSvc,
    [string]$GoSvcRegistry = "ghcr.io/luyuancpp",
    # 同 NodeImage:留空 = git 短 sha。
    [string]$GoSvcTag = "",
    [switch]$SkipJavaSvc,
    [string]$JavaSvcRegistry = "ghcr.io/luyuancpp",
    [string]$JavaSvcTag = "",
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
    # Cap = 32 per service.
    [object]$GoCounts = @{},
    [int]$GoPortStride = 1,
    # Disable tier-staged Go service startup (parallel launch like before).
    [switch]$NoTier,
    # Per-tier readiness budget (seconds). Soft TCP LISTEN probe per instance.
    [int]$TierReadySeconds = 10,
    # 仅本地 Go 启动：db 派生回环 MySQL 配置，不改生产源配置或 dual 围栏。
    [switch]$LocalMySQL,
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

    # no-raw-pointer-setup：可复用已有完整 LLVM 开发库，或只下载不编译。
    [string]$LlvmRoot = "",
    [switch]$DownloadOnly,

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
    # ── merge-zone / merge-zone-audit / merge-zone-unmerge ──
    # 合服只写一条映射,不动任何数据,不碰 Redis(tools/merge_zone,不停服 SOP 见 docs/ops/merge-zone-runbook.md)。
    # 全局库的 DSN 是末尾的 -MergeZoneRouteDbDsn,-DryRun 与其它命令共用。
    [int]$MergeSourceZone = 0,
    [int]$MergeTargetZone = 0,

    # ── kafka-offset-reset ────────────────────────────────────────
    # See tools/scripts/kafka_offset_reset.ps1 for full docs.
    [string]$KafkaBootstrapServer = "kafka:9092",
    [string]$KafkaGroup = "",
    [string]$KafkaTopic = "",
    [string]$KafkaToDatetime = "",      # ISO 8601 UTC, e.g. "2026-05-15T03:17:00Z"
    [switch]$KafkaToEarliest,
    [switch]$KafkaToLatest,
    [switch]$KafkaDeleteAndRecreate,
    [int]$KafkaPartitions = 0,
    # 0 = 没给(不透传)。-KafkaDeleteAndRecreate 时 kafka_offset_reset.ps1 要求显式给出、且等于现有 topic 的副本数;
    # 以前缺省 1 且只在 ≠ 1 时透传,单 broker 档显式给 1 也传不下去。
    [int]$KafkaReplicationFactor = 0,
    [string]$KafkaBin = "",
    [switch]$KafkaApply,

    # ── k8s-zone-rollback ─────────────────────────────────────────
    # See tools/scripts/k8s_zone_rollback.ps1 for full docs.
    # Wraps docs/design/zone_data_rollback.md §3 disaster recovery(负责人 2026-10-17 裁决:丢弃 TargetTime 之后的写)。
    [string]$RollbackTargetTime = "",       # UTC,yyyy-MM-ddTHH:mm:ss[.fff]Z
    # go/db 的共享 Redis(K8s 即 infra redis 的 DB 0;回档工具在这里丢弃 T 之后的状态,不做 FLUSHDB)。-RollbackApply 时必填。
    [string]$RollbackRedisHost = "",
    [string]$RollbackRedisPort = "6379",
    # 已废弃(2026-10-18):口令不再经命令行。非空即在调用回滚脚本之前拒绝,改用 -RollbackRedisPasswordEnv <环境变量名>。
    [string]$RollbackRedisPassword = "",
    [int]$RollbackRedisDB = 0,
    [switch]$RollbackSkipMySqlPause,
    # 转给 k8s_zone_rollback.ps1 -ConfirmNoGatewayIngress:显式声明回滚后该 zone 不要 Ingress gateway(Ingress 核对的显式决定,
    # 与 -GatewayIngressHost 互斥)。switch 不占位置参数,插在这里不影响后面参数的位置绑定。
    [switch]$RollbackConfirmNoGatewayIngress,
    # 已废弃(2026-10-18):回档不再 FLUSHDB(归属与位置键、交接标记、路由映射与账号目录同在一个 Redis DB)。
    # 传了即在调用回滚脚本之前拒绝;整套环境重建走 AGENTS.md §6.2。
    [switch]$RollbackConfirmFlushSharedRedis,
    # 以下转给 k8s_zone_rollback.ps1 的同名参数(去掉 Rollback 前缀),非空 / 大于 0 才转发,语义与校验都在回滚脚本一处:
    # Redis 口令所在的环境变量名、zone_<id>_db DSN 所在的环境变量名(都只给变量名)、预编译的 zone_rollback_discard、
    # rollback-id(单步或整脚本重跑用同一个)、-RollbackSkipMySqlPause 时声明的 PITR 恢复时刻、
    # 带 blocked 玩家继续的人数(待负责人裁决 #4,回滚脚本会拒绝)。
    # switch 与这些字符串 / 整数都放在参数块末尾,不影响前面参数的位置绑定。
    [string]$RollbackRedisPasswordEnv = "",
    [string]$RollbackMySqlDsnEnv = "",
    [string]$RollbackDiscardTool = "",
    [string]$RollbackId = "",
    [string]$RollbackPitrRestoredTo = "",
    [int]$RollbackAcceptBlockedPlayers = 0,
    # 转给 k8s_zone_rollback.ps1 的 -AllowEmptyTarget / -AcknowledgeNoPostTWrites(回档复核 [11]):只在被指定时转发,语义见回滚脚本同名参数
    # (前者只用于从未落过库的新 zone;后者只用于 T 之后确实没有任何存盘的 zone,inventory / purge / verify 一并带上)。
    # 以前经本入口走不到这两条路径,只能直接调用回滚脚本。
    [switch]$RollbackAllowEmptyTarget,
    [switch]$RollbackAcknowledgeNoPostTWrites,
    [switch]$RollbackApply,
    # 集群内 login 的第三方登录(微信 / QQ),透传给 k8s_deploy.ps1 同名参数(k8s-zone-up / k8s-all-up)与 k8s-zone-rollback 的 Step 6,
    # 语义、形状校验与微信密钥(环境变量 MMORPG_LOGIN_WECHAT_APP_SECRET,不经参数)都只在 k8s_deploy.ps1 一处。字符串非空才透传,
    # switch 只在被指定时透传:默认值只留在下游。不粘滞,而且漏传有闸:线上已开的第三方登录会被本次参数关掉或改掉时,
    # k8s_deploy.ps1 在任何写操作之前拒绝(k8s-zone-rollback 由回滚脚本第 0 步在停服之前拒绝),报错给出照传现状的参数串;
    # 确要关闭或改动加 -ConfirmLoginAuthProviderChange。k8s_image.ps1 不部署 login,k8s-image-* / k8s-release-* 显式给了即拒绝。
    # 追加在参数块末尾:字符串参数按声明顺序占位置,插在中间会挪动后面参数的位置绑定。
    [string]$LoginWeChatAppId = "",
    [string]$LoginWeChatEndpoint = "",
    [switch]$LoginWeChatAllowOpenId,
    [string]$LoginQQAppId = "",
    [string]$LoginQQEndpoint = "",
    [switch]$LoginQQAllowOpenId,
    [switch]$ConfirmLoginAuthProviderChange,
    # ── merge-zone 系列的全局库 ──
    # 转给 tools/merge_zone 的 -route-db-dsn。**没有缺省值**:全局库(dev testdb / K8s mmorpg_global)的 zone_merged_into 是映射的唯一真源,
    # 三个命令都读它,缺了在调用工具之前点名拒绝。含口令,建议从环境变量取,例如 -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN,
    # 不进 shell 历史。追加在参数块末尾,理由同上(字符串参数按声明顺序占位置)。
    [string]$MergeZoneRouteDbDsn = "",
    # ── k8s-route-tool ──
    # 转给 tools/scripts/k8s_route_tool.ps1:-RouteToolArgs 原样转给它的同名参数(子命令与参数,第一项是子命令;含 '-' 开头的项
    # 请用 -Command 形式传数组);-RouteToolReportDir / -RouteToolTimeoutSeconds / -RouteToolImage 去掉 RouteTool 前缀转给
    # -ReportDir / -TimeoutSeconds / -Image,留空 / 0 不转发(默认值只在 k8s_route_tool.ps1 一处)。zone 与集群取本脚本的
    # -ZoneName / -NamespacePrefix / -KubeContext / -KubeConfig,-DryRun 同名转发。退出码原样透传(route-tool 容器的 0 / 2)。
    [string[]]$RouteToolArgs = @(),
    [string]$RouteToolReportDir = "",
    [int]$RouteToolTimeoutSeconds = 0,
    [string]$RouteToolImage = ""
)

$ErrorActionPreference = "Stop"
if ($LocalMySQL -and $Command -notin @('go-svc-start', 'go-svc-start-exe', 'go-svc-migrate', 'dev-start', 'dev-start-exe', 'dev-start-zones')) {
    throw '-LocalMySQL 只用于本地 Go 启动命令，不用于 K8s 或发布。'
}

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
# 调用方有没有显式指定发布档位 —— k8s-zone-rollback 只转发显式值("留空不覆盖",默认 dev 只留在本脚本参数块一处)。
$script:ReleaseProfileExplicit = $PSBoundParameters.ContainsKey('ReleaseProfile')

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

        # 构建开始时刻:构建成功后用它给 exe 打写入时间戳(理由见下文),必须在 go build 启动之前取。
        $buildStartUtc = [DateTime]::UtcNow
        go build -v -o $legacyProtoGenExePath ./cmd
        # go build 是外部命令,失败不会触发 $ErrorActionPreference = "Stop"。不在这里拦,下一行就会把上一次的
        # 旧 pbgen.exe 当新产物复制过去、脚本以 0 退出,dev.bat 接着用旧生成器改写代码。
        # 2026-09-20 就这样吞掉了 scene_node_service.cpp 的手写守护段。构建失败必须让整条 proto 流程停下。
        if ($LASTEXITCODE -ne 0) { throw "proto generator build failed (go build exit $LASTEXITCODE); stale pbgen.exe / proto-gen.exe left untouched, do not run proto-gen-run" }
        Copy-Item -Path $legacyProtoGenExePath -Destination $primaryProtoGenExePath -Force
        # go build 发现产物已是最新时可以不重写文件,刷新写入时间也只是它的尽力而为(忽略错误)。
        # proto-gen-run 按写入时间判定 exe 是否落后于源码(Get-ProtoGenBinaryStaleReason),所以构建成功后在这里显式打戳,
        # 保证「构建成功 ⇒ 构建开始前已落盘的源码不判旧」:否则源码只被 touch(如切分支)时,构建了也会被 -UseBinary 一直拒绝。
        # 戳用构建开始时刻而不是构建结束时刻:go build 在各包编译时才读源码,同一工作区的其他会话在构建期间保存的源码
        # 可能没进这次产物;它们的写入时间晚于构建开始,proto-gen-run 会判旧(fail-closed),不会被更晚的戳盖住。
        # 受文件系统时间戳精度限制,紧贴构建开始那一刻的保存仍可能判为不旧。
        foreach ($builtExePath in @($legacyProtoGenExePath, $primaryProtoGenExePath)) {
            [System.IO.File]::SetLastWriteTimeUtc($builtExePath, $buildStartUtc)
        }

        if (Test-Path $staleCmdExePath) {
            Remove-Item $staleCmdExePath -Force -ErrorAction SilentlyContinue
        }
    }
    finally {
        Pop-Location
    }
}

# 预编译生成器 exe 是否落后于生成器源码:落后时返回一句英文原因(哪个 exe 比哪个源码文件旧),否则返回 $null。
# 背景:批 7 起 proto/message_id.txt 有 `N=reserved:<KeyName>` 墓碑行。不认识墓碑的旧 exe 会把墓碑号当空号回收、
# 写回时丢掉墓碑行,悄悄毁掉「号码不复用」;同一工作区又有多个会话随时可能 regen,所以跑 exe 前必须先判新旧。
# 只比现存源码文件的写入时间(LastWriteTimeUtc),不比内容,两个方向都有误差:
#   - 多判:切分支等只 touch 源码的操作会误判为旧,代价只是改走 go run 或提示重建;
#   - 漏判:删除源码文件、改名(NTFS 改名保留写入时间)、保留原写入时间的复制或解压,都不会留下比 exe 新的文件,判不出来。
#     这类改动通常伴随其他源码修改;单独发生时需要手动跑 proto-gen-build。
# 源码范围 = $ProtoGenDir 下会进入二进制的输入:
#   - *.go,排除 *_test.go(测试文件不进二进制);
#   - *.tmpl:grpc_service_handler.*.tmpl、service_metadata.h.tmpl 经 go:embed 编进二进制;其余模板在运行期按相对路径读取,
#     一并计入只会多判,不会因此漏判;
#   - 根目录 go.mod / go.sum(依赖版本变化)。
# 排除的顶层目录(生成器运行期写入或读取、不进二进制;不排除的话每次 regen 后都可能误判为旧):
#   - cpp/:cmd/root.go 的 makeProjectDir 在运行期按相对工作目录创建 generated_dir="cpp/generated/";
#   - etc/:运行期读取的 yaml 配置;
#   - bin/:.gitignore 里的本地构建输出目录。
function Get-ProtoGenBinaryStaleReason {
    param([Parameter(Mandatory = $true)][string]$BinaryPath)

    $excludedTopLevelDirs = @('cpp', 'etc', 'bin')
    $topLevelFiles = @(Get-ChildItem -LiteralPath $ProtoGenDir -File -Force)
    $nestedFiles = @(Get-ChildItem -LiteralPath $ProtoGenDir -Directory -Force |
        Where-Object { $excludedTopLevelDirs -notcontains $_.Name } |
        ForEach-Object { Get-ChildItem -LiteralPath $_.FullName -Recurse -File -Force })
    $sources = @(($topLevelFiles + $nestedFiles) | Where-Object {
            ($_.Name -like '*.go' -and $_.Name -notlike '*_test.go') -or $_.Name -like '*.tmpl'
        }) + @($topLevelFiles | Where-Object { $_.Name -in @('go.mod', 'go.sum') })

    $binaryWriteTimeUtc = (Get-Item -LiteralPath $BinaryPath -Force).LastWriteTimeUtc
    $newestSource = $sources | Sort-Object -Property LastWriteTimeUtc -Descending | Select-Object -First 1
    if ($null -eq $newestSource -or $newestSource.LastWriteTimeUtc -le $binaryWriteTimeUtc) {
        return $null
    }
    return "{0} ({1:o}) is older than generator source {2} ({3:o})" -f (Split-Path -Leaf $BinaryPath), $binaryWriteTimeUtc, $newestSource.FullName, $newestSource.LastWriteTimeUtc
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
            if ($LASTEXITCODE -ne 0) { throw "协议生成器执行失败（退出码 $LASTEXITCODE）；生成未完成，停止后续构建。" }
            return
        }

        # 预编译 exe:优先 proto-gen.exe,其次兼容名 pbgen.exe;两者都没有时为 $null。
        $selectedProtoGenExePath = $null
        if (Test-Path $primaryProtoGenExePath) {
            $selectedProtoGenExePath = $primaryProtoGenExePath
        }
        elseif (Test-Path $legacyProtoGenExePath) {
            $selectedProtoGenExePath = $legacyProtoGenExePath
        }
        # 选中的 exe 落后于生成器源码时不能直接跑:旧 exe 会抹掉 message_id.txt 墓碑等新格式(见 Get-ProtoGenBinaryStaleReason)。
        $binaryStaleReason = $null
        if ($null -ne $selectedProtoGenExePath) {
            $binaryStaleReason = Get-ProtoGenBinaryStaleReason -BinaryPath $selectedProtoGenExePath
        }

        if ($UseBinary) {
            if ($null -eq $selectedProtoGenExePath) {
                throw "proto-gen binary not found. Expected $primaryProtoGenExePath or $legacyProtoGenExePath. Please run proto-gen-build first."
            }
            # 调用者显式要求跑 exe:不偷偷换成 go run,直接拒绝,让调用者先重建或显式改用 -UseGoRun。
            if ($null -ne $binaryStaleReason) {
                throw "Stale proto-gen binary refused; run proto-gen-build first or pass -UseGoRun. $binaryStaleReason."
            }
            & $selectedProtoGenExePath
            if ($LASTEXITCODE -ne 0) { throw "协议生成器执行失败（退出码 $LASTEXITCODE）；生成未完成，停止后续构建。" }
            return
        }

        # Default strategy: prefer prebuilt proto-gen binary for faster startup, fall back to the legacy name, then go run.
        # 选中的 exe 落后于源码时告警并改走 go run(按当前源码生成),不跑旧 exe。
        if ($null -ne $binaryStaleReason) {
            Write-Warning "$binaryStaleReason; running 'go run ./cmd' instead. Run proto-gen-build to refresh the binary."
            go run ./cmd
        }
        elseif ($null -ne $selectedProtoGenExePath) {
            & $selectedProtoGenExePath
        }
        else {
            go run ./cmd
        }
        # 外部命令非零退出不会自动抛异常；必须在 finally 清理之前把失败传给调用者。
        if ($LASTEXITCODE -ne 0) { throw "协议生成器执行失败（退出码 $LASTEXITCODE）；生成未完成，停止后续构建。" }
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

# 生成器不会把**新域**的 C++ 产物登记进工程文件(CMakeLists.txt / *.vcxproj / *.vcxproj.filters),也不会同步 robot/vendor。
# 新域第一次生成时漏了这两步,C++ 要到全量 msbuild 链接期才报 LNK2019,robot(-mod=vendor)直接编不过 ——
# 而做统一重生成的人未必读过那个域的设计文档。所以挂在 proto-gen-run 成功之后自动跑,不靠人记。
# 每个钩子都幂等:已登记 / 已同步时零改动;该域本次没有 C++ 产物(比如用的配置不含它)时整条跳过。
# 新增一个这样的域 = 在 $ProtoGenPostHooks 里加一项(脚本照 tools/scripts/mail_postgen.py 的子命令约定)。
$ProtoGenPostHooks = @(
    @{ Domain = "mail"; Script = "tools\scripts\mail_postgen.py"; Marker = "cpp\generated\proto\mail"; Commands = @("register-cpp", "vendor-robot") }
    # 排行榜 rank(docs/design/leaderboard-system.md §5.3):新域,同一个问题;rank_postgen.py 复用 mail_postgen.py 的登记机制。
    @{ Domain = "rank"; Script = "tools\scripts\rank_postgen.py"; Marker = "cpp\generated\proto\rank"; Commands = @("register-cpp", "vendor-robot") }
)

# 钩子用的 Python 3:与 dev.bat :export 同一顺序(PYTHON_EXE → py -3 → python)。每个候选都实跑一次,
# 能跑通且是 3.x 才算 —— WindowsApps 下的应用商店占位程序会以 9009 失败,不会被选中。返回解释器的绝对路径,找不到返回 $null。
function Resolve-ProtoGenHookPython {
    $candidates = @()
    if (-not [string]::IsNullOrWhiteSpace($env:PYTHON_EXE)) { $candidates += @{ Exe = $env:PYTHON_EXE.Trim('"'); Args = @() } }
    $candidates += @{ Exe = "py"; Args = @("-3") }
    $candidates += @{ Exe = "python"; Args = @() }
    foreach ($candidate in $candidates) {
        if ($null -eq (Get-Command $candidate.Exe -ErrorAction SilentlyContinue)) { continue }
        $probe = & $candidate.Exe @($candidate.Args) -c "import sys; sys.exit(1) if sys.version_info.major != 3 else print(sys.executable)" 2>$null
        if ($LASTEXITCODE -eq 0 -and $probe) {
            return ([string]($probe | Select-Object -Last 1)).Trim()
        }
    }
    return $null
}

function Invoke-ProtoGenPostHooks {
    foreach ($hook in $ProtoGenPostHooks) {
        $marker = Join-Path $RepoRoot $hook.Marker
        if (-not (Test-Path -LiteralPath $marker)) { continue }
        $script = Join-Path $RepoRoot $hook.Script
        if (-not (Test-Path -LiteralPath $script)) {
            throw "$($hook.Domain) 的生成后钩子脚本不存在:$script(生成物已产出,但登记 / vendor 同步无法完成)"
        }
        $python = Resolve-ProtoGenHookPython
        if ($null -eq $python) {
            throw "找不到可用的 Python 3(PYTHON_EXE / py -3 / python 都不行):$($hook.Domain) 的生成物已产出,必须运行 $script $($hook.Commands -join ' / ') 完成工程登记与 robot vendor 同步"
        }
        foreach ($subcommand in $hook.Commands) {
            Write-Host "[proto-gen post-hook] $($hook.Domain): $subcommand"
            & $python $script $subcommand
            if ($LASTEXITCODE -ne 0) {
                throw "$($hook.Domain) 的生成后钩子 $subcommand 失败(退出码 $LASTEXITCODE);生成物已产出但登记 / 同步未完成,修好后单独重跑:python $script $subcommand"
            }
        }
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

function Invoke-ThirdPartyLockCheck {
    # 仅在本函数内关闭原生命令自动抛错，确保检查器非零退出码原样返回。
    $PSNativeCommandUseErrorActionPreference = $false
    $scriptPath = Join-Path $ScriptDir "third_party\verify_release_lock.py"
    if (-not (Test-Path -LiteralPath $scriptPath -PathType Leaf)) {
        throw "第三方固定版本检查脚本不存在: $scriptPath"
    }
    $python = Resolve-ProtoGenHookPython
    if ($null -eq $python) {
        throw '找不到可用的 Python 3（PYTHON_EXE / py -3 / python）；未执行固定版本检查。'
    }
    $checkerArgs = @($scriptPath)
    if ($ApplyPatches) { $checkerArgs += '--apply-patches' }
    & $python @checkerArgs
    # 直接保留校验器退出码，不让包装入口将失败伪装成成功。
    exit $LASTEXITCODE
}

function Invoke-ThirdPartyZlibBuild {
    $scriptPath = Join-Path $ScriptDir "third_party\build_zlib.ps1"
    if (-not (Test-Path -LiteralPath $scriptPath -PathType Leaf)) {
        throw "第三方 zlib 构建脚本不存在: $scriptPath"
    }
    $bound = $script:PSBoundParameters
    $buildArgs = @{}
    if (-not [string]::IsNullOrWhiteSpace($BuildRoot)) {
        $buildArgs.BuildRoot = $BuildRoot
    }
    if ($bound.ContainsKey('Jobs')) {
        $buildArgs.Jobs = $Jobs
    }
    & $scriptPath @buildArgs
    exit $LASTEXITCODE
}

function Invoke-ThirdPartySdkBuild {
    param([ValidateSet('protobuf', 'boringssl')][string]$Sdk)
    $scriptPath = Join-Path $ScriptDir "third_party/build_$Sdk.ps1"
    if (-not (Test-Path -LiteralPath $scriptPath -PathType Leaf)) {
        throw "第三方 SDK 构建脚本不存在: $scriptPath"
    }
    $bound = $script:PSBoundParameters
    $buildArgs = @{
        Configuration = $Configuration
        Architecture = $Architecture
        PreferredMsvcVersion = $PreferredMsvcVersion
        Clean = $Clean
        SkipToolCheck = $SkipToolCheck
        UseVSGenerator = $UseVSGenerator
    }
    if ($bound.ContainsKey('Jobs')) { $buildArgs.Jobs = $Jobs }
    if ($BuildRoot) { $buildArgs.BuildRoot = $BuildRoot }
    if ($Sdk -eq 'protobuf' -and $ZlibInstallRoot) { $buildArgs.ZlibInstallRoot = $ZlibInstallRoot }
    & $scriptPath @buildArgs
    exit $LASTEXITCODE
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
        GateServicePort = $GateServicePort
        WaitTimeoutSeconds = $WaitTimeoutSeconds
    }

    if (-not [string]::IsNullOrWhiteSpace($ZonesConfigPath)) {
        $args.ZonesConfigPath = $ZonesConfigPath
    }

    # 留空不传,由 k8s_deploy.ps1 的默认值(NodePort)接管;OpsProfile 为 managed-cloud / bare-metal 时下游还会强制改写。
    if (-not [string]::IsNullOrWhiteSpace($GateServiceType)) {
        $args.GateServiceType = $GateServiceType
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

    # 同上:switch 只在被指定时传,字符串非空才传。透传空的 CppLogSidecarImage
    # 会让生成的清单出现空 image:,Pod 直接建不起来。
    if ($NoCppLogSidecar) {
        $args.NoCppLogSidecar = $true
    }
    if (-not [string]::IsNullOrWhiteSpace($CppLogSidecarImage)) {
        $args.CppLogSidecarImage = $CppLogSidecarImage
    }
    if (-not [string]::IsNullOrWhiteSpace($LokiPushUrl)) {
        $args.LokiPushUrl = $LokiPushUrl
    }
    # 同上:留空不传,由 k8s_deploy.ps1 的默认值("1")接管;只有显式给了才覆盖。
    if (-not [string]::IsNullOrWhiteSpace($GateRouterMode)) {
        $args.GateRouterMode = $GateRouterMode
    }
    # 同上:留空不传,k8s_deploy.ps1 留空即不写 NODE_OPEN_FILE_LIMIT_MODE(进程按 enforce)。
    if (-not [string]::IsNullOrWhiteSpace($NodeOpenFileLimitMode)) {
        $args.NodeOpenFileLimitMode = $NodeOpenFileLimitMode
    }
    # 集群外入口参数同一口径:字符串非空才传,GateNodePortBase 非 -1 才传,switch 只在被指定时传。
    # 不抽成共用函数:契约测试按 AST 单独取出本函数执行,依赖外部函数会让它找不到命令。
    if (-not [string]::IsNullOrWhiteSpace($ClientEntryMode)) { $args.ClientEntryMode = $ClientEntryMode }
    if (-not [string]::IsNullOrWhiteSpace($ClientPublicHost)) { $args.ClientPublicHost = $ClientPublicHost }
    if (-not [string]::IsNullOrWhiteSpace($GateClientHostTemplate)) { $args.GateClientHostTemplate = $GateClientHostTemplate }
    if ($GateNodePortBase -ne -1) { $args.GateNodePortBase = $GateNodePortBase }
    if (-not [string]::IsNullOrWhiteSpace($GateExternalTrafficPolicy)) { $args.GateExternalTrafficPolicy = $GateExternalTrafficPolicy }
    if (-not [string]::IsNullOrWhiteSpace($BattleOrchestrator)) { $args.BattleOrchestrator = $BattleOrchestrator }
    if (-not [string]::IsNullOrWhiteSpace($GatewayIngressHost)) { $args.GatewayIngressHost = $GatewayIngressHost }
    if (-not [string]::IsNullOrWhiteSpace($GatewayIngressClassName)) { $args.GatewayIngressClassName = $GatewayIngressClassName }
    if (-not [string]::IsNullOrWhiteSpace($GatewayIngressTlsSecret)) { $args.GatewayIngressTlsSecret = $GatewayIngressTlsSecret }
    if (-not [string]::IsNullOrWhiteSpace($GatewayTrustedProxies)) { $args.GatewayTrustedProxies = $GatewayTrustedProxies }
    if ($LoginDevPasswordAuth) { $args.LoginDevPasswordAuth = $true }
    # login 第三方登录(微信 / QQ)同一口径:字符串非空才传,switch 只在被指定时传。漏传的防误关闸在 k8s_deploy.ps1
    # (Assert-LoginAuthProviderClusterState,非 DryRun、任何写操作之前),本函数只透传,不复制规则。
    if (-not [string]::IsNullOrWhiteSpace($LoginWeChatAppId)) { $args.LoginWeChatAppId = $LoginWeChatAppId }
    if (-not [string]::IsNullOrWhiteSpace($LoginWeChatEndpoint)) { $args.LoginWeChatEndpoint = $LoginWeChatEndpoint }
    if ($LoginWeChatAllowOpenId) { $args.LoginWeChatAllowOpenId = $true }
    if (-not [string]::IsNullOrWhiteSpace($LoginQQAppId)) { $args.LoginQQAppId = $LoginQQAppId }
    if (-not [string]::IsNullOrWhiteSpace($LoginQQEndpoint)) { $args.LoginQQEndpoint = $LoginQQEndpoint }
    if ($LoginQQAllowOpenId) { $args.LoginQQAllowOpenId = $true }
    if ($ConfirmLoginAuthProviderChange) { $args.ConfirmLoginAuthProviderChange = $true }
    if (-not [string]::IsNullOrWhiteSpace($RequireClientEndpoint)) { $args.RequireClientEndpoint = $RequireClientEndpoint }
    if ($AllowDisruptiveSwitch) { $args.AllowDisruptiveSwitch = $true }
    # 共享 Redis 重启闸的确认(P0-E),同一口径:switch 只在被指定时传。
    if ($ConfirmRedisCoreRestart) { $args.ConfirmRedisCoreRestart = $true }
    # 共享 Redis 的形态:非 0 才传(0 = k8s_deploy.ps1 按档位取)。
    if ($RedisReplicas -gt 0) { $args.RedisReplicas = $RedisReplicas }
    # 共享 Redis 按用途拆实例(G4a 第 1a 批):-RedisSplit 非 auto 才传(auto 是 k8s_deploy.ps1 的缺省),确认开关只在被指定时传,
    # 容量非空才传。防静默切换的闸只在 k8s_deploy.ps1(Assert-RedisPurposeSwitchIsSafe),本函数只透传。
    if (-not [string]::IsNullOrWhiteSpace($RedisSplit) -and $RedisSplit -ne 'auto') { $args.RedisSplit = $RedisSplit }
    if ($ConfirmRedisSplitSwitch) { $args.ConfirmRedisSplitSwitch = $true }
    if (-not [string]::IsNullOrWhiteSpace($RedisQueueMaxMemory)) { $args.RedisQueueMaxMemory = $RedisQueueMaxMemory }
    # k8s-redis-wipe 的确认,同一口径。准入(namespace 规则 + 确认)只在 k8s_deploy.ps1 的 Invoke-RedisWipe 判。
    if ($ConfirmWipeAllRedis) { $args.ConfirmWipeAllRedis = $true }
    # 玩家域 Redis Cluster:非 0 / 非空才传。档位规则与变更闸只在 k8s_deploy.ps1(Assert-RedisPlayerPreflight)。
    if ($PlayerRedisMasters -gt 0) { $args.PlayerRedisMasters = $PlayerRedisMasters }
    if ($PlayerRedisReplicasPerMaster -gt 0) { $args.PlayerRedisReplicasPerMaster = $PlayerRedisReplicasPerMaster }
    if (-not [string]::IsNullOrWhiteSpace($PlayerRedisMaxMemory)) { $args.PlayerRedisMaxMemory = $PlayerRedisMaxMemory }
    if (-not [string]::IsNullOrWhiteSpace($PlayerRedisStorage)) { $args.PlayerRedisStorage = $PlayerRedisStorage }

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

    # k8s_image.ps1 只发布 C++ 节点镜像:它不传 -GoSvcRegistry / -JavaSvcRegistry(k8s_deploy.ps1 因此跳过 Go / Java 服务),
    # 也不声明 Agones 高密度 / FleetAutoscaler 参数。下面这些参数经本入口必然不生效,显式给了就在任何副作用之前拒绝,
    # 不静默吞掉(AGENTS §11.3);要用它们请走 k8s-infra-up / k8s-zone-up / k8s-all-up。
    # AgonesRoomCapacity / AgonesBufferRooms / AgonesMinReplicas / AgonesMaxReplicas 只在两个开关打开时才被 k8s_deploy.ps1
    # 读取,拦住开关即可。存量缺陷(31e4d1d4c 起):这四个值曾被无条件 splat 给 k8s_image.ps1,而它从未声明过它们
    # (advanced script 遇到未知具名参数直接报错),k8s-image-preflight / k8s-build-image / k8s-push-image /
    # k8s-release-zone / k8s-release-all 因此全部在参数绑定阶段失败;现已从下面的哈希表里去掉。
    # -RequireClientEndpoint 不在此列:k8s_image.ps1 声明并透传它(只参与 k8s_deploy.ps1 的组合预检、不改写 login /
    # scene_manager 的 ConfigMap,给了由 k8s_image.ps1 告警说明),见下方透传处。
    $notForwarded = @()
    if ($AgonesHighDensity) { $notForwarded += '-AgonesHighDensity' }
    if ($AgonesAutoscale) { $notForwarded += '-AgonesAutoscale' }
    if ($LoginDevPasswordAuth) { $notForwarded += '-LoginDevPasswordAuth' }
    if (-not [string]::IsNullOrWhiteSpace($GatewayIngressHost)) { $notForwarded += '-GatewayIngressHost' }
    if (-not [string]::IsNullOrWhiteSpace($GatewayIngressClassName)) { $notForwarded += '-GatewayIngressClassName' }
    if (-not [string]::IsNullOrWhiteSpace($GatewayIngressTlsSecret)) { $notForwarded += '-GatewayIngressTlsSecret' }
    if (-not [string]::IsNullOrWhiteSpace($GatewayTrustedProxies)) { $notForwarded += '-GatewayTrustedProxies' }
    # login 第三方登录(微信 / QQ)与它的确认开关同 -LoginDevPasswordAuth:只落在 login 上,k8s_image.ps1 不部署 login、也不声明它们。
    # 经这条路发布反过来也关不掉线上的第三方登录(不重写 login ConfigMap、不碰 Secret login-oauth),不需要防误关。
    if (-not [string]::IsNullOrWhiteSpace($LoginWeChatAppId)) { $notForwarded += '-LoginWeChatAppId' }
    if (-not [string]::IsNullOrWhiteSpace($LoginWeChatEndpoint)) { $notForwarded += '-LoginWeChatEndpoint' }
    if ($LoginWeChatAllowOpenId) { $notForwarded += '-LoginWeChatAllowOpenId' }
    if (-not [string]::IsNullOrWhiteSpace($LoginQQAppId)) { $notForwarded += '-LoginQQAppId' }
    if (-not [string]::IsNullOrWhiteSpace($LoginQQEndpoint)) { $notForwarded += '-LoginQQEndpoint' }
    if ($LoginQQAllowOpenId) { $notForwarded += '-LoginQQAllowOpenId' }
    if ($ConfirmLoginAuthProviderChange) { $notForwarded += '-ConfirmLoginAuthProviderChange' }
    if ($notForwarded.Count -gt 0) {
        throw "k8s-image-* / k8s-release-* 不支持 $($notForwarded -join ', '):k8s_image.ps1 不部署 Go / Java 服务,也不支持 Agones 高密度 / FleetAutoscaler。请改用 k8s-infra-up / k8s-zone-up / k8s-all-up(直接调 k8s_deploy.ps1)。"
    }

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
        GateServicePort = $GateServicePort
        WaitTimeoutSeconds = $WaitTimeoutSeconds
    }

    if (-not [string]::IsNullOrWhiteSpace($ZonesConfigPath)) {
        $args.ZonesConfigPath = $ZonesConfigPath
    }
    # 留空不传,k8s_image.ps1 同样留空不传,最终由 k8s_deploy.ps1 的默认值接管(见参数注释)。
    if (-not [string]::IsNullOrWhiteSpace($GateServiceType)) {
        $args.GateServiceType = $GateServiceType
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
    # k8s_image.ps1 最终也会调 k8s_deploy.ps1,同一套口径透传(见 Invoke-K8sDeploy 处注释)。
    if ($NoCppLogSidecar) {
        $args.NoCppLogSidecar = $true
    }
    if (-not [string]::IsNullOrWhiteSpace($CppLogSidecarImage)) {
        $args.CppLogSidecarImage = $CppLogSidecarImage
    }
    if (-not [string]::IsNullOrWhiteSpace($LokiPushUrl)) {
        $args.LokiPushUrl = $LokiPushUrl
    }
    if (-not [string]::IsNullOrWhiteSpace($GateRouterMode)) {
        $args.GateRouterMode = $GateRouterMode
    }
    # B0-4 豁免开关同一口径:留空不传,k8s_image.ps1 同样留空不传,最终由 k8s_deploy.ps1 接管。
    if (-not [string]::IsNullOrWhiteSpace($NodeOpenFileLimitMode)) {
        $args.NodeOpenFileLimitMode = $NodeOpenFileLimitMode
    }
    # 集群外入口参数,口径同 Invoke-K8sDeploy(k8s_image.ps1 原样转给 k8s_deploy.ps1)。
    # -Gateway* / -LoginDevPasswordAuth / 第三方登录的 -Login* 与 -ConfirmLoginAuthProviderChange 不在此列,已在函数开头拒绝
    # (k8s_image.ps1 也不声明它们)。
    if (-not [string]::IsNullOrWhiteSpace($ClientEntryMode)) { $args.ClientEntryMode = $ClientEntryMode }
    if (-not [string]::IsNullOrWhiteSpace($ClientPublicHost)) { $args.ClientPublicHost = $ClientPublicHost }
    if (-not [string]::IsNullOrWhiteSpace($GateClientHostTemplate)) { $args.GateClientHostTemplate = $GateClientHostTemplate }
    if ($GateNodePortBase -ne -1) { $args.GateNodePortBase = $GateNodePortBase }
    if (-not [string]::IsNullOrWhiteSpace($GateExternalTrafficPolicy)) { $args.GateExternalTrafficPolicy = $GateExternalTrafficPolicy }
    if (-not [string]::IsNullOrWhiteSpace($BattleOrchestrator)) { $args.BattleOrchestrator = $BattleOrchestrator }
    # release-zone / release-all 同样重新生成 gate(release-all 还有 battle),切换形态的确认闸同一口径透传。
    if ($AllowDisruptiveSwitch) { $args.AllowDisruptiveSwitch = $true }
    # release-all(不带 -SkipInfra)会 infra-up,共享 Redis 重启闸的确认同一口径透传。
    if ($ConfirmRedisCoreRestart) { $args.ConfirmRedisCoreRestart = $true }
    # 共享 Redis 的形态同一口径:非 0 才传。
    if ($RedisReplicas -gt 0) { $args.RedisReplicas = $RedisReplicas }
    # 按用途拆实例(G4a 第 1a 批)同一口径:-RedisSplit 非 auto 才传,确认开关只在被指定时传,容量非空才传。k8s_image.ps1 不部署
    # login / gateway,只有 release-all(不带 -SkipInfra)的 infra-up 会按 -RedisSplit queue apply 排队实例。
    if (-not [string]::IsNullOrWhiteSpace($RedisSplit) -and $RedisSplit -ne 'auto') { $args.RedisSplit = $RedisSplit }
    if ($ConfirmRedisSplitSwitch) { $args.ConfirmRedisSplitSwitch = $true }
    if (-not [string]::IsNullOrWhiteSpace($RedisQueueMaxMemory)) { $args.RedisQueueMaxMemory = $RedisQueueMaxMemory }
    # 玩家域 Redis Cluster 同一口径:scene 的 player_redis 由它决定,staging / prod 发版必须给主节点数。
    if ($PlayerRedisMasters -gt 0) { $args.PlayerRedisMasters = $PlayerRedisMasters }
    if ($PlayerRedisReplicasPerMaster -gt 0) { $args.PlayerRedisReplicasPerMaster = $PlayerRedisReplicasPerMaster }
    if (-not [string]::IsNullOrWhiteSpace($PlayerRedisMaxMemory)) { $args.PlayerRedisMaxMemory = $PlayerRedisMaxMemory }
    if (-not [string]::IsNullOrWhiteSpace($PlayerRedisStorage)) { $args.PlayerRedisStorage = $PlayerRedisStorage }
    # 留空不透传;k8s_image.ps1 不部署 login / scene_manager,给了由它告警"只参与预检、不改写 ConfigMap"。
    if (-not [string]::IsNullOrWhiteSpace($RequireClientEndpoint)) { $args.RequireClientEndpoint = $RequireClientEndpoint }
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

# Get-MergeRouteDbArgs 组装全局库的 flag:三个命令都要读它,-route-db-dsn 缺了工具必然拒绝,这里先点名。
# DSN 含口令:只进参数数组,不 Write-Host。返回数组,调用方用 @(...) 接。
function Get-MergeRouteDbArgs {
    param([Parameter(Mandatory = $true)][string]$MergeCommand)
    if ($MergeZoneRouteDbDsn -eq "") {
        throw ("$MergeCommand requires -MergeZoneRouteDbDsn <DSN of the global DB that holds zone_merged_into " +
               "(dev: testdb, K8s: mmorpg_global)>; there is no default. Pass it from an environment variable, e.g. -MergeZoneRouteDbDsn `$env:MERGE_ZONE_ROUTE_DB_DSN")
    }
    return @("-route-db-dsn", $MergeZoneRouteDbDsn)
}

# Get-MergeZoneCommandArgs 返回某个 merge-zone 系列命令传给工具的完整参数。
# 与执行分开,是为了让契约测试(tests/dev_tools_merge_zone_contract.tests.ps1)不起 go、不连任何库就能钉住「哪个命令转发哪些 flag」:
# 多转一个工具已删的 flag,整条命令会被 flag 包拒绝(契约测试逐个核对 main.go 的声明)。
# 这里只做「缺了必然失败」的那几条校验,给出点名参数的报错;其余判定(映射冲突、成环)都在工具里,不在 ps1 复制第二份。
function Get-MergeZoneCommandArgs {
    param([Parameter(Mandatory = $true)][string]$MergeCommand)
    switch ($MergeCommand) {
        { $_ -in @("merge-zone", "merge-zone-unmerge") } {
            if ($MergeSourceZone -le 0 -or $MergeTargetZone -le 0) {
                throw "$MergeCommand requires -MergeSourceZone <id> and -MergeTargetZone <id>"
            }
            $mode = if ($MergeCommand -eq "merge-zone") { "merge" } else { "unmerge" }
            $a = @("-mode", $mode, "-source-zone", $MergeSourceZone, "-target-zone", $MergeTargetZone)
            $a += @(Get-MergeRouteDbArgs -MergeCommand $MergeCommand)
            if ($DryRun) { $a += "-dry-run" }
            return $a
        }
        "merge-zone-audit" {
            return @("-mode", "audit") + @(Get-MergeRouteDbArgs -MergeCommand $MergeCommand)
        }
        default { throw "Get-MergeZoneCommandArgs: unknown merge command '$MergeCommand'" }
    }
}

# New-MergeZoneExePath 给一次运行的 merge_zone 编译产物一个独占的临时路径:两个合服命令并发时
# 不会互相覆盖 / 删掉对方正在跑的二进制。单独成函数也是契约测试的替身接缝(换成假工具,不必真编译)。
function New-MergeZoneExePath {
    $name = "merge_zone_{0}" -f [guid]::NewGuid().ToString('N')
    if ($IsWindows) { $name += ".exe" }
    return Join-Path ([IO.Path]::GetTempPath()) $name
}

# Invoke-MergeZoneGo 在 tools/merge_zone 里编译并执行一个 merge-zone 系列命令,**以工具自己的退出码结束当前脚本(不返回)**。
# tools/merge_zone 是独立 go module,必须在那个目录里编译。
#
# 退出码为什么要这样透传(工具:0 完成 / 1 拒绝,没有写 / 2 没做成或结局未知):
#   1) 不能用 `go run`:它把子进程的任何非零退出码都压成 1,拒绝和结局未知从此分不开。所以先 build 再直接执行产物。
#   2) 必须显式 `exit`:`pwsh -File dev_tools.ps1 ...` 在脚本正常结束时进程退出码恒为 0,写失败了调用方拿到的也是「成功」。
#   3) 编译失败时工具一行都没跑,退出码按 2 给出。
# 不经管道接工具输出:管道会让 pwsh 按控制台代码页重解码 Go 写出的 UTF-8。
function Invoke-MergeZoneGo {
    param([Parameter(Mandatory = $true)][string]$MergeCommand)
    $repoRoot = (Resolve-Path (Join-Path $ScriptDir "..\..")).Path
    # 参数错误往上抛(pwsh -File 下为 1 = 拒绝、什么都没做),不编译、不运行工具。
    $toolArgs = @(Get-MergeZoneCommandArgs -MergeCommand $MergeCommand)
    # 工具非零退出是结论,不是脚本异常;防 profile 里打开了这个开关把它变成 throw(恒为 1)。
    $PSNativeCommandUseErrorActionPreference = $false
    $exe = New-MergeZoneExePath
    $code = 2
    Push-Location (Join-Path $repoRoot "tools\merge_zone")
    try {
        & go build -o $exe .
        if ($LASTEXITCODE -eq 0) {
            & $exe @toolArgs
            $code = $LASTEXITCODE
        }
        else {
            [Console]::Error.WriteLine("ERROR: merge_zone build failed (go build exit $LASTEXITCODE); the tool was NOT run, nothing was checked or written")
        }
    }
    finally {
        Pop-Location
        Remove-Item -LiteralPath $exe -Force -ErrorAction SilentlyContinue
    }
    exit $code
}

# Get-K8sRouteToolArgs 组装 tools/scripts/k8s_route_tool.ps1 的 hashtable 参数。
# 必须 hashtable splatting:数组 splatting 按位置绑定(与 k8s-zone-rollback 同一个坑)。参数名与 k8s_route_tool.ps1 的 param 块
# 逐字相同(契约测试按 AST 钉住);留空 / 0 / 未指定的不放,默认值只在 k8s_route_tool.ps1 一处。子命令与参数的形状
# (第一项是已知子命令、不许带 -f / -route-tool 等)由 k8s_route_tool.ps1 的入口校验判,这里不复制规则。
function Get-K8sRouteToolArgs {
    $rtArgs = @{
        ZoneName        = $ZoneName
        RouteToolArgs   = @($RouteToolArgs)
        NamespacePrefix = $NamespacePrefix
    }
    if ($RouteToolReportDir -ne "")                    { $rtArgs.ReportDir = $RouteToolReportDir }
    if ($RouteToolTimeoutSeconds -gt 0)                { $rtArgs.TimeoutSeconds = $RouteToolTimeoutSeconds }
    if ($RouteToolImage -ne "")                        { $rtArgs.Image = $RouteToolImage }
    if ($DryRun)                                       { $rtArgs.DryRun = $true }
    if (-not [string]::IsNullOrWhiteSpace($KubeContext)) { $rtArgs.KubeContext = $KubeContext }
    if (-not [string]::IsNullOrWhiteSpace($KubeConfig))  { $rtArgs.KubeConfig = $KubeConfig }
    return $rtArgs
}

# Invoke-K8sRouteTool 调 k8s_route_tool.ps1 并**以它的退出码结束当前脚本**:route-tool 容器的 0 完成 / 2 失败,
# 必须原样到达调用方(`pwsh -File dev_tools.ps1 ...` 在脚本正常结束时进程退出码恒为 0,见 Invoke-MergeZoneGo)。
# 本入口自己的失败(没给子命令、k8s_route_tool.ps1 参数绑定失败、未捕获的异常)一律 2,与 k8s_route_tool.ps1「本脚本自己失败一律 2」
# 同口径:异常往上冒时 pwsh -File 的退出码是 1,route-tool 没有这个码(review B3-7 / OPS-5)。
function Invoke-K8sRouteTool {
    try {
        if (@($RouteToolArgs).Count -eq 0) {
            throw "k8s-route-tool requires -RouteToolArgs <subcommand and its arguments>, e.g. -RouteToolArgs @('tidb-prepare', '-zones=1,2') (use the -Command form of pwsh for arrays)"
        }
        $rtArgs = Get-K8sRouteToolArgs
        $global:LASTEXITCODE = 0
        & (Join-Path $ScriptDir "k8s_route_tool.ps1") @rtArgs
    }
    catch {
        [Console]::Error.WriteLine("ERROR: k8s-route-tool: $($_.Exception.Message) (exit 2)")
        exit 2
    }
    exit $LASTEXITCODE
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
    -UseBinary     Force running proto-gen binary (proto-gen.exe, fallback: pbgen.exe);
                   refuses to run when the binary is older than the generator sources (run proto-gen-build first)
    -UseGoRun      Force running go run ./cmd

Go micro-service commands (local dev):
    -Command go-svc-start [-GoServices login,db,...] [-GoCounts @{login=2;scene_manager=2}] [-GoPortStride N] [-LocalMySQL]
    -Command go-svc-start-exe [-GoServices login,db,...] [-GoCounts @{login=2}] [-GoPortStride N] [-LocalMySQL]
    -Command go-svc-stop  [-GoServices login,...]
    -Command go-svc-status
    -Command go-svc-list
    -Command go-svc-migrate -LocalMySQL [-GoServices match,guild]
        本机 MySQL 档为 match / guild 建表后退出;两边启动时互相核对对方的表,须先于常驻进程
    -Command sql-boot-catalog-local [-GoServices db,data_service,...]
        Build the local writer boot catalog (loopback database only): schemas, per-instance SQL accounts and
        single-use grants under run/secrets/sql-boot-local. start_game.ps1 runs it on every start. See tools/scripts/README.md.

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
    -Command third-party-lock-check [-ApplyPatches]
        默认只读校验 third_party/releases.lock.json；显式 -ApplyPatches 时恢复登记补丁。
    -Command third-party-zlib-build [-BuildRoot <全新目录>] [-Jobs 1]
        隔离构建 Debug/MDd zlib，必须通过上游 ctest 后才安装。
    -Command third-party-protobuf-build [-Configuration Debug|Release] [-Architecture x64|x86] [-BuildRoot <目录>] [-ZlibInstallRoot <zlib安装目录>]
    -Command third-party-boringssl-build [-Configuration Debug|Release] [-Architecture x64|x86] [-BuildRoot <目录>]
    -Command no-raw-pointer-setup [-LlvmRoot <path>] [-DownloadOnly] [-DryRun]
        自动下载 LLVM/Clang 开发库并串行编译裸指针成员检查器。
    -Command iwyu-run
    -Command k8s-build-all
        Build all Docker images (C++ node + Go services + Java auth) before deployment.
    -Command k8s-infra-up | k8s-infra-down | k8s-infra-status
        Manage shared infrastructure (etcd/redis/kafka/mysql) in the mmorpg-infra namespace.
    -Command k8s-redis-split-status | k8s-redis-wipe -ConfirmWipeAllRedis
        Shared Redis split by purpose (G4a batch 1a, -RedisSplit none|queue, forwarded to k8s_deploy.ps1 with
        -ConfirmRedisSplitSwitch / -RedisQueueMaxMemory): split-status prints each zone's login / gateway queue Redis
        address and rollout state (exit 1 on mismatch or unfinished rollout); wipe flushes every Redis instance of a
        stress environment, every primary of the player cluster included, and checks DBSIZE=0
        (-InfraNamespace must match ^mmorpg-stress-). See deploy/k8s/README.md.
    -Command redis-wipe-local -ConfirmWipeAllRedis
        Flush the local compose Redis and every primary of the local player cluster (deploy/compose.redis-cluster.yml)
        when it is running, then check DBSIZE=0 on each. The cleanup step of AGENTS.md section 6.2 on this machine.
    -Command k8s-zone-up | k8s-zone-down | k8s-zone-status
    -Command k8s-all-up | k8s-all-down | k8s-all-status
        Global Go services other than client-rpc-router start after the first zone's data-service
        (their startup reads tables it creates, or data_service itself). k8s-all-up does this in
        order; on a fresh cluster run k8s-infra-up, k8s-zone-up, then k8s-infra-up again.
    -Command k8s-exposure-preflight
    -Command k8s-stage-runtime
    -Command k8s-image-preflight | k8s-build-image | k8s-push-image | k8s-release-zone | k8s-release-all
    -Command k8s-route-tool -ZoneName <zone> -RouteToolArgs @('<subcommand>', ...) [-RouteToolTimeoutSeconds <s>]
            [-RouteToolImage <image>] [-RouteToolReportDir <dir>] [-DryRun] [-KubeContext <ctx>]
        Run data_service's route-tool (tidb-prepare / freeze / unfreeze) as a one-off K8s Job via
        tools/scripts/k8s_route_tool.ps1; exits with the container's code (0 done / 2 failed).

Zone merge (合服) commands, online, no downtime — full SOP: docs/ops/merge-zone-runbook.md
    A merge writes one row to zone_merged_into and moves no data. Every merge-zone* command
    REQUIRES -MergeZoneRouteDbDsn <global DB DSN> (pass it from an env var; no default) and
    exits with merge_zone's own code: 0 done (including idempotent), 1 refused and nothing
    written, 2 not done or outcome unknown (re-running is safe). If merge_zone fails to build,
    nothing runs and the exit code is 2. A real merge or unmerge also needs
    MMORPG_SQL_OPERATOR_PRINCIPAL; set MMORPG_SQL_BACKEND=mysql on the mysql tier.
    -Command merge-zone -MergeSourceZone <s> -MergeTargetZone <d> [-DryRun]
        Record <s> -> <d>. Refuses a merged-away target, a source merged elsewhere, and any
        mapping that would cycle or exceed the hop limit. -DryRun validates and prints only.
    -Command merge-zone-unmerge -MergeSourceZone <s> -MergeTargetZone <d> [-DryRun]
        Delete <s> -> <d>. A missing row is success. Any hop can be undone in any order.
    -Command merge-zone-audit
        Read-only: list every mapping and the final zone of each merged zone.

Proto-gen naming docs:
    tools/docs/proto_gen_naming_audit.md
    tools/docs/proto_gen_naming_migration.md
"@ | Write-Output
}

switch ($Command) {
    "help" { Invoke-Help }
    "pbgen-build" { Invoke-ProtoGenBuild }
    "pbgen-run" { Invoke-ProtoGenRun; Invoke-ProtoGenPostHooks }
    "proto-gen-build" { Invoke-ProtoGenBuild }
    "proto-gen-run" { Invoke-ProtoGenRun; Invoke-ProtoGenPostHooks }
    "tree" { Invoke-Tree }
    "naming-audit" { Invoke-NamingAudit }
    "naming-apply" { Invoke-NamingApply }
    "third-party-lock-check" { Invoke-ThirdPartyLockCheck }
    "third-party-zlib-build" { Invoke-ThirdPartyZlibBuild }
    "third-party-protobuf-build" { Invoke-ThirdPartySdkBuild -Sdk protobuf }
    "third-party-boringssl-build" { Invoke-ThirdPartySdkBuild -Sdk boringssl }
    "no-raw-pointer-setup" {
        & (Join-Path $ScriptDir "third_party/setup_no_raw_pointer_check.ps1") -LlvmRoot $LlvmRoot -DownloadOnly:$DownloadOnly -DryRun:$DryRun
    }
    "iwyu-run"              { Invoke-IwyuRun }
    "k8s-infra-up" { Invoke-K8sDeploy -K8sCommand "infra-up" }
    "k8s-infra-down" { Invoke-K8sDeploy -K8sCommand "infra-down" }
    "k8s-infra-status" { Invoke-K8sDeploy -K8sCommand "infra-status" }
    # 共享 Redis 按用途拆实例(G4a 第 1a 批)的两个运维命令。k8s_deploy.ps1 以 exit 1 报「不一致 / 没清干净」,不 throw,
    # 所以这里把退出码原样带出来(先清零再判,同 kafka-offset-reset 的处理),否则调用方拿到的永远是 0。
    "k8s-redis-split-status" { $global:LASTEXITCODE = 0; Invoke-K8sDeploy -K8sCommand "redis-split-status"; exit $LASTEXITCODE }
    "k8s-redis-wipe" { $global:LASTEXITCODE = 0; Invoke-K8sDeploy -K8sCommand "redis-wipe"; exit $LASTEXITCODE }
    "redis-wipe-local" {
        if (-not $ConfirmWipeAllRedis) { throw "redis-wipe-local flushes the local Redis and the local player cluster; pass -ConfirmWipeAllRedis (AGENTS.md section 6.2)." }
        . (Join-Path $PSScriptRoot 'lib/redis_local_flush.ps1')
        Invoke-LocalRedisFlushAll -WriteLine { param($line) Write-Host $line }
        exit 0
    }
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
    "k8s-route-tool" { Invoke-K8sRouteTool }
    "go-svc-start"  { & (Join-Path $ScriptDir "go_services.ps1") -Command start  -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds -Zone $Zone -ZonePortShift $ZonePortShift -LocalMySQL:$LocalMySQL }
    "go-svc-start-exe"  { & (Join-Path $ScriptDir "go_services.ps1") -Command start-exe  -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds -Zone $Zone -ZonePortShift $ZonePortShift -LocalMySQL:$LocalMySQL }
    "go-svc-stop"   { & (Join-Path $ScriptDir "go_services.ps1") -Command stop   -Services $GoServices }
    "go-svc-status" { & (Join-Path $ScriptDir "go_services.ps1") -Command status }
    "go-svc-list"   { & (Join-Path $ScriptDir "go_services.ps1") -Command list   }
    "go-svc-build"        { & (Join-Path $ScriptDir "go_services.ps1") -Command build  -Services $GoServices }
    "go-svc-migrate"      { & (Join-Path $ScriptDir "go_services.ps1") -Command migrate -Services $GoServices -LocalMySQL:$LocalMySQL }
    "sql-boot-catalog-local" { & (Join-Path $ScriptDir "lib\sql_boot_catalog_local.ps1") -Services $GoServices }
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
        & (Join-Path $ScriptDir "go_services.ps1") -Command start -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds -LocalMySQL:$LocalMySQL
        Write-Host "`n=== Starting C++ nodes ==="  -ForegroundColor Cyan
        & (Join-Path $ScriptDir "cpp_nodes.ps1") -Command start -Nodes $CppNodes -GateCount $GateCount -SceneCount $SceneCount -BattleCount $BattleCount -NodeIp $NodeIp
    }
    "dev-start-exe" {
        Write-Host "=== Starting Go services (exe) ===" -ForegroundColor Cyan
        & (Join-Path $ScriptDir "go_services.ps1") -Command start-exe -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds -LocalMySQL:$LocalMySQL
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
            & (Join-Path $ScriptDir "go_services.ps1") -Command start-exe -Services $GoServices -Counts $GoCounts -PortStride $GoPortStride -NoTier:$NoTier -TierReadySeconds $TierReadySeconds -Zone $z -ZonePortShift $ZonePortShift -LocalMySQL:$LocalMySQL
            Write-Host "`n>>> Zone ${z}: starting C++ nodes ..." -ForegroundColor Cyan
            # battle 是全局池(非 zone-scoped;gate 不连 battle,match 按 etcd 发现它,客户端按分配下发的地址直连,turn-based §22 D66),
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
        Invoke-MergeZoneGo -MergeCommand "merge-zone"
    }
    "merge-zone-unmerge" {
        Invoke-MergeZoneGo -MergeCommand "merge-zone-unmerge"
    }
    "merge-zone-audit" {
        Invoke-MergeZoneGo -MergeCommand "merge-zone-audit"
    }
    "kafka-offset-reset" {
        # Wraps kafka_offset_reset.ps1:仍在用 Kafka 的消费组的通用位点重置工具。
        # 必须 hashtable splatting:数组 splatting 按位置绑定,"-Topic" 后面的 topic 名会撞上 [int]$Partitions 直接类型转换失败。
        # 字符串非空才放,switch 只在被指定时放,数值只在偏离缺省时放。
        $kArgs = @{
            BootstrapServer = $KafkaBootstrapServer
            Topic           = $KafkaTopic
        }
        if ($KafkaGroup -ne "")               { $kArgs.Group = $KafkaGroup }
        if ($KafkaToDatetime -ne "")          { $kArgs.ToDatetime = $KafkaToDatetime }
        if ($KafkaToEarliest)                 { $kArgs.ToEarliest = $true }
        if ($KafkaToLatest)                   { $kArgs.ToLatest = $true }
        if ($KafkaDeleteAndRecreate)          { $kArgs.DeleteAndRecreateTopic = $true }
        if ($KafkaPartitions -gt 0)           { $kArgs.Partitions = $KafkaPartitions }
        if ($KafkaReplicationFactor -gt 0)    { $kArgs.ReplicationFactor = $KafkaReplicationFactor }
        if ($KafkaBin -ne "")                 { $kArgs.KafkaBin = $KafkaBin }
        if ($KafkaApply)                      { $kArgs.Apply = $true }
        # 被调脚本三种模式的成功路径都以 exit 0 结束、失败一律 throw;以 exit 结束时只设 $LASTEXITCODE、不抛,先清零再判,失败不得当成成功。
        $global:LASTEXITCODE = 0
        & (Join-Path $ScriptDir "kafka_offset_reset.ps1") @kArgs
        if ($LASTEXITCODE -ne 0) { throw "kafka_offset_reset.ps1 failed (exit code $LASTEXITCODE)" }
    }
    "k8s-zone-rollback" {
        # Wraps k8s_zone_rollback.ps1 — see docs/design/zone_data_rollback.md §3.
        # Orchestrates: preflight → zone-down → post-stop checks → MySQL PITR (manual) → inventory
        #             → discard post-TargetTime Redis state → verify → zone-up → verification checklist.
        if ($RollbackTargetTime -eq "") {
            throw "k8s-zone-rollback requires -RollbackTargetTime (UTC, e.g. '2026-05-15T14:23:00Z')"
        }
        # 已废弃的两个参数在调用回滚脚本之前拒绝,不静默吞掉(AGENTS §11.3)。
        if ($RollbackConfirmFlushSharedRedis) {
            throw "k8s-zone-rollback 不再支持 -RollbackConfirmFlushSharedRedis:回档已不做 FLUSHDB(归属与位置键、交接标记、路由映射与账号目录同在一个 Redis DB,FLUSHDB 等于事故),改由 zone_rollback_discard 只丢弃 TargetTime 之后的状态;整套环境重建走 AGENTS.md §6.2。去掉该参数重跑;尚未做任何操作。"
        }
        if ($RollbackRedisPassword -ne "") {
            throw "k8s-zone-rollback 不再支持 -RollbackRedisPassword:口令经命令行会进进程列表与 shell 历史。把口令放进环境变量,改传 -RollbackRedisPasswordEnv <变量名>;尚未做任何操作。"
        }
        # 回滚目标版本必须显式给。默认的 git 短 sha 是**当前工作树**的版本,
        # 拿它回滚等于"把数据回档到过去、把代码留在现在",比不回滚更危险。
        if (-not $script:NodeImageExplicit) {
            throw "k8s-zone-rollback requires an explicit -NodeImage (回滚目标版本的镜像引用,如 ghcr.io/luyuancpp/mmorpg-node:<回滚目标 sha>)。默认值是当前工作树的 sha,不是回滚目标。"
        }
        # 按用途拆实例(G4a 第 1a 批):回档脚本只声明 -RedisSplit(Step 6 的 zone-up 用它),不声明确认开关与容量 ——
        # Step 1 已删掉 namespace,切换闸在 Step 6 无从触发;回档不 infra-up,容量不生效。显式给了就拒绝,不静默吞掉(AGENTS §11.3)。
        $rollbackRedisSplitExtras = @()
        if ($ConfirmRedisSplitSwitch) { $rollbackRedisSplitExtras += '-ConfirmRedisSplitSwitch' }
        if (-not [string]::IsNullOrWhiteSpace($RedisQueueMaxMemory)) { $rollbackRedisSplitExtras += '-RedisQueueMaxMemory' }
        if (-not [string]::IsNullOrWhiteSpace($PlayerRedisMaxMemory)) { $rollbackRedisSplitExtras += '-PlayerRedisMaxMemory' }
        if (-not [string]::IsNullOrWhiteSpace($PlayerRedisStorage)) { $rollbackRedisSplitExtras += '-PlayerRedisStorage' }
        if ($rollbackRedisSplitExtras.Count -gt 0) {
            throw "k8s-zone-rollback 不支持 $($rollbackRedisSplitExtras -join ', '):回档的 Step 6 只重建 zone(namespace 已在 Step 1 删掉,没有可比的线上排队地址;也不 apply 排队实例),给的值不会生效。以 queue 运行的 zone 回档只需 -RedisSplit queue;去掉这些参数重跑;尚未做任何操作。"
        }
        # 必须 hashtable splatting:数组 splatting 会按**位置**绑定,
        # "-ZoneName" 这个字符串本身会被绑到 ZoneName、"$ZoneName" 绑到 [int]$ZoneId,
        # 于是灾难回滚脚本连第一步都进不去。
        $rbArgs = @{
            ZoneName             = $ZoneName
            ZoneId               = $ZoneId
            TargetTime           = $RollbackTargetTime
            NodeImage            = $NodeImage
            NamespacePrefix      = $NamespacePrefix
        }
        if ($RollbackRedisHost -ne "")     { $rbArgs.RedisHost = $RollbackRedisHost }
        if ($RollbackRedisPort -ne "6379") { $rbArgs.RedisPort = $RollbackRedisPort }
        if ($RollbackRedisDB -ne 0)        { $rbArgs.RedisDB = $RollbackRedisDB }
        if ($RollbackSkipMySqlPause)       { $rbArgs.SkipMySqlPause = $true }
        # 回档丢弃语义的新参数(2026-10-18):非空 / 大于 0 才转发,默认值与校验只在回滚脚本一处。
        if ($RollbackRedisPasswordEnv -ne "")  { $rbArgs.RedisPasswordEnv = $RollbackRedisPasswordEnv }
        if ($RollbackMySqlDsnEnv -ne "")       { $rbArgs.MySqlDsnEnv = $RollbackMySqlDsnEnv }
        if ($RollbackDiscardTool -ne "")       { $rbArgs.RollbackDiscardTool = $RollbackDiscardTool }
        if ($RollbackId -ne "")                { $rbArgs.RollbackId = $RollbackId }
        if ($RollbackPitrRestoredTo -ne "")    { $rbArgs.PitrRestoredTo = $RollbackPitrRestoredTo }
        if ($RollbackAcceptBlockedPlayers -gt 0) { $rbArgs.AcceptBlockedPlayers = $RollbackAcceptBlockedPlayers }
        if ($RollbackAllowEmptyTarget)         { $rbArgs.AllowEmptyTarget = $true }
        if ($RollbackAcknowledgeNoPostTWrites) { $rbArgs.AcknowledgeNoPostTWrites = $true }
        if ($RollbackApply)                { $rbArgs.Apply = $true }
        # 留空不传,由下游默认值接管(k8s_zone_rollback.ps1 的 Step 6 再原样转给 k8s-zone-up);默认值只在 k8s_deploy.ps1 一处。
        # 这组参数不粘滞:以 external / "0" 回退 / 经 Ingress 对外 / staging、prod 档 / 开发口令登录运行的 zone 回滚时,
        # 必须照传上一次部署的同一组值。漏传 -GateRouterMode / -ClientEntryMode / -GatewayIngressHost 时由 k8s_zone_rollback.ps1
        # 第 0 步在停服之前拒绝(-Apply);地址参数不逐项核对,该步只把集群里 gate 现有的地址 env 打出来供核对,
        # 参数组合错误由同一步的 k8s-zone-up -DryRun 静态预检拦下。
        # -ReleaseProfile 只转发显式值:以前这里不转发,Step 6 恒按 dev 档部署,prod zone 回滚会得到 dev 模式的 login 与占位密钥。
        if ($script:ReleaseProfileExplicit)                                { $rbArgs.ReleaseProfile = $ReleaseProfile }
        if ($LoginDevPasswordAuth)                                         { $rbArgs.LoginDevPasswordAuth = $true }
        # 切换确认闸同一口径透传,由回滚脚本只转给 Step 6(不豁免它第 0 步的停服前核对),语义见 k8s_zone_rollback.ps1 同名参数。
        if ($AllowDisruptiveSwitch)                                        { $rbArgs.AllowDisruptiveSwitch = $true }
        if ($RollbackConfirmNoGatewayIngress)                              { $rbArgs.ConfirmNoGatewayIngress = $true }
        # 按用途拆实例:非 auto 才转发,回档脚本再原样转给 Step 6(以 queue 运行的 zone 必须显式给,理由见参数注释)。
        if (-not [string]::IsNullOrWhiteSpace($RedisSplit) -and $RedisSplit -ne 'auto') { $rbArgs.RedisSplit = $RedisSplit }
        # 玩家域 Redis Cluster:回滚脚本把主节点数转给 Step 6;种子与口令变量名交给回档工具,线上是集群时必填(回滚脚本第 0 步核对)。
        if ($PlayerRedisMasters -gt 0)                                     { $rbArgs.PlayerRedisMasters = $PlayerRedisMasters }
        if ($PlayerRedisReplicasPerMaster -gt 0)                           { $rbArgs.PlayerRedisReplicasPerMaster = $PlayerRedisReplicasPerMaster }
        if (-not [string]::IsNullOrWhiteSpace($RollbackPlayerRedisSeeds))  { $rbArgs.PlayerRedisSeeds = $RollbackPlayerRedisSeeds }
        if (-not [string]::IsNullOrWhiteSpace($RollbackPlayerRedisPasswordEnv)) { $rbArgs.PlayerRedisPasswordEnv = $RollbackPlayerRedisPasswordEnv }
        if (-not [string]::IsNullOrWhiteSpace($GateRouterMode))            { $rbArgs.GateRouterMode = $GateRouterMode }
        # B0-4 豁免开关同一口径,回滚脚本只转给 Step 6;以 warn 运行的 zone 回滚时照传(漏传的后果见 k8s_zone_rollback.ps1 同名参数)。
        if (-not [string]::IsNullOrWhiteSpace($NodeOpenFileLimitMode))     { $rbArgs.NodeOpenFileLimitMode = $NodeOpenFileLimitMode }
        if (-not [string]::IsNullOrWhiteSpace($ClientEntryMode))           { $rbArgs.ClientEntryMode = $ClientEntryMode }
        if (-not [string]::IsNullOrWhiteSpace($ClientPublicHost))          { $rbArgs.ClientPublicHost = $ClientPublicHost }
        if (-not [string]::IsNullOrWhiteSpace($GateClientHostTemplate))    { $rbArgs.GateClientHostTemplate = $GateClientHostTemplate }
        if ($GateNodePortBase -ne -1)                                      { $rbArgs.GateNodePortBase = $GateNodePortBase }
        if (-not [string]::IsNullOrWhiteSpace($GateExternalTrafficPolicy)) { $rbArgs.GateExternalTrafficPolicy = $GateExternalTrafficPolicy }
        if (-not [string]::IsNullOrWhiteSpace($GateServiceType))           { $rbArgs.GateServiceType = $GateServiceType }
        if (-not [string]::IsNullOrWhiteSpace($RequireClientEndpoint))     { $rbArgs.RequireClientEndpoint = $RequireClientEndpoint }
        if (-not [string]::IsNullOrWhiteSpace($GatewayIngressHost))        { $rbArgs.GatewayIngressHost = $GatewayIngressHost }
        if (-not [string]::IsNullOrWhiteSpace($GatewayIngressClassName))   { $rbArgs.GatewayIngressClassName = $GatewayIngressClassName }
        if (-not [string]::IsNullOrWhiteSpace($GatewayIngressTlsSecret))   { $rbArgs.GatewayIngressTlsSecret = $GatewayIngressTlsSecret }
        if (-not [string]::IsNullOrWhiteSpace($GatewayTrustedProxies))     { $rbArgs.GatewayTrustedProxies = $GatewayTrustedProxies }
        # login 第三方登录(微信 / QQ)同一口径:非空 / 被指定才转发。回滚脚本在 Step 1 删 namespace 之前核对线上 login ConfigMap 与
        # Secret login-oauth,线上开着的会被本次参数关掉或改掉就拒绝;它再把这组参数转给 Step 6。
        if (-not [string]::IsNullOrWhiteSpace($LoginWeChatAppId))          { $rbArgs.LoginWeChatAppId = $LoginWeChatAppId }
        if (-not [string]::IsNullOrWhiteSpace($LoginWeChatEndpoint))       { $rbArgs.LoginWeChatEndpoint = $LoginWeChatEndpoint }
        if ($LoginWeChatAllowOpenId)                                       { $rbArgs.LoginWeChatAllowOpenId = $true }
        if (-not [string]::IsNullOrWhiteSpace($LoginQQAppId))              { $rbArgs.LoginQQAppId = $LoginQQAppId }
        if (-not [string]::IsNullOrWhiteSpace($LoginQQEndpoint))           { $rbArgs.LoginQQEndpoint = $LoginQQEndpoint }
        if ($LoginQQAllowOpenId)                                           { $rbArgs.LoginQQAllowOpenId = $true }
        if ($ConfirmLoginAuthProviderChange)                               { $rbArgs.ConfirmLoginAuthProviderChange = $true }
        # 预检、Step 1、Step 6 用同一集群;以前这里不转发,-KubeContext 被静默忽略、回滚落到 kubectl 当前 context。
        if (-not [string]::IsNullOrWhiteSpace($KubeContext))               { $rbArgs.KubeContext = $KubeContext }
        if (-not [string]::IsNullOrWhiteSpace($KubeConfig))                { $rbArgs.KubeConfig = $KubeConfig }
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
