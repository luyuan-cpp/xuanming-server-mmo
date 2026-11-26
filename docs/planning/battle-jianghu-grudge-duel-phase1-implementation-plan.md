# 江湖恩怨与天墉生死状 · 一期（M1–M3）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 `xuanming-server-mmo-cpp`（C++/Go 权威服）、`xuanming-server-mmo`（Java 独立服）与 `mmorpg-client`（Unity 纯 C# 非 UI 客户端绑定层）三仓中，交付一期「天墉生死状（1V1 / 2V2·3V3·5V5）＋ 全服模板播报与公开分级观战 ＋ 个人江湖恩怨录 ＋ 帮派恩怨墙 ＋ 30 分钟败者印记与洗尘符 ＋ 百宝阁金帖/洗尘符直售」闭环，严守不波及和平散人、0 战力突破、0 银两对赌与 0 660 料泄漏。

**Architecture:** 在 `go/match` 新增独立模式 `MATCH_MODE_JIANGHU_DUEL = 12`、邀请表、个人恩怨录表与 `SPECTATE_VISIBILITY_PUBLIC` / PVP 回放白名单，复用于 `scene.PrepareBattle` 的 `PlayerItemHoldComp` 备战冻结并在建房失败或 `BATTLE_OUTCOME_VOID` 时自动释放或经 `mail` 幂等退还；帮派同帮小队生死状复用既有 `match → match-results → go/guild` 终局投递写入帮派恩怨墙；全服播报由 `go/chat` 新增白名单模板内部接口写入世界历史并扇出；官方直售「金帖生死状（20 灵石）」与「清风洗尘符（10 灵石）」直接挂载现役 `SpiritShopGoods.xlsx`（零改动 `java/mall_game`）。二期（M4–M6 灵潮劫火线、煞气红名、悬赏、思过崖）在单区活人达标后另起独立实施计划。

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
2. **并发应战 / 扣费成功但 `CreateBattle` 失败或中途 `BATTLE_OUTCOME_VOID` 导致吞掉 ¥2 金帖**：发起方队长 `ItemHold` 冻结 1 张金帖（或 500 银两）后，若对手在最后一毫秒退队、备战超时、`CreateBattle` 失败、或已开打后节点崩溃裁决 `VOID`/零回合中止，必须保证冻结释放或经 `mail` 幂等退还且绝不重退两次。（验证绑定至 Task 2 & Task 4）
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

- `data/Item.xlsx` 末尾顺延新增 2 行（当前最大 `3410`）：
  - `3411`：**金帖生死状**（绑定仅自用、`max_stack_size = 99`、`icon_key = "UI/Items/3411"`）
  - `3412`：**清风洗尘符**（绑定仅自用、`max_stack_size = 99`、`icon_key = "UI/Items/3412"`）
- `data/SpiritShopGoods.xlsx` 追加 2 行（直接复用现役百宝阁 `tab_id = 3 外观便利`，零改 `java/mall_game`）：
  - `goods_id = 303`：金帖生死状（`currency_type = 1` 灵石、`price = 20`、`item_id_1 = 3411`、`item_count_1 = 1`、`item_bound_1 = 1`、`max_buy_per_request = 20`、不限购）
  - `goods_id = 304`：清风洗尘符（`currency_type = 1` 灵石、`price = 10`、`item_id_1 = 3412`、`item_count_1 = 1`、`item_bound_1 = 1`、`max_buy_per_request = 20`、不限购）
- `data/schema/jianghurule_table.proto` + `data/JianghuRule.xlsx`（`id = 1` 单行全局规则，支持热更）：
  - `silver_writ_cost = 500`、`gold_writ_item_id = 3411`、`invite_ttl_sec = 60`、`team_confirm_sec = 30`、`same_target_cooldown_sec = 300`、`daily_unmatched_invite_limit = 20`、`duel_round_limit = 30`、`duel_timeout_sec = 720`、`defeat_mark_duration_sec = 1800`、`silver_clear_cost = 1000`、`clear_item_id = 3412`、`clear_badge_duration_sec = 1800`、`grudge_max_rivals = 50`、`grudge_max_logs_per_rival = 20`、`guild_wall_max_records = 20`。
- `data/PlayMode.xlsx`：为 `MATCH_MODE_JIANGHU_DUEL (12)` 注册默认天墉城生死擂副本配置模板行。
- `data/tip/Tip.xlsx`：末尾新增独立组头 `//jianghu_error base=38000 width=1000`，分配错误码 `kJianghuDuelDisabled`、`kJianghuBlockedByPeer`、`kJianghuTargetRefusesStrangers`、`kJianghuSelfRefusesStrangers`、`kJianghuSameTargetCooldown`、`kJianghuDailyInviteLimitReached`、`kJianghuTeamSizeMismatch`、`kJianghuTeamChangedAborted`、`kJianghuInsufficientWritOrSilver`、`kJianghuInvitePendingOrExpired`、`kJianghuNotInviteParticipant`、`kJianghuNoActiveDefeatMark`、`kJianghuGrudgeNotFound`、`kJianghuServiceBusy`（仅此项 `fault = 1`）。

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
- Modify: `cpp/libs/services/scene/battle/system/player_battle.cpp`（`PrepareBattle` 接入 `jianghu_duel_fee`：金帖 `3411` 走 `BagService::PlaceHold` 按 `battle_id` 冻结、`ConfirmBattle`/开打实扣、`CancelBattlePrepare`/作废释放；普通 500 银两在 `PrepareBattle` 按 `duel_invite_id` 幂等预扣并在 `CancelBattlePrepare` 原路返还或经 `mail` 对 `CreateBattle` 失败/`BATTLE_OUTCOME_VOID`/零回合中止幂等退还；`ApplyBattleSettlement` 调用 `PlayerJianghuSystem::ApplyDuelSettlement` 挂 30 分钟败者印记或复仇胜利清标）
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
- Modify: `go/match/internal/logic/gather.go`（`gatherTeamAssignment` 支持 `MATCH_MODE_JIANGHU_DUEL` 等额 `1V1–5V5` 分边；`preparePlayerFencedWithExtras` 仅对发起队长透传 `jianghu_duel_fee`；建房失败或 `VOID`/零回合终局时沿 `mail` `SenderKind("match") = 6` 按 `duel_invite_id` 幂等补退已实扣的战书/银两）
- Modify: `go/match/internal/constants/errors.go` + `errors_test.go`（绑定 `jianghu_error` 生成枚举，保持 `TestNoHandWrittenTipCodes` 全绿）
- Test: `go/match/internal/authority/jianghu_duel_test.go`、`go/match/internal/logic/jianghu_duel_logic_test.go`

**Interfaces:**
- Consumes: Task 1–3 的 `FriendInternal.BatchCheckSocialRelations`、`Scene.CheckJianghuDuelEligibility`、`Scene.PrepareBattle`、`ChatInternal.PublishSystemWorldNotice`、`mail` `SenderKind("match") = 6`。
- Produces: `MatchService` 3 个生死状操作 RPC + 2 个推送、`SPECTATE_VISIBILITY_PUBLIC` 登记、完整建房与 `VOID` 幂等退费链路。

- [ ] **Step 1: 编写失败的 Go 单元测试（覆盖 Review Focus #1/#2/#4/#5）**
  - `TestSpectateVisibilityDecidesEveryMatchMode` & `TestReplayPolicyDecidesEveryMatchMode`（校验 `MATCH_MODE_JIANGHU_DUEL` = PUBLIC + 录回放，`PVP_CHALLENGE` 仍默认 PRIVATE）；
  - `TestJianghuDuel_RejectsBlockRefuseStrangersAndSelfRefuse`（目标队任一成员开免战、任意两两拉黑、或开免战者自己主动发帖均拒）；
  - `TestJianghuDuel_TwoSideTeamConfirmationAndAbortOnMemberChange`（发起队全员确认后才发 60s 邀请；目标队 30s 全员确认；途中退队/换人整单作废、0 扣费、0 公告）；
  - `TestJianghuDuel_DeclineTimeoutZeroFeeZeroBroadcast`（拒绝或 60s 超时 0 扣费 0 全服播报，触发 5 分钟同目标冷却）；
  - `TestJianghuDuel_CreateBattleFailAndVoidRefundIdempotent`（建房失败或 `BATTLE_OUTCOME_VOID`/0 回合中止经 `mail` 按 `duel_invite_id` 仅退还 1 次，重放不重退）。
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
