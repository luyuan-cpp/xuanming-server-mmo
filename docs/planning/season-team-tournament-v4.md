# 《群仙问道赛》V4 策略博弈与长线荣耀扩展设计方案（模块 H / I / J）

> 状态：设计与全栈源码实现中（配表导出、Proto/表代码生成、三仓编译与单测交由 Gemini 执行；增量 UI 切片交由 Codex 制作，本文档不定义美术风格）。  
> 依赖前置：`docs/planning/season-team-tournament-v1.md`、`docs/planning/season-team-tournament-v2.md`、`docs/planning/season-team-tournament-v3.md`。

---

## 1. 需求分析

V1–V3 已覆盖赛季报名、海选、BO3 五行灵脉、冠军玉座观道、公开战报与观战喝彩、热身约战、神机免费竞猜、MVP 评定、季军战与 8 枚战队徽标。为进一步提升高端对决的策略深度、赛后内容消费与战队归属感，V4 增加 3 个零门槛、零付费、纯玩法增强模块：

1. **模块 H · BO3 赛前「五行灵脉禁选（Ban/Pick）」**：V2 的四强 BO3 灵脉完全靠随机抽取，双方队长缺少赛前博弈空间。允许双方队长在 BO3 首局开战前各禁 1 条灵脉，剩余 3 条按确定性顺序成为 G1/G2/G3 灵脉，让克制与反克制从进房前就开始。
2. **模块 I · 本季「名局录（Highlights Hall）」精选高光回放墙**：淘汰赛精彩对局散落在对阵树各个节点中，观众寻找总决赛、BO3 让一追二激战局或高分 MVP 局成本高。服务端自动基于既有公开战报聚合本季 Top 10 名局并打标签，一键直达回放（并可直接复用既有脱敏配装查看）。
3. **模块 J · 战队赛季「荣耀铭牌（Honor Milestones）」成就墙**：战队整季征战除了最终名次外缺少过程荣誉沉淀。新增 6 枚零数值、纯荣耀的战队里程碑徽记，随海选/淘汰赛/封榜自动点亮并永久挂在战队页、排行榜与冠军殿堂。

**红线约束**：100% 零付费、零数值加成、零押注、零自由文本/自由上传图片 UGC；零新增 SQL 表（仅复用现有锦标赛表追加 4 个小整型列）、零新增战斗引擎改动（复用 V2 灵脉与 V3 结算统计）。

---

## 2. 设计分析

### 2.1 模块 H：BO3 赛前「五行灵脉禁选（Ban/Pick）」
- **开放范围**：`TournamentRule.leyline_ban_enabled == true`（`rule_version = 4`），且对局满足 `phase == KNOCKOUT`、`best_of >= 3`、`wins_a == 0 && wins_b == 0`、`state IN (WAITING_OPPONENT, READY)`、`squad_a_id != 0 && squad_b_id != 0`。
- **交互协议**：`BanTournamentLeyline(match_id, leyline_element)`
  - 仅参战两队队长可调用（非队长拒回 `kMatchTournamentNotCaptain`）；
  - `leyline_element` 必须为 `1..5`（金/木/水/火/土，非法值回 `kInvalidParameter`）；
  - 若本对局已开战/非 BO3/非未开赛首局，拒回 `kMatchTournamentMatchNotReady`；
  - 开战前允许双方队长随时修改各自禁选（若一方未禁也不卡流程，绝不阻塞开赛）；
  - 每次提交禁选后，服务端立即调用确定性规则函数 `DrawDistinctLeylinesWithBans(seasonId, matchId, bestOf, bannedA, bannedB)`：从 `[1..5]` 中剔除 `{bannedA, bannedB}`（即使双方禁同一条，剩余池恒有 3–4 条 ≥ 3），用 SplitMix64 确定性抽取不重复的 `bestOf` 条灵脉写入 `leyline_sequence`，并同步更新 `current_leyline_element = leyline_sequence[0]`。

### 2.2 模块 I：本季「名局录（Highlights Hall）」
- **查询协议**：`GetTournamentHighlights(season_id)`（`season_id == 0` 表示当前赛季，允许只读回看往届）。
- **零新表纯确定性策展**：读取指定赛季 `phase == KNOCKOUT && state == FINISHED && battle_id != 0` 的公开战报场次（要求 `public_knockout_replay == true`），按以下优先级打标并排序取前 10 场：
  1. `HIGHLIGHT_TAG_FINAL (1)`「总决赛巅峰局」：`!is_third_place && round_index == maxRound`
  2. `HIGHLIGHT_TAG_BO3_THRILLER (2)`「决胜三局激战」：`best_of >= 3 && (wins_a + wins_b) >= 3`
  3. `HIGHLIGHT_TAG_THIRD_PLACE (3)`「季军争夺战」：`is_third_place`
  4. `HIGHLIGHT_TAG_HIGH_MVP (4)`「高光问鼎局」：其余已决出淘汰赛（按 `mvp_score` 降序展示高分表现）
- **确定性排序**：`tagPriority` 升序（Final=0 < Bo3Thriller=1 < ThirdPlace=2 < HighMvp=3）→ `mvp_score` 降序 → `finished_ms` 降序 → `match_id` 无符号升序。

### 2.3 模块 J：战队赛季「荣耀铭牌（6 枚纯荣耀位图）」
- **位图定义**（`uint32 honor_bits`，落库 `match_tournament_squad.honor_bits`，夺冠同步固化到 `match_tournament_season.champion_honor_bits`）：
  - `bit 0 (1)` — **问道初成**：战队本季累计完赛 ≥ `participation_min_matches`（默认 2 场）
  - `bit 1 (2)` — **连胜破竹**：海选胜场 ≥ 3（`qualifier_wins >= 3`）
  - `bit 2 (4)` — **不败金身**：海选已打 ≥ 3 场且 0 负（`qualifier_played >= 3 && qualifier_losses == 0`）
  - `bit 3 (8)` — **逆转乾坤**：淘汰赛赢下一场打满 3 局的 BO3 生死战（`best_of >= 3 && wins_a + wins_b >= 3`）
  - `bit 4 (16)` — **零封强敌**：淘汰赛 BO3 以 `2:0` 完胜对手
  - `bit 5 (32)` — **问鼎群仙**：夺得本季全服总冠军（`final_rank == 1`）
- **单调递增、不可回退**：仅以位或（`|=`）累加写入，绝不因后续输球被撤销。

---

## 3. 思想分析

1. **零额外状态机阻塞**：灵脉 Ban/Pick 直接挂在首局开战前的 `WAITING_OPPONENT / READY` 窗内，不引入额外的 Ban 阶段超时期限，玩家体验轻快且避免对手消极拖延。
2. **零新表、零 C++ 侵入**：全部持久化复用现有 3 张表的 4 个标量整型列；名局录完全由既有 `match_tournament_match` + `match_tournament_match_game` 视图实时策展；战斗引擎零改动即可享受 Ban/Pick 后的新灵脉序列。
3. **双版同构纯规则核**：`DrawDistinctLeylinesWithBans`、`ComputeSquadHonorBits`、`ClassifyHighlightTag` 与排序比较器在 Go `internal/tournament/rules.go` 与 Java `TournamentRules.java` 逐位一致。

---

## 4. 漏洞分析与防线

| 潜在漏洞 / 边界 | 防护措施 |
|---|---|
| 双方队长禁选同一条灵脉导致可用灵脉不足 3 条 | 5 条灵脉最多被禁 2 条（若同禁只占 1 条），剩余候选恒在 3..4 条之间，必定满足 BO3 不重复抽取 |
| 战斗开打后或局间换阵期恶意调用 `BanTournamentLeyline` 篡改当前灵脉 | 事务内锁对阵行严格校验 `phase == KNOCKOUT && best_of >= 3 && wins_a == 0 && wins_b == 0 && state IN (1, 2)`，开战后直接拒回 `kMatchTournamentMatchNotReady` |
| 非参赛方或普通队员偷禁对手灵脉 | 事务内校验调用者必须等于 `squad_a.captain_player_id` 或 `squad_b.captain_player_id`，否则回 `kMatchTournamentNotCaptain` |
| `public_knockout_replay=false` 时名局录泄露私密战报 | `GetTournamentHighlights` 严格检查 `rule.public_knockout_replay`，关闭时直接返回空列表 |
| 海选 0 场战队误亮「不败金身」 | `bit 2` 强制要求 `qualifier_played >= 3 && qualifier_losses == 0` |

---

## 5. 步骤分析

1. **契约与配表层**：`tournamentrule_table.proto` + `TournamentRule.xlsx`（`rule_version=4` + `leyline_ban_enabled=true`）、`match_service.proto`（新增 2 RPC + 视图字段）、`match_authority.proto` & `match_feature_tables.proto`（4 个整型字段）。
2. **Go `match` 服务端**：`catalog.go`、`rules.go` + `rules_test.go`、`tournament.go` + `tournament_test.go`、`tournamentlogic.go`、`matchserviceserver.go`。
3. **Java `game-match` 独立服务端对账**：`MatchMethods.java`（42 方法）+ `MatchMethodsTest.java`、`TournamentTables.java`、`TournamentRules.java`、`TournamentStore.java`、`TournamentService.java`、`MatchFeatureConfiguration.java`、`TournamentParityTest.java`。
4. **Unity 客户端**：`TournamentModels.cs`、`TournamentClient.cs`、`TournamentUiModel.cs`、`TournamentArtCatalog.cs`、`TournamentWindow.cs`、`TournamentUiRoot.cs` + `TournamentClientTests.cs` / `TournamentUiModelTests.cs`。
5. **静态门禁与交接**：跑轻量只读守卫并输出零风格干涉的 Codex 独立复制块与 Gemini 生成编译命令块。

---

## 6. 执行步骤（移交 Gemini 一次完成 V3+V4 生成与编译）

详见 `docs/planning/season-team-tournament-v3.md` §6 及交付回复中的 Gemini 统一验证命令块（一次 `dev.bat gen` + `ContractSync` + `gen.bat client` + 全量编译单测同时覆盖 V3 与 V4）。
