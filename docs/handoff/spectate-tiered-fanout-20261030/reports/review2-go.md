# R2-GO：观战分级扩容 Go 侧第二轮语义评审（FIX-GO 之后）

只读静态评审，未跑任何构建、测试或生成命令。基线 HEAD `993b6da9d`，看的是 2026-11-05 的工作区。路径前缀省略 `go/match/internal/`，其他目录另注。

评审期间其他会话还在改这些文件，行号可能继续漂移：
- `logic/gather.go`、`logic/global_match.go`：`createBattle` 改签名，没动观战调用点
- `authority/schema.go`：新增 `CheckCandidateIndexes`
- `authority/store.go`、`authority/direct_admission.go`、`authority/join_prepare.go`、`proto/match/match_authority.proto`：删 `preferred_region`
- `logic/global_spectate_lobby_native_test.go`：夹具同步删列
- `discovery/node_watcher.go`：改 protojson 解析，`EntryOfNode` 语义没变

`go/login/internal/logic/clientplayerlogin/entergamelogic.go` 的未提交改动只有一段注释。

**结论**：没有阻断问题。新发现 6 条，严重 1 条、一般 5 条。

FIX-GO 的 13 项里，11 项已修对，S4 和 G4 只修对一部分。65c69f4a9、2b8184428 和 10-31 删观战记录键都没有弄丢修正项。任务列出的 11 个「找不到」的用例全部还在，只是改了名或合并进别的用例，不需要重补；只有 S3 的生产接线没有用例。login 在 Bind 之前推的 tip 与 372caca25 之后的 gate 栅栏一致。

---

## 1. 修正项核对

| 项 | 结论 | 现在的代码与说明 |
|---|---|---|
| S1 | 已修对 | 常量 3780 s：`logic/spectate.go:52`，即 `MaxBattleSeconds` 3600 + `WorkLeaseMs` 120 + 60。档位 `spectate_tier.go:91`、中继 HASH `:213`、赛事局标记与索引 `:503-507`、活跃集合分界 `spectate.go:161-163` 都用它。65c69f4a9 删掉本地模式后，它从函数变成常量，口径没变。活跃集合的 score 是开局时刻，分界 = 开局 + 3780 s，不早于期限 + 60 s。 |
| S2 | 已修对，只覆盖 COMPLETE | 调用链：`logic/global_match.go:456`（他会话在改）→ `spectate.go:146-150` → `:127-140`。清理内容：摘活跃集合、L2 配额、赛事局索引；一条 DEL 删档位、HASH 和标记，三个键都在 `{id}` 槽，Redis Cluster 下合法。任务发不出只记日志。截止裁决的 ABORT（`authority/completion.go:464-502`）不走这里，后果见一般-3。 |
| S3 | 已修对 | 调用链：`logic/matcher.go:28` → `global_spectator.go:662-666`（一分钟一轮）→ `spectate.go:263-280`。活跃集合按 3780 s 分界清理，赛事局索引按到期清理。`resumeGlobalSpectator` 在 roomMissing 时摘索引（`global_spectator.go:624-627`）。生产接线没有用例，见 §2 第 8 条。 |
| S4 / C 严重-2 | 部分 | 按负责人口径落地的部分：三条判据（`global_spectator.go:503-519`、`authority/spectator.go:732-734`）；各入口共用一份判定，见 `:435-470`，包括排队 `:421-430`、组队 `logic/global_team_launch.go:26`、准入恢复 `logic/global_admission_recovery.go:35,118`、新观战 `:187-191` 和后台 `:573-575`；关闭另起 3 s 预算（`:451-461`）。判据只读成局与注册表的终态信号。实例真活着时，误关只剩设计已接受的僵尸实例情形。**缺口一**：(a) 只认 state 5，而正常完赛（COMPLETE）的成局停在 state 4，见严重-1。**缺口二**：新观众仍会在已崩溃或已换实例的场上建出原件，见一般-2。 |
| G1 | 已修对 | Attach 回 `broadcast=true` 时补 ZADD，超额也记（`spectate_tier.go:458-466`）。新占者失败要退还时先复读档位，读到 BROADCAST 或读不到都不退（`:354-356`、`:383-397`）。`:380-382` 的注释说「复读与 ZREM 之间的窗口由 Attach 补记收回」，这不准确：档位一旦是 BROADCAST，后来的观众走 `:479-480` 分支，不再 Attach。但这个窗口要求别的请求在一次 GET 与 ZREM 之间完成整个升档，实际走不到，不单列。 |
| G2 | 已修对 | 中继可用时先 `RejectSpectatorToLobby` 落结论，再升档，升档失败只记日志（`global_spectator.go:628-639`）。重放时补升档（`:342-350`）。battle 侧墓碑记首次拒绝码（`cpp/nodes/battle/logic/battle_room_manager.cpp:836-869`）。夹具 `logic/global_spectate_lobby_native_test.go:70-88` 已按墓碑语义改过。 |
| G3 | 已修对（观众路径） | `spectate_tier.go:350-359` 返回 rejectTip。`:470-478` 在房间不存在时拒绝这名观众，`global_spectator.go:286-290` 懒剔除，不管配额是谁占的。运营入口（`setspectatefeaturedlogic.go:71-78`）只撤标记，不摘活跃集合，也不退别人占的配额，注释写明这是取舍；下一名观众的 Add 或 Attach 会懒剔除。不单列。 |
| G4 | 部分 | 签票前查注册表只在 L1 / L2 准入的末尾（`spectate_tier.go:484-489`），覆盖已挂接的 gate 和 L2；注册表查不到不判（`spectate.go:252-258`）。L0 和新 gate 的 L1 不经过这道检查，见一般-2。 |
| G5 | 已修对 | 抢配额之前先 `SET spectate:promote_try:{id} NX EX 3`，拿不到计 throttled（`spectate_tier.go:328-338`）。这个键带 `{id}`，只有单键操作，在 Cluster 里同不同槽都没影响。`TestSpectateTierKeysShareBattleHashTag` 用的是真 CRC16（`logic/keys_test.go:18-49`）。 |
| G6 | 已修对 | 入口未开启、验签失败、带会话来源一律回 DENIED，不回 gRPC 错误（`setspectatefeaturedlogic.go:40-44,92-101`）。运营工具打印排查提示并以 1 退出（`cmd/spectatefeatured/main.go:56-67`）。`svc/servicecontext.go:80` 的注释已改。外层 `sessionInterceptor` 回的 PermissionDenied 仍在 `PublicationAdmission` 之内（`go/match/match_service.go:225-236`），fix-go §5 已列为残余。 |
| G7 | 已修对，缺启动核对 | 唯一写入点带 `created_ms`（`authority/spectator.go:256-258`）。翻页按 `(created_ms, 主键)`，满页才前进（`:764-771`）；请求头的行不删（`:778-785`）；DELETE 带 `created_ms<cutoff`，挡住旧请求重放后重新插入的行（`:793`）。不持锁删除的论证成立：state 5 不会回退，已存在的非请求头行不会因重放变回请求头。缺口：启动核对不检查这张表的清理索引，见一般-4。 |
| G8 | 已修对 | 见第 2 节。 |
| C 严重-1 | 已修对 | 未绑定会话专用的 builder：payload 的 player_id 留 0，信封照填（`go/login/internal/svc/gate_command.go:185-193,226-237`）。只有 Bind 写进 Kafka 成功才置 `bindSent`（`go/login/internal/logic/clientplayerlogin/entergamelogic.go:570-584`），`:392-403` 据此选择发送方式。用例：`gate_command_test.go:345-397`、`entergame_failure_notify_test.go:69-174`。 |

### 1.1 login 的 Bind 前 tip 与 gate 栅栏（372caca25 之后）

两边一致。有一点要先说清：gate 对显式 0 并不只在会话未绑定时才放行。

- **gate 对显式 0 一律放行**：`ClassifyPush(·, 0)` 不论会话是否已绑定都回 `kDeliverUnfenced`（`cpp/libs/services/gate/session/system/session_identity_fence.h:43-49`）。已有用例证明已绑定会话收 0 也会送达（`cpp/nodes/gate/tests/gate_push_delivery_test.cpp:355-372`）。另一个用例名 `UnboundSessionOnlyAcceptsTheExplicitZeroTarget`（`:376-387`）的意思是「未绑定会话只收显式 0」，不是「显式 0 只发给未绑定会话」。
- **login 的两种情形正依赖这一点**：
  - Bind 写 Kafka 报错、实际却已落地：按没发出处理，用 0（`entergamelogic.go:386-389`）。
  - 同一连接 LeaveGame 后换角：会话仍绑着旧角色（`cpp/nodes/gate/handler/event/scene_route_helper.h:138`），只有 0 能送达。

  以后如果把 gate 收紧成「0 只给未绑定会话」，这两种 tip 都会被丢，所以 gate 与 login 必须同批改。
- **372caca25 的认证绑定**：`cpp/nodes/gate/handler/event/authenticated_session_binding.h:9-35`、`gate_event_handler.cpp:275-279` 新增了几种 gate 拒绝 Bind 的条件：连接代次不符、身份字段缺失、`loginSucceeded` 为假、旧代次、同代身份冲突。这些都是旧连接或不完整身份。Bind 被拒之后，再推带真实 player_id 的 tip 会被栅栏丢掉，这正是想要的结果。合法的同连接 Bind 不会被拒：`loginSucceeded` 由 Login 回包按代次置位（`cpp/nodes/gate/handler/rpc/client_request_lane.cpp:311-314`、`rpc_reply_bridge.cpp:66-72`），EnterGame 一定在它之后。所以 login 以「Bind 已写进 Kafka」为界，在 0 和真实 player_id 之间切换，这条规则仍然成立。
- **可选改进（不计入发现）**：login 现在手里有会话代次 `state.fenceKey.Incarnation`，但 `PushToPlayerEvent` 没有代次字段，Bind 前的 tip 只能放开栅栏。如果给它加一个代次栅栏，就能把剩下的「同一 gate 实例内 session 序号回绕」也挡住。这要改 proto，并且 gate 要同批改。

---

## 2. 「找不到」的 11 个用例

11 个都还在：带 `TestWatchBattle*` 的改成了 `TestLobby*`，带 `TestGlobal*` 的改成了 `TestSpectate*` 或 `TestLongBattle*`，还有一个合并进了别的用例。每一个守的行为在现在的代码里都还在。我逐个核对过断言，都不恒真：把对应的修正去掉，断言会失败。

| # | 原名 | 现在的用例 | 结论 |
|---|---|---|---|
| 1 | TestWatchBattleAttachBroadcastReplyRecordsQuotaOverLimit | `TestLobbyAttachBroadcastReplyRecordsQuotaOverLimit`（`logic/spectate_tier_test.go:423`） | 覆盖。B=1，配额里预放了一场；断言补记后总数为 2，并且之后的抢占被挡住。 |
| 2 | TestSpectatePromoteTryKeySharesBattleHashTag | 合并进 `TestSpectateTierKeysShareBattleHashTag`（`:209-217`，第 215 行） | 覆盖。用的是真 CRC16。 |
| 3 | TestWatchBattlePromoteMissingRoomRejectsViewerAndEvicts | `TestLobbyPromoteMissingRoomRejectsViewerAndEvicts`（`:624-650`） | 覆盖。新占和他人已占两种情况都测了，断言回 16018、不签票、配额为 0、本场的键全部清掉。走的是 `admitGlobalLobby` 这一层；WatchBattle 外层只转发 closeTip，不必再补原生用例。 |
| 4 | TestWatchBattleRejectsTicketForReplacedInstance | `TestLobbyRejectsTicketForReplacedInstance`（`:665-682`），以及原生的 `TestGlobalWatchTiDBReplacedInstanceRejectedWithoutRPC`（`logic/global_spectate_lobby_native_test.go:781-801`） | 覆盖 RELAY 已挂接和 BROADCAST 两种档位。L0 没有对应用例，因为 L0 本身就没有这道检查，见一般-2。 |
| 5 | TestWatchBattleSignsTicketWhenInstanceNotInRegistry | `TestLobbySignsTicketWhenInstanceNotInRegistry`（`:685-697`） | 覆盖。如果把「查不到」误当成「已替换」，用例会失败。 |
| 6 | TestGlobalSpectateTTLCoversLongestBattle | `TestSpectateTTLCoversLongestBattle`（`:831-848`） | 覆盖。第 837 行的 `Greater` 是常量之间的比较，只在改常量时才会失败；对键 TTL 和赛事局索引到期时刻的断言是实测的。 |
| 7 | TestGlobalModeLongBattleSurvivesPastLocalTTL | `TestLongBattleSurvivesSpectateIndexMaintenance`（`:851-870`），以及原生的 `TestGlobalWatchTiDBLongBattleKeepsTierAndListingPastOldTTL`（native `:611-630`） | 覆盖。按旧的 360 s 分界，随机选场会把这场剔掉并退配额，档位键也会过期，两条用例都会失败。 |
| 8 | TestGlobalSpectateMaintenanceLoopPrunesIndexes | `TestSpectateMaintenanceLoopPrunesIndexes`（`:905-927`） | 循环本身覆盖了。生产接线 `logic/matcher.go:28` 没有用例：把这一行删掉，用例照样通过。低优先级：要补的话，把 `StartMatcherLoop` 里不依赖 SQL 的维护循环拆成一个函数，供用例直接调用。 |
| 9 | TestWatchBattlePromoteQuotaFullStaysRelay | `TestLobbyPromoteQuotaFullStaysRelay`（`:475-499`） | 覆盖。先快进过租约，名额空出后再升档。 |
| 10 | TestWatchBattlePromoteFailureReleasesQuota | `TestLobbyPromoteFailureReleasesQuota`（`:502-538`） | 覆盖 RPC 失败和 battle 拒绝两种情况。 |
| 11 | TestWatchBattleAttachBroadcastReplySyncsTier | `TestLobbyAttachBroadcastReplySyncsTier`（`:400-419`） | 覆盖，已带上配额断言（`:416-418`）。 |

**偏弱或测不到声称行为的用例**：
- `TestReleaseSpectateOnOutcomeSkipsWhenDraining`（`logic/spectate_tier_test.go:893-902`）：断言紧跟着一次异步发射。即使排空时任务照常发了出去，它多半也在断言之后才执行，所以这个用例测不出「排空时不该发任务」。建议在断言前加 `require.NoError(t, svcCtx.RelayAdmission.WaitIdle(ctx))`。
- `TestGlobalSpectatorClosesWithoutProofOnlyWhenSourceGone` 的「排队路径」子例传入的是 state 2 的原件，真实的捕获件是 state 3。两者走同一个 `cancelGlobalSpectator`，判据覆盖不受影响。
- 夹具的 `RemoveObserver`（native `:90-98`）对未知 op 不建墓碑，而真实 battle 会建（`battle_room_manager.cpp:1039-1046`），所以「Remove 先于 Add 到达」的时序没有被模拟。目前没有用例依赖这个时序，可以接受。

**需要补的条数**：这 11 个用例都不用补。新发现对应的回归用例写在第 3 节各条里。

---

## 3. 新发现

### 严重-1：COMPLETE 之后成局仍是 state 4，S4 的判据 (a) 认不出「房间已收尾」。实例随后消失时，L0 观众要被卡到原期限 + 60 s

**位置**
- `authority/spectator.go:732-734`：`SpectatorFormationEnded` 只认 `State == 5 || terminalAbortDue(DeadlineMs, stamp)`。
- `authority/completion.go:449`：COMPLETE 提交把成局写成 `state=4, retry_at_ms=terminalOutcomeNoRetryMs`，`deadline_ms` 不变。只有截止裁决的 ABORT（`:493`）和发房前失败（`authority/store.go` 的 `finish`）才写 5。
- 使用这个判定的地方：`logic/global_spectator.go:450-461`、`:503-519`、`:573-575`。
- 同样只看 state 的地方：`globalFormationWatchable`（`:369-371`）和 `loadSpectateBattle`（`logic/spectate.go:88-90`）。

**触发序列**（模板 1800 s）
1. T0 提交成局，期限为 T0 + 1920 s。玩家 P 在实例 I 上做 L0 观战，`match_spectator` 为 state 2。
2. T0 + 8 min 战斗正常结束，battle 调 `CommitBattleOutcome` 拿到 COMPLETE，成局变成 state 4、`retry_at_ms` 为 NoRetry，房间销毁。
3. P 那一行还没被恢复循环访问到，I 就退出了。可能是缩容摘掉了空闲节点、滚动发布，或者完赛后崩溃，同一个 node_id 上也没有新实例。恢复循环每个副本每 500 ms 至多发起 2 行（`:704-770`），在册行多时，一轮要几分钟。
4. P 发 JoinQueue。`closeCapturedSpectatorExit` 发 Remove，拨号失败或超时。`spectatorSourceGoneReason` 看到 state 4、期限未到、注册表查不到，判不出来，于是回「原观战退出尚待确认」（`logic/global_match.go:72-76`）。此时 WatchBattle 回 16004；后台每一轮都 Query 失败，并调用 HoldRelay（粘滞计数，`go/shared/drainproof/report.go:73-74`）。
5. 直到 T0 + 1980 s，判据 (b) 才成立，P 被卡了约 25 分钟。60 分钟模板的场最长接近 1 小时。
6. 这场 COMPLETE 之后，按 battle_id 进来的新观众（来自旧列表或分享链接；S2 已经把它摘出活跃集合）仍被判为可观战，于是走 L0 落下原件。由于 I 已不在，这名观众同样被卡到原期限 + 60 s。

**后果**：负责人接受的「最长约 1 小时」上限针对的是实例崩溃。这里是正常完赛加节点正常退出：match 手里已经有房间必已收尾的持久证据（COMPLETE 决定），却仍然按截止时间等。玩家在这段时间里排不了队、看不了别的战斗，也组不了队。
另一种情况不在本条范围内：滚动发布或缩容时被 `AbortAllRooms` 中止的在打房间没有 COMPLETE，仍要等期限 + 60 s。这属于负责人已接受的残余。

**最小修法**：`SpectatorFormationEnded` 增加一个条件：`formation.GetState() == 4 && formation.GetRetryAtMs() == terminalOutcomeNoRetryMs`，即终局已决定。写入这个值的只有 COMPLETE 提交（`completion.go:449`）和 `retireTerminalOutcomeSchedules`（`authority/ready_recovery.go:69`），两者都要求存在同一原件的终局行。把这条判据导出（例如 `authority.FormationOutcomeDecided`），也给 `globalFormationWatchable` 和 `loadSpectateBattle` 用：已决定终局的成局不可观战，列表、赛事局和运营入口读到时懒剔除。设计文档 §15.9 第一条补上「终局已决定」。

**回归用例**
- `TestSpectatorFormationEnded`：加一个 case，state 4、`RetryAtMs` 为 NoRetry、期限未到，期望 true。
- 原生 `TestGlobalSpectatorClosesWithoutProofOnlyWhenSourceGone`：加 case「终局已提交」，执行 `UPDATE match_formation SET retry_at_ms=9223372036854775807`，并设 instanceGone。期望三个入口都关闭，reason 为 `formation_ended`。
- `TestGlobalFormationWatchable`：加 COMPLETE 时为 false 的 case。
- 新增原生 `TestGlobalWatchTiDBCompletedBattleNotWatchable`：COMPLETE 之后按 battle_id 观战，期望回 16018、不写 `match_spectator`、不发 Add。

### 一般-2：L0 和新 gate 的 L1 没有 G4 的注册表核对。场的实例被替换后不剔索引；实例崩溃后，新观众会被卡住

**位置**
- `logic/global_spectator.go:202-232`：prepare 依次做 locate、选档位、BeginSpectator，中间不核对注册表。
- G4 的核对只在 `logic/spectate_tier.go:484-489`，位于 L1 / L2 准入末尾，排在 Attach 之后。
- battle 收到实例不符的 Add、Attach、Promote，回的是 `kInvalidParameter`（`cpp/nodes/battle/logic/battle_room_manager.cpp:819-823`、`:2318-2323`）。`spectateRejectVerdict`（`logic/spectate.go:217-226`）不把它当作房间不存在。

**触发序列 A：同一 node_id 换了实例**
1. battle 节点 7 重启，实例从 U1 换成 U2。场 B 原在 U1 上，成局仍是 state 4。
2. B 的活跃集合 score 还新（S1 之后最长保留约 63 分钟），列表也照常列出它，因为 `watchableSummary` 只看成局。
3. 观众 Q 点开 B。DIRECT 档下，BeginSpectator 落原件，Add 发到 U2，回 `kInvalidParameter`，结果是 16018；随后 Remove 回空包，按判据 (c) 无证明关闭。
4. 每个点进来的观众都走一遍「事务 + 两次 RPC + 无证明关闭」，而索引不剔除。B 一直留在列表和随机选场里，直到期限 + 60 s 判 ABORT、列表才剔除，或者 score 过期。
5. RELAY 档下新 gate 的观众同样：Attach 回 `kInvalidParameter`，结果 16018，不剔除。

**触发序列 B：实例崩溃，node_id 没有被复用**
1. 前面与 A 相同，区别在于 Add 的 RPC 直接失败，行进入 state 4。
2. 准入截止后转到 state 3，Remove 失败，三条判据都不成立。
3. Q 被卡到 B 的期限 + 60 s。

fix-go §6 把「崩溃实例上的 L0 观众」和「崩溃的场留在活跃集合里、随机选场可能选中」分别列为了残余。两条合在一起意味着：崩溃后约一小时内，任何点开这场或随机选中它的**新**观众都会被卡住，不只是崩溃时已在册的观众。

**后果**
- 序列 A：死场在列表和随机选场里最多滞留约 1 小时；随机观战的请求被永久关成 16018；`spectator_unproven_close_total` 被普通点击刷高，失去「实例崩溃信号」的意义。
- 序列 B：新观众最多被卡约 1 小时，期间不能排队、观战、组队。

**最小修法**
- 针对 A：把 G4 的注册表核对前移到 `locateGlobalWatch`（首次准入和 LOBBY 重放共用）。`battleInstanceReplaced(s, formation.TargetNodeId, formation.TargetInstanceId)` 成立时，调 `removeSpectateBattle` 并回 16018，不落原件、不发 RPC。`admitSpectateLobby` 末尾那道核对可以保留。
- 针对 B（需负责人定）：开新的 L0 原件之前，要求 `EntryOfNode` 查得到并且 uuid 相同；查不到就回 16004，不落原件。这对「开原件」是 fail-closed，与「关原件」时「查不到不算」方向相反，两边都偏安全；代价是 etcd 抖动时暂时开不了 L0。
- 注意：改完之后，现有 `TestGlobalSpectatorClosesWithoutProofOnlyWhenSourceGone` 里「同 node_id 已是别的实例 / 新观战」子例会改判成 16018，因为它的新场也在节点 7。要把新场挪到别的节点。

**回归用例**
- `TestGlobalWatchTiDBDirectTierReplacedInstanceEvictsWithoutAdd`：注册表登记为替换后的实例；期望 16018、`adds == 0`、本场的键被清掉。
- `TestGlobalWatchTiDBRelayNewGateReplacedInstanceEvicts`。
- 如果采纳 B 的修法，再加 `TestGlobalWatchTiDBUnregisteredNodeBeginsNoOriginal`。

### 一般-3：随机选场选中已判 ABORT 的场，请求被永久关成 16018，成员却不剔除

**位置**
- `logic/global_spectator.go:246-251`：成局缺失、非 state 4 或非公开时，locate 回 16018，但不清索引。
- `:192-208`：随机选场后 locate 被拒，直接 `closeUnpreparedGlobalWatch`，同一个 request_id 以后永远回 16018。
- `logic/spectate.go:169-203`：`pickRandomBattle` 只剔除 score 过期和非法的成员。
- 截止裁决 ABORT（`authority/completion.go:464-502`）不调 `releaseSpectateOnOutcome`。
- 对照：列表读到同一场时会调 `removeSpectateBattle`（`logic/listwatchablebattleslogic.go:119-128`）。

**触发序列**
1. 实例崩溃的场在期限 + 60 s 由到期终局恢复判 ABORT，成局变成 state 5。
2. 它在活跃集合里的 score 是开局时刻。S1 之后要到开局 + 3780 s 才过期：模板 180 s 的场 ABORT 后还会留约 57 分钟，1800 s 的约 30 分钟。
3. 随机观战选中它，locate 判不可观战，这次请求被关成 16018。客户端看到「该战斗当前无法观战」，而实际上还有活着的场。
4. 成员没有被剔除，下一次随机观战还可能选中它。

**后果**：崩溃之后一段时间里，随机观战会以「死场数 / 总场数」的概率失败。fix-go §6 第 5 条只写了「随机选场可能选中」，没写选中后不剔除、请求永久关闭。

**最小修法**：`locateGlobalWatch` 判到成局缺失或非 state 4 时调 `removeSpectateBattle`，判到非公开时调 `removeSpectateActiveMember`，与 `watchableSummary` 同一口径。严重-1 的「终局已决定」也走这里。可选：ABORT 提交之后也尽力调一次 `releaseSpectateOnOutcome`。

**回归用例**：`TestGlobalWatchTiDBRandomPickOfClosedFormationEvictsMember`。活跃集合里只有一场 state 5 的成局：第一次随机观战期望回 16018，并且该成员被摘掉；换一个 request_id 再观战，期望回 16017（无场可看）。

### 一般-4：启动时的索引核对不检查意图清理用的索引，旧库上 G7 的清理会退化成每分钟一次全表扫描

**位置**
- `authority/schema.go:42-93`：新增的 `CheckCandidateIndexes` 属于其他会话的未提交改动，它的表清单和 required 里都没有 `match_spectator_intent`。
- `proto/match/match_authority.proto:39`：声明了索引 `state,created_ms,player_id,request_high,request_low`。
- 迁移器对已有表缺普通索引只记 Warnings，不建（`go/schemamigrate/plan.go:312-313,363`）。
- 清理查询：`authority/spectator.go:807`。

**触发序列**
1. 10-30 之前建的库，`match_spectator_intent` 表已存在但没有这个索引。
2. 跑 `-migrate`，只 ADD COLUMN `created_ms`；缺索引只是一条警告。
3. `requireCurrentSchema` 和 `CheckCandidateIndexes` 都放行，服务开始接流。
4. 每个副本每分钟执行一次 `WHERE state=5 AND created_ms<? … ORDER BY … LIMIT 1000`，全表扫描加排序，预算 3 s（`logic/global_spectator.go:669,687`）。

**后果**：表大了以后每轮都超时。`round` 失败只记日志、不动游标，清理永远不前进，G7 要防的无界增长照旧发生，还额外多出每副本每分钟一次的全表扫描。fix-go §4.6 只在交付说明里建议「清库重建」，没有机械守卫。项目未上线，新库建表时会带上索引，所以定为一般。

**最小修法**：在 `CheckCandidateIndexes` 的 `table_name IN (...)` 和 required 里加上 `"match_spectator_intent": {"state,created_ms,player_id,request_high,request_low"}`。`schema.go` 归其他会话，改之前先协调。

**回归用例**：原生 `TestCheckCandidateIndexesRequiresSpectatorIntentPurgeIndex`：DROP 掉这个索引之后，期望 `CheckCandidateIndexes` 报错，拒绝接流。

### 一般-5：`cancelGlobalSpectator` 把「原件已关闭」当成「未获证明」。多副本恢复循环并发时会记一笔不清零的 HOLD（存量问题）

**位置**
- `logic/global_spectator.go:439-445`：第一次推进到 3 只要返回 false，就一律回 `errGlobalSpectatePending`。
- `authority/spectator.go:626-628`：推进到 3 时如果当前行已是 5，返回 false。推进到 3 返回 false 只有这一种可能，op 一致等前提已在 `:603` 核过。
- 对照：`rejectGlobalSpectator` 和 `rejectGlobalSpectatorToLobby`（`:529-532`、`:544-547`）都把 `!ok` 当作「已证实关闭」，回 nil。
- 后台恢复对任何错误都调 `HoldRelay`（`:760-761`），而 Hold 是粘滞的（`go/shared/drainproof/report.go:73-74`）。

**触发序列**
1. 副本 A 和 B 的恢复循环都扫全部观战行（`SpectatorsThrough` 不按副本分片）。
2. B 的 `SpectatorRecoveryCurrent` 读到某行还是 state 2 或 3。
3. A 先拿到证明，把这一行关到 5。
4. B 的 Query 或 Remove 回来，进入 `cancelGlobalSpectator`，第一次推进读到 5，返回 false，得到 `errGlobalSpectatePending`，于是 B 调 HoldRelay。

前台也有同样的情况：玩家发起新的 WatchBattle 清退旧原件时，旧原件恰好被后台关掉，玩家收到 16004，重试一次就好。

**后果**：没有任何实例崩溃，也会给排空证明记一笔永久 HOLD。在册的 L0 行越多、两个副本的扫描相位越接近，越容易撞上。这在 FIX-GO 之前就存在；S4 在 state 2 上新增的预判多了两次 SQL，窗口略宽了一点。

**最小修法**：`cancelGlobalSpectator` 第一次推进返回 false 时直接 `return nil`，并加注释说明「原 op 已证实关闭」，与 `rejectGlobalSpectator` 同一口径。第二次推进到 5 的 `!ok` 分支永远走不到，可以顺手删掉。

**回归用例**：原生 `TestCancelGlobalSpectatorTreatsAlreadyClosedAsDone`。先用正证明把原件关到 5，再拿 state 2 的旧快照调 `cancelGlobalSpectator`，期望回 nil，`RelayAdmission` 不记 Hold。

### 一般-6：观战列表逐条串行读 SQL，并且不受请求 ctx 约束（容量影响为 PLAUSIBLE）

**位置**
- `logic/listwatchablebattleslogic.go:61-90`、`:119-121`：赛事局和活跃成员每一条都调一次 `loadSpectateBattleFn`，串行执行。
- `logic/spectate.go:78-80`：`context.WithTimeout(context.Background(), 3s)`，不接请求的 ctx。
- 权威库连接池只有 16 条（`authority/schema.go:176`），撮合、观战原件事务、恢复循环和清理共用。

**触发序列**
1. TiDB 变慢，或列表请求量很大。
2. 每次列表请求串行做最多 limit 条赛事局加 limit 条活跃成员的点查，每次最长等 3 s，等池里的连接也算在内。
3. 客户端的 gRPC 截止（5 s）到了以后，服务端仍然跑完整个循环。

**后果**
- 确定会发生：客户端已取消的列表请求不会停下。
- PLAUSIBLE：高并发浏览列表时会挤占权威库连接池，拖慢撮合提交。缺压测数据，影响多大没有实测。10-31 删掉观战记录键之后，列表完全由 SQL 现算，这是那次改动带来的新负载路径。

**最小修法**：`loadSpectateBattleFn` 改成接收请求 ctx（`context.WithTimeout(l.ctx, 3s)`），并在循环里检查 ctx；或者用一次 `SELECT … WHERE battle_id IN (…)` 批量读出本页的成局。

**回归用例**：`TestListWatchableBattlesStopsWhenRequestCancelled`：用桩的 `loadSpectateBattleFn` 计数，ctx 取消之后不应再读。

---

## 4. 不计入发现的说明

- 其他会话正在改 `logic/gather.go` 和 `logic/global_match.go`，把 `createBattle` 的返回值改成了两个。这属于编译层面的问题，Codex 会查到，不在本评审范围。观战相关的调用点没有变化：可见性冻结在 `gather.go:194`，入活跃集合在 `gather.go:272`，释放在 `global_match.go:456`。
- 指标标签都是低基数的固定取值：`watch_battle_total{outcome}`、`spectate_tier_total{tier}`、`spectate_promote_total{result}`、`spectator_unproven_close_total{reason}` 和不带标签的 `spectator_intent_purged_total`（`metrics/metrics.go:82-124,386-442`）。没有 player_id，也没有 battle_id。
- 不变量：battle_id 只作为 ZSET 成员和 Lua 的字符串参数，不进分数；意图清理按时间列 `created_ms` 翻页，player_id 只用来打破并列，不用来判断先后。观战的各个事务（BeginSpectator、advance、意图、清理）里都没有 RPC，也没有跨库 SQL。符合 AGENTS §7 第 1、6 条。
