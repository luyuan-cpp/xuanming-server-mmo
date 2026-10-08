param(
	[Parameter(Mandatory = $true)]
	# infra-kafka-topics:只重跑审计 topic + 控制面命令 topic 的预建 Job(Apply-KafkaTopicInitJob)。
	# Kafka 换成 StatefulSet + PVC 之后(R06),普通重启不再丢 topic;仍然保留这条止血路径,
	# 因为**第一次从旧 emptyDir Deployment 切过来**、`infra-down` 删过 namespace(连 PVC 一起删)、
	# 或者有人手工删过 topic 之后,topic 都是空的,而 scene 一发消息就会把它自动建成 1 分区。
	# 不过发布门禁(与 *-down / *-status 同列),见 deploy/k8s/README.md「Kafka:StatefulSet + PVC」。
	[ValidateSet("zone-up", "zone-down", "zone-status", "all-up", "all-down", "all-status", "infra-up", "infra-down", "infra-status", "infra-kafka-topics")]
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
	[string]$KafkaHeapOpts = "",
	# Kafka broker 数。0 = 按档位取默认:-ReleaseProfile 不是 dev,或 -OpsProfile 不是 custom → 3;其余 → 1。
	# 只接受 1 或 ≥3:两个 broker 的选举组挂一个就没有多数派,比单 broker 还脆。
	# 前 3 个 Pod 兼任 controller、第 4 个起只当 broker,所以 3 → N 只是加副本;
	# 1 ↔ ≥3 不能原地切换,脚本会拒绝(Assert-KafkaTopologyChangeIsSafe,流程见 README「Kafka:多 broker」)。
	[ValidateRange(0, 99)]
	[int]$KafkaBrokers = 0,
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
	# 与 C++ gRPC server 的默认值保持一致。传 0 时 Deployment / Fleet
	# 都不注入环境变量,由进程默认值接管；正数则两种编排写入同一个值。
	[int]$GrpcServerMaxPollers = 8,
	# gate 对外 Service 的类型。-ClientEntryMode external:决定每序号 Service gate-<i> 的类型,只允许 NodePort / LoadBalancer
	# (preflight 校验);podip:决定单一 gate-entry 的类型,且 gate-entry 只在 gate 副本数为 1 时生成(D90)。
	# 取值在参数绑定后规范成 K8s 的标准大小写(见脚本顶部 ConvertTo-ClientEntryCanonicalName)。
	[ValidateSet("ClusterIP", "NodePort", "LoadBalancer")]
	[string]$GateServiceType = "NodePort",
	[int]$GateServicePort = 18000,
	# gate 的客户端 RPC 路由模式:写进 gate Deployment 的环境变量 GATE_CLIENT_RPC_ROUTER
	# (cpp/nodes/gate/gate_router_mode.h;docs/design/client-rpc-router.md D34)。
	#   "1" = gate 只连路由服 client-rpc-router,chat / friend / trade 等只承诺路由模式的服务经它可达
	#         (guild / team 等没登记进 $GoSvcCatalogue 的服务,K8s 上本来就不部署,与本开关无关);
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
	# 确认本次就是要切换形态(-ClientEntryMode / -BattleOrchestrator 换了取值)或改 gate / battle 的客户端地址来源,
	# 接受整台 gate 踢人、在打的战斗作废、改地址后须逐个排空重建 gate。
	# 不给时,只要"另一种形态"里会踢人的工作负载在集群里确实存在,或现有 gate / battle 的入口形态与地址来源会被改写,
	# 部署就在任何写操作之前整条拒绝、一个都不删。只在维护窗口里显式加;包装入口留空不透传。
	[switch]$AllowDisruptiveSwitch,

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
# battle 全局池的工作负载,由写路径入口 Assert-ClientEntryDeployPreflight 渲染(New-BattleWorkloadManifest),Apply-BattlePool 只 apply。
$script:BattleWorkload = $null

. (Join-Path $ScriptDir "lib\release_common.ps1")
# 集群外客户端入口(D76–D93)的生成器、preflight、就绪等待与模式切换清理。库函数不读本脚本的任何变量,
# 碰集群的函数一律显式传 -KubeContext / -KubeConfig(漏传不会悄悄落到本机默认 context)。
. (Join-Path (Join-Path $ScriptDir "lib") "k8s_client_entry.ps1")

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

	# gateway 管理面口令(见 $script:GatewayAdminApiKeyEnv 处的说明)。只在本次部署 gateway 时解析:infra-up 与不带 Java 服务的
	# zone-up 不因它被阻断。非 dev 档缺失 / 占位 / 过短由 Resolve-InjectedSecret throw;dev 档回落占位值,照样经 Secret 注入
	# (注入路径各档一致),但打警告 —— 管理面只在集群内可达(Ingress 只路由 /api),占位口令仍等于对集群内任何 Pod 开放。
	$script:GatewayAdminApiKey = ""
	if (Test-ZoneGatewayDeployed) {
		$script:GatewayAdminApiKey = Resolve-InjectedSecret -EnvName "MMORPG_GATEWAY_ADMIN_API_KEY" `
			-DevFallback "change-me-in-production" -ReleaseProfile $ReleaseProfile -Purpose "Java Gateway 管理面口令(X-Admin-Key / admin.api-key)" -MinLength 32
		if (Test-PlaceholderSecret -Value $script:GatewayAdminApiKey) {
			Write-Warning "gateway 管理面口令是占位值(MMORPG_GATEWAY_ADMIN_API_KEY 未设置或仍是占位串,ReleaseProfile=$ReleaseProfile):/admin/** 用公开常量鉴权,只允许本地 dev;非 dev 档会拒绝部署。"
		}
	}
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
# C++ gRPC 客户端 deadline 预算(docs/design/grpc-client-deadline-failure-callback.md §4.2 / §4.4)
# ─────────────────────────────────────────────────────────────────

<#
.SYNOPSIS
	纯函数:核对「C++ deadline ≥ 目标 Go 服务的 zrpc Timeout + 2000」(上游比下游宽),返回违例文本数组,空 = 通过。

.DESCRIPTION
	为什么是部署门禁而不只是注释:两个数分属两份配置(bin/etc 的 GrpcClient 块与 go/<svc>/etc 的 Timeout),
	改一边忘另一边不会有任何报错。C++ deadline 不比服务端超时宽时,C++ 先按自己的 DeadlineExceeded 收尾,
	拿不到服务端带真实错误码的应答,而服务端可能仍在执行 —— 本可确定的结果变成「结果未知」(设计 §3.3)。

	判定口径(每一项都是违例,不是警告):
	  - 目标必须在 GrpcClient.CallDeadlineMs 里显式写成正整数:缺席 / 0 在 C++ 侧落到内置默认 10000
	    (grpc_call_tag.h kDefaultGrpcCallDeadlineMs),预算不能依赖一个只写在 C++ 头文件里的隐式值。
	  - 服务 yaml 不写 Timeout = go-zero 默认 2000(go.mod 钉的 v1.9.2 / v1.10.0 的 zrpc/config.go 里
	    RpcServerConf.Timeout 都是 `default=2000`);写了就必须是正整数 —— 0 = go-zero 不装超时拦截器
	    (zrpc/server.go 只在 Timeout > 0 时装),服务端没有上界,「下游先超时」无从成立。
	  - 出现 MethodTimeouts:它能把个别方法的服务端超时放宽到全局 Timeout 之上,而这里只按全局 Timeout 核对;
	    真要用,先把它纳入本函数。

	只接收已拍平的 yaml(release_common.ps1 ConvertFrom-YamlToFlatMap 的 Scalars),不读文件,
	契约测试直接喂构造值验证每条口径(tools/scripts/tests/k8s_deploy_contract.tests.ps1)。

.PARAMETER DeployScalars
	bin/etc/base_deploy_config.yaml 拍平后的 Scalars,键形如 GrpcClient.CallDeadlineMs.SceneManagerNodeService。

.PARAMETER Targets
	有序字典:目标节点类型(ENodeType 枚举名)→ @{ Source = <服务 yaml 路径,只用于报错>; Scalars = <该 yaml 拍平后的 Scalars> }。
#>
function Get-GrpcClientDeadlineBudgetViolations {
	param(
		[Parameter(Mandatory = $true)][System.Collections.IDictionary]$DeployScalars,
		[Parameter(Mandatory = $true)][System.Collections.IDictionary]$Targets
	)

	# 余量与 go-zero 默认值是这条契约本身(设计 §4.2 不等式 1),不是调参旋钮,所以不开参数。
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

		$methodKeys = @($serviceScalars.Keys | Where-Object { $_ -like 'MethodTimeouts*' })
		if ($methodKeys.Count -gt 0) {
			$violations.Add("${target}:$source 出现 MethodTimeouts($($methodKeys -join ', ')),个别方法的服务端超时可能高于 C++ deadline;这里只按全局 Timeout 核对,要用先把它纳入 Get-GrpcClientDeadlineBudgetViolations")
			continue
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
	  DataServiceNodeService     ← go/data_service/etc/data_service.yaml(不写 Timeout,按 go-zero 默认)
	  ClientRpcRouterNodeService ← go/client_rpc_router/etc/client_rpc_router.yaml
	  MatchNodeService           ← go/match/etc/match_service.yaml(gate 直连模式)
	  LoginNodeService           ← go/login/etc/login.yaml(gate 直连模式)
	Battle / Etcd 没有 Go 服务端超时,不在表里;chat / friend / guild / team / trade 不在 C++ 节点白名单里,只经路由服到达。
	比对的是服务 yaml:scene-manager / match / 路由服 / login 的 go-svc ConfigMap 都从它镜像 Timeout
	(New-GoSvcConfigMapYaml);data-service 的 ConfigMap 不写 Timeout(= go-zero 默认 2000)—— 生成物同样满足
	这条不等式,由契约测试另外钉住(防镜像被改回常数、ConfigMap 键改名)。
#>
function Assert-GrpcClientDeadlineBudget {
	$deployPath = 'bin/etc/base_deploy_config.yaml'
	$targetSources = [ordered]@{
		SceneManagerNodeService    = 'go/scene_manager/etc/scene_manager_service.yaml'
		DataServiceNodeService     = 'go/data_service/etc/data_service.yaml'
		ClientRpcRouterNodeService = 'go/client_rpc_router/etc/client_rpc_router.yaml'
		MatchNodeService           = 'go/match/etc/match_service.yaml'
		LoginNodeService           = 'go/login/etc/login.yaml'
	}

	# 与 Get-AuthoritativeScalar 同一条纪律:文件缺席就 throw,不替你猜一个超时。
	$readScalars = {
		param([string]$RelativePath)
		$full = Join-Path $RepoRoot ($RelativePath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
		if (-not (Test-Path -LiteralPath $full)) {
			throw "gRPC deadline 预算核对失败:找不到 $RelativePath(fail-closed:不替你猜一个超时)"
		}
		return (ConvertFrom-YamlToFlatMap -Text (Get-Content -LiteralPath $full -Raw)).Scalars
	}

	$targets = [ordered]@{}
	foreach ($target in $targetSources.Keys) {
		$targets[$target] = @{ Source = $targetSources[$target]; Scalars = (& $readScalars $targetSources[$target]) }
	}
	$violations = @(Get-GrpcClientDeadlineBudgetViolations -DeployScalars (& $readScalars $deployPath) -Targets $targets)
	if ($violations.Count -gt 0) {
		throw ("C++ gRPC 客户端 deadline 预算不成立,拒绝部署(上游比下游宽:C++ deadline ≥ Go zrpc Timeout + 2000," +
			"docs/design/grpc-client-deadline-failure-callback.md §4.2):`n  - " + ($violations -join "`n  - "))
	}
	Write-Host ("GrpcClient deadline budget OK (C++ deadline >= Go zrpc Timeout + 2000): {0}" -f ($targetSources.Keys -join ', '))
}

# Go micro-service catalogue: name → { configMapName, manifestFile, port, configFlag, configFileName }
#   Global = $true 的条目是**全局池**服务:只在 infra-up / all-up 的基础设施阶段部署到
#   $InfraNamespace 一次,zone-up 跳过(见 Apply-GlobalGoSvcManifests)。
$GoSvcCatalogue = @{
	db              = @{ ConfigMap = "go-svc-db-config";              Manifest = "db.yaml";              Port = 6000;  ConfigFlag = "-f";              ConfigFile = "db.yaml";                    ImageName = "mmorpg-db" }
	"data-service"  = @{ ConfigMap = "go-svc-data-service-config";    Manifest = "data-service.yaml";    Port = 9000;  ConfigFlag = "-f";              ConfigFile = "data_service.yaml";             ImageName = "mmorpg-data-service" }
	login           = @{ ConfigMap = "go-svc-login-config";           Manifest = "login.yaml";           Port = 50000; ConfigFlag = "-loginService";   ConfigFile = "login.yaml";                  ImageName = "mmorpg-login" }
	"player-locator"= @{ ConfigMap = "go-svc-player-locator-config";  Manifest = "player-locator.yaml";  Port = 50100; ConfigFlag = "-f";              ConfigFile = "player_locator.yaml";           ImageName = "mmorpg-player-locator" }
	"scene-manager" = @{ ConfigMap = "go-svc-scene-manager-config";   Manifest = "scene-manager.yaml";   Port = 60000; ConfigFlag = "-f";              ConfigFile = "scene_manager_service.yaml";    ImageName = "mmorpg-scene-manager" }
	# 回合制战斗匹配:全局池、不分 zone(docs/design/cross-zone-matchmaking.md D1/D10),
	# 与 battle 池同形态。gate 发现它走非 zone-scoped 前缀,所以放 infra namespace 一份即可。
	match           = @{ ConfigMap = "go-svc-match-config";           Manifest = "match.yaml";           Port = 50500; ConfigFlag = "-f";              ConfigFile = "match_service.yaml";            ImageName = "mmorpg-match"; Global = $true }
	# 全局聊天 chat v1:世界频道全服唯一、私聊 key 不含 zone,表里天然有跨 zone 的行,所以是全局池(Global),
	# 与 match 同形态放 infra namespace 一份。客户端只经 gate → client-rpc-router 到达它:-GateRouterMode 默认 "1" 下可达
	# (K8s 上路由模式整链待事后补验,turn-based §22 D75);以 "0" 回退时「部署得起来但玩家不可达」,见下一条与参数注释。
	chat            = @{ ConfigMap = "go-svc-chat-config";            Manifest = "chat.yaml";            Port = 50700; ConfigFlag = "-f";              ConfigFile = "chat.yaml";                     ImageName = "mmorpg-chat"; Global = $true }
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
# id_segment 四张表由 data_service 启动期(或 -migrate)建在这里,所以库必须预先存在且账号有
# CREATE 权限:infra-up 的 mysql-init-sql ConfigMap 会生成 02_k8s_global_db.sql 预建它并 GRANT 给
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
		Write-Host "Kafka broker 扩容:$live → $($Topology.Brokers)。新 broker 起来后**不会**自动接手已有 topic 的分区,需要用 kafka-reassign-partitions.sh 把副本挪过去(README「Kafka:多 broker」)。"
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
# 控制面命令 topic 1 小时(kafka-topic-init)、match-results 7 天(match 侧常量)。
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
		}
		default {
			if ($KafkaBrokerRetentionMs -le 0) { $script:KafkaBrokerRetentionMs = 3600000 }
			# 24 小时:与 go/db/etc/db.yaml、go/login/etc/login.yaml 的 RetentionMs 默认值
			# (86400000)对齐。以前这里是 900000,而 db 的 ConfigMap 不写 RetentionMs、
			# 走 Go 结构体默认的 24h —— login 与 db 都会在启动时对同一个 db_task_zone_<N>
			# 执行 IncrementalAlterConfigs(kafkautil.EnsureTopics),于是 topic 的保留期
			# 在 15 分钟和 24 小时之间**随两个服务的重启顺序反复横跳**。对齐之后不再漂。
			if ($KafkaDbTaskRetentionMs -le 0) { $script:KafkaDbTaskRetentionMs = 86400000 }
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
	if ($ClientEntryMode -eq "external") {
		# external:gate 在发布进 etcd 之前自报客户端可达地址(NodeInfo.client_endpoint,D76),login / scene_manager 下发的
		# 就是它;每序号 Service gate-<i> 把这个地址接到同序号的 Pod。其余组合的合法性由 Assert-ClientEntryDeployPreflight 校验。
		return
	}
	if ($GateServiceType -ne "ClusterIP") {
		# podip:暴露 Service 只是一半。客户端的 gate 地址不是从 Service 拿的,而是 login 从 etcd 读到 endpoint 后
		# 下发的(go/login internal/svc/servicecontext.go CandidatesForZone),podip 下那就是 POD_IP —— 集群内地址,
		# 只有 in-cluster 的 robot 连得上。集群外客户端要走 -ClientEntryMode external(D76–D93)。
		Write-Warning "GateServiceType=$GateServiceType 只是把 Service 暴露了出去:-ClientEntryMode podip 下 login 下发的是 gate 在 etcd 里注册的集群内 POD_IP,集群外客户端仍然连不上。对外开放请用 -ClientEntryMode external(gate StatefulSet + 每序号 Service,见 docs/design/k8s-client-entry.md);单一 gate-entry 只在 gate 副本数为 1 时生成(D90)。"
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

<#
.SYNOPSIS
	集群外客户端入口的部署前校验,写操作入口调用,早于任何集群写操作。三步,任何一步失败都 throw:
	  1. 参数组合:lib Test-ClientEntryPreflight,并入本脚本 -RequireClientEndpoint 的矛盾检查,一次报全;
	  2. 渲染 battle 工作负载(New-BattleWorkloadManifest,DryRun 同样渲染),结果存 $script:BattleWorkload 给 Apply-BattlePool;
	  3. 集群现状(Assert-ClientEntryClusterState,DryRun 跳过):会踢人、会静默改写入口的情形,battle 与各 zone 一次判定完。

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
		Write-Warning "[client-entry] 给了 -LoginDevPasswordAuth,但本次不部署 login(它只随 zone-up / all-up 的 Go 服务生成,需要 -GoSvcRegistry 且不带 -SkipGoSvc),开关不生效。"
	}

	# battle 工作负载在这里渲染(Apply-Infra 开头"battle 配置在任何基础设施写操作之前生成并校验"的同一约定):
	# Resolve-BattleFleetHealth 与生成器的参数校验都可能 throw,不能等 etcd / kafka / redis / mysql 都 apply 完才发现。
	$script:BattleWorkload = $null
	if ($deploysBattle) {
		$script:BattleWorkload = New-BattleWorkloadManifest
	}

	if ($DryRun) {
		Write-Host "[dry-run] 跳过集群现状预检(gate / battle 的形态与地址来源、gateway Ingress):真实执行时在任何写操作之前判定。"
	}
	else {
		Assert-ClientEntryClusterState -Zones $zones -DeploysBattle $deploysBattle -DeploysGateway $deploysGateway `
			-AgonesFleetCrdPresent $agonesFleetCrdPresent
	}
	Write-Host "Client entry: mode=$ClientEntryMode battle_orchestrator=$BattleOrchestrator gate_service_type=$GateServiceType gate_etp=$GateExternalTrafficPolicy require_client_endpoint=$(Resolve-RequireClientEndpoint)($RequireClientEndpoint) ingress_host=$(if ($GatewayIngressHost) { $GatewayIngressHost } else { '<none>' }) login_dev_password_auth=$($LoginDevPasswordAuth.IsPresent) allow_disruptive_switch=$($AllowDisruptiveSwitch.IsPresent)"
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
		$drifts += @(Get-BattleEntryDrift -AgonesFleetCrdPresent $AgonesFleetCrdPresent)
	}

	# New-GatewayRateLimitYaml 为空 = 本次 gateway ConfigMap 不含 trusted-proxies(与 New-JavaSvcConfigMapYaml 同一判定)。
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
			$drifts += @(Get-GateAddressDrift -Zone $zone -Namespace $namespace)
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
			Write-Warning "[client-entry] 已给 -AllowDisruptiveSwitch,放行:$drift"
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

	# C++ unary gRPC 调用的 deadline(docs/design/grpc-client-deadline-failure-callback.md §4.4)。与 IdSegments 同法整块搬运:
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
LogLevel: 1
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
Kafka:
  Brokers:
    - "kafka.${InfraNamespace}:9092"
  Topics:
    - "game-events"
  GroupID: "game-consumer-group"
  CommandTopicPartitions: ${commandTopicPartitions}
  CommandTopicGeneration: ${commandTopicGeneration}
  EnableAutoCommit: true
  AutoOffsetReset: "earliest"
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
# C++ 节点每次 unary gRPC 调用的 deadline(毫秒),键 = 目标 ENodeType 枚举名(docs/design/grpc-client-deadline-failure-callback.md §4)。
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
	$sidecarAnnotationBlock = ""
	if ($script:CppLogSidecarEnabled) {
		$zoneLabelForSidecar = if ([string]::IsNullOrWhiteSpace($CurrentZoneName)) { "global" } else { $CurrentZoneName }
		# 放 initContainers + restartPolicy: Always(原生 sidecar),理由见 New-CppLogSidecarContainerYaml。
		# initContainers 与 containers 共用同一个 spec.volumes,所以卷片段不动。
		$sidecarInitBlock = "`n      initContainers:`n" + (New-CppLogSidecarContainerYaml -ItemIndent "        " -CurrentZoneName $zoneLabelForSidecar -AsNativeSidecar)
		$sidecarVolumeBlock = "`n" + (New-CppLogSidecarVolumeYaml -ItemIndent "        ")
		# 哈希加引号:12 位十六进制可能全是数字(如 123456789012)或是 12345678e123 这类科学计数法,裸写会被 YAML 解析成数字,annotation 值必须是字符串,API server 拒收。
		$sidecarAnnotationBlock = "`n      annotations:`n        mmorpg.io/cpp-log-sidecar-config-hash: `"$(Get-CppLogSidecarConfigHash)`""
	}

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
	# node-logs emptyDir 会遮住镜像目录；Node 构造器立即打开 logs/cpp_nodes/<role>，先建父目录。
	$grpcEnvBlock = $extraEnvLines -join "`n"
	# 端口与探针按角色拼在 ports 列表之后(推导与证据见 PROGRESS「2026-09-25 C++ 节点就绪探针」)。
	# 三个角色共同的约束:
	#  - C++ 尚未注册 grpc.health.v1,只能用 tcpSocket 探**实际在监听**的口;端口一律写数字,不写端口名。
	#  - 两个口都在 etcd 注册完成之后才 listen,所以 tcpSocket 通 = 「已发布进 etcd」,**不等于**依赖门
	#    (gate 等 ClientRpcRouter / Scene —— -GateRouterMode "0" 回退时是 Login / Scene,scene 等 SceneManager 与号段首段)已过。
	#    readiness 只能表达前者。
	#  - 不加 livenessProbe:tcpSocket 看不出 EventLoop 卡死(内核照样完成握手),加了只多一条杀容器的路。
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
	metadata:$sidecarAnnotationBlock
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
			  name: rpc
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
	Write-Host "  kind 集群需先把 $CppLogSidecarImage 载入节点:拉不到镜像时 gate/scene/battle 会卡在 Init:ErrImagePull,业务容器根本不启动(预载命令见 deploy/k8s/README.md)。不想要就加 -NoCppLogSidecar。"

	if ($DryRun) { return }
	# 只有用集群内默认 Loki 时才探测得了;-LokiPushUrl 指向外部时这里无从判断。
	if (-not [string]::IsNullOrWhiteSpace($LokiPushUrl)) { return }

	$probeArgs = @()
	$probeArgs += Build-KubectlBaseArgs
	$probeArgs += @("-n", $InfraNamespace, "get", "svc", "loki", "--ignore-not-found", "-o", "name")
	$found = & kubectl @probeArgs 2>$null
	if ($LASTEXITCODE -ne 0) {
		Write-Warning "没能确认 $InfraNamespace 里是否有 Loki Service(kubectl get svc loki 执行失败),C++ 日志可能推不出去。"
		return
	}
	if ([string]::IsNullOrWhiteSpace(($found -join ''))) {
		Write-Warning "sidecar 会把 C++ 日志推向 $script:CppLogSidecarPushUrl,但 $InfraNamespace 里没有 loki Service。先跑一次 -Command infra-up(别带 -SkipInfra),或用 -LokiPushUrl 指向已有的 Loki,或 -NoCppLogSidecar 关掉 —— 否则部署会成功,日志一条也查不到。"
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
	$sidecarAnnotationBlock = ""
	$gameServerContainerLine = ""
	if ($script:CppLogSidecarEnabled) {
		$sidecarInitBlock = "`n          initContainers:`n" + (New-CppLogSidecarContainerYaml -ItemIndent "            " -CurrentZoneName $ZoneLabel -AsNativeSidecar)
		$sidecarVolumeBlock = "`n" + (New-CppLogSidecarVolumeYaml -ItemIndent "            ")
		# 哈希加引号,理由同 New-NodeDeploymentYaml 的同名注解。
		$sidecarAnnotationBlock = "`n          annotations:`n            mmorpg.io/cpp-log-sidecar-config-hash: `"$(Get-CppLogSidecarConfigHash)`""
		$gameServerContainerLine = "      container: $FleetName`n"
	}

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
        metadata:$sidecarAnnotationBlock
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
                  value: "1"$grpcPollersEnv
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
                  name: rpc
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
	把本脚本的 C++ 节点公共参数(镜像、拉取策略、发号槽缓存、日志卷与清理循环、gRPC poller、日志 sidecar)
	打包成 lib/k8s_client_entry.ps1 生成器要的 PodCommon。与 New-NodeDeploymentYaml 同一组脚本变量,一处取值。

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
	return New-ClientEntryPodCommon -Image $NodeImage -ImagePullPolicy $ImagePullPolicy -SnowflakeCacheDir $SnowflakeCacheDir `
		-LogVolumeSizeLimit $script:CppLogVolumeSizeLimit -LogPrunerCommand $script:CppLogPrunerCommand `
		-GrpcServerMaxPollers $GrpcServerMaxPollers @sidecar
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
		Wait-ForGateStatefulSetReady -Namespace $Namespace -ExpectedReplicas $GateReplicas -TimeoutSeconds $WaitTimeoutSeconds `
			-KubeContext $KubeContext -KubeConfig $KubeConfig -DryRun:$DryRun
	}
	else {
		Wait-ForDeploymentReady -Namespace $Namespace -DeploymentName "gate"
	}

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
				Write-Host "  [skip-wait] ${svcName}: manifest 不存在,未部署,不等待"
				continue
			}
			Wait-ForDeploymentReady -Namespace $Namespace -DeploymentName $svcName
		}
	}
	Write-Host "Zone workloads are ready: namespace=$Namespace"
}

function New-GoSvcConfigMapYaml {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		[Parameter(Mandatory = $true)][int]$CurrentZoneId,
		[Parameter(Mandatory = $true)][int]$CurrentClusterId
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
	# PlayerId 号段长度(node-id-overhaul-plan §6.2:按「10 分钟峰值建角量」配)。IdSegment.Enabled 与
	# FallbackToSnowflake **不**从 yaml 取:那两个是部署决策,在 login 模板里显式写死并注释。
	$loginIdSegmentStep     = Get-AuthoritativeScalar -RelativePath 'go/login/etc/login.yaml' -KeyPath 'IdSegment.Step'
	# zrpc 服务端 Timeout(毫秒)从服务 yaml 镜像,与 scene-manager / match 同一条纪律。以前模板里写死 100000:
	# 部署门禁 Assert-GrpcClientDeadlineBudget 核对的是 login.yaml,两边一分家,门禁放行的就不是集群里真正生效的值。
	# 非正整数在这里就拒 —— 0 = go-zero 不装超时拦截器(服务端没有上界),非数字 go-zero 起服即失败,都不该写进 ConfigMap。
	# 值本身 2026-09-29 已由笔误 100000 改正为 10000(LoginNodeService 同步 12000,见 grpc-client-deadline-failure-callback.md §4.3);
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
	$dsMysqlMaxOpenConn           = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'SnapshotMySQL.MaxOpenConn'
	$dsMysqlMaxIdleConn           = Get-AuthoritativeScalar -RelativePath $dsYaml -KeyPath 'SnapshotMySQL.MaxIdleConn'
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
	# 失败语义见 grpc-client-deadline-failure-callback.md §3.3)。三者之间的不等式由 tests/k8s_deploy_contract.tests.ps1 守着;
	# 反方向的 C++ deadline(GrpcClient.CallDeadlineMs.SceneManagerNodeService)≥ 本 Timeout + 2000 由写路径入口的
	# Assert-GrpcClientDeadlineBudget 守着 —— 调大这里的 Timeout 必须同步调那边,否则部署被拒。
	$sceneManagerYaml                  = 'go/scene_manager/etc/scene_manager_service.yaml'
	$sceneManagerTimeout               = Get-AuthoritativeScalar -RelativePath $sceneManagerYaml -KeyPath 'Timeout'
	$sceneManagerKafkaWriteTimeout     = Get-AuthoritativeScalar -RelativePath $sceneManagerYaml -KeyPath 'KafkaWriteTimeoutSeconds'
	$sceneManagerHomeZoneLookupTimeout = Get-AuthoritativeScalar -RelativePath $sceneManagerYaml -KeyPath 'HomeZoneLookupTimeoutMs'

	# match 的时间窗口全是"多实例 / 崩溃自愈"语义(锁 TTL、票据 TTL、战斗时限),
	# 生成器里另抄一份就是又一处会漂移的常数,一律从服务自己的 yaml 取。
	$matchYaml                  = 'go/match/etc/match_service.yaml'
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
	if ($SvcName -eq 'chat') {
		# Name 也取服务 yaml:go-zero ServiceConf.Name 是必填键,两边写同一个名字,日志 / 链路里才对得上。
		$chatName               = Get-AuthoritativeScalar -RelativePath $chatYaml -KeyPath 'Name'
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
	if ($SvcName -eq 'friend') {
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
	#
	# 只在生成 db 自己的 ConfigMap 时解析:这是 db 独有的注入项,以前无条件求值只是
	# 顺手;现在全局服务(match)会在 infra-up 阶段也走这个函数,staging/prod 的
	# infra-up 不该因为一个 db 才用的密钥缺席而失败。
	$dbAllowedDatabasesYaml = ''
	if ($SvcName -eq 'db') {
		$dbAllowedDatabases = Resolve-InjectedSecret -EnvName "MMORPG_DB_ALLOWED_DATABASES" `
			-DevFallback "zone_${CurrentZoneId}_db" -ReleaseProfile $ReleaseProfile `
			-Purpose "db 库名白名单(逗号分隔,如 zone_1_db,zone_2_db)" -MinLength 3
		$dbAllowedList = @($dbAllowedDatabases -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ })
		if ($dbAllowedList.Count -eq 0) {
			throw "生成 db ConfigMap 失败:库名白名单解析后为空(MMORPG_DB_ALLOWED_DATABASES='$dbAllowedDatabases')。"
		}
		$dbAllowedDatabasesYaml = ($dbAllowedList | ForEach-Object { "      - `"$_`"" }) -join "`n"
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

	# data_service 全局库四张表的建表策略(go/data_service/internal/config/config.go SchemaConfig),
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
    # 顶层 Placement 段不写 = 整段取缺省(AllowStoreFamilies=true、Required=false、Redis 复用本段)。
    # 所以它必须与 data-service ConfigMap 的 MappingRedis 同实例、DB 0 —— 读错库 = 读不到任何记录与 home_zone,
    # 全员按本 zone 选库,被钉到别处的玩家会写错库。契约测试钉住两者同址。
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
# MappingRedis 刻意不写 DB 键:go-zero 的 redis.RedisConf 没有 DB 字段,以前这里的 `DB: 15` 被静默忽略,
# 映射一直落在 DB 0(与 go/data_service/etc/data_service.yaml 的注释同一结论)。留着它只会误导运维按 15 去查 /
# 给 merge_zone 填 -mapping-redis-db 15,让合服围栏与改映射打进 data_service 从不读的库。
# player:zone / player:placement / merge:in_progress / db:capability:zone 都在这个实例的 DB 0;
# go/db 的 Placement.Redis 缺省复用它自己的 RedisClient(同一实例、DB 0),所以 go-svc-db ConfigMap 不写 Placement 段。
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
# id_segment 四张表都在这里,由 data_service 按 proto 建表。库名 ${GlobalDbName} 由 infra-up 的
# mysql-init-sql(02_k8s_global_db.sql)预建并 GRANT 给 appuser;账号沿用 db 服务同一组注入值
# (MMORPG_MYSQL_USER / MMORPG_MYSQL_PASSWORD,dev 回落 root),换成别的账号要自己补 GRANT。
# 以前这份 ConfigMap 没有本段,Go 默认值是 127.0.0.1:3306/testdb —— K8s 上必然连不上:
# 三个 store 置 nil、AllocateIdSegment 不可用,login 开了 IdSegment 后建角会全部失败。
SnapshotMySQL:
  Host: "mysql.${InfraNamespace}:3306"
  User: "${mysqlUser}"
  Password: "${mysqlPassword}"
  DBName: "${GlobalDbName}"
  MaxOpenConn: ${dsMysqlMaxOpenConn}
  MaxIdleConn: ${dsMysqlMaxIdleConn}
# 建表策略(见生成器注释):dev = 服务 yaml 的值;staging/prod = false,部署阶段跑 -migrate。
Schema:
  AutoMigrate: ${dataServiceAutoMigrate}
# id_segment 行播种策略(见生成器注释):dev = 服务 yaml 的值;staging/prod = false,缺行不自动播种,
# 由人核对消费侧最大号后处理(README「恢复全局库前须先核对 id_segment.max_id」)。
# BootstrapTags 与服务 yaml / DefaultIdSegmentBootstrapTags 同一份清单:迁移(AutoMigrate / -migrate)
# 用 INSERT IGNORE 预建这些 biz_tag 行,幂等、绝不降低已有行的 max_id;新增永久身份两边同加。
IdSegment:
  AllowAutoSeed: ${dataServiceAllowAutoSeed}
  BootstrapTags: [player, guild, item, txlog, snapshot, trade_listing, guild_asset_op]
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
# DataService gRPC 客户端:login 只用它领 PlayerId 号段(DataService.AllocateIdSegment)。
# NonBlock: true —— 号段是弱依赖(node-id-overhaul-plan §6.5):data-service 没起时 login 照常起服,
# 领段失败按 IdSegment.FallbackToSnowflake 决定回退还是拒绝建角。
DataServiceRpc:
  Etcd:
    Hosts:
      - "etcd.${InfraNamespace}:2379"
    Key: dataservice.rpc
  Timeout: 3000
  NonBlock: true
  Middlewares:
    Breaker: false
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
    Value: "${internalAuthSecret}"
# 下发 gate 地址时是否必须用 gate 自报的客户端可达地址(NodeInfo.client_endpoint,D78):
# true = 缺地址的 gate 不下发;false = 回落 endpoint(PodIP)。取值见 k8s_deploy.ps1 -RequireClientEndpoint(默认跟随 -ClientEntryMode)。
RequireClientEndpoint: ${requireClientEndpointValue}${loginDevPasswordAuthBlock}
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
# DataService gRPC 客户端:EnterScene 用它查玩家归属 zone(GetPlayerHomeZone),随 RoutePlayerEvent
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
Team:
  AllowCrossZone: ${matchTeamAllowCrossZone}
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
# 与 match.yaml 里的 metrics 容器端口一致
MetricsListenAddr: ":9170"
"@
		}
		"chat" {
@"
Name: ${chatName}
ListenOn: 0.0.0.0:50700
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
# 双句柄:
#   Redis     = SharedRedis,既有共享单库(写者含 C++ scene,不能集群化);chat 私有 key 只在 ChatRedis 缺省时才回落到这里。
#   ChatRedis = chat 私有 key(chat:{world}:log / chat:{p:<小id>:<大id>}:log / chat:{req:...} / chat:{rl:...}),
#               全是单 key 操作,集群安全。go-zero redis.RedisConf 形状,不写 DB。
Redis:
  Host: redis.${InfraNamespace}:6379
  Type: node
  Pass: "${redisPassword}"
  Key: chatservice
# 注意:ChatRedis 复用 match 的 redis-match-cluster(同一个六节点集群),不是 chat 的独立实例。
#   该集群 maxmemory 512mb + maxmemory-policy volatile-lru(manifests/infra/redis-match-cluster.yaml):
#   chat 的历史 LIST / 幂等 / 限速 key 都带 TTL,内存吃紧时会被优先淘汰,还会与 match 的锁 / 票据争同一份内存,
#   聊天历史可能提前消失、反过来也可能挤掉 match 带 TTL 的 key。
#   v1 可接受的前提是「历史 = 7 天 / 200 条尽力而为窗口,不是权威账本」;一旦 chat 的某类数据成为唯一权威,
#   staging/prod 必须换独立实例且 maxmemory-policy=noeviction,并按 chat 的量级重新定 maxmemory。
ChatRedis:
  Host: ${matchRedisClusterHosts}
  Type: cluster
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
# 换成别的账号要自己补 GRANT。MaxOpenConn × Deployment 副本数(2)要算进 MySQL 的 max_connections。
MySQL:
  Host: "mysql.${InfraNamespace}:3306"
  User: "${mysqlUser}"
  Password: "${mysqlPassword}"
  DBName: "${tradeMysqlDBName}"
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
AssetOp:
  Enabled: false
  SecretEnv: MMORPG_ASSET_OP_SECRET_TRADE
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
#   **配额 / 限流 key 被淘汰等于限流静默失效**。共享库现在是 allkeys-lfu,这条风险已经存在,写在这里不是假装没有。
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
# 独占库(port-decisions D-14):库 ${friendMysqlDBName} 由 infra-up 的 mysql-init-sql(原样带入 deploy/mysql-init/00_init_zone_dbs.sql)
# 预建并 GRANT 给 appuser;账号沿用 db / data-service 同一组注入值(MMORPG_MYSQL_USER / MMORPG_MYSQL_PASSWORD,dev 回落 root),
# 换成别的账号要自己补 GRANT。MaxOpenConn × Deployment 副本数(2)要算进 MySQL 的 max_connections。
MySQL:
  Host: "mysql.${InfraNamespace}:3306"
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
	# initContainer 等 MySQL 用的地址:Job 与 MySQL 不一定同 namespace(目录允许非 Global 服务将来也挂 MigrateJob),写 FQDN 前缀。
	$content = $content.Replace("__INFRA_NAMESPACE__", $InfraNamespace)
	# 与 Apply-Infra 同一条 fail-closed:任何占位没替换就拒绝 apply,不把占位原样送进集群。
	$leftover = [regex]::Matches($content, '(__[A-Z][A-Z0-9_]*__|PLACEHOLDER_[A-Z_]+)') | ForEach-Object { $_.Value } | Sort-Object -Unique
	if ($leftover.Count -gt 0) {
		throw "migrate Job manifest $($info.MigrateJob) 里有未替换的占位:$($leftover -join ', ')。请在 Apply-GoSvcMigrateJob 里补对应的 Replace。"
	}

	# D-14 第 4 条:staging/prod 的迁移门禁不受 -WaitReady 控制,恒等 Job Complete、失败或超时中断发布;
	# 只有 dev 档把"等不等"交给 -WaitReady(dev 服务启动期自己也会迁移)。
	$gateOnJob = [bool]$WaitReady -or ($ReleaseProfile -ne 'dev')

	if ($gateOnJob) {
		# MySQL 起不来时 -migrate 连库失败以 1 退出,会被 podFailurePolicy 当成不可重试直接判 Job 失败;
		# 先等 mysql Deployment 滚动完成,把"镜像还在拉 / Pod 还没调度"这类原因挡在 Job 之外,失败时报的也是真实原因。
		# (本函数只经 Apply-Infra → Apply-GlobalGoSvcManifests 调到,mysql.yaml 在同一次 Apply-Infra 里已 apply。)
		Wait-ForDeploymentReady -Namespace $InfraNamespace -DeploymentName "mysql"
	}

	# 删之前确认上一次的 Job 不在途:删掉在途迁移会打断 DDL、把台账留在 dirty(理由见函数头注释)。
	Assert-GoSvcMigrateJobNotInFlight -Namespace $Namespace -JobName $jobName

	# Job 名固定,重跑必须先删:template 不可变,apply 同名 Job 会被拒。--ignore-not-found:首次是 no-op。
	Invoke-Kubectl -Args @("delete", "job", $jobName, "-n", $Namespace, "--ignore-not-found")
	Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $content
	Write-Host "  [applied] $jobName -> $SvcImage (D-14 建表 Job,先于 $SvcName Deployment)"

	if ($gateOnJob) {
		Wait-ForGoSvcMigrateJob -Namespace $Namespace -JobName $jobName -SvcName $SvcName
		return
	}
	Write-Warning ("dev 档未带 -WaitReady:不等 {0} 结束就 apply {1} Deployment(staging/prod 恒等,不走这里)。核对 'kubectl -n {2} get job {0}'(COMPLETIONS 1/1)与 'kubectl -n {2} logs job/{0}'。" -f $jobName, $SvcName, $Namespace)
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
			if ($result.ExitCode -ne 0) { Write-Warning "迁移诊断不可用: $($result.ErrorOutput)" -WarningAction Continue }
		} catch {
			Write-Warning "迁移诊断不可用: $($_.Exception.Message)" -WarningAction Continue
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
		Write-Host "上一次 $JobName 可安全替换(state=$state,UID 所属 Pod 均已终止),继续先删再建。"
		return
	}

	try { Write-GoSvcMigrateJobDiagnostics -Namespace $Namespace -JobName $JobName } catch { Write-Warning "迁移诊断失败: $($_.Exception.Message)" -WarningAction Continue }
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
	try { Write-GoSvcMigrateJobDiagnostics -Namespace $Namespace -JobName $JobName } catch { Write-Warning "迁移诊断失败: $($_.Exception.Message)" -WarningAction Continue }
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
	往服务 manifest 唯一的 Pod 模板(template: 紧跟 metadata:)新增 annotations: 段,写一条注解。
	用于让"只改 Secret"这类 Pod 模板本身看不见的变更也能触发滚动(见 Get-InjectedSecretFingerprint)。

.DESCRIPTION
	形状要求:恰好一处 template: 下一行是 metadata:,且该 metadata 下还没有 annotations: —— 已有就 throw,
	不猜怎么合并(重复键的 YAML 各解析器行为不一)。与 Add-GoSvcSecretEnv 一样在本服务任何集群写操作之前调用。
	值一律写成双引号标量:12 位十六进制可能全是数字或形如 1e5…,不加引号会被 YAML 解析成数字,注解值必须是字符串。
#>
function Add-PodTemplateAnnotation {
	param(
		[Parameter(Mandatory = $true)][string]$SvcName,
		[Parameter(Mandatory = $true)][string]$ManifestContent,
		[Parameter(Mandatory = $true)][string]$Key,
		[Parameter(Mandatory = $true)][string]$Value
	)

	if ($Value -match '["\\\r\n]') {
		throw "内部错误:服务 $SvcName 的注解 $Key 的值含引号、反斜杠或换行,拒绝渲染。"
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
	$injected = @(
		"${childIndent}# 由 k8s_deploy.ps1 生成(Add-PodTemplateAnnotation),manifest 里不要手写",
		"${childIndent}annotations:",
		('{0}  {1}: "{2}"' -f $childIndent, $Key, $Value)
	) -join $newline
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
		Write-Host "[dry-run] kubectl apply -n $Namespace -f -   # Secret $Name(key $Key,值已隐去)"
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

# 单个 Go 服务的 ConfigMap + 主 manifest apply。zone 循环与全局循环共用同一段逻辑。
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
	$commandTopic = Get-GoSvcCommandTopicContract
	$manifestContent = Add-GoSvcCommandTopicEnv -SvcName $SvcName -ManifestContent $manifestContent `
		-Partitions $commandTopic.Partitions -Generation $commandTopic.Generation
	$manifestContent = Add-GoSvcTopicReplicationEnv -SvcName $SvcName -ManifestContent $manifestContent `
		-ReplicationFactor (Get-KafkaTopology).ReplicationFactor

	# -LoginDevPasswordAuth:先本地渲染(形状不对就在写操作之前 throw),再建 Secret —— secretKeyRef 不是 optional,
	# Secret 必须先于引用它的 Deployment 落地。
	$isLoginDevPassword = ($SvcName -eq 'login' -and $LoginDevPasswordAuth)
	if ($isLoginDevPassword) {
		$manifestContent = Add-GoSvcSecretEnv -SvcName $SvcName -ManifestContent $manifestContent `
			-EnvName (Get-ClientEntryContract).LoginDevPasswordSecretEnv `
			-SecretName $script:LoginDevPasswordSecretName -SecretKey $script:LoginDevPasswordSecretKey
		Apply-LoginDevPasswordSecret -Namespace $Namespace
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

	Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $manifestContent

	if ($SvcName -eq 'login' -and -not $isLoginDevPassword) {
		# 开关关闭:ConfigMap 已不含 DevPasswordAuth,新 Deployment 也不再引用口令,删掉残留 Secret(放在 Deployment
		# apply 之后,不让新 Pod 因引用缺失的 Secret 卡在 CreateContainerConfigError)。
		Invoke-Kubectl -Args @("delete", "secret", $script:LoginDevPasswordSecretName, "-n", $Namespace, "--ignore-not-found")
	}

	Write-Host "  [applied] $SvcName -> $svcImage (port $($info.Port) pullPolicy=$svcPullPolicy)"
}

# 全局池 Go 服务(目录里 Global = $true,目前是 match / chat / client-rpc-router / trade / friend):部署到 $InfraNamespace 一次。
# trade / friend 各带一个 MigrateJob(共两个 Job):trade-migrate / friend-migrate 在 Apply-OneGoSvc 里先于各自的 Deployment 跑
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
				Write-Host "  [skip-wait] ${svcName}: manifest 不存在,未部署,不等待"
				continue
			}
			Wait-ForDeploymentReady -Namespace $InfraNamespace -DeploymentName $svcName
		}
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

	# gate.rate-limit.trusted-proxies(D91):gateway 只采信这些网段转发来的 X-Forwarded-For。片段接在下面已有的 gate: 键下
	# (不能另起一个 gate:),只写 application.yaml 这一处、不另设 env(ingress_final §3 二选一)。列表为空 = 不写 = 只信
	# socket 对端(fail-closed);非法 CIDR 由 New-GatewayRateLimitYaml throw(preflight 已先拒)。
	$gatewayRateLimitBlock = ''
	if ($SvcName -eq 'gateway') {
		$gatewayRateLimitBlock = New-GatewayRateLimitYaml -TrustedProxies $GatewayTrustedProxies -Indent '  '
		if (-not [string]::IsNullOrEmpty($gatewayRateLimitBlock)) { $gatewayRateLimitBlock = "`n" + $gatewayRateLimitBlock }
	}
	# admin.api-key 只写成对容器环境变量的引用(值经 Secret 注入,见 $script:GatewayAdminApiKeyEnv 处)。挂载的
	# /app/config/application.yaml 优先于 jar 内那份,所以公开的 change-me-in-production 被这行盖掉;env 缺失时
	# @Value("${admin.api-key}") 解析不了嵌套占位符,gateway 拒启(fail-closed)。拼成 PowerShell 变量再嵌进下面的
	# 双引号 here-string,既不用转义 $,env 名也只有一处真相。
	$gatewayAdminApiKeyRef = '${' + $script:GatewayAdminApiKeyEnv + '}'

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
		[Parameter(Mandatory = $true)][string]$CurrentZoneName
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
		$cmYaml = New-JavaSvcConfigMapYaml -SvcName $svcName
		$svcPullPolicy = Resolve-ImagePullPolicy -ImageRef $svcImage
		$manifestContent = (Get-Content $manifestPath -Raw) -replace 'PLACEHOLDER_IMAGE', $svcImage
		$manifestContent = $manifestContent -replace 'PLACEHOLDER_PULL_POLICY', $svcPullPolicy

		# gateway 管理面口令(admin.api-key)只经 Secret 进容器(见 $script:GatewayAdminApiKeyEnv 处的说明):env 走 secretKeyRef,
		# Pod 模板带口令指纹注解 —— 只改 Secret 时模板不变、apply 是 no-op,旧 Pod 会一直认旧口令;指纹让轮换自动滚动。
		# gateway 的 Ingress(D91)与 gateway Deployment 同处:只在 gateway 确实部署了才生成(manifest 缺席在上面就 continue 了)。
		# Ingress 只能引用同 namespace 的 Service,所以跟着 zone namespace 走。留空不生成,也不删已有的(见参数注释)。
		$ingressYaml = $null
		if ($isGateway) {
			$manifestContent = Add-GoSvcSecretEnv -SvcName $svcName -ManifestContent $manifestContent `
				-EnvName $script:GatewayAdminApiKeyEnv -SecretName $script:GatewayAdminApiKeySecretName -SecretKey $script:GatewayAdminApiKeySecretKey
			$manifestContent = Add-PodTemplateAnnotation -SvcName $svcName -ManifestContent $manifestContent `
				-Key $script:GatewayAdminApiKeyHashAnnotation `
				-Value (Get-InjectedSecretFingerprint -SecretName $script:GatewayAdminApiKeySecretName -Value $script:GatewayAdminApiKey)
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

		# gateNodePortBase(D88):external + NodePort 时 gate-<i> 的 nodePort = base + i。没写就落回命令行 -GateNodePortBase,
		# 并记下"未显式写":多 zone 部署时 preflight 要求每个 zone 显式写,否则各 zone 落在同一段 nodePort 上。
		$zoneGateNodePortBase = $GateNodePortBase
		$zoneGateNodePortBaseExplicit = $false
		if ($null -ne $zone.gateNodePortBase) {
			$zoneGateNodePortBase = [int]$zone.gateNodePortBase
			$zoneGateNodePortBaseExplicit = $true
		}

		$result += [pscustomobject]@{
			name = [string]$zone.name
			zoneId = [int]$zone.zoneId
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

	# 单一 gate-entry(D90):只在 podip 且 gate 副本数恰为 1 时生成。副本数 ≥2 时 Service 随机分流,约一半票据会被
	# token_gate_node_mismatch 拒绝;external 由每序号 Service 取代。不生成时的残留已由上面的 Remove-ClientEntryObsoleteResources 删除。
	if (Test-GateEntryServiceWanted -ClientEntryMode $ClientEntryMode -GateReplicas $CurrentGateReplicas) {
		$gateServiceYaml = New-GateServiceYaml -ServiceName $gateServiceName
		Invoke-KubectlWithInputFile -Args @("apply", "-n", $namespace) -InputContent $gateServiceYaml
	}

	Apply-GoSvcManifests -Namespace $namespace -CurrentZoneId $CurrentZoneId -CurrentClusterId $CurrentClusterId

	Apply-JavaSvcManifests -Namespace $namespace -CurrentZoneName $CurrentZoneName

	$sceneDeploymentNames = @($scenePlan | Where-Object { $_.Replicas -gt 0 } | ForEach-Object { $_.Name })
	Wait-ForZoneReady -Namespace $namespace -SceneDeploymentNames $sceneDeploymentNames -GateReplicas $CurrentGateReplicas

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
		Write-Warning "zones 配置不存在($zonesPath),mysql-init-sql 只预建 -ZoneId=$ZoneId 的 zone_${ZoneId}_db。其余 zone 的库要么补进 zones 配置后重建 mysql PVC,要么由 db 服务在 dev 档自建。"
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
	不按 zone 拆):data_service 启动期 / -migrate 要在里面 CREATE TABLE 四张表,库不在或
	账号无权就只剩 "schema auto-migrate failed",AllocateIdSegment 随之不可用。
	仓库里 00_init_zone_dbs.sql 顺带建的 testdb 只服务本地 compose 的
	go/data_service/etc/data_service.yaml,在 K8s 上是一个没人用的空库,无害。

	新全局服务的独占库(port-decisions D-14 第 5 条,目前是聚宝斋 trade 的 mmorpg_trade 与好友 friend 的 mmorpg_friend)**只登记在
	00_init_zone_dbs.sql**,由上面的循环原样带进本 ConfigMap;这里**不要**再生成第二份建库 sql。
	02_k8s_global_db.sql 是因为本地(testdb)与 K8s(mmorpg_global)库名不同才单独生成的,不是范式。
	表不在 initdb 里建:trade 的表由 manifests/go-svc/trade-migrate.yaml、friend 的表由 manifests/go-svc/friend-migrate.yaml 这两个 Job 建(Apply-GoSvcMigrateJob)。

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
		"-- (transaction_log / player_snapshot / rollback_audit_log / id_segment 四张表由 data_service 按 proto 建)。",
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
	infra-up 在五个 infra manifest 之后、任何 zone 之前 apply 这个 Job;-WaitReady 会等它 Complete。

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
	由写路径入口(Assert-ClientEntryDeployPreflight)调用、结果存 $script:BattleWorkload:health 推导(Resolve-BattleFleetHealth)
	与生成器的参数校验都可能 throw,必须早于任何基础设施写操作,更不能删了旧 battle 才发现新的生成不出来。
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
	return [pscustomobject]@{ Orchestrator = $BattleOrchestrator; Health = $health; Yaml = $yaml }
}

function Apply-BattlePool {
	param([Parameter(Mandatory = $true)][string]$ConfigMapYaml)

	# 工作负载已在写路径入口渲染并校验(New-BattleWorkloadManifest),集群现状(换 kind / 同 kind 换入口)也已在那里判定。
	$workload = $script:BattleWorkload
	if ($null -eq $workload) {
		throw "内部错误:battle 工作负载未在写路径入口渲染(Assert-ClientEntryDeployPreflight 未调用?),拒绝部署 battle。"
	}
	$battleHealth = $workload.Health
	$battleYaml = $workload.Yaml

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
			Write-Host "NOTE: battle Fleet health.initialDelaySeconds 由 -AgonesHealthInitialDelaySeconds=$($battleHealth.RequestedInitialDelaySeconds) 抬到 $($battleHealth.InitialDelaySeconds):battle 启动到首次 /health 的最坏耗时约 $($battleHealth.StartupWorstSeconds)s(推导见 lib/k8s_client_entry.ps1 Resolve-BattleFleetHealth);scene Fleet 仍用原值。"
		}
		Invoke-KubectlWithInputFile -Args @('apply', '-n', $InfraNamespace) -InputContent $battleYaml
		if ($WaitReady) {
			# 下限 300s(ingress_final WP9):GameServer 从调度、端口分配到 SDK Ready,单实例最坏约 135s,另有镜像拉取与排队。
			Wait-ForFleetReady -Namespace $InfraNamespace -FleetName 'battle' -ExpectedReplicas $BattleReplicas `
				-TimeoutSeconds ([Math]::Max($WaitTimeoutSeconds, 300)) -KubeContext $KubeContext -KubeConfig $KubeConfig -DryRun:$DryRun
		}
		return
	}

	Invoke-KubectlWithInputFile -Args @('apply', '-n', $InfraNamespace) -InputContent $battleYaml
	if ($WaitReady) {
		Wait-ForDeploymentReady -Namespace $InfraNamespace -DeploymentName 'battle'
	}
}

function Apply-Infra {
	# 先生成并校验 battle 配置，密钥/契约错误必须发生在任何基础设施写操作之前。
	$battleConfigMapYaml = $null
	if ($BattleReplicas -gt 0) {
		$battleConfigMapYaml = New-NodeConfigMapYaml -CurrentZoneId $ZoneId -CurrentClusterId $ClusterId `
			-ConfigName 'battle-node-config' -IncludeBattleSettings
	}
	Write-Host "Deploying shared infrastructure to namespace $InfraNamespace"
	Write-Host "Kafka profile: $KafkaProfile (broker_retention_ms=$KafkaBrokerRetentionMs db_task_retention_ms=$KafkaDbTaskRetentionMs)"
	Ensure-Namespace -Namespace $InfraNamespace

	# redis-match-cluster.yaml:match 私有的 Redis Cluster(StatefulSet + 建群 Job)。
	# 里面的 Job 幂等(已建群就跳过);但 Job 的 template 一旦落地就不可变,改了
	# 建群脚本要先 `kubectl -n <infra> delete job redis-match-cluster-init` 再 apply。
	# Loki 只在"要用集群内 Loki 当 C++ 日志 sidecar 的写入目标"时才部署:
	# 关了 sidecar(-NoCppLogSidecar)或指定了外部 Loki(-LokiPushUrl)时不占集群资源。
	$infraManifests = @("etcd.yaml", "redis.yaml", "redis-match-cluster.yaml", "kafka.yaml", "mysql.yaml")
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

		if ($manifest -eq "redis.yaml") {
			# 密码走**可选** Secret:dev 档 MMORPG_REDIS_PASSWORD 回落为空串 → 删掉 Secret → redis.yaml 里
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
			Invoke-Kubectl -Args @("delete", "deployment", "redis", "-n", $InfraNamespace, "--ignore-not-found")
		}

		if ($manifest -eq "mysql.yaml") {
			# initdb 脚本的 ConfigMap 必须先于 mysql Deployment 落地,否则 Pod 因
			# volume 引用的 ConfigMap 不存在卡在 ContainerCreating。
			Invoke-KubectlWithInputFile -Args @("apply", "-n", $InfraNamespace) -InputContent (New-MysqlInitConfigMapYaml)
		}

		$manifestContent = Get-Content -Path $path -Raw

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
		}

		# fail-closed:任何 __XXX__ 形态的占位没被替换就拒绝 apply。以前只有 kafka.yaml
		# 走替换,别的 manifest 里新加占位会原样送进集群,要到 Pod 起不来才发现。
		$leftover = [regex]::Matches($manifestContent, '__[A-Z][A-Z0-9_]*__') | ForEach-Object { $_.Value } | Sort-Object -Unique
		if ($leftover.Count -gt 0) {
			throw "infra manifest $manifest 里有未替换的占位:$($leftover -join ', ')。请在 Apply-Infra 里补对应的 Replace。"
		}

		Invoke-KubectlWithInputFile -Args @("apply", "-n", $InfraNamespace) -InputContent $manifestContent
	}

	# data_service 审计 topic 预建 Job:必须在任何 zone(scene)之前落地,见 Apply-KafkaTopicInitJob 注释。
	# 放在五个 infra manifest 之后即可:Job 自己会等 broker 可达,不要求 kafka Deployment 已 Ready。
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
		# 审计 topic 必须在 scene 产出第一条消息之前按契约建好。-WaitReady 下等 Job Complete,
		# all-up 里随后的 zone 就一定晚于它;不带 -WaitReady 时 zone-up 之前要自己核对
		# `kubectl -n <infra> get job kafka-topic-init` 已 Complete(README「Kafka 审计 topic 预建」)。
		Wait-ForJobComplete -Namespace $InfraNamespace -JobName "kafka-topic-init"
	}

	if ($BattleReplicas -gt 0) {
		Apply-BattlePool -ConfigMapYaml $battleConfigMapYaml
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
	# job 一栏同样列出建表 Job trade-migrate / friend-migrate(D-14):失败须核对 status.conditions(Failed/FailureTarget),
	# 看 `kubectl -n <infra> logs job/trade-migrate`(或 `job/friend-migrate`);删除前须确认该 Job UID 所属 Pod 均终态,ACTIVE=0 本身不代表安全。
	Invoke-Kubectl -Args @("get", "deploy,sts,po,pvc,pdb,job,svc,cm", "-n", $InfraNamespace) -AllowFailure
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
Show-ExposureProfileWarning

Write-Host "Release: profile=$ReleaseProfile image=$NodeImage pullPolicy=$ImagePullPolicy go_tag=$GoSvcTag java_tag=$JavaSvcTag cluster_id=$ClusterId"
if ($script:ReleaseStamp.Ok -and $script:ReleaseStamp.Dirty) {
	Write-Warning "工作树是脏的(git status 非空),镜像 tag 带 -dirty 后缀。生产发布(-ReleaseProfile prod)会拒绝这种 tag。"
}

# 写操作才需要门禁;*-down / *-status 是止血和排查路径,不能被预检或密钥缺失挡住。
if ($Command -in @("zone-up", "all-up", "infra-up")) {
	Assert-ImmutableReleaseImages
	Invoke-ReleasePreflight
	# 集群外入口的组合校验(含 -LoginDevPasswordAuth 只许 dev 档)排在密钥解析之前:非 dev 档传该开关时,
	# 报的是门禁原因,而不是"缺环境变量"。battle 工作负载渲染与集群现状预检(只读 kubectl)也在这里,早于任何写操作。
	Assert-ClientEntryDeployPreflight
	Initialize-InjectedSecrets
	# C++ gRPC deadline ≥ 目标 Go 服务 zrpc Timeout + 2000(上游比下游宽)。放在任何集群写操作之前:
	# 不成立就整条拒绝,不留半截部署(见 Assert-GrpcClientDeadlineBudget)。
	Assert-GrpcClientDeadlineBudget
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
