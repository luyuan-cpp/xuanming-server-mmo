# 「瑞兽迎春」活动实现设计与 UI Codex 独立接口契约

> 状态：实现落地方案与权威契约（2026-11-24）  
> 适用仓库：`xuanming-server-mmo-cpp`（C++ 场景/战斗 + Go 撮合/交易）、`mmorpg-client`（非视觉数据适配层）  
> 关联既有设计：[battle-festival-beast-v1.md](battle-festival-beast-v1.md)、[battle-festival-beast-logic-handoff.md](battle-festival-beast-logic-handoff.md)、[battle-festival-beast-ui-handoff.md](battle-festival-beast-ui-handoff.md)、[season-pvp-pve-economy-unified-v1.md](season-pvp-pve-economy-unified-v1.md)

---

## 一、需求分析

1. **玩法与排期**：
   - 活动名“瑞兽迎春”，神兽工作名“春禧兽”，名称、图标键与资源引用均由配置表提供，不写死。
   - 单人携本人 1 只合法出战宝宝挑战 1 只首领（首发单难度，无护卫、无组队、无排行、无活动币、无通行证）。
   - 单期开放 14 个完整 24 小时，全时段可参与；正式日期由运营填表配置，默认未排期关闭，不自行开放。
   - 当期累计胜 3 场即产生永久待领资格，免费领 1 只固定种类、固定首发资质模板的神兽（不要求连续登录或连胜；无门票/体力/每日挑战次数上限）。
2. **战斗节奏与交互**：
   - 使用玩家真实属性、装备、合法技能与出战宝宝，不授予临时全技能或免费预设。
   - 3 回合循环：第 1/2/4/5/7/8/10/11 回合普通单体物理攻击；第 3/6/9/12 回合慢速物理重击（普通攻击同一基础伤害的 180%、单目标、零法力，首领额外暴击/连击/反震/吸血为 0）。
   - 第 2/5/8/11 回合操作窗口打开前，从合法存活人物与宝宝中确定性随机预告下一回合重击目标；只抽一次，重连/回放不重抽。
   - 重击占首领当轮唯一正常行动，排在全员常规行动之后、回合末状态清理与摘防御之前；不改变全游戏防御优先级。
   - 被点名者兑现轮防御生效（上轮防御不跨轮），控制/免伤/睡眠唤醒/冰冻/复活沿公共规则；受控取消不补普攻、不延后；目标失效取消不换打另一方。
   - 按开局人物生效等级匹配固定等级档（装备/宝宝强弱不参与选档），12 回合内击败首领获胜，打满未胜判负；覆盖物攻/法攻/控辅/治疗配普通输出宠四种可行打法。
3. **局外隔离与失败恢复**：
   - 仅局内起始人物与出战宝宝血蓝补满，禁用背包道具与捕捉。
   - 活动对局严格资产隔离：绝不把局内血蓝、复活、技能、装备、词条、冷却写回永久养成；局外原计时照常推进，不回滚整份存档。
   - 局外低血存活宝宝可入场；0 血倒地宝宝拒绝入场。
   - 发起意图锁定 `locked_pet_id`，`PrepareBattle` 再次复核，杜绝预检后换宠/收回导致无宠或错宠开局。
   - 失败、逃跑、作废不增胜场、不清空已有进度；逃跑时人物与宝宝一起离场；断线/关界面不等同逃跑，沿现有重连托管，结果未知禁止重复开局。
4. **奖励、经济与一致性**：
   - 胜场按 `(角色, 期次)` 记录且封顶 3；满 3 胜生成永久待领资格并冻结当时奖励定义与版本（跨结束/离线/换期保留）；未满 3 胜不跨期累计。
   - 同角色对同一稳定 `reward_identity` 终身最多领 1 只，复刻复用同一身份。
   - 神兽复用正常 `PetInstance` 养成体系（不继承首领重击、无额外词条点、不增出战位）；新增专属赠送容量 `exclusive_gift_slots = 1`，独立于普通 `max_pets`，普通商店/捕捉/交易不得占用。
   - 神兽绑定角色（`transfer_policy = 1`），禁止单独交易、赠送、寄售或分解/合成，且校验前置到整单资产副作用之前；合法整角色转让保留宠物与领取记录不重置资格。
   - 胜局接入现有全游戏装备材料自选账（`PlayerEquipmentMaterialRewardSystem::PrepareBattleCredit`，`MATCH_MODE_PVE_SOLO` 难度 1，每胜至多 +10 份）；不额外发阶段材料，关闭普通经验、银两、掉落、捕捉。
     - **现状口径对齐说明**：本仓 `xuanming-server-mmo-cpp` 现行 `PlayerEquipmentMaterialRewardSystem` 已在 `5d96a4f79`（`season-pvp-pve-economy-unified-v1.md`）升级为两阶段池（阶段一首套 660 份永久池无日限，单胜至多 +10；阶段二每周一 UTC+8 05:00 重置 150 份全绑定；同时保留 UTC+8 05:00 换日时间戳字段与兼容字段）。本活动直接复用该单一真相入口、不另建平行材料账，并在契约中下发权威 `quota_mode`、`earnable_remaining_units`、`expected_win_units`、`available_units`、`day_starts_at_ms`、`next_day_reset_at_ms`、`week_starts_at_ms`、`next_week_reset_at_ms`，让 UI 零硬编码即可准确展示。
   - 服务端 UTC 毫秒裁定、左闭右开 `[effective_start_at_ms, effective_end_at_ms)`；UI 显示北京时间 UTC+8。
   - 截止前已持久准入的在途局允许结算至原期；结束后保留 7 天集中领奖展示，之后转入历史态，但已达成永久资格永远可领。
   - 领取时宠物实例 + 领取记录 + 请求回执同一同步提交并经 `PlayerLastPersistedSnapshotComp` 确认后才公开成功；同 UUID 重试回原结果，不同 UUID 并发至多得到同一只宝宝，号源失败或存盘未知不消耗资格；新设备无本地 UUID 通过角色查询即可恢复在途开局、在途/已完成领取及原 `pet_id`。

---

## 二、设计分析

### 2.1 分层架构与单一事实源

| 层级 | 模块 / 路径 | 职责边界 |
|---|---|---|
| 配置表 | `Pet.xlsx`、`Monster.xlsx`、`Dungeon.xlsx`、`FestivalBeastActivity.xlsx`、`FestivalBeastTier.xlsx`、`Tip.xlsx` | 活动期次与排期、8 个全覆盖无重叠等级档（1..120）、神兽行 `id=101`（`transfer_policy=1, exclusive_gift_slots=1, fixed_initial_aptitude=true`）、副本行 `id=111`（`boss_trial_kind=7` 且不进 `PlayMode.xlsx`）、错误码段 `festival_beast_error`（base=36000）。 |
| 持久化 | `PlayerFestivalBeastComp`（`player_database` field 33，`OptionPersistGroup=3` GROWTH 组） | 按期次存胜场（上限 3）、按 `reward_identity` 存永久资格（冻结 `reward_definition_version` / `reward_pet_table_id`）与终身领取回执（`request_id`, `pet_id`, `claimed_at_ms`）、持久化在途开局 `pending_launch` 供跨设备恢复。与 `PlayerPetComp`、`ClientRequestLedgerComp`、`PlayerBattleResultComp` 同属 GROWTH 组，同事务落盘。 |
| 场景服（C++） | `festival_beast_rules.h`、`player_festival_beast.{h,cpp}`、`player_pet.{h,cpp}`、`asset_op_system.cpp`、`player_battle.cpp`、`player_activity_handler.cpp` | 规则纯函数、配置完整性门禁、目录项与详情聚合、发起挑战（持久化 `pending_launch` + 异步签名调用 `MatchInternal.StartActivityBattle`）、`PrepareBattle` 二次锁宠复核与满血蓝隔离快照、收件箱严格资产隔离结算入账、原子领宠提交与落盘确认、宠物专属容量与禁交易前置校验。 |
| 撮合服（Go） | `go/match`（`activitybattlelogic.go`、`direct_intent.go`、`direct_admission.go`、`gather.go`、`catalog.go`） | `MatchInternal.StartActivityBattle` 支持 `BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST`（仅验 `scene` 调用方签名，与 `guild` 互不串用）；`MatchDirectIntentRecord.Kind = 5` 单人幂等准入并在 SQL 提交点以 DB `now()` 裁定 `[start, end)`；向 `PrepareBattleRequest` 和 `CreateBattleRequest` 透传冻结的 `BattleActivityContext`；禁止 `PlayMode`/`JoinQueue` 绕过。 |
| 战斗引擎（C++） | `turn_battle_constants.h`、`turn_battle_engine.{h,cpp}` | `kBossTrialKindFestivalBeast = 7` 校验必须带 `BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST`；按冻结档位覆盖首领血/物攻/防/抗/速/名且强制暴击/连击/反震/吸血为 0；R2/5/8/11 预告下轮目标（确定性单次抽取），R3/6/9/12 慢速重击（180%），输出 `resolved_telegraphs` 终态（已发动/受控取消/目标失效取消）；禁用道具/捕捉；0 经验/银两/掉落；结算原件回传 `festival_beast`。 |
| 交易服（Go） | `go/trade/internal/commerce/engine.go` + 场景 `asset_op_system.cpp` | 交易前双端拒收 `transfer_policy != 0` 宠物，场景整单扣款预检在任何副作用前拦截。 |
| 客户端非视觉层 | `mmorpg-client/Assets/Scripts/Game/FestivalBeast/`（`FestivalBeastModels.cs`、`FestivalBeastClient.cs`） | 纯 C# 不可变状态模型、UTC+8 展示时间格式化、64 位 ID 精确字符串、基于 `DurableRequestFileStore` 的原请求持久化与新设备恢复、会话/角色切换隔离，不碰任何 UI/Prefab/图片。 |

### 2.2 关键一致性状态机

1. **挑战开局（Launch）**：
   - `SceneActivityClientPlayer.StartFestivalBeastChallenge(client_request_id, activity_instance_id, config_revision)`
   - 场景同步校验：配置完整性 → 当前时间处于 `[start, end)` → 版本匹配 → 角色可写且非战斗/非换图 → 若已有未决 `pending_launch`（同请求直接回原状态，异请求拒绝并发） → 校验出战宝宝存在且 `health > 0` 且等级合法 → 写入 `pending_launch(state=LAUNCHING, locked_pet_id, tier_id, ...)` → 异步发送 `MatchInternal.StartActivityBattle`（携带 `"scene"` HMAC 签名，回调仅捕获 `playerId`、`requestId`、`activityInstanceId`，绝不捕获裸指针或 `entt::entity`）。
   - Match 收到后验 `"scene"` 签名 → 按 `(kind=5, owner=player_id, request_high, request_low)` 幂等查重 → 在 SQL 事务内按库时钟验证 `effective_start_at_ms <= now < effective_end_at_ms` 并提交成局与占位 → 异步 gather 调用 `Scene.PrepareBattle(activity_context)` → 场景再次复核 `active_pet_id == locked_pet_id && pet.health > 0` → `BattleNode.CreateBattle` → 成功进入战斗。
   - 若 `StartActivityBattle` 明确被拒或 `CancelBattlePrepare` / `VOID` 到达，清理对应 `pending_launch`（并记录明确拒因供前端/新设备识别）。
2. **结算入账（Settlement）**：
   - 战斗结束后通过现有 `Match SQL -> player_inbox -> Scene` 唯一通道投递 `BattleSettlementData`（内含冻结 `festival_beast` 上下文）。
   - 场景首次应用时走**严格资产隔离分支**：不动人物血蓝/宝宝血蓝/装备/道具/货币/经验，单帧原子更新 `ClientRequestLedgerComp`（材料自选额度）+ `PlayerFestivalBeastComp`（正常胜局且 `wins < required_wins` 则 `+1`，达 `required_wins` 即固化永久资格与冻结奖励版本，并清空对应 `pending_launch`）+ `PlayerBattleResultComp`（写入活动胜场增量与实际材料新增量）。
   - SQL 存盘确认后才在 `CompleteInboxSettlement` 公开战果、解冻并推送 `BattleEndS2C`。
3. **奖励领取（Claim）**：
   - `SceneActivityClientPlayer.ClaimFestivalBeastReward(client_request_id, reward_identity)`
   - 优先按 `client_request_id` 查已有领取回执：命中同 `reward_identity` 直接重放原 `pet_id`（不受活动是否结束或历史期限制）；同 `client_request_id` 不同 `reward_identity` 拒绝冲突。
   - 再按 `reward_identity` 查终身领取记录：若已领取，不同 UUID 并发直接收敛为 `ALREADY_CLAIMED` 并返回已领取的原 `pet_id`，绝不铸第二只。
   - 校验存在永久资格、角色可写且不在战斗中、配置表目标神兽行与资源引用完整、专属容量 `CountSpeciesPets(petTableId) < exclusive_gift_slots`。
   - 先调用 `PetSystem::PrepareExclusiveActivityPet` 分配永久 `pet_id` 并按固定 `aptitude_min`（须等于 `aptitude_max`）构造 `PetInstance`（号源失败直接返回，不消费资格）。
   - 单帧同步提交：`PlayerPetComp` 插入该 `PetInstance` + `PlayerFestivalBeastComp` 标记已领并写回执 + `SavePlayerForAssetCommit`，并仅当 `PlayerLastPersistedSnapshotComp` 按当前 `owner_epoch` 确认包含该回执与 `pet_id` 时才置 `durable = true`。

---

## 三、思想分析

- **单一事实源原则**：活动准入只走 Match DirectIntent + `PrepareBattle` 复核；材料额度只走 `PlayerEquipmentMaterialRewardSystem::PrepareBattleCredit`；神兽容量与禁交易规则只写在 `PetSystem` 一处供所有入口共用；前端完全消费服务端裁定结果，不自算阶段/胜场/资格/材料额度。
- **最小完备改动原则**：直接复用已有的 `BattleTelegraphEntry`、慢速阶段钩子 `ExecuteBossTrialSlowAndGuardianActions`、`MatchInternal.StartActivityBattle`、`PlayerLastPersistedSnapshotComp` 持久确认机制、以及 `BattleResultEntry` 战果恢复通道，不造平行框架。
- **Fail-Closed 与幂等收口**：缺表/缺资源/档位重叠或空洞/非活动入口请求 `boss_trial_kind=7`/跨设备无本地 UUID 重查/异步回调前实体销毁，全部默认拒绝或可恢复，永不静默降级、不双发宠物。

---

## 四、漏洞分析与防线

| 潜在漏洞 | 攻击/异常场景 | 防护措施 |
|---|---|---|
| 普通入口绕过活动准入 | 客户端直接调 `JoinQueue` / 切磋传 `battle_config_id=111` | `go/match` `catalog.go` 拒绝 `boss_trial_kind=7` 进入普通队列；`Scene.PrepareBattle` 与 `TurnBattleEngine::Initialize` 双重校验 `boss_trial_kind=7` 必须带合法 `BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST`。 |
| 预检后换宠/收宠/死宠进局 | 发起挑战通过校验，在 `PrepareBattle` 到达前收回或切换宝宝 | `StartChallenge` 将 `locked_pet_id` 冻结进 `BattleActivityContext`，`Match` 透传到 `PrepareBattleRequest`，场景在建快照前强制复核 `active_pet_id == locked_pet_id && pet.health > 0`。 |
| 跨接口调用方伪造 | `guild` 伪造节日兽单人局，或 `scene` 伪造帮会历练局 | `MatchInternal.StartActivityBattle` 按 `activity_context.kind` 严格绑定调用方身份：`GUILD_TRIAL` 只认 `"guild"`，`FESTIVAL_BEAST` 只认 `"scene"`。 |
| 活动截止临界竞争 | 截止前 1ms 发起但未持久化，或截止后才落库 | Match SQL 事务内以库时钟 `now()` 复核 `effective_start_at_ms <= now < effective_end_at_ms`；已提交成局则在途可正常结算回原期次。 |
| 并发不同 UUID 刷多只神兽 / 号源失败吃资格 | 客户端超时换 UUID 重试，或 `GuidKind::kItem` 号段耗尽 | `PrepareExclusiveActivityPet` 先铸号再同步写回执；回执按 `reward_identity` 终身唯一，异 UUID 并发只返回原 `pet_id`；号源失败未改任何组件。 |
| 换设备无本地 UUID 无法恢复 | 玩家在设备 A 发起开局/领宠后断网，在设备 B 登录 | `PlayerFestivalBeastComp` 持久化 `pending_launch` 与 `claim_receipts`，新设备调用 `GetFestivalBeastDetail` 即可直接恢复原 `request_id`、`battle_id` 与 `pet_id`。 |
| 混单交易绕过绑定限制 | 交易单把绑定的春禧兽与普通物品/银两打包扣款 | `asset_op_system.cpp` 整单预检在扣任何物品/货币之前先调 `PetSystem::IsTransferableSpecies`，命中绑定宠立即整单拒绝零副作用。 |
| 异步回调悬空实体 | `StartActivityBattle` gRPC 返回前玩家下线/销毁 | 回调只按值捕获 `playerId` / `requestId` / `activityInstanceId`，回到 loop 线程经 `tlsEcs.GetPlayer(playerId)` 重查有效实体，单测显式验证 Destroy-Before-Callback。 |
| 低速被点名者防御失效 / 上轮防御跨轮 | 玩家速度低于首领，或第 2 轮防御企图覆盖第 3 轮重击 | 第 3/6/9/12 轮重击排在常规行动之后、回合末摘防御之前；回合末统一清 `is_defending`，确保 R2 防御不跨 R3，R3 低速防御必生效。 |
| 减攻 Debuff 对首领无效 | 怪物攻击放旧 `strength` 字段导致 `EffectivePhysicalAttack` 不生效 | 瑞兽迎春首领将档位攻击力写入 `physical_attack`（`strength=0`）、防御写入 `defense`（`armor=0`），使减攻/减防技能按公共公式生效。 |

---

## 五、步骤分析与执行步骤

1. **Step 1**：落盘本实现设计文档与独立 UI Codex 契约，刷新 `docs/README.md`。
2. **Step 2**：修改表结构与 Excel（`Pet`、`Monster`、`Dungeon`、`FestivalBeastActivity`、`FestivalBeastTier`、`Tip`）及 Proto（`player_festival_beast_comp`、`mysql_database_table`、`battle_data`、`player_battle`、`player_battle_result_comp`、`scene`、`match_internal`、`player_activity`），执行代码与数据表生成。
3. **Step 3**：实现 C++ 战斗引擎（R2/5/8/11 预告、R3/6/9/12 慢速重击 180%、终态预告回传、禁道具捕捉、0 常规掉落）与 Go Match（`Kind=5` 准入、`"scene"` 鉴权、时间窗门禁、透传 `activity_context`、禁队列绕过）。
4. **Step 4**：实现 C++ 场景服（`PlayerFestivalBeastComp` 持久化接入、`PetSystem` 专属赠送容量与禁交易规则、`asset_op_system` 与 Go Trade 前置拦截、`PlayerFestivalBeastSystem` 查询/开局/结算/领宠、RPC Handler 接入）。
5. **Step 5**：同步客户端生成代码并实现 `mmorpg-client/Assets/Scripts/Game/FestivalBeast/` 纯非视觉数据适配层与单元测试。
6. **Step 6**：运行 C++ 门禁脚本、串行 MSBuild 编译、C++ GoogleTest、Go `go test ./...`、客户端编译与单测，并核查 Java/Python 待对齐清单。

---

## 六、交付给 UI Codex 的独立接口契约与状态样例（可直接复制）

> **给 UI Codex 的硬性约定**：
> 1. UI **绝不自行计算**活动阶段、胜场进度、永久资格、是否可挑战/可领奖、材料可赚额度或神兽资质；全部以服务端响应字段为唯一依据。
> 2. 所有 64 位整数 ID（`battle_id`、`pet_id`、`locked_pet_id`、`target_actor_id`、`source_actor_id`）及 UTC 毫秒时间戳在跨层展示或日志传递时须保留完整 64 位精度（客户端适配层提供精确字符串属性）。
> 3. 界面展示时间统一将服务端 UTC 毫秒时间戳转为**北京时间（UTC+8，无夏令时）**显示；活动开放区间语义为左闭右开 `[effective_start_at_ms, effective_end_at_ms)`。
> 4. 客户端非视觉适配入口：`Assets/Scripts/Game/FestivalBeast/FestivalBeastClient.cs` 与 `FestivalBeastModels.cs`。

### 6.1 服务端 RPC 清单

#### 1) 活动大厅目录：`SceneActivityClientPlayer.GetActivityList`（已有接口扩展）
- 响应 `GetActivityListResponse.entries` 中新增活动入口项：
  - `kind = PLAYER_ACTIVITY_ENTRY_FESTIVAL_BEAST (2)`
  - 含 `activity_id`（`activity_instance_id`）、`title`（如 `"瑞兽迎春"`）、`state`、`start_at_ms`、`end_at_ms`、`has_claimable_reward`（`true` 时显示红点/立即领奖标记）。
  - **可见性保证**：即使活动已结束或当前无新排期，只要玩家拥有**未领取的永久神兽资格**，目录项仍保留在列表中且 `has_claimable_reward = true`。

#### 2) 活动详情与跨设备恢复：`SceneActivityClientPlayer.GetFestivalBeastDetail`
- **请求 `GetFestivalBeastDetailRequest`**：
  - `uint32 activity_instance_id = 1;`（传 `0` 表示查询当前权威实例或最新待领/历史记录）
- **响应 `GetFestivalBeastDetailResponse`**：
  - `TipInfoMessage error_message = 1;`
  - `uint64 server_now_ms = 2;` — 服务端当前 UTC 毫秒（用于倒计时锚点，UI 换算 UTC+8 显示）
  - `FestivalBeastPhase phase = 3;` — 服务端裁定的界面展示阶段：
    - `FESTIVAL_BEAST_PHASE_UNSCHEDULED (1)`：未排期/未开放
    - `FESTIVAL_BEAST_PHASE_UPCOMING (2)`：预告期（未到 `effective_start_at_ms`）
    - `FESTIVAL_BEAST_PHASE_OPEN (3)`：开放挑战期（`[start, end)`）
    - `FESTIVAL_BEAST_PHASE_CLAIM_SHOWCASE (4)`：结束后 7 天集中领奖展示期（`[end, end + 7d)`）
    - `FESTIVAL_BEAST_PHASE_HISTORY (5)`：历史态（超过 7 天展示期；若仍有永久未领资格，依然允许领奖）
  - `uint32 activity_instance_id = 4;` — 活动期次 ID（如 `202601`）
  - `uint32 config_revision = 5;` — 活动配置版本号（发起挑战时原样回传校验）
  - `string activity_name = 6;` — 活动名（配置表下发，默认 `"瑞兽迎春"`）
  - `string boss_name = 7;` — 首领/神兽名（配置表下发，默认 `"春禧兽"`）
  - `string banner_asset_key = 8;` — 横幅资源键（配置表下发）
  - `string icon_key = 9;` — 图标资源键（配置表下发）
  - `uint64 effective_start_at_ms = 10;` — 开放开始 UTC 毫秒（含）
  - `uint64 effective_end_at_ms = 11;` — 开放结束 UTC 毫秒（不含）
  - `uint64 claim_showcase_end_at_ms = 12;` — 7 天领奖展示结束 UTC 毫秒（不含）
  - `uint32 current_wins = 13;` — 当前期次累计胜场（`0..required_wins`）
  - `uint32 required_wins = 14;` — 领奖所需胜场（默认 `3`）
  - `string reward_identity = 15;` — 稳定奖励身份（如 `"festival_beast_chunxishou_v1"`，复刻复用）
  - `uint32 reward_definition_version = 16;` — 奖励定义版本（达成永久资格后为冻结版本）
  - `uint32 reward_pet_table_id = 17;` — 固定奖励神兽配置 ID（默认 `101`）
  - `uint32 reward_pet_model_id = 18;` — 奖励神兽模型资源 ID
  - `string reward_pet_name = 19;` — 奖励神兽展示名
  - `bool has_permanent_eligibility = 20;` — 是否已达成永久待领资格（跨结束/离线/换期保留）
  - `uint32 eligibility_source_instance_id = 21;` — 达成永久资格时的原始期次 ID
  - `uint64 eligibility_earned_at_ms = 22;` — 达成永久资格的 UTC 毫秒
  - `bool reward_claimed = 23;` — 是否已终身领取（权威状态）
  - `bool reward_claim_durable = 24;` — 已领结果是否已确认落盘（`false` 时 UI 显示“正在确认发放，请勿重复操作/可继续重试”）
  - `uint64 claimed_pet_id = 25;` — 领取后的权威宠物实例 `pet_id`（未领为 `0`）
  - `uint64 claimed_at_ms = 26;` — 领取 UTC 毫秒
  - `string claim_request_id = 27;` — 领取请求 UUID（供新设备恢复与原请求重试）
  - `bool can_start_challenge = 28;` — 当前是否允许点击开始挑战
  - `uint32 start_block_code = 29;` — 不可挑战原因枚举 `FestivalBeastActionBlockCode`
  - `string start_block_reason = 30;` — 不可挑战直接展示文案
  - `bool can_claim_reward = 31;` — 当前是否允许点击领取神兽
  - `uint32 claim_block_code = 32;` — 不可领奖原因枚举 `FestivalBeastActionBlockCode`
  - `string claim_block_reason = 33;` — 不可领奖直接展示文案
  - `FestivalBeastPendingLaunchInfo pending_launch = 34;` — 在途开局恢复信息（跨设备无本地 UUID 也可恢复）
  - `FestivalBeastMaterialSummary material_summary = 35;` — 共享装备材料账摘要
  - `uint32 matched_tier_id = 36;` — 当前角色等级对应的首领等级档 ID
  - `uint32 round_limit = 37;` — 回合上限（默认 `12`）
  - `uint32 heavy_damage_percent = 38;` — 重击伤害倍率百分比（默认 `180`）
  - `uint64 state_revision = 39;` — 角色活动状态单调递增版本号

#### 3) 发起挑战：`SceneActivityClientPlayer.StartFestivalBeastChallenge`
- **请求 `StartFestivalBeastChallengeRequest`**：
  - `string client_request_id = 1;` — 客户端生成的标准 UUID v4（超时/弱网必须复用原 UUID）
  - `uint32 activity_instance_id = 2;` — 来自详情响应的期次 ID
  - `uint32 config_revision = 3;` — 来自详情响应的配置版本号
- **响应 `StartFestivalBeastChallengeResponse`**：
  - `TipInfoMessage error_message = 1;`
  - `FestivalBeastLaunchState launch_state = 2;` — `LAUNCHING(1)` / `ADMITTED(2)` / `REJECTED(3)` / `UNKNOWN(4)`
  - `bool replayed = 3;` — 是否为同 `client_request_id` 重放命中
  - `string client_request_id = 4;` — 回显权威请求 UUID
  - `uint64 locked_pet_id = 5;` — 本次开局锁定的出战宝宝 `pet_id`
  - `uint64 battle_id = 6;` — Match 准入后分配的 `battle_id`（异步受理瞬间可能为 `0`，随后刷新详情即可拿到）
  - `uint32 matched_tier_id = 7;` — 开局锁定的等级档 ID
  - `uint64 admitted_at_ms = 8;` — Match 持久准入时刻 UTC 毫秒
  - `string reject_reason = 9;` — 明确拒绝原因文案

#### 4) 领取神兽奖励：`SceneActivityClientPlayer.ClaimFestivalBeastReward`
- **请求 `ClaimFestivalBeastRewardRequest`**：
  - `string client_request_id = 1;` — 标准 UUID v4（超时/弱网必须复用原 UUID）
  - `string reward_identity = 2;` — 来自详情响应的稳定奖励身份
- **响应 `ClaimFestivalBeastRewardResponse`**：
  - `TipInfoMessage error_message = 1;`
  - `FestivalBeastClaimOutcome outcome = 2;` — `APPLIED(1)` / `ALREADY_CLAIMED(2)` / `REJECTED(3)` / `UNKNOWN(4)`
  - `bool replayed = 3;` — 同 `client_request_id` 重放标志
  - `bool durable = 4;` — **只有 `durable == true` 才能向玩家展示“已成功领取”最终态**；`false` 需保持确认态并按原 UUID 轮询/重试
  - `string client_request_id = 5;` — 生效的请求 UUID
  - `string reward_identity = 6;`
  - `uint32 reward_definition_version = 7;` — 冻结的奖励定义版本
  - `uint32 reward_pet_table_id = 8;` — 发放的神兽配置 ID
  - `uint64 pet_id = 9;` — 发放后的权威宠物实例 ID（`ALREADY_CLAIMED` 同样返回原 `pet_id`）
  - `uint64 claimed_at_ms = 10;`

#### 5) 战斗内预告与兑现终态：`BattleStateS2C` & `TurnResultS2C`（已有协议扩展）
- `BattleTelegraphEntry`：
  - `uint64 source_actor_id = 1;` — 首领局内 `actor_id`
  - `uint64 target_actor_id = 2;` — 被点名的人物或宝宝局内 `actor_id`（确定性单抽，重连不变）
  - `uint32 execute_round = 3;` — 兑现回合（`3 / 6 / 9 / 12`）
  - `uint32 intent_kind = 4;` — `9 = kTelegraphKindFestivalHeavyStrike`（或 `1` 兼容慢速重击）
  - `uint32 status = 5;` — 权威状态：
    - `1`：即将发动（R2/5/8/11 预告窗口）
    - `2`：本回合发动（R3/6/9/12 兑现窗口，引导被点名者防御或队友控场/减攻/套盾）
    - `3`：已发动（R3/6/9/12 结算完成）
    - `4`：已受控取消（眩晕/冰冻/昏睡/混乱/禁动/禁物攻阻止，未补普攻）
    - `5`：目标失效取消（被点名者已阵亡或撤离，未换目标）
  - `string label = 6;` — 权威提示文案
  - `uint32 telegraph_round = 7;` — 发出预告的回合（`2 / 5 / 8 / 11`）
  - `uint64 state_version = 8;` — 预告状态版本号（防乱序覆盖）
- `TurnResultS2C.resolved_telegraphs`（新字段）与 `BattleStateS2C.last_resolved_telegraphs`（新字段）：保存本回合刚兑现/取消的终态预告，确保演出与断线重连都能精确区分“已发动 / 受控取消 / 目标失效取消”。

#### 6) 战果与材料增量：`BattleEndS2C` & `BattleResultEntry`（已有协议扩展）
- 新增 `FestivalBeastSettlementSummary festival_beast_settlement`：
  - `uint32 activity_instance_id = 1;` — 开局锁定的活动期次
  - `string reward_identity = 2;`
  - `uint32 wins_before = 3;` — 本局前该期胜场
  - `uint32 wins_after = 4;` — 本局后该期胜场（败/逃/作废与 `wins_before` 相等）
  - `uint32 required_wins = 5;` — 所需胜场（3）
  - `bool newly_eligible = 6;` — 是否由本局首次达成永久待领资格
  - `uint32 material_units_added = 7;` — 本局实际材料新增量（0..10）
  - `uint32 material_tradable_units_added = 8;` — 本局实际可交易材料新增量
  - `uint64 material_available_units_after = 9;` — 结算后跨日/跨周待领材料总余额
  - `uint32 material_earnable_remaining_after = 10;` — 结算后当前周期剩余可赚材料额度

### 6.2 典型状态 JSON 样例（供 UI Codex 直接联调与 Mock）

#### 样例 A：开放中，2/3 胜场，可直接挑战（第 1 阶段材料池）
```json
{
  "server_now_ms": 1795651200000,
  "phase": "FESTIVAL_BEAST_PHASE_OPEN",
  "activity_instance_id": 202601,
  "config_revision": 1,
  "activity_name": "瑞兽迎春",
  "boss_name": "春禧兽",
  "banner_asset_key": "activity_festival_beast_banner",
  "icon_key": "pet_chunxishou_icon",
  "effective_start_at_ms": 1795564800000,
  "effective_end_at_ms": 1796774400000,
  "claim_showcase_end_at_ms": 1797379200000,
  "current_wins": 2,
  "required_wins": 3,
  "reward_identity": "festival_beast_chunxishou_v1",
  "reward_definition_version": 1,
  "reward_pet_table_id": 101,
  "reward_pet_model_id": 1012,
  "reward_pet_name": "春禧兽",
  "has_permanent_eligibility": false,
  "reward_claimed": false,
  "reward_claim_durable": false,
  "claimed_pet_id": "0",
  "can_start_challenge": true,
  "start_block_code": "FESTIVAL_BEAST_BLOCK_NONE",
  "start_block_reason": "",
  "can_claim_reward": false,
  "claim_block_code": "FESTIVAL_BEAST_BLOCK_NOT_ELIGIBLE",
  "claim_block_reason": "本期累计获胜 3 次后可免费领取春禧兽（当前 2/3）",
  "pending_launch": null,
  "material_summary": {
    "quota_mode": "FESTIVAL_MATERIAL_QUOTA_STAGE1_LIFETIME",
    "expected_win_units": 10,
    "earnable_remaining_units": 640,
    "available_units": 20,
    "available_tradable_units": 2,
    "earned_today": 0,
    "daily_limit": 20,
    "day_starts_at_ms": 1795640400000,
    "next_day_reset_at_ms": 1795726800000,
    "weekly_phase_active": false,
    "lifetime_earned": 20,
    "lifetime_limit": 660,
    "earned_this_week": 0,
    "weekly_limit": 100
  },
  "matched_tier_id": 3,
  "round_limit": 12,
  "heavy_damage_percent": 180,
  "state_revision": 4
}
```

#### 样例 B：活动已结束转历史态，但玩家有永久未领资格（普通宠位已满仍可经专属赠送位领奖）
```json
{
  "server_now_ms": 1798000000000,
  "phase": "FESTIVAL_BEAST_PHASE_HISTORY",
  "activity_instance_id": 202601,
  "config_revision": 1,
  "current_wins": 3,
  "required_wins": 3,
  "reward_identity": "festival_beast_chunxishou_v1",
  "reward_definition_version": 1,
  "reward_pet_table_id": 101,
  "reward_pet_name": "春禧兽",
  "has_permanent_eligibility": true,
  "eligibility_source_instance_id": 202601,
  "eligibility_earned_at_ms": 1796001234567,
  "reward_claimed": false,
  "reward_claim_durable": false,
  "claimed_pet_id": "0",
  "can_start_challenge": false,
  "start_block_code": "FESTIVAL_BEAST_BLOCK_NOT_OPEN",
  "start_block_reason": "本期活动挑战已结束",
  "can_claim_reward": true,
  "claim_block_code": "FESTIVAL_BEAST_BLOCK_NONE",
  "claim_block_reason": "",
  "state_revision": 7
}
```

#### 样例 C：领奖确认中（`reward_claimed = true, reward_claim_durable = false`）与新设备恢复已领终态
```json
{
  "reward_claimed": true,
  "reward_claim_durable": false,
  "claimed_pet_id": "900100000042",
  "claim_request_id": "7b64a021-8f2a-4c31-93ab-1a4d5c6e7f80",
  "can_claim_reward": false,
  "claim_block_code": "FESTIVAL_BEAST_BLOCK_CLAIM_CONFIRMING",
  "claim_block_reason": "正在确认神兽发放结果，请稍候"
}
```
> 落盘确认后：`reward_claim_durable = true`，`claim_block_code = "FESTIVAL_BEAST_BLOCK_ALREADY_CLAIMED"`，`claimed_pet_id = "900100000042"`。

#### 样例 D：在途开局恢复（新设备登录无本地 UUID）
```json
{
  "can_start_challenge": false,
  "start_block_code": "FESTIVAL_BEAST_BLOCK_LAUNCH_IN_FLIGHT",
  "start_block_reason": "正在进入瑞兽迎春战斗，请稍候",
  "pending_launch": {
    "client_request_id": "3c8e72d4-9b11-4f9e-b1c2-998877665544",
    "activity_instance_id": 202601,
    "config_revision": 1,
    "locked_pet_id": "900100000007",
    "matched_tier_id": 3,
    "launch_state": "FESTIVAL_BEAST_LAUNCH_ADMITTED",
    "battle_id": "884422110099",
    "admitted_at_ms": 1795651250000
  }
}
```

#### 样例 E：战斗内 R2 预告与 R3 取消/兑现态
```json
{
  "round_index": 2,
  "telegraphs": [
    {
      "source_actor_id": 1000000,
      "target_actor_id": 900100000007,
      "telegraph_round": 2,
      "execute_round": 3,
      "intent_kind": 9,
      "status": 1,
      "state_version": 1,
      "label": "春禧重击蓄力：第3回合慢速物理重击锁定目标，防御可减半、控场可打断"
    }
  ],
  "last_resolved_telegraphs": []
}
```
> R3 被控场打断后的 `TurnResultS2C.resolved_telegraphs`：
```json
[
  {
    "source_actor_id": 1000000,
    "target_actor_id": 900100000007,
    "telegraph_round": 2,
    "execute_round": 3,
    "intent_kind": 9,
    "status": 4,
    "state_version": 3,
    "label": "春禧重击受控取消"
  }
]
```

#### 样例 F：胜局结算摘要（`BattleEndS2C.festival_beast_settlement` / `BattleResultEntry`）
```json
{
  "activity_instance_id": 202601,
  "reward_identity": "festival_beast_chunxishou_v1",
  "wins_before": 2,
  "wins_after": 3,
  "required_wins": 3,
  "newly_eligible": true,
  "material_units_added": 10,
  "material_tradable_units_added": 1,
  "material_available_units_after": 30,
  "material_earnable_remaining_after": 630
}
```

### 6.3 UI Codex 展示优先级建议（非视觉适配层已提供枚举映射）
1. `reward_claimed && !reward_claim_durable` → 领奖确认中（按原 `claim_request_id` 轮询/重试，禁止灰掉误导失败，也禁止提示已到账）
2. `has_permanent_eligibility && !reward_claimed` → **可免费领取春禧兽**（最高业务引导，跨期/历史态始终可点）
3. `pending_launch != null` 或在战斗中 → 恢复/等待当前对局，禁止二次点击开局
4. `phase == OPEN && can_start_challenge` → 可免费挑战（展示 `current_wins / required_wins`、`material_summary` 与 10 元易容卡快捷入口）
5. 其他 → 按 `start_block_reason` / `claim_block_reason` 展示精确不可操作原因。
