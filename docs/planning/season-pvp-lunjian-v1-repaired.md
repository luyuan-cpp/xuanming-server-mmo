# 《论剑 / 人宠会武》PVP 玩法设计体检、修复方案与 UI 补全清单（V1 修复版）

> 日期：2026-11-24（2026-11-25 同步最新二阶段周上限 `150` 份；V2 体验重做与正式落地口径请继续参见 [ranked-play-v2-overhaul.md](ranked-play-v2-overhaul.md) 与 [season-pvp-pve-economy-unified-v1.md](season-pvp-pve-economy-unified-v1.md) §6）
> 定位：针对早期《论剑排位首发 V1》（[ranked-play-season-ui-candidate.md](ranked-play-season-ui-candidate.md)）及现役已落码实现的全链路体检与修复方案。  
> 上位红线：[项目设定](project-vision.md) · [核心循环 V1](core-gameplay-loop.md) · [经济总纲](equipment-economy-charter.md) · [赛季与经济统一落地设计](season-pvp-pve-economy-unified-v1.md) · [赛季技能与单槽赛季印](season-skills-and-seal-design.md)  
> 说明：本文档仅落盘修复设计与对账结论，不覆盖工作区并行会话的未提交改动（待负责人拍板文末待定项后再按步骤合入代码与刷新 `docs/README.md` 索引）。

---

## 一、需求分析

### 1.1 本轮审计对象与核心矛盾
负责人希望确认早期《论剑排位首发 V1》玩法设计是否存在问题、修复设计漏洞，并核查 `mmorpg-client` 当前 UI 是否还需补全。

结合仓库现役上位文档与双端代码核对，早期《论剑排位首发 V1》稿件（[ranked-play-season-ui-candidate.md](ranked-play-season-ui-candidate.md)）存在 **4 处与最新上位总纲直接冲突的结构性缺陷**；同时，即便在 2026-11-24 已拍板并落码 C++/Go 服务端的新版《人宠会武》架构下，仍残留 **4 个真实玩法/逻辑漏洞** 与 **6 项客户端 UI 断层**。

### 1.2 早期《论剑排位首发 V1》的 4 个根部设计问题（已被 2026-11-24 总纲推翻）

| 编号 | 旧稿原设计 | 为什么必须推翻 | 仓库最新权威口径（已落码 C++/Go） |
|---|---|---|---|
| **D1** | 常驻正式排位 `PVP_TEAM` 全员改用 6 套免费 3×3 预设，装备/宝宝/技能全不生效 | 彻底掏空八部位装备、词条、双法宝、自由技能、20 点宝宝条目与官方 7:3 商业化闭环，养成沦为纯单机刷怪 | 正式「人宠会武（`PVP_TEAM`）」固定走**本人真实养成**（`UseFreePreset=false`, `UsesOwnProgression=true`）；6 套免费 3×3 预设仅用于不计分的娱乐轮换（护卫救援 `109` / 物法易势 5V5 `110`） |
| **D2** | 开服直接全开 1V1 / 2V2 / 3V3 / 4V4 / 5V5 五档常驻匹配且不补机器人 | 个人开发冷启动在线少，5 个队列互相抽血，2V2/4V4/5V5 长期排不到人，形成“鬼服”体感 | 协议与配表保留 1V1–5V5 开关（`PlayMode.allowed_team_sizes`）；常驻主力 `1V1/3V3/5V5`，**冷启动仅开 `[1, 3]` 聚人保秒排**；`2V2/4V4` 划归帮会/主题特殊玩法开关 |
| **D3** | 每日共享 20 份、05:00 清零；PVP 只要完成 1 回合无论胜负平都给 10 份 | ① 每日清零制造打卡焦虑，违背“真不肝”；② 1 回合自动送死拿 10 份效率反超 6 分钟首领试炼，必然滋生送死脚本毁掉匹配环境 | 改为**两阶段总账**：阶段一**永久 660 份无日清**（单账号终身至多 60 份可交易且**仅限胜局/PVE 通关**）→ 阶段二**每周 150 份全绑定**（周一 05:00 重置，配合 `+11` 20 次硬保底）；PVP 胜 +15、平/负强制四重防挂机（未逃离 + `≥3 回合` + `≥2 手动回合` + 有效战斗贡献）才给 +10 全绑定 |
| **D4** | 小氪仅挂 10 元易容卡，禁止其余合规消费占位 | 现役可买形象总量封顶几十元，个人开发者无法覆盖长期研发与合规运维成本 | 坚持“不卖独占数值、免费同战力上限”，主城与养成线预留 4 个低打扰非数值/省时间合规入口（衣柜外观、赛季外观手记、轻便利月卡与配装槽 3→6、官方限量绑定提速包/聚宝斋次级来源） |

---

## 二、设计分析（修复后的《论剑 / 人宠会武》V1 完整玩法规则）

### 2.1 赛制分工与低人口聚人结构（零冲突单一真相）
1. **常驻正式「人宠会武（`PVP_TEAM`, `battle_config_id=5`）」**：
   - **构筑来源**：本人真实角色等级、八部位装备（+10 必成 / +11 20 次保底）、固定词条、双法宝、常驻与赛季技能、单槽赛季印、出战宝宝及 20 点免费条目。
   - **战前资产隔离**：局内满血蓝起手、禁用真实消耗品与捕捉、战后绝不回写场外血蓝/道具/宠物损失。
   - **人数档开关**：底层支持 1V1–5V5；冷启动 `PlayMode.allowed_team_sizes = [1, 3]`；五档共用 1 条 Elo、1 张赛季榜、1 份 4/8/12 场外观进度；4 人队可打 4V4（当 4V4 开放时）、禁排 5V5。
   - **120 秒低人口兜底**：实时显示已等待时长与分差（`<30s: ±100 / 30..90s: ±200 / >=90s: ±300` 硬上限）；满 120 秒弹出「继续等待」与「取消并转单人首领试炼」（确认撤销后切换；若已成局则直接进原局；绝不塞机器人）。
2. **娱乐轮换场（不计正式 Elo、不入正式榜、免费 3×3 人宠）**：
   - 前 4 周：**护卫救援（PVE `Dungeon 109`, 1–5 人）**；
   - 后 4 周：**物法易势 5V5（PVP `MATCH_MODE_POLARITY_5V5=8`, `Dungeon 110`, 仅 5V5 公平团战）**；
   - 仅这两项入口启用 `UseFreePreset = true` 与 6 套 `RankedPreset`（101–103 / 201–203）。
3. **跨区高光舞台**：每季配套《群仙问道赛》（`MATCH_MODE_SEASON_TOURNAMENT=11`，3 人出战 + 1 替补，7 天宽窗海选 + 1 周末单败淘汰 + L2 全服观战）。

### 2.2 针对现役 4 个真实遗留玩法漏洞的精准修复方案

| 遗留漏洞 | 现役代码实锤位置 | 修复规则（建议首测落地值） |
|---|---|---|
| **F1. 新手自养入场断层 & 无出战宠 1 打 2** | `go/match/internal/ratingrule` 初分 `1500.00`；`player_battle.cpp:1518` 正式会武无准入/出战宠校验 | ① **战前出战宠防呆**：未设出战宠（`ActivePetId==0`）时，客户端明确拦截提示“尚未设置出战灵宠，将少 1 个战斗单位”并提供一键跳转宠物页（允许玩家确认“仍单人出战”或先设出战宠）；<br>② **新手保护与过渡（待拍板二选一）**：<br>  - **方案 A（最简最小改动）**：新号仍用本人养成，前 5 场定级保护匹配低分段/新号优先，并在大厅提示「建议先通 1 次普通首领试炼穿戴基础装再进会武」；<br>  - **方案 B（更平滑，对齐 `gameplay-unified-design-v1.md`）**：允许低养成新号（如未穿满 4 件基础装或无宠）在正式会武可选勾选「新手入门借用套」（强度锚定同级 +0~+2 基础过渡，显著低于满配 +10 避免掏空养成，但防 2 回合暴毙）。 |
| **F2. 正式会武 `Dungeon 5` 回合上限回落为 120 回合** | `generated/tables/dungeon.json` `id=5` 为 `round_limit=0, time_limit=720`；`turn_battle_engine.cpp:256` 回落 `720/6=120` 回合 | 为 `Dungeon.id = 5` 显式配置 **`round_limit = 20`**（同时保留 `time_limit = 720` 秒硬超时兜底）；若实测 3V3 偏紧，可表调至 `25` 回合；第 16 回合起可选加温和「战意渐盛」（每回合双方治疗/护盾效果 -10% 或伤害 +10%）防双奶回生印拖平。 |
| **F3. 赛季 4/8/12 场参与口径与材料防挂机口径脱节** | `ranked.go:803` 与 `turn_battle_engine.cpp:4150` 允许 `>=1 回合 + 进房开自动` 拿赛季场次；`player_equipment_material_reward.cpp:80` 要求 `>=3 回合 + >=2 手动 + 有效贡献` | **统一反摆烂口径**：<br>- **胜局**：沿用 `>=1 回合 && !fled && entered_room`（正常 1–2 回合速胜不误伤）；<br>- **负局 / 平局**：赛季有效场次与亮段资格改为与材料一致的**最低有效对抗门槛**（`!fled && effective_combat_contribution && (total_rounds >= 3 && manual_action_rounds >= 2 || no_action_opportunity)`），彻底堵死“1 回合开自动送死白嫖青云玉符 + 3V3 坑队友”。 |
| **F4. 多人局掉线硬耗 & 连续战斗开关串票** | 禁逃跑无多人投降；`BattleClient.cs:985` `AckBattleEnd()` 对排位也触发 `ContinuousBattle` 自动重排；`BattleUiStyle.cs:28` 旧入口传 `configId=1` 必败 | ① **代码即刻可修**：`BattleClient.AckBattleEnd()` 对正式会武/PVP 强制禁止 `ContinuousBattle` 自动重排（必须由玩家显式点「同规模再排」）；修正 `BattleUiStyle.Pvp1V1BattleConfigId = 5` 或在旧 `BattleQueuePanel` 直接跳新会武大厅；补齐 `RankedClient.BeginSettlementFromRecovery` 的 `MaterialTradableUnitsAdded` 与 `RankedModels.UsesOwnProgression` 布尔判断；删除 `sql_launch.go:324` 死函数 `checkRankedPresets`。<br>② **3V3/5V5 投降预留（二期/待拍板）**：1V1 保持禁逃跑；3V3/5V5 建议在第 5 回合后开放「全队同意投降」（1 人掉线超 2 回合可视为弃权同意），减少垃圾时间。 |

### 2.3 与《项目设定》上位红线四问自检
1. **普通零氪玩家**：每天 10–30 分钟或隔周上线，单人可打 3 首领试炼、真人可排 1V1/3V3，永久 660 首套无日清，不建小号可 100% 拿满同战力上限与 4/8/12 永久展示外观（青云玉符）。**通过**。
2. **个人开发者收入**：正式会武回归真实养成，直接承接官方 7:3 商业化底座（永久外观、赛季手记、轻享卡配装槽与搬砖门票、限量绑定提速包与 `+11` 温和精进）。**通过**。
3. **物价与长期循环**：PVP 负/平 0 可交易且有四重防挂机门槛，首套可交易终身按账号 60 份封顶，阶段二周 150 全绑定；不会给零成本脚本留刷料口子。**通过**。
4. **防骗与资产安全**：竞技资产局内隔离不损实装、无站外交易引流、无赌博/保分卡/付费数值。**通过**。

---

## 三、思想分析

1. **为什么必须把「正式会武 = 真实养成」和「娱乐轮换 = 免费预设」一刀切清？**  
   回合制 MMO 的长线留存依赖“局外配装研究 → 局内实战验证 → 战后复盘调配”的完整正反馈。把免费公平预设放在不计分的娱乐轮换（护卫救援 / 物法易势 5V5），既保住了零门槛尝鲜和 9 种人宠组合乐趣，又把八部位装备、双法宝、赛季印和 20 点宝宝条目全部盘活在正式人宠会武与群仙问道赛里。
2. **为什么必须统一“材料防挂机”与“赛季 12 场参与”的负局口径？**  
   任何不对称的门槛都会变成脚本最优解——如果材料拦住了 1 回合自动送死，但 4/8/12 外观和亮段没拦住，玩家为了拿 12 场青云玉符就会在 3V3 里进场点自动挂机，毁掉 5 名正常玩家的对局体验。
3. **为什么客户端 UI 必须把「旧论剑 47 图美术/抽屉资产」与「新核心循环自养会武」合二为一，而不是各挂一套？**  
   旧 `RankedUiRoot` 有 47 张精绘道家素材和完整的排行榜/赛季记录/青云玉符抽屉，但挂的是废弃规则；新 `FormalPvpLobbyPanel` 规则是对的，却暂时缺了排行榜、赛季记录、段位卡、青云玉符和 0–119 秒排队取消条。合并入口、保留精品美术与抽屉、对齐最新服务端契约，是最低维护成本的唯一正解。

---

## 四、漏洞分析与防线清单

| 风险 / 漏洞点 | 触发路径 | 防线 / 修复动作 |
|---|---|---|
| **客户端新旧两套入口割裂导致抽屉失联** | `RankedUiRoot.OpenLobby()` 跳转 `FormalPvpLobbyPanel` 后，旧 `RankedLobbyPanel` 不可达，`RankedSharedDrawer`（排行榜/赛季记录/青云玉符）无入口 | 在 `FormalPvpLobbyPanel` 顶栏/左侧接入段位卡、4/8/12 进度条及打开 `RankedSharedDrawer` 三页签的按钮 |
| **正式会武排队 0–119 秒无法取消** | `FormalPvpLobbyPanel` 仅在 `QueuedSeconds >= 120` 显示 `QueuePrompt` 与取消按钮，`<120s` 时主按钮置灰 | 排队态（`BattlePhase.Queued`）全程原位显示真实等待秒数、当前分差、`取消匹配` 按钮及 `取消中…` 状态；满 120s 追加「取消并转单人首领试炼」 |
| **现场排位结算隐藏了赛季场次与新外观解锁** | `RankedSettlementPanel.SetCoreSettlementVisible(true)` 藏掉了全部 3 张 `_legacyRewardCards` | 仅用 `CoreSettlementRewardSection` 替换第 3 张材料卡，保留 Card 1（有效场次）与 Card 2（新解锁展示外观）；同步修正重登补弹路径的旧 Card 3 文案为两阶段 660 / 周 150 口径 |
| **排位结算点“返回”被旧 `ContinuousBattle` 偷偷重排** | 曾在旧 `BattleQueuePanel` 开启连续战斗 → 打完论剑点返回 → `AckBattleEnd()` 自动 `JoinQueue` | `BattleClient.AckBattleEnd()` 对 `PVP_TEAM` / `POLARITY_5V5` / `SEASON_TOURNAMENT` 禁用自动重排；旧 `BattleQueuePanel` 收口 PVP 入口并对齐 `battle_config_id=5` |
| **重登补弹丢可交易材料数字 / `UsesOwnProgression` 恒真** | `RankedClient.BeginSettlementFromRecovery` 未复制 `result.MaterialTradableUnitsAdded`；`RankedModels.cs:182` 多写了 `|| CurrentSeason.SeasonId != 0` | 补齐字段复制，并将 `UsesOwnProgression` 改为 `CurrentSeason == null || CurrentSeason.UsesOwnProgression` |
| **Java 独立服与 C++/Go 权威服行为分裂** | `xuanming-server-mmo` 仍停留在旧版免费预设 + 每日 20 份阶段 | 按 `PARITY.md` 第 13 行优先完成 `ContractSync` 与 Java `game-match` / `game-scene` / `game-battle-engine` 同步 |

---

## 五、步骤分析（先收口设计与契约，再补齐服务端/客户端/UI 缺口）

1. **步骤 1（负责人拍板 3 项待定微调）**：确认新手入门保护（方案 A vs 方案 B）、正式会武 `Dungeon 5` 显式回合上限（推荐 `20` 回合 + 16 回合起减疗）、3V3/5V5 是否二期补全队投降。
2. **步骤 2（服务端小幅修补与双服对齐）**：
   - C++/Go 权威仓：`Dungeon.xlsx` 给 `id=5` 补 `round_limit = 20`；对齐 `ranked.go` 与 `turn_battle_engine.cpp` 的负局有效参与门槛；清理 `sql_launch.go:324` 死代码 `checkRankedPresets`。
   - Java 独立仓：按 `PARITY.md` 同步 `use_free_preset` 解耦、`allowed_team_sizes`、`+11` 保底与两阶段 660/周 150 材料账。
3. **步骤 3（客户端纯 C# 层 4 处精确修复）**：
   - 修 `BattleClient.AckBattleEnd()` 排除排位/PVP 连续战斗自动重排；
   - 修 `RankedClient.BeginSettlementFromRecovery` 补传 `MaterialTradableUnitsAdded`；
   - 修 `RankedModels.UsesOwnProgression` 表达式；
   - 修 `BattleUiStyle.Pvp1V1BattleConfigId` / `BattleQueuePanel` 与 `PlayMode` 对齐。
4. **步骤 4（Unity UI 补全，交由 UI Codex / 并行 UI 线完成后收尾）**：按第六节 UI 补全清单逐项闭环 P0 → P1 → P2，并完成安全区/小游戏胶囊避让与真机联机验收。

---

## 六、执行步骤与 UI 补全检查清单（对应 2026-11-24 23:10 客户端实盘）

> 注：本机另一并行会话已新建并接入 `Assets/Scripts/UI/Ugui/CoreLoop/`（`CoreGameplayUiRoot`、`CoreMainCityHubPanel`、`BossTrialPrepPanel`、`FormalPvpLobbyPanel`、`SeasonRotationPanel`、`SeasonSealPanel`、`BattleCoreHudOverlay`、`CoreSettlementRewardSection`）及 `BattleUnitView` 角标。以下清单剔除了已完成项，只列**实测仍缺失或断裂的补全项**：

### P0（阻塞闭环，必须补全）
1. **恢复排行榜 / 赛季记录 / 青云玉符抽屉可达性**：
   - 在 `FormalPvpLobbyPanel` 增加顶部/侧边入口：当前段位徽章与积分、本季 4/8/12 场进度、「排行榜」「赛季记录」「已获外观（玉符）」「易容形象」按钮，直连 `RankedUiRoot` 既有 `RankedSharedDrawer`（47 图美术全部复用不浪费）。
2. **补齐 `FormalPvpLobbyPanel` 0–119 秒排队与取消状态条**：
   - `BattlePhase.Queued` 期间全程展示真实已等待时长、当前匹配分差、`取消匹配` 按钮与 `取消中…` 确认态；满 120 秒弹出「继续等待 / 取消并去单人首领试炼」。
   - 修复 `FormalPvpLobbyPanel.cs:125` 出战宝宝「查看」按钮误跳装备窗，改为跳转灵宠窗口；未设出战宠时显示明确提醒。
3. **修复 `RankedSettlementPanel` 隐藏场次/外观卡 & 重登补弹旧文案**：
   - 现场结算嵌入 `CoreSettlementRewardSection` 时，**只隐藏 Card 3（旧材料卡）**，保留 Card 1（本季有效场次）与 Card 2（新取得永久展示外观）；
   - 重登补弹路径同步切换为两阶段 660 / 周 150 文案（删掉 `"今日共享材料自选额度已达 20 份上限"`），并展示可交易份数与反挂机未达标原因。
4. **补齐 `SeasonRotationPanel` 排队态、招式详情与服务端周次**：
   - 点击 6 张预设卡可展开查看完整技能名、分类与说明；
   - 增加「物法易势 5V5」排队计时 / 取消 / 120s 兜底状态条；
   - 替换 `BuildRotations()` 写死的默认 `activeSeasonWeek = 1u`，改为读取服务端赛季当前时段/周次。
5. **收口旧 `BattleQueuePanel` 与 `ContinuousBattle`**：
   - 旧战斗面板不再对 `PVP_TEAM` 发 `battle_config_id=1`；排位/PVP 禁用旧连续战斗自动重排。

### P1（核心体验与合规，强烈建议补齐）
6. **战斗内操作细节（`BattleScreen.cs` & `BattleUnitView.cs`）**：
   - 宝宝技能面板加**「主动攻击 / 障碍 / 辅助」显式分类页签或分组头**，并只读展示条件追击/被动说明（不可点施放）；
   - 技能按钮补齐**法力不足 / 被封法 / 被封物 / 受控**的禁用状态与可读提示（当前仅判冷却）；
   - 强化“指令提交中 / 服务端已接受 / 改选生效”状态反馈；
   - `BattleCoreHudOverlay` 支持同时标注多条预告目标（如重劈目标 + 回春护卫同时存在），单位角标加清晰图形/短字（如「重劈」「保全」）而不止靠红/金圆点区分。
7. **安全区、本地时间与抽屉文案同步**：
   - `RankedUiRoot` 补齐 `Screen.safeArea` 与微信/抖音小游戏右上角胶囊避让（同 `CoreGameplayUiRoot.ApplySafeArea`）；
   - 大厅与赛季记录将 `StartMs / EndMs` 转玩家本地时间展示；
   - `RankedSharedDrawer` 清理写死的 `"五档人数共用"` 文案并改用玩家真实形象做名片预览。

### P2（关联玩法独立页面）
8. **五行试炼志（`TrialJournalClient`）独立 UGUI 面板**：展示三卷章节、通关印记、里程碑领奖与 8 法宝券兑换（当前 `CoreGameplayUiRoot.OpenTrialJournal` 临时复用 `SeasonSealPanel`）。
9. **《群仙问道赛》战队锦标赛（`TournamentClient`）UGUI 窗口**：接入已完成的 RPC 465–475（建队/邀请/报名锁定/海选积分/淘汰树/免费助威/领奖）。
