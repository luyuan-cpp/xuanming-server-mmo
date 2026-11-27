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

### 7. r2-gw#5 一名玩家的未决指令满上限挡住整局结算（medium，成立）

> §7～§10 是批 V 之后的续修（2026-11-26，r2-gw#5～#8 由批 V 的修复代理顺带发现，逐条复核后都成立）。涉及：`go/guild/internal/data/{war_credit_owed.go（新）, war_repo.go, role_fence_pending.go, tables.go}`、`go/guild/internal/logic/{war_logic.go, trial_background.go}`、`proto/guild/guild_db.proto`（新表 `GuildWarCreditOwedRecord`、`guild_war_bounty` 加索引 `escrow_op_id`）。未生成、未编译、未运行，验证命令见 §11。

**问题（含触发时序）**
1. 帮会给玩家入账一律走 GUILD_CREDIT 流，每人每流的未决指令至多 16 条、跨度 512（`assetop.DefaultLimits`，是 scene 窗口证明的前提，不能放宽）。物品指令永不中止，背包一直满、人久不上线，这条流就会积满。
2. 积满的人参加了一局帮战（或者他是追杀令的领赏人、撕单要退款的发布者）。结算第 3 步给他 `AllocateSeq` 回 `ErrTooManyPending`，旧代码原样上抛，整笔事务回滚：同局所有人的帮贡、银两、帮会资金、战功、灵脉旗势、追杀令结单一样都没写。
3. 消费者（消费组 `guild-trial`）把它当暂时性失败退避重投，他的积压不会自己消失；20 次（约 4～7.5 分钟）之后整局写进死信 `guild-trial-dlq`。帮战没有历练那样的巡检器从 match 的 SQL 终局兜底，只能人工重放。一个人的积压挡住了同局所有人。
4. 到期收尾同理：发布者流满时 `ExpireBounty` 整笔失败，追杀令停在 ACTIVE、每轮重扫都失败一次，失败还把待入队物品循环的排空证明打成 Hold。

**方案**
- 沿用历练「待入队物品」那一套（`guild_trial_reward_owed` 加待入队物品循环，「绝不跳过」，06 §6.32）：结算第 3 步某人 `AllocateSeq` 回 `ErrTooManyPending` 时不插指令、seq 不前进，把这一笔原样记进新表 `guild_war_credit_owed`，**同一事务**在全序末尾插入（第 10 步，锁序位置 24）；到期收尾同样（追杀令照样置 EXPIRED）。
- 待入账行存的是结算事务前预发的指令号 `op_id`，转换时原样用作 `guild_asset_op.op_id`，不另发号 —— 同一笔至多变成一条指令（幂等键）。行里还存种类（WAR_REWARD / BOUNTY_PAYOUT）、交易类型、ref_id、period_key、奖励包与来源（battle_id 或 bounty_id）。主键 `(player_id, op_id)`：同一局同一人可以有两笔（猎手的战后奖励与追杀赏金）。
- 转换挂在待入队物品循环上，与历练待入队物品同一节拍（`logic.convertWarCreditOwed`，每轮都跑；每页 50 行、每轮至多 500 行；游标翻页，读到表尾回表头；某人本轮已判仍满或仍冻结，他名下其余行本轮不再试）。事务 `WarRepo.ConvertWarCreditOwed`：

| 步 | 做什么 | 关键点 |
|---|---|---|
| 1 | 锁他的角色护栏副本，按 §8 的判据判 | 已删除：点删本行放弃；冻结中：回滚留着（不算失败，不 Hold 排空）；要自愈时整笔重来一次 |
| 2 | 建 seq 行（已有则不写）→ `AllocateSeq` | 仍满：回滚留着 |
| 3 | 持着护栏副本与 seq 行，普通读本行还在不在 | 不在 = 别的副本刚转走：回滚；指令号是预发的，直接插会撞主键，所以先判再插 |
| 4 | 插指令（PENDING、不持租约、`next_attempt_ms = now`，重投循环立刻可领）→ 完整主键点删本行 | 同一事务 |

- 领赏人这一笔记进待入账也照占当日领赏次数（钱只是晚到，不是没领）；满上限与冻结两种情况 seq 行锁都照持，满足领赏计数行写者的守卫前提（C6）。
- 新表进角色护栏的挂起全集（`RoleFencePending` 的 blocker `guild_war_credit_owed`）：有待入账行就挡过户与删角冻结，与历练待入队物品同一口径。

**为什么这样选**
- 「一个人收不了，只延后他自己那一笔」是唯一同时满足三条的做法：不挡别人（整局照常提交）、不丢（同事务落进待入账行，后台补插）、不重复（预发的 op_id 就是幂等键；对局幂等闸门保证这一局只落一次；转换先判本行在不在再插，两个副本并发转同一行时先在护栏副本行锁上串行，后到者读到本行已不在就回滚；即使绕过这一层，op_id 主键也让后到者失败）。
- 现成机制：历练结算在同一种情况下早就这么做（I21），循环、游标、「本轮已判仍满就跳过」照抄；挂在同一个循环上，不新起 goroutine，不新增循环活性点位。

**被否决的做法**
- **满上限照样整笔失败、交给 Kafka 重投**：现状，一个人的积压挡住全局所有人，最后进死信。
- **复用 `guild_trial_reward_owed`**：主键 `(player_id, battle_id)` 装不下同一局同一人的两笔；转换写死了 ACTIVITY_REWARD / TX_GUILD_ACTIVITY_REWARD；表名是「历练」，装帮战行会误导。改主键与转换要动历练的锁序论证与一串静态测试，比新开一张同形的表风险大。
- **把这一笔记在对局行 `guild_war_battle` 的一列里**：挂起全集要按玩家查「他还有没有没到账的」，对局行按 battle_id 存，查不了；同局多人转换还会在对局行上互相排队。
- **满上限时多分一个 seq 先插再说**：16 / 512 是 scene 窗口证明的前提，越过就可能让未决 seq 滑出窗口、结局查不回来。
- **满上限时放弃这一笔**：丢玩家的钱。
- **转换时另发新号（历练的做法）**：要处理发号失败；行里已经有一个从没用过的号，直接用它还白得一个幂等键。

**代价与边界**
- 新增一张表（锁序位置 24）与一个索引（`created_ms`）。
- 收不了的那一笔晚到：窗口腾出来之后最多等循环一轮（缺省 10 秒）。积压没有专门的 gauge 与告警，只有每轮的汇总日志（`owed war credit round`）与每笔的 Info 日志；要量化时照 `guild_trial_owed_rows` 补一个。
- 结算提交后的同步投递（`syncDeliverWarOp`）只对当场插了的指令；补插的走重投循环。
- 待入账行转换之前，客户端看不到这一笔（不进 `my_pending_reward_count`）。

### 8. r2-gw#6 帮战结算与到期退款不过角色护栏（medium，成立）

**问题（含触发时序）**
1. 角色过户（人民币寄售）与删角之前，协调者先让全部参与者冻结这名角色：guild 的 `PrepareRoleFreeze` 在一个事务里冻结本库副本、读本库的挂起全集（未终态的资产指令、历练待入队物品等），有挂起就不准冻结。冻结之后，guild 每一笔给这名角色写资产的事务都必须先锁他的护栏副本、见冻结就拒（[水平扩展](../design/horizontal-scale-logical-db.md) §3.1、§3.4：「冻结之后的业务事务在 Lock 上被拒」）。
2. 历练结算经 `withRoleTargets` 锁全部候选人的副本，`SettleWarBattle` 与 `ExpireBounty` 不锁。时序：帮战一局在 match 提交终局（占用随之释放）→ 玩家马上挂出寄售（或注销执行）→ guild 的挂起全集里没有这一局（帮战没有历练那样的 launch 原件行挡着）→ 冻结成功 → 结算消息这时才被消费（正常几十毫秒，消费者积压或重投时可以是几分钟）→ 结算给已冻结的角色插了 GUILD_CREDIT 指令。协调者以为他在 guild 已无挂起，过户照常完成，这条指令在过户之后才落到角色身上。
3. 删角更糟：墓碑之后删角收尾删了他的 seq 行；迟到的结算 `ensureSeqRowTx` 用新纪元把 seq 行建回来，再插一条永远投不出去的指令 —— 违反「墓碑之后本库不许再有他的业务写入」（删角收尾不用两阶段的前提，[账号注销](../design/account-deletion.md) §6.1）。到期收尾给已冻结、已删除的发布者退款同理。

**方案**
- 结算事务第 F 步（第一把锁，先于 guild 行）按 player_id 升序锁本局全部入账目标的护栏副本：参战者、追杀令领赏人、撕单退款的发布者；到期收尾锁发布者（只锁银两档，公费档退的是帮会资金）。判据收在纯函数 `classifyCreditFence`，结算、到期收尾、待入账转换三处共用：
  - **在役**（未冻结、未删除）：照常入账（流满时按 §7 记待入账）。
  - **冻结中**（过户投票、寄售中、删角投票）：先让 `Guard.RunTx` 自愈一次（冻结的副本可能只是解冻推送丢了），还冻结才是真冻结 —— 这名玩家的**资产入账**（战后银两、领赏、退款）不插指令、不分 seq，记进待入账行（与 §7 同一张表）；帮贡与个人帮战态照写（它们不是挂起项，不经 scene 落到角色身上；过户本来就要求先离帮）。
  - **已删除**（墓碑）：他的行一律不写 —— 资产入账放弃，帮贡、个人态、seq 行都不碰。领赏人已删除则追杀令整笔不转移（`BountyHeldRecipientRemoved`：托管不销毁，没人结单就到期原路退回发布者，与批 V「领赏人当日满上限」同一处理）；撕单时发布者已删除，目标那一半照发、发布者那一半放弃；到期时发布者已删除，追杀令照样到期、银两退款放弃。
- 判的是「目标」口径（只有角色号，没有原身份）：只看冻结与删除，不看归属，与历练结算的候选人、待入队物品转换同一判据（`withRoleTargets` 的 Expect 只带 PlayerID）。所以**已换主**的角色（过户完成、副本解冻）照常入给这名角色；冻结期间记下的待入账行在解冻之后照常补插。
- 结算输出 `FencedPlayers` / `RemovedPlayers`，logic 打英文日志（player_id 只进日志，不进指标 label）。

**为什么这样选**
- 口径与历练一致：判据就是 `withRoleTargets` 那一套（冻结拒、删除拒、换主不拒），只是**拒的粒度**从整笔改成这名玩家。历练能整笔拒，是因为参与者在 launch 原件行（`guild_trial_launch_role`）里挂着，挂起全集挡住冻结直到结算 —— 结算时候选人实际上不可能冻结，整笔拒只是第二道防线；帮战没有这样的原件行，冻结能插进「终局提交」与「结算消费」之间，整笔拒就是 §7 同样的「一个人挡住全局」，寄售中的角色能冻好几天，必进死信。它还能被利用：输的一方马上挂寄售，就能让对面这一局的奖励、破旗、追杀结单全部卡住。
- 冻结中记待入账而不是直接插指令：冻结期间不出现会经 scene 落到角色身上的资产指令（护栏要防的正是这个），过户期间没有在途的指令；冻结撤销（投票被否、寄售撤回）后照常补发，不丢。
- 已删除直接放弃：rank「收件人已注销的奖励直接放弃」、mail「收件人已删除拒收」是既定口径；墓碑不会再变回在役，留着也永远发不出去。领赏人已删除时不结单而不是结单后放弃：托管是发布者的钱，结单后发不出去就等于销毁。
- 锁序：护栏副本是全部服务共同的第一把锁、按 player_id 升序（与 `rolefence.LockAll` 同序），结算同时锁多名玩家只会排队、不成环；要自愈的一次收齐。

**被否决的做法**
- **照历练整笔 `withRoleTargets`**：一名参战者冻结或删除就让整局回滚重投直到死信，见上。
- **冻结中照常插指令**：破坏冻结的前提（挂起全集读完之后又新增），过户之后指令才落到新主身上。
- **冻结中直接放弃**：投票被否、寄售撤回是常态（试探通过、正式投票被别的参与者否掉），玩家白打一局。
- **待入账行存原身份（账号、修订号），换主就放弃**：帮战结果里没有参战者的原身份（不像历练原件），只能取结算那一刻副本上的；也与历练的目标口径不一致。
- **事务外按副本预读分流、事务内再整笔 `withRoleTargets`**：两次判定之间副本可能变，变了又是整笔回滚；在事务里锁着判一次更简单。

**代价与边界**
- 每局结算多锁几行护栏副本（每名参战者一行）；副本缺行（这名玩家从没在 guild 走过带护栏的写）时第一次结算要经 data_service 自愈一次，之后不再。data_service 不可达时自愈失败，这一局回可重试错误交 Kafka 重投（与 guild 其它带护栏的写同一口径）。
- 冻结中的待入账行每轮转换都自愈一次（每名冻结玩家每轮一次 `GetRoleFenceState`），寄售可能持续几天；只多 RPC，不写库。
- 已删除者的帮贡、个人态不写，但帮会周战功仍按参战人数算，不扣他那一份。
- 过户完成之后补入账给这名角色：卖家打的最后一局，奖励随角色走（目标口径）。

### 9. r2-gw#7 扣款终结按指令号全表扫、追杀令终态旧行不清理（low，成立）

**问题（含触发时序）**
- 银两档追杀令的预扣指令终结时（scene 回执到达或重投循环判定），`asset_store.go` 的 BOUNTY_ESCROW 分支执行 `UPDATE guild_war_bounty … WHERE escrow_op_id = ? AND status = 1`。表上四个索引都不以 `escrow_op_id` 开头，每次终结都全表扫（MySQL 下还会在扫过的行上短暂加锁）；表只增不删，越扫越慢。
- 追杀令终态行（已完成 3、撕单 4、到期退回 5、扣款作废 6）永远留在表里，没有任何清理。

**方案**
- `GuildWarBountyRecord` 的 `OptionIndex` 末尾追加 `escrow_op_id`（第 5 个索引 `idx_guild_war_bounty_4`，与批 V 加 `status,expires_at_ms` 同一写法）。终结语句不改，走新索引定位。
- 终态旧行按保留期删：`WarRepo.PurgeTerminalBounties` 普通读一批「终态且 `expires_at_ms` 早于 now − 保留期」的候选（走已有索引 `status,expires_at_ms`），逐行一个短事务：主键点锁 → 锁内复核仍是终态、仍过期 → 完整主键点删（与资产指令终态清理 `execCleanupDelete` 同一纪律，TiDB 下不与同一行的悲观写者各持一半）。挂在批 V 到期收尾的同一个节拍上（约每分钟一轮），每轮至多 200 行，没删完的下一轮接着删；只是清理，失败只记日志、不 Hold 排空。
- 保留期是常量 `warBountyTerminalRetention = 30 天`。

**为什么这样选**
- 删之前逐个核过引用，终态行谁都不再需要：
  - 扣款终结按 `escrow_op_id` 找行，但只改 `status = 1` 的行（重复投递的终结对终态行本来就是空操作，删了也一样）；状态 1、2 永远不删。
  - 结算按 `bounty_id` 读追杀令：读到终态行与读不到行，`ResolveBounty` 都是「不动追杀令」，结论相同；即使是保留期之外的死信重放，删掉也不改变结算结果。
  - 战报只读对局行自己的列（`bounty_outcome_tag`、`jackpot_silver`），不回查追杀令表；追杀榜、点选、开战挂钩、追踪只看状态 2（追杀榜含 1）；发单前判「目标已有未结单」只数 1、2。
  - `bounty_id` 是发号器永久 ID，不会被新单复用，删了不会撞键；终态不会再变（扣款终结只认 1，结算与到期收尾锁内只认 2），清理不必与谁协调。
- 30 天：远长于结果消息可能被重投的窗口（`match-results` 保留 7 天，死信修好后重放），并与资产指令终态行的缺省保留期（`AssetOp.TerminalRetentionDays = 30`）对齐 —— 预扣、领赏、退款指令还查得到的时候追杀令行也在，客服与对账两头都有据。
- 按到期时刻判龄：终态时刻不晚于到期后约 16 分钟（到期收尾 15 分钟宽限加一轮），复用已有的 `status,expires_at_ms` 索引，不为清理再加按 `resolved_at_ms` 的索引。

**被否决的做法**
- **终结改成「按指令号普通读出 bounty_id → 主键点锁 → 点改」**：要动资产指令终结的通用分支与它的锁序论证；加索引之后同一条语句已按索引定位，收益不值。
- **保留期跟 `AssetOp.TerminalRetentionDays` 配置走**：要把配置接进帮战依赖；调大指令保留期时追杀令行先删只少对账材料、不影响正确性，先用常量。
- **到期收尾时顺手删**：终态行要留一段给客服与对账查。

**代价与边界**
- schemamigrate 不给已存在的表补建索引：本地已有的 `guild_war_bounty` 要重建（未上线，可清）。
- 调大 `AssetOp.TerminalRetentionDays` 之后，超过 30 天的追杀令行会比它的指令行先删（只少对账材料）。

### 10. r2-gw#8 野外点选面板露出别帮「仅本帮可见」的追杀令（low，成立）

**问题（含触发时序）**：发单帮发一张仅本帮可见（`public_board = 0`）的追杀令。任意别帮成员在野外点选目标（`InspectFieldWarTargets`），旧代码对每名目标调 `ActiveBountyByTarget`，只要有 ACTIVE 的单就把单号、赏金、档位原样回给客户端，不看接单资格。追杀榜 `ListActiveBounties` 按 `public_board = 1 OR issuer_guild_id = 本帮` 过滤，点选面板没过滤，私单被别帮看到（信息面：没资格的帮拿到单号也开不了这张单，批 V 的 `BountyHuntable` 已在开战、追踪、结算挡住）。

**方案**：点选面板改用纯函数 `fieldTargetBounty`，判据就是批 V 的 `war.BountyHuntable`（同区、ACTIVE 未过期、全区江湖榜或本帮发的单）；不满足时单号、赏金、档位一律为空。

**为什么这样选**：「这张单此刻能不能由这个帮接」只收在 `BountyHuntable` 一处，追杀榜（SQL 条件同义）、开战挂钩、追踪、结算第 7 步与点选面板同一口径；不满足就当没有，客户端不必改。

**被否决的做法**：**只藏单号、照显赏金**：赏金与档位同样是发单帮不想公开的信息。**在 `ActiveBountyByTarget` 的 SQL 里加 `public_board` 条件**：开战入口的自动挂钩也用它，它要先拿到行、再按攻方帮判资格，把条件塞进 SQL 会让两处口径分叉。

**代价与边界**：别帮看不到私单，也就不知道这名目标正被追杀（与追杀榜一致）；合服后别区的单同样不显示。

### 11. 给 Gemini 的验证（§7～§10）

1. 生成（仓库根目录）：本批只改 `guild_db.proto`（库表，不进客户端）。批 V 的 `guild.proto` 若还没生成，按 §6 第 1 步用缺省配置一起生成并同步客户端；否则 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。通过：`go/proto/guild/guild_db.pb.go` 有 `GuildWarCreditOwedRecord`，`guild_war_bounty` 的索引串末尾是 `escrow_op_id`。
2. 编译与静态检查：`cd go/guild; gofmt -l internal/; go build ./...; go vet ./...`（gofmt 无输出）；仓库根目录 `python tools/scripts/check_log_text_english.py`。
3. 纯单测：`cd go/guild; go test ./internal/data -run 'TestWarCreditOwedColumnsCoverEveryProtoField|TestClassifyCreditFence|TestLockCreditFencesWithoutGuardAndVerdictLists|TestWarCreditRecordCarriesTheOwedCreditVerbatim|TestDropListCoversTables|TestGuildWarBountyColumnsCoverEveryProtoField' -count=1 -v`；`go test ./internal/logic -run 'TestFieldTargetBountyHidesGuildOnlyBountiesFromOtherGuilds|TestWarBountyExpiryEveryRounds' -count=1 -v`；再 `go test ./internal/war/... ./internal/logic/... -count=1`。
4. 真库 MySQL：设 `GUILD_TEST_MYSQL_DSN`（库名 `guild_test`），`go test ./internal/data -run 'SettleWarBattle_|ExpireBounty_|PurgeTerminalBounties_' -count=1 -v`，全部 PASS、不得 SKIP（新增 `TestSettleWarBattle_FullCreditWindowDefersOnlyThatPlayer`、`TestSettleWarBattle_BountyPayoutOwedWhenHunterWindowIsFull`、`TestExpireBounty_FullIssuerWindowExpiresAndOwesTheRefund`、`TestPurgeTerminalBounties_DeletesOnlyOldTerminalRows`，批 V 的原用例不得回归）。
5. 真库 TiDB（护栏）：设 `GUILD_ROLE_TEST_DSN`（本地独占 TiDB，见 `role_identity_tidb_test.go`），`go test ./internal/data -run 'TestTiDBWarSettle|TestTiDBExpireBountyOwes|TestTiDBPendingGuildBlocksFreezeAndRemoval' -count=1 -v` 全部 PASS、不得 SKIP。
6. 全包：`go test ./internal/data -count=1`、`go test ./... -count=1`；带 integration 标签的经济端到端：`go test -tags integration ./internal/logic -count=1`（删表清单新增 `guild_war_credit_owed`，`resetEconomyFlowSchema` 按表数守着）。
7. 已有本地库：重建 `guild_war_bounty`（补索引）；新表由 schemamigrate 补建。
