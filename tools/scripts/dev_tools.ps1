param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("help", "pbgen-build", "pbgen-run", "proto-gen-build", "proto-gen-run", "tree", "naming-audit", "naming-apply", "third-party-grpc-build", "no-raw-pointer-setup", "iwyu-run", "k8s-infra-up", "k8s-infra-down", "k8s-infra-status", "k8s-zone-up", "k8s-zone-down", "k8s-zone-status", "k8s-zone-rollback", "k8s-all-up", "k8s-all-down", "k8s-all-status", "k8s-build-all", "k8s-exposure-preflight", "k8s-stage-runtime", "k8s-image-preflight", "k8s-build-image", "k8s-push-image", "k8s-release-zone", "k8s-release-all", "go-svc-start", "go-svc-start-exe", "go-svc-stop", "go-svc-status", "go-svc-list", "go-svc-build", "go-svc-build-images", "go-svc-push-images", "java-svc-build-image", "java-svc-push-image", "cpp-node-start", "cpp-node-stop", "cpp-node-status", "cpp-node-list", "dev-start", "dev-start-exe", "dev-start-zones", "dev-stop", "dev-status", "dev-robot-zones", "merge-zone", "merge-zone-audit", "merge-zone-unmerge", "merge-zone-relocate", "merge-zone-relocate-abort", "merge-zone-pin-placement", "merge-zone-storage-audit", "merge-zone-capability-check", "kafka-offset-reset", "git-stats")]
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
    # -LoginDevPasswordAuth、-AllowDisruptiveSwitch、显式给了的 -ReleaseProfile 与 -KubeContext / -KubeConfig
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
    # Cap = 32 per service.
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
    #   guild   DB 2   guild_rank:zone / guild:v2 / guild_rank:maintenance_lock
    #   login   DB 0   player_merge_notice / player:session / kafka:retry|dead
    # friend 的 DB 3 已退役(friend 移植:F2 删读者、F3 删参数):friend:online 从来没有写者,读者也已删除,
    # 好友在线状态改为读共享库的契约 key player:session:{id}(login 段那一行)。
    # 所以这里不再有 friend 的 Redis 参数;好友数据在独占库 mmorpg_friend,归 merge_zone 的 MySQL 审计管。
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
    # 清单 / 复核 / 撤销 / 搬库续跑与放弃。合服与搬库留空时在仓库根按时间戳生成;
    # -DryRun 从不写它,只写 <path>.dryrun.json 预览(apply / unmerge / verify 都拒读预览)。
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
    # 转给 k8s_zone_rollback.ps1 -ConfirmNoGatewayIngress:显式声明回滚后该 zone 不要 Ingress gateway(Ingress 核对的显式决定,
    # 与 -GatewayIngressHost 互斥)。switch 不占位置参数,插在这里不影响后面参数的位置绑定。
    [switch]$RollbackConfirmNoGatewayIngress,
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

        go build -v -o $legacyProtoGenExePath ./cmd
        # go build 是外部命令,失败不会触发 $ErrorActionPreference = "Stop"。不在这里拦,下一行就会把上一次的
        # 旧 pbgen.exe 当新产物复制过去、脚本以 0 退出,dev.bat 接着用旧生成器改写代码。
        # 2026-09-20 就这样吞掉了 scene_node_service.cpp 的手写守护段。构建失败必须让整条 proto 流程停下。
        if ($LASTEXITCODE -ne 0) { throw "proto generator build failed (go build exit $LASTEXITCODE); stale pbgen.exe / proto-gen.exe left untouched, do not run proto-gen-run" }
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
    if (-not [string]::IsNullOrWhiteSpace($RequireClientEndpoint)) { $args.RequireClientEndpoint = $RequireClientEndpoint }
    if ($AllowDisruptiveSwitch) { $args.AllowDisruptiveSwitch = $true }

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
    # 集群外入口参数,口径同 Invoke-K8sDeploy(k8s_image.ps1 原样转给 k8s_deploy.ps1)。
    # -Gateway* / -LoginDevPasswordAuth 不在此列,已在函数开头拒绝(k8s_image.ps1 也不声明它们)。
    if (-not [string]::IsNullOrWhiteSpace($ClientEntryMode)) { $args.ClientEntryMode = $ClientEntryMode }
    if (-not [string]::IsNullOrWhiteSpace($ClientPublicHost)) { $args.ClientPublicHost = $ClientPublicHost }
    if (-not [string]::IsNullOrWhiteSpace($GateClientHostTemplate)) { $args.GateClientHostTemplate = $GateClientHostTemplate }
    if ($GateNodePortBase -ne -1) { $args.GateNodePortBase = $GateNodePortBase }
    if (-not [string]::IsNullOrWhiteSpace($GateExternalTrafficPolicy)) { $args.GateExternalTrafficPolicy = $GateExternalTrafficPolicy }
    if (-not [string]::IsNullOrWhiteSpace($BattleOrchestrator)) { $args.BattleOrchestrator = $BattleOrchestrator }
    # release-zone / release-all 同样重新生成 gate(release-all 还有 battle),切换形态的确认闸同一口径透传。
    if ($AllowDisruptiveSwitch) { $args.AllowDisruptiveSwitch = $true }
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
    if ($MergeSceneRedisAddr -ne "")     { $a += @("-scene-redis-addr", $MergeSceneRedisAddr) }
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
                return @("run", ".", "-backfill-home-zone", "-zone", $MergeBackfillZone) + @(Get-MergeZoneArgs) + @($intent)
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
            $a += $intent
            return $a
        }
        "merge-zone-capability-check" {
            # 只读:逐 zone 报告能力标记 present / missing / unreadable,工具以 0 / 1 / 2 退出(与审计同口径)。
            # 合服后 dst zone-up、开服之前必跑(runbook §8 Step 6)。none 由工具拒绝。
            if ($MergeDbCapabilityZones -eq "") {
                throw "merge-zone-capability-check requires -MergeDbCapabilityZones <zone ids to check>, e.g. the merge target right after its zone-up"
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
    $goArgs = @(Get-MergeZoneCommandArgs -MergeCommand $MergeCommand -RepoRoot $repoRoot)
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
    -Command no-raw-pointer-setup [-LlvmRoot <path>] [-DownloadOnly] [-DryRun]
        自动下载 LLVM/Clang 开发库并串行编译裸指针成员检查器。
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
    Every merge-zone* command exits with merge_zone's own exit code (the
    tool is built then run directly, not via `go run`). If merge_zone fails
    to build, nothing runs and the exit code is 2.
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
    "pbgen-run" { Invoke-ProtoGenRun }
    "proto-gen-build" { Invoke-ProtoGenBuild }
    "proto-gen-run" { Invoke-ProtoGenRun }
    "tree" { Invoke-Tree }
    "naming-audit" { Invoke-NamingAudit }
    "naming-apply" { Invoke-NamingApply }
    "third-party-grpc-build" { Invoke-ThirdPartyGrpcBuild }
    "no-raw-pointer-setup" {
        & (Join-Path $ScriptDir "third_party/setup_no_raw_pointer_check.ps1") -LlvmRoot $LlvmRoot -DownloadOnly:$DownloadOnly -DryRun:$DryRun
    }
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
        if (-not [string]::IsNullOrWhiteSpace($GateRouterMode))            { $rbArgs.GateRouterMode = $GateRouterMode }
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
