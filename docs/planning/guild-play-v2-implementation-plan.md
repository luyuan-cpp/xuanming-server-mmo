# 帮会玩法 V2（全天无匹配帮战、三灵脉争夺与江湖追杀令）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 `xuanming-server-mmo-cpp`（C++/Go 权威服）、`xuanming-server-mmo`（Java 独立服）与 `mmorpg-client`（Unity 非 UI 客户端绑定与纯 C# 状态层）三仓中，按两阶段零返工方式落地「全天无匹配帮战 + 三灵脉争夺 + 守旗阵踢馆 + 江湖追杀令 + 4 层丰厚帮会奖励 + 野外路上任意异帮直打」，并向异机 ChatGPT/Codex 交付冻结的协议与纯 C# 客户端接口供 UI 并行制作。

**Architecture:** 彻底摒弃匹配队列，复用既有 `MatchInternal` 直开管线（`PrepareBattle -> CreateBattle`）与 `battle:lock` 原子占用，由 `go/guild` 独占 `mmorpg_guild` 管理三座灵脉、守旗阵快照、连胜悬赏、仇敌与追杀令托管及 4 层奖励结算，经 Kafka `match-results` 幂等消费回流；`battle` 引擎新增 `ai_controlled` 守旗托管与第 12 回合「决胜煞气」；`scene` 接入 `EvaluateBattle` 660 首套池、资产通道白名单及阶段 2 野外安全/点人直打校验；Unity 客户端提供纯 C#（无 `UnityEngine` 依赖）的 `GuildWarClient` 与不可变视图模型供 UI 绑定。

**Tech Stack:** Protobuf3 / Go (`go-zero`, MySQL/TiDB `schemamigrate`, Redis, Kafka) / C++20 (`entt`, `muduo_grpc`, GoogleTest, MSBuild `/m:1`) / Java 21 (Spring Boot, Dubbo Triple, Maven, `ContractSync`) / C# (.NET / Unity WebGL2 & IL2CPP 兼容纯 C# 模型与 `IBattleTransport`)。

**Spec:** [guild-play-v2.md](guild-play-v2.md) · [guild-play-v2-ui-handoff.md](ui-handoff/guild-play-v2-ui-handoff.md)

---

## Global Constraints

- 严格遵守 `xuanming-server-mmo-cpp/AGENTS.md`、`xuanming-server-mmo/AGENTS.md`、`mmorpg-client/AGENTS.md`：中文协作与注释、英文结构化运行日志；绝不手改生成产物；绝不覆盖同工作区其他会话的未提交改动；未获授权不执行 `git commit` / `git push` / `git tag`。
- 逻辑库自治（`AGENTS.md §7`）：`go/guild` 只写 `mmorpg_guild`，`go/match` 只写 `mmorpg_match`，`scene` 独占玩家存档，跨服只走签名 RPC 与 Kafka 发件箱；**事务内绝不调 RPC**。
- `mmorpg_guild` 锁序严格单调追加（接 `tables.go` 位置 12 `guild_name_hold` 之后：位置 13 `guild_war_leyline` < 14 `guild_war_garrison` < 15 `guild_war_weekly_progress` < 16 `guild_war_bounty` < 17 `guild_war_battle`）；同步更新 `guildTestDropTables` 与 `economyFlowDropTables`。
- C++ 硬门禁：严禁类/结构体非静态裸指针成员（交付前跑 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`）；严禁新增 C++ 线程；MSVC 串行 `/m:1`；`entt::entity` lambda 使用 `[entity = entity]` 初始化捕获。
- 配表与 Tip 红线：`Tip.xlsx` 单轴发号（`guild_error` 从 `14048` 顺延），严禁手写数字或私有 fault map；周宝箱与周期数据按 `(player_id, week_key)` 独立存储，**不塞入永久全局位图**（`AGENTS.md §13`）；商店硬货（重铸砂 `3403`、悟道残页 `3406`、帮战限定形象）全部仅自用绑定且禁复用旧 ID。
- 客户端双形态（微信/抖音小游戏 WebGL2 + iOS/Android IL2CPP）：`GuildWarClient` 严禁使用 `Thread`/`Task.Run`/`Task.Delay`/`Socket`/`Reflection.Emit`，全部回调受 `(connectionIdentity, playerId, generation)` 代次围栏保护，倒计时以服务端 `server_time_ms` + 单调时钟锚定。
- 指标红线：Prometheus label 严禁包含 `player_id`，只允许 `{zone_id, leyline_id, mode, outcome}` 等有限枚举。

## Review Focus

1. **混编小队 / 和平免战成员误拖入战**：野外或界面发起帮战时，若被点方小队含未入帮、低于等级门槛、已开和平免战或与发起方同帮成员，必须整单拒绝并返回 `kGuildWarTeamInvalid`，绝不将无关或和平玩家拖入战斗。（验证绑定至 Task 5 & Task 8）
2. **远程界面偷拉野外跑图玩家**：帮会界面 `StartGuildWarBattle(TARGET_KIND_LIVE_PLAYER)` 只允许命中已主动开启「挂牌迎战」或位于灵脉战区内的异帮活人；普通野外路上跑图玩家只能由同图同线身边的敌帮现场点战，远程点名必须拒绝。（验证绑定至 Task 6）
3. **跨副本灵脉 1 小时分红重复发放**：多 `go/guild` 副本并发运行周期分红巡检时，必须在 `guild_war_leyline` 行 `SELECT ... FOR UPDATE` 事务内校验 `now - last_dividend_at_ms >= 3600_000` 并推进时间戳，保证每小时仅产 1 份分红且周上限 24 份不被并发击穿。（验证绑定至 Task 6）
4. **守旗阵 AI 快照污染个人存档或触发 660 额度**：`BattlePlayerSnapshot.ai_controlled = true` 的守旗单位绝不产生 `BattleSettlementData` 个人回写、绝不发送 `BattleEndS2C` 给守旗本人、绝不扣减或增加守旗本人 660 装备材料额度。（验证绑定至 Task 3 & Task 4）
5. **追杀令洗钱 / 换帮白嫖周宝箱 / 重复 Kafka 投递**：同一目标全区同时仅限 1 张活跃追杀令且仅允许固定档位 + 10% 销毁税 + 禁同帮/好友发单 + 每日领赏上限 5 次；周里程碑领奖按 `(player_id, week_key)` 持久化防跳槽重领；`match-results` 重复投递同一 `battle_id` 经 `guild_war_battle` 主键幂等闸门拦截零重复发奖。（验证绑定至 Task 6）

---

## 契约冻结总览（所有 Batch 共同遵循，同步交接给异机 ChatGPT UI）

### 1. Proto 增量清单

| 文件 | 新增类型 / 字段（精确字段号） |
|---|---|
| `proto/common/rollback/transaction_log.proto` | `TX_GUILD_BOUNTY_ESCROW = 41;`（`GUILD_DEBIT` 预扣追杀银两）<br>`TX_GUILD_WAR_REWARD = 42;`（`GUILD_CREDIT` 帮战银两/周宝箱/灵脉分红）<br>`TX_GUILD_BOUNTY_PAYOUT = 43;`（`GUILD_CREDIT` 追杀赏金/反杀赏金/过期退款） |
| `proto/match/match_service.proto` | `MatchMode`: `MATCH_MODE_GUILD_WAR_PVP = 9;`（真人帮战）、`MATCH_MODE_GUILD_WAR_SIEGE = 10;`（踢守旗阵/灵脉守卫） |
| `proto/battle/battle_data.proto` | `BattlePlayerSnapshot`: `bool ai_controlled = 25;`（守旗阵托管单位）<br>`eBattleActivityKind`: `BATTLE_ACTIVITY_KIND_GUILD_WAR = 2;`<br>`BattleActivityContext`: `uint64 defender_guild_id = 8;`、`uint32 leyline_id = 9;`、`uint32 war_battle_kind = 10;`（1=界面真人/2=守旗踢馆/3=野外路遇/4=灵脉现场/5=追杀结单）、`uint64 bounty_id = 11;`、`repeated uint64 attacker_player_ids = 12;`、`repeated uint64 defender_player_ids = 13;`、`uint32 week_key = 14;`（仅紧凑 ID，绝不塞快照） |
| `proto/battle/player_battle.proto` | `BattleStateS2C`: `uint32 guild_war_frenzy_stacks = 12;`（第 12 回合起每回合 +1，每层伤害 +15%、疗盾 -15%，客户端据此展示「决胜煞气」徽标） |
| `proto/match/match_internal.proto` | `StartGuildWarBattleRequest` / `StartGuildWarBattleResponse`（`MatchInternal.StartGuildWarBattle`）：带 `battle_config_id`、`match_mode`、`attacker_player_ids`、`defender_player_ids`、`repeated BattlePlayerSnapshot ai_defender_snapshots`、`BattleActivityContext activity_context`、`request_id`、`ServiceCallAuth auth`；拒绝时 `offender_player_id` + `offender_battle_id`（支持并发抢点落败直转 `WatchBattle`） |
| `proto/guild/guild_db.proto` | 枚举追加：`GUILD_ASSET_OP_KIND_WAR_REWARD = 5`、`GUILD_ASSET_OP_KIND_BOUNTY_ESCROW = 6`、`GUILD_ASSET_OP_KIND_BOUNTY_PAYOUT = 7`；`GUILD_DAILY_COUNTER_KIND_WAR_BOUNTY_CLAIM = 4`、`GUILD_DAILY_COUNTER_KIND_WAR_PUBLIC_BOUNTY = 5`。<br>追加 5 张表（锁序位置 13–17）：`GuildWarLeylineRecord`、`GuildWarGarrisonRecord`、`GuildWarWeeklyProgressRecord`、`GuildWarBountyRecord`、`GuildWarBattleRecord`（另加 `GuildWarPlayerStateRecord` 存个人和平免战/挂牌迎战/连胜/周领奖位图） |
| `proto/guild/guild.proto` | `GuildChangeKind`: `GUILD_CHANGE_KIND_WAR_UPDATED = 15;`<br>9 个客户端 RPC + 1 个推送占位（全部加入 `GuildService`）：<br>① `GetGuildWarOverview`<br>② `SetGuildWarStance`<br>③ `RegisterGuildWarGarrison`<br>④ `StartGuildWarBattle`<br>⑤ `ClaimGuildWarMilestone`<br>⑥ `ClaimGuildWarDividend`<br>⑦ `PostGuildWarBounty`<br>⑧ `TrackGuildWarBounty`<br>⑨ `GetGuildWarBattleResult`<br>⑩ `NotifyGuildWarAlert` (`CLIENT_RPC_PUSH`) |
| `proto/scene/player_guild_war.proto`（阶段 2） | `SceneGuildWarClientPlayer`: `AttackFieldGuildPlayer`、`TravelToGuildWarTarget`；`SceneActorView` 追加 `guild_id`、`guild_name`、`war_peace_mode`、`war_streak_count`、`active_bounty_id`、`watchable_battle_id` |

### 2. 配置表与 Tip 清单

- `data/schema/guildwarrule_table.proto` + `data/GuildWarRule.xlsx`（`id = 1` 单行全局规则）：`min_level=15`、`peace_cooldown_sec=60`、`post_battle_protect_sec=10`、`leyline_max_banner_hp=1000`、`leyline_capture_init_hp=600`、`leyline_shield_sec=900`、`pvp_win_banner_damage=100`、`pvp_defend_banner_heal=60`、`siege_win_banner_damage=35`、`garrison_max_slots=5`、`garrison_refresh_cooldown_sec=60`、`frenzy_start_round=12`、`frenzy_damage_step_bps=1500`、`frenzy_heal_reduce_step_bps=1500`、`frenzy_max_stacks=10`、`bounty_normal_silver=5000`、`bounty_heavy_silver=15000`、`bounty_tax_bps=1000`、`bounty_public_funds=2000`、`bounty_public_daily_limit=2`、`bounty_ttl_sec=86400`、`bounty_protect_sec=1800`、`bounty_counter_kill_tear=2`、`bounty_daily_payout_limit=5`、`track_cooldown_sec=15`、`diminish_window_sec=1800`、`dividend_interval_sec=3600`、`dividend_guild_weekly_cap=24`、`dividend_player_weekly_cap=12`、`dividend_min_personal_merit=30`。
- `data/schema/guildwarmilestone_table.proto` + `data/GuildWarMilestone.xlsx`（4 档：`id=1..4`，战功 `300/1200/3000/6000`，关联 `Reward` 或明确物品包：重铸砂 `3403`、悟道残页 `3406`、灵兽口粮 `20`、银两、帮贡）。
- `data/GuildShop.xlsx` 追加 5 行：`105` 仅自用重铸砂（`item_id=3403`，2 级，120 帮贡，周限 10）、`205` 悟道残页（`item_id=3406`，3 级，100 帮贡，周限 12）、`206` 聚宝银两袋（1 级，80 帮贡，周限 10）、`304` 帮战战旗名签/称号令（4 级，1,200 帮贡，永久限 1）、`305` 帮战限定形象兑换符（6 级，3,600 帮贡，永久限 1，联动 `CharacterAppearance` 新增 `card_unlockable=0` 形象）。
- `data/tip/Tip.xlsx` `//guild_error` 段顺延 `14048..14071`（共 24 个码，仅 `kGuildWarServiceBusy = 14071` 填 `fault=1`）。

---

## 分批实施拆解（4 个可独立验收的 Batch）

> 每批预计手改文件 ≤ 18 个（不含生成产物），每批结束有明确的编译、单测与门禁证据；全部改动均叠加在当前工作区之上，严禁回退或修改他人未提交代码。

---

### Batch 1 · 契约、配表、Tip 与跨端生成闭环（Task 1–2）

#### Task 1: 协议源与配置表源定义

**Files:**
- Modify: `proto/common/rollback/transaction_log.proto`
- Modify: `proto/match/match_service.proto`
- Modify: `proto/match/match_internal.proto`
- Modify: `proto/battle/battle_data.proto`
- Modify: `proto/battle/player_battle.proto`
- Modify: `proto/guild/guild.proto`
- Modify: `proto/guild/guild_db.proto`
- Create: `data/schema/guildwarrule_table.proto`
- Create: `data/schema/guildwarmilestone_table.proto`
- Create: `data/GuildWarRule.xlsx`
- Create: `data/GuildWarMilestone.xlsx`
- Modify: `data/GuildShop.xlsx`
- Modify: `data/tip/Tip.xlsx`

**Interfaces:**
- Produces: 上文「契约冻结总览」全部 proto 消息、枚举、`GuildService` 10 个新方法、`MatchInternal.StartGuildWarBattle`、2 张新配置表、5 行新商店货与 `14048..14071` 24 个 tip 码。

- [ ] **Step 1: 按「契约冻结总览」追加 proto 源定义**
  逐文件只在末尾追加字段与枚举，绝不改动或复用已有字段号。
- [ ] **Step 2: 编写 `guildwarrule_table.proto`、`guildwarmilestone_table.proto` 及配套 `.xlsx` 源表，扩充 `GuildShop.xlsx` 与 `Tip.xlsx`**
  使用 Python `openpyxl` 严格按仓库既有表头格式写入 `data/GuildWarRule.xlsx`、`data/GuildWarMilestone.xlsx`、追加 `data/GuildShop.xlsx` 与 `data/tip/Tip.xlsx`（不触碰其他 sheet/行）。
- [ ] **Step 3: 校验配表外键与字段号唯一性**
  确认所有新增 `item_id`（`3403` / `3406` / `20`）均存在于 `data/Item.xlsx`，新 tip 码名全局唯一。

#### Task 2: 官方导表、协议生成与同级 Unity 客户端同步体检

**Files:**
- Generated only (禁止手改): `generated/*`, `cpp/generated/*`, `go/proto/*`, `go/shared/generated/*`, `../mmorpg-client/Assets/Scripts/{Proto,Net,Table}/Generated/*`, `../mmorpg-client/Assets/StreamingAssets/Tables/*`
- Modify (仅生成表清单注册): `cpp/generated/table/CMakeLists.txt`（若导表器未自动写入）, `go/guild/internal/constants/constants.go` + `constants_test.go`, `go/guild/internal/data/tables.go`

**Interfaces:**
- Consumes: Task 1 全部源 proto 与 xlsx。
- Produces: C++ / Go / Unity 全链路最新绑定与可编译客户端程序集（此时即可把生成的 `Guildpb` C# 定义同步给异机 ChatGPT 做 UI 联编）。

- [ ] **Step 1: 运行服务端导表与 proto-gen**
  Run: `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`（及导表命令 `dev.bat gen` 或 `data_table_exporter`）
  Expected: 退出码 0，无 schema/FK/tip 冲突告警。
- [ ] **Step 2: 同步同级 Unity 客户端并执行编译体检**
  Run: `cd D:\luyuan\mmorpg-client && .\gen.bat client`
  Expected: `sync_tables.ps1`、`gen_proto.ps1`、`gen_messageids.ps1`、`client_compile_check.ps1` 全部 0 错误。
- [ ] **Step 3: 登记 `go/guild/internal/constants/constants.go` 新 tip 常量并跑静态护栏单测**
  Run: `cd D:\luyuan\xuanming-server-mmo-cpp\go\guild && go test ./internal/constants/...`
  Expected: `TestNoHandWrittenTipCodes` PASS。

---

### Batch 2 · C++ 战斗/场景底座 + Go Match 无匹配池直打核心（Task 3–5）

#### Task 3: C++ Battle 引擎支持守旗阵 AI 托管（`ai_controlled`）与第 12 回合「决胜煞气」

**Files:**
- Modify: `cpp/libs/services/battle/constants/turn_battle_constants.h`
- Modify: `cpp/libs/services/battle/system/turn_battle_engine.h`
- Modify: `cpp/libs/services/battle/system/turn_battle_engine.cpp`
- Modify: `cpp/nodes/battle/logic/battle_room_manager.cpp`
- Test: `cpp/tests/turn_battle_engine_test/guild_war_battle_test.cpp`（或追加到既有测试文件并登记 `.vcxproj`/`CMakeLists.txt`）

**Interfaces:**
- Consumes: `BattlePlayerSnapshot.ai_controlled`、`MatchMode::{GUILD_WAR_PVP, GUILD_WAR_SIEGE}`、`BattleStateS2C.guild_war_frenzy_stacks`。
- Produces: `TurnBattleEngine::IsGuildWarBattle()`、`GuildWarFrenzyStacks()`、`IsAiControlledPlayer(uint64_t playerId)`；`BattleRoomManager` 跳过 `ai_controlled` 玩家的直连/票据/个人结算下发。

- [ ] **Step 1: 写失败单测 `GuildWarBattleTest`**
  覆盖 4 个断言：① `AiControlledDefenderAutoActsAndProducesNoPersonalSettlement`（`ai_controlled=true` 的守方人物与宝宝自动按剧本/默认行动，不阻塞 `AllPlayersReady()`，且不被当作真人结算）；② `FrenzyStacksStartAtRound12AndScaleDamageAndHealShield`（第 11 回合 0 层、第 12 回合 1 层伤害 `+15%`/疗盾 `-15%`、第 22 回合封顶 10 层，重连/回放状态一致）；③ `GuildWarRoundLimitDrawsSymmetrically`（打满 120 回合双方存活判 `BATTLE_OUTCOME_DRAW` 而非 B 胜）；④ `GuildWarZeroBagConsumableDeduction`（不消耗背包药）。
- [ ] **Step 2: 实现引擎与房间层最小改动**
  - `InitPlayers`：当 `snap.ai_controlled()` 为真时强制 `actor.set_is_auto(true)` 且标记进 `aiControlledPlayerIds_`（不参与人工手动等待、`SubmitAction` 拒绝、不发个人 `BattleSettlementData`）；
  - `ResolveCurrentRound` / 伤害与疗盾公式：若为帮战模式且 `roundIndex >= 12`，按 `min(roundIndex - 11, 10)` 层施加伤害 `*(10000 + 1500*stacks)/10000`、治疗与护盾 `*(10000 - min(7500, 1500*stacks))/10000`（保留最低 25% 护底），写入 `BattleStateS2C.guild_war_frenzy_stacks`；
  - `BuildStateSnapshot` 与回合上限判定把 `MATCH_MODE_GUILD_WAR_PVP/SIEGE` 纳入平局分支；
  - `BattleRoomManager`：发票/推帧/生成个人结算列表时过滤 `ai_controlled` 快照。
- [ ] **Step 3: 跑 C++ 无裸指针门禁与串行构建单测**
  Run: `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`
  Run: `msbuild game.sln /m:1 /t:battle;turn_battle_engine_test /p:Configuration=Debug /p:Platform=x64` 并运行 `turn_battle_engine_test.exe --gtest_filter=GuildWarBattleTest.*`
  Expected: 0 裸指针违例，构建 0 错误，4/4 PASS。

#### Task 4: C++ Scene 接入 660 装备材料平权、帮战资产通道白名单与只读守旗快照

**Files:**
- Modify: `cpp/libs/services/scene/player/system/player_equipment_material_reward.cpp`
- Modify: `cpp/libs/services/scene/player/system/asset_op_system.cpp`
- Modify: `cpp/libs/services/scene/battle/system/player_battle.cpp`
- Test: `cpp/tests/bag_test/player_battle_settlement_test.cpp`

**Interfaces:**
- Consumes: `MatchMode::{GUILD_WAR_PVP, GUILD_WAR_SIEGE}`、`TransactionType::{TX_GUILD_BOUNTY_ESCROW, TX_GUILD_WAR_REWARD, TX_GUILD_BOUNTY_PAYOUT}`。
- Produces: `EvaluateBattle` 支持帮战真人（胜 15 / 达门槛负平 10）与守旗踢馆（胜 10 / 负 0）；`GUILD_DEBIT` 放行 `TX_GUILD_BOUNTY_ESCROW`、`GUILD_CREDIT` 放行 `TX_GUILD_WAR_REWARD` 与 `TX_GUILD_BOUNTY_PAYOUT`；`ApplySettlementToEntity` 对帮战模式实行零战损（不扣血蓝、不回写伤亡、不扣背包药，仅结算 660 材料与近况）；只读快照构建函数供守旗阵抓拍复用。

- [ ] **Step 1: 写失败单测**
  断言：① `GuildWarPvpWinGives15AndEligibleLossGives10`；② `GuildWarPvpAfkLossGives0`（1 回合速败 `pvpLossIneligible=true`）；③ `GuildWarSiegeWinGives10AndLossGives0`；④ `GuildWarSettlementDoesNotDrainHpMpOrItems`；⑤ `AssetOpAllowsGuildBountyEscrowAndWarRewards`。
- [ ] **Step 2: 实现 Scene 最小改动**
  - `player_equipment_material_reward.cpp`：`EvaluateBattle` 中 `MATCH_MODE_GUILD_WAR_PVP` 并入 PVP 15/10 分支，`MATCH_MODE_GUILD_WAR_SIEGE` 并入 PVE 胜 10（无困难加奖）分支；
  - `asset_op_system.cpp`：`kAssetOpStreamRules` 放行 `TX_GUILD_BOUNTY_ESCROW`（`GUILD_DEBIT`）及 `TX_GUILD_WAR_REWARD` / `TX_GUILD_BOUNTY_PAYOUT`（`GUILD_CREDIT`，并允许 `GUILD_CREDIT` 同时发货币+绑定物品，复用现有 `AssetBundle`）；
  - `player_battle.cpp`：识别 `MATCH_MODE_GUILD_WAR_PVP/SIEGE` 走无损战后满血满蓝恢复。
- [ ] **Step 3: 跑 C++ 无裸指针门禁与串行构建单测**
  Run: `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`
  Run: `msbuild game.sln /m:1 /t:scene;bag_test /p:Configuration=Debug /p:Platform=x64` 并跑 `bag_test.exe --gtest_filter=*GuildWar*:*AssetOp*`
  Expected: 0 错误，全部新增用例 PASS。

#### Task 5: Go Match 无匹配池直打管线（`MatchInternal.StartGuildWarBattle`）与公开观战/回放策略

**Files:**
- Create: `go/match/internal/logic/start_guild_war_battle.go`
- Create: `go/match/internal/logic/start_guild_war_battle_test.go`
- Modify: `go/match/internal/server/matchinternalserver.go`
- Modify: `go/match/internal/logic/gather.go`
- Modify: `go/match/internal/logic/spectate_visibility.go` + `spectate_visibility_test.go`
- Modify: `go/match/internal/logic/battle_replay_policy.go` + `battle_replay_policy_test.go`

**Interfaces:**
- Consumes: `MatchInternal.StartGuildWarBattleRequest`、`BattleActivityContext{kind=GUILD_WAR}`。
- Produces: `StartGuildWarBattle` 原子抢占全部真人成员 `battle:lock` / 占用（并发落败方返回占用者的 `offender_battle_id` 供一键观战）、支持 `LIVE_PVP` 显式双侧分队（attacker=`team_index 0`, defender=`team_index 1`）与 `GARRISON_SIEGE`（把 `ai_defender_snapshots` 强制置 `team_index=1, ai_controlled=true` 直接拼入 `CreateBattleRequest.Players` 跳过 `PrepareBattle`）；`spectateVisibilityFor` 对 `BATTLE_ACTIVITY_KIND_GUILD_WAR` 返回 `SPECTATE_VISIBILITY_PUBLIC`；`recordReplayFor` 对 `MATCH_MODE_GUILD_WAR_PVP` 返回 `true`。

- [ ] **Step 1: 写失败单测**
  覆盖：① `TestSpectateVisibilityDecidesEveryMatchMode`（帮战返回 `PUBLIC`）；② `TestReplayPolicyGuildWar`（真人帮战录回放、守旗踢馆不录）；③ `TestStartGuildWarBattleLivePvpSplitsTwoSidesAndSkipsQueue`；④ `TestStartGuildWarBattleSiegeAttachesAiSnapshots`；⑤ `TestStartGuildWarBattleConcurrentTargetReturnsOffenderBattleId`。
- [ ] **Step 2: 实现 `start_guild_war_battle.go` 及 `gather.go` 扩展**
  复用 `StartActivityBattle` 验签、发号、SQL 成局与 `runFormationGather` 补偿链，绝不新造排队池；严禁把快照塞进 `activity_context`。
- [ ] **Step 3: 跑 `go/match` 测试**
  Run: `cd D:\luyuan\xuanming-server-mmo-cpp\go\match && go test ./internal/logic/... ./internal/server/...`
  Expected: 全绿 PASS。

---

### Batch 3 · Go Guild 帮战/灵脉/守旗/追杀令核心业务 + Unity 客户端纯 C# 适配（Task 6–7）

#### Task 6: Go Guild 全天灵脉帮战、守旗阵、4 层奖励与江湖追杀令完整落地

**Files:**
- Create: `go/guild/internal/war/rules.go`（纯函数：旗势、连胜悬赏、30 分钟同对手递减、追杀税与反杀撕单、每帮限占 1 座、周宝箱资格）
- Create: `go/guild/internal/war/rules_test.go`
- Create: `go/guild/internal/data/war_repo.go`（MySQL 事务严格遵 `tables.go` 13–17 锁序：`leyline` / `garrison` / `weekly_progress` / `bounty` / `battle` 幂等闸门 + 周期分红幂等推进 + 退帮/解散清理）
- Create: `go/guild/internal/logic/war_logic.go`（9 个客户端 RPC 实现 + `SettleGuildWarResult` Kafka 结算 + 踢馆/追兵预警推送 + 15 秒追踪定位节流）
- Create: `go/guild/internal/logic/war_logic_test.go`
- Modify: `go/guild/internal/data/tables.go`（追加 5+1 张表）与 `guild_repo_test.go` / `economy_flow_integration_test.go` 删表清单
- Modify: `go/guild/internal/kafka/trial_result_consumer.go`（按 `kind` 分发 `GUILD_TRIAL` vs `GUILD_WAR`）
- Modify: `go/guild/internal/server/guild_server.go`（薄委托 9 个 RPC + `NotifyGuildWarAlert` 占位）
- Modify: `go/guild/internal/logic/guild_manage_logic.go` / `guild_manage_repo.go`（退帮/踢人清理守旗槽、解散清理灵脉与活跃守旗）

**Interfaces:**
- Consumes: Task 1–2 表与 proto、Task 5 `MatchInternal.StartGuildWarBattle`、既有 `assetop.Loop` / `GuildNotifier` / `OnlineStatusResolver` / `PlayerLocatorRedisClient`。
- Produces: 全部 9 个客户端 RPC + 1 个推送 + Kafka `match-results` 帮战幂等结算。

- [ ] **Step 1: 写纯规则失败单测 `war/rules_test.go`**
  断言：① 同对手 30 分钟递减 `100% -> 50% -> 20% -> 0%` 且第 2 场起不叠连胜；② 悬赏需 `>= 2` 不同对手；③ 追杀令 5,000/15,000 银两扣 10% 销毁税、反杀 2 名不同追兵撕单并没收 50% 托管赏金；④ 每帮限占 1 座灵脉（已占帮打空第 2 座只发破旗赏金、灵脉归无主）。
- [ ] **Step 2: 写集成/逻辑失败单测 `logic/war_logic_test.go`**
  覆盖 Review Focus 全部 5 项：远程界面禁拉野外普通跑图玩家、混编/和平小队拒战、多副本并发周期分红不重发、跳槽不重领本周已领里程碑档位、Kafka 重复投递同一 `battle_id` 零重复发奖、同一目标同时仅 1 张活跃追杀令 + 30 分钟免再挂保护 + 每日 5 次赏金上限、并发抢点返回 `offender_battle_id`。
- [ ] **Step 3: 实现 `war/rules.go`、`data/war_repo.go`、`logic/war_logic.go` 并接线 `guild_server.go` 与 `trial_result_consumer.go`**
  严格守：事务内零 RPC（守旗快照先取后入库、调用 `MatchInternal.StartGuildWarBattle` 在事务外、通过 `guild_asset_op` outbox 异步可靠发银两/物品与预扣追杀银两）。
- [ ] **Step 4: 跑 `go/guild` 全量单测**
  Run: `cd D:\luyuan\xuanming-server-mmo-cpp\go\guild && go test ./...`
  Expected: 全部单测（含锁序、表数守卫、`TestNoHandWrittenTipCodes` 与新增帮战单测）PASS。

#### Task 7: Unity 客户端纯 C# 非 UI 网络与状态层（`GuildWarClient`）

**Files:**
- Create: `../mmorpg-client/Assets/Scripts/Game/Guild/GuildWarModels.cs`（+ `.meta`）
- Create: `../mmorpg-client/Assets/Scripts/Game/Guild/GuildWarClient.cs`（+ `.meta`）
- Create: `../mmorpg-client/Assets/Tests/EditMode/Guild/GuildWarClientTests.cs`（+ `.meta`）
- Modify: `../mmorpg-client/Assets/Scripts/Game/Guild/GuildClient.cs`（处理 `GUILD_CHANGE_KIND_WAR_UPDATED` 通知与动态帮旗状态联动）

**Interfaces:**
- Consumes: Task 2 生成的 `Guildpb` / `GuildServiceRpc`、`IBattleTransport`、`RpcGuard`。
- Produces: 供异机 ChatGPT UI 直接绑定的唯一纯 C# 真源 `GuildWarClient`（零 `UnityEngine` 依赖，详尽签名见 [guild-play-v2-ui-handoff.md](ui-handoff/guild-play-v2-ui-handoff.md) §五）。

- [ ] **Step 1: 写 EditMode 单测 `GuildWarClientTests.cs`**
  断言：① 切号/断线/换连接后旧回调被 `(connectionIdentity, playerId, generation)` 丢弃；② 并发抢点返回 `kGuildWarTargetInBattle` 时正确暴露 `LastConflictBattleId` 供 UI 弹一键观战；③ 结算查询 `PENDING` 走有界退避轮询并在终态发出 `SettlementOpened`；④ 切和平冷却与追踪 15 秒冷却均以服务端锚点 + 单调时钟计算；⑤ `NotifyGuildWarAlert` 正确分发踢馆/追兵预警事件。
- [ ] **Step 2: 实现 `GuildWarModels.cs` 与 `GuildWarClient.cs`**
  遵守 WebGL2 单线程与 IL2CPP 约束（无 `Task.Run`/`Thread`/`Reflection.Emit`）。
- [ ] **Step 3: 跑 Unity 客户端编译与测试体检**
  Run: `cd D:\luyuan\mmorpg-client && pwsh -File tools/client_compile_check.ps1`
  Expected: 主程序集与 EditMode 测试程序集 0 错误。

---

### Batch 4 · Java 独立服全量对齐 + 阶段 2 野外现场任意异帮点人直打（Task 8–9）

#### Task 8: 阶段 2 · 野外三图路上任意异帮直点开战、跨图追杀追踪与灵脉战旗 `WorldPoi`（C++/Go + Unity 适配）

**Files:**
- Create: `proto/scene/player_guild_war.proto`
- Modify: `proto/scene/player_scene.proto`（AOI 头顶帮名/连胜/追杀标/免战/激战中 `watchable_battle_id`）
- Create: `cpp/libs/services/scene/player/system/player_guild_war.h` + `.cpp`
- Create: `cpp/nodes/scene/handler/rpc/player/player_guild_war_handler.h` + `.cpp`
- Modify: `../mmorpg-client/Assets/Scripts/Game/Guild/GuildWarClient.cs`（接入 `AttackFieldGuildPlayer` 与 `TravelToGuildWarTarget`）
- Test: `cpp/tests/bag_test/player_guild_war_test.cpp`

**Interfaces:**
- Consumes: Batch 1–3 全部能力 + [world-map-play-v2.md](world-map-play-v2.md) M1/M2（1 线聚人与 `WorldPoi`）。
- Produces: 天墉城安全区硬拦截、三张野外图同图同线点任意异帮直打（含 10s 战后保护、60s 和平切回冷却、普通路 `<= 目标人数 + 1` 与灵脉战区不限人数）、跨图追杀一键传送入口并向目标推 `GUILD_WAR_ALERT_PURSUER_ENTERED_MAP`。

- [ ] **Step 1: 写 Scene 野外点战单测**
  断言：天墉城拒战、未入帮/低于 15 级/和平免战/10s 保护拒战、普通路 5v1 拒战而灵脉战区内放行、合法任意两帮路上 0 秒直调 `StartGuildWarBattle`、追杀进图向目标推预警。
- [ ] **Step 2: 落实 proto、C++ Scene 域系统、生成同步与客户端编译检查**
  跑 `test_cpp_no_raw_pointer_member.py`、`/m:1` 构建与 `client_compile_check.ps1`。

#### Task 9: Java 独立服（`xuanming-server-mmo`）双端契约同步与全量对齐

**Files:**
- Sync (工具生成，禁手改): `java tools/ContractSync.java --mmorpg D:\luyuan\xuanming-server-mmo-cpp`
- Modify: `game-battle-engine`（`TurnBattleEngine.java` / `BattleUnit.java`：`aiControlled` + 第 12 回合决胜煞气 + 平局）
- Modify: `game-battle`（`BattleRoomServiceImpl.java`）
- Modify: `game-scene`（`BattleSettlementService.java` / `PlayerBattleService.java` / 资产流白名单 / 阶段 2 野外点战）
- Modify: `game-match`（`MatchModes.java` / `GatherPipeline.java` / `SpectateVisibilityPolicy.java` / `StartGuildWarBattle`）
- Modify/Create: `game-guild`（三灵脉、守旗阵、追杀令、4 层奖励、周里程碑与分红、Kafka 消费）
- Modify: `PARITY.md`（登记完整对账行与实跑测试证据）

- [ ] **Step 1: 执行 `ContractSync` 并 `./mvnw -B clean install -DskipTests`**
- [ ] **Step 2: 按 Java 21 惯用法逐模块实现并补充 JUnit 5 对齐测试（覆盖本计划全部 Review Focus）**
- [ ] **Step 3: 执行受影响模块与 `game-robot` 门禁测试并登记 `PARITY.md`**
  Run: `./mvnw -B -pl game-battle-engine,game-battle,game-scene,game-match,game-guild,game-robot -am test`
  Expected: 全模块 0 错误、全部测试 PASS。

---

## 帮战追杀令与灵脉结算的数据一致性定案（2026-11-26，数据覆写审计 r2 第 4 轮）

> 状态：代码已落（proto 未生成、未编译、未运行），验证交 Gemini，命令见 §6。审计条目 r2-gw#1～#4，来源 [第 4 轮交接单](../handoff/data-overwrite-audit-r2-round4-handoff-20261126.md)。
> 涉及：`go/guild/internal/{war/rules.go, data/war_repo.go, data/asset_store.go, logic/war_logic.go, logic/trial_background.go}`、`proto/guild/{guild.proto, guild_db.proto}`。

### 0. 两条事务的完整步骤（读代码前先看这里）

**结算事务 `WarRepo.SettleWarBattle`**（锁序严格单调：护栏副本 → G → M → Q → O → C → 灵脉 → 周进度 → 个人态 → 追杀令 → 对局 → 宿敌 → 待入账；护栏副本与待入账两步是 r2-gw#5 / #6 加的，见本节 §7、§8）：

| 步 | 做什么 | 关键点 |
|---|---|---|
| 0 | 幂等预查；**普通读**追杀令行与领赏人当日领赏次数，用 `war.ResolveBounty` 定出本局计划（销单 / 反杀计数 / 撕单 / 不动） | 领赏人当日已满 → 计划改成「不动」，输出 `BountyHeldRecipientID` |
| F | 按 player_id 升序锁全部入账目标（参战者、领赏人、撕单退款的发布者）的**角色护栏副本**，逐人判在役 / 冻结中 / 已删除 | 事务第一把锁；领赏人已删除 → 计划改成「不动」（`BountyHeldRecipientRemoved`）；要自愈时整笔重来一次 |
| 1 | 按 guild_id 升序锁帮会、加资金（含公费档撕单退给发单帮的一半） | 发单帮已解散 → 这一半不退，输出 `BountyIssuerGuildGone` |
| 2 | 按 (guild, player) 升序锁成员行、加帮贡；撕单退银两时一并锁发布者成员行 | 只作 seq 建行守卫，不要求领赏 / 退款人此刻仍在帮内 |
| 3 | 战后奖励指令与追杀令转账指令（领赏、撕单退发布者）并成**一个按 player_id 升序的序列**插入 | 指令号事务前预发，用不上就丢弃；某人护栏冻结中或本流未决已满 → 他那一笔不插、记待入账（第 10 步），已删除 → 放弃 |
| 3b | 占领赏人当日领赏次数（`upsertCounterWithLimit`，此时已持他的 GUILD_CREDIT seq 行） | 第 0 步之后被并发占满 → `ErrWarSettleStale` |
| 4 | 按 leyline_id 升序**逐行点锁本区全部灵脉**，与算 `BannerDelta` 用的快照逐项比对后写新旗势 | 不符 → `ErrWarSettleStale` |
| 6 | 个人态；追杀成功时给目标 30 分钟免再挂保护 | 目标必在守方名单里，行已在本步锁住 |
| 7 | **点锁追杀令**，按锁内的行再调一次 `ResolveBounty`，与第 0 步计划逐字段相同才写 | 不同 → `ErrWarSettleStale`，前面按旧计划做的转账一并回滚 |
| 10 | 第 3 步没能当场插指令的那几笔插进 `guild_war_credit_owed`（锁序位置 24，全序末尾） | 对局幂等闸门（第 8 步）已保证首次落定；转换时原样用预发的 op_id |

`SettleGuildWarResult` 遇 `ErrWarSettleStale` 从头重读重算（含发号、读灵脉快照、算奖励），进程内最多 3 次，仍冲突就返回错误交 Kafka 退避重投。

**到期收尾事务 `WarRepo.ExpireBounty`**（每张一个事务）：发布者护栏副本（银两档）→ G（公费档退资金）→ M（发布者成员行，seq 建行守卫）→ Q → O（银两档退款指令，不持租约、立刻可被重投循环领走；发布者冻结中或本流未决已满时改记待入账，已删除时退款放弃）→ 点锁追杀令，复核仍是到期的 ACTIVE 且本金不变 → 置 EXPIRED → 待入账行；复核不过就整笔回滚。候选由 `ListDueBounties` 按 `(expires_at_ms, bounty_id)` 游标翻页普通读（新索引 `status,expires_at_ms`），挂在待入队物品循环上约每分钟一轮、每轮至多 100 张，只收过期超过 15 分钟的单。

### 1. r2-gw#1 开战与结算不核对被击败方（high，成立）

**问题（含触发时序）**
1. 甲帮发一张全区江湖榜追杀令（银两档 4,500 入托管，状态 2）。任意别帮成员 H 在帮战总览里读到它的 `bounty_id`。
2. H 调 `StartGuildWarBattle`，`target_kind` 填 LEYLINE_GARRISON（打 AI 守旗阵）或 LIVE_PLAYER / FIELD_PLAYER（打任意别的异帮玩家），请求里带上这个 `bounty_id`。旧代码只有 BOUNTY 分支按追杀令查目标，其余分支把客户端的值原样冻结进 `GuildWarBattleContext.bounty_id`；直打活人只在 `bounty_id == 0` 时才自动挂钩。
3. H 赢下这一局。结算第 7 步只判「status=2 且未过期」，不比对本局防守方是不是追杀令目标、不比区服与接单资格，直接销单并把托管发给 H。反向同理：两名不同攻方带着这张单故意输两局，追杀令被判「反杀撕单」。
4. 同一缺口在灵脉上：LIVE_PLAYER 不改写客户端的 `leyline_id`，FIELD_PLAYER 在非灵脉图上也不清零；结算只要 `leyline_id > 0` 就对那座灵脉算旗势，`ApplyBannerImpact` 不核灵脉主人是不是本局防守帮 —— 打赢任意异帮玩家即可削第三方帮会的旗、拿破旗赏金 5,000 银两 + 1,500 帮会资金。连「两个外帮在别人灵脉所在的地图上打架」也会改第三方的旗。

**方案**
- 开战入口只信服务端推出来的目标信息，判据收在纯函数 `clientWarTargetHints` 一处：`leyline_id` 只在打守旗阵（含一键开战回落到守旗阵）时采信，照常校验、以灵脉主人为防守帮；野外直打按目标所在场景推；直打活人与一键开战选到活人时一律为 0。`bounty_id` 只在追杀榜直打（BOUNTY）时采信；直打活人一律丢弃客户端值，只由服务端按目标自动挂钩；守旗阵与一键开战一律为 0。
- 「这张单此刻能不能由这个帮接」收在 `war.BountyHuntable` 一处：同区、ACTIVE 未过期、全区江湖榜或本帮发的单。追杀榜直打、自动挂钩、追踪行踪与结算第 7 步共用。追杀榜直打的防守帮按目标**此刻**所在的帮记，不按单上的旧帮。
- 结算第 7 步在追杀令行锁内再核（`war.ResolveBounty`）：本局是追杀局、区服一致、攻方帮有接单资格、status=2 且按对局结束时刻未过期、**本局守方真人首人就是 `TargetPlayerId`**（追杀局的守方就是目标本人）；任一不满足就不动追杀令，这一局的其余结算照常。
- 灵脉旗势只在「本局防守帮就是灵脉此刻的主人」时才变（无主灵脉 = 打灵脉守卫，防守帮为 0）。

**为什么这样选**：开战上下文由 guild 自己签名发给 match、原样随结算消息回来，它可信的前提是写进去的值本身是服务端推出来的。所以修在两头：入口只放行服务端推出的号，结算在锁内按本局事实再判一次。判定写成纯函数，入口、追踪与结算三处同源，单测不连库就能穷举。

**被否决的做法**
- **只在开战入口校验**：追杀令在开战与结算之间会变（被别人结单、到期、撕单、扣款作废），入口那一刻成立不代表结算时成立；结算必须在追杀令行锁内再核。
- **结算时信任上下文里的 bounty_id、只补状态校验**：上下文里的号正是要防的输入，不比对被击败方就等于没校验。
- **旗势按「攻方是否赢」无条件结算**：两个外帮在第三方灵脉上的私斗会改写第三方的旗，与策划「攻方真人击败占领帮在线守军才扣旗」不符。

**代价与边界**：客户端发来的 `leyline_id` / `bounty_id` 在非对应分支被静默丢弃（不报错），客户端不必改。合服后旧区的追杀令按区服不符不可接单，到期原路退回。野外直打挂钩时若目标正好在灵脉图上，本局既结追杀令也按灵脉规则动旗（前提仍是目标帮就是灵脉主人）。

### 2. r2-gw#2 追杀令托管只进不出（high，成立）

**问题（含触发时序）**
- A（确定性）：全仓对 `guild_war_bounty` 的写只有建单 INSERT、结算第 7 步 UPDATE、扣款终结（1→2）三处，没有到期处理；24 小时没人结单，托管永远留在表里，策划要求的「到期原路退回」没有实现。
- B（确定性）：撕单只给目标发一半，另一半没有退给发布者。
- C（要故障）：结算事务先把追杀令置 COMPLETED / TORN、写下对局幂等行并提交，赏金指令在事务外另起一笔；这时发号失败、建指令出错、进程在两步之间崩溃都被吞掉，Kafka 重投走「已幂等结算」直接返回，赏金再也不发。
- D（确定性）：事务外那一笔在领赏人已离帮时静默不发；当日满 5 次时 `AllocateSeq` 已推进 seq 却不插指令就提交。追杀令已销单，托管既不给猎手也不退发布者。

**方案**
- 赏金与退款指令并进 `SettleWarBattle` 同一事务（第 3 步与战后奖励指令合成一个按 player_id 升序的序列），删掉事务外的 `EnqueueBountyPayout`。有挂钩追杀令时事务前预发两枚指令号（领赏 / 撕单退发布者），用不上的丢弃；发号失败整笔返回错误交 Kafka 重投。领赏与退款都不要求领赏人此刻仍在帮内（与战后奖励一致，钱是他本人的）。
- 撕单：同事务置 TORN，托管的一半（向下取整）给目标，其余退发布者 —— 银两档退扣税后托管的另一半银两给发布者本人；公费档的赏金池是系统配给的、不是谁出的，退的是**发单帮实付帮会资金的一半**，直接在 SQL 里加 `funds`，帮会已解散就跳过并打日志。
- 公费档实付资金写进单上的新列 `escrow_guild_funds`（`GuildWarBountyRecord` 字段 18），到期与撕单都按它退，不按退款那一刻的配表。
- 每日领赏上限在改追杀令之前判：领赏人当日已满就**整笔不转移** —— 追杀令保持 ACTIVE、反杀数不加、托管不动，本局其余结算照常，打一条英文日志（含 bounty_id；player_id 只进日志，不进指标 label）。
- 到期收尾：status=2 且过期超过 15 分钟的追杀令置 EXPIRED 并原路退回（银两档给发布者插 GUILD_CREDIT，交易类型复用 `TX_GUILD_BOUNTY_PAYOUT` —— 它本来就登记了「过期本金退还」，scene 资产通道白名单已放行；公费档退帮会资金）。每张一个事务，行锁 + 状态复核，多副本并发也只退一次。status=1（扣款尚未终结）的不动，扣款终结成 ACTIVE 之后的下一轮再处理。
- 扣款失败（预扣被拒 / 中止 / 放弃）改置新终态 `GUILD_WAR_BOUNTY_STATUS_ESCROW_FAILED = 6`；`EXPIRED = 5` 只表示「到期且托管已原路退回」。
- 到期收尾挂在待入队物品循环上（与合服榜折叠同理，借它的节拍与排空接入），按轮数折成约每分钟一轮，每轮至多 100 张，按游标翻页。

**为什么这样选**
- 同事务插指令是战后奖励、里程碑、分红已经在用的标准做法：状态变更与转账一起提交、一起回滚，没有中间态，Kafka 重投由对局幂等闸门兜住。
- 锁序上追杀令排在 seq / 指令 / 计数行之后，转账指令却要先插。做法是「普通读定计划 → 按锁序加锁做转账 → 到追杀令的位置点锁、按锁内的行重算一遍、与计划逐字段比对」，与 `ClaimDividend`「先按估算预留指令、锁内实算不同即回滚」同一模式，不需要任何锁序例外。
- 满上限时整笔不转移，是唯一既不让发布者白拿回本金、也不销毁玩家钱的处理；追杀令继续挂着，留给其他猎手或次日再领，最终没人结单就到期原路退回。
- 公费档按实付资金的一半退：撕单时目标拿系统赏金池的一半，发单帮退回实付的一半，「资金换赏金」的比例与销单时相同，撕单不会让发单帮倒赚。
- 到期收尾的 15 分钟宽限：结算按对局结束时刻判到期，结束在到期之前、结算消息晚到（消费者退避重投到转死信约 7.5 分钟）的那一局要先落定，否则猎手赢了却被退款抢先；多等一刻钟只推迟退款，不改金额。
- 游标翻页而不是每轮只取最老一页：某张单的退款一时进不了队（发布者未决指令已满）会留在原处，不能挡住排在后面的单。

**被否决的做法**
- **赏金在事务外入账、另加补偿扫描**：多一条要维护的链路，还要能区分「已发 / 没发 / 发了一半」；同事务插指令是战后奖励已经在用的标准做法。
- **满上限照样结单并把托管退给发布者**：发布者白拿回本金，猎手白打，等于给了发布者一条「找满次的人来结单」的免费撤单通道。
- **满上限照样结单并没收托管**：销毁玩家的钱。
- **读路径惰性过期退款**（像约战战书那样在总览里顺手收口）：没人打开总览就永远不退；读路径也不该带资产副作用。
- **另起一个到期收尾循环**：要新增循环活性点位与告警；待入队物品循环本来就每个 stable 副本无条件跑、带排空接入，挂上去零新增基础设施。
- **公费档撕单把赏金池另一半银两折成帮会资金退回**：赏金池是系统配给的，退它等于凭空造资金（发单花 2,000 资金、撕单退回 3,000），可被双开刷。
- **退款时按当时的配表重算公费档金额**：配表热更会让退款多退或少退；实付额必须记在单上。
- **扣款失败与到期退回共用 EXPIRED**：同一个状态既表示「退过了」又表示「根本没扣成」，审计时会去找一笔不存在的退款。

**代价与边界**：新增一列一个索引（schemamigrate 不给已存在的表补索引，本地库要重建 `guild_war_bounty`；项目未上线，可清表）。`guild.proto` 枚举追加一个值，客户端要重新生成（客户端 `BountyStatusLabel` 可补「扣款未成已作废」，列表本来只显示状态 1/2）。到期退款最晚在到期后约 16 分钟发出。发布者 GUILD_CREDIT 未决指令已满（16 条）或护栏冻结中时，追杀令照样到期、退款在同一事务记进待入账行，由待入队物品循环补插（§7、§8；原先是整笔失败、等游标转回来再试，还会打排空 Hold）。领赏 / 退款指令被 scene 拒收（REJECTED / ABANDONED）时与其它纯入账指令一样只留审计、不回滚追杀令（既有口径）。

### 3. r2-gw#3 灵脉旗势事务外读、事务内按绝对值盲写（medium，成立）

**问题（含触发时序）**：结果 topic `match-results` 的 key 是 `battle_id`（`go/match/internal/svc/servicecontext.go` 的 `publishResult`），同一座灵脉上的几局落在不同分区；guild 默认 2 副本（`deploy/k8s/manifests/go-svc/guild.yaml`）同在消费组 `guild-trial`，各自消费分到的分区（单进程内经 `serial` 串行，跨副本并发）。两场同一灵脉的结算几乎同时到达两个副本：两边都在事务外 `ListLeylines` 读到同一份旗势、各自算出结果（旗势够低时两边都算出破旗），两个事务的幂等键各不相同，都执行 `UPDATE … SET owner_guild_id=?, banner_hp=?, shield_until_ms=?` 写算好的绝对值。结果：先提交的扣减被覆盖；两场的首位胜者各拿一次破旗银两 5,000、攻方帮各拿一次 1,500 资金；「每帮限占 1 座」的判定也来自事务外读，两场不同灵脉的破旗并发时一帮能同时占两座。解散帮会释放灵脉（owner 置 0）与结算并发时，结算还可能把已解散的帮写回主人。

**方案**：事务外读照旧（奖励依赖旗势结果，规则计算留在 logic）。`BannerDelta` 记下算它时的快照（`OwnerBefore` / `HPBefore` / `ShieldUntilBefore` / `AttackerOwnsOther`）；结算事务第 4 步按 leyline_id 升序**逐行主键点锁本区全部灵脉**（锁序位置不变），用 `war.FindLeylineBasis` 在锁内重新取出目标灵脉与「攻方是否已占别的灵脉」，经 `BannerDelta.BasisHolds` 逐项比对；任一项不同就回 `ErrWarSettleStale`，整笔回滚。`SettleGuildWarResult` 遇它整笔重读重算，进程内最多 3 次，仍冲突就返回错误交 Kafka 重投。不改 Kafka key。

**为什么这样选**：标准的乐观并发 —— 读快照、带着快照去提交、提交点在锁内比对。锁本区全部三行而不只是目标灵脉，是为了把「攻方是否已占别的灵脉」也纳入同一把锁：两场分别拿下两座灵脉的结算会在锁上串行，后到者看到攻方已占一座 → 冲突重算 → 只破旗不兼并。点锁按固定升序，所有结算者同序取锁，只排队不成环；主键等值点锁符合本库「锁定语句只做完整主键点操作」的纪律（TiDB 走 Point_Get）。

**被否决的做法**
- **灵脉按区串行消费 / 把 Kafka key 改成 zone**：违反 AGENTS §7「topic key = 业务实体 ID」；`match-results` 还被 match 评分等别的消费组读，改 key 牵连全部消费者；还会把全区结算压到一个分区上。
- **在锁内重算全部奖励**：旗势结果决定破旗赏金、帮会资金与结算页展示，锁内重算等于把规则计算搬进数据层，两处各有一份规则。
- **把 UPDATE 改成带旧值条件的 CAS**（`WHERE banner_hp = ?`）：只能护住目标灵脉一行，护不住「每帮限占 1 座」那条跨行不变量；带复核谓词的写在 TiDB 上还要先补主键点锁。
- **用 `ErrWriteConflict` 表示快照过时**：它同时表示锁等待超时，那种情况进程内立刻重算只会再撞一次，交给 Kafka 退避更合适；专用的 `ErrWarSettleStale` 只在「重读就能成功」时触发进程内重试。

**代价与边界**：同一区的灵脉结算在三行锁上串行，冲突时多一次重读重算（同时收官的局数有限，3 次之内基本都能落定）。护盾期内（旗势不变）的局也要锁三行、比快照。

### 4. r2-gw#4 发号失败用墙钟自造持久 ID（low，成立）

**问题（含触发时序）**：发号器暂时取不到号时，`PostGuildWarBounty` 保留 `UnixNano` 当 `bounty_id`，`IssueGuildWarFeud` 用毫秒时间戳当 `feud_id`，`StartGuildWarBattle` 客户端不带 `client_request_id` 时开战请求号也是 `UnixNano`。它们与发号器打散后的 63 位号同处一个值域，违反 AGENTS §7 第 1 条。后果有界（INSERT 撞键只会让这一次失败），所以评 low。

**方案**：三处发号失败一律回 `ErrIDGenUnavailable`（复用现有 tip，不新增）。客户端没带 `client_request_id` 时用帮战发号器（与 op_id 同一个）现发一个。

**为什么这样选 / 被否决的做法**：同文件里 op_id 早已是「发不出号就整体失败」；墙钟回退被否决，理由就是不变量本身（值域重叠、多副本同一纳秒相撞、排障时分不清来源）。

**代价与边界**：发号器不可用期间发追杀令、下约战书、不带请求号的开战都会失败并提示稍后再试。

### 5. 顺带收口

- `GuildWarBountyRecord` 的全部读路径改用同一份列清单 `guildWarBountyColumns` 与 `scanBountyRow`（与约战的 `scanFeudRow` 同形），加列只改两处；不连库的 `TestGuildWarBountyColumnsCoverEveryProtoField` 守列序。
- 结算事务被 1213 整段重跑时，第 7 步改过的两列（`bounty_outcome_tag` / `jackpot_silver`）与输出都从原值重来，不再重复累加。
- 结算时读灵脉快照失败改为返回错误交重投，不再静默跳过这一局的旗势变化。

### 6. 给 Gemini 的验证

1. 生成（仓库根目录）：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。`guild.proto` 是客户端可见协议，按 AGENTS §4 在 `D:\luyuan\mmorpg-client` 跑 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`。
2. 编译与静态检查：`cd go/guild; go build ./...; go vet ./...`；仓库根目录 `python tools/scripts/check_log_text_english.py`。
3. 纯单测：`cd go/guild; go test ./internal/war/... ./internal/logic/... -count=1`（重点 `TestBountyHuntable*`、`TestResolveBounty*`、`TestBountyExpiryRefund*`、`TestBannerDeltaBasisHolds*`、`TestClientWarTargetHints*`、`TestWarBountyExpiryEveryRounds*`）。
4. 真库：设 `GUILD_TEST_MYSQL_DSN`（库名 `guild_test`），`cd go/guild; go test ./internal/data -run "GuildWarBounty|SettleWarBattle_|ExpireBounty_" -count=1 -v`，全部 PASS、不得 SKIP；再跑 `go test ./internal/data -count=1` 全包。
5. 已有本地库：schemamigrate 不给已存在的 `guild_war_bounty` 补新索引，重建这张表（未上线，可清）。
