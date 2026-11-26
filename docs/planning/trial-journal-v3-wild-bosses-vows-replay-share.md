# 《五行奇谈》五行试炼志 V3 扩展设计：卷四·三境明雷、试炼誓约与好友约战录像

> 日期：2026-11-26  
> 负责人拍板范围：**全量落地（方案 C）**——「卷四 · 三境明雷」＋「自选限制挑战（试炼誓约）」＋「好友约战帖与受控 PVE 回合录像播放」。  
> 上位红线：[project-vision.md](project-vision.md)（六条红线 + 零拒审合规红线）  
> 前置基线：[long-term-progression-v2.md](long-term-progression-v2.md)、[world-map-play-v2.md](world-map-play-v2.md)、[battle-report-replay.md](../design/battle-report-replay.md)  
> 配套 UI 交接单：[ui-handoff/trial-journal-v3-ui-handoff.md](ui-handoff/trial-journal-v3-ui-handoff.md)

---

## 1. 需求分析

### 1.1 为什么这三项是「真好玩、非硬扩」的自然延展

1. **卷四 · 三境明雷（打通野外三图与试炼志的最后一公里）**：
   - 野外三图（`BaseScene 2` 八景岛、`BaseScene 5` 碧渊水宫、`BaseScene 3` 月汐湾）已双端开放联机并接入双层灵潮与野外栖息地捕宠；`BattleTrialJournalFacts` 字段 `17..20` 与三端判定条件 `6/7/8`（风眼 / 阳钟阴鼓 / 退潮终结）早已预埋完毕。
   - 补齐三只野外明雷首领副本与野外现场挑战入口后，玩家不再只是在主城面板里点首领试炼，而是真正带着宝宝和好友走进三张野外实景打一图一机制的专属首领，**顺手把世界地图 V2 的 M2 明雷首领一并闭环**。
2. **试炼誓约（自选限制挑战，解决毕业后策略深度天花板）**：
   - 首套毕业并收齐 8 件法宝后，玩家最爽的复玩动机是「给自己加限制、用冷门构筑解题」，而不是反复刷无聊数值日常。
   - 提供 4 条彼此正交、真正改变回合决策的**试炼誓约**，由玩家开战前自愿勾选（0 强迫、0 战力加成、0 可交易材料），达成后在试炼志中永久镌刻最高誓约数与组合印记。
3. **好友约战帖 + 受控 PVE 回合录像播放（让单人/好友破局成果「看得见、抄得着、能叫板」）**：
   - 试炼志目前已有脱敏配装快照，但好友既看不到「怎么在第 5 回合断疗破局」的完整回合操作，也不能一键照同誓约接招。
   - 引入「受控试炼 PVE 录像录制 + 显式签发权威约战帖」机制：不破坏既有 PVP 战报隐私隔离（`BR-9`），且以严格的配额 + 引用计数杜绝 PVE 录像撑爆数据库。

### 1.2 四问准入自检（`project-vision.md §二`）

1. **普通零氪玩家**：三境明雷支持 1–5 人零门槛挑战，与常驻首领试炼共用既有 PVE 10 份基础账，无每日强制打卡；试炼誓约与约战帖完全免费且零数值属性，不勾选誓约照样拿满永久 36 印与 M901「星阵宗师」（18 印门槛不变，容错从 9 印扩至 18 印）。
2. **个人开发者收入**：零可交易核心料产出，不挤占官方直售与 7:3 商业模型；野外三图聚人 + 约战帖展示与录像回放直接放大永久收藏外观、灵宠幻化与技能特效的真实可见度。
3. **物价与长期循环**：卷四里程碑只给终身一次的 4 份仅自用重铸砂 + 零战力荣誉印戳/评语，绝不新增可交易货源；试炼誓约与约战帖恒产 0 货币、0 材料，纯靠策略乐趣拉长留存。
4. **防骗与资产安全**：约战帖经 `match` 权威签发、带永久 ID 与 14 天过期，走既有 `chat` 内容安全审核与禁言频控，不夹带外部链接或私下转账引导，且与交易/资金链完全隔离。

---

## 2. 设计分析

### 2.1 卷四 · 三境明雷（`chapter_id = 4`，永久位图追加）

#### 1) 新增 3 个野外明雷首领副本与怪物配表（全部复用宝宝库高清立绘，零新怪物原画依赖）

| 关卡格（`TrialJournalEntry.id`） | 副本 `Dungeon.id` / `PlayMode` | `BaseScene` | `boss_trial_kind` | 怪物组合（`Monster.id` → 宝宝库视觉映射） | 核心地貌机制（`turn_battle_engine` 权威执行） |
|---|---|---|---|---|---|
| `401` **八景岛·风眼引风** | `113` / `113(单), 213(队)` | `2` 八景岛 | `12` (`kBossTrialKindWildWind`) | `111` 风眼妖尊（云鹤 `Pet 4`）＋ `112` 疾风护卫（竹风狸 `Pet 13`） | 第 1/4/7/10 回合风眼预告下轮慢速强风打击，并对本回合主动直伤首领者挂「引风点名」；下回合风击命中时，若目标未处防御或无护盾保护并被击倒，置 `boss_wind_hit_unprotected_death = true` |
| `402` **碧渊水宫·水镜钟鸣** | `114` / `114(单), 214(队)` | `5` 碧渊水宫 | `13` (`kBossTrialKindWildBell`) | `113` 水镜妖尊（石灵 `Pet 2`）＋ `114` 钟鼓护卫（绛铃 `Pet 6`） | 每 2 回合交替环境相：**阳钟**（R1-2/5-6/9-10，物理直伤 +15%、法术减伤 12%）与 **阴鼓**（R3-4/7-8/11-12，法术直伤 +15%、物理减伤 12%）；阳钟期己方造成 >0 物理直伤置 `dealt_yang_bell_physical_damage = true`，阴鼓期己方造成 >0 法术直伤置 `dealt_yin_drum_magic_damage = true` |
| `403` **月汐湾·涨退潮** | `115` / `115(单), 215(队)` | `3` 月汐湾 | `14` (`kBossTrialKindWildTide`) | `115` 潮汐妖尊（金猊 `Pet 3`）＋ `116` 赶海护卫（灵狐 `Pet 1`） | 每 2 回合交替潮汐相：**涨潮**（R1-2/5-6/9-10，敌方减伤 25% 蓄势）与 **退潮**（R3-4/7-8/11-12，敌方破绽易伤 +20%）；在退潮期击败敌方终结战斗置 `finished_in_ebb_tide = true` |

- **经济与准入边界**：
  - `Dungeon 113..115` 设置 `max_team_size = 5`、`difficulty = 1`（开放门槛，不锁困难前置，让所有玩家到野外三图就能打）、`round_limit = 12`、`allow_pet_capture = 0`，纳入 `IsBossTrialKind(kind)` 与常驻首领试炼共用同一份 PVE 基础奖励账本。
  - **场景聚人联动**：野外三图底部 HUD 浮条（`WildPetHabitatBar`）新增本图明雷首领一键挑战按钮；从「五行试炼志 → 卷四」点「前往挑战」时，若玩家已在对应野外图则直接开打，若在其他地图则一键传送至该野外图并提示开启挑战（服务端 `PrepareBattle` 对 `Dungeon 113..115` 校验玩家须位于对应 `dungeon.scene_id`，既防止原地远程白嫖、又切实向野外三图聚人）。

#### 2) 卷四战法印（9 枚永久印）与里程碑（2 档，零战力膨胀）

| 关卡格 | 印记 ①（通） | 印记 ②（速/守） | 印记 ③（巧 · 既有条件枚举 `6/7/8`） |
|---|---|---|---|
| `401` 八景岛·风眼引风 | `4011` **破阵**（`kClear=1`） | `4012` **速破**（`kSpeedClear=2, param=8`）：8 回合内通关 | `4013` **御风**（`kWindDefend=6, param=0`）：全场零无护风击倒地并通关 |
| `402` 碧渊水宫·水镜钟鸣 | `4021` **破阵**（`kClear=1`） | `4022` **无损**（`kFlawlessSelfAndPet=3`）：本人及宝宝全程未倒地 | `4023` **应钟**（`kBellHarmony=7`）：阳钟物理与阴鼓法术均造成有效直伤并通关 |
| `403` 月汐湾·涨退潮 | `4031` **破阵**（`kClear=1`） | `4032` **无损**（`kFlawlessSelfAndPet=3`）：本人及宝宝全程未倒地 | `4033` **趁汐**（`kTideFinish=8`）：在退潮破绽窗口终结战斗并通关 |

- **卷四里程碑（追加进 `TrialJournalMilestone.xlsx`，永久位图自动追加，不改旧位）**：
  - `M401 · 踏境问阵`（`scope_chapter_id = 4, required_seals = 4`）：仅自用重铸砂 ×2 ＋ 名片印戳「三境行者」（`badge_trial_three_realms`）
  - `M402 · 三境明雷`（`scope_chapter_id = 4, required_seals = 8`）：仅自用重铸砂 ×2 ＋ 留灯院评语《听雨·三境风物跋》（`commentary_tingyu_realms_v3`）
- **顺手修齐 `M901` 文案**：将 `TrialJournalMilestone.xlsx` 行 `901` 的静态 `desc` 从过时的「18/24 留 6 印容错」修正为「永久卷累计点亮 18 枚战法印，永久免费解锁零战力专属角色形象「星阵宗师」与名片印戳「五行宗师」」（不再写死旧分母）。

---

### 2.2 自选限制挑战：「试炼誓约」（4 条正交位掩码）

在 `trial_journal_rules.h`（及 Go / Java / Python / C# 同构常量）定义位掩码 `uint32_t trial_vow_mask`（合法掩码范围 `0 .. 0x0F`，越界一律 fail-closed 拒绝）：

| Bit | 值 | 誓约名 | 权威执行位置 | 战术影响 |
|---:|---:|---|---|---|
| `0` | `1` | **禁控硬破** (`kVowBanControl`) | `turn_battle_engine` 复用 `ban_control_skills` 规则（仅对玩家方生效） | 禁用己方一切控制技，纯靠驱散/破盾/防御/爆发过关 |
| `1` | `2` | **单宝轻装** (`kVowSingleArtifact`) | `turn_battle_engine` 开局装载 `BattleEquipment` 时截断玩家方仅保留 1 号槽法宝 | 放弃双法宝协同，逼迫单核心法宝最优解 |
| `2` | `4` | **无宠孤剑** (`kVowNoPet`) | `turn_battle_engine` 开局跳过玩家方出战灵宠入场 | 纯人物孤身破阵，极大改变承伤与集火节奏 |
| `3` | `8` | **紧缩回合** (`kVowTightRounds`) | `turn_battle_engine` 将本局逻辑回合上限压紧为 `max(6, ceil(base_limit * 0.7))`（12→9、15→11、8→6） | 拒绝磨血，必须在更短回合窗口内击杀 |

- **协议透传与合法性守门**：
  - `JoinQueueRequest` 追加 `uint32 trial_vow_mask = 14`；`StartTeamMatchRequest` 追加 `uint32 trial_vow_mask = 7`；`TrialBattleContext` 追加 `uint32 challenge_vow_mask = 6`；`BattleTrialJournalFacts` 追加 `uint32 challenge_vow_mask = 26`。
  - **门禁**：仅允许 `PVE_SOLO / PVE_TEAM` 且归属永久 PVE 首领试炼格（卷一 / 卷二 / 卷四，`IsBossTrialKind(boss_trial_kind)`）携带非 0 `trial_vow_mask`；非试炼格、PVP、护卫救援或掩码超出 `0x0F` 时，Go `match` 与 C++ `battle` 均直接拒绝（`kInvalidParameter`）。
  - 组队带誓约时由队长 `StartTeamMatchRequest.trial_vow_mask` 冻结进 `TrialBattleContext`，整队生效并在开局战斗快照 `BattleStateS2C.trial_vow_mask` 同步给全员展示。
- **持久化记录（挂在 `TrialJournalEntryRecord` 与 `TrialJournalEntryView`）**：
  - `TrialJournalSoloRecord` 追加 `uint64 battle_id = 4`；`TrialJournalCoopRecord` 追加 `uint64 battle_id = 6`。
  - `TrialJournalEntryRecord` 追加：
    - `uint32 best_vow_mask = 5`（达成最高誓约纪录时的誓约组合）
    - `uint32 best_vow_count = 6`（最高达成誓约条数 0..4）
    - `uint32 best_vow_rounds = 7`（该最高誓约纪录的通关回合数）
    - `uint64 best_vow_battle_id = 8`（该最高誓约纪录对应的权威 `battle_id`，供一键回放与发约战帖）
  - 刷新规则：带非 0 誓约且胜出时，若 `vow_count > best_vow_count` 或 (`vow_count == best_vow_count && total_rounds < best_vow_rounds`)，刷新誓约最佳纪录；`PlayerBattleResult` 追加 `uint32 trial_vow_mask = 47`、`bool trial_new_vow_best = 48`、`uint64 trial_battle_id = 49`。

---

### 2.3 好友约战帖 + 受控 PVE 回合录像播放

#### 1) 受控 PVE 录像录制策略（零爆库、零破坏既有 PVP 契约）
- 保留 `recordReplayFor(enabled, mode)` 原签名与全部既有 PVP 单测不变；新增 `recordReplayForBattle(enabled bool, mode matchpb.MatchMode, trialCtx *battlepb.TrialBattleContext) bool`：
  - 原 PVP 策略全部不变；
  - 仅当 `enabled && trialCtx != nil && trialCtx.GetJournalEntryId() != 0 && (mode == PVE_SOLO || mode == PVE_TEAM)` 时返回 `true`，将 `CreateBattleRequest.record_replay = true` 冻结进建房原件。
  - 常规野外刷怪 / 捕宠 / 帮会历练 / 木桩残局仍恒为 `false`，绝不泛滥录制。
  - `BattleReplayRecorder`（1 MiB raw / 512 KiB GZIP）与 `writeBattleReportTx`（每玩家 100 条 FIFO 淘汰 + `ref_count` 回收）**零改动直接复用**！

#### 2) 权威约战帖表与受控共享回放（严格守住 `BR-9` 与删角清理）
在 `proto/match/match_authority.proto` 新增权威 SQL 表 `MatchTrialChallengeCardRecord`（表名 `match_trial_challenge_card`，纳入 `authority.Tables()` 自动迁移）：
- 主键：`card_id`（`uint64`，`AllocateIdSegment(biz_tag=battle)` 永久 ID，严禁解析位结构）
- 索引：`owner_player_id,created_ms,card_id`、`expires_ms,card_id`
- 字段：`card_id`、`owner_player_id`、`owner_name`、`battle_id`、`journal_entry_id`、`trial_rule_version`、`vow_mask`、`total_rounds`、`party_size`、`created_ms`、`expires_ms`（14 天 TTL）、`summary`（序列化 `TrialChallengeCardView`，含脱敏配装摘要 `pet + artifacts + season_seal` 与 `has_replay`）。
- **配额与引用计数闭环（零孤儿、零爆库）**：
  - `CreateTrialChallengeCard(battle_id)`（客户端 RPC，挂 `MatchService`）：
    1. 核验调用方 `player_id` 来自 gate 可信会话（`ctxkeys`）且通过归属预检；
    2. 校验 `match_player_battle_report` 中存在 `(player_id, battle_id)`（只能分享自己打过的局），读取建房原件与终局结算确认该局为试炼志 PVE **有效胜局**；
    3. 在同一事务内：若 `match_battle_report` 存在该 `battle_id`，执行 `ref_count = ref_count + 1`（锁定该份回放不随个人 100 条列表被提前挤删）；插入 `match_trial_challenge_card`；检查该玩家有效约战帖数上限 **`MaxTrialChallengeCardsPerPlayer = 10`**，超出则按最旧 `created_ms` 淘汰多余行并经同一段 `releaseBattleReportsTx` 减引用数归零删除正文！
    4. 若同一玩家对同一 `battle_id` 重复点分享，直接幂等返回已有未过期 `card_id`（不重复占额度、不多加 `ref_count`）。
  - `GetTrialChallengeCard(card_id)`（只读无锁）：返回权威 `TrialChallengeCardView`；过期或不存在回 `ErrBattleReportNotFound`。
  - `GetSharedTrialReplay(card_id)`（只读无锁）：核验 `card_id` 在 `match_trial_challenge_card` 存在且未过期 → 读取对应 `match_battle_report.envelope` 返回给任意登录玩家播放！**原有 `GetBattleReport(battle_id)` 的 `BR-9` 私有权限原封不动**，未显式签发约战帖的私密对局绝不可被他人拉取。
  - **删角清理**：`RoleFencePurge` 中新增 `purgeTrialChallengeCardsTx(ctx, tx, player)`，删掉已删角色的全部约战帖并把对应 `battle_id` 合并进 `released` 经 `releaseBattleReportsTx` 统一释放 `ref_count`。

#### 3) 聊天分享与一键回放 / 一键同誓约应战（零改 `chat` 表结构、100% 过内容安全）
- 客户端点击「发帖约战」时：先调 `MatchService.CreateTrialChallengeCard(battle_id)` 拿到权威 `card_id`，再选择渠道（世界 / 组队 / 好友私聊）经既有 `SocialClient.SendChat` 发送规范文本：
  `【试炼约战】{关卡名}｜{誓约标签或常规}｜{回合数}回合｜#TJC:{card_id}#`
- 聊天界面（`SocialWindow`）解析 `#TJC:(\d+)#`（严禁信任伪造数值，点击时以 `MatchService.GetTrialChallengeCard(card_id)` 权威回包为准）渲染内嵌操作卡，提供：
  - **「看录像」**：调 `MatchService.GetSharedTrialReplay(card_id)` → 直接复用 `BattleReportClient` / `BattleReplayPlayer` 原生播放完整回合动画；
  - **「同誓约应战」**：拉取权威卡片 `(entry_id, vow_mask, total_rounds, owner_name)` → 打开 `TrialJournalUiRoot` 自动选中该格并预勾相同 `vow_mask`（卷四若不在对应野外图则自动先传送至该图），挑战胜利破纪录时在战斗结算条高亮提示「已破好友 {owner_name} 约战纪录！」。

---

## 3. 思想分析

1. **为什么野外三图明雷首领走「`Dungeon 113..115` + 场景准入校验 + 野外浮条」，而不是在 Scene 节点上养大世界巡逻怪实体？**  
   复用已验证的 `WorldMapV2` 栖息地范式（零 Scene 实体 AI CPU 预算、零抢怪摩擦、1–5 人各自独立开战），同时用 `scene_config_id == dungeon.scene_id` 强制把人聚到八景岛/碧渊水宫/月汐湾三张实景图里，兼顾性能、聚人与零代码膨胀。
2. **为什么试炼誓约只做 4 条、而且全部是改变决策的结构限制而非“敌人血量 +50%”？**  
   单纯加血加攻只是把玩家推回数值堆砌；「禁控硬破」「单宝轻装」「无宠孤剑」「紧缩回合」每条都直接改变配装和出招树，且全部落在既有战斗引擎能力上（复用 `ban_control_skills`、法宝装载、灵宠入场与回合上限），代码极短、可玩性极高。
3. **为什么约战帖录像走「`match_trial_challenge_card` 显式授权 + `ref_count` 锚定」，而不是直接放开 `GetBattleReport` 公开读？**  
   直接放开 `GetBattleReport` 会违背 `BR-9` 隐私契约，让陌生人靠枚举或泄露 `battle_id` 窥探任何人的战报；显式签发约战帖既给玩家明确的「主动分享」边界，又借 `ref_count` 保证好友点开有效约战帖绝不会因原列表被挤出而“录像失效”，同时严控每玩家最多 10 帖、14 天 TTL 与删角回收，存储完全有界。

---

## 4. 漏洞分析

| 潜在漏洞 | 攻击/异常场景 | 防护方案 |
|---|---|---|
| 伪造或越权分享他人战报 | 恶意客户端传陌生人的 `battle_id` 调 `CreateTrialChallengeCard` | 服务端严格核验 `match_player_battle_report` 存在 `(session_player_id, battle_id)` 且为试炼志 PVE 胜局，不符一律拒回 `ErrBattleReportNotFound` |
| 刷帖撑爆录像表 / 孤儿引用计数 | 反复对不同或相同局发约战帖、或发帖后删角 | ① 同 `(owner_player_id, battle_id)` 幂等复用旧帖不加 `ref_count`；② 每人上限 10 帖，超出事务内删最旧并 `releaseBattleReportsTx`；③ 删角走 `RoleFencePurge` 释放全部 `ref_count`；④ 加 `MessageLimiter` 频控 |
| 聊天伪造假战绩文本 | 玩家手打 `【试炼约战】1回合｜#TJC:999999#` | 客户端不信任聊天文本里的回合数，拉取 `GetTrialChallengeCard(card_id)` 以服务端权威回包为准；不存在或已过期一律提示「约战帖已失效」并禁止应战/播放 |
| 誓约带进非试炼/PVP/逃跑刷纪录 | 恶意客户端对 PVP 或普通副本传 `trial_vow_mask != 0`，或组队切誓约绕过限制 | Go `match` 入队/整队开战 + C++ `battle` 开局双端校验：仅 `IsBossTrialKind` PVE 允许非 0 掩码且 `mask <= 0x0F`；逃跑/败局不刷新任何印记与誓约纪录 |
| 远程逃课打卷四野外明雷首领 | 玩家留在主城直接发 `Dungeon 113/114/115` 绕过进野外图 | `Scene` `PlayerBattleSystem::PrepareBattle` 校验 `dungeon.scene_id > 1 && boss_trial_kind in 12..14` 时必须 `player_scene_config_id == dungeon.scene_id`，不符拒绝 |
| 卷四新增印记破坏旧位图或倒推 M901 | 老存档读新表后位图错位 | 严格向后追加 `TrialJournalSeal.xlsx`（`4011..4033`）与 `TrialJournalMilestone.xlsx`（`401..402`），由导表器维持旧 `cfg_bit_index` 不变；`M901` 仍为 `18` 印门槛 |

---

## 5. 步骤分析

1. **阶段 A · 权威配表与协议生成（C++/Go 权威仓）**：
   - 更新 `Monster.xlsx`（`111..116`）、`Dungeon.xlsx`（`113..115`）、`PlayMode.xlsx`（`113..115`, `213..215`）、`TrialJournalEntry.xlsx`（`401..403`）、`TrialJournalSeal.xlsx`（`4011..4033`）、`TrialJournalMilestone.xlsx`（`401..402` + 修正 `901` 文案）；
   - 更新 `proto/battle/battle_data.proto`、`proto/common/component/player_trial_journal_comp.proto`、`proto/common/component/player_battle_result_comp.proto`、`proto/scene/player_trial_journal.proto`、`proto/match/match_service.proto`、`proto/match/match_authority.proto`、`proto/team/team.proto`；
   - 运行导表与 `proto-gen`，补齐新 RPC 的 `MessageLimiter.xlsx` 限频行并重导，同步全量生成物到 `mmorpg-client`。
2. **阶段 B · C++ 战斗引擎与场景持久化（`xuanming-server-mmo-cpp`）**：
   - `turn_battle_constants.h` + `trial_journal_rules.h`：加入 `kBossTrialKindWildWind/Bell/Tide (12..14)` 与 4 条誓约常量/计算函数；
   - `turn_battle_engine.{h,cpp}`：实现风眼引风、水镜钟鸣、涨退潮 3 套机制与 `facts` 埋点，以及 4 条试炼誓约的权威局内执行；
   - `player_battle.cpp`：补齐卷四野外明雷首领场景匹配守门；
   - `player_trial_journal.cpp`：支持 `chapter_id = 4`（自动纳入永久卷计数）、持久化 `best_vow_*` 与 `solo/coop_best.battle_id`、回填 `PlayerBattleResult` 25..27；
   - 补齐 `turn_battle_engine_test` 与 `bag_test` 单测并全量编译跑绿。
3. **阶段 C · Go `match` 约战帖、受控 PVE 录像与誓约透传**：
   - `catalog/catalog.go`：映射 `boss_trial_kind 12/13/14` → 卷四 `401/402/403`；
   - `battle_replay_policy.go` + `gather.go`：对试炼志 PVE 开启受控录制，并透传与校验 `trial_vow_mask`；
   - `authority/trial_challenge_card.go` + `schema.go` + `role_fence_purge.go` + `logic/trialchallengecardlogic.go` + `matchserviceserver.go`：实现 `CreateTrialChallengeCard` / `GetTrialChallengeCard` / `GetSharedTrialReplay`（含 10 帖配额、`ref_count` 锚定与删角释放）；
   - 跑通 `go test` 全量相关测试。
4. **阶段 D · Unity 客户端逻辑、uGUI 界面与单测（`mmorpg-client`）**：
   - `BattleArtCatalog.cs`：怪物 `111..116` 映射至宝宝库高清立绘；
   - `TrialJournalModels.cs` / `TrialJournalUiState.cs` / `BattleMatchEntryClient.cs` / `BattleReportClient.cs`：支持卷四跳转（自动判断所在野外图/跨图直达）、4 条试炼誓约透传、约战帖签发/解析/共享录像拉取与破友纪录判定；
   - `TrialJournalWindow.cs` / `TrialJournalUiRoot.cs` / `TrialJournalBattleResultHint.cs` / `CityTravelUiRoot.cs` / `SocialWindow`：接入卷四页签、誓约勾选条、「发帖约战 / 看录像」按钮、野外三图明雷首领入口与聊天内嵌约战帖卡片（全部新切片优先加载 `UI/Ugui/TrialJournalV3/` 缺图安全回落，Codex 产图导入后零改代码生效）；
   - 补齐 EditMode 单测并通过全仓编译与实跑验证。
5. **阶段 E · 跨服对账登记**：
   - 更新 `xuanming-server-mmo/PARITY.md` 登记 V3 增量契约（待后续按需同步 Java / Python 版）。

---

## 6. 执行步骤

1. 落盘本策划案与 [ui-handoff/trial-journal-v3-ui-handoff.md](ui-handoff/trial-journal-v3-ui-handoff.md)，运行 `python tools/scripts/gen_docs_index.py`。
2. 编写一次性配表脚本更新 6 张权威 Excel 表并改好 7 个 `.proto` 文件，执行服务端导表 + `proto-gen` + 客户端 `sync_tables / gen_proto / gen_messageids`。
3. 落地 C++ `battle` & `scene` 实现，跑 `test_cpp_no_raw_pointer_member.py`、串行 MSBuild 编译、`turn_battle_engine_test`、`bag_test` 与 `scene.exe --validate-tables`。
4. 落地 Go `match` 誓约校验、受控 PVE 录像与约战帖服务，跑 `go test ./internal/catalog/... ./internal/authority/... ./internal/logic/...` 与 `go build ./...`。
5. 落地 Unity 客户端模型、网络调用与 uGUI 全入口接线，补齐 `TrialJournalClientTests` 单测并跑通 `client_compile_check.ps1` 与离线 NUnit 实跑。
6. 更新 `PARITY.md`，在回复中同时给出可整段复制发给异机 Codex 的纯功能作图清单。
