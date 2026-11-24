# 《五行奇谈》赛季技能与单槽赛季印（方案 A）设计、实现与 UI 交接

更新：2026-11-23。状态：**本轮服务端（C++/Go 权威仓）完整落表、落协议、落逻辑与单测；前端 Unity UI 由另一台电脑 Codex 按本文第 8 节制作；Java 独立版在 `PARITY.md` 登记待对齐状态。**

---

## 1. 需求分析

### 1.1 背景与目标
《五行奇谈》的核心定位是「**不肝不氪、有空就玩、随时能停几周再回来接着打**」（见 [core-gameplay-loop.md](core-gameplay-loop.md)）。永久养成底盘（八部位装备 660 份封顶 +10 必成、宝宝四维与 20 条目点、62 项常驻现代技能）已确定永久保值、不重置。

在此基础上，负责人提出两项赛季化玩法需求：
1. **赛季技能**：技能分赛季提供变化，在不制造数值膨胀、不逼肝逼氪的前提下，让每个赛季的配招解题思路有新鲜感。
2. **单槽赛季装备栏（「赛季印」，方案 A）**：
   - **只做 1 个独立赛季槽位**，不做一整套多部位赛季装备（降低开发成本与玩家认知负担，不干扰现有八部位与两永久法宝）。
   - **轻数值 + 赛季小机制**：加的纯面板属性极少（照顾很久没上线的回归玩家，不形成排位数值碾压），核心乐趣是本季可选的 4 种轻量主题机制（类似第 3 个赛季法宝位），战外自由切换。
   - **获取与追赶**：只在**正常排位（人宠会武 1V1 / 5V5 / PVP_TEAM）**和**正规 PVE（首领试炼 / PVE_SOLO / PVE_TEAM 胜局）**中积累赛季进度；无每日任务、无周卡锁死；免费打满门槛极低（首测 12 点，约 1～2 小时），同时允许上班族/回归玩家花少量灵石一键或按点追满（**与免费玩家严格同上限，不可溢出继续叠加**）。

### 1.2 不做的范围（YAGNI）
- 不做多部位（4 件/8 件）赛季装备、不做赛季词条洗练/随机强化。
- 不为赛季技能增加银两修习、残页掉落、抽奖或付费解锁。
- 不把速度、暴击、控制命中、控制抵抗放进赛季印面板属性（杜绝回合制先手/控场数值税）。
- 不改动现有 62 项常驻技能、现有八部位装备栏布局与现有两永久法宝上限。
- 借用预设的非正规轮换局（护卫救援 / 物法易势）不带入个人赛季印。

---

## 2. 思想分析

1. **换题目，不换门槛**：永久装备和宝宝是角色的基本盘，保证老玩家隔季回来直接能打；赛季技能和单槽赛季印是每季的“新拼图”，靠机制组合变化而非提高强化上限制造新鲜感。
2. **零剥夺感原则**：任何赛季末会退场的内容都不得让玩家重度肝或重度氪。赛季技能开季全员免费直接满有效重；赛季印 4 种机制 0 点即可免费切换，进度点只把极轻微的血防加成从 0% 平滑补满到 3%（且 12 点极低封顶），回归玩家零压力、追满也只需一两晚或极小额灵石省时间。
3. **单一真相与最小侵入**：
   - 赛季技能直接复用 `SkillTable`（仅追加 1 列 `season_id`），复用现行 `battle_model = 1` 事件化技能管线与 `BattleTableFingerprint`。
   - 赛季印复用现行 `EquipmentEffects` -> `TurnBattleEngine` 法宝机制钩子，但用独立字段 `season_seal` 与独立状态 `season_seal_state` 承载，**零污染永久 `item_id` 与 2 个法宝位**（遵守 [AGENTS.md §7](../rules/invariants.md) 永久 ID 隔离与 [§11.6](../rules/engineering-principles.md) 实例/布局分层）。

---

## 3. 设计分析

### 3.1 配置表设计（权威 Schema）

| 表名 | 权威 Schema | 源表 | 职责 |
|---|---|---|---|
| `SeasonRule` | `data/schema/seasonrule_table.proto` | `data/SeasonRule.xlsx` | 全局赛季排期、赛季印满级点数、PVE/PVP 获取点数、血防上限（硬上限 `<= 500` 万分比）、追赶币种与单价、默认赛季印 ID |
| `SeasonSeal` | `data/schema/seasonseal_table.proto` | `data/SeasonSeal.xlsx` | 各赛季可选赛季印目录（名称、说明、图标键、机制 `effect_id` 及万分比/次数/回合限制参数） |
| `Skill`（扩 1 列） | `data/schema/skill_table.proto` | `data/Skill.xlsx` | 新增 `uint32 season_id = 58;`（`0` = 常驻技能，`> 0` = 对应赛季 ID 的免费限定技能） |
| `Tip`（扩 1 域） | `data/tip/Tip.xlsx` | `data/tip/Tip.xlsx` | 新增 `//season_error base=34000 width=1000` 错误码域 |

#### 首季（`season_id = 1`「破阵之季」）首测数值基线
- **`SeasonRule`（`id = 1`）**：
  - `name = "破阵之季"`，`enabled = true`，`starts_at_ms = 0`，`ends_at_ms = 0`（首测开发期常开，正式开季填全服统一左闭右开 UTC 毫秒时刻）。
  - `seal_max_points = 12`（满级总点数）。
  - `pve_win_points = 1`、`pvp_win_points = 2`、`pvp_other_points = 1`（正常排位胜 +2、平/负 +1，正规 PVE 胜 +1；切磋/逃跑/0 回合/作废 +0）。
  - `max_health_bps_cap = 300`（满级气血 +3.0%）、`defense_bps_cap = 300`（满级防御 +3.0%）；线性换算公式：`floor(cap * min(points, max_points) / max_points)`。
  - `catchup_currency_type = 1`（灵石 `kCurrencySpiritStone`）、`catchup_price_per_point = 10`（每补 1 点 10 灵石，全额 12 点最多 120 灵石）。
  - `default_seal_id = 101`。
- **`SeasonSeal`（4 种本季主题印，0 点即可战外免费切换）**：
  1. `101 破阵锐印`（`effect_id = 1`，`bonus_bps = 1000`，`per_round_limit = 1`）：主目标首段物理直伤后，额外削减伤害 10% 的剩余护盾（每回合 1 次）。
  2. `102 破阵玄印`（`effect_id = 2`，`bonus_bps = 1000`，`per_round_limit = 1`）：主目标首段法术直伤后，额外削减伤害 10% 的剩余护盾（每回合 1 次）。
  3. `103 固阵守印`（`effect_id = 6`，`bonus_bps = 1000`，`cap_bps = 300`，`active_rounds = 2`，`battle_limit = 1`）：前 2 回合首次受敌方扣血直伤减少 10%，上限入战最大气血 3%（每场 1 次，保护回归玩家防开局暴毙）。
  4. `104 聚息援印`（`effect_id = 7`，`bonus_bps = 400`，`per_round_limit = 1`，`battle_limit = 3`）：普通防御后回复入战最大法力 4%（每回合 1 次，每场 3 次，契合首领重劈防御回蓝）。
- **首季免费赛季技能（`Skill` `15001..15004`，`battle_model = 1`，`caster_mask = 1`，`season_id = 1`）**：
  1. `15001`（`SS101` **破阵裂锋**）：敌方最多 3 目标物理直伤（倍率 0.85），附带 2 回合破甲（防御 -15%，`Buff 150011`），耗蓝 6%，CD 2。
  2. `15002`（`SS102` **破阵灵雷**）：敌方最多 3 目标法术直伤（倍率 0.85），附带 2 回合破阵减防（防御 -15%，`Buff 150021`），耗蓝 6%，CD 2。
  3. `15003`（`SS103` **固阵灵障**）：友方最多 3 目标施加 2 回合吸收护盾（`per_level = 14`，`Buff 150030`），耗蓝 6%，CD 2，节奏型保人盾。
  4. `15004`（`SS104` **破阵蓄势**）：友方最多 3 目标提升 2 回合双攻势能（+15%，`Buff 150040`），耗蓝 5%，CD 2，短周期爆发节奏技。

### 3.2 持久化与协议设计

- **玩家存档组件**（`proto/common/component/player_season_comp.proto`）：
  - `PlayerSeasonSealRecord`：`season_id`、`equipped_seal_id`、`progress_points`、`battle_points`、`catchup_points`、`revision`。
  - `PlayerSeasonComp`：`repeated PlayerSeasonSealRecord records = 1;`（按 `season_id` 索引，列表下标不承载业务语义；换季保留历史赛季记录作纪念，不删老记录）。
  - 挂入 `player_database` 字段 `30`：`PlayerSeasonComp season_component = 30 [(OptionPersistGroup) = 3];`（GROWTH 组；一次存盘所有脏行同事务提交，满足 I3 原子性）。
- **幂等与流水**：
  - `ClientRequestKind::kCatchupSeasonSeal = 9`（`client_request_ledger.h`），`ClientRequestRecord` 追加 `season_catchup_points = 13`、`season_catchup_spent = 14`。
  - `TransactionType::TX_SEASON_SEAL_CATCHUP = 35`（`transaction_log.proto`），记录小额追赶扣灵石流水。
- **战斗快照与引擎**：
  - `EquipmentEffects` 追加：`SeasonSealEffect season_seal = 17;`、`uint32 season_max_health_bps = 18;`、`uint32 season_defense_bps = 19;`。
  - `BattlePlayerSnapshot` 追加：`uint32 active_season_id = 24;`（开局由 Scene 冻结，Battle 纵深复核所有 `season_id != 0` 的技能）。
  - `BattleActorState` 追加：`BattleArtifactState season_seal_state = 44;`（下发客户端前清除私有运行态）。
  - `BattleEventItem` 追加：`uint32 season_seal_id = 24;`（`BATTLE_EVENT_ARTIFACT` 触发赛季印时填充，与 `artifact_item_id` 永久法宝清晰区分）。
- **技能书展示扩展**：
  - `SkillCatalogEntry` 追加：`uint32 season_id = 24;`（`0` = 常驻技能，`> 0` = 当前赛季免费限定技能）。
- **客户端 RPC 服务**（`proto/scene/player_season.proto` -> `SceneSeasonClientPlayer`，已登记跨节点换场景可重投白名单 `player_handoff_gate.h`）：
  1. `GetSeasonState`：纯只读查询当前赛季状态、赛季印进度/血防加成/可选印列表、追赶报价与余额、以及本季免费技能列表。
  2. `EquipSeasonSeal`：战外免费切换当季赛季印，立即重算属性并推送面板。
  3. `CatchupSeasonSeal`：带 `client_request_id` 幂等键 + `expected_currency_type` / `expected_total_price` 报价防呆的小额灵石追赶（支持指定补 `points_to_buy` 点，或填 `0` 一键补满剩余点数）。

---

## 4. 漏洞分析与防护清单

| 潜在漏洞 / 风险 | 防护措施 |
|---|---|
| 1. 追赶连点、弱网重发导致双扣灵石或超出满级封顶 | 强制 `client_request_id != 0`，接入 `ClientRequestLedgerComp` 去重；校验 `points_to_buy <= seal_max_points - current_points`；与扣灵石、改进度、记账同一次同步帧提交并触发 `SavePlayerForAssetCommit`。 |
| 2. 玩家点追赶瞬间恰好战斗结算入账，导致按旧差额多花钱 | 请求携带 `expected_currency_type` 与 `expected_total_price`（当 `points_to_buy == 0` 一键补满时，剩余点数变少会让实际总价与预期不符），不符直接回 `kSeasonCatchupPriceChanged` 拒绝并附最新状态让 UI 刷新。 |
| 3. 战斗跨季（切季前一秒开局，切季后结算/进行） | `PrepareBattle` 开局冻结 `active_season_id` 与 `season_seal` 进战斗快照，局内按冻结值结算到底；战斗结束结算时按收件箱永久水位防重入账，且只写入结算时刻的有效赛季并受封顶夹取。 |
| 4. 切季后旧赛季印属性或旧赛季技能残留 | `CollectEffects` / `PlayerSkillSystem` 均实时按当前活跃赛季 `ActiveSeason(nowMs)` 计算；历史赛季记录只读归档，不提供任何属性与技能，旧赛季技能自动从技能书与快照退出。 |
| 5. 策划误配超标属性或重叠激活两个赛季 | `SeasonRule` 启动校验与运行期校验均 fail-closed：血防上限强限 `<= 500`（5%）、同刻最多 1 条激活且生效的赛季、默认印必须存在且属于本季、禁止将速度/控命/抵抗接入赛季印。 |
| 6. 伪造快照把过期赛季技能或超标赛季印送进战斗服 | `TurnBattleEngine::InitPlayers` 独立复核技能 `season_id` 与 `snapshot.active_season_id()`；`InitializeEquipment` 独立复核血防上限 `<= 500` 与 `ValidArtifactDefinition(season_seal)`。 |
| 7. 老角色存档缺字段或降级/混版本丢失数据 | `player_database.season_component = 30` 旧存档默认为空合法；加载无条件挂载、存盘缺组件 fail-closed 拒存；`PlayerSeasonComp` 保留未知字段与历史赛季记录不删改。 |
| 8. 技能升级接口误对赛季技能扣银两 | `PlayerSkillSystem::UpgradeSkill` 检测 `row->season_id() != 0` 直接回 `kSeasonSkillNoUpgrade`，零扣费。 |
| 9. 与 [skill-planning/README.md](skill-planning/README.md) 旧口径冲突 | 明确区分：常驻技能（`season_id == 0`）维持永久保留；赛季限定技能（`season_id > 0`）属本季免费共享池，季末随赛季自然退场，不消耗银两也不设付费解锁。 |

---

## 5. 步骤分析与执行步骤

1. **落盘设计文档**：编写本设计文档并更新 `docs/README.md` 索引。
2. **配表与协议层**：
   - 新增 `data/schema/seasonrule_table.proto`、`data/schema/seasonseal_table.proto` 与对应 `data/SeasonRule.xlsx`、`data/SeasonSeal.xlsx`。
   - 为 `data/schema/skill_table.proto` 增加 `season_id = 58`，在 `data/Skill.xlsx` 与 `data/Buff.xlsx` 追加首季 4 个赛季技能（`15001..15004`）与 6 条配套 Buff（`150010..150040`）。
   - 在 `data/tip/Tip.xlsx` 追加 `season_error` 错误码域（`34000` 起）。
   - 新增 `proto/common/component/player_season_comp.proto`、`proto/scene/player_season.proto`，并按增量规则扩展 `mysql_database_table.proto`、`client_request_ledger_comp.proto`、`transaction_log.proto`、`equipment_effects.proto`、`battle_data.proto`、`player_skill.proto`。
3. **代码生成与客户端同步**：运行 `dev.bat gen`（导表 + `proto-gen` + `gen_schema_index.py`）与同级 `mmorpg-client\gen.bat client`（同步裁剪表、C# Proto、消息号并过编译检查）。
4. **Scene 逻辑层**：
   - 在 `cpp/libs/services/battle/system/season_rules.h` 实现纯规则（活跃赛季解析、线性轻属性计算、赛季印校验、追赶费用计算、技能赛季判定，Scene 与 Battle 共用）。
   - 新增 `cpp/libs/services/scene/player/system/player_season.{h,cpp}` 实现 `ValidateTables`、`GetState`、`EquipSeal`、`Catchup`、`PrepareBattleCredit`、`CollectEffects`、`ActiveSeasonId`。
   - 接线 `player_database_loader.cpp`、`client_request_ledger.{h,cpp}`、`player_attribute.cpp`、`player_equipment_v1.cpp`、`player_skill.cpp`、`player_battle.cpp`、`player_season_handler.cpp`、`player_handoff_gate.h`、`cpp/nodes/scene/main.cpp` 及构建文件（`CMakeLists.txt` / `.vcxproj`）。
5. **Battle 引擎层**：
   - 在 `turn_battle_engine.cpp` 复核 `active_season_id` 与 `season_id`。
   - 在 `equipment_battle_effects.cpp` 校验并执行 `season_seal` 轻机制及独立状态追踪，并在客户端公开快照中清理 `season_seal_state`。
6. **单测与门禁验证**：
   - 新增赛季系统纯规则、Scene 选印/追赶幂等/战斗加分/换季隔离、技能书赛季技能免费生效、Battle 赛季印触发与赛季技能校验单测。
   - 运行 `test_cpp_no_raw_pointer_member.py`、编译受影响工程并运行全部新增与回归单测、检查客户端编译。
7. **双版对账**：更新 Java 仓库 `PARITY.md`，明确记录本功能 C++ 权威版已实现、Java 版待同步与实现。

---

## 6. 给另一台电脑 Codex 的完整 Unity UI 需求与联调契约

> 下面整段可直接发给负责 Unity UI 的 Codex。服务端已生成并同步客户端 RPC 与协议（`SceneSeasonClientPlayerRpc`、`PlayerSeason.cs`、`SeasonErrorTip.cs`、`PlayerSkill.cs`、`SeasonruleTable.cs`、`SeasonsealTable.cs`），**客户端所有数值、报价、解锁与可用态一律以服务端 `GetSeasonState` / `ListSkills` 响应为准，严禁本地硬编码数值或公式**。

### 6.1 玩家看到的两个入口（极简，不多造重型页面）
1. **角色/装备总览页（或主城赛季入口）→ 单槽「赛季印」面板**
   - 在既有八部位装备总览页旁边（或顶部/角落独立位置）只放 **1 个「赛季印」槽位入口**（例如玉印/符箓图标 + 当前所选印名 + 进度徽标 `6/12`），**绝不画多部位赛季装备栏**。
   - 点击打开「**破阵之季 · 赛季印**」轻量弹窗/面板（道家 Q 版风格，米白纸底、玉绿高亮、古金印边，适配手机 `Screen.safeArea` 与微信/抖音右上角胶囊避让，不依赖鼠标 Hover）。
2. **既有技能书页 → 增加「赛季限定」分组标签**
   - 复用现有 `ListSkills` 接口，新增字段 `SkillCatalogEntry.SeasonId`（`0` = 常驻技能，`> 0` = 本季免费限定技能）。
   - `SeasonId > 0` 的条目：
     - 展示角标「**本季免费**」与赛季名；
     - 显示服务端下发的当前生效重数（`Rank` / `MaxRank`）；
     - **隐藏银两修习按钮**（`NextRankCost == 0`，`UpgradeBlockTipId == kSeasonSkillNoUpgrade`），文案提示「本季全员免费生效，随角色等级自动成长，季末随赛季轮换」。

### 6.2 「赛季印」面板布局与交互

| 区域 | 展示内容（数据全部取自 `SeasonStateSnapshot`） | 交互行为 |
|---|---|---|
| **顶栏** | 标题：`SeasonName` +「赛季印」；右侧显示本季时间状态（`EndsAtMs > 0` 时用 `ServerNowMs` 算剩余天数，`0` 则显示「首测常开」）；关闭/返回按钮 | 返回上一级界面；切角色/重连时清空旧缓存并重拉 `GetSeasonState` |
| **顶部进度与轻属性卡** | 进度条 `ProgressPoints / MaxPoints`（如 `6/12`），明文展示来源拆分（战斗 `BattlePoints` + 追赶 `CatchupPoints`）；轻属性展示：`当前气血 +X.X%（满级 +Y.Y%）`、`当前防御 +X.X%（满级 +Y.Y%）`（由 `CurrentMaxHealthBps / 100.0` 格式化为一位小数，保留「不含速度与控制属性」的轻量说明）；下方一行来源说明：「正规会武胜 +`PvpWinPoints` / 完赛 +`PvpOtherPoints`，首领试炼胜 +`PveWinPoints`，无每日上限」 | 纯展示；打完排位/试炼回主城后打开面板自动拉取最新进度 |
| **中部 4 选 1 赛季印列表** | 遍历 `Seals`（本季 4 项：破阵锐印 / 破阵玄印 / 固阵守印 / 聚息援印），逐张展示图标（`IconKey`）、名称（`Name`）、效果说明（`Description`）与「当前佩戴」标签（`Equipped == true`） | 点击未佩戴的印 → 若 `CanSwitchSeal == true` 直接发起 `EquipSeasonSeal(season_id, seal_id)`（免费、零确认弹窗）；`CanSwitchSeal == false`（如战斗中）置灰并展示 `ActionUnavailableReason`。成功后直接用响应 `State` 整份覆盖缓存 |
| **底部轻追赶区** | 若 `ProgressPoints >= MaxPoints`：显示「**本季进度已满**」，隐藏付费按钮；若未满且 `CatchupPricePerPoint > 0`：展示当前灵石余额（`CurrencyBalance`）与两个轻操作按钮：**「补 1 点（消耗 X 灵石）」** 与 **「一键补满（补剩余 N 点，消耗 Y 灵石）」**（金额直接取 `CatchupPricePerPoint` / `CatchupRemainingTotalPrice`，不自算） | 点击后弹轻量二次确认（写清消耗灵石数、补齐后进度与“免费和付费同上限”），确认前先持久化本笔 `client_request_id`（非 0 随机/单调 `ulong`），调用 `CatchupSeasonSeal(client_request_id, season_id, points_to_buy, expected_currency_type, expected_total_price)`；弱网超时未知时仅重放原请求，收到明确成功/拒绝或 `kSeasonCatchupPriceChanged` 刷新状态后再清请求；绝不私自拉起真钱外链支付（灵石走现有合规充值体系） |
| **底部本季免费技能预览条（可折叠/次页签）** | 遍历 `SeasonSkills` 展示本季 4 个免费限定技能图标、名称、一句话说明与「已自动生效 / X 级解锁」状态 | 点击可查看技能详情，或跳往技能书「赛季限定」分组 |

### 6.3 战斗内表现（最小增量）
- **选招栏**：本季免费技能已由服务端自动写入 `BattleActorState.SkillTableIds`，复用现有选招列表即可施放；若 `SkillTableManager.FindById(id)?.SeasonId > 0`，可在图标角落叠一个小「季」角标。
- **战报/触发飘字**：`BattleEventItem` 在 `BATTLE_EVENT_ARTIFACT` 时若 `SeasonSealId != 0`（且 `ArtifactItemId == 0`），按 `SeasonSealTableManager.FindById(SeasonSealId)?.Name` 展示赛季印触发提示（例如「破阵锐印触发」），与永久法宝触发提示同通道展示。

### 6.4 必须覆盖的 UI 异常与状态
1. **首次加载 / 刷新中**：显示加载占位，不显示伪造的 `0/0` 或空印。
2. **暂无进行中赛季（`Available == false`）**：入口隐藏或面板显示 `UnavailableReason`（「当前暂无进行中的赛季」），不展示购买/切印按钮。
3. **战斗中打开 / 跨节点冻结**：展示只读态，禁用切换和追赶按钮并就地展示服务端 `ActionUnavailableReason`。
4. **价格或进度变化（`kSeasonCatchupPriceChanged`）**：例如刚结算到账了 1 点导致一键补满总价变低，弹 Tip 提示「进度或价格已刷新」，并用响应附带的最新 `State` 立即刷新界面，不重复自动扣费。
5. **幂等重放（`Replayed == true`）**：视为本次操作成功，使用最新 `State` 刷新并只提示一次成功。
6. **小游戏与 App 合规**：纯 C# 推导模型（`SeasonUiModel`）不依赖 `UnityEngine` 便于 EditMode 单测；严禁使用 `Task.Run` / `Thread` / 阻塞等待；所有异步回调校验对象存活与连接代次；严禁在本面板放任何 iOS 违规外链支付按钮。
