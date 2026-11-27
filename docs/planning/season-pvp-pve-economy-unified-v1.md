# 《五行奇谈》赛季、PVP/PVE 衔接与强化材料经济统一落地设计（V1 合入 A）

> 日期：2026-11-24  
> 状态：**负责人 2026-11-24 确认「合入 A + 全部实现完毕」，进入权威服务端（C++/Go）、协议配表、客户端非 UI 绑定同步与双服 `PARITY.md` 落地**  
> 上位红线：[项目设定](project-vision.md) · 经济与官方 7:3 总纲：[equipment-economy-charter.md](equipment-economy-charter.md) · 核心循环 V1：[core-gameplay-loop.md](core-gameplay-loop.md) · 赛季技能与单槽赛季印：[season-skills-and-seal-design.md](season-skills-and-seal-design.md)
>
> 说明：本文严格承接并尊重工作区既有并行总纲（不覆写 `project-vision.md` / `core-gameplay-loop.md` / `equipment-economy-charter.md`），作为**赛季 × PVP/PVE × 装备 +11 顶格突破 × 两阶段材料账 × 3 首领/轮换娱乐场 × 1V1/3V3 冷启动开关**的统一工程契约。
>
> **2026-11-26 反「上班」升级覆盖（详见 [anti-chore-remediation-v1.md](anti-chore-remediation-v1.md)）**：阶段二每周 150 份材料上限已由“周一 05:00 一刀切清零”升级为**4 周滚存蓄水池**（每周一 05:00 补充 `+150` 份、未赚取额度滚存累计至多 **4 周 = 600 份**；首入阶段二初始 150 份防新号爆发，抗时钟回拨）。

---

## 1. 需求分析

### 1.1 背景与要解决的核心矛盾
个人开发《问道/梦幻》风格横屏回合制手游，同时面临四个必须统一解决的现实问题：
1. **前期低在线人数**：开服阶段在线少，若同时开放 1V1～5V5 五档正式队列并拆分 5 张独立榜，必然出现鬼服排不到人与空榜劝退；但底层仍须保留 1V1～5V5 扩展能力，在线达标后一键改表全开。
2. **策略深度与 PVP/PVE 衔接**：赛季不能只是一张孤立的作业进度条或“第 3/6/9 回合防御回蓝”这类拖时长词缀，必须靠**已落码的 4 个免费赛季技能 + 单槽 4 选 1 赛季印**与**3 只差异化预告首领（慢动作重劈 / 灵障蓄盾 / 玄毒封灵）**形成跨 PVP/PVE 共用的战术解题体验。
3. **不肝、免费同战力上限与卖强化材料的兼容**：
   - 首套 600 份（300 打造 + 300 百炼石，`+1～+10` 必成）沿用并行总纲的**无每日打卡、永久 660 份总池**（含至多 60 份起步可交易额），让零氪与上班族随时能停、随时能续，不因漏登掉队；
   - 为了给中氪玩家留出长线精进空间、给免费玩家与聚宝斋提供持续材料买方，同时避免被骂“又肝又氪/概率骗氪”，**只把唯一最后 1 级（`+11` 顶格突破）**做成「**10% 概率提前一发入魂 + 失败不掉级且保底 +1 + 满 20 次必成（永不清零）**」，且只加极克制的基础攻防（`+20% → +24%`，绝不加速度与控制命中），装备升档 **100% 继承强化等级与保底进度**；
   - 角色拿满阶段一 660 份首套池后，自动切入**阶段二（`+11` 顶格周平滑精进期）**：每周共享上限 **150 份绑定自选材料**（UTC+8 周一 05:00 重置，每周玩 1.5～2 小时拿满，约 1.5 个月免费出首件 `+11`、约 5.5 个月免费叠满四件 `+11`，持续搬砖产可交易资格另走 [equipment-economy-charter.md](equipment-economy-charter.md) 月卡门槛）。
4. **废止历史口径分叉**：正式「人宠会武」（`PVP_TEAM`）拍死为**本人真实养成**；旧稿 [ranked-play-season-ui-candidate.md](ranked-play-season-ui-candidate.md) 的“正式排位全员免费预设”条款正式作废，已入表的 6 套 `RankedPreset` 仅保留给**轮换娱乐场（护卫救援 PVE / 物法易势 5V5 公平团战）**复用。

---

## 2. 设计分析

### 2.1 装备唯一 `+11` 顶格突破（概率提前成 + 满 20 次必成保底）

| 项目 | 正式规格（`EquipEnhance` `profile_id = 1`） |
|---|---|
| **`+1～+10`（沿用）** | 四件基础装（武/帽/衣/鞋）100% 必成，`fail_level_loss = 0`，`cost_gold = 0`，`pity_limit = 0`；单件累计 75 百炼石（四件 300），每级原始基础攻/防 +2%（+10 共 +20%），鞋速度/词条/法宝不强化。 |
| **`+11`（新增唯一顶格）** | 仅 `profile_id = 1` 基础装，新增行 `id = 1011, level = 11`：`bonus_permyriad = 2400`（总加成 +24%，相对 +10 仅多 +4% 原始基础攻/防，折算总面板约 +2%～3%）、`success_rate = 1000`（单次 10%）、`fail_level_loss = 0`（永不掉级）、`cost_gold = 0`、`material_id = 3002`（百炼石）、`material_count = 100`（2026-11-25 开服定稿由 30 调至 100，合 ¥10/手）、`pity_limit = 20`（前 19 次各 10%，第 20 次即 `pity_count >= 19` 时 100% 必成）。 |
| **期望与保底周期** | 单件期望 `E[X] = (1 - 0.9^20)/0.10 ≈ 8.78` 次（约 878 颗百炼石 / 等价 ¥88，免费约 1.5 个月；保底上限 2,000 颗 / ¥200）；四件总期望约 3,514 颗（等价 ¥351，每周 150 份约 5.5 个月，全非酋上限 8,000 颗 / ¥800）。 |
| **持久化与继承** | `EquipInstanceData` 新增 `uint32 enhance_pity_count = 5;`；成功时归零，失败时 `+1`；随实例完整穿脱、存盘、跨区迁移，未来装备升档 100% 无损继承 `enhance_level` 与 `enhance_pity_count`。 |
| **合规公示与防重放** | `EquipEnhancePreview` 新增 `pity_limit = 10`、`current_pity_count = 11`、`pity_guaranteed_next = 12`，客户端必须精确公示单次概率与保底次数（满足 App Store 3.1.1 与小游戏审核）；`EnhanceEquipRequest` 新增 `optional uint32 expected_pity_count = 4`，与 `client_request_id` 联手杜绝弱网失败重放双扣材料；`ClientRequestRecord` 新增 `uint32 enhance_pity_count = 20` 保证重放返回首次精确保底值。 |

### 2.2 两阶段装备材料账本 + PVP 防挂机门槛（合入 A）

1. **战斗结算权威事实（Battle → Scene）**：
   - `BattleSettlementData` 新增：
     - `uint32 manual_action_rounds = 26;`（本局玩家成功提交本人或本人宝宝手动指令的独立回合数）；
     - `bool effective_combat_contribution = 27;`（本人或本人宝宝整场造成 >0 有效伤害、有效治疗、护盾吸收、有效控制或驱散任一项）；
     - `uint32 battle_difficulty = 28;`（冻结自 `DungeonTable.difficulty`：0 常规 / 1 普通 / 2 困难）；
     - `uint32 boss_trial_kind = 29;`（冻结自 `DungeonTable.boss_trial_kind`）。
   - **PVP 负局 / 平局四重防挂机裁决**：必须同时满足 `!fled && total_rounds >= 3 && manual_action_rounds >= 2 && effective_combat_contribution`，不达标返回 0 份且不消耗永久 660 额度，并在 `BattleEndS2C` / `PlayerBattleResult` 标出 `material_pvp_loss_ineligible = true`。
2. **单场材料基准与可交易隔离**：
   - **普通 PVE 胜局 / 护卫救援胜局**：基础 **10 份**（胜局完整 10 份至多含 **1 份可交易**）；
   - **困难试炼胜局**（`battle_difficulty == 2`）：基础 **10 份**（至多 1 可交易）+ 困难奖 **5 份**（全自用）= **15 份**；
   - **正式会武（`1V1`/`5V5`/`PVP_TEAM`）与物法易势 5V5（`MATCH_MODE_POLARITY_5V5`）**：
     - 真人胜局：基础 **10 份**（至多 1 可交易）+ 胜奖 **5 份**（全自用）= **15 份**；
     - 达标平/负局：基础 **10 份**（**全自用，0 可交易**）；
     - 未达标负/平、主动逃离、零回合、作废、切磋、PVE 失败：**0 份**。
3. **两阶段额度账本（`EquipmentMaterialRewardLedger`）**：
   - **阶段一（`lifetime_earned < 660`）**：无每日、无周清，累计上限 **660 份**；先计入基础（至多 10），再计入放得下的额外奖（至多 5）；接近 660 尾数截断且不满基础 10 的尾奖无任何可交易份数；阶段一可交易累计上限 **60 份**（`tradable_earned_total <= 60`，用尽后完整胜局仍给 10 份全自用）。存量旧档旧 `available_units` 完整保留为自用余额、不抵扣新 660。
   - **阶段二（`lifetime_earned >= 660`）**：自动开启 `+11` 顶格周平滑精进池——**每周上限 150 份自选材料**（2026-11-25 开服定稿由 100 调至 150，UTC+8 周一 05:00 重置，`week_starts_at_ms` 抗时钟回拨），阶段二全部产出**仅自用（`bound = true`，0 免费可交易）**，按同样单场 10/15 与防挂机门槛累计（10 场困难/PVP 胜或 15 场普通胜正好整除拿满），供免费玩家约 1.5 个月出首件 `+11`、约 5.5 个月叠满四件 `+11` 或收集全法宝/第二套。
   - **领取与绑定严格隔离**：`ClaimEquipmentMaterialReward` 每次批次 `min(20, available_units)`，允许步长 5（支持 5/10/15/20 任意拆分）；该批兑现的可交易份数 `batch_tradable = min(batch_units, available_tradable_units)`（优先分配给打造的份额，余数分配给强化石），发放入包时可交易堆 `bound = false`、自用堆 `bound = true`，防止不同绑定态混堆洗白。

### 2.3 3 只预告首领、慢动作重劈、轮换娱乐场与 `1V1`/`3V3` 冷启动开关

1. **配置表结构化驱动（零硬编码副本 ID）**：
   - `DungeonTable` 追加 `boss_trial_kind = 7`、`difficulty = 8`、`round_limit = 9`：
     - `boss_trial_kind = 1`：破阵首领·回春（普通 id=101 / 12 回合；困难 id=102 / 15 回合）；
     - `boss_trial_kind = 2`：破阵首领·护势（普通 id=103 / 12 回合；困难 id=104 / 15 回合）；
     - `boss_trial_kind = 3`：灵障首领（普通 id=105 / 12 回合；困难 id=106 / 15 回合）；
     - `boss_trial_kind = 4`：玄毒首领（普通 id=107 / 12 回合；困难 id=108 / 15 回合）；
     - `boss_trial_kind = 5`：护卫救援（轮换 PVE，id=109 / 8 回合，护卫阵亡即判救援失败，击败首领且护卫存活即胜）；
     - `boss_trial_kind = 6`：物法易势（轮换 PVP `5V5`，id=110 / 16 回合，每 2 回合交替物势/法势直伤 ×1.15，打满 16 回合双方存活判平）。
   - `MonsterTable` 新增 3 只首领 + 4 种护卫正式行（`101..107`，均带 `is_boss = 1` 标识首领），按开局冻结的真人数 `N`（1～5）线性放大敌人气血上限，不追加 `N-1` 只杂兵。
   - `PlayModeTable` 追加 `repeated uint32 allowed_team_sizes = 6 [(cfg_slots) = 5];`：留空表示开放 `1..team_size_max` 全档；非空则只开放指定人数档。正式 `PVP_TEAM`（id=6）配置 `team_size_max = 5, allowed_team_sizes = [1, 3]`，冷启动只开 `1V1` 与 `3V3` 聚人保秒排，在线达标后改表即一键全开五档。
2. **战斗引擎权威规则（`TurnBattleEngine`）**：
   - **显式回合上限**：`Dungeon.round_limit > 0` 时直接使用 `round_limit`，不再被 `time_limit / 6` 误掐；
   - **慢动作重劈时序**：破阵首领每 3 回合预告下一轮重劈目标；兑现回合首领移出常规速度序，排在**本回合所有普通行动结束后、回合末 Buff Tick 前**执行，确保慢速单位点防御 100% 先建立减伤；被封物/封灵/眩晕/冰冻/昏睡或目标失效则取消且不补招；
   - **三只首领与四种护卫确定性机制**：
     - 回春护卫：普通 R3/6/9/12、困难 R2/5/8/11（与重劈重叠）为首领回复最大生命 8%（普通）/ 10%（困难），受减疗限制，被控取消；
     - 护势护卫：每 3 回合物防/法防交替（普通 30% / 困难 40% 对应类型减伤，可驱散）；
     - 灵障首领 + 灵障护卫：护卫每 3 回合为首领叠加吸收护盾，首领释放高威胁法术重击；
     - 玄毒首领 + 缚灵护卫：护卫周期施加封灵压制，首领预告群体毒蚀；
   - **护卫救援特殊终局**：任一行动组/回合末结算若护卫阵亡（含与首领同归于尽）立即判 `SIDE_B_WIN`（救援失败）；否则己方存活且首领阵亡立即判 `SIDE_A_WIN`（救援成功）；
   - **物法易势 5V5 轮换机制**：`MATCH_MODE_POLARITY_5V5 = 8`（独立非计分模式，绝不写 Elo 或投榜）；开局由冻结种子等概率确定首势（物势 / 法势），每 2 回合切换，强势类型普攻主段或主动技能原生直伤段 ×1.15（常规增减伤后、护盾吸收前向下取整一次）；打满 16 回合双方存活判 `BATTLE_OUTCOME_DRAW`；
   - **公开状态同步**：`BattleStateS2C` 携带 `telegraphs`、`active_polarity`、`next_polarity_switch_round`、`protected_guard_actor_id`，重连与回放逐帧确定性还原。

---

## 3. 思想分析

1. **为什么 `+11` 只做最后 1 级且必须“概率 + 保底 + 不掉级 + 100% 继承”？**  
   多级概率强化会摧毁新手留存并制造骗氪口碑；完全必成低封顶又会导致满套后缺乏材料买方。把概率精进严格收束到唯一的 `+11`（+4% 原始基础攻防 + 光效，永不掉级，满 20 次必成），既让前 10 级 100% 保持良心体感，又为中氪与玩家市场创造长期自愿消耗出口。
2. **为什么采用「阶段一 660 无周期 + 阶段二周平滑 150」两段式账本？**  
   既完全尊重并行总纲 [project-vision.md](project-vision.md) 对新手/普通玩家“零每日打卡、零漏签焦虑”的承诺，又通过阶段二温和的周平滑上限（每周 1.5～2 小时即可拿满）防止脚本 24 小时刷穿 `+11` 顶格，使免费约 1.5 个月出首件、约 5.5 个月叠满四件 `+11` 的节奏可控且不肝。
3. **为什么 `PlayMode.allowed_team_sizes` 用配置开关控制 `1V1`/`3V3`，且轮换 PVP 单独给 `MATCH_MODE_POLARITY_5V5`？**  
   低在线人数期最怕匹配池碎裂与串分。配置化人数档白名单让冷启动聚人、扩容零改码；独立非计分模式保证娱乐轮换绝不会通过旧 `PVP_TEAM` 通道污染正式 Elo 或排行榜。

---

## 4. 漏洞分析与防线清单

| 潜在漏洞 / 风险 | 硬防线 |
|---|---|
| **`+11` 概率失败后弱网重放导致双扣 100 颗百炼石** | 失败不改等级，故在 `EnhanceEquipRequest` 增加 `optional uint32 expected_pity_count = 4` 并结合 `client_request_id` 持久化账本（记 `enhance_pity_count`）；任一不匹配直接拒回 `kEquipEnhanceLevelChanged`，零扣费。 |
| **PVP 挂机/速死脚本刷材料与可交易额** | Battle 引擎逐回合确定性统计 `manual_action_rounds` 与 `effective_combat_contribution` 并冻结进 `BattleSettlementData`；Scene 侧对 PVP 负/平强制校验 `>=3 回合 && >=2 回合手操 && 有效贡献`，且负/平局 0 可交易。 |
| **可交易材料与仅自用材料入包合堆洗白** | `ClaimEquipmentMaterialReward` 明确区分可交易与自用两批 `InitItemParam`（`bound = false` vs `bound = true`），沿用背包层不同绑定态不可合堆规则，杜绝洗白。 |
| **老角色旧存档 `EquipmentMaterialRewardLedger` 迁移损坏** | 新增 `migrated_to_v2` 标志，旧档存量 `available_units` 全额保留为自用可领余额，不扣新 660 上限；`Validate` 兼容旧字段与 5 的倍数余额步长。 |
| **慢速角色点防御仍吃满首领重劈** | 重劈在 `TurnBattleEngine::ResolveCurrentRound` 中独立移至普通行动循环之后、回合末 Tick 之前执行，确保全员本回合防御 100% 先于重劈生效。 |
| **护卫救援同招击杀首领与护卫被误判胜利** | 终局判定严格先检护卫阵亡（任一时刻护卫死亡立即判 `SIDE_B_WIN`），即使同段/同回合首领亦亡仍判救援失败。 |
| **物法易势 5V5 混入正式 `PVP_TEAM` 导致串分** | 新增独立 `MATCH_MODE_POLARITY_5V5 = 8`，不在 `ratingrule.Rated` 与 `rankBoardTypes` 之内，绝不写 `match_rating` 或投榜。 |
| **冷启动期间绕过客户端 UI 强排 `2V2`/`4V4`/`5V5`** | `go/match` 的 `catalog.Catalog` 在 `Pools()`、`JoinQueue`、`StartTeamMatch` 及提交点统一校验 `AllowedTeamSize(mode, configId, size)`，客户端改包一律拒回 `ErrModeNotOpen`。 |

---

## 5. 步骤分析与执行步骤

1. **文档与索引**：落盘本文，给已废弃的 [ranked-play-season-ui-candidate.md](ranked-play-season-ui-candidate.md) 与 [ranked-play-v1-implementation.md](ranked-play-v1-implementation.md) 顶部添加作废指向横幅；运行 `python tools/scripts/gen_docs_index.py` 更新 `docs/README.md`。
2. **源表与协议**：
   - 更新 `data/schema/equipenhance_table.proto`、`data/schema/playmode_table.proto`、`data/schema/dungeon_table.proto` 及对应 `.xlsx`（`EquipEnhance.xlsx`、`PlayMode.xlsx`、`Dungeon.xlsx`、`Monster.xlsx`）；
   - 更新 `proto/common/component/item_base_comp.proto`、`client_request_ledger_comp.proto`、`player_battle_result_comp.proto`、`proto/scene/player_bag.proto`、`proto/battle/battle_data.proto`、`proto/battle/player_battle.proto`、`proto/match/match_service.proto`；
   - 执行 `dev.bat gen`（导表 + `proto-gen` + schema 索引）。
3. **C++ 服务端实现**：
   - 装备 `+11` 顶格突破（`equip_attribute_rules.h`、`player_equip.{h,cpp}`、`player_bag_handler.cpp`、`client_request_ledger.{h,cpp}`）；
   - 两阶段材料账本与 PVP 防挂机门槛（`player_equipment_material_reward.{h,cpp}`、`player_battle.cpp`、`battle_result_history.h`）；
   - 3 只首领、慢动作重劈、护卫救援、物法易势 5V5 与对局事实追踪（`turn_battle_engine.{h,cpp}`、`table_battle_data_provider.{h,cpp}`、`battle_data_provider.h`）。
4. **Go 匹配服务实现**：
   - `go/match/internal/catalog/catalog.go` 支持 `allowed_team_sizes` 白名单与 `MATCH_MODE_POLARITY_5V5`（固定每侧 5 人、10 人成局、非计分）；
   - `joinqueuelogic.go`、`team_battle.go`、`global_pool_matcher.go`、`authority/` 各入口与校验同步放行 `MATCH_MODE_POLARITY_5V5` 并拦截未开放的人数档。
5. **Unity 客户端同步与非 UI 适配**：
   - 在 `mmorpg-client` 执行 `.\gen.bat client`（裁剪表、C# Proto、消息号与编译体检）；
   - 更新 `PlayerFeaturesClient` / `EquipBagModel` / `PlayerFeaturesClient.MaterialChoices.cs` 支持 `+11` 保底预览/请求参数与两阶段材料快照。
6. **门禁与测试验证**：
   - 运行 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`；
   - 串行 `msbuild /m:1` 编译受影响 C++ 工程并运行 `bag_test.exe`、`turn_battle_engine_test.exe` 等新增与回归单测；
   - 运行 `go test ./...` 覆盖 `go/match`；
   - 运行客户端 `tools/client_compile_check.ps1`；
   - 更新独立 Java 仓 `xuanming-server-mmo/PARITY.md` 记录 C++/Go 权威版实现与 Java 同步状态，更新 `docs/PROGRESS.md`。

---

## 6. 2026-11-25 开服前价格锁死修订（首套 660/¥60 不动，+11 调至 100 颗/次 + 二阶段周上限 150 份）

### 6.1 需求分析
- 上线后绝不暗改涨价或砍产出，须在开服前一次性把长线商业化深度与物价锚定稳。
- 原口径单份参考锚 `¥0.1/份` 下，首套 600 份（¥60，免费 4～8 小时）亲民好破冰，但四件 `+11` 原单次 30 颗仅对应期望 `1,054` 颗（¥105），对覆盖个人开发月度固定成本与维持玩家市场百炼石买方偏薄；若反过来把首套强改 6666 份或 ¥300，要么单份碎成 ¥0.01/¥0.05、要么打死新手 ¥6–¥30 首充转化。

### 6.2 设计分析
- **维持不动**：单份参考锚恒定 **¥0.1/份**；阶段一永久总池 **660 份**（首套 600 份 = ¥60，可交易终身上限 60 份 = ¥6，单场 10/15 份、4～8 小时满首套）零改动。
- **仅改 2 项参数**：
  1. `EquipEnhance.xlsx` 行 `id=1011`（`profile_id=1, level=11`）：`material_count` 由 `30` 调整为 **`100`**（合 ¥10/手；10% 概率、不掉级、20 次硬保底、+24% 总加成不变）；单件期望约 `878` 颗（¥88，保底 ¥200），四件全满期望约 `3,514` 颗（¥351，保底 ¥800）。
  2. 阶段二每周全绑定平滑上限 `kWeeklyLimit` / `WEEKLY_MATERIAL_LIMIT` 由 `100` 调整为 **`150`**（10 场困难/PVP 胜或 15 场普通胜刚好整除，周玩 1.5～2 小时拿满；免费约 1.5 个月出首件 `+11`、约 5.5 个月四件全满）。

### 6.3 思想分析
- **低门槛破冰 + 长线深度分层**：新手前 8 小时只卖爽感与留存（首套 ¥60、6–30 元小额提速包 + ¥10 易容卡高转化），中氪付费与玩家市场 3.3 倍百炼石需求精准落在“跳过数月等待”的 `+11`（¥10/手补一手、¥351 期望全满）以及已落码的月卡（¥30/月）、赛季印/外观手记、定星保底符（`3405`）、天工秘墨（`3408`）和永久外观衣柜上。

### 6.4 漏洞分析与防线
- **跨端硬编码失配**：客户端 `CoreGameplayLoopModels.cs` 存在 `WeeklyLimit == 0` 时的 `100u` 兜底值，与服务端 C++/Java `kWeeklyLimit = 150` 同步修正并补单测覆盖；UI 仍优先消费权威快照字段。
- **测试夹具百炼石备货不足**：`+11` 满 20 次硬保底消耗升至 `2,000` 颗（含 `+9→+10` 共 `2,015` 颗），同步修正 C++ `PlayerEquipV1Test` 与 Java `EnhancePityAndMaterialLedgerTest` 并新增精确余量与阶段二 `145→150` 截断断言。
- **并行会话工作树隔离**：执行当日主仓存在并行 GuildWar 会话未提交改动，仅修改 `EquipEnhance.xlsx` 单格与阶段二周上限相关源码，绝不触碰并行会话文件；Java 端逻辑常量与单测当场对齐，`config-data/tables/equipenhance.pb` 待主仓提交后随 `ContractSync` 统一同步。

### 6.5 步骤分析与执行步骤
1. 修改 `data/EquipEnhance.xlsx` `id=1011` `material_count: 30 → 100` 并校验零其他单元格变动，运行导表器产出 `generated/tables/equipenhance.{json,pb}` 与 `generated/client_tables/equipenhance.pb`。
2. 更新 C++ `player_equipment_material_reward.{h,cpp}`、`client_request_ledger.cpp` 及 `bag_test` 单测，执行静态门禁与串行 MSBuild + 实跑回归。
3. 在 `mmorpg-client` 执行 `tools/sync_tables.ps1`，更新 `CoreGameplayLoopModels.cs`、`CoreGameplayLoopTests.cs` 与 `Docs/design/core-gameplay-loop-ui-handoff.md`，跑通编译体检与 NUnit 单测。
4. 在 `xuanming-server-mmo` 更新 `PlayerClientRequestLedger.java`、`EquipmentCraftService.java`、`EnhancePityAndMaterialLedgerTest.java` 与 `PARITY.md`，跑通 Maven 定向单测。
5. 同步更新本设计文档、`boss-trial-planning-ui.md`、`battle-festival-beast-implementation-contract.md` 与 `docs/PROGRESS.md`。
