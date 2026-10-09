package store

import (
	"context"
	"database/sql"
)

// 仅供同目录 store_test 包(真库集成用例)使用的只读出口;_test.go 文件不进生产二进制。

// 让用例用与生产**同一段文本**扮演另一个并发写者 / 扮演回滚窗口里的旧版本二进制。
const (
	// InsertSnapshotSQLForTest 普通单行 INSERT(GM 路径用它;实参顺序用 SnapshotInsertArgsForTest)。
	// 真库用例还拿它扮演"不做任何去重的第三方写者"(造历史重复行、逼库报 1062)。
	InsertSnapshotSQLForTest = insertSnapshotSQL
	// InsertSnapshotOnDuplicateKeepSQLForTest 唯一键路径的 ODKU 形态:撞键时取 X 而不是 S,
	// 同 guid 的排队者不成环。用例拿它扮演另一个并发写者。
	InsertSnapshotOnDuplicateKeepSQLForTest = insertSnapshotOnDuplicateKeepSQL
	// InsertSnapshotIfGuidAbsentLegacySQLForTest 唯一键不存在时的 NOT EXISTS 形态。
	// 它同时是"旧版本二进制"的写法:滚动升级窗口的回归用它对着**新**表结构跑。
	// 实参 = SnapshotInsertArgsForTest(row) 再追加一次 row.SnapshotGuid。
	InsertSnapshotIfGuidAbsentLegacySQLForTest = insertSnapshotIfGuidAbsentLegacySQL
	// LookupSnapshotIDByGuidNzSQLForTest 按唯一键点查已有 id 的语句(EXPLAIN 回归用)。
	LookupSnapshotIDByGuidNzSQLForTest = lookupSnapshotIDByGuidNzSQL
)

// SnapshotInsertArgsForTest 按 InsertSnapshotSQLForTest 的占位符顺序展开一行,
// 让用例不必再抄一份占位符顺序(抄了就会在加列时与生产漂移)。
func SnapshotInsertArgsForTest(row *SnapshotRow) []any { return snapshotInsertArgs(row) }

// DeadlockRetriesForTest 返回本 store 因 1213 就地重跑 InsertSnapshotIfGuidAbsent 的累计次数。
func (s *SnapshotStore) DeadlockRetriesForTest() uint64 { return s.deadlockRetries.Load() }

// UniqueGuidKeyForTest 返回建 store 时探到的结构能力(uk_snapshot_guid_nz 在不在)。
func (s *SnapshotStore) UniqueGuidKeyForTest() bool { return s.uniqueGuidKey }

// LookupSnapshotIDByGuidForTest 直接调"查这个 guid 已存在那行 id"的内部出口。
// 用例靠它给"查不到必须报错(而不是回出 id=0 让消费者提交 offset 丢数据)"取证 ——
// 这个交错没法在生产的两条语句之间稳定塞进一次删除,只能直接调出口。
func (s *SnapshotStore) LookupSnapshotIDByGuidForTest(ctx context.Context, guid uint64) (uint64, bool, error) {
	return s.lookupSnapshotIDByGuid(ctx, guid, "用例直接调用")
}

// ForceLegacyGuidDedupeForTest 把本 store 按"唯一键不存在"处理,用来在**已有唯一键**的表上
// 跑退路那条语句(TiDB / 未迁移 / DDL 刚回滚三种局面的回归)。只给用例用,生产没有这个开关:
// 结构能力只在建 store 时探一次,运行期不该被改。
func (s *SnapshotStore) ForceLegacyGuidDedupeForTest() { s.uniqueGuidKey = false }

// 迁移侧的只读出口:去重 / 唯一键那一步的语句与上限,让用例引用同一份定义。
const (
	SnapshotGuidDedupeMaxRowsForTest       = snapshotGuidDedupeMaxRows
	SnapshotGuidDedupeGuidsPerBatchForTest = snapshotGuidDedupeGuidsPerBatch
)

// DedupePointDeleteSQLForTest 迁移去重那一步的**主键点删**语句(EXPLAIN 回归用:
// 必须走 PRIMARY、key_len=8,不能退化成二级索引上的范围删)。
func DedupePointDeleteSQLForTest(table string) string { return dedupePointDeleteSQL(table) }

// SnapshotGuidNzExprForTest 生成列的表达式,真库用例造"同名但形状不对的列"时要用到它的反面。
const SnapshotGuidNzExprForTest = snapshotGuidNzExpr

// PlayerNameReserveSQLForTest / PlayerNameReleaseSQLForTest 让用例用与生产**同一段文本**扮演并发的
// Reserve / 未提交的 Release(生产路径是自动提交,停不在提交之前,编排时只能把同一条语句放进显式事务)。
// PlayerNameReleaseLegacySQLForTest 是旧主键形态下的那一条(迁移未跑 / TiDB 的窗口)。
const (
	PlayerNameReserveSQLForTest       = playerNameReserveSQL
	PlayerNameReleaseSQLForTest       = playerNameReleaseSQL
	PlayerNameReleaseLegacySQLForTest = playerNameReleaseLegacySQL
	// PlayerNameReleaseForcedIndexForTest 是新结构下 Release 钉死的索引名(EXPLAIN 回归拿它当期望值)。
	PlayerNameReleaseForcedIndexForTest = playerNameReleaseForcedIndex
)

// LockRetriesForTest 返回本 store 的 Reserve / Release 因 1213/1205 重来的累计次数。
func (s *PlayerNameStore) LockRetriesForTest() uint64 { return s.lockRetries.Load() }

// LegacyKeyedByPlayerIDForTest 返回建 store 时探到的表形态:true = 主键还是 player_id(迁移没跑过 / TiDB)。
// 真库用例靠它确认自己跑在哪一种结构上 —— 新结构的"零死锁"判据在旧结构上并不成立。
func (s *PlayerNameStore) LegacyKeyedByPlayerIDForTest() bool { return s.legacyKeyedByPlayerID }

// ReleaseSQLForTest 返回本实例实际在用的 Release 语句(按形态选定)。
func (s *PlayerNameStore) ReleaseSQLForTest() string { return s.releaseSQL }

// PlayerNamePKShapeIsByNameForTest 给用例判断一组主键列是不是目标形态(name_norm)。
func PlayerNamePKShapeIsByNameForTest(pkCols []string) bool {
	return playerNamePKShapeOf(pkCols) == playerNamePKByName
}

// PrimaryKeyColumnsForTest 读一张表主键的列序,给真库用例断言迁移结果用。
func PrimaryKeyColumnsForTest(ctx context.Context, db *sql.DB, dbName, tableName string) ([]string, error) {
	return primaryKeyColumns(ctx, db, dbName, tableName)
}

// MySQLErrDeadlockForTest / MySQLErrDupEntryForTest 让真库用例按错误号判定时引用本包唯一的权威定义
// (mysqlErrDeadlock 在 id_segment_store.go、mysqlErrDupEntry 在 player_name_store.go),不在用例里再写一份字面量。
const (
	MySQLErrDeadlockForTest = mysqlErrDeadlock
	MySQLErrDupEntryForTest = mysqlErrDupEntry
)

// MigrateLockNameForTest 返回某个库的迁移锁名,让真库用例能扮演"另一个正在迁移的实例"占住同一把锁。
func MigrateLockNameForTest(dbName string) string { return migrateLockName(dbName) }
