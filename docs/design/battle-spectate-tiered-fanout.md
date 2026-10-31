# 观战分级扩容：直连席 → gate 中继 → 全服广播（提案）

> 状态：**决策已定，分级本身未落码**（2026-10-23）。§12 / §13 已按负责人授权、以标准做法拍板；§10 的现存缺陷修复进行中，进展见 §10 末尾；P1 / P2 按 §11 逐阶段落码。
> **2026-10-30**：负责人授权把服务端按标准做法全部做完。P1 / P2 的落码契约见 §15，与前面草案冲突时以 §15 为准；B 批、C 批与 P0 剩余部分正在落码。
> 依据：本仓 C++ / Go 与 Java 版只读调研，三份独立方案，一轮对抗评审，再对其中 31 条代码事实做第二轮只读核实（结论见 §14）。
> 全部是静态阅读结论：**未编译、未运行、未压测**。帧大小与 gate 扇出上限都没有实测，§8 的数字只是量级估算。
> 现有观战的设计见 [turn-based-battle-server.md](turn-based-battle-server.md) §10（观战）、§18（直连）、§22（直连收缩 D65–D75）。本文的候选决策从 D94 起编号。
> 行号取自 2026-10-23 的工作区。`battle_room_manager.cpp` 当时有其他会话未提交的改动，约 1617 行以后的行号比 HEAD 晚 6 行左右。

## 0. 一句话结论

观众按人数分三档投递，**档位在观众进场时定下，之后不迁移**；一场战斗只会升档，不会降档：

- **L0 直连席**：前 K 个观众（默认 20，改为可配）维持现状，凭票直连 battle，延迟最低。
- **L1 gate 中继**：直连席坐满后，battle 每回合**只给有观众的那几台 gate 各发 1 条** Kafka，gate 编码一次，再分批扇出给本机观众。
- **L2 全服广播**：运营标记的赛事局，或有观众的 gate 已经覆盖大半个服时，battle 每回合只往一条广播 topic 写 1 条，所有 gate 都读，各自按本机订阅表扇出。L2 受全服并发配额限制。

人少的场次只会停在 L0 / L1，不给全服 gate 增加负担，这就是「区分对待、慢慢提升」。

## 1. 现状为什么扩不上去

| 位置 | 现状 | 人多时会怎样 |
|---|---|---|
| battle 房间 | `kMaxObserversPerRoom = 20`（`cpp/nodes/battle/logic/battle_room_manager.cpp:70`）；满员回 `kRateLimitExceeded`（`:1084`），与「全节点墓碑表满」同码（`:982`） | 第 21 人起被拒；满员还会被误认成限流 |
| battle 推送 | 观众版帧只脱敏一次，但每个观众都要各自序列化、加帧头、算 adler32、send；全部跑在一条 loop 线程上（`:1783-1808`、`battle_client_edge.cpp:82-94`） | 成本随观众数线性增长，全压在源站的单线程上 |
| battle 连接 | 观众和参战者共用 `battle_max_connections`（本地 4096），观众没有单独配额（turn-based §18.7） | 热门局的观众会挤占参战者的连接 |
| match 准入（生产＝全局模式） | 生产强制全局模式（`go/match/internal/svc/servicecontext.go:160`）。每个观众约 5 个事务；`BeginSpectator` 尾段对 `match_formation` 按 battle_id `FOR UPDATE`（`go/match/internal/authority/spectator.go:330`） | 同一场观众的提交尾段在同一行锁上串行。第 21 人起还要白走一遍事务、锁和两次 gRPC，最后拿到 16004「请稍后重试」（`global_spectator.go:25-27,230-231`） |
| gate 扇出 | `BroadcastToPlayersEvent` 已有。已提交代码（HEAD `720179638`）每个 session 都要重新编码（`codec.h:86-93`、`codec.cpp:24-76`）；**2026-10-31 更正：C 批（§15.8）已在工作区改为编码一次、复用字节，RPC 与 Kafka 两条入口共用 `gate_push_delivery.cpp`，尚未提交，未见运行验证；每个目标仍各调一次 `conn->send`，也没有按会话合包**。单 IO 线程；Kafka 回调在主 loop 上同步跑完整次扇出，不分片（G8）；输出缓冲超过 2MB 直接 `forceClose` 整条大厅连接（`client_session_handler.cpp:657-667`） | 一次大扇出会卡住整台 gate，慢客户端会被整条踢掉 |
| gate 订阅 | 会话表只按 session_id 索引，SessionInfo 没有任何频道或订阅字段（`session_manager.h:6`、`session_info_comp.h`） | 没有地方记录「这台 gate 上谁在看哪场」 |
| 全服广播 | Kafka 版 `BroadcastToAll` / `BroadcastToScene` 的 handler 守护段是空的，消息被静默吞掉（`gate_event_handler.cpp:388-397`）；chat 不推送 | 仓库里没有可直接复用的全服推送通路 |

D68 规定战斗帧不走 gate：`battle_push_policy.h:21-27,42-48` 把 158 / 161 / 166 归为战斗帧，`battle_push_policy_test.cpp:38-41,56-79` 用 static_assert 钉住；Java 版的 `PushPolicyTest`、`BattleProtocolTest:51-58` 同样钉住。但 D39 / D68 的理由只讨论了参战者的双通路和代码重复，**没有讨论过一对多扇出**；§9 也把「转播 relay」列为预留。所以全服观战是一个尚未决定的空位，不是被否决过的方案。本提案为观战帧新立一条 D68 的显式例外（D94）。

## 2. 目标与非目标

**目标**
- 同一套模型覆盖从「1 个人看」到「全服大部分在线都在看同一场」。
- 人少的场次走低延迟直连，不为它启用中继或广播。
- 每一档都能用配置单独关掉，关掉后回到上一档的行为。
- C++ / Go 版与 Java 版同批落地；两版共享的只有客户端契约（AGENTS.md §12）。

**非目标（本期不做）**
- **防偷看延迟**：观众帧按「非参战者」脱敏，看到的只是任一参战者视角的子集（§14 B3），延迟推送没有收益。以后加入私有信息，或赛事需要解说延时，再做。
- **回放落盘、跨机房中继、增量帧**：放到 P3，或者在 P0 实测不达标时再做。
- **私密局观战**：自定义房间 v1 不开观战（`configurable-templates.md:885`）。全服放大之前必须先补观战可见性闸（§10 第 6 条）。

## 3. 分级定义

符号：`F` 是一帧观众帧的大小（未实测，估 2~5KB）；`G` 是本场有观众的 gate 数；`N_g` 是全服 gate 总数。

| 档 | 新观众进入条件 | 数据路径 | 每回合 battle 产出 | 延迟 |
|---|---|---|---|---|
| **L0 直连席** | 本场直连席 < K（默认 20），且本节点观众连接配额未满 | 现状：AddObserver → 签票 → 直连 battle | N_d 次 send（编码一次） | 毫秒级 |
| **L1 gate 中继** | 直连席已满（专用 tip，D97），且 `SpectateRelay.Enabled` | battle → `gate-spectate_g<N>`，每台有观众的 gate 1 条 → gate 编码一次、分批扇出 | G 条 Kafka | 多一跳 Kafka（未测，估十到百毫秒） |
| **L2 全服广播** | 本场已经是 L2；或者运营标记 featured / `G ≥ max(G_min, N_g/2)`，并抢到全服 L2 配额 B | battle → `spectate-bcast_g<N>`，1 条 → 所有 gate 都读，按本机订阅表扇出 | 1 条 Kafka | 同 L1 |

**档位规则（D95）**
- **档位跟观众走，不跟房间走**：准入时定档，之后不迁移。直连的人一直直连，L1 的人不会回到直连，所以不存在「切换瞬间要迁移已有观众」的问题。
- **L2 按场单向棘轮，本场不降档**：一场只有几分钟；降档需要精确在线人数（L1 / L2 故意不精确统计），收益只是省下 N_g×F 的读流量，却换来切换竞态。
- **升到 L2 的回合 R 起，battle 只发广播，不再发中继**：gate 按 `(battle_id, round_index)` 单调去重，所以重叠或缺一回合都能自愈。每帧都带全量 `BattleStateS2C`（`proto/battle/player_battle.proto:42-48`），客户端感觉不到切换。
- **抢不到配额就留在 L1**：不报错。L1 没有人数上限，代价随有观众的 gate 数线性增长。

**为什么阈值是 G ≥ N_g/2**：L1 每回合 battle 发 G 条，只有有观众的 gate 才解析；L2 只发 1 条，但全服 N_g 台 gate 都要读、要解析。G 过半之后，L2 给所有 gate 多出的解析量不到 L1 的一倍，battle 的产出却从 G 条降到 1 条。`G_min`（建议 32）用来保证 gate 很少时不会靠自动触发进 L2：K8s 现在每个 zone 只有 2 台 gate（`deploy/k8s/zones.json`），这时只认运营标记。初值等 P0 实测后再定。

## 4. 为什么按 gate 定点发，而不是按 zone 广播

你最初的设想是「Kafka 广播到对应 zone 的 gate」。建议改成**定点发到有观众的 gate**，只有真正全服都在看的场次才用全服广播（D96）。理由：

1. **观众的会话在 gate 上，不在 zone 上。** gate 命令 topic 本来就按 gate 寻址（分区 = gate node_id % 256，`node_command_topic.h:40,85-88`）。观众在哪台 gate，准入时就能知道：match 读会话就能拿到 gate_node_id 和 gate_instance_id。
2. **按 zone 发有空读。** zone 里没有观众的 gate 也得读一遍再丢掉，每个 zone 多读 G_z 倍。
3. **按 zone 发并不能让 gate 重启更省事。** 订阅是 gate 进程内的状态，进程一重启就丢了，客户端照样要重新进观战。
4. **观战本来就不分 zone。** 观战列表和准入都没有 zone 过滤，现在已经是「全服可看」（`listwatchablebattleslogic.go:47`）。

## 5. 数据流

### 5.1 加入

```
客户端 WatchBattle(battle_id|0, request_id) ──gate──► match
  match: 观战可见性闸（§10 第 6 条）+ 现有互斥检查（票据 / 战斗锁 / 组队 / 历练占用；只读）
       → 读本场档位 tier、battle_instance_id（spectate:battle:{id} 记录，match 进程内缓存 1s）
       ├─ tier < L2，先试 L0：走现有准入（全局模式写 match_spectator 原件、AddObserver）
       │     ├─ 成功 → L0，维持现状（177 落点 → 直连握手 → 首帧 161）
       │     └─ 直连席已满（专用 tip）且中继开启 → 撤销本次 L0 原件，转 L1：
       │           SADD spectate:relay_gates:{id} (gate_node_id, gate_instance_id)
       │           只有新增的 gate 才调 battle.AttachRelayGate（幂等），battle 立刻给这台 gate 补发一帧
       │           G 达到 L2 阈值，或本场带 featured → Lua 抢全服配额 → battle.PromoteToBroadcast
       └─ tier = L2：不调任何 battle RPC
       → L1 / L2 不写 match_spectator 原件，也不锁 match_formation（Q1）
       → 签发观战订阅令牌 spectate_token = HMAC(spectate_token_secret, SpectateTokenPayload)
       → 回 WatchBattleResponse{delivery=LOBBY, spectate_token}
客户端 SubscribeSpectate{battle_id, spectate_token} ──► gate（gate 本地处理，不转发）
  gate: 验签；核对 payload.player_id == 本会话 playerId，且令牌未过期
       → SpectateHub 入表（battle_id → session_id）
       → 本机有该场缓存：用缓存帧里的 state 合成一条 161 先发，再接后续 158
       → 没有缓存：等下一帧（L1 有 Attach 时补发的那一帧；L2 最多等一回合，2~6s）
```

**L0 满员后的那一次 L0 尝试**：在全局模式下，这一次尝试本身就要走事务、锁 formation 行、发两次 gRPC。热门局要避免每个人都先撞一次 L0：本场记录里一旦标了 `direct_full`，match 就直接走 L1，`direct_full` 允许过期 1s。

**令牌订阅（D98），不用 match 经 Kafka 给 gate 发订阅事件**：
- 每个观众不再产生一条 gate 命令。容量文档更正后的控制面基线已经是约 4.4k 条/s（`capacity-model-hok-scale.md:220,527`）；开场 1 分钟涌入 20 万人，还会再叠加约 3.3k 条/s。
- 订阅由会话自己发起，天然带身份，不需要给 Kafka 多播补身份栅栏（§14 G2）。
- 不存在「订阅事件发到了已经重启的旧 gate 实例」的竞态。
- 代价有三条：
  1. 多一个持密者：match 签、gate 验。新密钥 `spectate_token_secret`，与 `battle_token_secret`、`gate_token_secret` 分开。
  2. 客户端多发一条消息。
  3. `SubscribeSpectate` / `UnsubscribeSpectate` 必须是 gate 本地处理的新服务，不能挂在 `BattleNodeService` 下：gate 会把目标为 BattleNodeService 的上行一律拒掉（D66，`client_session_handler.cpp:1152-1157`）。

### 5.2 每回合

- battle 把观众版帧（158 SpectateTurnResult；参战者版是 139）**编码一次**。
- L0：逐个直连 send，复用同一份字节（P0 改造）。
- L1：对 `relayGates` 里的每台 gate 各发一条 `SpectateFrameEvent`，必填 `target_gate_id` 与 `target_instance_id`。
- L2：往广播 topic 发 1 条，key = battle_id。
- 帧信封：`{battle_id, battle_instance_id, round_index, kind(STATE|TURN|END), deadline_ms, client_frame(bytes)}`。

### 5.3 退出、结束与故障

| 事件 | 处理 |
|---|---|
| 观众主动退出 | L0 不变（165 走直连）。L1 / L2 发 `UnsubscribeSpectate` 给 gate，gate 在本地删除；L1 / L2 在 match 侧本来就没有原件，不需要通知 match |
| 会话断开 | gate 在会话销毁时把它从 SpectateHub 删除 |
| 排队 / 开战互斥（D11） | L0 维持现状：全局模式在同一事务里 `CaptureSpectatorExitsTx` + `RequireNoSpectators`。L1 / L2 按 Q1 的选择处理，默认是软互斥（§9） |
| 战斗结束 | battle 在所有通道上发 END 帧；gate 扇出 166 后删除条目，保留 10 分钟墓碑，墓碑期内迟到的订阅直接回 166 |
| battle 崩溃 | 不会有 END 帧。gate 条目空闲超过 `T_idle`（默认 30s，必须大于手动行动窗口 6s 加结果提交等待），或超过信封里的 `deadline_ms + 宽限`，就在本地发 166（`SOURCE_LOST`）并删除 |
| gate 重启 | 本机订阅表丢失；发给旧实例的 L1 帧被 `target_instance_id` 过滤掉（`node_kafka_command_filter.h:126,135`）。客户端大厅重连后如果还在观战界面，用原 request_id 重发 WatchBattle（幂等），拿新令牌重新订阅 |
| match 重启 | 不受影响：订阅在 gate，L1 的 gate 集合在 battle 内存和 Redis 里 |
| battle 停机 | 正在等结果回执的房间，停机时不发 SpectateEnd（`:1926-1927`）。这很可能是有意的：不想发一个和真实结果矛盾的 ABORTED。中继观众靠 `T_idle` 退出，L0 观众只会收到 FIN（§10 第 7 条） |

## 6. 契约草案

### 6.1 客户端可见（两版同批改）

| 项 | 改动 |
|---|---|
| `WatchBattleResponse`（`match_service.proto:186-190`） | 新增 `eSpectateDelivery delivery = 4`（`NONE=0 / DIRECT=1 / LOBBY=2`）和 `bytes spectate_token = 5`。DIRECT 的行为与现在完全一致；`spectator_operation_id = 3` 的语义不变 |
| gate 本地服务（新） | `SubscribeSpectate{battle_id, spectate_token}` / `UnsubscribeSpectate{battle_id}`；消息号由生成器发号；目标节点类型不能是 BattleNodeService |
| 大厅连接收观战帧 | 158 / 161 / 166 的消息号不变，LOBBY 档改由大厅连接下发。客户端在大厅连接上也注册这三条；去重规则是 158 只与 158 比较 `round_index`，161 不推进水位（STATE(r) 是第 r 回合结算前的快照，紧接着的 TURN(r) 必须照常处理；TURN(r) 的 state 回合是 r + 1），丢弃不是当前观看场次的帧。首帧仍然必须是 161（D69） |
| `eSpectateEndReason`（`player_battle.proto:72-77`） | 新增 `SPECTATE_END_SOURCE_LOST = 4`（0~3 已占用）。旧客户端收到未知值时按 ABORTED 处理 |
| `SpectateStateS2C.observer_count` | 注释改为「LOBBY 档是近似值，0 表示未知」 |
| tip 码 | 走 `data/tip/Tip.xlsx` 的 `//match_error` 组，由导表器发号：「观战人数已满」（中继关闭时回客户端）、「观战令牌无效」。battle → match 内部的「直连席已满」也用专用码 |
| 过时注释 | `player_battle.proto:17-19`（战斗帧只走直连 → 加上 LOBBY 例外）；`match_service.proto:41-42`（首帧「经 Kafka 推」与 D69 矛盾）；`match_service.proto:176-178`（实际是建房前预写） |

### 6.2 内部（C++ / Go）

- **`battle_node.proto`**
  - `AttachRelayGate{battle_id, gate_node_id, gate_instance_id, op_id} → {tier, error}`：幂等；同一个 node_id 带新 instance_id 时替换旧的；新 gate 立刻补发一帧。
  - `PromoteToBroadcast{battle_id, op_id} → {tier, error}`：单向，幂等。
  - AddObserver 满员改回专用码（D97）。
- **`gate_event.proto`**：新增 `SpectateFrameEvent`（L1）。
- **Kafka topic**：名称都走 `node_command_topic` 的代号规则。当前 `CommandTopicGeneration: 2`（`bin/etc/base_deploy_config.yaml:101`），所以实际名字是 `_g2`；编译期默认值 g1 只是兜底。

| topic | 分区 | key | 消费方式 | 防僵尸 |
|---|---|---|---|---|
| `gate-spectate_g<N>` | 与 gate 命令 topic 同契约：分区 = gate node_id % 256 | battle_id | 同 gate 命令 topic：assign 本机分区，不进 consumer group，startAtEnd | `target_gate_id` + `target_instance_id` 必填（AGENTS.md §7.2） |
| `spectate-bcast_g<N>` | 16（契约常量，改数就换代号） | battle_id | 每台 gate assign 全部分区，不进 consumer group，startAtEnd，不提交 offset；保留 10 分钟 | 广播 topic 没有单一目标实例；gate 只接受令牌里登记的 `battle_instance_id` 发出的帧 |

L1 的帧单独开 topic，不和 gate 命令 topic 混用：控制面已经很重，观战帧不能和踢人、路由这类命令抢同一条通道。

- **Redis**（只有 match 读写）
  - `spectate:battle:{id}` 的记录加 `tier`、`battle_instance_id`、`direct_full`。
  - `spectate:relay_gates:{id}`（SET）。
  - `spectate:bcast:active`（ZSET，score 为到期毫秒，用作全服 L2 配额；Lua 先清过期项，再判断 `ZCARD < B`）。
  - `spectate:featured:{id}`（运营标记）。
  - 同一场的 key 用同一个 hash tag。
  - `spectate:battles:active` 现在就是单 key 热点（无 hash tag，生产也在用），P2 给热门列表加进程内缓存。

### 6.3 Java 版

客户端契约同 §6.1。内部按 Java 惯例实现，不照抄：Java gate 下行走 Redis pub/sub `game:gate-push:{zone}:{node}`，gate 不依赖 Kafka（`RedisKeys.java:400-405`，Java 仓 `architecture.md:198`）。

- L1：battle 往有观众的 gate 的推送频道发帧。
- L2：Redis 分片 pub/sub 按场开频道，gate 有本机观众时订阅，linger 到期退订。这样天然只有有观众的 gate 才收帧，L1 / L2 在 Java 里可以合成一种机制。
- Redis pub/sub 是至多一次投递，不能照搬 Kafka 的保序和重投语义。好在每帧都是全量状态，丢一帧可以自愈。
- 需要同批改动的地方：
  - `PushPolicy` 加上 LOBBY 观众这条例外，同步改 `PushPolicyTest`、`BattleProtocolTest:51-58`；
  - robot `BattleSmokeScenario.java:604`「大厅上 139/158/161/166 为 0」的断言，LOBBY 档要排除 158/161/166；
  - gate 新增 SpectateHub；
  - 观战帧写不动时丢帧，不等到 2MB 把大厅连接断掉（Java 现在是 1MB / 2MB 水位后断开，`ClientChannelHandler.java:77-87`）。
- 落地后在 Java 仓 `PARITY.md` 的 L131 / L141 补登。

Java 版现在的观战与 C++ 同形：上限 20、满员回 1008、只走直连。本提案在 Java 版全部**待做**。

## 7. gate SpectateHub 规格

- **数据结构**：全部在 gate 主 loop 上，无锁。
  - `battle_id → {battle_instance_id, 订阅会话集合, 最新帧缓存, 最后收帧时间, deadline_ms}`
  - `session_id → battle_id`：一个会话同时只订一场，新订阅覆盖旧订阅。
- **§11.7**：只存 session_id；发送时查会话表、`lock()` weak 连接，锁不住就当会话已不存在，并顺手摘除。
- **编码一次**：收到帧后只编码一次客户端帧字节，各会话用 `conn->send(data, len)` / `send(StringPiece)` 复用同一份字节。
  - **不能用 `send(Buffer*)`**：它在 loop 内会 `retrieveAll()` 清空传入的缓冲，第一个连接发完共享缓冲就空了（§14 G6）。
  - 复用只省编码；每个连接仍然各做一次 write，没写完的部分各自拷进 outputBuffer。
  - 前提是出站帧与会话无关。将来出站如果要按会话签名，这条复用就作废。
- **分批扇出**：每次最多发 `kFanoutBatch`（建议 512）个会话，剩下的 `queueInLoop` 续发。Kafka 回调在主 loop 上同步执行，而且队列无界、没有背压（G8）；不分批的话，一次 2 万人的扇出会卡住这台 gate 上所有会话的收发、定时器和 RPC 回包。
- **合并**：同一场的新帧到达时，上一帧还没发完的会话直接改发新帧。每帧都是全量状态，跳过中间帧只少播一回合动画。
- **软水位**：发送前检查 `conn->outputBuffer()->readableBytes()`，超过 `spectate_soft_watermark`（建议 256KB）就跳过这一帧并计数。仓内已有同类写法（`muduo_grpc/http2_transport.cpp:173,182`）。muduo 的高水位回调是延后执行、而且只触发一次，一轮大扇出里单连接缓冲可能远超 2MB 后才被关；软水位要挡在它前面。
- **首帧合成**：新订阅者到来时，gate 用缓存帧里的 `state` 合成一条 161 先发，保持「收到 161 才算进入观战」（D69）。只在订阅时解析一次，不是每帧都解析。
- **空闲与到期**：见 §5.3。
- **指标**：订阅会话数、扇出次数、软水位跳帧、合并跳帧、空闲收尾。标签不带 battle_id，也不带 player_id。
- **人数上报（P2）**：每 5 秒把本机各场的观众数写进 `spectate:viewers:{id}`（HASH，field 为 gate_node_id，带 TTL），供热门列表和近似 `observer_count` 使用。

## 8. 容量估算（量级，未实测）

假设 `F ≈ 3KB`，每 2 秒一回合（这是全自动局的下限；手动局的行动窗口是 6 秒，`turn_battle_constants.h:16,36-50`）。全服规模取容量文档的数字：`N_g ≈ 1000~4444`，每台 gate 4.5k~20k 人；K8s 现在每个 zone 2 台 gate。

| 场景 | battle | Kafka | gate | match |
|---|---|---|---|---|
| 1 人看（L0） | 1 条直连 | 只有加入时 1 条落点公告 | 0 | 现状（全局模式约 5 个事务） |
| 本服 500 人看一场（20 人 L0 + 480 人 L1，G=2） | 20 次直连 send + 每回合 2 条 | 每回合约 6KB | 每台约 240 人，约 360KB/s | 480 次轻量准入：只读互斥检查 + Redis + 一次签名；不写原件 |
| 全服 20 万人看同一场（L2） | 1 条广播 + 20 次直连 | 生产 1.5KB/s；broker 出向 N_g × 1.5KB/s ≈ 1.5~6.7MB/s | 平均每台 45~200 人；最坏单台 2 万人全在看，约 30MB/s，压在一条 IO 线程上 | 1 分钟涌入约 3.3k/s；每次是几次只读点查 + Redis，**不产生 Kafka 消息** |
| 2000 场各 3 人（全部 L0） | 6000 条直连，分散在 battle 池里 | 只有加入时 6000 条 | 0 | 6000 次现状准入 |

反例：如果这 2000 场都走 L2，broker 出向是 `2000 × N_g × 1.5KB/s ≈ 3~13GB/s`。所以 L2 必须有全服配额 B，冷门的长尾场次必须留在 L0 / L1。

**瓶颈排序**：gate 单 IO 线程的扇出能力 > 开场准入风暴 > Kafka。Kafka 不是瓶颈。

## 9. 一致性与失败模式

- **顺序**：每场只有一个生产者。L1 落在目标 gate 的单个分区上，L2 用 key = battle_id，各自有序。gate 和客户端都丢弃回合号不大于已收值的 158（只与 158 比较；161 不推进水位，见 §15.5），升档时的双发和 Kafka 重复投递也一并被吸收。
- **丢帧**：下一帧带全量状态，自动恢复，不做补发。
- **D11 互斥（Q1 已定：软互斥）**：
  - L0 维持现状，硬互斥。
  - L1 / L2 的观众在 match 侧没有原件，排队、组队、开战的事务看不到他们。
  - 准入时仍然只读检查「不在队列、不在战斗、不在组队 / 历练占用」。
  - 另写一个轻量在线标记 `spectate:lobby:{player_id}`（值为 battle_id + gate 实例，TTL = 战斗截止时间）。排队 / 开战时 match 读到它，就经 gate 命令尽力下发一次退订；客户端也会自己退订，并丢弃不是当前观看场次的帧。
  - 万一以上都没做到，后果只是多收几帧观战，不碰资产，也不碰战斗状态。
  - 不采用硬互斥：那样 L1 / L2 也得写原件，去掉 formation 行锁后每个观众仍是约 4 个事务，开场风暴下约 1.3 万事务/s。
- **fail-closed**：互斥检查、Redis 或 battle 调用失败时拒绝本次 WatchBattle；升档失败保持原档；令牌验签失败拒绝订阅。
- **安全**：只有通过可见性闸的公开局才能进 L1 / L2；只发已脱敏的观众版帧；令牌绑定 player_id 和 battle_instance_id，不能转让，也不能拿去收其它实例的帧。
- **跨机房**：生产按物理机房放置 battle（`gather.go:340` 的 `RequiredPhysicalRegion`）。但跨机房的 Kafka 拓扑没有定论：控制面分区文档要求「一个可达的集群」，Kafka 运维手册写「多 zone 每 zone 独立集群」，容量文档 :521 把它记为「要由负责人定」。L1 / L2 和现有的 177 落点公告一样，依赖「battle 能经 Kafka 到达观众所在 gate」，这一点是前置问题（Q6），不能当作既有事实。

## 10. 与分级无关、P0 先修的现存问题

1. **满员、墓碑表满和其他拒绝用同一个码**（`:982`、`:1084` 都回 `kRateLimitExceeded`）。另外，墓碑在所有校验之前就已插入（`:986-990`），之后的每个拒绝分支都留着这条墓碑，直到 `admit_before` 过期、并且后面又来一次 Add/Remove 才会被清理（`:930-941`）。改法：满员换成专用码（D97）；评估拒绝分支能否不留墓碑。拒绝没有副作用，所以重试时重新判定是安全的。
2. **掉线和从没连上的观众一直占着直连席**：直连断开只删 `directConnByPlayer`，不删 `routingByObserver`（`:2165-2178`），名单里没有任何观众定时器。这可以被用来占座：20 个账号各发一次观战、不建直连，就能把一场的直连席占到战斗结束。`observer_count` 也把这些人算进去了。
   - 改法：登记后 N 秒内没建直连就回收；直连断开后给一个宽限期（建议 30s），过期就摘名单。
   - 回收必须和 match 的 `match_spectator` 原件对齐，否则 QueryObserver 回 ABSENT 会触发 match 侧 cancel。
   - `observer_count` 只数有活直连的观众。
3. **battle 观众帧逐人序列化**：改成编码一次、复用字节。
4. **观众没有单独的连接配额**：在 `battle_max_connections` 内划出观众上限，例如 25%。
5. **全局模式把业务拒绝报成内部错误**：满员、离线、排队中、战斗中，统统回 16004「观战原请求尚未确认，请稍后重试」，而且全局路径没有任何观战指标（`global_spectator.go:25-35,83-94,230-231`）；本地模式和 Java 回的是 16018。改法：给每种拒绝回明确的 tip，并补上指标。客户端换个 request_id 重试的话，每次还要白走约 7 个事务。
6. **观战没有可见性闸**：按 battle_id 指定观战，两条路径都只检查战斗是否存在（`watchbattlelogic.go:171-216`、`global_spectator.go:60-97`）；gather 对所有模式无条件登记并公开观战（`gather.go:376-377,485`）。所以切磋局、帮会历练局现在都能被任何人按 id 观战。闸要同时落在三处：gather 的登记与公开、两条 WatchBattle 路径、ListWatchableBattles。
7. **停机时等结果回执的房间，观众只收到 FIN**（`:1926-1928`）：要么新增一个不与真实结果矛盾的结束原因（例如 `NODE_SHUTDOWN`），要么在客户端契约里写明这种收尾方式；同时禁止观众加入已经进入结果提交阶段的房间。

第 1~5 条不改客户端契约。第 5 条若新增 tip，第 6、7 条若新增枚举值，都要两版同批改。

### 10.1 修复进展（2026-10-24，A 批已落码，未编译，待 Codex 验证）

A 批修第 1、2、5 条。C++ / Go 与 Java 同批完成，每份改动都经过一轮对抗评审和修正。第 3、4 条（编码一次、观众连接配额）仍留在 P0，等实测时一起做；第 6 条是 B 批，第 7 条另议。

| 条目 | C++ / Go（本仓） | Java（0.1.0-SNAPSHOT） |
|---|---|---|
| 1 满员专用码（D97） | 新 tip `MatchSpectateFull`「观战人数已满」，Tip.xlsx 第 266 行，fault 留空；导表器已发号 16021。battle 满员改回它；墓碑表满仍回 `kRateLimitExceeded`。`kMaxObserversPerRoom` 移到 `BattleRoomManager` 公开区，测试引用同一个常量 | 满员回 `SPECTATE_FULL`；账本满维持 1003 |
| 墓碑语义（更正上面第 1 条「拒绝无副作用、重试重新判定是安全的」） | **拒绝分支保留墓碑**。全局 match 收到任何拒绝都会发 RemoveObserver，Remove 本来就要给这个 op 立一个封闭墓碑，用来挡住还在途中的 Add；所以拒绝时不立墓碑也省不下表项，反而会失去「同一个 op 被拒后永远不会翻成准入」的单调性。例外有两种：入参校验失败、墓碑表满，这两种不立墓碑，靠 match 的 Remove 补立 | 同 |
| 2 观众席回收 | 两段宽限都是 30s 常量：登记后 30s 内没建直连，或直连断开后 30s 内没重连，就回收席位。回收时先封墓碑，再删名单、关直连，不推 166；补签票不延长宽限。所有摘除路径都收口到 `RetireObserver`。定时器只绑 `WeakSelf()`，测试接缝是 `ReclaimExpiredObserverSeats(battleId, now)`。`observer_count` 只数有活直连的观众。回收后 QueryObserver 回 ABSENT，由 match 的恢复循环 cancel 原件，不需要 battle 主动通知 | 同语义：`BattleScheduler.after`，`cancelObserverOperation` 是唯一收口；新增指标 `game_battle_observer_reclaimed_total{reason}` |
| 5 全局路径错误码与指标 | 每个分支都回明确的 tip：16014 / 16015 / 16016 / 16017 / 16018 / 16019 / 16021；参数错、`ErrOriginalPlayer`、存储或传输故障维持 16004。authority 用 `%w` 包装出可区分的哨兵错误。关闭原因落在意图记录新增的 `close_tip` 字段（`match_authority.proto` 的 `MatchSpectatorIntentRecord` 字段 9，**要加列迁移**），同一个 request_id 重放得到与首答相同的结论；被新请求顶替的旧请求在顶替那一刻写入 16016。每个出口都计 `match_watch_battle_total{outcome}`（新增 full / closed / pending），只有故障才打 Error 日志。随机模式满员不换场，两条路径一致 | 同口径：player 0 在入口回 NO_IDENTITY；被顶替回 16016、已关闭回 16018（原先都回 16004）；每个出口按回执计 outcome |

**客户端可见的变化**：
- 满员从 16018 / 16004 变成 16021。
- 直连断开超过 30s 后再握手，会因「not in roster」被拒，客户端要把它当作观战结束。旧客户端收到 16021 只是查不到文案。

**残余，本批未处理**：
- 半开 TCP 仍会占座：直连没有应用层心跳。
- 墓碑表满时整个节点拒绝观战。
- Go 全局路径的 16018 一律记 `not_found`，不细分；要细分得给意图再加一个关闭类别字段。
- C++ 断线重连宽限的用例用回环连接加直接调用 Detach 模拟，没有经过 `BattleClientEdge` 的真实断开回调。
- 本地（dev / test）模式两版都发不出合规的 AddObserver：不带 `spectator_operation_id`、`admit_before_ms`、`target_instance_id`，被 battle 一律拒绝（C++：`spectate.go` 本地 addObserver 对 `battle_room_manager.cpp` 的三字段校验；Java：`WatchBattleService.java:373-374` 对 `ObserverOperationLedger.validate`，均已在 A 批评审中核实）。生产强制走全局模式，不受影响，但本地观战冒烟走不通。
  - **Go 已修（2026-10-24，未编译，待 Codex 验证）**，口径对齐全局 `BeginSpectator` / `cancelGlobalSpectator`：
    - 每次 WatchBattle 每选中一场生成一份 AddObserver 原件（`newLocalObserverAdd`）：op 是新生成的小写规范 UUID；`admit_before_ms` = match 墙钟 now + 25s。battle 上限是 30s，全局路径用 TiDB now() + 30s；本地没有统一时钟源，留 5s 给 match 与 battle 的墙钟偏差。`target_instance_id` 取 battle 节点在注册表里的 `NodeUuid`（新增 `NodeWatcher.EntryOfNode`），实例为空时不写标记、不发 Add，回 16018。观战记录 `SpectateBattleRecord` 只有 node_id、没有实例字段，所以从注册表取；给记录加字段要按 AGENTS §4 重生，与 `requestbattleticketlogic.go` 判 BattleGone 的同一缺口合并另做。
    - 观战标记 `spectate:watching:{player_id}` 的值从十进制 battle_id 改成原件 pb，在发 Add 之前用 SETNX 写入。换场、gather 入口清退、复查自我清退、结果未知补偿这四种 RemoveObserver 都从原件里取同一个 op、截止和实例。只有回包带回原 op 和原实例、并且 `cancelled=true`，才算拿到取消证明。解析不出原件的标记拿不到原 op，只删标记。
    - **行为变化：重看同一场也要先按旧原件 Remove，再用新 op 去 Add。** 原来的做法是只删标记、靠 battle 的幂等分支重推，但名单里挂着旧 op 时，账本会拒绝新 op，而旧原件的准入截止多半已经过了，也不能原样重放。代价是旧会话会先收到一次 `SpectateEnd(rewatch)`，再凭新分配重连。全局路径本来就是这样（`prepareGlobalWatch` 先 cancel）。
    - 成功回包带回 `spectator_operation_id`，与全局路径一致。
    - 回归测试：`spectate_test.go` 新增按账本校验的桩 battle（`observerLedgerBattle`），覆盖 Add 带齐三个字段、Remove 带同一个 op、取消后同 op 迟到的 Add 被拒、重看同场成功；另有一个用例覆盖实例为空时 fail-closed。原有用例改成断言 Remove 带原件的 op；`request_deadline_fence_test.go` 的结果未知补偿用例同样加了这条断言。
  - **Java 已修**：`add334cb`（`LocalObserverOperation`，回环用例 `LocalObserverRpcLoopbackTest`）。Java 只剩 robot 走 dev 口的待做项，登记在 Java 仓 `PARITY.md`「观战 P0 缺陷」行。
  - **本地 robot 观战冒烟**（`robot/battle_smoke_scenario.go`：B 随机观战 A）的阻塞点：
    1. Go 改动尚未编译和测试，要等 Codex 跑完 [specs/85](../handoff/spectate-tiered-fanout-20261030/specs/85-local-mark-consistency.md) 第 6 节的命令。
    2. 必须走本地模式（`GlobalMatch.Enabled=false`，`match_service.yaml` 默认值）。全局模式下 robot 的 WatchBattle 不带 `request_id`，会在入口回 16004「观战请求参数无效」。
    3. battle 节点在 etcd 的注册里要有 `nodeUuid` 和 `grpcEndpoint`（C++ NodeInfo 默认会带），并且与 battle 进程内 `gNode->GetNodeInfo().node_uuid()` 是同一个值；否则 match 回 16018，或者 battle 回 kInvalidParameter。
    4. match 的墙钟不能比 battle 快 5s 以上（battle 跑在 WSL / Docker 而 match 跑在宿主机时要留意），否则 Add 会因截止过远被拒。
    5. 跑之前要清 Redis（AGENTS §6.2），让 `spectate:battles:active` 里只剩 A 这一场；随机选场挑到残留的场会在 `b-watch-battle-id` 失败。
    6. 冒烟走 Java 服务端时，Java robot 的 `BattleScenario` 第 16 步仍走不通（dev 口缺这三个字段，见上一条）。
  - **Go 标记一致性（2026-10-30 复核后修复，未编译，待 Codex 验证）**：10-24 那版的标记语义还是尽力而为，比全局路径和 Java 都弱，现在与它们同一规则：**标记是 battle 上可能挂着的观众唯一的恢复入口，只有拿到取消证明，或 Add 被明确拒绝、请求确定没发出，才按值删除；其余一律保留，下一次 WatchBattle 沿同一个 op 重试。**
    - Remove 失败不再删标记。换场和重看同一场时，旧观战取消未获证明就回 16004、不登记新操作（否则同场重看会被账本一直拒绝，恢复入口却已丢失）。gather 入口清退、复查自我清退、结果未知补偿只记日志，结论不变。
    - 删标记改为按值比对（Lua），不会误删并发 WatchBattle 刚抢到的新标记。
    - 取消按原件固定的实例定位（`NodeWatcher.EntryOfInstance`），不再读会被 D82 改写的观战记录；观战记录不在或损坏都不影响取消。
    - AddObserver 的任何 RPC 错误都算结果未知（不再按状态码细分），要沿原件取消。Add 被明确拒绝后回滚标记失败按依赖故障回 16004，不转 L1、不换场，残留标记由下一次 WatchBattle 清掉。
    - `observer_name` 改为角色公开名（`challengePublicName`），不再用登录账号；查不到时为空串。
    - 补跨会话栅栏（`client-request-ordering.md` §6.3）：读会话、判在线、比对会话号与 gate 实例都提到清退旧观战之前，离线或旧会话的请求回 16019，没有任何副作用。
    - **残余（都只在本地模式）**：① battle 实例崩溃且不再回来时，挂在上面的玩家，以及崩溃后、租约到期前试图观战的玩家，WatchBattle 回 16004 直到标记 TTL（战斗最长期限 + 60s）到期；② 切磋没有票据，gather 读完标记之后、Prepare 写战斗锁之前到达的 WatchBattle 复查全过，玩家会短暂既是观众又是参战者，标记还在，可恢复。本地路径的 TTL 和尽力清退不等于全局路径的事务互斥。
    - 与 Java 的非契约差异：Go 固定实例、按实例查端点，Java 固定端点；解析不出原件的标记 Go 按值清掉，Java 对带版本前缀的损坏标记选择保留；Add 连接失败 Java 能判「确定未送达」并回滚，Go 的 gRPC 分不清，按结果未知保留标记。
    - 方案、定稿签名和测试清单见 [specs/85-local-mark-consistency.md](../handoff/spectate-tiered-fanout-20261030/specs/85-local-mark-consistency.md)。

## 11. 分阶段落地

每个阶段都带开关，关掉就回到上一阶段的行为。C++ 与 Java 同批。

| 阶段 | 内容 | 改动面 | 回滚 |
|---|---|---|---|
| **P0 修缺陷 + 去硬编码 + 实测** | K 改成配置（默认仍是 20）；观众连接配额；§10 第 1~5 条；battle 观众帧编码一次；**实测帧大小，以及 gate 单线程用编码一次的字节扇出的上限** | C++ battle、Go match；Java battle / match。第 5 条的新 tip 两版同批 | 改配置 |
| **P1 L1 gate 中继** | D94~D98 落地；`gate-spectate_g<N>`；gate SpectateHub；令牌订阅；match 档位判定与轻量准入；§10 第 6 条可见性闸；客户端按 `delivery` 分支，大厅连接挂 158/161/166 | C++ battle / gate、Go match、proto、Tip.xlsx、客户端；Java battle / gate / match / robot | `SpectateRelay.Enabled=false`：直连满了回「观战人数已满」 |
| **P2 L2 全服广播** | `spectate-bcast_g<N>`；全服配额 B；运营 featured 入口（admin）；人数上报与热门列表；`BattleWatchSummary` 加 featured / viewer_hint | C++ battle / gate、Go match、admin、客户端列表 | `SpectateBroadcast.MaxConcurrent=0`：只用 L1 |
| **P3 按需** | 多机房：每个机房一个中继消费者，再扇到本机房的 gate；回放：归档消费者把帧落对象存储，只对赛事开；有私有信息之后再做延迟推送 | — | — |

每个阶段完成后：在 `turn-based-battle-server.md` 登记对应的 D 号，在 Java 仓 `PARITY.md` 登记一行，在 `PROGRESS.md` 记流水。

## 12. 决策（2026-10-23 已拍板）

负责人 2026-10-23 授权：§13 的问题一律按最标准的做法定，§10 的现存缺陷一律按标准方案修。下表 D94–D98 随之生效；P1 / P2 仍按 §11 逐阶段落码。

| # | 决策 | 理由 |
|---|---|---|
| D94 | 观战帧在 LOBBY 档经 gate 下发，作为 D68 的显式例外；参战帧仍然只走直连，继续用单测钉住 | D68 的理由针对的是参战者双通路；一对多扇出需要 gate 这一层边缘节点。每个观众始终只有一条通路，这不是「回落」 |
| D95 | 档位按观众在准入时决定、不迁移；L2 按场单向棘轮 | 消除切换瞬间迁移已有观众的竞态；战斗只有几分钟，降档收益小 |
| D96 | 中继按 gate 定点发，不按 zone 广播；只有 L2 用全服广播 topic | 会话在 gate 上；按 zone 发有 G_z 倍空读，gate 重启也不会因此更省事 |
| D97 | 「直连席已满」用专用码，和限流、墓碑表满、其他拒绝区分开 | 现在同码，满员无法驱动转 L1，在全局模式下还被报成内部错误 |
| D98 | L1 / L2 观众凭 match 签发的令牌在 gate 本地订阅 | 每个观众零条 Kafka 控制面消息，天然带身份，不存在旧实例竞态 |

## 13. 已拍板的问题（2026-10-23，按标准做法）

| # | 问题 | 结论 | 理由 |
|---|---|---|---|
| Q1 | L1 / L2 的互斥强度 | **软互斥**。不写 `match_spectator` 原件，不锁 `match_formation`；准入时做只读检查（不在队列、不在战斗、不在组队 / 历练占用）。另写一个带 TTL 的轻量在线标记，供排队 / 开战时尽力经 gate 退订；客户端进入排队或战斗时自己退订，并丢弃不是当前观看场次的帧。**（2026-10-30 简化：在线标记与服务端退订不做，见 §15.1）** | D11 的硬互斥保护的是参战和观战共用的会话绑定与直连槽（battle_room_manager.h 按 player_id 单槽）。L1 / L2 走大厅连接，不占这个槽。互斥失效的后果只是多收几帧观战，不碰资产，也不碰战斗状态。硬互斥会让热门局准入被行锁和每人约 4 个事务拖住 |
| Q2 | 中继寻址 | **按 gate 定点发**，只有 L2 用全服广播 topic（D96） | 见 §4 |
| Q3 | L2 怎么触发 | **自动阈值和运营标记二者都认**，统一受全服配额 B 约束 | 爆款对局事先无法预知，只靠人工标记会漏；配额 B 给自动升档封了顶，不会失控 |
| Q4 | 直连席是否预留 | **不预留**（YAGNI） | 目前没有主播 / 解说的产品需求；直连席先到先得，满了自动进 L1，体验差别只是多一跳延迟 |
| Q5 | 阈值初值 | **P0 实测后再定**。实测前用保守默认值：K=20，G_min=32，B=4，软水位 256KB，T_idle=30s，kFanoutBatch=512 | 帧大小和 gate 扇出上限都没测，拍出来的数字没有依据 |
| Q6 | 跨机房 Kafka 拓扑未定时 | **L1 / L2 只在 battle 与观众所在 gate 处于同一个 Kafka 集群时启用**；跨集群的观众只能进 L0，L0 满了回「观战人数已满」（fail-closed） | 不能把「battle 能经 Kafka 到达任意 gate」当成既有事实（§9）；跨机房的直连是普通 TCP，L0 照常可用 |

## 14. 核实记录（2026-10-23，只读）

第二轮核实共 31 条断言。结论为「部分属实」的，都已按实际情况改进正文。

| 编号 | 断言 | 结论 | 要点 |
|---|---|---|---|
| B1 | 满员和墓碑表满同码，墓碑先于满员判定插入 | 属实 | 墓碑在所有校验之前插入，所有拒绝分支都留着它（`:986-990`） |
| B2 | 掉线观众占席位，没有回收 | 属实 | 唯一调用方是直连关闭；名单没有任何观众定时器；可被用来占座 |
| B3 | 观众看到的是参战者视角的子集 | 属实 | 只清掉 combat、全员冷却、本人道具；已提交的指令只在引擎内部，任何人都拿不到；基础属性、技能等级对所有人公开，不属于观战特有的问题 |
| B4 | 停机时等结果回执的房间不发 SpectateEnd | 属实 | 很可能是有意的（避免和真实结果矛盾），只是注释没写观众会怎样 |
| B5 | 手动窗口 6s；全自动 2000ms、超过 8 个出手单位每个 +250ms | 属实 | turn-based D13 的「30s 行动窗口」是过期描述 |
| B6 | battle 不消费 Kafka | 属实 | `cpp/nodes/battle/main.cpp:240` |
| B7 | 观众帧逐人序列化 | 属实 | |
| B8 | 观战帧被钉为只走直连 | 属实 | C++ `battle_push_policy_test.cpp`，Java `PushPolicyTest` / `BattleProtocolTest` |
| B9 | 每帧带全量状态 | 属实 | |
| G1 | gate 命令 topic 的消费方式 | 部分属实 | 当前生效的是 `_g2`；本机还会以 consumer group 方式消费旧的 `gate-<id>`（`DisableLegacyPerNodeTopic: false`），K8s 只走 assign |
| G2 | Kafka 版推送没有身份栅栏 | 属实 | PushToPlayer 和 BroadcastToPlayers 都没有，事件里根本没有可比对的 player_id；RPC 版有 ClassifyPush |
| G3 | Kafka 版 BroadcastToScene / All 是空实现 | 属实 | 静默丢弃，不打日志；目前没有生产者 |
| G4 | 单 IO 线程、2MB 强关、上限 2 万连接 | 属实 | 高水位回调延后执行且只触发一次 |
| G5 | 每个 session 重新编码 | 属实（2026-10-23） | 2026-10-31 补记：C 批已在工作区改为编码一次（`gate_push_delivery.cpp`），尚未提交，未见运行验证；HEAD `720179638` 仍是旧实现 |
| G6 | 编码一次可行 | 属实 | Linux gitlink 和两棵 Windows 树的 TcpConnection 逐字相同；不能用 `send(Buffer*)` |
| G7 | 会话表只按 session_id 索引 | 属实 | |
| G8 | 扇出在主 loop 上同步执行 | 属实 | Kafka 线程 → queueInLoop，队列无界，没有背压 |
| M1 | formation 行锁 | 属实 | 串行的只是 BeginSpectator 的尾段；生产强制全局模式 |
| M2 | Redis 路径约 10 次 Redis | 属实 | 但这是 dev / test 路径；`spectate:battles:active` 在生产也在用 |
| M3 | 停看只能走直连 | 部分属实 | MatchService 实际有 12 个 rpc；新的退订不能挂在 BattleNodeService 下 |
| M4 | 首帧必须是 161 | 部分属实 | 158 是观众版回合结果，参战者版是 139；`match_service.proto:41-42` 已过时 |
| M5 | 新字段和枚举的编号 | 属实 | delivery=4、spectate_token=5、SOURCE_LOST=4 |
| M6 | 按 id 观战没有鉴权 | 属实 | gather 对所有模式无条件公开；全局恢复建成的局不公开，但按 id 仍可看 |
| M7 | 容量数字 | 属实 | 500 条/s 是假设值，同文 :527 已更正为 0.4 万~4.4 万条/s |
| M8 | battle 按机房放置；Kafka 跨机房 | 部分属实 | 机房放置是代码走向；Kafka 拓扑没有定论 |
| M9 | 全局模式满员回 16004 | 属实 | 全局路径没有任何观战指标 |
| C1 | 客户端契约写明战斗帧只走直连 | 属实 | |
| C2 | robot 把观战帧挂在直连上 | 部分属实 | C++ robot 两条连接共用一张分发表，没有断言大厅连接上为 0；Java robot 有这条断言 |
| C3 | Java 版现状 | 属实 | 上限 20、满员 1008、gate 走 Redis pub/sub、不依赖 Kafka |
| C4 | PARITY 状态 | 属实 | 观战契约两版一致；L131「`73b852f3` Java 待跟」、L162「RemoveObserver 围栏」仍未闭合 |
| C5 | 没有「观战人数已满」的 tip | 属实 | 本地模式和 Java 用 16018，全局模式用 16004 |

**核实中顺带发现、不属于本提案范围的问题**
- Kafka 版 PushToPlayer / BroadcastToPlayers 没有身份栅栏（G2）：session_id 回绕或 gate node_id 复用后，推送可能写进陌生人的连接；RPC 版已经封住了这种情况。
- `gate_kafka_command_router.cpp:163-183`：payload 为空时的回退分支会给客户端发一个空的 MessageContent，三个广播回退分支是空事件，都不打日志。
- 命令 topic 代号的默认值不一致：C++ 编译期默认、Go `kafkacmd`、docker-compose 都是 g1，`bin/etc` 配的是 2。只有经 `start_game.ps1` / `k8s_deploy.ps1` 启动时会注入对齐；绕开这两个脚本直接起服务，gate 就收不到命令。
- turn-based D13 的「30s 行动窗口」与代码的 6s 不一致。

## 15. P1 / P2 落码契约（2026-10-30 定稿，实现按本节；与 §5–§7 冲突时以本节为准）

负责人 2026-10-30 授权按标准做法把服务端做完（项目未上线，玩家数据可清空，不做新旧兼容）。本节把 §5–§7 的草案收成可落码的契约，并按落码时核实的事实做了四处简化，理由附在条目里。

### 15.1 相对草案的简化

| 草案 | 定稿 | 理由 |
|---|---|---|
| Q1 的 `spectate:lobby:{player_id}` 标记 + 排队 / 开战时经 gate 命令尽力退订 | **不做**。L1 / L2 观众的服务端状态只有 gate 上的订阅 | 一个会话同时只订一场，新订阅自动替换旧订阅；战斗结束、会话断开、空闲超时都会清掉订阅；互斥失效的后果只是多收几帧观战。标记和退订事件没有正确性收益（YAGNI）。客户端进入排队或战斗时自己退订，丢弃不是当前观看场次的帧 |
| L2 自动阈值 `G ≥ max(G_min, N_g/2)` | **`G ≥ G_min`**（`Spectate.BroadcastAutoGateThreshold`，默认 32） | match 不需要知道全服 gate 总数；提前升档的代价被全服配额 B 封顶 |
| P2 的 gate 人数上报（gate 写 match 的 Redis） | **不做**。LOBBY 档 `observer_count` 填 0（未知）；热门列表只按 featured 排在前面 | gate 写 match 的 Redis 是跨服务存储耦合；人数只是展示，没有正确性用途 |
| Q6 的「同一 Kafka 集群」判定 | **部署级开关** `Spectate.RelayEnabled`：只在全部 battle 与 gate 共用一个 Kafka 集群时打开；关闭时观众只走 L0，满了回 16021 | 跨机房 Kafka 拓扑没定，靠运行期探测猜集群边界不可靠 |

### 15.2 协议（proto，主会话统一改；两版同批；Codex 统一跑 proto-gen）

**客户端可见**（Java 经 ContractSync 同步）：

- `proto/match/match_service.proto`
  - 新增 `enum eSpectateDelivery { SPECTATE_DELIVERY_NONE = 0; SPECTATE_DELIVERY_DIRECT = 1; SPECTATE_DELIVERY_LOBBY = 2; }`。
  - `WatchBattleResponse`：`eSpectateDelivery delivery = 4`；`bytes spectate_ticket = 5`（LOBBY 时非空，客户端原样转交 gate，不解析）。
  - `BattleWatchSummary`：`bool featured = 6`（运营标记的赛事局，列表排前）。
- 新文件 `proto/gate/gate_client_spectate.proto`（`cc_generic_services = true`、`OptionFileDefaultNode = NODE_GATE`、服务标 `OptionIsClientProtocolService`）：
  `SubscribeSpectate{battle_id, spectate_ticket} → {error_message}`、`UnsubscribeSpectate{battle_id} → {error_message}`。
  gate 在客户端消息分发处按 `targetNodeType == GateNodeService` 本地处理，不转发。
- `proto/battle/player_battle.proto`：`eSpectateEndReason` 新增 `SPECTATE_END_SOURCE_LOST = 4`（中继档 gate 本地收尾：源站失联或过了战斗期限）。
- tip：`data/tip/Tip.xlsx` match_error 组新增 `MatchSpectateTicketInvalid`「观战凭证无效或已过期」（导表器发号）。

**内部**：

- `proto/battle/battle_data.proto`：新增 `enum eSpectateTier { SPECTATE_TIER_DIRECT = 0; SPECTATE_TIER_RELAY = 1; SPECTATE_TIER_BROADCAST = 2; }`。
- `proto/battle/battle_node.proto`：
  - `rpc AttachRelayGate(AttachRelayGateRequest) returns (AttachRelayGateResponse)`：请求 `{battle_id, gate_node_id, gate_instance_id, target_instance_id}`；回包 `{error_message, battle_instance_id, deadline_ms, broadcast}`。
  - `rpc PromoteSpectateBroadcast(PromoteSpectateBroadcastRequest) returns (PromoteSpectateBroadcastResponse)`：请求 `{battle_id, target_instance_id}`；回包 `{error_message, battle_instance_id, deadline_ms}`。
- `proto/contracts/kafka/gate_event.proto`：
  - `enum eSpectateFrameKind { NONE = 0; STATE = 1; TURN = 2; END = 3; }`。
  - `SpectateFrameEvent`：`{battle_id, battle_instance_id, round_index, kind, client_frame, deadline_ms, target_gate_id, target_instance_id}`。`client_frame` 是已编码的客户端 `MessageContent`（message_id 为 161 / 158 / 166）。它就是观战帧 topic 的消息体：不套 GateCommand 信封、不标 `OptionKafkaIngress`、不进 gate 命令路由。gate 直接用 `RegisterKafkaCommandHandler<SpectateFrameEvent>` 的按分区 assign 与目标字段反射过滤消费（2026-10-30 定稿时改，不再生成用不上的 gate-cmd 处理函数骨架）。
  - 消息体里的 `message_id` 取值写成「161 / 158 / 166」，这是 2026-10-30 的号段；以 `proto/message_id.txt` 为准。
- 新文件 `proto/gate/gate_spectate_ticket.proto`（只给 match 与 gate 用）：
  - `SpectateTicketPayload{battle_id, player_id, battle_instance_id, deadline_ms, expire_at_ms}`。
  - `SpectateTicket{payload, signature}`，其中 `signature = HMAC-SHA256(spectate_token_secret, payload)`。
- `proto/common/base/config.proto`：`BaseDeployConfig.spectate_token_secret`（match 签、gate 验，与 gate / battle 令牌密钥分域）。
- `proto/match/match_service.proto` `SpectateBattleRecord`：`eSpectateTier tier = 4`、`string battle_instance_id = 5`、`uint64 deadline_ms = 6`。
- `proto/match/match_authority.proto` `MatchSpectatorIntentRecord`：`eSpectateDelivery delivery = 10`（全局模式的意图以 LOBBY 结案时写入，重放据此重新签发凭证）。

### 15.3 Kafka topic（名字一律走 `node_command_topic` 的代号规则）

| topic | 分区 | 生产 | 消费 | 防僵尸 |
|---|---|---|---|---|
| `gate-spectate_g<N>` | 与 gate 命令 topic 同契约（node_id % 256，同一个分区数配置） | battle：每回合给每台已挂的中继 gate 各 1 条 | gate：assign 本机分区，不进 consumer group，startAtEnd，不注册旧的一节点一 topic 兼容路径；保留 10 分钟（帧过期无用，下一帧自愈）；消息体就是 `SpectateFrameEvent` | 事件里的 `target_gate_id` + `target_instance_id` 必填（AGENTS.md §7.2），复用 `node_kafka_command_filter.h` 的反射过滤 |
| `spectate-bcast_g<N>` | 16（契约常量，分区 = battle_id % 16，改数就换代号，见 `node_command_topic.h`） | battle：已升 L2 的场次每回合 1 条 | 每台 gate assign 全部分区，不进 consumer group，startAtEnd，不提交 offset；保留 10 分钟 | 广播 topic 没有单一目标，两个目标字段留空；gate 只接受订阅凭证里登记的 `battle_instance_id` 发出的帧 |

两条 topic 都要进 docker-compose 的 Kafka 初始化与 `k8s_deploy.ps1` 的建 topic 步骤，代号与命令 topic 同源（`CommandTopicGeneration`）。

### 15.4 battle（C++ 与 Java 同语义）

- **房间状态**：`relayGates`（gate_node_id → gate_instance_id，同 node_id 新实例覆盖旧实例），`spectateBroadcast`（bool，只升不降）。
- **AttachRelayGate**：依次校验 `target_instance_id` 等于本实例、房间存在（否则 `kEntityIsNull`）、房间公开（否则 `kInvalidParameter`）。已是广播档则直接回 `broadcast = true`，不挂 gate；否则挂上 gate，只给这台 gate 补发一帧 STATE（当前快照编码成 161，`observer_count = 0`），回实例与期限。操作幂等。
- **PromoteSpectateBroadcast**：校验同上；置广播档，往广播 topic 发一帧 STATE；幂等。
- **每回合**（`BroadcastTurnResult`）：观众版 158 编码一次。L0 直连照旧；广播档往广播 topic 发 1 条；否则给每台中继 gate 各发 1 条。
- **结束 / 作废**：发 END（编码好的 166）：未升档时发给每台中继 gate；已升档时**只发广播 topic**（与最后一回合 TURN 同分区保序；两条 topic 之间没有顺序，中继 END 可能越过广播 TURN，观众就看不到最后一回合）。节点停机且结果未落定的房间不发 END（与 L0 同口径，见 §5.3），中继观众靠 gate 的空闲超时收尾。
- 帧的 `round_index` 取 `TurnResultS2C.round_index`；STATE 取快照的 `round_index`；END 取最后一回合的 `round_index`。

### 15.5 gate（`SpectateHub`，C++；Java 同语义）

- **表**：`battle_id → Entry{battle_instance_id, 订阅会话集, 最新状态帧缓存, last_round, last_frame_steady, deadline_ms, ended}`，另有 `session_id → battle_id`。全部在主 loop 上；只存 session_id，发送时查会话表并 `lock()` weak 连接（§11.7）。
- **SubscribeSpectate**：
  - 先验签，再核对 `payload.player_id == 会话 playerId`、`battle_id` 一致、按 gate 墙钟未过 `expire_at_ms`；任一不符回 `MatchSpectateTicketInvalid`。
  - 会话已订别的场时先退订旧场。
  - 条目里的实例与凭证不同时，以凭证为准，清掉缓存。
  - 有缓存就立刻下发 161：缓存本身是 161 原样发；缓存是 158 就取其 `state` 合成 161，只在订阅时解析一次。
  - 回执用带请求 id 的 MessageContent。
- **UnsubscribeSpectate**：会话从条目摘除，回执；会话断开时 gate 自己摘除。
- **收帧**：
  - 实例与条目不符就丢弃。
  - 两条水位（2026-10-30 修正）：`lastTurnRound` 只由 TURN 推进，TURN 只收 `round_index > lastTurnRound`；`cachedStateRound` 记缓存状态所处的回合（STATE(r) 记 r，TURN(r) 记 r + 1），STATE 只收 `round_index ≥ cachedStateRound`，只更新缓存，不下发给已在看的会话。STATE(r) 是第 r 回合结算前的快照，所以不能用它推进 TURN 的水位，否则紧接着的 TURN(r) 会被当重复丢掉；TURN 晚于更新的 STATE 到达时照常扇出，但不把缓存往回换。END 只收一次，扇出后标记 ended，10 分钟后删除。
  - 没有订阅者的场次也建条目缓存帧（L2 每台 gate 都会收到，B 封顶），无人订阅且 30 s 没有新帧就删除。
- **扇出**：
  - 同一帧只用 gate codec 编码一次，各会话 `conn->send(data, len)`，复用 C 批 `gate_push` 的编码一次接口。
  - 每次最多发 `kFanoutBatch = 512` 个会话，剩下的 `queueInLoop` 续发，闭包只捕获 weak。
  - TURN 到达时，上一帧还没发完的会话直接改发新帧，名单从上一轮停下的位置接着发（否则过载时表尾会话被持续饿死）；**END 不合并**：等在途的最后一回合发完再开始扇出 166，因为 166 不带状态，抢占了就看不到最后一回合。
  - 会话输出缓冲超过 256 KB 就跳过这一帧并计数，不等 2 MB 被强关。
- **空闲巡检**（定时器只绑 weak）：有订阅者的条目超过 `T_idle = 30 s` 无帧，或超过 `deadline_ms + 60 s`，就给订阅者合成 166（`SOURCE_LOST`）并删除条目。
- **指标**：每 30 s 一行 `metric=gate_spectate`，字段为订阅会话数、条目数、扇出帧数、软水位跳帧、合并跳帧、凭证拒绝、源站失联收尾。不带 battle_id，也不带 player_id。

### 15.6 match（Go；Java 同语义）

- **配置** `Spectate`：
  - `RelayEnabled`（bool，见 15.1）；
  - `BroadcastMaxConcurrent`（B，默认 4，0 = 不开 L2）；
  - `BroadcastAutoGateThreshold`（G_min，默认 32，0 = 只认运营标记）；
  - `TicketTTLSeconds`（默认 60）；
  - 密钥取部署注入的 `spectate_token_secret`。prod 下空密钥拒启，dev / test 放行并 WARN，口径同 battle 令牌。
- **Redis**（全部带本场 hash tag，TTL 同观战记录）：
  - `spectate:tier:{id}`：整数，只升不降，Lua 比较后设置；
  - `spectate:relay_gates:{id}`：HASH，field = gate_node_id，value = gate_instance_id；
  - `spectate:bcast:active`：ZSET，全服 L2 配额，score 是到期毫秒；
  - `spectate:featured:{id}`。
- **WatchBattle**（本地与全局两条路径共用同一个档位判定函数）：
  1. 现有校验依次做：可见性闸、互斥、离线、清退旧观战。
  2. 读档位。
     - **DIRECT**：走现有 L0。battle 回 `kMatchSpectateFull` 时：
       - `RelayEnabled` 打开：档位升到 RELAY，转第 3 步。
       - 未打开：回 16021。
     - **RELAY**：转第 3 步。
     - **BROADCAST**：只签凭证，回 LOBBY。
  3. **L1 准入**：
     - 查 HASH：本 gate（node, instance）还没挂，就调 `AttachRelayGate`；成功后再写 HASH（先 RPC 后记账；并发的重复 Attach 是幂等的）。
     - 回包 `broadcast = true` 时把档位同步成 BROADCAST。
     - 判定自动升档：`HLEN ≥ G_min`，或本场带 featured。
     - 签凭证，回 LOBBY。
  4. **升 L2**：
     - Lua 抢配额：先清过期项，`ZCARD < B` 或本场已在集合里，就 ZADD。
     - 调 `PromoteSpectateBroadcast`：成功则档位置为 BROADCAST；失败则 ZREM 退还配额，保持 RELAY。
     - 抢不到配额就留在 RELAY，不报错。
- **全局模式的幂等**：
  - L1 / L2 观众照常走 `SpectatorIntent`（请求头幂等与顶替旧请求），但不调 `BeginSpectator`、不锁 formation、不写 `match_spectator` 占用行，直接以 `delivery = LOBBY` 结案。
  - L0 满员转 L1 的那次请求，同样以 `delivery = LOBBY` 结案，不写 16021。
  - 重放结案为 LOBBY 的意图时，重新做幂等的 L1 / L2 准入，并签新凭证。
  - 清退玩家旧的 L0 观战，复用现有的取消流程。
- **运营入口**：
  - `MatchInternal` 新增内部 RPC `SetSpectateFeatured{battle_id, featured}`，只认公开局；置位时立即尝试升 L2。
  - 观战列表带 `featured`，赛事局排在前面。
- **指标**：`match_watch_battle_total{outcome}` 新增 `lobby`；新增 `match_spectate_tier_total{tier}`、`match_spectate_promote_total{result}`（result ∈ ok / quota_full / rpc_failed）。

### 15.7 部署与配置

- 新密钥 `MMORPG_SPECTATE_TOKEN_SECRET`：注入 gate 的 BaseDeployConfig 与 match 配置。要求至少 32 字节，并与 gate / battle 令牌密钥都不同，校验口径同 `MMORPG_BATTLE_TOKEN_SECRET`。本地 `bin/etc/base_deploy_config.yaml` 与 `go/match/etc/match_service.yaml` 填 dev 值。
- 运营入口密钥 `MMORPG_MATCH_OPS_CALLER_SECRET`：注入 match 的 `Spectate.OpsCallerSecret`，可空（空 = `SetSpectateFeatured` 关闭），非空时至少 32 字节。
- 带 `MMORPG_MAINTENANCE_CONTROL_DIR` 启动的 gate（本地受控维护流程，K8s 不用）不注册广播 topic 消费者：这台 gate 上的观众在场次升档后收不到帧，30 s 后收到 `SOURCE_LOST`，不碰资产。走这个流程的部署把 `Spectate.BroadcastMaxConcurrent` 置 0。
- `Spectate.RelayEnabled` 在 K8s 配置里写死 true（单 Kafka 集群）；多 Kafka 集群部署前必须改 false，否则跨集群的 gate 收不到中继帧（Q6）。
- 建 topic：见 15.3。
- **Java**：内部通道按 Java 惯例走 Redis pub/sub。中继发到目标 gate 的推送频道；广播用分片 pub/sub，按场开频道，gate 有本机订阅者时订阅。客户端契约同 15.2。

### 15.8 落码分批

| 批 | 内容 | 状态 |
|---|---|---|
| B | 可见性闸（§10 第 6 条） | 落码中 |
| C | gate 推送身份栅栏、Scene / All 两个广播 handler、编码一次、空 payload 拒绝 | 落码中 |
| P0 剩余 | 观众帧编码一次、观众席节点配额 | 落码中（与 B 同批） |
| P1 | 15.2 的协议、15.4 battle 中继、15.5 SpectateHub、15.6 L1、15.7 部署 | 待 B / C 落稳后开工 |
| P2 | 15.6 升 L2 与配额、featured、广播 topic 消费 | 与 P1 同批的 battle / gate 部分一起落，match 侧同批 |
| Java | B + 本地模式原操作 + P0 剩余 → P1 / P2 | 前三项落码中；P1 / P2 在 C++ / Go 落稳后对齐 |

### 15.9 评审后的定稿决策（2026-10-30，负责人授权按标准做法定）

三轮语义评审（`docs/handoff/spectate-tiered-fanout-20261030/reports/review-*.md`）之后定下，与前文冲突时以本节为准：

- **观战原件的关闭放宽**：原先只有拿到 battle 的取消证明才能把 `match_spectator` 推到 state 5，battle 实例崩溃后这些玩家再也排不了队。现在拿不到证明时，满足任一条件就关闭：成局 `state == 5`；已过成局 `deadline_ms` + `terminalOutcomeGraceMs`；注册表里同 node_id 已是别的 uuid（查不到不算）。依据：观众对战斗状态零写权，Add 带 `target_instance_id`，新实例不接纳旧 op，放宽不碰资产，最坏是僵尸实例多发几帧。
- **观战 Redis 键的 TTL**：全局模式按最长战斗期限算（模板时长最长 1 小时），不再是 `BattleMaxDurationSeconds + 60`；否则档位和赛事局标记 6 分钟就失效，L2 配额上限也会失守。
- **结束即释放**：`CommitBattleOutcome` 拿到结论后尽力摘掉本场的活跃索引、L2 配额、赛事局索引和档位键；另有全局模式的低频清理循环兜底。
- **L2 配额账**：`AttachRelayGate` 回 `broadcast = true` 时补记配额（超额也记）；Promote 失败退还前先复读档位，已是 BROADCAST 不退；升档 RPC 前加 3 秒短租约，battle 过载时不被观众请求放大。
- **同 op 重放回首次拒绝码**：battle 墓碑记下首次拒绝码，同 op、同请求重放回同一结论，前台请求与后台恢复并发时结论一致。
- **凭证**：签发前查注册表，来源实例已换就按房间不存在处理，不给死实例签凭证；Go、C++、Java 共用一组跨语言固定向量（secret `spectate-cross-language-vector-secret-0123456789`，payload hex `080110021a016920032804`，签名 `81e49d087e684b9df52c0c7bde47b88afce761af48f37b8dda6bdc3f01c2b0d8`）。
- **运营入口**：`SetSpectateFeatured` 未开启或验签失败回 `SPECTATE_FEATURED_REJECT_DENIED`，不回 gRPC PermissionDenied：拒绝发生在副作用之前，结局确定，不能拖住实例的排空证明。
- **意图保留期**：`match_spectator_intent` 已关闭且不是请求头的行，创建满 7 天后由低频任务分批删除（新增 `created_ms` 列与索引）。依据：同 request_id 的重放窗口是秒级，战斗最长约 1 小时。
- **登录 Bind 之前的失败提示**：`PushToPlayerEvent.player_id` 显式留 0（兼容放行），这是「生产者拒发 0」的唯一例外；否则未绑定会话上的 3023 提示会被身份栅栏丢掉，客户端要干等 60 秒。

**已知残余（不阻断）**：
- 半开 TCP 占座：直连没有应用层心跳，靠 battle 的 TCP 断开才计宽限。
- 墓碑表满时整个节点拒绝观战（有界，30 秒内自清）。
- 恢复循环对已崩溃实例的 RPC 失败仍会 `HoldRelay`，这类 match 副本的排空证明要人工处理（`go/shared/drainproof`，属于节点生命周期设计）。
- Go 全局路径的 16018 记指标时不细分原因。
