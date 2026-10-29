param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("help", "pbgen-build", "pbgen-run", "proto-gen-build", "proto-gen-run", "tree", "naming-audit", "naming-apply", "third-party-lock-check", "third-party-zlib-build", "third-party-protobuf-build", "third-party-boringssl-build", "no-raw-pointer-setup", "iwyu-run", "k8s-infra-up", "k8s-infra-down", "k8s-infra-status", "k8s-redis-split-status", "k8s-redis-wipe", "k8s-zone-up", "k8s-zone-down", "k8s-zone-status", "k8s-zone-rollback", "k8s-all-up", "k8s-all-down", "k8s-all-status", "k8s-build-all", "k8s-exposure-preflight", "k8s-stage-runtime", "k8s-image-preflight", "k8s-build-image", "k8s-push-image", "k8s-release-zone", "k8s-release-all", "k8s-route-tool", "go-svc-start", "go-svc-start-exe", "go-svc-stop", "go-svc-status", "go-svc-list", "go-svc-build", "go-svc-build-images", "go-svc-push-images", "java-svc-build-image", "java-svc-push-image", "cpp-node-start", "cpp-node-stop", "cpp-node-status", "cpp-node-list", "dev-start", "dev-start-exe", "dev-start-zones", "dev-stop", "dev-status", "dev-robot-zones", "merge-zone", "merge-zone-audit", "merge-zone-unmerge", "merge-zone-relocate", "merge-zone-relocate-abort", "merge-zone-pin-placement", "merge-zone-storage-audit", "merge-zone-capability-check", "kafka-offset-reset", "git-stats")]
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
    # go/db 存盘消费组的渲染模式(keep | zone | legacy),透传给 k8s_deploy.ps1 同名参数,语义见那边的参数注释与
    # docs/design/db-consumer-group-per-zone.md(zone / legacy 只许配 k8s-zone-up,由 k8s_deploy.ps1 在入口拒绝)。
    # 默认留空 = 不透传,由 k8s_deploy.ps1 的默认值 keep 接管(写法同 -GateRouterMode:默认值只留在 k8s_deploy.ps1 一处)。
    # k8s-image-* / k8s-release-* 不转发它(不部署 db);k8s-zone-rollback 不经本参数,回滚脚本的 Step 6 自己固定传 zone。
    # 这两条路上显式给了即在任何副作用之前拒绝,不静默吞掉。
    [ValidateSet("", "keep", "zone", "legacy")]
    [string]$DbConsumerGroup = "",
    # 排行榜评分快照发布开关(k8s-infra-up / k8s-all-up),透传给 k8s_deploy.ps1 -MatchRatingSnapshotPublish(docs/design/leaderboard-system.md §5.4)。
    # 默认留空 = 不透传,由 k8s_deploy.ps1 的默认值 "0" 接管(写法同 -GateRouterMode)。⚠ 那边**不粘滞**:以 "1" 运行的环境用本入口
    # 发版却不传,会落回 "0"、榜停止更新(部署输出有一行 MatchRatingSnapshotPublish: 0)。k8s-release-*(k8s_image.ps1)不部署全局
    # Go 服务、碰不到 match,不转发它。
    [ValidateSet("", "0", "1")]
    [string]$MatchRatingSnapshotPublish = "",
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
    # ── merge-zone / merge-zone-audit / merge-zone-unmerge / 落点(relocate / pin-placement / storage-audit)──
    # 合服跨了三个 Redis DB。DB 号猜错不会报错,只会**静默无效**。所以每个 DB
    # 都是独立参数,默认值取自各服务 yaml。
    #   mapping DB 0   player:zone / player:placement / lock:player / merge:in_progress /
    #                  merge:merged_into / db:capability:zone(落点记录与能力标记都和 player:zone 同库)
    #                  **一定是 0**:data_service 用 go-zero 的 MustNewRedis,
    #                  而 go-zero v1.10.0 的 RedisConf 没有 DB 字段,yaml 里写
    #                  `DB: 15` 会被静默忽略(那行 inert 的键已从 yaml 删除)。
    #                  传 -MergeMappingRedisDB 15 = fence 与 remap 一起指向
    #                  data_service 从不碰的库 = 审计恒绿、合服报成功却零改动。
    #   guild   DB 0   guild_rank:zone / guild:v2 / guild_rank:maintenance_lock(2026-10-23 由 DB 2 改为 0)
    #   login   DB 0   player_merge_notice / player:session / kafka:retry|dead
    # friend 的 DB 3 已退役(friend 移植:F2 删读者、F3 删参数):friend:online 从来没有写者,读者也已删除,
    # 好友在线状态改为读共享库的契约 key player:session:{id}(login 段那一行)。
    # 所以这里不再有 friend 的 Redis 参数;好友数据在独占库 mmorpg_friend,归 merge_zone 的 MySQL 审计管。
    [int]$MergeSourceZone = 0,
    [int]$MergeTargetZone = 0,
    [string]$MergeMySqlDsn = "root:@tcp(127.0.0.1:3306)/mmorpg?charset=utf8mb4&parseTime=true&loc=Local",
    [string]$MergeRedisAddr = "127.0.0.1:6379",
    [string]$MergeRedisPassword = "",
    [int]$MergeRedisDB = 0,
    # -1 = 从 go/data_service/etc/data_service.yaml 的 MappingRedis 现读(见 Get-MergeMappingRedis)。
    [string]$MergeMappingRedisAddr = "",
    [int]$MergeMappingRedisDB = -1,
    [string]$MergeMappingRedisPassword = "",
    [string]$MergeNoticeRedisAddr = "",
    [int]$MergeNoticeRedisDB = 0,
    [string]$MergeSceneRedisAddr = "",
    [int]$MergeSceneRedisDB = 0,
    # etcd 客户端地址(逗号分隔,http://host:2379),转发为 -etcd-endpoints。merge-zone / merge-zone-unmerge 必填:
    # 门禁 P2 与 -MergeClearSourceHotState 要 etcd 场景节点注册表的正面证据(该 zone 一条注册都没有)。
    # 不设缺省值:指向错的 etcd(例如本机空 etcd 而 Redis 指向 K8s)会把活着的 zone 误判为已下线。
    [string]$MergeEtcdEndpoints = "",
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
    # 清单 / 复核 / 撤销 / 搬库续跑与放弃。合服与搬库留空时在仓库根按时间戳生成;
    # -DryRun 从不写它,只写 <path>.dryrun.json 预览(apply / unmerge / verify 都拒读预览)。
    [string]$MergeManifestPath = "",
    [int]$MergeExpectedSrcPlayers = -1,
    [switch]$MergeAllowEmptySource,
    [string]$MergeTableListJson = "",
    # Kafka 积压门禁:有 CLI 就真查,没有就必须显式声明(不允许静默跳过)。
    # TopicGeneration 必须与 go/db/etc/db.yaml 一致 —— 查错 topic 等于「查了一个不存在的 topic,lag 恒 0」,
    # 门禁形同虚设;留空(-1)即现读 db.yaml。
    # 消费组(-MergeKafkaGroup)留空 = 不转发,由 tools/merge_zone 按 zone 推导候选组(db_rpc_consumer_group_z<zone>
    # 加 legacy 组 db_rpc_consumer_group),只认在本 topic 上有已提交位点的唯一权威组(docs/design/db-consumer-group-per-zone.md §7)。
    # 只在需要显式指定实际在跑的组时才给(例如本机混用过单 zone 默认启动的裸名组与 -Zone N 启动的 _zN 组)。
    [string]$MergeKafkaConsumerGroupsCmd = "",
    [string]$MergeKafkaBootstrap = "127.0.0.1:9092",
    [string]$MergeKafkaGroup = "",
    [int]$MergeKafkaTopicGeneration = -1,
    [switch]$MergeAssumeKafkaDrained,
    # 聚宝斋步骤 3b(tools/merge_zone -skip-trade-mysql):merge_zone 默认要求 mmorpg_trade.trade_listing 存在(fail-closed)。
    # 只在确实没有部署 trade 的环境显式跳过;跳过后 -VerifyMerged 的 verify:trade_listing 记 warn "NOT VERIFIED",不算通过。
    [switch]$MergeSkipTradeMySql,
    # 帮会步骤开关(tools/merge_zone -skip-guild-mysql / -skip-guild-rank)。与 trade 开关同理放在公共参数里:
    # 合服 / 撤销 / 审计必须同一口径。审计只在**两个都给**时才把 guild_member 核对记为 SKIPPED(warn);
    # 只给一个时审计仍按「部署了帮会」去查,库或表查不成是 INFRA(exit 2)。只在确实没部署 guild 的环境使用。
    [switch]$MergeSkipGuildMySql,
    [switch]$MergeSkipGuildRank,
    # 玩家主数据处理方式(player-storage-placement.md §10):留空 = 工具默认 pin(只改归属、给无记录者钉落点、
    # 不拷行,要求 -MergeDbCapabilityZones 里每个 zone 的 go/db 已带能力标记);copy = 旧版 go/db 的兼容模式。
    # 只对 merge-zone 生效:撤销与复核按清单里记录的模式,续跑必须与清单同模式。
    # PowerShell 不校验参数默认值,所以默认 "" 不受 ValidateSet 约束,只拦显式传错的值。
    [ValidateSet("pin", "copy")]
    [string]$MergePlayerRowsMode = "",
    # 必须带能力标记 db:capability:zone:{z}=placement-routing-v1 的 zone:逗号分隔的 zone 号,或字面量 none
    # (「此刻没有别的 zone 在跑,不检查」)。**工具没有缺省值**(标记是 90s 心跳,T-0 时 src / dst 已 zone-down,
    # 旧的缺省 "src,dst" 必然被拒;src / dst 记号已删除):
    #   merge-zone(pin)                必填,可 none;T-0 只列仍在跑的 zone(runbook §5.1)。copy 模式只在清单玩家已有
    #                                   落点记录时由工具要求
    #   merge-zone-unmerge              清单是 pin 模式时由工具要求(可 none);copy 撤销给了才查
    #   merge-zone-relocate             必填,不接受 none(搬库在线进行,全部在跑的 zone 都要列)
    #   merge-zone-capability-check     必填,不接受 none(合服后 dst zone-up、开服之前核对它)
    [string]$MergeDbCapabilityZones = "",
    # -Command merge-zone-pin-placement:给 home_zone==N 且无落点记录的玩家钉 "N:1"。有效落点不变,在线执行安全、幂等。
    [int]$MergePinPlacementZone = 0,
    # -Command merge-zone-relocate / merge-zone-relocate-abort(搬库,§9)。storage id:1..999999 = zone_{id}_db,
    # >=1000000 = player_store_{id}_db。用 long:工具按 uint32 接收,int 装不下上半段。
    # relocate-abort 可选给这两个,工具会与清单核对,防止拿错清单放弃了别的搬库。
    [long]$MergeRelocateSourceStorage = 0,
    [long]$MergeRelocateTargetStorage = 0,
    # 逗号分隔的 player_id;留空 = 有效落点为源库的全部玩家。
    [string]$MergeRelocatePlayerIds = "",
    # 0 / 空 = 不转发,用工具默认(批大小 100;R2 等排序锁 150s,略大于 go/db 排序锁 TTL 2m)。
    # 等锁时长是 Go duration 字面量,如 "150s" / "3m"。
    [int]$MergeRelocateBatchSize = 0,
    [string]$MergeRelocateLockWait = "",
    # -Command merge-zone-storage-audit:要审计的落点库 id。退役一个库之前,结论必须是
    # "RETIREMENT CHECK: nothing routes to storage" 且 unmapped / unreadable 都为 0。
    [long]$MergeAuditStorage = 0,
    # merge-zone-audit only — switches to post-merge verification mode that
    # asserts source-zone state has been emptied. See merge-zone-runbook.md §5.
    # 必须同时给 -MergeManifestPath:复核逐个核对清单里的玩家(映射 == dst、落点库有行),没有清单无从核对。
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
    # 0 = 没给(不透传)。-KafkaDeleteAndRecreate 时 kafka_offset_reset.ps1 要求显式给出、且等于现有 topic 的副本数;
    # 以前缺省 1 且只在 ≠ 1 时透传,单 broker 档显式给 1 也传不下去。
    [int]$KafkaReplicationFactor = 0,
    [string]$KafkaBin = "",
    # 转给 kafka_offset_reset.ps1 -RetireGroup / -FinalizeHandoff(db_task 消费组交接收尾与回档退役旧组位点,
    # 见 docs/design/db-consumer-group-per-zone.md §5)。都要配 -KafkaPartitions(> 0)。
    [string]$KafkaRetireGroup = "",
    [switch]$KafkaFinalizeHandoff,
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
    # 留空 = 由 k8s_zone_rollback.ps1 按 -ZoneId 与 go/db/etc/db.yaml 的 TopicGeneration 推导 db_task_zone_<id>[_g<gen>];
    # 显式给了却与推导值不同,回滚脚本在入口拒绝。旧默认 "db_task_topic" 全仓不存在,会静默重置一个空 topic。
    [string]$RollbackKafkaTopic = "",
    # 留空 = 由 k8s_zone_rollback.ps1 按 go/db/etc/db.yaml 的 GroupID 基名推导:-RollbackKafkaGroup 为 <基名>_z<ZoneId>
    # (回档后的权威组,Step 5 重置它),-RollbackKafkaLegacyGroup 为 <基名>(Step 5 删除它在本 topic 上的位点)。
    # 非空才转发。以前缺省是 legacy 共享组:别的 zone 在线时它不是 Empty,Step 5 的重置一定被拒。
    [string]$RollbackKafkaGroup = "",
    [string]$RollbackKafkaLegacyGroup = "",
    [switch]$RollbackSkipMySqlPause,
    # 转给 k8s_zone_rollback.ps1 -ConfirmNoGatewayIngress:显式声明回滚后该 zone 不要 Ingress gateway(Ingress 核对的显式决定,
    # 与 -GatewayIngressHost 互斥)。switch 不占位置参数,插在这里不影响后面参数的位置绑定。
    [switch]$RollbackConfirmNoGatewayIngress,
    # 已废弃(2026-10-18):回档不再 FLUSHDB(go/db 游标、重试收据、映射、账号目录与各 zone 缓存同在一个 Redis DB)。
    # 传了即在调用回滚脚本之前拒绝;整套环境重建走 AGENTS.md §6.2。
    [switch]$RollbackConfirmFlushSharedRedis,
    # 以下转给 k8s_zone_rollback.ps1 的同名参数(去掉 Rollback 前缀),非空 / 大于 0 才转发,语义与校验都在回滚脚本一处:
    # Redis 口令所在的环境变量名、zone_<id>_db DSN 所在的环境变量名(都只给变量名)、预编译的 zone_rollback_discard、屏障协议世代
    # (legacy;strict 待负责人裁决 #3)、rollback-id(单步或整脚本重跑用同一个)、Step 1b 的静默窗口秒数(≥ 300)、
    # -RollbackSkipMySqlPause 时声明的 PITR 恢复时刻、带 blocked 玩家继续的人数(待负责人裁决 #4,回滚脚本会拒绝)。
    # switch 与这些字符串 / 整数都放在参数块末尾,不影响前面参数的位置绑定。
    [string]$RollbackRedisPasswordEnv = "",
    [string]$RollbackMySqlDsnEnv = "",
    [string]$RollbackDiscardTool = "",
    [ValidateSet("", "legacy", "strict")]
    [string]$RollbackBarrierProtocol = "",
    [string]$RollbackId = "",
    [int]$RollbackQuiesceSeconds = 0,
    [string]$RollbackPitrRestoredTo = "",
    [int]$RollbackAcceptBlockedPlayers = 0,
    # 转给 k8s_zone_rollback.ps1 的 -AllowEmptyTarget / -AcknowledgeNoPostTWrites(回档复核 [11]):只在被指定时转发,语义见回滚脚本同名参数
    # (前者只用于从未落过库的新 zone;后者只用于 T 之后确实没有任何存盘的 zone,inventory / purge / verify 一并带上)。
    # 以前经本入口走不到这两条路径,只能直接调用回滚脚本。
    [switch]$RollbackAllowEmptyTarget,
    [switch]$RollbackAcknowledgeNoPostTWrites,
    # 原 -RollbackKafkaDrainTimeoutSec 已删除(2026-10-15):回滚脚本删掉了 zone-down 之后等 LAG 排空的 Step 2。
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
    # ── merge-zone 系列的映射来源(D1 第 2 批,docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §13.3 第 3 条)──
    # 转给 tools/merge_zone 的 -route-source / -route-db-dsn / -waitaof-local / -waitaof-replicas / -waitaof-timeout-ms。
    # **没有缺省值,也不从 data_service.yaml 推断**:除 merge-zone-capability-check(不读写映射)外的每个 merge-zone 命令都必须显式给
    # -MergeZoneRouteSource db|redis,缺了在调用工具之前点名拒绝。推断不可靠:本机 yaml 与线上部署可以是两种模式,猜错的一边
    # 会写错库(db_first 环境里直写 Redis 写出库不知道的值;redis 环境里写库则缓存根本不认)。
    #   db    data_service 已是 RouteStore.Mode=db_first:必须同时给 -MergeZoneRouteDbDsn(全局库 DSN,dev testdb / K8s mmorpg_global;
    #         含口令,建议从环境变量取,例如 -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN,不进 shell 历史)。
    #   redis 只在全部 data_service 仍是 RouteStore.Mode=redis 时用;不接受 -MergeZoneRouteDbDsn(由工具拒绝)。
    # 工具开写前还会核对库与 mapping Redis 是否成对(mapping:route:watermark),不成对即拒绝。
    # WAITAOF 三项 -1 = 不转发,用工具缺省(1 / 0 / 1500ms);redis-ha 档把 -MergeZoneWaitAofReplicas 设为 1。
    # 追加在参数块末尾,理由同上(字符串参数按声明顺序占位置)。
    [ValidateSet("", "db", "redis")]
    [string]$MergeZoneRouteSource = "",
    [string]$MergeZoneRouteDbDsn = "",
    [int]$MergeZoneWaitAofLocal = -1,
    [int]$MergeZoneWaitAofReplicas = -1,
    [int]$MergeZoneWaitAofTimeoutMs = -1,
    # ── k8s-route-tool(D1 第 1b-3 批,§13.4 第 3 条)──
    # 转给 tools/scripts/k8s_route_tool.ps1:-RouteToolArgs 原样转给它的同名参数(子命令与参数,第一项是子命令;含 '-' 开头的项
    # 请用 -Command 形式传数组);-RouteToolReportDir / -RouteToolTimeoutSeconds / -RouteToolImage 去掉 RouteTool 前缀转给
    # -ReportDir / -TimeoutSeconds / -Image,留空 / 0 不转发(默认值只在 k8s_route_tool.ps1 一处)。zone 与集群取本脚本的
    # -ZoneName / -NamespacePrefix / -KubeContext / -KubeConfig,-DryRun 同名转发。退出码原样透传(route-tool 容器的 0 / 1 / 2 / 3)。
    [string[]]$RouteToolArgs = @(),
    [string]$RouteToolReportDir = "",
    [int]$RouteToolTimeoutSeconds = 0,
    [string]$RouteToolImage = "",
    # ── merge-zone / merge-zone-unmerge 续跑接手库围栏(D1 第 2 批)──
    # 转给 tools/merge_zone 的 -take-over-fence <run_id>。-MergeZoneRouteSource db 下,写到一半中止的合服 / 撤销会把库围栏
    # (zone_merge_fence)故意留着,中止文案给出 run_id;确认那次运行的进程已不在之后,原命令加 -MergeZoneTakeOverFence <run_id>
    # 重跑,工具在一个库事务里把围栏接到新 run_id,两区一刻不解封(docs/ops/merge-zone-runbook.md §15.4)。db 源下不要再
    # 「DEL merge:in_progress 键后重跑」:库里的围栏才是真源。留空不转发;适用范围由工具判(经 Get-MergeRouteSourceArgs 转发)。
    # 追加在参数块末尾,理由同上(字符串参数按声明顺序占位置)。
    [string]$MergeZoneTakeOverFence = "",
    # ── D1 映射开关,透传给 k8s_deploy.ps1 同名参数(k8s-infra-up / k8s-zone-up / k8s-all-up)──
    # 语义、组合门禁与粘滞核对(线上现状与本次参数不一致且没带 -ConfirmRouteSwitchChange 即拒绝)都只在 k8s_deploy.ps1 一处,
    # 本入口只透传:-RouteStoreMode 非空才传,五个 switch 只在被指定时传,默认值只留在下游(2026-10-21 起 k8s_deploy 缺省
    # db_first / 全 true = 全球默认,部署 Go 服务时入口闸只许这一组值)。线上仍是旧值(redis / false)的集群经本入口部署会被
    # 粘滞核对拒绝,确认升级时加 -ConfirmRouteSwitchChange;读方前提(本次不部署的 zone 必须已 db_first + Arm)不受它豁免。
    # k8s_image.ps1 不部署 Go 服务,k8s-image-* / k8s-release-* 显式给了即拒绝。k8s_zone_rollback.ps1 不声明这六个参数,
    # 它的 Step 6 按 k8s_deploy 缺省重建 zone;k8s-zone-rollback 显式给了即在调用回档脚本之前拒绝(不静默丢掉)。
    # 追加在参数块末尾,理由同上(字符串参数按声明顺序占位置)。
    [ValidateSet("", "redis", "db_first")]
    [string]$RouteStoreMode = "",
    [switch]$RouteReconcileArm,
    [switch]$RouteReadRequiresReady,
    [switch]$DbPlacementRequireRouteReady,
    [switch]$GuildRequireRouteReady,
    [switch]$ConfirmRouteSwitchChange
)

$ErrorActionPreference = "Stop"
if ($LocalMySQL -and $Command -notin @('go-svc-start', 'go-svc-start-exe', 'dev-start', 'dev-start-exe', 'dev-start-zones')) {
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
    # 同上:留空不传,由 k8s_deploy.ps1 的默认值(keep)接管。
    if (-not [string]::IsNullOrWhiteSpace($DbConsumerGroup)) {
        $args.DbConsumerGroup = $DbConsumerGroup
    }
    # 同上:留空不传,由 k8s_deploy.ps1 的默认值("0")接管。
    if (-not [string]::IsNullOrWhiteSpace($MatchRatingSnapshotPublish)) {
        $args.MatchRatingSnapshotPublish = $MatchRatingSnapshotPublish
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
    # D1 映射开关同一口径:字符串非空才传,switch 只在被指定时传。组合门禁与粘滞核对只在 k8s_deploy.ps1
    # (Assert-RouteSwitchPreflight / Assert-RouteSwitchClusterState),本函数只透传,不复制规则。
    if (-not [string]::IsNullOrWhiteSpace($RouteStoreMode)) { $args.RouteStoreMode = $RouteStoreMode }
    if ($RouteReconcileArm) { $args.RouteReconcileArm = $true }
    if ($RouteReadRequiresReady) { $args.RouteReadRequiresReady = $true }
    if ($DbPlacementRequireRouteReady) { $args.DbPlacementRequireRouteReady = $true }
    if ($GuildRequireRouteReady) { $args.GuildRequireRouteReady = $true }
    if ($ConfirmRouteSwitchChange) { $args.ConfirmRouteSwitchChange = $true }
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
    # -DbConsumerGroup:k8s_image.ps1 不部署 db(不传 -GoSvcRegistry),给什么值都不生效;切组只走 k8s-zone-up(docs/design/db-consumer-group-per-zone.md)。
    if (-not [string]::IsNullOrWhiteSpace($DbConsumerGroup)) { $notForwarded += '-DbConsumerGroup' }
    # login 第三方登录(微信 / QQ)与它的确认开关同 -LoginDevPasswordAuth:只落在 login 上,k8s_image.ps1 不部署 login、也不声明它们。
    # 经这条路发布反过来也关不掉线上的第三方登录(不重写 login ConfigMap、不碰 Secret login-oauth),不需要防误关。
    if (-not [string]::IsNullOrWhiteSpace($LoginWeChatAppId)) { $notForwarded += '-LoginWeChatAppId' }
    if (-not [string]::IsNullOrWhiteSpace($LoginWeChatEndpoint)) { $notForwarded += '-LoginWeChatEndpoint' }
    if ($LoginWeChatAllowOpenId) { $notForwarded += '-LoginWeChatAllowOpenId' }
    if (-not [string]::IsNullOrWhiteSpace($LoginQQAppId)) { $notForwarded += '-LoginQQAppId' }
    if (-not [string]::IsNullOrWhiteSpace($LoginQQEndpoint)) { $notForwarded += '-LoginQQEndpoint' }
    if ($LoginQQAllowOpenId) { $notForwarded += '-LoginQQAllowOpenId' }
    if ($ConfirmLoginAuthProviderChange) { $notForwarded += '-ConfirmLoginAuthProviderChange' }
    # D1 映射开关只作用于 Go 服务的 ConfigMap,k8s_image.ps1 不部署 Go 服务。
    if (-not [string]::IsNullOrWhiteSpace($RouteStoreMode)) { $notForwarded += '-RouteStoreMode' }
    if ($RouteReconcileArm) { $notForwarded += '-RouteReconcileArm' }
    if ($RouteReadRequiresReady) { $notForwarded += '-RouteReadRequiresReady' }
    if ($DbPlacementRequireRouteReady) { $notForwarded += '-DbPlacementRequireRouteReady' }
    if ($GuildRequireRouteReady) { $notForwarded += '-GuildRequireRouteReady' }
    if ($ConfirmRouteSwitchChange) { $notForwarded += '-ConfirmRouteSwitchChange' }
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

# Get-MergeMappingRedis 从 go/data_service/etc/data_service.yaml 的 MappingRedis
# 段现读 Host / DB / Password,作为 -MergeMappingRedis* 的默认值。
#
# 为什么现读而不是写死:mapping Redis 的地址 / DB 号是合服里最容易出错、且出错
# **完全静默**的参数 —— 扫错库 = 一个玩家都匹配不到 =「合服成功,0 人被迁移」。
# DB 今天恒为 0(go-zero RedisConf 没有 DB 字段,yaml 里也已不写 DB 键,解析结果恒为 -1,
# 由 Get-MergeZoneArgs 兜底成 0);仍保留 DB 解析,是为了将来 data_service 真支持分库时与部署同源。
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
# 为什么必须现读:门禁查的是 db_task_zone_<src>[_g<gen>] 的 consumer lag。TopicGeneration
# 与 go/db 实际用的不一致时,查的是一个**不存在的 topic** ——
# lag 恒 0,门禁静默放行,于是在 go/db 还没消费完的时候就开始搬数据。
# 解析不到就返回 GroupID="" / TopicGeneration=-1,由调用方决定是回落到工具默认值
# 还是不转发(这里选择不转发,让工具用它自己文档化的默认值)。
# 返回的 GroupID 保留但不再转发:db.yaml 里的裸名只是基名,go/db 按 zone 拆组之后(本机 -Zone N 启动为 _zN,
# K8s 切组后为 _z<ZoneId>),实际组名由 tools/merge_zone 按 zone 推导并认定唯一权威组。
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
        "-scene-redis-db", $MergeSceneRedisDB
    )

    # Kafka 积压门禁:-kafka-topic-generation 必须转发。漏转时工具会去查 db_task_zone_<src>,
    # 在 TopicGeneration != 1 的环境上那是一个**不存在的 topic**:lag 查出来恒 0,门禁静默放行。
    # -kafka-group 只在显式 -MergeKafkaGroup 时转发:db.yaml 的 GroupID 只是基名(go/db 按 zone 拆组,本机 -Zone N 启动为 _zN,
    # K8s 切组后为 _z<ZoneId>),转发裸名会让门禁只查一个可能不存在、或已退役的组。不转发时由工具按 zone 推导候选组
    # (zone 组 + legacy 组),只认在本 topic 上有已提交位点的唯一权威组,两组都有就拒绝(先跑 kafka_offset_reset.ps1 -FinalizeHandoff)。
    $kGen = if ($MergeKafkaTopicGeneration -ge 0) { $MergeKafkaTopicGeneration } else { (Get-MergeDbKafka).TopicGeneration }
    # 只在这里转发 group / generation:CLI 路径与 -assume-kafka-drained 由
    # merge-zone 命令块单独追加(审计不跑积压门禁,给了也无害但没必要)。
    if ($MergeKafkaGroup -ne "") { $a += @("-kafka-group", $MergeKafkaGroup) }
    if ($kGen -ge 0) { $a += @("-kafka-topic-generation", $kGen) }
    if ($MergeRedisPassword -ne "")      { $a += @("-redis-password", $MergeRedisPassword) }
    if ($mapPwd -ne "")                  { $a += @("-mapping-redis-password", $mapPwd) }
    if ($MergeNoticeRedisAddr -ne "")    { $a += @("-notice-redis-addr", $MergeNoticeRedisAddr) }
    if ($MergeSceneRedisAddr -ne "")     { $a += @("-scene-redis-addr", $MergeSceneRedisAddr) }
    if ($MergeEtcdEndpoints -ne "")      { $a += @("-etcd-endpoints", $MergeEtcdEndpoints) }
    # 建表清单:go run 在 tools/merge_zone 里跑,相对路径会指错地方,一律转绝对。
    $tableList = if ($MergeTableListJson -ne "") { $MergeTableListJson }
                 else { Join-Path $repoRoot "generated\data\mysql_database_table_list.json" }
    $a += @("-table-list-json", (Convert-Path -LiteralPath $tableList -ErrorAction SilentlyContinue) ?? $tableList)
    if ($MergeSourceDataRedis -ne "")    { $a += @("-source-data-redis", $MergeSourceDataRedis, "-source-data-redis-db", $MergeSourceDataRedisDB) }
    if ($MergeTargetDataRedis -ne "")    { $a += @("-target-data-redis", $MergeTargetDataRedis, "-target-data-redis-db", $MergeTargetDataRedisDB) }
    if ($MergeDataRedisPassword -ne "")  { $a += @("-data-redis-password", $MergeDataRedisPassword) }
    if ($MergeExpectedSrcPlayers -ge 0)  { $a += @("-expected-src-players", $MergeExpectedSrcPlayers) }
    # trade 跳过开关放在公共参数里:merge-zone / merge-zone-unmerge / merge-zone-audit 必须同一口径,
    # 否则合服跳过了 3b、审计却仍去查 trade 库(恒报 INFRA),或反过来。
    if ($MergeSkipTradeMySql)            { $a += "-skip-trade-mysql" }
    # 帮会跳过开关同一理由:以前 ps1 不转发,没部署 guild 的环境只能绕过 ps1 手敲 go run,
    # 而 runbook §8 Step 5 / §10.1 的 dev_tools 命令在这类环境必然失败(gap-fixes A12)。
    if ($MergeSkipGuildMySql)            { $a += "-skip-guild-mysql" }
    if ($MergeSkipGuildRank)             { $a += "-skip-guild-rank" }
    Write-Host "merge_zone: mapping=$mapAddr db=$mapDB | guild=$MergeRedisAddr db=$MergeRedisDB | login db=$MergeNoticeRedisDB" -ForegroundColor DarkGray
    return $a
}

# Get-MergeKafkaGateArgs 组装 db_task 积压门禁的取证方式:有 CLI 就真查,没有就必须显式声明已排空。
# 合服与撤销共用这一份 —— 撤销(-mode unmerge)同样要先按目标区口径跑 P2~P7,缺这两个参数
# 工具直接拒绝;以前 merge-zone-unmerge 不转发它们,撤销从 ps1 入口根本跑不起来。
# 返回可能为空的数组,调用方一律用 @(...) 接,避免把 $null 当成一个参数塞给 go。
function Get-MergeKafkaGateArgs {
    $a = @()
    if ($MergeAssumeKafkaDrained)            { $a += "-assume-kafka-drained" }
    if ($MergeKafkaConsumerGroupsCmd -ne "") { $a += @("-kafka-consumer-groups-cmd", $MergeKafkaConsumerGroupsCmd, "-kafka-bootstrap", $MergeKafkaBootstrap) }
    return $a
}

# Get-MergeRouteSourceArgs 组装映射来源的 flag(D1 第 2 批,规格 §13.3 第 3 条):-route-source 必填、没有缺省值、**不从 yaml 推断**;
# db 时 -route-db-dsn 必填。只拒绝「缺了工具必然拒绝」的两条,其余(redis 配 DSN、WAITAOF 取值范围、库与 Redis 是否成对)
# 一律留给工具判,不在 ps1 里复制第二份规则。merge-zone-capability-check 不读写映射,不调用本函数。
# DSN 含口令:只进参数数组,不 Write-Host。返回数组,调用方用 @(...) 接。
function Get-MergeRouteSourceArgs {
    param([Parameter(Mandatory = $true)][string]$MergeCommand)
    if ($MergeZoneRouteSource -eq "") {
        throw ("$MergeCommand requires -MergeZoneRouteSource db|redis (there is no default and it is never inferred from data_service.yaml): " +
               "'db' when data_service runs RouteStore.Mode=db_first (also pass -MergeZoneRouteDbDsn <global DB DSN>), " +
               "'redis' only while every data_service still runs RouteStore.Mode=redis (docs/ops/merge-zone-runbook.md §15)")
    }
    $a = @("-route-source", $MergeZoneRouteSource)
    if ($MergeZoneRouteSource -eq "db" -and $MergeZoneRouteDbDsn -eq "") {
        throw ("$MergeCommand -MergeZoneRouteSource db requires -MergeZoneRouteDbDsn <DSN of the global DB that holds player_route " +
               "(dev: testdb, K8s: mmorpg_global)>; pass it from an environment variable, e.g. -MergeZoneRouteDbDsn `$env:MERGE_ZONE_ROUTE_DB_DSN")
    }
    if ($MergeZoneRouteDbDsn -ne "")      { $a += @("-route-db-dsn", $MergeZoneRouteDbDsn) }
    if ($MergeZoneWaitAofLocal -ge 0)     { $a += @("-waitaof-local", $MergeZoneWaitAofLocal) }
    if ($MergeZoneWaitAofReplicas -ge 0)  { $a += @("-waitaof-replicas", $MergeZoneWaitAofReplicas) }
    if ($MergeZoneWaitAofTimeoutMs -ge 0) { $a += @("-waitaof-timeout-ms", $MergeZoneWaitAofTimeoutMs) }
    # -take-over-fence 是库围栏的续跑接手:适用范围(只 merge / unmerge、只 db 源、run_id 形状)只由工具判,这里原样转发,
    # 给到别的命令时工具会点名拒绝,而不是被 ps1 静默丢掉。
    if ($MergeZoneTakeOverFence -ne "")   { $a += @("-take-over-fence", $MergeZoneTakeOverFence) }
    return $a
}

# Resolve-MergeFullPath 把清单路径按**调用者的**当前目录转成绝对路径。
# 必须在 Push-Location 进 tools/merge_zone 之前调用:清单是续跑 / 撤销 / 复核 / 放弃搬库的唯一凭据,
# 相对路径一旦按切换后的目录解释,就会在另一个地方新建一份空清单(= 把续跑当成全新的一次)。
function Resolve-MergeFullPath {
    param([Parameter(Mandatory = $true)][string]$Path)
    return [System.IO.Path]::GetFullPath($Path, $PWD.Path)
}

# Get-MergeZoneCommandArgs 返回某个 merge-zone 系列命令的完整参数(以 "run", "." 开头;
# Invoke-MergeZoneGo 去掉这个前缀后直接执行编译产物,见那里关于退出码的说明)。
# 与执行分开,是为了让契约测试(tests/dev_tools_merge_zone_contract.tests.ps1)不起 go、不连任何库
# 就能钉住「哪个命令转发哪些 flag」—— 这里漏转一个 flag 不会报错,只会让工具静默用默认值
# (如 relocate 的能力标记 zone 集合、verify 的清单),历史上这正是审计恒绿、撤销跑不起来的来源。
#
# 参数校验只做「缺了必然失败」的那几条,给出点名参数的中文报错;其余语义校验(模式组合、
# storage id 范围、清单是否 dry-run 预览)一律留给工具 —— 不在 ps1 里复制第二份业务规则。
function Get-MergeZoneCommandArgs {
    param(
        [Parameter(Mandatory = $true)][string]$MergeCommand,
        [Parameter(Mandatory = $true)][string]$RepoRoot
    )
    $intent = if ($DryRun) { "-dry-run" } else { "-apply" }
    $stamp = (Get-Date).ToUniversalTime().ToString("yyyyMMddTHHmmssZ")

    switch ($MergeCommand) {
        "merge-zone" {
            # 存量 player:zone 回填(合服前置,每个 zone 各跑一次)。合服之后源区带 merge:merged_into
            # 标记,工具会拒绝对它回填(否则映射丢失时会把人钉回源区,A10)。
            if ($MergeBackfillZone -gt 0) {
                return @("run", ".", "-backfill-home-zone", "-zone", $MergeBackfillZone) + @(Get-MergeZoneArgs) +
                       @(Get-MergeRouteSourceArgs -MergeCommand "merge-zone") + @($intent)
            }
            if ($MergeSourceZone -le 0 -or $MergeTargetZone -le 0) {
                throw "merge-zone requires -MergeSourceZone <id> and -MergeTargetZone <id> (or -MergeBackfillZone <id> for the backfill mode)"
            }
            # 清单必须落在仓库根(而不是 go run 的工作目录),而且路径要绝对。
            $manifest = if ($MergeManifestPath -ne "") { Resolve-MergeFullPath $MergeManifestPath }
                        else { Join-Path $RepoRoot ("merge_{0}_to_{1}_{2}.json" -f $MergeSourceZone, $MergeTargetZone, $stamp) }
            $a = @(
                "run", ".",
                "-source-zone", $MergeSourceZone,
                "-target-zone", $MergeTargetZone,
                "-manifest-path", $manifest
            )
            # -db-capability-zones 没有缺省值:pin(留空即工具默认 pin)必填,这里先点名拒绝,文案与工具一致。
            # copy 模式只在清单玩家已有落点记录时才需要,那要到工具立围栏之后才知道,交给工具判。
            if ($MergePlayerRowsMode -ne "copy" -and $MergeDbCapabilityZones -eq "") {
                throw ("merge-zone requires -MergeDbCapabilityZones (there is no default): list the ids of the zones whose go/db is running " +
                       "right now (comma-separated), or 'none' if no other zone is running. src/dst went down at T-0, so list only the zones " +
                       "still running; check dst's capability after its zone-up with -Command merge-zone-capability-check " +
                       "-MergeDbCapabilityZones <dst> before opening it (runbook §5.1 / §8 Step 6)")
            }
            $a += @(Get-MergeZoneArgs)
            $a += @(Get-MergeRouteSourceArgs -MergeCommand "merge-zone")
            # 模式只在显式给出时转发:留空 = 工具默认 pin,默认值只在工具里定义一处。能力标记 zone 给了就原样转发。
            if ($MergePlayerRowsMode -ne "")    { $a += @("-player-rows-mode", $MergePlayerRowsMode) }
            if ($MergeDbCapabilityZones -ne "") { $a += @("-db-capability-zones", $MergeDbCapabilityZones) }
            if ($MergeMigratePlayerBlobs)       { $a += "-migrate-player-blobs" }
            if ($MergeClearSourceHotState)      { $a += "-clear-source-hot-state" }
            if ($MergeAllowEmptySource)         { $a += "-allow-empty-source" }
            $a += @(Get-MergeKafkaGateArgs)
            $a += $intent
            return $a
        }
        "merge-zone-unmerge" {
            if ($MergeManifestPath -eq "") {
                throw "merge-zone-unmerge requires -MergeManifestPath <the manifest written by the original run>"
            }
            $manifest = Resolve-MergeFullPath $MergeManifestPath
            if (-not (Test-Path -LiteralPath $manifest)) { throw "manifest not found: $manifest" }
            $a = @("run", ".", "-mode", "unmerge", "-manifest-path", $manifest)
            $a += @(Get-MergeZoneArgs)
            $a += @(Get-MergeRouteSourceArgs -MergeCommand "merge-zone-unmerge")
            # 清单是 pin 模式时工具要求 -db-capability-zones(没有缺省值,可 none),copy 撤销给了才查。
            # 模式只在清单里,这里不读清单复制那条规则:给了就转发,没给时提示,由工具按清单拒绝。
            if ($MergeDbCapabilityZones -ne "") { $a += @("-db-capability-zones", $MergeDbCapabilityZones) }
            else {
                Write-Host ("merge-zone-unmerge: -MergeDbCapabilityZones not given. Unmerging a pin-mode merge requires it (there is no default): " +
                            "list the ids of the zones whose go/db is running right now, or 'none' if no other zone is running; " +
                            "merge_zone refuses before touching anything if the manifest is pin mode") -ForegroundColor Yellow
            }
            $a += @(Get-MergeKafkaGateArgs)
            $a += $intent
            return $a
        }
        "merge-zone-audit" {
            if ($MergeSourceZone -le 0 -or $MergeTargetZone -le 0) {
                throw "merge-zone-audit requires -MergeSourceZone <id> and -MergeTargetZone <id>"
            }
            $a = @(
                "run", ".",
                "-mode", "audit",
                "-source-zone", $MergeSourceZone,
                "-target-zone", $MergeTargetZone
            )
            $a += @(Get-MergeZoneArgs)
            # 审计也要:db 源下映射相关检查读库,合服后验证多一行 verify:route_cache(缓存与库逐人一致)。
            $a += @(Get-MergeRouteSourceArgs -MergeCommand "merge-zone-audit")
            if ($VerifyMerged) {
                # 合服后复核逐 id 核对清单玩家(A6),工具在没有清单时拒绝;这里先点名 ps1 参数报错。
                # 清单存在性不在这里查:清单缺失 / 是 dry-run 预览由工具报 INFRA(exit 2),退出码语义以工具为准。
                if ($MergeManifestPath -eq "") {
                    throw "merge-zone-audit -VerifyMerged requires -MergeManifestPath <the manifest written by the merge being verified>"
                }
                $a += @("-verify-merged", "-manifest-path", (Resolve-MergeFullPath $MergeManifestPath))
            }
            return $a
        }
        "merge-zone-pin-placement" {
            if ($MergePinPlacementZone -le 0) {
                throw "merge-zone-pin-placement requires -MergePinPlacementZone <zone id>"
            }
            $a = @("run", ".", "-mode", "pin-placement", "-zone", $MergePinPlacementZone)
            $a += @(Get-MergeZoneArgs)
            $a += @(Get-MergeRouteSourceArgs -MergeCommand "merge-zone-pin-placement")
            $a += $intent
            return $a
        }
        "merge-zone-relocate" {
            if ($MergeRelocateSourceStorage -le 0 -or $MergeRelocateTargetStorage -le 0) {
                throw "merge-zone-relocate requires -MergeRelocateSourceStorage <storage id> and -MergeRelocateTargetStorage <storage id>"
            }
            # 工具没有缺省值,搬库也不接受 none;这里先点名,文案与工具一致(none 由工具拒绝,不在这里复制规则)。
            # 漏掉在跑的 zone = 那个 zone 的 go/db 若还是旧版,会按本进程 zone 选库,把已搬走的玩家写回源库。
            if ($MergeDbCapabilityZones -eq "") {
                throw ("merge-zone-relocate requires -MergeDbCapabilityZones: list every zone whose go/db is running (comma-separated " +
                       "zone ids, e.g. 1,2,3); 'none' is not accepted - a relocation runs online, every running zone must carry the marker")
            }
            $manifest = if ($MergeManifestPath -ne "") { Resolve-MergeFullPath $MergeManifestPath }
                        else { Join-Path $RepoRoot ("relocate_{0}_to_{1}_{2}.json" -f $MergeRelocateSourceStorage, $MergeRelocateTargetStorage, $stamp) }
            $a = @(
                "run", ".",
                "-mode", "relocate",
                "-relocate-source-storage", $MergeRelocateSourceStorage,
                "-relocate-target-storage", $MergeRelocateTargetStorage,
                "-db-capability-zones", $MergeDbCapabilityZones,
                "-manifest-path", $manifest
            )
            if ($MergeRelocatePlayerIds -ne "") { $a += @("-relocate-player-ids", $MergeRelocatePlayerIds) }
            if ($MergeRelocateBatchSize -gt 0)  { $a += @("-relocate-batch-size", $MergeRelocateBatchSize) }
            if ($MergeRelocateLockWait -ne "")  { $a += @("-relocate-lock-wait", $MergeRelocateLockWait) }
            # 公共参数里的 -kafka-topic-generation 决定 R2 等的排序锁键(db_task_zone_<home>[_g<gen>]),
            # 现读 db.yaml;-table-list-json 决定搬哪些玩家表;-notice-redis-* 是排序锁所在的共享 DB 0。
            $a += @(Get-MergeZoneArgs)
            $a += @(Get-MergeRouteSourceArgs -MergeCommand "merge-zone-relocate")
            $a += $intent
            return $a
        }
        "merge-zone-relocate-abort" {
            if ($MergeManifestPath -eq "") {
                throw "merge-zone-relocate-abort requires -MergeManifestPath <the manifest written by the relocate run>"
            }
            $manifest = Resolve-MergeFullPath $MergeManifestPath
            if (-not (Test-Path -LiteralPath $manifest)) { throw "manifest not found: $manifest" }
            $a = @("run", ".", "-mode", "relocate-abort", "-manifest-path", $manifest)
            if ($MergeRelocateSourceStorage -gt 0) { $a += @("-relocate-source-storage", $MergeRelocateSourceStorage) }
            if ($MergeRelocateTargetStorage -gt 0) { $a += @("-relocate-target-storage", $MergeRelocateTargetStorage) }
            if ($MergeRelocateBatchSize -gt 0)     { $a += @("-relocate-batch-size", $MergeRelocateBatchSize) }
            $a += @(Get-MergeZoneArgs)
            $a += @(Get-MergeRouteSourceArgs -MergeCommand "merge-zone-relocate-abort")
            $a += $intent
            return $a
        }
        "merge-zone-capability-check" {
            # 只读:逐 zone 报告能力标记 present / missing / unreadable,工具以 0 / 1 / 2 退出(与审计同口径)。
            # 合服后 dst zone-up、开服之前必跑(runbook §8 Step 6)。none 由工具拒绝。
            if ($MergeDbCapabilityZones -eq "") {
                throw "merge-zone-capability-check requires -MergeDbCapabilityZones <zone ids to check>, e.g. the merge target right after its zone-up"
            }
            # 本命令不读写映射、不持围栏,不调用 Get-MergeRouteSourceArgs:给了 -MergeZoneTakeOverFence 就拒绝,不静默丢掉。
            if ($MergeZoneTakeOverFence -ne "") {
                throw "merge-zone-capability-check does not hold a merge fence; -MergeZoneTakeOverFence only applies to merge-zone / merge-zone-unmerge"
            }
            $a = @("run", ".", "-mode", "capability-check", "-db-capability-zones", $MergeDbCapabilityZones)
            $a += @(Get-MergeZoneArgs)
            return $a
        }
        "merge-zone-storage-audit" {
            if ($MergeAuditStorage -le 0) {
                throw "merge-zone-storage-audit requires -MergeAuditStorage <storage id>"
            }
            # 只读模式,工具不要求 -dry-run / -apply,所以这里不转发写意图。
            $a = @("run", ".", "-mode", "storage-audit", "-storage", $MergeAuditStorage)
            $a += @(Get-MergeZoneArgs)
            # 决定库能否退役:db 源下一律读库,不按缓存下结论。
            $a += @(Get-MergeRouteSourceArgs -MergeCommand "merge-zone-storage-audit")
            return $a
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

# Invoke-MergeZoneGo 在 tools/merge_zone 里编译并执行一个 merge-zone 系列命令,**以工具自己的退出码结束
# 当前脚本(不返回)**。
# ⚠️ tools/merge_zone 是**独立 go module**:必须在那个目录里编译。旧代码在仓库根跑
# `go run ./tools/merge_zone`,结果恒为 "go: cannot find main module" —— 那个入口从来没有真正执行过一次合服。
# 参数(含清单绝对路径)在 Push-Location 之前组好,相对路径才按调用者的目录解释。
#
# 退出码为什么要这样透传(runbook §7.1):
#   1) 不能用 `go run`:它把子进程的任何非零退出码都压成 1(只往 stderr 打一行 `exit status N`),
#      审计的 1(有 block)与 2(INFRA,没查成、结论不可信)从此分不开。所以先 build 再直接执行产物。
#   2) 必须显式 `exit`:`pwsh -File dev_tools.ps1 ...` 在脚本正常结束时进程退出码恒为 0 ——
#      合服 apply 半途失败、storage-audit 连不上库,调用方拿到的都是「成功」。
#   3) 编译失败时工具一行都没跑,退出码按 2(没做成 / 没查成、结论不可信)给出,不与工具的 1(block / 失败)混用。
# 不经管道接工具输出:管道会让 pwsh 按控制台代码页重解码 Go 写出的 UTF-8,中文报告会变乱码。
function Invoke-MergeZoneGo {
    param([Parameter(Mandatory = $true)][string]$MergeCommand)
    $repoRoot = (Resolve-Path (Join-Path $ScriptDir "..\..")).Path
    try {
        $goArgs = @(Get-MergeZoneCommandArgs -MergeCommand $MergeCommand -RepoRoot $repoRoot)
    }
    catch {
        #   4) merge-zone-capability-check 的 1 是业务结论(有 missing)。参数没给齐(漏 -MergeDbCapabilityZones 等)时一个标记都没查,
        #      属于「没查成」,按 2 退出,不与 1 混用(09-28 遗留;已 grep 全部调用方,没有依赖这里退出 1 的,runbook §8 Step 6 按 2 处理)。
        #      其余命令的参数错误仍照旧往上抛(pwsh -File 下为 1 = 拒绝、什么都没做),语义不变。
        if ($MergeCommand -eq "merge-zone-capability-check") {
            [Console]::Error.WriteLine("ERROR: $($_.Exception.Message); the tool was NOT run, no capability marker was checked (exit 2)")
            exit 2
        }
        throw
    }
    # Get-MergeZoneCommandArgs 仍以 "run", "." 开头(契约测试按这个形状钉 flag);执行时去掉这个前缀。
    if ($goArgs.Count -lt 2 -or [string]$goArgs[0] -ne "run" -or [string]$goArgs[1] -ne ".") {
        throw "Invoke-MergeZoneGo: unexpected argument shape for '$MergeCommand' (must start with 'run', '.')"
    }
    $toolArgs = @($goArgs | Select-Object -Skip 2)
    # 工具非零退出是业务结论(block / INFRA),不是脚本异常;防 profile 里打开了这个开关把它变成 throw(恒为 1)。
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

# Get-K8sRouteToolArgs 组装 tools/scripts/k8s_route_tool.ps1 的 hashtable 参数(D1 第 1b-3 批,§13.4 第 3 条)。
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

# Invoke-K8sRouteTool 调 k8s_route_tool.ps1 并**以它的退出码结束当前脚本**:route-tool 容器的 0 干净 / 1 要人工处理 / 2 失败 /
# 3 有生效的合服围栏,必须原样到达调用方(`pwsh -File dev_tools.ps1 ...` 在脚本正常结束时进程退出码恒为 0,见 Invoke-MergeZoneGo)。
# 本入口自己的失败(没给子命令、k8s_route_tool.ps1 参数绑定失败、未捕获的异常)一律 2,与 k8s_route_tool.ps1「本脚本自己失败一律 2」
# 同口径:异常往上冒时 pwsh -File 的退出码是 1,会与「完成但需要人工处理」撞号(review B3-7 / OPS-5)。
function Invoke-K8sRouteTool {
    try {
        if (@($RouteToolArgs).Count -eq 0) {
            throw "k8s-route-tool requires -RouteToolArgs <subcommand and its arguments>, e.g. -RouteToolArgs @('audit', '-frozen-recovery') (use the -Command form of pwsh for arrays)"
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
        stress environment and checks DBSIZE=0 (-InfraNamespace must match ^mmorpg-stress-). See deploy/k8s/README.md.
    -Command k8s-zone-up | k8s-zone-down | k8s-zone-status
    -Command k8s-all-up | k8s-all-down | k8s-all-status
        k8s-infra-up / k8s-zone-up / k8s-all-up pass the D1 route switches through to
        k8s_deploy.ps1 unchanged: [-RouteStoreMode redis|db_first] [-RouteReconcileArm]
        [-RouteReadRequiresReady] [-DbPlacementRequireRouteReady] [-GuildRequireRouteReady]
        [-ConfirmRouteSwitchChange]. k8s_deploy.ps1 defaults to db_first with every switch on
        (the global default; deploying Go services accepts nothing else). They are sticky: a
        cluster still on redis / off is refused before any write unless -ConfirmRouteSwitchChange
        is given; zones not deployed in this run must already be db_first + Arm (use k8s-all-up).
        k8s-zone-rollback refuses these switches (its Step 6 redeploys with the defaults).
    -Command k8s-exposure-preflight
    -Command k8s-stage-runtime
    -Command k8s-image-preflight | k8s-build-image | k8s-push-image | k8s-release-zone | k8s-release-all
    -Command k8s-route-tool -ZoneName <zone> -RouteToolArgs @('<subcommand>', ...) [-RouteToolTimeoutSeconds <s>]
            [-RouteToolImage <image>] [-RouteToolReportDir <dir>] [-DryRun] [-KubeContext <ctx>]
        Run data_service's route-tool (import-from-redis / audit / freeze / reconcile-from-cache /
        unfreeze / tidb-prepare / rewarm) as a one-off K8s Job via tools/scripts/k8s_route_tool.ps1;
        exits with the container's code (0 clean / 1 needs attention / 2 failed / 3 merge fence held).

Zone merge (合服) commands — full SOP: docs/ops/merge-zone-runbook.md
    Every merge-zone* command exits with merge_zone's own exit code (the
    tool is built then run directly, not via `go run`). If merge_zone fails
    to build, nothing runs and the exit code is 2.
    Every merge-zone* command except merge-zone-capability-check REQUIRES
    -MergeZoneRouteSource db|redis (no default, never inferred from yaml):
        db    data_service runs RouteStore.Mode=db_first; also pass
              -MergeZoneRouteDbDsn <global DB DSN> (from an env var) and, on
              redis-ha, -MergeZoneWaitAofReplicas 1.
        redis only while every data_service still runs RouteStore.Mode=redis.
    The tool refuses when the route DB and the mapping Redis are not a pair
    (mapping:route:watermark). Runbook §15.
    -MergeZoneTakeOverFence <run_id> (merge-zone / merge-zone-unmerge with
    -MergeZoneRouteSource db only): resume a run that aborted mid-write and
    left its DB merge fence in place, after confirming that run's process
    is gone; the fence moves to the new run in one DB transaction. Do NOT
    DEL merge:in_progress:* under db: the DB fence is the source of truth.
    Runbook §15.4.
    -Command merge-zone -MergeBackfillZone <id> [-DryRun]
        PREREQUISITE, run once per existing zone BEFORE the first ever merge:
        seeds player:zone:{id} (SET NX) from zone_<id>_db.player_database.
        Players without that mapping are silently left behind by a merge.
    -Command merge-zone-audit -MergeSourceZone <s> -MergeTargetZone <d>
        Read-only pre-merge gates (scene nodes / online / locks / kafka queues).
        Exit 0=clean, 1=blocking finding, 2=the audit itself could not run.
    -Command merge-zone -MergeSourceZone <s> -MergeTargetZone <d> [-DryRun]
            [-MergePlayerRowsMode pin|copy] -MergeDbCapabilityZones <z1,z2,...|none>
        Guild MySQL/ZSET/cache + player:zone remap (manifest players only) +
        post-merge notice flags. Player main data: pin (default) re-homes
        without copying rows and pins placement "<s>:1"; needs every
        -MergeDbCapabilityZones zone to run a placement-aware go/db.
        -MergeDbCapabilityZones has NO default (pin requires it): at T-0 src
        and dst are already down, so list only the zones still running, or
        'none' if no other zone runs; then check dst with
        merge-zone-capability-check after its zone-up, before opening it.
        copy = old go/db: copies rows zone_<s>_db -> zone_<d>_db (needs
        -MergeDbCapabilityZones only if manifest players have a placement). Writes a JSON manifest BEFORE the first
        write (-MergeManifestPath); -DryRun only writes <path>.dryrun.json.
        -DryRun first, ALWAYS; record players_in_source and pass it back as
        -MergeExpectedSrcPlayers on the real run.
        After a pin merge the source zone DB stays the source of truth for
        pinned players: relocate them before retiring it.
    -Command merge-zone-audit ... -VerifyMerged -MergeManifestPath <file> -MergeExpectedSrcPlayers <n>
        Post-merge assertions (runbook Step 5), checked player by player
        against the manifest.
    -Command merge-zone-unmerge -MergeManifestPath <file> [-DryRun]
            (-MergeKafkaConsumerGroupsCmd <cli> [-MergeKafkaBootstrap <b>] | -MergeAssumeKafkaDrained)
            [-MergeDbCapabilityZones <z1,z2,...|none>]
        Reverse one run, object by object. Target-zone natives are untouched.
        Runs the pre-flight gates against the TARGET zone first. A pin-mode
        manifest requires -MergeDbCapabilityZones (still-running zones, or none).
    -Command merge-zone-capability-check -MergeDbCapabilityZones <z1,z2,...>
        Read-only: per zone, is db:capability:zone:<z> = placement-routing-v1?
        Exit 0=all present, 1=some missing, 2=some unreadable. Run it for dst
        after its zone-up and BEFORE opening it ('none' is refused).
    Common: -MergeSkipGuildMySql / -MergeSkipGuildRank / -MergeSkipTradeMySql
        ONLY where that service was never deployed; merge, unmerge and audit
        must use the same switches (audit skips guild only when both are set).

Player storage placement commands — docs/design/player-storage-placement.md
    -Command merge-zone-pin-placement -MergePinPlacementZone <z> [-DryRun]
        Pin "<z>:1" for players with home_zone=<z> and no placement record.
        Safe online, idempotent. Order: login PinOnCreate -> every zone ->
        only then go/db Placement.Required.
    -Command merge-zone-relocate -MergeRelocateSourceStorage <S> -MergeRelocateTargetStorage <T>
            -MergeDbCapabilityZones <every running zone, 'none' refused> [-MergeRelocatePlayerIds <id,id>]
            [-MergeRelocateBatchSize <n>] [-MergeRelocateLockWait <dur>] [-MergeManifestPath <file>] [-DryRun]
        Freeze -> wait in-flight writes -> copy -> byte-compare -> switch.
        Storage id 1..999999 = zone_<id>_db, >=1000000 = player_store_<id>_db.
        Resume = re-run with the same -MergeManifestPath.
    -Command merge-zone-relocate-abort -MergeManifestPath <file> [-DryRun]
        Unfreeze everything that relocate run left frozen; the manifest
        cannot be resumed afterwards.
    -Command merge-zone-storage-audit -MergeAuditStorage <id>
        Read-only: who is still placed on that storage + its cold copies.
        Retire a DB only on "RETIREMENT CHECK: nothing routes to storage"
        with unmapped = unreadable = 0. Exit 0=done, 2=infra failure.

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
        # Drives tools/merge_zone/main.go directly (go build + run the binary,
        # exit code passed through; see Invoke-MergeZoneGo). The earlier
        # implementation invoked a `merge_zone.ps1` wrapper that never
        # actually existed in the repo. See docs/ops/merge-zone-runbook.md §5.
        #
        # 两种子模式共用这一个命令(撤销由 -Command merge-zone-unmerge 承担):
        #   -MergeBackfillZone <id>   存量 player:zone 回填(合服前置,每个 zone 各跑一次)
        #   其余                       正常合服(默认 pin 模式,见 -MergePlayerRowsMode)
        # 参数组装见 Get-MergeZoneCommandArgs,执行目录见 Invoke-MergeZoneGo。
        Invoke-MergeZoneGo -MergeCommand "merge-zone"
    }
    "merge-zone-unmerge" {
        # 按清单逐对象撤销一次合服(目标区原住民不受影响)。
        # 只在「刚合完、还没开服」的窗口里用;开服之后请走备份还原。
        # 工具先按目标区口径跑 P2~P7 门禁,所以同样要 -MergeKafkaConsumerGroupsCmd 或 -MergeAssumeKafkaDrained。
        Invoke-MergeZoneGo -MergeCommand "merge-zone-unmerge"
    }
    "merge-zone-audit" {
        # Read-only inspection — see tools/merge_zone/audit_resources.go and
        # docs/ops/merge-zone-runbook.md §4.2. Reports per-resource counts +
        # conflicts; never writes. Safe to run any time, even on a live zone.
        # T-1 day uses default mode; post-merge verification uses -VerifyMerged
        # (which requires -MergeManifestPath).
        #
        # 退出码:0=干净 / 1=有 block 级发现(不许合服)/ 2=审计自己没跑成
        # (连不上某个库),结论不可信 —— 不要把 2 当成 1 处理。
        Invoke-MergeZoneGo -MergeCommand "merge-zone-audit"
    }
    "merge-zone-pin-placement" {
        # 批量钉落点(player-storage-placement.md §10.3):home==N 且无记录的玩家钉 "N:1",在线安全、幂等。
        # 顺序:先开 login 的 Placement.PinOnCreate,再对全部 zone 跑本命令,最后才能开 go/db 的 Placement.Required。
        Invoke-MergeZoneGo -MergeCommand "merge-zone-pin-placement"
    }
    "merge-zone-relocate" {
        # 冻结式搬库(§9,R0–R5)。崩溃后用同一 -MergeManifestPath 重跑即续跑;
        # 放弃用 -Command merge-zone-relocate-abort,之后该清单不能再续跑。
        Invoke-MergeZoneGo -MergeCommand "merge-zone-relocate"
    }
    "merge-zone-relocate-abort" {
        # 把某次搬库留下的冻结全部解回稳定态(按清单)。
        Invoke-MergeZoneGo -MergeCommand "merge-zone-relocate-abort"
    }
    "merge-zone-storage-audit" {
        # 只读:谁的有效落点仍是这个库、库里有多少冷副本。退出码 0=查成 / 2=基础设施失败。
        Invoke-MergeZoneGo -MergeCommand "merge-zone-storage-audit"
    }
    "merge-zone-capability-check" {
        # 只读:逐 zone 核对 go/db 能力标记。退出码 0=全部 present / 1=有 missing / 2=有 unreadable(结论不可信)。
        # 合服后 dst zone-up 之后、开服之前必跑,非 0 不许开服(runbook §8 Step 6)。
        Invoke-MergeZoneGo -MergeCommand "merge-zone-capability-check"
    }
    "kafka-offset-reset" {
        # Wraps kafka_offset_reset.ps1 — see docs/design/zone_data_rollback.md §3 step 4.
        # Used during zone rollback to reset db_task_zone_<id> offsets before restarting a zone,
        # and to finalize a db_task consumer-group handoff (-KafkaFinalizeHandoff / -KafkaRetireGroup).
        # 必须 hashtable splatting:以前是数组 splatting,PowerShell 把数组元素当**位置参数**依次绑定,
        # "-Topic" 后面的 topic 名会撞上 [int]$Partitions 直接类型转换失败(与 k8s_zone_rollback.ps1 Step 5 注释里记录的是同一个 bug),
        # 本地回档 / 重置路径因此从来跑不通。字符串非空才放,switch 只在被指定时放,数值只在偏离缺省时放。
        $kArgs = @{
            BootstrapServer = $KafkaBootstrapServer
            Topic           = $KafkaTopic
        }
        if ($KafkaGroup -ne "")               { $kArgs.Group = $KafkaGroup }
        if ($KafkaRetireGroup -ne "")         { $kArgs.RetireGroup = $KafkaRetireGroup }
        if ($KafkaToDatetime -ne "")          { $kArgs.ToDatetime = $KafkaToDatetime }
        if ($KafkaToEarliest)                 { $kArgs.ToEarliest = $true }
        if ($KafkaToLatest)                   { $kArgs.ToLatest = $true }
        if ($KafkaDeleteAndRecreate)          { $kArgs.DeleteAndRecreateTopic = $true }
        if ($KafkaFinalizeHandoff)            { $kArgs.FinalizeHandoff = $true }
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
        #             → Kafka offsets to the log end (zone group; retire legacy offsets) → discard post-TargetTime Redis state
        #             → verify → zone-up → verification checklist.
        if ($RollbackTargetTime -eq "") {
            throw "k8s-zone-rollback requires -RollbackTargetTime (UTC, e.g. '2026-05-15T14:23:00Z')"
        }
        # 已废弃的两个参数在调用回滚脚本之前拒绝,不静默吞掉(AGENTS §11.3)。
        if ($RollbackConfirmFlushSharedRedis) {
            throw "k8s-zone-rollback 不再支持 -RollbackConfirmFlushSharedRedis:回档已不做 FLUSHDB(go/db 游标、重试收据、映射、账号目录与各 zone 缓存同在一个 Redis DB,FLUSHDB 等于事故),改由 zone_rollback_discard 只丢弃 TargetTime 之后的状态;整套环境重建走 AGENTS.md §6.2。去掉该参数重跑;尚未做任何操作。"
        }
        if ($RollbackRedisPassword -ne "") {
            throw "k8s-zone-rollback 不再支持 -RollbackRedisPassword:口令经命令行会进进程列表与 shell 历史。把口令放进环境变量,改传 -RollbackRedisPasswordEnv <变量名>;尚未做任何操作。"
        }
        # 回滚目标版本必须显式给。默认的 git 短 sha 是**当前工作树**的版本,
        # 拿它回滚等于"把数据回档到过去、把代码留在现在",比不回滚更危险。
        if (-not $script:NodeImageExplicit) {
            throw "k8s-zone-rollback requires an explicit -NodeImage (回滚目标版本的镜像引用,如 ghcr.io/luyuancpp/mmorpg-node:<回滚目标 sha>)。默认值是当前工作树的 sha,不是回滚目标。"
        }
        # 回档的 Step 6 固定传 -DbConsumerGroup zone(docs/design/db-consumer-group-per-zone.md §6),本参数在这条路上不生效;
        # 显式给了就在调用回档脚本之前拒绝,不静默吞掉(AGENTS §11.3)。
        if (-not [string]::IsNullOrWhiteSpace($DbConsumerGroup)) {
            throw "k8s-zone-rollback 不支持 -DbConsumerGroup:回档的 Step 6 固定使用 zone(回档后落到 zone 组的最终形状),给的 '$DbConsumerGroup' 不会生效。去掉该参数重跑;尚未做任何操作。"
        }
        # D1 映射开关同理:k8s_zone_rollback.ps1 不声明它们,Step 6 按 k8s_deploy 缺省(db_first / 全 true)重建 zone。
        # 尤其 -ConfirmRouteSwitchChange 被静默丢掉时,Step 6 要到 namespace 已删、库已回档之后才被粘滞核对拒绝(review B3-1 / OPS-3)。
        $rollbackRouteSwitches = @()
        if (-not [string]::IsNullOrWhiteSpace($RouteStoreMode)) { $rollbackRouteSwitches += '-RouteStoreMode' }
        if ($RouteReconcileArm) { $rollbackRouteSwitches += '-RouteReconcileArm' }
        if ($RouteReadRequiresReady) { $rollbackRouteSwitches += '-RouteReadRequiresReady' }
        if ($DbPlacementRequireRouteReady) { $rollbackRouteSwitches += '-DbPlacementRequireRouteReady' }
        if ($GuildRequireRouteReady) { $rollbackRouteSwitches += '-GuildRequireRouteReady' }
        if ($ConfirmRouteSwitchChange) { $rollbackRouteSwitches += '-ConfirmRouteSwitchChange' }
        if ($rollbackRouteSwitches.Count -gt 0) {
            throw "k8s-zone-rollback 不支持 $($rollbackRouteSwitches -join ', '):回档脚本不透传 D1 映射开关,Step 6 按 k8s_deploy.ps1 缺省(db_first / 全 true)重建 zone,给的值不会生效。线上 D1 开关不是全球默认的集群先用 k8s-all-up 切好再回档;去掉这些参数重跑;尚未做任何操作。"
        }
        # 按用途拆实例(G4a 第 1a 批):回档脚本只声明 -RedisSplit(Step 6 的 zone-up 用它),不声明确认开关与容量 ——
        # Step 1 已删掉 namespace,切换闸在 Step 6 无从触发;回档不 infra-up,容量不生效。显式给了就拒绝,不静默吞掉(AGENTS §11.3)。
        $rollbackRedisSplitExtras = @()
        if ($ConfirmRedisSplitSwitch) { $rollbackRedisSplitExtras += '-ConfirmRedisSplitSwitch' }
        if (-not [string]::IsNullOrWhiteSpace($RedisQueueMaxMemory)) { $rollbackRedisSplitExtras += '-RedisQueueMaxMemory' }
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
            KafkaBootstrap       = $KafkaBootstrapServer
            KafkaTopic           = $RollbackKafkaTopic
            NodeImage            = $NodeImage
            NamespacePrefix      = $NamespacePrefix
        }
        # 消费组留空不传,由回滚脚本按 go/db/etc/db.yaml 推导(zone 组 / legacy 组),默认值只在那一处。
        if ($RollbackKafkaGroup -ne "")       { $rbArgs.KafkaGroup = $RollbackKafkaGroup }
        if ($RollbackKafkaLegacyGroup -ne "") { $rbArgs.KafkaLegacyGroup = $RollbackKafkaLegacyGroup }
        if ($RollbackRedisHost -ne "")     { $rbArgs.RedisHost = $RollbackRedisHost }
        if ($RollbackRedisPort -ne "6379") { $rbArgs.RedisPort = $RollbackRedisPort }
        if ($RollbackRedisDB -ne 0)        { $rbArgs.RedisDB = $RollbackRedisDB }
        if ($RollbackSkipMySqlPause)       { $rbArgs.SkipMySqlPause = $true }
        # 回档丢弃语义的新参数(2026-10-18):非空 / 大于 0 才转发,默认值与校验只在回滚脚本一处。
        if ($RollbackRedisPasswordEnv -ne "")  { $rbArgs.RedisPasswordEnv = $RollbackRedisPasswordEnv }
        if ($RollbackMySqlDsnEnv -ne "")       { $rbArgs.MySqlDsnEnv = $RollbackMySqlDsnEnv }
        if ($RollbackDiscardTool -ne "")       { $rbArgs.RollbackDiscardTool = $RollbackDiscardTool }
        if ($RollbackBarrierProtocol -ne "")   { $rbArgs.BarrierProtocol = $RollbackBarrierProtocol }
        if ($RollbackId -ne "")                { $rbArgs.RollbackId = $RollbackId }
        if ($RollbackQuiesceSeconds -gt 0)     { $rbArgs.QuiesceSeconds = $RollbackQuiesceSeconds }
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
