package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

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
const insertSnapshotSQL = `INSERT INTO player_snapshot
	 (player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

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
//   - 每次重放都会烧掉一个自增值(取号发生在重复键检查之前),id 因此有空洞 —— 它本来就只是行号,
//     没有任何业务含义,bigint unsigned 也烧不完。
//   - 撞键时取的是唯一键上的 **next-key X**(手册:"An exclusive next-key lock is taken for a
//     duplicate unique key value"),范围仅限同一个 guid。没撞键的常态路径与普通 INSERT 完全一样,
//     只取插入意向锁,所以不同 guid 的写者仍然互不相干(回归 TestSnapshotStore_DistinctGuidsNeverDeadlock)。
const insertSnapshotOnDuplicateKeepSQL = insertSnapshotSQL + "\n\t ON DUPLICATE KEY UPDATE `id` = LAST_INSERT_ID(`id`)"

// insertSnapshotIfGuidAbsentLegacySQL 是唯一键**还不存在**时的去重写法(审计 #11 第二步之前的生产路径)。
//
// 现在它只在两种局面下使用,由建 store 时的一次探测决定(见 SnapshotStore.uniqueGuidKey):
// 表结构还没跑过 uk_snapshot_guid_nz 的迁移(含刚刚回滚 DDL 的窗口),以及 TiDB
// (官方文档不支持 ALTER 加 STORED 生成列,见 schema.go 的 ensureNullableUniqueKey)。
// 它的去重靠 REPEATABLE READ 下 INSERT ... SELECT 源端的**加锁读**,代价是并发写者会在
// idx(snapshot_guid) 的同一段间隙上互等插入意向锁而成环 —— 那正是唯一键要消掉的东西。
const insertSnapshotIfGuidAbsentLegacySQL = `INSERT INTO player_snapshot
	 (player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source)
	 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?
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
// 探测只是一条 INFORMATION_SCHEMA 点查;探不到不影响功能(退回 NOT EXISTS 形态),
// 所以宁可短超时放过,也不要让它把服务启动拖住。
const uniqueGuidKeyProbeTimeout = 5 * time.Second

// SnapshotStore provides CRUD operations for player snapshots and audit logs.
// 表结构由 MigrateSchema 按 proto 建 + schema.go 的 nullableUniqueKeys 追加可空唯一列,
// 本 store 不碰 DDL,只在建 store 时**读一次**结构来决定用哪条去重语句。
type SnapshotStore struct {
	db *sql.DB

	// uniqueGuidKey = 建 store 时探到 player_snapshot 上有 uk_snapshot_guid_nz,
	// **且**它约束的 snapshot_guid_nz 确实是那条表达式算出的 STORED 生成列(两条缺一不可,见 NewSnapshotStore)。
	//
	// 为什么要探、而不是直接假设它在:这个键是一次独立的 DDL 迁移(审计 #11 第二步),
	// 而"迁移还没跑 / 刚回滚 / 对端是 TiDB"这三种局面都真实存在。假设它在的后果是最坏的一种 ——
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
	// 探测失败按"键不在"处理(fail-safe 到**功能正确**的那一侧):NOT EXISTS 形态在有键和无键的
	// 表上都去重正确,而普通 INSERT 只在有键时才去重。所以猜错方向的代价是多承担一次间隙锁死锁重试,
	// 不是静默写重复行。
	// 必须用 hasUniqueIndex 而不是"按名字数一下索引在不在":一个**同名的非唯一索引**会让这里探成 true,
	// 写路径切到 ODKU,而 ODKU 在没有唯一约束的表上永远撞不到重复键 —— 同一 guid 静默落成多行。
	// 形状不符时它返回错误,下面按"键不在"处理并把原因打出来(退路的去重在有键无键的表上都正确)。
	//
	// 索引形状对**还不够**:键可能挂在一个同名的**普通列**上(迁移的 ADD COLUMN 被兼容层降级、
	// 或有人手工加过这一列),那种列恒为 NULL(约束不到任何行)或恒为 0(第二条 GM 行就撞键)。
	// 所以列的形状也要探一次,两条都成立才敢把写路径切到 ODKU —— 判据与迁移侧
	// (ensureNullableUniqueKey 的快路径)逐条一致,免得两处对"键在不在"给出不同答案。
	hasKey, err := hasUniqueIndex(ctx, db, cfg.DBName, PlayerSnapshotTableName, SnapshotGuidUniqueKey)
	if err == nil && hasKey {
		var hasCol bool
		hasCol, err = hasGeneratedColumn(ctx, db, cfg.DBName, PlayerSnapshotTableName, SnapshotGuidNzColumn, snapshotGuidNzExpr)
		if err == nil && !hasCol {
			err = fmt.Errorf("唯一键 %s 在,但它约束的列 %s 不存在(键在、列坏的中间态)",
				SnapshotGuidUniqueKey, SnapshotGuidNzColumn)
		}
		if err != nil {
			hasKey = false
		}
	}
	if err != nil {
		logx.Errorf("[SnapshotStore] 探测 %s.%s 失败(%v):按「唯一键不存在」处理,快照去重退回 "+
			"INSERT...SELECT...NOT EXISTS(功能正确,但并发写者会在 idx(snapshot_guid) 间隙上成环、靠有界 1213 重试吸收)。"+
			"确认迁移已跑过(`data_service -f <yaml> -migrate`)后重启本服务即可恢复",
			PlayerSnapshotTableName, SnapshotGuidUniqueKey, err)
	}
	s.uniqueGuidKey = hasKey
	if hasKey {
		logx.Infof("[SnapshotStore] %s.%s 在:快照按 guid 去重走 INSERT ... ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)"+
			"(撞键取 X 而不是 S,同 guid 的排队者不再成环)",
			PlayerSnapshotTableName, SnapshotGuidUniqueKey)
	} else if err == nil {
		logx.Errorf("[SnapshotStore] %s.%s 不存在:快照去重退回 INSERT...SELECT...NOT EXISTS。"+
			"MySQL 上跑一次 `data_service -f <yaml> -migrate` 即可建出(纯加法),建出后重启本服务即切到 ODKU 形态、"+
			"并消掉退路那条语句在 idx(snapshot_guid) 上的间隙环;"+
			"若对端是 TiDB 而迁移仍然成功了,说明有人打开了 MigrateOptions.AllowMissingGuidUniqueKey —— "+
			"**本实例的快照 guid 去重已失效**(TiDB 没有间隙锁,NOT EXISTS 去重不成立),见 schema.go 的 ensureNullableUniqueKey",
			PlayerSnapshotTableName, SnapshotGuidUniqueKey)
	}
	return s, nil
}

// snapshotInsertArgs 按 insertSnapshotSQL 的占位符顺序展开一行。
// 两条 INSERT(普通 / NOT EXISTS)的前九个占位符完全一致,顺序只在这里定义一次。
func snapshotInsertArgs(row *SnapshotRow) []any {
	return []any{row.PlayerID, row.ZoneID, row.SnapshotType, row.CreatedAt, row.Reason, row.Operator, row.Data,
		row.SnapshotGuid, row.Source}
}

// guidDedupeStatement 按探到的结构给出这次写入要用的语句与实参。
func (s *SnapshotStore) guidDedupeStatement(row *SnapshotRow) (string, []any) {
	if s.uniqueGuidKey {
		return insertSnapshotOnDuplicateKeepSQL, snapshotInsertArgs(row)
	}
	// NOT EXISTS 形态多一个占位符:子查询里再绑一次同一个 guid。
	return insertSnapshotIfGuidAbsentLegacySQL, append(snapshotInsertArgs(row), row.SnapshotGuid)
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

// InsertSnapshot saves a snapshot and returns the auto-increment ID.
// GM 路径调用方不设 Source / SnapshotGuid,即落成 source=0、guid=0。
func (s *SnapshotStore) InsertSnapshot(ctx context.Context, row *SnapshotRow) (uint64, error) {
	res, err := s.db.ExecContext(ctx, insertSnapshotSQL, snapshotInsertArgs(row)...)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return uint64(id), nil
}

// InsertSnapshotIfGuidAbsent 按 snapshot_guid 去重落库,返回 inserted=false 时
// id 是已存在那行的自增 id。
//
// # 去重由库上的可空唯一键兜住
//
// 事实源是 player_snapshot 上的存储型生成列 snapshot_guid_nz = NULLIF(snapshot_guid, 0)
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
// 唯一键不存在时(迁移还没跑、DDL 刚回滚、对端是 TiDB)退回 insertSnapshotIfGuidAbsentLegacySQL,
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
// 本表没有这样做:快照写者部署上是单实例(replicas=1 + Recreate),这一类只在多写者并存时出现,
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
// 留着是为了**退路那条语句**:唯一键不存在时(迁移未跑 / DDL 刚回滚 / 对端是 TiDB)走
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
//   - 能并发的写者本来就只有"短时两三个"这个量级:部署不变量是 data-service replicas=1 + Recreate
//     (deploy/k8s/manifests/go-svc/data-service.yaml),只有本地多开 / 手工改副本数 / 滚动更新窗口
//     才会出现并发写者,也才会看到本函数"就地吸收了 1213"的日志。
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
//     不能靠这条退路。ensureNullableUniqueKey 因此在 TiDB 上 **fail-closed**:建不出这个键就让迁移失败,
//     除非有人用 MigrateOptions.AllowMissingGuidUniqueKey 显式放行(放行 = 明确接受本实例去重失效)。
//     所以运行期真的走到这条退路,只有"MySQL 上迁移还没跑 / DDL 刚回滚"两种局面。
//
// idx(snapshot_guid) 由 store.MigrateSchema 的 ensureIndexes 保证存在;唯一键路径不依赖它,
// 但 GM 列表 / 去重迁移仍要用,缺了会退化成全表扫。
func (s *SnapshotStore) InsertSnapshotIfGuidAbsent(ctx context.Context, row *SnapshotRow) (id uint64, inserted bool, err error) {
	if row.SnapshotGuid == 0 {
		return 0, false, fmt.Errorf("snapshot_guid must be non-zero for check-then-insert")
	}

	query, args := s.guidDedupeStatement(row)
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
			logx.Infof("[SnapshotStore] snapshot guid=%d 在**唯一键路径**上因 InnoDB 死锁(1213)就地重跑了 %d 次,结果 err=%v:"+
				"预期内的固有情形 —— 同一个 guid 有两个写者排在第三个未提交写者后面、而后者回滚时,"+
				"等待锁被继承成间隙锁再互等插入意向锁(实测与写法无关,ODKU 也拆不掉,见 InsertSnapshotIfGuidAbsent)。"+
				"结局由重试保证正确。**但本服务部署上是单写者(replicas=1 + Recreate)**,所以这条日志频繁出现"+
				"说明有第二个写者在跑(本地多开 / 手工改副本数 / 滚动更新新旧并存)——那才是要查的事",
				row.SnapshotGuid, reruns, err)
		} else {
			logx.Errorf("[SnapshotStore] snapshot guid=%d 在**退路**(INSERT...SELECT...NOT EXISTS)上因 InnoDB 死锁(1213)"+
				"就地重跑了 %d 次,结果 err=%v:这是 idx(snapshot_guid) 的间隙环 —— 子查询在 RR 下是加锁读,"+
				"**连不同 guid 的写者也会互杀**,并不是同一条快照的竞争。它可以彻底消掉:"+
				"跑一次 `data_service -f <yaml> -migrate` 建出 %s 后重启本服务,写路径即切到 ODKU 形态",
				row.SnapshotGuid, reruns, err, SnapshotGuidUniqueKey)
		}
	}
	if err != nil {
		if isDupEntryMySQL(err) && !s.uniqueGuidKey {
			// 退路形态的**正常**去重结果:表上已经有唯一键(滚动升级窗口 / 迁移刚跑完但本进程是探测之前起的),
			// 而 NOT EXISTS 与插入之间被别的写者抢先。按 guid 点查出它的 id,返回 inserted=false。
			return s.lookupSnapshotIDByGuid(ctx, row.SnapshotGuid, "退路形态撞 1062")
		}
		// 唯一键形态的 1062 **不是**去重结果:ODKU 会就地吸收 uk_snapshot_guid_nz 的重复,
		// 还能撞出 1062 只说明撞的是**别的**唯一约束(今后给 player_snapshot 加的新键)。
		// 把它当成"这个 guid 已经落过了"会让消费者提交 offset 并丢掉这条快照,所以一律当故障往上报。
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
		newID, err := res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("insert snapshot guid %d last insert id: %w", row.SnapshotGuid, err)
		}
		return uint64(newID), true, nil
	}

	// 到这里 = 这个 guid 已经落过了(ODKU 判定值未变 affected=0,或 NOT EXISTS 子查询判假 affected=0)。
	if s.uniqueGuidKey {
		// ODKU 的 `id = LAST_INSERT_ID(id)` 已经把已存在那行的 id 记进会话并由 OK 包回带,
		// 常态下一次往返就够。手册没有明说"值未变时一定回带",拿到 0 时退回一次唯一键点查。
		if existingID, err := res.LastInsertId(); err == nil && existingID > 0 {
			return uint64(existingID), false, nil
		}
	}
	return s.lookupSnapshotIDByGuid(ctx, row.SnapshotGuid, "写入语句判定这个 guid 已存在")
}

// lookupSnapshotIDByGuid 查这个 guid 已存在那行的自增 id(日志/审计要用)。reason 只进错误文案。
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
// 唯一键形态由 ODKU 就地吸收重复,再冒出 1062 就是**别的**唯一约束,调用点按故障处理,
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
	ID            uint64 // player_snapshot.id(自增)
	SnapshotGuid  uint64 // C++ SnapshotId
	ZoneID        uint32
	Trigger       uint32 // C++ SnapshotTrigger 原值(snapshot_type 列)
	CreatedAt     uint64 // = PlayerSnapshotEntry.snapshot_time
	DataSizeBytes uint32
}

// ListSceneSnapshotsByPlayer lists a player's source=1 rows newest first (no blob).
// 尚未接 RPC:source=1 的恢复路径是下一阶段。
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
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rollback_audit_log
		 (player_id, zone_id, rollback_type, snapshot_id_used, pre_rollback_snapshot_id,
		  target_time, players_affected, players_failed, orphans_cleaned, reason, operator, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.PlayerID, row.ZoneID, row.RollbackType, row.SnapshotIDUsed,
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
