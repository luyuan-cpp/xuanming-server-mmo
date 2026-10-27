param(
	[Parameter(Mandatory = $true)]
	# infra-kafka-topics:只重跑审计 topic + 控制面命令 topic 的预建 Job(Apply-KafkaTopicInitJob)。
	# Kafka 换成 StatefulSet + PVC 之后(R06),普通重启不再丢 topic;仍然保留这条止血路径,
	# 因为**第一次从旧 emptyDir Deployment 切过来**、`infra-down` 删过 namespace(连 PVC 一起删)、
	# 或者有人手工删过 topic 之后,topic 都是空的,而 scene 一发消息就会把它自动建成 1 分区。
	# 不过发布门禁(与 *-down / *-status 同列),见 deploy/k8s/README.md「Kafka:StatefulSet + PVC」。
	# redis-split-status / redis-wipe:共享 Redis 按用途拆实例(G4a 第 1a 批)的运维命令,同样不过发布门禁。前者只读,对 zones 配置里每个 zone
	# 打印 login / gateway 的排队 Redis 地址与 rollout 状态,不一致或没滚完以退出码 1 结束;后者清空压测环境里的每个 Redis 实例并核对 DBSIZE=0
	# (要求 -InfraNamespace 匹配 ^mmorpg-stress- 且 -ConfirmWipeAllRedis)。见 Show-RedisSplitStatus / Invoke-RedisWipe 与 README「共享 Redis 按用途拆实例」。
	[ValidateSet("zone-up", "zone-down", "zone-status", "all-up", "all-down", "all-status", "infra-up", "infra-down", "infra-status", "infra-kafka-topics", "redis-split-status", "redis-wipe")]
	[string]$Command,

	[string]$ZoneName = "yesterday",
	[int]$ZoneId = 101,
	# 集群号(docs/design/node-id-overhaul-plan-20260908.md §5 改造 C):snowflake 的 17 位
	# worker 段切成 [cluster5][node12],这里就是高 5 位,取值 0..31。
	#
	# 这是**部署级常量**,由运维在建集群时定一次,策划不碰、zones 配置里也没有它:
	# 同一个 K8s 集群里所有 zone、全局池(match)和 C++ 节点必须写同一个值,否则
	# 两套 cluster 号下相同 node 号发出的 id 会撞。所以 infra-up(Apply-GlobalGoSvcManifests)
	# 与 zone-up(Apply-Zone)都从这一个参数取值,不允许各自另给。
	# 默认 0 = 与存量 id 逐位兼容(旧 id 的 node17 实际值 ≤ 几百,等价于 cluster=0 的 node)。
	[ValidateRange(0, 31)]
	[int]$ClusterId = 0,
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
	# 全服好友权威 TiDB 的同一 SQL 入口(host:port)。staging/prod 必填，禁止回落到 infra MySQL。
	# 这里只选择已有数据库，不创建 TiDB 集群、数据库或账号。
	[string]$FriendTiDBHost = "",
	# 交易全局权威SQL入口；只选择已配置的TiDB，不创建数据库或账号。
	[string]$TradeTiDBHost = "",
	# 全局匹配事务域与真实机房编号。生产不允许静默退回旧Redis匹配。
	[string]$MatchTiDBHost = "",
	# 全部跨域事务必须到同一SQL入口；别名参数只为兼容已有调用。
	[string]$GlobalAuthorityTiDBHost = "",
	[uint32]$PhysicalRegion = 0,
	# 玩家收件箱 scene 消费端开关的**核对值**(docs/design/player-inbox-service.md §7、§11.7;交接 player-inbox-handoff-20261015 §6 H1)。
	# 开关的权威值只有一份:git 跟踪的 bin/etc/game_config.yaml 的 player_inbox.consumer_enabled(与 PlayerInbox.EnqueueTypes 镜像
	# 服务 yaml 同口径)。node-config 的 game_config.yaml 把 player_inbox 段整段从那里复制,所以例行发布不会再把它写回 false。
	# 开 / 关消费 = 改那份 yaml 并提交,再 zone-up 并滚动 scene(ConfigMap 变了不会自己重启 C++ 进程)。
	# 本参数不改渲染值:留空 = 不核对(包装入口 dev_tools.ps1 / k8s_image.ps1 不透传,也不需要透传);给 on / off 时必须与 yaml 一致,
	# 不一致就在任何写操作之前 throw,并提示去改 yaml。用途是把「我认为它开着 / 关着」写进发布命令,yaml 被意外改回时发布直接失败。
	[ValidateSet("", "off", "on")]
	[string]$PlayerInboxConsumer = "",
	# 收件箱写前门禁的显式豁免(Assert-PlayerInboxConsumerGate):「消费端关、入箱白名单非空」时 zone-up / all-up / infra-up
	# 在任何写操作之前拒绝 —— 这样部署后 GM 处置照样入箱成功,却没有 scene 执行,条目 delivery_count=0,进不了任何告警。
	# 门禁先比当前检出里的两份 yaml,非 DryRun 时再只读另一侧线上已部署的值(Read-PlayerInboxGateLiveFacts):zone 与 infra 分开部署、
	# 可以来自不同 commit。只在明知如此时传(例如紧急关消费、来不及先清空白名单);它只放行,不改任何渲染值。
	# 包装入口(dev_tools.ps1 / k8s_image.ps1 / k8s_zone_rollback.ps1)没有这个开关:要豁免就直接调本脚本;
	# 包装入口被拒时的出路是先把 EnqueueTypes 改回 [] 并提交、重跑 infra-up(正常关消费的顺序)。
	[switch]$AllowPlayerInboxConsumerOff,
	# 留空 = 按 tag 是否可变自动推导(不可变 tag -> IfNotPresent,可变 tag -> Always)。
	# 以前无条件写死 IfNotPresent,配上 latest 就是"节点上有旧层就永远不拉新的"。
	[ValidateSet("", "Always", "IfNotPresent", "Never")]
	[string]$ImagePullPolicy = "",
	# 跳过发布预检。只给契约测试和"明知配置未就绪的演练"用,
	# staging/prod 真发布加这个开关等于把门禁拆了。
	[switch]$SkipPreflight,
	# C++ 节点日志采集:Linux 下 muduo **不往 stdout 写业务日志**
	# (cpp/.../node.cpp 的 Node::AsyncOutput 里调用 LogToConsole 那一句包在 #ifdef WIN32 内),
	# 业务日志只落 /app/bin/logs/cpp_nodes/<节点>.*.log。所以"读容器 stdout"的常规采集方案
	# 对 C++ 只能拿到 gate 的启动行和 gRPC/librdkafka 的 stderr,业务日志一条都采不到。
	# 默认给每个 C++ Pod 挂一个 Alloy sidecar,与业务容器共享同一个 node-logs 卷(只读)直接读文件送 Loki。
	# 关掉它就回到"C++ 业务日志无人采集"的状态,只给"集群里没有 Loki 也不打算部署"的场景用。
	[switch]$NoCppLogSidecar,
	# sidecar 镜像钉死版本:v1.19.2 的 loki.source.file 有读取位置回归(重启后几乎整文件重读),
	# 详见 docs/ops/grafana-loki-local-logs.md §1。升级前先跑该文 §5.3 的读取位置自检。
	[string]$CppLogSidecarImage = "grafana/alloy:v1.10.0",
	# 留空 = 写 infra namespace 里的 Loki(deploy/k8s/manifests/infra/loki.yaml)。
	# 指向集群外 / 已有的 Loki 时写完整 push 地址,例如 http://loki.observability:3100/loki/api/v1/push。
	[string]$LokiPushUrl = "",
	[ValidateSet("custom", "managed-cloud", "bare-metal")]
	[string]$OpsProfile = "custom",
	[ValidateSet("dev", "prod-like", "prod")]
	[string]$KafkaProfile = "prod",
	[int]$KafkaBrokerRetentionMs = 0,
	[int]$KafkaDbTaskRetentionMs = 0,
	[int]$KafkaRetentionCheckIntervalMs = 0,
	[long]$KafkaRetentionBytes = 0,
	[long]$KafkaSegmentBytes = 0,
	# Kafka 盘预算与 db_task 保留量(S5b,docs/design/db-task-delivery-and-retention.md §B;README「Kafka 磁盘预算与 db_task retention.bytes」)。
	# 以下五个参数 0 都表示「按档位 / 自动」,档位缺省值写在 Apply-KafkaProfileDefaults,公式在 Get-KafkaDiskPlan。
	# db_task topic 每分区的 topic 级 retention.bytes 目标值。0 = 按档位的设计字节率 × 缓冲目标算(Get-KafkaDbTaskRetentionBytesTarget);
	# >0 = 显式目标。两种都只是目标:盘放不下时按盘收缩并告警。负数在写路径入口拒绝(Kafka 里 -1 = 不限,磁盘无界)。
	# 算出的值同时渲染进 db 与 login 两个 ConfigMap 的 Kafka.RetentionBytes:两个服务启动时都会对同一个 topic 设置它。
	[long]$KafkaDbTaskRetentionBytes = 0,
	# 每个 zone 的 db_task 设计写入速率(字节/秒,按存盘峰值估算)。0 = 按档位:dev 256 KiB/s、prod-like 1 MiB/s、prod 3 MiB/s。
	[long]$KafkaDbTaskDesignBytesPerSec = 0,
	# db_task 的缓冲目标(毫秒):按设计速率至少要留住多久的积压(= 能扛住多长的 DB 故障、回档能往回拨多远)。
	# 0 = 按档位:dev 1 小时、prod-like / prod 2 小时。实际取 min(本值, db_task 的 retention.ms)。
	[long]$KafkaDbTaskBufferTargetMs = 0,
	# 每个 broker 的 Kafka PVC 大小(Gi)。0 = 自动:线上已有 StatefulSet kafka 就沿用线上模板(retention.bytes 按模板与每块 PVC 实际容量中小的算),
	# StatefulSet 不在但 PVC 在(--cascade=orphan 之后)时按最小的实际 PVC,都没有才按盘预算公式算。
	# 显式值与线上模板不同(volumeClaimTemplates 不可变、PVC 不能缩)、或大于最小的实际 PVC(盘还没扩完)时拒绝部署;
	# 扩盘按 README 的 runbook 先 patch PVC 再带本参数重跑 infra-up。
	[ValidateRange(0, 65536)]
	[int]$KafkaStorageGi = 0,
	# db_task 与审计 topic 之外所有 topic 的盘预留(GiB,单份数据、未乘副本数):控制面命令 topic、match-results、__consumer_offsets、
	# 旧的一节点一 topic 的残留。0 = 按档位:dev 1、prod-like 2、prod 4。
	[ValidateRange(0, 4096)]
	[int]$KafkaOtherTopicsReserveGi = 0,
	[string]$KafkaHeapOpts = "",
	# Kafka broker 数。0 = 按档位取默认:-ReleaseProfile 不是 dev,或 -OpsProfile 不是 custom → 3;其余 → 1。
	# 只接受 1 或 ≥3:两个 broker 的选举组挂一个就没有多数派,比单 broker 还脆。
	# 前 3 个 Pod 兼任 controller、第 4 个起只当 broker,所以 3 → N 只是加副本;
	# 1 ↔ ≥3 不能原地切换,脚本会拒绝(Assert-KafkaTopologyChangeIsSafe,流程见 README「Kafka:多 broker」)。
	[ValidateRange(0, 99)]
	[int]$KafkaBrokers = 0,
	# 共享 Redis 的形态。0 = 按档位取默认(与 -KafkaBrokers 同一判据):-ReleaseProfile 不是 dev,或 -OpsProfile 不是 custom → 3;其余 → 1。
	#   1:单实例(manifests/infra/redis.yaml),本机 kind / dev 档;
	#   3:高可用(manifests/infra/redis-ha.yaml):1 主 2 从 + 每个 Pod 一个哨兵 + 2 个 haproxy 入口代理,客户端地址仍是 redis:6379。
	# 1 → 3 可以原地切换(redis-0 带着数据当主,另外两个从它同步),但这是一次 Redis 重启,要 -ConfirmRedisCoreRestart;
	# 3 → 1 拒绝(主可能不在 redis-0 上,直接缩容会丢数据)。见 Assert-RedisTopologyChangeIsSafe 与 README「共享 Redis:高可用形态」。
	# 同一个值还决定 data-service / go/db 的 WAITAOF 副本确认份数(单实例 0、高可用 1,Get-MappingRedisTopology),所以必须与线上一致:
	# zone-up / all-up -SkipInfra 部署 zone Go 服务时(非 DryRun)读线上 sts/redis —— 没给本参数就跟随线上,给了而与线上不同即拒绝
	# (Resolve-RedisTopologyFromLive)。DryRun 不查集群,按本参数与档位渲染。
	[ValidateRange(0, 3)]
	[int]$RedisReplicas = 0,
	# 共享 Redis 按用途拆实例(G4a 第 1a 批;docs/design/shared-redis-split-by-purpose.md §25.3、§3.2.1)。本批只有 queue 一个用途。
	#   none :不拆。login 不写 Node.QueueRedis(与 RedisClient 共用同一个 client),gateway 的 spring.data.redis.host 仍是核心 redis.<infra>,
	#          渲染结果与引入本参数之前逐字节相同。
	#   queue:拆出排队实例 redis-queue-g<N>(N = zones 配置里该 zone 的 redisQueueGroup,缺省 0)。login 整段写 Node.QueueRedis(只承载排队键
	#          queue:* / admit:* / admitted:* 与放行锁),gateway 的 Bucket4j 限流桶(rl:zone / rl:ip)改连它;gate 排空标记 gate:<id>:draining|drained、
	#          会话、锁、映射等其余一切仍在核心。infra-up / 带 infra 的 all-up 另按 zones 配置里出现过的每个组 apply 一份 queue 实例,
	#          形态与核心同一套 -RedisReplicas 档位规则:1 = manifests/infra/redis-purpose.yaml 单实例,3(非 dev 档缺省)=
	#          redis-purpose-ha.yaml 的 1 主 2 从 + 哨兵 + 2 个代理(同名 Service 指向代理,地址不随形态变)。容量见 -RedisQueueMaxMemory。
	#   auto(缺省):所有档位都解析为 none —— 本批不在任何档位缺省开启拆分,首次上线显式 -RedisSplit queue 部署(§25.3 的 2026-10-23 订正)。
	#          例外是非 DryRun 的 zone 写路径(zone-up / all-up 部署 login 或 gateway):跟随线上,本 zone 线上 login ConfigMap 有 Node.QueueRedis
	#          即 queue,没有或 ConfigMap 不存在即 none。DryRun 不查集群,auto 按 none 渲染。infra-up 的 auto 按 none:不 apply、也不删除已有的 queue 实例。
	# 防静默切换(Assert-RedisPurposeSwitchIsSafe,非 DryRun、任何写操作之前):本次要写给 login / gateway 的排队地址与本 zone 线上不同时,
	# 必须带 -ConfirmRedisSplitSwitch,且线上 login 的 Queue.Enabled 不是 true(排队开着切换 = 新旧副本各连一个实例、两套队列、准入翻倍,
	# §3.2.1 约束 2),否则拒绝,报错写明线上现状与应带的参数。zone 回档(k8s_zone_rollback.ps1)Step 1 删掉 namespace 后读不到线上,
	# 所以回档要显式给本参数。包装入口 dev_tools.ps1 / k8s_image.ps1 非 auto 才透传。
	[ValidateSet('auto', 'none', 'queue')]
	[string]$RedisSplit = 'auto',
	# 确认本次就是要改 login / gateway 的排队 Redis 地址(none ↔ queue、换组)。它是许可不是指令:地址不变时给了也不改任何东西。不粘滞。
	# 不豁免「排队开着」那一条:线上 Queue.Enabled 为 true 时给了照样拒绝,先以 -LoginQueue off 部署本 zone、关掉排队再切。
	[switch]$ConfirmRedisSplitSwitch,
	# 每个 queue 实例的 maxmemory,redis-server 写法 <正整数><kb|mb|gb>(如 1gb);容器 requests 与 limits 的内存按 2 × maxmemory 渲染
	# (AOF 重写 fork 时写时复制最坏会翻倍)。留空:dev 档(-ReleaseProfile dev 且 -OpsProfile custom)取 128mb;release 档在本次要 apply
	# queue 实例时没给即在任何写操作之前拒绝(queue 实例是 noeviction,写满即拒写,排队开着时新登录全部失败;初值每组 1gb,§25.3 第 19 条)。
	# 只对 -RedisSplit queue 的 infra-up / 带 infra 的 all-up 生效,其余命令给了只告警。
	[string]$RedisQueueMaxMemory = "",
	# redis-wipe 的显式确认(压测前清空部署拓扑里的每个 Redis 实例,AGENTS §6.2、设计稿 §5.9)。命令还要求 -InfraNamespace 匹配
	# 脚本里写死的 ^mmorpg-stress-(白名单不能只靠本次命令行参数),两条缺一即拒绝。
	[switch]$ConfirmWipeAllRedis,
	[int]$CentreReplicas = 1,
	[int]$GateReplicas = 2,
	# battle 是不分 zone 的全局池，只由 infra-up / all-up 部署一次。
	# 默认与本地 cpp_nodes.ps1 一样为 1；0 表示不装配，不删除已部署的池。
	[ValidateRange(0, 65535)]
	[int]$BattleReplicas = 1,
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
	# 已退役(docs/design/grpc-muduo-transport.md §15.11):第 4 批起 C++ gRPC 服务端跑在节点 EventLoop 上,
	# 没有 poller 线程,进程也不再读 GRPC_SERVER_MAX_POLLERS。参数只为兼容旧的调用命令而保留:
	# Deployment / Fleet / 客户端入口生成器一律不再注入该环境变量;传入大于 0 的值只打一条 Write-Warning,取值被忽略。
	[int]$GrpcServerMaxPollers = 0,
	# gate 对外 Service 的类型。-ClientEntryMode external:决定每序号 Service gate-<i> 的类型,只允许 NodePort / LoadBalancer
	# (preflight 校验);podip:决定单一 gate-entry 的类型,且 gate-entry 只在 gate 副本数为 1 时生成(D90)。
	# 取值在参数绑定后规范成 K8s 的标准大小写(见脚本顶部 ConvertTo-ClientEntryCanonicalName)。
	[ValidateSet("ClusterIP", "NodePort", "LoadBalancer")]
	[string]$GateServiceType = "NodePort",
	[int]$GateServicePort = 18000,
	# gate 的客户端 RPC 路由模式:写进 gate Deployment 的环境变量 GATE_CLIENT_RPC_ROUTER
	# (cpp/nodes/gate/gate_router_mode.h;docs/design/client-rpc-router.md D34)。
	#   "1" = gate 只连路由服 client-rpc-router,chat / friend / trade 等只承诺路由模式的服务经它可达
	#         (team 等没登记进 $GoSvcCatalogue 的服务,K8s 上本来就不部署,与本开关无关;guild 自 2026-10-16 起登记,是全局池);
	#   "0" = 旧的逐服务直连,只作回退:chat / friend / trade 不可达(gate 直连白名单里没有它们)。
	# 两种模式下战斗都只走客户端 ↔ battle 直连,gate 不中继战斗(turn-based §22 D66),所以本开关与战斗无关。
	# **默认 "1"**(turn-based §22 D75,2026-09-29 用户拍板):豁免 D-12 / D36 / D37 的前提①
	# 「先在 K8s 上以路由模式跑通一次 battle-smoke」,改为事后补验;D-12 的前提②③(路由服 manifest 已落地、
	# 路由服按 POD_IP 通告)已满足。翻转只落在部署层:C++ 进程默认值(gate_router_mode.h 未设变量即直连)
	# 与 gate_security_test 的默认值断言一字不改(docs/design/xuanming-port-decisions-20260910.md D-12 及其修订)。
	# 前置(翻转后 gate 硬依赖它):infra-up 必须已把 client-rpc-router 部署就绪 —— 不带 -SkipGoSvc,且给了 -GoSvcRegistry。
	# 路由模式下 gate 的依赖门等的是 ClientRpcRouter + Scene 而不是 Login(gate/main.cpp requiredDependencies),
	# 路由服不在就过不了依赖门,登录 / 匹配 / 聊天全部 no_target;对一个没有路由服的旧 infra 只跑 zone-up 同理。
	# 路由服部署链:go_svc_image.ps1 镜像、$GoSvcCatalogue 条目、New-GoSvcConfigMapYaml 的 client-rpc-router case、
	# manifests/go-svc/client-rpc-router.yaml(注入 POD_IP)、node-config service_discovery_prefixes 的
	# ClientRpcRouterNodeService.rpc;路由服按 POD_IP 通告对外地址(client_rpc_router_service.go advertisedHost)。
	# 回退到 "0" = chat 及所有只承诺路由模式的服务同时不可达,回退前先 killswitch 关这些方法并公告(D-12)。
	# 回退态不粘滞:本值每次部署都原样重写进 gate env,脚本不从集群读回旧值。以 "0" 回退运行的 zone,之后每一次
	# 重新部署(日常 zone-up / release-zone、合服后的 zone-up、k8s_zone_rollback.ps1 的 Step 6,以及经 dev_tools.ps1 /
	# k8s_image.ps1 等包装入口)都必须显式再传 -GateRouterMode 0,否则静默落回默认 "1";回退的原因(如路由服不可用)
	# 若仍在,gate 就卡在依赖门,登录 / 匹配全部 no_target。
	# 只收 "0" / "1":gate 侧除 1/true/on 以外一律当关,拼错会静默落回直连,不如在脚本入口就拒。
	# 用字符串而不是 [bool]/[switch]:值原样写进 env,两边字面值一致,kubectl 里看到的就是传进来的。
	[ValidateSet("0", "1")]
	[string]$GateRouterMode = "1",
	# B0-4 运维豁免开关:写进本次部署全部 C++ 工作负载的环境变量 NODE_OPEN_FILE_LIMIT_MODE
	# (cpp/libs/engine/core/node/system/node/open_file_limit.h 的 kModeEnv / ParseMode;负责人 2026-10-17 拍板:缺省拒启 + 显式豁免)。
	#   留空(缺省)= 不写这个变量,生成物与引入开关之前逐字相同,进程按 enforce:软限抬到硬限后仍低于需求
	#               (gate GateMaxConnections + 4096、battle BattleMaxConnections + 4096、scene 4096)就拒启,Pod 进 CrashLoop;
	#               -WaitReady 失败时会把容器日志里的 "FATAL open file limit" 行捞出来打印(Write-CppOpenFileLimitRefusals)。
	#   warn  = 不够也继续启动、只打 WARN。代价:客户端连接可能先占满 fd 表,之后节点间 / Redis / Kafka / etcd 重连与日志滚动都会 EMFILE,
	#           节点可能被 etcd 摘除或崩溃,只作「硬限暂时改不了、必须先起服」的短时应急。gate / battle 的应急首选是把准入上限调到
	#           ≤ 软限 − 4096(检查直接通过,不需要豁免),见 docs/ops/gate-kernel-tuning-runbook.md §3.1。
	#   enforce 显式传也照写进 env,kubectl get 一眼看出取值。
	# 覆盖范围:zone-up 的 gate(Deployment / external StatefulSet)与 scene(Deployment / Agones Fleet),infra-up 的 battle
	# (Deployment / hostPort Deployment / Agones Fleet)。只开 zone-up 不影响 battle,反之亦然。与 *_RUN_MODE、GM 开关无关。
	# external gate 是 OnDelete:模板改了不重建 Pod,要按 k8s_gate_drain.ps1 逐台排空删 Pod 才生效。
	# 不粘滞(同 -GateRouterMode):脚本不从集群读回旧值,以 warn 运行的环境下一次部署不传就落回 enforce,硬限仍不够会再次拒启。
	# 区分大小写:C++ 只认小写,其余值按 enforce 处理;ValidateSet 缺省不分大小写,所以 IgnoreCase = $false,大写或拼错在入口就拒。
	[ValidateSet("enforce", "warn", IgnoreCase = $false)]
	[string]$NodeOpenFileLimitMode = "",
	# C++ 节点(gate / scene / battle)的 Prometheus 出口端口(cpp/libs/engine/core/metrics/metrics_http_exporter.h,
	# 进程读环境变量 MMORPG_METRICS_PORT;补 docs/ops/cross-zone-failure-test-runbook.md §6 观测缺口 G1 / G2)。
	# 每个 Pod 一个 IP,三种角色共用一个端口即可。大于 0 时渲染三样:env、名为 metrics 的容器端口、
	# prometheus.io/scrape / port / path 抓取注解(scene 开 NetworkPolicy 时一并放行这个口)。0 = 不开出口,三样都不写。
	# 改端口会改 Pod 模板(滚动);external gate 是 OnDelete,要按 k8s_gate_drain.ps1 逐台排空删 Pod 才生效。
	[ValidateRange(0, 65535)]
	[int]$CppMetricsPort = 9400,
	# go/db 存盘消费组的渲染模式(docs/design/db-consumer-group-per-zone.md §2 / §4;决策表见 Resolve-DbConsumerGroupPlan):
	#   keep(缺省)  按线上 go-svc-db-config 现状渲染:最终形状原样保留;未收尾的交接形状在对方组已没有位点时自动收尾成最终形状
	#                (去掉 GroupHandoffFrom,db 滚动一次);全新 namespace(读不到 ConfigMap)按 Kafka 位点只渲染最终形状。
	#   zone        把本 zone 交接到按 zone 的组 db_rpc_consumer_group_z<ZoneId>(legacy 组还有位点时渲染交接形状)。
	#   legacy      反向交接回共享组 db_rpc_consumer_group(回滚用)。
	# zone / legacy 只许配 -Command zone-up:切组要逐 zone 做、冻结其它 zone 的发布并紧跟收尾(kafka_offset_reset.ps1 -FinalizeHandoff),
	# all-up 一次切全部 zone 等于把所有 zone 同时放进交接窗口;其它命令在入口拒绝。zone / legacy 还要求本次部署 db(给 -GoSvcRegistry
	# 且不带 -SkipGoSvc,k8s_deploy.ps1 里 -GoSvcRegistry 缺省为空),否则在写路径入口拒绝:不部署 db 的 zone-up 切不了组。
	# 交接形状只写 ConsumerGroup + GroupHandoffFrom、不写 GroupID:交接闸之前的 go/db 二进制把 GroupID 当必填,在 conf.MustLoad 就拒启,
	# 这就是「新镜像带交接闸」的能力证明(maxUnavailable 0,新 Pod 拒启时 rollout 卡住);最终形状只写 GroupID,任何二进制都能加载。
	# 「旧 Pod 继续服务」只在线上 db 本身带交接闸时成立:ConfigMap 整目录挂载,旧 Pod 一旦重启就读到交接形状。所以显式切组渲染交接形状之前,
	# 写路径入口先经 API server 代理抓每个 db Pod 的 /metrics,要求都有 db_consumer_group_handoff_gate_total(要 pods/proxy 权限,
	# Assert-LiveDbPodsHaveHandoffGate),证明不了就在任何写操作之前拒绝。
	# 非 DryRun 时在写路径入口读线上 ConfigMap,并按需 kubectl exec 进 kafka-0 读两组在本 zone db_task topic 上的位点(要 pods/exec 权限),
	# 判不出就在任何写操作之前拒绝(all-up 不带 -SkipInfra 且 infra namespace 不存在时,本次新建的 Kafka 必然为空,不 exec);
	# DryRun 不读集群,渲染预览并告警。不粘滞;dev_tools.ps1 只在被指定时透传,k8s_image.ps1 不透传(等于 keep)。
	[ValidateSet("keep", "zone", "legacy")]
	[string]$DbConsumerGroup = "keep",

	# ── D1 玩家归属与落点映射持久化的部署载体(第 1b-3 批:docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §7、§13.4)──
	# 全球正式默认是 db_first / true；历史 redis / false 仅用于读取旧集群实况和拒绝演练,不能用于正式服务部署。写进哪里:
	#   -RouteStoreMode / -RouteReconcileArm / -RouteReadRequiresReady → zone 的 go-svc-data-service-config
	#       (RouteStore.Mode / RouteStore.Reconcile.ArmReadyMarker / RouteStore.ReadRequiresReady);
	#   -DbPlacementRequireRouteReady → zone 的 go-svc-db-config(Placement.RequireRouteReady);
	#   -GuildRequireRouteReady → infra namespace 的 go-svc-guild-config(MergeMarkerRedis.RequireRouteReady,只在部署全局 Go 服务时生效)。
	# 只改 ConfigMap 不会滚动进程(同 db 的 RetryRouting):改值后 rollout restart 对应 Deployment,以启动日志核对。
	# 首次上线先正规迁移与存量导入/audit,再 reconcile/ready,最后启动读方；全球服务不再按旧 P1–P5 演练流程回退 Redis。
	# 故障时保持 SQL 提交点并沿冻结/核验/恢复流程处理,见 player-route-mapping-durability-proposal.md §14。
	# 组合门禁(写路径入口,DryRun 同样判,Assert-RouteSwitchPreflight):Arm 或任一读方开关为 true 而 Mode=redis → 拒绝
	# (data_service 的 Validate 会拒启,进集群就是 CrashLoop);任一读方开关为 true 而 Arm=false → 拒绝(没有就绪标记,
	# 读方全部 fail-closed:go/db 停存盘消费、data_service 全部回源、guild 全拒)。
	# **粘滞(防翻转)**:这几个值不能「每次部署按参数重写、漏传就落回缺省」—— 漏传一次就把 db_first 静默降回 redis,
	# zone 回档的 Step 6 重建 namespace 时尤其如此。所以非 DryRun 时写路径入口在任何写操作之前读线上现状(Assert-RouteSwitchClusterState):
	# data-service 读**全部** zone namespace(Mode / Arm / ReadRequiresReady 是全服单值:所有 zone 共用同一个全局库与 mapping Redis),
	# db 读本次部署的 zone,guild 读 infra namespace;与本次参数不一致就整条拒绝,报错给出线上现状对应的参数串。
	# 确要切换时加 -ConfirmRouteSwitchChange。它不豁免读方前提:本次渲染了读方开关,而本次不部署的 zone 的 data-service 线上
	# 不是 db_first + Arm=true 时一律拒绝(多 zone 首次切换只能 all-up 一次切全部 zone,见 Assert-RouteSwitchClusterState)。
	# 包装入口:dev_tools.ps1 的 k8s-infra-up / k8s-zone-up / k8s-all-up 同名透传这六个参数
	# (2026-10-19,留空不传);k8s_image.ps1 不部署 Go 服务,dev_tools 的 k8s-image-* / k8s-release-* 显式给了即拒绝;
	# k8s_zone_rollback.ps1 不声明这六个参数,Step 6 按本脚本缺省(db_first / 全 true)重建 zone:线上已是全球默认的集群照常通过,
	# 线上仍是旧值的多 zone 集群在 Step 6 被粘滞核对拒绝(先 all-up 切好再回档);单 zone 集群删掉 namespace 后读不到任何现状,
	# Step 6 会按缺省直接切到 db_first(跳过存量导入 / audit),旧集群先按首次切换顺序切好再回档。
	# dev_tools 的 k8s-zone-rollback 显式给了这些开关即拒绝。
	# ValidateSet 不分大小写,参数绑定后规范成小写(data_service 的 Mode 区分大小写,`DB_FIRST` 会被它拒启)。
	[ValidateSet("redis", "db_first")]
	[string]$RouteStoreMode = "db_first",
	[switch]$RouteReconcileArm = $true,
	[switch]$RouteReadRequiresReady = $true,
	[switch]$DbPlacementRequireRouteReady = $true,
	[switch]$GuildRequireRouteReady = $true,
	# 确认本次部署就是要改动线上的上述开关(见上一段「粘滞」)。不给时线上取值会被本次参数改掉就在任何写操作之前拒绝;给了只打警告放行。
	# 它是许可不是指令:线上与本次一致时给了也不改任何东西。不粘滞,只在被指定时透传。
	[switch]$ConfirmRouteSwitchChange,

	# ── 集群外客户端入口(D76–D93;生成器、preflight 与就绪等待在 lib/k8s_client_entry.ps1)────────────
	# 模式矩阵(D80):
	#   podip    + deployment:gate / battle 都是现状 Deployment,客户端拿 PodIP,只给集群内 robot 与压测。
	#   podip    + agones    :battle 为 Agones Fleet(CLIENT_ENDPOINT_SOURCE=none)。
	#   external + deployment:gate 为 StatefulSet + 每序号 Service;battle 为 Deployment + hostPort 20000(只用于验证 / 回退,D86)。
	#   external + agones    :gate 同上;battle 为 Fleet portPolicy Dynamic(生产推荐)。
	# 模式参数**不粘滞**:每次部署按本次取值生成,漏传就落回默认 podip / deployment。以 external / agones 运行的环境
	# 重跑时(含 k8s_zone_rollback.ps1 Step 6 与包装入口)必须照传;否则清单会指向正在服务的 gate StatefulSet / battle Fleet,
	# 这类会踢人的删除由 -AllowDisruptiveSwitch 把关,没给就在删除之前整条拒绝;battle 同 kind 内换模式(按现有容器 env
	# CLIENT_ENDPOINT_SOURCE 判定)同样把关。以上判定都在写路径入口一次做完(Assert-ClientEntryClusterState,非 DryRun)。
	# 上线口径(turn-based §22 D75 / ingress_final §6):集群外部署须与 -GateRouterMode 1 同一窗口启用 external;
	# login / scene_manager 的 RequireClientEndpoint 见 -RequireClientEndpoint。
	[ValidateSet("podip", "external")]
	[string]$ClientEntryMode = "podip",
	# login / scene_manager ConfigMap 的 RequireClientEndpoint(D78):下发 gate 地址时是否必须用 gate 自报的客户端可达地址。
	# auto(默认)= 跟随 -ClientEntryMode:external → true,podip → false。
	# 上线顺序(ingress_final §6):第 2 批逐个 zone 切 external 的窗口里,已切 zone 取 true 会让 scene_manager 跨 zone
	# RedirectToGate 跳过未切 zone 的 gate(它们没有 client_endpoint),跨区跳转失败 —— 窗口期对已切 zone 显式传 false;
	# 第 3 批确认全部 gate 都已自报地址后,回到 auto 并滚动 login 与 scene_manager。podip 下传 true 在写操作之前拒绝
	# (podip 的 gate 从不自报地址,全部 gate 都会被跳过)。包装入口留空不透传。
	[ValidateSet("auto", "true", "false")]
	[string]$RequireClientEndpoint = "auto",
	# gate 与 battle 共用的客户端可达主机(裸主机名或 IPv4,不带 scheme / 端口);kind 填 127.0.0.1。只在 external 下生效;
	# 留空时 gate 取 status.hostIP,battle(agones)取 Agones status.address,battle(hostPort)取 status.hostIP。
	[string]$ClientPublicHost = "",
	# gate 客户端主机模板,例如 gate-{ordinal}.{zone}.example.com:{zone} 生成期渲染,{ordinal} 在 gate 启动 shell 里渲染。
	# 优先级高于 -ClientPublicHost;-GateServiceType LoadBalancer 时必填;gate 副本数 >1 时必须含 {ordinal}。
	# 地址参数(本参数、-ClientPublicHost、-GateNodePortBase、-GateServiceType)同样不粘滞:gate StatefulSet 是 OnDelete,
	# 现有 Pod 仍自报旧地址,每序号 Service 却会立刻改写。所以集群里已有 gate StatefulSet 而本次算出的地址来源与它的模板
	# 不同时,没给 -AllowDisruptiveSwitch 就在任何写操作之前拒绝,报错写明保持现状要传的值。
	[string]$GateClientHostTemplate = "",
	# external + NodePort:gate-<i> 的 nodePort = base + i,gate 自报同一个端口(D88)。只给单 zone 路径(zone-up)用;
	# all-up 由 zones 配置里每个 zone 的 gateNodePortBase 覆盖,多 zone 时每个 zone 必须显式写(preflight 校验段不重叠、不越界)。
	# 建议落在 K8s 静态子段 30000–30085。分多次 zone-up 部署时跨 zone 的重叠由删除旧形态之前的服务端预演
	# (kubectl apply --dry-run=server)拦下。
	[int]$GateNodePortBase = 30000,
	# 每序号 Service 的 externalTrafficPolicy(D89):Local 保留玩家真实源 IP;Cluster 会 SNAT,G9 按源限流失效(preflight 警告)。
	[ValidateSet("Local", "Cluster")]
	[string]$GateExternalTrafficPolicy = "Local",
	# battle 全局池的编排(D80 / D81):deployment = Deployment(external 下为 hostPort 形态);agones = agones.dev/v1 Fleet
	# (集群须已安装 Agones,preflight 探测 fleets.agones.dev,DryRun 跳过探测)。只对 infra-up / all-up(不带 -SkipInfra)生效,
	# 与 -SceneOrchestrator 相互独立。Fleet 的 health 复用 -AgonesHealth* 三个参数,initialDelaySeconds 不够覆盖 battle 启动
	# 最坏耗时时按推导抬高并打一行说明(Resolve-BattleFleetHealth)。
	[ValidateSet("deployment", "agones")]
	[string]$BattleOrchestrator = "deployment",
	# gateway 的 Ingress(D91)。**由 zone-up 生成**:Ingress 与 gateway Deployment 同处(zone namespace,Apply-JavaSvcManifests),
	# 只在本次确实部署 Java 服务(给了 -JavaSvcRegistry 且不带 -SkipJavaSvc)时生成;在 infra-up 上给它只会得到"被忽略"警告。
	# 可含 {zone}(生成期渲染,多 zone 各用一个 host)。留空 = 不生成,也**不删除**已有的 Ingress(参数不粘滞,
	# 自动删会切断玩家的 HTTP 入口;要撤掉请手动 kubectl delete ingress gateway)。Ingress 只路由 /api,/admin 与 /actuator 不出集群。
	[string]$GatewayIngressHost = "",
	[string]$GatewayIngressClassName = "nginx",
	# 非空时 Ingress 生成 tls 段,值为 zone namespace 里的 TLS Secret 名。
	[string]$GatewayIngressTlsSecret = "",
	# gateway 信任的反向代理 CIDR,逗号分隔,写进 java-svc-gateway-config 的 gate.rate-limit.trusted-proxies。
	# 配了 -GatewayIngressHost 却不给它 = 全体玩家共用 Ingress controller 那一个限流桶,preflight 报错。
	# 同样不粘滞:本次部署 gateway 而它为空时,若 zone namespace 里已有 Ingress gateway(留空 host 不会删它),
	# 在写操作之前拒绝 —— 要么照传,要么先手动删掉 Ingress(不受 -AllowDisruptiveSwitch 豁免)。
	[string]$GatewayTrustedProxies = "",
	# 集群内 login 的开发口令认证(2b §7):login ConfigMap 写 DevPasswordAuth(账号前缀 robot_ / dev_),共享口令从环境变量
	# MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET 读入 zone namespace 的 Secret login-dev-password,经 secretKeyRef 注入 login 容器的
	# LOGIN_DEV_PASSWORD_SHARED_SECRET,绝不进 ConfigMap。**只允许 -ReleaseProfile dev**(与 release_preflight.ps1 的
	# debug.login.devpassword 同一门禁),其它档位传它在任何写操作之前报错。不传 = login 没有任何口令认证配置(fail-closed)。
	[switch]$LoginDevPasswordAuth,
	# 集群内 login 的第三方登录(微信 / QQ):login ConfigMap 写顶层 AuthProviders 段(New-LoginAuthProvidersYaml)。两个 AppId 都不给 =
	# 不写 AuthProviders,产物与接入前逐字相同(login 不开第三方登录)。各档位都可用(不同于 -LoginDevPasswordAuth 只许 dev)。
	# 微信:-LoginWeChatAppId 非空才渲染 AuthProviders.WeChat(AppId + AppSecretEnv)。AppSecret 从环境变量 MMORPG_LOGIN_WECHAT_APP_SECRET
	# 读入 zone namespace 的 Secret login-oauth(key wechat-app-secret),经 secretKeyRef 注入 login 容器的 LOGIN_WECHAT_APP_SECRET,
	# 绝不进 ConfigMap;**任何档位**未设置都在写操作之前拒绝(没有回落值,login 对缺失的 AppSecretEnv 同样拒启)。Pod 模板带密钥指纹
	# 注解 mmorpg.io/login-oauth-secret-hash,只轮换密钥也会滚动 login。
	# QQ:-LoginQQAppId 非空才渲染 AuthProviders.QQ(只写 AppId):provider 只拿客户端的 access_token 调 /oauth2.0/me 并校验
	# client_id == AppId,不需要任何密钥,不建 Secret。
	# AppId 只允许 [A-Za-z0-9_.-]。-LoginWeChatEndpoint / -LoginQQEndpoint 只给沙箱 mock 用(留空 = 生产地址),须是
	# https://主机[:端口][/路径](dev 档也接受 http://,与 login 在 Mode=dev 下的放行口径一致),不带用户信息、查询串与片段;
	# 给了 Endpoint 却没给对应 AppId 在写操作之前拒绝。
	# -LoginWeChatAllowOpenId / -LoginQQAllowOpenId:写 RequireUnionId: false,拿不到 unionid 时按 openid 建账号。以后接入 unionid
	# 会把同一玩家分裂成两个账号且无法事后合并,非 dev 档给了要打警告;不给 = login 缺省 true(拿不到 unionid 即拒绝登录)。
	# 全部不粘滞:每次部署按本次取值重写 ConfigMap;上次配过微信、这次没给,就删掉 login-oauth Secret,微信登录随新 Pod 一起关闭。
	# 防误关(fail-closed,Assert-LoginAuthProviderClusterState,非 DryRun、任何写操作之前):本次部署 login 的每个 zone,线上
	# login ConfigMap 的 AuthProviders(或 Secret login-oauth)与本次参数不一致 —— 漏传而关掉某个 provider、漏传 Endpoint 把沙箱
	# 改连生产、漏传 AllowOpenId 把 RequireUnionId 翻回 true、换了 AppId、线上段认不出 —— 就整条拒绝,报错给出照传现状的参数串。
	# 确要关闭或改动时加 -ConfirmLoginAuthProviderChange。线上没有、本次新开不需要确认。
	# 包装入口照传:dev_tools.ps1(k8s-zone-up / k8s-all-up)与 k8s_zone_rollback.ps1 的 Step 6 非空才透传这六个参数与确认开关;
	# k8s_zone_rollback.ps1 在 Step 1 删 namespace 之前按同一份判据(lib Get-LoginAuthProviderDrift)自行核对。k8s_image.ps1 不部署
	# login,不声明它们,dev_tools.ps1 的 k8s-image-* / k8s-release-* 显式给了即拒绝。
	[string]$LoginWeChatAppId = "",
	[string]$LoginWeChatEndpoint = "",
	[switch]$LoginWeChatAllowOpenId,
	[string]$LoginQQAppId = "",
	[string]$LoginQQEndpoint = "",
	[switch]$LoginQQAllowOpenId,
	# 确认本次部署就是要关掉或改动线上 login 的第三方登录配置(见上一段「防误关」)。不给时,线上配置会被本次参数关掉或改掉
	# 就在任何写操作之前拒绝;给了只打警告放行。它是许可不是指令:线上与本次一致时给了也不改任何东西。不粘滞,只在被指定时透传。
	[switch]$ConfirmLoginAuthProviderChange,
	# 削峰(G4b,docs/design/open-server-rate-limit-design.md §十一):gateway Bucket4j 限流 / login 登录排队。
	# auto(缺省)= 跟随 -ReleaseProfile:dev 关(ConfigMap 与 G4b 之前逐字相同);staging / prod 开,阈值见 $script:PeakShavingLaunchTier。
	# off 只用于事故止血与基线压测(AGENTS §6 对比的「关削峰」那一轮);on 只用于 dev 档联调。
	# 不粘滞:以 off 运行的环境,下一次部署(含 k8s_zone_rollback.ps1 Step 6、dev_tools.ps1 包装入口)不传就按档位恢复为开。
	# 包装入口(dev_tools.ps1 / k8s_image.ps1 / k8s_zone_rollback.ps1)不透传这两个参数,off 止血要直接调本脚本。
	# 切换会改 Pod 模板上的 mmorpg.io/peak-shaving-hash 注解,gateway / login 自动滚动,不需要手工 rollout restart。
	[ValidateSet("auto", "on", "off")]
	[string]$GatewayRateLimit = "auto",
	# login 排队开时 staging / prod 必须注入 MMORPG_LOGIN_QUEUE_TOKEN_SECRET(≥32,与 GATE / INTERNAL_AUTH 两把不同)。
	# 排队的出队 leader 锁按 zone 拆(G1b,login 把 Queue.DispatcherLockKey 当前缀再拼本 zone);本脚本另在 ConfigMap 里显式写
	# 带本 zone 的 DispatcherLockKey 作纵深防御(见 New-LoginQueueYaml)。
	# 合入门槛(2026-10-15):$script:LoginQueueMergeGateCleared 为 $false 时,auto 在任何档位都解析为关,on 在任何写操作之前拒绝
	# (三项门槛与出处见该常量的注释)。上面「staging / prod 开」在门槛清掉之前只对 -GatewayRateLimit 成立。
	[ValidateSet("auto", "on", "off")]
	[string]$LoginQueue = "auto",
	# 确认本次就是要切换形态(-ClientEntryMode / -BattleOrchestrator 换了取值)或改 gate / battle 的客户端地址来源,
	# 接受整台 gate 踢人、在打的战斗作废、改地址后须逐个排空重建 gate。
	# 不给时,只要"另一种形态"里会踢人的工作负载在集群里确实存在,或现有 gate / battle 的入口形态与地址来源会被改写,
	# 部署就在任何写操作之前整条拒绝、一个都不删。只在维护窗口里显式加;包装入口留空不透传。
	[switch]$AllowDisruptiveSwitch,
	# 确认本次 infra-up / all-up 可以重启共享 Redis(redis-0):本次 apply 会改变线上 sts/redis 的 Pod 模板(命令、env、探针、
	# 镜像、resources 任何一处),或线上 sts/redis 还没有模板摘要注解(P0-E 合入后的第一次)时,不给它就在 Apply-Infra 的任何写操作
	# 之前拒绝(Assert-RedisCoreRestartConfirmed)。重启 redis-0 = 全服登录中断并重放 AOF,只在维护窗口里显式加。
	# 「变」指与上次经本脚本 apply 的模板(sts/redis 上的摘要注解)相比;对 redis.yaml 里声明了的 Pod 模板字段做的带外改动
	# (kubectl edit / set image / patch)看不见,apply 会把它们改回清单值并重启 redis-0(清单里没声明的带外字段 apply 会保留)。
	# 线上还是 2026-09-19 之前的 Deployment redis 时同样要它(切 StatefulSet = 停机且数据不迁移)。
	# 模板没变时给了也不会重启(它是许可,不是指令);DryRun 不查集群。不粘滞;dev_tools.ps1 / k8s_image.ps1 只在被指定时透传。
	# 名字沿用 docs/design/shared-redis-split-by-purpose.md(G4a)§4.2 的规划,语义按 P0-E 放宽为「模板有变化就要确认」。
	[switch]$ConfirmRedisCoreRestart,
	# 通用资产通道(帮会捐献 / 商店兑换、聚宝斋上架;docs/design/guild-phase2/04-asset-channel.md §4.32、90-consistency.md G-05)。
	# off(默认):guild / trade 的 ConfigMap 写 AssetOp.Enabled: false,不建签名密钥 Secret,guild / trade / scene 都不注入密钥。
	# on:两把签名密钥从环境变量 MMORPG_ASSET_OP_SECRET_GUILD / _TRADE 读入(各档位都没有回落值;≥32 字节、不含空白、两把不同、
	#     也不与其它口令相同),写成 Secret asset-op-secret-guild / asset-op-secret-trade,经 secretKeyRef 注入 guild(只 GUILD)、
	#     trade(只 TRADE)与 scene(两把都要,验签);guild / trade 的 AssetOp.Enabled 写 true(trade 同时补齐落地恢复与 Cleanup* 键)。
	# 前提:必须同时 -NetworkPolicies on(scene 的 gRPC 口只放行资产调用方,G-05);08-save-owner-fence.md §8.3 的六条门禁是运行期验证,脚本核不了;
	#       已知风险 K1(单写者只在 30s 重连租约内成立)同样是任何共享环境打开资产通道的前置(原 trade ConfigMap 注释的口径)。
	# 顺序:打开 = 先逐 zone 跑 zone-up -AssetOps on(scene 先拿到验签密钥),再 infra-up -AssetOps on(调用方开始签名);
	#       关闭 = 先 infra-up -AssetOps off,再逐 zone off;换密钥 = 先关、换值、再按打开的顺序开。
	#       非 DryRun 时按双方 Pod 模板上的密钥指纹核对集群现状(Assert-AssetOpClusterConsistency),顺序反了在任何写操作之前拒绝。
	# 切换会改 scene 的 Pod 模板(env + 指纹注解),整个 zone 的 scene 滚动一遍 = 在线玩家被换节点,只在维护窗口里切。
	# 不粘滞:以 on 运行的环境,下一次部署(含包装入口、k8s_zone_rollback.ps1 Step 6)不传就落回 off;调用方还开着时 zone-up 会被拒。
	[ValidateSet("off", "on")]
	[string]$AssetOps = "off",
	# scene / guild / match 的 gRPC 口准入(NetworkPolicy,G-05)。off(默认):不生成,并删掉本脚本以前建的同名策略(不粘滞)。
	# on:每个被保护的工作负载一份 NetworkPolicy($script:GrpcAllowLists):工作负载清单里**声明了的**其它端口(节点口 / metrics /
	#   日志 sidecar)照常对任何来源开放,只有 gRPC 口限定来源(任意 namespace 里 app 标签在名单里的 Pod);没声明的端口一律不通。
	#   scene(zone namespace,50000):scene-manager / match / player-inbox / guild / trade / mail;
	#   guild(infra,50300):client-rpc-router / data-service;
	#   match(infra,50500):client-rpc-router / guild / gate(gate 只在 -GateRouterMode 0 回退时直连 match)。
	# 名单按 2026-10-16 的代码整理(调用点见 deploy/k8s/README.md「gRPC 口准入与资产通道」);新增调用方要同时改名单,否则开了就断。
	# 需要集群网络插件支持 NetworkPolicy(Calico / Cilium 等);不支持的插件上策略不生效(不断流,也不设防)。
	[ValidateSet("off", "on")]
	[string]$NetworkPolicies = "off",
	# 排行榜评分快照的发布开关(docs/design/leaderboard-system.md §2.4 / §5.4,L-20;参数形态 2026-09-20 用户拍板,§9.1#7)。
	# 写进 match ConfigMap 的 RatingSnapshotPublishEnabled,**不镜像** go/match/etc/match_service.yaml(仓内那一行只管本地,默认 false),
	# 写法照 trade 的 AssetOp.Enabled:所有档位都由本参数给值。
	#   "0"(默认):match 不发快照,评分入账路径与排行榜上线前一致(rating_gen 字段照常补写,快照 topic 照常在后台声明,分区不一致只记错误日志、不影响评分);
	#   "1":一局评分入账后,把全员当前评分发到 match-rating-snapshot(key = player_id),rank 消费写榜。
	# 这是排行榜上线的**唯一闸门**:rank 消费者就绪之后,才用 "1" 重跑 infra-up;回滚 = 用 "0" 重跑,评分链不受影响,榜停止更新但仍可读。
	# 取值同时写进 match Pod 模板的注解 mmorpg.io/rating-snapshot-publish,所以翻值必然让 match 滚动重启(ConfigMap 只在启动时读)。
	# 快照 topic 在 RatingEnabled=true 时由 match 启动声明(保留期只由 match 声明,分区数 match 与 rank 共同校验);
	# 开关为 "1" 时 match 先声明成功才起结果消费者,不会让 broker 按默认分区数自动建 topic。
	# 不粘滞(与 -AssetOps 同一口径):以 "1" 运行的环境,下一次 infra-up / all-up 不传就落回 "0"(榜停止更新),
	# 部署输出会打一行本次取值;dev_tools.ps1 的 k8s-infra-up / k8s-all-up 同名参数留空不透传 = 落回 "0"。
	# 本次不部署全局 Go 服务(zone-up、-SkipGoSvc、没给 -GoSvcRegistry)时显式传它没有效果,写路径入口会告警。
	[ValidateSet("0", "1")]
	[string]$MatchRatingSnapshotPublish = "0",

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

# C++ 日志 sidecar(见参数 -NoCppLogSidecar)。留空时写 infra namespace 里的 Loki Service。
$script:CppLogSidecarEnabled = -not $NoCppLogSidecar
$script:CppLogSidecarPushUrl = if ([string]::IsNullOrWhiteSpace($LokiPushUrl)) {
	"http://loki.${InfraNamespace}:3100/loki/api/v1/push"
} else {
	$LokiPushUrl
}
# 与业务容器共享的日志卷名;sidecar 只读挂载同一个卷。
$script:CppLogVolumeName = "node-logs"
$script:CppLogSidecarConfigMapName = "cpp-log-sidecar"

# node-logs 的容量闸。muduo 每 8MiB 滚一个新文件,并且**从不删旧文件**
# (third_party/muduo-linux/muduo/base/LogFile.cc 全文没有清理逻辑),而 node-logs 是 emptyDir,
# 不封顶的话一个长跑的 scene 就能把节点盘写满。本机 kind 的 evictionHard 全是 0%,
# DiskPressure 永远不会置位 —— 盘满的表现不是"驱逐谁",而是 MySQL / Kafka / etcd / Loki
# 一起 ENOSPC 写失败,存档有损坏风险且不会自愈。所以设两道闸:
#   ① 业务容器里一个极小的清理循环,只留最近 8 个文件(≈64MiB),每 5 分钟扫一次;
#   ② emptyDir.sizeLimit 作为远端兜底(kubelet 的 emptyDirLimitEviction 独立于 DiskPressure,
#      只驱逐越界的那个 Pod)。正常情况下 ① 让用量停在几十 MiB,碰不到 ②。
# 日志已经进 Loki(retention 7 天),本地文件只用于"exec 进去看最近一段",不需要长期留存。
# 清理循环里不出现 $ 和双引号:这段要经 PowerShell 双引号 here-string 与 YAML 流式标量两层。
$script:CppLogVolumeSizeLimit = "2Gi"
$script:CppLogPrunerCommand = "(while true; do ls -1t /app/bin/logs/cpp_nodes/*.log 2>/dev/null | tail -n +9 | xargs -r rm -f; sleep 300; done &) && "
# 提示只在一次运行里打一遍(all-up 会依次进 battle 池和每个 zone)。
$script:CppLogSidecarNoticeShown = $false

# -LoginDevPasswordAuth 的共享口令 Secret(zone namespace)。login 容器里的变量名取自库契约 LoginDevPasswordSecretEnv。
$script:LoginDevPasswordSecretName = "login-dev-password"
$script:LoginDevPasswordSecretKey = "shared-secret"
# 口令由 Initialize-InjectedSecrets 解析(只在要部署 login 且开了开关时)。
$script:LoginDevPasswordSecret = ""
# login 微信登录的 AppSecret(-LoginWeChatAppId)。部署侧从环境变量 MMORPG_LOGIN_WECHAT_APP_SECRET 读入 zone namespace 的 Secret
# login-oauth(key wechat-app-secret),经 secretKeyRef 注入 login 容器的 LOGIN_WECHAT_APP_SECRET;ConfigMap 只写变量名
# (AuthProviders.WeChat.AppSecretEnv)。login 不展开 ${VAR},生产模式也不接受内联 AppSecret(go/login/internal/config/auth_providers.go),
# 所以只能走这条路。Secret 名按用途取 login-oauth 而不是 login-wechat:以后 QQ 若启用服务端换 token 要 AppKey,同一个 Secret 加 key 即可。
# 轮换:secretKeyRef 只在容器启动时读,Pod 模板带密钥指纹注解($script:LoginOAuthSecretHashAnnotation),密钥一变模板就变,重跑 zone-up 自动滚动。
$script:LoginOAuthSecretName = "login-oauth"
$script:LoginOAuthWeChatSecretKey = "wechat-app-secret"
$script:LoginWeChatAppSecretEnv = "LOGIN_WECHAT_APP_SECRET"
$script:LoginOAuthSecretHashAnnotation = "mmorpg.io/login-oauth-secret-hash"
# 由 Initialize-InjectedSecrets 解析(只在本次部署 login 且给了 -LoginWeChatAppId 时)。
$script:LoginWeChatAppSecret = ""
# gateway 管理面(/admin/**)的 X-Admin-Key,即 Spring 属性 admin.api-key(AdminApiKeyFilter 的 @Value("${admin.api-key}"))。
# 部署侧从环境变量 MMORPG_GATEWAY_ADMIN_API_KEY 读入 zone namespace 的 Secret,经 secretKeyRef 注入 gateway 容器的 ADMIN_APIKEY,
# 不进 ConfigMap。env 名取 Spring Boot 文档的规范写法(点换下划线、去掉短横、大写);OS 环境变量优先于 jar 内与 ConfigMap 挂载的
# application.yaml,所以 git 里公开的默认值 change-me-in-production 不再生效。dev 档允许回落占位值(打警告),其它档缺失 /
# 占位 / 短于 32 位一律在写操作之前拒绝(Resolve-InjectedSecret)。
# 纵深防御:生成的 gateway ConfigMap 显式写 admin.api-key: "${ADMIN_APIKEY}"(New-JavaSvcConfigMapYaml),不靠宽松绑定。
# env 注入一旦被删 / 改名,Spring 解析不了占位符直接拒启(fail-closed),而不是静默回落到 jar 内的公开常量。
# 轮换:secretKeyRef 只在容器启动时读,Pod 模板带口令指纹注解($script:GatewayAdminApiKeyHashAnnotation),
# 口令一变模板就变,重跑 zone-up 自动滚动 gateway,旧口令随旧 Pod 退场。
$script:GatewayAdminApiKeySecretName = "gateway-admin-api-key"
$script:GatewayAdminApiKeySecretKey = "api-key"
$script:GatewayAdminApiKeyEnv = "ADMIN_APIKEY"
$script:GatewayAdminApiKeyHashAnnotation = "mmorpg.io/gateway-admin-api-key-hash"
# 由 Initialize-InjectedSecrets 解析(只在本次部署 gateway 时)。
$script:GatewayAdminApiKey = ""
# 玩家收件箱运维口令(PlayerInbox.AdminToken,docs/design/player-inbox-service.md §1.6):守 PlayerInboxAdmin 全部方法
# (GmSanctionPlayer / VoidItem / 查询 / ReconcilePlayer),调用方放在 gRPC metadata 的 x-admin-token。GM 处置是常态业务,
# 所以口令常驻(不同于 data_service 回档口令只在维护窗口临时下发),且**不与回档口令共用**。
# 部署侧从环境变量 MMORPG_PLAYER_INBOX_ADMIN_TOKEN 读入 infra namespace 的 Secret,经 secretKeyRef 注入容器的
# PLAYER_INBOX_ADMIN_TOKEN;ConfigMap 只写变量名(PlayerInbox.AdminTokenEnv),口令值绝不进 ConfigMap / manifest / 仓库
# (与 trade 的 AssetOp.SecretEnv、login 的 SharedSecretEnv 同一纪律)。
# 未注入(任何档位)= 不建 Secret、不注入 env,Go 侧读到空串即停用 PlayerInboxAdmin(fail-closed,不是免鉴权);
# 注入了:staging/prod 占位 / 短于 32 位、任何档位短于 16 字节或与其它口令相同,一律在写操作之前拒绝。
# 轮换:Pod 模板带口令指纹注解(manifest 里的占位由 Apply-OneGoSvc 替换),口令一变模板就变,重跑 infra-up 自动滚动。
$script:PlayerInboxAdminTokenSecretName = "player-inbox-admin-token"
$script:PlayerInboxAdminTokenSecretKey = "admin-token"
$script:PlayerInboxAdminTokenEnv = "PLAYER_INBOX_ADMIN_TOKEN"
# 由 Initialize-InjectedSecrets 解析(只在本次部署全局 Go 服务时)。
$script:PlayerInboxAdminToken = ""
# 邮件现役管理请求使用生产者分用途签名；密钥由domain-service-secrets注入。
# 通用资产通道签名密钥(-AssetOps,见参数处说明)。每个调用方一个单键 Secret,键名统一 secret;环境变量名与
# C++ asset_op_auth.cpp 的 kAssetOpCallerRules、go/guild 与 go/trade 的 svc.AssetOpSecretEnv 逐字一致。
# 指纹注解(Get-InjectedSecretFingerprint,以 Secret 名做域分隔)同时写在调用方(guild / trade)与 scene 的 Pod 模板上:
# 一是只换密钥时 Pod 也会滚动,二是跨命令核对两边拿的是不是同一把(Assert-AssetOpClusterConsistency)。没有注解 = 那一侧没开。
$script:AssetOpsOn = ($AssetOps -eq 'on')
$script:NetworkPoliciesOn = ($NetworkPolicies -eq 'on')
$script:AssetOpSecretKey = 'secret'
$script:AssetOpCallers = [ordered]@{
	guild = [pscustomobject]@{ Env = 'MMORPG_ASSET_OP_SECRET_GUILD'; SecretName = 'asset-op-secret-guild'; HashAnnotation = 'mmorpg.io/asset-op-secret-guild-hash' }
	trade = [pscustomobject]@{ Env = 'MMORPG_ASSET_OP_SECRET_TRADE'; SecretName = 'asset-op-secret-trade'; HashAnnotation = 'mmorpg.io/asset-op-secret-trade-hash' }
}
# guild.yaml / trade.yaml 的 Pod 模板里各有一行指纹注解占位:on 时换成指纹,off 时整行删掉(模板与引入开关之前逐字相同,不触发滚动)。
$script:AssetOpSecretHashPlaceholder = 'PLACEHOLDER_ASSET_OP_SECRET_HASH'
# 由 Initialize-InjectedSecrets 解析(-AssetOps on 时),键 = 调用方(guild / trade)。
$script:AssetOpSecrets = @{}
# gRPC 口准入名单(-NetworkPolicies on)。单一来源:生成器、契约测试与 README 都对照这张表。gRPC 口:scene = RPC_PORT + 30000,
# guild / match = $GoSvcCatalogue 的 Port。2026-10-16 按代码整理的调用方(谁会拨这个 gRPC 口):
#   scene:scene_manager(go/scene_manager scene_node_client)、match(go/match node_watcher)、player_inbox(go/player_inbox notify)、
#          guild / trade(go/shared/assetop 资产通道)。gate ↔ scene 走 muduo 节点口(RPC_PORT),battle → scene 走 Kafka 命令,
#          client-rpc-router 的路由表里没有 SceneNodeService,都不经 scene 的 gRPC 口。
#   guild:client-rpc-router(路由表 GuildNodeService)、data_service(GuildInternalRpc,回档帮会闸);match 的历练结果走 Kafka。
#   match:client-rpc-router(路由表 MatchNodeService)、guild(MatchRpc,历练开战)、gate(只在 -GateRouterMode 0 时直连)。
# 名单是 app 标签;运维工具(如 go/guild/cmd/assetopfix)要在名单内的 Pod 里跑,或临时关掉策略。
$script:GrpcAllowLists = [ordered]@{
	scene = [pscustomobject]@{ PolicyName = 'scene-grpc-allowlist'; TargetApps = @('scene', 'scene-world', 'scene-instance'); AllowedApps = @('scene-manager', 'match', 'player-inbox', 'guild', 'trade', 'mail') }
	match = [pscustomobject]@{ PolicyName = 'match-grpc-allowlist'; TargetApps = @('match'); AllowedApps = @('client-rpc-router', 'guild', 'gate') }
}
# 共享 Redis 口令(MMORPG_REDIS_PASSWORD),由 Initialize-InjectedSecrets 解析:dev 档未注入时是空串。$null = 还没解析,
# C++ 节点配置的生成器(Get-CppZoneRedisPassword)据此拒绝生成,不把「没解析」当成「没口令」写出空串。
$script:RedisPassword = $null
# C++ 节点(gate / scene / battle)Pod 模板上的 zone Redis 口令指纹注解,见 Get-CppZoneRedisAuthAnnotations。
$script:ZoneRedisAuthHashAnnotation = 'mmorpg.io/zone-redis-auth-hash'

# 削峰(G4b)开服档起步值。单一来源:ConfigMap 生成、Pod 指纹、部署打印都只读这里。全部「待压测确认」(L-14)。
#   ZoneDefaultRps/Burst   docs/design/open-server-rate-limit-design.md「Layer 1 配额参考」的「开服日 / 大版本回归」档
#                          (负责人 2026-10-13 拍板按该档起步)
#   IpRps/IpBurst          java/gateway_node jar 内 application.yaml 的 gate.rate-limit.ip-rps / ip-burst(设计稿 §5.2 示例为 2/10;
#                          取宽者:网吧 / 运营商 NAT 下一次登录要扣 2 个 IP 令牌,/api/login 与 /api/assign-gate 共用 rl:ip:*)
#   AccountCooldownMs      设计稿 §5.2 与 jar 内 application.yaml 的 account-cooldown-ms 一致
#   LoginQueueZoneCapacity docs/design/login-queue-2026-05.md §10 与 go/login/etc/login.yaml Queue 段注释里的示例值。
#                          注意它是**本 zone 同时在线上限**,不是速率。
$script:PeakShavingLaunchTier = [ordered]@{
	ZoneDefaultRps         = 2000
	ZoneDefaultBurst       = 5000
	IpRps                  = 5
	IpBurst                = 20
	AccountCooldownMs      = 5000
	LoginQueueZoneCapacity = 5000
}
$script:PeakShavingHashAnnotation = 'mmorpg.io/peak-shaving-hash'
# 由 Initialize-InjectedSecrets 解析(只在本次部署 login 且排队开时)。
$script:LoginQueueTokenSecret = ""

# 登录排队(G4b-2)合入门槛的机械门禁(2026-10-15)。出处:docs/handoff/launch-readiness-20261013/spec_G4b.md §7「合入门槛」
# 与 §12 合入顺序。G4b-2 的 login 段已先于门槛随 e4ff1d165 / ebccaf04d 入库(越序),所以由这个常量挡住打开排队。三项门槛:
#   ① G1b 规格指定的多 zone 用例(L-01 验收标准:每个 zone 的队列都能出队;spec_G1b.md T3 / T8a)由 Codex 跑绿;
#   ② G4b-G(spec_G4b-G.md,在途占位逐个到期)由 Codex 跑绿;
#   ③ spec_G4b-G.md §8.4 的真实 Redis 最小冒烟跑通,命令原文与输出摘要记进 docs/PROGRESS.md(miniredis 的结果不能顶替)。
# spec §7 原列的「契约用例 15 绿」不单列:它属于 k8s_deploy_contract.tests.ps1,改本常量的提交必须改写用例 19 / 20 并重跑整套契约测试
# (本文件在 tools/scripts/** 下,也会触发 deploy-config-tests.yml),因此一定会重跑;它只核对 G1b / G4b-G 的代码形状,代替不了 ①②。
# 三项都满足、docs/PROGRESS.md 有记录之后,由负责人在同一提交里把它改为 $true,并在这里写上那条 PROGRESS 条目的标题。
# 为 $false 时(Resolve-LoginQueueSwitch):-LoginQueue auto 在任何档位都解析为关,-LoginQueue on 在参数解析后、任何 kubectl 之前拒绝。
# 本脚本只拦得住经它的部署:门槛未清时手改 login ConfigMap 或 go/login/etc/login.yaml 的 Queue.Enabled 同样违规(文档约束,脚本拦不住)。
# 清门槛本身要跑的真实 Redis 冒烟用本地栈 + scratchpad 里的 login 配置副本(Queue.Enabled=true),不经过本脚本,不受这道门禁影响。
# 只能写字面量 $true / $false(契约测试按 AST 核对):不要改成读参数或环境变量,否则门禁就成了一个开关。
$script:LoginQueueMergeGateCleared = $false

# -GatewayRateLimit / -LoginQueue 的取值规则:on / off 为显式覆盖,auto 跟随发布档位(dev 关,其余开)。
# -LoginQueue 另经 Resolve-LoginQueueSwitch 套一层合入门槛;门槛已清时两者结论相同。
. (Join-Path $PSScriptRoot "maintenance_drain.ps1")

function Resolve-PeakShavingSwitch {
	param(
		[Parameter(Mandatory = $true)][string]$Value,
		# 参数名不要用 $Profile:它会遮住 PowerShell 自动变量 $PROFILE。
		[Parameter(Mandatory = $true)][string]$ReleaseProfileName
	)
	switch ($Value) {
		'on'  { return $true }
		'off' { return $false }
		default { return ($ReleaseProfileName -ne 'dev') }
	}
}
# -LoginQueue 的最终取值:在 Resolve-PeakShavingSwitch 之上套合入门槛(见 $script:LoginQueueMergeGateCleared 的注释)。
# 纯函数,只看参数:契约测试按 AST 把它和 Resolve-PeakShavingSwitch 一起抽出来逐组合调用。
#   门槛已清(-MergeGateCleared,或 -DryRunMode 下 -TestOverride 为 1):结论与 Resolve-PeakShavingSwitch 完全相同;
#   门槛未清:auto / off 在任何档位都是关;on 直接 throw。
# -TestOverride 是契约测试专用接缝(环境变量 MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED),只在 -DryRun 下生效,让依赖「排队开」的
# DryRun 用例在门槛清掉之前也能跑。不带 -DryRun 时只要设置了它就 throw,常量为 $true 时也一样(fail-closed:它放不开真实部署,
# 也不让人误以为能放开);值不是 1 也 throw(不猜 0 / true 之类的含义)。
function Resolve-LoginQueueSwitch {
	param(
		[Parameter(Mandatory = $true)][string]$Value,
		[Parameter(Mandatory = $true)][string]$ReleaseProfileName,
		[Parameter(Mandatory = $true)][bool]$MergeGateCleared,
		[Parameter(Mandatory = $true)][bool]$DryRunMode,
		[Parameter(Mandatory = $true)][AllowNull()][AllowEmptyString()][string]$TestOverride
	)
	$testOverrideSet = -not [string]::IsNullOrEmpty($TestOverride)
	if ($testOverrideSet -and -not $DryRunMode) {
		throw '环境变量 MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED 是契约测试专用接缝,只在 -DryRun 下生效;不带 -DryRun 时设置了它即拒绝(fail-closed:它放不开登录排队的合入门槛)。清除该变量后重跑;合入门槛见 k8s_deploy.ps1 的 $script:LoginQueueMergeGateCleared。'
	}
	if ($testOverrideSet -and $TestOverride -cne '1') {
		throw '环境变量 MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED 只接受 1(契约测试专用,只在 -DryRun 下生效),当前值不是 1,拒绝猜测其含义。'
	}
	if ($MergeGateCleared -or ($DryRunMode -and $testOverrideSet)) {
		return (Resolve-PeakShavingSwitch -Value $Value -ReleaseProfileName $ReleaseProfileName)
	}
	if ($Value -eq 'on') {
		throw '-LoginQueue on 被拒绝:登录排队(G4b-2)的合入门槛未清($script:LoginQueueMergeGateCleared = $false)。三项门槛须全部满足:① G1b 规格指定的多 zone 用例由 Codex 跑绿;② G4b-G 由 Codex 跑绿;③ spec_G4b-G.md §8.4 的真实 Redis 最小冒烟跑通,命令原文与输出摘要记进 docs/PROGRESS.md。出处:docs/handoff/launch-readiness-20261013/spec_G4b.md §7「合入门槛」。在此之前任何环境都不打开排队(含手改 Queue.Enabled);三项都满足后由负责人把该常量改为 $true。'
	}
	return $false
}
# 两个开关的解析结果。脚本级、参数绑定后算一次:生成器、指纹与部署打印读的是同一个布尔值。
$script:GatewayRateLimitOn = Resolve-PeakShavingSwitch -Value $GatewayRateLimit -ReleaseProfileName $ReleaseProfile
# 登录排队另套合入门槛。门槛未清时 -LoginQueue on 在这里 throw,早于加载 release_common.ps1 与任何 kubectl;*-down / *-status 同样拒绝
# (门槛未清时 on 本身就是违规输入,与命令无关;不带 -DryRun 却设置了测试接缝变量同理)。测试接缝变量只在这里读一次,
# 生效与否由 -DryRunMode 决定 —— 契约测试核对这里绑的是本脚本的 -DryRun,且 $script:LoginQueueOn 只在这一处赋值。
$script:LoginQueueOn = Resolve-LoginQueueSwitch -Value $LoginQueue -ReleaseProfileName $ReleaseProfile `
	-MergeGateCleared $script:LoginQueueMergeGateCleared -DryRunMode $DryRun.IsPresent -TestOverride $env:MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED
# -GrpcServerMaxPollers 已退役(见参数处注释):只提示,不 throw —— 旧命令带着它照常部署,生成物里不再有该环境变量。
if ($GrpcServerMaxPollers -gt 0) {
	Write-Warning "-GrpcServerMaxPollers is retired (the C++ gRPC server has no poller threads since batch 4, see docs/design/grpc-muduo-transport.md section 15.11); ignoring value=$GrpcServerMaxPollers. GRPC_SERVER_MAX_POLLERS is no longer injected."
}
# B0-4 豁免开关(见参数处注释):warn 只用于短时应急,每次部署都打一行,免得被当成常态配置。
if ($NodeOpenFileLimitMode -ceq 'warn') {
	Write-Warning "-NodeOpenFileLimitMode warn: C++ workloads rendered by this run get NODE_OPEN_FILE_LIMIT_MODE=warn and start even when the nofile hard limit is below the requirement; once the fd table is full, reconnects and log rotation fail with EMFILE. Emergency use only; prefer lowering GateMaxConnections / BattleMaxConnections (docs/ops/gate-kernel-tuning-runbook.md section 3.1). Not sticky: the next deploy without it falls back to enforce."
}
# -RedisSplit 的 ValidateSet 不分大小写(QUEUE 也能进来),参数绑定后规范成小写,后面的判定与打印一律按小写。
$RedisSplit = $RedisSplit.ToLowerInvariant()

. (Join-Path $ScriptDir "lib\release_common.ps1")
# 集群外客户端入口(D76–D93)的生成器、preflight、就绪等待与模式切换清理。库函数不读本脚本的任何变量,
# 碰集群的函数一律显式传 -KubeContext / -KubeConfig(漏传不会悄悄落到本机默认 context)。
. (Join-Path (Join-Path $ScriptDir "lib") "k8s_client_entry.ps1")
# kafka-consumer-groups --describe 的分类(Resolve-KafkaGroupTopicOffsets)、db_task topic 名(Get-DbTaskTopicName)与线上 db 消费组形状
# (ConvertFrom-LiveDbConsumerGroup,2026-10-18 从本文件移入),与 kafka_offset_reset.ps1、k8s_zone_rollback.ps1 同一份规则;纯函数库,dot-source 无副作用。
. (Join-Path $ScriptDir "lib" "kafka_consumer_groups.ps1")

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

# K8s 枚举区分大小写,PowerShell 的 ValidateSet 既不分也不改写:-GateServiceType nodeport 能过参数校验,原样写进
# Service 的 type: 会被 API server 拒收(那时另一种形态的 gate 可能已经删了)。参数绑定后统一规范成标准写法,
# 之后所有模板(含 gate-entry 的 type: $GateServiceType)拿到的都是它。取值已被 ValidateSet 限定,规范化必然命中。
$GateServiceType = ConvertTo-ClientEntryCanonicalName -Value $GateServiceType -Allowed @("ClusterIP", "NodePort", "LoadBalancer")
$GateExternalTrafficPolicy = ConvertTo-ClientEntryCanonicalName -Value $GateExternalTrafficPolicy -Allowed @("Local", "Cluster")
$ImagePullPolicy = ConvertTo-ClientEntryCanonicalName -Value $ImagePullPolicy -Allowed @("Always", "IfNotPresent", "Never")
# 写进 ConfigMap 的是 YAML 布尔字面量,统一成小写(True / TRUE 在不同 YAML 库里的解释不一致)。
$RequireClientEndpoint = ConvertTo-ClientEntryCanonicalName -Value $RequireClientEndpoint -Allowed @("auto", "true", "false")
# 规范成小写(ValidateSet 不分大小写):决策表、打印行与注解都按小写取值比较。
$DbConsumerGroup = ConvertTo-ClientEntryCanonicalName -Value $DbConsumerGroup -Allowed @("keep", "zone", "legacy")
# 显式切组只许单 zone 的 zone-up(理由见参数注释)。排在任何 kubectl 之前;zone-down / *-status 同样拒绝:
# 显式模式本身就是一次部署决定,配在不部署 db 的命令上只会让人误以为切过了。
if ($DbConsumerGroup -ne "keep" -and $Command -ne "zone-up") {
	throw "-DbConsumerGroup $DbConsumerGroup 只允许与 -Command zone-up 一起用(逐 zone 切组 / 反向交接);$Command 请用默认的 keep。"
}
# D1 映射提交点模式写进 ConfigMap 的是 data_service 区分大小写的字面量(见参数 -RouteStoreMode)。
$RouteStoreMode = ConvertTo-ClientEntryCanonicalName -Value $RouteStoreMode -Allowed @("redis", "db_first")
if ($Command -in @('zone-up','all-up','infra-up') -and -not $SkipGoSvc -and -not [string]::IsNullOrWhiteSpace($GoSvcRegistry)) {
	if ($RouteStoreMode -cne 'db_first' -or -not $RouteReconcileArm -or -not $RouteReadRequiresReady -or -not $DbPlacementRequireRouteReady -or -not $GuildRequireRouteReady) {
		throw '正式全球服务必须db_first及全部Arm/读ready；历史Redis只保留隔离工具/拒绝测试，禁止部署成映射权威。'
	}
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
		Write-Warning "Release preflight skipped via -SkipPreflight (ReleaseProfile=$ReleaseProfile). This path should not be used for actual releases."
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
	解析 login 微信登录的 AppSecret(环境变量 MMORPG_LOGIN_WECHAT_APP_SECRET),返回值;不可用一律 throw。由 Initialize-InjectedSecrets
	在写路径入口调用(只在本次部署 login 且给了 -LoginWeChatAppId 时)。

.DESCRIPTION
	- **任何档位都没有回落值**:login 对 AppSecretEnv 指向的变量缺失 / 为空一律拒启(go/login/internal/config/auth_providers.go
	  resolveAuthCredential),生成器不替运维猜一个,未设置就在任何写操作之前拒绝。staging / prod 的「未设置 / 占位 / 短于 16 位」
	  由 Resolve-InjectedSecret 按它的原文 throw;dev 档 Resolve-InjectedSecret 回落空串,这里补一次拒绝。
	- 各档都拒首尾空白与 `${`:login 对这两种值同样拒启(checkCredentialValue;常见于 Secret 带进的行尾换行、照旧文档写的占位符),
	  部署出来只会 CrashLoopBackOff,不如在写操作之前拦下。
	- 报错只点环境变量名,不回显值。读环境变量、不读本脚本的变量,契约测试按 AST 抽出来直接调。
#>
function Resolve-LoginWeChatAppSecret {
	param([Parameter(Mandatory = $true)][ValidateSet('dev', 'staging', 'prod')][string]$ReleaseProfile)

	$value = Resolve-InjectedSecret -EnvName "MMORPG_LOGIN_WECHAT_APP_SECRET" -DevFallback "" -ReleaseProfile $ReleaseProfile `
		-Purpose "login 微信登录 AppSecret(AuthProviders.WeChat.AppSecretEnv)" -MinLength 16
	if ([string]::IsNullOrWhiteSpace($value)) {
		throw "-LoginWeChatAppId 需要环境变量 MMORPG_LOGIN_WECHAT_APP_SECRET(微信开放平台应用的 AppSecret,经 Secret 注入 login),当前未设置,拒绝部署(任何档位都没有回落值)。"
	}
	if ($value -cne $value.Trim()) {
		throw "环境变量 MMORPG_LOGIN_WECHAT_APP_SECRET 的值含首尾空白(常见于带进的行尾换行):login 拒绝这种 AppSecret,部署出来只会拒启。"
	}
	if ($value.Contains('${')) {
		throw "环境变量 MMORPG_LOGIN_WECHAT_APP_SECRET 的值含未展开的 `${...}:login 不展开环境变量,对这种 AppSecret 拒绝启动。"
	}
	return $value
}

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
	# 同一个口令要原样写进多份 YAML 双引号标量(C++ 的 node-config / battle-node-config、各 Go 服务与 gateway 的 ConfigMap、
	# infra 的 Secret redis-auth)。字符集在这里、任何写操作之前查一次,坏值不留半截部署。
	Assert-RedisPasswordYamlSafe -Password $script:RedisPassword
	# Java 网关数据源。dev 回落值以 deploy/docker-compose.yml + gateway_node
	# application.yaml 的口径为准(appuser/apppass123):mysql.yaml 用 MYSQL_USER 建的
	# 就是这个账号,且 MYSQL_DATABASE=mmorpg 会自动授权给它。以前回落 root/123456,
	# 而集群 root 密码是 Mmorpg#2026db —— dev 档的网关在 K8s 上连库必然失败。
	$script:GatewayDbUser = Resolve-InjectedSecret -EnvName "MMORPG_GATEWAY_DB_USER" `
		-DevFallback "appuser" -ReleaseProfile $ReleaseProfile -Purpose "Java Gateway 数据源用户名" -MinLength 1
	$script:GatewayDbPassword = Resolve-InjectedSecret -EnvName "MMORPG_GATEWAY_DB_PASSWORD" `
		-DevFallback "apppass123" -ReleaseProfile $ReleaseProfile -Purpose "Java Gateway 数据源密码" -MinLength 12

	# -LoginDevPasswordAuth 的共享口令(开发账号的登录口令,robot yaml 的 password 要与它一致)。只在本次会部署 login 时解析;
	# 开关只允许 dev 档(Assert-ClientEntryDeployPreflight 已先拒其它档位)。刻意**没有**回落值:口令是运维 / 本机显式决定的,
	# 生成器不替它猜一个 —— 未设置就在任何写操作之前拒绝(与 login 对空密钥 panic 同一口径)。
	$script:LoginDevPasswordSecret = ""
	if ($LoginDevPasswordAuth -and (Test-ZoneLoginDeployed)) {
		$script:LoginDevPasswordSecret = Resolve-InjectedSecret -EnvName "MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET" `
			-DevFallback "" -ReleaseProfile $ReleaseProfile -Purpose "login 开发口令认证共享口令(DevPasswordAuth)" -MinLength 1
		if ([string]::IsNullOrWhiteSpace($script:LoginDevPasswordSecret)) {
			throw "-LoginDevPasswordAuth 需要环境变量 MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET(开发账号的共享登录口令,robot yaml 的 password 须与它一致),当前未设置,拒绝部署。"
		}
	}

	# login 微信登录的 AppSecret(见 $script:LoginOAuthSecretName 处的说明)。只在本次部署 login 且给了 -LoginWeChatAppId 时解析:
	# infra-up、-SkipGoSvc 与不开微信的部署都不因它被阻断。各档位都没有回落值、缺失即拒绝,规则见 Resolve-LoginWeChatAppSecret。
	# QQ 不需要密钥,这里没有对应项。
	$script:LoginWeChatAppSecret = ""
	if (-not [string]::IsNullOrEmpty($LoginWeChatAppId) -and (Test-ZoneLoginDeployed)) {
		$script:LoginWeChatAppSecret = Resolve-LoginWeChatAppSecret -ReleaseProfile $ReleaseProfile
	}

	# 登录排队令牌的 HMAC 密钥(login Secrets.QueueToken,G4b)。生产模式开了排队就必需,且不得与 GateToken / InternalAuth 复用
	# (go/login/internal/config/secrets.go ResolveSecrets 的必填与复用检查;不满足 login 拒启 → CrashLoopBackOff)。
	# 只在本次部署 login 且排队开时解析:关排队 / infra-up / -SkipGoSvc 都不因它被阻断。dev 档回落空串
	# (不写 Secrets.QueueToken → login 宽松档回落 GateToken 密钥并告警)。
	# 与另两把用 -ceq 原样比较(不 trim、区分大小写,同 Go 侧按字节比);报错只点环境变量名,不回显值。
	# 任何档位只要非空就比:Go 宽松档(Mode dev / test)对复用只打 WARN,部署侧统一从严,不让 dev 上的复用配置被带进 staging。
	$script:LoginQueueTokenSecret = ""
	if ($script:LoginQueueOn -and (Test-ZoneLoginDeployed)) {
		$script:LoginQueueTokenSecret = Resolve-InjectedSecret -EnvName "MMORPG_LOGIN_QUEUE_TOKEN_SECRET" `
			-DevFallback "" -ReleaseProfile $ReleaseProfile -Purpose "login 登录排队令牌 HMAC 密钥(Secrets.QueueToken)" -MinLength 32
		if (-not [string]::IsNullOrEmpty($script:LoginQueueTokenSecret)) {
			if ($script:LoginQueueTokenSecret -ceq $script:GateTokenSecret) {
				throw "MMORPG_LOGIN_QUEUE_TOKEN_SECRET 必须与 MMORPG_GATE_TOKEN_SECRET 不同:login 拒绝跨用途复用主密钥(go/login/internal/config/secrets.go 的复用检查),部署出来只会 CrashLoopBackOff。"
			}
			if ($script:LoginQueueTokenSecret -ceq $script:InternalAuthSecret) {
				throw "MMORPG_LOGIN_QUEUE_TOKEN_SECRET 必须与 MMORPG_INTERNAL_AUTH_SECRET 不同:login 拒绝跨用途复用主密钥(go/login/internal/config/secrets.go 的复用检查),部署出来只会 CrashLoopBackOff。"
			}
		}
	}

	# gateway 管理面口令(见 $script:GatewayAdminApiKeyEnv 处的说明)。只在本次部署 gateway 时解析:infra-up 与不带 Java 服务的
	# zone-up 不因它被阻断。非 dev 档缺失 / 占位 / 过短由 Resolve-InjectedSecret throw;dev 档回落占位值,照样经 Secret 注入
	# (注入路径各档一致),但打警告 —— 管理面只在集群内可达(Ingress 只路由 /api),占位口令仍等于对集群内任何 Pod 开放。
	$script:GatewayAdminApiKey = ""
	if (Test-ZoneGatewayDeployed) {
		$script:GatewayAdminApiKey = Resolve-InjectedSecret -EnvName "MMORPG_GATEWAY_ADMIN_API_KEY" `
			-DevFallback "change-me-in-production" -ReleaseProfile $ReleaseProfile -Purpose "Java Gateway 管理面口令(X-Admin-Key / admin.api-key)" -MinLength 32
		if (Test-PlaceholderSecret -Value $script:GatewayAdminApiKey) {
			Write-Warning "gateway admin password is a placeholder (MMORPG_GATEWAY_ADMIN_API_KEY unset or still a placeholder, ReleaseProfile=$ReleaseProfile): /admin/** authenticates with a public constant; allowed only for local dev. Non-dev profiles reject deployment."
		}
	}

	# 玩家收件箱运维口令(见 $script:PlayerInboxAdminTokenSecretName 处的说明)。只在本次部署全局 Go 服务时解析:zone-up 与
	# -SkipGoSvc 不因它被阻断。**各档位都没有回落值**:未注入就是停用运维接口(fail-closed,设计 §1.6「为空就停用」),
	# 不替运维猜一个公开口令,也不因为没开 GM 运维面就挡住整次 infra-up。注入了就按档位校验:非 dev 档占位 / 短于 32 位
	# 由 Resolve-InjectedSecret throw。与其它几把口令原样比较(-ceq,不回显值):一把口令跨用途复用,泄露一处就等于两处同时失守。
	$script:PlayerInboxAdminToken = ""
	if (Test-GlobalGoSvcDeployed) {
		if ([string]::IsNullOrWhiteSpace([System.Environment]::GetEnvironmentVariable("MMORPG_PLAYER_INBOX_ADMIN_TOKEN"))) {
			Write-Host "player-inbox: MMORPG_PLAYER_INBOX_ADMIN_TOKEN unset (ReleaseProfile=$ReleaseProfile); PlayerInboxAdmin stays disabled (fail-closed), no Secret is created."
		}
		else {
			$script:PlayerInboxAdminToken = Resolve-InjectedSecret -EnvName "MMORPG_PLAYER_INBOX_ADMIN_TOKEN" `
				-DevFallback "" -ReleaseProfile $ReleaseProfile -Purpose "玩家收件箱运维口令(PlayerInbox.AdminToken / x-admin-token)" -MinLength 32
			# dev 档 Resolve-InjectedSecret 不查长度;服务端非空口令至少 16 字节(go/player_inbox config.MinAdminTokenBytes),
			# 短了 Pod 拒启 CrashLoop —— 在写操作之前拦下。
			if ($script:PlayerInboxAdminToken.Length -lt 16) {
				throw "MMORPG_PLAYER_INBOX_ADMIN_TOKEN 长度 $($script:PlayerInboxAdminToken.Length) < 16:player_inbox 对非空口令要求至少 16 字节(config.MinAdminTokenBytes),部署出来只会拒启。"
			}
			Assert-PlayerInboxAdminTokenEnvDeclared
			foreach ($other in @(
				@{ Env = 'MMORPG_GATE_TOKEN_SECRET'; Value = $script:GateTokenSecret }
				@{ Env = 'MMORPG_INTERNAL_AUTH_SECRET'; Value = $script:InternalAuthSecret }
				@{ Env = 'MMORPG_MYSQL_PASSWORD'; Value = $script:MysqlPassword }
				@{ Env = 'MMORPG_GATEWAY_ADMIN_API_KEY'; Value = $script:GatewayAdminApiKey }
			)) {
				if (-not [string]::IsNullOrEmpty($other.Value) -and $script:PlayerInboxAdminToken -ceq $other.Value) {
					throw "MMORPG_PLAYER_INBOX_ADMIN_TOKEN 必须与 $($other.Env) 不同:运维口令不得跨用途复用(docs/design/player-inbox-service.md §1.6)。"
				}
			}
		}
	}

	# 邮件运维使用 serviceauth 的 admin 生产者签名；值由现役 domain-service-secrets 注入，不新建 M1 token。

	# 通用资产通道签名密钥(-AssetOps on,见参数处说明)。各档位都没有回落值:同一把密钥要在 infra-up(guild / trade 签名)与每一次
	# zone-up(scene 验签)之间保持同值,生成器替运维随机出一个值只会让两次运行对不上。本机 dev 的随机值(lib/assetop_dev_secret.ps1)
	# 只给本机进程用,部署链不读它。报错只点环境变量名,不回显值。
	$script:AssetOpSecrets = @{}
	if ($script:AssetOpsOn) {
		foreach ($caller in $script:AssetOpCallers.Keys) {
			$spec = $script:AssetOpCallers[$caller]
			$value = Resolve-InjectedSecret -EnvName $spec.Env -DevFallback "" -ReleaseProfile $ReleaseProfile `
				-Purpose "通用资产通道签名密钥(调用方 $caller)" -MinLength 32
			Assert-AssetOpSecretShape -EnvName $spec.Env -Value $value
			$script:AssetOpSecrets[$caller] = $value
		}
		if ($script:AssetOpSecrets['guild'] -ceq $script:AssetOpSecrets['trade']) {
			throw "MMORPG_ASSET_OP_SECRET_GUILD 必须与 MMORPG_ASSET_OP_SECRET_TRADE 不同:每个调用方一把密钥(不变量 I6),同值等于任何一方都能冒充另一方签资产指令。"
		}
		$otherSecrets = @(
			@{ Env = 'MMORPG_GATE_TOKEN_SECRET'; Value = $script:GateTokenSecret }
			@{ Env = 'MMORPG_INTERNAL_AUTH_SECRET'; Value = $script:InternalAuthSecret }
			@{ Env = 'MMORPG_MYSQL_PASSWORD'; Value = $script:MysqlPassword }
			@{ Env = 'MMORPG_REDIS_PASSWORD'; Value = $script:RedisPassword }
			@{ Env = 'MMORPG_GATEWAY_DB_PASSWORD'; Value = $script:GatewayDbPassword }
			@{ Env = 'MMORPG_GATEWAY_ADMIN_API_KEY'; Value = $script:GatewayAdminApiKey }
			@{ Env = 'MMORPG_PLAYER_INBOX_ADMIN_TOKEN'; Value = $script:PlayerInboxAdminToken }
			@{ Env = 'MAIL_PRODUCER_SECRET_ADMIN'; Value = [Environment]::GetEnvironmentVariable('MAIL_PRODUCER_SECRET_ADMIN') }
			@{ Env = 'MMORPG_LOGIN_QUEUE_TOKEN_SECRET'; Value = $script:LoginQueueTokenSecret }
			@{ Env = 'MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET'; Value = $script:LoginDevPasswordSecret }
			@{ Env = 'MMORPG_BATTLE_TOKEN_SECRET'; Value = [System.Environment]::GetEnvironmentVariable('MMORPG_BATTLE_TOKEN_SECRET') }
		)
		foreach ($caller in $script:AssetOpCallers.Keys) {
			foreach ($other in $otherSecrets) {
				if (-not [string]::IsNullOrEmpty($other.Value) -and $script:AssetOpSecrets[$caller] -ceq $other.Value) {
					throw "$($script:AssetOpCallers[$caller].Env) 必须与 $($other.Env) 不同:资产签名密钥不得跨用途复用,泄露一处就等于两处同时失守。"
				}
			}
		}
	}
}

<#
.SYNOPSIS
	资产通道签名密钥的形状检查(纯函数,契约测试按 AST 抽出直接调用)。报错只点环境变量名,不回显值。

.DESCRIPTION
	- 空:-AssetOps on 却没注入(dev 档 Resolve-InjectedSecret 不 throw,在这里拦);
	- 含任何空白:Go 侧(assetop.NewSigner)按 Unicode 空白 TrimSpace、C++ 侧(VerifyAssetOpAuth)按 ASCII TrimAscii,遇到 NBSP 一类字符
	  两边算出的 HMAC 就分家,表现是每一次资产 RPC 都回 27008 且日志里没有线索(lib/assetop_dev_secret.ps1 头注释),干脆不收;
	- 短于 32 字节:与 go/shared/assetop.MinSecretLen、C++ kAssetOpAuthMinSecretBytes 同值,短了 guild / trade 带着 Enabled=true 拒启。
	  dev 档 Resolve-InjectedSecret 不查长度,所以各档位都在这里再查一次。
#>
function Assert-AssetOpSecretShape {
	param(
		[Parameter(Mandatory = $true)][string]$EnvName,
		[Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()][string]$Value
	)

	if ([string]::IsNullOrEmpty($Value)) {
		throw "-AssetOps on 需要环境变量 ${EnvName}(通用资产通道签名密钥,≥32 字节),当前未设置,拒绝部署。各档位都没有回落值:同一把密钥要在 infra-up 与每一次 zone-up 之间保持同值。"
	}
	if ($Value -match '\s') {
		throw "环境变量 $EnvName 含空白字符:Go 与 C++ 两侧去空白的口径不同,含空白的密钥会让验签静默分家(每一次资产 RPC 回 27008),拒绝部署。"
	}
	if ($Value.Length -lt 32) {
		throw "环境变量 $EnvName 长度 $($Value.Length) < 32:guild / trade 对资产签名密钥要求至少 32 字节(assetop.MinSecretLen),带着 Enabled=true 会拒启。"
	}
}

# 本次要写进 Pod 模板的资产密钥指纹:off = 空表(不写注解);on = 调用方 → 指纹(guild / trade 与 scene 用同一个值)。
function Get-AssetOpDesiredHashes {
	$hashes = [ordered]@{}
	if ($script:AssetOpsOn) {
		foreach ($caller in $script:AssetOpCallers.Keys) {
			$spec = $script:AssetOpCallers[$caller]
			$hashes[$caller] = Get-InjectedSecretFingerprint -SecretName $spec.SecretName -Value $script:AssetOpSecrets[$caller]
		}
	}
	return $hashes
}

# 签名方(调用方)与验签方(scene)的指纹相容性(纯函数,契约测试直接喂值)。空串 / $null = 那一侧没开(没有指纹注解)。
#   ok                         调用方没开,或两边指纹相同
#   scene-off-while-caller-on  调用方开着而 scene 没开:该 zone 的每一次资产 RPC 都会被 scene 拒绝
#   key-mismatch               两边都开但指纹不同:密钥不是同一把,同样每一次都被拒
function Get-AssetOpConsistencyVerdict {
	param(
		[AllowNull()][AllowEmptyString()][string]$CallerHash,
		[AllowNull()][AllowEmptyString()][string]$SceneHash
	)

	if ([string]::IsNullOrEmpty($CallerHash)) { return 'ok' }
	if ([string]::IsNullOrEmpty($SceneHash)) { return 'scene-off-while-caller-on' }
	if ($CallerHash -cne $SceneHash) { return 'key-mismatch' }
	return 'ok'
}

# 线上 Deployment 的 Pod 模板注解值;Deployment 或注解不存在返回 $null。查询失败 fail-closed(throw)。
function Get-LiveDeploymentPodAnnotation {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$Name,
		[Parameter(Mandatory = $true)][string]$Key
	)

	$result = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'deployment', $Name, '-n', $Namespace, '--ignore-not-found', '-o', 'json') `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
	if ($result.ExitCode -ne 0) {
		throw "查询线上 Deployment $Name 失败(namespace=$Namespace,exit $($result.ExitCode)):$($result.Stderr)。fail-closed:不知道现状就无法判断资产通道两侧是否对得上。"
	}
	if ([string]::IsNullOrWhiteSpace($result.Stdout)) { return $null }
	$document = $result.Stdout | ConvertFrom-Json
	$annotations = $document.spec.template.metadata.annotations
	if ($null -eq $annotations) { return $null }
	$property = $annotations.PSObject.Properties[$Key]
	if ($null -eq $property) { return $null }
	return [string]$property.Value
}

# 全部 namespace 里副本数 > 0 的 scene 工作负载(Deployment;集群注册了 Agones 时含 Fleet)及其 Pod 模板注解。查询失败 fail-closed。
function Get-LiveSceneWorkloads {
	$selector = 'app in (' + ($script:GrpcAllowLists['scene'].TargetApps -join ',') + ')'
	$workloads = @()
	$result = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'deployments', '--all-namespaces', '-l', $selector, '-o', 'json') `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
	if ($result.ExitCode -ne 0) {
		throw "查询线上 scene Deployment 失败(exit $($result.ExitCode)):$($result.Stderr)。fail-closed:不知道 scene 是否已拿到验签密钥,就不打开调用方。"
	}
	foreach ($item in @(($result.Stdout | ConvertFrom-Json).items)) {
		$replicas = if ($null -eq $item.spec.replicas) { 1 } else { [int]$item.spec.replicas }
		$workloads += [pscustomobject]@{ Kind = 'Deployment'; Namespace = $item.metadata.namespace; Name = $item.metadata.name; Replicas = $replicas; Annotations = $item.spec.template.metadata.annotations }
	}
	if (Test-AgonesFleetCrdPresent -KubeContext $KubeContext -KubeConfig $KubeConfig) {
		$fleetResult = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'fleets.agones.dev', '--all-namespaces', '-l', $selector, '-o', 'json') `
			-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
		if ($fleetResult.ExitCode -ne 0) {
			throw "查询线上 scene Fleet 失败(exit $($fleetResult.ExitCode)):$($fleetResult.Stderr)。fail-closed。"
		}
		foreach ($item in @(($fleetResult.Stdout | ConvertFrom-Json).items)) {
			$replicas = if ($null -eq $item.spec.replicas) { 1 } else { [int]$item.spec.replicas }
			# Fleet → GameServer 模板 → Pod 模板,注解在第二层 template 的 metadata 上(New-SceneFleetYaml)。
			$workloads += [pscustomobject]@{ Kind = 'Fleet'; Namespace = $item.metadata.namespace; Name = $item.metadata.name; Replicas = $replicas; Annotations = $item.spec.template.spec.template.metadata.annotations }
		}
	}
	return @($workloads | Where-Object { $_.Replicas -gt 0 })
}

<#
.SYNOPSIS
	-AssetOps 的跨命令一致性(写路径入口、密钥解析之后、任何写操作之前;DryRun 不查集群)。

.DESCRIPTION
	签名方(guild / trade,infra-up 部署)与验签方(scene,zone-up 部署)分属两条命令,两边的开关与密钥必须对得上,否则每一次资产 RPC
	都回 27008(AssetAuthFailed),日志里没有「密钥不匹配」的线索(密钥值不进日志)。依据是双方 Pod 模板上的密钥指纹注解:
	  - zone-up(以及带 -SkipInfra 的 all-up):读 infra namespace 里 guild / trade 的 Deployment。调用方开着而本次 scene 不开,
	    或两边指纹不同 → 拒绝(Get-AssetOpConsistencyVerdict);
	  - infra-up 且本次打开、确实部署全局 Go 服务:读全部 namespace 里副本数 > 0 的 scene 工作负载,有任何一个没拿到同一把密钥 → 拒绝;
	  - 不带 -SkipInfra 的 all-up 不查:同一次运行里 infra 与每个 zone 用同一组开关与密钥重新部署。调用方先开,zone 部署完之前的资产 RPC
	    会被拒、留在 PENDING 由重投补上,不丢东西。
#>
function Assert-AssetOpClusterConsistency {
	$zoneSide = ($Command -eq 'zone-up') -or ($Command -eq 'all-up' -and $SkipInfra)
	$infraSide = ($Command -eq 'infra-up') -and $script:AssetOpsOn -and (Test-GlobalGoSvcDeployed)
	if (-not $zoneSide -and -not $infraSide) { return }
	if ($DryRun) {
		Write-Host "[dry-run] AssetOps cluster consistency check skipped (no cluster access)"
		return
	}

	$desired = Get-AssetOpDesiredHashes
	if ($zoneSide) {
		foreach ($caller in $script:AssetOpCallers.Keys) {
			$spec = $script:AssetOpCallers[$caller]
			$live = Get-LiveDeploymentPodAnnotation -Namespace $InfraNamespace -Name $caller -Key $spec.HashAnnotation
			$verdict = Get-AssetOpConsistencyVerdict -CallerHash $live -SceneHash $desired[$caller]
			if ($verdict -eq 'scene-off-while-caller-on') {
				throw "$caller 的资产通道还开着(namespace $InfraNamespace 的 Deployment $caller 带签名密钥指纹),本次却不给 scene 注入验签密钥(-AssetOps off):这个 zone 的每一次资产 RPC 都会被 scene 拒绝。先 infra-up -AssetOps off 关掉调用方,或本次照传 -AssetOps on -NetworkPolicies on 与同一组密钥。"
			}
			if ($verdict -eq 'key-mismatch') {
				throw "本次给 scene 的 $($spec.Env) 与线上 $caller 正在用的签名密钥不同(Pod 模板指纹对不上):scene 验签会全部失败。换密钥按「先 infra-up -AssetOps off、换值、再逐 zone 与 infra 打开」(deploy/k8s/README.md「gRPC 口准入与资产通道」)。"
			}
		}
	}
	if ($infraSide) {
		$missing = @()
		foreach ($workload in Get-LiveSceneWorkloads) {
			foreach ($caller in $script:AssetOpCallers.Keys) {
				$key = $script:AssetOpCallers[$caller].HashAnnotation
				$sceneHash = $null
				if ($null -ne $workload.Annotations -and $null -ne $workload.Annotations.PSObject.Properties[$key]) {
					$sceneHash = [string]$workload.Annotations.PSObject.Properties[$key].Value
				}
				if ((Get-AssetOpConsistencyVerdict -CallerHash $desired[$caller] -SceneHash $sceneHash) -ne 'ok') {
					$missing += "$($workload.Namespace)/$($workload.Kind)/$($workload.Name)($caller)"
				}
			}
		}
		if ($missing.Count -gt 0) {
			throw ("-AssetOps on:以下 scene 工作负载还没拿到同一把验签密钥,先对它们所在的 zone 跑 zone-up -AssetOps on -NetworkPolicies on(同一组密钥),再打开调用方:`n  - " + ($missing -join "`n  - "))
		}
	}
}

# -AssetOps / -NetworkPolicies 的组合校验(写路径入口,密钥解析之前):打一行本次取值;资产通道打开而 scene 的 gRPC 口不设防时拒绝。
function Assert-AssetOpsSwitchPreflight {
	Write-Host "AssetOps: $AssetOps NetworkPolicies: $NetworkPolicies"
	if ($script:AssetOpsOn -and -not $script:NetworkPoliciesOn) {
		throw "-AssetOps on 必须同时 -NetworkPolicies on:资产指令只靠签名防伪,scene 的 gRPC 口还要只放行资产调用方(docs/design/guild-phase2/90-consistency.md G-05)。zone-up 与 infra-up 都要两个一起传。"
	}
}

# scene 验签要的两把资产密钥 env(-AssetOps on)。-ItemIndent = 列表项「- name:」所在缩进,-Step = 一级缩进单位
# (Deployment 模板用制表符、Fleet 模板用空格,两边各传各的)。
function New-SceneAssetOpEnvLines {
	param(
		[Parameter(Mandatory = $true)][string]$ItemIndent,
		[Parameter(Mandatory = $true)][string]$Step
	)

	$lines = @()
	foreach ($caller in $script:AssetOpCallers.Keys) {
		$spec = $script:AssetOpCallers[$caller]
		$lines += "${ItemIndent}- name: $($spec.Env)"
		$lines += "${ItemIndent}  valueFrom:"
		$lines += "${ItemIndent}${Step}secretKeyRef:"
		$lines += "${ItemIndent}${Step}  name: $($spec.SecretName)"
		$lines += "${ItemIndent}${Step}  key: $($script:AssetOpSecretKey)"
	}
	return $lines
}

# 清单文本里声明的全部 containerPort(去重、升序;纯函数)。
function Get-ManifestContainerPorts {
	param([Parameter(Mandatory = $true)][string]$ManifestContent)

	$ports = @()
	foreach ($match in [regex]::Matches($ManifestContent, '(?m)^[ \t]*(?:-[ \t]+)?containerPort:[ \t]*(\d+)[ \t]*\r?$')) {
		$ports += [int]$match.Groups[1].Value
	}
	return @($ports | Sort-Object -Unique)
}

<#
.SYNOPSIS
	gRPC 口准入的 NetworkPolicy(纯函数,-NetworkPolicies on)。

.DESCRIPTION
	一旦有策略选中某个 Pod,它的入站就只剩「本节点来的连接(kubelet 探针)」与策略 ingress 里列出的,所以 -OpenTcpPorts 必须覆盖
	工作负载清单里声明的其它全部端口(调用方从清单里抽,契约测试逐个核对),gRPC 口单独一条只放行 -AllowedApps(任意 namespace)。
	刻意不用 endPort 端口区间:不支持 endPort 的网络插件会只认起始端口,其余端口全部被拦。
#>
function New-GrpcAllowListNetworkPolicyYaml {
	param(
		[Parameter(Mandatory = $true)][string]$PolicyName,
		[Parameter(Mandatory = $true)][string[]]$TargetApps,
		[Parameter(Mandatory = $true)][int]$GrpcPort,
		[Parameter(Mandatory = $true)][AllowEmptyCollection()][int[]]$OpenTcpPorts,
		[Parameter(Mandatory = $true)][string[]]$AllowedApps
	)

	if ($GrpcPort -lt 1 -or $GrpcPort -gt 65535) {
		throw "内部错误:NetworkPolicy $PolicyName 的 gRPC 端口 $GrpcPort 越界。"
	}
	$open = @($OpenTcpPorts | Sort-Object -Unique)
	if ($open -contains $GrpcPort) {
		throw "内部错误:NetworkPolicy $PolicyName 的 gRPC 端口 $GrpcPort 同时出现在对所有来源开放的端口里,名单形同虚设。"
	}
	foreach ($port in $open) {
		if ($port -lt 1 -or $port -gt 65535) { throw "内部错误:NetworkPolicy $PolicyName 的开放端口 $port 越界。" }
	}
	$lines = @(
		'apiVersion: networking.k8s.io/v1',
		'kind: NetworkPolicy',
		'metadata:',
		"  name: $PolicyName",
		'  labels:',
		'    mmorpg.io/managed-by: k8s_deploy.ps1',
		'spec:',
		'  podSelector:',
		'    matchExpressions:',
		'      - key: app',
		'        operator: In',
		"        values: [$($TargetApps -join ', ')]",
		'  policyTypes:',
		'    - Ingress',
		'  ingress:'
	)
	if ($open.Count -gt 0) {
		$lines += '    # 清单里声明的其它端口(节点口 / metrics / 日志 sidecar):任何来源照常可达'
		$lines += '    - ports:'
		foreach ($port in $open) {
			$lines += '        - protocol: TCP'
			$lines += "          port: $port"
		}
	}
	$lines += "    # gRPC 口 ${GrpcPort}:只放行名单里的工作负载(任意 namespace)"
	$lines += '    - from:'
	$lines += '        - namespaceSelector: {}'
	$lines += '          podSelector:'
	$lines += '            matchExpressions:'
	$lines += '              - key: app'
	$lines += '                operator: In'
	$lines += "                values: [$($AllowedApps -join ', ')]"
	$lines += '      ports:'
	$lines += '        - protocol: TCP'
	$lines += "          port: $GrpcPort"
	return ($lines -join "`n")
}

# scene 的 gRPC 口准入策略(zone namespace)。开放端口 = scene 的节点口 + 日志 sidecar 声明的端口(开 sidecar 时);
# gRPC 口 = 节点口 + 30000(node_allocator.cpp kGrpcPortOffset,与 New-NodeDeploymentYaml 同一推导)。
function New-SceneGrpcNetworkPolicyYaml {
	param(
		[Parameter(Mandatory = $true)][int]$SceneRpcPort,
		[Parameter(Mandatory = $true)][string]$CurrentZoneName
	)

	$spec = $script:GrpcAllowLists['scene']
	$open = @($SceneRpcPort)
	# Prometheus 出口(-CppMetricsPort):不放行的话开了 NetworkPolicy 就抓不到 scene 的指标。
	if ($CppMetricsPort -gt 0) {
		$open += $CppMetricsPort
	}
	if ($script:CppLogSidecarEnabled) {
		$open += Get-ManifestContainerPorts -ManifestContent (New-CppLogSidecarContainerYaml -ItemIndent '  ' -CurrentZoneName $CurrentZoneName -AsNativeSidecar)
	}
	return New-GrpcAllowListNetworkPolicyYaml -PolicyName $spec.PolicyName -TargetApps $spec.TargetApps -GrpcPort ($SceneRpcPort + 30000) `
		-OpenTcpPorts $open -AllowedApps $spec.AllowedApps
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

<#
.SYNOPSIS
	把某个顶层 YAML 块(键那一行 + 其下所有缩进行,含块内注释)逐字抽出来,查不到直接 throw。

.DESCRIPTION
	Get-AuthoritativeScalar 只能取标量。像 IdSegments 这种"块式序列 + 每项一个映射"的结构,
	release_common.ps1 的扁平表模型表达不了(`- Kind: item` 被记成 IdSegments[0] 的整串,
	后面几行会被折成 IdSegments.Enabled / IdSegments.MinStep …,最后一项覆盖前面的),
	逐键取值只会取出一份错的。
	所以这里退一步做整块字节级搬运:ConfigMap 与仓库那份**不可能**漂移,而不是抄一份常数
	再靠人肉对齐 —— 键名写错在 C++ 侧是静默读不到(缺键 = proto 默认值),没有报错兜底。

	截断规则:从顶格(零缩进)的 `<Key>:` 那行起,到下一行顶格非空内容为止;块尾空行丢弃。
	只支持顶层块 —— 项目 etc/*.yaml 用到的就这一种。

.OUTPUTS
	[string] 块文本,行间以 CRLF 连接(与本脚本 here-string 的行尾一致)。
#>
function Get-AuthoritativeYamlBlock {
	param(
		[Parameter(Mandatory = $true)][string]$RelativePath,
		[Parameter(Mandatory = $true)][string]$Key
	)

	$full = Join-Path $RepoRoot ($RelativePath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
	if (-not (Test-Path -LiteralPath $full)) {
		throw "生成 ConfigMap 失败:找不到权威配置 $RelativePath(fail-closed:不替你猜一份 ${Key})"
	}

	$lines = (Get-Content -LiteralPath $full -Raw) -split "`r?`n"
	$start = -1
	for ($i = 0; $i -lt $lines.Count; $i++) {
		if ($lines[$i] -match ('^' + [regex]::Escape($Key) + '\s*:')) { $start = $i; break }
	}
	if ($start -lt 0) {
		throw "生成 ConfigMap 失败:$RelativePath 里没有顶层块 ${Key}:(fail-closed:不替你猜一份 ${Key})"
	}

	$out = New-Object System.Collections.Generic.List[string]
	$out.Add($lines[$start].TrimEnd())
	for ($i = $start + 1; $i -lt $lines.Count; $i++) {
		$line = $lines[$i]
		if ([string]::IsNullOrWhiteSpace($line)) { $out.Add(''); continue }
		# 顶格 = 下一个顶层键(或顶层注释),块到此为止
		if ($line -notmatch '^\s') { break }
		$out.Add($line.TrimEnd())
	}
	while ($out.Count -gt 0 -and [string]::IsNullOrWhiteSpace($out[$out.Count - 1])) {
		$out.RemoveAt($out.Count - 1)
	}

	return ($out -join "`r`n")
}

# ─────────────────────────────────────────────────────────────────
# C++ gRPC 客户端 deadline 预算(docs/design/grpc-muduo-transport.md §16.3 / §16.4)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	把 Go 时长写法("300s"、"1m30s"、"500ms")换算成毫秒(向上取整)。解析不了或不为正返回 $null。

.DESCRIPTION
	go-zero 的 MethodTimeouts[].Timeout 是 time.Duration,yaml 里必须是字符串,由 Go 的 time.ParseDuration 解析
	(go-zero core/mapping/unmarshaler.go)。这里认的是其中一个子集,只认 ms / s / m / h 四种单位:
	  - 裸数字("3000"):go-zero 不接受,加载配置时直接报错、服务起不来(数字报 expect string,带引号的
	    数字报 missing unit);几乎一定是把毫秒写错了地方。
	  - ns / us 对服务端超时没有意义。
	  - 正负号、小数点一侧缺数字的写法(+5s、.5s、5.s):Go 认,这里不认。
	这几类都返回 $null,由调用方按「解析不了」报违例,不替配置猜一个值(fail-closed)。
	单位大小写敏感、数字只认 ASCII 的 0-9,与 Go 一致("5S"、全角数字都不认)。
	用 decimal 而不是 double 算:16.1s 必须恰好是 16100,多算 1ms 会在「超时 + 2000 = deadline」的等号边界上误拒。
#>
function ConvertTo-ZrpcDurationMs {
	param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)

	$value = $Text.Trim()
	if ($value -cnotmatch '^(?:[0-9]+(?:\.[0-9]+)?(?:ms|s|m|h))+$') { return $null }

	try {
		$totalMs = [decimal]0
		foreach ($part in [regex]::Matches($value, '([0-9]+(?:\.[0-9]+)?)(ms|s|m|h)')) {
			$number = [decimal]::Parse($part.Groups[1].Value, [System.Globalization.NumberStyles]::AllowDecimalPoint, [System.Globalization.CultureInfo]::InvariantCulture)
			switch -CaseSensitive ($part.Groups[2].Value) {
				'ms' { $totalMs += $number }
				's'  { $totalMs += $number * [decimal]1000 }
				'm'  { $totalMs += $number * [decimal]60000 }
				'h'  { $totalMs += $number * [decimal]3600000 }
			}
		}
		if ($totalMs -le 0) { return $null }
		return [long][math]::Ceiling($totalMs)
	}
	catch {
		# 大到装不下的数:同样算解析不了,由调用方点名是哪个服务哪一条。
		return $null
	}
}

<#
.SYNOPSIS
	从 zrpc 服务 yaml 的文本里取出顶层 MethodTimeouts 的逐方法条目,每项是 @{ FullMethod = ...; Timeout = ... }。
	没有这个键(或写成 `MethodTimeouts: []`)返回空。

.DESCRIPTION
	为什么不用 ConvertFrom-YamlToFlatMap:它只展开「标量的列表」,遇到「映射的列表」会把每一项的第二个键压到
	同一条路径上 —— 三条 MethodTimeouts 的 Timeout 互相覆盖只剩最后一条,方法名和超时对不上号。

	只认一种写法:键顶格、不带引号,键后换行,每项缩进并以 `- ` 开头,项内只有 FullMethod 与 Timeout 两个键。
	其余一律 throw,宁可拒绝部署,也不拿一份解析错的表去核预算:
	  - 文件里别处提到了 MethodTimeouts 却不是上面这种键行(键带引号、整份文档统一缩进、流式 / JSON 写法、
	    同一个键写了两次)。go-zero 走完整的 YAML 解析,这些写法在服务端照样生效,这里读不出来就必须拒绝,
	    不能当作没有这张表。嵌在别的键下面的在服务端并不生效,但这里不去分辨,一并拒绝。
	  - 文本里有单独的 CR 或 NEL / LS / PS(U+0085 / U+2028 / U+2029)。yaml.v2 把它们当换行,这里只按 LF / CRLF
	    切行:注释后面跟一个这样的字符,后续内容就会被当成注释的一部分整段漏读。
	  - 列表项顶格写(YAML 允许)。ConvertFrom-YamlToFlatMap 数不到这种写法的条目,还会把项内的 Timeout
	    认成顶层 Timeout,后面的核对全都对不上。
	  - 看不懂的行、未知键、重复键、缺键。
	键名不分大小写,与 go-zero 的配置加载一致:写成 `methodTimeouts:` 在服务端同样生效,这里照常读出来核对。
#>
function Get-ZrpcMethodTimeouts {
	param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)

	if ($Text -match '\r(?!\n)|[\u0085\u2028\u2029]') {
		throw '文本里有单独的 CR 或 NEL / LS / PS 换行符(yaml.v2 把它们当换行,这里按 LF / CRLF 切行会漏读),请先把行尾统一成 LF 或 CRLF'
	}

	$entries = New-Object System.Collections.Generic.List[object]
	$current = $null
	$seen = $false
	$inBlock = $false
	foreach ($rawLine in ($Text -split "`r?`n")) {
		$line = (Remove-YamlInlineComment -Value ($rawLine -replace "`t", '    ')).TrimEnd()
		if ([string]::IsNullOrWhiteSpace($line)) { continue }

		if ($inBlock) {
			if ($line -match '^-') {
				throw "MethodTimeouts 的列表项要缩进书写(顶格的 '- ' 这里读不了):$($line.Trim())"
			}
			# 顶格的其他内容 = 下一个顶层键,MethodTimeouts 块到此结束。
			if ($line -notmatch '^\s') { $inBlock = $false }
		}

		if (-not $inBlock) {
			if ($line -match '^MethodTimeouts\s*:(?<rest>.*)$') {
				if ($seen) { throw 'MethodTimeouts 在顶层出现了不止一次' }
				$seen = $true
				$rest = $Matches['rest'].Trim()
				if ($rest -eq '[]') { continue }
				if ($rest.Length -gt 0) { throw "MethodTimeouts 只认块式写法(键后换行,每项缩进并以 '- ' 开头;空表请写成 MethodTimeouts: []),看不懂:$($line.Trim())" }
				$inBlock = $true
			}
			elseif ($line -match 'MethodTimeouts') {
				throw "这一行提到了 MethodTimeouts,却不是「顶格、不带引号的键 + 换行后的缩进列表」这种写法,读不了:$($line.Trim())"
			}
			continue
		}

		$item = [regex]::Match($line, '^\s+(?<dash>-\s+)?(?<key>[A-Za-z_][A-Za-z0-9_]*)\s*:\s*(?<value>\S.*)$')
		if (-not $item.Success) { throw "MethodTimeouts 里有看不懂的行:$($line.Trim())" }
		if ($item.Groups['dash'].Success) {
			$current = [ordered]@{}
			$entries.Add($current)
		}
		elseif ($null -eq $current) {
			throw "MethodTimeouts 的条目必须以 '- ' 开头:$($line.Trim())"
		}

		$key = $item.Groups['key'].Value
		$canonicalKey = $null
		if ($key -eq 'FullMethod') { $canonicalKey = 'FullMethod' }
		elseif ($key -eq 'Timeout') { $canonicalKey = 'Timeout' }
		else { throw "MethodTimeouts 的条目里只应有 FullMethod 与 Timeout,出现了未知键 ${key}" }
		if ($current.Contains($canonicalKey)) { throw "MethodTimeouts 的同一条目里 $canonicalKey 写了两次" }
		$current[$canonicalKey] = ConvertFrom-YamlScalarLiteral -Value $item.Groups['value'].Value
	}

	foreach ($entry in $entries) {
		if (-not ($entry.Contains('FullMethod') -and $entry.Contains('Timeout'))) {
			throw ("MethodTimeouts 有条目缺 FullMethod 或 Timeout:{0}" -f (($entry.Keys | ForEach-Object { "$_=$($entry[$_])" }) -join ', '))
		}
	}
	return $entries.ToArray()
}

<#
.SYNOPSIS
	「C++ 节点从不调用」的方法表:目标节点类型 → gRPC 全方法名数组。只有列在这里的方法,它在服务 yaml 的
	MethodTimeouts 里才允许高于「C++ deadline − 2000」。

.DESCRIPTION
	C++ 的 deadline 按目标节点类型配一个值,对该目标的所有方法生效;而 go-zero 的 MethodTimeouts 可以把个别方法的
	服务端超时单独放宽。一个被放宽的方法只要 C++ 不调用,就不存在「C++ 比服务端先放弃」的问题 —— 回档的三个 RPC
	正是如此:它们由运维带 x-admin-token 发起,服务端在回档过程中要等帮会资产沉降并反向调用 guild 做检查与复查,
	超时是分钟到小时级。

	登记一项之前先确认 C++ 不会调到它。C++ 调 Go 服务有两种形态,契约测试两种都查
	(tools/scripts/tests/k8s_deploy_contract.tests.ps1):
	  - 直接调用:cpp/ 下(生成物 cpp/generated 除外)出现生成客户端的符号 `Send<服务><方法>` /
	    `Send<服务><方法>Erased`,或它的消息号常量 `<服务><方法>MessageId`。
	  - gate 按消息号转发:gate 直连模式查 gRpcMethodRegistry 转发客户端消息,手写代码里不出现任何符号。
	    所以消息号列在生成的 IsClientMessageId(cpp/generated/rpc/service_metadata/rpc_event_registry.cpp)里的方法
	    一律算「C++ 会调用」,不许登记。
	表里登记了、服务 yaml 里却没有对应 MethodTimeouts 条目的过期项同样判失败。
	这道守护在跑契约测试时生效:CI 在 tools/scripts、各 etc 配置、k8s 清单等有改动时跑,发版流程必跑;
	只改 C++ 的提交不会当场触发(触发边界见 grpc-muduo-transport.md §16.4)。部署脚本自身不扫 C++ 源码。

	全方法名大小写敏感、逐字匹配,与 go-zero 按 info.FullMethod 查表的方式一致。
#>
function Get-GrpcClientMethodsNotCalledFromCpp {
	return [ordered]@{
		DataServiceNodeService = @(
			'/data_service.DataService/RollbackPlayer'
			'/data_service.DataService/RollbackZone'
			'/data_service.DataService/RollbackAll'
		)
	}
}

<#
.SYNOPSIS
	纯函数:核对「C++ deadline ≥ 目标 Go 服务的服务端超时 + 2000」(上游比下游宽),返回违例文本数组,空 = 通过。

.DESCRIPTION
	为什么是部署门禁而不只是注释:两个数分属两份配置(bin/etc 的 GrpcClient 块与 go/<svc>/etc 的 Timeout),
	改一边忘另一边不会有任何报错。C++ deadline 不比服务端超时宽时,C++ 先按自己的 DeadlineExceeded 收尾,
	拿不到服务端带真实错误码的应答,而服务端可能仍在执行 —— 本可确定的结果变成「结果未知」(grpc-muduo-transport.md §16.2)。

	判定口径(每一项都是违例,不是警告):
	  - 目标必须在 GrpcClient.CallDeadlineMs 里显式写成正整数:缺席 / 0 在 C++ 侧落到内置默认 10000
	    (grpc_client_call.h kDefaultGrpcCallDeadlineMs),预算不能依赖一个只写在 C++ 头文件里的隐式值。
	  - 服务 yaml 不写 Timeout = go-zero 默认 2000(go.mod 钉的 v1.9.2 / v1.10.0 的 zrpc/config.go 里
	    RpcServerConf.Timeout 都是 `default=2000`);写了就必须是正整数 —— 0 = go-zero 不装超时拦截器
	    (zrpc/server.go 只在 Timeout > 0 时装),服务端没有上界,「下游先超时」无从成立。
	  - MethodTimeouts 逐条核对:它能把个别方法的服务端超时放宽到全局 Timeout 之上,所以每一条都要满足
	    「C++ deadline ≥ 该方法的超时 + 2000」。唯一的例外是 C++ 节点从不调用的方法,由调用方通过
	    MethodsNotCalledFromCpp 显式列出(来源与守护见 Get-GrpcClientMethodsNotCalledFromCpp)。
	    例外只免「与 deadline 比大小」这一步,不免格式:每一条的全方法名都必须是 /包.服务/方法 的形状,
	    超时都必须解析得了(见 ConvertTo-ZrpcDurationMs)。
	    以下情形同样报违例而不放行:服务 yaml 里有 MethodTimeouts 键,调用方却没给逐方法解析结果;
	    解析出的条数与拍平表里数到的条数不一致(包括只有一边有);有这个键、解析出 0 条,而值又不是显式的 `[]`。

	只接收已解析好的值,不读文件,契约测试直接喂构造值验证每条口径
	(tools/scripts/tests/k8s_deploy_contract.tests.ps1)。

.PARAMETER DeployScalars
	bin/etc/base_deploy_config.yaml 拍平后的 Scalars,键形如 GrpcClient.CallDeadlineMs.SceneManagerNodeService。

.PARAMETER Targets
	有序字典:目标节点类型(ENodeType 枚举名)→ 一张表,键如下。
	  Source                   服务 yaml 路径,只用于报错。
	  Scalars                  该 yaml 经 ConvertFrom-YamlToFlatMap 拍平后的 Scalars。
	  MethodTimeouts           可选。该 yaml 经 Get-ZrpcMethodTimeouts 取出的条目数组。
	  MethodsNotCalledFromCpp  可选。该目标下 C++ 从不调用的全方法名数组。
#>
function Get-GrpcClientDeadlineBudgetViolations {
	param(
		[Parameter(Mandatory = $true)][System.Collections.IDictionary]$DeployScalars,
		[Parameter(Mandatory = $true)][System.Collections.IDictionary]$Targets
	)

	# 余量与 go-zero 默认值是这条契约本身(grpc-muduo-transport.md §16.3),不是调参旋钮,所以不开参数。
	$marginMs = 2000
	$goZeroDefaultServerTimeoutMs = 2000

	$violations = New-Object System.Collections.Generic.List[string]
	foreach ($target in $Targets.Keys) {
		$source = $Targets[$target].Source
		$serviceScalars = $Targets[$target].Scalars

		$deadlineRaw = [string]$DeployScalars["GrpcClient.CallDeadlineMs.$target"]
		$deadline = [long]0
		if (-not [long]::TryParse($deadlineRaw, [ref]$deadline) -or $deadline -le 0) {
			$violations.Add("${target}:GrpcClient.CallDeadlineMs.$target='$deadlineRaw' 必须显式写成正整数(缺席 / 0 在 C++ 侧落到内置默认 10000,预算无从核对)")
			continue
		}

		# -like / -match 不分大小写:go-zero 的配置键同样不分大小写,methodTimeouts: 也会生效。
		$methodKeys = @($serviceScalars.Keys | Where-Object { $_ -like 'MethodTimeouts*' })
		$hasParsedEntries = $Targets[$target].Contains('MethodTimeouts')
		if ($methodKeys.Count -gt 0 -and -not $hasParsedEntries) {
			$violations.Add("${target}:$source 出现 MethodTimeouts($($methodKeys -join ', ')),但没有拿到逐方法的解析结果,个别方法的服务端超时可能高于 C++ deadline,无法核对")
			continue
		}
		$entries = @()
		if ($hasParsedEntries) { $entries = @($Targets[$target].MethodTimeouts) }
		$itemCount = @($serviceScalars.Keys | Where-Object { $_ -match '^MethodTimeouts\[\d+\]$' }).Count
		if ($entries.Count -ne $itemCount) {
			$violations.Add("${target}:$source 的 MethodTimeouts 解析出 $($entries.Count) 条,拍平表里数到 $itemCount 条,两者不一致,无法核对(只认顶格不带引号的键 + 缩进的块式列表)")
			continue
		}
		if ($methodKeys.Count -gt 0 -and $entries.Count -eq 0 -and ([string]$serviceScalars['MethodTimeouts']).Trim() -cne '[]') {
			$violations.Add("${target}:$source 有 MethodTimeouts 键却没有解析出任何条目,无法核对(空表请写成 MethodTimeouts: [];有条目请用顶格不带引号的键 + 缩进的块式列表)")
			continue
		}

		$notCalledFromCpp = @()
		if ($Targets[$target].Contains('MethodsNotCalledFromCpp')) { $notCalledFromCpp = @($Targets[$target].MethodsNotCalledFromCpp) }
		foreach ($entry in $entries) {
			$fullMethod = [string]$entry.FullMethod
			$timeoutText = [string]$entry.Timeout
			if ($fullMethod -cnotmatch '^/[A-Za-z_][A-Za-z0-9_.]*/[A-Za-z_][A-Za-z0-9_]*$') {
				$violations.Add("${target}:$source 的 MethodTimeouts 里 FullMethod='$fullMethod' 不是 /包.服务/方法 的形状,无法核对")
				continue
			}
			$methodTimeoutMs = ConvertTo-ZrpcDurationMs -Text $timeoutText
			if ($null -eq $methodTimeoutMs) {
				$violations.Add("${target}:$source 的 MethodTimeouts 里 $fullMethod 的 Timeout='$timeoutText' 解析不了(只认带单位的正时长,单位 ms / s / m / h,如 300s)")
				continue
			}
			# 格式过关之后才看豁免:C++ 不调用的方法不用和 deadline 比大小。
			if ($notCalledFromCpp -ccontains $fullMethod) { continue }

			$methodRequiredMs = $methodTimeoutMs + $marginMs
			if ($deadline -lt $methodRequiredMs) {
				$violations.Add("${target}:C++ deadline $deadline < $source 的 MethodTimeouts $fullMethod $timeoutText($methodTimeoutMs ms)+ $marginMs = $methodRequiredMs(调大 bin/etc/base_deploy_config.yaml 的 GrpcClient.CallDeadlineMs.$target 或调小这条超时;若 C++ 节点根本不调用这个方法,把它登记进 k8s_deploy.ps1 的 Get-GrpcClientMethodsNotCalledFromCpp)")
			}
		}

		$serverTimeout = [long]$goZeroDefaultServerTimeoutMs
		$serverTimeoutText = "未写 Timeout(go-zero 默认 $goZeroDefaultServerTimeoutMs)"
		if ($serviceScalars.Contains('Timeout')) {
			$timeoutRaw = [string]$serviceScalars['Timeout']
			if (-not [long]::TryParse($timeoutRaw, [ref]$serverTimeout) -or $serverTimeout -le 0) {
				$violations.Add("${target}:$source 的 Timeout='$timeoutRaw' 必须是正整数(0 = go-zero 不装超时拦截器,服务端没有上界,下游先超时无从成立)")
				continue
			}
			$serverTimeoutText = "Timeout $serverTimeout"
		}

		$requiredMs = $serverTimeout + $marginMs
		if ($deadline -lt $requiredMs) {
			$violations.Add("${target}:C++ deadline $deadline < $source $serverTimeoutText + $marginMs = $requiredMs(改 bin/etc/base_deploy_config.yaml 的 GrpcClient.CallDeadlineMs.$target,或同步调该服务的 Timeout)")
		}
	}
	return $violations.ToArray()
}

<#
.SYNOPSIS
	部署门禁:读 GrpcClient 块与各目标 Go 服务的 yaml,按 Get-GrpcClientDeadlineBudgetViolations 核对,不满足即 throw 并逐项点名。

.DESCRIPTION
	只在写路径(zone-up / infra-up / all-up)的入口调用、先于任何集群写操作:node ConfigMap 会把 GrpcClient 块
	原样搬进集群(New-NodeConfigMapYaml),预算不成立时宁可在这里拒绝,也不产出一份让 C++ 比服务端先放弃的配置。
	*-down / *-status 不经过这里,止血与排查路径不受影响。

	核对的目标(键 = C++ 发往的 ENodeType 枚举名)与各自服务端超时的真源:
	  SceneManagerNodeService    ← go/scene_manager/etc/scene_manager_service.yaml
	  DataServiceNodeService     ← go/data_service/etc/data_service.yaml(不写 Timeout,按 go-zero 默认;
	                                三个回档 RPC 另有 MethodTimeouts,C++ 不调用,见下)
	  ClientRpcRouterNodeService ← go/client_rpc_router/etc/client_rpc_router.yaml
	  MatchNodeService           ← go/match/etc/match_service.yaml(gate 直连模式)
	  LoginNodeService           ← go/login/etc/login.yaml(gate 直连模式)
	  PlayerInboxNodeService     ← go/player_inbox/etc/player_inbox.yaml(scene 调 FetchItems / AckItems;
	                                PlayerInboxAdmin 的方法同端口同 Timeout,C++ 不调用)
	Battle / Etcd 没有 Go 服务端超时,不在表里;chat / friend / guild / team / trade / mail / rank 不在 C++ 节点白名单里,只经路由服到达
	(mail 的 MailAdmin 是运维直连,同样没有 C++ 调用方)。
	比对的是服务 yaml:scene-manager / match / 路由服 / login 的 go-svc ConfigMap 都从它镜像 Timeout
	(New-GoSvcConfigMapYaml);data-service 的 ConfigMap 不写 Timeout(= go-zero 默认 2000)—— 生成物同样满足
	这条不等式,由契约测试另外钉住(防镜像被改回常数、ConfigMap 键改名)。

	MethodTimeouts 逐条核对,只有 Get-GrpcClientMethodsNotCalledFromCpp 列出的方法可以高于 C++ deadline;
	本次放行了哪些,会在通过时的那一行里列出来。
#>
function Assert-GrpcClientDeadlineBudget {
	$deployPath = 'bin/etc/base_deploy_config.yaml'
	$targetSources = [ordered]@{
		SceneManagerNodeService    = 'go/scene_manager/etc/scene_manager_service.yaml'
		DataServiceNodeService     = 'go/data_service/etc/data_service.yaml'
		ClientRpcRouterNodeService = 'go/client_rpc_router/etc/client_rpc_router.yaml'
		MatchNodeService           = 'go/match/etc/match_service.yaml'
		LoginNodeService           = 'go/login/etc/login.yaml'
		# 玩家收件箱(docs/design/player-inbox-service.md §1.4):player-inbox 的 ConfigMap 从服务 yaml 镜像 Timeout。
		PlayerInboxNodeService     = 'go/player_inbox/etc/player_inbox.yaml'
	}
	$notCalledFromCpp = Get-GrpcClientMethodsNotCalledFromCpp

	# 与 Get-AuthoritativeScalar 同一条纪律:文件缺席就 throw,不替你猜一个超时。
	$readText = {
		param([string]$RelativePath)
		$full = Join-Path $RepoRoot ($RelativePath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
		if (-not (Test-Path -LiteralPath $full)) {
			throw "gRPC deadline 预算核对失败:找不到 $RelativePath(fail-closed:不替你猜一个超时)"
		}
		return [string](Get-Content -LiteralPath $full -Raw)
	}

	$targets = [ordered]@{}
	$exempted = New-Object System.Collections.Generic.List[string]
	foreach ($target in $targetSources.Keys) {
		$source = $targetSources[$target]
		$text = & $readText $source
		try {
			$methodTimeouts = @(Get-ZrpcMethodTimeouts -Text $text)
		}
		catch {
			throw "gRPC deadline 预算核对失败:$source 的 MethodTimeouts 解析不了(fail-closed:不拿一份读错的表去核预算):$($_.Exception.Message)"
		}
		$targetNotCalled = @()
		if ($notCalledFromCpp.Contains($target)) { $targetNotCalled = @($notCalledFromCpp[$target]) }
		$targets[$target] = @{
			Source                  = $source
			Scalars                 = (ConvertFrom-YamlToFlatMap -Text $text).Scalars
			MethodTimeouts          = $methodTimeouts
			MethodsNotCalledFromCpp = $targetNotCalled
		}
		foreach ($entry in $methodTimeouts) {
			if ($targetNotCalled -ccontains [string]$entry.FullMethod) { $exempted.Add("$target $($entry.FullMethod)=$($entry.Timeout)") }
		}
	}
	$deployScalars = (ConvertFrom-YamlToFlatMap -Text (& $readText $deployPath)).Scalars
	$violations = @(Get-GrpcClientDeadlineBudgetViolations -DeployScalars $deployScalars -Targets $targets)
	if ($violations.Count -gt 0) {
		throw ("C++ gRPC 客户端 deadline 预算不成立,拒绝部署(上游比下游宽:C++ deadline ≥ Go zrpc Timeout + 2000," +
			"docs/design/grpc-muduo-transport.md §16.3):`n  - " + ($violations -join "`n  - "))
	}
	$exemptedNote = ''
	if ($exempted.Count -gt 0) { $exemptedNote = "; MethodTimeouts exempt (never called from C++): " + ($exempted -join ', ') }
	Write-Host ("GrpcClient deadline budget OK (C++ deadline >= Go zrpc Timeout + 2000): {0}{1}" -f ($targetSources.Keys -join ', '), $exemptedNote)
}

<#
.SYNOPSIS
	路由服预算链的纯判定:每个路由目标 Go 服务的 zrpc Timeout + 1000 ≤ 路由服 ForwardTimeoutMs
	(docs/design/client-request-ordering.md §5.2 不变式 I1)。返回违例文本数组,空 = 通过。

.DESCRIPTION
	为什么要这条:gate 对同一会话的路由类请求单飞,上一条「观察到完成」才放下一条。路由服在 ForwardTimeoutMs 时
	回超时信封,如果目标服务的 handler 那时还没超时,它会继续跑完并提交,晚于 gate 已经放行的下一条落地,顺序就乱了。
	目标 Timeout 比 ForwardTimeoutMs 至少短 1000,路由服回超时的时候目标 handler 的 ctx 已经取消,不会再发出新的写。

	-ForwardTimeoutMsRaw:路由服 yaml 的 ForwardTimeoutMs 原文。
	-Targets:有序字典,键 = 路由目标 ENodeType 枚举名,值 = @{ Source = 来源描述; Scalars = 服务 yaml 拍平表 }。
	未写 Timeout 按 go-zero 默认 2000 计;Timeout 0 / 非正整数一律违例(0 = 不装超时拦截器,服务端没有上界)。
	出现 MethodTimeouts 一律违例:本判定不逐方法核对,个别方法可能高于 ForwardTimeoutMs,宁可拒绝也不静默放行。
#>
function Get-RouterForwardBudgetViolations {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$ForwardTimeoutMsRaw,
		[Parameter(Mandatory = $true)][System.Collections.IDictionary]$Targets
	)

	# 余量与 go-zero 默认值是契约本身(client-request-ordering.md §5.2),不是调参旋钮,所以不开参数。
	$marginMs = 1000
	$goZeroDefaultServerTimeoutMs = 2000

	$violations = New-Object System.Collections.Generic.List[string]
	$forward = [long]0
	if (-not [long]::TryParse($ForwardTimeoutMsRaw, [ref]$forward) -or $forward -le 0) {
		$violations.Add("路由服 ForwardTimeoutMs='$ForwardTimeoutMsRaw' 必须是正整数毫秒(fail-closed:不替你猜一个超时)")
		return $violations.ToArray()
	}

	foreach ($target in $Targets.Keys) {
		$source = $Targets[$target].Source
		$serviceScalars = $Targets[$target].Scalars

		# -like 不分大小写:go-zero 的配置键同样不分大小写。
		$methodKeys = @($serviceScalars.Keys | Where-Object { $_ -like 'MethodTimeouts*' })
		if ($methodKeys.Count -gt 0) {
			$violations.Add("${target}:$source 出现 MethodTimeouts($($methodKeys -join ', ')),本门禁不逐方法核对,个别方法可能高于路由服 ForwardTimeoutMs $forward;先扩展 Get-RouterForwardBudgetViolations 再部署")
			continue
		}

		$serverTimeout = [long]$goZeroDefaultServerTimeoutMs
		$serverTimeoutText = "未写 Timeout(go-zero 默认 $goZeroDefaultServerTimeoutMs)"
		if ($serviceScalars.Contains('Timeout')) {
			$timeoutRaw = [string]$serviceScalars['Timeout']
			if (-not [long]::TryParse($timeoutRaw, [ref]$serverTimeout) -or $serverTimeout -le 0) {
				$violations.Add("${target}:$source 的 Timeout='$timeoutRaw' 必须是正整数(0 = go-zero 不装超时拦截器,服务端没有上界,路由服回超时之后 handler 仍可能提交)")
				continue
			}
			$serverTimeoutText = "Timeout $serverTimeout"
		}

		$requiredForward = $serverTimeout + $marginMs
		if ($forward -lt $requiredForward) {
			$violations.Add("${target}:路由服 ForwardTimeoutMs $forward < $source $serverTimeoutText + $marginMs = $requiredForward(调小该服务的 Timeout;调大 ForwardTimeoutMs 要连带核对路由服 Timeout 与 gate deadline,见 client-request-ordering.md §5.2)")
		}
	}
	return $violations.ToArray()
}

<#
.SYNOPSIS
	部署门禁:读路由服 yaml 与各路由目标 Go 服务的 yaml,按 Get-RouterForwardBudgetViolations 核对,不满足即 throw 并逐项点名。

.DESCRIPTION
	与 Assert-GrpcClientDeadlineBudget 同一位置调用(写路径入口、先于任何集群写操作)。K8s 上这些服务的 go-svc ConfigMap
	都从服务 yaml 镜像 Timeout / ForwardTimeoutMs,所以核对服务 yaml 就是核对集群里生效的值。

	路由目标 = 路由表里客户端协议、非 Battle 的目标节点类型(go/client_rpc_router/internal/svc TargetNodeTypes)。
	TeamNodeService 寄宿在 match 进程里,与 MatchNodeService 共用 match_service.yaml 的 Timeout,单列一行便于点名。
	新增一个路由目标服务时,要在下面的表里加一行;漏加只会少核对一个服务,不会误拒。
#>
function Assert-RouterForwardBudget {
	$routerYaml = 'go/client_rpc_router/etc/client_rpc_router.yaml'
	$targetSources = [ordered]@{
		LoginNodeService  = 'go/login/etc/login.yaml'
		MatchNodeService  = 'go/match/etc/match_service.yaml'
		TeamNodeService   = 'go/match/etc/match_service.yaml'
		ChatNodeService   = 'go/chat/etc/chat.yaml'
		FriendNodeService = 'go/friend/etc/friend.yaml'
		GuildNodeService  = 'go/guild/etc/guild.yaml'
		TradeNodeService  = 'go/trade/etc/trade.yaml'
		MailNodeService   = 'go/mail/etc/mail.yaml'
		RankNodeService   = 'go/rank/etc/rank.yaml'
	}

	# 与 Assert-GrpcClientDeadlineBudget 同一条纪律:文件缺席就 throw,不替你猜一个超时。
	$readText = {
		param([string]$RelativePath)
		$full = Join-Path $RepoRoot ($RelativePath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
		if (-not (Test-Path -LiteralPath $full)) {
			throw "路由服预算核对失败:找不到 $RelativePath(fail-closed:不替你猜一个超时)"
		}
		return [string](Get-Content -LiteralPath $full -Raw)
	}

	$routerScalars = (ConvertFrom-YamlToFlatMap -Text (& $readText $routerYaml)).Scalars
	$forwardRaw = ''
	if ($routerScalars.Contains('ForwardTimeoutMs')) { $forwardRaw = [string]$routerScalars['ForwardTimeoutMs'] }

	$targets = [ordered]@{}
	foreach ($target in $targetSources.Keys) {
		$source = $targetSources[$target]
		$targets[$target] = @{
			Source  = $source
			Scalars = (ConvertFrom-YamlToFlatMap -Text (& $readText $source)).Scalars
		}
	}
	$violations = @(Get-RouterForwardBudgetViolations -ForwardTimeoutMsRaw $forwardRaw -Targets $targets)
	if ($violations.Count -gt 0) {
		throw ("路由服预算链不成立,拒绝部署(目标 zrpc Timeout + 1000 ≤ 路由服 ForwardTimeoutMs," +
			"docs/design/client-request-ordering.md §5.2 I1):`n  - " + ($violations -join "`n  - "))
	}
	Write-Host ("Router forward budget OK (Go zrpc Timeout + 1000 <= router ForwardTimeoutMs {0}): {1}" -f $forwardRaw, ($targetSources.Keys -join ', '))
}

# ─────────────────────────────────────────────────────────────────
# MySQL 连接总账(G2b;docs/PROGRESS.md 2026-10-12「落点多库连接拓扑核算」)
# ─────────────────────────────────────────────────────────────────
# 所有 zone 的 go/db、data_service、Java gateway 与全局的 friend、trade、player-inbox、guild、mail 共用 infra namespace 里同一台 MySQL。
# 以前每个服务各写一句「要算进 max_connections」,没有任何地方把它们加起来。这里把这笔账写成可执行的函数:
# 输入全部来自权威文件,写路径打印一行,契约测试钉住「规划峰值 ≤ max_connections」。
# Get-MysqlConnectionPlan / Get-MysqlMaxConnectionsFromManifest / Get-ManifestDeploymentReplicas /
# Get-MysqlConnectionLedgerInputs / Get-MysqlConnectionLedger 这五个只能依赖 $RepoRoot 与 Get-AuthoritativeScalar:
# 契约测试按 AST 把它们抽出来单独调用(与上面 deadline 守卫同一做法),引用 $InfraManifestsDir 等脚本级变量
# 或名单外的函数,在测试里就是「变量为空 / 命令未找到」。

<#
.SYNOPSIS
	MySQL 连接总账的规划档:deploy/k8s/manifests/infra/mysql.yaml 的 max_connections 按它的峰值取值。

.DESCRIPTION
	改这里 = 重算 max_connections 与 MySQL Pod 内存(mysql.yaml 里两处注释同步改)。
	契约测试钉住「规划峰值 ≤ max_connections」。
	deploy/k8s/mysql-connection-alerts.yaml 的额外库数阈值与这里同源,契约测试核对。
	纯函数,契约测试按 AST 抽出直接调用。
#>
function Get-MysqlConnectionPlan {
	$zoneCount = 4
	return [ordered]@{
		ZoneCount         = $zoneCount
		# 最坏情形:每个 go/db 实例都打开了其余全部 zone 的库(冷缓存访客读、合服 pin 后的源区库),打开后常驻。
		# 合服留下、已不在 zones 配置里的源区库不在其中 —— 合服后按实际库数重算。
		ExtraZoneStores   = $zoneCount - 1
		# Phase 2 的非 zone 落点库(player_store_*)未开;打开 PinOnCreate 前必须重做这份规划(launch L-16)。
		NonZoneStores     = 0
		# D1 第 2 阶段会把 RouteStore 切到 db_first(player-route-mapping-durability-proposal.md §7 P2),提前计入它的独立池。
		RouteStoreDbFirst = $true
		# 滚动发布时每个 Deployment 多出的实例数:maxSurge 1 的新 Pod + 1 个仍在优雅退出、连接还没释放的旧 Pod。
		# Deployment 控制器算 surge 时不计正在退出的 Pod,2 副本时同一刻最多 replicas + 2 个 Pod 持有连接
		# (K8s Deployment 文档滚动更新一节,外部事实;go/db 的 preStop + 排空约 20s、grace 45s)。
		RolloutExtraPodsPerDeployment = 2
		# 迁移 Job、备份、探针、merge_zone / route_tool、人工会话(估计值,待实测)。
		OpsReserve        = 20
	}
}

<#
.SYNOPSIS
	读 deploy/k8s/manifests/infra/mysql.yaml 里 my.cnf 的 max_connections。文件缺席、或不是恰好出现一次即 throw。

.DESCRIPTION
	只认整行(允许缩进)就是 `max_connections = <数字>` 的行;注释行以 # 开头,不会命中。
	出现 0 次时 mysqld 用它自己的默认值(MySQL 文档写 151,外部事实,未核实),出现多次时以哪一行为准不该靠猜:
	两种都拒绝(fail-closed)。
#>
function Get-MysqlMaxConnectionsFromManifest {
	$manifestPath = 'deploy/k8s/manifests/infra/mysql.yaml'
	$full = Join-Path $RepoRoot ($manifestPath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
	if (-not (Test-Path -LiteralPath $full)) {
		throw "MySQL 连接总账:找不到 $manifestPath(fail-closed:不替你猜一个 max_connections)"
	}
	$found = [regex]::Matches([string](Get-Content -LiteralPath $full -Raw), '(?m)^\s*max_connections\s*=\s*(\d+)\s*$')
	if ($found.Count -ne 1) {
		throw "MySQL 连接总账:$manifestPath 里 max_connections 应恰好出现 1 次,实际 $($found.Count) 次(fail-closed)"
	}
	return [int]$found[0].Groups[1].Value
}

<#
.SYNOPSIS
	读一份 manifest 里 Deployment 的副本数。文件缺席、或两格缩进的 replicas: 不是恰好出现一次即 throw。

.DESCRIPTION
	两格缩进 = Deployment 顶层 spec 下的那一行。总账用到的八份 manifest(go-svc 的 db / data-service / friend / trade /
	player-inbox / guild / mail、java-svc 的 gateway)各只有一个 Deployment;以后某份里多出第二个带 replicas 的工作负载,这里会拒绝而不是随便取一个。
#>
function Get-ManifestDeploymentReplicas {
	param(
		[Parameter(Mandatory = $true)][string]$RelativePath,
		# 报错前缀:哪一份总账在读。缺省是 MySQL 那份;Redis 连接总账(Get-RedisConnectionLedgerInputs)传「Redis 连接总账」。
		[string]$LedgerName = 'MySQL 连接总账'
	)

	$full = Join-Path $RepoRoot ($RelativePath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
	if (-not (Test-Path -LiteralPath $full)) {
		throw "${LedgerName}:找不到 $RelativePath(fail-closed:不替你猜一个副本数)"
	}
	$found = [regex]::Matches([string](Get-Content -LiteralPath $full -Raw), '(?m)^  replicas:\s*(\d+)\s*$')
	if ($found.Count -ne 1) {
		throw "${LedgerName}:$RelativePath 里两格缩进的 replicas 行(Deployment 顶层 spec)应恰好出现 1 次,实际 $($found.Count) 次(fail-closed)"
	}
	return [int]$found[0].Groups[1].Value
}

<#
.SYNOPSIS
	MySQL 连接总账的输入:每个连 MySQL 的客户端的池上限与副本数,全部从权威文件读,查不到即 throw。

.DESCRIPTION
	池上限读各服务自己的配置(ConfigMap 生成器从同一批键镜像,所以这里读到的就是集群里生效的值,见 Get-MysqlPoolMirror);
	副本数读 manifest;max_connections 读 mysql.yaml。
	Java gateway 无论本次是否部署(没给 -JavaSvcRegistry 时跳过)都计入:总账按「可能同时存在」保守算。
	login 的 PasswordAuth(池 20)不计入:K8s 的 login ConfigMap 不写该段、默认不启用;一旦启用,每 zone 要再加 2 × 20。
	新增一个连这台 MySQL 的服务 = 这里加一组键,Get-MysqlConnectionLedger 的公式加一项。
#>
function Get-AccountDirectoryPoolMaxOpen {
	# 账号目录独立池上限 = data_service.yaml 的 AccountDirectory.MaxOpenConn,口径与 Go 侧
	# config.AccountDirectoryConfig.PoolMaxOpenConn 一致:0 = 缺省 8,合法 [1, 128],越界服务拒启。
	# data-service ConfigMap 整段镜像 AccountDirectory,集群里生效的值与本账读的是同一个键。
	$raw = Get-AuthoritativeScalar -RelativePath 'go/data_service/etc/data_service.yaml' -KeyPath 'AccountDirectory.MaxOpenConn'
	$value = 0
	if (-not [int]::TryParse([string]$raw,[ref]$value)) { throw "DS账号目录池上限 AccountDirectory.MaxOpenConn 不是整数:$raw" }
	if ($value -eq 0) { return 8 }
	if ($value -lt 1 -or $value -gt 128) { throw "DS账号目录池上限 AccountDirectory.MaxOpenConn=$value 越界(合法 1..128,0 = 缺省 8)。" }
	return $value
}

function Get-MysqlConnectionLedgerInputs {
	$dbYaml = 'go/db/etc/db.yaml'
	$dsYaml = 'go/data_service/etc/data_service.yaml'
	return [ordered]@{
		MaxConnections        = Get-MysqlMaxConnectionsFromManifest
		DbReplicas            = Get-ManifestDeploymentReplicas -RelativePath 'deploy/k8s/manifests/go-svc/db.yaml'
		DbHomeMaxOpen         = Get-AuthoritativeScalar -RelativePath $dbYaml -KeyPath 'ServerConfig.Database.MaxOpenConn'
		# 每个额外落点库各自一个池,按库所属家族分两个旋钮(go/db config.go PlacementConfig)。
		DbZoneStoreMaxOpen    = Get-AuthoritativeScalar -RelativePath $dbYaml -KeyPath 'Placement.ZoneStoreMaxOpenConn'
		DbNonZoneStoreMaxOpen = Get-AuthoritativeScalar -RelativePath $dbYaml -KeyPath 'Placement.NonZoneStoreMaxOpenConn'
		DsReplicas            = Get-ManifestDeploymentReplicas -RelativePath 'deploy/k8s/manifests/go-svc/data-service.yaml'
		# snapshot / transaction_log / id_segment 各开一个池,共用 SnapshotMySQL.MaxOpenConn
		# (go/data_service/internal/svc/servicecontext.go NewServiceContext)。Go 侧没有可读的配置项,所以这里是常数:
		# 那边增减一个共用这组参数的 store,这个数要跟着改。
		DsAuditStorePools     = 3
		DsAuditMaxOpen        = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'SnapshotMySQL.MaxOpenConn'
		DsPlayerNameMaxOpen   = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'PlayerName.MaxOpenConn'
		DsDirectoryMaxOpen    = Get-AccountDirectoryPoolMaxOpen
		# route store 的独立池只在 RouteStore.Mode=db_first 时装配;是否计入由 Get-MysqlConnectionLedger 的 -RouteStoreDbFirst 决定。
		DsRouteMaxOpen        = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'RouteStore.MaxOpenConn'
		GatewayReplicas       = Get-ManifestDeploymentReplicas -RelativePath 'deploy/k8s/manifests/java-svc/gateway.yaml'
		GatewayMaxPool        = Get-AuthoritativeScalar -RelativePath 'java/gateway_node/src/main/resources/application.yaml' -KeyPath 'spring.datasource.hikari.maximum-pool-size'
		FriendReplicas        = Get-ManifestDeploymentReplicas -RelativePath 'deploy/k8s/manifests/go-svc/friend.yaml'
		FriendMaxOpen         = Get-AuthoritativeScalar -RelativePath 'go/friend/etc/friend.yaml' -KeyPath 'MySQL.MaxOpenConn'
		TradeReplicas         = Get-ManifestDeploymentReplicas -RelativePath 'deploy/k8s/manifests/go-svc/trade.yaml'
		TradeMaxOpen          = Get-AuthoritativeScalar -RelativePath 'go/trade/etc/trade.yaml' -KeyPath 'MySQL.MaxOpenConn'
		# 玩家收件箱(docs/design/player-inbox-service.md §10):独占库 mmorpg_player_inbox 一个池(入箱、审计、Fetch / Ack 共用,
		# 会话级 RC + 锁等待 1s),池上限取服务 yaml 的 MySQL.MaxOpenConn,player-inbox ConfigMap 从同一个键镜像。
		PlayerInboxReplicas   = Get-ManifestDeploymentReplicas -RelativePath 'deploy/k8s/manifests/go-svc/player-inbox.yaml'
		PlayerInboxMaxOpen    = Get-AuthoritativeScalar -RelativePath 'go/player_inbox/etc/player_inbox.yaml' -KeyPath 'MySQL.MaxOpenConn'
		# 帮会(2026-10-16 起有 K8s 清单):独占库 mmorpg_guild 一个池,上限取服务 yaml 的 MySQL.MaxOpenConn(以前不设 = 不限,同批补上),
		# guild ConfigMap 从同一个键镜像。
		GuildReplicas         = Get-ManifestDeploymentReplicas -RelativePath 'deploy/k8s/manifests/go-svc/guild.yaml'
		GuildMaxOpen          = Get-AuthoritativeScalar -RelativePath 'go/guild/etc/guild.yaml' -KeyPath 'MySQL.MaxOpenConn'
		# 邮件(docs/design/mail-system.md §7):独占库 mmorpg_mail 一个池,上限取服务 yaml 的 MySQL.MaxOpenConn,mail ConfigMap 从同一个键镜像。
		# 加入后规划档峰值 1780 → 1860,超过旧的 max_connections 1800,同批把它调到 2100(manifests/infra/mysql.yaml 的说明)。
		MailReplicas          = Get-ManifestDeploymentReplicas -RelativePath 'deploy/k8s/manifests/go-svc/mail.yaml'
		MailMaxOpen           = Get-AuthoritativeScalar -RelativePath 'go/mail/etc/mail.yaml' -KeyPath 'MaxOpenConns'
		RankReplicas          = Get-ManifestDeploymentReplicas -RelativePath 'deploy/k8s/manifests/go-svc/rank.yaml'
		RankMaxOpen           = Get-AuthoritativeScalar -RelativePath 'go/rank/etc/rank.yaml' -KeyPath 'MaxOpenConns'
		ChatReplicas          = Get-ManifestDeploymentReplicas -RelativePath 'deploy/k8s/manifests/go-svc/chat.yaml'
		ChatMaxOpen           = Get-AuthoritativeScalar -RelativePath 'go/chat/etc/chat.yaml' -KeyPath 'MaxOpenConns'
	}
}

<#
.SYNOPSIS
	MySQL 连接总账:由输入与部署形态算出每实例 / 每 zone / 稳态 / 滚动余量 / 峰值 / 对 max_connections 的余量。

.DESCRIPTION
	记号:Z = zone 数;E_z = 每个 go/db 实例打开的额外 zone 家族库数;E_n = 非 zone 落点库数;
	F = route store 是否 db_first;R = 滚动期间每个 Deployment 多出的实例数。
	  db 每实例 = DbHomeMaxOpen + E_z × DbZoneStoreMaxOpen + E_n × DbNonZoneStoreMaxOpen
	  ds 每实例 = DsAuditStorePools × DsAuditMaxOpen + DsPlayerNameMaxOpen (+ DsRouteMaxOpen,F 为真时)
	  每 zone   = 各自副本数 × (db、ds、gateway 每实例)之和
	  全局      = friend、trade、player-inbox、guild 与 mail 的副本数 × 池上限
	  稳态      = Z × 每 zone + 全局
	  滚动余量  = R × (db + ds + gateway + friend + trade + player-inbox + guild + mail 每实例)
	  峰值      = 稳态 + 滚动余量 + 运维余量
	纯函数,不读文件;输入不是正整数、形态参数越界即 throw(fail-closed:不替你猜一个连接数)。
#>
function Get-MysqlConnectionLedger {
	param(
		[Parameter(Mandatory = $true)][System.Collections.IDictionary]$Inputs,
		[Parameter(Mandatory = $true)][int]$ZoneCount,
		[Parameter(Mandatory = $true)][int]$ExtraZoneStores,
		[Parameter(Mandatory = $true)][int]$NonZoneStores,
		[Parameter(Mandatory = $true)][bool]$RouteStoreDbFirst,
		# 规划档与写路径传 Get-MysqlConnectionPlan 的值(2);自检复现 2026-10-12 的名义值时传 0(那份核算不含滚动余量)。
		[Parameter(Mandatory = $true)][int]$RolloutExtraPodsPerDeployment,
		[Parameter(Mandatory = $true)][int]$OpsReserve
	)
	$required = @('MaxConnections', 'DbReplicas', 'DbHomeMaxOpen', 'DbZoneStoreMaxOpen', 'DbNonZoneStoreMaxOpen',
		'DsReplicas', 'DsAuditStorePools', 'DsAuditMaxOpen', 'DsPlayerNameMaxOpen', 'DsDirectoryMaxOpen', 'DsRouteMaxOpen',
		'GatewayReplicas', 'GatewayMaxPool', 'FriendReplicas', 'FriendMaxOpen', 'TradeReplicas', 'TradeMaxOpen',
		'PlayerInboxReplicas', 'PlayerInboxMaxOpen', 'GuildReplicas', 'GuildMaxOpen', 'MailReplicas', 'MailMaxOpen', 'RankReplicas', 'RankMaxOpen', 'ChatReplicas', 'ChatMaxOpen')
	$v = @{}
	foreach ($key in $required) {
		$n = 0
		if (-not $Inputs.Contains($key) -or -not [int]::TryParse([string]$Inputs[$key], [ref]$n) -or $n -le 0) {
			throw "MySQL 连接总账:输入 $key='$($Inputs[$key])' 必须是正整数(fail-closed:不替你猜一个连接数)"
		}
		$v[$key] = $n
	}
	if ($ZoneCount -le 0 -or $ExtraZoneStores -lt 0 -or $NonZoneStores -lt 0 -or $RolloutExtraPodsPerDeployment -lt 0 -or $OpsReserve -lt 0) {
		throw "MySQL 连接总账:ZoneCount=$ZoneCount 必须为正,ExtraZoneStores=$ExtraZoneStores / NonZoneStores=$NonZoneStores / RolloutExtraPodsPerDeployment=$RolloutExtraPodsPerDeployment / OpsReserve=$OpsReserve 不能为负"
	}
	$dbPerInstance = $v.DbHomeMaxOpen + $ExtraZoneStores * $v.DbZoneStoreMaxOpen + $NonZoneStores * $v.DbNonZoneStoreMaxOpen
	$dsPerInstance = $v.DsAuditStorePools * $v.DsAuditMaxOpen + $v.DsPlayerNameMaxOpen + $v.DsDirectoryMaxOpen
	if ($RouteStoreDbFirst) { $dsPerInstance += $v.DsRouteMaxOpen }
	$gatewayPerInstance = $v.GatewayMaxPool
	$perZone = $v.DbReplicas * $dbPerInstance + $v.DsReplicas * $dsPerInstance + $v.GatewayReplicas * $gatewayPerInstance
	# 变量名不用 $global:它是作用域修饰符的名字,读的人会以为在写全局作用域。
	$globalPools = $v.FriendReplicas * $v.FriendMaxOpen + $v.TradeReplicas * $v.TradeMaxOpen + $v.PlayerInboxReplicas * $v.PlayerInboxMaxOpen +
		$v.GuildReplicas * $v.GuildMaxOpen + $v.MailReplicas * $v.MailMaxOpen + $v.RankReplicas * $v.RankMaxOpen + $v.ChatReplicas * $v.ChatMaxOpen
	$steady = $ZoneCount * $perZone + $globalPools
	# 一个 zone 的三个 Deployment 与五个全局 Deployment(friend / trade / player-inbox / guild / mail)各多 RolloutExtraPodsPerDeployment 个实例
	# (maxSurge 1 的新 Pod + 仍在优雅退出的旧 Pod;K8s 默认 25% 在 2 副本时 maxSurge 也是 1)。
	# all-up 不带 -WaitReady 时各 zone 的滚动会重叠,不在这个余量里。
	$surge = $RolloutExtraPodsPerDeployment * ($dbPerInstance + $dsPerInstance + $gatewayPerInstance + $v.FriendMaxOpen + $v.TradeMaxOpen + $v.PlayerInboxMaxOpen +
		$v.GuildMaxOpen + $v.MailMaxOpen + $v.RankMaxOpen + $v.ChatMaxOpen)
	$peak = $steady + $surge + $OpsReserve
	return [ordered]@{
		DbPerInstance = $dbPerInstance; DsPerInstance = $dsPerInstance; GatewayPerInstance = $gatewayPerInstance
		PerZone = $perZone; Global = $globalPools; Steady = $steady; RolloutSurge = $surge; OpsReserve = $OpsReserve
		Peak = $peak; MaxConnections = $v.MaxConnections; Headroom = $v.MaxConnections - $peak
	}
}

<#
.SYNOPSIS
	写路径打印 MySQL 连接总账一行:本次部署一份、规划档一份;任一份峰值超过 max_connections 时告警,不阻断。

.DESCRIPTION
	只在写路径(zone-up / infra-up / all-up)的入口调用;*-down / *-status 不经过这里。
	本次部署那一份的 Z 按命令计(同一 zoneId 只算一次),E_z 取 max(0, Z−1),其余参数取规划档:
	  - zone-up:zones 配置里的全部 zone ∪ 本次 -ZoneId(-ZoneId 不在 zones 配置里也并入:这个 zone 确实要部署);
	  - all-up / infra-up:只取 zones 配置里的 zone。这两条命令不为 -ZoneId 部署 zone 级工作负载(db / data-service / gateway):
	    -ZoneId 在这里用于基础设施阶段 —— 全局 Go 服务与 battle 节点配置里的 ZoneId(etcd 注册 zone)、mysql-init-sql 预建库的输入,
	    本身不产生每 zone 的 MySQL 连接,所以不论缺省值 101 还是显式传入都不并入。以前并入过:zones 配置不含它时
	    (例如 zones.10zones.yaml 的 1..10)Z 凭空多 1,可能打出一条虚假的 OverLimit。
	zones 配置直接用 Get-ZonesFromJson 读(Get-InfraZoneIds 内部用的同一个),不调 Get-InfraZoneIds:它为 mysql-init-sql
	预建库服务,在那里并入 -ZoneId 是对的,在这里会多算连接;它在 zones 配置缺席时那条 WARNING 讲的是
	「mysql-init-sql 只预建 -ZoneId 的库」,出现在 zone-up 的输出里也是误导。
	zones 配置文件缺席时 Z 取 1(zone-up 即 -ZoneId 那一个;infra-up 不部署 zone,取总账允许的下限 1),并在行尾标
	zones_config=missing。all-up 走不到这一支:写路径入口里排在前面的 Assert-ClientEntryDeployPreflight 读 zones 配置时已经 throw。
	超限只打 WARN 不 throw(是否升级为部署门禁待拍板)。输入文件缺键、max_connections / replicas 不是恰好一处、
	zones 配置在却解析不了,照常 throw —— 与 ConfigMap 生成器同一纪律(zone-up 以前不读 zones 配置,现在为了数 zone 会读)。
#>
function Write-MysqlConnectionLedger {
	$ledgerPlan = Get-MysqlConnectionPlan
	$ledgerInputs = Get-MysqlConnectionLedgerInputs

	$deployZoneCount = 1
	$zonesConfigTail = ' zones_config=missing'
	$zonesPath = Resolve-ZonesConfigPath
	# 与 Get-ZonesFromJson / Get-InfraZoneIds 用同一个判据(Test-Path 不带 -LiteralPath):几处对「文件在不在」不会各说各话。
	if (Test-Path $zonesPath) {
		$deployZoneIds = [System.Collections.Generic.HashSet[int]]::new()
		foreach ($zone in (Get-ZonesFromJson -Path $zonesPath)) {
			$null = $deployZoneIds.Add([int]$zone.zoneId)
		}
		# 只有 zone-up 为 -ZoneId 部署 zone 级工作负载;all-up / infra-up 里的 -ZoneId 不带来每 zone 的连接(缺省或显式都一样),
		# 并入会凭空多算一个 zone。
		if ($Command -eq "zone-up") {
			$null = $deployZoneIds.Add([int]$ZoneId)
		}
		$deployZoneCount = $deployZoneIds.Count
		$zonesConfigTail = ''
	}
	$deployExtraZoneStores = [Math]::Max(0, $deployZoneCount - 1)

	$deployLedger = Get-MysqlConnectionLedger -Inputs $ledgerInputs -ZoneCount $deployZoneCount `
		-ExtraZoneStores $deployExtraZoneStores -NonZoneStores $ledgerPlan.NonZoneStores `
		-RouteStoreDbFirst $ledgerPlan.RouteStoreDbFirst `
		-RolloutExtraPodsPerDeployment $ledgerPlan.RolloutExtraPodsPerDeployment -OpsReserve $ledgerPlan.OpsReserve
	$planLedger = Get-MysqlConnectionLedger -Inputs $ledgerInputs -ZoneCount $ledgerPlan.ZoneCount `
		-ExtraZoneStores $ledgerPlan.ExtraZoneStores -NonZoneStores $ledgerPlan.NonZoneStores `
		-RouteStoreDbFirst $ledgerPlan.RouteStoreDbFirst `
		-RolloutExtraPodsPerDeployment $ledgerPlan.RolloutExtraPodsPerDeployment -OpsReserve $ledgerPlan.OpsReserve

	$ledgerLineFormat = "MySQL connection ledger: zones={0} steady={1} peak={2} max_connections={3} headroom={4}; plan zones={5} peak={6} headroom={7}{8}"
	Write-Host ($ledgerLineFormat -f $deployZoneCount, $deployLedger.Steady, $deployLedger.Peak, $deployLedger.MaxConnections, $deployLedger.Headroom,
		$ledgerPlan.ZoneCount, $planLedger.Peak, $planLedger.Headroom, $zonesConfigTail)

	$overLimit = New-Object System.Collections.Generic.List[string]
	if ($deployLedger.Peak -gt $deployLedger.MaxConnections) {
		$overLimit.Add(("this deploy zones={0} (each go/db instance counted as opening the databases of the other {1} zones) peak {2}" -f $deployZoneCount, $deployExtraZoneStores, $deployLedger.Peak))
	}
	if ($planLedger.Peak -gt $planLedger.MaxConnections) {
		$overLimit.Add(("plan zones={0} (Get-MysqlConnectionPlan) peak {1}" -f $ledgerPlan.ZoneCount, $planLedger.Peak))
	}
	if ($overLimit.Count -gt 0) {
		# 开头「[MySQLConnectionLedger]OverLimit:」那一段刻意不留空格:ConsoleHost 可能按窗口宽度在空格处折行,契约测试只断这一段。
		# 控制台输出用英文,与本脚本其余 Write-Host / Write-Warning 一致;改措辞要同步改契约测试与 deploy/k8s/README.md「MySQL 连接总账」一节。
		# 格式串先拼好再 -f:-f 的优先级高于 +,写成一个表达式的话只有最后一段会被格式化。
		$overLimitFormat = ("[MySQLConnectionLedger]OverLimit:{0}; limit max_connections={1} (deploy/k8s/manifests/infra/mysql.yaml). Deployment continues: warning only, not blocking. Two ways out, pick one: " +
			"(1) finish TiDB Phase 1 first (docs/design/tidb-phase1-and-launch-readiness.md); " +
			"(2) redo Get-MysqlConnectionPlan and update both max_connections and the memory limits in mysql.yaml (raising the limit without raising memory trades error 1040 for OOMKill).")
		Write-Warning ($overLimitFormat -f ($overLimit -join '; '), $deployLedger.MaxConnections)
	}
}

# ConfigMap 里镜像的 MySQL 连接池参数:让上面总账的输入等于集群里真正生效的值。

<#
.SYNOPSIS
	ConfigMap 要镜像的 MySQL 连接池参数:按目标从服务自己的配置读出,核过才返回,不合格即 throw。

.DESCRIPTION
	以前这几组池参数不进 ConfigMap:go/db 的额外落点库池、data-service 的名字注册表池与 route store 池靠
	「Go 默认值恰好等于服务 yaml」,gateway 的 Hikari 靠 jar 内那份。总账读的是服务配置,两边一漂移,账就不是集群的账。

	核对规则:上限是正整数;空闲数是整数,不低于下限、不超过上限;gateway 的 Hikari connection-timeout 不小于 250(毫秒)。Go 服务的下限是 1 —— 0 在 Go 侧是「没配,取默认」
	(go/db、go/data_service 的 Normalize),镜像一个 0 进 ConfigMap,集群里生效的就不是写出来的值。
	为什么在部署侧就拒:镜像之后,服务 yaml 里一个非法的池值会原样进集群。data-service 不分 Mode 校验 RouteStore
	(go/data_service/data_service.go),K8s 上固定 Mode: redis 也会拒启;PlayerName 非法则名字 store 不装配,
	Store.Required: true 下同样拒启。宁可在 apply 之前失败,也不要 CrashLoopBackOff。

	两处调用、同一份清单:写路径入口的 Assert-MysqlPoolMirror(先于任何集群写操作把三个目标都读一遍),以及
	ConfigMap 生成器取值(New-GoSvcConfigMapYaml 的 db / data-service、New-JavaSvcConfigMapYaml 的 gateway)。
	新增一个要镜像的池 = 在这里加一行,再在模板里写出来;Get-MysqlConnectionLedgerInputs 读的是同一批键。

.OUTPUTS
	[ordered] 键 = 服务配置里的键路径,值 = [int]。
#>
function Get-MysqlPoolMirror {
	param([Parameter(Mandatory = $true)][ValidateSet('db', 'data-service', 'gateway')][string]$Target)

	$source = ''
	$idleFloor = 1
	$pools = @()
	$positiveScalars = @()
	# 键路径 → 下限(含)。用于「0 不等于没写、而是另有含义」的标量,不能套 $positiveScalars 那句 Go 侧的理由。
	$flooredScalars = [ordered]@{}
	switch ($Target) {
		'db' {
			$source = 'go/db/etc/db.yaml'
			$pools = @(
				@{ MaxOpen = 'Placement.ZoneStoreMaxOpenConn'; MaxIdle = 'Placement.ZoneStoreMaxIdleConn' }
				@{ MaxOpen = 'Placement.NonZoneStoreMaxOpenConn'; MaxIdle = 'Placement.NonZoneStoreMaxIdleConn' }
			)
			# 额外落点库池的空闲连接回收秒数:Go 侧 ≤0 视为没写、取默认 60,同样不镜像一个「等于没写」的值。
			$positiveScalars = @('Placement.ExtraStoreConnMaxIdleSeconds')
		}
		'data-service' {
			$source = 'go/data_service/etc/data_service.yaml'
			$pools = @(
				@{ MaxOpen = 'PlayerName.MaxOpenConn'; MaxIdle = 'PlayerName.MaxIdleConn' }
				@{ MaxOpen = 'RouteStore.MaxOpenConn'; MaxIdle = 'RouteStore.MaxIdleConn' }
			)
		}
		'gateway' {
			$source = 'java/gateway_node/src/main/resources/application.yaml'
			# Hikari 的 minimum-idle 写 0 是一个显式的合法值(不保留空闲连接;Hikari 行为凭记忆,未核实),所以下限是 0。
			$idleFloor = 0
			$pools = @(
				@{ MaxOpen = 'spring.datasource.hikari.maximum-pool-size'; MaxIdle = 'spring.datasource.hikari.minimum-idle' }
			)
			# 取连接的等待上限(毫秒,2026-10-15,G2b §10 #5)。HikariCP 5.1.0 的 setConnectionTimeout:0 = 无限等,
			# 小于 250 抛 IllegalArgumentException(按 jar 字节码核对),所以下限 250。
			$flooredScalars['spring.datasource.hikari.connection-timeout'] = 250
		}
	}

	$values = [ordered]@{}
	foreach ($pool in $pools) {
		$maxOpenRaw = Get-AuthoritativeScalar -RelativePath $source -KeyPath $pool.MaxOpen
		$maxIdleRaw = Get-AuthoritativeScalar -RelativePath $source -KeyPath $pool.MaxIdle
		$maxOpen = 0
		$maxIdle = 0
		if (-not [int]::TryParse([string]$maxOpenRaw, [ref]$maxOpen) -or $maxOpen -le 0) {
			throw "MySQL 连接池镜像:$source 的 $($pool.MaxOpen)='$maxOpenRaw' 必须是正整数(fail-closed:不把一个非法的池上限写进 ConfigMap)"
		}
		if (-not [int]::TryParse([string]$maxIdleRaw, [ref]$maxIdle) -or $maxIdle -lt $idleFloor -or $maxIdle -gt $maxOpen) {
			throw "MySQL 连接池镜像:$source 的 $($pool.MaxIdle)='$maxIdleRaw' 必须是不小于 ${idleFloor}、不超过 $($pool.MaxOpen)=$maxOpen 的整数(fail-closed:服务端会拒启或静默改写它)"
		}
		$values[$pool.MaxOpen] = $maxOpen
		$values[$pool.MaxIdle] = $maxIdle
	}
	foreach ($keyPath in $positiveScalars) {
		$raw = Get-AuthoritativeScalar -RelativePath $source -KeyPath $keyPath
		$parsed = 0
		if (-not [int]::TryParse([string]$raw, [ref]$parsed) -or $parsed -le 0) {
			throw "MySQL 连接池镜像:$source 的 $keyPath='$raw' 必须是正整数(fail-closed:Go 侧把 ≤0 当成没写)"
		}
		$values[$keyPath] = $parsed
	}
	foreach ($keyPath in $flooredScalars.Keys) {
		$floor = $flooredScalars[$keyPath]
		$raw = Get-AuthoritativeScalar -RelativePath $source -KeyPath $keyPath
		$parsed = 0
		if (-not [int]::TryParse([string]$raw, [ref]$parsed) -or $parsed -lt $floor) {
			throw "MySQL 连接池镜像:$source 的 $keyPath='$raw' 必须是不小于 $floor 的整数(fail-closed:服务端会拒启,或把 0 当成无限等)"
		}
		$values[$keyPath] = $parsed
	}
	return $values
}

<#
.SYNOPSIS
	部署门禁:ConfigMap 要镜像的 MySQL 连接池参数先全部读一遍,任何一项不合格即 throw(清单与规则见 Get-MysqlPoolMirror)。

.DESCRIPTION
	只在写路径(zone-up / infra-up / all-up)的入口调用、先于任何集群写操作;*-down / *-status 不经过这里。
	生成器取值时会再核一次,但那已经在 Apply-Zone 中途:node-config / gate / scene 都 apply 完了,才在生成 go-svc ConfigMap
	时失败,留下半截部署。
	三个目标都核,不看本次带没带 -SkipGoSvc / -SkipJavaSvc、是不是 infra-up:与 Get-MysqlConnectionLedgerInputs 同一口径 ——
	总账本来就要读这些文件,仓库里的服务配置不合格,就不该从这棵工作树发布。
#>
function Assert-MysqlPoolMirror {
	foreach ($mirrorTarget in @('db', 'data-service', 'gateway')) {
		[void](Get-MysqlPoolMirror -Target $mirrorTarget)
	}
}

# ─────────────────────────────────────────────────────────────────
# go/db 副本数门禁(S1 ④ (c):docs/design/db-retry-owner-routing.md §8)
# ─────────────────────────────────────────────────────────────────
# Test-DbReplicaPartitionBudget 是纯函数;Assert-DbReplicasWithinPartitionContract 只依赖 $RepoRoot、Get-AuthoritativeScalar、
# Get-ManifestDeploymentReplicas 与前者:契约测试按 AST 把它们抽出来单独调用(与上面 MySQL 总账同一做法)。

<#
.SYNOPSIS
	go/db 副本数门禁的判定:副本数(或 HPA 的 maxReplicas)超过 db_task topic 的分区数即 throw。纯函数。

.DESCRIPTION
	PartitionCnt ≤ 0 同样 throw:分区数不明就判不了副本数是否越界(fail-closed)。等于 PartitionCnt 放行。
	-ReplicasSource 只用于报错文本,说明副本数出自哪一行:缺省是 Deployment 的 replicas,HPA 传「HorizontalPodAutoscaler 的 maxReplicas」。
	为什么是这个上界、为什么只在部署脚本里拦,见 Assert-DbReplicasWithinPartitionContract。
#>
function Test-DbReplicaPartitionBudget {
	param(
		[Parameter(Mandatory = $true)][int]$Replicas,
		[Parameter(Mandatory = $true)][int]$PartitionCnt,
		[string]$ReplicasSource = 'replicas'
	)
	if ($PartitionCnt -le 0) {
		throw "go/db 副本数门禁:go/db/etc/db.yaml 的 ServerConfig.Kafka.PartitionCnt=$PartitionCnt 必须是正整数(fail-closed:分区数不明就判不了副本数是否越界)。"
	}
	if ($Replicas -gt $PartitionCnt) {
		throw "go/db 副本数门禁:deploy/k8s/manifests/go-svc/db.yaml 的 $ReplicasSource=$Replicas 超过 go/db/etc/db.yaml 的 ServerConfig.Kafka.PartitionCnt=$PartitionCnt(replicas ≤ PartitionCnt)。多出的副本拿不到分区只做热备;滚动发布的 maxSurge 临时副本不计入。要么减副本,要么走离线换代加分区。"
	}
}

<#
.SYNOPSIS
	部署门禁:go/db 的副本数不得超过 go/db/etc/db.yaml 的 ServerConfig.Kafka.PartitionCnt,超出即 throw。
	副本数取 deploy/k8s/manifests/go-svc/db.yaml 的 Deployment replicas;同一份 manifest 里若有 HorizontalPodAutoscaler,它的 maxReplicas 同样要 ≤ PartitionCnt。

.DESCRIPTION
	为什么是这个上界(docs/design/db-retry-owner-routing.md §8):db_task 的分区在消费组成员之间独占分配,有效并行上限就是分区数。
	超出分区数的副本拿不到任何分区的 claim:RetryRouting=owner 下它是热备 —— 只把共享收件箱按原始分区分拣进 p{N}、回收孤儿收据,
	不执行重试(db_owned_partitions=0,Setup 打 INFO 而不是 ERROR);属主崩溃后由它在 session 超时内接手。热备不是故障,
	但多开的副本只是白占连接(MySQL / Redis 连接总账都按 manifest 的副本数记)。
	为什么在这里拦、而 go/db 启动期不拒启:滚动发布的 maxSurge 本来就会短暂多出一个副本(本门禁不计入它);共享消费组下进程启动时
	无从判断分配;热备能在属主崩溃后接手。所以运行期允许多出的副本存在,只在部署脚本里静态比较仓库里的这两份文件。
	绕过:本门禁只看经本脚本 apply 的 manifest。kubectl scale deploy/db --replicas=N、kubectl edit 与带外 apply 的 HPA 都会绕过它 ——
	要扩副本就改 manifest 再走 zone-up,不要 kubectl scale。manifest 注释写明不要给 db 配 HPA(sarama eager 再平衡,每次成员变化全员暂停);
	本门禁不替那条约定把关,只保证同一份 manifest 里万一出现的 HPA 上界不越过分区数(maxReplicas 行缺失或不止一处即 throw)。
	只在写路径(zone-up / infra-up / all-up)的入口调用、先于任何集群写操作;与 Assert-MysqlPoolMirror 同一口径,不看本次是否部署 db:
	仓库里的这两份文件互相矛盾,就不该从这棵工作树发布。
#>
function Assert-DbReplicasWithinPartitionContract {
	$manifestPath = 'deploy/k8s/manifests/go-svc/db.yaml'
	$gateName = 'go/db 副本数门禁'
	$partitionText = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.PartitionCnt'
	$partitionCnt = 0
	if (-not [int]::TryParse([string]$partitionText, [ref]$partitionCnt)) {
		throw "${gateName}:go/db/etc/db.yaml 的 ServerConfig.Kafka.PartitionCnt='$partitionText' 不是整数(fail-closed:分区数不明就判不了副本数是否越界)。"
	}
	# 文件缺席、两格缩进的 replicas 行不是恰好一处,由 Get-ManifestDeploymentReplicas 以本门禁的名义 throw。
	Test-DbReplicaPartitionBudget -Replicas (Get-ManifestDeploymentReplicas -RelativePath $manifestPath -LedgerName $gateName) -PartitionCnt $partitionCnt

	# HPA:按 --- 拆成文档,只看 kind 为 HorizontalPodAutoscaler 的那几份(注释里提到 HPA 不算)。
	$full = Join-Path $RepoRoot ($manifestPath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
	$manifestText = [string](Get-Content -LiteralPath $full -Raw) -replace "`r`n", "`n"
	foreach ($doc in ($manifestText -split '(?m)^---[ \t]*$')) {
		if ($doc -notmatch '(?m)^kind:[ \t]*HorizontalPodAutoscaler[ \t]*$') { continue }
		$maxReplicas = [regex]::Matches($doc, '(?m)^  maxReplicas:[ \t]*(\d+)[ \t]*$')
		if ($maxReplicas.Count -ne 1) {
			throw "${gateName}:$manifestPath 里 HorizontalPodAutoscaler 两格缩进的 maxReplicas 行应恰好出现 1 次,实际 $($maxReplicas.Count) 次(fail-closed)"
		}
		Test-DbReplicaPartitionBudget -Replicas ([int]$maxReplicas[0].Groups[1].Value) -PartitionCnt $partitionCnt -ReplicasSource 'HorizontalPodAutoscaler 的 maxReplicas'
	}
}

# ─────────────────────────────────────────────────────────────────
# Redis 连接总账与重启闸(P0-E:docs/handoff/global-singletons-p0-findings-20261014.md「## P0-E」修复设计二、五)
# ─────────────────────────────────────────────────────────────────
# 所有 zone 的 Go 服务连接池、Java gateway、每个 C++ 进程与全局池服务共连 infra namespace 里同一个 Redis(-RedisReplicas 1:
# manifests/infra/redis.yaml 单实例;3:redis-ha.yaml 的 1 主 2 从,客户端经 2 个 haproxy 代理连到主,两份清单的 MAXCLIENTS 同值、
# 总账只读 redis.yaml 那一份)。连接数随 zone 数线性增长;go-redis 的池没有后台回收,连接数停在历史并发峰值。到线(maxclients)之后新连接被拒,
# Go 客户端多半只看到 EOF / failed to authenticate,唯一可靠的信号在服务端(INFO stats 的 rejected_connections;
# C++ 的断线 / 建连失败日志 2026-10-16 起带 hiredis 的 errstr)。
# 与上面 MySQL 连接总账同一纪律:输入从权威文件读,写路径打印一行,契约测试钉住「规划档峰值 ≤ 80% × maxclients」,超过只告警、不阻断。
# go-redis 直连客户端的池上限(P0-E 第 3 批)从服务 yaml 读,ConfigMap 镜像同一个键(Get-RedisPoolMirror,与 Get-MysqlPoolMirror 同形)。
# Get-RedisConnectionPlan / Get-RedisMaxClientsFromManifest / Get-RedisPoolMirror / Get-RedisConnectionLedgerInputs / Get-RedisConnectionLedger /
# Get-RedisPodTemplateDigest / Get-RedisCoreRestartVerdict 只依赖 $RepoRoot 与上一节的 Get-MysqlConnectionPlan、
# Get-ManifestDeploymentReplicas、Get-AuthoritativeScalar:契约测试按 AST 把它们抽出来单独调用(同上)。Assert-RedisCoreRestartConfirmed 读脚本参数
# ($DryRun / $InfraNamespace / $ConfirmRedisCoreRestart)与 Get-LiveRedisCoreTemplateDigest(读集群),契约测试给替身后调用;
# Get-LiveRedisCoreTemplateDigest 本身也按 AST 抽出,在测试里配假 kubectl / Build-KubectlBaseArgs 直测(失败即 throw、认出旧 Deployment);
# Write-RedisConnectionLedger、Get-RedisZoneCppProcesses、Test-RedisLiveMaxClients 读脚本参数或集群,用 DryRun 输出核对。

# 共享 Redis 的 StatefulSet 自己(不是 Pod 模板)上的 Pod 模板摘要注解。redis.yaml 里写占位,Apply-Infra 渲染时替换成
# Get-RedisPodTemplateDigest 的结果;下一次 infra-up 由 Assert-RedisCoreRestartConfirmed 读回比较。
$script:RedisPodTemplateDigestAnnotation = 'mmorpg.io/redis-pod-template-digest'
$script:RedisPodTemplateDigestPlaceholder = '__REDIS_POD_TEMPLATE_DIGEST__'

<#
.SYNOPSIS
	Redis 连接总账的规划档。规划 zone 数与滚动余量取自 Get-MysqlConnectionPlan(同一来源,不写第二个 4),其余是 Redis 自己的形态参数。

.DESCRIPTION
	契约测试钉住「规划档峰值 ≤ WarnRatio × maxclients」。纯函数,契约测试按 AST 抽出直接调用。
	GatePerZone / ScenePerZone / BattleProcesses 取本脚本 -GateReplicas / -SceneReplicas / -BattleReplicas 的缺省值(2 / 4 / 1)。
	交接里的目标形态(每 zone gate 5~22 台、scene 9~44 个,login 4 核)落地时重做本档,并同步复核 redis.yaml 的 MAXCLIENTS。
#>
function Get-RedisConnectionPlan {
	$mysqlPlan = Get-MysqlConnectionPlan
	return [ordered]@{
		ZoneCount                     = $mysqlPlan.ZoneCount
		RolloutExtraPodsPerDeployment = $mysqlPlan.RolloutExtraPodsPerDeployment
		GatePerZone                   = 2
		ScenePerZone                  = 4
		BattleProcesses               = 1
		# 规划档按 RouteStore.Mode=db_first 计(与 MySQL 规划档同一来源,不写第二个 $true):D1 的对账 leader 只在 db_first 下装配,
		# 它的独立 go-redis 池(Reconcile.RedisPoolSize)提前计入,切 db_first 时不用回来改账。本次部署那一份按 -RouteStoreMode 计。
		RouteStoreDbFirst             = $mysqlPlan.RouteStoreDbFirst
		# 探针(startup / readiness 每 5 秒一条 redis-cli 短连接)、运维 redis-cli(k8s_gate_drain.ps1 / k8s_zone_rollback.ps1)、
		# merge_zone / route_tool / data_consistency_check 等一次性工具、kubectl exec 会话。估计值,待实测。
		OpsReserve                    = 50
		# 告警水位:峰值超过 maxclients 的这个比例就告警。只告警、不阻断,与 MySQL 那份同一纪律(是否升级成门禁两份一起拍板)。
		WarnRatio                     = 0.8
	}
}

<#
.SYNOPSIS
	读 deploy/k8s/manifests/infra/redis.yaml 启动薄壳里的 MAXCLIENTS=<数字>。文件缺席、不是恰好出现一次、或不是正整数即 throw。

.DESCRIPTION
	只认整行(允许缩进)就是 `MAXCLIENTS=<数字>` 的行;注释行以 # 开头,不会命中。
	出现 0 次时 redis-server 用它自己的缺省值(10000,且会被容器 nofile 硬限下调;外部常识),出现多次时以哪一行为准不该靠猜:
	两种都拒绝(fail-closed)。「赋值右边不是字面量」由 tools/scripts/tests/redis_maxclients_contract.tests.ps1 拦。
	给了 -ManifestContent 就解析这份文本、不读盘:Apply-Infra 的部署后核对用它,保证与过闸、apply 的是同一次读取的内容。
	-RelativePath 缺省是核心 redis.yaml(原有调用全部不传,行为不变);按用途拆出的 queue 实例传 manifests/infra/redis-purpose.yaml(G4a 第 1a 批),
	同一套「恰好一次、正整数」规则,报错里点名的也是这个文件。
#>
function Get-RedisMaxClientsFromManifest {
	param(
		[string]$ManifestContent,
		[string]$RelativePath = 'deploy/k8s/manifests/infra/redis.yaml'
	)

	$manifestPath = $RelativePath
	if ($PSBoundParameters.ContainsKey('ManifestContent')) {
		$text = $ManifestContent
	}
	else {
		$full = Join-Path $RepoRoot ($manifestPath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
		if (-not (Test-Path -LiteralPath $full)) {
			throw "Redis 连接总账:找不到 $manifestPath(fail-closed:不替你猜一个 maxclients)"
		}
		$text = [string](Get-Content -LiteralPath $full -Raw)
	}
	$found = [regex]::Matches($text, '(?m)^[ \t]*MAXCLIENTS=(\d+)[ \t]*\r?$')
	if ($found.Count -ne 1) {
		throw "Redis 连接总账:$manifestPath 里 MAXCLIENTS=<数字> 应恰好出现 1 次,实际 $($found.Count) 次(fail-closed)"
	}
	$maxClients = 0
	if (-not [int]::TryParse($found[0].Groups[1].Value, [ref]$maxClients) -or $maxClients -le 0) {
		throw "Redis 连接总账:$manifestPath 的 MAXCLIENTS='$($found[0].Groups[1].Value)' 必须是正整数(fail-closed)"
	}
	return $maxClients
}

<#
.SYNOPSIS
	go-redis 直连客户端在服务配置里的连接池上限(P0-E 第 3 批):按目标从服务自己的 yaml 读出,不是正整数即 throw。

.DESCRIPTION
	两处调用、同一批键(与 Get-MysqlPoolMirror 同一纪律):Go 服务 ConfigMap 生成器(New-GoSvcConfigMapYaml)把它镜像进集群,
	Get-RedisConnectionLedgerInputs 按它记账 —— 总账读到的就是集群里生效的池上限,不再按 10 × GOMAXPROCS 估,也不随 CPU limit 漂。
	写路径入口的 Write-RedisConnectionLedger 先于任何集群写操作把全部目标读一遍,不合格就在 apply 之前 throw,不留半截部署。
	为什么要求正整数:Go 侧 0 / 不写 = go-redis 缺省 10 × GOMAXPROCS(负数拒启),镜像一个 0 进 ConfigMap,集群里生效的就不是写出来的值。
	go-zero 封装的客户端(data-service 的 MappingRedis、scene-manager / match / chat / friend)没有池参数,不在这里,总账按缺省池记。
	新增一个要镜像的池 = 这里加一行,再在 ConfigMap 模板里写出来;Get-RedisConnectionLedgerInputs 读的是同一批键。

.OUTPUTS
	[ordered] 键 = 服务配置里的键路径,值 = [int]。
#>
function Get-RedisPoolMirror {
	param([Parameter(Mandatory = $true)][ValidateSet('login', 'db', 'data-service', 'player-locator', 'trade', 'guild', 'friend', 'match', 'chat', 'player-inbox')][string]$Target)

	switch ($Target) {
		'login' { $source = 'go/login/etc/login.yaml'; $keyPaths = @('Node.RedisClient.PoolSize') }
		'db' { $source = 'go/db/etc/db.yaml'; $keyPaths = @('ServerConfig.RedisClient.PoolSize') }
		'data-service' { $source = 'go/data_service/etc/data_service.yaml'; $keyPaths = @('DevRedis.PoolSize') }
		'player-locator' { $source = 'go/player_locator/etc/player_locator.yaml'; $keyPaths = @('RedisClient.PoolSize') }
		'trade' { $source = 'go/trade/etc/trade.yaml'; $keyPaths = @('SharedRedis.PoolSize') }
		'player-inbox' { $source = 'go/player_inbox/etc/player_inbox.yaml'; $keyPaths = @('CoreRedis.PoolSize') }
		'guild' { $source = 'go/guild/etc/guild.yaml'; $keyPaths = @('RedisClient.PoolSize', 'PlayerLocatorRedis.PoolSize', 'MergeMarkerRedis.PoolSize', 'LoginRedis.PoolSize') }
		'friend' { $source = 'go/friend/etc/friend.yaml'; $keyPaths = @('LoginRedis.PoolSize') }
		'match' { $source = 'go/match/etc/match_service.yaml'; $keyPaths = @('LoginRedis.PoolSize') }
		'chat' { $source = 'go/chat/etc/chat.yaml'; $keyPaths = @('LoginRedis.PoolSize') }
	}

	$values = [ordered]@{}
	foreach ($keyPath in $keyPaths) {
		$raw = Get-AuthoritativeScalar -RelativePath $source -KeyPath $keyPath
		$poolSize = 0
		if (-not [int]::TryParse([string]$raw, [ref]$poolSize) -or $poolSize -le 0) {
			throw "Redis 连接池镜像:$source 的 $keyPath='$raw' 必须是正整数(fail-closed:0 在 Go 侧是 go-redis 缺省 10 × GOMAXPROCS,随 CPU limit 漂移,总账就不是集群的账)"
		}
		$values[$keyPath] = $poolSize
	}
	return $values
}

<#
.SYNOPSIS
	Redis 连接总账的输入:每副本上限常数表(每行带出处)、显式池上限(读服务 yaml)、各客户端的副本数(读 manifest)、maxclients(读 redis.yaml)。

.DESCRIPTION
	每副本上限 = ConfiguredPoolConns + DefaultPools × GoPoolSizePerProc × GoMaxProcs + PubSub + Fixed:
	  - ConfiguredPoolConns  显式设了 PoolSize 的 go-redis 直连池之和(P0-E 第 3 批)。行上的 PoolMirror 指明目标,值由 Get-RedisPoolMirror
	    从服务 yaml 读、ConfigMap 镜像同一个键,所以就是集群里生效的池上限,不随 CPU limit 漂;没有 PoolMirror 的行是 0;
	  - DefaultPools  仍是 go-redis 缺省大小的池个数:go-zero 封装(redis.RedisConf 没有池参数;go-zero v1.9.2 / v1.10.0 的
	    redisclientmanager.go 不设 PoolSize、固定 MinIdleConns 8,同一进程同一地址共用一个池),以后新接入、还没有 PoolSize 键的直连客户端也记在这里;
	  - GoPoolSizePerProc = 10:go-redis v9 的 PoolSize 缺省 10 × GOMAXPROCS(v9.16.0 / v9.17.3 / v9.18.0 的 options.go「opt.PoolSize == 0」分支,
	    2026-10-16 按本机模块缓存核对)。取连接由 PoolSize 大小的信号量封顶,池里的连接没有后台回收,所以按池上限记账,不按常驻下限;
	  - GoMaxProcs  该 Pod 的 GOMAXPROCS(G),只用于 DefaultPools(没有缺省池的行写 0)。Go 1.27.2 起容器感知:CPU limit 500m 与 "1" 预期都是 2
	    (按 limit 向上取整、下限 2,外部常识);go-zero 链进的 automaxprocs 若生效可能是 1。**未实测,待 S6**(在目标集群上只读取各 Go Pod 的
	    GOMAXPROCS 一次)。按 2 记,偏保守。契约测试钉住:有缺省池的行,manifest 的 CPU limit ≤ 2 核 —— 调大 limit 就变红,逼着重算这一列;
	  - PubSub      Pub/Sub 专用连接(go-redis 用 NewConn 另建,不占池名额,另算);
	  - Fixed       非 Go 客户端或代码里写死池上限的固定连接数(Java gateway、mail)。
	C++(gate / scene / battle)每进程 1 条(CppConnectionsPerProcess),台数由部署形态决定,不在这张表里(见 Get-RedisConnectionLedger 的参数)。
	不计入(写在这里,接上时加一行):
	  - client-rpc-router:不连 Redis;
	  - match 的 MatchRedis、chat 的 ChatRedis:K8s 上指向 redis-match-cluster,是另一套实例;
	  - rank:现役 SQL 权威榜不连接 Redis，不计共享 Redis 总账；旧 R2b 专用 Redis 不部署。
	  - login 的 PasswordAuth 失败配额池(auth_init.go passwordQuotaRedisOptions,PoolSize 4)与 SaToken 客户端:K8s 的 login ConfigMap
	    不写这两段;启用 PasswordAuth 后每个 login 副本 +4;
	  - 探针、运维 redis-cli 与一次性工具:进规划档的 OpsReserve。
	新增一个连这台 Redis 的服务 = 这里加一行。缺 manifest、replicas 不是恰好一处、MAXCLIENTS 不是恰好一处、PoolSize 不是正整数,照常 throw。
#>
# 缺省 owner epoch 句柄复用主池。显式拆分会新建独立池,本部署总账尚无独立实例的 MAXCLIENTS 预算,必须先拒绝。
# 此门禁同时用于连接总账入口与单独渲染 ConfigMap,避免绕过总账的调用静默丢配置或低估连接。
function Assert-DbOwnerEpochRedisDeployment {
	$source = Join-Path $RepoRoot 'go/db/etc/db.yaml'
	$ownerRedis = Get-YamlScalar -Path $source -KeyPath 'ServerConfig.OwnerEpochRedis'
	if ($ownerRedis.Found) {
		throw '生成 db ConfigMap 失败:ServerConfig.OwnerEpochRedis 显式配置会新建独立连接池,但尚未登记到 Redis 连接总账及对应实例的 MAXCLIENTS 预算。当前部署只支持省略该块并复用 RedisClient;请先补齐独立实例总账再启用,不能静默丢弃该配置。'
	}
}

function Get-RedisConnectionLedgerInputs {
	# -RouteStoreDbFirst:按 RouteStore.Mode=db_first 记账(D1 第 1b-3 批)。缺省 $false = redis 模式,与以前的调用方逐字相同。
	# -QueueSplit:排队与限流已拆到 redis-queue-g<N>(G4a 第 1a 批,-RedisSplit queue)。gateway 的 Redis 只用于 Bucket4j 限流,整行移到
	# queue 实例的总账(Get-RedisQueueConnectionLedgerInputs),核心这里去掉;login 的 RedisClient 池与 Pub/Sub 仍连核心,行不变。
	# 缺省 $false = 不拆,与以前的调用方逐字相同。
	param(
		[bool]$RouteStoreDbFirst = $false,
		[bool]$QueueSplit = $false
	)
	Assert-DbOwnerEpochRedisDeployment
	$ledgerName = 'Redis 连接总账'
	$goSvc = 'deploy/k8s/manifests/go-svc'
	# 缺省大小的池(DefaultPools)按 GOMAXPROCS = 2 计(见上;待 S6 实测)。
	$g = 2
	# D1 对账 leader 的独立 go-redis 池(第 1b-2 批 data_service 对账 leader 的 mapping Redis 客户端,与 Router 的 go-zero 池隔离;
	# PoolSize = RouteStore.Reconcile.RedisPoolSize,规格 spec_D1_batch1b_scout.md 1b-2 的 cache.go 一项):
	# 只在 RouteStore.Mode=db_first(且 Reconcile.Enabled≠false)时装配。ConfigMap 不写 RedisPoolSize(Get-RouteStoreDeploySettings),
	# 集群里生效的就是代码缺省 4,这里记同一个数;改 ConfigMap 写它时同改这里。全服同一时刻只有一个 leader 真正用它,但按每个
	# data-service 副本记:go-redis 的池没有后台回收,当过 leader 的副本掉主后连接仍可能留到进程退出(偏保守,同其余各行的口径)。
	$routeReconcileRedisPoolSize = 4
	$dsRouteLeaderPool = if ($RouteStoreDbFirst) { $routeReconcileRedisPoolSize } else { 0 }
	$clients = @(
		# —— 每 zone ——
		# login:go/login/internal/svc/servicecontext.go NewServiceContext 的 redis.NewClient(登录排队 loginqueue 复用它),
		# 池上限 Node.RedisClient.PoolSize;Pub/Sub:go/login/internal/dispatcher/task_result_dispatcher.go subscribeLoop 的一条 Subscribe(taskresult.LegacyNotifyChannel, 本实例频道)——同一连接订两条频道,所以仍记 1(B0-3,2026-10-18)。
		[ordered]@{ Name = 'login'; Scope = 'zone'; Manifest = "$goSvc/login.yaml"; PoolMirror = 'login'; DefaultPools = 0; GoMaxProcs = 0; PubSub = 1; Fixed = 0 }
		# db:go/db/internal/svc/servicecontext.go 的 RedisClient(ServerConfig.RedisClient.PoolSize);Placement.Redis 不写时落点句柄复用
		# 同一个 client(K8s 的 db ConfigMap 不写它)。
		[ordered]@{ Name = 'db'; Scope = 'zone'; Manifest = "$goSvc/db.yaml"; PoolMirror = 'db'; DefaultPools = 0; GoMaxProcs = 0; PubSub = 0; Fixed = 2 }  # Fixed 2:go/db/internal/kafka/durable_enqueue.go 的 WAITAOF 专用客户端(durableClientPoolSize = 2,组提交的粘连连接与启动探针都从它取,不占 PoolSize 主池;db.yaml 的 WaitAOFNumLocal / WaitAOFNumReplicas 都写 0 时不建)。按开启计,偏保守,口径同 trade 行。
		# data-service:go/data_service/internal/routing/router.go 的 DevRedis(go-redis,DevRedis.PoolSize)与 MappingRedis(go-zero,没有池参数,
		# 缺省池)两个池,K8s 的 ConfigMap 两段都指向共享 Redis;配了 DevRedis 时 Regions 的集群客户端不建;route_publish.go 的 WAITAOF 探针
		# (只在 RouteStore.Mode=db_first 时建)也是 go-zero 客户端,go-zero 按地址复用 MappingRedis 的池,不另开池。
		# Fixed:db_first 时加对账 leader 的独立 go-redis 池(见上面 $dsRouteLeaderPool;redis 模式不装配,0)。
		[ordered]@{ Name = 'data-service'; Scope = 'zone'; Manifest = "$goSvc/data-service.yaml"; PoolMirror = 'data-service'; DefaultPools = 1; GoMaxProcs = $g; PubSub = 0; Fixed = $dsRouteLeaderPool }
		# player-locator:go/player_locator/internal/svc/servicecontext.go 的 redis.NewClient(RedisClient.PoolSize)。
		[ordered]@{ Name = 'player-locator'; Scope = 'zone'; Manifest = "$goSvc/player-locator.yaml"; PoolMirror = 'player-locator'; DefaultPools = 0; GoMaxProcs = 0; PubSub = 0; Fixed = 0 }
		# scene-manager:go/scene_manager/internal/svc/servicecontext.go 的 redis.MustNewRedis(go-zero,缺省池)。
		# Fixed 32:go/scene_manager/internal/svc/owner_epoch_mint.go 的铸造专用客户端(ownerEpochMintPoolSize = 32,铸造 EVAL 与
		# WAITAOF 同一条连接、启动探针也从它取,不占 go-zero 缺省池)。按开启计;OwnerEpochMint 两项份数都写 0 时仍会建客户端、只是不发 WAITAOF。
		[ordered]@{ Name = 'scene-manager'; Scope = 'zone'; Manifest = "$goSvc/scene-manager.yaml"; DefaultPools = 1; GoMaxProcs = $g; PubSub = 0; Fixed = 32 }
		# Java gateway(Lettuce,不是池):StringRedisTemplate 的共享连接 1 条(ZoneHealthProbeService),限流打开时 RateLimitConfig 的
		# bucket4j 专用连接再 1 条。staging / prod 缺省开限流(-GatewayRateLimit auto),dev 关时实际 1 条;按 2 记,偏保守。
		[ordered]@{ Name = 'gateway'; Scope = 'zone'; Manifest = 'deploy/k8s/manifests/java-svc/gateway.yaml'; DefaultPools = 0; GoMaxProcs = 0; PubSub = 0; Fixed = 2 }
		# —— 全局池(infra namespace 一份)——
		# match:go/match/internal/svc/servicecontext.go NewRedisHandles 的 sharedRds(go-zero,缺省池);MatchRedis 是 redis-match-cluster,不计。
		[ordered]@{ Name = 'match'; Scope = 'global'; Manifest = "$goSvc/match.yaml"; PoolMirror = 'match'; DefaultPools = 1; GoMaxProcs = $g; PubSub = 0; Fixed = 0 }
		# Chat历史/幂等/限速均在SQL；Redis仅全局原登录态只读池，不再连接旧ChatRedis集群。
		[ordered]@{ Name = 'chat'; Scope = 'global'; Manifest = "$goSvc/chat.yaml"; PoolMirror = 'chat'; DefaultPools = 0; GoMaxProcs = 0; PubSub = 0; Fixed = 0 }
		# friend:go/friend/internal/svc/servicecontext.go NewRedisHandles(go-zero,缺省池);K8s 不配 FriendRedis,私有 key 回落共享库、两者是同一个句柄。
		[ordered]@{ Name = 'friend'; Scope = 'global'; Manifest = "$goSvc/friend.yaml"; PoolMirror = 'friend'; DefaultPools = 1; GoMaxProcs = $g; PubSub = 0; Fixed = 0 }
		# trade:go/trade/internal/svc/assetchannel.go newAssetChannel 的 redis.NewClient(SharedRedis.PoolSize),加 shared/assetopwake 订阅
		# assetop:wake 的 Pub/Sub 连接(AssetOp.Wake 缺省开)。开关从服务yaml镜像，缺省关闭；按打开计，开关翻转时不必回来改账。
		[ordered]@{ Name = 'trade'; Scope = 'global'; Manifest = "$goSvc/trade.yaml"; PoolMirror = 'trade'; DefaultPools = 0; GoMaxProcs = 0; PubSub = 1; Fixed = 0 }
		# guild 四个显式池（含只读全局 LoginRedis）；摘要 bump 专用池固定4，另有唤醒订阅。
		[ordered]@{ Name = 'guild'; Scope = 'global'; Manifest = "$goSvc/guild.yaml"; PoolMirror = 'guild'; DefaultPools = 0; GoMaxProcs = 0; PubSub = 1; Fixed = 4 }
		# player-inbox:go/player_inbox/internal/svc/servicecontext.go openCoreRedis 的 redis.NewClient(核心 Redis,CoreRedis.PoolSize,
		# 启动时 Ping);围栏上界读 owner_epoch、在线提醒读 location(scenenode.GoRedisReader)、后台循环选主锁
		# (leader.NewGoRedisStore)共用这一个客户端,没有 Pub/Sub。
		[ordered]@{ Name = 'player-inbox'; Scope = 'global'; Manifest = "$goSvc/player-inbox.yaml"; PoolMirror = 'player-inbox'; DefaultPools = 0; GoMaxProcs = 0; PubSub = 0; Fixed = 0 }
		# mail共享资产定位与全局登录态各有一个上限8的独立池；rank只访问SQL与RPC。
		[ordered]@{ Name = 'mail'; Scope = 'global'; Manifest = "$goSvc/mail.yaml"; DefaultPools = 0; GoMaxProcs = 0; PubSub = 0; Fixed = 16 }
	)
	if ($QueueSplit) {
		$clients = @($clients | Where-Object { $_.Name -ne 'gateway' })
	}
	foreach ($client in $clients) {
		$client['Replicas'] = Get-ManifestDeploymentReplicas -RelativePath $client.Manifest -LedgerName $ledgerName
		# 显式池上限:有 PoolMirror 的行读服务 yaml(与 ConfigMap 镜像同一批键,不是正整数即 throw),没有的行是 0。
		$configuredPoolConns = 0
		if ($client.Contains('PoolMirror')) {
			foreach ($poolSize in (Get-RedisPoolMirror -Target $client.PoolMirror).Values) { $configuredPoolConns += $poolSize }
		}
		$client['ConfiguredPoolConns'] = $configuredPoolConns
	}
	return [ordered]@{
		MaxClients               = Get-RedisMaxClientsFromManifest
		GoPoolSizePerProc        = 10
		# cpp/libs/engine/core/node/system/node/node.cpp 的 tlsRedis.Connect 是 C++ 唯一的建连点:每进程 1 条(不是每线程);
		# redis_manager.cpp 断线重连先销毁旧连接再新建,不叠加。
		CppConnectionsPerProcess = 1
		Clients                  = $clients
	}
}

<#
.SYNOPSIS
	queue 实例(redis-queue-g<N>,G4a 第 1a 批)的 Redis 连接总账输入,形状同 Get-RedisConnectionLedgerInputs,交给同一个 Get-RedisConnectionLedger。

.DESCRIPTION
	连 queue 实例的只有两类客户端,都按 zone 计:
	  - login:Node.QueueRedis 是独立的 go-redis 池(login 的 newQueueRedis),池上限与 RedisClient 同值 —— ConfigMap 里 QueueRedis.PoolSize
	    镜像的就是 Node.RedisClient.PoolSize(Get-RedisPoolMirror 同一个键)。任务结果的 Pub/Sub 仍在核心,这里不记;
	  - gateway:与核心总账 gateway 行同一口径(Lettuce 共享连接 1 条 + Bucket4j 专用连接 1 条,按 2 记)。
	没有 C++、没有全局池。maxclients 读 manifests/infra/redis-purpose.yaml 的 MAXCLIENTS(所有组共用这一个模板)。
	缺 manifest、replicas 不是恰好一处、MAXCLIENTS 不是恰好一处、PoolSize 不是正整数,照常 throw。
#>
function Get-RedisQueueConnectionLedgerInputs {
	$ledgerName = 'Redis 连接总账(queue 实例)'
	$clients = @(
		[ordered]@{ Name = 'login'; Scope = 'zone'; Manifest = 'deploy/k8s/manifests/go-svc/login.yaml'; PoolMirror = 'login'; DefaultPools = 0; GoMaxProcs = 0; PubSub = 0; Fixed = 0 }
		[ordered]@{ Name = 'gateway'; Scope = 'zone'; Manifest = 'deploy/k8s/manifests/java-svc/gateway.yaml'; DefaultPools = 0; GoMaxProcs = 0; PubSub = 0; Fixed = 2 }
	)
	foreach ($client in $clients) {
		$client['Replicas'] = Get-ManifestDeploymentReplicas -RelativePath $client.Manifest -LedgerName $ledgerName
		$configuredPoolConns = 0
		if ($client.Contains('PoolMirror')) {
			foreach ($poolSize in (Get-RedisPoolMirror -Target $client.PoolMirror).Values) { $configuredPoolConns += $poolSize }
		}
		$client['ConfiguredPoolConns'] = $configuredPoolConns
	}
	return [ordered]@{
		MaxClients               = Get-RedisMaxClientsFromManifest -RelativePath 'deploy/k8s/manifests/infra/redis-purpose.yaml'
		GoPoolSizePerProc        = 10
		CppConnectionsPerProcess = 1
		Clients                  = $clients
	}
}

<#
.SYNOPSIS
	Redis 连接总账:由输入与部署形态算出每 zone / 全局 / 稳态 / 滚动余量 / 峰值 / 告警水位 / 余量。

.DESCRIPTION
	记号:Z = zone 数;C = 全部 zone 的 gate + scene 进程数之和;B = battle 进程数;R = 滚动期间每个工作负载多出的实例数;
	每副本 = ConfiguredPoolConns + DefaultPools × GoPoolSizePerProc × GoMaxProcs + PubSub + Fixed(见 Get-RedisConnectionLedgerInputs)。
	  每 zone   = Σ(Scope=zone 的客户端:副本数 × 每副本)
	  全局      = Σ(Scope=global 的客户端:副本数 × 每副本)+ B × 每进程
	  稳态      = Z × 每 zone + C × 每进程 + 全局
	  滚动余量  = R × (Σ 每副本 + 3 × 每进程)   # 表里每个客户端一个 Deployment;C++ 按 gate / scene / battle 三个工作负载计
	                                            # (scene 拆分池时是四个,差一两条不影响结论)
	  峰值      = 稳态 + 滚动余量 + 运维余量
	  告警水位  = ⌊maxclients × WarnRatio⌋;余量 = maxclients − 峰值
	C 传全部 zone 之和而不是每 zone 数:zones 配置里各 zone 的 gate / scene 台数可以不同。
	all-up 不带 -WaitReady 时各 zone 的滚动会重叠,不在滚动余量里(同 MySQL 总账)。
	-CppSurgeWorkloads:滚动余量里按几个 C++ 工作负载计(上式的 3)。缺省 3,原有调用不传;queue 实例没有 C++ 客户端,传 0。
	纯函数,不读文件;输入不是整数、越界即 throw(fail-closed:不替你猜一个连接数)。
#>
function Get-RedisConnectionLedger {
	param(
		[Parameter(Mandatory = $true)][System.Collections.IDictionary]$Inputs,
		[Parameter(Mandatory = $true)][int]$ZoneCount,
		[Parameter(Mandatory = $true)][int]$CppZoneProcesses,
		[Parameter(Mandatory = $true)][int]$BattleProcesses,
		[Parameter(Mandatory = $true)][int]$RolloutExtraPodsPerDeployment,
		[Parameter(Mandatory = $true)][int]$OpsReserve,
		[Parameter(Mandatory = $true)][double]$WarnRatio,
		[ValidateRange(0, 3)][int]$CppSurgeWorkloads = 3
	)
	$v = @{}
	foreach ($key in @('MaxClients', 'GoPoolSizePerProc', 'CppConnectionsPerProcess')) {
		$n = 0
		if (-not $Inputs.Contains($key) -or -not [int]::TryParse([string]$Inputs[$key], [ref]$n) -or $n -le 0) {
			throw "Redis 连接总账:输入 $key='$($Inputs[$key])' 必须是正整数(fail-closed:不替你猜一个连接数)"
		}
		$v[$key] = $n
	}
	if ($ZoneCount -le 0 -or $CppZoneProcesses -lt 0 -or $BattleProcesses -lt 0 -or $RolloutExtraPodsPerDeployment -lt 0 -or $OpsReserve -lt 0) {
		throw "Redis 连接总账:ZoneCount=$ZoneCount 必须为正,CppZoneProcesses=$CppZoneProcesses / BattleProcesses=$BattleProcesses / RolloutExtraPodsPerDeployment=$RolloutExtraPodsPerDeployment / OpsReserve=$OpsReserve 不能为负"
	}
	if (-not ($WarnRatio -gt 0 -and $WarnRatio -le 1)) {
		throw "Redis 连接总账:WarnRatio=$WarnRatio 必须在 (0, 1] 内"
	}
	$clients = @()
	if ($Inputs.Contains('Clients')) { $clients = @($Inputs['Clients']) }
	if ($clients.Count -eq 0) {
		throw "Redis 连接总账:输入 Clients 为空(fail-closed:没有客户端表就算不出连接数)"
	}

	$perZone = 0
	$globalClients = 0
	$perReplicaSum = 0
	foreach ($client in $clients) {
		if ($client -isnot [System.Collections.IDictionary]) {
			throw "Redis 连接总账:Clients 里有一项不是字典:'$client'"
		}
		$name = [string]$client['Name']
		if ([string]::IsNullOrWhiteSpace($name)) {
			throw "Redis 连接总账:Clients 里有一项没有 Name"
		}
		$f = @{}
		foreach ($field in @('Replicas', 'ConfiguredPoolConns', 'DefaultPools', 'GoMaxProcs', 'PubSub', 'Fixed')) {
			$n = 0
			if (-not $client.Contains($field) -or -not [int]::TryParse([string]$client[$field], [ref]$n) -or $n -lt 0) {
				throw "Redis 连接总账:客户端 $name 的 $field='$($client[$field])' 必须是非负整数(fail-closed:不替你猜一个连接数)"
			}
			$f[$field] = $n
		}
		if ($f.DefaultPools -gt 0 -and $f.GoMaxProcs -le 0) {
			throw "Redis 连接总账:客户端 $name 有 $($f.DefaultPools) 个缺省大小的 Go 连接池,GoMaxProcs 必须为正(缺省池上限 = GoPoolSizePerProc × GOMAXPROCS)"
		}
		$perReplica = $f.ConfiguredPoolConns + $f.DefaultPools * $v.GoPoolSizePerProc * $f.GoMaxProcs + $f.PubSub + $f.Fixed
		if ($perReplica -le 0) {
			throw "Redis 连接总账:客户端 $name 每副本 0 条连接:不连这台 Redis 的服务不该在表里"
		}
		switch -CaseSensitive ([string]$client['Scope']) {
			'zone' { $perZone += $f.Replicas * $perReplica }
			'global' { $globalClients += $f.Replicas * $perReplica }
			default { throw "Redis 连接总账:客户端 $name 的 Scope='$($client['Scope'])' 只能是 zone 或 global" }
		}
		$perReplicaSum += $perReplica
	}

	$perProcess = $v.CppConnectionsPerProcess
	# 变量名不用 $global:它是作用域修饰符的名字(同 MySQL 总账)。
	$globalTotal = $globalClients + $BattleProcesses * $perProcess
	$steady = $ZoneCount * $perZone + $CppZoneProcesses * $perProcess + $globalTotal
	$surge = $RolloutExtraPodsPerDeployment * ($perReplicaSum + $CppSurgeWorkloads * $perProcess)
	$peak = $steady + $surge + $OpsReserve
	$warnAt = [int][Math]::Floor($v.MaxClients * $WarnRatio)
	return [ordered]@{
		PerZone = $perZone; CppZoneProcesses = $CppZoneProcesses; Global = $globalTotal; Steady = $steady
		RolloutSurge = $surge; OpsReserve = $OpsReserve; Peak = $peak
		MaxClients = $v.MaxClients; WarnAt = $warnAt; Headroom = $v.MaxClients - $peak
	}
}

# 一个 zone 的 C++ 进程数(gate + scene),Redis 连接总账用(每进程 1 条)。scene 的口径同 Apply-Zone / Resolve-SceneDeploymentPlan:
# scene_world / scene_instance 任一 ≥ 0 是拆分模式(两池之和,legacy scene 被忽略),否则 legacy 单池;Agones 开了 FleetAutoscaler 时
# 每个池按上界 -AgonesMaxReplicas 计。不调 Resolve-SceneDeploymentPlan:它会打拆分模式的 WARNING,在总账里调一次就多一遍。
function Get-RedisZoneCppProcesses {
	param(
		[Parameter(Mandatory = $true)][int]$Gate,
		[Parameter(Mandatory = $true)][int]$Scene,
		[Parameter(Mandatory = $true)][int]$SceneWorld,
		[Parameter(Mandatory = $true)][int]$SceneInstance
	)
	$splitMode = ($SceneWorld -ge 0 -or $SceneInstance -ge 0)
	if ($SceneOrchestrator -eq "agones" -and $AgonesAutoscale) {
		$pools = if ($splitMode) { 2 } else { 1 }
		$sceneProcesses = $pools * [Math]::Max(0, $AgonesMaxReplicas)
	}
	elseif ($splitMode) {
		$sceneProcesses = [Math]::Max(0, $SceneWorld) + [Math]::Max(0, $SceneInstance)
	}
	else {
		$sceneProcesses = [Math]::Max(0, $Scene)
	}
	return [Math]::Max(0, $Gate) + $sceneProcesses
}

<#
.SYNOPSIS
	写路径打印 Redis 连接总账一行:本次部署一份、规划档一份;任一份峰值超过告警水位(80% × maxclients)时告警,不阻断。

.DESCRIPTION
	只在写路径(zone-up / infra-up / all-up)的入口调用;*-down / *-status 不经过这里。
	本次部署那一份的 zone 集合与 Write-MysqlConnectionLedger 同一口径(两处要一起改;契约测试比对两行的 zones= 相等):
	  - zone-up:zones 配置里的全部 zone ∪ 本次 -ZoneId,-ZoneId 那个 zone 的 gate / scene 按命令行台数计;
	  - all-up / infra-up:只取 zones 配置里的 zone,台数按 zones 配置(没写的键落回命令行,同 Get-ZonesFromJson);
	  - zones 配置缺席:Z 取 1(-ZoneId 那一个,台数按命令行),行尾标 zones_config=missing。
	battle 按 -BattleReplicas 计(zone-up 不部署 battle,但它照样连着这台 Redis);其余形态参数取规划档。
	输入缺键、MAXCLIENTS / replicas 不是恰好一处,照常 throw(与 MySQL 那份同一纪律)。
#>
function Write-RedisConnectionLedger {
	$ledgerPlan = Get-RedisConnectionPlan
	# 本次部署那一份按 -RouteStoreMode 记 D1 对账 leader 的独立池(只有 db_first 才装配),规划档按 Get-RedisConnectionPlan 的 RouteStoreDbFirst 提前计入。
	$ledgerInputs = Get-RedisConnectionLedgerInputs -RouteStoreDbFirst ($RouteStoreMode -ceq 'db_first')
	$planLedgerInputs = Get-RedisConnectionLedgerInputs -RouteStoreDbFirst ([bool]$ledgerPlan.RouteStoreDbFirst)

	# 键是 zoneId(int)。不用 [ordered]@{}:OrderedDictionary 拿 int 当下标是按位置取值,不是按键。
	$cppByZone = [System.Collections.Generic.Dictionary[int, int]]::new()
	$zonesConfigTail = ''
	$zonesPath = Resolve-ZonesConfigPath
	# 与 Write-MysqlConnectionLedger / Get-ZonesFromJson 用同一个判据(Test-Path 不带 -LiteralPath)。
	if (Test-Path $zonesPath) {
		foreach ($zone in (Get-ZonesFromJson -Path $zonesPath)) {
			$cppByZone[[int]$zone.zoneId] = Get-RedisZoneCppProcesses -Gate $zone.gate -Scene $zone.scene `
				-SceneWorld $zone.scene_world -SceneInstance $zone.scene_instance
		}
		if ($Command -eq "zone-up") {
			$cppByZone[[int]$ZoneId] = Get-RedisZoneCppProcesses -Gate $GateReplicas -Scene $SceneReplicas `
				-SceneWorld $SceneWorldReplicas -SceneInstance $SceneInstanceReplicas
		}
	}
	else {
		$cppByZone[[int]$ZoneId] = Get-RedisZoneCppProcesses -Gate $GateReplicas -Scene $SceneReplicas `
			-SceneWorld $SceneWorldReplicas -SceneInstance $SceneInstanceReplicas
		$zonesConfigTail = ' zones_config=missing'
	}
	$deployCppProcesses = 0
	foreach ($processes in $cppByZone.Values) { $deployCppProcesses += $processes }

	# 按用途拆实例(G4a 第 1a 批,-RedisSplit):每个 zone 的排队拓扑。全部 zone 都拆出 queue 时 gateway 只连 queue 实例,从核心这一份去掉
	# (有一个 zone 没拆就照旧记在核心,偏保守);login 的 QueueRedis 池与 gateway 记在 queue 实例自己的一份(函数末尾)。
	# 不拆(缺省)时两份输入原样,核心这一行与拆分前逐字相同,也不打 queue 那一行。
	$queueTopologyByZone = [System.Collections.Generic.Dictionary[int, object]]::new()
	foreach ($zoneIdKey in @($cppByZone.Keys)) { $queueTopologyByZone[$zoneIdKey] = Get-RedisPurposeTopology -CurrentZoneId $zoneIdKey }
	$queueSplitAllZones = ($queueTopologyByZone.Count -gt 0 -and @($queueTopologyByZone.Values | Where-Object { -not $_.QueueSplit }).Count -eq 0)
	if ($queueSplitAllZones) {
		$ledgerInputs = Get-RedisConnectionLedgerInputs -RouteStoreDbFirst ($RouteStoreMode -ceq 'db_first') -QueueSplit $true
		$planLedgerInputs = Get-RedisConnectionLedgerInputs -RouteStoreDbFirst ([bool]$ledgerPlan.RouteStoreDbFirst) -QueueSplit $true
	}

	$deployLedger = Get-RedisConnectionLedger -Inputs $ledgerInputs -ZoneCount $cppByZone.Count -CppZoneProcesses $deployCppProcesses `
		-BattleProcesses ([Math]::Max(0, $BattleReplicas)) -RolloutExtraPodsPerDeployment $ledgerPlan.RolloutExtraPodsPerDeployment `
		-OpsReserve $ledgerPlan.OpsReserve -WarnRatio $ledgerPlan.WarnRatio
	$planLedger = Get-RedisConnectionLedger -Inputs $planLedgerInputs -ZoneCount $ledgerPlan.ZoneCount `
		-CppZoneProcesses ($ledgerPlan.ZoneCount * ($ledgerPlan.GatePerZone + $ledgerPlan.ScenePerZone)) `
		-BattleProcesses $ledgerPlan.BattleProcesses -RolloutExtraPodsPerDeployment $ledgerPlan.RolloutExtraPodsPerDeployment `
		-OpsReserve $ledgerPlan.OpsReserve -WarnRatio $ledgerPlan.WarnRatio

	$ledgerLineFormat = "Redis connection ledger: zones={0} steady={1} peak={2} maxclients={3} warn_at={4} headroom={5}; plan zones={6} peak={7} headroom={8}{9}"
	Write-Host ($ledgerLineFormat -f $cppByZone.Count, $deployLedger.Steady, $deployLedger.Peak, $deployLedger.MaxClients, $deployLedger.WarnAt,
		$deployLedger.Headroom, $ledgerPlan.ZoneCount, $planLedger.Peak, $planLedger.Headroom, $zonesConfigTail)

	$overWarn = New-Object System.Collections.Generic.List[string]
	if ($deployLedger.Peak -gt $deployLedger.WarnAt) {
		$overWarn.Add(("this deploy zones={0} peak {1}" -f $cppByZone.Count, $deployLedger.Peak))
	}
	if ($planLedger.Peak -gt $planLedger.WarnAt) {
		$overWarn.Add(("plan zones={0} (Get-RedisConnectionPlan) peak {1}" -f $ledgerPlan.ZoneCount, $planLedger.Peak))
	}
	if ($overWarn.Count -gt 0) {
		# 开头「[RedisConnectionLedger]OverWarnRatio:」刻意不留空格:ConsoleHost 可能按窗口宽度在空格处折行,契约测试只断这一段
		# (同 MySQL 那份)。改措辞要同步改契约测试与 deploy/k8s/README.md「Redis 连接上限与总账」一节。格式串先拼好再 -f。
		$overWarnFormat = ("[RedisConnectionLedger]OverWarnRatio:{0}; maxclients={1} warn_at={2} (deploy/k8s/manifests/infra/redis.yaml). Deployment continues: warning only, not blocking. " +
			"Raise the limit before the instance fills up (redis-cli cannot connect once it is full): CONFIG SET maxclients on redis-0 first, " +
			"then MAXCLIENTS in redis.yaml in a maintenance window (restarts redis-0, needs -ConfirmRedisCoreRestart); " +
			"or split the shared Redis by purpose (docs/design/shared-redis-split-by-purpose.md).")
		Write-Warning ($overWarnFormat -f ($overWarn -join '; '), $deployLedger.MaxClients, $deployLedger.WarnAt)
	}

	# 高可用形态(-RedisReplicas 3,redis-ha.yaml):客户端经代理连 Redis,先到线的是代理(PROXY_MAXCONN × 副本 + 预留 ≤ MAXCLIENTS,
	# Get-RedisHaLimits 已保证)。一个代理不在(所在节点故障;滚动时 hard-stop-after 10 秒后断开)时,它的客户端全部重连到另一个,
	# 所以再按「单个代理的 PROXY_MAXCONN」算一次告警水位(同一个 WarnRatio)。超过只告警、不阻断,纪律同上。
	$haTopology = Get-RedisTopology
	if ($haTopology.HighAvailability) {
		$haManifestRelative = 'deploy/k8s/manifests/infra/' + $haTopology.Manifest
		$haManifestFull = Join-Path $RepoRoot ($haManifestRelative -replace '/', [System.IO.Path]::DirectorySeparatorChar)
		if (-not (Test-Path -LiteralPath $haManifestFull)) {
			throw "Redis 连接总账:选的是高可用形态(-RedisReplicas $($haTopology.Replicas)),但找不到 $haManifestRelative(fail-closed)"
		}
		$haLimits = Get-RedisHaLimits -ManifestContent ([string](Get-Content -LiteralPath $haManifestFull -Raw)) -SingleInstanceMaxClients $deployLedger.MaxClients
		$proxyWarnAt = [int][Math]::Floor($haLimits.ProxyMaxConn * $ledgerPlan.WarnRatio)
		Write-Host ("Redis proxy ledger: proxy_maxconn={0} proxies={1}+{2} warn_at={3} (one proxy carries every client while the other is down); deploy peak={4} plan peak={5}" -f
			$haLimits.ProxyMaxConn, $haLimits.ProxyReplicas, $haLimits.ProxyMaxSurge, $proxyWarnAt, $deployLedger.Peak, $planLedger.Peak)
		$overProxy = New-Object System.Collections.Generic.List[string]
		if ($deployLedger.Peak -gt $proxyWarnAt) {
			$overProxy.Add(("this deploy zones={0} peak {1}" -f $cppByZone.Count, $deployLedger.Peak))
		}
		if ($planLedger.Peak -gt $proxyWarnAt) {
			$overProxy.Add(("plan zones={0} (Get-RedisConnectionPlan) peak {1}" -f $ledgerPlan.ZoneCount, $planLedger.Peak))
		}
		if ($overProxy.Count -gt 0) {
			# 开头不留空格,理由同上一条告警。
			$overProxyFormat = ("[RedisConnectionLedger]OverProxyWarnRatio:{0}; proxy_maxconn={1} warn_at={2} (deploy/k8s/manifests/infra/redis-ha.yaml). Deployment continues: warning only, not blocking. " +
				"Raise PROXY_MAXCONN in redis-ha.yaml (proxies roll automatically) while keeping PROXY_MAXCONN x (proxies + maxSurge) + 1000 <= MAXCLIENTS; " +
				"beyond that raise MAXCLIENTS on all 3 redis nodes (CONFIG SET maxclients on each first, then both manifests in a maintenance window, needs -ConfirmRedisCoreRestart).")
			Write-Warning ($overProxyFormat -f ($overProxy -join '; '), $haLimits.ProxyMaxConn, $proxyWarnAt)
		}
	}

	Write-RedisQueueConnectionLedger -TopologyByZone $queueTopologyByZone -LedgerPlan $ledgerPlan
}

<#
.SYNOPSIS
	queue 实例(redis-queue-g<N>)的 Redis 连接总账:本次总账 zone 集合里每个出现的组打一行;任一份峰值超过告警水位时告警,不阻断(同核心那份)。

.DESCRIPTION
	输入 Get-RedisQueueConnectionLedgerInputs(login 的 QueueRedis 池 + gateway),算式同 Get-RedisConnectionLedger,C++ 与全局池都是 0,
	滚动余量不计 C++ 工作负载(-CppSurgeWorkloads 0)。本次部署那一份 = 属于该组的 zone 数;规划档按 Get-RedisConnectionPlan 的 zone 数
	全部落在同一组算(最坏情况,组怎么分由 zones 配置定)。没有任何 zone 拆出 queue 时什么都不打,缺省输出与拆分前逐字相同。
	行首是「Redis queue connection ledger:」,与核心那行(「Redis connection ledger: zones=」)不同前缀,按前缀找核心那行的契约测试不受影响。
#>
function Write-RedisQueueConnectionLedger {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyCollection()][System.Collections.IDictionary]$TopologyByZone,
		[Parameter(Mandatory = $true)][System.Collections.IDictionary]$LedgerPlan
	)

	$zonesByInstance = [ordered]@{}
	foreach ($topology in $TopologyByZone.Values) {
		if (-not $topology.QueueSplit) { continue }
		$instance = [string]$topology.Queue.Service
		if (-not $zonesByInstance.Contains($instance)) { $zonesByInstance[$instance] = 0 }
		$zonesByInstance[$instance] = [int]$zonesByInstance[$instance] + 1
	}
	if ($zonesByInstance.Count -eq 0) { return }

	$inputs = Get-RedisQueueConnectionLedgerInputs
	$ledgerArgs = @{
		Inputs                        = $inputs
		CppZoneProcesses              = 0
		BattleProcesses               = 0
		RolloutExtraPodsPerDeployment = [int]$LedgerPlan.RolloutExtraPodsPerDeployment
		OpsReserve                    = [int]$LedgerPlan.OpsReserve
		WarnRatio                     = [double]$LedgerPlan.WarnRatio
		CppSurgeWorkloads             = 0
	}
	$planLedger = Get-RedisConnectionLedger @ledgerArgs -ZoneCount ([int]$LedgerPlan.ZoneCount)
	$overWarn = New-Object System.Collections.Generic.List[string]
	foreach ($instance in @($zonesByInstance.Keys | Sort-Object)) {
		$zoneCount = [int]$zonesByInstance[$instance]
		$ledger = Get-RedisConnectionLedger @ledgerArgs -ZoneCount $zoneCount
		Write-Host ("Redis queue connection ledger: instance={0} zones={1} steady={2} peak={3} maxclients={4} warn_at={5} headroom={6}; plan zones={7} peak={8} headroom={9} (deploy/k8s/manifests/infra/redis-purpose.yaml)" -f
			$instance, $zoneCount, $ledger.Steady, $ledger.Peak, $ledger.MaxClients, $ledger.WarnAt, $ledger.Headroom, $LedgerPlan.ZoneCount, $planLedger.Peak, $planLedger.Headroom)
		if ($ledger.Peak -gt $ledger.WarnAt) { $overWarn.Add(("{0} zones={1} peak {2}" -f $instance, $zoneCount, $ledger.Peak)) }
	}
	if ($planLedger.Peak -gt $planLedger.WarnAt) { $overWarn.Add(("plan zones={0} in one group peak {1}" -f $LedgerPlan.ZoneCount, $planLedger.Peak)) }
	if ($overWarn.Count -gt 0) {
		# 开头不留空格,理由同核心那条告警(ConsoleHost 可能在空格处折行)。
		Write-Warning ("[RedisQueueConnectionLedger]OverWarnRatio:{0}; maxclients={1} warn_at={2} (deploy/k8s/manifests/infra/redis-purpose.yaml). Deployment continues: warning only, not blocking. Raise MAXCLIENTS in redis-purpose.yaml (restarts the queue instances) or split zones into more redisQueueGroup values." -f
			($overWarn -join '; '), $planLedger.MaxClients, $planLedger.WarnAt)
	}
}

<#
.SYNOPSIS
	共享 Redis 的 Pod 模板摘要:取 StatefulSet redis 的 spec.template 段,去掉 YAML 注释与空行、行尾空白后算 SHA-256,取前 16 位十六进制。

.DESCRIPTION
	重启闸用它判断「这次 apply 会不会改 Pod 模板(= 重启 redis-0)」。只看 spec.template:volumeClaimTemplates 不可变,
	StatefulSet 自己的 metadata(含摘要注解)、PDB、Service 的改动都不重建 redis-0。
	字面块(`|` / `>`)里的行原样保留(只把 CRLF 统一成 LF,去掉块尾空行):启动薄壳里的一行 shell 注释也是 Pod 模板的一部分,改了照样滚动。
	字面块之外的 YAML 注释与空行不进摘要:只改注释不会重启,也就不该要求确认。行尾注释(`key: v  # 注释`)没有剥掉,
	改它会多要一次确认(偏保守)。kubectl edit 之类绕过本脚本的改动看不见:比较的是「上次经本脚本 apply 的模板」。
	纯函数,不读文件;找不到恰好一个 StatefulSet redis、或它没有非空的 spec.template 段即 throw。
#>
function Get-RedisPodTemplateDigest {
	param([Parameter(Mandatory = $true)][string]$ManifestContent)

	$documents = @(($ManifestContent -replace "`r`n", "`n") -split '(?m)^---[ \t]*$')
	$statefulSets = @($documents | Where-Object { $_ -cmatch '(?m)^kind:[ \t]*StatefulSet[ \t]*$' -and $_ -cmatch '(?m)^  name:[ \t]*redis[ \t]*$' })
	if ($statefulSets.Count -ne 1) {
		throw "Redis Pod 模板摘要:清单里 kind: StatefulSet、metadata.name 为 redis 的文档应恰好 1 份,实际 $($statefulSets.Count) 份"
	}
	$lines = $statefulSets[0] -split "`n"
	$start = -1
	for ($i = 0; $i -lt $lines.Count; $i++) {
		if ($lines[$i] -cmatch '^  template:[ \t]*$') {
			if ($start -ge 0) { throw "Redis Pod 模板摘要:StatefulSet redis 里两格缩进的 template: 出现了不止一次" }
			$start = $i
		}
	}
	if ($start -lt 0) {
		throw "Redis Pod 模板摘要:StatefulSet redis 里找不到 spec.template(两格缩进的 template: 行)"
	}

	$canonical = New-Object System.Collections.Generic.List[string]
	# ≥ 0 = 正处在字面块里,值是引出字面块那一行的缩进;字面块的正文是缩进比它深的行(以及其间的空行)。
	$blockIndent = -1
	$endBlock = {
		while ($canonical.Count -gt 0 -and [string]::IsNullOrWhiteSpace($canonical[$canonical.Count - 1])) {
			$canonical.RemoveAt($canonical.Count - 1)
		}
	}
	for ($i = $start + 1; $i -lt $lines.Count; $i++) {
		$line = $lines[$i]
		$trimmed = $line.Trim()
		$indent = $line.Length - $line.TrimStart(' ').Length
		if ($blockIndent -ge 0) {
			if ($trimmed.Length -eq 0 -or $indent -gt $blockIndent) {
				$canonical.Add($line)
				continue
			}
			. $endBlock
			$blockIndent = -1
		}
		if ($trimmed.Length -eq 0 -or $trimmed.StartsWith('#')) { continue }
		# 缩进回到 spec 的子键(volumeClaimTemplates 等)或更浅:template 段结束。
		if ($indent -le 2) { break }
		$canonical.Add($line.TrimEnd())
		if ($trimmed -match '(?:^-|:)[ \t]*[|>][-+0-9]*[ \t]*$') { $blockIndent = $indent }
	}
	if ($blockIndent -ge 0) { . $endBlock }
	if ($canonical.Count -eq 0) {
		throw "Redis Pod 模板摘要:StatefulSet redis 的 spec.template 段是空的"
	}

	$sha256 = [System.Security.Cryptography.SHA256]::Create()
	try {
		$bytes = $sha256.ComputeHash([System.Text.Encoding]::UTF8.GetBytes(($canonical -join "`n")))
	}
	finally {
		$sha256.Dispose()
	}
	return ((($bytes | ForEach-Object { $_.ToString('x2') }) -join '').Substring(0, 16))
}

# 线上 sts/redis 与本次渲染的 Pod 模板摘要 → 这次 apply 算哪一类(纯函数,契约测试直接喂值;写法同 Get-KafkaTopologyChangeVerdict):
#   absent             线上既没有 sts/redis 也没有旧的 Deployment redis(全新集群):不存在重启,放行
#   legacy-deployment  线上没有 sts/redis,但还有 2026-09-19(2d29163f8)之前的 Deployment redis(emptyDir):Apply-Infra 会先删它
#                      再建 StatefulSet = 停机切换且数据不迁移,要求 -ConfirmRedisCoreRestart
#   unchanged          线上注解与本次摘要相同:相对「上次经本脚本 apply 的模板」没变,放行(带外改动看不见,见 Assert-RedisCoreRestartConfirmed)
#   restart            摘要不同,或线上没有注解(P0-E 之前部署的、或注解被删):apply 会重启 redis-0,要求 -ConfirmRedisCoreRestart
#   orphan-pods        线上没有 sts/redis、也没有旧 Deployment,但还有 StatefulSet 形态的 redis-<n> Pod 在服务(--cascade=orphan 之后,
#                      包括 1 → 3 切换在 orphan 删除之后 apply 失败的半截状态):本次 apply 新建的 StatefulSet 会收养并按新模板重建它们,
#                      要求 -ConfirmRedisCoreRestart(以前这里判 absent 放行,是上面「另一个盲区」;2026-10-19 补上)
function Get-RedisCoreRestartVerdict {
	param(
		[Parameter(Mandatory = $true)][bool]$LiveExists,
		[AllowNull()][AllowEmptyString()][string]$LiveDigest,
		[Parameter(Mandatory = $true)][string]$DesiredDigest,
		[bool]$LegacyDeployment = $false,
		[int]$OrphanPodCount = 0
	)

	if (-not $LiveExists) {
		if ($LegacyDeployment) { return 'legacy-deployment' }
		if ($OrphanPodCount -gt 0) { return 'orphan-pods' }
		return 'absent'
	}
	if (-not [string]::IsNullOrWhiteSpace($LiveDigest) -and $LiveDigest.Trim() -ceq $DesiredDigest) { return 'unchanged' }
	return 'restart'
}

# 线上 sts/redis 是否存在、它自己 metadata 上的 Pod 模板摘要注解(没有注解 = 空串),以及没有 sts 时是否还留着旧的
# Deployment redis(LegacyDeployment)。调用方先判 DryRun:DryRun 不连集群。
# 读法照 Get-LiveStatefulSetReplicas。查询失败一律 throw:查不到不等于不存在(fail-closed),
# 否则一次 kubectl 抖动就会把「要重启」误判成「全新集群」放行。
function Get-LiveRedisCoreTemplateDigest {
	param([Parameter(Mandatory = $true)][string]$Namespace)

	$baseArgs = Build-KubectlBaseArgs
	$allArgs = @()
	$allArgs += $baseArgs
	$allArgs += @("get", "statefulset", "redis", "-n", $Namespace, "--ignore-not-found", "-o", "json")
	$out = & kubectl @allArgs
	if ($LASTEXITCODE -ne 0) {
		throw "查询线上 StatefulSet redis 失败(namespace=$Namespace)。fail-closed:不知道现状就无法判断这次 apply 会不会重启 redis-0。"
	}
	$text = ([string]($out | Out-String)).Trim()
	if ([string]::IsNullOrEmpty($text)) {
		# 没有 sts/redis 不一定是全新集群:2026-09-19 之前部署、之后没再跑过 infra-up 的集群上 Redis 还是 Deployment + emptyDir,
		# Apply-Infra 会先 delete deployment redis 再建 StatefulSet —— 停掉 Redis 且数据不迁移,同样要确认。
		$legacyArgs = @()
		$legacyArgs += $baseArgs
		$legacyArgs += @("get", "deployment", "redis", "-n", $Namespace, "--ignore-not-found", "-o", "name")
		$legacyOut = & kubectl @legacyArgs
		if ($LASTEXITCODE -ne 0) {
			throw "查询线上 Deployment redis 失败(namespace=$Namespace)。fail-closed:不知道现状就无法判断这次 apply 会不会停掉旧的 Redis。"
		}
		$legacyText = ([string]($legacyOut | Out-String)).Trim()
		$legacy = (-not [string]::IsNullOrEmpty($legacyText))
		# 旧 Deployment 的 Pod 同样带 app=redis,但名字不是 redis-<n>;它的切换由 legacy-deployment 分支管,这里不再查孤儿 Pod。
		$orphans = @()
		if (-not $legacy) { $orphans = @(Get-LiveRedisOrphanPodOrdinals -Namespace $Namespace) }
		return [pscustomobject]@{ Exists = $false; Digest = ''; LegacyDeployment = $legacy; OrphanPods = $orphans }
	}
	$annotations = ($text | ConvertFrom-Json).metadata.annotations
	$digest = ''
	if ($null -ne $annotations) {
		$property = $annotations.PSObject.Properties[$script:RedisPodTemplateDigestAnnotation]
		if ($null -ne $property) { $digest = ([string]$property.Value).Trim() }
	}
	return [pscustomobject]@{ Exists = $true; Digest = $digest; LegacyDeployment = $false }
}

<#
.SYNOPSIS
	共享 Redis 的重启闸:本次 apply 会改变线上 sts/redis 的 Pod 模板(= 重启 redis-0)而没给 -ConfirmRedisCoreRestart 时 throw。

.DESCRIPTION
	由 Apply-Infra 在它的任何写操作(含 Ensure-Namespace、删旧 Deployment、建 Secret redis-auth)之前调用。
	语义比 G4a 设计稿(docs/design/shared-redis-split-by-purpose.md §4.2)原计划的「给了 -RedisCoreMaxMemory 就必须给 Confirm」更宽:
	那是这里的特例;这里还能拦住「上次带了参数、这次漏带」的回落重启,以及直接改 redis.yaml 的任何一处 Pod 模板。
	不采用 updateStrategy: OnDelete:改动会潜伏到下一次意外重启才生效,而且 kubectl rollout status 对 OnDelete 的 StatefulSet
	直接报错,infra-up 的等待正是用它(Wait-ForStatefulSetReady)。
	运维后果:P0-E 合入后第一次 infra-up(以及以后每次改 redis.yaml 的 Pod 模板)被拒是预期行为,按 deploy/k8s/README.md
	「Redis 连接上限与总账」在维护窗口里带 -ConfirmRedisCoreRestart 执行。
	盲区:比较的是线上注解(= 上次经本脚本 apply 的模板摘要),不是线上真实的 spec.template。有人用 kubectl edit / set image / patch
	带外改过 redis.yaml 里声明了的字段时,这里判 unchanged 放行,而 apply 的三方合并会把那些字段改回清单值、照样重启 redis-0。
	改成拿 server-side dry-run 的模板与线上模板比较可以补上,待负责人拍板与实测(交接 handoff-global-singletons-p0-20261015.md)。
	以前的另一个盲区(2026-10-19 已补):有人 `kubectl delete sts redis --cascade=orphan`(改 volumeClaimTemplates 之类不可变字段的常规做法,
	或 1 → 3 切换在 orphan 删除之后 apply 失败)后,redis-<n> Pod 与 PVC 仍在服务,以前这里查不到 sts、也查不到旧 Deployment,判 absent 放行。
	现在没有 sts 与旧 Deployment 时再查带 app=redis 的 redis-<n> Pod(Get-LiveRedisOrphanPodOrdinals),有就判 orphan-pods,同样要求确认。
#>
function Assert-RedisCoreRestartConfirmed {
	param(
		[Parameter(Mandatory = $true)][string]$DesiredDigest,
		# 线上与本次都是高可用形态(Get-RedisTopologyChangeVerdict = unchanged 且 3 副本):模板变化 = 逐个滚动 3 个 redis Pod,
		# 每个先由停机钩子交接主,不是「重启 redis-0、重放 AOF」。只换后果的说法,确认要求不变(仍是一次需要知情的变更)。
		# 单实例与 1 → 3 切换不传:那两种都是 redis-0 带着全服重启。
		[switch]$RollingHandover
	)

	if ($DryRun) {
		# 行尾带上本次收到的开关值:DryRun 不查集群,从包装入口核对 -ConfirmRedisCoreRestart 有没有透传下来只能看这一项。
		Write-Host "Redis restart gate: skipped in DryRun (cluster not queried); rendered pod template digest=$DesiredDigest confirm_redis_core_restart=$([bool]$ConfirmRedisCoreRestart)"
		return
	}
	$live = Get-LiveRedisCoreTemplateDigest -Namespace $InfraNamespace
	$orphanPods = @()
	if ($null -ne $live.PSObject.Properties['OrphanPods']) { $orphanPods = @($live.OrphanPods) }
	$verdict = Get-RedisCoreRestartVerdict -LiveExists $live.Exists -LiveDigest $live.Digest -DesiredDigest $DesiredDigest `
		-LegacyDeployment ([bool]$live.LegacyDeployment) -OrphanPodCount $orphanPods.Count
	if ($verdict -eq 'orphan-pods') {
		$orphanText = (@($orphanPods | ForEach-Object { "redis-$_" }) -join ', ')
		if ($ConfirmRedisCoreRestart) {
			Write-Warning "[RedisCoreRestart]Confirmed:-ConfirmRedisCoreRestart supplied, statefulset/redis is gone but orphan pods $orphanText are still serving in namespace $InfraNamespace (after --cascade=orphan, e.g. an interrupted 1 -> 3 switch). The StatefulSet applied now adopts them and recreates them with the new template, digest=$DesiredDigest."
			return
		}
		throw @"
拒绝 apply 共享 Redis(namespace=$InfraNamespace):线上没有 statefulset/redis,但 Pod $orphanText 仍在服务
(--cascade=orphan 之后的状态,常见于上一次 1 → 3 切换在 orphan 删除之后 apply 失败)。本次 apply 新建的 StatefulSet 会收养它们并按新模板重建 =
一次共享 Redis 重启(单实例时全服登录中断)。本次未做任何写操作。确认要在本次维护窗口继续,加 -ConfirmRedisCoreRestart 重跑
(经 dev_tools.ps1 / k8s_image.ps1 同名参数透传);步骤见 deploy/k8s/README.md「共享 Redis:高可用形态 → 切换与回退」。
"@
	}
	if ($verdict -eq 'absent') {
		Write-Host "Redis restart gate: neither statefulset/redis nor the legacy deployment/redis exists in namespace $InfraNamespace (first deployment), digest=$DesiredDigest"
		return
	}
	if ($verdict -eq 'unchanged') {
		# 措辞不说「不会重启」:只证明了与上次经本脚本 apply 的模板相同,带外改过的字段 apply 会改回去(见上面「盲区」)。
		Write-Host "Redis restart gate: pod template unchanged since the last apply by this script (digest=$DesiredDigest); out-of-band edits to pod template fields declared in redis.yaml are not detected and would be reverted by this apply"
		return
	}
	if ($verdict -eq 'legacy-deployment') {
		if ($ConfirmRedisCoreRestart) {
			Write-Warning "[RedisCoreRestart]Confirmed:-ConfirmRedisCoreRestart supplied, legacy deployment/redis (emptyDir) found in namespace $InfraNamespace. This apply deletes it and creates statefulset/redis: Redis stops, its data is not migrated, every player must log in again."
			return
		}
		throw @"
拒绝 apply 共享 Redis(namespace=$InfraNamespace):线上没有 statefulset/redis,但还有旧的 deployment/redis(emptyDir,2026-09-19 之前的形态)。
本次 apply 会先删掉它再建 StatefulSet:Redis 停机,会话 / 锁 / 位置记录 / 战斗结算待销账记录全部清空、不迁移,全服需要重新登录。
本次未做任何写操作。确认要在本次维护窗口做这次切换,加 -ConfirmRedisCoreRestart 重跑(经 dev_tools.ps1 / k8s_image.ps1 同名参数透传);
切换前先停写,确认 db_task 积压、重试队列与死信都为 0。步骤见 deploy/k8s/README.md「Redis 连接上限与总账」。
"@
	}
	$liveText = if ([string]::IsNullOrWhiteSpace($live.Digest)) { '<none>' } else { $live.Digest }
	if ($RollingHandover) {
		if ($ConfirmRedisCoreRestart) {
			Write-Warning "[RedisCoreRestart]Confirmed:-ConfirmRedisCoreRestart supplied, redis pod template digest $liveText -> $DesiredDigest. This apply rolls the 3 redis pods one at a time; each pod hands the primary role over before it stops (writes pause about 2-3 s per handover)."
			return
		}
		throw @"
拒绝 apply 共享 Redis(namespace=$InfraNamespace):本次 redis-ha.yaml 的 Pod 模板摘要 $DesiredDigest 与线上 statefulset/redis 的 $liveText 不同
(<none> = 线上还没有摘要注解)。apply 会逐个滚动重启 3 个 redis Pod:每个 Pod 停机前先由停机钩子把主交接出去,
每次交接写入挂起约 2~3 秒(2026-10-08 本机 kind 实测零丢失;真实集群未测)。
本次未做任何写操作。确认要在本次窗口滚动,加 -ConfirmRedisCoreRestart 重跑(经 dev_tools.ps1 / k8s_image.ps1 同名参数透传)。
现在不打算滚动就不要跑 infra-up / 不带 -SkipInfra 的 all-up(zone 级发布用 zone-up 或 all-up -SkipInfra)。
步骤见 deploy/k8s/README.md「共享 Redis:高可用形态」。
"@
	}
	if ($ConfirmRedisCoreRestart) {
		Write-Warning "[RedisCoreRestart]Confirmed:-ConfirmRedisCoreRestart supplied, redis pod template digest $liveText -> $DesiredDigest. This apply restarts redis-0: logins in all zones are interrupted while the AOF is replayed."
		return
	}
	throw @"
拒绝 apply 共享 Redis(namespace=$InfraNamespace):本次所选 Redis 清单(-RedisReplicas 1 = redis.yaml,1 → 3 切换 = redis-ha.yaml)的 Pod 模板摘要 $DesiredDigest 与线上 statefulset/redis 的 $liveText 不同
(<none> = 线上还没有摘要注解:P0-E 之前部署的,或注解被删过)。apply 会重启 redis-0,全服登录中断并重放 AOF。
本次未做任何写操作。确认要在本次维护窗口重启,加 -ConfirmRedisCoreRestart 重跑(经 dev_tools.ps1 / k8s_image.ps1 同名参数透传);
在存盘关键写补上 WAITAOF 之前,重启前先停写,确认 db_task 积压、重试队列与死信都为 0。
现在不打算重启就不要跑 infra-up / 不带 -SkipInfra 的 all-up(zone 级发布用 zone-up 或 all-up -SkipInfra)。
步骤见 deploy/k8s/README.md「Redis 连接上限与总账」。
"@
}

<#
.SYNOPSIS
	部署后核对(非 DryRun 且带 -WaitReady):逐个读线上 redis-0 .. redis-<Nodes-1> 的 CONFIG GET maxclients,与清单的 MAXCLIENTS 比较,不等只告警。

.DESCRIPTION
	这是到线之前唯一能自动发现「maxclients 被容器 nofile 硬限悄悄下调」的地方(下调只在 redis-server 启动日志里留一条)。
	高可用形态(-Nodes 3)每个节点都要核:任何一个从都可能在下一次切主后变成主,它的上限就是全服的上限。
	-h 127.0.0.1 读的是该 Pod 自己的 redis-server(不经 Service / 代理),主、从都答 CONFIG GET。
	口令取 Pod 自己的 REDIS_PASSWORD(Secret redis-auth),经 REDISCLI_AUTH 交给 redis-cli:不经本机命令行、不进日志;
	为空时不设(设成空串 redis-cli 会发 AUTH "" 并报错)。写法同 k8s_gate_drain.ps1 的 Invoke-GateDrainRedis。
	读不到(exec 失败、已经到线连不上、回复认不出)同样只告警:核对是观测不是门禁,清单此时已经 apply 完了。
	-StatefulSetName 缺省 redis(核心,原有调用全部不传,输出不变);按用途拆出的 queue 实例传 redis-queue-g<N>(G4a 第 1a 批),
	Pod 名按 <名字>-<序号> 取,容器同样叫 redis,成功那一行末尾多一个 statefulset=<名字>。
#>
function Test-RedisLiveMaxClients {
	param(
		[Parameter(Mandatory = $true)][int]$ExpectedMaxClients,
		[ValidateRange(1, 3)][int]$Nodes = 1,
		[string]$StatefulSetName = 'redis'
	)

	# 核心(缺省名字)的输出与引入 -StatefulSetName 之前逐字相同;别的实例在行尾点名,免得两行混在一起看不出是谁。
	$instanceTail = if ($StatefulSetName -ceq 'redis') { '' } else { " statefulset=$StatefulSetName" }
	if ($DryRun) {
		Write-Host "[dry-run] redis maxclients check skipped (manifest MAXCLIENTS=$ExpectedMaxClients)$instanceTail"
		return
	}
	$shell = 'if [ -n "$REDIS_PASSWORD" ]; then REDISCLI_AUTH=$REDIS_PASSWORD; export REDISCLI_AUTH; fi; exec redis-cli --raw -h 127.0.0.1 -p 6379 config get maxclients'
	$baseArgs = Build-KubectlBaseArgs
	$okCount = 0
	$lastLive = $null
	for ($i = 0; $i -lt $Nodes; $i++) {
		$pod = "$StatefulSetName-$i"
		$allArgs = @()
		$allArgs += $baseArgs
		$allArgs += @("exec", "-n", $InfraNamespace, "pod/$pod", "-c", "redis", "--", "sh", "-c", $shell)
		$out = & kubectl @allArgs
		$exitCode = $LASTEXITCODE
		$reply = [regex]::Match([string]($out | Out-String), '(?m)^\s*maxclients\s*\r?\n\s*(\d+)\s*$')
		if ($exitCode -ne 0 -or -not $reply.Success) {
			Write-Warning ("[RedisMaxClients]CheckFailed:could not read CONFIG GET maxclients from {2} (namespace={0}, exit={1}). If redis-cli cannot connect, the instance may already be full; otherwise check the 'redis fd limit:' line in: kubectl -n {0} logs {2} -c redis" -f $InfraNamespace, $exitCode, $pod)
			continue
		}
		$liveMaxClients = [long]$reply.Groups[1].Value
		if ($liveMaxClients -ne $ExpectedMaxClients) {
			Write-Warning ("[RedisMaxClients]Mismatch:{3} live maxclients={0} but manifest MAXCLIENTS={1} (namespace={2}). Most likely the container nofile hard limit is below MAXCLIENTS + 32 and redis-server lowered it: see the 'redis fd limit:' / ERROR lines in kubectl -n {2} logs {3} -c redis, then raise LimitNOFILE of the container runtime on the node. Or someone ran CONFIG SET maxclients. Deployment continues: warning only." -f $liveMaxClients, $ExpectedMaxClients, $InfraNamespace, $pod)
			continue
		}
		$okCount++
		$lastLive = $liveMaxClients
	}
	if ($okCount -ne $Nodes) { return }
	if ($Nodes -eq 1) {
		Write-Host "Redis maxclients check OK: live=$lastLive manifest MAXCLIENTS=$ExpectedMaxClients$instanceTail"
		return
	}
	Write-Host "Redis maxclients check OK: live=$lastLive manifest MAXCLIENTS=$ExpectedMaxClients nodes=$Nodes$instanceTail"
}

# ─────────────────────────────────────────────────────────────────
# 共享 Redis 的拓扑:单实例(redis.yaml)或高可用(redis-ha.yaml)
# (docs/design/no-single-node-horizontal-scaling-20261001.md §5;deploy/k8s/README.md「共享 Redis:高可用形态」)
# ─────────────────────────────────────────────────────────────────
# 高可用形态 = 1 主 2 从 + 每个 redis Pod 一个哨兵容器 + 2 个 haproxy 入口代理。Service redis 的名字与 ClusterIP 不变,
# 只是 selector 从 redis Pod 换成代理;代理只把连接交给「自己答 role:master、且多数哨兵认它是主」的节点。
# 所有客户端(C++ / Go / Java)零改动,仍连 redis.<infra>:6379。故障转移、滚动更新、整体停机再拉起、从单实例原地切换,
# 2026-10-08 都在本机 kind 的隔离命名空间里实测过(数字见设计文档 §5)。
# 同一个拓扑决定三件事,全部从 Get-RedisTopology 推出,不各写一份:
#   - Apply-Infra apply 哪份清单(Manifest);
#   - WAITAOF 要几个副本确认(Get-MappingRedisTopology → Get-RouteStoreWaitAOFNumReplicas;data-service 与 go/db 两处 ConfigMap);
#   - 部署后核对几个节点(Test-RedisLiveMaxClients -Nodes)。
# Get-RedisTopologyFor / Get-MappingRedisTopologyFor / Get-RedisTopologyChangeVerdict / Get-RedisZoneTopologyDecision / Get-RedisHaLimits /
# Get-RedisHaScriptsDigest / Assert-RedisPasswordSentinelSafe 是纯函数,契约测试按 AST 抽出直接调用;Get-RedisTopology 读脚本参数
# (与 zone 写路径跟随线上时记下的 $script:RedisReplicasFollowedLive),
# Assert-RedisTopologyChangeIsSafe / Resolve-RedisTopologyFromLive 读集群(DryRun 不查,一律当全新集群)。

# zone 写路径没给 -RedisReplicas、而线上 sts/redis 的副本数与档位缺省不同时,Resolve-RedisTopologyFromLive 把线上值记在这里,
# Get-RedisTopology 随后按它解析(只在写路径入口写一次;$null = 没有跟随)。
$script:RedisReplicasFollowedLive = $null

# 代理 Pod 模板上的脚本摘要占位(redis-ha.yaml 的 Deployment redis-proxy),由 Apply-Infra 换成 Get-RedisHaScriptsDigest 的结果。
$script:RedisHaScriptsDigestPlaceholder = '__REDIS_HA_SCRIPTS_DIGEST__'

# 副本数 → 拓扑(纯函数)。只认 1 和 3:哨兵要凑多数派,两个节点挂一个就没有多数派,比单实例还脆;
# 更多副本没有收益(读写都只走主),还会拖慢每次滚动。
function Get-RedisTopologyFor {
	param([Parameter(Mandatory = $true)][int]$Replicas)

	if ($Replicas -ne 1 -and $Replicas -ne 3) {
		throw "共享 Redis 的副本数只能是 1(单实例,redis.yaml)或 3(1 主 2 从 + 哨兵 + 代理,redis-ha.yaml),实际 $Replicas。两个节点的哨兵多数派挂一个就没了,比单实例还脆。"
	}
	$ha = ($Replicas -eq 3)
	return [pscustomobject]@{
		Replicas            = $Replicas
		HighAvailability    = $ha
		Manifest            = if ($ha) { 'redis-ha.yaml' } else { 'redis.yaml' }
		PodManagementPolicy = if ($ha) { 'Parallel' } else { 'OrderedReady' }
	}
}

# 按命令行与档位解析出本次部署的 Redis 拓扑。-RedisReplicas 0(缺省)= 按档位,判据与 Get-KafkaTopology 相同;
# zone 写路径已跟随线上($script:RedisReplicasFollowedLive)时取线上值。
function Get-RedisTopology {
	$replicas = $RedisReplicas
	if ($replicas -le 0 -and $null -ne $script:RedisReplicasFollowedLive) {
		$replicas = [int]$script:RedisReplicasFollowedLive
	}
	if ($replicas -le 0) {
		$replicas = if ($ReleaseProfile -ne 'dev' -or $OpsProfile -ne 'custom') { 3 } else { 1 }
	}
	return Get-RedisTopologyFor -Replicas $replicas
}

# 拓扑 → mapping Redis 拓扑名(纯函数):'single' / 'ha',Get-RouteStoreWaitAOFNumReplicas 据此给 0 / 1。
# 共享 Redis 同时是 mapping Redis(player:zone / player:placement / mapping:route:ready …),没有单独的 mapping 实例。
function Get-MappingRedisTopologyFor {
	param([Parameter(Mandatory = $true)]$Topology)

	if ($Topology.HighAvailability) { return 'ha' }
	return 'single'
}

# 线上带 app=redis 标签、名字是 StatefulSet 形态 redis-<n> 的 Pod 序号(升序)。只在 sts/redis 不存在时有意义:那时这些 Pod 是
# --cascade=orphan 留下的孤儿,仍在服务(包括 1 → 3 切换在 orphan 删除之后 apply 失败的半截状态)。代理 Pod 的标签是 app=redis-proxy,
# 旧 Deployment 的 Pod 名字是 redis-<hash>-<hash>,都不计。DryRun 不连集群,返回空;查询失败一律 throw(查不到不等于不存在,fail-closed)。
function Get-LiveRedisOrphanPodOrdinals {
	param([Parameter(Mandatory = $true)][string]$Namespace)

	if ($DryRun) { return @() }
	$baseArgs = Build-KubectlBaseArgs
	$allArgs = @()
	$allArgs += $baseArgs
	$allArgs += @("get", "pods", "-n", $Namespace, "-l", "app=redis", "--ignore-not-found", "-o", "name")
	$out = & kubectl @allArgs
	if ($LASTEXITCODE -ne 0) {
		throw "查询线上 redis Pod 失败(namespace=$Namespace)。fail-closed:sts/redis 不在时还要确认没有孤儿 Pod 在服务,否则会把半截的切换当成全新集群。"
	}
	$ordinals = [System.Collections.Generic.List[int]]::new()
	foreach ($line in @(([string]($out | Out-String)) -split "`r?`n")) {
		$m = [regex]::Match($line.Trim(), '^(?:pod/)?redis-(\d+)$')
		if ($m.Success) { $ordinals.Add([int]$m.Groups[1].Value) }
	}
	return @($ordinals | Sort-Object)
}

# 孤儿 Pod 序号 → 推算的线上副本数(纯函数):没有 → $null(真的不存在);序号恰好是 0 .. n-1 → n;
# 不连续(中间缺号、只剩 redis-1 之类)→ -1:不知道它经历过什么,交给变更判定当作「本脚本从不产生的数量」拒绝。
function Get-RedisReplicasFromOrphanOrdinals {
	param([AllowNull()][AllowEmptyCollection()][int[]]$Ordinals)

	$list = @($Ordinals | Where-Object { $null -ne $_ } | Sort-Object -Unique)
	if ($list.Count -eq 0) { return $null }
	for ($i = 0; $i -lt $list.Count; $i++) {
		if ($list[$i] -ne $i) { return -1 }
	}
	return $list.Count
}

# 线上共享 Redis 的一次读数:{ Replicas; Source; Ordinals }。
#   Source = 'sts'          Replicas = sts/redis 的 spec.replicas
#   Source = 'orphan-pods'  sts 不在,Replicas 按孤儿 redis-<n> Pod 推算(Get-RedisReplicasFromOrphanOrdinals,缺号 = -1),Ordinals = 孤儿序号
#   Source = 'none'         两样都没有(全新集群),Replicas = $null;DryRun 不连集群,也返回这一种
# 拓扑变更闸与 zone 写路径的跟随线上共用这一处、只读一次:判定用的副本数与拒绝文本里点名的 Pod 出自同一次读取。
# 半截切换(orphan 删除之后 apply 失败)不会被当成全新集群。
function Get-LiveRedisReplicas {
	$live = Get-LiveStatefulSetReplicas -Namespace $InfraNamespace -Name 'redis'
	if ($null -ne $live) { return [pscustomobject]@{ Replicas = [int]$live; Source = 'sts'; Ordinals = @() } }
	if ($DryRun) { return [pscustomobject]@{ Replicas = $null; Source = 'none'; Ordinals = @() } }
	$ordinals = @(Get-LiveRedisOrphanPodOrdinals -Namespace $InfraNamespace)
	if ($ordinals.Count -eq 0) { return [pscustomobject]@{ Replicas = $null; Source = 'none'; Ordinals = @() } }
	return [pscustomobject]@{ Replicas = (Get-RedisReplicasFromOrphanOrdinals -Ordinals $ordinals); Source = 'orphan-pods'; Ordinals = $ordinals }
}

# 线上读数(Get-LiveRedisReplicas)→ 拒绝文本里描述线上的那半句(纯函数)。sts 不在时不说「sts/redis 有 N 个副本」,点名孤儿 Pod。
function Format-RedisLiveShape {
	param([Parameter(Mandatory = $true)]$Reading)

	switch ([string]$Reading.Source) {
		'sts' { return "statefulset/redis 有 $($Reading.Replicas) 个副本" }
		'orphan-pods' {
			$pods = (@($Reading.Ordinals) | ForEach-Object { "redis-$_" }) -join ', '
			$tail = if ([int]$Reading.Replicas -eq -1) { '序号不连续,推算不出形态' } else { "按序号推算为 $($Reading.Replicas) 个" }
			return "没有 statefulset/redis,但带 app=redis 的孤儿 Pod $pods 仍在(可能含 Terminating 中的;$tail)"
		}
		default { return '没有 statefulset/redis,也没有孤儿 redis Pod' }
	}
}

# 线上形态不是本脚本能接手的样子时(副本数不是 1 / 3:sts 上 0 / 2,孤儿推算出 2 或缺号 -1),拒绝文本里的处置(纯函数)。
# 这时不能建议「传 -RedisReplicas <线上值>」:参数只认 0 / 1 / 3(0 = 按档位),zone-up 与 infra-up 都会拒绝,只能先人工整理。
function Get-RedisLiveShapeManualFix {
	return ("不要靠参数绕过:-RedisReplicas 只认 0 / 1 / 3,zone-up 与 infra-up 都会拒绝这种线上形态。在维护窗口里停写,用哨兵(SENTINEL get-master-addr-by-name)或 " +
		"INFO replication 确认最新数据在哪个 Pod,把主切到要保留的序号,再删掉多余的 Pod 与它们的 PVC,整理成只剩 redis-0(单实例)或 redis-0..2 齐全(高可用);" +
		"sts 上是 0 / 2 个副本时先手工 kubectl scale 回 1 或 3。步骤见 deploy/k8s/README.md「共享 Redis:高可用形态 → 切换与回退」。")
}

# 线上 sts/redis 的副本数 → 目标副本数,这次变更算哪一类(纯函数,契约测试直接喂值;写法同 Get-KafkaTopologyChangeVerdict):
#   absent     线上没有 sts/redis(全新集群)
#   unchanged  副本数不变
#   upgrade    1 → 3:单实例原地切高可用。redis-0 带着 PVC 上的数据以主身份回来,redis-1 / redis-2 新建后从它全量同步
#   refuse     其余一律拒绝 —— 3 → 1(主可能在 redis-1 / redis-2 上,缩到只剩 redis-0 = 丢掉主上的最新数据),
#              以及线上是 0 / 2 这类本脚本从不产生的数量(不知道它经历过什么,不猜)
function Get-RedisTopologyChangeVerdict {
	param(
		[AllowNull()]$LiveReplicas,
		[Parameter(Mandatory = $true)][int]$DesiredReplicas
	)

	if ($null -eq $LiveReplicas) { return 'absent' }
	$live = [int]$LiveReplicas
	if ($live -eq $DesiredReplicas) { return 'unchanged' }
	if ($live -eq 1 -and $DesiredReplicas -eq 3) { return 'upgrade' }
	return 'refuse'
}

# apply 共享 Redis 之前调用(Apply-Infra 里,早于任何写操作)。拒绝的变更直接 throw;返回判定,upgrade 时 Apply-Infra 要先
# 「只删 StatefulSet 对象、留下 Pod 与 PVC」(podManagementPolicy 创建后不可改,见 Apply-Infra 的 redis 分支)。
function Assert-RedisTopologyChangeIsSafe {
	param([Parameter(Mandatory = $true)]$Topology)

	# sts/redis 不在时按孤儿 Pod 推算(Get-LiveRedisReplicas):1 → 3 切换在 orphan 删除之后失败、重跑时仍判 upgrade(orphan 删除带
	# --ignore-not-found,再做一次是空操作);只剩 redis-0 而目标 1(README 回退步骤)判 unchanged;3 个孤儿而目标 1 照样拒绝。
	$reading = Get-LiveRedisReplicas
	$live = $reading.Replicas
	$verdict = Get-RedisTopologyChangeVerdict -LiveReplicas $live -DesiredReplicas $Topology.Replicas
	if ($verdict -eq 'upgrade') {
		Write-Warning "[RedisTopology]Upgrade:shared Redis 1 -> 3 (single instance -> primary + 2 replicas + sentinels + 2 proxies). redis-0 restarts with its data and becomes the primary; redis-1 / redis-2 full-sync from it; Service redis switches to the proxies. Logins in all zones are interrupted while redis-0 restarts (maintenance window, needs -ConfirmRedisCoreRestart). Afterwards re-run zone-up for every zone and rollout restart deploy/db and deploy/data-service so WAITAOF waits for 1 replica. To keep the single instance this time, re-run with -RedisReplicas 1."
	}
	if ($verdict -ne 'refuse') { return $verdict }
	$advice = if ($live -ne 1 -and $live -ne 3) {
		Get-RedisLiveShapeManualFix
	}
	elseif ($reading.Source -eq 'orphan-pods') {
		# 只可能是 3 个连续孤儿而目标 1:多半是正在按 README 回退单实例,第 2 步还没删干净。
		"若正在按 README「高可用 → 单实例」回退:第 2 步还没删干净 pod/redis-1、pod/redis-2 与它们的 PVC(redis-data-redis-1 / -2),删完并等 Terminating 结束(kubectl -n $InfraNamespace get pods -l app=redis 只剩 redis-0)后重跑。"
	}
	else {
		"确实要回到单实例:先在维护窗口里把主切回 redis-0(redis-cli -p 26379 SENTINEL FAILOVER mymaster,直到哨兵答 redis-0),停写后按 deploy/k8s/README.md「共享 Redis:高可用形态」的回退步骤做;只是这次不想改形态,显式传 -RedisReplicas $live。"
	}
	throw @"
拒绝变更共享 Redis 的形态:线上$(Format-RedisLiveShape -Reading $reading) → 目标 $($Topology.Replicas) 个(namespace=$InfraNamespace)。
只支持 1 → 3(单实例原地切高可用)。3 → 1 不能原地做:主可能在 redis-1 / redis-2 上,缩到只剩 redis-0 会丢掉主上的最新数据;
线上是 0 / 2 这类本脚本从不产生的副本数、或孤儿 Pod 序号不连续时,不知道它经历过什么,也不猜。本次未做任何写操作。
$advice
"@
}

# zone 写路径(不经 Apply-Infra)的拓扑取哪个值(纯函数,契约测试直接喂值):
#   resolved  线上既没有 sts/redis 也没有孤儿 redis-<n> Pod(infra 还没部署,或只剩 2026-09-19 之前的旧 Deployment —— 那种集群要先 infra-up 迁移,
#             zone 本来就起不来,不替 infra-up 做判断),或线上与本次解析值一致:照本次解析值
#   live      没给 -RedisReplicas(RequestedReplicas ≤ 0),线上是 1 或 3 且与档位缺省不同:跟随线上
#   refuse    显式给了且与线上不同,或线上是 0 / 2 这类本脚本从不产生的副本数(不知道它经历过什么,不猜)
function Get-RedisZoneTopologyDecision {
	param(
		[AllowNull()]$LiveReplicas,
		[Parameter(Mandatory = $true)][int]$RequestedReplicas,
		[Parameter(Mandatory = $true)][int]$ResolvedReplicas
	)

	if ($null -eq $LiveReplicas) { return 'resolved' }
	$live = [int]$LiveReplicas
	if ($live -eq $ResolvedReplicas) { return 'resolved' }
	if ($RequestedReplicas -le 0 -and ($live -eq 1 -or $live -eq 3)) { return 'live' }
	return 'refuse'
}

# 不经 Apply-Infra 的写路径(zone-up、all-up -SkipInfra)部署 zone Go 服务时调用,早于任何写操作:WAITAOF 副本份数按拓扑渲染进
# data-service / go/db 的 ConfigMap(Get-RouteStoreWaitAOFNumReplicas),必须与线上共享 Redis 一致:
#   - 线上高可用、按单实例渲染(写 0):主崩溃切主时,已答复成功的映射提交与入队可能只在旧主上 —— 静默降级;
#   - 线上单实例、按高可用渲染(写 1):没有副本可等,go/db 启动探针拒启、data-service 的 WAITAOF 全部超时。
# zone 发布不改 infra 的形态,所以没给 -RedisReplicas 时跟随线上(staging / prod 还在单实例上时,zone-up、zone 回滚的 Step 6 照常可用);
# 显式给了而与线上不同即拒绝(那是参数写错,或想切形态却没先跑 infra-up)。DryRun 不查集群,按本次参数与档位渲染。
function Resolve-RedisTopologyFromLive {
	$resolved = Get-RedisTopology
	$reading = Get-LiveRedisReplicas
	$live = $reading.Replicas
	$decision = Get-RedisZoneTopologyDecision -LiveReplicas $live -RequestedReplicas $RedisReplicas -ResolvedReplicas $resolved.Replicas
	if ($decision -eq 'resolved') { return }
	if ($decision -eq 'live') {
		$script:RedisReplicasFollowedLive = [int]$live
		$followed = Get-RedisTopology
		$source = if ($reading.Source -eq 'orphan-pods') {
			"orphan redis pods $((@($reading.Ordinals) | ForEach-Object { "redis-$_" }) -join ',') (statefulset/redis absent)"
		}
		else { 'statefulset/redis' }
		Write-Host "Redis topology: following live $source replicas=$($followed.Replicas) (profile default was $($resolved.Replicas); -RedisReplicas not given) manifest=$($followed.Manifest) high_availability=$(([string]$followed.HighAvailability).ToLowerInvariant()) waitaof_replicas=$(Get-RouteStoreWaitAOFNumReplicas -Topology (Get-MappingRedisTopology))"
		return
	}
	$advice = if ($live -eq 1 -or $live -eq 3) {
		"去掉 -RedisReplicas(跟随线上)或传 -RedisReplicas $live;要切换形态,先跑 infra-up(1 → 3 见 deploy/k8s/README.md「共享 Redis:高可用形态」)。"
	}
	else { Get-RedisLiveShapeManualFix }
	throw @"
共享 Redis 的形态与线上不一致:线上$(Format-RedisLiveShape -Reading $reading),本次 -RedisReplicas $RedisReplicas(解析为 $($resolved.Replicas))(namespace=$InfraNamespace,命令 $Command)。
这个值决定 data-service / go/db 的 WAITAOF 要几个副本确认(单实例 0、高可用 1),渲染错了要么静默丢掉切主时的已确认写入、要么 go/db 拒启。
本次未做任何写操作。$advice
"@
}

# 高可用形态下口令的额外字符集限制(纯函数)。双引号、反斜杠与控制字符已由 Assert-RedisPasswordYamlSafe 拒绝;
# 这里再拒绝空白与单引号:哨兵把口令**原样、不加引号**写回它自己的配置文件(sentinel auth-pass / sentinel-pass,Redis 自身的行为),
# 下次重启按配置语法读回时,空白会把口令拆成两段、引号会被当成引号 —— 报 "Unbalanced quotes" 起不来(2026-10-08 实测)。
# 报错只点变量名,不回显值。空串放行(dev 档不认证)。
function Assert-RedisPasswordSentinelSafe {
	param([AllowEmptyString()][AllowNull()][string]$Password)

	if ([string]::IsNullOrEmpty($Password)) { return }
	if ($Password -match "[\s']") {
		throw "MMORPG_REDIS_PASSWORD 含空白或单引号:共享 Redis 的高可用形态(-RedisReplicas 3)里,哨兵会把口令原样、不加引号地写回它自己的配置文件,下次重启读不回来。请换一个只含字母、数字与 ._~@%+=:,^- 之类符号的口令。"
	}
}

<#
.SYNOPSIS
	从 redis-ha.yaml 读连接上限并核对:PROXY_MAXCONN × (代理副本数 + maxSurge) + 预留 ≤ MAXCLIENTS,且 MAXCLIENTS 与 redis.yaml 同值。

.DESCRIPTION
	为什么要让代理先到线:Redis 一旦到线,代理对它的健康检查连接也会被拒,连续两次失败就把它判下线、踢掉全部会话,
	客户端一起重连又撞线 —— 小故障被放大成全服断线;代理到线只是新连接排队。所以代理全部副本(含滚动时多出的那个)
	加起来也不能把 Redis 用满,留出的预留给复制、哨兵、代理健康检查、探针与运维 redis-cli。
	MAXCLIENTS 必须与 redis.yaml 相同:Redis 连接总账(Get-RedisConnectionLedgerInputs)只从 redis.yaml 读,两种形态共用一份账。
	纯函数(不读文件):找不到恰好一处取值、Deployment redis-proxy 的 replicas / maxSurge 不是恰好一处、或不等式不成立即 throw。
	-ProxyDeploymentName / -ManifestLabel / -SingleInstanceManifestLabel 缺省是核心(redis-proxy / redis-ha.yaml / redis.yaml,原有调用全部不传,
	报错逐字不变);按用途拆出的 queue 实例传 <实例名>-proxy / redis-purpose-ha.yaml / redis-purpose.yaml(Get-RedisQueueHaLimitsFor),同一套核对。
#>
function Get-RedisHaLimits {
	param(
		[Parameter(Mandatory = $true)][string]$ManifestContent,
		[Parameter(Mandatory = $true)][int]$SingleInstanceMaxClients,
		[string]$ProxyDeploymentName = 'redis-proxy',
		[string]$ManifestLabel = 'redis-ha.yaml',
		[string]$SingleInstanceManifestLabel = 'redis.yaml'
	)

	# 预留名额:复制 2 条、哨兵每个对主 / 对每个从各 2 条、两个代理的健康检查、探针与运维 redis-cli。估计值,取得宽裕。
	$reserved = 1000
	$text = $ManifestContent -replace "`r`n", "`n"
	$readOne = {
		param([string]$Name)
		$found = [regex]::Matches($text, "(?m)^[ \t]*$Name=(\d+)[ \t]*$")
		if ($found.Count -ne 1) {
			throw "${ManifestLabel}:$Name=<数字> 应恰好出现 1 次,实际 $($found.Count) 次(fail-closed:不替你猜连接上限)"
		}
		$value = 0
		if (-not [int]::TryParse($found[0].Groups[1].Value, [ref]$value) -or $value -le 0) {
			throw "${ManifestLabel}:$Name='$($found[0].Groups[1].Value)' 必须是正整数"
		}
		return $value
	}
	$maxClients = & $readOne 'MAXCLIENTS'
	$proxyMaxConn = & $readOne 'PROXY_MAXCONN'

	$proxyNamePattern = '(?m)^  name:[ \t]*' + [regex]::Escape($ProxyDeploymentName) + '[ \t]*$'
	$proxyDocs = @(($text -split '(?m)^---[ \t]*$') | Where-Object { $_ -cmatch '(?m)^kind:[ \t]*Deployment[ \t]*$' -and $_ -cmatch $proxyNamePattern })
	if ($proxyDocs.Count -ne 1) {
		throw "${ManifestLabel}:kind: Deployment、metadata.name 为 $ProxyDeploymentName 的文档应恰好 1 份,实际 $($proxyDocs.Count) 份"
	}
	$replicaMatches = [regex]::Matches($proxyDocs[0], '(?m)^  replicas:[ \t]*(\d+)[ \t]*$')
	$surgeMatches = [regex]::Matches($proxyDocs[0], '(?m)^[ \t]+maxSurge:[ \t]*(\d+)[ \t]*$')
	if ($replicaMatches.Count -ne 1 -or $surgeMatches.Count -ne 1) {
		throw "${ManifestLabel}:Deployment $ProxyDeploymentName 的 spec.replicas 与 maxSurge 应各恰好一处(整数),实际 $($replicaMatches.Count) / $($surgeMatches.Count) 处"
	}
	$proxyReplicas = [int]$replicaMatches[0].Groups[1].Value
	$proxyMaxSurge = [int]$surgeMatches[0].Groups[1].Value

	if ($maxClients -ne $SingleInstanceMaxClients) {
		throw "$ManifestLabel 的 MAXCLIENTS=$maxClients 与 $SingleInstanceManifestLabel 的 $SingleInstanceMaxClients 不同:两种形态共用一份 Redis 连接总账(只从 $SingleInstanceManifestLabel 读),必须同值。"
	}
	$proxyTotal = $proxyMaxConn * ($proxyReplicas + $proxyMaxSurge)
	if ($proxyTotal + $reserved -gt $maxClients) {
		throw "${ManifestLabel}:PROXY_MAXCONN=$proxyMaxConn × (代理副本 $proxyReplicas + maxSurge $proxyMaxSurge) + 预留 $reserved = $($proxyTotal + $reserved) 超过 MAXCLIENTS=$maxClients。代理必须比 Redis 先到线(否则到线时代理的健康检查失败、踢掉全部会话),调小 PROXY_MAXCONN 或在维护窗口调大两份清单的 MAXCLIENTS。"
	}
	return [pscustomobject]@{
		MaxClients    = $maxClients
		ProxyMaxConn  = $proxyMaxConn
		ProxyReplicas = $proxyReplicas
		ProxyMaxSurge = $proxyMaxSurge
		Reserved      = $reserved
		ProxyTotal    = $proxyTotal
	}
}

# 脚本 ConfigMap(redis-ha-scripts)内容的摘要,取 SHA-256 前 16 位(纯函数)。写进代理的 Pod 模板注解:代理只在启动时
# 按脚本生成配置,内容一变模板就变、Deployment 自动滚动。整块文档原样参与(只统一换行),脚本里的注释改了也会滚动 ——
# 代理是无状态的,多滚一次只是客户端重连一次,比漏滚一次(新脚本不生效)便宜。
# -ConfigMapName / -ManifestLabel 缺省是核心(redis-ha-scripts / redis-ha.yaml,原有调用不传);queue 实例传 <实例名>-ha-scripts / redis-purpose-ha.yaml。
function Get-RedisHaScriptsDigest {
	param(
		[Parameter(Mandatory = $true)][string]$ManifestContent,
		[string]$ConfigMapName = 'redis-ha-scripts',
		[string]$ManifestLabel = 'redis-ha.yaml'
	)

	$documents = @(($ManifestContent -replace "`r`n", "`n") -split '(?m)^---[ \t]*$')
	$configMapNamePattern = '(?m)^  name:[ \t]*' + [regex]::Escape($ConfigMapName) + '[ \t]*$'
	$configMaps = @($documents | Where-Object { $_ -cmatch '(?m)^kind:[ \t]*ConfigMap[ \t]*$' -and $_ -cmatch $configMapNamePattern })
	if ($configMaps.Count -ne 1) {
		throw "${ManifestLabel}:kind: ConfigMap、metadata.name 为 $ConfigMapName 的文档应恰好 1 份,实际 $($configMaps.Count) 份"
	}
	$sha256 = [System.Security.Cryptography.SHA256]::Create()
	try {
		$bytes = $sha256.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($configMaps[0].Trim()))
	}
	finally {
		$sha256.Dispose()
	}
	return ((($bytes | ForEach-Object { $_.ToString('x2') }) -join '').Substring(0, 16))
}

# ─────────────────────────────────────────────────────────────────
# 共享 Redis 按用途拆实例(G4a 第 1a 批:queue;docs/design/shared-redis-split-by-purpose.md §25.3、§3.2.1、§4.2~§4.6,冲突处以 §25.3 为准)
# ─────────────────────────────────────────────────────────────────
# 本批只拆排队与限流:login 的 Node.QueueRedis(queue:* / admit:* / admitted:* 与放行锁)和 gateway 的 Bucket4j 限流桶(rl:zone / rl:ip)
# 按 zone 组搬到 redis-queue-g<N>(组 = zones 配置的 redisQueueGroup,排队键名一个都不改);其余全部留在核心 redis —— 会话、锁、位置、
# gate 排空标记 gate:<id>:draining|drained(k8s_gate_drain.ps1 与 scene_manager 的跨 zone 重定向读核心上的那一份)、映射、存盘控制面。
# §4 里 mapping / dbctl / 迁移标记 / 墓碑 / purge 的内容本批不做(§25.3:数据可清空,不迁移;mapping 改直读 TiDB,dbctl 归玩家域线)。
# 地址只在 Get-RedisPurposeTopologyFor 一处算:login 与 gateway 的 ConfigMap 生成器从 Get-RedisPurposeTopology 取。C++ zoneredis、auth、
# go/db、data-service 与其余服务恒连核心,生成器里写的就是核心地址(tests/k8s_redis_split_contract.tests.ps1 钉住拆分时它们不变)。
# 部署闸只有两条(§25.3「由此对第 1a 批的修订」):地址要变必须 -ConfirmRedisSplitSwitch;本 zone 排队开着时拒绝切换。
# queue 实例的形态与核心同一套 -RedisReplicas 档位规则(§25.3 第 18 条子项 1;Get-RedisQueueShapeFor):单实例 redis-purpose.yaml,
# 高可用 redis-purpose-ha.yaml(1 主 2 从 + 哨兵 + 2 个代理,脚本与核心 redis-ha.yaml 逐行同源)。客户端入口两种形态都是同名 Service
# redis-queue-g<N>(高可用时它指向代理),所以地址只由组决定、不随形态变;形态变更另有一道闸(Assert-RedisQueueTopologyChangeIsSafe)。
# 纯函数(契约测试按 AST 抽出直接调用):Get-RedisPurposeTopologyFor / Get-RedisQueueShapeFor / Get-RedisQueueHaObjectNames /
#   Get-RedisQueueHaLimitsFor / Resolve-RedisQueueMemory / ConvertFrom-RedisPurposeLiveYaml / Get-RedisPurposeSwitchVerdict /
#   Format-RedisPurposeLiveArgs / ConvertFrom-RedisQueueInstanceAddress / ConvertFrom-RedisQueueInstanceLive / Get-RedisWipeRefusal /
#   ConvertFrom-RedisWipeProbe。ConvertTo-RedisQueueInstanceManifest 只多读一个脚本级常量(代理脚本摘要的占位名)。
# 读参数与 zones 配置:Get-ZoneRedisQueueGroup / Get-RedisPurposeTopology / Get-RedisQueueGroupsToApply;zones 配置的 redisQueueGroup
#   由 Get-ZonesFromJson 内嵌的 ConvertTo-ZoneRedisQueueGroup 校验(内嵌而不是顶层函数:按 AST 只抽 Get-ZonesFromJson 的既有契约测试照样能跑)。
# 读集群(只读;DryRun 不查):Read-RedisPurposeLiveFacts / Assert-RedisPurposeSwitchIsSafe / Assert-RedisQueueTopologyChangeIsSafe /
#   Read-RedisQueueInstanceLive / Show-RedisSplitStatus;写集群:Apply-RedisQueueInstances / Invoke-RedisWipe。

# 非 DryRun 的 zone 写路径在 -RedisSplit auto 时跟随线上:Assert-RedisPurposeSwitchIsSafe 按 zoneId 记下每个 zone 线上的 'none' / 'queue',
# Get-RedisPurposeTopology 随后按它解析(只在写路径入口写一次;没记 = none)。键是 [int] zoneId。
$script:RedisSplitByZone = @{}
# queue 实例的 PVC 大小。写成常量而不按 maxmemory 推:volumeClaimTemplates 创建后不可改,跟着 -RedisQueueMaxMemory 变的话,
# 调一次容量就会让 apply 被 API server 拒绝。8Gi 与核心 redis.yaml 相同,够几 GB 数据的 AOF 连同重写时的临时文件。
$script:RedisQueueStorage = '8Gi'
# 本次要 apply 的 queue 实例容量(Resolve-RedisQueueMemory 的结果),写路径入口(Assert-RedisPurposePreflight)算一次;$null = 还没算 / 本次不 apply。
$script:RedisQueueMemory = $null
# redis-wipe 只许清这一类 infra namespace(压测专用环境,tidb-phase1-and-launch-readiness.md §3.5)。写死在脚本里、不吃参数:
# 白名单要是能由本次命令行改,调用者想清哪个就能清哪个(§5.9)。
$script:RedisWipeNamespacePattern = '^mmorpg-stress-'
# redis-split-status 里每个 Deployment 的 rollout 等待上限(秒):只看一眼「滚完没有」,不等它滚完。
$script:RedisSplitStatusRolloutTimeoutSeconds = 10

# 按用途拆实例的拓扑(纯函数,写法同 Get-KafkaTopologyFor)。返回 { Split; QueueGroup; QueueSplit; Core; Queue },Core / Queue 各是
# { Service; Host; Port; HostPort }:分开给 Host 与 Port,是因为 gateway 的 spring.data.redis 与 C++ zoneredis 都是 host / port 两个键,
# 而 Go 的 RedisConf.Host 吃 "host:port"。Core 恒为 redis;Queue 在 none 时就是 Core(同一个对象),queue 时是 redis-queue-g<QueueGroup>。
# Queue.Service 是客户端入口:单实例时它直接选 redis Pod,高可用时选同名实例的代理(redis-purpose-ha.yaml),两种形态地址相同,所以这里不看形态。
# 名字不用 Get-RedisTopologyFor / Assert-RedisTopologyChangeIsSafe:那两个已被核心的单实例 / 高可用形态占用(§4.3 的 2026-10-19 更正)。
function Get-RedisPurposeTopologyFor {
	param(
		[Parameter(Mandatory = $true)][ValidateSet('none', 'queue')][string]$Split,
		[Parameter(Mandatory = $true)][string]$InfraNamespace,
		[Parameter(Mandatory = $true)][int]$QueueGroup
	)

	if ($QueueGroup -lt 0) {
		throw "redisQueueGroup 必须是 ≥ 0 的整数,实际 $QueueGroup。"
	}
	$port = 6379
	$core = [pscustomobject]@{ Service = 'redis'; Host = ('redis.{0}' -f $InfraNamespace); Port = $port; HostPort = ('redis.{0}:{1}' -f $InfraNamespace, $port) }
	$queueSplit = ($Split -eq 'queue')
	$queue = $core
	if ($queueSplit) {
		$queueService = 'redis-queue-g{0}' -f $QueueGroup
		$queue = [pscustomobject]@{ Service = $queueService; Host = ('{0}.{1}' -f $queueService, $InfraNamespace); Port = $port; HostPort = ('{0}.{1}:{2}' -f $queueService, $InfraNamespace, $port) }
	}
	return [pscustomobject]@{
		Split      = $Split.ToLowerInvariant()
		QueueGroup = $QueueGroup
		QueueSplit = $queueSplit
		Core       = $core
		Queue      = $queue
	}
}

# queue 实例的形态(纯函数)。-Topology 是核心的 Get-RedisTopology / Get-RedisTopologyFor 的结果:queue 与核心同一套 -RedisReplicas 档位规则
# (§25.3 第 18 条子项 1:dev 单实例,非 dev 高可用),同一次部署里两者形态一致。返回 { Replicas; HighAvailability; Template },
# Template 是 manifests/infra 下的模板文件名。
function Get-RedisQueueShapeFor {
	param([Parameter(Mandatory = $true)]$Topology)

	$ha = [bool]$Topology.HighAvailability
	return [pscustomobject]@{
		Replicas         = [int]$Topology.Replicas
		HighAvailability = $ha
		Template         = if ($ha) { 'redis-purpose-ha.yaml' } else { 'redis-purpose.yaml' }
	}
}

# queue 实例高可用形态里由实例名派生的对象名(纯函数)。与 redis-purpose-ha.yaml 的命名是同一套约定,脚本里只在这一处拼:
# 渲染时核对代理上限与脚本摘要、-WaitReady 等代理、redis-split-status 读线上、拒绝文本里的手工步骤都从这里取。
function Get-RedisQueueHaObjectNames {
	param([Parameter(Mandatory = $true)][string]$InstanceName)

	return [pscustomobject]@{
		Proxy            = "$InstanceName-proxy"
		ProxyPdb         = "$InstanceName-proxy-pdb"
		ScriptsConfigMap = "$InstanceName-ha-scripts"
	}
}

# queue 高可用模板的连接上限核对(纯函数):同核心的 Get-RedisHaLimits,代理名按实例派生;MAXCLIENTS 必须与 redis-purpose.yaml 同值
# (queue 的连接总账只读那一份,两种形态共用)。-ManifestContent 至少替换过实例名(其余占位与上限无关)。
function Get-RedisQueueHaLimitsFor {
	param(
		[Parameter(Mandatory = $true)][string]$ManifestContent,
		[Parameter(Mandatory = $true)][string]$InstanceName,
		[Parameter(Mandatory = $true)][int]$SingleInstanceMaxClients
	)

	return Get-RedisHaLimits -ManifestContent $ManifestContent -SingleInstanceMaxClients $SingleInstanceMaxClients `
		-ProxyDeploymentName (Get-RedisQueueHaObjectNames -InstanceName $InstanceName).Proxy -ManifestLabel 'redis-purpose-ha.yaml' -SingleInstanceManifestLabel 'redis-purpose.yaml'
}

# 一个 zone 在 zones 配置里的 redisQueueGroup;zones 配置缺席或该 zone 不在其中 = 0。按 zoneId 找:zone-up 的 -ZoneName 只决定 namespace,
# 组属于 zone 本身(与 Write-RedisConnectionLedger 合并 zones 配置与 -ZoneId 的口径一致)。
function Get-ZoneRedisQueueGroup {
	param([Parameter(Mandatory = $true)][int]$CurrentZoneId)

	$zonesPath = Resolve-ZonesConfigPath
	# 与 Get-ZonesFromJson / Write-RedisConnectionLedger 同一个判据(Test-Path 不带 -LiteralPath)。
	if (-not (Test-Path $zonesPath)) { return 0 }
	foreach ($zone in (Get-ZonesFromJson -Path $zonesPath)) {
		if ([int]$zone.zoneId -eq $CurrentZoneId) { return [int]$zone.redisQueueGroup }
	}
	return 0
}

# 本次部署某个 zone 的按用途拓扑:-RedisSplit 显式 none / queue 就按它;auto 取写路径入口跟随线上记下的值($script:RedisSplitByZone),
# 没记(DryRun、infra-up、本次不部署 login / gateway)按 none —— 本批任何档位缺省都不拆(§25.3 的 2026-10-23 订正)。
function Get-RedisPurposeTopology {
	param([Parameter(Mandatory = $true)][int]$CurrentZoneId)

	# 没绑定 -RedisSplit(契约测试按 AST 抽出生成器单独渲染时)按 auto 处理。
	$requested = if ([string]::IsNullOrWhiteSpace($RedisSplit)) { 'auto' } else { $RedisSplit }
	$split = 'none'
	if ($requested -ne 'auto') {
		$split = $requested
	}
	elseif ($null -ne $script:RedisSplitByZone -and $script:RedisSplitByZone.ContainsKey($CurrentZoneId)) {
		$split = [string]$script:RedisSplitByZone[$CurrentZoneId]
	}
	return Get-RedisPurposeTopologyFor -Split $split -InfraNamespace $InfraNamespace -QueueGroup (Get-ZoneRedisQueueGroup -CurrentZoneId $CurrentZoneId)
}

# infra-up / 带 infra 的 all-up 要 apply 的 queue 组(升序去重)。只在 -RedisSplit 显式 queue 时非空:auto / none 既不 apply 也不删除已有的
# redis-queue-g<N>(回退时数据还在里面,删不删由人决定,§4.3「不删已有对象」)。组取 zones 配置里出现过的每个 redisQueueGroup;
# zones 配置缺席时取 0(与 zone-up 找不到 zone 时的缺省组一致)。
function Get-RedisQueueGroupsToApply {
	if ($RedisSplit -ne 'queue') { return @() }
	$zonesPath = Resolve-ZonesConfigPath
	if (-not (Test-Path $zonesPath)) { return @(0) }
	$groups = foreach ($zone in (Get-ZonesFromJson -Path $zonesPath)) { [int]$zone.redisQueueGroup }
	return @($groups | Sort-Object -Unique)
}

# queue 实例的 maxmemory 与容器内存(纯函数)。-Value 是 redis-server 的写法 <正整数><kb|mb|gb>(不分大小写;Redis 里 1kb / 1mb / 1gb 是
# 1024 进制,所以容器内存换算成 Ki / Mi / Gi 正好对应)。留空时 dev 档取 128mb;release 档(ReleaseProfile 不是 dev,或 OpsProfile 不是 custom,
# 与 -KafkaBrokers / -RedisReplicas 同一判据)throw:queue 实例是 noeviction,写满即拒写,排队开着时新登录全部失败(§6.3、§7),
# 上线档不替运维猜容量(初值每组 1gb,§25.3 第 19 条)。返回 { MaxMemory(如 1gb); ContainerMemory(= 2 × maxmemory,如 2Gi) }。
function Resolve-RedisQueueMemory {
	param(
		[AllowEmptyString()][AllowNull()][string]$Value,
		[Parameter(Mandatory = $true)][string]$ReleaseProfileName,
		[Parameter(Mandatory = $true)][string]$OpsProfileName
	)

	$text = ([string]$Value).Trim()
	if ($text -eq '') {
		if ($ReleaseProfileName -ne 'dev' -or $OpsProfileName -ne 'custom') {
			throw "-RedisSplit queue 在 release 档(-ReleaseProfile $ReleaseProfileName -OpsProfile $OpsProfileName)要 apply 排队实例,必须显式给 -RedisQueueMaxMemory(例如 1gb,每个 redis-queue-g<N> 的 maxmemory;容器内存按 2 倍渲染)。排队实例是 noeviction,写满即拒写,排队开着时新登录全部失败,不替你猜容量。本次未做任何写操作。"
		}
		$text = '128mb'
	}
	$m = [regex]::Match($text, '^(?<n>[1-9][0-9]{0,6})(?<u>kb|mb|gb)$', [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)
	if (-not $m.Success) {
		throw "-RedisQueueMaxMemory '$text' 不合法:写成 <正整数><kb|mb|gb>,例如 512mb / 1gb(redis-server 的 maxmemory 写法)。本次未做任何写操作。"
	}
	$amount = [long]$m.Groups['n'].Value
	$unit = $m.Groups['u'].Value.ToLowerInvariant()
	$containerUnit = switch ($unit) { 'kb' { 'Ki' } 'mb' { 'Mi' } 'gb' { 'Gi' } }
	return [pscustomobject]@{
		MaxMemory       = ('{0}{1}' -f $amount, $unit)
		ContainerMemory = ('{0}{1}' -f (2 * $amount), $containerUnit)
	}
}

<#
.SYNOPSIS
	线上 login / gateway ConfigMap 里排队 Redis 的现状(纯函数)。-LoginYaml / -GatewayYaml 是 ConfigMap 里那份服务配置的文本,$null = 该 ConfigMap 不存在。

.DESCRIPTION
	返回:
	  LoginPresent / GatewayPresent  两份 ConfigMap 在不在
	  QueueRedisPresent              login 有没有 Node.QueueRedis 段(-RedisSplit auto 跟随线上的判据)
	  LoginCoreHostPort              login 的 Node.RedisClient.Host
	  LoginQueueHostPort             login 实际排队用的地址:Node.QueueRedis.Host,没有就是 Node.RedisClient.Host(整段不写 = 共用核心那个 client)
	  QueueEnabled                   login 的 Queue.Enabled:没写或 false 为 $false,true 为 $true;认不出的写法也按 $true(fail-closed:看不懂不等于关着)
	  GatewayHost                    gateway 的 spring.data.redis.host
	  LiveSplit / LiveQueueGroup     由 login 推出的线上档位('none' / 'queue')与组号(QueueRedis.Host 不是 redis-queue-g<N>.… 形状时为 $null)
#>
function ConvertFrom-RedisPurposeLiveYaml {
	param(
		[AllowNull()]$LoginYaml,
		[AllowNull()]$GatewayYaml
	)

	$loginPresent = ($null -ne $LoginYaml)
	$gatewayPresent = ($null -ne $GatewayYaml)
	$queueRedisPresent = $false
	$loginCoreHostPort = ''
	$loginQueueHostPort = ''
	$queueEnabled = $false
	if ($loginPresent) {
		$scalars = (ConvertFrom-YamlToFlatMap -Text ([string]$LoginYaml)).Scalars
		$queueRedisPresent = $scalars.Contains('Node.QueueRedis')
		if ($scalars.Contains('Node.RedisClient.Host')) { $loginCoreHostPort = ([string]$scalars['Node.RedisClient.Host']).Trim() }
		$loginQueueHostPort = $loginCoreHostPort
		if ($queueRedisPresent) {
			$loginQueueHostPort = ''
			if ($scalars.Contains('Node.QueueRedis.Host')) { $loginQueueHostPort = ([string]$scalars['Node.QueueRedis.Host']).Trim() }
		}
		if ($scalars.Contains('Queue.Enabled')) {
			$enabledText = ([string]$scalars['Queue.Enabled']).Trim()
			$queueEnabled = -not ($enabledText -eq '' -or $enabledText -eq 'false')
		}
	}
	$gatewayHost = ''
	if ($gatewayPresent) {
		$gatewayScalars = (ConvertFrom-YamlToFlatMap -Text ([string]$GatewayYaml)).Scalars
		if ($gatewayScalars.Contains('spring.data.redis.host')) { $gatewayHost = ([string]$gatewayScalars['spring.data.redis.host']).Trim() }
	}
	$liveGroup = $null
	$groupMatch = [regex]::Match($loginQueueHostPort, '^redis-queue-g(?<g>[0-9]+)\.')
	if ($queueRedisPresent -and $groupMatch.Success) { $liveGroup = [int]$groupMatch.Groups['g'].Value }
	return [pscustomobject]@{
		LoginPresent       = $loginPresent
		GatewayPresent     = $gatewayPresent
		QueueRedisPresent  = $queueRedisPresent
		LoginCoreHostPort  = $loginCoreHostPort
		LoginQueueHostPort = $loginQueueHostPort
		QueueEnabled       = $queueEnabled
		GatewayHost        = $gatewayHost
		LiveSplit          = $(if ($queueRedisPresent) { 'queue' } else { 'none' })
		LiveQueueGroup     = $liveGroup
	}
}

# 线上现状对应的参数串(纯函数),只用于拒绝文本里的「照传现状」。线上 QueueRedis.Host 认不出组号时返回 $null。
function Format-RedisPurposeLiveArgs {
	param([Parameter(Mandatory = $true)]$Live)

	if (-not $Live.QueueRedisPresent) { return '-RedisSplit none(或不给,zone 写路径的 auto 跟随线上)' }
	if ($null -eq $Live.LiveQueueGroup) { return $null }
	return "-RedisSplit queue,且 zones 配置里该 zone 的 redisQueueGroup 为 $($Live.LiveQueueGroup)"
}

<#
.SYNOPSIS
	排队 Redis 地址变更判定(纯函数,契约测试直接喂值;写法同 Get-RedisTopologyChangeVerdict)。

.DESCRIPTION
	-Live 是 ConvertFrom-RedisPurposeLiveYaml 的结果,-Desired 是 Get-RedisPurposeTopologyFor 的结果。返回 { Verdict; Changes; QueueEnabled }:
	  absent     本 zone 线上 login 与 gateway 的 ConfigMap 都不存在(新 zone、namespace 刚删):没有可比的现状
	  unchanged  本次会写的那一侧,地址都与线上相同(login 比实际排队用的 host:port,gateway 比 spring.data.redis.host)
	  switch     地址要变,带了 -ConfirmRedisSplitSwitch,且线上排队没开 → 放行(调用方打 [RedisSplit]Confirmed 警告)
	  refuse     地址要变而没带确认;或线上 login 的 Queue.Enabled 为 true —— 确认也不豁免:滚动时新旧副本各连一个实例、两套队列、
	             两份在途占位,本 zone 准入最多翻倍(§3.2.1 约束 2 的 2026-10-15 更正)
	只比本次会写的那一侧:不部署 login(或 gateway)时,它的线上值本次不会被改,不参与比较;Queue.Enabled 总是读线上 login 的。
#>
function Get-RedisPurposeSwitchVerdict {
	param(
		[Parameter(Mandatory = $true)]$Live,
		[Parameter(Mandatory = $true)]$Desired,
		[Parameter(Mandatory = $true)][bool]$DeploysLogin,
		[Parameter(Mandatory = $true)][bool]$DeploysGateway,
		[Parameter(Mandatory = $true)][bool]$Confirmed
	)

	$changes = New-Object System.Collections.Generic.List[string]
	if ($DeploysLogin -and $Live.LoginPresent -and $Live.LoginQueueHostPort -cne $Desired.Queue.HostPort) {
		$liveText = if ($Live.QueueRedisPresent) { "Node.QueueRedis.Host=$($Live.LoginQueueHostPort)" } else { "不写 Node.QueueRedis(与 RedisClient 共用 $($Live.LoginQueueHostPort))" }
		$desiredText = if ($Desired.QueueSplit) { "Node.QueueRedis.Host=$($Desired.Queue.HostPort)" } else { "不写 Node.QueueRedis(与 RedisClient 共用 $($Desired.Core.HostPort))" }
		$changes.Add("login 线上 $liveText,本次 $desiredText")
	}
	if ($DeploysGateway -and $Live.GatewayPresent -and $Live.GatewayHost -cne $Desired.Queue.Host) {
		$changes.Add("gateway spring.data.redis.host 线上 $($Live.GatewayHost),本次 $($Desired.Queue.Host)")
	}

	$verdict = 'refuse'
	if (-not $Live.LoginPresent -and -not $Live.GatewayPresent) {
		$verdict = 'absent'
	}
	elseif ($changes.Count -eq 0) {
		$verdict = 'unchanged'
	}
	elseif ($Confirmed -and -not $Live.QueueEnabled) {
		$verdict = 'switch'
	}
	return [pscustomobject]@{
		Verdict      = $verdict
		Changes      = [string[]]$changes.ToArray()
		QueueEnabled = [bool]$Live.QueueEnabled
	}
}

<#
.SYNOPSIS
	读一个 zone namespace 里 login 与 gateway 的线上 ConfigMap(只读 kubectl),交给 ConvertFrom-RedisPurposeLiveYaml。

.DESCRIPTION
	ConfigMap 不存在 = 没有现状($null)。kubectl 失败、输出不是 JSON、ConfigMap 在却没有服务配置那个数据键,一律 throw:
	读不到不等于没拆(fail-closed,同 Read-LiveRouteSwitchFacts)。Invoke-ClientEntryKubectl 不看 $DryRun,DryRun 判断由调用方负责。
#>
function Read-RedisPurposeLiveFacts {
	param([Parameter(Mandatory = $true)][string]$Namespace)

	$failClosed = '判不出排队 Redis 的线上现状,拒绝继续;尚未做任何写操作。'
	$readConfigText = {
		param([string]$ConfigMapName, [string]$DataKey)
		$source = "namespace $Namespace 的 ConfigMap $ConfigMapName"
		$result = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'configmap', $ConfigMapName, '-n', $Namespace, '--ignore-not-found', '-o', 'json') `
			-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
		if ($result.ExitCode -ne 0) {
			throw "读取线上 $source 失败(kubectl 退出码 $($result.ExitCode)):$($result.Stderr)。fail-closed:读不到不等于没拆,$failClosed"
		}
		$text = ([string]$result.Stdout).Trim()
		if ($text -eq '') { return $null }
		try { $configMap = $text | ConvertFrom-Json -ErrorAction Stop }
		catch { throw "线上 $source 的 kubectl 输出不是合法 JSON,$failClosed$($_.Exception.Message)" }
		$property = $null
		$dataProperty = $configMap.PSObject.Properties['data']
		if ($null -ne $dataProperty -and $null -ne $dataProperty.Value) { $property = $dataProperty.Value.PSObject.Properties[$DataKey] }
		if ($null -eq $property -or [string]::IsNullOrWhiteSpace([string]$property.Value)) {
			throw "线上 $source 存在,但没有 data.${DataKey}:不是本脚本生成的形状,$failClosed"
		}
		return [string]$property.Value
	}
	$loginYaml = & $readConfigText $GoSvcCatalogue['login'].ConfigMap $GoSvcCatalogue['login'].ConfigFile
	$gatewayYaml = & $readConfigText $JavaSvcCatalogue['gateway'].ConfigMap 'application.yaml'
	return ConvertFrom-RedisPurposeLiveYaml -LoginYaml $loginYaml -GatewayYaml $gatewayYaml
}

<#
.SYNOPSIS
	排队 Redis 的防静默切换(写路径入口,任何写操作之前):本次要写给 login / gateway 的排队地址与本 zone 线上不同就拒绝,除非带了
	-ConfirmRedisSplitSwitch 且线上排队没开。-RedisSplit auto 时在这里跟随线上(记进 $script:RedisSplitByZone)。

.DESCRIPTION
	只在本次部署 login 或 gateway 时做(zone-up / all-up,Test-ZoneLoginDeployed / Test-ZoneGatewayDeployed);zone 集合与
	Assert-ClientEntryClusterState 同一份(zone-up 一个,all-up 取 zones 配置):all-up 逐 zone 部署,必须在第一个写操作之前把全部 zone 判完,
	不留「前几个 zone 已切过去、后面的才被拒」的半截部署。不一致一次报全。
	DryRun 不查集群:只按本次参数打出每个 zone 将要写的地址,auto 按 none 渲染。
#>
function Assert-RedisPurposeSwitchIsSafe {
	$deploysLogin = [bool](Test-ZoneLoginDeployed)
	$deploysGateway = [bool](Test-ZoneGatewayDeployed)
	if (-not $deploysLogin -and -not $deploysGateway) { return }

	$zones = @()
	if ($Command -eq 'zone-up') {
		$zones = @([pscustomobject]@{ Name = $ZoneName; ZoneId = [int]$ZoneId })
	}
	elseif ($Command -eq 'all-up') {
		$zones = @(foreach ($zone in (Get-ZonesFromJson -Path (Resolve-ZonesConfigPath))) { [pscustomobject]@{ Name = [string]$zone.name; ZoneId = [int]$zone.zoneId } })
	}

	if ($DryRun) {
		foreach ($zone in $zones) {
			$topology = Get-RedisPurposeTopology -CurrentZoneId $zone.ZoneId
			Write-Host "Redis split: zone=$($zone.Name) zone_id=$($zone.ZoneId) requested=$RedisSplit resolved=$($topology.Split) queue_redis=$($topology.Queue.HostPort) core_redis=$($topology.Core.HostPort)"
		}
		Write-Host "[dry-run] Skipping Redis split live check (login / gateway ConfigMaps); -RedisSplit auto renders none. Actual execution follows / checks the live state before any writes."
		return
	}

	$problems = @()
	foreach ($zone in $zones) {
		$namespace = Get-ZoneNamespace -Name $zone.Name
		$live = Read-RedisPurposeLiveFacts -Namespace $namespace
		if ($RedisSplit -eq 'auto') {
			$script:RedisSplitByZone[[int]$zone.ZoneId] = $live.LiveSplit
		}
		$desired = Get-RedisPurposeTopology -CurrentZoneId $zone.ZoneId
		$result = Get-RedisPurposeSwitchVerdict -Live $live -Desired $desired -DeploysLogin $deploysLogin -DeploysGateway $deploysGateway `
			-Confirmed ([bool]$ConfirmRedisSplitSwitch)
		Write-Host "Redis split: zone=$($zone.Name) zone_id=$($zone.ZoneId) requested=$RedisSplit resolved=$($desired.Split) queue_redis=$($desired.Queue.HostPort) live_login_queue=$(if ($live.LoginPresent) { $live.LoginQueueHostPort } else { '<absent>' }) live_gateway=$(if ($live.GatewayPresent) { $live.GatewayHost } else { '<absent>' }) live_queue_enabled=$(([string]$live.QueueEnabled).ToLowerInvariant()) verdict=$($result.Verdict)"
		if ($result.Verdict -eq 'absent' -or $result.Verdict -eq 'unchanged') { continue }
		$changeText = $result.Changes -join ';'
		if ($result.Verdict -eq 'switch') {
			Write-Warning "[RedisSplit]Confirmed:-ConfirmRedisSplitSwitch supplied, zone $($zone.Name) (namespace=$namespace) switches its queue Redis: $changeText. Old and new login / gateway pods talk to different instances until the rollout finishes; do not run k8s_gate_drain.ps1 or roll gates meanwhile, then check with redis-split-status."
			continue
		}
		$keepArgs = Format-RedisPurposeLiveArgs -Live $live
		$keepText = if ($null -eq $keepArgs) { "线上 QueueRedis.Host=$($live.LoginQueueHostPort) 认不出组号,先人工核对该 ConfigMap" } else { "线上现状对应的参数是 $keepArgs" }
		if ($result.QueueEnabled) {
			$problems += "zone $($zone.Name)(namespace=$namespace):$changeText;线上 login 的 Queue.Enabled 为 true:排队开着时切换,新旧 login 副本各连一个实例、两套队列,本 zone 准入最多翻倍(§3.2.1 约束 2),-ConfirmRedisSplitSwitch 不豁免这一条 —— 先以 -LoginQueue off 部署本 zone 关掉排队再切;$keepText"
		}
		else {
			$problems += "zone $($zone.Name)(namespace=$namespace):$changeText;本次没给 -ConfirmRedisSplitSwitch;$keepText"
		}
	}
	if ($problems.Count -gt 0) {
		throw ("排队 Redis 的地址与线上不一致($($problems.Count) 个 zone),未做任何写操作:`n  - " + ($problems -join "`n  - ") +
			"`n只是这次不想改,照传线上现状的参数(漏传 -RedisSplit 就是想切回共享拓扑);确要切换,先确认本 zone 排队关着,再加 -ConfirmRedisSplitSwitch," +
			"切换后用 -Command redis-split-status 确认 login / gateway 已滚完且指向同一实例(deploy/k8s/README.md「共享 Redis 按用途拆实例」)。")
	}
}

# 写路径入口的按用途拆实例预检(任何写操作之前):本次要 apply queue 实例时先定容量(release 档缺 -RedisQueueMaxMemory 在这里拒绝),
# 再做排队地址的防静默切换(Assert-RedisPurposeSwitchIsSafe;auto 在这里跟随线上)。排在 Write-RedisConnectionLedger 之前:总账按解析后的拓扑记。
function Assert-RedisPurposePreflight {
	$deploysInfra = ($Command -eq 'infra-up') -or ($Command -eq 'all-up' -and -not $SkipInfra)
	$queueGroups = @()
	if ($deploysInfra) { $queueGroups = @(Get-RedisQueueGroupsToApply) }
	if ($queueGroups.Count -gt 0) {
		$script:RedisQueueMemory = Resolve-RedisQueueMemory -Value $RedisQueueMaxMemory -ReleaseProfileName $ReleaseProfile -OpsProfileName $OpsProfile
		$queueShape = Get-RedisQueueShapeFor -Topology (Get-RedisTopology)
		Write-Host ("Redis queue instances: {0} maxmemory={1} memory={2} storage={3} replicas={4} high_availability={5} (manifests/infra/{6}, same -RedisReplicas rule as the core Redis)" -f
			(($queueGroups | ForEach-Object { "redis-queue-g$_" }) -join ','), $script:RedisQueueMemory.MaxMemory, $script:RedisQueueMemory.ContainerMemory, $script:RedisQueueStorage,
			$queueShape.Replicas, ([string]$queueShape.HighAvailability).ToLowerInvariant(), $queueShape.Template)
	}
	elseif (-not [string]::IsNullOrWhiteSpace($RedisQueueMaxMemory)) {
		Write-Warning "[RedisSplit]NoEffect:-RedisQueueMaxMemory only takes effect when this run applies queue instances (-RedisSplit queue with infra-up, or all-up without -SkipInfra)."
	}
	if ($RedisSplit -ne 'auto' -and -not $deploysInfra -and -not (Test-ZoneLoginDeployed) -and -not (Test-ZoneGatewayDeployed)) {
		Write-Warning "[RedisSplit]NoEffect:-RedisSplit $RedisSplit only changes login / gateway ConfigMaps (zone-up / all-up with Go / Java services) and the queue instances (infra-up / all-up without -SkipInfra); nothing in this run uses it."
	}
	Assert-RedisPurposeSwitchIsSafe
}

<#
.SYNOPSIS
	渲染一个 queue 实例的清单(不读文件、不连集群)。返回 { Name; Replicas; HighAvailability; Template; MaxMemory; Content; HaLimits }。

.DESCRIPTION
	-Template 是 -Shape.Template 那份模板的原文;替换实例名、容量与 PVC 大小。高可用形态再做两件与核心 redis-ha.yaml 相同的事:
	  - 连接上限核对(Get-RedisQueueHaLimitsFor):代理全部副本(含 maxSurge)+ 预留 ≤ MAXCLIENTS,且 MAXCLIENTS 与 redis-purpose.yaml 同值,不成立即 throw;
	  - 脚本 ConfigMap 的摘要写进代理的 Pod 模板注解(Get-RedisHaScriptsDigest),脚本一改代理就滚动。
	最后过「未替换占位就拒绝」检查(Assert-InfraManifestPlaceholdersReplaced,与核心清单同一份)。
	-SingleInstanceMaxClients(redis-purpose.yaml 的 MAXCLIENTS)只在高可用时用到。
#>
function ConvertTo-RedisQueueInstanceManifest {
	param(
		[Parameter(Mandatory = $true)][string]$Template,
		[Parameter(Mandatory = $true)]$Shape,
		[Parameter(Mandatory = $true)][string]$InstanceName,
		[Parameter(Mandatory = $true)][string]$InfraNamespace,
		[Parameter(Mandatory = $true)]$Memory,
		[Parameter(Mandatory = $true)][string]$Storage,
		[int]$SingleInstanceMaxClients = 0
	)

	$content = $Template.Replace('__INFRA_NAMESPACE__', $InfraNamespace).Replace('__REDIS_NAME__', $InstanceName)
	$content = $content.Replace('__REDIS_MAXMEMORY__', $Memory.MaxMemory).Replace('__REDIS_MEM__', $Memory.ContainerMemory).Replace('__REDIS_STORAGE__', $Storage)
	$haLimits = $null
	if ($Shape.HighAvailability) {
		$haLimits = Get-RedisQueueHaLimitsFor -ManifestContent $content -InstanceName $InstanceName -SingleInstanceMaxClients $SingleInstanceMaxClients
		$scriptsConfigMap = (Get-RedisQueueHaObjectNames -InstanceName $InstanceName).ScriptsConfigMap
		$scriptsDigest = Get-RedisHaScriptsDigest -ManifestContent $content -ConfigMapName $scriptsConfigMap -ManifestLabel $Shape.Template
		$content = $content.Replace($script:RedisHaScriptsDigestPlaceholder, $scriptsDigest)
	}
	Assert-InfraManifestPlaceholdersReplaced -ManifestName "$($Shape.Template)($InstanceName)" -Content $content
	return [pscustomobject]@{
		Name             = $InstanceName
		Replicas         = [int]$Shape.Replicas
		HighAvailability = [bool]$Shape.HighAvailability
		Template         = $Shape.Template
		MaxMemory        = $Memory.MaxMemory
		Content          = $content
		HaLimits         = $haLimits
	}
}

<#
.SYNOPSIS
	queue 实例的形态变更闸(New-RedisQueueInstanceManifests 里,早于任何写操作)。返回 absent / unchanged / upgrade;拒绝的变更 throw。

.DESCRIPTION
	读线上 statefulset/<实例名> 的副本数(Get-LiveStatefulSetReplicas;DryRun 不查,一律当全新),判定复用核心的纯函数 Get-RedisTopologyChangeVerdict:
	  absent / unchanged  放行;
	  upgrade(1 → 3)     打 [RedisQueueTopology]Upgrade 告警后放行。Apply-RedisQueueInstances 先 delete --cascade=orphan 旧 StatefulSet 再 apply
	                       (podManagementPolicy 创建后不可改;做法同核心 1 → 3:<实例名>-0 带着 PVC 回来,另两个从它全量同步,Service 改指向代理);
	  refuse              3 → 1,或线上是 0 / 2 这类本脚本从不产生的副本数:throw,写明手工步骤。
	与核心那道闸的两处差别,都因为排队数据带 TTL、可以丢(redis-purpose.yaml 文件头):
	  - 不要 -ConfirmRedisCoreRestart:1 → 3 只让 <实例名>-0 重启一次,期间只影响本组 zone 的新登录;核心的确认防的是会话与锁;
	  - 不按孤儿 Pod 推算线上:1 → 3 在 orphan 删除之后中断时 StatefulSet 不在,重跑判 absent,apply 出的新 StatefulSet 照样按同名同标签
	    接管孤儿 <实例名>-0 并滚动,结果与 upgrade 相同。
#>
function Assert-RedisQueueTopologyChangeIsSafe {
	param(
		[Parameter(Mandatory = $true)][string]$InstanceName,
		[Parameter(Mandatory = $true)][int]$DesiredReplicas
	)

	$live = Get-LiveStatefulSetReplicas -Namespace $InfraNamespace -Name $InstanceName
	$verdict = Get-RedisTopologyChangeVerdict -LiveReplicas $live -DesiredReplicas $DesiredReplicas
	if ($verdict -eq 'upgrade') {
		Write-Warning "[RedisQueueTopology]Upgrade:queue instance $InstanceName 1 -> 3 (single instance -> primary + 2 replicas + sentinels + 2 proxies). $InstanceName-0 restarts with its data and the other two full-sync from it; Service $InstanceName switches to the proxies (same address: login / gateway need no redeploy). While $InstanceName-0 restarts, new logins in this group's zones fail if the login queue is on. To keep the single instance this time, re-run with -RedisReplicas 1."
	}
	if ($verdict -ne 'refuse') { return $verdict }
	$names = Get-RedisQueueHaObjectNames -InstanceName $InstanceName
	$advice = if ([int]$live -eq 3) {
		"排队数据都带 TTL、可以丢,回到单实例按「删掉重建」做:先以 -LoginQueue off 部署本组各 zone 关掉排队,再 kubectl -n $InfraNamespace delete statefulset/$InstanceName " +
		"deployment/$($names.Proxy) pdb/$($names.ProxyPdb) configmap/$($names.ScriptsConfigMap),等 Pod 退完后删 PVC redis-data-$InstanceName-0 / -1 / -2," +
		"然后带 -RedisReplicas 1 重跑本命令(apply 把 Service $InstanceName 改回指向单实例,名字与 ClusterIP 不变)。只是这次不想改形态,显式传 -RedisReplicas 3。"
	}
	else {
		"线上是本脚本从不产生的副本数:先 kubectl -n $InfraNamespace scale statefulset/$InstanceName --replicas=<与线上形态一致的 1 或 3> 再重跑。"
	}
	throw @"
拒绝变更排队 Redis 实例 $InstanceName 的形态:线上 statefulset/$InstanceName 有 $live 个副本 → 目标 $DesiredReplicas 个(namespace=$InfraNamespace)。
只支持 1 → 3(单实例原地切高可用)。3 → 1 不能原地 apply:podManagementPolicy 创建后不可改,代理、哨兵与脚本对象也不会被 apply 删掉。本次未做任何写操作。
$advice
"@
}

<#
.SYNOPSIS
	渲染本次要 apply 的 queue 实例清单(-RedisSplit queue 的 infra-up / 带 infra 的 all-up)。返回数组,每项是 ConvertTo-RedisQueueInstanceManifest
	的结果加 Verdict(形态变更判定);没有要 apply 的组时为空。

.DESCRIPTION
	形态按 Get-RedisQueueShapeFor(与核心同一个 Get-RedisTopology):单实例 redis-purpose.yaml,高可用 redis-purpose-ha.yaml;模板只读一次。
	Apply-Infra 在任何写操作之前调用它,下面这些都在 Ensure-Namespace 之前 throw:模板缺席(高可用时不悄悄退回单实例)、容量不合法、
	高可用的口令字符集(哨兵限制)与连接上限不等式、模板多了没人替换的占位,以及形态变更闸拒绝的变更(读线上 StatefulSet;DryRun 不查)。
	容量取写路径入口算好的 $script:RedisQueueMemory;没经过入口的调用现场再算一次(release 档缺值照样拒绝)。
#>
function New-RedisQueueInstanceManifests {
	$groups = @(Get-RedisQueueGroupsToApply)
	if ($groups.Count -eq 0) { return @() }
	$shape = Get-RedisQueueShapeFor -Topology (Get-RedisTopology)
	$templatePath = Join-Path $InfraManifestsDir $shape.Template
	if (-not (Test-Path -LiteralPath $templatePath)) {
		throw "-RedisSplit queue 要 apply 排队实例(replicas=$($shape.Replicas)),但找不到 deploy/k8s/manifests/infra/$($shape.Template)(fail-closed:不悄悄退回不拆,也不悄悄退回单实例)。本次未做任何写操作。"
	}
	$template = [string](Get-Content -LiteralPath $templatePath -Raw)
	$memory = $script:RedisQueueMemory
	if ($null -eq $memory) {
		$memory = Resolve-RedisQueueMemory -Value $RedisQueueMaxMemory -ReleaseProfileName $ReleaseProfile -OpsProfileName $OpsProfile
	}
	$singleInstanceMaxClients = 0
	if ($shape.HighAvailability) {
		# 与核心高可用同一条口令限制(哨兵把口令原样写回自己的配置文件,同一个 Secret redis-auth)。核心那道已在 Apply-Infra 开头做过,
		# 这里再做一次,不依赖两处的调用顺序。
		Assert-RedisPasswordSentinelSafe -Password $script:RedisPassword
		$singleInstanceMaxClients = Get-RedisMaxClientsFromManifest -RelativePath 'deploy/k8s/manifests/infra/redis-purpose.yaml'
	}
	$manifests = @()
	foreach ($group in $groups) {
		$topology = Get-RedisPurposeTopologyFor -Split 'queue' -InfraNamespace $InfraNamespace -QueueGroup $group
		$manifest = ConvertTo-RedisQueueInstanceManifest -Template $template -Shape $shape -InstanceName $topology.Queue.Service -InfraNamespace $InfraNamespace `
			-Memory $memory -Storage $script:RedisQueueStorage -SingleInstanceMaxClients $singleInstanceMaxClients
		$verdict = Assert-RedisQueueTopologyChangeIsSafe -InstanceName $manifest.Name -DesiredReplicas $manifest.Replicas
		$manifests += ($manifest | Add-Member -NotePropertyName Verdict -NotePropertyValue $verdict -PassThru)
	}
	return $manifests
}

# apply 渲染好的 queue 实例。Apply-Infra 在全部 infra 清单之后调用,也就在核心 Redis 之后:口令 Secret redis-auth 已在核心分支按
# MMORPG_REDIS_PASSWORD 建好或删掉,queue 实例引用同一个可选 Secret(同一信任域、同一轮换节奏,同 redis-rank.yaml 的做法)。
# 只 apply、不删除:-RedisSplit 不是 queue 时脚本不碰已有的 redis-queue-g<N>。唯一的删除是形态变更闸判 upgrade(1 → 3)时
# 「只删 StatefulSet 对象、留下 Pod 与 PVC」(--cascade=orphan,同 Apply-Infra 里核心 1 → 3 的做法),紧接着 apply 的新 StatefulSet
# 按同名同标签接管 <实例名>-0 并滚动成高可用模板。
function Apply-RedisQueueInstances {
	param([AllowEmptyCollection()][object[]]$Manifests = @())

	foreach ($manifest in $Manifests) {
		if ($manifest.Verdict -eq 'upgrade') {
			Invoke-Kubectl -Args @("delete", "statefulset", $manifest.Name, "-n", $InfraNamespace, "--cascade=orphan", "--ignore-not-found")
		}
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $InfraNamespace) -InputContent $manifest.Content
		Write-Host "  [applied] Redis queue instance $($manifest.Name) (replicas=$($manifest.Replicas) maxmemory=$($manifest.MaxMemory) maxmemory-policy=noeviction)"
	}
}

# -WaitReady:逐个等 queue 实例就绪。高可用时连代理一起等:客户端连的是同名 Service 后面的代理,redis 节点就绪而代理没起来照样连不上(同核心)。
# 再像核心那样逐个节点核对生效的 maxclients(被容器 nofile 硬限下调时到线之前只有这里能发现;不等只告警;高可用每个节点都核,任何一个从
# 都可能在下一次切主后变成主)。期望值取自 apply 出去的同一份内容,不重新读盘(理由同 Apply-Infra 对核心清单的处理)。
function Wait-RedisQueueInstancesReady {
	param([AllowEmptyCollection()][object[]]$Manifests = @())

	foreach ($manifest in $Manifests) {
		Wait-ForStatefulSetReady -Namespace $InfraNamespace -StatefulSetName $manifest.Name
		if ($manifest.HighAvailability) {
			Wait-ForDeploymentReady -Namespace $InfraNamespace -DeploymentName (Get-RedisQueueHaObjectNames -InstanceName $manifest.Name).Proxy
		}
		$expected = Get-RedisMaxClientsFromManifest -ManifestContent $manifest.Content -RelativePath "deploy/k8s/manifests/infra/$($manifest.Template)"
		Test-RedisLiveMaxClients -ExpectedMaxClients $expected -Nodes $manifest.Replicas -StatefulSetName $manifest.Name
	}
}

# Deployment 的 rollout 是否完成(只读,短超时):'absent'(没有这个 Deployment)/ 'complete' / 'incomplete'(超时或失败:还有旧 ReplicaSet 的 Pod,
# 或新 Pod 没就绪)/ 'error'(查询本身失败)。只看 ConfigMap 不够:ConfigMap 改了不等于旧 Pod 都退场了,旧 Pod 还在往旧实例写(§4.4)。
function Get-RedisSplitRolloutState {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$Deployment
	)

	$found = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'deployment', $Deployment, '-n', $Namespace, '--ignore-not-found', '-o', 'name') `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
	if ($found.ExitCode -ne 0) { return 'error' }
	if ([string]::IsNullOrWhiteSpace([string]$found.Stdout)) { return 'absent' }
	$status = Invoke-ClientEntryKubectl -KubectlArgs @('rollout', 'status', "deployment/$Deployment", '-n', $Namespace, '--timeout', ('{0}s' -f $script:RedisSplitStatusRolloutTimeoutSeconds)) `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
	if ($status.ExitCode -eq 0) { return 'complete' }
	return 'incomplete'
}

<#
.SYNOPSIS
	-Command redis-split-status:对 zones 配置里每个 zone 打印 login 的 RedisClient / QueueRedis Host、gateway 的 spring.data.redis.host,
	以及 login、gateway 的 rollout 是否完成;同一 zone 的 login 与 gateway 指向不同的排队实例,或任一 rollout 没完成 / 读不到,以退出码 1 结束。

.DESCRIPTION
	zone 列表用 Resolve-ZonesConfigPath(支持 -ZonesConfigPath、json 与 yaml),不写死 zones.json。切换 -RedisSplit 之后、
	执行 k8s_gate_drain.ps1 或滚动 gate 之前,以它为闸(§3.2.1 约束 3)。group= 是 zones 配置里的 redisQueueGroup(拆分时该连的组)。
	ConfigMap / Deployment 不存在(zone 没部署 login 或 gateway)只打印 <absent>,不算失败。DryRun 只打印将要读什么。
#>
function Show-RedisSplitStatus {
	$zonesPath = Resolve-ZonesConfigPath
	$zones = Get-ZonesFromJson -Path $zonesPath
	Write-Host "Redis split status: zones_config=$zonesPath infra_namespace=$InfraNamespace"
	if ($DryRun) {
		foreach ($zone in $zones) {
			Write-Host "[dry-run] would read ConfigMaps $($GoSvcCatalogue['login'].ConfigMap) / $($JavaSvcCatalogue['gateway'].ConfigMap) and rollout status of deployment/login / deployment/gateway in namespace $(Get-ZoneNamespace -Name ([string]$zone.name))"
		}
		return
	}
	$failures = New-Object System.Collections.Generic.List[string]
	foreach ($zone in $zones) {
		$zoneName = [string]$zone.name
		$namespace = Get-ZoneNamespace -Name $zoneName
		try {
			$live = Read-RedisPurposeLiveFacts -Namespace $namespace
		}
		catch {
			$failures.Add("zone ${zoneName}:$($_.Exception.Message)")
			Write-Host "  zone=$zoneName namespace=$namespace group=$($zone.redisQueueGroup) ERROR reading ConfigMaps"
			continue
		}
		$loginRollout = Get-RedisSplitRolloutState -Namespace $namespace -Deployment 'login'
		$gatewayRollout = Get-RedisSplitRolloutState -Namespace $namespace -Deployment 'gateway'
		$loginCoreText = if ($live.LoginPresent) { $live.LoginCoreHostPort } else { '<absent>' }
		$loginQueueText = if (-not $live.LoginPresent) { '<absent>' } elseif ($live.QueueRedisPresent) { $live.LoginQueueHostPort } else { '<none: shares RedisClient>' }
		$gatewayText = if ($live.GatewayPresent) { $live.GatewayHost } else { '<absent>' }
		Write-Host "  zone=$zoneName namespace=$namespace group=$($zone.redisQueueGroup) login.RedisClient=$loginCoreText login.QueueRedis=$loginQueueText gateway.redis.host=$gatewayText rollout login=$loginRollout gateway=$gatewayRollout queue_enabled=$(([string]$live.QueueEnabled).ToLowerInvariant())"
		if ($live.LoginPresent -and $live.GatewayPresent) {
			# login 写 host:port,gateway 只写 host(端口另一个键,两边都是 6379):比主机部分。
			$loginQueueHost = $live.LoginQueueHostPort -replace ':[0-9]+$', ''
			if ($loginQueueHost -cne $live.GatewayHost) {
				$failures.Add("zone ${zoneName}:login 排队用 $($live.LoginQueueHostPort),gateway 限流用 $($live.GatewayHost),两者不是同一个实例(上次切换没做完,或只部署了其中一个)")
			}
		}
		foreach ($pair in @(@{ Name = 'login'; State = $loginRollout }, @{ Name = 'gateway'; State = $gatewayRollout })) {
			if ($pair.State -eq 'incomplete' -or $pair.State -eq 'error') {
				$failures.Add("zone ${zoneName}:deployment/$($pair.Name) 的 rollout 状态是 $($pair.State)(还有旧 Pod 连着旧实例,或查询失败)")
			}
		}
	}
	if ($failures.Count -gt 0) {
		Write-Host ("Redis split status: NOT OK ($($failures.Count))`n  - " + ($failures -join "`n  - "))
		exit 1
	}
	Write-Host "Redis split status: OK"
}

# redis-wipe 的准入(纯函数):返回拒绝原因,空串 = 放行。两条都要满足:infra namespace 匹配 -Pattern(调用方传脚本里写死的
# $script:RedisWipeNamespacePattern),且显式给了 -ConfirmWipeAllRedis。
function Get-RedisWipeRefusal {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][bool]$Confirmed,
		[Parameter(Mandatory = $true)][string]$Pattern
	)

	$reasons = @()
	if ($Namespace -cnotmatch $Pattern) {
		$reasons += "infra namespace '$Namespace' 不匹配压测环境的命名规则 $Pattern(规则写死在 k8s_deploy.ps1,不能由参数放宽)"
	}
	if (-not $Confirmed) {
		$reasons += '没给 -ConfirmWipeAllRedis'
	}
	if ($reasons.Count -eq 0) { return '' }
	return ("拒绝 redis-wipe(它会对部署拓扑里每个 Redis 实例执行 FLUSHALL),未做任何操作:" + ($reasons -join ';') + "。它只用于 AGENTS.md §6.2 的压测前清库。")
}

# 一次清库 exec 的输出 → { Role; FlushReply; DbSize; Parsed }(纯函数)。输出由 Invoke-RedisWipe 的 sh 片段逐行打出:role=… / flushall=… / dbsize=…。
# 认不出(缺 role 或 dbsize 不是整数)时 Parsed = $false,调用方按失败处理。
function ConvertFrom-RedisWipeProbe {
	param([AllowEmptyString()][AllowNull()][string]$Output)

	$role = ''
	$flushReply = ''
	$dbSize = -1
	$dbSizeSeen = $false
	foreach ($line in @(([string]$Output) -split "`r?`n")) {
		$trimmed = $line.Trim()
		if ($trimmed.StartsWith('role=')) { $role = $trimmed.Substring(5) }
		elseif ($trimmed.StartsWith('flushall=')) { $flushReply = $trimmed.Substring(9) }
		elseif ($trimmed.StartsWith('dbsize=')) {
			$value = 0
			if ([int]::TryParse($trimmed.Substring(7), [ref]$value)) { $dbSize = $value; $dbSizeSeen = $true }
		}
	}
	return [pscustomobject]@{ Role = $role; FlushReply = $flushReply; DbSize = $dbSize; Parsed = ($role -ne '' -and $dbSizeSeen) }
}

<#
.SYNOPSIS
	-Command redis-wipe:清空部署拓扑里的每个 Redis 实例并逐个核对 DBSIZE=0(压测前清库,AGENTS.md §6.2;设计稿 §5.9 与 §25.3 第 19 条)。

.DESCRIPTION
	实例按 infra namespace 里**实际存在**的 StatefulSet 列,不按本次参数(参数漏写一个用途就会漏清那个实例):
	  redis                核心:单实例 1 个 Pod,高可用 3 个 Pod(清其中的主,从跟着复制)
	  redis-queue-g<N>     按用途拆出的排队实例(本批单实例)
	  redis-match-cluster  match 的 Redis Cluster:逐主节点 FLUSHALL(Cluster 里 FLUSHALL 只清本节点)
	每个 Pod:kubectl exec -c redis 进去,先问 ROLE,是主就 FLUSHALL SYNC,再读 DBSIZE。口令用 Pod 自己的 REDIS_PASSWORD 经 REDISCLI_AUTH
	交给 redis-cli(写法同 k8s_gate_drain.ps1 的 Invoke-GateDrainRedis),不上本机命令行、不进日志;没有口令(dev、match 集群)时不设。
	通过条件:每个实例至少找到一个主,每个主 FLUSHALL 回 OK 且之后 DBSIZE=0;任一不满足以退出码 1 结束。从节点的 DBSIZE 只打印
	(复制有延迟,不作判据)。准入:Get-RedisWipeRefusal(namespace 匹配 ^mmorpg-stress- 且 -ConfirmWipeAllRedis),DryRun 同样先判准入,再只打印计划。
	只清 Redis:MySQL / etcd / Kafka 照 AGENTS.md §6.2 另行处理;只清 Redis 而复用 player_id 时先按 db-write-fencing-mysql.md §9.1 处理计数器。
#>
function Invoke-RedisWipe {
	$refusal = Get-RedisWipeRefusal -Namespace $InfraNamespace -Confirmed ([bool]$ConfirmWipeAllRedis) -Pattern $script:RedisWipeNamespacePattern
	if (-not [string]::IsNullOrEmpty($refusal)) { throw $refusal }
	if ($DryRun) {
		Write-Host "[dry-run] redis-wipe would list StatefulSets redis / redis-queue-g<N> / redis-match-cluster in namespace $InfraNamespace, run FLUSHALL SYNC on every primary (kubectl exec -c redis; password from the pod's own REDIS_PASSWORD via REDISCLI_AUTH) and require DBSIZE=0 on each primary."
		return
	}

	$listed = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'statefulset', '-n', $InfraNamespace, '-o', 'json') `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
	if ($listed.ExitCode -ne 0) {
		throw "列出 namespace $InfraNamespace 的 StatefulSet 失败(kubectl 退出码 $($listed.ExitCode)):$($listed.Stderr)。fail-closed:列不全就会漏清实例,未做任何操作。"
	}
	try { $list = ([string]$listed.Stdout) | ConvertFrom-Json -ErrorAction Stop }
	catch { throw "namespace $InfraNamespace 的 StatefulSet 列表不是合法 JSON,未做任何操作:$($_.Exception.Message)" }
	$targets = @(foreach ($item in @($list.items)) {
			if ($null -eq $item) { continue }
			$name = [string]$item.metadata.name
			if ($name -cne 'redis' -and $name -cnotmatch '^redis-queue-g[0-9]+$' -and $name -cne 'redis-match-cluster') { continue }
			[pscustomobject]@{ Name = $name; Replicas = [int]$item.spec.replicas }
		})
	if ($targets.Count -eq 0) {
		throw "namespace $InfraNamespace 里一个 Redis StatefulSet 都没有(redis / redis-queue-g<N> / redis-match-cluster):目标不对,不当成已清空。"
	}
	Write-Host "Redis wipe: namespace=$InfraNamespace instances=$((@($targets | ForEach-Object { "$($_.Name)x$($_.Replicas)" })) -join ',')"

	# 单引号:$ 一律留给容器里的 sh 展开。FLUSHALL 带 SYNC,返回时已清完(不受 lazyfree-lazy-user-flush 影响),紧接着的 DBSIZE 才有意义。
	$shell = 'if [ -n "$REDIS_PASSWORD" ]; then REDISCLI_AUTH=$REDIS_PASSWORD; export REDISCLI_AUTH; fi; ' +
		'role=$(redis-cli --raw -h 127.0.0.1 -p 6379 role | head -n 1); echo "role=$role"; ' +
		'if [ "$role" = "master" ]; then r=$(redis-cli --raw -h 127.0.0.1 -p 6379 flushall sync); echo "flushall=$r"; [ "$r" = "OK" ] || exit 3; fi; ' +
		'echo "dbsize=$(redis-cli --raw -h 127.0.0.1 -p 6379 dbsize)"'
	$failures = New-Object System.Collections.Generic.List[string]
	foreach ($target in $targets) {
		$primaries = 0
		for ($i = 0; $i -lt $target.Replicas; $i++) {
			$pod = "$($target.Name)-$i"
			$exec = Invoke-ClientEntryKubectl -KubectlArgs @('exec', '-n', $InfraNamespace, "pod/$pod", '-c', 'redis', '--', 'sh', '-c', $shell) `
				-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
			$probe = ConvertFrom-RedisWipeProbe -Output ([string]$exec.Stdout)
			if ($exec.ExitCode -ne 0 -or -not $probe.Parsed) {
				$failures.Add("${pod}:exec 失败或输出认不出(退出码 $($exec.ExitCode),role='$($probe.Role)' flushall='$($probe.FlushReply)'):$($exec.Stderr)")
				continue
			}
			if ($probe.Role -ceq 'master') {
				$primaries++
				Write-Host "  [wiped] $pod role=master flushall=$($probe.FlushReply) dbsize=$($probe.DbSize)"
				if ($probe.DbSize -ne 0) { $failures.Add("${pod}(主):FLUSHALL 之后 DBSIZE=$($probe.DbSize)") }
			}
			else {
				Write-Host "  [checked] $pod role=$($probe.Role) dbsize=$($probe.DbSize) (replica: follows its primary, not a pass criterion)"
			}
		}
		if ($primaries -eq 0) {
			$failures.Add("$($target.Name):$($target.Replicas) 个 Pod 里没有找到主节点,没清到")
		}
	}
	if ($failures.Count -gt 0) {
		Write-Host ("Redis wipe: NOT OK ($($failures.Count))`n  - " + ($failures -join "`n  - "))
		exit 1
	}
	Write-Host "Redis wipe OK: every primary of $($targets.Count) instance(s) answered FLUSHALL OK and DBSIZE=0"
}

# Go micro-service catalogue: name → { configMapName, manifestFile, port, configFlag, configFileName }
#   Global = $true 的条目是**全局池**服务:只在 infra-up / all-up 的基础设施阶段部署到
#   $InfraNamespace 一次,zone-up 跳过(见 Apply-GlobalGoSvcManifests)。
$GoSvcCatalogue = @{
	mail = @{ ConfigMap = "go-svc-mail-config"; Manifest = "mail.yaml"; Port = 50900; ConfigFlag = "-f"; ConfigFile = "mail.yaml"; ImageName = "mmorpg-mail"; Global = $true; MigrateJob = "mail-migrate.yaml" }
	rank = @{ ConfigMap = "go-svc-rank-config"; Manifest = "rank.yaml"; Port = 51100; ConfigFlag = "-f"; ConfigFile = "rank.yaml"; ImageName = "mmorpg-rank"; Global = $true; MigrateJob = "rank-migrate.yaml" }
	db              = @{ ConfigMap = "go-svc-db-config";              Manifest = "db.yaml";              Port = 6000;  ConfigFlag = "-f";              ConfigFile = "db.yaml";                    ImageName = "mmorpg-db" }
	"data-service"  = @{ ConfigMap = "go-svc-data-service-config";    Manifest = "data-service.yaml";    Port = 9000;  ConfigFlag = "-f";              ConfigFile = "data_service.yaml";             ImageName = "mmorpg-data-service"; MigrateJob = "data-service-migrate.yaml" }
	login           = @{ ConfigMap = "go-svc-login-config";           Manifest = "login.yaml";           Port = 50000; ConfigFlag = "-loginService";   ConfigFile = "login.yaml";                  ImageName = "mmorpg-login" }
	"player-locator"= @{ ConfigMap = "go-svc-player-locator-config";  Manifest = "player-locator.yaml";  Port = 50100; ConfigFlag = "-f";              ConfigFile = "player_locator.yaml";           ImageName = "mmorpg-player-locator" }
	"scene-manager" = @{ ConfigMap = "go-svc-scene-manager-config";   Manifest = "scene-manager.yaml";   Port = 60000; ConfigFlag = "-f";              ConfigFile = "scene_manager_service.yaml";    ImageName = "mmorpg-scene-manager" }
	# 回合制战斗匹配:全局池、不分 zone(docs/design/cross-zone-matchmaking.md D1/D10),
	# 与 battle 池同形态。gate 发现它走非 zone-scoped 前缀,所以放 infra namespace 一份即可。
	match           = @{ ConfigMap = "go-svc-match-config";           Manifest = "match.yaml";           Port = 50500; ConfigFlag = "-f";              ConfigFile = "match_service.yaml";            ImageName = "mmorpg-match"; Global = $true; MigrateJob = "match-migrate.yaml" }
	# 全局聊天 chat v1:世界频道全服唯一、私聊 key 不含 zone,表里天然有跨 zone 的行,所以是全局池(Global),
	# 与 match 同形态放 infra namespace 一份。客户端只经 gate → client-rpc-router 到达它:-GateRouterMode 默认 "1" 下可达
	# (K8s 上路由模式整链待事后补验,turn-based §22 D75);以 "0" 回退时「部署得起来但玩家不可达」,见下一条与参数注释。
	chat            = @{ ConfigMap = "go-svc-chat-config";            Manifest = "chat.yaml";            Port = 50700; ConfigFlag = "-f";              ConfigFile = "chat.yaml";                     ImageName = "mmorpg-chat"; Global = $true; MigrateJob = "chat-migrate.yaml" }
	# 客户端 RPC 路由服(契约 zone_contract_v1 §1 路由服部署链):GATE_CLIENT_RPC_ROUTER=1 时 gate 唯一的 gRPC 目标。
	# 全局池(node_util.cpp IsGlobalPoolNodeType 含 ClientRpcRouter),与 match / chat 同放 infra namespace。
	# 端口 50600 与 go/client_rpc_router/etc/client_rpc_router.yaml、go_services.ps1 一致;metrics 9200。
	# manifest 在 deploy/k8s/manifests/go-svc/client-rpc-router.yaml(照 match.yaml:replicas 2 + podAntiAffinity + PDB,
	# 50600/9200,注入 POD_IP);路由服写进 NodeInfo 的是 POD_IP 而不是 ListenOn 的 0.0.0.0(client_rpc_router_service.go advertisedHost)。
	# gate 是否只连路由服由 -GateRouterMode 决定:默认 "1" 下它是 gate 唯一的 gRPC 目标、依赖门等它就绪;以 "0" 回退时 gate 不连它。
	"client-rpc-router" = @{ ConfigMap = "go-svc-client-rpc-router-config"; Manifest = "client-rpc-router.yaml"; Port = 50600; ConfigFlag = "-f"; ConfigFile = "client_rpc_router.yaml"; ImageName = "mmorpg-client-rpc-router"; Global = $true }
	# 聚宝斋 trade(docs/design/jubaozhai-market.md,P1):商品表天然有跨 zone 的行,所以是全局池(Global),与 chat 同放 infra namespace。
	# 客户端只经 gate → client-rpc-router 可达:-GateRouterMode 默认 "1" 下可达;以 "0" 回退时「部署得起来但玩家不可达」,与 chat 同口径。
	# 端口 50800 / metrics 9230 与 go/trade/etc/trade.yaml、go_services.ps1 一致;ImageName 与 go_svc_image.ps1 的 trade 条目一致。
	# 独占库 mmorpg_trade(port-decisions D-14):库由 mysql-init-sql 带入的 deploy/mysql-init/00_init_zone_dbs.sql 预建(建库只登记那一处);
	# 表由 MigrateJob 登记的 trade-migrate Job 建 —— Apply-OneGoSvc 在 ConfigMap 之后、Deployment 之前 delete + apply 它
	# (Apply-GoSvcMigrateJob)。MigrateJob 是可选字段,只有建表服务才写。
	trade           = @{ ConfigMap = "go-svc-trade-config";           Manifest = "trade.yaml";           Port = 50800; ConfigFlag = "-f";              ConfigFile = "trade.yaml";                    ImageName = "mmorpg-trade"; Global = $true; MigrateJob = "trade-migrate.yaml" }
	# 好友 friend(docs/design/friend-port-20260918.md):好友关系天然跨 zone(表里一行的两个 player_id 可能分属不同区),
	# 所以是全局池(Global),与 chat / trade 同放 infra namespace 一份。客户端只经 gate → client-rpc-router 可达;
	# -GateRouterMode 默认 "1" 下可达;以 "0" 回退时「部署得起来但玩家不可达」,与 chat / trade 同口径。
	# 注意本目录的 Global 与 go_services.ps1 $ServiceCatalogue 的字段集**语义不同**:这里的 Global 决定
	# 「infra namespace 部署一份、zone-up 跳过」(Get-GlobalGoSvcNames → Apply-GlobalGoSvcManifests);
	# 那边没有 Global 字段这一说,本地双 zone 仍每 zone 起一份进程。所以 friend 在这里标 Global、在那边不标,不矛盾。
	# 端口 50400 / metrics 9180 与 go/friend/etc/friend.yaml、go_services.ps1 一致;ImageName 与 go_svc_image.ps1 的 friend 条目一致。
	# 独占库 mmorpg_friend(port-decisions D-14):库由 infra-up 的 mysql-init-sql(deploy/mysql-init/00_init_zone_dbs.sql)预建;
	# 表由 MigrateJob 登记的 friend-migrate Job 建(Apply-OneGoSvc 在 ConfigMap 之后、Deployment 之前跑它)。
	friend          = @{ ConfigMap = "go-svc-friend-config";          Manifest = "friend.yaml";          Port = 50400; ConfigFlag = "-f";              ConfigFile = "friend.yaml";                   ImageName = "mmorpg-friend"; Global = $true; MigrateJob = "friend-migrate.yaml" }
	# 玩家持久收件箱 player-inbox(docs/design/player-inbox-service.md §1.4,D3/D14):每 (玩家, 域) 一条有序邮箱,
	# 按全局唯一的 player_id 存,全服一份、多副本,所以是全局池(Global),与 trade / friend 同放 infra namespace。
	# 调用方不是客户端:scene 按 etcd 前缀 PlayerInboxNodeService.rpc 直连(FetchItems / AckItems),运维直连 PlayerInboxAdmin;
	# 与 -GateRouterMode 无关。端口 50950 / metrics 9270 与 go/player_inbox/etc/player_inbox.yaml、go_services.ps1 一致;
	# ImageName 与 go_svc_image.ps1 的 player-inbox 条目一致。
	# 独占库 mmorpg_player_inbox(D-14):库由 infra-up 的 mysql-init-sql(deploy/mysql-init/00_init_zone_dbs.sql)预建;
	# 表由 MigrateJob 登记的 player-inbox-migrate Job 建(Apply-OneGoSvc 在 ConfigMap 之后、Deployment 之前跑它)。
	# 运维口令 PlayerInbox.AdminToken 只经 Secret 进容器(见 $script:PlayerInboxAdminTokenSecretName 处的说明)。
	"player-inbox"  = @{ ConfigMap = "go-svc-player-inbox-config";    Manifest = "player-inbox.yaml";    Port = 50950; ConfigFlag = "-f";              ConfigFile = "player_inbox.yaml";             ImageName = "mmorpg-player-inbox"; Global = $true; MigrateJob = "player-inbox-migrate.yaml" }
	# 帮会 guild(docs/design/guild-phase2/;K8s 上线批 BK8s = 90-consistency.md G-05,2026-10-16):帮会天然跨 zone,是全局池(Global),
	# 与 trade / friend 同放 infra namespace 一份。客户端只经 gate → client-rpc-router 可达(路由服按 GuildNodeService.rpc/ 前缀发现,
	# guild 刻意不写 go-zero 的 Etcd.Key,D-13);data-service 的回档帮会闸按 Service 名 guild.<infra>:50300 直连(GuildInternalRpc)。
	# 端口 50300 / metrics 9220 与 go/guild/etc/guild.yaml、go_services.ps1 一致;ImageName 与 go_svc_image.ps1 的 guild 条目一致。
	# 独占库 mmorpg_guild(D-14):库由 infra-up 的 mysql-init-sql(deploy/mysql-init/00_init_zone_dbs.sql)预建;
	# 表由 MigrateJob 登记的 guild-migrate Job 建(Apply-OneGoSvc 在 ConfigMap 之后、Deployment 之前跑它)。
	guild           = @{ ConfigMap = "go-svc-guild-config";           Manifest = "guild.yaml";           Port = 50300; ConfigFlag = "-f";              ConfigFile = "guild.yaml";                    ImageName = "mmorpg-guild"; Global = $true; MigrateJob = "guild-migrate.yaml" }
	# 邮件 mail(docs/design/mail-system.md;M1d,2026-10-16):邮件按全局唯一的 player_id / 号段 id 存,是全局池(Global),infra namespace 一份。
	# 客户端只经 gate → client-rpc-router 可达(路由服按 MailNodeService.rpc/ 前缀发现,mail 的 go-zero Etcd.Key 留空,D-13);
	# `-GateRouterMode 0` 时部署得起来但玩家够不着(与 chat / friend / trade 同口径)。MailAdmin(发件)由运维直连 Service mail:50900。
	# 端口 50900 / metrics 9240 与 go/mail/etc/mail.yaml、go_services.ps1 一致;ImageName 与 go_svc_image.ps1 的 mail 条目一致。
	# 独占库 mmorpg_mail(D-14):库由 infra-up 的 mysql-init-sql(deploy/mysql-init/00_init_zone_dbs.sql)预建;
	# 表由 MigrateJob 登记的 mail-migrate Job 建(Apply-OneGoSvc 在 ConfigMap 之后、Deployment 之前跑它)。
	# 运维使用 MAIL_PRODUCER_SECRET_ADMIN 的请求签名，密钥由现役 domain-service-secrets 注入。
	# 排行榜 rank(docs/design/leaderboard-system.md §5.4;R2b):榜按全局唯一的 player_id 存,是全局池(Global),infra namespace 一份。
	# 客户端只经 gate → client-rpc-router 可达(路由服按 RankNodeService.rpc/ 前缀发现,rank 的 go-zero Etcd.Key 留空,D-13);
	# `-GateRouterMode 0` 时部署得起来但玩家够不着(与 chat / friend / mail 同口径)。端口与 go/rank/etc/rank.yaml、go_services.ps1 一致;
	# 现役 rank 使用 SQL 权威代次及 rank-migrate，奖励经 mail 发放；不启用历史 Redis 写榜 runtime。
	# 写分只经 match 的评分快照:match 的 -MatchRatingSnapshotPublish 默认 0,不打开时 rank 起得来、榜可读但不更新(上线顺序见设计 §12.6)。
}

# 目录里非全局(= 随 zone 部署)的服务名。两处 zone 循环共用,避免各写一遍过滤条件。
function Get-ZoneScopedGoSvcNames {
	return @($GoSvcCatalogue.Keys | Where-Object { -not $GoSvcCatalogue[$_].Global })
}
function Get-GlobalGoSvcNames {
	return @($GoSvcCatalogue.Keys | Where-Object { $GoSvcCatalogue[$_].Global })
}

# snowflake 槽位本地缓存目录(docs/design/node-id-overhaul-plan-20260908.md §3.5)。
# Go 侧默认值是相对 cwd 的 ../../run/snowflake:容器里 WORKDIR 是 /app,归一化后是 /run/snowflake,
# 能不能写取决于镜像是否以 root 跑、根文件系统是否只读 —— 部署侧不该赌这个。这里显式给一个
# 可写路径,并让 login / scene-manager / match 的 Deployment 与 C++ gate/scene 的 Deployment / Fleet
# 都在同一路径挂 emptyDir。缓存是**每个 Pod 自己的**启动期草稿(etcd 不可达时凭它续用上次的槽位,
# 有效期 2h),不需要跨 Pod 共享、不需要持久卷:Pod 重建就重新申领,与设计一致。
# C++ 通过环境变量 SNOWFLAKE_CACHE_DIR 读同一路径(只有 scene 启用发号槽会写,gate 收到也不写)。
$SnowflakeCacheDir = "/tmp/snowflake"

# data_service 全局库(SnapshotMySQL.DBName)。transaction_log / player_snapshot / rollback_audit_log /
# id_segment 等全局表(清单以 go/data_service/internal/store/schema.go 为准)由 data_service
# 启动期(或 -migrate)建在这里,所以库必须预先存在且账号有 CREATE 权限:infra-up 的 mysql-init-sql ConfigMap 会生成 02_k8s_global_db.sql 预建它并 GRANT 给
# appuser(New-MysqlInitConfigMapYaml)。它是**全集群一份**,不按 zone 拆;与
# go/data_service/etc/data_service.yaml 本地用的 testdb 刻意不同名 —— 生产库不叫 testdb。
$GlobalDbName = "mmorpg_global"

# Java service catalogue
$JavaSvcCatalogue = @{
	auth    = @{ ConfigMap = "java-svc-auth-config";    Manifest = "auth.yaml";    HttpPort = 5555; GrpcPort = 5556; ImageName = "mmorpg-auth" }
	gateway = @{ ConfigMap = "java-svc-gateway-config"; Manifest = "gateway.yaml"; HttpPort = 8081; GrpcPort = 0;    ImageName = "mmorpg-gateway" }
}

# 注意:OpsProfile 的副本数下限只作用于 legacy 单池参数 $SceneReplicas 与全局的 $BattleReplicas。
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
			# battle 是**不分 zone 的全局池**,只部署一份:1 副本时它挂掉 = 全服回合制战斗全停
			# (在打的战斗全部作废,且玩家身上的 battle:lock 要等 InBattleComp.deadline_ms 到期才由
			# scene reaper 解冻)。gate / scene 有下限而它没有,是这份门禁一直漏掉的一项。
			# 0 不抬:参数区约定 0 = 不装配、不删除已部署的池(例如 battle 由别的 infra 统一管),
			# 抬成 2 会违背调用方的显式决定,还会连带要求 battle 票据密钥。
			if ($BattleReplicas -gt 0 -and $BattleReplicas -lt 2) { $script:BattleReplicas = 2 }
		}
		"bare-metal" {
			$script:GateServiceType = "NodePort"
			if ($CentreReplicas -lt 1) { $script:CentreReplicas = 1 }
			if ($GateReplicas -lt 2) { $script:GateReplicas = 2 }
			if ($SceneReplicas -lt 4) { $script:SceneReplicas = 4 }
			# battle 是**不分 zone 的全局池**,只部署一份:1 副本时它挂掉 = 全服回合制战斗全停
			# (在打的战斗全部作废,且玩家身上的 battle:lock 要等 InBattleComp.deadline_ms 到期才由
			# scene reaper 解冻)。gate / scene 有下限而它没有,是这份门禁一直漏掉的一项。
			# 0 不抬,理由同 managed-cloud。
			if ($BattleReplicas -gt 0 -and $BattleReplicas -lt 2) { $script:BattleReplicas = 2 }
		}
		default {
		}
	}
}

# Kafka 拓扑:broker 数决定的全部派生值都在这一个函数里算(docs/design/no-single-node-horizontal-scaling-20261001.md §2)。
# kafka.yaml 的副本数 / 选举组成员表 / broker 默认副本数、kafka-topic-init 的建 topic 副本数、注入给 Go 服务的
# KAFKA_TOPIC_REPLICATION_FACTOR 全部取自这里 —— 多 broker 要同时成立的几件事不允许各算各的。
#
#   controller:前 min(3, N) 个 Pod 兼任,其余只当 broker。选举组固定 3 票:挂 1 个仍有多数派,
#     而且与 broker 总数无关,所以加 broker 不改成员表、不改 Pod 模板。
#   副本数 min(3, N)、min.insync.replicas:N≥3 时为 2(三份里允许一份掉队;不允许只剩一份还照常收写)。
#   PDB minAvailable = N − 1(一次最多自愿驱逐一个),单 broker 为 1(不允许自愿驱逐)。
#   podManagementPolicy:多 broker 必须 Parallel(见 kafka.yaml 该字段旁的注释);单 broker 保持缺省值 OrderedReady,
#     既有的单 broker StatefulSet 不会因为这个不可变字段而 apply 失败。
function Get-KafkaTopologyFor {
	param(
		[Parameter(Mandatory = $true)][int]$Brokers,
		[Parameter(Mandatory = $true)][string]$Namespace
	)

	if ($Brokers -lt 1 -or $Brokers -eq 2) {
		throw "Kafka broker 数必须是 1 或 ≥3(实际 $Brokers)。两个 broker 的 KRaft 选举组挂一个就没有多数派,比单 broker 还脆。"
	}
	$controllers = [Math]::Min(3, $Brokers)
	$voters = @(for ($i = 0; $i -lt $controllers; $i++) {
		"{0}@kafka-{1}.kafka-headless.{2}.svc.cluster.local:9093" -f ($i + 1), $i, $Namespace
	})
	return [pscustomobject]@{
		Brokers                = $Brokers
		ControllerCount        = $controllers
		ControllerQuorumVoters = ($voters -join ',')
		ReplicationFactor      = [Math]::Min(3, $Brokers)
		MinInsyncReplicas      = if ($Brokers -ge 3) { 2 } else { 1 }
		PodManagementPolicy    = if ($Brokers -ge 3) { 'Parallel' } else { 'OrderedReady' }
		PdbMinAvailable        = [Math]::Max(1, $Brokers - 1)
	}
}

# 按命令行与档位解析出本次部署的 Kafka 拓扑。-KafkaBrokers 0(缺省)= 按档位:
# 非 dev 的发布档(staging / prod)或非 custom 的运维档(managed-cloud / bare-metal)默认 3 个 broker,其余 1 个。
# zone-up 与 infra-up 必须用同一套档位参数:zone 里的 Go 服务按这里的副本数建自己的 topic。
function Get-KafkaTopology {
	$brokers = $KafkaBrokers
	if ($brokers -le 0) {
		$brokers = if ($ReleaseProfile -ne 'dev' -or $OpsProfile -ne 'custom') { 3 } else { 1 }
	}
	return Get-KafkaTopologyFor -Brokers $brokers -Namespace $InfraNamespace
}

# 线上 StatefulSet 的 spec.replicas;不存在返回 $null。DryRun 不连集群,一律当不存在。
function Get-LiveStatefulSetReplicas {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$Name
	)

	if ($DryRun) { return $null }

	$baseArgs = Build-KubectlBaseArgs
	$allArgs = @()
	$allArgs += $baseArgs
	$allArgs += @("get", "statefulset", $Name, "-n", $Namespace, "--ignore-not-found", "-o", "jsonpath={.spec.replicas}")
	$out = & kubectl @allArgs
	if ($LASTEXITCODE -ne 0) {
		throw "查询线上 StatefulSet $Name 失败(namespace=$Namespace)。fail-closed:不知道现状就无法判断这次变更是否安全。"
	}
	$text = ([string]($out | Out-String)).Trim()
	if ([string]::IsNullOrEmpty($text)) { return $null }
	$replicas = 0
	if (-not [int]::TryParse($text, [ref]$replicas)) {
		throw "线上 StatefulSet $Name 的 spec.replicas 读出来不是整数:'$text'"
	}
	return $replicas
}

# 线上 broker 数 → 目标 broker 数,这次变更算哪一类(纯函数,契约测试直接喂值):
#   absent     线上没有这个 StatefulSet(全新集群)
#   unchanged  数量不变
#   scale-up   已经是多 broker(≥3),继续加:新 Pod 只当 broker,不动选举组
#   refuse     其余一律拒绝 —— 1 ↔ ≥3(选举组成员表变了,见 kafka.yaml 文件头)、缩容(被摘掉的 broker 上还有分区副本)、
#              以及线上是 0 / 2 这类本脚本从不产生的数量(不知道它经历过什么,不猜)
function Get-KafkaTopologyChangeVerdict {
	param(
		[AllowNull()]$LiveReplicas,
		[Parameter(Mandatory = $true)][int]$DesiredBrokers
	)

	if ($null -eq $LiveReplicas) { return 'absent' }
	$live = [int]$LiveReplicas
	if ($live -eq $DesiredBrokers) { return 'unchanged' }
	if ($live -ge 3 -and $DesiredBrokers -gt $live) { return 'scale-up' }
	return 'refuse'
}

# apply kafka.yaml 之前调用:会改掉选举组或摘掉 broker 的变更直接拒绝,不交给 kubectl 去"试试看"。
function Assert-KafkaTopologyChangeIsSafe {
	param([Parameter(Mandatory = $true)]$Topology)

	$live = Get-LiveStatefulSetReplicas -Namespace $InfraNamespace -Name 'kafka'
	$verdict = Get-KafkaTopologyChangeVerdict -LiveReplicas $live -DesiredBrokers $Topology.Brokers
	if ($verdict -eq 'absent' -or $verdict -eq 'unchanged') { return }
	if ($verdict -eq 'scale-up') {
		Write-Host "Kafka broker expansion: $live -> $($Topology.Brokers). New brokers do not automatically take over existing topic partitions; move replicas with kafka-reassign-partitions.sh (README section Kafka: multiple brokers)."
		return
	}
	throw @"
拒绝变更 Kafka broker 数:线上 $live 个 → 目标 $($Topology.Brokers) 个(namespace=$InfraNamespace)。
这类变更不能原地做:1 ↔ ≥3 会改掉 KRaft 选举组的成员表,两个空白的新节点可以先凑成多数派、用一份空的元数据日志当选,
原有 broker 回来时被截断,全部 topic 元数据消失;缩容则会摘掉仍持有分区副本的 broker。
要切换,按 deploy/k8s/README.md「Kafka:多 broker」的重建流程做(停写 → 删 StatefulSet 与 PVC → 以新 broker 数 infra-up → 重跑 infra-kafka-topics)。
若只是这次不想改 broker 数,显式传 -KafkaBrokers $live。
"@
}

# Kafka 保留期的**唯一一条规则**(routing-identity-audit-20260908.md R11):
#
#   任何"消费者可能落后"的 topic,保留期必须 > 消费者可能落后的最长时间,并留足余量。
#
# 落后多久算封顶,取决于消费者:
#   * C++ 的 gate / scene 消费者把 max.poll.interval.ms 钉在 **900000**(15 分钟,
#     cpp/libs/engine/infra/messaging/kafka/kafka_consumer.cpp)。也就是说 broker 眼里
#     一个消费者"合法地"消失 15 分钟仍然算活着,回来时必须还能读到那 15 分钟的消息。
#     保留期 < 900s 时它读不到的不是"旧数据",是 BindSession / RoutePlayer / KickPlayer,
#     而且 Kafka 一个错都不报 —— 表现是玩家卡在登录或进世界。
#   * db_task 那条链的封顶不是 poll 间隔而是 **MySQL 故障时长**:消费者停多久,
#     积压就要留多久,否则丢的是玩家存档。
#
# 所以 $KafkaBrokerRetentionMs(broker 默认值,给所有没有自己 retention.ms 覆盖的 topic:
# 迁移窗口里 auto-create 出来的 per-node topic gate-<id> / scene-<id>、game-events)
# 一律 ≥ 2 × 900s;$KafkaDbTaskRetentionMs 按"能容忍多长的 DB 故障"给,不是按 poll 间隔给。
# 有自己契约的 topic 不吃这两个值:审计 topic 30 天(data_service.yaml)、
# 控制面命令 topic 1 小时(kafka-topic-init)、match-results 7 天(match 侧常量)、
# match-rating-snapshot 7 天(match_service.yaml 的 RatingSnapshotRetentionHours,只由 match 声明)。
function Apply-KafkaProfileDefaults {
	switch ($KafkaProfile) {
		"dev" {
			# 30 分钟 = 2 × max.poll.interval.ms。dev 也不能低于这条线:R11 的症状
			# (落后的 gate 丢命令)在 dev 上同样会发生,只是更难归因。
			if ($KafkaBrokerRetentionMs -le 0) { $script:KafkaBrokerRetentionMs = 1800000 }
			# 1 小时:dev 允许 MySQL 挂一小时而不丢存档,再长没必要占盘。
			if ($KafkaDbTaskRetentionMs -le 0) { $script:KafkaDbTaskRetentionMs = 3600000 }
			if ($KafkaRetentionCheckIntervalMs -le 0) { $script:KafkaRetentionCheckIntervalMs = 120000 }
			if ($KafkaRetentionBytes -le 0) { $script:KafkaRetentionBytes = 134217728 }
			if ($KafkaSegmentBytes -le 0) { $script:KafkaSegmentBytes = 16777216 }
			if ([string]::IsNullOrWhiteSpace($KafkaHeapOpts)) { $script:KafkaHeapOpts = "-Xms128m -Xmx256m" }
			# Kafka 盘预算(S5b,负责人 2026-10-13 拍板):每 zone 256 KiB/s、缓冲目标 1 小时、其它 topic 预留 1 GiB。
			# 2 个 zone 时 target retention.bytes = 150994944,PVC 取下限 20Gi(Get-KafkaDiskPlan)。
			if ($KafkaDbTaskDesignBytesPerSec -le 0) { $script:KafkaDbTaskDesignBytesPerSec = 262144 }
			if ($KafkaDbTaskBufferTargetMs -le 0) { $script:KafkaDbTaskBufferTargetMs = 3600000 }
			if ($KafkaOtherTopicsReserveGi -le 0) { $script:KafkaOtherTopicsReserveGi = 1 }
		}
		"prod-like" {
			# 1 小时 = 4 × max.poll.interval.ms。
			if ($KafkaBrokerRetentionMs -le 0) { $script:KafkaBrokerRetentionMs = 3600000 }
			# 6 小时:一次有人值守的 MySQL 故障处理窗口。
			if ($KafkaDbTaskRetentionMs -le 0) { $script:KafkaDbTaskRetentionMs = 21600000 }
			if ($KafkaRetentionCheckIntervalMs -le 0) { $script:KafkaRetentionCheckIntervalMs = 120000 }
			if ($KafkaRetentionBytes -le 0) { $script:KafkaRetentionBytes = 536870912 }
			if ($KafkaSegmentBytes -le 0) { $script:KafkaSegmentBytes = 33554432 }
			if ([string]::IsNullOrWhiteSpace($KafkaHeapOpts)) { $script:KafkaHeapOpts = "-Xms512m -Xmx1g" }
			# Kafka 盘预算(S5b):每 zone 1 MiB/s、缓冲目标 2 小时、其它 topic 预留 2 GiB。
			# 2 个 zone 时 target retention.bytes = 973078528,PVC 40Gi。retention.bytes 按 2 小时算,先于 6 小时的 retention.ms 生效。
			if ($KafkaDbTaskDesignBytesPerSec -le 0) { $script:KafkaDbTaskDesignBytesPerSec = 1048576 }
			if ($KafkaDbTaskBufferTargetMs -le 0) { $script:KafkaDbTaskBufferTargetMs = 7200000 }
			if ($KafkaOtherTopicsReserveGi -le 0) { $script:KafkaOtherTopicsReserveGi = 2 }
		}
		default {
			if ($KafkaBrokerRetentionMs -le 0) { $script:KafkaBrokerRetentionMs = 3600000 }
			# 24 小时:与 go/db/etc/db.yaml、go/login/etc/login.yaml 的 RetentionMs 默认值(86400000)同值。
			# login 与 db 都会在启动时对同一个 db_task_zone_<N> 执行 IncrementalAlterConfigs(kafkautil.EnsureTopics),
			# 两边的值不同,topic 的保留期就**随两个服务的重启顺序反复横跳**。以前这里是 900000、db 的 ConfigMap 又不写
			# RetentionMs(走 Go 结构体默认的 24h),于是在 15 分钟与 24 小时之间横跳;把本档改成 24h 只消掉了 prod 档的横跳,
			# dev / prod-like 档照样在 1h / 6h 与 24h 之间来回(B-15)。现在 db 与 login 两个 ConfigMap 的 RetentionMs 与
			# RetentionBytes 都从 New-GoSvcConfigMapYaml 里同一个 $dbTaskRetentionMs / $dbTaskRetentionBytes 渲染,所有档位两边同值,
			# 契约测试断言两个 ConfigMap 相等。真要改保留期只改这里(或 -KafkaDbTaskRetentionMs),不要只改一个服务的 yaml。
			if ($KafkaDbTaskRetentionMs -le 0) { $script:KafkaDbTaskRetentionMs = 86400000 }
			if ($KafkaRetentionCheckIntervalMs -le 0) { $script:KafkaRetentionCheckIntervalMs = 120000 }
			if ($KafkaRetentionBytes -le 0) { $script:KafkaRetentionBytes = 536870912 }
			if ($KafkaSegmentBytes -le 0) { $script:KafkaSegmentBytes = 33554432 }
			if ([string]::IsNullOrWhiteSpace($KafkaHeapOpts)) { $script:KafkaHeapOpts = "-Xms512m -Xmx1g" }
			# Kafka 盘预算(S5b):每 zone 3 MiB/s、缓冲目标 2 小时(全局单点交接要求 db_task 缓冲 ≥ 2h)、其它 topic 预留 4 GiB。
			# 2 个 zone 时 target retention.bytes = 2785017856,PVC 80Gi;retention.bytes 按 2 小时算,先于 24 小时的 retention.ms 生效,
			# 账本行会就此告警(BufferBelowRetention)。
			if ($KafkaDbTaskDesignBytesPerSec -le 0) { $script:KafkaDbTaskDesignBytesPerSec = 3145728 }
			if ($KafkaDbTaskBufferTargetMs -le 0) { $script:KafkaDbTaskBufferTargetMs = 7200000 }
			if ($KafkaOtherTopicsReserveGi -le 0) { $script:KafkaOtherTopicsReserveGi = 4 }
		}
	}
}

# ─────────────────────────────────────────────────────────────────
# Kafka 盘预算与 db_task retention.bytes(S5b:docs/design/db-task-delivery-and-retention.md §B;
# 规格 docs/handoff/godb-pipeline-impl-20261015/S5-dirty-snapshot-retention-spec.md B-10 / B-11 / B-16 / B-17;容量模型 A0-3)
# ─────────────────────────────────────────────────────────────────
# 以前 db_task_zone_<id> 的积压上限只有 broker 缺省的每分区 retention.bytes 兜底,与盘的大小无关:zone 一多,
# Σ 保留上限就超过 PVC(容量模型估算是 PVC 的 2.5~5.8 倍),盘写满时整个 broker 停摆。现在分四步:
#   1. 目标值 target = roundUp(R × T × 1.2 ÷ P, 64MiB) + segment(Get-KafkaDbTaskRetentionBytesTarget);
#   2. 盘需求 PVC ≥ 1.3 × Σ(分区数 × (retention.bytes + segment)),另计审计 topic 与其它 topic 预留(Get-KafkaDiskPlan);
#      每个 broker 按「持有每个分区的一份副本」算(上界,不按 RF ÷ broker 数均摊,理由见 Get-KafkaDiskPlan);
#   3. 实际盘按 显式 -KafkaStorageGi > 线上(StatefulSet 模板与每块 PVC 实际容量取小)> 公式 取;盘装不下 target 时按盘收缩
#      retention.bytes 并告警,连 2 个 segment 都放不下才 throw;显式值与线上不同、或大于最小的实际 PVC 时拒绝(Assert-KafkaStorageChangeIsSafe);
#      zone 数 = zones 配置 ∪ -ZoneId,非 DryRun 时再并入线上不在配置里的 zone namespace;
#   4. 收缩后的值同时渲染进 db 与 login 的 ConfigMap(Kafka.RetentionBytes),两个服务启动时在 topic 级设置它。
# 纯函数(Get-KafkaDiskBudgetConstants / Get-KafkaDbTaskRetentionBytesTarget / Get-KafkaDiskPlan / Get-KafkaStorageChangeVerdict)
# 不读脚本参数、不碰集群,契约测试按 AST 抽出来直接喂值;胶水函数(Get-LiveKafkaStorageGi / Get-LiveKafkaPvcMinGi /
# Get-LiveZoneNamespaceNames / Resolve-KafkaDiskPlan / Write-KafkaDiskLedger / Assert-KafkaStorageChangeIsSafe)由写路径入口调用。

# 盘预算公式里的常数只写在这里一处,两个纯函数共用(契约测试与它们一起抽出)。
function Get-KafkaDiskBudgetConstants {
	return [pscustomobject]@{
		# 分区间的倾斜余量:key = player_id 的哈希不会把字节均分到 P 个分区上。
		SkewFactor              = [decimal]1.2
		# 盘的余量:retention 按 segment 删除且有检查间隔滞后,索引文件与 KRaft 元数据日志也在这块盘上。
		Headroom                = [decimal]1.3
		# retention.bytes 的取整粒度 64MiB。
		RetentionGranuleBytes   = [long]67108864
		# PVC 的下限与取整步长(Gi)。
		MinStorageGi            = 20
		StorageStepGi           = 10
		# 每分区至少要能留住的 segment 数:少于 2 个时只剩活跃 segment,关闭的 segment 一出现就被删,缓冲约等于零。
		MinSegmentsPerPartition = 2
	}
}

<#
.SYNOPSIS
	db_task 每分区 retention.bytes 的目标值(纯函数):roundUp(R × T × 1.2 ÷ P, 64MiB) + SegmentBytes,T = min(缓冲目标, retention.ms)。

.DESCRIPTION
	R × T 是一个 zone 按设计速率在 T 内写进 db_task 的字节数;× 1.2 是分区间倾斜余量,÷ P 摊到每个分区。
	再加一个 segment:retention 只删整个已关闭的 segment,不加的话实际能留住的比目标少最多一个 segment。
	T 取 min:retention.bytes 留得比 retention.ms 还久没有意义(时间先到先删),只会白占盘。
	输入任一项不为正即 throw(fail-closed:不替你猜一个保留量)。
#>
function Get-KafkaDbTaskRetentionBytesTarget {
	param(
		[Parameter(Mandatory = $true)][long]$DesignBytesPerSec,
		[Parameter(Mandatory = $true)][long]$BufferTargetMs,
		[Parameter(Mandatory = $true)][long]$RetentionMs,
		[Parameter(Mandatory = $true)][int]$Partitions,
		[Parameter(Mandatory = $true)][long]$SegmentBytes
	)

	if ($DesignBytesPerSec -le 0 -or $BufferTargetMs -le 0 -or $RetentionMs -le 0 -or $Partitions -le 0 -or $SegmentBytes -le 0) {
		throw "db_task retention.bytes 目标值的输入必须全部为正(design_bytes_per_sec=$DesignBytesPerSec buffer_target_ms=$BufferTargetMs retention_ms=$RetentionMs partitions=$Partitions segment_bytes=$SegmentBytes)。fail-closed:不替你猜一个保留量。"
	}
	$c = Get-KafkaDiskBudgetConstants
	$bufferMs = [Math]::Min($BufferTargetMs, $RetentionMs)
	# decimal 运算:1.2 用二进制浮点表示不精确,恰好落在 64MiB 整数倍上的值会被 Ceiling 多进一格。
	$perPartition = [decimal]$DesignBytesPerSec * [decimal]$bufferMs / [decimal]1000 * $c.SkewFactor / [decimal]$Partitions
	$granule = [decimal]$c.RetentionGranuleBytes
	return [long]([Math]::Ceiling($perPartition / $granule) * $granule) + $SegmentBytes
}

<#
.SYNOPSIS
	Kafka 盘预算(纯函数):按公式算每个 broker 需要的 PVC,再按实际用的盘把 db_task 的 retention.bytes 收缩到放得下。

.DESCRIPTION
	单位是字节,Gi = 1073741824。记 Z = zone 数、P = 每 zone 的 db_task 分区数、seg = SegmentBytes、
	A = 审计 topic 分区数之和(没有自己的 retention.bytes,按 broker 缺省 BrokerRetentionBytes 记)、reserve = 其它 topic 预留(GiB):
	  需求    required    = 1.3 × (Z × P × (target + seg) + A × (BrokerRetentionBytes + seg) + reserve × Gi)
	          RequiredGi  = max(20, ceil(required ÷ Gi ÷ 10) × 10)
	  实际盘  EffectiveGi = ExplicitStorageGi(> 0)
	                      > 线上 StatefulSet 在:min(LiveStorageGi, LivePvcGi)(LivePvcGi 为 $null 时取 LiveStorageGi)
	                      > 线上 StatefulSet 不在但 PVC 在:LivePvcGi
	                      > RequiredGi;Source 依次为 explicit / live / live-pvc / computed
	  可容纳  capacity    = (EffectiveGi × Gi ÷ 1.3 − A × (BrokerRetentionBytes + seg) − reserve × Gi) ÷ (Z × P) − seg
	          (上面需求式对 retention.bytes 的反解:每分区不超过它,Σ 保留上限就不超过 PVC ÷ 1.3)
	每个 broker 的份额取上界「每个分区各持有一份副本」,不按 RF ÷ Brokers 均摊:一个 broker 对同一分区至多持有一份副本
	(RF ≤ Brokers),所以它的落盘不超过单份数据的总量;RF ÷ Brokers 只在副本已在各 broker 间均分时成立。3 → N 扩容是放行的
	(Assert-KafkaTopologyChangeIsSafe),而新 broker 不会自动接手已有分区,kafka-reassign-partitions 之前老 broker 仍持有全部
	分区的副本:按均摊算会把收缩后的 retention.bytes 抬高 N/3 倍、写爆老 broker(B-17)。单 broker / RF 1 与 3 broker / RF 3
	结果与均摊相同,只有 ≥ 4 个 broker 时偏保守。Brokers / ReplicationFactor 仍校验并带回(账本行打印)。
	  生效值  target ≤ capacity 时 = target(FitLimited = false);否则 = floor(capacity, 64MiB)(FitLimited = true,只告警不阻断)
	  floor(capacity, 64MiB) < 2 × seg 时 throw:每分区连 2 个 segment 都放不下。
	  ImpliedBufferSec = P × (生效值 − seg) ÷ (R × 1.2):按设计速率估算的实际缓冲秒数(估算,不是承诺)。
	只在放不下时才按 64MiB 取整:target 本身是「64MiB 整数倍 + seg」,一律取整会把按公式算出的盘也截掉一个粒度
	(prod 2 个 zone:2785017856 → 2751463424),与缺省值表对不上。
	LiveStorageGi = 线上 StatefulSet kafka 的 volumeClaimTemplates 大小,$null = 没有这个 StatefulSet(全新集群、DryRun,
	或扩盘 runbook 的 --cascade=orphan 之后);LivePvcGi = 线上各块 kafka-data-kafka-<i> 实际容量的最小值,$null = 一块都没有。
	只看模板会按一块并不存在的盘算(扩盘 runbook 第 ② 步某块没扩成、停在 FileSystemResizePending,或 orphan 之后不带
	-KafkaStorageGi 跑了公式值),所以实际盘取两者中小的那个。
	StorageTemplateGi 是渲染进 kafka.yaml volumeClaimTemplates 的值:StatefulSet 在时必须等于线上模板(模板不可变,
	不同即 apply 被拒),哪怕它大于实际 PVC;其余情况等于 EffectiveGi。
	返回 [ordered] 哈希表:除结果外原样带回各项输入,账本行与 Assert-KafkaStorageChangeIsSafe 只读这一份。
#>
function Get-KafkaDiskPlan {
	param(
		[Parameter(Mandatory = $true)][int]$Brokers,
		[Parameter(Mandatory = $true)][int]$ReplicationFactor,
		[Parameter(Mandatory = $true)][int]$ZoneCount,
		[Parameter(Mandatory = $true)][int]$DbTaskPartitions,
		[Parameter(Mandatory = $true)][long]$DbTaskRetentionBytesTarget,
		[Parameter(Mandatory = $true)][long]$SegmentBytes,
		[Parameter(Mandatory = $true)][long]$BrokerRetentionBytes,
		[Parameter(Mandatory = $true)][int]$AuditPartitions,
		[Parameter(Mandatory = $true)][int]$OtherTopicsReserveGi,
		[AllowNull()]$LiveStorageGi = $null,
		[AllowNull()]$LivePvcGi = $null,
		[int]$ExplicitStorageGi = 0,
		[Parameter(Mandatory = $true)][long]$DesignBytesPerSec,
		[Parameter(Mandatory = $true)][long]$DbTaskRetentionMs
	)

	if ($Brokers -lt 1 -or $ReplicationFactor -lt 1 -or $ReplicationFactor -gt $Brokers -or $ZoneCount -lt 1 -or $DbTaskPartitions -lt 1 -or
		$DbTaskRetentionBytesTarget -le 0 -or $SegmentBytes -le 0 -or $BrokerRetentionBytes -le 0 -or $AuditPartitions -lt 0 -or
		$OtherTopicsReserveGi -lt 0 -or $ExplicitStorageGi -lt 0 -or $DesignBytesPerSec -le 0 -or $DbTaskRetentionMs -le 0) {
		throw "Kafka 盘预算的输入不合法(brokers=$Brokers rf=$ReplicationFactor zones=$ZoneCount db_task_partitions=$DbTaskPartitions target_retention_bytes=$DbTaskRetentionBytesTarget segment_bytes=$SegmentBytes broker_retention_bytes=$BrokerRetentionBytes audit_partitions=$AuditPartitions other_topics_reserve_gib=$OtherTopicsReserveGi explicit_gib=$ExplicitStorageGi design_bytes_per_sec=$DesignBytesPerSec db_task_retention_ms=$DbTaskRetentionMs)。fail-closed:不替你猜一块盘。"
	}
	$live = $null
	if ($null -ne $LiveStorageGi) {
		$live = [int]$LiveStorageGi
		if ($live -le 0) {
			throw "线上 Kafka PVC 大小 '$LiveStorageGi' 不是正数。fail-closed:不知道线上盘多大,就算不出不会写爆盘的 retention.bytes。"
		}
	}
	$livePvc = $null
	if ($null -ne $LivePvcGi) {
		$livePvc = [int]$LivePvcGi
		if ($livePvc -le 0) {
			throw "线上 Kafka PVC 的最小实际容量 '$LivePvcGi' 不是正数。fail-closed:不知道线上盘多大,就算不出不会写爆盘的 retention.bytes。"
		}
	}

	$c = Get-KafkaDiskBudgetConstants
	$gib = [decimal]1073741824
	$seg = [decimal]$SegmentBytes
	$auditBytes = [decimal]$AuditPartitions * ([decimal]$BrokerRetentionBytes + $seg)
	$reserveBytes = [decimal]$OtherTopicsReserveGi * $gib
	$dbTaskPartitionsTotal = [decimal]$ZoneCount * [decimal]$DbTaskPartitions
	# 单份数据 → 每个 broker 的落盘:取上界「每个分区各一份副本」(不乘 RF ÷ broker 数,理由见 DESCRIPTION),再乘 1.3 的余量。
	$perBrokerFactor = $c.Headroom
	$requiredBytes = $perBrokerFactor * ($dbTaskPartitionsTotal * ([decimal]$DbTaskRetentionBytesTarget + $seg) + $auditBytes + $reserveBytes)
	$step = [decimal]$c.StorageStepGi
	$requiredGi = [int]([Math]::Max([decimal]$c.MinStorageGi, [Math]::Ceiling($requiredBytes / $gib / $step) * $step))

	if ($ExplicitStorageGi -gt 0) {
		# 显式值与线上不符(含大于最小的实际 PVC)由 Assert-KafkaStorageChangeIsSafe 拒绝,早于任何写操作。
		$effectiveGi = $ExplicitStorageGi
		$templateGi = $ExplicitStorageGi
		$source = 'explicit'
	}
	elseif ($null -ne $live) {
		$effectiveGi = if ($null -ne $livePvc) { [Math]::Min($live, $livePvc) } else { $live }
		$templateGi = $live
		$source = 'live'
	}
	elseif ($null -ne $livePvc) {
		$effectiveGi = $livePvc
		$templateGi = $livePvc
		$source = 'live-pvc'
	}
	else {
		$effectiveGi = $requiredGi
		$templateGi = $requiredGi
		$source = 'computed'
	}

	$granule = [decimal]$c.RetentionGranuleBytes
	$capacity = ([decimal]$effectiveGi * $gib / $perBrokerFactor - $auditBytes - $reserveBytes) / $dbTaskPartitionsTotal - $seg
	$fit = [long]([Math]::Floor($capacity / $granule) * $granule)
	$minBytes = [long]$c.MinSegmentsPerPartition * $SegmentBytes
	if ($fit -lt $minBytes) {
		throw ("Kafka 盘预算:每个 broker ${effectiveGi}Gi(来源 $source)放不下 db_task —— 扣掉审计 topic($AuditPartitions 个分区)与其它 topic 预留(${OtherTopicsReserveGi}GiB)之后," +
			"$ZoneCount 个 zone × $DbTaskPartitions 个分区每分区只能分到 $([Math]::Max([long]0, $fit)) 字节,连 2 个 segment($minBytes 字节)都放不下" +
			"(RF=$ReplicationFactor,$Brokers 个 broker,余量 1.3)。按目标 retention.bytes=$DbTaskRetentionBytesTarget 需要每个 broker 至少 ${requiredGi}Gi:" +
			"按 deploy/k8s/README.md「Kafka 磁盘预算与 db_task retention.bytes」的扩盘 runbook 扩到 ${requiredGi}Gi 后带 -KafkaStorageGi $requiredGi 重跑;" +
			"或减少 zone、调小 -KafkaOtherTopicsReserveGi。尚未做任何写操作。")
	}
	$fitLimited = [decimal]$DbTaskRetentionBytesTarget -gt $capacity
	$effectiveRetentionBytes = if ($fitLimited) { $fit } else { $DbTaskRetentionBytesTarget }
	$impliedBufferSec = [Math]::Round([double]([decimal]$DbTaskPartitions * ([decimal]$effectiveRetentionBytes - $seg) / ([decimal]$DesignBytesPerSec * $c.SkewFactor)), 1)

	return [ordered]@{
		RequiredGi           = $requiredGi
		EffectiveGi          = [int]$effectiveGi
		StorageTemplateGi    = [int]$templateGi
		Source               = $source
		DbTaskRetentionBytes = [long]$effectiveRetentionBytes
		TargetRetentionBytes = $DbTaskRetentionBytesTarget
		FitLimited           = [bool]$fitLimited
		ImpliedBufferSec     = $impliedBufferSec
		ZoneCount            = $ZoneCount
		Brokers              = $Brokers
		ReplicationFactor    = $ReplicationFactor
		DbTaskPartitions     = $DbTaskPartitions
		SegmentBytes         = $SegmentBytes
		DesignBytesPerSec    = $DesignBytesPerSec
		DbTaskRetentionMs    = $DbTaskRetentionMs
		AuditPartitions      = $AuditPartitions
		BrokerRetentionBytes = $BrokerRetentionBytes
		OtherTopicsReserveGi = $OtherTopicsReserveGi
		LiveStorageGi        = $live
		LivePvcGi            = $livePvc
		ExplicitStorageGi    = $ExplicitStorageGi
	}
}

# 线上盘 → 显式 -KafkaStorageGi,这次算哪一类(纯函数,契约测试直接喂值)。LiveStorageGi = StatefulSet kafka 的模板大小
# ($null = 没有 StatefulSet),LivePvcGi = 各块 kafka-data-kafka-<i> 实际容量的最小值($null = 一块都没有):
#   absent            两者都没有(全新集群、DryRun):显式值或公式值直接用
#   auto              没给 -KafkaStorageGi:盘取 min(模板, 最小 PVC),渲染值 = 线上模板(StatefulSet 不在时 = 最小 PVC),apply 不冲突
#   unchanged         显式值 = 线上模板(StatefulSet 在时),且不大于最小 PVC;StatefulSet 不在时显式值 = 最小 PVC
#   expand-refused    StatefulSet 在、显式值 > 模板:volumeClaimTemplates 不可变,直接 apply 会被拒,改模板也不会改已有 PVC;按 runbook 先扩 PVC
#   shrink-refused    显式值 < 模板(StatefulSet 在),或 StatefulSet 不在、显式值 < 最小 PVC:PVC 不能缩
#   pvc-not-expanded  显式值 > 最小 PVC:有一块盘还没扩到这么大(扩盘 runbook 第 ② 步没成功、停在 FileSystemResizePending,
#                     或 --cascade=orphan 之后按一块不存在的盘重跑),按它算 retention.bytes 会写爆那块盘(B-17)
# 先判模板再判 PVC:StatefulSet 在时模板不等于显式值,apply 本身就会被拒。
function Get-KafkaStorageChangeVerdict {
	param(
		[AllowNull()]$LiveStorageGi,
		[AllowNull()]$LivePvcGi = $null,
		[int]$ExplicitStorageGi = 0
	)

	if ($null -eq $LiveStorageGi -and $null -eq $LivePvcGi) { return 'absent' }
	if ($ExplicitStorageGi -le 0) { return 'auto' }
	if ($null -ne $LiveStorageGi) {
		$template = [int]$LiveStorageGi
		if ($ExplicitStorageGi -gt $template) { return 'expand-refused' }
		if ($ExplicitStorageGi -lt $template) { return 'shrink-refused' }
	}
	if ($null -ne $LivePvcGi) {
		$pvc = [int]$LivePvcGi
		if ($ExplicitStorageGi -gt $pvc) { return 'pvc-not-expanded' }
		if ($null -eq $LiveStorageGi -and $ExplicitStorageGi -lt $pvc) { return 'shrink-refused' }
	}
	return 'unchanged'
}

# 线上 StatefulSet kafka 的 PVC 模板大小(Gi);不存在返回 $null。DryRun 不连集群,一律当不存在。
# 只认 <整数>Gi:别的单位(Ti / G / Mi …)、查询失败、StatefulSet 在却读不出大小,一律 throw(fail-closed)——
# 不知道线上盘多大,就算不出不会写爆盘的 retention.bytes。名字与大小一起取,两种「空」分得开:
# StatefulSet 不存在时整行为空(--ignore-not-found),存在却没有 volumeClaimTemplates 时只剩「kafka:」。
function Get-LiveKafkaStorageGi {
	if ($DryRun) { return $null }

	$baseArgs = Build-KubectlBaseArgs
	$allArgs = @()
	$allArgs += $baseArgs
	$allArgs += @("get", "statefulset", "kafka", "-n", $InfraNamespace, "--ignore-not-found", "-o", "jsonpath={.metadata.name}:{.spec.volumeClaimTemplates[0].spec.resources.requests.storage}")
	$out = & kubectl @allArgs
	if ($LASTEXITCODE -ne 0) {
		throw "查询线上 StatefulSet kafka 的 PVC 大小失败(namespace=$InfraNamespace)。fail-closed:不知道线上盘多大,就算不出不会写爆盘的 db_task retention.bytes,拒绝部署;尚未做任何写操作。"
	}
	$text = ([string]($out | Out-String)).Trim()
	if ([string]::IsNullOrEmpty($text)) { return $null }
	if ($text -notmatch '^kafka:(\d+)Gi$') {
		throw "线上 StatefulSet kafka 的 volumeClaimTemplates[0] 读出来是 '$text'(namespace=$InfraNamespace),不是「kafka:<整数>Gi」。fail-closed:只认 Gi,别的单位不替你换算;按 deploy/k8s/README.md「Kafka 磁盘预算与 db_task retention.bytes」核对线上 PVC 后再部署。"
	}
	return [int]$Matches[1]
}

# 线上各块 Kafka PVC(名字恰好是 kafka-data-kafka-<序号>,同 namespace 里别的 PVC 不算)实际容量的最小值(Gi);一块都没有返回 $null。
# DryRun 不连集群,一律当没有。模板(Get-LiveKafkaStorageGi)只说明「新建的 PVC 会申请多大」,说明不了已有的盘多大:
# 扩盘 runbook 第 ② 步某块没扩成或停在 FileSystemResizePending、第 ③ 步 --cascade=orphan 之后 StatefulSet 不在,都只能看 PVC。
# 读 status.capacity(实际分到的盘;需要文件系统扩容的驱动要扩完才更新它),不读 spec.resources.requests(patch 之后立刻就变,
# 扩容没做完时比实际盘大)。还没绑定的 PVC 没有 status.capacity(custom-columns 印成 <none>),取它的 requests:绑定时 PV 至少这么大,
# 而且这块盘上还没有任何数据 —— 不这样处理的话,一个调度不上的新 broker 就会把所有 zone 的 zone-up 拦下。
# 只认 <整数>Gi;别的单位、查询失败、行格式不对一律 throw(fail-closed)。custom-columns 不用 jsonpath 的 {"\n"}:
# 内嵌双引号经 Windows 原生命令传参容易被拆坏。
function Get-LiveKafkaPvcMinGi {
	if ($DryRun) { return $null }

	$baseArgs = Build-KubectlBaseArgs
	$allArgs = @()
	$allArgs += $baseArgs
	$allArgs += @("get", "pvc", "-n", $InfraNamespace, "--no-headers", "-o", "custom-columns=NAME:.metadata.name,CAPACITY:.status.capacity.storage,REQUEST:.spec.resources.requests.storage")
	$out = & kubectl @allArgs
	if ($LASTEXITCODE -ne 0) {
		throw "查询线上 Kafka PVC 的实际容量失败(namespace=$InfraNamespace)。fail-closed:不知道每块盘实际多大,就算不出不会写爆盘的 db_task retention.bytes,拒绝部署;尚未做任何写操作。"
	}
	$min = $null
	foreach ($line in (([string]($out | Out-String)) -split "`r?`n")) {
		$trimmed = $line.Trim()
		if ([string]::IsNullOrEmpty($trimmed)) { continue }
		$fields = @($trimmed -split '\s+')
		if ($fields.Count -ne 3) {
			throw "线上 PVC 列表(namespace=$InfraNamespace)有一行不是「名字 容量 申请量」三列:'$trimmed'。fail-closed:读不懂就不猜线上盘多大;尚未做任何写操作。"
		}
		if ($fields[0] -cnotmatch '^kafka-data-kafka-\d+$') { continue }
		$size = if ($fields[1] -ceq '<none>') { $fields[2] } else { $fields[1] }
		if ($size -cnotmatch '^(\d+)Gi$') {
			throw "线上 PVC $($fields[0])(namespace=$InfraNamespace)的容量读出来是 '$size'(status.capacity='$($fields[1])' requests='$($fields[2])'),不是「<整数>Gi」。fail-closed:只认 Gi,别的单位不替你换算;按 deploy/k8s/README.md「Kafka 磁盘预算与 db_task retention.bytes」核对线上 PVC 后再部署;尚未做任何写操作。"
		}
		$gi = [int]$Matches[1]
		if ($null -eq $min -or $gi -lt $min) { $min = $gi }
	}
	return $min
}

# 线上已有的 zone namespace(名字以「-NamespacePrefix-」开头,不含 -InfraNamespace),返回去掉前缀后的 zone 名数组(可能为空);
# DryRun 不连集群,返回 $null(= 没读)。盘预算的 Σ 覆盖共用这套 Kafka 的全部 zone,只数 zones 配置会漏掉用 -ZoneId 单独部署、
# 没登记进配置的 zone(或者这次换了 -ZonesConfigPath、配置缺席):漏一个 zone,按盘算出的每分区可容纳值就偏大。
# 只认 namespace,不看里面有没有 go/db:没有 go/db 的 zone 多算一个只会让预算偏保守。zone-down 删 namespace 之后,
# 它的 db_task topic 仍按旧的 retention.bytes 留着数据直到 retention.ms 到期,这里数不到(README 记为残余风险)。
# 查询失败一律 throw:读不到不等于没有。
function Get-LiveZoneNamespaceNames {
	if ($DryRun) { return $null }

	$baseArgs = Build-KubectlBaseArgs
	$allArgs = @()
	$allArgs += $baseArgs
	$allArgs += @("get", "namespace", "--no-headers", "-o", "custom-columns=NAME:.metadata.name")
	$out = & kubectl @allArgs
	if ($LASTEXITCODE -ne 0) {
		throw "列出线上 namespace 失败(数 zone namespace 用,前缀 $NamespacePrefix-)。fail-closed:数不清共用这套 Kafka 的 zone,就算不出不会写爆盘的 db_task retention.bytes,拒绝部署;尚未做任何写操作。"
	}
	$prefix = "$NamespacePrefix-"
	$names = [System.Collections.Generic.List[string]]::new()
	foreach ($line in (([string]($out | Out-String)) -split "`r?`n")) {
		$trimmed = $line.Trim()
		if ([string]::IsNullOrEmpty($trimmed)) { continue }
		if ($trimmed -ceq $InfraNamespace) { continue }
		if (-not $trimmed.StartsWith($prefix, [System.StringComparison]::Ordinal) -or $trimmed.Length -le $prefix.Length) { continue }
		$names.Add($trimmed.Substring($prefix.Length))
	}
	# 逗号:空数组也原样交给调用方($null 才表示没读)。
	return , ([string[]]$names.ToArray())
}

# 写路径入口算好的盘预算(Resolve-KafkaDiskPlan)。kafka.yaml 的 PVC 大小与 db / login ConfigMap 的 RetentionBytes 都读它。
$script:KafkaDiskPlan = $null

<#
.SYNOPSIS
	按本次部署的参数与仓库权威配置解出 Kafka 盘预算,缓存进 $script:KafkaDiskPlan 并返回。

.DESCRIPTION
	写路径入口(zone-up / all-up / infra-up)调用一次;Apply-Infra 的 kafka.yaml 分支与 db / login 的 ConfigMap 生成器读缓存,
	缓存为空时现场再算一次(同一套输入,结果相同;算不出即 throw,不回落任何常数)。输入:
	  - broker 数 / 副本数:Get-KafkaTopology(与 kafka.yaml、注入给 Go 服务的 KAFKA_TOPIC_REPLICATION_FACTOR 同源);
	  - zone 数:zones 配置 ∪ -ZoneId,与 Get-InfraZoneIds 同一口径(共用这套 Kafka 的全部 zone;infra-up 时 -ZoneId 不在 zones 配置里
	    也并入,多算一个 zone 只会让盘预算偏保守)。不调 Get-InfraZoneIds:它在 zones 配置缺席时那条 WARNING 讲的是 mysql-init-sql
	    预建库,出现在 zone-up 的输出里是误导(同 Write-MysqlConnectionLedger);缺席时只数 -ZoneId,账本行尾标 zones_config=missing,
	    并由 Write-KafkaDiskLedger 打 [KafkaDiskLedger]ZonesConfigMissing: 告警;-ReleaseProfile 不是 dev 时直接拒绝(Z 不可信)。
	    非 DryRun 时再并入线上的 zone namespace(Get-LiveZoneNamespaceNames):名字既不在 zones 配置里、也不是 -ZoneName 的,
	    每个各算一个 zone(Σ 约束覆盖的是线上真实存在的 zone,只数本次调用的配置会少算)。-ZoneName 与 -ZoneId 成对,
	    -ZoneId 已经在集合里,同名 namespace 不重复算;
	  - db_task 分区数:go/db/etc/db.yaml 的 ServerConfig.Kafka.PartitionCnt;审计分区数:go/data_service/etc/data_service.yaml 的
	    Kafka.TransactionLogPartitions + Kafka.SnapshotPartitions(与 kafka-topic-init 预建的同一对值);
	  - 线上盘:Get-LiveKafkaStorageGi(StatefulSet 模板)与 Get-LiveKafkaPvcMinGi(各块 PVC 实际容量的最小值),DryRun 都不查集群。
	-KafkaDbTaskRetentionBytes 为负,或显式给了却不到 2 个 segment,都在这里拒绝。
#>
function Resolve-KafkaDiskPlan {
	if ($KafkaDbTaskRetentionBytes -lt 0) {
		throw "-KafkaDbTaskRetentionBytes 必须 ≥ 0(实际 $KafkaDbTaskRetentionBytes):0 = 按档位的设计字节率与缓冲目标算,>0 = 显式的每分区目标值。Kafka 里 -1 表示不限,会让磁盘无界;go/db 与 go/login 启动时同样拒绝负值。尚未做任何写操作。"
	}
	$minBytes = [long](Get-KafkaDiskBudgetConstants).MinSegmentsPerPartition * [long]$KafkaSegmentBytes
	if ($KafkaDbTaskRetentionBytes -gt 0 -and $KafkaDbTaskRetentionBytes -lt $minBytes) {
		throw "-KafkaDbTaskRetentionBytes $KafkaDbTaskRetentionBytes 不到 2 个 segment($minBytes 字节,segment_bytes=$KafkaSegmentBytes):retention 只删整个已关闭的 segment,每分区只剩活跃 segment 时缓冲约等于零。拒绝部署;尚未做任何写操作。"
	}

	$topology = Get-KafkaTopology

	$zoneIds = [System.Collections.Generic.HashSet[int]]::new()
	$null = $zoneIds.Add([int]$ZoneId)
	# 已经由 ID 计入的 zone 名:zones 配置里的名字,加上与 -ZoneId 成对的 -ZoneName。比较不区分大小写:K8s 的 namespace 名只能是小写,
	# zones 配置里写成大写的同一个 zone 不该被多算一次。
	$knownZoneNames = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
	$null = $knownZoneNames.Add([string]$ZoneName)
	$zonesConfigMissing = $true
	$zonesPath = Resolve-ZonesConfigPath
	# 与 Get-ZonesFromJson / Get-InfraZoneIds 用同一个判据(Test-Path 不带 -LiteralPath)。zones 配置在却解析不了照常 throw。
	if (Test-Path $zonesPath) {
		$zonesConfigMissing = $false
		foreach ($zone in (Get-ZonesFromJson -Path $zonesPath)) {
			$null = $zoneIds.Add([int]$zone.zoneId)
			$null = $knownZoneNames.Add([string]$zone.name)
		}
	}
	if ($zonesConfigMissing -and $ReleaseProfile -ne 'dev') {
		throw ("zones 配置缺席($zonesPath),-ReleaseProfile $ReleaseProfile 拒绝部署:Kafka 盘预算的 zone 数只能数 -ZoneId 与线上已有的 zone namespace," +
			"还没部署、或已 zone-down 但 db_task topic 仍留着数据的 zone 都数不到,Z 不可信,db_task 的 retention.bytes 可能超量、写爆 Kafka 的盘(B-17)。" +
			"用 -ZonesConfigPath 指向登记了共用这套 Kafka 的全部 zone 的配置再部署;尚未做任何写操作。")
	}
	# 线上不在配置里的 zone namespace,各算一个 zone。$null = DryRun 没读(账本行印 unread)。
	$liveExtraZoneNames = $null
	$liveZoneNames = Get-LiveZoneNamespaceNames
	if ($null -ne $liveZoneNames) {
		$liveExtraZoneNames = @($liveZoneNames | Where-Object { -not $knownZoneNames.Contains([string]$_) } | Sort-Object -Unique)
	}
	# 不写 @($liveExtraZoneNames).Count:@($null) 是一个元素的数组,DryRun 时会凭空多算一个 zone。
	$liveExtraZoneCount = if ($null -eq $liveExtraZoneNames) { 0 } else { $liveExtraZoneNames.Count }
	$zoneCount = $zoneIds.Count + $liveExtraZoneCount

	$dbYaml = 'go/db/etc/db.yaml'
	$dsYaml = 'go/data_service/etc/data_service.yaml'
	$counts = @{}
	foreach ($item in @(
		@{ Name = 'DbTaskPartitions'; Path = $dbYaml; Key = 'ServerConfig.Kafka.PartitionCnt' }
		@{ Name = 'TransactionLogPartitions'; Path = $dsYaml; Key = 'Kafka.TransactionLogPartitions' }
		@{ Name = 'SnapshotPartitions'; Path = $dsYaml; Key = 'Kafka.SnapshotPartitions' }
	)) {
		$text = Get-AuthoritativeScalar -RelativePath $item.Path -KeyPath $item.Key
		$n = 0
		if (-not [int]::TryParse([string]$text, [ref]$n) -or $n -le 0) {
			throw "$($item.Path) 的 $($item.Key)='$text' 不是正整数,算不出 Kafka 盘预算,拒绝部署(fail-closed);尚未做任何写操作。"
		}
		$counts[$item.Name] = $n
	}

	$target = [long]$KafkaDbTaskRetentionBytes
	if ($target -eq 0) {
		$target = Get-KafkaDbTaskRetentionBytesTarget -DesignBytesPerSec $KafkaDbTaskDesignBytesPerSec -BufferTargetMs $KafkaDbTaskBufferTargetMs `
			-RetentionMs $KafkaDbTaskRetentionMs -Partitions $counts['DbTaskPartitions'] -SegmentBytes $KafkaSegmentBytes
	}

	$plan = Get-KafkaDiskPlan -Brokers $topology.Brokers -ReplicationFactor $topology.ReplicationFactor -ZoneCount $zoneCount `
		-DbTaskPartitions $counts['DbTaskPartitions'] -DbTaskRetentionBytesTarget $target -SegmentBytes $KafkaSegmentBytes `
		-BrokerRetentionBytes $KafkaRetentionBytes -AuditPartitions ($counts['TransactionLogPartitions'] + $counts['SnapshotPartitions']) `
		-OtherTopicsReserveGi $KafkaOtherTopicsReserveGi -LiveStorageGi (Get-LiveKafkaStorageGi) -LivePvcGi (Get-LiveKafkaPvcMinGi) `
		-ExplicitStorageGi $KafkaStorageGi -DesignBytesPerSec $KafkaDbTaskDesignBytesPerSec -DbTaskRetentionMs $KafkaDbTaskRetentionMs
	$plan['ZonesConfigMissing'] = $zonesConfigMissing
	$plan['ZonesConfigPath'] = [string]$zonesPath
	$plan['ZoneIdSelf'] = [int]$ZoneId
	# $null = DryRun 没读线上;否则是线上不在 zones 配置里(也不是 -ZoneName)的 zone 名,可能为空数组。
	$plan['LiveExtraZoneNames'] = $liveExtraZoneNames
	$script:KafkaDiskPlan = $plan
	return $plan
}

<#
.SYNOPSIS
	写路径打印 Kafka 盘预算一行(以「Kafka disk ledger:」开头);盘装不下目标保留量、或按设计速率的缓冲短于 retention.ms 时告警,不阻断。

.DESCRIPTION
	与 MySQL / Redis 连接总账同一纪律:打印用英文,告警开头那一段([KafkaDiskLedger]FitLimited: / [KafkaDiskLedger]BufferBelowRetention: /
	[KafkaDiskLedger]ZonesConfigMissing: / [KafkaDiskLedger]LiveZonesNotInConfig:)不留空格,ConsoleHost 按窗口宽度折行时契约测试只断这一段。
	改字段或措辞要同步改契约测试与 deploy/k8s/README.md 的「Kafka 磁盘预算」一节。
	implied_db_task_buffer_min 是按设计速率估算的缓冲,回档的 TargetTime 不能早于「现在 − 这个时长」(B-18),但它不是承诺。
	live_sts_gib / live_pvc_min_gib 是线上 StatefulSet 模板与最小实际 PVC(none = 没有或 DryRun 没读),live_extra_zones 是线上
	不在 zones 配置里的 zone namespace 数(unread = DryRun 没读),pvc_gib 由前两者取小、zones 已含后者。
#>
function Write-KafkaDiskLedger {
	param([Parameter(Mandatory = $true)]$Plan)

	$impliedMin = [long][Math]::Floor([double]$Plan.ImpliedBufferSec / 60)
	$retentionMin = [long][Math]::Floor([double]$Plan.DbTaskRetentionMs / 60000)
	$zonesTail = if ($Plan.ZonesConfigMissing) { ' zones_config=missing' } else { '' }
	$liveStsText = if ($null -eq $Plan.LiveStorageGi) { 'none' } else { [string]$Plan.LiveStorageGi }
	$livePvcText = if ($null -eq $Plan.LivePvcGi) { 'none' } else { [string]$Plan.LivePvcGi }
	$extraZones = $Plan.LiveExtraZoneNames
	$liveExtraText = if ($null -eq $extraZones) { 'unread' } else { [string]@($extraZones).Count }
	$ledgerFormat = "Kafka disk ledger: brokers={0} rf={1} zones={2} db_task_partitions={3} segment_bytes={4} db_task_retention_bytes={5} " +
		"target_retention_bytes={6} pvc_gib={7} pvc_source={8} required_gib={9} implied_db_task_buffer_min={10} db_task_retention_ms_min={11} " +
		"design_bytes_per_sec={12} audit_partitions={13} other_topics_reserve_gib={14} live_sts_gib={15} live_pvc_min_gib={16} live_extra_zones={17}{18}"
	Write-Host ($ledgerFormat -f $Plan.Brokers, $Plan.ReplicationFactor, $Plan.ZoneCount, $Plan.DbTaskPartitions, $Plan.SegmentBytes,
		$Plan.DbTaskRetentionBytes, $Plan.TargetRetentionBytes, $Plan.EffectiveGi, $Plan.Source, $Plan.RequiredGi, $impliedMin, $retentionMin,
		$Plan.DesignBytesPerSec, $Plan.AuditPartitions, $Plan.OtherTopicsReserveGi, $liveStsText, $livePvcText, $liveExtraText, $zonesTail)
	Write-Host "Kafka disk ledger note: db_task retention.bytes is a per-topic setting applied by db / login at startup (Kafka.RetentionBytes in their ConfigMaps); zones deployed earlier keep their previous value until their next zone-up. After adding zones or resizing the PVC, re-run zone-up for every zone, or apply it at once with kafka-configs.sh --alter (deploy/k8s/README.md)."

	if ($Plan.FitLimited) {
		# 格式串先拼好再 -f:-f 的优先级高于 +,写成一个表达式的话只有最后一段会被格式化。
		$fitFormat = ("[KafkaDiskLedger]FitLimited: PVC {0}Gi per broker (source={1}) cannot hold the db_task target retention.bytes={2}; " +
			"the topic-level retention.bytes is shrunk to {3} so that retained data stays within PVC/1.3 (about {4} min of backlog at the design rate of {5} bytes/s per zone). " +
			"Deployment continues: warning only, not blocking. " +
			"When {3} is below the topic's current retention.bytes (e.g. the broker default {7} on a topic without a topic-level value), applying it lowers the topic's current limit " +
			"and Kafka deletes unconsumed db_task backlog beyond it within retention.check.interval, whether or not db has read it; " +
			"confirm the db consumer lag is ~0 for every partition before rolling db / login (or expand the PVC first). " +
			"To restore the target, expand the PVC to at least {6}Gi per broker with the runbook in deploy/k8s/README.md " +
			"(patch each PVC kafka-data-kafka-<i>, kubectl delete statefulset kafka --cascade=orphan, then infra-up -KafkaStorageGi {6}), then re-run zone-up for every zone.")
		Write-Warning ($fitFormat -f $Plan.EffectiveGi, $Plan.Source, $Plan.TargetRetentionBytes, $Plan.DbTaskRetentionBytes, $impliedMin, $Plan.DesignBytesPerSec, $Plan.RequiredGi,
			$Plan.BrokerRetentionBytes)
	}
	if ($Plan.ZonesConfigMissing) {
		# dev 档才会走到这里(其它档 Resolve-KafkaDiskPlan 已拒绝)。措辞避开「Zones configuration missing」:那是 Get-InfraZoneIds 讲预建库的告警。
		$liveText = if ($null -eq $extraZones) { 'live zone namespaces were not read (DryRun)' } else { "plus $(@($extraZones).Count) live zone namespace(s) not named -ZoneName" }
		$missingFormat = ("[KafkaDiskLedger]ZonesConfigMissing: the zones config file {0} is absent, so the Kafka disk budget counts only -ZoneId {1} ({2}); zones={3} " +
			"can miss zones that share this Kafka and the db_task retention.bytes={4} may overcommit the PVC. Point -ZonesConfigPath at the config listing every zone on this Kafka. " +
			"Warning only under -ReleaseProfile dev; other release profiles refuse to deploy.")
		Write-Warning ($missingFormat -f $Plan.ZonesConfigPath, $Plan.ZoneIdSelf, $liveText, $Plan.ZoneCount, $Plan.DbTaskRetentionBytes)
	}
	elseif ($null -ne $extraZones -and @($extraZones).Count -gt 0) {
		$extraFormat = ("[KafkaDiskLedger]LiveZonesNotInConfig: zone namespaces for {0} exist on the cluster but are not in the zones config {1}; they share this Kafka, " +
			"so each is counted in zones={2}. Add them to the zones config (or zone-down them) so that every zone-up / infra-up counts the same zones.")
		Write-Warning ($extraFormat -f (@($extraZones) -join ','), $Plan.ZonesConfigPath, $Plan.ZoneCount)
	}
	if ([double]$Plan.ImpliedBufferSec * 1000 -lt [double]$Plan.DbTaskRetentionMs) {
		$bufferFormat = ("[KafkaDiskLedger]BufferBelowRetention: at the design rate of {0} bytes/s per zone, db_task retention.bytes={1} holds about {2} min of backlog, " +
			"shorter than the db_task retention.ms ({3} min), so retention.bytes deletes first. A DB outage longer than about {2} min loses queued saves, " +
			"and a rollback TargetTime older than that cannot be fully replayed from Kafka. Warning only, not blocking; the design rate is an estimate, not a guarantee.")
		Write-Warning ($bufferFormat -f $Plan.DesignBytesPerSec, $Plan.DbTaskRetentionBytes, $impliedMin, $retentionMin)
	}
}

# 写路径入口调用:显式 -KafkaStorageGi 与线上模板不同、或大于最小的实际 PVC 时拒绝(判定见 Get-KafkaStorageChangeVerdict),早于任何写操作。
# 不只 infra-up / all-up:zone-up 也按 -KafkaStorageGi 算 retention.bytes,给一块线上并不存在的盘算出来的值会写爆真实的盘(B-17)。
function Assert-KafkaStorageChangeIsSafe {
	param([Parameter(Mandatory = $true)]$Plan)

	$template = $Plan.LiveStorageGi
	$pvc = $Plan.LivePvcGi
	$explicit = [int]$Plan.ExplicitStorageGi
	$verdict = Get-KafkaStorageChangeVerdict -LiveStorageGi $template -LivePvcGi $pvc -ExplicitStorageGi $explicit
	if ($verdict -in @('absent', 'auto', 'unchanged')) { return }
	# expand-refused / shrink-refused 文案里的「线上」:StatefulSet 在时是它的模板,不在时(--cascade=orphan 之后)是最小的实际 PVC。
	$live = if ($null -ne $template) { $template } else { $pvc }
	if ($verdict -eq 'pvc-not-expanded') {
		$stsText = if ($null -ne $template) { "StatefulSet kafka 的模板是 ${template}Gi" } else { 'StatefulSet kafka 不在(--cascade=orphan 之后)' }
		throw @"
拒绝按 ${explicit}Gi 部署 Kafka 盘预算(namespace=$InfraNamespace):线上 PVC kafka-data-kafka-<i> 里最小的实际容量(status.capacity)只有 ${pvc}Gi($stsText),
按 ${explicit}Gi 算出的 db_task retention.bytes 会写爆那块盘(B-17)。尚未做任何写操作。
先完成 deploy/k8s/README.md「Kafka 磁盘预算与 db_task retention.bytes」扩盘 runbook 的第 ② 步并确认 CAPACITY:
  kubectl -n $InfraNamespace get pvc 列出的每块 kafka-data-kafka-<i> 的 CAPACITY 都已是 ${explicit}Gi 才算扩完;没扩成的重新 patch,
  状态里有 FileSystemResizePending 的逐个重启对应 broker(kubectl -n $InfraNamespace delete pod kafka-<i>,等它 Ready、under-replicated 分区为 0)。
然后带 -KafkaStorageGi $explicit 重跑。若只是这次不想改盘:去掉 -KafkaStorageGi(自动按线上最小的实际容量 ${pvc}Gi 算 retention.bytes)。
"@
	}
	if ($verdict -eq 'expand-refused') {
		throw @"
拒绝把 Kafka PVC 从线上 ${live}Gi 改成 ${explicit}Gi(namespace=$InfraNamespace):StatefulSet 的 volumeClaimTemplates 不可变,直接 apply 会被 API server 拒绝,改模板也不会改已有的 PVC。尚未做任何写操作。
扩盘按 deploy/k8s/README.md「Kafka 磁盘预算与 db_task retention.bytes」的 runbook 做:
  ① kubectl get sc <StorageClass> -o jsonpath={.allowVolumeExpansion} 必须为 true(否则只能按「Kafka:多 broker」的重建流程换盘);
  ② 逐个 kubectl -n $InfraNamespace patch pvc kafka-data-kafka-<i> -p '{"spec":{"resources":{"requests":{"storage":"${explicit}Gi"}}}}',
     确认容量生效(需要文件系统扩容时逐个重启 broker,并确认 under-replicated 分区为 0);
  ③ kubectl -n $InfraNamespace delete statefulset kafka --cascade=orphan(Pod 与 PVC 保留,broker 不停);
  ④ 带 -KafkaStorageGi $explicit 重跑 infra-up(新 StatefulSet 收养原有 Pod 与 PVC),再对每个 zone 重跑 zone-up,让 db / login 按新盘设置 db_task 的 retention.bytes。
若只是这次不想改盘:去掉 -KafkaStorageGi(自动沿用线上 ${live}Gi),或显式传 -KafkaStorageGi $live。
"@
	}
	if ($verdict -eq 'shrink-refused') {
		throw @"
拒绝把 Kafka PVC 从线上 ${live}Gi 缩到 ${explicit}Gi(namespace=$InfraNamespace):PVC 不能缩,Kubernetes 只支持扩容。尚未做任何写操作。
真要换小盘只能按 deploy/k8s/README.md「Kafka:多 broker」的重建流程(停写、删 StatefulSet 与 PVC、重建;Kafka 里未消费的消息全部丢弃)。
若只是这次不想改盘:去掉 -KafkaStorageGi(自动沿用线上 ${live}Gi),或显式传 -KafkaStorageGi $live。
"@
	}
	throw "内部错误:Get-KafkaStorageChangeVerdict 返回了未知判定 '$verdict'(live=$live explicit=$explicit),拒绝部署。"
}

function Show-ExposureProfileWarning {
	if ($OpsProfile -eq "custom" -and $GateServiceType -eq "LoadBalancer") {
		Write-Warning "Using OpsProfile=custom with GateServiceType=LoadBalancer. Ensure your cluster has a mature LB implementation; otherwise prefer NodePort + external L4 load balancer or use -OpsProfile bare-metal."
	}
	if ($ClientEntryMode -eq "external") {
		# external:gate 在发布进 etcd 之前自报客户端可达地址(NodeInfo.client_endpoint,D76),login / scene_manager 下发的
		# 就是它;每序号 Service gate-<i> 把这个地址接到同序号的 Pod。其余组合的合法性由 Assert-ClientEntryDeployPreflight 校验。
		return
	}
	if ($GateServiceType -ne "ClusterIP") {
		# podip:暴露 Service 只是一半。客户端的 gate 地址不是从 Service 拿的,而是 login 从 etcd 读到 endpoint 后
		# 下发的(go/login internal/svc/servicecontext.go CandidatesForZone),podip 下那就是 POD_IP —— 集群内地址,
		# 只有 in-cluster 的 robot 连得上。集群外客户端要走 -ClientEntryMode external(D76–D93)。
		Write-Warning "GateServiceType=$GateServiceType only exposes the Service: with -ClientEntryMode podip, login returns the in-cluster POD_IP registered by gate in etcd, so external clients still cannot connect. For external access, use -ClientEntryMode external (gate StatefulSet + per-ordinal Service; see docs/design/k8s-client-entry.md). A single gate-entry is generated only when gate replicas=1 (D90)."
	}
}

# 本次是否部署 zone 内的 login(Apply-GoSvcManifests 的同一组条件)。-LoginDevPasswordAuth 只作用于它:
# preflight 的"不生效"警告与共享口令的解析共用这一条判定,不在两处各写一遍。
function Test-ZoneLoginDeployed {
	return (($Command -in @("zone-up", "all-up")) -and -not $SkipGoSvc -and -not [string]::IsNullOrWhiteSpace($GoSvcRegistry))
}

# 本次是否部署 gateway(Apply-Zone → Apply-JavaSvcManifests 的同一组开关)。集群外入口预检与管理面口令解析共用这一处判定。
function Test-ZoneGatewayDeployed {
	return (($Command -in @("zone-up", "all-up")) -and -not $SkipJavaSvc -and -not [string]::IsNullOrWhiteSpace($JavaSvcRegistry))
}

# 本次是否部署 zone 内的 db(与 Test-ZoneLoginDeployed 同一组条件:db 与 login 都是随 zone 走的 Go 服务,
# 由 Apply-GoSvcManifests 一起部署)。db 消费组计划(Initialize-DbConsumerGroupPlans)只在它为真时计算。
function Test-ZoneDbDeployed {
	return (($Command -in @("zone-up", "all-up")) -and -not $SkipGoSvc -and -not [string]::IsNullOrWhiteSpace($GoSvcRegistry))
}

# 要注入玩家收件箱运维口令时(口令已解析、非空),核对服务 yaml 声明了 PlayerInbox.AdminTokenEnv。
# 口令只经环境变量进容器,ConfigMap 只写变量名;Go 侧不认这个键时,go-zero 把它当未知键静默忽略,运维接口就一直停用 ——
# 部署方以为开了 GM 处置,实际所有调用都被拒。宁可在任何写操作之前拒绝部署并点名缺什么。
# 服务 yaml 里有这个键只是「Go 侧声明了它」的代理判据(go/player_inbox 的 config_test 加载同一份 yaml),不替代集成验证。
function Assert-PlayerInboxAdminTokenEnvDeclared {
	$relativePath = 'go/player_inbox/etc/player_inbox.yaml'
	$full = Join-Path $RepoRoot ($relativePath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
	$declared = Get-YamlScalar -Path $full -KeyPath 'PlayerInbox.AdminTokenEnv'
	if (-not $declared.Found) {
		throw ("MMORPG_PLAYER_INBOX_ADMIN_TOKEN 已设置,但 $relativePath 没有声明 PlayerInbox.AdminTokenEnv($($declared.Reason)):" +
			"部署侧只经环境变量 $($script:PlayerInboxAdminTokenEnv) 注入口令,ConfigMap 不写口令值,Go 侧不认这个键时口令会被静默忽略、运维接口一直停用。" +
			"先让 go/player_inbox 的配置支持 AdminTokenEnv(见 docs/ops/player-inbox-runbook.md §1),或本次不注入口令。")
	}
}

<#
.SYNOPSIS
	scene 收件箱消费端开关的权威值:bin/etc/game_config.yaml 的 player_inbox.consumer_enabled,返回 'true' 或 'false'。

.DESCRIPTION
	交接 player-inbox-handoff-20261015 §6 H1:以前这个值由 -PlayerInboxConsumer 渲染,默认 off、不粘滞、包装入口不透传,
	打开消费之后任何一次例行发布都会把它写回 false。现在只有 git 跟踪的这一份(与 PlayerInbox.EnqueueTypes 镜像服务 yaml 同口径),
	渲染值不从集群取:zone-rollback 会删 zone namespace,那里的值不可靠。写前门禁另外只读 infra 侧线上的白名单作交叉核对
	(Read-PlayerInboxGateLiveFacts),不改这里的返回值。
	读的是 $RepoRoot 下的**工作区文件**,不是 HEAD:这份 yaml 也是本机 scene 的运行配置,可能带着没提交的改动。
	未提交改动由 Get-PlayerInboxGateSourceState 核对,staging / prod 的真部署在写操作之前拒绝(Assert-PlayerInboxConsumerGate)。
	三个调用方读同一份:New-NodeConfigMapYaml(渲染前核对,再整段复制 player_inbox 块)、Read-PlayerInboxGateLiveFacts
	(决定读不读 infra 侧)与写路径入口的 Assert-PlayerInboxConsumerGate。
	- 键缺失即 throw:C++ 缺键取 false,部署侧要求显式写出(与 EnqueueTypes 空也要写 [] 同理)。
	- 只认小写 true / false:C++ 用 yaml-cpp 的 as<bool>,它还认 yes / on / True 等写法,门禁不替你猜,写成别的即 throw。
	- -Expected 是 -PlayerInboxConsumer 的值:空 = 不核对;on / off 与 yaml 不一致即 throw,并指向要改的文件。它不改返回值。
	按动态作用域读 $RepoRoot(Get-AuthoritativeScalar)。
#>
function Get-PlayerInboxConsumerEnabled {
	param([string]$Expected = '')

	if ($Expected -cnotin @('', 'on', 'off')) {
		throw "内部错误:Get-PlayerInboxConsumerEnabled -Expected 只收 空 / on / off,得到 '$Expected'。"
	}
	$relativePath = 'bin/etc/game_config.yaml'
	$keyPath = 'player_inbox.consumer_enabled'
	$value = Get-AuthoritativeScalar -RelativePath $relativePath -KeyPath $keyPath
	if ($value -cne 'true' -and $value -cne 'false') {
		throw "收件箱消费开关:$relativePath 的 $keyPath='$value' 只能写小写 true 或 false(fail-closed:不替你猜开关)。"
	}
	if (-not [string]::IsNullOrEmpty($Expected)) {
		$expectedValue = if ($Expected -ceq 'on') { 'true' } else { 'false' }
		if ($expectedValue -cne $value) {
			throw ("-PlayerInboxConsumer $Expected 与 $relativePath 的 $keyPath=$value 不一致,拒绝部署。" +
				"本参数只用来核对,不改渲染值:要开 / 关消费,改那份 yaml 并提交,再 zone-up 并滚动 scene" +
				"(deploy/k8s/README.md「玩家收件箱 player-inbox」;上线顺序见 docs/ops/player-inbox-runbook.md)。")
		}
	}
	return $value
}

<#
.SYNOPSIS
	收件箱写前门禁(交接 §6 H1):scene 消费端关着、而入箱白名单非空时拒绝部署,除非显式传 -AllowPlayerInboxConsumerOff。

.DESCRIPTION
	消费端关 + 白名单非空 = GM 处置照样回 created=true,却没有 scene 执行;条目 delivery_count=0,进不了任何告警。
	由写路径入口在任何集群写操作之前调用,zone-up / all-up / infra-up 三条写命令都判。分三层:
	  1. 静态:当前检出里的两份 yaml。
	     - 消费端:bin/etc/game_config.yaml 的 player_inbox.consumer_enabled(Get-PlayerInboxConsumerEnabled,顺带核对 -PlayerInboxConsumer);
	     - 入箱:go/player_inbox/etc/player_inbox.yaml 的 PlayerInbox.EnqueueTypes(Get-PlayerInboxEnqueueTypes,顺带核对每一项)。
	     这一层只拦「同一个检出里两个值相互矛盾」。它不知道另一侧线上部署的是哪个 commit 的值:zone(consumer_enabled)与
	     infra(EnqueueTypes)分开部署,检出开消费之前的旧 commit 重跑 zone-up,这一层读到的是那个 commit 的空白名单,照样放行。
	  2. 线上交叉核对(-LiveFacts,Read-PlayerInboxGateLiveFacts 的结果):本次把 false 渲染进 node-config 时,看 infra 线上的白名单;
	     本次把非空白名单渲染进 ConfigMap 时,看各 zone 线上 node-config 的开关。-DryRun 不读集群,这一层跳过。
	     局限:读的是 ConfigMap,不是进程。C++ 与 go-zero 都只在启动时读配置,ConfigMap 改了而 Pod 没滚动时两者可以不一致;
	     上线验收仍以 scene 启动行 consumer_enabled=1 与 GmSanctionPlayer 的回包为准。
	  3. 来源(-SourceState,Get-PlayerInboxGateSourceState 的结果):两份 yaml 相对 HEAD 有没有未提交改动。
	     -RequireCommittedSources 为真(写路径入口在非 DryRun 的 staging / prod 传)时,有改动或判不出就拒绝;否则只告警。
	-AllowConsumerOff 只豁免 1、2 两层的「消费端关、入箱开」(打一条警告放行),不豁免第 3 层,也不豁免 yaml 本身写错。
	-SourceState / -LiveFacts 为 $null = 不核对这一层(契约测试直接调用时);写路径入口两个都传。
	放行时打一行 "player-inbox consumer gate: ... OK"(豁免时改为警告),写明两个值各取自哪份文件、HEAD、工作区是否干净、线上核对结果。
#>
function Assert-PlayerInboxConsumerGate {
	param(
		# -PlayerInboxConsumer 的值,原样交给 Get-PlayerInboxConsumerEnabled 核对(空 / on / off)。
		[string]$ExpectedConsumer = '',
		[bool]$AllowConsumerOff = $false,
		# Get-PlayerInboxGateSourceState 的结果;$null = 不核对来源。
		$SourceState = $null,
		[bool]$RequireCommittedSources = $false,
		# Read-PlayerInboxGateLiveFacts 的结果;$null = 不做线上交叉核对。
		$LiveFacts = $null
	)

	$consumerFile = 'bin/etc/game_config.yaml'
	$enqueueFile = 'go/player_inbox/etc/player_inbox.yaml'
	$consumerEnabled = Get-PlayerInboxConsumerEnabled -Expected $ExpectedConsumer
	$enqueueTypes = @(Get-PlayerInboxEnqueueTypes -RelativePath $enqueueFile)

	# 第 3 层先判:staging / prod 拿未提交的值去比,比出来的结论本身就不可追溯。
	$head = 'unknown'
	$worktree = 'unchecked'
	if ($null -ne $SourceState) {
		if (-not [string]::IsNullOrEmpty([string]$SourceState.Head)) { $head = [string]$SourceState.Head }
		$dirtyFiles = @($SourceState.DirtyFiles | Where-Object { -not [string]::IsNullOrEmpty($_) })
		$sourceProblem = ''
		if (-not $SourceState.Ok) {
			$worktree = 'unknown'
			$sourceProblem = "判不出 $consumerFile / $enqueueFile 是否已提交($($SourceState.Reason))"
		}
		elseif ($dirtyFiles.Count -gt 0) {
			$worktree = 'dirty'
			$sourceProblem = "$($dirtyFiles -join ', ') 相对 HEAD $head 有未提交改动"
		}
		else {
			$worktree = 'clean'
		}
		if ($sourceProblem -ne '') {
			if ($RequireCommittedSources) {
				throw ("拒绝部署:$sourceProblem。收件箱两个开关的权威值是提交过的 yaml,部署读的却是工作区文件;" +
					"staging / prod 不接受未提交的值(发布记录里看不出它来自哪里)。先提交或还原这两份文件再部署。")
			}
			Write-Warning "player-inbox consumer gate: $sourceProblem;本次按工作区的值渲染(只有 staging / prod 的真部署拒绝)。"
		}
	}
	elseif ($RequireCommittedSources) {
		throw '内部错误:Assert-PlayerInboxConsumerGate -RequireCommittedSources 需要 -SourceState(Get-PlayerInboxGateSourceState 的结果)。'
	}

	$refusals = New-Object System.Collections.Generic.List[string]
	# 第 1 层:同一个检出里的两份 yaml。
	if ($consumerEnabled -ceq 'false' -and $enqueueTypes.Count -gt 0) {
		$refusals.Add("$consumerFile 的 player_inbox.consumer_enabled=false,而 $enqueueFile 的 PlayerInbox.EnqueueTypes 非空($($enqueueTypes -join ', '))")
	}
	# 第 2 层:另一侧线上已部署的值。
	$liveSummary = 'not-read'
	if ($null -ne $LiveFacts) {
		$liveSummary = [string]$LiveFacts.Note
		if ($null -ne $LiveFacts.InfraEnqueueTypes) {
			$liveEnqueue = @($LiveFacts.InfraEnqueueTypes | Where-Object { -not [string]::IsNullOrEmpty($_) })
			if ($consumerEnabled -ceq 'false' -and $liveEnqueue.Count -gt 0) {
				$refusals.Add(("本次把 consumer_enabled=false 渲染进 node-config,而线上 $($LiveFacts.InfraSource) 的 PlayerInbox.EnqueueTypes 非空($($liveEnqueue -join ', '))。" +
					"常见原因是检出开消费之前的旧 commit 重跑了 zone-up:开消费之后回滚 zone,用当前 HEAD 的脚本配旧 -NodeImage,或 kubectl rollout undo"))
			}
		}
		if ($null -ne $LiveFacts.ZoneConsumers -and $enqueueTypes.Count -gt 0) {
			$offZones = @($LiveFacts.ZoneConsumers | Where-Object { $null -ne $_ -and [string]$_.Value -cne 'true' })
			if ($offZones.Count -gt 0) {
				$offText = @($offZones | ForEach-Object {
						$shown = if ([string]::IsNullOrEmpty([string]$_.Value)) { 'absent' } else { [string]$_.Value }
						"$($_.Namespace)=$shown"
					}) -join ', '
				$refusals.Add(("本次把非空的 PlayerInbox.EnqueueTypes($($enqueueTypes -join ', '))渲染进 player-inbox 的 ConfigMap,而线上这些 zone 的 node-config 里 " +
					"player_inbox.consumer_enabled 不是 true(absent = 缺键,C++ 取 false):$offText。" +
					"先逐 zone zone-up 打开消费并滚动 scene,再部署白名单;all-up 先部署白名单、后部署 zone,不能一步完成,先跑 all-up -SkipInfra 或逐 zone zone-up"))
			}
		}
	}

	$summary = "player-inbox consumer gate: consumer_enabled=$consumerEnabled ($consumerFile) enqueue_types=$($enqueueTypes.Count) ($enqueueFile) head=$head worktree=$worktree live=$liveSummary"
	if ($refusals.Count -gt 0) {
		if (-not $AllowConsumerOff) {
			throw ("拒绝部署:" + ($refusals -join ';另外,') + "。这样部署后 GM 处置照样入箱成功,却没有 scene 执行,也不会触发任何告警。" +
				"要开消费:把 consumer_enabled 改成 true 并提交,逐 zone zone-up 并滚动 scene(上线顺序第 5 步),之后才部署白名单;" +
				"要关消费:先把 EnqueueTypes 改回 [] 并提交、重跑 infra-up,确认 GmSanctionPlayer 回 kPlayerInboxDisabled,再关 consumer_enabled。" +
				"明知如此仍要部署(例如紧急关消费),直接调 tools/scripts/k8s_deploy.ps1 并显式传 -AllowPlayerInboxConsumerOff。" +
				"包装入口(dev_tools.ps1 / k8s_image.ps1 / k8s_zone_rollback.ps1)没有这个开关:从包装入口部署被拒时," +
				"先把 $enqueueFile 的 EnqueueTypes 改回 [] 并提交、重跑 infra-up(正常关消费的顺序),门禁随之放行。")
		}
		Write-Warning ($summary + ':-AllowPlayerInboxConsumerOff 放行了「消费端关、入箱开」(' + ($refusals -join ';') + ')。新入箱的处置不会执行,直到 consumer_enabled 改回 true 并滚动 scene。')
		return
	}
	Write-Host "$summary OK"
}

<#
.SYNOPSIS
	收件箱两个开关的来源:bin/etc/game_config.yaml 与 go/player_inbox/etc/player_inbox.yaml 相对 HEAD 有没有未提交改动。
	返回 [pscustomobject]@{ Ok [bool]; Head [string](12 位短 sha,判不出为空); DirtyFiles [string[]]; Reason [string] }。

.DESCRIPTION
	部署读的是工作区文件(Get-AuthoritativeScalar / Get-AuthoritativeYamlBlock 都读 $RepoRoot 下的文件),而 bin/etc/game_config.yaml
	也是本机 scene 的运行配置,本机联调收件箱就要改它。不核对的话,一份没提交的 consumer_enabled: true 会随部署进集群,发布记录里看不出来。
	整树脏只让镜像 tag 带 -dirty,而 -dirty 只在 prod 档被拒、显式传干净的 -NodeImage 时任何档位都不拦,所以这里单独看这两份文件。
	每份文件先 git ls-files --error-unmatch 证明它被跟踪(未跟踪的文件 git diff 恒为空),再 git diff --quiet HEAD(含暂存区)。
	git 不在 PATH、不是仓库、命令出错都返回 Ok=$false,由 Assert-PlayerInboxConsumerGate 按档位决定拒绝还是告警。
	只读本地仓库,不碰集群;按动态作用域读 $RepoRoot。
#>
function Get-PlayerInboxGateSourceState {
	$files = @('bin/etc/game_config.yaml', 'go/player_inbox/etc/player_inbox.yaml')
	$state = [pscustomobject]@{ Ok = $false; Head = ''; DirtyFiles = [string[]]@(); Reason = '' }
	if ($null -eq (Get-Command git -ErrorAction SilentlyContinue)) {
		$state.Reason = 'git 不在 PATH 里'
		return $state
	}
	$headOutput = [string](& git -C $RepoRoot rev-parse --short=12 HEAD 2>$null)
	if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($headOutput)) {
		$state.Reason = "git rev-parse HEAD 失败(不是 git 仓库?):$RepoRoot"
		return $state
	}
	$state.Head = $headOutput.Trim()
	$dirty = New-Object System.Collections.Generic.List[string]
	foreach ($file in $files) {
		& git -C $RepoRoot ls-files --error-unmatch -- $file 2>$null | Out-Null
		if ($LASTEXITCODE -ne 0) {
			$state.Reason = "$file 不在 git 跟踪里"
			return $state
		}
		& git -C $RepoRoot diff --quiet HEAD -- $file 2>$null
		$diffExit = $LASTEXITCODE
		if ($diffExit -eq 1) {
			$dirty.Add($file)
		}
		elseif ($diffExit -ne 0) {
			$state.Reason = "git diff HEAD -- $file 失败(退出码 $diffExit)"
			return $state
		}
	}
	$state.DirtyFiles = [string[]]$dirty.ToArray()
	$state.Ok = $true
	return $state
}

<#
.SYNOPSIS
	收件箱写前门禁的线上交叉核对:按本次渲染哪一侧,只读另一侧线上已部署的值,交给 Assert-PlayerInboxConsumerGate -LiveFacts。
	返回 [pscustomobject]@{ Note [string]; InfraSource [string]; InfraEnqueueTypes [string[]] 或 $null; ZoneConsumers [pscustomobject[]] 或 $null }。
	ZoneConsumers 每项 { Namespace; Value },Value 是 consumer_enabled 的原文,缺键为空串(C++ 缺键取 false)。$null = 这一侧没读。

.DESCRIPTION
	静态门禁只比当前检出,而 zone(consumer_enabled)与 infra(EnqueueTypes)分开部署、可以来自不同 commit(交接 §6 H1 复核)。
	  - 本次渲染 scene 开关(zone-up / all-up)且 yaml 是 false:读 infra namespace 线上 go-svc-player-inbox-config 的 PlayerInbox.EnqueueTypes。
	    本次先部署全局 Go 服务时(all-up 不带 -SkipInfra,Test-GlobalGoSvcDeployed 为真)不读:Apply-Infra 先把 yaml 的白名单写上去,
	    zone 才滚动,静态门禁核过的就是这一份。zone-rollback 只删 zone namespace、不碰 infra,所以 zone 回档期间这个值仍可靠。
	    ConfigMap 不存在(收件箱没部署)按空白名单算;ConfigMap 在而 EnqueueTypes 缺键,按 Go 侧缺省(空)算。只看空不空,不核对类型名:
	    线上值可能来自别的 commit。
	  - 本次渲染非空白名单(Test-GlobalGoSvcDeployed 为真):读线上全部 zone namespace(名字以 "<-NamespacePrefix>-" 开头)里 node-config 的
	    game_config.yaml,取 player_inbox.consumer_enabled。all-up 也读:它先部署白名单、后部署 zone,这之间那些 zone 还关着。
	    namespace 还不存在的 zone(新 zone、回档中被删的)不在其中:它们之后 zone-up 时由上一条核对。用别的 -NamespacePrefix 部署的 zone 看不到。
	-DryRun 不读集群(Invoke-ClientEntryKubectl 不看 $DryRun,判断在这里做):两侧都是 $null,Note 写明跳过。
	读 yaml 的错误(缺键、写法不对、-ExpectedConsumer 与 yaml 不一致)先于任何 kubectl 调用 throw,与 Assert-PlayerInboxConsumerGate 报同一句。
	kubectl 失败、输出不是 JSON、ConfigMap 形状不对一律 throw:读不到不等于没有(同 Read-LiveDbConfigMapYaml)。尚未做任何写操作。
#>
function Read-PlayerInboxGateLiveFacts {
	param(
		# -PlayerInboxConsumer 的值:只为让「参数与 yaml 不一致」先于读集群报出来,不影响读什么。
		[string]$ExpectedConsumer = ''
	)

	$facts = [pscustomobject]@{ Note = 'not-needed'; InfraSource = ''; InfraEnqueueTypes = $null; ZoneConsumers = $null }
	$consumerEnabled = Get-PlayerInboxConsumerEnabled -Expected $ExpectedConsumer
	$rendersEnqueue = [bool](Test-GlobalGoSvcDeployed)
	$needInfra = ($Command -in @('zone-up', 'all-up')) -and -not $rendersEnqueue -and ($consumerEnabled -ceq 'false')
	$needZones = $rendersEnqueue -and (@(Get-PlayerInboxEnqueueTypes -RelativePath 'go/player_inbox/etc/player_inbox.yaml').Count -gt 0)
	if (-not $needInfra -and -not $needZones) { return $facts }
	if ($DryRun) {
		$facts.Note = 'skipped(dry-run)'
		return $facts
	}

	$notes = New-Object System.Collections.Generic.List[string]
	if ($needInfra) {
		$info = $GoSvcCatalogue['player-inbox']
		$facts.InfraSource = "namespace $InfraNamespace 的 ConfigMap $($info.ConfigMap)"
		$result = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'configmap', $info.ConfigMap, '-n', $InfraNamespace, '--ignore-not-found', '-o', 'json') `
			-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
		if ($result.ExitCode -ne 0) {
			throw "读取线上 $($facts.InfraSource) 失败(kubectl 退出码 $($result.ExitCode)):$($result.Stderr)。fail-closed:读不到不等于入箱关着,判不出本次关消费会不会让已开的入箱无人执行,拒绝部署;尚未做任何写操作。"
		}
		$items = [string[]]@()
		$text = ([string]$result.Stdout).Trim()
		if ($text -ne '') {
			try {
				$configMap = $text | ConvertFrom-Json -ErrorAction Stop
			}
			catch {
				throw "线上 $($facts.InfraSource) 的 kubectl 输出不是合法 JSON,判不出入箱白名单,拒绝部署;尚未做任何写操作。$($_.Exception.Message)"
			}
			$property = $null
			$dataProperty = $configMap.PSObject.Properties['data']
			if ($null -ne $dataProperty -and $null -ne $dataProperty.Value) {
				$property = $dataProperty.Value.PSObject.Properties[$info.ConfigFile]
			}
			if ($null -eq $property -or [string]::IsNullOrWhiteSpace([string]$property.Value)) {
				throw "线上 $($facts.InfraSource) 存在,但没有 data.$($info.ConfigFile):不是本脚本生成的形状,判不出入箱白名单,拒绝部署;尚未做任何写操作。"
			}
			$items = (ConvertFrom-PlayerInboxEnqueueTypesText -Text ([string]$property.Value) -Source "线上 $($facts.InfraSource)").Items
		}
		$facts.InfraEnqueueTypes = [string[]]@($items)
		$notes.Add("infra_enqueue_types=$(@($items).Count)")
	}
	if ($needZones) {
		$prefix = "$NamespacePrefix-"
		$result = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'configmap', '--all-namespaces', '--field-selector', 'metadata.name=node-config', '-o', 'json') `
			-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
		if ($result.ExitCode -ne 0) {
			throw "读取各 zone 线上的 ConfigMap node-config 失败(kubectl 退出码 $($result.ExitCode)):$($result.Stderr)。fail-closed:判不出各 zone 的 scene 是否在消费收件箱,拒绝部署;尚未做任何写操作。"
		}
		try {
			$list = ([string]$result.Stdout) | ConvertFrom-Json -ErrorAction Stop
		}
		catch {
			throw "各 zone 线上 ConfigMap node-config 的 kubectl 输出不是合法 JSON,判不出各 zone 的 scene 是否在消费收件箱,拒绝部署;尚未做任何写操作。$($_.Exception.Message)"
		}
		$zones = New-Object System.Collections.Generic.List[object]
		foreach ($item in @($list.items)) {
			if ($null -eq $item) { continue }
			$namespace = [string]$item.metadata.namespace
			if (-not $namespace.StartsWith($prefix, [System.StringComparison]::Ordinal)) { continue }
			$gameYaml = ''
			$dataProperty = $item.PSObject.Properties['data']
			if ($null -ne $dataProperty -and $null -ne $dataProperty.Value) {
				$gameProperty = $dataProperty.Value.PSObject.Properties['game_config.yaml']
				if ($null -ne $gameProperty) { $gameYaml = [string]$gameProperty.Value }
			}
			if ([string]::IsNullOrWhiteSpace($gameYaml)) {
				throw "线上 namespace $namespace 的 ConfigMap node-config 没有 data.game_config.yaml:不是本脚本生成的形状,判不出该 zone 的 scene 是否消费收件箱,拒绝部署;尚未做任何写操作。"
			}
			$scalars = (ConvertFrom-YamlToFlatMap -Text $gameYaml).Scalars
			$value = if ($scalars.Contains('player_inbox.consumer_enabled')) { ([string]$scalars['player_inbox.consumer_enabled']).Trim() } else { '' }
			$zones.Add([pscustomobject]@{ Namespace = $namespace; Value = $value })
		}
		$facts.ZoneConsumers = $zones.ToArray()
		$onCount = @($zones | Where-Object { $_.Value -ceq 'true' }).Count
		$notes.Add("zones_consumer_enabled=$onCount/$($zones.Count)")
	}
	$facts.Note = $notes -join ','
	return $facts
}

# 本次是否部署全局池 Go 服务(Apply-Infra → Apply-GlobalGoSvcManifests 的同一组开关:infra-up,或不带 -SkipInfra 的 all-up;
# 且没有 -SkipGoSvc、给了 -GoSvcRegistry)。玩家收件箱运维口令的解析用这一处判定(Initialize-InjectedSecrets)。
function Test-GlobalGoSvcDeployed {
	return (($Command -eq "infra-up" -or ($Command -eq "all-up" -and -not $SkipInfra)) -and
		-not $SkipGoSvc -and -not [string]::IsNullOrWhiteSpace($GoSvcRegistry))
}

# 削峰(G4b)的部署计划:写路径入口打一行「两个开关的解析结果 + 起步值」,并对四种容易被忽略的状态告警(都不阻断)。
# 警告只在本次确实部署对应服务时打(infra-up 不部署 gateway / login)。警告文本以不含空格、四条互不相同且互不为前缀的
# 「[PeakShaving]Xxx=yyy」开头(GatewayRateLimit=off / LoginQueue=off / LoginQueueGate=closed / GatewayTrustedProxies=unset):
# Write-Warning 可能在空格处折行,契约测试只匹配这个开头,改措辞要同步改测试。控制台输出用英文,与本脚本其余 Write-Host / Write-Warning 一致。
# 已有 Ingress 却不给 -GatewayTrustedProxies 的情形由入口预检直接拒绝(Assert-ClientEntryDeployPreflight),这里不重复。
function Show-PeakShavingPlan {
	$t = $script:PeakShavingLaunchTier
	$gw = if ($script:GatewayRateLimitOn) { 'on' } else { 'off' }
	$lq = if ($script:LoginQueueOn) { 'on' } else { 'off' }
	Write-Host "PeakShaving: gateway_rate_limit=${gw}(-GatewayRateLimit $GatewayRateLimit) login_queue=${lq}(-LoginQueue $LoginQueue) profile=$ReleaseProfile initial values (pending stress-test validation) zone=$($t.ZoneDefaultRps)/s burst=$($t.ZoneDefaultBurst) ip=$($t.IpRps)/s burst=$($t.IpBurst) cooldown=$($t.AccountCooldownMs)ms queue_capacity=$($t.LoginQueueZoneCapacity)/zone"
	# 生成器自检提前到这里(本函数先于 Initialize-InjectedSecrets 与任何 kubectl 写操作)。两个生成器本来要到 Apply-Zone 中途才求值,
	# 那时同一 zone 的 node ConfigMap / gate / scene 已经 apply 过了(all-up 还先跑了 Apply-Infra),这时 throw 就留下半截部署:
	#   - Get-GatewayRateLimitSettings:限流阈值「rps ≥ 1、burst ≥ rps、冷却非负」;
	#   - New-LoginQueueYaml:排队容量在 1..uint32 内,且 go/login/etc/login.yaml 的 7 个 Queue.* 镜像键都读得到(缺一个就 throw)。
	#     这两项与 zone 无关,求一次就够,产物丢弃。Max(1, …):all-up 不用 -ZoneId,顺手带了 -ZoneId 0 也不该在这里误报「ZoneId 必须为正」;
	#     zone-up 的 ZoneId < 1 照旧在生成 login 段时报。
	if ($script:GatewayRateLimitOn -and (Test-ZoneGatewayDeployed)) {
		$null = Get-GatewayRateLimitSettings
	}
	if ($script:LoginQueueOn -and (Test-ZoneLoginDeployed)) {
		$null = New-LoginQueueYaml -CurrentZoneId ([Math]::Max(1, $ZoneId))
	}
	if ($ReleaseProfile -ne 'dev' -and -not $script:GatewayRateLimitOn -and (Test-ZoneGatewayDeployed)) {
		Write-Warning "[PeakShaving]GatewayRateLimit=off(ReleaseProfile=${ReleaseProfile}):gateway rate limiting disabled. Use only for incident mitigation or baseline stress tests; this flag is not persistent and the next deployment enables it unless overridden."
	}
	# 合入门槛把本来要开的排队关掉了:Resolve-PeakShavingSwitch 是不看门槛时的结论(非 dev 档 auto = 开),$script:LoginQueueOn 是
	# 套上门槛之后的结论(Resolve-LoginQueueSwitch)。门槛未清时 on 已在参数解析后 throw,所以这里只会是非 dev 档的 auto。
	# 这时打 LoginQueueGate=closed、不打下面的 LoginQueue=off:那条说的「不粘滞、下次部署按档位恢复」在门槛未清时不成立。显式 -LoginQueue off 仍打那条。
	$loginQueueClosedByGate = (Resolve-PeakShavingSwitch -Value $LoginQueue -ReleaseProfileName $ReleaseProfile) -and -not $script:LoginQueueOn
	if ($ReleaseProfile -ne 'dev' -and $loginQueueClosedByGate -and (Test-ZoneLoginDeployed)) {
		Write-Warning "[PeakShaving]LoginQueueGate=closed(ReleaseProfile=${ReleaseProfile}):login queue disabled because its merge gate has not been cleared (`$script:LoginQueueMergeGateCleared = `$false in tools/scripts/k8s_deploy.ps1). The queue stays off in every profile until all three gate items in docs/handoff/launch-readiness-20261013/spec_G4b.md section 7 are met (G1b multi-zone tests green, G4b-G green, real-Redis smoke from spec_G4b-G.md section 8.4 recorded in docs/PROGRESS.md) and the owner sets that constant to `$true. Do not set Queue.Enabled by hand meanwhile."
	}
	if ($ReleaseProfile -ne 'dev' -and -not $script:LoginQueueOn -and -not $loginQueueClosedByGate -and (Test-ZoneLoginDeployed)) {
		Write-Warning "[PeakShaving]LoginQueue=off(ReleaseProfile=${ReleaseProfile}):login queue disabled; queued players retry the fast path after their queue_token becomes EXPIRED. This flag is not persistent."
	}
	if ($script:GatewayRateLimitOn -and (Test-ZoneGatewayDeployed) -and (ConvertTo-ClientEntryCidrList -CommaSeparated $GatewayTrustedProxies).Count -eq 0) {
		Write-Warning "[PeakShaving]GatewayTrustedProxies=unset(GatewayRateLimit=on):gateway rate limiting is enabled without trusted proxies (-GatewayTrustedProxies). IP buckets use the socket peer; robots connecting directly inside the cluster share a bucket per Pod IP ($($t.IpRps)/s, burst $($t.IpBurst)). An unregistered L7 proxy makes all players share one bucket."
	}
}

<#
.SYNOPSIS
	集群外客户端入口的部署前校验,写操作入口调用,早于任何集群写操作。两步,任何一步失败都 throw:
	  1. 参数组合:lib Test-ClientEntryPreflight,并入本脚本 -RequireClientEndpoint 的矛盾检查,一次报全;
	  2. 集群现状(Assert-ClientEntryClusterState,DryRun 跳过):会踢人、会静默改写入口的情形,battle 与各 zone 一次判定完。
	battle 工作负载的渲染(New-BattleWorkloadManifest)不在这里,而在 Apply-Infra 开头(仍早于任何基础设施写操作):
	Pod 模板要带 zone Redis 口令指纹,口令到 Initialize-InjectedSecrets 才解析,而本函数排在密钥解析之前。

.DESCRIPTION
	必须在 Apply-OpsProfileDefaults 之后调用:GateServiceType 可能已被 OpsProfile 改写。
	-Zones 只收本次真正要部署的 zone:zone-up 一个(-GateNodePortBase 视为显式值),all-up 取 zones 配置
	(是否显式写了 gateNodePortBase 由 Get-ZonesFromJson 记下),infra-up 为空(跳过 gate 校验)。
	Agones CRD 只在本次要以 agones 编排部署 battle 时探测,DryRun 跳过探测(ingress_final WP9);
	探测失败(集群不可达、Forbidden)由库函数 throw —— 查不到不等于"没装"。
	不受 -SkipPreflight 影响:那个开关只跳过发布预检,这里拦的是会生成错误清单或踢人的组合。
#>
function Assert-ClientEntryDeployPreflight {
	$deploysInfra = ($Command -eq "infra-up") -or ($Command -eq "all-up" -and -not $SkipInfra)

	# FromZonesConfig 只给报错文本用:保持现状的 nodePort 起点要写在命令行(zone-up)还是 zones 配置(all-up)。
	$zones = @()
	if ($Command -eq "zone-up") {
		$zones += [pscustomobject]@{ Name = $ZoneName; GateReplicas = $GateReplicas; GateNodePortBase = $GateNodePortBase; GateNodePortBaseExplicit = $true; FromZonesConfig = $false }
	}
	elseif ($Command -eq "all-up") {
		foreach ($zone in (Get-ZonesFromJson -Path (Resolve-ZonesConfigPath))) {
			$zones += [pscustomobject]@{
				Name                     = $zone.name
				GateReplicas             = $zone.gate
				GateNodePortBase         = $zone.gateNodePortBase
				GateNodePortBaseExplicit = $zone.gate_node_port_base_explicit
				FromZonesConfig          = $true
			}
		}
	}

	$deploysBattle = $deploysInfra -and $BattleReplicas -gt 0
	$deploysGateway = Test-ZoneGatewayDeployed
	$deploysLogin = Test-ZoneLoginDeployed

	$agonesFleetCrdPresent = $null
	if ($deploysBattle -and $BattleOrchestrator -eq "agones" -and -not $DryRun) {
		$agonesFleetCrdPresent = Test-AgonesFleetCrdPresent -KubeContext $KubeContext -KubeConfig $KubeConfig
	}

	$result = Test-ClientEntryPreflight -ClientEntryMode $ClientEntryMode -GateServiceType $GateServiceType -Zones $zones `
		-GateClientHostTemplate $GateClientHostTemplate -ClientPublicHost $ClientPublicHost `
		-GateExternalTrafficPolicy $GateExternalTrafficPolicy `
		-DeploysBattle $deploysBattle -BattleOrchestrator $BattleOrchestrator -AgonesFleetCrdPresent $agonesFleetCrdPresent `
		-DeploysGateway $deploysGateway -GatewayIngressHost $GatewayIngressHost -GatewayIngressTlsSecret $GatewayIngressTlsSecret `
		-GatewayTrustedProxies $GatewayTrustedProxies `
		-LoginDevPasswordAuth $LoginDevPasswordAuth.IsPresent -ReleaseProfile $ReleaseProfile

	# -RequireClientEndpoint 是本脚本的参数(库只给 auto 的取值规则),矛盾检查并进同一份结果,一次报全。
	$extraErrors = @()
	$extraWarnings = @()
	if ($ClientEntryMode -eq "podip" -and $RequireClientEndpoint -eq "true") {
		$extraErrors += "-RequireClientEndpoint true 与 -ClientEntryMode podip 矛盾:podip 的 gate 从不自报客户端地址,login / scene_manager 会跳过全部 gate,登录与跨 zone 跳转全部失败。"
	}
	if ($ClientEntryMode -eq "external" -and $RequireClientEndpoint -eq "false") {
		$extraWarnings += "-ClientEntryMode external 配 -RequireClientEndpoint false:没自报地址的 gate 会回落下发集群内 PodIP。只用于 ingress_final §6 第 2 批逐个 zone 切换的窗口,全部 zone 切完后回到 auto。"
	}
	Assert-ClientEntryPreflightResult -Result ([pscustomobject]@{
		Errors   = [string[]](@($result.Errors) + $extraErrors)
		Warnings = [string[]](@($result.Warnings) + $extraWarnings)
	})

	if ($LoginDevPasswordAuth -and -not $deploysLogin) {
		Write-Warning "[client-entry] -LoginDevPasswordAuth supplied, but login is not deployed in this run (only generated with Go services during zone-up / all-up; requires -GoSvcRegistry without -SkipGoSvc); flag has no effect."
	}
	# login 第三方登录(微信 / QQ)参数的形状门禁与 -LoginDevPasswordAuth 同在这里、排在密钥解析之前:非法 AppId / Endpoint 报的是参数原因,
	# 而不是「缺 MMORPG_LOGIN_WECHAT_APP_SECRET」;也排在下面只读集群的现状预检之前(纯参数检查不该先等集群)。
	Assert-LoginAuthProviderParams -DeploysLogin $deploysLogin

	if ($DryRun) {
		Write-Host "[dry-run] Skipping current cluster-state preflight (gate / battle modes and address sources, gateway Ingress); actual execution checks before any writes."
		if ($deploysLogin) {
			Write-Host "[dry-run] Skipping login third-party auth (AuthProviders / Secret $($script:LoginOAuthSecretName)) cluster-state preflight; actual execution checks before any writes."
		}
	}
	else {
		Assert-ClientEntryClusterState -Zones $zones -DeploysBattle $deploysBattle -DeploysGateway $deploysGateway `
			-AgonesFleetCrdPresent $agonesFleetCrdPresent
		# 第三方登录防误关闸:只在本次部署 login 时查(infra-up、-SkipGoSvc、k8s_image.ps1 路径都不碰 login 的 ConfigMap 与 Secret)。
		if ($deploysLogin) {
			Assert-LoginAuthProviderClusterState -Zones $zones
		}
	}
	Write-Host "Client entry: mode=$ClientEntryMode battle_orchestrator=$BattleOrchestrator gate_service_type=$GateServiceType gate_etp=$GateExternalTrafficPolicy require_client_endpoint=$(Resolve-RequireClientEndpoint)($RequireClientEndpoint) ingress_host=$(if ($GatewayIngressHost) { $GatewayIngressHost } else { '<none>' }) login_dev_password_auth=$($LoginDevPasswordAuth.IsPresent) login_wechat=$(-not [string]::IsNullOrEmpty($LoginWeChatAppId)) login_qq=$(-not [string]::IsNullOrEmpty($LoginQQAppId)) allow_disruptive_switch=$($AllowDisruptiveSwitch.IsPresent)"
}

# login / scene_manager ConfigMap 的 RequireClientEndpoint 取值(见参数 -RequireClientEndpoint):auto 跟随 -ClientEntryMode
# (库 Get-ClientEntryRequireClientEndpoint 是 auto 规则的唯一真相),true / false 为运维在上线窗口里的显式覆盖。
function Resolve-RequireClientEndpoint {
	if ($RequireClientEndpoint -eq "auto") {
		return Get-ClientEntryRequireClientEndpoint -ClientEntryMode $ClientEntryMode
	}
	return $RequireClientEndpoint
}

<#
.SYNOPSIS
	集群现状预检(非 DryRun,Assert-ClientEntryDeployPreflight 第 3 步):本次参数会踢人、或会静默改写正在服务的
	客户端入口时,在任何写操作之前拒绝,所有问题一次列全。

.DESCRIPTION
	模式与地址参数都不粘滞,漏传就落回默认值;下面几类情形 apply 不会报错,玩家却会大面积连不上:
	  1. 换 kind 的切换(gate Deployment ↔ StatefulSet、battle Deployment ↔ Fleet):库 Find-ClientEntryObsoleteResources
	     给出拒绝文本,与 Remove-ClientEntryObsoleteResources 的闸是同一份判据与文本(删除前那里还会再判一次,纵深防御:
	     两次之间集群可能被别人改动)。提前到这里,是为了 all-up 下 battle 与
	     各 zone 一次判定完,不会出现 infra 已重写、battle 已翻过去,排在后面的 zone 才被拒的半截部署。
	  2. battle 同 kind 内换 -ClientEntryMode 或换客户端主机(Get-BattleEntryDrift)。
	  3. external gate 的地址来源变化(Get-GateAddressDrift)。
	  1–3 给了 -AllowDisruptiveSwitch 就只打警告放行(1 的删除由 Remove-ClientEntryObsoleteResources 告警)。
	  4. 本次部署 gateway 却没有 trusted proxies,而 zone namespace 里已有 Ingress gateway:ConfigMap 会被重写成不含
	     trusted-proxies,gateway Pod 重建后全体玩家共用 Ingress controller 的一个限流桶(D91)。与切换无关,
	     不受 -AllowDisruptiveSwitch 豁免。
	任何 kubectl 失败(集群不可达、Forbidden)都 throw:查不到不等于不存在(fail-closed)。
#>
function Assert-ClientEntryClusterState {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Zones,
		[Parameter(Mandatory = $true)][bool]$DeploysBattle,
		[Parameter(Mandatory = $true)][bool]$DeploysGateway,
		# Test-AgonesFleetCrdPresent 的结果;$null = 静态 preflight 没探测(非 agones),这里按需再探测一次。
		[AllowNull()][object]$AgonesFleetCrdPresent = $null
	)

	$problems = @()
	# 会踢人 / 会改写入口的漂移:没给 -AllowDisruptiveSwitch 就并入 $problems,给了只打警告。
	# Get-*Drift 整体返回数组，先接收再追加，避免把空数组也当成一个问题。
	$drifts = @()

	if ($DeploysBattle) {
		if ($null -eq $AgonesFleetCrdPresent) {
			$AgonesFleetCrdPresent = Test-AgonesFleetCrdPresent -KubeContext $KubeContext -KubeConfig $KubeConfig
		}
		if (-not $AllowDisruptiveSwitch) {
			$probe = Find-ClientEntryObsoleteResources -Namespace $InfraNamespace -KicksPlayersOnly `
				-Resources (Get-BattleObsoleteResources -BattleOrchestrator $BattleOrchestrator) -AgonesFleetCrdPresent $AgonesFleetCrdPresent `
				-KubeContext $KubeContext -KubeConfig $KubeConfig
			$problems += @($probe.Refusals)
		}
		$battleDrifts = Get-BattleEntryDrift -AgonesFleetCrdPresent $AgonesFleetCrdPresent
		$drifts += $battleDrifts
	}

	# 不传 -Settings 的 New-GatewayRateLimitYaml 为空 = 本次 gateway ConfigMap 不含 trusted-proxies。只看 trusted-proxies 是否为空;削峰阈值(G4b)另由 -Settings 写,不参与本判定。
	# 非法 CIDR 已被静态 preflight 拒绝,这里不会 throw。
	$gatewayLosesTrustedProxies = $DeploysGateway -and [string]::IsNullOrEmpty((New-GatewayRateLimitYaml -TrustedProxies $GatewayTrustedProxies))
	foreach ($zone in $Zones) {
		$namespace = Get-ZoneNamespace -Name $zone.Name
		if (-not $AllowDisruptiveSwitch) {
			$probe = Find-ClientEntryObsoleteResources -Namespace $namespace -KicksPlayersOnly `
				-Resources (Get-GateObsoleteResources -ClientEntryMode $ClientEntryMode -GateReplicas $zone.GateReplicas) -AgonesFleetCrdPresent $AgonesFleetCrdPresent `
				-KubeContext $KubeContext -KubeConfig $KubeConfig
			$problems += @($probe.Refusals)
		}
		if ($ClientEntryMode -eq "external") {
			$gateDrifts = Get-GateAddressDrift -Zone $zone -Namespace $namespace
			$drifts += $gateDrifts
		}
		if ($gatewayLosesTrustedProxies) {
			$ingress = Invoke-ClientEntryKubectl -KubectlArgs @("get", "ingress", "gateway", "-n", $namespace, "--ignore-not-found", "-o", "name") `
				-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput
			if (-not [string]::IsNullOrWhiteSpace($ingress.Stdout)) {
				$problems += "zone $($zone.Name)(namespace=$namespace):已有 Ingress gateway,本次部署 gateway 却没给 -GatewayTrustedProxies —— java-svc-gateway-config 会被重写成不含 gate.rate-limit.trusted-proxies,gateway Pod 重建后把 Ingress controller 的地址当成所有玩家的来源 IP,全体玩家共用一个限流桶,assign-gate / login 大面积 429(D91)。请照传 -GatewayTrustedProxies,或先手动 kubectl -n $namespace delete ingress gateway。"
			}
		}
	}

	if ($AllowDisruptiveSwitch) {
		foreach ($drift in $drifts) {
			Write-Warning "[client-entry] -AllowDisruptiveSwitch supplied; allowed: $drift"
		}
	}
	else {
		$problems += $drifts
	}
	if ($problems.Count -gt 0) {
		throw ("集群现状预检失败($($problems.Count) 项),未做任何写操作:`n  - " + ($problems -join "`n  - "))
	}
}

# pod spec(kubectl -o json 的解析结果)里指定容器某个 env 的字面值;容器或变量缺席返回 $null。
# 值为空串的 env 由 API server 省略 value 字段,读出来同样是 $null:调用方按 [string] 取值时两者都是空串,
# 与启动 shell 里 [ -n "$X" ] 的口径一致。本脚本未开 StrictMode,缺席的 JSON 字段按 $null 读。
function Get-PodSpecEnvValue {
	param(
		[AllowNull()]$PodSpec,
		[Parameter(Mandatory = $true)][string]$ContainerName,
		[Parameter(Mandatory = $true)][string]$EnvName
	)

	if ($null -eq $PodSpec) { return $null }
	foreach ($container in @($PodSpec.containers)) {
		if ($null -eq $container -or $container.name -cne $ContainerName) { continue }
		foreach ($entry in @($container.env)) {
			if ($null -ne $entry -and $entry.name -ceq $EnvName) { return $entry.value }
		}
		return $null
	}
	return $null
}

<#
.SYNOPSIS
	battle 同 kind 内的入口漂移:读集群里本次要 apply 的那一种 battle 工作负载(Fleet 或 Deployment),按容器 env 判定
	它现在的客户端入口形态与主机覆盖,与本次参数比对。不存在(首次部署 / 换 kind)或一致时返回空数组。

.DESCRIPTION
	env 名与取值一律取自库 Get-ClientEntryContract(与生成器同一份):CLIENT_ENDPOINT_SOURCE 为 agones / static = external,
	none / 缺省 = podip(改造前的 Deployment 模板不写它);CLIENT_ENDPOINT_HOST 为空或 hostPort 形态的 $(HOST_IP) = 没有主机覆盖。
	换 kind 的切换由 Find-ClientEntryObsoleteResources 负责,这里只管同 kind:apply 不会报错,但 Fleet 滚动后新开的房间
	改按新形态下发地址(external → podip 时集群外客户端全部连不上新房间),Deployment 滚动重建、在打的局全部作废。
#>
function Get-BattleEntryDrift {
	param([AllowNull()][object]$AgonesFleetCrdPresent)

	$contract = Get-ClientEntryContract
	if ($BattleOrchestrator -eq "agones") {
		# 没装 Agones 已被静态 preflight 拒绝,走不到这里;防御性地当作不存在。
		if (-not $AgonesFleetCrdPresent) { return ,@() }
		$resource = $contract.AgonesFleetResource
		$shape = "fleet battle"
	}
	else {
		$resource = "deployment"
		$shape = "deployment battle"
	}
	$live = Invoke-ClientEntryKubectl -KubectlArgs @("get", $resource, "battle", "-n", $InfraNamespace, "--ignore-not-found", "-o", "json") `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput
	if ([string]::IsNullOrWhiteSpace($live.Stdout)) { return ,@() }
	$workload = $live.Stdout | ConvertFrom-Json
	# Fleet:spec.template(GameServer 模板).spec.template(Pod 模板).spec;Deployment:spec.template.spec。
	$podSpec = if ($BattleOrchestrator -eq "agones") { $workload.spec.template.spec.template.spec } else { $workload.spec.template.spec }

	$source = [string](Get-PodSpecEnvValue -PodSpec $podSpec -ContainerName "battle" -EnvName $contract.ClientEndpointSourceEnv)
	$liveMode = if ($source -cin @($contract.ClientEndpointSourceAgones, $contract.ClientEndpointSourceStatic)) { "external" } else { "podip" }
	if ($liveMode -ne $ClientEntryMode) {
		return ,@("battle(namespace=$InfraNamespace):当前 $shape 以 -ClientEntryMode $liveMode 运行($($contract.ClientEndpointSourceEnv)='$source'),本次参数会改成 $ClientEntryMode —— Fleet 滚动后新开的房间改按新形态下发地址(external → podip 时集群外客户端全部连不上新房间),Deployment 滚动重建、在打的局全部作废。要保持现状请显式传 -ClientEntryMode $liveMode;确认要切换请加 -AllowDisruptiveSwitch。")
	}
	if ($liveMode -eq "external") {
		$liveHost = [string](Get-PodSpecEnvValue -PodSpec $podSpec -ContainerName "battle" -EnvName $contract.ClientEndpointHostEnv)
		if ($liveHost -ceq $contract.BattleHostPortDefaultClientHost) { $liveHost = "" }
		if ($liveHost -ne $ClientPublicHost) {
			$keep = if ($liveHost) { "显式传 -ClientPublicHost $liveHost" } else { "不传 -ClientPublicHost" }
			return ,@("battle(namespace=$InfraNamespace):当前 $shape 的客户端主机覆盖是 '$liveHost'(空 = 取节点 / Agones 地址),本次 -ClientPublicHost 为 '$ClientPublicHost' —— 滚动后新开的房间改报新主机。要保持现状请$keep;确认要改请加 -AllowDisruptiveSwitch。")
		}
	}
	return ,@()
}

<#
.SYNOPSIS
	gate 自报地址的两个来源(D88)→ 可比较的描述 + "保持现状要传的参数"。取值口径同 lib New-GateStatefulSetYaml 的
	生成器内部 env 与启动 shell 前缀:主机按 模板 > CLIENT_PUBLIC_HOST > status.hostIP 取第一个非空的。
#>
function Get-GateAddressSource {
	param(
		[AllowEmptyString()][string]$PortMode = "",
		[AllowEmptyString()][string]$NodePortBase = "",
		[AllowEmptyString()][string]$ClientPort = "",
		[AllowEmptyString()][string]$HostTemplate = "",
		[AllowEmptyString()][string]$PublicHost = "",
		[Parameter(Mandatory = $true)][string]$ZoneName,
		# true = all-up(nodePort 起点写在 zones 配置),false = zone-up(写命令行)。
		[bool]$FromZonesConfig = $false
	)

	$contract = Get-ClientEntryContract
	if ($PortMode -ceq $contract.GateClientPortModeNodePort) {
		$port = "nodePort $NodePortBase+序号(NodePort)"
		$portHint = if ($FromZonesConfig) { "-GateServiceType NodePort,并在 zones 配置里给 zone $ZoneName 写 gateNodePortBase: $NodePortBase" } else { "-GateServiceType NodePort -GateNodePortBase $NodePortBase" }
	}
	elseif ($PortMode -ceq $contract.GateClientPortModeService) {
		$port = "Service 端口 $ClientPort(LoadBalancer)"
		$portHint = "-GateServiceType LoadBalancer"
	}
	else {
		$port = "未知形态($($contract.GateClientPortModeEnv)='$PortMode')"
		$portHint = "与现有 StatefulSet gate 的 env 一致的参数(先人工核对 kubectl get sts gate -o yaml)"
	}

	if ($HostTemplate) {
		$hostDesc = "主机模板 '$HostTemplate'"
		$hostHint = "-GateClientHostTemplate(本 zone 渲染 {zone} 后须为 '$HostTemplate')"
	}
	elseif ($PublicHost) {
		$hostDesc = "主机 '$PublicHost'"
		$hostHint = "-ClientPublicHost $PublicHost 且不传 -GateClientHostTemplate"
	}
	else {
		$hostDesc = "节点地址 status.hostIP"
		$hostHint = "既不传 -GateClientHostTemplate 也不传 -ClientPublicHost"
	}
	return [pscustomobject]@{ Port = $port; PortHint = $portHint; Host = $hostDesc; HostHint = $hostHint }
}

<#
.SYNOPSIS
	external gate 的地址来源漂移:集群里 StatefulSet gate 模板的生成器内部 env vs 本次地址计划(Resolve-ZoneGatePlan)。
	StatefulSet 不存在(首次切 external)或一致时返回空数组。

.DESCRIPTION
	端口与主机分开比,只比实际生效的来源:
	  - 端口(GATE_CLIENT_PORT_MODE + GATE_NODE_PORT_BASE | GATE_CLIENT_PORT):每序号 Service 会在本次 apply 时立刻改写,
	    而 updateStrategy 为 OnDelete,现有 Pod 仍自报旧端口 —— 该 zone 全部登录失败,且不报任何错,直到逐个排空重建;
	  - 主机(GATE_CLIENT_HOST_TEMPLATE > CLIENT_PUBLIC_HOST > status.hostIP):现有 Pod 仍报旧主机,之后任何重建(排空、
	    崩溃、节点故障)的 Pod 改报新主机,同一 zone 混用两套地址。
	比的是 StatefulSet 模板(最近一次 apply 的意图),不是逐个 Pod:OnDelete 下它就是下一批重建 Pod 要用的地址。
#>
function Get-GateAddressDrift {
	param(
		# Assert-ClientEntryDeployPreflight 的 zone 记录:Name / GateReplicas / GateNodePortBase / FromZonesConfig。
		[Parameter(Mandatory = $true)]$Zone,
		[Parameter(Mandatory = $true)][string]$Namespace
	)

	$live = Invoke-ClientEntryKubectl -KubectlArgs @("get", "statefulset", "gate", "-n", $Namespace, "--ignore-not-found", "-o", "json") `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput
	if ([string]::IsNullOrWhiteSpace($live.Stdout)) { return ,@() }
	$podSpec = ($live.Stdout | ConvertFrom-Json).spec.template.spec
	# env 名取自库契约(与 New-GateStatefulSetYaml 写入的同一份)。
	$contract = Get-ClientEntryContract
	$liveEnv = @{}
	foreach ($name in @($contract.GateClientPortModeEnv, $contract.GateNodePortBaseEnv, $contract.GateClientPortEnv,
			$contract.GateClientHostTemplateEnv, $contract.GateClientPublicHostEnv)) {
		$liveEnv[$name] = [string](Get-PodSpecEnvValue -PodSpec $podSpec -ContainerName "gate" -EnvName $name)
	}
	$plan = Resolve-ZoneGatePlan -CurrentZoneName $Zone.Name -Replicas $Zone.GateReplicas -NodePortBase $Zone.GateNodePortBase

	$fromZonesConfig = [bool]$Zone.FromZonesConfig
	$liveSource = Get-GateAddressSource -PortMode $liveEnv[$contract.GateClientPortModeEnv] -NodePortBase $liveEnv[$contract.GateNodePortBaseEnv] `
		-ClientPort $liveEnv[$contract.GateClientPortEnv] -HostTemplate $liveEnv[$contract.GateClientHostTemplateEnv] `
		-PublicHost $liveEnv[$contract.GateClientPublicHostEnv] -ZoneName $Zone.Name -FromZonesConfig $fromZonesConfig
	$planSource = Get-GateAddressSource -PortMode $plan.PortMode -NodePortBase ([string]$plan.NodePortBase) `
		-ClientPort ([string]$plan.ClientPort) -HostTemplate $plan.HostTemplate -PublicHost $plan.PublicHost `
		-ZoneName $Zone.Name -FromZonesConfig $fromZonesConfig

	$where = "zone $($Zone.Name)(namespace=$Namespace)"
	$drifts = @()
	if ($liveSource.Port -cne $planSource.Port) {
		$drifts += "${where}:gate 自报端口将从 $($liveSource.Port) 改为 $($planSource.Port)。StatefulSet 是 OnDelete,现有 gate Pod 仍自报旧端口,每序号 Service 却会在本次 apply 时立刻改写 —— 该 zone 全部登录失败,直到逐个排空重建。要保持现状请显式传 $($liveSource.PortHint);确认要改请加 -AllowDisruptiveSwitch,并随即用 k8s_gate_drain.ps1 逐个排空重建全部 gate。"
	}
	if ($liveSource.Host -ne $planSource.Host) {
		$drifts += "${where}:gate 自报主机将从 $($liveSource.Host) 改为 $($planSource.Host)。现有 Pod 仍报旧主机,之后任何重建(排空、崩溃、节点故障)的 Pod 改报新主机,同一 zone 混用两套地址。要保持现状请显式传 $($liveSource.HostHint);确认要改请加 -AllowDisruptiveSwitch,并随即用 k8s_gate_drain.ps1 逐个排空重建全部 gate。"
	}
	return ,$drifts
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

<#
.SYNOPSIS
	MMORPG_REDIS_PASSWORD 的字符集保护:口令要原样嵌进 YAML 双引号标量,含双引号、反斜杠、换行或其它控制字符就 throw。

.DESCRIPTION
	同一个值写进 C++ 的 node-config / battle-node-config(zoneredis.password)、各 Go 服务与 gateway 的 ConfigMap、infra 的
	Secret redis-auth,全部是 `"<值>"` 形式:双引号会提前结束标量,把 YAML 写坏;反斜杠是转义起始,`\n` 之类会被解析成别的字符,
	口令被静默改掉;换行与其它控制字符不能原样出现在双引号标量里;制表符还会被 Invoke-KubectlWithInputFile 换成 4 个空格。
	拒绝而不是转义:C++(yaml-cpp)、Go、Java 三边对转义序列的支持不必逐一核对,口令由运维生成,换一个不含这些字符的值即可。
	报错只点变量名,不回显值。空串放行:dev 档未注入 = 不认证;非 dev 档的空值由 Resolve-InjectedSecret 与 Get-CppZoneRedisPassword 拒绝。
	由 Initialize-InjectedSecrets 在任何写操作之前调用一次,Get-CppZoneRedisPassword 生成 C++ 配置时再查一次(纵深防御)。
#>
function Assert-RedisPasswordYamlSafe {
	param([Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()][string]$Password)

	if ([string]::IsNullOrEmpty($Password)) { return }
	if ($Password -match '["\\\p{Cc}  ]') {
		throw "MMORPG_REDIS_PASSWORD 含双引号、反斜杠、换行或其它控制字符:该口令要原样写进 node-config 等 ConfigMap 与 Secret redis-auth 的 YAML 双引号标量,会把 YAML 写坏或被转义改写。请换一个不含这些字符的口令(值不回显)。"
	}
}

<#
.SYNOPSIS
	C++ 节点(gate / scene / battle)连 zone Redis 的口令,带生成期保护。New-NodeConfigMapYaml 写 zoneredis.password 与
	Get-CppZoneRedisAuthAnnotations 算 Pod 模板指纹都从这里取,两边不可能取到不同的值。

.DESCRIPTION
	P0-A 第二批,方案 A(负责人 2026-10-15 定;docs/handoff/global-singletons-p0-findings-20261014.md「P0-A」修复设计第 6 条):
	口令明文进 node ConfigMap,与各 Go 服务 / gateway 的 ConfigMap 同一做法。改走 Secret 不缩小暴露面:同 namespace 能读这份
	ConfigMap 的人也能读那几份 Go / Java ConfigMap 里的同一个口令(G4b 给 gateway 写口令时的同一结论,见 New-JavaSvcConfigMapYaml)。
	C++ 第一批已认 game_config.yaml 的 zoneredis.password(环境变量 ZONE_REDIS_PASSWORD 可覆盖):非空即每条新连接先 AUTH,
	认证失败 fail-closed(zone Redis 对业务保持不可用,不放行未认证连接);空口令不发 AUTH。
	两道保护,写法仿 New-NodeConfigMapYaml 里 GateTokenSecret 的生成期 throw(宁可在生成期炸,也不产出一份连不上 Redis 的 ConfigMap):
	  1. 非 dev 档口令为空:该档 Redis 开 --requirepass(Apply-Infra 建 Secret redis-auth),C++ 不带口令就全线 NOAUTH。
	     写路径入口的 Resolve-InjectedSecret 已先拒绝,这里是纵深防御;
	  2. 字符集(Assert-RedisPasswordYamlSafe):值要原样嵌进 `password: "<值>"`。
	dev 档未注入时返回空串:node-config 写 `password: ""`、不写指纹注解,与 P0-A 第二批之前逐字相同。
	DryRun 会把整份 ConfigMap 连口令一起打出来,与 Go 服务 ConfigMap、Secret redis-auth 的既有行为相同。
#>
function Get-CppZoneRedisPassword {
	$password = $script:RedisPassword
	if ($null -eq $password) {
		throw "生成 C++ 节点配置失败:zone Redis 口令未解析。请先调用 Initialize-InjectedSecrets(写操作路径会自动调用)。"
	}
	if ($password.Length -eq 0 -and $ReleaseProfile -ne 'dev') {
		throw "生成 C++ 节点配置失败:ReleaseProfile=$ReleaseProfile 要求 zone Redis 口令非空(MMORPG_REDIS_PASSWORD):该档 Redis 开了 requirepass,C++ 节点不带口令会全线 NOAUTH。"
	}
	Assert-RedisPasswordYamlSafe -Password $password
	return $password
}

<#
.SYNOPSIS
	C++ 节点 Pod 模板上的 zone Redis 口令指纹注解({ 注解名 = 指纹 })。有口令时一条,口令为空(dev 未注入)时是空表 → 模板不写该注解,
	与 P0-A 第二批之前逐字相同。New-CppPodAnnotationBlock(gate / scene / battle 的 Deployment、scene 的 Agones Fleet)与
	New-CppNodePodCommon(lib 的 gate StatefulSet、battle Fleet、battle hostPort Deployment)共用这一处,覆盖全部挂 /app/bin/etc 的工作负载。

.DESCRIPTION
	为什么要指纹:口令在 node-config / battle-node-config 里,而 C++ 只在进程启动时读一次配置(config.cpp readGameConfig)。
	只改 ConfigMap 时 Pod 模板逐字节没变,kubectl apply 是 no-op —— 开口令或换口令后,旧 Pod 拿旧值(或空口令)连 Redis,
	一直 NOAUTH / 认证被拒,直到有人手动滚动。指纹进模板后,口令一变模板就变,apply 自然滚动。
	**只哈希口令,不哈希整份 node-config**:node-config 里还有 Kafka / etcd / 号段等与口令无关的键(P0-B 等改动会动它们),
	哈希整份会让每一次无关改动都滚动 gate(踢人)与 battle(作废在打的局)。
	改口令会滚动 C++ Pod,这是期望行为(C++ 只在启动时读配置,不滚就一直用旧口令):
	  - gate / scene / battle 的 Deployment:apply 即滚动;gate 滚动 = 断开该 Pod 上的玩家,scene 滚动 = 玩家被迁走,
	    battle 滚动 = 作废该 Pod 上在打的局;
	  - Agones Fleet(scene / battle):新建 GameServerSet,已 Allocated 的实例不被打断,空闲后才退场(battle 另有排空标签);
	    这些旧实例一直用旧口令,Redis 开口令 / 换口令重启后会认证失败或 NOAUTH 直到退出,上线前要先删掉(README 上线步骤第 3 步前置);
	  - external 的 gate StatefulSet 是 OnDelete:模板换了不自动重建,按 k8s_gate_drain.ps1 逐个排空删 Pod 才换上新口令。
	所以开口令 / 换口令要进维护窗口,步骤见 deploy/k8s/README.md「C++ 节点的 zone Redis 口令」。
	指纹用 Get-InjectedSecretFingerprint(用途串 zone-redis/cpp-node 做域分隔、只截 48 位),暴露面同该函数的说明;
	口令本身已在同 namespace 的 ConfigMap 里明文可读,指纹不扩大暴露面。
#>
function Get-CppZoneRedisAuthAnnotations {
	$annotations = [ordered]@{}
	$password = Get-CppZoneRedisPassword
	if ($password.Length -gt 0) {
		$annotations[$script:ZoneRedisAuthHashAnnotation] = Get-InjectedSecretFingerprint -SecretName 'zone-redis/cpp-node' -Value $password
	}
	return $annotations
}

<#
.SYNOPSIS
	校验 C++ 节点 LogLevel 的结构并返回整数:只认 0..5(muduo Logger::LogLevel 的 TRACE..FATAL),其余一律 throw。

.DESCRIPTION
	纯函数,New-NodeConfigMapYaml 用它把 bin/etc/base_deploy_config.yaml 的 LogLevel 镜像进 node-config / battle-node-config;
	契约测试按 AST 抽出来直接喂非法值。生产下界(>= 2,不开 DEBUG / TRACE)是发布策略,由 release_preflight.ps1 判,不在这里。
#>
function ConvertTo-CppNodeLogLevel {
	param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value)

	$level = 0
	if (-not [int]::TryParse($Value, [ref]$level) -or $level -lt 0 -or $level -gt 5) {
		throw "生成 node ConfigMap 失败:bin/etc/base_deploy_config.yaml 的 LogLevel 必须是 0..5 的整数(muduo 枚举 0=TRACE 1=DEBUG 2=INFO 3=WARN 4=ERROR 5=FATAL;当前 '$Value')。"
	}
	return $level
}

function New-NodeConfigMapYaml {
	param(
		[Parameter(Mandatory = $true)][int]$CurrentZoneId,
		[Parameter(Mandatory = $true)][int]$CurrentClusterId,
		[Parameter(Mandatory = $true)][string]$ConfigName,
		[switch]$IncludeBattleSettings
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
	# 与 topic-init 的预建值同源，挂载 ConfigMap 后不能退回 C++ 默认的 g1/旧分区数。
	$commandTopicPartitions = Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.CommandTopicPartitions'
	$commandTopicGeneration = Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.CommandTopicGeneration'
	if ([int]$commandTopicPartitions -le 0 -or [int]$commandTopicGeneration -le 0) {
		throw "生成 node ConfigMap 失败:Kafka.CommandTopicPartitions / CommandTopicGeneration 必须为正数。"
	}
	# Kafka.GroupID 同样从 bin/etc 镜像,不在这里写常数(P0-B 批 2,docs/handoff/global-singletons-p0-findings-20261014.md)。
	# 2026-10-15 之前模板写死 "game-consumer-group",而 bin/etc 自 2026-05-27 起就是空串 —— 这份 ConfigMap 整目录遮蔽
	# 镜像里的 bin/etc,生成器自带的常数就是漂移来源。后果有二:
	#   1) 旧二进制的 KafkaManager::Init 在 GroupID 非空时不给生产端登记 broker,只生产不消费的 battle 每次 send() 都被拒
	#      (C++ 侧已在 P0-B 批 1 改为无条件登记;本处让旧二进制在新模板下也恢复);
	#   2) gate / scene 的旧路径(一节点一 topic)用它当组名,全服挤进同一个消费组。
	# 空串是合法值(本机就是空,C++ 旧路径据此回落 <类型>-group-<node_id>):Get-AuthoritativeScalar 只在键缺席时 throw,
	# GroupID: "" 读出来就是空串。值要原样嵌进下面模板的双引号 YAML 标量,只放行 Kafka 名字常用的字符集,
	# 引号、反斜杠、空白等会把 YAML 写坏或改变语义,当场拒绝(fail-closed)。
	$kafkaGroupId = Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.GroupID'
	if ($kafkaGroupId -notmatch '^[A-Za-z0-9._-]*$') {
		throw "生成 node ConfigMap 失败:bin/etc/base_deploy_config.yaml 的 Kafka.GroupID 只允许字母、数字、'.'、'_'、'-'(当前 '$kafkaGroupId')。"
	}
	# Kafka.DisableLegacyPerNodeTopic 在 K8s 上直接写 true(模板常数,刻意不从 bin/etc 镜像),与上面的 GroupID 镜像同批:
	#   - 全仓已无旧 topic(<类型>-<node_id>)的生产者:C++ 走 ResolveCommandRoute,Go 走 shared/kafkacmd,Java gateway 不连 Kafka
	#     (核实记录见 deploy/k8s/README.md「控制面命令 topic」一节的迁移窗口条);
	#   - GroupID 改空后旧路径的组名从共享组变成 <类型>-group-<node_id>,新组没有已提交位点,旧路径开着就会从 earliest
	#     重放旧 topic 里保留期内的消息(非空且不匹配的 target_instance_id 会被 ValidateCommandTarget 滤掉,空实例号的不会);
	#     关掉旧路径则 GroupID 在 K8s 上再无 C++ 读者。
	#   - 不镜像:bin/etc 那份是本机的迁移窗口开关,仍为 false,由运维确认旧 topic 写入与积压为 0 后显式翻;镜像会让一次改动
	#     同时翻本机与所有 K8s 环境。
	# 取值待实测 / 压测复核:上线前按 README 那一条确认线上 login 已写新 topic、旧 topic 无积压;回退 = 把下面这一行改回 false
	# 并重启 gate / scene(改 ConfigMap 不会自动滚 Pod)。
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
	# zone Redis 口令(P0-A 第二批,方案 A:明文进 ConfigMap;取值与两道生成期保护见 Get-CppZoneRedisPassword)。C++ 只在启动时
	# 读这份配置:只改 ConfigMap 不会让进程重读,口令变了靠 Pod 模板上的指纹注解(Get-CppZoneRedisAuthAnnotations)滚动。
	$zoneRedisPassword = Get-CppZoneRedisPassword

	$battleSettingsBlock = ''
	if ($IncludeBattleSettings) {
		# 只有 battle 池解析自己的密钥；zone/status/down 不因无关密钥而被阻断。
		$battleTokenSecret = Resolve-InjectedSecret -EnvName 'MMORPG_BATTLE_TOKEN_SECRET' `
			-DevFallback 'change-me-in-production-battle-ticket-shared-key' `
			-ReleaseProfile $ReleaseProfile -Purpose 'Battle 直连票据 HMAC 共享密钥' -MinLength 32
		# K8s 不注入 BATTLE_RUN_MODE，运行时按 prod 执行，dev 发布也必须满足其启动门禁。
		if ([System.Text.Encoding]::UTF8.GetByteCount($battleTokenSecret.Trim()) -lt 32) {
			throw '生成 battle ConfigMap 失败:MMORPG_BATTLE_TOKEN_SECRET 去除首尾空白后必须至少 32 字节。'
		}
		if ($battleTokenSecret.Trim() -ceq $gateTokenSecret.Trim()) {
			throw '生成 battle ConfigMap 失败:MMORPG_BATTLE_TOKEN_SECRET 必须与 MMORPG_GATE_TOKEN_SECRET 不同。'
		}
		$battleMaxConnections = Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'BattleMaxConnections'
		if ([int]$battleMaxConnections -lt 1 -or [int]$battleMaxConnections -gt 65535) {
			throw '生成 battle ConfigMap 失败:BattleMaxConnections 必须在 1..65535。'
		}
		$battleSettingsBlock = "BattleTokenSecret: `"${battleTokenSecret}`"`nBattleMaxConnections: ${battleMaxConnections}"
	}

	# 永久 guid 号段。整块从权威文件原样搬运,而不是在这里抄一份常数:
	# 键名 / 结构必须与 C++ 读法逐字对上(见下面模板处的注释),抄一份就是等着漂移。
	$idSegmentsBlock = Get-AuthoritativeYamlBlock -RelativePath 'bin/etc/base_deploy_config.yaml' -Key 'IdSegments'

	# C++ unary gRPC 调用的 deadline(docs/design/grpc-muduo-transport.md §16.4)。与 IdSegments 同法整块搬运:
	# 这份 ConfigMap 遮蔽镜像里的 bin/etc,不搬 = K8s 上所有目标都落回 C++ 内置默认 10000,而号段 fetchTimeout、
	# 换图在途 TTL 都从 deadline 派生,会跟着偏离仓库口径。预算不等式(≥ 服务端 Timeout + 2000)由写路径入口的
	# Assert-GrpcClientDeadlineBudget 核过,这里只负责逐字搬运;缺 GrpcClient 块则生成期直接 throw(fail-closed)。
	$grpcClientBlock = Get-AuthoritativeYamlBlock -RelativePath 'bin/etc/base_deploy_config.yaml' -Key 'GrpcClient'

	# 审计 topic 世代号。**真源取 data_service 那份而不是 bin/etc 那份**:C++ 是
	# transaction_log / player_snapshot 唯一的生产者,go/data_service 是唯一的消费者,
	# 消费者那边的 Kafka.TopicGeneration 同时还喂着 Apply-KafkaTopicInitJob 预建 topic 的名字
	# (见 $dsKafkaTopicGeneration)。从同一个键派生,生产者、消费者、预建 Job 三边不可能各说各话。
	# 缺席 = 1:与 C++ NormalizeAuditTopicGeneration / Go topicForGeneration 的 0→1 同一条规则,
	# 所以这里不用 Get-AuthoritativeScalar(那个查不到就 throw),留给 yaml 可以不写这一键。
	$auditTopicGeneration = 1
	$auditGen = Get-YamlScalar -Path (Join-Path $RepoRoot ('go/data_service/etc/data_service.yaml' -replace '/', [System.IO.Path]::DirectorySeparatorChar)) -KeyPath 'Kafka.TopicGeneration'
	if ($auditGen.Found -and -not [string]::IsNullOrWhiteSpace($auditGen.Value)) {
		$auditTopicGeneration = $auditGen.Value
	}

	# 玩家存盘 DBTask topic 世代号(player-storage-placement.md §7)。**真源取 go/db 那份**:C++ scene
	# 按 home_zone 往 db_task_zone_{zone}[_g<N>] 写存盘,go/db 是唯一的消费者,go-svc-db ConfigMap 也是从
	# 同一个键镜像($dbTopicGeneration)。login 读写同一组 topic,三方必须相等,这里顺带钉住 login ——
	# 分家不报任何错:换代后 scene 仍写旧代 topic,存盘静默积压在没人消费的旧 topic 里。
	# 与 AuditTopicGeneration 不同,这里用 Get-AuthoritativeScalar(查不到就 throw):go-svc-db ConfigMap
	# 本来就强制要这一键,不存在"yaml 可以不写"的窗口,宽容只会让两份 ConfigMap 各取各的默认值。
	$dbTaskTopicGeneration = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.TopicGeneration'
	$loginDbTaskTopicGeneration = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Kafka.TopicGeneration'
	$dbTaskGenNumber = 0L
	if (-not [long]::TryParse($dbTaskTopicGeneration, [ref]$dbTaskGenNumber) -or $dbTaskGenNumber -le 0 -or $dbTaskGenNumber -gt [uint32]::MaxValue) {
		throw "生成 node ConfigMap 失败:go/db/etc/db.yaml 的 ServerConfig.Kafka.TopicGeneration 必须是 1..4294967295 的整数(当前 '$dbTaskTopicGeneration')。"
	}
	if ($loginDbTaskTopicGeneration -cne $dbTaskTopicGeneration) {
		throw "生成 node ConfigMap 失败:go/db ServerConfig.Kafka.TopicGeneration($dbTaskTopicGeneration)与 go/login Kafka.TopicGeneration($loginDbTaskTopicGeneration)不一致;db_task topic 换代必须 login/db/C++ 三方同一次改。"
	}

	# 混版本持久字段保护:scene 严格提交屏障 C3 的模式(docs/design/mixed-version-persist-field-protection.md §5.2)。
	# 真源是 bin/etc/base_deploy_config.yaml(只有 C++ 读这一键)。这份 ConfigMap 整目录遮蔽镜像里的 bin/etc,
	# 不镜像 = C++ 读到缺键按 warn:仓库切到 enforce 时集群里悄悄不生效。只认 warn / enforce(区分大小写),
	# 其余在生成期就 throw —— C++ 读到非法值会拒绝启动,不能等 pod 起不来才发现。
	$persistSchemaRegressionMode = Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'PersistSchemaRegressionMode'
	if ($persistSchemaRegressionMode -cne 'warn' -and $persistSchemaRegressionMode -cne 'enforce') {
		throw "生成 node ConfigMap 失败:bin/etc/base_deploy_config.yaml 的 PersistSchemaRegressionMode 只能是 warn 或 enforce(当前 '$persistSchemaRegressionMode')。"
	}

	# C++ 节点日志级别。数值直接按 muduo Logger::LogLevel 枚举解释(0=TRACE 1=DEBUG 2=INFO 3=WARN 4=ERROR 5=FATAL;
	# node.cpp 的 Node::InitLogSystem 直接 static_cast,没有映射表)。真源是 bin/etc/base_deploy_config.yaml,
	# release_preflight.ps1 的 debug.cpp.loglevel 查的也是这一键,staging / prod 部署入口先跑预检(要求 >= 2)。
	# 2026-10-22 之前这里写死 1:K8s 上 gate / scene / battle 全部带着 DEBUG 跑,而预检只看 bin/etc 那份(2),两边各说各话。
	# 生成期只核结构(ConvertTo-CppNodeLogLevel),取值范围与 C++ 侧 cpp/libs/engine/config/node_log_level.h 相同(0..5 的整数);
	# 写法上这里更宽([int]::TryParse 放过 +2 / 首尾空白),但写进 ConfigMap 的是规整后的整数,C++ 一定接受。
	# C++ 读配置时对缺键 / 空值 / 非整数 / 越界一律拒启(2026-10-22 起;此前缺键落到 proto 默认 0 = TRACE、非整数抛 yaml-cpp 异常),
	# 这里先拦下,不必等 Pod 起不来才发现。这一键必须写出:这份 ConfigMap 整目录遮蔽镜像里的 bin/etc。
	$cppLogLevel = ConvertTo-CppNodeLogLevel -Value (Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'LogLevel')

	$baseDeployConfig = (@"
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  KeepaliveInterval: ${keepaliveInterval}
  NodeTTLSeconds: ${nodeTtlSeconds}
GateTokenSecret: "${gateTokenSecret}"
GateMaxConnections: ${gateMaxConnections}
${battleSettingsBlock}
# ClusterId:部署级常量,运维建集群时定一次(k8s_deploy.ps1 -ClusterId,默认 0),策划不碰。
# 它是 snowflake worker 段 [cluster5][node12] 的高 5 位(docs/design/node-id-overhaul-plan-20260908.md §5),
# C++ 读进 BaseDeployConfig.cluster_id;同一集群里各 Go 服务 ConfigMap 写的是同一个值。
# readBaseDeployConfig 按键名逐个取值,C++ 侧字段落地前多出这一键不会影响启动。
ClusterId: ${CurrentClusterId}
TableDataDirectory: "../generated/generated_tables/"
DataRootDirectory: "/app/"
# 从 bin/etc/base_deploy_config.yaml 镜像,muduo 枚举 0=TRACE 1=DEBUG 2=INFO 3=WARN 4=ERROR 5=FATAL;生成期已校验是 0..5 的整数。
LogLevel: ${cppLogLevel}
HealthCheckInterval: 1
service_discovery_prefixes:
  - "SceneNodeService.rpc"
  - "GateNodeService.rpc"
  - "LoginNodeService.rpc"
  # 与 bin/etc/base_deploy_config.yaml 对齐(2026-09-02 跨 zone 匹配审计发现此处只有三条):
  # 缺 MatchNodeService 时直连模式("0")的 gate 发现不到 match,JoinQueue 报 "Node not found ... message id: 157"。
  # BattleNodeService 已不供 gate 使用(两种模式下 gate 都不连 battle、不中继战斗,turn-based §22 D66);保留它是因为
  # battle-node-config 也由本模板生成,battle 按这些前缀 watch 自己的节点键(etcd_service 劫持检测与注册自检)。
  # 这份 ConfigMap 以只读整目录挂载覆盖镜像里的 bin/etc。
  - "SceneManagerNodeService.rpc"
  - "BattleNodeService.rpc"
  # 客户端 RPC 路由服(Go-Zero,全局池):GATE_CLIENT_RPC_ROUTER=1 时 gate 唯一的 gRPC 目标(契约 zone_contract_v1 §1)。
  # 与 bin/etc/base_deploy_config.yaml 对齐;-GateRouterMode 0 时 gate 发现到也不连,多这一条无副作用。
  - "ClientRpcRouterNodeService.rpc"
  - "MatchNodeService.rpc"
  # 全局数据服务(Go-Zero gRPC,全局池不分 zone):scene 经它调 AllocateIdSegment 领 GuidSegment 各 Kind 的号段。
  # data_service 按 C++ 约定注册 NodeInfo(go/data_service/internal/noderegistry);缺这一条 scene 永远过不了
  # DependencyGate。与 bin/etc/base_deploy_config.yaml 对齐。
  - "DataServiceNodeService.rpc"
  # 玩家持久收件箱(Go-Zero gRPC,infra namespace 全服一份):scene 经它 FetchItems / AckItems(docs/design/player-inbox-service.md §1.4)。
  # player_inbox 经 go/shared/noderegistry 注册 NodeInfo;弱依赖,不进 DependencyGate,缺这一条只是收件箱永远拉不到(IB-7)。
  # 与 bin/etc/base_deploy_config.yaml 对齐。
  - "PlayerInboxNodeService.rpc"
# 审计 topic 世代号:C++ 生产者按 `<基名>_g<N>` 拼实际 topic 名(基名 transaction_log_topic /
# player_snapshot_topic 是 cpp/libs/modules/{transaction_log,snapshot} 里的常量,后缀由本键来),
# 读法见 config.cpp::readBaseDeployConfig 的 AuditTopicGeneration 分支 + modules/audit/audit_topic.h。
# 值从 go/data_service/etc/data_service.yaml 的 Kafka.TopicGeneration 取:消费者(data-service)与
# kafka-topic-init 预建的 topic 名都由那一个键派生,生产者跟着同一个键走,三边不可能分家。
# 2026-09-09 之前 C++ 这个后缀是编译期常量、这里刻意不生成任何键;现在 C++ 真读了,必须生成 ——
# 缺这一键 C++ 回落成第一代(_g1),消费者若已换代就是"生产者写进没人消费的 topic",
# 不报任何错,流水 / 快照静默积压到保留期(30 天)被删。
AuditTopicGeneration: ${auditTopicGeneration}
# 玩家存盘 DBTask topic 世代号:<= 1 → db_task_zone_{zone}(第一代不带后缀),>= 2 → db_task_zone_{zone}_g<N>
# (与审计 topic 第一代就带 _g1 不同)。读法见 config.cpp::readBaseDeployConfig 的 DbTaskTopicGeneration 分支 +
# services/scene/player/constants/player.h。值从 go/db/etc/db.yaml 的 ServerConfig.Kafka.TopicGeneration 取
# (生成期已核对与 login 相等):缺这一键 C++ 回落第一代,go/db 若已换代就是存盘写进没人消费的旧 topic。
DbTaskTopicGeneration: ${dbTaskTopicGeneration}
# 混版本持久字段保护:scene 严格提交屏障 C3 的模式(warn = 照写并计数,enforce = 拒写、本次进度不落盘)。
# 读法见 config.cpp::readBaseDeployConfig 的 PersistSchemaRegressionMode 分支;值从 bin/etc/base_deploy_config.yaml 镜像,
# 生成期已校验只能是两个字面量之一。缺这一键 C++ 按 warn:仓库切到 enforce 时集群里不生效。
PersistSchemaRegressionMode: ${persistSchemaRegressionMode}
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
  Topics:
    - "game-events"
  # 从 bin/etc/base_deploy_config.yaml 的 Kafka.GroupID 镜像(本机为空串;只有旧的一节点一 topic 路径读它)。
  GroupID: "${kafkaGroupId}"
  CommandTopicPartitions: ${commandTopicPartitions}
  CommandTopicGeneration: ${commandTopicGeneration}
  EnableAutoCommit: true
  AutoOffsetReset: "earliest"
  # 迁移窗口开关:K8s 上关掉旧的一节点一 topic(<类型>-<node_id>)消费路径,命令只走 <类型>-cmd_g<N> 的 assign。
  # 刻意不从 bin/etc 镜像(那份是本机开关,仍为 false);依据与回退见 k8s_deploy.ps1 New-NodeConfigMapYaml 的注释。
  DisableLegacyPerNodeTopic: true
# 永久 guid 号段(Leaf-segment,node-id-overhaul-plan §6 / §7.5):scene 经 DataService.AllocateIdSegment
# 从全局库 id_segment 表按 Kind 领 [lo, hi) 双 buffer 发号;只有 scene 读,gate / battle 不铸这些 guid。
#
# 键名和结构是 C++ 那边的硬契约,不能自己发明:
# cpp/libs/engine/config/config.cpp::readBaseDeployConfig(约 126-140 行)只认**顶层 IdSegments 列表**,
# 逐项显式读 Kind / Enabled / InitialStep / MinStep / MaxStep 五个键。写成别的形状(2026-09-08 前这里
# 生成的是一个 C++ 根本不读的 GuidSegment 顶层映射 + Kinds / Tag / Step 键名)不会报错,只会一个键都读不到 → IdSegmentConfig
# 里 kinds 为空 → scene 的 Enable 校验拒绝 → DependencyGate "id segments ready" 永不放行,玩家进不来。
# 而这份 ConfigMap 是 readOnly 整目录挂到 /app/bin/etc、**完全遮蔽**镜像里那份的,所以这里错=线上错。
#
# 下面整块由 Get-AuthoritativeYamlBlock 从 bin/etc/base_deploy_config.yaml 逐行原样搬运(含每个 Kind 的
# 取值说明注释):单一真相在那份文件,改值 / 加种类(pet / guild …)只改那里,这里不留可漂移的副本。
# 注:Kind 名要与 data-service 的 IdSegment.BootstrapTags 及 id_segment 表的种子行一一对应
# (生产缺行 = ErrCodeIdSegmentUnknownTag,不自动补种)。
${idSegmentsBlock}
# C++ 节点每次 unary gRPC 调用的 deadline(毫秒),键 = 目标 ENodeType 枚举名(docs/design/grpc-muduo-transport.md §16.3)。
# config.cpp::readBaseDeployConfig 读 GrpcClient.CallDeadlineMs,grpc_call_deadline::Apply 校验后写进生成的客户端;
# 不认识的键 / 0 记 ERROR 后该类型按内置默认 10000。gate / scene / battle 共用这一块(battle-node-config 同样生成)。
# 下面整块由 Get-AuthoritativeYamlBlock 从 bin/etc/base_deploy_config.yaml 原样搬运,单一真相在那份文件;
# 「deadline ≥ 目标 Go 服务的 zrpc Timeout + 2000」在部署入口由 Assert-GrpcClientDeadlineBudget 核对,不满足即拒绝部署。
${grpcClientBlock}
"@) -replace "`t", "  "

	# SceneNodeType 在这里只是**文件基线**,gate / scene 共用同一份 ConfigMap。
	# 真正决定 scene pod 角色的是 Deployment 上的 SCENE_NODE_TYPE 环境变量:
	# cpp/libs/engine/config/config.cpp::readGameConfig 先读 yaml,再用 env 覆盖
	# (last-wins)。所以拆分模式不需要两份 ConfigMap,只需要两个 Deployment 各自
	# 带不同的 SCENE_NODE_TYPE。详见 docs/ops/scene-node-role-split.md §2。
	# 玩家收件箱消费端(docs/design/player-inbox-service.md §7.3、§11.7;交接 §6 H1):player_inbox 段由 Get-AuthoritativeYamlBlock
	# 从 bin/etc/game_config.yaml 整段复制,那份 git 跟踪的 yaml 是 consumer_enabled 的唯一来源;periodic_fetch_minutes 跟着同一段走,
	# 不在这里另写常数。复制前先过 Get-PlayerInboxConsumerEnabled:值只认小写 true / false,-PlayerInboxConsumer 与 yaml 不一致即 throw,
	# 所以任何路径都渲染不出与 yaml 不同的值。键名与 config.cpp::readGameConfig 一致。
	# battle-node-config 也由本模板生成,battle 不读这一段,多出来无副作用。
	[void](Get-PlayerInboxConsumerEnabled -Expected $PlayerInboxConsumer)
	$playerInboxBlock = Get-AuthoritativeYamlBlock -RelativePath 'bin/etc/game_config.yaml' -Key 'player_inbox'
	$gameConfig = @"
SceneNodeType: 0
ZoneId: $CurrentZoneId
zoneredis:
  host: "redis.${InfraNamespace}"
  port: 6379
  password: "${zoneRedisPassword}"
  db: 0
  timeout: 3000
  max_connections: 100
  retry_interval: 1000
${playerInboxBlock}
"@

	return @"
apiVersion: v1
kind: ConfigMap
metadata:
  name: $ConfigName
data:
  base_deploy_config.yaml: |
$($baseDeployConfig -split "`r?`n" | ForEach-Object { "    $_" } | Out-String)
  game_config.yaml: |
$($gameConfig -split "`r?`n" | ForEach-Object { "    $_" } | Out-String)
"@
}

function Get-SceneAssetSecretEnvLines {
	param([string]$Indent)
	foreach ($caller in @('guild', 'trade', 'mail')) {
		if ($script:AssetOpsOn -and $caller -ne 'mail') { continue }
		"${Indent}- name: MMORPG_ASSET_OP_SECRET_$($caller.ToUpperInvariant())"
		"${Indent}  valueFrom: {secretKeyRef: {name: domain-service-secrets, key: asset-${caller}, optional: true}}"
	}
}

# C++ Prometheus 出口(-CppMetricsPort,见参数处注释)的 env 行与容器端口行,给本脚本自己的两个 C++ 模板用
# (New-NodeDeploymentYaml、New-SceneFleetYaml);lib 的三个生成器经 PodCommon.MetricsPort 写同样的两样。env 名、端口名取 lib 契约。
# 端口为 0 时返回空数组。-ItemIndent = 列表项「- name:」/「- containerPort:」所在缩进。
function New-CppMetricsEnvLines {
	param([Parameter(Mandatory = $true)][string]$ItemIndent)

	if ($CppMetricsPort -le 0) {
		return @()
	}
	return @(
		"${ItemIndent}- name: $((Get-ClientEntryContract).CppMetricsPortEnv)",
		"${ItemIndent}  value: `"$CppMetricsPort`""
	)
}

function New-CppMetricsPortLines {
	param([Parameter(Mandatory = $true)][string]$ItemIndent)

	if ($CppMetricsPort -le 0) {
		return @()
	}
	return @(
		"${ItemIndent}- containerPort: $CppMetricsPort",
		"${ItemIndent}  name: $((Get-ClientEntryContract).CppMetricsPortName)"
	)
}

# Prometheus 抓取注解(与 Go 服务同一套 prometheus.io/*)。端口为 0 时返回空表。
function Get-CppMetricsAnnotations {
	$annotations = [ordered]@{}
	if ($CppMetricsPort -gt 0) {
		$annotations['prometheus.io/scrape'] = 'true'
		$annotations['prometheus.io/port'] = [string]$CppMetricsPort
		$annotations['prometheus.io/path'] = '/metrics'
	}
	return $annotations
}

# B0-4 豁免开关的 env 行(-NodeOpenFileLimitMode,见参数处注释),给本脚本自己的两个 C++ 模板用(New-NodeDeploymentYaml、New-SceneFleetYaml);
# lib 的三个生成器经 PodCommon.OpenFileLimitMode 写同一个变量(Get-ClientEntryNodeBaseEnv)。env 名取 lib 契约,两边同一份。
# 留空返回空数组:模板与引入开关之前逐字相同。-ItemIndent = 列表项「- name:」所在缩进(Deployment 模板用制表符、Fleet 模板用空格)。
function New-CppOpenFileLimitModeEnvLines {
	param([Parameter(Mandatory = $true)][string]$ItemIndent)

	if ([string]::IsNullOrEmpty($NodeOpenFileLimitMode)) {
		return @()
	}
	return @(
		"${ItemIndent}- name: $((Get-ClientEntryContract).NodeOpenFileLimitModeEnv)",
		"${ItemIndent}  value: `"$NodeOpenFileLimitMode`""
	)
}

function Get-SceneAccountDirectoryEnvLines {
	param([string]$Indent)
	foreach ($caller in @('scene','trade')) {
		"${Indent}- name: MMORPG_ACCOUNT_DIRECTORY_SECRET_$($caller.ToUpperInvariant())"
		"${Indent}  valueFrom:"
		"${Indent}    secretKeyRef: {name: domain-service-secrets, key: account-directory-$caller, optional: false}"
	}
}

function New-NodeDeploymentYaml {
	param(
		[Parameter(Mandatory = $true)][string]$NodeName,
		[Parameter(Mandatory = $true)][int]$Replicas,
		[Parameter(Mandatory = $true)][int]$RpcPort,
		[Parameter(Mandatory = $true)][string]$StartCommand,
		[Parameter(Mandatory = $true)][string]$ConfigMapName,
		# -1 = 不写 SCENE_NODE_TYPE(gate 等非 scene 角色)。
		[int]$SceneNodeType = -1,
		# 只用于日志 sidecar 的 zone 标签;调用方没有 zone 上下文时留空,写 "global"
		# (battle 这类全局池)。不要用 "-":它同时要进 k8s label,而 label 值必须字母数字开头结尾。
		[string]$CurrentZoneName = "",
		# 优雅退出预算。默认值就是 k8s 的默认 30s;scene 走 deployment 编排时要和 Fleet 路径
		# (New-SceneFleetYaml 用 $SceneTerminationGracePeriodSeconds)取同一个数,否则同一个
		# scene 进程换个编排方式 drain 预算就从 60s 掉到 30s —— drain 期写的存档/踢人日志正是
		# 最该留下的那批,而原生 sidecar 要等业务容器退完才停,预算不够就一起被砍。
		[int]$TerminationGracePeriodSeconds = 30
	)

	# C++ 日志 sidecar(见参数 -NoCppLogSidecar):与业务容器共享 node-logs 卷,只读读 muduo 写的文件。
	# Deployment 模板用制表符缩进,Invoke-KubectlWithInputFile 把每个制表符换成 4 空格,
	# 所以 `containers:`(1 制表符 + 2 空格)换算后落第 6 列、列表项落第 8 列。
	# 这里的片段直接按字面空格拼(6 / 8),不能再用制表符,否则会被再乘 4。
	$sidecarInitBlock = ""
	$sidecarVolumeBlock = ""
	if ($script:CppLogSidecarEnabled) {
		$zoneLabelForSidecar = if ([string]::IsNullOrWhiteSpace($CurrentZoneName)) { "global" } else { $CurrentZoneName }
		# 放 initContainers + restartPolicy: Always(原生 sidecar),理由见 New-CppLogSidecarContainerYaml。
		# initContainers 与 containers 共用同一个 spec.volumes,所以卷片段不动。
		$sidecarInitBlock = "`n      initContainers:`n" + (New-CppLogSidecarContainerYaml -ItemIndent "        " -CurrentZoneName $zoneLabelForSidecar -AsNativeSidecar)
		$sidecarVolumeBlock = "`n" + (New-CppLogSidecarVolumeYaml -ItemIndent "        ")
	}
	# Pod 模板注解:sidecar 配置指纹(开 sidecar 时)+ zone Redis 口令指纹(有口令时),同一个 annotations: 段,列位置同上(6 / 8)。
	# scene(SceneNodeType ≥ 0)在 -AssetOps on 时另带两条资产密钥指纹,见 New-CppPodAnnotationBlock。
	$podAnnotationBlock = New-CppPodAnnotationBlock -KeyIndent "      " -SceneAssetOp:($SceneNodeType -ge 0)

	# 用数组逐行拼,最后 join 换行。
	# 旧写法是 `$block += @"..."@` 连续追加两个 here-string —— here-string 内容不含
	# 结尾换行,第二次追加会直接接在上一行尾部,生成非法 YAML。
	# GRPC_SERVER_MAX_POLLERS 已退役(grpc-muduo-transport.md §15.11),不再注入。
	$extraEnvLines = @()
	if ($SceneNodeType -ge 0) {
		# 覆盖 ConfigMap 里的 SceneNodeType 基线,是角色拆分真正生效的那一步。
		$extraEnvLines += "`t`t`t- name: SCENE_NODE_TYPE"
		$extraEnvLines += "`t`t`t  value: `"$SceneNodeType`""
		$extraEnvLines += @(Get-SceneAssetSecretEnvLines -Indent "`t`t`t")
		$extraEnvLines += @(Get-SceneAccountDirectoryEnvLines -Indent "`t`t`t")
	}
	# 资产通道验签密钥(-AssetOps on,只给 scene):guild / trade 两把都要,值经 Secret 注入(secretKeyRef 不写 optional,
	# Secret 由 Apply-Zone 先于 scene 建好)。off 时不写,模板与引入开关之前逐字相同。
	if ($SceneNodeType -ge 0 -and $script:AssetOpsOn) {
		$extraEnvLines += New-SceneAssetOpEnvLines -ItemIndent "`t`t`t" -Step "`t"
	}
	# 发号槽本地缓存目录(见顶部 $SnowflakeCacheDir),下面 volumes 在同一路径挂 emptyDir。
	# gate / scene 共用本模板:只有 scene 启用 SnowflakeSlotClient 会写它,gate 收到这个环境变量
	# 什么也不做,统一注入省一个分支。
	$extraEnvLines += "`t`t`t- name: SNOWFLAKE_CACHE_DIR"
	$extraEnvLines += "`t`t`t  value: `"$SnowflakeCacheDir`""
	# 客户端 RPC 路由模式(脚本参数 -GateRouterMode,默认 "1";前置与回退见参数处注释)。只给 gate 注入:
	# scene / battle 不读这个变量,写进它们的 Deployment 只会让人误以为它们也分模式。
	# 两个值都显式写出而不是省略:`kubectl get deploy gate -o yaml` 一眼就能看出这个 zone 的 gate 跑在哪个模式,
	# 不必再去翻 C++ 默认值 —— 部署层默认 "1" 与 C++ 进程默认(未设即直连)刻意不同(D-12),省略 "1" 就会落回直连。
	if ($NodeName -eq 'gate') {
		$extraEnvLines += "`t`t`t- name: GATE_CLIENT_RPC_ROUTER"
		$extraEnvLines += "`t`t`t  value: `"$GateRouterMode`""
	}
	# B0-4 豁免开关(-NodeOpenFileLimitMode):gate / scene / battle 都读,留空不写。
	$extraEnvLines += @(New-CppOpenFileLimitModeEnvLines -ItemIndent "`t`t`t")
	# Prometheus 出口(-CppMetricsPort):gate / scene / battle 都开,0 不写。
	$extraEnvLines += @(New-CppMetricsEnvLines -ItemIndent "`t`t`t")
	$metricsPortBlock = (@(New-CppMetricsPortLines -ItemIndent "`t`t`t") | ForEach-Object { "`n$_" }) -join ''
	# node-logs emptyDir 会遮住镜像目录；Node 构造器立即打开 logs/cpp_nodes/<role>，先建父目录。
	$grpcEnvBlock = $extraEnvLines -join "`n"
	# 端口与探针按角色拼在 ports 列表之后(推导与证据见 PROGRESS「2026-09-25 C++ 节点就绪探针」)。
	# 三个角色共同的约束:
	#  - C++ 尚未注册 grpc.health.v1,只能用 tcpSocket 探**实际在监听**的口;端口一律写数字,不写端口名。
	#  - 两个口都在 etcd 注册完成之后才 listen,所以 tcpSocket 通 = 「已发布进 etcd」,**不等于**依赖门
	#    (gate 等 ClientRpcRouter / Scene —— -GateRouterMode "0" 回退时是 Login / Scene,scene 等 SceneManager 与号段首段)已过。
	#    readiness 只能表达前者。
	#  - 不加 livenessProbe:tcpSocket 看不出 EventLoop 卡死(内核照样完成握手),加了只多一条杀容器的路。
	#  - 探 gRPC 口(scene / battle):新服务端(muduo_grpc,grpc-muduo-transport.md §15.5.1)对不发 HTTP/2 preface 的连接
	#    静默处理 —— 一个字节都不写、断开只打 DEBUG、计入 closed_before_preface,探针不会刷日志。
	#    停机排空期间监听已关,探 gRPC 口的 readiness 失败属预期(此时 Pod 已在终止,本来就是 NotReady)。
	#  - startupProbe 会杀容器,预算必须越过 etcd 租约:同 Pod 重启时 POD_IP 不变,旧进程的注册要等
	#    NodeTTLSeconds(180s,bin/etc/base_deploy_config.yaml)到期才消失,在此之前新进程命中
	#    node_allocator.cpp「Preset RPC port ... already registered」一直退避重试、不开监听。
	#  - readiness 失败不杀容器,只影响滚动更新节奏与 -WaitReady:login 从 etcd 取 POD_IP:18000 原样下发给
	#    客户端,不经过任何 Service,所以 gate NotReady 不会把玩家挡在门外。
	# 现状(加之前)是容器一启动就 Ready:滚动更新时新 Pod 还没注册进 etcd,旧 Pod 就可能开始收 SIGTERM。
	$nodePortsAndProbes = ''
	if ($NodeName -eq 'gate') {
		# gate 只有一个监听口 $RpcPort:玩家连接与节点 RPC 共用 Node 自带的那个 TcpServer
		# (gate/main.cpp 在 SetAfterStart 里换掉它的连接 / 消息回调)。gate 不注册任何 gRPC 服务,
		# **RpcPort+30000 上没有监听** —— 照抄 battle / scene 去探那个口,探针会恒失败。
		# 不加 startupProbe:要加的话预算不少于 300s(180s 租约 + 正常启动),给不准就会在同 Pod 重启时多杀几轮。
		# 探测开销:每次探测在 gate 上建一个未验证会话、断开即删、不通知 login,muduo 多打两行 INFO,可忽略。
		$nodePortsAndProbes = @"
		  readinessProbe:
			tcpSocket:
			  port: $RpcPort
			periodSeconds: 5
			failureThreshold: 3
"@
	}
	elseif ($SceneNodeType -ge 0) {
		# scene(Deployment 模式;只有 scene 的调用方传 SceneNodeType)。Agones 模式走 New-SceneFleetYaml,
		# 那边**不加**任何 K8s 探针:Agones 给 GameServer Pod 写死 restartPolicy: Never,会杀容器的探针
		# 等于销毁整个 GameServer,就绪交给 Agones SDK 的 Ready / Health。
		# C++ 非 gate TCP 合法区间为 20000..35535,gRPC 固定派生为 TCP+30000(node_allocator.cpp kGrpcPortOffset)。
		if ($RpcPort -lt 20000 -or $RpcPort -gt 35535) {
			throw 'scene RPC_PORT 必须在 20000..35535，保证派生 gRPC 端口不越界。'
		}
		$sceneGrpcPort = $RpcPort + 30000
		# readiness 探 gRPC 口:它依赖 main.cpp 的 RegisterGrpcService,多确认一次 gRPC server 起来了
		# (与 battle 同形)。gRPC 口此前没有 containerPort 声明,这里一并补上。
		# 停机时 before-shutdown hook 跑完才关 gRPC,而 Pod 进入终止状态时 K8s 本来就把它摘成 NotReady,没有副作用。
		# 不加 startupProbe:要加的话预算不少于 600s —— 同 Pod 重启要等 180s 租约,listen 前还要同步加载配表与
		# 导航数据,这段耗时在 K8s 里没有测过。
		$nodePortsAndProbes = @"
			- containerPort: $sceneGrpcPort
			  name: grpc
		  readinessProbe:
			tcpSocket:
			  port: $sceneGrpcPort
			periodSeconds: 5
			failureThreshold: 3
"@
	}
	elseif ($NodeName -eq 'battle') {
		# C++ 非 gate TCP 合法区间为 20000..35535，gRPC 固定派生为 TCP+30000。
		if ($RpcPort -lt 20000 -or $RpcPort -gt 35535) {
			throw 'battle RPC_PORT 必须在 20000..35535，保证派生 gRPC 端口不越界。'
		}
		$battleGrpcPort = $RpcPort + 30000
		# C++ 尚未注册 grpc.health.v1；用实际监听端口，不能套 Go 的 gRPC health 探针。
		# startup 预算 2s × 150 = 300s:原来的 2s × 90 = 180s 恰好等于 etcd 租约,同 Pod 重启撞上旧注册时
		# 正处在临界点,会被多杀几轮进 CrashLoopBackOff(见上方共同约束)。
		$nodePortsAndProbes = @"
			- containerPort: $battleGrpcPort
			  name: grpc
		  startupProbe:
			tcpSocket:
			  port: $RpcPort
			periodSeconds: 2
			failureThreshold: 150
		  readinessProbe:
			tcpSocket:
			  port: $battleGrpcPort
			periodSeconds: 5
"@
	}

	# 滚动策略显式写 maxUnavailable 0(B0-4 部署侧后续项,2026-10-20):新 Pod 就绪之前不缩旧 Pod。
	# 不写时按 K8s 缺省 25% 向下取整:scene 缺省 4 副本会先缩掉 1 台旧 scene(该台玩家按关机流程退出),新 Pod 若因节点池
	# nofile 硬限不够而拒启(open_file_limit.h),这份容量一直缺到回滚;写成 0 之后拒启只会让滚动停住,旧 Pod 继续服务。
	# gate / battle 在 4 副本以下按缺省本来就是 0,行为不变。maxSurge 不写,仍取 K8s 缺省 25%(向上取整,至少 1 个新 Pod),
	# 滚动节奏与改动前相同;两者不能同为 0(K8s 拒收)。battle 的 hostPort 形态走 lib New-BattleHostPortDeploymentYaml,
	# 固定 maxSurge 0 / maxUnavailable 1(hostPort 独占,D86),不经过这里。
	return @"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $NodeName
spec:
  replicas: $Replicas
  strategy:
	type: RollingUpdate
	rollingUpdate:
	  maxUnavailable: 0
  selector:
	matchLabels:
	  app: $NodeName
  template:
	metadata:$podAnnotationBlock
	  labels:
		app: $NodeName
	spec:
	  terminationGracePeriodSeconds: $TerminationGracePeriodSeconds$sidecarInitBlock
	  containers:
		- name: $NodeName
		  image: $NodeImage
		  imagePullPolicy: $ImagePullPolicy
		  workingDir: /app/bin
		  command: ["/bin/sh", "-lc"]
		  args: ["mkdir -p /app/bin/logs/cpp_nodes && $CppLogPrunerCommand$StartCommand"]
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
			- name: snowflake-cache
			  mountPath: $SnowflakeCacheDir
		  ports:
			- containerPort: $RpcPort
			  name: rpc$metricsPortBlock
$nodePortsAndProbes
	  volumes:
		- name: node-config
		  configMap:
			name: $ConfigMapName
		- name: node-logs
		  emptyDir: { sizeLimit: $CppLogVolumeSizeLimit }
		- name: snowflake-cache
		  emptyDir: { sizeLimit: 64Mi }$sidecarVolumeBlock
"@
}

<#
.SYNOPSIS
生成 C++ 日志 sidecar 的 Alloy 配置 ConfigMap。

.DESCRIPTION
读的是业务容器写在共享卷 /app/bin/logs/cpp_nodes/*.log 里的 muduo 日志,**不读容器 stdout**:
Linux 下 node.cpp 的 Node::AsyncOutput 只在 #ifdef WIN32 里调 LogToConsole,容器 stdout 上
只有 gate 的 [gate_version] 启动行和 gRPC / librdkafka 的 stderr,业务日志一行都没有。

标签口径与本机观测台一致(docs/ops/grafana-loki-local-logs.md §3):job / lang / service / level,
另加 k8s 独有的 namespace / pod / zone,方便和 Go / Java 的 pod 日志在同一个 Grafana 里对齐。
#>
function Get-CppLogSidecarConfigAlloy {
	# 单引号 here-string:里面的 $ 和反引号全部按字面量处理,正则不用转义。
	$config = @'
// 本文件由 tools/scripts/k8s_deploy.ps1 的 New-CppLogSidecarConfigMapYaml 生成,不要直接改集群里的副本。
// 作用:读同一个 Pod 里业务容器写下的 muduo 日志文件,直接送 Loki(全程不经过 stdout)。
logging {
  level  = "warn"
  format = "logfmt"
}

loki.write "out" {
  endpoint {
    url = "__LOKI_PUSH_URL__"
  }
  external_labels = {
    env       = "k8s",
    namespace = sys.env("POD_NAMESPACE"),
    pod       = sys.env("POD_NAME"),
    zone      = sys.env("ZONE_NAME"),
  }
}

// muduo 自己滚动写的文件:<节点>.<时间>.<主机>.<pid>.log,主机名在容器里就是 Pod 名。
// ignore_older_than:muduo 滚动后不删旧文件,不甩掉它们的话每个滚过的文件都常驻一个 tailer
// (goroutine + fd + 缓冲),长跑的 scene 上只增不减。阈值只要大于"节点最长可能不写一行日志的时间"
// 即可,不要往小调:文件掉出 target 列表会连读取位置一起丢,它再被追加时整份(最多 8MiB)重推一遍。
local.file_match "cpp" {
  path_targets      = [{ __path__ = "/app/bin/logs/cpp_nodes/*.log" }]
  sync_period       = "5s"
  ignore_older_than = "168h"
}

loki.source.file "cpp" {
  targets    = local.file_match.cpp.targets
  forward_to = [loki.process.cpp.receiver]

  file_watch {
    min_poll_frequency = "500ms"
    max_poll_frequency = "2s"
  }
}

loki.process "cpp" {
  forward_to = [loki.write.out.receiver]

  // Build Info 这类多行消息只有第一行带时间戳,其余并回同一条。
  // 线程号右对齐占 5 列,不足 5 位前面补空格,所以是 " +" 而不是单个空格。
  // "Dropped log messages at ..." 是 muduo 后端写不过来时自己插进**日志文件**的告警
  // (AsyncLogging.cc threadFunc:fputs 到 stderr 的同时也 output.append 进 LogFile),
  // 它没有行头,不单列一个分支就会被并进上一条、跟着那条的时间和级别走 —— 压测时
  // "日志被丢了多少"这唯一的自证信号会被降级成 info 藏在别人的正文里。
  // (Windows 上那条 "Assertion failed: " 是 MSVC _wassert 写到控制台 stderr 的,
  //  Linux 容器里不存在:release 带 -DNDEBUG 编掉了 assert,glibc 的断言文本也走 stderr
  //  而不是 muduo 文件。所以这里不再有它的分支,见 docs/ops/grafana-loki-local-logs.md §6。)
  stage.multiline {
    firstline     = `^(\d{8} \d{2}:\d{2}:\d{2}\.\d{6} +\d+ [A-Z]+ |Dropped log messages at )`
    max_wait_time = "2s"
    max_lines     = 64
  }

  // 文件名 -> service / pid。muduo 文件里没有实例名,pid 用来区分同一节点的多次重启。
  stage.regex {
    source     = "filename"
    expression = `/(?P<service>[a-z_]+)\.\d{8}-\d{6}\.[^/]+\.(?P<pid>\d+)\.log$`
  }
  stage.labels {
    values = { service = "" }
  }
  stage.structured_metadata {
    values = { pid = "" }
  }
  stage.static_labels {
    values = {
      job  = "cpp_nodes",
      lang = "cpp",
    }
  }

  // 标准 muduo 行:解析时间与级别。行里的时间由 node.cpp:1163 的
  // TimeZone::loadZoneFile("zoneinfo/Asia/Hong_Kong") 出,所以这里按同一个时区解析。
  // muduo 只输出 TRACE/DEBUG/INFO/WARN/ERROR/FATAL 六种;LOG_SYSERR 实际按 ERROR 输出。
  stage.match {
    selector = `{job="cpp_nodes"} |~ "^\\d{8} \\d{2}:\\d{2}:\\d{2}\\.\\d{6} +\\d+ (TRACE|DEBUG|INFO|WARN|ERROR|FATAL) "`

    stage.regex {
      expression = `^(?P<ts>\d{8} \d{2}:\d{2}:\d{2}\.\d{6}) +(?P<tid>\d+) (?P<level>[A-Z]+) `
    }
    stage.timestamp {
      source            = "ts"
      format            = "20060102 15:04:05.000000"
      location          = "Asia/Hong_Kong"
      action_on_failure = "fudge"
    }
    stage.template {
      source   = "level"
      template = `{{ ToLower .Value }}`
    }
    stage.labels {
      values = { level = "" }
    }
    stage.structured_metadata {
      values = { tid = "" }
    }
  }

  // 日志被丢弃的告警行:没有行头也没有级别,按 warn 记,压测时才能用
  // level=~"warn|error|fatal" 一眼看出"这段时间日志被丢了"。
  // 行里那个时间是 Timestamp::toFormattedString(),走 gmtime_r 出的是 UTC,
  // 与正文 muduo 行的 +8 不同轴,所以故意不接 stage.timestamp,用采集时间。
  stage.match {
    selector = `{job="cpp_nodes"} |~ "^Dropped log messages at "`

    stage.static_labels {
      values = { level = "warn" }
    }
  }

  // filename 里带滚动时间戳和 pid(LogFile.cc 每次滚动都用当前时间+主机名+pid 拼新名),
  // 留成流标签等于把上面刻意挪进结构化元数据的 pid 又从旁边放回去:每滚 8MiB 就新开一条流。
  // 转存结构化元数据仍可回查是哪一次滚动,但不再参与流切分。必须放在上面的
  // stage.regex { source = "filename" } 之后,否则 service 标签解析不出来。
  stage.structured_metadata {
    values = { filename = "" }
  }
  stage.label_drop {
    values = ["filename"]
  }
}
'@

	return $config.Replace('__LOKI_PUSH_URL__', $script:CppLogSidecarPushUrl)
}

<#
.SYNOPSIS
取 sidecar Alloy 配置正文的短哈希,给 Pod 模板注解用。

.DESCRIPTION
ConfigMap 变了不会让任何进程重读:Alloy 只在 SIGHUP / /-/reload 时重载,
而只改 ConfigMap 时 Deployment / Fleet 的 pod 模板逐字节没变,kubectl apply 是 no-op。
把正文哈希打进 pod 模板注解,配置一变模板就变,apply 自然滚动重建。
代价要知道:改 sidecar 配置 = 滚动重启 C++ 节点(Agones 侧是 Fleet RollingUpdate 换 GameServer),
所以改 -LokiPushUrl / 解析规则要挑时机,见 docs/ops/grafana-loki-local-logs.md §6。
镜像 tag 不进哈希:它本来就在容器片段的 image: 行里,改了模板自然会变。
#>
function Get-CppLogSidecarConfigHash {
	$sha256 = [System.Security.Cryptography.SHA256]::Create()
	try {
		$bytes = $sha256.ComputeHash([System.Text.Encoding]::UTF8.GetBytes((Get-CppLogSidecarConfigAlloy)))
	} finally {
		$sha256.Dispose()
	}
	return ((($bytes | ForEach-Object { $_.ToString('x2') }) -join '').Substring(0, 12))
}

function New-CppLogSidecarConfigMapYaml {
	param(
		[Parameter(Mandatory = $true)][string]$ConfigName,
		# zone 名;battle 这类不属于任何 zone 的池传空串 = 不写 mmorpg.io/zone 标签。
		# 不要拿 "-" 当占位:k8s 的 label value 必须以字母数字开头结尾,"-" 会被 API server 拒掉
		# (而且 kubectl apply --dry-run=client 不查 label value,本地全绿、打到集群才报错)。
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$CurrentZoneName
	)

	$config = Get-CppLogSidecarConfigAlloy
	# 塞进 YAML 块标量,统一缩进 4 空格;空行不补空格,避免块标量里出现尾随空白。
	$indented = (($config -split "`r?`n") | ForEach-Object { if ([string]::IsNullOrEmpty($_)) { "" } else { "    $_" } }) -join "`n"

	# 值加引号:将来出现纯数字 zone 名(例如 2026)时,不加引号会被解析成数字,
	# kubectl 直接拒:cannot unmarshal number into ObjectMeta.labels of type string。
	$zoneLabelLine = if ([string]::IsNullOrWhiteSpace($CurrentZoneName)) { "" } else { "`n    mmorpg.io/zone: `"$CurrentZoneName`"" }

	return @"
apiVersion: v1
kind: ConfigMap
metadata:
  name: $ConfigName
  labels:
    app: cpp-log-sidecar$zoneLabelLine
data:
  config.alloy: |
$indented
"@
}

<#
.SYNOPSIS
生成 C++ 日志 sidecar 的容器片段与它额外需要的两个卷。

.DESCRIPTION
Deployment 模板用制表符缩进(Invoke-KubectlWithInputFile 会把制表符换成 4 空格),Fleet 模板用空格,
两边的列位置不同,所以由调用方传入列表项的缩进字符串,这里只用空格拼,拼出来的列与两种模板都能对齐。
#>
function New-CppLogSidecarContainerYaml {
	param(
		[Parameter(Mandatory = $true)][string]$ItemIndent,
		[Parameter(Mandatory = $true)][string]$CurrentZoneName,
		# 片段要拼进 initContainers 时传这个。restartPolicy 只对 initContainers 无条件合法;
		# 普通容器要 API server 开了 ContainerRestartRules 门控才收(本机 kind v1.37 实测能收,
		# 但 1.28~1.33 会报 containers[N].restartPolicy: Forbidden,整份清单 apply 不上去),
		# 所以这一行只在放进 initContainers 时输出。
		[switch]$AsNativeSidecar
	)

	$keyIndent = $ItemIndent + "  "
	$lines = @(
		"${ItemIndent}- name: log-sidecar",
		"${keyIndent}image: $CppLogSidecarImage",
		"${keyIndent}imagePullPolicy: IfNotPresent"
	)
	if ($AsNativeSidecar) {
		# k8s 原生 sidecar(1.29 默认开、1.33 GA,本仓 kind 是 1.37):
		#   ① 它的重启策略独立于 Pod —— Agones 给 GameServer Pod 写死 restartPolicy: Never,
		#      普通容器形态下 sidecar 一旦 OOM 就永远停着,而 Agones 的健康检查只看游戏容器,
		#      不会把 GameServer 置 Unhealthy,日志会在这个 Pod 的余生里静默断流;
		#   ② Pod 终止时 kubelet 会等业务容器完全退出后再停 sidecar,scene 那 60 秒 drain
		#      里写的存档/踢人/关服日志才追得完(普通容器是同时收 SIGTERM,这段全丢)。
		# 明知的代价:initContainers 里的容器不 Started,业务容器就不启动 —— sidecar 镜像拉不到时
		# Pod 卡在 Init:ErrImagePull,而不是"业务在跑、只是 Pod 没 Ready"。两种形态其实都是故障
		# (普通容器形态下 Pod 同样不 Ready、gate-entry 没有后端),原生 sidecar 至少报得更直白。
		# 逃生口:把镜像预载进 kind(见 deploy/k8s/README.md),或 -NoCppLogSidecar。
		$lines += "${keyIndent}restartPolicy: Always"
	}
	$lines += @(
		"${keyIndent}args:",
		"${keyIndent}  - run",
		# 绑 0.0.0.0 而不是 127.0.0.1:Pod 外才够得着 /metrics 和 /-/reload。
		# 实测这两个指标是"sidecar 还在读、还推得出去吗"的唯一信号(名字没有 alloy_ 前缀):
		#   loki_source_file_files_active_total —— 当前在 tail 的文件数
		#   loki_write_sent_entries_total       —— 推走的条数
		"${keyIndent}  - --server.http.listen-addr=0.0.0.0:12345",
		"${keyIndent}  - --storage.path=/var/lib/alloy",
		"${keyIndent}  - /etc/alloy/config.alloy",
		"${keyIndent}ports:",
		"${keyIndent}  - containerPort: 12345",
		"${keyIndent}    name: alloy-http",
		# 镜像默认 uid 0;C++ 业务镜像是 USER 10001:10001,没理由让观测容器把 root 塞回来。
		# 三条读写路径都不需要 root:日志文件 0644、positions 落在 0777 的 emptyDir、
		# configMap 卷 0644。Alloy 只往 --storage.path 写,根文件系统可以只读。
		"${keyIndent}securityContext:",
		"${keyIndent}  runAsUser: 10001",
		"${keyIndent}  runAsGroup: 10001",
		"${keyIndent}  runAsNonRoot: true",
		"${keyIndent}  allowPrivilegeEscalation: false",
		"${keyIndent}  readOnlyRootFilesystem: true",
		"${keyIndent}  capabilities:",
		"${keyIndent}    drop:",
		"${keyIndent}      - ALL",
		"${keyIndent}env:"
	)
	$lines += @(
		"${keyIndent}  - name: POD_NAME",
		"${keyIndent}    valueFrom:",
		"${keyIndent}      fieldRef:",
		"${keyIndent}        fieldPath: metadata.name",
		"${keyIndent}  - name: POD_NAMESPACE",
		"${keyIndent}    valueFrom:",
		"${keyIndent}      fieldRef:",
		"${keyIndent}        fieldPath: metadata.namespace",
		"${keyIndent}  - name: ZONE_NAME",
		"${keyIndent}    value: `"$CurrentZoneName`"",
		"${keyIndent}volumeMounts:",
		"${keyIndent}  - name: $script:CppLogVolumeName",
		"${keyIndent}    mountPath: /app/bin/logs",
		"${keyIndent}    readOnly: true",
		"${keyIndent}  - name: cpp-log-sidecar-config",
		"${keyIndent}    mountPath: /etc/alloy",
		"${keyIndent}    readOnly: true",
		"${keyIndent}  - name: cpp-log-sidecar-state",
		"${keyIndent}    mountPath: /var/lib/alloy",
		"${keyIndent}resources:",
		"${keyIndent}  requests:",
		"${keyIndent}    cpu: 20m",
		"${keyIndent}    memory: 64Mi",
		"${keyIndent}  limits:",
		"${keyIndent}    memory: 256Mi"
	)
	return ($lines -join "`n")
}

function New-CppLogSidecarVolumeYaml {
	param([Parameter(Mandatory = $true)][string]$ItemIndent)

	$keyIndent = $ItemIndent + "  "
	$lines = @(
		"${ItemIndent}- name: cpp-log-sidecar-config",
		"${keyIndent}configMap:",
		"${keyIndent}  name: $script:CppLogSidecarConfigMapName",
		"${ItemIndent}- name: cpp-log-sidecar-state",
		# 只放 positions.yml 和 alloy_seed.json,几 KB;给个上限免得它成为下一个无界卷。
		"${keyIndent}emptyDir: { sizeLimit: 64Mi }"
	)
	return ($lines -join "`n")
}

<#
.SYNOPSIS
启用日志 sidecar 时,在 apply 之前提醒两件会静默失败的事。

.DESCRIPTION
① 镜像:sidecar 是本轮新引入的 Docker Hub 依赖,而 kind 节点不共享宿主的镜像缓存。
   拉不到镜像 = Pod 永远 NotReady = gate-entry 没有后端、整个 zone 连不进来,
   可业务进程本身是好的 —— 只看 rollout 超时基本想不到是观测容器拖的。
② 目标 Loki:它只随 infra-up 部署(见 Apply-Infra 的 $infraManifests)。
   zone-up-only / all-up -SkipInfra / 存量集群升级这几种走法会得到"sidecar 装上了但推无可推"
   的半配置状态:Alloy 的推送失败只写在它自己的 stdout 里,脚本不失败、部署报成功,
   表现为"一条 C++ 日志都查不到"。
只读探测,查不到只警告不阻断:集群权限或网络抖动不该让部署失败。
#>
function Write-CppLogSidecarNotice {
	param([Parameter(Mandatory = $true)][string]$Namespace)

	if ($script:CppLogSidecarNoticeShown) { return }
	$script:CppLogSidecarNoticeShown = $true

	Write-Host "C++ log sidecar: enabled (namespace=$Namespace image=$CppLogSidecarImage push=$script:CppLogSidecarPushUrl)"
	# 说清真实症状:sidecar 是原生 sidecar(initContainers),拉不到镜像时业务容器**一次都不会启动**,
	# Pod 卡在 Init:ErrImagePull / Init:ImagePullBackOff,报错在 describe 的 Init Containers 一节。
	Write-Host "  Load $CppLogSidecarImage into kind nodes first: if the image cannot be pulled, gate/scene/battle remain in Init:ErrImagePull and the application container never starts (preload command in deploy/k8s/README.md). Pass -NoCppLogSidecar to disable it."

	if ($DryRun) { return }
	# 只有用集群内默认 Loki 时才探测得了;-LokiPushUrl 指向外部时这里无从判断。
	if (-not [string]::IsNullOrWhiteSpace($LokiPushUrl)) { return }

	$probeArgs = @()
	$probeArgs += Build-KubectlBaseArgs
	$probeArgs += @("-n", $InfraNamespace, "get", "svc", "loki", "--ignore-not-found", "-o", "name")
	$found = & kubectl @probeArgs 2>$null
	if ($LASTEXITCODE -ne 0) {
		Write-Warning "Could not confirm whether $InfraNamespace contains a Loki Service (kubectl get svc loki failed); C++ log delivery may fail."
		return
	}
	if ([string]::IsNullOrWhiteSpace(($found -join ''))) {
		Write-Warning "The sidecar sends C++ logs to $script:CppLogSidecarPushUrl, but $InfraNamespace has no loki Service. Run -Command infra-up without -SkipInfra, set -LokiPushUrl to an existing Loki, or disable it with -NoCppLogSidecar; otherwise deployment succeeds but no logs can be queried."
	}
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

	# C++ 日志 sidecar(见参数 -NoCppLogSidecar)。Fleet 模板用空格缩进:pod spec 的
	# initContainers / containers / volumes 这几个键在第 10 列,列表项在第 12 列。
	# sidecar 放 initContainers + restartPolicy: Always(原生 sidecar),原因见
	# New-CppLogSidecarContainerYaml —— Agones 给 GameServer Pod 写死 restartPolicy: Never,
	# 普通容器形态下它 OOM 一次就永不重启,而 Agones 的健康检查只看游戏容器,不会报这个故障。
	# container: 那一行现在是冗余的(移进 initContainers 后 containers 只剩 1 个,Agones 的
	# applyContainerDefaults 会自己认),但显式写出仍然合法,而且将来再加第二个普通容器时
	# 不写这行 Fleet 会被 Agones 拒掉 —— 所以保留,别顺手删。
	$sidecarInitBlock = ""
	$sidecarVolumeBlock = ""
	$gameServerContainerLine = ""
	if ($script:CppLogSidecarEnabled) {
		$sidecarInitBlock = "`n          initContainers:`n" + (New-CppLogSidecarContainerYaml -ItemIndent "            " -CurrentZoneName $ZoneLabel -AsNativeSidecar)
		$sidecarVolumeBlock = "`n" + (New-CppLogSidecarVolumeYaml -ItemIndent "            ")
		$gameServerContainerLine = "      container: $FleetName`n"
	}
	# Pod 模板注解(sidecar 配置指纹 + zone Redis 口令指纹),同 New-NodeDeploymentYaml;Fleet 的 Pod 模板 metadata 子键在第 10 列。
	$podAnnotationBlock = New-CppPodAnnotationBlock -KeyIndent "          " -SceneAssetOp

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

	$sceneAssetEnvLines = @(Get-SceneAssetSecretEnvLines -Indent '                ')
	$sceneAssetEnvLines += @(Get-SceneAccountDirectoryEnvLines -Indent '                ')
	if ($script:AssetOpsOn) { $sceneAssetEnvLines += @(New-SceneAssetOpEnvLines -ItemIndent (' ' * 16) -Step '    ') }
	# B0-4 豁免开关(-NodeOpenFileLimitMode)借用资产密钥 env 的插入点接在其后,模板 here-string 不动;留空不写,Fleet 逐字不变。
	$sceneAssetEnvLines += @(New-CppOpenFileLimitModeEnvLines -ItemIndent (' ' * 16))
	# Prometheus 出口(-CppMetricsPort)同样借用这个插入点;端口接在 rpc 之后。0 不写,Fleet 逐字不变。
	$sceneAssetEnvLines += @(New-CppMetricsEnvLines -ItemIndent (' ' * 16))
	$sceneAssetEnvBlock = $sceneAssetEnvLines -join "`n"
	$fleetMetricsPortBlock = (@(New-CppMetricsPortLines -ItemIndent (' ' * 16)) | ForEach-Object { "`n$_" }) -join ''
	# GRPC_SERVER_MAX_POLLERS 已退役(grpc-muduo-transport.md §15.11),Fleet 不再注入。

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
$gameServerContainerLine      ports:
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
        metadata:$podAnnotationBlock
          labels:
            app: $FleetName
            mmorpg.io/role: $RoleLabel
            mmorpg.io/zone: $ZoneLabel
            mmorpg.io/zone-id: "$ZoneIdLabel"
            mmorpg.io/build: $buildLabel
        spec:
          terminationGracePeriodSeconds: $SceneTerminationGracePeriodSeconds$sidecarInitBlock
          containers:
            - name: $FleetName
              image: $NodeImage
              imagePullPolicy: $ImagePullPolicy
              workingDir: /app/bin
              command: ["/bin/sh", "-lc"]
              args: ["mkdir -p /app/bin/logs/cpp_nodes && $CppLogPrunerCommand$StartCommand"]
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
                # 发号槽本地缓存目录(见顶部 $SnowflakeCacheDir),volumes 里同一路径挂 emptyDir;
                # 每个 GameServer Pod 自己一份,不跨 Pod 共享。
                - name: SNOWFLAKE_CACHE_DIR
                  value: "$SnowflakeCacheDir"
                - name: AGONES_ENABLED
                  value: "1"
$sceneAssetEnvBlock
              volumeMounts:
                - name: node-config
                  mountPath: /app/bin/etc
                  readOnly: true
                - name: node-logs
                  mountPath: /app/bin/logs
                - name: snowflake-cache
                  mountPath: $SnowflakeCacheDir
              ports:
                - containerPort: $RpcPort
                  name: rpc$fleetMetricsPortBlock
          volumes:
            - name: node-config
              configMap:
                name: $ConfigMapName
            - name: node-logs
              emptyDir: { sizeLimit: $CppLogVolumeSizeLimit }
            - name: snowflake-cache
              emptyDir: { sizeLimit: 64Mi }$sidecarVolumeBlock
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

<#
.SYNOPSIS
	本脚本自己的两个 C++ 节点模板(New-NodeDeploymentYaml、New-SceneFleetYaml)的 Pod 模板 annotations 段:
	日志 sidecar 配置指纹(开 sidecar 时)在前,zone Redis 口令指纹(有口令时,Get-CppZoneRedisAuthAnnotations)在后。
	两条都没有时返回空串,模板与不带注解时逐字相同。lib 的三个模板经 New-CppNodePodCommon 拿到同样的两条、按同样的顺序写出。
	KeyIndent = Pod 模板 metadata 子键所在的列(Deployment 换算后第 6 列,Fleet 第 10 列)。渲染与校验在 lib 的
	ConvertTo-ClientEntryPodAnnotationBlock(值加双引号:12 位十六进制可能全是数字或形如 1e5,裸写会被解析成数字,API server 拒收)。
#>
function New-CppPodAnnotationBlock {
	param(
		[Parameter(Mandatory = $true)][string]$KeyIndent,
		# scene 的 Pod 模板:-AssetOps on 时追加两条资产密钥指纹($script:AssetOpCallers 的 HashAnnotation),
		# 只换密钥也会滚动,并供 Assert-AssetOpClusterConsistency 跨命令核对。off 时不写,注解段与引入开关之前逐字相同。
		[switch]$SceneAssetOp
	)

	$annotations = [ordered]@{}
	if ($script:CppLogSidecarEnabled) {
		$annotations['mmorpg.io/cpp-log-sidecar-config-hash'] = Get-CppLogSidecarConfigHash
	}
	$zoneRedisAuth = Get-CppZoneRedisAuthAnnotations
	foreach ($key in $zoneRedisAuth.Keys) { $annotations[$key] = $zoneRedisAuth[$key] }
	if ($SceneAssetOp -and $script:AssetOpsOn) {
		$assetOpHashes = Get-AssetOpDesiredHashes
		foreach ($caller in $assetOpHashes.Keys) { $annotations[$script:AssetOpCallers[$caller].HashAnnotation] = $assetOpHashes[$caller] }
	}
	# Prometheus 抓取注解排在各条指纹之后(-CppMetricsPort 0 时不写)。
	$metricsAnnotations = Get-CppMetricsAnnotations
	foreach ($key in $metricsAnnotations.Keys) { $annotations[$key] = $metricsAnnotations[$key] }
	return ConvertTo-ClientEntryPodAnnotationBlock -Annotations $annotations -KeyIndent $KeyIndent
}

<#
.SYNOPSIS
	把本脚本的 C++ 节点公共参数(镜像、拉取策略、发号槽缓存、日志卷与清理循环、日志 sidecar、
	zone Redis 口令指纹注解、B0-4 豁免开关 -NodeOpenFileLimitMode)打包成 lib/k8s_client_entry.ps1 生成器要的 PodCommon。与 New-NodeDeploymentYaml 同一组脚本变量,一处取值。
	-GrpcServerMaxPollers 已退役(grpc-muduo-transport.md §15.11),固定透传 0,库里也不再据它注入任何 env。
	口令指纹要求 Initialize-InjectedSecrets 已经跑过(Get-CppZoneRedisPassword 对未解析 fail-closed),所以只在写路径入口之后调用:
	Apply-Zone(New-ExternalGateManifests)与 Apply-Infra(New-BattleWorkloadManifest)。

.PARAMETER SidecarZoneLabel
	日志 sidecar 的 zone 标签:zone 内节点传 zone 名,battle 这类全局池传 "global"(同 New-NodeDeploymentYaml)。
#>
function New-CppNodePodCommon {
	param([Parameter(Mandatory = $true)][string]$SidecarZoneLabel)

	$sidecar = @{ SidecarContainerYaml = ''; SidecarVolumeYaml = ''; SidecarConfigHash = '' }
	if ($script:CppLogSidecarEnabled) {
		# 缩进随便给:库会去掉公共前导空格后按各模板的列位置重新缩进(-ItemIndent 必填、不收空串)。
		$sidecar.SidecarContainerYaml = New-CppLogSidecarContainerYaml -ItemIndent '  ' -CurrentZoneName $SidecarZoneLabel -AsNativeSidecar
		$sidecar.SidecarVolumeYaml = New-CppLogSidecarVolumeYaml -ItemIndent '  '
		$sidecar.SidecarConfigHash = Get-CppLogSidecarConfigHash
	}
	# 注解:zone Redis 口令指纹在前,Prometheus 抓取注解在后(与 New-CppPodAnnotationBlock 同序)。
	$podAnnotations = [ordered]@{}
	$zoneRedisAuth = Get-CppZoneRedisAuthAnnotations
	foreach ($key in $zoneRedisAuth.Keys) { $podAnnotations[$key] = $zoneRedisAuth[$key] }
	$metricsAnnotations = Get-CppMetricsAnnotations
	foreach ($key in $metricsAnnotations.Keys) { $podAnnotations[$key] = $metricsAnnotations[$key] }
	return New-ClientEntryPodCommon -Image $NodeImage -ImagePullPolicy $ImagePullPolicy -SnowflakeCacheDir $SnowflakeCacheDir `
		-LogVolumeSizeLimit $script:CppLogVolumeSizeLimit -LogPrunerCommand $script:CppLogPrunerCommand `
		-GrpcServerMaxPollers 0 -PodAnnotations $podAnnotations -OpenFileLimitMode $NodeOpenFileLimitMode -MetricsPort $CppMetricsPort @sidecar
}

<#
.SYNOPSIS
	一个 zone 的 external gate 地址计划(lib Resolve-GateClientEndpointPlan)。清单渲染(New-ExternalGateManifests)
	与集群现状比对(Get-GateAddressDrift)共用这一处:比对的就是本次真正要 apply 的计划。
#>
function Resolve-ZoneGatePlan {
	param(
		[Parameter(Mandatory = $true)][string]$CurrentZoneName,
		[Parameter(Mandatory = $true)][int]$Replicas,
		[Parameter(Mandatory = $true)][int]$NodePortBase
	)

	return Resolve-GateClientEndpointPlan -ServiceType $GateServiceType -Replicas $Replicas -ServicePort $GateServicePort `
		-NodePortBase $NodePortBase -ClientHostTemplate $GateClientHostTemplate -ClientPublicHost $ClientPublicHost `
		-ZoneName $CurrentZoneName
}

<#
.SYNOPSIS
	在本地渲染 -ClientEntryMode external 的 gate 全部清单(D87 / D88 / D89),不碰集群。

.DESCRIPTION
	地址计划(Resolve-ZoneGatePlan)是 StatefulSet 里 gate 自报端口与每序号 Service 暴露端口的唯一来源。
	Apply-Zone 在删除 Deployment 形态之前先调它:地址计划或模板校验失败就在任何删除 / apply 之前 throw。
#>
function New-ExternalGateManifests {
	param(
		[Parameter(Mandatory = $true)][string]$CurrentZoneName,
		[Parameter(Mandatory = $true)][int]$Replicas,
		[Parameter(Mandatory = $true)][int]$NodePortBase,
		[Parameter(Mandatory = $true)][string]$ConfigMapName
	)

	$plan = Resolve-ZoneGatePlan -CurrentZoneName $CurrentZoneName -Replicas $Replicas -NodePortBase $NodePortBase
	$podCommon = New-CppNodePodCommon -SidecarZoneLabel $CurrentZoneName
	return [pscustomobject]@{
		Plan            = $plan
		Replicas        = $Replicas
		HeadlessService = New-GateHeadlessServiceYaml -ServicePort $GateServicePort
		StatefulSet     = New-GateStatefulSetYaml -PodCommon $podCommon -Plan $plan -RpcPort 18000 -StartCommand "./gate" `
			-ConfigMapName $ConfigMapName -GateRouterMode $GateRouterMode
		# 副本数为 0 时为空串,apply 时跳过。
		OrdinalServices = New-GateOrdinalServicesYaml -Plan $plan -ExternalTrafficPolicy $GateExternalTrafficPolicy
		Pdb             = New-GatePdbYaml
	}
}

<#
.SYNOPSIS
	external gate 清单的服务端预演(kubectl apply --dry-run=server):Apply-Zone 在删除 Deployment 形态之前调用,DryRun 不调用。

.DESCRIPTION
	跨 zone 的 nodePort 冲突只有集群知道:分多次 zone-up 部署时,preflight 看不到别的 zone 的 gateNodePortBase。
	等到真正 apply 每序号 Service 才被拒,旧 gate 已经删了,本 zone 没有可回退的入口。服务端预演走同一套准入校验与
	nodePort 分配检查但不落库,被拒就在任何删除之前 throw。只需要 zone namespace 内的权限,不要求集群级 list services。
	整套清单一起预演(Service / StatefulSet / PDB),StatefulSet 不可变字段的冲突同样在删除之前暴露。
#>
function Test-ExternalGateServerSide {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)]$Manifests
	)

	$documents = @($Manifests.HeadlessService, $Manifests.OrdinalServices, $Manifests.StatefulSet, $Manifests.Pdb) |
		Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
	try {
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace, "--dry-run=server") -InputContent (@($documents) -join "`n---`n")
	}
	catch {
		throw "namespace $Namespace 的 external gate 清单服务端预演(--dry-run=server)被集群拒绝,未删除、未改动任何资源:$($_.Exception.Message)。常见原因是 gate-<i> 的 nodePort 已被其他 namespace 占用(或本 namespace 残留的 gate-entry 恰好占着同一端口):换一个不重叠的 gateNodePortBase,或先处理占用者。kubectl 的原始报错见上方输出。"
	}
}

<#
.SYNOPSIS
	apply New-ExternalGateManifests 的结果:headless → 每序号 Service → StatefulSet → 清理多余的每序号 Service → PDB。

.DESCRIPTION
	Service 先于 StatefulSet:gate 启动即按 nodePort 自报地址进 etcd,Service 被集群拒绝(例如 nodePort 冲突)时
	StatefulSet 还没建,不会出现"gate 已按冲突端口发布、玩家连到别的 zone"的状态。
	updateStrategy 为 OnDelete:apply 新模板不重建现有 Pod,换版本走 k8s_gate_drain.ps1 逐个排空删 Pod。
	缩容后 apply 不会回收多出来的 gate-<i>,由 Remove-StaleGateOrdinalServices 删除(否则继续占着 nodePort);
	放在 StatefulSet 之后,高序号 Pod 先被缩掉,再删它们的 Service。
	Deployment 形态的残留由 Apply-Zone 里的 Remove-ClientEntryObsoleteResources 先行删除。
#>
function Apply-ExternalGate {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)]$Manifests
	)

	$plan = $Manifests.Plan
	Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $Manifests.HeadlessService
	if (-not [string]::IsNullOrWhiteSpace($Manifests.OrdinalServices)) {
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $Manifests.OrdinalServices
	}
	Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $Manifests.StatefulSet
	Remove-StaleGateOrdinalServices -Namespace $Namespace -Replicas $Manifests.Replicas `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -DryRun:$DryRun
	Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $Manifests.Pdb

	$hostSource = if ($plan.HostTemplate) { "template=$($plan.HostTemplate)" } elseif ($plan.PublicHost) { "host=$($plan.PublicHost)" } else { "host=status.hostIP" }
	$portSource = if ($plan.PortMode -eq (Get-ClientEntryContract).GateClientPortModeNodePort) { "nodePort=$($plan.NodePortBase)+ordinal" } else { "port=$($plan.ClientPort)" }
	Write-Host "Gate (external): StatefulSet replicas=$($Manifests.Replicas) service_type=$($plan.ServiceType) etp=$GateExternalTrafficPolicy $hostSource $portSource"
}

<#
.SYNOPSIS
	-WaitReady 等 C++ 工作负载(gate / scene / battle)失败后,把容器日志里的 B0-4 拒启行捞出来打印。

.DESCRIPTION
	节点池 nofile 硬限低于需求时进程缺省拒启(open_file_limit.h),Pod 进 CrashLoop;拒启原因只在**上一个**容器实例的
	stderr 里(kubectl logs --previous),rollout status / describe 都看不到,不捞的话运维只看到「rollout 超时」。
	- 只看本工作负载里容器未就绪的 Pod(标签 app=<名字>,容器名 = <名字>:New-NodeDeploymentYaml、New-SceneFleetYaml 与
	  lib 三个生成器都按这个约定),已就绪的旧 Pod 不读;最多读 10 个,控制失败路径上的耗时。
	- 每个 Pod 先读 --previous;没有上一个实例(Agones GameServer 是 restartPolicy: Never,或刚崩第一次还没重启)时读当前实例。
	- 只读、尽力而为:kubectl 失败只告警不 throw,也不改变调用方的失败判定(调用方随后照常 throw)。DryRun 不调用集群。
#>
function Write-CppOpenFileLimitRefusals {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$WorkloadName
	)

	# 拒启行的固定前缀:node.cpp FatalStartupConfig("open file limit", ...) 写 stderr 的 "FATAL open file limit config: ..."。
	# 改那边的文案要同改这里;docs/ops/gate-kernel-tuning-runbook.md §3.1 与 tools/scripts/deploy-staging.sh 按同一行 grep。
	$marker = 'FATAL open file limit'
	$podScanLimit = 10
	# 拒启行是进程退出前最后几行之一(之后只有致命信号处理的输出),200 行足够,不拉整份日志。
	$tailLines = '200'

	if ($DryRun) {
		return
	}
	try {
		$baseArgs = @() + (Build-KubectlBaseArgs)
		$getArgs = $baseArgs + @('get', 'pods', '-n', $Namespace, '-l', "app=$WorkloadName", '-o', 'json')
		$out = & kubectl @getArgs 2>$null
		if ($LASTEXITCODE -ne 0) {
			Write-Warning "open file limit check skipped: kubectl get pods -l app=$WorkloadName -n $Namespace failed (exit $LASTEXITCODE)"
			return
		}
		$items = @((($out | Out-String) | ConvertFrom-Json -ErrorAction Stop).items | Where-Object { $null -ne $_ })
		# 容器状态缺席(还没调度 / 还在 Init)也算未就绪:sidecar 镜像拉不到时业务容器一次都不会启动,读日志失败即跳过。
		$notReady = @($items | Where-Object {
				$status = @($_.status.containerStatuses | Where-Object { $_.name -eq $WorkloadName })
				$status.Count -eq 0 -or $status[0].ready -ne $true
			})
		$scanned = @($notReady | Select-Object -First $podScanLimit)
		$hits = 0
		foreach ($pod in $scanned) {
			$podName = [string]$pod.metadata.name
			$currentArgs = $baseArgs + @('logs', $podName, '-n', $Namespace, '-c', $WorkloadName, '--tail', $tailLines)
			$previousArgs = $currentArgs + @('--previous')
			$source = 'previous'
			$logs = & kubectl @previousArgs 2>$null
			if ($LASTEXITCODE -ne 0) {
				$source = 'current'
				$logs = & kubectl @currentArgs 2>$null
				if ($LASTEXITCODE -ne 0) { continue }
			}
			foreach ($line in @($logs)) {
				if (([string]$line).Contains($marker)) {
					Write-Host "  [open_file_limit] pod=$podName ($source): $line"
					$hits++
				}
			}
		}
		if ($hits -gt 0) {
			Write-Host "  -> $WorkloadName refused to start: the nofile hard limit of this node pool is below the requirement. Lower GateMaxConnections / BattleMaxConnections, raise the container runtime LimitNOFILE, or redeploy with -NodeOpenFileLimitMode warn as a short-lived exemption (docs/ops/gate-kernel-tuning-runbook.md section 3.1)."
		}
		else {
			Write-Host "  no '$marker' line in the logs of $($scanned.Count) not-ready pod(s) (app=$WorkloadName, $($notReady.Count) not ready of $($items.Count))"
		}
	}
	catch {
		Write-Warning "open file limit check failed for app=$WorkloadName -n ${Namespace}: $($_.Exception.Message)"
	}
}

function Wait-ForDeploymentReady {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$DeploymentName,
		# C++ 节点(gate / scene 池 / battle):失败时多捞一遍 B0-4 拒启行(Write-CppOpenFileLimitRefusals)。Go / Java 服务不传。
		[switch]$CppNode
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
	if ($CppNode) {
		Write-CppOpenFileLimitRefusals -Namespace $Namespace -WorkloadName $DeploymentName
	}
	throw "Deployment rollout failed: namespace=$Namespace deployment=$DeploymentName"
}

# StatefulSet 版的 rollout 等待(etcd 与 kafka)。`kubectl rollout status statefulset/...`
# 对 RollingUpdate 策略的 StatefulSet 有效,全部副本 Ready 才返回 0。
# 之所以单独等它们:
#   * etcd —— 全局池 match 与所有 zone 服务启动第一件事就是注册进 etcd,etcd 没形成
#     quorum 之前把它们拉起来只会看到一串 "context deadline exceeded" 重启。
#   * kafka —— broker 的 readiness 是 API 级探测(见 manifests/infra/kafka.yaml:9092 在
#     日志恢复期间就已经 bind,TCP 探针会谎报就绪)。等它 Ready 之后再等
#     kafka-topic-init,Job 失败时看到的就是真正的契约错误,而不是"broker 还没起来"。
function Wait-ForStatefulSetReady {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$StatefulSetName
	)

	if ($DryRun) {
		Write-Host "[dry-run] kubectl rollout status statefulset/$StatefulSetName -n $Namespace --timeout ${WaitTimeoutSeconds}s"
		return
	}

	Invoke-Kubectl -Args @("rollout", "status", "statefulset/$StatefulSetName", "-n", $Namespace, "--timeout", ("{0}s" -f $WaitTimeoutSeconds)) -AllowFailure
	if ($LASTEXITCODE -eq 0) {
		return
	}

	Write-Host "StatefulSet not ready: namespace=$Namespace statefulset=$StatefulSetName"
	# PVC 一起列:StatefulSet 起不来最常见的原因是 PVC Pending(没有默认 StorageClass)。
	Invoke-Kubectl -Args @("get", "pods,pvc", "-n", $Namespace, "-l", "app=$StatefulSetName", "-o", "wide") -AllowFailure
	Invoke-Kubectl -Args @("describe", "statefulset", $StatefulSetName, "-n", $Namespace) -AllowFailure
	throw "StatefulSet rollout failed: namespace=$Namespace statefulset=$StatefulSetName"
}

# 一次性 Job 的完成等待(目前只有 kafka-topic-init 用)。
# `kubectl wait --for=condition=complete` 只认 Complete:Job 失败(condition=Failed,backoffLimit 用尽)时
# 它会一直等到超时,所以超时后把 describe 与 Pod 日志都打出来再 throw —— 分区契约不一致
# ("PartitionCount=1 but the contract is 6")就是从这里看到的。
function Wait-ForJobComplete {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$JobName
	)

	if ($DryRun) {
		Write-Host "[dry-run] kubectl wait --for=condition=complete job/$JobName -n $Namespace --timeout ${WaitTimeoutSeconds}s"
		return
	}

	Invoke-Kubectl -Args @("wait", "--for=condition=complete", "job/$JobName", "-n", $Namespace, "--timeout", ("{0}s" -f $WaitTimeoutSeconds)) -AllowFailure
	if ($LASTEXITCODE -eq 0) {
		return
	}

	Write-Host "Job not complete: namespace=$Namespace job=$JobName"
	Invoke-Kubectl -Args @("describe", "job", $JobName, "-n", $Namespace) -AllowFailure
	Invoke-Kubectl -Args @("logs", "job/$JobName", "-n", $Namespace, "--all-containers", "--tail", "100") -AllowFailure
	throw "Job did not complete: namespace=$Namespace job=$JobName"
}

function Wait-ForZoneReady {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		# 本 zone 实际生成的 scene Deployment 名字(legacy 单池 = @("scene"),
		# 拆分模式 = @("scene-world","scene-instance"))。副本数为 0 的池不等。
		[string[]]$SceneDeploymentNames = @("scene"),
		# external 下 gate StatefulSet 要等到的就绪副本数。
		[Parameter(Mandatory = $true)][int]$GateReplicas
	)

	if (-not $WaitReady) {
		return
	}

	Write-Host "Waiting for zone workloads to become ready: namespace=$Namespace"
	if ($ClientEntryMode -eq "external") {
		# 不能用 Wait-ForStatefulSetReady:kubectl rollout status 对 updateStrategy: OnDelete 的 StatefulSet 直接报错。
		# 库函数有界轮询 readyReplicas;OnDelete 下等到的是"现存 Pod 已就绪",新模板何时生效由排空脚本逐个删 Pod 决定。
		# 失败时先捞 B0-4 拒启行再原样抛出(库函数不读本脚本变量,诊断留在这一侧)。
		try {
			Wait-ForGateStatefulSetReady -Namespace $Namespace -ExpectedReplicas $GateReplicas -TimeoutSeconds $WaitTimeoutSeconds `
				-KubeContext $KubeContext -KubeConfig $KubeConfig -DryRun:$DryRun
		}
		catch {
			Write-CppOpenFileLimitRefusals -Namespace $Namespace -WorkloadName 'gate'
			throw
		}
	}
	else {
		Wait-ForDeploymentReady -Namespace $Namespace -DeploymentName "gate" -CppNode
	}

	if ($SceneOrchestrator -eq "agones") {
		# Fleet 不是 Deployment,`kubectl rollout status` 对它无效(会直接报
		# "no matches for kind")。这里不假装等过,而是把该看的命令打出来。
		# 真正的就绪判据是 Fleet 的 status.readyReplicas。
		foreach ($fleetName in $SceneDeploymentNames) {
			Write-Host "  [agones] scene fleet '$fleetName' readiness is NOT waited on by this script."
			Write-Host "           kubectl -n $Namespace get fleet $fleetName -o jsonpath='{.status.readyReplicas}'"
			Write-Host "           kubectl -n $Namespace get gameservers -l app=$fleetName"
			# 没有等待也就没有失败诊断:B0-4 拒启(nofile 硬限不够)要运维自己看。GameServer Pod 不重启,读当前实例日志。
			Write-Host "           kubectl -n $Namespace logs -l app=$fleetName -c $fleetName --tail 200 | Select-String 'FATAL open file limit'"
		}
	}
	else {
		foreach ($sceneDeployment in $SceneDeploymentNames) {
			Wait-ForDeploymentReady -Namespace $Namespace -DeploymentName $sceneDeployment -CppNode
		}
	}

	if (-not $SkipGoSvc -and -not [string]::IsNullOrWhiteSpace($GoSvcRegistry)) {
		# 全局服务(match)不在 zone namespace 里,等它会直接超时
		foreach ($svcName in (Get-ZoneScopedGoSvcNames)) {
			Wait-ForDeploymentReady -Namespace $Namespace -DeploymentName $svcName
		}
	}
	if (-not $SkipJavaSvc -and -not [string]::IsNullOrWhiteSpace($JavaSvcRegistry)) {
		foreach ($svcName in $JavaSvcCatalogue.Keys) {
			# 目录里有条目但没有 manifest 的服务(auth:仓库里没有可打包的工程,
			# manifests/java-svc/auth.yaml 也不存在)在 Apply-JavaSvcManifests 里只告警不 apply,
			# 这里若照等就会在 `rollout status deploy/auth` 上白白超时,拖垮整条 -WaitReady。
			if (-not (Test-Path (Join-Path $JavaSvcManifestsDir $JavaSvcCatalogue[$svcName].Manifest))) {
				Write-Host "  [skip-wait] ${svcName}: manifest missing; not deployed; skipping wait"
				continue
			}
			Wait-ForDeploymentReady -Namespace $Namespace -DeploymentName $svcName
		}
	}
	Write-Host "Zone workloads are ready: namespace=$Namespace"
}

<#
.SYNOPSIS
	login.yaml 顶层的 Queue 段(G4b)。关 = 空串(不写 → go-zero 零值 Enabled=false,产物与 G4b 之前逐字相同)。
.DESCRIPTION
	- 除 Enabled / DispatcherLockKey / ZoneCapacityOverride 外,全部从 go/login/etc/login.yaml 镜像(契约值单一来源)。
	- DispatcherLockKey = <login.yaml 的基名>:zone:<本 zone>。login 把这个配置值当**前缀**、自己再拼一次 :zone:<Node.ZoneId>
	  (G1b,loginqueue.ZoneDispatcherLockKey),所以实际锁键是 <基名>:zone:<id>:zone:<id>:每个 zone 仍恰有一个 leader,只是排查时
	  键名要按它查。这里显式带 zone 防的是另一种情形:kubectl rollout undo 把 login 退回 G1b 之前的镜像,而 ConfigMap 不随之回退、
	  仍是开排队的这一份。旧镜像原样使用配置值,写全局基名就会退回「全集群一个 leader、其它 zone 的队列不出队」。同一 zone
	  新旧镜像混跑的窗口里两把键不互斥,短暂双 leader,最多多放行一批(docs/design/login-queue-2026-05.md §7.1)。
	  这一行依赖「配置值是前缀、锁按 zone 拆」:login 若改成「一个 leader 覆盖全部 zone」,必须删掉显式的 zone 后缀,
	  否则 N 个 leader 同时出队所有 zone、超发准入(契约测试钉住 ZoneDispatcherLockKey 的前缀语义)。
	- ZoneCapacityOverride 只写本 zone:每个 zone 的 login 只出队本 zone。不配会落到 loginqueue.FreeSlots 的回落公式
	  (max(在线, 1) × SoftCapMultiplier 向下取整,在线 0 / 1 人时容量为 1),队列非空时可能永远放不出人。
	- 调用方:生成 login 的 ConfigMap / Pod 指纹(New-GoSvcConfigMapYaml、Apply-OneGoSvc),两处拿到的是同一段文本;另有写路径入口的
	  Show-PeakShavingPlan 先求一次、丢弃产物,让容量越界与 login.yaml 缺 Queue 键在任何写操作之前 throw(那两处已在 Apply-Zone 中途)。
#>
function New-LoginQueueYaml {
	param([Parameter(Mandatory = $true)][int]$CurrentZoneId)

	if (-not $script:LoginQueueOn) { return '' }
	$y = 'go/login/etc/login.yaml'
	$dispatchInterval = Get-AuthoritativeScalar -RelativePath $y -KeyPath 'Queue.DispatchInterval'
	$admitTtl         = Get-AuthoritativeScalar -RelativePath $y -KeyPath 'Queue.AdmitTTL'
	$entryTtl         = Get-AuthoritativeScalar -RelativePath $y -KeyPath 'Queue.QueueEntryTTL'
	$softCap          = Get-AuthoritativeScalar -RelativePath $y -KeyPath 'Queue.SoftCapMultiplier'
	$retryAfterMs     = Get-AuthoritativeScalar -RelativePath $y -KeyPath 'Queue.DefaultRetryAfterMs'
	$lockTtl          = Get-AuthoritativeScalar -RelativePath $y -KeyPath 'Queue.DispatcherLockTTL'
	$lockKeyBase      = Get-AuthoritativeScalar -RelativePath $y -KeyPath 'Queue.DispatcherLockKey'
	$capacity = [long]$script:PeakShavingLaunchTier.LoginQueueZoneCapacity
	if ($capacity -lt 1 -or $capacity -gt [uint32]::MaxValue) {
		throw "内部错误:LoginQueueZoneCapacity=$capacity 不在 1..$([uint32]::MaxValue)(Go 侧是 uint32)。"
	}
	if ($CurrentZoneId -lt 1) { throw "生成 login Queue 段失败:ZoneId=$CurrentZoneId 必须为正。" }

	return @"
# 登录排队(G4b,按 -LoginQueue / -ReleaseProfile 生成)。除 Enabled / DispatcherLockKey / ZoneCapacityOverride 三项外全部镜像 go/login/etc/login.yaml。
# DispatcherLockKey 带本 zone:新镜像把它当前缀再拼一次 zone(实际锁键 ...:zone:N:zone:N),G1b 之前的旧镜像原样用,两种都按 zone 隔离
# (见 k8s_deploy.ps1 New-LoginQueueYaml)。
# ZoneCapacityOverride 是本 zone 同时在线上限(不是速率),开服档起步值,待压测确认。
Queue:
  Enabled: true
  DispatchInterval: $dispatchInterval
  AdmitTTL: $admitTtl
  QueueEntryTTL: $entryTtl
  SoftCapMultiplier: $softCap
  DefaultRetryAfterMs: $retryAfterMs
  DispatcherLockTTL: $lockTtl
  DispatcherLockKey: "${lockKeyBase}:zone:${CurrentZoneId}"
  ZoneCapacityOverride:
    "${CurrentZoneId}": $capacity
"@
}

<#
.SYNOPSIS
	第三方登录参数(-LoginWeChatAppId / -LoginWeChatEndpoint / -LoginQQAppId / -LoginQQEndpoint)的形状校验。纯函数:只看入参,
	返回错误文本数组(空 = 合法),不抛、不打印。

.DESCRIPTION
	调用方:写路径入口的 Assert-LoginAuthProviderParams(在密钥解析与任何写操作之前一次报全)与生成器 New-LoginAuthProvidersYaml
	(纵深防御,同一份规则)。规则对齐 login 的启动期门禁(go/login/internal/config/auth_providers.go checkAuthAppId /
	checkOAuthEndpoint),但部署侧更严,宁可在部署时拒也不留一个拒启的 Pod:
	  - AppId 空串 = 不配该 provider;非空只允许 [A-Za-z0-9_.-](公开的应用号,原样写进 YAML 双引号标量,不许任何需要转义的字符)。
	  - Endpoint 空串 = 生产地址;非空须是 https://主机[:端口][/路径],dev 档也接受 http://(login 只在 Mode=dev / test 放行 http,
	    而 Mode: dev 只在 dev 档写);不带用户信息、查询串与片段;不得含 `${`(login 不展开环境变量,出现即拒启)。scheme 只认小写。
	  - 给了 Endpoint 却没给对应 AppId:Endpoint 只在渲染该 provider 时才写,静默丢掉等于让人以为指向了沙箱。
	正则一律以 \z 收尾:.NET 的 $ 会放过结尾的一个换行,换行进 YAML 就是注入。
	报错不回显 Endpoint 的值(可能带用户信息里的口令),AppId 是公开值但同样不回显,点参数名即可定位。
#>
function Get-LoginAuthProviderParamErrors {
	param(
		[AllowEmptyString()][string]$WeChatAppId = '',
		[AllowEmptyString()][string]$WeChatEndpoint = '',
		[AllowEmptyString()][string]$QQAppId = '',
		[AllowEmptyString()][string]$QQEndpoint = '',
		[Parameter(Mandatory = $true)][ValidateSet('dev', 'staging', 'prod')][string]$ReleaseProfile
	)

	$errors = [System.Collections.Generic.List[string]]::new()
	$providers = @(
		[pscustomobject]@{ Name = 'WeChat'; AppIdParam = '-LoginWeChatAppId'; EndpointParam = '-LoginWeChatEndpoint'; AppId = $WeChatAppId; Endpoint = $WeChatEndpoint }
		[pscustomobject]@{ Name = 'QQ'; AppIdParam = '-LoginQQAppId'; EndpointParam = '-LoginQQEndpoint'; AppId = $QQAppId; Endpoint = $QQEndpoint }
	)
	foreach ($p in $providers) {
		if (-not [string]::IsNullOrEmpty($p.AppId) -and $p.AppId -cnotmatch '^[A-Za-z0-9_.-]+\z') {
			$errors.Add("$($p.AppIdParam) 只允许 [A-Za-z0-9_.-](AuthProviders.$($p.Name).AppId 是公开的应用号,原样写进 login ConfigMap)。")
		}
		if ([string]::IsNullOrEmpty($p.Endpoint)) { continue }
		if ([string]::IsNullOrEmpty($p.AppId)) {
			$errors.Add("给了 $($p.EndpointParam) 却没给 $($p.AppIdParam):Endpoint 只在渲染 AuthProviders.$($p.Name) 时才写,不能静默丢掉。")
		}
		if ($p.Endpoint.Contains('${')) {
			$errors.Add("$($p.EndpointParam) 含未展开的 `${...}:login 不展开环境变量,出现即拒绝启动。")
			continue
		}
		$m = [regex]::Match($p.Endpoint, '^(?<scheme>https?)://(?<host>[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?)(?::(?<port>[0-9]{1,5}))?(?<path>/[A-Za-z0-9._~%/-]*)?\z')
		if (-not $m.Success -or ($m.Groups['port'].Success -and ([int]$m.Groups['port'].Value -lt 1 -or [int]$m.Groups['port'].Value -gt 65535))) {
			$errors.Add("$($p.EndpointParam) 须是 https://主机[:端口][/路径](dev 档也接受 http://),不带用户信息、查询串与片段,路径只用 [A-Za-z0-9._~%/-]。")
			continue
		}
		if ($m.Groups['scheme'].Value -ceq 'http' -and $ReleaseProfile -ne 'dev') {
			$errors.Add("$($p.EndpointParam) 在 -ReleaseProfile $ReleaseProfile 只接受 https://:http:// 只在 dev 档放行(login 生产模式拒绝 http,Endpoint 上的请求带着 AppSecret / access_token)。")
		}
	}
	# 逐条写进管道:调用方一律用 @(...) 收集,合法时得到空数组。
	return $errors.ToArray()
}

<#
.SYNOPSIS
	login.yaml 顶层的 AuthProviders 段(第三方登录,-LoginWeChatAppId / -LoginQQAppId)。两个 AppId 都为空 = 空串(不写 AuthProviders,
	login 模板的产物与接入前逐字相同)。纯函数:只看入参,参数不合法即 throw。

.DESCRIPTION
	键名核对自 go/login/internal/config/config.go WeChatAuthConf / QQAuthConf:
	  - WeChat:AppId + AppSecretEnv(环境变量名,值由 Apply-OneGoSvc 经 Secret login-oauth 注入容器);**从不写 AppSecret**
	    (生产模式拒绝内联密钥,dev 也不需要)。
	  - QQ:只写 AppId(provider 只用 access_token 调 /oauth2.0/me 并校验 client_id,不需要 AppKey);RequestUnionId 不写 = login 缺省 true。
	  - RequireUnionId 只在 -Login*AllowOpenId 时写 false;不写 = login 缺省 true(拿不到 unionid 即拒绝登录,不回落 openid)。
	  - Endpoint 只在给了时写(沙箱 mock);不写 = 生产地址。
	AuthProviders 在整份 login.yaml 里只能出现一次(重复的顶层键各 YAML 解析器行为不一),login 模板别处不得再写这个键。
	值全部写成 YAML 双引号标量;字符集已由 Get-LoginAuthProviderParamErrors 限定,不需要转义。
	调用方:New-GoSvcConfigMapYaml 生成 login 的 ConfigMap。
#>
function New-LoginAuthProvidersYaml {
	param(
		[AllowEmptyString()][string]$WeChatAppId = '',
		[AllowEmptyString()][string]$WeChatEndpoint = '',
		[bool]$WeChatAllowOpenId = $false,
		[AllowEmptyString()][string]$QQAppId = '',
		[AllowEmptyString()][string]$QQEndpoint = '',
		[bool]$QQAllowOpenId = $false,
		[Parameter(Mandatory = $true)][ValidateSet('dev', 'staging', 'prod')][string]$ReleaseProfile,
		# login 容器里存放微信 AppSecret 的环境变量名(调用方传 $script:LoginWeChatAppSecretEnv,与注入 env 的名字同源)。
		[Parameter(Mandatory = $true)][string]$WeChatAppSecretEnv
	)

	$errors = @(Get-LoginAuthProviderParamErrors -WeChatAppId $WeChatAppId -WeChatEndpoint $WeChatEndpoint `
		-QQAppId $QQAppId -QQEndpoint $QQEndpoint -ReleaseProfile $ReleaseProfile)
	if ($errors.Count -gt 0) {
		throw ("生成 login ConfigMap 的 AuthProviders 段失败:" + ($errors -join ' '))
	}
	if ($WeChatAppSecretEnv -cnotmatch '^[A-Z][A-Z0-9_]*\z') {
		throw "内部错误:微信 AppSecret 的环境变量名 '$WeChatAppSecretEnv' 不是 [A-Z][A-Z0-9_]* 形状,拒绝渲染。"
	}
	$hasWeChat = -not [string]::IsNullOrEmpty($WeChatAppId)
	$hasQQ = -not [string]::IsNullOrEmpty($QQAppId)
	if (-not $hasWeChat -and -not $hasQQ) { return '' }

	$lines = [System.Collections.Generic.List[string]]::new()
	$lines.Add('# 第三方登录(k8s_deploy.ps1 -LoginWeChatAppId / -LoginQQAppId,New-LoginAuthProvidersYaml 生成)。只写公开值:微信 AppSecret 经 Secret')
	$lines.Add('# 注入 AppSecretEnv 指向的容器环境变量,QQ 不需要密钥。RequireUnionId 不写 = login 缺省 true(拿不到 unionid 即拒绝登录)。')
	$lines.Add('AuthProviders:')
	if ($hasWeChat) {
		$lines.Add('  WeChat:')
		$lines.Add("    AppId: `"$WeChatAppId`"")
		$lines.Add("    AppSecretEnv: `"$WeChatAppSecretEnv`"")
		if ($WeChatAllowOpenId) { $lines.Add('    RequireUnionId: false') }
		if (-not [string]::IsNullOrEmpty($WeChatEndpoint)) { $lines.Add("    Endpoint: `"$WeChatEndpoint`"") }
	}
	if ($hasQQ) {
		$lines.Add('  QQ:')
		$lines.Add("    AppId: `"$QQAppId`"")
		if ($QQAllowOpenId) { $lines.Add('    RequireUnionId: false') }
		if (-not [string]::IsNullOrEmpty($QQEndpoint)) { $lines.Add("    Endpoint: `"$QQEndpoint`"") }
	}
	return ($lines -join "`n")
}

<#
.SYNOPSIS
	写路径入口的第三方登录参数门禁:形状不对就在密钥解析与任何写操作之前 throw(一次报全),并按需打英文警告。
	由 Assert-ClientEntryDeployPreflight 调用(与 -LoginDevPasswordAuth 的门禁同处)。

.DESCRIPTION
	规则只有一份(Get-LoginAuthProviderParamErrors);New-LoginAuthProvidersYaml 生成时再查一次作纵深防御。
	参数非法就拒绝,不论本次是否部署 login:错的参数配在不部署 login 的命令上,下次带同一条命令部署 login 时照样会错。
	不部署 login 时给了这些参数只告警「不生效」,与 -LoginDevPasswordAuth 同口径。
#>
function Assert-LoginAuthProviderParams {
	param([Parameter(Mandatory = $true)][bool]$DeploysLogin)

	$errors = @(Get-LoginAuthProviderParamErrors -WeChatAppId $LoginWeChatAppId -WeChatEndpoint $LoginWeChatEndpoint `
		-QQAppId $LoginQQAppId -QQEndpoint $LoginQQEndpoint -ReleaseProfile $ReleaseProfile)
	if ($errors.Count -gt 0) {
		throw ("login 第三方登录参数校验失败($($errors.Count) 项),未做任何写操作:`n  - " + ($errors -join "`n  - "))
	}

	$hasWeChat = -not [string]::IsNullOrEmpty($LoginWeChatAppId)
	$hasQQ = -not [string]::IsNullOrEmpty($LoginQQAppId)
	if ($LoginWeChatAllowOpenId -and -not $hasWeChat) {
		Write-Warning "[login-auth] -LoginWeChatAllowOpenId supplied without -LoginWeChatAppId; flag has no effect (AuthProviders.WeChat is not rendered)."
	}
	if ($LoginQQAllowOpenId -and -not $hasQQ) {
		Write-Warning "[login-auth] -LoginQQAllowOpenId supplied without -LoginQQAppId; flag has no effect (AuthProviders.QQ is not rendered)."
	}
	if ($ReleaseProfile -ne 'dev') {
		if ($LoginWeChatAllowOpenId -and $hasWeChat) {
			Write-Warning "[login-auth] -LoginWeChatAllowOpenId (ReleaseProfile=$ReleaseProfile): WeChat accounts are created by openid when unionid is missing; connecting unionid later splits each such player into two accounts that cannot be merged."
		}
		if ($LoginQQAllowOpenId -and $hasQQ) {
			Write-Warning "[login-auth] -LoginQQAllowOpenId (ReleaseProfile=$ReleaseProfile): QQ accounts are created by openid when unionid is missing; connecting unionid later splits each such player into two accounts that cannot be merged."
		}
	}
	if (($hasWeChat -or $hasQQ) -and -not $DeploysLogin) {
		Write-Warning "[login-auth] -LoginWeChatAppId / -LoginQQAppId supplied, but login is not deployed in this run (only generated with Go services during zone-up / all-up; requires -GoSvcRegistry without -SkipGoSvc); flags have no effect."
	}
	if ($ConfirmLoginAuthProviderChange -and -not $DeploysLogin) {
		Write-Warning "[login-auth] -ConfirmLoginAuthProviderChange supplied, but login is not deployed in this run; flag has no effect."
	}
}

<#
.SYNOPSIS
	login 第三方登录的防误关闸(集群现状预检,非 DryRun):本次要部署 login 的每个 zone,线上配置会被本次参数关掉或改掉,
	就在任何写操作之前拒绝,所有 zone 一次报全。由 Assert-ClientEntryDeployPreflight 在 Assert-ClientEntryClusterState 之后调用。

.DESCRIPTION
	六个 -Login* 参数不粘滞(见参数注释),漏传一个 apply 照样成功,玩家却登不上:比 gate 换形态更隐蔽,因为 login 不报错,
	只是 provider 没注册或 unionid 校验翻转。判据只有一份,在库里(lib Get-LoginAuthProviderDrift,k8s_zone_rollback.ps1
	第 0 步用同一份);线上现状由 Get-LoginAuthProviderLiveState 读(login ConfigMap 的 AuthProviders 段 + Secret login-oauth
	是否存在,名字取自库契约,与本脚本 $script:GoSvcInfo.login / $script:LoginOAuthSecretName 的相等由契约测试钉住)。
	给了 -ConfirmLoginAuthProviderChange 只打警告放行。任何 kubectl 失败都 throw(查不到不等于没开,fail-closed)。
	-Zones 与 Assert-ClientEntryClusterState 同一份(zone-up 一个,all-up 取 zones 配置);all-up 逐个 zone 部署,所以必须在
	第一个写操作之前把全部 zone 判完,不留「前几个 zone 已关掉第三方登录、后面的才被拒」的半截部署。
#>
function Assert-LoginAuthProviderClusterState {
	param([Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Zones)

	$problems = @()
	foreach ($zone in $Zones) {
		$namespace = Get-ZoneNamespace -Name $zone.Name
		$state = Get-LoginAuthProviderLiveState -Namespace $namespace -KubeContext $KubeContext -KubeConfig $KubeConfig
		$drift = Get-LoginAuthProviderDrift -Live $state.Live -OAuthSecretPresent $state.OAuthSecretPresent `
			-WeChatAppId $LoginWeChatAppId -WeChatEndpoint $LoginWeChatEndpoint -WeChatAllowOpenId $LoginWeChatAllowOpenId.IsPresent `
			-QQAppId $LoginQQAppId -QQEndpoint $LoginQQEndpoint -QQAllowOpenId $LoginQQAllowOpenId.IsPresent
		if ($drift.Drifts.Count -eq 0) { continue }
		$keep = if ($drift.KeepArgs) { "要保持现状请照传 $($drift.KeepArgs)" } else { '线上没有可照传的 provider' }
		$text = "zone $($zone.Name)(namespace=$namespace):" + ($drift.Drifts -join ';') + "。$keep;确认要关闭或改动请加 -ConfirmLoginAuthProviderChange。"
		if ($ConfirmLoginAuthProviderChange) {
			Write-Warning "[login-auth] -ConfirmLoginAuthProviderChange supplied; allowed: $text"
		}
		else {
			$problems += $text
		}
	}
	if ($problems.Count -gt 0) {
		throw ("login 第三方登录现状预检失败($($problems.Count) 个 zone),未做任何写操作:`n  - " + ($problems -join "`n  - "))
	}
}

# ─────────────────────────────────────────────────────────────────
# D1 玩家归属与落点映射持久化的部署载体(第 1b-3 批:docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §7、§13.4)
# ─────────────────────────────────────────────────────────────────
# 参数、切换与回退顺序见参数 -RouteStoreMode 处的注释。本节的函数:
#   Get-RouteStoreWaitAOFNumReplicas / Get-RouteStoreDeploySettings  data-service ConfigMap 的 RouteStore 段取值(纯函数)
#   Get-RouteSwitchParamErrors                                       开关组合门禁(纯函数)
#   ConvertFrom-RouteSwitchLiveYaml / Get-RouteSwitchDrift / Get-RouteSwitchKeepArgs   线上现状的解析与比对(纯函数)
#   Read-LiveRouteSwitchFacts                                        读线上 ConfigMap(只读;Invoke-ClientEntryKubectl 不看 DryRun,由调用方判)
#   Assert-RouteSwitchClusterState / Assert-RouteSwitchPreflight     写路径入口的两道闸
# 纯函数只依赖参数、$RepoRoot 与 Get-AuthoritativeScalar / ConvertFrom-YamlToFlatMap:契约测试按 AST 抽出直接调用;
# 读集群的那两个在测试里给 Invoke-ClientEntryKubectl 替身。

# mapping Redis 的拓扑('single' / 'ha'),决定 WAITAOF 的副本确认份数:data-service 的 RouteStore.WaitAOFNumReplicas 与 go/db 的
# ServerConfig.RedisClient.WaitAOFNumReplicas 两处 ConfigMap 都按它渲染(单实例 0;高可用 1:哨兵切主时被提升的副本上要有这次写)。
# 共享 Redis 就是 mapping Redis,所以判据只有一个:与 Apply-Infra 选清单用同一个 Get-RedisTopology(-RedisReplicas + 档位,zone 写路径
# 没给参数时跟随线上,见 Resolve-RedisTopologyFromLive),不另存一份 —— 每次现取,跟随线上之后的渲染自然跟着变。
function Get-MappingRedisTopology {
	return Get-MappingRedisTopologyFor -Topology (Get-RedisTopology)
}

# WAITAOF 要求的副本确认份数:单实例 0,redis-ha(1 主 2 从)1。纯函数。
function Get-RouteStoreWaitAOFNumReplicas {
	param([Parameter(Mandatory = $true)][string]$Topology)
	switch -CaseSensitive ($Topology) {
		'single' { return 0 }
		'ha' { return 1 }
	}
	throw "内部错误:mapping Redis 拓扑 '$Topology' 不是 single / ha,拒绝渲染 RouteStore.WaitAOFNumReplicas。"
}

<#
.SYNOPSIS
	data-service ConfigMap 的 RouteStore 段取值。纯函数(读服务 yaml 与传入的开关),ConfigMap 生成器与写路径入口的自检共用这一处。

.DESCRIPTION
	- Mode / ReadRequiresReady / Reconcile.ArmReadyMarker:取开关(缺省 redis / false / false),不镜像服务 yaml。
	- FenceShards:按全局库后端分档。K8s 上 data-service 的全局库恒为 infra namespace 的 MySQL(ConfigMap 的 SnapshotMySQL.Host
	  = mysql.<infra>:3306),即 MySQL 档;MySQL 档取服务 yaml 的值(go/data_service/etc/data_service.yaml 就是 MySQL 档,16)。
	  **TiDB 档(64)等 TiDB Phase 1 的部署方式定了再补**:那时这里按后端选值,并在契约测试里钉住 TiDB 档。
	  不在 [1, 64] 即 throw(64 = placement.MaxRouteFenceShards,围栏持有者写满 0..63 全部分片的契约)。
	- WaitAOFNumLocal 固定 1(redis.yaml 与 redis-ha.yaml 的每个节点都开着 --appendonly yes);WaitAOFNumReplicas 按拓扑(Get-RouteStoreWaitAOFNumReplicas)。
	- WaitAOFTimeoutMs 取服务 yaml,不在 [1, 1500] 即 throw(data_service 的 Validate 同一上界 MaxRouteStoreWaitAOFTimeoutMs)。
	- Reconcile 段只写 ArmReadyMarker:其余键不写 = data_service 的代码缺省(Enabled 缺省 true、LeaseMs 10000、RedisPoolSize 4 …);
	  Redis 连接总账里 leader 的独立池就按这个代码缺省记(Get-RedisConnectionLedgerInputs),所以这里不写 RedisPoolSize。
	这些键在 redis 模式下没有运行效果,但 data_service 不分 Mode 校验 RouteStore(非法即拒启),所以照样先在部署侧核过。
#>
function Get-RouteStoreDeploySettings {
	param(
		[Parameter(Mandatory = $true)][string]$Mode,
		[Parameter(Mandatory = $true)][bool]$ArmReadyMarker,
		[Parameter(Mandatory = $true)][bool]$ReadRequiresReady,
		[Parameter(Mandatory = $true)][string]$Topology
	)
	if ($Mode -cnotin @('redis', 'db_first')) {
		throw "内部错误:RouteStore.Mode '$Mode' 不是 redis / db_first(参数绑定后应已规范成小写),拒绝渲染。"
	}
	$dsYaml = 'go/data_service/etc/data_service.yaml'
	$fenceRaw = [string](Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'RouteStore.FenceShards')
	$fence = 0
	if (-not [int]::TryParse($fenceRaw, [ref]$fence) -or $fence -lt 1 -or $fence -gt 64) {
		throw "生成 ConfigMap 失败:$dsYaml 的 RouteStore.FenceShards='$fenceRaw' 必须是 [1, 64] 内的整数(MySQL 档;64 = placement.MaxRouteFenceShards)。fail-closed:data_service 对越界值拒启。"
	}
	$timeoutRaw = [string](Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'RouteStore.WaitAOFTimeoutMs')
	$timeout = 0
	if (-not [int]::TryParse($timeoutRaw, [ref]$timeout) -or $timeout -lt 1 -or $timeout -gt 1500) {
		throw "生成 ConfigMap 失败:$dsYaml 的 RouteStore.WaitAOFTimeoutMs='$timeoutRaw' 必须是 [1, 1500] 内的整数(0 = WAITAOF 无限等待;上界给 2000ms 服务端预算留余量)。fail-closed:data_service 对越界值拒启。"
	}
	return [ordered]@{
		Backend            = 'tidb-required'
		Mode               = $Mode
		FenceShards        = $fence
		WaitAOFNumLocal    = 1
		WaitAOFNumReplicas = (Get-RouteStoreWaitAOFNumReplicas -Topology $Topology)
		WaitAOFTimeoutMs   = $timeout
		ReadRequiresReady  = $(if ($ReadRequiresReady) { 'true' } else { 'false' })
		ArmReadyMarker     = $(if ($ArmReadyMarker) { 'true' } else { 'false' })
	}
}

<#
.SYNOPSIS
	D1 开关的组合门禁:返回错误文本数组(空 = 合法)。纯函数。

.DESCRIPTION
	1. Arm 或任一读方开关为 true 而 Mode 不是 db_first:data_service 的 Validate 对 ArmReadyMarker / ReadRequiresReady 要求 db_first,
	   进集群就是 CrashLoop;go/db / guild 的读方开关在 redis 模式下没有人挂就绪标记(leader 只在 db_first 下装配),打开即全部 fail-closed。
	2. 任一读方开关为 true 而 Arm=false:Arm=false 时 leader 每拍撤掉现存标记(规格 M4),读方读不到可信标记,全部 fail-closed
	   (go/db 暂停存盘消费、data_service 全部回源、guild 全拒)。回退顺序因此是「先关读方、再关 Arm」(§13.5 第 1 条)。
	只看本次参数,不看本次部署了哪些服务:参数是对全服目标状态的声明(Mode 与 Arm 是全服单值)。
#>
function Get-RouteSwitchParamErrors {
	param(
		[Parameter(Mandatory = $true)][string]$Mode,
		[Parameter(Mandatory = $true)][bool]$ArmReadyMarker,
		[Parameter(Mandatory = $true)][bool]$ReadRequiresReady,
		[Parameter(Mandatory = $true)][bool]$DbRequireRouteReady,
		[Parameter(Mandatory = $true)][bool]$GuildRequireRouteReady
	)
	$errors = New-Object System.Collections.Generic.List[string]
	$readers = @()
	if ($ReadRequiresReady) { $readers += '-RouteReadRequiresReady' }
	if ($DbRequireRouteReady) { $readers += '-DbPlacementRequireRouteReady' }
	if ($GuildRequireRouteReady) { $readers += '-GuildRequireRouteReady' }
	$needDbFirst = @()
	if ($ArmReadyMarker) { $needDbFirst += '-RouteReconcileArm' }
	$needDbFirst += $readers
	if ($Mode -cne 'db_first' -and $needDbFirst.Count -gt 0) {
		$errors.Add("$($needDbFirst -join ' / ') 要求 -RouteStoreMode db_first(本次是 $Mode):redis 模式下 data_service 对 ArmReadyMarker / ReadRequiresReady 拒启(进集群即 CrashLoop),也没有对账 leader 挂就绪标记,读方开关一打开就全部 fail-closed")
	}
	if ($readers.Count -gt 0 -and -not $ArmReadyMarker) {
		$errors.Add("$($readers -join ' / ') 要求同时给 -RouteReconcileArm:Arm=false 时对账 leader 每拍撤掉就绪标记(规格 M4),读方读不到可信标记,全部 fail-closed(go/db 暂停存盘消费、data_service 全部回源、guild 全拒);回退顺序是先关读方开关、再关 Arm")
	}
	return [string[]]$errors.ToArray()
}

<#
.SYNOPSIS
	从线上 ConfigMap 里那份服务 yaml 读出 D1 开关的现状,返回 [ordered]@{ <本脚本参数名(不带 -)> = <规范值> }。纯函数。

.DESCRIPTION
	-Scope data-service → RouteStoreMode / RouteReconcileArm / RouteReadRequiresReady(RouteStore.Mode / RouteStore.Reconcile.ArmReadyMarker /
	RouteStore.ReadRequiresReady);db → DbPlacementRequireRouteReady(Placement.RequireRouteReady);guild → GuildRequireRouteReady
	(MergeMarkerRedis.RequireRouteReady)。缺键按服务的代码缺省:Mode 缺省 redis,三个布尔缺省 false(本批之前的 ConfigMap 都不写它们)。
	写法认不出(Mode 不是 redis / db_first、布尔不是小写 true / false)时记成 '<原文>?',与任何参数值都不相等 → 粘滞核对拒绝
	(fail-closed:看不懂不等于没开)。
#>
function ConvertFrom-RouteSwitchLiveYaml {
	param(
		[Parameter(Mandatory = $true)][ValidateSet('data-service', 'db', 'guild')][string]$Scope,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text
	)
	$scalars = (ConvertFrom-YamlToFlatMap -Text $Text).Scalars
	$read = {
		param([string]$KeyPath, [string[]]$Allowed, [string]$Default)
		if (-not $scalars.Contains($KeyPath)) { return $Default }
		$raw = ([string]$scalars[$KeyPath]).Trim()
		if ($raw -eq '') { return $Default }
		if ($Allowed -ccontains $raw) { return $raw }
		# 显式拼接，避免 PowerShell 把问号当成变量名的一部分而丢失非法值标记。
		return ($raw + '?')
	}
	$bools = @('true', 'false')
	switch ($Scope) {
		'data-service' {
			return [ordered]@{
				RouteStoreMode         = (& $read 'RouteStore.Mode' @('redis', 'db_first') 'redis')
				RouteReconcileArm      = (& $read 'RouteStore.Reconcile.ArmReadyMarker' $bools 'false')
				RouteReadRequiresReady = (& $read 'RouteStore.ReadRequiresReady' $bools 'false')
			}
		}
		'db' { return [ordered]@{ DbPlacementRequireRouteReady = (& $read 'Placement.RequireRouteReady' $bools 'false') } }
		'guild' { return [ordered]@{ GuildRequireRouteReady = (& $read 'MergeMarkerRedis.RequireRouteReady' $bools 'false') } }
	}
}

# 线上现状与本次参数逐键比较(只比 -Desired 里有的键),返回「-<参数> 线上 X,本次 Y」数组。纯函数。
function Get-RouteSwitchDrift {
	param(
		[Parameter(Mandatory = $true)][System.Collections.IDictionary]$Live,
		[Parameter(Mandatory = $true)][System.Collections.IDictionary]$Desired
	)
	$drifts = New-Object System.Collections.Generic.List[string]
	foreach ($key in @($Live.Keys)) {
		if (-not $Desired.Contains($key)) { continue }
		$liveValue = [string]$Live[$key]
		$desiredValue = [string]$Desired[$key]
		if ($liveValue -cne $desiredValue) { $drifts.Add("-$key 线上 $liveValue,本次 $desiredValue") }
	}
	return [string[]]$drifts.ToArray()
}

# 原样描述线上实况的参数串:布尔值全部显式写出,不依赖本轮已改为 true 的默认。旧 redis/off 仍会被全球写入口拒绝。
function Get-RouteSwitchKeepArgs {
	param([Parameter(Mandatory = $true)][System.Collections.IDictionary]$Live)
	$parts = @()
	foreach ($key in @($Live.Keys)) {
		$value = [string]$Live[$key]
		if ($value.EndsWith('?')) { return $null }
		if ($key -ceq 'RouteStoreMode') { $parts += "-RouteStoreMode $value"; continue }
		if ($value -cne 'true' -and $value -cne 'false') { return $null }
		$parts += ('-{0}:${1}' -f $key, $value)
	}
	return ($parts -join ' ')
}

<#
.SYNOPSIS
	读线上 D1 开关现状(只读 kubectl)。返回 [object[]],每项 [pscustomobject]@{ Scope; Namespace; Values }(Values 见 ConvertFrom-RouteSwitchLiveYaml)。

.DESCRIPTION
	- -ReadDataService:一次 kubectl get configmap --all-namespaces --field-selector metadata.name=go-svc-data-service-config,
	  只认名字以 "<-NamespacePrefix>-" 开头的 namespace(与 Read-PlayerInboxGateLiveFacts 同一口径;用别的前缀部署的 zone 看不到)。
	  读全部 zone 而不只读本次部署的 zone:Mode / Arm / ReadRequiresReady 是全服单值,本 zone 的 namespace 刚被删掉重建
	  (zone 回档 Step 1)或是新 zone 时,只读它自己等于不核对。
	- -DbZoneNames:逐个读这些 zone 的 go-svc-db-config(Placement.RequireRouteReady 逐 zone 打开,只核对本次部署的 zone)。
	  ConfigMap 不存在(新 zone、namespace 被删)= 没有现状,不比对。
	- -ReadGuild:读 infra namespace 的 go-svc-guild-config;不存在 = 没有现状。
	kubectl 失败、输出不是 JSON、ConfigMap 在却没有服务 yaml 那个数据键,一律 throw:读不到不等于没开(fail-closed,同 Read-LiveDbConfigMapYaml)。
	Invoke-ClientEntryKubectl 不看 $DryRun,DryRun 判断由调用方(Assert-RouteSwitchPreflight)负责。
#>
function Read-LiveRouteSwitchFacts {
	param(
		[bool]$ReadDataService,
		[AllowEmptyCollection()][string[]]$DbZoneNames = @(),
		[bool]$ReadGuild
	)
	$facts = New-Object System.Collections.Generic.List[object]
	$failClosed = '判不出 D1 映射开关的线上现状,拒绝部署;尚未做任何写操作。'
	# 取 ConfigMap 里服务 yaml 那个数据键的文本;ConfigMap 在而键不在 = 不是本脚本生成的形状,throw。
	$configFileText = {
		param($ConfigMap, [string]$ConfigFile, [string]$Source)
		$property = $null
		$dataProperty = $ConfigMap.PSObject.Properties['data']
		if ($null -ne $dataProperty -and $null -ne $dataProperty.Value) { $property = $dataProperty.Value.PSObject.Properties[$ConfigFile] }
		if ($null -eq $property -or [string]::IsNullOrWhiteSpace([string]$property.Value)) {
			throw "线上 $Source 存在,但没有 data.${ConfigFile}:不是本脚本生成的形状,$failClosed"
		}
		return [string]$property.Value
	}
	# 读一个 namespace 里的一份 ConfigMap:不存在返回 $null。
	$readOne = {
		param([string]$Namespace, $Info, [string]$Scope)
		$source = "namespace $Namespace 的 ConfigMap $($Info.ConfigMap)"
		$result = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'configmap', $Info.ConfigMap, '-n', $Namespace, '--ignore-not-found', '-o', 'json') `
			-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
		if ($result.ExitCode -ne 0) {
			throw "读取线上 $source 失败(kubectl 退出码 $($result.ExitCode)):$($result.Stderr)。fail-closed:读不到不等于没开,$failClosed"
		}
		$text = ([string]$result.Stdout).Trim()
		if ($text -eq '') { return $null }
		try { $configMap = $text | ConvertFrom-Json -ErrorAction Stop }
		catch { throw "线上 $source 的 kubectl 输出不是合法 JSON,$failClosed$($_.Exception.Message)" }
		$yaml = & $configFileText $configMap $Info.ConfigFile $source
		return [pscustomobject]@{ Scope = $Scope; Namespace = $Namespace; Values = (ConvertFrom-RouteSwitchLiveYaml -Scope $Scope -Text $yaml) }
	}

	if ($ReadDataService) {
		$info = $GoSvcCatalogue['data-service']
		$result = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'configmap', '--all-namespaces', '--field-selector', "metadata.name=$($info.ConfigMap)", '-o', 'json') `
			-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
		if ($result.ExitCode -ne 0) {
			throw "读取各 zone 线上的 ConfigMap $($info.ConfigMap) 失败(kubectl 退出码 $($result.ExitCode)):$($result.Stderr)。fail-closed:读不到不等于还是 redis 模式,$failClosed"
		}
		try { $list = ([string]$result.Stdout) | ConvertFrom-Json -ErrorAction Stop }
		catch { throw "各 zone 线上 ConfigMap $($info.ConfigMap) 的 kubectl 输出不是合法 JSON,$failClosed$($_.Exception.Message)" }
		$prefix = "$NamespacePrefix-"
		foreach ($item in @($list.items)) {
			if ($null -eq $item) { continue }
			$namespace = [string]$item.metadata.namespace
			if (-not $namespace.StartsWith($prefix, [System.StringComparison]::Ordinal)) { continue }
			$yaml = & $configFileText $item $info.ConfigFile "namespace $namespace 的 ConfigMap $($info.ConfigMap)"
			$facts.Add([pscustomobject]@{ Scope = 'data-service'; Namespace = $namespace; Values = (ConvertFrom-RouteSwitchLiveYaml -Scope 'data-service' -Text $yaml) })
		}
	}
	foreach ($dbZoneName in @($DbZoneNames | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })) {
		$fact = & $readOne (Get-ZoneNamespace -Name $dbZoneName) $GoSvcCatalogue['db'] 'db'
		if ($null -ne $fact) { $facts.Add($fact) }
	}
	if ($ReadGuild) {
		$fact = & $readOne $InfraNamespace $GoSvcCatalogue['guild'] 'guild'
		if ($null -ne $fact) { $facts.Add($fact) }
	}
	return [object[]]$facts.ToArray()
}

<#
.SYNOPSIS
	D1 开关的粘滞核对(防翻转;非 DryRun,写路径入口、任何写操作之前):线上现状与本次参数不一致就整条拒绝,所有不一致一次报全。

.DESCRIPTION
	谁和谁比:
	  - 本次部署 zone 的 Go 服务(zone-up / all-up,Test-ZoneDbDeployed):全部 zone 的 data-service(Mode / Arm / ReadRequiresReady),
	    以及本次部署的 zone 的 db(Placement.RequireRouteReady);
	  - 本次部署全局 Go 服务(Test-GlobalGoSvcDeployed):infra 的 guild(MergeMarkerRedis.RequireRouteReady)。本次要打开 guild 的开关
	    而不部署 zone(infra-up)时,另读全部 zone 的 data-service,只比 Mode 与 Arm —— guild 读方的前提是全服已 db_first 且 Arm=true,
	    参数里声明了不算数,要线上真是这样。
	给了 -ConfirmRouteSwitchChange 只打 [RouteSwitch]Confirmed 警告放行。任何 kubectl 失败都 throw(Read-LiveRouteSwitchFacts)。
	**读方前提不受 -ConfirmRouteSwitchChange 豁免**(review OPS-2(a) / B3-2):本次渲染的 ConfigMap 里有读方开关为 true
	(zone 的 ReadRequiresReady / Placement.RequireRouteReady,或 guild)时,本次**不**渲染的 zone 的 data-service 线上必须是
	Mode=db_first 且 Arm=true(认不出也算不满足);否则那个 zone 的副本当选 leader 就撤标记(规格 M4),读方全部 fail-closed
	(go/db 暂停存盘消费、guild 全拒)。这一条单独 throw:Confirm 只放行「本次要改的值」,放行不了「改完之后全服处于读方无标记可依的状态」。
	一个 data-service 都读不到(全新集群,infra-up 先于任何 zone)不算违反:guild 在第一个 zone 的 leader 挂上标记之前 fail-closed,不写错。
	-ZoneNames 与 Assert-ClientEntryClusterState 同一份(zone-up 一个,all-up 取 zones 配置,infra-up 为空):all-up 逐 zone 部署,
	必须在第一个写操作之前把全部 zone 判完,不留「前几个 zone 已翻过去、后面的才被拒」的半截部署。
#>
function Assert-RouteSwitchClusterState {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyCollection()][string[]]$ZoneNames,
		[Parameter(Mandatory = $true)][bool]$DeploysZoneGoSvc,
		[Parameter(Mandatory = $true)][bool]$DeploysGuild
	)
	$asText = { param([bool]$On) if ($On) { 'true' } else { 'false' } }
	$desired = [ordered]@{
		RouteStoreMode               = $RouteStoreMode
		RouteReconcileArm            = (& $asText ([bool]$RouteReconcileArm))
		RouteReadRequiresReady       = (& $asText ([bool]$RouteReadRequiresReady))
		DbPlacementRequireRouteReady = (& $asText ([bool]$DbPlacementRequireRouteReady))
		GuildRequireRouteReady       = (& $asText ([bool]$GuildRequireRouteReady))
	}
	$guildPrecondition = $DeploysGuild -and (-not $DeploysZoneGoSvc) -and ([bool]$GuildRequireRouteReady)
	# 不用 `$x = if (...) { @() }`:if 语句把空数组展开成 $null,传下去就成了一个 $null 的 zone 名。
	$dbZones = @()
	if ($DeploysZoneGoSvc) { $dbZones = @($ZoneNames) }
	$facts = @(Read-LiveRouteSwitchFacts -ReadDataService ($DeploysZoneGoSvc -or $guildPrecondition) -DbZoneNames $dbZones -ReadGuild $DeploysGuild)

	# 读方前提(见 DESCRIPTION):本次渲染出的读方开关,与本次渲染的 zone namespace。guild 读方为 true 时 $guildPrecondition 或
	# $DeploysZoneGoSvc 必有一个成立,所以有读方要核时全部 zone 的 data-service 都已读到。
	$renderedReaders = @()
	if ($DeploysZoneGoSvc -and $desired.RouteReadRequiresReady -ceq 'true') { $renderedReaders += '-RouteReadRequiresReady' }
	if ($DeploysZoneGoSvc -and $desired.DbPlacementRequireRouteReady -ceq 'true') { $renderedReaders += '-DbPlacementRequireRouteReady' }
	if ($DeploysGuild -and $desired.GuildRequireRouteReady -ceq 'true') { $renderedReaders += '-GuildRequireRouteReady' }
	$renderedNamespaces = @()
	if ($DeploysZoneGoSvc) {
		$renderedNamespaces = @($ZoneNames | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | ForEach-Object { Get-ZoneNamespace -Name $_ })
	}

	$problems = @()
	$preconditionProblems = @()
	foreach ($fact in $facts) {
		$compare = $desired
		if ($fact.Scope -ceq 'data-service' -and -not $DeploysZoneGoSvc) {
			$compare = [ordered]@{ RouteStoreMode = $desired.RouteStoreMode; RouteReconcileArm = $desired.RouteReconcileArm }
		}
		$drifts = @(Get-RouteSwitchDrift -Live $fact.Values -Desired $compare)
		if ($fact.Scope -ceq 'data-service' -and $renderedReaders.Count -gt 0 -and -not ($renderedNamespaces -ccontains $fact.Namespace) -and
			($fact.Values.RouteStoreMode -cne 'db_first' -or $fact.Values.RouteReconcileArm -cne 'true')) {
			# 本次不渲染它,Confirm 改不了它的值;这一条只进不可豁免的前提清单,不再进下面可确认的漂移清单(避免同一个 zone 报两遍)。
			$driftText = if ($drifts.Count -gt 0) { ($drifts -join ';') + ';' } else { '' }
			$preconditionProblems += ("data-service(namespace=$($fact.Namespace)):${driftText}线上 RouteStore.Mode=$($fact.Values.RouteStoreMode)、" +
				"Reconcile.ArmReadyMarker=$($fact.Values.RouteReconcileArm),本次部署不渲染它,而本次要打开 $($renderedReaders -join ' / ')")
			continue
		}
		if ($drifts.Count -eq 0) { continue }
		$keep = Get-RouteSwitchKeepArgs -Live $fact.Values
		# 只描述线上现状,不写「照传即可保持」:全球正式部署的入口闸只许 db_first 与全部开关为 true,照传线上的 redis / false 会在入口被拒(review OPS-9)。
		$keepText = if ($null -eq $keep) { '线上取值认不出(见上面带 ? 的值),先人工核对该 ConfigMap' } else { "线上现状对应的参数是 $keep" }
		$text = "$($fact.Scope)(namespace=$($fact.Namespace)):" + ($drifts -join ';') + ";$keepText"
		if ($ConfirmRouteSwitchChange) {
			Write-Warning "[RouteSwitch]Confirmed:-ConfirmRouteSwitchChange supplied; allowed: $text"
		}
		else {
			$problems += $text
		}
	}
	$dataServiceZones = @($facts | Where-Object { $_.Scope -ceq 'data-service' }).Count
	$dbZonesLive = @($facts | Where-Object { $_.Scope -ceq 'db' }).Count
	$guildLive = @($facts | Where-Object { $_.Scope -ceq 'guild' }).Count
	Write-Host "Route switch live state: data_service_zones=$dataServiceZones db_zones=$dbZonesLive guild=$guildLive drifts_refused=$($problems.Count) confirm_change=$(([bool]$ConfirmRouteSwitchChange)) precondition_refused=$($preconditionProblems.Count)"
	if ($preconditionProblems.Count -gt 0) {
		$text = "D1 读方开关的前提不满足($($preconditionProblems.Count) 个 zone),-ConfirmRouteSwitchChange 不豁免这一条,未做任何写操作:`n  - " + ($preconditionProblems -join "`n  - ") +
			"`n读方(data-service ReadRequiresReady / go/db Placement.RequireRouteReady / guild)只认对账 leader 挂的就绪标记:任一 zone 的 data-service 仍是 redis 或 Arm=false," +
			"leader 落到那里就撤标记(规格 M4),读方全部 fail-closed(go/db 暂停存盘消费、guild 全拒)。" +
			"改用 all-up 一次部署全部 zone(zones 配置要覆盖上面列出的 namespace,不再使用的 zone 先 zone-down);" +
			"infra-up 的 guild 读方排在全部 zone 之后(只想先部署 infra 的其它组件可带 -SkipGoSvc)。"
		if ($problems.Count -gt 0) {
			$text += "`n另有线上现状与本次参数不一致($($problems.Count) 处):`n  - " + ($problems -join "`n  - ")
		}
		throw $text
	}
	if ($problems.Count -gt 0) {
		throw ("D1 映射开关的线上现状与本次参数不一致($($problems.Count) 处),未做任何写操作:`n  - " + ($problems -join "`n  - ") +
			"`n确要按本次参数改动请加 -ConfirmRouteSwitchChange。全球正式部署的入口闸只许 db_first 与全部开关为 true,线上旧值(redis / false)不能照传保持;" +
			"首次切换顺序见 -RouteStoreMode 参数注释(先迁移与存量导入 / audit,再对账与就绪标记,最后读方)。")
	}
}

<#
.SYNOPSIS
	D1 开关的写路径入口闸:组合门禁 → 生成器自检 → 计划行 → 粘滞核对(非 DryRun)。任何一步失败都 throw,早于任何集群写操作。

.DESCRIPTION
	组合门禁(Get-RouteSwitchParamErrors)与生成器自检(Get-RouteStoreDeploySettings)DryRun 同样判:它们只看参数与仓库文件。
	生成器自检提前到这里的理由同 Show-PeakShavingPlan:服务 yaml 的 FenceShards / WaitAOFTimeoutMs 读不到或越界时,
	不要等到 Apply-Zone 中途(node-config / gate / scene 已 apply)才在生成 data-service ConfigMap 时失败。
	不受 -SkipPreflight 影响(那个开关只跳过发布预检),理由同 Assert-ClientEntryDeployPreflight。
#>
function Assert-RouteSwitchPreflight {
	$errors = @(Get-RouteSwitchParamErrors -Mode $RouteStoreMode -ArmReadyMarker ([bool]$RouteReconcileArm) `
			-ReadRequiresReady ([bool]$RouteReadRequiresReady) -DbRequireRouteReady ([bool]$DbPlacementRequireRouteReady) `
			-GuildRequireRouteReady ([bool]$GuildRequireRouteReady))
	if ($errors.Count -gt 0) {
		throw ("D1 映射开关的参数组合不合法,未做任何写操作:`n  - " + ($errors -join "`n  - "))
	}
	$deploysZoneGoSvc = [bool](Test-ZoneDbDeployed)
	$deploysGuild = [bool](Test-GlobalGoSvcDeployed)
	$settings = Get-RouteStoreDeploySettings -Mode $RouteStoreMode -ArmReadyMarker ([bool]$RouteReconcileArm) `
		-ReadRequiresReady ([bool]$RouteReadRequiresReady) -Topology (Get-MappingRedisTopology)
	$zoneSwitchesSet = ($RouteStoreMode -cne 'redis') -or ([bool]$RouteReconcileArm) -or ([bool]$RouteReadRequiresReady) -or ([bool]$DbPlacementRequireRouteReady)
	if ($zoneSwitchesSet -and -not $deploysZoneGoSvc) {
		Write-Warning "[RouteSwitch]NoEffect:-RouteStoreMode / -RouteReconcileArm / -RouteReadRequiresReady / -DbPlacementRequireRouteReady only take effect when zone Go services are deployed (zone-up / all-up with -GoSvcRegistry and without -SkipGoSvc); nothing is rendered for them in this run."
	}
	if ($GuildRequireRouteReady -and -not $deploysGuild) {
		Write-Warning "[RouteSwitch]NoEffect:-GuildRequireRouteReady only takes effect when global Go services are deployed (infra-up, or all-up without -SkipInfra, with -GoSvcRegistry and without -SkipGoSvc); nothing is rendered for it in this run."
	}
	if ($ConfirmRouteSwitchChange -and -not $deploysZoneGoSvc -and -not $deploysGuild) {
		Write-Warning "[RouteSwitch]NoEffect:-ConfirmRouteSwitchChange supplied, but neither zone nor global Go services are deployed in this run; flag has no effect."
	}
	Write-Host ("Route switches: mode={0} arm={1} read_requires_ready={2} db_require_route_ready={3} guild_require_route_ready={4} fence_shards={5}({6}) waitaof={7}/{8}/{9}ms redis_topology={10} confirm_change={11}" -f
		$settings.Mode, $settings.ArmReadyMarker, $settings.ReadRequiresReady, $(if ([bool]$DbPlacementRequireRouteReady) { 'true' } else { 'false' }),
		$(if ([bool]$GuildRequireRouteReady) { 'true' } else { 'false' }), $settings.FenceShards, $settings.Backend, $settings.WaitAOFNumLocal,
		$settings.WaitAOFNumReplicas, $settings.WaitAOFTimeoutMs, (Get-MappingRedisTopology), ([bool]$ConfirmRouteSwitchChange))
	if ($DryRun) {
		Write-Host "[dry-run] Skipping D1 route switch sticky check (live data-service / db / guild ConfigMaps); actual execution checks before any writes."
		return
	}
	if (-not $deploysZoneGoSvc -and -not $deploysGuild) { return }
	$zoneNames = @()
	if ($Command -eq 'zone-up') {
		$zoneNames = @($ZoneName)
	}
	elseif ($Command -eq 'all-up') {
		$zoneNames = @(Get-ZonesFromJson -Path (Resolve-ZonesConfigPath) | ForEach-Object { [string]$_.name })
	}
	Assert-RouteSwitchClusterState -ZoneNames $zoneNames -DeploysZoneGoSvc $deploysZoneGoSvc -DeploysGuild $deploysGuild
}

function Assert-GlobalMatchKafkaTopology {
	param($Topology)
	if ($Topology.ReplicationFactor -lt 3 -or $Topology.MinInsyncReplicas -lt 2) {
		throw '全球匹配结果提交要求Kafka至少3副本、min.insync.replicas至少2；不能以单broker声明耐久提交。'
	}
}

function Get-GlobalMatchDeploymentSettings {
	param([string]$Profile, [string]$TiDBHost, [uint32]$Region)
	$enabled = -not [string]::IsNullOrWhiteSpace($TiDBHost)
	if (-not $enabled -and $Profile -ne 'dev') {
		throw '全局匹配在 staging/prod 必须指定 -MatchTiDBHost(host:port)，禁止退回旧Redis权威。'
	}
	if ($enabled -and $Region -eq 0) {
		throw '全局匹配必须指定非零 -PhysicalRegion，使用真实机房编号。'
	}
	$hostValue = ''
	if ($enabled) {
		try { $hostValue = (Get-FriendSqlSettings -Profile 'dev' -TiDBHost $TiDBHost -DevelopmentHost '').Host }
		catch { throw '-MatchTiDBHost 必须是有效的 host:port，不接受凭据或DSN。' }
	}
	return @{ Enabled = $enabled; Host = $hostValue; Region = $Region }
}

function Get-TradeSqlSettings {
	param([string]$Profile, [string]$TiDBHost, [string]$DevelopmentHost)
	if ([string]::IsNullOrEmpty($TiDBHost) -and $Profile -ne 'dev') {
		throw '全服交易在 staging/prod 必须显式提供 -TradeTiDBHost(host:port)，禁止回落到区域MySQL。'
	}
	try { return Get-FriendSqlSettings -Profile 'dev' -TiDBHost $TiDBHost -DevelopmentHost $DevelopmentHost }
	catch { throw '-TradeTiDBHost 必须是有效的 host:port，不接受凭据、DSN 或其他配置内容。' }
}

function Get-GlobalAuthoritySqlSettings {
	param([string]$Profile, [string]$CanonicalHost, [string]$FriendHost, [string]$TradeHost, [string]$MatchHost, [string]$DevelopmentHost)
	$provided = @($CanonicalHost, $MatchHost, $TradeHost, $FriendHost | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
	if ($provided.Count -eq 0) {
		if ($Profile -ne 'dev') { throw '全球权威数据必须指定 -GlobalAuthorityTiDBHost(host:port)，禁止目录与资产分别落在区域MySQL。' }
		return @{ Host = $DevelopmentHost; RequireTiDB = 'false' }
	}
	$canonical = $provided[0]
	$settings = Get-FriendSqlSettings -Profile 'dev' -TiDBHost $canonical -DevelopmentHost ''
	foreach ($candidate in $provided) {
		$null = Get-FriendSqlSettings -Profile 'dev' -TiDBHost $candidate -DevelopmentHost ''
		if ($candidate -ine $canonical) { throw '目录、好友、交易和匹配必须配置同一个全球TiDB SQL入口；禁止跨独立数据库集群模拟原子事务。' }
	}
	return $settings
}

# 与GoDB的独立外部库清单共源；只反解正规storage族，不把当前zone/Regions自证为白名单。
function Get-RoleProofStorageIds {
	param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$DatabaseNames)
	$names = @($DatabaseNames -split ',' | ForEach-Object { $_.Trim() })
	if ($names.Count -lt 1 -or $names.Count -gt 1024) { throw '玩家数据库白名单必须含1..1024个正规落点库。' }
	$ids = @{}
	foreach ($name in $names) {
		$id = [uint32]0
		if ($name -cmatch '^zone_([1-9][0-9]{0,5})_db$') {
			if (-not [uint32]::TryParse($Matches[1],[ref]$id) -or $id -gt 999999) { throw '非法zone玩家库白名单。' }
		} elseif ($name -cmatch '^player_store_([1-9][0-9]*)_db$') {
			if (-not [uint32]::TryParse($Matches[1],[ref]$id) -or $id -lt 1000000) { throw '非法非zone玩家库白名单。' }
		} else { throw '玩家数据库白名单仅接受正规zone/player_store库，不接受其它schema或凭据。' }
		if ($ids.ContainsKey($id)) { throw '玩家数据库白名单含重复storage。' }
		$ids[$id]=$true
	}
	return @($ids.Keys | Sort-Object { [uint64]$_ } | ForEach-Object { [uint32]$_ })
}

function Get-FriendSqlSettings {
	param([string]$Profile, [string]$TiDBHost, [string]$DevelopmentHost)
	if ([string]::IsNullOrEmpty($TiDBHost)) {
		if ($Profile -ne 'dev') {
			throw '全服好友在 staging/prod 必须显式提供 -FriendTiDBHost(host:port)，全部副本使用同一权威 TiDB 事务域。'
		}
		return @{ Host = $DevelopmentHost; RequireTiDB = 'false' }
	}
	# 限定为主机与端口，避免把凭据、DSN 或换行带入 ConfigMap；后端身份仍由服务启动探测。
	if ($TiDBHost -notmatch '\A(?:[A-Za-z0-9][A-Za-z0-9.-]*|\[[0-9A-Fa-f:]+\]):([0-9]{1,5})\z' -or
		[int]$Matches[1] -lt 1 -or [int]$Matches[1] -gt 65535) {
		throw '-FriendTiDBHost 必须是有效的 host:port，不接受凭据、DSN 或其他配置内容。'
	}
	return @{ Host = $TiDBHost; RequireTiDB = 'true' }
}

<#
.SYNOPSIS
	从服务 yaml 读 PlayerInbox.EnqueueTypes(入箱白名单),逐项核对后按原顺序返回类型名;空白名单不返回任何元素。

.DESCRIPTION
	白名单是「哪些消息类型允许入箱」的唯一来源(docs/design/player-inbox-service.md §3.3),空 = 入箱全部停用(Go 侧回
	kPlayerInboxDisabled)。上线顺序(§11.7):第 3 步部署收件箱时为空,第 6 步全部 scene 打开 consumer_enabled 之后,
	才在服务 yaml 里填上并重跑 infra-up。各档位都镜像服务 yaml(与 trade 的 Market.* 同口径):白名单进 git、可审。
	服务 yaml 可以写块式列表或流式 `[]` / `[A, B]`;每一项必须是 proto/player_inbox/player_inbox.proto 里
	PlayerInboxMessageType 的枚举名(PLAYER_INBOX_MSG_*,不含 UNSPECIFIED)且不重复,否则 throw:拼错的类型名在 Go 侧
	要么拒启、要么被当成未知类型,都不该等到集群里才发现。
	三个调用方读同一份:Get-PlayerInboxEnqueueTypesYaml(渲染 ConfigMap)、Assert-PlayerInboxConsumerGate(写前门禁)与
	Read-PlayerInboxGateLiveFacts(决定读不读各 zone 线上的开关)。列表的形状由 ConvertFrom-PlayerInboxEnqueueTypesText 解析,
	线上 ConfigMap 走同一个解析函数;类型名只在这里核对。
	调用方用 @(...) 收集,空白名单得到长度 0 的数组。
#>
function Get-PlayerInboxEnqueueTypes {
	param([Parameter(Mandatory = $true)][string]$RelativePath)

	$keyPath = 'PlayerInbox.EnqueueTypes'
	$full = Join-Path $RepoRoot ($RelativePath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
	if (-not (Test-Path -LiteralPath $full)) {
		throw "player-inbox 入箱白名单校验失败:找不到 $RelativePath(fail-closed:不替你猜一个入箱白名单)"
	}
	$parsed = ConvertFrom-PlayerInboxEnqueueTypesText -Text (Get-Content -LiteralPath $full -Raw) -Source $RelativePath
	if (-not $parsed.Found) {
		throw "player-inbox 入箱白名单校验失败:$RelativePath 里查不到 $keyPath(入箱白名单必须显式写出,空白名单写 [])"
	}
	$items = [string[]]@($parsed.Items)

	$protoPath = Join-Path $RepoRoot ('proto/player_inbox/player_inbox.proto' -replace '/', [System.IO.Path]::DirectorySeparatorChar)
	if (-not (Test-Path -LiteralPath $protoPath)) {
		throw "player-inbox 入箱白名单校验失败:找不到 proto/player_inbox/player_inbox.proto,无法核对入箱白名单"
	}
	$known = @([regex]::Matches((Get-Content -LiteralPath $protoPath -Raw), '(?m)^\s*(PLAYER_INBOX_MSG_[A-Z0-9_]+)\s*=') |
		ForEach-Object { $_.Groups[1].Value } | Where-Object { $_ -cne 'PLAYER_INBOX_MSG_UNSPECIFIED' })
	$seen = New-Object System.Collections.Generic.List[string]
	foreach ($item in $items) {
		if ($known -cnotcontains $item) {
			throw "player-inbox 入箱白名单校验失败:$RelativePath 的 $keyPath 里 '$item' 不是 PlayerInboxMessageType 的已知类型(已知:$($known -join ', '))"
		}
		if ($seen -ccontains $item) {
			throw "player-inbox 入箱白名单校验失败:$RelativePath 的 $keyPath 里 '$item' 重复出现"
		}
		$seen.Add($item)
	}

	return $items
}

<#
.SYNOPSIS
	从一段 yaml 文本里取 PlayerInbox.EnqueueTypes 的各项,只解析形状、不核对类型名。
	返回 [pscustomobject]@{ Found [bool]; Items [string[]] };键不存在时 Found=$false、Items 为空。

.DESCRIPTION
	支持块式列表与流式 `[]` / `[A, B]`(项两侧的引号去掉),按原顺序返回;既不是列表也不是流式就 throw,-Source 写进错误。
	两个调用方:Get-PlayerInboxEnqueueTypes(git 跟踪的服务 yaml,键必须在、之后核对类型名)与 Read-PlayerInboxGateLiveFacts
	(线上 ConfigMap,缺键按 Go 侧缺省的空白名单算,只看空不空)。形状只在这里解析一份,两边不会对同一段文字得出不同的项数。
#>
function ConvertFrom-PlayerInboxEnqueueTypesText {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
		[Parameter(Mandatory = $true)][string]$Source
	)

	$keyPath = 'PlayerInbox.EnqueueTypes'
	$flat = ConvertFrom-YamlToFlatMap -Text $Text
	if (-not $flat.Scalars.Contains($keyPath)) {
		return [pscustomobject]@{ Found = $false; Items = [string[]]@() }
	}
	$items = New-Object System.Collections.Generic.List[string]
	if ($flat.ListCounts.Contains($keyPath)) {
		for ($i = 0; $i -lt [int]$flat.ListCounts[$keyPath]; $i++) {
			$items.Add(([string]$flat.Scalars[('{0}[{1}]' -f $keyPath, $i)]).Trim())
		}
	}
	else {
		$raw = ([string]$flat.Scalars[$keyPath]).Trim()
		$flow = [regex]::Match($raw, '^\[(?<body>[^\[\]]*)\]$')
		if (-not $flow.Success) {
			throw "player-inbox 入箱白名单校验失败:$Source 的 $keyPath='$raw' 既不是块式列表也不是流式 [..](空白名单请写 [])"
		}
		foreach ($part in ($flow.Groups['body'].Value -split ',')) {
			$name = $part.Trim().Trim('"', "'")
			if ($name.Length -gt 0) { $items.Add($name) }
		}
	}
	return [pscustomobject]@{ Found = $true; Items = [string[]]$items.ToArray() }
}

<#
.SYNOPSIS
	player-inbox ConfigMap 的 PlayerInbox.EnqueueTypes 行:取 Get-PlayerInboxEnqueueTypes 的结果,渲染成 PlayerInbox 段下的 YAML 行。
	返回值带两格缩进,直接拼进模板的 PlayerInbox 段;空白名单写 `[]`。
#>
function Get-PlayerInboxEnqueueTypesYaml {
	param([Parameter(Mandatory = $true)][string]$RelativePath)

	$items = @(Get-PlayerInboxEnqueueTypes -RelativePath $RelativePath)
	if ($items.Count -eq 0) { return '  EnqueueTypes: []' }
	return (@('  EnqueueTypes:') + @($items | ForEach-Object { "    - $_" })) -join "`n"
}

# ─────────────────────────────────────────────────────────────────
# go/db 存盘消费组按 zone 拆分(S2 ③;docs/design/db-consumer-group-per-zone.md §2 / §4)
# ─────────────────────────────────────────────────────────────────
# 记 L = go/db/etc/db.yaml 的 ServerConfig.Kafka.GroupID(legacy 共享组,也是按 zone 推导组名的基名),Z = L + "_z" + ZoneId,
# T = 本 zone 的 db_task topic(lib Get-DbTaskTopicName,世代号取 db.yaml 的 ServerConfig.Kafka.TopicGeneration)。
# go-svc-db-config 的 db.yaml 只有四种形状(生成器只写这四种,ConvertFrom-LiveDbConsumerGroup 只认这四种):
#   final(G)      一行 `    GroupID: "G"`,G ∈ {L, Z}。任何二进制都能加载;最终形状只用它,不写 ConsumerGroup。
#   handoff(X←Y)  `    ConsumerGroup: "X"` 加 `    GroupHandoffFrom: "Y"`,{X, Y} = {L, Z},不写 GroupID。
#                 交接闸之前的二进制把 GroupID 当必填,在 conf.MustLoad 就拒启 —— 能力证明来自二进制本身,不靠镜像标签或注解。
# 交接形状只在两条路径上渲染:显式 -DbConsumerGroup zone|legacy 的单 zone zone-up,以及 keep 读到一个尚未收尾的交接形状(原样保留)。
# 「非破坏性」的实际成立条件:db Deployment 是 maxUnavailable 0,新 Pod 不带交接闸时拒启、rollout 卡住;但 go-svc-db-config 整目录挂载、
# 进程只在启动时读,旧 ReplicaSet 的 Pod 只要重启一次就读到交接形状 —— 所以只有线上 db 本身带交接闸时旧 Pod 才「继续服务」。
# 显式交接在写路径入口证明这一点(Assert-LiveDbPodsHaveHandoffGate:每个 db Pod 的 /metrics 都有 db_consumer_group_handoff_gate_total);
# keep 不把最终形状改成交接形状,不引入新的暴露。回档 Step 6、全新 namespace、收尾之后的 keep 一律只渲染最终形状。每个 zone 的计划在写路径入口算一次(Initialize-DbConsumerGroupPlans,早于任何写操作),
# ConfigMap(New-GoSvcConfigMapYaml)与 Pod 模板注解(Apply-OneGoSvc)读同一份(Get-DbConsumerGroupPlan)。
# 部署脚本只读 Kafka、从不删位点:收尾必须由运维显式执行 kafka_offset_reset.ps1 -FinalizeHandoff;两组都有位点时运行时的 R7
# (go/db 交接闸)仍是兜住手工部署 / 旧脚本的纵深防御。
$script:DbConsumerGroupPlans = @{}

# 组名字符集校验:组名会被写进 YAML 双引号标量、Pod 注解,以及 kafka-0 里的 sh -c 命令行。不合格即 throw。
function Assert-DbConsumerGroupName {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()][string]$Name,
		[Parameter(Mandatory = $true)][string]$What
	)
	if ($Name -cnotmatch '^[A-Za-z0-9._-]+$') {
		throw "${What} 的消费组名 '$Name' 不合法(空值,或含 [A-Za-z0-9._-] 以外的字符),拒绝渲染。"
	}
}

# ConvertFrom-LiveDbConsumerGroup(把线上 go-svc-db-config 的 db.yaml 归到上面四种形状之一,不属于即 throw)2026-10-18 原样移到
# lib/kafka_consumer_groups.ps1(本文件开头已 dot-source),行为不变:回档 k8s_zone_rollback.ps1 第 0 步 f 核对部署事实要用同一份规则
# (回档复核 [0]),不另写一份。它依赖的 YAML 解析函数来自 release_common.ps1,本文件在 dot-source 那个库之前先 dot-source 了 release_common.ps1。

# 本 zone 的计划要不要读 Kafka 位点(§3.4):非 DryRun,且(显式模式、线上没有 ConfigMap、或线上是交接形状)。
# keep 遇到最终形状不读 Kafka:最终形状原样保留永远安全,也不让日常发布依赖 kafka-0 的 exec 权限。纯函数。
function Test-DbConsumerGroupNeedsKafkaFacts {
	param(
		[Parameter(Mandatory = $true)][ValidateSet('keep', 'zone', 'legacy')][string]$Mode,
		[Parameter(Mandatory = $true)][bool]$IsDryRun,
		[AllowNull()]$Live = $null
	)
	if ($IsDryRun) { return $false }
	return (($Mode -ne 'keep') -or ($null -eq $Live) -or ($Live.Shape -ceq 'handoff'))
}

<#
.SYNOPSIS
	一个 zone 的 db 消费组计划(§2 决策表)。返回 [pscustomobject]@{ Shape = 'final' | 'handoff'; Group; HandoffFrom; Source; Notice; Warning },
	或者 throw(调用方在任何写操作之前调用)。纯函数:不读集群、不读脚本级变量(契约测试按 AST 抽出来逐行喂值)。

.DESCRIPTION
	-Live:ConvertFrom-LiveDbConsumerGroup 的结果,$null = 线上没有 go-svc-db-config(全新 namespace)。
	-Facts:@{ ZoneHasOffsets; LegacyHasOffsets }(Read-DbGroupTopicFacts),各表示该组在 T 上至少有一行数值 CURRENT-OFFSET。
	        infra namespace 不存在的 all-up(不带 -SkipInfra,本次 Apply-Infra 新建一个空 Kafka)由 Initialize-DbConsumerGroupPlans
	        直接传两项都为 $false(不 exec),本函数照常按决策表判定。
	-FactsError:读 Kafka 失败的原因。只有「keep + 线上交接形状」接受它(保持线上形状,永远安全),其余一律 throw。
	-Topic / -Partitions / -BootstrapServer 只用来拼收尾命令(kafka_offset_reset.ps1 -FinalizeHandoff),不参与判定。
	DryRun(不读集群):keep → final(Z);zone → handoff(Z←L);legacy → handoff(L←Z);Source=dryrun-preview,都带 Warning。
	非 DryRun,keep:
	  线上 final(G)            → final(G),Source=live-configmap(不需要 Kafka 事实);
	  线上 handoff(X←Y)        → 读不到 Kafka:保持 handoff(X←Y) + Warning;Y 在 T 上没有位点:final(X),Source=handoff-finalized + Notice
	                             (去掉 GroupHandoffFrom,db 滚动一次);Y 仍有位点:保持 handoff(X←Y) + Warning(附收尾命令);
	  线上没有(全新 namespace) → Z、L 都有位点:throw(F4,附两种收尾命令);只有 Z:final(Z);只有 L:final(L)(保留运维的反向决定,
	                             不隐式切组);都没有:final(Z)(新 zone)。Source=fresh-namespace-kafka。全新 namespace 永远只渲染最终形状。
	非 DryRun,显式(zone:X=Z、Y=L;legacy:X=L、Y=Z):
	  X、Y 都有位点            → 线上就是 handoff(X←Y):幂等重跑,保持 + Warning;否则 throw(F2,附收尾命令,当前权威组取线上有效组);
	  只有 X                   → final(X),Source=explicit-already-authoritative(回档 Step 6 走这一条:Step 5 收尾后只有 Z 有位点);
	  只有 Y                   → handoff(X←Y),Source=explicit-handoff;
	  都没有                   → final(X),Source=explicit-nothing-to-inherit。
	Facts 缺字段或不是布尔值时 throw,不按「没有位点」猜(把读不到当成没有,F2 / F4 就形同虚设)。
#>
function Resolve-DbConsumerGroupPlan {
	param(
		[Parameter(Mandatory = $true)][ValidateSet('keep', 'zone', 'legacy')][string]$Mode,
		[Parameter(Mandatory = $true)][string]$LegacyGroup,
		[Parameter(Mandatory = $true)][string]$ZoneGroup,
		[Parameter(Mandatory = $true)][bool]$IsDryRun,
		[AllowNull()]$Live = $null,
		[AllowNull()]$Facts = $null,
		[AllowNull()][AllowEmptyString()][string]$FactsError = '',
		[string]$Topic = '<db_task topic>',
		[int]$Partitions = 0,
		[string]$BootstrapServer = '<broker>'
	)

	$modeName = $Mode.ToLowerInvariant()
	$plan = {
		param([string]$PlanShape, [string]$PlanGroup, [string]$PlanHandoffFrom, [string]$PlanSource, [string]$PlanNotice, [string]$PlanWarning)
		[pscustomobject]@{ Shape = $PlanShape; Group = $PlanGroup; HandoffFrom = $PlanHandoffFrom; Source = $PlanSource; Notice = $PlanNotice; Warning = $PlanWarning }
	}
	$partitionsText = if ($Partitions -gt 0) { [string]$Partitions } else { '<分区数>' }
	$finalizeCommand = {
		param([string]$Authority, [string]$Retire)
		"tools/scripts/kafka_offset_reset.ps1 -BootstrapServer $BootstrapServer -Topic $Topic -Group $Authority -RetireGroup $Retire -Partitions $partitionsText -FinalizeHandoff"
	}
	$finalizeHint = "先 dry-run、确认无问题后加 -Apply(命令里的 -BootstrapServer 是集群内地址,在集群外执行时换成执行机连得到的 broker 地址),收尾后重跑本命令。见 docs/design/db-consumer-group-per-zone.md §5。"

	if ($IsDryRun) {
		if ($modeName -eq 'keep') { $previewShape = 'final'; $previewGroup = $ZoneGroup; $previewFrom = '' }
		elseif ($modeName -eq 'zone') { $previewShape = 'handoff'; $previewGroup = $ZoneGroup; $previewFrom = $LegacyGroup }
		else { $previewShape = 'handoff'; $previewGroup = $LegacyGroup; $previewFrom = $ZoneGroup }
		$preview = if ($previewShape -eq 'final') { "GroupID=$previewGroup" } else { "ConsumerGroup=$previewGroup GroupHandoffFrom=$previewFrom" }
		$warning = "DbConsumerGroup=$modeName under -DryRun: cluster not read (dryrun-preview); previewing $preview. A real run decides from the live go-svc-db-config ConfigMap and the committed offsets of $ZoneGroup / $LegacyGroup on ${Topic}: it may render a final shape (GroupID only) instead, or refuse before any write."
		return (& $plan $previewShape $previewGroup $previewFrom 'dryrun-preview' '' $warning)
	}

	if ($null -ne $Live -and $Live.Shape -cnotin @('final', 'handoff')) {
		throw "内部错误:线上 db 消费组形状 '$($Live.Shape)' 不是 final / handoff(应先经 ConvertFrom-LiveDbConsumerGroup)。"
	}
	$liveIsHandoff = ($null -ne $Live -and $Live.Shape -ceq 'handoff')
	if (-not [string]::IsNullOrEmpty($FactsError) -and -not ($modeName -eq 'keep' -and $liveIsHandoff)) {
		# keep + 全新 namespace:infra namespace 不存在的 all-up(不带 -SkipInfra)走不到这里(Initialize-DbConsumerGroupPlans 直接按
		# 「两组都没有位点」判定);能走到这里的是 infra 已在、kafka-0 还没就绪(infra-up 没带 -WaitReady、上一次 Apply-Infra 半途失败)。
		$freshHint = if ($modeName -eq 'keep' -and $null -eq $Live) { '全新集群(或 infra 刚建、kafka-0 尚未就绪)可以先跑 infra-up -WaitReady,再跑 all-up -SkipInfra(或 zone-up)。' } else { '' }
		throw "读不到 Kafka 位点,无法判定 db 消费组归属(-DbConsumerGroup ${modeName},topic ${Topic},组 $ZoneGroup / ${LegacyGroup}),拒绝部署;尚未做任何写操作。检查 kafka-0 是否就绪、部署账号是否有 pods/exec 权限后重跑。${freshHint}原因:$FactsError"
	}
	$hasOffsets = {
		param([string]$FactGroup)
		$factName = if ($FactGroup -ceq $ZoneGroup) { 'ZoneHasOffsets' } elseif ($FactGroup -ceq $LegacyGroup) { 'LegacyHasOffsets' } else { throw "内部错误:组 $FactGroup 既不是 $ZoneGroup 也不是 ${LegacyGroup}。" }
		$factValue = $null
		if ($Facts -is [System.Collections.IDictionary]) {
			if ($Facts.Contains($factName)) { $factValue = $Facts[$factName] }
		}
		elseif ($null -ne $Facts -and $null -ne $Facts.PSObject.Properties[$factName]) {
			$factValue = $Facts.PSObject.Properties[$factName].Value
		}
		if ($factValue -isnot [bool]) {
			throw "内部错误:决策需要 Kafka 位点事实 ${factName}(布尔),实际没有拿到(-DbConsumerGroup ${modeName}),拒绝按「没有位点」猜测。"
		}
		return $factValue
	}

	if ($modeName -eq 'keep') {
		if ($null -ne $Live -and $Live.Shape -ceq 'final') {
			return (& $plan 'final' $Live.Group '' 'live-configmap' '' '')
		}
		if ($liveIsHandoff) {
			$x = $Live.Group
			$y = $Live.HandoffFrom
			if (-not [string]::IsNullOrEmpty($FactsError)) {
				return (& $plan 'handoff' $x $y 'live-configmap' '' "DbConsumerGroup=keep: Kafka offsets unreadable, keeping the live handoff shape ConsumerGroup=$x GroupHandoffFrom=$y unchanged (keeping the live shape is always safe). Once kafka-0 is reachable and the handoff is finalized, re-run zone-up with keep to drop GroupHandoffFrom. Cause: $FactsError")
			}
			if (-not (& $hasOffsets $y)) {
				return (& $plan 'final' $x '' 'handoff-finalized' "DbConsumerGroup=keep: handoff finalized ($y holds no committed offsets on $Topic); dropping GroupHandoffFrom and rendering GroupID=$x, db rolls once (annotation db-group-handoff-from: $y -> none)." '')
			}
			return (& $plan 'handoff' $x $y 'live-configmap' '' "DbConsumerGroup=keep: handoff $y -> $x not finalized yet ($y still holds committed offsets on $Topic); keeping ConsumerGroup=$x GroupHandoffFrom=$y. Finalize (dry-run first, then -Apply), then re-run zone-up with keep: $(& $finalizeCommand $x $y)")
		}

		# 全新 namespace(Remove-Zone 连 ConfigMap 一起删了):以 Kafka 为准,只渲染最终形状。
		$zoneHas = & $hasOffsets $ZoneGroup
		$legacyHas = & $hasOffsets $LegacyGroup
		if ($zoneHas -and $legacyHas) {
			throw ("全新 namespace(线上没有 go-svc-db-config),而 $ZoneGroup 与 $LegacyGroup 在 topic $Topic 上都有已提交位点:上一次交接没有收尾,判不出这个 zone 该用哪个组,拒绝部署(尚未做任何写操作)。" +
				"先 describe 两组(kafka-consumer-groups.sh --describe --group <组>),以停服前最后在用、CURRENT-OFFSET 更大的那一组为权威组收尾(选错时 -FinalizeHandoff 的前置检查会因新组位点小于旧组而拒绝):" +
				"若 $ZoneGroup 是权威组:$(& $finalizeCommand $ZoneGroup $LegacyGroup);若 $LegacyGroup 是权威组:$(& $finalizeCommand $LegacyGroup $ZoneGroup)。" + $finalizeHint)
		}
		if ($legacyHas) {
			return (& $plan 'final' $LegacyGroup '' 'fresh-namespace-kafka' "DbConsumerGroup=keep: fresh namespace and only $LegacyGroup holds committed offsets on $Topic; rendering GroupID=$LegacyGroup (the zone stays on the legacy group, no implicit group switch)." '')
		}
		return (& $plan 'final' $ZoneGroup '' 'fresh-namespace-kafka' '' '')
	}

	# 显式模式:X = 目标组,Y = 另一组。
	$x = if ($modeName -eq 'zone') { $ZoneGroup } else { $LegacyGroup }
	$y = if ($modeName -eq 'zone') { $LegacyGroup } else { $ZoneGroup }
	$xHas = & $hasOffsets $x
	$yHas = & $hasOffsets $y
	if ($xHas -and $yHas) {
		if ($liveIsHandoff -and $Live.Group -ceq $x -and $Live.HandoffFrom -ceq $y) {
			return (& $plan 'handoff' $x $y 'explicit-handoff' '' "DbConsumerGroup=${modeName}: re-run while handoff $y -> $x is not finalized ($y still holds committed offsets on $Topic); keeping ConsumerGroup=$x GroupHandoffFrom=$y. Finalize (dry-run first, then -Apply), then re-run zone-up with keep: $(& $finalizeCommand $x $y)")
		}
		if ($null -ne $Live) {
			$authority = $Live.Group
			$other = if ($authority -ceq $ZoneGroup) { $LegacyGroup } else { $ZoneGroup }
			$liveShapeText = if ($Live.Shape -ceq 'final') { '最终形状' } else { "交接形状 $authority ← $($Live.HandoffFrom)" }
			$howTo = "当前权威组是线上 go-svc-db-config 的 ${authority}(${liveShapeText}),先对它收尾:$(& $finalizeCommand $authority $other)。"
		}
		else {
			$howTo = "线上没有 go-svc-db-config,先 describe 两组判定权威组(停服前最后在用、CURRENT-OFFSET 更大的那一组;选错时 -FinalizeHandoff 的前置检查会因新组位点小于旧组而拒绝):若 $ZoneGroup 是权威组:$(& $finalizeCommand $ZoneGroup $LegacyGroup);若 $LegacyGroup 是权威组:$(& $finalizeCommand $LegacyGroup $ZoneGroup)。"
		}
		throw ("-DbConsumerGroup $modeName 被拒绝(尚未做任何写操作):$x 与 $y 在 topic $Topic 上都有已提交位点,上一次交接没有收尾。此时再切组,go/db 交接闸的 R7 会停住本 zone 的存盘消费,合服门禁也会拒绝。" +
			$howTo + $finalizeHint)
	}
	if ($xHas) {
		return (& $plan 'final' $x '' 'explicit-already-authoritative' '' '')
	}
	if ($yHas) {
		return (& $plan 'handoff' $x $y 'explicit-handoff' "DbConsumerGroup=${modeName}: rendering handoff ConsumerGroup=$x GroupHandoffFrom=$y ($y holds committed offsets on $Topic, $x holds none). Once db has rolled and seeded, finalize right away (dry-run first, then -Apply), then re-run zone-up with keep: $(& $finalizeCommand $x $y)" '')
	}
	return (& $plan 'final' $x '' 'explicit-nothing-to-inherit' '' '')
}

<#
.SYNOPSIS
	把一个 zone 的计划渲染成 db.yaml 里 ServerConfig.Kafka 下的组名键(4 格缩进,行间 LF),替换模板里原来那一行 GroupID。

.DESCRIPTION
	先一行注释,再按形状:final 只一行 GroupID;handoff 两行 ConsumerGroup + GroupHandoffFrom(不写 GroupID)。
	计划不是四种形状之一就 throw(内部错误),不渲染半截配置。纯函数。
#>
function New-DbConsumerGroupYaml {
	param([Parameter(Mandatory = $true)]$Plan)

	$lines = @('    # 消费组由 k8s_deploy.ps1 按 -DbConsumerGroup 与 Kafka 位点决定(docs/design/db-consumer-group-per-zone.md)')
	Assert-DbConsumerGroupName -Name $Plan.Group -What 'db 消费组计划'
	if ($Plan.Shape -ceq 'final') {
		if (-not [string]::IsNullOrEmpty($Plan.HandoffFrom)) {
			throw "内部错误:db 消费组计划是最终形状却带了 HandoffFrom=$($Plan.HandoffFrom),拒绝渲染(最终形状只写 GroupID)。"
		}
		$lines += ('    GroupID: "{0}"' -f $Plan.Group)
	}
	elseif ($Plan.Shape -ceq 'handoff') {
		Assert-DbConsumerGroupName -Name $Plan.HandoffFrom -What 'db 消费组计划的交接对方组'
		if ($Plan.HandoffFrom -ceq $Plan.Group) {
			throw "内部错误:db 消费组计划的交接对方组等于本组 $($Plan.Group),拒绝渲染(go/db 拒启这种形状)。"
		}
		$lines += ('    ConsumerGroup: "{0}"' -f $Plan.Group)
		$lines += ('    GroupHandoffFrom: "{0}"' -f $Plan.HandoffFrom)
	}
	else {
		throw "内部错误:db 消费组计划的形状 '$($Plan.Shape)' 不是 final / handoff,拒绝渲染。"
	}
	return ($lines -join "`n")
}

# 取一个 zone 的计划(ConfigMap 生成器与 Apply-OneGoSvc 的 db 注解共用)。缺失 = 写路径入口没算过,一律 throw,不回落任何默认组。
function Get-DbConsumerGroupPlan {
	param([Parameter(Mandatory = $true)][int]$CurrentZoneId)

	if ($null -eq $script:DbConsumerGroupPlans -or -not $script:DbConsumerGroupPlans.ContainsKey($CurrentZoneId)) {
		throw "内部错误:zone $CurrentZoneId 没有 db 消费组计划(Initialize-DbConsumerGroupPlans 未在写路径入口调用,或本 zone 不在本次部署的 zone 列表里),拒绝生成 db 的 ConfigMap / Pod 模板。"
	}
	return $script:DbConsumerGroupPlans[$CurrentZoneId]
}

<#
.SYNOPSIS
	读线上 zone namespace 里 go-svc-db-config 的 db.yaml 文本;ConfigMap 不存在(含 namespace 不存在)返回 $null。只在非 DryRun 时调用。

.DESCRIPTION
	kubectl 失败(退出码非 0)一律 throw:读不到不等于没有 —— 把「没读到」当成全新 namespace,keep 就会按 Kafka 重新判组,
	线上的交接形状会被静默改写。有输出却解析不了、或没有 data.db.yaml 键,同样 throw。
	Invoke-ClientEntryKubectl 不看 $DryRun,DryRun 判断由调用方(Initialize-DbConsumerGroupPlans)负责。
#>
function Read-LiveDbConfigMapYaml {
	param([Parameter(Mandatory = $true)][string]$Namespace)

	$info = $GoSvcCatalogue['db']
	$result = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'configmap', $info.ConfigMap, '-n', $Namespace, '--ignore-not-found', '-o', 'json') `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
	if ($result.ExitCode -ne 0) {
		throw "读取线上 ConfigMap $($info.ConfigMap) 失败(namespace=${Namespace},kubectl 退出码 $($result.ExitCode)):$($result.Stderr)。fail-closed:读不到不等于没有,无法判定 db 消费组归属,拒绝部署;尚未做任何写操作。"
	}
	$text = ([string]$result.Stdout).Trim()
	if ($text -eq '') { return $null }
	try {
		$configMap = $text | ConvertFrom-Json -ErrorAction Stop
	}
	catch {
		throw "线上 ConfigMap $($info.ConfigMap)(namespace=${Namespace})的 kubectl 输出不是合法 JSON,无法判定 db 消费组归属,拒绝部署;尚未做任何写操作。$($_.Exception.Message)"
	}
	$property = $null
	$dataProperty = $configMap.PSObject.Properties['data']
	if ($null -ne $dataProperty -and $null -ne $dataProperty.Value) {
		$property = $dataProperty.Value.PSObject.Properties[$info.ConfigFile]
	}
	if ($null -eq $property -or [string]::IsNullOrWhiteSpace([string]$property.Value)) {
		throw "线上 ConfigMap $($info.ConfigMap)(namespace=${Namespace})存在,但没有 data.$($info.ConfigFile):不是本脚本生成的形状,无法判定 db 消费组归属,拒绝部署;尚未做任何写操作。"
	}
	return [string]$property.Value
}

<#
.SYNOPSIS
	读 zone 组与 legacy 组在 -Topic 上有没有已提交位点:kubectl exec 进 kafka-0 跑 kafka-consumer-groups.sh --describe。
	返回 [pscustomobject]@{ ZoneHasOffsets [bool]; LegacyHasOffsets [bool] };判不出就 throw。只在非 DryRun 时调用。

.DESCRIPTION
	分类规则只有一份(lib Resolve-KafkaGroupTopicOffsets):组不存在 / 组在但本 topic 没有数值 CURRENT-OFFSET → $false;有 → $true;
	exec 失败、"Error:" 行、退出码非 0、输出解析不了都 throw(fail-closed:把「读不到」当成「没有位点」,显式切组会跳过 F2 拒绝,
	全新 namespace 会被判成新 zone)。stdout 与 stderr 拆行后合并交给分类函数:「组不存在」那行在不同版本里可能打到任一路。
	KAFKA_HEAP_OPTS 在命令里覆盖成小堆,与 kafka.yaml 探针命令同一写法:容器 env 里 broker 自己的大堆参数会被 /opt/kafka/bin/*.sh 继承,
	在 broker 的 memory limit 里再起一个大堆 JVM 会把 kafka-0 撑到 OOMKilled。所以经 /bin/sh -c 执行,组名先过字符集校验再拼进命令行。
	命令与分类只在 lib 一份(Get-DbGroupDescribeExecArgs / Resolve-DbGroupDescribeExecResult):k8s_zone_rollback.ps1 第 0 步 e
	在停服之前用同一条路径预读,回档 Step 6 走到这里时不会才第一次发现 exec 不通。
	Invoke-ClientEntryKubectl 不看 $DryRun,DryRun 判断由调用方(Initialize-DbConsumerGroupPlans)负责。
#>
function Read-DbGroupTopicFacts {
	param(
		[Parameter(Mandatory = $true)][string]$Topic,
		[Parameter(Mandatory = $true)][string]$ZoneGroup,
		[Parameter(Mandatory = $true)][string]$LegacyGroup
	)

	$facts = [ordered]@{}
	foreach ($entry in @(
		@{ Name = 'ZoneHasOffsets'; Group = $ZoneGroup }
		@{ Name = 'LegacyHasOffsets'; Group = $LegacyGroup }
	)) {
		Assert-DbConsumerGroupName -Name $entry.Group -What 'Kafka 位点查询'
		$result = Invoke-ClientEntryKubectl -KubectlArgs (Get-DbGroupDescribeExecArgs -InfraNamespace $InfraNamespace -Group $entry.Group) `
			-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
		$state = (Resolve-DbGroupDescribeExecResult -Result $result -Topic $Topic).State
		$facts[$entry.Name] = ($state -ceq 'offsets')
	}
	return [pscustomobject]$facts
}

<#
.SYNOPSIS
	显式交接(决策表落到 Source=explicit-handoff,即 -DbConsumerGroup zone|legacy 渲染交接形状)之前,证明本 zone 线上每个 db Pod
	跑的都是带交接闸的二进制;证明不了就 throw。只在非 DryRun 时调用,调用方在任何写操作之前调用。

.DESCRIPTION
	为什么要证明(R7 复核 [0]):go-svc-db-config 整目录挂到 /app/etc(deploy/k8s/manifests/go-svc/db.yaml,没有 subPath),
	apply 之后 kubelet 会把**已在运行**的 Pod 的挂载卷也同步成新内容,而进程只在启动时读一次。交接形状不写 GroupID,交接闸之前的
	二进制在 conf.MustLoad 拒启:新 Pod 拒启时 rollout 卡住(maxUnavailable 0)只是第一步 —— 旧 ReplicaSet 的 Pod 此后只要重启一次
	(OOM、存活探针失败、节点故障后被重建),读到的同样是交接形状,同样拒启并进入 CrashLoop。两个副本都重启过之后本 zone 存盘消费全停,
	db 全部 CrashLoop 时没有任何 db 指标序列,告警面是盲的。所以「旧 Pod 继续服务」只在线上 Pod 本身就能加载交接形状时成立
	(docs/design/db-consumer-group-per-zone.md §8.1 的唯一硬前提),这里把这条前提从约定变成部署前检查。
	证据取自二进制本身,不靠镜像标签或注解:带交接闸的 go/db 在 /metrics 上以 0 预建 db_consumer_group_handoff_gate_total 的全部序列
	(go/db/internal/metrics/metrics.go register,metrics_test.go 钉住),之前的二进制没有这条序列。读法(只读,不 exec):
	  1. kubectl get deployments,pods -l app=db -n <zone ns> -o json:取 db Deployment 的副本数与本 zone 的 db Pod;
	  2. 对每个没在删除(无 deletionTimestamp)、没终结(phase 不是 Succeeded / Failed)的 Pod,经 API server 代理抓 /metrics:
	     kubectl get --raw /api/v1/namespaces/<ns>/pods/<pod>:9160/proxy/metrics(9160 = db ConfigMap 的 MetricsListenAddr 与 manifest 的 metrics 端口;
	     部署账号要有 pods/proxy 的 get 权限)。
	任一次 kubectl 失败、输出不是 JSON、任一 Pod 抓不到或缺这条序列,都 throw(Pending / 未就绪的 Pod 抓不到,同样拒绝:它将来也会读到交接形状)。
	一个可抓的 Pod 都没有时:db Deployment 不存在或副本数为 0 = 没有会被旧 ReplicaSet 拉起的进程,放行并告警(新 Pod 不带交接闸时
	rollout 卡住,恢复见下);副本数 > 0 却没有可抓的 Pod(例如正被重建)判不出,throw。
	keep 不调用:keep 只会原样保留或去掉交接形状,本次不会把 ConfigMap 从最终形状改成交接形状。
	Invoke-ClientEntryKubectl 不看 $DryRun,DryRun 判断由调用方(Initialize-DbConsumerGroupPlans)负责。
#>
function Assert-LiveDbPodsHaveHandoffGate {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][ValidateSet('zone', 'legacy')][string]$Mode,
		[Parameter(Mandatory = $true)]$Plan
	)

	$gateSeries = 'db_consumer_group_handoff_gate_total'
	$metricsPort = 9160
	$modeName = $Mode.ToLowerInvariant()
	$recoverMode = if ($modeName -eq 'zone') { 'legacy' } else { 'zone' }
	$refusal = "-DbConsumerGroup $modeName 被拒绝(尚未做任何写操作):本次要把 namespace $Namespace 的 go-svc-db-config 渲染成交接形状 ConsumerGroup=$($Plan.Group) GroupHandoffFrom=$($Plan.HandoffFrom),"
	$why = ("交接形状只有带交接闸的 go/db 能加载;go-svc-db-config 整目录挂载,旧 ReplicaSet 的 Pod 一旦重启(OOM、探针失败、节点故障)就会读到它并在 conf.MustLoad 拒启,本 zone 存盘消费随之停止。" +
		"先以 keep(不切组)把 db 发布到带交接闸的镜像(docs/design/db-consumer-group-per-zone.md §8.1),等每个 db Pod 的 /metrics 都出现 $gateSeries 后再重跑本命令。" +
		"若线上 go-svc-db-config 已经是交接形状、旧 Pod 已在 MustLoad 拒启,用 -DbConsumerGroup $recoverMode 收回到最终形状 GroupID=$($Plan.HandoffFrom)(§8.3)。")

	$list = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'deployments,pods', '-l', 'app=db', '-n', $Namespace, '-o', 'json', '--request-timeout=15s') `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
	if ($list.ExitCode -ne 0) {
		throw ($refusal + "但读不到本 zone 的 db Deployment / Pod(kubectl 退出码 $($list.ExitCode):$($list.Stderr)),证明不了线上 db 带交接闸。" + $why)
	}
	$items = @()
	$text = ([string]$list.Stdout).Trim()
	if ($text -ne '') {
		try {
			$document = $text | ConvertFrom-Json -ErrorAction Stop
		}
		catch {
			throw ($refusal + "但 kubectl get deployments,pods 的输出不是合法 JSON($($_.Exception.Message)),证明不了线上 db 带交接闸。" + $why)
		}
		$items = if ($document.kind -eq 'List') { @($document.items) } else { @($document) }
	}
	$deployment = @($items | Where-Object { $null -ne $_ -and $_.kind -eq 'Deployment' -and $_.metadata.name -eq 'db' }) | Select-Object -First 1
	$pods = @($items | Where-Object {
			$null -ne $_ -and $_.kind -eq 'Pod' -and $null -eq $_.metadata.deletionTimestamp -and [string]$_.status.phase -notin @('Succeeded', 'Failed')
		})

	if ($pods.Count -eq 0) {
		$replicas = if ($null -ne $deployment) { [int]$deployment.spec.replicas } else { 0 }
		if ($replicas -gt 0) {
			throw ($refusal + "但 db Deployment 的副本数是 $replicas、却没有一个可检查的 db Pod(可能正在重建),判不出将被拉起的进程带不带交接闸。等 db Pod 就绪后重跑。" + $why)
		}
		Write-Warning "DbConsumerGroup=${modeName}: namespace $Namespace has no running db process (db Deployment absent or scaled to 0), so no old pod can restart into the handoff shape. The new db pods must run a handoff-gate binary; otherwise they refuse to start in conf.MustLoad and the rollout stalls. Recover with -DbConsumerGroup $recoverMode (final shape GroupID=$($Plan.HandoffFrom))."
		return
	}

	$problems = @()
	foreach ($pod in $pods) {
		$podName = [string]$pod.metadata.name
		$scrape = Invoke-ClientEntryKubectl -KubectlArgs @('get', '--raw', "/api/v1/namespaces/$Namespace/pods/${podName}:$metricsPort/proxy/metrics", '--request-timeout=15s') `
			-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
		if ($scrape.ExitCode -ne 0) {
			$problems += "Pod ${podName}:/metrics 抓不到(kubectl 退出码 $($scrape.ExitCode):$($scrape.Stderr);未就绪的 Pod 同样抓不到,部署账号还需要 pods/proxy 的 get 权限)"
			continue
		}
		if ([string]$scrape.Stdout -cnotmatch "(?m)^$([regex]::Escape($gateSeries))\{") {
			$problems += "Pod ${podName}:/metrics 里没有 $gateSeries(交接闸之前的二进制)"
			continue
		}
		Write-Host "  db pod ${podName}: $gateSeries present (handoff gate binary)"
	}
	if ($problems.Count -gt 0) {
		throw ($refusal + "但证明不了线上每个 db Pod 都带交接闸($($problems.Count) 项):`n  - " + ($problems -join "`n  - ") + "`n" + $why)
	}
}

<#
.SYNOPSIS
	写路径入口:为本次要部署 db 的每个 zone 算出消费组计划,存进 $script:DbConsumerGroupPlans[zoneId];判不出就在任何写操作之前 throw。

.DESCRIPTION
	紧跟 Assert-ClientEntryDeployPreflight 调用(写路径入口块,早于密钥解析与任何 kubectl 写操作),读失败不会留下半截部署。
	只在 Test-ZoneDbDeployed 为真时计算:不部署 db 时没有计划,也不读集群;此时显式 -DbConsumerGroup zone|legacy 直接 throw。
	zone-up 处理 -ZoneName / -ZoneId,all-up 处理 zones 配置里的每个 zone(all-up 只能是 keep,显式模式已在入口拒绝)。
	all-up 不带 -SkipInfra 且非 DryRun 时先只读探测 infra namespace:不存在 = 本次 Apply-Infra 新建一个空 Kafka,各 zone 不 exec、
	直接按「两组都没有位点」判定(理由见函数体)。逐 zone:
	  1. 非 DryRun 时读线上 go-svc-db-config(Read-LiveDbConfigMapYaml)并归到四种形状(ConvertFrom-LiveDbConsumerGroup);
	  2. 需要时(Test-DbConsumerGroupNeedsKafkaFacts)kubectl exec 进 kafka-0 读两组在本 zone topic 上的位点(Read-DbGroupTopicFacts);
	     读失败的原因作为 -FactsError 交给决策表:只有「keep + 线上交接形状」接受它(保持线上形状),其余由决策表 throw;
	  3. Resolve-DbConsumerGroupPlan 出计划;非 DryRun 且计划来自显式交接(Source=explicit-handoff,要渲染交接形状)时,先证明线上每个 db Pod
	     都带交接闸(Assert-LiveDbPodsHaveHandoffGate),证明不了就 throw、本 zone 不留计划;
	  4. 每个 zone 打一行 DB consumer group: …;Notice 用 Write-Host,Warning 用 Write-Warning。
	DryRun 不读集群(Invoke-ClientEntryKubectl 不看 $DryRun,判断在这里做),按决策表渲染预览并告警。
#>
function Initialize-DbConsumerGroupPlans {
	$script:DbConsumerGroupPlans = @{}
	if (-not (Test-ZoneDbDeployed)) {
		# 显式切组配在不部署 db 的运行上(没给 -GoSvcRegistry,或带了 -SkipGoSvc):C++ 节点照常重部署、退出码 0,运维会以为切过了。
		# 与入口拒绝同一理由;Test-ZoneDbDeployed 在入口检查处还没定义,所以在这里拒绝,同样早于任何写操作。
		if ($DbConsumerGroup -ne 'keep') {
			throw "-DbConsumerGroup $DbConsumerGroup 需要本次部署 db(给 -GoSvcRegistry 且不带 -SkipGoSvc),否则切组不会生效,拒绝部署;尚未做任何写操作。"
		}
		return
	}

	$dbYamlPath = 'go/db/etc/db.yaml'
	$legacyGroup = Get-AuthoritativeScalar -RelativePath $dbYamlPath -KeyPath 'ServerConfig.Kafka.GroupID'
	Assert-DbConsumerGroupName -Name $legacyGroup -What "$dbYamlPath 的 ServerConfig.Kafka.GroupID(legacy 组,也是按 zone 推导组名的基名)"
	$generationText = Get-AuthoritativeScalar -RelativePath $dbYamlPath -KeyPath 'ServerConfig.Kafka.TopicGeneration'
	$partitionText = Get-AuthoritativeScalar -RelativePath $dbYamlPath -KeyPath 'ServerConfig.Kafka.PartitionCnt'
	$generation = 0
	$partitions = 0
	if (-not [int]::TryParse($generationText, [ref]$generation) -or $generation -lt 0) {
		throw "$dbYamlPath 的 ServerConfig.Kafka.TopicGeneration='$generationText' 不是非负整数,拼不出 db_task topic 名,拒绝部署(fail-closed)。"
	}
	if (-not [int]::TryParse($partitionText, [ref]$partitions) -or $partitions -le 0) {
		throw "$dbYamlPath 的 ServerConfig.Kafka.PartitionCnt='$partitionText' 不是正整数,拒绝部署(fail-closed)。"
	}
	$bootstrap = "kafka.${InfraNamespace}:9092"

	$zones = @()
	if ($Command -eq 'zone-up') {
		$zones += [pscustomobject]@{ Name = $ZoneName; ZoneId = [int]$ZoneId }
	}
	elseif ($Command -eq 'all-up') {
		foreach ($zone in (Get-ZonesFromJson -Path (Resolve-ZonesConfigPath))) {
			$zones += [pscustomobject]@{ Name = [string]$zone.name; ZoneId = [int]$zone.zoneId }
		}
	}

	# 全新 infra:all-up 不带 -SkipInfra 时,Kafka 要到 switch ($Command) 里的 Apply-Infra 才建,而计划必须在那之前算好 ——
	# 不处理的话,新集群上的一键开服(以及 infra-down / all-down 之后的 all-up)会因 exec 不到 kafka-0 被决策表拒绝,照提示重跑也不会好。
	# infra namespace 不存在 = 里面不可能有 Kafka 的 PVC(Remove-Infra 删的是整个 namespace,PVC 随之删除),本次新建的 Kafka 必然为空,
	# 两组在任何 topic 上都没有位点:不 exec,直接按两项 $false 交给决策表(线上没有 → final(Z);线上 handoff(X←Y) → final(X),
	# 即收尾;线上 final 原样保留,本来就不读 Kafka)。判据只用「namespace 不存在」,不用「kafka-0 / StatefulSet 不存在」:
	# StatefulSet 被删而 PVC 还在时位点仍然存在。namespace 在但 kafka-0 没就绪(上次 Apply-Infra 半途失败、Terminating 中)照旧 exec,
	# 读不到由决策表拒绝(fail-closed)。zone-up 与 -SkipInfra 不建 Kafka,不走这条。探测本身失败同样 throw:读不到不等于没有。
	$kafkaFresh = $false
	if ($Command -eq 'all-up' -and -not $SkipInfra -and -not $DryRun) {
		$infraProbe = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'namespace', $InfraNamespace, '--ignore-not-found', '-o', 'name') `
			-KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
		if ($infraProbe.ExitCode -ne 0) {
			throw "读取 infra namespace $InfraNamespace 失败(kubectl 退出码 $($infraProbe.ExitCode)):$($infraProbe.Stderr)。fail-closed:判不出本次 all-up 是否新建 Kafka,无法判定 db 消费组归属,拒绝部署;尚未做任何写操作。"
		}
		if ([string]::IsNullOrWhiteSpace([string]$infraProbe.Stdout)) {
			$kafkaFresh = $true
			Write-Host "DB consumer group: infra namespace $InfraNamespace does not exist; this all-up creates Kafka from scratch (Apply-Infra), so neither group can hold committed offsets. Deciding every zone from empty Kafka facts, no kubectl exec."
		}
	}

	foreach ($zone in $zones) {
		$zoneGroup = "${legacyGroup}_z$($zone.ZoneId)"
		$topic = Get-DbTaskTopicName -ZoneId $zone.ZoneId -Generation $generation
		$live = $null
		$facts = $null
		$factsError = ''
		if (-not $DryRun) {
			$liveYaml = Read-LiveDbConfigMapYaml -Namespace (Get-ZoneNamespace -Name $zone.Name)
			if ($null -ne $liveYaml) {
				$live = ConvertFrom-LiveDbConsumerGroup -DbYaml $liveYaml -LegacyGroup $legacyGroup -ZoneGroup $zoneGroup
			}
			if (Test-DbConsumerGroupNeedsKafkaFacts -Mode $DbConsumerGroup -IsDryRun $false -Live $live) {
				if ($kafkaFresh) {
					$facts = [pscustomobject]@{ ZoneHasOffsets = $false; LegacyHasOffsets = $false }
				}
				else {
					try {
						$facts = Read-DbGroupTopicFacts -Topic $topic -ZoneGroup $zoneGroup -LegacyGroup $legacyGroup
					}
					catch {
						$factsError = $_.Exception.Message
					}
				}
			}
		}
		$plan = Resolve-DbConsumerGroupPlan -Mode $DbConsumerGroup -LegacyGroup $legacyGroup -ZoneGroup $zoneGroup -IsDryRun ([bool]$DryRun) `
			-Live $live -Facts $facts -FactsError $factsError -Topic $topic -Partitions $partitions -BootstrapServer $bootstrap
		# 交接形状只在线上 db 本身就能加载它时才是非破坏性的(旧 ReplicaSet 的 Pod 重启后读到的是新 ConfigMap),先证明再存计划。
		if (-not $DryRun) {
			if ($plan.Source -ceq 'explicit-handoff') {
				Assert-LiveDbPodsHaveHandoffGate -Namespace (Get-ZoneNamespace -Name $zone.Name) -Mode $DbConsumerGroup -Plan $plan
			}
		}
		$script:DbConsumerGroupPlans[[int]$zone.ZoneId] = $plan
		$handoffText = if ([string]::IsNullOrEmpty($plan.HandoffFrom)) { 'none' } else { $plan.HandoffFrom }
		Write-Host "DB consumer group: zone=$($zone.Name)($($zone.ZoneId)) mode=$DbConsumerGroup shape=$($plan.Shape) group=$($plan.Group) handoff_from=$handoffText source=$($plan.Source) topic=$topic"
		if (-not [string]::IsNullOrEmpty($plan.Notice)) { Write-Host $plan.Notice }
		if (-not [string]::IsNullOrEmpty($plan.Warning)) { Write-Warning $plan.Warning }
	}
}

function New-GuildServiceConfigYaml {
	param([int]$CurrentZoneId, [int]$CurrentClusterId)
	$source = 'go/guild/etc/guild.yaml'
	$mode = if ($ReleaseProfile -eq 'dev') { Get-AuthoritativeScalar -RelativePath $source -KeyPath 'Mode' } else { 'pro' }
	$autoMigrate = if ($ReleaseProfile -eq 'dev') { Get-AuthoritativeScalar -RelativePath $source -KeyPath 'Schema.AutoMigrate' } else { 'false' }
	$requireTiDB = if ($ReleaseProfile -eq 'dev') { Get-AuthoritativeScalar -RelativePath $source -KeyPath 'MySQL.RequireTiDB' } else { 'true' }
	$trialEnabled = Get-AuthoritativeScalar -RelativePath $source -KeyPath 'Activity.TrialResult.Enabled'
	$matchAuthoritySchema = Get-AuthoritativeScalar -RelativePath $source -KeyPath 'MatchAuthoritySchema'
	if ($requireTiDB -eq 'true' -and $trialEnabled -eq 'true') { Assert-GlobalMatchKafkaTopology -Topology (Get-KafkaTopology) }
	$timeout = Get-AuthoritativeScalar -RelativePath $source -KeyPath 'Timeout'
	$quota = Get-AuthoritativeScalar -RelativePath $source -KeyPath 'ApplicationQuotaPerMinute'
	$maxOpen = Get-AuthoritativeScalar -RelativePath $source -KeyPath 'MySQL.MaxOpenConn'
	$lease = Get-AuthoritativeScalar -RelativePath $source -KeyPath 'Node.LeaseTTL'
	$dial = Get-AuthoritativeScalar -RelativePath $source -KeyPath 'Registry.Etcd.DialTimeout'
	$null = Get-RedisPoolMirror -Target 'guild'
	$loginSessionDb = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.RedisClient.DB'
	$blocks = [Collections.Generic.List[string]]::new()
	$blocks.Add(@"
Name: guild.rpc
Mode: $mode
AccountRoleSchema: ${GlobalDbName}
MatchAuthoritySchema: $matchAuthoritySchema
GlobalLoginSessions: true
ListenOn: 0.0.0.0:50300
Timeout: $timeout
ApplicationQuotaPerMinute: $quota
TableDir: /generated/tables
ClusterId: $CurrentClusterId
SnowflakeCacheDir: $SnowflakeCacheDir
MySQL:
  DataSource: ""
  DataSourceEnv: MMORPG_GUILD_SQL_DSN
  RequireTiDB: $requireTiDB
  MaxOpenConn: $maxOpen
Schema:
  AutoMigrate: $autoMigrate
Node:
  ZoneId: $CurrentZoneId
  LeaseTTL: $lease
Registry:
  Etcd:
    Hosts: ["etcd.${InfraNamespace}:2379"]
    DialTimeout: $dial
Kafka:
  Brokers: ["kafka.${InfraNamespace}:9092"]
Prometheus:
  Host: 0.0.0.0
  Port: 9220
  Path: /metrics
"@)
	foreach ($key in @('RedisClient', 'PlayerLocatorRedis', 'MergeMarkerRedis', 'LoginRedis')) {
		$block = Get-AuthoritativeYamlBlock -RelativePath $source -Key $key
		$block = $block -replace '(?m)^  Host:.*$', "  Host: redis.${InfraNamespace}:6379"
		$passwordKey = if ($key -eq 'MergeMarkerRedis') { 'Pass' } else { 'Password' }
		$block = $block -replace "(?m)^  ${passwordKey}:.*$", "  ${passwordKey}: `"$($script:RedisPassword)`""
		if ($key -eq 'LoginRedis') {
			$block = $block -replace '(?m)^  DB:.*$', "  DB: $loginSessionDb"
		}
		if ($key -eq 'MergeMarkerRedis') {
			# D1 第 3 批 guild 读方门禁(spec_D1_batch1b2_2_3.md §5.4):取本脚本参数 -GuildRequireRouteReady(缺省 false),不镜像服务 yaml;
			# 服务 yaml 里有这一行就改写,没有就补一行。打开前提与回退顺序见参数 -RouteStoreMode 处的注释。
			$guildRouteReadyText = if ([bool]$GuildRequireRouteReady) { 'true' } else { 'false' }
			if ($block -match '(?m)^  RequireRouteReady:.*$') {
				$block = $block -replace '(?m)^  RequireRouteReady:.*$', "  RequireRouteReady: $guildRouteReadyText"
			}
			else {
				$block = $block + "`n  RequireRouteReady: $guildRouteReadyText"
			}
		}
		$blocks.Add($block)
	}
	foreach ($key in @('DataServiceRpc', 'MatchRpc')) {
		$block = Get-AuthoritativeYamlBlock -RelativePath $source -Key $key
		$block = $block -replace '(?m)^      -.*$', "      - `"etcd.${InfraNamespace}:2379`""
		$blocks.Add($block)
	}
	foreach ($key in @('Cache', 'IdSegment', 'AssetOp', 'Activity', 'KillSwitchPrefix')) {
		$block = Get-AuthoritativeYamlBlock -RelativePath $source -Key $key
		if ($key -eq 'AssetOp' -and $ReleaseProfile -ne 'dev') {
			# 08-save-owner-fence.md 的生产启用门禁仍未全验；配置接线不能替代父快照屏障证据。
			$block = $block -replace '(?m)^  Enabled:.*$', '  Enabled: false'
		}
		if ($key -eq 'Activity') {
			$block = $block -replace '(?m)^      -.*$', "      - `"kafka.${InfraNamespace}:9092`""
		}
		$blocks.Add($block)
	}
	return ($blocks -join "`n")
}

function New-GoSvcConfigMapYaml {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		[Parameter(Mandatory = $true)][int]$CurrentZoneId,
		[Parameter(Mandatory = $true)][int]$CurrentClusterId
	)
	$authoritySql = Get-GlobalAuthoritySqlSettings -Profile $ReleaseProfile -CanonicalHost $GlobalAuthorityTiDBHost `
		-FriendHost $FriendTiDBHost -TradeHost $TradeTiDBHost -MatchHost $MatchTiDBHost -DevelopmentHost "mysql.${InfraNamespace}:3306"
	if ($authoritySql.RequireTiDB -cne 'true') { throw '全球服务ConfigMap必须显式配置同一GlobalAuthorityTiDBHost；dev不再默认回落MySQL。' }
	if ($authoritySql.RequireTiDB -eq 'true') {
		$FriendTiDBHost = $authoritySql.Host
		$TradeTiDBHost = $authoritySql.Host
		$MatchTiDBHost = $authoritySql.Host
	}

	$info = $GoSvcCatalogue[$SvcName]
	$configMapName = $info.ConfigMap
	$configFileName = $info.ConfigFile
	$dbTaskRetentionMs = $script:KafkaDbTaskRetentionMs
	# db_task topic 每分区的 retention.bytes(S5b):写路径入口的 Resolve-KafkaDiskPlan 按盘预算算好(盘不够时已按盘收缩)。
	# db 与 login 的 ConfigMap 都从这一个变量渲染 Kafka.RetentionBytes,与上面的 $dbTaskRetentionMs 同一条纪律:两个服务启动时
	# 都对同一个 db_task topic 做 IncrementalAlterConfigs,值一不同,保留量就随重启顺序来回横跳(B-15)。
	# 只在生成 db / login 时求值;缓存为空(没经过写路径入口的调用)时现场 Resolve 并判盘变更,算不出即 throw。
	$dbTaskRetentionBytes = ''
	if ($SvcName -eq 'db' -or $SvcName -eq 'login') {
		$dbTaskDiskPlan = $script:KafkaDiskPlan
		if ($null -eq $dbTaskDiskPlan) {
			$dbTaskDiskPlan = Resolve-KafkaDiskPlan
			Assert-KafkaStorageChangeIsSafe -Plan $dbTaskDiskPlan
		}
		$dbTaskRetentionBytes = [string][long]$dbTaskDiskPlan.DbTaskRetentionBytes
	}

	# 契约关键值一律从服务自己的 etc/*.yaml 取,不在这里再写一份常数。
	$dbPartitionCnt    = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.PartitionCnt'
	$dbTopicGeneration = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.TopicGeneration'
	$dbSubShardCount   = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.SubShardCount'
	$dbMaxOpenConn     = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Database.MaxOpenConn'
	$dbMaxIdleConn     = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Database.MaxIdleConn'
	# 重试路由与入队落盘确认(S1 ④ 与 WAITAOF,docs/design/db-retry-owner-routing.md):RetryRouting、WaitAOFNumLocal、WaitAOFTimeoutMs
	# 原样镜像 go/db/etc/db.yaml。不在这里另写默认值或取值范围:go/db 启动时 ResolveRetryRouting / Config.Validate 判合法性(非法即拒启),
	# 仓库那份由 go/db/internal/config/config_test.go 的 TestEtcYamlRetryKeysValid 钉住;原样镜像,ConfigMap 与 db.yaml 经同一个加载器、语义相同。
	# 例外是 WaitAOFNumReplicas:它取决于 K8s 上共享 Redis 的形态,不取决于服务配置 —— 按 mapping Redis 拓扑渲染(Get-RouteStoreWaitAOFNumReplicas,
	# 与 data-service 的 RouteStore 同一个函数:单实例 0、高可用 1)。db.yaml 的那一项保持 0,只给本机 compose 单实例用:
	# 改成 1 会让本机 go/db 的 WAITAOF 启动探针等不到副本而拒启。db.yaml 里四个键仍须显式写出,查不到即 throw(Get-AuthoritativeScalar),
	# 不让 ConfigMap 悄悄落回 go/db 的代码默认值。只在生成 db 自己的 ConfigMap 时求值(同下面的落点池参数)。
	$dbRetryRouting       = ''
	$dbWaitAOFNumLocal    = ''
	$dbWaitAOFNumReplicas = ''
	$dbWaitAOFTimeoutMs   = ''
	# MySQL 行内围栏开关 ServerConfig.Fencing(docs/design/db-write-fencing-mysql.md §8):同样原样镜像 db.yaml,
	# 合法性由 go/db 启动时的 Config.Validate 判(legacy | dual,非法即拒启);db.yaml 必须显式写出,查不到即 throw。
	$dbFencingMode          = ''
	$dbFencingEpochOverride = ''
	$dbRequireTiDB          = ''
	if ($SvcName -eq 'db') {
		Assert-DbOwnerEpochRedisDeployment
		$dbFencingMode          = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Fencing.Mode'
		$dbFencingEpochOverride = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Fencing.EpochOverride'
		$dbRequireTiDB          = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Database.RequireTiDB'
		$dbRetryRouting       = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Kafka.RetryRouting'
		$dbWaitAOFNumLocal    = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.RedisClient.WaitAOFNumLocal'
		$null                 = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.RedisClient.WaitAOFNumReplicas'
		$dbWaitAOFNumReplicas = [string](Get-RouteStoreWaitAOFNumReplicas -Topology (Get-MappingRedisTopology))
		$dbWaitAOFTimeoutMs   = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.RedisClient.WaitAOFTimeoutMs'
	}
	# 额外落点库(本 zone 库之外)的连接池:每个库一个池,按库所属家族分两个旋钮,外加空闲连接的回收秒数(G2b)。
	# 从 go/db/etc/db.yaml 的 Placement 段镜像,并核过「正整数且 idle ≤ open」(Get-MysqlPoolMirror);MySQL 连接总账读的是同一批键。
	# 只在生成 db 自己的 ConfigMap 时求值:别的服务不该因为 db 的池参数不合格而生成失败(写路径入口的
	# Assert-MysqlPoolMirror 已经先于任何集群写操作把它们核过一遍)。
	$dbZoneStoreMaxOpenConn         = ''
	$dbZoneStoreMaxIdleConn         = ''
	$dbNonZoneStoreMaxOpenConn      = ''
	$dbNonZoneStoreMaxIdleConn      = ''
	$dbExtraStoreConnMaxIdleSeconds = ''
	if ($SvcName -eq 'db') {
		$dbPoolMirror = Get-MysqlPoolMirror -Target 'db'
		$dbZoneStoreMaxOpenConn         = $dbPoolMirror['Placement.ZoneStoreMaxOpenConn']
		$dbZoneStoreMaxIdleConn         = $dbPoolMirror['Placement.ZoneStoreMaxIdleConn']
		$dbNonZoneStoreMaxOpenConn      = $dbPoolMirror['Placement.NonZoneStoreMaxOpenConn']
		$dbNonZoneStoreMaxIdleConn      = $dbPoolMirror['Placement.NonZoneStoreMaxIdleConn']
		$dbExtraStoreConnMaxIdleSeconds = $dbPoolMirror['Placement.ExtraStoreConnMaxIdleSeconds']
	}
	# D1 第 3 批 go/db 读方门禁 Placement.RequireRouteReady:取本脚本参数 -DbPlacementRequireRouteReady(缺省 false),不镜像 go/db/etc/db.yaml。
	$dbRouteReadyText = if ([bool]$DbPlacementRequireRouteReady) { 'true' } else { 'false' }
	$dbPlacementRequired = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'Placement.Required'
	$loginPlacementBlock = Get-AuthoritativeYamlBlock -RelativePath 'go/login/etc/login.yaml' -Key 'Placement'
	# db 的消费组键(四种形状之一,见 Resolve-DbConsumerGroupPlan):计划由写路径入口的 Initialize-DbConsumerGroupPlans 先算好,
	# 这里只取本 zone 那一份并渲染;缺计划即 throw,不回落任何默认组名。只在生成 db 自己的 ConfigMap 时求值。
	$dbConsumerGroupYaml = ''
	if ($SvcName -eq 'db') {
		$dbConsumerGroupYaml = New-DbConsumerGroupYaml -Plan (Get-DbConsumerGroupPlan -CurrentZoneId $CurrentZoneId)
	}

	$loginPartitionCnt      = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Kafka.PartitionCnt'
	$loginInitialPartition  = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Kafka.InitialPartition'
	$loginTopicGeneration   = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Kafka.TopicGeneration'
	$loginSessionExpireMin  = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.SessionExpireMin'
	$loginMaxLoginDevices   = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.MaxLoginDevices'
	$loginNodeLeaseTTL      = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.LeaseTTL'
	$loginQueueShardCount   = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.QueueShardCount'
	$loginAccountLockTTL    = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Locker.AccountLockTTL'
	$loginPlayerLockTTL     = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Locker.PlayerLockTTL'
	# PlayerId 号段长度(node-id-overhaul-plan §6.2:按「10 分钟峰值建角量」配)。IdSegment.Enabled 与
	# FallbackToSnowflake **不**从 yaml 取:那两个是部署决策,在 login 模板里显式写死并注释。
	$loginIdSegmentStep     = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'IdSegment.Step'
	# 混版本持久字段保护:login C4 的模式(docs/design/mixed-version-persist-field-protection.md §5.1)。从 login.yaml 镜像:
	# 这份 ConfigMap 替代镜像里的 yaml,不镜像 = Go 读到空串按 warn,仓库切 enforce 时集群里悄悄不生效。
	# 只认 warn / enforce(区分大小写),其余生成期 throw(login 启动同样会拒,不能等 pod 起不来才发现)。
	$loginPersistSchemaRegressionMode = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'PersistSchemaRegressionMode'
	if ($loginPersistSchemaRegressionMode -cne 'warn' -and $loginPersistSchemaRegressionMode -cne 'enforce') {
		throw "生成 ConfigMap 失败:go/login/etc/login.yaml 的 PersistSchemaRegressionMode 只能是 warn 或 enforce(当前 '$loginPersistSchemaRegressionMode')。"
	}
	# zrpc 服务端 Timeout(毫秒)从服务 yaml 镜像,与 scene-manager / match 同一条纪律。以前模板里写死 100000:
	# 部署门禁 Assert-GrpcClientDeadlineBudget 核对的是 login.yaml,两边一分家,门禁放行的就不是集群里真正生效的值。
	# 非正整数在这里就拒 —— 0 = go-zero 不装超时拦截器(服务端没有上界),非数字 go-zero 起服即失败,都不该写进 ConfigMap。
	# 值本身 2026-09-29 已由笔误 100000 改正为 10000(LoginNodeService 同步 12000,见 grpc-muduo-transport.md §16.3);
	# 以后再改只需动 login.yaml 与 bin/etc 的 GrpcClient.CallDeadlineMs.LoginNodeService 两处,这里自动跟随,不用动生成器。
	$loginTimeout           = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Timeout'
	$loginTimeoutMs         = [long]0
	if (-not [long]::TryParse($loginTimeout, [ref]$loginTimeoutMs) -or $loginTimeoutMs -le 0) {
		throw "生成 ConfigMap 失败:go/login/etc/login.yaml 的 Timeout='$loginTimeout' 必须是正整数毫秒(0 = go-zero 不装超时拦截器,服务端没有上界;fail-closed:不替你猜一个超时)"
	}

	# data-service 全局库落库消费者(node-id-overhaul-plan §2.0c):topic 名必须与 C++ 生产者一致,
	# 分区数是不可变契约(kafkautil.EnsureTopics 会拒绝与 broker 现状不一致的值),一律从服务 yaml 取。
	$dsYaml                       = 'go/data_service/etc/data_service.yaml'
	$dsKafkaTxTopic               = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.TransactionLogTopic'
	$dsKafkaTxPartitions          = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.TransactionLogPartitions'
	$dsKafkaTxConsumerGroup       = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.TransactionLogConsumerGroup'
	$dsKafkaSnapshotTopic         = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.SnapshotTopic'
	$dsKafkaSnapshotPartitions    = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.SnapshotPartitions'
	$dsKafkaSnapshotConsumerGroup = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.SnapshotConsumerGroup'
	$dsKafkaRetentionMs           = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.RetentionMs'
	# 分区契约的代号,进有效 topic 名(<基名>_g<N>,config.go EffectiveTransactionLogTopic)。镜像进 ConfigMap 是为了
	# 让「这个集群消费第几代 topic」一眼可见;Apply-KafkaTopicInitJob 用同一个键预建 topic,两边不会各说各话。
	$dsKafkaTopicGeneration       = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.TopicGeneration'
	# player_snapshot 消费者的批量 / 并发旋钮与 lag 探针(G2a,config.go KafkaConfig):同样从服务 yaml 镜像,生成器不自带常数。
	# 越界由 data_service 启动时 ValidateConsumers 拒启;打开批量(SnapshotBatchMaxRows > 1)是负责人拍板的显式动作,
	# 改服务 yaml 即可,见 docs/ops/audit-topic-generation-bump-runbook.md。
	$dsKafkaSnapshotBatchMaxRows       = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.SnapshotBatchMaxRows'
	$dsKafkaSnapshotBatchMaxBytes      = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.SnapshotBatchMaxBytes'
	$dsKafkaSnapshotFlushIntervalMs    = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.SnapshotFlushIntervalMs'
	$dsKafkaSnapshotMaxInflightFlushes = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.SnapshotMaxInflightFlushes'
	$dsKafkaLagProbeIntervalMs         = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.LagProbeIntervalMs'
	$dsMysqlMaxOpenConn           = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'SnapshotMySQL.MaxOpenConn'
	$dsMysqlMaxIdleConn           = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'SnapshotMySQL.MaxIdleConn'
	# 名字注册表与 route store 各自的独立连接池(G2b):从服务 yaml 的 PlayerName / RouteStore 段镜像池大小两项,
	# 并核过「正整数且 idle ≤ open」(Get-MysqlPoolMirror);MySQL 连接总账读的是同一批键。以前 ConfigMap 不写这两组,
	# 池大小靠「Go 默认值恰好等于服务 yaml」。与上面 db 的池参数同理,只在生成 data-service 自己的 ConfigMap 时求值。
	$dsPlayerNameMaxOpenConn      = ''
	$dsPlayerNameMaxIdleConn      = ''
	$dsRouteStoreMaxOpenConn      = ''
	$dsRouteStoreMaxIdleConn      = ''
	# RouteStore 段的其余取值(D1 第 1b-3 批):Mode / ReadRequiresReady / Reconcile.ArmReadyMarker 取本脚本参数,FenceShards 按全局库档位、
	# WaitAOF 按 mapping Redis 拓扑(Get-RouteStoreDeploySettings;写路径入口的 Assert-RouteSwitchPreflight 已先于任何写操作核过一遍)。
	$dsRouteSettings              = $null
	if ($SvcName -eq 'data-service') {
		$dsPoolMirror = Get-MysqlPoolMirror -Target 'data-service'
		$dsPlayerNameMaxOpenConn = $dsPoolMirror['PlayerName.MaxOpenConn']
		$dsPlayerNameMaxIdleConn = $dsPoolMirror['PlayerName.MaxIdleConn']
		$dsRouteStoreMaxOpenConn = $dsPoolMirror['RouteStore.MaxOpenConn']
		$dsRouteStoreMaxIdleConn = $dsPoolMirror['RouteStore.MaxIdleConn']
		$dsRouteSettings = Get-RouteStoreDeploySettings -Mode $RouteStoreMode -ArmReadyMarker ([bool]$RouteReconcileArm) `
			-ReadRequiresReady ([bool]$RouteReadRequiresReady) -Topology (Get-MappingRedisTopology)
	}
	# C++ 约定注册(DataServiceNodeService.rpc)那把 etcd 租约的 TTL:Go config 顶层键、默认 60
	# (go/data_service/internal/config/config.go LeaseTTL),与 scene-manager / match 同形,同样从服务自己的 yaml 取。
	$dsLeaseTTL                   = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'LeaseTTL'

	$locatorNodeLeaseTTL    = Get-AuthoritativeScalar -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath 'Node.LeaseTTL'
	$locatorLeaseTTLSeconds = Get-AuthoritativeScalar -RelativePath 'go/player_locator/etc/player_locator.yaml' -KeyPath 'Lease.DefaultTTLSeconds'

	# scene-manager 的 LeaseTTL 在 Go config 顶层(go/scene_manager/internal/config/config.go,
	# default 60),与 match 同形、与 login / player-locator 的 Node.LeaseTTL 不同层。
	# 同样从服务自己的 yaml 取,不在这里另写一份常数。
	$sceneManagerLeaseTTL   = Get-AuthoritativeScalar -RelativePath 'go/scene_manager/etc/scene_manager_service.yaml' -KeyPath 'LeaseTTL'

	# zrpc 服务端 Timeout 连同它必须盖住的两段预算(归属查询、Kafka 同步写)一起从服务 yaml 取。
	# 不写 Timeout 就会落到 go-zero 默认 2000ms,比 Kafka 写超时还短:服务端先超时,EnterScene 的业务失败应答回不到
	# C++ 源 scene,只剩一个「结果未知」的传输失败,交接仍要等满 30s 看门狗裁决(cross-zone-scene-travel.md §12.2;
	# 失败语义见 grpc-muduo-transport.md §16.2)。三者之间的不等式由 tests/k8s_deploy_contract.tests.ps1 守着;
	# 反方向的 C++ deadline(GrpcClient.CallDeadlineMs.SceneManagerNodeService)≥ 本 Timeout + 2000 由写路径入口的
	# Assert-GrpcClientDeadlineBudget 守着 —— 调大这里的 Timeout 必须同步调那边,否则部署被拒。
	$sceneManagerYaml                  = 'go/scene_manager/etc/scene_manager_service.yaml'
	$sceneManagerTimeout               = Get-AuthoritativeScalar -RelativePath $sceneManagerYaml -KeyPath 'Timeout'
	$sceneManagerKafkaWriteTimeout     = Get-AuthoritativeScalar -RelativePath $sceneManagerYaml -KeyPath 'KafkaWriteTimeoutSeconds'
	$sceneManagerHomeZoneLookupTimeout = Get-AuthoritativeScalar -RelativePath $sceneManagerYaml -KeyPath 'HomeZoneLookupTimeoutMs'
	# 铸造 owner_epoch 之后的落盘确认(OwnerEpochMint,docs/design/zone-redis-regression-guard-20261022.md §4.3.2 G3b),
	# 与 db 的 RedisClient.WaitAOF* 同一条纪律:NumLocal / TimeoutMs 原样镜像服务 yaml;NumReplicas 按共享 Redis 的形态渲染
	# (Get-RouteStoreWaitAOFNumReplicas:单实例 0、高可用 1 —— 哨兵切主时被提升的副本上要有这次铸造),yaml 那一项给本机
	# compose 单实例用、保持 0,但仍须显式写出。三个键查不到即 throw,不让 ConfigMap 落回 Go 代码默认值。
	# 只在生成 scene-manager 自己的 ConfigMap 时求值(同 db 段)。
	$sceneManagerWaitAOFNumLocal    = ''
	$sceneManagerWaitAOFNumReplicas = ''
	$sceneManagerWaitAOFTimeoutMs   = ''
	if ($SvcName -eq 'scene-manager') {
		$sceneManagerWaitAOFNumLocal    = Get-AuthoritativeScalar -RelativePath $sceneManagerYaml -KeyPath 'OwnerEpochMint.WaitAOFNumLocal'
		$null                           = Get-AuthoritativeScalar -RelativePath $sceneManagerYaml -KeyPath 'OwnerEpochMint.WaitAOFNumReplicas'
		$sceneManagerWaitAOFNumReplicas = [string](Get-RouteStoreWaitAOFNumReplicas -Topology (Get-MappingRedisTopology))
		$sceneManagerWaitAOFTimeoutMs   = Get-AuthoritativeScalar -RelativePath $sceneManagerYaml -KeyPath 'OwnerEpochMint.WaitAOFTimeoutMs'
	}
	# login 调 scene_manager 的客户端超时同样从 login.yaml 取,不在这里写死:它必须 ≥ 上面的 Timeout + 2000,
	# 不等式由 tests/k8s_deploy_contract.tests.ps1 守着(cross-zone-scene-travel.md §13.5)。
	$loginSceneManagerRpcTimeout       = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'SceneManagerRpc.Timeout'

	# match 的时间窗口全是"多实例 / 崩溃自愈"语义(锁 TTL、票据 TTL、战斗时限),
	# 生成器里另抄一份就是又一处会漂移的常数,一律从服务自己的 yaml 取。
	$matchYaml                  = 'go/match/etc/match_service.yaml'
	$matchMode = 'pro'; $matchGlobalBlock = ''; $matchRatingBlock = ''; $matchMiddlewaresBlock = ''
	if ($SvcName -eq 'match') {
		$matchMiddlewaresBlock = Get-AuthoritativeYamlBlock -RelativePath $matchYaml -Key 'Middlewares'
		if ($ReleaseProfile -eq 'dev') { $matchMode = 'dev' }
		$globalMatch = Get-GlobalMatchDeploymentSettings -Profile $ReleaseProfile -TiDBHost $MatchTiDBHost -Region $PhysicalRegion
		if ($globalMatch.Enabled) { Assert-GlobalMatchKafkaTopology -Topology (Get-KafkaTopology) }
		$matchGlobalBlock = Get-AuthoritativeYamlBlock -RelativePath $matchYaml -Key 'GlobalMatch'
		$enabledValue = ([string][bool]$globalMatch.Enabled).ToLowerInvariant()
		$matchGlobalBlock = $matchGlobalBlock -replace '(?m)^  Enabled:.*$', "  Enabled: $enabledValue"
		if ($globalMatch.Enabled) {
			$matchGlobalBlock = $matchGlobalBlock -replace '(?m)^  RequireTiDB:.*$', '  RequireTiDB: true'
			$matchGlobalBlock = $matchGlobalBlock -replace '(?m)^  Host:.*$', "  Host: $($globalMatch.Host)"
			$matchGlobalBlock = $matchGlobalBlock -replace '(?m)^  User:.*$', "  User: $script:MysqlUser"
		}
		$matchGlobalBlock = $matchGlobalBlock -replace '(?m)^  RankEndpoint:.*$', "  RankEndpoint: rank.${InfraNamespace}:51100"
		$matchGlobalBlock = $matchGlobalBlock -replace '(?m)^  AccountRoleSchema:.*$', "  AccountRoleSchema: ${GlobalDbName}"
		$matchRatingBlock = (@('RatingEnabled','ResultTopic','ResultTopicPartitions','ResultConsumerGroup','RatingToleranceBase',
			'RatingToleranceStepSeconds','RatingToleranceStepDelta','RatingToleranceMax','RatingToleranceMaxWaitSeconds',
			'RatingDrawRoundCap','TableFingerprintMode') | ForEach-Object {
			"${_}: $(Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath $_)"
		}) -join "`n"
	}
	$matchTimeout               = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'Timeout'
	$matchLeaseTTL              = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'LeaseTTL'
	$matchKafkaWriteTimeout     = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'KafkaWriteTimeoutSeconds'
	$matchMatcherIntervalMs     = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'MatcherIntervalMs'
	$matchMatcherLockTTL        = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'MatcherLockTTLSeconds'
	$matchBattleMaxDuration     = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'BattleMaxDurationSeconds'
	$matchChallengeTTL          = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'ChallengeTTLSeconds'
	$matchTicketTTL             = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'TicketTTLSeconds'
	$matchReadyTicketTTL        = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'ReadyTicketTTLSeconds'
	# PveTeamSizeByConfigId 是 map(键 = battle_config_id)。以前按标量只读键 "1",yaml 新增的 config id
	# 在 K8s 上会静默缺失,PVE_TEAM 入队与组队整队开战一律按"未开放"拒绝,本地冒烟又发现不了。
	# 改为整段逐字搬运(Get-AuthoritativeYamlBlock),ConfigMap 与服务 yaml 不会漂移(team-system.md §I.1 批 1 #28)。
	$matchPveTeamSizeBlock      = Get-AuthoritativeYamlBlock -RelativePath $matchYaml -Key 'PveTeamSizeByConfigId'
	# 组队(team-system.md §D.1):跨区开关与 data_service 客户端的超时 / NonBlock 从服务 yaml 取。
	# NonBlock 必须为 true(data-service 没起不能拖垮 match 起服),Go 侧也会强制;这里照抄 yaml 便于一眼核对。
	$matchTeamAllowCrossZone     = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'Team.AllowCrossZone'
	# 队伍补位匹配的应急开关(team-backfill-matching.md §4.6):不渲染的话 Pod 里永远是缺省值,开关按不动。
	$matchTeamFillQueueDisabled  = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'Team.FillQueueDisabled'
	$matchDataServiceRpcTimeout  = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'DataServiceRpc.Timeout'
	$matchDataServiceRpcNonBlock = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'DataServiceRpc.NonBlock'
	# MatchedTicketTTLSeconds 是本轮新增项(cross-zone-matchmaking.md D5,Go 侧缺省 30)。
	# 和上面不同,它允许缺席:yaml 里没有就整行不写,交给 Go 的 default 标签,
	# 这样 go/match 与部署脚本可以分开落地,不会让 infra-up 卡在别人的提交上。
	$matchMatchedTicketTTLLine  = ''
	$matchMatchedTicketTTL = Get-YamlScalar -Path (Join-Path $RepoRoot ($matchYaml -replace '/', [System.IO.Path]::DirectorySeparatorChar)) -KeyPath 'MatchedTicketTTLSeconds'
	if ($matchMatchedTicketTTL.Found) {
		$matchMatchedTicketTTLLine = "MatchedTicketTTLSeconds: $($matchMatchedTicketTTL.Value)"
	}
	# 排行榜评分快照(docs/design/leaderboard-system.md §2.4 / §5.4):开关由参数 -MatchRatingSnapshotPublish 给值、不镜像服务 yaml;
	# topic 名 / 分区数 / 保留期从服务 yaml 取(分区数是与 rank 共享的不可变契约,保留期只由 match 声明)。
	$matchRatingSnapshotPublishEnabled  = if ($MatchRatingSnapshotPublish -eq '1') { 'true' } else { 'false' }
	$matchRatingSnapshotTopic           = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'RatingSnapshotTopic'
	$matchRatingSnapshotTopicPartitions = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'RatingSnapshotTopicPartitions'
	$matchRatingSnapshotRetentionHours  = Get-AuthoritativeScalar -RelativePath $matchYaml -KeyPath 'RatingSnapshotRetentionHours'
	# MatchRedis:六个 StatefulSet pod 的 headless DNS 逗号串,go-zero 的 cluster 客户端按逗号拆
	# (core/stores/redis/redisclustermanager.go splitClusterAddrs)。
	$matchRedisClusterHosts = @(0..5 | ForEach-Object { "redis-match-cluster-$_.redis-match-cluster.${InfraNamespace}.svc.cluster.local:6379" }) -join ','

	# chat 的契约值(超时预算 / 租约 / 长度闸 / 限速 / 历史窗口)同样只从服务自己的 yaml 取,不在这里另抄常数。
	# 与上面 match 那组不同,这里**只在生成 chat 自己的 ConfigMap 时求值**:本函数也被 zone-up 为 db / login 等
	# 逐个调用,无条件读的话,go/chat 尚未落地或 chat.yaml 缺键会把所有 Go 服务的 ConfigMap 一起拖挂
	# (Get-AuthoritativeScalar 查不到即 throw)。生成 chat 时照样 fail-closed。
	$chatYaml                = 'go/chat/etc/chat.yaml'
	$chatName                = ''
	$chatTimeout             = ''
	$chatLeaseTTL            = ''
	$chatMaxContentBytes     = ''
	$chatRateLimitPerSecond  = ''
	$chatHistoryMaxEntries   = ''
	$chatHistoryTTLSeconds   = ''
	$chatOptionalLines       = ''
	$chatSqlMaxOpen          = ''
	$chatSqlDsnEnv           = ''
	$chatLoginPool           = ''
	if ($SvcName -eq 'chat') {
		# Name 也取服务 yaml:go-zero ServiceConf.Name 是必填键,两边写同一个名字,日志 / 链路里才对得上。
		$chatName               = Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'Name'
		$chatSqlDsnEnv          = Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'DSNEnv'
		$chatSqlMaxOpen         = Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'MaxOpenConns'
		$chatLoginPool          = (Get-RedisPoolMirror -Target 'chat')['LoginRedis.PoolSize']
		if ($chatSqlDsnEnv -cne 'CHAT_AUTHORITY_DSN' -or $chatSqlMaxOpen -notmatch '^\d+$' -or [int]$chatSqlMaxOpen -lt 2 -or [int]$chatSqlMaxOpen -gt 128 -or (Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'RequireTiDB') -cne 'true' -or (Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'GlobalLoginSessions') -cne 'true') {
			throw 'Chat必须使用显式TiDB DSN、全局原登录态与有界SQL连接池；禁止Redis权威回退。'
		}
		$chatTimeout            = Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'Timeout'
		$chatLeaseTTL           = Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'LeaseTTL'
		$chatMaxContentBytes    = Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'MaxContentBytes'
		$chatRateLimitPerSecond = Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'RateLimitPerSecond'
		$chatHistoryMaxEntries  = Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'HistoryMaxEntries'
		$chatHistoryTTLSeconds  = Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'HistoryTTLSeconds'
		# 下面三项是查询条数 / 幂等窗口的调参值,允许缺席:yaml 里有就逐字镜像,没有就整行不写、交给 Go 侧 default
		# (照 match 的 MatchedTicketTTLSeconds 写法)。这样 ConfigMap 永远等于服务 yaml 的形状,
		# 不会出现「本地没配、K8s 却多写了一个值」的漂移,go/chat 与本脚本也能分开落地。
		$chatOptional = New-Object System.Collections.Generic.List[string]
		$chatYamlFull = Join-Path $RepoRoot ($chatYaml -replace '/', [System.IO.Path]::DirectorySeparatorChar)
		foreach ($chatKey in @('HistoryDefaultLimit', 'HistoryMaxLimit', 'RequestIdTTLSeconds')) {
			$chatScalar = Get-YamlScalar -Path $chatYamlFull -KeyPath $chatKey
			if ($chatScalar.Found) {
				$chatOptional.Add("${chatKey}: $($chatScalar.Value)")
			}
		}
		$chatOptionalLines = $chatOptional -join "`n"
	}

	# 路由服 client-rpc-router 的契约值(契约 zone_contract_v1 §1):超时预算 / 租约同样只从服务自己的 yaml 取。
	# 与 chat 同理只在生成它自己的 ConfigMap 时求值,免得路由服 yaml 缺键把别的服务的 ConfigMap 一起拖挂。
	# Timeout 必须 > ForwardTimeoutMs(路由服 config.Validate),而 chat 的 Timeout 又必须 <= ForwardTimeoutMs - 1000,
	# 三个数全从各自 yaml 读,改一处就在本地和 K8s 同时生效,不会出现 K8s 上预算链断开。
	$routerYaml             = 'go/client_rpc_router/etc/client_rpc_router.yaml'
	$routerName             = ''
	$routerTimeout          = ''
	$routerLeaseTTL         = ''
	$routerForwardTimeoutMs = ''
	if ($SvcName -eq 'client-rpc-router') {
		$routerName             = Get-AuthoritativeScalar -RelativePath $routerYaml -KeyPath 'Name'
		$routerTimeout          = Get-AuthoritativeScalar -RelativePath $routerYaml -KeyPath 'Timeout'
		$routerLeaseTTL         = Get-AuthoritativeScalar -RelativePath $routerYaml -KeyPath 'LeaseTTL'
		$routerForwardTimeoutMs = Get-AuthoritativeScalar -RelativePath $routerYaml -KeyPath 'ForwardTimeoutMs'
	}

	# 聚宝斋 trade 的契约值:键名与 go/trade/etc/trade.yaml、go/trade/internal/config 逐字一致,值只从服务 yaml 取。
	# 与 chat 同理**只在生成 trade 自己的 ConfigMap 时求值**:go/trade 尚未落地或 trade.yaml 缺键时,
	# 不能把 zone-up 里 db / login 等服务的 ConfigMap 一起拖挂;生成 trade 时照样 fail-closed。
	$tradeYaml                   = 'go/trade/etc/trade.yaml'
	$tradeName                   = ''
	$tradeTimeout                = ''
	$tradeLeaseTTL               = ''
	$tradeMysqlDBName            = ''
	$tradeMysqlMaxOpenConn       = ''
	$tradeMysqlMaxIdleConn       = ''
	$tradeDataServiceTimeout     = ''
	$tradeMarketScope            = ''
	$tradeMarketDefaultPageSize  = ''
	$tradeMarketMaxPageSize      = ''
	$tradeMarketMaxPage          = ''
	$tradeMarketMaxFavorites     = ''
	$tradeIdSegmentOptionalLines = ''
	# 非 dev 档的固定值(见下面 if 块里的注释)。
	$tradeMode                   = 'pro'
	$tradeAutoMigrate            = 'false'
	if ($SvcName -eq 'trade') {
		$tradeSql = Get-TradeSqlSettings -Profile $ReleaseProfile -TiDBHost $TradeTiDBHost -DevelopmentHost "mysql.${InfraNamespace}:3306"
		$tradeAssetOpBlock = Get-AuthoritativeYamlBlock -RelativePath $tradeYaml -Key 'AssetOp'
		$tradeCommerceBlock = Get-AuthoritativeYamlBlock -RelativePath $tradeYaml -Key 'Commerce'
		$tradeMallBlock = Get-AuthoritativeYamlBlock -RelativePath $tradeYaml -Key 'Mall'
		$tradeName                  = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'Name'
		$tradeTimeout               = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'Timeout'
		$tradeLeaseTTL              = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'LeaseTTL'
		# 库名同样取服务 yaml:D-14 规定本地与 K8s 同名(mmorpg_trade),go/trade 的 config.Validate 还会断言它等于
		# 代码常量 data.DatabaseName;库本身由 mysql-init-sql 预建,这里不另写一份库名常数。
		$tradeMysqlDBName           = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'MySQL.DBName'
		$tradeMysqlMaxOpenConn      = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'MySQL.MaxOpenConn'
		$tradeMysqlMaxIdleConn      = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'MySQL.MaxIdleConn'
		$tradeDataServiceTimeout    = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'DataServiceRpc.Timeout'
		$tradeMarketScope           = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'Market.Scope'
		$tradeMarketDefaultPageSize = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'Market.DefaultPageSize'
		$tradeMarketMaxPageSize     = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'Market.MaxPageSize'
		$tradeMarketMaxPage         = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'Market.MaxPage'
		$tradeMarketMaxFavorites    = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'Market.MaxFavoritesPerPlayer'
		# IdSegment 的 Enabled / FallbackToSnowflake 是部署决策,在 case 里写死并注释;Step / MinStep / MaxStep 是调参值,
		# 允许缺席:yaml 里有就逐字镜像,没有就整行不写、交给 shared/idsegment 的默认(照 chat 可选键的写法)。
		$tradeIdSegmentOptional = New-Object System.Collections.Generic.List[string]
		$tradeYamlFull = Join-Path $RepoRoot ($tradeYaml -replace '/', [System.IO.Path]::DirectorySeparatorChar)
		foreach ($tradeKey in @('Step', 'MinStep', 'MaxStep')) {
			$tradeScalar = Get-YamlScalar -Path $tradeYamlFull -KeyPath "IdSegment.$tradeKey"
			if ($tradeScalar.Found -and -not [string]::IsNullOrWhiteSpace($tradeScalar.Value)) {
				$tradeIdSegmentOptional.Add("  ${tradeKey}: $($tradeScalar.Value)")
			}
		}
		$tradeIdSegmentOptionalLines = $tradeIdSegmentOptional -join "`n"
		# Mode 决定 TradeAdmin.SeedListing 是否可用(方法内只放行 dev/test,聚宝斋 P1-5);Schema.AutoMigrate 决定启动路径是否执行 DDL。
		# 与 data-service 的 Schema.AutoMigrate、login 的 Mode 同一条纪律:dev 档镜像服务 yaml 的值;staging/prod 固定
		# Mode: pro(种子造数入口关死,不能带着 dev 造数能力上线)与 AutoMigrate: false(表只由 trade-migrate Job 建,
		# 启动路径只跑一次只读 plan,有待执行语句或需人工项就拒绝启动)。
		if ($ReleaseProfile -eq 'dev') {
			$tradeMode        = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'Mode'
			$tradeAutoMigrate = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'Schema.AutoMigrate'
		}
	}
	# 资产通道段(-AssetOps):off 时只写 Enabled: false 与 SecretEnv 两行(与开关引入之前逐字相同);on 时整块从服务 yaml 原样搬运、
	# 只把第一层 Enabled 改成 true(go/trade/etc/trade.yaml 注释:打开 K8s 通道要把落地恢复与 Cleanup* 键连同 Enabled=true 一起补进 ConfigMap)。
	# SecretEnv 必须是部署侧注入的那个变量名,否则 trade 读不到密钥、带着 Enabled=true 拒启。
	$tradeAssetOpBlock = "AssetOp:`n  Enabled: false`n  SecretEnv: $($script:AssetOpCallers['trade'].Env)"
	if ($SvcName -eq 'trade' -and $script:AssetOpsOn) {
		$tradeSecretEnv = Get-AuthoritativeScalar -RelativePath $tradeYaml -KeyPath 'AssetOp.SecretEnv'
		if ($tradeSecretEnv -cne $script:AssetOpCallers['trade'].Env) {
			throw "生成 trade ConfigMap 失败:$tradeYaml 的 AssetOp.SecretEnv=$tradeSecretEnv,而部署侧注入的是 $($script:AssetOpCallers['trade'].Env)(trade 会读不到密钥而拒启)"
		}
		$tradeAssetOpLines = @((Get-AuthoritativeYamlBlock -RelativePath $tradeYaml -Key 'AssetOp') -split "`r?`n")
		$tradeEnabledHits = @(for ($i = 0; $i -lt $tradeAssetOpLines.Count; $i++) { if ($tradeAssetOpLines[$i] -match '^  Enabled:') { $i } })
		if ($tradeEnabledHits.Count -ne 1) {
			throw "生成 trade ConfigMap 失败:$tradeYaml 的 AssetOp 块里两格缩进的 Enabled 应恰好 1 行,实际 $($tradeEnabledHits.Count) 行"
		}
		$tradeAssetOpLines[$tradeEnabledHits[0]] = '  Enabled: true'
		$tradeAssetOpBlock = $tradeAssetOpLines -join "`n"
	}

	# 好友 friend 的契约值:键名与 go/friend/etc/friend.yaml、go/friend/internal/config 逐字一致,值只从服务 yaml 取
	# (go/friend/internal/config/config.go 头注释把"三处逐字一致"写成了硬约束 —— 键名写错 go-zero 当未知键静默忽略,
	#  现象是"线上跑的还是默认值",没有任何报错)。
	# 与 chat / trade 同理**只在生成 friend 自己的 ConfigMap 时求值**:friend.yaml 缺键时不能把 zone-up 里
	# db / login 等服务的 ConfigMap 一起拖挂;生成 friend 时照样 fail-closed(Get-AuthoritativeScalar 查不到即 throw)。
	$friendYaml                    = 'go/friend/etc/friend.yaml'
	$friendName                    = ''
	$friendTimeout                 = ''
	$friendLeaseTTL                = ''
	$friendMysqlDBName             = ''
	$friendMysqlMaxOpenConn        = ''
	$friendMysqlMaxIdleConn        = ''
	$friendMaxFriends              = ''
	$friendMaxPendingRequests      = ''
	$friendMaxIncomingRequests     = ''
	$friendMaxBlocks               = ''
	$friendRecommendDefaultLimit   = ''
	$friendRecommendMaxLimit       = ''
	$friendRecommendMaxExclude     = ''
	$friendRequestQuotaPerMinute   = ''
	$friendListReadHardLimit       = ''
	$friendCacheTTL                = ''
	$friendSweepMode               = ''
	$friendSweepInterval           = ''
	$friendSweepRetentionDays      = ''
	$friendSweepBatchLimit         = ''
	# 非 dev 档的固定值(见下面 if 块里的注释)。
	$friendMode                    = 'pro'
	$friendAutoMigrate             = 'false'
	$friendSql                    = $null
	if ($SvcName -eq 'friend') {
		$friendSql = Get-FriendSqlSettings -Profile $ReleaseProfile -TiDBHost $FriendTiDBHost -DevelopmentHost "mysql.${InfraNamespace}:3306"
		$friendName                  = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Name'
		$friendTimeout               = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Timeout'
		$friendLeaseTTL              = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'LeaseTTL'
		# 库名同样取服务 yaml:D-14 规定本地与 K8s 同名(mmorpg_friend),go/friend 的 config.Validate 还会断言它等于
		# 代码常量 data.DatabaseName;库本身由 mysql-init-sql 预建,这里不另写一份库名常数。
		$friendMysqlDBName           = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'MySQL.DBName'
		$friendMysqlMaxOpenConn      = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'MySQL.MaxOpenConn'
		$friendMysqlMaxIdleConn      = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'MySQL.MaxIdleConn'
		# 好友业务阈值:全是**硬上限与限流**而不是调优旋钮,每一项为 0 都会被 config.Validate 拒。
		# 一条都不能漏写:go-zero 对"非 optional 但整段缺失"的 Friend 段会按空 map 下钻并回填一整套 default,
		# 不报错也不全变 0 —— 漏写等于线上悄悄换了一套阈值。
		$friendMaxFriends            = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.MaxFriends'
		$friendMaxPendingRequests    = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.MaxPendingRequests'
		$friendMaxIncomingRequests   = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.MaxIncomingRequests'
		$friendMaxBlocks             = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.MaxBlocks'
		$friendRecommendDefaultLimit = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.RecommendDefaultLimit'
		$friendRecommendMaxLimit     = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.RecommendMaxLimit'
		$friendRecommendMaxExclude   = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.RecommendMaxExclude'
		$friendRequestQuotaPerMinute = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.RequestQuotaPerMinute'
		$friendListReadHardLimit     = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.ListReadHardLimit'
		$friendCacheTTL              = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.CacheTTL'
		# Sweep 四个键必须全写:整段标了 optional,而 go-zero **不下钻整段缺失的 optional 嵌套结构**、
		# default 标签不回填 —— 漏写会让 Mode 变空串并被 Validate 拒(配置错误而不是静默降级,但照样 CrashLoop)。
		$friendSweepMode             = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.Sweep.Mode'
		$friendSweepInterval         = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.Sweep.Interval'
		$friendSweepRetentionDays    = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.Sweep.RetentionDays'
		$friendSweepBatchLimit       = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Friend.Sweep.BatchLimit'
		# 与 trade / data-service / login 同一条纪律:dev 档镜像服务 yaml 的值;staging/prod 固定
		# Mode: pro(dev/test 才放行的调试入口关死,不能带着造数能力上线)与 AutoMigrate: false
		# (表只由 friend-migrate Job 建,启动路径只跑一次只读 plan,不净即拒启并打印补救命令)。
		if ($ReleaseProfile -eq 'dev') {
			$friendMode        = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Mode'
			$friendAutoMigrate = Get-AuthoritativeScalar -RelativePath $friendYaml -KeyPath 'Schema.AutoMigrate'
		}
	}

	# 玩家收件箱 player-inbox 的契约值(docs/design/player-inbox-service.md §1.4、§11.6):键名与 go/player_inbox/etc/player_inbox.yaml、
	# go/player_inbox/internal/config 逐字一致,值只从服务 yaml 取。与 trade / friend 同理**只在生成 player-inbox 自己的 ConfigMap 时求值**。
	$playerInboxYaml                 = 'go/player_inbox/etc/player_inbox.yaml'
	$playerInboxName                 = ''
	$playerInboxTimeout              = ''
	$playerInboxLeaseTTL             = ''
	$playerInboxMysqlDBName          = ''
	$playerInboxMysqlMaxOpenConn     = ''
	$playerInboxMysqlMaxIdleConn     = ''
	$playerInboxDataServiceTimeout   = ''
	$playerInboxKafkaWriteTimeoutMs  = ''
	$playerInboxEnqueueTypesLines    = ''
	# 顶层 Middlewares 段(评审 L12):整段从服务 yaml 复制,只在生成 player-inbox 自己的 ConfigMap 时取。
	$playerInboxMiddlewaresBlock     = ''
	# PlayerInbox 段里的后台循环参数(清理 / 提醒补发 / gauge / 对账,设计 §6.3、§9):逐键镜像服务 yaml,一项都不漏 ——
	# Go 侧对缺席的键回填默认值,模板漏一项不会报错,现象是「集群里跑的是默认值」。键名与 go/player_inbox/internal/config 逐字一致。
	$playerInboxLoopKeys = @('TerminalRetentionDays', 'SweepIntervalSeconds', 'NotifyResendIntervalSeconds',
		'GaugeIntervalSeconds', 'ReconcileIntervalMinutes', 'ReconcileWindowHours')
	$playerInboxLoopLines            = ''
	# 非 dev 档的固定值(理由同 trade:不能带着 dev 调试入口上线;表只由 player-inbox-migrate Job 建)。
	$playerInboxMode                 = 'pro'
	$playerInboxAutoMigrate          = 'false'
	if ($SvcName -eq 'player-inbox') {
		$playerInboxName               = Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath 'Name'
		# zrpc 服务端超时:C++ 侧 GrpcClient.CallDeadlineMs.PlayerInboxNodeService ≥ 本值 + 2000 由写路径入口的
		# Assert-GrpcClientDeadlineBudget 核对服务 yaml;这里镜像同一个值,集群里生效的就是被核对过的那个。
		$playerInboxTimeout            = Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath 'Timeout'
		$playerInboxLeaseTTL           = Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath 'LeaseTTL'
		# 库名同样取服务 yaml:D-14 规定本地与 K8s 同名(mmorpg_player_inbox),Go 侧 config.Validate 断言它等于代码常量。
		$playerInboxMysqlDBName        = Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath 'MySQL.DBName'
		$playerInboxMysqlMaxOpenConn   = Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath 'MySQL.MaxOpenConn'
		$playerInboxMysqlMaxIdleConn   = Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath 'MySQL.MaxIdleConn'
		$playerInboxDataServiceTimeout = Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath 'DataServiceRpc.Timeout'
		$playerInboxKafkaWriteTimeoutMs = Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath 'Kafka.WriteTimeoutMs'
		$playerInboxLoopLines = @($playerInboxLoopKeys | ForEach-Object {
			"  ${_}: $(Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath "PlayerInbox.$_")"
		}) -join "`n"
		# 下面这些键的**值**由本模板按部署形态写死(集群内地址),但键本身必须在服务 yaml 里出现:
		# go-zero 对写错的键静默忽略,模板与 Go 侧键名一旦分家,现象是「集群里用的是默认值」而没有任何报错。
		# 在生成期就逐个核对(查不到即 throw,点名是哪个键)。PlayerInbox.AdminTokenEnv 的核对在 Initialize-InjectedSecrets
		# (只在确实要注入口令时才要求,见 Assert-PlayerInboxAdminTokenEnvDeclared)。
		foreach ($playerInboxKey in @('MySQL.Host', 'DataServiceRpc.Etcd.Key', 'CoreRedis.Host', 'Kafka.Brokers', 'PlayerInbox.EnqueueTypes')) {
			[void](Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath $playerInboxKey)
		}
		$playerInboxEnqueueTypesLines = Get-PlayerInboxEnqueueTypesYaml -RelativePath $playerInboxYaml
		# go-zero 服务端中间件配置(评审 L12):Stat 拦截器缺省开启(Middlewares.Stat 缺省 true,整段缺席也回填),Go 侧 config.Validate
		# 在 Stat 开着时要求 StatConf.IgnoreContentMethods 同时列出 FetchItems 与 AckItems,缺了就拒启 —— player-inbox 与用同一份
		# ConfigMap 的 player-inbox-migrate Job(conf.MustLoad 同样跑 Validate)都起不来。整段原样复制,不在模板里抄第二份方法名;
		# 服务 yaml 缺这一段即 throw(Get-AuthoritativeYamlBlock)。
		$playerInboxMiddlewaresBlock = Get-AuthoritativeYamlBlock -RelativePath $playerInboxYaml -Key 'Middlewares'
		if ($ReleaseProfile -eq 'dev') {
			$playerInboxMode        = Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath 'Mode'
			$playerInboxAutoMigrate = Get-AuthoritativeScalar -RelativePath $playerInboxYaml -KeyPath 'Schema.AutoMigrate'
		}
	}

	# mail/rank 沿用下方服务 YAML 整体镜像；容量、Sweep、签名与 SQL 配置保持单一事实源。

	$mysqlUser = $script:MysqlUser
	$mysqlPassword = $script:MysqlPassword
	$redisPassword = $script:RedisPassword
	$gateTokenSecret = $script:GateTokenSecret
	$internalAuthSecret = $script:InternalAuthSecret

	# go-redis 直连客户端的连接池上限(P0-E 第 3 批):从服务 yaml 镜像,并核过「正整数」(Get-RedisPoolMirror);
	# Redis 连接总账读的是同一批键。以前 ConfigMap 不写它,池上限 = go-redis 缺省 10 × GOMAXPROCS,跟着 CPU limit 漂。
	# 只在生成该服务自己的 ConfigMap 时求值(写路径入口的 Write-RedisConnectionLedger 已经先于任何集群写操作把它们全部读过一遍)。
	$loginRedisPoolSize   = ''
	$dbRedisPoolSize      = ''
	$dsDevRedisPoolSize   = ''
	$locatorRedisPoolSize = ''
	$tradeRedisPoolSize   = ''
	$playerInboxRedisPoolSize = ''
	switch ($SvcName) {
		'login' { $loginRedisPoolSize = (Get-RedisPoolMirror -Target 'login')['Node.RedisClient.PoolSize'] }
		'db' { $dbRedisPoolSize = (Get-RedisPoolMirror -Target 'db')['ServerConfig.RedisClient.PoolSize'] }
		'data-service' { $dsDevRedisPoolSize = (Get-RedisPoolMirror -Target 'data-service')['DevRedis.PoolSize'] }
		'player-locator' { $locatorRedisPoolSize = (Get-RedisPoolMirror -Target 'player-locator')['RedisClient.PoolSize'] }
		'trade' { $tradeRedisPoolSize = (Get-RedisPoolMirror -Target 'trade')['SharedRedis.PoolSize'] }
		'player-inbox' { $playerInboxRedisPoolSize = (Get-RedisPoolMirror -Target 'player-inbox')['CoreRedis.PoolSize'] }
	}

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
	#
	# 只在生成 db 自己的 ConfigMap 时解析:这是 db 独有的注入项,以前无条件求值只是
	# 顺手;现在全局服务(match)会在 infra-up 阶段也走这个函数,staging/prod 的
	# infra-up 不该因为一个 db 才用的密钥缺席而失败。
	$dbAllowedDatabasesYaml = ''
	$dsAccountDirectoryBlock = ''
	if ($SvcName -in @('db','data-service')) {
		$dbAllowedDatabases = Resolve-InjectedSecret -EnvName "MMORPG_DB_ALLOWED_DATABASES" `
			-DevFallback '' -ReleaseProfile $ReleaseProfile `
			-Purpose "db 库名白名单(逗号分隔,如 zone_1_db,zone_2_db)" -MinLength 3
		$roleProofStorageIds = @(Get-RoleProofStorageIds -DatabaseNames $dbAllowedDatabases)
		$dbAllowedList = @($dbAllowedDatabases -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ })
		if ($dbAllowedList.Count -eq 0) {
			throw "生成 db ConfigMap 失败:库名白名单解析后为空(MMORPG_DB_ALLOWED_DATABASES='$dbAllowedDatabases')。"
		}
		$dbAllowedDatabasesYaml = ($dbAllowedList | ForEach-Object { "      - `"$_`"" }) -join "`n"
		if ($SvcName -eq 'data-service') {
			$dsAccountDirectoryBlock = Get-AuthoritativeYamlBlock -RelativePath 'go/data_service/etc/data_service.yaml' -Key 'AccountDirectory'
			if ([regex]::Matches($dsAccountDirectoryBlock,'(?m)^    AllowedStorageIDs:.*$').Count -ne 1) { throw 'DS正规角色SQL证明白名单源不唯一。' }
			$dsAccountDirectoryBlock = $dsAccountDirectoryBlock -replace '(?m)^    AllowedStorageIDs:.*$', ('    AllowedStorageIDs: ['+($roleProofStorageIds -join ', ')+']')
		}
	}

	# 启动期 DDL 开关。go/db 默认全 false 且库不存在时 fail-closed
	# (proto_sql/db.go openDB:"database does not exist and startup-path DDL is disabled"),
	# 以前这份 ConfigMap 两个开关都没写,K8s 上的 db 只要 zone_<id>_db 没预建就拒启。
	# dev 档镜像 go/db/etc/db.yaml 的值(本地"起了就能用"语义,建库仍受上面的白名单约束);
	# staging/prod 固定 false:库由 infra-up 的 mysql-init-sql ConfigMap 预建、
	# 表由部署阶段的 `go run ./cmd/migrate -command up` 迁移负责(go/db/README.md)。
	$dbAutoCreateDatabase = 'false'
	$dbAutoMigrateSchema = 'false'
	if ($SvcName -eq 'db' -and $ReleaseProfile -eq 'dev') {
		$dbAutoCreateDatabase = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Database.AutoCreateDatabase'
		$dbAutoMigrateSchema = Get-AuthoritativeScalar -RelativePath 'go/db/etc/db.yaml' -KeyPath 'ServerConfig.Database.AutoMigrateSchema'
	}

	# data_service 全局库各表的建表策略(go/data_service/internal/config/config.go SchemaConfig),
	# 与上面 db 的 AutoMigrateSchema 同一条纪律:dev 档镜像服务 yaml 的值(AutoMigrate=true,
	# 起了就能用);staging/prod 固定 false —— 多副本同时启动会对同一张表并发 ALTER 互相 MDL 阻塞,
	# 生产改在部署阶段单进程跑一次 `data_service -f /app/etc/data_service.yaml -migrate`
	# (同一段代码,跑完即退)。设 false 时启动路径不碰任何 DDL。
	$dataServiceAutoMigrate = 'false'
	if ($SvcName -eq 'data-service' -and $ReleaseProfile -eq 'dev') {
		$dataServiceAutoMigrate = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Schema.AutoMigrate'
	}

	# id_segment 的**行**播种策略(config.go IdSegmentConfig.AllowAutoSeed),与 Schema.AutoMigrate 同一条纪律:
	# dev 档镜像服务 yaml 的值(true:空库起了就能建角,随手换 tag 不必先跑迁移);staging/prod 固定 false ——
	# 生产里 biz_tag 行缺失只可能是全局库被 drop / 从旧备份恢复,自动从 1 播种等于把已发出的号再发一遍
	# (login 的 INSERT ... ON DUPLICATE KEY UPDATE 会静默覆盖别人的角色行),必须由人核对消费侧最大号后处理
	# (deploy/k8s/README.md「恢复全局库前须先核对 id_segment.max_id」)。false 时缺行 = ErrCodeIdSegmentUnknownTag,不写任何行。
	$dataServiceAllowAutoSeed = 'false'
	if ($SvcName -eq 'data-service' -and $ReleaseProfile -eq 'dev') {
		$dataServiceAllowAutoSeed = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'IdSegment.AllowAutoSeed'
	}

	# login 的 go-zero Mode。secrets.go 只在 Mode=dev/test 时把密钥校验降级为 WARN,
	# 其余(含不写,默认 pro)一律 fail-closed。dev 档位的密钥本来就是占位回落
	# (Initialize-InjectedSecrets),不写 Mode 的话 login 起来就 panic
	# "HMAC 密钥配置不合格,拒绝启动"(2026-09-03 kind 实跑);与 go/login/etc/login.yaml
	# 的 `Mode: dev` 口径一致。staging/prod 不写,保持 pro 的严格门禁。
	$loginModeLine = if ($ReleaseProfile -eq 'dev') { 'Mode: dev' } else { '' }

	# login / scene_manager 下发 gate 地址时是否必须用 gate 自报的客户端可达地址(D78,ingress_final §3):
	# 默认 auto 跟随模式(external → true,没自报地址的 gate 被跳过、绝不回落集群内 PodIP;podip → false),
	# 上线窗口里可由 -RequireClientEndpoint 显式覆盖(见参数注释)。两个服务取同一个值。
	# 变量名刻意不叫 $requireClientEndpoint:PowerShell 变量名不分大小写,那会遮住脚本参数 -RequireClientEndpoint。
	$requireClientEndpointValue = Resolve-RequireClientEndpoint

	# login 的开发口令认证段(-LoginDevPasswordAuth,2b §7)。生效还依赖两件事,缺一个 login 就启动 panic(fail-closed):
	# 同一份配置的 Mode 为 dev(上面的 $loginModeLine,dev 档恒写),以及容器 env 里的共享口令(Apply-OneGoSvc 经 Secret 注入)。
	# 与 PasswordAuth 互斥(auth_init.go),这份 ConfigMap 不写后者。非 dev 档在这里再拒一次(纵深防御,preflight 已先拒)。
	$loginDevPasswordAuthBlock = ''
	if ($SvcName -eq 'login' -and $LoginDevPasswordAuth) {
		if ($ReleaseProfile -ne 'dev') {
			throw "生成 login ConfigMap 失败:-LoginDevPasswordAuth 只允许 -ReleaseProfile dev(当前 $ReleaseProfile)。"
		}
		$loginDevPasswordAuthBlock = "`n" + (New-LoginDevPasswordAuthYaml)
	}

	# login 的登录排队段与排队令牌密钥(G4b,-LoginQueue / -ReleaseProfile)。只在生成 login 自己的 ConfigMap 时求值:
	# 别的服务不读这两项,也不该因为 login.yaml 的 Queue 段缺键而生成失败(New-LoginQueueYaml 查不到即 throw)。
	# 关 = 两个片段都是空串,login 模板的产物与 G4b 之前逐字相同。
	$loginQueueBlock = ''
	$loginQueueTokenSecretBlock = ''
	if ($SvcName -eq 'login') {
		$queueYaml = New-LoginQueueYaml -CurrentZoneId $CurrentZoneId
		if (-not [string]::IsNullOrEmpty($queueYaml)) {
			$loginQueueBlock = "`n" + $queueYaml
			# Secrets.QueueToken 与 InternalAuth 同处一个 Secrets: 键(不能另起一个 Secrets:,重复键后者覆盖前者)。与兄弟项 InternalAuth
			# 一样写明文 Value(暴露面相同)。dev 档允许为空:不写 → login 宽松档回落 GateToken 密钥并打 WARN(secrets.go ResolveSecrets);
			# staging / prod 由 Initialize-InjectedSecrets 保证非空、≥32、与另两把不同。
			if (-not [string]::IsNullOrEmpty($script:LoginQueueTokenSecret)) {
				$loginQueueTokenSecretBlock = "`n  QueueToken:`n    Value: `"$($script:LoginQueueTokenSecret)`""
			}
		}
	}

	# login 连核心与排队 Redis 的地址(G4a 第 1a 批,-RedisSplit;唯一来源 Get-RedisPurposeTopology)。Node.RedisClient 恒为核心;
	# 解析为 queue 时在 Node 段整段写 QueueRedis:除 Host 外与 RedisClient 逐项同值(口令、DB 0、DefaultTTL、三个超时、池上限)。
	# login 的 QueueRedis 是指针 + optional,段一出现内层字段全部必填(go/login/internal/config/config.go NodeConfig),所以必须写全;
	# 它只承载排队键 queue:* / admit:* / admitted:* 与放行锁,gate 排空标记、会话、锁仍走 RedisClient。none 时为空串,login 模板的产物
	# 与拆分前逐字节相同。连接总账:QueueRedis 的池记在 queue 实例那一份(Write-RedisQueueConnectionLedger)。
	# 两段共用的取值写成变量,RedisClient 与 QueueRedis 不会各改各的。
	$loginRedisDefaultTTL = '24h'
	$loginRedisTimeout = '3s'
	$loginCoreRedisHost = ''
	$loginQueueRedisBlock = ''
	if ($SvcName -eq 'login') {
		$loginRedisPurpose = Get-RedisPurposeTopology -CurrentZoneId $CurrentZoneId
		$loginCoreRedisHost = $loginRedisPurpose.Core.HostPort
		if ($loginRedisPurpose.QueueSplit) {
			$loginQueueRedisBlock = "`n" + @"
  # 排队 Redis(-RedisSplit queue,docs/design/shared-redis-split-by-purpose.md §25.3):只放排队键 queue:* / admit:* / admitted:* 与放行锁;
  # gate 排空标记、会话、锁仍在上面的 RedisClient。除 Host 外与 RedisClient 逐项同值(k8s_deploy.ps1 同一组变量渲染)。
  QueueRedis:
    Host: $($loginRedisPurpose.Queue.HostPort)
    Password: "${redisPassword}"
    DB: 0
    DefaultTTL: ${loginRedisDefaultTTL}
    DialTimeout: ${loginRedisTimeout}
    ReadTimeout: ${loginRedisTimeout}
    WriteTimeout: ${loginRedisTimeout}
    PoolSize: ${loginRedisPoolSize}
"@
		}
	}

	# login 的第三方登录段(-LoginWeChatAppId / -LoginQQAppId,New-LoginAuthProvidersYaml)。只在生成 login 自己的 ConfigMap 时求值;
	# 两个 AppId 都没给 = 空串,login 模板的产物与接入前逐字相同。只写公开值:微信 AppSecret 经 Apply-OneGoSvc 建的 Secret login-oauth
	# 注入 AppSecretEnv 指向的容器环境变量,绝不进 ConfigMap。AuthProviders 在模板别处没有,整份 login.yaml 只此一处顶层键。
	$loginAuthProvidersBlock = ''
	if ($SvcName -eq 'login') {
		$authProvidersYaml = New-LoginAuthProvidersYaml -WeChatAppId $LoginWeChatAppId -WeChatEndpoint $LoginWeChatEndpoint `
			-WeChatAllowOpenId $LoginWeChatAllowOpenId.IsPresent -QQAppId $LoginQQAppId -QQEndpoint $LoginQQEndpoint `
			-QQAllowOpenId $LoginQQAllowOpenId.IsPresent -ReleaseProfile $ReleaseProfile -WeChatAppSecretEnv $script:LoginWeChatAppSecretEnv
		if (-not [string]::IsNullOrEmpty($authProvidersYaml)) {
			$loginAuthProvidersBlock = "`n" + $authProvidersYaml
		}
	}

	$svcConfig = switch ($SvcName) {
		"guild" { New-GuildServiceConfigYaml -CurrentZoneId $CurrentZoneId -CurrentClusterId $CurrentClusterId }
		"mail" {
			# 域配置仍以服务yaml为源，只改容器寻址；DSN/密钥值由manifest的Secret注入。
			$source = Get-Content -LiteralPath (Join-Path $RepoRoot 'go/mail/etc/mail.yaml') -Raw
			$source = $source -replace '(?m)^KafkaBrokers:.*$', "KafkaBrokers: [kafka.${InfraNamespace}:9092]"
			$source = $source -replace '(?m)^ListenOn:.*$', 'ListenOn: 0.0.0.0:50900'
			$source = $source -replace '(?m)^AdvertiseHost:.*$', 'AdvertiseHost: ""'
			$authorityMode = if ($ReleaseProfile -eq 'dev') { 'dev' } else { 'pro' }
			$source = $source -replace '(?m)^Mode:.*$', "Mode: $authorityMode"
            if ($ReleaseProfile -ne 'dev') {
                $source = $source -replace '(?m)^AutoMigrate:.*$', 'AutoMigrate: false'
                $source = $source -replace '(?m)^RequireTiDB:.*$', 'RequireTiDB: true'
            }
			$source = $source -replace '(?m)^ZoneId:.*$', "ZoneId: $CurrentZoneId"
			$source = $source -replace '(?m)^MetricsListenAddr:.*$', 'MetricsListenAddr: 0.0.0.0:9240'
			$source = $source -replace '127.0.0.1:2379', "etcd.${InfraNamespace}:2379"
			$source = $source -replace '(?m)^  Endpoints: \[127.0.0.1:9000\]\r?$', "  Etcd:`n    Hosts: [etcd.${InfraNamespace}:2379]`n    Key: dataservice.rpc"
			$source = $source -replace '(?m)^TableDir:.*$', 'TableDir: /generated/tables'
			$source = $source -replace '(?m)^SharedRedisAddr:.*$', "SharedRedisAddr: redis.${InfraNamespace}:6379"
			$source = $source -replace '(?m)^AccountRoleSchema:.*$', "AccountRoleSchema: ${GlobalDbName}"
			$source = $source -replace '(?m)^GlobalLoginSessions:.*$', 'GlobalLoginSessions: true'
			$loginSessionDb = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.RedisClient.DB'
			$source = $source -replace '(?ms)^LoginRedis:\r?\n(?:[ \t]+[^\r\n]*\r?\n)*', "LoginRedis:`n  Host: redis.${InfraNamespace}:6379`n  DB: $loginSessionDb`n  PasswordEnv: MMORPG_MAIL_REDIS_PASSWORD`n"
			$source = $source -replace '(?m)^SharedRedisPasswordEnv:.*\r?\n', ''
			$source + "`nSharedRedisPasswordEnv: MMORPG_MAIL_REDIS_PASSWORD`n"
		}
		"rank" {
			$source = Get-Content -LiteralPath (Join-Path $RepoRoot 'go/rank/etc/rank.yaml') -Raw
			$source = $source -replace '(?m)^ListenOn:.*$', 'ListenOn: 0.0.0.0:51100'
			$source = $source -replace '(?m)^AdvertiseHost:.*$', 'AdvertiseHost: ""'
			$authorityMode = if ($ReleaseProfile -eq 'dev') { 'dev' } else { 'pro' }
			$source = $source -replace '(?m)^Mode:.*$', "Mode: $authorityMode"
            if ($ReleaseProfile -ne 'dev') {
                $source = $source -replace '(?m)^AutoMigrate:.*$', 'AutoMigrate: false'
                $source = $source -replace '(?m)^RequireTiDB:.*$', 'RequireTiDB: true'
            }
			$source = $source -replace '(?m)^ZoneId:.*$', "ZoneId: $CurrentZoneId"
			$source = $source -replace '(?m)^MetricsListenAddr:.*$', 'MetricsListenAddr: 0.0.0.0:9250'
			$source = $source -replace '127.0.0.1:2379', "etcd.${InfraNamespace}:2379"
			$source = $source -replace '(?m)^  Endpoints: \[127.0.0.1:9000\]\r?$', "  Etcd:`n    Hosts: [etcd.${InfraNamespace}:2379]`n    Key: dataservice.rpc"
			$source = $source -replace '127.0.0.1:50900', "mail.${InfraNamespace}:50900"
			$source -replace '(?m)^TableDir:.*$', 'TableDir: /generated/tables'
		}
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
${dbConsumerGroupYaml}
    TopicGeneration: ${dbTopicGeneration}
    PartitionCnt: ${dbPartitionCnt}
    SubShardCount: ${dbSubShardCount}
    # 重试路由 shared | owner(镜像 go/db/etc/db.yaml,docs/design/db-retry-owner-routing.md)。只改 ConfigMap 不会滚动 db:
    # 改值后必须 rollout restart deploy/db,以每个 Pod 的启动日志 "retry routing: mode=… source=…" 与 db_retry_routing_mode 核对。
    # 单 zone 金丝雀用 kubectl set env deploy/db DB_RETRY_ROUTING=owner(环境变量优先于本键,并且会触发滚动)。
    RetryRouting: "${dbRetryRouting}"
    IsOfflineExpand: false
    # db_task topic 的保留期与每分区保留量:与 login 的 ConfigMap 同值(k8s_deploy.ps1 的 Apply-KafkaProfileDefaults / Get-KafkaDiskPlan)。
    RetentionMs: ${dbTaskRetentionMs}
    RetentionBytes: ${dbTaskRetentionBytes}
  Database:
    Hosts: "$($authoritySql.Host)"
    User: "${mysqlUser}"
    Passwd: "${mysqlPassword}"
    MaxOpenConn: ${dbMaxOpenConn}
    MaxIdleConn: ${dbMaxIdleConn}
    Net: ""
    # 实际后端验证与服务 yaml 同源;true 时使用 TiDB 当前严格读,连到 MySQL 会拒启。
    RequireTiDB: $(if ($authoritySql.RequireTiDB -eq 'true') { 'true' } else { $dbRequireTiDB })
    # 启动期 DDL(见生成器注释):dev = go/db/etc/db.yaml 的值;staging/prod = false。
    AutoCreateDatabase: ${dbAutoCreateDatabase}
    AutoMigrateSchema: ${dbAutoMigrateSchema}
    # 库名白名单(启动期硬断言)。刻意不写 AllowlistEnforcement:留空即 strict,
    # 也就是生产语义 —— 白名单一旦解析为空就拒启,不允许静默放行。
    AllowedDatabases:
${dbAllowedDatabasesYaml}
  RedisClient:
    Hosts: "redis.${InfraNamespace}:6379"
    DefaultTTLSeconds: 3600
    Password: "${redisPassword}"
    # 这个 RedisClient 同时是按落点选库读 player:placement / player:zone 的句柄(player-storage-placement.md §6.2):
    # 顶层 Placement 只写连接池,Redis 不写 = Placement.Redis 取缺省(复用本段)。
    # 所以它必须与 data-service ConfigMap 的 MappingRedis 同实例、DB 0 —— 读错库 = 读不到任何记录与 home_zone,
    # 全员按本 zone 选库,被钉到别处的玩家会写错库。契约测试钉住两者同址。
    DB: 0
    # 连接池上限(镜像 go/db/etc/db.yaml,Get-RedisPoolMirror;计入 Redis 连接总账)。
    PoolSize: ${dbRedisPoolSize}
    # 重试 / 死信 / 毒消息入队的落盘确认 WAITAOF(docs/design/db-retry-owner-routing.md §7):
    # Kafka 位点提交之前入队的副本是任务唯一的副本,确认不足就不提交位点(fail-closed,消息重投);开启时启动先探一次,失败拒启。
    # 确认走每实例 2 条的专用连接(不占上面的 PoolSize),Redis 连接总账的 db 行按开启计。两项都写 0 = 关闭确认,只给确实没开 AOF 的环境。
    # NumLocal / TimeoutMs 镜像 go/db/etc/db.yaml;NumReplicas 按共享 Redis 的形态(-RedisReplicas:单实例 0、高可用 1,与 data-service 同源)。
    # 只改 ConfigMap 不会滚动 db:改值(含单实例切高可用之后)必须 rollout restart deploy/db。
    WaitAOFNumLocal: ${dbWaitAOFNumLocal}
    WaitAOFNumReplicas: ${dbWaitAOFNumReplicas}
    WaitAOFTimeoutMs: ${dbWaitAOFTimeoutMs}
  # MySQL 行内围栏开关 legacy | dual(镜像 go/db/etc/db.yaml,docs/design/db-write-fencing-mysql.md §8)。
  # 本脚本没有逐 zone 覆盖:切 dual 的门禁与逐 zone 做法见该设计 §8.3。只改 ConfigMap 不会滚动 db,
  # 改值后必须 rollout restart deploy/db,以每个 Pod 的启动日志 "player row fencing: mode=…" 核对。
  Fencing:
    Mode: "${dbFencingMode}"
    # 仅全 writer 已升级后才能打开;本脚本逐字镜像,不能替代滚动门禁。
    EpochOverride: ${dbFencingEpochOverride}
# 与 manifests/go-svc/db.yaml 的 metrics 容器端口 / prometheus.io/port 注解一致(9160 = db,与本地 go/db/etc/db.yaml 同值)。
# 不写 = 缺省空串 = 不起 /metrics 端口:manifest 已声明 9160 并打了抓取注解,不写的话抓取恒失败,
# 连接池指标 mmorpg_sql_pool_* 与 owner-epoch-alerts.yaml 的抓取前提在集群里都不成立。
MetricsListenAddr: ":9160"
# 额外落点库连接池(镜像 go/db/etc/db.yaml;计入 MySQL 连接总账 Get-MysqlConnectionLedger)。每个额外落点库一个池,
# 按库所属家族分两个旋钮:ZoneStore* = zone 家族库(冷缓存访客读、合服 pin 后的源区库),NonZoneStore* = 非 zone 落点库(Phase 2);
# ExtraStoreConnMaxIdleSeconds = 这些池里空闲连接的回收秒数。本 zone 库的池仍是上面的 Database.MaxOpenConn / MaxIdleConn。
# 只写连接池这几项:AllowStoreFamilies / Required / Redis 仍不写 = 取缺省(true / false / 复用上面的 RedisClient),
# 语义与以前整段不写相同(go/db config.go PlacementConfig 的缺省规则)。
Placement:
  Required: ${dbPlacementRequired}
  ZoneStoreMaxOpenConn: ${dbZoneStoreMaxOpenConn}
  ZoneStoreMaxIdleConn: ${dbZoneStoreMaxIdleConn}
  NonZoneStoreMaxOpenConn: ${dbNonZoneStoreMaxOpenConn}
  NonZoneStoreMaxIdleConn: ${dbNonZoneStoreMaxIdleConn}
  ExtraStoreConnMaxIdleSeconds: ${dbExtraStoreConnMaxIdleSeconds}
  # D1 第 3 批读方门禁(k8s_deploy.ps1 -DbPlacementRequireRouteReady,缺省 false;docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §5.3 / §13.2):
  # true = mapping Redis 的副本被判可信才按 player:placement / player:zone 选库,不可信时进程级就绪闸暂停取 Kafka 消息(存盘积压在 Kafka,不丢)。
  # 逐 zone 打开(规格 §8 P5),前提是全服 Arm=true、标记已挂、leader 自检稳定为 trusted;回退先关它(先读方、后写方)。
  # RouteReadyGateIntervalMs 不写 = 代码缺省 1000。只改 ConfigMap 不会滚动 db:改值后 rollout restart deploy/db。
  RequireRouteReady: ${dbRouteReadyText}
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
# MappingRedis 刻意不写 DB 键:go-zero 的 redis.RedisConf 没有 DB 字段,以前这里的 `DB: 15` 被静默忽略,
# 映射一直落在 DB 0(与 go/data_service/etc/data_service.yaml 的注释同一结论)。留着它只会误导运维按 15 去查 /
# 给 merge_zone 填 -mapping-redis-db 15,让合服围栏与改映射打进 data_service 从不读的库。
# player:zone / player:placement / merge:in_progress / db:capability:zone 都在这个实例的 DB 0;
# go/db 的 Placement.Redis 缺省复用它自己的 RedisClient(同一实例、DB 0),所以 go-svc-db ConfigMap 不写 Placement.Redis。
MappingRedis:
  Host: redis.${InfraNamespace}:6379
  Type: node
  Pass: "${redisPassword}"
Regions:
  - Id: 1
    Zones: [1]
    Redis:
      Addrs:
        - redis.${InfraNamespace}:6379
      Password: "${redisPassword}"
DevRedis:
  Host: redis.${InfraNamespace}:6379
  Type: node
  Password: "${redisPassword}"
  DB: 0
  # 连接池上限(镜像 go/data_service/etc/data_service.yaml,Get-RedisPoolMirror;计入 Redis 连接总账)。
  # 上面的 MappingRedis 是 go-zero 封装,没有池参数,总账按 go-redis 缺省 10 × GOMAXPROCS 记它。
  PoolSize: ${dsDevRedisPoolSize}
PlayerLockTTLSec: 3
# C++ 约定的 etcd 注册(DataServiceNodeService.rpc/zone/<ZoneId>/node_type/26/node_id/<N> + 同前缀的
# allocated 占位键,挂同一把租约):scene 靠它发现 data_service 并调 AllocateIdSegment 领 GuidSegment 号段,
# go-zero 自己的 dataservice.rpc 键 C++ 看不见。注册在 gRPC 端口 accept 之后,失败即 panic(CrashLoopBackOff 可见)。
# 对外 IP 取 Deployment 注入的 POD_IP(manifests/go-svc/data-service.yaml)。
# ZoneId:DataServiceNodeService 不是 zone-scoped 节点类型,C++ 发现侧不按 zone 过滤,任何 zone 的 scene 都能
# 发现任何 zone 的 data_service;写部署 zone 只是排障时对得上号。LeaseTTL 从服务 yaml 取(见生成器注释)。
ZoneId: ${CurrentZoneId}
LeaseTTL: ${dsLeaseTTL}
# 全局库(node-id-overhaul-plan §2.0c / §6.2):transaction_log / player_snapshot / rollback_audit_log /
# id_segment / player_name 与映射持久化的路由表(player_route 等,第 1a 批)都在这里,由 data_service 迁移建表
# (proto 驱动 + 手写 DDL,清单以 go/data_service/internal/store/schema.go 为准)。库名 ${GlobalDbName} 由 infra-up 的
# mysql-init-sql(02_k8s_global_db.sql)预建并 GRANT 给 appuser;账号沿用 db 服务同一组注入值
# (MMORPG_MYSQL_USER / MMORPG_MYSQL_PASSWORD,dev 回落 root),换成别的账号要自己补 GRANT。
# 以前这份 ConfigMap 没有本段,Go 默认值是 127.0.0.1:3306/testdb —— K8s 上必然连不上:
# 三个 store 置 nil、AllocateIdSegment 不可用,login 开了 IdSegment 后建角会全部失败。
SnapshotMySQL:
  RequireTiDB: $($authoritySql.RequireTiDB)
  Host: "$($authoritySql.Host)"
  User: "${mysqlUser}"
  Password: "${mysqlPassword}"
  DBName: "${GlobalDbName}"
  MaxOpenConn: ${dsMysqlMaxOpenConn}
  MaxIdleConn: ${dsMysqlMaxIdleConn}
# 名字注册表的独立连接池(镜像 go/data_service/etc/data_service.yaml;计入 MySQL 连接总账 Get-MysqlConnectionLedger)。
# 只写池大小两项:其余键不写 = 零值,由 PlayerNameConfig.Normalize 补默认(go/data_service/internal/config/config.go)。
# 以前整段不写,池大小靠 Go 默认值恰好等于服务 yaml;现在集群值与总账输入同源。
# 副作用:这里的值非法(非正整数 / MaxIdleConn > MaxOpenConn)时名字 store 不装配,下面 Store.Required: true 会让服务拒启 ——
# 所以生成器取值时先核一遍(Get-MysqlPoolMirror),不合格在 apply 之前失败,而不是进集群 CrashLoopBackOff。
PlayerName:
  MaxOpenConn: ${dsPlayerNameMaxOpenConn}
  MaxIdleConn: ${dsPlayerNameMaxIdleConn}
# 建表策略(见生成器注释):dev = 服务 yaml 的值;staging/prod = false,由 data-service-migrate Job 在 Deployment 之前跑 -migrate。
Schema:
  AutoMigrate: ${dataServiceAutoMigrate}
# 多副本的前提(manifests/go-svc/data-service.yaml 头部):MySQL 侧的 store 装配不起来就拒启,不带病进发现池。
# 所有档位及正式本地 yaml 都写 true；缺池一律拒启。
Store:
  Required: true
${dsAccountDirectoryBlock}
# 映射提交点(player-route-mapping-durability-proposal.md §14):正式默认 db_first,全部 zone 同一取值。
# 存量环境先停旧写者并正规导入/audit,新空环境先迁移目录与全部白名单玩家库；未证明历史完整不能 Arm。
# 部署前脚本读线上现状做粘滞核对,
# 不一致要显式 -ConfirmRouteSwitchChange。K8s 上跑 route-tool 用 tools/scripts/k8s_route_tool.ps1(一次性 Job,不 exec 进本 Pod)。
# 连接池大小两项(G2b)镜像服务 yaml:route store 的独立池只在 db_first 下装配,redis 模式下不开连接;MySQL 连接总账的规划档
# 已按 db_first 计入它(Get-MysqlConnectionPlan)。
# 副作用:以前这里只写 Mode、池大小取代码默认;镜像之后,服务 yaml 里一个非法的池值(如 MaxIdleConn > MaxOpenConn)会让
# K8s 上的 data-service 在 redis 模式下也拒启(data_service.go 不分 Mode 校验 RouteStore)—— 所以生成器取值时先核
# 「正整数且 idle ≤ open」(Get-MysqlPoolMirror),不合格在 apply 之前失败。下面几项同理先核(Get-RouteStoreDeploySettings)。
# FenceShards:从正式服务 yaml 镜像当前 64 分片,全球库必须是显式强读 TiDB。
# WaitAOF:本地落盘 1(两份 Redis 清单都开着 AOF);副本份数按 mapping Redis 拓扑(-RedisReplicas:单实例 redis.yaml 写 0,高可用 redis-ha.yaml 写 1);超时取服务 yaml。
# ReadRequiresReady 与 Reconcile.ArmReadyMarker 正式默认 true,全部副本同一取值。
# 未完成 reconcile/ready 时读方闭闸。Reconcile 段其余键不写 = 代码缺省(Enabled true、LeaseMs 10000、
# RedisPoolSize 4 …;leader 的独立 go-redis 池按这个缺省计入 Redis 连接总账)。
RouteStore:
  Mode: $($dsRouteSettings.Mode)
  FenceShards: $($dsRouteSettings.FenceShards)
  MaxOpenConn: ${dsRouteStoreMaxOpenConn}
  MaxIdleConn: ${dsRouteStoreMaxIdleConn}
  WaitAOFNumLocal: $($dsRouteSettings.WaitAOFNumLocal)
  WaitAOFNumReplicas: $($dsRouteSettings.WaitAOFNumReplicas)
  WaitAOFTimeoutMs: $($dsRouteSettings.WaitAOFTimeoutMs)
  ReadRequiresReady: $($dsRouteSettings.ReadRequiresReady)
  Reconcile:
    ArmReadyMarker: $($dsRouteSettings.ArmReadyMarker)
# 与 manifest 的 9260 端口、prometheus 注解一致。多副本之后要能按 Pod 看消费者是否在跑。
MetricsListenAddr: ":9260"
# 回档的帮会资产闸(07-rollback-fail-closed.md §7.5.5):回档写任何玩家数据之前先问 guild「这些玩家自快照以来有没有已应用的帮会资产操作」。
# 不写这一段 = 回档一律被拒。guild 是全局池,按 Service 名直连(guild 刻意不写 go-zero Etcd.Key,按 etcd 找不到它;§7.11-U10)。
# NonBlock:guild 不在线不得阻塞 data-service 启动;Breaker 关:本链路不重试、一次失败即整体拒绝,熔断器帮不上忙。
GuildInternalRpc:
  Endpoints:
    - "guild.${InfraNamespace}:50300"
  Timeout: ${dsGuildInternalTimeout}
  NonBlock: true
  Middlewares:
    Breaker: false
GuildClockSkewMarginMs: ${dsGuildClockSkewMarginMs}
GuildCheckBudgetSeconds: ${dsGuildCheckBudgetSeconds}
# 三个 Rollback* 的服务端超时,从服务 yaml 原样搬运(取值推导见那份文件)。部署门禁逐条核对:这三个方法 C++ 从不调用
# (Get-GrpcClientMethodsNotCalledFromCpp),所以可以高于 C++ 的 deadline。
${dsMethodTimeoutsBlock}
# id_segment 行播种策略(见生成器注释):dev = 服务 yaml 的值;staging/prod = false,缺行不自动播种,
# 由人核对消费侧最大号后处理(README「恢复全局库前须先核对 id_segment.max_id」)。
# BootstrapTags 与服务 yaml / DefaultIdSegmentBootstrapTags 同一份清单:迁移(AutoMigrate / -migrate)
# 用 INSERT IGNORE 预建这些 biz_tag 行,幂等、绝不降低已有行的 max_id;新增永久身份两边同加。
IdSegment:
  AllowAutoSeed: ${dataServiceAllowAutoSeed}
  BootstrapTags: [player, guild, item, txlog, snapshot, trade_listing, guild_asset_op, friend_request, trade_asset_op, mail, mail_asset_op, rank_board, rank_settlement, rank_reward]
# C++ scene 产出的交易流水 / 玩家快照落库消费者。topic 名与分区数是不可变契约,值来自服务 yaml。
# Kafka 不可达不影响 Load/Save,只记日志并每 30s 后台重试。
# TopicGeneration 是分区契约的代号,进有效 topic 名(<基名>_g<N>);从服务 yaml 镜像过来,当前集群
# 消费第几代 topic 在这里一眼可见。改分区数 = 这个数 +1(换一批新 topic),绝不原地扩分区;
# infra-up 的 kafka-topic-init Job 按同一个值预建 topic(README「Kafka 审计 topic 预建」)。
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
  TopicGeneration: ${dsKafkaTopicGeneration}
  TransactionLogTopic: ${dsKafkaTxTopic}
  TransactionLogPartitions: ${dsKafkaTxPartitions}
  TransactionLogConsumerGroup: ${dsKafkaTxConsumerGroup}
  SnapshotTopic: ${dsKafkaSnapshotTopic}
  SnapshotPartitions: ${dsKafkaSnapshotPartitions}
  SnapshotConsumerGroup: ${dsKafkaSnapshotConsumerGroup}
  RetentionMs: ${dsKafkaRetentionMs}
  # player_snapshot 消费者的批量 / 并发旋钮与 lag 探针(G2a):值来自服务 yaml(默认 = 逐条串行 + lag 探针 30s)。
  SnapshotBatchMaxRows: ${dsKafkaSnapshotBatchMaxRows}
  SnapshotBatchMaxBytes: ${dsKafkaSnapshotBatchMaxBytes}
  SnapshotFlushIntervalMs: ${dsKafkaSnapshotFlushIntervalMs}
  SnapshotMaxInflightFlushes: ${dsKafkaSnapshotMaxInflightFlushes}
  LagProbeIntervalMs: ${dsKafkaLagProbeIntervalMs}
"@
		}
		"login" {
@"
Name: login.rpc
ListenOn: 0.0.0.0:50000
Timeout: ${loginTimeout}
${loginModeLine}
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: login.rpc
# ClusterId:部署级常量(-ClusterId,默认 0),snowflake worker 段的高位 cluster 号,
# 与本集群其它 ConfigMap(C++ 节点 / scene-manager / match)必须同值。顶层键。
# Go 侧字段(node-id-overhaul-plan §5 Phase 3)落地前 go-zero 忽略未知键,先写不伤。
ClusterId: ${CurrentClusterId}
# PlayerId 槽位本地缓存目录(生成器顶部 $SnowflakeCacheDir):manifests/go-svc/login.yaml 在同一
# 路径挂 emptyDir。Go 默认的 ../../run/snowflake 在容器里落到 /run/snowflake,不该赌它可写。
SnowflakeCacheDir: ${SnowflakeCacheDir}
# 混版本持久字段保护 C4 的模式(warn = 照常进游戏并计数,enforce = 判定截断时拒绝进游戏),
# 从 go/login/etc/login.yaml 镜像,生成期已校验只能是两个字面量之一。
PersistSchemaRegressionMode: ${loginPersistSchemaRegressionMode}
Node:
  ZoneId: ${CurrentZoneId}
  SessionExpireMin: ${loginSessionExpireMin}
  MaxLoginDevices: ${loginMaxLoginDevices}
  LeaseTTL: ${loginNodeLeaseTTL}
  QueueShardCount: ${loginQueueShardCount}
  MaxLoginDuration: 5m
  LogoutGraceTime: 5s
  RedisClient:
    Host: ${loginCoreRedisHost}
    Password: "${redisPassword}"
    DB: 0
    DefaultTTL: ${loginRedisDefaultTTL}
    DialTimeout: ${loginRedisTimeout}
    ReadTimeout: ${loginRedisTimeout}
    WriteTimeout: ${loginRedisTimeout}
    # 连接池上限(镜像 go/login/etc/login.yaml,Get-RedisPoolMirror;计入 Redis 连接总账)。
    PoolSize: ${loginRedisPoolSize}${loginQueueRedisBlock}
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
  Timeout: ${loginSceneManagerRpcTimeout}
  Middlewares:
    Breaker: false
# DataService gRPC 客户端:SQL 账号目录(ReadAccountDirectory / CreateAccountRole / LookupAccountRole …)、PlayerId 号段
# (AllocateIdSegment)、归属登记与查询(RegisterPlayerZone / GetPlayerHomeZone / BatchGetPlayerHomeZone)、角色名登记与回源
# 共用这一条连接。
# 2026-10-22 更正:账号目录改走 data-service 之后,它是 Login 的**硬依赖** —— 每次 Login(HTTP 与 gate 各一次)先读账号目录,
# 读失败即登录失败;建角、EnterGame 的目录读写同样 fail-closed。只有角色列表上的归属 / 名字回填是 fail-open 的展示数据
# (查不到按建角 zone、空名字原样返回)。原注释「data-service 是弱依赖、熔断后按保留建角 zone 放行」已不成立。
# NonBlock: true —— 只让 login 起服时不被 data-service 的拨号卡住;data-service 不可用期间登录照样失败,不是降级放行。
# Breaker: true —— 与 go/login/etc/login.yaml 一致:data-service 僵住但 etcd 键还在时,让调用快速失败(登录 / 建角直接回故障码,
# 角色列表的回填按建角 zone 返回),而不是每次都白等满各自的预算。
DataServiceRpc:
  Etcd:
    Hosts:
      - "etcd.${InfraNamespace}:2379"
    Key: dataservice.rpc
  Timeout: 3000
  NonBlock: true
  Middlewares:
    Breaker: true
# PlayerId 号段发号(node-id-overhaul-plan §6)。
# Enabled 必须**显式写 true**:go-zero 对缺席的 optional 块不填 default,shared/idsegment 的规则是
#   「整块不写 = 关 = 纯 snowflake 老路径」;漏写这一行等于静默回到旧发号,不会报错。
# Step 从 go/login/etc/login.yaml 取(按「10 分钟峰值建角量」配,双 buffer = 库倒下后还能发完两段)。
# FallbackToSnowflake 固定 false 是 §6.4 的决定:回退虽安全(号段 [1, 2^55) 与存量 snowflake 值域
#   不相交)但会让 snowflake 机器永远留着;号段失败即建角失败,靠 data-service 的可用性兜底,不靠回退。
IdSegment:
  Enabled: true
  Step: ${loginIdSegmentStep}
  FallbackToSnowflake: false
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
    Value: "${internalAuthSecret}"${loginQueueTokenSecretBlock}
# 下发 gate 地址时是否必须用 gate 自报的客户端可达地址(NodeInfo.client_endpoint,D78):
# true = 缺地址的 gate 不下发;false = 回落 endpoint(PodIP)。取值见 k8s_deploy.ps1 -RequireClientEndpoint(默认跟随 -ClientEntryMode)。
RequireClientEndpoint: ${requireClientEndpointValue}${loginDevPasswordAuthBlock}${loginQueueBlock}${loginAuthProvidersBlock}
${loginPlacementBlock}
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
  # 死配置:login 只生产不消费、代码不读(go/login 已把它改为 optional);它不是 db 的消费组,值不随 -DbConsumerGroup 切组变化,Step 7 删除(docs/design/db-consumer-group-per-zone.md §8)。
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
  # db_task topic 的保留期与每分区保留量:与 db 的 ConfigMap 同值(k8s_deploy.ps1 的 Apply-KafkaProfileDefaults / Get-KafkaDiskPlan)。
  RetentionMs: ${dbTaskRetentionMs}
  RetentionBytes: ${dbTaskRetentionBytes}
# Prometheus 抓取端点(go-zero 自带 agent:zrpc.MustNewServer → ServiceConf.SetUp → prometheus.StartAgent;Host 为空时直接 return,
# 端点根本不存在,go-zero metric 包记的样本 —— 内置 RPC 指标与 login 用它注册的自定义指标 —— 也被直接丢弃)。
# 以前 K8s 上不写本段,集群里一条 login 序列都抓不到。
# 9101 与 manifests/go-svc/login.yaml 的 metrics 容器端口及 prometheus.io/port 注解一致,三处必须同批发布(或先发本段):
# 只有注解、进程没在 9101 监听时,按注解抓取的目标是 down。登录链路对 data-service 的调用耗时与结果码
# (rpc_client_requests_duration_ms / rpc_client_requests_code_total)、gate 目录读来源、任务结果投递都只从这里导出
# (L-18 压测的 t<N>m_login.txt 快照)。契约测试:tools/scripts/tests/login_metrics_contract.tests.ps1。
Prometheus:
  Host: 0.0.0.0
  Port: 9101
  Path: /metrics
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
  # 连接池上限(镜像 go/player_locator/etc/player_locator.yaml,Get-RedisPoolMirror;计入 Redis 连接总账)。
  PoolSize: ${locatorRedisPoolSize}
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
  # Lease.ReconcileIntervalSeconds 与 Lease.Reconcile 刻意不写:会话对账是全服单 leader,各项事实源是
  # go/player_locator/internal/config/config.go 的默认值,必须全服一致(docs/design/player-locator-gate-session-index.md §10)。
# Prometheus 抓取端点(go-zero 自带 agent;Host 为空时 StartAgent 直接 return,端点根本不存在)。
# 9190 与 manifests/go-svc/player-locator.yaml 的 prometheus.io/port 注解及 metrics 容器端口一致,两者必须同批发布;
# 会话对账 leader 卡死 / 无 leader 只能靠这里的 reconcile 指标与 deploy/k8s/player-locator-alerts.yaml 发现。
Prometheus:
  Host: 0.0.0.0
  Port: 9190
  Path: /metrics
"@
		}
		"scene-manager" {
@"
Name: scenemanagerservice.rpc
ListenOn: 0.0.0.0:60000
# zrpc 服务端超时(毫秒),取自 scene_manager_service.yaml。以前不写,落到 go-zero 默认 2000ms,
# 任何超过 2s 的第一条腿都会让源 scene 冻满 30s 看门狗(cross-zone-scene-travel.md §12.2)。
Timeout: ${sceneManagerTimeout}
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: scenemanagerservice.rpc
Redis:
  Host: redis.${InfraNamespace}:6379
  Type: node
  Pass: "${redisPassword}"
  Key: scenemanagerservice
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
# Kafka 同步写超时(秒)。以前不写、落 Go 默认 5;显式镜像 yaml,让上面 Timeout 的不等式能在 ConfigMap 里核对。
KafkaWriteTimeoutSeconds: ${sceneManagerKafkaWriteTimeout}
# 铸造 owner_epoch 之后的落盘确认 WAITAOF(docs/design/zone-redis-regression-guard-20261022.md §4.3.2):铸造 EVAL 与 WAITAOF
# 走每副本 32 条上限的专用连接(Redis 连接总账 scene-manager 行 Fixed),确认之后才推路由;确认不足按推送失败回滚、回 7。
# NumLocal / TimeoutMs 镜像 scene_manager_service.yaml;NumReplicas 按共享 Redis 的形态(单实例 0、高可用 1,与 db、data-service 同源)。
# 开启时启动先探一次,失败拒启。只改 ConfigMap 不会滚动:改值(含单实例切高可用之后)必须 rollout restart deploy/scene-manager。
OwnerEpochMint:
  WaitAOFNumLocal: ${sceneManagerWaitAOFNumLocal}
  WaitAOFNumReplicas: ${sceneManagerWaitAOFNumReplicas}
  WaitAOFTimeoutMs: ${sceneManagerWaitAOFTimeoutMs}
# ZoneId / LeaseTTL 都在 Go config 顶层(config.go 默认 1 / 60)。以前这份模板没有 ZoneId,
# 于是每个 zone 的 scene-manager 都按 zone 1 注册到 SceneManagerNodeService.rpc/zone/1/,
# zone 102 的 scene 节点找不到自己的 SceneManager(node-id-overhaul-plan-20260908.md §2.0a)。
# 原来的 NodeID: "node-1" 已 deprecated、零读取,一并删掉。
ZoneId: ${CurrentZoneId}
LeaseTTL: ${sceneManagerLeaseTTL}
# ClusterId:部署级常量(-ClusterId,默认 0),snowflake worker 段的高位 cluster 号,
# 与本集群其它 ConfigMap 必须同值。Go 侧字段落地前 go-zero 忽略未知键。
ClusterId: ${CurrentClusterId}
# snowflake 槽位本地缓存目录(生成器顶部 $SnowflakeCacheDir):manifests/go-svc/scene-manager.yaml
# 在同一路径挂 emptyDir。Go 默认的 ../../run/snowflake 在容器里落到 /run/snowflake,不该赌它可写。
SnowflakeCacheDir: ${SnowflakeCacheDir}
# 选主 gauge(scene_manager_is_leader)与 EnterScene 分阶段指标都从这里出;
# 不开的话 scene-manager.yaml 注释里让运维盯的告警口径全部落空。
MetricsListenAddr: ":9150"
# 跨 zone 重定向签发 gate 令牌要用,缺了 gate_redirect.go 直接返回
# "GateTokenSecret not configured"。以前这份 ConfigMap 压根没有这一项。
GateTokenSecret: "${gateTokenSecret}"
# DataService gRPC 客户端:同一连接查home zone与原owner_revision(LookupAccountRole)，不新增SQL/Redis池，连接总账数不变。
# Lookup仅用scene-manager.yaml独立Secret account-directory-scene-manager，不给SM login/trade写密钥。
# EnterScene 用home zone随 RoutePlayerEvent
# 下发给 scene 决定存盘落哪个 zone 的库(cross-zone-scene-travel.md CZ-3)。K8s 是多 zone 形态,
# 缺这一块 scene_manager 会 fail-closed 拒绝所有进场景(AllowGateZoneAsHomeZone 默认 false);
# 而把开关打开则会让访客的存盘写进目标 zone 的库 —— 两种都不对,所以这里必须配。
# data-service 是全局池(不分 zone),Key 不带 zone 后缀,与 login 的同名块一致。
DataServiceRpc:
  Etcd:
    Hosts:
      - "etcd.${InfraNamespace}:2379"
    Key: dataservice.rpc
  Timeout: 3000
  NonBlock: true
  Middlewares:
    Breaker: false
HomeZoneLookupTimeoutMs: ${sceneManagerHomeZoneLookupTimeout}
# 跨 zone 重定向(RedirectToGate)下发 gate 地址时是否必须用 gate 自报的客户端可达地址(D78),与 login 同值:
# 取值见 k8s_deploy.ps1 -RequireClientEndpoint(默认跟随 -ClientEntryMode;逐个 zone 切 external 的窗口里显式传 false)。
RequireClientEndpoint: ${requireClientEndpointValue}
"@
		}
		"match" {
@"
Name: matchservice.rpc
Mode: ${matchMode}
GlobalLoginSessions: true
LoginRedis:
  Host: redis.${InfraNamespace}:6379
  DB: $(Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.RedisClient.DB')
  PasswordEnv: MMORPG_LOGIN_REDIS_PASSWORD
  PoolSize: $(Get-AuthoritativeScalar -RelativePath 'go/match/etc/match_service.yaml' -KeyPath 'LoginRedis.PoolSize')
${matchGlobalBlock}
${matchRatingBlock}
${matchMiddlewaresBlock}
ListenOn: 0.0.0.0:50500
Timeout: ${matchTimeout}
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: matchservice.rpc
# 双存储(cross-zone-matchmaking.md D2):
#   Redis      = SharedRedis,既有共享单库,只读跨运行时契约 key
#                (player:*:location / player:session:* / battle:lock:*,写者含 C++ scene)
#   MatchRedis = match 独占的 Redis Cluster,放 match:* / challenge:* / spectate:*
Redis:
  Host: redis.${InfraNamespace}:6379
  Type: node
  Pass: "${redisPassword}"
  Key: matchservice
MatchRedis:
  Host: ${matchRedisClusterHosts}
  Type: cluster
  Key: matchservice
# 全局池:ZoneId 只影响 etcd 注册路径,gate 按非 zone-scoped 前缀发现,任何 zone 的 gate 都能连到。
ZoneId: ${CurrentZoneId}
LeaseTTL: ${matchLeaseTTL}
# ClusterId:部署级常量(-ClusterId,默认 0)。全局池也必须与本集群的 zone 服务同值 ——
# cluster 段是「这个 K8s 集群」的号,不是 zone 的号;infra-up 与 zone-up 读的是同一个参数。
ClusterId: ${CurrentClusterId}
# snowflake 槽位本地缓存目录(生成器顶部 $SnowflakeCacheDir):manifests/go-svc/match.yaml 在同一
# 路径挂 emptyDir。Go 默认的 ../../run/snowflake 在容器里落到 /run/snowflake,不该赌它可写。
SnowflakeCacheDir: ${SnowflakeCacheDir}
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
KafkaWriteTimeoutSeconds: ${matchKafkaWriteTimeout}
MatcherIntervalMs: ${matchMatcherIntervalMs}
MatcherLockTTLSeconds: ${matchMatcherLockTTL}
BattleMaxDurationSeconds: ${matchBattleMaxDuration}
ChallengeTTLSeconds: ${matchChallengeTTL}
TicketTTLSeconds: ${matchTicketTTL}
ReadyTicketTTLSeconds: ${matchReadyTicketTTL}
${matchMatchedTicketTTLLine}
# PveTeamSizeByConfigId 整块逐字搬自 go/match/etc/match_service.yaml:新增 config id 只改服务 yaml。
# 组队整队开战也按它判副本人数上限,未配置的 id 回 TeamDungeonNotOpen。
${matchPveTeamSizeBlock}
# 组队(docs/design/team-system.md §D.1)。AllowCrossZone 切换要重启所有 match 实例(J-13a)。
# FillQueueDisabled 是队伍补位匹配的应急开关(docs/design/team-backfill-matching.md §4.6),同样要重启所有实例。
Team:
  AllowCrossZone: ${matchTeamAllowCrossZone}
  FillQueueDisabled: ${matchTeamFillQueueDisabled}
# data_service 客户端:team 用 BatchGetPlayerHomeZone 查玩家 home zone,调用失败 fail-closed。
# Etcd 指向集群 etcd,Key 与 data-service 自身注册一致(同 login 模板)。
DataServiceRpc:
  Etcd:
    Hosts:
      - "etcd.${InfraNamespace}:2379"
    Key: dataservice.rpc
  Timeout: ${matchDataServiceRpcTimeout}
  NonBlock: ${matchDataServiceRpcNonBlock}
  Middlewares:
    Breaker: false
# 排行榜评分快照(docs/design/leaderboard-system.md §2.4):RatingSnapshotPublishEnabled 来自 k8s_deploy.ps1 -MatchRatingSnapshotPublish
# (默认 "0" → false,是排行榜上线的唯一闸门),**不镜像** go/match/etc/match_service.yaml;其余三项从服务 yaml 取。
# RatingEnabled=true 时 topic 不论开关都在 match 启动时 Ensure;保留期只由 match 声明(rank 侧 Ensure 传 -1 不改写)。
# 分区数与 broker 不一致时 match 记错误日志并每 30s 后台重试、进程照常运行(开关为 1 时评分入账一起停住,不会带着错分区发快照)。
RatingSnapshotPublishEnabled: ${matchRatingSnapshotPublishEnabled}
RatingSnapshotTopic: ${matchRatingSnapshotTopic}
RatingSnapshotTopicPartitions: ${matchRatingSnapshotTopicPartitions}
RatingSnapshotRetentionHours: ${matchRatingSnapshotRetentionHours}
# 与 match.yaml 里的 metrics 容器端口一致
MetricsListenAddr: ":9170"
"@
		}
		"chat" {
@"
Name: ${chatName}
ListenOn: 0.0.0.0:50700
# 迁移/常驻实例分别从专用Secret key读取DSN；ConfigMap不含凭据。
DSNEnv: ${chatSqlDsnEnv}
RequireTiDB: true
MaxOpenConns: ${chatSqlMaxOpen}
AccountRoleSchema: ${GlobalDbName}
GlobalLoginSessions: true
LoginRedis:
  Host: redis.${InfraNamespace}:6379
  PasswordEnv: LOGIN_REDIS_PASSWORD
  DB: $(Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.RedisClient.DB')
  PoolSize: ${chatLoginPool}
# zrpc 服务端超时(毫秒),必须 <= 路由服 ForwardTimeoutMs - 1000,否则路由服先超时会把 chat 的正常慢响应判成故障。
Timeout: ${chatTimeout}
# go-zero Stat 拦截器默认按 INFO 打每个请求的整包 JSON:SendChat 请求体就是聊天正文(含私聊),
# 不能进 Pod 日志 / Loki(AGENTS.md §11.3 敏感信息最少暴露)。只屏蔽内容,不关 Stat —— 慢调用告警照常保留。
# 与 go/chat/etc/chat.yaml 同一段,两边必须一致。
Middlewares:
  StatConf:
    IgnoreContentMethods:
      - /chatpb.ClientPlayerChat/SendChat
# Etcd 段 **Key 显式留空**:chat 按 C++ 约定注册 ChatNodeService.rpc/zone/<z>/...(go/shared/noderegistry),
# 路由服按这个前缀发现它;没有任何 Go 调用方经 go-zero 发现键找 chat,填了 Key 只会多一条无人消费的注册,
# 本地 -Zone 派生还会给它加 .z<N> 后缀。等首个 Go 调用方出现再同批拍板(port-decisions D-13)。
# ⚠ 不能整行省略 Key:go-zero v1.10.0 的 discov.EtcdConf.Key 不是 optional,Etcd 段存在而缺 Key 时
#   conf.MustLoad 直接 Fatal("Etcd.Key" is not set,core/mapping processNamedFieldWithoutValue),
#   Pod 会 CrashLoop。空串 → HasEtcd()=false → go-zero 不注册;chat 的 config.Validate 只拒非空 Key。
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: ""
# 仅保留RpcServerConf的兼容字段；正式SQL聊天不创建此句柄，不读旧ChatRedis历史。
Redis:
  Host: redis.${InfraNamespace}:6379
  Type: node
  Key: chatservice
# 全局池:ZoneId 只影响注册路径,路由服对 ChatNodeService 不做 zone 过滤(ZoneScopedNodeTypes 默认仅 Login),
# 任何 zone 的玩家都会被路由到这里的实例;业务代码不读它。取命令行 -ZoneId(Apply-GlobalGoSvcManifests),
# 与 match 同口径;Go 侧 config.Validate 拒 0。
ZoneId: ${CurrentZoneId}
# 租约 TTL(秒)= 崩溃后路由服仍可能把请求打到死实例的最长窗口(PickRandom 不看连接状态)。
LeaseTTL: ${chatLeaseTTL}
# 正文按字节限长(gate 的 1024B 整包闸之后的第二道闸);超限或 trim 后为空回 kMessageSizeExceeded。
MaxContentBytes: ${chatMaxContentBytes}
# 每玩家每秒发送上限(gate 每会话每消息号 3 次/秒之后的第二道闸)。
RateLimitPerSecond: ${chatRateLimitPerSecond}
HistoryMaxEntries: ${chatHistoryMaxEntries}
HistoryTTLSeconds: ${chatHistoryTTLSeconds}
${chatOptionalLines}
# 与 manifests/go-svc/chat.yaml 的 metrics 容器端口 / prometheus.io/port 注解一致(9210 = chat)
MetricsListenAddr: ":9210"
# 留空 = shared/killswitch 的 DefaultPrefix(/mmorpg/killswitch/),与其它服务共用同一棵规则树。
KillSwitchPrefix: ""
"@
		}
		"client-rpc-router" {
@"
Name: ${routerName}
ListenOn: 0.0.0.0:50600
# zrpc 服务端整体超时(毫秒):必须 0 或 > ForwardTimeoutMs(路由服 config.Validate 强制),
# 否则转发链上游先于目标超时,把目标的正常慢响应误判成故障。
Timeout: ${routerTimeout}
# **必带**(契约 §1;路由服 config.Validate 缺了直接拒启动):go-zero Stat 拦截器默认按 INFO 打整包,
# ForwardRequest.body 一解码就是目标请求原文 —— 登录消息即明文账号密码,聊天即私聊正文。
Middlewares:
  StatConf:
    IgnoreContentMethods:
      - /client_rpc_router.ClientRpcRouter/Forward
# Etcd 段 Key 显式留空(与 chat 同理,port-decisions D-13):gate 按 ClientRpcRouterNodeService.rpc 前缀发现
# 路由服(C++ NodeInfo 约定),没有 Go 调用方经 go-zero 发现键找它;K8s 上填了 Key 只会多一条无人消费的注册。
# 不能整行省略 Key:go-zero v1.10.0 EtcdConf.Key 不是 optional,缺了 conf.MustLoad 直接 Fatal。
# 路由服的 etcd 客户端只用 Etcd.Hosts(internal/svc NewServiceContext),与 Key 无关。
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: ""
# 全局池:ZoneId 只影响路由服自己的注册路径,gate 按非 zone-scoped 前缀发现它;取命令行 -ZoneId,与 match / chat 同口径。
ZoneId: ${CurrentZoneId}
LeaseTTL: ${routerLeaseTTL}
# 单次向目标业务服务 Invoke 的超时(毫秒)。chat 等业务服务的 Timeout 必须 <= 它 - 1000(契约 §3 超时预算)。
ForwardTimeoutMs: ${routerForwardTimeoutMs}
# 只挑与发起 gate 同 zone 实例的目标节点类型(设计决策 D33;契约 §1 只允许 Login)。
# 新业务服务(chat 等)**不得**加进来:加了就只能被本 zone 的 gate 路由到,全局池语义被悄悄破坏。
ZoneScopedNodeTypes:
  - LoginNodeService
# 端口分工(契约 §7):9200 = 路由服;与 manifests/go-svc/client-rpc-router.yaml 的 metrics 容器端口 / 注解一致。
MetricsListenAddr: ":9200"
"@
		}
		"trade" {
@"
Name: ${tradeName}
ListenOn: 0.0.0.0:50800
# zrpc 服务端超时(毫秒),必须 <= 路由服 ForwardTimeoutMs - 1000(契约 §3),否则路由服先超时会把 trade 的正常慢响应判成故障。
Timeout: ${tradeTimeout}
# go-zero Mode:TradeAdmin.SeedListing 只在 dev/test 放行(方法内检查,否则 gRPC PermissionDenied)。
# dev 档 = go/trade/etc/trade.yaml 的值;staging/prod 固定 pro(见生成器注释)。显式写出,不赌 go-zero 缺省值。
Mode: ${tradeMode}
# Etcd 段 **Key 显式留空**(port-decisions D-13,与 chat 同理):trade 按 C++ 约定注册 TradeNodeService.rpc/zone/<z>/...
# (go/shared/noderegistry),路由服按这个前缀发现它;没有任何 Go 调用方经 go-zero 发现键找 trade。
# 不能整行省略 Key:go-zero v1.10.0 的 EtcdConf.Key 不是 optional,缺了 conf.MustLoad 直接 Fatal,Pod 会 CrashLoop。
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: ""
# 全局池:ZoneId 只影响注册路径,业务代码不读它做分支;取命令行 -ZoneId(Apply-GlobalGoSvcManifests),与 match / chat 同口径。
ZoneId: ${CurrentZoneId}
# 租约 TTL(秒)= 崩溃后路由服仍可能把请求打到死实例的最长窗口(PickRandom 不看连接状态)。
LeaseTTL: ${tradeLeaseTTL}
# 与 manifests/go-svc/trade.yaml 的 metrics 容器端口 / prometheus.io/port 注解一致(9230 = trade)
MetricsListenAddr: ":9230"
# 留空 = shared/killswitch 的 DefaultPrefix(/mmorpg/killswitch/),与其它服务共用同一棵规则树。
KillSwitchPrefix: ""
# 独占库(port-decisions D-14):库 ${tradeMysqlDBName} 由 infra-up 的 mysql-init-sql(原样带入 deploy/mysql-init/00_init_zone_dbs.sql)
# 预建并 GRANT 给 appuser;账号沿用 db / data-service 同一组注入值(MMORPG_MYSQL_USER / MMORPG_MYSQL_PASSWORD,dev 回落 root),
# 换成别的账号要自己补 GRANT。MaxOpenConn × Deployment 副本数(2)要算进 MySQL 的 max_connections(总账见 Get-MysqlConnectionLedger)。
MySQL:
  Host: "$($tradeSql.Host)"
  User: "${mysqlUser}"
  Password: "${mysqlPassword}"
  DBName: "${tradeMysqlDBName}"
  RequireTiDB: $($tradeSql.RequireTiDB)
  MaxOpenConn: ${tradeMysqlMaxOpenConn}
  MaxIdleConn: ${tradeMysqlMaxIdleConn}
# 建表策略(D-14 第 4 条,见生成器注释):dev = 服务 yaml 的值(启动期 schemamigrate.Up,GET_LOCK 保护多副本);
# staging/prod = false:表只由 trade-migrate Job 建,启动路径只跑只读 plan,不净即拒启并打印补救命令。
Schema:
  AutoMigrate: ${tradeAutoMigrate}
# 卖家 / 买家 home_zone 查询(BatchGetPlayerHomeZone,未映射 / 故障一律 fail-closed)与 listing_id 号段(AllocateIdSegment)。
# data-service 按 zone namespace 各部署一份,但 go-zero 发现键 dataservice.rpc 不分 zone、全局库只有一份,
# 所以 infra namespace 里的 trade 发现到哪个实例应答都一样。这是 trade 作为**调用方**的发现键,
# 不是 D-13 禁止的「全局服务自己的 go-zero 注册 Key」。
DataServiceRpc:
  Etcd:
    Hosts:
      - "etcd.${InfraNamespace}:2379"
    Key: dataservice.rpc
  Timeout: ${tradeDataServiceTimeout}
  NonBlock: true
  Middlewares:
    Breaker: false
# listing_id 号段(biz_tag = trade_listing,已进本脚本 data-service ConfigMap 的 IdSegment.BootstrapTags)。
# Enabled 必须**显式写 true**:go-zero 对缺席的 optional 块不填 default,go/trade 的 config.Validate 拒绝 false。
# FallbackToSnowflake 固定 false:trade 没有 snowflake 回退,号段失败即本次发号失败(SeedListing 回 kServiceUnavailable)。
# Step / MinStep / MaxStep 从服务 yaml 逐字镜像,缺席则整行不写。
IdSegment:
  Enabled: true
${tradeIdSegmentOptionalLines}
  FallbackToSnowflake: false
# 市场范围与分页 / 收藏上限,全部从服务 yaml 取。Scope ∈ {zone, global}:zone = 只看 / 只收藏本 home_zone 市场的商品。
Market:
  Scope: ${tradeMarketScope}
  DefaultPageSize: ${tradeMarketDefaultPageSize}
  MaxPageSize: ${tradeMarketMaxPageSize}
  MaxPage: ${tradeMarketMaxPage}
  MaxFavoritesPerPlayer: ${tradeMarketMaxFavorites}
# 共享单库 Redis:通用资产通道(guild-phase2/04-asset-channel.md §S4)按 player:{id}:location 定位
# 玩家所在 scene 节点。写者是 C++ scene / scene_manager,所以**必须**是这份共享实例的 DB 0,
# 不能指向 trade 自己的库,也不能指向 match 的独占集群(那里没有位置键)。
# 段名不能叫 Redis:zrpc.RpcServerConf 已有同名字段,加载期报 conflict key Redis(键名三处逐字一致:
# go/trade/etc/trade.yaml、go/trade/internal/config/config.go、本文件)。
# config.Validate 拒绝空 Host:缺它 trade 起不来,而不是"能浏览但一上架就永远卡住"。
SharedRedis:
  Host: "redis.${InfraNamespace}:6379"
  Password: "${redisPassword}"
  DB: 0
  # 连接池上限(镜像 go/trade/etc/trade.yaml,Get-RedisPoolMirror;计入 Redis 连接总账)。
  PoolSize: ${tradeRedisPoolSize}
# 通用资产通道开关(guild-phase2/04-asset-channel.md §S4)。键名与 go/trade/etc/trade.yaml、
# go/trade/internal/config/config.go 三处逐字一致。
# **所有档位固定 false**,不镜像服务 yaml —— 与 Mode / Schema.AutoMigrate 那条"dev 档取 yaml"
# 的纪律刻意不同,理由是上线前置还没做完(§4.43 第 29 项:两把资产密钥尚未经
# Resolve-InjectedSecret -MinLength 32 注入;scene gRPC 端口也还没有 NetworkPolicy 只放行
# scene_manager / match / guild / trade)。已知风险 K1(单写者只在 30s 重连租约内成立)同样
# 建议把"任何共享环境打开资产操作"列为 B4c 的硬前置。
# 打开之前要同时做三件事:注入密钥、补 NetworkPolicy、把这一行改成 true —— 缺一件就别改。
# 密钥值绝不写进 ConfigMap:trade 只从下面这个**环境变量名**读,值由部署侧注入;
# Enabled=true 而密钥缺失 / 不足 32 字节时 trade 直接拒启(fail-closed),不会静默降级。
${tradeAssetOpBlock}
${tradeCommerceBlock}
${tradeMallBlock}
"@
		}
		"friend" {
@"
Name: ${friendName}
ListenOn: 0.0.0.0:50400
# zrpc 服务端超时(毫秒),必须 <= 路由服 ForwardTimeoutMs - 1000(契约 §3)。反过来路由服先超时,会把 friend 的
# 正常慢响应判成信封级 kServiceUnavailable,而 friend 这边好友边已经落库 —— 客户端重试就是重复关系。
Timeout: ${friendTimeout}
# go-zero Mode:dev/test 才放开的调试入口(config.IsRelaxedMode,含 gRPC reflection)。
# dev 档 = go/friend/etc/friend.yaml 的值;staging/prod 固定 pro(见生成器注释)。显式写出,不赌 go-zero 缺省值。
Mode: ${friendMode}
RequireTiDB: $($friendSql.RequireTiDB)
AccountRoleSchema: ${GlobalDbName}
GlobalLoginSessions: true
LoginRedis:
  Host: redis.${InfraNamespace}:6379
  Password: "${redisPassword}"
  DB: $(Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'Node.RedisClient.DB')
  PoolSize: $(Get-AuthoritativeScalar -RelativePath 'go/friend/etc/friend.yaml' -KeyPath 'LoginRedis.PoolSize')
# Etcd 段 **Key 显式留空**(port-decisions D-13,与 chat / trade 同理):friend 按 C++ 约定注册
# FriendNodeService.rpc/zone/<z>/...(go/shared/noderegistry),路由服按这个前缀发现它;没有任何 Go 调用方
# 经 go-zero 发现键找 friend,填了 Key 只会多一条无人消费的注册(friend 的 config.Validate 直接拒非空 Key)。
# ⚠ 不能整行省略 Key:go-zero v1.10.0 的 discov.EtcdConf.Key 不是 optional,Etcd 段存在而缺 Key 时
#   conf.MustLoad 直接 Fatal("Etcd.Key" is not set),Pod 会 CrashLoop —— chat 在 kind 上实测踩过这个阻塞项。
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: ""
# 共享 Redis(既有单库;写者含 C++ scene / login,hiredis 没有集群客户端 ⇒ 这个库不许集群化)。两个用途:
#   1) FriendRedis 未配置时 friend 私有 key 的回落目标;
#   2) 跨运行时契约 key player:session:{id}(判好友在不在线)的**唯一**句柄。
# 这是 zrpc.RpcServerConf 自带的 Redis(RedisKeyConf)段:Key 仅在 Auth=true 时使用,但字段不是 optional,
# Redis 段存在时必须**显式写空串**,否则加载期报 "Redis.Key" is not set,同样 CrashLoop(与上面 Etcd.Key 同一个坑)。
# 不写 DB(契约 §4):契约 key 由多个运行时共写,DB 号必须全仓一致 = 默认 0。
# 密码:自 2df78d4a5(2026-09-19)起共享 Redis 的密码走可选 Secret redis-auth —— 注入的密码非空时
#   infra/redis.yaml 才加 --requirepass,这里与 chat / match / scene-manager 等共享库段**同批**写 Pass;
#   dev 档密码为空时两边都不设密码。改密码或开关 requirepass 时这些段必须同拍:只改服务端,客户端全线 NOAUTH;
#   只给客户端配密码,向无密码的 Redis 发 AUTH 会被直接拒("ERR Client sent AUTH, but no password is set")。
Redis:
  Host: redis.${InfraNamespace}:6379
  Type: node
  Pass: "${redisPassword}"
  Key: ""
# FriendRedis(friend 私有 key:好友列表缓存 / 申请配额 / 限流)在 K8s 上**刻意整段不配**,回落到上面的共享库。
# F2 的缓存键已带 hash tag、全是单 key 操作,Redis Cluster 安全,所以将来换独立实例不需要改代码;
# ⚠ 但 staging/prod 若要配独立实例,必须 maxmemory-policy=noeviction:好友列表缓存被淘汰只是多打一次 MySQL,
#   **配额 / 限流 key 被淘汰等于限流静默失效**。回落时这些 key 在共享库上:共享库是 noeviction 且不设 maxmemory
#   (infra/redis.yaml 与 redis-ha.yaml 同一取舍;本地 compose 自 2026-10-12 起也是 noeviction),今天不会被淘汰,
#   但会和契约 key 抢内存;共享库将来设了上限、写满即拒写(连带契约 key),所以回落只是过渡。
#   (2026-10-19 更正:原文称共享库是 LFU 全键淘汰型,K8s 清单从未用过淘汰型策略,那是本地 compose 2026-10-12 之前的配置。)
# 全局池:ZoneId 只影响 etcd 注册路径,路由服对 FriendNodeService 不做 zone 过滤(ZoneScopedNodeTypes 默认仅 Login),
# 任何 zone 的玩家都被路由到这里的实例;业务代码**禁止**读它做分支 —— 好友关系天然跨 zone,按 zone 过滤会把
# 跨区好友判成"不存在"。取命令行 -ZoneId(Apply-GlobalGoSvcManifests),与 chat / trade 同口径;Go 侧 Validate 拒 0。
ZoneId: ${CurrentZoneId}
# 租约 TTL(秒)= 崩溃后路由服仍可能把请求打到死实例的最长窗口(PickRandom 不看连接状态)。
LeaseTTL: ${friendLeaseTTL}
# 与 manifests/go-svc/friend.yaml 的 metrics 容器端口 / prometheus.io/port 注解一致(9180 = friend,契约 §7)
MetricsListenAddr: ":9180"
# 留空 = shared/killswitch 的 DefaultPrefix(/mmorpg/killswitch/),与其它服务共用同一棵规则树。
KillSwitchPrefix: ""
# 独占逻辑库(port-decisions D-14):开发 MySQL 由 infra-up 预建；TiDB 入口的同名库与权限必须事先准备。
# staging/prod 必须显式指定同一个 -FriendTiDBHost；启动验证真实 TiDB，不能按 zone 写独立库。
# 账号沿用注入值(MMORPG_MYSQL_USER / MMORPG_MYSQL_PASSWORD,dev 回落 root),
# 换成别的账号要自己补 GRANT。MaxOpenConn × Deployment 副本数(2)要算进 MySQL 的 max_connections(总账见 Get-MysqlConnectionLedger)。
MySQL:
  Host: "$($friendSql.Host)"
  User: "${mysqlUser}"
  Password: "${mysqlPassword}"
  DBName: "${friendMysqlDBName}"
  MaxOpenConn: ${friendMysqlMaxOpenConn}
  MaxIdleConn: ${friendMysqlMaxIdleConn}
# 建表策略(D-14 第 4 条,见生成器注释):dev = 服务 yaml 的值(启动期 schemamigrate.Up,GET_LOCK 保护多副本);
# staging/prod = false:表只由 friend-migrate Job 建,启动路径只跑只读 plan,不净即拒启并打印补救命令。
Schema:
  AutoMigrate: ${friendAutoMigrate}
# S2C 推送(kafkautil.PushToPlayer → gate-cmd_g<N>)的 broker 地址。推送是 at-most-once(契约 §5),
# 只作"去拉"的触发:丢了对端下次拉列表照样看到,所以 Brokers 为空也是合法配置(降级为不推)。
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
# 好友业务阈值,全部取自服务 yaml,不在这里另写一份常数。它们是**硬上限与限流**不是调优旋钮:
# 写大了单个玩家的好友 / 黑名单表无界增长(一次列表读要回几万行),写小了玩家加不上好友。
# 一项都不能漏:go-zero 对"整段 Friend 缺失"会按空 map 下钻并回填一整套 default,不报错也不全变 0。
Friend:
  MaxFriends: ${friendMaxFriends}
  MaxPendingRequests: ${friendMaxPendingRequests}
  MaxIncomingRequests: ${friendMaxIncomingRequests}
  MaxBlocks: ${friendMaxBlocks}
  RecommendDefaultLimit: ${friendRecommendDefaultLimit}
  RecommendMaxLimit: ${friendRecommendMaxLimit}
  RecommendMaxExclude: ${friendRecommendMaxExclude}
  RequestQuotaPerMinute: ${friendRequestQuotaPerMinute}
  ListReadHardLimit: ${friendListReadHardLimit}
  CacheTTL: ${friendCacheTTL}
  # 两类后台清理共用这一段参数(不另设配置键):终态好友申请行,以及零好友且超过保留期的 friend_capacity 行。
  # 默认 report_only:只出指标 friend_sweep_pending_rows{mode} / friend_sweep_idle_capacity_rows{mode} 并打 WARN,
  # **不删任何数据**;清的是权威数据,新环境先观察两个"待清理行数"合理再改 delete。四个键必须全写:Sweep 段标了 optional,
  # go-zero 不下钻整段缺失的 optional 嵌套结构、default 不回填,漏写会让 Mode 变空串并被 Validate 拒。
  Sweep:
    Mode: ${friendSweepMode}
    Interval: ${friendSweepInterval}
    RetentionDays: ${friendSweepRetentionDays}
    BatchLimit: ${friendSweepBatchLimit}
# 刻意**不写** Middlewares.StatConf.IgnoreContentMethods(与 go/friend/etc/friend.yaml 同一判断,不是漏了):
# chat 必须屏蔽 SendChat 是因为那个请求体就是聊天正文(含私聊);friend 的请求体只有 target_player_id / limit /
# exclude 这类数字 id,没有隐私正文,全量打日志反而是排障资产(能看出刷子的请求序列)。
"@
		}
		"player-inbox" {
@"
Name: ${playerInboxName}
ListenOn: 0.0.0.0:50950
# zrpc 服务端超时(毫秒)= 服务 yaml 的值。C++ scene 的 GrpcClient.CallDeadlineMs.PlayerInboxNodeService 必须 ≥ 本值 + 2000,
# 由写路径入口的 Assert-GrpcClientDeadlineBudget 核对(docs/design/player-inbox-service.md §1.4)。
Timeout: ${playerInboxTimeout}
# go-zero Mode:dev 档 = go/player_inbox/etc/player_inbox.yaml 的值;staging/prod 固定 pro。显式写出,不赌 go-zero 缺省值。
Mode: ${playerInboxMode}
# 服务端中间件(评审 L12):Stat 拦截器缺省按 INFO 打每个非慢调用的整包请求体。Fetch / Ack 只屏蔽内容,方法名 / 耗时 / 慢调用照常记;
# PlayerInboxAdmin 的请求体是审计线索,不屏蔽。下面整段由 Get-AuthoritativeYamlBlock 从服务 yaml 原样复制:
# Go 侧 config.Validate 在 Stat 开着时要求两个方法都在列表里,缺了本服务与 player-inbox-migrate Job 都拒启。
${playerInboxMiddlewaresBlock}
# Etcd 段 **Key 显式留空**(port-decisions D-13,设计 §1.4「D-13 不触发」):player_inbox 按 C++ 约定注册
# PlayerInboxNodeService.rpc/zone/<z>/...(go/shared/noderegistry),scene 按这个前缀发现它;没有任何 Go 调用方
# 经 go-zero 发现键找它。不能整行省略 Key:go-zero 的 EtcdConf.Key 不是 optional,缺了 conf.MustLoad 直接 Fatal。
Etcd:
  Hosts:
    - "etcd.${InfraNamespace}:2379"
  Key: ""
# 全局池:ZoneId 只影响 etcd 注册路径,业务代码不读它做分支;取命令行 -ZoneId(Apply-GlobalGoSvcManifests),与 trade / friend 同口径。
# C++ 侧 PlayerInboxNodeService 不在 IsZoneScopedNodeType 里,scene 连全部实例,所以填哪个 zone 都不影响发现。
ZoneId: ${CurrentZoneId}
# 租约 TTL(秒)= 崩溃后 scene 仍可能把 Fetch / Ack 打到死实例的最长窗口;失败只退避,不影响进场(IB-7)。
LeaseTTL: ${playerInboxLeaseTTL}
# 与 manifests/go-svc/player-inbox.yaml 的 metrics 容器端口 / prometheus.io/port 注解一致(9270 = player-inbox,D14)。
# 告警规则见 deploy/k8s/player-inbox-alerts.yaml(扫描型 gauge 只由领导者产出,规则一律 max() 聚合)。
MetricsListenAddr: ":9270"
# 留空 = shared/killswitch 的 DefaultPrefix(/mmorpg/killswitch/),与其它服务共用同一棵规则树。
KillSwitchPrefix: ""
# 独占库(D-14):库 ${playerInboxMysqlDBName} 由 infra-up 的 mysql-init-sql(原样带入 deploy/mysql-init/00_init_zone_dbs.sql)
# 预建并 GRANT 给 appuser;账号沿用 db / data-service / trade 同一组注入值(MMORPG_MYSQL_USER / MMORPG_MYSQL_PASSWORD)。
# 一个池(入箱、审计、Fetch / Ack 共用;会话级 RC + 锁等待 1s,设计 §10)。MaxOpenConn × Deployment 副本数(2)
# 要算进 MySQL 的 max_connections(总账见 Get-MysqlConnectionLedger)。
MySQL:
  Host: "mysql.${InfraNamespace}:3306"
  User: "${mysqlUser}"
  Password: "${mysqlPassword}"
  DBName: "${playerInboxMysqlDBName}"
  MaxOpenConn: ${playerInboxMysqlMaxOpenConn}
  MaxIdleConn: ${playerInboxMysqlMaxIdleConn}
# 建表策略(D-14 第 4 条):dev = 服务 yaml 的值(启动期 schemamigrate.Up,GET_LOCK 保护多副本);
# staging/prod = false:表只由 player-inbox-migrate Job 建,启动路径只跑只读 plan,不净即拒启并打印补救命令。
# TiDB 上的索引预切(PRE_SPLIT_REGIONS 只切行数据)不在 Job 里做,见 docs/ops/player-inbox-runbook.md「TiDB 索引预切」。
Schema:
  AutoMigrate: ${playerInboxAutoMigrate}
# 玩家存在校验(GetPlayerHomeZone,入箱前置)与对账读已落盘账本(GetPlayerInboxLedger)。
# data-service 按 zone namespace 各部署一份,但 go-zero 发现键 dataservice.rpc 不分 zone、全局映射只有一份,
# 发现到哪个实例应答都一样。这是 player_inbox 作为**调用方**的发现键,不是 D-13 禁止的「全局服务自己的注册 Key」。
DataServiceRpc:
  Etcd:
    Hosts:
      - "etcd.${InfraNamespace}:2379"
    Key: dataservice.rpc
  Timeout: ${playerInboxDataServiceTimeout}
  NonBlock: true
  Middlewares:
    Breaker: false
# 核心(共享单库)Redis,与 C++ scene / scene_manager 同一实例的 DB 0(只在事务外读,设计 §10):
#   player:{id}:owner_epoch —— 围栏上界 R(IB-8);player:{id}:location —— 在线提醒定位 scene 节点;
#   player_inbox:maintenance:leader —— 后台循环选主锁。
# 段名是 CoreRedis 而不是 Redis:zrpc.RpcServerConf 已有同名字段,加载期报 conflict key Redis。
# 指到别的实例或别的库 = 围栏上界永远读不到 / 提醒永远发不出;Go 侧 config.Validate 拒绝空 Host。
CoreRedis:
  Host: "redis.${InfraNamespace}:6379"
  Password: "${redisPassword}"
  DB: 0
  # 连接池上限(镜像 go/player_inbox/etc/player_inbox.yaml,Get-RedisPoolMirror;计入 Redis 连接总账)。
  PoolSize: ${playerInboxRedisPoolSize}
# 在线提醒(PlayerInboxNotifyEvent → scene-cmd,至多一次,丢了由落地拉取与在线兜底补,设计 §6.3)的 broker 地址。
# 命令 topic 的分区数 / 代号由 Apply-OneGoSvc 注入的 KAFKA_COMMAND_TOPIC_* 环境变量给出,与 C++ scene 同一份契约。
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
  WriteTimeoutMs: ${playerInboxKafkaWriteTimeoutMs}
# 收件箱业务参数(设计 §3.3:每种开关只有一个来源)。
#   AdminTokenEnv:运维口令所在的**环境变量名**。口令值只经 Secret player-inbox-admin-token 注入容器(Apply-OneGoSvc),
#                  绝不写进 ConfigMap;刻意**不写** AdminToken 键:变量缺失 / 为空 = PlayerInboxAdmin 全部方法停用
#                  (fail-closed,不是免鉴权)。Go 侧须认这个键(见 docs/ops/player-inbox-runbook.md §1;未认时注入的口令被忽略,
#                  写路径入口 Assert-PlayerInboxAdminTokenEnvDeclared 在真要注入口令时拒绝部署)。
#   EnqueueTypes: 入箱白名单(PlayerInboxMessageType 枚举名),镜像服务 yaml;空 = 入箱全部停用(上线顺序 §11.7 第 3 / 6 步)。
#   其余是后台循环参数(终态保留 / 清理 / 提醒补发 / gauge / 对账),逐键镜像服务 yaml。
PlayerInbox:
  AdminTokenEnv: $($script:PlayerInboxAdminTokenEnv)
${playerInboxEnqueueTypesLines}
${playerInboxLoopLines}
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

# 迁移 Job 两段等待(删前的在途检查 Assert-GoSvcMigrateJobNotInFlight、apply 后的终态轮询 Wait-ForGoSvcMigrateJob)
# 的预算下限,单位秒。实际预算 = max(-WaitTimeoutSeconds, 本值),统一由 Get-GoSvcMigrateJobWaitSeconds 给出。
# 为什么要下限:-WaitTimeoutSeconds 默认 180s 是按 Deployment rollout 定的,而 staging/prod 恒等迁移 Job(D-14 第 4 条)。
# 一次**正常**的迁移 Pod 要走完:调度 + 首次拉 mmorpg-<svc> 镜像(Job 先于 Deployment,是该镜像在节点上的第一个消费者)
#   + initContainer wait-mysql 等 3306(manifests/go-svc/trade-migrate.yaml 的 WAIT_MYSQL_TIMEOUT_SECONDS=150,
#     截止后最多再多一轮 sleep 5 + nc -w 3,硬上限 158s)
#   + 迁移本身(go/schemamigrate 每条 DDL 硬超时 60s;P1 两张表,正常几秒)。
# 158 + 约 60(调度 / 拉镜像)+ 约 60(迁移)≈ 280,取 300。预算装不下这些时,全新集群 initdb 还没开 3306 的
# 正常等待会被脚本误判为超时、中断发布。改大 WAIT_MYSQL_TIMEOUT_SECONDS(或别的建表服务的 Job 等得更久)时必须同步抬高本值。
$GoSvcMigrateJobMinWaitSeconds = 300

# 迁移 Job 等待的实际预算(秒):-WaitTimeoutSeconds 与下限 $GoSvcMigrateJobMinWaitSeconds 取大。
# 只抬不降:显式传更大的 -WaitTimeoutSeconds(如全新集群首次 infra-up 传 600)照样生效。
function Get-GoSvcMigrateJobWaitSeconds {
	return [Math]::Max($WaitTimeoutSeconds, $GoSvcMigrateJobMinWaitSeconds)
}

<#
.SYNOPSIS
	建表服务的一次性迁移 Job(<svc>-migrate):确认上一次不在途 → delete → apply → 门禁下等它结束。

.DESCRIPTION
	port-decisions D-14 第 4 条:staging/prod 的服务 ConfigMap 固定 Schema.AutoMigrate=false,表只由这个 Job 建。
	Job 与服务 Deployment 同一镜像、同一 ConfigMap,args 为 -f /app/etc/<yaml> -migrate(服务二进制自带的迁移入口,
	go/schemamigrate 的退出码:0 成功 / 1 失败 / 3 锁忙 / 4 需人工)。形状照 Apply-KafkaTopicInitJob:
	Job 名固定、template 不可变,每次先删再 apply;重跑幂等(schemamigrate 自带台账 + GET_LOCK)。

	重试语义写在 manifest 的 podFailurePolicy 里(1 / 4 → FailJob 立即失败,3 → 按 backoffLimit 重试),脚本自己不重试。

	门禁(D-14 第 4 条原文"此门禁不受 -WaitReady 控制"):
	  staging / prod:**恒等**。先等 mysql Deployment 就绪(Job 的 initContainer 还会再等 3306 可连,理由见 manifest 注释),
	    apply 后在迁移 Job 等待预算(Get-GoSvcMigrateJobWaitSeconds = max(-WaitTimeoutSeconds, $GoSvcMigrateJobMinWaitSeconds))
	    内轮询 Job 终态;Failed 或超时即打印 describe / Pod / 日志并 throw,调用方的 Deployment 不会被 apply。
	  dev:只在 -WaitReady 下同上;不带 -WaitReady 时不等 Job,Deployment 紧接着 apply —— dev 档 AutoMigrate=true,
	    服务启动期自己也会迁移(同一把 GET_LOCK),脚本只打印核对命令。

	删之前先确认上一次的 Job 不在途(与门禁无关、各档都做):kafka-topic-init 可以无脑先删再建,是因为它没有台账;
	迁移 runner 执行 DDL 前先写 dirty=1、成功后才清零,删掉在途 Job(Pod 被 SIGTERM / SIGKILL)打断 DDL 会把台账
	留在 dirty,之后每次 -migrate 都以 1 退出、被 FailJob 直接判失败,只能人工清台账。所以在途就先在
	迁移 Job 等待预算(同上,Get-GoSvcMigrateJobWaitSeconds)内等它到终态;等不到就 throw,绝不删在途 Job。
#>
function Apply-GoSvcMigrateJob {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$SvcImage,
		[Parameter(Mandatory = $true)][string]$PullPolicy
	)

	$info = $GoSvcCatalogue[$SvcName]
	# 显式旧dev兼容不使用SQL；全球Match即使dev也只读Plan，必须等专用迁移Job完成。
	if ($SvcName -eq 'match' -and -not (Get-GlobalMatchDeploymentSettings -Profile $ReleaseProfile -TiDBHost $MatchTiDBHost -Region $PhysicalRegion).Enabled) {
		return
	}
	$jobName = "$SvcName-migrate"
	$path = Join-Path $GoSvcManifestsDir $info.MigrateJob
	# fail-closed:主 manifest 在、迁移 Job 却不在,staging/prod 的 Deployment 必然因 plan 不净拒启;
	# 与其部署一个注定 CrashLoop 的服务,不如在这里直接拦下(与主 manifest 缺席时"整条跳过"的口径刻意不同)。
	if (-not (Test-Path $path)) {
		throw "Go service migrate Job manifest not found: $path($SvcName 的目录条目声明了 MigrateJob;fail-closed:表建不出来就不部署服务)"
	}

	$content = Get-Content -Path $path -Raw
	$content = $content.Replace("PLACEHOLDER_IMAGE", $SvcImage)
	$content = $content.Replace("PLACEHOLDER_PULL_POLICY", $PullPolicy)
	$content=Add-SchemaPermissionManifest -Content $content -Namespace $Namespace -SvcName $SvcName
	# initContainer 等 MySQL 用的地址:Job 与 MySQL 不一定同 namespace(目录允许非 Global 服务将来也挂 MigrateJob),写 FQDN 前缀。
	$content = $content.Replace("__INFRA_NAMESPACE__", $InfraNamespace)
	# 与 Apply-Infra 同一条 fail-closed:任何占位没替换就拒绝 apply,不把占位原样送进集群。
	$leftover = [regex]::Matches($content, '(__[A-Z][A-Z0-9_]*__|PLACEHOLDER_[A-Z_]+)') | ForEach-Object { $_.Value } | Sort-Object -Unique
	if ($leftover.Count -gt 0) {
		throw "migrate Job manifest $($info.MigrateJob) 里有未替换的占位:$($leftover -join ', ')。请在 Apply-GoSvcMigrateJob 里补对应的 Replace。"
	}

	# D-14 第 4 条:staging/prod 的迁移门禁不受 -WaitReady 控制,恒等 Job Complete、失败或超时中断发布;
	# 只有 dev 档把"等不等"交给 -WaitReady(dev 服务启动期自己也会迁移)。
	$gateOnJob = [bool]$WaitReady -or ($ReleaseProfile -ne 'dev') -or ($SvcName -eq 'match')

	if ($gateOnJob) {
		# MySQL 起不来时 -migrate 连库失败以 1 退出,会被 podFailurePolicy 当成不可重试直接判 Job 失败;
		# 先等 mysql Deployment 滚动完成,把"镜像还在拉 / Pod 还没调度"这类原因挡在 Job 之外,失败时报的也是真实原因。
		# (全局服务经 Apply-Infra → Apply-GlobalGoSvcManifests 调到,mysql.yaml 在同一次 Apply-Infra 里已 apply;
		#  zone 内的 data-service 经 zone-up 调到,那时 mysql 应当已由先前的 infra-up 部署在 infra namespace。)
		Wait-ForDeploymentReady -Namespace $InfraNamespace -DeploymentName "mysql"
	}

	# 删之前确认上一次的 Job 不在途:删掉在途迁移会打断 DDL、把台账留在 dirty(理由见函数头注释)。
	Assert-GoSvcMigrateJobNotInFlight -Namespace $Namespace -JobName $jobName

	# Job 名固定,重跑必须先删:template 不可变,apply 同名 Job 会被拒。--ignore-not-found:首次是 no-op。
	Invoke-Kubectl -Args @("delete", "job", $jobName, "-n", $Namespace, "--ignore-not-found")
	Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $content
	Write-Host "  [applied] $jobName -> $SvcImage (D-14 schema Job, before $SvcName Deployment)"

	if ($gateOnJob) {
		Wait-ForGoSvcMigrateJob -Namespace $Namespace -JobName $jobName -SvcName $SvcName
		return
	}
	Write-Warning ("Dev profile without -WaitReady: apply {1} Deployment without waiting for {0} (staging/prod always wait). Check 'kubectl -n {2} get job {0}' (COMPLETIONS 1/1) and 'kubectl -n {2} logs job/{0}'." -f $jobName, $SvcName, $Namespace)
}

# 迁移查询/诊断专用:HTTP 请求与整个 kubectl 进程都有截止。仅加 --request-timeout 仍不能
# 限制凭证插件等请求之外的等待;外层 Stopwatch 也不能打断阻塞的原生命令。
# 保留 context/kubeconfig,参数逐项传递,不经 shell 拼接;结果不用 LASTEXITCODE 跨函数传递。
function Invoke-GoSvcMigrateKubectl {
	param(
		[Parameter(Mandatory = $true)][string[]]$Args,
		[ValidateRange(0.001, 2147483)][double]$TimeoutSeconds = 10
	)
	$allArgs = @((Build-KubectlBaseArgs)) + $Args
	$timeoutMs = [int][Math]::Ceiling($TimeoutSeconds * 1000)
	$allArgs += "--request-timeout=${timeoutMs}ms"
	if ($DryRun) {
		Write-Host "[dry-run] kubectl $($allArgs -join ' ') (process timeout ${timeoutMs}ms)"
		return [pscustomobject]@{ ExitCode = 0; Output = ''; ErrorOutput = ''; TimedOut = $false }
	}
	$process = [System.Diagnostics.Process]::new()
	try {
		$process.StartInfo.FileName = (Get-Command kubectl -CommandType Application -ErrorAction Stop).Source
		$process.StartInfo.UseShellExecute = $false
		$process.StartInfo.CreateNoWindow = $true
		$process.StartInfo.RedirectStandardOutput = $true
		$process.StartInfo.RedirectStandardError = $true
		$process.StartInfo.StandardOutputEncoding = [System.Text.Encoding]::UTF8
		$process.StartInfo.StandardErrorEncoding = [System.Text.Encoding]::UTF8
		foreach ($arg in $allArgs) { $process.StartInfo.ArgumentList.Add([string]$arg) }
		$null = $process.Start()
		$stdout = $process.StandardOutput.ReadToEndAsync()
		$stderr = $process.StandardError.ReadToEndAsync()
		if (-not $process.WaitForExit($timeoutMs)) {
			# 仅杀本次查询及其凭证插件子进程,不触碰集群工作负载;清理最多再等 1s。
			try { $process.Kill($true) } catch { }
			$null = $process.WaitForExit(1000)
			return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = "kubectl 查询/诊断超过 ${timeoutMs}ms"; TimedOut = $true }
		}
		# 继承管道的后台子进程也不能让收尾无限阻塞。
		if (-not [System.Threading.Tasks.Task]::WaitAll([System.Threading.Tasks.Task[]]@($stdout, $stderr), 1000)) {
			return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = 'kubectl 输出管道未关闭'; TimedOut = $true }
		}
		return [pscustomobject]@{ ExitCode = $process.ExitCode; Output = $stdout.Result; ErrorOutput = $stderr.Result; TimedOut = $false }
	} catch {
		return [pscustomobject]@{ ExitCode = -1; Output = ''; ErrorOutput = $_.Exception.Message; TimedOut = $false }
	} finally {
		$process.Dispose()
	}
}

# 发布失败可在 FailureTarget 时立即报告;删除前必须逐个核对该 Job UID 所属 Pod 全终态。
# active=0 不包括 terminating Pod;旧 K8s 的 Failed/Complete 也可能早于 Pod 全退出。
function Get-GoSvcMigrateJobState {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$JobName,
		[ValidateRange(0.001, 2147483)][double]$TimeoutSeconds = 10,
		[switch]$RequirePodsTerminal
	)
	$budget = [System.Diagnostics.Stopwatch]::StartNew()
	try {
		$result = Invoke-GoSvcMigrateKubectl -Args @("get", "job", $JobName, "-n", $Namespace, "--ignore-not-found", "-o", "json") -TimeoutSeconds $TimeoutSeconds
		if ($result.ExitCode -ne 0) { return 'unknown' }
		if ([string]::IsNullOrWhiteSpace($result.Output)) { return 'absent' }
		$job = $result.Output | ConvertFrom-Json -ErrorAction Stop
		if ([string]::IsNullOrWhiteSpace($job.metadata.uid)) { return 'unknown' }
		$trueTypes = @($job.status.conditions | Where-Object { $_.status -eq 'True' } | ForEach-Object { $_.type })
		$state = 'running'
		if ($trueTypes -contains 'Failed' -or $trueTypes -contains 'FailureTarget') { $state = 'failed' }
		elseif ($trueTypes -contains 'Complete') { $state = 'complete' }
		if (-not $RequirePodsTerminal -or $state -eq 'running') { return $state }

		$remaining = $TimeoutSeconds - $budget.Elapsed.TotalSeconds
		if ($remaining -lt 0.001) { return 'unknown' }
		$result = Invoke-GoSvcMigrateKubectl -Args @("get", "pods", "-n", $Namespace, "-l", "job-name=$JobName", "-o", "json") -TimeoutSeconds $remaining
		if ($result.ExitCode -ne 0) { return 'unknown' }
		$pods = $result.Output | ConvertFrom-Json -ErrorAction Stop
		if ($null -eq $pods -or $null -eq $pods.PSObject.Properties['items'] -or $null -eq $pods.items) { return 'unknown' }
		foreach ($pod in $pods.items) {
			$owned = @($pod.metadata.ownerReferences | Where-Object { $_.kind -eq 'Job' -and $_.uid -eq $job.metadata.uid })
			if ($owned.Count -gt 0 -and $pod.status.phase -notin @('Succeeded', 'Failed')) { return 'running' }
		}
		return $state
	} catch {
		# API/JSON/凭证异常只能说明未知,既不能当成不存在,也不能放行删除。
		return 'unknown'
	}
}

# 两段等待共用单调总预算,每次查询只分到剩余时间(最多 10s),sleep 也不能越过截止。
function Wait-GoSvcMigrateJobSettled {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$JobName,
		[switch]$RequirePodsTerminal
	)
	$budgetSeconds = Get-GoSvcMigrateJobWaitSeconds
	$budget = [System.Diagnostics.Stopwatch]::StartNew()
	$state = 'unknown'
	while ($true) {
		$remaining = $budgetSeconds - $budget.Elapsed.TotalSeconds
		if ($remaining -lt 0.001) { return $state }
		$state = Get-GoSvcMigrateJobState -Namespace $Namespace -JobName $JobName -TimeoutSeconds ([Math]::Min([double]10, $remaining)) -RequirePodsTerminal:$RequirePodsTerminal
		if ($state -ne 'running' -and $state -ne 'unknown') { return $state }
		$remaining = $budgetSeconds - $budget.Elapsed.TotalSeconds
		if ($remaining -lt 0.001) { return $state }
		Start-Sleep -Milliseconds ([int][Math]::Min(5000, [Math]::Floor($remaining * 1000)))
	}
}

# 诊断共用一个短预算,错误只作补充信息,绝不替换调用方的迁移失败/超时错误。
function Write-GoSvcMigrateJobDiagnostics {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$JobName
	)
	$budget = [System.Diagnostics.Stopwatch]::StartNew()
	$commands = @(
		,@("describe", "job", $JobName, "-n", $Namespace)
		,@("get", "pods", "-n", $Namespace, "-l", "job-name=$JobName", "-o", "wide")
		,@("logs", "job/$JobName", "-n", $Namespace, "--all-containers", "--tail", "100")
	)
	foreach ($commandArgs in $commands) {
		$remaining = 10 - $budget.Elapsed.TotalSeconds
		if ($remaining -lt 0.001) { break }
		try {
			$result = Invoke-GoSvcMigrateKubectl -Args $commandArgs -TimeoutSeconds ([Math]::Min([double]3, $remaining))
			if ($result.Output) { Write-Host $result.Output }
			if ($result.ExitCode -ne 0) { Write-Warning "Migration diagnostics unavailable: $($result.ErrorOutput)" -WarningAction Continue }
		} catch {
			Write-Warning "Migration diagnostics unavailable: $($_.Exception.Message)" -WarningAction Continue
		}
	}
}
# 删 <svc>-migrate 之前的在途检查。场景:上一次发布等 Job 超时中断(锁忙在 backoff、DDL 慢),运维按提示重跑发布,
# 而旧 Job 其实还在跑 —— 此时直接 delete 会 SIGTERM 正在执行 DDL 的迁移容器,台账停在 dirty=1,之后每次 -migrate
# 都以 1 退出。所以:不存在 / Job 终态且其 UID 所属 Pod 全终态 → 放行;在途或状态读不到 → 在预算内等;
# 仍等不到 → throw,且**不删**。DryRun 不连集群,只打印这一步。
function Assert-GoSvcMigrateJobNotInFlight {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$JobName
	)

	$budgetSeconds = Get-GoSvcMigrateJobWaitSeconds
	if ($DryRun) {
		Write-Host "[dry-run] check job/$JobName -n $Namespace not in flight before delete (wait for Job and its UID-owned Pods to terminate --timeout ${budgetSeconds}s, unknown/running = abort without deleting)"
		return
	}

	$state = Wait-GoSvcMigrateJobSettled -Namespace $Namespace -JobName $JobName -RequirePodsTerminal
	if ($state -ne 'running' -and $state -ne 'unknown') {
		Write-Host "Previous $JobName can be replaced safely (state=$state; all Pods owned by its UID have terminated); proceeding with delete then create."
		return
	}

	try { Write-GoSvcMigrateJobDiagnostics -Namespace $Namespace -JobName $JobName } catch { Write-Warning "Migration diagnostics failed: $($_.Exception.Message)" -WarningAction Continue }
	throw ("上一次迁移 Job {0} 在 {1}s 内仍未结束(namespace={2} state={3}),发布中断,未删除它,也未 apply 新 Job 与服务 Deployment。" +
		"等 Job 及其 UID 所属 Pod 均进入终态后再重跑同一条发布命令(ACTIVE 为 0 本身不代表安全);" +
		"**不要手工 delete 在途 Job**:打断 DDL 会把 schema_migrations 台账留在 dirty,之后每次迁移都会以 1 失败。") -f $JobName, $budgetSeconds, $Namespace, $state
}

# 迁移 Job 的终态等待。与 Wait-ForJobComplete(kafka-topic-init 用)不同,这里要**同时**认 Complete 与 Failed:
# podFailurePolicy 把退出码 1 / 4 判成 FailJob,而 `kubectl wait --for=condition=complete` 遇到 Failed 会一直等到超时,
# 发布者要白等 WaitTimeoutSeconds 才看到"需人工"。所以轮询 status.conditions:见到 Failed(或 K8s 1.31+ 先出现的
# FailureTarget)立刻停;超时同样 throw —— 锁忙(退出码 3)还在 backoff 重试时也不算完成,不能 apply Deployment。
function Wait-ForGoSvcMigrateJob {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$JobName,
		[Parameter(Mandatory = $true)][string]$SvcName
	)

	$budgetSeconds = Get-GoSvcMigrateJobWaitSeconds
	if ($DryRun) {
		Write-Host "[dry-run] wait job/$JobName -n $Namespace until condition Complete (Failed / FailureTarget = abort) --timeout ${budgetSeconds}s"
		return
	}

	Write-Host "Waiting for migrate Job: namespace=$Namespace job=$JobName (timeout ${budgetSeconds}s = max(-WaitTimeoutSeconds ${WaitTimeoutSeconds}, floor ${GoSvcMigrateJobMinWaitSeconds}))"
	$state = Wait-GoSvcMigrateJobSettled -Namespace $Namespace -JobName $JobName

	if ($state -eq 'complete') {
		Write-Host "Migrate Job complete: namespace=$Namespace job=$JobName"
		return
	}

	# running / unknown 走到这里 = 预算用完;absent = 刚 apply 的 Job 被人删了,同样不能 apply Deployment。
	$verdict = switch ($state) {
		'failed' { 'failed' }
		'absent' { 'absent' }
		default { 'timeout' }
	}
	Write-Host "Migrate Job not complete: namespace=$Namespace job=$JobName verdict=$verdict"
	try { Write-GoSvcMigrateJobDiagnostics -Namespace $Namespace -JobName $JobName } catch { Write-Warning "Migration diagnostics failed: $($_.Exception.Message)" -WarningAction Continue }
	throw ("迁移 Job {0} 未成功(namespace={1} 结果={2}),发布中断,{3} Deployment 未 apply。看报告:kubectl -n {1} logs job/{0}。" +
		"退出码(go/schemamigrate):1 失败(库不存在 / 连不上 / dirty 台账 / DDL 报错,库名见日志)、4 需人工(类型漂移 / 缺主键 / 表清单异常)、" +
		"3 锁忙(timeout 时可能仍在 backoff 重试)。" +
		"重跑前:确认该 Job UID 所属 Pod 均处于 Succeeded/Failed 终态(ACTIVE 为 0 仍可能有 terminating Pod;重跑时脚本也会检查,在途或状态未知时不删);" +
		"日志里是 dirty 台账(schemamigrate.ErrDirty)时重跑不会自己好,先人工核对 schema_migrations 与表的实际结构再清 dirty;" +
		"其余原因修复后重跑同一条 infra-up / all-up(已终态的 Job 会先删再建,迁移幂等)。") -f $JobName, $Namespace, $verdict, $SvcName
}

function Apply-GoSvcManifests {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][int]$CurrentZoneId,
		[Parameter(Mandatory = $true)][int]$CurrentClusterId
	)

	if ($SkipGoSvc) { return }
	if ([string]::IsNullOrWhiteSpace($GoSvcRegistry)) {
		Write-Host "[skip] Go services: -GoSvcRegistry not set, skipping Go service deployment."
		return
	}

	Write-Host "Applying Go micro-service manifests to namespace $Namespace (registry=$GoSvcRegistry tag=$GoSvcTag)"

	# 全局服务(match)由 Apply-GlobalGoSvcManifests 在 infra 阶段部署,这里只放随 zone 走的
	foreach ($svcName in (Get-ZoneScopedGoSvcNames)) {
		Apply-OneGoSvc -SvcName $svcName -Namespace $Namespace -CurrentZoneId $CurrentZoneId -CurrentClusterId $CurrentClusterId
	}
}

# 控制面命令 topic 的寻址契约(gate-cmd_g<N> / scene-cmd_g<N>,partition = node_id % P,
# docs/design/control-plane-topic-partitioning-20260908.md)。与 C++ 节点 ConfigMap(New-NodeConfigMapYaml)、
# kafka-topic-init 预建读的是同一处真相:bin/etc/base_deploy_config.yaml 的 Kafka.CommandTopicPartitions / CommandTopicGeneration。
function Get-GoSvcCommandTopicContract {
	$partitionsRaw = Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.CommandTopicPartitions'
	$generationRaw = Get-AuthoritativeScalar -RelativePath 'bin/etc/base_deploy_config.yaml' -KeyPath 'Kafka.CommandTopicGeneration'
	$partitions = 0
	$generation = 0
	if (-not [int]::TryParse($partitionsRaw, [ref]$partitions) -or $partitions -le 0 -or
		-not [int]::TryParse($generationRaw, [ref]$generation) -or $generation -le 0) {
		throw "bin/etc/base_deploy_config.yaml 的 Kafka.CommandTopicPartitions / CommandTopicGeneration 必须是正整数(实际 '$partitionsRaw' / '$generationRaw');fail-closed:这是 gate / scene 命令的寻址协议"
	}
	return [pscustomobject]@{ Partitions = $partitions; Generation = $generation }
}

# 往 go-svc manifest **唯一**的那个 env: 段最前面插入一组环境变量 —— 部署级契约(只能有一处真相、不该写进各 manifest
# 的值)的统一注入点。目前两组:控制面命令 topic 契约(Add-GoSvcCommandTopicEnv)、topic 副本数(Add-GoSvcTopicReplicationEnv)。
#
# 对 manifest 形状 fail-closed:必须恰好一个 env: 段(go-svc 都是单容器),且它下面第一条非注释内容是列表项;
# 插入的条目沿用那一项的缩进,不假设「env 缩进 + 2」。manifest 里手写了同名变量也拒绝 —— 那是第二份真相。
# 所有 go-svc 都注入,不只是今天用得到的服务:新加一个服务时不需要记得来这里登记。
function Add-GoSvcEnvEntries {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		[Parameter(Mandatory = $true)][string]$ManifestContent,
		# 有序字典:变量名 -> 值(一律当字符串写进 YAML)。
		[Parameter(Mandatory = $true)][System.Collections.Specialized.OrderedDictionary]$Entries,
		# 报错里对这组变量的称呼(如 KAFKA_COMMAND_TOPIC_*)与用途(如「命令 topic 契约」)。
		[Parameter(Mandatory = $true)][string]$NamesLabel,
		[Parameter(Mandatory = $true)][string]$Purpose,
		# 写在注入处的一行 YAML 注释(不带 # 前缀),说明值从哪来。
		[Parameter(Mandatory = $true)][string]$Comment
	)

	$namePattern = (@($Entries.Keys) | ForEach-Object { [regex]::Escape([string]$_) }) -join '|'
	if ([regex]::IsMatch($ManifestContent, '(?m)^[ \t]*(-[ \t]+)?name:[ \t]*"?(' + $namePattern + ')"?[ \t]*\r?$')) {
		throw "Go service $SvcName 的 manifest 手写了 ${NamesLabel}:这组变量($Purpose)由 k8s_deploy.ps1 统一注入,请从 manifest 里删掉(一处真相,见 Add-GoSvcEnvEntries 注释)"
	}

	$envMatches = [regex]::Matches($ManifestContent, '(?m)^(?<indent>[ ]*)env:[ \t]*\r?$')
	if($ManifestContent.Contains('initContainers:')){
		$range=Get-SqlRuntimeManifestRange -ManifestContent $ManifestContent -SvcName $SvcName
		$envMatches=@($envMatches | Where-Object { $_.Index -gt $range.Start -and $_.Index -lt $range.End })
	}
	if ($envMatches.Count -ne 1) {
		throw "Go service $SvcName 的 manifest 应当恰好有 1 个 env: 段(实际 $($envMatches.Count) 个),无法注入${Purpose}。多容器或无 env 的 manifest 需要先在 Add-GoSvcEnvEntries 里明确注入目标"
	}
	$envMatch = $envMatches[0]
	$envIndent = $envMatch.Groups['indent'].Value
	# (?m) 下的 $ 停在 \n 之前(\r 已被 \r? 吃掉),所以 env: 行之后紧跟的必须是 \n。
	$lineEnd = $envMatch.Index + $envMatch.Length
	if ($lineEnd -ge $ManifestContent.Length -or $ManifestContent[$lineEnd] -ne "`n") {
		throw "Go service $SvcName 的 manifest 在 env: 之后没有内容,无法注入${Purpose}"
	}
	$insertAt = $lineEnd + 1

	$itemIndent = $null
	foreach ($line in ($ManifestContent.Substring($insertAt) -split "`r?`n")) {
		$trimmed = $line.Trim()
		if ($trimmed.Length -eq 0 -or $trimmed.StartsWith('#')) { continue }
		if ($line -match '^(?<indent>[ ]*)-[ ]') { $itemIndent = $Matches['indent'] }
		break
	}
	if ($null -eq $itemIndent -or $itemIndent.Length -lt $envIndent.Length) {
		throw "Go service $SvcName 的 manifest 里 env: 下第一条内容不是列表项,无法注入${Purpose}"
	}

	$newline = if ($ManifestContent.Contains("`r`n")) { "`r`n" } else { "`n" }
	$valueIndent = $itemIndent + '  '
	$injectedLines = @("${itemIndent}# $Comment")
	foreach ($name in $Entries.Keys) {
		$injectedLines += "${itemIndent}- name: $name"
		$injectedLines += "${valueIndent}value: `"$($Entries[$name])`""
	}
	$injected = $injectedLines -join $newline
	return $ManifestContent.Substring(0, $insertAt) + $injected + $newline + $ManifestContent.Substring($insertAt)
}

# 控制面命令 topic 契约(go/shared/kafkacmd 只认这两个变量,不配就回落到编译期默认 256 / 1)。
#
# 为什么必须注入:C++ gate / scene 按 base_deploy_config.yaml 消费 gate-cmd_g<N>(当前 N=2),kafka-topic-init
# 也只预建 g<N>;Go 侧(login 的会话绑定 / 顶号踢人、scene-manager 的换场景、player-locator、match、friend、guild 的推送)
# 若回落到 g1,命令落进一个没人消费的 topic,**静默丢失**,Kafka 不报错。本机 start_game.ps1 一直注入这两个变量,
# 所以本机与 robot 冒烟从来看不到这个缺口。
#
# 为什么在这里统一注入而不是写进各 manifest:代号是部署级常量,只能有一处真相;每个 manifest 各写一份,
# 下次换代号必漏改(kafkacmd 注释里的「四处同拍」纪律)。
function Add-GoSvcCommandTopicEnv {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		[Parameter(Mandatory = $true)][string]$ManifestContent,
		[Parameter(Mandatory = $true)][int]$Partitions,
		[Parameter(Mandatory = $true)][int]$Generation
	)

	$entries = [ordered]@{
		KAFKA_COMMAND_TOPIC_PARTITIONS = [string]$Partitions
		KAFKA_COMMAND_TOPIC_GENERATION = [string]$Generation
	}
	return Add-GoSvcEnvEntries -SvcName $SvcName -ManifestContent $ManifestContent -Entries $entries `
		-NamesLabel 'KAFKA_COMMAND_TOPIC_*' -Purpose '命令 topic 契约' `
		-Comment '由 k8s_deploy.ps1 从 bin/etc/base_deploy_config.yaml 注入(Add-GoSvcCommandTopicEnv),manifest 里不要手写'
}

# topic 副本数(go/shared/kafkautil.EnsureTopics 建 topic 时用,不配就是 1)。
# 值来自 Get-KafkaTopology:单 broker = 1,≥3 个 broker = 3,与 kafka.yaml 的 broker 默认值、kafka-topic-init 同源。
# 不注入的后果:多 broker 集群上 Go 服务按 1 份副本建出自己的 topic(db_task、match-results 等)——
# 那个 topic 仍是单点,而且 broker 的 min.insync.replicas=2 会让 acks=all 的写入直接被拒。
function Add-GoSvcTopicReplicationEnv {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		[Parameter(Mandatory = $true)][string]$ManifestContent,
		[Parameter(Mandatory = $true)][int]$ReplicationFactor
	)

	$entries = [ordered]@{ KAFKA_TOPIC_REPLICATION_FACTOR = [string]$ReplicationFactor }
	return Add-GoSvcEnvEntries -SvcName $SvcName -ManifestContent $ManifestContent -Entries $entries `
		-NamesLabel 'KAFKA_TOPIC_REPLICATION_FACTOR' -Purpose 'topic 副本数契约' `
		-Comment '由 k8s_deploy.ps1 按 -KafkaBrokers 注入(Add-GoSvcTopicReplicationEnv),manifest 里不要手写'
}

<#
.SYNOPSIS
	往服务 manifest 唯一的 env: 段追加一条 secretKeyRef 环境变量:密钥只经 K8s Secret 进容器,不进 ConfigMap / manifest。
	go-svc(login 开发口令)与 java-svc(gateway 管理面口令)共用。

.DESCRIPTION
	对 manifest 形状的要求与 Add-GoSvcCommandTopicEnv 相同(恰好一个 env: 段、其下第一条非注释内容是列表项),
	不对就 throw。两个调用方(Apply-OneGoSvc / Apply-JavaSvcManifests)都在本服务的任何集群写操作(ConfigMap /
	Secret / 迁移 Job / Deployment)之前调用它,形状不对不留半截部署。manifest 里手写了同名变量视为冲突,拒绝(一处真相)。
	secretKeyRef 不写 optional(= false):Secret 缺失时 Pod 卡在 CreateContainerConfigError,而不是带着空密钥起来。
#>
function Add-MatchAuthoritySecretEnv {
	param([string]$ManifestContent)
	# domain-service-secrets沿现役mail/rank/目录签名合同由运维预置，本脚本不生成或读取其真实值。
	# 新协议部署前须在Match/Battle所属namespace预置独立key match-outcome-battle，
	# 在Match/PlayerInbox所属namespace预置另一独立key player-inbox-match；同用途两端值须相同。
	# 下方均为mandatory secretKeyRef，缺Secret/缺key由K8s拒启，不借其他用途密钥降级。
	$settings = Get-GlobalMatchDeploymentSettings -Profile $ReleaseProfile -TiDBHost $MatchTiDBHost -Region $PhysicalRegion
	if (-not $settings.Enabled) { return $ManifestContent }
	foreach ($entry in @(
		@{ Field = 'ProbeSecretEnv'; Key = 'match-probe' },
		@{ Field = 'RankSecretEnv'; Key = 'rank-producer-match' }
	)) {
		$envName = Get-AuthoritativeScalar -RelativePath 'go/match/etc/match_service.yaml' -KeyPath "GlobalMatch.$($entry.Field)"
		if ($envName -cnotmatch '^[A-Z][A-Z0-9_]*$') { throw '匹配密钥环境变量名无效。' }
		$ManifestContent = Add-GoSvcSecretEnv -SvcName 'match' -ManifestContent $ManifestContent `
			-EnvName $envName -SecretName 'domain-service-secrets' -SecretKey $entry.Key
	}
	foreach ($entry in @(
		@{ Env = 'MMORPG_MATCH_OUTCOME_SECRET_BATTLE'; Key = 'match-outcome-battle' },
		@{ Env = 'MMORPG_PLAYER_INBOX_SECRET_MATCH'; Key = 'player-inbox-match' }
	)) {
		$ManifestContent = Add-GoSvcSecretEnv -SvcName 'match' -ManifestContent $ManifestContent `
			-EnvName $entry.Env -SecretName 'domain-service-secrets' -SecretKey $entry.Key
	}
	return $ManifestContent
}

function Add-PlayerInboxMatchSecretEnv {
	param([string]$ManifestContent)
	# 与Match共用同一开关；未启全球匹配时不要求仅供Battle入箱的独立key。
	$settings = Get-GlobalMatchDeploymentSettings -Profile $ReleaseProfile -TiDBHost $MatchTiDBHost -Region $PhysicalRegion
	if (-not $settings.Enabled) { return $ManifestContent }
	return Add-GoSvcSecretEnv -SvcName 'player-inbox' -ManifestContent $ManifestContent `
		-EnvName 'MMORPG_PLAYER_INBOX_SECRET_MATCH' -SecretName 'domain-service-secrets' -SecretKey 'player-inbox-match'
}

function Add-GoSvcSecretEnv {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		[Parameter(Mandatory = $true)][string]$ManifestContent,
		[Parameter(Mandatory = $true)][string]$EnvName,
		[Parameter(Mandatory = $true)][string]$SecretName,
		[Parameter(Mandatory = $true)][string]$SecretKey
	)

	$handWritten = '(?m)^[ \t]*(-[ \t]+)?name:[ \t]*"?{0}"?[ \t]*\r?$' -f [regex]::Escape($EnvName)
	if ($ManifestContent -cmatch $handWritten) {
		throw "服务 $SvcName 的 manifest 手写了 ${EnvName}:它由 k8s_deploy.ps1 经 Secret $SecretName 注入(Add-GoSvcSecretEnv),请从 manifest 里删掉。"
	}

	$envMatches = [regex]::Matches($ManifestContent, '(?m)^(?<indent>[ ]*)env:[ \t]*\r?$')
	if($ManifestContent.Contains('initContainers:')){
		$range=Get-SqlRuntimeManifestRange -ManifestContent $ManifestContent -SvcName $SvcName
		$envMatches=@($envMatches | Where-Object { $_.Index -gt $range.Start -and $_.Index -lt $range.End })
	}
	if ($envMatches.Count -ne 1) {
		throw "服务 $SvcName 的 manifest 应当恰好有 1 个 env: 段(实际 $($envMatches.Count) 个),无法注入 $EnvName。"
	}
	$envMatch = $envMatches[0]
	$lineEnd = $envMatch.Index + $envMatch.Length
	if ($lineEnd -ge $ManifestContent.Length -or $ManifestContent[$lineEnd] -ne "`n") {
		throw "服务 $SvcName 的 manifest 在 env: 之后没有内容,无法注入 $EnvName。"
	}
	$insertAt = $lineEnd + 1

	$itemIndent = $null
	foreach ($line in ($ManifestContent.Substring($insertAt) -split "`r?`n")) {
		$trimmed = $line.Trim()
		if ($trimmed.Length -eq 0 -or $trimmed.StartsWith('#')) { continue }
		if ($line -match '^(?<indent>[ ]*)-[ ]') { $itemIndent = $Matches['indent'] }
		break
	}
	if ($null -eq $itemIndent -or $itemIndent.Length -lt $envMatch.Groups['indent'].Value.Length) {
		throw "服务 $SvcName 的 manifest 里 env: 下第一条内容不是列表项,无法注入 $EnvName。"
	}

	$newline = if ($ManifestContent.Contains("`r`n")) { "`r`n" } else { "`n" }
	$valueIndent = $itemIndent + '  '
	$injected = @(
		"${itemIndent}# 由 k8s_deploy.ps1 经 Secret $SecretName 注入(Add-GoSvcSecretEnv),manifest 里不要手写",
		"${itemIndent}- name: $EnvName",
		"${valueIndent}valueFrom:",
		"${valueIndent}  secretKeyRef:",
		"${valueIndent}    name: $SecretName",
		"${valueIndent}    key: $SecretKey"
	) -join $newline
	return $ManifestContent.Substring(0, $insertAt) + $injected + $newline + $ManifestContent.Substring($insertAt)
}

<#
.SYNOPSIS
	往服务 manifest 唯一的 Pod 模板(template: 紧跟 metadata:)新增 annotations: 段,写一条注解(-Key / -Value),
	可再带若干条附加注解(-AdditionalAnnotations)。
	用于让"只改 Secret"这类 Pod 模板本身看不见的变更也能触发滚动(见 Get-InjectedSecretFingerprint)。

.DESCRIPTION
	形状要求:恰好一处 template: 下一行是 metadata:,且该 metadata 下还没有 annotations: —— 已有就 throw,
	不猜怎么合并(重复键的 YAML 各解析器行为不一)。与 Add-GoSvcSecretEnv 一样在本服务任何集群写操作之前调用。
	值一律写成双引号标量:12 位十六进制可能全是数字或形如 1e5…,不加引号会被 YAML 解析成数字,注解值必须是字符串。
	同一个 Pod 模板只能调用本函数一次(第二次会撞上「已有 annotations:」而 throw),所以多条注解必须经
	-AdditionalAnnotations 一次写入。-Key 那一条恒排在 annotations: 的下一行(契约测试按这个位置取 gateway 口令指纹),
	附加项按字典的迭代顺序跟在后面 —— 要稳定顺序就传 [ordered]。不传附加项时输出与只写一条时逐字相同。
	每一条都做同样的校验:键必须是「小写 DNS 前缀/名字」形状且互不重复,值非空且不含引号、反斜杠、换行。
#>
function Add-PodTemplateAnnotation {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		[Parameter(Mandatory = $true)][string]$ManifestContent,
		[Parameter(Mandatory = $true)][string]$Key,
		[Parameter(Mandatory = $true)][string]$Value,
		# 附加注解(按迭代顺序写在 -Key 之后)。同一 Pod 模板只能有一个 annotations: 段,多条注解必须一次写入。
		[AllowNull()][System.Collections.IDictionary]$AdditionalAnnotations = $null
	)

	# 主注解排第一,附加项跟在后面;下面逐条校验、逐条渲染,两处走同一个列表。
	$annotationItems = @([pscustomobject]@{ Key = $Key; Value = $Value })
	if ($null -ne $AdditionalAnnotations) {
		foreach ($extraKey in $AdditionalAnnotations.Keys) {
			$annotationItems += [pscustomobject]@{ Key = [string]$extraKey; Value = [string]$AdditionalAnnotations[$extraKey] }
		}
	}
	$seenAnnotationKeys = @()
	foreach ($item in $annotationItems) {
		if ($item.Key -cnotmatch '^[a-z0-9]([a-z0-9.-]*[a-z0-9])?/[a-z0-9]([a-z0-9._-]*[a-z0-9])?$') {
			throw "内部错误:服务 $SvcName 的注解键 '$($item.Key)' 不是「小写 DNS 前缀/名字」形状,拒绝渲染。"
		}
		if ($seenAnnotationKeys -ccontains $item.Key) {
			throw "内部错误:服务 $SvcName 的注解 $($item.Key) 重复出现,拒绝渲染(重复键的 YAML 各解析器行为不一)。"
		}
		$seenAnnotationKeys += $item.Key
		if ([string]::IsNullOrEmpty($item.Value)) {
			throw "内部错误:服务 $SvcName 的注解 $($item.Key) 的值为空,拒绝渲染。"
		}
		if ($item.Value -match '["\\\r\n]') {
			throw "内部错误:服务 $SvcName 的注解 $($item.Key) 的值含引号、反斜杠或换行,拒绝渲染。"
		}
	}
	$templateMatches = [regex]::Matches($ManifestContent, '(?m)^(?<tindent>[ ]*)template:[ \t]*\r?\n(?<mindent>[ ]*)metadata:[ \t]*\r?$')
	if ($templateMatches.Count -ne 1) {
		throw "服务 $SvcName 的 manifest 应当恰好有 1 个 Pod 模板(template: 下一行 metadata:),实际 $($templateMatches.Count) 个,无法写注解 $Key。"
	}
	$templateMatch = $templateMatches[0]
	$metadataIndent = $templateMatch.Groups['mindent'].Value
	if ($metadataIndent.Length -le $templateMatch.Groups['tindent'].Value.Length) {
		throw "服务 $SvcName 的 manifest 里 template: 下的 metadata: 缩进不对,无法写注解 $Key。"
	}
	$lineEnd = $templateMatch.Index + $templateMatch.Length
	if ($lineEnd -ge $ManifestContent.Length -or $ManifestContent[$lineEnd] -ne "`n") {
		throw "服务 $SvcName 的 manifest 在 Pod 模板 metadata: 之后没有内容,无法写注解 $Key。"
	}
	$insertAt = $lineEnd + 1

	# 取 metadata 的子键缩进,顺带确认它下面还没有 annotations:。缩进回到 metadata 同级或更浅即出块。
	$childIndent = $null
	foreach ($line in ($ManifestContent.Substring($insertAt) -split "`r?`n")) {
		$trimmed = $line.Trim()
		if ($trimmed.Length -eq 0 -or $trimmed.StartsWith('#')) { continue }
		$indent = $line.Length - $line.TrimStart(' ').Length
		if ($indent -le $metadataIndent.Length) { break }
		if ($null -eq $childIndent) { $childIndent = ' ' * $indent }
		if ($indent -eq $childIndent.Length -and $trimmed -match '^annotations:') {
			throw "服务 $SvcName 的 manifest 在 Pod 模板里已有 annotations:,$Key 由 k8s_deploy.ps1 生成,请把手写的注解段并入生成逻辑或删掉。"
		}
	}
	if ($null -eq $childIndent) {
		throw "服务 $SvcName 的 manifest 里 Pod 模板 metadata: 下没有子键,无法写注解 $Key。"
	}

	$newline = if ($ManifestContent.Contains("`r`n")) { "`r`n" } else { "`n" }
	$injectedLines = @(
		"${childIndent}# 由 k8s_deploy.ps1 生成(Add-PodTemplateAnnotation),manifest 里不要手写",
		"${childIndent}annotations:"
	)
	foreach ($item in $annotationItems) {
		$injectedLines += ('{0}  {1}: "{2}"' -f $childIndent, $item.Key, $item.Value)
	}
	$injected = $injectedLines -join $newline
	return $ManifestContent.Substring(0, $insertAt) + $injected + $newline + $ManifestContent.Substring($insertAt)
}

<#
.SYNOPSIS
	注入密钥的短指纹(SHA-256 前 12 位十六进制),给 Pod 模板注解用:secretKeyRef 只在容器启动时读,
	只改 Secret 时 Pod 模板逐字节没变,kubectl apply 是 no-op,旧口令会一直有效。指纹进模板后,口令一变即滚动。

.DESCRIPTION
	与 Get-CppLogSidecarConfigHash 同一手法,只是输入是密钥,所以写明暴露面:
	- 哈希输入带 Secret 名做域分隔,同一个值用在别处得到的指纹不同,不能跨用途比对;
	- 只截 48 位,是变更探测器而不是校验值;非 dev 档口令 ≥32 位(Resolve-InjectedSecret),无法由指纹反推;
	- 能读 Deployment 的人本就能读同 namespace 的 ConfigMap,而 gateway ConfigMap 里有明文的 gate.token-secret 与数据源密码,
	  指纹不扩大暴露面;但它也不能把弱口令变强 —— 口令强度仍由运维负责。
	削峰指纹(G4b,$script:PeakShavingHashAnnotation)复用本函数:-SecretName 传用途串(peak-shaving/gateway、peak-shaving/login)
	作域分隔,-Value 传「写进 ConfigMap 的那段配置文本 + 相关密钥」,输入不是单个密钥,其余同上。
	纯本地计算、不读集群,DryRun 与真跑结果一致。
#>
function Get-InjectedSecretFingerprint {
	param(
		[Parameter(Mandatory = $true)][string]$SecretName,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value
	)

	if ([string]::IsNullOrWhiteSpace($Value)) {
		throw "内部错误:Secret $SecretName 的值未解析(Initialize-InjectedSecrets 未在写路径入口调用?),无法计算指纹。"
	}
	$sha256 = [System.Security.Cryptography.SHA256]::Create()
	try {
		$bytes = $sha256.ComputeHash([System.Text.Encoding]::UTF8.GetBytes("$SecretName`n$Value"))
	} finally {
		$sha256.Dispose()
	}
	return ((($bytes | ForEach-Object { $_.ToString('x2') }) -join '').Substring(0, 12))
}

<#
.SYNOPSIS
	把部署侧解析好的一个密钥写成 zone namespace 的单键 Opaque Secret。必须先于引用它的 Deployment apply
	(secretKeyRef 不写 optional,Secret 缺失的 Pod 卡在 CreateContainerConfigError)。

.DESCRIPTION
	DryRun 不走 Invoke-KubectlWithInputFile:它会把整份 YAML(含明文)打进输出,这里只打一行已隐去值的意图。
	值写成 YAML 双引号标量:反斜杠、双引号、制表符与换行全部转义 —— 制表符不转义会被 Invoke-KubectlWithInputFile
	换成 4 个空格,密钥被静默改掉。值为空一律 throw:空密钥进 Secret 等于静默关掉鉴权。
#>
function Apply-InjectedSecret {
	param(
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][string]$Name,
		[Parameter(Mandatory = $true)][string]$Key,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value,
		# 引用它的工作负载,写进 app 标签与报错文本。
		[Parameter(Mandatory = $true)][string]$AppLabel
	)

	if ([string]::IsNullOrWhiteSpace($Value)) {
		throw "内部错误:Secret $Name 的值未解析(Initialize-InjectedSecrets 未在写路径入口调用?),拒绝部署 $AppLabel。"
	}
	if ($DryRun) {
		Write-Host "[dry-run] kubectl apply -n $Namespace -f -   # Secret $Name (key $Key; value hidden)"
		return
	}

	$escaped = '"' + $Value.Replace('\', '\\').Replace('"', '\"').Replace("`t", '\t').Replace("`r", '\r').Replace("`n", '\n') + '"'
	$secretYaml = @"
apiVersion: v1
kind: Secret
metadata:
  name: $Name
  labels:
    app: $AppLabel
    mmorpg.io/managed-by: k8s_deploy.ps1
type: Opaque
stringData:
  ${Key}: $escaped
"@
	Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $secretYaml
}

# -LoginDevPasswordAuth 的共享口令 Secret(zone namespace)。必须先于引用它的 login Deployment apply。
function Apply-LoginDevPasswordSecret {
	param([Parameter(Mandatory = $true)][string]$Namespace)

	if ([string]::IsNullOrWhiteSpace($script:LoginDevPasswordSecret)) {
		throw "内部错误:-LoginDevPasswordAuth 的共享口令未解析(Initialize-InjectedSecrets 未在写路径入口调用?),拒绝部署 login。"
	}
	Apply-InjectedSecret -Namespace $Namespace -Name $script:LoginDevPasswordSecretName -Key $script:LoginDevPasswordSecretKey `
		-Value $script:LoginDevPasswordSecret -AppLabel "login"
}

# -LoginWeChatAppId 的 AppSecret Secret login-oauth(zone namespace)。必须先于引用它的 login Deployment apply。
function Apply-LoginOAuthSecret {
	param([Parameter(Mandatory = $true)][string]$Namespace)

	if ([string]::IsNullOrWhiteSpace($script:LoginWeChatAppSecret)) {
		throw "内部错误:-LoginWeChatAppId 的 AppSecret 未解析(Initialize-InjectedSecrets 未在写路径入口调用?),拒绝部署 login。"
	}
	Apply-InjectedSecret -Namespace $Namespace -Name $script:LoginOAuthSecretName -Key $script:LoginOAuthWeChatSecretKey `
		-Value $script:LoginWeChatAppSecret -AppLabel "login"
}

# 单个 Go 服务的 ConfigMap + 主 manifest apply。zone 循环与全局循环共用同一段逻辑。
function Get-SqlRuntimeManifestRange {
    param([string]$ManifestContent,[string]$SvcName)
    $main=[regex]::Matches($ManifestContent,'(?m)^      containers:\s*\r?$')
    if($main.Count -ne 1){throw '工作负载必须有唯一普通containers段；init不参与runtime密钥注入'}
    $namePattern='(?m)^        - name:\s*"?'+[regex]::Escape($SvcName)+'(?:-migrate)?"?\s*\r?$'
    $candidates=@([regex]::Matches($ManifestContent,$namePattern) | Where-Object { $_.Index -gt $main[0].Index })
    if($candidates.Count -ne 1){throw 'runtime具名容器缺失/重复，拒绝注入而不猜第一个env'}
    $start=$candidates[0].Index
    $tail=$ManifestContent.Substring($start+$candidates[0].Length)
    $next=[regex]::Match($tail,'(?m)^(?:        - name:|      [A-Za-z]|---)')
    $end=if($next.Success){$start+$candidates[0].Length+$next.Index}else{$ManifestContent.Length}
    return @{Start=$start;End=$end}
}

function Get-SqlWriterDeploymentEntry {
    param([string]$Namespace,[string]$SvcName)
    $catalogFile=[Environment]::GetEnvironmentVariable('MMORPG_SQL_K8S_BOOT_CATALOG','Process')
    if([string]::IsNullOrWhiteSpace($catalogFile)){throw 'SQL部署必须提供MMORPG_SQL_K8S_BOOT_CATALOG，不使用共享runtime账号默认值'}
    $catalog=Read-MaintenanceJson -Path $catalogFile
    $key=$Namespace+'/'+$SvcName
    if(-not $catalog.Contains($key)){throw '部署实例缺少预固定per-boot计划及独立Secret'}
    $entry=$catalog[$key]
    Assert-MaintenanceProperties -Value $entry -Required @('requestSecret','registrarSecret','registrarImage','physicalAuthoritySha256','runtimeConfigSecret','replicas','rosterFile','rosterSha256') -Optional @('schemaConfigSecret','schemaPermissionSecret')
    $secrets=@($entry.requestSecret,$entry.registrarSecret,$entry.runtimeConfigSecret)
    foreach($name in $secrets){if($name -cnotmatch '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'){throw 'boot Secret名称非法'}}
    if(@($secrets | Select-Object -Unique).Count -ne 3){throw '运行配置全集、registrar和注册计划必须用不同Secret'}
    if($entry.Contains('schemaConfigSecret') -and $entry.schemaConfigSecret -cin $secrets){throw 'schema配置不得复用normal/registrar/roster Secret'}
    if($entry.Contains('schemaPermissionSecret') -and ($entry.schemaPermissionSecret -cin $secrets -or $entry.schemaPermissionSecret -ceq $entry.schemaConfigSecret)){throw 'schema/registrar/normal凭据Secret必须不同'}
    if($entry.physicalAuthoritySha256 -cnotmatch '^[a-f0-9]{64}$' -or $entry.rosterSha256 -cnotmatch '^[a-f0-9]{64}$' -or $entry.registrarImage -cnotmatch '^[A-Za-z0-9._:/-]+@sha256:[a-f0-9]{64}$'){throw 'registrar镜像、roster或物理authority未固定不可变摘要'}
    if(-not [IO.Path]::IsPathRooted($entry.rosterFile)){throw '有限roster必须明确本地已批准原件路径'}
    $roster=Read-MaintenanceJson -Path $entry.rosterFile -ExpectedSha256 $entry.rosterSha256
    Assert-MaintenanceProperties -Value $roster -Required @('format','entries')
    if($roster.format -cne 'SQL_WRITER_BOOT_ROSTER_V1' -or @($roster.entries).Count -lt [int]$entry.replicas -or @($roster.entries).Count -gt 1024 -or [int]$entry.replicas -lt 1){throw '有限roster不能覆盖本次副本或超过既有1024预算'}
    # 这里仅校验本地批准原件；init再次核同SHA、所有entry身份及真实永久REGISTER结局后才选中一个。
    return $entry
}

function Add-SqlWriterBootManifest {
    param([string]$ManifestContent,[string]$Namespace,[string]$SvcName,[ValidateSet('go','java')][string]$RuntimeKind='go')
    $entry=Get-SqlWriterDeploymentEntry -Namespace $Namespace -SvcName $SvcName
    $replicas=[regex]::Matches($ManifestContent,'(?m)^  replicas:\s*(\d+)\s*$')
    if($replicas.Count -ne 1 -or [int]$replicas[0].Groups[1].Value -ne [int]$entry.replicas){throw 'boot计划与真实Deployment副本集合不符'}
    if(-not $ManifestContent.Contains('sql-writer-registrar')){
        # 注入块只来自同批data-service模板；所有normal编辑限定具名containers范围，绝不误改init。
        $prototype=[IO.File]::ReadAllText((Join-Path $RepoRoot 'deploy/k8s/manifests/go-svc/data-service.yaml'))
        $init=[regex]::Match($prototype,'(?ms)^      # 独立registrar.*?(?=^      containers:)').Value
        $protoRange=Get-SqlRuntimeManifestRange -ManifestContent $prototype -SvcName data-service
        $protoRuntime=$prototype.Substring($protoRange.Start,$protoRange.End-$protoRange.Start)
        $env=[regex]::Match($protoRuntime,'(?ms)^          # grant只读.*?^              value: "PLACEHOLDER_SQL_WRITER_PHYSICAL_SHA256"\r?\n').Value
        $volumes=[regex]::Match($prototype,'(?ms)^        - name: sql-writer-request\r?\n.*?(?=^        - name: svc-config)').Value
        $range=Get-SqlRuntimeManifestRange -ManifestContent $ManifestContent -SvcName $SvcName
        $runtime=$ManifestContent.Substring($range.Start,$range.End-$range.Start)
        if(-not $init -or -not $env -or -not $volumes -or [regex]::Matches($runtime,'(?m)^          env:\s*$').Count -ne 1 -or [regex]::Matches($runtime,'(?m)^          volumeMounts:\s*$').Count -ne 1 -or [regex]::Matches($ManifestContent,'(?m)^      volumes:\s*$').Count -ne 1 -or $ManifestContent -match '(?m)^      (?:securityContext|initContainers):'){throw 'SQL boot注入形状变化，拒绝猜测或覆盖现有init/securityContext'}
        $envAt=[regex]::Match($runtime,'(?m)^          env:\s*\r?$')
        $runtime=$runtime.Insert($envAt.Index+$envAt.Length,[Environment]::NewLine+$env.TrimEnd())
        $mountAt=[regex]::Match($runtime,'(?m)^          volumeMounts:\s*\r?$')
        $mounts='            - {name: sql-writer-grant, mountPath: /run/sql-writer/grant, readOnly: true}'+[Environment]::NewLine+'            - {name: sql-writer-consume, mountPath: /run/sql-writer/consume}'
        $runtime=$runtime.Insert($mountAt.Index+$mountAt.Length,[Environment]::NewLine+$mounts)
        if($RuntimeKind -ceq 'go'){
            $match=[regex]::Match($runtime,'(?m)^          args: (\[[^\r\n]+\])\s*$')
            if(-not $match.Success){throw 'Go runtime参数形状不符，不能遗漏grant SHA导入'}
            $originalArgs=@($match.Groups[1].Value | ConvertFrom-Json)
            if($originalArgs.Count -lt 2 -or $originalArgs[0] -cne '-f' -or $originalArgs[1] -cnotmatch '^/app/etc/(?<file>[A-Za-z0-9_.-]+\.yaml)$'){throw 'Go配置必须显式固定/app/etc下单文件'}
            $configName=$Matches.file
            $command='MMORPG_SQL_WRITER_GRANT_SHA256="$(cat /run/sql-writer/grant/boot-grant.json.sha256)"; export MMORPG_SQL_WRITER_GRANT_SHA256; exec /app/service "$@"'
            $args=@($command,'--')+$originalArgs
            $replacement='          command: ["/bin/sh", "-ec"]'+[Environment]::NewLine+'          args: '+(ConvertTo-Json -InputObject $args -Compress)
            $runtime=$runtime.Substring(0,$match.Index)+$replacement+$runtime.Substring($match.Index+$match.Length)
        }else{$configName='application-k8s.yaml'}
        $init=$init.Replace('/run/sql-writer/runtime/data_service.yaml','/run/sql-writer/runtime/'+$configName)
        $ManifestContent=$ManifestContent.Substring(0,$range.Start)+$runtime+$ManifestContent.Substring($range.End)
        $containers=[regex]::Match($ManifestContent,'(?m)^      containers:\s*\r?$')
        $ManifestContent=$ManifestContent.Insert($containers.Index,$init)
        $volumeAt=[regex]::Match($ManifestContent,'(?m)^      volumes:\s*\r?$')
        $ManifestContent=$ManifestContent.Insert($volumeAt.Index+$volumeAt.Length,[Environment]::NewLine+$volumes.TrimEnd())
        $configPattern='(?m)^        - name: svc-config\r?\n(?:          configMap:\s*\r?\n            name: [^\r\n]+|          configMap: \{name: [^\r\n]+\})'
        $configMatch=[regex]::Match($ManifestContent,$configPattern)
        if(-not $configMatch.Success -or [regex]::Matches($ManifestContent,$configPattern).Count -ne 1){throw 'runtime必须唯一完整配置卷，不能复用旧共享凭据ConfigMap'}
        $replacement='        - name: svc-config'+[Environment]::NewLine+'          emptyDir: {medium: Memory, sizeLimit: 20Mi}'
        $ManifestContent=$ManifestContent.Substring(0,$configMatch.Index)+$replacement+$ManifestContent.Substring($configMatch.Index+$configMatch.Length)
    }
    if(-not $ManifestContent.Contains('"-roster"') -or -not $ManifestContent.Contains('"-runtime-config-output"') -or -not $ManifestContent.Contains('sql-writer-runtime-input')){throw '现有boot模板没有真正有限roster与选中配置隔离'}
    $replace=@{PLACEHOLDER_SQL_WRITER_REQUEST_SECRET=$entry.requestSecret;PLACEHOLDER_SQL_WRITER_RUNTIME_SECRET=$entry.runtimeConfigSecret;PLACEHOLDER_SQL_WRITER_REGISTRAR_SECRET=$entry.registrarSecret;PLACEHOLDER_SQL_WRITER_REGISTRAR_IMAGE=$entry.registrarImage;PLACEHOLDER_SQL_WRITER_PHYSICAL_SHA256=$entry.physicalAuthoritySha256;PLACEHOLDER_SQL_WRITER_ROSTER_SHA256=$entry.rosterSha256}
    foreach($key in $replace.Keys){$ManifestContent=$ManifestContent.Replace($key,[string]$replace[$key])}
    if($ManifestContent -match 'PLACEHOLDER_SQL_WRITER_'){throw 'SQL boot占位残留，拒绝部署'}
    $normalRange=Get-SqlRuntimeManifestRange -ManifestContent $ManifestContent -SvcName $SvcName
    $normal=$ManifestContent.Substring($normalRange.Start,$normalRange.End-$normalRange.Start)
    if($normal -match 'MMORPG_SQL_(?:REGISTRAR|MAINTENANCE|SCHEMA)_DSN|name: sql-writer-request|name: sql-writer-runtime-input'){throw 'normal不能读取控制凭据、完整roster或其他boot配置'}
    return $ManifestContent
}

function Add-SchemaPermissionManifest {
    param([string]$Content,[string]$Namespace,[string]$SvcName)
    $entry=Get-SqlWriterDeploymentEntry -Namespace $Namespace -SvcName $SvcName
    if(-not $entry.Contains('schemaConfigSecret') -or -not $entry.Contains('schemaPermissionSecret')){throw 'fresh schema Job缺独立schema配置/权限Secret，禁止借normal'}
    foreach($name in @($entry.schemaConfigSecret,$entry.schemaPermissionSecret)){if($name -cnotmatch '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'){throw 'schema Secret名称非法'}}
    $pattern='(?m)^(        - name: svc-config\r?\n)          configMap:\r?\n            name: [^\r\n]+'
    if([regex]::Matches($Content,$pattern).Count -ne 1){throw 'schema Job必须唯一独立配置挂载'}
    $Content=[regex]::Replace($Content,$pattern,('$1          secret:'+[Environment]::NewLine+'            secretName: '+$entry.schemaConfigSecret))
    $range=Get-SqlRuntimeManifestRange -ManifestContent $Content -SvcName $SvcName
    $envMatches=@([regex]::Matches($Content,'(?m)^          env:\s*\r?$') | Where-Object { $_.Index -gt $range.Start -and $_.Index -lt $range.End })
    if($envMatches.Count -ne 1){throw 'schema Job权限env形状不符'}
    $env='            - name: MMORPG_SQL_SCHEMA_DSN'+[Environment]::NewLine+'              valueFrom: {secretKeyRef: {name: '+$entry.schemaPermissionSecret+', key: dsn, optional: false}}'+[Environment]::NewLine+'            - name: MMORPG_SQL_SCHEMA_PRINCIPAL'+[Environment]::NewLine+'              valueFrom: {secretKeyRef: {name: '+$entry.schemaPermissionSecret+', key: principal, optional: false}}'
    $at=$envMatches[0].Index+$envMatches[0].Length
    return $Content.Substring(0,$at)+[Environment]::NewLine+$env+$Content.Substring($at)
}

function Apply-OneGoSvc {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		[Parameter(Mandatory = $true)][string]$Namespace,
		[Parameter(Mandatory = $true)][int]$CurrentZoneId,
		[Parameter(Mandatory = $true)][int]$CurrentClusterId
	)

	$info = $GoSvcCatalogue[$SvcName]
	$svcImage = "$GoSvcRegistry/$($info.ImageName):$GoSvcTag"

	# manifest 缺席就整条跳过,**连 ConfigMap 也不 apply**:以前是先 apply ConfigMap 再发现 manifest 不在,
	# 留下一份没有 Deployment 消费的孤儿 ConfigMap。目录里允许「部署链先登记、manifest 后落地」的条目
	# (client-rpc-router 曾经如此,现已落地),所以这里是预期内的跳过而不是错误。
	$manifestPath = Join-Path $GoSvcManifestsDir $info.Manifest
	if (-not (Test-Path $manifestPath)) {
		Write-Warning "Go service manifest not found: $manifestPath – skipping $SvcName (ConfigMap not applied)"
		return
	}

	# 主 manifest 先在本地渲染完(镜像占位 + 控制面命令 topic 契约注入),再做任何集群写操作:
	# 注入对 manifest 形状 fail-closed,形状不对要在 ConfigMap / 迁移 Job 落地之前就拦下,不留半截部署。
	$svcPullPolicy = Resolve-ImagePullPolicy -ImageRef $svcImage
	$manifestContent = (Get-Content $manifestPath -Raw) -replace 'PLACEHOLDER_IMAGE', $svcImage
	$manifestContent = $manifestContent -replace 'PLACEHOLDER_PULL_POLICY', $svcPullPolicy
	if ($SvcName -in @('db','data-service','login','chat','friend','guild','mail','rank','match','trade','player-inbox')) {
		$manifestContent=Add-SqlWriterBootManifest -ManifestContent $manifestContent -Namespace $Namespace -SvcName $SvcName -RuntimeKind go
	}

	$commandTopic = Get-GoSvcCommandTopicContract
	$manifestContent = Add-GoSvcCommandTopicEnv -SvcName $SvcName -ManifestContent $manifestContent `
		-Partitions $commandTopic.Partitions -Generation $commandTopic.Generation
	$manifestContent = Add-GoSvcTopicReplicationEnv -SvcName $SvcName -ManifestContent $manifestContent `
		-ReplicationFactor (Get-KafkaTopology).ReplicationFactor
	if ($SvcName -eq 'match') {
		$manifestContent = Add-MatchAuthoritySecretEnv -ManifestContent $manifestContent
	}
	if ($SvcName -eq 'player-inbox') {
		$manifestContent = Add-PlayerInboxMatchSecretEnv -ManifestContent $manifestContent
	}

	# db 专属注解(docs/design/db-consumer-group-per-zone.md §4):Pod 模板上的 mmorpg.io/db-consumer-group / mmorpg.io/db-group-handoff-from
	# 取本 zone 的消费组计划(Get-DbConsumerGroupPlan,与 ConfigMap 同一份)。消费组键只在 ConfigMap 里,而 ConfigMap 只在进程启动时读:
	# 组名或交接对方组一变,注解随之变 → 模板变 → db 滚动(keep 收尾时对方组变为 none 也靠它滚一次)。注解只触发滚动,不作能力证明。
	# 走 manifest 里的占位而不是 Add-PodTemplateAnnotation:db.yaml 的 Pod 模板已有 prometheus 注解段。占位缺失即 throw(同 player-inbox
	# 的口令指纹占位):没有它,只改消费组的部署 apply 是 no-op,切组 / 收尾都静默不生效。替换完仍残留 PLACEHOLDER_DB_ 同样 throw。
	# 服务名判断留在 -and 左边 / 块最外层(理由同下面 login 那段:k8s_migrate_gate.tests.ps1 按 AST 单独抽出本函数跑别的服务)。
	# 本块是 db 专属注解替换的唯一位置,④ 的 RetryRouting 注解(如需要)用同样的占位手法放进这个块。
	if ($SvcName -eq 'db') {
		$dbConsumerGroupPlan = Get-DbConsumerGroupPlan -CurrentZoneId $CurrentZoneId
		$dbGroupHandoffFromAnnotation = if ([string]::IsNullOrEmpty($dbConsumerGroupPlan.HandoffFrom)) { 'none' } else { $dbConsumerGroupPlan.HandoffFrom }
		foreach ($dbPlaceholder in @('PLACEHOLDER_DB_CONSUMER_GROUP', 'PLACEHOLDER_DB_GROUP_HANDOFF_FROM')) {
			if (-not $manifestContent.Contains($dbPlaceholder)) {
				throw "服务 $SvcName 的 manifest 缺少占位 ${dbPlaceholder}(Pod 模板注解 mmorpg.io/db-consumer-group / mmorpg.io/db-group-handoff-from):没有它,只改消费组的部署不会滚动 db,切组与收尾都静默不生效。"
			}
		}
		$manifestContent = $manifestContent.Replace('PLACEHOLDER_DB_CONSUMER_GROUP', $dbConsumerGroupPlan.Group).Replace('PLACEHOLDER_DB_GROUP_HANDOFF_FROM', $dbGroupHandoffFromAnnotation)
		if ($manifestContent.Contains('PLACEHOLDER_DB_')) {
			throw "内部错误:服务 $SvcName 的 manifest 替换消费组注解后仍残留 PLACEHOLDER_DB_ 占位,拒绝部署(本块是 db 专属占位的唯一替换处,新增占位要在这里一并替换)。"
		}
	}

	# login 的削峰指纹(G4b,$script:PeakShavingHashAnnotation):排队开时给 Pod 模板写一条注解,关时不写。Queue 段与排队密钥都只在
	# ConfigMap 里,而 ConfigMap 只在进程启动时读:只改 ConfigMap 时 Pod 模板不变、apply 是 no-op,login 一直按旧配置跑 ——
	# -LoginQueue off 止血、轮换排队密钥都不生效。指纹输入 = Queue 段文本 + 排队密钥:任一变化 → 模板变 → 滚动;
	# 开 → 关时注解消失,同样滚动。只改 Redis 口令时 login 不滚动是既有缺口(go-svc 的 ConfigMap 都没有内容指纹),不在这里修。
	# 条件里服务名判断必须留在 -and 左边:本函数会被 k8s_migrate_gate.tests.ps1 按 AST 单独抽出来跑非 login 服务,那里
	# 右边的变量未定义、New-LoginQueueYaml 也没有加载,靠短路才不会求值。不要调换两边顺序。
	# login 的 manifest 在 Pod 模板里没有 annotations: 段(已有的话 Add-PodTemplateAnnotation 会 throw,要并进这一次调用)。
	# login 微信登录(-LoginWeChatAppId,见 $script:LoginOAuthSecretName 处的说明):AppSecret 只经 Secret login-oauth 进容器,env 走
	# secretKeyRef(不写 optional,Secret 缺失的 Pod 卡在 CreateContainerConfigError,而不是带着空密钥起来 —— login 对空密钥本来也拒启);
	# Pod 模板带密钥指纹注解 —— 只改 Secret 时模板不变、apply 是 no-op,旧 Pod 一直拿旧 AppSecret 调微信;指纹让轮换自动滚动。
	# 两条指纹必须一次写入(同一个 Pod 模板只能有一个 annotations: 段):排队开时削峰指纹恒排第一行(契约测试按这个位置取),
	# 微信指纹作附加项跟在后面;排队关时微信指纹自己排第一行。
	# Prometheus 抓取注解(prometheus.io/scrape / port / path)恒写,排在两条指纹之后、不抢第一行;它不能像 player-locator 那样
	# 手写进 manifest(手写的 annotations: 段会让上面的指纹写入 throw)。端口 9101 与 login ConfigMap 的 Prometheus 段、manifest 的
	# metrics 容器端口三处一致,契约测试 tools/scripts/tests/login_metrics_contract.tests.ps1 钉住。
	# 服务名判断留在 -and 左边 / 块最外层(理由同上)。
	$isLoginOAuth = ($SvcName -eq 'login' -and -not [string]::IsNullOrEmpty($LoginWeChatAppId))
	if ($SvcName -eq 'login') {
		$loginAnnotations = [ordered]@{}
		if ($script:LoginQueueOn) {
			$loginPeakShavingInput = (New-LoginQueueYaml -CurrentZoneId $CurrentZoneId) + "`n" + $script:LoginQueueTokenSecret
			$loginAnnotations[$script:PeakShavingHashAnnotation] = Get-InjectedSecretFingerprint -SecretName 'peak-shaving/login' -Value $loginPeakShavingInput
		}
		if ($isLoginOAuth) {
			$manifestContent = Add-GoSvcSecretEnv -SvcName $SvcName -ManifestContent $manifestContent `
				-EnvName $script:LoginWeChatAppSecretEnv -SecretName $script:LoginOAuthSecretName -SecretKey $script:LoginOAuthWeChatSecretKey
			$loginAnnotations[$script:LoginOAuthSecretHashAnnotation] = Get-InjectedSecretFingerprint -SecretName $script:LoginOAuthSecretName -Value $script:LoginWeChatAppSecret
		}
		$loginAnnotations['prometheus.io/scrape'] = 'true'
		$loginAnnotations['prometheus.io/port'] = '9101'
		$loginAnnotations['prometheus.io/path'] = '/metrics'
		if ($loginAnnotations.Count -gt 0) {
			$loginAnnotationKeys = @($loginAnnotations.Keys)
			$loginExtraAnnotations = [ordered]@{}
			foreach ($extraKey in ($loginAnnotationKeys | Select-Object -Skip 1)) {
				$loginExtraAnnotations[$extraKey] = $loginAnnotations[$extraKey]
			}
			$manifestContent = Add-PodTemplateAnnotation -SvcName $SvcName -ManifestContent $manifestContent `
				-Key $loginAnnotationKeys[0] -Value $loginAnnotations[$loginAnnotationKeys[0]] -AdditionalAnnotations $loginExtraAnnotations
		}
	}

	# -LoginDevPasswordAuth:先本地渲染(形状不对就在写操作之前 throw),再建 Secret —— secretKeyRef 不是 optional,
	# Secret 必须先于引用它的 Deployment 落地。
	$isLoginDevPassword = ($SvcName -eq 'login' -and $LoginDevPasswordAuth)
	if ($isLoginDevPassword) {
		$manifestContent = Add-GoSvcSecretEnv -SvcName $SvcName -ManifestContent $manifestContent `
			-EnvName (Get-ClientEntryContract).LoginDevPasswordSecretEnv `
			-SecretName $script:LoginDevPasswordSecretName -SecretKey $script:LoginDevPasswordSecretKey
		Apply-LoginDevPasswordSecret -Namespace $Namespace
	}
	# 微信 AppSecret 的 Secret 同理:env 与指纹已在上面本地渲染完,这里建 Secret,先于 ConfigMap 与 Deployment。
	if ($isLoginOAuth) {
		Apply-LoginOAuthSecret -Namespace $Namespace
	}

	# match 的排行榜快照开关(-MatchRatingSnapshotPublish)写进 Pod 模板注解:ConfigMap 只在进程启动时读,开关值必须改变模板,
	# 翻值才会滚动 match。走 manifest 里的占位而不是 Add-PodTemplateAnnotation(match.yaml 的 Pod 模板已有 prometheus 注解段)。
	# 条件里服务名判断留在 -and 左边的同一理由:k8s_migrate_gate.tests.ps1 按 AST 单独抽出本函数跑别的服务。
	if ($SvcName -eq 'match') {
		if (-not $manifestContent.Contains('PLACEHOLDER_MATCH_RATING_SNAPSHOT_PUBLISH')) {
			throw "服务 $SvcName 的 manifest 缺少占位 PLACEHOLDER_MATCH_RATING_SNAPSHOT_PUBLISH(Pod 模板注解 mmorpg.io/rating-snapshot-publish):没有它,只翻 -MatchRatingSnapshotPublish 时 match 不会滚动,开关不生效。"
		}
		$manifestContent = $manifestContent.Replace('PLACEHOLDER_MATCH_RATING_SNAPSHOT_PUBLISH', $MatchRatingSnapshotPublish)
	}

	# player-inbox 运维口令(见 $script:PlayerInboxAdminTokenSecretName 处的说明):先本地渲染,再建 Secret,与上面 login 同一顺序。
	#   - 口令已解析:env PLAYER_INBOX_ADMIN_TOKEN 经 secretKeyRef 注入(不写 optional,Secret 缺失的 Pod 卡在
	#     CreateContainerConfigError,而不是带着空口令起来);Pod 模板上的口令指纹占位换成指纹,口令一变即滚动;
	#   - 未解析(只可能是 dev 档未注入):不注入 env、不建 Secret,指纹占位写 disabled,Go 侧读到空串即停用 PlayerInboxAdmin。
	# 指纹走 manifest 里的占位而不是 Add-PodTemplateAnnotation:player-inbox.yaml 的 Pod 模板已有 prometheus 注解段。
	# 条件里服务名判断留在 -and 左边(理由同上面 login 那段:k8s_migrate_gate.tests.ps1 按 AST 单独抽出本函数跑别的服务)。
	$isPlayerInboxAdmin = ($SvcName -eq 'player-inbox' -and -not [string]::IsNullOrEmpty($script:PlayerInboxAdminToken))
	if ($SvcName -eq 'player-inbox') {
		$playerInboxAdminTokenHash = 'disabled'
		if ($isPlayerInboxAdmin) {
			$manifestContent = Add-GoSvcSecretEnv -SvcName $SvcName -ManifestContent $manifestContent `
				-EnvName $script:PlayerInboxAdminTokenEnv `
				-SecretName $script:PlayerInboxAdminTokenSecretName -SecretKey $script:PlayerInboxAdminTokenSecretKey
			$playerInboxAdminTokenHash = Get-InjectedSecretFingerprint -SecretName $script:PlayerInboxAdminTokenSecretName -Value $script:PlayerInboxAdminToken
		}
		if (-not $manifestContent.Contains('PLACEHOLDER_PLAYER_INBOX_ADMIN_TOKEN_HASH')) {
			throw "服务 $SvcName 的 manifest 缺少口令指纹占位 PLACEHOLDER_PLAYER_INBOX_ADMIN_TOKEN_HASH(Pod 模板注解 mmorpg.io/player-inbox-admin-token-hash):没有它,只轮换口令时 Pod 不会滚动,旧口令一直有效。"
		}
		$manifestContent = $manifestContent.Replace('PLACEHOLDER_PLAYER_INBOX_ADMIN_TOKEN_HASH', $playerInboxAdminTokenHash)
		if ($isPlayerInboxAdmin) {
			Apply-InjectedSecret -Namespace $Namespace -Name $script:PlayerInboxAdminTokenSecretName `
				-Key $script:PlayerInboxAdminTokenSecretKey -Value $script:PlayerInboxAdminToken -AppLabel $SvcName
		}
	}

	# mail 的 DSN、生产者密钥与资产密钥已在现役 manifest 的 secretKeyRef 中声明。

	# 通用资产通道(-AssetOps,见参数处说明):guild / trade 各自只拿自己那把签名密钥。先本地渲染,再建 Secret(与上面 player-inbox 同一顺序)。
	#   - on:env 经 secretKeyRef 注入(不写 optional),Pod 模板上的指纹占位换成指纹;
	#   - off:不注入、不建 Secret,指纹占位整行删掉(Pod 模板与引入开关之前逐字相同,不触发滚动)。
	# 指纹走 manifest 里的占位而不是 Add-PodTemplateAnnotation:两份 manifest 的 Pod 模板都已有 prometheus 注解段。
	# 条件里服务名判断留在 -and / -or 左边(理由同上:k8s_migrate_gate.tests.ps1 按 AST 单独抽出本函数跑 trade)。
	$isAssetOpCaller = ($SvcName -eq 'guild' -or $SvcName -eq 'trade')
	if ($isAssetOpCaller) {
		$assetOpSpec = $script:AssetOpCallers[$SvcName]
		$assetOpPlaceholderLine = '(?m)^[ \t]*' + [regex]::Escape($assetOpSpec.HashAnnotation) + ':[ \t]*"' + [regex]::Escape($script:AssetOpSecretHashPlaceholder) + '"[ \t]*\r?\n'
		if ([regex]::Matches($manifestContent, $assetOpPlaceholderLine).Count -ne 1) {
			throw "服务 $SvcName 的 manifest 应当恰好有一行资产密钥指纹占位 $($assetOpSpec.HashAnnotation): `"$($script:AssetOpSecretHashPlaceholder)`"(Pod 模板注解):没有它,只换密钥时 Pod 不会滚动,跨命令核对也无从判断这一侧开没开。"
		}
		if ($script:AssetOpsOn) {
			if ($SvcName -eq 'guild') {
				$manifestContent = $manifestContent.Replace('name: domain-service-secrets, key: asset-guild', "name: $($assetOpSpec.SecretName), key: $($script:AssetOpSecretKey)")
			} else {
				$manifestContent = Add-GoSvcSecretEnv -SvcName $SvcName -ManifestContent $manifestContent `
					-EnvName $assetOpSpec.Env -SecretName $assetOpSpec.SecretName -SecretKey $script:AssetOpSecretKey
			}
			$manifestContent = $manifestContent.Replace($script:AssetOpSecretHashPlaceholder,
				(Get-InjectedSecretFingerprint -SecretName $assetOpSpec.SecretName -Value $script:AssetOpSecrets[$SvcName]))
			Apply-InjectedSecret -Namespace $Namespace -Name $assetOpSpec.SecretName -Key $script:AssetOpSecretKey `
				-Value $script:AssetOpSecrets[$SvcName] -AppLabel $SvcName
		}
		else {
			if ($SvcName -eq 'guild') {
				# Guild 模板的本域资产密钥是三行 inline secretKeyRef；off 只精确移除这一块，保留其 SQL/目录/邮件必需签名。
				$guildAssetEnv = '(?m)^(?<indent>[ \t]*)- name: MMORPG_ASSET_OP_SECRET_GUILD[ \t]*\r?\n\k<indent>  valueFrom:[ \t]*\r?\n\k<indent>    secretKeyRef: \{name: domain-service-secrets, key: asset-guild, optional: false\}[ \t]*\r?\n'
				if ([regex]::Matches($manifestContent, $guildAssetEnv).Count -ne 1) {
					throw 'Guild AssetOps off 要求唯一的本域资产 env 原块；模板改变或重复时拒绝，不能误删其它签名。'
				}
				$manifestContent = [regex]::Replace($manifestContent, $guildAssetEnv, '')
			}
			$manifestContent = [regex]::Replace($manifestContent, $assetOpPlaceholderLine, '')
		}
	}

	# gRPC 口准入(-NetworkPolicies on,只 guild / match):开放端口取本服务 manifest 里声明的其它全部 containerPort,gRPC 口取目录的 Port。
	# 先渲染(形状不对在写操作之前 throw),在 Deployment 之前 apply。
	$grpcPolicyYaml = $null
	if (($SvcName -eq 'guild' -or $SvcName -eq 'match') -and $script:NetworkPoliciesOn) {
		$grpcAllowList = $script:GrpcAllowLists[$SvcName]
		$declaredPorts = Get-ManifestContainerPorts -ManifestContent $manifestContent
		if ($declaredPorts -notcontains [int]$info.Port) {
			throw "服务 $SvcName 的 manifest 没有声明 gRPC 端口 $($info.Port)(`$GoSvcCatalogue 的 Port):NetworkPolicy 会保护一个它根本不监听的口。"
		}
		$grpcPolicyYaml = New-GrpcAllowListNetworkPolicyYaml -PolicyName $grpcAllowList.PolicyName -TargetApps $grpcAllowList.TargetApps `
			-GrpcPort ([int]$info.Port) -OpenTcpPorts @($declaredPorts | Where-Object { $_ -ne [int]$info.Port }) -AllowedApps $grpcAllowList.AllowedApps
	}

	# Apply ConfigMap
	$cmYaml = New-GoSvcConfigMapYaml -SvcName $SvcName -CurrentZoneId $CurrentZoneId -CurrentClusterId $CurrentClusterId
	Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $cmYaml

	# 建表服务(目录条目带 MigrateJob,port-decisions D-14 第 4 条):<svc>-migrate Job 与 Deployment 同镜像、同 ConfigMap,
	# 必须排在上面的 ConfigMap 之后、下面的 Deployment 之前;门禁生效时(staging/prod 恒生效,dev 仅 -WaitReady)
	# Job 没 Complete 就 throw,Deployment 不 apply。
	if ($info.MigrateJob) {
		Apply-GoSvcMigrateJob -SvcName $SvcName -Namespace $Namespace -SvcImage $svcImage -PullPolicy $svcPullPolicy
	}

	if ($null -ne $grpcPolicyYaml) {
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $grpcPolicyYaml
	}

	Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $manifestContent

	if ($SvcName -eq 'login' -and -not $isLoginDevPassword) {
		# 开关关闭:ConfigMap 已不含 DevPasswordAuth,新 Deployment 也不再引用口令,删掉残留 Secret(放在 Deployment
		# apply 之后,不让新 Pod 因引用缺失的 Secret 卡在 CreateContainerConfigError)。
		Invoke-Kubectl -Args @("delete", "secret", $script:LoginDevPasswordSecretName, "-n", $Namespace, "--ignore-not-found")
	}
	if ($SvcName -eq 'login' -and -not $isLoginOAuth) {
		# 本次没给 -LoginWeChatAppId:ConfigMap 已不含 AuthProviders.WeChat,新 Deployment 也不再引用 AppSecret,删掉残留 Secret
		# (放在 Deployment apply 之后,理由同上)。不粘滞 —— 上次开过微信登录、这次没给,微信登录随新 Pod 一起关闭;
		# 线上开着而本次没给的情形已在写路径入口被 Assert-LoginAuthProviderClusterState 拦下:走到这里,要么线上本来就没开微信,
		# 要么已用 -ConfirmLoginAuthProviderChange 确认关闭。
		Invoke-Kubectl -Args @("delete", "secret", $script:LoginOAuthSecretName, "-n", $Namespace, "--ignore-not-found")
	}
	if ($SvcName -eq 'player-inbox' -and -not $isPlayerInboxAdmin) {
		# 本次未注入运维口令:新 Deployment 不再引用 Secret,删掉残留(放在 Deployment apply 之后,理由同上);
		# 不粘滞 —— 上次带口令部署过、这次没带,运维接口随新 Pod 一起停用。
		Invoke-Kubectl -Args @("delete", "secret", $script:PlayerInboxAdminTokenSecretName, "-n", $Namespace, "--ignore-not-found")
	}
	if ($isAssetOpCaller -and -not $script:AssetOpsOn) {
		# 资产通道关着:新 Deployment 不再引用签名密钥,删掉残留(放在 Deployment apply 之后,理由同上;不粘滞)。
		Invoke-Kubectl -Args @("delete", "secret", $script:AssetOpCallers[$SvcName].SecretName, "-n", $Namespace, "--ignore-not-found")
	}
	if (($SvcName -eq 'guild' -or $SvcName -eq 'match') -and -not $script:NetworkPoliciesOn) {
		# 准入策略关着:删掉以前开着时留下的同名策略(删策略只会放开流量,放在哪里都不断流)。
		Invoke-Kubectl -Args @("delete", "networkpolicy", $script:GrpcAllowLists[$SvcName].PolicyName, "-n", $Namespace, "--ignore-not-found")
	}

	Write-Host "  [applied] $SvcName -> $svcImage (port $($info.Port) pullPolicy=$svcPullPolicy)"
}

# 全局池 Go 服务(目录里 Global = $true,目前是 match / chat / client-rpc-router / trade / friend / player-inbox / guild / mail / rank):部署到 $InfraNamespace 一次。
# trade / friend / player-inbox / guild / mail 各带一个 MigrateJob(共五个 Job):trade-migrate / friend-migrate / player-inbox-migrate /
# guild-migrate / mail-migrate 在 Apply-OneGoSvc 里先于各自的 Deployment 跑
# (staging/prod 恒等 Complete,dev 仅 -WaitReady 下等;D-14 第 4 条)。
# 由 Apply-Infra 调用,所以 infra-up / all-up 都会带上;zone-up 不碰它。
function Apply-GlobalGoSvcManifests {
	if ($SkipGoSvc) { return }
	if ([string]::IsNullOrWhiteSpace($GoSvcRegistry)) {
		Write-Host "[skip] Global Go services: -GoSvcRegistry not set, skipping."
		return
	}

	$globalNames = Get-GlobalGoSvcNames
	if ($globalNames.Count -eq 0) { return }

	Write-Host "Applying global Go micro-service manifests to namespace $InfraNamespace (registry=$GoSvcRegistry tag=$GoSvcTag)"
	foreach ($svcName in $globalNames) {
		# 全局服务没有"自己的 zone"。ZoneId 只决定 etcd 注册路径,gate 按非 zone-scoped
		# 前缀发现 match(cpp node_util.cpp IsZoneScopedNodeType 不含它),所以填哪个 zone
		# 都不影响路由;沿用命令行 -ZoneId 让 infra-up 与 zone-up 的取值口径一致。
		# ClusterId 同理但更硬:它是本集群的常量,全局池与 zone 服务必须同值,所以同样只认命令行 -ClusterId。
		Apply-OneGoSvc -SvcName $svcName -Namespace $InfraNamespace -CurrentZoneId $ZoneId -CurrentClusterId $ClusterId
	}
	if ($WaitReady) {
		foreach ($svcName in $globalNames) {
			# 与 Apply-OneGoSvc 同一判据:manifest 缺席 = 没部署,等它只会在 rollout status 上白白超时,
			# 拖垮整条 -WaitReady(Java 服务那段 skip-wait 同一理由)。
			if (-not (Test-Path (Join-Path $GoSvcManifestsDir $GoSvcCatalogue[$svcName].Manifest))) {
				Write-Host "  [skip-wait] ${svcName}: manifest missing; not deployed; skipping wait"
				continue
			}
			Wait-ForDeploymentReady -Namespace $InfraNamespace -DeploymentName $svcName
		}
	}
}

# gate.rate-limit 下要写的削峰阈值(G4b)。关 = $null(什么都不写,落回 jar 内的 enabled: false)。键是 Spring 属性名(kebab-case),
# 必须与 jar 内 application.yaml 的 gate.rate-limit.* 同名 —— 拼错的键不报错、只是不生效(契约测试逐键核对)。
# 只写这六项:不写 wave.*(开服波次要一个开服时刻,属于运营决策)、zone-overrides,也不写没有消费方的 queue-timeout-ms。
# 只依赖 $script:GatewayRateLimitOn 与 $script:PeakShavingLaunchTier:契约测试按 AST 把本函数抽出来单独调用。
function Get-GatewayRateLimitSettings {
	if (-not $script:GatewayRateLimitOn) { return $null }
	$t = $script:PeakShavingLaunchTier
	# release_preflight.ps1 的「rps ≥ 1、burst ≥ rps」只查 jar 内 application.yaml;K8s 上真正生效的是这里写进
	# ConfigMap 的值,同一条规则必须在这里再守一次,否则改坏常量表会把闸门焊死(rps 0 = 全量排队)或自相矛盾。
	foreach ($pair in @(@('ZoneDefaultRps', 'ZoneDefaultBurst'), @('IpRps', 'IpBurst'))) {
		$rps = [long]$t[$pair[0]]
		$burst = [long]$t[$pair[1]]
		if ($rps -lt 1 -or $burst -lt $rps) {
			throw "内部错误:PeakShavingLaunchTier.$($pair[0])=$rps / $($pair[1])=$burst 不满足 rps ≥ 1 且 burst ≥ rps。"
		}
	}
	if ([long]$t.AccountCooldownMs -lt 0) {
		throw "内部错误:PeakShavingLaunchTier.AccountCooldownMs=$($t.AccountCooldownMs) 不能为负。"
	}
	return [ordered]@{
		'enabled'             = $true
		'zone-default-rps'    = [long]$t.ZoneDefaultRps
		'zone-default-burst'  = [long]$t.ZoneDefaultBurst
		'ip-rps'              = [long]$t.IpRps
		'ip-burst'            = [long]$t.IpBurst
		'account-cooldown-ms' = [long]$t.AccountCooldownMs
	}
}

# gateway application.yaml 里挂在 gate: 下的整段 rate-limit 文本(削峰阈值在前、trusted-proxies 在后;两者都没有 = 空串)。
# ConfigMap 与 Pod 指纹共用这一处(New-JavaSvcConfigMapYaml、Apply-JavaSvcManifests):两处算出来不一致,指纹就失去意义。
function New-GatewayRateLimitBlockText {
	return New-GatewayRateLimitYaml -TrustedProxies $GatewayTrustedProxies -Indent '  ' -Settings (Get-GatewayRateLimitSettings)
}

function New-JavaSvcConfigMapYaml {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		# 本 zone 的 zoneId:gateway 的限流 Redis 按 zone 组走(-RedisSplit queue → redis-queue-g<N>,组来自 zones 配置)。
		[Parameter(Mandatory = $true)][int]$CurrentZoneId
	)

	$info = $JavaSvcCatalogue[$SvcName]
	$configMapName = $info.ConfigMap

	$gateTokenSecret = $script:GateTokenSecret
	$gatewayDbUser = $script:GatewayDbUser
	$gatewayDbPassword = $script:GatewayDbPassword

	# gate.rate-limit(D91 的 trusted-proxies + G4b 的削峰阈值):片段接在下面已有的 gate: 键下(不能另起一个 gate:),
	# 两者共用同一个 rate-limit: 键,只写 application.yaml 这一处、不另设 env(ingress_final §3 二选一)。
	#   - trusted-proxies:gateway 只采信这些网段转发来的 X-Forwarded-For。列表为空 = 不写 = 只信 socket 对端(fail-closed);
	#     非法 CIDR 由 New-GatewayRateLimitYaml throw(preflight 已先拒)。
	#   - 削峰阈值:按 -GatewayRateLimit / -ReleaseProfile 决定写不写(Get-GatewayRateLimitSettings);关 = 不写,落回 jar 内的 enabled: false。
	# 限流关且没给可信代理时整段为空,gateway 模板的产物与 G4b 之前逐字相同。
	$gatewayRateLimitBlock = ''
	# spring.data.redis.password 那一行(G4b)。Bucket4j 的 Lettuce 客户端只从这个属性取口令(RateLimitConfig.java 的
	# bucket4jRedisClient,空串不设)。release 档的 Redis 带 --requirepass(manifests/infra/redis.yaml),不写口令时限流的
	# Redis 调用过不了认证:要么每次调用都落进 AssignGateRateLimiter 的 catch 一律放行(fail-open),限流开了等于没开,
	# 要么 gateway 在建连接时就起不来(哪一种取决于 Lettuce 的握手行为,未实测)。写口令把两种都修掉。
	# 与 go-svc ConfigMap、本 ConfigMap 的数据源口令同为明文:同 namespace 的 go-svc-login-config 已经带同一个值,
	# 改走 Secret 不缩小暴露面。只看口令是否非空、与限流开关无关;空口令不写:dev 档(口令回落空串)的产物逐字不变。
	$gatewayRedisPasswordLine = ''
	# spring.data.redis.host(G4a 第 1a 批,-RedisSplit;唯一来源 Get-RedisPurposeTopology)。gateway 的 Redis 只用于 Bucket4j 限流桶
	# rl:zone / rl:ip(RateLimitConfig;ZoneHealthProbeService 注入了 StringRedisTemplate 但不读写任何键),所以跟着排队实例走:
	# queue → redis-queue-g<N>.<infra>,none → 核心 redis.<infra>(产物与拆分前逐字节相同)。端口与口令行不变:queue 实例同为 6379、
	# 用同一个可选 Secret redis-auth。探针分组不含 redis(java/gateway_node 的 management.endpoint.health.group),queue 实例不在时
	# 限流 fail-open,gateway 不会因此被判不就绪。
	$gatewayRedisHost = ''
	if ($SvcName -eq 'gateway') {
		$gatewayRedisHost = (Get-RedisPurposeTopology -CurrentZoneId $CurrentZoneId).Queue.Host
		$gatewayRateLimitBlock = New-GatewayRateLimitBlockText
		if (-not [string]::IsNullOrEmpty($gatewayRateLimitBlock)) { $gatewayRateLimitBlock = "`n" + $gatewayRateLimitBlock }
		if (-not [string]::IsNullOrEmpty($script:RedisPassword)) {
			$gatewayRedisPasswordLine = "`n      password: `"$($script:RedisPassword)`""
		}
	}
	# admin.api-key 只写成对容器环境变量的引用(值经 Secret 注入,见 $script:GatewayAdminApiKeyEnv 处)。挂载的
	# /app/config/application.yaml 优先于 jar 内那份,所以公开的 change-me-in-production 被这行盖掉;env 缺失时
	# @Value("${admin.api-key}") 解析不了嵌套占位符,gateway 拒启(fail-closed)。拼成 PowerShell 变量再嵌进下面的
	# 双引号 here-string,既不用转义 $,env 名也只有一处真相。
	$gatewayAdminApiKeyRef = '${' + $script:GatewayAdminApiKeyEnv + '}'
	# gateway 的 Hikari 连接池大小(G2b):从 java/gateway_node/src/main/resources/application.yaml 镜像并核过
	# (Get-MysqlPoolMirror);MySQL 连接总账读的是同一个 maximum-pool-size。挂载的 /app/config/application.yaml 与 jar 内那份
	# 按属性合并(Spring Boot 默认加载 ./config/,外部事实,未核实),不镜像时 jar 内的值也生效 —— 镜像是为了让集群值
	# 与总账输入同源、契约测试可钉。只在生成 gateway 自己的 ConfigMap 时读:auth 不连 MySQL,不该因为 gateway 的配置缺键而失败。
	$gatewayHikariMaxPool = ''
	$gatewayHikariMinIdle = ''
	$gatewayHikariConnectionTimeout = ''
	$gatewayJdbcTimeoutQuery = ''
	if ($SvcName -eq 'gateway') {
		$gatewayPoolMirror = Get-MysqlPoolMirror -Target 'gateway'
		$gatewayHikariMaxPool = $gatewayPoolMirror['spring.datasource.hikari.maximum-pool-size']
		$gatewayHikariMinIdle = $gatewayPoolMirror['spring.datasource.hikari.minimum-idle']
		$gatewayHikariConnectionTimeout = $gatewayPoolMirror['spring.datasource.hikari.connection-timeout']
		# 2026-10-16:JDBC 连接级超时(connectTimeout / socketTimeout,毫秒)从 jar 内 spring.datasource.url 的查询串镜像,
		# 生成器不自带常数;缺项或不是正整数即失败(fail-closed:不带一个没有读超时的 URL 上线)。理由见该文件 hikari 段的
		# 2026-10-16 补注(续)。ConfigMap 的 URL 其余部分(主机、库名、SSL 选项)仍按集群写,不镜像。
		$gatewayJarJdbcUrl = Get-AuthoritativeScalar -RelativePath 'java/gateway_node/src/main/resources/application.yaml' -KeyPath 'spring.datasource.url'
		$gatewayJarJdbcParams = @{}
		$gatewayJarJdbcQuery = ($gatewayJarJdbcUrl -split '\?', 2) | Select-Object -Skip 1 -First 1
		foreach ($pair in @("$gatewayJarJdbcQuery" -split '&')) {
			$kv = $pair -split '=', 2
			if ($kv.Count -eq 2) { $gatewayJarJdbcParams[$kv[0]] = $kv[1] }
		}
		foreach ($timeoutKey in @('connectTimeout', 'socketTimeout')) {
			if ("$($gatewayJarJdbcParams[$timeoutKey])" -notmatch '^[1-9][0-9]*$') {
				throw "生成 gateway ConfigMap 失败:java/gateway_node/src/main/resources/application.yaml 的 spring.datasource.url 缺少正整数的 $timeoutKey(实际 '$($gatewayJarJdbcParams[$timeoutKey])')。fail-closed:不生成没有 JDBC 连接级超时的 URL"
			}
		}
		$gatewayJdbcTimeoutQuery = '&connectTimeout={0}&socketTimeout={1}' -f $gatewayJarJdbcParams['connectTimeout'], $gatewayJarJdbcParams['socketTimeout']
	}

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
# 探针走独立管理端口(2026-10-15):actuator 用自己的连接器与线程池,业务线程被 MySQL / Redis 故障或不限流的端点耗尽时,
# liveness 不再跟着超时、把所有副本一起杀掉。端口与 deploy/k8s/manifests/java-svc/gateway.yaml 三个探针一致(契约测试钉住)。
# jar 内 application.yaml 刻意不写:本机 start_game.ps1 按 8081 取 /actuator/health。
management:
  server:
    port: 8082
spring:
  application:
    name: gateway-node
  datasource:
    # connectTimeout / socketTimeout 镜像 jar 内 spring.datasource.url 的查询串(2026-10-16,见上方生成处注释)。
    url: jdbc:mysql://mysql.${InfraNamespace}:3306/mmorpg?useSSL=false&allowPublicKeyRetrieval=true${gatewayJdbcTimeoutQuery}
    username: "${gatewayDbUser}"
    password: "${gatewayDbPassword}"
    # 连接池大小镜像 java/gateway_node/src/main/resources/application.yaml(单一真相),计入 MySQL 连接总账;
    # connection-timeout(取连接的等待上限,毫秒)同样镜像该文件。
    hikari:
      maximum-pool-size: ${gatewayHikariMaxPool}
      minimum-idle: ${gatewayHikariMinIdle}
      connection-timeout: ${gatewayHikariConnectionTimeout}
  data:
    redis:
      host: ${gatewayRedisHost}
      port: 6379${gatewayRedisPasswordLine}
etcd:
  endpoints: http://etcd.${InfraNamespace}:2379
gate:
  token-secret: "${gateTokenSecret}"${gatewayRateLimitBlock}
zone:
  probe:
    interval-ms: 5000
admin:
  api-key: "${gatewayAdminApiKeyRef}"
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
		[Parameter(Mandatory = $true)][string]$Namespace,
		# 只用于渲染 -GatewayIngressHost 里的 {zone}。
		[Parameter(Mandatory = $true)][string]$CurrentZoneName,
		# gateway 的限流 Redis 按 zone 组取地址(New-JavaSvcConfigMapYaml,-RedisSplit)。
		[Parameter(Mandatory = $true)][int]$CurrentZoneId
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
		$isGateway = ($svcName -eq 'gateway')

		# manifest 缺席就整条跳过,连 ConfigMap 也不 apply(与 Apply-OneGoSvc 同口径,不留没有 Deployment 消费的孤儿 ConfigMap;
		# 目录里的 auth 就是这种条目)。
		$manifestPath = Join-Path $JavaSvcManifestsDir $info.Manifest
		if (-not (Test-Path $manifestPath)) {
			Write-Warning "Java service manifest not found: $manifestPath – skipping $svcName (ConfigMap not applied)"
			continue
		}

		# 本服务要写的东西先全部在本地渲染完,再做任何集群写操作(与 Apply-OneGoSvc 同序):ConfigMap 生成器
		# (非法 CIDR)与 manifest 注入(形状不对)都 fail-closed,要在 ConfigMap / Secret 落地之前拦下,不留半截部署。
		$cmYaml = New-JavaSvcConfigMapYaml -SvcName $svcName -CurrentZoneId $CurrentZoneId
		$svcPullPolicy = Resolve-ImagePullPolicy -ImageRef $svcImage
		$manifestContent = (Get-Content $manifestPath -Raw) -replace 'PLACEHOLDER_IMAGE', $svcImage
		$manifestContent = $manifestContent -replace 'PLACEHOLDER_PULL_POLICY', $svcPullPolicy
		$manifestContent=Add-SqlWriterBootManifest -ManifestContent $manifestContent -Namespace $Namespace -SvcName $svcName -RuntimeKind java


		# gateway 管理面口令(admin.api-key)只经 Secret 进容器(见 $script:GatewayAdminApiKeyEnv 处的说明):env 走 secretKeyRef,
		# Pod 模板带口令指纹注解 —— 只改 Secret 时模板不变、apply 是 no-op,旧 Pod 会一直认旧口令;指纹让轮换自动滚动。
		# gateway 的 Ingress(D91)与 gateway Deployment 同处:只在 gateway 确实部署了才生成(manifest 缺席在上面就 continue 了)。
		# Ingress 只能引用同 namespace 的 Service,所以跟着 zone namespace 走。留空不生成,也不删已有的(见参数注释)。
		$ingressYaml = $null
		if ($isGateway) {
			$manifestContent = Add-GoSvcSecretEnv -SvcName $svcName -ManifestContent $manifestContent `
				-EnvName $script:GatewayAdminApiKeyEnv -SecretName $script:GatewayAdminApiKeySecretName -SecretKey $script:GatewayAdminApiKeySecretKey
			# 削峰指纹(G4b,$script:PeakShavingHashAnnotation):限流开时再带一条注解,关时不带。ConfigMap 只在 gateway 启动时读,
			# 只改 ConfigMap 时 Pod 模板不变、apply 是 no-op,旧 Pod 一直按旧配置跑 —— -GatewayRateLimit off 止血也就不生效。
			# 指纹输入 = 写进 ConfigMap 的整段 rate-limit 文本(阈值 + trusted-proxies)+ Redis 口令:开关 / 阈值 / 代理网段 / 口令
			# 任一变化 → 模板变 → 滚动;开 → 关时注解消失,同样滚动。口令必须在内:Bucket4j 的 Lettuce 客户端只在启动时读
			# spring.data.redis.password,轮换 MMORPG_REDIS_PASSWORD 而 gateway 不滚动,旧口令被 Redis 拒绝后限流一律 fail-open。
			# Get-InjectedSecretFingerprint 是通用的「域分隔串 + 值」SHA-256 截断(48 位,不可反推),域分隔串写用途。
			# 与口令指纹必须一次写入(同一个 Pod 模板只能有一个 annotations: 段),口令指纹恒排第一行。
			$peakShavingAnnotations = $null
			if ($script:GatewayRateLimitOn) {
				$peakShavingAnnotations = [ordered]@{}
				$peakShavingAnnotations[$script:PeakShavingHashAnnotation] = Get-InjectedSecretFingerprint -SecretName 'peak-shaving/gateway' `
					-Value ((New-GatewayRateLimitBlockText) + "`n" + $script:RedisPassword)
			}
			$manifestContent = Add-PodTemplateAnnotation -SvcName $svcName -ManifestContent $manifestContent `
				-Key $script:GatewayAdminApiKeyHashAnnotation `
				-Value (Get-InjectedSecretFingerprint -SecretName $script:GatewayAdminApiKeySecretName -Value $script:GatewayAdminApiKey) `
				-AdditionalAnnotations $peakShavingAnnotations
			if (-not [string]::IsNullOrWhiteSpace($GatewayIngressHost)) {
				$ingressYaml = New-GatewayIngressYaml -IngressHost $GatewayIngressHost -ZoneName $CurrentZoneName `
					-IngressClassName $GatewayIngressClassName -TlsSecret $GatewayIngressTlsSecret -ServicePort $info.HttpPort
			}
		}

		# 写操作:ConfigMap → Secret → Deployment(secretKeyRef 不是 optional,Secret 必须先于 Deployment 落地)→ Ingress。
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $cmYaml
		if ($isGateway) {
			Apply-InjectedSecret -Namespace $Namespace -Name $script:GatewayAdminApiKeySecretName -Key $script:GatewayAdminApiKeySecretKey `
				-Value $script:GatewayAdminApiKey -AppLabel "gateway"
		}
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $manifestContent

		Write-Host "  [applied] $svcName -> $svcImage (http=$($info.HttpPort) grpc=$($info.GrpcPort) pullPolicy=$svcPullPolicy)"

		if ($null -ne $ingressYaml) {
			Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $ingressYaml
			Write-Host "  [applied] gateway Ingress host=$($GatewayIngressHost.Replace('{zone}', $CurrentZoneName)) class=$GatewayIngressClassName tls=$(if ($GatewayIngressTlsSecret) { $GatewayIngressTlsSecret } else { '<none>' }) path=/api"
		}
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
					gateNodePortBase = $null
					redisQueueGroup = $null
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

			if ($trimmed -match "^gateNodePortBase\s*:\s*(\d+)$") {
				$currentZone.gateNodePortBase = [int]$matches[1]
				continue
			}

			# redisQueueGroup(G4a 第 1a 批):值原样留成字符串(去掉行尾注释与引号),合法性交给 ConvertTo-ZoneRedisQueueGroup 统一判 ——
			# 不像上面那样只认 \d+:负数、小数写错了要报错,不能因为正则没匹配上就被静默当成没写(= 组 0)。
			if ($trimmed -match "^redisQueueGroup\s*:(.*)$") {
				$rawGroup = ($matches[1] -replace '\s+#.*$', '').Trim().Trim('"', "'")
				$currentZone.redisQueueGroup = if ($rawGroup -eq '') { $null } else { $rawGroup }
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

	# 一个 zone 的 redisQueueGroup(G4a 第 1a 批):没写(或 null)= 0;只认 ≥ 0 的整数 —— JSON / YAML 的整数,或只含数字的字符串
	# (回落解析器读出来的都是字符串)。负数、小数、布尔与其它字符串一律 throw:组号决定 login / gateway 连哪个 redis-queue-g<N>,
	# 写错了当成 0 就会把这个 zone 悄悄并进别的组。
	function ConvertTo-ZoneRedisQueueGroup {
		param(
			[AllowNull()]$Value,
			[Parameter(Mandatory = $true)][string]$ZoneLabel
		)

		if ($null -eq $Value) { return 0 }
		$text = $null
		if ($Value -is [string]) {
			$text = $Value.Trim()
		}
		elseif ($Value -is [int] -or $Value -is [long] -or $Value -is [int16] -or $Value -is [byte] -or $Value -is [sbyte] -or
			$Value -is [uint16] -or $Value -is [uint32] -or $Value -is [uint64]) {
			$text = [string]$Value
		}
		$group = 0
		if ($null -eq $text -or $text -notmatch '^[0-9]+$' -or -not [int]::TryParse($text, [ref]$group)) {
			throw "zones 配置 $Path 里 zone '$ZoneLabel' 的 redisQueueGroup='$Value' 不合法:必须是 ≥ 0 的整数(不写 = 0)。组号决定 login / gateway 连哪个 redis-queue-g<N>,不替你猜。"
		}
		return $group
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

		# gateNodePortBase(D88):external + NodePort 时 gate-<i> 的 nodePort = base + i。没写就落回命令行 -GateNodePortBase,
		# 并记下"未显式写":多 zone 部署时 preflight 要求每个 zone 显式写,否则各 zone 落在同一段 nodePort 上。
		$zoneGateNodePortBase = $GateNodePortBase
		$zoneGateNodePortBaseExplicit = $false
		if ($null -ne $zone.gateNodePortBase) {
			$zoneGateNodePortBase = [int]$zone.gateNodePortBase
			$zoneGateNodePortBaseExplicit = $true
		}

		# redisQueueGroup(可选,缺省 0):-RedisSplit queue 时本 zone 的 login / gateway 连 redis-queue-g<N>(G4a 第 1a 批)。
		# JSON、ConvertFrom-Yaml 与上面的回落解析器三条路都在这里统一校验,非法值(负数、非整数)throw。
		$zoneRedisQueueGroup = ConvertTo-ZoneRedisQueueGroup -Value $zone.redisQueueGroup -ZoneLabel ([string]$zone.name)

		$result += [pscustomobject]@{
			name = [string]$zone.name
			zoneId = [int]$zone.zoneId
			redisQueueGroup = $zoneRedisQueueGroup
			centre = $zoneCentre
			gate = $zoneGate
			scene = $zoneScene
			scene_world = $zoneSceneWorld
			scene_instance = $zoneSceneInstance
			scene_legacy_explicit = $sceneLegacyExplicit
			gateNodePortBase = $zoneGateNodePortBase
			gate_node_port_base_explicit = $zoneGateNodePortBaseExplicit
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
		Write-Warning "zone ${ZoneLabel}: replicas.scene and scene_world/scene_instance coexist; split mode is active and legacy scene=$LegacySceneReplicas is ignored. Remove the scene key from the zones configuration."
	}
	if ($world -le 0) {
		Write-Warning "zone ${ZoneLabel}: scene_world=0; this zone has no world-scene nodes. With StrictNodeTypeSeparation=true, world-scene creation returns ErrNoNodeForPurpose."
	}
	if ($instance -le 0) {
		Write-Warning "zone ${ZoneLabel}: scene_instance=0; this zone has no instance-scene nodes. With StrictNodeTypeSeparation=true, instance/battleground creation returns ErrNoNodeForPurpose."
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
		# 与 -ZoneId 不同,它不来自 zones 配置:集群常量,调用方一律传命令行 -ClusterId。
		[Parameter(Mandatory = $true)][int]$CurrentClusterId,
		[Parameter(Mandatory = $true)][int]$CurrentCentreReplicas,
		[Parameter(Mandatory = $true)][int]$CurrentGateReplicas,
		[Parameter(Mandatory = $true)][int]$CurrentSceneReplicas,
		[int]$CurrentSceneWorldReplicas = -1,
		[int]$CurrentSceneInstanceReplicas = -1,
		[bool]$CurrentSceneLegacyExplicit = $false,
		# external + NodePort 时 gate-<i> 的 nodePort 起点(D88):zone-up 取 -GateNodePortBase,all-up 取 zones 配置。
		[Parameter(Mandatory = $true)][int]$CurrentGateNodePortBase
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

	Write-Host "Applying zone deployment: zone=$CurrentZoneName zone_id=$CurrentZoneId cluster_id=$CurrentClusterId namespace=$namespace"
	Write-Host "Ops profile resolved: profile=$OpsProfile gate_service_type=$GateServiceType centre=$CurrentCentreReplicas gate=$CurrentGateReplicas"
	Write-Host "Scene orchestrator: $SceneOrchestrator -> $sceneKind"
	Write-Host "Scene pools resolved: $sceneSummary"
	$gateKind = if ($ClientEntryMode -eq "external") { "apps/v1 StatefulSet + per-ordinal Service" } else { "apps/v1 Deployment" }
	Write-Host "Client entry mode: $ClientEntryMode -> gate $gateKind"

	Ensure-Namespace -Namespace $namespace

	$configMapName = "node-config"
	$gateServiceName = "gate-entry"

	# external 的 gate 先在本地渲染:地址计划或模板校验失败要在删除 Deployment 形态之前 throw。
	# 再做服务端预演(非 DryRun):nodePort 被其他 zone 占用这类只有集群知道的冲突,也要在删除之前暴露。
	$externalGateManifests = $null
	if ($ClientEntryMode -eq "external") {
		$externalGateManifests = New-ExternalGateManifests -CurrentZoneName $CurrentZoneName -Replicas $CurrentGateReplicas `
			-NodePortBase $CurrentGateNodePortBase -ConfigMapName $configMapName
		if (-not $DryRun) {
			Test-ExternalGateServerSide -Namespace $namespace -Manifests $externalGateManifests
		}
	}

	# 模式切换:再删"另一种形态"的 gate(Deployment 与 StatefulSet 同名不同 kind,apply 不会互相回收,两套 gate 会同时
	# 注册、同时收玩家),以及本模式不该有的 Service / PDB 残留(含 D90 不再生成的 gate-entry)。会踢人的条目在集群里确实
	# 存在而没给 -AllowDisruptiveSwitch 时整条拒绝、一个都不删;放在本 zone 任何配置与工作负载 apply 之前,被拒时本 zone 未被改动。
	# 写路径入口(Assert-ClientEntryClusterState)已对全部 zone 与 battle 先判过一次,这里是删除前的纵深防御。
	$gateObsoleteResources = Get-GateObsoleteResources -ClientEntryMode $ClientEntryMode -GateReplicas $CurrentGateReplicas
	Remove-ClientEntryObsoleteResources -Namespace $namespace -Resources $gateObsoleteResources `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -AllowDisruptiveSwitch:$AllowDisruptiveSwitch -DryRun:$DryRun

	# Agones 模式:必须先在本 namespace 里建 agones-sdk 的 SA + RoleBinding,
	# 否则 GameServer 控制器建 Pod 会被 API server 拒掉,GameServer 全进 Error。
	# 必须在 Fleet 之前 apply —— 反过来的话第一批 GameServer 会先失败一轮。
	if ($SceneOrchestrator -eq "agones") {
		$rbacYaml = New-AgonesSdkRbacYaml -Namespace $namespace
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $rbacYaml
	}

	$configMapYaml = New-NodeConfigMapYaml -CurrentZoneId $CurrentZoneId -CurrentClusterId $CurrentClusterId -ConfigName $configMapName
	Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $configMapYaml

	# 日志 sidecar 的配置必须先于 gate / scene 落地,否则 Pod 因引用不存在的 ConfigMap 卡在 ContainerCreating。
	if ($script:CppLogSidecarEnabled) {
		Write-CppLogSidecarNotice -Namespace $namespace
		$sidecarConfigYaml = New-CppLogSidecarConfigMapYaml -ConfigName $script:CppLogSidecarConfigMapName -CurrentZoneName $CurrentZoneName
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $sidecarConfigYaml
	}

	if ($ClientEntryMode -eq "external") {
		Apply-ExternalGate -Namespace $namespace -Manifests $externalGateManifests
	}
	else {
		$gateYaml = New-NodeDeploymentYaml -NodeName "gate" -Replicas $CurrentGateReplicas -RpcPort 18000 -StartCommand "./gate" -ConfigMapName $configMapName -CurrentZoneName $CurrentZoneName
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $gateYaml
	}

	# scene 的 gRPC 口准入与资产验签密钥(-NetworkPolicies / -AssetOps,见参数处说明),都先于 scene 工作负载落地:
	# 验签密钥的 secretKeyRef 不写 optional,Secret 缺失的 Pod 卡在 CreateContainerConfigError;策略先到,新 Pod 从第一秒起就受保护。
	$sceneRpcPort = 20000
	if ($script:NetworkPoliciesOn) {
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) `
			-InputContent (New-SceneGrpcNetworkPolicyYaml -SceneRpcPort $sceneRpcPort -CurrentZoneName $CurrentZoneName)
	}
	if ($script:AssetOpsOn) {
		foreach ($caller in $script:AssetOpCallers.Keys) {
			$assetOpSpec = $script:AssetOpCallers[$caller]
			Apply-InjectedSecret -Namespace $namespace -Name $assetOpSpec.SecretName -Key $script:AssetOpSecretKey `
				-Value $script:AssetOpSecrets[$caller] -AppLabel 'scene'
		}
	}

	foreach ($scenePool in $scenePlan) {
		if ($SceneOrchestrator -eq "agones") {
			$sceneYaml = New-SceneFleetYaml `
				-FleetName $scenePool.Name `
				-Replicas $scenePool.Replicas `
				-RpcPort $sceneRpcPort `
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
				-RpcPort $sceneRpcPort `
				-StartCommand "./scene" `
				-ConfigMapName $configMapName `
				-SceneNodeType $scenePool.SceneNodeType `
				-CurrentZoneName $CurrentZoneName `
				-TerminationGracePeriodSeconds $SceneTerminationGracePeriodSeconds
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
	# 关着的开关不粘滞:删掉以前开着时留下的策略与验签密钥(放在 scene apply 之后,新模板已不再引用 Secret)。
	if (-not $script:NetworkPoliciesOn) {
		Invoke-Kubectl -Args @("delete", "networkpolicy", $script:GrpcAllowLists['scene'].PolicyName, "-n", $namespace, "--ignore-not-found")
	}
	if (-not $script:AssetOpsOn) {
		foreach ($caller in $script:AssetOpCallers.Keys) {
			Invoke-Kubectl -Args @("delete", "secret", $script:AssetOpCallers[$caller].SecretName, "-n", $namespace, "--ignore-not-found")
		}
	}

	# 单一 gate-entry(D90):只在 podip 且 gate 副本数恰为 1 时生成。副本数 ≥2 时 Service 随机分流,约一半票据会被
	# token_gate_node_mismatch 拒绝;external 由每序号 Service 取代。不生成时的残留已由上面的 Remove-ClientEntryObsoleteResources 删除。
	if (Test-GateEntryServiceWanted -ClientEntryMode $ClientEntryMode -GateReplicas $CurrentGateReplicas) {
		$gateServiceYaml = New-GateServiceYaml -ServiceName $gateServiceName
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $gateServiceYaml
	}

	Apply-GoSvcManifests -Namespace $namespace -CurrentZoneId $CurrentZoneId -CurrentClusterId $CurrentClusterId

	Apply-JavaSvcManifests -Namespace $namespace -CurrentZoneName $CurrentZoneName -CurrentZoneId $CurrentZoneId

	$sceneDeploymentNames = @($scenePlan | Where-Object { $_.Replicas -gt 0 } | ForEach-Object { $_.Name })
	Wait-ForZoneReady -Namespace $namespace -SceneDeploymentNames $sceneDeploymentNames -GateReplicas $CurrentGateReplicas

	if ($scenePlan.Count -gt 1) {
		Write-Host "NOTE: Switching from a legacy single pool to split mode does not automatically remove the old 'scene' workload during apply. After the new pools are Ready, run manually: kubectl -n $namespace delete deployment scene"
	}
	if ($SceneOrchestrator -eq "agones") {
		# 换编排方式会换 kind:apply 只会新建 Fleet,不会回收同名 Deployment,
		# 两者同时在线 = 同一个 zone 里有两套 scene 进程、两套容量语义。
		Write-Host "NOTE: Switching to Agones orchestration does not automatically remove an existing Deployment with the same name. After the Fleet is ready, run manually:"
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
	# sts / pdb / ingress:-ClientEntryMode external 下 gate 是 StatefulSet(带 PDB),gateway 可能带 Ingress,只看 deploy 会漏掉它们。
	Invoke-Kubectl -Args @("get", "deploy,sts,po,svc,pdb,ingress,cm", "-n", $namespace) -AllowFailure
}

<#
.SYNOPSIS
	infra-up 要预建 zone_<id>_db 的 zone 列表:zones 配置里的全部 zone ∪ 本次 -ZoneId。

.DESCRIPTION
	zones 配置走 Resolve-ZonesConfigPath(-ZonesConfigPath / deploy/k8s/zones.json)。
	文件不存在时**只警告不阻断**:infra-up 本来就不依赖 zones.json(zone-up 才要),
	此时至少还有 -ZoneId 这一个库,不能让"没有 zones.json"把整个 infra-up 拦下。
#>
function Get-InfraZoneIds {
	$ids = [System.Collections.Generic.List[int]]::new()
	$ids.Add([int]$ZoneId)

	$zonesPath = Resolve-ZonesConfigPath
	if (Test-Path $zonesPath) {
		foreach ($zone in (Get-ZonesFromJson -Path $zonesPath)) {
			$zid = [int]$zone.zoneId
			if (-not $ids.Contains($zid)) { $ids.Add($zid) }
		}
	}
	else {
		Write-Warning "Zones configuration missing ($zonesPath); mysql-init-sql only creates zone_${ZoneId}_db for -ZoneId=$ZoneId. For other zones, add them to the zones configuration and recreate the MySQL PVC, or let db create their databases under the dev profile."
	}

	return @($ids | Sort-Object)
}

<#
.SYNOPSIS
	把 deploy/mysql-init/*.sql 打成 ConfigMap mysql-init-sql,供 mysql.yaml 挂到
	/docker-entrypoint-initdb.d —— 与 compose 的 ./mysql-init 卷同一份源。

.DESCRIPTION
	以前 K8s 侧的 mysql.yaml 只有 my.cnf 一个 ConfigMap,mysql-init 里的
	zone_config / guild / friend 建表与 zone_N_db 预建全都只在 compose 生效:
	K8s 上 Java 网关查 zone_config 报表不存在,db 服务因 zone_<id>_db 不存在拒启。

	除仓库里的 sql 原样带入(CRLF 统一成 LF)外,再按 zones 配置与本次 -ZoneId
	生成 01_k8s_zone_dbs.sql 预建每个 zone 的库并授权给 appuser。文件名以 01_
	开头保证排在 00_init_zone_dbs.sql 之后、gateway_tables.sql 之前(initdb 按
	文件名顺序执行);CREATE DATABASE IF NOT EXISTS 与 00_ 里的 zone_1/2 重叠无害。

	另生成 02_k8s_global_db.sql 预建 data_service 的全局库 $GlobalDbName(全集群一份,
	不按 zone 拆):data_service 启动期 / -migrate 要在里面 CREATE TABLE 全局表,库不在或
	账号无权就只剩 "schema auto-migrate failed",AllocateIdSegment 随之不可用。
	仓库里 00_init_zone_dbs.sql 顺带建的 testdb 只服务本地 compose 的
	go/data_service/etc/data_service.yaml,在 K8s 上是一个没人用的空库,无害。

	新全局服务的独占库(port-decisions D-14 第 5 条,目前是聚宝斋 trade 的 mmorpg_trade、好友 friend 的 mmorpg_friend、玩家收件箱
	player-inbox 的 mmorpg_player_inbox、帮会 guild 的 mmorpg_guild 与邮件 mail 的 mmorpg_mail)**只登记在 00_init_zone_dbs.sql**,
	由上面的循环原样带进本 ConfigMap;这里**不要**再生成第二份建库 sql。
	02_k8s_global_db.sql 是因为本地(testdb)与 K8s(mmorpg_global)库名不同才单独生成的,不是范式。
	表不在 initdb 里建:trade 的表由 manifests/go-svc/trade-migrate.yaml、friend 的表由 manifests/go-svc/friend-migrate.yaml、
	player-inbox 的表由 manifests/go-svc/player-inbox-migrate.yaml、guild 的表由 guild-migrate.yaml、mail 的表由 mail-migrate.yaml
	这几个 Job 建(Apply-GoSvcMigrateJob)。

	注意 initdb 只在数据目录为空的首次启动执行,PVC 已有数据时改 sql 不会重跑
	(见 mysql.yaml 里 mysql-init 卷的注释)。
#>
function New-MysqlInitConfigMapYaml {
	$initDir = Join-Path $RepoRoot "deploy\mysql-init"
	if (-not (Test-Path $initDir)) {
		throw "生成 mysql-init-sql ConfigMap 失败:找不到 $initDir(fail-closed:没有 initdb 脚本的 MySQL 等于没有 zone_config / zone_<id>_db)"
	}

	$entries = [ordered]@{}
	foreach ($file in (Get-ChildItem -Path $initDir -Filter "*.sql" -File | Sort-Object Name)) {
		# CRLF → LF:\r 会原样落进容器,mysql 客户端把 "utf8mb4;\r" 当成语句的一部分报语法错。
		$entries[$file.Name] = ((Get-Content -Path $file.FullName -Raw) -replace "`r`n", "`n")
	}
	if ($entries.Count -eq 0) {
		throw "生成 mysql-init-sql ConfigMap 失败:$initDir 下没有任何 *.sql"
	}

	$zoneIds = Get-InfraZoneIds
	$zoneSql = @(
		"-- 由 k8s_deploy.ps1 Apply-Infra 生成,不要手改:按 zones 配置 + -ZoneId 预建每个 zone 的库。",
		"-- zones = $($zoneIds -join ',')",
		""
	)
	foreach ($id in $zoneIds) {
		$zoneSql += ('CREATE DATABASE IF NOT EXISTS `zone_{0}_db`;' -f $id)
		# 单引号字符串:双引号里的反引号会被 PowerShell 当转义符吃掉,SQL 里的库名引号就没了。
		$zoneSql += ('GRANT ALL PRIVILEGES ON `zone_{0}_db`.* TO ''appuser''@''%'';' -f $id)
	}
	$zoneSql += "FLUSH PRIVILEGES;"
	$entries["01_k8s_zone_dbs.sql"] = (($zoneSql -join "`n") + "`n")

	# 全局库(data_service SnapshotMySQL,见顶部 $GlobalDbName):全集群一份,不按 zone 拆。
	# GRANT 给 appuser 与 zone 库同一口径;dev 档 data-service 用注入的 root 本来就有权,
	# 这条 GRANT 是给 staging/prod 把 MMORPG_MYSQL_USER 注成 appuser 的部署用的。
	$globalSql = @(
		"-- 由 k8s_deploy.ps1 Apply-Infra 生成,不要手改:data_service 全局库",
		"-- (transaction_log / player_snapshot / rollback_audit_log / id_segment 等全局表由 data_service 迁移建,清单见其 internal/store/schema.go)。",
		"-- 全集群一份,不按 zone 拆;go-svc-data-service-config 的 SnapshotMySQL.DBName 指向它。",
		"",
		('CREATE DATABASE IF NOT EXISTS `{0}`;' -f $GlobalDbName),
		('GRANT ALL PRIVILEGES ON `{0}`.* TO ''appuser''@''%'';' -f $GlobalDbName),
		"FLUSH PRIVILEGES;"
	)
	$entries["02_k8s_global_db.sql"] = (($globalSql -join "`n") + "`n")

	# 每个文件一个 data 键,内容用 YAML 字面块(|)带入:sql 里的反引号 / 引号 / 冒号
	# 在字面块里都是纯文本,不需要转义;空行保留(YAML 允许字面块内空行)。
	$dataBlock = foreach ($kv in $entries.GetEnumerator()) {
		$body = @($kv.Value.TrimEnd("`n") -split "`n" | ForEach-Object {
			if ($_.Length -gt 0) { "    $_" } else { "" }
		}) -join "`n"
		"  $($kv.Key): |`n$body"
	}

	return @"
apiVersion: v1
kind: ConfigMap
metadata:
  name: mysql-init-sql
data:
$($dataBlock -join "`n")
"@
}

<#
.SYNOPSIS
	预建按分区契约寻址的 topic:data_service 的两个审计 topic(transaction_log / player_snapshot),
	以及控制面命令 topic(gate-cmd_g<N> / scene-cmd_g<N>)。

.DESCRIPTION
	控制面命令 topic(docs/design/control-plane-topic-partitioning-20260908.md):
	gate/scene 不再一个节点一个 topic,而是按 partition = node_id % P 各自 assign 一个分区。
	分区数少一个,一批 node_id 就永远收不到命令,而且 Kafka 不报错 —— 所以它和审计 topic
	一样必须在任何节点起来之前按契约建好,分区数取自 bin/etc/base_deploy_config.yaml
	(gate/scene 消费者启动时拿去和 broker 核对的是同一份数字)。

	broker(manifests/infra/kafka.yaml)开着 auto.create.topics.enable 且 num.partitions=1:
	C++ scene 一发消息就会把 topic 自动建成 1 分区,而 data_service 的 kafkautil.EnsureTopics
	拿 6 / 3 的契约去比只会永远报 "partition contract mismatch",两条落库消费者一条都起不来,
	唯一症状是每 30s 一条 Error 日志。所以 topic 必须在**任何 scene 起来之前**由部署侧建好:
	infra-up 在 $infraManifests 里的全部 infra manifest 之后、任何 zone 之前 apply 这个 Job;-WaitReady 会等它 Complete。

	契约值(基名 / 分区数 / 保留期 / 代号)全部取自 go/data_service/etc/data_service.yaml,
	与 data-service ConfigMap 同源;有效 topic 名 = <基名>_g<TopicGeneration>,与 Go 侧
	EffectiveTransactionLogTopic() 同一条规则。改分区数 = TopicGeneration +1(新 topic),
	绝不原地扩分区(重映射 key=player_id 的哈希,同一玩家的流水顺序就断了)。

	Job 幂等(kafka-topics.sh --if-not-exists,再 --describe 核对 PartitionCount,不符即失败;
	retention.ms 每次都用 kafka-configs.sh --alter 重新声明,不只在建 topic 那一刻生效)。
	每次都先删旧 Job 再 apply:Job 的 template 不可变,而且必须能重跑 —— 第一次从旧 emptyDir
	Deployment 切到 StatefulSet+PVC(R06)、`infra-down` 删过 namespace(PVC 一起没)、
	或有人手工删过 topic 之后,topic 都是空的。单独的 infra-kafka-topics 命令就是干这个的。
#>
function Apply-KafkaTopicInitJob {
	$manifestName = "kafka-topic-init.yaml"
	$path = Join-Path $InfraManifestsDir $manifestName
	if (-not (Test-Path $path)) {
		throw "Infra manifest not found: $path(fail-closed:没有预建 Job,scene 会把审计 topic 自动建成 1 分区)"
	}

	$dsYaml = 'go/data_service/etc/data_service.yaml'
	$generation = [int](Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.TopicGeneration')
	if ($generation -le 0) { $generation = 1 }   # 与 Go 侧 topicForGeneration 一致:0 按第一代处理
	$txTopic        = "{0}_g{1}" -f (Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.TransactionLogTopic'), $generation
	$txPartitions   = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.TransactionLogPartitions'
	$snapTopic      = "{0}_g{1}" -f (Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.SnapshotTopic'), $generation
	$snapPartitions = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.SnapshotPartitions'
	$retentionMs    = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'Kafka.RetentionMs'

	# 控制面命令 topic 的分区契约(docs/design/control-plane-topic-partitioning-20260908.md)。
	# 权威值在 C++ 侧的 bin/etc/base_deploy_config.yaml —— 那是 gate/scene 消费者启动时
	# 拿去和 broker 核对的同一份数字。这里读它、而不是在脚本里另写一个常数:
	# 预建的分区数与消费者 assign 的分区号必须同源,否则命令会落到没人 assign 的分区上,
	# 静默全丢且 Kafka 不报错。
	$baseDeployYaml   = 'bin/etc/base_deploy_config.yaml'
	$cmdPartitions    = [int](Get-AuthoritativeScalar -RelativePath $baseDeployYaml -KeyPath 'Kafka.CommandTopicPartitions')
	$cmdGeneration    = [int](Get-AuthoritativeScalar -RelativePath $baseDeployYaml -KeyPath 'Kafka.CommandTopicGeneration')
	if ($cmdPartitions -le 0) {
		throw "bin/etc/base_deploy_config.yaml 的 Kafka.CommandTopicPartitions 必须 > 0(fail-closed:分区数是寻址协议)"
	}
	if ($cmdGeneration -le 0) { $cmdGeneration = 1 }   # 与两端 Normalize* 一致:0 按第一代处理
	$gateCmdTopic  = "gate-cmd_g$cmdGeneration"
	$sceneCmdTopic = "scene-cmd_g$cmdGeneration"

	Write-Host "Kafka audit topic bootstrap: $txTopic(partitions=$txPartitions) $snapTopic(partitions=$snapPartitions) retention_ms=$retentionMs generation=g$generation"
	Write-Host "Kafka command topic bootstrap: $gateCmdTopic / $sceneCmdTopic (partitions=$cmdPartitions)"

	# Job 名固定,重跑必须先删:template 不可变,apply 同名 Job 会被拒。--ignore-not-found:首次是 no-op。
	Invoke-Kubectl -Args @("delete", "job", "kafka-topic-init", "-n", $InfraNamespace, "--ignore-not-found")

	$content = Get-Content -Path $path -Raw
	$content = $content.Replace("__INFRA_NAMESPACE__", $InfraNamespace)
	$content = $content.Replace("__KAFKA_AUDIT_TX_TOPIC__", $txTopic)
	$content = $content.Replace("__KAFKA_AUDIT_TX_PARTITIONS__", [string]$txPartitions)
	$content = $content.Replace("__KAFKA_AUDIT_SNAPSHOT_TOPIC__", $snapTopic)
	$content = $content.Replace("__KAFKA_AUDIT_SNAPSHOT_PARTITIONS__", [string]$snapPartitions)
	$content = $content.Replace("__KAFKA_AUDIT_RETENTION_MS__", [string]$retentionMs)
	$content = $content.Replace("__KAFKA_GATE_COMMAND_TOPIC__", $gateCmdTopic)
	$content = $content.Replace("__KAFKA_SCENE_COMMAND_TOPIC__", $sceneCmdTopic)
	$content = $content.Replace("__KAFKA_COMMAND_PARTITIONS__", [string]$cmdPartitions)
	# 副本数与 kafka.yaml / 注入给 Go 服务的值同源(Get-KafkaTopology):单 broker 1,多 broker 3。
	$content = $content.Replace("__KAFKA_REPLICATION_FACTOR__", [string](Get-KafkaTopology).ReplicationFactor)

	# 与 Apply-Infra 同一条 fail-closed:任何双下划线大写占位没替换就拒绝 apply。
	$leftover = [regex]::Matches($content, '__[A-Z][A-Z0-9_]*__') | ForEach-Object { $_.Value } | Sort-Object -Unique
	if ($leftover.Count -gt 0) {
		throw "infra manifest $manifestName 里有未替换的占位:$($leftover -join ', ')。请在 Apply-KafkaTopicInitJob 里补对应的 Replace。"
	}

	Invoke-KubectlWithInputFile -Args @("apply", "-n", $InfraNamespace) -InputContent $content
}

<#
.SYNOPSIS
	渲染 battle 全局池的工作负载(纯渲染,不碰集群),返回 { Orchestrator; Health; Yaml }。

.DESCRIPTION
	客户端必须连房间所在的那个 battle 实例,不能由一个 Service 随机分流。podip 下是 POD_IP:20000(只有集群内可达);
	external 下 battle 自报逐实例的客户端地址:Agones Fleet 取 Dynamic 端口(D81),Deployment 形态取 hostPort 20000(D86)。
	由 Apply-Infra 开头调用(与 battle-node-config 一起生成),结果交给 Apply-BattlePool:health 推导(Resolve-BattleFleetHealth)
	与生成器的参数校验都可能 throw,必须早于任何基础设施写操作,更不能删了旧 battle 才发现新的生成不出来。
	不在写路径入口(Assert-ClientEntryDeployPreflight)渲染:Pod 模板要带 zone Redis 口令指纹(Get-CppZoneRedisAuthAnnotations),
	口令到 Initialize-InjectedSecrets 才解析,而入口预检排在密钥解析之前。
#>
function New-BattleWorkloadManifest {
	$health = $null
	if ($BattleOrchestrator -eq 'agones') {
		$health = Resolve-BattleFleetHealth -InitialDelaySeconds $AgonesHealthInitialDelaySeconds `
			-PeriodSeconds $AgonesHealthPeriodSeconds -FailureThreshold $AgonesHealthFailureThreshold
		$yaml = New-BattleFleetYaml -PodCommon (New-CppNodePodCommon -SidecarZoneLabel 'global') -Replicas $BattleReplicas `
			-RpcPort 20000 -StartCommand 'exec ./battle' -ConfigMapName 'battle-node-config' -ClientEntryMode $ClientEntryMode `
			-Health $health -BuildLabel (Get-ImageBuildLabel -Image $NodeImage) -ClientPublicHost $ClientPublicHost
	}
	elseif ($ClientEntryMode -eq 'external') {
		# Deployment + hostPort 20000:只用于验证与回退(D86),每个节点只能跑 1 个副本、没有忙碌保护(preflight 已警告)。
		$yaml = New-BattleHostPortDeploymentYaml -PodCommon (New-CppNodePodCommon -SidecarZoneLabel 'global') `
			-Replicas $BattleReplicas -RpcPort 20000 -StartCommand 'exec ./battle' -ConfigMapName 'battle-node-config' `
			-ClientPublicHost $ClientPublicHost
	}
	else {
		$yaml = New-NodeDeploymentYaml -NodeName 'battle' -Replicas $BattleReplicas `
			-RpcPort 20000 -StartCommand 'exec ./battle' -ConfigMapName 'battle-node-config'
	}
	$globalMatch = Get-GlobalMatchDeploymentSettings -Profile $ReleaseProfile -TiDBHost $MatchTiDBHost -Region $PhysicalRegion
	if ($globalMatch.Enabled) {
		# 三种battle编排使用同一注入路径；密钥只引用已有Secret，不进入配置或脚本输出。
		Assert-GlobalMatchKafkaTopology -Topology (Get-KafkaTopology)
		$yaml = $yaml.Replace("`t", '    ')
		$yaml = Add-GoSvcEnvEntries -SvcName 'battle' -ManifestContent $yaml `
			-Entries ([ordered]@{ GLOBAL_MATCH_ENABLED = 'true'; PHYSICAL_REGION = [string]$globalMatch.Region }) `
			-NamesLabel '全球匹配探测' -Purpose '真实机房探测身份' -Comment '全局匹配部署参数，全部battle必须与match同密钥'
		$yaml = Add-GoSvcSecretEnv -SvcName 'battle' -ManifestContent $yaml -EnvName 'MATCH_PROBE_SECRET' `
			-SecretName 'domain-service-secrets' -SecretKey 'match-probe'
		$yaml = Add-GoSvcSecretEnv -SvcName 'battle' -ManifestContent $yaml -EnvName 'MMORPG_MATCH_OUTCOME_SECRET_BATTLE' `
			-SecretName 'domain-service-secrets' -SecretKey 'match-outcome-battle'
	}
	return [pscustomobject]@{ Orchestrator = $BattleOrchestrator; Health = $health; Yaml = $yaml }
}

function Apply-BattlePool {
	param(
		[Parameter(Mandatory = $true)][string]$ConfigMapYaml,
		# New-BattleWorkloadManifest 的结果:Apply-Infra 在任何基础设施写操作之前已渲染并校验。
		[Parameter(Mandatory = $true)]$Workload
	)

	# 集群现状(换 kind / 同 kind 换入口)已在写路径入口判定(Assert-ClientEntryClusterState)。
	$battleHealth = $Workload.Health
	$battleYaml = $Workload.Yaml

	# 模式切换:再删另一种编排的 battle(Deployment 与 Fleet 同名不同 kind,apply 不会互相回收,两套 battle 会同时注册)。
	# 在打的局会作废,所以集群里确实存在另一种形态而没给 -AllowDisruptiveSwitch 时整条拒绝、一个都不删。
	# 写路径入口已判过一次(Assert-ClientEntryClusterState);这里删除前再判一次是纵深防御:两次之间集群可能被别人改动。
	$battleObsoleteResources = Get-BattleObsoleteResources -BattleOrchestrator $BattleOrchestrator
	Remove-ClientEntryObsoleteResources -Namespace $InfraNamespace -Resources $battleObsoleteResources `
		-KubeContext $KubeContext -KubeConfig $KubeConfig -AllowDisruptiveSwitch:$AllowDisruptiveSwitch -DryRun:$DryRun

	Invoke-KubectlWithInputFile -Args @('apply', '-n', $InfraNamespace) -InputContent $ConfigMapYaml
	# battle 是全局池,跑在 infra namespace,所以日志 sidecar 的 ConfigMap 也要在这里建一份
	# (zone namespace 里的那份对它不可见)。它不属于任何 zone:传空串 = 不写 mmorpg.io/zone 标签,
	# 这样 {mmorpg.io/zone=<某个真 zone>} 的选择器不会把它误选进去。
	if ($script:CppLogSidecarEnabled) {
		Write-CppLogSidecarNotice -Namespace $InfraNamespace
		$battleSidecarConfigYaml = New-CppLogSidecarConfigMapYaml -ConfigName $script:CppLogSidecarConfigMapName -CurrentZoneName ""
		Invoke-KubectlWithInputFile -Args @('apply', '-n', $InfraNamespace) -InputContent $battleSidecarConfigYaml
	}
	if ($BattleOrchestrator -eq 'agones') {
		# agones-sdk 的 SA + RoleBinding 必须先于 Fleet(理由同 scene Fleet,见 New-AgonesSdkRbacYaml)。
		Invoke-KubectlWithInputFile -Args @('apply', '-n', $InfraNamespace) -InputContent (New-AgonesSdkRbacYaml -Namespace $InfraNamespace)
		if ($battleHealth.Raised) {
			Write-Host "NOTE: battle Fleet health.initialDelaySeconds raised from -AgonesHealthInitialDelaySeconds=$($battleHealth.RequestedInitialDelaySeconds) to $($battleHealth.InitialDelaySeconds): worst-case battle startup to first /health is about $($battleHealth.StartupWorstSeconds)s (see Resolve-BattleFleetHealth in lib/k8s_client_entry.ps1); scene Fleet keeps the original value."
		}
		Invoke-KubectlWithInputFile -Args @('apply', '-n', $InfraNamespace) -InputContent $battleYaml
		if ($WaitReady) {
			# 下限 300s(ingress_final WP9):GameServer 从调度、端口分配到 SDK Ready,单实例最坏约 135s,另有镜像拉取与排队。
			# 失败时先捞 B0-4 拒启行再原样抛出(GameServer Pod 没有上一个实例,诊断函数会改读当前实例)。
			try {
				Wait-ForFleetReady -Namespace $InfraNamespace -FleetName 'battle' -ExpectedReplicas $BattleReplicas `
					-TimeoutSeconds ([Math]::Max($WaitTimeoutSeconds, 300)) -KubeContext $KubeContext -KubeConfig $KubeConfig -DryRun:$DryRun
			}
			catch {
				Write-CppOpenFileLimitRefusals -Namespace $InfraNamespace -WorkloadName 'battle'
				throw
			}
		}
		return
	}

	Invoke-KubectlWithInputFile -Args @('apply', '-n', $InfraNamespace) -InputContent $battleYaml
	if ($WaitReady) {
		# Deployment 与 hostPort Deployment 同名 battle、同标签同容器名。
		Wait-ForDeploymentReady -Namespace $InfraNamespace -DeploymentName 'battle' -CppNode
	}
}

# fail-closed:渲染好的 infra 清单里还有 __XXX__ 形态的占位就拒绝 apply。以前只有 kafka.yaml 走替换,别的清单里新加占位会原样送进集群,
# 要到 Pod 起不来才发现。Apply-Infra 的循环与按用途拆出的 queue 实例(New-RedisQueueInstanceManifests)共用这一份检查。
function Assert-InfraManifestPlaceholdersReplaced {
	param(
		[Parameter(Mandatory = $true)][string]$ManifestName,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$Content
	)

	$leftover = [regex]::Matches($Content, '__[A-Z][A-Z0-9_]*__') | ForEach-Object { $_.Value } | Sort-Object -Unique
	if ($leftover.Count -gt 0) {
		throw "infra manifest $ManifestName 里有未替换的占位:$($leftover -join ', ')。请在 Apply-Infra 里补对应的 Replace。"
	}
}

function Apply-Infra {
	# 先生成并校验 battle 配置与工作负载，密钥/契约错误必须发生在任何基础设施写操作之前。
	# 工作负载也在这里渲染(不在写路径入口):Pod 模板要带 zone Redis 口令指纹,口令到 Initialize-InjectedSecrets 才解析;
	# 这里仍早于任何基础设施写操作,Resolve-BattleFleetHealth 与生成器的参数校验照样在 etcd / kafka / redis / mysql apply 之前 throw。
	$battleConfigMapYaml = $null
	$battleWorkload = $null
	if ($BattleReplicas -gt 0) {
		$battleConfigMapYaml = New-NodeConfigMapYaml -CurrentZoneId $ZoneId -CurrentClusterId $ClusterId `
			-ConfigName 'battle-node-config' -IncludeBattleSettings
		$battleWorkload = New-BattleWorkloadManifest
	}
	# 共享 Redis 的形态(-RedisReplicas,见 Get-RedisTopology):单实例 apply redis.yaml,高可用 apply redis-ha.yaml。两份清单里的
	# 对象同名(Service redis / redis-headless、PDB redis-pdb、StatefulSet redis),所以同一时刻只 apply 其中一份。
	# 下面三道检查都在任何写操作(Ensure-Namespace、删旧 Deployment、建 Secret redis-auth、apply)之前:
	#   1. 拓扑变更闸:3 → 1 等拒绝的变更直接 throw(Assert-RedisTopologyChangeIsSafe);
	#   2. 高可用前置:口令字符集(哨兵限制)、连接上限不等式(Get-RedisHaLimits)、代理脚本摘要;
	#   3. 重启闸(P0-E):按下面循环里同一种渲染(只替换 __INFRA_NAMESPACE__)算出 Pod 模板摘要,本次 apply 会改变线上 sts/redis 的
	#      Pod 模板(= 重启 redis Pod;1 → 3 必然属于这一类:闸在 orphan 删除之前读线上摘要)而没给 -ConfirmRedisCoreRestart 时 throw。
	#      摘要随后写进 StatefulSet 自己的注解。
	# 所选清单只读这一次:循环里 apply 的、-WaitReady 部署后核对用的 MAXCLIENTS,都取自这份 $redisManifestRendered。
	# 两次读之间文件被改(同一工作区常有多个会话在写)时,闸会按旧内容放行而实际 apply 新模板。
	$redisTopology = Get-RedisTopology
	$redisTopologyVerdict = $null
	$redisPodTemplateDigest = $null
	$redisHaScriptsDigest = $null
	$redisManifestRendered = $null
	$redisManifestPath = Join-Path $InfraManifestsDir $redisTopology.Manifest
	if ($redisTopology.HighAvailability -and -not (Test-Path $redisManifestPath)) {
		throw "共享 Redis 选的是高可用形态(-RedisReplicas $($redisTopology.Replicas)),但找不到 $redisManifestPath。fail-closed:不悄悄退回单实例。"
	}
	if (Test-Path $redisManifestPath) {
		$redisManifestRendered = (Get-Content -Path $redisManifestPath -Raw).Replace("__INFRA_NAMESPACE__", $InfraNamespace)
		$redisTopologyVerdict = Assert-RedisTopologyChangeIsSafe -Topology $redisTopology
		if ($redisTopology.HighAvailability) {
			Assert-RedisPasswordSentinelSafe -Password $script:RedisPassword
			$redisHaLimits = Get-RedisHaLimits -ManifestContent $redisManifestRendered -SingleInstanceMaxClients (Get-RedisMaxClientsFromManifest)
			Write-Host ("Redis HA limits: maxclients={0} proxy_maxconn={1} proxies={2}+{3} reserved={4} (proxy total {5} + reserved <= maxclients)" -f
				$redisHaLimits.MaxClients, $redisHaLimits.ProxyMaxConn, $redisHaLimits.ProxyReplicas, $redisHaLimits.ProxyMaxSurge,
				$redisHaLimits.Reserved, $redisHaLimits.ProxyTotal)
			$redisHaScriptsDigest = Get-RedisHaScriptsDigest -ManifestContent $redisManifestRendered
		}
		$redisPodTemplateDigest = Get-RedisPodTemplateDigest -ManifestContent $redisManifestRendered
		Assert-RedisCoreRestartConfirmed -DesiredDigest $redisPodTemplateDigest `
			-RollingHandover:($redisTopology.HighAvailability -and $redisTopologyVerdict -eq 'unchanged')
	}
	# 按用途拆出的 queue 实例(G4a 第 1a 批,-RedisSplit queue):同样在任何写操作之前渲染完并过占位检查(模板缺席、容量不合法即 throw);
	# 不是 queue 时为空,下面的 apply 与等待都是空操作,产物与拆分前逐字相同。
	$redisQueueManifests = @(New-RedisQueueInstanceManifests)
	Write-Host "Deploying shared infrastructure to namespace $InfraNamespace"
	Write-Host "Kafka profile: $KafkaProfile (broker_retention_ms=$KafkaBrokerRetentionMs db_task_retention_ms=$KafkaDbTaskRetentionMs)"
	Ensure-Namespace -Namespace $InfraNamespace

	# redis-match-cluster.yaml:match 私有的 Redis Cluster(StatefulSet + 建群 Job)。
	# 里面的 Job 幂等(已建群就跳过);但 Job 的 template 一旦落地就不可变,改了
	# 建群脚本要先 `kubectl -n <infra> delete job redis-match-cluster-init` 再 apply。
	# Loki 只在"要用集群内 Loki 当 C++ 日志 sidecar 的写入目标"时才部署:
	# 关了 sidecar(-NoCppLogSidecar)或指定了外部 Loki(-LokiPushUrl)时不占集群资源。
	# etcd-maintenance.yaml:etcd MVCC 历史压缩 CronJob etcd-compact(P0-C,docs/ops/etcd-maintenance-runbook.md),
	# 紧跟 etcd.yaml。它只新增 ConfigMap + CronJob,不碰 StatefulSet etcd,apply 不会滚动 etcd;占位只有 __INFRA_NAMESPACE__,
	# 走下面的通用替换。有真实存档的环境不要为了它跑 infra-up(会连带 apply mysql.yaml,L-04 约束见
	# docs/handoff/godb-pipeline-handoff-20261013.md §2.4),按 runbook 单独 apply 这一个清单。
	# 共享 Redis 那一项按拓扑取 redis.yaml 或 redis-ha.yaml(见上)。
	$infraManifests = @("etcd.yaml", "etcd-maintenance.yaml", $redisTopology.Manifest, "redis-match-cluster.yaml", "kafka.yaml", "mysql.yaml")
	if ($script:CppLogSidecarEnabled -and [string]::IsNullOrWhiteSpace($LokiPushUrl)) {
		$infraManifests += "loki.yaml"
	}

	foreach ($manifest in $infraManifests) {
		$path = Join-Path $InfraManifestsDir $manifest
		if (-not (Test-Path $path)) {
			Write-Warning "Infra manifest not found: $path — skipping"
			continue
		}

		if ($manifest -eq "etcd.yaml") {
			# etcd 已从单副本 Deployment(emptyDir)改成 3 副本 StatefulSet + PVC
			# (docs/design/node-id-overhaul-plan-20260908.md §7 Phase 0.5)。两者 kind 不同、名字相同,
			# `kubectl apply` 只会新建 StatefulSet,不会回收旧 Deployment;而 Service `etcd`
			# 的 selector(app=etcd)会同时命中新旧 Pod —— 客户端就会在「旧单机 etcd」和
			# 「新集群」之间随机落点,两边数据各不相同,等于脑裂。所以先删旧 Deployment 再 apply。
			# 旧 emptyDir 里的数据**不迁移**:里面只有 lease 绑定的节点注册与发号槽位,
			# 节点重启就重新申领(见 deploy/k8s/README.md「etcd:3 副本 StatefulSet」一节)。
			# --ignore-not-found:全新集群 / 已经切过的集群这里是 no-op。
			Invoke-Kubectl -Args @("delete", "deployment", "etcd", "-n", $InfraNamespace, "--ignore-not-found")
		}

		if ($manifest -eq "kafka.yaml") {
			# kafka 已从单副本 Deployment(emptyDir)改成 StatefulSet + PVC
			# (routing-identity-audit-20260908.md R06),与上面 etcd 完全同形的 kind 变更:
			# 两者 kind 不同、名字相同,`kubectl apply` 只会新建 StatefulSet,不会回收旧
			# Deployment;而 Service `kafka` 的 selector(app=kafka)会同时命中新旧 Pod ——
			# 客户端(以及 controller quorum 用的 kafka:9093)就会在"旧 emptyDir broker"和
			# "新 PVC broker"之间随机落点,两边各有一套 topic 与 offset,等于脑裂。
			# 所以先删旧 Deployment 再 apply。
			# 旧 emptyDir 里的数据**不迁移**,而且本来就没有:旧 manifest 把卷挂在
			# /tmp/kafka-logs,而镜像的 log.dirs 默认是 /tmp/kraft-combined-logs。
			# 切换后 topic 全部为空,**必须重跑 infra-kafka-topics**(下面 Apply-KafkaTopicInitJob
			# 在同一次 infra-up 里就会跑一遍;单独止血用 -Command infra-kafka-topics)。
			# --ignore-not-found:全新集群 / 已经切过的集群这里是 no-op。
			Invoke-Kubectl -Args @("delete", "deployment", "kafka", "-n", $InfraNamespace, "--ignore-not-found")

			# broker 数变更的安全闸:会改掉选举组或摘掉 broker 的变更在这里拒绝(DryRun 不查集群)。
			Assert-KafkaTopologyChangeIsSafe -Topology (Get-KafkaTopology)
		}

		if ($manifest -eq $redisTopology.Manifest) {
			# 密码走**可选** Secret:dev 档 MMORPG_REDIS_PASSWORD 回落为空串 → 删掉 Secret → 两份 Redis 清单里
			# secretKeyRef 的 optional:true 让容器以无密码启动(与改造前行为一致);release 档强制 ≥12 位
			# (Resolve-InjectedSecret 的 -MinLength 12)→ 建 Secret → 服务端 --requirepass。
			# 这一步必须在 apply redis.yaml **之前**:Pod 起来时 Secret 不存在的话,optional 引用会解析成空,
			# 于是服务端无密码而客户端配置带密码,登录链路全线 NOAUTH。
			if ([string]::IsNullOrEmpty($script:RedisPassword)) {
				Invoke-Kubectl -Args @("delete", "secret", "redis-auth", "-n", $InfraNamespace, "--ignore-not-found")
			}
			else {
				# 用 stringData 而不是 data:值由 API server 做 base64,脚本里不必自己编码,
				# 也避免把编码后的密文写进日志。走 Invoke-KubectlWithInputFile 与本函数其余 apply 同一条路径
				# (DryRun 下会打印将要提交的 YAML —— 注意它会连密码一起打印,与 mysql-init 走的是同一套约定)。
				$redisAuthSecretYaml = @"
apiVersion: v1
kind: Secret
metadata:
  name: redis-auth
type: Opaque
stringData:
  password: "$($script:RedisPassword)"
"@
				Invoke-KubectlWithInputFile -Args @("apply", "-n", $InfraNamespace) -InputContent $redisAuthSecretYaml
			}

			# redis 从单副本 Deployment(emptyDir)改成 StatefulSet + PVC,与 etcd / kafka 同形的 kind 变更:
			# 两者 kind 不同、名字相同,`kubectl apply` 只会新建 StatefulSet 而不回收旧 Deployment,而 Service
			# `redis` 的 selector(app=redis)会同时命中新旧 Pod —— 客户端在「旧 emptyDir 实例」和「新 PVC 实例」
			# 之间随机落点,会话 / 锁 / 选主锁两边各一套,等于脑裂。所以先删旧 Deployment 再 apply。
			# 旧 emptyDir 里的数据**不迁移**(它本来也活不过一次 Pod 重建)。切换 = 一次 Redis 重启:
			# 在线玩家的会话与位置记录清空,需要重新登录。**这是一次有停机窗口的变更。**
			# 高可用形态的入口代理叫 redis-proxy,不受这一行影响。
			Invoke-Kubectl -Args @("delete", "deployment", "redis", "-n", $InfraNamespace, "--ignore-not-found")

			if ($redisTopologyVerdict -eq 'upgrade') {
				# 单实例 → 高可用:podManagementPolicy 要从 OrderedReady 改成 Parallel,这个字段创建后不能改,直接 apply 会被 API server 拒绝。
				# 所以先只删 StatefulSet 对象本身(--cascade=orphan:redis-0 这个 Pod 与它的 PVC 原样保留、继续服务),紧接着 apply 新的
				# StatefulSet,它按同名、同标签接管 redis-0,再按滚动更新把它换成高可用模板,redis-1 / redis-2 新建后从它全量同步。
				# 2026-10-08 在本机 kind 上实测:数据不丢,Service redis 的 ClusterIP 不变。重启闸已在函数开头(删除之前)判过。
				# 删除之后、下面 apply 之前失败(apply 被拒、网络、进程中断)会留下孤儿 redis-0:重跑时拓扑闸按孤儿 Pod 仍判 upgrade,
				# 重启闸判 orphan-pods,同样要 -ConfirmRedisCoreRestart(Get-LiveRedisReplicas / Get-LiveRedisOrphanPodOrdinals)。
				Invoke-Kubectl -Args @("delete", "statefulset", "redis", "-n", $InfraNamespace, "--cascade=orphan", "--ignore-not-found")
			}
		}

		if ($manifest -eq "mysql.yaml") {
			# initdb 脚本的 ConfigMap 必须先于 mysql Deployment 落地,否则 Pod 因
			# volume 引用的 ConfigMap 不存在卡在 ContainerCreating。
			Invoke-KubectlWithInputFile -Args @("apply", "-n", $InfraNamespace) -InputContent (New-MysqlInitConfigMapYaml)
		}

		if ($manifest -eq $redisTopology.Manifest -and $null -ne $redisManifestRendered) {
			# 重启闸判过的那一份(函数开头只读一次;redis.yaml 或 redis-ha.yaml),不重读文件;下面的 __INFRA_NAMESPACE__ 替换对它是幂等的。
			$manifestContent = $redisManifestRendered
		}
		else {
			$manifestContent = Get-Content -Path $path -Raw
		}

		# 所有 infra manifest 共用的占位:__INFRA_NAMESPACE__ → -InfraNamespace。
		# etcd / kafka 的广播地址必须是跨 namespace 可解析的 FQDN(zone namespace 里的
		# 客户端 bootstrap 后拿到的是 broker 广播的地址再去连),而 FQDN 里带 namespace,
		# 写死在 manifest 里换 namespace 部署就会广播一个不存在的地址。
		$manifestContent = $manifestContent.Replace("__INFRA_NAMESPACE__", $InfraNamespace)

		if ($manifest -eq "kafka.yaml") {
			$manifestContent = $manifestContent.Replace("__KAFKA_LOG_RETENTION_MS__", [string]$KafkaBrokerRetentionMs)
			$manifestContent = $manifestContent.Replace("__KAFKA_LOG_RETENTION_CHECK_INTERVAL_MS__", [string]$KafkaRetentionCheckIntervalMs)
			$manifestContent = $manifestContent.Replace("__KAFKA_LOG_RETENTION_BYTES__", [string]$KafkaRetentionBytes)
			$manifestContent = $manifestContent.Replace("__KAFKA_LOG_SEGMENT_BYTES__", [string]$KafkaSegmentBytes)
			$manifestContent = $manifestContent.Replace("__KAFKA_HEAP_OPTS__", [string]$KafkaHeapOpts)
			# 拓扑相关的占位全部取自同一个 Get-KafkaTopology(broker 数 / 选举组 / 副本数 / PDB 必须一起变)。
			$kafkaTopology = Get-KafkaTopology
			$manifestContent = $manifestContent.Replace("__KAFKA_BROKER_COUNT__", [string]$kafkaTopology.Brokers)
			$manifestContent = $manifestContent.Replace("__KAFKA_CONTROLLER_COUNT__", [string]$kafkaTopology.ControllerCount)
			$manifestContent = $manifestContent.Replace("__KAFKA_CONTROLLER_QUORUM_VOTERS__", [string]$kafkaTopology.ControllerQuorumVoters)
			$manifestContent = $manifestContent.Replace("__KAFKA_POD_MANAGEMENT_POLICY__", [string]$kafkaTopology.PodManagementPolicy)
			$manifestContent = $manifestContent.Replace("__KAFKA_REPLICATION_FACTOR__", [string]$kafkaTopology.ReplicationFactor)
			$manifestContent = $manifestContent.Replace("__KAFKA_MIN_INSYNC_REPLICAS__", [string]$kafkaTopology.MinInsyncReplicas)
			$manifestContent = $manifestContent.Replace("__KAFKA_PDB_MIN_AVAILABLE__", [string]$kafkaTopology.PdbMinAvailable)
			# 每个 broker 的 PVC 大小(S5b):写路径入口的 Resolve-KafkaDiskPlan 已算好并判过「显式值与线上不同即拒绝」。
			# 缓存为空(没经过写路径入口的调用)时现场再算并再判一次;算不出即 throw,不回落任何默认尺寸(fail-closed)。
			# 局部变量不叫 $kafkaDiskPlan:PowerShell 变量名不分大小写,同名会遮住脚本级的 $KafkaDiskPlan,读的人容易看岔。
			$infraDiskPlan = $script:KafkaDiskPlan
			if ($null -eq $infraDiskPlan) {
				$infraDiskPlan = Resolve-KafkaDiskPlan
				Assert-KafkaStorageChangeIsSafe -Plan $infraDiskPlan
			}
			# 渲染的是 StorageTemplateGi 而不是 EffectiveGi:StatefulSet 在时模板必须原样等于线上值(不可变字段,不同即 apply 被拒),
			# 哪怕它大于最小的实际 PVC —— 那时 retention.bytes 已按较小的实际盘(EffectiveGi)算(见 Get-KafkaDiskPlan)。
			$manifestContent = $manifestContent.Replace("__KAFKA_STORAGE_SIZE__", ("{0}Gi" -f [int]$infraDiskPlan.StorageTemplateGi))
		}

		if ($manifest -eq $redisTopology.Manifest -and $null -ne $redisPodTemplateDigest) {
			# 重启闸(P0-E):函数开头算好的 Pod 模板摘要写进 StatefulSet redis 自己的注解(不在 Pod 模板里,改它不滚动 redis Pod),
			# 下一次 infra-up 由 Assert-RedisCoreRestartConfirmed 读回比较。摘要与这里 apply 的内容是同一份字符串($redisManifestRendered)。
			$manifestContent = $manifestContent.Replace($script:RedisPodTemplateDigestPlaceholder, $redisPodTemplateDigest)
			if ($null -ne $redisHaScriptsDigest) {
				# 高可用形态:脚本 ConfigMap 的摘要写进代理的 Pod 模板注解,脚本一改代理就滚动(见 Get-RedisHaScriptsDigest)。
				$manifestContent = $manifestContent.Replace($script:RedisHaScriptsDigestPlaceholder, $redisHaScriptsDigest)
			}
		}

		# fail-closed:任何 __XXX__ 形态的占位没被替换就拒绝 apply(Assert-InfraManifestPlaceholdersReplaced,queue 实例共用同一份检查)。
		Assert-InfraManifestPlaceholdersReplaced -ManifestName $manifest -Content $manifestContent

		Invoke-KubectlWithInputFile -Args @("apply", "-n", $InfraNamespace) -InputContent $manifestContent
	}

	# 按用途拆出的 queue 实例(-RedisSplit queue):排在全部 infra 清单之后,也就在核心 Redis 之后 —— 口令 Secret redis-auth 已在上面的
	# 核心分支建好或删掉,queue 实例引用同一个可选 Secret。清单在函数开头(任何写操作之前)已渲染并过了占位检查。
	Apply-RedisQueueInstances -Manifests $redisQueueManifests

	# data_service 审计 topic 预建 Job:必须在任何 zone(scene)之前落地,见 Apply-KafkaTopicInitJob 注释。
	# 放在 $infraManifests 的全部 infra manifest 之后即可:Job 自己会等 broker 可达,不要求 kafka Deployment 已 Ready。
	Apply-KafkaTopicInitJob

	Write-Host "Shared infrastructure deployed: namespace=$InfraNamespace"

	if ($WaitReady) {
		# 全局池 match 起来第一件事就是注册 etcd;-WaitReady 下先等 etcd 3 副本成 quorum 再拉它。
		Wait-ForStatefulSetReady -Namespace $InfraNamespace -StatefulSetName "etcd"
		# kafka 也是 StatefulSet + PVC(R06)。先等它 Ready 再等下面的预建 Job:
		# Job 自己会等 broker 可达,但等到的超时长得像"契约错误";先在这里失败,
		# 报的是 PVC Pending / 镜像拉不动这类真正的原因。
		Wait-ForStatefulSetReady -Namespace $InfraNamespace -StatefulSetName "kafka"
		# redis 现在也是 StatefulSet(见上面的 kind 变更)。它在**同步登录路径**上(login 拿不到锁即拒绝
		# EnterGame),没起来就让后面的 zone 部署继续,只会把失败推迟到玩家进不去游戏时才暴露。
		Wait-ForStatefulSetReady -Namespace $InfraNamespace -StatefulSetName "redis"
		if ($redisTopology.HighAvailability) {
			# 高可用形态下客户端连的是代理(Service redis 指向它):redis 节点就绪而代理没起来,照样连不上。
			Wait-ForDeploymentReady -Namespace $InfraNamespace -DeploymentName "redis-proxy"
		}
		# 部署后核对(P0-E):生效的 maxclients 是否等于清单的 MAXCLIENTS。被容器 nofile 硬限下调时,到线之前只有这里能自动发现;
		# 不等只告警,不阻断(清单已经 apply 完了)。高可用形态逐个核 3 个节点。DryRun 只打一行说明,不连集群。
		# 期望值取自函数开头那一次读取($redisManifestRendered),不重新读盘:等 StatefulSet 就绪的几分钟里文件被改,
		# 重新读会拿新值去比刚 apply 的旧值(误报),或在编辑中途读到 0 / 2 个 MAXCLIENTS 而在 apply 之后 throw。
		if ($null -ne $redisManifestRendered) {
			Test-RedisLiveMaxClients -ExpectedMaxClients (Get-RedisMaxClientsFromManifest -ManifestContent $redisManifestRendered) -Nodes $redisTopology.Replicas
		}
		else {
			Test-RedisLiveMaxClients -ExpectedMaxClients (Get-RedisMaxClientsFromManifest) -Nodes $redisTopology.Replicas
		}
		# queue 实例逐个等就绪并核对 maxclients(不是 queue 时为空操作)。
		Wait-RedisQueueInstancesReady -Manifests $redisQueueManifests
		# 审计 topic 必须在 scene 产出第一条消息之前按契约建好。-WaitReady 下等 Job Complete,
		# all-up 里随后的 zone 就一定晚于它;不带 -WaitReady 时 zone-up 之前要自己核对
		# `kubectl -n <infra> get job kafka-topic-init` 已 Complete(README「Kafka 审计 topic 预建」)。
		Wait-ForJobComplete -Namespace $InfraNamespace -JobName "kafka-topic-init"
	}

	if ($BattleReplicas -gt 0) {
		Apply-BattlePool -ConfigMapYaml $battleConfigMapYaml -Workload $battleWorkload
	}

	# 全局池服务(match)与基础设施同命运:一份、放 infra namespace、所有 zone 共用
	Apply-GlobalGoSvcManifests
}

function Remove-Infra {
	Write-Host "Deleting shared infrastructure namespace: $InfraNamespace"
	Invoke-Kubectl -Args @("delete", "namespace", $InfraNamespace) -AllowFailure
}

function Show-InfraStatus {
	Write-Host "Shared infrastructure status for namespace=$InfraNamespace"
	# sts / pvc / pdb:etcd(3 副本)与 kafka(单 broker)都是 StatefulSet,只看 deploy 会漏掉它们;
	# PVC Pending(集群没有默认 StorageClass)是这两个起不来的头号原因,pdb 则解释为什么
	# `kubectl drain` 会挂在 etcd / kafka 上(kafka 的 PDB 是 minAvailable: 1,不允许自愿驱逐)。
	# job:kafka-topic-init(审计 + 控制面命令 topic 预建)的 COMPLETIONS 0/1 = 分区契约没建成,
	# scene 起来前必须先看它。
	# job 一栏同样列出建表 Job trade-migrate / friend-migrate / player-inbox-migrate / guild-migrate / mail-migrate(D-14):失败须核对
	# status.conditions(Failed/FailureTarget),看 `kubectl -n <infra> logs job/trade-migrate`(或 `job/friend-migrate`、`job/player-inbox-migrate`、
	# `job/guild-migrate`、`job/mail-migrate`);删除前须确认该 Job UID 所属 Pod 均终态,ACTIVE=0 本身不代表安全。
	# cronjob:etcd-compact(etcd MVCC 压缩,P0-C)的 LAST SCHEDULE,加上 job 一栏里 etcd-compact-<时间戳> 的失败记录,
	# 是目前唯一能看到压缩停了的地方(仓库里还没有 etcd 的抓取与告警);处置见 docs/ops/etcd-maintenance-runbook.md。
	Invoke-Kubectl -Args @("get", "deploy,sts,po,pvc,pdb,job,cronjob,svc,cm", "-n", $InfraNamespace) -AllowFailure
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
# -KafkaBrokers 非法(例如 2)要在任何写操作之前拦下;同时把本次解析出的拓扑打出来,
# zone-up 与 infra-up 的档位参数不一致时,从这一行就能看出两边的副本数对不上。
$kafkaTopologyAtStart = Get-KafkaTopology
Write-Host "Kafka topology: brokers=$($kafkaTopologyAtStart.Brokers) controllers=$($kafkaTopologyAtStart.ControllerCount) replication_factor=$($kafkaTopologyAtStart.ReplicationFactor) min_insync_replicas=$($kafkaTopologyAtStart.MinInsyncReplicas)"
# 共享 Redis 的形态:-RedisReplicas 非法(例如 2)在这里就 throw(Get-RedisTopologyFor),早于任何集群操作;同时打出本次用哪份清单与
# WAITAOF 副本份数,infra-up 的日志一眼能看出是不是高可用形态。zone 写路径随后可能改为跟随线上(写路径入口另打一行)。
$redisTopologyAtStart = Get-RedisTopology
Write-Host "Redis topology: replicas=$($redisTopologyAtStart.Replicas) manifest=$($redisTopologyAtStart.Manifest) high_availability=$(([string]$redisTopologyAtStart.HighAvailability).ToLowerInvariant()) waitaof_replicas=$(Get-RouteStoreWaitAOFNumReplicas -Topology (Get-MappingRedisTopology))"
Show-ExposureProfileWarning

Write-Host "Release: profile=$ReleaseProfile image=$NodeImage pullPolicy=$ImagePullPolicy go_tag=$GoSvcTag java_tag=$JavaSvcTag cluster_id=$ClusterId"
if ($script:ReleaseStamp.Ok -and $script:ReleaseStamp.Dirty) {
	Write-Warning "Working tree is dirty (git status is nonempty); image tag has the -dirty suffix. Production releases (-ReleaseProfile prod) reject this tag."
}

# 写操作才需要门禁;*-down / *-status 是止血和排查路径,不能被预检或密钥缺失挡住。
if ($Command -in @("zone-up", "all-up", "infra-up")) {
	if (-not $SkipGoSvc -and -not [string]::IsNullOrWhiteSpace($GoSvcRegistry)) {
		$globalSql = Get-GlobalAuthoritySqlSettings -Profile $ReleaseProfile -CanonicalHost $GlobalAuthorityTiDBHost `
			-FriendHost $FriendTiDBHost -TradeHost $TradeTiDBHost -MatchHost $MatchTiDBHost -DevelopmentHost "mysql.${InfraNamespace}:3306"
		if ($globalSql.RequireTiDB -cne 'true') { throw '正式全球服务必须指定同一GlobalAuthorityTiDBHost，所有档位禁止MySQL权威回退。' }
		if ($globalSql.RequireTiDB -eq 'true') { $FriendTiDBHost = $globalSql.Host; $TradeTiDBHost = $globalSql.Host; $MatchTiDBHost = $globalSql.Host }
	}
	# 好友全局池部署前检查权威事务域配置，早于任何基础设施写入。
	if (($Command -eq 'infra-up' -or ($Command -eq 'all-up' -and -not $SkipInfra)) -and
		-not $SkipGoSvc -and -not [string]::IsNullOrWhiteSpace($GoSvcRegistry)) {
		$null = Get-FriendSqlSettings -Profile $ReleaseProfile -TiDBHost $FriendTiDBHost -DevelopmentHost "mysql.${InfraNamespace}:3306"
		$null = Get-TradeSqlSettings -Profile $ReleaseProfile -TiDBHost $TradeTiDBHost -DevelopmentHost "mysql.${InfraNamespace}:3306"
	}
	Assert-ImmutableReleaseImages
	Invoke-ReleasePreflight
	# 集群外入口的组合校验(含 -LoginDevPasswordAuth 只许 dev 档)排在密钥解析之前:非 dev 档传该开关时,
	# 报的是门禁原因,而不是"缺环境变量"。battle 工作负载渲染与集群现状预检(只读 kubectl)也在这里,早于任何写操作。
	Assert-ClientEntryDeployPreflight
	# go/db 存盘消费组计划(S2 ③):逐 zone 读线上 go-svc-db-config,按需 kubectl exec 进 kafka-0 读两组位点,按决策表定形状。
	# 只读;F2 / F4 与「读不到」的拒绝都发生在这里,早于下面的密钥解析与 switch ($Command) 里的任何写操作(见 Initialize-DbConsumerGroupPlans)。
	Initialize-DbConsumerGroupPlans
	# 共享 Redis 形态的线上核对(只读):不经 Apply-Infra 的写路径(zone-up、all-up -SkipInfra)部署 zone Go 服务时,db / data-service 的
	# WAITAOF 副本份数必须与线上 sts/redis 一致 —— 没给 -RedisReplicas 就跟随线上,显式值与线上不同即拒绝(Resolve-RedisTopologyFromLive)。
	# 不部署 zone Go 服务(-SkipGoSvc、没给 -GoSvcRegistry,例如 k8s_image.ps1 的 release-zone)时不渲染这两项,不查。
	# infra-up / 带 infra 的 all-up 由 Apply-Infra 的拓扑变更闸负责。DryRun 不查集群(Get-LiveStatefulSetReplicas 返回 $null);
	# 排在下面 Assert-RouteSwitchPreflight(打印 redis_topology)与 Write-RedisConnectionLedger 之前,早于密钥解析与任何写操作。
	if (($Command -eq 'zone-up' -or ($Command -eq 'all-up' -and $SkipInfra)) -and (Test-ZoneDbDeployed)) {
		Resolve-RedisTopologyFromLive
	}
	# 共享 Redis 按用途拆实例(G4a 第 1a 批,-RedisSplit):本次要 apply queue 实例时先定容量(release 档缺 -RedisQueueMaxMemory 在这里拒绝),
	# 再做排队地址的防静默切换 —— 非 DryRun 读本次部署的每个 zone 的线上 login / gateway ConfigMap,auto 在这里跟随线上,地址要变而没给
	# -ConfirmRedisSplitSwitch、或线上排队开着即拒绝。只读,排在密钥解析与 Write-RedisConnectionLedger(按解析后的拓扑记账)之前。
	Assert-RedisPurposePreflight
	# D1 映射持久化开关(第 1b-3 批):组合门禁(Arm / 读方开关要求 db_first,读方开关要求 Arm)与 RouteStore 段生成器自检 DryRun 同样判;
	# 非 DryRun 再读线上 data-service(全部 zone)/ db / guild 的 ConfigMap 做粘滞核对,与本次参数不一致且没给 -ConfirmRouteSwitchChange 即拒绝。
	# 只读,早于下面的密钥解析与 switch ($Command) 里的任何写操作(见 Assert-RouteSwitchPreflight)。
	Assert-RouteSwitchPreflight
	# 削峰(G4b)的开关与起步值打一行,关着 / 没配可信代理时告警;只打印、不阻断。例外是生成器自检,在这里就 throw:常量表被改坏
	# (限流阈值、排队容量不在 1..uint32),或 go/login/etc/login.yaml 缺 Queue.* 镜像键。排在密钥解析之前:
	# 缺 MMORPG_LOGIN_QUEUE_TOKEN_SECRET 被拒时,输出里已经有「login_queue=on」这一行可对照。
	Show-PeakShavingPlan
	# -AssetOps / -NetworkPolicies 的组合校验(资产通道打开必须同时设 gRPC 口准入),排在密钥解析之前:报的是组合原因而不是「缺环境变量」。
	Assert-AssetOpsSwitchPreflight
	# 排行榜评分快照开关(-MatchRatingSnapshotPublish,不粘滞):只在本次部署全局 Go 服务(match 在其中)时打一行本次取值,
	# 「榜为什么停更」先对照这一行。只打印、不阻断。本次不部署全局 Go 服务却显式传了它:参数没有效果,告警而不是静默接受。
	if (Test-GlobalGoSvcDeployed) {
		Write-Host "MatchRatingSnapshotPublish: $MatchRatingSnapshotPublish (match RatingSnapshotPublishEnabled; leaderboard snapshots are published only when 1; the value is in the match pod template, so a change rolls match)"
	}
	elseif ($PSBoundParameters.ContainsKey('MatchRatingSnapshotPublish')) {
		Write-Warning "-MatchRatingSnapshotPublish $MatchRatingSnapshotPublish has no effect: this run does not deploy global Go services (match). Use infra-up / all-up with -GoSvcRegistry and without -SkipGoSvc."
	}
	Initialize-InjectedSecrets
	# 资产通道签名方(guild / trade)与验签方(scene)的跨命令核对:顺序反了、密钥对不上就在任何写操作之前拒绝(只读 kubectl;DryRun 跳过)。
	Assert-AssetOpClusterConsistency
	# C++ gRPC deadline ≥ 目标 Go 服务 zrpc Timeout + 2000(上游比下游宽)。放在任何集群写操作之前:
	# 不成立就整条拒绝,不留半截部署(见 Assert-GrpcClientDeadlineBudget)。
	Assert-GrpcClientDeadlineBudget
	# 收件箱写前门禁(交接 player-inbox-handoff-20261015 §6 H1):渲染值只取 git 跟踪的两份 yaml;-PlayerInboxConsumer 与 yaml 不一致、
	# 「消费端关而白名单非空」(当前检出里的两份 yaml,或非 DryRun 时另一侧线上已部署的值)且没传 -AllowPlayerInboxConsumerOff、
	# 或 staging / prod 真部署时两份 yaml 带未提交改动,同样先于任何集群写操作整条拒绝。线上值只读(Read-PlayerInboxGateLiveFacts)。
	$playerInboxGateSource = Get-PlayerInboxGateSourceState
	$playerInboxGateLive = Read-PlayerInboxGateLiveFacts -ExpectedConsumer $PlayerInboxConsumer
	Assert-PlayerInboxConsumerGate -ExpectedConsumer $PlayerInboxConsumer -AllowConsumerOff ([bool]$AllowPlayerInboxConsumerOff) `
		-SourceState $playerInboxGateSource -RequireCommittedSources ((-not $DryRun) -and $ReleaseProfile -ne 'dev') `
		-LiveFacts $playerInboxGateLive
	# 路由服预算链:路由目标 Go 服务 zrpc Timeout + 1000 ≤ 路由服 ForwardTimeoutMs(client-request-ordering.md §5.2 I1)。
	# 不成立时 gate 放行同一会话的下一条请求后,上一条仍可能晚落地;同样先于任何集群写操作。
	Assert-RouterForwardBudget
	# MySQL 连接池(G2b),同样放在任何集群写操作之前:
	#   - Assert-MysqlPoolMirror:ConfigMap 要镜像的池参数不合格(非正整数 / idle > open / gateway connection-timeout < 250)就整条拒绝,不留半截部署;
	#   - Write-MysqlConnectionLedger:打印 "MySQL connection ledger" 一行;峰值超过 max_connections 只告警、不阻断。
	Assert-MysqlPoolMirror
	Write-MysqlConnectionLedger
	# Redis 连接总账(P0-E):打印 "Redis connection ledger" 一行;峰值超过 80% × maxclients 只告警、不阻断(同上一行的纪律)。
	Write-RedisConnectionLedger
	# go/db 副本数门禁(S1 ④ (c)):manifest 的 replicas(及同一份 manifest 里 HPA 的 maxReplicas)> db.yaml 的 PartitionCnt 即整条拒绝,
	# 先于任何集群写操作;kubectl scale 绕过它(见 Assert-DbReplicasWithinPartitionContract)。
	Assert-DbReplicasWithinPartitionContract
	# Kafka 盘预算(S5b):算出 db_task 的 retention.bytes 与每个 broker 的 PVC 大小(缓存进 $script:KafkaDiskPlan,
	# Apply-Infra 的 kafka.yaml 与 db / login 的 ConfigMap 都读它),打印 "Kafka disk ledger" 一行;盘不够时按盘收缩、只告警不阻断,
	# 连 2 个 segment 都放不下才 throw。显式 -KafkaStorageGi 与线上 PVC 不同时拒绝(volumeClaimTemplates 不可变,扩盘走 README 的 runbook)。
	# 三条写命令都判:zone-up 同样按这块盘算 retention.bytes。全部早于 switch ($Command) 里的任何写操作。
	$null = Resolve-KafkaDiskPlan
	Write-KafkaDiskLedger -Plan $script:KafkaDiskPlan
	Assert-KafkaStorageChangeIsSafe -Plan $script:KafkaDiskPlan
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
	"infra-kafka-topics" {
		# 止血路径:切到 StatefulSet+PVC 之后 / infra-down 删过 PVC 之后 / 手工删过 topic 之后,
		# topic 都是空的,scene 产出第一条消息之前必须重跑预建,否则会被 auto-create 成 1 分区。
		Apply-KafkaTopicInitJob
		if ($WaitReady) {
			Wait-ForJobComplete -Namespace $InfraNamespace -JobName "kafka-topic-init"
		}
	}
	"redis-split-status" {
		# 只读:各 zone 的 login / gateway 是否指向同一个排队实例、是否滚完(切换 -RedisSplit 之后、排空 gate 之前以它为闸)。
		Show-RedisSplitStatus
	}
	"redis-wipe" {
		# 压测前清库(AGENTS.md §6.2):准入与做法见 Invoke-RedisWipe。
		Invoke-RedisWipe
	}
	"zone-up" {
		Apply-Zone -CurrentZoneName $ZoneName -CurrentZoneId $ZoneId -CurrentClusterId $ClusterId -CurrentCentreReplicas $CentreReplicas -CurrentGateReplicas $GateReplicas -CurrentSceneReplicas $SceneReplicas -CurrentSceneWorldReplicas $SceneWorldReplicas -CurrentSceneInstanceReplicas $SceneInstanceReplicas -CurrentGateNodePortBase $GateNodePortBase
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
			# zones 配置里没有 cluster:它是集群常量,所有 zone 与 infra 阶段共用命令行 -ClusterId。
			Apply-Zone -CurrentZoneName $zone.name -CurrentZoneId $zone.zoneId -CurrentClusterId $ClusterId -CurrentCentreReplicas $zone.centre -CurrentGateReplicas $zone.gate -CurrentSceneReplicas $zone.scene -CurrentSceneWorldReplicas $zone.scene_world -CurrentSceneInstanceReplicas $zone.scene_instance -CurrentSceneLegacyExplicit $zone.scene_legacy_explicit -CurrentGateNodePortBase $zone.gateNodePortBase
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
