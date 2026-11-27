# 《五行奇谈》五行试炼志 V4：卷三·季游奇谈 S2–S10 全十季无缝轮换与技印共鸣解题

> 日期：2026-11-26  
> 上位红线：[project-vision.md](project-vision.md)（七条红线 + 反「上班」打卡 + 零拒审合规）  
> 前置基线：[long-term-progression-v2.md](long-term-progression-v2.md)、[trial-journal-v3-wild-bosses-vows-replay-share.md](trial-journal-v3-wild-bosses-vows-replay-share.md)、[season-ten-seasons-infinite-cycle-v4.md](season-ten-seasons-infinite-cycle-v4.md)  
> 配套 UI 交接单：[ui-handoff/trial-journal-v4-seasonal-badges-ui-handoff.md](ui-handoff/trial-journal-v4-seasonal-badges-ui-handoff.md)

---

## 1. 需求分析

### 1.1 为什么只做「卷三 S2–S10 全十季翻篇」而不硬扩其他大系统
1. **填平跨季空窗**：[season-ten-seasons-infinite-cycle-v4.md](season-ten-seasons-infinite-cycle-v4.md) 已在四仓完成 **S1–S10 十季差异化主题池、40 招免费赛季技、40 枚单槽赛季印与 8 种表驱动技印共鸣（`R1..R8`）** 以及无限期号自动轮换；但「五行试炼志」卷三目前仅配置了 **S1 破阵之季** 3 格（`301..303`）。一旦切换到 S2–S10，试炼志卷三会整卷空白。
2. **让「技印共鸣（`R1..R8`）」在试炼志里有专属解题反馈**：S1 人宠会武格仅检测通用破盾与抗控/施放赛季技（条件 `11/12`）；补齐通用的「施放当季免费赛季技（条件 `15`）」与「触发当季技印共鸣（条件 `16`，支持按季配置共鸣位掩码）」后，每一季都能把当季 4 招新技能与 `R1..R8` 共鸣自然变成卷三解题乐趣，而无需为 9 个赛季写任何硬编码。
3. **顺手修齐多季匹配与客户端展示硬伤**：
   - Go `match`（`catalog.go`）、Java `game-match`（`RankedTables.java`）与 Python `match`（`trial_cards.py`）原先把 `TrialJournalEntry` 唯一键定为 `(source_kind, difficulty, variant_key)`（未包含 `season_id`），多季同时配置时会启动失败或只匹配首行；
   - Unity `TrialJournalUiState.cs` 原先硬编码 `"破阵之季 · {ChapterName}"`，切入 S2–S10 后标题会错显为 S1。

### 1.2 五问准入自检（`project-vision.md §二`）
1. **普通零氪玩家（反「上班」一票否决）**：卷三 S2–S10 严格按 `season_id` 隔离在 `SeasonTrialJournalArchive`，零每日打卡、零定时在线、零活跃条；错过任意整季**绝不影响**永久卷 36 印与 M901「星阵宗师」（18 印）及 8 件法宝全免费集齐。
2. **个人开发者收入**：卷三每季里程碑仅产 2 份终身不可交易的「仅自用重铸砂」+ 1 枚零战力当季名片印戳 + 1 篇留灯院人物短评，恒产 0 可交易材料/银两，不冲击官方 7:3 模型。
3. **物价与长期循环**：每季 6 印鼓励玩家在正式人宠会武里尝试当季免费技能与 `R1..R8` 共鸣搭配，延长赛季重玩新鲜感，无任何通货膨胀口子。
4. **防骗与资产安全**：无新交易入口、无资金流变化，所有里程碑领取继续走既有 `client_request_id` 幂等账本与满包预检。
5. **商店与平台过审/防下架安全**：零新增付费/广告/外链/隐私项，零文案越界；首测默认 `SeasonRule` 仍为 S1 常开，零影响首测基线。

---

## 2. 设计分析

### 2.1 新增通用战法事实与判定条件（零硬编码赛季 ID）

1. **`proto/battle/battle_data.proto`（`BattleTrialJournalFacts` 仅追加 3 个可选字段）**：
   - `bool self_cast_season_skill = 27;` — 玩家本场直接施放过任意 `skill.season_id > 0` 的当季免费赛季技；
   - `bool self_triggered_season_resonance = 28;` — 玩家本场触发过任意 `R1..R8` 技印共鸣（子事件号 `101..108`）；
   - `uint32 self_season_resonance_mask = 29;` — 玩家本场触发过的共鸣类型位掩码（`resonance_kind ∈ 1..8` 置位 `1 << (kind - 1)`）。
2. **新增 2 个 `SealConditionType`（C++ / Java / Python 三端同构）**：
   - `15`（`kSeasonSkillCast`）：`(won || settlement.participated()) && facts.self_cast_season_skill()`；
   - `16`（`kSeasonResonance`）：`(won || settlement.participated()) && (condition_param == 0 ? facts.self_triggered_season_resonance() : ((facts.self_season_resonance_mask() & condition_param) != 0))`。
   - **零卡流派**：每季 `condition_param` 填该季全部 4 枚赛季印已配 `resonance_kind` 的并集位掩码（例如 S2 玄脉 `R1|R2|R5|R6 = 51`），确保玩家任选该季 4 枚印之一配合对应赛季技都能点亮，且精准拦截不匹配共鸣。

### 2.2 卷三 S2–S10 配表方案（严格向后追加，不改旧行与旧位）

- **ID 编码规范**（`s ∈ 2..10`，`base = 3000 + s * 10`，可扩展至 S11+）：
  - `TrialJournalEntry.xlsx` 新增 27 行（`3021..3103`）：每季 3 格（`base+1` 护卫救援 `source_kind=2/difficulty=1/variant=rescue`、`base+2` 物法易势 5V5 `source_kind=3/difficulty=0/variant=polarity_5v5`、`base+3` 人宠会武 `source_kind=4/difficulty=0/variant=ranked_pvp`），`chapter_id = 3`、`season_id = s`、`enabled = true`。
  - `TrialJournalSeal.xlsx` 新增 54 行（`entry_id * 10 + {1, 2}` = `30211..31032`）：
    - 护卫救援格：`9`（保住护卫胜出）+ 隔季交替 `3`（人宠无损）或 `2, param=6`（6 回合速援），免费 3×3 预设 100% 可达成；
    - 物法易势 5V5 格：`10`（有效胜出）+ 交替 `5`（物法双系直伤胜出）或 `3`（人宠无损胜出），免费 3×3 预设 100% 可达成；
    - 人宠会武格：`15`（施放当季免费赛季技）+ `16`（触发当季 `R1..R8` 技印共鸣，带当季并集掩码），紧扣每季 4 招技能与 4 枚赛季印。
  - `TrialJournalMilestone.xlsx` 新增 18 行（`3021..3102`，`scope_chapter_id = 3, season_id = s`）：
    - `base + 1`（`required_seals = 2`）：仅自用重铸砂 ×1；
    - `base + 2`（`required_seals = 4`）：仅自用重铸砂 ×1 + 零战力名片印戳 `badge_season_s{s}_trial` + 留灯院人物短评 `commentary_season_s{s}_trial`（由祝停云 / 阿禾 / 砚生 / 听雨轮笔）。

| 季 | 主题名 | Entry IDs | 人宠会武共鸣掩码（`condition_param`） | 4 印里程碑印戳（`badge_key` / 展示名） | 留灯院短评标题 |
|---|---|---|---|---|---|
| **S2** | 玄脉之季 | `3021..3023` | `51`（`R1/R2/R5/R6`） | `badge_season_s2_trial` · 玄脉季游 | 砚生·玄脉录 |
| **S3** | 离火之季 | `3031..3033` | `21`（`R1/R3/R5`） | `badge_season_s3_trial` · 离火季游 | 祝停云·离火跋 |
| **S4** | 青木之季 | `3041..3043` | `178`（`R2/R5/R6/R8`） | `badge_season_s4_trial` · 青木季游 | 阿禾·青木札 |
| **S5** | 玄水之季 | `3051..3053` | `75`（`R1/R2/R4/R7`） | `badge_season_s5_trial` · 玄水季游 | 听雨·玄水记 |
| **S6** | 厚土之季 | `3061..3063` | `165`（`R1/R3/R6/R8`） | `badge_season_s6_trial` · 厚土季游 | 祝停云·厚土跋 |
| **S7** | 庚金之季 | `3071..3073` | `42`（`R2/R4/R6`） | `badge_season_s7_trial` · 庚金季游 | 砚生·庚金评 |
| **S8** | 太阴之季 | `3081..3083` | `83`（`R1/R2/R5/R7`） | `badge_season_s8_trial` · 太阴季游 | 听雨·太阴录 |
| **S9** | 天雷之季 | `3091..3093` | `43`（`R1/R2/R4/R6`） | `badge_season_s9_trial` · 天雷季游 | 祝停云·天雷跋 |
| **S10** | 混元之季 | `3101..3103` | `23`（`R1/R2/R3/R5`） | `badge_season_s10_trial` · 混元季游 | 阿禾·混元合评 |

### 2.3 多季 `match` 试炼上下文动态解析（Go / Java / Python 同构）

- **唯一键升级**：`TrialJournalEntry` 在 `match` 的唯一索引由 `(source_kind, difficulty, variant_key)` 升级为 `(season_id, source_kind, difficulty, variant_key)`；永久格（`season_id == 0`）仍按 `Dungeon.id` 静态映射，赛季格（`season_id > 0`）按 `season_id` 分桶存放。
- **确定性活跃季解析**：`match` 同步载入 `SeasonRule` 表，使用与 C++/Java/Python `SeasonRules.ResolveActiveSeason(rules, nowMs)` 逐位一致的纯函数（兼容固定单季窗与无限自动循环池）解析 `activeSeasonId`：
  - `PVE_SOLO / PVE_TEAM / POLARITY_5V5`：优先命中永久格；未命中时按 `activeSeasonId` 查当季 `Dungeon 109 / 110` 赛季格（无活跃季返回 `nil`）。
  - `PVP_TEAM`：当 `SeasonRule` 已载入时按 `activeSeasonId` 查当季 `ranked_pvp` 格；若 `rankedCtx.season_id != 0` 且与 `activeSeasonId` 不一致（固定窗跨季错配）返回 `nil`；测试构造未载入 `SeasonRule` 时回退按 `rankedCtx.season_id` 精确匹配，保持所有既有单测语义不变。

### 2.4 客户端动态季名与印戳平滑兜底
- `TrialJournalUiState.cs`：移除硬编码 `"破阵之季"`，优先从 `SeasonRuleTableManager` 按 `seasonChapter.SeasonId` 查当季正式名称（表未加载时按 1..10 静态名称表回退），切入 S2–S10 自动展示 `"玄脉之季 · 卷三 · 季游奇谈"` 等标题。
- `SeasonEconomyUiArt.LoadTrialJournalBadge`：优先加载 `UI/Ugui/TrialJournal/Badges/<badge_key>`；在 Codex 专属试炼印戳图尚未落库前，对 `badge_season_s2_trial..badge_season_s10_trial` 自动回退展示已验收落库的 `UI/SeasonHonors/honor_s2_xuanmai..honor_s10_hunyuan`（零空白图标，落图后零代码自动切专属图）。

---

## 3. 思想分析
1. **为什么坚持「表驱动共鸣掩码 + 按 `season_id` 独立隔离」，而不是在业务代码里写 `if (season == 2)`？**  
   未来开到 S11/S12 时只需往 `SeasonRule / SeasonSeal / TrialJournal*` 追加表行，C++/Go/Java/Python/Unity 五端零改代码自动支持，彻底消除重复维护成本。
2. **为什么护卫救援与物法易势不放条件 `15/16`（赛季技/共鸣），而把 `15/16` 聚焦在正式人宠会武？**  
   护卫救援（`Dungeon 109`）与物法易势 5V5（`Dungeon 110`）走公平 3×3 免费预设（不带玩家赛季技与赛季印），若把 `15/16` 填在免费预设格会导致无法点亮；正式人宠会武携带全部 4 招当季免费技能与单槽赛季印，刚好与 `15/16` 100% 契合。

---

## 4. 漏洞分析

| 潜在漏洞 | 触发场景 | 防护方案 |
|---|---|---|
| 多季配表导致 `match` 启服撞重复键 | `TrialJournalEntry` 同时启用 S1–S10 的 `rescue`/`polarity_5v5`/`ranked_pvp` | Go `catalog.go`、Java `RankedTables.java`、Python `trial_cards.py` 均将唯一键扩为 `(season_id, source_kind, difficulty, variant_key)`；同季内重复仍 fail-closed 拒启 |
| 赛季切换瞬间迟到结算污染新旧季档 | S1 末开局、S2 生效后才收到结算 | `CreateBattleRequest.trial_context` 冻结开局 `season_id` 与 `journal_entry_id`；`Scene` 结算仍强校验 `facts.season_id() == entry.season_id() && activeSeasonId == entry.season_id()`，跨季迟到安全跳过 |
| 赛季印记污染永久位图或影响 M901 | 新增 54 枚赛季印与 18 档赛季里程碑 | 所有新行 `season_id ∈ 2..10` 且向后追加在 Excel 末尾：永久位图旧索引 `0..41` / `0..12` 零变动，运行时全走 `SeasonTrialJournalArchive`（`lit_seal_ids` / `claimed_milestone_ids`），`total_permanent_seals` 恒定维持 36、M901 恒定 18 |
| 免费预设格误配赛季技/共鸣条件导致死锁 | 玩家在护卫救援/物法易势无法触发共鸣 | 仅 `source_kind = 4`（正式人宠会武）使用条件 `15/16`；护卫救援与物法易势严格使用免费预设可达条件（`2/3/5/9/10`） |
| 客户端在专属印戳未到图时显示白框 | 切换到 S2–S10 查看里程碑 | `SeasonEconomyUiArt.LoadTrialJournalBadge` 优先查专属路径，缺图平滑回退至已验收的 10 季荣誉徽章 `UI/SeasonHonors/honor_s2..s10` |

---

## 5. 步骤分析

1. **阶段 A · 设计与作图交接落盘**：落盘本策划案与 [ui-handoff/trial-journal-v4-seasonal-badges-ui-handoff.md](ui-handoff/trial-journal-v4-seasonal-badges-ui-handoff.md)，执行 `gen_docs_index.py`。
2. **阶段 B · 协议与配表生成**：`battle_data.proto` 追加字段 `27..29`；向 `TrialJournalEntry/Seal/Milestone.xlsx` 追加 S2–S10 行；运行导表与 `proto-gen-run`。
3. **阶段 C · C++ 战斗/场景与 Go `match` 权威实现**：接入 `15/16` 条件、共鸣埋点与 Go `Catalog` 多季 `SeasonRule` 解析；跑 C++ 编译、`turn_battle_engine_test`、`bag_test`、`scene --validate-tables` 与 Go `match` 全包测试。
4. **阶段 D · Unity 客户端同步与适配**：执行客户端表/协议同步，修复 `TrialJournalUiState` 动态季名与 `SeasonEconomyUiArt` 印戳回退，补齐 `TrialJournalClientTests` 并过全仓编译与单测。
5. **阶段 E · Java 21 与 Python 服务端对齐**：`ContractSync` + Java 战斗引擎/场景/匹配对齐与 Maven 回归；Python 协议/配表同步与战斗/场景/匹配对齐及 `pytest` 回归；更新 `PARITY.md`。

---

## 6. 执行步骤

1. 落盘设计文档、UI 交接单并更新 `docs/README.md`。
2. 编辑 `proto/battle/battle_data.proto` 与 3 张 `TrialJournal*.xlsx`（严格追加），执行导表与 `proto-gen-run`。
3. 完成 C++ `trial_journal_rules.h` / `turn_battle_engine` / `configured_battle_effects` / `equipment_battle_effects` 与 Go `catalog.go` 改动及单测，执行编译与实跑。
4. 在 `mmorpg-client` 同步表与协议，改动 `TrialJournalModels.cs` / `TrialJournalUiState.cs` / `SeasonEconomyUiArt.cs` / `TrialJournalClientTests.cs`，跑编译检查与 NUnit 单测。
5. 在 `xuanming-server-mmo`（Java）执行 `ContractSync`、对齐引擎/场景/匹配代码并跑通 Maven 测试，更新 `PARITY.md`。
6. 在 `xuanming-server-mmo-python` 同步协议与配表、对齐战斗/场景/匹配代码并跑通 `pytest`。
