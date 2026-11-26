# 五行奇谈 · 三大核心玩法扩展统一设计（V1）

> 状态：设计定稿，进入服务端（C++/Go）与客户端（Unity）全链路实现。
> 适用范围：`xuanming-server-mmo-cpp`（权威 C++ Scene/Battle + Go Match）、`mmorpg-client`（Unity UGUI 与纯 C# 模型层）、`D:/work/image/designs/wuxing-expansions-ui-20261125`（Codex 纯美术交付目录）。
> 关联设计：[season-pvp-pve-economy-unified-v1.md](season-pvp-pve-economy-unified-v1.md)、[season-skills-and-seal-design.md](season-skills-and-seal-design.md)、[battle-report-replay.md](../design/battle-report-replay.md)。
> Java 版对账状态：待同步（本批先完成 C++/Go 权威服务端与 Unity 客户端，并在 Java 仓 `PARITY.md` 记账）。

---

## 一、需求分析

当前《五行奇谈》已完成「首领试炼（普通/困难/V2 双护卫）× 8 周轮换娱乐 × 正式人宠会武 1V1–5V5 × +11 保底强化 × 两阶段 660/100 材料总账」的核心闭环，但在长线体验上存在三个明显可拉升点：

1. **PVE 长线重玩度不足**：固定机制的首领试炼通关熟练后套路趋同，缺少「每局流派不同、低负担高爽感」的周常可重玩 PVE 内容。
2. **「五行」与「人宠」主题深度未在阵前与回合内形成强策略闭环**：目前人宠并肩出战但缺少阵法博弈与五行相生联动，单人与组队站位决策维度偏单薄。
3. **社交切磋与抄作业链路偏单薄**：现有 `ChallengePlayer` 仅支持固定默认规则私密 1V1 切磋，无法按帮会水友赛或测试流派需要开启「公平 3×3 模板 / 物法易势 / 8 回合快节奏 / 禁控 / 公开观战」，且战报回放仅能重播动画，无法一键查看对手脱敏公开构筑与收藏套路。

**统一底线约束**：
- **零卖数值红线**：5 套五行阵法全免费解锁、15 枚幻境符印仅本轮爬塔生效出塔清空、自定义切磋零排位分且不刷材料额度、配装收藏纯策略便利，绝不引入额外数值付费或每日打卡。
- **不打破既有 660/100 经济总账**：五行幻境周常首通里程碑奖励复用现有两阶段材料总账与赛季印进度口径，不额外超发可交易材料。
- **双端平台硬约束**：客户端全纯 C# 模型 + 主线程事件/协程，严格兼容微信/抖音小游戏（WebGL2 单线程）与 iOS/Android App（IL2CPP/AOT）。

---

## 二、设计分析

### 模块 A：五行阵法 × 人宠相生合击

1. **五套免费道家阵法（`WuxingFormationId = 1..5`，`0` 表示默认/无阵）**
   - 玩家在战外免费一键切换，持久化于 `PlayerSeasonComp.equipped_formation_id`，通过 `SceneSeasonClientPlayer.EquipWuxingFormation` 修改。
   - 阵法属性与克制环（`1 > 2 > 3 > 4 > 5 > 1`，克制方获得定点数 **+5% 阵法克制直伤**，被克方不倒扣基础属性）：

| 阵法 ID | 名称 | 五行属性 | 队伍常驻增益（全队人物与灵宠生效） | 克制阵法 |
|---|---|---|---|---|
| 1 | 天覆阵 | 金 | 直伤 +5%、速度 +2% | 克 2 地载阵 |
| 2 | 地载阵 | 土 | 受击减伤 +6%、开局护盾 +4% 最大气血 | 克 3 风扬阵 |
| 3 | 风扬阵 | 木 | 速度 +6%、治疗与护盾效果 +4% | 克 4 云垂阵 |
| 4 | 云垂阵 | 水 | 控制抗性 +8%、每回合初回复 3% 法力 | 克 5 虎翼阵 |
| 5 | 虎翼阵 | 火 | 暴击率 +4%、出战灵宠全伤害 +8% | 克 1 天覆阵 |

2. **组队阵法冻结口径**
   - 单人局取本人 `equipped_formation_id`；组队局取本阵营 `formation_slot` 最小的参战玩家（即队长/1 号位）的 `equipped_formation_id`。
   - 开局时 `TurnBattleEngine::Initialize` 冻结双方阵法至 `BattleStateS2C.team_a_formation_id` / `team_b_formation_id`（对手与观众可见）。
3. **人宠五行相生合击**
   - 五行枚举：`1=金, 2=木, 3=水, 4=火, 5=土`；相生关系：`金(1)→水(3)→木(2)→火(4)→土(5)→金(1)`。
   - 角色五行由 `class_id` 确定性映射（`((class_id - 1) % 5) + 1`，`class_id == 0` 视作无五行）；灵宠五行由 `pet_table_id` 确定性映射（`((pet_table_id - 1) % 5) + 1`）。
   - 当存活主角与本人存活出战灵宠满足**互为相生**（人克/生宠？严格为**相生**：人生宠或宠生人皆触发，鼓励双向搭配），每逢第 **3、6、9、… 回合**（`roundIndex % 3 == 0`）回合初激活「五行相生合击」：该回合内该主人与灵宠**直伤 +10%、治疗与护盾 +10%**，并向事件流写入 `BATTLE_EVENT_WUXING_SYNERGY` 供客户端演出与 HUD 播报。

---

### 模块 B：五行幻境 · 轻肉鸽周常爬塔（10 层）

1. **周常周期与状态持久（零新表，位图存储遵循 `AGENTS.md §13`）**
   - 复用 `PlayerSeasonComp.wuxing_tower`（已持久化于 `player_database` `GROWTH` 组）。
   - 核心字段：
     - `week_key`（UTC+8 周一 05:00 换周，跨周自动重置周位图与本周最快记录，不保留过期在途轮次）；
     - `run_active`、`run_id`（单调递增，用于防重放与确定性抽符种子）、`current_floor`（`1..10`）、`run_total_rounds`（本轮累计回合数）；
     - `active_sigil_ids`（本轮已选符印，至多 9 枚，出塔或重开立即清空）；
     - `pending_offer_sigil_ids`（通关第 `k` 层后服务端生成的 3 枚待选符印，未三选一前禁止进第 `k+1` 层）；
     - `weekly_cleared_mask`（10 位位图：`bit (floor - 1)` 标示本周首通该层）；
     - `weekly_best_floor`、`weekly_best_rounds`（本周个人最佳成绩，通关 10 层且总回合数更短时写入场景内周榜内存快照 Top 20，展示玩家名、最佳回合、阵法与所选符印流派）。
2. **15 枚局内五行符印（5 行 × 3 档：凡品 1 / 灵品 2 / 仙品 3）**

| ID | 名称 | 五行 | 品质 | 局内确定性效果（仅五行幻境生效，出塔清空） |
|---|---|---|---|---|
| 101 | 金锋符 | 金 | 凡 | 全直伤 +8% |
| 102 | 破甲金印 | 金 | 灵 | 暴击率 +12%，破盾伤害额外 +25% |
| 103 | 庚金剑气 | 金 | 仙 | 全直伤 +14%，暴击伤害额外 +20% |
| 201 | 青木润灵 | 木 | 凡 | 治疗与护盾效果 +12% |
| 202 | 灵宠共鸣 | 木 | 灵 | 出战灵宠全伤害 +18% |
| 203 | 建木生息 | 木 | 仙 | 回合初回复 5% 最大气血，出战灵宠全伤害 +22% |
| 301 | 沧澜回气 | 水 | 凡 | 回合初回复 8% 最大法力 |
| 302 | 玄冰护心 | 水 | 灵 | 控制抗性 +15%，受直接伤害回复 4% 法力 |
| 303 | 弱水涤尘 | 水 | 仙 | 受击减伤 8%，回合初回复 10% 法力，控制抗性 +20% |
| 401 | 离火焚势 | 火 | 凡 | 前 3 回合全直伤 +14% |
| 402 | 炎阳暴烈 | 火 | 灵 | 单体主目标伤害额外 +12% |
| 403 | 祝融烈焰 | 火 | 仙 | 全直伤 +16%，暴击附加目标最大气血 3% 灼伤（单段封顶 1500） |
| 501 | 厚土筑垒 | 土 | 凡 | 开局获得 12% 最大气血护盾 |
| 502 | 坤元反震 | 土 | 灵 | 受直接攻击反震 12% 实际伤害 |
| 503 | 昆仑镇岳 | 土 | 仙 | 受击减伤 12%，开局 20% 护盾，反震 15% 实际伤害 |

3. **防作弊权威开战链路（复用 `MATCH_MODE_PVE_SOLO`）**
   - 玩家先在 `SceneSeasonClientPlayer` 调 `StartWuxingTowerRun`（建立第 1 层新轮次）。
   - 玩家点击「挑战当前层」时，发送 `JoinQueue(mode=MATCH_MODE_PVE_SOLO, battle_config_id=<层映射基础副本 101..104>, is_wuxing_tower=true)`。
   - Go `match` 将 `is_wuxing_tower=true` 经 `PrepareBattleRequest` 透传给 `Scene`；`Scene.PrepareBattle` **不信任客户端传入的任何层数或符印**，而是直接读取玩家自身权威 `PlayerSeasonComp.wuxing_tower`，强校验 `run_active && pending_offer_sigil_ids.empty()` 后，把权威 `WuxingTowerBattleContext { week_key, run_id, floor, leyline_element, active_sigil_ids }` 写入 `BattlePlayerSnapshot.tower_context`！
   - `Battle` 引擎在 `Initialize` 时提取并冻结 `tower_context`，对怪物应用层数倍率（1 层 85% 起步，每层 +8% 气血与 +5% 攻击，5/10 层额外首领强化），激活全部已选符印与周层灵脉，满血满蓝无耗材入战；终局时将 `WuxingTowerSettlementEcho { week_key, run_id, floor, total_rounds }` 写入 `BattleSettlementData.tower_settlement` 回传 `Scene`。
   - `Scene` 在 `PlayerSeasonSystem::PrepareBattleCredit` 原子结算：仅当胜利且 `run_id`/`floor`/`week_key` 与当前活跃轮次一致时推进层数、置位 `weekly_cleared_mask`、在 1..9 层生成 3 枚确定性待选符印，第 10 层完成全通并刷新周最速榜。

---

### 模块 C：道友切磋坊 · 自定义规则约战 + 战报一键看配装

1. **自定义规则切磋（`CustomDuelRules`）**
   - 在 `ChallengePlayerRequest`、`ChallengeInviteS2C`、`MatchChallengeInvitationRecord`、`CreateBattleRequest`、`BattleStateS2C` 透传 `CustomDuelRules`：
     - `use_free_preset`（`false` 真养成 / `true` 公平 3×3 免费模板）；
     - `enable_polarity_rotation`（开启每 2 回合物势/法势交替）；
     - `round_limit_override`（白名单 `0 / 8 / 12 / 20`，其余拒绝）；
     - `ban_control_skills`（禁控模式：引擎拒绝控制类主动技能提交并在效果层拦截控制异常）；
     - `allow_public_spectate`（允许全服公开观战：`spectateVisibilityFor` 在切磋模式且此项为真时冻结为 `SPECTATE_VISIBILITY_PUBLIC`）；
     - `challenger_character_preset_id` / `challenger_pet_preset_id`（`use_free_preset=true` 时生效，缺省回落 `101/203`）。
   - `RespondChallengeRequest` 新增可选 `responder_character_preset_id` / `responder_pet_preset_id`，应战接受时由 Go `match` 组装 `RankedMatchContext{ season_id=0, use_free_preset=true, members=[双方预设] }`（`season_id=0` 保证绝不计排位分、不进 Elo 榜！），同时 `Scene.PrepareBattle` 为所有切磋局启用局内满血蓝、不消耗局外道具、不刷 660/100 额度，且由既有 `battle_replay_policy` 自动录制战报！
2. **战报一键看配装 + 配装收藏夹**
   - `BattleReplay` 新增 `repeated BattleActorBuildSummary actor_builds = 14;`：建房初始化 `BattleReplayRecorder` 时直接从冻结 `CreateBattleRequest.players` 采集**脱敏公开构筑**（`player_id, name, team_index, class_id, level, formation_id, wuxing_element, season_seal_id, active_pet_table_id, active_pet_name, active_pet_element, skill_table_ids, wuxing_sigil_ids, used_free_preset`），绝不泄露装备概率、冷却、货币或背包内容。
   - `SceneSeasonClientPlayer` 新增 `SaveBattleBuildBookmark` / `DeleteBattleBuildBookmark`（最多保存 12 条至 `PlayerSeasonComp.saved_builds`，支持按收藏一键套用同款阵法与赛季印）。

---

## 三、思想分析

1. **复用优于造轮子（KISS / Ponytail）**：不新建任何 MySQL 表、不新建独立匹配服务、不改动任何脏 Excel 文件；三套系统分别附着于现有 `PlayerSeasonComp`、`TurnBattleEngine`、`ChallengePlayer` 与 `BattleReplay` 主干，以最小侵入面实现最大玩法纵深。
2. **确定性优先（回放 / 重连 / 观战同源）**：阵法属性、五行相生、15 枚幻境符印、层数缩放、禁控规则、物法易势全部使用整数定点数与确定性伪随机，保证客户端回放与战中补拉逐回合一致。
3. **契约即边界（零客户端信任）**：幻境层数与符印由 `Scene` 权威塞进快照；自定义切磋规则白名单由 Go `match` 校验并随 SQL 邀请与 `CreateBattleRequest` 原件冻结；战报构筑由 `Battle` 节点脱敏生成，杜绝任何客户端伪造或隐私泄露。

---

## 四、漏洞分析与防线

| 潜在漏洞 / 风险 | 应对防线 |
|---|---|
| 客户端伪造 `wuxing_tower_floor` 或 `active_sigil_ids` 跳层/堆满 15 枚仙品符印 | `JoinQueue` 只传布尔标记 `is_wuxing_tower`；真实 `floor` 与 `active_sigil_ids` 全部由 `Scene.PrepareBattle` 从权威 `PlayerSeasonComp.wuxing_tower` 注入，且强校验 `pending_offer_sigil_ids` 为空。 |
| 重复结算同一场幻境战斗（弱网重投/并发）导致连跳两层或双抽符印 | `WuxingTowerSettlementEcho` 携带 `(week_key, run_id, floor)`；`Scene` 结算严格比对三元组与 `pending_offer_sigil_ids.empty()`，不匹配直接幂等跳过。 |
| 跨周一 05:00 时旧轮次幻境正在战斗中，结算写回新周 | 结算校验 `echo.week_key == currentWeekKey`，跨周旧战局不污染新周首通位图和最速榜。 |
| 切磋局勾选 `use_free_preset=true` 被误判为排位计分局或产出 660 额度 | 切磋局 `RankedMatchContext.season_id` 恒为 `0`、`match_mode == MATCH_MODE_PVP_CHALLENGE`，排位记账与材料总账均显式排除切磋模式。 |
| 自定义切磋非法 `round_limit_override`（如 1 回合或 9999 回合拖垮战斗服） | Go `match` 入口与 C++ `TurnBattleEngine` 双重白名单校验：仅允许 `{0, 8, 12, 20}`，非法直接拒绝。 |
| 战报配装泄露对手私密暴击/抗性概率或背包物品 | `BattleActorBuildSummary` 仅包含公开可见身份、阵法、五行、赛季印 ID、出战灵宠种类与技能表 ID，与 `StripEngineOnlyState` 同口径。 |
| 收藏夹写入超限撑爆 `PlayerSeasonComp` Blob | 服务端硬上限 `kMaxSavedBuildBookmarks = 12`、标题截断 ≤ 48 字节、技能列表 ≤ 12 项，且按 `bookmark_id` 去重/覆盖。 |
| 并行会话修改共享 `.proto` 产生字段号冲突 | 所有共享 `.proto` 严格只追加到末尾、避开并行会话已占字段号，绝不修改或重排任何既有字段。 |

---

## 五、步骤分析与执行步骤

1. **阶段 1 · 协议扩展与全语言生成**
   - 追加 `proto/common/component/player_season_comp.proto`（阵法、幻境爬塔状态与位图、配装收藏）。
   - 追加 `proto/battle/battle_data.proto`（`WuxingTowerBattleContext`、`WuxingTowerSettlementEcho`、`CustomDuelRules`、`BattleActorBuildSummary`、`BATTLE_EVENT_WUXING_SYNERGY` 及快照/结算扩展字段）。
   - 追加 `proto/battle/battle_node.proto`（`CreateBattleRequest.custom_duel_rules`）、`proto/battle/player_battle.proto`（`BattleStateS2C` 阵法/幻境/切磋规则字段与 `BattleReplay.actor_builds`）。
   - 追加 `proto/match/match_service.proto` & `proto/match/match_authority.proto`（`JoinQueueRequest.is_wuxing_tower`、`ChallengePlayerRequest/ChallengeInviteS2C/RespondChallengeRequest` 自定义切磋字段与 SQL 持久化载荷）。
   - 追加 `proto/scene/player_season.proto`（`EquipWuxingFormation`、`GetWuxingTowerState`、`StartWuxingTowerRun`、`PickWuxingTowerSigil`、`AbandonWuxingTowerRun`、`SaveBattleBuildBookmark`、`DeleteBattleBuildBookmark`）。
   - 在 `xuanming-server-mmo-cpp` 执行 `dev.bat proto` 生成全语言 proto / RPC / 元数据产物。
2. **阶段 2 · C++ 战斗引擎与节点实现**
   - 新增独立共享规则头 `cpp/libs/services/battle/constants/wuxing_expansion_rules.h`（5 套阵法、五行相生函数、15 枚幻境符印表与层数缩放纯函数）。
   - 接入 `TurnBattleEngine`（`Initialize` 冻结阵法/幻境/切磋规则、伤害/治疗/护盾/反震/回合初恢复/禁控/物法易势/五行相生合击事件）。
   - 接入 `BattleReplayRecorder` 与 `BattleRoomManager`（冻结脱敏 `actor_builds`）。
3. **阶段 3 · C++ 场景服与 Go 匹配服实现**
   - 扩展 `PlayerSeasonSystem` 与 `player_season_handler.cpp`（阵法切换、五行幻境开局/三选一/放弃/周榜/结算推进、配装收藏 CRUD）。
   - 扩展 `PlayerBattleSystem::PrepareBattle` 与 `BuildBattleSnapshot`（注入 `equipped_formation_id`、校验并注入权威 `WuxingTowerBattleContext`、切磋满血蓝与免费预设支持）。
   - 扩展 Go `match`（`challenge.go`、`global_challenge.go`、`challengelogic.go`、`global_match.go`、`gather.go`、`spectate_visibility.go`）透传并校验 `CustomDuelRules` 与 `is_wuxing_tower`。
4. **阶段 4 · 服务端编译与单测验收**
   - 跑 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py` 预检。
   - 编排 C++ 编译（串行 `/m:1`）并运行 `turn_battle_engine_test`、`bag_test`、`skill_test` 新增用例；运行 Go `match` 单元测试。
5. **阶段 5 · 客户端同步协议、纯 C# 模型、UGUI 界面与单测**
   - 在 `mmorpg-client` 执行 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`。
   - 新增/扩展客户端纯 C# 模型与客户端门面：`WuxingExpansionModels.cs`、`SeasonClient.cs`、`BattleClient.cs`、`BattleReportClient.cs`。
   - 新增/扩展 UGUI 界面：`WuxingTowerPanel.cs`（含 10 层进度、三选一符印弹层与周最速流派榜）、`SeasonSealPanel.cs`（新增免费五阵切换页签与人宠相生预览）、`BattleChallengePopup.cs` / `CustomDuelSetupPanel.cs`（自定义规则勾选与应战规则明示）、`BattleReportPanel.cs`（战报一键看配装与收藏夹）、`BattleCoreHudOverlay.cs`（阵法与五行相生合击提示条）。所有新美术槽位预绑定到 `Resources/UI/Ugui/WuxingExpansionsV1/` 并提供优雅降级绘制，待 Codex 交付 PNG 导入即刻生效。
   - 跑 `tools/client_compile_check.ps1` 与全部 EditMode NUnit 单测。
6. **阶段 6 · 交付 Codex 纯美术 UI 清单**
   - 输出 100% 纯功能、零代码术语、不指定画风/材质/色号的作图清单，供 Codex 按项目既有的统一 UI 美术风格在 `D:\work\image\designs\wuxing-expansions-ui-20261125` 产出透明底无字新图。
