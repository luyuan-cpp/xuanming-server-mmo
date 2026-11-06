# 第二轮语义评审：观战分级扩容 battle 节点（R2-BATTLE，2026-11-05）

只读静态阅读，未编译、未运行、未压测，规则见 `specs/review-rules.md`。

**范围**：`cpp/nodes/battle/logic/battle_room_manager.{h,cpp}` 的观战部分、`battle_spectate_relay.{h,cpp}`、`handler/grpc/battle_node.{h,cpp}` 的两条新 RPC、`main.cpp` 的观战装配，以及两个测试文件 `tests/battle_observer_contract_test.cpp`、`tests/battle_spectate_relay_test.cpp`。为核对跨端语义，还读了 gate `SpectateHub`、`node.cpp` 的维护排空、Go match `global_spectator.go` / `spectate.go`、Java `BattleRoomServiceImpl.java`。这些文件不在评审对象内。

**行号口径**：
- 与 HEAD（993b6da9d）一致的文件：`battle_room_manager.cpp`、`battle_spectate_relay.*` 和两个测试文件。
- 评审期间另一个会话在改 `battle_room_manager.h`、`battle_node.{h,cpp}`、`main.cpp`（R6 可分配状态发布，与观战无关）。这几个文件的行号取 11-05 读到的磁盘内容，之后可能继续漂移，所以都附了函数名。
- `node.cpp`、`result_commit.h` 在工作区也有他人改动，行号同样以读取时为准。

**结论**：修正批和主会话补的「同 op 重放回首次拒绝码」在 battle 侧都修对了。65c69f4a9 删掉旧终局协议分支后，观战语义没有丢：DestroyBattle 不再结束活房间、停机不发 END，都与 §15.4 一致。

新发现 2 条，都是一般，没有阻断，也没有严重：
1. battle 等 SQL 决定期间不出任何观战帧。手动回合窗口现在是 25 s，再赶上一次提交重试，gate 的 30 s 空闲收尾就会抢先，中继 / 广播观众看不到最后一回合和真实结局。
2. 停机 / 维护排空时，冻结的房间仍然接纳 AddObserver、Attach、Promote。Java 版已经拦了。

---

## 一、修正项核对

| # | 来源 | 要求 | 现状（以磁盘为准） | 结论 |
|---|---|---|---|---|
| 1 | review-cpp-relay 严重-2；96-fix-cpp 第 2 条 | 已升档时 END 只发广播 | `battle_room_manager.cpp:2457-2476`：`PublishSpectateEnd` 升档后发 `PublishToBroadcast` 即返回。END 唯一出口是 `NotifySpectateEndAndClose:1622`，COMPLETE 经 `FinishBattle:1482` 进来，ABORT 经 `OnSqlOutcomeAbort:1445` 进来。TURN 同口径（`PublishSpectateTurn:2437-2455`）。注释已同步：`.h:209`、`:216`、`:525-529`、§15.4 | 已修对 |
| 2 | 同上（用例） | `PromoteIsIdempotentAndLaterRoundsOnlyBroadcast` 的 END 改成 1 条广播 | `battle_observer_contract_test.cpp:1361-1367`；`EndFrameReachesEveryChannelOnceWithDirectObserverReason:1398-1406` 另钉了一次 | 已修对。只覆盖 ABORT 路径，见第四节 |
| 3 | fix-cpp 未完成项 6 | 协议与用例注释 | `proto/battle/battle_node.proto:187` 仍只写「此后每回合只发广播」，没提 END（该文件工作区另有改动）。`battle_spectate_relay_test.cpp:91` 写的「同一帧刚发过中继」，生产上已不会出现（TURN / END 都是二选一），用例当防御仍有效 | 部分（只剩注释） |
| 4 | review-match G2（battle 侧）；§15.9 | 墓碑记首次拒绝码；同 op 同请求重放回它；无记录维持 kInvalidParameter | 见下方展开 | 已修对（细节见第三节第 1 条） |
| 5 | review-cpp-relay 严重-1 的上游 | battle 在同一调用栈里先 TURN 后 END，且同分区 | `FinishBattle:1479-1482`。中继按 gate node_id % P 分区，广播按 battle_id % 16，key = battle_id。生产者开了 `enable.idempotence`（`kafka_producer.cpp:105-111`），分区内保序 | 成立。gate 侧的修正不在本评审 |
| 6 | p1-battle 风险 3 | 同 (gate, instance) 重复 Attach、重复 Promote 只回包、不补 STATE，靠下一回合自愈 | `:2373-2376`、`:2399-2402` 未变 | 保持。但等 SQL 决定期间没有「下一回合」，自愈前提不成立，见一般-1 |
| 7 | p1-battle 风险 4 | 迟到的旧实例 Attach 会覆盖新实例 | 仍是后写赢（`:2372-2377`）。match 先发 RPC 后写 HASH，新 gate 的下一名观众读到实例不符就会重挂，能自愈 | 未处理，按残余接受，不再报 |
| 8 | p1-battle 风险 2 | 等决定期间 STATE 帧取冻结的可见状态 | `BuildSpectatorState:1559-1570` 取 `pendingVisibleState`；用例 `RelayStateFrameDuringOutcomeWaitDoesNotLeakTerminalResult:1478-1506` | 改写后仍在 |
| 9 | cpp-battle（B 批） | 可见性第二道闸 | AddObserver 在 `:969-975`，位于幂等块之后；Attach / Promote 在 `FindSpectateChannelRoom:2333-2339` | 改写后仍在 |
| 10 | cpp-battle（P0） | 节点观众席配额 | 判定在 `:988-995`；注入在 `SetClientEdge:1833-1846`，`main.cpp` 当前第 393 行调用 | 改写后仍在 |
| 11 | cpp-battle（P0） | 观众帧只编码一次 | `PushSpectatorFrame:1907-1922`。回合帧 `:1512-1519` 和终局帧 `:1621-1637` 都是一份信封同时给 L0 和通道 | 改写后仍在 |
| 12 | review-c-batch (5) | 观众席回收 | `ReclaimExpiredObserverSeats:1725-1752`；挂上直连停表 `:2094-2098`；断开计时 `:2106-2125`；`RetireObserver:1654-1678` | 改写后仍在。`sql_outcome_abandoned` 删房（`:1365-1374`）不经 RetireObserver，残留的活墓碑靠 match 的 Remove 清，结论同上一轮 |
| 13 | 65c69f4a9 改写 | 有没有把修正弄丢或改坏 | 见下方展开 | 没有修正项丢失。但见一般-2 |

**第 4 行展开（首次拒绝码）**：
- 字段与重放：`.h:629-630` 新增 `rejectTip`；`.cpp:836-842` 遇到已封闭的墓碑，回首次码。
- 记录器：`:854-869` 的 `FirstRejectRecorder` 在函数退出时按 key 重查墓碑，只在 `cancelled && rejectTip == 0` 时写入。
- 立墓碑之后的 9 个拒绝出口都先写了 error id：`:876`、`:888`、`:898`、`:910`、`:929`、`:973`、`:983`、`:993`、`:1005`。
- 3 个成功出口（`:941`、`:961`、`:1012`）都把 `cancelled` 置回 false，记录器不动作。
- 立墓碑之前的 3 类拒绝不记码，与 `.h:186-189` 的契约一致：入参校验 `:819-824`、原件不符 `:831-835`、墓碑表满 `:844-849`。

**第 13 行展开（65c69f4a9 改动了什么）**：
- 删的是旧终局协议。
- `HandleDestroyBattle:636-659` 不再结束活房间，也就不发 END；用例 `DestroyNeverEndsALiveRoomAndTombstonesAnAbsentOne` 钉住了。
- `AbortAllRooms:1754-1795` 去掉了旧协议分支。房间全部保留、不发 END，符合 §15.4「停机且结果未落定不发」。
- `ResolveRound` 和 `OnBattleDeadline` 一律进 `BeginResultCommit`。最后一回合的 TURN 存进 `pendingFinalTurn`，拿到 COMPLETE 时与 END 在同一调用栈发出。
- `main.cpp` 只改了注释。被删的旧注释「AddObserver / IssueBattleTicket 因房间不存在自然被拒」本来就不成立：v1 房间从 10-23 起停机时保留在表里。删了注释却没补闸，这就是一般-2。

---

## 二、新发现

### 一般-1：battle 等 SQL 决定期间不出观战帧，gate 的 30 s 空闲收尾会抢在最后一回合和 END 之前

**位置（battle）**：
- `battle_room_manager.cpp:1229-1235`：分出胜负的那一回合，帧存进 `pendingFinalTurn`，不发。
- `:1321-1352`：`BeginResultCommit` 不发任何帧。
- `:1383-1394`：提交失败或没有 READY 的 match 通道时，下一次提交在 `SqlOutcomeRetryInterval` 之后（`result_commit.h:98`，5 s）。单次 RPC 的 deadline 是 7 s（`bin/etc/base_deploy_config.yaml:216`）。
- `:1479-1482`：最后一回合的 TURN 和 END 只在拿到 COMPLETE 之后发。
- `:1181-1183`：手动回合窗口 `kActionTimeoutMs`，取值见 `turn_battle_constants.h:19`，为 25 s。

**位置（gate）**：
- `gate_spectate_hub.h:92`：`idleTimeout` 30 s。
- `gate_spectate_client.h:37`：每 5 s 巡检一次。
- `gate_spectate_hub.cpp:89-96`、`:507-543`：有订阅者的条目 30 s 没有新帧，就合成 166（SOURCE_LOST）。
- `gate_spectate_hub.cpp:495-504`：SOURCE_LOST 发完即删条目、释放订阅。

**位置（设计）**：`battle-spectate-tiered-fanout.md:126`（§5.3）要求「T_idle 必须大于手动行动窗口 6s 加结果提交等待」。从 10-29（a3c9b9b7d）起手动窗口已是 25 s，这条约束只剩约 5 s 留给结果提交。

**触发序列**：
1. 一场中继或广播场次里，某个参战者挂机或想得久，第 r 回合等满 25 s。TURN(r-1) 在 T0 发出。
2. T0+25 s，第 r 回合结算并分出胜负，`BeginResultCommit` 发出第一次 CommitBattleOutcome。如果 match 副本正在滚动重启、这次调用 7 s 超时，或此刻没有 READY 通道，下一次要再等 5 s。
3. T0+30 s 到 35 s 之间，gate 巡检发现 30 s 没有新帧，给全部订阅者发 166（SOURCE_LOST），然后删条目。
4. 之后 COMPLETE 才到，battle 发出 TURN(r) 和 END(BATTLE_FINISHED)。gate 为它们新建一个没有订阅者的条目，没有人收到。

两种扩展情况：
- match 不可用超过 30 s 时，不论回合长短，这期间结束的场次都会这样。battle 按设计会重放 6 小时，gate 只等 30 s。
- 同一原因还让另一条自愈失效：Attach / Promote 补发的 STATE 没发出去时，本来靠下一回合 TURN 补上。等决定期间没有下一回合，新订阅者 30 s 后同样只拿到 SOURCE_LOST。

**后果**：
- L1 / L2 观众看不到最后一回合和真实胜负，只收到「源站失联」。热门局和赛事局的大多数观众都在这两档。
- 同一场的 L0 直连观众能看到。
- 不碰资产，没有告警，只计入 gate 的 `source_lost`。
- 这是条件触发。一旦触发，后果与上一轮的严重-1 / 严重-2 相同。

**最小修法**（任选其一，都要同步改 §5.3 的「6s」）：
- **battle 侧补关键帧（推荐）**：
  - 在 `PumpSqlOutcomes` 里做。它本来就是每秒一拍。
  - 对象：`outcomePending`，且开了中继或广播通道的房间。
  - 距上一帧观战帧满 10 s，就按 TURN 的通道再发一帧 `BuildSpectateStateFrame`：已升档只发广播，否则逐台中继。这一帧取的是 `pendingVisibleState`，不会提前泄露终局。
  - 过了 `deadlineMs + 60 s` 就停，因为 gate 此时已按期限收尾。不停的话，滞留房间会按中继 gate 数长时间持续发帧。
  - gate 侧不用改：同实例的帧都会刷新空闲计时（`gate_spectate_hub.cpp:223`）；STATE(r) 不低于 `cachedStateRound`，只更新缓存，不打扰正在看的会话。
- **gate 侧放宽**：把 `idleTimeout` 放到「手动窗口 + 提交预算」以上。代价是 battle 真崩时收尾变慢。

**回归用例**：`BattleObserverContract.OutcomeWaitRepublishesFrozenStateBeforeGateIdleTimeout`。
- 夹具：1 点气血建房，挂一台 gate，结算一回合，进入等决定。
- 驱动：用显式时刻调用补帧入口，接缝做法同 `ReclaimExpiredObserverSeats`。
- 断言：
  - 10 s 后这台中继收到一帧 STATE，帧里参战者都还活着。
  - 升档后改为只发一条广播。
  - `Complete` 之后仍是 TURN 在前、END 在后。

### 一般-2：停机 / 维护排空时，冻结的房间照常接纳观战

涉及的接口：AddObserver、AttachRelayGate、PromoteSpectateBroadcast。

**位置**：
- `AbortAllRooms:1754-1795` 只停定时器、关直连。房间留在表里，没有任何观战准入标记。
- `HandleAddObserver:812-1023` 和 `FindSpectateChannelRoom:2314-2341` 都不看停机状态，`BuildAssignment` 此刻照样签得出票。
- 装配在 `main.cpp` 的 `closeSource`（当前 :489-508）：先把 TCP 新连接回调换成立即 forceClose（:493），再调 `AbortAllRooms`（:503）。
- `SetMaintenanceInboundRelay()`（当前 :510）：维护模式下关闭 source 之后，gRPC 服务端不停（`node.cpp:1705`），要到 relay 阶段才 `ShutdownGrpcServer`（`node.cpp:1794`）。
- 对照 Java：`BattleRoomServiceImpl.java` 在 `shutdown != null` 之后一律回 SERVICE_UNAVAILABLE，包括 `:410`（addObserver）、`:370`（issueBattleTicket）、`:577-579`（中继开关校验）。

**触发序列**：
1. K8s 删 Pod 时：`closeSource` 调 `AbortAllRooms`，随后 gRPC 排空约 3 s，期间照常派发已经收下的调用。维护模式下，match 的调用在整个 P1 阶段都会到。
2. AddObserver 到达：房间存在、公开、有席位，于是登记观众，并经 Kafka 下发落点。客户端连直连面时被立即 forceClose。match 照常把原件推进到 state 2（`global_spectator.go` `advanceGlobalSpectator`）。
3. AttachRelayGate 到达：挂上 gate，补发一帧 STATE。PromoteSpectateBroadcast 到达：房间置为广播档，match 占用一份 L2 配额。
4. 房间定时器已经停了，之后不会再有 TURN 或 END。随后进程退出。

**后果**：
- 新 L0 观众拿到一张用不上的票。原件要等实例消失、满足 §15.9 的放宽条件（同 node_id 换了 uuid，或过了成局截止加宽限，最长约 1 小时）才能关闭。在此之前玩家排不了队。
- 新 L1 / L2 观众看到一帧冻结画面，30 s 后收到 SOURCE_LOST。
- Promote 让一个必然作废的场次占住 L2 配额（默认 B=4），直到配额按期限到期或被 match 清理。
- 影响面：K8s 下只影响停机那一刻在途的调用；维护流程下持续整个 P1 阶段。

**最小修法**：
- `AbortAllRooms` 设置一个管理器级标记，例如 `spectateAdmissionClosed_`。
- 在 `HandleAddObserver` 入口、以及 `FindSpectateChannelRoom` 的实例校验之后检查它，回 `kServiceUnavailable`。这与 Java 同口径。Go 会把它映射成 16018、不剔索引，影响以排空时长为界。
- 可选：同时对各房间的 L0 观众逐个 `RetireObserver`（直连已关，不推 166）。这样 QueryObserver 立刻回 ABSENT，维护模式下 match 能在进程退出前拿到取消证明。
- 管理器是进程级单例，测试夹具要复位这个标记。

**回归用例**：`BattleObserverContract.AbortAllRoomsClosesSpectateAdmission`。
- 夹具：建房，挂一台 gate，然后调 `AbortAllRooms`。
- 断言：
  - 新 op 的 AddObserver 回 kServiceUnavailable，Query 为 ABSENT。
  - Attach 和 Promote 都回 kServiceUnavailable，且不产生任何帧。
  - `ShutdownSendsNoEndFrameWhileOutcomeIsUndecided` 的原有断言保持不变。

---

## 三、问题 2 其余各点（已核对，未发现问题）

**1. 首次拒绝码的各条路径**
- **提前 return**：立墓碑之前的三类拒绝不记码，同一 op 之后仍可能被重新判定，与契约一致。
- **拒绝时墓碑被删**：签票失败的幂等重推（`:928`）经 `RetireObserver` 可能删掉墓碑（恰好原截止已过）。记录器按 key 重查，查不到就不写，不会碰失效的迭代器。
- **成功后又被封闭**：成功出口不写码。之后被 Remove、回收或结束封闭时，`rejectTip` 仍是 0，重放回 kInvalidParameter。用例 `:756`、`:824` 钉住了。
- **曾经成功、后来某次重放签票失败**：这一次记成 kServiceUnavailable，之后的重放都回它。Go 的 `spectateRejectVerdict`（`spectate.go:217`）对 kServiceUnavailable 和 kInvalidParameter 都落 16018，结论不变。
- **Remove 先到**：建出的墓碑原件为空，之后任何 Add 都按原件不符回 kInvalidParameter，不经记录器。
- **异常**：记录器析构时写入的是已经设置的 id；没设置就是 0，等于不写。墓碑在 `:854` 已经封闭，fail-closed。
- **只在准入窗口内成立**：过了 `admit_before_ms` 的重放，在 `:820` 直接回 kInvalidParameter。Go 过了截止就不再发 Add，按截止自己结案（`global_spectator.go:602-605`），不构成缺口。

**2. Attach / Promote 的幂等与重放**
- 同 (gate, instance) 重复 Attach 只回包。同 node_id 换了实例就覆盖，只给新实例补 STATE。
- 升档后再 Attach 回 `broadcast = true`，不挂 gate。Promote 单向、幂等。
- 全部在 loop 线程执行：muduo_grpc 在 loop 上派发，两个守护段为空，直接 `Proceed`。所以与 END 没有并发。
- END 之后房间已删，Attach / Promote 回 kEntityIsNull。
- 等决定期间升档的话，COMPLETE 时最后一回合和 END 都只走广播。

**3. 中继 gate 名单**
- 键是 gate node_id，上限就是 gate 节点数。
- 只随房间销毁回收；死掉的 gate 会一直收到帧，直到本场结束。
- battle 侧没有上限。抢不到 L2 配额时，每回合产出随 G 线性增长，这是 §3 的设计取舍（「L1 没有人数上限」）。
- `relayGates[gateNodeId]` 用 `operator[]` 插入，但实例已校验非空，空串不可能与它相等，不会留下空项。

**4. END 与 TURN 的顺序**：两者同一调用栈、同一通道、同一分区；升档后都只走广播。L0 直连上终局包先于 FIN，因为关闭推迟到了本轮 loop 之后。

**5. 房间结束时的回收**
- `NotifySpectateEndAndClose` 逐个调 `RetireObserver`，然后清名单、取消席位定时器。
- `relayGates` 和 `spectateBroadcast` 随房间一起析构。
- 墓碑表有 `kMaxSpectatorOperations` 上限。过期的封闭墓碑，每次准入或取消最多清 128 条。
- 活墓碑数受节点配额约束，过了截止后由 RetireObserver 或 Remove 直接删。
- 没有无界增长。

**6. 目标字段与 Kafka key**
- 中继帧填了 `target_gate_id` 和 `target_instance_id`（`battle_spectate_relay.cpp:29-37`）。Attach 时已校验非 0、非空。
- 广播帧清空这两个字段，按 §15.3 靠 `battle_instance_id` 防僵尸。`battle_instance_id` 恒为本实例。
- key 一律是 battle_id 的十进制字符串。
- 落点公告的 GateCommand 带了 gate_instance_id 和 player_id。
- 符合 AGENTS §7.2 / §7.3。

**7. AGENTS §11.7**：新增路径没有引入新回调。席位定时器和 SQL 重放定时器只绑 `WeakSelf()`，直连关闭只捕获 weak 连接。

**8. 热路径**：Kafka 发送不阻塞，队列满时只 `poll(0)` 后重试一次。Attach 时每台 gate 只做一次快照。

---

## 四、测试核对（问题 3）

- **恒真的断言**：`FullRoomRejectsWithSpectateFullWhileSeatedReplaySucceeds:729` 的重放断言恒真，因为重放时房间仍满，重新判定也回 16021。
  - 真正能区分「回记录」与「重新判定」的是两个用例：`ReclaimedSeatsAdmitTheTwentyFirstObserver:782` 和 `NodeObserverQuotaSpansRoomsAndReopensAfterReclaim:912`。它们都先腾出席位再重放，结果仍是 16021。
  - 三处合起来，覆盖是到位的。
- **区分不了的路径**：
  - 可见性拒绝的重放（`:875`）：记录值和缺省值都是 kInvalidParameter，区分不了。
  - 签票失败后的重放回 kServiceUnavailable，没有用例：`AssignmentFailureRemovesOriginalOperationAndNewOperationRecovers:652-678` 失败之后没有重放原 op。
  - 建议补 `IdempotentTicketFailureReplayReturnsServiceUnavailable`。
- **升档后的 COMPLETE 路径没有用例**：
  - 「升档后 END 只发广播」只在 ABORT 路径钉住了（`:1363-1367`、`:1402-1406`）。
  - 升档后 COMPLETE，即最后一回合 TURN 和 END 都走广播、TURN 在前，没有用例。`CompleteDecisionPublishesFinalTurnThenEndAndErasesRoom:1049-1080` 只测了中继。
  - 两条路径共用 `PublishSpectateTurn` / `PublishSpectateEnd`，风险低。
  - 建议补 `PromotedCompleteSendsFinalTurnThenEndOnBroadcastOnly`。
- **停机用例**：`ShutdownSendsNoEndFrameWhileOutcomeIsUndecided:1466-1474` 只断言不发 END、房间还在，没有断言准入已关（对应一般-2）。
- **夹具**：`TearDown` 用 `EraseAllRooms`（`:73-79`）直接删房，不经 `RetireObserver`。已准入观众的活墓碑会留在单例里跨用例累积。每个用例的 battle_id 不同，不会串，只是规模很小的泄漏，不影响判据。
- **`battle_spectate_relay_test.cpp`**：五条用例都对着出口的真实记录断言，不恒真。`:91` 的注释问题见核对表第 3 行。

---

## 五、其它（不计入发现，供拍板）

- **两版停机口径不同**：
  - Java 的 `beginShutdown`（`BattleRoomServiceImpl.java:317-329`，第 327 行）对没有待提交终局的房间推 166 ABORTED，并清掉观众。
  - C++ 一律不发（`.h:253-254`），依据是「唯有 Match 持久 ABORT 才可宣布作废」（`.cpp:1444`）。
  - 但对还在打的房间，match 只可能判 ABORT，不存在「与真实决定矛盾」。C++ 下，L1 / L2 观众 30 s 后收到 SOURCE_LOST，L0 观众只收到 FIN。
  - 两版的客户端可见行为不一致（AGENTS §12），而 §10 第 7 条仍标着「另议」，需要负责人定一个口径。
- **无记录时的缺省码两版不同**：C++ 回 kInvalidParameter，Java 回 SERVICE_UNAVAILABLE（`BattleRoomServiceImpl.java:414-416`）。两版各自的 match 都把它们当普通拒绝处理，暂无行为差异。
- **文档**：
  - §5.3、§8 仍按 6 s 手动窗口写（`battle-spectate-tiered-fanout.md:126`、`:205`）。
  - `battle_node.proto:117-119` 中 `AddObserverResponse` 的注释没写首次拒绝码，也没写满员专用码。
  - 按规则本次都未改。
