# 《群仙问道赛》V3 全服狂欢与策略扩展设计方案（模块 D / E / F / G）

> 状态：设计与全栈源码实现中（配表导出、Proto/表代码生成、三仓编译与单测交由 Gemini 执行；20 枚 V3 美术切片交由 Codex 制作）。  
> 依赖前置：`docs/planning/season-team-tournament-v1.md`（四阶段闭环）、`docs/planning/season-team-tournament-v2.md`（模块 A BO3 五行地脉与 60s 局间换阵、模块 B 冠军玉座纯荣誉展示〔原每日观道致敬已于 2026-11-26 彻底移除〕、模块 C 淘汰赛公开战报与 6 条预设观战喝彩）。  
> 配套 Codex UI 交接规格书：[season-team-tournament-v3-ui-handoff.md](ui-handoff/season-team-tournament-v3-ui-handoff.md)。

---

## 1. 需求分析

V1 与 V2 已建立完整的赛季制全服战队锦标赛闭环，但长线游玩仍存在四个体验断层：

1. **赛前干等期无处练兵（模块 D）**：报名阶段与海选空档期，战队组齐 3+1 后无法提前磨合阵容或实测五行地脉手感，只能干等开赛。
2. **未晋级观众中期流失（模块 E）**：V1 全服助威仅有每季 1 票选队，淘汰赛多轮角逐期间，占全服 90% 的非参赛观众缺乏持续追更抓手。
3. **赛后缺少个人高光传播与真实形象沉淀（模块 F）**：① BO3 淘汰赛打完只见战队胜负，缺乏基于真实战斗数据的「本场 MVP（问鼎之星）」评定；② 战队成员建队时未携带外观字段，跨服玩家在冠军殿堂与天墉城玉座容易落回中性占位。
4. **四强落败即离场且战队缺少视觉徽记（模块 G）**：① 半决赛输掉的两支强队直接并列止步，少了一场高含金量的收官焦点战；② 全服榜单、对阵树与天墉城牌匾上战队仅有纯文字队名，缺乏直观道家旗徽辨识度。

**红线约束**：全模块坚持 **100% 零付费门槛、零货币/道具押注（绝不涉赌）、零自由文本/自由上传图片 UGC 风险、100% 绑定自用奖励**，所有数值与结算规则由权威服务端裁决，双服务端（C++/Go 与 Java）严格同构。

---

## 2. 设计分析

### 2.1 模块 D：赛前零积分热身约战（Warmup Spar）
- **开放范围**：赛季处于 `REGISTRATION` 或 `QUALIFIER` 且 `TournamentRule.warmup_spar_enabled=true` 时，任意达到首发人数（`member_count >= battle_team_size`）的 `DRAFT` / `REGISTERED` 战队队长均可发起。
- **匹配方式**：`StartTournamentWarmup(target_squad_id, leyline_element, lineup_player_ids)`
  - `target_squad_id == 0`：进入全服热身等待池（`phase = TOURNAMENT_MATCH_PHASE_WARMUP = 3`），优先撮合另一支等待中的热身队。
  - `target_squad_id != 0`：定向约战指定战队编号；当目标战队也发起对本队（或随机池匹配）时直接成局。
  - `leyline_element`（`0..5`）：允许队长自选演练灵脉（无脉/金锐/木生/水润/火炽/土厚），成局时以先挂单方选定灵脉为准（若为 0 则采纳应战方灵脉）。
- **零污染隔离**：建房时 `TournamentBattleContext.is_warmup = true`；战果到达 `ApplyTournamentBattleResult` 时仅标记热身局 `FINISHED` 并归档小局记录，**绝不改动**海选积分、场次、胜平负、晋级签表或赛季奖励资格。

### 2.2 模块 E：淘汰赛「神机问道」免费全服竞猜榜
- **零成本免费预测**：淘汰赛期间（`stage == KNOCKOUT` 且 `prediction_enabled == true`），全服任意玩家可对每一场尚未开战（`state IN (WAITING_OPPONENT, READY)` 且 `squad_a_id != 0 && squad_b_id != 0`）的淘汰赛免费预测胜者（含季军战与决赛），开战前可自由改选，一旦进入 `IN_BATTLE / INTERMISSION / FINISHED / FORFEIT / BYE` 即锁定。
- **防刷与分值**：SQL 表 `match_tournament_prediction` 以 `(season_id, player_id, match_id)` 为主键，物理保证一人一场一票；单场淘汰赛结束时同一事务结算全部该场预测：
  - 命中（`result_state = 1`）：按轮次权重记慧眼分（十六强=1 分，八强=2 分，半决赛=3 分，季军战=3 分，总决赛=5 分）；
  - 未中（`result_state = 2`）：0 分。
- **《神机榜》与达标纪念礼**：`GetTournamentPredictionBoard` 聚合本季命中数与慧眼分 Top 50（排序：慧眼分降序 → 命中场次降序 → 最新更新时间升序 → `player_id` 无符号升序）与本人完整预测明细；赛季封榜结算时，累计命中数 `>= rule.prediction_reward_min_hits`（默认 2 场）的玩家自动落账 `TOURNAMENT_REWARD_TIER_PREDICTION(9)`（绑定银两 + 悟道残页 + 灵兽口粮 + 「慧眼识仙」称号），经既有幂等邮箱通道领取。

### 2.3 模块 F：淘汰赛「问鼎之星（MVP）」评定 + 冠军真实外观自动回填
- **战斗引擎真实统计上报**：C++ 与 Java `TurnBattleEngine::BuildSettlement` 在 `BattleSettlementData` 新增 3 个只读累计统计字段：
  - `total_damage_dealt (33)`：玩家本人及其出战宝宝整场造成的有效扣血伤害总和；
  - `total_healing_done (34)`：玩家本人及其出战宝宝整场造成的有效治疗量与护盾吸收/赋予值总和；
  - `total_controls_landed (35)`：玩家本人及其出战宝宝整场成功施加硬控（封印/冰封/昏睡/混乱/眩晕）的次数。
- **确定性 MVP 评分公式**（Go 与 Java 共用纯函数 `ComputeMatchMvp`）：
  - 对每个参战玩家计算：`score = min(100000, damage/100 + healing/80 + controls*60 + (alive ? 120 : 0) + (onWinningSide ? 180 : 0))`；
  - 确定性平局决胜：`score` 降序 → `damage + healing` 降序 → 胜方优先 → 存活优先 → 无符号 `player_id` 升序；
  - BO3 淘汰赛跨局保留单局最高 MVP 表现记录（若 `newScore > match.mvp_score` 则更新），总决赛结束时同步固化到 `match_tournament_season.final_mvp_*`，供对阵树、高光战报卡与冠军殿堂展示。
- **真实外观自动回填**：`Gather` 阶段已从场景服拿到完整 `BattlePlayerSnapshot`（含 `player_name / class_id / gender / appearance_id`）。建房时对 `MATCH_MODE_SEASON_TOURNAMENT` 顺手异步/最佳努力调用 `SyncTournamentMemberProfiles`，把最新真实外观写回 `match_tournament_member`，从而保证夺冠封存 `champion_members_blob` 时全员永久拥有真实获批角色立绘（零额外客户端上报接口、不可伪造）。

### 2.4 模块 G：四强「季军争夺战（铜牌战）」+ 8 枚官方预设道家队徽
- **季军争夺战**：当 `knockout_team_count >= 4` 且 `third_place_match_enabled == true` 时：
  - `transitionToKnockout` 在末轮（`round = totalRounds`）生成两场对阵：`slot = 0` 为总决赛（`is_third_place = false`），`slot = 1` 为季军争夺战（`is_third_place = true`，同样适用 `bo3_top4_enabled` 与独立五行灵脉抽签）；
  - 半决赛（`round = totalRounds - 1`）决出胜负后，胜者进 `slot = 0`（总决赛），**败者自动进 `slot = 1`（季军争夺战）**，不再直接判并列第 3；
  - 季军争夺战结束后，胜者写 `final_rank = 3` 并写回 `match_tournament_season.third_place_*`、败者写 `final_rank = 4`；赛季封榜结算（`settleSeason`）须待决赛与季军战均完赛（或到达 `settle_start_ms` 强制超时裁决）后触发；
  - 奖励档位新增 `TOURNAMENT_REWARD_TIER_THIRD_PLACE (8)` 专供季军（第 3 名），`TOP4 (3)` 覆盖第 4 名（若关闭季军战则仍平滑覆盖 3–4 名）。
- **8 枚预设道家队徽（零 UGC 风险）**：
  - 枚举 `TournamentSquadEmblem` 严格限定 `1..8`：`1 青剑 / 2 太极 / 3 丹炉 / 4 青莲 / 5 玄鹤 / 6 雷印 / 7 八卦 / 8 云鼎`；
  - `CreateTournamentSquadRequest.emblem_id` 支持建队直选（0 默认 1），并新增 `UpdateTournamentSquadEmblem` 允许队长在 `SETTLED` 封榜前随时更换；
  - 队徽 ID 随 `TournamentSquadView`、`TournamentMatchView`、`TournamentSeasonView` 与 `TournamentBattleContext` 全链路透传。

---

## 3. 思想分析

1. **以既有原子模型承载扩展，不另起烟囱**：热身约战复用 `match_tournament_match` 与 `MatchLauncher`（仅以 `phase=WARMUP` + `is_warmup=true` 隔离）；季军战复用末轮 `slot=1` + `is_third_place=true`；竞猜领奖复用 `match_tournament_reward` + 既有 `MailAdmin` 幂等发件箱；外观回填直接消费 `Gather` 已存在的 `BattlePlayerSnapshot`。
2. **零信任边界与合规底线**：竞猜严禁消耗或产出可交易资产；队徽严控白名单整数枚举 `1..8`；MVP 只读 `BattleEngine` 权威冻结数据，客户端零写入权。
3. **平滑开关降级**：四个模块均受 `TournamentRule.xlsx` 单行配置独立控制，任意开关关闭均可无损回落至 V2 行为。

---

## 4. 漏洞分析与防线

| 潜在漏洞 / 边界 | 防护措施 |
|---|---|
| 玩家开战后或比分落后时改投神机竞猜刷命中 | `PredictTournamentMatch` 事务内锁对阵行校验 `phase == KNOCKOUT && state IN (1,2) && squad_a_id != 0 && squad_b_id != 0`，开战后直接拒回 `kMatchTournamentPredictionClosed` |
| 热身约战误计海选场次/积分或抢占正式淘汰赛 `MyActiveMatch` | 热身局 `phase=WARMUP`；`ApplyTournamentBattleResult` 显式早返回不改 `qualifier_*` 与晋级树；`findActiveMatchForSquad` 按 `phase` 优先返回正式赛（`QUALIFIER/KNOCKOUT` 优先于 `WARMUP`） |
| 季军战未打完就被决赛完成触发提前 `settleSeason` | `advanceSeason` 在 `KNOCKOUT` 阶段检查：须 `champion_squad_id != 0` 且（无未完赛的 `is_third_place` 场次，或已超过 `settle_start_ms`）才进入 `SETTLED` |
| BO3 某局提前退场/空结算导致 MVP 除零或非参战者混入 | `ComputeMatchMvp` 仅遍历 `BattleResultEvent.settlements` 中 `player_id != 0` 的真实参战玩家，全整型无浮点非确定性误差 |
| 非法队徽 ID（0/9/999）导致客户端资源缺失或越界 | 服务端 `UpdateTournamentSquadEmblem` 严格校验 `1 <= emblem_id <= 8`（越界回 `kMatchTournamentEmblemInvalid`），建队传 `0` 归一化为 `1` |
| 双版 Go/Java 竞猜排序、MVP 分或季军战槽位漂移 | 统一纯规则函数 `ComputeMatchMvp` / `PredictionWeightForRound` / `IsValidEmblemId` 在 Go `internal/tournament/rules.go` 与 Java `TournamentRules.java` 逐条对齐并覆盖 H2/Go 单测 |

---

## 5. 步骤分析

1. **契约与配表层**：扩展 `tournamentrule_table.proto`、`TournamentRule.xlsx`（`rule_version=3`）、`TournamentReward.xlsx`（新增季军 8 档与神机竞猜 9 档）、`Tip.xlsx`（新增 4 条 `match_error` Tip）、`battle_data.proto`、`match_service.proto`（新增 4 RPC 与 V3 字段）、`match_authority.proto`。
2. **战斗引擎统计（C++ + Java）**：在 `TurnBattleEngine` 累计每个玩家及其宝宝的 `total_damage_dealt / total_healing_done / total_controls_landed` 并写入 `BattleSettlementData`。
3. **匹配服务端（Go + Java 双版同构）**：
   - 纯规则核：队徽白名单、竞猜轮次权重、MVP 确定性评定；
   - SQL 存储：新表 `match_tournament_prediction` + 季军战生成/推进/封榜门禁 + 热身约战配对与零污染结算 + 成员外观回填 + 队徽更新 + 竞猜投票/结算/神机榜查询 + 季军/竞猜奖励发账；
   - 服务接线：Go `TournamentLogic` / `MatchServiceServer` / `gather.go` 与 Java `TournamentStore` / `TournamentService` / `GatherPipeline` / `MatchFeatureConfiguration` / `MatchMethods`（40 方法）。
4. **Unity 客户端（非 UI + UGUI + 单测）**：
   - `TournamentModels.cs` / `TournamentClient.cs` / `TournamentUiModel.cs` 补齐 4 条新 RPC、8 枚道家队徽目录、神机榜快照、热身约战与 MVP 格式化；
   - `TournamentArtCatalog.cs` 注册 20 枚 V3 切片路径（缺失时自动优雅回退，等 Codex 交付 20 枚 PNG 导入即自动点亮）；
   - `TournamentWindow.cs` / `TournamentUiRoot.cs` / `TournamentSpectateCheerOverlay.cs` 接入队徽选择、热身约战弹窗、对阵树季军战 + 预测按钮 + MVP 玉章、神机榜弹层与高光战报卡；
   - 补齐 EditMode 单元测试。
5. **生成、编译与双端实跑（移交 Gemini）**：导表、`proto-gen`、`ContractSync`、客户端协议同步、C++/Go/Java/Unity 全量编译与单测回归。

---

## 6. 执行步骤与交接清单（供 Gemini 执行生成与编译）

```powershell
# 1. 权威仓（D:\luyuan\xuanming-server-mmo-cpp）导表 + Proto 生成 + Schema/Docs 校验
cd D:\luyuan\xuanming-server-mmo-cpp
python tools\data_table_exporter\run.py tools\data_table_exporter\exporter_config.yaml
pwsh -File dev_tools.ps1 -Command proto-gen-run -UseBinary
python tools\data_table_exporter\tools\gen_schema_index.py
python tools\scripts\gen_docs_index.py

# 2. C++ 静态门禁 + 串行 MSBuild + 引擎单测
python tools\scripts\tests\test_cpp_no_raw_pointer_member.py
python tools\scripts\tests\test_cpp_thread_sources.py
python tools\scripts\check_log_text_english.py
# 串行 MSBuild 编译 proto/table/rpc/grpc_client/core/battle/gate/turn_battle_engine_test 后运行：
.\build\cpp\tests\turn_battle_engine_test.exe --gtest_filter="*Tournament*"

# 3. Go match 单测（待并行会话修复 shared/generated/table/spiritshopgoods_table.go 后运行）
cd D:\luyuan\xuanming-server-mmo-cpp\go\match
go test -count=1 ./internal/constants/... ./internal/tournament/... ./internal/authority/... ./internal/logic/...

# 4. Java 独立服务端（D:\luyuan\xuanming-server-mmo）契约同步 + clean install + 单测
cd D:\luyuan\xuanming-server-mmo
java tools/ContractSync.java --mmorpg ..\xuanming-server-mmo-cpp --maps-source ..\xuanming-server-mmo-cpp
java tools/ContractSync.java --check --mmorpg ..\xuanming-server-mmo-cpp --maps-source ..\xuanming-server-mmo-cpp
.\mvnw.cmd -B -pl "game-proto,game-table,game-battle-engine,game-battle,game-match" -am clean install -DskipTests
.\mvnw.cmd -B -pl game-battle-engine test "-Dtest=TournamentLeylineBattleEngineTest,SeasonBattleEngineTest"
.\mvnw.cmd -B -pl game-match test "-Dtest=TournamentParityTest,MatchMethodsTest,MatchApplicationContextTest,MatchReportsScriptsAndRankedTest"

# 5. Unity 客户端（D:\luyuan\mmorpg-client）同步 + 编译检查 + 单元测试
cd D:\luyuan\mmorpg-client
pwsh -File tools/sync_tables.ps1
pwsh -File tools/gen_proto.ps1
pwsh -File tools/gen_messageids.ps1
pwsh -File tools/client_compile_check.ps1
```
