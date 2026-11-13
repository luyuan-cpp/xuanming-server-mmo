# 换场景节点期间的玩家消息：客户端请求在 gate 暂存并按序重投（场景 lane）

**Created:** 2026-11-12
**状态:** 已落码，定案见 §4.1。**未生成协议、未编译、未测试**，待 Codex 按 §9 验证。Java 版待做（§10）。
**关联:** [client-request-ordering.md](./client-request-ordering.md)（路由 lane，O1–O14；本文修订其 O2 对 TCP 路径的表述）、[muduo-rpc-channel.md](./muduo-rpc-channel.md) §8.1（gate → scene 客户端消息）、[client-rpc-callback.md](./client-rpc-callback.md) §7 R1（scene 早退回 1003）、[cross-zone-scene-travel.md](./cross-zone-scene-travel.md) §14（连续换场景期间的消息投递，设计参考）、[reliable-player-inbox-20261014.md](./reliable-player-inbox-20261014.md)（重要消息的持久待办原则）、[player-inbox-service.md](./player-inbox-service.md)、[guild-phase2/04-asset-channel.md](./guild-phase2/04-asset-channel.md)、[gate-drain-session-migration.md](./gate-drain-session-migration.md)。

**证据标注:** 【码】代码事实，写函数名与文件，不写行号（这些文件改动频繁）。【推】静态阅读得出的推断，未运行验证。

---

## 0. 结论速览

1. **问题（负责人 2026-11-12）**：别的玩家或别的服务器给某个玩家发了要他处理的消息，他正好在换场景，甚至连续换好几次。换服过程中的消息到了新服务器还会不会处理、会不会乱序。
2. **服务器发给玩家的重要消息，本来就不跟着位置走**（§2）。资产操作、战斗结算、GM 处置各有持久待办，由发送方保管。玩家不在或在交接时，scene 答「不在 / 稍后」；落到新节点后，发送方重投或新节点主动拉，按账本去重。连续换几次节点只会晚一点，不会丢，也不会执行两次。逐条对过代码，这部分没有改。
3. **缺口在客户端发给 scene 的请求**（§3）。交接窗口里这些请求一律被拒：旧节点冻结期间各业务系统回各自的拒绝码，实体销毁后与新节点载入期间 gate 回 1003。请求确定没执行，但要客户端自己重发。服务器发起的换节点（组队跟随跨节点、计划排空疏散、世界频道改派）客户端事先不知道，玩家看到的是点了没反应或报错。
4. **修法：gate 给每个会话加一条场景 lane**（§4）。发往 scene 的客户端消息按收包顺序发出，请求-响应消息一次只放一条在途。scene 明确答复「确定未执行、稍后重投」的请求留在 gate 的会话队头，约每 250 ms 重投一次，落到新节点（或交接未成、原地解冻）后按原顺序执行。最长等 2 秒，超过回 1003，与改造前相同。连续换几次节点，队列一直在 gate 的会话上。
5. **只带「换节点执行含义不变」的请求**。scene 按白名单判：背包与装备、属性、宝宝、任务、货币、活跃度、战斗结果七个服务的请求，以及技能服务里的看技能、升技能。它们读写的是玩家自己的状态，落到哪个节点含义都一样。换图、跨 zone 传送、瞬移、放技能、神秘商人这类绑在当前场景上的请求不带，照旧当场处置，否则会被拿到新场景里执行（§4.3）。
6. **只重投「确定未执行」的请求**。超时、断连这类结果未知的失败绝不重投，照旧回 1003，由客户端按幂等键决定是否重发（[客户端请求幂等键](./client-request-idempotency.md)）。
7. **执行顺序与改造前相同**（§4.4）。单向消息（移动等）不等回包：排在它前面的请求都已发出就发，所以连接上的顺序仍是收包顺序，在途请求再慢也挡不住移动。唯一的例外是队头请求在等重投时，单向消息不等它；能被重投的请求与场景无关，与移动谁先谁后不影响结果。
8. **旧场景的消息不在新场景执行**（S13）。一条消息若在 gate 里排着等过一次换场景或换节点、换完才发出，gate 在转发信封上标出来；含义绑在旧场景上的（瞬移、放技能、神秘商人、移动等）scene 不执行，与改造前交接窗口里一律被拒的结果相同；换节点含义不变的照常执行。
9. **客户端契约不变**：消息格式、消息号、错误码都没有新增，客户端代码不用改（§4.7）。可见的变化：交接窗口里白名单内的请求大多不再报错而是稍晚成功；同一会话发往 scene 的请求-响应消息一次一条在途。
10. **做不到的**（§8）：跨 zone 传送要换 gate，旧 gate 暂存的请求等不到目标节点，当场回 1003，由客户端到新 gate 重发；跨 zone 窗口里推给客户端的通知丢失、不补发的既有限制不变。

---

## 1. 两个问题分开看

| 方向 | 例子 | 谁保管未完成的消息 | 换节点时 |
|---|---|---|---|
| 服务器 → 玩家所在的 scene | 交易成交入账、帮会扣款、邮件附件、战斗结算、GM 封禁 | 发送方的持久待办（§2） | 发送方重投，或新节点落地后主动拉 |
| 客户端 → 玩家所在的 scene | 换装备、强化、加点、领任务奖励、领活跃度奖励 | **改造前没有人保管**，被拒后靠客户端重发 | 本文：gate 的会话场景 lane 保管并重投 |

公开的成熟做法对迁移窗口里的在途消息有三种处理（[reliable-player-inbox-20261014.md](./reliable-player-inbox-20261014.md) §3）：旧节点拒收、调用方刷新位置后重试（Dapr、Service Fabric）；旧节点有限次数转发（Orleans）；路由层缓冲、交接完成后按序放行（Akka Cluster Sharding 的 ShardRegion）。重要业务另外要有按收件人身份保存的持久待办，推送只是提醒。

本仓的对应关系：
- 服务器 → 玩家：持久待办加「拒收后重投」，已有。
- 客户端 → scene：本次补「路由层缓冲」。路由层就是 gate：只有它看得到客户端的收包顺序，会话也不随场景节点变。缓冲只收「确定未执行」的请求，所以不需要去重，也不会执行两次。

---

## 2. 服务器发给玩家的消息：现状核对（本次没有改）

| 消息 | 通道 | 玩家不在本节点 / 正在交接 | 落到新节点之后 | 顺序与去重 |
|---|---|---|---|---|
| 资产操作（trade、guild、mail、match、data_service 回收） | 发送方发件箱 + scene 的 `AssetDebit` / `AssetCredit` / `AssetAbortDebit` / `AssetAckReceipt` | 不在回 `NOT_HERE`；交接冻结、退出中、归属未铸造回 `RETRY`，都不记账【码】`asset_op_system.cpp` 的改动闸。发送方对这两种结局只会退避重投，不终结【码】`go/shared/assetop/decide.go` `Decide` | 进场钩子 `PlayerLandingSystem::OnEnteredScene` 发唤醒，发送方立刻重投该玩家的未决行【码】`player_landing.cpp` | 每条流 `(stream, epoch, seq)` 的账本随玩家存档一起落盘，重投按账本回原结局；`durable` 以落盘快照为准。不同操作之间不保证先后，要先后的业务自己串（等上一笔终结再发下一笔） |
| 战斗结算 | battle 的结算发件箱 + Kafka `BattleConfirmedEvent` 提醒 | 冻结、交接、退出中不应用【码】`player_battle.cpp` | 进场钩子 `PlayerBattleSystem::OnPlayerEnterScene` 补应用 | 按 `battle_id` 条件销账 |
| GM 处置等「改 scene 持有状态」的待办 | 玩家收件箱 `go/player_inbox`，每个（玩家，域）一个有序邮箱 | 不可改时延后【码】`player_inbox_system.cpp` | 落地无条件拉一次；游标与效果同一次落盘后才确认 | 按 seq 升序执行，重复投递按（seq，request_id，类型）回放原结局 |
| 提醒（组队刷新、收件箱提醒、托管落点） | Kafka `scene-{node_id}`【码】`scene_kafka_command_router.cpp` 只有这四种事件 | 不在就丢 | 落地时由 scene 主动查权威状态 | 提醒可丢，权威状态在各自的服务里 |
| 推给客户端的通知 | Go 服务 → Kafka `gate-{id}` → 客户端 | 同 zone 换场景节点时 gate 不变，不受影响 | 跨 zone 传送要换 gate，窗口内会丢且不补发，靠客户端落地后按版本摘要拉（[cross-zone-scene-travel.md](./cross-zone-scene-travel.md) §7、[landing-recovery-sync-digest.md](./landing-recovery-sync-digest.md)） | — |
| 场景时效消息（技能伤害、AOI） | scene 内部 | 按契约丢弃 | 不追到新场景 | — |

这张表回答了「换服过程中的消息到新服务器还会不会处理」：重要消息会，因为保管它的是发送方，不是某个场景节点；提醒类不会原样送达，但新节点落地时会自己去查；场景时效消息不会，也不应该。

没有持久通道的新业务要给玩家发「必须处理」的消息时，按 [player-inbox-service.md](./player-inbox-service.md) 的域准入接入玩家收件箱，不要直接发 Kafka 提醒了事。

---

## 3. 缺口：客户端发给 scene 的请求

改造前，一次同 zone 跨节点换图（A → B）里，客户端请求按时间落在四段窗口里，全部被拒：

| 窗口 | 改造前的结果 | 证据 |
|---|---|---|
| A 上交接冻结（挂着 `PlayerTravelHandoffComp`） | 照常派发；只读请求照答，写请求被各系统的冻结闸拒掉，码各不相同：`kEquipFrozen`、`kInvalidParameter`（属性、宝宝、任务、整理背包）、`kSkillUpgradeBusy`、`kBagTemporaryRetrieveBusy` | 【码】`player_equip.cpp`、`player_attribute.cpp`、`player_pet.cpp`、`player_mission.cpp`、`player_skill.cpp`、`player_feature_snapshot.cpp` 的 `CheckWritable` 一类 |
| A 已销毁实体，gate 的路由还没切到 B | A 回空应答（session id not found 或 player not loaded），gate 回 1003 | 【码】`SceneHandler::ProcessClientPlayerMessage`、`scene_client_call_reply.h` `Decide` |
| gate 已切到 B，B 还在入场登记或载入存档 | B 回空应答，gate 回 1003 | 同上 |
| 路由已到 gate，进场还没转发给 scene（`pendingSceneEntry`），且会话此前没有 scene 节点 | gate 自己回 1003 | 【码】gate 转发 scene 消息时的「Scene not ready」 |

四段都是「确定未执行」。本次处理前三段。第四段只出现在首次进游戏（会话此前没有任何 scene 节点），客户端在等进游戏的结果，不会发玩法请求，仍由 gate 回 1003；换场景节点时会话一直指向上一个节点（[cross-zone-scene-travel.md](./cross-zone-scene-travel.md) CPP-2 不变量 I5），落在前三段。

改造前的问题有三个：
- 要客户端自己重发。服务器发起的换节点客户端事先不知道，没有加载界面挡着，玩家正在点的操作直接失败。
- 冻结期间的拒绝码不统一，`kInvalidParameter` 让客户端分不清「稍后再试」和「参数错」。
- 连续换节点（A → B → C）时，每一段窗口都再来一遍。

---

## 4. 设计

### 4.1 决策（全部定案）

| # | 决策 | 理由 |
|---|---|---|
| S1 | 暂存点放在 gate 的会话上（`SessionInfo::sceneLane`） | 只有 gate 看得到客户端的收包顺序；会话不随场景节点变，连续换节点时队列不用搬；会话 erase 即取消 |
| S2 | 只重投 scene 明确答复 `RETRY_LATER` 的请求。RPC0 失败（超时、断连、对端拒绝）一律不重投；gate 自己不决定暂存谁 | 结果未知的失败重投可能让请求执行两次；确定未执行的重投不需要去重。gate 不认识业务方法，判断不了一条请求换节点执行对不对，这件事只能由 scene 说 |
| S3 | 发往 scene 的请求-响应消息按会话单飞：一次一条在途，上一条完成后才发下一条 | 重投和并发发送放在一起必然乱序（被拒的旧请求晚于后发的新请求执行）。单飞之后顺序只由队列决定。与路由 lane 同一模型。这一条修订了 [client-request-ordering.md](./client-request-ordering.md) O2「TCP 路径不进 lane」：那句话对路由 lane 仍成立，TCP 路径现在有自己的 lane |
| S4 | 单向消息（内层响应为 Empty，移动等）不等回包：前面没有还没发出的请求就马上发（不建排队项）；有，就排在它们后面，它们发出后马上跟着发。队头请求在等重投时，单向消息直接发出，不等它，原先排在它后面的单向消息也立即发出 | 连接上的发出顺序与收包顺序相同，scene 按连接顺序执行，执行顺序与改造前一致；在途请求再慢也挡不住移动。等重投时不等的理由：能被重投的请求与场景无关（S11），与移动谁先谁后不改变结果；让移动等它，落地之后会把旧场景里攒下的移动一次性放进新场景，或者把落地后最初一段的移动丢掉。单向消息照旧发到会话此刻的路由上，由 scene 决定收不收，与改造前相同 |
| S5 | 从收包起最多等 2000 ms（排队加等重投），超过时请求回带请求 id 的 1003，单向消息丢弃 | 预算链 I6（§5）允许的上限：12000 + 2000 + 1000 ≤ 15000。定案，不等压测 |
| S6 | 重投由定时器驱动，间隔不小于 200 ms，实际是下一个 0.25 s 的拍；不依赖路由变更事件 | 交接未成、原地解冻时 gate 看不到任何事件，只有定时重投能发现；路由刚切过去时新节点通常还没载入完，立刻重投多半白发 |
| S7 | scene 在入口统一判，规则收在一个函数里（`scene_handoff_gate::ShouldAnswerRetryLater`）：会话不在、实体没载入、归属交接在途三种状态下，白名单内的请求答复 `RETRY_LATER`；交接在途时这些请求不派发 | 交接期间本节点的状态已落盘且不得再写，写请求派发了也只会被拒；只读请求答了也是马上作废的数据 |
| S8 | 每会话排队加在途上限 64，请求超限回 1008，单向消息超限丢弃 | 进场后客户端会连发一批面板查询；单飞之后它们要排队，上限取路由 lane（32）的两倍 |
| S9 | 在途调用过了「RPC0 超时 + 1000 ms」仍没有完成回调时判丢，回 1003；由定时器主动扫描 | RPC0 承诺恰好回调一次，这只是纵深防御。必须主动扫：队头卡住时客户端在等回包，不会再发新请求来触发惰性清理（路由 lane 同一条教训） |
| S10 | gate 排空迁移遇到场景 lane 不空的会话，本拍不迁 | 免得客户端到新 gate 上发的请求抢在旧 gate 上还没执行的请求前面 |
| S11 | 只有「换节点执行含义不变」的请求才答复 `RETRY_LATER`。按服务列白名单，同一服务里有绑场景方法的按方法列，默认不带 | 换图、瞬移、放技能、神秘商人购买的含义绑在当前场景上，带到新场景执行是错的。默认不带：新加的服务或方法不登记就保持改造前的行为，漏登记只是少一点无感，不会出错。写在 scene 的代码里而不是 proto 选项里：这些服务的 proto 是与 Java 版共享的客户端契约，加自定义选项要两版同批改 |
| S12 | 会话被重定向到别的 gate（跨 zone 传送推 msg 124）时，还没被受理的请求当场回 1003，不再重投 | 玩家正在离开，这里等不到受理；当场回执让客户端立刻到新 gate 重发，不必干等 2 秒 |
| S13 | gate 记下每条排队消息收到时会话绑的场景路由（场景实例 + scene 节点），发出时与此刻不同就在转发信封上标 `received_on_previous_route`。scene 对带这个标记、且不在白名单里的消息不执行（请求回 1003，单向消息丢弃）；白名单内的照常执行 | 排队让消息可能跨过一次换场景才送到。白名单外的消息（瞬移、放技能、神秘商人、移动等）是在旧场景里发出的，坐标、目标、交互距离都属于旧场景；改造前它们在交接窗口里一律被拒，从不在新场景执行，这里保持同一结果。gate 不认识业务方法，只能如实标出「换过路由」，由 scene 按白名单判。路由在 gate 提交进场转发之后才变，而客户端要等新节点推 NotifyEnterScene 才知道换了场景，所以客户端落地后发的消息收到时路由已是新的，不会被误标 |

### 4.2 数据结构

全部只在 gate 唯一的 EventLoop 线程上读写，不加锁，不新增线程。

| 状态 | 位置 | 说明 |
|---|---|---|
| `SceneLane<SceneLaneItem> sceneLane` | `SessionInfo` 新成员 | 队列、在途序号、在途那一条本身（重投要用）、重投时刻 |
| `SceneLaneItem` | `cpp/libs/services/gate/session/comp/scene_lane_item.h` | 客户端原包（shared_ptr）、收包时的 trace、收包时刻、收包时会话绑的场景路由（`SceneRouteStamp`：场景实例 + scene 节点实体）、请求 id、消息号、是否单向、被放回队头的次数 |
| 在途索引 `InflightIndex<InflightCall>` | `client_scene_lane.cpp`，thread_local | 每次发出的请求及其判丢截止；完成时注销，定时器扫过期项。与路由 lane、`RpcChannel` 共用 engine/core 的同一份实现 |
| 等重投登记表 `session_id → 会话代次` | 同上 | 定时器只扫这张表，不扫全部会话；会话没了或代次变了就摘掉 |
| 调用序号 | 同上 | 进程内单调，每次发出（含重投）换新号；完成回调按它认领 |

纯逻辑（排队、单飞、放回队头、单向消息何时能发）在只依赖 STL 的 `scene_lane.h`，时刻由调用方传入。装配层三个入口 `Admit` / `OnSceneCallDone` / `Tick` 也都由调用方传入当前时刻，单测不读真实时钟。

生命周期（AGENTS §11.7）：排队项、在途索引与回调都不持有 `TcpConnectionPtr` 或裸 `this`；每次下发先按 session_id 找会话、核对会话代次，再 `conn.lock()`。

### 4.3 scene 侧：什么时候答复「稍后重投」

`ProcessClientPlayerMessageResponse` 新增字段 `dispatch_verdict = 3`，枚举 `ClientMessageDispatchVerdict`。这是 gate 与 scene 之间的内部契约，不是客户端可见契约。`RETRY_LATER` 的含义：请求确定未执行，而且等一等就能受理；应答不带 `message_content`。

**哪些请求可以答复它**（`cpp/nodes/scene/handler/rpc/player/player_handoff_gate.h` 的 `scene_handoff_gate::MayBeRetriedLater`）：应答不是 Empty 的请求-响应方法，且在白名单里。

| 服务 | 可以换节点再执行 | 原因 |
|---|---|---|
| `SceneBagClientPlayer`、`SceneAttributeClientPlayer`、`ScenePetClientPlayer`、`SceneMissionClientPlayer`、`SceneCurrencyClientPlayer`、`SceneActivityClientPlayer`、`SceneBattleResultClientPlayer` | 整个服务都可以 | 只读写玩家自己的持久状态，不引用当前场景里的东西 |
| `SceneSkillClientPlayer` | 只有 `ListSkills`、`UpgradeSkill` | 看技能、升技能读写的是玩家自己的技能表；同一服务里的 `ReleaseSkill` 的目标与位置是旧场景的 |
| `SceneSceneClientPlayer` | 只有 `GetSyncDigest`、`SceneInfoC2S` | 换图、跨 zone 传送：交接中再来一次换图是重复请求，handler 有自己的在途闸（`kEnterSceneChangingScene`）；留到落地之后执行会变成第二次换图。取版本摘要读的是玩家自己的各域版本；查场景信息答的是此刻所在的场景，落地后答新场景正是客户端要的。这两个都是单向消息，不会被扣住重投，登记它们只为不被当成旧场景的消息丢掉（S13） |
| `SceneMovementClientPlayer`（瞬移、移动、跟随） | 否 | 坐标是旧场景的 |
| `SceneMysteryMerchantClientPlayer` | 否 | 购买要求玩家与商人在同一场景、在交互距离内【码】`PlayerMysteryMerchantSystem::Buy` 的 `kMysteryMerchantNotHere` / `kMysteryMerchantTooFar` |
| `ScenePlayer`（ExitGame 等） | 否 | 退出优先，不排队 |

白名单用生成的服务描述符与方法下标比较，不用名字字符串：服务或方法改名时这里编译不过，不会悄悄失效。往整服务名单里的服务加方法之前，先确认它不引用当前场景；引用的话放到别的服务里，或把那个服务改成按方法列。

`SceneHandler::ProcessClientPlayerMessage` 在三处调同一个 `ShouldAnswerRetryLater(EntryState, player, method)`：

| 状态（`EntryState`） | 改造前 | 现在（白名单内的请求） |
|---|---|---|
| `kSessionUnknown`：会话不在本节点的 SessionMap 里 | 空应答 | 空应答 + `RETRY_LATER`。玩家刚被放行去了别的节点而 gate 的路由还没切过来，或入场登记还没走完，都是过渡态 |
| `kPlayerNotLoaded`：玩家实体还没载入 | 空应答 | 空应答 + `RETRY_LATER` |
| `kPlayerPresent` 且归属交接在途（`PlayerTravelHandoffComp`） | 照常派发 | 不派发，空应答 + `RETRY_LATER` |

白名单外的请求与单向消息在这三处的行为与改造前完全相同。

**旧场景消息闸**（S13，`scene_handoff_gate::IsStaleForThisScene`）：紧接在交接判定之后，转发信封带 `received_on_previous_route`、且方法不在白名单里的消息不派发。请求-响应类不带 `message_content`，gate 回带请求 id 的 1003（确定未执行）；单向消息不回包。白名单内的消息（包括被扣住后重投到新节点的那条）照常往下走。

第三处排在退出闸之后（退出优先）、GM 闸与热关停之前（那两道由最终执行的节点判）。只挂 `PlayerFrozenComp`、没有 `PlayerTravelHandoffComp` 的冻结（账号角色交易封存）不算：那不是换节点，等不到目标节点；照旧派发，由各系统的冻结闸当场拒绝。各业务系统自己的冻结闸全部保留，作纵深防御。

其余早退（消息号 / 服务 / 方法查不到、反向身份栅栏、旧会话、请求解析失败、字段校验）不填这个字段：等也没用，gate 照旧回 1003。退出中（`UnregisterPlayer`）照旧回 `kFeatureUnavailable`。

「session id not found」「player not loaded」两行日志改为逐条 DEBUG、每 1024 条一行 WARN（原来分别是逐条 ERROR、逐条 WARN）：换节点窗口里这是常态，条数由客户端发包速率决定，gate 重投还会让同一条请求多来几次。

### 4.4 gate 侧：准入与发出

挂载点在 `DispatchClientRpcMessage` 的 `PROTOCOL_TCP` 分支，前面的会话 / 验签 / 白名单 / 体积 / 限流 / GM / 热关停 / LeaveGame 屏障都不变。所有 scene 消息（单向与请求-响应）都经 `gate_scene_lane::Admit`。

```
Admit(消息, oneWay, now):
    oneWay 且 (队列为空 或 队头在等重投) → 直接发出,不建排队项(S4)
    入队:满 → 请求回 1008,单向丢弃
    Pump

Pump(now):  按队列顺序,直到遇到不能发的那条
    队头是单向消息 → 发出(它前面的请求都已发出),继续
    队头是请求 → 有在途、或在等重投且没到重投时刻 → 停
    取出这一条
    从收包起已超过 2000 ms → 请求回 1003,单向丢弃;继续
    发送函数 SendClientMessageToScene:
        kNoScene → 回 1003(单向消息同样回,请求号为 0);继续
        kSent    → 请求记在途、登记判丢截止;继续(紧跟在它后面的单向消息可以马上发)
    每次发出(含重投与等重投时马上发出的单向消息):收到时的场景路由 ≠ 此刻的 → 信封标 received_on_previous_route(S13)
```

发送函数 `SendClientMessageToScene`（`client_session_handler.cpp`）只管怎么发：单向消息 `SendRequest`，请求-响应消息 `CallMethod` 并登记每次调用回调（只捕获会话号、会话代次、调用序号）。会话没有可用的 scene 通道（没进游戏、路由已清、节点还没连上，或单向发送被本地拒收）时回 `kNoScene`：确定没发出，与改造前的「Scene not ready」相同。gate 在这里不暂存：它不知道这条请求换节点执行对不对（S2）。

换场景节点时，路由事件到了 gate 但进场还没转发给新节点（`pendingSceneEntry`）的那一段，会话仍指向上一个节点。消息照旧发往旧节点，由旧节点答复要不要稍后重投。

### 4.5 完成处置与定时驱动

RPC0 的完成回调转给 `gate_scene_lane::OnSceneCallDone`：

```
注销在途登记(不管会话还在不在)
找会话并核对代次;找不到 → 丢弃
按调用序号认领在途那一条;认领不到(已判丢)→ 丢弃,计 late_dropped
按 scene_client_call_reply.h 的 Decide:
    有 message_content            → 原样下发
    没有内容但 RETRY_LATER         → 会话已重定向:回 1003
                                      超过等待上限:回 1003
                                      否则放回队头;排在它后面的单向消息立即发出(S4)
    RPC0 失败                      → 回 1003,不重投
    没有内容也没有 RETRY_LATER     → 请求号非 0 回 1003,为 0 不下发(与改造前相同)
Pump
```

每个请求恰好一个回执：在途那一条只能被认领一次；判丢之后迟到的回调因序号对不上被丢弃。带内容又带 `RETRY_LATER` 的应答以内容为准（带内容说明 handler 跑过了，重投会执行两次）。

定时器 `gate_scene_lane::Tick` 与路由 lane 的过期扫描挂在同一个 0.25 s 定时器上【码】`cpp/nodes/gate/main.cpp`，每拍三件事：
1. 在途判丢：过了截止仍没有回调的调用回 1003，放行后面的消息（S9）；
2. 重投：只对等重投登记表里的会话调 `Pump`；
3. 每 30 秒一行 `[scene_lane] summary`。

### 4.6 与其它机制的关系

- **路由 lane**：两条 lane 互相独立。发往 Go 服务的请求与发往 scene 的请求之间仍然没有先后保证，与改造前相同；唯一的跨 lane 约束仍是 LeaveGame 屏障（LeaveGame 还排在路由 lane 里时，TCP 消息在收包时回 1003，不进场景 lane）。
- **客户端 LeaveGame**：发出时清掉会话的 scene 路由。之后场景 lane 里还排着的请求轮到时拿到 `kNoScene`，回 1003。已经发出的请求与 gate 代发的 ExitGame 走同一条连接，scene 按序先处理请求。
- **断线、顶号**：lane 随 `SessionInfo` 一起销毁；在途调用的回调找不到会话就丢弃（同时注销在途登记）；等重投登记表里的残项由下一拍摘掉。断线路径不需要新增清理调用。
- **重定向（跨 zone 传送推 msg 124）**：`gate_redirect::PushRedirect` 在推 124 之前调 `gate_scene_lane::OnSessionRedirected`，与路由 lane 的 `OnSessionRedirected` 同一位置、同一理由（回执与 124 走同一条连接顺序写出，客户端先收齐回执再换连接）。排队中与等重投的请求逐条回 1003（确定未执行，到新 gate 重试安全），排队中的单向消息丢弃；在途那一条的回包照常下发，若被答复「稍后重投」也不再等，当场回 1003（S12）。推 124 之后新到的 scene 消息照旧在收包时回 1003。
- **gate 排空迁移**：`drain_migration.cpp` `Classify` 在场景 lane 不空时返回 `kSkip`（S10），所以排空迁移的推 124 不会遇到不空的场景 lane。正常会话的场景 lane 绝大多数时刻是空的：移动走直发、不建排队项，请求在途只有一个往返。
- **客户端请求幂等键**：两者互补。幂等键管「执行了但回包丢了，客户端重发」；场景 lane 管「确定没执行，gate 替客户端重投」。场景 lane 的重投不经过幂等账本，因为被重投的请求从未执行。
- **热关停、GM 闸**：由最终执行请求的那个节点判。

### 4.7 客户端可见的变化

消息格式、消息号、错误码都没有新增，客户端代码不用改。客户端把 1003 一律按「结果未知」处理（`MmorpgClient.Net.RpcStatus.OutcomeUnknown` 的注释、`ClientRequestKeys.ShouldResendOnce`），场景 lane 新增的几种 1003（排队超时、等重投超时、重定向时还没被受理）其实都是确定未执行，按结果未知处理同样安全。客户端请求超时是 15 秒（`GameClient.RpcTimeoutSec`），I6 按它推导。

| 情形 | 改造前 | 现在 |
|---|---|---|
| 同 zone 换场景节点的窗口里发白名单内的请求 | 冻结期间各系统的拒绝码，其余窗口 1003 | 2 秒内交接出结果：照常成功，只是晚一点；超过 2 秒：1003 |
| 进游戏后存档载入期间发白名单内的请求 | 1003 | 同上 |
| 上面两个窗口里发白名单外的请求（换图、跨 zone 传送、瞬移、放技能、神秘商人） | 冻结期间由 handler 当场处置，其余窗口 1003 | 不变；若它排在被扣住的请求后面、换完场景才发出，scene 不执行，回 1003（S13） |
| 同一会话连发多条 scene 请求 | 并行在途 | 一次一条在途，回包按请求顺序到达 |
| 会话的 scene 请求积压超过 64 条 | 无此限制 | 1008（kRateLimitExceeded），不计非法包 |
| 移动等单向消息 | 收到即发 | 前面没有还没发出的请求时收到即发（绝大多数时候）；有则紧跟它们发出 |
| 跨 zone 传送时还排着的 scene 请求 | 照常发往原节点，玩家走后回 1003 | 推 124 之前当场回 1003 |

### 4.8 观测

gate 每 30 秒打一行 `[scene_lane] summary`，字段见 `client_scene_lane.h` 的 `SceneLaneCounters`。不按会话、玩家、消息号分维度（AGENTS §9）。

- `held`：至少被放回队头一次的请求数；`retry_verdicts`：「稍后重投」的次数。两者之比是平均每条请求重投几次。
- `expired_on_hold`：等了 2 秒仍没被受理。持续增长说明交接耗时经常超过 2 秒，先看 scene 的 `[TravelHandoff]` 汇总里的 `frozen_ms_max`。
- `expired_in_queue`：排在别的请求后面超过 2 秒，说明 scene 回包慢。
- `dropped_on_redirect`：跨 zone 传送时还没被受理的请求。
- `one_way_direct` / `one_way_queued` / `one_way_sent` / `one_way_dropped`：单向消息直发、排队、发出、丢弃的条数。`one_way_queued` 远小于 `one_way_direct` 是正常形态。
- `sent_on_new_route`：发出时会话已换过场景路由、信封标了 `received_on_previous_route` 的消息数（含被扣住后重投的请求）。
- `inflight_lost`、`late_dropped`：正常为 0。不为 0 说明 RPC0 的「完成回调恰好一次」出了问题。
- `in_flight`、`holding_sessions`：此刻的在途调用数与队头在等重投的会话数。

scene 一侧，交接期间留给 gate 的请求每 1024 条打一行 INFO（`request left to the gate for retry during ownership handoff`）；旧场景消息闸挡下的消息每 1024 条打一行 INFO（`scene-bound message received under a previous scene route is not executed`）。

---

## 5. 预算链

```
I6  kGateToSceneClientCallTimeout(12000) + kMaxSceneLaneWait(2000) + 1000 ≤ 客户端请求超时(15000)
```

一条请求在 gate 最多先等 2000 ms，发出后最多再等 12000 ms 的 RPC0 超时。两者相加必须早于客户端自己超时，否则客户端先放弃、gate 之后才把请求交给 scene 执行。`client_session_handler.cpp` 里有 `static_assert` 守着，改任何一个值都要重算。

I5（[client-request-ordering.md](./client-request-ordering.md) §5.2）不变：`kGateToSceneClientCallTimeout` 没有动。

2000 ms 定案为预算链允许的上限（S5）。同 zone 跨节点交接由一次存盘、一次 scene_manager 调用、一次路由事件和一次读档组成【推】，正常在 2 秒以内；超过 2 秒的回 1003，与改造前相同。要加长等待只能缩短 `kGateToSceneClientCallTimeout` 或加长客户端超时，两者都牵动 I5，不做。

---

## 6. 时序示例：连续换两次节点（A → B → C）

客户端依次发了 r1（换装备）、一串移动、r2（领任务奖励）。服务器同时在把玩家从 A 改派到 B，落到 B 之后队长又把他带去 C。

```
t=0     gate 收 r1,发给 A。随后的移动前面没有未发出的请求,直接发给 A(A 冻结中,不处理)
        gate 收 r2,排在 r1 后面(r1 在途)
t=5     A 正在交接冻结:不派发 r1,答复 RETRY_LATER
        gate 把 r1 放回队头;r2 仍排在后面,不发。之后的移动不等 r1,直接发往会话此刻的路由
t=250   重投 r1。A 已销毁实体(session 不在)→ RETRY_LATER;放回队头
t=300   gate 收到路由事件,会话指向 B,进场转发给 B
t=500   重投 r1 到 B。B 还在载入存档 → RETRY_LATER;放回队头
t=750   重投 r1 到 B。B 已载入,但又开始了去 C 的交接冻结 → RETRY_LATER;放回队头
t=1000  重投 r1。会话已指向 C,C 载入完成:执行 r1,回包下发
        gate 立刻发 r2 到 C:执行,回包下发
```

r1、r2 都只执行一次，先后顺序与客户端发出的顺序相同。如果到 t=2000 还没有节点受理 r1，gate 对 r1 回 1003；r2 从自己的收包时刻另算 2 秒。

---

## 7. 失败矩阵

| 情形 | 结果 |
|---|---|
| 交接未成，原地解冻 | 下一次重投由原节点受理，与没发生过交接一样 |
| 交接超过 2 秒 | 回 1003，确定未执行，客户端重试安全（与改造前相同） |
| scene 执行了请求但回包丢了（超时、断连） | 回 1003，结果未知，gate 不重投；客户端按幂等键重发 |
| 旧版 scene（不填 `dispatch_verdict`）+ 新版 gate | 空应答按原规则回 1003；冻结期间照旧派发。行为与改造前相同 |
| 新版 scene + 旧版 gate（不认识字段） | 空应答按原规则回 1003。交接冻结期间的白名单内请求由「各系统的拒绝码」变成「1003」 |
| gate 重启或崩溃 | 暂存的请求随进程消失；客户端断线重连后自己重发。这些请求都没执行过 |
| 客户端等不及先断线 | lane 随会话销毁，暂存的请求不再发出 |
| 完成回调丢失（不应发生） | 定时扫描在 RPC0 超时 + 1 秒时判丢，回 1003；之后迟到的回调被丢弃 |
| 重投期间 session_id 被新连接复用 | 完成回调与定时重投都按会话代次核对，不会落到新会话上 |
| 跨 zone 传送 | 推 124 之前还没被受理的请求当场回 1003，客户端到新 gate 重发 |
| 放技能、瞬移等排在被扣住的请求后面，换完场景才发出 | 信封带 `received_on_previous_route`，scene 不执行，回 1003（S13）；排着的移动同样不在新场景执行 |

混跑两行只在滚动升级时出现；项目尚未上线，首次上线整批起步，不存在混跑窗口。

---

## 8. 做不到的与边界

- **跨 zone 传送不做跨 gate 的暂存接力**（S12）。要换 gate，旧 gate 暂存的请求等不到目标节点；把它们交给新 gate 需要两个 gate 之间传递会话队列，而跨 zone 传送本身已经要求客户端重新建连、重新进场，客户端在新 gate 上重发是更简单的契约。
- **推给客户端的通知**在跨 zone 窗口里丢失、不补发的既有限制不变（[cross-zone-scene-travel.md](./cross-zone-scene-travel.md) §7），由客户端落地后按版本摘要拉取。
- **scene 节点崩溃**。接管要等节点死亡屏障（约 20 秒），远超 2 秒，请求回 1003，客户端重发。
- **两条 lane 之间没有先后保证**（§4.6），与改造前相同。
- **白名单外的请求不带**（S11）。这是定案的边界，不是待办：它们的含义绑在当前场景上。
- **队头请求等重投期间，单向消息会越过它和排在它后面的请求**（S4）。被扣住的请求与场景无关，不改变结果；排在它后面的请求本来也要等落地后才执行。
- **单飞的代价**：同一会话连发 N 条 scene 请求，总耗时从一个往返变成 N 个往返之和，单个往返在机房内是毫秒级【推】。没有压测对比，不宣称任何性能结论（AGENTS §6）。
- **scene 入口三处接线没有单测宿主**。判定规则收在 `ShouldAnswerRetryLater` 一个函数里并有单测；三处调用本身靠冒烟（§9 第 5 步）验证。

---

## 9. 给 Codex 的验证命令

工作目录都是仓库根目录。任一步失败即停，保留输出。

1. **生成协议**（`proto/scene/scene.proto` 加了枚举与字段，不生成的话下面全部编不过）：
   `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`
   通过标准：`cpp/generated/proto/scene/scene.pb.h` 里有 `CLIENT_MESSAGE_DISPATCH_VERDICT_RETRY_LATER`、`set_dispatch_verdict` 与 `set_received_on_previous_route`；robot 的 Go 绑定（`go/proto` 下 scene 包）有 `DispatchVerdict`。
2. **C++ 整解，串行**：`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`
   通过标准：0 个 `error C` / `LNK`。改了 gate 的服务库（`session_info_comp.h` 与两个新头文件）、gate 节点、scene 节点；`run_cpp_tests.ps1 -Build` 不重编库，不能代替这一步。
3. **gate 侧单测**：`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter routing_identity_test -LogDir build/verification/scene-lane-20261112`
   通过标准：全部通过，其中新增 `SceneLane.*` 14 条、`ClientSceneLane.*` 26 条、`SceneClientCallReplyTest.*` 新增 4 条、`DrainMigrationLaneBusy.InGameSessionWithBusySceneLaneIsSkipped`；原有的 `RouterLane.*`、`ClientRequestLane.*`、`GateRedirectDelivery.*`、`SceneClientCallReplyTest.*` 不回归。
4. **scene 侧单测**：`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone_test -LogDir build/verification/scene-lane-20261112`
   通过标准：全部通过，其中新增 `PlayerHandoffGate.*` 10 条。
5. **robot 编译与冒烟**（本机，两个 scene 节点，目标地图至少两条线且分在不同节点）：
   - 协议生成改了 `go/proto`，robot 以 vendor 构建：先在 `robot/` 下 `go mod vendor`，再 `go build ./... && go vet ./...`，通过标准：0 错误。
   - `pwsh -File tools/scripts/dev_tools.ps1 -Command cpp-node-start -GateCount 1 -SceneCount 2`，再以 `mode: channel-smoke` 跑 robot。
   - 通过标准：日志有 `CHANNEL_SMOKE_OK`，其中 `[channel-smoke] PASS step=C4-hold`（受理切线后立刻发的 GetCurrencyList 拿到正常回包）；等一行 gate 的 `[scene_lane] summary` 出现（30 秒一行），跨节点切线时 `held` 大于 0，`expired_on_hold`、`inflight_lost` 为 0；scene 日志有 `request left to the gate for retry during ownership handoff`。目标线与原线在同一节点时 `held` 为 0 也算通过，如实记录。`C4-hold` 为 SKIP（被限流）时如实记录。

失败时保留：msbuild 的首个错误及其上下文；`build/verification/scene-lane-20261112` 下对应工程的 stdout；robot 与 gate、scene 的日志。

---

## 10. Java 版（AGENTS §12）

**待做**，本轮不写 Java 代码，已在 Java 仓 `PARITY.md` 登记。Java 版 gate 把 scene 消息按收包顺序流水发出、不等 scene 回包（只在等 login 回包时排队）。要对齐的是：scene 对白名单内的请求答复「稍后重投」、gate 对 scene 请求单飞并放回队头按间隔重投、2 秒等待上限、单向消息的发出规则（S4）、重定向时当场回执（S12）、旧场景消息闸（S13）。客户端契约两版都不变，不需要同批。
