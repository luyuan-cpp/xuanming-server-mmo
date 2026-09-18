# S2 管理与审批 + 推送(B2s/B2c)
> 本节由 14 个分部合并而成(原分部名保留在小标题里),另附对抗评审处理记录。

<!-- s2_management_part1.md -->

# S2 管理与审批 + 推送(批次 B2)— 第 1 部分:范围、现状、角色与权限、配表、Tip

> 状态:设计稿,未落码、未编译,待 Codex 验证。遵守 `00_contract.md`;依赖 B1(`mmorpg_guild` 库、`guild_db.proto`、`GuildData.Funds`、`MemberData.ContributionTotal/Balance`、`resetGuildSchemaViaMigrate`、全库锁序 §2.1)。行号为 2026-09-15/16 核对值,并行会话在改同一工作树,落码按函数名重新定位。

## 0. 范围与结论

B2 交付:
1. **成员管理**:`SetGuildMemberRole`(帮主任免长老,长老上限取 `GuildLevel.max_officers`)、`KickGuildMember`(按 rank 踢人)、`TransferGuildLeader`(原帮主降为长老,满额则降为成员)、`LeaveGuild` 改为加锁事务(帮主不能退)。
2. **申请制入帮**:删除 `JoinGuild`;新增 `ApplyJoinGuild / CancelGuildApplication / ListMyGuildApplications / ListGuildApplications / ReviewGuildApplication`。72h 过期惰性清理,每人 ≤3 个待审,每帮 ≤50 个待审;**审批通过时复核申请人归属 zone**;通过即删申请人全部申请,**建帮即删建帮者全部申请**,解散即删该帮全部申请。
2a. **一致性加固**(评审采纳):写事务锁等待封顶 1s(DSN `innodb_lock_wait_timeout=1`);死锁重试 3 次,仍失败回业务 tip `kGuildBusyRetry`(不再回 gRPC Aborted);缓存失效失败异步有界重试;陈旧 `player_guild` 映射在写路径与客户端 `GetPlayerGuild` 上用 MySQL 自愈(第 3b 部分)。
3. **合服闸门**:所有写 RPC(含建帮、退帮、解散、公告、撤回)查 `merge:in_progress:{zone}`,命中或读失败都回 tip `kGuildZoneMerging`,取代现有 gRPC FailedPrecondition。
4. **推送**:`NotifyGuildChanged` 占位 RPC,至多一次,经 `kafkautil.PushToPlayer/BroadcastToPlayers`;Kafka writer 为 nil 时退化为 no-op。客户端收到后只触发拉取。
5. **协议扩展**:`GuildMember / GuildInfo / GuildRankEntry` 按契约 §3.1 加字段(B1 推迟到本批,见 s1 第 5 部分偏差 3)。名字字段本批恒空,由 B3b 填。
6. **配表**:新建 `GuildRule`、`GuildLevel` 两张表(契约 §5 全列一次建齐;B5/B6 可从字段号 8 起追加列,不得改 1–7 号)。默认行采用 B5 §5.9 的 10 级数值(第 1 级 30 人),两表的结构校验只有 B2 的 `validateGuildTables` 一份。建帮人数上限改读 `GuildLevel[1].max_members`。
7. **客户端**:`GuildClient` 新方法与推送注册;`GuildWindow` 成员行操作按钮、申请列表子视图、排行页“申请/撤回”;`GuildUiRoot` 接线;EditMode 测试。
8. **robot**:`guild_smoke` 现有第 4、6 步改为申请制;新增管理段,账号 `robot_9211`–`robot_9213`。

**不在 B2**:名字取值(B3b)、资金/帮贡写入(B5)、活动(B6)。`SetAnnouncementResponse` 在本批增加 `GuildInfo guild = 2`(契约 §2“写 RPC 回带 GuildInfo”,见第 2 部分 §5.3)。

## 1. 现状证据(已核对)

| 事实 | 位置 |
|---|---|
| GuildService 10 个 RPC,`OptionIsClientProtocolService=true`,无管理类 RPC | `proto/guild/guild.proto` service 段 |
| `GuildMember.contribution=5`,`join_time_ms/last_active_ms` 为 int64 | `guild.proto:11-18` |
| 入帮即时:`JoinGuild → AddMemberInZone`(锁 guild 行、判 zone、COUNT 判满、INSERT) | `guild_logic.go:311-347`;`guild_repo.go:303-360` |
| `RemoveMember` 为无锁 DELETE;`LeaveGuild` 用缓存 `LeaderID` 判帮主 | `guild_repo.go:362-372`;`guild_logic.go:349-386` |
| 授权写模板:锁 guild 行 → 锁 member 行 → 复核 role → 写 → 提交 → 失效缓存 | `guild_repo.go:374-418` |
| 解散:锁 guild → 锁全部 member → DELETE member → DELETE guild,返回权威 zone | `guild_repo.go:527-589` |
| 缓存失效 Lua(INCR generation + DEL),读路径按 generation 回填 | `guild_repo.go:115-127,420-440` |
| 合服闸门只在 CreateGuild,返回 FailedPrecondition | `guild_logic.go:136-140,213-229`;`merge_fence_test.go:35-66` |
| `ClientMethods` 白名单 9 个;`TestClientMethodsCoverEveryRPCExceptScoreWrites` 强制每个 RPC 表态 | `session.go:42-52`;`session_test.go:99-106` |
| `NewGuildLogic(repo, ids, onlineResolver, mergeFence, homeZones)` 被 16 处测试调用 | `client_zone_test.go`、`merge_fence_test.go`、`guild_id_mint_test.go` |
| `inband_observability_test.go` 使用 `pb.JoinGuildRequest/Response` | `server/inband_observability_test.go:62-90` |
| svcCtx 已有 `KafkaWriter`(Brokers 空时为 nil)、`GateCommandBuilder`、`PlayerLocatorRedisClient`;writer 未设 BatchTimeout/WriteTimeout/MaxAttempts | `svc/servicecontext.go:77-105` |
| `kafkautil.PushToPlayer` 不判 nil writer;空 `GateInstanceID` fail-closed;`BroadcastToPlayers` 按 (gate_id, instance) 分组 | `go/shared/kafkautil/gate_push.go:50-130` |
| match 推送模板:读 `player:session:{id}`,仅 ONLINE 推 | `go/match/internal/logic/push.go:26-83` |
| guild 无 metrics 包;仓库惯例用 `go-zero/core/metric.NewCounterVec` | `go/login/internal/logic/pkg/callerauth/metrics.go` |
| Tip.xlsx `//guild_error` 组 13 行,A 列名不带 `k`(如 `GuildHomeZoneUnknown`),下一组 `//friend_error` | `data/tip/Tip.xlsx` 第 160-173 行(openpyxl 读) |
| MessageLimiter.xlsx:`id, max_requests, time_window, tip_message`,guild 读 10/1、写 5/1、tip 1000 | `data/MessageLimiter.xlsx` |
| message_id.txt 当前最大 200(聚宝斋 196-200) | `proto/message_id.txt` |
| 单行规则表范式 PetRule(id 固定 1);C++ 手工登记 CMakeLists/vcxproj | `data/schema/petrule_table.proto`;`cpp/generated/table/CMakeLists.txt:78,127`;`table.vcxproj:73-74,113,170,202` |
| 客户端 `GuildClient`:单请求在途、`Request<T>`、`Accept` tip 映射,无推送注册 | `mmorpg-client/Assets/Scripts/Game/Guild/GuildClient.cs:1-218` |
| `GuildWindow` 成员行 `(0,86+i*86,1508,78)`,排行行 `JoinGuild_{id}` 按钮 `(1280,y+7,210,64)` | `GuildWindow.cs:226-287` |
| `GuildUiVerification.cs:184` 用 `Contribution =`,字段删除后必须改 | `Assets/Editor/Guild/GuildUiVerification.cs` |
| robot 第 4 步 C `JoinGuild` 断言 NotFound、第 6 步 B `JoinGuild`;`guildSmokeIsGuildMessage` 硬编码 10 个 id | `robot/guild_smoke_scenario.go:263-292,567-582` |

## 2. 角色、Rank 与权限矩阵

### 2.1 `constants.Rank`(`go/guild/internal/constants/constants.go`,在 role 常量块之后)

```go
// Rank 把持久化的 role 编码(D-4:0 成员 / 1 长老 / 3 帮主,2 跳过)映射成可比较的职位高低。
// 所有权限判断一律比 Rank,不比 role 原值:原值不连续,`role >= 1` 这种写法会把 2(未启用)也放进去。
// 未知编码返回 RankNone,任何权限判断都不通过(fail-closed)。
const (
	RankNone    = 0
	RankMember  = 1
	RankOfficer = 2
	RankLeader  = 3
)

func Rank(role uint32) int {
	switch role {
	case RoleMember:
		return RankMember
	case RoleOfficer:
		return RankOfficer
	case RoleLeader:
		return RankLeader
	default:
		return RankNone
	}
}

// AssignableRole:SetGuildMemberRole 只能设 0 / 1;帮主只能经 TransferGuildLeader 产生。
func AssignableRole(role uint32) bool { return role == RoleMember || role == RoleOfficer }
```

### 2.2 权限判定(纯函数,放 `internal/data/guild_manage_repo.go`,事务内调用,单测覆盖)

```go
func canAssignRole(actorRole uint32) bool          { return constants.Rank(actorRole) == constants.RankLeader }
func canTransferLeader(actorRole uint32) bool      { return constants.Rank(actorRole) == constants.RankLeader }
func canReviewApplications(actorRole uint32) bool  { return constants.Rank(actorRole) >= constants.RankOfficer }
// canKick:职位必须严格高于对方,且至少是长老。帮主可踢长老与成员;长老只能踢成员;没人能踢帮主。
func canKick(actorRole, targetRole uint32) bool {
	a, t := constants.Rank(actorRole), constants.Rank(targetRole)
	return a >= constants.RankOfficer && t != constants.RankNone && a > t
}
```

现有 `canSetAnnouncement(role)`(`guild_repo.go:416`)改为 `return constants.Rank(role) >= constants.RankOfficer`,语义不变(`guild_repo_test.go` 的 role 2/4 → false 用例仍成立)。

| 操作 | 成员 | 长老 | 帮主 | 失败 tip |
|---|---|---|---|---|
| 任免长老(目标 role 0↔1) | ✗ | ✗ | ✓ | `kGuildRankTooLow` |
| 踢成员 | ✗ | ✓ | ✓ | `kGuildRankTooLow` |
| 踢长老 | ✗ | ✗ | ✓ | `kGuildRankTooLow` |
| 转让帮主 | ✗ | ✗ | ✓ | `kGuildRankTooLow` |
| 查看/审批申请 | ✗ | ✓ | ✓ | `kGuildRankTooLow` |
| 退帮 | ✓ | ✓ | ✗(先转让或解散) | `kGuildLeaderCantLeave` |
| 解散 | ✗ | ✗ | ✓ | `kGuildNotLeader`(沿用) |
| 对自己执行任免/踢/转让/审批 | — | — | — | `kGuildCannotTargetSelf` |

### 2.3 长老上限

`max_officers = GuildLevel[guild.level].max_officers`,`guild.level` 在事务里从 `FOR UPDATE` 锁住的 guild 行读取。`officer_count = SELECT COUNT(*) FROM guild_member WHERE guild_id=? AND role=1`,在同一把 guild 行锁下读:所有改 role / 增删成员的事务都先锁 guild 行,所以计数不会与并发写交错。

- 任命:`officer_count >= max_officers` → `kGuildOfficerLimit`。
- 配表下调上限使现有长老超额:不强制降级;后续任命一律拒绝,转让时原帮主降为成员。
- 等级行在表中缺失:返回 `ErrGuildLevelConfigMissing` → gRPC `Internal`(启动校验已拦截,运行期出现即配置热改错误,fail-closed)。

## 3. 配表(契约 §5,本批新建)

### 3.1 `data/schema/guildrule_table.proto`(新建)

```proto
syntax = "proto3";

// GuildRule 表的权威 schema。源表 data/GuildRule.xlsx。本文件不参与 protoc 编译,导表器文本解析。
// 字段号只增不改、删字段用 reserved(data/AGENTS.md)。帮会服务的单行全局规则(与 PetRule 同套路:只取 id=1)。
// B2 建表并读取 1-4 号字段;5-6 号由 B5(资产通道)读取,7 号由 B6a(中秋团圆)读取。
// 1-7 号字段号与含义冻结;B5/B6 只能从 8 号起追加列(如 B6a 的 activity_join_min_hours = 8)。
// 每列默认值由读取该列的批次拍板:2-4 列 B2、5-6 列 B5、7 列及以后 B6。
package mmorpg.cfgtable.v1;

import "cfg_options.proto";

message GuildRuleTable {
  option (cfg_sheet)       = "GuildRule";
  option (cfg_source_file) = "GuildRule.xlsx";
  option (cfg_primary_key) = "id";

  // 规则行 id(固定 1)
  uint32 id = 1;
  // 入帮申请有效期(小时),1–720
  uint32 application_expire_hours = 2;
  // 每个玩家同时待审的申请上限,1–10
  uint32 max_pending_applications_per_player = 3;
  // 每个帮会同时待审的申请上限,1–500
  uint32 max_pending_applications_per_guild = 4;
  // 资产操作截止秒数(B5)
  uint32 asset_op_deadline_seconds = 5;
  // 资产操作重投基准毫秒(B5)
  uint32 asset_op_retry_base_ms = 6;
  // 中秋团圆在线人数兜底阈值(B6a)
  uint32 reunion_min_online_members = 7;
}
```

`data/GuildRule.xlsx`:工作表名 `GuildRule`;第 1 行列名 `id, application_expire_hours, max_pending_applications_per_player, max_pending_applications_per_guild, asset_op_deadline_seconds, asset_op_retry_base_ms, reunion_min_online_members`;第 2–5 行留空(第 5 行由导表器按 schema 注释回填);第 6 行数据:`1, 72, 3, 50, 300, 1000, 3`。

取值依据(按列归属拍板,见 schema 注释):`asset_op_deadline_seconds=300`、`asset_op_retry_base_ms=1000` 取 B5 §5.9;`reunion_min_online_members=3` 取 B6a §6.2.3(B5 §5.9 表中的 5 作废,以列归属批次为准)。B6a 追加第 8 列时在同一行补值,不改前 7 列。

### 3.2 `data/schema/guildlevel_table.proto`(新建)

```proto
syntax = "proto3";

// GuildLevel 表的权威 schema。源表 data/GuildLevel.xlsx。id = 帮会等级,从 1 连续。
// B2 读 max_members(建帮)与 max_officers(任免 / 转让);B5 读 upgrade_cost_funds 并按 max_members 改写 guild.max_members。
package mmorpg.cfgtable.v1;

import "cfg_options.proto";

message GuildLevelTable {
  option (cfg_sheet)       = "GuildLevel";
  option (cfg_source_file) = "GuildLevel.xlsx";
  option (cfg_primary_key) = "id";

  // 帮会等级(从 1 连续)
  uint32 id = 1;
  // 该等级成员上限(≥2,≤100;100 是推送 MGET 与单帮快照预算实测前提)
  uint32 max_members = 2;
  // 该等级长老上限(< max_members)
  uint32 max_officers = 3;
  // 升到下一级所需帮会资金;0 = 满级(只允许最后一级为 0)
  uint64 upgrade_cost_funds = 4;
}
```

`data/GuildLevel.xlsx` 第 1 行 `id, max_members, max_officers, upgrade_cost_funds`,第 6–15 行(与 B5 §5.9 逐格一致,B2 落表即为最终默认行,B5 不再改):

| id | max_members | max_officers | upgrade_cost_funds |
|---|---|---|---|
| 1 | 30 | 2 | 20000 |
| 2 | 35 | 2 | 50000 |
| 3 | 40 | 3 | 100000 |
| 4 | 45 | 3 | 180000 |
| 5 | 50 | 4 | 300000 |
| 6 | 60 | 4 | 460000 |
| 7 | 70 | 5 | 680000 |
| 8 | 80 | 5 | 960000 |
| 9 | 90 | 6 | 1300000 |
| 10 | 100 | 6 | 0 |

行为变化:建帮上限由 `DefaultMaxMembers=50` 变为第 1 级的 30(未上线,无存量帮会)。robot `guild_smoke` 同帮最多 6 人,不受影响;第 1 级长老上限 2,robot M5–M8 最多同时 2 名长老,不受影响。

### 3.3 启动校验(fail-closed,`logic/guild_manage_logic.go` 的 `ValidateGuildTables`)

`guild.go` 在 `svc.NewServiceContext` 之后、`node.NewNode` 之前调用,失败则 `logx.Must`。校验内容:
- `GuildRuleTableManagerInstance.FindById(1)` 必须存在;`application_expire_hours ∈ [1,720]`;`max_pending_applications_per_player ∈ [1,10]`;`max_pending_applications_per_guild ∈ [1,500]`。
- `GuildLevelTableManagerInstance.FindAll()` 非空;id 从 1 连续无缺口;每行 `2 ≤ max_members ≤ MaxGuildMembersCap(100)`、`max_officers < max_members`;`max_members`、`max_officers` 随等级非递减;`upgrade_cost_funds == 0` 当且仅当该行是最后一级。

纯函数 `validateGuildTables(rule *tablepb.GuildRuleTable, levels []*tablepb.GuildLevelTable) error` 承载规则,`ValidateGuildTables()` 只负责取表后调用它,便于单测。`constants.MaxGuildMembersCap = 100`(推送 §15.2 与 `GuildInfo` 快照的预算前提,改大须重做预算)。

**唯一性约定**:GuildRule / GuildLevel 的结构与 1–4 列规则**只在** `validateGuildTables` 写一份。B5 的 `ValidateEconomyTables`(同在 `logic` 包)删去其第 2 条 GuildLevel 规则,改为先调用 `validateGuildTables(t.Rule, t.Levels)`,自己只补 `asset_op_deadline_seconds ∈ [30,86400]`、`asset_op_retry_base_ms ∈ [100,60000]` 与 Donate/Shop 规则;B6 的 `activity.ValidateTables` 只校验自己追加的列。三处上限不一致时一律以本节为准(见第 9b 部分偏差 2)。

### 3.4 C++ 构建登记(导表器不改构建文件)

- `cpp/generated/table/CMakeLists.txt`:SOURCE_FILES 中,`code/guildlevel_table.cpp`、`code/guildrule_table.cpp` 按字母序插入 `code/` 段;`proto/guildlevel_table.pb.cc`、`proto/guildrule_table.pb.cc` 插入 `proto/` 段。两表无外键,不生成 `_fk.cpp`。
- `table.vcxproj`:ClInclude 增 `code\guildlevel_table.h`、`code\guildlevel_table_comp.h`、`code\guildrule_table.h`、`code\guildrule_table_comp.h`、`proto\guildlevel_table.pb.h`、`proto\guildrule_table.pb.h`;ClCompile 增 `code\guildlevel_table.cpp`、`code\guildrule_table.cpp`、`proto\guildlevel_table.pb.cc`、`proto\guildrule_table.pb.cc`。
- `table.vcxproj.filters`:上述 10 项按现有 `code` / `proto` 过滤器登记。
- **落码前以 `dev.bat gen` 实际生成的文件名为准**。若 Codex 发现 `_comp.h` 未生成,删除对应条目。已存在的 ActivitySchedule CMake 缺口不在本批修(由 C++ 表负责人另行处理,见第 9b 部分 §30)。

## 4. Tip 码(`data/tip/Tip.xlsx`,`//guild_error` 组)

在 `GuildHomeZoneUnknown` 行(当前第 173 行,1 起计)之后、`//friend_error` 组头之前**插入 9 行**,顺序固定。B 列文案,C 列 fault 留空(均为业务拒绝;`GuildBusyRetry` 是契约外新增,见第 9b 部分偏差 13):

| A 列 | B 列 |
|---|---|
| GuildZoneMerging | 区服合并维护中,帮会操作暂停,请稍后再试 |
| GuildTargetNotMember | 对方已不在本帮会 |
| GuildCannotTargetSelf | 不能对自己执行此操作 |
| GuildRankTooLow | 帮会职位不足,无法执行此操作 |
| GuildOfficerLimit | 长老人数已达当前帮会等级上限 |
| GuildApplicationNotFound | 入帮申请不存在或已失效 |
| GuildApplicationLimit | 同时进行中的入帮申请已达上限 |
| GuildApplicationQueueFull | 该帮会待审申请已满,请稍后再试 |
| GuildBusyRetry | 帮会操作繁忙,请稍后重试 |

契约 §4 其余 guild tip(`GuildFundsInsufficient` 起)由 B5/B6 在这 9 行之后追加。

`constants.go` 新增(接在 `ErrHomeZoneUnknown` 之后):

```go
	// ErrZoneMerging:归属 zone 正在合服维护(merge:in_progress:{zone} 存在或读不到),所有写操作暂停。
	ErrZoneMerging = uint32(table.GuildError_kGuildZoneMerging)
	// ErrTargetNotMember:操作目标不是本帮成员(可能刚退帮或被踢)。
	ErrTargetNotMember = uint32(table.GuildError_kGuildTargetNotMember)
	ErrCannotTargetSelf = uint32(table.GuildError_kGuildCannotTargetSelf)
	// ErrRankTooLow:MySQL 权威 role 的 Rank 不够(见 Rank)。
	ErrRankTooLow = uint32(table.GuildError_kGuildRankTooLow)
	ErrOfficerLimit = uint32(table.GuildError_kGuildOfficerLimit)
	// ErrApplicationNotFound:申请不存在、已过期、已被处理,或申请人已加入别的帮会。
	ErrApplicationNotFound = uint32(table.GuildError_kGuildApplicationNotFound)
	ErrApplicationLimit = uint32(table.GuildError_kGuildApplicationLimit)
	ErrApplicationQueueFull = uint32(table.GuildError_kGuildApplicationQueueFull)
	// ErrBusyRetry:写事务连续死锁 3 次或锁等待超时(1205)。业务 tip,客户端不进入重连隔离。
	ErrBusyRetry = uint32(table.GuildError_kGuildBusyRetry)
```

另加常量(同文件,非 tip):

```go
	// MaxGuildMembersCap:GuildLevel.max_members 的校验上限(推送与快照预算前提)。
	MaxGuildMembersCap = 100
```

同时删除 `constants.go:53-63` 那段“合服闸门刻意没有加码”的注释(已有码),删除 `DefaultMaxMembers`(改读 GuildLevel,见第 5 部分)。`constants_test.go` 的 `tipCodes()` 追加上述 9 项。

<!-- s2_management_part2.md -->

# S2 管理与审批 + 推送(B2)— 第 2 部分:`proto/guild/guild.proto` 变更

## 5. 协议

开发期删字段与删 RPC:须全量重编 C++ gate/router 生成物、全部 Go 服务、robot、客户端(AGENTS §4.3)。

### 5.1 import

文件头在现有两个 import 之后加:

```proto
import "proto/common/base/empty.proto";
```

### 5.2 基础消息(整体替换 `GuildMember`、`GuildInfo`、`GuildRankEntry`)

```proto
message GuildMember {
  uint64 player_id            = 1;
  uint32 role                 = 2;  // 0=成员 1=长老 3=帮主(2 不启用);权限比较一律走 go/guild constants.Rank
  int64  join_time_ms         = 3;
  int64  last_active_ms       = 4;
  reserved 5;                       // 原 contribution,2026-09 拆为 total / balance(开发期删除)
  bool   online               = 6;  // 是否在线(读 player:session,不入库)
  string name                 = 7;  // 展示名(不入库;B3b 起由 data_service BatchGetPlayerName 填,B2 恒空)
  uint64 contribution_total   = 8;  // 累计帮贡,只增(B5 起写入)
  uint64 contribution_balance = 9;  // 可消费帮贡(B5 起写入)
}

message GuildInfo {
  uint64 guild_id       = 1;
  string name           = 2;
  uint64 leader_id      = 3;
  uint32 level          = 4;
  string announcement   = 5;
  int64  create_time_ms = 6;
  uint32 max_members    = 7;
  repeated GuildMember members = 8;  // 按 player_id 升序(B1 起 ORDER BY player_id)
  uint32 zone_id        = 9;
  uint64 funds                      = 10;  // 帮会资金(B5 起写入)
  uint32 max_officers               = 11;  // GuildLevel[level].max_officers
  uint32 officer_count              = 12;  // members 中 role=1 的人数
  uint64 upgrade_cost_funds         = 13;  // GuildLevel[level].upgrade_cost_funds;0 = 满级
  string leader_name                = 14;  // 展示用(B3b 填,B2 恒空)
  uint32 pending_application_count  = 15;  // 未过期待审申请数;仅当请求者本人是该帮长老 / 帮主时非 0
}

message GuildRankEntry {
  uint64 guild_id       = 1;
  string name           = 2;
  uint64 leader_id      = 3;
  uint32 level          = 4;
  uint32 member_count   = 5;
  int64  score          = 6;
  uint32 rank           = 7;
  string leader_name    = 8;  // 展示用(B3b 填,B2 恒空)
}
```

`reserved 5` 与契约“删除”一致且更安全:防止后续批次误复用 5 号而与旧客户端二进制错位。

### 5.3 删除与存量消息改动

删除 `JoinGuildRequest`、`JoinGuildResponse` 两个 message 与 `rpc JoinGuild`。

`SetAnnouncementResponse` 整体替换为(契约 §2“所有写 RPC 成功时回带刷新后的 GuildInfo”):

```proto
message SetAnnouncementResponse {
  TipInfoMessage error_message = 1;
  GuildInfo guild = 2;  // 成功时为提交前事务内读到的权威快照(含新公告)
}
```

### 5.4 新增请求 / 响应与视图(放在 `SetAnnouncementResponse` 之后、`// ── Ranking` 之前)

```proto
// ── 成员管理(B2)────────────────────────────────────────────────
// 以下请求都不带 player_id:操作者一律取 gate 会话(无会话的内部调用被拒绝,见 go/guild logic)。

message SetGuildMemberRoleRequest {
  uint64 target_player_id = 1;
  uint32 role             = 2;  // 只允许 0(成员)/ 1(长老)
}
message SetGuildMemberRoleResponse {
  TipInfoMessage error_message = 1;
  GuildInfo guild = 2;  // 成功时为提交后的权威快照
}

message KickGuildMemberRequest {
  uint64 target_player_id = 1;
}
message KickGuildMemberResponse {
  TipInfoMessage error_message = 1;
  GuildInfo guild = 2;
}

message TransferGuildLeaderRequest {
  uint64 target_player_id = 1;
}
message TransferGuildLeaderResponse {
  TipInfoMessage error_message = 1;
  GuildInfo guild = 2;
}

// ── 入帮申请(B2)────────────────────────────────────────────────

message ApplyJoinGuildRequest {
  uint64 guild_id = 1;
}
message ApplyJoinGuildResponse {
  TipInfoMessage error_message = 1;  // 同帮重复申请 = 刷新有效期并成功
}

message CancelGuildApplicationRequest {
  uint64 guild_id = 1;
}
message CancelGuildApplicationResponse {
  TipInfoMessage error_message = 1;
}

// 申请人视角的一条待审申请。
message GuildApplicationView {
  uint64 guild_id     = 1;
  string guild_name   = 2;
  uint32 level        = 3;
  uint32 member_count = 4;
  uint32 max_members  = 5;
  uint64 leader_id    = 6;
  string leader_name  = 7;  // B3b 填,B2 恒空
  uint64 apply_ms     = 8;  // 最近一次申请 / 刷新时刻(服务端时钟,Unix 毫秒)
  uint64 expire_ms    = 9;  // 过期时刻(服务端时钟);客户端只用于展示
}

message ListMyGuildApplicationsRequest {}
message ListMyGuildApplicationsResponse {
  TipInfoMessage error_message = 1;
  repeated GuildApplicationView applications = 2;  // 按 apply_ms 降序、guild_id 升序
}

// 审批人视角的一位申请人。
message GuildApplicantView {
  uint64 player_id = 1;
  string name      = 2;  // B3b 填,B2 恒空
  bool   online    = 3;  // 读 player:session,不入库
  uint64 apply_ms  = 4;
  uint64 expire_ms = 5;
}

message ListGuildApplicationsRequest {}
message ListGuildApplicationsResponse {
  TipInfoMessage error_message = 1;
  repeated GuildApplicantView applicants = 2;  // 按 apply_ms 升序、player_id 升序,至多 GuildRule.max_pending_applications_per_guild 条
}

message ReviewGuildApplicationRequest {
  uint64 applicant_player_id = 1;
  bool   approve             = 2;
}
message ReviewGuildApplicationResponse {
  TipInfoMessage error_message = 1;
  GuildInfo guild = 2;  // 成功(通过或拒绝)时为审批人所在帮会的权威快照
}

// ── 推送(B2)────────────────────────────────────────────────────
// 至多一次;客户端收到只触发拉取(GetPlayerGuild 等),拉取结果才是真相。
// 枚举值带前缀:与 guild_db.proto 同在 package guildpb,避免包级重名(s1 第 5 部分偏差 5)。
enum GuildChangeKind {
  GUILD_CHANGE_KIND_UNSPECIFIED          = 0;
  GUILD_CHANGE_KIND_MEMBER_JOINED        = 1;
  GUILD_CHANGE_KIND_MEMBER_LEFT          = 2;
  GUILD_CHANGE_KIND_MEMBER_KICKED        = 3;
  GUILD_CHANGE_KIND_ROLE_CHANGED         = 4;
  GUILD_CHANGE_KIND_LEADER_TRANSFERRED   = 5;
  GUILD_CHANGE_KIND_DISBANDED            = 6;
  GUILD_CHANGE_KIND_APPLICATION_RECEIVED = 7;
  GUILD_CHANGE_KIND_APPLICATION_REJECTED = 8;
  GUILD_CHANGE_KIND_FUNDS_CHANGED        = 9;   // B5
  GUILD_CHANGE_KIND_LEVEL_UP             = 10;  // B5
  GUILD_CHANGE_KIND_ANNOUNCEMENT_CHANGED = 11;
  GUILD_CHANGE_KIND_ACTIVITY_CHANGED     = 12;  // B6
  GUILD_CHANGE_KIND_DELIVERY_DONE        = 13;  // B5
}

message GuildChangedS2C {
  GuildChangeKind kind   = 1;
  uint64 guild_id        = 2;
  uint64 actor_player_id = 3;  // 发起者;系统触发为 0
  uint64 target_player_id = 4; // 被操作者;无则 0
}
```

C# 生成名(protoc 去掉枚举名前缀):`GuildChangeKind.MemberJoined`、`GuildChangeKind.ApplicationRejected` 等。

### 5.5 service 段(整体替换 `service GuildService`)

```proto
service GuildService {
  option (OptionIsClientProtocolService) = true;

  rpc CreateGuild (CreateGuildRequest) returns (CreateGuildResponse);
  rpc GetGuild (GetGuildRequest) returns (GetGuildResponse);
  rpc GetPlayerGuild (GetPlayerGuildRequest) returns (GetPlayerGuildResponse);
  rpc LeaveGuild (LeaveGuildRequest) returns (LeaveGuildResponse);
  rpc DisbandGuild (DisbandGuildRequest) returns (DisbandGuildResponse);
  rpc SetAnnouncement (SetAnnouncementRequest) returns (SetAnnouncementResponse);

  // 成员管理(B2)
  rpc SetGuildMemberRole (SetGuildMemberRoleRequest) returns (SetGuildMemberRoleResponse);
  rpc KickGuildMember (KickGuildMemberRequest) returns (KickGuildMemberResponse);
  rpc TransferGuildLeader (TransferGuildLeaderRequest) returns (TransferGuildLeaderResponse);

  // 入帮申请(B2;取代直接入帮 JoinGuild)
  rpc ApplyJoinGuild (ApplyJoinGuildRequest) returns (ApplyJoinGuildResponse);
  rpc CancelGuildApplication (CancelGuildApplicationRequest) returns (CancelGuildApplicationResponse);
  rpc ListMyGuildApplications (ListMyGuildApplicationsRequest) returns (ListMyGuildApplicationsResponse);
  rpc ListGuildApplications (ListGuildApplicationsRequest) returns (ListGuildApplicationsResponse);
  rpc ReviewGuildApplication (ReviewGuildApplicationRequest) returns (ReviewGuildApplicationResponse);

  // 推送占位(B2):只为分配 message id;服务端实现恒返回 Empty。
  // **不进** go/guild session.ClientMethods —— 客户端发这个 id 会被 PermissionDenied。
  rpc NotifyGuildChanged (GuildChangedS2C) returns (Empty);

  // Ranking
  rpc UpdateGuildScore (UpdateGuildScoreRequest) returns (UpdateGuildScoreResponse);
  rpc GetGuildRank (GetGuildRankRequest) returns (GetGuildRankResponse);
  rpc GetGuildRankByGuild (GetGuildRankByGuildRequest) returns (GetGuildRankByGuildResponse);
}
```

### 5.6 生成与 id

- proto-gen 为 9 个新方法分配 message id(预计从 201 起,但**不得假定**:组队 / 聚宝斋会话可能先占号)。所有代码只引用生成常量,例如 `game.GuildServiceApplyJoinGuildMessageId`、`game.GuildServiceNotifyGuildChangedMessageId`。
- `JoinGuild` 的 19 号在 `message_id.txt` 中的去留由生成器决定(`service_register_info.go:148-214` 保留已有行、优先回填空号)。Codex 须记录 `git diff proto/message_id.txt`,不得手改,并记录 19 号是否被**任何**方法(含其它会话的新方法)占用。MessageLimiter 在 B2c 开工时按该记录填写(第 9b 部分 §28 B2c 第 1 步);B2s 与 B2c 之间新方法按限流器缺省行为运行(无专属行;缺省值以 gate 实现为准,robot 冒烟每个方法每秒 ≤2 次,不受影响)。
- 生成物(不手改):`go/proto/guild/guild.pb.go`、`guild_grpc.pb.go`;各服务 `generated/pb/game/message_id.go`;`cpp/generated/rpc/service_metadata/rpc_event_registry.{h,cpp}`;`go/client_rpc_router/generated/pb/game/route_table.go`;`robot/generated/pb/game/*`。
- 生成期检查 `kMaxRpcMethodCount`(critic.md 未决项):如生成器因方法数增长报错,由 Codex 报告,不自行改上限。

### 5.7 `guild_server.go` 薄委托(追加,删除 `JoinGuild`)

```go
func (s *GuildServer) SetGuildMemberRole(ctx context.Context, req *pb.SetGuildMemberRoleRequest) (*pb.SetGuildMemberRoleResponse, error) {
	return s.logic.SetGuildMemberRole(ctx, req)
}
func (s *GuildServer) KickGuildMember(ctx context.Context, req *pb.KickGuildMemberRequest) (*pb.KickGuildMemberResponse, error) {
	return s.logic.KickGuildMember(ctx, req)
}
func (s *GuildServer) TransferGuildLeader(ctx context.Context, req *pb.TransferGuildLeaderRequest) (*pb.TransferGuildLeaderResponse, error) {
	return s.logic.TransferGuildLeader(ctx, req)
}
func (s *GuildServer) ApplyJoinGuild(ctx context.Context, req *pb.ApplyJoinGuildRequest) (*pb.ApplyJoinGuildResponse, error) {
	return s.logic.ApplyJoinGuild(ctx, req)
}
func (s *GuildServer) CancelGuildApplication(ctx context.Context, req *pb.CancelGuildApplicationRequest) (*pb.CancelGuildApplicationResponse, error) {
	return s.logic.CancelGuildApplication(ctx, req)
}
func (s *GuildServer) ListMyGuildApplications(ctx context.Context, req *pb.ListMyGuildApplicationsRequest) (*pb.ListMyGuildApplicationsResponse, error) {
	return s.logic.ListMyGuildApplications(ctx, req)
}
func (s *GuildServer) ListGuildApplications(ctx context.Context, req *pb.ListGuildApplicationsRequest) (*pb.ListGuildApplicationsResponse, error) {
	return s.logic.ListGuildApplications(ctx, req)
}
func (s *GuildServer) ReviewGuildApplication(ctx context.Context, req *pb.ReviewGuildApplicationRequest) (*pb.ReviewGuildApplicationResponse, error) {
	return s.logic.ReviewGuildApplication(ctx, req)
}
// NotifyGuildChanged 只是推送的 message id 占位;客户端调用在 session 拦截器就被拒绝,内部调用也无意义。
func (s *GuildServer) NotifyGuildChanged(context.Context, *pb.GuildChangedS2C) (*base.Empty, error) {
	return &base.Empty{}, nil
}
```

`base` 为 `proto/common/base`(与 match `matchserviceserver.go:57-67` 同形)。

### 5.8 `session.go` 白名单

`ClientMethods` 删 `GuildService_JoinGuild_FullMethodName`,加 8 项:`SetGuildMemberRole`、`KickGuildMember`、`TransferGuildLeader`、`ApplyJoinGuild`、`CancelGuildApplication`、`ListMyGuildApplications`、`ListGuildApplications`、`ReviewGuildApplication` 的 `_FullMethodName`。**不加** `NotifyGuildChanged`。注释补一句:“Notify* 是推送占位,永远不进本表”。

`session_test.go:100`:`internalOnly := map[string]bool{"UpdateGuildScore": true, "NotifyGuildChanged": true}`,并新增:

```go
func TestNotifyPlaceholdersAreNeverClientCallable(t *testing.T) {
	for _, method := range pb.GuildService_ServiceDesc.Methods {
		if strings.HasPrefix(method.MethodName, "Notify") {
			_, allowed := ClientMethods["/"+pb.GuildService_ServiceDesc.ServiceName+"/"+method.MethodName]
			assert.False(t, allowed, "%s 是推送占位,不得进客户端白名单", method.MethodName)
		}
	}
}
```

<!-- s2_management_part3.md -->

# S2 管理与审批 + 推送(B2)— 第 3 部分:repo 事务基座与成员管理事务

新文件 `go/guild/internal/data/guild_manage_repo.go` 承载本部分、第 3b 部分与第 4 部分的全部新事务;`guild_repo.go` 的改动清单见第 9 部分 §26 第 19 项。

## 6. 事务基座

### 6.1 锁序(B1 §2.1 全库锁序的 B2 子集)

1. `guild`:单行 `SELECT ... FROM guild WHERE guild_id=? FOR UPDATE`(建帮是 INSERT 新行)。
2. `guild_member`:操作者与目标用**一条**语句加锁:`SELECT player_id, role FROM guild_member WHERE guild_id=? AND player_id IN (?,?) ORDER BY player_id FOR UPDATE`。InnoDB 按主键 `(guild_id, player_id)` 升序扫描加锁,满足“同表多行按主键升序”。只锁操作者时用单行等值查询。
3. `guild_application`。

任何 B2 事务都不在持有 `guild_application` 行锁之后再去锁 `guild` 或 `guild_member`。

### 6.2 事务助手

```go
// errCommitThen:事务函数要求“先提交已做的写,再把 inner 作为业务结果返回”(如删除过期申请后回 NotFound)。
type errCommitThen struct{ inner error }
func (e errCommitThen) Error() string { return e.inner.Error() }
func (e errCommitThen) Unwrap() error { return e.inner }

// maxTxAttempts:InnoDB 死锁(1213)时整事务已回滚,重跑安全(事务函数内没有非数据库副作用)。
// 锁等待超时(1205)不重试:锁等待已被 DSN 封顶 1s(§6.2a),再等只会耗尽 4s 预算。
const maxTxAttempts = 3

// ErrWriteConflict:死锁重试耗尽或锁等待超时;logic 映射为业务 tip kGuildBusyRetry(不是 gRPC 错误)。
var ErrWriteConflict = errors.New("guild write conflict (deadlock retries exhausted or lock wait timeout)")

func (r *GuildRepo) inTx(ctx context.Context, op string, fn func(tx *sql.Tx) error) error {
	return retryOnDeadlock(ctx, op, func() error { return r.runTxOnce(ctx, fn) })
}

// retryOnDeadlock 与数据库解耦,单测用假错误驱动。
func retryOnDeadlock(ctx context.Context, op string, run func() error) error {
	for attempt := 1; ; attempt++ {
		err := run()
		switch {
		case isLockWaitTimeout(err):
			guildTxLockWaitTimeoutTotal.Inc(op)
			return ErrWriteConflict
		case !isDeadlock(err):
			return err
		}
		guildTxDeadlockTotal.Inc(op)
		if attempt >= maxTxAttempts {
			logx.Errorf("[guild] %s: deadlock persisted after %d attempts: %v", op, attempt, err)
			return ErrWriteConflict
		}
		// 10–50ms 随机退避,错开两个互相回滚的事务再次撞上。
		backoff := 10*time.Millisecond + time.Duration(rand.Int64N(int64(40*time.Millisecond)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

func isDeadlock(err error) bool         { return mysqlErrNumber(err) == 1213 }
func isLockWaitTimeout(err error) bool  { return mysqlErrNumber(err) == 1205 }
func mysqlErrNumber(err error) uint16 {
	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) { return mysqlErr.Number }
	return 0
}
```

`runTxOnce` 与原稿相同(BeginTx → fn → `errCommitThen` 分支先提交再返回 inner → 否则 Commit;`defer tx.Rollback()`)。

**事务函数的结果变量约定(强制)**:`fn` 会被重跑,所有在事务里累积的结果(`MemberIDs`、`ReviewerIDs`、快照、`Changed`/`Inserted` 标志)**一律在 `fn` 内部声明**,`fn` 成功返回前才赋给外层变量:

```go
var out DisbandResult
err := r.inTx(ctx, "disband", func(tx *sql.Tx) error {
	var res DisbandResult // 每次尝试从零开始
	// ... 逐行 append 到 res.MemberIDs ...
	out = res             // 只在本次尝试成功路径的最后一步赋值
	return nil
})
```

`errCommitThen` 返回路径不赋值 `out`(logic 只看错误)。

指标(go-zero `core/metric`,无高基数 label):`guild_tx_deadlock_total{op}`、`guild_tx_lock_wait_timeout_total{op}`、`guild_cache_invalidate_failed_total{op}`(Help 文案同原稿;新增一项 Help:“帮会写事务锁等待超过 innodb_lock_wait_timeout 的次数”)。`op` 取值固定:`create`、`set_role`、`kick`、`transfer`、`leave`、`apply`、`cancel`、`review`、`disband`、`announcement`、`verify_mapping`(最后一个只用于失效指标)。

### 6.2a 锁等待封顶(DSN 会话变量)

InnoDB 默认 `innodb_lock_wait_timeout=50`。RPC 的 ctx 到期后 go-sql-driver 只关闭连接;服务端等锁线程在锁等待期间一般不检测对端断连(推断,Codex 以 §22 用例实测),会继续占着本事务已拿到的 `guild` 行锁,最长 50s,该帮后续写全部连锁超时。

做法:guild 在代码里强制给 DSN 加会话变量,不依赖 yaml(K8s ConfigMap 与本地 yaml 都无需改):

```go
// LockWaitTimeoutSeconds:帮会写事务都是几条主键 / 索引语句(毫秒级),等锁超过 1s 即视为异常长事务。
const LockWaitTimeoutSeconds = 1

// WithLockWaitTimeout 给 DSN 附加 innodb_lock_wait_timeout(go-sql-driver 把未知参数当作连接后 SET 的系统变量;
// 该变量为会话级可写,appuser 有权设置)。已有同名参数时覆盖。解析失败返回不含原文的错误(DSN 含口令)。
func WithLockWaitTimeout(dsn string) (string, error) {
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil { return "", errors.New("MySQL.DataSource 无法解析(原文含口令,不打印)") }
	if cfg.Params == nil { cfg.Params = map[string]string{} }
	cfg.Params["innodb_lock_wait_timeout"] = strconv.Itoa(LockWaitTimeoutSeconds)
	return cfg.FormatDSN(), nil
}
```

接线:`svc/servicecontext.go:69` 的 `sql.Open("mysql", c.MySQL.DataSource)` 改为先 `dsn, err := data.WithLockWaitTimeout(c.MySQL.DataSource)`(失败按该文件现有的打开失败路径处理)再 `sql.Open("mysql", dsn)`。`-migrate` 模式(B1 `runMigration`)**不**加该参数:DDL 等元数据锁另有语义。测试助手 `openGuildIntegrationRepo`(`guild_repo_zone_test.go`)同样先经 `WithLockWaitTimeout`。

**同步预算**(zrpc `Timeout=4000`,B1 另留 500ms 回包余量):归属区查询 ≤1500(审批通过时申请人与审批人两次查询**并行**,仍 ≤1500,第 5b 部分 §12.2.1)+ 闸门 EXISTS + 事务(单次锁等待 ≤1000 + 语句 ≪100)+ 装配(在线 MGET、待审计数)≈ 3100 < 3500。多次连续等锁可能超出预算:此时客户端得到超时,但服务端锁在 ≤1s 内随语句失败释放,不会连锁 50s。

### 6.3 事务内权威快照

把 `loadGuildFromMySQL` 改为接收读接口,事务内复用:

```go
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
func (r *GuildRepo) loadGuildFromMySQL(ctx context.Context, guildID uint64) (*GuildData, error) {
	return loadGuild(ctx, r.db, guildID)
}
func loadGuild(ctx context.Context, q queryer, guildID uint64) (*GuildData, error) // 原函数体,db 换成 q
```

写事务在最后一次写之后、提交之前调用 `loadGuild(ctx, tx, guildID)`,读到本事务自己的写,作为响应快照与推送收件人来源。**不**用提交后的 `GetGuild`:那一步若 Redis 失败,已提交的写会被报成失败,客户端随即进入 `RequiresReconnect` 隔离。

缓存失效语义(`invalidateAfterCommit`)与陈旧映射自愈见第 3b 部分 §6.4。

### 6.5 哨兵错误(追加到 `guild_repo.go` 的 var 块)

```go
	// ErrNotGuildMember:操作者在 MySQL 权威表中已不是该帮成员(缓存映射落后)。
	ErrNotGuildMember = errors.New("operator is not a member of the guild")
	ErrTargetNotMember = errors.New("target is not a member of the guild")
	ErrRankTooLow = errors.New("operator rank too low")
	ErrOfficerLimit = errors.New("officer limit reached")
	ErrLeaderCantLeave = errors.New("leader cannot leave")
	// ErrLeaderMismatch:guild.leader_id 与 role=3 行不一致(双存储被破坏),fail-closed 不写。
	ErrLeaderMismatch = errors.New("guild leader_id disagrees with member roles")
	ErrGuildLevelConfigMissing = errors.New("guild level row missing in GuildLevel table")
	ErrApplicationNotFound = errors.New("guild application not found or expired")
	ErrApplicationLimit = errors.New("player pending application limit reached")
	ErrApplicationQueueFull = errors.New("guild pending application queue full")
```

`ErrAnnouncementForbidden` 保留(公告沿用);`ErrLegacyRankSnapshotMismatch` 已由 B1 删除。

### 6.6 结果类型

```go
// MemberWriteResult:成员管理写的结果。Guild 为提交前在事务内读到的权威快照(含本次写)。
type MemberWriteResult struct {
	Guild   *GuildData
	Changed bool // false = 幂等无操作(如任免为同一角色),logic 不推送
}

// OfficerCapFunc 由 logic 注入:按等级查长老上限;ok=false 表示配表缺行。
type OfficerCapFunc func(level uint32) (maxOfficers uint32, ok bool)
```

<!-- s2_management_part3b.md -->

# S2 管理与审批 + 推送(B2)— 第 3b 部分:缓存失效、陈旧映射自愈、成员管理事务

## 6.4 提交后的缓存失效与陈旧映射自愈

### 6.4.1 `invalidateAfterCommit`(`guild_manage_repo.go`)

```go
// invalidateRetryDelays:同步失效失败后,在后台按此间隔重试;测试可改为 1ms。
var invalidateRetryDelays = []time.Duration{100 * time.Millisecond, 400 * time.Millisecond, 1600 * time.Millisecond}

// invalidateAfterCommit:MySQL 已是真相。先用请求 ctx 同步失效一次;失败的键交给 safego 协程
// (context.Background + 3s 预算)按 invalidateRetryDelays 有界重试;重试耗尽才计
// guild_cache_invalidate_failed_total{op} 并记 ERROR。任何情况下都不把已提交的写报成失败。
// guildID == 0 表示只失效玩家映射。
func (r *GuildRepo) invalidateAfterCommit(ctx context.Context, op string, guildID uint64, players ...uint64)
```

```go
// invalidateGaveUp:重试耗尽时调用;生产实现计指标 + ERROR 日志,单测替换为记录器。
var invalidateGaveUp = func(op string) { guildCacheInvalidateFailedTotal.Inc(op) }
```

实现要点:第一次同步尝试逐键调用 `invalidateGuildCache` / `invalidatePlayerGuildCache`,收集失败键;有失败键才起协程(协程名 `guild.cache_invalidate`),每轮只重试上一轮仍失败的键;全部轮次后仍有失败键 → `logx.Errorf` + `invalidateGaveUp(op)`。失效 Lua 本身幂等(INCR generation + DEL),重试多次无害。

**全部**提交后失效都走它:B2 新事务,以及改造后的存量 `CreateGuild`、公告写(第 4 部分 §8.7、§8.8)。`op` 增加 `verify_mapping`(§6.4.2)。

### 6.4.2 陈旧映射自愈(`guild_manage_repo.go`)

背景(已核对):`GetPlayerGuildID` 把 0 也缓存 `Cache.DefaultTTL=30m`(`guild_repo.go` “0 也缓存”,`etc/guild.yaml:51`);`GetPlayerGuild` 命中缓存 0 直接回 NotInGuild(`guild_logic.go:282-284`)。后台重试也失败时(Redis 长时间故障),刚被批准的玩家会一直读到 0,被踢者会一直读到旧帮。两个新方法用 MySQL 点查兜底:

```go
// VerifyPlayerGuildID:以 MySQL(uk_guild_member 点查)复核缓存映射 cached。
// 相同 → 原样返回,不写 Redis;不同 → invalidateAfterCommit("verify_mapping", 0, playerID) 后返回 MySQL 值。
// Redis 失败不影响返回值(MySQL 是真相)。
func (r *GuildRepo) VerifyPlayerGuildID(ctx context.Context, playerID, cached uint64) (uint64, error)

// ResolvePlayerGuild:客户端 GetPlayerGuild 的权威解析,返回 (nil, nil) 表示未入帮。
//  1. id := GetPlayerGuildID(缓存);id == 0 → id = VerifyPlayerGuildID(id);仍为 0 → (nil, nil)。
//  2. g := GetGuild(id)(缓存);g != nil 且成员含 playerID → 返回 g。
//  3. 否则(帮已不存在或快照里没有本人):失效 guild:v2:{id}(invalidateAfterCommit("verify_mapping", id))
//     → id2 := VerifyPlayerGuildID(id);id2 == 0 → (nil, nil)。
//  4. g2 := loadGuildFromMySQL(id2)(**不经缓存**,避免 Redis 仍坏时读回旧快照);
//     g2 == nil → (nil, nil);成员不含本人 → 返回内部错误(两份 MySQL 表互相矛盾,fail-closed)。
func (r *GuildRepo) ResolvePlayerGuild(ctx context.Context, playerID uint64) (*GuildData, error)
```

使用点(logic 侧细节见第 5 部分 §11.3、第 5b 部分 §12.3):
- **写路径决策前复核**:`operatorGuild` 读到 0;`ApplyJoinGuild` / `CreateGuild` 缓存预检读到 >0;`ApplyToGuild` / `CreateGuild` 事务返回 `ErrPlayerAlreadyInGuild`;事务返回 `ErrNotGuildMember` / `ErrGuildGone`。一律先 `VerifyPlayerGuildID` 再决定回哪个 tip。
- **客户端 `GetPlayerGuild`** 改调 `ResolvePlayerGuild`(替代原先只处理“帮已不存在”的分支)。内部调用(无会话)保持原缓存路径,不给高频内部读增加 MySQL 负载。

开销:只在“未入帮”或“快照不含本人”时多一次唯一索引点查;客户端读受 MessageLimiter 10/s 约束。

## 7. 成员管理事务

公共片段(以下 SQL 中 `?` 顺序即参数顺序):

```go
const sqlLockGuild = `SELECT level, leader_id, zone_id, max_members FROM guild WHERE guild_id = ? FOR UPDATE`
const sqlLockMemberPair = `SELECT player_id, role FROM guild_member
	WHERE guild_id = ? AND player_id IN (?, ?) ORDER BY player_id FOR UPDATE`
const sqlCountOfficers = `SELECT COUNT(*) FROM guild_member WHERE guild_id = ? AND role = ?`
```

`sqlLockGuild` 无行 → `ErrGuildGone`。锁完两行后:操作者不在结果中 → `ErrNotGuildMember`;目标不在 → `ErrTargetNotMember`。所有事务经 `inTx`,结果变量遵守 §6.2 约定。

### 7.1 `SetMemberRole(ctx, guildID, actorID, targetID uint64, role uint32, officerCap OfficerCapFunc) (MemberWriteResult, error)`

前置(logic 已校验,repo 再断言,违反返回 `fmt.Errorf` 内部错误):`actorID != targetID`,`constants.AssignableRole(role)`。事务 `op=set_role`:
1. `sqlLockGuild`。
2. `sqlLockMemberPair(guildID, min(actor,target), max(actor,target))`。
3. `!canAssignRole(actorRole)` → `ErrRankTooLow`。
4. `targetRole == RoleLeader` → `ErrRankTooLow`(防御分支)。
5. `targetRole == role` → `loadGuild(tx)`,`Changed=false`,提交(无写,释放锁)后返回成功。
6. `role == RoleOfficer`:`officerCap(level)` → `!ok` 返回 `ErrGuildLevelConfigMissing`;`sqlCountOfficers(guildID, RoleOfficer)` ≥ cap → `ErrOfficerLimit`。
7. `UPDATE guild_member SET role = ? WHERE guild_id = ? AND player_id = ?`(RowsAffected 必须为 1,否则内部错误)。
8. `loadGuild(tx)` → 提交 → `invalidateAfterCommit("set_role", guildID)`。

### 7.2 `KickMember(ctx, guildID, actorID, targetID uint64) (MemberWriteResult, error)`

事务 `op=kick`:1. `sqlLockGuild`;2. `sqlLockMemberPair`;3. `!canKick(actorRole, targetRole)` → `ErrRankTooLow`;4. `DELETE FROM guild_member WHERE guild_id = ? AND player_id = ?`(RowsAffected=1);4a. `DELETE FROM guild_application WHERE player_id = ?`(targetID,I3,第 4 部分 §8.1);5. `loadGuild(tx)` → 提交 → `invalidateAfterCommit("kick", guildID, targetID)`。

被踢者的帮贡随成员行一起删除(见第 9b 部分 §30)。

### 7.3 `TransferLeader(ctx, guildID, actorID, targetID uint64, officerCap OfficerCapFunc) (MemberWriteResult, error)`

事务 `op=transfer`:
1. `sqlLockGuild` → 得 `level, leaderID`。
2. `sqlLockMemberPair`。
3. `!canTransferLeader(actorRole)` → `ErrRankTooLow`。
4. `leaderID != actorID` → `logx.Errorf` 两份存储不一致 + 返回 `ErrLeaderMismatch`(不写)。
5. `officerCap(level)` → `!ok` 返回 `ErrGuildLevelConfigMissing`。
6. `officers := sqlCountOfficers(guildID, RoleOfficer)`;若 `targetRole == RoleOfficer` 则 `officers--`。
7. `oldLeaderRole := demotedLeaderRole(officers, cap)`:

   ```go
   // demotedLeaderRole:转让后原帮主的新角色。长老位有空 → 长老;否则成员(契约:超上限则变成员)。
   func demotedLeaderRole(officersAfterTarget, maxOfficers uint32) uint32 {
   	if officersAfterTarget < maxOfficers { return constants.RoleOfficer }
   	return constants.RoleMember
   }
   ```
8. 三条写,顺序固定:`UPDATE guild SET leader_id = ? WHERE guild_id = ?`(targetID);`UPDATE guild_member SET role = ? ...`(RoleLeader, targetID);`UPDATE guild_member SET role = ? ...`(oldLeaderRole, actorID)。
9. 断言 `SELECT COUNT(*) FROM guild_member WHERE guild_id = ? AND role = 3` 为 1,否则内部错误(回滚)。
10. `loadGuild(tx)` → 提交 → `invalidateAfterCommit("transfer", guildID)`。

### 7.4 `LeaveGuild(ctx, guildID, playerID uint64) (MemberWriteResult, error)`(取代 `RemoveMember`)

事务 `op=leave`:1. `sqlLockGuild` → 得 `leaderID`;2. `SELECT role FROM guild_member WHERE guild_id = ? AND player_id = ? FOR UPDATE` → 无行 `ErrNotGuildMember`;3. `role == RoleLeader || leaderID == playerID` → `ErrLeaderCantLeave`;4. `DELETE FROM guild_member WHERE guild_id = ? AND player_id = ?`;4a. `DELETE FROM guild_application WHERE player_id = ?`(I3);5. `loadGuild(tx)` → 提交 → `invalidateAfterCommit("leave", guildID, playerID)`。

`RemoveMember`(`guild_repo.go:362-372`)删除,不留调用点。

### 7.5 崩溃窗口

| 时刻 | 状态 | 恢复 |
|---|---|---|
| 提交前进程崩溃 / 连接断 | InnoDB 回滚,无写;锁在 ≤1s(锁等待封顶)或断连检测后释放 | 客户端超时隔离,重连后刷新看到旧状态;重试安全 |
| 提交后、失效缓存前崩溃(或后台重试耗尽) | MySQL 已写,缓存旧 | 授权永远读 MySQL。当事人:客户端 `GetPlayerGuild` 经 `ResolvePlayerGuild` 自愈;写路径经 `VerifyPlayerGuildID` 自愈。旁观成员看到的快照最长陈旧 `DefaultTTL`,下次任一写失效或 TTL 到期后恢复 |
| 提交后、推送前崩溃 | 无推送 | 至多一次;打开界面 / 刷新时拉取自愈 |
| 响应回包丢失 | 已写 | 客户端隔离至重连;重试得到幂等结果或 `TargetNotMember` / `RankTooLow`,均触发刷新 |

### 7.6 幂等

- 任免为同一角色:成功、`Changed=false`、不推送。
- 踢已离开的目标:`ErrTargetNotMember`。
- 转让重放:操作者已不是帮主 → `ErrRankTooLow`。
- 退帮重放:`ErrNotGuildMember` → logic `VerifyPlayerGuildID` 为 0 → 按成功处理(第 5b 部分 §12.3)。

<!-- s2_management_part4.md -->

# S2 管理与审批 + 推送(B2)— 第 4 部分:申请事务、建帮/解散/公告变更

## 8. 申请事务(`guild_manage_repo.go`)

### 8.1 数据规则

- `guild_application(guild_id, player_id, apply_ms, expire_ms)`(B1 已建;主键 `(guild_id, player_id)`,普通索引 `idx_guild_application_1(player_id)`、`idx_guild_application_2(expire_ms)`)。只存待审;通过、拒绝、撤回、过期、解散都删行。
- **“有效申请”判据 I1**:`expire_ms > now` 且申请人**没有** `guild_member` 行。所有计数与列表都在 SQL 里按 I1 过滤;物理删除只在写路径顺手做(惰性清理)。
- **成员行出现即清申请(I2)**:任何让玩家获得 `guild_member` 行的事务,必须在同一事务里 `DELETE FROM guild_application WHERE player_id = ?`。B2 的入口只有两个:审批通过(§8.5 第 5d 步)与建帮(§8.8)。否则建帮者日后解散 / 转让退帮时,72h 内的旧申请会按 I1 “复活”。B5/B6 若新增入帮入口,同样照做(AGENTS §11.4 逐项对齐)。
- **成员行消失即清申请(I3)**:任何删除 `guild_member` 行的事务(退帮 §7.4、踢人 §7.2、解散 §8.6)在同一事务里删除被删成员的全部申请。成员期间这些申请按 I1 本来就无效,删掉语义不变;它兜住 I2 覆盖不到的竞态残留(Apply 第 2 步非锁定读与他帮审批 / 建帮并发时插入的行,第 4b 部分),保证离帮后不会有旧申请“复活”。锁序 guild → guild_member → guild_application 合规。
- `now` 由 logic 传入:`uint64(time.Now().UnixMilli())`,取服务进程时钟(契约 §2)。多副本 NTP 偏差 ≤1s,相对 72h 可忽略。
- 注入的规则:

```go
type ApplicationRules struct {
	TTLMs        uint64 // GuildRule.application_expire_hours × 3_600_000
	MaxPerPlayer uint32 // GuildRule.max_pending_applications_per_player
	MaxPerGuild  uint32 // GuildRule.max_pending_applications_per_guild
}

const sqlCountLiveApplicationsOfGuild = `SELECT COUNT(*) FROM guild_application a
	LEFT JOIN guild_member m ON m.player_id = a.player_id
	WHERE a.guild_id = ? AND a.expire_ms > ? AND m.player_id IS NULL`
```

### 8.2 `ApplyToGuild(ctx, guildID, playerID uint64, requiredZone uint32, now uint64, rules ApplicationRules) (ApplyResult, error)`

```go
type ApplyResult struct {
	Inserted    bool     // false = 同帮重复申请,只刷新了有效期
	ReviewerIDs []uint64 // Inserted 时:该帮长老与帮主(推送收件人),player_id 升序
}
```

事务 `op=apply`(契约:同帮重复申请 = 刷新并成功,因此刷新判定排在满员与上限判定**之前**):
1. `SELECT zone_id, max_members FROM guild WHERE guild_id = ? FOR UPDATE` → 无行 `ErrGuildGone`;`requiredZone != 0 && zone_id != requiredZone` → `ErrGuildZoneMismatch`。
2. `SELECT guild_id FROM guild_member WHERE player_id = ?`(**非锁定**一致性读)→ 有行 `ErrPlayerAlreadyInGuild`(理由见 §8.9 末段)。
3. 清理本人过期行:`DELETE FROM guild_application WHERE player_id = ? AND expire_ms <= ?`。
4. 清理本帮过期行(有界,持有本帮 guild 行锁):`DELETE FROM guild_application WHERE guild_id = ? AND expire_ms <= ? LIMIT 100`。
5. `SELECT guild_id FROM guild_application WHERE player_id = ? FOR UPDATE`:得到本人全部未过期申请;`existing := 其中是否有 guildID`,`count := 行数`。
6. `existing` 为真:`UPDATE guild_application SET apply_ms = ?, expire_ms = ? WHERE guild_id = ? AND player_id = ?`(now, now+TTLMs)→ `Inserted=false` → 跳到第 10 步。刷新**不**检查满员与两个上限(该行本来就计入)。
7. `SELECT COUNT(*) FROM guild_member WHERE guild_id = ?` ≥ `max_members` → `ErrGuildFull`(满员帮不收**新**申请)。
8. `count >= rules.MaxPerPlayer` → `ErrApplicationLimit`;`sqlCountLiveApplicationsOfGuild(guildID, now)` ≥ `rules.MaxPerGuild` → `ErrApplicationQueueFull`。
9. `INSERT INTO guild_application (guild_id, player_id, apply_ms, expire_ms) VALUES (?, ?, ?, ?)` → `Inserted=true`;`SELECT player_id FROM guild_member WHERE guild_id = ? AND role IN (1, 3) ORDER BY player_id` → `ReviewerIDs`。
10. 提交。不失效任何缓存:`pending_application_count` 不进缓存,每次按请求者现算(第 5b 部分 §12.4)。

### 8.3 `CancelApplication(ctx, guildID, playerID, now uint64) error`

不开事务(单行单锁,autocommit;锁等待同样受 §6.2a 封顶):
1. `DELETE FROM guild_application WHERE guild_id = ? AND player_id = ? AND expire_ms > ?` → RowsAffected=1 → nil。
2. 否则 `DELETE FROM guild_application WHERE guild_id = ? AND player_id = ?`(顺手删过期行)→ `ErrApplicationNotFound`。

1205 → `ErrWriteConflict`(同 `retryOnDeadlock` 的判定,直接包一层 `retryOnDeadlock(ctx, "cancel", ...)`,`op` 固定集合追加 `cancel`)。

### 8.4 读

```go
type ApplicationRow struct{ GuildID, ApplyMs, ExpireMs uint64 }
type ApplicantRow struct{ PlayerID, ApplyMs, ExpireMs uint64 }

// ListMyApplications:本人未过期申请,至多 limit 条(logic 传 10)。
// SELECT guild_id, apply_ms, expire_ms FROM guild_application
//   WHERE player_id = ? AND expire_ms > ? ORDER BY apply_ms DESC, guild_id ASC LIMIT ?
func (r *GuildRepo) ListMyApplications(ctx context.Context, playerID, now uint64, limit uint32) ([]ApplicationRow, error)

// ListApplicants:按 I1 过滤。
// SELECT a.player_id, a.apply_ms, a.expire_ms FROM guild_application a
//   LEFT JOIN guild_member m ON m.player_id = a.player_id
//   WHERE a.guild_id = ? AND a.expire_ms > ? AND m.player_id IS NULL
//   ORDER BY a.apply_ms ASC, a.player_id ASC LIMIT ?
func (r *GuildRepo) ListApplicants(ctx context.Context, guildID, now uint64, limit uint32) ([]ApplicantRow, error)

// MemberRole:非锁定读权威 role,供 ListGuildApplications 授权。
func (r *GuildRepo) MemberRole(ctx context.Context, guildID, playerID uint64) (role uint32, found bool, err error)

func (r *GuildRepo) CountLiveApplications(ctx context.Context, guildID, now uint64) (uint32, error)
```

I2 保证有效申请数不会因“建帮者旧申请复活”超过 `MaxPerGuild`,`ListApplicants` 的 `LIMIT MaxPerGuild` 不会截掉有效行(过期残留已被 `expire_ms > ?` 排除)。

### 8.5 `ReviewApplication(ctx, guildID, actorID, applicantID uint64, approve bool, applicantZone uint32, now uint64) (ReviewResult, error)`

```go
type ReviewResult struct {
	Guild    *GuildData // 事务内权威快照
	Approved bool
}
```

前置断言:`approve && applicantZone == 0` → 内部错误(logic 必须先查到申请人归属 zone,fail-closed)。事务 `op=review`:
1. `SELECT zone_id, max_members FROM guild WHERE guild_id = ? FOR UPDATE` → 无行 `ErrGuildGone`。
2. `SELECT role FROM guild_member WHERE guild_id = ? AND player_id = ? FOR UPDATE`(操作者)→ 无行 `ErrNotGuildMember`;`!canReviewApplications(role)` → `ErrRankTooLow`。
3. `SELECT expire_ms FROM guild_application WHERE guild_id = ? AND player_id = ? FOR UPDATE` → 无行 `ErrApplicationNotFound`;`expire_ms <= now` → `DELETE` 该行 → `errCommitThen{ErrApplicationNotFound}`。
4. **拒绝**:`DELETE FROM guild_application WHERE guild_id = ? AND player_id = ?` → `loadGuild(tx)` → 提交。`Approved=false`,不失效缓存。
5. **通过**:
   a. **zone 复核**:`guild.zone_id != applicantZone` → `DELETE` 该申请行 → `errCommitThen{ErrApplicationNotFound}`。原因:`tools/merge_zone/unmerge.go:133-149` 只把清单里帮会的 `guild.zone_id` 改回源区、另行改回玩家路由,不动 `guild_application`;合服后开服到回滚之间提交的 (目标区玩家, 源区帮) 申请在回滚后仍存在。旧 `AddMemberInZone` 也是在插入成员那一刻复核 zone(`guild_repo.go:330`),本步保持同一防线。
   b. `SELECT COUNT(*) FROM guild_member WHERE guild_id = ?` ≥ `max_members` → `ErrGuildFull`(回滚,**保留**申请)。
   c. `INSERT INTO guild_member (guild_id, player_id, role, join_time_ms, last_active_ms, contribution_total, contribution_balance) VALUES (?, ?, 0, ?, ?, 0, 0)`。遇 1062(申请人已在某帮):InnoDB 只回滚该语句 → `DELETE FROM guild_application WHERE guild_id = ? AND player_id = ?` → `errCommitThen{ErrApplicationNotFound}`。
   d. `DELETE FROM guild_application WHERE player_id = ?`(I2:删申请人在所有帮会的申请)。
   e. `loadGuild(tx)` → 提交 → `invalidateAfterCommit("review", guildID, applicantID)`。`Approved=true`。

原 `AddMember` / `AddMemberInZone`(`guild_repo.go:300-360`)删除。

### 8.6 解散:`DisbandGuild(ctx, guildID, actorID uint64) (DisbandResult, error)`

```go
type DisbandResult struct {
	ZoneID    uint32   // 删除事务内 FOR UPDATE 读到的 zone(清榜唯一可信输入)
	MemberIDs []uint64 // 解散前全部成员,player_id 升序(推送收件人 + 缓存失效)
}
```

取代 `DeleteGuild`(删除;`rank_zone_integration_test.go:195` 改调 `DisbandGuild(ctx, guildID, leaderID)`)。事务 `op=disband`,结果变量遵守 §6.2 约定:
1. `SELECT zone_id, leader_id FROM guild WHERE guild_id = ? FOR UPDATE` → 无行 `ErrGuildGone`;`leader_id != actorID` → `ErrRankTooLow`(logic 映射 `kGuildNotLeader`;修复原先按缓存 `LeaderID` 授权,`guild_logic.go:402`)。
2. `SELECT player_id FROM guild_member WHERE guild_id = ? ORDER BY player_id FOR UPDATE`。
3. `DELETE FROM guild_application WHERE guild_id = ?`;再按 I3 `DELETE FROM guild_application WHERE player_id IN (<第 2 步的成员,≤100 个占位符>)`。
4. `DELETE FROM guild_member WHERE guild_id = ?`。
5. `DELETE FROM guild WHERE guild_id = ?`(RowsAffected=0 → `ErrGuildGone`)。
6. 提交 → `invalidateAfterCommit("disband", guildID, memberIDs...)`。

B5/B6 以 guild_id 为键的新表由对应批次加进第 3 步之后。

### 8.7 公告写:`UpdateAnnouncement(ctx, guildID, playerID uint64, text string) (*GuildData, error)`

新方法放 `guild_manage_repo.go`,事务 `op=announcement`,步骤与现 `UpdateAnnouncementAuthorized` 相同(锁 guild 行 → 锁操作者行 → `canSetAnnouncement` → UPDATE),UPDATE 之后 `loadGuild(tx)` 取快照 → 提交 → `invalidateAfterCommit("announcement", guildID)`。存量 `UpdateAnnouncementAuthorized` 改为薄包装 `_, err := r.UpdateAnnouncement(...); return err`,`guild_repo_test.go` 的三处调用不动(若其中有断言“失效失败要报错”的用例,Codex 报告后按新语义改断言)。

### 8.8 建帮:`CreateGuild` 改造(`guild_repo.go`)

改为经 `inTx(ctx, "create", ...)`:INSERT guild → INSERT guild_member(帮主)→ **`DELETE FROM guild_application WHERE player_id = ?`(I2)** → 提交 → `invalidateAfterCommit("create", guild.GuildID, leader.PlayerID)`。锁序 guild → guild_member → guild_application 合规;与 Apply 第 5/9 步在申请人 `player_id` 索引间隙上可能互等 → 死锁由 `inTx` 重跑,耗尽回 `ErrWriteConflict`。1062 分支(`ErrGuildNameTaken` / `ErrPlayerAlreadyInGuild`)不变;提交后不再返回失效错误。

<!-- s2_management_part4b.md -->

# S2 管理与审批 + 推送(B2)— 第 4b 部分:并发与死锁分析

## 8.9 并发与死锁分析

| 场景 | 锁交错 | 结果 |
|---|---|---|
| 同一玩家并发向 G1、G2 申请,已有 2 条 | 两事务各持自己帮的 guild 行锁;第 5 步对已有的 2 条记录取 X 锁 → 后到者等待,串行执行;若本人没有任何申请行,双方只拿到互相兼容的间隙锁,第 9 步 INSERT 的插入意向锁互相冲突 → InnoDB 死锁检测回滚一方 | 串行时后者读到 3 条 → `ErrApplicationLimit`;死锁时 `inTx` 退避后重跑(最多 3 次)得到同样结论。**上限不会被突破** |
| 同帮重复申请且该帮已满员 | 第 5 步命中 existing → 第 6 步刷新,不走第 7 步 | 成功(契约“刷新并成功”);新申请人才会得到 `ErrGuildFull` |
| 两个帮会并发通过同一申请人 P | T1 持 (G1,P) 申请行,INSERT 成员 P;T2 持 (G2,P),INSERT 成员 P 等 T1 的唯一键锁;T1 第 5d 步删 P 的全部申请,等 T2 持有的 (G2,P) → 死锁,回滚一方 | 若 T2 被回滚并重跑:(G2,P) 已被 T1 删除 → `ErrApplicationNotFound`。若 T1 被回滚:T2 通过,T1 重跑同理。**P 恰好进一个帮**。这是正常玩法可触发的场景,所以重试 3 次 + 退避,耗尽也只回业务 tip `kGuildBusyRetry`,客户端不隔离 |
| 申请与他帮通过并发 | Apply 第 2 步不加锁,可能在对方提交前读到“未入帮”并插入 (G1,P) | 留下一条 I1 判为无效的行:不计数、不列出;G1 审批通过时 INSERT 撞 1062 → 删行回 NotFound;P 再申请时第 2 步拒绝。P 日后退帮 / 被踢 / 帮会解散时,I3 在同一事务里删掉该行,不会复活 |
| 申请与建帮并发(同一玩家 P) | Create 不锁已有 guild 行;Create 第 3 步 DELETE P 的申请与 Apply 第 5 步 FOR UPDATE / 第 9 步 INSERT 在 `idx_guild_application_1(player_id)` 上互等 → 可能死锁 | 回滚一方后重跑:Create 先提交 → Apply 第 2 步读到成员行 → `ErrPlayerAlreadyInGuild`;Apply 先提交 → Create 删掉该申请。若 Apply 的第 2 步快照早于 Create 提交、第 5 步等锁晚于 Create 提交,会留下一条按 I1 无效的物理行,P 离开 G2 时由 I3 删除 |
| 申请与目标帮解散并发 | 两者都先锁同一 guild 行,串行 | 解散在先 → `ErrGuildGone`;申请在先 → 解散第 3 步删掉该申请 |
| 审批与申请人撤回并发 | 同一 (G,P) 行上串行 | 撤回在先 → 审批 NotFound;审批在先 → 撤回 NotFound |
| 审批时帮会 zone 与申请人归属 zone 不一致 | zone 在 guild 行锁下读 | 删申请行,`ErrApplicationNotFound`;不产生跨区成员 |
| 踢人与该成员自己退帮并发 | 同一 guild 行锁串行 | 后者得到 `TargetNotMember` / `NotGuildMember` |
| 任命长老并发(上限 2,已有 1) | 同一 guild 行锁串行,COUNT 在锁内 | 第二个得到 `ErrOfficerLimit` |
| 长事务占住 guild 行锁 | 后续同帮写在语句级等锁 ≤1s(`innodb_lock_wait_timeout=1`)→ 1205 | `ErrWriteConflict` → `kGuildBusyRetry`;不重试,不会连锁 50s |
| 死锁重跑时结果变量 | `fn` 内声明、成功末尾赋值(§6.2) | `MemberIDs` / `ReviewerIDs` 不会因重跑重复 |

Apply 第 2 步不锁 `guild_member` 的理由:锁定读在 `uk_guild_member` 上取间隙锁,会与并发通过的 INSERT 形成更多死锁环,而“申请人已入帮”的竞态已被 I1 与审批时的 1062 分支兜住,不加锁不会产生错误的成员关系。

**ErrWriteConflict 的对外语义**:logic 映射为 tip `kGuildBusyRetry`(第 5 部分 §11.4),响应体正常返回,客户端 `Accept` 显示“帮会操作繁忙,请稍后重试。”,**不**置 `RequiresReconnect`。原稿返回 gRPC `Aborted` 会让客户端隔离到重登,而两帮并发审批同一申请人是正常玩法,不应有这种代价。

**ListGuildApplications 与跨区残留**:列表不逐个查申请人归属 zone(每人一次 data_service 调用,50 人即 50 次)。回滚合服后若出现跨区残留行,列表里仍可见,审批通过时第 5a 步删除并回 NotFound,客户端随即重拉列表;拒绝则直接删除。残留行最多存在 72h。

<!-- s2_management_part5.md -->

# S2 管理与审批 + 推送(B2)— 第 5 部分:logic 层(构造、前置、错误映射)

## 10. 构造与配表读取

### 10.1 `NewGuildLogic` 改为可选项(不改已有 16 处调用)

```go
type Option func(*GuildLogic)

// WithNotifier 注入推送实现;不传 = NoopNotifier(单测与未配 Kafka 的环境)。
func WithNotifier(n GuildNotifier) Option {
	return func(l *GuildLogic) {
		if n != nil { l.notifier = n }
	}
}

// WithApplyPushGate 注入“申请推送冷却”判定;不传 = 总是放行(单测)。生产由 guild.go 传 repo.TryMarkApplyPush。
func WithApplyPushGate(g ApplyPushGate) Option {
	return func(l *GuildLogic) {
		if g != nil { l.applyPushGate = g }
	}
}

// ApplyPushGate:返回 true 才发 APPLICATION_RECEIVED。
type ApplyPushGate func(ctx context.Context, guildID, playerID uint64) bool

func NewGuildLogic(repo *data.GuildRepo, ids IDMinter, onlineResolver *OnlineStatusResolver,
	mergeFence MergeFence, homeZones HomeZoneLookup, opts ...Option) *GuildLogic {
	l := &GuildLogic{repo: repo, ids: ids, onlineResolver: onlineResolver, mergeFence: mergeFence,
		homeZones: homeZones, notifier: NoopNotifier{},
		applyPushGate: func(context.Context, uint64, uint64) bool { return true }}
	for _, opt := range opts { opt(l) }
	return l
}
```

`GuildLogic` 增字段 `notifier GuildNotifier`、`applyPushGate ApplyPushGate`。B3b 的 `PlayerNameResolver` 按同一方式加 `WithPlayerNames`。

### 10.2 配表读取(`logic/guild_manage_logic.go` 顶部)

```go
// 配表只存 id、用时现查(table 包是 atomic 快照)。启动时 ValidateGuildTables 已保证行存在且自洽;
// 运行期查不到 = 配置被错误替换,一律 fail-closed(gRPC Internal)。
const guildRuleRowID uint32 = 1

func applicationRulesFromTable() (data.ApplicationRules, bool) {
	row, ok := table.GuildRuleTableManagerInstance.FindById(guildRuleRowID)
	if !ok { return data.ApplicationRules{}, false }
	return data.ApplicationRules{
		TTLMs:        uint64(row.GetApplicationExpireHours()) * uint64(time.Hour/time.Millisecond),
		MaxPerPlayer: row.GetMaxPendingApplicationsPerPlayer(),
		MaxPerGuild:  row.GetMaxPendingApplicationsPerGuild(),
	}, true
}

// officerCapFromTable 满足 data.OfficerCapFunc。
func officerCapFromTable(level uint32) (uint32, bool) {
	row, ok := table.GuildLevelTableManagerInstance.FindById(level)
	if !ok { return 0, false }
	return row.GetMaxOfficers(), true
}

func ValidateGuildTables() error {
	rule, _ := table.GuildRuleTableManagerInstance.FindById(guildRuleRowID)
	return validateGuildTables(rule, table.GuildLevelTableManagerInstance.FindAll())
}
```

`validateGuildTables` 的规则见第 1 部分 §3.3(B5/B6 复用,不另写);错误文案带表名、行 id、字段名与实际值。`FindAll()` 顺序不作假设,函数内先按 id 排序。

`guild.go`:在 `svcCtx := svc.NewServiceContext(...)` 之后加:

```go
	// 帮会规则表 / 等级表自洽校验:缺行或数值越界直接拒启(fail-closed)。
	if err := logic.ValidateGuildTables(); err != nil {
		logx.Must(fmt.Errorf("guild config tables invalid: %w", err))
	}
```

### 10.3 `CreateGuild` 的成员上限

`guild_logic.go:163` `MaxMembers: constants.DefaultMaxMembers` 改为读 `table.GuildLevelTableManagerInstance.FindById(constants.DefaultInitLevel)` 的 `MaxMembers`(默认行为 30);查不到返回 `status.Error(codes.Internal, "GuildLevel row 1 missing")`。读表放在闸门之后、铸号之前(不白烧号)。

## 11. 写 RPC 公共前置

### 11.1 合服闸门改回 tip

`checkMergeFence` 删除,换成 `mergeFenceTip(ctx, zoneID) *base.TipInfoMessage`:`mergeFence == nil || zoneID == 0` → nil;`MergeInProgress` 读失败 → 记 ERROR,返回 `tipErr(constants.ErrZoneMerging, "merge fence unreadable")`(fail-closed);命中 → 记 Info(带 `MergeFenceKey(zoneID)`),返回 `tipErr(constants.ErrZoneMerging, "zone merging")`。同时删除 `var ErrZoneMerging = errors.New(...)`(`guild_logic.go:196-198`)与 `constants.go:53-63` 旧注释。闸门查**请求者归属 zone**。

### 11.2 客户端写前置

```go
// clientWrite:新管理 / 申请 RPC 的统一入口。只接受带 gate 会话的调用 —— 请求体没有 player_id,
// 内部调用没有可信的操作者身份。顺序:会话 → 归属 zone → 合服闸门。
func (l *GuildLogic) clientWrite(ctx context.Context) (playerID uint64, zoneID uint32, tip *base.TipInfoMessage, err error) {
	who := callerOf(ctx, 0)
	if !who.fromClient {
		return 0, 0, nil, status.Error(codes.PermissionDenied, "guild management requires a client session")
	}
	zoneID, tip, err = l.clientZone(ctx, who.playerID)
	if err != nil || tip != nil {
		return who.playerID, 0, tip, err
	}
	return who.playerID, zoneID, l.mergeFenceTip(ctx, zoneID), nil
}
```

存量写 RPC(`CreateGuild`、`LeaveGuild`、`DisbandGuild`、`SetAnnouncement`)保留内部调用能力:客户端来源时调用 `clientZone` + `mergeFenceTip`;内部调用 zone 取 0(`CreateGuild` 取 `req.ZoneId`),闸门对 0 不生效,与现状一致。

### 11.3 操作者所在帮会(带自愈)

```go
// operatorGuild:缓存映射定位操作者帮会;读到 0 时以 MySQL 复核一次(第 3b 部分 §6.4.2),
// 防止“审批通过但映射失效失败”的玩家在 30 分钟内什么都做不了。事务仍以 MySQL 复核成员身份。
func (l *GuildLogic) operatorGuild(ctx context.Context, playerID uint64) (uint64, *base.TipInfoMessage, error) {
	id, err := l.repo.GetPlayerGuildID(ctx, playerID)
	if err != nil { return 0, nil, err }
	if id == 0 {
		if id, err = l.repo.VerifyPlayerGuildID(ctx, playerID, 0); err != nil { return 0, nil, err }
		if id == 0 { return 0, tipErr(constants.ErrNotInGuild, "not in any guild"), nil }
	}
	return id, nil, nil
}
```

事务返回 `data.ErrNotGuildMember` 或 `data.ErrGuildGone`:`l.repo.VerifyPlayerGuildID(ctx, playerID, guildID)`(失败只记日志),再回 `kGuildNotInGuild` / `kGuildNotFound`。

### 11.4 错误映射(`mapWriteErr`,guild_manage_logic.go)

| repo 错误 | 响应 |
|---|---|
| `ErrGuildGone`、`ErrGuildZoneMismatch` | tip `kGuildNotFound` |
| `ErrNotGuildMember` | tip `kGuildNotInGuild`(并复核映射) |
| `ErrTargetNotMember` | tip `kGuildTargetNotMember` |
| `ErrRankTooLow` | tip `kGuildRankTooLow`(`DisbandGuild` 例外:`kGuildNotLeader`) |
| `ErrOfficerLimit` | tip `kGuildOfficerLimit` |
| `ErrLeaderCantLeave` | tip `kGuildLeaderCantLeave` |
| `ErrGuildFull` | tip `kGuildFull` |
| `ErrPlayerAlreadyInGuild` | tip `kGuildAlreadyInGuild`(先 `VerifyPlayerGuildID(actor, 0)` 修正陈旧 0 映射) |
| `ErrApplicationNotFound` / `ErrApplicationLimit` / `ErrApplicationQueueFull` | 同名 tip |
| `ErrWriteConflict` | tip `kGuildBusyRetry`(记 Info;**不**返回 gRPC 错误) |
| `ErrLeaderMismatch`、`ErrGuildLevelConfigMissing` | `status.Error(codes.Internal, ...)`,ERROR 日志 |
| 其它 | 原样返回 error(serverbase 记故障) |

logic 前置校验产生的 tip:`target_player_id==0` → `kGuildTargetNotMember`;`target == 操作者` → `kGuildCannotTargetSelf`;`!AssignableRole(role)` → `kGuildNoPermission`;`guild_id==0`(申请 → `kGuildNotFound`,撤回 → `kGuildApplicationNotFound`);`applicant_player_id==0` → `kGuildApplicationNotFound`;`applicant == 操作者` → `kGuildCannotTargetSelf`。配表缺行 → `codes.Internal`。

存量 `CreateGuild` 的 `ErrPlayerAlreadyInGuild` 与缓存预检同样先复核(第 5b 部分 §12.3)。

<!-- s2_management_part5b.md -->

# S2 管理与审批 + 推送(B2)— 第 5b 部分:logic 层 RPC 处理

## 12. RPC 处理(`logic/guild_manage_logic.go`)

### 12.1 模板:`SetGuildMemberRole`

```go
func (l *GuildLogic) SetGuildMemberRole(ctx context.Context, req *pb.SetGuildMemberRoleRequest) (*pb.SetGuildMemberRoleResponse, error) {
	actor, _, tip, err := l.clientWrite(ctx)
	if err != nil { return nil, err }
	if tip != nil { return &pb.SetGuildMemberRoleResponse{ErrorMessage: tip}, nil }
	if tip := targetTip(actor, req.GetTargetPlayerId()); tip != nil {
		return &pb.SetGuildMemberRoleResponse{ErrorMessage: tip}, nil
	}
	if !constants.AssignableRole(req.GetRole()) {
		return &pb.SetGuildMemberRoleResponse{ErrorMessage: tipErr(constants.ErrNoPermission, "role not assignable")}, nil
	}
	guildID, tip, err := l.operatorGuild(ctx, actor)
	if err != nil || tip != nil { return &pb.SetGuildMemberRoleResponse{ErrorMessage: tip}, err }
	res, err := l.repo.SetMemberRole(ctx, guildID, actor, req.GetTargetPlayerId(), req.GetRole(), officerCapFromTable)
	if tip, err := l.mapWriteErr(ctx, actor, guildID, err); err != nil || tip != nil {
		return &pb.SetGuildMemberRoleResponse{ErrorMessage: tip}, err
	}
	if res.Changed {
		l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_ROLE_CHANGED, res.Guild.GuildID, actor, req.GetTargetPlayerId(), membersExcept(res.Guild, actor))
	}
	return &pb.SetGuildMemberRoleResponse{Guild: l.guildInfoFor(ctx, res.Guild, actor)}, nil
}
```

`mapWriteErr(ctx, actor, guildID, err)` 在 `ErrNotGuildMember` / `ErrGuildGone` / `ErrPlayerAlreadyInGuild` 分支内部做映射复核(§11.3、§11.4)。err 非 nil 时响应体被 gRPC 丢弃,只有 error 生效,与现有写法一致。

### 12.2 其余处理(步骤均在 `clientWrite` 之后,除注明外)

| RPC | 步骤 | 成功推送(第 6 部分 §14) | 响应 |
|---|---|---|---|
| `KickGuildMember` | targetTip → operatorGuild → `repo.KickMember` | `MEMBER_KICKED`:剩余成员(除操作者)+ 被踢者 | 快照 |
| `TransferGuildLeader` | targetTip → operatorGuild → `repo.TransferLeader(..., officerCapFromTable)` | `LEADER_TRANSFERRED`:全体成员除操作者 | 快照 |
| `ApplyJoinGuild` | guild_id≠0 → 缓存预检 `c := GetPlayerGuildID`;`c>0` → `VerifyPlayerGuildID(actor, c)`,仍 >0 回 `kGuildAlreadyInGuild` → `applicationRulesFromTable` → `repo.ApplyToGuild(guildID, actor, zone, now, rules)` | 仅 `Inserted` 且 `applyPushGate(ctx, guildID, actor)` 为真:`APPLICATION_RECEIVED` → `ReviewerIDs`,target=申请人 | 空 |
| `CancelGuildApplication` | guild_id≠0 → `repo.CancelApplication` | 无 | 空 |
| `ReviewGuildApplication` | 见 §12.2.1 | 通过:`MEMBER_JOINED` → 快照全体除操作者(含申请人);拒绝:`APPLICATION_REJECTED` → 申请人 | 快照 |

**申请推送冷却**(防“申请 → 撤回 → 申请”刷屏):`repo.TryMarkApplyPush(ctx, guildID, playerID) bool`(`guild_manage_repo.go`)执行 `SET guild:apply_push:{guild_id}:{player_id} 1 NX PX 60000`(常量 `ApplyPushCooldown = 60 * time.Second`);拿到键返回 true;键已存在或 Redis 出错返回 false(出错记 Info)→ 不推(仍符合至多一次;审批人打开申请页或刷新即可看到)。`guild.go` 以 `logic.WithApplyPushGate(repo.TryMarkApplyPush)` 注入。键不带敏感信息,60s 自然过期,无需清理。

#### 12.2.1 `ReviewGuildApplication`

```go
func (l *GuildLogic) ReviewGuildApplication(ctx context.Context, req *pb.ReviewGuildApplicationRequest) (*pb.ReviewGuildApplicationResponse, error) {
	who := callerOf(ctx, 0)
	if !who.fromClient { return nil, status.Error(codes.PermissionDenied, "guild management requires a client session") }
	applicant := req.GetApplicantPlayerId()
	if applicant == 0 { return reviewTip(constants.ErrApplicationNotFound), nil }
	if applicant == who.playerID { return reviewTip(constants.ErrCannotTargetSelf), nil }

	// 通过时并行查申请人归属 zone:与 clientWrite 里的审批人查询同时进行,同步预算仍是一次 ≤1500ms。
	var zoneCh chan applicantZoneResult
	if req.GetApprove() && l.homeZones != nil {
		zoneCh = make(chan applicantZoneResult, 1)
		safego.Go("guild.review.applicant_zone", func() {
			z, err := l.homeZones.HomeZone(ctx, applicant)
			zoneCh <- applicantZoneResult{zone: z, err: err}
		})
	}
	actor, _, tip, err := l.clientWrite(ctx)
	if err != nil { return nil, err }
	if tip != nil { return &pb.ReviewGuildApplicationResponse{ErrorMessage: tip}, nil }

	var applicantZone uint32
	if req.GetApprove() {
		if zoneCh == nil { return nil, status.Error(codes.Unavailable, "guild home zone lookup is not configured") }
		select {
		case r := <-zoneCh:
			if r.err != nil { return nil, r.err } // data_service 故障:fail-closed,不批
			if r.zone == 0 {
				logx.Infof("[guild] review refused: applicant %d has no home zone mapping", applicant)
				return reviewTip(constants.ErrApplicationNotFound), nil
			}
			applicantZone = r.zone
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	guildID, tip, err := l.operatorGuild(ctx, actor)
	if err != nil || tip != nil { return &pb.ReviewGuildApplicationResponse{ErrorMessage: tip}, err }
	res, err := l.repo.ReviewApplication(ctx, guildID, actor, applicant, req.GetApprove(), applicantZone, nowMs())
	// mapWriteErr → 推送(见上表)→ 响应快照,同 §12.1
}
```

`clientWrite` 先返回 tip / error 时,后台查询随请求 ctx 取消(`HomeZone` 以请求 ctx 派生超时),带缓冲的 channel 保证协程不泄漏。`safego` 兜住 panic 时 channel 无写入,由 `ctx.Done()` 分支收尾。拒绝不查申请人 zone(删除申请总是安全的)。

#### 12.2.2 读 RPC(要求会话,不查闸门)

- `ListMyGuildApplications`:无会话 → PermissionDenied;`clientZone`(tip / error 原样返回);`c := GetPlayerGuildID`,`c>0` 时 `VerifyPlayerGuildID(actor, c)` 仍 >0 → 返回空列表;否则 `repo.ListMyApplications(now, 10)`,逐条 `repo.GetGuild`(缓存)组装 `GuildApplicationView`;帮会已不存在或 `!visibleIn(guild, zone)` 的条目跳过(跨区残留不展示,见第 4b 部分末段);`GetGuild` 报错 → 整体返回 error。
- `ListGuildApplications`:operatorGuild → `repo.MemberRole` 非锁定读;未找到 → `VerifyPlayerGuildID` + `kGuildNotInGuild`;`Rank<Officer` → `kGuildRankTooLow`;`repo.ListApplicants(now, rules.MaxPerGuild)`;在线状态经 `onlineResolver.BatchResolve`;组装 `GuildApplicantView`(name 恒空)。不查归属 zone(本帮对自己永远可见)。

### 12.3 存量 RPC 的改动

- **`LeaveGuild`**:客户端来源先 `clientZone` + `mergeFenceTip`;`operatorGuild` → `repo.LeaveGuild`。`ErrNotGuildMember` / `ErrGuildGone` → `VerifyPlayerGuildID`:为 0 → 成功(幂等退帮);非 0 → `kGuildAlreadyInGuild`(沿用 `guild_logic.go:373-375` 语义)。成功 → `MEMBER_LEFT` → 快照中剩余成员。删掉原先的 `GetGuild` + 缓存 `LeaderID` 判断。
- **`DisbandGuild`**:客户端来源先闸门;`operatorGuild` → `repo.DisbandGuild(guildID, actor)`;`ErrRankTooLow` → `kGuildNotLeader`;成功 → `RemoveGuildFromRank(res.ZoneID)`(失败只记日志)→ `DISBANDED` → `res.MemberIDs` 除操作者。
- **`SetAnnouncement`**:客户端来源先闸门;改调 `repo.UpdateAnnouncement`(第 4 部分 §8.7)取事务内快照;`ErrAnnouncementForbidden` 映射不变;成功 → `ANNOUNCEMENT_CHANGED` → `membersExcept(snapshot, actor)` → 响应 `SetAnnouncementResponse{Guild: l.guildInfoFor(ctx, snapshot, actor)}`。不再在成功后读缓存 `GetGuild`。
- **`CreateGuild`**:闸门换 `mergeFenceTip`;缓存预检 `existingGuildID>0`(`guild_logic.go:143`)→ `VerifyPlayerGuildID(actor, existing)` 仍 >0 才回 `kGuildAlreadyInGuild`;repo 返回 `ErrPlayerAlreadyInGuild` 时同样复核;`ErrWriteConflict` → `kGuildBusyRetry`;其余见 §10.3。不推送。
- **`GetPlayerGuild`**:客户端来源(`who.fromClient`)改调 `repo.ResolvePlayerGuild(ctx, who.playerID)`:nil → `kGuildNotInGuild`;否则 `guildInfoFor`。内部调用保持原缓存路径(含原“帮已不存在 → RefreshPlayerGuildID”分支)。
- **`JoinGuild`**:整函数删除。

### 12.4 `GuildInfo` 装配

`toProtoGuild` 增补(与请求者无关):`Funds: g.Funds`;成员 `ContributionTotal / ContributionBalance`;`OfficerCount` = 成员中 `Role == RoleOfficer` 的个数;`GuildLevelTableManagerInstance.FindById(g.Level)` 命中则填 `MaxOfficers`、`UpgradeCostFunds`,未命中记 ERROR、两字段留 0(只影响展示,授权另查)。

```go
// guildInfoFor:toProtoGuild + 只给长老 / 帮主看的待审数。viewerID=0(内部调用)不算。
// 计数读 MySQL(不进缓存);失败只记日志、留 0 —— 纯展示字段,不值得让整个读失败。
func (l *GuildLogic) guildInfoFor(ctx context.Context, g *data.GuildData, viewerID uint64) *pb.GuildInfo
```

使用点:`GetPlayerGuild`(viewer=本人)、`GetGuild`(客户端来源 viewer=本人,内部 0)、`CreateGuild`、`SetAnnouncement`、全部返回快照的写 RPC。写 RPC 的计数在提交之后读,与刚完成的审批一致。

<!-- s2_management_part6.md -->

# S2 管理与审批 + 推送(B2)— 第 6 部分:推送 NotifyGuildChanged

## 13. 服务端实现(`go/guild/internal/logic/push.go`,新建)

### 13.1 接缝

```go
// GuildNotifier 是帮会变更推送的唯一接缝。
// 契约(实现必须全部满足):
//   - 只在 MySQL 提交成功之后调用;
//   - 至多一次、异步,Notify 立即返回,不阻塞 RPC;
//   - 失败 / 离线只记日志与指标,绝不影响 RPC 结果;
//   - 收件人去重、丢弃 0;
//   - 载荷只有 GuildChangedS2C 四个字段,客户端据此拉取,不下发快照。
type GuildNotifier interface {
	Notify(change *pb.GuildChangedS2C, recipients []uint64)
}

// NoopNotifier:未配 Kafka(Brokers 为空 → svcCtx.KafkaWriter 为 nil)或单测时使用。
type NoopNotifier struct{}

func (NoopNotifier) Notify(*pb.GuildChangedS2C, []uint64) {}
```

### 13.2 Kafka 实现

```go
const guildPushBudget = 3 * time.Second // 整批预算(读会话 + 写 Kafka),同 team-system G.3

type KafkaGuildNotifier struct {
	writer    *kafkago.Writer
	builder   kafkautil.GateCommandBuilder
	sessions  *redis.Client // svcCtx.PlayerLocatorRedisClient(player:session:*)
	messageID uint32        // game.GuildServiceNotifyGuildChangedMessageId
}

// NewGuildNotifier 任一依赖为 nil 即返回 NoopNotifier 并打一条 Info —— kafkautil.PushToPlayer
// 不判 nil writer(gate_push.go:50-78),这里是 nil 安全的唯一收口。
func NewGuildNotifier(w *kafkago.Writer, b kafkautil.GateCommandBuilder, sessions *redis.Client) GuildNotifier {
	if w == nil || b == nil || sessions == nil {
		logx.Info("[guild] Kafka writer / gate builder / session Redis 未配置:帮会推送关闭,客户端靠拉取刷新")
		return NoopNotifier{}
	}
	return &KafkaGuildNotifier{writer: w, builder: b, sessions: sessions,
		messageID: game.GuildServiceNotifyGuildChangedMessageId}
}

func (n *KafkaGuildNotifier) Notify(change *pb.GuildChangedS2C, recipients []uint64) {
	ids := uniqueNonZero(recipients)
	kind := changeKindLabel(change.GetKind())
	if len(ids) == 0 {
		return
	}
	body, err := proto.Marshal(change)
	if err != nil {
		guildPushTotal.Inc(kind, "error")
		logx.Errorf("[guild] marshal GuildChangedS2C kind=%s guild=%d: %v", kind, change.GetGuildId(), err)
		return
	}
	safego.Go("guild.push", func() {
		ctx, cancel := context.WithTimeout(context.Background(), guildPushBudget)
		defer cancel()
		online, offline, err := loadGateInfos(ctx, n.sessions, ids)
		if err != nil {
			guildPushTotal.Add(float64(len(ids)), kind, "session_error")
			logx.Errorf("[guild] push kind=%s guild=%d: read sessions: %v", kind, change.GetGuildId(), err)
			return
		}
		if offline > 0 {
			guildPushTotal.Add(float64(offline), kind, "offline")
		}
		if len(online) == 0 {
			return
		}
		if len(online) == 1 {
			err = kafkautil.PushToPlayer(ctx, n.writer, n.builder, online[0], n.messageID, body)
		} else {
			err = kafkautil.BroadcastToPlayers(ctx, n.writer, n.builder, online, n.messageID, body)
		}
		if err != nil {
			guildPushTotal.Add(float64(len(online)), kind, "error")
			logx.Errorf("[guild] push kind=%s guild=%d recipients=%d: %v", kind, change.GetGuildId(), len(online), err)
			return
		}
		guildPushTotal.Add(float64(len(online)), kind, "ok")
	})
}
```

`BroadcastToPlayers` 对个别坏条目(空 instance id)会跳过并返回首个错误,其余照发;此时整批记 `error` 偏保守,可接受(日志里有明细)。

### 13.3 会话解码共用(改 `online_status_resolver.go`)

抽出共享函数,两处共用同一份“什么算在线”的判定(DRY):

```go
// onlineSessionFrom 解一条 MGET 结果;只有 SESSION_STATE_ONLINE 算在线(与 match push.go:41-44 同判据)。
func onlineSessionFrom(value any) (*plpb.PlayerSession, bool) {
	var raw []byte
	switch v := value.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		return nil, false
	}
	session := &plpb.PlayerSession{}
	if err := proto.Unmarshal(raw, session); err != nil {
		return nil, false
	}
	return session, session.GetState() == plpb.PlayerSessionState_SESSION_STATE_ONLINE
}

func playerSessionKey(playerID uint64) string { return fmt.Sprintf("%s%d", playerSessionKeyPrefix, playerID) }
```

`BatchResolve` 的循环体改为调用 `onlineSessionFrom`,行为不变。`push.go`:

```go
// loadGateInfos:MGET player:session:*,返回在线者的网关路由信息与离线人数。
// GateInstanceID 为空的会话按离线计(PushToPlayer 会 fail-closed 拒绝,提前剔除避免整批报错)。
func loadGateInfos(ctx context.Context, rdb *redis.Client, ids []uint64) ([]kafkautil.PlayerGateInfo, int, error)
```

### 13.4 指标(同文件)

```go
var guildPushTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "guild", Name: "push_total",
	Help:   "NotifyGuildChanged 推送结果,按收件人计数。outcome: ok / offline / error / session_error。",
	Labels: []string{"kind", "outcome"},
})
```

`kind` 由 `changeKindLabel` 映射为固定小写串(`member_joined`、`member_left`、`member_kicked`、`role_changed`、`leader_transferred`、`disbanded`、`application_received`、`application_rejected`、`announcement_changed`,其余 `other`),不用 `String()`,避免枚举改名改变序列名。**不带 player_id / guild_id label**(AGENTS §9)。

### 13.5 logic 侧助手(`push.go`)

```go
// notify:组装 S2C 并交给 notifier。guildID 显式传(解散后已没有快照)。
func (l *GuildLogic) notify(kind pb.GuildChangeKind, guildID, actor, target uint64, recipients []uint64) {
	l.notifier.Notify(&pb.GuildChangedS2C{Kind: kind, GuildId: guildID, ActorPlayerId: actor, TargetPlayerId: target}, recipients)
}

// membersExcept:快照成员中去掉 excluded 的 player_id(顺序按快照,即 player_id 升序)。
func membersExcept(g *data.GuildData, excluded ...uint64) []uint64
```

### 13.6 Kafka writer(改 `svc/servicecontext.go:79-94`,该文件有他人未提交改动,只改这一段)

在 `&kafkago.Writer{...}` 中追加三项(照 match `servicecontext.go:144-146`):

```go
			// 推送是至多一次的“提示拉取”:不重试(重试可能乱序且拖长预算),单次写 3s 封顶,
			// 与 logic.guildPushBudget 一致;BatchTimeout 缩到 10ms,默认 1s 会让每条推送多等 1 秒。
			BatchTimeout: 10 * time.Millisecond,
			MaxAttempts:  1,
			WriteTimeout: 3 * time.Second,
```

import 增 `time`。

### 13.7 `guild.go` 接线

`guildLogic := logic.NewGuildLogic(repo, guildIDs, onlineResolver, mergeFence, homeZones,` 追加
`logic.WithNotifier(logic.NewGuildNotifier(svcCtx.KafkaWriter, svcCtx.GateCommandBuilder, svcCtx.PlayerLocatorRedisClient)), logic.WithApplyPushGate(repo.TryMarkApplyPush))`。

## 14. 推送矩阵

“快照”指事务内提交前读到的权威成员表。操作者一律不推(他的响应已带最新状态)。

| 触发 | kind | actor | target | 收件人 |
|---|---|---|---|---|
| 申请(新建行,且 60s 冷却键拿到) | APPLICATION_RECEIVED | 申请人 | 申请人 | 事务内读到的长老 + 帮主 |
| 申请(新建行,冷却中,即 60s 内撤回后重申) | — | | | 不推(第 5b 部分 §12.2) |
| 申请(同帮刷新) | — | | | 不推 |
| 撤回申请 | — | | | 不推 |
| 审批通过 | MEMBER_JOINED | 审批人 | 申请人 | 快照全体(含申请人)除审批人 |
| 审批拒绝 | APPLICATION_REJECTED | 审批人 | 申请人 | 申请人 |
| 任免(角色确有变化) | ROLE_CHANGED | 帮主 | 目标 | 快照全体除帮主 |
| 踢人 | MEMBER_KICKED | 操作者 | 被踢者 | 快照(已不含被踢者)除操作者 + 被踢者 |
| 转让 | LEADER_TRANSFERRED | 原帮主 | 新帮主 | 快照全体除原帮主 |
| 退帮 | MEMBER_LEFT | 退帮者 | 退帮者 | 快照剩余全体 |
| 解散 | DISBANDED | 帮主 | 0 | 解散前全体除帮主 |
| 公告 | ANNOUNCEMENT_CHANGED | 操作者 | 0 | 事务内快照成员除操作者 |
| 建帮 | — | | | 不推 |

`GUILD_CHANGE_KIND_FUNDS_CHANGED / LEVEL_UP / ACTIVITY_CHANGED / DELIVERY_DONE` 由 B5/B6 使用同一 `notify` 助手。

## 15. 语义、预算与上线顺序

### 15.1 投递语义

- 至多一次:离线不推;gate 找不到会话时丢弃(`gate_event_handler.cpp:307-325`);Kafka 写失败不重试。
- 客户端真相来自拉取:收到推送 → 排队一次 `GetPlayerGuild`(或申请列表);错过推送 → 打开帮会界面 / 手动刷新时同样拿到最新状态。
- 推送与响应的时序:推送在提交后异步发出,可能早于操作者之外的人任何请求,也可能晚于其下一次拉取;两种顺序下拉取结果都正确,重复拉取无副作用。

### 15.2 预算

- 每次写 RPC 同步部分:归属 zone 查询 ≤1500ms(`DefaultHomeZoneLookupTimeout`;审批通过时两次查询并行)+ 闸门 EXISTS + 事务(单次锁等待 ≤1000ms,语句 ≪100ms)+ 装配(在线 MGET、待审计数)≈ 3100ms,低于 zrpc `Timeout=4000` 减 B1 回包余量 500ms。明细见第 3 部分 §6.2a。
- 推送在 `safego.Go` 协程里,3s 独立预算,不占 RPC 时间。单帮成员 ≤100(`constants.MaxGuildMembersCap`,启动校验强制),一次 MGET + 按网关分组的少量 Kafka 消息。
- 缓存失效后台重试(第 3b 部分 §6.4.1)同样在 `safego` 协程里,≤3s,不占 RPC 时间。

### 15.3 崩溃窗口

| 时刻 | 后果 |
|---|---|
| 提交后、`Notify` 前崩溃 | 无推送;相关玩家下次打开 / 刷新自愈 |
| `safego` 协程内崩溃(panic) | safego 兜住并计 `safego_panic_total{point="guild.push"}`;本次推送丢失 |
| 会话 Redis 读失败 | 整批 `session_error`,不推 |
| 申请冷却键 Redis 读写失败 | 不推 `APPLICATION_RECEIVED`,只记 Info;审批人刷新时看到 |

### 15.4 上线顺序(message id 变化)

1. proto-gen 后重建并重启 `client_rpc_router`(路由表)与 C++ gate(`IsClientMessageId`)。旧 gate 不认识新 id 时会按非法包计数并断开客户端(team-system.md G.3 已记录),所以网关必须先于客户端。
2. 重启 guild(新二进制 + 新配表)。
3. robot 的编译级改写随 B2s 一起落地(第 9 部分 §25),B2s 之后 robot 仍可编译、原冒烟可跑;客户端与 robot 管理段在 B2c 更新。B2s 与 B2c 之间旧客户端的“加入”按钮会因 `JoinGuild` 路由不存在而失败(本地开发可接受,B2c 修复)。

本地开发一次性全栈重启即可;项目未上线,无灰度要求。

<!-- s2_management_part7.md -->

# S2 管理与审批 + 推送(B2)— 第 7 部分:客户端(`E:\work\mmorpg-client`,批次 B2c)

客户端全部 UGUI 代码构建(不用 FairyGUI)。坐标沿用 `QdaoUguiFactory.CreateRect` 左上角、y 向下的约定;帮会正文区 `GuildContent` 为 1508×610。

## 16. 协议生成

- `tools/gen_messageids.ps1`(他人有未提交改动,只改帮会段 `:35-43`):删除 `"GuildServiceJoinGuild" = "JoinGuild"`;追加
  ```powershell
    "GuildServiceSetGuildMemberRole"       = "SetGuildMemberRole"
    "GuildServiceKickGuildMember"          = "KickGuildMember"
    "GuildServiceTransferGuildLeader"      = "TransferGuildLeader"
    "GuildServiceApplyJoinGuild"           = "ApplyJoinGuild"
    "GuildServiceCancelGuildApplication"   = "CancelGuildApplication"
    "GuildServiceListMyGuildApplications"  = "ListMyGuildApplications"
    "GuildServiceListGuildApplications"    = "ListGuildApplications"
    "GuildServiceReviewGuildApplication"   = "ReviewGuildApplication"
    # 推送占位:只收不发(服务端白名单拒绝客户端调用)
    "GuildServiceNotifyGuildChanged"       = "NotifyGuildChanged"
  ```
- `tools/gen_proto.ps1` 不改(`proto/guild/guild.proto` 与 `guild_error_tip.proto` 已在清单;`guild_db.proto` 不进客户端)。
- 生成物(不手改):`Assets/Scripts/Net/MessageIds.cs`、`Guild.cs`(Guildpb)、`GuildErrorTip.cs`。

## 17. `Assets/Scripts/Game/Guild/GuildClient.cs`

### 17.1 角色助手(同文件,类外)

```csharp
/// <summary>与 go/guild constants.Rank 同表:0 成员 / 1 长老 / 3 帮主;2 与其它值为 RankNone。服务端仍以 MySQL 复核。</summary>
public static class GuildRoles
{
    public const uint Member = 0, Officer = 1, Leader = 3;
    public const int RankNone = 0, RankMember = 1, RankOfficer = 2, RankLeader = 3;
    public static int Rank(uint role) => role switch { Member => RankMember, Officer => RankOfficer, Leader => RankLeader, _ => RankNone };
    public static bool CanKick(uint actor, uint target) => Rank(actor) >= RankOfficer && Rank(target) != RankNone && Rank(actor) > Rank(target);
}
```

`CanEditAnnouncement` 改为 `Info != null && GuildRoles.Rank(Role) >= GuildRoles.RankOfficer`(role 2 仍为 false)。

### 17.2 新状态

```csharp
public IReadOnlyList<GuildApplicationView> MyApplications { get; private set; }   // null = 未加载
public IReadOnlyList<GuildApplicantView> Applicants { get; private set; }         // null = 未加载
public bool RefreshQueued { get; private set; }        // 推送要求重拉 GetPlayerGuild
public bool ApplicantsQueued { get; private set; }     // 推送要求重拉申请列表
public bool MyApplicationsQueued { get; private set; } // 推送要求重拉本人申请
public bool IsOfficerOrLeader => Info != null && GuildRoles.Rank(Role) >= GuildRoles.RankOfficer;
public bool CanAssignRoles => Info != null && GuildRoles.Rank(Role) == GuildRoles.RankLeader;
public bool CanKick(GuildMember target) => Info != null && target != null && target.PlayerId != PlayerId && GuildRoles.CanKick(Role, target.Role);
public bool HasApplied(ulong guildId) { if (MyApplications != null) foreach (var a in MyApplications) if (a.GuildId == guildId) return true; return false; }
```

私有字段 `private string _pendingNotice;`:推送带来的提示(被请离 / 解散),由 `Refresh` 回包落到 `Status`,避免被 `Request` 的“正在读取帮会,请稍候…”与 NotInGuild 分支文案覆盖。

`Reset()` 追加清空:`MyApplications = null; Applicants = null; RefreshQueued = ApplicantsQueued = MyApplicationsQueued = false; _pendingNotice = null;`。`Refresh()` 开头记 `int before = _generation; bool queued = RefreshQueued; RefreshQueued = false;`,调用 `Request(...)` 后若 `_generation == before`(`Request` 只在真正发出时 `++_generation`,被 Busy / 隔离 / 未就绪挡掉则不变)则恢复 `RefreshQueued = queued`,由下一帧 `DrainQueued` 再发。不用 `Busy` 判断:同步回调的替身(`GuildUiVerification`)在 `Request` 返回前就已清掉 Busy。

### 17.3 构造函数注册推送

```csharp
_net.RegisterNotify(MessageIds.NotifyGuildChanged, HandleGuildChanged);
```

一个 message id 只有一个处理器(`GameClient.OnNotify` 覆盖写),只在 GuildClient 注册。

```csharp
private void HandleGuildChanged(MessageContent content)
{
    if (_disposed || content == null) return;
    GuildChangedS2C change;
    try { change = GuildChangedS2C.Parser.ParseFrom(content.SerializedMessage); }
    catch (InvalidProtocolBufferException) { return; }
    bool mine = Info != null && change.GuildId == Info.GuildId;
    bool aboutMe = change.TargetPlayerId == PlayerId;
    if (mine)
    {
        if (change.Kind == GuildChangeKind.ApplicationReceived) ApplicantsQueued = true;
        else RefreshQueued = true;
        if (change.Kind == GuildChangeKind.Disbanded) _pendingNotice = "帮会已被帮主解散。";
        else if (change.Kind == GuildChangeKind.MemberKicked && aboutMe) _pendingNotice = "你已被请离帮会。";
        if (_pendingNotice != null) Status = _pendingNotice; // 立即可见;Refresh 回包后再次落定
    }
    else if (Info == null && aboutMe && change.Kind == GuildChangeKind.MemberJoined)
    { RefreshQueued = true; Status = "入帮申请已通过，正在读取帮会信息…"; }
    else if (Info == null && aboutMe && change.Kind == GuildChangeKind.ApplicationRejected)
    { MyApplicationsQueued = true; Status = "有一份入帮申请未获通过。"; }
    else return;
    Changed?.Invoke();
}

/// <summary>由界面每帧调用;一次只发一个排队请求,Busy / 隔离 / 未就绪时什么也不做。</summary>
public void DrainQueued(bool applicantsVisible)
{
    if (Busy || RequiresReconnect || !_net.IsReady) return;
    if (RefreshQueued) { Refresh(); return; }
    if (ApplicantsQueued && IsOfficerOrLeader)
    {
        ApplicantsQueued = false;
        // 申请视图可见 → 重拉列表;不可见 → 重拉 GetPlayerGuild,服务端重算 pending_application_count,成员页角标随之更新。
        if (applicantsVisible) LoadApplications(); else Refresh();
        return;
    }
    if (MyApplicationsQueued) { MyApplicationsQueued = false; LoadMyApplications(); }
}
```

推送不直接改 `Info`(拉取才是真相)。

`Refresh()` 回包的 `KGuildNotInGuild` 分支改为:

```csharp
{
    Info = null; Applicants = null; HasLoaded = true;
    Status = _pendingNotice ?? "尚未加入帮会，和同道相聚于此。"; _pendingNotice = null;
    if (MyApplications == null) MyApplicationsQueued = true; // 入帮时已清空;退帮 / 被踢 / 解散后由 DrainQueued 重拉
    return;
}
```

成功分支(`Apply(response.Guild)` 之后)若 `_pendingNotice != null` 也清空(帮会仍在,说明提示已过时)。

### 17.4 新请求方法

全部走现有 `Request<T>`(单请求在途、代次校验、传输错误隔离)。写方法成功后应用响应快照,**不再额外 Refresh**。

```csharp
public void SetMemberRole(ulong target, uint role)
{
    if (!CanAssignRoles || target == 0 || target == PlayerId || (role != GuildRoles.Member && role != GuildRoles.Officer))
    { Reject("仅帮主可任免长老。"); return; }
    Request(MessageIds.SetGuildMemberRole, new SetGuildMemberRoleRequest { TargetPlayerId = target, Role = role },
        SetGuildMemberRoleResponse.Parser, r => { if (AcceptWrite(r.ErrorMessage)) Apply(r.Guild); });
}
public void Kick(ulong target)            // 前置:Info 中找到目标且 CanKick(member),否则 Reject("当前身份无权请离该成员。")
public void TransferLeader(ulong target)  // 前置:CanAssignRoles && 目标在册 && 非自己,否则 Reject("仅帮主可转让帮会。")
public void ApplyToJoin(ulong guildId)    // 前置(不叫 Apply:与私有 Apply(GuildInfo) 同名易混):CanJoin() && guildId != 0,否则 Reject("已入帮时不能申请其他帮会。")
    // 成功:Status = "申请已提交，等待帮主或长老审批。"; LoadMyApplications();
public void CancelApplication(ulong guildId) // 前置:HasLoaded && Info == null && guildId != 0
    // 成功:Status = "已撤回入帮申请。"; LoadMyApplications();  KGuildApplicationNotFound 也 LoadMyApplications()
public void LoadMyApplications()          // 前置:HasLoaded && Info == null;成功:MyApplications = r.Applications.ToList()
public void LoadApplications()            // 前置:IsOfficerOrLeader;成功:Applicants = r.Applicants.ToList(); Status = 条数文案
public void Review(ulong applicant, bool approve) // 前置:IsOfficerOrLeader && applicant != 0
    // 成功:Apply(r.Guild); Status = approve ? "已同意入帮申请。" : "已拒绝入帮申请。"; LoadApplications();
    // KGuildApplicationNotFound / KGuildFull:LoadApplications()
```

`AcceptWrite(tip)`:先调用 `Accept(tip)`;若 tip 为 `KGuildTargetNotMember` 或 `KGuildNotInGuild`,置 `RefreshQueued = true`(界面下一帧自动重拉)。`KGuildBusyRetry` 只显示文案,不排队、不隔离(业务 tip 走 success 回调,`RequiresReconnect` 不变)。`Join()` 删除。

**私有 `Apply(GuildInfo info)`**(`GuildClient.cs:152`)在 `Info = info.Clone(); HasLoaded = true;` 之后追加 `MyApplications = null;`:服务端入帮时已删光本人申请,本地列表作废;日后退帮经 `Refresh` NotInGuild 分支重拉。

**`SaveAnnouncement`**(`GuildClient.cs:123`)回调改为 `{ if (Accept(response.ErrorMessage)) Apply(response.Guild); }`(`Apply` 自带“快照为空 / 非本人”的防御与状态文案),不再 `Refresh()`(服务端回带快照,见第 2 部分 §5.3)。`Browse()` 成功回调末尾追加:`if (HasLoaded && Info == null && MyApplications == null) MyApplicationsQueued = true;`(排行页首次打开时顺带拉本人申请,不在回调里连发)。

### 17.5 `Accept` 新增 tip 文案

```csharp
(uint)guild_error.KGuildZoneMerging => "区服合并维护中，帮会操作暂停，请稍后再试。",
(uint)guild_error.KGuildTargetNotMember => "对方已不在本帮会，列表即将刷新。",
(uint)guild_error.KGuildCannotTargetSelf => "不能对自己执行此操作。",
(uint)guild_error.KGuildRankTooLow => "你的帮会职位不足以执行此操作。",
(uint)guild_error.KGuildOfficerLimit => "长老人数已达当前帮会等级上限。",
(uint)guild_error.KGuildApplicationNotFound => "申请不存在或已失效，列表已刷新。",
(uint)guild_error.KGuildApplicationLimit => "同时进行中的入帮申请已达上限，请先撤回其他申请。",
(uint)guild_error.KGuildApplicationQueueFull => "该帮会待审申请已满，请稍后再试。",
(uint)guild_error.KGuildBusyRetry => "帮会操作繁忙，请稍后重试。",
```

## 18. `Assets/Scripts/UI/Ugui/Guild/GuildWindow.cs`

### 18.1 事件

删除 `JoinRequested`。新增:

```csharp
public event Action<ulong> ApplyRequested, CancelApplicationRequested, KickRequested, TransferRequested;
public event Action<ulong, uint> RoleRequested;
public event Action<ulong, bool> ReviewRequested;
public event Action ApplicationsRequested;
public bool ShowingApplications => IsVisible && Page == GuildPage.Members && _showApplications;
```

新字段 `private bool _showApplications; private int _applicationPage;`,`ResetSession()` 与 `SetClient` 换帮时清零。

### 18.2 成员页顶栏(`RenderMembers`,正文坐标)

| 控件 | 现 | 改为 |
|---|---|---|
| `OnlineGuildMembers` | (0,0,295,66) | 不变 |
| 标签“按角色编号查找” | (344,3,300,58) | 文案“按编号查找”,(316,3,230,58) |
| `GuildMemberSearch` | (640,0,575,66) | (556,0,470,66) |
| `SearchGuildMembers` | (1236,0,272,66) | (1040,0,196,66) |
| `GuildApplicationsToggle`(新,仅长老 / 帮主) | — | (1252,0,256,66),文案 `_showApplications ? "返回成员" : "入帮申请 " + Info.PendingApplicationCount`(角标随 `GetPlayerGuild` / 写响应快照更新;收到 `ApplicationReceived` 且列表不可见时 `DrainQueued` 会发 `Refresh`,见 §17.3);点击切换 `_showApplications`,切到申请视图时 `_applicationPage = 0` 并 `ApplicationsRequested?.Invoke()`;`enabled: !Busy` |

`_showApplications` 为真且 `IsOfficerOrLeader` 时渲染 §18.4 申请视图,否则渲染成员列表(身份被降为成员后自动回到成员列表)。

### 18.3 成员行(y = 86 + i×86,行框不变)

| 元素 | 现 | 改为 |
|---|---|---|
| 名字 | (90,y+12,600,56) | (90,y+12,420,56);文案 `string.IsNullOrEmpty(member.Name) ? "道友 · " + member.PlayerId : member.Name`,自己加“(我)” |
| 身份 | (724,y+12,205,56) Gold | (524,y+12,150,56) Gold |
| 贡献 | (950,y+12,300,56) “贡献  {Contribution}” | (686,y+12,260,56) “贡献  {ContributionTotal}” |
| 在线 | (1330,y+10,162,56) | (958,y+10,120,56) |
| 任免槽(帮主可见,非自己) | — | `GuildMemberPromote_{id}` “任长老”(目标 role 0,`enabled: !Busy && Info.OfficerCount < Info.MaxOfficers`)或 `GuildMemberDemote_{id}` “免长老”(目标 role 1);(1096,y+9,128,60),fontSize 26 |
| 转让槽(帮主可见,非自己) | — | `GuildMemberTransfer_{id}` “转让”,(1232,y+9,128,60) |
| 请离槽(`CanKick(member)`) | — | `GuildMemberKick_{id}` “请离”,(1368,y+9,128,60) |

三个按钮全部先 `Confirm`:
- 任长老:“确认任命长老” / “任命 {名字} 为长老。当前长老 {OfficerCount}/{MaxOfficers}。” → `RoleRequested(id, 1)`。
- 免长老:“确认免去长老” / “{名字} 将恢复为帮众。” → `RoleRequested(id, 0)`。
- 转让:“确认转让帮主” / “转让后 {名字} 成为帮主,你将成为长老(长老已满则为帮众)。此操作无法撤回。” → `TransferRequested(id)`。
- 请离:“确认请离成员” / “{名字} 将被请离帮会。” → `KickRequested(id)`。

### 18.4 申请视图(同一正文区,5 行一页)

- 数据:`_client.Applicants`;为 null 时居中提示“正在读取入帮申请…”(30,210,1400,100);为空时“暂无待审申请。”。
- 行 y = 86 + i×86:行框 `GuildListRow`(0,y,1508,78);名字(90,y+12,520,56)同 §18.3 兜底规则;在线(630,y+12,140,56);剩余时长(790,y+12,280,56)文案 `"剩余 " + Math.Max(1, (int)Math.Ceiling((expire_ms - nowMs) / 3600000.0)) + " 小时"`,`nowMs` 取 `DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()`(仅展示);`GuildApplicationReject_{id}` “拒绝”(1100,y+7,190,64);`GuildApplicationApprove_{id}` “同意”(1306,y+7,190,64,primary)。两按钮 `enabled: !Busy`,直接触发 `ReviewRequested(id, false/true)`(可再次申请、可再次审批,不需要确认框)。
- `Pager(_body, "Applications", _applicationPage + 1, pages, delta => { _applicationPage += delta; Render(); }, true)`。

### 18.5 排行页按钮(`RenderRanking`)

`JoinGuild_{id}` 改名 `ApplyGuild_{id}`,几何不变 (1280,y+7,210,64):
- 我的帮会:文案“我的帮会”,禁用。
- `_client.HasApplied(id)`:文案“撤回申请”,`Confirm("确认撤回申请", "帮会名称：" + name + "\n撤回后可重新申请。", () => CancelApplicationRequested?.Invoke(id))`。
- 否则:文案“申请”,`Confirm("确认申请加入", "帮会名称：" + name + "\n申请需帮主或长老审批，逾期未处理将自动失效。", () => ApplyRequested?.Invoke(id))`。
- `enabled: _client?.HasLoaded == true && _client.Info == null && !Busy`(同现状)。

### 18.6 其它

- 总览“我的贡献”:`member.Contribution` → `member.ContributionTotal`。
- 未入帮总览(`RenderOverview` 的 info==null 分支)底部文字 (24,532,1460,55):`MyApplications` 非空时改为“已提交 N 份入帮申请，等待审批中。”,否则保持原文案。
- `RoleName`:删去 `2 => "副帮主"`(2 不启用,落到默认“帮众”)。

## 19. `GuildUiRoot.cs`

`Awake` 中删 `JoinRequested` 订阅,新增:

```csharp
_window.ApplyRequested += id => { if (_available) _client?.ApplyToJoin(id); };
_window.CancelApplicationRequested += id => { if (_available) _client?.CancelApplication(id); };
_window.RoleRequested += (id, role) => { if (_available) _client?.SetMemberRole(id, role); };
_window.KickRequested += id => { if (_available) _client?.Kick(id); };
_window.TransferRequested += id => { if (_available) _client?.TransferLeader(id); };
_window.ReviewRequested += (id, approve) => { if (_available) _client?.Review(id, approve); };
_window.ApplicationsRequested += () => { if (_available) _client?.LoadApplications(); };
```

`Update()` 末尾(`_available` 为真、键盘处理之前):`if (_window.IsVisible) _client?.DrainQueued(_window.ShowingApplications);`。窗口关闭时不拉取,`Toggle()` 打开时已有 `Refresh()`。

## 20. `Assets/Editor/Guild/GuildUiVerification.cs`

- `FixtureTransport.Fixture()`(`:180-186`):`Contribution =` 改 `ContributionTotal =`;`GuildInfo` 初始化增 `OfficerCount = 1, MaxOfficers = 6, PendingApplicationCount = 3`。
- `FixtureTransport.Call`(`:161-176`)在 `GetGuildRank` 分支之后增两个分支:`MessageIds.ListGuildApplications` 返回 3 个 `GuildApplicantView`(player_id 20001–20003,`ApplyMs` = 当前毫秒 − 1h,`ExpireMs` = 当前毫秒 + 48h,第 1 个 `Online = true`);`MessageIds.ListMyGuildApplications` 返回空的 `ListMyGuildApplicationsResponse`。
- `Capture`(`:103-104` 接线处)追加 `window.ApplicationsRequested += () => client.LoadApplications();`。
- 在 `Shoot("07-announcement");` 之后、`window.Back(); net.HasGuild = false;` 之前插入:`window.Back(); window.Show(GuildPage.Members);` → 遍历按钮点击 `GuildApplicationsToggle` → `Shoot("11-applications");`。
- `:128` 的按钮名 `"JoinGuild_88001"` 改为 `"ApplyGuild_88001"`。

<!-- s2_management_part8.md -->

# S2 管理与审批 + 推送(B2)— 第 8 部分:Go 测试

B2 不涉及 C++ 业务代码,无 gtest;C++ 只做表库登记后的编译验证(第 9b 部分 §28 B2s 第 2 步)。

## 21. Go 单元测试(无外部依赖)

### 21.1 `internal/constants/constants_test.go`
- `tipCodes()` 追加 9 个新 const(`TestNoHandWrittenTipCodes` 自动覆盖)。
- `TestRankOrdering`:`Rank(0)=1`、`Rank(1)=2`、`Rank(3)=3`、`Rank(2)=0`、`Rank(4)=0`、`Rank(math.MaxUint32)=0`;`AssignableRole` 仅 0、1 为真。

### 21.2 `internal/session/session_test.go`
- `internalOnly` 加 `NotifyGuildChanged`;新增 `TestNotifyPlaceholdersAreNeverClientCallable`(第 2 部分 §5.8)。

### 21.3 `internal/data/guild_manage_repo_test.go`(新建;本节用例不需要 DSN)
- `TestCanKickMatrix`:`(3,1,true) (3,0,true) (1,0,true) (1,1,false) (1,3,false) (0,0,false) (3,3,false) (2,0,false) (3,2,false)`。
- `TestCanAssignTransferReview`:`canAssignRole` / `canTransferLeader` 仅 3 为真;`canReviewApplications` 对 1、3 为真,对 0、2 为假。
- `TestDemotedLeaderRole`:`(0,2)→1`、`(1,2)→1`、`(2,2)→0`、`(3,2)→0`、`(0,0)→0`。
- `TestMySQLErrClassifiers`:1213 → 仅 `isDeadlock`;1205 → 仅 `isLockWaitTimeout`;1062、非 MySQL 错误、nil → 两者都 false。
- `TestCommitThenUnwraps`:`errors.Is(errCommitThen{ErrApplicationNotFound}, ErrApplicationNotFound)` 为真。
- `TestRetryOnDeadlock`(驱动 `retryOnDeadlock`,`run` 为假函数):
  - 前 2 次返回 `&mysqlDriver.MySQLError{Number:1213}`、第 3 次返回 nil → 结果 nil,`run` 调用 3 次;
  - 连续 3 次 1213 → `ErrWriteConflict`,调用 3 次;
  - 第 1 次 1205 → `ErrWriteConflict`,调用 1 次(不重试);
  - 第 1 次 1062 → 原样返回,调用 1 次;
  - ctx 已取消 + 首次 1213 → 返回 `context.Canceled`,调用 1 次。
- `TestRetryResultsDoNotAccumulate`:按 §6.2 约定写一个假事务函数(`fn` 内声明 `ids []uint64`,append 3 个,成功才赋外层);`run` 第 1 次在 append 后返回 1213,第 2 次成功 → 外层 `ids` 恰为 3 个。
- `TestWithLockWaitTimeout`:`"u:p@tcp(127.0.0.1:3306)/mmorpg_guild?charset=utf8mb4&parseTime=true&loc=Local"` → 输出经 `mysqlDriver.ParseDSN` 后 `Params["innodb_lock_wait_timeout"]=="1"`、`DBName`、`charset`、`ParseTime`、`Loc` 不变;已带 `innodb_lock_wait_timeout=50` 时被覆盖为 1;无法解析的 DSN → 错误文案不含原文。
- `TestInvalidateAfterCommitRetriesInBackground`(miniredis,repo 按 `guild_repo_test.go` 现有 miniredis 方式构造,db 传 nil;`invalidateRetryDelays` 临时改为 `{1ms,1ms,1ms}`):预置 `guild:v2:{7}` 与玩家映射键 → `mr.SetError("boom")` → 调 `invalidateAfterCommit(ctx,"kick",7,42)` 立即返回 → 5ms 后 `mr.SetError("")` → `require.Eventually`(1s)两键都不存在,且 `invalidateGaveUp` 未被调用;另一组一直 SetError → `require.Eventually` 观察到 `invalidateGaveUp("kick")` 恰被调用 1 次(测试临时替换该包级变量为记录器,结束时还原)。
- `TestApplyPushGateCooldown`(miniredis):同一 (guild, player) 第 1 次 true、第 2 次 false;`mr.FastForward(61s)` 后 true;`mr.SetError` 时 false。

### 21.4 `internal/logic/guild_manage_logic_test.go`(新建,repo 传 nil,误触即 panic,以此证明前置顺序)
- `TestManagementRPCsRequireClientSession`:8 个新 RPC 用 `context.Background()` 调用 → `codes.PermissionDenied`。
- `TestWriteRPCsRefusedWhileMerging`:`fakeFence{merging:true}`、`&fakeHomeZones{zone: 2}`、`clientCtx(42)`;对 `SetGuildMemberRole / KickGuildMember / TransferGuildLeader / ApplyJoinGuild / CancelGuildApplication / ReviewGuildApplication{applicant:7, approve:false} / LeaveGuild / DisbandGuild / SetAnnouncement / CreateGuild` 逐个断言 `ErrorMessage.Id == constants.ErrZoneMerging` 且 `fence.lastZone == 2`。
- `TestFenceUnreadableReturnsZoneMergingTip`:`fakeFence{err: ...}` → 同上 tip。
- `TestHomeZoneUnknownPrecedesFence`:`&fakeHomeZones{zone: 0}` → `ErrHomeZoneUnknown`,`fence.calls == 0`。
- `TestTargetValidationBeforeRepo`:target=0 → `ErrTargetNotMember`;target=42 → `ErrCannotTargetSelf`;`SetGuildMemberRole{role:3}` 与 `{role:2}` → `ErrNoPermission`;`ReviewGuildApplication{applicant:0}` → `ErrApplicationNotFound`;`{applicant:42}` → `ErrCannotTargetSelf`;`ApplyJoinGuild{guild_id:0}` → `ErrGuildNotFound`。
- `TestReviewApproveChecksApplicantZoneBeforeRepo`(`fakeFence{}` 放行;本文件新建 `perPlayerZones{mu sync.Mutex; zones map[uint64]uint32; errs map[uint64]error; calls map[uint64]int}`,并发安全,供 `-race` 下的并行查询使用;审批人 42 → zone 2):申请人 7 映射缺失(0)→ tip `ErrApplicationNotFound`;申请人 7 查询报错 → 返回该 error;`approve:false` 时不查申请人(`calls[7] == 0`,随后因 repo 为 nil 在 `operatorGuild` panic —— 用 `require.Panics` 断言确实走到了 repo)。
- `TestMembersExcept`:快照 3 人,排除操作者后顺序不变;排除不存在的 id 无影响。
- `TestNewGuildNotifierNilSafe`:三个依赖任一为 nil 均返回 `NoopNotifier`;`Notify` 不 panic。
- `TestLoadGateInfosFiltersOffline`:miniredis 写入 4 个 `player:session:*`(ONLINE+实例 id、OFFLINE、ONLINE 但实例 id 为空、无法解码)+ 1 个不存在的 key → 返回 1 个 `PlayerGateInfo`、`offline == 4`。
- `TestOnlineSessionFromMatchesResolver`:同一组值,`BatchResolve` 的在线集合与 `onlineSessionFrom` 判定一致。
- `TestValidateGuildTables`:合法样例 = 第 1 部分 §3.1/§3.2 默认行(7 列 + 10 级)必须通过;非法样例:rule 为 nil;expire_hours=0、=721;per_player=0、=11;per_guild=0、=501;levels 为空;id 从 2 开始;缺 3 级;`max_officers >= max_members`;`max_members=101`;`max_members` 递减;中间级 `upgrade_cost_funds=0`;末级 `upgrade_cost_funds≠0` —— 每个都报错且文案含字段名。
- `TestChangeKindLabelIsBounded`:枚举 0–13 与一个越界值,返回值都在固定集合内。
- `TestWriteConflictMapsToBusyTip`:`mapWriteErr(ctx, 42, 1, data.ErrWriteConflict)` → tip `ErrBusyRetry`、err 为 nil(不触碰 repo)。

### 21.5 需要同步修改的已有测试
- `merge_fence_test.go`:`TestCreateGuild_RefusedWhileZoneIsMerging` / `TestCreateGuild_FenceUnreadableFailsClosed` 改为 `require.NoError(err)` 且 `resp.GetErrorMessage().GetId() == constants.ErrZoneMerging`;`TestCheckMergeFence_*` 三个用例改测 `mergeFenceTip`。删去对 `ErrZoneMerging` 变量的断言。
- `client_zone_test.go:82-84`:`JoinGuild` 块改为 `ApplyJoinGuild(ctx, &pb.ApplyJoinGuildRequest{GuildId: 1})` 断言 `ErrHomeZoneUnknown`;注释 `:28` 的 `AddMemberInZone` 改为 `ReviewApplication`。`fakeHomeZones`(`:30`,单值、非并发安全)保持不变。
- `server/inband_observability_test.go:62-90`:`JoinGuild` 全部改为 `ApplyJoinGuild`。
- `data/guild_repo_zone_test.go`:`openGuildIntegrationRepo` 的 `sql.Open` 前经 `WithLockWaitTimeout(dsn)`;`TestAddMemberInZoneRejectsGuildFromAnotherZone` 改名 `TestApplyToGuildRejectsGuildFromAnotherZone`(`requiredZone=3` → `ErrGuildZoneMismatch` 且无申请行;`=2` 成功;`=0` 成功)。
- `data/rank_zone_integration_test.go:195`:改为 `res, err := repo.DisbandGuild(ctx, guildID, leaderID)`,`zoneFromDelete := res.ZoneID`。

## 22. Go 真库测试(`GUILD_TEST_MYSQL_DSN`,放 `guild_manage_repo_test.go`)

复用 `openGuildIntegrationRepo`(B1 起内部 `resetGuildSchemaViaMigrate`,miniredis 作缓存)。助手:`seedGuild(t, ctx, db, guildID, zone, level, maxMembers, leader, roles map[uint64]uint32)`;`capOf(n)`;`testRules = ApplicationRules{TTLMs: 72*3_600_000, MaxPerPlayer: 3, MaxPerGuild: 2}`。

| 用例 | 断言 |
|---|---|
| `TestSetMemberRole_PromoteUntilCap` | cap=2:任命 2 人成功,第 3 人 `ErrOfficerLimit`;再任命已是长老者 → `Changed=false` 且无错 |
| `TestSetMemberRole_OfficerCannotAssign` | 长老操作 → `ErrRankTooLow`;库中 role 未变 |
| `TestSetMemberRole_StaleCachedRoleIgnored` | 先缓存帮主身份,再 SQL 把操作者降为成员 → `ErrRankTooLow` |
| `TestKickMember_Matrix` | 帮主踢长老 ✓;长老踢长老 / 帮主 → `ErrRankTooLow`;踢不存在者 → `ErrTargetNotMember`;被踢者 `player_guild:v2` 键被删;快照不含被踢者 |
| `TestTransferLeader_OldLeaderBecomesOfficerWhenSlotFree` / `_OldLeaderBecomesMemberWhenFull` / `_TargetOfficerFreesSlot` / `_LeaderIDMismatchFailsClosed` | 同原稿(cap=2 已有 1 → 原帮主 1;cap=1 已有 1 → 0;目标即唯一长老 → 1;leader_id 被改 → `ErrLeaderMismatch` 且数据不变) |
| `TestLeaveGuild` | 帮主 → `ErrLeaderCantLeave`;成员 ✓;再退 → `ErrNotGuildMember` |
| `TestApply_RefreshSameGuild` | 第二次 `Inserted=false`,`expire_ms` 变大,行数 1 |
| `TestApply_RefreshWhileGuildFull` | P 申请 G(未满)→ 再 seed 成员到满 → P 再申请 → 成功、`Inserted=false`、`expire_ms` 变大;新玩家 Q 申请 → `ErrGuildFull` |
| `TestApply_PlayerLimitCountsLiveOnly` | 3 条有效 → 第 4 条 `ErrApplicationLimit`;1 条改过期 → 再申请成功且过期行被删 |
| `TestApply_GuildQueueFull` | MaxPerGuild=2:第 3 人 `ErrApplicationQueueFull`;一名申请人入别帮后第 3 人成功(I1) |
| `TestApply_GuildFullAndMember` | 满员帮新申请 → `ErrGuildFull`;已入帮者 → `ErrPlayerAlreadyInGuild`;不存在的帮 → `ErrGuildGone` |
| `TestApply_ReviewerIDs` | 帮主 + 1 长老 + 2 成员 → `ReviewerIDs` 为前两人升序 |
| `TestCancelApplication` | 有效 → nil 且删行;再撤 → `ErrApplicationNotFound`;过期行撤回 → NotFound 且行被删 |
| `TestListApplicantsAppliesI1` | 有效 / 过期 / 已入别帮 → 只返回第 1 行;`CountLiveApplications == 1` |
| `TestReview_ApproveDeletesAllApplicantRows` | P 申请 G1、G2;G1 通过 → P 在 G1,两条申请都没了,P 的映射键被删 |
| `TestReview_ZoneMismatchDeletesApplication` | G1 zone=2;P 申请后 SQL 把 G1 zone 改为 3(模拟 unmerge)→ `ReviewApplication(approve, applicantZone=2)` → `ErrApplicationNotFound`,(G1,P) 行已删,P 无成员行 |
| `TestReview_Reject` / `_ExpiredCommitsDelete` / `_FullKeepsApplication` / `_ApplicantJoinedElsewhere` / `_MemberCannotReview` | 同原稿 |
| `TestCreateGuild_DeletesOwnApplications` | P 申请 G1 → `CreateGuild` 建 G2(P 为帮主)→ G1 `ListApplicants` 为空;`DisbandGuild(G2,P)` 后 G1 `ListApplicants` 仍为空 |
| `TestDisbandGuild_AuthorizesByMySQLAndDeletesApplications` | 非帮主 → `ErrRankTooLow`;帮主 → 全删,`MemberIDs` 升序、`ZoneID` 正确 |
| `TestUpdateAnnouncement_ReturnsSnapshot` | 长老改公告 → 返回快照 `Announcement` 为新值;成员 → `ErrAnnouncementForbidden` |
| `TestLockWaitTimeoutBounded` | 用 `db.BeginTx` 手动 `SELECT ... FROM guild WHERE guild_id=? FOR UPDATE` 占锁不提交;另起 `KickMember` → 在 2.5s 内返回 `ErrWriteConflict`(记录实际耗尽时间,期望约 1s);占锁事务回滚后同一操作成功 |
| `TestVerifyPlayerGuildID_StaleZeroHeals` | 先 `GetPlayerGuildID(P)` 缓存 0 → SQL 直接插 P 的成员行 → `VerifyPlayerGuildID(P,0)` 返回 G,随后 `GetPlayerGuildID(P)` 也返回 G;映射一致时调用不改变 generation 键 |
| `TestResolvePlayerGuild_KickedStaleSnapshot` | P 在 G,缓存帮会快照与映射 → SQL 删 P 的成员行(模拟失效失败)→ `ResolvePlayerGuild(P)` 返回 nil;另一例 SQL 把 P 移到 G2 → 返回 G2 快照且含 P |
| `TestConcurrentApproveSameApplicant` | G1、G2 同时审批 P(20 轮)→ 每轮 P 恰 1 行成员;另一方错误 ∈ {`ErrApplicationNotFound`, `ErrWriteConflict`};无残留申请 |
| `TestConcurrentApplyRespectsPlayerLimit` | P 已有 2 条;并发申请 G3、G4、G5(20 轮)→ 有效申请数恰为 3;失败者 ∈ {`ErrApplicationLimit`, `ErrWriteConflict`} |
| `TestConcurrentPromoteRespectsCap` | cap=1,同时任命 A、B → 恰 1 人成功 |
| `TestConcurrentCreateAndApply` | P 同时建帮 G2 与申请 G1(20 轮,每轮重建)→ Create 成功的轮次:`CountLiveApplications(G1)==0`,随后 `DisbandGuild(G2,P)` 后 G1 的 `ListApplicants` 仍不含 P、`guild_application` 中 P 的物理行数为 0(I3);Apply 失败错误 ∈ {`ErrPlayerAlreadyInGuild`, `ErrWriteConflict`} |
| `TestLeaveAndKickDeleteOwnApplications` | SQL 直接给成员 M 插一条 (G9,M) 申请(模拟竞态残留)→ `LeaveGuild(M)` 后该行不存在;同样构造后 `KickMember` 目标 → 行不存在;`DisbandGuild` 后全部成员的残留行不存在 |

<!-- s2_management_part8b.md -->

# S2 管理与审批 + 推送(B2)— 第 8b 部分:Unity EditMode 与 robot 冒烟

## 23. Unity EditMode(`Assets/Tests/EditMode/Guild/GuildUiTests.cs`)

现状(已核对):文件内 `GuildClientTests`(`:18`)14 个 `[Test]` + 1 个 4 例 `[TestCase]` 方法 = 18 条;`GuildWindowTests`(`:183`)9 个 `[Test]` + 1 个 3 例 `[TestCase]` 方法 = 12 条;合计 **30 条**。

### 23.1 测试替身与固定数据
- `GuildFakeTransport`(`:295`):**替换**现有空实现 `public void RegisterNotify(uint id, Action<MessageContent> action) { }`(`:305`)为 `=> Notifies[id] = action;`,并新增:

  ```csharp
  public readonly Dictionary<uint, Action<MessageContent>> Notifies = new();
  public void Push(uint id, IMessage message) => Notifies[id](new MessageContent { MessageId = id, SerializedMessage = message.ToByteString() });
  ```
  (`MessageContent` 字段名以生成的 C# 为准:`PetClient.cs:290` 读 `content.SerializedMessage`。)
- `GuildClientTests.Fixture()`(`:178`):`Contribution = 100 + i` → `ContributionTotal = 100 + i`;`GuildInfo` 初始化增 `OfficerCount = 0, MaxOfficers = 2`。
- `OverviewStatisticsUpdateOnlyFromTheGuildSnapshot`(`:215`):`updated.Members[0].Contribution = 987;` → `updated.Members[0].ContributionTotal = 987;`。该用例对 `GuildMyContribution` 的断言文本 `"101"` / `"987"` 不变(总览改读 `ContributionTotal`,第 7 部分 §18.6)。
- 落码后 `rg -n "\.Contribution\b|Contribution =" Assets/Tests Assets/Editor Assets/Scripts/UI Assets/Scripts/Game/Guild` 必须为 0。

### 23.2 修改的已有用例(条数不变)
- `UnconfirmedMembershipCannotSendJoin` → `UnconfirmedMembershipCannotSendApply`(`ApplyToJoin(555)`,断言无请求)。
- `DuplicateJoinIsBlockedAndDoesNotOptimisticallySetMembership` → `DuplicateApplyIsBlockedWhileBusyAndDoesNotSetMembership`:`Empty()` 后 `ApplyToJoin(555); ApplyToJoin(556)` 只发 1 个 `ApplyJoinGuild`;注意 `Empty()` 的 NotInGuild 回包会把 `MyApplicationsQueued` 置真,但 `DrainQueued` 未被调用,不产生请求;回 `ApplyJoinGuildResponse{}` 后 `Info` 仍为 null,`Calls.Last()` 为 `ListMyGuildApplications`。
- `MemberPaginationAndOnlineFilterConsumeTheRealSnapshot`:保持“道友 · N”断言。

### 23.3 新增到 `GuildClientTests`(28 条)
1. `RankHelperMatchesServer`(1)。
2. `KickPermissionFollowsRank(actor, target, expected)`:`(3,1,T)(3,0,T)(1,0,T)(1,1,F)(1,3,F)(0,0,F)(2,0,F)`(7)。
3. `WriteResponseAppliesSnapshotWithoutExtraRefresh`:帮主加载 → `SetMemberRole(2,1)` → 回带 role=1 的快照 → 2 号 role=1,`Calls.Last()` 仍为 `SetGuildMemberRole`(1)。
4. `ReviewReloadsApplicants`:回 `ReviewGuildApplicationResponse{Guild=...}` → 下一个调用 `ListGuildApplications`(1)。
5. `NotifyForAnotherGuildIsIgnored`:`{GuildId=999, Kind=MemberLeft}` → `RefreshQueued == false`(1)。
6. `KickedNotifyQueuesExactlyOneRefresh`:`{GuildId=555, Kind=MemberKicked, TargetPlayerId=1}` → `RefreshQueued`;`DrainQueued(false)` 两次只新增 1 个 `GetPlayerGuild`(1)。
7. `NotifyWhileBusyDefersUntilIdle`(1)。
8. `ApprovedNotifyWhileOutsideGuildRefreshes`:未入帮收到 `{MemberJoined, TargetPlayerId=1, GuildId=777}` → `RefreshQueued`(1)。
9. `ApplicationReceivedReloadsListOrBadge(bool listVisible)`:`[TestCase(true)] [TestCase(false)]`。帮主加载后收到 `ApplicationReceived` → `DrainQueued(listVisible)` → `Calls.Last()` 为 `listVisible ? ListGuildApplications : GetPlayerGuild`;`false` 分支回带 `PendingApplicationCount=4` 的快照后 `Info.PendingApplicationCount == 4`(2)。
10. `NewTipsHaveReadableText(uint tip)`:9 个新码各一个 `[TestCase]`,作为 `SetMemberRole` 的写响应 tip → `Status` 不含“暂未完成请求”,且 `RequiresReconnect == false`(9)。
11. `JoinedThenLeftReloadsMyApplications`:`Empty()` → `DrainQueued(false)` 发出 `ListMyGuildApplications`,回 1 条(队列已消费,`MyApplicationsQueued == false`)→ `Refresh` 回本人在册快照(断言 `MyApplications == null`)→ `Refresh` 回 NotInGuild(断言 `MyApplicationsQueued == true`)→ `DrainQueued(false)` 发 `ListMyGuildApplications`(1)。
12. `KickNoticeSurvivesRefresh`:加载后推 `MemberKicked`(target=本人)→ `DrainQueued(false)` → 回 NotInGuild → `Status` 含“请离”;再 `Refresh` 回 NotInGuild → `Status` 为默认未入帮文案(提示只用一次)(1)。
13. `SaveAnnouncementAppliesSnapshotWithoutRefresh`:长老 `SaveAnnouncement("新公告")` → 回 `SetAnnouncementResponse{Guild = 公告为“新公告”的快照}` → `Info.Announcement == "新公告"`,`Calls.Last()` 仍为 `SetGuildAnnouncement`(1)。

小计 1+7+1+1+1+1+1+1+2+9+1+1+1 = 28。

### 23.4 新增到 `GuildWindowTests`(7 条)
14. `LeaderSeesActionsButNotOnSelf`;15. `OfficerOnlySeesKickForMembers`;16. `PromoteDisabledAtOfficerCap`;17. `KickRequiresConfirmation`;18. `ApplicationsToggleOnlyForOfficersAndRequestsList`;19. `ApplicationRowsDriveReview`;20. `RankingShowsApplyAndCancel` —— 内容同原稿(按钮名 `GuildMemberPromote_2` / `GuildMemberTransfer_2` / `GuildMemberKick_2`、`ConfirmGuildAction`、`GuildApplicationsToggle`、`GuildApplicationApprove_20001`、`ApplyGuild_{id}`)。

**通过标准**:两个类合计 30 + 28 + 7 = **65 条**全部 PASS(改名 2 条不改变条数)。Codex 以 `editmode.xml` 的 `test-run total="65"`(按 `-testFilter "GuildClientTests;GuildWindowTests"`)核对;若其它会话同期在这两个类里加了用例,以 Codex 列出的差异名单为准。

## 24. robot 冒烟(`robot/guild_smoke_scenario.go`)

### 24.1 B2s 部分(编译级,随 proto 改动一起落地)

B2s 跑完 proto-gen 后 `game.GuildServiceJoinGuildMessageId`、`guildpb.JoinGuildRequest` 消失(`:263-264`、`:281-282`、`:572` 引用),robot 必须同批改好,否则并行会话的 robot 冒烟编译不过:
- `guildSmokeIsGuildMessage`:删 `JoinGuild`,加 8 个新 RPC 的 id;`game.GuildServiceNotifyGuildChangedMessageId` 在 `onMessage` 里直接忽略(不进 replies,B2c 再收集)。
- 第 4 步:C `ApplyJoinGuild{guild_id}` 断言 `tipGuildNotFound`。
- 第 6 步:B `ApplyJoinGuild` → 0;B `ListMyGuildApplications` 含 guildID;A `ListGuildApplications` 含 B;A `ReviewGuildApplication{B, true}` → 0 且响应 guild 含 B;B `myGuild()` 为成员(原断言保留)。
- `leaveAnyGuild` 前新增 `cancelAllApplications()`:`ListMyGuildApplications` 后逐条 `CancelGuildApplication`(可重复跑)。

### 24.2 B2c 部分(推送与管理段)
- 账号:`guildSmokeAccountD = "robot_9211"`(将任长老)、`E = "robot_9212"`(将被请离)、`F = "robot_9213"`(将被拒绝),须首次在 zone_a 建角;`specs` 追加到 `sc.ZoneA`。9214–9219 留给 B5/B6。
- 推送收集:`guildSmokeBot` 增 `pushes []*guildpb.GuildChangedS2C`(mu 保护),`onMessage` 解码追加;`waitPush(kind, guildID, timeout)` 轮询 50ms,命中即移除;`clearPushes()`;`guildSmokePushTimeout = 5 * time.Second`。
- 第 6 步追加:A 审批前 B `clearPushes`,审批后 B `waitPush(MEMBER_JOINED, guildID)`。
- 新 tip 变量:`tipGuildTargetNotMember`、`tipGuildCannotTargetSelf`、`tipGuildRankTooLow`、`tipGuildApplicationNotFound`。

管理段(原第 9 步之后、第 10 步解散之前):

| 步 | 动作 | 断言 |
|---|---|---|
| M1 | D、E、F 各 `ApplyJoinGuild`;F 再申请一次 | 均 0;F `ListMyGuildApplications` 仍 1 条 |
| M2 | E `ListGuildApplications` | `tipGuildNotInGuild` |
| M3 | A 审批 D、E 通过;D 等推送 | 两次 0;D `waitPush(MEMBER_JOINED)` |
| M4 | A 拒绝 F;F 等推送;F 列本人申请;A 再审 F | 0;F 收到 `APPLICATION_REJECTED`;F 列表为空;再审 `tipGuildApplicationNotFound` |
| M5 | A `SetGuildMemberRole{D,1}` 两次 | 均 0;响应 D role=1、`officer_count==1`、`max_officers==2`;D 收到 `ROLE_CHANGED` |
| M6 | D 踢 E;E 等推送;E 查帮;D 踢 A;D 踢 D;D `SetGuildMemberRole{A,1}`;D 再踢 E | 0;E 收到 `MEMBER_KICKED`;E `tipGuildNotInGuild`;`tipGuildRankTooLow`;`tipGuildCannotTargetSelf`;`tipGuildRankTooLow`;`tipGuildTargetNotMember` |
| M7 | A `TransferGuildLeader{D}` | 0;`leader_id==D`,A role=1;D 收到 `LEADER_TRANSFERRED` |
| M8 | D `TransferGuildLeader{A}` | 0;A 为帮主,D role=1 |
| M9 | D `LeaveGuild`;A 等推送 | 0;A 收到 `MEMBER_LEFT` |
| M10 | F `ApplyJoinGuild`;(第 10 步 A 解散后)F `ListMyGuildApplications` | 解散后为空 |

M1 中 F 的第二次申请是同帮刷新,不推送;M10 中 F 距 M1 已超过数秒但可能不足 60s,`APPLICATION_RECEIVED` 可能被冷却抑制 —— 冒烟**不**断言该推送。M 段通过后日志 `GUILD_MGMT_OK guild_id=… leader=… officer=… kicked=… rejected=…`;最终仍输出 `GUILD_SMOKE_OK`。任一步失败 `fail("mgmt-M<n>", ...)`。推送超时而 RPC 全部成功时,Codex 附 guild 日志 `[guild] push` 行与 `guild_push_total` 指标。

<!-- s2_management_part9.md -->

# S2 管理与审批 + 推送(B2)— 第 9 部分:文件清单与批次

## 25. 批次拆分

B2 手改文件共 39 个(robot 文件两批各改一次,分别计数),超过契约“每批 ≤30”。拆为两个子批,**每个子批开工前各自取得用户授权**(契约 §1):

- **B2s(服务端 + robot 编译级,30 个)**:proto、配表、Tip、C++ 表登记、go/guild、`robot/guild_smoke_scenario.go` 的编译级改写(第 8b 部分 §24.1)。B2s 结束时 robot 可编译、原冒烟可跑,并行会话(组队 / 聚宝斋)在同一工作树跑 robot 不受影响。
- **B2c(客户端 + 限流 + robot 管理段,9 个)**:mmorpg-client 7 个、`data/MessageLimiter.xlsx`(依赖 B2s 生成的号)、`robot/guild_smoke_scenario.go` 推送与 M1–M10。

B2s 与 B2c 之间:客户端在重跑 `gen_proto.ps1 / gen_messageids.ps1` 前仍能编译,但旧客户端的“加入帮会”按钮会失败(路由表已无 `JoinGuild`);新方法无 MessageLimiter 专属行。两者都只影响本地开发,B2c 修复,不要求两批连在一次授权内。

## 26. B2s 手改文件(30)

| # | 文件 | 类型 | 备注 |
|---|---|---|---|
| 1 | proto/guild/guild.proto | 改 | 第 2 部分(含 `SetAnnouncementResponse.guild`) |
| 2 | data/tip/Tip.xlsx | 改 | 插 9 行;**他人有未提交改动**,只插行 |
| 3 | data/GuildRule.xlsx | 新 | §3.1(默认行 `1,72,3,50,300,1000,3`) |
| 4 | data/schema/guildrule_table.proto | 新 | §3.1 |
| 5 | data/GuildLevel.xlsx | 新 | §3.2(10 级) |
| 6 | data/schema/guildlevel_table.proto | 新 | §3.2 |
| 7 | robot/guild_smoke_scenario.go | 改 | 第 8b 部分 §24.1(编译级) |
| 8 | cpp/generated/table/CMakeLists.txt | 改 | §3.4 |
| 9 | cpp/generated/table/table.vcxproj | 改 | §3.4 |
| 10 | cpp/generated/table/table.vcxproj.filters | 改 | §3.4 |
| 11 | go/guild/guild.go | 改 | 表校验、`WithNotifier`、`WithApplyPushGate`(B1 也改此文件,合并) |
| 12 | go/guild/internal/svc/servicecontext.go | 改 | writer 三项 + `WithLockWaitTimeout`;**他人有未提交改动** |
| 13 | go/guild/internal/constants/constants.go | 改 | Rank、9 个 tip、`MaxGuildMembersCap`、删 DefaultMaxMembers 与旧注释 |
| 14 | go/guild/internal/constants/constants_test.go | 改 | |
| 15 | go/guild/internal/session/session.go | 改 | |
| 16 | go/guild/internal/session/session_test.go | 改 | |
| 17 | go/guild/internal/server/guild_server.go | 改 | |
| 18 | go/guild/internal/server/inband_observability_test.go | 改 | |
| 19 | go/guild/internal/data/guild_repo.go | 改 | 哨兵、loadGuild(queryer)、删 AddMember* / RemoveMember / DeleteGuild、disband 事务(含 I3)、**CreateGuild 经 inTx + 删本人申请(I2)+ invalidateAfterCommit**、`UpdateAnnouncementAuthorized` 改薄包装、canSetAnnouncement |
| 20 | go/guild/internal/data/guild_manage_repo.go | 新 | §6–§8、`retryOnDeadlock`、`WithLockWaitTimeout`、`invalidateAfterCommit`、`VerifyPlayerGuildID`、`ResolvePlayerGuild`、`UpdateAnnouncement`、`TryMarkApplyPush` |
| 21 | go/guild/internal/data/guild_manage_repo_test.go | 新 | §21.3、§22 |
| 22 | go/guild/internal/data/guild_repo_zone_test.go | 改 | 测试助手加锁等待参数、改名用例 |
| 23 | go/guild/internal/data/rank_zone_integration_test.go | 改 | 1 处调用 |
| 24 | go/guild/internal/logic/guild_logic.go | 改 | 闸门 tip、Leave/Disband/Announcement/Create/GetPlayerGuild、删 JoinGuild、toProtoGuild |
| 25 | go/guild/internal/logic/guild_manage_logic.go | 新 | §10–§12 |
| 26 | go/guild/internal/logic/push.go | 新 | §13 |
| 27 | go/guild/internal/logic/online_status_resolver.go | 改 | 抽 `onlineSessionFrom` |
| 28 | go/guild/internal/logic/guild_manage_logic_test.go | 新 | §21.4 |
| 29 | go/guild/internal/logic/client_zone_test.go | 改 | JoinGuild 块与注释 |
| 30 | go/guild/internal/logic/merge_fence_test.go | 改 | |

`guild_repo_test.go` 不在清单:`UpdateAnnouncementAuthorized` 签名不变。若 Codex 发现其中有断言“提交后失效失败要返回错误”的用例,报告后在 B2c 或后续批次改,不在 B2s 里超额修改。`fakeHomeZones` 定义在 `client_zone_test.go:30`(已核对),保持不变;按玩家的并发安全替身 `perPlayerZones` 新建在 #28。

## 27. B2c 手改文件(9)

| # | 文件 | 类型 |
|---|---|---|
| 1 | mmorpg-client/tools/gen_messageids.ps1 | 改(**他人有未提交改动**,只改帮会段) |
| 2 | mmorpg-client/Assets/Scripts/Game/Guild/GuildClient.cs | 改 |
| 3 | mmorpg-client/Assets/Scripts/UI/Ugui/Guild/GuildWindow.cs | 改 |
| 4 | mmorpg-client/Assets/Scripts/UI/Ugui/Guild/GuildUiRoot.cs | 改 |
| 5 | mmorpg-client/Assets/Tests/EditMode/Guild/GuildUiTests.cs | 改 |
| 6 | mmorpg-client/Assets/Editor/Guild/GuildUiVerification.cs | 改 |
| 7 | mmorpg-client/Docs/GuildUI.md | 改(他人有未提交改动;补“申请制 / 成员操作 / 推送”三段) |
| 8 | xuanming-server-mmo/data/MessageLimiter.xlsx | 改(按 B2s 记录的号填) |
| 9 | xuanming-server-mmo/robot/guild_smoke_scenario.go | 改(第 8b 部分 §24.2) |

**生成物(不手改)**:`go/proto/guild/guild.pb.go`、`guild_grpc.pb.go`;`proto/message_id.txt`;各服务 `generated/pb/game/message_id.go`;`cpp/generated/rpc/service_metadata/rpc_event_registry.{h,cpp}`;`go/client_rpc_router/generated/pb/game/route_table.go`;`robot/generated/pb/game/*` 与 `robot/logic/handler/*`;`generated/code/proto/tip/guild_error_tip.proto` 及各语言 tip 产物;`generated/tables/{guildrule,guildlevel,messagelimiter,tip_text}.{json,pb}`;`go/shared/generated/{table,pb/table}`;`cpp/generated/table/{code,proto}`;`java/config_node/.../table`;客户端 `Assets/Scripts/Table/Generated`、`Assets/Scripts/Net/MessageIds.cs`、`Guild.cs`;`data/AGENTS.md` 索引段;`robot/vendor/**`。

**文档(随 guild-phase2 文档批处理,不计入)**:`docs/design/guild-phase2.md`(本节正文)、`docs/design/guild-zone-client-access.md` §5、`PROGRESS.md`。

<!-- s2_management_part9b.md -->

# S2 管理与审批 + 推送(B2)— 第 9b 部分:Codex 验证、契约偏差、风险

## 28. Codex 验证(串行;Claude 未执行任何一步)

**前置**:B1 已落地并验证通过;buildenv 提供 Go 1.26.5、protoc 35.1、protoc-gen-go 在 PATH;Docker 中 MySQL / Redis / Kafka / etcd 已起。执行 `git status --short` 并记录:Tip.xlsx、MessageLimiter.xlsx、servicecontext.go、table.vcxproj、gen_messageids.ps1、guild_smoke_scenario.go 若有他人未提交改动,只合并不覆盖;与聚宝斋、组队会话确认本时段无人跑导表 / proto-gen。

### B2s(用户授权后)

1. **导表 + proto-gen**(仓库根,cmd):`dev.bat gen`,随后 `py tools/data_table_exporter/tools/gen_schema_index.py`。期望:
   - `rg -n "kGuildZoneMerging|kGuildApplicationQueueFull|kGuildBusyRetry" generated/code/proto/tip/guild_error_tip.proto` 各 1 行;
   - `generated/tables/guildrule.json` 1 行(7 列,值 `72,3,50,300,1000,3`)、`guildlevel.json` **10 行**;
   - `rg -n "GuildServiceApplyJoinGuild|GuildServiceNotifyGuildChanged|GuildServiceReviewGuildApplication" proto/message_id.txt` 命中;
   - `rg -n "GuildServiceJoinGuild" go/guild/generated go/client_rpc_router/generated cpp/generated/rpc robot/generated` 为 0;
   - 保存 `git diff proto/message_id.txt` 全文,**写明 19 号现归属哪个方法(或空)以及 9 个新 id**,交给 B2c。
2. **C++**(VS MSBuild,Debug x64,**/m:1**):`msbuild game.sln /p:Configuration=Debug /p:Platform=x64 /m:1`(或按 guild-zone-client-access.md §7 顺序 proto → table → core → rpc → gate-lib → gate,再其余节点)。期望无 LNK2019 / C1083。
3. **go/guild**(`cd go/guild`):`go build ./...`;`go vet ./...`;`go test ./... -count=1 -race`(MySQL 用例 Skip)。
4. **go/guild 真库**:`$env:GUILD_TEST_MYSQL_DSN='appuser:apppass123@tcp(127.0.0.1:3306)/guild_test?parseTime=true&charset=utf8mb4'`(测试助手自动追加 `innodb_lock_wait_timeout=1`);`go test ./internal/data/ -count=1 -v -run 'Test(SetMemberRole|KickMember|TransferLeader|LeaveGuild|LeaveAndKick|Apply|CancelApplication|ListApplicants|Review|CreateGuild|DisbandGuild|UpdateAnnouncement|LockWaitTimeout|VerifyPlayerGuildID|ResolvePlayerGuild|Concurrent)'`,再不带 `-run` 全跑一次;`$env:GUILD_IT_MYSQL_DSN='root:<本地 root 口令>@tcp(127.0.0.1:3306)/mysql?charset=utf8mb4&parseTime=true'; go test -tags integration ./internal/data/... -count=1 -v`。通过标准:无 Skip、全 PASS;并发用例 20 轮全过;**记录 `TestLockWaitTimeoutBounded` 的实际返回耗时**(期望约 1s;若等到 ctx 超时才返回,说明 DSN 参数未生效,判失败)。
5. **其它 Go 模块回归**:`cd go/client_rpc_router; go build ./...; go test ./... -count=1`;`cd go/shared; go test ./generated/table/... -count=1`。
6. **静态检查**:
   - `rg -n "JoinGuild" go/guild robot --glob '!**/generated/**' --glob '!**/vendor/**'` 为 0;
   - `rg -n "RemoveMember|AddMemberInZone|DefaultMaxMembers|FailedPrecondition|codes.Aborted" go/guild/internal` 为 0;
   - `rg -n "GuildService_NotifyGuildChanged_FullMethodName" go/guild/internal/session/session.go` 为 0;
   - `rg -n "Labels" go/guild/internal/logic/push.go go/guild/internal/data/guild_manage_repo.go | rg "player|guild_id"` 为 0;
   - `rg -n "innodb_lock_wait_timeout" go/guild/internal/data/guild_manage_repo.go` ≥1 且 `rg -n "WithLockWaitTimeout" go/guild/internal/svc/servicecontext.go` ≥1。
7. **robot 编译与原冒烟**(`cd robot`):`go mod vendor`(`git status --short vendor` 只应出现 shared/generated 与 proto/guild 相关变化);`go build ./...`;`go vet ./...`;按顺序重启 client_rpc_router → gate → guild(日志无 `guild config tables invalid`);`go run . -c etc/guild_smoke.yaml` → `GUILD_SMOKE_OK`,退出码 0。

### B2s 生成结果记录(2026-09-18,供 B2c 直接使用)

`git show 969bbec4c -- proto/message_id.txt`(回合制战斗 G1-G9 会话在主工作树跑的那轮完整导表 + proto-gen,
过程见 `docs/design/turn-battle-gap-closure.md` §8;`go/build.bat` 的 goctl 未跑,但帮会不依赖那层 —— 
`go/proto/guild/guild_grpc.pb.go` 已含全部新 RPC):

```
-19=GuildServiceJoinGuild
+19=GuildServiceSetGuildMemberRole
+216=GuildServiceTransferGuildLeader
+217=GuildServiceKickGuildMember
+218=GuildServiceApplyJoinGuild
+219=GuildServiceCancelGuildApplication
+220=GuildServiceNotifyGuildChanged
+221=GuildServiceListGuildApplications
+222=GuildServiceListMyGuildApplications
+223=GuildServiceReviewGuildApplication
```

**19 号被复用给了 `SetGuildMemberRole`(写操作)**,而 `data/MessageLimiter.xlsx` 里 id=19 那行还是当初为
查询类 `JoinGuild` 定的 `5, 1, 1000`。按 §28 B2c 第 1 步的规则:19 号被本批方法复用 ⇒ **按写操作类别重定**
(写 = `5, 1, 1000`,与读的 `10, 1, 1000` 区分)。216–223 八个号在 `generated/tables/messagelimiter.json`
里一行都没有,按读写类别新增:读 `ListGuildApplications(221)` / `ListMyGuildApplications(222)` = `10, 1, 1000`;
写 `TransferGuildLeader(216)` / `KickGuildMember(217)` / `ApplyJoinGuild(218)` / `CancelGuildApplication(219)` /
`ReviewGuildApplication(223)` = `5, 1, 1000`;`NotifyGuildChanged(220)` 是下行推送,**不加行**。

tip 发号结果(同一轮):`kGuildZoneMerging=14013`、`kGuildApplicationNotFound=14018`、
`kGuildApplicationLimit=14019`、`kGuildApplicationQueueFull=14020`、`kGuildBusyRetry=14021`。
配表:`generated/tables/guildrule.json` 单行 7 列、`asset_op_deadline_seconds=600`(按 90 清单 D1,
**不是** §29 第 2 条写的 300);`guildlevel.json` 10 行。

### B2c(另行授权,B2s 验证通过后)

1. 实现方按 B2s 步骤 1 的记录改 `data/MessageLimiter.xlsx`(列 `id, max_requests, time_window, tip_message`):读 `ListMyGuildApplications`、`ListGuildApplications` 为 `10, 1, 1000`;写 `SetGuildMemberRole`、`KickGuildMember`、`TransferGuildLeader`、`ApplyJoinGuild`、`CancelGuildApplication`、`ReviewGuildApplication` 为 `5, 1, 1000`;原第 19 行:19 号空 → 删行,被本批方法复用 → 按其读写类别改值,**被其它会话的方法复用 → 不动并在 PROGRESS 记录**;`NotifyGuildChanged` 不加行。
2. **重导表**:`py tools/data_table_exporter/run.py tools/data_table_exporter/exporter_config.yaml`;`generated/tables/messagelimiter.json` 含上述 8 个 id。
3. **客户端**(`cd E:\work\mmorpg-client`):`pwsh -File tools/gen_proto.ps1`;`pwsh -File tools/gen_messageids.ps1`;`MessageIds.cs` 含 `NotifyGuildChanged`、`ApplyJoinGuild`,不含 `JoinGuild`;`rg -n "\.Contribution\b|Contribution =" Assets/Tests Assets/Editor Assets/Scripts/UI Assets/Scripts/Game/Guild` 为 0。Unity 6000.6.0f1 编译检查;EditMode:`Unity.exe -batchmode -projectPath E:\work\mmorpg-client -runTests -testPlatform EditMode -testFilter "GuildClientTests;GuildWindowTests" -testResults run/logs/guild-b2/editmode.xml -logFile run/logs/guild-b2/editmode.log`(不带 `-quit`)。通过标准:**total=65 且全 PASS**(第 8b 部分 §23)。可选 `GuildUiVerification.CaptureAll` 出图核对 `11-applications` 与成员页按钮无重叠。
4. **robot 管理段**:`go build ./... && go vet ./...`;重启 guild(读新 MessageLimiter 的 gate 同时重启);`go run . -c etc/guild_smoke.yaml` → 依次出现 `GUILD_MGMT_OK`、`GUILD_SMOKE_OK`,退出码 0;`curl -s http://127.0.0.1:9220/metrics | rg "guild_push_total"` 至少有 `outcome="ok"` 的 `member_joined`、`member_kicked`、`application_rejected`。

**保留证据**:每步命令输出摘要;B2s-1 的 message_id diff 与 19 号归属;B2s-4 并发用例日志与锁等待耗时;B2c-3 的 editmode.xml;B2c-4 的 robot 日志与 metrics 片段;任一失败的完整错误。

## 29. 契约偏差

1. **批次拆分**:B2 拆为 B2s(30)与 B2c(9),各自授权(§25)。
2. **配表归属与默认值仲裁**:B2 一次建齐两表契约 7 列;B5/B6 **只能从字段号 8 起追加列**(B6a 的 `activity_join_min_hours` = 8),不得改 1–7 号。默认值按“读取该列的批次”拍板:GuildRule `1,72,3,50,300,1000,3`;GuildLevel 采用 B5 §5.9 的 10 级。**需同步修订的其它分节**:s5 §5.9 GuildRule 行的 `reunion_min_online_members` 改为 3;s5 §5.10 删去第 2 条 GuildLevel 规则、改为调用 `validateGuildTables`,`max_members` 上限统一为 100;s6 §6.2.3 “若 B2 值不同以 B2 为准”保持。
3. **闸门范围**:`LeaveGuild`、`DisbandGuild`、`SetAnnouncement`、`CancelGuildApplication` 也查闸门(“所有写 RPC”取字面)。
4. **`SetAnnouncementResponse` 增 `GuildInfo guild = 2`**:契约 §3.1 未列,按契约 §2“所有写 RPC 回带 GuildInfo”补齐。
5. **新 RPC 只接受会话调用**:无会话即 `PermissionDenied`;GM / 运维代操作另设内部 RPC。
6. **`GuildMember.contribution=5` 用 `reserved 5`**(语义等价、更安全)。
7. **非法 role 映射 `kGuildNoPermission`**(契约无“角色非法”码)。
8. **审批时申请人已入他帮或归属 zone 与帮会不一致**,均映射 `kGuildApplicationNotFound` 并删除该申请。
9. **解散授权改读 MySQL**(修复缓存 `LeaderID` 授权)。
10. **推送收件人**:不含操作者;`APPLICATION_RECEIVED` 只推长老与帮主,且同一 (帮, 申请人) 60s 内至多一次(Redis NX 冷却);解散不通知仅提交过申请的玩家。
11. **长老上限按 GuildLevel 等级读**,配表下调后不强制降级。
12. **`pending_application_count` 按请求者现算**,不进缓存。
13. **新增 tip `GuildBusyRetry`**(契约 §4 未列):死锁重试 3 次耗尽或锁等待超时时回该业务码,取代原稿的 gRPC `Aborted`,客户端不隔离。
14. **写库连接强制 `innodb_lock_wait_timeout=1`**(代码里改 DSN,B1 的 yaml 与 config.Validate 不变);B5/B6 复用同一 `svcCtx` 连接池,自动继承。
15. **申请清理不变式 I2 / I3**:契约只写“通过/拒绝/撤销/过期即删行”;本节另加“建帮删本人申请”“退帮 / 被踢 / 解散删被移除成员的申请”。
16. **`ListMyGuildApplications` 按归属 zone 过滤**;`ListGuildApplications` 不按申请人 zone 过滤(见第 4b 部分末段)。
17. **客户端 `GetPlayerGuild` 在“未入帮 / 快照无本人”时以 MySQL 复核**(`ResolvePlayerGuild`);内部调用不变。

## 30. 风险与未验证项

- **message id 竞争**:组队 / 聚宝斋会话可能在 B2s 期间跑 proto-gen;若 19 号被复用,B2s 与 B2c 之间旧客户端的 JoinGuild 包(`guild_id`)会被按新方法解码。旧客户端未入帮时点“加入”才会发出,多数落到 `kGuildNotInGuild`,仍建议该窗口内不用旧客户端操作帮会。
- **锁等待与断连**:“ctx 取消后服务端等锁线程不感知断连”是推断,由 `TestLockWaitTimeoutBounded` 实测;若实测 1s 封顶不生效(如账号无权设会话变量),Codex 报告,不得自行调大。
- **C++ 表登记**:CMakeLists 已缺 ActivitySchedule(critic.md),本批只保证 Windows MSBuild。
- **被踢 / 退帮帮贡清零**:成员行删除即丢失 `contribution_total/balance`;B5 若要求保留,须另立表。
- **推送断言依赖本地 Kafka / gate 命令 topic 代次**(`gate-cmd_g2`,PROGRESS.md:4261);代次不一致时推送步骤超时,需核对 `KAFKA_COMMAND_TOPIC_GENERATION`。
- **跨分节一致性**:偏差 2 列出的 s5 / s6 修订若未随文档合并落实,B5/B6 落码时会出现第二份校验或不同默认值;合并 `docs/design/guild-phase2.md` 时须逐条核对。
- **建帮上限变化**:第 1 级由 50 变为 30;本地若有 B1 之后手工造的超过 30 人的测试帮会,不受影响(`guild.max_members` 存在行里,只有 B5 升级会改写)。
- **行号漂移**:并行会话持续改动,行号仅作定位,落码以函数名为准。

---

## 附录:对抗评审处理记录

# S2 管理与审批 + 推送(B2)— 评审处理记录

评审结论:无 blocker,6 major + 8 minor。逐条对照代码核实后**全部采纳**,其中 4 条的修法与评审建议不同(见各条“差异”)。修订后的分节:part1、part2、part3、**part3b(新)**、part4、**part4b(新)**、part5、**part5b(新)**、part6、part7、part8、**part8b(新)**、part9、**part9b(新)**。

## Major

**M1 审批不复核 zone —— 已采纳(做法有调整)**
- 核实:`tools/merge_zone/unmerge.go:133-149` 只把清单里帮会的 `guild.zone_id` 改回源区,不动 `guild_application`;文件头注释说明该工具用于“合服刚跑完、还没开服”,但代码不强制。旧 `AddMemberInZone` 在插入成员时复核 zone(`guild_repo.go:330` 附近)。结论成立,只是触发窗口窄。
- 修订:审批通过时 logic 查申请人归属 zone,repo 在 guild 行锁下比较,不一致就删申请并回 NotFound(part4 §8.5 第 5a 步,part5b §12.2.1)。映射缺失时不访问 repo,直接回 NotFound;查询失败直接返回错误(fail-closed)。`ListMyGuildApplications` 改为调用 `clientZone` 并按 `visibleIn` 过滤。新增用例:`TestReview_ZoneMismatchDeletesApplication`、`TestReviewApproveChecksApplicantZoneBeforeRepo`。
- 差异:① 评审建议串行查询、子预算 1000ms。实际改为**与审批人的归属区查询并行**,同步预算仍是一次 ≤1500ms;串行时 1500 + 1000 + 锁等待,会超出 4000 − 500 的预算。② 只在“通过”时查,“拒绝”不查。③ `ListGuildApplications` 不逐人查 zone:50 人就是 50 次 data_service 调用。跨区残留行由审批时的删除兜底(part4b 末段)。

**M2 缓存失效失败没有自愈路径 —— 已采纳**
- 核实:`GetPlayerGuildID` 对 0 也缓存(注释“0 也缓存”),`etc/guild.yaml:51` `DefaultTTL: 30m`;`guild_logic.go:282-284` 命中 0 直接回 NotInGuild;`GuildClient.cs:158` 快照里没有本人时只提示“尚未确认”,不刷新。
- 修订(part3b §6.4):(a) `invalidateAfterCommit` 同步失败后,在 safego 协程里按 100/400/1600ms 有界重试,重试耗尽才计指标;(b) 新增 `VerifyPlayerGuildID`,供 `operatorGuild` 读到 0、Apply/Create 预检读到 >0、事务返回 AlreadyInGuild / NotGuildMember / GuildGone 这几处调用;(c) 新增 `ResolvePlayerGuild`,客户端 `GetPlayerGuild` 在“缓存为 0”或“快照里没有本人”时用 MySQL 复核,必要时绕过缓存直读。
- 差异:评审建议在 logic 层补 `TestStaleZeroMappingSelfHealsOnWrite` 等用例,但 logic 单测的 repo 为 nil,改为在 data 层补真库用例 `TestVerifyPlayerGuildID_StaleZeroHeals`、`TestResolvePlayerGuild_KickedStaleSnapshot`,另加 miniredis 用例 `TestInvalidateAfterCommitRetriesInBackground`(断言通过可替换的钩子 `invalidateGaveUp` 进行,不去读 go-zero 指标)。

**M3 建帮不清本人申请 —— 已采纳并扩展**
- 核实:`guild_repo.go` 的 `CreateGuild` 事务只 INSERT guild 与 guild_member 两行。
- 修订:新增不变式 I2,建帮事务里执行 `DELETE ... WHERE player_id=?`,并改走 `inTx("create")`(part4 §8.8)。补用例 `TestCreateGuild_DeletesOwnApplications`。
- 扩展:核实过程中发现同类漏洞。Apply 第 2 步是非锁定读,与他帮审批或建帮并发时会留下物理申请行,玩家离帮后这些行同样会“复活”。为此再加不变式 I3:退帮、踢人、解散时,在同一事务里删掉被移除成员的全部申请(part3b §7.2/§7.4,part4 §8.6)。补用例 `TestLeaveAndKickDeleteOwnApplications`、`TestConcurrentCreateAndApply`。

**M4 GuildLevel/GuildRule 默认值与校验跟 s5/s6 冲突 —— 已采纳**
- 核实:s5_economy_part3 §5.9 为 10 级、第 1 级 30 人,deadline 为 300,reunion 为 5;§5.10 另写了一套 `ValidateEconomyTables`,max_members 上限 1000。s6_activities_part1 §6.2.3 追加列,reunion 为 3。原 part6 按“每帮 ≤100 人”估预算,但原校验放行到 500。
- 修订:GuildLevel 采用 B5 的 10 级;GuildRule 取 `1,72,3,50,300,1000,3`,每列由读取它的批次拍板。结构校验只保留 `validateGuildTables` 一份,新增常量 `MaxGuildMembersCap=100`。1–7 号字段冻结,B5/B6 从字段号 8 起追加。偏差 2 已重写(part1 §3,part9b §29-2)。
- 限制:本次只允许改 s2 文件,s5 §5.9/§5.10 需要的修订(reunion 改 3、删除重复的 GuildLevel 规则、上限统一为 100)已写进 part9b 偏差 2 与风险,合并总文档时落实。

**M5 锁等待不受 4s 预算约束 —— 已采纳(取值和接入点有调整)**
- 核实:`svc/servicecontext.go:69` 直接用 `sql.Open("mysql", c.MySQL.DataSource)`;s1 的 DSN 不带该参数。“断连后等锁线程不退出”仍是推断。
- 修订:新增纯函数 `WithLockWaitTimeout(dsn)`,在 servicecontext 打开连接和测试助手里强制加上 `innodb_lock_wait_timeout`;1205 映射为 `ErrWriteConflict`,不重试(part3 §6.2a)。新增 `TestWithLockWaitTimeout`、真库用例 `TestLockWaitTimeoutBounded`,要求 Codex 记录实测耗时。
- 差异:① 取 **1s** 而不是 2s,因为预算是 1500 + 1000 + 语句 + 装配 ≈ 3100 < 3500;取 2s 时超出。② 在代码里改 DSN,不改 yaml 或 config.Validate:K8s ConfigMap 不会漂移,也不增加 B2s 的文件数。

**M6 批次安排让 robot 编译中断 —— 已采纳**
- 核实:`robot/guild_smoke_scenario.go:263-264, 281-282, 572` 引用了 `JoinGuild` 的 id 和消息类型。
- 修订:robot 的编译级改写移入 B2s,`data/MessageLimiter.xlsx` 移到 B2c,B2s 仍是 30 个文件、B2c 为 9 个;两批**各自授权**;B2s 第 1 步记录 19 号当前归属,B2c 第 1 步据此填写限流表(part9 §25–27,part9b §28,part8b §24.1)。

## Minor

**m1 SetAnnouncement 不回带 GuildInfo —— 已采纳**:`SetAnnouncementResponse` 增加 `guild = 2`;新增 `UpdateAnnouncement`,在事务内取快照并调用 `invalidateAfterCommit`;原 `UpdateAnnouncementAuthorized` 改为薄包装,`guild_repo_test.go` 不用改;客户端 `SaveAnnouncement` 直接应用快照;删除原偏差 4。顺带把 `CreateGuild` 的提交后失效也改为不报错(part2 §5.3,part4 §8.7/§8.8,part7 §17.4)。核实:`guild_repo.go` 公告提交后执行 `return r.invalidateGuildCache(...)`;`GuildClient.cs:204` 遇到传输错误会置 `RequiresReconnect`。

**m2 满员时重复申请回 GuildFull —— 已采纳**:把刷新判定移到满员与上限判定之前(part4 §8.2 第 5–7 步),补用例 `TestApply_RefreshWhileGuildFull`。

**m3 “申请→撤回→申请”推送刷屏 —— 已采纳**:用 `TryMarkApplyPush` 执行 `SET guild:apply_push:{guild}:{player} NX PX 60000`,拿到键才推送,Redis 出错时不推;通过 `WithApplyPushGate` 注入(part5 §10.1,part5b §12.2,part6 §14)。补用例 `TestApplyPushGateCooldown`;robot M10 不断言该推送。

**m4 inTx 重跑时结果累积,且返回 Aborted —— 已采纳(映射方式有调整)**:规定结果变量在 fn 内部声明,成功后才赋给外层;`maxTxAttempts=3`,每次重试前随机退避 10–50ms;抽出 `retryOnDeadlock`,用假错误驱动单测(`TestRetryOnDeadlock`、`TestRetryResultsDoNotAccumulate`)。差异:评审给了“新增业务码”和“保留 Aborted”两个选项,本次选前者,新增 tip `GuildBusyRetry`(契约外,记为偏差 13),客户端不进入隔离。

**m5 入帮后 MyApplications 不失效 —— 已采纳**:私有 `Apply(GuildInfo)` 执行时置 `MyApplications = null`;`Refresh` 回包为 NotInGuild 且列表为 null 时,置 `MyApplicationsQueued`(part7 §17.3/§17.4)。补用例 `JoinedThenLeftReloadsMyApplications`。

**m6 待审数角标不随推送更新 —— 已采纳**:收到 `ApplicantsQueued` 时,申请列表可见就重拉列表,不可见就执行 `Refresh`,由服务端重算待审数(part7 §17.3)。原用例 9 改为 `ApplicationReceivedReloadsListOrBadge`,用 `[TestCase(true)]` 和 `[TestCase(false)]` 两例覆盖。

**m7 被踢/解散提示被 Refresh 覆盖 —— 已采纳**:新增 `_pendingNotice`,推送到达时立即写入 Status;`Refresh` 的 NotInGuild 分支再落定一次,然后清空(part7 §17.2/§17.3)。核实:`GuildClient.cs:194` 发请求时置 Status 为“正在读取帮会,请稍候…”,`:87` 的 NotInGuild 分支覆盖为默认文案。补用例 `KickNoticeSurvivesRefresh`。

**m8 EditMode 改动清单遗漏,用例数不准 —— 已采纳**:核实结果:`GuildUiTests.cs:215` 有 `Contribution = 987` 的赋值;`:305` 已有空的 `RegisterNotify`;现有用例共 30 条(`GuildClientTests` 14 个 Test + 4 个 TestCase,`GuildWindowTests` 9 个 Test + 3 个 TestCase)。修订后清单改为“替换 RegisterNotify”,加入 `:215` 的改写(断言文本 `"101"`/`"987"` 不变),并加 rg 零命中检查;新增 28 + 7 = 35 条,通过标准写死为 **total=65**(part8b §23,part9b §28 B2c 第 3 步)。另外,评审算出的 +30 是按原清单计的;本轮新增的 m5/m7/m1 用例和第 10 项的第 9 个 TestCase 也已计入。

## 顺带修正(评审未提)
- 并发审批时 `fakeHomeZones` 在后台协程中被访问,它不是并发安全的。新测试改用 `perPlayerZones`(带 mutex),原 `fakeHomeZones`(`client_zone_test.go:30`,已核实)保持不动。
- 修订后部分文件的交叉引用编号已同步更新,例如 part4 §8.8/§8.9 交换编号,以及“第 5b / 9b 部分”的引用。
