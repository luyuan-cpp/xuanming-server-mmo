# C 批（gate 推送身份栅栏）与 battle 观众席（A / B / P0）语义评审

只读评审，未编译、未运行；基线 `720179638`，看的是工作区当前状态（含已叠加的 P1 改动）。Java 版未核。

结论：发现 5 条，严重 2、一般 3，没有阻断。最担心的「新判据把 Kick / Bind / Route 全拒掉」不成立。
严重-1 是本批引入的回归，上线前要修。严重-2 是 HEAD 上就有的缺口，不是本批引入，但属于「与 match 恢复循环对齐」的范围。

## 发现

### 严重-1：Bind 之前失败的进场 tip（3023）被新栅栏丢弃，客户端退回等 60 秒

- **位置**：
  - 生产者：`go/login/internal/logic/clientplayerlogin/entergamelogic.go:313-317`（预加载失败）、`:697-736`（Bind 之前的 apply 失败）、`:463-472`（推 tip）、`:742`（Bind 在这之后才发）；`go/login/internal/svc/gate_command.go:186-188`（tip 带真实 player_id）。
  - gate：`cpp/nodes/gate/handler/gate_push_delivery.cpp:202-207`；`cpp/libs/services/gate/session/system/session_identity_fence.h:47-58`。
  - 会话初值：`cpp/libs/services/gate/session/comp/session_info_comp.h:44` 是 `kInvalidGuid`；写入点只有 `cpp/nodes/gate/handler/event/scene_route_helper.h:302`（Bind）、`:326`（Route）。
- **触发序列**：
  1. 客户端连上 gate，会话 S 的 playerId 为 `kInvalidGuid`。EnterGame 同步回「已受理」。
  2. login 异步链预加载失败（或 GetSession / 铸代次 / 会话落盘 / bindSessionFence 失败），走不到 `:742`，Bind 没发。
  3. `notifyEnterGameFailed` 发出 `PushToPlayerEvent{session=S, player_id=P, tip=3023}`。这是本批 `gate_command.go:186-188` 新填的 player_id。
  4. gate 走 `DeliverToSession(kKafka, S, P)`，`ClassifyPush(kInvalidGuid, P)` 判 kDrop，tip 被丢。

  本批之前 Kafka 入口没有栅栏，tip 能送达。同连接换角色（LeaveGame 后选 Q）同样中招：`scene_route_helper.h:139` 保留旧 playerId，`ClassifyPush(P', Q)` 也是 kDrop。Bind 之后的失败不受影响，tip 与 Bind 同分区且排在后面。
- **后果**：
  - 破坏 `docs/design/cross-zone-scene-travel.md:381` 的契约：所有显式失败都推 3023，客户端（`:394`）立即收口。现在 Bind 之前的失败要等客户端 60 秒超时，且看不到原因。
  - `entergamelogic.go:349-364` 的注释记录，历史压测里预加载失败率约 2.7%~4%，所以这不是罕见路径。
  - login 计 `notifyOutcomeSent`（`:478`），指标显示已发送。
  - gate 打 `metric=gate_push_dropped reason=fence`，这条按注释是「真正要报警的那一条」，会造成假告警。
  - `reports/cpp-gate.md:26` 的冒烟判据是「不应出现」这条日志，但冒烟走不到这条失败路径，发现不了。
- **最小修法**：
  - login 记住本链是否已发 Bind。Bind 之前的失败走单独的 builder，`PushToPlayerEvent.player_id` 留 0（gate 判 `kDeliverUnfenced`），信封 `PlayerId` 照填；Bind 之后仍带真实 player_id。
  - 风险可接受：`target_instance_id` 已挡住 node_id 复用；剩下的 session 序号回绕在链预算内基本不会发生；内容是固定 tip，不含私有数据。
  - 这是「生产者拒发 0」的唯一例外，要写进 `gate_command.go` 注释和 `session_identity_fence.h:17-20`（那里计划删 `kDeliverUnfenced`）。
  - 补两条回归：login 用例断言 Bind 前后 player_id 的差别；gate 用例断言未绑定会话收 player_id=0 的单播能送达、收真实 player_id 被丢。

### 严重-2：battle 实例换掉后，全局模式的观战原件永远关不掉，玩家排不了队（HEAD 已有）

- **位置**：
  - battle：`cpp/nodes/battle/logic/battle_room_manager.cpp:1018-1020`、`:1228-1231`（目标实例不符回空应答）。
  - match：`go/match/internal/logic/global_spectator.go:527-534`、`:448-460`（只认本实例的正证明）、`:461`（关到 5 的唯一入口）、`:657-658`。
  - 阻塞点：`go/match/internal/authority/spectator.go:681-692` `RequireNoSpectators`；调用方 `store.go:288,522,649`、`team_launch.go:225`、`team_claim.go:54`、`direct_admission.go:265`、`team_spectator.go:30`；入队 `go/match/internal/logic/global_match.go:72-76`。
- **触发序列**：
  1. 玩家 O 在实例 I1 上观战，`match_spectator` 为 state 2。
  2. I1 崩溃或滚动发布。滚动发布同样走不通：v1 房间停机时观众不出房（`battle_room_manager.cpp:2329-2336`），进程退出前 Query 一直回 ACTIVE，原件关不掉，退出后同样进入第 3 步。
  3. 恢复循环 QueryObserver：旧地址 RPC 报错，或新实例 I2 回空应答，都得到 `errGlobalSpectatePending`，每轮再 `HoldRelay`。
  4. O 发 JoinQueue：`PrepareJoin` 把原件转到 state 3，`closeCapturedSpectatorExit` 发 RemoveObserver，同样拿不到证明，回 `原观战退出尚待确认`。WatchBattle 回 16004，组队、切磋回 ErrSpectatorBusy。
- **后果**：
  - 没有 TTL，也没有别的关闭路径。Redis 路径有 TTL 兜底（`go/match/internal/logic/spectate_test.go:570`），全局路径没有，而生产强制全局路径。
  - 崩溃时在册的全部 L0 观众（单节点配额可达 1024）都排不了队、看不了战，要人工改 SQL 才能恢复。
  - 恢复循环对这些行反复失败，`HoldRelay` 累加 unknown，`go/shared/drainproof/report.go:31` 注释写明「未知结局永久保留到人工处理」，维护排空也被拖住。
- **最小修法**：
  - 在 `cancelGlobalSpectator` 与 state 2 分支拿不到证明时，读成局记录：`match_formation.state == 5`（`completion.go:488`，权威截止后必达）就直接把原件推进到 5。
  - 观众零写权（`battle_room_manager.h:397-399`），不带证明关闭碰不到资产；残余风险只是僵尸实例多发几帧。
  - 这样卡住的时长以本场权威截止为上限。补用例：RemoveObserver 回空应答且成局 state 5 时原件关闭、成局未到 5 时不关。
  - Java 版需同步核对。

### 一般-3：Codex 判据与当前生成器不一致，磁盘上的路由还是旧渲染

- **位置**：`reports/cpp-gate.md:22`、`:29`、`:33`；`reports/p1-gate.md:32`；`cpp/nodes/gate/handler/event/gate_kafka_command_router.cpp:147-186`。
- **触发序列**：Codex 按 README §3「各批细节见 reports」执行，`cpp-gate.md:22` 要求「只有四个 case 变为拒绝，其余不变」；而当前生成器（`gate_kafka_command_router.go:81-88`）会让 9 个全变，`p1-gate.md:32` 写的才是对的。
- **后果**：正确的生成结果可能被判失败，或有人为了「对齐说明」回退判据。磁盘上的路由仍是旧渲染（Route / Kick / Lease / Bind / Redirect 还在做信封映射），不重新生成就不生效。
- **最小修法**：在 `cpp-gate.md:22` 注明以 `p1-gate.md:32` 为准。`:29`（单播缺内容仍发空帧）和 `:33`（Redirect 未纳入）也已过时，一并标注。

### 一般-4：协议注释与设计文档还写着旧语义

- **位置**：
  - `proto/contracts/kafka/gate_event.proto:38`（信封路径映射 player_id）、`:66`（空 payload 的旧版 login 只带得到前两个字段）、`:130`（ALL connected sessions）。
  - `cpp/libs/engine/core/network/client_message_sender.h:33`（全部客户端会话）。
  - `docs/design/scene-enter-chain-residuals.md:290`、`:294`（新 gate + 旧 login 走信封路径、在途空 payload 踢线由信封路径处理）。
  - `cpp/libs/services/gate/session/system/session_identity_fence.h:56-57`（「未绑定却收到指名推送不可能发生」，严重-1 就是反例）。
- **触发序列**：下一个开发者或代理按注释相信空 payload 的 Kick / Bind 还能工作，或相信全服广播会到未登录连接。
- **后果**：不影响当前运行，但这些写的都是协议语义，会误导后续生产者和兼容判断（AGENTS §5）。
- **最小修法**：随 proto 统一改一次注释；设计文档 §2.5 补一句 2026-10-30 起空 payload 一律拒绝（项目未上线，不做混跑）。

### 一般-5：关键行为没有自动化证据，交付说明列的冒烟也测不出来

- **位置**：`cpp/nodes/gate/handler/gate_push_delivery.cpp:179-351`（`git grep` 确认没有任何测试调用 `Deliver*`，`gate_spectate_hub_test.cpp:949-974` 只测 `EncodedFrame` 和 `ContentOf`）；`cpp/nodes/battle/logic/battle_room_manager.cpp:2216-2251`（`ArmObserverSeatTimer`）。
- **触发序列**：
  - gate 入口接线写错（比如 Kafka 入口传错目标玩家、栅栏分支写反），或投递前漏判，单测全绿。`cpp-gate.md:26` 的冒烟只看「没有丢弃日志」，栅栏一直放行时也通过。
  - battle 的席位回收用例都显式调 `ReclaimExpiredObserverSeats`。真实定时器到期、早到重挂这条路径，30 秒常量下没有用例走到。代码已读过，未发现缺陷。
- **后果**：回归只能靠人工发现。严重-1 就是这类问题。
- **最小修法**：
  - 照 `battle_observer_contract_test.cpp` 的回环夹具加 gate_push 用例：向 `tlsSessionManager` 放会话，覆盖三态、未绑定跳过、scene 0、player_list 不齐、缺内容。
  - 冒烟再加一条正向断言：跨会话推送的目标玩家不符时 `dropped_fence` 增加。
  - 席位定时器若要覆盖，需要让宽限可注入，代价要和收益一起评估，可以不做。

## 重点核对结论

### (1) PayloadRequired 不会拒掉线上命令

当前生成器确实让 9 个入口事件的信封分支全部拒绝，但所有生产者都带 payload。检索了 `GateCommand{`、C++ 的 `GateCommand` 声明、`set_event_id`、`ResolveCommandRoute`、`GateCommandMessage`，范围含未跟踪文件；Java 与脚本里没有生产者。

| 事件 | 生产者 | payload |
|---|---|---|
| RoutePlayer | `go/scene_manager/internal/logic/enterscenelogic.go:1684-1707` | 有 |
| RedirectToGate | 同文件 `:1370-1391` | 有 |
| BindSession | `go/login/internal/svc/gate_command.go:115-131` | 有 |
| KickPlayer | 同文件 `:143-156` | 有 |
| PlayerLeaseExpired | `go/player_locator/internal/logic/leasemonitor.go:240-262` | 有 |
| PushToPlayer | friend / guild / match builder；`gate_command.go:186-203`；`go/mail/internal/notify/notify.go:93-97`；`battle_room_manager.cpp:145-158` | 有 |
| BroadcastToPlayers | 三份 builder | 有 |
| BroadcastToScene / All | 三份 builder（没有调用方） | 有 |

序列化结果也不会为空：Go 与 C++ 都显式设置了 message_content，字段即使全默认也会编出 tag；其余事件的 session_id 或 player_id 非 0。

### (2) 栅栏三态与 0 值：两条入口一致，生产者都填了

- RPC 与 Kafka 都走同一个 `AcquireFencedConnection`，判定只有一份。
- 生产者：friend `push.go:140-141`、guild `push.go:236-237`、match `playercontract.go:160-161`、login、mail、battle `:152` 都填目标玩家；scene RPC 按 `client_message_sender.cpp:77-80,126-129` 填。
- Go 与 battle 拒发 0，scene 只在实体缺 Guid 时走兼容位。
- 没有遗漏的生产者。问题不在 0 值，在未绑定会话的合法推送，见严重-1。

### (3) 对齐顺序：两端一致

- Go `gate_push.go:314-341`（稳定排序、`CompactFunc` 留首个、阈值与体积判据）与 C++ `broadcast_target_codec.h:60-64,83-103,157-181` 同口径，低位在前的位序也相同。
- 手工核对了 7 条向量（bitmap、去重、0x55 位序、稀疏退回 list），都符合契约；Go 与 C++ 两端都有读同一份向量的用例。

### (4) 场景 / 全服广播只推已绑定玩家：不破坏现有依赖

- `BroadcastToSceneClients`、`BroadcastToAllClientsOnKnownGates` 只有定义没有调用方（`player_mystery_merchant.cpp:219` 只是注释）；Go 的 `kafkautil.BroadcastToScene/All` 也没有调用方。
- 以后接入场景广播也不受影响：sceneId 非 0 的会话一定已被路由事件绑定（`scene_route_helper.h:324-326`）。
- 只有全服广播语义变了，不再发给未登录连接，注释要改（一般-4）。

### (5) 观众席回收：未发现本批缺陷

- **定时器**：重挂安全（`timer_task_comp.cpp` 按 generation 让旧回调失效，`OnTimer` 先清 armed）；早到时只重挂；全部摘除后取消；随房间析构取消。
- **重连竞争**：单线程；Attach 即停表（`:2641-2645`）；Detach 按连接身份比对（`:2662`）；回收后旧票被 `:2617` 拒绝；迟到的同 op Add 被墓碑或时间校验挡住。
- **match 对齐**：回收后 Query 回 ABSENT，走 Remove（准入已过期则删墓碑并回 cancelled，`:1244-1245`）到 state 5，能走通。实例换掉的情况走不通，见严重-2。
- **配额计数**：每次遍历房间表求和（`:2120-2131`），所有删房路径都不会漂移。
- `sql_outcome_abandoned` 删房（`:1635-1637`）不经 `RetireObserver`，残留的墓碑由 match 恢复循环的 Remove 清掉，不泄漏。

### (6) 编码一次：不影响参战帧，缓冲区所有权正确

- `PushSpectatorFrame`（`:2454-2469`）只服务观众帧；参战帧循环（`:2031-2037`）仍是逐人裁剪、逐人编码。
- `EncodedFrame` 持有独立字节，`SendEncoded` 经 `send(StringPiece)` 只读不改（`battle_client_edge.cpp:96-111`）。muduo 写不完的部分拷进各连接自己的缓冲。
- gate 的 `EncodedFrame::SendTo` 同理。关闭推迟到本轮 loop 之后，每条直连上终局包先于 FIN。
