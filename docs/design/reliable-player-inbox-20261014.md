# 重要玩家消息：持久收件箱、拉取恢复与迁移围栏（2026-10-14）

> 状态：用户提供的公开资料调研与仓库对照记录，补充本次静态核对；本次只改文档，没有实现功能、编译或运行测试。
> 本仓核对基线：`ab00ad8b8`。行号会变化，代码定位以本文链接和函数名为准。
> 外部产品的精确参数属于调研时的产品与版本条件，不是本项目配置。本次补核的官方来源见末节；其余细节保留为用户调研快照，不代表本次逐条复核。
> 2026-10-15 修订：用户拍板新建独立的玩家收件箱服务 `go/player_inbox`（设计 [player-inbox-service.md](player-inbox-service.md) v1.0，其 D1）。本文 §1 与 §5.2「不再新建第三套通用投递器」据此**收窄修订**：资产、结算、邮件的分工不变；只有「没有任何持久渠道、不是资产、改的是 scene 持有状态」的玩家待办，按域准入统一收件箱。修订处在 §1、§5.2 就地标注，入口见 §5.4。

## 1. 采用的工程原则

**重要消息存进按收件人身份组织的持久收件箱，接收方准备好之后按自己的进度去拉；推送只起提醒作用，丢了也不要紧。**

消息跟随固定的 `player_id`，不跟随 scene、zone、gate 或连接。玩家从 A 经过 B 到 C，期间换多少次节点，待办记录都留在原来的业务存储。C 接管完成后恢复进度；定位与推送只用于降低等待时间，不承担最终恢复责任。

这是本仓库建议采用的模式，不是下列公司唯一共同的实现。Iris 会推送实际更新，Temporal Signal 自身就是持久历史输入；这些案例分别支持身份寻址、持久进度、补齐和围栏等机制。

这里的“收件箱”是语义，不要求新建一个通用消息服务：资产待办表、战斗结算发件箱、邮件业务库都可以承担相应职责。需要明确每条业务链的权威数据、提交点、确认点、保留窗口和超窗处置。

（2026-10-15 收窄修订）上一段对已有持久渠道的业务仍然成立。没有任何持久渠道、不是资产、改的是 scene 持有状态的玩家待办（第一个域是 GM 处置），改由新建的玩家收件箱服务按域准入承接，见 [player-inbox-service.md](player-inbox-service.md)。它不是能投任意消息的通用投递器，也不迁移资产通道、结算发件箱或邮件。

原调研中 BigWorld、EVE、魔兽和国内中心服架构一组资料尚在补跑，本记录没有其结果，不将其列作已核实依据，也不登记自动跟进任务。

## 2. 公开资料中的共同做法

### 2.1 收件箱按人存，不按位置存

- Meta Messenger 的 Iris 按用户组织全序更新队列，每个消费者（手机端、存储层）有自己的进度指针，成功消费后前移；手机离线时指针停住，新消息继续入队。
- Telegram 用消息箱的 `pts` 等状态跟踪增量进度；Temporal 按 Workflow Id 将 signal 写入持久历史，调用方无需追踪 worker 的物理位置。

### 2.2 推送只是提醒，准确数据靠接收方按进度拉

- Telegram 在启动、发现序号断档、长时间未收到更新等情况下调用 `getDifference` 恢复缺失更新，实时推送不能取代同步状态。
- Pusher、Roblox 的相关推送接口不保证送达，重要数据需要业务存储与重连后的恢复路径。
- Riot Messaging Service 的消息承担通知作用，接收后回业务服务取最新状态；原调研记载其断线缓存只保留几分钟，超出后全量同步。
- **不能把恢复条件只写成“收到推送后再拉”**：推送全丢时这个条件永远不会发生。登录、重连、每次跨区落地及打开相关面板都应有独立恢复触发点。

### 2.3 序号或幂等键去重，持久化后再确认

- Telegram 对 `pts` 更新比较“本地进度 + 本批数量”和远端进度，区分连续、重复与缺口；微信 seqsvr 的用户序号允许跳号，用作版本水位，不能把所有业务序号都理解为连续消息号。
- 原文把 Telegram `random_id` 与 Stripe、SQS 一并归为“去重窗口有限”。**本次订正**：Telegram 官方当前说明相关 `random_id` 服务端去重记录不失效；有限窗口不能泛化到它。
- Stripe API v1 的幂等结果可在至少 24 小时后清理；SQS FIFO 的去重间隔为 5 分钟。这类传输或 API 层去重不能替代业务端可持久恢复的幂等记录。
- Cloudflare Durable Objects 的 output gate 会在相关写入确认持久化之前扣住对外消息与回包。对应本项目的要求是：业务结果和去重账本落到承诺的持久层后，才回 durable 确认。

### 2.4 旧节点还能不能写，由存储在提交时判断

- Orleans 默认目录的最终一致性与不稳定期重复 activation、Akka 单写者对正确脑裂处理的依赖，说明“路由看来只有一个持有者”不能单独证明数据安全。
- fencing 的共同点是存储提交时拒绝旧持有者：Kleppmann 的 fencing token、Chubby sequencer、Kafka producer epoch 都体现这个原则，具体令牌格式和验证协议各不相同。
- Kafka 这里指同一 `transactional.id` 的事务生产者在初始化时取得新 producer id / epoch，并隔离旧生产者；不能泛化为任何生产者新实例启动都简单地把 epoch 加一。
- 原调研还记录了流程约束：微信 seqsvr 旧节点 N 秒读不到配置后停止服务，新节点等待 N 秒再接管；Discord 在 2026 年 3 月语音事故后补准入检查，交接未完成不允许缩容。它们作为原调研案例保存，未在本次逐条补核。
- epoch 必须在真正有副作用的入口与存储提交点生效。交接完成前不接管、不下线等流程约束用于保证活性与可恢复性，不能替代存储围栏。

### 2.5 链路有上限，超窗有明确退路

原调研记录的窗口示例：WhatsApp 未送达消息最多保存 30 天、Ably 补发窗口 2 分钟、Discord 重连补发窗口几分钟。Iris 的“约一周”来自 2014 年文章，2018 年文章描述可排队数周，不能当作当前现网固定值。这些窗口不是同一种保证，不能直接用作本项目默认值。

超出增量窗口就回源重建或执行业务补偿：

- [Discord `Invalid Session`](https://github.com/discord/discord-api-docs/blob/main/developers/events/gateway-events.mdx) 的 `d=false` 表示不可恢复，需要重新 `Identify`，`d=true` 则可能 `Resume`；Telegram `differenceTooLong` 后按该消息箱对应的状态与同步接口恢复。
- [FCM Android 离线非折叠消息](https://firebase.google.com/docs/cloud-messaging/customize-messages/collapsible-message-types)超过 100 条会丢弃队列并通知应用全量同步。
- LINE 落后过多走全量同步，定期交换摘要对账。

对于余额、列表等状态，权威快照可以重建；对于未完成的资产操作，不能仅用全量拉状态代替执行、终结或补偿，也不能到期静默删账。

### 2.6 按类型分级

- 瞬时消息不进入可靠业务链：原调研的 Slack“正在输入”不写库。
- 场景内短命事件只在订阅生命周期内有效：Xbox RTA 进入场景订阅、离开退订，迟到更新丢弃；重连通过订阅时返回当前值恢复，不要求补发全部历史。
- 资产、结算、邮件等重要业务各走已有持久渠道；社交列表提醒可以丢，列表本身必须可拉取；技能伤害等场景时效事件按其业务契约处理。

## 3. 迁移窗口里的在途消息

| 做法 | 原调研代表 | 原调研参数与边界 |
|---|---|---|
| 拒收，调用方刷新位置后重试 | Dapr、Service Fabric | Service Fabric 为尽力投递，可能重复 |
| 旧节点有限次数转发 | Orleans | 原调研默认最多转发 2 次，超出退回调用方；属于版本配置 |
| 路由层缓冲 | Akka Cluster Sharding | 默认每个 ShardRegion 缓冲上限 10 万条，满后丢弃；交接期间会丢弃特定缓冲以避免乱序 |

这些是迁移时的路由机制，本身不能作为重要消息的持久恢复保证；框架及可选持久模块的保证应分别核对。Orleans 默认是 at-most-once，不自动提供业务去重；Akka Reliable Delivery 的当前文档仍提示不建议用于生产，不能仅凭模块名把它视为已满足本项目可靠性要求。

“内存缓冲可能丢”不能扩展到所有可选模块：Akka Reliable Delivery 配置持久生产者队列后提供崩溃恢复所需的持久机制，但业务仍需处理确认前故障引起的重复。

## 4. 与上一轮方案的差别

上一轮的固定 `player_id` 寻址、持久待办、幂等去重、归属代际校验、类型分级方向保留，补足三点：

1. **从追着位置推，转为待办不动、接管后按进度恢复。** 查位置、投递、被拒、刷新、重试可以保留为加速路径；推送失败不影响业务记录存在。
2. **补保留期限、积压上限、超窗处置和对账。** 分别说明消息过期、长期离线、重试用尽、增量断档与摘要不一致如何恢复。
3. **围栏之外补交接流程。** 新主准备好才消费，旧主完成交接才退出；存储仍在提交时裁决 epoch，不能用流程检查替代它。

## 5. C++ / Go 混合服现状

以下是原文针对本仓库的对照，并按上述基线做了静态定位；设计里记载的“已落码”与“已验证”分开看。

| 已有渠道 | 当前职责与边界 | 本仓入口 |
|---|---|---|
| 资产通道 | 业务变更和待发行同事务；`(stream, epoch, seq)` 配有界滑动窗口；`NOT_HERE` / `RETRY` 退避；终结要求持久确认 | [Go assetop](../../go/shared/assetop/)、[PlayerAssetOpSystem](../../cpp/libs/services/scene/player/system/asset_op_system.cpp)、[资产通道契约](guild-phase2/04-asset-channel.md) |
| 战斗结算发件箱 | battle 先写共享 Redis 再推；重试前重新查位置；scene 按 `battle_id` 条件销账；进场钩子补应用 | [settlement_outbox.h](../../cpp/libs/services/battle/settlement/settlement_outbox.h)、[player_battle.cpp](../../cpp/libs/services/scene/battle/system/player_battle.cpp) |
| owner_epoch 围栏 | scene 的 owner_epoch、Redis 条件写与 go/db epoch 守卫已有；它们不能等同 MySQL 提交时的行内围栏 | [归属屏障](scene-owner-reentry-barrier.md)、[存盘围栏](guild-phase2/08-save-owner-fence.md)、[go/db 守卫](../../go/db/internal/kafka/key_ordered_consumer.go) |
| 消息分级 | 社交推送一次，回源拉权威列表；交接中的技能伤害按时效契约丢弃 | [好友推送设计](friend-port-20260918.md)、[跨区传送](cross-zone-scene-travel.md) |

**持久确认的实际边界**（2026-11-06 按现行机制改写）：资产通道 `durable` 指账本结局已进入最后一次提交成功的玩家存档，即 go/db `Save` 的 TiDB 事务已提交（玩家存档只存 TiDB，写权由 TiDB 写权行判定）。它不防整库按时间点恢复（PITR）到更早时刻：账本与资产一起回退而 Go 侧可能已终结，处置见 [PITR 手册](../ops/mysql-backup-pitr-runbook.md) §4.1a。

### 5.1 原文六项缺口与本次核对

| # | 缺口 | 本次记录的证据与状态 |
|---|---|---|
| 1 | 推送提醒与落地后拉取只完成一半 | [cross-zone-scene-travel.md §7](cross-zone-scene-travel.md) 明确迁移窗口 S2C 丢失、不补推，要求登录、跨区落地和打开面板主动拉权威状态；文档记载只有 robot friend-smoke 有拉取验证，Unity handler 尚未编写。本次未读取或修改独立客户端，客户端当前落地状态仍待其证据确认。正常两段看门狗合计约 60s，另有 70s 冻结硬上限，不能把 60s 当绝对总上限。 |
| 2 | MySQL 写入缺行内围栏 | [db-write-fencing-mysql.md](db-write-fencing-mysql.md) 为 v0.2 待授权设计，标明未落码、未编译、未测试；go/db 的 Redis epoch 守卫不能阻止已经在途的旧 SQL 无条件覆盖。 |
| 3 | 进场入口缺 epoch 放行校验 | [SceneHandler::EnterScene](../../cpp/nodes/scene/handler/rpc/scene_handler.cpp) 在 `ReconcileRelocateOnReentry` 前的注释仍说明入口没有 owner_epoch 栅栏；陈旧路由应在清会话、改待确认表等副作用前被拒。 |
| 4 | 通用玩家消息 RPC 会静默丢 | 同文件 `SendMessageToPlayer` 找不到 session 或玩家时记日志后返回，没有业务级 `NOT_HERE` 结果；`MESSAGE_PRIORITY_IMPORTANT` 不会自动生成落库、确认、补投机制。重要操作不能凭该标签走通用 RPC。 |
| 5 | 邮件服务尚无落地实现 | 本仓 `go/mail` 不存在；[邮件设计](mail-system.md) 采用业务库作收件箱并携带幂等键。这个状态不适用于独立 Go 主服仓库。 |
| 6 | 资产待办缺长期离线超窗处置与对账闭环 | [资产通道](guild-phase2/04-asset-channel.md) 用未决条数与序号跨度限制新操作；尚不能据此宣称具备长期离线的终结、补偿和对账。不能泛化成“全仓没有期限”：结算发件箱已有 7 天 TTL 与有限重投，仍需定义到期后的业务恢复。 |

> 2026-10-16 补注:第 4 项所说 `scene_handler.cpp` 的 `SendMessageToPlayer`(`Scene.SendMessageToPlayer`,1 号)已于 RpcChannel 批 7 退役并从 proto 删除(1 号留墓碑)
> ([muduo-rpc-channel.md §20.3](muduo-rpc-channel.md));「重要操作不能走通用 RPC」的结论不变。

### 5.2 后续顺序与验收口径

不再新建第三套通用投递器（2026-10-15 收窄修订：不新建能投任意消息的通用投递器；无渠道、非资产、改 scene 持有状态的待办按域进入玩家收件箱服务，见 §5.4）。先补原文第 1 至第 3 项，具体实现另行落码和验证：

1. **落地恢复**：目标 scene 接管和数据加载完成后，客户端按域拉权威状态；连续迁移、断线重连、窗口内全丢推送时最终仍能恢复。明确域版本、重入、分页与失败重试，不能只在推送回调里调用。
2. **MySQL 行内围栏**：沿已有设计完成条件写与迁移；以旧实例暂停后恢复、乱序任务、相同版本重放、缓存回写为验收场景，确认存储最终值不会被旧持有者覆盖。
3. **进场 epoch 门禁**：在清会话、待确认表变更等副作用前校验；核对重复进场、旧路由迟到与交接未完成的入口，保留明确的拒绝与恢复结果。
4. 后续各业务分别补保留期、积压上限、超窗终结/补偿、死信处置、摘要或账本对账；不得把“重试次数用尽”写成“业务已完成”。

两版对齐：独立 Java 服已有不同的存储与进场围栏，本次由其仓库文档独立记录，不按 C++ 的未完成清单判定 Java 状态。独立 Go 主服亦已有邮件与推送重同步，不能套用本节缺口。

### 5.3 第 1、4 项的设计入口（2026-10-14 追加）

- 第 1 项「落地恢复」与第 4 项「超窗处置与对账」的详细设计见 [landing-recovery-sync-digest.md](landing-recovery-sync-digest.md)：
  - 每玩家按域版本摘要（核心 Redis `player:{id}:sync_digest`）；
  - 客户端落地后主动取摘要、只拉变化的域；
  - scene 进场完成时唤醒资产通道；
  - 资产待办的离线停放、日对账与 180 天自动放弃；
  - 战斗结算待办去掉到期删除。
- 它没有新建投递器：摘要只索引现有业务的权威数据，唤醒只加速现有资产通道。
- 第 2 项 MySQL 行内围栏见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md)（v0.3 修订中）。
- 第 3 项进场门禁由 [scene-enter-session-fence.md](scene-enter-session-fence.md) 以登录代次实现。该文论证了单靠 owner_epoch 修不了同节点顶号，因此不再按 owner_epoch 拒绝进场。

### 5.4 玩家收件箱服务入口（2026-10-15 追加）

- 用户于 2026-10-15 拍板新建独立进程 `go/player_inbox`（独占库 `mmorpg_player_inbox`），设计见 [player-inbox-service.md](player-inbox-service.md) v1.0。
- 第一个域是 GM 处置（封锁 / 解封货币获取、挂补缴欠款）：它没有任何现成的可靠路径（`recall_logic.go` fail-closed、scene 侧 `TODO(P1-B)` 空桩）。
- 每个 (玩家, 域) 是一个有序邮箱：scene 落地后主动拉取，游标与效果同一次落盘后才确认，围栏是流行上的 owner_epoch 高水位。v1 不接版本摘要。
- 本文 §2.6「按类型分级」在收件箱里对应入箱分级：v1 只收持久类，瞬时类与「需要刷新」不入箱（后者归版本摘要）。
- 状态：设计已定稿，分批落码；第 1 批开始前向用户报文件清单。

## 6. 来源与记录边界

主体来源为用户在 2026-10-14 提供的完整调研结论；原文中的 `claude.ai/epitaxy/...` 代码地址在此改为仓库相对链接，避免把聊天跳转地址当代码事实源。未提供原始出处的数值与事故细节仍按“用户调研快照”记录。

本次补核的官方链接与订正说明在此集中登记，后续引用参数时应再次核对对应版本：

- [Telegram：更新同步、pts 与 random_id](https://core.telegram.org/api/updates)：支持按进度补齐，并订正 `random_id` 有限窗口说法。
- [Akka：Reliable Delivery](https://doc.akka.io/libraries/akka-core/current/typed/reliable-delivery.html)：当前生产使用提示与可靠投递边界。
- [Meta：Messenger 基础设施（2014）](https://engineering.fb.com/2014/10/09/production-engineering/building-mobile-first-infrastructure-for-messenger/)、[Messenger 存储迁移（2018）](https://engineering.fb.com/2018/06/26/core-infra/migrating-messenger-storage-to-optimize-performance/)：用户队列与独立进度；保留时间只作历史参数。
- [Temporal：Workflow 消息](https://docs.temporal.io/develop/go/workflows/message-passing)：Workflow Id 寻址与 Signal 写入历史。
- [Orleans：消息投递保证](https://learn.microsoft.com/en-us/dotnet/orleans/implementation/messaging-delivery-guarantees)、[默认转发次数源码](https://github.com/dotnet/orleans/blob/main/src/Orleans.Runtime/Configuration/Options/SiloMessagingOptions.cs)：默认投递语义、去重边界与 `MaxForwardCount = 2`。
- [Akka：Cluster Sharding](https://doc.akka.io/libraries/akka-core/current/typed/cluster-sharding.html)、[ShardRegion 源码](https://github.com/akka/akka/blob/main/akka-cluster-sharding/src/main/scala/akka/cluster/sharding/ShardRegion.scala)：每个 ShardRegion 的缓冲范围与 `HandOff` 分支。
- [Cloudflare：Durable Objects 术语](https://developers.cloudflare.com/durable-objects/reference/glossary/)：output gate。
- [Kafka：KafkaProducer API](https://kafka.apache.org/43/javadoc/org/apache/kafka/clients/producer/KafkaProducer.html)：事务生产者初始化与旧 epoch 隔离。
- [Stripe：幂等请求](https://docs.stripe.com/api/idempotent_requests)、[SQS FIFO：去重术语](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/FIFO-key-terms.html)：API v1 至少 24 小时与 FIFO 5 分钟的适用范围。

本文交付仅代表设计记录、来源边界及代码静态定位完成，不代表上述缺口已经修复或运行验证通过。
