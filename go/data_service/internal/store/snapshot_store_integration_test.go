//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// player_snapshot 的 source 列契约(设计 §2.0c 步骤 5)在真实表上的表现:
//   - snapshot_guid 去重是 check-then-insert,同一 guid 两次只落一行;
//   - GM 行(source=0,guid=0)可以有任意多条,与 source=1 行同表共存(索引非 UNIQUE);
//   - 现有 GM 读路径一律只看 source=0,source=1 行对它们不可见;
//   - source=1 行的 data 原样存取(消费者写的是 PlayerSnapshotEntry 原始字节)。

func newSnapshotStore(t *testing.T, db *storetest.DB) *store.SnapshotStore {
	t.Helper()
	ss, err := store.NewSnapshotStore(db.Cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	return ss
}

func sceneRow(playerID, guid, createdAt uint64, data []byte) *store.SnapshotRow {
	return &store.SnapshotRow{
		PlayerID:     playerID,
		ZoneID:       4,
		SnapshotType: 2, // C++ SnapshotTrigger 原值,不是 data_service 的 SnapshotType
		CreatedAt:    createdAt,
		Reason:       "cpp:SNAPSHOT_LOGIN",
		Operator:     store.SnapshotOperatorSceneNode,
		Data:         data,
		SnapshotGuid: guid,
		Source:       store.SnapshotSourceSceneKafka,
	}
}

func gmRow(playerID, createdAt uint64) *store.SnapshotRow {
	return &store.SnapshotRow{
		PlayerID:     playerID,
		ZoneID:       4,
		SnapshotType: 1,
		CreatedAt:    createdAt,
		Reason:       "gm manual",
		Operator:     "gm-1",
		Data:         []byte(`{"fields":{"player_database":"AQID"}}`),
		// SnapshotGuid / Source 不设:GM 路径落成 guid=0、source=0。
	}
}

func TestSnapshotStore_GuidDedupAndGmRowsCoexist(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	ctx := context.Background()

	raw := []byte{0x0a, 0x03, 'a', 'b', 'c', 0x12, 0x02, 0xff, 0x00} // 任意字节,含 NUL 与高位
	id1, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(100, 777, 1700000200, raw))
	require.NoError(t, err)
	require.True(t, inserted)
	require.NotZero(t, id1)

	// 同一 guid 重放(消费者提交 offset 前崩溃后的重放形态):不再插,返回已有行 id。
	id2, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(100, 777, 1700000200, raw))
	require.NoError(t, err)
	assert.False(t, inserted)
	assert.Equal(t, id1, id2)
	assert.Equal(t, int64(1), db.Count(t, "player_snapshot", "snapshot_guid = ?", 777))

	// guid=0 不能走 check-then-insert:那是 GM 行的形态,按 0 去重会把所有 GM 行当成重复。
	_, _, err = ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(100, 0, 1700000200, raw))
	require.Error(t, err)

	// 两条 GM 行 guid 都是 0,必须都能落(索引非 UNIQUE)。
	gm1, err := ss.InsertSnapshot(ctx, gmRow(100, 1700000100))
	require.NoError(t, err)
	gm2, err := ss.InsertSnapshot(ctx, gmRow(100, 1700000150))
	require.NoError(t, err)
	assert.NotEqual(t, gm1, gm2)
	assert.Equal(t, int64(2), db.Count(t, "player_snapshot", "snapshot_guid = 0 AND source = 0"))
	assert.Equal(t, int64(3), db.Count(t, "player_snapshot", "player_id = 100"))

	// source=1 行原样存取:字节与列值都不能被改写。
	var (
		gotData   []byte
		gotType   uint32
		gotSource uint32
		gotOp     string
	)
	require.NoError(t, db.Raw.QueryRow(
		`SELECT data, snapshot_type, source, operator FROM player_snapshot WHERE id = ?`, id1).
		Scan(&gotData, &gotType, &gotSource, &gotOp))
	assert.Equal(t, raw, gotData)
	assert.Equal(t, uint32(2), gotType)
	assert.Equal(t, store.SnapshotSourceSceneKafka, gotSource)
	assert.Equal(t, store.SnapshotOperatorSceneNode, gotOp)
}

func TestSnapshotStore_GmReadersExcludeSceneRows(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	ctx := context.Background()

	// 玩家 100:两条 GM 行 + 一条**更新**的 scene 行(created_at 最大,故意诱导"最新"查询选中它)。
	gm1, err := ss.InsertSnapshot(ctx, gmRow(100, 1700000100))
	require.NoError(t, err)
	gm2, err := ss.InsertSnapshot(ctx, gmRow(100, 1700000150))
	require.NoError(t, err)
	sceneID, _, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(100, 777, 1700000999, []byte("raw")))
	require.NoError(t, err)
	// 玩家 200:只有 scene 行。
	_, _, err = ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(200, 778, 1700000300, []byte("raw2")))
	require.NoError(t, err)

	// GetSnapshotByID:scene 行按 id 也查不到(rollback 会对它 json.Unmarshal 失败)。
	got, err := ss.GetSnapshotByID(ctx, sceneID)
	require.NoError(t, err)
	assert.Nil(t, got, "source=1 row must be invisible to GetSnapshotByID")
	got, err = ss.GetSnapshotByID(ctx, gm1)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, store.SnapshotSourceDataService, got.Source)
	assert.Equal(t, uint64(0), got.SnapshotGuid)

	// GetLatestSnapshotBefore:最新的是 scene 行,但必须返回最新的 GM 行。
	latest, err := ss.GetLatestSnapshotBefore(ctx, 100, 1800000000)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, gm2, latest.ID)
	assert.Equal(t, store.SnapshotSourceDataService, latest.Source)
	// 玩家 200 只有 scene 行 → GM 视角没有快照。
	latest, err = ss.GetLatestSnapshotBefore(ctx, 200, 1800000000)
	require.NoError(t, err)
	assert.Nil(t, latest)

	// ListSnapshotsMeta / ListSnapshots:只列 GM 行。
	metas, err := ss.ListSnapshotsMeta(ctx, 100, 0, 10)
	require.NoError(t, err)
	require.Len(t, metas, 2)
	for _, m := range metas {
		assert.Equal(t, store.SnapshotSourceDataService, m.Source)
	}
	assert.Equal(t, gm2, metas[0].ID, "newest first")
	list, err := ss.ListSnapshots(ctx, 200, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, list)

	// GetSnapshotPlayerIDsByZone:zone 4 里只有玩家 100 有 GM 快照;200 只有 scene 行,不算。
	ids, err := ss.GetSnapshotPlayerIDsByZone(ctx, 4, 1800000000)
	require.NoError(t, err)
	assert.Equal(t, []uint64{100}, ids)

	// scene 专用读路径看得到 source=1 行,且不带 blob 只带长度。
	scene, err := ss.ListSceneSnapshotsByPlayer(ctx, 100, 10)
	require.NoError(t, err)
	require.Len(t, scene, 1)
	assert.Equal(t, uint64(777), scene[0].SnapshotGuid)
	assert.Equal(t, sceneID, scene[0].ID)
	assert.Equal(t, uint32(2), scene[0].Trigger)
	assert.Equal(t, uint64(1700000999), scene[0].CreatedAt)
	assert.Equal(t, uint32(3), scene[0].DataSizeBytes)

	// rollback_audit_log 的 orphans_cleaned 列真的能写(以前 proto 缺这列)。
	require.NoError(t, ss.InsertAuditLog(ctx, &store.AuditLogRow{
		ZoneID: 4, RollbackType: 2, TargetTime: 1700000100, PlayersAffected: 1, OrphansCleaned: 3,
		Reason: "it", Operator: "gm-1", CreatedAt: 1700001000,
	}))
	var orphans uint32
	require.NoError(t, db.Raw.QueryRow(`SELECT orphans_cleaned FROM rollback_audit_log WHERE zone_id = 4`).Scan(&orphans))
	assert.Equal(t, uint32(3), orphans)
}

// ── 审计 #11:并发写者在 idx(snapshot_guid) 同一段间隙上互等插入意向锁,1213 由 InsertSnapshotIfGuidAbsent 就地吸收 ──

// legacyInsertArgs 按 store.InsertSnapshotIfGuidAbsentLegacySQLForTest 的占位符顺序展开一行:
// 九个列值(顺序由生产代码的 store.SnapshotInsertArgsForTest 给,用例不再拄一份)+ 子查询里再绑一次 guid。
func legacyInsertArgs(r *store.SnapshotRow) []any {
	return append(store.SnapshotInsertArgsForTest(r), r.SnapshotGuid)
}

// requireGlobalRepeatableRead:store 的 DSN 不设隔离级别,走库的全局默认。本组用例证的是 RR(生产默认)下的
// 间隙锁行为;库被全局改成 RC 时这个环根本不存在(去重也同时失效,见 InsertSnapshotIfGuidAbsent),只能跳过。
func requireGlobalRepeatableRead(t *testing.T, db *storetest.DB) {
	t.Helper()
	var level string
	require.NoError(t, db.Raw.QueryRow(`SELECT @@GLOBAL.transaction_isolation`).Scan(&level))
	if level != "REPEATABLE-READ" {
		t.Skipf("库的全局隔离级别是 %s:本场景的间隙锁只在 REPEATABLE-READ 下存在,跳过(不代表通过)", level)
	}
}

// requireDeadlockDetect:本组用例靠 InnoDB **当场**判出 1213 来观察环(并验证被吸收 / 不出现)。库上关掉了
// innodb_deadlock_detect 时,同一个环不会报 1213,而是挂满 innodb_lock_wait_timeout(默认 50s)后报 1205 ——
// 被测代码刻意不就地重试 1205,用例会以"没有吸收 1213"这种指错方向的消息判红,且耗时接近 ctx 上限。
// 这是环境前提不成立,不是产品缺陷,所以跳过(不代表通过)。
//
// 值按字符串读再判:MySQL 返回 1/0,个别代理或兼容库可能回 ON/OFF;读不到这个变量(不是 InnoDB / MySQL)
// 同样说明前提不成立。只有明确是"开"才继续,其余一律跳过并写明读到了什么,不猜。
func requireDeadlockDetect(t *testing.T, raw *sql.DB) {
	t.Helper()
	var v sql.NullString
	if err := raw.QueryRow(`SELECT @@GLOBAL.innodb_deadlock_detect`).Scan(&v); err != nil {
		t.Skipf("读不到 innodb_deadlock_detect(%v):无法确认 InnoDB 会当场判出死锁,跳过(不代表通过)", err)
	}
	switch strings.ToUpper(strings.TrimSpace(v.String)) {
	case "1", "ON":
		return
	default:
		t.Skipf("innodb_deadlock_detect=%q(未开启):环会表现为 50s 后的 1205 而不是 1213,本场景的判据不成立,跳过(不代表通过)", v.String)
	}
}

// lockWaitersQuery 数本库指定表上正在排队等锁的事务数(别的库、别的表不算)。
// 手工编排死锁交错的用例(本文件与 player_name_store_integration_test.go)共用。
const lockWaitersQuery = `
	SELECT COUNT(DISTINCT w.REQUESTING_ENGINE_TRANSACTION_ID)
	FROM performance_schema.data_lock_waits w
	JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
	WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = ?`

// errLockWaitsUnobservable:测试账号读不了 performance_schema 的锁视图,或实例上根本没有这两张表。
// 这时手工编排做不出来 —— 那不是产品缺陷,由 skipOrFailOnLockWait 跳过(不代表通过)。
// 只有 isLockWaitsUnobservable 认定的错误号才归到这里,见 lockWaitsQueryError。
var errLockWaitsUnobservable = errors.New("读 performance_schema.data_lock_waits 失败")

// awaitLockWaiters 轮询,直到 table 上至少有 want 个事务在排队等锁,或 budget 用尽(返回错误)。
// 靠轮询**观察**,不靠 sleep 估时间:两次轮询之间的短停顿只是不去空转打爆 MySQL,
// 条件不满足就一直等到预算用尽并报错,不会"睡够了就当它们已经在等"。
// 查询失败时的定性见 lockWaitsQueryError:只有权限 / 对象缺失类错误算"不可观测",ctx 结束与其余错误都判红。
//
// 返回值刻意只有 error(不带观察到的等待者数):player_name_store_integration_test.go 也在用这个助手,
// 它的调用点不归本组改。要加返回值时两处一起改。
func awaitLockWaiters(ctx context.Context, raw *sql.DB, table string, want int, budget time.Duration) error {
	const pollEvery = 10 * time.Millisecond
	deadline := time.Now().Add(budget)
	for {
		var waiters int
		if err := raw.QueryRowContext(ctx, lockWaitersQuery, table).Scan(&waiters); err != nil {
			return lockWaitsQueryError(ctx, err)
		}
		if waiters >= want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%v 内只看到 %d/%d 个事务排进 %s 的锁等待队列", budget, waiters, want, table)
		}
		time.Sleep(pollEvery)
	}
}

// lockWaitsQueryError 给 lockWaitersQuery 的失败定性:是"环境看不见锁等待"(可跳过),还是"编排出错"(判红)。
//   - 用例 ctx 已结束(预算用尽 / 被取消):带出 ctx 错误(errors.Is 可认出 context.DeadlineExceeded / Canceled),判红。
//     这时查询失败只是结果,原因是编排卡住或超了预算。先看 ctx.Err() 而不是先看错误本身,是因为 ctx 结束时驱动
//     报出来的形态不固定(可能是 ctx 错误,也可能是 invalid connection 之类),以 ctx 的状态为准。
//   - MySQL 权限 / 对象缺失类错误(isLockWaitsUnobservable):归为 errLockWaitsUnobservable。
//   - 其余一律判红:断连、SQL 写错、服务端内部错误都说明编排本身坏了。若也当成"不可观测",
//     卡住就会被报成 SKIP —— 该红的时候不红(2026-09-21 复审登记的缺陷,这里修的就是它)。
//
// 与 go/trade/internal/data/listing_repo_integration_test.go、
// go/friend/internal/data/friend_guard_lock_order_mysql_test.go 的同名助手同一口径,改一处要同步其余两处。
func lockWaitsQueryError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("轮询锁等待时用例 ctx 已结束(编排卡住或超出预算,不是读不了 performance_schema): %w; 查询错误: %v", ctxErr, err)
	}
	if isLockWaitsUnobservable(err) {
		return fmt.Errorf("%w: %v", errLockWaitsUnobservable, err)
	}
	return fmt.Errorf("查询 performance_schema 锁等待失败,且不是权限 / 对象缺失类错误,按编排失败判红: %w", err)
}

// isLockWaitsUnobservable 只按驱动给出的 MySQL 错误号判定"账号或实例不支持观察锁等待" —— 都是环境问题,
// 与被测的锁行为无关。刻意不匹配错误文本,也不把"查询失败"整体归进来。
// 注意 performance_schema=OFF 时表仍在、只是恒为空,查询不报错,会走到 awaitLockWaiters 的"等待者没到齐"而判红;
// 本仓的 MySQL 8 默认开启,真遇到时先检查 SELECT @@performance_schema。
func isLockWaitsUnobservable(err error) bool {
	var myErr *mysql.MySQLError
	if !errors.As(err, &myErr) {
		return false
	}
	switch myErr.Number {
	case 1044, // ER_DBACCESS_DENIED_ERROR:对 performance_schema 库整体无权
		1142, // ER_TABLEACCESS_DENIED_ERROR:对 data_lock_waits / data_locks 没有 SELECT 权限
		1143, // ER_COLUMNACCESS_DENIED_ERROR:只授了部分列的 SELECT 权限
		1146, // ER_NO_SUCH_TABLE:实例没有这两张表(MySQL 8.0 以前、MariaDB 等)
		1227: // ER_SPECIFIC_ACCESS_DENIED_ERROR:缺某项全局权限
		return true
	}
	return false
}

// skipOrFailOnLockWait:读不了 performance_schema 就跳过;其余错误(等待者没到齐、ctx 结束、
// 非权限类查询错误)一律判红 —— 编排失败本身就说明锁行为与推演不符,或者用例卡住了。
func skipOrFailOnLockWait(t *testing.T, err error, expectation string) {
	t.Helper()
	if errors.Is(err, errLockWaitsUnobservable) {
		t.Skipf("无法编排本场景(测试账号需要 performance_schema 的 SELECT 权限),跳过(不代表通过): %v", err)
	}
	t.Fatalf("夹具编排失败:%v —— %s", err, expectation)
}

// TestLockWaitsQueryErrorClassification 钉住 lockWaitsQueryError 的定性。不连库:带 integration 标签、不设 DSN 也照跑。
// 只有权限 / 对象缺失类错误号算"不可观测";ctx 结束与其余错误都必须判红 —— 修复前这里把 ctx 超时 / 取消
// 也包成了"不可观测",卡住的编排会被报成 SKIP,该红的时候不红。
func TestLockWaitsQueryErrorClassification(t *testing.T) {
	live := context.Background()
	for _, errNo := range []uint16{1044, 1142, 1143, 1146, 1227} {
		err := lockWaitsQueryError(live, fmt.Errorf("中间包了一层: %w", &mysql.MySQLError{Number: errNo}))
		assert.ErrorIs(t, err, errLockWaitsUnobservable, "错误号 %d 是权限 / 对象缺失类,应归为不可观测", errNo)
	}
	for _, cause := range []error{
		&mysql.MySQLError{Number: 1213},
		&mysql.MySQLError{Number: 1062},
		mysql.ErrInvalidConn,
		errors.New("任意非 MySQL 错误"),
	} {
		err := lockWaitsQueryError(live, cause)
		assert.NotErrorIs(t, err, errLockWaitsUnobservable, "%v 不是权限 / 对象缺失类错误,必须判红", cause)
		assert.ErrorIs(t, err, cause, "判红时要带出原始错误")
	}

	// ctx 已结束时,不论驱动报的是什么形态,都必须判红并带出 ctx 的原因。
	for _, tc := range []struct {
		name string
		ctx  func() context.Context
		want error
	}{
		{"已取消", func() context.Context {
			c, cancel := context.WithCancel(context.Background())
			cancel()
			return c
		}, context.Canceled},
		{"已超时", func() context.Context {
			c, cancel := context.WithTimeout(context.Background(), -time.Second)
			defer cancel()
			return c
		}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 连"权限类"错误号也不许把 ctx 结束盖过去:先看 ctx 才分得清"看不见"与"卡住了"。
			err := lockWaitsQueryError(tc.ctx(), &mysql.MySQLError{Number: 1142})
			assert.ErrorIs(t, err, tc.want, "ctx 已结束时必须带出 ctx 的原因")
			assert.NotErrorIs(t, err, errLockWaitsUnobservable, "ctx 已结束不是「读不了 performance_schema」,不许报成 SKIP")
		})
	}
}

// TestSnapshotStore_ConcurrentWritersNeverSurfaceDeadlock:两个写者成对并发落快照,偶数轮同 guid、奇数轮不同 guid
// (都大于表内已有的最大 guid,正是 SnowFlake 单调递增时的常态)。
// 无论走哪条写入语句(有否唯一键),调用方看到的必须全部是 err == nil,并且同一 guid 恰好一行。
//
// 每轮用 barrier 同时放行两个写者、等两者都结束再进下一轮:胜者不会紧接着再写下一条,所以被牺牲方的一次
// 重跑必然收敛(见 InsertSnapshotIfGuidAbsent 的收敛论证),对 err 的断言是确定的。某一轮是否真的撞上 1213
// 取决于调度,只记日志不断言 —— 确定性的证据分在两处:
// 唯一键路径看 TestSnapshotStore_SameGuidQueueDoesNotDeadlock 与
// TestSnapshotStore_DistinctGuidDoesNotWaitOnUncommittedNeighbour,退路看
// TestSnapshotStore_LegacyGapDeadlockIsAbsorbedInPlace。
func TestSnapshotStore_ConcurrentWritersNeverSurfaceDeadlock(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	requireDeadlockDetect(t, db.Raw)
	ss := newSnapshotStore(t, db)
	if !ss.UniqueGuidKeyForTest() {
		// 退路那条语句的去重靠 RR 的加锁读;库被全局改成 RC 时它根本不成立。
		// 唯一键路径不依赖隔离级别,所以这条前提只在退路上检查。
		requireGlobalRepeatableRead(t, db)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		rounds    = 200
		baseGuid  = uint64(10_000)
		createdAt = uint64(1700000000)
	)
	_, _, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(1, baseGuid, createdAt, []byte("seed")))
	require.NoError(t, err)

	type result struct {
		id       uint64
		inserted bool
		err      error
	}
	retriesBefore := ss.DeadlockRetriesForTest()
	for r := 0; r < rounds; r++ {
		sameGuid := r%2 == 0
		guids := [2]uint64{baseGuid + uint64(2*r+1), baseGuid + uint64(2*r+1)}
		if !sameGuid {
			guids[1]++
		}

		var results [2]result
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // 一起放行,尽量把两条语句压进同一个锁窗口
				id, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(uint64(100+i), guids[i], createdAt, []byte("raw")))
				results[i] = result{id: id, inserted: inserted, err: err}
			}()
		}
		close(start)
		wg.Wait()

		for i, res := range results {
			require.NoErrorf(t, res.err, "第 %d 轮写者 %d(guid=%d)把错误冒泡给了调用方:1213 没有被就地吸收", r, i, guids[i])
		}
		if sameGuid {
			require.Truef(t, results[0].inserted != results[1].inserted, "第 %d 轮同 guid:应恰好一个 inserted=true", r)
			require.Equalf(t, results[0].id, results[1].id, "第 %d 轮同 guid:重复方应回出已存在那行的 id", r)
		} else {
			require.Truef(t, results[0].inserted && results[1].inserted, "第 %d 轮不同 guid:两个都应插入", r)
		}
	}
	t.Logf("%d 轮里就地重跑吸收的 1213 共 %d 次", rounds, ss.DeadlockRetriesForTest()-retriesBefore)

	var dupGuids int64
	require.NoError(t, db.Raw.QueryRow(`SELECT COUNT(*) FROM (
		SELECT snapshot_guid FROM player_snapshot WHERE source = 1 GROUP BY snapshot_guid HAVING COUNT(*) > 1) d`).Scan(&dupGuids))
	assert.Equal(t, int64(0), dupGuids, "同一 guid 不许落成两行 source=1")
	// 种子 1 行 + 同 guid 轮每轮 1 行 + 不同 guid 轮每轮 2 行。
	assert.Equal(t, int64(1+rounds/2+rounds), db.Count(t, "player_snapshot", "source = 1"))
}

// TestSnapshotStore_LegacyGapDeadlockIsAbsorbedInPlace 按审计 #11 的交错一步一步摆出那个间隙环,
// 证明 1213 真的发生了、并且被 InsertSnapshotIfGuidAbsent 就地吸收,调用方看到的只是一次成功插入。
//
// 这条环已经被 uk_snapshot_guid_nz 消掉了(普通 INSERT 不做加锁读),所以本用例把 store
// 强行拉回**退路**那条语句再测 —— 退路不是死代码:迁移还没跑、DDL 刚回滚、以及对端是 TiDB
// 时都走它。同时它也是"为什么要做唯一键"的证据:注意第 3 / 4 步里两个写者的 guid
// **完全不同**(3000 与 4000),本来毫无冲突,却因为同在一段间隙上而互杀。
//
// 编排(库的全局隔离级别必须是 RR,即生产默认):
//  1. 表里先有 guid=1000(已提交),新 guid 都比它大 —— 全部落在 (1000, +∞) 这段间隙,SnowFlake 单调递增的常态。
//  2. 写者 A = 显式 RR 事务,执行生产同一条语句插 guid=2000 且**先不提交**:持有 idx(snapshot_guid) supremum 上的
//     S 间隙锁。再往 A 里塞几条 GM 行加重它的事务权重,让 InnoDB 选牺牲者时确定地选 B(InnoDB 优先回滚改动行数少的一方)。
//  3. 写者 B = 被测的 store.InsertSnapshotIfGuidAbsent(guid=3000):子查询同样拿到 supremum 上的 S 间隙锁(与 A 兼容),
//     插索引项时的插入意向锁被 A 的 S 间隙锁挡住 —— 在 performance_schema.data_lock_waits 里**看见**它排队才走下一步。
//  4. A 再执行同一条语句插 guid=4000:插入意向锁被 B 的 S 间隙锁挡住 → 成环,B 被回滚、收到 1213。
//  5. B 在函数内部短退避后重跑:此后只会单向等 A;提交 A 之后 B 必须成功插入。
//
// 判据:B 返回 err == nil 且 inserted=true;就地重跑计数至少涨 1(证明环确实成了,而不是这一轮根本没撞上);
// 四个 guid 各恰好一行。把就地重试删掉(1213 冒泡)本用例稳定变红。
// (InnoDB 的取锁细节按手册推演,以真库上跑出来的结果为准;编排步骤与推演不符时用例判红并说明卡在哪一步。)
func TestSnapshotStore_LegacyGapDeadlockIsAbsorbedInPlace(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	requireGlobalRepeatableRead(t, db)
	requireDeadlockDetect(t, db.Raw)
	ss := newSnapshotStore(t, db)
	// 本用例测的是退路那条语句的间隙环,与这张表上唯一键存在与否无关(库里有键也不影响
	// NOT EXISTS 子查询在 idx(snapshot_guid) 上取 S 间隙锁)。所以直接强行拉回退路。
	ss.ForceLegacyGuidDedupeForTest()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const (
		createdAt  = uint64(1700000000)
		waitBudget = 10 * time.Second
		weightRows = 8
	)

	// 1. 已提交的旧快照。
	_, _, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(1, 1000, createdAt, []byte("seed")))
	require.NoError(t, err)

	// 2. 写者 A。任何提前失败的路径都必须放掉 A 的锁,否则 B 会一直挂到 ctx 超时;Commit 之后的 Rollback 无害。
	txA, err := db.Raw.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	require.NoError(t, err)
	defer txA.Rollback()
	res, err := txA.ExecContext(ctx, store.InsertSnapshotIfGuidAbsentLegacySQLForTest,
		legacyInsertArgs(sceneRow(2, 2000, createdAt, []byte("a1")))...)
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "夹具:A 应插入 guid=2000")
	for i := 0; i < weightRows; i++ {
		// GM 行 guid=0,落在 idx(snapshot_guid) 的最左端,不碰 supremum 那段间隙,只用来加重 A 的事务权重。
		_, err := txA.ExecContext(ctx,
			`INSERT INTO player_snapshot (player_id, zone_id, snapshot_type, created_at, reason, operator, data, snapshot_guid, source)
			 VALUES (?, 4, 1, ?, 'weight', 'it', '{}', 0, 0)`, 900+i, createdAt)
		require.NoError(t, err)
	}

	// 3. 写者 B。
	type result struct {
		id       uint64
		inserted bool
		err      error
	}
	retriesBefore := ss.DeadlockRetriesForTest()
	doneB := make(chan result, 1)
	go func() {
		id, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(3, 3000, createdAt, []byte("b")))
		doneB <- result{id: id, inserted: inserted, err: err}
	}()
	if err := awaitLockWaiters(ctx, db.Raw, "player_snapshot", 1, waitBudget); err != nil {
		_ = txA.Rollback()
		<-doneB
		skipOrFailOnLockWait(t, err, "B 的插入意向锁应被 A 在 idx(snapshot_guid) supremum 上的 S 间隙锁挡住")
	}

	// 4. A 插 guid=4000:成环的时刻。
	if _, err := txA.ExecContext(ctx, store.InsertSnapshotIfGuidAbsentLegacySQLForTest,
		legacyInsertArgs(sceneRow(4, 4000, createdAt, []byte("a2")))...); err != nil {
		_ = txA.Rollback()
		<-doneB
		t.Fatalf("A 的第二条语句失败:%v —— 若是 1213,说明 InnoDB 选了 A 当牺牲者(夹具\"A 更重\"的前提不成立),"+
			"本轮没有测到 B 的就地重试", err)
	}

	// 5. 提交 A,B 的重跑只会单向等它。
	require.NoError(t, txA.Commit())
	b := <-doneB
	require.NoError(t, b.err, "B 把错误冒泡给了调用方:InsertSnapshotIfGuidAbsent 没有就地吸收 1213")
	assert.True(t, b.inserted, "guid=3000 此前不存在,B 应插入")
	assert.NotZero(t, b.id)
	assert.GreaterOrEqual(t, ss.DeadlockRetriesForTest()-retriesBefore, uint64(1),
		"B 成功了但就地重跑计数没涨:这一轮没有真的成环,编排与推演不符(见本用例头注)")

	for _, g := range []uint64{1000, 2000, 3000, 4000} {
		assert.Equalf(t, int64(1), db.Count(t, "player_snapshot", "snapshot_guid = ?", g), "guid=%d 应恰好一行", g)
	}
}

// ── 审计 #11 第二步:可空唯一列(snapshot_guid_nz + uk_snapshot_guid_nz)的真库回归 ──────────

// requireGuidUniqueKey:唯一键不在就跳过。探不到的合法原因只有两个 —— 对端是 TiDB
// (官方文档不支持 ALTER 加 STORED 生成列),或迁移没跑成。两者都不是"本用例失败",但也**不代表通过**。
func requireGuidUniqueKey(t *testing.T, ss *store.SnapshotStore) {
	t.Helper()
	if !ss.UniqueGuidKeyForTest() {
		t.Skipf("%s.%s 不存在(TiDB 或迁移未生效):本用例证的是唯一键路径,跳过(不代表通过)",
			store.PlayerSnapshotTableName, store.SnapshotGuidUniqueKey)
	}
}

// dropGuidUniqueKey 把表拉回"唯一键还没加"的形态(键先删、列后删:列上挂着键时删不掉)。
// 用来造"迁移还没跑 / DDL 刚回滚"的起点 —— 这也正是回滚方案的两条语句,顺手证明了它们可执行。
func dropGuidUniqueKey(t *testing.T, ctx context.Context, db *storetest.DB) {
	t.Helper()
	for _, stmt := range []string{
		fmt.Sprintf("ALTER TABLE `%s` DROP INDEX `%s`", store.PlayerSnapshotTableName, store.SnapshotGuidUniqueKey),
		fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `%s`", store.PlayerSnapshotTableName, store.SnapshotGuidNzColumn),
	} {
		_, err := db.Raw.ExecContext(ctx, stmt)
		require.NoErrorf(t, err, "回滚语句执行失败(回滚方案就是这两条,它们必须随时可执行): %s", stmt)
	}
}

// snapshotGuidNzOf 读一行的生成列值(GM 行是 NULL)。
func snapshotGuidNzOf(t *testing.T, ctx context.Context, db *storetest.DB, id uint64) sql.NullInt64 {
	t.Helper()
	var nz sql.NullInt64
	require.NoError(t, db.Raw.QueryRowContext(ctx,
		fmt.Sprintf("SELECT `%s` FROM `%s` WHERE id = ?", store.SnapshotGuidNzColumn, store.PlayerSnapshotTableName),
		id).Scan(&nz))
	return nz
}

// rawInsertSnapshot 用生产的普通 INSERT 文本直接写一行(绕过 store 的去重),返回自增 id。
// 造历史重复行用:它模拟的正是"两个写者都插进去了"那个已经发生过的事实。
func rawInsertSnapshot(t *testing.T, ctx context.Context, db *storetest.DB, row *store.SnapshotRow) uint64 {
	t.Helper()
	res, err := db.Raw.ExecContext(ctx, store.InsertSnapshotSQLForTest, store.SnapshotInsertArgsForTest(row)...)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return uint64(id)
}

// snapshotIDsOfGuid 返回某个 guid 现有的全部 id(升序)。
func snapshotIDsOfGuid(t *testing.T, ctx context.Context, db *storetest.DB, guid uint64) []uint64 {
	t.Helper()
	rows, err := db.Raw.QueryContext(ctx,
		fmt.Sprintf("SELECT id FROM `%s` WHERE snapshot_guid = ? ORDER BY id", store.PlayerSnapshotTableName), guid)
	require.NoError(t, err)
	defer rows.Close()
	var ids []uint64
	for rows.Next() {
		var id uint64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

// TestSnapshotStore_GuidUniqueKeyIsEnforcedByTheDatabase:迁移之后,"同一 guid 只落一行"由**库**兜住,
// 不再依赖任何一条应用层语句的写法。
//
// 四条断言对着四种事故形态:
//   - 生成列与唯一键都在 —— 否则整个根治等于没做,而写路径(普通 INSERT)会静默写重复行;
//   - guid≠0 的行 nz = guid、GM 行 nz = NULL —— NULLIF 写错(比如写成 IFNULL)时,GM 行会挤在同一个值上;
//   - 直接插重复 guid 必须被库以 1062 拒掉 —— 这是"库兜住"的直接证据;
//   - GM 行(guid=0)可以有任意多条 —— 这是当初不敢建 UNIQUE 的唯一障碍,必须证明它被解开了。
func TestSnapshotStore_GuidUniqueKeyIsEnforcedByTheDatabase(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	requireGuidUniqueKey(t, ss)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const createdAt = uint64(1700000000)

	sceneID, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(10, 8888, createdAt, []byte("raw")))
	require.NoError(t, err)
	require.True(t, inserted)
	nz := snapshotGuidNzOf(t, ctx, db, sceneID)
	require.True(t, nz.Valid, "guid≠0 的行生成列不该是 NULL,否则唯一键约束不到它")
	assert.Equal(t, int64(8888), nz.Int64)

	gmID, err := ss.InsertSnapshot(ctx, gmRow(10, createdAt))
	require.NoError(t, err)
	assert.False(t, snapshotGuidNzOf(t, ctx, db, gmID).Valid,
		"GM 行(guid=0)的生成列必须是 NULL —— 唯一键不约束 NULL,GM 行才能有任意多条")

	// 直接插同一个 guid:必须被唯一键以 1062 拒掉(而不是安静落成第二行)。
	_, err = db.Raw.ExecContext(ctx, store.InsertSnapshotSQLForTest,
		store.SnapshotInsertArgsForTest(sceneRow(11, 8888, createdAt+1, []byte("dup")))...)
	require.Error(t, err, "同一 guid 的第二行必须被库拒掉")
	var myErr *mysql.MySQLError
	require.ErrorAs(t, err, &myErr)
	assert.EqualValues(t, store.MySQLErrDupEntryForTest, myErr.Number, "应是 1062(撞唯一键),got %v", err)
	assert.Equal(t, int64(1), db.Count(t, store.PlayerSnapshotTableName, "snapshot_guid = ?", 8888))

	// GM 行任意多条:再写三条,全部落库。
	for i := 0; i < 3; i++ {
		_, err := ss.InsertSnapshot(ctx, gmRow(10, createdAt+uint64(i)+1))
		require.NoErrorf(t, err, "第 %d 条 GM 行被拒:唯一键把 guid=0 也约束了(NULLIF 没生效?)", i+2)
	}
	assert.Equal(t, int64(4), db.Count(t, store.PlayerSnapshotTableName, "snapshot_guid = 0 AND source = 0"))
}

// TestSnapshotStore_GuidLookupUsesUniqueKey:撞 1062 之后"查已有 id"那条语句必须是**完整唯一键等值点查**。
// 锁与扫描量只取决于执行计划,并发用例只能按概率撞上,而 EXPLAIN 每次都答得出来。
// 对生产代码里**同一个 SQL 常量**做 EXPLAIN(不在用例里另抄一份,否则两边漂移时本用例就失去意义)。
//
// key_len 也要断言:bigint unsigned 可空 = 8 字节 + 1 字节 NULL 标记 = 9。只看 key 不看 key_len,
// 有人把语句改成"前缀 / 范围"形态时(比如加个 created_at 条件再按 guid 范围找)本用例照样绿。
func TestSnapshotStore_GuidLookupUsesUniqueKey(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	requireGuidUniqueKey(t, ss)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// 先落一行:命中已存在的行时计划才显示真实的索引。
	_, _, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(20, 9001, 1700000000, []byte("raw")))
	require.NoError(t, err)

	stmt := strings.Replace(store.LookupSnapshotIDByGuidNzSQLForTest, "?", "9001", 1)
	require.NotContains(t, stmt, "?", "占位符数量与参数不符")

	plan := explainFirstRow(t, ctx, db.Raw, stmt)
	t.Logf("按 guid 查已有 id 的执行计划: %v", plan)
	assert.Equal(t, store.SnapshotGuidUniqueKey, plan["key"],
		"必须走 %s 的等值点查:走别的索引或全表扫说明语句被改成了非点查形态", store.SnapshotGuidUniqueKey)
	assert.Equal(t, "9", plan["key_len"],
		"key_len 必须用满(bigint unsigned 8 + 可空 1 = 9):不足说明退化成了前缀 / 范围访问")
	assert.Equal(t, "const", plan["type"], "唯一键等值查一行,type 应是 const")
}

// explainFirstRow 跑一条 EXPLAIN FORMAT=TRADITIONAL,返回第一行的列名 → 值(NULL 记为空串)。
// 这些语句都只涉及一张表,第一行就是它的计划。
func explainFirstRow(t *testing.T, ctx context.Context, raw *sql.DB, stmt string) map[string]string {
	t.Helper()
	rows, err := raw.QueryContext(ctx, "EXPLAIN FORMAT=TRADITIONAL "+stmt)
	require.NoErrorf(t, err, "EXPLAIN 失败: %s", stmt)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	require.Truef(t, rows.Next(), "EXPLAIN 没有返回任何行: %s", stmt)
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	require.NoError(t, rows.Scan(ptrs...))
	plan := make(map[string]string, len(cols))
	for i, c := range cols {
		plan[c] = vals[i].String
	}
	return plan
}

// TestSnapshotStore_SameGuidConcurrentWritersLandOneRow:同一 guid 的 N 个并发写者,恰好一行落库,
// 恰好一个 inserted=true,其余全部拿到**同一个** id 且 err == nil。
//
// 这是消费者重放(提交 offset 前崩溃)与滚动更新窗口里两个实例同时消费的形态。判据是终局的,与调度无关:
// 谁赢不确定,但"只有一行、其余都看到那一行"必须确定。
func TestSnapshotStore_SameGuidConcurrentWritersLandOneRow(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	requireGuidUniqueKey(t, ss)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		writers   = 16
		guid      = uint64(12345)
		createdAt = uint64(1700000000)
	)

	type result struct {
		id       uint64
		inserted bool
		err      error
	}
	results := make([]result, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(uint64(300+i), guid, createdAt, []byte("raw")))
			results[i] = result{id: id, inserted: inserted, err: err}
		}()
	}
	close(start)
	wg.Wait()

	var insertedCount int
	var winnerID uint64
	for i, r := range results {
		require.NoErrorf(t, r.err, "写者 %d 把错误冒泡给了调用方(1062 应当成去重结果、1213 应被就地吸收)", i)
		require.NotZerof(t, r.id, "写者 %d 既没插入也没查到已有行的 id", i)
		if r.inserted {
			insertedCount++
			winnerID = r.id
		}
	}
	assert.Equal(t, 1, insertedCount, "恰好一个写者应 inserted=true")
	for i, r := range results {
		assert.Equalf(t, winnerID, r.id, "写者 %d 回出的 id 与实际落库那行不一致", i)
	}
	assert.Equal(t, int64(1), db.Count(t, store.PlayerSnapshotTableName, "snapshot_guid = ?", guid))
	t.Logf("%d 个同 guid 写者,就地吸收的 1213 共 %d 次", writers, ss.DeadlockRetriesForTest())
}

// TestSnapshotStore_DistinctGuidDoesNotWaitOnUncommittedNeighbour 是"唯一键消掉了那一类环"的**确定性**证据。
//
// 编排:A = 显式事务,用生产的普通 INSERT 插 guid=5000 且不提交;B = 被测的 InsertSnapshotIfGuidAbsent(6000),
// guid 比 A 的大,落在同一段"表内最大 guid 之后"的间隙里 —— 正是老写法互杀的那段。
//
// 老写法下 B 的 NOT EXISTS 子查询会在这段间隙上取 S 锁,而 A 已经持有这段间隙的锁,B 必须等到 A 提交;
// 普通 INSERT 只在间隙上取**插入意向锁**,而插入意向锁之间相容,所以 B 必须**当场**成功。
// 判据就是这个:给 B 一个短 ctx(noWaitBudget),它一旦被挡住就会以 ctx 超时判红,而不是悄悄慢下来。
// 这条用例不需要 performance_schema,也不依赖 InnoDB 选谁当牺牲者,任何环境都确定。
func TestSnapshotStore_DistinctGuidDoesNotWaitOnUncommittedNeighbour(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	requireGuidUniqueKey(t, ss)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const (
		createdAt = uint64(1700000000)
		// noWaitBudget 远大于一次本地 INSERT 的耗时,又远小于 innodb_lock_wait_timeout(默认 50s):
		// B 只要真的排上队,就一定在这个预算内超时,而不会因为机器慢而误报。
		noWaitBudget = 5 * time.Second
	)

	// 先放一行已提交的旧快照,让"表内最大 guid 之后"这段间隙有明确起点。
	_, _, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(1, 1000, createdAt, []byte("seed")))
	require.NoError(t, err)

	txA, err := db.Raw.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	require.NoError(t, err)
	defer txA.Rollback()
	_, err = txA.ExecContext(ctx, store.InsertSnapshotSQLForTest,
		store.SnapshotInsertArgsForTest(sceneRow(2, 5000, createdAt, []byte("a")))...)
	require.NoError(t, err, "夹具:A 应插入 guid=5000")

	retriesBefore := ss.DeadlockRetriesForTest()
	bCtx, bCancel := context.WithTimeout(ctx, noWaitBudget)
	defer bCancel()
	id, inserted, err := ss.InsertSnapshotIfGuidAbsent(bCtx, sceneRow(3, 6000, createdAt, []byte("b")))
	require.NoError(t, err, "B 被 A 未提交的邻居挡住了(%v 内没插完):唯一键路径不该取间隙 S 锁 —— "+
		"写入语句是不是被改回了 INSERT...SELECT...NOT EXISTS?", noWaitBudget)
	assert.True(t, inserted)
	assert.NotZero(t, id)
	assert.Equal(t, uint64(0), ss.DeadlockRetriesForTest()-retriesBefore,
		"不同 guid 的写者之间不该成环,不该有任何就地重跑")

	// A 回滚:它那一行消失,B 那一行留下 —— 两者本来就互不相干。
	require.NoError(t, txA.Rollback())
	assert.Equal(t, int64(0), db.Count(t, store.PlayerSnapshotTableName, "snapshot_guid = ?", 5000))
	assert.Equal(t, int64(1), db.Count(t, store.PlayerSnapshotTableName, "snapshot_guid = ?", 6000))
}

// TestSnapshotStore_DistinctGuidsNeverDeadlock:一批不同 guid 的写者并发落库,就地重跑计数**一次都不涨**。
// 与上一条确定性用例互补:那条证"不会互等",这条覆盖真实并发度下的量级(老写法在这种并发下会频繁互杀)。
func TestSnapshotStore_DistinctGuidsNeverDeadlock(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	requireGuidUniqueKey(t, ss)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		rounds    = 50
		writers   = 8
		baseGuid  = uint64(100_000)
		createdAt = uint64(1700000000)
	)
	retriesBefore := ss.DeadlockRetriesForTest()
	for r := 0; r < rounds; r++ {
		errs := make([]error, writers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				// guid 单调递增且互不相同,全部落在"表内最大 guid 之后"那段间隙里。
				guid := baseGuid + uint64(r*writers+i)
				_, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(uint64(400+i), guid, createdAt, []byte("raw")))
				if err == nil && !inserted {
					err = fmt.Errorf("guid=%d 此前不存在,却报 inserted=false", guid)
				}
				errs[i] = err
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			require.NoErrorf(t, err, "第 %d 轮写者 %d", r, i)
		}
	}
	assert.Equal(t, uint64(0), ss.DeadlockRetriesForTest()-retriesBefore,
		"不同 guid 的并发写者之间出现了 1213:唯一键应已消掉这一类环(写入语句被改回 NOT EXISTS 形态了?)")
	assert.Equal(t, int64(rounds*writers), db.Count(t, store.PlayerSnapshotTableName, "source = 1"))
}

// TestSnapshotStore_SameGuidQueueDoesNotDeadlock 摆出 MySQL 手册那个经典的 S→X 升级环,
// 证明它在 ODKU 形态下**根本不会成环** —— 不是"成了环再被重试吸收"。
//
// 这条用例此前叫 ...DeadlockIsAbsorbedInPlace,断言的是 `retries >= 1`,等于把"本可消掉的环"
// 写成了规格。复审按手册核实后改成消环(见 insertSnapshotOnDuplicateKeepSQL),断言随之翻面。
//
// 编排(MySQL 手册 "Locks Set by Different SQL Statements in InnoDB" 三会话例的形态):
//  1. A = 显式事务,用**普通 INSERT** 插 guid=G 且不提交 —— 在唯一键上持有这条新索引记录的 X 锁;
//  2. B、C = 两个被测写者(走生产的 ODKU 语句),各插同一个 guid=G:重复键检查撞上 A 那条未提交记录。
//     手册:普通 INSERT 在这一步申请 **S**,而 ODKU 申请 **X**("an exclusive lock rather than a
//     shared lock is placed on the row to be updated when a duplicate-key error occurs")。
//     两人都排队等 A —— 在 performance_schema.data_lock_waits 里**看见**两个等待者才走下一步;
//  3. A **回滚**。S 形态下 B、C 会被**同时**授予 S,随后互等 X 而成环(手册原例);
//     X 形态下只有一人被授予,另一人继续单向等待 —— 不成环。
//  4. 先拿到 X 的那个插入并自动提交;另一个随后看到一条**已提交**的同 guid 记录,
//     ODKU 判为重放(affected=0,一列不改),回出同一个 id。
//
// 判据:B、C 都 err == nil,恰好一个 inserted=true、两人 id 相同,该 guid 恰好一行,
// 并且就地重跑计数 **一次都不涨** —— 涨了就说明环还在,ODKU 没有起到消环的作用。
func TestSnapshotStore_SameGuidQueueDoesNotDeadlock(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	requireDeadlockDetect(t, db.Raw)
	ss := newSnapshotStore(t, db)
	requireGuidUniqueKey(t, ss)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const (
		guid       = uint64(20000)
		createdAt  = uint64(1700000000)
		waitBudget = 10 * time.Second
	)

	// 1. A 插 guid=G 不提交。任何提前失败的路径都必须放掉 A 的锁,否则 B/C 会一直挂到 ctx 超时;
	//    Rollback 之后再 Rollback 无害。
	txA, err := db.Raw.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	require.NoError(t, err)
	defer txA.Rollback()
	_, err = txA.ExecContext(ctx, store.InsertSnapshotSQLForTest,
		store.SnapshotInsertArgsForTest(sceneRow(1, guid, createdAt, []byte("a")))...)
	require.NoError(t, err, "夹具:A 应插入 guid=20000")

	// 2. B、C 各插同一个 guid,排队等 A。
	type result struct {
		id       uint64
		inserted bool
		err      error
	}
	retriesBefore := ss.DeadlockRetriesForTest()
	done := make(chan result, 2)
	for i := range 2 {
		go func() {
			id, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(uint64(500+i), guid, createdAt, []byte("bc")))
			done <- result{id: id, inserted: inserted, err: err}
		}()
	}
	if err := awaitLockWaiters(ctx, db.Raw, store.PlayerSnapshotTableName, 2, waitBudget); err != nil {
		_ = txA.Rollback()
		<-done
		<-done
		skipOrFailOnLockWait(t, err, "B、C 的重复键检查都应申请 S 锁并排在 A 那条未提交的唯一键记录后面")
	}

	// 3. A 回滚 → B、C 同时拿到 S,随即各自要 X,成环。
	require.NoError(t, txA.Rollback())

	results := [2]result{<-done, <-done}
	var insertedCount int
	var winnerID uint64
	for i, r := range results {
		require.NoErrorf(t, r.err, "写者 %d 把错误冒泡给了调用方:ODKU 形态下这一步本该只是单向等待 + 重放判定", i)
		require.NotZerof(t, r.id, "写者 %d 既没插入也没查到已有行的 id", i)
		if r.inserted {
			insertedCount++
			winnerID = r.id
		}
	}
	assert.Equal(t, 1, insertedCount, "恰好一个写者应 inserted=true")
	for i, r := range results {
		assert.Equalf(t, winnerID, r.id, "写者 %d 回出的 id 与实际落库那行不一致", i)
	}
	assert.Equal(t, int64(1), db.Count(t, store.PlayerSnapshotTableName, "snapshot_guid = ?", guid))
	retries := ss.DeadlockRetriesForTest() - retriesBefore
	t.Logf("同 guid 排队 + 先到者回滚:就地重跑的 1213 共 %d 次(期望 0)", retries)
	assert.Equal(t, uint64(0), retries,
		"两个后到者之间仍然成环了:ODKU 的重复键检查应当取 X 而不是 S,后到者因此被串行化"+
			"(手册 innodb-locks-set:\"an exclusive lock rather than a shared lock is placed on the row "+
			"to be updated when a duplicate-key error occurs\")。写入语句是不是被改回普通 INSERT 了?"+
			"见 insertSnapshotOnDuplicateKeepSQL")
}

// TestSnapshotStore_LegacyWriterCoexistsWithUniqueKey 钉住滚动升级窗口的行为:
// **旧版本二进制**(按 INSERT...SELECT...NOT EXISTS 去重)对着**新表结构**(已有唯一键)写,
// 不会违反唯一键,也不会写出重复行。
//
// 这是回滚方案能成立的前提:DDL 与二进制的上线顺序不需要编排(见 schema.go 的「回滚方案」)。
// 用例直接执行旧版那段 SQL 文本 —— 它就是旧二进制在库上留下的东西,不是近似模拟。
func TestSnapshotStore_LegacyWriterCoexistsWithUniqueKey(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	requireGuidUniqueKey(t, ss)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const (
		guid      = uint64(31000)
		gmRows    = 3
		createdAt = uint64(1700000000)
	)
	row := sceneRow(60, guid, createdAt, []byte("legacy"))

	// 旧版第一次写:NOT EXISTS 判真,插一行。
	res, err := db.Raw.ExecContext(ctx, store.InsertSnapshotIfGuidAbsentLegacySQLForTest, legacyInsertArgs(row)...)
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	assert.Equal(t, int64(1), affected)

	// 旧版重放同一条:NOT EXISTS 判假,0 行受影响 —— 不是 1062,所以旧版连错误都看不到。
	res, err = db.Raw.ExecContext(ctx, store.InsertSnapshotIfGuidAbsentLegacySQLForTest, legacyInsertArgs(row)...)
	require.NoError(t, err, "旧版重放撞上唯一键了:说明 NOT EXISTS 没能在插入前挡住重复,回滚窗口不安全")
	affected, err = res.RowsAffected()
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected, "旧版重放应 0 行受影响")

	// 新版看到的是同一行。
	id, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, row)
	require.NoError(t, err)
	assert.False(t, inserted, "新版应看到旧版已经写过这个 guid")
	assert.NotZero(t, id)
	assert.Equal(t, int64(1), db.Count(t, store.PlayerSnapshotTableName, "snapshot_guid = ?", guid))

	// 旧版写 GM 行(guid=0)也不受唯一键影响。
	for i := 0; i < gmRows; i++ {
		_, err := ss.InsertSnapshot(ctx, gmRow(60, createdAt+uint64(i)))
		require.NoError(t, err)
	}
	assert.EqualValues(t, gmRows, db.Count(t, store.PlayerSnapshotTableName, "snapshot_guid = 0"))

	// 反过来:store 被拉回退路(TiDB / 迁移未生效)时,对着有唯一键的表写也照样只落一行。
	ss.ForceLegacyGuidDedupeForTest()
	id2, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, row)
	require.NoError(t, err)
	assert.False(t, inserted)
	assert.Equal(t, id, id2, "退路形态回出的 id 应与唯一键形态一致")
}

// TestSnapshotMigration_DedupesDuplicateGuidsAndIsRerunnable:迁移在建唯一键之前把历史重复行清掉,
// 每个 guid 只留**最小** id,GM 行(guid=0)一条不动;跑第二遍什么都不改。
//
// 起点是"唯一键还没加"的形态(DROP 掉之后造重复行)—— 这正是上线那一刻真实的库状态:
// 重复行在设计上不该存在,但滚动更新窗口 / 本地多开确实可能留下,而唯一键建不上去会让整个迁移卡住。
func TestSnapshotMigration_DedupesDuplicateGuidsAndIsRerunnable(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 迁移没建出唯一键(TiDB)时,本用例证不了任何东西。
	hasKey := newSnapshotStore(t, db).UniqueGuidKeyForTest()
	if !hasKey {
		t.Skipf("迁移没有建出 %s(TiDB?):跳过(不代表通过)", store.SnapshotGuidUniqueKey)
	}
	dropGuidUniqueKey(t, ctx, db)

	const createdAt = uint64(1700000000)
	// guid=41000 三行、42000 两行、43000 一行;另有两条 GM 行(guid=0)。
	dupes := map[uint64][]uint64{}
	for _, spec := range []struct {
		guid  uint64
		times int
	}{{41000, 3}, {42000, 2}, {43000, 1}} {
		for i := 0; i < spec.times; i++ {
			id := rawInsertSnapshot(t, ctx, db, sceneRow(70, spec.guid, createdAt+uint64(i), []byte("dup")))
			dupes[spec.guid] = append(dupes[spec.guid], id)
		}
	}
	gm1 := rawInsertSnapshot(t, ctx, db, gmRow(70, createdAt))
	gm2 := rawInsertSnapshot(t, ctx, db, gmRow(70, createdAt+1))
	require.NotEqual(t, gm1, gm2)
	require.Equal(t, int64(6), db.Count(t, store.PlayerSnapshotTableName, "snapshot_guid <> 0"))

	// 跑迁移:去重 → 加生成列 → 加唯一键。
	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}),
		"迁移应先清掉重复行再建唯一键;失败多半是 1062 —— 说明去重那一步没跑或没跑干净")

	for guid, ids := range dupes {
		got := snapshotIDsOfGuid(t, ctx, db, guid)
		require.Lenf(t, got, 1, "guid=%d 应只剩一行", guid)
		assert.Equalf(t, ids[0], got[0],
			"guid=%d 应保留最小 id(先到的那一行;rollback_audit_log.snapshot_id_used 引用的是它)", guid)
	}
	assert.Equal(t, int64(2), db.Count(t, store.PlayerSnapshotTableName, "snapshot_guid = 0"),
		"GM 行不参与去重(生成列是 NULL),一条都不该被删")

	// 唯一键与生成列都已就位,新的重复写入由库拒掉。
	ss := newSnapshotStore(t, db)
	require.True(t, ss.UniqueGuidKeyForTest(), "迁移之后 store 应探到唯一键")
	_, err := db.Raw.ExecContext(ctx, store.InsertSnapshotSQLForTest,
		store.SnapshotInsertArgsForTest(sceneRow(71, 41000, createdAt, []byte("dup again")))...)
	require.Error(t, err, "去重之后库必须拒掉同 guid 的第二行")

	// 再跑一遍:幂等,行数与唯一键都不变(这也覆盖"proto 同步不会删掉生成列与唯一键")。
	before := db.Count(t, store.PlayerSnapshotTableName, "1 = 1")
	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}))
	assert.Equal(t, before, db.Count(t, store.PlayerSnapshotTableName, "1 = 1"), "重跑迁移不该再删任何行")
	assert.True(t, newSnapshotStore(t, db).UniqueGuidKeyForTest(),
		"重跑迁移之后唯一键应还在:proto 的列 / 索引同步不该删掉 proto 里没有的生成列与唯一键")
}

// TestSnapshotStore_FallsBackWhenUniqueKeyAbsent:表上没有唯一键时(TiDB / 迁移未跑 / DDL 刚回滚),
// store 必须探出来并退回 NOT EXISTS 形态,去重照旧有效 —— 而不是拿普通 INSERT 静默写出重复行。
//
// 这是"探测"这一步存在的全部理由:猜错方向的代价不对称。假设键在而它不在 = 同一 guid 落两行且零报错。
func TestSnapshotStore_FallsBackWhenUniqueKeyAbsent(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if !newSnapshotStore(t, db).UniqueGuidKeyForTest() {
		t.Skipf("迁移没有建出 %s(TiDB?):本用例要先有键再删掉,跳过(不代表通过)", store.SnapshotGuidUniqueKey)
	}
	dropGuidUniqueKey(t, ctx, db)

	ss := newSnapshotStore(t, db)
	require.False(t, ss.UniqueGuidKeyForTest(), "键已删掉,store 必须探出「不存在」")

	const (
		guid      = uint64(51000)
		createdAt = uint64(1700000000)
	)
	id, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(80, guid, createdAt, []byte("raw")))
	require.NoError(t, err)
	require.True(t, inserted)

	id2, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(80, guid, createdAt, []byte("raw")))
	require.NoError(t, err)
	assert.False(t, inserted, "退路形态也必须去重")
	assert.Equal(t, id, id2)
	assert.Equal(t, int64(1), db.Count(t, store.PlayerSnapshotTableName, "snapshot_guid = ?", guid))
}

// ── 复审补的三条回归:形状守卫 fail-closed、去重点删的执行计划 ────────────────────────────

// TestSnapshotMigration_RefusesSameNamedNonUniqueIndex:表上有一个**同名但非唯一**的索引时,
// 迁移必须报错停住,而不是当成"键已存在"直接跳过。
//
// 这是最坏的一种放行:迁移在快路径 return nil(既不去重也不建唯一键),SnapshotStore 探测置
// uniqueGuidKey=true、写路径切到 ODKU —— 而 ODKU 在没有唯一约束的表上永远撞不到重复键,
// 同一个 guid 静默落成多行、全程零报错。把 UNIQUE 悄悄降级成普通索引的兼容层(代理 / 旧版本),
// 与 assertNullableUniqueKey 头注里那条"ALTER 被解析后忽略"落在同一个威胁模型里。
func TestSnapshotMigration_RefusesSameNamedNonUniqueIndex(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if !newSnapshotStore(t, db).UniqueGuidKeyForTest() {
		t.Skipf("迁移没有建出 %s(TiDB?):本用例要先有键再换成非唯一的,跳过(不代表通过)", store.SnapshotGuidUniqueKey)
	}
	dropGuidUniqueKey(t, ctx, db)

	// 造出"同名非唯一索引":建在源列上,名字与唯一键一样。
	_, err := db.Raw.ExecContext(ctx, fmt.Sprintf("ALTER TABLE `%s` ADD INDEX `%s` (`snapshot_guid`)",
		store.PlayerSnapshotTableName, store.SnapshotGuidUniqueKey))
	require.NoError(t, err, "夹具:造一个同名的非唯一索引")

	err = store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{})
	require.Error(t, err, "同名的非唯一索引必须让迁移失败 —— 放行等于让写路径以为唯一约束在,"+
		"而普通 / ODKU 写入在没有唯一约束的表上根本不去重")
	assert.Contains(t, err.Error(), store.SnapshotGuidUniqueKey, "错误里要指名是哪个索引,运维才知道去 DROP 哪条")

	// store 侧同一判据:探测也必须**不**把它当成唯一键。
	_, probeErr := store.NewSnapshotStore(db.Cfg)
	require.NoError(t, probeErr, "探测失败不该让建 store 失败(它 fail-safe 到退路那条语句)")
}

// TestSnapshotMigration_RefusesSameNamedPlainColumn:表上有一个**同名但不是生成列**的普通列时,
// 迁移必须报错停住,而不是跳过 ADD COLUMN 然后把唯一键建到它上面。
//
// 建上去的后果按那一列的默认值分两种,都坏:恒为 NULL → 唯一键约束不到任何行(等于没做);
// 恒为 0 → 第二条 GM 行就撞键,GM 回滚路径直接挂掉。两种都是"名字对、形状不对"。
func TestSnapshotMigration_RefusesSameNamedPlainColumn(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if !newSnapshotStore(t, db).UniqueGuidKeyForTest() {
		t.Skipf("迁移没有建出 %s(TiDB?):本用例要先有键再把列换成普通列,跳过(不代表通过)", store.SnapshotGuidUniqueKey)
	}
	dropGuidUniqueKey(t, ctx, db)

	// 造出"同名普通列"(可空、无生成表达式)——人工加列、或兼容层把生成列降级成普通列的形态。
	_, err := db.Raw.ExecContext(ctx, fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `%s` bigint unsigned NULL",
		store.PlayerSnapshotTableName, store.SnapshotGuidNzColumn))
	require.NoError(t, err, "夹具:造一个同名的普通列")

	err = store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{})
	require.Error(t, err, "同名的普通列必须让迁移失败 —— 跳过 ADD COLUMN 之后,唯一键会建在一个恒为 NULL "+
		"(约束不到任何行)或恒为 0(第二条 GM 行就撞键)的列上")
	assert.Contains(t, err.Error(), store.SnapshotGuidNzColumn, "错误里要指名是哪一列")
}

// TestSnapshotMigration_DedupePointDeleteUsesPrimaryKey:迁移去重删行走的是**主键等值点删**。
//
// 锁面只取决于执行计划,并发用例只能按概率撞上,而 EXPLAIN 每次都答得出来。
// 这条点删若退化成 `WHERE snapshot_guid = ? AND id > ?` 的二级索引范围删,取锁顺序就与在线写者相反
// (写者先聚簇后二级,范围删先二级 next-key X 后回表),构成残余环;一次 1213 会让整条
// MigrateSchema 失败(AutoMigrate 下三个 store 全置 nil)。
// 对生产代码里**同一段 SQL** 做 EXPLAIN,不在用例里另抄一份。
func TestSnapshotMigration_DedupePointDeleteUsesPrimaryKey(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ss := newSnapshotStore(t, db)
	requireGuidUniqueKey(t, ss)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// 先落一行,让计划命中真实存在的主键值。
	id, _, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(90, 61000, 1700000000, []byte("raw")))
	require.NoError(t, err)

	stmt := strings.Replace(store.DedupePointDeleteSQLForTest(store.PlayerSnapshotTableName),
		"?", fmt.Sprintf("%d", id), 1)
	require.NotContains(t, stmt, "?", "占位符数量与参数不符")

	plan := explainFirstRow(t, ctx, db.Raw, stmt)
	t.Logf("迁移去重点删的执行计划: %v", plan)
	assert.Equal(t, "PRIMARY", plan["key"],
		"去重删必须走主键:走 idx(snapshot_guid) 说明退化成了二级索引上的范围删,取锁顺序与在线写者相反")
	assert.Equal(t, "8", plan["key_len"], "key_len 必须用满(bigint unsigned = 8):不足说明退化成了前缀 / 范围访问")
	// 单表 DELETE 的主键等值,MySQL 8 的 EXPLAIN 报 range(不是 SELECT 的 const),两者都表示"只这一行";
	// 出现 index / ALL 才是真的退化,所以这里只排除扫描型的 type。
	assert.Containsf(t, []string{"const", "range"}, plan["type"],
		"type=%q:主键等值删只应是 const / range,出现扫描型计划说明语句被改坏了", plan["type"])
	assert.Equal(t, "1", plan["rows"], "主键等值只应估到一行")
}

// TestSnapshotStore_LookupMissIsAnError:写入语句判定"这个 guid 已存在"、而随后的点查却查不到
// 那一行时,必须**报错**,不能回出 (id=0, inserted=false, err=nil)。
//
// 这条组合出口原本是一条静默丢数据的通道:kafka.SnapshotConsumer 收到它会记一条 duplicate 指标、
// 打一条 `already stored as id=0` 的 INFO,然后**提交 offset** —— 这条快照永久丢失且全程零报错。
// 窗口很窄(要求写入那一刻库里有同 guid 行、点查时已被删),但删者是真实存在的:
// DeleteOldSnapshots 的 retention 清理,以及迁移期的去重 DELETE。
//
// 这个交错没法在两条语句之间稳定塞进一次删除(store 走自己的连接池、全程自动提交),
// 所以直接对**内部出口**取证:对一个库里不存在的 guid 调用点查,断言它报错而不是回 nil。
// 两个形态都要覆盖 —— 有唯一键时走 uk 点查,退路时走 idx(snapshot_guid) 点查。
func TestSnapshotStore_LookupMissIsAnError(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const missingGuid = uint64(71000)

	for _, tc := range []struct {
		name   string
		legacy bool
	}{{"唯一键形态", false}, {"退路形态", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ss := newSnapshotStore(t, db)
			if tc.legacy {
				ss.ForceLegacyGuidDedupeForTest()
			} else {
				requireGuidUniqueKey(t, ss)
			}
			id, inserted, err := ss.LookupSnapshotIDByGuidForTest(ctx, missingGuid)
			require.Errorf(t, err, "查不到这一行时必须报错:回出 (id=0, inserted=false, err=nil) 会让消费者"+
				"记一条 duplicate、打一条 already stored as id=0 的 INFO,然后提交 offset 把这条快照丢掉")
			assert.Zero(t, id)
			assert.False(t, inserted)
			assert.Contains(t, err.Error(), fmt.Sprintf("%d", missingGuid), "错误里要带上 guid,运维才定位得到")
		})
	}

	// 反面:行在的时候照常回出 id,不报错 —— 否则上面那条断言用"永远报错"也能通过。
	ss := newSnapshotStore(t, db)
	wantID, _, err := ss.InsertSnapshotIfGuidAbsent(ctx, sceneRow(95, missingGuid+1, 1700000000, []byte("raw")))
	require.NoError(t, err)
	gotID, inserted, err := ss.LookupSnapshotIDByGuidForTest(ctx, missingGuid+1)
	require.NoError(t, err)
	assert.Equal(t, wantID, gotID)
	assert.False(t, inserted)
}
