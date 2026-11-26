# 赛季玩法 V4：十季差异化主题池、无限期号自动循环与往季永久徽章追忆设计方案

- **状态**：待负责人审批（已确认采用「方案 1：无限期号 × 可扩展 10 季自动循环池 + 满级后往季徽章追忆」）
- **关联文档**：[season-skills-and-seal-design.md](season-skills-and-seal-design.md)（V1–V3 基线）
- **影响仓库**：`xuanming-server-mmo-cpp`（C++ / Go / 配置表权威源）、`xuanming-server-mmo`（Java 对齐版）、`xuanming-server-mmo-python`（Python 对齐版）、`mmorpg-client`（Unity 客户端）

---

## 1. 需求背景与目标

### 1.1 背景与设计目标

当前赛季玩法 V1–V3 已在四仓完成首季「破阵之季」闭环，但面临两项长期运营瓶颈：

1. **内容与运营排期瓶颈**：`SeasonRule.xlsx` 目前仅支持固定单次 `[starts_at_ms, ends_at_ms)` 时间窗，尚未支持无人值守自动轮换；除 S1 外仅预埋了复用 S1 效果的 S2 占位。
2. **无限赛季与新玩家防绝版矛盾**：若赛季随版本持续推进（S1 → S2 → … → S10 → 无限延续），后入坑或回流玩家要么永远错过老赛季永久纪念徽章，要么要干等大半年等待循环轮回。

本方案（V4）目标：

1. **10 套不重样主题赛季（S1–S10）**：每季提供 4 招当季免费自动生效技能（共 40 招）、4 枚单槽赛季印（共 40 枚）、4 组表驱动「技印共鸣」与 1 枚永久零数值纪念徽章及称谓（共 10 枚）。
2. **无限期号 + 可扩展自动循环池**：`SeasonRule.xlsx` 新增 `cycle_order` / `cycle_duration_days` 两列；启用后服务端按统一时钟确定性推进无限递增的 `season_term`（第 1 届、第 2 届 … 第 N 届）并自动轮换主题池；未来追加 S11/S12 仅需增表行，循环池自动扩容，无需改引擎。首测默认 `cycle_duration_days = 0`，100% 保持现有 S1 常开行为不变。
3. **每届轻进度重置 + 永久徽章永不收回 + 满级后往季徽章追忆**：每开启新一届（含循环回到第 2 轮 S1），当届 12 点轻进度自动重置为 `0/12`；永久纪念徽章改由动态位图持久化，永不收回。当届打满 `12/12` 后，继续参与合格排位或首领试炼产生的溢出点数自动累积至玩家选定（或自动顺延）的任意 1 枚未解锁往季徽章，满 12 点即永久解锁，彻底消除绝版焦虑。

### 1.2 绝对不可突破的红线（Anti-Goals）

- ❌ **严禁**赛季印洗练、升星、多槽套装或任何赛季专属养成材料（始终保持单槽 4 选 1 免费切换）。
- ❌ **严禁**赛季每日必做打卡任务或 50 级重度战令（保持 12 点轻进度且无每日上限）。
- ❌ **严禁**给赛季印或共鸣加速度、必杀、控率、控抗或突破 `<= 5%`（`500 bps`）轻属性硬帽（禁止赛季印使用 `effect_id = 3` 控命与 `effect_id = 8` 控抗）。
- ❌ **严禁**往季徽章追忆开放灵石直买或解锁老赛季战斗属性/老赛季技能（纯玩法溢出点免费补领、纯零数值永久外观荣誉）。
- ❌ **严禁**触碰应用商店/小游戏审核红线（零外链、零切支付、零诱导文案）。

### 1.3 备选方案对比（Approaches）

| 方案 | 核心机制 | 优点 | 缺点 / 取舍 | 结论 |
|---|---|---|---|---|
| **方案 1（已选）** | 无限期号 × 10 季可扩展自动循环池 + 满级后溢出点免费「往季徽章追忆」 | 老玩家每届有 12 点轻目标；新玩家无需干等 8–9 个月即可补齐任意老徽章；未来持续追加 S11+ 也不绝版 | 需增加 1 条轻量设目标 RPC（567）及存档位图/追忆字段 | **采用** |
| 方案 2 | 纯 10 季循环，不做往季追忆 | 零新 RPC | 新玩家错过老赛季需干等约 8 个半月；未来加新赛季不循环时老徽章永久绝版 | 放弃 |
| 方案 3 | 满级后直接永久锁 `12/12` 不按届重置 | 零存档字段变动 | 循环回老赛季时老玩家无轻进度目标，缺失换季仪式感 | 放弃 |

---

## 2. 数据流与所有权

### 2.1 权威所有权划分

| 数据 / 状态 | 权威节点 | 持久化 / 传输位置 | 说明 |
|---|---|---|---|
| 10 季配置与循环排期 | 策划配置表 | `data/SeasonRule.xlsx`、`data/SeasonSeal.xlsx`、`data/Skill.xlsx`、`data/Buff.xlsx` | `SeasonRule` 控制固定窗 vs 自动循环；`SeasonSeal` 直接配置表驱动共鸣参数 |
| 当前届号、轮次、活跃主题与起止时刻 | Scene（全节点同口径纯规则计算） | `seasonrules::ResolveActiveSeason(...)` | 仅依赖表快照与 UTC 毫秒墙钟，无状态、跨节点确定性一致 |
| 玩家当届进度、永久徽章位图、追忆目标与追忆进度 | Scene（GROWTH 存档组） | `PlayerSeasonComp`（`player_database` 字段 30） | 随常规战斗结算与 RPC 原子落盘 |
| 局内单槽赛季印基础效果与表驱动共鸣 | Battle Engine（C++ / Java / Python） | `EquipmentEffects.season_seal` → `TurnBattleEngine` 运行态 | 开局由 Scene 冻结传入，战内不可篡改；公开快照脱敏 |
| 战后当届进度与往季追忆增量反馈 | Scene → Client | `BattleEndS2C` 字段 `10..13`（当届）与 `21..25`（追忆） | 仅随权威结算推包展示，客户端不自算 |

### 2.2 无限循环排期纯规则（`ResolveActiveSeason`）

在 `SeasonRuleTable` 新增两列：`cycle_order`（字段 18）、`cycle_duration_days`（字段 19）。

1. **模式判定**：
   - 若所有行 `cycle_duration_days == 0`：**固定单季模式**（沿用既有 `IsWindowActive` 逻辑，`season_term = 1`、`cycle_index = 1`、`cycle_pool_size = 0`），首测 S1 常开零变化。
   - 若任意行 `cycle_duration_days > 0`：**自动无限循环模式**。
2. **自动循环 fail-closed 校验**（启动 / 热更全校验，任一不满足返回非法并拒启或无活跃赛季）：
   - 收集所有 `cycle_order > 0` 的行作为循环池（数量 `N >= 1`），要求所有入池行 `enabled == true`、`cycle_order` 严格构成 `1..N` 连续不重复序列；
   - 所有入池行 `cycle_duration_days` 相等且在硬边界 `[7, 180]` 天内；
   - 全局锚点 `anchorMs = pool[0].starts_at_ms` 必须 `> 0`；
   - 非入池行（`cycle_order == 0`）不得与自动循环并存 `enabled == true`，防止双赛季冲突；
   - 池内每条 `SeasonRule` 通过 `ValidateRule`，且其 `default_seal_id` 与该季全部赛季印通过 `ValidSealEffect`。
3. **确定性换算**：
   - 若 `nowMs < anchorMs`：尚未开季（返回无活跃赛季）。
   - `periodMs = cycle_duration_days * 86_400_000ULL`
   - `elapsedIdx = (nowMs - anchorMs) / periodMs`
   - `season_term = elapsedIdx + 1`（`1, 2, 3, ..., ∞`）
   - `cycle_index = (elapsedIdx / N) + 1`（`1, 2, ...`）
   - `active_rule = pool[elapsedIdx % N]`（`theme_id = active_rule.id`）
   - `term_start_ms = anchorMs + elapsedIdx * periodMs`
   - `term_end_ms = term_start_ms + periodMs`
   - 周环境词缀周次按 `term_start_ms` 计算（`weekIndex = (nowMs - term_start_ms) / kMsPerWeek + 1`），`weeklyAffix = WeeklyAffixForWeek(season_term, weekIndex)`，确保每届第 1 周起完整轮换且相邻两届不重复。

### 2.3 永久徽章位图与往季追忆数据流

1. **懒迁移与期号重置**：
   - 读取/写入 `PlayerSeasonComp` 时，对历史旧存档中任意 `record.progress_points >= rule.seal_max_points` 的 `record.season_id`，自动置位 `unlocked_honor_words` 对应 bit（`word = (season_id - 1) / 64, bit = (season_id - 1) % 64`）。
   - 当访问当期活跃主题的 `PlayerSeasonSealRecord` 时，若 `record.last_term != currentTerm`：
     - 先确认旧 `progress_points` 若已满则已置位永久徽章位图；
     - 若为历史遗留存档（`last_term == 0` 且 `currentTerm == 1`），原样继承进度并将 `last_term` 补写为 `1`（**首测老号零清档**）；
     - 若进入新一届（`last_term != 0 && last_term != currentTerm` 或 `currentTerm > 1 && last_term < currentTerm`），将 `progress_points`、`battle_points`、`catchup_points` 归零，写入 `last_term = currentTerm`，保留 `equipped_seal_id` 与永久徽章位图不变。
2. **战后点数分流（合格排位 / 首领试炼结算）**：
   - 计算本场应得点数 `pts`（排位胜 `+2`、合格完赛 `+1`、首领试炼胜 `+1`，沿用全部防挂机与隔离副本拦截规则）。
   - **第一步（当届优先）**：若当届进度 `< maxPoints`，先补足当届，消耗 `usedForCurrent = min(pts, maxPoints - currentProgress)`；若当届因此达到 `maxPoints`，置位 `unlocked_honor_words[activeSeasonId]`，清除 `retro_honor_progress[activeSeasonId]`，若 `retro_honor_target_season_id == activeSeasonId` 则清 0；标记 `season_honor_newly_unlocked = (!wasAlreadyUnlocked)`（第二轮再打满已拥有徽章时不再重复弹「首次解锁」）。
   - **第二步（溢出自动追忆）**：若 `overflow = pts - usedForCurrent > 0`：
     - 解析有效追忆目标 `targetSeasonId`：若 `retro_honor_target_season_id` 非 0、在 `SeasonRule` 表中存在且尚未解锁，则取之；否则自动选取表内按 `id` 升序**首个未解锁且 `id != activeSeasonId`** 的赛季（若全部赛季徽章皆已解锁则停止，不产生多余写入）。
     - 累加 `retro_honor_progress[targetSeasonId] += overflow`，封顶 `targetRule.seal_max_points`（12）。
     - 若达到满值：置位 `unlocked_honor_words[targetSeasonId]`、删除 `retro_honor_progress[targetSeasonId]`、清空 `retro_honor_target_season_id`（若匹配）、在 `BattleEndS2C` 填充 `retro_honor_newly_unlocked = true` 与相关字段。
   - **快捷追赶边界**：既有 `CatchupSeasonSeal`（455）**只作用于当届活跃赛季**，绝不触发溢出追忆，杜绝用灵石直买往季徽章。
   - **荣誉墙全量展示**：`SeasonStateSnapshot.honors` 按 `SeasonRule.id` 升序返回全部已配置赛季（1..10）及各自解锁态、当季态与追忆进度，使玩家随时可在面板一键切换追忆目标。

---

## 3. 表驱动共鸣原语与十季完整配置设计

### 3.1 基础赛季印机制范围（仅 6 类零膨胀原语）

赛季印严格禁止 `effect_id = 3`（控命）与 `effect_id = 8`（控抗），全引擎（C++ / Java / Python）对单槽赛季印开放以下 6 类零速度/零必杀/零控率基础机制：

| `effect_id` | 名称 | 机制规则 | 结算功劳主口径 |
|---|---|---|---|
| `1` | 物理削盾 | 主目标首段物理直伤后额外削减 `bonus_bps` 剩余护盾，不伤气血 | 累计削盾量 |
| `2` | 法术削盾 | 主目标首段法术直伤后额外削减 `bonus_bps` 剩余护盾，不伤气血 | 累计削盾量 |
| `4` | 急救增疗 | 主动治疗主目标且目标血比 ≤ `health_threshold_bps` 时额外增疗 `bonus_bps` | 累计额外治疗量 |
| `5` | 首盾增容 | 主动施盾命中主目标首段护盾时额外增容 `bonus_bps` | 累计额外护盾量 |
| `6` | 开局减伤 | 前 `active_rounds` 回合首次受敌直伤减免 `bonus_bps`（封顶入战最大血量 `cap_bps`） | 累计减伤量（+ 共鸣增盾/增疗量） |
| `7` | 防御回蓝 | 实际执行普通防御后回复入战最大法力 `bonus_bps`（按缺口封顶） | 累计回蓝量（+ 共鸣省蓝量） |

*(注：V1–V3 已在三端接入 `1/2/6/7`，V4 补齐 `4/5` 在单槽赛季印通道的触发、统计与脱敏，代码量约每端 20 行。)*

### 3.2 8 种表驱动「技印共鸣」原语（硬上限校验、零硬编码赛季 ID）

`SeasonSeal.xlsx` 新增 4 列：`resonance_skill_id`、`resonance_kind`、`resonance_param`、`resonance_desc`。共鸣子事件统一复用 `BATTLE_EVENT_ARTIFACT`（`artifact_item_id=0, season_seal_id=seal_id, artifact_effect_id=100 + resonance_kind`）。

| `resonance_kind` | 事件号 | 允许搭配的 `effect_id` | 触发条件与效果 | 服务端硬上限 |
|---|---|---|---|---|
| `1` 碎盾返蓝 | `101` | `1, 2` | 使用 `resonance_skill_id` 触发削盾且目标常规护盾归零 → 返还当次实付耗蓝 `param%`（向上取整，缺口封顶） | `param <= 40` |
| `2` 破盾延减益 | `102` | `1, 2` | 使用 `resonance_skill_id` 触发削盾 → 同次施放紧随落地的敌方 `Modifier` 减益持续回合 `+param`（绝不延硬控） | `param <= 1` |
| `3` 守印次轮增盾 | `103` | `6` | 触发开局减伤后次轮直接施放 `resonance_skill_id` → 本次实际落地护盾量 `+param%`（每场限 1 次） | `param <= 15` |
| `4` 聚息次轮省蓝 | `104` | `7` | 防御触发回蓝后次轮施放 `resonance_skill_id` → 耗蓝降低 `param%`（校验与扣费同口径） | `param <= 50` |
| `5` 急救返蓝 | `105` | `4` | 直接施放 `resonance_skill_id` 触发急救增疗 → 返还当次实付耗蓝 `param%`（向上取整，每回合受印限次） | `param <= 35` |
| `6` 首盾回蓝 | `106` | `5` | 直接施放 `resonance_skill_id` 触发首盾增容 → 回复入战最大法力 `param` 万分比（缺口封顶） | `param <= 300` |
| `7` 守印次轮增疗 | `107` | `6` | 触发开局减伤后次轮直接施放 `resonance_skill_id` → 本次实际落地治疗量 `+param%`（每场限 1 次） | `param <= 15` |
| `8` 聚息次轮增盾 | `108` | `7` | 防御触发回蓝后次轮直接施放 `resonance_skill_id` → 本次实际落地护盾量 `+param%` | `param <= 12` |

### 3.3 十季完整目录（S1–S10，共 40 免费技能 + 40 赛季印 + 10 永久徽章）

全部 10 季统一轻数值上限：`seal_max_points=12`、`pve_win=1`、`pvp_win=2`、`pvp_other=1`、`max_health_bps_cap=300`、`defense_bps_cap=300`、`catchup_currency_type=1`（灵石）、`catchup_price_per_point=10`；全部 40 技能均为 `unlock_level=1, max_rank=10, battle_model=1, caster_mask=1, ability_kind=1, scroll_id=0, rarity_tier=0` 且无硬控/无加速/无暴击。

| 季 | ID | 名称 | 主题特色 | 4 招当季免费技能（ID & 机制） | 4 枚单槽赛季印（ID / `effect_id` / 共鸣原语与参数） | 永久称谓 & 徽章图标键 |
|---|---|---|---|---|---|---|
| **S1** | `1` | **破阵之季** | 破盾压制与短回合破甲（保持既有不变） | `15001` 破阵裂锋(3体物+破甲-15%)、`15002` 破阵灵雷(3体法+破甲-15%)、`15003` 固阵灵障(3体护盾)、`15004` 破阵蓄势(3体双攻+15%) | `101` 破阵锐印(1 / R1=30%)、`102` 破阵玄印(2 / R2=+1)、`103` 固阵守印(6 / R3=10%)、`104` 聚息援印(7 / R4=50%) | 破阵宗师 · `UI/SeasonHonors/honor_s1_pozhen`（✅已有） |
| **S2** | `2` | **玄脉之季** | 易伤聚火、急救稳血与厚盾续流 | `15101` 玄脉破军(3体物+受击易伤+10%)、`15102` 玄脉天罚(3体法+降敌法攻-12%)、`15103` 玄脉灵幕(2体高额护盾)、`15104` 玄脉归元(3体群疗+低血急救+25%) | `201` 玄脉锋印(1 / R2=+1延易伤)、`202` 玄脉雷印(2 / R1=30%碎盾返蓝)、`203` 玄脉护印(5首盾12% / R6=250bps回蓝)、`204` 玄脉息印(4急救12% / R5=30%返蓝) | 玄脉行者 · `UI/SeasonHonors/honor_s2_xuanmai`（✅已有） |
| **S3** | `3` | **离火之季** | 灼烧叠势、炎刃引燃与火幕净秽 | `15201` 离火焚阵(3体法+2回合灼烧)、`15202` 炎刃引爆(单体高物+催动灼烧)、`15203` 炽幕护心(2体护盾+反伤15%)、`15204` 薪火回生(2体治疗+驱散1减益) | `301` 离火刃印(1 / R1=30%)、`302` 离火炎印(2 / R1=30%)、`303` 炽幕守印(6 / R3=10%增盾)、`304` 薪火疗印(4 / R5=30%返蓝) | 离火尊者 · `UI/SeasonHonors/honor_s3_lihuo` |
| **S4** | `4` | **青木之季** | 毒蔓缠甲、回春续航与苍木筑垒 | `15301` 青藤缠甲(3体物+破甲-15%)、`15302` 碧毒蚀脉(3体法+2回合中毒)、`15303` 枯荣回春(3体治疗+受疗+15%)、`15304` 苍木灵盖(3体护盾+防+12%) | `401` 青藤锐印(1 / R2=+1延破甲)、`402` 苍木盾印(5 / R6=250bps回蓝)、`403` 枯荣济印(4 / R5=30%返蓝)、`404` 灵木息印(7 / R8=10%次轮增盾) | 青木济世 · `UI/SeasonHonors/honor_s4_qingmu` |
| **S5** | `5` | **玄水之季** | 精准斩残、卸势净尘与控蓝节奏 | `15401` 寒潮斩浪(2体物+副目标选最低血比)、`15402` 冥水覆涛(3体法+降敌物攻-12%)、`15403` 碧波净尘(2体驱散1减益+回血)、`15404` 潮汐聚灵(3体回蓝+减耗-15%) | `501` 寒潮锋印(1 / R1=30%)、`502` 冥水玄印(2 / R2=+1延降攻)、`503` 静水守印(6 / R7=12%次轮增疗)、`504` 潮汐援印(7 / R4=50%半蓝) | 玄水清流 · `UI/SeasonHonors/honor_s5_xuanshui` |
| **S6** | `6` | **厚土之季** | 穿防重锤、崩势驱敌与铁壁固本 | `15501` 裂地重击(2体物+1回合穿防15%)、`15502` 岩震崩势(2体法+驱敌1增益)、`15503` 山岳承伤(2体护盾+受直伤-12%)、`15504` 磐石固本(3体防+18%+小额回血) | `601` 裂地破印(1 / R1=30%)、`602` 山岳盾印(5 / R6=250bps回蓝)、`603` 磐石守印(6 / R3=10%增盾)、`604` 厚土息印(7 / R8=10%次轮增盾) | 厚土宗师 · `UI/SeasonHonors/honor_s6_houtu` |
| **S7** | `7` | **庚金之季** | 点杀破核、专挑脆皮与一击蓄锋 | `15601` 庚金断锋(单体强物+破甲-18%)、`15602` 剑雨贯阵(3体法+副目标选最低法防+易伤+10%)、`15603` 金钟镇岳(单体厚盾)、`15604` 锐金砺刃(2体下击双攻+20%) | `701` 庚金锐印(1 / R2=+1延破甲)、`702` 剑气玄印(2 / R2=+1延易伤)、`703` 金钟护印(5 / R6=250bps回蓝)、`704` 砺锋援印(7 / R4=50%半蓝) | 庚金剑首 · `UI/SeasonHonors/honor_s7_gengjin` |
| **S8** | `8` | **太阴之季** | 幽影点弱、延毒绵耗与素月保命 | `15701` 幽影袭脉(2体物+副目标选最低物防+易伤+10%)、`15702` 太阴蚀魂(3体法+延长持续伤害1跳)、`15703` 月华抚伤(2体强急救治疗)、`15704` 素月避锋(2体护盾+法伤减免15%) | `801` 幽影锋印(1 / R2=+1延易伤)、`802` 太阴玄印(2 / R1=30%碎盾返蓝)、`803` 月华疗印(4 / R5=30%返蓝)、`804` 素月守印(6 / R7=12%次轮增疗) | 太阴巡夜 · `UI/SeasonHonors/honor_s8_taiyin` |
| **S9** | `9` | **天雷之季** | 雷枪破盾、群雷压脆与雷罡反制 | `15801` 雷枪破盾(2体物+破甲-15%)、`15802` 九霄神雷(3体法+副目标选法攻最高+降法攻-12%)、`15803` 雷罡护体(2体护盾+受法伤-15%)、`15804` 引雷蓄能(2体双攻+15%+回蓝) | `901` 雷枪锐印(1 / R1=30%)、`902` 九霄雷印(2 / R2=+1延降法攻)、`903` 雷罡盾印(5 / R6=250bps回蓝)、`904` 引雷援印(7 / R4=50%半蓝) | 天雷御使 · `UI/SeasonHonors/honor_s9_tianlei` |
| **S10** | `10` | **混元之季** | 五行合流、吸血反打与两仪均衡 | `15901` 混元一击(2体物+15%吸血)、`15902` 太极归虚(3体法+驱敌1增益+降防-12%)、`15903` 两仪灵罩(3体护盾)、`15904` 混元回生(3体治疗+双攻+10%) | `1001` 混元锐印(1 / R1=30%)、`1002` 太极玄印(2 / R2=+1延降防)、`1003` 两仪守印(6 / R3=10%增盾)、`1004` 混元疗印(4 / R5=30%返蓝) | 混元大宗 · `UI/SeasonHonors/honor_s10_hunyuan` |

---

## 4. 关键协议、配置与代码改动清单

### 4.1 权威配置表与 Schema（`xuanming-server-mmo-cpp/data/`）

1. `data/schema/seasonrule_table.proto` & `data/SeasonRule.xlsx`：
   - 追加 `uint32 cycle_order = 18;`、`uint32 cycle_duration_days = 19;`；
   - 配齐 S1–S10 共 10 行（默认 `S1 enabled=true, starts_at_ms=0, cycle_duration_days=0` 维持首测常开；`S2..S10 enabled=false, cycle_order=2..10, cycle_duration_days=0`，公测切循环只需统一改 `cycle_duration_days=28` + `S1.starts_at_ms=公测时间` + 全 10 行 `enabled=true`）。
2. `data/schema/seasonseal_table.proto` & `data/SeasonSeal.xlsx`：
   - 追加 `uint32 resonance_skill_id = 13;`、`uint32 resonance_kind = 14;`、`uint32 resonance_param = 15;`、`string resonance_desc = 16;`；
   - 配齐 S1–S10 共 40 行赛季印（S1 `101..104` 补填既有共鸣字段；S2 `201..204` 升级为玄脉主题；新增 `301..1004`）。
3. `data/Skill.xlsx` & `data/Buff.xlsx`：
   - 保持 S1 `15001..15004` 不变，升级 S2 `15101..15104` 为真正差异化技能，新增 S3–S10 `15201..15904` 共 32 招技能与配套 Buff 行 `151010..159041`（全部 `ban_pool_category ∈ {2, 3}`）。
4. `data/tip/Tip.xlsx` & `data/MessageLimiter.xlsx`：
   - `season_error` 新增 `kSeasonHonorAlreadyUnlocked = 34012`、`kSeasonHonorTargetInvalid = 34013`；
   - `MessageLimiter.xlsx` 注册 `567`（与 `454` 同档限流）。

### 4.2 协议改动（`xuanming-server-mmo-cpp/proto/`，同步至 Java / Python / Unity）

1. `proto/common/component/equipment_effects.proto`（`SeasonSealEffect` 追加 `11..13`）：
   - `uint32 resonance_skill_id = 11; uint32 resonance_kind = 12; uint32 resonance_param = 13;`
2. `proto/common/component/player_season_comp.proto`：
   - `PlayerSeasonSealRecord` 追加 `uint32 last_term = 7;`；
   - `PlayerSeasonComp` 追加 `repeated uint64 unlocked_honor_words = 5;`、`uint32 retro_honor_target_season_id = 6;`、`map<uint32, uint32> retro_honor_progress = 7;`。
3. `proto/scene/player_season.proto`：
   - `SeasonSealOption` 追加 `13..16`（`resonance_skill_id / resonance_kind / resonance_param / resonance_desc`）；
   - `SeasonHonorView` 追加 `bool is_retro_target = 9; uint32 retro_progress_points = 10;`；
   - `SeasonStateSnapshot` 追加 `35..39`（`season_term / cycle_index / cycle_pool_size / retro_honor_target_season_id / retro_honor_target_explicit`）；
   - 新增 RPC `SetRetroHonorTarget`（消息号 `567`，`SetRetroHonorTargetRequest { uint32 season_id = 1; }` / `SetRetroHonorTargetResponse { TipInfoMessage error_message = 1; SeasonStateSnapshot state = 2; }`）。
4. `proto/battle/player_battle.proto`（`BattleEndS2C` 追加 `21..25`）：
   - `retro_honor_season_id / retro_honor_points_added / retro_honor_progress_after / retro_honor_max_points / retro_honor_newly_unlocked`。

### 4.3 四仓代码改动

1. **C++（`xuanming-server-mmo-cpp`）**：
   - `season_rules.h`：新增 `ResolveActiveSeason`（固定窗 + 无限循环池校验与换算）、永久徽章位图读写纯函数、8 种共鸣参数边界校验与计算公式、禁止赛季印使用 `effect_id ∈ {3, 8}`。
   - `turn_battle_engine.{h,cpp}` / `equipment_battle_effects.cpp` / `configured_battle_effects.cpp`：将 V3 的 S1 硬编码共鸣全面升级为读 `seal.resonance_skill_id / resonance_kind / resonance_param` 的通用表驱动分支，补齐赛季印 `effect_id = 4`（急救增疗）与 `effect_id = 5`（首盾增容）及 `R5..R8` 4 种共鸣原语。
   - `player_season.{h,cpp}` / `player_season_handler.cpp` / `player_battle.cpp` / `main.cpp`：接入 `ResolveActiveSeason`、`last_term` 换届重置、永久徽章位图懒迁移、满级溢出往季追忆、`SetRetroHonorTarget`（567）及 `BattleEndS2C` `21..25` 透传。
2. **Java（`xuanming-server-mmo`）**：
   - `ContractSync` 全量同步表与协议；`game-player-store` 升级 `player_state.proto` 并新增第 22 代持久台账 `0022.schema`；
   - `game-battle-engine`（`SeasonRules`、`BattleContext`、`BattleEquipment`、`ConfiguredBattle`、`ConfiguredEffects`、`ConfiguredImpact`）与 `game-scene`（`SeasonTables`、`PlayerSeason`、`SeasonService`、`SeasonFeature`、`PlayerBattleService`、`BattleSettlementService`）逐项对齐。
3. **Python（`xuanming-server-mmo-python`）**：
   - 同步 4 份 proto、4 张数据表与 `build_protocol.py`；同步 `battle_dropin_effects.py`、`battle.py`、`scene_season.py`、`battle_settlement.py`。
4. **Unity 客户端（`mmorpg-client`）**：
   - 同步 `gen_proto.ps1`、`gen_messageids.ps1`、`sync_tables.ps1`；
   - `SeasonModels.cs` / `SeasonClient.cs`：新增 `SetRetroHonorTarget(uint seasonId)`（567）、支持共鸣事件 `101..108` 飘字、优先读取服务端 `seal.ResonanceDesc`（彻底去硬编码）、扩展 `SeasonSettlementProgressView` 展示往季追忆增量与解锁；
   - `SeasonSealPanel.cs`：顶部显示 `第 N 届 · 主题名`（循环模式显示轮次），荣誉陈列区升级为 **2×5 十季永久徽章墙**（已解锁亮金、未解锁支持一键点击「设为追忆 / 追忆中 x/12」），复用现有切图零新切图；
   - `CoreSettlementRewardSection.cs`：满级溢出触发往季追忆时展示追忆进度条/解锁提示。
   - **Codex 美术需求（仅 8 枚新图标，零风格指令）**：补画 `honor_s3_lihuo.png` ~ `honor_s10_hunyuan.png` 共 8 枚 512×512 透明 RGBA 永久纪念徽章，导入 `Assets/Resources/UI/SeasonHonors/`（缺图时 `SeasonV1UiArt.LoadHonorBadgeIcon` 安全降级不报错，不阻塞联调）。

---

## 5. 边界、并发、异常与安全处理

1. **首测老档与混版本兼容**：`last_term == 0` 的存量 S1 玩家在固定窗（`term=1`）下无缝保留全部进度与已解锁徽章；`unlocked_honor_words` 动态按 `word = (season_id - 1) / 64` 扩容，支持超过 64 季且对高位未知 bit 原样回写不截断。
2. **热更与配置 fail-closed**：`SeasonRule` 循环池出现 `cycle_order` 缺号/重号、天数不一致或越界 `[7, 180]`、`anchorMs == 0`、或任意赛季印共鸣参数越界 / `effect_id ∈ {3, 8}` 时，启动与热更直接报错拒绝加载，运行期绝不靠猜测降级。
3. **跨届时刻与在途战斗边界**：战斗开局在 `PrepareBattle` 冻结当刻活跃 `season_id` 与 `SeasonSealEffect`（含 `resonance_*`）；若对局跨越换届整点，战内全程按冻结快照执行不漂移，战后结算按结算时刻活跃届号归账（与既有 `season_id` 一致）。
4. **防刷与幂等**：`SetRetroHonorTarget` 仅改纯荣誉目标指针，战斗中与角色冻结态禁止调用（`kSeasonInBattle` / `kSeasonFrozen`）；已解锁目标拒绝设置（`kSeasonHonorAlreadyUnlocked`）；往季追忆仅接受常规合格战斗溢出点，受既有挂机门禁、隔离副本拦截和战斗结果收件箱幂等保护。
5. **公开快照脱敏**：战内 `SeasonSealRuntimeState` 与 `season_seal_state` 不下发给对手与观众；`SeasonHonorView` 仅向本人展示追忆进度。

---

## 6. 测试计划

1. **C++（`xuanming-server-mmo-cpp`）**：
   - `SeasonRulesTest`：固定窗 vs 自动无限循环（验证 `term 1..22` 跨 2 轮 10 季切换、`cycle_order` 缺口/超限 fail-closed、动态位图置位与 65+ 季扩容、8 种共鸣参数边界校验与拒绝 `effect_id 3/8`）。
   - `SeasonBattleTest`：覆盖 `effect_id 4/5` 赛季印触发 + 全部 8 种表驱动共鸣原语（`R1..R8`）+ 真实导表加载 S1–S10 全 40 技能 / 40 印零坏表回归。
   - `PlayerSeasonSystemTest`：验证换届 `last_term` 进度重置而永久徽章保留、满级溢出自动顺延追忆、手动 `SetRetroHonorTarget(567)` 切换与满 12 点解锁、灵石追赶绝不溢出至往季。
   - 护栏：`dev.bat gen`、`test_cpp*`、`check_log_text_english.py`、Go `go vet`。
2. **Java（`xuanming-server-mmo`）**：
   - `PlayerStateSchemaLedgerTest`（验证 `0022.schema` 合法）、`SeasonBattleEngineTest`（`R1..R8` 与 `effect_id 4/5`）、`SeasonFeatureAndServiceTest`（10 季无限循环、换届重置、追忆 RPC 与结算溢出）、`CoreGameplayLoopParityTest`、`InBattleGateMatrixTest`（567 战中拦截）。
3. **Python（`xuanming-server-mmo-python`）**：
   - `build_protocol.py` + `test_season_v3_parity.py` 扩展为 V4 覆盖 `R1..R8`、`effect_id 4/5`、10 季循环与追忆流程 + 既有 `test_battle_dropin_parity.py` / `test_cpp_wt_wuxing_and_battle.py`。
4. **Unity（`mmorpg-client`）**：
   - `client_compile_check.ps1` 全量 0 错误 + `SeasonClientTests` 覆盖 `101..108` 共鸣飘字、10 季荣誉墙与追忆目标切换、结算页往季追忆增量展示。

---

## 7. 回滚与降级路径

1. **一键退回单季常开（零代码热更）**：将 `SeasonRule.xlsx` 中所有行的 `cycle_duration_days` 改回 `0`、仅保留 `S1 enabled=true`，热更后立即退回首测单季常开，玩家已解锁的永久徽章与追忆进度完整保留不丢失。
2. **单条共鸣紧急关闭（零代码热更）**：若某季某枚赛季印共鸣发现平衡问题，在 `SeasonSeal.xlsx` 将其 `resonance_kind` 置 `0` 并热更即可瞬间关闭该条共鸣，保留基础印效果继续运行。
3. **缺失徽章美术降级**：若 S3–S10 徽章 PNG 尚未导入，客户端 `SeasonV1UiArt.LoadHonorBadgeIcon` 自动回退默认徽章框/纯色占位且不抛异常，代码与表可先于美术完整联调上线。
