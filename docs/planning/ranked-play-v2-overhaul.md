# 《论剑 / 人宠会武》V2 玩法体验重做与双服客户端落地设计

> 日期：2026-11-25  
> 状态：**已拍板并进入 C++/Go 权威服、Java 独立服与 Unity 客户端非 UI 层全量落地；Unity UGUI 补全与新控件交由另一台电脑的 Codex 按配套交接单执行**  
> 上位红线：[项目设定](project-vision.md) · [核心循环 V1](core-gameplay-loop.md) · [赛季与经济统一落地设计](season-pvp-pve-economy-unified-v1.md) · [赛季技能与单槽赛季印](season-skills-and-seal-design.md)  
> 替代与收口：[ranked-play-season-ui-candidate.md](ranked-play-season-ui-candidate.md)（历史旧稿，已废止）· [season-pvp-lunjian-v1-repaired.md](season-pvp-lunjian-v1-repaired.md)（体检与修复基线）

---

## 一、需求分析

在 2026-11-24/25 收口「常驻正式人宠会武 = 本人真实养成 + `1V1/3V3` 冷启动白名单 + 永久 660 / 周 150 两阶段材料总账」后，从玩家实战手感出发，PVP 核心体验仍有 **5 处直接伤「好玩」的根部缺陷**：

1. **轮换娱乐场 6 套免费人宠预设（`RankedPreset` 101–103 / 201–203）偏科且单薄**：
   - `102 控场` 仅带 `10021 闭灵锁魄`（仅封法），遇纯物理「破阵 + 破甲」几乎半残；
   - `203 护主（石灵）` 仅带 `11006 神龙护体`（仅防物理），在「物法易势 5V5」法势回合与法毒阵容面前变白板；
   - `201 破甲（金猊）` 带仅普攻触发的 `13001 乘胜追击`（追 2 敌），既与双技能循环冲突又会在「护卫救援」波及护卫；
   - 62 技能库中最有心理博弈味的招式（全封 `10020 封灵咒`、猜招复制 `10041 玄镜映法`、减疗破龟 `10035 断脉咒`、复活印 `10026 生生不息`、法术追击 `13006 灵术回响`）全部缺席，且人物只填了 3 格浪费第 4 格。
2. **1V1 选主目标缺博弈 + 奶盾拖满平局**：
   - 人物主动攻击清一色为敌 5 群攻，在 1V1（敌方仅 2 单位）里敌 5 等于必全中，失去了「赌集火谁 vs 预判单保」的回合制核心博弈；
   - `Dungeon.id = 5` 未显式写死 `round_limit = 20`，且无后期决胜加速，双奶盾局易拖成冗长平局。
3. **段位起跑线反直觉 + 中段与 12 场后动力断层**：
   - 新号初分 `1500.00`，但「青锋」下限为 `1400`、「初试」为 `0..1399`，新手第一局输了也在青锋，「初试」成了连输 7 场的惩罚段；
   - 「云章」无封榜段位徽章与称号留存，12 场参与外观拿满后，普通玩家缺乏「再赢几场 / 冲一个够得着的中段荣誉」的清晰阶梯。
4. **队列与重登细节残留断层**：
   - 客户端重登补弹漏传可交易材料数与反挂机未达标标记；旧战斗入口常量仍指向 `configId = 1`；大厅缺少服务端权威五段门槛与 6 胜里程碑提示。
5. **PVP 全程禁投降导致残局/托管垃圾时间**：
   - 残局大势已去或遇挂机队友拖满 25 秒读条时，无法体面投降，强迫消耗数分钟垃圾时间。

---

## 二、设计分析

### 2.1 6 套免费人宠预设彻底重做（`RankedPreset.xlsx`，`RankedRule.rule_version: 1 -> 2`）

严格复用已落表、双端引擎均已支持的 `battle_model = 1` 正式技能，补满 4 槽、清零偏科、每套具备高记忆点博弈核心：

| ID | 名称与定位 | 四维权重（体/灵/力/敏） | V2 技能清单（4 槽） | 宝宝条目（20 点） | 战术设计意图 |
|---|---|---|---|---|---|
| `101` | **破阵** · 爆发破疗 | `1 / 0 / 3 / 1` | `10002` 横戈裂岳、`10039` 天崩裂岳、`10025` 战意昂扬、**`10035` 断脉咒** | — | 保留物理群爆轴，补入 5 回合 -35% 减疗「破龟矛」反制双守心回血阵 |
| `102` | **控场** · 全封映法 | `1 / 2 / 0 / 2` | `10006` 紫霄雷阵、**`10020` 封灵咒**、`10034` 驱灵散势、**`10041` 玄镜映法** | — | 替换偏科封法为全主动封灵（物法奶通吃），加入猜招零耗蓝复制制造高光博弈 |
| `103` | **守心** · 奶盾复苏 | `2 / 2 / 0 / 1` | `10010` 青莲润生、`10033` 清心荡秽、`10040` 独立护盾、**`10026` 生生不息** | — | 补入预挂 5 回合复活印，与敌方驱散/断脉形成强交互拉扯 |
| `201` | **破甲（金猊）** · 碎甲提速 | `1 / 0 / 3 / 1` | `10002` 横戈裂岳、`10018` 碎甲击、**`10016` 青羽催行**、**`13005` 裂甲追击** | `501: 20` | 既可碎甲接爆发，又可给核心单体加速抢下轮先手；单目标裂甲追击不再误伤救援护卫 |
| `202` | **灵咒（灵狐）** · 毒法回响 | `1 / 2 / 0 / 2` | `10006` 紫霄雷阵、`10019` 蚀骨毒、`10034` 驱灵散势、**`13006` 灵术回响** | `502: 10`, `531: 10` | 法术技能后 35% 概率追加 2 敌法伤，盘活法系技能连动 |
| `203` | **护主（石灵）** · 双罩保核 | `2 / 2 / 0 / 1` | `10014` 灵鼎护元、`10010` 青莲润生、`11006` 神龙护体（物罩）、**`11005` 如意灵环（法罩）** | `503: 10`, `504: 10` | 物势挂神龙护体、法势挂如意灵环，彻底消灭遇法系变白板 |

### 2.2 战斗爽点重做：主目标凝锋（`+20%`）+ 战意渐盛（绝杀加速）+ 显式 20 回合上限

1. **显式回合上限**：`Dungeon.xlsx` `id = 5`（正式会武）显式落表 `round_limit = 20`（保留 `time_limit = 720` 秒硬兜底）。
2. **主目标凝锋（Primary Target Focus Bonus，仅竞技 PVP 生效）**：
   - 生效范围：正式会武（`IsRankedBattle()`）、物法易势（`MATCH_MODE_POLARITY_5V5`）与赛季锦标赛（`MATCH_MODE_SEASON_TOURNAMENT`）；绝不修改 PVE 或普通切磋。
   - 触发条件：人物主动释放 `battle_model = 1` 的多目标直接伤害技能（`origin == Direct`、`max_targets > 1`），显式指定了合法敌方主目标，且本轮实际目标数 `< max_targets`（例如 1V1 敌方 2 单位，或 3V3/5V5 残局剩余少于 5 单位）。
   - 效果：**该主目标本次伤害 `+20%`（×1.20）**，并在对应 `BattleEventItem` 标记 `is_primary_focus = true`（字段 `27`）供客户端弹出「凝锋」集火反馈。
3. **战意渐盛（Sudden-Death Escalation，防双奶拖平）**：
   - 生效范围与起算回合：正式会武（20 回合）从 **第 13 回合** 起；物法易势（16 回合）从 **第 11 回合** 起；赛季锦标赛从 `max(1, maxRounds - 7)` 起（通用公式：仅上述三类竞技 PVP 且 `maxRounds >= 10` 时，`startRound = maxRounds - 7`；若表配 `maxRounds < 10` 的极短测试局则不触发，避免污染短局单测）。
   - 叠加公式：当 `roundIndex >= startRound` 时，`escalation_stacks = roundIndex - startRound + 1`：
     - 所有直接伤害与持续伤害（非资源自耗）额外乘 `1.0 + 0.10 * stacks`（每层 +10%）；
     - 所有治疗与新增有限容量护盾（非复活起始血）额外乘 `max(0.20, 1.0 - 0.10 * stacks)`（每层 -10%，最低保留 20%）。
   - 状态透传：`BattleStateS2C` 追加 `round_limit = 21`、`escalation_stacks = 22`、`escalation_start_round = 23`，重连与观战逐帧确定性还原。

### 2.3 段位锚点校准、云章荣誉留存与 6 胜里程碑

1. **`RankedRule.xlsx`（`id = 1`, `rule_version = 2`）五段下限校准**（新号初始 Elo 保持 `1500.00`，K=32）：
   - **初试**：`[0, 1520)` → 新号首局亮段落在「初试」，净胜 2 场等分局（1500 → 1532）即可体验首次升段「青锋」；
   - **青锋**：`[1520, 1600)`（`tier_qingfeng_centi = 152000`）；
   - **云章**：`[1600, 1720)`（`tier_yunzhang_centi = 160000`）→ **新增封榜永久段位徽章与赛季称号留存**；
   - **天枢**：`[1720, 1850)`（`tier_tianshu_centi = 172000`）→ 封榜留存天枢徽章与称号；
   - **无双**：`[1850, +∞)`（`tier_wushuang_centi = 185000`）→ 封榜留存无双徽章、称号及封榜第 1 名榜首题名。
2. **本季 6 胜「锋芒」里程碑（不肝，给 12 场参与外提供胜场追求）**：
   - `RankedRuleTable` 追加列 `uint32 milestone_win_matches = 23;`（首测值 `6`）；
   - `RankedRewardKind` 追加枚举 `RANKED_REWARD_WIN_MILESTONE = 7;`；
   - 玩家单季累计 `wins >= milestone_win_matches` 且本局有效参与时，置位 `milestone_bits` 的 **`bit3 (0x8)`**，即时授予永久称号 `s{season_id}_win_milestone_{N}`（如 `s1_win_milestone_6`），可在 `UpdateRankedCosmetic.equipped_title_code` 自由佩戴或卸下。
3. **服务端权威门槛透传**：
   - `RankedSeasonInfo` 追加字段 `24..28`：`tier_qingfeng_centi`、`tier_yunzhang_centi`、`tier_tianshu_centi`、`tier_wushuang_centi`、`milestone_win_matches`，客户端禁止硬编码旧阈值。

### 2.4 PVP 延迟投降（`SurrenderBattle`，第 5 回合解锁）

1. **解锁与防刷**：
   - 仅限 PVP 模式（`!IsPveMatch()`），且必须在 **`roundIndex >= 5`**（`kPvpSurrenderUnlockRound = 5`）发起；前 4 回合拒绝回 `MatchBattleSurrenderTooEarly`，PVE 拒绝回 `MatchBattleSurrenderNotAllowed`。
   - 体面投降按正常战败结算（`outcome = 敌方胜`，`BattleSettlementData.surrendered = true`、**`fled = false`**）：照常扣 Elo；若本局已满足有效对抗门槛（`>=3 回合 && >=2 手动回合 && 有效贡献`），照常获得赛季有效场次与绑定材料额度，绝不按逃跑惩罚。
2. **1V1 与多人投票规则（100% 引擎内确定性，零时钟依赖）**：
   - **1V1（本队仅 1 名真人）**：玩家提交 `SurrenderBattle(agree=true)` 后**立即判本方战败结束战斗**；
   - **多人局（本队 ≥ 2 名真人）**：
     - 统计每名玩家连续未手动提交指令的已结算回合数 `consecutiveIdleRounds[playerId]`（每次被接受手动指令清 0，回合结算时未手动提交则 +1）；
     - 发起投票时，本队「需表决人数 `required_agrees`」= 本队所有玩家中 `consecutiveIdleRounds < 2`（含发起人本人）的人数，至少为 1 —— **防 1 名挂机/掉线队友绑架全队投降**；
     - 任一未投票队友提交 `agree = false` → 投票立即失败清除，本队冷却至 `roundIndex + 3`（拒绝回 `MatchBattleSurrenderCooldown`）；
     - 已达 `agreed_player_ids.size() >= required_agrees` → 立即判本方战败结束本局；
     - 回合结算进入下一回合时，未满票的活跃投票自动过期关闭，本队冷却至 `roundIndex + 2`。
3. **协议与隐私脱敏**：
   - `proto/battle/battle_data.proto`：新增 `BattleSurrenderVoteState`，`BattleSettlementData` 追加 `bool surrendered = 32;`、`BattleEventItem` 追加 `bool is_primary_focus = 27;`；
   - `proto/battle/player_battle.proto`：`BattleStateS2C` 追加 `round_limit = 21`、`escalation_stacks = 22`、`escalation_start_round = 23`、`surrender_unlock_round = 24`、`surrender_vote = 25`；`BattleClientPlayer` 末尾追加 `rpc SurrenderBattle (SurrenderBattleRequest) returns (SurrenderBattleResponse)` 与 `rpc NotifySurrenderVote (SurrenderVoteS2C) returns (Empty)`；
   - `RedactStateForViewer`（C++）与 `BattleViews`（Java）强制向对手及观众清空 `surrender_vote`，`NotifySurrenderVote` 仅推送给同队成员，杜绝军情泄露。

---

## 三、思想分析

1. **为什么不用「恢复旧单体技能」而是用「PVP 主目标凝锋 +20%」解决 1V1 选目标博弈？**  
   2026-11-21 已拍板撤回人物 5 个旧单体技能且永不复用 ID。用引擎层 10 行确定性「主目标凝锋」规则，既严格遵守既有技能目录红线、不增加配表与学习负担，又在 1V1 与多人残局瞬间恢复「点杀主目标 vs 预判单保」的回合制灵魂心理战。
2. **为什么投降必须卡在第 5 回合且按「连续 2 回合未手动」排除挂机绑架？**  
   卡第 5 回合可彻底杜绝 1–2 回合秒投刷 12 场外观与低保材料；而通过引擎内已结算回合的 `consecutiveIdleRounds` 排除纯挂机队友，既不引入节点时钟破坏回放确定性，又解决 3V3/5V5 掉线 1 人卡死全队的真实痛点。
3. **为什么给「云章」发封榜荣誉并加「6 胜锋芒称号」，而不是堆更多肝度任务？**  
   保留 12 场败局保底（零压力不肝），再把 1500 新号放进「初试」让赢 2 场就能破冰升「青锋」、赢 6 场拿锋芒称号、冲到 1600 留存「云章」徽章，形成「新手破冰 → 6 胜精进 → 12 场玉符 → 云章/天枢/无双段位荣誉」的自然阶梯。

---

## 四、漏洞分析与防线清单

| 潜在漏洞 / 风险 | 触发路径 | 硬防线 |
|---|---|---|
| **1–2 回合秒投白嫖 12 场外观或绑定材料** | 开局立刻调用 `SurrenderBattle` 结束对局 | 引擎强制 `roundIndex >= 5` 方可投降（否则回 `MatchBattleSurrenderTooEarly`），且负局仍需同时通过 `>=3 回合 && >=2 手动回合 && 有效贡献` 门槛 |
| **敌方或观战者窥探本队投降投票状态** | 收到 `TurnResultS2C` / `GetBattleState` / `SpectateStateS2C` 读取 `surrender_vote` | C++ `RedactStateForViewer` 与 Java `BattleViews` 对非同队视角及观众统一 `clear_surrender_vote()`；`NotifySurrenderVote` 仅单播同队成员 |
| **恶搞队友每回合狂点投降弹窗骚扰全队** | 投票被拒后立刻重新发起 | 投票被否决后同队进入 3 回合冷却（过期未满票冷却 2 回合），冷却期内一律拒回 `MatchBattleSurrenderCooldown` |
| **同队 1 人断线托管导致其余 2 人无法投降** | 3V3 中 1 人离线，剩下 2 人投同意却凑不够 3 票 | 引擎按已结算回合统计 `consecutiveIdleRounds >= 2` 自动移出必要表决池（至少留 1 票），纯确定性零时钟依赖 |
| **投降终局重复提交或竞态双结** | 两名队友同帧投出最后赞成票或恰好踩中回合超时定时器 | `HandleSurrenderBattle` 复核 `TerminalPending(*room)` 并在投票通过瞬间进入与 `ResolveRound` 相同的唯一 `FinishBattle` 终局提交闸 |
| **预设改表后三端指纹或默认兜底不同步** | 改了 `RankedPreset.xlsx` / `RankedRule.xlsx` 但忘同步 Go `DefaultFallback` 或 Java/C++ 测试 | 递增 `RankedRule.rule_version = 2`，同步更新 Go `DefaultFallback`、Java `ContractSync` 及三端校验单测；`scene.exe --validate-tables` 与 `go test` / `mvn test` 全量核验 |
| **战意渐盛误伤短局单元测试或 PVE 首领** | `maxRounds < 10` 的测试夹具或 PVE 副本意外吃到增伤/减疗 | 仅对 `IsRankedBattle() || Polarity5V5 || SeasonTournament` 且 `maxRounds >= 10` 启用，PVE 与短局测试零回归 |

---

## 五、步骤分析

1. **步骤 1（设计与契约源）**：落盘本文，在旧稿 [ranked-play-season-ui-candidate.md](ranked-play-season-ui-candidate.md) 顶部加作废指向；修改 `RankedRule` / `RankedPreset` / `Dungeon` / `Tip` 源表与 `battle_data.proto` / `player_battle.proto` / `match_service.proto`，运行 `dev.bat gen` 导表并生成全语言协议。
2. **步骤 2（C++ 战斗引擎与战斗节点）**：在 `TurnBattleEngine` 实现主目标凝锋 `+20%`、战意渐盛增伤/减疗盾、PVP 第 5 回合投降与多人投票状态机，并在 `BattleStateS2C` / `BattleSettlementData` 输出全量权威字段；在 `BattleClientEdge` 与 `BattleRoomManager` 接入 `SurrenderBattle` RPC、同队 `NotifySurrenderVote` 推送、终局收尾与敌方/观众脱敏；补充 `ranked_battle_test.cpp` 单测。
3. **步骤 3（Go `match` 排位服务）**：更新 `internal/ranked/catalog.go` 校验 `milestone_win_matches` 与同步 `DefaultFallback`（v2 预设与 1520/1600/1720/1850 阈值）；更新 `internal/authority/ranked.go` 透传五段门槛与 `milestone_win_matches`、落地 `bit3` 6 胜锋芒称号及允许佩戴；更新 `internal/authority/ranked_rollover.go` 将封榜段位徽章与称号下探覆盖「云章」及以上；更新与新增 Go 单测。
4. **步骤 4（Java 独立服完全对齐）**：运行 `ContractSync` 同步契约；在 `game-battle-engine`、`game-battle`、`game-match` 对齐全部 V2 规则（凝锋、战意渐盛、第 5 回合投降投票、新预设/五段/云章荣誉/6 胜里程碑），跑通 `clean install` 与受影响全套单测。
5. **步骤 5（Unity 客户端非 UI 同步与适配）**：运行 `.\gen.bat client` 同步表、Proto、消息号；更新 `BattleClient.cs`（投降 RPC + 投票推送 + 战意渐盛/回合上限/投降解锁状态）、`RankedModels.cs` & `RankedClient.cs`（服务端权威五段门槛、距下段/6 胜提示、重登补弹漏传字段修复、出战宠防呆提示）、`BattleUiStyle.cs`（旧入口对齐 `configId=5`），补充 EditMode 纯 C# 单测并通过 `client_compile_check.ps1`。
6. **步骤 6（全链路编译、单测、对账与 Codex UI 交接）**：执行 C++ 静态门禁与 `/m:1` 串行构建 + 单测、Go 全包测试、Java 模块测试、Unity 编译与单测；落盘独立的 [Codex Unity UI 交接单](ui-handoff/ranked-play-v2-ui-handoff.md)，刷新 `docs/README.md`、`docs/PROGRESS.md` 与 `xuanming-server-mmo/PARITY.md`。

---

## 六、执行步骤与验收标准

1. **配置与表校验**：`dev.bat gen` 零错产出，新 `RankedRule` `rule_version=2`、`tier=152000/160000/172000/185000`、`milestone_win_matches=6`，6 套 `RankedPreset` 全部 4 技能且通过 `scene.exe --validate-tables generated/tables` 强校验；`Dungeon.id=5` `round_limit=20`。
2. **战斗机制单测（C++ + Java 双端）**：
   - 1V1 人物释放 `10002` 敌 5 群攻时，显式主目标吃 `+20%` 凝锋增伤并产出 `is_primary_focus=true`，副目标不吃；满 5 目标或 PVE 局不触发；
   - 正式会武 R13 起每回合 `escalation_stacks` 递增，直伤增幅 `+10%/层`、治疗与护盾衰减 `-10%/层`（下限 20%）；
   - PVP 前 4 回合投降拒回 `MatchBattleSurrenderTooEarly`，PVE 拒回 `MatchBattleSurrenderNotAllowed`；1V1 第 5 回合投降立刻判本方负且 `surrendered=true, fled=false`；3V3 需全活跃队友同意、1 人否决触发 3 回合冷却、连续 2 回合未手动挂机队友不阻挡其余队友通过投降；对手与观众视角 `surrender_vote` 为空。
3. **段位与里程碑单测（Go + Java 双端）**：
   - 新号 1500 首局亮段为「初试」，胜 2 场（1532）升「青锋」，1600 升「云章」，1720 升「天枢」，1850 升「无双」；
   - 累计 6 胜自动解锁 `RANKED_REWARD_WIN_MILESTONE` 并支持佩戴到 `equipped_title_code`；
   - 封榜为「云章 / 天枢 / 无双」三档颁发对应最高段位徽章与赛季称号。
4. **客户端非 UI 验收**：
   - `BattleClient` 支持 `SurrenderBattle(bool agree)`、接收 `NotifySurrenderVote` 并暴露 `RoundLimit / EscalationStacks / EscalationStartRound / SurrenderUnlockRound / SurrenderVote`；
   - `RankedClient` 重登补弹正确保留 `MaterialTradableUnitsAdded` 与 `MaterialPvpLossIneligible`，`RankedProfileSnapshot` 消费服务端下发的五段门槛与 6 胜里程碑；
   - `client_compile_check.ps1` 与受影响纯 C# 单测全绿。
