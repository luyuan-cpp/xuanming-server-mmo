> ## 决策覆盖(2026-09-17,效力高于本节正文与 90 清单)
>
> **U1 = 本节默认**(个人次数按游戏日;帮会级进度按活动档期),正文不改。
>
> **U2 = 阵亡也得奖**,正文按"阵亡不得奖"写,以下替换:
>
> 1. §6.29 第 4 步(与 part8 同名小节)的候选集合改为 `candidates = team_index == 0 的玩家 − fled_player_ids`,**不再减 `dead_player_ids`**;§6.17.3、§6.20 同步。
> 2. `BattleResultEvent.dead_player_ids = 11` 与 C++ 回显**保留**(用于统计与日后回退),只是不参与过滤;B6b-srv1 的实现不变。
> 3. 测试口径改为:`TestTrialCandidates` 期望"team 0 − 逃跑"(阵亡者仍在候选,升序去重);T13 改为"逃跑不在候选、阵亡在候选";§6.44 冒烟断言同步。
> 4. 其余过滤条件不变:结算时已不在帮、当日次数已满的仍然不发。

# S6 帮会活动(B6a/B6b)
> 本节由 11 个分部合并而成(原分部名保留在小标题里),另附对抗评审处理记录。

<!-- s6_activities_part1.md -->

# S6 帮会活动(批次 B6a 元宵灯会 / 中秋团圆,B6b 同道历练)— 第 1 部分:结论、现状、配表、规则函数

> 状态:设计稿(2026-09-16 按对抗评审修订,修订记录见 `s6_activities_review.md`),未落码、未编译,待 Codex 验证。遵守 `00_contract.md`;行号是 2026-09-16 的核对值。并行会话(组队、聚宝斋、聊天、属性)正在改同一个工作树,落码前按函数名重新定位。
> 依赖:B1(库与锁序)、B2(推送 `GuildNotifier`/`l.notify`、合服闸门、`GuildRule` 表、客户端 `RefreshQueued`/`DrainQueued`)、B4a/B4b(资产通道)、B5(`guild_member.contribution_*`、`guild.funds`、`guild_asset_op`/`guild_daily_counter` 表、`assetop.Loop`、`withSyncDelivery`、`go/shared/gameday`)。
> **开工前须用户拍板两件事**(第 11 部分 U1、U2):"当期"的含义;历练阵亡者是否得奖。本稿按推荐默认写。

## 6.0 结论(先读这段)

- **一句话**:三个活动都是 go/guild 里的 RPC,玩家只在帮会界面点按钮,不需要地图交互(用户决策 3)。开放与否由配表 `GuildActivity` 决定。
- **两种周期**(类比:个人打卡按"天",帮会集体目标按"这一届活动"):
  1. **个人次数**一律按**游戏日**(`gameday.DayKey`,UTC+8 每天 05:00 重置)。
  2. **帮会进度**(灯会点灯数、团圆锁存、资金只发一次)按**档期**:`GuildPeriodKey(row, now)` = 该行 `start_at_ms` 那天的 DayKey;`0/0` 常开行(仅开发)退化为当天 DayKey。历练的"每日计资金胜场上限"仍按游戏日。见 U1。
- **元宵灯会** `LightGuildLantern`:成员每游戏日点灯 `daily_limit` 次(默认 1),每次立即得 `personal_contribution` 帮贡。全帮本档期点灯人次达到 `guild_threshold` 时,帮会**本档期只得一次** `guild_funds`。
- **中秋团圆** `ClaimGuildReunion`:领奖那一刻批量查帮会成员在线状态,数"在线且入帮满 N 小时"的人数。达到阈值后本档期进度**锁存**,之后每人每游戏日领一次(帮贡 + 物品),不必再凑人。
- **同道历练**:改为**邀请确认制**(评审阻断项)。
  1. `StartGuildTrial` 只建一个 Redis 待确认房间(TTL 30s),推送给被选中的人;
  2. 每个被邀请人调新增的 `RespondGuildTrialInvite{lobby_id, accept}`;任何人拒绝或超时,房间解散;
  3. 全员同意的那次调用里,guild 复核后调 match 内部服务 `MatchInternal.StartActivityBattle`,拿到 battle_id 并登记 `guild_trial_battle`;
  4. 结果:battle 先把带活动上下文的 `BattleResultEvent` 落到 SharedRedis `battle:activity_result:{battle_id}` 再发 Kafka,未销账就重发;guild 消费组 `guild-trial` 结算后删键销账;guild 巡检器兜底"事件丢失"。
- **奖励三类**:
  1. 帮贡:更新 `guild_member`,与计数同一事务;
  2. 帮会资金:更新 `guild.funds`,同一事务;
  3. 物品:`reward_id` → `AssetBundle`,同一事务插 `guild_asset_op`(kind=ACTIVITY_REWARD、流 GUILD_CREDIT、`deadline_ms=0` 永不中止)。**背包满保持 PENDING,腾出空间后自动到账**;历练结算时某人待发行已满 16 条,物品转存 `guild_trial_reward_owed`,由后台循环等有空位再入队,**绝不跳过**。
- **防刷**:
  1. 个人计数挂在玩家上,键 `(player_id, 3, activity_id, DayKey)`,换帮不重置;
  2. 三个活动都要求"入帮满 `GuildRule.activity_join_min_hours` 小时",团圆只数满足该条件的在线成员;
  3. 历练逃跑者、(默认)阵亡者不得奖;
  4. 历练必须被邀请人亲自同意;每个发起人两次建房至少间隔 `trial_invite_cooldown_seconds`;每人同时只能在一个有效房间里。
- **与组队会话的关系**:不复用 `StartTeamMatch`(以队伍为单位、gather 异步不回 battle_id、尚未落码)。组队落码后,可让"队长发起、名单取当前队伍"直接走本 RPC(第 5 部分 6.15)。

## 6.1 已核验的现状

| 事实 | 出处 |
|---|---|
| guild 已加载全部配表,可读 Reward/Dungeon 的 Go 绑定 | `go/guild/internal/svc/servicecontext.go:43`;`go/shared/generated/table/reward_table.go:88` |
| guild 全局 Redis 是单节点 `*redis.Client`;另有 `PlayerLocatorRedisClient`(会话、位置、`battle:lock` 所在的 SharedRedis) | `servicecontext.go:24-25`;`go/match/internal/config/config.go:19` |
| `OnlineStatusResolver.BatchResolve` 只返回在线者,MGET 出错时吞错当全离线 | `go/guild/internal/logic/online_status_resolver.go:25-74` |
| `DefaultHomeZoneLookupTimeout = 1500ms` | `go/guild/internal/logic/home_zone.go:28` |
| 客户端任何传输错误都置 `RequiresReconnect`,须重登才能再用帮会界面 | `mmorpg-client/Assets/Scripts/Game/Guild/GuildClient.cs:199-206` |
| `Request` 在 Busy 时直接 return;`Toggle` 先 `Show()` 再 `Refresh()` | `GuildClient.cs:189`;`GuildUiRoot.cs:102-113` |
| match 无方法白名单,`MatchService` 整体是客户端协议;`sessionInterceptor` 不校验调用方 | `proto/match/match_service.proto:17-18`;`go/match/match_service.go:169-190` |
| 内部服务分文件、不标客户端选项的先例 | `proto/trade/trade_admin.proto:13-18,43-45` |
| gather 第 1 步生成 battle_id;先 `stopWatchingIfAny` 再逐人 PrepareBattle 冻结 | `go/match/internal/logic/gather.go:130-136,160-166,212-221` |
| battle `SendBattleResultEvent` 发送失败只打 LOG_ERROR;个人结算走 R07 发件箱(先落 Redis、后投递、未销账重投) | `cpp/nodes/battle/logic/battle_room_manager.cpp:287-296,1056-1061,1406-1600` |
| 引擎只给"未逃跑且存活"的 team 0 玩家发经验金币;`BattleSettlementData` 有 `is_dead`、`fled` | `cpp/libs/services/battle/system/turn_battle_engine.cpp:1369-1370,1388-1389` |
| `BattleResultEvent` 字段 1–8;topic `match-results`,key=battle_id | `proto/contracts/kafka/match_event.proto` |
| 消费者模板:EnsureTopics、`CommitInterval: 0`、`FirstOffset`;保留 7 天 | `go/match/internal/kafka/result_consumer.go:47-117` |
| 本地 `go_services.ps1 -Zone N` 只给**带双引号**的 `GroupID: "…"` 加 `_zN` | `tools/scripts/go_services.ps1:381-388` |
| 导表 FK 列值 0 / -1 / 空视为"无引用"跳过 | `tools/data_table_exporter/core/foreign_key.py:13` |
| 内部调用 HMAC 验签先例 `callerauth`(login 私有包,他服不可 import) | `go/login/internal/logic/pkg/callerauth/` |
| 客户端活动页是 `RenderUnavailable` 占位;`Page` 属性、`Modal()`/`CloseModal()` 可复用 | `GuildWindow.cs:86-93,133-134,288,363-381` |

## 6.2 配表

### 6.2.1 `data/schema/guildactivity_table.proto`(新建)

```proto
syntax = "proto3";

// GuildActivity 表的权威 schema。源表 data/GuildActivity.xlsx。设计:docs/design/guild-phase2.md §6。
// 时间为 UTC Unix 毫秒,开放区间 [start_at_ms, end_at_ms);两者都为 0 = 常开(仅供开发,上线前填真实档期)。
// 个人次数按游戏日;帮会进度按档期(start_at_ms 当天的游戏日键)。同 type 的启用行时间窗不得重叠。

package mmorpg.cfgtable.v1;

import "cfg_options.proto";

message GuildActivityTable {
  option (cfg_sheet)       = "GuildActivity";
  option (cfg_source_file) = "GuildActivity.xlsx";
  option (cfg_primary_key) = "id";

  uint32 id = 1;
  string name = 2;                 // 界面显示名
  uint32 type = 3;                 // 1 元宵灯会 / 2 中秋团圆 / 3 同道历练
  bool enabled = 4;                // false = 关闭(界面显示"未开放")
  uint64 start_at_ms = 5;
  uint64 end_at_ms = 6;
  uint32 min_guild_level = 7;      // 帮会等级下限(GuildLevel.id)
  uint64 personal_contribution = 8;// 每次参与(点灯 / 领奖 / 历练胜利)个人所得帮贡
  uint64 guild_funds = 9;          // 灯会、团圆 = 本档期达阈值时发一次;历练 = 每个计资金的胜场发一次
  uint32 guild_threshold = 10;     // 灯会:本档期点灯人次阈值;团圆:同时在线人数阈值(0 = 用 GuildRule 兜底);历练:每游戏日计资金胜场上限
  uint32 reward_id = 11 [(cfg_fk) = "Reward"];   // 物品奖励,0 = 无
  uint32 dungeon_id = 12 [(cfg_fk) = "Dungeon"]; // 仅历练:DungeonTable id(= battle_config_id);其它填 0
  uint32 team_size_min = 13;       // 仅历练:含发起人的人数范围;其它填 0
  uint32 team_size_max = 14;
  uint32 daily_limit = 15;         // 每人每游戏日可参与(得奖)次数
}
```

### 6.2.2 默认行 `data/GuildActivity.xlsx`(开发值:常开,阈值按 4 个 robot 能达成来取)

| id | name | type | enabled | start_at_ms | end_at_ms | min_guild_level | personal_contribution | guild_funds | guild_threshold | reward_id | dungeon_id | team_size_min | team_size_max | daily_limit |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 元宵灯会 | 1 | TRUE | 0 | 0 | 1 | 20 | 500 | 3 | 0 | 0 | 0 | 0 | 1 |
| 2 | 中秋团圆 | 2 | TRUE | 0 | 0 | 1 | 30 | 0 | 3 | 1 | 0 | 0 | 0 | 1 |
| 3 | 同道历练 | 3 | TRUE | 0 | 0 | 1 | 50 | 300 | 3 | 2 | 1 | 2 | 5 | 2 |

- 团圆 `guild_funds=0`:用户决策 3 只给团圆设个人奖;代码支持填正数。
- **上线前改值清单**(写进 guild-phase2.md,本批不改):start/end 填节庆档期;灯会阈值 20、团圆阈值 10;`activity_join_min_hours` 24;资金数额与 B5 `GuildLevel.upgrade_cost_funds` 一起定。

### 6.2.3 `GuildRule` 追加三列(契约偏差 1、2)

`data/schema/guildrule_table.proto` 在末尾依次追加(字段号顺延):

| 列 | 类型 | 默认行 id=1 | 注释 / 校验 |
|---|---|---|---|
| `activity_join_min_hours` | uint32 | **0**(开发值) | 参与活动、计入团圆在线人数所需最短入帮时长;≤ 720 |
| `trial_invite_ttl_seconds` | uint32 | **30** | 历练邀请房间有效期;10–120 |
| `trial_invite_cooldown_seconds` | uint32 | **10** | 同一发起人两次成功建房的最小间隔;0–600 |

契约列 `reunion_min_online_members` 默认 **3**;B2 已建表且值不同时以 B2 为准。

### 6.2.4 guild 启动期校验(fail-closed)

`go/guild/internal/activity/config.go`:`func ValidateTables() error` 取表后调纯函数 `validateRows(rows []*pb.GuildActivityTable, rule *pb.GuildRuleTable) error`;在 `guild.go` 的 `svc.NewServiceContext` 之后调用,出错 `logx.Must`,拒绝启动。错误信息必须含表名与行 id。检查项:

1. `GuildRule` 行 id=1 存在,三列在 6.2.3 范围内。
2. 每行:`type ∈ {1,2,3}`,`name` 非空,`daily_limit ≥ 1`;`start_at_ms`、`end_at_ms` 同为 0 或 `end > start`,且二者 ≤ `math.MaxInt64`。
3. `reward_id != 0`:Reward 行存在,`BuildRewardBundle` 非空、每项 `count ≥ 1`、物品种类 ≤ 16(S4 Credit 上限)。
4. type 1:`guild_threshold ≥ 1`,`dungeon_id`、`team_size_*` 为 0。
5. type 2:`guild_threshold` 与 `reunion_min_online_members` 至少一个 ≥ 1。
6. type 3:`dungeon_id != 0` 且 Dungeon 行存在;`2 ≤ team_size_min ≤ team_size_max ≤ 5`;`guild_threshold ≥ 1`。
7. **同 type 且 enabled 的行,时间窗 `[start,end)` 两两不重叠**;`0/0` 视为全时段,与同类型任何其它启用行都算重叠。

## 6.3 规则纯函数(`go/guild/internal/activity/rules.go`,不依赖 DB/Redis)

```go
type State uint8 // 与 proto GuildActivityState 数值一致
const ( StateDisabled State = 1; StateUpcoming State = 2; StateOpen State = 3; StateEnded State = 4 )

func StateOf(row *pb.GuildActivityTable, nowMs uint64) State
//   !enabled → Disabled;start==0&&end==0 → Open;now<start → Upcoming;now>=end → Ended;否则 Open

func DayKey(now time.Time) uint32 { return gameday.DayKey(now) }            // 个人次数键
func GuildPeriodKey(row *pb.GuildActivityTable, now time.Time) uint32
//   type==3 → gameday.DayKey(now)(每日计资金胜场上限)
//   start==0&&end==0 → gameday.DayKey(now)
//   否则 → gameday.DayKey(time.UnixMilli(int64(row.StartAtMs)))
//   (U1 若选"期=游戏日":函数体改为恒 return gameday.DayKey(now),其余代码不动)
func NextResetMs(now time.Time) uint64 { return uint64(gameday.NextDailyReset(now).UnixMilli()) } // 复用 B5,不新增 API

func SelectVisible(rows []*pb.GuildActivityTable, nowMs uint64) []*pb.GuildActivityTable
//   type 1..3 各取一行:Open 中 id 最小;否则 Upcoming 中 start 最小;否则 id 最小;该类型无行则跳过
func PickForWrite(rows []*pb.GuildActivityTable, typ uint32, reqID uint32, nowMs uint64) (*pb.GuildActivityTable, bool)
//   = SelectVisible 给该 type 选出的行;行不存在、id != reqID、或状态不是 Open → false(写 RPC 回 NotOpen)

func JoinedLongEnough(joinTimeMs uint64, nowMs uint64, minHours uint32) bool
//   minHours==0 → true;joinTimeMs==0 → false;joinTimeMs+minHours*3_600_000 <= nowMs

func ReunionThreshold(row *pb.GuildActivityTable, rule *pb.GuildRuleTable) uint32
func BuildRewardBundle(rewardID uint32) (*assetpb.AssetBundle, error)
//   0 → (nil,nil);行不存在 → error;按 item_id 合并 count(跳过 0),升序输出;合并为空 → (nil,nil);单项超 uint32 → error
func RewardItems(b *assetpb.AssetBundle) []*pb.GuildRewardItem   // 视图用,转 GuildRewardItem

func BlockedTip(in BlockInput) uint32   // 按固定优先级返回第一个不满足项;0 = 可参与
type BlockInput struct {
    State State; GuildLevel, MinLevel uint32; Joined bool
    Used, DailyLimit uint32
    Type uint32; ThresholdReached bool; Progress, Threshold uint32
}
//   State!=Open → ActivityNotOpen;GuildLevel<MinLevel → ActivityLevelTooLow;!Joined → ActivityJoinTooRecent;
//   Used>=DailyLimit → ActivityAlreadyClaimed;Type==2 && !ThresholdReached && Progress<Threshold → ThresholdNotReached;否则 0
```

**时钟**:`now` 取 guild 进程墙钟,一次请求只取一次。多实例 NTP 偏差 ≤1s 时,只是 05:00 重置早晚不到 1 秒;同一 DayKey 的上限不变。
**历练跨日**:结算用**确认开战时**写进上下文的 `period_key`/`guild_period_key`,不用结算时刻。

<!-- s6_activities_part2.md -->

# S6 帮会活动 — 第 2 部分:协议 `guild.proto` 与表 `guild_db.proto`

## 6.4 `proto/guild/guild.proto` 追加(B6a 一次加齐五个 RPC)

message id 由 proto-gen 顺延分配。五个 RPC 在 B6a **一起**加入,客户端、网关、MessageLimiter 只需处理一次;`StartGuildTrial`、`RespondGuildTrialInvite` 在 B6b 落地前固定回 `kGuildActivityNotOpen`(第 3 部分 6.6 末尾)。**不 import `asset_op.proto`**:奖励物品用本文件自定义的 `GuildRewardItem`,避免把内部资产通道 schema 带进客户端包。

```proto
enum GuildActivityType {
  GUILD_ACTIVITY_TYPE_UNSPECIFIED = 0;
  GUILD_ACTIVITY_TYPE_LANTERN = 1;   // 元宵灯会
  GUILD_ACTIVITY_TYPE_REUNION = 2;   // 中秋团圆
  GUILD_ACTIVITY_TYPE_TRIAL = 3;     // 同道历练
}

enum GuildActivityState {
  GUILD_ACTIVITY_STATE_UNSPECIFIED = 0;
  GUILD_ACTIVITY_STATE_DISABLED = 1;
  GUILD_ACTIVITY_STATE_UPCOMING = 2;
  GUILD_ACTIVITY_STATE_OPEN = 3;
  GUILD_ACTIVITY_STATE_ENDED = 4;
}

enum GuildTrialLobbyState {
  GUILD_TRIAL_LOBBY_STATE_UNSPECIFIED = 0;
  GUILD_TRIAL_LOBBY_STATE_PENDING = 1;     // 等待被邀请人确认
  GUILD_TRIAL_LOBBY_STATE_LAUNCHING = 2;   // 全员已同意,正在开战(≤5s)
  GUILD_TRIAL_LOBBY_STATE_LAUNCHED = 3;    // 已开战,battle_id 有效
  GUILD_TRIAL_LOBBY_STATE_ENDED = 4;       // 已解散(拒绝 / 超时 / 开战失败),原因见 end_tip_id
}

message GuildRewardItem {
  uint32 item_id = 1;
  uint32 count = 2;
}

message GuildTrialLobbyView {
  uint64 lobby_id = 1;
  uint64 initiator_player_id = 2;
  repeated uint64 member_player_ids = 3;     // 含发起人,发起人在首位
  repeated uint64 accepted_player_ids = 4;   // 已同意者(发起人建房即同意)
  GuildTrialLobbyState state = 5;            // 服务端已按 expire/launch 截止时刻折算成有效状态
  uint64 expire_at_ms = 6;                   // PENDING 截止时刻
  uint64 battle_id = 7;                      // LAUNCHED 时非 0
  uint32 end_tip_id = 8;                     // ENDED 原因:GuildTrialInviteDeclined / GuildTrialInviteExpired / GuildTrialTeamInvalid / GuildTrialServiceBusy / GuildActivityAlreadyClaimed / GuildActivityNotOpen
  repeated string end_parameters = 9;        // 原因 tip 的参数
}

message GuildActivityView {
  uint32 activity_id = 1;
  GuildActivityType type = 2;
  string name = 3;
  GuildActivityState state = 4;
  uint64 start_at_ms = 5;              // 0/0 = 常开
  uint64 end_at_ms = 6;
  uint32 min_guild_level = 7;
  uint32 period_key = 8;               // 当前游戏日 YYYYMMDD(个人次数键)
  uint64 next_reset_ms = 9;            // 下一次 05:00(UTC+8)
  uint64 server_time_ms = 10;
  uint64 personal_contribution = 11;
  uint64 guild_funds = 12;
  uint32 guild_threshold = 13;         // 团圆为生效值(已套 GuildRule 兜底);历练为每日计资金胜场上限
  repeated GuildRewardItem reward_items = 14;
  uint32 daily_limit = 15;
  uint32 my_used_count = 16;           // 本人本游戏日已参与(得奖)次数
  uint32 progress = 17;                // 灯会:本档期点灯人次;团圆:锁存前为当前合格在线人数,锁存后为 0;历练:今日已计资金胜场
  bool threshold_reached = 18;         // 灯会 / 团圆:本档期已达阈值(已锁存)
  bool funds_granted = 19;             // 灯会 / 团圆:本档期资金已发
  uint32 blocked_tip_id = 20;          // 0 = 本人现在可参与(不含在线、战斗等瞬时条件)
  uint32 dungeon_id = 21;
  uint32 team_size_min = 22;
  uint32 team_size_max = 23;
  uint64 my_trial_battle_id = 24;      // 本人当前 battle:lock 指向的、仍为 STARTED 的历练对局;0 = 无
  uint32 my_pending_reward_count = 25; // 本人本活动仍在发放中的物品奖励数(PENDING op 行 + 待入队行)
  uint32 my_pending_reason_tip_id = 26;// 最早一条 PENDING 行的 last_reason(27001 背包满等);0 = 正常排队
  uint32 my_last_reward_reject_tip_id = 27; // 近 24h 最近一次永久拒绝(封禁 / 非法包)的原因;0 = 无
  uint32 join_min_hours = 28;
  uint32 guild_period_key = 29;        // 帮会进度键(档期键;历练为游戏日)
  GuildTrialLobbyView trial_lobby = 30;// 仅历练:本人所在、属于本活动的房间;无则不填
}

message GetGuildActivitiesRequest {}
message GetGuildActivitiesResponse {
  TipInfoMessage error_message = 1;
  repeated GuildActivityView activities = 2;   // 每种类型至多一条,按 type 升序
}
message LightGuildLanternRequest  { uint32 activity_id = 1; }
message LightGuildLanternResponse { TipInfoMessage error_message = 1; GuildActivityView activity = 2; }
message ClaimGuildReunionRequest  { uint32 activity_id = 1; }
message ClaimGuildReunionResponse { TipInfoMessage error_message = 1; GuildActivityView activity = 2; }
// 建邀请房间(不直接开战);member_player_ids 须含发起人
message StartGuildTrialRequest    { uint32 activity_id = 1; repeated uint64 member_player_ids = 2; }
message StartGuildTrialResponse   { TipInfoMessage error_message = 1; GuildActivityView activity = 2; }
// 被邀请人同意 / 拒绝;发起人 accept=false = 取消房间
message RespondGuildTrialInviteRequest  { uint64 lobby_id = 1; bool accept = 2; }
message RespondGuildTrialInviteResponse { TipInfoMessage error_message = 1; GuildActivityView activity = 2; }

// service GuildService 内追加:
  rpc GetGuildActivities(GetGuildActivitiesRequest) returns (GetGuildActivitiesResponse) {}
  rpc LightGuildLantern(LightGuildLanternRequest) returns (LightGuildLanternResponse) {}
  rpc ClaimGuildReunion(ClaimGuildReunionRequest) returns (ClaimGuildReunionResponse) {}
  rpc StartGuildTrial(StartGuildTrialRequest) returns (StartGuildTrialResponse) {}
  rpc RespondGuildTrialInvite(RespondGuildTrialInviteRequest) returns (RespondGuildTrialInviteResponse) {}
```

- 请求体**不带** player_id,身份一律 `callerOf(ctx, 0)`;无会话 → gRPC **`PermissionDenied`**(与 S5 统一,契约偏差 19)。
- `go/guild/internal/session/session.go` 的 `ClientMethods` 追加五个 `pb.GuildService_*_FullMethodName`;`session_test` 的"每个方法要么在白名单、要么在排除清单"断言随之更新,`NotifyGuildChanged` 仍在排除清单。
- `go/guild/internal/server/guild_server.go` 每个 RPC 一行委托。
- 写 RPC 成功只回活动视图;帮贡、资金变了由客户端排队拉 GuildInfo(第 9 部分)。

## 6.5 `proto/guild/guild_db.proto` 追加(S1 修订后由 S6 自行定义)

写法照 S1 §5(`OptionTableName`/`OptionPrimaryKey`/`OptionIndex`、TiDB 三项、`Record` 后缀、枚举值带前缀);新表在 `go/guild/internal/data/tables.go` 的 `Tables()` 追加。**B6a** 加 `GuildActivityProgressRecord`;**B6b** 加其余两张表与两个枚举。

```proto
// ---- B6a ----
message GuildActivityProgressRecord {
  option(OptionTableName) = "guild_activity_progress";
  option(OptionPrimaryKey) = "guild_id,activity_id,period_key";
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 guild_id = 1;
  uint32 activity_id = 2;
  uint32 period_key = 3;             // 帮会期键:activity.GuildPeriodKey
  uint32 progress_count = 4;         // 灯会:点灯人次;历练:已计资金胜场;团圆不用(恒 0)
  uint64 threshold_reached_ms = 5;   // 0 = 未达;非 0 = 首次达成时刻(锁存)
  uint32 funds_granted = 6;          // 0/1:本期资金已发
  uint64 updated_ms = 7;
  // 追加区:从 8 起。
}

// ---- B6b ----
enum GuildTrialBattleState {
  GUILD_TRIAL_BATTLE_STATE_UNSPECIFIED = 0;
  GUILD_TRIAL_BATTLE_STATE_STARTED = 1;   // 已开战、未结算
  GUILD_TRIAL_BATTLE_STATE_SETTLED = 2;   // 已结算(终态;幂等闸门)
  GUILD_TRIAL_BATTLE_STATE_EXPIRED = 3;   // 巡检判定无结果(gather 失败 / 战斗作废);迟到的结果仍可把它结算成 SETTLED
}

enum GuildTrialSettleResult {
  GUILD_TRIAL_SETTLE_RESULT_UNSPECIFIED = 0;
  GUILD_TRIAL_SETTLE_RESULT_WIN = 1;
  GUILD_TRIAL_SETTLE_RESULT_LOSS = 2;          // 失败 / 平局
  GUILD_TRIAL_SETTLE_RESULT_GUILD_GONE = 3;
  GUILD_TRIAL_SETTLE_RESULT_CONFIG_MISSING = 4;// 活动行被删或类型不符,不发奖
  GUILD_TRIAL_SETTLE_RESULT_POISON = 5;        // 确定性失败(溢出 / 奖励包构建失败),不发奖,人工审计
}

message GuildTrialBattleRecord {
  option(OptionTableName) = "guild_trial_battle";
  option(OptionPrimaryKey) = "battle_id";
  option(OptionIndex) = "guild_id;state,created_ms";  // idx_0 = 解散清理;idx_1 = 巡检扫超时 STARTED
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 battle_id = 1;              // match 雪花 id
  uint64 guild_id = 2;
  uint32 activity_id = 3;
  uint32 period_key = 4;             // 开战时游戏日(个人次数)
  uint32 guild_period_key = 5;       // 开战时帮会期键
  uint64 initiator_player_id = 6;
  GuildTrialBattleState state = 7;
  GuildTrialSettleResult settle_result = 8;
  uint32 rewarded_count = 9;         // 得帮贡人数(审计)
  uint64 created_ms = 10;
  uint64 settled_ms = 11;
  // 追加区:从 12 起。
}

// 历练结算时该玩家 GUILD_CREDIT 流未决行已满(assetop.ErrTooManyPending),物品先记在这里,
// 由 OwedRewardLoop 等窗口有空位再转成 guild_asset_op 行(同事务删本行)。锁序位置 10(契约偏差 15)。
message GuildTrialRewardOwedRecord {
  option(OptionTableName) = "guild_trial_reward_owed";
  option(OptionPrimaryKey) = "player_id,battle_id";
  option(OptionIndex) = "created_ms";
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 player_id = 1;
  uint64 battle_id = 2;
  uint64 guild_id = 3;               // 仅审计;解散不删(物品属于玩家)
  uint32 activity_id = 4;
  uint32 period_key = 5;             // 转入 op 行的 period_key
  bytes payload = 6;                 // 序列化 AssetBundle
  uint32 attempts = 7;               // 因窗口仍满而跳过的次数(诊断)
  uint64 created_ms = 8;
  // 追加区:从 9 起。
}
```

**全库锁序追加**:位置 10 = `guild_trial_reward_owed`(S1 §2.1 表只有 9 行)。

**多玩家结算的同表交错**:历练结算按 player_id 升序对每人依次锁 `guild_player_op_seq`(5)→`guild_asset_op`(6),形成 `(player_id, 表位置)` 的全序,而不是严格"先锁完全部 5 再锁 6"。不会成环:单人事务只锁自己的 5→6;多人事务都按 player_id 升序;重投 worker 只锁单个 op 行(S1 规则 6)。写进 S1 规则 2 的注释(契约偏差 21)。

**开发库**:新增表 schemamigrate 自动建;若改已有表的索引,按 S1 §6.4 删库重建(未上线)。

<!-- s6_activities_part3.md -->

# S6 帮会活动 — 第 3 部分:依赖注入与时间预算、公共前置、查询视图、奖励入队、错误映射

## 6.6 依赖注入与时间预算(`go/guild/internal/logic/activity_logic.go`)

照 B5 `EnableEconomy` 的注入方式,不改 `NewGuildLogic` 签名:

```go
type ActivityDeps struct {
    Repo          *data.ActivityRepo
    Lobby         *data.TrialLobbyRepo           // B6b;B6a 为 nil
    Loop          *assetop.Loop                  // B5 注入的同一个 Loop
    OpIDs         IDMinter                       // biz_tag=guild_asset_op;nil → 带物品奖励的写 RPC 回 kGuildIdGenUnavailable
    Notifier      GuildNotifier                  // B2
    LocatorRedis  *redis.Client                  // svcCtx.PlayerLocatorRedisClient(读 battle:lock、结果记录)
    Match         matchpb.MatchInternalClient    // B6b;nil = 历练不可用
    Now           func() time.Time
    SyncBudget    time.Duration                  // 2500ms,同 B5
    HandlerBudget time.Duration                  // 3500ms,同 B5:从进入 handler 起算
    MatchBudget   time.Duration                  // 1500ms(= MatchRpc.Timeout)
}
func (l *GuildLogic) EnableActivities(d ActivityDeps)   // guild.go 在 EnableEconomy 之后调用一次
```

**预算规则**(每个写 RPC 进入时 `start := d.Now()`):

| 调用 | 预算 | 不足时 |
|---|---|---|
| 提交后同步投递 `Loop.ProcessOne` | `b := min(SyncBudget, HandlerBudget − since(start))` | `b < 300ms` → 不调,计 `guild_asset_sync_skipped_total{kind="activity"}`,行 10s 后由重投循环领取 |
| 调 match `StartActivityBattle` | `b := min(MatchBudget, HandlerBudget − since(start) − 500ms)` | `b < 300ms` → 不调,回 `kGuildTrialServiceBusy`,房间转 ENDED |

同步投递写法与 B5 完全相同:`_, _ = d.Loop.ProcessOne(withSyncDelivery(ctx, b), op)`,错误只打日志,结局以回读为准;`withSyncDelivery` 的标记让 Finalize 不再推送。单测用注入时钟覆盖两个分支(第 10 部分)。

## 6.7 公共前置 `activityPrelude`

```go
type activityActor struct {
    playerID uint64
    zoneID   uint32
    guild    *data.GuildData   // 缓存读,只用于视图与预检;写事务内以 MySQL 行为准
    member   data.MemberData
}
func (l *GuildLogic) activityPrelude(ctx context.Context, write bool) (*activityActor, *base.TipInfoMessage, error)
```

1. `c := callerOf(ctx, 0)`;`c.playerID == 0` → `status.Error(codes.PermissionDenied, "missing session")`。
2. `zone, tip, err := l.clientZone(ctx, pid)`:tip 非空直接回;err 非空 → 返回 err。
3. `gid := repo.GetPlayerGuildID`;`gid == 0` → `ErrNotInGuild`。
4. `g := repo.GetGuild`;`g == nil || !visibleIn(g, zone)` → `ErrNotInGuild`(与 `GetPlayerGuild` 同口径,`guild_logic.go:283,298`)。
5. 在 `g.Members` 找 pid,找不到 → `ErrNotInGuild`。
6. `write` 为真:B2 合服闸门(`kGuildZoneMerging`,读失败按封锁),入参 `g.ZoneID`。

**B6a 桩**:`StartGuildTrial`、`RespondGuildTrialInvite` 在 prelude(write=true)通过后直接回 `kGuildActivityNotOpen`,计 `result="not_open"`;`BuildView` 对 type 3 强制 `state=DISABLED`、`blocked_tip_id=kGuildActivityNotOpen`。B6b 删除桩,但在 `d.Match == nil || d.Lobby == nil` 时保留同样的强制。

## 6.8 查询 `GetGuildActivities`

一次请求内 `now` 只取一次。**只读路径的 Redis 读失败一律降级**(字段填 0 / 不填,打 WARN),MySQL 失败返回 err。

1. `a := activityPrelude(ctx, false)`。
2. `rows := activity.SelectVisible(allRows, nowMs)`;`day := DayKey(now)`;每行 `gpk[id] := GuildPeriodKey(row, now)`。
3. 个人计数(PK 前缀):
   `SELECT ref_id, used_count FROM guild_daily_counter WHERE player_id=? AND counter_kind=3 AND period_key=? AND ref_id IN (…)`
4. 帮会进度(每行一个 PK 点查条件,至多 3 组):
   `SELECT activity_id, period_key, progress_count, threshold_reached_ms, funds_granted FROM guild_activity_progress WHERE guild_id=? AND ((activity_id=? AND period_key=?) OR …)`
5. 团圆行 Open 且未锁存:`eligible` = `g.Members` 中 `JoinedLongEnough` 者,`BatchResolve`(宽松版)数在线人数作 `progress`。
6. 历练行(B6b):
   - `v, err := LocatorRedis.Get("battle:lock:{pid}")`:值解析为 uint64 且非 0 → `SELECT state FROM guild_trial_battle WHERE battle_id=?`;state=STARTED → `my_trial_battle_id = v`。锁不在、行不在、非 STARTED → 0。
   - 房间:`Lobby.LoadForPlayer(ctx, pid, nowMs)`(第 7 部分 6.20.4);`lobby.GuildID == gid && lobby.ActivityID == row.Id` 才填 `trial_lobby`。
7. 待发物品(两条都走 `idx_guild_asset_op_2 (player_id,stream,stream_epoch,status,seq)` —— 原文写的 `_3` 与四列列序按 90 清单 X-02 订正;查询按 part2 §3 带 `stream_epoch`):
   ```sql
   -- 7a 待发:至多 16 行
   SELECT ref_id, last_reason FROM guild_asset_op
    WHERE player_id=? AND stream=2 AND status=?P AND kind=?K ORDER BY seq;
   -- 7b 最近永久拒绝:取 4 行后在内存里按 kind=?K 且 next_attempt_ms >= now−86_400_000 过滤
   SELECT ref_id, kind, reason_tip_id, next_attempt_ms FROM guild_asset_op
    WHERE player_id=? AND stream=2 AND status=?R ORDER BY seq DESC LIMIT 4;
   -- 7c 历练待入队(B6b,PK 前缀)
   SELECT activity_id, COUNT(*) FROM guild_trial_reward_owed WHERE player_id=? GROUP BY activity_id;
   ```
   `?P/?R/?K` 绑定 `assetop.StatusPending`、`assetop.StatusRejected`、`GUILD_ASSET_OP_KIND_ACTIVITY_REWARD`,不写字面量。按 ref_id(=activity_id):`my_pending_reward_count` = 7a 行数 + 7c 计数;`my_pending_reason_tip_id` = 7a 中 seq 最小那行的 `last_reason`;`my_last_reward_reject_tip_id` = 7b 过滤后第一行的 `reason_tip_id`(Finalize 把 `next_attempt_ms` 写成终结时刻,S5 §5.6)。
8. 每行 `activity.BuildView(row, inputs)`:`threshold_reached = threshold_reached_ms != 0`;灯会与历练 `progress = progress_count`;`blocked_tip_id = BlockedTip(...)`;`reward_items = RewardItems(BuildRewardBundle(row.RewardId))`。

新增 `OnlineStatusResolver.BatchResolveStrict(ctx, ids) (map[uint64]bool, error)`:与 `BatchResolve` 相同,但 MGET 出错返回 error。**写路径(团圆领奖、历练建房与开战)必须用 Strict**;原函数不改。返回 map 只含在线者,所以 `len(map)` 就是在线人数。

## 6.9 奖励打包与入 outbox(灯会、团圆、历练结算、待入队循环共用)

`go/guild/internal/data/activity_repo.go`:

```go
type activityReward struct {
    Bundle       *assetpb.AssetBundle // nil = 无物品
    OpID         uint64               // Bundle 非 nil 时事务前由 OpIDs 取得
    Contribution uint64
}
// 事务内:AllocateSeq → B5 的 outbox 插入函数。返回 assetop.ErrTooManyPending 时**不写任何行**,调用方决定如何处理。
func (r *ActivityRepo) enqueueActivityRewardTx(ctx context.Context, tx *sql.Tx, guildID, playerID uint64,
    activityID, dayKey uint32, rw activityReward, nowMs uint64) error
```

| 列 | 值 |
|---|---|
| op_id / correlation | rw.OpID |
| player_id / guild_id | 调用参数 |
| stream | `ASSET_OP_STREAM_GUILD_CREDIT`(2) |
| seq | `assetop.AllocateSeq(ctx, tx, seqTables, playerID, GUILD_CREDIT, assetop.DefaultLimits)` |
| kind | `GUILD_ASSET_OP_KIND_ACTIVITY_REWARD` |
| status | `assetop.StatusPending` |
| tx_type | `TX_GUILD_ACTIVITY_REWARD` |
| payload | `proto.Marshal(rw.Bundle)`(失败 → 返回 `errActivityPoison` 包装的错误) |
| ref_id / period_key / ref_count | activityID / dayKey / 0 |
| contribution_delta / funds_delta | **0 / 0**(帮贡、资金已在入队事务里记完;Finalize 对 ACTIVITY_REWARD **不做对侧账**,只改状态并推 `DELIVERY_DONE`) |
| deadline_ms | **0**(永不中止) |
| lease_token / lease_until_ms / next_attempt_ms | 按 S4 §4.19:提交令牌 / now+10000 / now+10000 |

- `EnsureSeqRow(ctx, db, seqTables, playerID, GUILD_CREDIT)` 必须在**事务外**、BEGIN 前调用(autocommit)。
- `ErrTooManyPending` 的处理:**灯会、团圆**整事务回滚,回 `kGuildAssetPending`(玩家稍后重试,什么都没提交);**历练结算**把该人的物品写 `guild_trial_reward_owed`(第 8 部分),**绝不跳过**。
- 背包满:scene 回 `RETRY + kAssetBagFull`,行保持 PENDING 并写 `last_reason=27001`,腾出空间后下次重投自动到账(S4 §4.10)。**REJECTED 只出现在封禁、非法包等永久拒绝**。

## 6.10 错误映射(写 RPC 通用)

| 条件 | 返回 |
|---|---|
| 无会话身份 | gRPC `PermissionDenied` |
| 归属区未知 / 不在帮 / 帮不在本区 | 既有 `kGuildHomeZoneUnknown` / `kGuildNotInGuild` |
| 合服中或闸门不可读 | `kGuildZoneMerging` |
| `PickForWrite` 失败(id 不存在、类型不符、不是该类型当前选中行、未开放) | `kGuildActivityNotOpen` |
| 帮会等级不足 | `kGuildActivityLevelTooLow`,parameters=[min_level] |
| 入帮未满 N 小时 | `kGuildActivityJoinTooRecent`,parameters=[N] |
| 今日次数已满 | `kGuildActivityAlreadyClaimed` |
| 团圆人数不足且未锁存 | `kGuildActivityThresholdNotReached`,parameters=[online, threshold] |
| GUILD_CREDIT 未决行过多(单人 RPC) | `kGuildAssetPending` |
| 号段不可用(带物品时) | B5 的 `kGuildIdGenUnavailable` |
| 历练名单不合法 | `kGuildTrialTeamInvalid`,parameters=[reason, player_id](第 7 部分) |
| 历练建房冷却中 | `kGuildTrialInviteCooldown`,parameters=[剩余秒数] |
| 房间不存在 / 已过期 / 已结束 / 调用者不在名单 | `kGuildTrialInviteExpired` |
| match 不可用、超时、gRPC 出错、回 INTERNAL、剩余预算不足 | `kGuildTrialServiceBusy`(Tip.xlsx fault 列填 1,仍计故障指标) |
| guild 自身 MySQL / Redis 故障;死锁重试仍失败 | `return nil, err` |

`constants.go` 追加并在 `constants_test.go` 的 `tipCodes()` 登记(前批已加则跳过):`ErrZoneMerging`、`ErrAssetPending`、`ErrActivityNotOpen`、`ErrActivityAlreadyClaimed`、`ErrActivityThresholdNotReached`、`ErrTrialTeamInvalid`、`ErrActivityLevelTooLow`、`ErrActivityJoinTooRecent`、`ErrTrialInviteExpired`、`ErrTrialInviteDeclined`、`ErrTrialInviteCooldown`、`ErrTrialServiceBusy`,均写成 `uint32(table.GuildError_kGuild…)`。

**事务重试**:活动事务用 `withActivityTxRetry(ctx, fn)`:MySQL 1213 / 1205 时整事务重来,最多 2 次,间隔 20ms、60ms;`errRetryTx`(历练结算 e 步)同样重来,最多 3 次。判定 `errors.As(err, &*mysql.MySQLError)` 取 Number。B2 的 `withTxRetry` 只重试 1213 一次,活动事务不用它。

<!-- s6_activities_part4.md -->

# S6 帮会活动 — 第 4 部分:元宵灯会、中秋团圆(B6a)的事务、崩溃窗口、清理、指标

## 6.11 `LightGuildLantern`

### 6.11.1 预检(不加锁,只为快速失败;权威判定在事务内)

1. `start := d.Now()`;`a, tip, err := l.activityPrelude(ctx, true)`。
2. `row, ok := activity.PickForWrite(rows, 1, req.ActivityId, nowMs)`;`!ok` → `kGuildActivityNotOpen`。
3. `a.guild.Level < row.MinGuildLevel` → `kGuildActivityLevelTooLow`。
4. `!JoinedLongEnough(a.member.JoinTimeMs, nowMs, rule.ActivityJoinMinHours)` → `kGuildActivityJoinTooRecent`。
5. `day := DayKey(now)`;`gpk := GuildPeriodKey(row, now)`;已用次数 ≥ `row.DailyLimit` → `kGuildActivityAlreadyClaimed`。
6. `bundle, err := BuildRewardBundle(row.RewardId)`;出错 → 返回 err(启动期已校验)。
7. bundle 非空:`EnsureSeqRow`(autocommit),再 `opID := d.OpIDs.Next(ctx)`;`OpIDs == nil` → `kGuildIdGenUnavailable`;其它失败 → 返回 err。

### 6.11.2 事务 `repo.LightLanternTx(ctx, in LanternTxInput) (LanternTxResult, error)`

锁序:guild → guild_member → guild_player_op_seq → guild_asset_op → guild_daily_counter → guild_activity_progress。

```sql
-- a. 帮会行(之后对 guild 的 UPDATE 不再新增锁)
SELECT level, zone_id FROM guild WHERE guild_id=? FOR UPDATE;
--    无行 → kGuildNotInGuild;level < min_guild_level → kGuildActivityLevelTooLow

-- b. 成员行
SELECT join_time_ms FROM guild_member WHERE guild_id=? AND player_id=? FOR UPDATE;
--    无行 → kGuildNotInGuild;join_time_ms 复核 → kGuildActivityJoinTooRecent

-- c. bundle 非空:enqueueActivityRewardTx(AllocateSeq + 插 op 行)
--    ErrTooManyPending → 回滚,kGuildAssetPending

-- d. 每日计数(带上限的 upsert;赋值从左到右求值,updated_ms 必须写在前面)
INSERT INTO guild_daily_counter (player_id, counter_kind, ref_id, period_key, used_count, updated_ms)
VALUES (?, 3, ?, ?, 1, ?)
ON DUPLICATE KEY UPDATE
  updated_ms = IF(used_count < ?, ?, updated_ms),
  used_count = IF(used_count < ?, used_count + 1, used_count);
--    参数:pid, activity_id, day, now, daily_limit, now, daily_limit
--    RowsAffected:1 新插入;2 已累加;0 已达上限 → 回滚,kGuildActivityAlreadyClaimed

-- e. 帮会进度(period_key = gpk)
INSERT INTO guild_activity_progress (guild_id, activity_id, period_key, progress_count, threshold_reached_ms, funds_granted, updated_ms)
VALUES (?, ?, ?, 1, 0, 0, ?)
ON DUPLICATE KEY UPDATE progress_count = progress_count + 1, updated_ms = ?;
SELECT progress_count, threshold_reached_ms, funds_granted
  FROM guild_activity_progress WHERE guild_id=? AND activity_id=? AND period_key=? FOR UPDATE;

-- f. 阈值与资金(本档期各只发生一次)
--    reachedNow = progress_count >= guild_threshold AND threshold_reached_ms = 0
UPDATE guild_activity_progress SET threshold_reached_ms=? WHERE guild_id=? AND activity_id=? AND period_key=?;  -- 仅 reachedNow
--    grantFunds = progress_count >= guild_threshold AND funds_granted = 0 AND guild_funds > 0
UPDATE guild SET funds = funds + ? WHERE guild_id=? AND funds <= ?;   -- 第 3 参数 = MaxUint64 − guild_funds;RowsAffected 0 → 溢出
UPDATE guild_activity_progress SET funds_granted=1 WHERE guild_id=? AND activity_id=? AND period_key=?;

-- g. 个人帮贡(personal_contribution > 0)
UPDATE guild_member
   SET contribution_total = contribution_total + ?, contribution_balance = contribution_balance + ?
 WHERE guild_id=? AND player_id=? AND contribution_total <= ? AND contribution_balance <= ?;
--    后两参数 = MaxUint64 − delta;RowsAffected 0 → 溢出
COMMIT;
```

- 上限在 Go 里用 `math.MaxUint64 - delta` 算好再绑定,避免 MySQL unsigned 减法 out of range。溢出返回 `errActivityPoison`(单人 RPC 下就是 err → gRPC 故障;实际不可达)。
- DSN **不得**开 `clientFoundRows=true`(否则 d 步"值未变"会回 1)。`config.Validate` 断言 DSN query 不含它。
- `LanternTxResult{OpID uint64; Enqueued, ReachedNow, FundsGranted bool}`。
- 1213/1205 整事务重试(6.10),**沿用同一个 opID**:上一次已回滚,op 行不存在。

### 6.11.3 提交后

1. `repo.invalidateGuildCache(ctx, gid)`;失败只打日志,由缓存代次 Lua 兜底。
2. `Enqueued` → 按 6.6 预算同步 `ProcessOne`。
3. `ReachedNow` → `l.notify(ACTIVITY_CHANGED, gid, pid, 0, membersExcept(a.guild, pid))`。**不达阈值不推**(每盏灯都推会放大 N×M)。
4. 按 6.8 重建本行视图并返回(只对本行执行 3、4、7 步)。
5. 指标:`guild_activity_action_total{type="lantern",action="light",result}`;`FundsGranted` → `guild_activity_funds_granted_total{type="lantern"}`。

## 6.12 `ClaimGuildReunion`

### 6.12.1 预检

1–7 同 6.11.1,类型为 2。之后:
8. `threshold := ReunionThreshold(row, rule)`。
9. 读本档期进度行;`latched := threshold_reached_ms != 0`。
10. `!latched`:`eligible` = `a.guild.Members` 中 `JoinedLongEnough` 的 id;`onl, err := BatchResolveStrict(ctx, eligible)`,err → 返回 err;`online := len(onl)`;`observed := online >= threshold`;`!observed` → `kGuildActivityThresholdNotReached`,parameters=[online, threshold]。
11. 缓存成员表最多落后一个代次;人数只用来开门,事务内不重数(C11)。

### 6.12.2 事务 `repo.ClaimReunionTx`

a、b、c、d 同灯会。e、f 改为:

```sql
-- e. 进度行保底存在,再加锁读(period_key = gpk)
INSERT INTO guild_activity_progress (guild_id, activity_id, period_key, progress_count, threshold_reached_ms, funds_granted, updated_ms)
VALUES (?, ?, ?, 0, 0, 0, ?) ON DUPLICATE KEY UPDATE updated_ms = ?;
SELECT threshold_reached_ms, funds_granted FROM guild_activity_progress
 WHERE guild_id=? AND activity_id=? AND period_key=? FOR UPDATE;
--    threshold_reached_ms = 0:observed → UPDATE … SET threshold_reached_ms=?,reachedNow=true;
--                             !observed → 回滚,kGuildActivityThresholdNotReached
-- f. guild_funds > 0 且 funds_granted = 0 → 与灯会 f 步相同的两条 UPDATE
```

g 同灯会。提交后同 6.11.3;指标 `type="reunion",action="claim"`。

**防刷要点**(写进 guild-phase2.md):在线人数只数入帮满 N 小时的成员;计数挂在玩家上,退帮换帮当天仍算已领;锁存只在有人成功领取时写;团圆默认不发资金。

## 6.13 崩溃与并发窗口(B6a)

| # | 场景 | 结果 |
|---|---|---|
| C1 | 提交前崩溃或出错 | 计数、进度、帮贡、资金、op 行、seq 一起回滚;可重试 ✓ |
| C2 | 提交后、`ProcessOne` 前崩溃 | op 行 PENDING,租约 10s 后重投循环投递 ✓ |
| C3 | 提交后同步投递耗时过长 | 预算 `min(2500, 3500−已用)`,不足 300ms 不投;RPC 必在 guild Timeout 4000 内返回,客户端不会因超时进入 `RequiresReconnect` ✓ |
| C4 | 响应仍丢失(网络) | 客户端重连后拉视图,`my_used_count=1`;再点回 AlreadyClaimed ✓ |
| C5 | 投递时玩家下线或换节点 | NOT_HERE → 退避重投,上线后到账;`deadline_ms=0` 不中止 ✓ |
| C6 | 背包满 | scene 回 RETRY,行保持 PENDING、`last_reason=27001`;视图"背包已满,腾出空间后自动发放";腾出后自动到账 ✓ |
| C7 | 永久拒绝(物品或货币被封禁、非法包) | REJECTED,不补发、帮贡不回收;视图显示原因。v1 接受(契约偏差 5) |
| C8 | 推送丢失 | 切到活动页或点刷新时拉取 ✓ |
| C9 | 同帮两人在两个实例上同时点灯 | guild 行锁串行;`funds_granted` 同锁下判定,资金只发一次 ✓ |
| C10 | 同一玩家并发两次 | 第二个事务等 guild 行锁;拿到后 d 步 RowsAffected=0 → 回滚(含 seq)→ AlreadyClaimed ✓ |
| C11 | 团圆:数人数后、提交前有人下线 | 以数人数时刻为准,锁存;语义"曾经同时在线" ✓ |
| C12 | 跨 05:00 | 个人次数换新一天;帮会进度按档期不变(常开行按天) ✓ |
| C13 | 预检后被踢或帮会解散 | 事务内成员行或帮会行不存在 → NotInGuild,回滚 ✓ |
| C14 | 数据服务回档玩家 | 帮贡与计数不回滚(同 S4 C10:接受 + 审计);已 APPLIED 物品被回档抹掉的不补发 |
| C15 | 客户端伪造同类型另一行的 activity_id | `PickForWrite` 只接受该类型当前选中行 → NotOpen;配表又禁止同类型启用行重叠 ✓ |

## 6.14 解散与清理

- `guild_repo.go` 的 `deleteGuildFromMySQL`(`:529` 起)在删成员行之后、提交之前追加 `DELETE FROM guild_activity_progress WHERE guild_id=?`(B6a);B6b 再追加 `DELETE FROM guild_trial_battle WHERE guild_id=?`(走 idx_0)。顺序符合锁序。
- `guild_daily_counter` **不删**(按玩家计,删了可"解散重建再领")。
- ACTIVITY_REWARD 的 op 行、`guild_trial_reward_owed` 行**不动**:物品属于玩家,解散后照常投递。
- 旧进度与历练行 v1 不清理(每帮每天 ≤3 行进度、≤ 胜场数行历练);v1.1 按 `period_key` 清 90 天前的行。op 行清理归 B5 §5.22。
- merge_zone 不需要新步骤:这些表不带 zone 列,zone 跟 `guild` 行走。

## 6.15 指标(`go/guild/internal/logic/activity_metrics.go`,promauto,**不带 player_id / guild_id 标签**)

| 指标 | 标签 |
|---|---|
| `guild_activity_action_total` | `type`(lantern/reunion/trial)、`action`(light/claim/invite/respond/launch/settle)、`result`(ok/not_open/level_low/join_recent/claimed/threshold/asset_pending/team_invalid/cooldown/expired/declined/service_busy/merging/not_in_guild/error) |
| `guild_activity_reward_total` | `type`、`result`(enqueued/no_items/owed) |
| `guild_activity_funds_granted_total` | `type` |
| `guild_activity_tx_retry_total` | `type` |
| `guild_asset_sync_skipped_total{kind="activity"}` | 复用 B5 指标,新增 kind 值 |

<!-- s6_activities_part5.md -->

# S6 帮会活动 — 第 5 部分:同道历练(B6b)的跨服务协议与 match 侧

## 6.16 为什么新增内部 RPC,而不复用组队的 `StartTeamMatch`

| 需求 | `StartTeamMatch`(team-system.md §E.1) | 本方案 `MatchInternal.StartActivityBattle` |
|---|---|---|
| 名单来源 | Redis 队伍记录(队长发起) | guild 邀请房间里**全员已同意**的同帮成员 |
| 同步拿到 battle_id | 否(异步 gather 才生成) | 是:先生成并同步返回,guild 才能登记 `guild_trial_battle` |
| 携带活动上下文 | 否 | 是:`BattleActivityContext` 透传到结果事件 |
| 落码状态 | 只有设计 | 本批落码 |

两者共用 matched 票、`runGather` 管线与补偿矩阵。组队落码后若要"整队打历练":客户端仍调 `GuildService.StartGuildTrial`,名单取当前队伍,邀请/同意流程不变(队员已在组队时同意过的,v1.1 可免确认)。**不要**让客户端或组队服务直接调 `MatchInternal`。

## 6.17 协议

### 6.17.1 `proto/battle/battle_data.proto` 文件末尾追加(该文件有他人未提交修改,只追加)

```proto
// ---- 活动对局上下文:match 透传给 battle,battle 原样回显进 BattleResultEvent;battle/scene 不解释 ----
// 设计:docs/design/guild-phase2.md §6(帮会同道历练)。
enum eBattleActivityKind {
  BATTLE_ACTIVITY_KIND_NONE = 0;
  BATTLE_ACTIVITY_KIND_GUILD_TRIAL = 1;   // go/guild 消费 match-results 结算
}

message BattleActivityContext {
  eBattleActivityKind kind = 1;
  uint64 guild_id = 2;
  uint32 activity_id = 3;           // GuildActivity.id
  uint32 period_key = 4;            // 确认开战时的游戏日 YYYYMMDD(个人次数)
  uint64 initiator_player_id = 5;
  uint32 guild_period_key = 6;      // 确认开战时的帮会期键(每日计资金胜场上限)
}
```

### 6.17.2 `proto/battle/battle_node.proto` 的 `CreateBattleRequest` 追加

```proto
  BattleActivityContext activity_context = 9;  // 可空;kind != NONE 时 battle 回显并走持久化结果通道(6.19)
```

### 6.17.3 `proto/contracts/kafka/match_event.proto` 的 `BattleResultEvent` 追加

```proto
  BattleActivityContext activity_context = 9;   // 开局时的活动上下文;普通对局不填
  repeated uint64 fled_player_ids = 10;         // 结算时 fled=true 的玩家,升序去重
  repeated uint64 dead_player_ids = 11;         // 结算时 is_dead=true 的玩家,升序去重(U2)
```

文件头注释"battle -> match"补成"battle -> match(评分)/ guild(同道历练结算,消费组 guild-trial;带上下文的事件另有 SharedRedis 持久记录,见 guild-phase2 §6)"。

### 6.17.4 新建 `proto/match/match_internal.proto`

```proto
syntax = "proto3";
package match;
option go_package = "match/match";

import "proto/db/proto_option.proto";
import "proto/battle/battle_data.proto";

option cc_generic_services = false;
option (OptionFileDefaultNode) = NODE_MATCH;

// match 内部服务(docs/design/guild-phase2.md §6 同道历练),照 proto/trade/trade_admin.proto 的隔离做法:
//   - 刻意不标 OptionIsClientProtocolService,与 match_service.proto 分文件:gate IsClientMessageId 不收,
//     路由服对 ClientProtocol=false 回信封拒绝;
//   - match 会话拦截器对"带会话 metadata 调 MatchInternal.*"回 PermissionDenied;
//   - 部署层只放行 guild 访问 match gRPC 端口(见契约偏差 13);
//   - 客户端 tools/gen_proto.ps1 不列本文件。

enum ActivityBattleReject {
  ACTIVITY_BATTLE_REJECT_NONE = 0;
  ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT = 1;   // 人数越界 / 重复或 0 id / 上下文缺失 / 发起人不在首位
  ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE = 2;
  ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE = 3;
  ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY = 4;   // 无场景位置 / 有在途票据 / 建票失败
  ACTIVITY_BATTLE_REJECT_INTERNAL = 5;           // 发号失败 / Redis 故障
}

message StartActivityBattleRequest {
  uint32 battle_config_id = 1;                 // DungeonTable id
  repeated uint64 member_player_ids = 2;       // 站位顺序,发起人在首位;1..5 人、不重复
  BattleActivityContext activity_context = 3;  // kind 必须非 NONE
}

message StartActivityBattleResponse {
  ActivityBattleReject reject = 1;
  uint64 offender_player_id = 2;   // reject 为 2/3/4 时非 0
  uint64 battle_id = 3;            // reject == NONE 时非 0;此时 gather 已异步启动
}

service MatchInternal {
  rpc StartActivityBattle(StartActivityBattleRequest) returns (StartActivityBattleResponse);
}
```

- proto-gen 的 match 域源目录是整个 `proto/match/`(`proto_gen.yaml:485-496`),新文件自动编译并分配 message id(TradeAdmin 先例 id 199)。
- 生成物登记:proto-gen 后 `git diff` 看 `cpp/generated/proto/{proto.vcxproj,.filters,CMakeLists.txt}`、`cpp/generated/grpc_client/{grpc_client.vcxproj,.filters,CMakeLists.txt}`、`cpp/generated/rpc/rpc.vcxproj` 是否出现 `match_internal`;缺哪处照同文件 `trade_admin` 行手补。顺带核对 `grpc_client/CMakeLists.txt` 缺 `match_service_grpc_client` 是否既有缺口(Linux 链接时确认)。

## 6.18 match 侧实现

### 6.18.1 `go/match/internal/logic/gather.go`(只加包装,`runGather` 签名与行为不变)

```go
// gatherOptions:活动开局的附加参数。零值 = 原有行为。
type gatherOptions struct {
    presetBattleID  uint64                          // 非 0:调用方已生成 battle_id
    activityContext *battlepb.BattleActivityContext // 非空:原样放进 CreateBattleRequest.activity_context
}

func runGather(svcCtx *svc.ServiceContext, mode matchpb.MatchMode, battleConfigId uint32,
    members []uint64, requeueOnFail bool, withTickets bool, tickets map[uint64]string) bool {
    return runGatherWithOptions(svcCtx, mode, battleConfigId, members, requeueOnFail, withTickets, tickets, gatherOptions{})
}

// runGatherWithOptions = 原 runGather 函数体,只改两处:
//   ① 第 1 步:battleId := opts.presetBattleID;为 0 才调 svcCtx.BattleIDGen.Generate()(失败分支不变)
//   ② 第 4 步 CreateBattleRequest 追加 ActivityContext: opts.activityContext
func runGatherWithOptions(..., opts gatherOptions) bool

// RunActivityGather:活动开局,与 RunChallengeGather 同级;失败时全员删票、不回队列。
func RunActivityGather(svcCtx *svc.ServiceContext, battleConfigId uint32, members []uint64,
    tickets map[uint64]string, battleID uint64, actx *battlepb.BattleActivityContext) bool {
    return runGatherWithOptions(svcCtx, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, battleConfigId, members,
        false, true, tickets, gatherOptions{presetBattleID: battleID, activityContext: actx})
}
```

### 6.18.2 新建 `go/match/internal/logic/activitybattlelogic.go`

```go
var runActivityGatherFn = RunActivityGather // 测试缝,照 matcher.go 的 runGatherFn
type StartActivityBattleLogic struct { ctx context.Context; svcCtx *svc.ServiceContext; logx.Logger }
func NewStartActivityBattleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StartActivityBattleLogic
func (l *StartActivityBattleLogic) StartActivityBattle(in *matchpb.StartActivityBattleRequest) (*matchpb.StartActivityBattleResponse, error)
```

1. **防御**:ctx 里有会话详情 → `status.Error(codes.PermissionDenied, …)`。
2. **参数校验**,任一不满足 → `INVALID_ARGUMENT`:`1 ≤ len(members) ≤ kMaxBattleTeamSize`,id 非 0 且不重复;`battle_config_id != 0`;`actx != nil`、`kind != NONE`、`guild_id`/`activity_id`/`period_key`/`guild_period_key` 非 0;`actx.initiator_player_id == members[0]`。
3. **逐成员只读预检**(按名单顺序,第一个失败即返回,offender=该 pid):
   - `loadPlayerSession`:出错 → INTERNAL;`!isSessionOnline` → MEMBER_OFFLINE;
   - `isPlayerBattleLocked`:出错 → INTERNAL;真 → MEMBER_IN_BATTLE;
   - `loadPlayerLocation`:出错 → INTERNAL;nil → MEMBER_NOT_READY;记 `loc.ZoneId`;
   - `loadTicket`:出错 → INTERNAL;非 nil 时 `NewJoinQueueLogic(l.ctx, l.svcCtx).healOrphanQueuedTicket(pid, t)`,出错 → INTERNAL,未自愈 → MEMBER_NOT_READY。
4. `battleID, err := l.svcCtx.BattleIDGen.Generate()`;出错 → INTERNAL,不得用 0 顶替。
5. **逐人建 matched 票**:`queueTicket{Ticket: uuid.New().String(), Mode: int32(MATCH_MODE_PVE_TEAM), Config: cfg, EnqueuedAtMs: nowMs(), ZoneId: zone, State: ticketStateMatched}`(QueueKey 留空,同 PVE_SOLO);`createTicketIfAbsent(svcCtx, pid, t, matchedTicketTTLFor(svcCtx, uint32(n)))`。返回 false 或出错进入回滚:回滚集合 = 返回 true 的成员 ∪ 返回 err 的成员,对每人按本次 ticket id `deleteTicketIfOwned`(`context.Background()` 语义);之后 false → MEMBER_NOT_READY,err → INTERNAL。
6. `actxCopy := proto.Clone(actx)`;`safego.Go("match.gather.activity", func(){ ok := runActivityGatherFn(svcCtx, cfg, members, tickets, battleID, actxCopy); metrics.ObserveActivityBattle(kindName, gatherResult(ok)) })`。
7. 返回 `{battle_id: battleID}`,计 `ObserveActivityBattle(kindName, "started")`。

- **时间预算**:每人 4 次 Redis 读 + 1 次 Lua 写,5 人约 25 次往返,本地 <50ms;guild 侧预算见第 3 部分 6.6。
- **崩溃窗口**:第 5 步中途崩溃,已建票按 matched TTL 自愈,无人被冻结;第 6 步之后崩溃按 gather 补偿矩阵(`gather.go:147-154`)。guild 拿不到响应 → 房间 ENDED(ServiceBusy),玩家重试时碰到在途票得 NOT_READY,TTL 过后恢复。

### 6.18.3 注册、拦截与鉴权

- 新建 `go/match/internal/server/matchinternalserver.go`:`type MatchInternalServer struct{ svcCtx *svc.ServiceContext; matchpb.UnimplementedMatchInternalServer }`,唯一方法委托 `logic.NewStartActivityBattleLogic(ctx, s.svcCtx).StartActivityBattle(in)`。
- `go/match/match_service.go:102` 注册闭包里 `RegisterMatchServiceServer` 之后加 `matchpb.RegisterMatchInternalServer(grpcServer, server.NewMatchInternalServer(svcCtx))`。
- `sessionInterceptor`(`:169`)开头加:`strings.HasPrefix(info.FullMethod, "/"+matchpb.MatchInternal_ServiceDesc.ServiceName+"/")` 且 metadata 带 `x-session-detail-bin` → `status.Error(codes.PermissionDenied, "internal method")`。
- **鉴权边界(v1)**:match 现有所有 gRPC 方法对内网调用方都不验签(请求体 player_id 直接信任),`match-results` topic 也可被内网任意生产者写入,MatchInternal 与之处于同一信任边界。v1 在部署层收口:K8s NetworkPolicy 只允许 gate 路由服与 guild 的 Pod 访问 match gRPC 端口(guild 目前无 manifest,随 D-14 遗留项一起补);v1.1 把 `login/internal/logic/pkg/callerauth` 提到 `go/shared/callerauth`,MatchInternal 要求 `x-caller-id=guild` 签名。写进契约偏差 13。
- `go/match/internal/metrics/metrics.go` 追加 `match_activity_battle_total{kind,result}`,result ∈ started/invalid/offline/in_battle/not_ready/internal/gather_ok/gather_failed;`func ObserveActivityBattle(kind, result string)`。

<!-- s6_activities_part6.md -->

# S6 帮会活动 — 第 6 部分:battle 节点(C++)回显与活动结果持久化通道

## 6.19 为什么要持久化

现状 `SendBattleResultEvent`(`battle_room_manager.cpp:287-296`)发送失败只打日志;battle 进程在 librdkafka 缓冲 flush 前崩溃,事件同样丢失。普通对局丢一条只影响评分;历练丢一条就是**一整局的帮贡、资金、物品永久丢失**,`guild_trial_battle` 永远停在 STARTED。个人结算已有 R07 发件箱(先落 Redis、后投递、未销账重投,`:1406-1600`),活动结果照同一纪律做,但销账方是 guild。

**通道契约**(跨语言,C++ 头文件与 Go 常量各写一份并互相引用注释):

| 项 | 值 |
|---|---|
| 键 | `battle:activity_result:{battle_id}`(十进制) |
| 值 | `BattleResultEvent` 序列化字节(含 `activity_context`) |
| 存储 | battle 的 `tlsRedis.GetZoneRedis()`,即 R07 `battle:settlement:pending:*` 所在实例;注释称 SharedRedis。**必须**与 guild 的 `PlayerLocatorRedis` 为同一实例、同一 DB(Codex 验证第 B6b-srv1 步 0 核对,不同则停下) |
| TTL | 604800 秒(7 天,与 R07 `kPendingSettlementTtlSec`、Kafka 保留期一致) |
| 写入方 | battle,只在 `activity_context.kind != NONE` 时写;写成功后才发 Kafka |
| 销账方 | guild:该 battle 进入任何终态(SETTLED 各结果、bad_context 含 battle_id)后 `DEL` |
| 重发 | battle 每 10s `EXISTS`,仍在则按原 payload 重发 Kafka,最多 30 次;用尽后留记录给 guild 巡检器 |

## 6.20 C++ 改动

### 6.20.1 新建 header-only `cpp/libs/services/battle/system/battle_result_activity.h`

```cpp
#pragma once
#include <algorithm>
#include <cstdint>
#include <vector>
#include "proto/battle/battle_data.pb.h"
#include "proto/contracts/kafka/match_event.pb.h"

namespace turnbattle
{
// 活动结果持久记录键(与 go/guild/internal/data/trial_result_record.go 的 activityResultKey 一致)。
inline constexpr char kActivityResultKeyFmt[] = "battle:activity_result:%llu";
inline constexpr uint32_t kActivityResultTtlSec = 604800;
inline constexpr uint32_t kActivityResultRetryIntervalSec = 10;
inline constexpr uint32_t kActivityResultRetryMaxAttempts = 30;

inline void SortUnique(std::vector<uint64_t> &ids)
{
    std::sort(ids.begin(), ids.end());
    ids.erase(std::unique(ids.begin(), ids.end()), ids.end());
}

// 把开局时的活动上下文、逃跑与阵亡名单写进对局结果(docs/design/guild-phase2.md §6 同道历练)。
// kind==NONE:不写 activity_context;两份名单总是写,升序去重。
inline void FillBattleResultActivityFields(const ::BattleActivityContext &activity,
                                           std::vector<uint64_t> fledPlayerIds,
                                           std::vector<uint64_t> deadPlayerIds,
                                           contracts::kafka::BattleResultEvent &out)
{
    if (activity.kind() != ::BATTLE_ACTIVITY_KIND_NONE)
    {
        *out.mutable_activity_context() = activity;
    }
    SortUnique(fledPlayerIds);
    SortUnique(deadPlayerIds);
    for (const auto id : fledPlayerIds) { out.add_fled_player_ids(id); }
    for (const auto id : deadPlayerIds) { out.add_dead_player_ids(id); }
}

enum class ActivityResultRetryAction { kDone, kResend, kExhausted };

// 判定顺序不能换:已销账 > 次数用尽 > 重发。
inline ActivityResultRetryAction ClassifyActivityResultRetry(bool recordExists, uint32_t attemptsSoFar,
                                                             uint32_t maxAttempts)
{
    if (!recordExists) { return ActivityResultRetryAction::kDone; }
    if (attemptsSoFar >= maxAttempts) { return ActivityResultRetryAction::kExhausted; }
    return ActivityResultRetryAction::kResend;
}
} // namespace turnbattle
```

### 6.20.2 `cpp/nodes/battle/logic/battle_room_manager.h`

1. `BattleRoom`(`:156-158` 开局参数副本处)追加 `::BattleActivityContext activityContext;`。
2. `BattleRoomManager` 私有区追加:
   ```cpp
   struct PendingActivityResult { std::string payload; uint32_t attempts = 0; };
   std::unordered_map<uint64_t, PendingActivityResult> activityResultOutbox_;   // key = battle_id
   // 定时器成员与 settlementRetryTimer_ 同类型
   void DispatchActivityResultDurably(const contracts::kafka::BattleResultEvent &result);
   void RetryPendingActivityResults();
   void StopActivityResultRetryTimerIfIdle();
   ```
   (以及同类型定时器成员 `activityResultRetryTimer_`。)

### 6.20.3 `cpp/nodes/battle/logic/battle_room_manager.cpp`

1. `:518-519` 之后:`room->activityContext = request.activity_context();`。
2. `FinishBattle`(`:1029` 起):
   - 循环外声明 `std::vector<uint64_t> fledPlayerIds, deadPlayerIds;`;
   - 循环里 `playersByTeam[...]` 一行之后:`if (settlement.fled()) fledPlayerIds.push_back(playerId); if (settlement.is_dead()) deadPlayerIds.push_back(playerId);`;
   - `result.set_finished_at_ms(...)`(`:1087`)之后:
     ```cpp
     turnbattle::FillBattleResultActivityFields(room.activityContext, std::move(fledPlayerIds),
                                                std::move(deadPlayerIds), result);
     if (result.has_activity_context()) { DispatchActivityResultDurably(result); }
     else { SendBattleResultEvent(result); }
     ```
3. `DispatchActivityResultDurably`:
   - `SerializeToString` 失败 → `LOG_ERROR metric=battle_activity_result_serialize_failed`,return(不可恢复,guild 巡检器会把该局判 EXPIRED 并告警)。
   - `!RedisReady()` → `LOG_ERROR metric=battle_activity_result_not_durable battle_id=…`,`SendBattleResultEvent(result)` 一次,return。
   - 否则 `tlsRedis.GetZoneRedis()->command(cb, "SET battle:activity_result:%llu %b EX %u", battleId, payload.data(), payload.size(), turnbattle::kActivityResultTtlSec)`;回调:回复为空或 `REDIS_REPLY_ERROR` → 同上 not_durable 日志并发一次;成功 → `SendBattleResultEvent(result)`,`activityResultOutbox_[battleId] = {payload, 0}`,定时器未启动则 `RunEvery(kActivityResultRetryIntervalSec, [this]{ RetryPendingActivityResults(); })`。
4. `RetryPendingActivityResults`:`RedisReady()` 为假本轮跳过(WARN);先抄出 key 列表,再逐个 `EXISTS battle:activity_result:%llu`,回调里重新 find 条目(可能已被摘):
   - `recordExists = reply && reply->type == REDIS_REPLY_INTEGER && reply->integer == 1`;回复出错按"存在"处理(宁可多发);
   - `kDone` → erase,`LOG_INFO` 已销账;`kResend` → `++attempts`,按原 payload `KafkaProducer::Instance().send(kMatchResultsTopic, payload, std::to_string(battleId))`,`LOG_WARN metric=battle_activity_result_resend`;`kExhausted` → `LOG_ERROR metric=battle_activity_result_undelivered`,erase(记录留在 Redis,由 guild 巡检器兜底);
   - 每次 erase 后 `StopActivityResultRetryTimerIfIdle()`。
5. `DestroyBattle` / `AbortAllRooms` 不产生结果,不写记录(对局作废,guild 巡检器判 EXPIRED)。

scene 不需要任何改动:活动奖励不走 scene 结算,scene 照常发 PVE 金币与击杀进度。

## 6.21 崩溃窗口(battle 侧)

| # | 场景 | 结果 |
|---|---|---|
| R1 | 战斗进行中 battle 崩溃 | 无结果、无记录;scene 按 deadline 解冻;guild 巡检器 3600s 后判 EXPIRED(没有奖励,也没有人得到本局个人奖励,口径一致) ✓ |
| R2 | 写记录前崩溃(FinishBattle 与 SET 回调之间) | 同 R1。窗口是一次 Redis 往返 ✓(接受) |
| R3 | 写记录后、Kafka flush 前崩溃 | 记录在;guild 巡检器 420s 后从记录直接结算 ✓ |
| R4 | Kafka 投递失败 | 10s 后重发,最多 30 次;之后巡检器兜底 ✓ |
| R5 | guild 已结算但 DEL 失败 | battle 继续重发 → guild h 步 duplicate → 再 DEL;最坏 TTL 7 天自然过期 ✓ |
| R6 | Redis 不可用 | 降级为只发一次(与改造前一致),打 `battle_activity_result_not_durable`,须告警 |

<!-- s6_activities_part7.md -->

# S6 帮会活动 — 第 7 部分:同道历练(B6b)guild 侧配置、邀请房间、确认开战

## 6.22 配置(`go/guild/internal/config/config.go` + `etc/guild.yaml`)

```go
// Config 追加
MatchRpc zrpc.RpcClientConf `json:"MatchRpc,optional"` // 没配 → 历练视为未开放,启动打一行 Info
Activity ActivityConf       `json:"Activity,optional"`

type ActivityConf struct {
    TrialResultOverdueSeconds uint32          `json:",default=420"`  // match BattleMaxDurationSeconds 300 + 120:超过仍 STARTED 即巡检
    TrialAbandonSeconds       uint32          `json:",default=3600"` // 超过且无持久记录 → EXPIRED
    TrialSweepIntervalSeconds uint32          `json:",default=60"`
    OwedLoopIntervalSeconds   uint32          `json:",default=10"`
    TrialResult               TrialResultConf `json:",optional"`
}
type TrialResultConf struct {
    Enabled    bool   `json:",default=true"`
    Topic      string `json:",default=match-results"`
    Partitions int32  `json:",default=3"`          // 必须与 match ResultTopicPartitions 相同
    GroupID    string `json:",default=guild-trial"`
}
// TrialResultActive:Enabled 且 Kafka.Brokers 非空。为假时历练视为未开放(没有结算就不许开战)。
func (c Config) TrialResultActive() bool
```

`Validate` 追加:配了 `MatchRpc.Etcd.Key` 时 `0 < MatchRpc.Timeout ≤ 2000`;`TrialResultOverdueSeconds ≥ 60`;`TrialAbandonSeconds ≥ TrialResultOverdueSeconds + 600`(battle 重发窗口 300s 之外再留余量);`10 ≤ TrialSweepIntervalSeconds ≤ 600`;`1 ≤ OwedLoopIntervalSeconds ≤ 60`;`Enabled` 时 `Topic`、`GroupID` 非空、`Partitions ≥ 1`。**Brokers 为空不拒绝启动**:`guild.go` 在 `Enabled && !TrialResultActive()` 时打 WARN"历练结果消费未启用,同道历练关闭"。

yaml 追加:

```yaml
# 同道历练开战:调 match 内部服务 MatchInternal.StartActivityBattle(docs/design/guild-phase2.md §6)。
# 本地 go_services.ps1 -Zone N 会把 Key 改成 matchservice.rpc.zN,guild 只连该 zone 的 match;匹配池全局共享,不影响功能。
# NonBlock:match 没起时 guild 照常起服,开战回 GuildTrialServiceBusy。
MatchRpc:
  Etcd:
    Hosts:
      - 127.0.0.1:2379
    Key: matchservice.rpc
  Timeout: 1500
  NonBlock: true
  Middlewares:
    Breaker: false

Activity:
  TrialResultOverdueSeconds: 420
  TrialAbandonSeconds: 3600
  TrialSweepIntervalSeconds: 60
  OwedLoopIntervalSeconds: 10
  TrialResult:
    Enabled: true
    Topic: match-results
    Partitions: 3
    # 刻意不加引号:go_services.ps1 -Zone 只给带引号的 GroupID 加 _zN(go_services.ps1:381-388)。
    # guild 是全局服务、match-results 是全局 topic,本地多个 guild 进程必须同组分摊分区,而不是各组各跑一遍结算。
    GroupID: guild-trial
```

`svc.ServiceContext` 追加 `MatchInternal matchpb.MatchInternalClient`:`MatchRpc.Etcd.Key` 非空时 `zrpc.MustNewClient(c.MatchRpc)` → `matchpb.NewMatchInternalClient(cli.Conn())`,否则 nil。`guild.go` 组装 `ActivityDeps` 时:`Match = svcCtx.MatchInternal`(`!TrialResultActive()` 时置 nil)、`Lobby = data.NewTrialLobbyRepo(svcCtx.RedisClient)`。

## 6.23 邀请房间 `go/guild/internal/data/trial_lobby_repo.go`

guild 全局 Redis(`RedisClient`,单节点 `*redis.Client`,`servicecontext.go:24`)。Lua 内按 id 拼接其它房间键只在单节点成立;迁 Redis Cluster 时须改两段式(写进文件头注释)。

| 键 | 类型 | TTL | 内容 |
|---|---|---|---|
| `guild:trial:lobby:seq` | STRING | 无 | INCR 发 lobby_id |
| `guild:trial:lobby:{lobby_id}` | HASH | ttl+60s | `gid` `aid` `init` `roster`(逗号分隔,发起人首位)`acc`(位图,bit i ↔ roster[i])`state`(1..4,数值同 `GuildTrialLobbyState`)`exp` `ldl`(开战截止)`bid` `etip` `epar`(`\x1f` 分隔)`created` |
| `guild:trial:lobby:of:{player_id}` | STRING | ttl+60s | 该玩家最近所在房间 id |
| `guild:trial:cooldown:{initiator}` | STRING | cooldown | 建房冷却 |

```go
type TrialLobby struct {
    LobbyID, GuildID, InitiatorID, ExpireAtMs, LaunchDeadlineMs, BattleID, CreatedMs uint64
    ActivityID, State, EndTipID uint32
    Roster []uint64; AcceptedMask uint32; EndParams []string
}
func (r *TrialLobbyRepo) Create(ctx context.Context, lb TrialLobby, ttl, cooldown time.Duration, nowMs uint64) (code int, arg int64, err error)
func (r *TrialLobbyRepo) Load(ctx context.Context, lobbyID uint64) (*TrialLobby, error)          // 原样,不折算
func (r *TrialLobbyRepo) LoadForPlayer(ctx context.Context, pid, nowMs uint64) (*TrialLobby, error) // 折算有效状态
func (r *TrialLobbyRepo) Respond(ctx context.Context, lobbyID, pid uint64, accept bool, nowMs, launchWindowMs uint64, declineTip uint32) (code int, arg int64, err error)
func (r *TrialLobbyRepo) Finish(ctx context.Context, lobbyID uint64, from, to uint32, battleID uint64, endTip uint32, endParams []string) (bool, error)
```

**CREATE**(KEYS[1]=房间,KEYS[2]=冷却键,KEYS[3..]=名单各人的 of 键;ARGV[1]=now、[2]=lobby_id、[3]=房间 TTL ms、[4]=冷却 ms、[5..]=HSET 字段值对):

```lua
local now = tonumber(ARGV[1])
local cd = redis.call('PTTL', KEYS[2])
if cd > 0 then return {-5, cd} end
if redis.call('EXISTS', KEYS[1]) == 1 then return {-6, 0} end   -- seq 回卷撞 id:调用方重新 INCR 一次
for i = 3, #KEYS do
  local other = redis.call('GET', KEYS[i])
  if other then
    local h = redis.call('HMGET', 'guild:trial:lobby:' .. other, 'state', 'exp', 'ldl')
    local st = tonumber(h[1] or '0')
    if (st == 1 and now < tonumber(h[2])) or (st == 2 and now < tonumber(h[3])) then return {-1, i - 3} end
  end
end
redis.call('HSET', KEYS[1], unpack(ARGV, 5))
redis.call('PEXPIRE', KEYS[1], ARGV[3])
for i = 3, #KEYS do redis.call('SET', KEYS[i], ARGV[2], 'PX', ARGV[3]) end
if tonumber(ARGV[4]) > 0 then redis.call('SET', KEYS[2], '1', 'PX', ARGV[4]) end
return {1, 0}
```
初始字段:`acc=1`(发起人已同意)、`state=1`、`exp=now+ttl`、`ldl=0`、`bid=0`、`etip=0`、`epar=""`、`created=now`。

**RESPOND**(KEYS[1]=房间;ARGV[1]=pid、[2]=accept 0/1、[3]=now、[4]=开战窗口 ms、[5]=拒绝 tip):

```lua
local h = redis.call('HMGET', KEYS[1], 'state', 'exp', 'roster', 'acc')
if not h[1] then return {0, 0} end
local now = tonumber(ARGV[3]); local idx = -1; local n = 0
for id in string.gmatch(h[3], '[^,]+') do if id == ARGV[1] then idx = n end; n = n + 1 end
if idx < 0 then return {-2, 0} end
local acc = tonumber(h[4]); local bit = 2 ^ idx; local mine = math.floor(acc / bit) % 2
if tonumber(h[1]) ~= 1 then return {-3, mine} end
if now >= tonumber(h[2]) then return {-4, 0} end
if ARGV[2] == '0' then
  redis.call('HSET', KEYS[1], 'state', 4, 'etip', ARGV[5], 'epar', ARGV[1]); return {3, 0}
end
if mine == 0 then acc = acc + bit; redis.call('HSET', KEYS[1], 'acc', acc) end
if acc == 2 ^ n - 1 then
  redis.call('HSET', KEYS[1], 'state', 2, 'ldl', now + tonumber(ARGV[4])); return {2, 0}
end
return {1, 0}
```

**FINISH**:`HGET state` 不等于 ARGV from → 0;否则 `HSET state to bid etip epar` → 1。

**有效状态折算**(`LoadForPlayer`):`GET of` → `HGETALL`;缺失 → nil;`state=1 && now ≥ exp` → 视为 4、`etip=kGuildTrialInviteExpired`;`state=2 && now ≥ ldl` → 视为 4、`etip=kGuildTrialServiceBusy`。

## 6.24 `StartGuildTrial`(建房)

1. `start := d.Now()`;`activityPrelude(ctx, true)`。`row, ok := PickForWrite(rows, 3, req.ActivityId, nowMs)`;`!ok || d.Match == nil || d.Lobby == nil` → `kGuildActivityNotOpen`。
2. 帮会等级不足 → `LevelTooLow`;发起人入帮未满 → `JoinTooRecent`。
3. `roster, reason, offender := normalizeTrialRoster(pid, req.MemberPlayerIds, row.TeamSizeMin, row.TeamSizeMax)`:含 0 或重复 → `duplicate`(offender = 重复 id 或 0);不含发起人 → `initiator_missing`;人数越界 → `size`。合法时发起人移到首位,其余保持原相对顺序。不合法 → `kGuildTrialTeamInvalid` [reason, offender]。
4. 缓存成员表逐人:不在帮 → `not_member`;入帮未满 → `join_recent`。
5. 发起人本游戏日 `used_count ≥ daily_limit` → `kGuildActivityAlreadyClaimed`(次数用完的其他成员可随队,结算时无奖励,界面提示)。
6. `onl, err := BatchResolveStrict(roster)`:err → 返回 err;有人不在线 → `offline`。
7. 权威成员核对(非锁定读)`SELECT player_id FROM guild_member WHERE guild_id=? AND player_id IN (…)`,缺人 → `not_member`。
8. `id := INCR guild:trial:lobby:seq`;`Create(ttl=rule.TrialInviteTtlSeconds, cooldown=rule.TrialInviteCooldownSeconds)`:
   - `-5` → `kGuildTrialInviteCooldown`,parameters=[ceil(arg/1000)];
   - `-1` → `kGuildTrialTeamInvalid` [`busy`, roster[arg]];
   - `-6` → 重新 INCR 再试一次,仍 -6 → 返回 err;
   - Redis 出错 → 返回 err。
9. 对每个被邀请人 `l.notify(ACTIVITY_CHANGED, gid, pid, invitee, []uint64{invitee})`(target=被邀请人,客户端据此弹邀请)。
10. 计 `action="invite",result="ok"`,返回视图(含 `trial_lobby`)。

名单校验失败都发生在建房之前,**不消耗冷却**。reason 全集:`duplicate`、`initiator_missing`、`size`、`not_member`、`join_recent`、`offline`、`busy`、`in_battle`、`not_ready`、`invalid`。

## 6.25 `RespondGuildTrialInvite`(同意 / 拒绝 / 发起人取消)

1. `start := d.Now()`;`activityPrelude(ctx, true)`;`d.Match == nil || d.Lobby == nil` → NotOpen。
2. `lb := Lobby.Load(req.LobbyId)`;nil 或 `lb.GuildID != gid` → `kGuildTrialInviteExpired`。
3. `code, arg := Lobby.Respond(…, launchWindowMs=5000, declineTip=kGuildTrialInviteDeclined)`:

| code | 含义 | 处理 |
|---|---|---|
| 0 / -2 / -4 | 房间不存在 / 不在名单 / 已过期 | `kGuildTrialInviteExpired` |
| -3 | 已不是 PENDING | `arg==1`(本人早已同意)→ 成功回视图;否则 `kGuildTrialInviteExpired` |
| 3 | 已拒绝(发起人则为取消) | 推 ACTIVITY_CHANGED 给名单其余人(target 0);成功回视图;`result="declined"` |
| 1 | 已同意,等其他人 | 推 ACTIVITY_CHANGED 给名单其余人(target 0);成功回视图 |
| 2 | 全员同意,本调用负责开战 | 执行 6.26 |

## 6.26 确认开战 `launchTrial(ctx, a, lb, start)`

`end(tip, params)` = `Lobby.Finish(id, 2, 4, 0, tip, params)` + 推 ACTIVITY_CHANGED 给名单其余人 + 本 RPC 回 `error_message = tip` **且同时回重建的视图**(房间已是 ENDED,客户端先应用视图再显示原因);计 `action="launch"`、result 取原因。

1. `row := activity.Row(lb.ActivityID)`;`PickForWrite(rows, 3, lb.ActivityID, nowMs)` 失败 → `end(kGuildActivityNotOpen)`。
2. `BatchResolveStrict(roster)`:err → `end(kGuildTrialServiceBusy)` 后返回 err;有人不在线 → `end(kGuildTrialTeamInvalid, [offline, pid])`。
3. MySQL 成员核对(同 6.24 第 7 步):缺人 → `end(…, [not_member, pid])`;MySQL 出错 → `end(ServiceBusy)` 后返回 err。
4. 发起人本游戏日次数已满 → `end(kGuildActivityAlreadyClaimed)`。
5. `mb := min(MatchBudget, HandlerBudget − since(start) − 500ms)`;`mb < 300ms` → `end(kGuildTrialServiceBusy)`。
6. `actx := &battlepb.BattleActivityContext{Kind: GUILD_TRIAL, GuildId: gid, ActivityId: row.Id, PeriodKey: DayKey(now), InitiatorPlayerId: lb.InitiatorID, GuildPeriodKey: GuildPeriodKey(row, now)}`(以**开战时刻**为准)。
7. `resp, err := d.Match.StartActivityBattle(ctxWithTimeout(mb), {BattleConfigId: row.DungeonId, MemberPlayerIds: roster, ActivityContext: actx})`:
   - err、超时、`INTERNAL` → `end(kGuildTrialServiceBusy)`;
   - `INVALID_ARGUMENT` → ERROR 日志,`end(kGuildTrialTeamInvalid, [invalid, 0])`;
   - `MEMBER_OFFLINE / IN_BATTLE / NOT_READY` → `end(kGuildTrialTeamInvalid, [offline|in_battle|not_ready, offender])`。
8. 登记 `repo.RegisterTrialBattle`(autocommit 单行):
   ```sql
   INSERT INTO guild_trial_battle (battle_id, guild_id, activity_id, period_key, guild_period_key, initiator_player_id,
                                   state, settle_result, rewarded_count, created_ms, settled_ms)
   VALUES (?, ?, ?, ?, ?, ?, ?STARTED, 0, 0, ?, 0)
   ON DUPLICATE KEY UPDATE battle_id = battle_id;
   ```
   失败只打 ERROR、计 `guild_trial_register_fail_total`,**仍成功**:战斗已开始,结算 h 步补登记。
9. `Lobby.Finish(id, 2, 3, battleID, 0, nil)`;失败只 WARN(视图 5s 后显示 ServiceBusy,纯展示偏差,参战者已收到 `BattleStartS2C`)。
10. 推 ACTIVITY_CHANGED 给名单其余人;计 `action="launch",result="ok"`;返回视图。

## 6.27 崩溃与并发窗口(房间)

| # | 场景 | 结果 |
|---|---|---|
| L1 | 建房后、推送前崩溃 / 推送丢失 | 被邀请人打开活动页即看到邀请;30s 后自然过期 ✓ |
| L2 | 最后两人几乎同时同意 | Lua 原子;只有一个调用拿到 2 负责开战,另一个拿到 1 或 -3(已同意)→ 成功 ✓ |
| L3 | LAUNCHING 后、调 match 前崩溃 | 5s 后折算 ENDED(ServiceBusy);成员可重新建房 ✓ |
| L4 | match 已返回、登记与 Finish 前崩溃 | 战斗照常;结算 h 步补登记 ✓;房间展示为 ServiceBusy(偏差,接受) |
| L5 | 被邀请人同意后下线 / 进别的战斗 / 被踢 | 开战复核或 match 预检拒绝 → ENDED,原因带人名 ✓ |
| L6 | 骚扰:反复邀请同一人 | 每人同时只在一个有效房间;发起人建房冷却 10s;被邀请人可拒绝;写限流 5/s ✓ |
| L7 | guild 全局 Redis 故障 | 建房 / 响应返回 err;视图不填 `trial_lobby` ✓ |
| L8 | Redis 被清空 | 房间全失效,seq 回卷由 -6 分支兜住 ✓ |

<!-- s6_activities_part8.md -->

# S6 帮会活动 — 第 8 部分:同道历练(B6b)结果消费、结算事务、待入队循环、巡检器

## 6.28 结果消费者 `go/guild/internal/kafka/trial_result_consumer.go`

```go
type TrialSettleOutcome uint8 // Settled / Duplicate / GuildGone / BadContext / ContextMismatch / Poison / Skipped
type TrialResultHandler func(ctx context.Context, ev *kafkapb.BattleResultEvent) (TrialSettleOutcome, error)
type trialReader interface {
    FetchMessage(ctx context.Context) (kafka.Message, error)
    CommitMessages(ctx context.Context, msgs ...kafka.Message) error
}
func StartTrialResultConsumer(ctx context.Context, brokers []string, cfg config.TrialResultConf, handle TrialResultHandler) error
func runTrialResultConsumer(ctx context.Context, r trialReader, handle TrialResultHandler)   // 可测
```

结构照 `go/match/internal/kafka/result_consumer.go:55-117`:`EnsureTopics`(RetentionMs 7×24×3600×1000,与 match 一致)、`StartOffset: FirstOffset`、`CommitInterval: 0`,在 `safego.Go("guild.kafka.trial_results")` 中运行。每条消息:

1. 拉取失败:退避 1s 重拉。
2. 反序列化失败:ERROR,计 `decode_error`,提交后继续。
3. `ev.ActivityContext == nil` 或 `Kind != GUILD_TRIAL`:直接提交,不计数。
4. 否则循环调 handler:
   - 返回 nil error → 提交 offset;
   - 返回**暂时性**错误(MySQL 驱动错误、1213/1205 重试耗尽、Redis 错误、号段不可用)→ 按 1s、2s、4s… 退避(上限 30s)重调,不提交;ctx 结束则退出不提交;
   - handler 内部已把**确定性**错误(`errActivityPoison`)转成 Poison 终态并返回 nil error,消费者照常提交。

启动:`guild.go` 在 `EnableActivities` 之后、`TrialResultActive()` 为真时启动;首次失败(Kafka 未就绪)照 `match_service.go:78-98` 每 30s 重试,不拒绝启动。

## 6.29 结算 `GuildLogic.SettleTrialResult(ctx, ev) (TrialSettleOutcome, error)`

消费者与巡检器共用。

1. `actx := ev.ActivityContext`;`battle_id`、`guild_id`、`activity_id`、`period_key`、`guild_period_key` 任一为 0,或 `ev.MatchMode != PVE_TEAM` → ERROR,计 `bad_context`;`battle_id != 0` 时 `ackResult(battle_id)`;返回 `BadContext, nil`。
2. `row := activity.Row(actx.ActivityId)`;`cfgOK := row != nil && row.Type == 3`;为假 → ERROR,结果记 `CONFIG_MISSING`,不发奖。
3. `win := ev.Outcome == BATTLE_OUTCOME_SIDE_A_WIN`。
4. `candidates` = `team_index == 0` 的玩家 − `fled_player_ids` − `dead_player_ids`(U2 默认),去重升序。
5. `win && cfgOK`:`bundle, err := BuildRewardBundle(row.RewardId)`;err → `markPoison`(6.31)。bundle 非空时对每个候选人事务外 `EnsureSeqRow` 并取一个 opID;出错 → 返回暂时性 err(已取的 id 浪费,不影响正确性)。
6. `repo.SettleTrialBattleTx(ctx, in)`(6.30),`withActivityTxRetry` 含 `errRetryTx` 最多 3 次。返回 `errActivityPoison` → `markPoison`;其它 err → 返回暂时性 err。
7. 提交后(或 Duplicate / GuildGone / ContextMismatch):
   - `ackResult(battle_id)`:`LocatorRedis.Del("battle:activity_result:{battle_id}")`,失败只 WARN(battle 会重发,再走 Duplicate);
   - 有变更 → `invalidateGuildCache`;
   - 对每个入队 op 按 6.6 预算 `ProcessOne`(消费者无 RPC 截止,`HandlerBudget` 从 handler 开始算);
   - 推送:`FundsGranted` → ACTIVITY_CHANGED 给全帮在线成员;否则只给 `team_index==0` 的参战者;
   - 计指标。

## 6.30 事务 `SettleTrialBattleTx`(按全库锁序)

```sql
-- a. 帮会行
SELECT level FROM guild WHERE guild_id=? FOR UPDATE;
--    无行 → 回滚,走"帮会已解散"分支(见下)

-- 仅 win && cfgOK && len(candidates)>0 时执行 b–g:
-- b. 候选人成员行(升序)
SELECT player_id FROM guild_member WHERE guild_id=? AND player_id IN (…) ORDER BY player_id FOR UPDATE;
--    eligible = candidates ∩ 查到的行(战后退帮者无奖励)

-- c. 每日计数:非锁定读,只决定名单;权威判定在 e 步
SELECT player_id, used_count FROM guild_daily_counter
 WHERE counter_kind=3 AND ref_id=? AND period_key=? AND player_id IN (…);
--    rewarded = eligible 中 used_count < daily_limit 的人(无行视为 0);测试钩子 settleAfterCounterReadHook 在此

-- d. 物品(bundle 非空,按 player_id 升序逐人):enqueueActivityRewardTx
--    ErrTooManyPending → 该人记入 owedList(不写 op 行),继续下一人

-- e. 每人带上限的 upsert(SQL 同 6.11.2 d 步,period_key = actx.period_key)
--    任一 RowsAffected=0 → errRetryTx,整事务回滚重来(快照读可能落后,重试拿新快照)

-- f. 帮会资金(len(rewarded)>0 且 guild_funds>0;period_key = actx.guild_period_key)
INSERT INTO guild_activity_progress (guild_id, activity_id, period_key, progress_count, threshold_reached_ms, funds_granted, updated_ms)
VALUES (?, ?, ?, 0, 0, 0, ?) ON DUPLICATE KEY UPDATE updated_ms = ?;
SELECT progress_count FROM guild_activity_progress WHERE guild_id=? AND activity_id=? AND period_key=? FOR UPDATE;
--    progress_count < guild_threshold 时:
UPDATE guild SET funds = funds + ? WHERE guild_id=? AND funds <= ?;     -- RowsAffected 0 → errActivityPoison
UPDATE guild_activity_progress SET progress_count = progress_count + 1 WHERE guild_id=? AND activity_id=? AND period_key=?;

-- g. rewarded 每人帮贡 UPDATE(同 6.11.2 g 步;RowsAffected 0 → errActivityPoison)

-- h. 幂等闸门(无论输赢)
INSERT INTO guild_trial_battle (battle_id, guild_id, activity_id, period_key, guild_period_key, initiator_player_id,
                                state, settle_result, rewarded_count, created_ms, settled_ms)
VALUES (?, ?, ?, ?, ?, ?, ?STARTED, 0, 0, ?, 0) ON DUPLICATE KEY UPDATE battle_id = battle_id;
SELECT guild_id, state FROM guild_trial_battle WHERE battle_id=? FOR UPDATE;
--    state = SETTLED → ROLLBACK,Duplicate(前面所有奖励随之撤销)
--    guild_id ≠ actx.guild_id → ROLLBACK,ERROR,ContextMismatch(返回 nil)
--    state ∈ {STARTED, EXPIRED} → 继续(迟到的结果照常结算 EXPIRED 行)
UPDATE guild_trial_battle SET state=?SETTLED, settle_result=?, rewarded_count=?, settled_ms=? WHERE battle_id=?;

-- i. 待入队物品(owedList 非空;位置 10,在 h 之后)
INSERT INTO guild_trial_reward_owed (player_id, battle_id, guild_id, activity_id, period_key, payload, attempts, created_ms)
VALUES (?, ?, ?, ?, ?, ?, 0, ?);   -- 每人一行,升序;h 已保证首次结算,1062 视为暂时性 err
COMMIT;
```

- 幂等检查放最后是锁序要求;同帮结算先拿 guild 行锁彼此串行,重复消息代价只是一次多余事务。
- settle_result:win → WIN;非 win → LOSS;`!cfgOK` → CONFIG_MISSING(b–g、i 跳过)。
- `created_ms` 补插时取 `ev.finished_at_ms`,为 0 取 now。
- **"帮会已解散"分支**:单独事务只写一张表:
  ```sql
  INSERT INTO guild_trial_battle (battle_id, guild_id, activity_id, period_key, guild_period_key, initiator_player_id,
                                  state, settle_result, rewarded_count, created_ms, settled_ms)
  VALUES (?, ?, ?, ?, ?, ?, ?SETTLED, ?GUILD_GONE, 0, ?, ?)
  ON DUPLICATE KEY UPDATE settled_ms = IF(state = ?SETTLED, settled_ms, VALUES(settled_ms)),
                          settle_result = IF(state = ?SETTLED, settle_result, VALUES(settle_result)),
                          state = ?SETTLED;
  ```
  `state` 必须写在最后(前两列要读旧 state)。

## 6.31 毒消息 `markPoison(ctx, ev, cause)`

同上"已解散"分支的单表写法,`settle_result=?POISON`;成功后 `ackResult`,ERROR 日志带 battle_id、guild_id 与 cause,计 `poison`,返回 `(Poison, nil)`。单表写失败 → 返回暂时性 err(下轮再试)。确定性错误来源:资金或帮贡溢出、`BuildRewardBundle` 出错、`proto.Marshal` 出错。运维据日志人工补发(runbook 写明)。

## 6.32 待入队循环 `OwedRewardLoop`(`go/guild/internal/logic/trial_background.go`)

每个 guild 实例每 `OwedLoopIntervalSeconds`(10s)跑一轮:
1. `SELECT player_id, battle_id, activity_id, period_key, payload FROM guild_trial_reward_owed ORDER BY created_ms LIMIT 50`(idx_0)。
2. 每行:事务外 `EnsureSeqRow`、取 opID(失败本轮结束);事务:
   ```sql
   BEGIN;
   -- AllocateSeq(位置 5、6)→ ErrTooManyPending:ROLLBACK;autocommit UPDATE … SET attempts=attempts+1;计 still_full;下一行
   -- 插 op 行:同 6.9 各列,payload 原样,next_attempt_ms = now(重投循环立刻领取)
   DELETE FROM guild_trial_reward_owed WHERE player_id=? AND battle_id=?;   -- 位置 10;RowsAffected 0 → ROLLBACK(别的实例已转)
   COMMIT;
   ```
3. 计 `guild_trial_owed_total{result="converted|still_full|error"}`;每轮末 `guild_trial_owed_rows` gauge = `SELECT COUNT(*)`(上限 10000 行时只报 10000)。

## 6.33 巡检器 `TrialSweeper`(同文件)

每 `TrialSweepIntervalSeconds`(60s);先 `SET guild:trial:sweep:lease <实例 id> NX PX 55000`(全局 Redis),抢不到本轮跳过。
1. `SELECT battle_id, guild_id, created_ms FROM guild_trial_battle WHERE state=?STARTED AND created_ms < ? ORDER BY created_ms LIMIT 50`(idx_1;参数 = now − Overdue×1000)。
2. 每行 `GET battle:activity_result:{battle_id}`(LocatorRedis):
   - Redis 出错 → WARN,本轮结束;
   - 有记录 → 反序列化(失败 → `markPoison`)→ `SettleTrialResult` → 计 `sweeper_recovered`;
   - 无记录且 `created_ms < now − Abandon×1000` → `UPDATE guild_trial_battle SET state=?EXPIRED, settled_ms=? WHERE battle_id=? AND state=?STARTED`,计 `sweeper_expired`;
   - 无记录但未到 Abandon → 计入本轮 overdue 数。
3. `guild_trial_result_overdue` gauge = 本轮 overdue 数。告警:>0 持续 10 分钟。

## 6.34 崩溃与并发窗口(结算)

| # | 场景 | 结果 |
|---|---|---|
| T1 | match 返回后、guild 登记前崩溃 | 结算 h 步补行 ✓ |
| T2 | gather 失败(换场景、掉线、prepare 失败) | 无结果;全员删票解冻;发起人无 battle:lock → 视图不显示"进行中",可立即重开 ✓;行 3600s 后 EXPIRED |
| T3 | battle 在记录写入后丢事件 | battle 重发;仍丢则巡检器从记录结算 ✓ |
| T4 | 结算提交后、提交 offset 前崩溃 | 重放 → h 步 Duplicate ✓ |
| T5 | 重平衡 / 巡检器与消费者同时结算同一局 | guild 行锁串行,后到者 Duplicate ✓ |
| T6 | MySQL 故障 | 暂时性错误退避重试,不提交 ✓;lag 告警 |
| T7 | 结算提交后、ProcessOne 前崩溃 | op PENDING,重投循环投递 ✓ |
| T8 | 某人待发行满 16 条 | 物品写 owed 表,循环有空位后入队,最终到账 ✓ |
| T9 | 确定性错误 | POISON 终态、提交 offset,分区不被卡住 ✓;人工补发 |
| T10 | 战后帮会解散 / 成员退帮 | GUILD_GONE / 无该人奖励 ✓ |
| T11 | guild 停机超过 7 天 | topic 与 Redis 记录都过期,不发奖;巡检器判 EXPIRED;runbook 注明 |
| T12 | 新消费组首次启动从头消费 | 普通对局跳过;带上下文未结算的是真实胜场,补发正确 ✓ |
| T13 | 逃跑 / 阵亡 | 不在候选 ✓(U2);挂机不逃跑且存活的仍算参战,v1 接受 |
| T14 | 跨 05:00 | 用上下文里开战时刻的键 ✓ |

## 6.35 指标(B6b 追加)

| 指标 | 标签 / 说明 |
|---|---|
| `guild_trial_result_total` | `result` ∈ settled_win / settled_loss / duplicate / guild_gone / config_missing / bad_context / context_mismatch / poison / decode_error / handler_retry / sweeper_recovered / sweeper_expired |
| `guild_trial_result_lag_seconds` | 直方图 now − finished_at_ms,桶 0.1 0.5 1 5 30 120 600 |
| `guild_trial_result_overdue` | gauge |
| `guild_trial_owed_total{result}`、`guild_trial_owed_rows` | counter、gauge |
| `guild_trial_register_fail_total` | 无标签 |

<!-- s6_activities_part9.md -->

# S6 帮会活动 — 第 9 部分:客户端(mmorpg-client;B6a 三张卡 + 灯会/团圆,B6b 历练选人与邀请)

原则:服务端视图是唯一依据;推送只触发拉取;**遵守 B2 客户端纪律**(s2 §17):写方法成功后应用响应快照、不在回调里连发请求,需要重拉的一律置队列标志,由界面每帧 `DrainQueued` 发出。布局沿用 body 矩形 `GuildContent (556,214,1508,610)`,坐标左上原点、y 向下。

## 6.36 消息号与协议生成

- `tools/gen_messageids.ps1` 白名单(`:35-43` guild 段末尾,排在 B2、B5 行之后)追加五行:
  ```powershell
      "GuildServiceGetGuildActivities" = "GetGuildActivities"
      "GuildServiceLightGuildLantern" = "LightGuildLantern"
      "GuildServiceClaimGuildReunion" = "ClaimGuildReunion"
      "GuildServiceStartGuildTrial" = "StartGuildTrial"
      "GuildServiceRespondGuildTrialInvite" = "RespondGuildTrialInvite"
  ```
  然后重新生成 `Assets/Scripts/Net/MessageIds.cs`,不手改。
- `tools/gen_proto.ps1` **不改**:guild.proto 不再 import asset_op.proto;`match_internal.proto`、battle 新消息不进客户端。

## 6.37 `Assets/Scripts/Game/Guild/GuildClient.cs`

新增状态与方法(补 `using System.Linq; using System.Collections.Generic;`):

```csharp
public GetGuildActivitiesResponse Activities { get; private set; }   // null = 未加载
public bool ActivitiesQueued { get; private set; }                   // 需要重拉活动视图
public bool TrialInvitePending { get; private set; }                 // 收到定向邀请推送,界面应打开活动页
private string _pendingNotice;                                       // 写成功后,紧接着的 GetPlayerGuild 回包沿用这条提示

public void QueueActivities() { if (Info != null) { ActivitiesQueued = true; Changed?.Invoke(); } }
public void ConsumeTrialInvite() => TrialInvitePending = false;

public void RefreshActivities()
{
    if (Info == null) { Reject("加入帮会后可参与帮会活动。"); return; }
    bool queued = ActivitiesQueued; ActivitiesQueued = false; int before = _generation;
    Request(MessageIds.GetGuildActivities, new GetGuildActivitiesRequest(), GetGuildActivitiesResponse.Parser, r =>
    {
        if (!AcceptWrite(r.ErrorMessage)) return;
        Activities = r; Status = "帮会活动已更新";
    });
    if (_generation == before) ActivitiesQueued = queued;   // 被 Busy / 隔离挡掉:下一帧再发(同 B2 Refresh 写法)
}
public void LightLantern(uint id) => ActivityWrite(MessageIds.LightGuildLantern, new LightGuildLanternRequest { ActivityId = id },
    LightGuildLanternResponse.Parser, r => (r.ErrorMessage, r.Activity), "花灯已点亮，帮贡已到账。", true);
public void ClaimReunion(uint id) => ActivityWrite(MessageIds.ClaimGuildReunion, new ClaimGuildReunionRequest { ActivityId = id },
    ClaimGuildReunionResponse.Parser, r => (r.ErrorMessage, r.Activity), "团圆礼已领取，物品稍后到账。", true);
public void StartTrial(uint id, IReadOnlyList<ulong> members)   // B6b
{
    var view = FindActivity(id);
    if (view == null || members == null || members.Count == 0 || members[0] != PlayerId
        || members.Count < view.TeamSizeMin || members.Count > view.TeamSizeMax || members.Distinct().Count() != members.Count)
    { Reject("请选择符合人数要求的在线同道。"); return; }
    var req = new StartGuildTrialRequest { ActivityId = id }; req.MemberPlayerIds.AddRange(members);
    ActivityWrite(MessageIds.StartGuildTrial, req, StartGuildTrialResponse.Parser, r => (r.ErrorMessage, r.Activity),
        "邀请已发出，等待同道确认。", false);
}
public void RespondTrialInvite(ulong lobbyId, bool accept)       // B6b
{
    if (lobbyId == 0) { Reject("邀请已失效。"); return; }
    ActivityWrite(MessageIds.RespondGuildTrialInvite, new RespondGuildTrialInviteRequest { LobbyId = lobbyId, Accept = accept },
        RespondGuildTrialInviteResponse.Parser, r => (r.ErrorMessage, r.Activity), accept ? "已同意同道历练。" : "已婉拒同道历练。", false);
}
private void ActivityWrite<T>(uint id, IMessage req, MessageParser<T> parser,
    Func<T, (TipInfoMessage tip, GuildActivityView view)> pick, string notice, bool guildChanged) where T : IMessage<T>
{
    if (Info == null) { Reject("加入帮会后可参与帮会活动。"); return; }
    Request(id, req, parser, r =>
    {
        var (tip, view) = pick(r);
        ReplaceActivity(view);                       // 失败时服务端也可能回视图(历练开战失败),先应用
        if (!AcceptWrite(tip)) { ActivitiesQueued = true; return; }
        Status = notice;
        if (guildChanged) { _pendingNotice = notice; RefreshQueued = true; }   // 帮贡 / 资金变了:下一帧拉 GuildInfo
    });
}
private GuildActivityView FindActivity(uint id) => Activities?.Activities.FirstOrDefault(a => a.ActivityId == id);
private void ReplaceActivity(GuildActivityView view)
{
    if (Activities == null || view == null) return;
    for (int i = 0; i < Activities.Activities.Count; i++)
        if (Activities.Activities[i].ActivityId == view.ActivityId) { Activities.Activities[i] = view; return; }
}
public string MemberDisplayName(ulong id)
{
    var m = Info?.Members.FirstOrDefault(x => x.PlayerId == id);
    return m != null && !string.IsNullOrEmpty(m.Name) ? m.Name : "道友 · " + id;   // B3b 前回退
}
```

改动既有(B2 已改过的)方法:
- `RefreshQueued` 的 setter 在 B2 是 `private set`,本类内部可写,无需改可见性。
- `Refresh()` 成功分支 `Apply(...)` 之后:`if (_pendingNotice != null) { Status = _pendingNotice; _pendingNotice = null; }`。`KGuildNotInGuild` 分支追加 `Activities = null; ActivitiesQueued = false; TrialInvitePending = false; _pendingNotice = null;`。
- `Reset()` 追加同样四项清空。
- `DrainQueued(bool applicantsVisible)` 改为 `DrainQueued(bool applicantsVisible, bool activitiesVisible)`,在 `RefreshQueued` 分支之后插入:`if (ActivitiesQueued && activitiesVisible && Info != null) { RefreshActivities(); return; }`。
- `HandleGuildChanged` 的 `mine` 分支:`ApplicationReceived` 之后加 `else if (change.Kind == GuildChangeKind.ActivityChanged) { ActivitiesQueued = true; if (aboutMe) { TrialInvitePending = true; Status = "收到同道历练邀请。"; } }`(其余 kind 仍置 `RefreshQueued`)。
- `Accept()` 追加(参数辅助 `static string P(TipInfoMessage t, int i) => i < t.Parameters.Count ? t.Parameters[i] : ""`):

| 码 | Status 文案 |
|---|---|
| `KGuildActivityNotOpen` | 该活动暂未开放。 |
| `KGuildActivityAlreadyClaimed` | 今日已参与，明日 5:00 后再来。 |
| `KGuildActivityThresholdNotReached` | `$"同时在线的同道不足({P(t,0)}/{P(t,1)})，再等等大家吧。"` |
| `KGuildActivityLevelTooLow` | `$"帮会达到 {P(t,0)} 级后才能参与。"` |
| `KGuildActivityJoinTooRecent` | `$"入帮满 {P(t,0)} 小时后才能参与帮会活动。"` |
| `KGuildAssetPending`(B5 已加则跳过) | 上一份奖励仍在发放中，请稍后再试。 |
| `KGuildTrialTeamInvalid` | `TrialReason(P(t,0), P(t,1))` |
| `KGuildTrialInviteExpired` | 邀请已失效。 |
| `KGuildTrialInviteDeclined` | `TrialDeclined(P(t,0))` |
| `KGuildTrialInviteCooldown` | `$"发起太频繁，请 {P(t,0)} 秒后再试。"` |
| `KGuildTrialServiceBusy` | 历练服务繁忙，请稍后再试。 |

`TrialReason(reason, idText)`:`name = ulong.TryParse(idText, out var id) && id != 0 ? MemberDisplayName(id) : ""`;duplicate"队伍名单有重复或无效成员。";initiator_missing"发起人必须在队伍中。";size"队伍人数不符合要求。";not_member `name+" 不是本帮成员。"`;join_recent `name+" 入帮时间不足。"`;offline `name+" 当前不在线。"`;busy `name+" 正在响应其他历练邀请。"`;in_battle `name+" 正在战斗中。"`;not_ready `name+" 暂时无法入场(不在场景中或正在排队)。"`;其它"队伍信息无效，请刷新后重试。"。
`TrialDeclined(idText)`:id 等于当前视图房间发起人 → `name+" 取消了同道历练。"`,否则 `name+" 婉拒了同道历练邀请。"`。
`LobbyEndText(GuildTrialLobbyView lb)`:把 `end_tip_id`/`end_parameters` 构造成临时 `TipInfoMessage` 走上表同一映射(抽成 `static string TipText(TipInfoMessage)`,`Accept` 与它共用),不改 `Status`。
`AssetReasonText` 复用 B5(27001"背包已满，腾出空间后自动发放"等)。

## 6.38 `Assets/Scripts/UI/Ugui/Guild/GuildWindow.cs`

**事件与属性**:
```csharp
public event Action ActivitiesRequested;
public event Action<uint> LanternRequested, ReunionRequested;
public event Action<uint, IReadOnlyList<ulong>> TrialRequested;          // B6b
public event Action<ulong, bool> TrialInviteResponded;                    // B6b
public bool ShowingActivities => IsVisible && Page == GuildPage.Activities;
private ulong _inviteModalLobbyId;                                        // 已为该房间弹过邀请框
```

**入口**:
- `Show(page)` 的 `:92` 之后:`if (page == GuildPage.Activities && _client?.Info != null && _client.Activities == null) ActivitiesRequested?.Invoke();`(**不判 Busy**:只是排队,Toggle 的 Refresh 在途时下一帧自动发出)。
- 刷新按钮 `:75`:Ranking → `RankRequested`;Activities 且已入帮 → `ActivitiesRequested`;其余 → `RefreshRequested`。
- `Render()` 的 `:133-134` 改为 `case GuildPage.Activities: RenderActivities(); break;`。`RenderUnavailable` 保留。

**`RenderActivities()` 布局**(body 坐标):

| 元素 | 名字 | 矩形 (x,y,w,h) | 字号 / 颜色 |
|---|---|---|---|
| 标题 | — | (12,0,900,56) | 40 |
| 重置提示 | `GuildActivityReset` | (12,58,1450,44) | 26 Muted:`"每日 05:00 重置 · 距下次重置 {h} 小时 {m} 分"`(`next_reset_ms − server_time_ms`) |
| 卡片底板 ×3 | `GuildActivityPanel_{i}` | (x,112,484,462),x=i×512 | GuildField |
| 图标 | `GuildIcon_{lantern\|crest\|sword}` | (x+24,132,96) | 沿用现有精灵 |
| 名称 | `GuildActivityName_{i}` | (x+136,136,324,56) | 34 |
| 开放状态 | `GuildActivityState_{i}` | (x+136,192,324,40) | 24 Muted |
| 进度 | `GuildActivityProgress_{i}` | (x+30,250,424,40) | 27 |
| 奖励 | `GuildActivityReward_{i}` | (x+30,296,424,80) | 25 Muted,换行 |
| 我的状态 | `GuildActivityMine_{i}` | (x+30,380,424,72) | 25,换行 |
| 操作按钮 | `GuildActivityAction_{type}` | (x+66,470,352,64) | 28 |
| 页脚 | — | (14,582,1480,28) | 24 Muted:"活动奖励中的物品经背包发放，离线或背包满时稍后自动到账。" |

- 未入帮:标题 + `"加入帮会后可参与帮会活动。"`(12,166,1450,58)+ 按钮 `BrowseGuildsFromActivities`(12,240,350,76)→ `Show(GuildPage.Ranking)`。
- `Activities == null`:标题 + `"正在读取帮会活动…"`。
- 第 i 张卡对应 type=i+1;无视图时画底板加禁用按钮"暂无活动"。

**文案**(UTC+8,`MM-dd HH:mm`,`DateTimeOffset.FromUnixTimeMilliseconds(ms).ToOffset(TimeSpan.FromHours(8))`):
- 开放状态:Open 且 0/0 → "常开",否则 `"进行中 · 至 {end}"`;Upcoming `"{start} 开启"`;Ended"已结束";Disabled"未开放";`min_guild_level > Info.Level` 追加 `" · 需帮会 {n} 级"`。
- 进度:灯会 `"本期点灯 {progress} / {threshold}"`,资金已发追加 `" · 帮会资金已入库"`;团圆:已达 `"本期已团圆"`,否则 `"同时在线 {progress} / {threshold}"`;历练 `"今日资金胜场 {progress} / {threshold}"`。
- 奖励:`"帮贡 +{pc}"`;资金 >0 灯会/团圆拼 `" · 达成后帮会资金 +{gf}"`、历练拼 `" · 胜利帮会资金 +{gf}"`;有物品拼 `" · 物品 " + string.Join("、", items.Select(g => $"#{g.ItemId}×{g.Count}"))`。
- 我的状态:`"今日 {used} / {limit}"`;`pending>0` → `" · " + AssetReasonText(my_pending_reason_tip_id) + $"({pending})"`;`reject_tip!=0` → `" · 上次物品发放失败"`;历练另起一行:`my_trial_battle_id!=0` → "历练进行中";否则房间存在时 → PENDING `"等待同道确认 {accepted}/{members}"`、LAUNCHING"正在开战…"、LAUNCHED"历练已开启"、ENDED `LobbyEndText(lb)`。

**按钮**(`Busy` 时一律不可点):

| 类型 | 可参与(blocked_tip_id == 0) | 否则(不可点) |
|---|---|---|
| 灯会 | "点亮花灯" → `LanternRequested(id)` | AlreadyClaimed"今日已点灯";NotOpen"未开放";LevelTooLow"帮会等级不足";JoinTooRecent"入帮时间不足" |
| 团圆 | "领取团圆礼" → `ReunionRequested(id)` | ThresholdNotReached"人数未齐";AlreadyClaimed"今日已领取";其余同上 |
| 历练 | 按下表顺序取第一条 | AlreadyClaimed"今日次数已满";其余同上 |

历练按钮(B6b):① `my_trial_battle_id != 0` → "历练进行中"(不可点);② 房间 PENDING 且本人是发起人 → "取消邀请" → `TrialInviteResponded(lobby, false)`;③ 房间 PENDING、本人在名单未同意 → "响应邀请" → `ShowTrialInvite(view)`;④ 房间 PENDING/LAUNCHING 且本人已同意 → "等待同道"(不可点);⑤ 其它 → "组队历练" → `ShowTrialPicker(view)`。**被邀请的成员即使次数已满也能响应**:③④ 优先于 blocked_tip_id。

**`ShowTrialInvite(view)`**(B6b):`Render` 末尾若 `ShowingActivities`、历练视图房间 PENDING、本人在名单未同意、`lobby_id != _inviteModalLobbyId` 且当前无其它 modal,则自动调用并记下 `_inviteModalLobbyId`。`Modal("同道历练邀请", $"{MemberDisplayName(init)} 邀请你同往历练，{sec} 秒内有效。{(used>=limit ? "你今日次数已满，胜利不再得奖。" : "")}")`;`DeclineGuildTrial`(66,460,300,82)"婉拒" 与 `AcceptGuildTrial`(692,460,340,82)"同意":点击 `CloseModal()` 后触发 `TrialInviteResponded(lobby_id, accept)`。`sec = max(0, (expire_at_ms − server_time_ms)/1000)`。

**`ShowTrialPicker(view)`**(B6b):`Modal("同道历练", $"选择 {min-1}–{max-1} 位在线同道，对方同意后开战。")`;候选 = `Info.Members` 中 `Online && PlayerId != 自己`,按 PlayerId 升序;状态 `List<ulong> _trialSelected`(保持点选顺序)、`int _trialPage`;每页 6 人两列三行,`GuildTrialMember_{id}` (66+c×490, 250+r×66, 470, 58),已选前缀"✓ ",未选且已满 `max-1` 时忽略,切换后重画;无候选 `"暂无在线同道，邀请帮会成员上线后再来。"`(66,260,968,60);`GuildTrialPrevious`(390,470,130,60)、`GuildTrialNext`(540,470,130,60);`CancelGuildTrial`(66,460,300,82);`ConfirmGuildTrial`(692,460,340,82)"发出邀请":人数合法且不 Busy 可点,点击 `CloseModal()` 后触发 `TrialRequested(id, [自己]+_trialSelected)` 并清空。`Hide()`、`ResetSession()` 清 `_trialSelected`、`_trialPage`、`_inviteModalLobbyId`。

## 6.39 `Assets/Scripts/UI/Ugui/Guild/GuildUiRoot.cs`

- `:61-67` 之后:
  ```csharp
  _window.ActivitiesRequested += () => { if (_available) _client?.QueueActivities(); };
  _window.LanternRequested += id => { if (_available) _client?.LightLantern(id); };
  _window.ReunionRequested += id => { if (_available) _client?.ClaimReunion(id); };
  _window.TrialRequested += (id, m) => { if (_available) _client?.StartTrial(id, m); };           // B6b
  _window.TrialInviteResponded += (lobby, ok) => { if (_available) _client?.RespondTrialInvite(lobby, ok); }; // B6b
  ```
- `Update()` 中 B2 的 `DrainQueued` 调用改为 `_client?.DrainQueued(_window.ShowingApplications, _window.ShowingActivities);`,并在它**之前**加(B6b):
  ```csharp
  if (_client != null && _client.TrialInvitePending && !GameplayInputGate.IsKeyboardBlocked)
  {
      _client.ConsumeTrialInvite();
      if (!_window.IsVisible) { Team.TeamUiRoot.Instance?.HidePanel(); GameplayUiRoot.Instance?.HidePanel();
          CityTravelUiRoot.Instance?.HidePanel(); AttributeUiRoot.Instance?.HidePanel(); PetUiRoot.Instance?.HidePanel(); }
      if (!_window.ShowingActivities) _window.Show(GuildPage.Activities);
      _client.QueueActivities();
  }
  ```
  战斗中 `_available` 为假,邀请标志保留到战斗结束(届时多半已过期,视图显示"邀请已失效")。输入框聚焦时推迟弹出。
- 战斗入场复用既有链路:参战者收到 `BattleStartS2C`,由战斗 UI 接管。

<!-- s6_activities_part10.md -->

# S6 帮会活动 — 第 10 部分:测试(Go / C++ / Unity)与 robot 冒烟

## 6.40 Go 单元测试(不依赖外部服务)

### `go/guild/internal/activity/rules_test.go`(B6a)

`TestMain` 调 `table.LoadTables("../../../../generated/tables", false)`(相对路径落码时核对)。

| 用例 | 断言 |
|---|---|
| `TestStateOf` | enabled=false → Disabled;0/0 → Open;now<start → Upcoming;now==start → Open;now==end → Ended |
| `TestGuildPeriodKey` | 0/0 行 → DayKey(now);start=UTC+8 `2026-09-20 10:00` 的灯会行在 09-21、09-25 两个时刻 → 都是 20260920;同一时刻历练行 → DayKey(now);UTC+8 `09-21 04:59:59.999` 的 DayKey → 20260920 |
| `TestNextResetMs` | 等于 `gameday.NextDailyReset(now).UnixMilli()`(04:00 → 当天 05:00;05:00 → 次日 05:00) |
| `TestJoinedLongEnough` | minHours=0 恒真;恰好满为真;差 1ms 为假;joinTimeMs=0 为假 |
| `TestBuildRewardBundle` | 0 → nil;Reward 1(两槽 item 1×2)→ 单项 item 1×4;不存在 → error;全 0 → nil |
| `TestBlockedTipPriority` | NotOpen > LevelTooLow > JoinTooRecent > AlreadyClaimed > ThresholdNotReached;全满足 → 0;type≠2 忽略阈值 |
| `TestSelectVisibleAndPickForWrite` | 同类型 Open(id 5)+ Upcoming(id 2)→ 选 5;只有两行 Upcoming → start 小者;缺类型跳过;`PickForWrite(type 1, id 2)` 在选中 5 时 → false;选中行非 Open → false |
| `TestValidateRows` | 默认 3 行 → nil;逐条违规:type=4、daily_limit=0、end<start、type3 dungeon=0、team_size_max=6、type1 threshold=0、reward 不存在、**两行灯会时间窗重叠**、**0/0 行与另一启用灯会行并存**、`trial_invite_ttl_seconds=5` → 各自报错且信息含表名与 id;一行 enabled=false 的重叠 → nil |

### `go/guild/internal/logic/activity_logic_test.go`(B6a)+ `activity_trial_test.go`(B6b)

假 repo、假 Loop、miniredis、注入时钟:

| 用例 | 断言 |
|---|---|
| `TestActivityNoSessionPermissionDenied` | 五个 RPC 无会话 → `codes.PermissionDenied` |
| `TestLanternSyncBudget` | 时钟使 `HandlerBudget − 已用 < 300ms` → ProcessOne 未调、`guild_asset_sync_skipped_total{kind="activity"}` +1;余量充足 → 调用且 ctx 截止 ≤ 2500ms |
| `TestTrialStubsB6a`(B6a) | 预检通过后 Start/Respond 恒回 NotOpen;视图 type 3 为 DISABLED |
| `TestNormalizeTrialRoster` | 含 0 → duplicate;重复 → duplicate+该 id;不含 pid → initiator_missing;越界 → size;合法时 pid 到首位、其余保持相对顺序 |
| `TestLaunchMatchBudget` | 剩余预算 <800ms → 不调 match、回 ServiceBusy、房间 ENDED;match 桩返回 gRPC Unavailable / INTERNAL → ServiceBusy(**不是** gRPC 错误);OFFLINE+offender → TeamInvalid [offline, id] |
| `TestMyTrialBattleIDFromLock` | miniredis 有 `battle:lock:{pid}=77` 且假 repo 77 为 STARTED → 视图 77;锁不在 → 0;行为 SETTLED → 0 |
| `TestTrialCandidates` | team 0/1、逃跑、阵亡、重复 id → 只剩 team 0 未逃未亡者,升序去重 |
| `TestSettleFiltersEvents` | 无上下文、kind=NONE、guild_period_key=0、PVE_SOLO → BadContext,不调仓库;battle_id≠0 时 miniredis 结果键被删 |
| `TestSettlePoisonMarksAndAcks` | 仓库桩返回 `errActivityPoison` → 调 markPoison、删结果键、返回 (Poison, nil) |

### `go/guild/internal/data/trial_lobby_repo_test.go`(B6b,miniredis 跑 Lua)

1. 建房后 hash 字段、三把 of 键、冷却键 TTL 正确;同一发起人 10s 内再建 → -5 且剩余 ms>0。
2. 被邀请人已在另一 PENDING 未过期房间 → -1 与其下标;那个房间过期(推时钟)或 ENDED → 可建。
3. 同意顺序 B→C:B 回 1,C 回 2、state=2、ldl=now+5000;C 再调 → -3 arg=1。
4. B 拒绝 → 3、state=4、etip=Declined、epar=B;之后 C 同意 → -3 arg=0。
5. now ≥ exp → -4;不在名单 → -2;不存在 → 0。
6. `LoadForPlayer`:PENDING 过期折算 ENDED+Expired;LAUNCHING 超 ldl 折算 ENDED+ServiceBusy。
7. `Finish` from 不符 → false。
8. 预置同 id 房间 → Create 回 -6。

### `go/guild/internal/logic/trial_background_test.go`(B6b)

- 巡检器:假 repo 返回 3 行超时 STARTED;miniredis 只有第 1 行的结果记录 → 第 1 行调 Settle、第 2 行(created 超 Abandon)置 EXPIRED、第 3 行只计入 overdue gauge=1;租约键被别的值占用 → 本轮不查库。
- 待入队循环:假 AllocateSeq 第一次 ErrTooManyPending → 不删 owed、attempts+1;第二次成功 → 插 op 且删 owed。

### `go/guild/internal/kafka/trial_result_consumer_test.go`(B6b)

用假 `trialReader`:1 坏字节 → 提交、不调 handler;2 无上下文 → 提交、不调;3 handler 前两次暂时性 err、第三次 nil → 调 3 次、之后提交 1 次、期间不提交;4 handler 返回 (Poison, nil) → 提交;5 退避中 ctx 取消 → 退出不提交。

### `go/match/internal/logic/activitybattlelogic_test.go`(B6b)

用 `newGatherSvcCtx(t)`(`gather_fingerprint_test.go:81`)、`stubGatherRPCs`,`runActivityGatherFn` 换成记录参数的桩:

| 用例 | 断言 |
|---|---|
| `TestStartActivityBattleValidates` | 0 人、6 人、重复、kind=NONE、guild_period_key=0、initiator≠首位 → INVALID_ARGUMENT;带会话 ctx → PermissionDenied;不写票 |
| `TestStartActivityBattleOfflineMember` | 第 2 人无 session → MEMBER_OFFLINE,offender=第 2 人,无票 |
| `TestStartActivityBattleLockedMember` | 写入 `battle:lock:{id}` → MEMBER_IN_BATTLE |
| `TestStartActivityBattleExistingTicket` | 第 3 人持 queued 票且在队列 → NOT_READY;第 1、2 人刚建的票已删 |
| `TestStartActivityBattleHappy` | battle_id≠0;3 人有 matched 票(PVE_TEAM,TTL=`matchedTicketTTLFor(3)`);桩收到同一 battleID、`proto.Equal(actx)`;改请求里的 actx 不影响桩副本 |
| `TestRunActivityGatherPassesPresetIDAndContext` | 真跑 gather:`createBattleFn` 收到 `BattleId == 预设值`、`ActivityContext` 相同;BattleIDGen 未调用 |
| 回归 | 既有 `TestGather*` 全部通过 |

match 根包 `match_service_internal_guard_test.go`:带 `x-session-detail-bin` 调 `/match.MatchInternal/StartActivityBattle` → PermissionDenied、handler 未调;不带 → 调用;`/match.MatchService/JoinQueue` 带 metadata → 放行。

## 6.41 Go 集成测试(MySQL)

`go/guild/internal/data/activity_repo_integration_test.go`,标签 `integration`,环境变量 `GUILD_IT_MYSQL_DSN`,用 B1 schemamigrate 测试建库助手建全部表。**不允许 SKIP 当作通过**。

| # | 用例 | 断言 |
|---|---|---|
| I1 | 灯会首次点灯 | 计数 1、进度 1、帮贡各 +20;无 op 行 |
| I2 | 同日第二次 | AlreadyClaimed;全部不变 |
| I3 | 阈值 3,4 人依次点灯 | 仅第 3 次 ReachedNow+FundsGranted;funds 恰 +500;第 4 次不加 |
| I3b | 档期行(start 设在"昨天"),第 1 游戏日 3 人点灯达阈值;推时钟到第 2 游戏日,同 3 人再点 | 第 2 天个人计数重新为 1、都成功;进度 6(同一 period_key);funds 仍只 +500 |
| I4 | 同一玩家 10 goroutine | 恰 1 成功,其余 AlreadyClaimed |
| I5 | 3 人各 2 goroutine | 进度 3,资金一次,每人计数 1 |
| I6 | 团圆 observed=false 未锁存 | ThresholdNotReached;回滚后进度行不存在 |
| I7 | 团圆 observed=true,之后另一人 observed=false | 两人都成功;`threshold_reached_ms` 保持首次值 |
| I7b | 档期行团圆第 1 天锁存;第 2 天 observed=false 领取 | 成功(锁存跨游戏日有效) |
| I8 | 带物品 | op kind=3、stream=2、tx_type、deadline_ms=0、contribution_delta=0、payload=item 1×4;seq 行 next_seq=2 |
| I9 | 预置 16 条 PENDING,点团圆 | AssetPending;计数、帮贡、seq 不变 |
| I10 | funds 已到上限 | errActivityPoison,全部回滚 |
| I11 | 先删成员行 | NotInGuild |
| I12 | 解散 | 进度与历练行被删;计数、op、owed 行保留 |
| I13(B6b) | 胜利:4 名候选,1 人已用满 | 3 人得帮贡与 op;funds +300;进度 1;历练行 SETTLED/WIN/rewarded_count=3 |
| I14(B6b) | 同一 battle 重放 | Duplicate;所有表与 I13 后逐字节相同 |
| I15(B6b) | 上限 3:连续 4 场胜 | 第 4 场帮贡照发、资金不加 |
| I16(B6b) | 失败 / 平局 | 仅历练行 SETTLED/LOSS |
| I17(B6b) | 帮会不存在 | 插 SETTLED/GUILD_GONE;重放仍 GuildGone 不报错 |
| I18(B6b) | 登记行 guild_id 与事件不同 | ContextMismatch,行不变 |
| I19(B6b) | 无登记行 | 补插并 SETTLED |
| I20(B6b) | 过期计数(钩子在读后插 used_count=2) | 第一轮 errRetryTx,第二轮该人无奖、其他人有 |
| I21(B6b) | 预置候选人 B 16 条 PENDING,胜利结算 | B 无 op 行、owed 表 1 行、帮贡照发;把 B 的 16 行改 APPLIED 后跑一轮 OwedRewardLoop → B 出现第 17 个 seq 的 op、owed 行删除 |
| I22(B6b) | 行为 EXPIRED,迟到结果到达 | 正常结算为 SETTLED/WIN |
| I23(B6b) | markPoison 于 STARTED 行、于 SETTLED 行 | 前者 SETTLED/POISON;后者不变 |

## 6.42 C++ gtest(B6b)

新建 `cpp/tests/turn_battle_engine_test/battle_result_activity_test.cpp`,登记进同目录 vcxproj 与 `.filters`(**他人有未提交修改,只追加 `ClCompile` 一行**)。include 路径已含 `../../libs/services/battle/` 与 `../../generated/`(`turn_battle_engine_test.vcxproj:123`)。

```cpp
TEST(BattleResultActivity, NoneKindLeavesContextUnset)     // kind NONE → !has_activity_context();空名单 size 0
TEST(BattleResultActivity, GuildTrialContextEchoed)        // 6 个字段逐一相等
TEST(BattleResultActivity, FledAndDeadSortedDeduplicated)  // fled {9,3,9} → [3,9];dead {5,5} → [5]
TEST(BattleResultActivity, ClassifyRetryOrder)             // (false,99,30)→kDone;(true,30,30)→kExhausted;(true,0,30)→kResend
```

## 6.43 Unity EditMode(`Assets/Tests/EditMode/Guild/GuildUiTests.cs`)

- `UnsupportedActionsAreClearlyDisabled`(`:198`)删掉 `[TestCase(GuildPage.Activities)]`。
- `GuildClientTests`:
  1. `RefreshActivitiesStoresResponse`:调用 id 为 `GetGuildActivities`;回包后 `Activities.Activities.Count == 3`。
  2. `LanternWriteQueuesGuildRefreshKeepingNotice`:回包后第 1 条 `MyUsedCount==1`、`RefreshQueued==true`、**回调内未发第二个请求**;`DrainQueued(false,true)` 发 `GetPlayerGuild`,回包后 `Status == "花灯已点亮，帮贡已到账。"`。
  3. `ActivityPushWhileBusyDrainsNextFrame`:`Refresh` 在途(Busy)时收到 kind=12 推送 → `ActivitiesQueued==true`、未发请求;回包后 `DrainQueued(false,true)` → 发 `GetGuildActivities`。
  4. `ActivitiesRequestedWhileToggleRefreshInFlight`:`Refresh` 在途时 `QueueActivities()`,回包后 Drain → 发活动请求(页面不会卡在"正在读取")。
  5. `InvitePushSetsPending`:target=自己的 kind=12 → `TrialInvitePending==true`;target=0 → false。
  6. `[TestCase]` 11 个活动 tip 码 `ActivityTipsMapToReadableStatus`(含参数替换)。
  7. `StartTrialValidatesLocally`;8. `TrialTeamInvalidShowsMemberName`(["offline","42"]);9. `NotInGuildClearsActivitiesAndNotice`;10. `ResetClearsActivities`。
- `GuildWindowTests`:
  11. `ActivitiesPageQueuesLoadEvenWhenBusy`:Busy 时 Show(Activities) 仍触发 `ActivitiesRequested` 1 次。
  12. `ActivitiesPageRendersDataDrivenPanels`:3 条 Fixture → `GuildActivityAction_1..3` 存在,文本含 `"本期点灯 2 / 3"`。
  13. `LanternButtonDisabledAfterClaim`。
  14. `PendingRewardShowsBagFullReason`:`my_pending_reward_count=1`、reason 27001 → 我的状态含"背包已满"。
  15. `TrialPickerLimitsSelectionAndRaisesEvent`:max=3 点 3 人只选 2;确认后参数 `[self,a,b]`。
  16. `TrialInviteModalShownOnceAndResponds`:房间 PENDING、本人未同意 → 出现 `AcceptGuildTrial`;再 Render 不重复弹;点击 → `TrialInviteResponded(lobby,true)`。
  17. `TrialButtonStates`:五种按钮状态各一例(进行中 / 取消邀请 / 响应邀请 / 等待同道 / 组队历练)。
- Fixture:静态 `GuildClientTests.ActivitiesFixture()`;`Assets/Editor/Guild/GuildUiVerification.cs` 的 `Fixture()` 追加活动与一个 PENDING 房间,`CaptureAll` 自动截活动页。
- 命令:编辑器未锁时 batchmode `-runTests -testPlatform EditMode -testFilter "Guild"`(**不带 `-quit`**);锁定时先 Roslyn 离线编译体检。

## 6.44 robot 冒烟(`robot/guild_smoke_scenario.go` 新阶段 `activities`)

- **账号**(写进契约 §6):B2 = `robot_9211–9213`,B5 = `robot_9214–9215`(**S5 须把默认值从 9217/9218 改为 9214/9215**),B6 = `robot_9216`(A,帮主)、`9217`(B)、`9218`(C)、`9219`(D)。头注释第 4 条补:这些账号首次必须在 zone_a 建角。
- **yaml** `robot/etc/guild_smoke.yaml` 追加 `activities: true`、`activity_accounts: [robot_9216, robot_9217, robot_9218, robot_9219]`、`trial: true`(B6a 阶段 false);字段加在 `robot/config/config.go` 的 guild smoke 结构。
- `guildSmokeIsGuildMessage`(`:567`)追加 5 个消息号;推送沿用 B2 的 `waitPush`。
- **前置**:默认配表、`activity_join_min_hours=0`;4 个账号登录进场,**各自先 `leaveAnyGuild`**(复用 B5 助手;帮主则解散);A 建帮(名带 nonce),B/C/D 申请、A 审批;记 `funds0` 与各人 `contribution_total0`。

| 步 | 操作 | 通过标准 |
|---|---|---|
| S1 | A `GetGuildActivities` | 3 条,type 1/2/3;B6a:灯会、团圆 OPEN,历练 DISABLED;B6b:三条 OPEN;灯会 `blocked_tip_id=0`;若 `my_used_count≥1` → `FAIL step=S1 reason=same-game-day-rerun` |
| S2 | A 点灯 | `my_used_count=1`、`progress=1`;GetPlayerGuild 帮贡 = 基线+20 |
| S3 | A 再点 | `kGuildActivityAlreadyClaimed` |
| S4 | B、C 各点一次 | C 回包 `progress=3`、`threshold_reached`、`funds_granted`;funds=funds0+500;5s 内 A 收到 kind=12(没收到只 WARN) |
| S5 | D 点灯 | 成功;funds 仍 funds0+500 |
| S6 | A 领团圆(4 人在线) | 帮贡 +30;10s 内轮询至 `my_pending_reward_count=0`;背包助手可用时 item 1 +4 |
| S7 | D 登出,B 领团圆 | 成功(已锁存) |
| S8 | D 重登;A 再领 | AlreadyClaimed |
| S9(B6b) | A `StartGuildTrial(3,[A,B,C])` | 回包 `trial_lobby.state=PENDING`、accepted=[A];5s 内 B、C 各收到 kind=12 且 target=自己(没收到只 WARN) |
| S10(B6b) | B `GetGuildActivities` 取 lobby_id → `RespondGuildTrialInvite(lobby,true)`;C 同样 | B 回包 PENDING 且 accepted 含 B;C 回包无错、`state=LAUNCHED`、`battle_id≠0`;A、B、C 20s 内收到 `NotifyBattleStart` 且 battle_id 相同;各自 `SetAutoBattle`(`battle_smoke_scenario.go:341`);都收到 `NotifyBattleEnd`,outcome=SIDE_A_WIN,且结算里三人 `is_dead=false`(否则 WARN 并跳过 S11 的该人断言) |
| S11(B6b) | 15s 内轮询 A 视图 | 历练 `progress=1`;A、B、C 帮贡各 +50;funds 再 +300;`my_trial_battle_id=0` |
| S12(B6b) | A `StartGuildTrial(3,[A,D])` → D `Respond(lobby,false)` | D 回包无错;A 视图房间 ENDED、`end_tip_id=kGuildTrialInviteDeclined`、parameters=[D] |
| S13(B6b) | D 登出;A `StartGuildTrial(3,[A,D])` | `kGuildTrialTeamInvalid` ["offline","<D>"](校验先于建房,不受 S12 冷却影响) |
| S14(B6b) | A `StartGuildTrial(3,[A])` | ["size","0"] |
| S15(B6b) | B `StartGuildTrial(3,[A,C])` | ["initiator_missing","0"] |

全部通过输出 `GUILD_SMOKE_ACTIVITIES_OK`;任一步失败输出 `GUILD_SMOKE_ACTIVITIES_FAIL step=Sx reason=…` 并非 0 退出。

**可重复性**:个人计数按游戏日;重跑前由 Codex 只对这 4 个测试账号执行
`DELETE FROM mmorpg_guild.guild_daily_counter WHERE player_id IN (<4 个 id>) AND counter_kind=3 AND period_key=<今天>;`
(前置里的解散会删旧进度行)。冒烟脚本本身不碰数据库。

<!-- s6_activities_part11.md -->

# S6 帮会活动 — 第 11 部分:批次文件清单、Codex 验证、会话协调、待拍板、契约偏差

## 6.45 批次拆分(每子批 ≤30 手改文件,开工前各自向用户申请授权)

生成物不计:`generated/**`、`go/*/generated/**`、`cpp/generated/proto|rpc/**`、`go/proto/**`、`MessageIds.cs`、客户端 Generated。

### B6a-srv(30)

| # | 文件 | 动作 |
|---|---|---|
| 1–2 | `data/schema/guildactivity_table.proto`、`data/GuildActivity.xlsx` | 新建(6.2.1、6.2.2) |
| 3–4 | `data/schema/guildrule_table.proto`、`data/GuildRule.xlsx` | 追加三列并填默认值(6.2.3) |
| 5 | `data/tip/Tip.xlsx` | `//guild_error` 追加 `GuildActivityJoinTooRecent`、`GuildTrialInviteExpired`、`GuildTrialInviteDeclined`、`GuildTrialInviteCooldown`、`GuildTrialServiceBusy`(fault=1);契约 §4 活动码若前批未加一并补 |
| 6 | `data/MessageLimiter.xlsx` | 拿到消息号后:GetGuildActivities 10/1s;其余四个 5/1s |
| 7 | `proto/guild/guild.proto` | 6.4 |
| 8 | `proto/guild/guild_db.proto` | `GuildActivityProgressRecord`(6.5) |
| 9–11 | `cpp/generated/table/{CMakeLists.txt,table.vcxproj,table.vcxproj.filters}` | 登记 guildactivity(照 petrule;核对 activityschedule 缺口是否已由 B5 补) |
| 12 | `go/guild/internal/data/tables.go` | `Tables()` 追加进度表 |
| 13–15 | `go/guild/internal/activity/{rules.go,config.go,rules_test.go}` | 新建 |
| 16–17 | `go/guild/internal/data/{activity_repo.go,activity_repo_integration_test.go}` | 新建(I1–I12) |
| 18 | `go/guild/internal/data/guild_repo.go` | 解散删进度行 |
| 19–21 | `go/guild/internal/logic/{activity_logic.go,activity_logic_test.go,activity_metrics.go}` | 新建 |
| 22 | `go/guild/internal/logic/online_status_resolver.go` | `BatchResolveStrict` |
| 23 | `go/guild/internal/server/guild_server.go` | 五个委托 |
| 24–25 | `go/guild/internal/session/{session.go,session_test.go}` | 白名单 |
| 26–27 | `go/guild/internal/constants/{constants.go,constants_test.go}` | tip 常量(含 B6b 用的四个,一次加齐) |
| 28 | `go/guild/guild.go` | `ValidateTables` + `EnableActivities` |
| 29–30 | `go/guild/internal/config/{config.go,config_test.go}` | DSN 不含 `clientFoundRows=true` |

(原稿的 `go/shared/gameday` 改动删除:复用 B5 的 `NextDailyReset`。)

### B6a-cli(11)

`mmorpg-client/tools/gen_messageids.ps1`;`Assets/Scripts/Game/Guild/GuildClient.cs`;`Assets/Scripts/UI/Ugui/Guild/GuildWindow.cs`;`Assets/Scripts/UI/Ugui/Guild/GuildUiRoot.cs`;`Assets/Tests/EditMode/Guild/GuildUiTests.cs`;`Assets/Editor/Guild/GuildUiVerification.cs`;`robot/guild_smoke_scenario.go`;`robot/etc/guild_smoke.yaml`;`robot/config/config.go`;`docs/design/guild-phase2.md` §6;`PROGRESS.md`。历练选人、邀请框、`StartTrial`/`RespondTrialInvite` 留到 B6b-cli。

### B6b-srv1:battle + match(17;另 ≤3 个生成目录工程登记)

| # | 文件 |
|---|---|
| 1–4 | `proto/battle/battle_data.proto`、`proto/battle/battle_node.proto`、`proto/contracts/kafka/match_event.proto`、`proto/match/match_internal.proto`(新建) |
| 5 | `cpp/libs/services/battle/system/battle_result_activity.h`(新建) |
| 6–7 | `cpp/nodes/battle/logic/battle_room_manager.{h,cpp}` |
| 8–10 | `cpp/tests/turn_battle_engine_test/battle_result_activity_test.cpp`(新建)、同目录 `.vcxproj`、`.vcxproj.filters` |
| 11–17 | `go/match/internal/logic/{gather.go,activitybattlelogic.go,activitybattlelogic_test.go}`、`go/match/internal/server/matchinternalserver.go`、`go/match/match_service.go`、`go/match/match_service_internal_guard_test.go`、`go/match/internal/metrics/metrics.go` |

### B6b-srv2:guild(22)

| # | 文件 |
|---|---|
| 1–2 | `proto/guild/guild_db.proto`(两表两枚举)、`go/guild/internal/data/tables.go` |
| 3–6 | `go/guild/internal/config/{config.go,config_test.go}`、`go/guild/etc/guild.yaml`、`go/guild/internal/svc/servicecontext.go` |
| 7–8 | `go/guild/internal/kafka/{trial_result_consumer.go,trial_result_consumer_test.go}` |
| 9–10 | `go/guild/internal/data/{trial_lobby_repo.go,trial_lobby_repo_test.go}` |
| 11–13 | `go/guild/internal/data/{activity_repo.go,activity_repo_integration_test.go(I13–I23),guild_repo.go}` |
| 14–17 | `go/guild/internal/logic/{activity_logic.go(去桩),activity_trial.go,activity_trial_test.go,activity_metrics.go}` |
| 18–19 | `go/guild/internal/logic/{trial_background.go,trial_background_test.go}` |
| 20 | `go/guild/guild.go`(消费者、巡检器、待入队循环) |
| 21–22 | `go/guild/internal/data/trial_result_record.go`(结果键常量与 ack)、`docs/design/guild-phase2.md`(§6 运维段:毒消息、EXPIRED 局、owed 积压的排查与人工补发) |

### B6b-cli(7)

`GuildClient.cs`、`GuildWindow.cs`、`GuildUiRoot.cs`、`GuildUiTests.cs`、`robot/guild_smoke_scenario.go`(S9–S15)、`robot/etc/guild_smoke.yaml`(`trial: true`)、`PROGRESS.md`。

## 6.46 Codex 验证(串行,工作目录 `E:\work\xuanming-server-mmo`)

### B6a-srv
0. 前置:加载 buildenv;`git status --short proto cpp/generated go/proto go/*/generated robot/generated data generated tools/data_table_exporter/state > pre-b6a-status.txt`,生成后逐项对比他人未提交产物未被覆盖;依赖核对(任一不满足即停):`rg -n "GUILD_ASSET_OP_KIND_ACTIVITY_REWARD" proto/guild/guild_db.proto`、`rg -n "TX_GUILD_ACTIVITY_REWARD" proto/common/rollback/transaction_log.proto`、`rg -n "func NextDailyReset" go/shared/gameday/gameday.go`、`rg -n "NotifyGuildChanged" proto/guild/guild.proto`、`rg -n "EnableEconomy|withSyncDelivery" go/guild/internal/logic`、`rg -n "DrainQueued" ../mmorpg-client/Assets/Scripts/Game/Guild/GuildClient.cs`。
1. 导表:`python tools/data_table_exporter/run.py tools/data_table_exporter/exporter_config.yaml`。通过:`generated/tables/guildactivity.json` 3 行;`go/shared/generated/table/guildactivity_table.go` 存在;tip 出现 5 个新码;`tip_enum_ids.json` 只有新增。(FK 列 0 值按"无引用"跳过,`foreign_key.py:13`,无需预案。)
2. proto-gen:过期则 `tools/proto_generator/protogen` 下 `go build -o proto-gen.exe ./cmd`;`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath tools/proto_generator/protogen/etc/proto_gen.yaml`。通过:`message_id.txt` 新增 5 个 `GuildService*`;gate `IsClientMessageId` 含这 5 个;路由表 5 项 `ClientProtocol: true`;记录 `kMaxRpcMethodCount` 变化;Agones 块确被改坏才恢复。
3. 回填 MessageLimiter 5 行,重跑第 1 步。
4. C++:MSBuild Debug x64 `/m:1` 编 `cpp/generated/table/table.vcxproj` 与 gate 工程,0 error;Linux CMake 构建 table 目标。
5. Go:`cd go/guild; go build ./...; go vet ./...; go test -count=1 ./internal/activity ./internal/logic ./internal/session ./internal/constants ./internal/config`;设 `$env:GUILD_IT_MYSQL_DSN` 后 `go test -tags=integration -count=1 -v -run "Activity|Lantern|Reunion|Disband" ./internal/data`,输出不得有 SKIP;`go build -o ../../bin/go_services/guild.exe .`;`cd go/client_rpc_router; go build -o ../../bin/go_services/client_rpc_router.exe .; go test -count=1 ./...`。
6. 部署顺序:路由服 → gate(重载 MessageLimiter)→ guild;启动日志无 `ValidateTables` 报错。

### B6a-cli
1. `mmorpg-client` 下 `pwsh tools/gen_messageids.ps1`,再跑 `client_compile_check.ps1`。
2. EditMode:`-runTests -testPlatform EditMode -testFilter "Guild"`(不带 `-quit`),`GuildClientTests`、`GuildWindowTests` 全过;编辑器锁定时 Roslyn 离线编译。
3. robot:`cd robot; go mod vendor; go build -mod=vendor -o robot.exe .; go vet -mod=vendor ./...`。
4. 冒烟:按本地开机 runbook 起全栈;执行 6.44 清理 SQL;`.\robot.exe -c etc/guild_smoke.yaml`(`trial: false`);须同时输出 `GUILD_SMOKE_OK` 与 `GUILD_SMOKE_ACTIVITIES_OK`;保留 robot 日志与 guild 日志 `guild_activity` 行。

### B6b-srv1
0. 前置同上;确认属性会话对 `battle_data.proto` 与其生成物的改动已提交或协调;**核对 Redis 同源**:battle 节点配置里 zone Redis 的 host/port/DB 与 `go/guild/etc/guild.yaml` 的 `PlayerLocatorRedis`(127.0.0.1:6379 DB 0)一致,不一致即停下报告。
1. proto-gen。通过:`go/proto/match/match_internal{.pb.go,_grpc.pb.go}` 存在;`cpp/generated/proto/match/match_internal.pb.cc` 已登记进 proto 工程与 CMake(否则照 trade_admin 手补);`rg -n "MatchInternal" cpp/nodes/gate` 不在 `IsClientMessageId`;路由表 `ClientProtocol: false`;battle、contracts/kafka 出现新字段。
2. C++:MSBuild `/m:1` 顺序 proto → grpc_client → battle lib → `cpp/nodes/battle/battle.vcxproj` → `turn_battle_engine_test`;`turn_battle_engine_test.exe --gtest_filter=BattleResultActivity*`,再全量回归;Linux CMake 构建 battle 节点。
3. match:`cd go/match; go build ./...; go vet ./...; go test -count=1 ./...`(重点 `ActivityBattle|Gather|InternalGuard`);`go build -o ../../bin/go_services/match.exe .`。

### B6b-srv2
1. `cd go/guild; go build ./...; go vet ./...; go test -count=1 ./internal/...`;集成 `-run "Trial|Owed|Poison"`,不得 SKIP。
2. 部署顺序:battle → match → 路由服与 gate → guild。guild 日志应有"历练结果消费者启动 topic=match-results group=guild-trial"(本地 `-Zone` 也**不带** `_zN` 后缀)。

### B6b-cli
1. 同 B6a-cli 1–3。
2. 冒烟 `trial: true`,S9–S15 通过;回归 `battle_smoke`(BATTLE_SMOKE_OK)与 features smoke。
3. `kafka-consumer-groups --bootstrap-server localhost:9092 --describe --group guild-trial` → LAG=0;`mmorpg_guild.guild_trial_battle` 中该 battle_id `state=2, settle_result=1`;SharedRedis `EXISTS battle:activity_result:<id>` → 0。
4. 故障注入(手工一次,验证持久化通道):S10 开战后、战斗结束前停 Kafka broker;战斗结束 60s 后再启动 Kafka。通过:battle 日志有 `battle_activity_result_resend`;8 分钟内(重发或巡检器兜底)该局 `state=2`,guild 日志有 `settled_win` 或 `sweeper_recovered`;SharedRedis 结果键已删除。

## 6.47 与并行会话的协调

- **组队会话**:`gather.go` 只新增 `gatherOptions`、`runGatherWithOptions`、`RunActivityGather`,`runGather` 签名与行为不变;`healOrphanQueuedTicket` 原样调用,若组队先抽成包级 `healOrphanTicket` 则改调它;活动票不设 `team_id`;`MatchInternal` 占一个消息号,proto-gen 与组队、交易会话串行。组队落码后"整队打历练"仍走 `StartGuildTrial`(6.16)。
- **属性会话**:`battle_data.proto` 只末尾追加;重生成的 `battle_data.pb.*` 与对方改动一起核对。
- **交易会话**:TRADE_* 流不受影响;`go/schemamigrate` 归交易会话。
- **聊天会话**:v1 无 GUILD 频道,活动通知不走聊天。
- **帮会其它批次**:B2 提供 `l.notify`、`RefreshQueued`/`DrainQueued`/`AcceptWrite`、`GuildRule` 表、合服闸门;B5 提供 outbox 插入、`EnableEconomy` 同款注入、`withSyncDelivery`、ACTIVITY_REWARD 的 Finalize 分支(只改状态并推 `DELIVERY_DONE`,不做对侧账)、`AssetReasonText`、`leaveAnyGuild`,并**改冒烟默认账号为 9214/9215**;B3b 提供 `GuildMember.name`。

## 6.48 待用户拍板(开工前)

- **U1 "当期 / 每期"指什么**。推荐(本稿默认):个人次数按游戏日;灯会点灯阈值、团圆锁存、灯会资金"每期一次"按**活动档期**;历练资金胜场上限按游戏日。另一选项"期 = 游戏日":只改 `GuildPeriodKey` 为恒返回 DayKey,删 I3b/I7b,界面"本期"改"今日",其余不变。开发配表 0/0 常开时两者行为相同,冒烟不受影响。
- **U2 历练阵亡者是否得奖**。推荐(本稿默认):与引擎发经验金币口径一致,**逃跑或阵亡都不得**帮贡与物品。另一选项"阵亡也算参与":`candidates` 不减 `dead_player_ids`,字段仍保留。

## 6.49 契约偏差(需主设计确认)

1. 新增 tip `GuildActivityJoinTooRecent` 与 `GuildRule.activity_join_min_hours`(开发 0,上线 24)。
2. **同道历练改为邀请确认制**:新增 RPC `RespondGuildTrialInvite{uint64 lobby_id; bool accept}`(进白名单、写限流 5/s)、视图 `GuildTrialLobbyView`、枚举 `GuildTrialLobbyState`、tip `GuildTrialInviteExpired/Declined/Cooldown`、`GuildRule.trial_invite_ttl_seconds`(30)与 `trial_invite_cooldown_seconds`(10);`StartGuildTrial` 语义由"开战"改为"建邀请房间"。原因:契约原写法允许任何成员把他人强拉进战斗。
3. 新增 match 内部服务 `MatchInternal.StartActivityBattle`;`BattleActivityContext`(含 `guild_period_key`)进 battle_data / battle_node / match_event;`BattleResultEvent` 追加 `fled_player_ids = 10`、`dead_player_ids = 11`。
4. 写 RPC 只回活动视图,客户端排队再拉 GuildInfo;如需一次往返可给响应加 `GuildInfo guild = 3`。
5. ACTIVITY_REWARD `deadline_ms=0`(永不中止),不同于契约"created + asset_op_deadline_seconds";背包满保持 PENDING 自动补发;仅永久拒绝(封禁、非法包)为 REJECTED,不补发、帮贡不回收。
6. 视图奖励物品用 `GuildRewardItem{item_id, count}`,guild.proto 不 import asset_op.proto。
7. `guild_activity_progress` 计数列命名 `progress_count`(S6 自定义表,灯会与历练共用)。
8. `GUILD_TRIAL_BATTLE_STATE_EXPIRED` 由巡检器写;EXPIRED 不是幂等终态,迟到结果仍可结算。
9. "当期"按 U1。
10. 历练发奖名单 = team 0 − 逃跑 − 阵亡(U2)− 结算时已不在帮 − 当日次数已满;发起人确认开战时须有剩余次数。
11. B6a、B6b 拆成 B6a-srv / B6a-cli / B6b-srv1 / B6b-srv2 / B6b-cli,各自授权。
12. Kafka 结果保留 7 天,SharedRedis 结果记录 TTL 7 天;guild 停机超过 7 天的胜场不补发。
13. guild 在 K8s 无 manifest(D-14 遗留),ConfigMap 须带 `MatchRpc`、`Activity`,`PlayerLocatorRedis` 必须指 SharedRedis;`MatchInternal` v1 只靠 NetworkPolicy 收口,v1.1 把 `callerauth` 提到 `go/shared` 后要求签名。
14. 活动事务依赖 DSN 不开 `clientFoundRows`,由 `config.Validate` 断言。
15. 新增第 10 张表 `guild_trial_reward_owed`,锁序位置 10(S1 §2.1 追加)。
16. 新增 tip `GuildTrialServiceBusy`(fault=1):match 不可用不回 gRPC 错误,避免客户端 `RequiresReconnect`。
17. 跨服务键契约 `battle:activity_result:{battle_id}`:battle 写、guild 删;要求 battle 的 zone Redis 与 guild `PlayerLocatorRedis` 同源。
18. robot 账号分配写进契约 §6:B2 9211–9213、B5 9214–9215、B6 9216–9219;S5 改默认值。
19. 无会话身份统一回 gRPC `PermissionDenied`(写进契约 §2)。
20. `GuildActivity` 同 type 启用行时间窗不得重叠;写 RPC 只接受该类型当前选中行。
21. 多玩家结算按 `(player_id, 表位置)` 全序交错锁 `guild_player_op_seq`/`guild_asset_op`,写进 S1 规则 2 注释。
22. 活动事务重试用 `withActivityTxRetry`(1213/1205 各最多 2 次),不同于 S1 规则 5 的 `withTxRetry`(1213 一次)。

---

## 附录:对抗评审处理记录

# S6 帮会活动 — 评审处理记录(2026-09-16)

结论:19 条中 17 条采纳、2 条部分采纳、0 条驳回。修订后 S6 由原 8 部分改为 11 部分(`s6_activities_part1.md` … `part11.md`),章节号整体重排。另外自查补了 1 处评审未提的缺口(见末尾)。

| # | 级别 | 问题 | 处理 | 核验与落点 |
|---|---|---|---|---|
| 1 | 阻断 | 历练无同意环节,可强拉他人进战斗 | **已采纳** | 核实 `gather.go:160-166` 先停观战再逐人冻结;契约原文只说"组队打副本"。改为邀请确认制:`StartGuildTrial` 只建 Redis 房间(TTL 30s),新增 `RespondGuildTrialInvite{lobby_id, accept}`,全员同意的那次调用才调 match;另加每人同时一个有效房间、发起人建房冷却 10s、`busy` 原因。Lua 脚本、状态机、崩溃窗口 L1–L8 见 part7 §6.23–6.27;客户端邀请框见 part9;契约偏差 2 |
| 2 | 重要 | 结果事件至多一次,奖励可能永久丢失 | **已采纳** | 核实 `battle_room_manager.cpp:287-296` 失败只打日志,个人结算走 R07(`:1406-1600`)。R07 发件箱由 scene 销账,不能直接复用,改为同纪律的新通道:battle 先写 SharedRedis `battle:activity_result:{battle_id}`(7 天)再发 Kafka,每 10s 查键、仍在则重发(≤30 次);guild 结算进入终态后 DEL 销账;guild 巡检器扫超时 STARTED 行,有记录就直接结算,无记录超 3600s 置 EXPIRED 并有 overdue 告警。part6 §6.19–6.21、part8 §6.33;契约偏差 17 |
| 3 | 重要 | 待发满 16 条时跳过物品,违反 fail-closed | **已采纳**(方案 a) | 核实 `DefaultLimits.MaxPending=16`、商店同流。新增表 `guild_trial_reward_owed`(锁序位置 10),结算事务内写入;`OwedRewardLoop` 每 10s 等窗口有空位转成 op 行并同事务删除。单人 RPC 仍回 `GuildAssetPending`(未提交任何东西,可重试)。集成用例 I21。part2 §6.5、part8 §6.30 i 步与 §6.32;契约偏差 15 |
| 4 | 重要 | 背包满在 S6 被当成 REJECTED,与 S4/S5 的 RETRY 矛盾 | **已采纳** | 核实 S4 §4.10 背包满 = RETRY 不记账、S5 PENDING reason=27001。C6/C7 重写;视图加 `my_pending_reason_tip_id`(最早待发行的 `last_reason`),客户端复用 `AssetReasonText`;REJECTED 仅留给封禁 / 非法包。part3 §6.8–6.9、part4 §6.13、part9 |
| 5 | 重要 | 同步投递与调 match 未扣已用时间,可能超时触发重登 | **已采纳** | 核实 `home_zone.go:28` 1500ms、`GuildClient.cs:199-206`。复用 B5 的 `SyncBudget 2500 / HandlerBudget 3500` 与 `withSyncDelivery`,<300ms 不投;match 预算 `min(1500, 剩余−500)`,<300ms 回 ServiceBusy。单测 `TestLanternSyncBudget`、`TestLaunchMatchBudget`。part3 §6.6 |
| 6 | 重要 | "当期"被一律当游戏日 | **已采纳**(列为待拍板 U1,按推荐默认写) | 个人次数按游戏日;帮会进度按档期 `GuildPeriodKey`(start_at_ms 当天键,0/0 退化为当天);历练资金胜场上限按游戏日。`BattleActivityContext` 增 `guild_period_key`,视图增 `guild_period_key`,文案"本期";新增 I3b、I7b;另一选项只需改一个函数。part1 §6.0/§6.3、part11 §6.48 |
| 7 | 重要 | 客户端刷新做法与 B2 队列纪律冲突 | **已采纳** | 核实 s2 §17.2–17.4、`GuildUiRoot.cs:102-113`、`GuildClient.cs:189`。新增 `ActivitiesQueued`,并入 `DrainQueued(applicantsVisible, activitiesVisible)`;推送、写成功、首次进页都只置标志;写成功需要帮贡时置 `RefreshQueued` 并用 `_pendingNotice` 保留提示;NotInGuild 分支清 `_pendingNotice`。EditMode 用例 2–4、11。part9 §6.37–6.39 |
| 8 | 重要 | "进行中"按 created_ms 保留 420s,gather 失败后发起人被锁 7 分钟 | **已采纳** | 删除 `LoadMyStartedTrial` 时间窗查询;改为读 SharedRedis `battle:lock:{pid}`,锁值对应的历练行仍为 STARTED 才回填 `my_trial_battle_id`。T2 更新;单测 `TestMyTrialBattleIDFromLock`。part3 §6.8 第 6 步 |
| 9 | 重要 | robot 账号与 B5 重叠 | **已采纳**(部分需 S5 执行) | 核实 s5 §5.36 默认 9217/9218、s2 §24.1。定为 B2 9211–9213、B5 9214–9215、B6 9216–9219;活动前置先 `leaveAnyGuild`;头注释补 zone_a 建角要求。S5 默认值须由 S5 修订者改(本角色只能改 S6 文件)。part10 §6.44;契约偏差 18 |
| 10 | 次要 | 同类型多行同时开放可重复领奖 | **已采纳** | `validateRows` 加同 type 启用行时间窗不重叠(0/0 视为全时段);写 RPC 用 `PickForWrite` 只接受该类型当前选中行。C15、`TestValidateRows`、`TestSelectVisibleAndPickForWrite`。part1 §6.2.4/§6.3 |
| 11 | 次要 | 确定性错误会让分区永久卡住 | **已采纳** | handler 区分暂时性 / 确定性;确定性错误(溢出、奖励包构建失败、编码失败)单表写 `SETTLED + POISON`、销账、提交 offset;表增 `settle_result` 列。I23、消费者用例 4。part8 §6.28、§6.31 |
| 12 | 次要 | match 故障回 gRPC 错误导致客户端重登 | **已采纳** | 新增 tip `GuildTrialServiceBusy`(fault=1);match 出错、超时、INTERNAL、预算不足统一回它;gRPC 错误只留给 guild 自身 MySQL/Redis。part3 §6.10、part7 §6.26;契约偏差 16 |
| 13 | 次要 | 本地多 zone 时消费组被加 `_zN` 后缀 | **已采纳** | 核实 `go_services.ps1:381-388` 只匹配带引号的 GroupID。yaml 改为不带引号 `GroupID: guild-trial` 并写注释;验证只查 `guild-trial` 一个组。part7 §6.22、part11 §6.46。(评审引用的 `match_service.yaml:95-99` 实际无 GroupID 行,不影响结论) |
| 14 | 次要 | MatchInternal 调用方不验签 | **部分采纳** | 核实 `callerauth` 是 login 的 internal 包,他服不能 import;match 现有全部方法与 `match-results` topic 对内网同样不验签,属同一信任边界。v1:拦截器拒带会话 metadata + K8s NetworkPolicy 只放行 gate 路由服与 guild;v1.1 把 callerauth 提到 `go/shared` 后要求签名。未在本批做 HMAC:需要移包、密钥分发并改 guild 调用方,超出本批范围。part5 §6.18.3;契约偏差 13 |
| 15 | 次要 | 重复实现 NextReset | **已采纳** | 核实 s5 §5.13 已有 `NextDailyReset`。`activity.NextResetMs` 直接调用它;删 B6a 原第 11–12 项 gameday 改动与原偏差 2;B6a-srv 名额改给 `tables.go`。part1 §6.3、part11 §6.45 |
| 16 | 次要 | 视图引用 ItemGrant,把资产通道 schema 带进客户端 | **已采纳** | 定义 `GuildRewardItem{item_id, count}`;删 import 与客户端 `gen_proto.ps1` 改动。part2 §6.4、part9 §6.36;契约偏差 6 |
| 17 | 次要 | 待发查询用不上索引且可能漏算 | **已采纳** | 拆两条都走 `idx_guild_asset_op_3`:待发 `status=?P AND kind=?K ORDER BY seq`(≤16 行);拒绝 `status=?R ORDER BY seq DESC LIMIT 4` 后内存过滤 kind 与 24h(用 Finalize 写入的 `next_attempt_ms` 判龄,不依赖 created_ms);另加 owed 表 PK 前缀计数。part3 §6.8 第 7 步 |
| 18 | 次要 | 无会话错误码与 S5 不一致;Brokers 为空时 Validate 拒启与正文矛盾 | **已采纳** | 统一 `PermissionDenied`;`Validate` 不再因 Brokers 为空拒启,改 `TrialResultActive()`,为假时打 WARN 并把历练视为未开放(无结算不许开战)。part2 §6.4、part3 §6.7、part7 §6.22;契约偏差 19 |
| 19 | 次要 | 阵亡者也得奖,与引擎口径不一致 | **部分采纳**(列为待拍板 U2) | 核实 `turn_battle_engine.cpp:1388-1389` 只奖"未逃跑且存活"、`BattleSettlementData.is_dead` 存在。`BattleResultEvent` 追加 `dead_player_ids = 11`(C++ 回显),默认结算时排除阵亡者;若用户选"阵亡也算参与",只改候选过滤一行。part5 §6.17.3、part6 §6.20、part8 §6.29 第 4 步、part11 U2 |

## 自查补充(评审未提)

- **S6 缺表定义**:S1 修订(`s1_storage_part7.md:94,108`)已把 `guild_activity_progress`、`guild_trial_battle` 的列与索引交给 B6a/B6b 自行定义,原 S6 没写 message、批次清单也没列 `guild_db.proto`/`tables.go`。已在 part2 §6.5 补全三张表与两个枚举(含 `idx (state, created_ms)` 供巡检器),并把计数列定名 `progress_count`(关闭原偏差 7),批次清单补入。
- **导表 FK 预案删除**:核实 `foreign_key.py:13` 对 0 值跳过,原 Codex 验证里"FK=0 报错则去掉 cfg_fk"的预案删去。
- **原偏差 6(outbox 状态数值)关闭**:S4 E1 与 S5 §5.6 已统一为 1 PENDING … 4 ABORTED,S6 SQL 一律绑定 `assetop.Status*` 常量。
- **多玩家结算的锁序交错**:按 `(player_id, 表位置)` 全序加锁并给出不成环理由(part2 §6.5 末尾;契约偏差 21)。
