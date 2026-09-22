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

// snapshotInsertArgs 按 store.InsertSnapshotIfGuidAbsentSQLForTest 的占位符顺序展开一行(与生产 ExecContext 的实参一致)。
func snapshotInsertArgs(r *store.SnapshotRow) []any {
	return []any{r.PlayerID, r.ZoneID, r.SnapshotType, r.CreatedAt, r.Reason, r.Operator, r.Data,
		r.SnapshotGuid, r.Source, r.SnapshotGuid}
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

// errLockWaitsUnobservable:测试账号读不了 performance_schema(或库不是 MySQL 8)。
// 这时手工编排做不出来 —— 那不是产品缺陷,用例跳过(不代表通过)。
var errLockWaitsUnobservable = errors.New("读 performance_schema.data_lock_waits 失败")

// awaitLockWaiters 轮询,直到 table 上至少有 want 个事务在排队等锁,或 budget 用尽(返回错误)。
// 靠轮询**观察**,不靠 sleep 估时间:两次轮询之间的短停顿只是不去空转打爆 MySQL。
func awaitLockWaiters(ctx context.Context, raw *sql.DB, table string, want int, budget time.Duration) error {
	const pollEvery = 10 * time.Millisecond
	deadline := time.Now().Add(budget)
	for {
		var waiters int
		if err := raw.QueryRowContext(ctx, lockWaitersQuery, table).Scan(&waiters); err != nil {
			return fmt.Errorf("%w: %v", errLockWaitsUnobservable, err)
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

// skipOrFailOnLockWait:读不了 performance_schema 就跳过;其余错误(等待者没到齐)一律判红 ——
// 编排失败本身就说明锁行为与推演不符。
func skipOrFailOnLockWait(t *testing.T, err error, expectation string) {
	t.Helper()
	if errors.Is(err, errLockWaitsUnobservable) {
		t.Skipf("无法编排本场景(测试账号需要 performance_schema 的 SELECT 权限),跳过(不代表通过): %v", err)
	}
	t.Fatalf("夹具编排失败:%v —— %s", err, expectation)
}

// TestSnapshotStore_ConcurrentWritersNeverSurfaceDeadlock:两个写者成对并发落快照,偶数轮同 guid、奇数轮不同 guid
// (都大于表内已有的最大 guid,正是 SnowFlake 单调递增时的常态,全部落在 supremum 那段间隙)。
// 修复前这类并发会让其中一方把 1213 当错误返回;现在必须全部 err == nil,并且同一 guid 恰好一行。
//
// 每轮用 barrier 同时放行两个写者、等两者都结束再进下一轮:胜者不会紧接着再写下一条,所以被牺牲方的一次
// 重跑必然收敛(见 InsertSnapshotIfGuidAbsent 的收敛论证),对 err 的断言是确定的。某一轮是否真的撞上 1213
// 取决于调度,只记日志不断言 —— 确定性的"撞上并被就地吸收"由 TestSnapshotStore_GapDeadlockIsAbsorbedInPlace 证明。
func TestSnapshotStore_ConcurrentWritersNeverSurfaceDeadlock(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	requireGlobalRepeatableRead(t, db)
	requireDeadlockDetect(t, db.Raw)
	ss := newSnapshotStore(t, db)
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

// TestSnapshotStore_GapDeadlockIsAbsorbedInPlace 按审计 #11 的交错一步一步摆出那个环,证明 1213 真的发生了、
// 并且被 InsertSnapshotIfGuidAbsent 就地吸收,调用方看到的只是一次成功插入。
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
func TestSnapshotStore_GapDeadlockIsAbsorbedInPlace(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	requireGlobalRepeatableRead(t, db)
	requireDeadlockDetect(t, db.Raw)
	ss := newSnapshotStore(t, db)
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
	res, err := txA.ExecContext(ctx, store.InsertSnapshotIfGuidAbsentSQLForTest,
		snapshotInsertArgs(sceneRow(2, 2000, createdAt, []byte("a1")))...)
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
	if _, err := txA.ExecContext(ctx, store.InsertSnapshotIfGuidAbsentSQLForTest,
		snapshotInsertArgs(sceneRow(4, 4000, createdAt, []byte("a2")))...); err != nil {
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
