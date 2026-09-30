# gate / battle 集群外客户端入口(D76–D93)

- **日期**:2026-09-29。
- **状态**:代码、脚本、测试已全部落盘;**未编译、未测试、未上集群,待 Codex 验证**(AGENTS.md §10.1)。本文任何「通过」「生效」都指设计意图,不是运行证据。
- **范围**:让集群外客户端(Unity、宿主机上的 robot)经 K8s 连上三类入口:gate 长连接、battle 每局直连、Java gateway 的 HTTP 接口。
- **决策编号**:本文 D76–D93。D51–D64 属 [client-access-band-routing.md](client-access-band-routing.md);D65–D75(battle 直连收缩)属 [turn-based-battle-server.md](turn-based-battle-server.md) §22。
- **事实准绳**:以磁盘代码为准。文中「路径:行号」取自 2026-09-29 写作时的磁盘状态,后续改动会让行号漂移,查找时以符号名为准。
- **冲突处理**:[k8s_gate_exposure_guidance*.md](k8s_gate_exposure_guidance.md)、[gate-load-balancing-design.md](gate-load-balancing-design.md)、[gateway-k8s-deployment.md](gateway-k8s-deployment.md) 与本文冲突的,以本文为准。turn-based §21.1 关于「K8s 上直连失败回落 gate 中继」的推断已被本文取代(见背景第 4 条)。
- **引用本文的文件**(2026-09-29 更正:初稿只列了 kind-config.yaml,漏了其余按节名引用的位置)。改下列节名时,必须同改对应位置,否则会留下悬空引用:
  - 「kind 端到端验证」:`deploy/k8s/kind-config.yaml` 第 2、9、60 行;`deploy/k8s/README.md:259`、`:749`。另有不带书名号的提法:`docs/design/turn-based-battle-server.md:1061`、`docs/design/moba-battle-target-architecture.md:229`。
  - 「运维手册」:`deploy/k8s/README.md:259`、`:380`;`docs/design/k8s_gate_exposure_guidance.md:3`、`k8s_gate_exposure_guidance_zh.md:3`、`k8s_gate_exposure_guidance_en.md:4`;`tools/scripts/k8s_gate_drain.ps1:12`。
  - 「模式矩阵」:`docs/design/battle-transport-decision.md:111`。
  - 下列位置只链接全文或引用 D 号,改节名不受影响,改 D 号要同改:`docs/design/ARCH.md:430`、`:504`,以及 turn-based-battle-server.md、moba-battle-target-architecture.md、battle-transport-decision.md、cross-zone-matchmaking.md、gate-connection-admission-control.md、xuanming-port-decisions-20260910.md 等设计文档;代码注释方面有 `tools/scripts/lib/k8s_client_entry.ps1`、`tools/scripts/k8s_deploy.ps1`、`tools/scripts/k8s_image.ps1`、`tools/scripts/dev_tools.ps1`、`tools/scripts/k8s_gate_drain.ps1`、`cpp/nodes/battle/**`、`cpp/libs/engine/infra/agones/**`、`go/match/**`、`go/scene_manager/**`、`go/shared/**`。行号取自 2026-09-29 的 grep。

## 背景与问题

收缩批(turn-based §22 D65–D75)之后,客户端平时保持 1 条 gate 长连接;参战或观战时,每局再加 1 条 battle 直连,直连是战斗的唯一通路。此外还有 gateway 上的 HTTP 短请求:server-list、assign-gate、queue-status,可选的 /api/login,以及 refresh-token。改动前,这三类入口在 K8s 上对集群外客户端全部不通。原因如下(2026-09-29 测绘,行号是改动前的):

1. **三个下发地址的出口都给 POD_IP,中间没有任何翻译。**
   - C++ 节点的 `endpoint` 由 `ResolveNodeIp` 产生,优先级 POD_IP > NODE_IP > localip()。K8s 上所有 C++ Pod 都注入 POD_IP。`grpc_endpoint.ip` 复制它,gRPC server 也绑在它上面,所以不能把它直接改成公网地址或 NAT 后的地址,否则 gRPC 绑定失败、LOG_FATAL。
   - gate 地址有两个出口。一个是 login 的 `CandidatesForZone`,快路径、排队 dispatcher、重入都经过它;Java 的 `/api/assign-gate` 只是透传。另一个是 scene_manager 的 `AssignGateForZone`,即 RedirectToGate。
   - battle 地址的出口是 `BattleRoomManager::BuildAssignment`,它把 `BattleAssignedS2C.host/port` 填成自身的 `endpoint`,即 POD_IP:20000。补签 IssueBattleTicket 走同一条路。
   - 改动前 NodeInfo 只有 `endpoint(5)` 和 `grpc_endpoint(10)` 两个地址字段,没有对外地址字段。`node.cpp` 注释、`deploy/k8s/README.md`、`k8s_deploy.ps1` 都写着「对外地址由 login 按 node_id 翻译」,但这件事从未实现。node_id 由 etcd CAS 按最小空闲号分配,重启后会变,本来也当不了翻译键。
2. **gate-entry 随机落点会被拒。** gate 是 2 副本 Deployment,前面挂一个选中全部 gate 的 `gate-entry` Service。gate 票据只绑 `gate_node_id`,gate 验票时会核对它(`token_gate_node_mismatch`)。经 Service 随机落点时,2 副本约有一半票据落到别的 gate 上被拒。仓里原有的「NodePort + 外部 L4」指引,与 gate-load-balancing-design.md 的「L4 透传不适用」互相矛盾。
3. **battle 和 gateway 没有对外入口。** battle 在 infra namespace 是一个没有 Service 的 Deployment;Java gateway 只有 ClusterIP Service,没有 Ingress。
4. **(2026-09-29 更正)** turn-based §21.1 曾推断「K8s 上直连失败时回落 gate 中继,只剩 1 条连接」,这不成立。K8s 上 gate 同样通告 POD_IP:集群外客户端连不上 gate,也连不上 battle;集群内的 robot 两者都能连上。收缩后 gate 也不再中继战斗(D66)。该推断已被本文 D76–D93 取代。

## 最终决策 D76–D93

最终口径 = `ingress_final` 原稿,再依次叠加三份编排补充(优先级从高到低:2c > 2b > 第一份补充),最后以实现与评审中核实的代码为准。与原稿不同的地方,在各条里标为「与原稿差异」,并汇总在下一节。

### D76 地址模型:进程自报客户端可达地址(advertised address)

- NodeInfo 新增 `EndpointComp client_endpoint = 11`(`proto/common/base/common.proto:33`)。它由客户端面节点(gate、battle)在发布到 etcd 之前自报。
- `endpoint` 永远是集群内身份:gRPC 绑定、端口 CAS 键、节点间对端校验、FindNodeByPodIP 都用它。`client_endpoint` 只用于下发给客户端。
- 「可用」的定义:ip 非空,且 port 在 1..65535;只填了一半视为缺失。ip 可以是 IPv4 字面量或 DNS 名,Unity 的 `TcpClient.Connect(host, port)` 能解析 DNS。
- 客户端可见协议不变。`BattleAssignedS2C.host/port` 以及 login、scene_manager 下发的 gate ip/port 本来就是字符串字段,只把语义改为「客户端可达地址」;`proto/battle/player_battle.proto` 只改注释。
- 字段号 11 上线后不复用,废弃时写 `reserved 11;`(AGENTS §4.3)。
- 理由:与 Kafka `advertised.listeners`、Redis `cluster-announce-ip` 同型;三个出口只认一份真相;node_id 重启会变,当不了翻译键。
- 否决方案④(由下发方查表翻译):gate 地址要在 login 与 scene_manager 用 Go 各写一遍,battle 地址还要在 C++ 里再写一遍,违反 AGENTS §11.2 的单一真相。

### D77 滚动兼容:所有解析方忽略未知字段,且永久保留

- Go 的 9 处严格 protojson 解析,统一改经 `go/shared/nodeinfo.Unmarshal`(`unmarshal.go:33`,`DiscardUnknown: true`)。它们是:login 的 watcher;scene_manager 的 `decodeGateNodes` 与 noderegistry;player_locator 的 session_reconciler 与 node;guild 的 node;data_service、match、client_rpc_router 的 noderegistry。
- 用 `encoding/json` 的几个 watcher 天然容忍未知字段,不改;Java 的 `NodeInfoRecord` 已标 `@JsonIgnoreProperties(ignoreUnknown=true)`,也不改。
- C++ 的 3 处 `JsonStringToMessage` 设 `ignore_unknown_fields = true`:`node.cpp:1338`(HandleServiceNodeStop)、`etcd_service.cpp:323`(HandlePutEvent)、`service_discovery_manager.cpp:49`(AddServiceNode)。第 4 处在 `agones_gameserver_status.cpp`,解析的是 `google::protobuf::Struct`,与本条无关。
- 理由:旧 player_locator 读到新字段,会把整台 gate 判为缺席,两轮之后把大面积会话标成 DISCONNECTING;旧 login 读到新字段会报 no gate available。
- 字段为空时 protojson 不输出它,所以只加字段本身不影响旧解析方。但**生产方真正填这个字段之前,所有消费方必须先滚到宽松解析**,见「上线与回退顺序」第 0 批。
- 附带语义:DiscardUnknown 同时会忽略未知的枚举名。NodeInfo 目前没有枚举字段。

### D78 三个出口统一选择规则,各语言一份权威实现,并按地址去重

- 规则:`client_endpoint` 可用就用它。否则 require=false 时回落 `endpoint`,但回落前要求 `endpoint` 本身可用,不可用就跳过(fail-closed,绝不下发空地址或 0 端口);require=true 时跳过该节点或拒签。
- 权威实现:Go 用 `shared/clientendpoint.Select`(`select.go:41`)与 `DedupeNewest`(`dedupe.go:19`);C++ 用 `client_endpoint::ClientFacing`(`client_endpoint.h:109`)。两边靠注释和各自的单测对齐,**没有跨语言共享用例**,改一边必须同改另一边。
- 三个出口:
  - login:`candidatesFromNodes` → `buildGateCandidates`(`go/login/internal/svc/servicecontext.go:310/342`);
  - scene_manager:`signRedirectToGate` → `selectGateTargets`(`go/scene_manager/internal/logic/gate_redirect.go:62/125`);
  - battle:`BattleRoomManager::BuildAssignment`(`battle_room_manager.cpp:1432`),补签 IssueBattleTicket 同路。
- 去重:login 与 scene_manager 按有效客户端地址 `net.JoinHostPort(host, port)` 去重,同一地址只留 launch_time 最大的一条。login 的顺序是先按 zone 过滤、再去重、最后做排空过滤;如果先做排空过滤,同地址上没被标记的陈旧影子会顶上来。
- 去重的理由:地址稳定时,gate 崩溃后旧记录会残留到 NodeTTL(180s)。客户端能连通 TCP,却会被新 gate 以 `token_gate_node_mismatch` 拒绝。
- 线程约束(battle):`ClientFacing(gNode->GetNodeInfo(), ...)` 只能在 loop 线程上调用。`GetNodeInfo()` 读的是 thread_local,在 gRPC 线程上会得到空 NodeInfo,结果是整批静默拒签。`ClientEndpointRequired()` 可以跨线程读。
- 指标:`mmorpg_client_endpoint_select_total{result="client|fallback|rejected|deduped"}`(`go/shared/clientendpoint/metrics.go`),不带 player_id。读法见运维手册「客户端地址排障」。

### D79 生产方 fail-closed,并在发布前同步解析

- `Node::InitClientEndpoint`(`node.cpp:562`)由 `InitRpcServer` 在设完 endpoint 之后、etcd 分配与发布之前调用(`node.cpp:526`)。它解析 `CLIENT_ENDPOINT_*` 并写入 `client_endpoint`。
- 失败的处理:source 为 static 或 agones 时,解析或取址失败一律走 `FatalClientEndpoint`(`node.cpp:355`)。它先同步写一行 stderr `FATAL client endpoint: <原因>`,再 LOG_FATAL 以非 0 退出,由 K8s 或 Agones 重建。先写 stderr 是因为 muduo 的 LOG_FATAL 会直接 abort,异步日志来不及落盘。
- 注入时机:`Node::Initialize()` 在 Node 构造函数里执行,早于 main 的 configure lambda,所以 Agones 地址来源不能在 configure 里注入。它经 `node_entry.h` 的 THooks 预构造钩子 `ClientEndpointSourceFactory` 注册。battle 的 `BattleNodeHooks::ClientEndpointSourceFactory::Make` 返回 `agones::MakeClientEndpointSourceFromEnv()`;Agones 未启用,或没有 curl(Windows 构建)时返回 nullptr,进而致命退出。
- Agones 来源有界:最多 30 次,退避 200ms→2s,**单调时钟总预算 60s**(`agones_client_endpoint_source.h:43-47`),单次 GET 的超时也截到剩余预算。**与原稿差异**:原稿只写「最坏约 60s」;首版实现只按次数 × sleep 计,最坏约 113s,评审后改为硬截止。
- `CLIENT_ENDPOINT_REQUIRED` 只用于消费方的纵深防御。battle 侧:`BuildAssignment` 拒签,external 下 CreateBattle / AddObserver / IssueBattleTicket 回 `kServiceUnavailable`。login / scene_manager 侧由 ConfigMap 的 `RequireClientEndpoint` 控制,见运维手册。
- 理由:match 的 battle watcher 看不到这个字段。如果只在出口处拒绝,那时房间已建好、玩家已凑齐,太晚了。
- source 为 none(含未设置)时,HOST / PORT 被忽略但会留痕:stderr 与 LOG_WARN 各一行 `WARN client endpoint: CLIENT_ENDPOINT_HOST/PORT set but CLIENT_ENDPOINT_SOURCE=none; ignored. ...`(`node.cpp:580-593`)。

### D80 两个正交开关,一个部署只能二选一

- `-ClientEntryMode podip|external`,默认 podip;`-BattleOrchestrator deployment|agones`,默认 deployment。组合见「模式矩阵」。podip 专供集群内的 robot 和压测。
- 不做「同时下发内外两个地址」:那会改客户端可见协议。
- **补充(原稿无)**:两个开关**都不粘滞**,每次部署按本次取值生成。会踢人的切换由 `-AllowDisruptiveSwitch` 把关,见运维手册「模式不粘滞与切换闸」。
- **与原稿差异**:login / scene_manager 的 `RequireClientEndpoint` 原稿与 ClientEntryMode 硬绑(external→true)。现在是独立参数 `-RequireClientEndpoint auto|true|false`,auto 等同原稿规则,true / false 供上线窗口显式覆盖。

### D81 battle 用 Agones Fleet,portPolicy Dynamic

- `spec.ports` 只列一项:`name: client`、`portPolicy: Dynamic`、`containerPort: 20000`。容器 ports 只写 gRPC 50000;20000 由 Agones 注入,重复声明会触发端口重复校验。
- gRPC 50000 永不进 hostPort、nodePort 或 Agones ports,也禁止 hostNetwork。
- 否决 Passthrough:它会让 endpoint.port 与 hostPort 混用,还与 `kGrpcPortOffset` 以及 20000..35535 的端口校验冲突。本批也不做 Static。
- 地址来源:sidecar `GET /gameserver` 返回的 `status.address` 与 `status.ports[client]`。`CLIENT_ENDPOINT_HOST` 可以覆盖 host(kind 上填 127.0.0.1)。agones 来源下**禁止设** `CLIENT_ENDPOINT_PORT`,设了即致命,以免歧义。
- Fleet 其余形态(生成器 `New-BattleFleetYaml`,`tools/scripts/lib/k8s_client_entry.ps1:988`):`scheduling: Packed`;`eviction.safe: Never`;`allocationOverflow.labels`(见 D83);Pod `serviceAccountName: agones-sdk`;env `AGONES_ENABLED=1`。
- health:**与原稿差异**,不用原稿的 10/5/3,见「battle Fleet health 预算与就绪判据」。

### D82 battle 分配许可与建房准入

- CreateBattle 的节点级准入共三道,全部在任何副作用之前(`cpp/nodes/battle/handler/grpc/battle_node.cpp:56-124`;2026-09-29 更正,初稿写作 56-110,第三道的拒绝在 `:120`):
  1. 建房准入闸(`battle_admission_gate.h`),在 gRPC 线程上判定。SetAfterStart 完成后才打开,停机开始即关闭。
  2. 分配许可:gRPC 线程上、`runInLoop` 之前调 `AcquireAllocationPermitBlocking()`,等 Agones allocate 的上限是 allocateWaitTimeout(3s)。
  3. 投递进 loop 后,复核准入闸。
- 任一道不过,即返回 `grpc::Status(UNAVAILABLE, "battle_not_allocatable")`。三道都通过后,才是 loop 内预签票据(D70),再插表。
- match 侧(`go/match/internal/logic/gather.go:65`,`isBattleNotAllocatable` 在 `:581`)只认 `codes.Unavailable` 加上消息**精确等于** `battle_not_allocatable` 的组合。命中时不发 DestroyBattle,用 `PickRandomExcept`(`node_watcher.go:317`)按 endpoint 排除已试节点,换一个节点重试**一次**;仍被拒就干净失败,记 `match_gather_total{outcome="not_allocatable"}`。其他 UNAVAILABLE 照旧走 DestroyBattle 兜底。
- 偏离「CreateBattle 错误走 tip、status 恒 OK」惯例的理由:这是节点级的准入拒绝,发生在业务处理之前;UNAVAILABLE 是 gRPC 标准的「换一台试」语义;客户端看不到这个错误,不值得为它往 Tip.xlsx 发号。
- AddObserver / IssueBattleTicket 只作用于已有房间,不取许可。
- **补充(原稿无)**:
  - 准入闸覆盖两个窗口。启动窗口:etcd 已发布、但 SetAfterStart 还没完成时,lifecycle 默认是 Disabled,会不经 allocate 放行许可。停机窗口:关闸与 AbortAllRooms 在同一个 loop 任务里。
  - 观战记录 `spectate:battle:{id}` 改在 CreateBattle **之前**写(fail-closed:写失败无副作用,记 `index_failed`);D82 换节点之前改写一次;建成后 best-effort 再写一次。
- 时长口径随之变更:matched ticket TTL = ⌈N×6 + 2×(W+C) + R⌉ + 10(`go/match/internal/logic/queue.go:342/367`)。其中 W = 6.1s(记录写入最坏值)、C = 5s(createBattleTimeout)、R = 3s。1/2/5/10 人依次为 42/48/66/96s;5 人开战锁 101s;team 的 endMatchMaxDuration 为 110s;battle 确认补发窗口 180s(≥ 96 + 60 = 156s)。
- Agones 晚到分配:如果所有等待方都已超时离开,allocate 才成功,那么零单元时自动回 Ready,并打 WARN `[Agones] allocate confirmed after every waiter gave up`(`agones_gameserver_lifecycle.cpp:525`)。

### D83 battle 排空

- Fleet 设 `allocationOverflow.labels: {mmorpg.io/drain: "true"}`。滚动更新或缩容时,旧的 Allocated 实例会被打上这个标签。运维维护节点时手动打同一个标签。
- Agones SDK 的 SetLabel 会自动加 `agones.dev/sdk-` 前缀,打不出这个键,所以标签只能由 Fleet 或运维打上,进程只读。
- lifecycle worker 每 drainPollInterval(5s)`GET /gameserver` 读自己的 labels。标签**键**存在就算排空,不看值:此后拒绝新许可,房间归零后回 Ready,由 Fleet 回收。删除标签(`kubectl label gs <name> mmorpg.io/drain-`)即恢复接房间。
- **修正后的语义(原稿未写)**:
  - Ready 成功后先同步读一次标签,再处理挂起的 allocate。
  - 只解析元数据段(`ParseGameServerLabels`)。GET 失败、响应里没有 `object_meta` / `objectMeta`、labels 形状不符,都算读取失败,保持上一次的判定;status 段的形状问题不影响排空判定。
  - 首次读取失败时按「未排空」处理。这是可用性优先的 fail-open,每 5s 打一条 WARN,最迟一个轮询周期后纠正。
- `eviction.safe: Never`。

### D84 battle 的 Health 绑 EventLoop 心跳(opt-in)

- EventLoop 每 1s 调一次 `TouchLoopHeartbeat()`。心跳超过 loopStaleAfter(10s)没更新,worker 就停发 /health,Agones 判 Unhealthy 后替换实例。Allocated 实例被替换意味着房间作废,这是设计如此。
- Fleet 的 health `periodSeconds` 必须大于 C++ 的 healthInterval(2s),否则正常心跳也会被判失败。生成器不满足时直接 throw。
- scene 本批不开心跳,也不开排空。

### D85 Agones 代码下沉并泛化

- 从 `cpp/nodes/scene/agones/` 迁到 `cpp/libs/engine/infra/agones/`:`agones_rest_client`;`agones_gameserver_lifecycle`(原 `agones_scene_lifecycle`);新增 `agones_gameserver_status`(`ParseGameServer` / `ParseGameServerLabels`,用 `google::protobuf::Struct` 解析,不写并行结构体)与 `agones_client_endpoint_source`。
- 改名:`SceneLifecycle` → `GameServerLifecycle`,`CreatePermit` → `AllocationPermit`,`OnScene*` → `OnUnit*`,`SceneCount` → `UnitCount`。scene 只做机械改名,不复制第二份状态机。
- 端口名常量 `agones::kClientPortName = "client"`(`agones_client_endpoint_source.h:34`)。
- 搬迁没有用 `git mv`:新建目标文件、删除原文件,构建清单同步更新。

### D86 battle 的 Deployment + hostPort 形态只用于验证与回退

- 只有客户端口 20000 带 `hostPort: 20000`;策略 `RollingUpdate maxSurge 0 / maxUnavailable 1`。每个节点只能跑 1 个,没有忙碌保护,滚动时在打的局作废。
- env:`CLIENT_ENDPOINT_SOURCE=static`,`CLIENT_ENDPOINT_PORT=20000`,host 取 `-ClientPublicHost`,留空时取 `$(HOST_IP)`,由 kubelet 展开。
- **(2026-09-29 补充)多节点必须留空 `-ClientPublicHost`**:给了它,所有副本都自报同一个 `<host>:20000`(`lib/k8s_client_entry.ps1:1122`)。副本又是一节点一个,客户端会被发往 host 指向的那个节点,连不上或连错房间。`-OpsProfile managed-cloud` / `bare-metal` 会把 battle 副本数抬到 ≥2(`k8s_deploy.ps1:782`、`:793`),hostPort 形态因此至少要 2 个节点,也必然落进这个问题。
- external + deployment 组合时,preflight 打警告。

### D87 gate 用 StatefulSet + 每序号 Service,不进 Agones

- 形态:`podManagementPolicy: Parallel`,`updateStrategy: OnDelete`;PDB 名 `gate`,`maxUnavailable: 0`;headless Service `gate-headless`;每序号 Service `gate-<i>`。
- 滚动一律走 `tools/scripts/k8s_gate_drain.ps1`:标 `gate:{id}:draining` → 等 `gate:{id}:drained` → 删 Pod。脚本不做缩容。
- 理由:gate 只要有长连接就恒为 Allocated,Fleet 滚动永远收敛不了;Health 失败一次就会删掉整台 gate;仓里的 Agones 链路从没上过集群;StatefulSet 保留了现有的 readinessProbe。
- 否决方案:评审稿 standard-ops 的方案①让 gate 也进 Agones。它被 correctness 评审判为致命问题:gate 的全员入口会依赖一条从未上过集群的链路。ingress_final 原稿即采用 StatefulSet,本文沿用。
- **与原稿差异**:原稿写「保留现有 `Wait-ForStatefulSetReady`」,这不成立。`kubectl rollout status` 对 OnDelete 的 StatefulSet 直接报错,就绪等待改用新增的 `Wait-ForGateStatefulSetReady`(`lib/k8s_client_entry.ps1:1790`),轮询 `.status.readyReplicas`。

### D88 gate 客户端地址的计算

- 在 gate 启动 shell 的前缀里(`Get-GateClientEndpointShellPrefix`,`lib/k8s_client_entry.ps1:477`),由 `POD_NAME` 的序号算出 `CLIENT_ENDPOINT_HOST/PORT` 并 export。序号或 nodePort 基址不是数字就 exit 64,并在 stderr 回显 `gate: client endpoint <host>:<port> (ordinal <i>)`。
- 主机优先级:`-GateClientHostTemplate` > `-ClientPublicHost` > `HOST_IP`(`status.hostIP`)。模板里的 `{zone}` 在生成期渲染,`{ordinal}` 在 shell 里渲染。
- **`-ClientPublicHost` 的适用范围(2026-09-29 补充)**:它是 gate 与 battle 共用的一个值,会让所有实例自报同一个主机。
  - gate:每个序号端口不同,但在 NodePort + ETP Local 下,只有 Pod 所在的节点收这条流量。多节点时,host 指向的那个节点上没有该序号的 Pod,连接就会被丢弃。改成 ETP Cluster 虽然能转发过去,代价是 SNAT(D89)。
  - battle:Fleet 带了它,就给所有 GameServer 写同一个 `CLIENT_ENDPOINT_HOST`,覆盖各自的 Agones `status.address`(`New-BattleFleetYaml`,`lib/k8s_client_entry.ps1:1013-1015`)。Dynamic hostPort 只在 GameServer 所在的节点上生效,别的节点上同号端口可能属于另一个实例。hostPort 形态见 D86。
  - 所以它只用于单节点集群(如 kind),或该地址确实能到达每个 gate / battle 实例的场景。多节点生产留空:battle 取 Agones `status.address`(hostPort 形态取 `HOST_IP`),gate 取 `HOST_IP` 或 `-GateClientHostTemplate`。
  - preflight 不知道集群有几个节点,对此**不告警**,列入剩余风险。
- 端口:NodePort 形态为 `gateNodePortBase + ordinal`;LoadBalancer 形态为 18000。LoadBalancer 必须配 DNS 模板,并加注解 `external-dns.alpha.kubernetes.io/hostname`。
- 每个 zone 显式配 `gateNodePortBase`:zones 配置里是 zone 级的键;单 zone 的 zone-up 用 `-GateNodePortBase`,默认 30000。不从 zoneId 推导。推荐放在 K8s 静态子段 30000–30085。
- preflight:越界报错(base < 30000,或 base + 副本数 − 1 > 32767);超出 30085 警告;多 zone 且 external + NodePort 时未显式写 base 报错;各 zone 的段重叠报错。分多次 zone-up 部署时,跨 zone 的重叠在删除旧形态之前由服务端预演(`kubectl apply --dry-run=server`)拦下。
- 样例:`zones.sample.json`、`zones.sample.yaml`、`zones.ops-recommended.yaml` 为 30000 / 30010;`zones.10zones.yaml` 为 30000–30072,步长 8。

### D89 保留客户端真实源 IP

- 每序号 Service 的 `externalTrafficPolicy` 默认 Local。Agones 的 hostPort 走 DNAT,本身就保留源 IP。
- 选 Cluster 时 preflight 警告:会做 SNAT,G9 按源 IP 限流不可用。
- NodePort + Local 在多节点下要求 `HOST_IP` 对客户端可达;做不到就改用 LoadBalancer + DNS 模板。
- kind 验收里有一项「peer IP 不是节点 IP」。

### D90 单一 gate-entry Service

- 只在 podip 模式、且 gate 副本数恰为 1 时生成(`Test-GateEntryServiceWanted`)。external 模式不生成;其余情形删除残留。
- 理由:副本数 ≥2 时,约一半票据会被 `token_gate_node_mismatch` 拒绝。
- 行为变化:默认 podip、gate 2 副本的 zone-up 从此不再有 gate-entry。「经 gate-entry 从集群外进 gate」的旧口径作废。

### D91 Java gateway 的 HTTP 入口

- 生成 Ingress:`networking.k8s.io/v1`,名 `gateway`,后端 `gateway:8081`;ingressClassName 可配,默认 nginx;`-GatewayIngressTlsSecret` 非空时生成 tls 段。
- **与原稿差异**:
  1. **由 zone-up 生成**。Ingress 与 gateway Deployment 同在 zone namespace,只在本次确实部署 Java 服务时生成(给了 `-JavaSvcRegistry` 且不带 `-SkipJavaSvc`)。在 infra-up 上给 `-GatewayIngressHost`,只会得到「被忽略」的警告;但如果同时没给 `-GatewayTrustedProxies`,preflight 仍会报错拒绝(`lib/k8s_client_entry.ps1:1460`,该检查不区分本次是否部署 gateway)。(2026-09-29 更正:初稿漏了后半句。)
  2. **只路由 `/api`(pathType Prefix)**。`/admin/**` 管理面和 `/actuator/**` 探针永不出集群。
  3. `-GatewayIngressHost` 留空时不删已有的 Ingress;要撤掉请手动删。
  4. admin 口令经 Secret 注入,见运维手册「Java 管理面」。
- 启用 Ingress 时 `-GatewayTrustedProxies` 必填,preflight 拒绝缺失。另有集群现状预检:zone 里已有 Ingress、本次部署 gateway 却没给 trusted proxies 时拒绝,且不受 `-AllowDisruptiveSwitch` 豁免。理由:否则 gateway 会把 Ingress controller 当作所有玩家的来源 IP,全体玩家共用一个限流桶,assign-gate / login 大面积 429。
- trusted proxies 写进 gateway ConfigMap 的 Spring 属性 `gate.rate-limit.trusted-proxies`。
- Java 代码不改:`LoginNodeDiscovery` 只用 endpoint 找 login,assign-gate 是透传。

### D92 env 命名

- 新变量只用 `CLIENT_ENDPOINT_*`、`HOST_IP`、`POD_NAME`。
- 严禁复用 `NODE_IP` / `NODE_PORT`:它们是 `ResolveNodeIp` / `TryResolveNodePortFromEnv` 的集群内身份输入。
- 契约测试按整词检查,因为 `GATE_NODE_PORT_BASE` 是合法的生成器内部 env,子串匹配会误伤。

### D93 本地验证

- 新建单节点 kind 集群,用独立 kubeconfig,不改用户默认的 current-context,不碰 pandora 的 minikube。
- 分两个阶段:A 不装 Agones(gate StatefulSet + battle hostPort);B 装 Agones。
- gate 和 battle 严禁用 port-forward 验证。gateway 是 HTTP,可以 port-forward。
- **与原稿差异**:Agones 端口段从 7000–7009 改为 **7100–7109**;kind 固定 v0.33.0,node 镜像钉 v1.35.8 digest;所有命令 fail-closed 指定 context `kind-mmorpg`;robot 用专用 yaml,经 gateway 的 port-forward 18081 进入;Ingress 在第 5 步 zone-up 生成。完整步骤见「kind 端到端验证」。

## 与 ingress_final 原稿的差异汇总

原稿 = 编排方的 `ingress_final`(D76–D93 初版落码契约)。下表只列与原稿不同、或原稿没写的项;其余按原稿执行。

| 项 | 原稿 | 最终口径 | 依据 |
|---|---|---|---|
| gate 形态 | StatefulSet + 每序号 Service(评审稿 standard-ops 方案①曾主张 gate 进 Agones) | 同原稿,不进 Agones | D87 |
| gate 就绪等待 | 保留 `Wait-ForStatefulSetReady` | `Wait-ForGateStatefulSetReady`,轮询 readyReplicas(OnDelete 不支持 rollout status) | D87 |
| Agones 端口段 | 7000–7009 | **7100–7109**(本机 7000–7005 被常驻 redis-cluster 占用) | D93 |
| battle Fleet health | 10 / 5 / 3 | 复用 `-AgonesHealth*`(30 / 10 / 3),initialDelaySeconds 抬到 max(传入值, 135 − P×F),默认 105 | 预算推导一节 |
| Fleet 就绪判据 | `.status.readyReplicas` ≥ 副本数 | `Measure-FleetRolloutReadiness`:只认当前模板的 GameServerSet,ready + allocated 计入 | 就绪判据一节 |
| Agones 取址最坏耗时 | 约 60s | 单调时钟 60s 硬预算 | D79 |
| CreateBattle 准入 | 只有分配许可 | 准入闸 + 许可 + loop 内复核,三道都回 `battle_not_allocatable` | D82 |
| 观战记录写入时机 | 原稿未涉及;改动前代码为建房后写、失败只打日志(2b §9)(2026-09-29 更正:初稿误记为原稿内容) | 建房前写(fail-closed),换节点前改写,建成后再写一次 | D82 |
| matched ticket TTL | 原稿未涉及;改动前 2/5/10 人 30/48/78s;5 人锁 83s;battle 确认补发窗口 150s | 1/2/5/10 人 42/48/66/96s;5 人锁 101s;battle 确认补发窗口 180s | D82 |
| battle 排空语义 | 每 5s 读标签 | 另加:Ready 后先同步读一次;只解析元数据段;读取失败保持原判定 | D83 |
| Ingress 生成处 | infra-up(原 §5.2 第 4 步) | **zone-up**,与 gateway 同处 zone namespace | D91 |
| Ingress 路由 | 未限定(实现首版为 `/`) | **只路由 `/api`**,`/admin`、`/actuator` 只在集群内 | D91 |
| gateway admin 口令 | 未涉及 | Secret 注入 `ADMIN_APIKEY`,非 dev 档缺失 / 占位 / 短于 32 位即拒绝部署 | 运维手册 |
| RequireClientEndpoint | 与 ClientEntryMode 硬绑 | 独立参数 `-RequireClientEndpoint`,取值 auto / true / false | D80 |
| 模式切换 | 删除另一种工作负载并打警告 | `-AllowDisruptiveSwitch` 闸 + 写操作之前的集群现状预检 | 运维手册 |
| login 认证(kind) | 未涉及(集群内 login 实际没有任何认证配置) | `-LoginDevPasswordAuth`,只允许 dev 档,口令经 Secret 注入 | 运维手册 |
| GateRouterMode 默认 1 的时机 | 第 4 批才翻 | 已由 turn-based §22 D75 默认为 1;集群外部署在同一窗口启用 external | 上线顺序 |
| kind 负向验证(原 6f) | `kubectl set env` 清空 gate 的 HOST | 三选一写法,原写法在 gate StatefulSet 上复现不出来 | kind 一节 |
| kind robot 口径 | robot 经 port-forward svc/login | 专用 robot yaml + gateway port-forward 18081 | kind 一节 |
| kind 版本 | 不钉 node 镜像 | kind v0.33.0 + node v1.35.8(digest 钉死) | kind 一节 |
| proto 重生命令 | `cd go && build.bat` | protoc 35.1 置于 PATH 最前 → `proto-gen-build` → `proto-gen-run -UseBinary`(见上线顺序) | 上线顺序 |

## 模式矩阵

`-ClientEntryMode` × `-BattleOrchestrator`。SOURCE / REQUIRED 指容器 env `CLIENT_ENDPOINT_SOURCE` / `CLIENT_ENDPOINT_REQUIRED`;「-」表示不设(等价 none / 0)。

| ClientEntryMode | BattleOrchestrator | gate 工作负载 | battle 工作负载 | gate SOURCE / REQUIRED | battle SOURCE / REQUIRED | RequireClientEndpoint(auto) | 用途 |
|---|---|---|---|---|---|---|---|
| podip(默认) | deployment(默认) | Deployment;gate-entry 仅在 1 副本时生成 | Deployment,无 Service | - / - | - / - | false | 集群内 robot、压测 |
| podip | agones | 同上 | Fleet(Dynamic 端口照常注入,但不自报) | - / - | none / - | false | 集群内验证 Fleet 生命周期 |
| external | deployment | StatefulSet + `gate-<i>` + `gate-headless` + PDB | Deployment + hostPort 20000 | static / 1 | static / 1 | true | 验证、回退(D86) |
| external | agones | 同上 | Fleet,portPolicy Dynamic | static / 1 | agones / 1 | true | **生产推荐** |

- podip 下不输出 `CLIENT_ENDPOINT_HOST/PORT`:SOURCE=none 时带上它们会触发常态 WARN(D79)。
- gate 的地址来源只有 static:NodePort 的端口是 base + 序号,LoadBalancer 的端口是 18000(D88)。
- 生成器内部 env(只给 gate 启动 shell 用,C++ 不读):`GATE_CLIENT_HOST_TEMPLATE`、`CLIENT_PUBLIC_HOST`、`GATE_CLIENT_PORT_MODE`(`nodeport` | `service`)、`GATE_NODE_PORT_BASE`、`GATE_CLIENT_PORT`。

## 跨语言字符串契约

下列字面量两端必须逐字一致,改任何一处都要同改对端。PowerShell 侧的唯一定义在 `Get-ClientEntryContract`(`tools/scripts/lib/k8s_client_entry.ps1:131`);排空脚本自己的一份在 `Get-GateDrainContract`(`tools/scripts/k8s_gate_drain.ps1`),由 `k8s_gate_drain.tests.ps1` 钉住与源头一致。

| 字面量 | 含义 | 定义 / 使用位置 |
|---|---|---|
| `battle_not_allocatable` | CreateBattle 节点级准入拒绝的 gRPC 消息,配 `UNAVAILABLE` | C++ `battle_node.cpp:15` `kBattleNotAllocatableMessage`;Go `gather.go:65` `battleNotAllocatableMessage`(精确匹配,带空白即不认) |
| `mmorpg.io/drain` | battle 排空标签键,只看键是否存在 | C++ `cpp/nodes/battle/main.cpp:45` `kAgonesDrainLabelKey`;Fleet `allocationOverflow.labels`;运维 `kubectl label gs`;契约 `DrainLabelKey` |
| `client` | Fleet 客户端端口名 | C++ `agones_client_endpoint_source.h:34` `kClientPortName`;Fleet `spec.ports[].name`;契约 `AgonesClientPortName` |
| `CLIENT_ENDPOINT_SOURCE` | `none` \| `static` \| `agones`;缺省 = none,其他值致命 | C++ `client_endpoint.h:32`;来源名表在 `client_endpoint.cpp`;契约 `ClientEndpointSource*` |
| `CLIENT_ENDPOINT_HOST` | 裸主机名或 IPv4,不带 scheme / 端口 / 路径;static 必填,agones 下为可选覆盖 | C++ `client_endpoint.h:33`;gate 启动 shell;battle hostPort 缺省值 `$(HOST_IP)` |
| `CLIENT_ENDPOINT_PORT` | 1..65535;static 必填,**agones 下禁止设置** | C++ `client_endpoint.h:34`;gate shell 计算;battle hostPort 为 20000 |
| `CLIENT_ENDPOINT_REQUIRED` | `0` \| `1`;缺省 0,其他值致命;none + 1 是矛盾配置,致命 | C++ `client_endpoint.h:35`;external 形态的 gate / battle 模板写 1 |
| `HOST_IP` | fieldRef `status.hostIP`,只参与 `CLIENT_ENDPOINT_HOST` 的计算 | gate StatefulSet、battle hostPort Deployment 模板 |
| `POD_NAME` | fieldRef `metadata.name`,gate 由它的后缀算序号 | gate StatefulSet 模板与启动 shell |
| `gate:<id>:draining` | 值 = 标记时刻的整数 Unix 秒(取 Redis `TIME`),带 EX TTL | 由排空脚本写;login `loginqueue` 的 `GateDrainingKeyFmt` 读 |
| `gate:<id>:drained` | 值 = `below_threshold` \| `deadline`;TTL 跟随 draining 的剩余 TTL | 由 login `gatedrain_monitor.go` 写;排空脚本读 |
| `GateDrain.Interval` / `DrainedBelowPlayers` / `Deadline` | login 排空判定参数,默认 5s / 0 / 25m | `go/login/internal/config/config.go:132-145`;排空脚本从 login ConfigMap 读取并校验 TTL |
| `RequireClientEndpoint` | login / scene_manager 配置的顶层布尔键,默认 false;ConfigMap 里只能是字面量 true / false | `go/login/internal/config/config.go:115`、scene_manager config;`k8s_deploy.ps1 -RequireClientEndpoint`;排空脚本也读它 |
| `mmorpg_client_endpoint_select_total{result}` | `client` \| `fallback` \| `rejected` \| `deduped` | `go/shared/clientendpoint/metrics.go` |
| `AGONES_ENABLED=1` | Fleet 模板固定写入 | `New-BattleFleetYaml`;C++ `ReadAgonesEnv` |

## battle Fleet health 预算与就绪判据

### Fleet health 预算推导(与原稿差异:不用 10 / 5 / 3)

原稿给 battle Fleet 的 health 是 initialDelaySeconds 10、periodSeconds 5、failureThreshold 3。这意味着 sidecar 起来后约 25s 内必须收到第一次 /health,否则判 Unhealthy、删除 GameServer,容易陷入重建循环。battle 启动到第一次 /health 的最坏耗时(`Resolve-BattleFleetHealth`,`lib/k8s_client_entry.ps1:553`)如下:

| 段 | 最坏耗时 | 来源 |
|---|---|---|
| a. 构造期 `InitClientEndpoint` 的 Agones 取址 | 60s | `ClientEndpointSourceOptions.totalBudget`,单调时钟截止(D79) |
| b. 读表、etcd 注册、端口 CAS 退避 | 20s(余量,**K8s 上未实测**) | Agones Pod 重建即换 PodIP,不会撞同 IP 旧注册的 180s 租约 |
| c. `SetAfterStart` 启动 lifecycle,先跑完 `RunInitialReady` | 约 53s | 30 次,退避 200ms 翻倍、封顶 2s |
| d. 第一次 /health 再等一个 healthInterval | 2s | `LifecycleOptions.healthInterval` |
| 合计 T | **135s** | |

- Agones sidecar 从自身启动起计时,最早的 Unhealthy 判定时刻约为 initialDelaySeconds + periodSeconds × failureThreshold。所以要求 initialDelaySeconds ≥ T − periodSeconds × failureThreshold。
- 生成器复用 scene 的 `-AgonesHealthInitialDelaySeconds 30` / `-AgonesHealthPeriodSeconds 10` / `-AgonesHealthFailureThreshold 3`(`k8s_deploy.ps1:97-99`),把 initialDelaySeconds 抬到 max(传入值, 135 − 10 × 3) = **105**。抬高时 `Raised = true`,部署脚本打一行说明,不静默改写运维给的值。
- 约束:periodSeconds 必须大于 2s(C++ healthInterval),failureThreshold ≥ 1,不满足直接 throw。
- 不在预算内:业务镜像**首次拉取**的耗时。sidecar 先于业务容器启动,拉镜像会吃掉这段预算,所以生产必须预拉镜像,与 scene Fleet 同一前提。

### 就绪判据

- **battle Fleet**(`Wait-ForFleetReady` `lib/k8s_client_entry.ps1:1710` → `Measure-FleetRolloutReadiness` `:1653`,截止 300s;2026-09-29 更正,初稿两个行号顺序写反):
  - 不看 Fleet.status 的合计。滚动时,旧 GameServerSet 的 Ready 实例,以及被打上排空标签的 Allocated 实例,都会让合计提前达标。
  - 当前 GameServerSet = creationTimestamp 最新的那个;最新的时间戳并列时无法判定,继续等。
  - 当前 GameServerSet 的模板 labels / annotations 必须与 Fleet 当前模板一致,用来挡住「apply 之后控制器还没建出新 GameServerSet」的窗口。残留:只改了 env 等 spec 字段的那次 apply,在控制器建出新 GameServerSet 之前的亚秒级窗口里挡不住。
  - 当前 GameServerSet 的 ready + allocated ≥ 它自己的 spec.replicas;并且当前的 ready + allocated,加上旧 GameServerSet 上仍在打的 allocated,≥ 期望副本数。旧 GameServerSet 的 Ready 实例一律不计。
  - allocated 计入的理由:在线环境重跑 `-WaitReady` 时,只看 Ready 会把「正在打」误判成未就绪、超时报失败。
  - 「GameServerSet 模板 metadata 与 Fleet 模板一致」是按 Agones 源码推断的,要在 kind 阶段 B 核实;如果实测不成立,会一直等到超时,方向是 fail-closed。
- **gate StatefulSet**(`Wait-ForGateStatefulSetReady`,`lib/k8s_client_entry.ps1:1790`):有界轮询 `.status.readyReplicas` ≥ 副本数。就绪 = tcpSocket 探针通 = 已发布进 etcd。OnDelete 下 apply 新模板不会重建现有 Pod,所以这里等到的是「现存 Pod 已就绪」,新模板何时生效由排空脚本逐个删 Pod 决定。
- **Agones CRD 探测**(`Test-AgonesFleetCrdPresent`):用 `kubectl get --raw /apis/agones.dev/v1`,不用 `kubectl api-resources`,以免无关的聚合 APIService(如 metrics-server)故障把 infra-up 整个阻断。NotFound 判为未装;资源列表里有 fleets 判为已装;其余失败(不可达、Forbidden、返回不是 JSON)一律 throw。DryRun 时跳过探测。

## 运维手册

本节只写集群外入口相关的操作;通用部署见 [deploy/k8s/README.md](../../deploy/k8s/README.md)。所有 kubectl / helm / 脚本命令都要显式指定目标集群(`-KubeContext` / `-KubeConfig`,或 `--context`),不要依赖本机默认 current-context。

### 部署参数速查(`tools/scripts/k8s_deploy.ps1:166-233`)

| 参数 | 默认 | 说明 |
|---|---|---|
| `-ClientEntryMode` | podip | podip / external,见模式矩阵 |
| `-BattleOrchestrator` | deployment | deployment / agones;只在 infra-up 路径生效(infra-up、all-up、release-all,且不带 `-SkipInfra`) |
| `-RequireClientEndpoint` | auto | auto / true / false,见下文「RequireClientEndpoint 三值」 |
| `-ClientPublicHost` | 空 | gate 与 battle 共用的裸主机名或 IPv4;kind 上填 127.0.0.1。**只用于单节点集群,或该地址确实能到达每个 gate / battle 实例的场景**:它让所有实例自报同一主机,多节点生产必须留空(见 D88 的适用范围一条) |
| `-GateClientHostTemplate` | 空 | 如 `gate-{ordinal}.{zone}.example.com`;LoadBalancer 必填;gate 副本数 >1 时必须含 `{ordinal}` |
| `-GateServiceType` | NodePort | external 下决定每序号 Service 的类型,只允许 NodePort / LoadBalancer;podip 下决定 gate-entry 的类型。**会被 `-OpsProfile` 覆盖**,见表后说明 |
| `-GateNodePortBase` | 30000 | 单 zone 的 zone-up 用;all-up 由 zones 配置的 zone 级 `gateNodePortBase` 覆盖 |
| `-GateExternalTrafficPolicy` | Local | Local / Cluster(Cluster 会 SNAT,打警告) |
| `-GatewayIngressHost` | 空 | 可含 `{zone}`;空 = 不生成,也不删除已有 Ingress |
| `-GatewayIngressClassName` | nginx | |
| `-GatewayIngressTlsSecret` | 空 | 非空时生成 tls 段 |
| `-GatewayTrustedProxies` | 空 | 逗号分隔的 CIDR;配了 Ingress 必填,且每次部署 gateway 都要照传 |
| `-LoginDevPasswordAuth` | 关 | 只允许 `-ReleaseProfile dev`,见「login 开发口令认证」 |
| `-AllowDisruptiveSwitch` | 关 | 确认切换形态或改地址来源,见下文 |

**`-OpsProfile` 的交互**(2026-09-29 补充;`k8s_deploy.ps1:770-796` 的 `Apply-OpsProfileDefaults`,在 preflight 之前执行):

- `managed-cloud`:无条件把 `-GateServiceType` 改为 LoadBalancer,显式传 NodePort 也会被改写(`:773`)。所以 external + managed-cloud 必须给 `-GateClientHostTemplate`,否则 preflight 拒绝(`lib/k8s_client_entry.ps1:1393`)。
- `bare-metal`:强制改为 NodePort(`:785`)。
- 两者都把 gate 副本数抬到 ≥2;battle 副本数大于 0 时也抬到 ≥2(`:782`、`:793`)。external + deployment(hostPort)形态因此至少需要 2 个节点;这时也必须留空 `-ClientPublicHost`(D86)。
- 默认 `custom` 不改任何值。

包装入口:

- `dev_tools.ps1` 与 `k8s_image.ps1` 都按「留空不覆盖」透传上述参数,默认路径不受影响。两者的 `-GateServiceType` 默认值都改为留空、跟随 k8s_deploy。`k8s_image.ps1` 原来默认 LoadBalancer,改动只影响 `-OpsProfile custom` 且未显式传值的路径。
- `k8s_image.ps1` 只透传 gate / battle 侧参数,加上 `-AllowDisruptiveSwitch` 和 `-RequireClientEndpoint`。它不部署 Go / Java 服务,所以 `-RequireClientEndpoint` 经它只参与 k8s_deploy 的组合预检,并在入口打告警(`k8s_image.ps1:442-443`)。
- `-Gateway*` 与 `-LoginDevPasswordAuth` 只能经 `dev_tools.ps1` 的 k8s-infra-up / zone-up / all-up,或直接调 `k8s_deploy.ps1` 生效;经 dev_tools 的 k8s-image-* / k8s-release-* 显式给出会被拒绝。
- 存量修复:`dev_tools.ps1` 的 k8s-image-* / k8s-release-* 自 31e4d1d4c 起一直多传 4 个 k8s_image 未声明的 Agones 参数,在参数绑定阶段就失败。本批已删掉这 4 个键,并对 `-AgonesHighDensity` / `-AgonesAutoscale` 显式拒绝。这条链路很久没人跑通过,首次使用建议先加 `-DryRun`。

### 模式不粘滞与切换闸

- **不粘滞的参数**:`-ClientEntryMode`、`-BattleOrchestrator`、全部地址参数(`-ClientPublicHost`、`-GateClientHostTemplate`、`-GateNodePortBase`、`-GateServiceType`、`-GateExternalTrafficPolicy`)、`-GatewayTrustedProxies`、`-RequireClientEndpoint`、`-LoginDevPasswordAuth`,以及原有的 `-GateRouterMode`、`-ReleaseProfile`。脚本不从集群读回旧值,每次部署都要照传上一次的同一组值。
- **集群现状预检**(`Assert-ClientEntryClusterState`,`k8s_deploy.ps1:1000`):只在非 DryRun 时执行,在任何写操作之前把 battle 与各 zone 一次判完,拒绝时报 `集群现状预检失败(N 项),未做任何写操作`。判四类情形:
  1. 换 kind 的切换(gate Deployment ↔ StatefulSet、battle Deployment ↔ Fleet),且会踢人的工作负载在集群里确实存在。报错原文形如 `当前集群是 statefulset gate(…),本次参数会删除它:… 要保持现状请显式传 -ClientEntryMode external;确认要切换请加 -AllowDisruptiveSwitch。`
  2. battle 在同一种 kind 内换入口模式或换客户端主机(`Get-BattleEntryDrift`,读现有容器 env 里的 `CLIENT_ENDPOINT_SOURCE` / `CLIENT_ENDPOINT_HOST`)。
  3. external gate 的地址来源变了(`Get-GateAddressDrift`,与 StatefulSet 模板里的地址 env 比对)。gate 是 OnDelete,现有 Pod 仍自报旧地址,每序号 Service 却会立刻改写,整个 zone 会登录失败。
  4. zone 里已有 Ingress gateway,本次部署 gateway 却没给 `-GatewayTrustedProxies`。这一类**不受** `-AllowDisruptiveSwitch` 豁免。
- 1–3 给了 `-AllowDisruptiveSwitch` 就只打警告放行。删除旧形态时 `Remove-ClientEntryObsoleteResources` 还会再判一次(纵深防御),拒绝时报 `拒绝删除正在服务的工作负载(…),未删除任何资源`。
- 任何 kubectl 失败(集群不可达、Forbidden)都 throw:查不到不等于不存在。部署账号需要对 sts / deploy / fleet / ingress 的 get 权限,以及 agones.dev 组的发现权限。
- DryRun 跳过集群现状预检,输出一行 `[dry-run] 跳过集群现状预检…`。
- `-AllowDisruptiveSwitch` 只在维护窗口里显式加,意味着接受整台 gate 踢人、在打的战斗作废。带它改了 gate 地址之后,现有 gate Pod 仍自报旧地址,必须用排空脚本带 `-DeletePod` 逐个重建。

### RequireClientEndpoint 三值

- `auto`(默认)跟随 `-ClientEntryMode`:external → true,podip → false(`Get-ClientEntryRequireClientEndpoint`)。
- `true`:login / scene_manager 跳过没自报 `client_endpoint` 的 gate。podip 配 true 会在写操作之前被拒:podip 的 gate 从不自报地址,全部 gate 都会被跳过。
- `false`:没自报地址的 gate 回落下发 PodIP。external 配 false 会打警告,只用于逐个 zone 切 external 的上线窗口。原因:已切的 zone 若取 true,scene_manager 跨 zone RedirectToGate 时会跳过未切 zone 的 gate,跨区跳转失败。全部 zone 切完、确认所有 gate 都已自报地址之后,回到 auto 并滚动 login 与 scene_manager。
- `RequireClientEndpoint=true` 且整个 zone 的 gate 都缺地址时,候选集为空,登录与跨 zone 跳转整体失败。这是设计要的 fail-closed。

### gate 排空(`tools/scripts/k8s_gate_drain.ps1`)

用法(gate 滚动更新、换镜像、改地址之后重建,都走它;gate StatefulSet 是 OnDelete,apply 新模板不会自动重建 Pod):

```powershell
# 只读定位 + 打印计划(不写 Redis、不删 Pod)
pwsh -File tools/scripts/k8s_gate_drain.ps1 -ZoneName <zone> -Ordinal <i> -DryRun -KubeContext <ctx> -KubeConfig <file>
# 排空 gate-<i>,drained 之后删 Pod 并等 StatefulSet 重建就绪
pwsh -File tools/scripts/k8s_gate_drain.ps1 -ZoneName <zone> -Ordinal <i> -DeletePod -KubeContext <ctx> -KubeConfig <file>
```

- 参数:`-DrainTtlSeconds`(默认 1800)、`-WaitTimeoutSeconds`(默认 1800)、`-ReadyTimeoutSeconds`(默认 300)、`-NamespacePrefix`(默认 mmorpg-zone,与 k8s_deploy 同义)、`-DeletePod`、`-DryRun`、`-KubeContext` / `-KubeConfig`。
- 流程(详见脚本文件头):
  1. 从 zone 的 login ConfigMap(`go-svc-login-config` / `login.yaml`)读出 zone_id、Redis、etcd、`RequireClientEndpoint` 与 GateDrain 参数。
  2. 在写之前校验预算。
  3. 取 Pod `gate-<i>` 的 podIP,在 etcd 里按 `endpoint.ip` 定位唯一的 NodeInfo,得到 node_id;0 条或多条都报错。
  4. 以 Redis 服务器的 `TIME` 为标记时刻,用一次原子 EVAL 写 `gate:<id>:draining`:同 zone 其余候选都已在排空时,什么都不写并拒绝;否则先 DEL 残留的 drained,再 `SET … NX EX <ttl>`。写完立即复核 Pod UID 与 etcd 里的进程身份,变了就撤回标记并中止。
  5. 每 5s 轮询 `gate:<id>:drained`,期间定期复核被排空的进程还在不在。
  6. 只有带 `-DeletePod` 才删 Pod。删之后等旧进程的 etcd 记录消失,再清掉两个标记,最后等新 Pod 与 StatefulSet 就绪。
- **退出码**:
  - `0`:完成。含 `-DryRun`,以及不带 `-DeletePod` 时到 drained 为止。
  - `1`:出错中止,输出里会说明标记的现状。
  - `3`:结束时留下了需要人工清理的排空标记,输出里有 DEL 指引。清理之前,之后拿到同一 node_id 的 gate 分不到玩家。
- 不带 `-DeletePod` 跑完之后,用同样的参数加 `-DeletePod` 重跑,即可接着删。
- **单副本 zone 不能无损排空**:login 的 `FilterDrainingGates` 在候选全部 draining 时会忽略标记照常分配,标了也挡不住新玩家。脚本在任何 Redis 写之前拒绝(原子 EVAL 回 `NO_UNDRAINED_PEER`;`-DryRun` 也会按 etcd 判定并拒绝)。要先把该 zone 扩到 ≥2 个 gate 再排空。
- `reason=deadline` 时打醒目警告:这台 gate 上还有玩家,删 Pod 会让他们断线重连。
- 取消排空:在 login 所连的 Redis 上执行 `DEL gate:<id>:draining gate:<id>:drained`。
- **脚本不做缩容**。直接 scale StatefulSet 之后,要等旧 node_id 的 etcd 记录消失,再手工 `DEL gate:<id>:draining gate:<id>:drained`。不清的话,因为 node_id 全局唯一、按最小空闲号复用,而 login 的 Redis 跨 zone 共用,之后拿到这个 node_id 的 gate(可能在任意 zone)在 TTL 内分不到玩家。
- 依赖:脚本 `kubectl exec` 进 `svc/etcd`、`svc/redis` 背后的 Pod,再按 login 配置里的 Service 地址连回同一个 Service,需要 pods/exec 权限,也依赖集群支持 hairpin 流量。Redis 若用 ACL 禁了 EVAL,脚本会 fail-closed 拒绝。这三项都要在 kind 上顺带验证。

### GateDrain 配置与 Deadline 约束

- login 配置块 `GateDrain`(`go/login/internal/config/config.go:117-145`):`Interval` 默认 5s,`DrainedBelowPlayers` 默认 0,`Deadline` 默认 25m。块刻意**不标 optional**:K8s 的 login ConfigMap 由 `k8s_deploy.ps1` 另写模板、不含这一块,整块缺失时全部取默认值。
- 判定:已标 draining 的 gate,在线数 ≤ DrainedBelowPlayers 时写 `drained=below_threshold`;标记满 Deadline 时写 `drained=deadline`,不论还剩多少人,并打 ERROR。drained 的 TTL 跟随 draining 的剩余 TTL,默认只有约 5 分钟的观察窗口。
- **Deadline 约束**:`-DrainTtlSeconds` 与 `-WaitTimeoutSeconds` 都必须 ≥ Deadline + 观察窗口,窗口取 max(60, 2 × (判定周期 + 5s 轮询 + 10s 单次查询))。脚本在入口、任何写之前校验;默认 1800s 对 1500s 能通过。如果 Deadline 不早于 TTL,draining 会先过期,gate 重新接客,drained 永远等不到。调小脚本 TTL 时同步调小 Deadline。
- `Deadline = 0`:永不因超时放行,只认人走干净。这时有人的 gate 只能等脚本超时,脚本不删 Pod。
- `Interval ≤ 0` 会关闭监控,启动日志打 ERROR `[GateDrain] monitor NOT started`,drained 永远不出现。
- 正常的接线证据:login 启动日志里有 `[GateDrain] monitor started: interval=5s drained_below=0 deadline=1500s`。
- 每个 login 副本各跑一份判定,幂等。判定只覆盖本 zone 的 gate:没有 login 副本的 zone,它的 gate 永远等不到 drained。
- 手工 SET 且没带 EX 的 draining(TTL = -1)不会产生 drained(login 打 ERROR 提示补 TTL 或 DEL);排空脚本也拒绝沿用这种标记。

### battle 节点维护(操作建议,未在集群上验证)

- battle Fleet 设了 `eviction.safe: Never`,gate 有 PDB `maxUnavailable: 0`。两者都会挡住 `kubectl drain` 与 cluster-autoscaler,必须先按下面的步骤手动腾空。
- battle:
  1. `kubectl cordon <node>`。
  2. 给该节点上的每个 GameServer 打排空标签:`kubectl -n mmorpg-infra label gs <name> mmorpg.io/drain=true`。进程最多 5s 内读到标签,之后拒绝新房间(match 收到 `battle_not_allocatable` 会换节点)。
  3. 等这些 gs 回到 Ready:在打的局打完即回,单局上限 BattleMaxDurationSeconds = 300s。
  4. `kubectl -n mmorpg-infra delete gs <name>`,由 Fleet 在别的节点补齐。
  5. 最后再 drain 节点。
  要中途撤销,删掉标签即可:`kubectl -n mmorpg-infra label gs <name> mmorpg.io/drain-`。
- gate:对该节点上的每个 gate 序号跑一次排空脚本(带 `-DeletePod`)。节点已 cordon,重建的 Pod 会落到别的节点;启动 shell 会按新节点重算 `HOST_IP`,所以 NodePort + HOST_IP 形态会自报新的节点地址。

### 防火墙

- 对公网只放行三类端口:
  1. gate 的 nodePort 段:每个 zone 的 [gateNodePortBase, gateNodePortBase + 副本数 − 1],推荐落在 30000–30085;LoadBalancer 形态则是各 LB 的 18000。
  2. Agones 端口段:helm 的 `gameservers.minPort`–`gameservers.maxPort`。kind 上是 7100–7109,生产按实际安装值。
  3. Ingress controller 的 80 / 443。
- 只有 external + deployment(验证 / 回退)形态,才需要临时放行 battle 的 hostPort 20000。
- 永不放行:任何 gRPC 端口(TCP 端口 + 30000,如 battle 的 50000,没有鉴权);gate 段以外的 NodePort 范围。`/admin`、`/actuator` 不经 Ingress,本来就不出集群。
- battle 票据不绑 IP、整局有效。公网暴露后,收窄防火墙是目前唯一的缓解手段(battle-transport-decision.md §8.5)。

### Java 管理面(只在集群内)与 admin 口令

- Ingress 只路由 `/api`。`/admin/**`(zones、whitelist、announcements 等管理接口)和 `/actuator/**` 只能在集群内访问。运维要调管理面时,用 `kubectl -n <zone ns> port-forward svc/gateway <本地端口>:8081`,带 `X-Admin-Key` 请求头。
- 口令来源:部署前设环境变量 `MMORPG_GATEWAY_ADMIN_API_KEY`。k8s_deploy 把它写进 zone namespace 的 Secret `gateway-admin-api-key`(键 `api-key`),再经 secretKeyRef 注入 gateway 容器的 `ADMIN_APIKEY`(`k8s_deploy.ps1:291-303`)。
- 纵深防御:生成的 gateway ConfigMap 显式写 `admin.api-key: "${ADMIN_APIKEY}"`。env 注入一旦丢失,Spring 解析不了占位符会直接拒启,而不是静默回落到 jar 内公开的 `change-me-in-production`。
- 档位规则:非 dev 档缺失、仍是占位串、或短于 32 位,都在任何写操作之前拒绝部署。dev 档回落占位值并打警告。
- 轮换:Pod 模板带口令指纹注解 `mmorpg.io/gateway-admin-api-key-hash`(12 位十六进制截断哈希,以 Secret 名作域分隔)。口令一变模板就变,重跑 zone-up 会自动滚动 gateway。本变更之后的第一次 zone-up 也会滚动 gateway 一次。
- 上线后需实测一次:在集群内用注入的口令请求 `/admin/**` 应被接受,用 `change-me-in-production` 应被拒。「Spring 把 `ADMIN_APIKEY` 绑定到 `admin.api-key`、且优先于 application.yaml」只按文档与源码核对过,没有运行期证据。
- 已知未做(独立任务):`AdminApiKeyFilter` 按 `getRequestURI().startsWith("/admin/")` 判断,`/admin;x/zones` 这类写法可能绕过,应改为按规范化后的路径判断并用常量时间比较。Ingress 收窄到 `/api` 不能替代这一项。
- 注意:Java gateway 的 HTTP 接口不校验 access token,access token 在 gate TCP 的 Login 上出示。

### login 开发口令认证(`-LoginDevPasswordAuth`)

- 集群内 login 的 ConfigMap 原来没有任何认证配置(没有 PasswordAuth / DevPasswordAuth / SaToken),login fail-closed,robot 无论用哪种 auth_type 都登不上。
- 带 `-LoginDevPasswordAuth` 时:login ConfigMap 写 `DevPasswordAuth`(Enabled、`SharedSecretEnv: LOGIN_DEV_PASSWORD_SHARED_SECRET`、`AllowedAccountPrefixes: robot_ / dev_`);共享口令从环境变量 `MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET` 读入 Secret `login-dev-password`,经 secretKeyRef 注入 login,绝不进 ConfigMap。
- 只允许 `-ReleaseProfile dev`,其他档位在任何写操作之前报错,与 `release_preflight.ps1` 的 `debug.login.devpassword` 门禁同口径。
- 环境变量未设置时拒绝部署,没有回落值。robot yaml 的 `password` 必须与它一致,账号必须以 `robot_` 或 `dev_` 开头。
- 本次不部署 login 时(没给 `-GoSvcRegistry` 或带了 `-SkipGoSvc`)只打警告,开关不生效。关掉开关后,脚本会在 login Deployment apply 之后删除残留的 Secret。

### 客户端地址排障

**先分清日志在哪**(2026-09-29 补充)。Linux 下 C++ 节点的 `LOG_*` 只写日志文件,只有 WIN32 构建才同时打到控制台(`Node::AsyncOutput`,`node.cpp:1300-1310`),所以下表的 C++ 日志大多**不在 `kubectl logs` 里**:

- **`kubectl logs` 看得到的**:`fprintf` 到 stderr 的 `FATAL client endpoint: …` / `WARN client endpoint: …`(`node.cpp:355-362`、`:580-593`);gate 启动 shell 的 `gate: client endpoint …` / `gate: bad …`;全部 Go 服务日志(login、scene_manager、match 等,go-zero 默认输出到控制台)。
- **只在 C++ 日志文件里的**:`Client endpoint resolved …`、`battle 客户端直连面已就绪 …`、`battle 票据签发被拒 …`、`CreateBattle 拒绝 …` / `CreateBattle 成功 …`、`[Agones] …`,以及 gate 的安全拒绝采样日志。文件在容器内的 `/app/bin/logs/cpp_nodes/*.log`,读法是 `kubectl exec <pod> -c <gate|battle> -- sh -c 'tail -n 200 /app/bin/logs/cpp_nodes/*.log'`(带上 `--context` / `-n`)。也可以查 Loki 的 `{job="cpp_nodes"}`(`k8s_deploy.ps1:1821`,见 docs/ops/grafana-loki-local-logs.md)。日志目录是 emptyDir,Pod 内的容器重启后文件还在;但 CrashLoop 中 exec 不进去,Agones 的 GameServer Pod 被删后文件也随之消失,这两种情况只能查 Loki。

| 现象 / 日志 | 含义 | 处理 |
|---|---|---|
| stderr `FATAL client endpoint: invalid settings: …`,Pod CrashLoopBackOff | `CLIENT_ENDPOINT_*` 配置非法。例:`CLIENT_ENDPOINT_HOST is required when CLIENT_ENDPOINT_SOURCE=static`;`CLIENT_ENDPOINT_REQUIRED must be 0 or 1, got '2'`;agones 来源设了 PORT | 用 `kubectl logs <pod> --previous` 看原因(业务日志文件可能来不及落盘),修模板或参数 |
| stderr `FATAL client endpoint: CLIENT_ENDPOINT_SOURCE=agones but the source factory returned null …` | 该构建或环境没启用 Agones,如 Windows 构建没有 curl、`AGONES_ENABLED` 未设 | 核对 Fleet 模板与镜像 |
| stderr `FATAL client endpoint: CLIENT_ENDPOINT_SOURCE=agones but this node registered no ClientEndpointSourceFactory hook`(`node.cpp:601-602`)(2026-09-29 补充) | 非 battle 节点(如 gate、scene)被设成 agones 来源。只有 battle 注册了 Agones 地址来源的预构造钩子 | gate 只能用 static(D88);查是谁往该工作负载注入了 `CLIENT_ENDPOINT_SOURCE=agones` |
| stderr `FATAL client endpoint: resolve failed: source=agones error=…` | 60s 预算内没从 sidecar 取到 `status.address` 或 `client` 端口 | 查 sidecar、RBAC(`agones-sdk`)、`/gameserver` 的 JSON 形状 |
| stderr + WARN `WARN client endpoint: CLIENT_ENDPOINT_HOST/PORT set but CLIENT_ENDPOINT_SOURCE=none; ignored. …` | 给了地址却漏写 SOURCE=static:节点照常启动,但不自报地址,消费方回落 PodIP,集群外连不上 | 补 `CLIENT_ENDPOINT_SOURCE`;podip 形态下不该出现这一行 |
| 启动日志 `Client endpoint resolved. source=… endpoint=… client_endpoint=… required=…`(C++ 日志文件,不在 `kubectl logs` 里) | 每个节点都会打,是地址自报的证据;podip 时为 `client_endpoint=(unset, clients use endpoint) required=0` | 验收 6a / 9a 用 |
| gate stderr `gate: client endpoint <host>:<port> (ordinal <i>)`;`gate: bad ordinal …` / `gate: bad GATE_NODE_PORT_BASE=…` 后 exit 64 | 启动 shell 算出的地址;或 POD_NAME 序号 / 基址不是数字 | 核对 StatefulSet 名与生成器内部 env |
| battle `battle 客户端直连面已就绪: endpoint=… client_endpoint=… client_facing=… client_endpoint_required=…` | battle 实际下发的地址是 `client_facing`;它为空时会另打 LOG_ERROR,此后每张票都签不出 | |
| battle `battle 票据签发被拒: 本节点没有客户端可达地址 …` | external 下缺 `client_endpoint`,BuildAssignment 拒签,match 侧按 `kServiceUnavailable` 处理 | 查该节点的 FATAL / WARN 与 env |
| battle `CreateBattle 拒绝: 本节点不接新房间 … admission=not_started`(或 `admission=closed`)/ `本实例不可分配 … draining=1` | 启动未完成、停机中、打了排空标签,或 allocate 未确认 | 正常准入拒绝,match 会换节点;持续出现时查 Agones 状态 |
| battle `CreateBattle 拒绝: 投递期间停机已开始(loop 内复核)`(`battle_node.cpp:120`)(2026-09-29 补充) | 第三道准入:请求通过前两道后,投递进 loop 时停机已经开始 | 停机窗口内的正常拒绝,match 会换节点;不需要处理 |
| login `[GateSelect] gate skipped: no client-reachable address …`(30s 节流) | require=true 时 gate 没自报地址;或 require=false 时连 endpoint 都不可用(坏记录) | 查该 gate 的启动日志 |
| login 启动 `[GateSelect] RequireClientEndpoint=…` | login 实际采用的 require 值 | |
| scene_manager ERROR `selectGateTargets: N 台 gate 没有可下发给客户端的地址,已跳过(require_client_endpoint=…)`(`go/scene_manager/internal/logic/gate_redirect.go:152`)(2026-09-29 补充) | 跨 zone RedirectToGate 时,目标 zone 有 gate 没自报地址而被跳过(require=true),或连 endpoint 都不可用;全被跳过时 RedirectToGate 返回错误,跨区跳转失败 | 先查**发起方** scene_manager 所在 zone 的 `RequireClientEndpoint`,再查**目标 zone** 的 gate 是否已切 external、是否自报了地址。逐个 zone 切换的窗口里,已切的 zone 应取 false(见「RequireClientEndpoint 三值」) |
| `mmorpg_client_endpoint_select_total` | external 稳态下 `fallback` 应恒为 0;`rejected` > 0 说明有 gate 没自报地址;`deduped` 在 NodeTTL 窗口内短暂增长属预期,持续增长说明两台活 gate 报了同一地址(模板或 nodePort 基址配错) | 绝对值随登录 QPS × gate 数增长,看比例与是否非 0,不看速率 |
| gate 日志 `token_gate_node_mismatch` | 客户端连到了与票据不符的 gate:经 gate-entry 随机落点(D90),或地址撞车 / 陈旧记录(D78) | 按上面几行排查。**注意(2026-09-29 补充)**:gate 侧这一行走采样日志 `LogClientSecurityRejectionSampled`(`client_message_processor.cpp:40-50`,调用处 `:996`)。它所有拒绝原因共用一个计数,只记第一次和此后每第 1024 次,记下的一条也只带当次的原因,所以日志里没有这一行不等于没发生过。要计数请看客户端侧:robot 的 WARN `gate token verify failed` 带 `gate token rejected: token not for this gate`,统计行里有 `gate_token_retry=N`(`robot/main.go:368`、`robot/metrics/stats.go:191`) |

### 回滚(`k8s_zone_rollback.ps1`)第 0 步预检

- 集群外入口参数在回滚时同样不粘滞。Step 1 的 zone-down 会删掉整个 zone namespace,Step 6 的 zone-up 面对的是空 namespace,k8s_deploy 自己的集群现状预检一条都触发不了。所以核对前移到 `-Apply` 的第 0 步,在停服之前做(`k8s_zone_rollback.ps1` 文件头第 15–48 行)。
- 第 0 步 a · 集群现状(只读,一次 kubectl):凡是留空的项,都与集群现状核对。
  - `-GateRouterMode` 留空:gate 模板里的 `GATE_CLIENT_RPC_ROUTER` 必须等于默认值。
  - `-ClientEntryMode` 留空:gate 的当前形态必须与默认值一致(StatefulSet = external,Deployment = podip),两种形态都认。
  - `-GatewayIngressHost` 与 `-ConfirmNoGatewayIngress` 都没给:namespace 里不能有 Ingress gateway,而且必须读得到。一个 gate 都没有时,比如上一次回滚中途放弃、namespace 已删,判为读不到并拒绝。
  - 核对不一致或判不出,都在停服之前拒绝。
- 第 0 步 b · 静态预检:把 Step 6 要传的同一份参数加 `-DryRun`,完整跑一遍 `dev_tools.ps1 k8s-zone-up`,非 0 即拒绝。参数组合、档位与密钥的规则只在 k8s_deploy 一处,这里不复制。这一步要求 git 可用。
- 以 external 运行、或经 Ingress 对外的 zone 回滚时,须照传:`-ClientEntryMode external`、`-GateServiceType`、`-GateNodePortBase`、`-ClientPublicHost` / `-GateClientHostTemplate`、`-GateExternalTrafficPolicy`、`-Gateway*`(含 TrustedProxies);上线窗口里还要照传 `-RequireClientEndpoint`;staging / prod 必须传 `-ReleaseProfile`(留空按 dev 档部署);依赖开发口令登录的 dev zone 照传 `-LoginDevPasswordAuth`。
- `-ConfirmNoGatewayIngress` 表示「回滚后不要 Ingress」,与 `-GatewayIngressHost` 互斥;dev_tools 入口上的名字是 `-RollbackConfirmNoGatewayIngress`。
- `-AllowDisruptiveSwitch` 只透传给 Step 6,仅在 namespace 没删净、k8s_deploy 的预检真的触发时起作用,**不豁免第 0 步**。dev_tools 的 k8s-zone-rollback 入口现在会透传 `-KubeContext` / `-KubeConfig`;以前会静默吞掉它们,回滚落在当前 context 上。
- 已知未做(独立任务):Step 6 的 GoSvcTag / JavaSvcTag 取的是当前工作树的 git sha,不是回滚目标版本;OpsProfile、GateReplicas、SceneReplicas 等也不透传。

## kind 端到端验证

本节标题被 `deploy/k8s/kind-config.yaml`(第 2、9、60 行)与 `deploy/k8s/README.md`(`:259`、`:749`)按原文引用,不要改名;完整清单见文首「引用本文的文件」。执行者是用户或 Codex(AGENTS §10.1);安装 kind、Agones、ingress-nginx 属于「安装工具」,要用户单独授权。本节全部步骤尚未实际跑过。

### 前置条件

- 用户已授权安装 **kind v0.33.0**(`go install sigs.k8s.io/kind@v0.33.0`,不要 `@latest`)。node 镜像在 `kind-config.yaml` 里钉为 `kindest/node:v1.35.8@sha256:07b2…23c0`。理由:Agones 1.58 只支持 Kubernetes 1.33–1.35,ingress-nginx v1.15.x 也只支持到 1.35,而 kind v0.33.0 默认的 node 是 v1.37.0。
- Docker Desktop 在运行;`docker ps -a` 里没有 `mmorpg-control-plane`;**全程不执行任何 minikube 命令**。本机默认 current-context 是另一个项目的集群,严禁落到它上面。
- **先停掉宿主机上直接跑的全部 C++ 节点**(`bin/*.exe`),kind 集群存在期间也不要再起。它们会占 20000 / 30000 / 30001:非 gate 节点的 TCP 端口在 20000..35535 里分配。gate 在 10000..19999 里分配,18080 / 18443 也在这个区间里(2026-09-29 补充,口径同 `kind-config.yaml:48-50`)。20000 挪不了,它就是 battle 的 `CLIENT_ENDPOINT_PORT`。唯一的例外是 6f 第 1 种负向验证,见该条说明。
- 本地 sa-token 认证服务默认占 18080(`server.port=${SERVER_PORT:18080}`)。它起着的话先停,或者用 `SERVER_PORT` 挪走。
- Agones 段选 7100–7109,避开常驻 redis-cluster 的 7000–7005 / 17000–17005,所以不用停 redis-cluster。
- **端口预检**(只读,任一条有输出就中止,先处理占用方。extraPortMappings 在 kind create 时一次性全部绑定,哪怕阶段 A 用不到 Agones 段也一样):

```powershell
$ports = @(18080, 18443, 30000, 30001, 20000) + (7100..7109)
Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue | Where-Object { $ports -contains $_.LocalPort }
netsh interface ipv4 show excludedportrange protocol=tcp   # 上面 15 个端口都不能落在保留段里
```

- 本节命令中的公共变量(PowerShell,工作目录为仓库根):

```powershell
$before = kubectl config current-context                 # 先记下默认 context,步骤 1 核对它没被切走
$env:KUBECONFIG = "$env:TEMP\kind-mmorpg.kubeconfig"
$kc = $env:KUBECONFIG
$env:MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET = '<开发口令,与专用 robot yaml 的 password 相同>'
```

- fail-closed 约定:kubectl 一律带 `--context kind-mmorpg`;helm 一律带 `--kube-context kind-mmorpg --kubeconfig $kc`;k8s_deploy.ps1 与 k8s_gate_drain.ps1 一律带 `-KubeContext kind-mmorpg -KubeConfig $kc`。默认 kubeconfig 里没有 `kind-mmorpg`,环境变量丢了会直接报「context 不存在」,不会部署到别的集群。新开窗口必须重新设 `$env:KUBECONFIG`。
- **取证口径**(2026-09-29 补充)。下文凡是写「日志有 …」的 C++ 行(`Client endpoint resolved`、`battle 客户端直连面已就绪`、`[Agones] …`、`CreateBattle 成功` 等),都在容器内的 C++ 日志文件里,**不在 `kubectl logs` 里**,没在 `kubectl logs` 里出现不代表失败。读法:
  - `kubectl --context kind-mmorpg -n <ns> exec <pod> -c <gate 或 battle> -- sh -c 'tail -n 200 /app/bin/logs/cpp_nodes/*.log'`;
  - 或查 Loki 的 `{job="cpp_nodes"}`;
  - `kubectl logs` 只含 stderr(`FATAL` / `WARN client endpoint`、gate 启动 shell 的 `gate: client endpoint …`)与 Go 服务日志(login、match、scene_manager)。详见运维手册「客户端地址排障」开头。

### 阶段 A:不装 Agones(gate StatefulSet + battle hostPort)

**1. 建集群**

```powershell
kind create cluster --name mmorpg --config deploy/k8s/kind-config.yaml --kubeconfig $kc
```

通过标准:

- a. `docker port mmorpg-control-plane`:除了 apiserver 的 `6443/tcp -> 127.0.0.1:<随机端口>` 之外,恰好还有 15 条,全部是 127.0.0.1,端口集合 = {18080, 18443, 30000, 30001, 20000, 7100..7109}。
- b. `kubectl --context kind-mmorpg get node --show-labels` 带 `ingress-ready=true`。
- c. `kubectl --context kind-mmorpg get node -o jsonpath='{.items[0].spec.podCIDR}'` 落在 10.244.0.0/16 内。这个网段就是后面 `-GatewayTrustedProxies` 的取值。
- d. `kubectl --context kind-mmorpg get node -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}'` = v1.35.8。
- e. 先在原窗口执行 `Write-Host $before`,把值记进验收报告;再新开一个 PowerShell 窗口(不设 KUBECONFIG),核对 `kubectl config current-context` 的输出与记下的值相同。新窗口里没有 `$before` 变量,不能直接比较(2026-09-29 更正)。同一窗口里 `kubectl --context kind-mmorpg get node` 必须报 context 不存在。

**2. 构建并预载镜像**:按 [deploy/k8s/README.md](../../deploy/k8s/README.md)「Local Trial on kind」构建 Go 服务、Java 网关和 C++ 节点镜像,再用 `kind load docker-image … --name mmorpg` 预载。Hub 上的多平台镜像(loki、alloy 等)走 `docker save --platform linux/amd64` + `kind load image-archive`。C++ 节点这次必须是真镜像,不能用占位镜像。`kind load` 按 `--name` 找节点容器,不读 kubeconfig。

**3. 安装 ingress-nginx v1.15.1**(官方 kind provider 清单,钉在发布 tag 上,实际使用的 tag 记进报告):

```powershell
kubectl --context kind-mmorpg apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.15.1/deploy/static/provider/kind/deploy.yaml
kubectl --context kind-mmorpg -n ingress-nginx rollout status deployment/ingress-nginx-controller --timeout=300s
```

- 上面的 URL 与 Deployment 名按 ingress-nginx 的发布 tag 命名惯例写出,**未实测**,执行前先核对该 tag 存在。
- 先确认该清单里 controller 的 hostPort 仍是 80 / 443,否则 18080 / 18443 的映射会落空。
- **ingress-nginx 官方已宣布退役**(2026-03 起不再发布版本与安全修复,v1.15.x 最高只支持 k8s 1.35)。这里只用于本地验证 D91,生产的 Ingress 选型需另立决策;报告里要注明这一点。
- 拉不到镜像时,gateway 只用 port-forward 验证,并在报告里注明「D91 未在 kind 上验证」。

**4. 部署 infra**(battle 为 hostPort Deployment)。这一步不带 `-Gateway*`:Ingress 由第 5 步的 zone-up 生成,infra-up 上给了只会得到「被忽略」警告。

```powershell
pwsh tools/scripts/k8s_deploy.ps1 -Command infra-up -ZoneId 101 `
  -NodeImage local/mmorpg-node:<tag> -GoSvcRegistry local `
  -ClientEntryMode external -ClientPublicHost 127.0.0.1 `
  -BattleOrchestrator deployment -BattleReplicas 1 `
  -KubeContext kind-mmorpg -KubeConfig $kc -WaitReady
```

- `-GoSvcRegistry` 不能省:GateRouterMode 默认已是 1(turn-based §22 D75),gate 依赖 infra-up 部署的 client-rpc-router。
- preflight 会对 external + deployment 打 D86 警告,属预期。

**5. 部署 zone**(gate StatefulSet 2 副本 + Ingress + login 开发口令认证):

```powershell
pwsh tools/scripts/k8s_deploy.ps1 -Command zone-up -ZoneName e2e -ZoneId 101 `
  -NodeImage local/mmorpg-node:<tag> -GoSvcRegistry local -JavaSvcRegistry local `
  -GateReplicas 2 -GateServiceType NodePort -GateNodePortBase 30000 `
  -ClientEntryMode external -ClientPublicHost 127.0.0.1 `
  -GatewayIngressHost gateway.mmorpg.test -GatewayTrustedProxies 10.244.0.0/16 `
  -LoginDevPasswordAuth `
  -KubeContext kind-mmorpg -KubeConfig $kc -WaitReady
```

- zone namespace 为 `mmorpg-zone-e2e`(`-NamespacePrefix` 默认 mmorpg-zone),infra namespace 为 `mmorpg-infra`。
- 期望:StatefulSet `gate`;Service `gate-0` / `gate-1`,nodePort 分别为 30000 / 30001;`gate-headless`;PDB `gate`;**没有** gate-entry;Ingress `gateway` 只有一条 `/api` 路由。
- dev 档下,gateway 管理面口令没设 `MMORPG_GATEWAY_ADMIN_API_KEY` 时会回落占位值并打警告,属预期。

**6. 阶段 A 通过标准**

- a. **地址自报**。gate 的 `clientEndpoint` 分别是 127.0.0.1:30000 / 127.0.0.1:30001,battle 的是 127.0.0.1:20000;`endpoint` 仍是 PodIP。
  - 取证:etcd 的位置以 login ConfigMap 的 `Registry.Etcd.Hosts` 为准,例如 `kubectl --context kind-mmorpg -n mmorpg-infra exec svc/etcd -- etcdctl get --prefix GateNodeService.rpc/zone/101/`,battle 用前缀 `BattleNodeService.rpc/`。
  - 日志:`kubectl logs` 里,gate 的 stderr 有 `gate: client endpoint 127.0.0.1:3000x (ordinal x)`。C++ 日志文件里(读法见前置条件「取证口径」),各节点有 `Client endpoint resolved. source=static … required=1`,battle 有 `battle 客户端直连面已就绪: … client_facing=127.0.0.1:20000 client_endpoint_required=1`。
- b. **Ingress**。`curl.exe -s -o NUL -w "%{http_code}" -H "Host: gateway.mmorpg.test" http://127.0.0.1:18080/api/server-list` 返回 200;同样的 Host 头请求 `/admin/zones` 应返回 404,证明 Ingress 不路由管理面。
- c. **登录分布**。robot 在宿主机上运行,用**专用 robot yaml**,不要用默认的 `robot/etc/robot.yaml`:它是 satoken 口径,sa-token 没起时请求会落到 ingress-nginx 拿到 404,看起来像网关坏了;集群内的 login 也不配 SaToken。
  - robot 的实际链路是用 `gateway_addr` 调 HTTP `/api/assign-gate`,不带 Host 头,拿到 gate 地址后直连 gate TCP;它不直连 login。原稿「login 经 port-forward svc/login」的写法不对。
  - 专用 yaml 至少改这几项:`gateway_addr: "http://127.0.0.1:18081"`、`auth_type: "password"`、`password` = `$env:MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET`、`account_fmt: "robot_%04d"`(前缀必须是 robot_ 或 dev_)、`zone_id: 101`。
  - 另开窗口保持端口转发:`kubectl --context kind-mmorpg -n mmorpg-zone-e2e port-forward svc/gateway 18081:8081`。gateway 在 zone namespace,不在 infra;它是 HTTP,D93 只禁止 gate / battle 用 port-forward。
  - 登录 ≥ 50 次。要求:两台 gate 都有会话;没有票据落错 gate,以 robot 侧为准:统计行 `gate_token_retry=0`(它计入全部验票失败,包括过期),且没有 `gate token rejected: token not for this gate`。gate 日志里的 `token_gate_node_mismatch` 是采样日志,没出现也证明不了为 0,只能作旁证(见「客户端地址排障」,2026-09-29 更正);login 有 `[GateSelect] RequireClientEndpoint=true` 与 `[GateDrain] monitor started: interval=5s drained_below=0 deadline=1500s`;`mmorpg_client_endpoint_select_total` 里 `fallback` 与 `rejected` 为 0。
- d. **战斗冒烟**。复制 `robot/etc/battle_smoke.yaml` 成专用版本,改同样的 gateway_addr / password / zone_id。要求:`BattleAssignedS2C` 为 127.0.0.1:20000;战斗帧经直连收发;没有 ABORTED;battle 的 C++ 日志文件里每局成对出现 `[Agones] unit created/destroyed key=<battle_id>`(Disabled 形态也记账;读法见「取证口径」)。
- e. **源 IP**。gate 和 battle 的 C++ 日志文件里,peer IP 等于 `docker network inspect kind` 的 Gateway,不等于节点 IP。这只能证明没被 SNAT 成节点 IP,证明不了真实公网 IP。
- f. **负向:fail-closed**(三选一)。原稿写法「`kubectl set env` 给 gate 注入 SOURCE=static 并清空 HOST」在 gate StatefulSet 上复现不出来:启动 shell 总会 export HOST,兜底是恒非空的 `HOST_IP`,会得出「fail-closed 没生效」的假阴性。改用下面任一种:
  1. 本地直接起一个 gate 进程,环境里只设 `CLIENT_ENDPOINT_SOURCE=static`,不设 HOST 与 PORT。期望在 etcd 注册之前以非 0 退出,stderr 出现 `FATAL client endpoint: invalid settings: CLIENT_ENDPOINT_HOST is required when CLIENT_ENDPOINT_SOURCE=static`。
     - 这是前置条件「kind 存在期间不起宿主机 C++ 节点」的唯一例外(2026-09-29 补充)。理由:`Node::Initialize` 先调 `InitRpcServer`,后者在 `node.cpp:526` 调 `InitClientEndpoint`,解析失败就在 `:573` 致命退出。这一步早于端口 CAS、监听与 etcd 注册(`InitEtcdService` 在 `:498`),进程来不及占任何端口。
     - 期望之外的情况是进程没退出、继续运行。这时立即停掉它,并按失败处理。
     - 更稳妥的做法是在 kind 建起之前,或第 10 步删除集群之后再做这一种。
  2. `kubectl --context kind-mmorpg -n mmorpg-zone-e2e set env statefulset/gate CLIENT_ENDPOINT_REQUIRED=2`,再 `kubectl --context kind-mmorpg -n mmorpg-zone-e2e delete pod gate-0`(OnDelete,不删 Pod 不生效)。期望 `kubectl logs gate-0 --previous` 出现 `FATAL client endpoint: invalid settings: CLIENT_ENDPOINT_REQUIRED must be 0 or 1, got '2'`,Pod 进 CrashLoopBackOff,etcd 里没有它的新记录。
  3. battle 为 Deployment 形态时,`kubectl --context kind-mmorpg -n mmorpg-infra set env deployment/battle CLIENT_ENDPOINT_HOST-`。它的启动 shell 不重算 HOST,期望出现 `…CLIENT_ENDPOINT_HOST is required when CLIENT_ENDPOINT_SOURCE=static` 并以非 0 退出。
  - ~~验完恢复原 env:重跑第 4 / 5 步即可;第 2 种还要再删一次 Pod。~~(2026-09-29 更正:这句对第 3 种不成立,改为下面的写法。)
  - 验完恢复原 env:
    - 第 2 种:重跑第 5 步,把模板里的 `CLIENT_ENDPOINT_REQUIRED` 改回 1(该变量不参与地址漂移比对,不会被切换闸拦)。OnDelete 下还要再删一次 `gate-0`,才会生效。
    - 第 3 种:删掉 HOST 之后,重跑第 4 步时 `Get-BattleEntryDrift`(`k8s_deploy.ps1:1094`)读到的现有 HOST 是空的,与 `-ClientPublicHost 127.0.0.1` 不一致,会判为主机漂移。集群现状预检因此抛出「集群现状预检失败…」,不加 `-AllowDisruptiveSwitch` 就被拒。两种恢复办法任选其一:
      - 重跑第 4 步时加 `-AllowDisruptiveSwitch`,反正这一步本来就会作废 battle;
      - 先手工执行 `kubectl --context kind-mmorpg -n mmorpg-infra set env deployment/battle CLIENT_ENDPOINT_HOST=127.0.0.1`,再照原样重跑第 4 步。
- g. **去重**。`kubectl --context kind-mmorpg -n mmorpg-zone-e2e delete pod gate-1 --force --grace-period=0` 之后 180s 内持续登录。robot 侧不能出现 `token not for this gate`,口径同 6c(不以 gate 的采样日志判定);`deduped` 计数可以短暂增长。PDB 只作用于驱逐 API,不挡直接删 Pod。
- h. **排空**。`pwsh tools/scripts/k8s_gate_drain.ps1 -ZoneName e2e -Ordinal 1 -DeletePod -KubeContext kind-mmorpg -KubeConfig $kc`,期望:
  - login 不再把新玩家分到 gate-1;出现 drained 标记;Pod 被重建;标记被清除;退出码 0。
  - drained 只在 gate-1 在线数降到 0,或标记满 25 分钟后才出现。验收前先让 robot 断开 gate-1,或预留 25 分钟。
  - 顺带验证三件事:原子 Lua 在 redis:7.2 上的真实回复(OK、(nil)、`NO_UNDRAINED_PEER`、`(integer) -1`);`exec svc/redis`、`exec svc/etcd` 之后的 hairpin 连通;对单副本 zone 执行时被拒绝(可用 `-DryRun` 验)。

### 阶段 B:装 Agones(battle Fleet)

**7. 安装 Agones 1.58.0**(镜像来自 us-docker.pkg.dev,可能拉不下来,拉不到就停在这里并报告;阶段 A 的结论不受影响):

```powershell
helm repo add agones https://agones.dev/chart/stable
helm install agones agones/agones -n agones-system --create-namespace --version 1.58.0 `
  --set gameservers.minPort=7100 --set gameservers.maxPort=7109 `
  --set "gameservers.namespaces={mmorpg-infra}" `
  --kube-context kind-mmorpg --kubeconfig $kc
```

- 版本先用 `helm search repo agones/agones --versions` 核对。scene 也走 Agones 时,把 zone namespace 一并加进 `gameservers.namespaces`。
- 端口段必须是 7100–7109,与 `kind-config.yaml` 的映射一一对应。用了 7000–7009 的话,GameServer 分到的 hostPort 没有映射,客户端要么被拒,要么打进 redis-cluster。
- 确认 FleetAllocationOverflow 特性在 1.58 下已启用;未启用时 `allocationOverflow` 不生效,排空退化为运维手动 `kubectl label gs`。

**8. 重新部署 infra**:参数同第 4 步,把 `-BattleOrchestrator` 改为 `agones`,**并加 `-AllowDisruptiveSwitch`**。集群里已有 Deployment 版 battle,不加会被切换闸拒绝;这一步会作废在打的局。期望:

- 旧的 battle Deployment 被删除;Fleet 渲染出 `initialDelaySeconds: 105`(部署输出有一行抬高说明);
- Fleet 的 `client` 端口为 Dynamic;env 没有 `CLIENT_ENDPOINT_PORT`;`Wait-ForFleetReady` 在 300s 内通过。

**9. 阶段 B 通过标准**

- a. `kubectl --context kind-mmorpg -n mmorpg-infra get gs` 状态为 Ready,端口在 7100–7109 之间;etcd 里 battle 的 `clientEndpoint` = 127.0.0.1:<该端口>;battle 的 C++ 日志文件里有 `[Agones] lifecycle starting … require_loop_heartbeat=true … drain_label=mmorpg.io/drain`。GameServer Pod 被删后文件随之消失,要查 Loki。
- b. 战斗冒烟通过;打的过程中 gs 为 Allocated,结束后回 Ready。
- c. 打的过程中改 Fleet 模板(例如加一个注解)触发滚动:旧 gs 被打上 `mmorpg.io/drain=true`;这一局打完,没有 ABORTED;新房间落到新 gs;旧 gs 随后被回收;`-WaitReady` 不会在新版本还没就绪时提前返回。
- d. 边造房边把 Fleet 缩到 0:match 日志只出现 `battle_not_allocatable` 换节点重试,或 `match_gather_total{outcome="not_allocatable"}` 干净失败;battle 关闸之后,C++ 日志文件里不再有 `CreateBattle 成功`(Fleet 缩到 0 后 Pod 会被删,这一项查 Loki);没有孤儿房间。
- e. 在 battle Pod 内请求 sidecar 的 `/gameserver`(HTTP 端口见 `AGONES_SDK_HTTP_PORT`;镜像里没有 curl 就用 ephemeral container),把实测的 JSON 形状附进报告。重点核对元数据键是 `object_meta` 还是 `objectMeta`、`status.ports` 的形状。如果是别的写法,排空会一直停在上一次的判定、每 5s 打 WARN,需要按实测补键名。

**10. 清理**:`kind delete cluster --name mmorpg --kubeconfig $kc`。

### 失败时保留的证据

- 第 0 步端口预检的输出;`kind version`、`kubectl version`、`helm version`、`docker port mmorpg-control-plane`、`docker ps -a`;失败的 kind create 的完整输出。
- 资源清单分两条取(2026-09-29 更正:初稿合成一条,阶段 A 没装 Agones 时 kubectl 会报 `the server doesn't have a resource type "fleet"`,整条一行都不输出):
  - 通用:`kubectl --context kind-mmorpg get sts,deploy,svc,pod,pdb,ingress -A -o wide`;
  - 只在阶段 B 取:`kubectl --context kind-mmorpg get fleet,gs,gss -A -o wide`。
- 日志分两类取(2026-09-29 更正,口径见前置条件「取证口径」):
  - **`kubectl logs`**:相关 Pod 末尾 200 行,CrashLoop 的加 `--previous`。这里只有 stderr(`FATAL` / `WARN client endpoint`、`gate: client endpoint …`)和 Go 服务日志。
  - **C++ 日志文件**:`kubectl --context kind-mmorpg -n <ns> exec <pod> -c <gate 或 battle> -- sh -c 'tail -n 200 /app/bin/logs/cpp_nodes/*.log'`。exec 不进去时(CrashLoop,或 GameServer 已被删),查 Loki 的 `{job="cpp_nodes"}`。
- etcd 里 `GateNodeService.rpc/` 与 `BattleNodeService.rpc/` 前缀下的 NodeInfo 导出。
- k8s_deploy.ps1 的完整输出,含 preflight 警告与「集群现状预检」原文。
- 排空脚本的完整输出与退出码。
- match 日志里的 `battle_not_allocatable` 行;login 的 `[GateSelect]` / `[GateDrain]` 行;`mmorpg_client_endpoint_select_total` 的快照。
- 阶段 B 的 `/gameserver` JSON 原文。

### 与原稿 §5.2 的差异

Agones 段 7000–7009 → 7100–7109;前置条件新增端口预检、「先停宿主机 C++ 节点」、kind v0.33.0 + node v1.35.8;所有命令指定 context `kind-mmorpg`;ingress-nginx 钉 v1.15.1 并注明已退役;`-GatewayIngressHost` / `-GatewayTrustedProxies` 从第 4 步 infra-up 挪到第 5 步 zone-up;新增 `-LoginDevPasswordAuth` 与口令环境变量;6c 改为专用 robot yaml + gateway port-forward 18081;6f 改为三选一写法;6h 注明 drained 的出现条件;第 8 步加 `-AllowDisruptiveSwitch`;helm 的 `gameservers.namespaces` 按实际 namespace 名填(原稿的 `mmorpg-e2e` 不是 zone namespace 的实际名字)。

## 上线与回退顺序

**与原稿差异**:原稿 §6 写「第 4 批之后才允许在 K8s 上默认启用 GateRouterMode=1」。实际上 turn-based §22 D75 已把 `k8s_deploy.ps1 -GateRouterMode` 默认改为 "1",代码与脚本同批落地,不再单列第 4 批。集群外部署要在同一个维护窗口里启用 `-ClientEntryMode external`。

### 上线之前:代码验证(由 Codex 执行,Claude 未编译、未测试)

1. **proto 重生**。注意:`cd go && build.bat` 只包装 goctl,**不会**重生 pb 产物。AGENTS.md §4.1 的旧写法要等用户确认后再改,本文不改 AGENTS.md。正确做法(仓库根):
   - 把 `third_party\grpc\install_vs2026_dbg\bin` 放到 PATH 最前,`protoc --version` 必须是 `libprotoc 35.1`。
   - 核对客户端仓 `tools/gen_proto.ps1` 收录了 team / jubaozhai / friend 三行;缺任何一行,就用一份 `enable_unity_client: false` 的配置副本。
   - `pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`,再 `pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath tools/proto_generator/protogen/etc/proto_gen.yaml`。
   - 之后在 `robot` 目录执行 `go mod vendor`。
   - 验收用 grep,不信退出码:`go/proto/common/base/common.pb.go` 里有 `ClientEndpoint`;`cpp/generated/proto/common/base/common.pb.h` 里有 `client_endpoint`;`generated/proto/{_unified,login,db}/proto/common/base/common.proto` 里有 `client_endpoint = 11`。`generated/robot/` 下的镜像本来就不刷新,那里没有字段 11 属预期。
   - 共用工作树:全量重生会把其他会话在途的 proto 改动一并带进 diff,提交时按路径分拣。
2. **Go**:
   - `go/shared`:`go test ./nodeinfo/... ./clientendpoint/...`
   - `go/login`:`go test ./internal/svc/... ./internal/config/... ./internal/logic/pkg/...`
   - `go/scene_manager`:`go test ./internal/logic/... ./internal/noderegistry/...`
   - `go/player_locator`、`go/match`:`go test ./...`
   - `go/guild`、`go/data_service`、`go/client_rpc_router`:`go build ./... && go vet ./...`
   - 另在仓库根执行 `rg "protojson\.Unmarshal\(" go -g '!**/generated/**' -g '!*_test.go'`,无任何输出即通过。
3. **C++(Windows,必须串行 `/m:1`)**:
   - 依次 msbuild core → infra → battle → gate → scene → client_endpoint_test → agones_lifecycle_test;
   - 再跑 `pwsh -File tools/scripts/run_cpp_tests.ps1`,以及 `check_no_raw_pointer_member.ps1 -ProjectDir <各工程>`;
   - battle 的两个独立 gtest(`cpp/nodes/battle/tests/battle_room_table_test.cpp`、`battle_admission_gate_test.cpp`)按文件头命令编译运行。
   - Linux:`tools/scripts/build_linux.sh`,`ldd bin/battle | grep libcurl` 应有输出。
4. **ps1**(`pwsh -NoProfile -NonInteractive -File`):`tools/scripts/tests/` 下的 `k8s_client_entry_contract.tests.ps1`、`k8s_deploy_contract.tests.ps1`、`k8s_gate_drain.tests.ps1`、`k8s_migrate_gate.tests.ps1`、`k8s_zone_rollback_gate_router_mode.tests.ps1`、`dev_tools_merge_zone_contract.tests.ps1`,要求 fail=0。已知会红的,见「剩余风险」。

### 上线批次

1. **第 0 批:只换镜像,行为不变。**
   - 所有镜像都包含本批代码,`-ClientEntryMode` 保持默认 podip。
   - **先滚动全部 Go 服务,再滚动 C++ 节点**:宽松解析(D77)必须先于任何生产方填 `client_endpoint`。podip 下 C++ 节点不会填这个字段。
   - 注意默认路径的一处行为变化:podip 下 gate 副本数不是 1 时,不再生成 gate-entry,并会删除残留(D90)。
2. **第 1 批:基础设施就位。**
   - 安装 Agones,`gameservers.namespaces` 包含 infra namespace(scene 也用 Agones 时再加 zone namespace),端口段按防火墙规划确定。
   - 安装 Ingress controller:ingress-nginx 已退役,生产选型另立决策。
   - 按「防火墙」一节放行端口。
   - staging / prod 准备好 `MMORPG_GATEWAY_ADMIN_API_KEY`(≥32 位,非占位)。
   - 各节点预拉 battle 镜像:Fleet health 预算不含首次拉取。
   - zones 配置给每个 zone 写好不重叠的 `gateNodePortBase`。
3. **第 2 批:一个维护窗口内完成,提前公告。**
   - ~~先 infra:`infra-up -ClientEntryMode external -BattleOrchestrator agones -ClientPublicHost … -AllowDisruptiveSwitch`。~~(2026-09-29 更正:多节点生产不能带 `-ClientPublicHost`,否则所有 GameServer 都会自报同一主机,见 D88 的适用范围一条。)
   - 先 infra:`infra-up -ClientEntryMode external -BattleOrchestrator agones -AllowDisruptiveSwitch`,`-ClientPublicHost` 留空,battle 取各自的 Agones `status.address`。旧 Deployment 上在打的局会作废。
   - 再逐个 zone:`zone-up -ClientEntryMode external …地址参数… -GatewayIngressHost … -GatewayTrustedProxies … -RequireClientEndpoint false -AllowDisruptiveSwitch`。gate 从 Deployment 切到 StatefulSet,会整台踢人。
     - 地址参数(2026-09-29 补充):多节点下不用 `-ClientPublicHost`。gate 取 `HOST_IP`,即 NodePort + ETP Local,要求节点 IP 对客户端可达;或给 `-GateClientHostTemplate`,LoadBalancer 形态必须给。
   - 窗口期内已切的 zone 用 `-RequireClientEndpoint false`,否则跨 zone 跳转到未切 zone 会失败。
4. **第 3 批:收紧。** 确认 etcd 里所有 gate 都带 `clientEndpoint`(`mmorpg_client_endpoint_select_total{result="fallback"}` 稳定为 0)之后,逐个 zone 以 `-RequireClientEndpoint auto` 重跑 zone-up(其余参数照传),滚动 login 与 scene_manager。
5. 此后每一次部署、包装入口调用和回滚,都照传同一组模式与地址参数(不粘滞)。gate 换镜像走排空脚本逐个重建。

### 回退

- 顺序:
  1. `-RequireClientEndpoint false`(滚动 login / scene_manager);
  2. 需要时 `-ClientEntryMode podip -AllowDisruptiveSwitch`:脚本重新生成 Deployment 版 gate,删除 StatefulSet、每序号 Service、headless 与 PDB,会整台踢人;battle 改以 SOURCE=none 运行;
  3. 消费方自动回落到 endpoint。
- 回到 podip 等于集群外客户端完全不可用,只剩集群内 robot。只有 battle 出问题时,可以只把 `-BattleOrchestrator` 退回 deployment(external 下即 hostPort 形态,D86),同样要 `-AllowDisruptiveSwitch`,并会作废在打的局。
- `-GatewayIngressHost` 留空不会删除 Ingress。要撤掉入口,手动 `kubectl -n <zone ns> delete ingress gateway`。
- proto 字段 11 与宽松解析永久保留,不回退。

## 剩余风险与明确不做

### 剩余风险

- **全部未编译、未测试、未上集群。** 本文所有行为都只有静态阅读与评审依据,待 Codex 按「上线之前:代码验证」和「kind 端到端验证」执行后才算有证据。
- **Agones 链路第一次上集群。** 以下几项只能靠阶段 B 验证:
  - GameServer JSON 的真实形状;
  - `allocationOverflow` 在滚动更新下是否作用于旧 GameServerSet(不生效就退化为运维手动 `kubectl label gs`);
  - Fleet 就绪判据依赖的「GameServerSet 模板 metadata 与 Fleet 一致」这一推断;
  - FleetAllocationOverflow 特性是否启用。
  另外,Agones 镜像来自 us-docker.pkg.dev,可能拉不下来。
- **Fleet health 预算**里 20s 的启动余量没在 K8s 上实测;业务镜像首次拉取不在预算内。sidecar 很慢时,仍可能陷入 Unhealthy 重建循环。
- **源 IP**:Docker Desktop 下只能证明没被 SNAT 成节点 IP。NodePort + ETP Local 在多节点下要求 HOST_IP 对客户端可达。
- **`-ClientPublicHost` 误用于多节点**(2026-09-29 补充):它让所有 gate / battle 实例自报同一主机,多节点下客户端会被发往错误的节点(D88 的适用范围一条)。preflight 不知道节点数,目前对此不告警;`-OpsProfile managed-cloud` / `bare-metal` 抬高副本数之后,这个问题必然出现。**待办(未实现)**:外部入口下,battle 为 agones 编排或 hostPort 且副本数 > 1 时,preflight 若见到 `-ClientPublicHost` 非空就告警;gate 副本数 > 1、没给模板却给了 `-ClientPublicHost` 时同样告警。kind 单节点会误报,所以只做告警、不拒绝。
- **节点维护受阻**:PDB `maxUnavailable: 0` 与 `eviction.safe: Never` 会挡住 `kubectl drain` 和 cluster-autoscaler,必须按手册先腾空。已判 Unhealthy 的 Allocated battle 会被 Agones 删除,房间作废(设计如此)。
- **安全**:
  - battle 票据不绑 IP、整局有效,公网暴露后只能靠防火墙收窄(battle-transport-decision.md §8.5)。
  - `AdminApiKeyFilter` 的路径规范化没做。
  - admin 口令的 Spring 绑定没有运行期证据;口令指纹注解暴露 48 位截断哈希,读得到 Deployment 的人可以离线校验弱口令(非 dev 档要求 ≥32 位)。
- **gate**:
  - gate 崩溃前已签发的票据会失效,客户端需要重新登录。
  - scene_manager 的 RedirectToGate 不过滤正在排空的 gate(既有缺口)。
  - 两台活 gate 报了同一地址时,只有较新的那台会被下发,另一台静默不接新玩家,只能从 `deduped` 持续增长看出来。
  - 去重键里的 DNS 名区分大小写。
- **模式与地址不粘滞**:漏传参数时,集群现状预检能在写操作之前拦下,前提是集群读得到。
  - 地址漂移比对的是 StatefulSet 模板,不是逐个 Pod:带 `-AllowDisruptiveSwitch` 改了地址但还没排空重建时,旧 Pod 与模板的差异不会再被提示。
  - 回滚时显式给了与现状不同的 `-ClientEntryMode`,不需要 `-AllowDisruptiveSwitch` 就会切换:namespace 已删,显式值即决定。
- **nodePort 跨 zone 重叠**:分多次 zone-up 时靠 apiserver 的 `--dry-run=server` 分配检查拦截,需在 kind 上实测。podip 切 external 时,本 namespace 残留的 gate-entry 如果恰好占着段内端口,会被误拒。
- **排空脚本**:
  - 依赖 pods/exec、hairpin 与 Redis EVAL;
  - `Select-GateDrainPeers` 是 login 候选规则的第二份拷贝,由测试钉住 Go 源码文本;
  - 等待期间遇到进程租约短暂丢失后重注册,会被判为进程消失,撤回标记并中止。
- **battle**:
  - 排空标签首次读取失败时按未排空处理,最多 5s 的 fail-open。
  - 非阻塞调用方(scene)踢起了 allocate 却不再重试时,实例会停在「Allocated 且零单元」,这是刻意保留的 scene 旧行为。
  - 停机时,如果 gRPC drain 取消了在途的 CreateBattle,match 会按普通失败发 DestroyBattle;若 DestroyBattle 也失败,玩家要等 prepare 期限才解冻。
- **ClientFacing 与 Select** 没有跨语言共享用例,改一边必须同改另一边。
- **match**:换节点重试后,scene 的 `InBattleComp.battle_node_id` 仍记着首选节点(只用于日志,D72);matched ticket TTL 变长,match 在 gather 中途崩溃时,玩家要多等十几秒才能自愈重排。
- **已知会红的测试(属预期,待测试属主修)**:
  - ~~`k8s_deploy_contract.tests.ps1` 的 prod 基线,要等 `$ProdEnv` 补上 `MMORPG_GATEWAY_ADMIN_API_KEY`(≥32 位,非占位)才会绿;~~ **(2026-09-29 更正)** 这一条已过时,撤出「已知会红」。`$ProdEnv` 已在 `tools/scripts/tests/k8s_deploy_contract.tests.ps1:551` 补上 `MMORPG_GATEWAY_ADMIN_API_KEY`(43 位,非占位串)。该文件还有两条相关用例:prod 缺口令时拒绝(`:999`),dev 用占位值时告警(`:1011`)。prod 基线应当是绿的,**变红就是真实问题**,不能按预期放过。
  - `k8s_zone_rollback_gate_router_mode.tests.ps1` 里「拒绝 `-AllowDisruptiveSwitch`」「k8s_image 拒绝 `-RequireClientEndpoint`」两个旧负向用例(`:776`、`:820`),需要按新行为改写:dev_tools 现在透传这两个参数,不再拒绝。回滚入口在 `dev_tools.ps1:1601` 透传 `-AllowDisruptiveSwitch`,`Invoke-K8sImage` 在 `:869` 透传 `-RequireClientEndpoint`。2026-09-29 复核时两个旧用例都还在磁盘上,没改;该文件头注释 `:32-33` 也要同步改。
  - 同一文件 `:363-367` 的假 dev_tools 参数表(`PositionalBinding = $false`)还没有 `[switch]$AllowDisruptiveSwitch`,来源是 ingress2d wrappers-passthrough 的交接。它现在不会让现有用例变红;但以后新增「回滚直调带 `-AllowDisruptiveSwitch`」的用例时,会报「找不到参数」,需要测试属主一并补上。
- **共用工作树**:proto 全量重生与 robot `go mod vendor` 会把其他会话在途的改动带进同一份 diff,提交时按路径分拣。

### 明确不做

- 方案④(下发方查表翻译),以及「节点名 → 公网地址」映射表。
- gate 进 Agones;battle 每实例一个 LB,或 OpenKruise Game 适配;Agones 的 Passthrough / Static 端口策略。
- 给客户端同时下发内外两个地址;用 SDK SetAnnotation 回写地址。
- battle 的 FleetAutoscaler(本批固定 replicas)。
- Deployment 形态下 battle 的 SIGTERM 优雅排空。
- 排空超时后对剩余会话用 RedirectToGate 强制迁移:超时后直接删 Pod,玩家重新登录。
- scene 的 Health 改绑心跳(后续项)。
- gate / battle TCP 上加 TLS、票据绑定 IP。
- 客户端仓改动(Unity 已支持 DNS 名)。
- 触碰 pandora 的 minikube,或用户默认 kubeconfig 的 current-context。
- 已登记为独立任务、本批不做:
  - Java `AdminApiKeyFilter` 路径规范化与常量时间比较;
  - 回滚 Step 6 复用回滚目标版本(GoSvcTag / JavaSvcTag)与其余部署参数;
  - Agones lifecycle 增加默认拒绝许可的 NotStarted 状态(battle 已用准入闸兜住启动窗口,scene 仍有这个窗口);
  - 停机路径 `DisconnectAll` 强关房间直连;
  - D40 回合号幂等。
- AGENTS.md §4.1 的 proto 重生写法更正,需用户确认后另改。
