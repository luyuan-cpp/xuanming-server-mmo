# S3 成员名字 / 最小昵称(B3a/B3b)
> 本节由 9 个分部合并而成(原分部名保留在小标题里),另附对抗评审处理记录。

<!-- s3_names_part1.md -->

# §3 成员名字 / 最小昵称(批次 B3a、B3b)— 第 1 部分:总览与 data_service 名字注册表

> 状态:设计稿(2026-09-16 按对抗评审修订,逐条处理见 `s3_names_review.md`),未落码、未编译。Claude 不跑构建;验证由 Codex 按第 6、8 部分命令执行。
> 契约名以 `00_contract.md` §3.4 / §4 为准;偏离处集中列在第 9 部分 §3.25。行号是 2026-09-15/16 工作区快照,落码前再核(多处文件有他人未提交改动)。
> 分部:1 总览、proto、表、建表(3.0–3.3);2 Store / 缓存 / Logic / 配置 / 崩溃窗口(3.4–3.7);3 规则包、RoleNameRule 配表、login proto、登记客户端、Tip(3.8–3.10、3.13);4 login 建角、入场补齐、角色列表(3.11–3.12);5 Java、scene、zone 库加列、合服 / 回档、机器人(3.14–3.17);6 B3a 清单 / 测试 / 验证(3.18–3.20);7 guild 与客户端规则 / 管线(3.21–3.22);8 客户端界面 / 测试、B3b 清单 / 验证(3.22 续–3.24);9 契约偏差、未决(3.25–3.26)。

## 3.0 目标与边界

- 名字**全服唯一**(用户决策 §0.5),真源 = data_service 全局库(SnapshotMySQL)表 `player_name`;Redis `player:name:{id}` 只是读缓存。
- 副本两份,都只读:`AccountSimplePlayer.name`(角色列表)与 `PlayerProfileComp.name`(随 `player_database`,供 scene 战斗快照)。v1 不做改名,副本**内容**不会变;但副本可能**缺失**(entergame self-heal 恢复出的空记录、早于首次入场的回档快照),所以读侧一律"缺了就回源 `BatchGetPlayerName`"(第 4 部分 §3.12)。
- **建角必填**:客户端建角界面不允许空名(第 7、8 部分)。服务端对空名生成 `前缀 + 随机后缀`,只服务机器人与客户端无 UI 路径。
- 名字是展示数据:建角写入 fail-closed;guild、角色列表、入场补齐等读取方 fail-open。
- 不在本节:远端头顶名(`ActorCreateS2C` 无名字字段)、组队 / 聊天 / 切磋的名字槽、改名、按名字查人(列入未决)。

现状(已读码):`CreatePlayerRequest` 只有 class_id/gender(proto/login/login.proto:52-56);`AccountSimplePlayer` 字段 1-4(user_accounts.proto:6-12);`player_database` 已用 1-14(mysql_database_table.proto:107-130);scene 快照 `set_player_name("")`(player_battle.cpp:442);机器人发空 `CreatePlayerRequest{}`(robot/login.go:60/353/416、robot/main.go:698)。

## 3.1 proto:`proto/data_service/data_service.proto`

在 `AllocateIdSegment` 之后追加:

```proto
  // ── Player name registry(全服唯一;docs/design/guild-phase2.md §3)──
  rpc ReservePlayerName(ReservePlayerNameRequest) returns (ReservePlayerNameResponse) {}
  rpc ReleasePlayerName(ReleasePlayerNameRequest) returns (google.protobuf.Empty) {}
  rpc BatchGetPlayerName(BatchGetPlayerNameRequest) returns (BatchGetPlayerNameResponse) {}

message ReservePlayerNameRequest  { uint64 player_id = 1; string name = 2; }
// result:0 成功(含同 player_id 同名幂等重试)、1 已被别人占用、2 不合规(含敏感词)。
// owner_player_id:仅 result=1 时填占用者 id;只给 login 判定"丢响应后的重试",login 不下发客户端。
message ReservePlayerNameResponse { uint32 result = 1; uint64 owner_player_id = 2; }
// 条件删除:player_id 与 name 归一化后的 name_norm 都匹配才删。
// 不带 x-admin-token:只删 created_ms 在 PlayerName.ReleaseWindow 内的行(login 建角补偿);
// 带合法 x-admin-token:不限登记时间(运维按日志清孤儿)。
message ReleasePlayerNameRequest  { uint64 player_id = 1; string name = 2; }
message BatchGetPlayerNameRequest { repeated uint64 player_ids = 1; }   // ≤ 500 个
// 缺席的 id(未登记 / 已释放)不出现在 map 里,不是错误(同 BatchGetPlayerHomeZone 口径)。
message BatchGetPlayerNameResponse { map<uint64, string> names = 1; }
```

`result` 常量放 `go/shared/playername`(第 3 部分),data_service / login 共用。

data_service 错误码轴 `internal/constants/error_codes.go` 追加(非 tip 码;当前末码 `ErrCodeMergeFenceMissing = 24`,聚宝斋会话可能也在追加,落码取"当时末码 +1、+2",下文写 25/26):
- `ErrCodePlayerNameDBError uint32 = 25`:player_name 读写失败或 store 未就绪。
- `ErrCodePlayerNameConflict uint32 = 26`:该 player_id 已登记**另一个**名字(v1 无改名,出现即 ID 复用事故)。

gRPC status 映射(照 `RegisterPlayerZone` 的 `error_code=<n>: ...` 写法):

| 情形 | 返回 |
|---|---|
| Reserve/Release 的 player_id=0 | `InvalidArgument`,`error_code=14` |
| Reserve 名字不合规 / 敏感 | OK,`result=2` |
| Reserve 被他人占用 | OK,`result=1`,`owner_player_id`=占用者 |
| Reserve 同 id 同名已存在 | OK,`result=0` |
| Reserve 同 id 已有别名 | `FailedPrecondition`,`error_code=26`,ERROR 日志 |
| store 为 nil / SQL 错 | `Unavailable`,`error_code=25` |
| Release 的 name 归一化后为空或非法 | `InvalidArgument`,`error_code=14` |
| Release metadata 里带了 x-admin-token 但校验失败 | 原样返回 `s.authorizeAdmin(ctx, "ReleasePlayerName")` 的错误(dataserviceserver.go:163 起) |
| Release 行不存在 | OK(幂等) |
| Release 行存在但早于窗口且无 token | `FailedPrecondition`,`error_code=23`(复用 `ErrCodeAdminAuthRequired`),WARN 日志 |
| BatchGet 超 500 个 | `InvalidArgument`,`error_code=14` |
| BatchGet 有未命中且 SQL 失败 | `Unavailable`,`error_code=25`(不回部分结果) |

## 3.2 表:`proto/common/database/rollback_database_table.proto`

```proto
// 玩家名字注册表(全服唯一)。只在全局库一处;zone 库 player_database.profile_component 是副本。
// 预建表:proto2mysql v0.1.0 把 string 渲染成 MEDIUMTEXT,TEXT 列上的 UNIQUE KEY 会撞 MySQL 1170,
// 所以同 id_segment 走 store/schema.go 的 bootstrap DDL(VARCHAR),本 message 只做列漂移校验。
message player_name
{
  option(OptionTableName) = "player_name";
  option(OptionPrimaryKey) = "player_id";
  option(OptionUniqueKey) = "name_norm";
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 player_id  = 1;
  string name       = 2;   // NFKC + 去首尾空白后的展示名(保留大小写)
  string name_norm  = 3;   // name 再转小写;唯一性只看它
  uint64 created_ms = 4;   // 登记时刻 Unix 毫秒(data_service 进程时钟;Release 窗口与孤儿排查用)
}
```

已核:proto2mysql v0.1.0 `protoreflect.StringKind: "MEDIUMTEXT"`(proto2mysql.go:259),唯一键渲染为 `UNIQUE KEY uk_<table>`(:416)。

## 3.3 建表:`go/data_service/internal/store/schema.go`

1. `TableMessages()` 追加 `&dbpb.PlayerName{}`(第 5 张)。
2. 新常量 `PlayerNameTableName = "player_name"` 与:

```go
const playerNameBootstrapDDL = "CREATE TABLE IF NOT EXISTS `player_name` (\n" +
	"  `player_id` bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:1',\n" +
	"  `name` VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'pb:2',\n" +
	"  `name_norm` VARCHAR(191) NOT NULL COMMENT 'pb:3',\n" +
	"  `created_ms` bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:4',\n" +
	"  PRIMARY KEY (`player_id`) /*T![clustered_index] NONCLUSTERED */,\n" +
	"  UNIQUE KEY `uk_player_name` (`name_norm`)\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin" +
	" /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */ COMMENT='player_name';"
```

   `utf8mb4_bin` 是刻意的:归一化在 Go 完成,库只逐字节比较;`unicode_ci` 会把部分不同字符判等,造成"库说撞名、Go 说不撞"。
3. "唯一例外"改清单:`var bootstrapTables = map[string]string{IdSegmentTableName: idSegmentBootstrapDDL, PlayerNameTableName: playerNameBootstrapDDL}`。`migrateSchemaOn` 先逐条 `ExecContext`,同步循环里清单内的表只跑 `assertColumnsPresent`(错误文案 "pre-created by bootstrap DDL (schema.go bootstrapTables)")。注释"唯一一段手写 DDL"改"两段"。
4. 注释"四张表"改"五张表":`data_service.go:41`、`internal/config/config.go:43,116`、`store/storetest/mysql.go:143`。

<!-- s3_names_part2.md -->

# §3 成员名字 — 第 2 部分:data_service Store、缓存、Logic、配置、崩溃窗口(B3a-1)

## 3.4 Store:新文件 `go/data_service/internal/store/player_name_store.go`

```go
type PlayerNameStore struct{ db *sql.DB }
// cfg 为 SnapshotMySQL 连接参数,但 MaxOpenConn/MaxIdleConn 由 logic 装配时用 PlayerName 段覆盖(3.6a)。
func NewPlayerNameStore(cfg MySQLConfig) (*PlayerNameStore, error)   // openMySQL,独立连接池
func (s *PlayerNameStore) Close() error

type ReserveOutcome uint8
const (
	ReserveInserted     ReserveOutcome = iota + 1 // 本次新插入
	ReserveAlreadyOwned                           // 同 player_id 同 name_norm 已在(幂等)
	ReserveTaken                                  // name_norm 属于别的 player_id(owner 随返回)
	ReserveConflict                               // 本 player_id 已登记别的名字
)
func (s *PlayerNameStore) Reserve(ctx context.Context, playerID uint64, name, norm string, nowMs uint64) (ReserveOutcome, uint64 /*owner*/, error)

type ReleaseOutcome uint8
const (
	ReleaseDeleted       ReleaseOutcome = iota + 1
	ReleaseAbsent                         // 没有 (player_id, name_norm) 这行
	ReleaseOutsideWindow                  // 行在,但 created_ms < minCreatedMs
)
// minCreatedMs = 0 表示不限时间(已过 admin 校验)。
func (s *PlayerNameStore) Release(ctx context.Context, playerID uint64, norm string, minCreatedMs uint64) (ReleaseOutcome, error)
func (s *PlayerNameStore) BatchGet(ctx context.Context, playerIDs []uint64) (map[uint64]string, error)
```

**Reserve**(无显式事务,单条 INSERT 即提交点):

```
for attempt := 1..3:
  INSERT INTO player_name (player_id, name, name_norm, created_ms) VALUES (?,?,?,?)
  成功 → (ReserveInserted, 0)
  1213/1205(isRetryableMySQL)→ continue
  非 1062 → return err
  // 1062:两条等值读分别走 PK / UK(不用 OR)
  SELECT player_id, name_norm FROM player_name WHERE player_id = ?
  SELECT player_id          FROM player_name WHERE name_norm = ?
  pk 行存在且 name_norm 相同 → AlreadyOwned
  pk 行存在且 name_norm 不同 → Conflict
  uk 行存在(player_id 不同) → (Taken, uk.player_id)
  两条都空(被并发 Release 删掉)→ continue
return fmt.Errorf("player_name reserve: exhausted 3 attempts")
```

并发抢同名:InnoDB 唯一键检查让后到的 INSERT 等锁,先到者提交后后到者得 1062 → Taken。autocommit 下每条 SELECT 是新快照,读得到刚提交的行。

**服务端不做"ctx 已超时就补偿删除"**:login 对超时会以同 `(player_id, name)` 重试一次(第 4 部分),重试可能在补偿 DELETE 之前拿到 AlreadyOwned=成功,随后补偿把在役角色的名字删掉。补偿统一由 login 的两次条件 Release 负责(3.7)。

**Release**:
```
DELETE FROM player_name WHERE player_id = ? AND name_norm = ? AND created_ms >= ?
affected=1 → Deleted
affected=0 且 minCreatedMs=0 → Absent
affected=0 且 minCreatedMs>0 → SELECT 1 FROM player_name WHERE player_id=? AND name_norm=? → 有 OutsideWindow / 无 Absent
```
**BatchGet**:`SELECT player_id, name FROM player_name WHERE player_id IN (?, …)`,入参由 logic 去重、去 0、≤500。

## 3.5 缓存:新文件 `go/data_service/internal/routing/player_name_cache.go`

放 routing 包是因为 `Router.mappingRedis` 未导出,与 `player:zone:` 同库(MappingRedis)。

```go
const playerNameKeyPrefix = "player:name:"
const playerNameAbsentSentinel = "\x00" // 合法名字不可能含 NUL(3.8 字符集)
func PlayerNameKey(playerID uint64) string
// hits:命中名字;absent:命中负缓存;两者都不含的 id = 未命中。
func (r *Router) MGetPlayerNames(ctx context.Context, ids []uint64) (hits map[uint64]string, absent map[uint64]bool, err error)
func (r *Router) SetPlayerNames(ctx context.Context, names map[uint64]string, ttl time.Duration) error // Pipelined SET EX(覆盖负缓存)
func (r *Router) SetPlayerNamesAbsent(ctx context.Context, ids []uint64, ttl time.Duration) error   // Pipelined SET NX EX
func (r *Router) DelPlayerName(ctx context.Context, playerID uint64) error
```

- 正缓存 TTL = `PlayerName.CacheTTL`(24h),只为给"释放后 DEL 失败"的脏值封顶。
- 负缓存 TTL = `PlayerName.NegativeCacheTTL`(60s),用 **SET NX**:永远盖不掉真名。时序:BatchGet 读库未命中 → Reserve 提交并 SET 真名 → BatchGet 的 SETNX 失败,正确。唯一坏序列"Reserve 的 SET 失败 + BatchGet SETNX 成功"最多让名字隐藏 60s,展示数据可接受。

## 3.6 Logic:新文件 `go/data_service/internal/logic/player_name_logic.go`

```go
var ErrPlayerNameStoreUnavailable = errors.New("player name store unavailable")
var ErrPlayerNameConflict = errors.New("player already holds a different name")
var ErrPlayerNameReleaseOutsideWindow = errors.New("release outside window requires admin token")
func ReservePlayerName(ctx, svcCtx, playerID uint64, raw string) (result uint32, owner uint64, err error)
// admin=true 表示 server 层已过 authorizeAdmin,不限窗口。
func ReleasePlayerName(ctx, svcCtx, playerID uint64, raw string, admin bool) error
func BatchGetPlayerName(ctx, svcCtx, ids []uint64) (map[uint64]string, error)
```

- Reserve:`playername.Normalize(raw, playername.StructuralRules)`(结构校验:UTF-8、1–32 字、字符集、敏感词;玩法长度 2–12 由 login 按配表把关,第 3、4 部分)。非 OK → `ReserveInvalid`,不碰库。store nil → `ErrPlayerNameStoreUnavailable`。`Inserted/AlreadyOwned` → 已提交,再 `SetPlayerNames`(失败只 ERROR,仍返回 0);`Taken` → `(1, owner)`;`Conflict` → `ErrPlayerNameConflict`。
- Release:Normalize 非 OK/Empty → `InvalidArgument`。`minCreatedMs = admin ? 0 : nowMs - ReleaseWindow.Milliseconds()`(nowMs < 窗口时取 0)。**先删库再 DEL 缓存**;`Deleted` → DEL(失败只日志)并 Info `[player-name] released player_id=%d admin=%v`;`OutsideWindow` → `ErrPlayerNameReleaseOutsideWindow`。
- BatchGet:去重去 0 → `MGetPlayerNames`(Redis 报错视为全未命中);未命中的 id 查库,store nil 或 SQL 错 → 返回错误;查到的 `SetPlayerNames` 回填,查不到的 `SetPlayerNamesAbsent`(均 best-effort);`absent` 命中的 id 不出现在结果里。
- server 层(`internal/server/dataserviceserver.go` 三个薄 handler):Release 若 `metadata.FromIncomingContext` 里有 `x-admin-token` 键 → 先 `authorizeAdmin`,通过则 `admin=true`;没有该键 → `admin=false`。
- 指标(无 player_id label,go-zero `metric.NewCounterVec/NewHistogramVec`,namespace 留空):
  - `data_service_player_name_ops_total{op="reserve|release|batch_get",result="ok|taken|invalid|conflict|outside_window|error"}`
  - `data_service_player_name_op_seconds{op}`,桶 `[0.001,0.005,0.01,0.025,0.05,0.1,0.25,0.5,1]`
  - `data_service_player_name_cache_total{result="hit|negative_hit|miss"}`

### 3.6a 配置与装配

`internal/config/config.go` 新增(全部 optional,零值取默认):

```go
type PlayerNameConfig struct {
	MaxOpenConn      int           `json:"MaxOpenConn,optional"`      // 默认 20
	MaxIdleConn      int           `json:"MaxIdleConn,optional"`      // 默认 5
	ReleaseWindow    time.Duration `json:"ReleaseWindow,optional"`    // 默认 10m
	CacheTTL         time.Duration `json:"CacheTTL,optional"`         // 默认 24h
	NegativeCacheTTL time.Duration `json:"NegativeCacheTTL,optional"` // 默认 60s
}
// Config 增字段 PlayerName PlayerNameConfig `json:"PlayerName,optional"`;Normalize() 里填默认值。
```

`etc/data_service.yaml` 在 `SnapshotMySQL` 段后加显式块(与默认相同,便于压测调参):
```yaml
PlayerName:
  MaxOpenConn: 20      # 独立连接池:建角每次一条同步 INSERT,别与快照/号段共用 5 连接
  MaxIdleConn: 5
  ReleaseWindow: 10m   # 无 admin token 的 Release 只能删这么新的登记
  CacheTTL: 24h
  NegativeCacheTTL: 60s
```

`svc/servicecontext.go`:新接口 `PlayerNameStore interface{ Close() error; Reserve(...); Release(...); BatchGet(...) }` 与字段;在 `if schemaReady {`(:115,与 `NewIdSegmentStore` 同块)里复制 `mysqlCfg`,覆盖 `MaxOpenConn/MaxIdleConn` 后 `store.NewPlayerNameStore(cfg)`,失败保持 nil 并 ERROR "player name store init failed (CreatePlayer disabled)";`Close()`(:154)补一行。

**上线顺序**(config.go:116-140:staging/prod `Schema.AutoMigrate=false`):先 `data_service -f <yaml> -migrate` 建出 `player_name` → 滚新 data_service → 再滚 login。dev 默认 AutoMigrate=true。

## 3.7 崩溃窗口与恢复

| 窗口 | 后果 | 处理 |
|---|---|---|
| INSERT 已提交、写缓存前崩 | 缓存缺 | BatchGet 回源回填 |
| INSERT 已发出、login 超时 | 结果未知 | login 同名重试一次(幂等);仍未知 → 立即条件 Release + 约 10s 后再发一次(防"Release 先于在途 INSERT 提交")|
| login 在延迟 Release 前崩溃 / 两次 Release 都失败 | 名字被不存在的角色占住(孤儿) | ERROR 日志带 player_id、name + `login_create_player_name_orphan_total`;运维带 x-admin-token 调 `ReleasePlayerName{player_id,name}` |
| DELETE 已提交、DEL 缓存失败 | 已释放 id 在缓存里仍有名 ≤24h | 该 id 不在任何账号 / 帮会,展示面不可见;唯一性只看库 |
| 全局库不可用 | 建角全部拒绝;读侧名字降级为空 | 与发号器、home zone 同档 fail-closed |

不做自动孤儿清扫:没有权威的"角色存在"信号(账号 blob 在 Redis 12h 过期,`player_to_account` 写失败不阻断建角),自动按它们释放会拿走在役角色的名字。

<!-- s3_names_part3.md -->

# §3 成员名字 — 第 3 部分:规则包、RoleNameRule 配表、login proto、登记客户端、Tip(B3a)

## 3.8 共享规则包:新目录 `go/shared/playername/`

login(按配表的玩法规则)与 data_service(结构复检)共用一份实现。

```go
package playername

// StructuralMaxRunes 是结构上限(player_name.name VARCHAR(64)、客户端输入框 64 个 UTF-16 单元),
// 不是玩法数值;玩法长度在 RoleNameRule 表。
const StructuralMaxRunes = 32

type Rules struct{ MinRunes, MaxRunes int }
var StructuralRules = Rules{MinRunes: 1, MaxRunes: StructuralMaxRunes}
func (r Rules) Validate() error // 1 ≤ MinRunes ≤ MaxRunes ≤ StructuralMaxRunes

const ( // ReservePlayerNameResponse.result
	ReserveOK      uint32 = 0
	ReserveTaken   uint32 = 1
	ReserveInvalid uint32 = 2
)

type Verdict uint8
const (
	VerdictOK Verdict = iota
	VerdictEmpty     // 去空白后为空:建角时由服务端生成
	VerdictInvalid   // 长度 / 字符集 / 非法 UTF-8
	VerdictSensitive // 命中敏感词
)

func Normalize(raw string, rules Rules) (display, norm string, v Verdict)
func IsAllowedRune(r rune) bool

type GenerateSpec struct{ Prefix string; SuffixLen int }
// 前缀每个 rune 都 IsAllowedRune、不命中敏感词;1 ≤ SuffixLen ≤ 16;前缀字数+SuffixLen ∈ [Min,Max]。
func (g GenerateSpec) Validate(rules Rules) error
// Generate:Prefix + SuffixLen 位 [a-z0-9];r 传 crypto/rand.Reader,测试注入固定字节。
func Generate(r io.Reader, spec GenerateSpec) (string, error)

type SensitiveChecker interface{ Contains(norm string) bool }
var DefaultSensitive SensitiveChecker = builtinSensitive{} // 占位词表,来源未定(E1)
```

`Normalize` 步骤(顺序固定):
1. `!utf8.ValidString(raw)` → Invalid。
2. `s := norm.NFKC.String(raw)`(`golang.org/x/text/unicode/norm`;全角字母数字、U+3000、多数兼容汉字在此折叠)。
3. `s = strings.TrimSpace(s)`;空 → Empty。
4. `n := utf8.RuneCountInString(s)`;`n < rules.MinRunes || n > rules.MaxRunes` → Invalid。
5. 每个 rune `IsAllowedRune`,否则 Invalid。
6. `display = s`,`norm = strings.ToLower(s)`。
7. `DefaultSensitive.Contains(norm)` → Sensitive(display/norm 照常返回供日志)。

`IsAllowedRune` **显式区间表**(不用 `unicode.Han`):`'0'–'9'`、`'A'–'Z'`、`'a'–'z'`、`U+3007 〇`、`U+3400–U+4DBF`(扩展 A)、`U+4E00–U+9FFF`(基本区)。理由:`unicode.Han` 还含部首补充 U+2E80–2EF3(除 U+2E9F、U+2EF3 外 NFKC 不折叠)、`々` U+3005、杭州码 U+3021–3029、`〻` U+303B,挡不住形近冒名(U+3038–303A 会被 NFKC 折叠成 十卄卅,折叠后与正字同 norm,无害);扩展 B 及以后(U+20000+)v1 不开放,客户端字体覆盖未核。未被 NFKC 折叠的兼容汉字(如 U+FA0E)不在区间内,两端一致拒绝。

共享测试向量(新文件)`go/shared/playername/testdata/charset_vectors.json`,客户端测试放逐字节相同的副本(第 8 部分),Codex 比对哈希:

```json
{
  "source": "xuanming-server-mmo/go/shared/playername/testdata/charset_vectors.json",
  "allowed":  ["0030","0039","0041","005A","0061","007A","3007","3400","4DBF","4E00","4E91","9FFF"],
  "rejected": ["0020","005F","00E9","0416","2E80","2EF3","3000","3005","3021","3029","3038","303B","F92C","FA0E","FF01","1F600","20000","323AF"]
}
```

内置占位词表:汉字词子串匹配 `管理员`、`客服`、`官方`、`系统`、`运营`;ASCII 只做前缀 `gm`(子串会误伤 `sigma`)。`Generate`:字母表 `abcdefghijklmnopqrstuvwxyz0123456789`,逐字节读、`b < 252` 才取 `b % 36`(拒绝采样去偏)。

依赖:`golang.org/x/text v0.32.0` 在 go/shared、login、data_service、guild 的 go.sum 均已有;go/shared/go.mod 去掉 `// indirect` 由 Codex `go mod tidy` 完成。

单测 `playername_test.go`(表驱动,rules={2,12}):
- `"  云中君 "` → OK,display=`云中君`;`"ＡＢ１２"` → display=`AB12`、norm=`ab12`;`"Ab12"` 与 `"aB12"` norm 相同。
- `"云"`、13 字、`"云 中君"`、`"云中君!"`、`"😀😀"`、`"Привет"`、`"々々"`、`"〡〡"`(U+3021)、`"⺀⺀"`(U+2E80)、`"﨎﨎"`(U+FA0E,不被 NFKC 折叠)、非法 UTF-8 → Invalid;`"〸〸"`(U+3038)经 NFKC 折叠为 `"十十"` → OK 且 norm 与直接输入"十十"相同(折叠后不构成冒名);`"   "` → Empty;StructuralRules 下 33 字 Invalid、1 字 OK。
- `"官方小助手"`、`"GM01"` → Sensitive;`"sigma"` → OK。
- 读 `testdata/charset_vectors.json`:allowed 全 true,rejected 全 false。
- `Rules{0,12}`、`Rules{2,33}`、`Rules{5,3}` Validate 报错;`GenerateSpec{"道友",6}.Validate({2,12})` 通过,`{"道友",11}` 报错。
- `Generate` 喂 `[]byte{0..255}` 断言跳过 ≥252、格式 `^道友[a-z0-9]{6}$`;1000 次 crypto/rand 结果全部 Normalize OK。

## 3.8a 配表 RoleNameRule(单行,决策 §0.5"所有数值进配表")

新文件 `data/schema/rolenamerule_table.proto`(照 petrule_table.proto 形状):

```proto
syntax = "proto3";

// RoleNameRule 表的权威 schema。源表 data/RoleNameRule.xlsx。
// 本文件不参与 protoc 编译;导表器直接文本解析。字段号只增不改(data/AGENTS.md)。
// 角色名单行全局规则(与 PetRule 同套路:只取 id=1)。结构上限 32 字在代码里(playername.StructuralMaxRunes)。

package mmorpg.cfgtable.v1;

import "cfg_options.proto";

message RoleNameRuleTable {
  option (cfg_sheet)       = "RoleNameRule";
  option (cfg_source_file) = "RoleNameRule.xlsx";
  option (cfg_primary_key) = "id";

  // 规则行 id(固定 1)
  uint32 id = 1;
  // 角色名最少字数(Unicode 码点,去首尾空白后)
  uint32 min_chars = 2;
  // 角色名最多字数(≤ 32)
  uint32 max_chars = 3;
  // 空名时服务端生成名的前缀
  string generated_prefix = 4;
  // 生成名随机后缀位数([a-z0-9])
  uint32 generated_suffix_len = 5;
  // 生成名撞名时最多尝试次数
  uint32 max_generate_attempts = 6;
  // 说明
  string desc = 7;
}
```

`data/RoleNameRule.xlsx`:第 1 行列名 `id|min_chars|max_chars|generated_prefix|generated_suffix_len|max_generate_attempts|desc`,第 6 行数据 `1|2|12|道友|6|5|角色名规则`(第 2–5 行照 PetRule.xlsx 表头格式)。

C++ 全节点加载全部表,导表器不改构建清单,手工登记:`cpp/generated/table/CMakeLists.txt`(`code/rolenamerule_table.cpp`、`proto/rolenamerule_table.pb.cc`)、`table.vcxproj` 与 `.filters`(ClCompile 同上;ClInclude `code\rolenamerule_table.h`、`code\rolenamerule_table_comp.h`、`proto\rolenamerule_table.pb.h`)。无 fk,无 `_fk` 文件。`data/AGENTS.md` 表清单加一行。客户端不在运行期加载配表(map_tables 已核),C# 生成物只随导表拷贝。

## 3.9 proto 改动(login 侧)

- `proto/login/login.proto` `CreatePlayerRequest` 增 `string name = 3;`(注释:正式客户端必填;空串 = 服务端生成,仅机器人 / 无 UI 路径)。`EnterGameResponse` 附近"项目没有昵称"注释(:94-103)改为"名字全服唯一,force_rename 管线保持未启用"。
- `proto/common/base/user_accounts.proto` `AccountSimplePlayer` 增 `string name = 5;`(注释:建角时写入的只读副本,真源 data_service player_name;可能为空,读侧回源)。

## 3.10 login:名字登记客户端 `go/login/internal/logic/pkg/playernamereg/playernamereg.go`(新)

```go
var ErrUnavailable = errors.New("player name registry unavailable") // 未配置:一个 RPC 都没发
const (
	DefaultReserveTimeout = 1000 * time.Millisecond
	DefaultReleaseTimeout = 1000 * time.Millisecond
	DefaultLookupTimeout  = 500 * time.Millisecond
	DelayedReleaseAfter   = 10 * time.Second
)
type ReserveResult struct{ Code uint32; Owner uint64 }
type Client struct{ ds dspb.DataServiceClient }
func New(ds dspb.DataServiceClient) *Client
func (c *Client) Reserve(ctx context.Context, playerID uint64, name string) (ReserveResult, error)
func (c *Client) Release(ctx context.Context, playerID uint64, name string) error
func (c *Client) Lookup(ctx context.Context, playerIDs []uint64) (map[uint64]string, error) // BatchGet
// Rules 读 RoleNameRule id=1 并 Validate;行缺失或非法 → error(调用方 fail-closed)。
func Rules() (playername.Rules, playername.GenerateSpec, int /*attempts*/, error)
```
`c == nil || c.ds == nil` → `ErrUnavailable`。`svc/servicecontext.go` 增字段 `PlayerNames *playernamereg.Client`,在 `initHomeZoneResolver()` 之后 `playernamereg.New(s.DataServiceClient)`(复用连接)。`Rules()` 里 `max_generate_attempts` 取 1–10 之外视为非法。

## 3.13 Tip 码(`data/tip/Tip.xlsx` `//login_error` 组尾追加;fault 列留空)

| A 列 | B 列文案(不写数字,长度来自配表) | Go 枚举 |
|---|---|---|
| RoleNameInvalid | 角色名长度或字符不符合要求,仅限汉字、字母与数字 | `table.LoginError_kRoleNameInvalid` |
| RoleNameTaken | 该角色名已被使用,请换一个 | `table.LoginError_kRoleNameTaken` |
| RoleNameSensitive | 角色名包含不允许使用的词语,请修改 | `table.LoginError_kRoleNameSensitive` |

login 回 `kRoleNameInvalid` 时 `TipInfoMessage.parameters = [min_chars, max_chars]`(十进制字符串),客户端据此显示具体字数。当前组末码 `kLoginTimeout = 2031`,新码由导表器顺延,代码不写数字。Tip.xlsx 有他人未提交改动,导表前 `git status` 核对。

<!-- s3_names_part4.md -->

# §3 成员名字 — 第 4 部分:login 建角、首次入场补齐、角色列表回源(B3a-2)

## 3.11 login:`createplayerlogic.go` 新顺序

顺序改为 **mint → reserve name → register home zone → 围栏写账号 blob**(原稿 register 在 reserve 前:撞名会留下永久 `player:zone:{id}` 幽灵映射,router.go:150 SET NX 无 TTL,回档孤儿报告与合服重映射都会扫到)。reserve 失败只烧掉一个号段 id,不留任何持久状态。

| 步 | 动作 | 失败 |
|---|---|---|
| 1-4 | 会话、账号锁(TTL 见下)、读账号 blob(:44-98) | 原样 |
| 6 / 6a | 角色数上限、职业 / 性别(:101-126) | 原样 |
| **6a'** | `rules, spec, attempts, err := loadNameRules()`(包变量,默认 `playernamereg.Rules`,测试替换);err → ERROR + `kLoginDataSerializeFailed`。`display, _, v := playername.Normalize(in.Name, rules)`:Invalid → `kRoleNameInvalid`(parameters=[min,max]);Sensitive → `kRoleNameSensitive`;Empty → `requested=""`;OK → `requested=display`。无副作用 | 见左 |
| 6b | `mintPlayerID`(:131) | 原样 |
| **6c** | `name, owner, tip := l.reservePlayerName(account, newPlayerId, requested, spec, attempts)` | 见下 |
| **6c'** | tip=`kRoleNameTaken` 且 owner≠0 且 owner 在 `userAccount.SimplePlayers` 中、其 ClassId/Gender 与本次 classId/gender 相同 → 视为"上次建角响应丢失后的重试":Info `[player-name] create retry matched existing player_id=%d (burned id=%d)`,**不建新角色**,按第 8 步返回当前全量列表、无 tip | — |
| **6d** | `registerHomeZone`(:145);失败 → `l.releaseName(newPlayerId, name, false)` 后返回原 tip | 原样 |
| 6e | `newPlayer` 增 `Name: name` | — |
| **7** | Marshal 失败 → releaseName → `kLoginDataSerializeFailed`。写入改 `writeAccountBlobScript`(下)| 见下 |
| 7b / 8 | 反向映射、返回列表(:171-183,不变) | — |

**6c' 的客户端效果**:CreatePlayerCo 在响应里 diff 不出新 id 时取最后一个角色(GameClient.cs:576-577,已核),直接进入那个已建好的角色。

### reservePlayerName

- 整体预算 `ctx, cancel := context.WithTimeout(l.ctx, 3*time.Second)`;每次 RPC 再套 `DefaultReserveTimeout`(1s)。
- `requested != ""`:`Reserve(id, requested)`
  - `Code 0` → 返回;`Code 1` → `kRoleNameTaken` + owner(未插入,不释放);`Code 2` → ERROR(login 与 data_service 规则版本错配)+ `kRoleNameInvalid`。
  - `ErrUnavailable`(一个 RPC 都没发)→ `kLoginDataSerializeFailed`,**不 Release、不计孤儿**。
  - `codes.FailedPrecondition`(同 id 已有别名)→ ERROR,不 Release(会删掉另一角色的名字),`kLoginDataSerializeFailed`。
  - 其它错误(超时 / Unavailable 等,结果未知)→ 剩余预算 ≥1s 则**同名重试一次**(同 id 同名幂等:在途 INSERT 若已提交,重试等锁后得 AlreadyOwned=0;若连接被杀、语句回滚,重试直接插入)。重试 0 → 返回;重试 1 → Taken(先前那次必未插入);仍是未知错误 → `l.releaseName(id, requested, true)`,`kLoginDataSerializeFailed`。
- `requested == ""`:最多 `attempts` 次 `playername.Generate(rand.Reader, spec)`;`1` → 换名再试;`0` → 返回;`2` → ERROR + `kLoginDataSerializeFailed`;未知错误按上面"同名重试一次 → releaseName(uncertain=true)"处理并结束;用尽 → `kLoginDataSerializeFailed`。

### releaseName(id, name, uncertain)

- 立即:`ctx := context.WithoutCancel(l.ctx)` + `DefaultReleaseTimeout`,`PlayerNames.Release(ctx, id, name)`。
- `uncertain=true` 时再 `afterFunc(delayedNameReleaseAfter, …)` 发第二次(包变量默认 `time.AfterFunc` 与 `playernamereg.DelayedReleaseAfter`=10s,测试替换;回调内 `defer recover()` 记 ERROR)。第二次覆盖"Release 先于在途 INSERT 提交"的竞态;minted id 已放弃,晚删安全;两次都在 data_service 10 分钟窗口内。
- 指标(clientplayerlogin/metrics.go,无 player_id label):`login_create_player_name_release_total{phase="immediate|delayed",result="ok|error"}`;`login_create_player_name_orphan_total`——uncertain 时仅延迟那次失败才 +1,非 uncertain 时立即那次失败 +1;ERROR 日志 `[player-name] orphan reservation player_id=%d name=%q account=%s: %v`(运维据此带 token 释放)。
- 阶段耗时 `login_create_player_stage_seconds{stage="mint|name|register|account_write"}`,桶 `[0.005,0.01,0.025,0.05,0.1,0.25,0.5,1,2.5,5]`。

### 第 7 步:围栏写账号 blob

```go
var writeAccountBlobScript = redis.NewScript(`
if redis.call("GET", KEYS[2]) ~= ARGV[1] then
    return -1
end
if ARGV[3] == "0" then
    redis.call("SET", KEYS[1], ARGV[2])
else
    redis.call("SET", KEYS[1], ARGV[2], "PX", ARGV[3])
end
return 1
`)
// KEYS = {accountDataKey, createLock.Key};ARGV = {createLock.Value, dataBytes, CacheExpire 毫秒}
```

| 结果 | 处理 |
|---|---|
| 1 | 成功,进 7b |
| -1(锁已丢,blob 确定未写) | releaseName(uncertain=false),`kLoginInProgress` |
| 脚本报错(结果未知) | 用 `WithoutCancel`+1s `GET accountDataKey` 回读:含 newPlayerId → 当成功进 7b;不含 → releaseName(uncertain=false)+`kLoginRedisSetFailed`;GET 也失败 → **保留登记**(宁可孤儿不可重名),ERROR 孤儿日志 + orphan +1,`kLoginRedisSetFailed` |

`Lock.Key/Value` 已导出(pkg/locker/player_locker.go TryLock 内赋值)。回读"不含"之后原 EVAL 才在 Redis 执行的残余风险需要该命令在服务端滞留超过 3s 读超时 + 一次往返,接受并注明。

**锁 TTL**:持锁后最坏 = 读 blob 3s(login.yaml:35-36 Read/WriteTimeout)+ 发号 3s(idsegment FetchTimeout)+ 名字 3s + 映射 3s(HomeZone.RegisterTimeout)+ 写 blob 3s + 回读 1s = 16s。`go/login/etc/login.yaml` `Locker.AccountLockTTL: 10` → **20**(k8s_deploy.ps1:1356 从该文件读,自动跟随;deploy/login-stack.linux/login.yaml 已是 30)。超时仍由脚本围栏兜住,不会写出超上限或丢角色的 blob。

## 3.12 login:首次入场写 `PlayerProfileComp`、角色列表回源

`PlayerAllData` 在建角时不存在:首次 EnterGame 由 `EnsurePlayerAllDataInRedisAsync` 建出,再由 `backfillPlayerClass` 整字节 CAS 补职业(player_class_backfill.go:17-70)。名字走同一条路。

1. `entergamelogic.go`:`enterGameSessionState`(:37)增 `playerName string`;新纯函数 `resolveEnterName(ctx context.Context, accountName string, names roleNameLookup, id uint64) string`:accountName 非空直接返回;否则 `names.Lookup(ctx+DefaultLookupTimeout, []uint64{id})`,任何错误 Info 日志后返回 ""(`roleNameLookup` 接口见第 3 条,`*playernamereg.Client` 实现它)。
   - 找到路径(:140 旁):`flowState.playerName = resolveEnterName(ctx, p.GetName(), l.svcCtx.PlayerNames, in.PlayerId)`(与 classID 同源:已验证归属的账号记录)。
   - self-heal 路径(:176-178):先 `name := resolveEnterName(ctx, "", l.svcCtx.PlayerNames, in.PlayerId)`,恢复记录写 `&AccountSimplePlayer{PlayerId: in.PlayerId, Name: name}`,`flowState.playerName = name`。
   有名字的正常角色不多一次 RPC;只有缺名记录才回源。
2. `player_class_backfill.go` 改名 `backfillPlayerIdentity`(调用点 :475):
   - 提前返回 `existing != nil || (state.classID == 0 && state.playerName == "")`。
   - 读 blob、解析、校验 player_id 不变。
   - `needClass := player.GetUint32PbComponent().GetClass() == 0 && state.classID != 0`;`needName := player.GetProfileComponent().GetName() == "" && state.playerName != ""`;都不需要 → return nil(替换原 :41-43)。
   - 填字段 → Marshal → 原 `backfillPlayerClassScript`(脚本与返回码语义不变)。**不在函数内调 RPC、不新增 error 返回**。
3. `loginlogic.go`:`roleListWithCurrentHomeZone`(:227)在 `buildRoleList` 之后调用新函数(不改 buildRoleList 签名,既有 homezone_rolelist_test.go 不动):
   ```go
   type roleNameLookup interface{ Lookup(ctx context.Context, ids []uint64) (map[uint64]string, error) }
   func fillMissingRoleNames(ctx context.Context, names roleNameLookup, roles []*login_proto.AccountSimplePlayerWrapper, timeout time.Duration)
   ```
   收集 `Name==""` 的 id,没有就不发 RPC;一次 Lookup,超时 `config.AppConfig.HomeZone.RoleListLookupTimeout`(0 → 500ms);错误 Info 日志后原样返回;命中的先 `proto.Clone` 再写 Name(不改账号对象,不回写 blob,与 zone 刷新同一纪律)。`l.svcCtx.PlayerNames` 为 nil `*Client` 时 Lookup 返回 ErrUnavailable,不 panic。

<!-- s3_names_part5.md -->

# §3 成员名字 — 第 5 部分:Java 网关、scene、zone 库迁移、合服 / 回档、机器人(B3a)

## 3.14 Java 网关:`LoginRpcClient.java`

`parseLoginResponse`(:586)的 `case 2`(:610-642)内层循环(AccountSimplePlayer)增加 `field 5 / wire 2`。在 `long playerId = 0;`(:617)下一行声明 `String name = null;`,内层循环体:

```java
int pField = pTag >>> 3, pWire = pTag & 0x7;
if (pField == 1 && pWire == 0) {
    playerId = in.readUInt64();
} else if (pField == 5 && pWire == 2) {
    name = in.readStringRequireUtf8();
} else {
    in.skipField(pTag);
}
...
p.setPlayerId(playerId);
p.setName(name);   // 老 login 不发字段 5 时为 null,客户端 GatewayPlayerInfo.name 已按空处理
```
同步改掉 :613-614 过时注释("目前只有 player_id 一个字段")。`LoginResponse.PlayerInfo` 已有 `name` / `setName`(dto/LoginResponse.java:75-86),DTO 不改。`parseLoginResponse` 由 `private static` 放宽为包级 `static`;新增 `java/gateway_node/src/test/java/com/game/gateway/grpc/LoginRpcClientParseTest.java`:`CodedOutputStream` 手编含 player_id=42、name="云中君" 的响应断言 `getName()`;不含字段 5 的断言 name 为 null、id 正确;含未知字段 6 的断言被跳过。

## 3.15 scene(C++)

### proto
- `proto/common/component/player_comp.proto` 在 `PlayerUint32Comp` 之后加:

```proto
// 角色展示资料。真源是 data_service 全局库 player_name;这里是随 player_database 走的只读副本,
// 由 login 首次入场补齐(player_class_backfill.go backfillPlayerIdentity)。scene 只读、只原样存回。
message PlayerProfileComp
{
	string name = 1;
}
```

- `proto/common/database/mysql_database_table.proto` `player_database` **在 message 末尾**(当前 `mission_component = 14` 之后)加:

```proto
  // 角色名副本,见 player_comp.proto PlayerProfileComp。
  // 【加列纪律】player_database 新字段一律取"落码当时的下一个空闲号",并声明在 message 末尾:
  // 新库 CREATE TABLE 按声明顺序建列,老库 cmd/migrate 只会 ADD COLUMN 追加到末尾,
  // 两者列序必须一致(tools/merge_zone 依赖列名对齐,见 3.16)。
  PlayerProfileComp profile_component = 15;
```

  **字段号 = 落码时的下一个空闲号**(2026-09-16 为 15)。若 B4a 的 `PlayerAssetOpLedgerComp` 或其它会话先占了 15,本字段顺延并仍放末尾;B4a 同样遵守(契约 §3.3 "落码前再核"的具体化,见 §3.25)。不再预留空洞:留 15 给后到者会让它声明在 16 之前,新库列序 `…14,15,16`、老库迁移后 `…14,16,15`,两列都是 MEDIUMBLOB,`INSERT … SELECT *` 会静默串档。

### 加载 / 存盘:`cpp/libs/services/scene/player/system/player_database_loader.cpp`
(该文件有他人未提交改动,按内容合并)
- `PlayerDatabaseMessageFieldsUnmarshal` 在 `emplace<PlayerPetComp>` 之后:`tlsEcs.actorRegistry.emplace<PlayerProfileComp>(player, message.profile_component());`
- `PlayerDatabaseMessageFieldsMarshal` 在 `mutable_pet_component()` 之后:`message.mutable_profile_component()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<PlayerProfileComp>(player));`(非 per-tick,`get_or_emplace` 合规)
- 显式 `#include "proto/common/component/player_comp.pb.h"`。

scene 从不写 name。首次入场补齐发生在 scene 加载之前(会话存在时 CAS 脚本回 2 跳过),正常路径 scene 一上来就有名字;补齐被跳过时本次在线战斗名为空,下次无会话入场再补。

### 战斗快照:`cpp/libs/services/scene/battle/system/player_battle.cpp:441-442`

```cpp
	// 名字副本来自 PlayerProfileComp(真源 data_service player_name);缺组件时留空,battle 侧照常结算。
	const auto* profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(player);
	snapshot.set_player_name(profile != nullptr ? profile->name() : std::string());
```
battle `turn_battle_engine.cpp:130` 已抄进 `BattleActorState.name`;match 观战 `gather.go:249-253` 的 `player_names` 自动有值,match 不改码。

### C++ gtest(`cpp/tests/bag_test/player_feature_persistence_test.cpp`)
- `ProfileNameRoundTripsThroughDatabaseRecord`:照 :178-201 写法,`cold.mutable_player_database_data()->mutable_profile_component()->set_name("云中君")` → `PlayerAllDataMessageFieldsUnMarshal` → 断言组件名 → `PlayerAllDataMessageFieldsMarshal` 存回 → `has_profile_component()` 且名字一致 → 序列化再解析仍一致。
- `MissingProfileComponentLoadsEmptyName`:不设字段,加载后组件存在、name 为空、存回不崩。
沿用 fixture `PlayerFeaturePersistenceTest`;文件顶部加 include。

## 3.15a zone 库加列(阻断项,随 B3a-1 上线)

go/db 启动期 DDL 默认关闭:`go/db/etc/db.yaml:62 AutoMigrateSchema: false`(本机 `run/etc/go_services/z2_db.yaml:62` 同),`proto_sql/db.go:308-311` 直接返回不建列;而 go/db 存盘的 INSERT 列清单来自完整 descriptor。proto-gen 一旦带上新字段,**未加列的 zone 库所有玩家存盘都会 unknown column 失败**。所以 B3a-1 的生成物落地后、任何新 go/db 或 scene 启动前,逐个 zone 执行:

```powershell
cd E:\work\xuanming-server-mmo\go\db
go run ./cmd/migrate -f etc/db.yaml -command plan                          # zone 1
go run ./cmd/migrate -f ..\..\run\etc\go_services\z2_db.yaml -command plan # zone 2(本机多 zone 时)
#   期望:只有 ALTER TABLE `player_database` ADD COLUMN `profile_component` ...;出现其它语句先停下核对
go run ./cmd/migrate -f etc/db.yaml -command up
go run ./cmd/migrate -f ..\..\run\etc\go_services\z2_db.yaml -command up
# 库名白名单拒绝时按 cmd/migrate/main.go 注释设置 DB_ALLOWED_DATABASES
# 核对(每个 zone 库):SHOW COLUMNS FROM player_database LIKE 'profile_component';  → 1 行
```
然后重启 go/db、scene。k8s:在新 db / scene 镜像滚动前跑 go/db migrate(k8s_deploy.ps1:1616 注释所述"部署阶段 cmd/migrate up"),顺序 migrate → go/db → scene。合服前置检查同样核这一列(3.16)。

## 3.16 合服 / 回档 / 摘角色

- **合服**:注册表在全局库,合服不搬不改;`PlayerProfileComp` 随 `player_database` 由 `copyPlayerRows` 带走。
  - 前置检查:源、目标 zone 库都有 `profile_component` 列(3.15a 的 SHOW COLUMNS)。
  - `tools/merge_zone/player_rows.go` 改为**按列名显式拷贝**:`copyPlayerRows`(:203)逐表循环里、调 `copyOneTable`(:276)之前,用现成的 `tableColumns(ctx, db, schema, table)`(:383)分别取 src / dst 列,`copyOneTable` 增参数 `cols []string`;新纯函数 `buildCopyColumns(src, dst []string) ([]string, error)`:两边列名集合不等 → 报错 `column set mismatch %s: only_in_src=%v only_in_dst=%v`(合服中止);相等则按 dst 顺序返回。语句改 `INSERT INTO dst (c1,…) SELECT c1,… FROM src WHERE player_id IN (…)`(列名反引号包裹)。列序漂移从"静默串档"变成"列名对齐、照常拷贝",列集合不一致变成"明确失败"。删掉 :285-287 "列顺序必然一致"注释。`deletePlayerRows` 已按列名 `d.col <=> s.col` 比较(:329-335),不改。
  - `audit_resources.go:433-440` `auditPlayerNameConflicts`:`Name: "player.name (global)"`、`UniqueScope: "global"`、`Severity: "info"`、`Notes: "名字在 data_service 全局库 player_name 全服唯一,合服无冲突、不改名;force_rename 管线保持未启用。"`。`force_rename_required` 与 `player_force_rename:{id}` 不动。
- **回档**:profile_component 随旧 player_database 回到旧值(同名或空,空的下次入场补齐,3.12)。注册表不回档、不释放。`reportOrphanCandidates`(rollback_logic.go:444)只报告,不改。
- **摘角色**:`remove_players_from_accounts.go` **本期不释放名字**。它的输入常是回档孤儿"候选"名单(rollback_logic.go:444-465 明言含普通老玩家),账号 blob 过期时 `removePlayersFromAccount` 返回 0、反向映射却照删;释放名字不可逆,不能挂在这条路上。需要释放时运维按 player_id + 名字带 x-admin-token 调 `ReleasePlayerName`(3.1)。
- **不改** `DeletePlayerData(delete_zone_mapping=true)`,列未决。
- **聚宝斋角色交易**:`jubaozhai-market.md` §6.3 `TransferPlayer` 为目标账号构造 `AccountSimplePlayer` 时必须带上 `name`(从源记录拷,源记录为空则 `BatchGetPlayerName`),写入 §3.25 由聚宝斋会话合入。

## 3.17 机器人

- `robot/login.go`、`robot/main.go` 继续发 `CreatePlayerRequest{}`(服务端生成名),压测与冒烟不改请求。
- `robot/login.go` 三处建角成功分支后加 Debug 日志打印新角色 `GetName()`,不断言。
- robot 带 vendor 拷贝(`robot/vendor/proto`、`robot/vendor/shared`),proto-gen 后 Codex **必须** `go mod vendor` 再 `go build ./...`。

<!-- s3_names_part6.md -->

# §3 成员名字 — 第 6 部分:B3a 文件清单、测试、Codex 验证

## 3.18 B3a 文件清单(手改;生成物、vendor、go.mod/go.sum tidy、纯注释改动、文档不计)

两批之间各自可编译:B3a-1 只加协议 / 配表 / data_service 能力,login 不调用新 RPC,行为不变;**但 B3a-1 的 `profile_component` 列必须先按 3.15a 迁移**。

**B3a-1(29)**
- proto(6):`proto/data_service/data_service.proto`、`proto/common/database/rollback_database_table.proto`、`proto/common/database/mysql_database_table.proto`、`proto/common/component/player_comp.proto`、`proto/login/login.proto`、`proto/common/base/user_accounts.proto`
- 配表(5):`data/RoleNameRule.xlsx`(新)、`data/schema/rolenamerule_table.proto`(新)、`cpp/generated/table/CMakeLists.txt`、`cpp/generated/table/table.vcxproj`、`cpp/generated/table/table.vcxproj.filters`
- Tip(1):`data/tip/Tip.xlsx`
- go/shared(3):`go/shared/playername/playername.go`(新)、`playername_test.go`(新)、`testdata/charset_vectors.json`(新)
- data_service(14,相对 `go/data_service/`):`internal/constants/error_codes.go`、`internal/config/config.go`、`internal/config/config_test.go`、`etc/data_service.yaml`、`internal/store/schema.go`、`internal/store/player_name_store.go`(新)、`internal/store/player_name_store_integration_test.go`(新)、`internal/store/schema_integration_test.go`、`internal/routing/player_name_cache.go`(新)、`internal/logic/player_name_logic.go`(新)、`internal/logic/player_name_logic_test.go`(新)、`internal/svc/servicecontext.go`、`internal/server/dataserviceserver.go`、`internal/server/dataserviceserver_test.go`
- 不计:`data_service.go:41`、`internal/store/storetest/mysql.go:143`(注释)、`data/AGENTS.md`(表清单文档)

**B3a-2(23)**
- login(13,相对 `go/login/`):`etc/login.yaml`、`internal/svc/servicecontext.go`、`internal/logic/pkg/playernamereg/playernamereg.go`(新)、`internal/logic/pkg/playernamereg/playernamereg_test.go`(新)、`internal/logic/clientplayerlogin/createplayerlogic.go`、`createplayer_name_test.go`(新)、`createplayer_homezone_test.go`、`entergamelogic.go`、`player_class_backfill.go`、`player_identity_backfill_test.go`(新)、`metrics.go`、`loginlogic.go`、`rolelist_names_test.go`(新)
- C++(3):`cpp/libs/services/scene/player/system/player_database_loader.cpp`、`cpp/libs/services/scene/battle/system/player_battle.cpp`、`cpp/tests/bag_test/player_feature_persistence_test.cpp`
- Java(2):`LoginRpcClient.java`、`LoginRpcClientParseTest.java`(新)
- 工具(4):`tools/merge_zone/audit_resources.go`、`tools/merge_zone/player_rows.go`、`tools/merge_zone/merge_unit_test.go`、`tools/scripts/stress_summarize.ps1`(新增 "CreatePlayer stages" 段:按快照打印 `login_create_player_stage_seconds_sum/count{stage}` 均值与 `login_create_player_name_orphan_total`,缺指标打印 n/a)
- robot(1):`robot/login.go`

B3a 不新增客户端可达消息,**不改** MessageLimiter.xlsx、gate 路由、`session.ClientMethods`;DataService 三个 RPC 只在服务间调用。

## 3.19 B3a 测试

**Go 单测**
- `playername_test.go`(3.8)。
- `player_name_logic_test.go`(fake store + miniredis Router):非法名不碰 store;Inserted 写缓存;缓存 SET 失败仍 0;Taken → `(1, owner)`;Conflict / store nil 返回对应错误;Release:admin=false 传 `minCreatedMs=now-10m`、admin=true 传 0、OutsideWindow → `ErrPlayerNameReleaseOutsideWindow`、Deleted 后缓存键被删;BatchGet:全中不调 store、部分未命中回源回填、库里没有的 id 写负缓存且第二次调用不再查库、负缓存 SETNX 不覆盖已有真名、Reserve 的 SET 覆盖负缓存、store 报错返回错误、去重去 0、501 个被拒。
- `config_test.go`:`PlayerName` 全缺省时 Normalize 得 20 / 5 / 10m / 24h / 60s。
- `dataserviceserver_test.go`:3.1 映射表驱动(照 `newHomeZoneTestServer`),含 taken 回 `owner_player_id`、Release 带错 token 返回 authorizeAdmin 错误、无 token 窗外 `FailedPrecondition` + `error_code=23`。
- `playernamereg_test.go`:nil `*Client` 三个方法都回 `ErrUnavailable`;纯函数 `rulesFromRow(row)`:nil 行、min=0、max=33、attempts=0 / 11、前缀含 `_` 均报错;`Rules()` 在 `gametable.LoadTables("../../../../../../generated/tables", false)` 后得 `{2,12}`、`{"道友",6}`、5。
- `createplayer_name_test.go`(fake DataServiceClient 计数 Reserve/Release/RegisterPlayerZone;包变量 `loadNameRules` 固定为 {2,12}/{道友,6}/5;`afterFunc` 替换为同步执行):
  1. `"云"` 在发号前被拒(minter 计数 0),tip=`kRoleNameInvalid`、parameters=`["2","12"]`;2. `"官方小助手"` → `kRoleNameSensitive`;
  3. Taken 且 owner 不在账号 → `kRoleNameTaken`,Register 0、Release 0;
  4. Taken 且 owner 在账号、class/gender 相同 → 无 tip、账号仍 1 个角色、Register 0;5. owner 在账号但 class 不同 → `kRoleNameTaken`;
  6. 空名:首次 Taken 再次 0 → 名字匹配 `^道友[a-z0-9]{6}$` 且写进账号 blob 与响应;
  7. Reserve 首次 DeadlineExceeded、重试 0 → 成功,Release 0;
  8. 两次都超时 → Release 立即 1 + 延迟 1(同名),`kLoginDataSerializeFailed`,Register 0;
  9. FailedPrecondition → Release 0;10. `PlayerNames` 为 nil → `kLoginDataSerializeFailed`、Release 0、orphan 计数不变;
  11. RegisterPlayerZone 失败 → Release 立即 1、延迟 0;
  12. fake Reserve 回调里删掉 `account_lock:create:<acct>` → `kLoginInProgress`、Release 1、账号 blob 未变。
- `createplayer_homezone_test.go`:`fakeRegisterClient` 增 `ReservePlayerName`(回 0)与 `ReleasePlayerName`(计数);harness 的 `svc.ServiceContext` 加 `PlayerNames: playernamereg.New(fake)` 并设置 `loadNameRules`;`TestCreatePlayer_RegistersHomeZoneThenPersists` 追加断言落盘角色 `Name` 非空;登记失败用例追加断言 Release 调用 1 次。
- `player_identity_backfill_test.go`(miniredis 支持 EVAL):职业与名字一次写入;blob 已有名字不覆盖;账号无名只补职业;`classID==0 && playerName==""` 不读 blob;会话存在脚本回 2 不写;纯函数 `resolveEnterName`:账号有名不调 lookup、无名调 1 次、lookup 报错返回 ""。
- `rolelist_names_test.go`:全有名不发 RPC;两个缺名一次 Lookup 带 2 个 id、回填在克隆上、原账号对象不变;Lookup 报错 / 超时 / nil `*Client` 原样返回不 panic。
- `merge_unit_test.go`:`buildCopyColumns` 同集合异序 → dst 顺序;src 多列、dst 多列 → 报错含列名。

**Go 集成**(`-tags=integration`,真 MySQL)`player_name_store_integration_test.go`:
- `TestPlayerName_BootstrapDDLIsVarcharUniqueBin`:`name_norm` 为 `varchar(191)`、`utf8mb4_bin`,`uk_player_name` NON_UNIQUE=0;二次迁移 ShowCreateTable 不变。
- `TestPlayerName_ReserveIdempotentAndTaken`:A 占 `ab12` → Inserted;A 重试 → AlreadyOwned;B 占 → Taken 且 owner=A;A 占 `cd34` → Conflict。
- `TestPlayerName_ConcurrentSameName`:32 goroutine 抢同一 norm,恰 1 Inserted、其余 Taken、表内 1 行。
- `TestPlayerName_ReleaseConditional`:norm 不符 → Absent 且行仍在;`minCreatedMs=now+1h` → OutsideWindow;`0` → Deleted;Release 后 B 可占。
- `schema_integration_test.go`:期望表集合加 `player_name` 列 `{player_id,name,name_norm,created_ms}`,"四张表"断言改五张。

C++:3.15 两个用例。Java:3.14。

## 3.20 B3a Codex 验证(按序串行)

```powershell
# ── B3a-1 ──
cd E:\work\xuanming-server-mmo; git status --short data proto generated tools/data_table_exporter/state go/*/generated robot/vendor cpp/generated/table
.\dev.bat gen                                   # 导 Tip + RoleNameRule(protoc-gen-go 在 PATH)
cd go; .\build.bat; cd ..                       # proto 重生
cd go/shared;       go mod tidy; go test ./playername/... ./generated/table/...
cd ../data_service; go vet ./...; go test ./internal/...
$env:DATA_SERVICE_IT_MYSQL_USER='root'; go test -tags=integration ./internal/store/... -run 'PlayerName|Schema' -count=1
cd ../login; go build ./...; cd ../match; go build ./...; cd ../guild; go build ./...; cd ../db; go build ./...
cd ../../robot; go mod vendor; go build ./...
# C++(Debug x64,/m:1):proto → table → core → scene 库 → scene 节点 → battle 节点
# 3.15a:每个 zone 库 cmd/migrate plan(只允许 ADD COLUMN profile_component)→ up → SHOW COLUMNS 1 行
# ── B3a-2 ──
cd E:\work\xuanming-server-mmo\go\login; go vet ./...; go test ./internal/logic/clientplayerlogin/... ./internal/logic/pkg/playernamereg/... ./internal/logic/admin/...
cd ../../robot; go mod vendor; go build ./...
cd ../tools/merge_zone; go test ./... -run 'Audit|BuildCopyColumns'
cd ../../java/gateway_node; mvn -q test -Dtest=LoginRpcClientParseTest,LoginRpcClientRetryTest
# C++:同上 + bag_test,运行 --gtest_filter=PlayerFeaturePersistenceTest.*
# 端到端前置(冒烟账号是固定的,已有 B3a 之前的无名角色,必须清):
#   全栈停 → redis-cli DEL account:robot_0000 account:robot_0001 account:robot_0002 account:robot_0003 account:robot_9001 account:robot_9002 account:robot_9201 account:robot_9202 account:robot_9203
#   (或本机 dev 直接 redis-cli FLUSHALL;不清 MySQL testdb,号段水位在里面)→ 全栈起
#   robot\robot.exe -c etc/robot_smoke.yaml   → 退出码 0
#   MySQL 全局库:SELECT COUNT(*) FROM player_name WHERE name LIKE '道友%';  ≥ 本次新建角色数
#   Redis:GET player:name:<新角色 id>  与库一致
#   robot\robot.exe -c etc/battle_smoke.yaml → 观战摘要 player_names 非空
# 负载:按 AGENTS §6.2 流程跑 robot\robot.exe -c etc/robot.stress-200.yaml(清 Redis;testdb 的 id_segment 保留,player_name 可 TRUNCATE),
#   stress_summarize.ps1 对比基线:"CreatePlayer stages" name 段均值 < 50ms、orphan=0,EnterGame fail% 不劣于基线
```
通过标准:全部退出码 0;集成测试 Skip 不算通过(改 root 重跑);负载项有对比表才算完成。失败保留 `go test -v` 输出与 data_service / login 日志 `[player-name]` 行。

<!-- s3_names_part7.md -->

# §3 成员名字 — 第 7 部分:B3b guild 批量查名、客户端规则与建角管线

## 3.21 guild 服务端(B3b,依赖 B2 已落的 guild.proto 与构造形状)

### proto(契约 §3.1 已定号;B2 已加则跳过)
`GuildMember.name = 7`、`GuildInfo.leader_name = 14`、`GuildRankEntry.leader_name = 8`,注释"展示用,不入库,来自 data_service BatchGetPlayerName,查不到为空"。

### 新文件 `go/guild/internal/logic/player_name_resolver.go`

```go
// PlayerNameResolver 批量取展示名。实现必须 fail-open:任何错误返回空 map,不返回 error。
type PlayerNameResolver interface {
	BatchResolve(ctx context.Context, playerIDs []uint64) map[uint64]string
}
const DefaultPlayerNameLookupTimeout = 800 * time.Millisecond
type DataServicePlayerNames struct{ client dspb.DataServiceClient; timeout time.Duration }
func NewDataServicePlayerNames(client dspb.DataServiceClient, timeout time.Duration) *DataServicePlayerNames
```
`BatchResolve`:nil 接收者或 nil client → 空 map;去重、去 0;超过 500 只查前 500 并 ERROR;`context.WithTimeout(ctx, timeout)`;错误 → `logx.Errorf("[guild] player name lookup failed (n=%d): %v")` + `guild_player_name_lookup_failed_total.Inc()` → 空 map。预算:guild zrpc ≤4000ms,GetGuild = 仓储 + 在线 MGET + 名字 ≤800ms。data_service 侧有 60s 负缓存(3.5),无名成员不会让每次拉取都打库。

### 装配
- `GuildLogic` 增字段 `playerNames PlayerNameResolver`。**不改 `NewGuildLogic` 参数表**(guild_logic.go:46 现 5 参,测试 18 处调用),加装配方法:
  ```go
  // SetPlayerNameResolver 只在启动装配时调用一次(guild.go),之后只读;nil = 名字一律为空。
  func (l *GuildLogic) SetPlayerNameResolver(r PlayerNameResolver) { l.playerNames = r }
  ```
  若 B2 已把构造改成依赖结构体,改为结构体加 `PlayerNames` 字段,本方法不加。
- `guild.go`:在 :162 `guildLogic := logic.NewGuildLogic(...)` 之后,`if svcCtx.DataServiceClient != nil { guildLogic.SetPlayerNameResolver(logic.NewDataServicePlayerNames(svcCtx.DataServiceClient, logic.DefaultPlayerNameLookupTimeout)) }`。已核:DataServiceClient 为 nil 时 guild **只记 ERROR 继续起服**(guild.go:156-160;只有 `IdSegment.Enabled=true` 时 guild_id_minter.go:42 才拒启),此时 resolver 保持 nil,名字一律为空,不 panic。

### 填充点(`guild_logic.go`)
- `toProtoGuild`(:576):`ids := append(memberIDs, g.LeaderID)` → `names := l.resolveNames(ctx, ids)`(nil 安全包装)→ `GuildMember.Name = names[m.PlayerID]`、`info.LeaderName = names[g.LeaderID]`。与 `onlineResolver.BatchResolve` 顺序执行。
- `enrichRankEntries`(:552):循环内收集 `leaderIDs`,循环后**一次** `BatchResolve` 回填 `LeaderName`。
- `GetGuildRankByGuild`(:510):单条 `BatchResolve([]uint64{guild.LeaderID})`。
- B2 的 `ListGuildApplications` 构造 `GuildApplicantView` 复用同一 resolver。

### guild 测试
- `player_name_resolver_test.go`:成功映射;RPC 报错 → 空 map;nil client → 空 map;重复 id 与 0 只发一次 / 被丢弃;慢响应超 timeout → 空 map。
- `guild_logic_names_test.go`(新,照 client_zone_test.go 的 miniredis + 仓储夹具,构造后 `SetPlayerNameResolver(fake)`):fake `{A:"云中君"}`,A 帮主、B 成员 → `GetGuild` 里 A 的 `Name`、`LeaderName` 为"云中君",B 为空;`toProtoGuild` 一次只发 1 次批量查询;`GetGuildRank` 两个帮会只 1 次且各自 `LeaderName` 正确;不注入时全空且不 panic。
- robot `guild_smoke_scenario.go`:B 加入后第一次 `myGuild()` 处加步骤 `member-names`:每个成员 `GetName() != ""`、`GetLeaderName() != ""`;失败文案 `fail("member-names", "role %d has no name: legacy role created before B3a — DEL account:robot_9201/9202/9203 then rerun")`;通过时日志打印名字。

## 3.22 客户端(B3b,`E:\work\mmorpg-client`)

### 协议
`tools/gen_proto.ps1` 的 `$files` 增 `"generated/code/proto/tip/login_error_tip.proto"`;`pwsh -File tools/gen_proto.ps1` 重生 Login / UserAccounts / Guild / login_error 的 C#。枚举形如 `login_error.KRoleNameTaken`(同 `guild_error.KGuildNameTaken`,GuildClient.cs:166-177)。

### 新文件 `Assets/Scripts/Game/Role/RoleNameRules.cs`

```csharp
public static class RoleNameRules
{
    // 结构上限,与服务端 playername.StructuralMaxRunes 对齐;玩法字数(2–12)在服务端 RoleNameRule 表,客户端不写死。
    public const int StructuralMaxChars = 32;
    public static bool IsAllowedCodePoint(int cp);
    public static bool TryNormalize(string raw, out string display, out string error);
    public static string RandomName(System.Random rng);
    public static string TipText(uint tipId, IList<string> parameters);   // 三个名字 tip;其它 null
    public static string RetryableCreateHint(uint tipId);                  // 服务端可重试失败;其它 null
}
```

- `IsAllowedCodePoint`:`'0'–'9'`、`'A'–'Z'`、`'a'–'z'`、`0x3007`、`0x3400–0x4DBF`、`0x4E00–0x9FFF`(与 Go 同一张表,扩展 B 起不开放)。
- `TryNormalize`(**不抛异常**):
  1. `raw ??= ""`;逐 char 扫描:高代理后面不是低代理、或孤立低代理 → `error="角色名包含无效字符"`,false(TMP 截断可能切开代理对)。
  2. `try { s = raw.Normalize(NormalizationForm.FormKC).Trim(); } catch (ArgumentException) { error="角色名包含无效字符"; return false; }`
  3. 空 → `error="请输入角色名"`,false(建角必填)。
  4. 按码点遍历(代理对 `char.ConvertToUtf32`):非 `IsAllowedCodePoint` → `"角色名仅限汉字、字母与数字"`;码点数 > 32 → `"角色名过长"`。
  5. `display = s`,true。字数下限上限由服务端 `kRoleNameInvalid` 回显。
- `TipText`:`KRoleNameInvalid` → parameters≥2 时 `$"角色名需为 {p[0]}–{p[1]} 个字,仅限汉字、字母与数字"`,否则"角色名长度或字符不符合要求,仅限汉字、字母与数字";`KRoleNameTaken` → "该角色名已被使用,请换一个";`KRoleNameSensitive` → "角色名包含不允许使用的词语,请修改"。
- `RetryableCreateHint`:`KLoginDataSerializeFailed`、`KLoginInProgress`、`KLoginRedisSetFailed` → "服务繁忙,角色未创建,请稍后再试"。
- `RandomName`:`姓 + 名`,姓 80 个、名由两池组成,共 80×(60×60+60)=292,800 种,结果 2–4 字(已脚本核对:单姓 68、池 A 60、池 B 60 均无重复且都在 U+4E00–9FFF;名池不含 管理员客服官方系统运营 任一字;复姓"上官""东方"虽含"官""方",与名池拼接不会构成内置敏感词,由测试断言):
  - 姓(68 单 + 12 复):赵钱孙李周吴郑王冯陈褚卫蒋沈韩杨朱秦尤许何吕施张孔曹严华金魏陶姜谢邹喻柏水窦章云苏潘葛范彭郎鲁韦马苗凤花俞任袁柳鲍史唐薛雷贺倪汤殷罗毕郝;慕容、上官、欧阳、司马、诸葛、东方、南宫、独孤、令狐、皇甫、公孙、轩辕
  - 池 A(60):清玄若无青子长听明星逸紫行问凌云风月雪霜寒江秋春夜晨松竹梅兰书墨琴剑歌羽鸿鹤灵素静安宁远怀知思慕景修承天君如惊落流飞映望
  - 池 B(60):尘忌衫歌雪河烟舟道霄澜川岚溪泉峰岳林萧瑶璃珏琳瑜璇华辰曦阳光影声心意情仪然真虚空渊宸翎翊珩砚笙箫弦诗棠蘅芷蔚蕴菡萱茗荷萝
  - 算法:`s = 姓[rng.Next(80)]`;`rng.Next(5) == 0` → `s + B[rng.Next(60)]`,否则 `s + A[rng.Next(60)] + B[rng.Next(60)]`。

### `Assets/Scripts/Game/GameClient.cs`
1. `PlayerChoice`(:155)增 `public string Name;`、`public string RejectHint;`。
2. `CreatePlayerCo`(:551)签名改 `(int gen, uint classId, uint gender, string name, RepeatedField<AccountSimplePlayerWrapper> known, Action<ulong> onCreated, Action<string> onRetryableReject, Action<string> onError)`;请求 `new CreatePlayerRequest { ClassId = classId, Gender = gender, Name = name ?? "" }`;:567 tip 分支在 `FailPipeline` 前插入:`var hint = RoleNameRules.TipText(id, cpResp.ErrorMessage.Parameters) ?? RoleNameRules.RetryableCreateHint(id); if (hint != null && onRetryableReject != null) { onRetryableReject(hint); yield break; }`。传输错误(Call 的错误回调、断线、15s 超时)仍 FailPipeline。无 UI 分支(:527)改 `CreatePlayerCo(gen, 0, 0, "", loginResp.Players, id => newId = id, null, onError)`(服务端生成名)。
3. 有 UI 分支(:503-527)改为**不设轮数上限**的循环(玩家随时可取消):
```csharp
var choice = new PlayerChoice();
while (true)
{
    yield return PlayerChooser(zoneId, zonePlayers, choice);
    if (gen != _pipelineGen) yield break;
    if (choice.Cancelled) { FailPipeline(gen, onError, "已返回选服"); yield break; }
    if (!choice.CreateNew) { playerId = choice.SelectedPlayerId; break; }
    ulong newId = 0; string reject = null;
    yield return CreatePlayerCo(gen, choice.ClassId, choice.Gender, choice.Name,
        loginResp.Players, id => newId = id, h => reject = h, onError);
    if (gen != _pipelineGen) yield break;
    if (reject != null) { choice.RejectHint = reject; choice.CreateNew = false; continue; } // 保留 Name/ClassId/Gender
    if (newId == 0) yield break; // CreatePlayerCo 已 FailPipeline
    playerId = newId; break;
}
```
4. `ResolveCharacterId`(:216)旁加 `public string ResolveRoleName(ulong playerId) => playerId != 0 && _knownRoles.TryGetValue(playerId, out var r) ? r.Name : null;`(`_knownRoles` 由 `CacheRoleMetadata` 填,:230-240)。

<!-- s3_names_part8.md -->

# §3 成员名字 — 第 8 部分:客户端界面与测试、B3b 清单与验证

## 3.22(续)客户端界面

### `Assets/Scripts/UI/Ugui/Role/RoleFlowUi.cs`
- 字段:`TMP_InputField _nameInput; UiTextButton _randomNameButton; readonly System.Random _rng = new System.Random();`。
- `BuildCreateRoot`(:392)末尾,挂在 `_createRoot` 下(随建角模式显隐),占右侧详情窗标题位(详情窗 x1920–2430、y174–830;`_previewTitle` 在 1980,250,390×88):
  - 已核:`BuildCanvas` 顺序 `BuildDetails → BuildSelectRoot → BuildCreateRoot`(:337-339),`_createRoot` 下控件画在详情窗之上;`TextButton` 固定高 112、文字区宽 = width − 88(:447-457);`CreateInputField` 不给底图时底色 alpha≈0(QdaoUguiFactory.cs:216-218),且无字号参数。
  - `_nameInput = QdaoUguiFactory.CreateInputField("RoleNameInput", _createRoot, 1940f, 226f, 330f, 84f, "请输入角色名", 64, QdaoRefreshArt.Load("list_row_normal"));` 随后 `_nameInput.textComponent.fontSize = 28f; ((TMP_Text)_nameInput.placeholder).fontSize = 26f;`(照 PetPanel.cs:304-307、JubaozhaiWindow.cs:85-87)。`characterLimit` 64 是结构上限(TMP 按 UTF-16 计数,32 码点 ≤ 64 单元),真正字数由服务端回显把关。
  - `_randomNameButton = TextButton("RandomRoleName", _createRoot, 2276f, 212f, 150f, "随机", false, 26f);`(x 2276–2426、y 212–324,竖直中心 268 与输入框对齐;文字区宽 62 放得下两个 26 号字;在详情首行 y376 之上、右缘 2430 之内)。点击 `_nameInput.text = RoleNameRules.RandomName(_rng)`。
  - `Update()`(:147)Esc 分支前加 `if (_nameInput != null && _nameInput.isFocused) return;`。
- `ShowCreateMode`(:176):`_previewTitle.gameObject.SetActive(false)`;`_previewHint.text` = `_result?.RejectHint` 非空则显示它(颜色 Gold),否则原文案"创建后将直接进入所选区服"。
- `ShowSelectMode`(:162):`_previewTitle.gameObject.SetActive(true)`;`if (_result != null) _result.RejectHint = null;`(离开建角即清掉上次拒绝原因)。
- `Choose`(:95):`choice.RejectHint` 非空 → 沿用 `choice.ClassId/Gender`(非 0 时)与 `_nameInput.text = choice.Name ?? ""`,直接 `ShowCreateMode()`;否则原初始化并清空输入框。
- `ResolveCreate`(:130):
  ```csharp
  if (!RoleNameRules.TryNormalize(_nameInput.text, out var n, out var err)) { _previewHint.text = err; return; }
  var font = _nameInput.textComponent.font;
  if (font != null && !font.HasCharacters(n, out List<char> missing, true, true)) { _previewHint.text = "角色名包含无法显示的字"; return; }
  _result.Name = n; _result.RejectHint = null;
  ```
  (字体缺字检查只在客户端:同一字体资产下别人也显示不了;`TMP_FontAsset.HasCharacters(string, out List<char>, bool searchFallbacks, bool tryAddCharacter)`,若该重载在工程 TMP 版本不存在,Codex 编译体检会报错,改用 `HasCharacters(string, out List<char>)`。)
- 角色卡(:227-229):名字行改 `DisplayName(player)`;新增 `public static string DisplayName(AccountSimplePlayer p) => string.IsNullOrWhiteSpace(p.Name) ? CharacterName(p.ClassId, p.Gender) : p.Name;`。
- `RefreshPreview`(:278)加参数 `string roleName`:`_previewTitle.text = string.IsNullOrWhiteSpace(roleName) ? CharacterName(classId, gender) : roleName;`;`PreviewPlayer` 传 `player.Name`,`RefreshCreateHighlights` 传 null。

### 头顶名 / 会话昵称
`Assets/Scripts/UI/AppBootstrap.cs` `ResolveActorDisplayName`(:127):取到 `playerId` 后先 `var n = client.ResolveRoleName(playerId); if (!string.IsNullOrWhiteSpace(n)) return n;`,再走原 `GatewayPlayerInfo.name` 与 `SessionModel.RoleNickname`。`SessionModel.RoleNickname`(:60)默认值 `"云行客"` 改 `""`。

### `Assets/Scripts/UI/Ugui/Guild/GuildWindow.cs`
- 新增 `public static string MemberDisplayName(GuildMember m) => string.IsNullOrWhiteSpace(m.Name) ? "道友 · " + m.PlayerId : m.Name;`,:255 改用它。
- 搜索(:236):`member.PlayerId.ToString().Contains(q) || (!string.IsNullOrEmpty(member.Name) && member.Name.Contains(q))`;:244 占位符"输入名字或编号"。
- 概览侧栏(:161-162):"帮主编号" → "帮主";值 `string.IsNullOrWhiteSpace(info.LeaderName) ? info.LeaderId.ToString() : info.LeaderName`。已核 `LeaderId` 只在 :162 显示。

### 客户端测试(EditMode)
- 新目录 `Assets/Tests/EditMode/Role/`:`MmorpgClient.Tests.EditMode.Role.asmdef`(照 Guild 的 asmdef,改 name/rootNamespace)、`RoleNameCharsetVectors.json`(与服务端 `go/shared/playername/testdata/charset_vectors.json` **逐字节相同**)、`RoleNameRulesTests.cs`:
  - `" 云中君 "` → `云中君`;`"ＡＢ１２"` → `AB12`;`"云中君!"`、`"😀😀"`、33 个"云" 失败;`""`、`"   "` 失败且 error="请输入角色名"。
  - `"\uD840"`、`"云\uD840"`、`"\uDC00云"` 返回 false、error="角色名包含无效字符"、不抛异常。
  - 读 `Path.Combine(Application.dataPath, "Tests/EditMode/Role/RoleNameCharsetVectors.json")`,`JsonUtility.FromJson<Vectors>`(`[Serializable] class Vectors { public string source; public string[] allowed; public string[] rejected; }`),`IsAllowedCodePoint(Convert.ToInt32(hex, 16))` 与 allowed/rejected 一致。
  - `var rng = new System.Random(12345)` 连续生成 100,000 个 `RandomName`:全部 `TryNormalize` 通过、均不含 管理员/客服/官方/系统/运营、去重后 ≥ 20,000 个(用单一随机流;相邻种子的 `System.Random` 首个输出相关,不用"每种子一个")。
  - `TipText(KRoleNameInvalid, ["2","12"])` 含 "2–12";无参数时非 null;Taken / Sensitive 非 null;`TipText(0, null)` 为 null;`RetryableCreateHint(KLoginDataSerializeFailed)` 非 null、`RetryableCreateHint(KRoleNameTaken)` 为 null。
  - `RoleFlowUi.DisplayName`:空名回落职业名、有名用名字。
- `Assets/Tests/EditMode/Guild/GuildUiTests.cs` 加 `MemberDisplayNameFallsBackToIdWhenNameMissing`、`MemberDisplayNameUsesServerName`。

## 3.23 B3b 文件清单(手改 19)
服务端 7:`proto/guild/guild.proto`(B2 已加字段则不计)、`go/guild/guild.go`、`go/guild/internal/logic/guild_logic.go`、`go/guild/internal/logic/player_name_resolver.go`(新)、`go/guild/internal/logic/player_name_resolver_test.go`(新)、`go/guild/internal/logic/guild_logic_names_test.go`(新)、`robot/guild_smoke_scenario.go`。
客户端 12:`tools/gen_proto.ps1`、`Assets/Scripts/Game/Role/RoleNameRules.cs`(新)、`Assets/Scripts/Game/GameClient.cs`、`Assets/Scripts/UI/Ugui/Role/RoleFlowUi.cs`、`Assets/Scripts/UI/AppBootstrap.cs`、`Assets/Scripts/UI/SessionModel.cs`、`Assets/Scripts/UI/Ugui/Guild/GuildWindow.cs`、`Assets/Tests/EditMode/Role/MmorpgClient.Tests.EditMode.Role.asmdef`(新)、`Assets/Tests/EditMode/Role/RoleNameCharsetVectors.json`(新)、`Assets/Tests/EditMode/Role/RoleNameRulesTests.cs`(新)、`Assets/Tests/EditMode/Guild/GuildUiTests.cs`;`.meta` 由编辑器生成,不计。(若 `RoleFlowUi.cs` 所在 asmdef 对测试不可见,在测试 asmdef 的 references 里加它,同文件计数内。)

## 3.24 B3b Codex 验证
```powershell
cd E:\work\xuanming-server-mmo; git status --short proto/guild go/guild robot
cd go; .\build.bat; cd guild; go vet ./...; go test ./internal/logic/... -count=1
cd ..\..\robot; go mod vendor; go build ./...
# 向量文件一致性(两仓库)
(Get-FileHash E:\work\xuanming-server-mmo\go\shared\playername\testdata\charset_vectors.json).Hash -eq (Get-FileHash E:\work\mmorpg-client\Assets\Tests\EditMode\Role\RoleNameCharsetVectors.json).Hash   # 必须 True
cd E:\work\mmorpg-client; git status --short Assets/Scripts/Game Assets/Scripts/UI Assets/Tests tools; pwsh -File tools/gen_proto.ps1; pwsh -File tools/client_compile_check.ps1
# Unity EditMode(不带 -quit):-runTests -testPlatform EditMode -assemblyNames "MmorpgClient.Tests.EditMode.Role;MmorpgClient.Tests.EditMode.Guild" -testResults run/role_guild_editmode.xml
# 端到端:先按 3.20 清冒烟账号;全栈 + guild 起;robot\robot.exe -c etc/guild_smoke.yaml → GUILD_SMOKE_OK 且日志含 member-names 通过
# 手动/出包:输入框留空点创建 → 停在建角页提示"请输入角色名";输入"云中君"成功;第二个账号再输同名 → 停在建角页提示"已被使用"且输入框保留"云中君";
#   输入"云" → 提示"角色名需为 2–12 个字…";点"随机"连点 20 次无明显重复;帮会成员列表显示名字
```

<!-- s3_names_part9.md -->

# §3 成员名字 — 第 9 部分:契约偏差、未决

## 3.25 契约偏差

1. **player_name 不能纯 proto 建表**:proto2mysql 把 string 渲染为 MEDIUMTEXT,`name_norm` 唯一键会撞 MySQL 1170。沿用 id_segment 的 bootstrap DDL(VARCHAR(191) + `utf8mb4_bin`),proto message 只做列校验。表名、字段名不变。
2. **`player_database` 加列纪律**(契约 §3.3"当前 15,落码前再核"的具体化,**B4a 与其它会话同样适用**):新字段取落码当时的下一个空闲号,并声明在 message 末尾。`profile_component` 按 2026-09-16 状态取 15;谁先落码谁先占号,后到者顺延。不再给别人预留空洞号——预留会让后到字段声明在前,新库与迁移库列序不一致。B4a 的 `PlayerAssetOpLedgerComp` 请按此取号(若 B3a 先落则为 16)。
3. **"写入于创建时"实为首次入场补齐**:建角时 `PlayerAllData` 不存在,名字随 `backfillPlayerIdentity` 同一次 CAS 写入;另加读侧回源(入场、角色列表缺名即 `BatchGetPlayerName`)。
4. **`result` 不含"敏感"值**:敏感词在 data_service 归入 2,login 用同一共享包预检区分 `RoleNameSensitive`。若日后词表只在服务端,需加 `3 sensitive`。
5. **契约外的 RPC 字段**:`ReservePlayerNameResponse.owner_player_id = 2`(login 判定丢响应重试,不下发客户端);`ReleasePlayerNameRequest.name = 2`(条件删除;无 token 只删 `ReleaseWindow` 内的登记,带 x-admin-token 不限)。
6. **故障语义与错误码**:同 id 别名 → `FailedPrecondition`/26;库故障 → `Unavailable`/25;窗外无 token → `FailedPrecondition`/23(复用 `ErrCodeAdminAuthRequired`)。25/26 以落码时末码顺延为准。
7. **建角顺序**:mint → reserve name → register home zone → 围栏写账号 blob(契约未规定顺序;上一稿写成"契约顺序 register 在前"有误,且会留幽灵映射)。
8. **新增配表 `RoleNameRule`**(契约 §5 未列):落实决策 §0.5"所有数值进配表";结构上限 32 字留在代码(与 VARCHAR(64) 绑定,不是玩法数值)。data_service 只做结构校验,玩法字数由 login 按表把关。Tip 文案不写数字,`kRoleNameInvalid` 以 `parameters=[min,max]` 回显。
9. **运维配置**:data_service yaml 新 `PlayerName` 段(连接池 20/5、ReleaseWindow 10m、缓存 24h、负缓存 60s);`go/login/etc/login.yaml` `Locker.AccountLockTTL` 10 → 20,并以 Lua 围栏写账号 blob。都是部署参数,不进 xlsx。
10. **B3a 超 30 文件**:拆 B3a-1(29)/ B3a-2(23)(3.18)。B3a-1 上线必须带 zone 库 `cmd/migrate` 加列步骤(3.15a),契约批次表未提。
11. **B3a-2 捎带 merge_zone 按列名拷贝**(`copyOneTable` 不再 `SELECT *`):不属于名字功能本身,但本批是第一个在 `player_database` 加 BLOB 列的批次,不修则列序漂移会静默串档。
12. **摘角色不释放名字**:`RemovePlayersFromAccounts` 输入含回档孤儿"候选"(可能是在役老玩家),释放不可逆,本期不接;运维按日志带 token 调 `ReleasePlayerName`。
13. **聚宝斋 `TransferPlayer`**(`jubaozhai-market.md` §6.3):给目标账号构造 `AccountSimplePlayer` 时必须带 `name`(源记录为空则 `BatchGetPlayerName`)。由聚宝斋会话合入其文档与实现,本节只登记约束。
14. guild 名字解析器用 `SetPlayerNameResolver` 注入,不改 `NewGuildLogic` 参数表(避免改 18 处测试调用)。
15. 文档不计入手改文件数,随契约 §6 文档批次提交:`docs/design/guild-phase2.md` §3(本节)、`docs/ops/merge-zone-runbook.md` §4.4(删"项目没有昵称",改"名字全局唯一,合服无冲突";前置检查加 `profile_component` 列与列集合一致)、`docs/design/team-system.md` §G.2(名字来源 `BatchGetPlayerName`,组队会话合入)、`data/AGENTS.md` 表清单。

## 3.26 未决(需用户 / 他节拍板)

- **敏感词词表来源**(E1),以及帮会名是否复用 `playername.DefaultSensitive`。
- **自动孤儿清扫**:当前没有权威的"角色存在"信号(账号 blob 在 Redis 12h 过期后 `GetOrInitUserAccount` 静默重建为空;`player_to_account` 写失败不阻断建角),自动清扫会误删在役角色的名字。v1 只做日志 + 指标 + 带 token 手工 Release;账号数据有持久真源后再议。
- **按名字查人** `FindPlayerByName{name}`(走 `name_norm` 唯一键、需 x-admin-token):GM / 客服 / 日后"按名字加好友"会要,v1 不做。
- `DeletePlayerData(delete_zone_mapping=true)` 是否应释放名字(需先确认合服清理源区是否调用它)。
- 扩展 B 及以后汉字(U+20000+)何时开放:先核客户端 TMP 字体覆盖,再同时改 Go / C# 区间表与向量文件。
- 组队(`TeamMemberView.name`)、切磋邀请(`challengelogic.go:157` 用账号名)、聊天发送者名:各会话接 `BatchGetPlayerName`。
- 远端头顶名需 `ActorCreateS2C` 带名字,不在本期。

---

## 附录:对抗评审处理记录

# §3 成员名字 — 对抗评审处理记录(2026-09-16)

原 4 个分部已重写,内容扩到 9 个分部(`s3_names_part1.md`–`s3_names_part9.md`)。每条先对照代码核实,再决定处理方式。

| # | 级别 | 问题 | 结论 | 核实与处理 |
|---|---|---|---|---|
| 1 | 阻断 | zone 库不会自动加 `profile_component` 列 | **已采纳** | 核实:`go/db/etc/db.yaml:62` 与本机 `run/etc/go_services/z2_db.yaml:62` 都是 `AutoMigrateSchema: false`(yaml 注释说本地保持 true,与实际值不符);`proto_sql/db.go:308-311` 直接返回;本地启动脚本不跑 cmd/migrate。新增 §3.15a:B3a-1 落地后逐 zone 执行 `cmd/migrate plan`(只允许这一条 ADD COLUMN)→ `up` → `SHOW COLUMNS` 核对 → 重启 go/db 与 scene;k8s 按 migrate → go/db → scene 顺序;合服前置检查也核这一列。验证命令已写进 §3.20。 |
| 2 | 主要 | 空着 15 用 16,列序漂移会让 `SELECT *` 静默串档 | **已采纳**(删行部分驳回) | 核实:`plan.go:151` 只 ADD COLUMN 不带 AFTER;`player_rows.go:285-289` 用 `SELECT *`。处理:本字段取落码时的下一个空闲号(现为 15)并声明在 message 末尾,写成加列纪律,B4a 同样适用(§3.15、§3.25-2);`copyOneTable` 改为按列名显式拷贝,列集合不一致就中止合服(§3.16)。**驳回**"deletePlayerRows 也改":它已按列名做 `d.col <=> s.col` 比较(player_rows.go:329-335)。 |
| 3 | 主要 | 既有 CreatePlayer 测试会挂,文件也不在清单 | **已采纳** | 核实:`createplayer_homezone_test.go:30-41` 嵌入的是 nil 接口,:86-90 没有 PlayerNames。已加进 B3a-2:fake 补上 Reserve/Release,harness 注入 `PlayerNames` 与 `loadNameRules`,并断言落盘名字和登记失败时的 Release 次数。`ErrUnavailable`(一个 RPC 都没发)不 Release、不计孤儿(§3.11,用例 10)。 |
| 4 | 主要 | 界面允许空名,违背"建角必填" | **已采纳** | 空名在客户端提示"请输入角色名",占位符同改;服务端生成名只给机器人和无 UI 路径;已加 EditMode 用例(§3.22)。 |
| 5 | 主要 | 2/12 等数值写死在代码里 | **已采纳**(有调整) | 新增单行配表 `RoleNameRule`,由 login 读取(§3.8a、§3.10)。调整三处:(a)schema 文件按仓库惯例用表名小写,叫 `rolenamerule_table.proto`;(b)客户端运行期不加载配表(map_tables 已核),所以客户端不写 2/12,改由 `kRoleNameInvalid` 的 `parameters=[min,max]` 回显,Tip 文案也不写数字;(c)32 字结构上限与 VARCHAR(64) 绑定,留在代码里。data_service 只做结构校验。 |
| 6 | 主要 | Reserve 超时后发的 Release 可能早于在途 INSERT 提交,留下永久孤儿 | **部分采纳** | (1)采纳并加强:login 先用同名重试一次(同 id 同名幂等,重试会等在途 INSERT 的锁),结果仍未知才条件 Release,立即发一次,约 10s 后再发一次(§3.11)。(2)**驳回**"服务端 ctx 超时后补偿 DELETE":它和 login 的幂等重试有竞态,重试可能刚拿到"成功",补偿就把在役角色的名字删了(§3.4)。(3)运维路径采纳:`ReleasePlayerName` 按 player_id+name 条件删除,带 x-admin-token 时不限登记时间,孤儿日志里带名字。**自动清扫驳回**:没有权威的"角色存在"信号(账号 blob 12h 过期后会被静默重建为空,反向映射写失败不阻断建角),已列未决。延迟 Release 用 fake 测(§3.19 用例 8)。 |
| 7 | 主要 | 响应丢失后重试建角,玩家会被告知自己的名字"已被使用" | **已采纳** | `ReservePlayerNameResponse` 增 `owner_player_id = 2`。占用者在本账号里、且职业和性别都相同时,按重试处理,返回全量列表(§3.11 第 6c' 步)。职业和性别条件是新加的,防止玩家故意建同名异职业角色时被悄悄进入老角色。客户端 diff 为空时取最后一个角色,已核(GameClient.cs:576-577)。有测试。 |
| 8 | 主要 | self-heal 恢复的记录永久丢名,backfill 里的 Lookup 分支走不到 | **已采纳** | 核实:entergamelogic.go:176-178 恢复出的记录只有 id。回源移到入场:找到的记录缺名、以及 self-heal,都 Lookup(fail-open)。backfill 内不再发 RPC,原来那个走不到的分支删掉。角色列表加 `fillMissingRoleNames`,一次批量查询,不改 `buildRoleList` 签名,既有测试不动(§3.12)。 |
| 9 | 主要 | 随机名只有 225 种,且最多 5 轮就踢回选服 | **已采纳** | 姓 80 个,名由池 A 60 字和池 B 60 字组成,共 292,800 种;字数与去重已用脚本核对。去掉轮数上限;服务端的可重试失败也留在建角页并保留输入;离开建角模式时清掉拒绝原因(§3.22)。测试改用单一随机流生成 10 万个名字、去重后不少于 2 万,没按评审写的"10 万个种子":相邻种子的 `System.Random` 首个输出相关,那样测不出真实分布。 |
| 10 | 次要 | 摘角色时释放名字,不可逆,且会漏放 | **已采纳**(方式不同) | 核实:账号 blob 过期时返回 0,反向映射却照删;输入是含老玩家的"候选"名单。处理:本期摘角色**完全不释放名字**,没有加 `release_names` 开关,省掉一次 proto 改动,也避免在候选名单上做不可逆操作。需要释放时运维带 token 调 Release(§3.16)。 |
| 11 | 次要 | 撞名会烧掉 id 并留下幽灵 `player:zone` 映射 | **已采纳** | 顺序改为 mint → reserve → register;注册失败就释放名字;撞名不再留下映射(§3.11,用例 3)。上一稿说"契约顺序是 register 在前",这个说法不对,已在 §3.25-7 更正。 |
| 12 | 次要 | `unicode.Han` 挡不住形近字,与客户端白名单也不一致 | **已采纳** | Go 与 C# 改用同一张显式区间表,扩展 B 及以后不开放;共享 JSON 向量,两仓库比对哈希。更正评审一处细节:U+3038–303A 会被 NFKC 折叠成 十卄卅,折叠后与正字 norm 相同,不构成冒名;真正需要挡的是 U+3005、U+3021–3029、U+303B、部首补充区。 |
| 13 | 次要 | 孤立代理字符让 `Normalize` 抛异常 | **已采纳** | 先扫描代理对,再用 try/catch 包住 Normalize;加孤立高位代理、孤立低位代理、截断代理对三个用例(§3.22)。 |
| 14 | 次要 | 锁 TTL 预算不够,账号 blob 写入没有围栏 | **已采纳** | 核实 login.yaml:35-36 Read/WriteTimeout 3s,最坏约 16s。TTL 改 20;写 blob 改 Lua 脚本,先校验锁令牌(`Lock.Key/Value` 已导出);锁丢失就释放名字,返回 `kLoginInProgress`;写入结果未知时回读,读不到则保留登记,宁可孤儿也不重名(§3.11 第 7 步)。 |
| 15 | 次要 | Release 无条件删除、无鉴权 | **已采纳** | `ReleasePlayerNameRequest` 增 `name = 2`,按 player_id + name_norm 条件删除;不带 token 时只能删 10 分钟内的登记;带 token 走 `authorizeAdmin`(§3.1、§3.6)。 |
| 16 | 次要 | 冒烟不可重复:固定账号里是 B3a 之前建的无名角色 | **已采纳** | 核实 guild_smoke.yaml:28、battle_smoke.yaml:15 用的是固定账号。端到端前先 DEL 这些账号的 `account:*` 键,或本机 dev 直接 FLUSHALL,不清 testdb。robot 失败文案写明原因和处理方法(§3.20、§3.21)。 |
| 17 | 次要 | 没做负载评估 | **已采纳** | 核实:data-service 部署 1 副本,MaxOpenConn 5(每个 store 已各开独立池,问题在池大小)。新增 `PlayerName` 配置段(连接池 20/5),data_service 耗时直方图,login 分阶段耗时,60s 负缓存(用 SET NX,盖不掉真名),stress_summarize 新增一段,并按 AGENTS §6.2 跑一轮 stress-200 做对比(§3.6a、§3.11、§3.20)。 |
| 18 | 次要 | 漏了若干读写路径 | **已采纳** | TransferPlayer 必须带上名字,登记到 §3.25-13;`FindPlayerByName` 列入未决;更正 guild.go 的说法:未配 DataServiceRpc 时只记 ERROR、继续起服,resolver 为 nil,名字一律为空(§3.21)。 |

未改的判断:全局注册表 + 唯一键、bootstrap DDL、login 先预检再发号、guild 读取 fail-open、Java 字段 5、robot vendor 步骤、`x/text` 依赖,评审均认可,保持不变。
