# Gate 负载均衡、票据与重连边界

2026-10-15 按 `6281e4d27` 核对。本文记录当前分配入口与连接约束，本次未执行构建、测试或集群验收。

## 选 Gate 的权威入口

客户端通过 Java gateway 的 HTTP `POST /api/assign-gate` 申请入口，gateway 做限流与 zone
准入后转给 go-zero login 的 `LoginPreGate.AssignGate`。gateway 不选 Gate、不签票；login（assign-gate 入口）与
scene_manager（跨 zone 重定向）各自按负载选 Gate 并签发，挑选规则共用 `go/shared/gatepick`（见下文「gate 目录怎么读、怎么挑」）。

```text
客户端 → HTTP /api/assign-gate → Java gateway → gRPC LoginPreGate.AssignGate
login → 按 zone、可用性与负载选 Gate → 签 GateTokenPayload
客户端 ← gateway ← Gate 地址、payload、signature、deadline（或排队状态）
客户端 → 选中的 Gate：TCP connect → ClientTokenVerifyRequest
Gate → 本地校验签名、目标 Gate 与到期时间 → 允许后续游戏请求
```

选与签有三条路径：

1. 不排队的快路径用 `PickAndSignGateToken`，选取和签发一次完成。
2. 排队 dispatcher 准入时只挑 Gate（批内 `gatePicker`，经 `newBatchPickFn`，不走 `PickGate`）、不签；
   客户端通过 `/api/queue-status` 消费 admit 时才签，TTL 从消费时起算。
3. 携 queue token 重入 `AssignGate` 的 `handleReentry` 同样在消费时签发。

2、3 两个消费入口用同一个签票函数 `loginqueue.NewAdmitSigner`（2026-11-05 起）：签之前复核放行时选定的 Gate，它仍在分档选出的
那一档里就沿用，已停写、排空、满载或不在此刻的候选里就重挑（见下文「满载与停写的 gate」）。此前两处各写一个闭包，照签已选 Gate。

代码：`go/login/internal/logic/loginpregate/assigngatelogic.go`、`querystatuslogic.go`、
`go/login/internal/logic/pkg/loginqueue/gatetoken.go`、`admit_recheck.go`。

### gate 目录怎么读、怎么挑（2026-10-18 起；已落码，未编译、未测试，待 Codex 验证）

**读目录**：login 的 `node.NodeWatcher` 对本 zone 的 gate 前缀做 list-watch，维护一份进程内镜像
（`go/login/internal/logic/pkg/node/node_cache.go`、`watcher.go`）。AssignGate 快路径与排队 dispatcher
经 `CandidatesForZone` → `NodeWatcher.Nodes(ctx)` 读镜像，不再每个请求打一次 etcd 前缀 Range。

- **新鲜度按每台 gate 各自计时**：镜像记下每台 gate 最近一次被刷新（收到它的事件或全量同步）的时刻，整体新鲜度
  取最久没刷新的那台；镜像为空时取最近一次事件或同步的时刻。不用整个前缀共用一个时间戳：多 gate 时别的 gate 的事件
  不能替一台没刷新的 gate 作证。
- 镜像还没完成第一次全量同步，或有 gate 超过 `cacheStaleAfter`（15s）没刷新，`Nodes` 回落直读 etcd：一次线性一致
  Range，超时取请求 ctx 截止时间与 `ServiceDiscoveryTimeout` 中较早者。回落路径的行为与改动前相同。
- 重新全量同步的时机：watch 出错、被压缩、丢 leader（`WithRequireLeader`）；建流超过 `cacheResyncAfter`（12s）
  仍未确认；有 gate 超过 12s 没刷新（gate 每 10s 无条件上报一次，健康时不会触发；某台漏报或还没开始上报时
  每约 12s 多一次 Range）。空 zone 按同样间隔周期重同步，镜像保持新鲜。每轮 watch 用独立的 `clientv3.Watcher`、
  结束时 Close，卡在半开连接上的流随之拆掉；login 的 etcd 客户端另开 gRPC keepalive（10s 探、5s 超时）。
- 两个时限都按 gate 上报周期 10s 推（`cpp/nodes/gate/main.cpp` 的 `playerCountReportTimer`），改上报周期
  必须同步改 `gateReportPeriod`。按每台计时，镜像里任一台 gate 的数据最多比 etcd 落后约 5s（它错过的那次上报
  之后最多 5s 就被重同步补上或判过期），人数最坏晚约 5s 出现，仍在排队在途占位宽限 `defaultHandoffGrace`（20s）之内。
- **gate 排空监视不读镜像**，仍用直读的 `FetchAllNodes`：写 drained 会让脚本删 Pod，读不到 etcd 就本轮不判，
  不能拿可能过期的镜像判定。
- **排空脚本的配合**：`tools/scripts/k8s_gate_drain.ps1` 确认旧记录在 etcd 里消失后，再等契约
  `LoginMirrorSettleSeconds`（20s，必须大于 `cacheStaleAfter`，契约测试钉住）才清排空标记：watch 卡住时 login
  镜像可能还留着旧记录，标记清早了会把玩家分到已删 Pod 的地址上。
- 指标 `login_gate_directory_reads_total{source}`，source 取 `cache` / `etcd_unsynced` / `etcd_stale`。
  稳态应几乎全是 cache；`etcd_stale` 持续不为 0 说明 etcd 不可达或 watch 反复卡住，登录又回到逐请求打 etcd。

**挑 gate**（两条规则；随机二选一的实现只有一份，在 `go/shared/gatepick` 的 `TwoChoices`，login 快路径与
scene_manager 跨 zone 重定向共用；Java 版 `game-discovery` 的 `GatePicker.pick` 同规则）：

1. 快路径 `PickGate`：在候选里随机抽两台不同的 gate，取人数少的那台，同人数取第一次抽中的；只有一台就是它，
   恰好两台时等于取最少。同人数不按 NodeID 定胜负：开新区第一个上报窗口里所有 gate 都报 0，按 NodeID 定会让
   小号 gate 拿双倍份额、NodeID 最大的那台一个人都分不到。
   原来是确定性取全局最少：gate 人数每 10s 才上报一次，快路径各请求互相看不到别人刚分出去的人，一个上报
   周期里本 zone 的新连接会全部落到同一台（容量模型 §2.1「PickGate 集中度」）。随机二选一下，滞后窗口里
   最闲那台最多拿 2/n 的新连接（3 台 67%，10 台 20%）；最忙的那台只有和同样忙的 gate 比时才会被选中。
   **当前 K8s 默认每 zone 2 台 gate**（`k8s_deploy.ps1` 的 `GateReplicas = 2`、`deploy/k8s/zones.json`）：两台人数不等时
   与旧规则相同；全体同人数（开新区第一个上报窗口）时从全落 NodeID 最小的一台变为两台各半；之后的窗口里羊群依旧，
   充分摊开要每 zone 至少 3 台。验证这一项必须按至少 3 台部署，并按 gate 统计快路径下发分布。
2. 排队 dispatcher 一拍放一批，用确定性的 `gatePicker`：在同一份候选上逐条挑并记账（负载 = 上报人数 +
   本拍已分给它的人数，同负载 NodeID 小的在前），先填最闲的，填平之后轮转。账不跨拍。

**scene_manager 跨 zone 重定向**（`go/scene_manager/internal/logic/gate_redirect.go`，2026-10-18 起；已落码，未编译、未测试）：
顺序与 login 的 `CandidatesForZone` + `PickGate` 一致——选客户端地址并按地址去重 → 按排空、停写与满载分档（2026-11-05 起；
11-06 起排空也并入 `gatepick` 的同一次分档，见下节）→ 在选出的那一档里随机二选一。此前是 `sort.Slice` 确定性取最少（同数不稳定）且不过滤排空。排空标记由 `drainingGateIDs` 一次 MGET 读，
读失败放行；键名 `gate:%d:draining` 在 scene_manager 里抄了一份，由 `tools/scripts/tests/k8s_gate_drain.tests.ps1`
钉住与 login、排空脚本三处一致。scene_manager 经**自己的** Redis 连接读标记，今天与 login 的 `Node.RedisClient` 是同一实例；
**拆 Redis（G4a）把排空标记挪到 redis-queue 时，scene_manager 这条读也要一起指过去**（G4a 修订稿需把 scene_manager 列为
`gate:{id}:draining` 的读者；2026-10-19 注：已在 `shared-redis-split-by-purpose.md` §2.2 A 表、§3.2.1 约束 1 与 §20.5 登记），否则读不到标记、退回不过滤。另一个隐含前提：go-zero 的 `RedisConf` 没有 DB 字段，scene_manager
只能读 DB 0；login 的 `Node.RedisClient.DB` 改成非 0 时要同步处理。两边读标记的客户端不同（login 用 go-redis，经
`loginqueue.DrainMarkCache` 按 zone 缓存 1 秒；scene_manager 用 go-zero，每次重定向一次 MGET），各读各的；怎么用标记只有
`gatepick.Eligible` 一份（2026-11-06 起；此前 login 的 `FilterDrainingGates` 与 scene_manager 的 `filterDrainingTargets` 各写了一份
先行剔除，见下节「为什么一次分档」）。

### 满载与停写的 gate（2026-11-05 起；11-06 排空并入分档；已落码，未编译、未测试，待 Codex 验证）

挑 gate 之前先给候选分档，不该再接新玩家的 gate 只在没有更好的选择时才用。规则只有一份，在 `go/shared/gatepick`
（`eligibility.go` 的 `Eligible`、`Verdict`、`Full`、`StaleAfter`、`ReportPeriod`），四处共用：login 快路径 `PickGate`、dispatcher 的
`newBatchPickFn`、排队取走复核（三处都经 `loginqueue.eligibleGates` 适配，`go/login/internal/logic/pkg/loginqueue/gate_eligibility.go`），
以及 scene_manager 跨 zone 重定向（`eligibleTargets`，见下文）。各处只是三种状态怎么读到不同：停写 login 读 watch 镜像、scene_manager
每次直读 etcd；排空两边各读 Redis 标记；满载都看 NodeInfo 自报的上限。

**分档**（`gatepick.Eligible` 返回最高的非空一档，调用方在这一档里随机二选一）：

1. 完全合格：没停写、没排空、没到水位；
2. 连得上的：在排空或到水位，但没停写。没有完全合格的才用（`Fallback = FallbackReachable`）；
3. 全部：全部停写时才用（`FallbackAll`）。

一台 gate 几条同时成立时按停写、排空、满载的先后归类（`Verdict`）：停写的多半连不上，排空中与到水位的连得上。

**为什么一次分档**（2026-11-06 起；此前排空在 `candidatesFromNodes` / `filterDrainingTargets` 先剔，停写与满载再剔，两层各自在
全部不合格时回落）：

- 分层回落会退到更差的 gate 上：「没排空的全部停写、只有排空中的健康」时，排空那层已经把健康的剔掉，停写那层只能在卡住的
  gate 里回落，票签给一台多半连不上的 gate。跨 zone 传送时票签出去源实体就销毁了，连不上只能断线重登。
- login 先剔排空，候选集里就没有排空 gate 上的在线人数：`FreeSlots` 少算在线，排空期间区级准入多放人，与「只影响挑哪台、不影响
  容量记账」相矛盾。现在 `candidatesFromNodes` 只给候选打 `GateCandidate.Draining` 标记（`loginqueue.MarkDrainingGates`），不剔除。
- 规则收一处：两份先行剔除（login、scene_manager 各一份）合并进 `gatepick`，四处挑法同一口径。

- **停写**：gate 的事件循环卡住时，每 10s 的上报定时器不走了，etcd 租约却可能还在续，记录要等租约到期（NodeTTL，默认 180s）
  才消失，这期间它一直在候选里吸人（容量模型 A0-7 的「黑洞」）。镜像给每台 gate 记 etcd 的 ModRevision 和本进程第一次
  看到这个 ModRevision 的时刻（`node_cache.go` 的 `changedAt`）。gate 每次上报都是一次新的 Put，值不变 ModRevision 也前进；
  超过 `nodeDataStaleAfter`（即 `gatepick.StaleAfter`，三个上报周期，30s）没变，就在 `NodeWatcher.Nodes` 的结果里标 `WatchedNode.Stale`。
  - 这条与镜像新鲜度（`cacheStaleAfter`）分开判。镜像新鲜度问「镜像还跟得上 etcd 吗」，按刷新时刻算，一次全量同步就重置；
    停写问「这台 gate 还在写吗」，按 ModRevision 变没变算，全量同步拿到同一个 ModRevision 时不重置。用刷新时刻判停写，
    镜像每约 12s 一次的重同步会让卡死的 gate 永远显得在写；用停写判镜像过期，一台卡死的 gate 就会把所有读路径推回直读 etcd。
  - 阈值的下界：镜像可读期间，健康 gate 的停写时长至多约「一个上报周期 + `cacheStaleAfter`」= 25s，30s 比它大，单测钉住。
  - 回落直读 etcd 时没有历史可比，判不了停写，`Stale` 一律为 false，与改动前相同。login 刚起服时所有 gate 都按刚写过算，
    卡死的 gate 要在起服 30s 后才被剔除。
- **满载**：gate 启动时把 `GateMaxConnections` 写进自己的 NodeInfo（新字段 `max_connections = 13`，`cpp/nodes/gate/main.cpp`
  的初始化回调），随注册与每 10s 的上报进 etcd。人数到上限的 90%（`PlayerCount*100 >= MaxConnections*90`，整数算）就不再接新玩家。
  10% 余量给人数上报滞后：login 看到的人数最多晚约 15s。余量不保证不超，这 15s 里分给它的人超过上限的 10% 时仍会顶满，
  由 gate 按 `GateMaxConnections` 拒连兜底。`max_connections` 为 0（dev 下不设上限、旧版 gate）不按上限判。
  没有用「只在较低一半里二选一」：它在 n≤4 时等于回到确定性取最少。
- **排空**：运维缩容前用 `tools/scripts/k8s_gate_drain.ps1` 打 `gate:<id>:draining` 标记（带 TTL）。排空中的 gate 照常服务已在线的
  玩家，只是不再分新玩家；它连得上，所以排在停写的 gate 前面。login 的标记经 `DrainMarkCache` 按 zone 缓存 1 秒、查询失败放行；
  scene_manager 每次重定向一次 MGET、失败放行。排空脚本标记前要求同 zone 还有没排空的候选：全部排空时分档退到「连得上的」照常分配，
  标了挡不住新玩家（脚本侧的说明见 `Select-GateDrainPeers`）。
- **总会给出非空的一档**，不拒绝：全部停写时要么本 zone 的 gate 全卡住了（分过去照样失败，与没有这层判定时一样），要么是本 login 的
  视图有问题，一律拒绝会把一次可能的误判放大成整区登不上；连得上的全在排空多半是整区误标，拒绝比分到待缩容的 gate 更糟；
  全部满载说明有人没经过区级准入就来了：2026-11-10 起排队的区容量按在接客的 gate 到水位的承载封顶（`gatepick.AdmissionCapacity`，
  见 [login-queue-2026-05.md](./login-queue-2026-05.md)「FreeSlots 公式」），排队开着时只剩重连豁免与人数上报滞后，排队关着时快速通道
  不判容量，gate 拒连兜底。此前这里写的「ZoneCapacity 配得比 gate 上限之和还大」在封顶之后不再会让排队多放人。
- **只影响挑哪台，不影响容量记账**：`FreeSlots` 仍按全部候选的人数算 online，卡住、排空中或满载的 gate 上的人照样占着名额
  （区容量本身按在接客那一档 gate 的水位承载封顶，见上一条，2026-11-10 起）。
  `buildGateCandidates` 只把 `Stale` 与 `MaxConnections` 透传进 `GateCandidate`，`candidatesFromNodes` 再打 `Draining` 标记，都不过滤。
  （2026-11-06 之前排空中的 gate 在这里就被剔掉，`FreeSlots` 数不到它上面的人。）
- **观测**：`login_gate_pick_excluded_total{reason}`（stale / draining / full，每次挑每台被剔掉的 gate 计一次；回落时被用上的那一档
  不计）、`login_gate_pick_fallback_total{reason}`（stale = 退到全部；draining / full / mixed = 退到连得上的，那些 gate 全在排空 /
  全到水位 / 两种都有）；被剔除的停写 gate 与回落各有一条 ERROR 日志，各自 30s 至多一条，停写那条带 node_id。
  `excluded_total{reason="stale"}` 持续增长说明有 gate 卡住，按日志里的 node_id 去查那台 gate。
- **告警**（2026-11-05；11-06 随分档调整；`deploy/k8s/login-alerts.yaml` 组 `login.gate-pick`，规则未编译、未实测）：
  `LoginGatePickFallbackStale`（reason 为 stale，critical：本 zone 一台连得上的 gate 都不剩，gate 全卡住或全写不进 etcd）、
  `LoginGatePickFallbackDegraded`（reason 为 draining / full / mixed，warning：没有完全合格的 gate，整区误标排空，或有人没经过区级准入：
  排队关着、重连豁免、人数上报滞后；11-06 前叫 `LoginGatePickFallbackFull`、只管 full）。gate 台数装不下配置的区容量另由
  `LoginQueueCapacityLimitedByGates`（组 `login.queue-capacity`，2026-11-10 起）报。健康时恒 0，任一次即告警；go-zero 计数不预置 0 值序列，
  规则带 `unless … offset 10m` 的新序列分支。按 `namespace` 聚合（一个 zone 一个 namespace）。
- 测试：`go/shared/gatepick` 的 `eligibility_test.go`（水位整数边界、归类先后、只留完全合格、排空中健康胜过停写、各档回落原因与剔除计数、
  三个周期）、`report_period_crosslang_test.go`（逐字核对 C++ gate 每 10s 无条件重写 etcd 记录，停写判定的前提）、`node_cache_test.go`
  （停写判定、阈值边界、未知 ModRevision、阈值契约）、`watcher_test.go`（全量同步与 watch 事件都把 ModRevision 带进镜像、直读回落
  不判停写）、`gate_eligibility_test.go`（适配、各档、回落计数、两条挑法）、`gatedrain_test.go`（只打标记不剔除、整区排空照样挑、
  排空中健康胜过停写）、`candidates_client_endpoint_test.go`（透传、先去重后打标记）。

**scene_manager 跨 zone 重定向**（2026-11-05 起，`go/scene_manager/internal/logic/gate_redirect.go`）：按地址去重之后、随机二选一之前过
`eligibleTargets`，分档同上（`gatepick.Eligible`；排空标记 `gateMarks.draining` 与停写判定 `gateMarks.stale` 都交给它，11-06 起不再先单独剔排空）。

- **满载**：直接读 etcd 里 NodeInfo 的 `max_connections` 与 `player_count`，与 login 同一水位。
- **停写**：跨 zone 重定向每次直读 etcd 前缀，没有 watch 镜像，也不为它加 watch（全服 watch 正是控制面按 zone 化要消除的）。改为
  在本进程记历史：`fetchGateNodes` 每条记录带上 etcd 键与 ModRevision，交给 `svcCtx.RedirectGateRevisions`
  （`gatepick.RevisionTracker`，零值可用、并发安全）；同一个 ModRevision 从本进程第一次看到起超过 `StaleAfter`（30s）就判停写。
  - 第一次看到的时刻不早于那次写入，算出的停写时长只会偏短：读到同一个 ModRevision 说明这次读的线性化点还在下一次写之前，
    读返回又至多比线性化点晚一次直读超时（`gateFetchTimeout`，5s），健康 gate 的停写时长至多约 15s，比 30s 小一倍，单测钉住。
  - 只靠本进程历史发现得晚：进程刚起、某个 zone 第一次被重定向时判不出；读得稀疏时最早在第一次看到之后、隔超过 30s 的那次重定向
    才判出来；多副本还各自从头学。所以 2026-11-10 起再读一份目标 zone 的 login 发布的判定（下一条），两个来源任一成立即算停写。
  - 每次读用全量结果替换该 zone 的记录，记录删了历史随之丢掉，内存与 gate 数同阶；并发的读乱序到达时，旧结果不判停写、不冲掉新版本。
  - 多副本 scene_manager 各记各的历史，互不依赖。
- **停写标记**（2026-11-10 起；已落码，未编译、未测试）：由目标 zone 里本来就在持续观察本 zone gate 的一方发布判定，做法同 Kubernetes
  节点控制器：观察者用自己的单调时钟判，消费者只读结果。
  - **写**：login 的 `NodeWatcher` 对本 zone 的 gate 做 list-watch、按 ModRevision 判停写（`WatchedNode.Stale`）。各 login 副本每个
    上报周期（`gatepick.ReportPeriod`）读一次镜像，给判为停写的 gate 写 `gatepick.StaleKey`（`gate:<node_id>:stale`，值 `"1"`，
    过期 `gatepick.StaleMarkTTL` = 30s），不主动删：恢复后不再续写，30s 内自然过期。写在 login 的 `Node.RedisClient`（核心实例，
    与排空标记同一处）。代码：`go/login/internal/logic/pkg/loginqueue/gatestale.go`（`PublishStaleMarks`）、
    `go/login/internal/svc/gate_stale_publisher.go`（循环与接线，`Start` 起、`CloseSource` 停）。正常时一台都不停写，零写入；
    镜像不新鲜、`Nodes` 回落直读 etcd 的那一拍 `Stale` 恒为 false，什么都不写，等于放行。
  - **读**：scene_manager 读排空标记的那次 MGET 一并读出它（`gateRedisMarks`，经 `CoreRedis`），与本进程历史取并集（`unionMarks`）；
    读失败两种标记都当没有，照旧放行。部署前提与排空标记相同：scene-manager ConfigMap 的 `CoreRedis` 必须指向 login 的 `Node.RedisClient`。
  - **键名只定义一份**：`go/shared/gatepick/marks.go` 的 `DrainingKeyFmt` / `StaleKeyFmt`；login 的 `GateDrainingKeyFmt` 是它的别名，
    scene_manager 不再自己抄一份排空键名。`tools/scripts/tests/k8s_gate_drain.tests.ps1` 钉住它与排空脚本的字面量一致、两边都不另写字面量。
  - **不用的两种**：给 scene_manager 加 watch——跨区要看的是别的 zone 的 gate，按 zone 收窄的 watch（缺口清单第 4 条）看不到，
    全服 watch 正是第 4 条要消除的；gate 在 NodeInfo 里自报心跳时间——要拿 gate 的墙钟和 scene_manager 的墙钟比，时钟偏差没有可靠上界
    （AGENTS §11.3）。
  - **边界**：多个 login 副本判断不一时，只要有一个写了标记就会剔除；误判上界仍是健康 gate 至多约 25s（小于 30s）。目标 zone 的 login
    全挂时没有标记，回到只靠本进程历史，不比改动前差。恢复的 gate 最多晚 30s 重新参与跨区重定向（login 自己挑 gate 读的是镜像，不受影响）。
- **观测**：`scene_manager_gate_redirect_excluded_total{zone_id, reason}`（stale / draining / full，每次重定向每台被剔掉的 gate 计一次，
  回落时被用上的不计）、`scene_manager_gate_redirect_fallback_total{zone_id, reason}`（stale / draining / full / mixed，含义同 login）；
  停写剔除与回落各一条 ERROR，各 30s 至多一条，停写那条带 node_id。告警 `SceneManagerGateRedirectFallback`
  （`deploy/k8s/scene-manager-alerts.yaml` 组 `scene-manager.gate-redirect`，warning，写法同 login 那两条；目标 zone 的 gate 全卡住时由
  那个 zone 的 `LoginGatePickFallbackStale` 呼人，其余原因对应 `LoginGatePickFallbackDegraded`，这条不重复呼）。
- 测试：`gate_redirect_eligibility_test.go`（剔除、排空中健康胜过停写、退到连得上的、全部停写退到全部、跨两次读判停写、时限契约）、
  `gate_redirect_client_endpoint_test.go`（整区排空照样重定向、先去重后分档、解析带上键与 ModRevision）、
  `gate_redirect_draining_test.go`（两种标记按 node_id 读、互不串位、读失败放行、退化输入、并集、第一次重定向靠发布的标记避开卡住的 gate）、
  `go/shared/gatepick` 的 `revisions_test.go`（阈值从第一次看到起算、健康 gate 不误判、新写重新起算、乱序旧结果、未知 ModRevision、按 zone 替换、并发）
  与 `marks_test.go`（键名金样、过期时间盖住两次漏写）；login 的 `loginqueue/gatestale_test.go`（每台一条、带过期、续写与自然过期、
  空输入、Redis 失败）与 `svc/gate_stale_publisher_test.go`（只写判为停写的、读镜像失败不写、都健康不写）。
- **观测（停写标记）**：`login_gate_pick_stale_marks_total{outcome}`（`written` 按台计、`error` 按次计）；发布失败一条 ERROR
  `[GateStale] publishing stale gate marks failed`，30s 至多一条。健康时恒 0；`written` 持续增长说明本 zone 有 gate 卡住，与
  `login_gate_pick_excluded_total{reason="stale"}` 同步出现。

**排队取走时复核放行时选定的 gate**（2026-11-05 起，`go/login/internal/logic/pkg/loginqueue/admit_recheck.go`）：
dispatcher 放行时只挑 gate、不签票，放行槽在 Redis 里最多待 `AdmitTTL`。这段时间里选定的 gate 可能卡住、被标了排空、被摘掉、
在滚动升级里换了 node_id（旧 node_id 的票会被新 gate 以 `token_gate_node_mismatch` 拒绝），或者被填满；照签它，玩家连不上
（或刚登录就随排空改派），放行槽已经取走，只能从 `/assign-gate` 重新排队。

- 两个消费入口用 `NewAdmitSigner` 现签：先 `CandidatesForZone(放行槽里的 zone)` 取此刻的候选（与挑 gate 同一份视图：按地址去重、
  排空中的带标记），再过 `eligibleGates`。选定的 gate（按 node_id 认，票绑的就是它）仍在选出的那一档里就签它，地址取此刻候选里的那份；
  否则按快路径的规则（随机二选一）在那一档里重挑。没有完全合格的 gate 时 `eligibleGates` 退到连得上的（或全部），选定的 gate
  只要在退到的那一档里就留着它，不为了换而换。
- 重挑只换 gate，不动名额：在途占位按 zone 记，与签给哪台无关。
- **读不到此刻的候选**（读出错、为空）时照签放行槽里的 gate，与改动前相同，复核本身不产生错误：放行槽在签票之前已经 GETDEL，
  这一步出错会让已放行的玩家白白失去名额，所以它只能改善、不能多失败一次（fail-open，计 `unchecked`）。
- 代价：每次取走多一次 `CandidatesForZone`（读镜像；镜像不新鲜时直读 etcd；排空标记 2026-11-05 起按 zone 缓存 1 秒，见 `loginqueue.DrainMarkCache`，每个 login 实例每个 zone 每秒至多一次 MGET），与快路径每次 AssignGate 的读同量级。
- **观测**：`login_queue_admit_gate_recheck_total{outcome}`，outcome 取 `kept` / `gone`（不在候选里）/ `stale` / `draining` / `full` / `unchecked`；
  重挑与 `unchecked` 各一条节流日志（30s 至多一条）。复核经 `eligibleGates`，`login_gate_pick_excluded_total` 与 `fallback_total`
  也按每次取走计入。`unchecked` 持续不为 0 说明取走请求打到了看不见本 zone gate 的 login，或 etcd 不可达。
- 测试：`admit_recheck_test.go`（保留、四种重挑、换 node_id、整区排空与「排空中胜过停写」时保留、全部停写时保留、签出的票绑新 gate、
  读不到候选照签、经 `Queue.Poll` 的端到端）。

- Java 版（`game-discovery` 的 `GatePicker`）三项都未做，记在 Java 仓 `PARITY.md` 待做；做的时候按同一分档（排空并入，不先行剔除）。

**还没做**（容量模型 A0-7 的其余部分与评审登记）：

- 跨请求的本地分配累加：快路径只靠随机二选一摊开，不记「本周期已分给某台多少人」；
- dispatcher 跨拍仍从同一份滞后人数起算，同一个上报周期内连续几拍都会先填同一台最闲的 gate；
- 三条回落告警（2026-11-05 加，11-06 随分档改了 reason 口径并把 `LoginGatePickFallbackFull` 改名 `LoginGatePickFallbackDegraded`）
  还没过 promtool、没部署到任何 Prometheus，阈值与 severity 没有演练依据；
  `login_queue_admit_gate_recheck_total{outcome="unchecked"}` 与停写剔除的持续增长没有告警，只进看板。

## 有状态连接与跨 Gate 重连

`SessionInfo`、socket、节点绑定与当前 session 属于承载该 TCP 连接的 Gate，Kafka 下行按目标
Gate 定向；不能把同一条已建立连接的包随意送给另一实例。

**重新连接可以选择另一 Gate。** 新连接先取得匹配目标 Gate 的票据，再握手和 EnterGame；
login 调 `player_locator.Reconnect` 更新 Gate/session，BindSession 与 RoutePlayer 在新 Gate
建立上下文。不能沿用旧 session 就直接向任意 Gate 发游戏包；也不能据“Gate 有状态”推导
“重连必须回原 Gate”。流程见 [player_login_flow.md](player_login_flow.md)。

| 场景 | 当前约束 |
|---|---|
| 已建立的 TCP 连接 | 保持在持有它的 Gate，由该 Gate 感知断线 |
| 相同 Gate 重连 | 仍经过新连接、票据与登录会话更新 |
| 不同 Gate 重连 | 支持；由新连接与 BindSession / RoutePlayer 重建上下文 |
| 跨 zone 换 Gate | 目标 zone 重新入场；同时遵守 owner_epoch / handoff 换手门 |
| 已签给 Gate-A 的票据被 LB 送到 Gate-B | node_id 不匹配，Gate 拒绝 |

## L4 与集群入口

本仓采用“login 分配实例地址 + HMAC 票据”。票据绑定选中的 Gate，因此不能在这个地址后放一个
会把新 TCP 连接随机分给所有 Gate 的共享入口。限制来自票据目标和定向路由，不是所有有状态
网关都天然不能使用 L4，也不是跨 Gate 重连缺失。

K8s 集群外使用 Gate StatefulSet 与每序号 Service `gate-<i>`，保持每实例可寻址；单一
`gate-entry` Service 仅用于 podip 单副本形态。见
[k8s-client-entry.md](k8s-client-entry.md) D87–D90 与
[k8s_gate_exposure_guidance.md](k8s_gate_exposure_guidance.md)。

## Token Details

- payload 至少包含 `gate_node_id`、`zone_id`、`expire_timestamp`、随机 `hmac_session_key`；
  跨区重定向另带玩家和目标 zone 绑定字段，完整字段以 `proto/common/base/message.proto` 为准。
- 签名为 `HMAC-SHA256(secret, serialized_payload)`，按小写 hex 传输。签发使用随机会话 HMAC key，
  但这不等于业务消息逐包 HMAC 已启用，见 [hmac-message-signing.md](hmac-message-signing.md)。
- 普通 AssignGate 票据当前 TTL 为 **10 分钟**（`assigngatelogic.go::gateTokenTTL`），从实际签发
  开始计；不是本页旧版的 60 秒。其他用途的凭证以各自签发点为准。
- login 新配置使用 `Secrets.GateToken`，旧 `GateTokenSecret` 只作兼容回退；Gate 读取
  `BaseDeployConfig.gate_token_secret`。两端必须使用匹配的签名/验签密钥，不能从 Go 的 keyring
  能力推导 C++ Gate 已支持同样的多密钥验签或无缝轮换。密钥值不写入跟踪文档或日志。
- 票据用于短期 TCP 握手；Gate 本地验签不等于全局一次性消费，不能仅凭该流程宣称票据不能重放。

## Gate Enforcement

- `SessionInfo.verified` 默认 false；token 校验通过后置 true，未验证会话不能发送游戏请求。
- `HandleClientTokenVerify` 使用常数时间 HMAC 比较，检查目标 Gate 和到期时间；重定向票据的
  `target_zone_id` 非零时还检查本 zone。玩家/目标绑定继续交给 login 入场链验证。
- **空密钥不默认放行**：`GATE_RUN_MODE` 未设置按 prod；prod 缺密钥拒绝启动，并在连接/握手
  入口再次拒绝。只有显式 dev/test 模式允许空密钥降级，且记录警告。
- 校验实现与启动门禁分别在 `cpp/nodes/gate/handler/rpc/client_message_processor.cpp` 和
  `cpp/nodes/gate/main.cpp`，运行模式策略在 `cpp/nodes/gate/gate_security.h`。

## 客户端地址选择

login 从 etcd NodeInfo 生成候选：按 zone 过滤，优先选择可用的 `client_endpoint`；缺失且
`RequireClientEndpoint=false` 才回落 `endpoint`，为 true 则跳过。随后按地址去重，只留最新
注册，并排除正在 drain 的 Gate。实现为 `buildGateCandidates` 与 `clientendpoint.Select`。

SceneManager 的跨 zone `RedirectToGate` 复用相同客户端地址规则。部署参数
`RequireClientEndpoint=auto` 随入口形态决定，集群外要求显式客户端地址。

Java `GateWatcher` 用于 zone 健康查询，并提供 login 发现；不负责 Gate 选择或票据签发。

## 验证边界

本页修订只对照源码、配置与现有进度记录，没有连接 Redis、启动节点或执行 kind 验收。后续
应分别验证目标 Gate 不符拒绝、prod 缺密钥拒绝、排队兑现后票据有效期、跨 Gate 重连，以及
每实例外部地址与 drain 后重分配，不能以文档已更新替代运行证据。
