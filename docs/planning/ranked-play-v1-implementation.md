# 《论剑排位首发 V1》工程落盘设计

> **[2026-11-24 已作废 — 仅保留 `RankedPreset` 表结构供轮换娱乐场复用]** 负责人已拍板：正式 `PVP_TEAM` 固定走本人真实养成，不落地纯标准化正式排位；统一工程方案见 [season-pvp-pve-economy-unified-v1.md](season-pvp-pve-economy-unified-v1.md)。

对应规格：[ranked-play-season-ui-candidate.md](ranked-play-season-ui-candidate.md)（2026-11-23）。本文记录 C++/Go 权威服、Java 独立服、共享配置与协议、以及 Unity 非 UI 适配的完整落地设计。

## 1. 需求分析

1. **五档真人排位与单票约束**：`1V1`、`2V2`、`3V3`、`4V4`、`5V5` 统一走 `MATCH_MODE_PVP_TEAM`（每侧真人玩家数 `1..5`，每人操作 1 人物 + 1 竞技宝宝，两侧人数相等）。单人可任选五档；预组队整体报名并由系统补足真人；4 人队可打 4V4，打 5V5 明确拒绝。每人同时仅有一张排队票，取消须经服务端确认后方可改人数档。
2. **统一 Elo、硬上限分差窗口与等待提示**：五档共用 `PVP_TEAM` 一条 Elo（初分 `1500.00`、K=32、下限 0、保留存量分）、一张赛季榜（`RANK_BOARD_TYPE_MATCH_PVP_TEAM = 3`）和一份个人赛季参与进度。沿用既有预组队限制（队内分差 ≤ 500、队伍分 = 均分 + 25×(人数-1)、网络放置）。排位匹配分差窗口为 `<30s: 100`、`30..<90s: 200`、`>=90s: 300`（硬上限 300，永不退化为 `+Inf`），撮合与提交点同曲线、同指纹。排队超过 120 秒客户端提示继续等或取消。
3. **六套免费正式预设与身份/资产隔离**：人物（破阵 101 / 控场 102 / 守心 103）与宝宝（破甲 201 / 灵咒 202 / 护主 203）独立选择，默认破阵 + 护主，全员免费。人物保留真实名字与有效外观（含 10 元形象卡/试用）；已有合法出战宝宝沿用其名字与展示外观，零宠物/未出战直接按预设种类默认名字与外观入场。竞技快照必须从空竞技数据构建（参考职业 `Class.id=1`、30 级、150 点按权重分配、正式技能 1 重、宝宝物种自然成长 + 资质中点 + 20 点条目预算、满血蓝起手），不读实际装备/强化/法宝/资质/场外 Buff/消耗品，跳过背包预留；零宠物不造假永久宠物 ID、不耗号段，以 `pet_id = 0` + `competitive_temporary = true` 生成正常局内 `actor_id`（`kPetActorIdBase + idx`）；重连、结算、作废与补算均禁止写回场外血蓝、技能或宠物。
4. **组队预设确认**：单排复用 `JoinQueue`；预组队复用 `StartTeamMatch` 并新增成员预设确认入口 `ClientPlayerTeam.ConfirmRankedPreset`，队长可在视图看到每名成员的确认态；名单变动自动撤单并使旧确认失效；Match 在接票与成局提交点复核全员最新确认预设、名单与规则版本。
5. **战斗引擎规则**：新增排位专用模板 `Dungeon.id = 5`（`time_limit = 720` 秒，对应 120 逻辑回合，`max_team_size = 5`，无怪物，`allow_pet_capture = 0`），`PlayMode` 将 `PVP_TEAM` 切换到 `battle_config_id = 5`（老 `battle_config_id = 1` 自动被目录拒绝）。排位同速出手序使用冻结 `seed + round_index + actor_id` 的 SplitMix64 确定性随机决胜（可重放、重连不重掷、不偏斜阵营或 ID 大小）。排位逻辑回合上限直接由引擎产出 `BATTLE_OUTCOME_DRAW`；健康到时提交 `COMPLETE + DRAW`，节点失联/停机走 `ABORT + VOID`。
6. **五段位、亮段上榜、八周赛季与奖励**：初试 0 / 青锋 1400 / 云章 1550 / 天枢 1700 / 无双 1850（服务端原精度比较，前端向下取整）。个人本季首场有效参与结算后亮段上榜（首次上榜 `achieved_ms` 取本季首次亮段时刻）；未亮段不入新季榜；已亮段后逃离仍照常扣分并投榜。个人有效参与事实在终局前由 battle 房间冻结进 `BattleResultEvent` 与 `BattleSettlementData`（非逃离，且满足：服务端接受过手动指令，或进房后显式开自动，或进房后在首次可操作机会前即阵亡/被全控/结束）。跨规模累计 4 场头像纹样、8 场名片底纹、12 场永久青云玉符；封榜时按本季最高段位发天枢/无双徽章与称号；封榜第 1 且终分 ≥ 无双发榜首题名（否则空缺）。
7. **装备材料日额度复用**：复用既有 `PlayerEquipmentMaterialRewardSystem`（有效 PVP +10 份，与 PVE 共用每日 20 份上限，UTC+8 05:00 重置，玩家走原领取流程后才入包）；排位不达标参与不给材料额度；日额度已满后有效排位仍累计赛季场次。在 `BattleEndS2C` 与 `PlayerBattleResult` 显式带出本局实增额度（+10 / +0），并补齐 Java `game-scene` 同等能力。
8. **权威成局冻结与可恢复换季**：Match 在成局提交点冻结 `season_id`、`season_key`、`rank_generation`、`rule_version`、`team_size`、`config_fingerprint` 与全员预设；终局按 `battle_id` + 摘要幂等结算。换季按 `[start_ms, end_ms)` 状态机可恢复推进：停旧季新成局并清退未成局票 → 排空已接纳旧季对局至终局 → 排空计分与权益落账 → 停旧季 64 片投榜来源 → 确认发件箱全量投榜 → `FreezeRankGeneration` 封榜 → 写荣誉/榜首快照 → 开新季代次与排队（继承 Elo、清零赛季场次、新 `period/generation/source_epoch`）。换季中对外返回「上季结算中」态。

## 2. 设计分析

### 2.1 配表与配置
- `data/Dungeon.xlsx`：新增 `id = 5`（`scene_id=17, max_team_size=5, time_limit=720, allow_pet_capture=0`, 无怪）。
- `data/PlayMode.xlsx`：第 6 行 `PVP_TEAM` 改为 `battle_config_id = 5, enabled = true, team_size_max = 5`（直接阻断旧 `battle_config_id = 1` 绕行）。
- `data/schema/rankedrule_table.proto` + `data/RankedRule.xlsx`：单行 `id = 1` 维护赛季规则版本、参考职业、竞技等级、默认预设、8 周天数、120s 提示阈值、100/200/300 窗口、4/8/12 场次、五段下限。
- `data/schema/rankedpreset_table.proto` + `data/RankedPreset.xlsx`：6 行正式预设（101 破阵、102 控场、103 守心、201 破甲、202 灵咒、203 护主）。
- `data/tip/Tip.xlsx`：
  - `team_error` 末尾新增：`TeamRankedPresetUnconfirmed`（有队员尚未确认本季论剑预设）、`TeamRankedPresetInvalid`（所选论剑预设无效或版本已更新）。
  - `match_error` 末尾新增：`MatchRankedSeasonSettling`（上季正在结算封榜中，请稍后再试）、`MatchRankedPresetInvalid`（所选论剑预设无效或规则已更新）、`MatchRankedRuleVersionMismatch`（排位规则已更新，请刷新后重试）。

### 2.2 协议契约（只追加，不破兼容）
- `proto/battle/battle_data.proto`：追加 `RankedPresetSelection`、`RankedMemberLoadout`、`RankedMatchContext`、`BattleParticipationFact`；在 `BattlePetSnapshot` 追加 `competitive_temporary`、`combat_pet_table_id`；在 `BattleActorState` 追加 `competitive_temporary`；在 `BattleSettlementData` 追加 `competitive_ranked`、`season_id`、`ranked_rule_version`、`participated`。
- `proto/scene/scene.proto`：`PrepareBattleRequest` 追加 `ranked_context`。
- `proto/battle/battle_node.proto`：`CreateBattleRequest` 追加 `ranked_context`。
- `proto/contracts/kafka/match_event.proto`：`BattleResultEvent` 追加 `ranked_context`、`participation_facts`。
- `proto/battle/player_battle.proto`：`BattleEndS2C` 追加 `material_credit_units_added`、`season_id`。
- `proto/common/component/player_battle_result_comp.proto`：`PlayerBattleResult` 追加 `material_credit_units_added`、`season_id`。
- `proto/team/team.proto` & `proto/team/team_authority.proto`：追加 `ConfirmRankedPreset` RPC、成员确认态字段、视图汇总字段、`TeamChangeReason.TEAM_CHANGE_REASON_RANKED_PRESET_CONFIRMED`、`StartTeamMatchRequest.ranked_rule_version` 及 `TeamLaunchOriginal` / `TeamLaunchMember` 冻结预设字段。
- `proto/match/match_service.proto`：`JoinQueueRequest` 追加 `character_preset_id`、`pet_preset_id`、`ranked_rule_version`；`GetQueueStatusResponse` 追加 `wait_prompt_seconds`、`current_rating_spread`、`team_size`、`season_id`；`MatchService` 末尾追加 `GetRankedProfile`、`SaveRankedPreset`、`UpdateRankedCosmetic`、`GetRankedBattleResult` 四个客户端 RPC。
- `proto/match/match_authority.proto`：在 `MatchAuthorityTicketRecord` 追加 `ranked_context`；在 `MatchFormationRecord` 追加 `ranked_context`、`ranked_season_id`（加索引）；新增 5 张权威表 `match_ranked_season`、`match_ranked_player_prefs`、`match_ranked_player_season`、`match_ranked_reward`、`match_ranked_battle`。

### 2.3 五本权威账边界
| 领域数据 | 唯一权威源 | 客户端读取/写入入口 |
|---|---|---|
| Elo / 段位 / 本季有效场次 / 亮段 / 往季记录 / 展示权益 | `match` SQL | `MatchService.GetRankedProfile`、`GetRankedBattleResult`、`SaveRankedPreset`、`UpdateRankedCosmetic` |
| 赛季排行榜（在榜、名次、榜首、封榜状态） | `rank` SQL | `ClientPlayerRank.GetRankPage` / `GetRankAround`（`board_type=3, scope=GLOBAL, period=season_key, generation=rank_generation`） |
| 装备材料日额度与可领取余额 | `scene` 玩家存档 | `SceneBagClientPlayer.GetEquipmentMaterialReward`（每局增量见 `BattleEndS2C` / `GetRecentBattleResults`） |
| 打造材料/强化石实际入包 | `scene` 玩家存档 | `SceneBagClientPlayer.ClaimEquipmentMaterialReward` + `GetBag` |
| 10 元角色形象卡与外观 | `scene` 玩家存档 | `SceneAppearanceClientPlayer.*`（原入口不变） |

## 3. 思想分析

- **单一真相与防分叉**：规则参数全收口在 `RankedRule` / `RankedPreset` 表与纯规则函数；快照构建复用现有人物/宝宝属性公式，不抄第二套静态数值表；有效参与由 battle 房一次性冻结，Match 场次与 Scene 材料额度读同一布尔结论。
- **失败封闭（Fail-Closed）**：配表缺行、指纹不符、规则版本过旧、成员未确认预设、或赛季处于结算态时，宁可拒绝入队/备战/建房，也不降级为普通养成快照或把旧季数据漏写进新季榜。
- **可恢复状态机**：换季各阶段落库 `match_ranked_season.rollover_stage`，任意一步重启均可幂等续跑；`match_rank_source` 与 `match_rank_outbox` 天然按 `source_epoch` 分代，旧季停写后不污染新季。

## 4. 漏洞分析（重点风险与防线）

1. **老 `ratingrule.Curve` 等满 90 秒退化为 `+Inf`**：为 `PVP_TEAM` 独立实现带 300 分硬上限的分段曲线 `RankedCurve`，纳入 `match_global_policy` 指纹，撮合与提交点共用。
2. **零宠物账号被引擎拒收或污染宠物列表**：引擎只在核验通过的 `ranked_context` 下允许 `competitive_temporary = true && pet_id == 0` 单位；结算不产出永久宠物回写条目；Scene 排位结算完全跳过血蓝/宠物/背包消耗/掉落/捕捉写回。
3. **场外 0 血或残血被 `PrepareBattle` 拦截或影响数值**：排位 `PrepareBattle` 跳过场外 0 血拒绝与道具预留，从空数据按满血蓝构建快照。
4. **逃离保分 / 零回合刷场次 / 纯挂机蹭场次**：`>=1` 回合 `COMPLETE` 局全员（含逃离）都结算 Elo 且已亮段者照常投榜；0 回合与 `VOID` 局不改 Elo 也不计场次；有效参与须非逃离且具备真实参与事实。
5. **换季边界与迟到结果**：已接纳旧季对局在停招后继续打完并归属旧季；新请求一律被 `SETTLING` 拒绝；停源后若出现异常迟到未记账旧季结果，明确报错并计指标，绝不静默丢弃或误写入新季榜。
6. **Java 持久状态台账**：Java `game-scene` 新增材料额度与近期战绩持久化到 `player_state.proto` 时，必须按 `PersistentSchemaTool` 追加第 7 代台账 `0007.schema` 并补未知字段往返单测。
7. **Unity WebGL2 / IL2CPP 约束**：新增客户端适配类纯 C# 实现，不引入线程、`Task.Run`/`Task.Delay`、`Socket`、`HttpClient` 或反射代码生成，并为全部新增 `.cs` 同步生成 `.meta`。

## 5. 步骤分析与执行步骤

1. 修改 `Dungeon.xlsx`、`PlayMode.xlsx`、`Tip.xlsx`，新增 `RankedRule` / `RankedPreset` schema 与 `.xlsx`，更新上述 `.proto` 文件。
2. 运行 `gen_schema_index.py`、导表器、协议与消息号生成、`ContractSync.java`（含 `--check`）及 Unity 同步脚本。
3. 实现 Go `match` / `team` / `rank`：硬上限排位曲线、规则与表指纹启动校验、组队预设确认、单排/整队冻结上下文、评分/亮段/场次/里程碑/赛季记录与换季状态机、按 `source_epoch` 多代投榜与 4 个客户端 RPC。
4. 实现 C++ `scene` 与 `battle`：空数据竞技快照与身份继承、排位结算隔离与材料增量曝光、引擎临时宝宝与确定性同速随机决胜与回合上限判平、房间参与事实冻结。
5. 执行 C++ 裸指针预检、串行 `msbuild /m:1` 构建、C++ 与 Go 单元/集成测试。
6. 实现 Java 独立服全链路对齐（`game-battle-engine` 实数核验 531/503/504 与排位规则、`game-battle`、`game-scene` 竞技快照/隔离/材料额度/战绩恢复、`game-match`/`game-team`/`game-rank`/`game-gate`）并运行 `./mvnw -B clean install` 与测试。
7. 实现 Unity 非 UI 适配（`RankedClient`、`TeamClient` 预设确认、`BattleMatchEntryClient`/`BattleClient`/`BattleResultRecoveryClient`/`RankClient` 适配）并运行 `client_compile_check.ps1`。
8. 跑通双服全量验收场景，更新 `docs/PROGRESS.md`、`PARITY.md` 与 `docs/README.md`，输出给另一台电脑 Codex 的正式对接清单与脱敏样例。
