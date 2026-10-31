package routetool

// route_tool 的 import / audit 逻辑(第 1a 批规格 f 节「routetool/routetool_test.go」)。
//
// miniredis + 假 routeDB,不连库:库内的判定(decideImport、ODKU、加锁读)由 store 包的契约用例与真库用例钉住,
// 这里只钉「从 Redis 读什么、怎么校验、交给库什么、怎么计数、退出码是几」。
// 真库 + miniredis 的端到端在 routetool_integration_test.go(-tags=integration)。

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"data_service/internal/store"

	"shared/placement"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// go-zero 的 *redis.Redis 必须满足 mappingReader(cmd/route_tool 直接把它传进来);签名漂移在这里编译期就炸。
var _ mappingReader = (*redis.Redis)(nil)

// *store.RouteStore 必须满足 routeDB 与 tidbPreparer。
var (
	_ routeDB      = (*store.RouteStore)(nil)
	_ tidbPreparer = (*store.RouteStore)(nil)
)

// ── 替身 ──────────────────────────────────────────────────────────────────────

type importCall struct {
	rows   []store.RouteImportRow
	dryRun bool
}

type mergedCall struct {
	rows   []store.MergedIntoRow
	dryRun bool
}

// fakeRouteDB 是 routeDB 的内存替身:导入只记录入参(按预置结果回答),只读接口读 rows / merged。
type fakeRouteDB struct {
	rows   map[uint64]store.RouteRow
	merged []store.MergedIntoRow
	// lateRows 只有 GetRoutes 看得到、ScanRoutes 看不到:模拟 audit 方向 A 的键集翻页扫过之后才提交的行
	// (db_first 下审计期间新登记的玩家)。
	lateRows map[uint64]store.RouteRow

	importCalls []importCall
	mergedCalls []mergedCall

	// importHook 在每次 ImportRoutes 里执行(n 从 1 起);返回非 nil 时用它作为这一次的统计,
	// 否则按「全部插入」回答。
	importHook func(n int, rows []store.RouteImportRow) *store.RouteImportStats
	importErr  error
	scanErr    error
}

var _ routeDB = (*fakeRouteDB)(nil)

func newFakeRouteDB() *fakeRouteDB {
	return &fakeRouteDB{rows: make(map[uint64]store.RouteRow)}
}

func (f *fakeRouteDB) ImportRoutes(_ context.Context, rows []store.RouteImportRow, dryRun bool) (store.RouteImportStats, error) {
	f.importCalls = append(f.importCalls, importCall{rows: append([]store.RouteImportRow(nil), rows...), dryRun: dryRun})
	if f.importErr != nil {
		return store.RouteImportStats{}, f.importErr
	}
	if f.importHook != nil {
		if st := f.importHook(len(f.importCalls), rows); st != nil {
			return *st, nil
		}
	}
	return store.RouteImportStats{Inserted: len(rows)}, nil
}

func (f *fakeRouteDB) ImportMergedInto(_ context.Context, rows []store.MergedIntoRow, dryRun bool) (store.MergedIntoImportStats, error) {
	f.mergedCalls = append(f.mergedCalls, mergedCall{rows: append([]store.MergedIntoRow(nil), rows...), dryRun: dryRun})
	return store.MergedIntoImportStats{Inserted: len(rows)}, nil
}

// ScanRoutes 与真 store 同口径:沿聚簇键 (route_shard, player_id) 翻页,返回键 > after 的至多 limit 行。
func (f *fakeRouteDB) ScanRoutes(_ context.Context, after store.RouteScanCursor, limit int) ([]store.RouteRow, error) {
	if f.scanErr != nil {
		return nil, f.scanErr
	}
	out := make([]store.RouteRow, 0, len(f.rows))
	for _, row := range f.rows {
		if after.Less(store.RouteScanCursorAt(row)) {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return store.RouteScanCursorAt(out[i]).Less(store.RouteScanCursorAt(out[j])) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRouteDB) GetRoutes(_ context.Context, ids []uint64) (map[uint64]store.RouteRow, error) {
	out := make(map[uint64]store.RouteRow)
	for _, id := range ids {
		if row, ok := f.rows[id]; ok {
			out[id] = row
		} else if row, ok := f.lateRows[id]; ok {
			out[id] = row
		}
	}
	return out, nil
}

func (f *fakeRouteDB) ListMergedInto(context.Context) ([]store.MergedIntoRow, error) {
	out := append([]store.MergedIntoRow(nil), f.merged...)
	sort.Slice(out, func(i, j int) bool { return out[i].SrcZone < out[j].SrcZone })
	return out, nil
}

// importedRows 按调用顺序摊平所有 ImportRoutes 收到的行。
func (f *fakeRouteDB) importedRows() []store.RouteImportRow {
	var out []store.RouteImportRow
	for _, c := range f.importCalls {
		out = append(out, c.rows...)
	}
	return out
}

// ── 公共装配 ──────────────────────────────────────────────────────────────────

func newMappingRedis(t *testing.T) (*miniredis.Miniredis, *redis.Redis) {
	t.Helper()
	mr := miniredis.RunT(t)
	return mr, redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
}

func setKeys(t *testing.T, mr *miniredis.Miniredis, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		require.NoError(t, mr.Set(k, v))
	}
}

func rec(storageID uint32, version uint64) *placement.Record {
	return &placement.Record{StorageID: storageID, Version: version}
}

type cliRun struct {
	code   int
	stdout string
	stderr string
}

// runImport 用小批量(2)跑一次 import,覆盖分批与凑批路径。
func runImport(t *testing.T, rd mappingReader, db routeDB, dryRun bool) (ImportReport, cliRun) {
	t.Helper()
	var out, errOut bytes.Buffer
	rep, err := Import(context.Background(), rd, db, ImportOptions{
		DryRun:    dryRun,
		Batch:     2,
		MaxReport: 100,
		Stdout:    &out,
		Stderr:    &errOut,
	})
	return rep, cliRun{code: ImportExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}
}

func noSleep(context.Context, time.Duration) error { return nil }

func runAudit(t *testing.T, rd mappingReader, db routeDB, maxRecheck int, sleep func(context.Context, time.Duration) error) (AuditReport, cliRun) {
	t.Helper()
	var out, errOut bytes.Buffer
	rep, err := Audit(context.Background(), rd, db, AuditOptions{
		Batch:      2,
		MaxReport:  100,
		MaxRecheck: maxRecheck,
		Stdout:     &out,
		Stderr:     &errOut,
		Sleep:      sleep,
	})
	return rep, cliRun{code: AuditExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}
}

// ── 规范形式 ──────────────────────────────────────────────────────────────────

// TestRouteToolKeyPrefixesMatchPlacement:本包镜像的前缀必须与 shared/placement 拼出来的键逐字节相同。
func TestRouteToolKeyPrefixesMatchPlacement(t *testing.T) {
	assert.Equal(t, placement.HomeZoneKey(7), homeZoneKeyPrefix+"7")
	assert.Equal(t, placement.MergedIntoKey(3), mergedIntoKeyPrefix+"3")
	assert.Equal(t, placement.Key(7), placement.KeyPrefix+"7")
	assert.Equal(t, placement.MapRevKey(7), placement.MapRevKeyPrefix+"7")
}

func TestRouteToolCanonicalForms(t *testing.T) {
	keyCases := []struct {
		key  string
		want uint64
		ok   bool
	}{
		{"player:zone:7", 7, true},
		{"player:zone:18446744073709551615", 18446744073709551615, true},
		{"player:zone:007", 0, false},
		{"player:zone:0", 0, false},
		{"player:zone:", 0, false},
		{"player:zone:+7", 0, false},
		{"player:zone:7:x", 0, false},
		{"player:zone:18446744073709551616", 0, false},
		{"player:placement:7", 0, false},
	}
	for _, c := range keyCases {
		got, ok := parseKeyPlayerID(c.key, homeZoneKeyPrefix)
		assert.Equal(t, c.ok, ok, c.key)
		assert.Equal(t, c.want, got, c.key)
	}

	for _, raw := range []string{"1", "12", "999999"} {
		_, err := parseCanonicalZone(raw)
		assert.NoError(t, err, raw)
	}
	for _, raw := range []string{"0", "0012", "01", "+1", " 1", "1000000", "x"} {
		_, err := parseCanonicalZone(raw)
		assert.Error(t, err, raw)
	}

	for _, raw := range []string{"1:1", "1000000:3", "2:5:frozen:5-6-20261012T000000Z-abcdef12"} {
		_, err := parseCanonicalPlacement(raw)
		assert.NoError(t, err, raw)
	}
	for _, raw := range []string{"01:1", "1:01", "1:0", "0:1", "1", "1:1:frozen:", "1:1:cold:r1", "1:1:frozen:bad id"} {
		_, err := parseCanonicalPlacement(raw)
		assert.Error(t, err, raw)
	}

	for _, raw := range []string{"1", "18446744073709551615"} {
		_, err := parseCanonicalMapRev(raw)
		assert.NoError(t, err, raw)
	}
	for _, raw := range []string{"0", "07", "-1", "1a", "18446744073709551616"} {
		_, err := parseCanonicalMapRev(raw)
		assert.Error(t, err, raw)
	}
}

// ── import ────────────────────────────────────────────────────────────────────

// TestRouteImportMergedIntoChangedInsideWindowExitsThree:导入窗口内跑过的合服 / 撤销合服,
// merged_into 的快照一定不同 → 退出码 3,统计行照出。
// 不抓的话导入以退出码 0 结束,留下的旧 home 行只有 audit 才能发现,重跑又只会判 conflict、永远收敛不了。
func TestRouteImportMergedIntoChangedInsideWindowExitsThree(t *testing.T) {
	cases := []struct {
		name string
		// change 在第一批 ImportRoutes 里执行 = 导入窗口内。
		change func(t *testing.T, mr *miniredis.Miniredis)
		// want 是 ABORT 行里应出现的差异项。
		want string
	}{
		{"merge ran: marker added", func(t *testing.T, mr *miniredis.Miniredis) {
			require.NoError(t, mr.Set("merge:merged_into:7", `{"dst":8,"run_id":"r7"}`))
		}, "+merge:merged_into:7"},
		{"unmerge ran: marker removed", func(t *testing.T, mr *miniredis.Miniredis) {
			mr.Del("merge:merged_into:5")
		}, "-merge:merged_into:5"},
		{"marker rewritten", func(t *testing.T, mr *miniredis.Miniredis) {
			require.NoError(t, mr.Set("merge:merged_into:5", `{"dst":9,"run_id":"r5b"}`))
		}, "~merge:merged_into:5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mr, rd := newMappingRedis(t)
			setKeys(t, mr, map[string]string{
				"player:zone:1":       "1",
				"merge:merged_into:5": `{"dst":6,"run_id":"r5"}`,
			})
			db := newFakeRouteDB()
			db.importHook = func(n int, _ []store.RouteImportRow) *store.RouteImportStats {
				if n == 1 {
					c.change(t, mr)
				}
				return nil
			}

			rep, run := runImport(t, rd, db, false)

			assert.Equal(t, ExitMergeChanged, run.code, run.stderr)
			assert.Equal(t, 1, rep.Inserted, "窗口内已写的部分不回滚,计数照实给出")
			assert.Contains(t, run.stdout, "route_import ")
			assert.Contains(t, run.stderr, "ABORT: merge:merged_into changed during the import")
			assert.Contains(t, run.stderr, c.want)
			assert.Contains(t, run.stderr, "after the import")
		})
	}
}

// TestRouteImportMergedIntoUnchangedIsClean:merged_into 在窗口内没变(只是存在)→ 不误报,退出码 0。
func TestRouteImportMergedIntoUnchangedIsClean(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{
		"player:zone:1":       "1",
		"merge:merged_into:5": `{"dst":6,"run_id":"r5"}`,
	})
	db := newFakeRouteDB()

	_, run := runImport(t, rd, db, false)

	assert.Equal(t, ExitClean, run.code, run.stderr)
	assert.NotContains(t, run.stderr, "ABORT")
}

// TestRouteImportMalformedPlayersAreSkippedWhole:任一值不规范 → 整名玩家跳过且计数;007 / 0012 / 01:1 都判畸形。
func TestRouteImportMalformedPlayersAreSkippedWhole(t *testing.T) {
	mr, rd := newMappingRedis(t)
	// 101 的落点不规范:zone 合法也不能导(不导半截);007 是键名后缀不规范;105 只在 B 遍出现。
	setKeys(t, mr, map[string]string{
		"player:zone:101":      "1",
		"player:placement:101": "01:1",
		"player:zone:102":      "0012",
		"player:zone:007":      "7",
		"player:zone:103":      "2",
		"player:maprev:103":    "07",
		"player:zone:104":      "3",
		"player:placement:104": "3:1",
		"player:placement:105": "x:1",
	})
	db := newFakeRouteDB()

	rep, run := runImport(t, rd, db, false)

	assert.Equal(t, ExitAttention, run.code)
	assert.Equal(t, 5, rep.Malformed)
	assert.Equal(t, 5, rep.PlayersScanned, "101..105;键名不规范的 007 不算玩家")
	assert.Equal(t, []store.RouteImportRow{{PlayerID: 104, HomeZone: 3, Placement: rec(3, 1)}}, db.importedRows())
	assert.Contains(t, run.stderr, `MALFORMED player=101 key=player:placement:101 value="01:1"`)
	assert.Contains(t, run.stderr, `MALFORMED player=102 key=player:zone:102 value="0012"`)
	assert.Contains(t, run.stderr, `key=player:zone:007`)
	assert.Contains(t, run.stderr, `MALFORMED player=103 key=player:maprev:103 value="07"`)
	assert.Contains(t, run.stderr, `MALFORMED player=105 key=player:placement:105 value="x:1"`)
}

// TestRouteImportPassBSkipsBySeenSetNotByZone:B 遍按「A 遍处理过」跳过,不按「zone 非空」跳过 ——
// A、B 两遍之间新写了 zone 的玩家(203)在 A 遍没被 SCAN 到,不能漏;A 遍处理过的(201)不能导两次。
func TestRouteImportPassBSkipsBySeenSetNotByZone(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{
		"player:zone:201":      "1",
		"player:placement:201": "1:1",
		"player:placement:202": "2:1",
		"player:placement:203": "3:1",
	})
	db := newFakeRouteDB()
	db.importHook = func(n int, _ []store.RouteImportRow) *store.RouteImportStats {
		if n == 1 { // A 遍的那一批:此时 B 遍还没开始
			require.NoError(t, mr.Set("player:zone:203", "3"))
		}
		return nil
	}

	rep, run := runImport(t, rd, db, false)

	assert.Equal(t, ExitClean, run.code, run.stderr)
	assert.Equal(t, []store.RouteImportRow{
		{PlayerID: 201, HomeZone: 1, Placement: rec(1, 1)},
		{PlayerID: 202, Placement: rec(2, 1)},
		{PlayerID: 203, HomeZone: 3, Placement: rec(3, 1)},
	}, db.importedRows())
	assert.Equal(t, 3, rep.PlayersScanned)
	assert.Equal(t, 3, rep.Inserted)
}

// TestRouteImportPassesMapRevAndReportsRegressionsAndConflicts:maprev 交给 ImportRoutes;
// rev_regression / conflict 只报告不覆盖,退出码 1。
func TestRouteImportPassesMapRevAndReportsRegressionsAndConflicts(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{
		"player:zone:301":   "1",
		"player:maprev:301": "5",
		"player:zone:302":   "2",
	})
	db := newFakeRouteDB()
	db.importHook = func(_ int, rows []store.RouteImportRow) *store.RouteImportStats {
		st := &store.RouteImportStats{}
		for _, r := range rows {
			switch r.PlayerID {
			case 301:
				st.RevRegressions = append(st.RevRegressions, store.RouteImportConflict{
					PlayerID: 301, DB: store.RouteRow{PlayerID: 301, HomeZone: 1, Rev: 2}, Redis: r})
			case 302:
				st.Conflicts = append(st.Conflicts, store.RouteImportConflict{
					PlayerID: 302, DB: store.RouteRow{PlayerID: 302, HomeZone: 9, Rev: 1}, Redis: r})
			}
		}
		return st
	}

	rep, run := runImport(t, rd, db, false)

	assert.Equal(t, ExitAttention, run.code)
	assert.Equal(t, []store.RouteImportRow{
		{PlayerID: 301, HomeZone: 1, MapRev: 5},
		{PlayerID: 302, HomeZone: 2},
	}, db.importedRows())
	assert.Equal(t, 1, rep.RevRegressions)
	assert.Equal(t, 1, rep.Conflicts)
	assert.Contains(t, run.stderr, "REV_REGRESSION player=301 db_rev=2 redis_maprev=5")
	assert.Contains(t, run.stderr, `CONFLICT player=302 db_home=9 db_placement="" db_rev=1 redis_home=2`)
}

// TestRouteImportMergedIntoValidation:merged_into 的 JSON 畸形、run_id 非法、dst == src、键名不规范都判畸形;
// 未知字段放行(字段只增不改名)。
func TestRouteImportMergedIntoValidation(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{
		"merge:merged_into:5":   `{"dst":6,"run_id":"5-6-20261012T000000Z-abcdef12"}`,
		"merge:merged_into:11":  `{"dst":12,"run_id":"r11","started_at":"x"}`,
		"merge:merged_into:7":   `not-json`,
		"merge:merged_into:8":   `{"dst":9,"run_id":"bad id!"}`,
		"merge:merged_into:9":   `{"dst":9,"run_id":"r9"}`,
		"merge:merged_into:010": `{"dst":3,"run_id":"r10"}`,
	})
	db := newFakeRouteDB()

	rep, run := runImport(t, rd, db, false)

	assert.Equal(t, ExitAttention, run.code)
	assert.Equal(t, 4, rep.MergedIntoMalformed)
	assert.Equal(t, 2, rep.MergedIntoInserted)
	var got []store.MergedIntoRow
	for _, c := range db.mergedCalls {
		got = append(got, c.rows...)
	}
	assert.Equal(t, []store.MergedIntoRow{
		{SrcZone: 5, DstZone: 6, RunID: "5-6-20261012T000000Z-abcdef12"},
		{SrcZone: 11, DstZone: 12, RunID: "r11"},
	}, got)
	assert.Contains(t, run.stderr, "MALFORMED merged_into key=merge:merged_into:7")
	assert.Contains(t, run.stderr, "key=merge:merged_into:010")
}

// TestRouteImportDryRunPassesThroughAndNoteIsFirstLine:-dry-run 原样交给 store;stderr 第一行是 NOTE;
// 干净的一次运行退出码 0,stdout 恰好一行。
func TestRouteImportDryRunPassesThroughAndNoteIsFirstLine(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{
		"player:zone:1":       "1",
		"merge:merged_into:5": `{"dst":6,"run_id":"r5"}`,
	})
	db := newFakeRouteDB()

	rep, run := runImport(t, rd, db, true)

	assert.Equal(t, ExitClean, run.code, run.stderr)
	require.NotEmpty(t, db.importCalls)
	require.NotEmpty(t, db.mergedCalls)
	for _, c := range db.importCalls {
		assert.True(t, c.dryRun)
	}
	for _, c := range db.mergedCalls {
		assert.True(t, c.dryRun)
	}
	assert.True(t, rep.DryRun)
	assert.Equal(t, ImportNote, strings.SplitN(run.stderr, "\n", 2)[0])
	assert.Equal(t, 1, strings.Count(run.stdout, "\n"), run.stdout)
	assert.True(t, strings.HasPrefix(run.stdout, "route_import dry_run=true players_scanned=1 inserted=1 "), run.stdout)
}

// TestRouteImportStoreErrorExitsTwo:库报错(含 1205)→ 退出码 2,不出统计行。
func TestRouteImportStoreErrorExitsTwo(t *testing.T) {
	mr, rd := newMappingRedis(t)
	setKeys(t, mr, map[string]string{"player:zone:1": "1"})
	db := newFakeRouteDB()
	db.importErr = errors.New("Error 1205: Lock wait timeout exceeded")

	_, run := runImport(t, rd, db, false)

	assert.Equal(t, ExitFailed, run.code)
	assert.Empty(t, run.stdout)
}

// ── audit ─────────────────────────────────────────────────────────────────────

// TestRouteAuditClassifiesBothDirections:两个方向的每一类差异各造一例(含 maprev 族、db_only_unpublished、
// redis_rev_ahead、墓碑),复核读到同样的东西,全部计入。
func TestRouteAuditClassifiesBothDirections(t *testing.T) {
	mr, rd := newMappingRedis(t)
	db := newFakeRouteDB()
	// 1 一致;2 db_only_unpublished;3 落点 missing_in_redis;4 home missing_in_db;5 value_mismatch;
	// 6 redis_malformed("06");7 db_invalid(version 0);8 redis_rev_ahead;11 墓碑 = 没有记录(缓存也没有,不算差异)。
	// 9 只有 maprev、10 只有 zone,库里都没有行:missing_in_db(方向 B)。player:placement:012 键名不规范。
	for _, row := range []store.RouteRow{
		{PlayerID: 1, HomeZone: 1, Rev: 1},
		{PlayerID: 2, HomeZone: 2, Rev: 1},
		{PlayerID: 3, HomeZone: 3, StorageID: 3, PlacementVersion: 1, Rev: 2},
		{PlayerID: 4, StorageID: 4, PlacementVersion: 1, Rev: 1},
		{PlayerID: 5, HomeZone: 5, Rev: 1},
		{PlayerID: 6, HomeZone: 6, Rev: 1},
		{PlayerID: 7, HomeZone: 7, StorageID: 7, Rev: 1},
		{PlayerID: 8, HomeZone: 8, Rev: 2},
		{PlayerID: 11, Rev: 1},
	} {
		db.rows[row.PlayerID] = row
	}
	// merged_into:20 一致;22 只在库里;24 只在 Redis;26 dst 不同。
	db.merged = []store.MergedIntoRow{
		{SrcZone: 20, DstZone: 21, RunID: "r1"},
		{SrcZone: 22, DstZone: 23, RunID: "r2"},
		{SrcZone: 26, DstZone: 27, RunID: "r4"},
	}
	setKeys(t, mr, map[string]string{
		"player:zone:1":        "1",
		"player:zone:3":        "3",
		"player:zone:4":        "4",
		"player:placement:4":   "4:1",
		"player:zone:5":        "6",
		"player:zone:6":        "06",
		"player:zone:8":        "8",
		"player:maprev:8":      "5",
		"player:maprev:9":      "3",
		"player:zone:10":       "10",
		"player:placement:012": "1:1",
		"merge:merged_into:20": `{"dst":21,"run_id":"r1"}`,
		"merge:merged_into:24": `{"dst":25,"run_id":"r3"}`,
		"merge:merged_into:26": `{"dst":28,"run_id":"r4"}`,
	})

	sleeps := 0
	rep, run := runAudit(t, rd, db, 100, func(context.Context, time.Duration) error { sleeps++; return nil })

	assert.Equal(t, AuditReport{
		DBRows:                   9,
		RedisPlayers:             8,
		DBOnlyUnpublished:        1,
		MissingInRedis:           1,
		MissingInDB:              3,
		ValueMismatch:            1,
		RedisMalformed:           2,
		DBInvalid:                1,
		RedisRevAhead:            1,
		MergedIntoMissingInRedis: 1,
		MergedIntoMissingInDB:    1,
		MergedIntoMismatch:       1,
	}, rep)
	assert.Equal(t, ExitAttention, run.code)
	assert.Equal(t, 1, sleeps, "差异玩家没超过 -max-recheck,复核一次")
	assert.Contains(t, run.stderr, "DIFF kind=db_only_unpublished player=2 ")
	assert.Contains(t, run.stderr, "DIFF kind=redis_rev_ahead player=8 ")
	assert.Contains(t, run.stderr, "DIFF kind=missing_in_db player=9 db=none ")
	assert.True(t, strings.HasPrefix(run.stdout, "route_audit db_rows=9 redis_players=8 "), run.stdout)
}

// TestRouteAuditComparesRowsCommittedAfterDirectionA:方向 A 的键集翻页扫过之后才提交的行(db_first 下审计期间
// 新登记的玩家),方向 B 用 GetRoutes 看到它有行时也要按库行比对一次 —— 以前按「库里有行就跳过」,这些行在本次审计里
// 完全没比对过,差异被静默漏掉。
func TestRouteAuditComparesRowsCommittedAfterDirectionA(t *testing.T) {
	mr, rd := newMappingRedis(t)
	db := newFakeRouteDB()
	// 1 方向 A 扫得到、一致;5 / 7 只有 GetRoutes 看得到:5 与缓存不一致(value_mismatch),7 一致(不算差异)。
	db.rows[1] = store.RouteRow{PlayerID: 1, HomeZone: 1, Rev: 1}
	db.lateRows = map[uint64]store.RouteRow{
		5: {PlayerID: 5, HomeZone: 5, Rev: 1},
		7: {PlayerID: 7, HomeZone: 7, Rev: 1},
	}
	setKeys(t, mr, map[string]string{
		"player:zone:1": "1",
		"player:zone:5": "6",
		"player:zone:7": "7",
	})

	rep, run := runAudit(t, rd, db, 10, noSleep)

	assert.Equal(t, AuditReport{DBRows: 1, RedisPlayers: 3, ValueMismatch: 1}, rep)
	assert.Equal(t, ExitAttention, run.code)
	assert.Contains(t, run.stderr, `DIFF kind=value_mismatch player=5 db={home="5" `)
	assert.NotContains(t, run.stderr, "kind=missing_in_db", "库里有行的 id 不能被当成「库缺」")
}

// TestRouteAuditRecheckClearsTransientDifferences:首读的差异在复核时已消失(途中的 W1 发布完了 / 导入补上了行)→ 不计入,
// 记 transient_cleared,退出码 0。
func TestRouteAuditRecheckClearsTransientDifferences(t *testing.T) {
	mr, rd := newMappingRedis(t)
	db := newFakeRouteDB()
	// 首读:1 在库里、缓存三键全无;2 在缓存里、库里没有行。复核前两边都补齐。
	db.rows[1] = store.RouteRow{PlayerID: 1, HomeZone: 1, Rev: 1}
	setKeys(t, mr, map[string]string{"player:zone:2": "2"})

	var out, errOut bytes.Buffer
	var waited []time.Duration
	rep, err := Audit(context.Background(), rd, db, AuditOptions{
		Batch:        2,
		MaxReport:    100,
		RecheckDelay: 2 * time.Second,
		MaxRecheck:   10,
		Stdout:       &out,
		Stderr:       &errOut,
		Sleep: func(_ context.Context, d time.Duration) error {
			waited = append(waited, d)
			require.NoError(t, mr.Set("player:zone:1", "1"))
			db.rows[2] = store.RouteRow{PlayerID: 2, HomeZone: 2, Rev: 1}
			return nil
		},
	})

	require.NoError(t, err)
	assert.Equal(t, []time.Duration{2 * time.Second}, waited)
	assert.Equal(t, 2, rep.TransientCleared)
	assert.Equal(t, 0, rep.Differences())
	assert.Equal(t, ExitClean, AuditExitCode(rep, err))
}

// TestRouteAuditOverMaxRecheckCountsWithoutRecheck:首读差异玩家超过 -max-recheck → 不复核、全部计入,打 WARN。
func TestRouteAuditOverMaxRecheckCountsWithoutRecheck(t *testing.T) {
	_, rd := newMappingRedis(t)
	db := newFakeRouteDB()
	db.rows[1] = store.RouteRow{PlayerID: 1, HomeZone: 1, Rev: 1}
	db.rows[2] = store.RouteRow{PlayerID: 2, HomeZone: 2, Rev: 1}

	rep, run := runAudit(t, rd, db, 1, func(context.Context, time.Duration) error {
		t.Fatal("超过 -max-recheck 时不应复核")
		return nil
	})

	assert.Equal(t, 2, rep.DBOnlyUnpublished)
	assert.Equal(t, 0, rep.TransientCleared)
	assert.Equal(t, ExitAttention, run.code)
	assert.Contains(t, run.stderr, "WARN: more than -max-recheck=1 players differ")
	assert.Contains(t, run.stderr, "DIFF kind=db_only_unpublished player=1 ")
}

// TestRouteAuditExitCodes:干净 → 0 且 stdout 恰好一行;库查询失败 → 2 且不出统计行。
func TestRouteAuditExitCodes(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		db := newFakeRouteDB()
		db.rows[1] = store.RouteRow{PlayerID: 1, HomeZone: 1, StorageID: 1, PlacementVersion: 1, Rev: 1}
		setKeys(t, mr, map[string]string{"player:zone:1": "1", "player:placement:1": "1:1"})

		rep, run := runAudit(t, rd, db, 10, noSleep)

		assert.Equal(t, ExitClean, run.code, run.stderr)
		assert.Equal(t, 0, rep.Differences())
		assert.Equal(t, 1, strings.Count(run.stdout, "\n"), run.stdout)
		assert.True(t, strings.HasPrefix(run.stdout, "route_audit db_rows=1 redis_players=1 "), run.stdout)
	})
	t.Run("db error", func(t *testing.T) {
		_, rd := newMappingRedis(t)
		db := newFakeRouteDB()
		db.scanErr = errors.New("db down")

		_, run := runAudit(t, rd, db, 10, noSleep)

		assert.Equal(t, ExitFailed, run.code)
		assert.Empty(t, run.stdout)
	})
}

// TestRouteAuditPagesAcrossShards:方向 A 沿聚簇键翻页(G1a),游标是 (route_shard, player_id) 而不是 player_id。
// id {1, 65, 129, 2, 66, 63, 127} 的行片是 1,1,1,2,2,63,63(64 片:相差 64 才落同一片),Batch=2 时三处页边界
// (1, 65) | (1, 129)、(2, 2) | (2, 66)、(63, 63) | (63, 127) 都落在片中间,第 2、3 页各跨一次片:按 player_id
// 当游标会漏掉 2 / 63 之后的行,按片当游标会漏掉同片的后续行。两边一致时差异为 0;
// 删掉其中一名玩家的 Redis 键,恰好多出 1 个差异 —— 证明每一行都被比到了一次,不漏不重。
func TestRouteAuditPagesAcrossShards(t *testing.T) {
	mr, rd := newMappingRedis(t)
	db := newFakeRouteDB()
	// 65 = 64 + 1、129 = 2×64 + 1(片 1);66 = 64 + 2(片 2);63 与 127 = 64 + 63(片 63,最后一片)。
	ids := []uint64{1, 65, 129, 2, 66, 63, 127}
	// 夹具自检:上面的 id 若不再「三个同片、两个同片、两个同片」(模数变了),用例会悄悄退化成每片一行。
	wantShards := []uint32{1, 1, 1, 2, 2, 63, 63}
	for i, id := range ids {
		require.Equalf(t, wantShards[i], placement.RouteRowShard(id), "id %d 的行片", id)
	}
	for _, id := range ids {
		db.rows[id] = store.RouteRow{PlayerID: id, HomeZone: 1, Rev: 1}
		require.NoError(t, mr.Set(placement.HomeZoneKey(id), "1"))
	}

	rep, run := runAudit(t, rd, db, 100, noSleep)
	assert.Equal(t, ExitClean, run.code, run.stderr)
	assert.Equal(t, len(ids), rep.DBRows)
	assert.Equal(t, 0, rep.Differences())

	mr.Del(placement.HomeZoneKey(66))
	rep, run = runAudit(t, rd, db, 100, noSleep)
	assert.Equal(t, ExitAttention, run.code)
	assert.Equal(t, len(ids), rep.DBRows)
	assert.Equal(t, 1, rep.Differences())
	assert.Equal(t, 1, rep.DBOnlyUnpublished)
	assert.Contains(t, run.stderr, "DIFF kind=db_only_unpublished player=66 ")
}

// ── tidb-prepare ──────────────────────────────────────────────────────────────

// fakeTiDBPreparer 按预置结果回答 PrepareTiDB,并记下入参。
type fakeTiDBPreparer struct {
	rep  store.RouteTiDBPrepareReport
	err  error
	reqs []store.RouteTiDBPrepareRequest
}

func (f *fakeTiDBPreparer) PrepareTiDB(_ context.Context, req store.RouteTiDBPrepareRequest) (store.RouteTiDBPrepareReport, error) {
	f.reqs = append(f.reqs, req)
	return f.rep, f.err
}

// TestRouteTiDBPrepareOutputAndExitCodes:MySQL 跳过 → 退出 0、stdout 一行统计、stderr 有 SKIP;TiDB dry-run → 每步一行 PLAN、
// executed=0;真跑 → 每步一行 DONE;出错 → 退出 2、stdout 为空、stderr 有 FAILED(已执行的步骤照列)。
func TestRouteTiDBPrepareOutputAndExitCodes(t *testing.T) {
	run := func(p *fakeTiDBPreparer, dryRun bool) cliRun {
		var out, errOut bytes.Buffer
		rep, err := TiDBPrepare(context.Background(), p, TiDBPrepareOptions{
			Zones:    []uint32{2, 1, 2},
			Storages: []uint32{1000000},
			DryRun:   dryRun,
			Target:   "route_tool tidb-prepare: global_db=127.0.0.1:4000/testdb",
			Stdout:   &out,
			Stderr:   &errOut,
		})
		return cliRun{code: TiDBPrepareExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}
	}
	steps := func(executed bool) []store.RouteTiDBPrepareStep {
		return []store.RouteTiDBPrepareStep{
			{SQL: "ALTER TABLE `player_route` ATTRIBUTES 'merge_option=deny'", Executed: executed},
			{SQL: "SPLIT TABLE `player_route` BY (1, 0), (2, 0)", Executed: executed, SplitRegions: map[bool]int64{true: 2}[executed]},
		}
	}

	t.Run("MySQL跳过", func(t *testing.T) {
		p := &fakeTiDBPreparer{rep: store.RouteTiDBPrepareReport{Engine: "mysql", Version: "8.0.36", Skipped: true}}
		r := run(p, false)
		assert.Equal(t, ExitClean, r.code, r.stderr)
		assert.Equal(t, 1, strings.Count(r.stdout, "\n"), r.stdout)
		assert.Contains(t, r.stdout, "route_tidb_prepare engine=mysql skipped=true ")
		assert.Contains(t, r.stdout, " zones=2 storages=1 statements=0 executed=0 split_regions=0")
		assert.Contains(t, r.stderr, "SKIP: 8.0.36 is not TiDB")
		assert.True(t, strings.HasPrefix(r.stderr, "route_tool tidb-prepare: global_db="), "stderr 第一行是连接目标")
		require.Len(t, p.reqs, 1)
		assert.Equal(t, []uint32{2, 1, 2}, p.reqs[0].Zones, "入参原样交给 store(规范化在 store 里)")
	})

	t.Run("TiDB_dry-run", func(t *testing.T) {
		p := &fakeTiDBPreparer{rep: store.RouteTiDBPrepareReport{Engine: "tidb", Version: "8.0.11-TiDB-v8.5.2", Steps: steps(false)}}
		r := run(p, true)
		assert.Equal(t, ExitClean, r.code, r.stderr)
		assert.True(t, p.reqs[0].DryRun)
		assert.Contains(t, r.stderr, "PLAN ALTER TABLE `player_route` ATTRIBUTES 'merge_option=deny'\n")
		assert.Contains(t, r.stderr, "PLAN SPLIT TABLE `player_route` BY (1, 0), (2, 0)\n")
		assert.NotContains(t, r.stderr, "DONE ")
		assert.Contains(t, r.stdout, "route_tidb_prepare engine=tidb skipped=false dry_run=true ")
		assert.Contains(t, r.stdout, " statements=2 executed=0 split_regions=0")
	})

	t.Run("TiDB_真跑", func(t *testing.T) {
		p := &fakeTiDBPreparer{rep: store.RouteTiDBPrepareReport{Engine: "tidb", Version: "8.0.11-TiDB-v8.5.2", Steps: steps(true)}}
		r := run(p, false)
		assert.Equal(t, ExitClean, r.code, r.stderr)
		assert.Contains(t, r.stderr, "DONE SPLIT TABLE `player_route` BY (1, 0), (2, 0) split_regions=2\n")
		assert.Contains(t, r.stdout, " statements=2 executed=2 split_regions=2")
	})

	t.Run("失败_退出2_stdout为空", func(t *testing.T) {
		failed := steps(false)
		failed[0].Executed = true
		p := &fakeTiDBPreparer{
			rep: store.RouteTiDBPrepareReport{Engine: "tidb", Version: "8.0.11-TiDB-v8.5.2", Steps: failed},
			err: errors.New("tidb-prepare step 2/2: SPLIT TABLE `player_route` BY (1, 0), (2, 0): Error 1105"),
		}
		r := run(p, false)
		assert.Equal(t, ExitFailed, r.code)
		assert.Empty(t, r.stdout)
		assert.Contains(t, r.stderr, "DONE ALTER TABLE `player_route`")
		assert.Contains(t, r.stderr, "FAILED: tidb-prepare step 2/2")
	})

	t.Run("没给writer_退出2", func(t *testing.T) {
		rep, err := TiDBPrepare(context.Background(), &fakeTiDBPreparer{}, TiDBPrepareOptions{Zones: []uint32{1}})
		assert.Equal(t, ExitFailed, TiDBPrepareExitCode(rep, err))
	})
}

// ── audit -require-maprev(2026-10-19 第 1b-2 批收尾)──────────────────────────────────────

// runAuditWith 用小批量(2)跑一次 audit,口径开关由 opts 给(Batch / MaxReport / MaxRecheck / 输出 / Sleep 在这里填)。
func runAuditWith(t *testing.T, rd mappingReader, db routeDB, opts AuditOptions) (AuditReport, cliRun) {
	t.Helper()
	var out, errOut bytes.Buffer
	opts.Batch, opts.MaxReport, opts.MaxRecheck = 2, 100, 100
	opts.Stdout, opts.Stderr, opts.Sleep = &out, &errOut, noSleep
	rep, err := Audit(context.Background(), rd, db, opts)
	return rep, cliRun{code: AuditExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}
}

// maprevAuditFixture 造一份覆盖 redis_rev_behind 判据各分支的数据:
//
//	1 maprev = 库 rev,值一致 → 不算;2 缺 maprev(值一致)→ behind;3 maprev < 库 rev(值一致)→ behind;
//	4 缓存三键全无 → 只计 db_only_unpublished,不重复计 behind;5 墓碑、缓存也没有 → 不算;
//	6 缓存值畸形 → 只计 redis_malformed;7 maprev > 库 rev → redis_rev_ahead,不是 behind;
//	8 只有 maprev(1)、库里只有落点且 rev 3 → missing_in_redis 加 behind(缓存有键,maprev 落后)。
func maprevAuditFixture(t *testing.T) (*miniredis.Miniredis, *redis.Redis, *fakeRouteDB) {
	t.Helper()
	mr, rd := newMappingRedis(t)
	db := newFakeRouteDB()
	for _, row := range []store.RouteRow{
		{PlayerID: 1, HomeZone: 1, Rev: 3},
		{PlayerID: 2, HomeZone: 2, Rev: 3},
		{PlayerID: 3, HomeZone: 3, Rev: 5},
		{PlayerID: 4, HomeZone: 4, Rev: 2},
		{PlayerID: 5, Rev: 2},
		{PlayerID: 6, HomeZone: 6, Rev: 1},
		{PlayerID: 7, HomeZone: 7, Rev: 2},
		{PlayerID: 8, StorageID: 8, PlacementVersion: 1, Rev: 3},
	} {
		db.rows[row.PlayerID] = row
	}
	setKeys(t, mr, map[string]string{
		"player:zone:1":   "1",
		"player:maprev:1": "3",
		"player:zone:2":   "2",
		"player:zone:3":   "3",
		"player:maprev:3": "4",
		"player:zone:6":   "06",
		"player:zone:7":   "7",
		"player:maprev:7": "9",
		"player:maprev:8": "1",
	})
	return mr, rd, db
}

// TestAudit_RequireMapRevCountsBehind:严格 maprev 口径下,库行不是墓碑、缓存里至少有一把键,而 maprev 缺席或小于库 rev → redis_rev_behind,
// 计入 Differences 与退出码 1,统计行末尾多 redis_rev_behind=N,明细 DIFF kind=redis_rev_behind;复核用同一口径(两次都在才计)。
// 与 -frozen-recovery 同用时它照常计入退出码(-frozen-recovery 只豁免 db_only_unpublished)。
func TestAudit_RequireMapRevCountsBehind(t *testing.T) {
	_, rd, db := maprevAuditFixture(t)

	rep, run := runAuditWith(t, rd, db, AuditOptions{RequireMapRev: true})

	assert.Equal(t, 3, rep.RedisRevBehind, "2、3、8")
	assert.Equal(t, 1, rep.DBOnlyUnpublished, "4 只计 db_only_unpublished")
	assert.Equal(t, 1, rep.RedisMalformed, "6 只计 redis_malformed")
	assert.Equal(t, 1, rep.RedisRevAhead, "7 是 ahead 不是 behind")
	assert.Equal(t, 1, rep.MissingInRedis, "8 的落点缓存里没有")
	assert.True(t, rep.RequireMapRev)
	assert.Equal(t, 7, rep.Differences())
	assert.Equal(t, ExitAttention, run.code)
	for _, id := range []string{"2", "3", "8"} {
		assert.Contains(t, run.stderr, "DIFF kind=redis_rev_behind player="+id+" ")
	}
	for _, id := range []string{"1", "4", "5", "6", "7"} {
		assert.NotContains(t, run.stderr, "DIFF kind=redis_rev_behind player="+id+" ")
	}
	assert.True(t, strings.HasSuffix(run.stdout, " transient_cleared=0 redis_rev_behind=3\n"), run.stdout)
	assert.Contains(t, run.stderr, "NOTE: -require-maprev")

	t.Run("与-frozen-recovery同用", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rd := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
		db := newFakeRouteDB()
		db.rows[1] = store.RouteRow{PlayerID: 1, HomeZone: 1, Rev: 2} // 孤儿行:豁免
		db.rows[2] = store.RouteRow{PlayerID: 2, HomeZone: 2, Rev: 2} // maprev 落后:不豁免
		setKeys(t, mr, map[string]string{"player:zone:2": "2", "player:maprev:2": "1"})

		rep, run := runAuditWith(t, rd, db, AuditOptions{RequireMapRev: true, FrozenRecovery: true})
		assert.Equal(t, 1, rep.DBOnlyUnpublished)
		assert.Equal(t, 1, rep.RedisRevBehind)
		assert.Equal(t, 1, rep.BlockingDifferences())
		assert.Equal(t, ExitAttention, run.code)

		require.NoError(t, mr.Set("player:maprev:2", "2"))
		rep, run = runAuditWith(t, rd, db, AuditOptions{RequireMapRev: true, FrozenRecovery: true})
		assert.Zero(t, rep.RedisRevBehind)
		assert.Equal(t, ExitClean, run.code, "只剩被豁免的孤儿行")
	})
}

// TestAudit_DefaultIgnoresMapRev:默认口径(不加 -require-maprev)与 1a / 1b-1 逐字相同:maprev 缺席或落后不算差异,
// 统计行没有 redis_rev_behind,同一份数据里只有 behind 类的行时退出码 0。
func TestAudit_DefaultIgnoresMapRev(t *testing.T) {
	_, rd, db := maprevAuditFixture(t)

	rep, run := runAuditWith(t, rd, db, AuditOptions{})

	assert.Zero(t, rep.RedisRevBehind)
	assert.False(t, rep.RequireMapRev)
	assert.Equal(t, 4, rep.Differences(), "db_only_unpublished、redis_malformed、redis_rev_ahead、missing_in_redis 各 1")
	assert.NotContains(t, run.stdout, "redis_rev_behind")
	assert.NotContains(t, run.stderr, "redis_rev_behind")
	assert.True(t, strings.HasSuffix(run.stdout, " transient_cleared=0\n"), run.stdout)

	t.Run("只有behind类的行_退出码0", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rd := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
		db := newFakeRouteDB()
		db.rows[2] = store.RouteRow{PlayerID: 2, HomeZone: 2, Rev: 3}
		db.rows[3] = store.RouteRow{PlayerID: 3, HomeZone: 3, Rev: 5}
		setKeys(t, mr, map[string]string{"player:zone:2": "2", "player:zone:3": "3", "player:maprev:3": "4"})

		rep, run := runAuditWith(t, rd, db, AuditOptions{})
		assert.Equal(t, ExitClean, run.code, run.stderr)
		assert.Zero(t, rep.Differences())

		rep, run = runAuditWith(t, rd, db, AuditOptions{RequireMapRev: true})
		assert.Equal(t, ExitAttention, run.code)
		assert.Equal(t, 2, rep.RedisRevBehind)
	})
}
