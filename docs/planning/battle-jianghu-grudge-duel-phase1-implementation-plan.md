# 江湖恩怨与天墉生死状 · 一期（M1–M3）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 `xuanming-server-mmo-cpp`（C++/Go 权威服）、`xuanming-server-mmo`（Java 独立服）与 `mmorpg-client`（Unity 纯 C# 非 UI 客户端绑定层）三仓中，交付一期「天墉生死状（1V1 / 2V2·3V3·5V5）＋ 全服模板播报与公开分级观战 ＋ 个人江湖恩怨录 ＋ 帮派恩怨墙 ＋ 30 分钟败者印记与洗尘符 ＋ 百宝阁金帖/洗尘符直售」闭环，严守不波及和平散人、0 战力突破、0 银两对赌与 0 660 料泄漏。

**Architecture:** 在 `go/match` 新增独立模式 `MATCH_MODE_JIANGHU_DUEL = 12`、邀请表、个人恩怨录表与 `SPECTATE_VISIBILITY_PUBLIC` / PVP 回放白名单，复用于 `scene.PrepareBattle` 的 `PlayerItemHoldComp` 备战冻结并在建房失败或 `BATTLE_OUTCOME_VOID` 时自动释放，普通战书预扣的银两只由 `scene` 随玩家存档原子退还（取消 / 作废结算 / SQL 终局复核；2026-11-26 定案 match 不发退费邮件，见文末「普通战书银两预扣与退还的数据一致性定案」）；帮派同帮小队生死状复用既有 `match → match-results → go/guild` 终局投递写入帮派恩怨墙；全服播报由 `go/chat` 新增白名单模板内部接口写入世界历史并扇出；官方直售「金帖生死状（20 灵石）」与「清风洗尘符（10 灵石）」直接挂载现役 `SpiritShopGoods.xlsx`（零改动 `java/mall_game`）。二期（M4–M6 灵潮劫火线、煞气红名、悬赏、思过崖）在单区活人达标后另起独立实施计划。

**Tech Stack:** Protobuf3 / Go (`go-zero`, MySQL/TiDB `schemamigrate`, Redis, Kafka) / C++20 (`entt`, `muduo_grpc`, GoogleTest, MSBuild `/m:1`) / Java 21 (Spring Boot, Dubbo Triple, Maven, `ContractSync`) / C# (Unity WebGL2 & IL2CPP 兼容纯 C# 模型与 `RpcGuard`)。

**Spec:** [battle-jianghu-grudge-duel-bounty-v1.md](battle-jianghu-grudge-duel-bounty-v1.md)（上位红线：[project-vision.md](project-vision.md)、[equipment-economy-charter.md](equipment-economy-charter.md)、[spirit-shop-and-starter-packs-v1.md](spirit-shop-and-starter-packs-v1.md)）

## Global Constraints

- 严格遵守 `xuanming-server-mmo-cpp/AGENTS.md`、`xuanming-server-mmo/AGENTS.md`、`mmorpg-client/AGENTS.md`：中文注释与文档、英文结构化运行日志；严禁手改任何生成产物；严禁覆盖工作区其他并行会话的未提交改动；未获负责人明确授权不执行 `git commit` / `git push` / `git tag`。
- 逻辑库自治（`AGENTS.md §7.6`）：`go/match` 独占 `mmorpg_match`、`go/guild` 独占 `mmorpg_guild`、`go/friend` 独占 `mmorpg_friend`、`go/chat` 独占 `mmorpg_chat`、`scene` 独占玩家存档；跨服只走签名 RPC 或 Kafka 发件箱，**事务内严禁调 RPC**。
- 协议与配表红线（`AGENTS.md §4 / §7`）：永久 ID 全用 `uint64` 且禁拆位/按大小排先后；时间戳用 64 位毫秒；货币用 `uint64`（`CurrencyType`: `0 = kCurrencyGold`, `1 = kCurrencyDiamond`）；`Tip.xlsx` 单轴发号（新增 `//jianghu_error base=38000 width=1000`），严禁手写数字或私有 fault map；字段号只增不复用。
- C++ 硬门禁（`AGENTS.md §9 / §11 / §13`）：严禁类/结构体非静态裸指针成员（每次改 C++ 交付前跑 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`）；严禁新增 C++ 线程（跑 `test_cpp_thread_sources.py`）；MSBuild 串行 `/m:1`；`entt::entity` 异步回调按值捕获并判活，禁 `[=]`/`[&]` 与裸 `this`。
- 模型档位与分工（`AGENTS.md §10`）：分析与写码使用最高档模型；编译/生成/跑测与冒烟按仓规核验，无实测证据不得宣称通过；Codex 仅作 UI 美术图，不参与代码与测试。
- 客户端双形态（微信/抖音小游戏 WebGL2 + iOS/Android IL2CPP）：纯 C# 客户端与模型层禁止使用 `Thread`、`Task.Run`、`Task.Delay`、`Socket`、`Reflection.Emit`，全部回调受 `(connectionIdentity, playerId, generation)` 代次围栏保护，倒计时以服务端 `server_time_ms` ＋ 单调时钟锚定。
- 合规与反骚扰红线：对外播报与败者印记 100% 走服务端白名单模板，严禁自由文本；禁用“杀人/强杀/监狱/认怂”等敏感词；严禁自由银两对赌、严禁 660 核心材料与论剑 Elo 泄漏；邀请期绝不扣费、绝不发全服公告。
- 指标红线：Prometheus label 严禁包含 `player_id`，只允许 `{action, mode, writ_tier, outcome}` 等低基数枚举。

## Review Focus

1. **小队中混入开启「闭关免战」或双向拉黑成员时的静默越权**：发起 2V2–5V5 生死状时，哪怕只有目标队 1 名非队长队员开启了免战（且与发起方非好友），或两队任意 1 对成员存在单向/双向 `friend_block`，必须整单拒绝；且开启免战者本人发起战书必须同样被拒。（验证绑定至 Task 3 & Task 4）
2. **并发应战 / 扣费成功但 `CreateBattle` 失败或中途 `BATTLE_OUTCOME_VOID` 导致吞掉 ¥2 金帖**：发起方队长 `ItemHold` 冻结 1 张金帖（或 500 银两）后，若对手在最后一毫秒退队、备战超时、`CreateBattle` 失败、或已开打后节点崩溃裁决 `VOID`/零回合中止，必须保证冻结释放、银两由 `scene` 一条通道退还（取消、作废结算、SQL 终局复核）且绝不重退两次；取消没送到在线实体时也必须最终退还。（验证绑定至 Task 2 & Task 4；定案见文末）
3. **5V5 多人局冲爆个人恩怨录 50 人上限 & 角色过户背前任旧仇**：5V5 生死状终局只能在双方队长的个人恩怨录记 1 对关系（而非 5×5 写入 25 对），50 人上限按最旧 `last_battle_ms` 精准淘汰；角色过户（`owner_revision` 递增）后，新主人查恩怨录必须为空且原败者印记立即失效。（验证绑定至 Task 2 & Task 5）
4. **打满回合上限误落入默认 B 胜分支，或生死状误刷 660 首套材料 / 论剑 Elo / 误将普通私密切磋公开**：`MATCH_MODE_JIANGHU_DUEL` 满回合或 720s 到期必须判 `BATTLE_OUTCOME_DRAW`；`EvaluateBattle` / `RatingStore` 必须返回 0 材料与 0 分；`MATCH_MODE_PVP_CHALLENGE`（未勾自定义公开）必须维持 `PRIVATE`。（验证绑定至 Task 3 & Task 5）
5. **同账号/重试重放触发重复全服刷屏或重复扣清风洗尘符**：`ClearJianghuDefeatMark` 必须过 `ClientRequestLedgerComp` 幂等账本，同 `client_request_id` 重放不重复扣符或扣 1,000 银两；同一 `battle_id` 的开局/终局播报与帮派恩怨墙投递必须幂等去重，绝不发两条系统公告或重复记两场帮派战绩。（验证绑定至 Task 2、Task 4 & Task 5）

---

## 契约冻结总览（C++/Go、Java、Unity 三仓唯一事实源）

### 1. Proto 增量清单（所有字段号均为末尾追加，不复用旧号）

| 源文件 | 新增类型 / 枚举 / 字段 / RPC |
|---|---|
| `proto/common/component/player_jianghu_comp.proto` *(新建)* | `message PlayerJianghuComp`：`bool duel_refuse_strangers = 1;`、`uint64 owner_revision = 2;`、`uint64 defeat_mark_expires_at_ms = 3;`、`uint64 defeat_mark_winner_id = 4;`、`string defeat_mark_winner_name = 5;`、`uint32 defeat_mark_style_id = 6;`、`uint32 defeat_mark_is_guild_name = 7;`、`uint64 defeat_mark_battle_id = 8;`、`uint64 clear_badge_expires_at_ms = 9;`、`uint32 clear_badge_style_id = 10;` |
| `proto/common/database/mysql_database_table.proto` | `player_database` 追加：`PlayerJianghuComp jianghu_component = 35 [(OptionPersistGroup) = 4]; // PROFILE`（加载无条件挂组件，存盘缺组件 fail-closed，旧档缺失默认空合法） |
| `proto/match/match_service.proto` | `MatchMode` 追加：`MATCH_MODE_JIANGHU_DUEL = 12;`（天墉生死状 1V1–5V5，公开观战、录回放、0 材料、0 Elo）。<br>新增枚举 `JianghuWritTier`：`JIANGHU_WRIT_TIER_UNSPECIFIED = 0;`、`JIANGHU_WRIT_TIER_SILVER = 1;`（500 银两普通战书）、`JIANGHU_WRIT_TIER_GOLD_ITEM = 2;`（`Item 3411` 金帖生死状）。<br>`MatchService` 末尾追加 5 个客户端 RPC + 2 个 `CLIENT_RPC_PUSH`：<br>① `CreateJianghuDuelInvite` ② `ConfirmJianghuDuelMember`（发起队/应战队成员本人确认）③ `RespondJianghuDuelInvite`（目标队长拒绝或启动队内 30s 确认）④ `ListJianghuGrudges`（查个人近 50 宿敌与近 20 场战报 ID）⑤ `DismissJianghuGrudge`（单向一笔勾销）⑥ `NotifyJianghuDuelInvite` (`CLIENT_RPC_PUSH`) ⑦ `NotifyJianghuDuelStatus` (`CLIENT_RPC_PUSH`) |
| `proto/match/match_authority.proto` | 新增 3 张 `mmorpg_match` 权威表 proto：`MatchJianghuDuelInvitationRecord`（`match_jianghu_duel_invitation`，含 `duel_invite_id`、`writ_tier`、双队冻结成员与确认位图、`owner_account`/`owner_revision`、`expires_ms`、`confirm_deadline_ms`、`state`、`battle_id`）、`MatchGrudgeRelationRecord`（`match_grudge_relation`，主键 `(player_id, owner_revision, peer_player_id)`，存胜负平计数、`last_battle_ms`、`dismissed`）、`MatchGrudgeLogRecord`（`match_grudge_log`，主键 `(player_id, owner_revision, peer_player_id, battle_id)`，存回合数、胜负、战书档位、完成时刻） |
| `proto/scene/scene.proto` | `PrepareBattleRequest` 追加：`JianghuDuelPrepareFee jianghu_duel_fee = 12;`（仅对发起方队长填充：`duel_invite_id`、`writ_tier`、`item_config_id`、`silver_cost`；`scene` 用 `PlayerItemHoldComp` 按 `battle_id` 冻结并校验）；新增 `CheckJianghuDuelEligibility` 内部 RPC（批量核查目标成员 `duel_refuse_strangers` 与发起队长资费预检，不修改状态） |
| `proto/scene/player_jianghu.proto` *(新建)* | `SceneJianghuClientPlayer`（`OptionIsPlayerService = true`, `OptionIsClientProtocolService = true`）：① `GetJianghuProfile`（查免战开关、败者印记、清风自嘲徽记、剩余毫秒）② `SetJianghuRefuseStrangers`（切换闭关免战开关）③ `ClearJianghuDefeatMark`（带 `client_request_id` 与 `mode`: `1 = 1000 银两普通洗尘`, `2 = 消耗 Item 3412 清风洗尘符`） |
| `proto/battle/battle_data.proto` | `BattleActivityContext` 或独立轻量上下文 `JianghuDuelBattleContext`（透传 `duel_invite_id`、`writ_tier`、`challenger_captain_id`、`target_captain_id`、`side_a_guild_id`、`side_b_guild_id`、`side_a_guild_name`、`side_b_guild_name`），随 `CreateBattleRequest` 冻结并原样回传 `BattleSettlementData` 与 `BattleResultEvent` |
| `proto/friend/friend_internal.proto` *(新建或追加)* | `FriendInternal.BatchCheckSocialRelations`（带 `ServiceCallAuth`）：输入两队成员列表，返回是否存在任一双向 `friend_block` 及全员是否互为好友（供 `go/match` 邀请前只读校验，严守一库一服务） |
| `proto/chat/chat_internal.proto` *(新建或追加)* | `ChatInternal.PublishSystemWorldNotice`（带 `ServiceCallAuth`）：输入 `notice_id`（幂等键，如 `battle_id:stage`）、`template_id`（白名单枚举：开战普通/金帖、战果普通/金帖）、权威展示名与 `battle_id`，由 `go/chat` 写世界历史并经 `worldPublisher` 扇出 |
| `proto/guild/guild_db.proto` & `proto/guild/guild.proto` | `mmorpg_guild` 新增 2 张表：`GuildJianghuRivalSummaryRecord`（主键 `(guild_id, rival_guild_id)`，胜负平计数）与 `GuildJianghuGrudgeRecord`（主键 `(guild_id, battle_id)`，每帮保留近 20 场）；`GuildService` 末尾追加 `GetGuildJianghuGrudgeWall`（仅本帮成员可读） |

### 2. 配置表与 Tip 清单

- `data/Item.xlsx` 末尾顺延新增 2 行（按 2026-11-26 实时最大号 `3414` 顺延）：
  - `3415`：**金帖生死状**（绑定仅自用、`max_stack_size = 99`、`icon_key = "UI/Ugui/JianghuDuelV1/item_gold_duel_writ"`）
  - `3416`：**清风洗尘符**（绑定仅自用、`max_stack_size = 99`、`icon_key = "UI/Ugui/JianghuDuelV1/item_clear_dust_charm"`）
- `data/SpiritShopGoods.xlsx` 追加 2 行（直接复用现役百宝阁 `tab_id = 3 外观便利`，零改 `java/mall_game`）：
  - `goods_id = 305`：金帖生死状（`currency_type = 1` 灵石、`price = 20`、`item_id_1 = 3415`、`item_count_1 = 1`、`item_bound_1 = 1`、`max_buy_per_request = 20`、不限购）
  - `goods_id = 306`：清风洗尘符（`currency_type = 1` 灵石、`price = 10`、`item_id_1 = 3416`、`item_count_1 = 1`、`item_bound_1 = 1`、`max_buy_per_request = 20`、不限购）
- `data/schema/jianghurule_table.proto` + `data/JianghuRule.xlsx`（`id = 1` 单行全局规则，支持热更）：
  - `silver_writ_cost = 500`、`gold_writ_item_id = 3415`、`invite_ttl_sec = 60`、`team_confirm_sec = 30`、`same_target_cooldown_sec = 300`、`daily_unmatched_invite_limit = 20`、`duel_round_limit = 30`、`duel_timeout_sec = 720`、`defeat_mark_duration_sec = 1800`、`silver_clear_cost = 1000`、`clear_item_id = 3416`、`clear_badge_duration_sec = 1800`、`grudge_max_rivals = 50`、`grudge_max_logs_per_rival = 20`、`guild_wall_max_records = 20`、`streak_highlight_threshold = 3`。
- `data/PlayMode.xlsx`：为 `MATCH_MODE_JIANGHU_DUEL (12)` 注册默认天墉城生死擂模板行（`id = 474, match_mode = 12, battle_config_id = 0, enabled = True, team_size_max = 0`）。
- `data/tip/Tip.xlsx`：末尾新增独立组头 `//jianghu_error base=40000 width=1000`，分配 14 个错误码（仅 `JianghuServiceBusy` `fault = 1`）；`player_database` 组件字段顺延取 `37`。

---

## 分任务实施拆解（Task 1–7，覆盖 M1–M3 全链路与双服/客户端同步）

### Task 1: 协议源、配置表源、Tip 轴与三仓契约生成（M1）

**Files:**
- Create: `proto/common/component/player_jianghu_comp.proto`
- Create: `proto/scene/player_jianghu.proto`
- Create: `proto/friend/friend_internal.proto`
- Create: `proto/chat/chat_internal.proto`
- Create: `data/schema/jianghurule_table.proto`
- Create: `data/JianghuRule.xlsx`
- Modify: `proto/common/database/mysql_database_table.proto`（末尾追加字段 `35`）
- Modify: `proto/match/match_service.proto`、`proto/match/match_authority.proto`
- Modify: `proto/battle/battle_data.proto`、`proto/scene/scene.proto`
- Modify: `proto/guild/guild_db.proto`、`proto/guild/guild.proto`
- Modify: `data/Item.xlsx`、`data/SpiritShopGoods.xlsx`、`data/PlayMode.xlsx`、`data/tip/Tip.xlsx`
- Generated (禁手改): `generated/*`, `cpp/generated/*`, `go/proto/*`, `go/shared/generated/*`, `../mmorpg-client/Assets/Scripts/{Proto,Net,Table}/Generated/*`

**Interfaces:**
- Produces: 上文「契约冻结总览」全部 proto 消息、枚举、服务接口、`JianghuRule` 配置表、`Item 3411/3412`、`SpiritShopGoods 303/304`、`jianghu_error`（`38000+`）Tip 码。

- [ ] **Step 1: 核对并行未提交改动并逐文件追加 proto 源定义**
  检查 `git diff -- proto/ data/` 确认不改动其他会话在途字段号；按「契约冻结总览」创建 4 个新 `.proto` 并在 7 个既有 `.proto` 末尾追加定义（严守 `uint64` ID/时间戳/货币与 `OptionPersistGroup = 4`）。
- [ ] **Step 2: 编写 `data/schema/jianghurule_table.proto` 并用 Python `openpyxl` 增量更新 5 份 `.xlsx`**
  创建 `data/JianghuRule.xlsx`（`id = 1` 单行全默认参数），在 `Item.xlsx` 末尾追加 `3411/3412`，在 `SpiritShopGoods.xlsx` 末尾追加 `303/304`，在 `PlayMode.xlsx` 注册 `MATCH_MODE_JIANGHU_DUEL = 12`，在 `Tip.xlsx` 末尾追加 `//jianghu_error base=38000 width=1000` 及 14 个错误码。
- [ ] **Step 3: 运行权威导表与 proto 生成，并验证 schema 与客户端编译**
  Run in `xuanming-server-mmo-cpp`: `dev.bat gen`（或 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`）与 `python tools/scripts/gen_schema_index.py --check`
  Run in `mmorpg-client`: `.\gen.bat client`
  Expected: 导表与 proto 生成 exit 0，`client_compile_check.ps1` `errors=0`。
- [ ] **Step 4: 注册新 Tip 生成文件与新表到 C++/Go 工程清单**
  检查 `cpp/generated/table/CMakeLists.txt`、`table.vcxproj` 与 `proto.vcxproj` 已包含 `jianghu_error_tip.pb.*`、`jianghurule_table.*`、`player_jianghu*.pb.*`，确认无缺漏。

---

### Task 2: C++ `scene` 玩家江湖组件、百宝阁直售、备战冻结扣费、败者印记与 660 隔离（M1/M2/M3）

**Files:**
- Create: `cpp/libs/services/scene/player/system/player_jianghu.h`
- Create: `cpp/libs/services/scene/player/system/player_jianghu.cpp`
- Create: `cpp/nodes/scene/handler/rpc/player/player_jianghu_handler.h`
- Create: `cpp/nodes/scene/handler/rpc/player/player_jianghu_handler.cpp`
- Modify: `cpp/libs/services/scene/player/system/player_database_loader.cpp`（加载无条件挂 `PlayerJianghuComp`，存盘缺组件 fail-closed，过户 `owner_revision` 不符自动清空印记）
- Modify: `cpp/libs/services/scene/battle/system/player_battle.cpp`（`PrepareBattle` 接入 `jianghu_duel_fee`：金帖 `3411` 走 `BagService::PlaceHold` 按 `battle_id` 冻结、`ConfirmBattle`/开打实扣、`CancelBattlePrepare`/作废释放；普通 500 银两在 `PrepareBattle` 按 `battle_id` 幂等预扣并记待退标记，`CancelBattlePrepare` / 作废结算 / 零回合结算 / SQL 终局复核由 `scene` 一条通道退还（2026-11-26 起不经 `mail`，见文末定案）；`ApplyBattleSettlement` 调用 `PlayerJianghuSystem::ApplyDuelSettlement` 挂 30 分钟败者印记或复仇胜利清标）
- Modify: `cpp/libs/services/scene/player/system/player_equipment_material_reward.cpp`（`EvaluateBattle` 显式 `case match::MATCH_MODE_JIANGHU_DUEL: return eval;` 返回 0 材料）
- Test: `cpp/tests/bag_test/player_jianghu_test.cpp`（新建）与 `cpp/tests/bag_test/player_equipment_material_reward_test.cpp`（追加用例）

**Interfaces:**
- Consumes: Task 1 生成的 `PlayerJianghuComp`、`SceneJianghuClientPlayer`、`JianghuRuleTable`、`jianghu_error_tip`、`BagService::PlaceHold/CommitHold/ReleaseHold`、`ClientRequestLedgerComp`、`PlayerLifecycleSystem::SavePlayerForAssetCommit`。
- Produces: `PlayerJianghuSystem::GetProfile`、`SetRefuseStrangers`、`ClearDefeatMark`、`CheckDuelEligibility`、`ApplyDuelSettlement`、`ReserveDuelFee`、`CommitDuelFee`、`ReleaseDuelFee`。

- [ ] **Step 1: 编写失败的 C++ GTest 单元测试（覆盖核心逻辑与 Review Focus #2/#3/#4/#5）**
  在 `cpp/tests/bag_test/player_jianghu_test.cpp` 与 `player_equipment_material_reward_test.cpp` 编写断言用例：
  - `EvaluateBattle_JianghuDuelReturnsZeroMaterial`：验证 `MATCH_MODE_JIANGHU_DUEL` 胜/负/平 `eligible == false`、`baseUnits == 0`、`bonusUnits == 0`；
  - `JianghuDuelFee_HoldAndReleaseOnCancelOrVoid`：验证 `PrepareBattle` 冻结 `Item 3411`（或 500 银两）后取消或 `BATTLE_OUTCOME_VOID` 100% 释放回原余额、不扣减；
  - `DefeatMark_SetOverwriteExpireAndRevengeClear`：验证战败挂 1800s 印记、连输覆盖刷新、击败原胜方队长/成员立刻清除、过户 `owner_revision` 递增后自动清空；
  - `ClearDefeatMark_IdempotentReplayDoesNotDoubleCharge`：验证同 `client_request_id` 重放 `ClearJianghuDefeatMark`（1,000 银两或 `Item 3412`）只扣一次且返回首次结果；
  - `SpiritShop_CanBuyGoldWritAndClearTalismanBound`：验证 `goods_id = 303/304` 用灵石直购发出 `bound = true` 堆且拒绝绑定灵石混扣。
- [ ] **Step 2: 跑只读静态门禁并运行单测确认失败**
  Run: `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`
  Expected: 新增测试因符号未实现编译失败（红灯）。
- [ ] **Step 3: 实现 `PlayerJianghuSystem`、`PlayerJianghuHandler` 及 `player_battle.cpp` / `player_database_loader.cpp` / `player_equipment_material_reward.cpp` 接缝**
  严守零类内裸指针成员、零新增线程；`ClearDefeatMark` 走 `ExchangeInventoryExact` / `beforeCommit` 同一次关键进度存盘原子提交；`ApplyDuelSettlement` 严格校验 `owner_revision` 一致后再落盘印记。
- [ ] **Step 4: 串行构建并运行 C++ 门禁与单测验证通过**
  Run: `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`、`python tools/scripts/tests/test_cpp_thread_sources.py`
  Run (MSBuild 串行 `/m:1`): 编译 `scene.lib`、`scene.exe`、`bag_test.exe` 并运行 `bag_test.exe --gtest_filter=*Jianghu*:*EquipmentMaterialReward*:*SpiritShop*`
  Expected: 静态门禁全绿，GTest 用例 100% PASS。

---

### Task 3: C++ `battle` 判平分支、Go `friend` 批量关系查询与 Go `chat` 模板全服播报（M2 前置）

**Files:**
- Modify: `cpp/libs/services/battle/system/turn_battle_engine.cpp`（`MATCH_MODE_JIANGHU_DUEL` 满回合上限 `duel_round_limit` 权威判 `BATTLE_OUTCOME_DRAW`、满血蓝隔离禁背包药、透传 `JianghuDuelBattleContext` 到终局结算）
- Test (C++): `cpp/tests/battle_test/` 追加 `JianghuDuelRoundLimitDrawsTest`
- Create/Modify (Go `friend`): `go/friend/internal/logic/batch_check_social_logic.go`、`go/friend/internal/server/friend_internal_server.go`、`go/friend/internal/logic/batch_check_social_test.go`
- Create/Modify (Go `chat`): `go/chat/internal/logic/system_world_notice_logic.go`、`go/chat/internal/server/chat_internal_server.go`、`go/chat/internal/logic/system_world_notice_test.go`

**Interfaces:**
- Consumes: Task 1 生成的 `FriendInternal.BatchCheckSocialRelations`、`ChatInternal.PublishSystemWorldNotice`、`JianghuDuelBattleContext`、`worldPublisher.PushWorld`。
- Produces: 服务端白名单模板全服系统播报（按 `notice_id` 幂等去重、零自由文本）、两队 `friend_block` + 好友批量查询 RPC、生死状满回合判平引擎行为。

- [ ] **Step 1: 为三处底座分别编写失败的单元测试**
  - C++ `battle_test`: `JianghuDuelRoundLimitDraws`（打满 30 回合双方均存活 → `BATTLE_OUTCOME_DRAW`，绝不落入 B 胜）；
  - Go `friend`: `TestBatchCheckSocialRelations_DetectsAnyDirectionalBlockAndFriendPair`（任意单向拉黑返回 `has_block = true`，全员互为好友返回 `all_friends = true`）；
  - Go `chat`: `TestPublishSystemWorldNotice_TemplateOnlyAndIdempotent`（未知 `template_id` 拒绝；同 `notice_id` 重发只写一次历史并只推送一次 Kafka，绝不接受客户端自由文本）。
- [ ] **Step 2: 运行测试确认红灯**
  Run: `go test -count=1 ./internal/logic -run TestBatchCheckSocialRelations`（`go/friend`）与 `go test -count=1 ./internal/logic -run TestPublishSystemWorldNotice`（`go/chat`）
  Expected: FAIL（方法未实现）。
- [ ] **Step 3: 实现 C++ `turn_battle_engine.cpp` 判平分支、Go `friend` 批量关系 RPC 与 Go `chat` 模板公告 RPC**
  `go/chat` 内部维护轻量幂等去重记录（同库表或有界窗口键），4 套固定白名单文案全部软化为仙侠江湖词（`【天墉生死擂】`），不含任何违规敏感词。
- [ ] **Step 4: 编译并运行全部新增测试确认通过**
  Run: `test_cpp_no_raw_pointer_member.py`、串行 `/m:1` 编 `battle_test.exe` 并运行过滤用例；`go test -count=1 ./...`（`go/friend`、`go/chat` 受影响包）
  Expected: 全量 PASS。

---

### Task 4: Go `match` 生死状邀请状态机、全员确认、公开观战与 `VOID` 退费闭环（M2）

**Files:**
- Create: `go/match/internal/authority/jianghu_duel.go`（`match_jianghu_duel_invitation` CRUD、双队成员确认状态机、`owner_account/owner_revision` 围栏、5 分钟同目标冷却与每日 20 次未成局频控）
- Create: `go/match/internal/logic/jianghu_duel_logic.go`（`CreateJianghuDuelInvite`、`ConfirmJianghuDuelMember`、`RespondJianghuDuelInvite`、调 `friend` 拉黑/好友校验、调 `scene` 免战与资费预检、成局调 `chat` 发开战全服播报）
- Modify: `go/match/internal/authority/schema.go`（`Tables()` 与 `CheckCandidateIndexes()` 注册 3 张新表及必要查询索引）
- Modify: `go/match/internal/logic/spectate_visibility.go`（`spectateVisibilityOfMode` 新增 `MATCH_MODE_JIANGHU_DUEL -> PUBLIC`）
- Modify: `go/match/internal/logic/battle_replay_policy.go`（`replayPolicyOfMode` 新增 `MATCH_MODE_JIANGHU_DUEL -> true`）
- Modify: `go/match/internal/logic/gather.go`（`gatherTeamAssignment` 支持 `MATCH_MODE_JIANGHU_DUEL` 等额 `1V1–5V5` 分边；`preparePlayerFencedWithExtras` 仅对发起队长透传 `jianghu_duel_fee`；建房失败时只发 `CancelBattlePrepare`（`skip_jianghu_silver_refund` 恒为 false），不发退费邮件，退还归 `scene`，2026-11-26 定案见文末）
- Modify: `go/match/internal/constants/errors.go` + `errors_test.go`（绑定 `jianghu_error` 生成枚举，保持 `TestNoHandWrittenTipCodes` 全绿）
- Test: `go/match/internal/authority/jianghu_duel_test.go`、`go/match/internal/logic/jianghu_duel_logic_test.go`

**Interfaces:**
- Consumes: Task 1–3 的 `FriendInternal.BatchCheckSocialRelations`、`Scene.CheckJianghuDuelEligibility`、`Scene.PrepareBattle` / `Scene.CancelBattlePrepare`、`ChatInternal.PublishSystemWorldNotice`（2026-11-26 起退费不再经 `mail`）。
- Produces: `MatchService` 3 个生死状操作 RPC + 2 个推送、`SPECTATE_VISIBILITY_PUBLIC` 登记、完整建房与 `VOID` 幂等退费链路。

- [ ] **Step 1: 编写失败的 Go 单元测试（覆盖 Review Focus #1/#2/#4/#5）**
  - `TestSpectateVisibilityDecidesEveryMatchMode` & `TestReplayPolicyDecidesEveryMatchMode`（校验 `MATCH_MODE_JIANGHU_DUEL` = PUBLIC + 录回放，`PVP_CHALLENGE` 仍默认 PRIVATE）；
  - `TestJianghuDuel_RejectsBlockRefuseStrangersAndSelfRefuse`（目标队任一成员开免战、任意两两拉黑、或开免战者自己主动发帖均拒）；
  - `TestJianghuDuel_TwoSideTeamConfirmationAndAbortOnMemberChange`（发起队全员确认后才发 60s 邀请；目标队 30s 全员确认；途中退队/换人整单作废、0 扣费、0 公告）；
  - `TestJianghuDuel_DeclineTimeoutZeroFeeZeroBroadcast`（拒绝或 60s 超时 0 扣费 0 全服播报，触发 5 分钟同目标冷却）；
  - `TestJianghuDuel_CreateBattleFailAndVoidRefundIdempotent`（2026-11-26 改：建房失败只发一次 `CancelBattlePrepare` 且 `skip_jianghu_silver_refund = false`、不发退费邮件；「只退一次」由 `scene` 侧单测覆盖，见文末定案）。
- [ ] **Step 2: 运行测试确认失败**
  Run: `go test -count=1 ./internal/authority ./internal/logic ./internal/constants`（`go/match`）
  Expected: FAIL（新表/方法未实现）。
- [ ] **Step 3: 实现 `authority/jianghu_duel.go`、`logic/jianghu_duel_logic.go` 及 `schema`/`spectate_visibility`/`battle_replay_policy`/`gather` 改动**
  严格保证所有 RPC 调用在 SQL 事务外执行；单目标同一时刻仅允许 1 条待应答生死状；拒绝写墓碑并记录冷却。
- [ ] **Step 4: 运行 Go 测试套件确认全绿**
  Run: `go test -count=1 ./internal/authority ./internal/logic ./internal/constants`（`go/match`）
  Expected: PASS（含 `TestNoHandWrittenTipCodes`、`TestSpectateVisibilityDecidesEveryMatchMode`、`TestReplayPolicyDecidesEveryMatchMode`）。

---

### Task 5: Go `match` 个人恩怨录与 Go `guild` 帮派恩怨墙终局归档（M3）

**Files:**
- Create: `go/match/internal/authority/jianghu_grudge.go`（终局写入 `match_grudge_relation` 与 `match_grudge_log`：1V1 双方互记、2V2–5V5 仅记双方队长；每角色按 `last_battle_ms` 裁剪至 50 人、每对裁剪至 20 场；按 `(player_id, owner_revision)` 隔离；`ListJianghuGrudges` / `DismissJianghuGrudge`）
- Modify: `go/match/internal/logic/completion` / `rating.go`（`MATCH_MODE_JIANGHU_DUEL` 有效终局显式跳过 Elo 评分写入，写入恩怨录、触发终局模板全服播报并经既有 `match-results` 发件箱发事件）
- Create: `go/guild/internal/data/jianghu_grudge_repo.go`（`guild_jianghu_rival_summary` 与 `guild_jianghu_grudge_record` 幂等落盘，单帮近 20 场裁剪，帮会解散不继承）
- Modify: `go/guild/internal/data/tables.go`（单调追加新表锁序）与 `go/guild/internal/logic/trial_result_consumer.go`（或并列消费路由：消费 `match-results` 中 `MATCH_MODE_JIANGHU_DUEL` 且两队为不同帮纯同帮队伍的终局事件，幂等写帮派恩怨墙）
- Create: `go/guild/internal/logic/get_guild_jianghu_grudge_wall_logic.go`
- Test: `go/match/internal/authority/jianghu_grudge_test.go`、`go/guild/internal/data/jianghu_grudge_repo_test.go`

**Interfaces:**
- Consumes: Task 4 提交的 `BattleResultEvent`（带 `JianghuDuelBattleContext`）、`go/guild` 既有 `match-results` 消费管线。
- Produces: `MatchService.ListJianghuGrudges`、`DismissJianghuGrudge`、`GuildService.GetGuildJianghuGrudgeWall`。

- [ ] **Step 1: 编写失败的 Go 单元测试（覆盖 Review Focus #3/#4/#5）**
  - `TestGrudgeLedger_5v5OnlyIndexesCaptainsAndCapsAt50Rivals`（5V5 仅写双方队长 2 条关系行；插入第 51 个对手淘汰最旧 `last_battle_ms`）；
  - `TestGrudgeLedger_OwnerRevisionChangeClearsOldGrudges`（`owner_revision` 从 1 升至 2 后查询为空，旧关系不泄露）；
  - `TestJianghuDuel_DoesNotTouchRankedElo`（验证终局不写 `match_rating`）；
  - `TestGuildGrudgeWall_IdempotentDeliveryAndCap20`（重复消费同一 `battle_id` 只记 1 场胜负；第 21 场裁剪最旧记录；同帮或混编野队不入帮派墙）。
- [ ] **Step 2: 运行测试确认失败**
  Run: `go test -count=1 ./internal/authority -run TestGrudgeLedger`（`go/match`）与 `go test -count=1 ./internal/data -run TestGuildGrudgeWall`（`go/guild`）
  Expected: FAIL。
- [ ] **Step 3: 实现个人恩怨录、终局分流与帮派恩怨墙消费/查询逻辑**
  所有查询不返回和平目标所在野外地图或坐标；帮派恩怨墙仅向本帮现役成员开放。
- [ ] **Step 4: 运行 Go 测试验证全绿**
  Run: `go test -count=1 ./internal/authority ./internal/logic`（`go/match`）与 `go test -count=1 ./internal/data ./internal/logic`（`go/guild`）
  Expected: PASS。

---

### Task 6: 独立 Java 服务端双版同步与 `PARITY.md` 核销（`AGENTS.md §12`）

**Files (in `../xuanming-server-mmo`):**
- Synced (禁手改): 经 `java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp` 同步 `game-proto`、`game-table`、`config-data/tables`
- Modify/Create:
  - `game-match`: `MatchMethods.java` 补齐 5 个新客户端 RPC、`MatchModes` / `SpectateVisibilityPolicy` / `BattleReplayPolicy` 注册 `MATCH_MODE_JIANGHU_DUEL = 12`、新增 `com.game.match.jianghu.{JianghuDuelService,JianghuGrudgeStore}`
  - `game-battle-engine`: `TurnBattleEngine.java` 补齐 `MATCH_MODE_JIANGHU_DUEL` 满回合判平分支
  - `game-scene`: `EquipmentCraftService.isEligibleBattle` 排除 `MATCH_MODE_JIANGHU_DUEL`；新增 `JianghuService`（`PlayerJianghuComp` 存取、过户清空、免战开关、败者印记、洗尘符与备战冻结）
  - `game-guild`、`game-friend`、`game-chat`: 补齐帮派恩怨墙消费/查询、批量拉黑好友检查、模板系统播报
  - `PARITY.md`: 登记「江湖恩怨与天墉生死状一期（M1–M3）」双服对齐行
- Test: `game-match` `JianghuDuelParityTest.java`、`game-scene` `JianghuSceneTest.java`

**Interfaces:**
- Consumes: Task 1–5 在 `xuanming-server-mmo-cpp` 冻结的全部客户端可见协议、配置表与 Tip 码。
- Produces: Java 独立服 100% 等价的玩家可见契约与 `PARITY.md` 核销记录。

- [ ] **Step 1: 同步契约并验证 `--check` 通过**
  Run in `xuanming-server-mmo`: `java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp` 然后 `--check`
  Expected: 退出码 0，无契约漂移。
- [ ] **Step 2: 按 Java 21 惯用法编写双服对拍单测（覆盖免战/拉黑、零吞货、5V5 队长恩怨录、过户清空、满回合判平、0 660 料）**
- [ ] **Step 3: 实现 `game-match` / `game-battle-engine` / `game-scene` / `game-guild` / `game-friend` / `game-chat` 逻辑并更新 `PARITY.md`**
  严格遵守 `xuanming-server-mmo/AGENTS.md` 命名空间注入、线程所有权与 `owner_epoch` 围栏规则。
- [ ] **Step 4: 执行 `clean install` 与定向测试验证通过**
  Run in `xuanming-server-mmo`: `./mvnw -B -pl game-proto,game-table,game-battle-engine,game-match,game-scene,game-guild,game-friend,game-chat -am clean test`
  Expected: 0 编译错误，新增与既有测试全部 PASS。

---

### Task 7: Unity 客户端纯 C# 非 UI 协议层、EditMode 单测与美术交接准备（M3）

**Files (in `../mmorpg-client`):**
- Create: `Assets/Scripts/Game/Jianghu/JianghuModels.cs`（纯 C# 不可变视图模型，零 `UnityEngine` 依赖）
- Create: `Assets/Scripts/Game/Jianghu/JianghuClient.cs`（封装 5 个 Match RPC + 3 个 Scene RPC + 1 个 Guild 恩怨墙 RPC + 2 个 S2C 推送监听，`RpcGuard` 节流与 `(connectionIdentity, playerId, generation)` 代次围栏，单调时钟倒计时，严格兼容 WebGL2/IL2CPP）
- Modify: `Assets/Scripts/Game/GameClient.cs`（挂载 `GameClient.Jianghu` 属性，对称退订生命周期）
- Test: `Assets/Tests/EditMode/Jianghu/JianghuClientTests.cs`（附完整 `.meta` 文件）

**Interfaces:**
- Consumes: Task 1 经 `.\gen.bat client` 生成的 `*Rpc.cs`、`Proto/Generated/*.cs`、`MessageIds.cs` 与 `TipFaults`。
- Produces: 稳定冻结的 `GameClient.Jianghu` 纯 C# 接口与只读事件快照，供后续 uGUI 与 Codex 美术切图直接绑定。

- [ ] **Step 1: 编写失败的 EditMode 单测 `JianghuClientTests.cs`**
  断言：① 切号/换连接代次后迟到的战书推送与回包被静默丢弃；② 败者印记与邀请 60s 倒计时仅随单调时钟推进且不漂移；③ `ClearJianghuDefeatMark` 重试复用同一 `client_request_id`；④ 全程零使用 `Thread`/`Task.Run`/`Task.Delay`。
- [ ] **Step 2: 实现 `JianghuModels.cs`、`JianghuClient.cs` 并挂载至 `GameClient.cs`（补齐全部 `.meta`）**
- [ ] **Step 3: 执行客户端编译门禁与 NUnit EditMode 单测**
  Run in `mmorpg-client`: `pwsh -File tools/client_compile_check.ps1` 与 Roslyn/NUnit 运行 `JianghuClientTests`
  Expected: `exit=0, errors=0`，`JianghuClientTests` 100% PASS，无重复 `.meta` GUID。
- [ ] **Step 4: 更新 `docs/PROGRESS.md`（仅追加）并做全链路回归核对**
  核对全部手改文件 `git diff` 未波及他人并行改动；确认未执行未授权的 `git commit` / `git push`。

---

## 普通战书银两预扣与退还的数据一致性定案（2026-11-26，数据覆写审计 r2 第 4 轮）

> 状态：scene 侧已落码，**未生成、未编译、未运行**，验证交 Gemini（本节「验证」）。match 侧（删掉退费邮件通道、`CancelBattlePrepare` 一律带 `skip_jianghu_silver_refund = false`）由另一个修复代理实现，本节只记决定。
> 对应审计条目 r2-jh#1（medium，普通战书 500 银两没有终局兜底）、r2-jh#2（low，邮件与在线两条退费通道叠加成双退或丢）。金帖档不在本节范围：它走战斗道具预留，早已有 SQL 终局复核（[battle-item-reservation.md](../design/battle-item-reservation.md)）。

### 1. 整条流程

只有发起方队长、只有普通战书（`writ_tier = 1`）会写待退标记。标记是 `PlayerJianghuComp` 的三个字段：`pending_silver_fee_battle_id`、`pending_silver_fee_amount`、`pending_silver_fee_review_after_ms`（字段 13，本次新增），随玩家存档 PROFILE 行与货币余额同一次存档原子提交。

```text
PrepareBattle（发起方队长，普通战书）
  ValidateAndCollectPrepareFee：标记是同一局 → 不重扣；标记是别的局 → kFeatureUnavailable，拒绝这次备战，标记不动
  PlaceHold（只有金帖档会有：生死状快照不带道具）→ CommitPrepareSilverFee：提交点再判一次同一条规则 → 扣 500
     → 写标记 (battle_id, 500, review_after_ms = FreezeExpiresAtMs(FIGHTING, deadline_ms) = 同一局道具预留的复核时刻)
     → 关键进度存盘（1 秒内落盘）
        │
        ├─ 出口 1 取消：CancelBattlePrepare 送到在线实体（PREPARING，或 PREPARING 冻结已被 reaper 摘掉）→ 退还
        ├─ 出口 2 作废：VOID 结算（ApplyVoidSettlement，随收件箱账本那一次存档）→ 退还
        ├─ 出口 3 结算：COMPLETE 结算（PrepareBattleSettlement）→ 回合数 > 0 消耗标记（不退）；0 回合退还
        └─ 出口 4 终局复核（reaper，30 秒一拍）：
             · FIGHTING 冻结到期、终局 ABORT / 没有可裁决原件 → 解冻 + 释放预留 + 退还（OnExpiredFightingTerminalAnswer）
             · 冻结已不在这一局（不存在或是别的局）、复核时刻已过、玩家可改 → 与同一局的道具预留合并成一项，
               只发一次 GetBattleTerminalDecision（ReviewBattleHoldsAndFees → ReviewBattleHoldAndFee）：
                 ABORT / NO_ORIGINAL → 释放预留 + 退还；COMPLETE 且结算已在本实体应用过 → 标记按已消耗清掉（缺陷兜底）；
                 COMPLETE / PENDING / UNKNOWN → 都留着，下一拍再问（COMPLETE 的结算到了会经出口 3 消耗标记）

退还（RefundPendingSilverFee，出口 1 / 2 / 4 共用）：
  标记不是这一局 → 什么都不做（幂等）；玩家此刻不可改（player_mutability）→ 什么都不做，标记留给复核；
  AddCurrency(TX_JIANGHU_DUEL_REFUND) 成功 → 同一帧清三个字段 + 关键进度存盘；失败 → 标记留着 + ERROR，复核下一拍重试
另一个清标记的点：角色官方过户（NormalizeOwnerRevision，沿用原策略，不退）。
```

「最终一定退」：标记在存档里，跨重登、换节点不丢；只要 SQL 终局落定为 ABORT 或确认没有可裁决原件，复核就会在玩家可改的某一拍退掉。「最多退一次」：退还与清标记是同一帧的内存改动、随同一次存档提交；所有出口都只认「标记是这一局」，先到的出口退掉之后，后到的出口（重复的回答、迟到的取消、作废结算）都看不到标记。match 不再有第二条通道（§2 问题 3）。

### 2. 问题与定案

#### 问题 1：取消没送到在线实体，500 银两永久不退，下一局还会覆盖标记（r2-jh#1）

- **问题（触发时序）**：① gather 最后一个备战发起方队长，scene 扣 500 并记 `pending_silver_fee_battle_id = X`；② gather 在落 Create 原件之前失败（battle 节点排空或启动中回 not_allocatable、能力查询失败、配表指纹不一致、`SaveSupportedIntent` 出错），或 match 在这一步崩溃；③ match 只发一次 `CancelBattlePrepare`，失败只打日志，租约变化或 `CheckWork` 失败时根本不发（`globalAbortPrepared`）；④ 这一次没落到在线实体上（RPC 失败、队长断线或换了节点）：离线分支只处理战斗锁，备战到期 reaper 只摘 `InBattleComp`，没有原件也就没有 VOID 结算，标记没有任何复核；⑤ 下一局普通战书的 `CommitPrepareSilverFee` 只判「同一 battle_id」，直接覆盖标记，X 那 500 永久丢失。
- **方案**：待退银两并入道具预留那一套 SQL 终局复核。预扣时写 `pending_silver_fee_review_after_ms`，取值与同一局道具预留的 `review_after_ms` 相同（冻结到期时刻）；reaper 每拍收集「复核时刻已过、这一局的冻结不在本实体上」的预留与待退银两，同一 (玩家, 局) 合并成一项，只发一次终局查询，一个回答同时收尾两样，判据与 `battle_freeze_hold::IsBattleHoldReleasable` 相同。FIGHTING 冻结到期查到 ABORT / 没有可裁决原件时，与预留一起退掉。另外，别的局还有未结清的标记时，新的普通战书备战在 `ValidateAndCollectPrepareFee` 与 `CommitPrepareSilverFee` 两处都回 `kFeatureUnavailable`，不覆盖。
- **为什么这样选**：「这一局不会再有结算」只有 SQL 终局能说；道具预留已经有一套现成、测过的兜底（在途登记、单调钟判丢失、不设上限、玩家不可改时不动），复用它就是一条业务知识一个来源。合并成一次查询：`SendTrackedTerminalQuery` 按 (player_id, battle_id) 去重，两条路分别查同一局时，后发的那次撞上前一次的在途登记，回答只交给先发的一方，另一样就得多等一拍甚至一直轮空。今天普通战书局没有道具预留（生死状快照不带道具，金帖档又不写待退标记），两样同局并存的情形还不会出现；照样合并，以后快照或档位一改也不会互相顶掉。复核时刻取冻结到期时刻（截止 + 60 秒终局宽限 + 60 秒投递余量），这时 match 不会再为这一局建房，NO_ORIGINAL 才是终局证据。拒绝码选 `kFeatureUnavailable` 而不是 `kServiceUnavailable`：这是玩家自身状态（旧局未结清）造成的拒绝，与「已有战斗在途」同一个码；`kServiceUnavailable` 在 Tip 表里是 fault，match 会把它记成 scene 内部故障、不定罪。生死状是点名成局（`RequeueOnFailure = false`），两种码都让这一局整体收尾、不自动重排，区别只在 match 的归因与告警口径。
- **被否决的做法**：
  - 到点不查终局直接退：COMPLETE 不受截止约束，发件箱积压时可能在退款之后才到；晚到的结算找不到标记就不消耗，等于白打一局，若同一局又被恢复重跑还会重扣。终局查询的成本只是每局一次只读 RPC。
  - 把银两并进道具预留（`PlayerItemHoldComp`）：预留按物品配置号计数、只约束人物背包里可按配置号计数的单位，出包入口全按这个口径判；货币不是物品，硬塞要给预留加币种维度、改全部出包入口的语义，换来的只是少一个字段。
  - 允许多笔待退（标记改成 repeated）：玩家同一时刻只在一局里，正常情况至多一笔，多笔只在「取消丢失」这类故障下出现；改集合要定上界（玩家存档里的集合必须有上界）、每笔独立复核与告警，复杂度高、收益小。代价换成「等旧局退完再开新的普通战书」。
  - 为银两单起一条查询：见上，与预留复核互相顶掉在途登记。
- **代价与边界**：
  - 旧局的取消丢失时，这名玩家要等旧局复核退款（约在旧局 `deadline_ms` + 120 秒之后的第一拍，match 可用时）才能再开普通生死状，期间发起会在备战时被拒（金帖档、应战、其他玩法不受影响）。match 不可用时复核一直按「未知」留着，钱不丢，只是晚退；越过「复核时刻 + 15 分钟」每 10 分钟一行 ERROR `metric=jianghu_duel_fee_overdue`。
  - 资格预检 `CheckJianghuDuelEligibility` 没有改：它仍只看余额，「旧局未结清」要到成局备战时才被拒（邀请与确认环节白走一遍）。这种情形只在故障后出现，没有为它新增 tip 码或字段。
  - 玩家不在本节点、或此刻不可改（交接 / 退出中 / 没有归属围栏）时不复核，下次加载后接着；reaper 每拍多扫一遍 `PlayerJianghuComp`（每个在线玩家都有），只比一个字段，30 秒一次。
  - 两样的复核时刻按构造相同，合并之后回答同时收尾两样；若以后有路径单独改其中一个时刻，要同步改合并逻辑。

#### 问题 2：先清标记再入账，入账失败就丢钱；不判玩家可改；作废结算走同一函数而收件箱照记 APPLIED（r2-jh#1 附带）

- **问题（触发时序）**：原 `OnBattlePrepareCancelled` 先把两个标记字段清零，再 `AddCurrency`；入账失败（本人或全服获取封禁回 `kAssetBlocked`、余额溢出）只打 ERROR，标记已经没了，这 500 再也找不回。它也不判 `player_mutability::IsPlayerMutable`：交接、退出存盘在途时照样改存档。作废结算（`ApplyVoidSettlement`）调同一个函数，退款失败时收件箱仍记 APPLIED，这一局再不会有第二份作废结算。
- **方案**：`RefundPendingSilverFee` 取代 `OnBattlePrepareCancelled` / `OnBattleVoid`：玩家不可改时什么都不做；先 `AddCurrency`，成功之后同一帧清三个字段并请关键进度存盘；失败保留标记并打 ERROR `metric=jianghu_duel_fee_refund_failed`，由复核下一拍重试。`TX_JIANGHU_DUEL_REFUND` 加进 `GainBlockService::CountsAsGain` 的豁免（与 `TX_AUCTION_RETURN` 同一做法），获取封禁不挡退费。复核不按「本地已应用」收尾待退银两（道具预留照旧按它释放）：作废结算应用了而退款没入账时，复核照样去问，ABORT 就退。COMPLETE 的结算已在本实体应用过而标记还在（结算按生死状路径应用时本该同帧消耗，只可能是缺陷），回答 COMPLETE 时 `ConsumePendingSilverFee` 按已消耗清掉并打 ERROR，免得这名玩家永远开不了普通生死状。
- **为什么这样选**：标记就是「还欠玩家多少」的唯一凭据，先清后入账等于先撕欠条再付钱；退的是玩家自己预扣的钱，不是新增获得，封禁挡住它只会让钱卡住（与审计 r2-trade#1 豁免交易退回同一理由）。`TX_JIANGHU_DUEL_REFUND` 只由 scene 内部调用，不在资产通道任何一条流的允许表里，外部服务借不到这个豁免。
- **被否决的做法**：
  - 退款入账失败时作废结算整笔 DEFER：收件箱的 DEFER 要求零副作用，得把退款预检挪到释放预留之前，而且 DEFER 期间 match 的 ready 票一直占着；有了终局复核兜底，不值得改作废结算的形状。
  - 封禁时跳过退款并清标记（与结算「被封的币种这一局整笔不入账、解封不补」同口径）：那条口径针对新增获得；退款跳过就是没收玩家自己的钱。
- **代价与边界**：封禁期间退款照常入账（玩家本来就有这笔钱）。入账失败只剩余额溢出（只可能是坏数据）与缺货币组件（加载不变量被破坏）两种，标记会一直留着、每拍重试、超期告警，需要人看。金额为 0 或超出 int64 的标记只可能是坏数据，退还时清掉不退并打 ERROR `metric=jianghu_duel_fee_marker_invalid`。

#### 问题 3：两条退费通道在结果不明时必然双退或丢（r2-jh#2，match 侧决定）

- **问题（触发时序）**：① 队长已扣 500，gather 失败且能确定不会建房；② match `refundJianghuSilverOnce` 插退费标记后调 `SendPersonalMail`，mail 已建信但回包超时；③ match 删标记、回 false，随后发 `CancelBattlePrepare(skip = false)`，scene 在线再退 500，邮件里还有 500，玩家净多 500 流通银两；重试也救不回来（`ExpireMs` 每次取当前时刻，摘要不同，mail 回请求冲突）。变体：邮件成功后 `FinishFailed` 没提交、成局恢复重跑，或 `SaveSupportedIntent` 回错但原件已提交、有原件分支固定 `skip = false`，都会再退一次；反向：邮件成功且 `skip = true` 送达后 `FinishFailed` 失败，重跑再扣，再失败时 scene 跳过退费，净丢 500。
- **方案**：退费只留 scene 一条通道。match 删掉退费邮件通道，`CancelBattlePrepare` 一律带 `skip_jianghu_silver_refund = false`（另一个修复代理实现）；scene 删掉读这个字段的跳过分支（读到 true 也照退），proto 字段注释改为「已废弃，恒为 false」，字段号 3 保留不复用。
- **为什么这样选**：两条通道之间没有共享的原子提交点：邮件建信落在 mail 库，待退标记落在玩家存档，「结果不明」时任何一方都无法确定另一方做没做，只能在双退与丢之间二选一。scene 的标记与余额随同一次存档原子提交，单通道下「退了就清标记、没退就留着」天然恰好一次；取消丢失的那一半由终局复核补上（问题 1）。
- **被否决的做法**：
  - 保留邮件通道：上面的双退 / 丢都来自它，接好 match 的发件密钥之后默认可达。
  - 保留邮件、结果不明时保留标记并按同一原件重投到确定结局：要把 `ExpireMs` 冻结进标记行、加后台重投循环，还要让 match 的标记与 scene 的标记跨服务协调（scene 已在线退过时 match 怎么知道、邮件已建信时 scene 怎么知道），等于为 500 银两造一套跨库两阶段提交。
  - 只留邮件、scene 不退：邮件附件要玩家去领，过期作废就是丢；而且 match 拿不到「scene 是否已退过」的事实，作废结算那条路照样会双退。
- **代价与边界**：取消送到在线实体时当场退；没送到时要等终局复核（问题 1 的代价）。match 侧只剩取消 RPC 与作废结算两条「通知」，都不直接动钱。

### 3. 持久字段与兼容

- `PlayerJianghuComp.pending_silver_fee_review_after_ms = 13`（`uint64`，Unix 毫秒），PROFILE 组随 `player_database.jianghu_component`（字段 37）落库；加载 `emplace` 整份拷贝、存盘 `CopyFrom`，不用改加载器。按 [mixed-version-persist-field-protection.md](../design/mixed-version-persist-field-protection.md) §3.1：台账 `proto/persist_schema_gen.txt` 由生成器维护，当前是 `state=bootstrap`、没有封存，生成时追加一行 `1=PlayerJianghuComp#13 optional uint64 pending_silver_fee_review_after_ms`（引导期并入世代 1），不手改台账；`PlayerJianghuComp` 是可携带类型，往返一致性测试（`PersistRoundTripTest`）按描述符自动覆盖新字段，不用改夹具。
- 语义耦合（§3.2 R5）：三个字段必须一起写、一起清（`ClearPendingSilverFee`）。不认识字段 13 的旧版本写新标记时会留下旧局的复核时刻，新版本可能提前复核那一局。项目尚未上线、玩家数据可清空，首个上线版本就带这个字段，没有混版本窗口；上线之后再改这组字段要按 R5 评审。
- `CancelBattlePrepareRequest.skip_jianghu_silver_refund = 3` 只改注释；两个 proto 都不是客户端可见协议（客户端仓没有这两个类型的生成物），不用跑客户端生成。

### 4. 可观测性（scene 结构化日志，英文）

| 日志 | 级别 | 含义 |
|---|---|---|
| `metric=jianghu_duel_fee_refunded reason=…` | INFO | 退还成功；reason = prepare_cancelled / void_settlement / ABORT / NO_ORIGINAL |
| `metric=jianghu_duel_fee_refund_deferred` | WARN | 玩家此刻不可改，标记留给复核 |
| `metric=jianghu_duel_fee_refund_failed` | ERROR | 入账失败，标记留着，复核下一拍重试 |
| `metric=jianghu_duel_fee_pending_other_battle` | WARN | 别的局未结清，新的普通战书备战被拒（`where=commit` 是提交点复核） |
| `metric=jianghu_duel_fee_overdue` | ERROR | 越过「复核时刻 + 15 分钟」仍没收尾，每 10 分钟一行 |
| `metric=jianghu_duel_fee_consumed_after_applied_settlement` | ERROR | COMPLETE 已应用而标记还在（缺陷兜底），清掉不退 |
| `metric=jianghu_duel_fee_marker_invalid` | ERROR | 标记金额不合法（坏数据），清掉不退 |

值班固定查询（写法同 `battle_item_hold_overdue`）：`count_over_time({job="cpp_nodes", service="scene"} |= "metric=jianghu_duel_fee_overdue" [10m]) > 0`。本批没有往告警清单里加规则。

### 5. 改动清单

| 类别 | 文件 |
|---|---|
| proto | `proto/common/component/player_jianghu_comp.proto`（字段 13）、`proto/scene/scene.proto`（`skip_jianghu_silver_refund` 注释） |
| scene | `cpp/libs/services/scene/player/system/player_jianghu.{h,cpp}`（拒绝覆盖、`CommitPrepareSilverFee` 带复核时刻、`RefundPendingSilverFee` / `ConsumePendingSilverFee` 取代 `OnBattlePrepareCancelled` / `OnBattleVoid`）、`cpp/libs/services/scene/battle/system/player_battle.{h,cpp}`（备战写复核时刻、取消与作废改调退还、冻结到期 ABORT 一并退、`ReviewBattleItemHolds` / `ReviewBattleItemHold` / `OnBattleItemHoldTerminalAnswer` 改为合并复核的 `ReviewBattleHoldsAndFees` / `ReviewBattleHoldAndFee` / `OnBattleHoldAndFeeTerminalAnswer`） |
| 获取封禁 | `cpp/libs/modules/gain_block/gain_block_service.h`（`CountsAsGain` 豁免 `TX_JIANGHU_DUEL_REFUND`） |
| 测试 | `cpp/tests/bag_test/player_jianghu_test.cpp`（改 2 个、加 3 个用例）、`cpp/tests/bag_test/battle_freeze_hold_test.cpp`（新夹具 `BattleSilverFeeReviewTest`，6 个用例） |
| 文档 | 本节与本文正文里「经 mail 退还」的几处、[battle-item-reservation.md](../design/battle-item-reservation.md)、[battle-jianghu-grudge-duel-bounty-v1.md](battle-jianghu-grudge-duel-bounty-v1.md)、[turn-battle-gap-closure.md](../design/turn-battle-gap-closure.md) §11 里的函数名 |
| match（另一个代理） | `go/match/internal/logic/{gather.go, global_match.go, jianghu_duel_logic.go}`：删退费邮件，取消一律 `skip_jianghu_silver_refund = false` |

### 6. 验证（全部未执行，交 Gemini）

仓库根目录，串行，失败不重试，保留首个错误与失败用例输出：

1. 协议生成：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`，再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。通过标准：`cpp/generated/proto/common/component/player_jianghu_comp.pb.h` 出现 `pending_silver_fee_review_after_ms`；`proto/persist_schema_gen.txt` 因本批只多出 `1=PlayerJianghuComp#13 optional uint64 pending_silver_fee_review_after_ms` 一行（别的会话的新增行另算），`state=bootstrap` 不变；`cpp/generated/persist_schema/persist_schema_gen.h` 的 `kPersistSchemaGen` 仍为 1。
2. 台账单测：`cd tools/proto_generator/protogen && go vet ./internal/persistschema/... && go test -count=1 ./internal/persistschema/...`（含 `TestRepositoryLedgerIsCanonical`）。
3. 静态门禁：`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`（2026-11-26 已在本机跑过一次：13/13 OK）、`python tools/scripts/tests/test_cpp_thread_sources.py`、`python tools/scripts/check_log_text_english.py`。
4. 构建：`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`，0 个 `error C` / `LNK`。改了 `modules`（`gain_block_service.h`）与 `scene` 两个库，测试工程链接预编好的库，必须先编库再跑测试；只编局部时顺序是 `cpp/generated/proto/proto.vcxproj` → `cpp/libs/modules/modules.vcxproj` → `cpp/libs/services/scene/scene.vcxproj` → `cpp/tests/bag_test/bag_test.vcxproj`（另编 scene 节点、`currency_test`、`cross_zone_test` 确认链接）。
5. 单测：`build\cpp\tests\bag_test.exe --gtest_filter=PlayerJianghuTest.*:BattleSilverFeeReviewTest.*:BattleItemHoldTest.*:BattleItemHoldRules.*:BattleFreezeHold*:PlayerItemGainBlockTest.*:PersistRoundTripTest.*:PlayerFeaturePersistence*` 全绿（新用例：`PlayerJianghuTest.SilverWritPendingForAnotherBattleRejectsTheNewPreparationWithoutTouchingTheMarker`、`PlayerJianghuTest.SilverRefundKeepsTheMarkerWhenTheCreditFailsOrThePlayerIsNotMutable`、`PlayerJianghuTest.GainBlockDoesNotStopTheSilverRefund`、`BattleSilverFeeReviewTest.*` 六个；改过的：`PlayerJianghuTest.SilverWritDebitAndCancelRefund`、`PlayerJianghuTest.VoidAndZeroRoundRefundPendingSilverFeeOnce`），再跑一次完整 `bag_test.exe`；`currency_test.exe` 与 `cross_zone_test.exe` 完整套件不回归。
6. match 侧另一个代理的改动按它自己的验证单跑；两边都合入后补一次 robot 生死状冒烟（普通战书成局、成局失败取消、`VOID`），核对每局 500 银两恰好扣一次、退一次或被结算消耗一次。

### 7. Java 版

Java 版（`xuanming-server-mmo`）的普通战书退费走 `match_jianghu_refund` + 个人邮件（`PARITY.md` 2026-11-26 江湖恩怨一期条目），是另一套实现，同类问题（两条通道、取消丢失）是否存在待按 Java 的模型核对。本批没有改 Java 代码，也没有登记 `PARITY.md`，由主会话决定是否登记。

## K8s 上的跨服务接线(2026-11-26 补,改名与内容安全第三轮复核时发现)

match 调 friend(下战书前查拉黑与好友关系,`BatchCheckSocialRelations`)、chat(战书与战果的全服系统播报,`PublishSystemWorldNotice`)、guild(纯帮会对阵的帮派归属,`GetPlayerGuild`)三个服务。本机 yaml 里有这三个客户端,K8s 上原来没有:

- `tools/scripts/k8s_deploy.ps1` 给 match 生成的 ConfigMap 没有 `FriendRpc` / `ChatRpc` / `GuildRpc`,三个客户端都是空的。friend 缺失按 fail-closed 处理,集群上**下战书一律回「社交关系校验服务未就绪」**;系统播报与帮派归属静默跳过。
- friend、chat、guild 的 gRPC 准入名单里也没有 match,开了 `-NetworkPolicies` 时即使配上也拨不进去。

已补(未运行):三个客户端直连同 namespace 的 Service(三个服务都不写 `Etcd.Key`,D-13),超时镜像服务 yaml;三份准入名单加 match(金丝雀副本自动补上);契约测试「match 的江湖生死状接线」与准入名单用例的期望;`deploy/k8s/README.md` 的准入名单表。chat 的系统播报是服务端方法,在集群上靠这张准入名单只让 match 拨进来,不另加签名(理由见 `docs/design/rename-and-content-safety.md` §7.3)。验证命令在 `docs/PROGRESS.md` 2026-11-26「改名与内容安全第三轮」。

### r2-tn#3 的五项（数据覆写审计 r2 第 4 轮编号；修法就是上一节，这里补齐另三项）

- **问题（含触发时序）**：同上一节。集群部署之后玩家下战书 → match 先经 friend 查拉黑与好友关系 → K8s 的 match ConfigMap 没有 `FriendRpc`，客户端为空 → 按 fail-closed 一律回「社交关系校验服务未就绪」，生死状在集群上整条不可用；战书与战果的系统播报（chat）、纯帮会对阵的帮派归属（guild）静默跳过。只补 ConfigMap 也不够：friend、chat、guild 的 gRPC 准入名单没有 match，开了 `-NetworkPolicies` 照样拨不进去。
- **方案**：同上一节（ConfigMap 三段直连 infra namespace 的 Service、超时镜像 `go/match/etc/match_service.yaml`、`NonBlock: true`；三份准入名单加 match；契约测试与 README 同步）。
- **为什么这样选**：三个服务都不写 `Etcd.Key`（D-13），直连同 namespace 的 Service 与批 W 给 match 补 `MailRpc`、mail 读 `GuildRpc` 是同一写法；超时从服务 yaml 镜像，本机与集群一个口径，契约测试逐项核对，漏一个就红。`NonBlock` 让依赖没起时不挡 match 启动，安全性不靠启动检查：friend 不可用时下战书在请求处就拒绝，不会放过拉黑。
- **被否决的做法**：**经 etcd 发现**：这三个服务不注册 `Etcd.Key`，D-13 已定。**缺客户端时 match 拒启**：friend 偶发不可用就会连带 match 起不来，挡住全部匹配；拒绝放在下战书请求上就够。**只补 ConfigMap、不补准入名单**：开了 `-NetworkPolicies` 的环境照样拨不进去，问题只是换了个样子。
- **代价与边界**：friend / chat / guild 的 Service 名或端口要改时同步这三段模板（契约测试「match 的江湖生死状接线」会红）；本机 `go_services` 的地址不受影响；本条只是部署接线，与生死状的银两、恩怨录数据无关。修改由「改名与内容安全第三轮」会话完成（未运行），本审计会话复核了 HEAD 确实缺这三段与三条准入、工作区的补丁补全了它们（端口 50400 / 50700 / 50300 与 `$GoSvcCatalogue` 一致，K8s 侧只有入站准入、没有出站策略）。
