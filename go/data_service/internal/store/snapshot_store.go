package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"shared/idsegment"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// player_snapshot.source 列的取值契约。两种来源的 data 列格式**不兼容**:
//   - SnapshotSourceDataService(0):GM / data_service 自己写入,data 是 JSON
//     {fields: map[string][]byte}(Redis 字段图),snapshot_type 是 data_service 的 SnapshotType。
//   - SnapshotSourceSceneKafka(1):C++ scene 经 player_snapshot_topic 落库,data 是收到的
//     PlayerSnapshotEntry 原始 proto 字节(内含两个 player_database blob + schema_version),
//     snapshot_type 是 C++ 的 SnapshotTrigger 原值。统一两套枚举是以后的 proto 改动。
//
// 回滚 / diff / 列表等所有现有读路径在学会解 proto blob 之前必须只看 source=0,
// 否则 rollback_logic 的 json.Unmarshal 会在第一条 C++ 快照上炸掉今天能用的 GM 回滚。
const (
	SnapshotSourceDataService uint32 = 0
	SnapshotSourceSceneKafka  uint32 = 1

	// SnapshotOperatorSceneNode 是 source=1 行的 operator 固定值。
	SnapshotOperatorSceneNode = "scene-node"
)

// ErrNoRowIDSource 表示没接上行号来源:发号器没配好,或者装配漏了 UseRowIDs。
// 插入一律失败,不自造 id;快照消费者停在当前 offset 重试,不会丢。
var ErrNoRowIDSource = errors.New("snapshot store: no row id source")

// SnapshotRow represents a row in the player_snapshot table.
type SnapshotRow struct {
	ID           uint64
	PlayerID     uint64
	ZoneID       uint32
	SnapshotType uint32
	CreatedAt    uint64
	Reason       string
	Operator     string
	Data         []byte // source=0: JSON snapshotData; source=1: raw PlayerSnapshotEntry bytes
	SnapshotGuid uint64 // source=1: C++ SnapshotId (SnowFlake); 0 for GM rows
	Source       uint32 // SnapshotSourceDataService | SnapshotSourceSceneKafka
}

// AuditLogRow represents a row in the rollback_audit_log table.
type AuditLogRow struct {
	PlayerID              uint64
	ZoneID                uint32
	RollbackType          uint32 // 1=player, 2=zone, 3=server
	SnapshotIDUsed        uint64
	PreRollbackSnapshotID uint64
	TargetTime            uint64
	PlayersAffected       uint32
	PlayersFailed         uint32
	OrphansCleaned        uint32
	Reason                string
	Operator              string
	CreatedAt             uint64
}

const (
	// snapshotGuidInsertAttempts 是 InsertSnapshotIfGuidAbsent 对 InnoDB 死锁(1213)的就地尝试上限(含首次)。
	// 为什么是有界重试、为什么一定收敛,见 InsertSnapshotIfGuidAbsent 的「并发语义」;用尽后 1213 原样
	// 交给消费者的通用重试(kafka.SnapshotConsumer.insertWithRetry),不在这里无限转。
	snapshotGuidInsertAttempts = 3

	// snapshotGuidDeadlockBackoffMin / Max:两次尝试之间的等待取 [Min, Max] 内的均匀随机值。
	// 抖动是为了把两个刚互杀过的写者错开,不让它们在同一毫秒再同时拿同一段间隙锁;上限压在 50ms,
	// 单次调用就地最多多睡 (snapshotGuidInsertAttempts-1)×Max = 100ms,远小于消费者 1s 的通用退避,
	// 两层叠加不会把单条消息的最坏等待放大到超出消费者原有预算的量级。
	snapshotGuidDeadlockBackoffMin = 10 * time.Millisecond
	snapshotGuidDeadlockBackoffMax = 50 * time.Millisecond
)

// insertSnapshotSQL 是落一行快照的普通单行 INSERT。GM 路径(InsertSnapshot)用它;
// 按 guid 去重的唯一键路径在它后面接一段 ODKU(insertSnapshotOnDuplicateKeepSQL)——
// 两者插的是同一张表的同一组列,各写一份只会在加列时漏掉一处。
// 抽成常量还让真库回归用例能用**同一段文本**扮演并发写者。
// VALUES 后面那一组占位符单独成常量(snapshotValuesTupleSQL),批量语句按行数重复它;拼出来的文本与拆分前逐字相同。
const insertSnapshotSQL = `INSERT INTO player_snapshot
	 (id, player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source)
	 VALUES ` + snapshotValuesTupleSQL

// snapshotValuesTupleSQL 是一行的十个占位符,顺序 = snapshotInsertArgs。单行与多行(批量)语句共用这一段,
// 列清单只在 insertSnapshotSQL 里写一次。
const snapshotValuesTupleSQL = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// onDuplicateKeepIDSQL 是唯一键形态**单行**语句的 ODKU 尾巴:撞 uk_snapshot_guid_nz 时一列不改,只把已存在那行的 id 回带。
// 取舍见 insertSnapshotOnDuplicateKeepSQL 的注释。多行语句**不用 ODKU**,见 InsertSnapshotsIfGuidAbsent 的「为什么不是多行 ODKU」。
const onDuplicateKeepIDSQL = "\n\t ON DUPLICATE KEY UPDATE `id` = LAST_INSERT_ID(`id`)"

// insertSnapshotOnDuplicateKeepSQL 是按 guid 去重的**唯一键**形态:撞 uk_snapshot_guid_nz 时一列都不改,
// 只把已存在那行的 id 回带给调用方。
//
// # 为什么是 ODKU,而不是"普通 INSERT + 撞 1062 再点查"
//
// 普通 INSERT 的重复键检查取的是 **S 锁**,那正是"同 guid 多个后到者排在先到者后面、先到者回滚"
// 那个死锁环的成因(见 InsertSnapshotIfGuidAbsent 的「哪一类死锁」)。ODKU 让同一次检查直接取 **X**,
// 后到者彼此串行,环不再成立。判据是 MySQL 手册 "Locks Set by Different SQL Statements in InnoDB":
//
//	"INSERT ... ON DUPLICATE KEY UPDATE differs from a simple INSERT in that an exclusive lock
//	 rather than a shared lock is placed on the row to be updated when a duplicate-key error occurs."
//
// 手册紧接着那段"三会话 + ROLLBACK"的死锁例子("sessions 2 and 3 ... both request a shared lock ...
// At this point, sessions 2 and 3 deadlock"),描述的就是普通 INSERT 的 S 锁形态 —— 换成 ODKU 即消失。
//
// # `id = LAST_INSERT_ID(id)` 为什么不算"一次真实修改"
//
// 这一句同时做两件事,都不改数据:
//   - **值不变**。手册 "INSERT ... ON DUPLICATE KEY UPDATE Statement" 写明 affected-rows
//     "0 if an existing row is set to its current values" —— 没有行更新,快照仍是不可变记录。
//     (前提:连接**没有**打开 CLIENT_FOUND_ROWS,否则这里会变成 1,与"新插入"混淆。
//     本包的 DSN 由 buildDSN 独家给出、不带 clientFoundRows,单测 TestSnapshotDSNKeepsAffectedRowsSemantics 钉住。)
//   - LAST_INSERT_ID(expr) 把已存在那行的 id 记进会话并由 OK 包回带,调用方一次往返就拿到 id,
//     不必再点查一次(手册 mysql_insert_id():"or have used INSERT or UPDATE to set a column value
//     with LAST_INSERT_ID(expr)")。手册没有明说"值未变时 OK 包一定回带",所以拿到 0 时仍退回一次
//     按唯一键的等值点查 —— 两条路结果相同,只差一次往返。
//
// # 代价(都不是正确性问题,记在这里免得下次又被当成"不能用 ODKU"的理由)
//
//   - 每次重放都白发一个行号(发号在语句之前),id 因此有空洞 —— 它本来就只是行号,
//     打散、不连续、没有任何业务含义,63 位也用不完。
//   - 撞键时取的是唯一键上的 **next-key X**(手册:"An exclusive next-key lock is taken for a
//     duplicate unique key value"),范围仅限同一个 guid。没撞键的常态路径与普通 INSERT 完全一样,
//     只取插入意向锁,所以不同 guid 的写者仍然互不相干(回归 TestSnapshotStore_DistinctGuidsNeverDeadlock)。
const insertSnapshotOnDuplicateKeepSQL = insertSnapshotSQL + onDuplicateKeepIDSQL

// MaxSnapshotBatchRows 是一条多行快照语句的行数硬上限(InsertSnapshotsIfGuidAbsent 拒绝更多)。
// 10×256 = 2560 个占位符,远低于预处理语句 65535 的上限;与 config.MaxSnapshotBatchMaxRows 相等,单测钉住。
const MaxSnapshotBatchRows = 256

// insertSnapshotsSQL 拼 n(>=1)行的**普通**多行 INSERT(不带 ODKU / IGNORE);n==1 时与 insertSnapshotSQL 逐字相同。
// 每行一组 snapshotValuesTupleSQL,实参 = 各行 snapshotInsertArgs 依次拼接。为什么不用 ODKU 见 InsertSnapshotsIfGuidAbsent。
func insertSnapshotsSQL(n int) string {
	var b strings.Builder
	b.Grow(len(insertSnapshotSQL) + (n-1)*(len(snapshotValuesTupleSQL)+4))
	b.WriteString(insertSnapshotSQL)
	for i := 1; i < n; i++ {
		b.WriteString(",\n\t ")
		b.WriteString(snapshotValuesTupleSQL)
	}
	return b.String()
}

// existingSnapshotGuidsSQL 拼批量预读:按唯一键 uk_snapshot_guid_nz 的等值 IN 列表找出库里已有的 guid。
// **普通读**(自动提交的一致性读,不加任何锁),只用来把确定已存在的行剔出多行 INSERT;并发窗口由 INSERT 的 1062 兜住。
func existingSnapshotGuidsSQL(n int) string {
	return "SELECT snapshot_guid_nz FROM player_snapshot WHERE snapshot_guid_nz IN (" +
		strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}

// insertSnapshotIfGuidAbsentLegacySQL 是唯一键**还不存在**时的去重写法(审计 #11 第二步之前的生产路径)。
//
// 现在它只用于 MySQL 尚未迁移或刚刚回滚 DDL 的兼容窗口，由建 store 时的一次探测决定。
// TiDB 没有 RR 间隙锁，缺少真实唯一约束时直接拒绝建池，不走此退路。
// 它的去重靠 REPEATABLE READ 下 INSERT ... SELECT 源端的**加锁读**,代价是并发写者会在
// idx(snapshot_guid) 的同一段间隙上互等插入意向锁而成环 —— 那正是唯一键要消掉的东西。
const insertSnapshotIfGuidAbsentLegacySQL = `INSERT INTO player_snapshot
	 (id, player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source)
	 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
	 FROM DUAL
	 WHERE NOT EXISTS (SELECT 1 FROM player_snapshot AS existing WHERE existing.snapshot_guid = ?)`

const (
	// lookupSnapshotIDByGuidNzSQL 按唯一键 uk_snapshot_guid_nz 点查已存在那行的 id。
	// 完整唯一键等值点查:key=uk_snapshot_guid_nz、key_len 用满(bigint unsigned 可空 = 8+1),
	// 只锁(读)这一条索引项,不带范围。真库回归见 TestSnapshotStore_GuidLookupUsesUniqueKey。
	lookupSnapshotIDByGuidNzSQL = `SELECT id FROM player_snapshot WHERE snapshot_guid_nz = ?`

	// lookupSnapshotIDByGuidSQL 是唯一键不存在时的退化形态:走普通 idx(snapshot_guid),
	// 同一 guid 理论上只有一行,LIMIT 1 只是明确"取一行就够"。
	lookupSnapshotIDByGuidSQL = `SELECT id FROM player_snapshot WHERE snapshot_guid = ? LIMIT 1`
)

// uniqueGuidKeyProbeTimeout 是建 store 时探测唯一键的时限。
// 探测包含版本与 INFORMATION_SCHEMA 只读查询；TiDB 超时或缺约束拒启，MySQL 保留旧兼容路径。
const uniqueGuidKeyProbeTimeout = 5 * time.Second

// SnapshotStore provides CRUD operations for player snapshots and audit logs.
// 表结构由 MigrateSchema 按 proto 建 + schema.go 的 nullableUniqueKeys 追加可空唯一列,
// 本 store 不碰 DDL,只在建 store 时**读一次**结构来决定用哪条去重语句。
type SnapshotStore struct {
	db *sql.DB

	// uniqueGuidKey = 建 store 时探到 player_snapshot 上有 uk_snapshot_guid_nz,
	// **且**它只覆盖完整的 snapshot_guid_nz 列，列符合对应方言的可空 uint64 生成表达式。
	//
	// 为什么要探、而不是直接假设它在:这个键是一次独立的 DDL 迁移(审计 #11 第二步),
	// 而“迁移还没跑 / 刚回滚”都可能发生。假设它在的后果是最坏的一种 ——
	// 普通 INSERT 不会撞 1062,于是同一个 guid 静默落成两行,零报错。探一次的代价是启动期一条点查。
	//
	// 探测结果只读一次、之后不再变:运行期不接受"DDL 半路被加上/删掉"。真发生了(人工跑迁移 /
	// 人工 DROP),重启 data_service 即重新探测 —— 这比每次写入都去问一遍库便宜得多,
	// 也不会让写路径的行为在同一进程里跳来跳去、把日志变成猜谜。
	uniqueGuidKey bool

	// deadlockRetries 累计 InsertSnapshotIfGuidAbsent 因 1213 就地重跑语句的次数。
	// 只给日志与真库回归用例用:用例要证明"死锁确实撞上了、并且被就地重跑吸收",
	// 否则"最终成功"分不清是重试生效了,还是这一轮根本没撞上。
	deadlockRetries atomic.Uint64
	// batchFallbacks 是批量路径的普通多行 INSERT 撞 1062、整批转逐行的累计次数(InsertSnapshotsIfGuidAbsent)。
	// 常态(无并发重叠)必须为 0:不为 0 而又没有 rebalance,说明预读没起作用。
	batchFallbacks atomic.Uint64

	// snapshotRowIDs、auditRowIDs 发两张表的主键,由 UseRowIDs 接上。
	// 行号只回答「是哪一行」:打散、不连续、不代表先后,排序一律看 created_at。
	snapshotRowIDs idsegment.Source
	auditRowIDs    idsegment.Source
}

// NewSnapshotStore opens the pool. It does NOT create tables — see schema.go.
// 建池之后探一次 uk_snapshot_guid_nz,决定 InsertSnapshotIfGuidAbsent 用哪条语句去重。
func NewSnapshotStore(cfg MySQLConfig) (*SnapshotStore, error) {
	db, err := openMySQL(cfg)
	if err != nil {
		return nil, err
	}
	logx.Infof("[SnapshotStore] connected to %s/%s", cfg.Host, cfg.DBName)

	s := &SnapshotStore{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), uniqueGuidKeyProbeTimeout)
	defer cancel()
	key, isTiDB, err := nullableUniqueKeyForServer(ctx, db, snapshotGuidNullableKey)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("snapshot GUID 方言探测失败: %w", err)
	}
	// 与迁移使用同一判据：索引恰好覆盖生成列全长，列形态、表达式与 uint64/NULL 语义一致。
	// MySQL 的 NOT EXISTS 退路依赖 RR 间隙锁；TiDB 不具备此前提，不能把探测失败当成功建池。
	hasKey, err := hasNullableUniqueIndex(ctx, db, cfg.DBName, key)
	if err == nil && hasKey {
		var hasCol bool
		hasCol, err = hasGeneratedColumn(ctx, db, cfg.DBName, key.table, key.column, key.columnExpr, key.columnStorage)
		if err == nil && !hasCol {
			err = fmt.Errorf("唯一键 %s 在,但它约束的列 %s 不存在(键在、列坏的中间态)",
				SnapshotGuidUniqueKey, SnapshotGuidNzColumn)
		}
		if err != nil {
			hasKey = false
		}
	}
	if isTiDB && (err != nil || !hasKey) {
		_ = db.Close()
		if err == nil {
			err = fmt.Errorf("唯一约束 %s 缺失", SnapshotGuidUniqueKey)
		}
		return nil, fmt.Errorf("TiDB 快照 GUID 唯一约束不可用，拒绝启用写池；先停写并执行正式 -migrate: %w", err)
	}
	if err != nil {
		logx.Errorf("[SnapshotStore] Failed to inspect %s.%s (%v): treating unique key as absent; snapshot deduplication falling back to "+
			"INSERT...SELECT...NOT EXISTS (functionally correct, but concurrent writers form gap-lock cycles on idx(snapshot_guid), handled by bounded 1213 retries). "+
			"After confirming migration ran (data_service -f <yaml> -migrate), restart this service to restore the unique-key path",
			PlayerSnapshotTableName, SnapshotGuidUniqueKey, err)
	}
	s.uniqueGuidKey = hasKey
	if hasKey {
		logx.Infof("[SnapshotStore] %s.%s present: snapshot guid deduplication uses INSERT ... ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id) "+
			"(key conflicts acquire X rather than S locks, eliminating cycles among waiters for the same guid)",
			PlayerSnapshotTableName, SnapshotGuidUniqueKey)
	} else if err == nil {
		logx.Errorf("[SnapshotStore] %s.%s absent: snapshot deduplication falls back to INSERT...SELECT...NOT EXISTS. "+
			"On MySQL, run data_service -f <yaml> -migrate to create the key (additive only), then restart this service to switch to ODKU "+
			"and eliminate the fallback statement's gap-lock cycle on idx(snapshot_guid)",
			PlayerSnapshotTableName, SnapshotGuidUniqueKey)
	}
	return s, nil
}

// UseRowIDs 接上两张表的行号来源。必须在第一次写入之前调用,之后只读。
// 只读工具(cmd/debugutil)用不到行号,所以不放进 NewSnapshotStore 的参数。
func (s *SnapshotStore) UseRowIDs(snapshots, audits idsegment.Source) {
	s.snapshotRowIDs = snapshots
	s.auditRowIDs = audits
}

// nextRowID 发一个行号。发不出来这次写入就失败,不用 0 或自造的值顶替。
func nextRowID(ctx context.Context, source idsegment.Source) (uint64, error) {
	if source == nil {
		return 0, ErrNoRowIDSource
	}
	return source.Next(ctx)
}

// snapshotInsertArgs 按 insertSnapshotSQL 的占位符顺序展开一行,id 是这一行的行号。
// 两条 INSERT(普通 / NOT EXISTS)的前十个占位符完全一致,顺序只在这里定义一次。
func snapshotInsertArgs(id uint64, row *SnapshotRow) []any {
	return []any{id, row.PlayerID, row.ZoneID, row.SnapshotType, row.CreatedAt, row.Reason, row.Operator, row.Data,
		row.SnapshotGuid, row.Source}
}

// guidDedupeStatement 按探到的结构给出这次写入要用的语句与实参。
func (s *SnapshotStore) guidDedupeStatement(id uint64, row *SnapshotRow) (string, []any) {
	if s.uniqueGuidKey {
		return insertSnapshotOnDuplicateKeepSQL, snapshotInsertArgs(id, row)
	}
	// NOT EXISTS 形态多一个占位符:子查询里再绑一次同一个 guid。
	return insertSnapshotIfGuidAbsentLegacySQL, append(snapshotInsertArgs(id, row), row.SnapshotGuid)
}

// guidLookupStatement 按探到的结构给出"查这个 guid 已存在那行 id"的语句。
func (s *SnapshotStore) guidLookupStatement() string {
	if s.uniqueGuidKey {
		return lookupSnapshotIDByGuidNzSQL
	}
	return lookupSnapshotIDByGuidSQL
}

// Close releases the database connection.
func (s *SnapshotStore) Close() error {
	return s.db.Close()
}

// InsertSnapshot saves a snapshot and returns its row ID.
// GM 路径调用方不设 Source / SnapshotGuid,即落成 source=0、guid=0。
func (s *SnapshotStore) InsertSnapshot(ctx context.Context, row *SnapshotRow) (uint64, error) {
	id, err := nextRowID(ctx, s.snapshotRowIDs)
	if err != nil {
		return 0, fmt.Errorf("mint snapshot row id: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, insertSnapshotSQL, snapshotInsertArgs(id, row)...); err != nil {
		return 0, err
	}
	return id, nil
}

// InsertSnapshotIfGuidAbsent 按 snapshot_guid 去重落库,返回 inserted=false 时
// id 是已存在那行的行号。
//
// # 去重由库上的可空唯一键兜住
//
// 事实源是 player_snapshot 上把零 GUID 映射为 NULL、保留非零值的可空生成列及其唯一键，
// 与建在它上面的 uk_snapshot_guid_nz(DDL 与回滚方案见 schema.go 的 nullableUniqueKeys)。
// GM 行 guid 恒为 0 → 这一列是 NULL → 唯一键不约束它们,任意多条 GM 行照样能落;
// source=1 的 C++ 快照 guid 非 0,按 guid 严格唯一。
//
// 于是写入是一条 **INSERT ... ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)**
// (语句形态与取舍见 insertSnapshotOnDuplicateKeepSQL):affected=1 = 首次落库;affected=0 =
// 这个 guid 已经在库里、一列都没改,OK 包回带的就是已存在那行的 id,返回 inserted=false。
// 拆成"先 SELECT 再 INSERT"的老写法在两次往返之间没有任何约束,滚动更新窗口里(或本地多开)
// 两个消费者实例足以把同一条快照写成两行。
//
// MySQL 唯一键不存在时(迁移还没跑、DDL 刚回滚)退回 insertSnapshotIfGuidAbsentLegacySQL，
// 由建 store 时的一次探测选定(见 SnapshotStore.uniqueGuidKey)。两条语句的返回语义完全一致,
// 调用方看不出区别。
//
// # 哪一类死锁被唯一键消掉了,哪一类只能靠重试(MySQL/InnoDB,REPEATABLE READ)
//
// 本 store 的 DSN 不设隔离级别,走库的全局默认 RR。
//
// **被消掉的那一类:两个写者在同一段间隙上互等插入意向锁。**
// 老写法的 NOT EXISTS 子查询在 RR 下是**加锁读**:guid 不存在时锁住它所在的整段间隙。
// snapshot_guid 是 SnowFlake、单调递增,新 guid 几乎总比表里所有 guid 都大,那段间隙几乎总是
// (max_guid, +∞)。于是任意两个并发写者 —— **同 guid 或不同 guid 都一样** —— 都会:
// 各拿到同一段间隙上的 S 锁(间隙 S 锁彼此兼容)→ 各自申请插入意向锁 → 各被对方的 S 间隙锁挡住
// → 成环,一方收到 1213。也就是说,连"两个不同 guid 的写者"都会互杀,而它们本来毫无冲突。
// 普通 INSERT 不做加锁读:重复键检查在唯一键上进行,查不到就只在目标间隙上取插入意向锁,
// 而插入意向锁之间是**相容**的。所以不同 guid 的并发写者现在互不相干,这条环随之消失。
// 回归:TestSnapshotStore_DistinctGuidsNeverDeadlock 断言就地重跑计数一次都不涨。
//
// **没有被消掉、只能靠有界重试吸收的那一类:先到者回滚,排在它后面的两个后到者互等。**
//  1. A 插 guid=G 未提交 —— 在唯一键上持有这条新索引记录的 X 锁;
//  2. B、C 各插 guid=G,重复键检查撞上 A 那条未提交记录,两人一起排队等 A;
//  3. A 回滚(被别处的死锁选为牺牲者、ctx 取消断连、或纯粹是业务失败)——
//     B、C 等待中的锁被**继承成间隙锁**;
//  4. 两人随后各自申请插入意向锁,被对方继承来的间隙锁挡住 → 成环,InnoDB 牺牲其一。
//
// **ODKU 拆不掉这一类**,这是 2026-09-29 对着真 MySQL 26.7.0 做的受控实验结论(每格 10 次重复):
// 首个插入者回滚、两个后到者排队的现场,**10/10 全部成环**,且与写法、与键的位置都无关 ——
// 普通 INSERT / INSERT IGNORE / ODKU 三种写法,聚簇主键 / 二级唯一索引两种位置,六格全是 10/10。
// 成环的是第 3 步的**锁继承**,不是第 2 步取 S 还是取 X。
// 要真正消掉它只有一条路:别让两个插入者同时排在同一条未提交记录后面 —— 让所有首次插入者先排在
// 一行**已提交**的守卫记录上(trade 的哨兵行就是这么做的,见 go/trade/internal/data/asset_op_repo.go)。
// 本表没有这样做:同一个 guid 要有三个写者同时在场才成环,而 data-service 虽是多副本,每个分区同一时刻只属于
// 一个实例(consumer group),同 guid 的并发写者只在 rebalance 重叠的瞬间出现。
// 频率低、且被下面的有界重试完整吸收(结局仍是"恰好一行、两人回出同一个 id")。
// 回归:TestSnapshotStore_SameGuidQueueDeadlockIsAbsorbed。
//
// (两次翻车都记在这里:先是有注释写"不能用 ODKU,因为 UPDATE id = id 要写一次 undo" —— 错,
// 手册明写 affected-rows "0 if an existing row is set to its current values";随后第四轮复审按手册
// 推演出"ODKU 取 X 就能消掉这个环",把用例断言改成 retries == 0 —— 也错,真库一跑就红。
// 教训:手册那句 X/S 的区别管的是**删除标记记录上的 S→X 升级**那一类,不是回滚继承这一类。)
//
// # 有界重试为什么还留着,以及它的收敛论证
//
// 留着是为了 MySQL 的退路语句：唯一键不存在时(迁移未跑 / DDL 刚回滚)走
// insertSnapshotIfGuidAbsentLegacySQL,它的间隙环真实存在且 SQL 层去不掉
// (回归 TestSnapshotStore_LegacyGapDeadlockIsAbsorbedInPlace)。唯一键路径上它只是纯兜底:
// 按上面的推演不该再撞 1213,真撞上了说明推演与实现对不上,日志会明说(见下面 reruns>0 的分支)。
//
// 对 1213 **就地**有界重跑同一条语句(最多 snapshotGuidInsertAttempts 次,间隔 10–50ms 抖动,
// ctx 可取消),不让它冒泡成消费者 1s 一次的通用重试和一条 ERROR:
//   - 被牺牲方的语句已整条回滚、锁全部放掉,胜者随即拿到插入意向锁,语句完成并自动提交
//     (本路径全程自动提交,不存在"胜者迟迟不提交"的形态)。
//   - 重跑时这个 guid 已经在唯一键里(胜者插的),重复键检查撞上一条**已提交**记录:
//     唯一键形态由 ODKU 就地判为重放(affected=0,回出已有 id),退路形态的 NOT EXISTS 判假(affected=0)
//     或撞 1062,两者都走下面"查已存在 id"的分支,返回 inserted=false。一次重跑即收敛。
//   - 每成一次环都至少有一方提交,整体一直在前进,不存在活锁。
//   - 单条消息连输 snapshotGuidInsertAttempts 次时,1213 交给消费者按 1s 间隔再试
//     (最多 DBMaxAttempts 次),1s 的错峰足以把写者的节奏打散;消费者最终放弃也只是停在当前
//     offset 不提交(宁可滞后不丢),既不丢也不重。
//   - 能并发的写者本来就只有"短时两三个"这个量级:data-service 是多副本,但两条消费者是 consumer group,
//     分区在实例间独占分配(deploy/k8s/manifests/go-svc/data-service.yaml),同一个 guid 只在 rebalance
//     重叠(发布 / 崩溃 / DB 故障后 reader 重建)的瞬间才会有并发写者,也才会看到本函数"就地吸收了 1213"的日志。
//
// 为什么只就地重试 1213、不重试 1205(锁等待超时):1213 在环形成的那一刻就报出,就地重跑的代价是几十
// 毫秒;1205 则说明本条语句已经白等了一整个 innodb_lock_wait_timeout(默认 50s,多半是被别的长事务压住),
// 就地再等 snapshotGuidInsertAttempts 次,会把消费者单条消息的最坏等待从 DBMaxAttempts×50s 放大到
// DBMaxAttempts×150s。1205 按原样交给消费者的 1s 重试。关掉 innodb_deadlock_detect 的库上,死锁会以 1205
// 的形式出现,同样走消费者重试。
//
// # 退回 NOT EXISTS 形态时的两条约束
//
//   - **不要**把那条语句或 DSN 改成 READ COMMITTED 来"消掉间隙锁":RC 下子查询是一致性读、不加任何锁,
//     两个并发写者会同时判定"不存在"并各插一行,去重直接失效。
//   - 同理它在没有间隙锁的库上(TiDB)本就不成立。所以 TiDB 上必须先把可空唯一约束落下来,
//     不能靠这条退路。迁移和建池都会在唯一约束缺失或不符时拒绝继续，不提供 TiDB 绕过开关。
//     所以运行期真的走到这条退路,只有"MySQL 上迁移还没跑 / DDL 刚回滚"两种局面。
//
// idx(snapshot_guid) 由 store.MigrateSchema 的 ensureIndexes 保证存在;唯一键路径不依赖它,
// 但 GM 列表 / 去重迁移仍要用,缺了会退化成全表扫。
func (s *SnapshotStore) InsertSnapshotIfGuidAbsent(ctx context.Context, row *SnapshotRow) (id uint64, inserted bool, err error) {
	if row.SnapshotGuid == 0 {
		return 0, false, fmt.Errorf("snapshot_guid must be non-zero for check-then-insert")
	}

	// 就地重跑 1213 用同一个行号:牺牲方的语句已整条回滚,这个行号并没有落库。
	rowID, err := nextRowID(ctx, s.snapshotRowIDs)
	if err != nil {
		return 0, false, fmt.Errorf("mint row id for snapshot guid %d: %w", row.SnapshotGuid, err)
	}
	query, args := s.guidDedupeStatement(rowID, row)
	var res sql.Result
	reruns, err := retryOnDeadlock(ctx, snapshotGuidInsertAttempts, snapshotGuidDeadlockBackoff, func() error {
		var execErr error
		res, execErr = s.db.ExecContext(ctx, query, args...)
		return execErr
	})
	if reruns > 0 {
		s.deadlockRetries.Add(uint64(reruns))
		// 两条路的 1213 成因完全不同,给运维的动作也不同 —— 日志必须分开说,
		// 否则退路上那个**可以修掉**的环会被照着"InnoDB 固有、忍了"的结论放过。
		if s.uniqueGuidKey {
			logx.Infof("[SnapshotStore] snapshot guid=%d retried %d times on the unique-key path after InnoDB deadlock (1213), result err=%v: "+
				"an expected inherent case -- two writers for the same guid queue behind a third uncommitted writer that then rolls back, "+
				"causing waiting locks to be inherited as gap locks that mutually wait on insert intention locks (observed regardless of statement form; ODKU cannot eliminate it, see InsertSnapshotIfGuidAbsent). "+
				"Retries preserve correctness. This service has multiple replicas; two instances only write the same guid simultaneously during consumer-group rebalance overlap, "+
				"so occasional occurrences are normal; persistent high frequency warrants checking for repeated consumer-group rebalance (membership churn / reader recreation after DB failures)",
				row.SnapshotGuid, reruns, err)
		} else {
			logx.Errorf("[SnapshotStore] snapshot guid=%d encountered InnoDB deadlock (1213) on the fallback path (INSERT...SELECT...NOT EXISTS), "+
				"retried %d times, result err=%v: this is an idx(snapshot_guid) gap-lock cycle -- the subquery is a locking read under RR, "+
				"even writers with different guids can deadlock; this is not contention for the same snapshot. It can be eliminated entirely: "+
				"run data_service -f <yaml> -migrate to create %s, then restart this service to switch writes to ODKU",
				row.SnapshotGuid, reruns, err, SnapshotGuidUniqueKey)
		}
	}
	if err != nil {
		if isDupEntryMySQL(err) && !s.uniqueGuidKey {
			// 退路形态的**正常**去重结果:表上已经有唯一键(滚动升级窗口 / 迁移刚跑完但本进程是探测之前起的),
			// 而 NOT EXISTS 与插入之间被别的写者抢先。按 guid 点查出它的 id,返回 inserted=false。
			return s.lookupSnapshotIDByGuid(ctx, row.SnapshotGuid, "退路形态撞 1062")
		}
		// 唯一键形态的 1062 **不是**去重结果,一律当故障往上报(当成"已经落过了"会让消费者提交 offset 丢掉快照)。
		// 注意 ODKU 会就地吸收**任何**唯一键上的重复,不只 uk_snapshot_guid_nz:若表上多出别的唯一键,撞它时
		// 这里拿不到 1062,而是 affected=0、LAST_INSERT_ID 回带那一行的 id,这条快照被当成已存在静默丢弃
		// (2026-10-16 真库实测)。所以 player_snapshot 只许有 PRIMARY 与 uk_snapshot_guid_nz 两个唯一索引,
		// 由 TestSnapshotTableUniqueIndexesAreOnlyGuidKey 守住;这里的 1062 只剩异常形态。
		// 就地预算用尽的 1213 同样走这里:原样交给调用方(消费者)的通用重试,由它负责错峰与最终放弃。
		return 0, false, fmt.Errorf("insert snapshot guid %d: %w", row.SnapshotGuid, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot guid %d rows affected: %w", row.SnapshotGuid, err)
	}
	if affected == 1 {
		// 恰好 1 行 = 真的插进去了。两种形态都一样,且都不可能用 1 表示"重放":
		// ODKU 的重放是 0("set to its current values"),NOT EXISTS 的重放也是 0(子查询判假)。
		// 这个判据依赖连接没有打开 CLIENT_FOUND_ROWS —— 见 insertSnapshotOnDuplicateKeepSQL 的注释与
		// TestSnapshotDSNKeepsAffectedRowsSemantics。
		return rowID, true, nil
	}

	// 到这里 = 这个 guid 已经落过了(ODKU 判定值未变 affected=0,或 NOT EXISTS 子查询判假 affected=0)。
	if s.uniqueGuidKey {
		// ODKU 的 `id = LAST_INSERT_ID(id)` 已经把已存在那行的 id 记进会话并由 OK 包回带,
		// 常态下一次往返就够。手册没有明说"值未变时一定回带",拿到 0 时退回一次唯一键点查。
		if existingID, err := res.LastInsertId(); err == nil && existingID > 0 {
			if uint64(existingID) == rowID {
				// 撞的是主键不是 guid,只有发号器发重了才会走到这里。ODKU 同样吸收成 affected=0,
				// 当成「已存在」就会静默丢掉这条快照。报错让调用方重试,重试时换一个行号。
				return 0, false, fmt.Errorf("snapshot guid %d: row id %d already belongs to another row, the id issuer issued a duplicate", row.SnapshotGuid, rowID)
			}
			return uint64(existingID), false, nil
		}
	}
	return s.lookupSnapshotIDByGuid(ctx, row.SnapshotGuid, "写入语句判定这个 guid 已存在")
}

// InsertSnapshotsIfGuidAbsent 按 snapshot_guid 去重批量落库,返回本次真正新插入的行数。
// 调用方是 player_snapshot_topic 消费者的批量路径(G2a 快照审计管道扩容;批大小由 Kafka.SnapshotBatchMaxRows 控制,默认 1 = 不走这里)。
//
// 语义与逐行调用 InsertSnapshotIfGuidAbsent 相同(每个 guid 恰好一行、批内重复时先出现即 offset 小的那条落库、
// 返回值 = 真正新插入的行数),只是常态下一次预读 + 一条多行语句:
//   - 唯一键形态:① 普通读(existingSnapshotGuidsSQL,不加锁)找出库里已有的 guid;② 剔掉它们与批内的后续重复,
//     剩下的行按 snapshot_guid 升序拼**一条普通多行 INSERT**(insertSnapshotsSQL);③ 这条语句撞 1062:表上只有
//     uk_snapshot_guid_nz 一个二级唯一键(TestSnapshotTableUniqueIndexesAreOnlyGuidKey 守住),所以只会是并发写者在①之后
//     落了同一个 guid(rebalance 重叠);整条已自动回滚,改为对剩下的行逐行调 InsertSnapshotIfGuidAbsent(batchFallbacks 计数)。
//   - 退路形态(唯一键不在):逐行调 InsertSnapshotIfGuidAbsent。不把 NOT EXISTS 拼成多行:它的去重靠 RR 加锁读,
//     拼成一条会把间隙锁范围扩到整批,成环更多;退路只在迁移未跑 / 回滚窗口出现,不值得为它做批量。
//   - 不回带每行 id:消费者只拿 id 打 duplicate 日志,批量路径改为每批一条汇总日志。
//
// # 为什么不是多行 ODKU(死锁审计 #20,docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md §9.6)
//
// 多行 ODKU 里只要有一行撞到**已存在**的记录,InnoDB 会先在自增主键末尾试插那一行、回滚后把锁继承成
// PRIMARY supremum 上的 X 间隙锁,并且**一直持有到整条语句结束** —— 之后同一条语句再在唯一键上等别人的未提交行,
// 而那个写者要在主键末尾插入、被这把间隙锁挡住,就成环。2026-10-16 真库:按 offset 序 3/3、按 guid 升序也 3/3。
// (多行 ODKU 给自增列赋值还会在批内重复时报 1869,这是另一个问题,见 #19。)
// 这些都是主键还自增时测的。行号打散之后试插不再都落在末尾,撞上的概率变小,但这把间隙锁和这个环都还在,所以仍不用多行 ODKU。
// 现在的写法拆掉了它:普通 INSERT 撞到**已提交**的重复会立刻 1062、整条自动回滚,不会带着间隙锁继续往下。
// ①的预读把「新属主重读旧属主已落库的行」这一常见重叠直接剔掉,1062 → 逐行只剩①与②之间那一瞬的竞态。
//
// # 为什么要按 guid 升序插(第二个环,复核员真库实测)
//
// 普通 INSERT 撞到**未提交**的重复 x 时,它申请的 S next-key 锁在**等待中**也会罩住 x 下方的间隙 (pred(x), x),
// InnoDB 判断插入意向时把排队的请求也算进去。于是持有 x 的领先写者 A 往后插的行只要落进这个间隙,就会被等着 A 的
// 落后写者 B 挡住 —— A 等 B、B 等 A,两个写者就成环,不需要回滚、不需要第三方。按 offset 序拼批时 guid 并不单调
// (同一秒里不同 scene 节点的快照到达序与 guid 序无关),所以批内倒序是常态:两写者 [5000, 6000..6149, 4999] 10/10 成环,
// 流式重叠压测 7/279 条批量语句 1213,就地 3 次重试还会被连续耗尽。剔重之后按 snapshot_guid 升序拼(逐行回退同序),
// 领先写者之后的行都大于 x、落不进 x 下方的间隙,同组实验全部 0(0/10、0/10、0/6、0/273)。
// 这样排序没有别的代价:行号是打散的,同一秒内的快照在 ListSceneSnapshotsByPlayer(`ORDER BY created_at DESC, id DESC`)
// 里本来就没有先后含义。
//
// 残余属于单行路径已有的那一类:同一条未提交记录后面排着 ≥2 个等待者(即 ≥3 个写者同写一个 guid)、且第一个插入者
// 回滚时,等待者继承的间隙锁互挡(§9.1 现场二),由 retryOnDeadlock 有界吸收(#11 已登记;复核员实测 6/6,与声明一致)。
// 不同分区的 guid 不相交,但别的分区的排队请求同样可能让插入短暂等待;按升序后只剩有界的单向等待,不成环。
//
// 原子性:多行 INSERT 是单条自动提交语句,要么整批落、要么整批不落,就地重跑安全。走到逐行(退路形态、1062 回退)
// 中途出错时返回已落行数与 error:消费者出错即丢弃计数、整批重试,已落的行在重试时被预读 / 判重剔掉,数据不丢,
// 只是这些行在 inserted / duplicate 指标里会被记成 duplicate。
// 「批太大」类错误(IsSnapshotBatchTooLarge)原样返回,由消费者降级为逐条落库,不会卡住分区。
func (s *SnapshotStore) InsertSnapshotsIfGuidAbsent(ctx context.Context, rows []*SnapshotRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	if len(rows) > MaxSnapshotBatchRows {
		return 0, fmt.Errorf("snapshot batch of %d rows exceeds MaxSnapshotBatchRows=%d", len(rows), MaxSnapshotBatchRows)
	}
	for _, r := range rows {
		if r.SnapshotGuid == 0 {
			return 0, fmt.Errorf("snapshot_guid must be non-zero for check-then-insert")
		}
	}
	if !s.uniqueGuidKey {
		return s.insertSnapshotsRowByRow(ctx, rows)
	}

	fresh, err := s.snapshotRowsNotYetStored(ctx, rows)
	if err != nil {
		return 0, err
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	query := insertSnapshotsSQL(len(fresh))
	args := make([]any, 0, len(fresh)*10)
	for _, r := range fresh {
		rowID, err := nextRowID(ctx, s.snapshotRowIDs)
		if err != nil {
			return 0, fmt.Errorf("mint row id for snapshot batch of %d: %w", len(fresh), err)
		}
		args = append(args, snapshotInsertArgs(rowID, r)...)
	}
	var res sql.Result
	reruns, err := retryOnDeadlock(ctx, snapshotGuidInsertAttempts, snapshotGuidDeadlockBackoff, func() error {
		var execErr error
		res, execErr = s.db.ExecContext(ctx, query, args...)
		return execErr
	})
	if reruns > 0 {
		s.deadlockRetries.Add(uint64(reruns))
		logx.Infof("[SnapshotStore] snapshot batch of %d (first guid=%d) retried %d times on the unique-key path after InnoDB deadlock (1213), result err=%v: "+
			"rows are inserted in snapshot_guid order, so this should only come from the >=3-writers-on-one-guid first-inserter-rollback residual (see InsertSnapshotsIfGuidAbsent); persistent occurrences warrant checking for repeated consumer-group rebalance",
			len(fresh), fresh[0].SnapshotGuid, reruns, err)
	}
	if isDupEntryMySQL(err) {
		// 预读之后有并发写者落了同一个 guid(rebalance 重叠):整条语句已自动回滚,逐行走单行 ODKU 判重。
		s.batchFallbacks.Add(1)
		logx.Infof("[SnapshotStore] snapshot batch of %d (first guid=%d) hit 1062 after the pre-read: a concurrent writer stored "+
			"one of these guids in between (rebalance overlap); falling back to row-by-row: %v", len(fresh), fresh[0].SnapshotGuid, err)
		return s.insertSnapshotsRowByRow(ctx, fresh)
	}
	if err != nil {
		return 0, fmt.Errorf("insert snapshot batch of %d (first guid %d): %w", len(fresh), fresh[0].SnapshotGuid, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("insert snapshot batch of %d rows affected: %w", len(fresh), err)
	}
	if affected != int64(len(fresh)) {
		// 普通多行 INSERT 成功就是每行 1:不等于行数说明语句被改坏(例如被加上了 IGNORE / ODKU),
		// 拿它当插入数会把丢行或重放算错,fail-closed,不让消费者提交。
		return 0, fmt.Errorf("insert snapshot batch of %d: rows affected %d, want exactly %d", len(fresh), affected, len(fresh))
	}
	return affected, nil
}

// snapshotRowsNotYetStored 返回 rows 里**库里还没有**、且是批内第一次出现的那些行,按 snapshot_guid 升序。
// 预读是普通读(不加锁);它只决定哪些行进多行 INSERT,正确性由 INSERT 的唯一键与 1062 → 逐行兜住。
func (s *SnapshotStore) snapshotRowsNotYetStored(ctx context.Context, rows []*SnapshotRow) ([]*SnapshotRow, error) {
	guids := make([]any, 0, len(rows))
	seen := make(map[uint64]struct{}, len(rows))
	for _, r := range rows {
		if _, dup := seen[r.SnapshotGuid]; dup {
			continue
		}
		seen[r.SnapshotGuid] = struct{}{}
		guids = append(guids, r.SnapshotGuid)
	}
	stored := make(map[uint64]struct{}, len(guids))
	dbRows, err := s.db.QueryContext(ctx, existingSnapshotGuidsSQL(len(guids)), guids...)
	if err != nil {
		return nil, fmt.Errorf("pre-read stored guids of snapshot batch of %d: %w", len(rows), err)
	}
	defer dbRows.Close()
	for dbRows.Next() {
		var g uint64
		if err := dbRows.Scan(&g); err != nil {
			return nil, fmt.Errorf("scan stored guid of snapshot batch: %w", err)
		}
		stored[g] = struct{}{}
	}
	if err := dbRows.Err(); err != nil {
		return nil, fmt.Errorf("pre-read stored guids of snapshot batch of %d: %w", len(rows), err)
	}
	fresh := make([]*SnapshotRow, 0, len(guids))
	taken := make(map[uint64]struct{}, len(guids))
	for _, r := range rows {
		if _, ok := stored[r.SnapshotGuid]; ok {
			continue
		}
		if _, ok := taken[r.SnapshotGuid]; ok {
			continue // 批内重复:先出现的那条已经入选
		}
		taken[r.SnapshotGuid] = struct{}{}
		fresh = append(fresh, r)
	}
	// 剔重之后按 guid 升序(取锁顺序 = 唯一键顺序),原因见 InsertSnapshotsIfGuidAbsent「为什么要按 guid 升序插」。
	// fresh 是新切片,不动调用方的 rows;剔重已在前面按 offset 序做完,所以「先出现者赢」不受排序影响。
	slices.SortFunc(fresh, func(a, b *SnapshotRow) int { return cmp.Compare(a.SnapshotGuid, b.SnapshotGuid) })
	return fresh, nil
}

// insertSnapshotsRowByRow 逐行调 InsertSnapshotIfGuidAbsent,返回新插入行数(退路形态,以及唯一键形态撞 1062 后)。
func (s *SnapshotStore) insertSnapshotsRowByRow(ctx context.Context, rows []*SnapshotRow) (int64, error) {
	var inserted int64
	for _, r := range rows {
		_, ok, err := s.InsertSnapshotIfGuidAbsent(ctx, r)
		if err != nil {
			return inserted, err
		}
		if ok {
			inserted++
		}
	}
	return inserted, nil
}

// 「这一批本身太大」的确定性错误号(G2a 评审 I-15):同一批原样重试只会得到同一个错误。
const (
	mysqlErrNetPacketTooLarge = 1153 // ER_NET_PACKET_TOO_LARGE:语句超过服务端 max_allowed_packet
	tidbErrTxnTooLarge        = 8004 // TiDB ErrTxnTooLarge:事务超过 txn-total-size-limit
)

// IsSnapshotBatchTooLarge 报告 err 是不是「这一批太大」的确定性错误(服务端 1153 / 驱动 ErrPktTooLarge / TiDB 8004)。
// 消费者据此跳过批级的退避重试、直接逐条落库:等 DBMaxAttempts 秒再撞一次同样的墙没有意义。
// 只认这几个号;其余错误(含 1205 / 断连)照常按通用重试处理,不在这里猜。
func IsSnapshotBatchTooLarge(err error) bool {
	if errors.Is(err, mysql.ErrPktTooLarge) {
		return true
	}
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == mysqlErrNetPacketTooLarge || me.Number == tidbErrTxnTooLarge)
}

// lookupSnapshotIDByGuid 查这个 guid 已存在那行的行号(日志/审计要用)。reason 只进错误文案。
//
// 查不到**一律当故障**,不再返回 (0, false, nil):调用方是 kafka.SnapshotConsumer,它收到
// (id=0, inserted=false, err=nil) 会记一条 duplicate 指标、打一条 `already stored as id=0` 的 INFO,
// 然后**提交 offset** —— 这条快照就永久丢了,且全程零报错。
// 能走到这里只有一种局面:写入语句判定"已存在"之后、这次点查之前,有人把那一行删了
// (DeleteOldSnapshots 的 retention 清理,或迁移期的去重 DELETE)。窗口很窄但删者是真实存在的。
// 报错让消费者重试:重试时那个 guid 确实不在了,写入语句会**重新插进去**并成功,所以这条错误必然收敛,
// 不会把消费者永久卡住。宁可滞后不丢,与本文件其它路径同口径。
func (s *SnapshotStore) lookupSnapshotIDByGuid(ctx context.Context, guid uint64, reason string) (uint64, bool, error) {
	var existing uint64
	if err := s.db.QueryRowContext(ctx, s.guidLookupStatement(), guid).Scan(&existing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("snapshot_guid %d:%s,但按 %s 查不到这一行"+
				"(并发删除?retention 清理 / 迁移去重 刚好删掉了它?)—— 当故障上报,让调用方重试后重新落库,"+
				"不能回出 id=0 让消费者提交 offset 把这条快照丢掉", guid, reason, s.guidLookupStatement())
		}
		return 0, false, fmt.Errorf("lookup snapshot_guid %d: %w", guid, err)
	}
	return existing, false, nil
}

// retryOnDeadlock 执行 op;op 返回 InnoDB 死锁(1213)时等 backoff() 后重跑,总尝试次数不超过 attempts。
//
// 契约:
//   - 只重跑 1213(isDeadlockMySQL)。1205 与其它错误立即原样返回,原因见 InsertSnapshotIfGuidAbsent。
//   - op 必须是"整条回滚后可原样重跑"的操作:这里只用于单条自动提交语句,InnoDB 选中牺牲者时
//     已把它整条回滚,重跑不会重复写。
//   - reruns 是因 1213 重跑 op 的次数;最后一次仍是 1213 时返回该错误(reruns = attempts-1)。
//   - 等待可被 ctx 取消:取消时立即返回,错误同时带上最后一次的 1213 与 ctx 的原因,调用方两者都能 errors.Is。
func retryOnDeadlock(ctx context.Context, attempts int, backoff func() time.Duration, op func() error) (reruns int, err error) {
	for attempt := 1; ; attempt++ {
		err = op()
		if err == nil || !isDeadlockMySQL(err) || attempt >= attempts {
			return reruns, err
		}
		if waitErr := sleepCtx(ctx, backoff()); waitErr != nil {
			return reruns, errors.Join(err, waitErr)
		}
		reruns++
	}
}

// snapshotGuidDeadlockBackoff 在 [snapshotGuidDeadlockBackoffMin, snapshotGuidDeadlockBackoffMax] 内均匀取一个等待时长。
func snapshotGuidDeadlockBackoff() time.Duration {
	return jitteredBackoff(snapshotGuidDeadlockBackoffMin, snapshotGuidDeadlockBackoffMax)
}

// isDeadlockMySQL 只认 1213(错误号常量见 id_segment_store.go)。刻意不复用 isRetryableMySQL(它把 1205 也算进来):
// 就地重跑 1205 会把消费者的最坏等待放大数倍,见 InsertSnapshotIfGuidAbsent。
func isDeadlockMySQL(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlErrDeadlock
}

// isDupEntryMySQL 只认 1062(错误号常量见 player_name_store.go)。
// 它只在 InsertSnapshotIfGuidAbsent 的**退路**(NOT EXISTS)形态上被当成正常的去重结果 ——
// 那条路上 1062 只可能来自 uk_snapshot_guid_nz(库里有键而本进程探测时还没有)。
// 唯一键形态的单行路径由 ODKU 就地吸收重复(任何唯一键都吸收,见 InsertSnapshotIfGuidAbsent 的注释),冒出 1062
// 属异常,调用点按故障处理;批量路径的普通多行 INSERT 撞 1062 则转逐行(见 InsertSnapshotsIfGuidAbsent)。
// 不靠这个分类器区分撞的是哪个键(错误文本里的键名不是稳定契约,不拿它做判据)。
// 与 player_name_store.go 的 isDuplicateEntryMySQL 同判据 —— 两个 store 的重试/去重口径各自独立演进,
// 刻意不强行共用一个函数;错误号常量只有一处定义,不会漂移。
func isDupEntryMySQL(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlErrDupEntry
}

// ── 本包锁冲突重试共用的两个原语(SnapshotStore 与 PlayerNameStore 都用,不各写一份)──────────────

// jitteredBackoff 在 [lo, hi] 内均匀取一个等待时长(即以区间中点为基准的 ± 抖动)。
//
// 抖动的目的:两个刚互杀过(1213)的事务如果按固定间隔同时回来,多半还会按同样的顺序撞在同一批锁上,
// 次数预算被空耗;随机错开之后,胜者通常已经提交,重来的一方只剩单向等待。
// hi <= lo 时返回 lo(区间退化成一个点),不让 rand.N 收到非正参数而 panic。
func jitteredBackoff(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + rand.N(hi-lo+1)
}

// sleepCtx 可取消的等待:ctx 先结束就立即返回 ctx.Err()。不用 time.Sleep —— 在一个已经没有预算的
// ctx 上睡满,等于把"早点告诉调用方"拖成"超时失败"。
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// GetSnapshotByID loads a snapshot by primary key. source=1 行对回滚读路径不可见(见文件头)。
func (s *SnapshotStore) GetSnapshotByID(ctx context.Context, id uint64) (*SnapshotRow, error) {
	row := &SnapshotRow{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source
		 FROM player_snapshot WHERE id = ? AND source = ?`, id, SnapshotSourceDataService,
	).Scan(&row.ID, &row.PlayerID, &row.ZoneID, &row.SnapshotType, &row.CreatedAt,
		&row.Reason, &row.Operator, &row.Data, &row.SnapshotGuid, &row.Source)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return row, err
}

// GetLatestSnapshotBefore returns the most recent source=0 snapshot for a player before the given time.
func (s *SnapshotStore) GetLatestSnapshotBefore(ctx context.Context, playerID, beforeTime uint64) (*SnapshotRow, error) {
	row := &SnapshotRow{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source
		 FROM player_snapshot
		 WHERE player_id = ? AND created_at <= ? AND source = ?
		 ORDER BY created_at DESC LIMIT 1`, playerID, beforeTime, SnapshotSourceDataService,
	).Scan(&row.ID, &row.PlayerID, &row.ZoneID, &row.SnapshotType, &row.CreatedAt,
		&row.Reason, &row.Operator, &row.Data, &row.SnapshotGuid, &row.Source)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return row, err
}

// ListSnapshots returns metadata (no data blob) for a player's source=0 snapshots.
func (s *SnapshotStore) ListSnapshots(ctx context.Context, playerID, beforeTime uint64, limit uint32) ([]*SnapshotRow, error) {
	metas, err := s.ListSnapshotsMeta(ctx, playerID, beforeTime, limit)
	if err != nil {
		return nil, err
	}
	result := make([]*SnapshotRow, 0, len(metas))
	for _, m := range metas {
		r := m.SnapshotRow
		result = append(result, &r)
	}
	return result, nil
}

// SnapshotMeta is ListSnapshotsMeta's row: metadata plus data size, no blob.
type SnapshotMeta struct {
	SnapshotRow
	DataSizeBytes uint32
}

// ListSnapshotsMeta lists a player's source=0 snapshots newest first.
func (s *SnapshotStore) ListSnapshotsMeta(ctx context.Context, playerID, beforeTime uint64, limit uint32) ([]*SnapshotMeta, error) {
	if limit == 0 {
		limit = 20
	}

	var (
		rows *sql.Rows
		err  error
	)
	if beforeTime > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, snapshot_guid, source, COALESCE(LENGTH(data),0)
			 FROM player_snapshot
			 WHERE player_id = ? AND created_at <= ? AND source = ?
			 ORDER BY created_at DESC LIMIT ?`, playerID, beforeTime, SnapshotSourceDataService, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, player_id, zone_id, snapshot_type, created_at, reason, operator, snapshot_guid, source, COALESCE(LENGTH(data),0)
			 FROM player_snapshot
			 WHERE player_id = ? AND source = ?
			 ORDER BY created_at DESC LIMIT ?`, playerID, SnapshotSourceDataService, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*SnapshotMeta
	for rows.Next() {
		m := &SnapshotMeta{}
		if err := rows.Scan(&m.ID, &m.PlayerID, &m.ZoneID, &m.SnapshotType, &m.CreatedAt,
			&m.Reason, &m.Operator, &m.SnapshotGuid, &m.Source, &m.DataSizeBytes); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// GetSnapshotPlayerIDsByZone returns all player IDs with a source=0 snapshot in a zone
// at or before beforeTime. Uses the zone_id stored at snapshot time.
func (s *SnapshotStore) GetSnapshotPlayerIDsByZone(ctx context.Context, zoneID uint32, beforeTime uint64) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT player_id FROM player_snapshot
		 WHERE zone_id = ? AND created_at <= ? AND source = ?`, zoneID, beforeTime, SnapshotSourceDataService)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uint64
	for rows.Next() {
		var pid uint64
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		ids = append(ids, pid)
	}
	return ids, rows.Err()
}

// GetSnapshotPlayerTimesByZone 返回 zone 内每个在 beforeTime(含)之前有 source=0 快照的玩家,
// 以及他在**这个 zone 下**最新那份快照的 created_at(秒):player_id → MAX(created_at)。
//
// 用途是 zone / 全服回档的"计划"(docs/design/guild-phase2/07-rollback-fail-closed.md §7.5.3-1):
// 一条 SQL 拿到清单与每人的快照时刻,作为帮会检查的 since 起点,替代逐人读整份快照 blob。
//
// 与执行期的关系(R5):执行期 GetLatestSnapshotBefore **不带** zone_id 条件,选中的快照只会
// 等于或晚于这里的值(玩家在别的 zone 另有更新的快照时)。计划值更早 = since 更早 = 多查,方向安全;
// 反过来(执行期更早)只可能来自快照被并发删除,调用方据此拒绝写该玩家。
// 走 (zone_id, created_at) 索引,与 GetSnapshotPlayerIDsByZone 同一过滤条件。
func (s *SnapshotStore) GetSnapshotPlayerTimesByZone(ctx context.Context, zoneID uint32, beforeTime uint64) (map[uint64]uint64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT player_id, MAX(created_at) FROM player_snapshot
		 WHERE zone_id = ? AND created_at <= ? AND source = ?
		 GROUP BY player_id`, zoneID, beforeTime, SnapshotSourceDataService)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	times := make(map[uint64]uint64)
	for rows.Next() {
		var pid, createdAt uint64
		if err := rows.Scan(&pid, &createdAt); err != nil {
			return nil, err
		}
		times[pid] = createdAt
	}
	return times, rows.Err()
}

// SceneSnapshotMeta 是 source=1(C++ scene)快照的元数据,给将来的 GM 列表 / 恢复路径用。
type SceneSnapshotMeta struct {
	ID            uint64 // player_snapshot.id(行号,不代表先后)
	SnapshotGuid  uint64 // C++ SnapshotId
	ZoneID        uint32
	Trigger       uint32 // C++ SnapshotTrigger 原值(snapshot_type 列)
	CreatedAt     uint64 // = PlayerSnapshotEntry.snapshot_time
	DataSizeBytes uint32
}

// ListSceneSnapshotsByPlayer lists a player's source=1 rows newest first (no blob).
// 尚未接 RPC:source=1 的恢复路径是下一阶段。
// id DESC 只是让同一秒内的次序稳定,行号是打散的,这个次序不代表先后。
func (s *SnapshotStore) ListSceneSnapshotsByPlayer(ctx context.Context, playerID uint64, limit uint32) ([]*SceneSnapshotMeta, error) {
	if limit == 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, snapshot_guid, zone_id, snapshot_type, created_at, COALESCE(LENGTH(data),0)
		 FROM player_snapshot
		 WHERE player_id = ? AND source = ?
		 ORDER BY created_at DESC, id DESC LIMIT ?`, playerID, SnapshotSourceSceneKafka, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*SceneSnapshotMeta
	for rows.Next() {
		m := &SceneSnapshotMeta{}
		if err := rows.Scan(&m.ID, &m.SnapshotGuid, &m.ZoneID, &m.Trigger, &m.CreatedAt, &m.DataSizeBytes); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// InsertAuditLog writes a rollback audit record.
func (s *SnapshotStore) InsertAuditLog(ctx context.Context, row *AuditLogRow) error {
	id, err := nextRowID(ctx, s.auditRowIDs)
	if err != nil {
		return fmt.Errorf("mint rollback audit row id: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO rollback_audit_log
		 (id, player_id, zone_id, rollback_type, snapshot_id_used, pre_rollback_snapshot_id,
		  target_time, players_affected, players_failed, orphans_cleaned, reason, operator, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, row.PlayerID, row.ZoneID, row.RollbackType, row.SnapshotIDUsed,
		row.PreRollbackSnapshotID, row.TargetTime, row.PlayersAffected,
		row.PlayersFailed, row.OrphansCleaned, row.Reason, row.Operator, row.CreatedAt,
	)
	return err
}

// DeleteOldSnapshots removes snapshots (both sources) older than the given timestamp.
// Used for retention policies.
func (s *SnapshotStore) DeleteOldSnapshots(ctx context.Context, olderThan uint64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM player_snapshot WHERE created_at < ?`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
