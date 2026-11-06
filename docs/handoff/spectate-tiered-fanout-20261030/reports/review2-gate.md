# R2-GATE：观战分级扩容 C++ gate 侧第二轮语义评审（FIX-CPP 之后）

- 时间：2026-11-05 22:20–22:58（America/New_York）。只读静态阅读，按 `specs/review-rules.md`：没有编译、运行、生成，没有 git 写操作，没有导出临时文件。
- 基线：HEAD `993b6da9d`。评审对象 `cpp/nodes/gate/spectate/*`、`handler/gate_push_delivery.*`、`cpp/nodes/gate/tests/gate_{spectate_hub,push_delivery,spectate_ticket}_test.cpp`、`node_kafka_command_filter.h` / `node_kafka_command_handler.h`、`routing_identity_test.vcxproj`、`battle_room_manager.cpp` 的 END 段，磁盘内容都与 HEAD 一致，最后一次提交是 `fc504300b`。
- 评审期间另一个会话还在改 gate（断线保留角色 `player-connection-lost-hold`、发布轨道）：`client_session_handler.{h,cpp}`、`gate_event_handler.cpp`、`scene_entry_dispatch.cpp`、`session_info_comp.h`、`main.cpp`。这几个文件的行号以 22:57 的磁盘为准，之后可能漂移；当时三处 `gate_spectate::OnSessionClosed` 都在，没有被改动波及。

## 结论

发现 2 条：阻断 0，严重 0，一般 2（新身份模型下的回选角残余、身份栅栏的 kInvalidGuid 口子）。

`specs/96-fix-cpp.md` 的 8 条：7 条已修对，第 4 条（relay 一般-3 回选角）只修了一半，残余见一般-1。两份评审里 gate 侧的其余条目：relay 一般-2（维护 boot 没有 L2 消费者）不在本批，§15.7 已定为部署约束；c-batch 一般-3（Codex 判据）实质已解决；c-batch 严重-2 属于 match。修正批之后主会话改的两条水位、END 等在途一轮、轮转起点累计、僵尸实例 WARN 采样，按代码路径走下来都对，没有引入阻断或严重问题。

其他会话的身份改动 `372caca25` / `ac6cc026c` 和工作区改动，既没有绕过 gate_push 的身份栅栏和订阅凭证校验，也没有破坏观战装配。

---

## 一、修正项核对表

### review-cpp-relay.md

| 条目 | 结论 | 依据（文件:行号） | 说明 |
|---|---|---|---|
| 严重-1 END 抢占在途扇出 | 已修对 | `gate_spectate_hub.cpp:301-307`（OnEndFrame 只置 kEnded、存 166）、`:326-338`（StartFinalFanoutUnlessBusy：`:330` 在途就返回，`:335-336` 开始扇 166 时才清缓存）、`:479-494`（FinishFanout 发现刚发完的不是 166，接着发 166）、`:462`（必达只看 `fanoutFrame == finalFrame`）、`:218-221`（非 kLive 一律丢帧，在途一轮不会被接手，等待有上限） | 嵌套删条目是安全的：kSourceLost 在内层 FinishFanout 里 `entries_.erase`（`:501-504`），外层 PumpFanout（`:420-424`）、StartFanout（`:410`）、StartFinalFanoutUnlessBusy（`:337`）、FinishFanout（`:492-493`）、ContinueFanout（`:450`）在调用之后都直接返回，不再碰 entry；SweepIdle 逐个 `entries_.at`（`:540-543`），不持有引用跨调用 |
| 严重-2 升档后 END 只发广播 | 已修对 | `battle_room_manager.cpp:2457-2476`；`FinishBattle` 的 `:1479`（最后一回合 TURN）和 `:1482`（END）在同一调用栈，按同一时刻的 `spectateBroadcast` 选通道，Promote 插不进中间；`OnSqlOutcomeAbort`（`:1442`）只发 END | 用例 `battle_observer_contract_test.cpp:1361-1367` 已改成只有 1 帧广播 END。`proto/battle/battle_node.proto:187` 仍只说「每回合只发广播」，没提 END（注释遗留，非 gate） |
| 一般-1 合并时饿死表尾 | 已修对 | `gate_spectate_hub.cpp:77-87`（累计起点 `:84`、取模、`std::rotate` `:85`）、`:298`、RebindToInstance 复位 `:185` | 手算 5 人、批 2、连来 5 帧：起点 0→2→4→1→3，每人恰好 2 帧，与用例 `:762-792` 一致。取模的前提「有订阅者」由 `:294-297` 保证，不越界；每轮名单是订阅表键的一个排列，轮内每人恰好一次；`lastTurnRound` 去重，同一帧不会发两次。rehash 或上一轮是 STATE 补发时只是近似位置（`:83` 已写明），不会系统性饿死 |
| 一般-2 维护 boot 没有 L2 消费者 | 不在本批 | `main.cpp:283-288` 只打 WARN；§15.7 定为「走这个流程的部署把 `BroadcastMaxConcurrent` 置 0」 | `tools/scripts/cpp_nodes.ps1`、`maintenance_drain.ps1` 没有落实这条约束，只靠人记得；K8s 不受影响 |
| 一般-3 回选角不摘订阅 | 部分 | `scene_entry_dispatch.cpp:380`（ResetSceneRoute `:377` 之后摘）；hub 这条路径只动两张表（`gate_spectate_hub.cpp:149-167`），不发帧、不碰连接 | TCP 不断时语义正确，之后能重新订阅。但 LeaveGame 之后、新角色 Bind 之前建立的订阅仍会带进新角色，见新发现一般-1 |
| 一般-4 继任 gate 逐帧 WARN | 已修对 | `node_kafka_command_filter.h:43`、`:146-155`；`main.cpp:274`（中继打开采样）、`:291-293`（广播 options 由中继复制，共用计数） | 广播帧的目标字段为空，走不到这条分支，实际只计中继帧。默认（计数器为空）仍逐条打，只是 WARN 行多了 `sampled_total=0` |
| 一般-5 单水位注释 | 已修对 | `gate_spectate_hub.h:16-21`、`proto/contracts/kafka/gate_event.proto:160-163`、设计文档 `:139`（§6.1）、`:220`（§9）、`:433`（§15.5 收帧） | 遗留一处：设计文档 `:423`（§15.5「表」）的字段表仍写单个 `last_round` |

### review-c-batch.md（gate 侧）

| 条目 | 结论 | 依据 | 说明 |
|---|---|---|---|
| 严重-1 Bind 之前的失败提示被栅栏丢掉 | gate 侧已修对；Go 侧已落地 | gate 判定没变（`session_identity_fence.h:43-62`），注释已补；login 侧 `gate_command.go:179-192`（`buildPushTipToUnboundSessionCommand` 显式填 0）、`entergamelogic.go:257-262`、`:386-402`（按 `bindSent` 二选一） | 用例 `GatePushDelivery.UnboundSessionOnlyAcceptsTheExplicitZeroTarget`。「未绑定会话只接受显式 0」并不完全成立，例外见一般-2 |
| 严重-2 battle 换实例后原件关不掉 | 不属于 gate | match 侧，§15.9 已定放宽规则 | — |
| 一般-3 Codex 判据与生成器不一致 | 实质已解决 | 磁盘上的 `gate_kafka_command_router.cpp:150-184` 已重新生成，9 个信封分支都是 `RejectEmptyPayload`；`CODEX.md:51-52` 已注明以 p1-gate 为准 | `reports/cpp-gate.md:22` 原文还没加「已取代」标注（README 第 61 行列为收尾项），不影响执行 |
| 一般-4 协议注释旧语义 | 已修对 | `gate_event.proto:38`、`:66`、`:130`、`:137`；`client_message_sender.h:33-35`（372caca25 改名为 `BroadcastToKnownGateClients`，注释保留「未登录的连接不收」）；`session_identity_fence.h:20-22`、`:58-60`；`scene-enter-chain-residuals.md:295` | — |
| 一般-5 gate_push 缺测试 | 已修 | `gate_push_delivery_test.cpp` 9 个用例，见第四节 | 两条入口转调那一层没有单测（已披露）。我逐个读了 8 处调用点：`gate_service_handler.cpp:108-109`、`:121`、`:132`、`:143` 都是 `Path::kRpc`，目标玩家取 `header().target_player_id()`；`gate_event_handler.cpp:346-347`、`:355`、`:362`、`:369` 都是 `Path::kKafka`，取 payload 里的 `player_id`、`scene_id`。字段都没有取错 |

### specs/96-fix-cpp.md 逐条

| # | 对应 | 结论 |
|---|---|---|
| 1 | relay 严重-1 | 已修对。spec 指定的用例 `EndWaitsForTheLastTurnSoEverySessionSeesTurnThenEnd`（`gate_spectate_hub_test.cpp:922`）存在，断言每个会话恰好先 158 后 166 |
| 2 | relay 严重-2 | 已修对（battle 侧，见上） |
| 3 | relay 一般-1 | 已修对。fix-cpp.md 说「照抄评审只按 `fanoutNext % size` 转会在第三轮回到原点」，这个判断成立，累计起点的实现正确 |
| 4 | relay 一般-3 | 部分（见一般-1）。「TCP 不断时语义正确」这一点核实成立 |
| 5 | relay 一般-4 | 已修对 |
| 6 | relay 一般-5 | 已修对 |
| 7 | c-batch 一般-4 | 已修对 |
| 8 | c-batch 一般-5 | 已修；接线层没有单测，已如实写明替代验证 |
| 额外 | 源站失联也等在途的一轮发完 | 正确（`:323` 同样走 StartFinalFanoutUnlessBusy），用例 `SourceLostAlsoWaitsForTheInFlightFanout`（`:1024`）有区分力 |
| 额外 | ContinueFanout 判据（`:448`） | 防御性写法，无害。没有它时，过期续发会对已发完的一轮再走一次 FinishFanout；按现有状态机推下来，那时订阅表已空，不会重复发 166，所以没有可观测差异 |

### 修正批之后主会话改的两条水位

逐个序列核对，都得到预期结果：

- 挂接时 STATE(r) 之后紧跟 TURN(r)：STATE 只把缓存记到 r（`:251-256`），不推进 `lastTurnRound`；TURN(r) 大于 r−1，照常扇出，缓存记到 r+1（`:281-293`）。
- TURN(r) 之后才到 STATE(r)：r 小于 r+1，丢弃（`:251`）。
- 升档时广播 STATE(r+1) 先于中继 TURN(r)：TURN 照常扇出；缓存换成 TURN(r) 带的状态，与 STATE(r+1) 是同一回合，等价（`:288-293`）。
- 中继 TURN(r−1) 晚于广播 TURN(r)：丢弃。
- Kafka 重复投递：丢弃。
- 确认模式冻结的 STATE(r) → TURN(r) → END：battle 侧 `:1479-1482` 保证 TURN 与 END 同栈发出。

用例 `:404`、`:438`、`:451`、`:465`、`:482` 覆盖了这些序列。

---

## 二、重点场景逐个走查

1. **END / SOURCE_LOST 等在途一轮时**
   - 新订阅：直接收一帧 166（`:123-128`），不进订阅表，166 开始扇出时的名单（`:337`）也不含它。
   - 还没轮到的老订阅者重发订阅：先摘掉，直接收 166，所以看不到最后一回合。用例 `SubscribeWhileEndIsPendingGetsOnlyTheEnd`（`:999-1021`）把这一点定成了契约。这是有意的取舍，窗口只有在途一轮那么长，不报。
   - 退订或会话关闭：DetachSession 摘掉；在途一轮按订阅表跳过（`:456-460`）；166 名单按开始扇出时的订阅表建，也不含它。
2. **END 与 SOURCE_LOST 的先后**：两者互斥。非 kLive 一律丢帧（`:218-221`），巡检只处理 kLive（`:522-533`）。
   - 先 SOURCE_LOST、没有在途：条目立即删除，之后到的 END 重建一个没有订阅者的 kEnded 墓碑。
   - 先 SOURCE_LOST、等在途：这段时间到的 END 被丢，观众收到的是 SOURCE_LOST 而不是真 END。这要求巡检时恰好有一轮在途，只可能是「过了 deadline 仍在出帧」或单轮扇出超过 30 s。修正前 166 自身跨批扇出时也有同样的窗口，不是本批引入，不报。
3. **RebindToInstance**（`:171-190`）
   - 复位 phase、两条水位、缓存、终局帧、在途一轮和轮转起点；保留订阅者（`receivedState` 置 false）和 `continuationToken`。
   - 保留令牌是对的：排着的旧闭包会接着发换代之后开的那一轮；没有在途时被 `:448` 挡住。
   - match 只按成局记录的 TargetInstanceId 签凭证（`go/match/internal/logic/spectate.go:118`、`spectate_ticket.go:36-45`），签发前还查注册表（§15.9），所以同一 battle_id 的凭证只有一个实例。换代只会发生在条目由僵尸帧建起来的情况，以凭证为准正是想要的结果。未发现问题。
4. **合并扇出的累计起点**：越界、跳人、重复发都不会发生，理由见核对表一般-1。
5. **软水位跳帧之后的首帧 161**
   - kBufferFull 不置 `receivedState`（`:386-388`）；下一轮由 FirstFrameOf 合成 161（`:472-476`）。
   - END 等待期间缓存保留到 166 开始扇出（`:334-336`），名单尾部的会话照样能合成 161。
   - 166 必达（`:462-465`；`gate_spectate_client.cpp:61` 只对 BestEffort 判软水位）。
   - 不在在途名单里、又没收过状态的会话，END 时只收 166，与墓碑语义相同，客户端本来就要处理。
   - 用例 `:1100`、`:952`、`:1120`。
6. **同一会话订多场**
   - 订新场先退旧场（`:101-105`）；旧场在途一轮按订阅表跳过；FinishFanout 只清本场订阅者，不变量是「`battleOfSession_` 有记录 ⇔ 会话在该场订阅表里」（`:495-500`）。
   - 166 扇出途中改订别场也不会误删新场的记录，因为 DetachSession 先摘了旧场。
   - Unsubscribe 核对场次（`:140-147`）。
7. **终局后的回收**
   - kEnded：166 扇出完清掉订阅表和索引（`:495-500`）；缓存在开始扇出时已清；墓碑在 endedRetention 后删除（`:516-519`）。
   - kSourceLost：发完即删（`:501-504`），巡检跳过（`:520-521`）。
   - 没有订阅者的 kLive 条目：30 s 无帧或过了期限就删（`:522-533`）。
   - 条目数的上限是「30 s 内给本机发过帧的场次 + 10 分钟内结束的场次」；L2 每台 gate 都收全部广播场次，但受配额 B 限制（默认 4）。没有无界增长。
8. **重入与生命周期（§11.7）**
   - 续发闭包只捕获 `weak_from_this` 和值（`:429-434`），靠令牌作废（`:441-446`）。
   - `sendFrame` 不会同步回调 hub：Windows 版 muduo 的 `TcpConnection::sendInLoop`（`third_party/muduo/muduo/net/TcpConnection.cc:139-192`）只把 writeComplete / highWaterMark 回调排进队列，写失败不同步走 handleClose。
   - `tlsInstalled` 只存 `weak_ptr<SpectateHub>`（`gate_spectate_client.cpp:33-38`）。
   - defer 捕获的 `&loop` 比 context 活得长（`node_entry.h:176-185`：context 先于 Node 建、后于 Node 析构，loop 比两者都长）。

---

## 三、新身份模型下的身份栅栏与订阅校验（问题 2）

**别人改了什么**

- `372caca25`：
  - `SessionInfo` 新增 `authenticatedAccount` / `authenticatedOwnerRevision`（`session_info_comp.h:65-67`），只由被接受的 Bind 写（`authenticated_session_binding.h:32-33`），LeaveGame 时 ResetSceneRoute 清空（`scene_route_helper.h:155-156`）。
  - BindSessionEventHandler 在 ApplyLoginBinding 之前先过 `gate_authenticated_session::Bind`，核对连接代次、账号、修订、登录态，以及同角色时的登录代次（`gate_event_handler.cpp:278-282`）。
  - 路由 lane 里排队的请求要求受理时的身份仍然成立（`client_request_lane.cpp:140-146`、`session_details_builder.h:37-43`）。
  - 出站 gRPC 加 caller_auth 签名；main 缺内部签名密钥时拒绝启动。
- `ac6cc026c`：去掉握手印章，链路就绪改用 `RpcClient::IsReady` / `IsReadyOn`；踢线来源栅栏改用 `IsReadyOn` 认发送方（`gate_service_handler.cpp:45-59`）；OnClientLeaveGame 删了握手相关代码，摘订阅那一行还在。
- 工作区：
  - `client_session_handler.cpp`：一处注释，加上 PickRandomNode / SendRawViaRouter 按发布轨道选点。
  - `main.cpp`：写入 `max_connections`。
  - 断线保留角色：ExitGame 带原因，租约到期时置 `exitSceneImmediatelyOnClose`。

**gate_push 身份栅栏：仍然成立**

- `gate_push_delivery.cpp` 自 `fc504300b` 起没有变。`session.playerId` 仍然只有两个写入点：ApplyLoginBinding（`scene_route_helper.h:303`）和 ApplyRouteIdentity（`:325-328`）。
- 新的 Bind 闸门拒绝时，playerId 保持原值，带真实 player_id 的推送照样按栅栏丢弃，是 fail-closed 方向。
- `PushToPlayerEvent.player_id` 现在只能来自 payload：生成路由对空 payload 一律拒绝（`gate_kafka_command_router.cpp:150-184`），信封路径已不存在。
- `BroadcastToPlayersEvent.player_list` 先过 PlayerListAligned，再逐个目标过 ClassifyPush（`gate_push_delivery.cpp:225-249`）。
- 未绑定会话只放行显式 0，login 也确实按 `bindSent` 发 0。例外是 kInvalidGuid，见一般-2。

**两点观察（不计入发现）**

- 栅栏里的「未绑定」实际判的是「playerId 还没写入」。RoutePlayer 仍可在没有任何被接受的 Bind 时把 playerId 写进会话（`scene_route_helper.h:325-328`）。`RoutePlayerEvent` 不带连接代次，新的代次闸门只覆盖 Bind。这是改动前就有的设计（R14 残余），对观战没有影响。
- 新闸门拒掉 login 已发出的 Bind 后，login 的 `bindSent` 仍是 true，之后的失败提示带真实 player_id，会被栅栏丢掉，客户端要等超时。只在代次不符、字段不全这类异常 Bind 上发生，属 fail-closed，不报。

**SubscribeSpectate 的身份与凭证校验：仍然成立，没被绕过**

- 判据：`gate_spectate_client.cpp:100-102`。kInvalidGuid 或 0 一律当未绑定，判 kPlayerMismatch。
- 校验顺序（`gate_spectate_ticket.cpp:31-86`）：
  1. 密钥
  2. 凭证外层
  3. 签名
  4. 载荷（实例、玩家、场次不得为空）
  5. 玩家是否等于会话绑定的玩家
  6. 场次
  7. 期限（`<=` 判过期）
- 观战的两条本地消息在 lane 和 caller_auth 之前就分走了（`client_session_handler.cpp:1189-1192`）。新身份模型的那几道闸对它们不适用，它们也不需要：消息不出 gate。
- 问题出在 `session.playerId` 在 LeaveGame 后保留、而认证绑定已清空，见一般-1。

**观战装配：完好**

- InstallHub 先于 SetKafkaHandlers（`main.cpp:399-403`）。`node_entry.h` 的 ApplyPostConstructionHooks 在 configure 之前装默认钩子，所以 gate 的覆盖生效，gate-cmd 的注册保留在 RegisterGateKafkaConsumers 里。
- 中继消费者 assign `node_id % P`。广播消费者 assign 全部 16 个分区，并核对分区契约：broker 明确答了不同的分区数就拒绝启动，查不到就照契约继续（`kafka_consumer.cpp:211-256`）。维护 boot 不注册广播消费者（`main.cpp:283-288`）。
- 巡检定时器在 `main.cpp:545`。
- 三处摘订阅的调用：断线 `client_session_handler.cpp:663`、无连接的残留会话 `gate_event_handler.cpp:231`、LeaveGame `scene_entry_dispatch.cpp:380`。gate 里只有两处 `sessions.erase`（`client_session_handler.cpp:665`、`gate_event_handler.cpp:232`），前面都先摘了订阅。
- 发布轨道改动只影响 PickRandomNode 和路由转发，观战本地消息在它们之前就已分走。

---

## 四、测试核对（问题 3）

**`gate_spectate_hub_test.cpp`**

- 发送、续发、时钟全部注入，不开 EventLoop，不用真实墙钟，结果不依赖订阅表的哈希序（只断言「谁收到什么」）。
- 新增用例在修正前的代码上都会红，不是恒真：
  - `EndWaitsForTheLastTurn...`：旧代码第一批之后 END 就抢占，尾部会话收不到 158。
  - `TailSessionWithoutStateStillGetsAStateFrameBeforeEnd`：旧代码提前清缓存，第二个会话只收到 166。
  - `LastTurnIsStillBestEffortWhileEndIsPending`：旧代码按 phase 判必达，会多发一帧 158。
  - `SourceLostAlsoWaitsForTheInFlightFanout`：旧代码巡检后立刻多发一帧 166。
  - `OverloadedFanoutTakesTurnsInsteadOfStarvingTheTail`：不旋转或不累加，每人收到的帧数就不是 2/2/2/2/2。
- `TurnFanoutStartStaysInRangeWhenSubscribersLeave` 靠 Debug 迭代器检查报红（去掉取模时是 `begin()+4` 越界）。工程只登记了 Debug|x64，可以接受；Release 下会是未定义行为，不是干净的失败。
- 夹具的 `TurnFrame(r)` 把 `state.round_index` 设成 r，真实 TURN(r) 带的是 r+1（`gate_event.proto:163`）。hub 不读内层回合号，用例仍然有效；`TailSession...` 断言 9 只对这个夹具成立。

**测不到的接线（都已披露）**

- `SessionLeavingTheGameStopsReceivingAndCanSubscribeAgain`（`:306-333`）只测了 hub 接口。删掉 `scene_entry_dispatch.cpp:380` 它照样绿，修正前的 hub 本来就是这个行为，所以它不是一般-3 的回归保护。
- 同样没有用例的还有：`main.cpp:274` / `:291-293` 的采样接线，两条入口 8 处 `gate_push::Deliver*` 转调。现在只靠 fix-cpp 第 4 步的 grep 核对。考虑到他人还在改这几个文件，建议 Codex 验证时重跑那条 grep。
- `SpectateFrameTargetFilter.*` 用手写的 `GateFilter()`（`:1141-1147`），没有用 `BuildDefaultKafkaOptions(GateNodeService)`。它只钉住了「SpectateFrameEvent 的这几个字段名能被过滤器读到」，没钉住默认派生出的名字。如果派生变了，node_id 这一级过滤会静默失效：实例那一级仍会挡掉别的 gate 的帧，但僵尸实例 WARN 会消失。影响小，不单列。

**`gate_push_delivery_test.cpp`**

- 用真实回环连接，「没有写」用哨兵帧判定：TCP 保序，不靠等待。计数只看本用例的增量。连不上或等超时都会报红，不会误过。
- 覆盖了栅栏三态、未绑定会话、0 值兼容、场景与全服广播跳过未绑定会话、scene 0、player_list 不齐、缺 message_content，与 fix-cpp 的声明一致。
- 缺 target = kInvalidGuid 的用例（一般-2）。

**`gate_spectate_ticket_test.cpp`**

- 跨语言固定向量与 §15.9 一致：payload `080110021a016920032804`，11 字节，签名常量相同。
- 期限边界、篡改、换密钥、空密钥、载荷缺字段都有区分力。

---

## 五、新发现

### 一般-1：回选角残余仍在，LeaveGame 之后、新角色 Bind 之前建立的观战订阅会带进新角色；新身份模型已有现成判据可以堵

- **位置**
  - `cpp/nodes/gate/spectate/gate_spectate_client.cpp:100-102`：身份判据只看 `session.playerId`。
  - `cpp/nodes/gate/handler/event/scene_route_helper.h:138`、`:145-169`：LeaveGame 保留 playerId，只清 `authenticatedAccount`。
  - `scene_route_helper.h:303`：Bind 换角色时改 playerId。
  - `cpp/nodes/gate/handler/event/gate_event_handler.cpp:278-287`（22:57 行号）：Bind 换角色时没有摘订阅。
  - `authenticated_session_binding.h:32-33`：`authenticatedAccount` 只由被接受的 Bind 写。
- **触发序列**
  1. 会话 S 以角色 P 在游戏里，客户端持有 P 的观战凭证（WatchBattle 回包，TTL 60 s；在途 WatchBattle 的回包可能晚于 LeaveGame 到达）。
  2. 客户端发 LeaveGame：OnClientLeaveGame 摘掉已有订阅（`scene_entry_dispatch.cpp:380`），ResetSceneRoute 清掉 `authenticatedAccount`，playerId 仍是 P。
  3. 客户端在选角界面发 SubscribeSpectate（P 的凭证）。`boundToPlayer` 为真，sessionPlayerId 是 P，验签通过，`hub->Subscribe(S)`。
  4. 客户端以 Q 发 EnterGame。login 允许 LeaveGame 之后换角（`entergamelogic.go:111`）。Bind(Q) 依次通过 IsBindFromBeforeLeaveGame（不同角色，代次不可比，放行）和 `gate_authenticated_session::Bind`；ApplyLoginBinding 把 playerId 改成 Q。整个过程中没有任何一步摘 S 的订阅。
  5. 该场的观战帧继续发到 S，直到 END、空闲超时、断线或客户端退订。
- **后果**
  - 凭证绑的是 P，帧却发给现在是 Q 的会话，违反「凭证不能转让」（§9）。
  - 帧是公开局的脱敏帧，不碰资产和战斗状态，只是多收帧。这与 fix-cpp.md 未完成项第 5 条是同一个缺口。
  - LeaveGame 之后新发的 WatchBattle 多半会被 match 按离线拒绝（`global_spectator.go:259`），所以窗口主要是 LeaveGame 前已签出、60 s 内仍有效的凭证。
- **最小修法**（二选一，都做也只是几行）
  - `HandleSubscribe` 的身份判据加上 `!session.authenticatedAccount.empty()`。这个字段由 LeaveGame 清空、只有被接受的 Bind 才写，与路由 lane 的 MatchesAuthenticatedBinding 是同一份真相，不必再去改 ApplyLoginBinding。
  - 或者在 BindSessionEventHandler 里，ApplyLoginBinding 之前记下旧 playerId，角色变了就调 `gate_spectate::OnSessionClosed(sessionId)`。这样不依赖身份模型，换角路径上都能兜住。
- **回归用例**：把判据抽成纯函数（例如 `gate_spectate::SpectatePlayerOf(const SessionInfo&)`，登记进 routing_identity_test），加 `SpectateSessionIdentity.LeaveGameClearsTheIdentityUntilTheNextBind`，断言 Bind 之后 → LeaveGame 之后 → 新 Bind 之后三种状态。走第二种修法时，再在 fix-cpp 第 4 步的 grep 清单里加上 Bind 换角那一处。

### 一般-2：身份栅栏把 target = kInvalidGuid 判成「与未绑定会话相符」，「未绑定会话只接受显式 0」有一个 UINT64_MAX 口子（当前没有生产者能触发）

- **位置**
  - `cpp/libs/services/gate/session/system/session_identity_fence.h:43-62`：`:49` 的相等比较在两边都是 UINT64_MAX 时成立，返回 kDeliver。
  - `cpp/nodes/gate/handler/gate_push_delivery.cpp:189-201`：据此写 socket。
  - `session_identity_fence.h:98-115`：ClassifyKick 复用同一判定，同值时判 kKick。
- **触发序列**
  1. 会话 S 刚连上、还没绑定，playerId 是默认值 kInvalidGuid（`session_info_comp.h:44`）。
  2. 某个生产者给 S 发 `PushToPlayerEvent{player_id = UINT64_MAX}`；scene 的 `header.target_player_id` 或指名广播 `player_list` 里的 UINT64_MAX 也一样。
  3. `ClassifyPush(UINT64_MAX, UINT64_MAX)` 返回 kDeliver，推送写进这条未登录连接。同值的 KickPlayerEvent 会踢掉它。
- **后果**
  - 这正是 R13 要挡的形态：继任 gate 上一条刚连上的陌生连接收到别人的推送。
  - 已核对当前生产者都不会发这个值：
    - scene 两条发送路径都把 kInvalidGuid 归一成「不填」或 0（`client_message_sender.cpp:77`、`:129`）。
    - battle 发真实玩家号，并拒发 0（`battle_room_manager.cpp:90-97`）。
    - Go 的 builder 拒发 0，player_id 来自发号器（63 位以内）。
  - 所以这是纵深缺口，不是现网路径。`client_message_sender.cpp:74-76` 的注释说 kInvalidGuid「必然失配丢弃」，对未绑定会话并不成立。
- **最小修法**：在 ClassifyPush 的 0 判定之后、相等比较之前加一句：会话未绑定（kInvalidGuid 或 0）时一律 kDrop。ClassifyKick 随之自动正确。
- **回归用例**
  - `GateIdentityFence.InvalidGuidTargetNeverMatchesAnUnboundSession`（`routing_identity_test.cpp` R13-A 段）：`ClassifyPush(kUnbound, kUnbound) == kDrop`；ClassifyKick 同值判 kSkipOtherPlayer。
  - `GatePushDelivery.UnboundSessionDropsTheInvalidGuidTarget`。

---

## 六、观察与未覆盖（不计入发现）

- **确认模式等回执与 T_idle**
  - 最后一回合的 TURN 要等 CommitBattleOutcome 回执之后才发（BeginResultCommit 冻结，`battle_room_manager.cpp:1321` 起；FinishBattle `:1479-1482` 才发出）。
  - 回执每 5 s 重放一次，最长可达数小时（`result_commit.h:97-100`，22:58 行号，该文件有他人在途改动，常量未变）。
  - match 不可达超过约 30 s 时，gate 巡检按 T_idle 给 LOBBY 观众合成 SOURCE_LOST 并删除条目，之后到的最后一回合和真 END 只进墓碑。
  - `gate_spectate_hub.h:91` 要求 idleTimeout 大于「结果提交等待」，battle 侧并不保证这一点。失联收尾本身是 fail-safe；要不要在等回执期间发心跳帧，由负责人定。
- **未覆盖**
  - `gate_spectate_client.cpp` 的真实软水位判定与回执顺序、`main.cpp` 的消费者装配，仍没有自动化证据，要等 robot 支持 SubscribeSpectate 后做 LOBBY 冒烟。
  - Linux muduo 的 send 语义与 Windows 版一致，只核对了 Windows 版源码。
  - Go、Java 不在本次范围；Java 是否有一般-1、一般-2 同类问题未核对。
