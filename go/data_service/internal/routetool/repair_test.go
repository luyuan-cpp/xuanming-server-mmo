package routetool

// reconcile-from-cache(第 1b-1 批)与 audit -frozen-recovery 的逻辑:miniredis + 假 repairDB,不连库。
// 库内的判定(decideRepair、R18x 加锁复核、R17 的 CAS)由 store 包的契约用例与真库用例钉住;这里只钉
// 「从 Redis 读什么、三遍怎么无状态去重、交给库什么、怎么计数、退出码是几」。

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"data_service/internal/store"

	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/drainproof"
	"shared/placement"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// *store.RouteStore 必须满足 repairDB(routetoolcli 直接把它传进来);签名漂移在这里编译期就炸。
var _ repairDB = (*store.RouteStore)(nil)

type repairCall struct {
	rows []store.RouteImportRow
	opts store.RouteRepairOptions
}

// fakeRepairDB 是 repairDB 的内存替身:修库只记录入参(按 hook 回答,缺省「全部 Restored」),并像真 store 一样拒绝
// 不是严格升序的批次 —— 页内去重漏了的话这里会报错。
type fakeRepairDB struct {
	meta        store.RouteMeta
	calls       []repairCall
	mergedCalls []mergedCall
	hook        func(n int, rows []store.RouteImportRow, opts store.RouteRepairOptions) store.RouteRepairStats
}

func newFrozenRepairDB() *fakeRepairDB {
	return &fakeRepairDB{meta: store.RouteMeta{Generation: "0123456789abcdef0123456789abcdef", Writable: 0}}
}

func (f *fakeRepairDB) ReadRouteMeta(context.Context) (store.RouteMeta, error) { return f.meta, nil }

func (f *fakeRepairDB) RepairRoutesFromCache(_ context.Context, rows []store.RouteImportRow, opts store.RouteRepairOptions) (store.RouteRepairStats, error) {
	for i := 1; i < len(rows); i++ {
		if rows[i].PlayerID <= rows[i-1].PlayerID {
			return store.RouteRepairStats{}, fmt.Errorf("%w: rows not strictly ascending (%d after %d)", store.ErrRouteInvalidArgument, rows[i].PlayerID, rows[i-1].PlayerID)
		}
	}
	f.calls = append(f.calls, repairCall{rows: append([]store.RouteImportRow(nil), rows...), opts: opts})
	if f.hook != nil {
		return f.hook(len(f.calls), rows, opts), nil
	}
	return store.RouteRepairStats{Restored: len(rows)}, nil
}

func (f *fakeRepairDB) RepairMergedIntoFromCache(_ context.Context, rows []store.MergedIntoRow, dryRun bool) (store.MergedIntoImportStats, error) {
	f.mergedCalls = append(f.mergedCalls, mergedCall{rows: append([]store.MergedIntoRow(nil), rows...), dryRun: dryRun})
	return store.MergedIntoImportStats{Inserted: len(rows)}, nil
}

// repairedIDs 按调用顺序摊平所有修库批次里的 player_id。
func (f *fakeRepairDB) repairedIDs() []uint64 {
	var out []uint64
	for _, c := range f.calls {
		for _, r := range c.rows {
			out = append(out, r.PlayerID)
		}
	}
	return out
}

// dupScanReader 让每一页 SCAN 把每把键回两次(Redis rehash 期间同一页也可能回重复键),钉住「页内先去重」。
type dupScanReader struct{ mappingReader }

func (d dupScanReader) ScanCtx(ctx context.Context, cursor uint64, match string, count int64) ([]string, uint64, error) {
	keys, next, err := d.mappingReader.ScanCtx(ctx, cursor, match, count)
	return append(append([]string(nil), keys...), keys...), next, err
}

// runRepair 用小批量(2)跑一次 reconcile-from-cache;sleeps 收集注入的等待时长(不真睡)。
func runRepair(t *testing.T, rd mappingReader, db repairDB, dryRun, resolveTies bool) (RepairReport, cliRun, []time.Duration) {
	t.Helper()
	var out, errOut bytes.Buffer
	var sleeps []time.Duration
	receipt := unitFrozenReceipt(t)
	rep, err := ReconcileFromCache(context.Background(), rd, db, RepairOptions{
		Catalog: &unitMaintenanceCatalog{frozen: receipt, meta: db.ReadRouteMeta}, FrozenReceipt: receipt, Maintenance: drainproof.New(),
		DryRun:      dryRun,
		ResolveTies: resolveTies,
		Batch:       2,
		MaxReport:   100,
		FreezeGrace: 10 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
		Stdout: &out,
		Stderr: &errOut,
	})
	return rep, cliRun{code: RepairExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}, sleeps
}

// TestReconcileFromCache_RefusedWhenWritable:writable=1 → 退出码 2,对 Redis 不扫、对库零写入,stdout 不出统计行。
func TestReconcileFromCache_RefusedWhenWritable(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{"player:zone:1": "1"})
	db := newFrozenRepairDB()
	db.meta.Writable = 1

	_, run, sleeps := runRepair(t, rd, db, false, false)

	assert.Equal(t, ExitFailed, run.code)
	assert.Empty(t, db.calls)
	assert.Empty(t, db.mergedCalls)
	assert.Empty(t, sleeps, "没冻结就不该等宽限")
	assert.Empty(t, run.stdout)
	assert.NotContains(t, run.stderr, RepairNote, "正式冻结校验先于业务恢复输出")
}

// TestReconcileFromCache_FenceExitsThree:开头有围栏 → 退出码 3,一行都不修;途中才立的围栏由结尾复查发现,已修的保留、统计行照出。
func TestReconcileFromCache_FenceExitsThree(t *testing.T) {
	t.Run("开头", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		setKeys(t, mr, map[string]string{"player:zone:1": "1", placement.MergeFenceKey(3): "{}"})
		db := newFrozenRepairDB()

		_, run, _ := runRepair(t, rd, db, false, false)

		assert.Equal(t, ExitFenced, run.code)
		assert.Empty(t, db.calls)
		assert.Empty(t, run.stdout)
		assert.Contains(t, run.stderr, placement.MergeFenceKey(3))
	})
	t.Run("结尾", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		setKeys(t, mr, map[string]string{"player:zone:1": "1"})
		db := newFrozenRepairDB()
		db.hook = func(n int, rows []store.RouteImportRow, _ store.RouteRepairOptions) store.RouteRepairStats {
			if n == 1 {
				require.NoError(t, mr.Set(placement.MergeFenceKey(1), "{}"))
			}
			return store.RouteRepairStats{Restored: len(rows)}
		}

		rep, run, _ := runRepair(t, rd, db, false, false)

		assert.Equal(t, ExitFenced, run.code)
		assert.Equal(t, 1, rep.Restored, "围栏出现之前修的那一批不回滚")
		assert.Contains(t, run.stdout, "route_reconcile_from_cache ")
		assert.Contains(t, run.stderr, "after the repair")
	})
}

// TestReconcileFromCache_StatelessDedupeAcrossPasses:三族键都在的玩家只修一次(A 遍);只有落点(+ maprev)的在 B 遍修一次;
// 每页 SCAN 回重复键时同一页里只交一次(否则真 store 按「严格升序、无重复」整批拒绝)。不保留跨遍的已见集合。
func TestReconcileFromCache_StatelessDedupeAcrossPasses(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{
		"player:zone:1": "1", "player:placement:1": "1:1", "player:maprev:1": "4",
		"player:zone:2": "2", "player:maprev:2": "2",
		"player:placement:3": "3:1", "player:maprev:3": "5",
		"player:placement:4": "4:1",
	})
	db := newFrozenRepairDB()

	rep, run, _ := runRepair(t, dupScanReader{rd}, db, false, false)

	assert.Equal(t, ExitClean, run.code, run.stderr)
	got := db.repairedIDs()
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	assert.Equal(t, []uint64{1, 2, 3, 4}, got, "每名玩家恰好修一次")
	assert.Equal(t, 4, rep.PlayersScanned)
	byID := map[uint64]store.RouteImportRow{}
	for _, c := range db.calls {
		for _, r := range c.rows {
			byID[r.PlayerID] = r
		}
	}
	assert.Equal(t, store.RouteImportRow{PlayerID: 1, HomeZone: 1, Placement: rec(1, 1), MapRev: 4}, byID[1])
	assert.Equal(t, store.RouteImportRow{PlayerID: 3, Placement: rec(3, 1), MapRev: 5}, byID[3], "B 遍的玩家带着 maprev 交给库")
	assert.Equal(t, 1, strings.Count(run.stdout, "\n"), run.stdout)
}

// TestReconcileFromCache_MapRevOnlyIsMalformed:只有 maprev、没有 zone 与落点 → C 遍记 MALFORMED、不交给库,退出码 1;
// 值不规范的整名玩家跳过。
func TestReconcileFromCache_MapRevOnlyIsMalformed(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{
		"player:maprev:9":  "3",
		"player:zone:10":   "0012",
		"player:zone:11":   "11",
		"player:zone:007":  "7",
		"player:maprev:12": "07",
	})
	db := newFrozenRepairDB()

	rep, run, _ := runRepair(t, rd, db, false, false)

	assert.Equal(t, ExitAttention, run.code)
	assert.Equal(t, []uint64{11}, db.repairedIDs())
	assert.Equal(t, 4, rep.Malformed, "9(只有 maprev)、10(zone 不规范)、007(键名)、12(maprev 不规范)")
	assert.Contains(t, run.stderr, "MALFORMED player=9 key=player:maprev:9")
	assert.Contains(t, run.stderr, `MALFORMED player=10 key=player:zone:10 value="0012"`)
	assert.Contains(t, run.stderr, "key=player:zone:007")
}

// TestReconcileFromCache_TiesReportedByDefault:默认 -resolve-ties=report 交给库的 ResolveTies 为 false;库报的平局打 TIE 明细、退出码 1。
func TestReconcileFromCache_TiesReportedByDefault(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{"player:zone:21": "7", "player:maprev:21": "3"})
	db := newFrozenRepairDB()
	db.hook = func(_ int, rows []store.RouteImportRow, opts store.RouteRepairOptions) store.RouteRepairStats {
		require.False(t, opts.ResolveTies)
		return store.RouteRepairStats{Ties: []store.RouteImportConflict{{PlayerID: 21, DB: store.RouteRow{PlayerID: 21, HomeZone: 8, Rev: 3}, Redis: rows[0]}}}
	}

	rep, run, _ := runRepair(t, rd, db, false, false)

	assert.Equal(t, ExitAttention, run.code)
	assert.Equal(t, 1, rep.Ties)
	assert.Contains(t, run.stderr, "TIE player=21 db_home=8 db_placement=\"\" db_rev=3 redis_home=7 redis_placement=\"\" redis_maprev=3")
}

// TestReconcileFromCache_ResolveTiesCache:-resolve-ties=cache 交给库的 ResolveTies 为 true;已覆盖的平局打 TIE_RESOLVED 明细,不算人工项。
func TestReconcileFromCache_ResolveTiesCache(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{"player:zone:21": "7", "player:maprev:21": "3"})
	db := newFrozenRepairDB()
	db.hook = func(_ int, rows []store.RouteImportRow, opts store.RouteRepairOptions) store.RouteRepairStats {
		require.True(t, opts.ResolveTies)
		return store.RouteRepairStats{TiesResolved: []store.RouteImportConflict{{PlayerID: 21, DB: store.RouteRow{PlayerID: 21, HomeZone: 8, Rev: 3}, Redis: rows[0]}}}
	}

	rep, run, _ := runRepair(t, rd, db, false, true)

	assert.Equal(t, ExitClean, run.code, run.stderr)
	assert.Equal(t, 1, rep.TiesResolved)
	assert.Contains(t, run.stderr, "TIE_RESOLVED player=21 ")
	assert.Contains(t, run.stdout, " ties=0 ties_resolved=1 ")
}

// TestReconcileFromCache_DryRunWritesNothing:-dry-run 原样交给库的两个修库原语(由它们只发不加锁的读);stdout 一行 dry_run=true。
func TestReconcileFromCache_DryRunWritesNothing(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{"player:zone:1": "1", "merge:merged_into:5": `{"dst":6,"run_id":"r5"}`})
	db := newFrozenRepairDB()

	rep, run, _ := runRepair(t, rd, db, true, false)

	assert.Equal(t, ExitClean, run.code, run.stderr)
	require.NotEmpty(t, db.calls)
	require.NotEmpty(t, db.mergedCalls)
	for _, c := range db.calls {
		assert.True(t, c.opts.DryRun)
	}
	for _, c := range db.mergedCalls {
		assert.True(t, c.dryRun)
	}
	assert.True(t, rep.DryRun)
	assert.True(t, strings.HasPrefix(run.stdout, "route_reconcile_from_cache dry_run=true players_scanned=1 "), run.stdout)
	assert.Equal(t, RepairNote, strings.SplitN(run.stderr, "\n", 2)[0], "stderr 第一行是 NOTE")
}

// TestReconcileFromCache_MergedIntoInsertedNeverDeleted:缓存里的合走标记(合法的)全部交给库补回;畸形的记 MALFORMED;
// repairDB 接口里没有任何删除合走标记的方法 —— 「永不删除」由接口形状保证,库内的只增语义由 store 契约用例钉住。
func TestReconcileFromCache_MergedIntoInsertedNeverDeleted(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{
		"merge:merged_into:5": `{"dst":6,"run_id":"5-6-20261018T000000Z-abcdef12"}`,
		"merge:merged_into:7": `not-json`,
	})
	db := newFrozenRepairDB()

	rep, run, _ := runRepair(t, rd, db, false, false)

	assert.Equal(t, ExitAttention, run.code, "畸形的合走标记要人看")
	require.Len(t, db.mergedCalls, 1)
	assert.Equal(t, []store.MergedIntoRow{{SrcZone: 5, DstZone: 6, RunID: "5-6-20261018T000000Z-abcdef12"}}, db.mergedCalls[0].rows)
	assert.Equal(t, 1, rep.MergedIntoInserted)
	assert.Equal(t, 1, rep.MergedIntoMalformed)
	assert.Contains(t, run.stderr, "MALFORMED merged_into key=merge:merged_into:7")
}

// 正式冻结原件替代固定宽限等待；等待器既不能补授权，也不决定已经闭合后的恢复结果。
func TestReconcileFromCache_GraceUsesInjectedSleeper(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{"player:zone:1": "1"})
	db := newFrozenRepairDB()

	_, run, sleeps := runRepair(t, rd, db, false, false)
	assert.Equal(t, ExitClean, run.code, run.stderr)
	assert.Empty(t, sleeps, "永久冻结收据替代按时间猜排空")

	db = newFrozenRepairDB()
	var out, errOut bytes.Buffer
	receipt := unitFrozenReceipt(t)
	rep, err := ReconcileFromCache(context.Background(), rd, db, RepairOptions{
		Catalog: &unitMaintenanceCatalog{frozen: receipt, meta: db.ReadRouteMeta}, FrozenReceipt: receipt, Maintenance: drainproof.New(),
		Batch: 2, FreezeGrace: time.Second, Stdout: &out, Stderr: &errOut,
		Sleep: func(context.Context, time.Duration) error { return context.Canceled },
	})
	assert.NoError(t, err, "已经核验正式冻结时不调用过期的等待器")
	assert.Equal(t, ExitClean, RepairExitCode(rep, err))
	assert.NotEmpty(t, db.calls)
}

// TestRouteAudit_FrozenRecoveryIgnoresDBOnlyUnpublished:冻结期恢复口径下 db_only_unpublished 照常计数与打明细,但不计入退出码;
// 其余任何一类差异仍是退出码 1;统计行的格式不变。
func TestRouteAudit_FrozenRecoveryIgnoresDBOnlyUnpublished(t *testing.T) {
	audit := func(t *testing.T, mutate func(*fakeRouteDB, func(map[string]string)), frozen bool) (AuditReport, cliRun) {
		t.Helper()
		mr, rd := newMappingRedis(t)
		db := newFakeRouteDB()
		// 1 一致;2 库独有(三族键全无)= db_only_unpublished。
		db.rows[1] = store.RouteRow{PlayerID: 1, HomeZone: 1, Rev: 1}
		db.rows[2] = store.RouteRow{PlayerID: 2, HomeZone: 2, Rev: 1}
		setKeys(t, mr, map[string]string{"player:zone:1": "1"})
		if mutate != nil {
			mutate(db, func(kv map[string]string) { setKeys(t, mr, kv) })
		}
		var out, errOut bytes.Buffer
		rep, err := Audit(context.Background(), rd, db, AuditOptions{
			Batch: 2, MaxReport: 100, MaxRecheck: 100, Stdout: &out, Stderr: &errOut, Sleep: noSleep, FrozenRecovery: frozen,
		})
		return rep, cliRun{code: AuditExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}
	}

	rep, run := audit(t, nil, true)
	assert.Equal(t, ExitClean, run.code, run.stderr)
	assert.Equal(t, 1, rep.DBOnlyUnpublished, "照常计数")
	assert.Contains(t, run.stderr, "DIFF kind=db_only_unpublished player=2 ")
	assert.Contains(t, run.stderr, "NOTE: -frozen-recovery: db_only_unpublished=1")
	assert.True(t, strings.HasPrefix(run.stdout, "route_audit db_rows=2 redis_players=1 db_only_unpublished=1 "), run.stdout)

	_, run = audit(t, nil, false)
	assert.Equal(t, ExitAttention, run.code, "默认口径 db_only_unpublished 仍算差异")

	rep, run = audit(t, func(_ *fakeRouteDB, set func(map[string]string)) {
		set(map[string]string{"player:zone:1": "9"}) // value_mismatch
	}, true)
	assert.Equal(t, ExitAttention, run.code, "其余类别在恢复口径下照样要为 0")
	assert.Equal(t, 1, rep.ValueMismatch)
}

func TestReconcileFromCacheRequiresBoundPermanentFreeze(t *testing.T) {
	for _, name := range []string{"missing-catalog", "missing-receipt", "different-run", "wrong-kind", "wrong-run-phase", "different-generation"} {
		t.Run(name, func(t *testing.T) {
			_, rd := newMappingRedis(t)
			db := newFrozenRepairDB()
			permanent := unitFrozenReceipt(t)
			input := proto.Clone(permanent).(*pb.SqlWriterMaintenanceReceipt)
			catalog := &unitMaintenanceCatalog{frozen: permanent, meta: db.ReadRouteMeta}
			var out, errOut bytes.Buffer
			opts := RepairOptions{Catalog: catalog, FrozenReceipt: input, Maintenance: drainproof.New(), Batch: 2, MaxReport: 10, Stdout: &out, Stderr: &errOut}
			switch name {
			case "missing-catalog":
				opts.Catalog = nil
			case "missing-receipt":
				opts.FrozenReceipt = nil
			case "different-run":
				input.RunId[0] ^= 1
			case "wrong-kind":
				permanent.Kind = pb.SqlWriterMaintenanceReceipt_EMERGENCY_FREEZE
				input.Kind = permanent.Kind
			case "wrong-run-phase":
				permanent.RunPhase = pb.SqlWriterDrainRun_DRAINING
				input.RunPhase = permanent.RunPhase
			case "different-generation":
				db.meta.Generation = "fedcba9876543210fedcba9876543210"
			}
			rep, err := ReconcileFromCache(context.Background(), rd, db, opts)
			assert.Error(t, err)
			assert.Equal(t, ExitFailed, RepairExitCode(rep, err))
			assert.Empty(t, db.calls)
			assert.Empty(t, db.mergedCalls)
			assert.Empty(t, out.String())
			assert.NotContains(t, errOut.String(), RepairNote)
		})
	}
}

func TestReconcileFromCacheClosedOrMissingToolDoesNotWrite(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			mr, rd := newMappingRedis(t)
			setKeys(t, mr, map[string]string{"player:zone:1": "1"})
			db := newFrozenRepairDB()
			receipt := unitFrozenReceipt(t)
			work := drainproof.New()
			work.CloseAdmission()
			var out, errOut bytes.Buffer
			opts := RepairOptions{Catalog: &unitMaintenanceCatalog{frozen: receipt, meta: db.ReadRouteMeta}, FrozenReceipt: receipt, Maintenance: work, Batch: 2, MaxReport: 10, Stdout: &out, Stderr: &errOut}
			if missing {
				opts.Maintenance = nil
			}
			rep, err := ReconcileFromCache(context.Background(), rd, db, opts)
			require.Error(t, err)
			require.Equal(t, ExitFailed, RepairExitCode(rep, err))
			require.Empty(t, db.calls)
			require.Empty(t, db.mergedCalls)
			require.Empty(t, out.String())
			require.NoError(t, work.Wait(context.Background()), "本地拒绝没有接受恢复任务")
		})
	}
}

func TestReconcileFromCacheFailureKeepsToolHoldAfterReturn(t *testing.T) {
	_, rd := newMappingRedis(t)
	db := newFrozenRepairDB()
	receipt := unitFrozenReceipt(t)
	db.meta.Writable = 1
	work := drainproof.New()
	var out, errOut bytes.Buffer
	_, err := ReconcileFromCache(context.Background(), rd, db, RepairOptions{Catalog: &unitMaintenanceCatalog{frozen: receipt, meta: db.ReadRouteMeta}, FrozenReceipt: receipt, Maintenance: work, Batch: 2, MaxReport: 10, Stdout: &out, Stderr: &errOut})
	require.Error(t, err)
	require.Zero(t, work.Pending())
	work.CloseAdmission()
	require.ErrorIs(t, work.Wait(context.Background()), drainproof.ErrHold)
	require.Empty(t, db.calls)
}
