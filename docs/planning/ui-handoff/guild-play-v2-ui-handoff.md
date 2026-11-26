# 帮会全天帮战 V2 · Unity UI 异机交接（给 ChatGPT / Codex 完整技术细节版）

> 日期：2026-11-24  
> 配套文档：[guild-play-v2.md](../guild-play-v2.md)（玩法规格） · [guild-play-v2-implementation-plan.md](../guild-play-v2-implementation-plan.md)（双服与客户端实施计划）  
> 与旧稿关系：全面替代 2026-11-22 的 [guild-play-ui-handoff.md](guild-play-ui-handoff.md)。

**分工边界**：
- **本机 Gemini（服务端 + 客户端非 UI）**：负责 C++/Go 与 Java 双服、协议/配表源、导表与客户端生成绑定（`Guildpb` / `GuildServiceRpc`）、纯 C# 状态与联网层 `GuildWarClient.cs` / `GuildWarModels.cs`。
- **异机 ChatGPT / Codex（仅作图）**：按项目既有的统一 UI 美术风格制作新增透明底无字切图（本文档不指定画风、材质或色号）；Unity 界面、Prefab、Mock 与最终联机接线按 **UI 层只绑定 `GuildWarClient` / `GuildClient` / `BattleClient` / `AppearanceClient` / 材料领取入口，严禁直接碰底层网络连接、不得写死表数值、不得按本地倒计时或血条动画私自判胜负** 执行。

本文已将**协议名、字段名与单位、枚举值、错误码名、纯 C# 客户端接口签名、页面布局规格、状态文案与完整脱敏 Mock JSON 样例**全部冻结，可直接发给 ChatGPT 立刻并行开工做 UI 与可点击演示。

---

## 一、玩家核心动线（0 匹配池，三条闭环）

1. **入口一 · 帮会界面 0 秒直打（阶段 1 完整闭环）**
   - 主界面点「帮会」→ 看到升级后的**动态帮旗**（占领灵脉徽记、本周战功前 2 名功臣、当前连胜王与建帮首胜史记），下方主按钮**「帮会争锋」**，次级**「帮会商店」「成员」「同道历练」「捐献与升级」**。
   - 点「帮会争锋」→ 一屏呈现顶部**本周全帮战功 4 档里程碑宝箱 + 灵脉分红**、中部**八仙岛 / 碧渊水宫 / 月汐湾 3 座灵脉战况卡**、底部**连胜悬赏条 + 在线迎战活人/守旗阵 0 秒直打列表 + 一键快速开战**。
   - 点「一键快速开战」或任一目标「直接开战」→ **0 秒直接进现有回合制战斗页**（无排队弹窗、不强制切图），使用本人真实装备/61 技能/宝宝；打完看胜负、660 材料、帮贡银两、悬赏/追杀暴击与旗势变化，点**「继续开战」**通宵连打，或点**「逛帮会商店」**兑换重铸砂（`3403`）、悟道残页（`3406`）与帮战限定永久形象。
   - 点灵脉卡「前往现场」→ 一键传送至对应野外图 1 线灵脉大旗旁。

2. **入口二 · 野外三图路上任意异帮直打（阶段 2 挂接）**
   - 在八仙岛（`scene_config_id=2`）、碧渊水宫（`5`）、月汐湾（`3`）路上看到任意异帮玩家/小队（不限对方是否占灵脉），点击对方弹轻动作条：**「发起帮战 / 执行追杀」「实时观战（激战中时）」「查看资料」**，0 秒开打。
   - 走到灵脉大旗旁看见占领帮大旗与旗势条，点旗直接踢馆或迎战；屏幕角落常驻轻量**「战意 / 和平免战」**徽标（天墉城 `scene_config_id=1` 自动显示「安全区」）。

3. **入口三 · 江湖追杀令与仇敌追踪（阶段 1 挂单 + 阶段 2 跨图堵截）**
   - 帮战败给异帮后，结算页直接给**「悬赏追杀」**按钮；也可在「帮会争锋 → 仇敌与追杀榜」选择固定档位（5,000 / 15,000 银两含 10% 销毁税，或帮会公费）挂出追杀令（支持勾选本帮或全区江湖榜）。
   - 猎手/帮友点**「追踪行踪」**（15 秒节流）查出目标所在野外图与分线（如「碧渊水宫 · 1 线」），点**「一键追踪前往」**直传该图入口，目标收到「敌帮追兵已踏入【碧渊水宫】」预警；找到头顶挂「追杀目标」红金标的对方点**「执行追杀」**开打，胜者领赏销单，被追杀者连反杀 2 名不同追兵直接撕毁追杀令并没收一半托管赏金。

---

## 二、页面结构与详细 UI 规格

优先扩展现有帮会窗口、结算页、帮会商店与大世界 HUD，按项目既有的统一 UI 美术风格执行（本文档不指定画风、材质或色号；底板无字、动态文字分层、适配刘海/胶囊安全区与最大字号）。

### 页面 1 · 帮会首页（扩展既有 `GuildWindow`）
- **无帮态**：保持现有本区帮会列表、我的申请、「申请加入」「撤回申请」「创建帮会」。
- **已入帮态**：
  - **上方帮旗区**：帮旗底图 + 帮名 + 等级（`Lv.X / 10`）+ 资金条（凸显帮战攒资金升 10 级）；若本帮占领灵脉，旗角叠灵脉徽记（如「镇守 · 碧渊水宫」）；旗下展示**本周战功功臣前 2 名**（并列），底部小字保留「建帮首胜：甲 & 乙（或等待首次题名）」。
  - **主操作区**：主按钮**「帮会争锋（全天开战）」**（有可领周宝箱 / 灵脉分红 / 本帮灵脉被踢馆时亮轻角标）；若玩家正处于帮战对局断线未回，主按钮变为「回到帮战对局」。
  - **次级导航**：`帮会商店`（带帮贡余额提示）、`同道历练`、`成员`、`战绩大事记`、`捐献与升级`。

### 页面 2 · 帮会争锋主界面（新建 `GuildWarWindow`，核心页）
推荐**上中下三段式横屏布局**（窄屏可纵向滚动底部目标列表）：
- **顶栏（集体进度与个人状态）**：
  - 左侧：**本周全帮战功进度条**（4 个宝箱节点：`300 / 1200 / 3000 / 6000`；显示当前值、个人战功达标线 `>= 30`、每个箱子呈「未达 / 可领（高亮呼吸）/ 本周已领」三态）；旁边放**「灵脉分红（可领 X 份）」**一键领取钮。
  - 右侧：当前帮贡余额、个人连胜标（如 `3 连胜`）、**「挂牌迎战」开关**、**「战意 / 和平免战」开关**（切和平冷却中显示 `脱战冷却 00:42`）、二级入口按钮**「守旗阵（X/5）」「仇敌与追杀榜」「大事记」**。
- **中栏（三座灵脉卡片横排 3 列）**：
  - 固定 3 张卡：`leyline_id = 1` 八仙岛（`scene_config_id=2`）、`2` 碧渊水宫（`5`）、`3` 月汐湾（`3`）。
  - 每张卡展示：灵脉名 + 地貌小底纹、占领帮名（无主显「无主 · 灵脉守卫」）、旗势条（`banner_hp / banner_max_hp`，如 `620 / 1000`）、护盾剩余时间（`shield_until_ms > now` 时显「整备护盾 12:30，期间可打真人不掉旗势」）、实时状态角标（`激战中 X 场` / `正被踢馆！` / `守旗驻守`）。
  - 每张卡 3 个按钮：**「攻打 / 驰援」**（0 秒进战）、**「观战」**（`active_battle_count > 0` 时可点，直进 `WatchBattle`）、**「前往现场」**（阶段 2 传至该图 1 线灵脉旗旁；阶段 1 灰显「二期开放」或隐藏）。
  - 本帮已占 1 座灵脉时，攻打其他两座灵脉卡片副文案提示「已占 1 座灵脉 · 攻破敌旗享破旗赏金」。
- **底栏（连胜悬赏 + 0 秒直打目标区）**：
  - 顶部一条**连胜悬赏横幅**：展示当前区最高连胜悬赏目标（如「[青云阁·剑尘] 5 连胜 · 挑落额外赏 +200 帮贡 / +6,000 银两」），右侧按钮「立即挑战」或「观战」。
  - 左侧/主体目标列表，双页签切换：**「在线迎战活人（亮绿灯）」** / **「敌帮守旗阵」**；每行显示目标名、所属帮会、等级、出战宝宝图标/名、人数（`1人` / `2人队`）、连胜标、状态（可挑战 / 激战中），右侧按钮「直接开战」或「观战」。
  - 右下角醒目主按钮：**「一键快速开战」**（服务端优先连有空的在线异帮活人，无活人 0 秒连守旗阵，绝不弹匹配等待框）。

### 页面 3 · 守旗阵弹窗 & 仇敌追杀榜弹窗 & 大事记抽屉
- **守旗阵弹窗（`GuildWarGarrisonDialog`）**：展示本帮 5 个守旗槽（`slot_index = 1..5`）：角色名、等级、外观头像、出战宝宝名、快照更新时间；底部按钮「登记 / 更新我的守旗阵（60 秒冷却）」「下阵」（帮主/长老可移除或置顶他人槽位）；显眼说明「守旗阵独立托管，不占用本人在线与战斗」。
- **仇敌与追杀榜弹窗（`GuildWarBountyDialog`）**：
  - 三页签：**「本帮追杀令」「全区江湖榜」「帮会仇敌录（24h）」**。
  - 追杀卡片字段：目标名、目标帮名、赏金（如 `4,500 银两` 或 `帮会公费`）、发布帮会、剩余倒计时（24h）、**已反杀进度（`已反杀 1/2，再反杀 1 次撕单`）**、最近追踪结果（如「碧渊水宫 · 1 线」或「藏身天墉城安全区 / 暂未露面」）。
  - 按钮：**「追踪行踪」**（带 15 秒冷却读秒）、**「一键追踪前往」**（阶段 2 跨图传入口）或**「直打结单」**（目标挂牌迎战时可用）。
  - **发单二级确认弹框**：从仇敌录或结算页点「悬赏追杀」弹出，选择固定 3 档：
    - `档位 1 · 普通追杀令`：付 `5,000 银两`（含 10% 销毁税，4,500 入赏金池）
    - `档位 2 · 重金追杀令`：付 `15,000 银两`（含 10% 销毁税，13,500 入赏金池）
    - `档位 3 · 帮会公费追杀令`（仅帮主/长老可用，今日剩余 `X/2`）：扣 `2,000 帮会资金`
    - 勾选框：`公开至全区江湖榜`（默认勾选）；明示「同一目标同时限 1 单、24 小时过期退还托管本金（税不退）」。
- **战绩大事记抽屉**：列本帮近 30 场帮战记录（时间、模式标签：野外直打 / 灵脉攻防 / 守旗踢馆 / 追杀结单、双方帮名、参战者、胜负平、旗势变化与悬赏/雪耻标记）；参战者行尾有「回放」入口，非参战帮友仅看摘要。

### 页面 4 · 帮战结算与无缝连战页（扩展既有 `BattleResultPanel`）
- 顶部：胜 / 负 / 平 / 作废主标题 + 对局类型标（真人帮战 / 守旗踢馆 / 追杀结单）+ 回合数。
- **暴击荣誉横幅**（有则展示，按优先级亮金/红条）：
  - `终结敌方 N 连胜！额外获得 +X 帮贡 / +Y 银两`
  - `追杀成功，替帮雪耻！领走赏金 X 银两`
  - `反杀追兵成功！（1/2）` 或 `连退追兵，撕毁追杀令！没收赏金 X 银两`
  - `破旗首功！击溃敌方灵脉战旗`
- **分项权威结算栏**（各项独立展示「已到账 / 确认中 / 满额 +0」，严禁互相推断）：
  1. **660 首套装备材料**：`+15`（基础 10 + 胜奖 5，含可交易 1）/ `+10` / `+0`（挂机速败明示「未达 3 回合/2 手动/有效贡献门槛，不扣 660 额度」）；
  2. **帮战即时收益**：个人帮贡 `+X`、银两 `+Y`、帮会资金 `+Z`、帮会周战功 `+W`；若触发同对手 30 分钟递减，显眼灰黄条提示「与该对手近期交手过多，本场帮战收益按 50% / 20% / 0% 结算，换对手立即恢复 100%」；
  3. **灵脉旗势变化**：`八仙岛灵脉：620 → 520 (-100)` 或 `护盾保护中（旗势不变）`。
- **底部动作按钮**：
  - 主按钮：界面发起局为**「继续开战」**（0 秒直接连下一场）；野外现场发起局为**「回到现场」**（回原地并带 10 秒保护倒计时）；
  - 败于异帮且对方可挂追杀令时，显示高亮按钮**「悬赏追杀」**（直开固定档位发单框）；
  - 次按钮：**「材料自选」「逛帮会商店」「返回帮会」**。

### 页面 5 · 帮会商店升级页（扩展既有 `GuildShop` 视图）
- 保留 3 分类页签与既有 11 件商品，新增 5 件核心硬货需给出明显品质角标与「仅自用」标识：
  - `goods_id=105` **重铸砂**（`item_id=3403`，2 级，120 帮贡，周限 10）
  - `goods_id=205` **悟道残页**（`item_id=3406`，3 级，100 帮贡，周限 12）
  - `goods_id=206` **聚宝银两袋**（1 级，80 帮贡，周限 10）
  - `goods_id=304` **帮战荣誉名签/称号令**（4 级，1,200 帮贡，永久限 1）
  - `goods_id=305` **帮战限定永久角色形象**（6 级，3,600 帮贡，永久限 1，支持点「试穿预览」，兑换后直通永久衣柜）
- 等级未达标行展示锁图标与引导文案「帮会升至 X 级解锁（打帮战攒资金升级）」；背包满沿用 `GuildAssetOrderStatus.PENDING` 展示「待发放，腾空背包自动到账」。

### 页面 6 · 战斗内环境徽标 & 野外现场 HUD
- **战斗页（阶段 1）**：当 `BattleStateS2C.guild_war_frenzy_stacks > 0` 时，战斗顶栏展示常驻徽标**「决胜煞气 ×N（伤害 +15N% / 疗盾 -15N%）」**，点击/长按展开说明。
- **野外现场 HUD（阶段 2）**：
  - 灵脉战旗 `WorldPoi` 头顶展示占领帮名、旗势条与护盾态，地面铺淡金阵纹标清灵脉战区范围；
  - 异帮玩家头顶：`[帮会名]` + 战意/连胜火焰标（3/5/8+ 三档）+ 追杀目标红金标 + 激战中双剑标（点双剑直接调 `WatchBattle`）；
  - 点击异帮玩家浮出轻动作条：**「发起帮战」**（若为追杀目标显示**「执行追杀」**）、「实时观战」「查看资料」；不满足条件（天墉城、无帮、低级、和平免战、10 秒保护、普通路人数超限）灰显并附精准原因；
  - 屏幕右上/安全区内非阻断顶部预警横幅（自动 6 秒收起，不打断操作）：
    - 踢馆预警：`本帮【碧渊水宫】灵脉正被【青云阁】踢馆（剩 62%）` + `[立即驰援]` `[忽略]`
    - 追兵预警：`敌帮追兵已踏入【碧渊水宫】，请小心迎战或暂避！` + `[查看追杀令]`

---

## 三、冻结的协议与字段契约（与服务端逐字一致）

> 所有时间戳均为服务端 Unix 毫秒（`uint64`），客户端计算倒计时一律用 `server_time_ms` + 本机单调时钟差值，禁止信任本机系统时间；64 位 ID 一律 `ulong`，严禁转 `float`/`double` 或按大小排序。

### 1. 枚举定义（`namespace Guildpb`）

```csharp
// 目标类型（StartGuildWarBattleRequest.target_kind）
public enum GuildWarTargetKind
{
    GUILD_WAR_TARGET_KIND_UNSPECIFIED = 0,
    GUILD_WAR_TARGET_KIND_QUICK_AUTO = 1,       // 一键快速开战：有空活人优先，无活人 0 秒打守旗阵
    GUILD_WAR_TARGET_KIND_LIVE_PLAYER = 2,      // 界面直打在线挂牌/战区异帮活人
    GUILD_WAR_TARGET_KIND_LEYLINE_GARRISON = 3, // 攻打指定灵脉守旗阵或无主灵脉守卫
    GUILD_WAR_TARGET_KIND_BOUNTY = 4,           // 追杀榜直接迎战挂牌中的追杀目标
}

// 帮战战况类型（记录与结算标示）
public enum GuildWarBattleKind
{
    GUILD_WAR_BATTLE_KIND_UNSPECIFIED = 0,
    GUILD_WAR_BATTLE_KIND_PANEL_PVP = 1,     // 帮会界面真人直打
    GUILD_WAR_BATTLE_KIND_GARRISON_SIEGE = 2,// 守旗阵踢馆 / 灵脉守卫战（AI 托管）
    GUILD_WAR_BATTLE_KIND_FIELD_ROAD = 3,    // 野外路上任意异帮点战（阶段 2）
    GUILD_WAR_BATTLE_KIND_LEYLINE_PVP = 4,   // 灵脉现场真人攻防
    GUILD_WAR_BATTLE_KIND_BOUNTY_HUNT = 5,   // 追杀令结单战
}

// 追杀令档位（PostGuildWarBountyRequest.tier）
public enum GuildWarBountyTier
{
    GUILD_WAR_BOUNTY_TIER_UNSPECIFIED = 0,
    GUILD_WAR_BOUNTY_TIER_NORMAL_SILVER = 1, // 5,000 银两（10% 税，4,500 入赏金池）
    GUILD_WAR_BOUNTY_TIER_HEAVY_SILVER = 2,  // 15,000 银两（10% 税，13,500 入赏金池）
    GUILD_WAR_BOUNTY_TIER_GUILD_PUBLIC = 3,  // 帮会公费（帮主/长老，扣 2,000 资金，每日限 2 张）
}

// 追杀令状态
public enum GuildWarBountyStatus
{
    GUILD_WAR_BOUNTY_STATUS_UNSPECIFIED = 0,
    GUILD_WAR_BOUNTY_STATUS_ESCROW_PENDING = 1, // 扣款确认中
    GUILD_WAR_BOUNTY_STATUS_ACTIVE = 2,         // 生效追杀中
    GUILD_WAR_BOUNTY_STATUS_COMPLETED = 3,      // 追杀成功已销单
    GUILD_WAR_BOUNTY_STATUS_TORN_BY_TARGET = 4, // 目标连反杀 2 名不同追兵撕毁
    GUILD_WAR_BOUNTY_STATUS_EXPIRED = 5,        // 24h 到期已退本金
}

// 实时推送预警种类（NotifyGuildWarAlert / GuildWarAlertS2C.kind）
public enum GuildWarAlertKind
{
    GUILD_WAR_ALERT_KIND_UNSPECIFIED = 0,
    GUILD_WAR_ALERT_KIND_LEYLINE_UNDER_SIEGE = 1, // 本帮灵脉正被踢馆
    GUILD_WAR_ALERT_KIND_LEYLINE_CAPTURED = 2,    // 灵脉易主广播
    GUILD_WAR_ALERT_KIND_PURSUER_ENTERED_MAP = 3, // 追兵已踏入目标所在野外图
    GUILD_WAR_ALERT_KIND_BOUNTY_RESOLVED = 4,     // 追杀雪耻或反杀撕单播报
    GUILD_WAR_ALERT_KIND_STREAK_BOUNTY = 5,       // 高连胜悬赏挂牌或终结播报
}
```

### 2. 核心 RPC 契约表（`GuildService` 新增 9 读/写 + 1 推送，阶段 2 增 2 场景 RPC）

| RPC 方法 | 关键请求字段 | 关键响应字段 |
|---|---|---|
| `GetGuildWarOverview` | `{}`（身份取会话） | `error_message`、`server_time_ms`、`week_key`、`next_weekly_reset_ms`、`my_stance`（`peace_mode`, `peace_ready_at_ms`, `open_challenge`, `protect_until_ms`, `my_streak`, `my_weekly_merit`, `my_bounty_payouts_today`, `my_bounty_payout_limit`）、`banner_honor`（`occupied_leyline_id`, `weekly_mvps[0..1]`, `first_win_names[0..1]`, `first_win_at_ms`）、`leylines[3]`、`milestones[4]`、`dividend`（`claimable_count`, `claimed_this_week`, `weekly_cap`, `guild_produced_this_week`, `guild_weekly_cap`）、`streak_bounties[]`、`live_targets[]`、`garrison_slots[]`（本帮 5 槽）、`enemies[]`（近 24h 仇敌）、`bounties[]`（本帮 + 全区生效追杀令）、`recent_logs[]`（近 30 场）、`active_battle_id`（断线回局） |
| `SetGuildWarStance` | `set_peace_mode` (`optional bool`), `set_open_challenge` (`optional bool`) | `error_message`、`my_stance`、`server_time_ms` |
| `RegisterGuildWarGarrison` | `action`（1=登记/更新, 2=下阵, 3=长老/帮主移槽置顶）、`target_player_id`（管理操作时填） | `error_message`、`garrison_slots[]`、`next_refresh_at_ms` |
| `StartGuildWarBattle` | `target_kind`、`leyline_id`、`target_player_id`、`bounty_id`、`client_request_id` | `error_message`、`battle_id`（成功时非 0，直进战斗）、`war_battle_kind`、`offender_player_id`、`offender_battle_id`（目标刚开战时非 0，用于弹「目标激战中 → 一键观战」） |
| `ClaimGuildWarMilestone` | `milestone_id`（1..4）、`client_request_id` | `error_message`、`milestone`（更新后的档位态）、`asset_order_status`（`PENDING`/`APPLIED`） |
| `ClaimGuildWarDividend` | `client_request_id` | `error_message`、`claimed_count`、`dividend`、`asset_order_status` |
| `PostGuildWarBounty` | `target_player_id`、`tier`（1..3）、`public_board` (`bool`)、`client_request_id` | `error_message`、`bounty`（`ESCROW_PENDING` 或 `ACTIVE`） |
| `TrackGuildWarBounty` | `bounty_id` | `error_message`、`target_online`、`in_safe_zone`、`scene_config_id`（`2/3/5`，安全区/离线为 0）、`channel_no`、`next_track_at_ms` |
| `GetGuildWarBattleResult` | `battle_id` | `error_message`、`settled` (`bool`, false=待确认需退避轮询)、`war_battle_kind`、`outcome`、`rounds`、`material_units_added`、`material_tradable_added`、`material_pvp_loss_ineligible`、`contribution_gain`、`silver_gain`、`guild_funds_gain`、`merit_gain`、`streak_after`、`bounty_jackpot_contribution`、`bounty_jackpot_silver`、`bounty_outcome_tag`（0无/1终结连胜/2追杀成功/3反杀成功/4反杀撕单/5破旗首功）、`diminish_percent`（100/50/20/0）、`leyline_id`、`banner_hp_before`、`banner_hp_after`、`banner_shielded`、`can_post_bounty`、`bounty_candidate_player_id`、`bounty_candidate_name` |
| `NotifyGuildWarAlert` (PUSH) | — | `kind`、`leyline_id`、`scene_config_id`、`channel_no`、`attacker_guild_name`、`defender_guild_name`、`actor_name`、`target_name`、`banner_hp_percent`、`bounty_id`、`battle_id`、`reward_silver`、`streak_count` |
| `AttackFieldGuildPlayer`（阶段 2, Scene） | `target_player_id`、`bounty_id`（可 0）、`client_request_id` | `error_message`、`battle_id`、`offender_battle_id` |
| `TravelToGuildWarTarget`（阶段 2, Scene） | `purpose`（1=前往灵脉现场, 2=追踪追杀目标）、`leyline_id`、`bounty_id` | `error_message`、`target_scene_config_id`、`target_channel_no` |

### 3. 错误 Tip 码名对照（`guild_error`，由 `Tip.xlsx` 自动生成，UI 禁写死数字）

`kGuildWarNotOpen`、`kGuildWarLevelTooLow`、`kGuildWarSameGuild`、`kGuildWarTargetPeaceMode`、`kGuildWarSelfPeaceMode`、`kGuildWarPeaceCooldown`、`kGuildWarTargetProtected`、`kGuildWarTargetInBattle`（此时必读 `offender_battle_id` 弹观战！）、`kGuildWarTargetOffline`、`kGuildWarSafeZone`、`kGuildWarTeamInvalid`、`kGuildWarTeamSizeExceedsRoadLimit`、`kGuildWarLeylineNotFound`、`kGuildWarGarrisonFull`、`kGuildWarGarrisonCooldown`、`kGuildWarMilestoneLocked`、`kGuildWarMilestoneClaimed`、`kGuildWarDividendEmpty`、`kGuildWarBountyInvalidTarget`、`kGuildWarBountyAlreadyActive`、`kGuildWarBountyLimit`、`kGuildWarBountyNotFound`、`kGuildWarTrackCooldown`、`kGuildWarServiceBusy`（fault）。

---

## 四、给 UI 绑定的纯 C# 客户端接口签名（`MmorpgClient.Game.Guild`）

UI 层（`MonoBehaviour` / UGUI Window）**只需绑定 `GuildWarClient`**（由 Gemini 在 Batch 3 交付；ChatGPT 在本地演示期可先用同一接口的 `IGuildWarViewSource` 或 Mock 构造 `GuildWarOverviewSnapshot` 实现 100% 同签名无缝替换）：

```csharp
namespace MmorpgClient.Game.Guild
{
    public sealed class GuildWarClient : IDisposable
    {
        public static GuildWarClient Instance { get; private set; }

        // 只读状态（不可变快照，UI 仅渲染，绝不原地修改）
        public GuildWarOverviewSnapshot Overview { get; private set; }
        public GuildWarBattleSettlementSnapshot ActiveSettlement { get; private set; }
        public ulong LastConflictBattleId { get; private set; } // 并发抢点落败时的观战 battle_id
        public bool OverviewLoading { get; private set; }
        public bool ActionInFlight { get; private set; }
        public bool SettlementPolling { get; private set; }
        public string Status { get; private set; }
        public string LastError { get; private set; }

        // 服务端锚定当前时间戳（Unix ms），所有倒计时（护盾/冷却/24h追杀/10s保护）一律减此值
        public ulong EstimatedServerNowMs { get; }

        // 事件（UI 必须在 OnDisable / OnDestroy 对称退订！）
        public event Action Changed;
        public event Action<GuildWarAlertSnapshot> AlertReceived;
        public event Action<GuildWarBattleSettlementSnapshot> SettlementOpened;
        public event Action<ulong> TargetAlreadyInBattle; // 参数 = offender_battle_id，UI 弹「实时观战 / 换目标」

        // 读与写操作
        public void RefreshOverview();
        public void SetPeaceMode(bool enabled);
        public void SetOpenChallenge(bool enabled);
        public void RegisterMyGarrison();
        public void RemoveGarrison(ulong targetPlayerId = 0);
        public void StartQuickBattle(uint preferredLeylineId = 0);
        public void StartLivePvpBattle(ulong targetPlayerId, uint leylineId = 0);
        public void StartLeylineSiege(uint leylineId);
        public void StartBountyBattle(ulong bountyId, ulong targetPlayerId);
        public void ClaimMilestone(uint milestoneId);
        public void ClaimDividend();
        public void PostBounty(ulong targetPlayerId, GuildWarBountyTier tier, bool publicBoard);
        public void TrackBounty(ulong bountyId);
        public void QueryBattleResult(ulong battleId); // PENDING 自动有界退避轮询 [2,3,5,8,10,12]s
        public void Tick(); // 每帧调用：驱动结算退避轮询与推送队列

        // 阶段 2（野外现场）
        public void AttackFieldPlayer(ulong targetPlayerId, ulong bountyId = 0);
        public void TravelToLeyline(uint leylineId);
        public void TravelToBountyTarget(ulong bountyId);
    }
}
```

---

## 五、本地可点击演示脱敏 JSON 样例（ChatGPT 可直接粘贴做 Mock 数据）

### 样例 A：帮会争锋总览（`GetGuildWarOverviewResponse`）
```json
{
  "server_time_ms": 1795430400000,
  "week_key": 202648,
  "next_weekly_reset_ms": 1795986000000,
  "my_stance": {
    "peace_mode": false,
    "peace_ready_at_ms": 1795430442000,
    "open_challenge": true,
    "protect_until_ms": 0,
    "my_streak": 3,
    "my_weekly_merit": 145,
    "my_bounty_payouts_today": 1,
    "my_bounty_payout_limit": 5
  },
  "banner_honor": {
    "occupied_leyline_id": 2,
    "weekly_mvps": ["云中客", "青莲小医"],
    "first_win_names": ["云中客", "松风道童"],
    "first_win_at_ms": 1795300000000
  },
  "leylines": [
    {
      "leyline_id": 1,
      "name": "八仙岛 · 灵脉",
      "scene_config_id": 2,
      "owner_guild_id": 10001,
      "owner_guild_name": "青云阁",
      "banner_hp": 820,
      "banner_max_hp": 1000,
      "shield_until_ms": 0,
      "active_battle_count": 2,
      "under_siege": false,
      "featured_watch_battle_id": 900101
    },
    {
      "leyline_id": 2,
      "name": "碧渊水宫 · 灵脉",
      "scene_config_id": 5,
      "owner_guild_id": 10002,
      "owner_guild_name": "沧澜盟",
      "banner_hp": 380,
      "banner_max_hp": 1000,
      "shield_until_ms": 0,
      "active_battle_count": 1,
      "under_siege": true,
      "featured_watch_battle_id": 900102
    },
    {
      "leyline_id": 3,
      "name": "月汐湾 · 灵脉",
      "scene_config_id": 3,
      "owner_guild_id": 10003,
      "owner_guild_name": "归墟堂",
      "banner_hp": 600,
      "banner_max_hp": 1000,
      "shield_until_ms": 1795431150000,
      "active_battle_count": 0,
      "under_siege": false,
      "featured_watch_battle_id": 0
    }
  ],
  "milestones": [
    { "milestone_id": 1, "required_merit": 300, "guild_merit": 1420, "claimed": true, "claimable": false, "preview": "重铸砂×2 + 银两×5000" },
    { "milestone_id": 2, "required_merit": 1200, "guild_merit": 1420, "claimed": false, "claimable": true, "preview": "悟道残页×4 + 重铸砂×3" },
    { "milestone_id": 3, "required_merit": 3000, "guild_merit": 1420, "claimed": false, "claimable": false, "preview": "悟道残页×6 + 灵兽口粮×20" },
    { "milestone_id": 4, "required_merit": 6000, "guild_merit": 1420, "claimed": false, "claimable": false, "preview": "重铸砂×5 + 悟道残页×8 + 帮贡×300" }
  ],
  "dividend": {
    "claimable_count": 3,
    "claimed_this_week": 2,
    "weekly_cap": 12,
    "guild_produced_this_week": 9,
    "guild_weekly_cap": 24
  },
  "streak_bounties": [
    { "player_id": 20011, "player_name": "剑尘", "guild_name": "青云阁", "streak": 5, "bonus_contribution": 200, "bonus_silver": 6000, "in_battle": false, "battle_id": 0 }
  ],
  "bounties": [
    {
      "bounty_id": 70001,
      "target_player_id": 20011,
      "target_name": "剑尘",
      "target_guild_name": "青云阁",
      "issuer_guild_name": "沧澜盟",
      "tier": 1,
      "reward_silver": 4500,
      "public_board": true,
      "status": 2,
      "counter_kills": 1,
      "counter_kills_to_tear": 2,
      "expires_at_ms": 1795516800000,
      "last_scene_config_id": 5,
      "last_channel_no": 1,
      "next_track_at_ms": 1795430412000
    }
  ]
}
```

### 样例 B：帮战结算（真人胜 + 终结 5 连胜悬赏 + 660 材料满额）
```json
{
  "battle_id": 900188,
  "settled": true,
  "war_battle_kind": 4,
  "outcome": 1,
  "rounds": 8,
  "material_units_added": 15,
  "material_tradable_added": 1,
  "material_pvp_loss_ineligible": false,
  "contribution_gain": 80,
  "silver_gain": 3000,
  "guild_funds_gain": 400,
  "merit_gain": 30,
  "streak_after": 4,
  "bounty_jackpot_contribution": 200,
  "bounty_jackpot_silver": 6000,
  "bounty_outcome_tag": 1,
  "diminish_percent": 100,
  "leyline_id": 1,
  "banner_hp_before": 820,
  "banner_hp_after": 720,
  "banner_shielded": false,
  "can_post_bounty": false
}
```

---

## 六、可点击演示与截图验收清单

| 验收场景 | 演示动作与必交截图 |
|---|---|
| 1. 帮会首页动态帮旗 | 展示占领灵脉徽记、本周战功前 2 功臣、建帮首胜史记、帮会等级与资金条；空态与满态各 1 张 |
| 2. 帮会争锋主界面（0 匹配池） | 3 座灵脉卡片（激战中 / 被踢馆 / 护盾中）、周战功 4 档箱与灵脉分红、连胜悬赏条、在线活人与守旗阵切换；并发抢点返回 `kGuildWarTargetInBattle` 弹「实时观战 / 换目标」 |
| 3. 守旗阵、仇敌追杀榜与发单 | 守旗阵 5 槽弹窗；仇敌录选固定档位发追杀令（明示 10% 税）；追杀榜 15 秒追踪显示「碧渊水宫 · 1 线」及「藏身安全区」态；已反杀 `1/2` 进度 |
| 4. 结算暴击与无缝连战 | 展示样例 B（+15 材料含 1 可交易、+80 帮贡、终结 5 连胜 +200 帮贡/+6000 银两、旗势 -100）、挂机速败 `+0` 原因、同对手递减 `50%/0%`、败于异帮亮「悬赏追杀」按钮、异步「确认中」态及「继续开战」 |
| 5. 帮会商店硬货与战斗煞气 | 商店展示重铸砂（`3403`）、悟道残页（`3406`）、银两袋与帮战限定永久形象（含免费试穿、帮会等级未解锁态、背包满「待发放」）；战斗顶栏展示第 12 回合「决胜煞气 ×3」徽标与 `WatchBattle` 观战 |
| 6. 野外现场直打与追杀预警（阶段 2） | 三图灵脉大旗与战区边界、异帮头顶帮名/连胜火焰/追杀红金标/激战双剑、点击动作条「发起帮战 / 执行追杀 / 观战」、天墉城安全区、和平 60s 冷却、战后 10s 保护与踢馆/追兵顶部非阻断预警横幅 |
| 7. 双形态安全区与双服联机 | 16:9 与带刘海/小游戏胶囊横屏、最大字号与长中文名验收；接 Gemini 绑定后分别连 C++/Go 与 Java 跑通全闭环并标注演示/联机证据 |

---

## 七、可直接整段复制给 ChatGPT / Codex 的指令

```text
请在本机 Unity 客户端 mmorpg-client 制作「帮会全天帮战 V2」的全部 UI 界面、Prefab 与本地可点击演示，并在协议与 GuildWarClient 就绪后完成联机接线（视觉风格完全按项目既有的统一 UI 美术风格，不另定画风或色号）。

先读客户端 AGENTS.md 及引用规范（严守微信/抖音小游戏 WebGL2 单线程与 iOS/Android IL2CPP 约束：禁 Thread/Task.Run/Task.Delay/Reflection.Emit，所有事件在 OnDisable/OnDestroy 对称退订，适配手机刘海与小游戏右上角胶囊安全区），再完整阅读随任务附带的 C++/Go 主仓文档：
1. docs/planning/ui-handoff/guild-play-v2-ui-handoff.md（本文：已冻结全部页面结构、RPC/字段名、枚举值、Tip 码名、GuildWarClient C# 签名与脱敏 Mock JSON 样例）
2. docs/planning/guild-play-v2.md（完整玩法规则）

核心要求：
- 彻底无匹配池、无排队倒计时弹窗，主打「0 秒直打、全天连战、三座灵脉争夺、野外任意异帮直打、江湖追杀令与仇敌追踪、4 层丰富帮会奖励」。
- 分两阶段制作：
  - 阶段 1：扩展帮会首页（动态帮旗/占领灵脉角标/本周功臣/资金条）；新建「帮会争锋」主界面（顶部 4 档周战功宝箱+灵脉分红+挂牌迎战/和平免战开关，中部八仙岛/碧渊水宫/月汐湾 3 座灵脉卡，底部连胜悬赏横幅+在线活人/守旗阵 0 秒直打列表+一键快速开战）；新建守旗阵 5 槽弹窗、仇敌与追杀悬赏榜弹窗（3 固定档位明示 10% 税、15 秒追踪、反杀 1/2 进度）、近 30 场战绩大事记抽屉；扩展帮战结算页（660 材料、帮贡银两、悬赏/追杀/反杀/破旗暴击条、败于异帮快捷「悬赏追杀」、同对手递减提示、异步「确认中」与「继续开战 / 回到现场」）；升级帮会商店展示重铸砂（3403）、悟道残页（3406）、聚宝银两袋与帮战限定永久形象试穿兑换；战斗顶栏补第 12 回合「决胜煞气」徽标。
  - 阶段 2：在八仙岛/碧渊水宫/月汐湾三图接灵脉战旗 WorldPoi 与战区视觉、异帮头顶帮名/连胜火焰/追杀目标红金标/激战双剑观战标、点击异帮轻动作条（发起帮战/执行追杀/实时观战）、和平免战 60 秒冷却与战后 10 秒保护提示，以及灵脉被踢馆驰援与追兵踏入本图的非阻断顶部预警横幅。
- UI 层只绑定 docs/planning/ui-handoff/guild-play-v2-ui-handoff.md §四定义的 GuildWarClient / 只读快照接口（先用 §五的脱敏 JSON 搭本地带「演示」角标的可点击原型，绝不触达正式网络与资产；服务端与 GuildWarClient 交付后直接换接真源零重构）。
- 视觉风格完全按项目既有的统一 UI 美术风格执行（严禁另定画风、材质或色号），底板无字、动态文字分层，字号与触控区兼容最大字号与长中文名，键鼠与触控皆可完整操作。完成后按 §六交付 Prefab/素材路径、全部验收截图与客户端编译检查结果。
```

---

## 八、已验收正式切图与可选玩法扩展新增切图清单（仅给 Codex 作图，纯功能规格）

### 1. 已交付并完整接入 Unity UGUI 的 31 张正式运行切图
目录：`D:/work/image/designs/guild-war-ui-v2/slices/` → 已同步导入 `mmorpg-client/Assets/Resources/UI/Ugui/GuildWarV2/` 并全部接入 `GuildWarUiArt.cs` / `GuildWarWindow.cs` / `GuildWarFieldHud.cs` / `GuildWindow.cs` / `BattleScreen.cs`：
- 8 张无字主界面底板：`panel_01_guild_home.png` ～ `panel_08_field_hud.png`（`1932×814`）
- 23 张独立无字透明运行切片：`banner_guild_flag.png`、`leyline_bg_{baxian,biyuan,yuexi}.png`、`leyline_shield_overlay.png`、`bar_banner_hp_{track,fill_ally,fill_enemy}.png`、`badge_streak_{3,5,8}.png`、`badge_bounty_target.png`、`badge_spectate_swords.png`、`badge_stance_{war,peace}.png`、`badge_frenzy_round12.png`、`chest_merit_{locked,ready,claimed}.png`、`alert_banner_{siege,bounty}.png`、`tag_exclusive_bg.png`、`btn_danger_crimson.png`。

### 2. 已落地玩法扩展的可选增补无字切图（当前客户端已用文案标签兜底可用；如需更直观图标可交给 Codex 补画）

| 文件名 | 界面与用途 | 尺寸与格式 | 缩放与文字规范 | 交付目录 |
|---|---|---|---|---|
| `badge_gold_leyline.png` | 帮会争锋主界面灵脉卡右上角角标：标示当前小时轮换生效的「1.5× 旺脉（金脉）」 | `128×128` 透明底 PNG-32（四角 Alpha=0） | 非拉伸等比缩放；严禁烘焙任何文字、字母或数字；视觉按项目既有的统一 UI 美术风格 | `D:/work/image/designs/guild-war-ui-v2/slices/` |
| `badge_rival_guild.png` | 帮会争锋对手列表、灵脉卡及野外敌帮轻动作条角标：标示「本周宿敌帮会（本周交手 ≥ 3 场，收益 +20%）」 | `128×128` 透明底 PNG-32（四角 Alpha=0） | 非拉伸等比缩放；严禁烘焙任何文字、字母或数字；视觉按项目既有的统一 UI 美术风格 | `D:/work/image/designs/guild-war-ui-v2/slices/` |

