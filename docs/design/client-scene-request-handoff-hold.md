# 换场景节点期间的玩家消息：客户端请求在 gate 暂存并按序重投（场景 lane）

**Created:** 2026-11-12
**状态:** 已落码。**未生成协议、未编译、未测试**，待 Codex 按 §9 验证。
**关联:** [client-request-ordering.md](./client-request-ordering.md)（路由 lane，O1–O14；本文修订其 O2 对 TCP 路径的表述）、[muduo-rpc-channel.md](./muduo-rpc-channel.md) §8.1（gate → scene 客户端消息）、[client-rpc-callback.md](./client-rpc-callback.md) §7 R1（scene 早退回 1003）、[cross-zone-scene-travel.md](./cross-zone-scene-travel.md) §14（连续换场景期间的消息投递，设计参考）、[reliable-player-inbox-20261014.md](./reliable-player-inbox-20261014.md)（重要消息的持久待办原则）、[player-inbox-service.md](./player-inbox-service.md)、[guild-phase2/04-asset-channel.md](./guild-phase2/04-asset-channel.md)、[gate-drain-session-migration.md](./gate-drain-session-migration.md)。

**证据标注:** 【码】代码事实，写函数名与文件，不写行号（这些文件改动频繁）。【推】静态阅读得出的推断，未运行验证。

---

## 0. 结论速览

1. **问题（负责人 2026-11-12）**：别的玩家或别的服务器给某个玩家发了要他处理的消息，他正好在换场景，甚至连续换好几次；换服过程中的消息到了新服务器还会不会处理、会不会乱序。
2. **服务器发给玩家的重要消息，本来就不跟着位置走**（§2）。资产操作、战斗结算、GM 处置各有持久待办：发送方一直保管，玩家不在或在交接就答「不在 / 稍后」，落到新节点后重投或由新节点主动拉，按账本去重。连续换几次节点只会晚一点，不会丢，也不会执行两次。本次逐条对过代码，这部分没有改。
3. **真正的缺口在客户端发给 scene 的请求**（§3）。交接窗口里这些请求一律被拒：旧节点冻结期间各业务系统回各自的拒绝码，实体销毁后与新节点载入期间 gate 回 1003。请求确定没执行，但要客户端自己重发；服务器发起的换节点（组队跟随跨节点、计划排空疏散、世界频道改派）客户端事先不知道，玩家看到的是点了没反应或报错。
4. **修法：gate 给每个会话加一条场景 lane**（§4）。发往 scene 的客户端消息按收包顺序单飞；scene 明确答复「确定未执行、稍后重投」的请求留在 gate 的会话队头，每 200 ms 左右重投一次，落到新节点（或交接未成、原地解冻）后按原顺序执行。最长等 2 秒，超过回 1003，与今天相同。连续换几次节点，队列一直在 gate 的会话上，不跟着场景节点走。
5. **只带「换节点执行含义不变」的请求**。scene 按服务白名单判：背包与装备、属性、宝宝、任务、货币、活跃度、战斗结果七个服务的请求读写的是玩家自己的状态，落到哪个节点含义都一样，可以带过去。换图、瞬移、放技能这类绑在当前场景上的请求不带，照旧当场拒绝，否则它们会被拿到新场景里执行（§4.3）。
6. **只重投「确定未执行」的请求**。超时、断连这类结果未知的失败绝不重投，照旧回 1003，由客户端按幂等键决定是否重发（[客户端请求幂等键](./client-request-idempotency.md)）。
7. **客户端契约不变**：消息格式、消息号、错误码都没有新增。可见的变化只有：交接窗口里白名单内的请求大多不再报错而是稍晚成功；同一会话发往 scene 的请求-响应消息改为一次一条在途。
8. **没做的**（§8）：白名单外的请求与单向消息（移动等）在交接窗口里照旧被拒或丢弃；跨 zone 传送要换 gate，旧 gate 上暂存的请求等不到新节点，2 秒后回 1003；推给客户端的通知在跨 zone 窗口里丢失不补发的既有限制不变。

---

## 1. 两个问题分开看

| 方向 | 例子 | 谁保管未完成的消息 | 换节点时 |
|---|---|---|---|
| 服务器 → 玩家所在的 scene | 交易成交入账、帮会扣款、邮件附件、战斗结算、GM 封禁 | 发送方的持久待办（§2） | 发送方重投，或新节点落地后主动拉 |
| 客户端 → 玩家所在的 scene | 换装备、强化、加点、领任务奖励、领活跃度奖励 | **本次之前没有人保管**，被拒后靠客户端重发 | 本文：gate 的会话场景 lane 保管并重投 |

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
| A 已销毁实体，gate 的路由还没切到 B | A 回空应答（session id not found），gate 回 1003 | 【码】`SceneHandler::ProcessClientPlayerMessage`、`scene_client_call_reply.h` `Decide` |
| gate 已切到 B，B 还在入场登记或载入存档 | B 回空应答（session id not found / player not loaded），gate 回 1003 | 同上 |
| 路由已到 gate，进场还没转发给 scene（`pendingSceneEntry`），且会话此前没有 scene 节点 | gate 自己回 1003 | 【码】原 `HandleTcpNodeMessage` 的「Scene not ready」 |

四段都是「确定未执行」。本次处理前三段；第四段不变，仍由 gate 回 1003（那时玩家还没进过任何场景，客户端在等进游戏的结果，不会发玩法请求）。问题有三个：
- 要客户端自己重发。服务器发起的换节点客户端事先不知道，没有加载界面挡着，玩家正在点的操作直接失败。
- 冻结期间的拒绝码不统一，`kInvalidParameter` 让客户端分不清「稍后再试」和「参数错」。
- 连续换节点（A → B → C）时，每一段窗口都再来一遍。

---

## 4. 设计

### 4.1 决策

| # | 决策 | 理由 |
|---|---|---|
| S1 | 暂存点放在 gate 的会话上（`SessionInfo::sceneLane`） | 只有 gate 看得到客户端的收包顺序；会话不随场景节点变，连续换节点时队列不用搬；会话 erase 即取消 |
| S2 | 只重投 scene 明确答复 `RETRY_LATER` 的请求。RPC0 失败（超时、断连、对端拒绝）一律不重投；gate 自己不决定暂存谁 | 结果未知的失败重投可能让请求执行两次；确定未执行的重投不需要去重。gate 不认识业务方法，判断不了一条请求换节点执行对不对，这件事只能由 scene 说 |
| S3 | 发往 scene 的请求-响应消息按会话单飞：一次一条在途，上一条完成后才发下一条 | 重投和并发发送放在一起必然乱序（被拒的旧请求晚于后发的新请求执行）。单飞之后顺序只由队列决定。与路由 lane、Java 版 gate「每会话每后端域一个在途」同一模型。这一条修订了 [client-request-ordering.md](./client-request-ordering.md) O2「TCP 路径不进 lane」：那句话对路由 lane 仍成立，TCP 路径现在有自己的 lane |
| S4 | 单向消息（内层响应为 Empty，移动等）：lane 空时直接发出，不建排队项；有在途请求时排在后面，保持收包顺序；队头在等重投时直接丢 | 移动是 gate 最热的上行，不能多一次入队出队；改造前同一条 TCP 连接上本来就是严格先后；交接期间的移动对新场景没有意义，晚送达反而是错的 |
| S5 | 从收包起最多等 2000 ms（排队加等重投），超过回带请求 id 的 1003 | 预算链 I6（§5）：12000 + 2000 + 1000 ≤ 15000 |
| S6 | 重投由定时器驱动，间隔不小于 200 ms，实际是下一个 0.25 s 的拍；不依赖路由变更事件 | 交接未成、原地解冻时 gate 看不到任何事件，只有定时重投能发现；路由刚切过去时新节点通常还没载入完，立刻重投多半白发 |
| S7 | scene 在入口统一判「归属交接在途」：挂着 `PlayerTravelHandoffComp` 的玩家，白名单内的请求不派发，答复 `RETRY_LATER` | 交接期间本节点的状态已落盘且不得再写，写请求派发了也只会被拒；只读请求答了也是马上作废的数据 |
| S11 | 只有「换节点执行含义不变」的请求才答复 `RETRY_LATER`，按服务列白名单，默认不带 | 换图、瞬移、放技能的含义绑在当前场景上，带到新场景执行是错的（重复的换图变成第二次换图，瞬移用的是旧场景的坐标）。默认不带：新加的服务不登记就保持改造前的行为，漏登记只是少一点无感，不会出错。写在 scene 的代码里而不是 proto 选项里：这些服务的 proto 是与 Java 版共享的客户端契约，加自定义选项要两版同批改 |
| S8 | 每会话排队加在途上限 64，请求-响应超限回 1008，单向消息超限丢弃 | 进场后客户端会连发一批面板查询；单飞之后它们要排队，上限取路由 lane（32）的两倍 |
| S9 | 在途调用过了「RPC0 超时 + 1000 ms」仍没有完成回调时本地判丢，回 1003 | RPC0 承诺恰好回调一次，这只是纵深防御：回调万一丢了，这条会话的 scene 请求不能永远卡住 |
| S10 | gate 排空迁移遇到场景 lane 不空的会话，本拍不迁 | 免得客户端到新 gate 上发的请求抢在旧 gate 上还没执行的请求前面 |

### 4.2 数据结构

全部只在 gate 唯一的 EventLoop 线程上读写，不加锁，不新增线程。

| 状态 | 位置 | 说明 |
|---|---|---|
| `SceneLane<SceneLaneItem> sceneLane` | `SessionInfo` 新成员 | 队列、在途序号、在途那一条本身（重投要用）、判丢截止、重投时刻 |
| `SceneLaneItem` | `cpp/libs/services/gate/session/comp/scene_lane_item.h` | 客户端原包（shared_ptr）、收包时的 trace、收包时刻、请求 id、消息号、是否单向、被放回队头的次数 |
| 等重投登记表 `session_id → 会话代次` | `client_scene_lane.cpp`，thread_local | 定时器只扫这张表，不扫全部会话；会话没了或代次变了就摘掉 |
| 调用序号 | 同上 | 进程内单调，每次发出（含重投）换新号；完成回调按它认领 |

纯逻辑（排队、单飞、放回队头、判丢）在只依赖 STL 的 `scene_lane.h`，时刻由调用方传入；装配层三个入口 `Admit` / `OnSceneCallDone` / `Tick` 也都由调用方传入当前时刻，单测不读真实时钟。

生命周期（AGENTS §11.7）：排队项与回调都不持有 `TcpConnectionPtr` 或裸 `this`；每次下发先按 session_id 找会话、核对会话代次，再 `conn.lock()`。

### 4.3 scene 侧：什么时候答复「稍后重投」

`ProcessClientPlayerMessageResponse` 新增字段 `dispatch_verdict = 3`，枚举 `ClientMessageDispatchVerdict`。这是 gate 与 scene 之间的内部契约，不是客户端可见契约。`RETRY_LATER` 的含义：请求确定未执行，而且等一等就能受理；应答不带 `message_content`。

**哪些请求可以答复它**（`player/player_handoff_gate.h` 的 `scene_handoff_gate::MayBeRetriedLater`）：应答不是 Empty 的请求-响应方法，且所在服务在白名单里。

| 服务 | 可以换节点再执行 | 原因 |
|---|---|---|
| `SceneBagClientPlayer`、`SceneAttributeClientPlayer`、`ScenePetClientPlayer`、`SceneMissionClientPlayer`、`SceneCurrencyClientPlayer`、`SceneActivityClientPlayer`、`SceneBattleResultClientPlayer` | 是 | 只读写玩家自己的持久状态，不引用当前场景里的东西 |
| `SceneSceneClientPlayer`（换图、跨 zone 传送、取摘要） | 否 | 交接中再来一次换图是重复请求，handler 有自己的在途闸（`kEnterSceneChangingScene`）；留到落地之后执行会变成第二次换图 |
| `SceneMovementClientPlayer`（瞬移、移动） | 否 | 坐标是旧场景的 |
| `SceneSkillClientPlayer`（放技能、学技能、升技能） | 否 | 放技能的目标与位置是旧场景的；同一服务里的学、升技能因此也不带，照旧由冻结闸当场拒绝 |
| `SceneMysteryMerchantClientPlayer` | 否 | 与场景内的导航点有关，没有逐一核对，先不放进来 |
| `ScenePlayer`（ExitGame 等） | 否 | 退出优先，不排队 |

白名单按生成的服务描述符比较，不用服务名字符串：服务改名时这里编译不过，不会悄悄失效。往白名单里的服务加方法之前，先确认它不引用当前场景；引用的话放到别的服务里。

`SceneHandler::ProcessClientPlayerMessage` 在三处答复它，都先过上面的判定：

| 情形 | 之前 | 现在（白名单内的请求） |
|---|---|---|
| 会话不在本节点的 SessionMap 里 | 空应答 | 空应答 + `RETRY_LATER`。玩家刚被放行去了别的节点而 gate 的路由还没切过来，或入场登记还没走完，都是过渡态 |
| 玩家实体还没载入 | 空应答 | 空应答 + `RETRY_LATER` |
| 归属交接在途（`PlayerTravelHandoffComp`） | 照常派发 | 不派发，空应答 + `RETRY_LATER` |

白名单外的请求与单向消息在这三处的行为与改造前完全相同。

第三处的判定是 `scene_handoff_gate::IsHeldForOwnershipHandoff`，排在退出闸之后、GM 闸与热关停之前。只挂 `PlayerFrozenComp`、没有 `PlayerTravelHandoffComp` 的冻结（账号角色交易封存）不算：那不是换节点，等不到目标节点；照旧派发，由各系统的冻结闸当场拒绝。

其余早退（消息号 / 服务 / 方法查不到、反向身份栅栏、旧会话、请求解析失败、字段校验）不填这个字段：等也没用，gate 照旧回 1003。退出中（`UnregisterPlayer`）照旧回 `kFeatureUnavailable`。

各业务系统自己的冻结闸全部保留，作纵深防御：单向消息、节点内部触发的写都不经过入口这道判定。

### 4.4 gate 侧：准入与发出

挂载点在 `DispatchClientRpcMessage` 的 `PROTOCOL_TCP` 分支，前面的会话 / 验签 / 白名单 / 体积 / 限流 / GM / 热关停 / LeaveGame 屏障都不变。

```
Admit(消息, oneWay, now):
    在途那条过了判丢截止 → 判丢,回 1003(S9)
    oneWay 且 lane 空 → 直接发出,返回(S4 快路径)
    入队:满 → 请求回 1008,单向丢;队头在等重投且是单向 → 丢
    Pump

Pump(now):  没有在途、且队头不在等重投时循环
    取队头
    从收包起已超过 2000 ms → 请求回 1003,单向丢;继续
    发送函数 SendClientMessageToScene:
        kSent        → 记在途,结束循环(等完成)
        kSentOneWay  → 继续
        kNoScene     → 回 1003;继续
```

`kNoScene`：会话没有可用的 scene 通道（没进游戏、路由已清、节点还没连上）。确定没发出，当场回 1003，与改造前的「Scene not ready」相同。gate 在这里不暂存：它不知道这条请求换节点执行对不对（S2）。

换场景节点时，路由事件到了 gate 但进场还没转发给新节点（`pendingSceneEntry`）的那一段，会话仍指向上一个节点（[cross-zone-scene-travel.md](./cross-zone-scene-travel.md) CPP-2 的不变量 I5）。消息照旧发往旧节点，由旧节点答复要不要稍后重投。

### 4.5 完成处置

RPC0 的完成回调只捕获会话号、会话代次、调用序号，转给 `gate_scene_lane::OnSceneCallDone`：

```
找会话并核对代次;找不到 → 丢弃
按调用序号认领在途那一条;认领不到(本地已判丢)→ 丢弃
按 scene_client_call_reply.h 的 Decide:
    有 message_content            → 原样下发
    没有内容但 RETRY_LATER         → 没超过等待上限就放回队头,排着的单向消息清掉;超过则回 1003
    RPC0 失败                      → 回 1003,不重投
    没有内容也没有 RETRY_LATER     → 请求号非 0 回 1003,为 0 不下发(与改造前相同)
Pump
```

每个请求恰好一个回执：在途那一条只能被认领一次；判丢之后迟到的回调因序号对不上被丢弃。

定时器 `gate_scene_lane::Tick` 与路由 lane 的过期扫描挂在同一个 0.25 s 定时器上【码】`cpp/nodes/gate/main.cpp`，只对等重投登记表里的会话调 `Pump`；没有会话在等时每拍只做一次 `empty()`。

### 4.6 与其它机制的关系

- **路由 lane**：两条 lane 互相独立。发往 Go 服务的请求与发往 scene 的请求之间仍然没有先后保证，与改造前相同；唯一的跨 lane 约束仍是 LeaveGame 屏障（LeaveGame 还排在路由 lane 里时，TCP 消息在收包时回 1003，不进场景 lane）。
- **客户端 LeaveGame**：发出时清掉会话的 scene 路由。之后场景 lane 里还排着的请求轮到时拿到 `kNoScene`，回 1003。
- **断线、顶号**：lane 随 `SessionInfo` 一起销毁；在途调用的回调找不到会话就丢弃；登记表里的残项由下一拍摘掉。不需要在断线路径上加清理调用。
- **重定向（跨 zone 传送、gate 排空迁移推 124）**：推 124 之后新到的 TCP 消息照旧在收包时回 1003。已经在场景 lane 里的请求不清：玩家还在原场景节点时照常执行、回包照常下发（旧连接还活着）；玩家正被跨 zone 交接走时等不到，2 秒后回 1003。
- **gate 排空迁移**：`drain_migration.cpp` `Classify` 在场景 lane 不空时返回 `kSkip`（S10）。
- **客户端请求幂等键**：两者互补。幂等键管「执行了但回包丢了，客户端重发」；场景 lane 管「确定没执行，gate 替客户端重投」。场景 lane 的重投不经过幂等账本，因为被重投的请求从未执行。
- **热关停、GM 闸**：由最终执行请求的那个节点判。

### 4.7 客户端可见的变化

消息格式、消息号、错误码都没有新增。

| 情形 | 之前 | 现在 |
|---|---|---|
| 同 zone 换场景节点的窗口里发白名单内的请求（背包、装备、属性、宝宝、任务、货币、活跃度、战斗结果） | 冻结期间各系统的拒绝码，其余窗口 1003 | 2 秒内交接出结果：照常成功，只是晚一点；超过 2 秒：1003 |
| 进游戏后存档载入期间发白名单内的请求 | 1003 | 同上 |
| 上面两个窗口里发白名单外的请求（换图、跨 zone 传送、瞬移、技能、神秘商人、取摘要） | 冻结期间由 handler 当场处置，其余窗口 1003 | 不变 |
| 同一会话连发多条 scene 请求 | 并行在途 | 一次一条在途，回包按请求顺序到达 |
| 会话的 scene 请求积压超过 64 条 | 无此限制 | 1008（kRateLimitExceeded），不计非法包 |
| 交接窗口里的移动等单向消息 | 发给 scene 后被冻结闸或「实体不在」丢弃 | gate 直接丢弃，不发 |

### 4.8 观测

gate 每 30 秒打一行 `[scene_lane] summary`，字段见 `client_scene_lane.h` 的 `SceneLaneCounters`。不按会话、玩家、消息号分维度（AGENTS §9）。

- `held`：至少被放回队头一次的请求数；`retry_verdicts`：「稍后重投」的次数。两者之比是平均每条请求重投几次。
- `expired_on_hold`：等了 2 秒仍没被受理。持续增长说明交接耗时经常超过 2 秒，先看 scene 的 `[TravelHandoff]` 汇总里的 `frozen_ms_max`。
- `expired_in_queue`：排在别的请求后面超过 2 秒，说明 scene 回包慢。
- `inflight_lost`、`late_dropped`：正常为 0。不为 0 说明 RPC0 的「完成回调恰好一次」出了问题。
- `holding_sessions`：此刻队头在等重投的会话数。

scene 一侧，交接期间留给 gate 的请求每 1024 条打一行 INFO（`request left to the gate for retry during ownership handoff`）。

---

## 5. 预算链

```
I6  kGateToSceneClientCallTimeout(12000) + kMaxSceneLaneWait(2000) + 1000 ≤ 客户端请求超时(15000)
```

一条请求在 gate 最多先等 2000 ms，发出后最多再等 12000 ms 的 RPC0 超时。两者相加必须早于客户端自己超时，否则客户端先放弃、gate 之后才把请求交给 scene 执行。`client_session_handler.cpp` 里有 `static_assert` 守着，改任何一个值都要重算。

I5（[client-request-ordering.md](./client-request-ordering.md) §5.2）不变：`kGateToSceneClientCallTimeout` 没有动。

2000 ms 是按预算链反推出来的上限，不是按交接耗时测出来的。同 zone 跨节点交接由一次存盘、一次 scene_manager 调用、一次 Kafka 路由事件和一次读档组成【推】，是否通常在 2 秒内完成没有实测数据；上线前用 `[scene_lane]` 的 `held` 与 `expired_on_hold` 之比校准。要加长等待只能缩短 `kGateToSceneClientCallTimeout` 或加长客户端超时，两者都牵动 I5。

---

## 6. 时序示例：连续换两次节点（A → B → C）

客户端依次发了 r1（换装备）、r2（领任务奖励），服务器同时在把玩家从 A 改派到 B，落到 B 之后队长又把他带去 C。

```
t=0     gate 收 r1,发给 A。gate 收 r2,排在 r1 后面(r1 在途)
t=5     A 正在交接冻结:不派发 r1,答复 RETRY_LATER
        gate 把 r1 放回队头;r2 仍排在后面,不发
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
| 新版 scene + 旧版 gate（不认识字段） | 空应答按原规则回 1003。交接冻结期间的请求由「各系统的拒绝码」变成「1003」 |
| gate 重启或崩溃 | 暂存的请求随进程消失；客户端断线重连后自己重发。这些请求都没执行过 |
| 客户端等不及先断线 | lane 随会话销毁，暂存的请求不再发出 |
| 完成回调丢失（不应发生） | 下一条消息进来时判丢，回 1003；之后迟到的回调被丢弃 |
| 重投期间 session_id 被新连接复用 | 完成回调与定时重投都按会话代次核对，不会落到新会话上 |

---

## 8. 没做与残余

- **白名单外的请求不带**（§4.3）。换图、跨 zone 传送、瞬移、放技能是有意不带的；学技能、升技能、神秘商人、取版本摘要是因为所在服务里有绑场景的方法，或没有逐一核对，先保持改造前的行为。要带它们，把方法挪到白名单内的服务，或把白名单细化到方法。
- **单向消息不保**。移动、`SceneInfoC2S` 这类应答为 Empty 的消息在交接窗口里直接丢。它们是场景时效消息，对新场景没有意义。
- **跨 zone 传送**。要换 gate，旧 gate 上暂存的请求等不到目标节点，2 秒后回 1003，客户端到新 gate 上重发。要做到无感需要把暂存的请求交给新 gate，这次不做。
- **推给客户端的通知**在跨 zone 窗口里丢失、不补发的限制不变（[cross-zone-scene-travel.md](./cross-zone-scene-travel.md) §7）。
- **退出中（`UnregisterPlayer`）的实体**照旧回 `kFeatureUnavailable`，不留给 gate。服务器发起的改派走的是归属交接（`PlayerTravelHandoffComp`），已被覆盖；真正的退出没有「目标节点」。
- **scene 节点崩溃**。接管要等节点死亡屏障（约 20 秒），远超 2 秒，请求回 1003。
- **两条 lane 之间没有先后保证**（§4.6），与改造前相同。
- **单向消息会排在在途请求后面**（S4）。今天 scene 的玩家 handler 都是同步回包，在途只有毫秒级，移动不受影响。以后若有 handler 等下游完成才回包（[player-handler-deferred-reply.md](./player-handler-deferred-reply.md)），这条会话的移动会被它挡住，最长挡到那次调用回包；引入这类 handler 时要一并决定：要么让单向消息不排在在途请求后面（放弃这一小段的严格先后），要么让那类请求不占场景 lane 的在途。
- **性能没有实测**。请求-响应消息从并行在途改成单飞，同一会话连发 N 条的总耗时从一个往返变成 N 个往返之和，单个往返是毫秒级【推】。按 AGENTS §6，没有压测对比表之前不宣称任何性能结论。
- **scene 入口没有单测宿主**。`ProcessClientPlayerMessage` 里三处答复 `RETRY_LATER` 的接线只能靠冒烟验证；判定函数本身有单测（§9）。
- **scene 的「session id not found」是 ERROR 级日志**。重投会让这行日志在交接窗口里多打几次（每条暂存请求最多 8 次），本次没有改级别。

---

## 9. 给 Codex 的验证命令

工作目录都是仓库根目录。任一步失败即停，保留输出。

1. **生成协议**（`proto/scene/scene.proto` 加了枚举与字段，不生成的话下面全部编不过）：
   `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`
   通过标准：`cpp/generated/proto/scene/scene.pb.h` 里有 `CLIENT_MESSAGE_DISPATCH_VERDICT_RETRY_LATER` 与 `set_dispatch_verdict`。
2. **C++ 整解，串行**：`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`
   通过标准：0 个 `error C` / `LNK`。改了 gate 的服务库（`session_info_comp.h`）、gate 节点、scene 节点；`run_cpp_tests.ps1 -Build` 不重编库，不能代替这一步。
3. **gate 侧单测**：`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter routing_identity_test -LogDir build/verification/scene-lane-20261112`
   通过标准：全部通过，其中新增 `SceneLane.*` 11 条、`ClientSceneLane.*` 16 条、`SceneClientCallReplyTest.*` 新增 4 条、`DrainMigrationLaneBusy.InGameSessionWithBusySceneLaneIsSkipped`；原有的 `RouterLane.*`、`ClientRequestLane.*` 不回归。
4. **scene 侧单测**：`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone_test -LogDir build/verification/scene-lane-20261112`
   通过标准：全部通过，其中新增 `PlayerHandoffGate.*` 6 条。
5. **冒烟**（本机，两个 scene 节点）：`pwsh -File tools/scripts/dev_tools.ps1 -Command cpp-node-start -GateCount 1 -SceneCount 2`，用 robot 让一个角色在两个节点的场景之间来回换图，换图请求发出后立刻连发几条背包（`GetBag`）/ 货币（`GetCurrencyList`）查询。
   通过标准：这些查询都拿到正常回包（不是 1003）；gate 日志的 `[scene_lane] summary` 里 `held` 大于 0、`expired_on_hold` 为 0、`inflight_lost` 为 0；scene 日志有 `request left to the gate for retry during ownership handoff`。换图途中再发一次换图请求，仍然当场得到 `kEnterSceneChangingScene`，落地后不会多换一次图。robot 目前没有专门的用例，需要临时脚本或手工操作；做不了就如实记「冒烟未跑」。

失败时保留：msbuild 的首个错误及其上下文；`build/verification/scene-lane-20261112` 下对应工程的 stdout。

---

## 10. Java 版（AGENTS §12）

**待做**，本轮不写 Java 代码。Java 版 gate 已经是「每会话每后端域一个在途」，缺的是：scene 域答复「稍后重投」的约定与「换节点执行含义不变」的服务白名单、放回队头按间隔重投、2 秒等待上限、单向消息在等重投期间丢弃。客户端契约两版都不变，不需要同批。
