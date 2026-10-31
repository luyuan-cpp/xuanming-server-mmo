package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"

	"shared/idsegment"

	"github.com/luyuancpp/proto2mysql"
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
	// 实参 = SnapshotInsertArgsForTest(row) 再追加一次 row.SnapshotGuid。
	InsertSnapshotIfGuidAbsentLegacySQLForTest = insertSnapshotIfGuidAbsentLegacySQL
	// LookupSnapshotIDByGuidNzSQLForTest 按唯一键点查已有 id 的语句(EXPLAIN 回归用)。
	LookupSnapshotIDByGuidNzSQLForTest = lookupSnapshotIDByGuidNzSQL
)

// testRowID 是用例共用的行号计数器:单调递增,所以「id 小 = 先到」这类用例语义不变(生产的行号是打散的,没有这个性质)。
// 从当前纳秒起算,TiDB 用例共用一个库,重跑不会撞上次留下的行。
var testRowID atomic.Uint64

func init() { testRowID.Store(uint64(time.Now().UnixNano())) }

type rowIDsForTest struct{}

func (rowIDsForTest) Next(context.Context) (uint64, error) { return testRowID.Add(1), nil }

// RowIDsForTest 是 SnapshotStore.UseRowIDs 的用例来源。
var RowIDsForTest idsegment.Source = rowIDsForTest{}

type mixedRowIDsForTest struct{}

func (mixedRowIDsForTest) Next(context.Context) (uint64, error) {
	return idsegment.MixID(testRowID.Add(1)), nil
}

// MixedRowIDsForTest 发打散的行号,与生产同形:写入落在主键中间,不在末尾。锁相关的用例用它再跑一轮。
var MixedRowIDsForTest idsegment.Source = mixedRowIDsForTest{}

// SnapshotInsertArgsForTest 按 InsertSnapshotSQLForTest 的占位符顺序展开一行,行号取自 RowIDsForTest(实参第一个),
// 让用例不必再抄一份占位符顺序(抄了就会在加列时与生产漂移)。
func SnapshotInsertArgsForTest(row *SnapshotRow) []any {
	return snapshotInsertArgs(testRowID.Add(1), row)
}

// DeadlockRetriesForTest 返回本 store 因 1213 就地重跑 InsertSnapshotIfGuidAbsent / InsertSnapshotsIfGuidAbsent 的累计次数。
func (s *SnapshotStore) DeadlockRetriesForTest() uint64 { return s.deadlockRetries.Load() }

// BatchFallbacksForTest 返回批量路径撞 1062、整批转逐行的累计次数(钉住「先预读」:常态必须为 0)。
func (s *SnapshotStore) BatchFallbacksForTest() uint64 { return s.batchFallbacks.Load() }

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
// (mysqlErrDeadlock 在 mysql_errors.go、mysqlErrDupEntry 在 player_name_store.go),不在用例里再写一份字面量。
const (
	MySQLErrDeadlockForTest = mysqlErrDeadlock
	MySQLErrDupEntryForTest = mysqlErrDupEntry
)

// MigrateLockNameForTest 返回某个库的迁移锁名,让真库用例能扮演"另一个正在迁移的实例"占住同一把锁。
func MigrateLockNameForTest(dbName string) string { return migrateLockName(dbName) }

// 路由表(route_store.go)的 SQL:真库用例用与生产**同一段文本**扮演并发持锁者 / 造数据,
// 免得用例里抄一份语句、改生产时两边漂移。编号同规格 b.4。
const (
	RouteReadWritableSQLForTest      = routeReadWritableSQL      // R1
	RouteInsertNewSQLForTest         = routeInsertNewSQL         // R4
	RouteLockRowSQLForTest           = routeLockRowSQL           // R5
	RouteFillHomeSQLForTest          = routeFillHomeSQL          // R6a
	RouteFillHomePlacementSQLForTest = routeFillHomePlacementSQL // R6b
	RouteImportFillSQLForTest        = routeImportFillSQL        // R9
	RouteScanSQLForTest              = routeScanSQL              // R12
	RouteMetaSeedSQLForTest          = routeMetaSeedSQL          // R15
	RouteReconcileSeedSQLForTest     = routeReconcileSeedSQL     // R16
	RouteProbeShardCheckSQLForTest   = routeProbeShardCheckSQL   // P4
	// RouteDBNowMsSQLForTest 是库时钟毫秒的表达式:用例按库时钟取时刻,不用本机时钟。
	RouteDBNowMsSQLForTest = dbNowMsSQL
)

// RouteSelectRowsSQLForTest 返回 n 个 id 的批量读(R8 / R13;forUpdate 即 R8 的加锁形态),EXPLAIN 回归用。
func RouteSelectRowsSQLForTest(n int, forUpdate bool) string { return routeSelectRowsSQL(n, forUpdate) }

// PlayerRouteBootstrapDDLForTest 是 player_route 的建表语句:真库用例把其中 chk_route_shard 的模数换掉,
// 手工建出「按另一个模数建过的表」,证明建池探针 P5 认得出来(TestRouteStore_ProbeRejectsTableBuiltWithAnotherShardModulus)。
const PlayerRouteBootstrapDDLForTest = playerRouteBootstrapDDL

// RouteTiDBPrefixSplitPointsForTest 是 tidb-prepare 在 idx_route_home / idx_route_storage 上每个前缀值的切点数
// (真 TiDB 用例按它断言预切后的 Region 数,不写字面量)。
const RouteTiDBPrefixSplitPointsForTest = routeTiDBPrefixSplitPoints

// BootstrapRouteTablesOnlyForTest 只建路由表(按 routeTableMessages 的顺序——即表名升序——执行它们的 bootstrap DDL),
// 再跑 ensureRouteTables(形状守卫 + 种两张单行表的行)。给真 TiDB 用例用:全局库的整套迁移在 TiDB 上会因
// player_snapshot 的可空唯一键失败,用不了 NewMigratedDB。
func BootstrapRouteTablesOnlyForTest(ctx context.Context, db *sql.DB, dbName string) error {
	for _, m := range routeTableMessages() {
		name, _ := proto2mysql.TableNameFromDescriptor(m.ProtoReflect().Descriptor())
		ddl, ok := bootstrapTables[name]
		if !ok {
			return fmt.Errorf("route table %s has no bootstrap DDL", name)
		}
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("bootstrap route table %s: %w", name, err)
		}
	}
	return ensureRouteTables(ctx, db, dbName)
}

// LockRetriesForTest 返回本 store 因 1213 / 9007 重来的累计次数。
func (s *RouteStore) LockRetriesForTest() uint64 { return s.lockRetries.Load() }

// RouteProbeGuardSQLForTest / RouteProbeShardModulusSQLForTest 让真库用例用**同一段生产文本**扮演另一个正在探针的副本
// (TestRouteStore_ProbesQueueOnMetaGuard)。
const (
	RouteProbeGuardSQLForTest        = routeLockProbeGuardSQL
	RouteProbeShardModulusSQLForTest = routeProbeShardModulusSQL
)

// RouteLockMetaSQLForTest 是反向修库写事务的第一句(R18x,route_meta(id=1) FOR UPDATE):真库用例用同一段生产文本
// 扮演一个正在修库的事务,证明解冻的 R22 排在它后面(TestRouteStore_UnfreezeWaitsForRepairMetaLock)。
const RouteLockMetaSQLForTest = routeLockMetaSQL

// NewRouteStoreFromDBForTest 在调用方给的池上跑探针并装出 store(失败时关掉 db)。
// 真库用例拿一个**没有**会话变量的池(storetest 的 Raw)来证明探针 fail-closed。
func NewRouteStoreFromDBForTest(ctx context.Context, db *sql.DB, opts RouteStoreOptions) (*RouteStore, error) {
	return newRouteStoreFromDB(ctx, db, opts)
}
