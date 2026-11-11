# 断线重连恢复审计（2026-10-20）

> **证据范围**：当前工作区静态代码审计；起始 HEAD 为 `6ecf275fa`，包含工作区已有改动，不等于该提交或已部署版本的行为。本文未启动服务，未运行构建、测试、断网实验或压测，不能作为运行验收。
> **目标**：回答玩家断线约 10 秒后，哪些当前状态重新读取、哪些消息真正补投，以及剩余恢复缺口；区分客户端提示丢失、客户端缓存陈旧和业务权威数据丢失。
> **取证原则**：以实际生产者、存储、重连挂点、消费代码为准；设计文档只作契约和历史背景。源码链接附定位行号；行号漂移时以函数名为准。
> **跨仓范围**：共享客户端只读基线 `4418d2c`（`../mmorpg-client`）；Java 版有限核对基线 `425f1da`（`../xuanming-server-mmo`）。三仓均保留既有工作区改动。配置结论只针对跟踪默认值，不代表本机容器或正式部署的实际开关。
> **并行变化**：收尾时 Java HEAD 已到 `da363d8f`；相对 `425f1da` 的本轮相关 `game-*/src/main/**` 与 `PARITY.md` 无提交差异。新增 build 证据没有被当作本轮运行结果。

## 1. 总结与边界

本项目采用多种恢复机制，不能把“重连成功”等同于“期间所有 S2C 原包重放”。跨服务的好友、组队、帮会主要恢复**当前视图**；资产待办与持久收件箱恢复**未完成的业务指令**；聊天按已知频道/会话读取有限历史；邮件从 SQL 重新读取。两类恢复不是同一条队列，也没有全业务共用的客户端消息确认/游标。

最应先处理的是：**新会话复用同场景实体时跳过入场基线**；其次是实时场景持续状态没有完整恢复基线、重登后的战斗终局展示缺口。Java 另有待结算记录仍保留七天的差异。下文区分确定代码路径、按需读取边界和待确认的产品规则，不把这些都归为资产丢失。

已核实的业务恢复矩阵如下。场景、连接、战斗及客户端完整结论见后续章节。

| 内容 | 权威/恢复来源 | 重连后的服务端行为 | 是否补发断线期间原通知 | 边界 |
|---|---|---|---|---|
| 好友列表、申请、黑名单 | friend 查询 RPC；核心 Redis 摘要域 1–3 | 客户端取摘要后按域拉取；服务端不主动推摘要 | 否；好友事件至多一次 | 好友列表/黑名单的客户端消费者与按需拉取须结合第 3 节看，不能把服务端有摘要当作 UI 已自动刷新 |
| 队伍视图、收到的邀请 | match 的 `GetMyTeam` / `ListMyInvites`；摘要域 4–5 | 已提交变化给受影响玩家 bump，落地由客户端拉取 | 否；重新得到当前名册/仍有效邀请 | 过期邀请不补播；提交后 bump 不是同一原子事务 |
| 帮会、本人申请、经济、活动 | guild 查询 RPC；摘要域 6–9 | 管理/申请/本人经济/活动变化给对应玩家 bump | 否；`GuildEvent` 等只是即时提醒 | 共有数值并非每次变化都全帮 bump，打开面板回源仍是契约的一部分 |
| 帮会/交易资产 PENDING | 各业务 SQL outbox + scene 资产账本 | 进场发布 `assetop:wake`，订阅方对该玩家未决行重新投递；普通后台重投兜底 | 补的是资产指令，不是旧 S2C | 有开关、冻结、背包容量、退避等条件；trade 跟踪配置中资产通道仍关闭 |
| 邮件与领取结局 | `mail_message` / `mail_state` / `mail_asset_op` | 查询恢复列表/未读/可领取数/领取状态；已领取待办后台继续 | 邮件改变提醒不补发 | 客户端已有重连首拉；领取资产未订阅落地 wake，可能等普通重投 |
| 聊天 | ChatRedis 的世界/私聊 LIST | 只有客户端显式 `PullChatHistory`，按频道/已知 peer 拉最新 N 条 | 无自动补发，当前 SendChat 本身也不做实时广播 | 最近 200 条、TTL 7 天；单次最多 50 条，无历史翻页/游标/最近私聊列表 |
| 通用玩家持久收件箱 | `player_inbox` SQL PENDING + 玩家持久账本 | 开启时新落地 Fetch，顺序执行、持久后 Ack；提醒补发和在线周期拉取兜底 | 补的是登记的业务指令 | 默认消费关闭、入箱白名单为空；当前仅 GM 处置执行器，不能承载任意游戏 S2C |

## 2. 连接、会话与进场链

### 2.1 十秒短断线并不是旧连接续用

> 2026-11-05 已改(未编译、未测试,待 Codex 验证):意外断线时 gate 改发 `ExitGame(reason = DISCONNECTED)`,scene 把角色停在原地保留 25 秒,窗口内同一角色的新会话直接接回这个实体并补完整入场基线,超时才按原路径存盘退出;主动登出、租约到期关假死连接仍立即退出。下面描述的是改之前的行为。见 [断线保留](player-connection-lost-hold.md)。Java 版待做。

Gate 的 `HandleConnectionDisconnection` 立即代发 Scene `ExitGame`，清 Gate 会话及请求 lane，并通知 Login。Scene 收到退出后停止运动、离开场景/AOI，存盘收敛后销毁实体。旧 TCP 会话的输出队列不会转接给新 TCP 会话。[Gate 退出](../../cpp/nodes/gate/handler/rpc/client_session_handler.cpp#L473)、[断线入口](../../cpp/nodes/gate/handler/rpc/client_session_handler.cpp#L508)、[Scene 退出](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp#L3147)

这里审计的是旧连接已经断开、建立新会话的路径。若只是网络暂时不通而原 TCP 连接仍存活，可能发生同连接的传输层重传；它不等于本节的应用层重连，也不能证明有业务消息恢复。

Login 给 locator 的 `DISCONNECTING` 状态申请默认 30 秒租约。重连判据是尚有该状态且账号一致，随后用新 gate/session、登录代次及 CAS 恢复 `ONLINE`、取消清理 claim。**30 秒保留的是恢复资格，不是 Scene 实体驻留承诺，也不是严格到秒的拒绝截止**：monitor 默认每秒检查，重连接口不直接检查 deadline；挂机权益还可续租。[租约与决策](../../go/login/internal/logic/pkg/sessionmanager/session_manager.go#L120)、[重连裁决](../../go/login/internal/logic/pkg/sessionmanager/session_manager.go#L192)、[Locator CAS](../../go/player_locator/internal/logic/reconnectlogic.go#L71)、[租约检查](../../go/player_locator/internal/logic/leasemonitor.go#L29)

正常完成断线登记且采用默认租约时，十秒本身不足以让租约到期，通常仍是 `ShortReconnect`；旧记录额外失效/被清理等情况才可能走新登录。两者都会重新绑定/路由：Scene 有可复用实体就就地入场，否则从 Redis 异步加载玩家数据，不能假定仅换一下 socket。[复用与重载分支](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp#L2631)

### 2.2 重连落点不保证原场景、原频道

> 2026-11-05 已改(未编译、未测试,待 Codex 验证):没有任何去向的进场请求(只有 login 的 ShortReconnect / ReplaceLogin 这样发)在 location 记的场景还在时回到原场景,原场景已回收或解析不出来才回落默认大世界,并计 `enter_scene_rejected_total{reason="resume_scene_fallback"}`。下面描述的是改之前的行为。见 [断线保留](player-connection-lost-hold.md) §6。

Login 重连/顶号请求给 SceneManager 的目标为 `ZoneId=0, SceneId=0`；SceneManager 从已有 location 恢复 zone，但没有据此恢复原 SceneId，随后选默认大世界地图和轻载频道。非默认地图、副本或多频道玩家可能改派，也可能遇到跨节点交接门禁。[Login 重连请求](../../go/login/internal/logic/clientplayerlogin/entergamelogic.go#L727)、[顶号请求](../../go/login/internal/logic/clientplayerlogin/entergamelogic.go#L1005)、[恢复 zone](../../go/scene_manager/internal/logic/enterscenelogic.go#L386)、[默认落点](../../go/scene_manager/internal/logic/enterscenelogic.go#L1415)

这是代码事实；是否要求“短断线回原副本/原频道”需确定产品契约，本文不把默认重分配直接判成数据一致性缺陷。

### 2.3 普通推送没有跨会话历史回放

Scene RPC 推送和 Kafka 玩家推送最终都按当前 session 查连接；session 不存在或连接已释放即丢弃，没有保存给下一会话的通用队列。技能使用/中断等表现通知同样不是可追溯事件流。[RPC 出口](../../cpp/nodes/gate/handler/rpc/gate_service_handler.cpp#L62)、[Kafka 出口](../../cpp/nodes/gate/handler/event/gate_event_handler.cpp#L333)、[技能即时通知](../../cpp/libs/services/scene/combat/skill/system/skill.cpp#L609)

恢复靠后续章节的查询、快照、业务 outbox 或持久指令执行；Kafka 消费成功、Gate 写 socket 成功都不代表客户端已应用。`2/5/10/30/60 Frames` 是属性发送频率档位，不是断线期间的历史帧日志。

## 3. 客户端落地恢复与状态同步

### 3.1 当前大厅需要重新进入，战斗直连有自动恢复

`GameClient.DisconnectInternal` 清 `InGame`、角色/场景、actor 世界、人物属性缓存，关闭 battle 直连，并使旧 RPC 在途调用以 `Disconnected` 收场。选服界面重新显示断线原因，未发现大厅普通断线后自动重新执行登录入场的调度；当前用户可再次进入角色。跨 gate 重定向有专门的自动管线，不能据此说普通网络断线也会自动重登。[断线清理](../../../mmorpg-client/Assets/Scripts/Game/GameClient.cs#L2079)、[选服界面处理](../../../mmorpg-client/Assets/Scripts/UI/Ugui/QdaoServerSelectView.cs#L495)、[换连接处理](../../../mmorpg-client/Assets/Scripts/Game/GameClient.cs#L1510)

`EnterGameAndWaitScene` 等服务端 `NotifyEnterScene` 才完成入场。通知处理清旧世界，再由自身/邻居的创建通知重建 actor；不能用 EnterGame 的“已受理”应答代替恢复完成。[入场等待](../../../mmorpg-client/Assets/Scripts/Game/GameClient.cs#L815)、[场景及 actor 通知](../../../mmorpg-client/Assets/Scripts/Game/GameClient.cs#L1744)

### 3.2 状态恢复矩阵

| 状态 | 当前真正触发方式 | 恢复范围 / 限制 | 主要证据 |
|---|---|---|---|
| 场景、自身角色、附近实体 | 正常入场推 scene_info 和自身 ActorCreate；下一次 AOI 更新发当前可见邻居 | 重建当前可见集合，不补十秒移动轨迹；同场景复用缺口见 §9.1 | [入场](../../cpp/libs/services/scene/player/system/player_scene.cpp#L265)、[AOI](../../cpp/libs/services/scene/spatial/system/aoi.cpp#L40) |
| actor 外观、位置 | ActorCreate | 实际填 net id、location、类型、GUID、外观/职业/性别；不是含运动、血蓝、Buff、CD 的完整战斗快照 | [实际填充](../../cpp/libs/services/scene/spatial/system/view.cpp#L90)、[契约](../../proto/scene/player_scene.proto#L91) |
| 背包、装备、任务、活动 | 清旧缓存后，打开相应页调用 GetBag/GetMissionList/GetActivityList | 按需全量读取；不能说重连时一包自动同步所有玩家数据 | [清缓存](../../../mmorpg-client/Assets/Scripts/Game/PlayerFeatures/PlayerFeaturesClient.cs#L807)、[UI 请求接线](../../../mmorpg-client/Assets/Scripts/UI/Ugui/Gameplay/GameplayUiRoot.cs#L79) |
| 人物属性、技能书 | 打开面板后 RequestPanel/ListSkills；属性另消费面板变化推送 | 断线作废旧快照，成功拉取后整份覆盖；技能书不含实时技能剩余 CD | [属性](../../../mmorpg-client/Assets/Scripts/Game/Attribute/AttributeClient.cs#L145)、[技能书](../../../mmorpg-client/Assets/Scripts/Game/Skills/SkillBookClient.cs#L88) |
| 宠物列表 | InGame 且 HasList=false 时主动 RequestList，失败下次至少间隔 15 秒 | 是“直到首次成功”的检查，不是每 15 秒刷新已存在列表 | [Tick](../../../mmorpg-client/Assets/Scripts/Game/GameClient.cs#L368) |
| 队伍、邀请 | TeamClient.Reset 排队 GetMyTeam、ListMyInvites；UI 常驻驱动 | 不依赖摘要是否改变，每次会话重建都会读取；面板打开期间另有周期刷新 | [Reset/DrainQueued](../../../mmorpg-client/Assets/Scripts/Game/Team/TeamClient.cs#L200)、[宿主](../../../mmorpg-client/Assets/Scripts/UI/Ugui/Team/TeamUiRoot.cs#L111) |
| 好友申请 | 摘要 FriendRequest 变更后后台 QueueRefresh；打开申请窗也可拉 | 好友列表/黑名单无自动摘要消费者；申请内存清空但版本缓存保留时，不变域也不会后台重建，须按需读取 | [绑定范围](../../../mmorpg-client/Assets/Scripts/Game/Sync/LandingSyncClient.cs#L571)、[清申请缓存](../../../mmorpg-client/Assets/Scripts/Game/Social/FriendClient.cs#L79) |
| 帮会、本人申请 | 摘要变更后后台排队，隐藏窗口也 DrainSyncQueued | 经济/活动只在对应页读取并确认；不变域的空本地快照仍由打开面板取得 | [绑定](../../../mmorpg-client/Assets/Scripts/Game/Sync/LandingSyncClient.cs#L590)、[宿主驱动](../../../mmorpg-client/Assets/Scripts/UI/Ugui/Guild/GuildUiRoot.cs#L101) |
| 邮件 | 每次玩家或连接变化后立即 State.Refresh→ListMails，即使窗口关闭 | 覆盖列表、未读及领取状态；在线通知不构成可靠消息日志 | [落地首拉](../../../mmorpg-client/Assets/Scripts/UI/Ugui/Mail/MailUiRoot.cs#L58)、[实际分页读取](../../../mmorpg-client/Assets/Scripts/Game/Mail/MailClient.cs#L74) |
| 世界聊天 | 打开聊天或手动刷新时 PullChatHistory(limit=50)，发送成功后也刷新 | 只支持世界频道；无即时聊天接收注册、周期追拉、私聊界面恢复 | [SocialClient](../../../mmorpg-client/Assets/Scripts/Game/Social/SocialClient.cs#L33) |
| 匹配队列 | 本地持久匹配意图 + 重新就绪后 GetQueueStatus；排队期间每 3 秒查询 | 恢复自己的排队/开局事实；不是重播丢失的所有匹配推送 | [意图装配](../../../mmorpg-client/Assets/Scripts/Game/GameClient.cs#L305)、[Tick](../../../mmorpg-client/Assets/Scripts/Game/Battle/BattleClient.cs#L283)、[意图恢复](../../../mmorpg-client/Assets/Scripts/Game/Battle/BattleClient.cs#L646) |
| 商城/交易订单 | 商业界面按当前玩家/连接重新查询订单、未决请求 | 有订单查询及幂等恢复面；没有加入统一落地摘要，本文不声称已审遍所有商业界面 | [客户端](../../../mmorpg-client/Assets/Scripts/Game/Jubaozhai/JubaozhaiClient.cs)、[订单查询](../../go/trade/internal/logic/commerce_logic.go#L185) |

### 3.3 摘要客户端的实际保证

`LandingSyncClient` 观测 `(玩家, gate连接身份, InGame/ready)`，新落地单向发 GetSyncDigest；15 秒未收到推送即先将所有已知域标脏，再按 15/30/60 秒退避重取。代次改变、摘要不可用等情形按全量标脏；拉取真正发出时取票，成功应用后才确认域版本；换会话作废旧票。**全量标脏只通知已注册消费者，并不会替无消费者的域调用 RPC，也不包含背包、邮件等其他域。** [超时常量](../../../mmorpg-client/Assets/Scripts/Game/Sync/LandingSyncClient.cs#L68)、[落地与超时](../../../mmorpg-client/Assets/Scripts/Game/Sync/LandingSyncClient.cs#L256)、[失败退化](../../../mmorpg-client/Assets/Scripts/Game/Sync/LandingSyncClient.cs#L354)、[取票与确认](../../../mmorpg-client/Assets/Scripts/Game/Sync/LandingSyncClient.cs#L221)

本地持久化的是域版本，不是好友/帮会完整数据。会话重置后部分业务内存快照为空而摘要相同，不会触发后台拉取；当前界面打开后强拉可恢复。若要把“后台已同步全部域”作为就绪标准，需要同时验证本地内容有效性，不能只比较版本。

## 4. 跨服务状态如何补齐

### 4.1 版本摘要是失效提示，不是事件日志

`playersync` 在核心 Redis 写 `player:{id}:sync_digest`，字段 `g` 为摘要代次，其余字段为各域计数；每次 bump 续期 30 天。业务提交成功后 bump；摘要不存变更内容，也不保存客户端确认位置。[摘要契约与 Lua](../../go/shared/playersync/digest.go#L42)、[BumpAll](../../go/shared/playersync/digest.go#L166)

scene 的 `PlayerLandingSystem::SendSyncDigest` 异步 `HGETALL` 后推 `NotifySyncDigest`；Redis 未就绪、命令未发出、空/错误/畸形应答会返回 `UNAVAILABLE`，供客户端走全量恢复。`OnEnteredScene` **只做资产唤醒及收件箱落地钩子，不主动下发摘要**，摘要依赖客户端 `GetSyncDigest`。[SendSyncDigest](../../cpp/libs/services/scene/player/system/player_landing.cpp#L147)、[OnEnteredScene](../../cpp/libs/services/scene/player/system/player_landing.cpp#L206)

已实现九个域：好友列表、好友申请、黑名单、队伍、队伍邀请、帮会、本人帮会申请、帮会经济、帮会活动；没有聊天、邮件、交易域。[协议事实源](../../proto/scene/player_scene.proto)

### 4.2 好友：恢复列表，不补好友通知

写操作在提交后 bump：新增申请给接收方，接受给双方的列表/申请，移除给实际受影响双方，拉黑/解黑按实际改动 bump 列表/申请/黑名单。查询入口分别是 `GetFriendList`、`GetPendingRequests`、`ListBlocks`。[bump 出口与调用](../../go/friend/internal/logic/friend_logic.go#L262)、[好友列表](../../go/friend/internal/logic/friend_logic.go#L481)、[申请列表](../../go/friend/internal/logic/friend_logic.go#L531)、[黑名单](../../go/friend/internal/logic/friend_logic.go#L655)

`pushFriendEvent` 只向 `ONLINE` 发 Kafka→gate 提醒，`DISCONNECTING` 直接按离线跳过；没有重试、回执或离线补推。新增申请和接受申请会推，拒绝/移除/拉黑/解黑刻意不推。断线期间收到申请，正确恢复结果是列表里出现仍有效申请；不会重播原 `FriendEventS2C`。[实际推送语义](../../go/friend/internal/logic/push.go#L1)、[在线状态过滤](../../go/friend/internal/logic/push.go#L114)

### 4.3 队伍：当前名册、邀请与排队态分别恢复

`publish` 是提交后的摘要/推送汇合点，先 bump 再做即时推送及 scene 刷新信号；bump 包含 RPC 操作者，避免操作者的应答在掉线时丢失后没有失效提示。队伍名册给新旧成员全集 bump，邀请增删给被邀请人 bump，纯时间过期不 bump。即时推送不在线就不发，没有离线通知回放。[publish](../../go/match/internal/team/notify.go#L65)、[受影响集合与 bump](../../go/match/internal/team/syncdigest.go#L23)

恢复接口是 `GetMyTeam` 与 `ListMyInvites`，返回当前有效视图。申请被拒等瞬时提示可以丢失，但重拉的名册/邀请不会因此保留旧关系。排队还有独立的 `GetQueueStatus`，不能仅用队伍恢复代替排队状态恢复。[GetMyTeam](../../go/match/internal/team/service.go#L169)、[ListMyInvites](../../go/match/internal/team/service.go#L307)、[GetQueueStatus](../../go/match/internal/logic/getqueuestatuslogic.go#L38)

### 4.4 帮会：覆盖域较全，共有数值仍依赖面板回源

管理操作（建帮、成员变化、公告、职位等）、申请及审批、经济预留/终结、活动变化均有对应 bump。`OnAssetFinalized` 是资产终结之后 bump 的公共出口；不是只有同步成功路径才更新摘要。[管理与申请调用点](../../go/guild/internal/logic/guild_manage_logic.go#L551)、[建帮](../../go/guild/internal/logic/guild_logic.go#L316)、[资产终结](../../go/guild/internal/logic/economy_logic.go#L693)、[活动](../../go/guild/internal/logic/activity_logic.go#L493)

恢复 RPC 为 `GetPlayerGuild`、`ListMyGuildApplications`、`GetGuildDonateOptions`、`GetGuildShop`、`GetGuildActivities`。摘要扇出异步执行，有整批 2 秒预算，失败记录指标/日志而不回滚业务。[帮会摘要装配与预算](../../go/guild/internal/logic/push.go#L323)、[经济查询](../../go/guild/internal/logic/economy_logic.go#L1236)、[活动查询](../../go/guild/internal/logic/activity_logic.go#L758)

契约明确接受“共有帮会资金/活动进度不为每笔变化专门向全帮 bump”，本人及已有扇出会顺带更新；其余成员打开面板重新查询。因此摘要不是“任何可见字段变化都保证使所有受影响缓存立即失效”的完整协议。[既有契约 §3.3](landing-recovery-sync-digest.md#33-域表客户端可见契约)

## 5. 真正会重投的业务

### 5.1 帮会和交易资产待办

scene `OnEnteredScene` 发布核心 Redis `assetop:wake <player_id>`。guild/trade 订阅后按服务/玩家去重，调用 `Loop.WakePlayer`，忽略原 `next_attempt_ms` 列出该玩家最多 32 条 PENDING，再用与后台循环相同的 Claim→ProcessOne 路径投递。唤醒丢失、订阅断线或并发满时不重试该提示，SQL 待办仍然保留。[scene 发布](../../cpp/libs/services/scene/player/system/player_landing.cpp#L230)、[订阅与降级](../../go/shared/assetopwake/wake.go#L1)、[WakePlayer](../../go/shared/assetop/reconcile.go#L722)

scene 按 `(player, stream, stream_epoch, seq)` 查账本，已处理序号返回原结局；资产变化与账本同玩家记录存盘。调用方遇到 `RETRY`、`NOT_HERE`、传输错误保留 PENDING；仅拿到持久终局才 Finalize。`RequireSQLCommit` 开启时还须核对 SQL 提交证据。[资产处理持久判定](../../cpp/libs/services/scene/player/system/asset_op_system.cpp#L225)、[决策](../../go/shared/assetop/decide.go#L49)、[ProcessOne](../../go/shared/assetop/reconcile.go#L811)

这里补的是“尚未结清的资产操作”。例如玩家断线期间帮会奖励尚在 outbox，重连后会再尝试应用；不会重放此前已经结束的每次金币变化通知。交易有 `GetCommerceOrder` / `ListCommerceOrders` / `ListMarketOrders` 等查询接口，但没有加入九域摘要，客户端是否自动拉单另看第 3 节。[交易客户端协议](../../proto/trade/jubaozhai.proto#L155)、[订单回源](../../go/trade/internal/logic/commerce_logic.go#L185)

**配置与时延边界**：当前跟踪配置 `guild.AssetOp.Enabled=true`，`trade.AssetOp.Enabled=false`；不能把已落码的 trade 恢复链描述成默认启用。guild/trade 都配置落地 wake、离线 5 次后约 30 分钟停放、180 天自动放弃，后者有离线与未应用证据条件，是已有产品处置政策。普通最大退避 60 秒是中值，另有 ±20% 抖动，不能承诺“最多 60 秒”。[guild 配置](../../go/guild/etc/guild.yaml#L145)、[trade 配置](../../go/trade/etc/trade.yaml#L156)、[退避与停放算法](../../go/shared/assetop/decide.go#L119)

### 5.2 邮件：列表恢复，领取待办继续完成

当前 `go/mail` 已落码。`Repo.Send` 写 SQL，`Repo.List` 在同一 RR 快照中返回邮件列表及未读/可领取数，包含当前领取状态。客户端重连首拉可恢复断线期间新邮件，不需要重投 `MailChangedEvent`。[Send](../../go/mail/internal/data/repo.go#L127)、[List](../../go/mail/internal/data/inbox.go#L54)

邮件提醒只在个人邮件首次提交后尝试一次；不在线直接跳过，失败也不改变已提交邮件。系统/帮会邮件没有逐玩家提醒，靠列表读取发现。[提交后提醒](../../go/mail/internal/server/server.go#L175)、[提醒在线过滤与单次预算](../../go/mail/internal/notify/notify.go#L42)

附件领取在 SQL 事务中把 `mail_state` 改为 PENDING 并创建 `mail_asset_op`；请求应答丢失后重读邮件仍可查到同一领取状态。后台以 SYSTEM_CREDIT 流投 scene，终结要求 `SQLCommitted`。未决资产不因邮件自然过期而直接丢弃。[Claim](../../go/mail/internal/data/claim.go#L17)、[Finalize](../../go/mail/internal/data/asset_store.go#L114)、[后台装配](../../go/mail/mail.go#L173)

当前 mail 装配没有订阅 `assetop:wake`，`OfflineAfterAttempts=0`，因此离线期间领取待办继续按普通退避；重连不额外加速这条流，可能在下一轮到期才到账。服务端 2 秒循环、60 秒最大退避中值及 ±20% 抖动也不是到账硬 SLA。[mail 循环](../../go/mail/mail.go#L173)、[默认循环参数](../../go/shared/assetop/reconcile.go#L347)

### 5.3 通用玩家收件箱：可靠指令能力已实现，默认未开启

`Store.Enqueue` 在 SQL 事务中分配 seq、插入 PENDING、推进 open_count、登记 `notify_due_ms`，提交后发 Kafka scene 提醒。提醒失败有服务端补发；离线/等待落点会结束提醒重试，交给落地 Fetch。Kafka 写入成功仍不是执行确认。[Enqueue](../../go/player_inbox/internal/inbox/store_enqueue.go#L46)、[通知落定语义](../../go/player_inbox/internal/inbox/notify.go#L21)、[ResendNotify](../../go/player_inbox/internal/inbox/store_maintenance.go#L112)

启用消费后，`OnPlayerLanded` 按实体首次/owner_epoch 变化判断新落地，无条件安排 Fetch；同 owner_epoch 的重复进场不重新创建一轮。Fetch 按 seq 升序取 PENDING 并围栏；scene 执行、更新玩家持久账本，确认实际落盘之后 Ack；应答丢失重拉由账本返回原结局。另有 Kafka 提醒、默认 30 分钟 ±20% 在线周期检查，以及战斗解冻立即检查。[OnPlayerLanded](../../cpp/libs/services/scene/player/system/player_inbox_system.cpp#L1088)、[Fetch](../../go/player_inbox/internal/inbox/store_consume.go#L113)、[持久后 TryAck](../../cpp/libs/services/scene/player/system/player_inbox_system.cpp#L593)、[Tick](../../cpp/libs/services/scene/player/system/player_inbox_system.cpp#L1314)

**当前并非全业务可靠消息箱**：执行器表只有封锁货币收益、解封、附加欠款三种 GM 类型，欠款入箱仍禁止。`bin/etc/game_config.yaml` 的 `consumer_enabled=false`，`go/player_inbox/etc/player_inbox.yaml` 的 `EnqueueTypes=[]`，所以默认配置不会消费或接受这些新指令。这是显式关闭，不应报成重连代码漏调。[执行器表](../../cpp/libs/services/scene/player/system/player_inbox_registry.cpp#L21)、[消费开关](../../bin/etc/game_config.yaml#L34)、[入箱白名单](../../go/player_inbox/etc/player_inbox.yaml)

收件箱确认目前依据 Redis 玩家落盘快照；它不自动获得 mail 的 SQL 提交证据语义。涉及 Redis 与 SQL 灾难恢复时，应使用既有围栏/回档约束，不能据此承诺任意存储回退下恰好一次。[确认判定](../../cpp/libs/services/scene/player/system/player_inbox_system.cpp#L514)、[既有设计风险 §7.10](player-inbox-service.md)

## 6. 聊天：只能恢复有限且已知的会话历史

`SendChat` 通过 Lua 对世界频道或双方共享的私聊 LIST 做 LPUSH→LTRIM→EXPIRE，然后返回成功；该函数没有向其他在线玩家发 S2C 的步骤。当前协议也只有 `SendChat` 和 `PullChatHistory`。因此“重连补聊天”实际依赖客户端拉历史，不能描述为实时推送断线后自动重发。[实际写入与返回](../../go/chat/internal/logic/chat_logic.go#L251)、[协议](../../proto/chat/chat.proto#L47)

历史窗口为每频道/会话最多 200 条，TTL 7 天（每次写入续期）。`PullChatHistory` 默认 20、上限 50，只取最新 N 条；没有 `before`、`since`、message_id 或后续页游标。私聊要求调用者给 `peer_player_id`，没有最近会话目录。TEAM/SYSTEM 频道当前拒绝。[PullChatHistory](../../go/chat/internal/logic/chat_logic.go#L268)、[频道限制](../../go/chat/internal/logic/chat_logic.go#L317)、[配置](../../go/chat/etc/chat.yaml#L72)

可构造的确定边界：玩家断线 10 秒期间，同一个频道产生 60 条消息，即使 Redis 保留 200 条，重连拉一次最大 50 条后仍有 10 条没有可用接口继续向前拉；若陌生玩家首次私聊，接收方没有原 peer 缓存，服务端也没有最近会话查询供发现。两者分别是**历史完整性/发现能力缺口**，不涉及资产丢失；若当前产品只承诺“最近 50 条、已知会话”，则是功能边界而非违反已承诺 SLA。

## 7. 战斗、结算与副本恢复

### 7.1 活动中的回合战斗：当前快照恢复

scene 在换会话、战斗仍为 FIGHTING 时推 `BattleReconnectS2C{battle_id}`（144）；客户端经 match `RequestBattleTicket`（179）补签并重建 battle 直连，就绪后主动 `GetBattleState`（140）。PREPARING 期间不提前补签，转 FIGHTING 时再检查会话变化并补推提示。观众在直连握手应答之后主动收到 `SpectateStateS2C`；参战者握手不自动推，必须调用 140。[scene 提示](../../cpp/libs/services/scene/battle/system/player_battle.cpp#L2281)、[参战/观众首帧](../../cpp/nodes/battle/logic/battle_room_manager.cpp#L2029)、[客户端补拉入口](../../../mmorpg-client/Assets/Scripts/Game/Battle/BattleClient.cs#L999)

补签分两种模式：有 `Authority` 时由 SQL Formation 及 operation/instance 查询房间创建状态；无 Authority 才使用 Redis 观战索引。旧索引路径的“节点缺席却要等索引过期才判房间消失”不能泛化到全球权威模式。[分支](../../go/match/internal/logic/requestbattleticketlogic.go#L80)、[全球补签](../../go/match/internal/logic/global_match.go#L450)

快照覆盖当前回合/胜负、全部单位当前属性和最大血蓝、死亡/逃跑/防御/自动状态、Buff 层数与剩余回合、技能、冷却、阵位/宝宝归属和外观，以及行动截止时间、未提交行动的单位、本人剩余道具。冷却按收信人裁剪，本人道具只给本人。只能恢复“谁还没提交”，不包含本人已选的具体行动内容。[BuildStateSnapshot](../../cpp/libs/services/battle/system/turn_battle_engine.cpp#L1885)、[字段契约](../../proto/battle/battle_data.proto#L94)、[状态回包](../../proto/battle/player_battle.proto#L24)、[视角裁剪](../../cpp/nodes/battle/logic/battle_room_manager.cpp#L1693)

无活直连时 TurnResult 等战斗帧直接丢弃，不回落到 Gate、不排历史重播；重连通过当前快照继续。已结束/删除的房间补拉返回默认 `battle_id=0`，不能用这个接口取历史战报。[推送策略](../../cpp/nodes/battle/battle_push_policy.h#L3)、[无直连出口](../../cpp/nodes/battle/logic/battle_room_manager.cpp#L1666)、[不存在时的补拉](../../cpp/nodes/battle/logic/battle_room_manager.cpp#L904)

### 7.2 奖励待办和终局画面是不同保证

battle 向直连发送 BattleEnd，并经独立待办投递结算；scene 应用后再经大厅推一份 BattleEnd。当前 C++ pending 记录不过期，进场先读 pending；Redis 无应答/错误不当作“无待办”，会登记复查。资产与账本一起存盘，持久后才条件销账，已应用重投不会再加钱。全球确认模式还在发布结果前为全员预写 pending。[登录恢复](../../cpp/libs/services/scene/battle/system/player_battle.cpp#L2303)、[持久与去重](../../cpp/libs/services/scene/battle/system/player_battle.cpp#L2172)、[全员预写](../../cpp/nodes/battle/logic/battle_room_manager.cpp#L1354)

但 `alreadyApplied` 的结算不再推终局；客户端大厅断线还会清 `_battleId` 与 `_endPendingBattleId`，无战斗上下文的 BattleEnd 直接丢弃。因此“奖励正确到账”不保证“重登后恢复胜负与奖励明细面板”。详见 §9.3。[服务端只在首次应用时推](../../cpp/libs/services/scene/battle/system/player_battle.cpp#L2197)、[客户端接收条件](../../../mmorpg-client/Assets/Scripts/Game/Battle/BattleClient.cs#L978)、[断线清上下文](../../../mmorpg-client/Assets/Scripts/Game/Battle/BattleClient.cs#L1083)

副本空间落点仍受 §2.2 的默认重分配影响，不能因回合战斗快照较完整就推断所有 MMO 副本和实时战斗也已完整恢复。

## 8. 双版本与 Java 版

本轮只核对对应接缝，没有运行 Java 构建或全域验收，也没有修改 Java 仓库或对账本。

| 项目 | Java 当前源码事实 | 与 C++/Go 的关系 |
|---|---|---|
| 战斗补签及快照 | 参战者主动 140，观众握手推；scene 重建/沿用冻结后推 144 | 恢复形态一致；同样不是历史帧回放 |
| 终局通知 | 只在 `APPLIED` 时推，`ALREADY_APPLIED` 不推 | 同样存在结果展示恢复边界 |
| 待结算保留 | `SETTLEMENT_TTL_SEC=604800`，实际 STORE_SETTLEMENT 执行 EXPIRE | **仍为七天，与 C++ 不过期存在差异**；Redis 待办过期后自动奖励恢复可能中断，全球模式仍有 SQL 原件，不能说结算来源永久丢失 |
| 全球结算预写 | `attemptResultCommit` 发布确认后才进入 finishBattle/dispatchSettlement | 未采用当前 C++ 发布结果前全员 pending 预写；不能宣称闭环完全对齐 |
| 通用在线推送 | Redis Pub/Sub 至多一次，玩家离线不保留 | 仍需业务主动查询和待办恢复 |
| 按域摘要 | 已同步 253/254 契约，但 main 源无 GetSyncDigest/NotifySyncDigest 实现；未注册的 Empty/ONE_WAY 请求被忽略 | 共享客户端 15 秒超时后才将已有域标脏，且只通知已注册消费者，不是全业务自动全拉 |
| GM 持久 Inbox | SceneNode 已创建 InboxService 并 attach 到 world | PARITY 部分“未创建/未装配”记载滞后；仅能确认源码接线，不能说全业务消息已可靠补发 |

证据：[战斗状态](../../../xuanming-server-mmo/game-battle/src/main/java/com/game/battle/room/BattleRoomServiceImpl.java#L467)、[进场战斗恢复](../../../xuanming-server-mmo/game-scene/src/main/java/com/game/scene/battle/PlayerBattleService.java#L588)、[终局推送](../../../xuanming-server-mmo/game-scene/src/main/java/com/game/scene/battle/PlayerBattleService.java#L1039)、[七天常量](../../../xuanming-server-mmo/game-discovery/src/main/java/com/game/discovery/battle/BattleRedis.java#L82)、[EXPIRE](../../../xuanming-server-mmo/game-discovery/src/main/java/com/game/discovery/battle/BattleRedis.java#L287)、[实际调用](../../../xuanming-server-mmo/game-battle/src/main/java/com/game/battle/outbox/SettlementOutbox.java#L82)、[全球结果发布顺序](../../../xuanming-server-mmo/game-battle/src/main/java/com/game/battle/room/BattleRoomServiceImpl.java#L738)、[至多一次推送](../../../xuanming-server-mmo/game-discovery/src/main/java/com/game/discovery/presence/PlayerPushes.java#L25)、[未注册单向请求](../../../xuanming-server-mmo/game-scene/src/main/java/com/game/scene/world/ClientRequestHandler.java#L247)、[Inbox 装配](../../../xuanming-server-mmo/game-scene/src/main/java/com/game/scene/SceneNode.java#L317)、[原对账本](../../../xuanming-server-mmo/PARITY.md)。

Java 全球模式的 `BattleCompletionStore` 已将完成原件和个人结算来源同 SQL 事务归档；它明确不负责应用资产。所审进场/后台恢复链未见从该归档回源补投的接线，故七天问题应验证“自动恢复能否完成”，而非宣称原件永久丢失。[归档职责](../../../xuanming-server-mmo/game-match/src/main/java/com/game/match/rating/BattleCompletionStore.java#L20)、[个人来源写入](../../../xuanming-server-mmo/game-match/src/main/java/com/game/match/rating/BattleCompletionStore.java#L67)

## 9. 已确认缺口、受限能力与后续验证

### 9.1 优先修复：新会话复用同场景实体，漏掉入场基线

> 2026-11-01 已修(未编译、未测试,待 Codex 验证):`EnterScene` 发现会话变了且实体还在目标场景时,先离场再走完整入场;同一会话的重复路由仍然幂等。Java 版未核对,`PARITY.md` 已记待做。
> 2026-11-06 修正(11-08 Codex 跑整套 `cross_zone_test`,对应用例通过):上面那次修复判「会话变了」比的是会话快照,而入口第 2 步已先把快照会话置无效,同一会话的重复路由实际也会先离场再入场,「仍然幂等」当时并不成立。现改为比实体的登录身份(会话号不同,或号相同而登录代次更新),判据与用例见 [断线保留](player-connection-lost-hold.md) §5 第 3 步。

**条件**：旧实体尚在目标场景，例如旧 Gate→Scene 的 ExitGame 尚未到达/丢失，或在线顶号先于旧会话退出。`HandleEnterGameNode` 复用实体，`EnterScene` 将 snapshot 绑定到新 session；`HandleEnterScene` 却因 `oldScene==scene` 直接返回 true。入场通知与自身创建在这个 return 后面，因而都没有发出。后续 team/battle/landing 钩子也不补场景基线。[复用](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp#L2631)、[新绑定](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp#L2781)、[幂等早退](../../cpp/libs/services/scene/player/system/player_scene.cpp#L231)、[后续钩子](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp#L2878)

**影响**：新客户端已清旧 actor 世界，仍在等待 NotifyEnterScene；可能直至入场预算到期才失败。即使其他入口让等待结束，Hex/AoiList 仍沿用旧连接的观察状态，同格 AOI 不会重建当前邻居，依然缺实体。普通属性脏推送不能代替 actor 创建。[AOI 同格早退](../../cpp/libs/services/scene/spatial/system/aoi.cpp#L62)、[客户端等待](../../../mmorpg-client/Assets/Scripts/Game/GameClient.cs#L815)

**判定与建议**：静态调用链已确认，尚无运行复现。修复时须区分“同一会话的重复路由”和“新会话需要完整基线”：后者重建入场通知、自身与当前 AOI；前者继续幂等，避免重复通知清空已建立的客户端世界。不能简单删去所有幂等判断。

### 9.2 实时场景持续状态缺少基线，CD/Buff 生命周期也未明确

**已确认**：ActorCreate 只有 §3.2 所列内容；`ActorBaseAttributesS2C` 的 transform/velocity/combat flags 由实体全局 dirty mask 驱动，广播前即移除该 mask，没有为新观察者保留确认或生成完整初始状态。目标的沉默等 flags 已向旧观察者发过且之后不变时，新观察者缺当前 flags。共享客户端目前也未找到生产代码对 `SyncBaseAttribute` 及 2/5/10/30/60 属性档位的消费，actor 世界主要消费创建、移动和技能表现通知。[脏位消费](../../cpp/libs/services/scene/generated/attribute/actorbaseattributess2c_attribute_sync.cpp#L24)、[协议内容](../../proto/scene/player_state_attribute_sync.proto#L14)、[客户端生产注册](../../../mmorpg-client/Assets/Scripts/Game/GameClient.cs#L1744)

这是一条实时场景基线/消费链不完整的问题，不要求补播旧技能动画，也不能把回合战斗的完整快照拿来证明该链已覆盖。

另有规则需确定：实时技能 CD 为运行时 `CooldownTimeListComp`，缺表时视为可用；Buff 为运行时 `BuffListComp`。`DestroyPlayer` 销毁实体后，新实体通过两张玩家 DB 表的完整 marshal/unmarshal 恢复，其中不含这两类运行时实例及剩余状态。十秒断线可能触发实体重建，因而不能承诺原 CD/Buff 一定延续。需要明确哪些状态应跨重登保持、哪些应清除，再为应保留部分提供权威恢复；本轮不推定所有 Buff 都必须落库。[CD 读取与写入](../../cpp/libs/services/scene/combat/skill/system/skill.cpp#L175)、[技能列表](../../cpp/nodes/scene/handler/rpc/player/player_skill_handler.cpp#L58)、[实体销毁](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp#L2970)、[加载入口](../../cpp/libs/services/scene/player/system/player_data_loader.h#L28)、[完整加载与写回](../../cpp/libs/services/scene/player/system/player_database_loader.cpp#L118)、[玩家持久契约](../../proto/common/database/mysql_database_table.proto#L101)

### 9.3 战斗结果展示缺少重登恢复

> 2026-11-05 已补(服务端已落码,未编译、未测试,待 Codex 验证;客户端改动交 Codex):结算应用的同一帧把本人实际入账的结果写进存档里的有界列表(最多 10 局、按 battle_id 去重),与奖励同一次存盘提交,确认提交之后才公开;客户端每次落地查 `GetRecentBattleResults`,收到无战斗上下文的 BattleEnd 时再查一次,展示后 `AckBattleResults`。方案比较与协议见 [战斗结果重登恢复](battle-result-recovery.md)。Java 版待做,`PARITY.md` 已记。

**情形 A**：断线期间场景已经应用结算，直连和大厅两份终局通知均未被客户端消费。重连后 pending 已销账、账本命中不会重推，房间也已删除，140/179 无法取得历史终局。

**情形 B**：断线期间战斗结束，重登才补应用 pending。客户端断线时已经清掉 `_battleId` 和 `_endPendingBattleId`；服务端即便此时发 BattleEnd，客户端无上下文会丢弃。该局已结束，冻结恢复不会先为它发送有效 144 来建立上下文。[首次应用才推](../../cpp/libs/services/scene/battle/system/player_battle.cpp#L2197)、[恢复顺序](../../cpp/libs/services/scene/battle/system/player_battle.cpp#L2303)、[清理与接收条件](../../../mmorpg-client/Assets/Scripts/Game/Battle/BattleClient.cs#L978)

这两条都可能让玩家看不到胜负与奖励明细，**不等于奖励重复或丢失**。若产品要求重登后可查看，应有按玩家授权的近期结果查询或持久结果通知，并按 battle_id 去重；不能靠反复应用结算来补弹窗。

### 9.4 摘要是尽力失效提示，有提交后空窗

跨服务权威提交与 `BumpAll` 不原子，bump 失败不持久排队；team 还明确在异步上下文派发。若提交成功→进程退出/预算耗尽，或客户端落地取摘要恰好发生在提交与 bump 之间，同时即时通知未送到，客户端可能看到与上次相同的域版本而跳过查询；此后没有新变化时，只能由面板强制回源恢复。业务已经提交，缺的是缓存刷新保证，不是权威数据丢失。[明确接受的窗口](../../go/match/internal/team/syncdigest.go#L9)、[失败计数但不持久重试](../../go/shared/playersync/digest.go#L166)

既有落地设计 L-1“任何变化下次落地后最终能看到”的文字，比上述代码保证强。修订承诺时应明确：哪些域每次落地强拉、哪些有在线周期回源、哪些只靠摘要与打开面板；若要保证无需开面板也必恢复，需另行实现可验证的兜底。[原不变量](landing-recovery-sync-digest.md#12-目标不变量)

### 9.5 受限能力与文档滞后

§6 的聊天窗口与私聊发现能力、§3 的部分域按需读取、§5 的 Inbox 默认关闭和邮件无落地 wake，分别是历史范围、恢复时机、启用范围和时延边界。它们均不应直接写成资产丢失。§2.2 的落点策略和实时 CD/Buff 清理规则需要产品契约；若要求原处连续恢复，应列入后续实现。

`landing-recovery-sync-digest.md` 的“不在范围内/邮件域追加”仍写 `go/mail` 未落码；当前已有 SQL 邮件及客户端重连首拉。Java PARITY 也有 Inbox 装配状态滞后。本文按实际代码纠正事实，本轮不重写这些历史记录；阅读旧设计时应结合本文。摘要域表仍不含邮件，这是当前实现；邮件走独立强拉恢复。

### 9.6 建议验收场景（尚未执行）

| 场景 | 应观察的结果 | 需要区分 |
|---|---|---|
| 保持旧实体同场景，建立新会话/在线顶号 | 新会话能收入场、自身及当前邻居基线；入场等待结束 | 同会话重复路由仍须幂等，不把已建立世界清空 |
| 目标早已处于沉默/持续移动，新观察者重连进入 | 当前 flags、运动与应展示的持续状态可重建 | 验证完整基线与客户端消费，不靠下一次状态变化 |
| 断线前开一个长 CD/持续 Buff，分别走实体复用和销毁重建 | 按确定的产品规则连续保留或明确清除 | 当前代码不能承诺两条路径等价 |
| 在副本/非默认地图/多频道断十秒 | 记录真实落点和跨节点拒绝行为 | 先明确是否承诺回原处，不能只测默认地图 |
| 中局断十秒后回合战斗补签、140恢复 | 回合、血蓝、Buff、截止、本人道具和提交权限正确 | 不要求补播断线期间所有 TurnResult |
| 战斗断线后结束，分别让结算已应用/重登才应用 | 奖励只应用一次；独立检查结算面板能否恢复 | 当前代码存在结果展示缺口；不能只用金币正确判整链通过 |
| Java 未应用结算模拟超过七天 | 验证 pending 过期后的奖励恢复来源 | 与 C++ 不过期不同，不用等待真实七天，可用受控到期夹具 |
| 断线期间新增好友申请/被踢队伍/被踢帮会 | 落地后当前有效列表与关系一致 | 不要求旧弹窗逐条补播；好友列表/黑名单按需边界另验 |
| 业务提交后、bump 前退出服务 | 面板强拉能恢复；记录是否有无需开窗的兜底 | 摘要相同不等于数据相同 |
| 断线期间个人/系统/帮会邮件到达 | 重连首拉返回邮件和当前未读/可领取数 | 通知丢失与邮件数据丢失分开 |
| 领取邮件已提交但客户端应答丢失 | 重连状态保持 PENDING 或终局，最终只到账一次 | 同一 mail_id 重试不应产生第二条领取 |
| 帮会资产投到一半断线、唤醒订阅暂断 | SQL PENDING 保留；后台或落地 wake 完成，账本不重复应用 | 普通重试可受冻结/容量/退避影响 |
| Inbox 消费/入箱白名单关闭 | 请求明确拒绝，不误报消费成功 | 不为测试临时修改或开启实际部署配置 |
| Inbox 开启的隔离环境中断在应用后/Ack 前 | 下次落地重新拉到 PENDING，回放账本原结局并确认 | Redis 快照确认与 SQL 灾难恢复保证分开 |
| 10 秒内 60 条聊天、陌生人首次私聊 | 验证目前只能取最新 50 条及会话发现限制 | 不把窗口能力说成完整消息恢复 |

已有测试源码覆盖摘要状态机、初次直连补拉/观众首帧、结算幂等与登录复查等；它们不等于完整十秒断线恢复验收。本轮未运行这些测试，也未找到覆盖本表全部路径的现成一键场景。[客户端摘要测试](../../../mmorpg-client/Assets/Tests/EditMode/Net/LandingSyncClientTests.cs)、[直连测试](../../robot/battle_direct_conn_test.go#L194)、[战斗冒烟](../../robot/battle_smoke_scenario.go#L305)、[结算测试](../../cpp/tests/bag_test/player_battle_settlement_test.cpp#L1461)

本轮只落审计文档并检查引用/格式。后续运行按本表明确的隔离输入、故障点与观察结果执行；当前没有这些场景通过/失败的运行结果，没有修改业务逻辑、配置、客户端或 Java 实现。
