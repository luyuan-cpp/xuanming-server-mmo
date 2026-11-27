# 《五行奇谈》局外人观战交流模式与个人禁交开关设计（V1）

> 日期：2026-11-27  
> 状态：**策划与工程方案初稿（待负责人拍板默认档位与多人裁决细节后进入实施）**  
> 上位红线：[项目设定](project-vision.md) · [全局玩法统一收口总账](gameplay-unified-design-v1.md) · [位图存储与删表 ID 规范](../rules/bit-storage-and-table-ids.md)  
> 依赖前置：[回合制战斗服务器](../design/turn-based-battle-server.md) · [观战分级扩容（L0/L1/L2）](../design/battle-spectate-tiered-fanout.md) · [群仙问道赛 V2（观战预设喝彩）](season-team-tournament-v2.md) · [改名与内容安全](../design/rename-and-content-safety.md)  
> 美术需求：**零新美术阻塞**（首期 100% 复用既有战斗 HUD、观战底栏与社交聊天控件，暂不向 Codex 下发新切图任务）  
> 适用仓库：`xuanming-server-mmo-cpp`（C++ `scene` / `battle` + Go `match` / `chat` 权威源）、`xuanming-server-mmo`（Java 独立版对账）、`xuanming-server-mmo-python`（Python 增量移植对账）、`mmorpg-client`（Unity 客户端逻辑与 UGUI）

---

## 1. 需求分析

### 1.1 意图与核心边界（先钉死最关键的一句话）
经典回合制 MMORPG（问道 / 梦幻式）中，亲友、帮众或围观道友在观战打本、擂台切磋、帮战或联赛时，**能够直接与战斗中的玩家互动交流、起哄吹牛**，是极强的社区热闹感与社交黏性来源。但同时，竞技玩家又天然担心两件事：一是**对手双开看不同阵营偷信息或场外指挥作弊**，二是**不想在认真打局时被局外人刷屏干扰**。

因此，本功能的核心设计边界非常明确——**把「能不能观战」与「观战期间能不能交流」彻底拆成两把独立的锁**：
- **不是禁止观战**：哪怕玩家关闭了本开关，只要该模式本身允许公开观战（`spectate_visibility == PUBLIC`），局外人依然可以正常进场观看整场战斗；
- **而是禁止观战期间的局外人交流**：每名玩家可在个人设置中自主开关「允许局外人观战交流」。**若某场战斗开局冻结为「关闭」，则整场战斗中任何局外人皆不可在观战期间发言交流；若冻结为「开启」，则该场战斗开放全房公开观战交流，参战者与观战者可自由互动。**

### 1.2 目标、非目标与反目标
- **目标（Goals）**：
  1. **玩家个人开关 → 开局冻结为战斗级规则**：每名玩家持久化维护个人偏好（开启 / 关闭局外人观战交流）；进战斗建房时由权威服务端按参战者设置一次性裁决并冻结为该场战斗的 `spectate_chat_enabled`，**整场战斗内不可中途翻盘**。
  2. **彻底防「一台电脑双开看不同方」偷信息**：坚守「唯一中立脱敏观众视角」与「队内私密频道物理隔离」，使双开多个账号分别“观战不同方”在信息论层面多拿不到 1 bit 额外情报。
  3. **对称化解「场外战术指挥」争议**：
     - 不接受场外交流的玩家关掉开关，即可通过 PVP 一票否决确保自己打的每一局 100% 禁止局外人交流；
     - 双方均允许开启的对局，交流室**全场唯一且双方公开可见**——观众给一方支招时对手同步可见，转化为公开心理博弈，叠加回合制技能命中/暴击/乱敏随机性，避免单边暗箱指挥。
  4. **天然覆盖 L0 / L1 / L2 全档观战与参战者**：交流通道独立承载于 `go/chat`（不污染 `battle` 计算进程），与既有分级观战体系解耦，并全量接入 `content_guard` 内容安全审核与举报撤回链路。
- **非目标（Non-Goals）**：
  - 不改变现有 `spectate_visibility`（哪类玩法能被观战仍完全由 `spectate_visibility.go` 决定）。
  - 不做「分阵营私密观众频道」（绝不允许观众加入仅对 A 方或 B 方可见的战术频道）。
  - 不做语音聊天、观战发红包/付费打赏弹幕、喝彩加战斗 Buff 或观战发言领奖励。
  - 不为单场战斗观战聊天永久落盘 TiDB 历史表（战斗结束自动过期回收，仅违规举报命中时按既有审计表留证）。
  - 不依赖“同 IP / 同设备禁止双开观战”作为核心反作弊前提（双手机、4G/Wi-Fi 混用即可轻易绕过，治标不治本）。
- **反目标（Anti-Goals）**：
  - 误把“关闭观战交流”做成“禁止别人观战”。
  - 玩家关闭开关后，仍有改包客户端绕过前端隐藏、向该战斗强行发送局外人聊天。
  - 观众进入观战后能看到任一方的技能冷却、剩余道具、备选灵宠、变阵禁护招盲选或队内战术标点。
  - 热门 L2 赛事局数千观众同时刷屏把 `battle` 或 `gate` 主循环打卡，或未经敏感词审核直接放出 UGC 文本触碰应用商店/小游戏拒审红线。

### 1.3 上位红线准入五问自检（[project-vision.md](project-vision.md) §2）
1. **普通零氪玩家（反「上班」一票否决）**：纯社交开关与观战互动体验，零每日打卡、零发言任务奖励、零战力或胜率付费特权，想热闹就开、想清静就关。**通过**。
2. **个人开发者收入**：纯体验与社交留存增益，零可交易产出、零打赏切支付，不向工作室泄露任何经济套利口子。**通过**。
3. **物价与长期循环**：零货币、零材料、零数值通胀。**通过**。
4. **防骗与资产安全**：不涉及交易与资产；全量接入 `content_guard` 拦截广告引流、代练加群、站外转账诈骗文本，并支持一键举报与黑名单联动。**通过**。
5. **商店与平台过审 / 防下架安全**：自由文本强制走既有 `go/chat` 审核、实名防沉迷拦截、举报与异步撤回链路；同时预留服务端总控开关 `SpectateChat.FreeTextEnabled`（首审期可按需一键关闭，客户端入口随服务端状态彻底隐藏，杜绝空按钮）。**通过**。

---

## 2. 方案对比与选型记录

| 方案 | 核心做法 | 优势 | 劣势 / 取舍 | 选型结论 |
|---|---|---|---|---|
| **方案 A（选定）** | **个人开关 → 开局冻结为「战斗级开关」**（不开 = 该场战斗禁止任何局外人交流；开 = 全房公开交流），同时保留局内本地折叠静音按钮 | 完全符合“想打纯净局就不让场外人插嘴、想热闹就全场一起聊”的公平对称预期；规则简单清晰，观战者进场一眼可知本局能否发言 | PVP 多人局需明确「全员同意 / 队长同意」裁决口径与默认值，否则随机 10 人局开启率偏低 | **负责人拍板采用** |
| 方案 B | 仅做个人本地屏蔽（每个人只控制自己看不看观战发言，不限制局外人发） | 多人局零裁决冲突，任何时候观众都能聊 | 竞技玩家无法阻止对手在局内接受局外人实时支招，不符合“关了就谁都别在场外说”的公平预期 | 弃用（仅吸收“开启局内的本地一键折叠”作防遮挡辅助） |
| 方案 C | 全模式一刀切永久禁止观战自由交流，只留 6 条预设喝彩 | UGC 风险与场外指挥零增量 | 彻底丧失回合制 MMO 围观好友打本/切磋唠嗑的核心社交乐趣，体验过冷 | 弃用（保留为服务端紧急降级/首审开关兜底态） |

---

## 3. 设计分析

### 3.1 两把正交锁：观战可见性 vs 观战交流权
在全链路中严格区分以下两个独立状态，**互不覆盖、互不混淆**：

| 维度 | 控制对象 | 权威决定点 | 关闭时的真实表现 |
|---|---|---|---|
| **锁 1：`spectate_visibility`**（既有） | **能不能进场观战** | `match` 建房时 `spectateVisibilityForRankedBattle(...)` 冻结进 `CreateBattleRequest.spectate_visibility` | 不可进场观战、不入观战列表 |
| **锁 2：`spectate_chat_enabled`**（本次新增） | **观战期间局外人能不能交流** | `match` 建房时按参战者个人开关 + 模式规则裁决，冻结进 `CreateBattleRequest.spectate_chat_enabled` | **照常可观战**；但本局禁止任何局外人在观战室内发言交流，服务端硬拒该场发言请求，观战端隐藏输入框并提示「本局已关闭观战交流」 |

> **与既有《群仙问道赛》V2 预设喝彩的关系**：
> - 当 `spectate_chat_enabled == false` 时，**本局禁止局外人自由交流与向参战者传递文本信息**；
> - 若处于《群仙问道赛》直播观战，既有 6 条固定枚举预设喝彩（`SendTournamentSpectateCheer`，仅观众间人气条与飘字、参战者不可见）可按配置独立保留为「无交流纯应援气氛」，也可由 `TournamentRule` 统一随 `spectate_chat_enabled` 控制（**推荐前者**：零自由文本、参战者不可见，不破坏观赛氛围，同时补齐服务端禁止参战者拉取喝彩的权限校验）。

### 3.2 玩家个人设置定义与存储
- **玩家可见交互**：在设置面板及战前/切磋面板提供清晰的开关项：**「允许局外人观战交流（开 / 关）」**，下方附一行小字说明：「关闭后，你参与的战斗仍可正常被观战，但观战期间禁止任何局外人发言交流」。
- **协议与存档枚举（防 proto3 零值默认陷阱）**：
  - 按 `AGENTS.md §8` 命名第一原则，使用明确三态枚举而非歧义布尔值，便于未来运营调整全局默认值而无需刷老玩家存量库：
    ```protobuf
    enum eSpectateChatPreference {
      SPECTATE_CHAT_PREF_UNSPECIFIED = 0; // 未手动设置：沿用服务端全局默认策略
      SPECTATE_CHAT_PREF_ALLOW       = 1; // 显式开启：允许局外人观战交流
      SPECTATE_CHAT_PREF_FORBID      = 2; // 显式关闭：禁止局外人观战交流（仍允许正常观战）
    }
    ```
  - **持久化归属**：追加至 `PlayerProfileComp`（或 `PlayerUint64Comp`）末尾单一枚举字段 `eSpectateChatPreference spectate_chat_pref = 15;`（单枚举仅占 1 字节 varint，天然向后兼容；注：因不是对应配置表稳定 ID 集合的批处理布尔表，不强套 `cfg_bit_index` 表映射，完全符合 `bit-storage-and-table-ids.md §1` 适用边界）。
  - **服务端解析默认口径（建议产品拍板点 ①）**：
    - `SPECTATE_CHAT_PREF_UNSPECIFIED` 时，**推荐默认解析为「允许（ALLOW）」**（原因：若新号默认关闭，3V3/5V5 匹配局中采用“全员允许才开”几乎永远凑不齐人，功能会形同虚设；默认开启 + 战斗内与设置页一键关闭并永久记住偏好，既保证自然热度又尊重不愿被打扰的玩家）。
    - 若产品倾向极端克制，也可通过配置项 `SpectateChat.DefaultAllowWhenUnset`（`bool`）随时切为默认关闭，零改代码。

### 3.3 多人战斗开局冻结裁决矩阵（`spectateChatEnabledForBattle`）
进战斗时，由 `scene` 在 `PrepareBattle` 阶段把每位真人参战者解析后的布尔值 `bool allow_spectate_chat` 写入 `BattlePlayerSnapshot`；`match` 在 `gather` 建房阶段调用唯一权威纯函数 `spectateChatEnabledForBattle(...)` 裁决并**随 `CreateBattleRequest` 永久冻结**（AI 托管单位不参与投票，恒视作中立忽略）：

| 玩法模式 | 先决条件 | 裁决规则（开局冻结，整场不变） | 理由 |
|---|---|---|---|
| **任意私密不可观战局**（`spectate_visibility != PUBLIC`） | 无观众 | 恒为 `false` | 场外无人可观战，直接关通道 |
| **单人 PVE**（`MATCH_MODE_PVE_SOLO`、单人打本/野外明雷/试炼） | `PUBLIC` | 按**本人** `allow_spectate_chat` | 100% 尊重本人意愿 |
| **组队 PVE**（`MATCH_MODE_PVE_TEAM`） | `PUBLIC` | 按**队长** `allow_spectate_chat`（无显式队长时取首名真人） | 怪物对手无 PVP 作弊风险，避免 5 人队因 1 名路人未开导致整队不能与亲友唠嗑 |
| **1V1 切磋 / 天墉生死状**（`MATCH_MODE_PVP_CHALLENGE` / `MATCH_MODE_JIANGHU_DUEL`） | `PUBLIC` | **双方均允许（A && B）** | 任一方关闭即锁死全场禁止局外交流，绝对公平 |
| **正式排位 PVP（1V1 / 3V3 / 5V5）与帮战真人局**（`MATCH_MODE_PVP_TEAM` / `MATCH_MODE_GUILD_WAR_PVP` / `MATCH_MODE_GUILD_WAR_SIEGE`） | `PUBLIC` | **默认：全部真人参战者均允许（全员 AND，一票否决）**；`【可配策略位】`3V3/5V5 可按配置切换为「双方队长均允许」 | 默认全员 AND 守住「我关了就绝对无场外交流」的硬承诺；若线上实测 5V5 随机匹配开启率过低，可经配置切至双方队长裁决 |
| **《群仙问道赛》**（`MATCH_MODE_SEASON_TOURNAMENT`） | `PUBLIC` | 须同时满足：`TournamentRule.allow_spectate_chat == true` **且** 双方战队队长（或全员）均允许 | 正式淘汰赛由赛事模板保底防刷屏，海选/热身约战可自由放开 |
| **服务端总控开关关闭**（`SpectateChat.FreeTextEnabled == false`） | 全局/首审 | 恒为 `false` | 首审期或内容安全应急止血，秒级全局收口 |

> **为什么开局冻结、禁止局中动态改写？**  
> 避免出现“我顺风时开交流嘲讽、逆风怕对面有人支招就中途关掉”的赖皮博弈；同时让 `CreateBattleRequest` 原件保持与 `spectate_visibility`、`record_replay` 一致的不可变契约，断线重连、多 `match` 副本恢复、L1/L2 观众进场读到的结论 100% 一致。

### 3.4 开启后的交流规则与防作弊铁律（四道硬锁）

1. **铁律一：画面永远唯一中立，彻底废掉「双开看不同方」**
   - 观众帧继续 100% 走 [battle_room_manager.cpp](../../cpp/nodes/battle/logic/battle_room_manager.cpp#L2199-L2267) 的 `RedactStateForViewer(state, 0)` 与 [turn_battle_engine.cpp](../../cpp/libs/services/battle/system/turn_battle_engine.cpp#L7521-L7534) 的 `PopulatePrepStateForViewer(..., 0, ...)`。
   - 严格保持不变量：**`观众视角 ⊆ A方视角 ∩ B方视角`**（全员技能冷却、本人道具余量、投降投票、变阵禁护招盲选、队内战术标点、备选灵宠、装备隐藏概率一律剥离，未结算指令永不下发）。
   - **效果**：一台电脑开 2 个甚至 10 个号，分别从 A 方或 B 方入口点进观战，拿到的状态帧逐字节完全一致，不存在任何“偷看对手底牌”的可能。

2. **铁律二：交流室全场唯一公开，绝不分阵营私聊，物理隔断队内频道**
   - **队内私密通道**（队伍聊天 `CHAT_CHANNEL_TYPE_TEAM`、战斗内战术标点 `team_pings`、战前变阵提名）：**永远只对本队参战成员开放**，任何局外观众（含好友、同帮、双开小号）一律不可读、不可写。
   - **战斗公开观战交流室**（新增临时频道 `CHAT_CHANNEL_TYPE_BATTLE_SPECTATE`）：
     - 每场 `spectate_chat_enabled == true` 的战斗仅有**唯一一个公开室**，索引键为 `battle_id`；
     - 发言者身份标明清晰轻量标签：`【参战】` 或 `【观战】`（**不按阵营拆房**），A 方、B 方与全体观众看到的聊天流**完全一致**；
     - 观众在公开室里支招时，对手同样瞬间可见，彻底消除“单边暗中报点”的不对称优势；
     - 若某方确实需要私密商量集火或换宠，在**队内频道 / 队内标点**沟通即可，局外人不可见。

3. **铁律三：局内「防遮挡本地折叠」与黑名单联动**
   - 即使本局 `spectate_chat_enabled == true`，参战者与观众在战斗画面上仍拥有一个**纯本地显示按钮（「收起 / 展开观战聊天」）**——收起时只保留角标未读红点，绝不遮挡底部技能栏、25 秒读秒倒计时环与右上 140px 小游戏胶囊区。
   - 已加入个人黑名单的玩家发言在客户端自动过滤不展示。

4. **铁律四：未来扩展红线（上帝视角 / 跨项目迷雾局）**
   - 一旦未来任何玩法给观战开放“参战者看不到的额外信息”（如解说上帝视角看双方冷却/剩余大药/备宠，或跨项目用于战争迷雾 MOBA），**该模式必须在 `spectateChatEnabledForBattle` 中强制返回 `false`，且观战流整体加延迟**，严禁实时局外人交流。

### 3.5 技术架构与协议增量（解耦 `battle`，复用 `go/chat` 与 `content_guard`）

#### 为什么绝不把文本聊天塞进 `battle` 节点？
- `battle` 节点是确定性回合计算核心，承载直连低延迟战斗帧；且 L1 / L2 观众根本不连 `battle` 节点（直连席满 20 人后全走 `gate` 中继/广播）。
- 将观战交流归口到 **`go/chat`**，同时具备四大优势：① 与战斗计算物理隔离（刷屏打不卡回合结算）；② 天然覆盖 L0、L1、L2 观众与参战者；③ 零重复造轮子，直接走 `go/chat` 现成 `content_review.go`（`content_guard` 敏感词/风控）、`retract_report.go`（举报与撤回）、实名/禁言拦截；④ Redis 短 TTL 环形队列，战斗结束自动过期不膨 DB。

#### 最小契约增量（仅末尾追加字段，严禁复用字段号）
1. **玩家设置与建房快照**：
   - `proto/common/component/player_comp.proto`：`PlayerProfileComp` 末尾追加 `eSpectateChatPreference spectate_chat_pref = 15;`（定义 `enum eSpectateChatPreference`）。
   - `proto/scene/player_scene.proto`（或独立轻设置 RPC）：提供 `GetPlayerSocialSettings` / `SetSpectateChatPreference`（读/写本角色 `spectate_chat_pref`，返回最新生效值与全局默认态）。
   - `proto/battle/battle_data.proto`：`BattlePlayerSnapshot` 末尾追加 `bool allow_spectate_chat = 34;`（由 `scene` 在 `PrepareBattle` 填入解析后的布尔值）。
   - `proto/battle/battle_node.proto`：`CreateBattleRequest` 末尾追加 `bool spectate_chat_enabled = 25;`（由 `match` 建房冻结）。
   - `proto/battle/player_battle.proto`：`BattleStateS2C` 末尾追加 `bool spectate_chat_enabled = 47;`（随开战首帧 / 观战 161 / 重连补拉 / 回合帧透传；离线战报回放时客户端忽略交流区）。
   - `proto/match/match_service.proto`：`BattleWatchSummary` 末尾追加 `bool spectate_chat_enabled = 9;`（观战列表可顺手展示「可交流 / 静默观战」轻量标识）。
2. **`match` 侧权威元数据投影（供 `chat` 校验）**：
   - `match` 建房写 `spectate:battle:{id}` / 成局活跃元数据时，同步写入短生命周期 Redis 校验键 `battle:spectate_chat_meta:{<battle_id>}`（字段：`enabled(0/1)`、`visibility`、`deadline_ms`、`participant_ids` 集合，TTL = 战斗 `deadline_ms` + 5 分钟；战斗终局提交时即刻置 `closed=1` 并缩 TTL 为 60 秒）。
   - `go/chat` 发送/拉取战斗观战聊天前，**只读该权威元数据键**（fail-closed：查不到、已结束或 `enabled != 1` 一律拒回专用 tip，绝不信任客户端自报的 `spectate_chat_enabled`）。
3. **`go/chat` 临时战斗观战频道（复用 `proto/chat/chat.proto`）**：
   - `ChatChannelType` 追加 `CHAT_CHANNEL_TYPE_BATTLE_SPECTATE = 5;`。
   - `ChatMessage` 末尾追加：`uint64 battle_id = 6;`、`uint32 sender_battle_role = 7;`（`1=参战者 / 2=观战者`，由服务端按 `participant_ids` 权威填充，客户端不可伪造）、`uint64 seq = 8;`（该战斗室内单调递增序号）、`string sender_name = 9;`（服务端权威公开名截断）。
   - 复用 `SendChat` / `PullChatHistory`（或专用轻接口 `PullBattleSpectateChat{battle_id, after_seq, limit}`）与 `ReportChat`：
     - **存储**：Redis 有界环形列表/Stream（`chat:battle_spectate:{<battle_id>}`，单场上限保留最近 **60 条**，TTL 同战斗期限 + 5 分钟，战斗结束自动清退）；
     - **推/拉策略（防 L2 万级观众推送风暴）**：
       - 常规人少场次或首期极简闭环：客户端在战斗/观战打开交流区时，按 **1.5～2.5 秒增量拉取**（携带 `after_seq`，无新消息回空包，零 Gate 扇出耦合）；
       - 热门 L2 场次压测确需更低延迟时，二期再按每 500ms 聚合单包（每帧上限 6 条）复用观战广播通道下发，带宽恒定 O(1) 封顶。
4. **Tip 错误码（`data/tip/Tip.xlsx`，导表器单轴发号）**：
   - `kBattleSpectateChatDisabled`：「本场战斗已关闭观战交流（可正常观战）」
   - `kBattleSpectateChatEnded`：「本场战斗已结束，观战交流已关闭」
   - `kBattleSpectateChatCooldown`：「发言过于频繁，请稍后再试」
   - `kBattleSpectateChatRoomBusy`：「当前观战讨论过于火爆，请稍候再试」

---

## 4. 思想分析

1. **为什么把「观战可见性」和「观战交流权」彻底解耦？**
   - 很多玩家并不排斥别人看自己打架（甚至乐于被围观），他们真正排斥的是“场外有人插嘴指挥或起哄”。把两把锁拆开后，关闭交流完全不影响观战生态和赛事热度，体验损失降到最低。
2. **为什么坚持“开局冻结为整场规则”，而不是“每人只管自己收不收”？**
   - 如果只是本地静音，竞技场里依然存在“我静音了，但对手没静音且在接收场外人实时指挥”的心理不公平感；开局冻结 + PVP 一票否决，把“这局是纯净无场外局还是公开热闹局”变成了双方开战前明牌对等的契约。
3. **为什么不靠「查同电脑 / 同 IP 禁止双开」来防双开观战作弊？**
   - 客户端设备指纹与 IP 在现实中极易被“手机流量 + 电脑 Wi-Fi”或“帮友开语音共享屏幕”绕过，还会误伤同宿舍/同家庭/网吧正常玩家。真正的安全防线必须建立在**信息论不变量**上：让任意数量的观众窗口看到的都是严格等于双方交集的脱敏帧，并且隔离队内私密频道——就算他双开十个号，也偷不到半点隐藏信息。
4. **为什么「唯一公开室」天然能克制场外指挥？**
   - 一旦把交流室严格限定为全场公开、不分阵营，观众喊出的每一句战术建议都会同时暴露给对手，从“单边偷袭”变成了“擂台下观众起哄的明牌心理博弈”；再加上回合制固有的命中/暴击/乱敏随机性，既保住了江湖热闹感，又把恶性作弊降到最低。
5. **为什么必须放进 `go/chat` 而不是 `battle` 或 `match`？**
   - `battle` 要保确定性与低延迟，不能扛 UGC 文本与刷屏流量；`match` 管全局成局与匹配事务，不应再耦合敏感词审核与举报系统；`go/chat` 已具备完整的 `content_guard` 审核、实名禁言、举报与撤回底座，按单一职责原则是最短且最合规的归宿。

---

## 5. 漏洞分析与权威防线

| 潜在漏洞 / 攻击场景 | 风险后果 | 权威硬防线 |
|---|---|---|
| **1. 一台电脑双开两个号分别从 A/B 方入口观战偷底牌** | 偷看对手冷却、大药、备宠、变阵禁护招或指令 | 所有观众统一走 `RedactStateForViewer(state, 0)`，不存在阵营观战视角；`观众视角 ⊆ A方 ∩ B方`，双开多窗口画面完全一致 |
| **2. 对手开小号伪装粉丝混进己方频道偷听战术** | 泄露队内集火目标、换宠与变阵配合 | 禁止分阵营观战频道；队内聊天、队内标点 `team_pings` 与变阵提名物理隔离，仅同队参战者可见；观战交流仅唯一公开室 |
| **3. 改包客户端向已关闭交流的战斗强行发消息** | 绕过前端隐藏输入框，破坏纯净局承诺 | `go/chat` 发送入口强制校验权威元数据 `battle:spectate_chat_meta:{id}` 的 `enabled == 1` 且战斗未结束，fail-closed 拒回 `kBattleSpectateChatDisabled` |
| **4. 战斗外路人批量枚举 `battle_id` 刷屏或伪造「参战者」身份** | 骚扰陌生对局、冒充选手带节奏 | `sender_battle_role` 由服务端查 `participant_ids` 权威标注；观众须已通过观战准入（校验有效 `spectate_token` 或活跃观战记录，或至少校验目标局为 `PUBLIC` 且本账号非禁言/达最低等级门槛） |
| **5. 热门 L2 场次万级观众并发刷屏打爆服务端 / 遮挡选手读秒** | 拖垮聊天服、影响决赛选手 25 秒手动操作 | 三层限流：① 单人冷却（观众 4s / 参战者 2s）；② 单场总流速令牌桶（如全房上限 8 条/秒，超限回 `kBattleSpectateChatRoomBusy`）；③ 单场环形队列上限 60 条 + 客户端 2s 增量拉取 + 选手端一键本地折叠 |
| **6. 观战自由文本夹带违禁词、诈骗微信号、外链赌盘或假赛喊话** | 触碰 App Store / 小游戏 UGC 拒审与下架一票否决红线 | 100% 强制经过 `go/chat` 既有 `content_review.go`（`content_guard` 同步初审 + 异步复检撤回）、未实名/禁言账号拦截、黑名单过滤与 `ReportChat` 一键举报；配全局开关 `SpectateChat.FreeTextEnabled` 支持首审一键收口 |
| **7. 参战者中场切个人设置试图临时关停正在进行的公开室** | 顺风嘲讽、逆风赖皮关聊天，且引起状态竞态 | 个人设置变更**仅影响之后新开的战斗**，进行中战斗以建房冻结的 `spectate_chat_enabled` 为唯一真相（设置界面明确提示「下次进战斗生效」） |
| **8. 战斗结束后残留房间继续被当作黑产私聊室使用** | 绕过常规世界/私聊风控建地下聊天室 | 战斗终局回执或超时 `deadline_ms` 到期后立即标记 `closed = 1`，拒绝一切新发言并 60 秒内过期清理内存/Redis 键 |
| **9. 单副本内存存储导致多 `go/chat` 副本消息割裂** | 观众 A 与选手 B 落在不同副本互相看不见发言 | 严禁沿用单进程内存 map（避免重蹈 `SpectateCheerHub` 单机局限），首期即直接用 Redis 集群同 slot hash tag（`{<battle_id>}`）承载有界队列与限流 |
| **10. 未来新增上帝视角或套用到有迷雾玩法时误开实时交流** | 演变为实时透视报点外挂 | 在 `spectateChatEnabledForBattle` 中显式枚举白名单模式（同 `spectateVisibilityOfMode` 全枚举覆盖单测），新增模式未显式授权一律 fail-closed 返回 `false` |

---

## 6. 留给实现者自行决定的细节
- 玩家个人设置 `SetSpectateChatPreference` 是挂在既有 `PlayerScene` / 社交设置服务下，还是合并入轻量通用设置 RPC（以最小接口增量为准，不另起新微服务）；
- `go/chat` 拉取观战交流流采用复用 `PullChatHistory` 追加 `battle_id` / `after_seq` 字段，还是在 `ClientPlayerChat` 末尾追加专用轻量 `PullBattleSpectateChat`（以不破坏既有世界/私聊单测与字段清晰为优先）；
- 客户端战斗内观战交流面板的默认折叠高度与弹幕/气泡二选一展示形态（只需严守不遮挡技能栏、行动计时环与右上 140px 小游戏胶囊区，且零新切图依赖）。

---

## 7. 步骤分析与执行步骤（待负责人拍板默认档位与裁决细节后立项）

### 前置拍板项（请负责人确认 2 项即可开码）
1. **未手动设置时的默认值**：推荐 **默认开启**（玩家可一键关闭并永久记住偏好），避免随机匹配局因人人未改过设置导致永远开不出来；若希望更保守也可设默认关闭。
2. **3V3 / 5V5 随机匹配 PVP 的裁决粒度**：推荐 **全员真人均允许（全员 AND）** 作为一票否决护栏（配合默认开启，只有显式关掉的玩家才会锁死本局）；若担心高段位局开启率过低，可改按「双方队长均允许」。

### 阶段 1 · 协议与 Tip 契约（权威 C++/Go 仓）
1. 更新 `proto/common/component/player_comp.proto`（`eSpectateChatPreference` 与 `PlayerProfileComp.spectate_chat_pref = 15`）、`proto/scene/player_scene.proto`（读/写偏好 RPC）、`proto/battle/battle_data.proto`（`BattlePlayerSnapshot.allow_spectate_chat = 34`）、`proto/battle/battle_node.proto`（`CreateBattleRequest.spectate_chat_enabled = 25`）、`proto/battle/player_battle.proto`（`BattleStateS2C.spectate_chat_enabled = 47`）、`proto/match/match_service.proto`（`BattleWatchSummary.spectate_chat_enabled = 9`）、`proto/chat/chat.proto`（`CHAT_CHANNEL_TYPE_BATTLE_SPECTATE = 5`、`ChatMessage` 追加战斗观战字段与增量拉取请求）。
2. 在 `data/tip/Tip.xlsx` 追加 4 条观战交流 tip 码（导表器单轴发号）。
3. 交由 Gemini 执行 `dev_tools.ps1 -Command proto-gen-run`、Java `ContractSync` 与 Unity `gen_proto.ps1` / `gen_messageids.ps1`。

### 阶段 2 · 权威服务端（C++ `scene` / `battle` + Go `match` / `chat`）
1. **C++ `scene`**：落盘与读写 `PlayerProfileComp.spectate_chat_pref`；在 `PrepareBattle` 构造 `BattlePlayerSnapshot` 时按全局默认解析并填入 `allow_spectate_chat`。
2. **Go `match`**：
   - 新增纯函数 `spectateChatEnabledForBattle(...)`（全 `MatchMode` 枚举覆盖单测，表外值 fail-closed `false`）；
   - `gather` 建房冻结 `CreateBattleRequest.spectate_chat_enabled`，并写入 Redis 权威元数据 `battle:spectate_chat_meta:{<battle_id>}`（含 `participant_ids` 与 `deadline_ms`），终局提交时立刻置 `closed = 1`；
   - 顺手补齐既有 `SendTournamentSpectateCheer` / `GetTournamentSpectateCheer` 禁止本局参战者调用的权限守卫。
3. **C++ `battle`**：`BattleRoom` 保存冻结的 `spectateChatEnabled`，随 `BattleStateS2C` 透传给参战者与 L0/L1/L2 观众（绝不碰聊天业务，不新增线程）。
4. **Go `chat`**：
   - 实现 `CHAT_CHANNEL_TYPE_BATTLE_SPECTATE` 发送、增量拉取、举报与异步撤回；
   - 接入 `battle:spectate_chat_meta:{<battle_id>}` 权限与开关校验、全量 `content_review.go` 敏感词审核、实名/禁言校验、单人冷却与单房总流速令牌桶、Redis 有界 60 条环形队列与自动过期；
   - 补齐单元测试（关闭交流仍可观战、改包强发被拒、双开中立帧无差异、单房流速封顶、终局封房、敏感词与举报闭环）。

### 阶段 3 · Java / Python 镜像服务端对齐
1. `xuanming-server-mmo`（Java）：同步 proto 与 Tip 契约，在 `game-scene`、`game-match`、`game-battle`、`game-chat` 逐条同构实现，更新 `PARITY.md` 并跑单测回归。
2. `xuanming-server-mmo-python`（Python）：按同步基线跟进对应契约与测试。

### 阶段 4 · Unity 客户端接入与全链路验证（`mmorpg-client`）
1. **设置入口**：在系统/社交设置面板增加「允许局外人观战交流（开/关）」开关与说明文案（提示「关闭不影响被观战，仅禁止观战期间局外人发言；下次进入战斗生效」）。
2. **观战列表与战斗/观战 HUD**：
   - `SpectatePanel` 读取 `BattleWatchSummary.spectate_chat_enabled` 展示轻量状态标签；
   - `BattleHud` / `BattleScreen` 在 `BattleStateS2C.spectate_chat_enabled == true` 且非离线回放时，挂载轻量可折叠「观战交流」小窗（支持发言、区分【参战】/【观战】标签、一键举报、本地一键收起静音）；当 `spectate_chat_enabled == false` 时，观战端隐藏输入框并显示「本局已关闭观战交流（可正常观战）」，参战端零显示零打扰。
3. **验证门禁（Gemini 执行）**：C++ 裸指针预检 + MSBuild + GTest、Go `go test ./...`、Java `mvnw` 定向回归、Unity `client_compile_check.ps1` + EditMode 单测、本地实栈冒烟（开/关对照：关开关后依然可观战但局外人发言被服务端拒收；开开关后 L0/LOBBY 观众与参战者实时互通且敏感词被拦截），验证实证追加记入 `docs/PROGRESS.md`。
