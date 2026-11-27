# 《五行奇谈》战前技能禁选（每方 3 Ban · 方案 A）玩法与全端工程设计（V1）

> 日期：2026-11-26  
> 状态：**设计完成（经独立实现视角复核收敛，待负责人最终确认后进入实施计划）**  
> 上位红线：[项目设定](project-vision.md) · [全局玩法统一收口总账](gameplay-unified-design-v1.md) · [自由技能体系总览](skill-planning/README.md)  
> 依赖前置：[论剑 V2 体验重做](ranked-play-v2-overhaul.md) · [论剑 V3 战前 15 秒明牌变阵](ranked-play-v3-tactical-prep.md) · [群仙问道赛 V4](season-team-tournament-v4.md) · [五行玩法三大扩展](wuxing-gameplay-expansions-v1.md)  
> 配套 UI 交接单：[battle-prep-skill-ban-ui-handoff.md](ui-handoff/battle-prep-skill-ban-ui-handoff.md)（纯功能清单，不含美术风格）  
> 适用仓库：`xuanming-server-mmo-cpp`（C++ `battle` / Go `match` 权威源）、`xuanming-server-mmo`（Java 独立版对账）、`mmorpg-client`（Unity 客户端逻辑与 UGUI）

---

## 1. 需求分析

### 1.1 意图与为什么做
《五行奇谈》采用无职业锁死的自由技能体系（现役含 59 项常驻事件化主动技 + 4 项当季免费赛季主动技 + 被动/追击/开场），人物全部已学主动技能均可在局内随时释放。随着《人宠会武》高段位环境与《群仙问道赛》跨区战队赛成型，引入类似《英雄联盟（LoL）》的**战前技能禁选（Ban）机制**：

1. **给高阶排位与战队锦标赛提供玩家自发平衡阀**：遇到当期环境偏强或己方阵容极度讨厌的核心招式（如群控、定向控场、蓄力爆发、复活印、群盾、玄镜复制等），双方开战前各自封禁 **3 个技能**，逼出第二套打法与横向流派价值。
2. **拉满《群仙问道赛》BO3 赛事策略深度与观战看点**：与 V4 刚落地的“BO3 赛前禁五行灵脉”形成两层互补——进房前禁灵脉定战场大环境，进房后每小局 25 秒内双方各禁 3 招（BO3 的 G2/G3 可针对上一局对手绝活临场重 Ban），大幅提升 L2 全服观战与《名局录》回放的观赏性。
3. **守住快节奏与零氪新手底线**：绝不做 1～2 分钟轮流读秒 BP 大厅，直接并入既有战前明牌变阵窗；豁免基础输出、基础治疗与全部被动，避免单系配装玩家被 Ban 成“只能平 A 的木桩”。

### 1.2 目标、非目标与反目标
- **目标（Goals）**：
  - 统一覆盖 **《群仙问道赛》（海选 / 淘汰 / 季军战 / 总决赛 / 3V3 热身约战）**、**云章（1600）及以上正式《人宠会武》排位** 与 **《道友切磋坊》1V1 自定义禁招局**。
  - 每方拥有 **3 个 Ban 位**（`skill_ban_slots_per_team = 3`），同步暗 Ban，亮牌后去重合并为本局 **0～6 个双边共同禁用技能**。
  - 全量覆盖现役 51 项非基础常驻主动（含 4/5 号卷宗 `S01–S14`、`E01–E02`、`SP01–SP02`）+ 4 项当季赛季主动技，共 **55 项**，采用 4 页签可滚动网格，未来新卷宗主动技改表即入池、免改 UI。
  - 1V1、3V3、5V5 以及固定战队/野排均采用同一套零并发抢锁、防挂机浪费、不依赖 `player_id` 大小的确定性聚合规则。
  - 战斗内全链路拦截（手动提交、结算复核、挂机/宝宝自动选招、`A41 玄镜映法` 复制、`P09/P19` 镜像反击），且重连、L2 全服观战、战报回放与「看配装」详情 100% 确定性还原。
- **非目标（Non-Goals）**：
  - 不做 LoL 式多轮交替读秒选人大厅。
  - 不扩展多人（3V3/5V5）《道友切磋坊》——3V3 战队赛前模拟演练统一走既有锦标赛「热身约战（`StartTournamentWarmup`）」，天然携带完整 3V3 + 3 Ban + 换印/换宠。
  - 不在 1V1 《道友切磋坊》中额外打包换赛季印/换首发宠（保持零 `scene` 改动，仅跑 25 秒纯 3 Ban）。
  - 不在 `ListBattleReports` 战报列表摘要行展示禁招（仅在「看配装」详情、战报回放与观战 HUD 展示，保持零 Kafka / 零 `BattleReportSummary` / 零 DB 变更）。
  - 不做任何与 Ban 位挂钩的付费道具、VIP 特权或后端数据库存储偏好。
- **反目标（Anti-Goals）**：
  - 某玩家被 Ban 完后全技能栏灰掉、只能平 A。
  - 3V3/5V5 中 1 名队友挂机导致本队白丢 Ban 位，或多人操作互相覆盖。
  - 季中上线因 `rule_version` 校验不匹配导致存量活跃赛季无法排位。
  - 对手或观众在变阵期内通过抓包或推送偷看己方 Ban 意向。

### 1.3 上位红线准入四问自检（[project-vision.md](project-vision.md) §2）
1. **普通零氪玩家**：正式论剑默认在「云章（1600）」及以上开启，保护初试/青锋新手；基础群攻/群疗/回蓝与全部被动永久豁免；禁用双边对称且全部技能零付费永久可学。**通过**。
2. **个人开发者收入**：纯玩法策略扩展，零可交易产出、零战力突破；横向盘活多套流派与副套配装槽（`3411`）需求。**通过**。
3. **物价与长期循环**：零货币、零材料、零奖励改动。**通过**。
4. **防骗与资产安全**：无交易、无押注、无资产写入，且绝不删改快照或场外技能存档。**通过**。

---

## 2. 方案对比与选型记录

讨论阶段直接排除了 1～2 分钟轮流读秒 BP 大厅（拖慢 10 分钟快节奏单局，且战前已锁装备加点），重点对比 3 条落地路线：

| 方案 | 做法摘要 | 优势 | 劣势 / 取舍 | 选型结果 |
|---|---|---|---|---|
| **方案 A（选定）** | **并入现有战前明牌变阵窗的同步暗 Ban（每方 3 招）** + 客户端本地记住上次 3 Ban | 复用 V3 既有变阵协议与房间定时器；看对面初阵/首发宠临场拆招与懒人一键沿用兼得；统一覆盖比赛、高阶论剑与切磋 | 同步暗 Ban 偶发双方撞车（最终共禁 3～5 招）；开 Ban 局变阵窗从 15 秒微调至 25 秒 | **负责人拍板采用** |
| 方案 B | 匹配前预选心愿 Ban（零局内操作） | 手机端零操作负担；天然统计全服 Ban 率 | 缺少看对面阵容与 BO3 局间针对性拆招的竞技爽感，撑不起锦标赛对抗 | 弃用（吸收“本地记住上次 3 Ban”融入方案 A） |
| 方案 C | 仅在《群仙问道赛》BO3 赛前与切磋坊开启 | 对日常排位零打扰 | 普通玩家日常论剑玩不到，覆盖面过窄 | 弃用（赛事与切磋完整纳入方案 A） |

---

## 3. 设计分析

> **决策归属标注**：负责人已明确确认 **方案 A、每方 3 Ban、覆盖比赛与高阶论剑、55 项全量非基础主动技入池（含 4/5 号卷宗 18 项与灵宠专属策略 10 项，4 页签可滚动网格）、日常论剑默认云章（1600）起开**。以下标 `【工程收敛定夺】` 的项目为经独立实现视角复核后确定的无歧义工程边界。

### 3.1 开放范围与触发规则（全配置驱动）

| 玩法模式 | 是否开启 3 Ban | 配置依据与冻结方式 | 变阵窗内容与时长 |
|---|---|---|---|
| **《群仙问道赛》**（`MATCH_MODE_SEASON_TOURNAMENT`：海选 / 淘汰 / 季军战 / 总决赛 / 3V3 热身约战） | **默认开启（每方 3 Ban）** | `TournamentRule.skill_ban_slots_per_team`（默认 `3`），由 `match` 冻结进 `TournamentBattleContext.skill_ban_slots_per_team`；BO3 **每小局独立重 Ban**，与 V4 赛前禁灵脉互补 | 3 Ban + 换赛季印 + 换首发宠，**25 秒**（全员锁定秒开） |
| **正式《人宠会武》排位**（`MATCH_MODE_PVP_TEAM && season_id != 0 && !use_free_preset`） | **云章（1600）及以上默认开启（每方 3 Ban）** | `RankedRule.skill_ban_slots_per_team`（默认 `3`）与 `skill_ban_min_rating_centi`（默认 `160000`；改 `0` 即全段位开）；成局时房间内**全部真人最低分 ≥ 门槛**则 `match` 冻结 `RankedMatchContext.skill_ban_slots_per_team = 3`，否则 `0` | 开 Ban 局 **25 秒**；低段位未开 Ban 局保持原 V3 **15 秒** |
| **《道友切磋坊》**（`MATCH_MODE_PVP_CHALLENGE`，1V1）`【工程收敛定夺】` | **房主可选开关（0 或 3 Ban）** | `CustomDuelRules.skill_ban_slots_per_team`（白名单仅放行 `0` 或 `3`；**严禁与 `use_free_preset = true` 同时开启**）；3V3 赛前演练统一走锦标赛热身约战 | 仅 1V1 **25 秒纯 3 Ban**（不打包换印/换宠，零 `scene` 改动）；不开时不进变阵 |
| PVE、首领试炼、五行幻境、瑞兽迎春、娱乐轮换预设、帮战 V2 | **永不开启** | `skill_ban_slots_per_team` 恒为 `0` | 零影响 |

- **战前触发门槛**：`TurnBattleEngine::InitTacticalPrep` 在 `skillBanSlotsPerTeam > 0 && !banCandidateSkillIds.empty()` 时必然进入 25 秒变阵期（哪怕参战者只有 1 枚赛季印和 1 只灵宠，或为 1V1 纯禁招切磋）；若 `skillBanSlotsPerTeam == 0`，100% 沿用 V3 静默跳过规则，零历史单测回归。
- **`rule_version` 热兼容策略**`【工程收敛定夺】`：本次未改动 `RankedPreset` 6 套预设与锦标赛赛程，**保持 `RankedRule.rule_version = 2` 与 `TournamentRule.rule_version = 4` 不递增**（与论剑 V3/V4 先例一致），彻底避免存量活跃赛季库中旧 `rule_version` 与目录不一致引发全服 `ErrRankedRuleVersionMismatch` 排位锁死；同时将 `skill_ban_slots_per_team` 与 `skill_ban_min_rating_centi` 纳入 Go / C++ / Java 三端 `ComputeConfigFingerprint()`，确保跨节点表一致性仍受硬指纹保护。

### 3.2 可禁技能白名单与 4 页签分类（`Skill.xlsx` 列 `ban_pool_category = 63`）

`【工程收敛定夺】`核对现役 `Skill.xlsx` 发现 `10029 蔓毒术`、`10035 断脉咒` 的 `action_tags` 均为 `16`（纯障碍位），与控场技字段完全相同，且战斗引擎依赖 `action_tags` 做封物/封法/混乱判定不能轻改。因此直接在 `data/schema/skill_table.proto` 末尾追加**页签分类枚举列 `uint32 ban_pool_category = 63;`**（语义清晰、一列解决“能否禁 + 归哪个页签”两件事，运行时零硬编码技能 ID）：

| `ban_pool_category` 值 | 含义 / UI 页签 | 引擎判定 |
|---:|---|---|
| `0` | 不可禁（永久豁免） | 不入候选池 |
| `1` | **控场障碍** | 入池，归页签 1 |
| `2` | **爆发与毒火** | 入池，归页签 2 |
| `3` | **辅助、护盾与绝学** | 入池，归页签 3 |
| `4` | **灵宠策略** | 入池，归页签 4 |

引擎开局收集本局合法 Ban 候选池的唯一条件：
1. `battle_model == 1` 且 `ability_kind == 1`（结构上排除全部被动、追击、开场与旧职业技能）；
2. `ban_pool_category ∈ {1, 2, 3, 4}`；
3. `season_id == 0 || season_id == battleActiveSeasonId`（常驻技能 + 当季免费赛季主动技自动轮转）；
4. 若本局 `CustomDuelRules.ban_control_skills == true`，自动剔除已全局禁控的障碍技（`IsControlSkill(*row)`），避免浪费 Ban 位。

#### 现役 67 项事件化主动技（59 常驻 + S1/S2 各 4 赛季技）全量落表口径
- **永久豁免（`ban_pool_category = 0`，共 8 项常规主动 + 全部被动/追击/开场）**：
  - 人物/共用保底（4）：`10002` 横戈裂岳、`10006` 紫霄雷阵、`10010` 青莲润生、`10015` 太虚澄心；
  - 灵宠基础小技（4）：`10018` 碎甲击、`11011` 连战之印、`11012` 摄魂汲血、`11015` 移灵养元。
- **页签 1 · 控场障碍（`ban_pool_category = 1`，共 10 项常驻）**：
  - 经典群控（6）：`10020` 封灵咒、`10021` 闭灵锁魄、`10022` 禁武缚刃、`10023` 乱心惑神、`10024` 七魄入梦、`10038` 冰封玄狱；
  - 4 号卷宗定向控场（4）：`10101` 截锋令（S01）、`10102` 遏灵令（S02）、`10103` 断刃锁心（S03）、`10104` 缚咒锁灵（S04）。
- **页签 2 · 爆发与毒火（`ban_pool_category = 2`，共 15 项常驻 + 2 项当季赛季技 = 17 项）**：
  - 群爆与毒火（7）：`10003` 戈鸣断势、`10008` 九霄归墟、`10039` 天崩裂岳、`10019` 蚀骨毒（人宠共用）、`10029` 蔓毒术、`10030` 焚心炎、`10031` 毒火交攻；
  - 4/5 号卷宗定向输出与蓄力大招（8）：`10105` 逐命斩（S05）、`10106` 追魄雷（S06）、`10111` 寻隙破阵（S11）、`10112` 穿隙灵雷（S12）、`10113` 掠影追锋（S13）、`10114` 流光落星（S14）、`10115` 蓄锋破阵（E01）、`10116` 蓄雷天刑（E02）；
  - 赛季攻击技（随季生效 2 项）：S1 `15001` 破阵裂锋、`15002` 破阵灵雷；S2 `15101` 玄脉破军、`15102` 玄脉天罚。
- **页签 3 · 辅助、护盾与绝学（`ban_pool_category = 3`，共 16 项常驻 + 2 项当季赛季技 = 18 项）**：
  - 经典辅盾绝学（12）：`10012` 丹心返照、`10014` 灵鼎护元、`10016` 青羽催行、`10025` 战意昂扬、`10026` 生生不息、`10027` 幻影迷踪、`10033` 清心荡秽、`10034` 驱灵散势、`10035` 断脉咒、`10036` 同袍共济、`10040` 太清灵盾、`10041` 玄镜映法；
  - 4 号卷宗定向疗盾增益（4）：`10107` 扶危甘霖（S07）、`10108` 护命灵障（S08）、`10109` 授锋战印（S09）、`10110` 聚灵法印（S10）；
  - 赛季辅助技（随季生效 2 项）：S1 `15003` 固阵灵障、`15004` 破阵蓄势；S2 `15103` 玄脉灵幕、`15104` 玄脉归元。
- **页签 4 · 灵宠策略（`ban_pool_category = 4`，共 10 项常驻）**：
  - 经典灵宠策略（8）：`11005` 如意灵环、`11006` 神龙护体、`11007` 乾坤反震、`11008` 五色返照、`11009` 游说退敌、`11013` 舍命破灵、`11014` 法力护盾、`11016` 舍身救主；
  - 4 号卷宗灵宠定向技（2）：`11101` 猎血撕咬（SP01）、`11102` 追魂灵弹（SP02）。
- **人宠共用口径**：`10014/10016/10019/10025/10026/10027/10033/10034` 按唯一 `SkillTable.id` 判定，被禁后双方人物与灵宠同步不可施放。

### 3.3 1V1 / 3V3 / 5V5 队内 3 Ban 确定性聚合规则

1. **个人独立提名（零抢锁并发冲突）**：每名非 AI 真人玩家在锁定前，可自由维护本人有序提名列表 `nominated_ban_skill_ids`（`0..3` 项，去重且全部在 `ban_candidate_skill_ids` 内；下标 `0` 为本人最高优先级禁招）。
2. **队长标记独立透传**`【工程收敛定夺】`：不在上游重排 `CreateBattleRequest.players`（避免影响 `formation_slot` 与既有排位组队逻辑），改为在 `BattlePlayerSnapshot` 追加 `bool is_team_captain = 32;`——由 Go / Java `match` 在 `gather` 组装 `CreateBattleRequest` 时为锦标赛两队队长与排位组排队队长置 `true`（单排/路人队全 `false`，零 `scene` 改动）。
3. **同队实时确定性聚合 `AggregateTeamBans(teamIndex)`**`【工程收敛定夺】`：
   - 对本队至少被 1 名非 AI 队友提名过的候选技能 `s`，按以下四级全序排列取前 `min(count, skillBanSlotsPerTeam)` 项：
     1. `votes(s)` **降序**（队内提名人数多者胜，共识优先）；
     2. `bestSlotRank(s)` **升序**（`0 / 1 / 2`，优先保住每名队友的第 1 顺位禁招）；
     3. `nominatedByCaptain(s)` **降序**（本队标记 `is_team_captain == true` 的队长是否提名了 `s`，比赛与开黑同票由队长拍板）；
     4. `SplitMix64(battleSeed ^ uint64(s))` **升序**，终极兜底 `s` 升序（复用引擎既有 `SplitMix64`，**全程零依赖 `player_id` 大小与节点时钟**，既不偏袒小技能 ID，又在 C++ / Java / 变阵实时预览 / 终局亮牌四处逐位一致）。
4. **场景验证**：
   - **1V1**：本人选 `[A, B, C]` → 100% 生效 `[A, B, C]`。
   - **3V3 比赛固定队**：三人各选 1 个不同招（或队长选 3 招、队友不选）→ 100% 精准生效该 3 招；出现同票时队长提名优先胜出。
   - **3V3 野排全员各填 3 招且无共识**：三人的第 1 顺位（`bestSlotRank = 0`）各占 1 席 → 等价于每人各定 1 Ban。
   - **3V3 野排 1 人挂机**：其余 2 人自动补满全部 3 个 Ban 位，零浪费。
   - **5V5**：2 票以上共识优先，其余按顺位与确定性哈希补满 3 招。
5. **亮牌合并与超时保底**：
   - 全员点击「锁定出战」或 25 秒倒计时到期，服务端执行 `FinalizePrepPhase()`：冻结 `team_a_banned_skill_ids`（0..3）与 `team_b_banned_skill_ids`（0..3），合并去重升序生成 `banned_skill_ids`（0..6）。
   - 未选满 3 招或未选任何招时按实际数量结算，**绝不随机代 Ban、绝不判逃跑**；随后广播正式开战首帧、启动回放录制起点与第 1 回合 25 秒行动窗。

### 3.4 战斗引擎全链路拦截规则

| 拦截点 | C++ 位置（Java `game-battle-engine` 逐条同构） | 行为口径 |
|---|---|---|
| **1. 手动施放校验** | `TurnBattleEngine::ValidateAction` | 命中 `IsSkillBanned(action.skill_table_id())` 立即拒绝，返回 `kSkillBannedInBattle`（“该技能本局已被禁用”） |
| **2. 结算期复核** | `TurnBattleEngine::ExecuteAction` / `CheckConfiguredSkill` | 二次纵深复核 `IsSkillBanned` |
| **3. 自动战斗与宝宝自动出手** | `SelectAutoConfiguredSkill` / `SelectAutoClassSkill` | 跳过所有 `IsSkillBanned` 技能；全部主动不可用时安全回落普攻，永不卡回合 |
| **4. `A41 玄镜映法` 复制** | `TurnBattleEngine::IsSkillCopyableFor`（`configured_battle_effects.cpp`） | 命中 `IsSkillBanned(row.id())` 直接返回 `false`，杜绝通过复制目标已学技能池放出被禁技 |
| **5. `P09 / P19` 镜像反击** | `configured_battle_effects.cpp` 镜像分支 | 纵深拦截 `IsSkillBanned(incoming->id())` |
| **6. 快照零污染** | `actors` 与结算回写 | 严禁从 `actor.skill_table_ids` 删除技能，不回写场外存档；客户端凭 `BattleStateS2C.banned_skill_ids` 原地置灰并叠放封禁标记 |

### 3.5 协议、配表与错误码增量（零新 RPC、零新消息号、零新 DB 表）

1. **配置表增量（只末尾追加列）**：
   - `data/schema/skill_table.proto` & `data/Skill.xlsx`：追加 `uint32 ban_pool_category = 63;`（`0..4`），同步纳入 `BattleTableFingerprint`。
   - `data/schema/rankedrule_table.proto` & `data/RankedRule.xlsx`：保持 `rule_version = 2`，追加 `uint32 skill_ban_slots_per_team = 24;`（首测 `3`）、`uint32 skill_ban_min_rating_centi = 25;`（首测 `160000`），两列纳入三端 `ComputeConfigFingerprint()`。
   - `data/schema/tournamentrule_table.proto` & `data/TournamentRule.xlsx`：保持 `rule_version = 4`，追加 `uint32 skill_ban_slots_per_team = 32;`（首测 `3`）。
   - `data/tip/Tip.xlsx`（`//skill_error` 末尾）：追加 `kSkillBannedInBattle`（导表器单轴发号）。
2. **协议增量（只末尾追加字段，零新 RPC / 零新消息号）**：
   - `proto/battle/battle_data.proto`：
     - `BattlePlayerSnapshot` 追加：`bool is_team_captain = 32;`
     - `RankedMatchContext` 追加：`uint32 skill_ban_slots_per_team = 10;`
     - `TournamentBattleContext` 追加：`uint32 skill_ban_slots_per_team = 16;`
     - `CustomDuelRules` 追加：`uint32 skill_ban_slots_per_team = 8;`
     - `BattlePrepPlayerState` 追加：`repeated uint32 nominated_ban_skill_ids = 8;`
     - `BattlePrepState` 追加：`uint32 skill_ban_slots_per_team = 7;`、`repeated uint32 ban_candidate_skill_ids = 8;`、`repeated uint32 team_effective_ban_skill_ids = 9;`
   - `proto/battle/player_battle.proto`：
     - `BattleStateS2C` 追加：`repeated uint32 banned_skill_ids = 39;`、`repeated uint32 team_a_banned_skill_ids = 40;`、`repeated uint32 team_b_banned_skill_ids = 41;`
     - `UpdateBattlePrepRequest` 追加：`bool update_banned_skills = 5;`、`repeated uint32 banned_skill_ids = 6;`（`update_banned_skills = false` 保持既有提名不变；`true` 用 `banned_skill_ids` 原子替换，支持传空清空）。
3. **客户端本地轻量记忆与战报展示边界**`【工程收敛定夺】`：
   - Unity 客户端按 `(player_id, "prep_skill_bans")` 本地记住上次确认的至多 3 个禁招 ID；进房开 Ban 且本人尚未提名/未锁定时，与服务端下发 `ban_candidate_skill_ids` 求交集后预勾一次（**绝不自动锁定**），换季失效旧技自动过滤；满足 WebGL2 / IL2CPP 单线程约束。
   - `BattleReportPanel` 仅在点击「看配装（`RenderInspectedBuilds`）」与进入「战报回放 / 观战」时读取 `BattleReplay.start_state` 展示双方 3 Ban 详情，不改动战报摘要列表与 Kafka 事件。

---

## 4. 思想分析

1. **为什么复用 V3 `BattlePrepState` 而不另起 LoL 选人房？**
   - 遵循 KISS 与零分布式补偿地狱原则：房间建立、直连握手、同队单播、视角脱敏与超时自动开战全部已由 `BattleRoomManager` 单 EventLoop 闭环；BO3 每小局还能天然独立重 Ban。
2. **为什么选“个人提名 + 确定性票数/顺位/队长/SplitMix64 聚合”？**
   - 兼顾 1V1 精确控制、3V3 比赛固定队与队长决胜、3V3/5V5 野排防单人挂机浪费，且每个玩家只改自己的列表、无共享槽位并发写冲突，全程零 `player_id` 排序。
3. **为什么用单列枚举 `ban_pool_category (0..4)` 代替 `0/1` 开关 + 按 `action_tags` 猜页签？**
   - `Skill.xlsx` 中 `action_tags` 承担战斗封物/封法/混乱判定，不能为 UI 分组而修改；单列 `0..4` 同时驱动引擎白名单与客户端 4 页签滚动网格，彻底消灭硬编码技能 ID。
4. **为什么保持 `rule_version` 不变、靠 `ComputeConfigFingerprint` 防漂移？**
   - 现役排位活跃赛季库在建季后不刷新 `match_ranked_season.rule_version`，盲目递增会把已有开发库和未来线上季中热开关锁死；扩展配置指纹即可在零停服风险下守住节点一致。

---

## 5. 漏洞分析与权威防线

| 潜在漏洞 / 边界 | 风险后果 | 权威硬防线 |
|---|---|---|
| **伪造请求 Ban 掉基础输出、被动、旧季技或非法 ID** | 对手变木桩或引擎异常 | 引擎只认开局从 `BattleDataProvider::GetBanEligibleSkillIds(activeSeasonId)` 冻结的白名单集合；越界、重复、超 3 个一律拒回 `kInvalidParameter` |
| **对手或观众在 25 秒变阵期偷看己方 Ban 意向** | 泄露战队战术或针对换印换宠 | `PopulatePrepStateForViewer` 与 `RedactStateForViewer` 严格仅向同队投影 `team_states` 与 `team_effective_ban_skill_ids`；`NotifyBattlePrepUpdate` 仅同队单播 |
| **通过 `A41 玄镜映法` 复制敌方已学技能绕过禁令** | 本局禁掉冰封，却被 A41 随机抽敌方技能库放出来 | `TurnBattleEngine::IsSkillCopyableFor` 拦截所有 `IsSkillBanned` 技能；`P09/P19` 镜像反击同步纵深防御 |
| **自动战斗 / 宝宝自动选招选到被禁技能导致卡死** | 挂机或宝宝出招失败、回合异常 | `SelectAutoConfiguredSkill` 与 `SelectAutoClassSkill` 跳过 `IsSkillBanned`，无可用主动技时回落普攻 |
| **切磋坊同开「4 技能免费预设 + 3 Ban」** | 4 技能模板被禁光无招可放 | Go/Java `validateCustomDuelRules` 与引擎 `Initialize` 双层拦截 `use_free_preset && skill_ban_slots_per_team > 0`（拒回 `kInvalidParameter`） |
| **换印/换宠/锁定时误清空已选 3 Ban** | 选完 3 Ban 点一下换印导致禁招丢失 | `UpdateBattlePrepRequest` 显式带 `bool update_banned_skills = 5` 区分“不改 Ban”与“原子替换/清空”；锁定后禁止修改禁招 |
| **季中升级 `rule_version` 导致活跃赛季排位锁死** | 客户端持旧 DB 版本号被服务端全部拒入队 | 保持 `RankedRule.rule_version = 2` / `TournamentRule.rule_version = 4`，将新列纳入三端 `ComputeConfigFingerprint` |
| **按 `player_id` 大小做同票决胜违反永久 ID 不变量** | 打散永久 ID 引入随机偏置且破坏规范 | 同票走 `votes DESC -> bestSlotRank ASC -> nominatedByCaptain DESC -> SplitMix64(seed ^ skill_id) ASC` |
| **换季后本地记住的旧赛季技触发服务端报错** | S2 进房自动发送 S1 `15001` 被拒弹错 | 客户端预填前严格与服务端下发 `ban_candidate_skill_ids` 求交集剔除旧技，且绝不自动锁定 |
| **多卷宗 55 招撑爆手机小屏战前弹窗** | 按钮重叠或被小游戏右上胶囊遮挡 | 候选池采用 4 页签（10/17/18/10 项）+ 纵向 `ScrollRect` 网格，并严守 `Screen.safeArea` 与右上 `140px` 胶囊避让区 |

---

## 6. 留给实现者自行决定的细节
- C++ / Java 引擎内部存放候选池与禁用集合的具体容器选型（C++ 严守无裸指针成员门禁）；
- 战前变阵弹窗中「3 禁招区 + 4 页签滚动网格 + 印/宠区」在 `1920×1080` 与 `2400×1080` 下的具体坐标尺寸分配（保证不遮挡敌方初阵与同队锁定条、不依赖鼠标悬停）；
- 开局双方 3 Ban 亮牌横幅的入场/停留动画时长（建议 2.5～3 秒后收起为顶部常驻小条，支持点击展开查看双方禁招名称，无悬停依赖）。

---

## 7. 步骤分析与执行步骤（待负责人拍板后转入实施计划）

### 阶段 1 · 配表、Tip 与协议契约（权威 C++/Go 仓）
1. 更新 `data/schema/skill_table.proto`（`ban_pool_category = 63`）、`data/schema/rankedrule_table.proto`（`24..25`）、`data/schema/tournamentrule_table.proto`（`32`）。
2. 更新源表 `data/Skill.xlsx`（按 §3.2 标定 51 项常驻 + S1/S2 各 4 项赛季主动技的 `ban_pool_category = 1..4`）、`data/RankedRule.xlsx`（`skill_ban_slots_per_team = 3, skill_ban_min_rating_centi = 160000`）、`data/TournamentRule.xlsx`（`skill_ban_slots_per_team = 3`）、`data/tip/Tip.xlsx`（`kSkillBannedInBattle`）。
3. 更新 `proto/battle/battle_data.proto` 与 `proto/battle/player_battle.proto`（只末尾追加字段，零新消息号）。
4. 交由 Gemini 执行 `dev.bat gen`、`gen_schema_index.py --check`、Java `ContractSync` 与 Unity `gen.bat client`。

### 阶段 2 · C++ `battle` 与 Go `match` 权威服务端
1. **`cpp/libs/services/battle`**：
   - `turn_battle_constants.h`：新增 `kPrepSkillBanSlotsPerTeam = 3`、`kTacticalPrepWithBanDurationSeconds = 25`、`kTacticalPrepWithBanTimeoutMs = 25000`。
   - `battle_data_provider.h` & `table_battle_data_provider.{h,cpp}`：新增 `GetBanEligibleSkillIds(uint32_t activeSeasonId)`，`BattleTableFingerprint` 纳入 `ban_pool_category`；`ranked_preset_rules.h` 的 `ComputeConfigFingerprint` 纳入 `RankedRule` 两新列。
   - `turn_battle_engine.{h,cpp}` & `configured_battle_effects.cpp`：实现 `InitTacticalPrep`（支持锦标赛/云章+论剑/1V1 切磋开 3 Ban、25s 时长）、`UpdateBattlePrep`（校验并替换提名）、`AggregateTeamBans`（四级确定性聚合）、`FinalizePrepPhase`（冻结 `bannedSkillIds` / `teamA` / `teamB`）、`PopulatePrepStateForViewer`（严格同队脱敏）、`BuildStateSnapshot`（透传终态禁招）及 5 处战斗内全链路拦截。
2. **`cpp/nodes/battle`**：
   - `battle_room_manager.cpp`：`ArmPrepTimer` 按 `room.engine.PrepTimeoutMs()` 动态装填 15s/25s；`HandleUpdateBattlePrep` 透传 `update_banned_skills` 与 `banned_skill_ids`；复核 `RedactStateForViewer` 与回放起点录制。
3. **`go/match`**：
   - `internal/ranked/catalog.go` / `internal/authority/ranked.go` / `internal/logic/global_match.go`：读取 `RankedRule` 两新列、更新 `ComputeConfigFingerprint`，按全员最低分 ≥ `skill_ban_min_rating_centi` 且 `!use_free_preset` 冻结 `RankedMatchContext.skill_ban_slots_per_team`。
   - `internal/tournament/catalog.go` & `internal/logic/tournamentlogic.go`：冻结 `TournamentBattleContext.skill_ban_slots_per_team`。
   - `internal/logic/gather.go`：为锦标赛队长与组排小队队长填入 `BattlePlayerSnapshot.is_team_captain = true`。
   - `internal/logic/challengelogic.go`：`validateCustomDuelRules` 放行 `skill_ban_slots_per_team ∈ {0, 3}` 并拒收与 `use_free_preset = true` 同开。
4. **单测**：补齐 C++ `ranked_battle_test.cpp`（1V1 3 Ban、3V3 个人提名/共识/顺位/队长决胜/挂机补位、5V5、撞车去重、A41 禁复制、自动战斗回落普攻、重连/观众脱敏、1V1 切磋纯禁招与互斥）与 Go `match` 规则/指纹单测。

### 阶段 3 · Java 独立服务端双版对齐（`xuanming-server-mmo`）
1. 同步契约与配表（`ContractSync --check` 通过），实施前先核对 `game-battle` / `game-match` 在排位/切磋路径上是否存在尚未登记 `PARITY.md` 的历史漂移。
2. 在 `game-battle-engine`、`game-battle`（`BattleViews`、`BattleRoomServiceImpl`）与 `game-match`（`RankedRules`/`RankedStore`/`TournamentTables`/`TournamentService`/`GatherPipeline`/`Challenge`）逐条同构实现并补齐边界单测，登记 `PARITY.md`。

### 阶段 4 · Unity 客户端逻辑、UGUI 与异机 Codex 美术交接（`mmorpg-client`）
1. **逻辑层**：扩展 `BattleClient.cs`（`UpdateBattlePrep` 支持 `updateBannedSkills` + `bannedSkillIds`）；新增纯 C# `BattleSkillBanPreferenceStore`（本地记忆 + 白名单交集过滤）与基于 `SkillTable.BanPoolCategory` 的 4 页签分组器。
2. **UGUI 层**：
   - `BattleScreen.cs`：战前变阵浮层新增「禁招（0/3）」区（顶部 3 生效槽 + 4 页签纵向滚动候选网格 + 同队提名计数标记；1V1 切磋纯禁招模式自动隐藏空印/宠区），接入开局亮牌与顶部双方禁招常驻小条，在人物与宝宝技能按钮叠加「已禁」置灰禁用态并拦截点击提示；缺新切图时优雅降级至既有控件底板。
   - `CustomDuelChallengePanel.cs`：新增「战前各禁 3 招」开关（勾选公平免费预设时自动置灰互斥并提示）。
   - `BattleReportPanel.cs`（`RenderInspectedBuilds`）与观战 HUD：读取 `BattleReplay.start_state` 展示双方 3 Ban 详情。
3. **美术切片交接**：交付 [battle-prep-skill-ban-ui-handoff.md](ui-handoff/battle-prep-skill-ban-ui-handoff.md) 的 8 枚无字透明 PNG-32 切片给异机 Codex 制作并导入 `Assets/Resources/UI/Ugui/BattlePrepSkillBan/`。
4. **验证门禁（Gemini 执行）**：C++ 静态守卫 + MSBuild `/m:1` + GTest、Go `go test ./...`、Java `mvnw clean install` + 定向单测、Unity `client_compile_check.ps1` + EditMode NUnit 单测，并追加实跑证据至 `docs/PROGRESS.md`。

---

## 8. V2 增量扩展：每方「3 禁 + 1 保（护招破禁）」、队友已带标记与开局亮牌揭示

> 落盘日期：2026-11-26｜零新 RPC、零新消息号、零配表新列、零 `rule_version` 变更，沿用同一 25 秒战前窗。

1. **核心博弈规则（「3 禁 + 1 保」）**：
   - 任意开启战前技能禁选的对局（`skill_ban_slots_per_team > 0`）自动激活每队 `skill_protect_slots_per_team = 1`；
   - 每名真人玩家可在候选白名单内独立提名 `0..1` 个护招（`UpdateBattlePrepRequest.update_protected_skill = 7` / `protected_skill_id = 8`），严禁与本人最终禁招列表同招（客户端切换点选时单次原子请求自动移出冲突项）；
   - 队内护招确定性聚合 `AggregateTeamProtect(teamIndex)`：`votes DESC -> nominatedByCaptain DESC -> SplitMix64(seed ^ 0x50524F54454354ULL ^ skill_id) ASC -> skill_id ASC`；
   - `FinalizePrepPhase()` 破禁结算：B 队护招反制 A 队禁招，A 队护招反制 B 队禁招；若某技能被双边同时封禁而仅有一边护招，另一方自己的未受反制禁招仍生效（`countered_ban_skill_ids` 与 `banned_skill_ids` 严格互斥），被破禁化解的技能从 `banned_skill_ids` 移除并恢复全员可施放。
2. **「队友已带」防误禁标记（零泄密）**：
   - `BattlePrepState.teammate_equipped_skill_ids = 13` 仅向同队参战者下发“本队其他队友人物及当前所选首发宠已携带、且属于候选池”的升序去重技能 ID（队友临场换宠即实时刷新；1V1 / 对手 / 观众恒为空）。
3. **协议增量（仅末尾追加字段）**：
   - `BattlePrepPlayerState.nominated_protect_skill_id = 9`
   - `BattlePrepState.{skill_protect_slots_per_team = 11, team_effective_protected_skill_id = 12, teammate_equipped_skill_ids = 13}`
   - `BattleStateS2C.{team_a_protected_skill_id = 44, team_b_protected_skill_id = 45, countered_ban_skill_ids = 46}`（天然覆盖重连 / L2 观战 / 战报回放 / 看配装）
   - `UpdateBattlePrepRequest.{update_protected_skill = 7, protected_skill_id = 8}`
4. **Unity 客户端与 V2 5 枚切片（Codex `72ca11aa44177de8d3a9997a92ef02c0a7a842b3`）**：
   - 导入 `prep_protect_slot_empty.png`、`prep_protect_slot_filled.png`、`skill_protected_badge.png`、`battle_ban_reveal_banner_bg.png`（960×220 九宫格 `180/52/180/55`）、`prep_teammate_equipped_tag.png`（48×48，卡片局部 `[68,2,90,24]`）至 `Assets/Resources/UI/Ugui/BattlePrepSkillBan/`；
   - 战前浮层新增 1 护招槽与禁/保切换按钮、候选卡右上角「队友已带」标记、顶部禁招条破禁角标与详情展开、以及第 1 回合开局 2.65 秒居中亮牌揭示横幅（避让右上 140px 小游戏胶囊区）。
