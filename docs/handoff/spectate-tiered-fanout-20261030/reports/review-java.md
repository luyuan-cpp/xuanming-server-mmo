# Java 版观战中继 / 广播：语义评审（2026-10-30）

范围：Java 仓 `D:\luyuan\xuanming-server-mmo`（HEAD `cab6ef77` 加工作区）的 P0 剩余与 P1 / P2 改动，对照 `docs/design/battle-spectate-tiered-fanout.md` §15。C++ / Go 评审查出的 8 类问题逐条核对，结论见文末对照表。

口径：全部是静态阅读，未编译、未运行、未压测。Redisson 行为读的是本机 `redisson-3.50.0.jar` 的字节码（`javap -c`，只看标准输出）和仓库自己的说明。文中的发生概率与容量后果都是推得的，没有实测。

路径相对 Java 仓，`.../` 省略 `src/main/java/com/game/<模块>/`。

**结论**：没有阻断，严重 3 条（1 条存量）、一般 6 条。协议字节、两条水位、`ByteBuf` 引用计数、线程模型、订阅退订竞态都没有问题。最要紧的一条：Java 的「单频道有序」只在 Redis 到 gate 这一段成立，battle 发布这一段不成立，每场结束都可能丢最后一回合。

---

## 严重

### 严重-1：END 可能越过最后一回合 TURN（Redisson 连接池打乱发布顺序），观众收到 166 却看不到最后一回合

- **位置**
  - 发布：`game-discovery/.../spectate/SpectateFrameChannel.java:48-56`（SET 与 SPUBLISH 同时发出，不等前一条）、`game-battle/.../port/RedisSpectateFrameSink.java:39-47`（帧与帧之间也不等）。
  - 同一调用栈连发：`game-battle/.../room/BattleRoomServiceImpl.java:934-937`（普通局）、`:1077-1078`（确认模式），TURN 在 `:998-1000` 发布，END 在 `:1137-1139` 发布。
  - 丢弃：`game-gate/.../spectate/SpectateHub.java:325-327`（已收 END 之后一律丢弃）、`:378-382`。
  - 依据：`game-discovery/.../RedisProperties.java:20` 与 `docs/design/architecture.md:1337` 都写明「一条命令从借到连接起占着它，应答回来才归还」；字节码里 `RedisExecutor.lambda$execute$5` 在本次尝试完成时才 `releaseConnection`。所以紧挨着的异步命令走不同的连接，各连接又在各自的 Netty 线程上写出，Redis 按 socket 可读的先后执行。
- **触发序列**（任一打开了中继的场次打完最后一回合）
  1. 逻辑线程在同一个任务里调 `publish(TURN r)`、`publish(END)`，发出 `SET TURN`、`SPUBLISH TURN`、`SET END`、`SPUBLISH END`，分别落在四条连接上，前后相隔微秒级。
  2. Redis 先执行 `SPUBLISH END`。
  3. 所有订了这一场的 gate 都先收到 END：`finish` 扇出 166、清订阅、退频道。
  4. TURN(r) 随后到达（或已退订收不到），`:325` 丢弃。
- **后果**
  - 这场在所有 gate 上的大厅档观众都看不到最后一回合。`SpectateEndS2C` 只有 `battle_id / outcome / reason`，不带状态，这不是「下一帧自愈」。
  - `SET` 同样会乱序：`SET TURN` 排到 `SET END` 之后时，最新帧键停在 TURN(r) 直到 TTL。没有墓碑的 gate 上，迟到的订阅者先收 161，30～35 秒后收 `SOURCE_LOST`，收不到真正的 166。
  - `SpectateFrameChannel.java:42` 与 `SpectateHub.java:51` 承诺的「先写键后发布、不漏帧」也不成立：`SPUBLISH` 排到 `SET` 之前时，恰好夹在中间订阅的 gate 会漏这一帧。漏的是 END 就走 `SOURCE_LOST`。
  - 与 C++ 严重-2 的后果相同，成因不同：C++ 是两条 topic 之间没有顺序，Java 是同一频道的发布端没有顺序。
- **测试缺口**：`SpectateRelayRoomTest.java:171-187` 只断言调用端口的先后；`SpectateHubTest` 用同步假频道，没有 END 先于 TURN 的用例；`SpectateFrameChannelIntegrationTest.java:51-74` 每发一帧都 `.get()` 等完；`RedisSpectateFrameSinkTest.java:11-18` 只测 TTL。
- **最小修法**：发布端按场串行。房间持有一个 `CompletionStage` 尾巴（只在逻辑线程读写，随房间回收，不用全局表），上一帧完成（失败也算完成）才发下一帧；`SpectateFrameChannel.publish` 内部改成 SET 完成后再 SPUBLISH，SET 失败也继续发布。逻辑线程仍不阻塞，后续命令在 Redisson 回调线程上发出。回归用例：假频道让 TURN 的 future 悬着，断言 END 在它完成之前没有发出；TURN 失败时 END 照发。

### 严重-2：单台 gate 同时观看的场次有约 250 的硬上限，超过后观众空等 30 秒再收 SOURCE_LOST

- **位置**：`game-discovery/.../RedisAutoConfiguration.java:45-57`（没设 `subscriptionsPerConnection`）、`RedisProperties.java:45`（订阅池 50）；Redisson 缺省 `subscriptionsPerConnection = 5`（`BaseConfig` 构造器，`PubSubConnectionEntry` 按它计数）；`SpectateFrameChannel.java:64-74`（一场一个频道）；`SpectateHub.java:281-286`（失败只打 WARN，不重试）。全仓没有 `SubscriptionsPerConnection` 的设置。
- **触发序列**
  1. 一台 gate 上有本机观众的场次（含 linger 30 秒的）累计占满 50 × 5 = 250 个频道，再扣掉同一客户端的其它订阅（如 `GateNode.java:233` 的推送频道）。
  2. 下一场的 `addListenerAsync` 拿不到连接，按 `RedisProperties.java:61-62` 的 1 次重试、200 ms 间隔收场（上限 `subscriptionTimeout` 7.5 秒），以 `RedisTimeoutException` 失败（`PublishSubscribeService.trySubscribe` 的报错就是提示调大这两个参数）。
  3. 条目留着但没有频道，订阅者收不到帧，空闲巡检在 30～35 秒后发 `SOURCE_LOST` 并删除条目。客户端再请求，只要没有空位就再来一轮。
- **后果**：Java 把中继档与广播档合成一种机制的依据是「只有有本机观众的 gate 才订阅，不需要全服配额」（`SpectateFrameChannel.java:17-18`、`PARITY.md` 的 P1 行），但实际多了这个按进程的上限，且只有 WARN，没有指标（`game.gate.spectate.entries` 把没有频道的条目也算进去）。什么时候够得到：中继开关不只开在热门房间，节点观众席配额满时（`BattleRoomServiceImpl.java:462-468`）这个节点上所有公开局都会转中继，随机观战又把观众撒在各场。未实测。
- **测试缺口**：`SpectateFrameChannelIntegrationTest` 只订一个频道。
- **最小修法**：`RedisProperties` 增加 `subscriptionsPerConnection`，`configure` 写进 Redisson，gate 配到与目标容量匹配的值（例如 500，上限变成 25000 个频道，连接数反而减少）；同步 `docs/design/architecture.md` §6.1 的连接总账。`onChannelAttached` 失败时加计数指标，并立即给订阅者发 `SOURCE_LOST`，不让他们空等。补一条真 Redis 用例：按生产配置订 300 个频道都能收到帧。

### 严重-3（存量，非本批引入）：battle 实例换掉后，全局模式的观战原件永远关不掉，玩家不能观战、不能排队

只在 `game.match.global.enabled=true` 时存在（`game-match/.../spectate/SpectateStoreConfiguration.java:31-37`；yaml 与 compose 缺省 false）。`SqlSpectateStore.java` 本批没有改动。

- **位置**
  - 恢复循环：`game-match/.../spectate/SqlSpectateStore.java:78-94`。
  - 关闭判定：`game-match/.../global/GlobalWatchAuthority.java:283-294`，`:289` 只认带原 op 与原实例的 `cancelled`，否则 `:293` 只把 `retry_ms` 推后 1 秒。
  - 新实例的回应：`BattleRoomServiceImpl.java:520-521`（实例不符回空应答，判据在 `ObserverOperationLedger.java:37`）、`:143`（Query 同样回空）。
  - 阻塞点：`GlobalWatchService.java:72-73` 经 `SqlSpectateStore.java:36-40`（新观战回 16004）；`GlobalMatchService.java:50-54`、`:220` 经 `SqlSpectateStore.java:41-44` 与 `GlobalMatchStore.java:163-166`（排队回 16004）；`game-player-store/.../RolePendingFence.java:71`（交易冻结、角色操作）；`GlobalMatchStore.java:53` 经 `GlobalMatchService.java:83,87`、`MatchLifecycle.java:168`（排空永远不为 0）。
- **触发序列**
  1. 玩家 O 是实例 I1 上的 L0 观众，行 state=3；或刚 L0 满员转 LOBBY，行 state=4 等 Remove。
  2. I1 崩溃或被替换。
  3. 恢复循环 `lease` 把行留在 3，到 `expires_ms`（360 秒）后转 4（`GlobalWatchAuthority.java:280`）。Query 调不通或新实例回空，走 `SqlSpectateStore.java:86-87`。
  4. state=4 发 Remove：死端点没有回包，或新实例回空应答，`cancelled=false`，行不动。没有 TTL，也没有别的关闭路径（`global_match_tables.proto` 的 `expires_ms` 注释写明不能作为终局证明）。
  5. O 发 WatchBattle 或 JoinQueue，一律 16004，换 request_id 也一样。
- **后果**：崩溃时在册的 L0 观众全部卡死，只能人工改库。这些行一直参与恢复扫描，恢复线程只有 2 条（`SqlSpectateStore.java:21`），每行调死端点耗时 1.5～2.5 秒（`:81`、`:91`），行多时拖慢其它行的后台收尾。维护排空也被它们挡住。
- **测试缺口**：`SqlSpectateStore` 只在 `GlobalWatchServiceTest`、`MatchLifecycleTest` 里以替身出现，没有「Remove 回空应答」的用例。
- **最小修法**（与 `specs/97-fix-go.md` 第 4 条同口径，负责人已授权放宽）：`settled` 拿不到证明时，在同一笔 P 锁事务里满足任一条件就 `closed` 并置 5：(a) `match_global_formation.state == 5`；(b) SQL 时间已过 `deadline_ms + MatchBudgets.TERMINAL_OUTCOME_GRACE_MS`；(c) battle 目录里同 node_id 已是别的实例（`DirectPlacementDialer.java:175` 的 `nodes.lookup`，查不到不算，放在事务外查好传进来）。理由写进注释：观众对战斗状态零写权，Add 带 `target_instance_id`，新实例不接纳旧 op。三条前台路径都经 `drive` → `settled`，不必分别改；`closed` 置 `cancelled=1`，`ObserverExitAuthority.provedClosed`（`:76-80`）与 `cancelExit`（`GlobalWatchAuthority.java:190`）随之放行。回归用例：Remove 回空应答且满足 (a) / (b) / (c) 时关闭，都不满足时不关。

---

## 一般

### 一般-1：L0 满员转 LOBBY 不是一笔事务，同 UUID 重放拿到 16018，battle 的同 op 重放也改了码

- **位置**：`game-match/.../spectate/GlobalWatchService.java:100`、`:104-105`、`:59`、`:155-161`；`GlobalWatchAuthority.java:158-164`（`observed` 只改状态）、`:95-101`（`respond` 另一笔事务）、`:60-64`（`closed` 置 `cancelled`）；`BattleRoomServiceImpl.java:414`、`:454-458`；`ObserverOperationLedger.java:47-52`；`game-match/.../spectate/SpectateRelay.java:39-41`、`:116-119`。
- **触发序列**
  - A：Add 回满员 → `observed` 提交 state=4 → `escalate` 成功 → `respond` 失败（SQL 抖动，或 Add 与 Attach 用完了预算）→ 客户端收 16004 → 恢复循环约 1 秒后用 Remove 关闭，置 `cancelled=1` → 同 UUID 重试走 `:59`，把 `WATCH_NOT_WATCHABLE`（16018）写成永久回执。
  - B：恢复循环还没跑到时重试：`prepare` 见 state=4 抛异常，回 16004，之后落到 A。
  - C：`observed` 本身失败，行仍是 state=2 → 重试时 `prepare` 取回原 Add 重发 → 账本墓碑让 `begin` 回 false，battle 回 1003（首次回的是 16021）→ `rejectionOf` 映射成 16018 并持久化。这就是「battle 重放拒绝码」，C++ 回 1005，Java 回 1003，同类问题。
  - D：Attach 超时（Add 已用掉大半预算）→ `Unavailable` 被 `SpectateRelay` 当成确定结论 → `settle` 把 16018 持久化。同一 UUID 永远是这个结果，而 Attach 本身是幂等、可重试的。
- **后果**：契约要求这次请求以 LOBBY 结案（§15.6），观众却看到「该战斗当前无法观战」，重放也一样。换 request_id 能恢复，不卡死。窗口窄，但恰好出现在满员且系统繁忙的时候。
- **测试缺口**：`GlobalWatchServiceTest.java:329-343` 用替身 `authority`，走不到真实的状态推进和重放。
- **最小修法**
  1. 中继可用且 Add 回满员时，`observed` 在同一笔事务里写入 LOBBY 回执（同 `specs/97-fix-go.md` 第 6 条的顺序），之后的准入与 `replayLobby` 同路。
  2. `Unavailable` 不持久化，回 16004，留给同 UUID 重试。
  3. `ObserverOperationLedger.Entry` 记下首次拒绝码，同 op 重放回原码（C++ 同批）。

### 一般-2：已是中继档的路径不复核实例，继续给死实例签凭证

- **位置**：`SpectateRelay.java:91-93`；调用点 `WatchBattleService.java:294-296`、`GlobalWatchService.java:89`；期限 `game-match/.../spectate/RedissonSpectateTierStore.java:57`。
- **触发序列**：battle 节点崩溃或重启 → 档位键还在 → 每名新观众都不调 battle，凭证写旧实例 → gate 等不到帧，30～35 秒后 `SOURCE_LOST` → 客户端再请求，再拿旧实例。持续到档位键过期（升档起最多 360 秒），全局模式下成局作废（期限 + 60 秒）后也会停。
- **后果**：观众反复空等，不碰资产。Java 没有配额，所以比 Go 轻，有上界。
- **最小修法**：签票前查 battle 目录（同 `specs/97-fix-go.md` 第 8 条）：同 node_id 已是别的实例就按房间不存在处理，查不到不判。这条路径量大，目录读要加几秒的本地缓存，不增加 RPC。

### 一般-3：pub/sub 丢帧后不看最新帧键，真实终局被 SOURCE_LOST 盖掉

- **位置**：`SpectateHub.java:510-513`、`:517-530`；`BattleRoomServiceImpl.java:1137-1139`（END 也写进最新帧键）。
- **触发序列**
  - 丢 END：gate 的订阅连接断开（Redis 重启、网络抖动）时 battle 发了 END，Redisson 重连并重新订阅，这一帧已丢。键里明明是 END，hub 不读，30～35 秒后发 `outcome=ONGOING`、`reason=SOURCE_LOST`。
  - 丢最后一回合：TURN(r) 丢了而 END 到了，观众同样看不到最后一回合。键已被 END 覆盖，没有地方补。
- **后果**：回合中途丢 TURN 能自愈（下一帧带全量状态），丢的是这两帧才有影响。只在订阅连接断开时发生，频率低。
- **最小修法**：`sweep` 判定源站失联前先 `readState` 一次，读到同实例的 END 就按真实终局收尾，读不到再发 `SOURCE_LOST`。丢最后一回合这一半不补，写进设计文档 §15.7 作为至多一次通道的已知代价。

### 一般-4：`match_global_watch_request` 没有保留期，大厅档观众每个新 request_id 永久留一行

- **位置**：`GlobalWatchAuthority.java:78-84`（插入）；`game-match` 与 `game-common` 里都没有针对它的 `DELETE`。表定义 `global_match_tables.proto:133`。
- **触发序列**：万人观战的场，每人每次新发起观战各加一行（请求原文 + 回执）。`head` 只保留最新，历史行不删。
- **后果**：存储无界增长。这张表原本面向每房 20 席的低频 L0，大厅档把写入量放大了。
- **最小修法**（同 `specs/97-fix-go.md` 第 11 条）：低频任务分批删除 `created_ms` 超过 7 天、不是 head、已有回执或已取消的行，每轮有上限，幂等。必须跳过仍被引用的行：`match_global_watching` 里 `state<>5` 的 `operation_id`（`closed` 里的 `history` 要读它），以及未完结 Join 的退出原件（`ObserverExitAuthority.java:77-78` 缺行会抛异常）。

### 一般-5：本地路径每名大厅档观众打一行 INFO

- **位置**：`WatchBattleService.java:427`。L0 成功的同类日志（`:591`）被每房 20 席封顶，大厅档没有上限。
- **触发序列**：热门场开播，大量观众同时发 163，每人一行 `观战转大厅档 player=... battle_id=...`。
- **后果**：日志量随观众数线性增长，正好在峰值时。指标 `game.match.watch.battle{outcome=lobby}` 已能看趋势。
- **最小修法**：降为 DEBUG，或第一条必打、此后每 1024 条一行（同 gate 其它丢弃日志）。

### 一般-6：关键行为没有测试能覆盖

除上面各条的测试缺口外：

- 没有端到端：robot 不会 `SubscribeSpectate`（`reports/java-p1.md` 未完成第 1 条），battle 真实产出的帧没有驱动过 hub，hub 到真实 Redisson 的路径也只有频道层的单帧用例。
- 没有「最后一个订阅者退订后立刻重新订阅」的真 Redis 用例。代码读下来没有竞态（见下表），但这是真实依赖的行为。

最小修法：`SpectateFrameChannelIntegrationTest` 补退订后立刻重订；在真实发布顺序修好之后，补一条连发 TURN + END 的真 Redis 用例，循环多次断言到达顺序。

### 未单列的小项

都是确定的，但影响很小，顺手改即可。

- `SpectateHub.java:406-416`：`finish` 不清 `latest` / `firstFrame`，墓碑多占一两帧内存 10 分钟（C++ 会清）。
- `SpectateHub.java:251-253`、`:291`：END 之后实例变化且上一次 attach 还在途时，第一个订阅句柄会被覆盖而不退订，要到进程退出才释放。要求实例在 END 后变化，极少发生。

---

## `GAME_MATCH_SPECTATE_RELAY_ENABLED` 默认打开的改动点

**必须一起改**，只改 yaml 会让 `MatchPropertiesTest.java:61-84` 失败（它断言 yaml 与代码缺省逐项相同，`:84` 比较 `spectate()`）：

| 位置 | 现状 | 改成 |
|---|---|---|
| `game-match/src/main/resources/application.yaml:138` | `:false` | `:true` |
| `game-match/.../MatchProperties.java:277` | `relayEnabled != null && relayEnabled` | `relayEnabled == null \|\| relayEnabled` |
| `game-match/.../MatchProperties.java:260` | 注释写缺省 false | 同步 |
| `deploy/compose/stack.yaml:216` | `:-false`（显式环境变量，会盖过 yaml） | `:-true` |
| `deploy/compose/env.example:62-63` | 注释写缺省 false | 同步 |
| `tools/local/start-slice.sh:16` | 注释写「大厅档缺省关」 | 同步 |
| `game-match/src/test/.../spectate/SpectateFeaturedServiceTest.java:155` | 断言 `isFalse` | 改断言 |
| `game-match/src/test/.../MatchApplicationContextTest.java:429-437` | 用例名与第一句写「缺省关闭」 | 断言仍会过（test 模式空密钥按关闭），名字和说明要改 |
| `PARITY.md` P1 行、`reports/java-p1.md` 待拍板第 3 条 | | 登记结论 |

**打开之后的行为变化**

- `game.run-mode` 缺省 prod（`application.yaml:68`），没配 `GAME_SPECTATE_TOKEN_SECRET` 的 match 会拒启（`SpectateRelayConfiguration.java:49-52`）。compose（`stack.yaml:215`）、切片脚本、`ReleasePolicy.java:18` 已经带这把密钥；dev / test 空密钥仍按关闭处理并告警。
- `MatchSkeletonContextTest`、`MatchRpcLoopbackTest` 用 test 模式，不受影响。其余上下文用例以 Codex 全量测试为准。
- Java 没有 Kafka 集群边界的问题，前提是 battle、gate、match 共用同一个 Redis 且版本不低于 7（`deploy/compose/infra.yaml:60` 是 8.10.2）。

**建议**：先修严重-1 和严重-2 再打开。默认打开后，这两条从「开了才有」变成所有部署都有。

---

## 逐条对照 C++ / Go 的问题

| 问题 | Java 结论 | 依据 |
|---|---|---|
| (1) STATE 推进 TURN 水位；缓存往回换 | 没有 | `SpectateHub.java:352-377` 两条水位。回合语义对得上：STATE 取快照回合（`BattleRoomServiceImpl.java:595`），确认模式取 `visibleBefore`（`:915`、`:1022`、`:1184`），TURN 取 `round_index`（`:999`）。用例 `SpectateHubTest.java:316-355`、`SpectateRelayRoomTest.java:143-147` |
| (2) END 抢占没发完的回合 | 没有 | `fanOut`（`:474-493`）一帧同步写完，没有分批，`finish` 排在后面；hub 单线程写入 EventLoop 是 FIFO；END 不看软水位 |
| (2) 多通道 END 越过 TURN | 没有多通道，但有同类问题 | 见严重-1。Redis 到监听器这一段有序：`Config.keepPubSubOrder` 缺省 true，`CommandPubSubDecoder.enqueueMessage` 按频道排队 |
| (2) pub/sub 丢消息 | 中途丢帧自愈，终局前后丢帧有代价 | 见一般-3 |
| (3) 合并饿死表尾 | 不适用 | 没有合并 |
| (3) 回选角 / 换角色不摘订阅 | 没有 | `ClientDispatcher.java:893-897` → `:1457-1479` → `:1482-1485`。打开的会话不存在 `playerId≠0` 且 `sceneNodeId=0` 的状态：`enterScene`（`:835`）同时写两者，`failEnter`（`:867-872`）一起清，被踢与链路断开（`:1377`、`:1435`）都关闭连接。用例 `ClientSpectateRequestTest.java:194` |
| (3) 关闭钩子覆盖不全 | 没有 | 所有断开都经 `channelInactive` → `onDisconnected`（`:432`）；同一 EventLoop 上 subscribe 与 drop 按 FIFO 进 hub |
| (4) TTL 与战斗期限脱节 | 目前不触发 | Java 全局与本地期限都是 300 秒（`GlobalMatchStore.java:456`、`GatherPipeline.java:265`），TTL 与过期分界 360 秒（`MatchBudgets.java:82`、`:107`），game-match 里没有 `time_limit`。以后移植模板时长时，`RedissonSpectateTierStore.java:57`、`SpectateRules.staleCutoff`、`WATCHING_TTL_SECONDS` 要一起改。最新帧键已按真实期限算（`RedisSpectateFrameSink.java:54-56`） |
| (5) 结束后键不释放；索引无界 | 没有 | 没有配额。档位键靠 TTL；赛事局 ZSET 读时过滤、标记时清理，只随运营操作增长；gate 墓碑 10 分钟；全局列表走 SQL 按 `created_ms` 过滤。结束的场留在列表到分界，点进去收 166（`reports/java-p1.md` 未完成第 6 条，非缺陷） |
| (6) 实例换掉后原件关不掉 | 有 | 见严重-3 |
| (7) 已挂接路径不复核实例 | 有，有上界 | 见一般-2 |
| (7) L0 满员转 LOBBY 重放错码；battle 重放拒绝码 | 有 | 见一般-1 |
| (8) 历史行无保留期 | 有 | 见一般-4 |

## Java 特有项

- **`ByteBuf` 引用计数：未发现问题。** `fanOut` 每条连接一份 `retainedDuplicate`，原件在 `finally` 释放；`ClientSession.writeEncodedBelow`（`:176-184`）跳过时释放，写出后归 Netty，连接已关或 EventLoop 已停时 Netty 也会释放；`writeOne` 所有权移交。`ClientFrameEncoder` 只匹配 `Message`，`ByteBuf` 原样过，线上字节相同。battle 侧 `EncodedFrame` 与 `pushBattleFrameToAll` 的 `finally` 同样正确。
- **跨 EventLoop：未发现问题。** hub 状态只在 `gate-spectate` 单线程上读写；写连接只用 Netty 线程安全的 API，不碰会话状态；软水位读的是 volatile 的待写字节数，跨线程提交时已计入。
- **订阅 / 退订竞态：未发现问题。** `PublishSubscribeService` 的 `subscribe` 与 `removeListenerAsync` 用同一把按频道的信号量（`getSemaphore`），`hasListeners` 为假才发 SUNSUBSCRIBE，先后两种顺序都安全。hub 只在 attach 完成后才退订，attach 期间条目被删时由 `onChannelAttached`（`:287-289`）补退。退订与重订之间漏的帧由「先订后读键」补上（前提见严重-1）。linger 由单线程 `sweep` 判定，不与订阅并发。
- **回调线程阻塞：没有。** Redisson 回调只 `post` 到无界单线程池（`SpectateHub.java:277-278`，没有 CallerRuns）；hub 线程与 battle 逻辑线程上发起的都是异步命令。
- **凭证字节级一致：未发现问题。** 签名对象都是 payload 原始字节，小写 hex 的 ASCII，密钥取原值（`SpectateTickets.java:62-63`、`:85`、`:110-112`；Go `spectate_ticket.go:53-59`；C++ `gate_spectate_ticket.cpp:55-56`）。到期判据都是 `expire_at_ms <= now`，拒绝集合相同。`SpectateTicketsTest` 的固定向量我用 Python 独立算过一遍，结果相同（`81e49d08…b0d8`）。
- **`spectate_public`：未发现问题。** 写入点只有 `GlobalMatchStore.java:778`、`:807`，都与 `create_request` 同一笔事务；之后的写都基于 `FOR UPDATE` 读出的行，不会被旧快照覆盖；缺省 false 即不可见；只有列表、随机、采样三处读（`GlobalWatchAuthority.java:35`），与新索引匹配；提交点还按原件复核（`:149`）。`syncAll` 只核验结构，旧库缺列会拒启，按「未上线、schema 直接改」可接受。
- **§15.2 客户端可见部分：未发现不一致。** L0 带 DIRECT、LOBBY 带 ticket 且无 op id（与 Go `global_spectator.go:121-122` 相同）；凭证被拒与解析失败都回带请求 id 的信封（与 C++ `gate_spectate_client.cpp:89`、`:113` 相同）；回执先于 161；合成 161 的 `observer_count=0`；合成 166 为 `ONGOING` + `SOURCE_LOST`（与 C++ `gate_spectate_hub.cpp:315-316` 相同）；消息号 351 / 352 与 16024 两版一致。

## 未覆盖

没有运行任何构建或测试。deploy 脚本与 robot 场景只看了和默认开关相关的部分。工作区里其他会话的改动（login、scene、trade 等）不在范围内。
