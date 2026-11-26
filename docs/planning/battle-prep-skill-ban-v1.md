# 《五行奇谈》战前技能禁选（每方 3 Ban · 方案 A）玩法与全端工程设计（V1）

> 日期：2026-11-26  
> 状态：**设计草案（待负责人审阅拍板；拍板前不落任何产品代码）**  
> 上位红线：[项目设定](project-vision.md) · [全局玩法统一收口总账](gameplay-unified-design-v1.md) · [自由技能体系总览](skill-planning/README.md)  
> 依赖前置：[论剑 V2 体验重做](ranked-play-v2-overhaul.md) · [论剑 V3 战前 15 秒明牌变阵](ranked-play-v3-tactical-prep.md) · [群仙问道赛 V4](season-team-tournament-v4.md) · [五行玩法三大扩展](wuxing-gameplay-expansions-v1.md)  
> 配套 UI 交接单：[battle-prep-skill-ban-ui-handoff.md](ui-handoff/battle-prep-skill-ban-ui-handoff.md)（纯功能清单，不含美术风格）  
> 适用仓库：`xuanming-server-mmo-cpp`（C++ `battle` / Go `match` 权威源）、`xuanming-server-mmo`（Java 独立版对账）、`mmorpg-client`（Unity 客户端逻辑与 UGUI）

---

## 1. 需求分析

### 1.1 意图与为什么做
《五行奇谈》采用**无职业锁死的 62 项常驻自由技能 + 当季免费赛季技能**，人物全部已学主动技能均可在局内随时释放，策略博弈高度依赖见招拆招。随着《人宠会武》高段位环境与《群仙问道赛》跨区战队赛成型，亟需一套类似《英雄联盟（LoL）》的**战前技能禁用（Ban）机制**：

1. **给高阶排位与战队锦标赛提供玩家自发平衡阀**：遇到当期环境偏强或己方阵容极度讨厌的毒瘤/强干扰招式（如群体冰封、复活印、群盾、玄镜复制等），双方可在开战前各自封禁 **3 个技能**，逼出第二套打法与多流派配装价值。
2. **拉开《群仙问道赛》BO3 赛事策略深度与观战看点**：配合 V4 刚落地的“BO3 赛前禁五行灵脉”，每小局进房后双方再各禁 3 招（G2/G3 可针对上一局对手掏出的绝活临场重 Ban），极大提升 L2 全服直播与《名局录》回放的观赏性。
3. **不拖慢快节奏回合体验、不伤害新手零氪**：绝不引入 1～2 分钟轮流读秒 BP 大厅，直接并入既有战前明牌变阵窗；严格豁免基础输出与被动，避免单系配装玩家被 Ban 成“只能平 A 的木桩”。

### 1.2 目标、非目标与反目标
- **目标（Goals）**：
  - 统一服务 **《群仙问道赛》（海选/淘汰/决赛/热身）**、**高段位正式《人宠会武》排位** 与 **《道友切磋坊》自定义训练房** 三大竞技场景。
  - 每方拥有 **3 个独立 Ban 位**（`skill_ban_slots_per_team = 3`），亮牌后去重合并为本局 **0～6 个双边共同禁用技能**。
  - 1V1、3V3、5V5 以及比赛固定队/野排均有清晰、零并发冲突、防挂机浪费的确定性定 Ban 规则。
  - 战斗内全链路拦截（手动提交、自动/宝宝选招、`A41 玄镜映法` 复制、`P09/P19` 镜像反击），且重连、L2 观战、战报回放 100% 确定性还原。
- **非目标（Non-Goals）**：
  - 不做 LoL 式多轮交替读秒 Pick-Ban 选人大厅。
  - 不在战前变阵中开放临场更换人物 8 槽装备、四维加点或被动/追击/开场战法槽（维持 V3 轻量决策边界）。
  - 不做任何与 Ban 位挂钩的付费道具、VIP 特权或额外肝度任务。
  - 不持久化玩家“上次常 Ban 3 招”到后端数据库（纯客户端本地轻量记忆，零 DB 迁移）。
- **反目标（Anti-Goals，出现即视为设计失败）**：
  - 某玩家被 Ban 完后全技能栏灰掉、只能平 A。
  - 3V3/5V5 中 1 名队友挂机导致本队白丢 Ban 位，或全员互相覆盖抢操作。
  - 低段位新手、PVE、首领试炼、娱乐预设局、帮战被平白拖慢节奏。
  - 对手或观众在变阵期内通过抓包/推送偷看己方 Ban 意向。

### 1.3 上位红线准入四问自检（[project-vision.md](project-vision.md) §2）
1. **普通零氪玩家**：不花钱、每天只玩 10–30 分钟会不会被卡脖子？**通过**——正式论剑默认仅在「云章（1600）」及以上段位开启（初试/青锋零门槛不受影响）；基础常规输出/治疗与全部被动永久豁免不可被 Ban；禁用采取双边同禁（敌我对称），且所有技能均零付费永久可学。
2. **个人开发者收入**：有没有把付费流向送给工作室？**通过**——纯玩法策略增强，0 可交易产出、0 战力突破；横向逼出多流派与备选灵宠使用率，反而自然提升副套配装槽（`3411`）与永久外观的长期价值。
3. **物价与长期循环**：有没有零成本通胀口子？**通过**——不改动任何货币、材料或奖励发放逻辑。
4. **防骗与资产安全**：有没有站外诱导或毁装风险？**通过**——无交易、无押注、无资产写入，且严禁在战斗快照或场外存档中删除玩家技能。

---

## 2. 方案对比与选型记录

讨论阶段评估了 3 条可行路径（直接淘汰了 1～2 分钟轮流读秒 BP 大厅，因人物主动仅 29 项、战前已锁装备加点，且违背 10 分钟快节奏定位）：

| 方案 | 做法摘要 | 优势 | 劣势 / 取舍 | 选型结果 |
|---|---|---|---|---|
| **方案 A（选定）** | **并入现有战前明牌变阵窗的同步暗 Ban（每方 3 招）** + 客户端本地记住上次 3 Ban | 复用 V3 既有变阵协议/定时器；既支持看对面初阵与首发宠临场拆招，又允许懒人沿用上次 3 Ban 秒锁；统一覆盖锦标赛、高阶论剑与切磋练习 | 同步暗 Ban 偶发双方撞车（最终共禁 3～5 招）；需把开 Ban 局的变阵窗从 15 秒微调至 25 秒 | **负责人拍板采用** |
| 方案 B | 匹配前预选 1～3 个心愿 Ban（零局内操作） | 手机端零操作负担；天然统计全服 Ban 率 | 失去看对面阵容与 BO3 局间针对性拆招的竞技爽感，撑不起锦标赛对抗深度 | 弃用（吸收其“记住上次常用 Ban”优点融入方案 A） |
| 方案 C | 仅在《群仙问道赛》BO3 赛前与切磋坊开启 | 对日常排位零侵入 | 绝大多数普通玩家日常玩不到，覆盖面偏窄 | 弃用（其赛事与切磋场景完整纳入方案 A） |

---

## 3. 设计分析

> **关于决策归属的显式标注**：负责人已拍板采用 **方案 A**、**每方 3 个 Ban 位**、**覆盖比赛（《群仙问道赛》）与日常竞技**；下文标有 `【设计建议默认】` 的细则均可通过配表调整或审阅时直接修正，不作为负责人已承诺项。

### 3.1 开放范围与触发规则（全配置驱动）

| 玩法模式 | 是否开启 3 Ban | 配置依据与冻结方式 | 变阵窗时长 |
|---|---|---|---|
| **《群仙问道赛》**（`MATCH_MODE_SEASON_TOURNAMENT`：海选、淘汰赛、季军战、总决赛、热身约战） | **默认开启（每方 3 Ban）** | `TournamentRule.skill_ban_slots_per_team`（默认 `3`），由 `match` 建房冻结进 `TournamentBattleContext.skill_ban_slots_per_team`；BO3 **每小局独立重 Ban**，与 V4 赛前禁灵脉完全互补 | **25 秒**（全员锁定秒开） |
| **正式《人宠会武》排位**（`MATCH_MODE_PVP_TEAM && season_id != 0 && !use_free_preset`） | **高段位默认开启（每方 3 Ban）**`【设计建议默认】` | `RankedRule.skill_ban_slots_per_team`（默认 `3`）与 `skill_ban_min_rating_centi`（默认 `160000` 即「云章」段位；改 `0` 即全段位开）；成局时当房间内**所有真人最低分 ≥ 门槛**，由 `match` 冻结 `RankedMatchContext.skill_ban_slots_per_team = 3`，否则 `0` | 开 Ban 局 **25 秒**；低段位未开 Ban 局保持原 V3 **15 秒** |
| **《道友切磋坊》**（`MATCH_MODE_PVP_CHALLENGE`） | **房主可选开关（0 或 3 Ban）** | `CustomDuelRules.skill_ban_slots_per_team`（白名单仅允许 `0` 或 `3`；**严禁与 `use_free_preset = true` 同时开启**，防止把 4 技能免费模板 Ban 穿），供战队赛前模拟演练 | 开 Ban 时 **25 秒**；不开时不进变阵 |
| PVE、首领试炼、五行幻境、瑞兽迎春、物法易势/护卫救援（免费预设）、帮战 V2 | **永不开启** | `skill_ban_slots_per_team` 恒为 `0` | 零影响 |

- **战前触发门槛补充**：`TurnBattleEngine::InitTacticalPrep` 中，只要本局 `skillBanSlotsPerTeam > 0 && !banCandidateSkillIds.empty()`，即使参战者只有 1 枚赛季印和 1 只灵宠，也会正常进入 25 秒变阵期（隐藏无选项的印/宠区，聚焦禁招）；若某对局 `skillBanSlotsPerTeam == 0`，100% 沿用既有 V3 静默跳过规则，保证全部历史单测零回归。

### 3.2 可禁技能白名单（`Skill.xlsx` 数据驱动，`ban_eligible = 63`）

在 `data/schema/skill_table.proto` 末尾追加字段 `uint32 ban_eligible = 63;`（`0` = 不可禁，`1` = 允许进入竞技禁选池），**运行时严禁硬编码技能 ID 或策划编号**。引擎在开局筛选本局合法 Ban 候选池的唯一条件：
1. `battle_model == 1` 且 `ability_kind == 1`（**仅事件化主动技能可禁**；所有被动、追击、开场、旧职业技结构上豁免）；
2. `ban_eligible != 0`；
3. `season_id == 0 || season_id == battleActiveSeasonId`（常驻技能 + 当季免费赛季主动技自动跟随换季轮转，旧季技能自动退池）；
4. 若本局 `CustomDuelRules.ban_control_skills == true`，自动过滤已全局禁控的障碍技（`IsControlSkill(*row)`），避免玩家浪费 Ban 位。

#### 首批 `ban_eligible` 配表基线`【设计建议默认】`
- **永久豁免（`ban_eligible = 0`）**：
  - 基础输出与基础续航（保底不哑火）：`10002` 横戈裂岳（A02）、`10006` 紫霄雷阵（A06）、`10010` 青莲润生（A10）、`10015` 太虚澄心（A15）；
  - 宝宝基础攻击/小技能：`10018` 碎甲击（A18）、`11011` 连战之印（B11）、`11012` 摄魂汲血（B12）、`11015` 移灵养元（B15）；
  - 全部被动（15 项 + 成长被动）、追击（3 项）、开场（3 项）。
- **首批入池（`ban_eligible = 1`，共 33 项常驻 + 4 项当季赛季技 = 37 项；客户端按 `action_tags` + `caster_mask` 自动归入 4 页签一屏展示）**：
  - **控场（6）**：`10020` 封灵咒、`10021` 闭灵锁魄、`10022` 禁武缚刃、`10023` 乱心惑神、`10024` 七魄入梦、`10038` 冰封玄狱；
  - **爆发 / 毒火（7 常驻 + 2 赛季）**：`10003` 戈鸣断势、`10008` 九霄归墟、`10039` 天崩裂岳、`10019` 蚀骨毒、`10029` 蔓毒术、`10030` 焚心炎、`10031` 毒火交攻、`15001` 破阵裂锋、`15002` 破阵灵雷（S2 同理配表）；
  - **辅助 / 护盾 / 绝学（12 常驻 + 2 赛季）**：`10012` 丹心返照、`10014` 灵鼎护元、`10016` 青羽催行、`10025` 战意昂扬、`10026` 生生不息、`10027` 幻影迷踪、`10033` 清心荡秽、`10034` 驱灵散势、`10035` 断脉咒、`10036` 同袍共济、`10040` 太清灵盾、`10041` 玄镜映法、`15003` 固阵灵障、`15004` 破阵蓄势；
  - **灵宠专属策略（8）**`【设计建议默认】`：`11005` 如意灵环、`11006` 神龙护体、`11007` 乾坤反震、`11008` 五色返照、`11009` 游说退敌、`11013` 舍命破灵、`11014` 法力护盾、`11016` 舍身救主。
- **人宠共用技能口径**：`10019 蚀骨毒`、`10026 生生不息`、`10033 清心荡秽`、`10034 驱灵散势`、`10014 灵鼎护元`、`10016 青羽催行`、`10025 战意昂扬`、`10027 幻影迷踪` 均以唯一 `SkillTable.id` 标识，**一旦被禁，敌我双方的人物与灵宠同步不可施放**，零歧义。

### 3.3 1V1 / 3V3 / 5V5 队内 3 Ban 确定性聚合规则

为同时满足 **1V1 个人全权**、**3V3 比赛固定队协同**、**3V3/5V5 野排防单人挂机浪费 Ban 位** 且做到 **零并发抢锁冲突**：

1. **个人提名独立隔离**：每名非 AI 真人玩家在变阵未锁定前，均可自由维护自己的有序禁用提名列表 `nominated_ban_skill_ids`（长度 `0..3`，元素去重、均属于 `ban_candidate_skill_ids`；**第 1 顺位 = 最想禁的技能**）。
2. **同队实时聚合预览（对手与观众绝对不可见）**：每当任一队友修改提名，引擎以无状态纯函数 `AggregateTeamBans(teamIndex)` 实时计算本队当前生效的 Top `skillBanSlotsPerTeam`（至多 3 个），并经 `NotifyBattlePrepUpdate` 单播给同队全员：
   - 对本队所有被至少 1 名队友提名过的候选技能 `s`，统计三项确定性指标：
     1. `votes(s)`：提名了 `s` 的本队不同玩家数（**降序，共识优先**）；
     2. `bestSlotRank(s)`：提名了 `s` 的队友中，`s` 处于其个人列表的最小下标 `0 / 1 / 2`（**升序，优先保住每名队友的 #1 顺位禁招**）；
     3. `minMemberOrder(s)`：提名了 `s` 的队友中，在 `CreateBattleRequest.players` 同队序列中的最小顺位（**升序，禁止按 `player_id` 大小比较以严守永久 ID 不变量**；锦标赛发包天然将队长排在队内第 0 位）；
     4. 最终兜底：`skill_id` 升序。
   - 取前 `min(candidates.size(), skillBanSlotsPerTeam)` 项作为该队 `team_effective_ban_skill_ids`（展示按生效优先级或升序，最终汇总去重升序）。
3. **验证典型场景是否完全直觉**：
   - **1V1**：本人选 `[A, B, C]` → 100% 生效 `[A, B, C]`。
   - **3V3 比赛开麦**：三人各选 1 个不同招式（或队长选 3 招、队友不填）→ 100% 精准生效该 3 招。
   - **3V3 三人各填满 3 招且无共识**：三人的第 1 顺位（`bestSlotRank = 0`）各占 1 席 → 等价于每人各决定 1 个 Ban 位！
   - **3V3 野排 1 人挂机**：剩下 2 人的第 1、2 顺位自动补满全部 3 个 Ban 位，零浪费。
   - **5V5**：2 票及以上共识必中，其余按顺位补齐 3 招，绝不禁穿技能池。
4. **亮牌合并与双边同禁**：
   - 当全员点击「锁定出战」或 25 秒倒计时到期，服务端执行 `FinalizePrepPhase()`：
     - 冻结 `team_a_banned_skill_ids`（0..3）与 `team_b_banned_skill_ids`（0..3）；
     - 合并去重升序写入 `banned_skill_ids`（0..6）；
     - 若某方未选满 3 招或未选任何招，**按实际选中的 0..2 招结算，绝不随机代 Ban、绝不判逃跑**；
     - 同步广播开战首帧（`prep_state.active = false`，带 `banned_skill_ids` / `team_a_banned_skill_ids` / `team_b_banned_skill_ids`），随后初始化回放起点与第 1 回合 25 秒行动窗。

### 3.4 战斗引擎全链路拦截规则（防绕过闭环）

| 拦截点 | C++ 位置（Java `game-battle-engine` 逐条同构） | 行为口径 |
|---|---|---|
| **1. 手动施放校验** | `TurnBattleEngine::ValidateAction`（紧邻 `IsControlBannedByCustomDuel` 之后） | `IsSkillBanned(action.skill_table_id())` 命中 → 拒绝提交，返回新 Tip 码 `kSkillBannedInBattle`（“该技能本局已被禁用”） |
| **2. 结算期复核** | `TurnBattleEngine::ExecuteAction` / `CheckConfiguredSkill` | 二次纵深复核 `IsSkillBanned`，防止任何非标准路径穿透 |
| **3. 挂机与宝宝自动选招** | `SelectAutoConfiguredSkill` / `SelectAutoClassSkill` | 跳过全部 `IsSkillBanned(skillId)` 技能；若无其他可用主动技则平滑回落普攻，永不卡回合 |
| **4. `A41 玄镜映法` 复制** | `TurnBattleEngine::IsSkillCopyableFor`（`configured_battle_effects.cpp`） | 命中 `IsSkillBanned(row.id())` 直接返回 `false`——彻底封死“用 A41 随机抽敌方已学技能池把被禁技能放出来”的漏洞 |
| **5. `P09 / P19` 镜像反击** | `configured_battle_effects.cpp` 镜像触发段 | 纵深拦截 `IsSkillBanned(incoming->id())` |
| **6. 快照零污染** | `actors` 与结算回写 | 严禁从 `actor.skill_table_ids` 删除被禁技能，也不回写任何场外存档；客户端凭 `BattleStateS2C.banned_skill_ids` 原地置灰并叠加「已禁」标记 |

### 3.5 协议、配表与错误码增量（零新 RPC、零新消息号、零新 DB 表）

1. **配置表增量（只末尾追加列）**：
   - `data/schema/skill_table.proto` & `data/Skill.xlsx`：追加 `uint32 ban_eligible = 63;`；同步纳入 `BattleTableFingerprint` 计算，确保跨节点指纹强一致。
   - `data/schema/rankedrule_table.proto` & `data/RankedRule.xlsx`：`rule_version` 递增（`2 -> 3`），追加 `uint32 skill_ban_slots_per_team = 24;`（首测 `3`）、`uint32 skill_ban_min_rating_centi = 25;`（首测 `160000`）。
   - `data/schema/tournamentrule_table.proto` & `data/TournamentRule.xlsx`：`rule_version` 递增（`4 -> 5`），追加 `uint32 skill_ban_slots_per_team = 32;`（首测 `3`）。
   - `data/tip/Tip.xlsx`（`//skill_error` 末尾）：追加 `kSkillBannedInBattle`（由导表器单轴发号，严禁手写数字）。
2. **协议增量（只末尾追加字段，不增删改已有字段号与 RPC 序号）**：
   - `proto/battle/battle_data.proto`：
     - `RankedMatchContext` 追加：`uint32 skill_ban_slots_per_team = 10;`
     - `TournamentBattleContext` 追加：`uint32 skill_ban_slots_per_team = 16;`
     - `CustomDuelRules` 追加：`uint32 skill_ban_slots_per_team = 8;`
     - `BattlePrepPlayerState` 追加：`repeated uint32 nominated_ban_skill_ids = 8;`
     - `BattlePrepState` 追加：`uint32 skill_ban_slots_per_team = 7;`、`repeated uint32 ban_candidate_skill_ids = 8;`、`repeated uint32 team_effective_ban_skill_ids = 9;`
   - `proto/battle/player_battle.proto`：
     - `BattleStateS2C` 追加：`repeated uint32 banned_skill_ids = 36;`、`repeated uint32 team_a_banned_skill_ids = 37;`、`repeated uint32 team_b_banned_skill_ids = 38;`
     - `UpdateBattlePrepRequest` 追加：`bool update_banned_skills = 5;`、`repeated uint32 banned_skill_ids = 6;`
       > **关键语义**：`update_banned_skills == false` 时保持本人既有禁用提名不变（完全兼容旧客户端及单独切换赛季印/首发宠/锁定出战的请求）；`update_banned_skills == true` 时用 `banned_skill_ids`（允许空列表清空）原子替换本人提名。
3. **客户端本地轻量记忆（零后端存储）**：
   - Unity 客户端在本地偏好层按 `(player_id, "prep_skill_bans")` 记住最近一次确认使用的至多 3 个禁招 ID；
   - 仅在进入开 Ban 变阵期、本人当前尚未提名且未锁定时，按本局服务端下发 `prep_state.ban_candidate_skill_ids` **过滤有效交集**后自动发起一次预勾请求（**绝不自动点击「锁定出战」**），若上次记住的某招已换季退池则静默剔除，永不发送非法 ID；
   - 满足 WebGL2 小游戏与 iOS/Android App 双形态约束（同步读取，本地缓存丢失仅退化为手动勾选，零资金/状态一致性风险）。

---

## 4. 思想分析

1. **为什么坚决复用 V3 `BattlePrepState` 与 `UpdateBattlePrep`，而不另起一套 LoL 选人房微服务/状态机？**
   - 遵循 KISS 与零额外状态机阻塞原则：战斗房间创建与直连握手已经稳定运行在 `BattleRoomManager` 单 EventLoop 定时器上；复用同一个变阵状态机与 `RedactStateForViewer` 脱敏闸，避免引入“选人房掉线→重新匹配→取消备战冻结”的分布式补偿地狱，且 BO3 每小局都能天然重 Ban。
2. **为什么选“同队个人提名 + 确定性票数/顺位聚合”，而不是“队长独占 3 槽”或“队内共享可抢写数组”？**
   - 队长独占在野排 3V3 会被挂机 1 号位坑死；共享数组在 3 人同时点击时会互相覆盖竞态；“每人维护私有名单 + 引擎纯函数确定性聚合”天然具备 Actor 隔离与无锁并发美感，同时在 1V1、3V3 比赛、3V3 野排、5V5 四种形态下全部直观好用。
3. **为什么只放行非基础主动技进白名单、豁免基础输出与全部被动？**
   - 《五行奇谈》的配装（词条/加点/被动槽）在进房前已锁定，这与 LoL“先 Ban 再选英雄配符文”存在结构性差异。白名单 + 豁免基础输出守住了“被 Ban 3～6 招后只是拆掉对方的绝活套路、逼出变招，而绝不会让人变成无招可放的木桩”的体验底线。
4. **为什么将“记住上次 3 Ban”放在客户端本地而不是服务端存档组件？**
   - 偏好预填属于纯 UX 便利项，不影响权威裁决；放本地可避免新增持久化组件字段、避免 Java `PlayerStateSchemaLedger` 升代迁移与 `go/db` 写放大，成本最低、风险为零。

---

## 5. 漏洞分析与权威防线

| 潜在漏洞 / 边界 | 风险后果 | 权威硬防线 |
|---|---|---|
| **伪造请求 Ban 掉基础输出（A02/A06）、被动或旧季/不存在技能** | 把全物理/全法系对手 Ban 成木桩，或使引擎崩溃 | 服务端只认开局从 `BattleDataProvider::GetBanEligibleSkillIds(activeSeasonId)` 冻结的白名单集合；越界、重复或超 3 个一律拒回 `kInvalidParameter` |
| **对手或观众在 25 秒变阵期偷看己方 Ban 意向** | 针对性反改赛季印/首发宠或偷窥战队战术 | `PopulatePrepStateForViewer` 与 `RedactStateForViewer` 仅向同队投影 `team_states` 与 `team_effective_ban_skill_ids`；`NotifyBattlePrepUpdate` 仅同队单播；对手与观众视图恒清空 |
| **用 `A41 玄镜映法` 复制敌方已学技能池绕过禁令** | 本局已禁 `A38 冰封玄狱`，却借 A41 随机抽敌方技能库把冰封放出来 | `TurnBattleEngine::IsSkillCopyableFor` 直接拒绝对任何 `IsSkillBanned` 技能的复制（无论主动命中还是方案 A 随机兜底）；`P09/P19` 同步纵深拦截 |
| **自动战斗 / 宝宝自动出手卡死或违规放出被禁技能** | 玩家挂机或宝宝自动选到被禁技能导致结算异常或白嫖施放 | `SelectAutoConfiguredSkill` 与 `SelectAutoClassSkill` 均跳过 `IsSkillBanned`；全主动受限时自动回落普攻 |
| **道友切磋坊同时开「公平免费 4 技能预设 + 3 Ban」** | 4 技能模板被禁掉 3 个甚至 4 个，直接无技能可放 | Go/Java `validateCustomDuelRules` 与引擎 `Initialize` 双层校验：禁止 `use_free_preset == true && skill_ban_slots_per_team > 0`（拒回 `kInvalidParameter`） |
| **客户端点击换印/换宠/锁定时误把已选 3 Ban 清空** | 选完 3 个 Ban 再点一下换赛季印，Ban 列表全丢 | `UpdateBattlePrepRequest` 显式引入 `bool update_banned_skills = 5` 区分“不改 Ban”与“替换/清空 Ban”；锁定后同样禁止再修改 `banned_skill_ids` |
| **换季后客户端本地记住的旧赛季技触发报错弹窗** | S2 进房自动预填了 S1 的 `15001` 导致服务端拒绝 | 客户端自动预填前严格与服务端下发的 `prep_state.ban_candidate_skill_ids` 求交集，剔除失效 ID；仅在交集非空时发请求 |
| **按 `player_id` 大小做同票决胜导致不可预测偏置** | 永久 ID 无序打散，按 ID 大小决胜违反系统不变量 | 同票严格按 `votes DESC -> bestSlotRank ASC -> minMemberOrderInTeam ASC -> skillId ASC`，全程零依赖 `player_id` 大小与节点时钟 |
| **双服（C++/Go vs Java）或节点间表指纹漂移** | A 节点允许 Ban 某技能而 B 节点认为不可 Ban | `ban_eligible` 纳入 `BattleTableFingerprint`；双服共用 `ContractSync` 契约与同一份确定性聚合算法并配备同口径边界单测 |
| **弱网连点 / 切号导致旧房间预填请求串台** | 切号或开新局后旧回调改坏新局状态 | 复用 `BattleClient` `(battleId, connectionIdentity, generation)` 守护与 `RpcGuard` 限流；`BattleScreen` 仅在 `_state.BattleId == battleId` 时处理回包 |

---

## 6. 留给实现者自行决定的细节（Scope 边界）
以下细节不影响核心玩法与跨端契约，实现者按各仓惯用法直接定夺即可：
- C++ `TurnBattleEngine` 内部 `PrepPlayerOptions` / `bannedSkillIds` 具体容器选型（如 `std::vector<uint32_t>` vs `std::set<uint32_t>`，保证无裸指针成员即可）；
- 战前变阵弹窗在 `1920×1080` 与 `2400×1080` 下的具体像素坐标微调（只需遵守不遮挡敌方初阵、同队锁定条与右上角 140px 小游戏胶囊避让区）；
- 战斗开局亮牌横幅的入场/停留动画时长（建议 2.5～3.0 秒自动收起为顶部常驻小条，随时可点击查看完整双边 3 Ban 技能详情，不依赖鼠标悬停）。

---

## 7. 步骤分析与执行步骤（待设计审阅通过后，按此进入实施计划）

### 阶段 1 · 配表、Tip 与协议契约（权威 C++/Go 仓）
1. 更新 `data/schema/skill_table.proto`（`ban_eligible = 63`）、`data/schema/rankedrule_table.proto`（`24..25`）、`data/schema/tournamentrule_table.proto`（`32`）。
2. 更新源表 `data/Skill.xlsx`（首批 33 常驻 + S1/S2 赛季主动技置 `ban_eligible = 1`）、`data/RankedRule.xlsx`（`rule_version = 3, skill_ban_slots_per_team = 3, skill_ban_min_rating_centi = 160000`）、`data/TournamentRule.xlsx`（`rule_version = 5, skill_ban_slots_per_team = 3`）、`data/tip/Tip.xlsx`（`kSkillBannedInBattle`）。
3. 更新 `proto/battle/battle_data.proto` 与 `proto/battle/player_battle.proto`（只追加字段，零新消息号）。
4. 交由 Gemini 执行 `dev.bat gen`、`gen_schema_index.py --check`、Java `ContractSync` 与 Unity `gen.bat client`。

### 阶段 2 · C++ `battle` 与 Go `match` 权威服务端
1. **`cpp/libs/services/battle`**：
   - `turn_battle_constants.h`：新增 `kPrepSkillBanSlotsPerTeam = 3`、`kTacticalPrepWithBanDurationSeconds = 25`、`kTacticalPrepWithBanTimeoutMs = 25000`。
   - `battle_data_provider.h` & `table_battle_data_provider.{h,cpp}`：新增 `GetBanEligibleSkillIds(uint32_t activeSeasonId)`，`BattleTableFingerprint` 纳入 `ban_eligible`。
   - `turn_battle_engine.{h,cpp}` & `configured_battle_effects.cpp`：实现 `InitTacticalPrep`（支持锦标赛/高阶论剑/切磋开 Ban、25s 时长）、`UpdateBattlePrep`（校验并更新个人提名）、`AggregateTeamBans`（确定性聚合）、`FinalizePrepPhase`（冻结双边与全局禁招）、`PopulatePrepStateForViewer`（严格同队脱敏）、`BuildStateSnapshot`（透传终态禁招）、以及 5 处战斗内全链路拦截（手动/复核/自动/A41 复制/P09&P19 镜像）。
2. **`cpp/nodes/battle`**：
   - `battle_room_manager.cpp`：`ArmPrepTimer` 按 `room.engine.PrepTimeoutMs()` 动态装填 15s/25s；`HandleUpdateBattlePrep` 透传 `update_banned_skills` 与 `banned_skill_ids`；复核 `RedactStateForViewer` 与回放录制起点自动带入禁招。
3. **`go/match`**：
   - `internal/authority/ranked.go` / `internal/logic/global_match.go`：读取 `RankedRule` 新列，按房间全员最低分 ≥ `skill_ban_min_rating_centi` 且 `!use_free_preset` 冻结 `RankedMatchContext.skill_ban_slots_per_team`。
   - `internal/tournament/catalog.go` & `internal/logic/tournamentlogic.go`：冻结 `TournamentBattleContext.skill_ban_slots_per_team`。
   - `internal/logic/challengelogic.go`：`validateCustomDuelRules` 放行 `skill_ban_slots_per_team ∈ {0, 3}` 并禁止与 `use_free_preset = true` 同开。
4. **单测**：补齐 C++ `ranked_battle_test.cpp`（1V1 3 Ban、3V3 个人提名/共识/顺位/挂机补位、5V5、撞车去重、A41 禁复制、自动战斗回落普攻、重连/观众脱敏、切磋互斥）与 Go `match` 规则单测。

### 阶段 3 · Java 独立服务端双版对齐（`xuanming-server-mmo`）
1. 同步最新契约与配表（`ContractSync --check` 通过）。
2. 在 `game-battle-engine`（`BattleConstants`、`BattleData`、`TableBattleData`、`BattleTableFingerprint`、`ActionChecks`、`AutoClassSkillSelector`、`ConfiguredEffects`、`TurnBattleEngine`）、`game-battle`（`BattleViews`、`BattleRoomServiceImpl`）与 `game-match`（`RankedRules`/`RankedStore`、`TournamentTables`/`TournamentService`、`Challenge` 校验）逐条对齐实现。
3. 补齐同口径单元测试并登记 `PARITY.md`。

### 阶段 4 · Unity 客户端逻辑、UGUI 与异机 Codex 美术交接（`mmorpg-client`）
1. **逻辑层**：扩展 `BattleClient.cs`（`UpdateBattlePrep` 支持 `updateBannedSkills` + `bannedSkillIds`）、新增纯 C# 无 `UnityEngine` 依赖的 `BattleSkillBanPreferenceStore`（本地记忆与白名单交集过滤）与候选技能 4 分类分组器。
2. **UGUI 层**：
   - `BattleScreen.cs`：在现有战前变阵浮层接入「禁招（0/3）」区（3 个本队生效槽 + 4 分类白名单技能格 + 队友提名标记），接入局内顶部双方禁招亮牌条，在人物与宝宝技能按钮上叠加「已禁」置灰禁用态并拦截点击（回弹提示“该技能本局已被禁用”）；缺新切图时优雅降级至既有控件底板与技能图标。
   - `CustomDuelChallengePanel.cs`：新增「战前各禁 3 招」勾选开关（勾选免费预设时自动置灰互斥并给提示）。
   - `BattleReportPanel.cs` / 观战 HUD：展示本局双方 3 Ban 详情。
3. **美术切片交接**：将 [battle-prep-skill-ban-ui-handoff.md](ui-handoff/battle-prep-skill-ban-ui-handoff.md) 的纯功能清单交付异机 Codex 制作 8 枚透明 PNG-32 切片并导入 `Assets/Resources/UI/Ugui/BattlePrepSkillBan/`。
4. **验证门禁（由 Gemini 执行）**：C++ 静态守卫（无裸指针/英文日志/线程源）+ MSBuild `/m:1` + GTest、Go `go test ./...`、Java `mvnw clean install` + 定向单测、Unity `client_compile_check.ps1` + EditMode NUnit 单测，并追加实跑证据至 `docs/PROGRESS.md`。
