package store

// 仅供同目录 store_test 包(真库集成用例)使用的只读出口;_test.go 文件不进生产二进制。

// InsertSnapshotIfGuidAbsentSQLForTest 让用例用与生产**同一段文本**扮演另一个并发写者。
const InsertSnapshotIfGuidAbsentSQLForTest = insertSnapshotIfGuidAbsentSQL

// DeadlockRetriesForTest 返回本 store 因 1213 就地重跑 InsertSnapshotIfGuidAbsent 的累计次数。
func (s *SnapshotStore) DeadlockRetriesForTest() uint64 { return s.deadlockRetries.Load() }

// PlayerNameReserveSQLForTest / PlayerNameReleaseSQLForTest 让用例用与生产**同一段文本**扮演并发的
// Reserve / 未提交的 Release(生产路径是自动提交,停不在提交之前,编排时只能把同一条语句放进显式事务)。
const (
	PlayerNameReserveSQLForTest = playerNameReserveSQL
	PlayerNameReleaseSQLForTest = playerNameReleaseSQL
)

// LockRetriesForTest 返回本 store 的 Reserve / Release 因 1213/1205 重来的累计次数。
func (s *PlayerNameStore) LockRetriesForTest() uint64 { return s.lockRetries.Load() }

// MySQLErrDeadlockForTest / MySQLErrDupEntryForTest 让真库用例按错误号判定时引用本包唯一的权威定义
// (mysqlErrDeadlock 在 id_segment_store.go、mysqlErrDupEntry 在 player_name_store.go),不在用例里再写一份字面量。
const (
	MySQLErrDeadlockForTest = mysqlErrDeadlock
	MySQLErrDupEntryForTest = mysqlErrDupEntry
)
