package main

// 别名合服的流程用例:步骤顺序、续跑幂等、连续合服的拒绝规则、撤销之后映射消失。
//
// 库原语与发布层用接口替身(memRouteDB / memRouteCache):本组用例只证「本层的推进规则」。原语自身的 SQL 语句序列由
// route_store_test.go 的假驱动钉住,真库语义由 merge_integration 用例证。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	testSrcZone = uint32(901)
	testDstZone = uint32(902)
	testRunID   = "901-902-20261030T000000Z-aaaaaaaa"
)

// ── 路由库与发布层的替身 ──────────────────────────────────────────────────────

// memRouteDB 是 routeDBOps 的内存替身:zone_merged_into 一个 map,player_route 按 home_zone 分组。
type memRouteDB struct {
	merged  map[uint32]mergedIntoValue
	homes   map[uint32][]uint64
	readErr error
	markErr error
	marks   int
	clears  int
}

func newMemRouteDB() *memRouteDB {
	return &memRouteDB{merged: map[uint32]mergedIntoValue{}, homes: map[uint32][]uint64{}}
}

func (d *memRouteDB) ListMergedInto(context.Context) (map[uint32]uint32, error) {
	if d.readErr != nil {
		return nil, d.readErr
	}
	out := map[uint32]uint32{}
	for src, v := range d.merged {
		out[src] = v.Dst
	}
	return out, nil
}

func (d *memRouteDB) ScanByHome(_ context.Context, zone uint32, after routeScanCursor, limit int) ([]mergeRouteRow, error) {
	if d.readErr != nil {
		return nil, d.readErr
	}
	var rows []mergeRouteRow
	for _, id := range d.homes[zone] {
		if after.less(routeCursorOf(id)) {
			rows = append(rows, mergeRouteRow{PlayerID: id, HomeZone: zone})
		}
	}
	// 真实顺序是聚簇键序,与 player_id 序不同。
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && routeCursorOf(rows[j].PlayerID).less(routeCursorOf(rows[j-1].PlayerID)); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (d *memRouteDB) ReadMergedInto(_ context.Context, src uint32) (uint32, string, bool, error) {
	if d.readErr != nil {
		return 0, "", false, d.readErr
	}
	v, ok := d.merged[src]
	return v.Dst, v.RunID, ok, nil
}

func (d *memRouteDB) MarkMergedInto(_ context.Context, src, dst uint32, markRunID string) (string, error) {
	d.marks++
	if d.markErr != nil {
		return "", d.markErr
	}
	if cur, ok := d.merged[src]; ok && cur.Dst != dst {
		return "", errRouteMergedIntoConflict
	}
	if _, ok := d.merged[dst]; ok {
		return "", errRouteMergedIntoConflict
	}
	d.merged[src] = mergedIntoValue{Dst: dst, RunID: markRunID}
	return encodeRouteMergedIntoValue(dst, markRunID)
}

func (d *memRouteDB) ClearMergedInto(_ context.Context, src, dst uint32, markRunID string) (bool, error) {
	d.clears++
	cur, ok := d.merged[src]
	if !ok || cur.Dst != dst || cur.RunID != markRunID {
		return false, nil
	}
	delete(d.merged, src)
	return true, nil
}

// memRouteCache 是 routeCacheOps 的内存替身。
type memRouteCache struct {
	mirror map[uint32]string
	setErr error
	delErr error
}

func (c *memRouteCache) setMergedIntoMirror(_ context.Context, src uint32, value string) error {
	if c.setErr != nil {
		return c.setErr
	}
	c.mirror[src] = value
	return nil
}

func (c *memRouteCache) deleteMergedIntoMirror(_ context.Context, src uint32, value string) (bool, error) {
	if c.delErr != nil {
		return false, c.delErr
	}
	if c.mirror[src] != value {
		return false, nil
	}
	delete(c.mirror, src)
	return true, nil
}

func newMemBackend() (*routeBackend, *memRouteDB, *memRouteCache) {
	db, cache := newMemRouteDB(), &memRouteCache{mirror: map[uint32]string{}}
	return &routeBackend{db: db, pub: cache}, db, cache
}

// ── 步骤的替身 ────────────────────────────────────────────────────────────────

// recordingSteps 是 mergeStepOps 的替身:按发生顺序记下每一步与每次落盘,fail 里的步骤返回错误(一次性)。
type recordingSteps struct {
	events []string
	fail   map[string]error
}

func (r *recordingSteps) do(name string) (string, error) {
	r.events = append(r.events, "run:"+name)
	if err := r.fail[name]; err != nil {
		delete(r.fail, name)
		return "", err
	}
	return "detail of " + name, nil
}

func (r *recordingSteps) writeMergedInto(context.Context) (string, error) {
	return r.do(stepMergedInto)
}
func (r *recordingSteps) settleMergedInto(context.Context) (string, error) {
	return r.do(stepMergedIntoSettle)
}
func (r *recordingSteps) migrateGuilds(context.Context) (string, error) { return r.do(stepGuildMySQL) }
func (r *recordingSteps) migrateTrade(context.Context) (string, error)  { return r.do(stepTradeMySQL) }
func (r *recordingSteps) mergeRank(context.Context) (string, error)     { return r.do(stepGuildRank) }

func (r *recordingSteps) ran() []string {
	var out []string
	for _, e := range r.events {
		if s, ok := strings.CutPrefix(e, "run:"); ok {
			out = append(out, s)
		}
	}
	return out
}

// manifestHarness 把 runMergeSteps 接到一份真落盘的清单上:续跑用例从磁盘读回来,证明进度确实落盘了。
type manifestHarness struct {
	t    *testing.T
	path string
}

func newManifestHarness(t *testing.T) *manifestHarness {
	t.Helper()
	h := &manifestHarness{t: t, path: filepath.Join(t.TempDir(), "merge.json")}
	if err := saveManifest(h.path, newMergeManifest(testRunID, testSrcZone, testDstZone, "op", time.Now())); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *manifestHarness) run(ops *recordingSteps, o options, gate writeGate) (*mergeManifest, error) {
	h.t.Helper()
	m, err := loadRunManifest(h.path)
	if err != nil || m == nil {
		h.t.Fatalf("load manifest: (%v, %v)", m, err)
	}
	persist := func(step, detail string) error {
		ops.events = append(ops.events, "persist:"+step)
		m.markStep(step, detail)
		return saveManifest(h.path, m)
	}
	err = runMergeSteps(context.Background(), mergeStepPlan(ops, o), m, gate, o.dryRun, persist)
	return m, err
}

// ── 合服顺序 ──────────────────────────────────────────────────────────────────

// TestMergeSteps_MappingIsWrittenBeforeAnyBusinessStep:映射落盘之后才等缓存,等完缓存才动业务数据;完成步骤最后写。
// 映射先写才不会有漏网的行,顺序本身是正确性的一部分。
func TestMergeSteps_MappingIsWrittenBeforeAnyBusinessStep(t *testing.T) {
	h := newManifestHarness(t)
	ops := &recordingSteps{}
	m, err := h.run(ops, options{}, nil)
	if err != nil {
		t.Fatalf("runMergeSteps: %v", err)
	}
	want := []string{
		"run:" + stepMergedInto, "persist:" + stepMergedInto,
		"run:" + stepMergedIntoSettle, "persist:" + stepMergedIntoSettle,
		"run:" + stepGuildMySQL, "persist:" + stepGuildMySQL,
		"run:" + stepTradeMySQL, "persist:" + stepTradeMySQL,
		"run:" + stepGuildRank, "persist:" + stepGuildRank,
		"persist:" + stepMergeDone,
	}
	if !reflect.DeepEqual(ops.events, want) {
		t.Fatalf("order\n got  %v\n want %v", ops.events, want)
	}
	if !m.stepDone(stepMergeDone) {
		t.Fatal("the completion step must be recorded")
	}
}

// TestMergeSteps_StepNamesOnDiskAreStable:步骤名写进清单,发布之后不能改字面量。
func TestMergeSteps_StepNamesOnDiskAreStable(t *testing.T) {
	got := []string{stepMergedInto, stepMergedIntoSettle, stepGuildMySQL, stepTradeMySQL, stepGuildRank, stepMergeDone}
	want := []string{"merged_into", "merged_into_settle", "guild_mysql", "trade_mysql", "guild_rank", "merge_done"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("step names = %v, want %v", got, want)
	}
}

// ── 续跑幂等 ──────────────────────────────────────────────────────────────────

// TestMergeSteps_ResumeSkipsFinishedSteps:中途失败不标记完成、不写完成步骤;重跑从失败的那一步接着做,做完的不再做;
// 全部完成后再跑一遍什么都不做,也不再落盘。
func TestMergeSteps_ResumeSkipsFinishedSteps(t *testing.T) {
	h := newManifestHarness(t)
	boom := errors.New("trade rewrite failed")
	ops := &recordingSteps{fail: map[string]error{stepTradeMySQL: boom}}
	m, err := h.run(ops, options{}, nil)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), stepTradeMySQL) || !strings.Contains(err.Error(), "NOT marked done") {
		t.Fatalf("error = %v", err)
	}
	if m.stepDone(stepTradeMySQL) || m.stepDone(stepMergeDone) || !m.stepDone(stepGuildMySQL) {
		t.Fatalf("progress after the failure: %+v", m.Steps)
	}

	ops = &recordingSteps{}
	m, err = h.run(ops, options{}, nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if want := []string{stepTradeMySQL, stepGuildRank}; !reflect.DeepEqual(ops.ran(), want) {
		t.Fatalf("resume ran %v, want %v", ops.ran(), want)
	}
	if !m.stepDone(stepMergeDone) {
		t.Fatal("the resumed run must record the completion step")
	}

	ops = &recordingSteps{}
	if _, err := h.run(ops, options{}, nil); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if len(ops.events) != 0 {
		t.Fatalf("a finished merge must do nothing on a re-run, got %v", ops.events)
	}
}

// TestMergeSteps_CrashBetweenMappingAndSettleWaitsAgain:映射已写、缓存等待没记完成的续跑要重新等:上一次等了多久不知道。
func TestMergeSteps_CrashBetweenMappingAndSettleWaitsAgain(t *testing.T) {
	h := newManifestHarness(t)
	ops := &recordingSteps{fail: map[string]error{stepMergedIntoSettle: context.Canceled}}
	if _, err := h.run(ops, options{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	ops = &recordingSteps{}
	if _, err := h.run(ops, options{}, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if want := []string{stepMergedIntoSettle, stepGuildMySQL, stepTradeMySQL, stepGuildRank}; !reflect.DeepEqual(ops.ran(), want) {
		t.Fatalf("resume ran %v, want %v", ops.ran(), want)
	}
}

// TestMergeSteps_SkipFlagIsNotMarkedDone:-skip-* 跳过的步骤不标记完成,之后不带开关重跑还能补上。
func TestMergeSteps_SkipFlagIsNotMarkedDone(t *testing.T) {
	h := newManifestHarness(t)
	ops := &recordingSteps{}
	m, err := h.run(ops, options{skipGuild: true, skipTrade: true, skipRank: true}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := []string{stepMergedInto, stepMergedIntoSettle}; !reflect.DeepEqual(ops.ran(), want) {
		t.Fatalf("ran %v, want %v", ops.ran(), want)
	}
	if m.stepDone(stepGuildMySQL) || m.stepDone(stepTradeMySQL) || m.stepDone(stepGuildRank) {
		t.Fatalf("skipped steps must not be marked done: %+v", m.Steps)
	}
	ops = &recordingSteps{}
	if _, err := h.run(ops, options{}, nil); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if want := []string{stepGuildMySQL, stepTradeMySQL, stepGuildRank}; !reflect.DeepEqual(ops.ran(), want) {
		t.Fatalf("re-run ran %v, want %v", ops.ran(), want)
	}
}

// TestMergeSteps_GateFailureStopsBeforeTheStep:判据失败的那一步一个字节都不写,也不标记完成。
func TestMergeSteps_GateFailureStopsBeforeTheStep(t *testing.T) {
	h := newManifestHarness(t)
	frozenGone := errors.New("freeze receipt no longer current")
	calls := 0
	gate := writeGate(func() error {
		calls++
		if calls == 3 {
			return frozenGone
		}
		return nil
	})
	ops := &recordingSteps{}
	m, err := h.run(ops, options{}, gate)
	if !errors.Is(err, frozenGone) {
		t.Fatalf("error = %v", err)
	}
	if want := []string{stepMergedInto, stepMergedIntoSettle}; !reflect.DeepEqual(ops.ran(), want) {
		t.Fatalf("ran %v, want %v", ops.ran(), want)
	}
	if m.stepDone(stepGuildMySQL) || m.stepDone(stepMergeDone) {
		t.Fatalf("progress: %+v", m.Steps)
	}
}

func TestMergeSteps_DryRunPersistsNothing(t *testing.T) {
	h := newManifestHarness(t)
	ops := &recordingSteps{}
	m, err := h.run(ops, options{dryRun: true}, nil)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if want := []string{stepMergedInto, stepMergedIntoSettle, stepGuildMySQL, stepTradeMySQL, stepGuildRank}; !reflect.DeepEqual(ops.ran(), want) {
		t.Fatalf("dry-run ran %v, want %v", ops.ran(), want)
	}
	if len(m.Steps) != 0 || len(ops.events) != 5 {
		t.Fatalf("a dry-run must not record progress: steps=%v events=%v", m.Steps, ops.events)
	}
}

// ── 连续合服的拒绝规则 ────────────────────────────────────────────────────────

func TestStepCheckMergedIntoBeforeMerge(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		merged map[uint32]mergedIntoValue
		want   string // 空 = 放行
	}{
		{"两边都没有映射", nil, ""},
		{"续跑_源区已指向同一个目标区", map[uint32]mergedIntoValue{901: {Dst: 902, RunID: testRunID}}, ""},
		{"连续合服_有别的区合进了源区", map[uint32]mergedIntoValue{900: {Dst: 901, RunID: "r"}}, ""},
		{"连续合服_有别的区合进了目标区", map[uint32]mergedIntoValue{900: {Dst: 902, RunID: "r"}}, ""},
		{"源区已合进别的区", map[uint32]mergedIntoValue{901: {Dst: 903, RunID: "r"}}, "already merged into zone 903"},
		{"目标区已被合走", map[uint32]mergedIntoValue{902: {Dst: 903, RunID: "r"}}, "itself merged into zone 903"},
		{"反向合服_会成环", map[uint32]mergedIntoValue{902: {Dst: 901, RunID: "r"}}, "itself merged into zone 901"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rb, db, _ := newMemBackend()
			for k, v := range tc.merged {
				db.merged[k] = v
			}
			err := stepCheckMergedIntoBeforeMerge(ctx, rb, testSrcZone, testDstZone)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("must pass: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestStepCheckMergedIntoBeforeMerge_ReadFailureFailsClosed:证明不了就不动手。
func TestStepCheckMergedIntoBeforeMerge_ReadFailureFailsClosed(t *testing.T) {
	rb, db, _ := newMemBackend()
	db.readErr = errors.New("connection lost")
	if err := stepCheckMergedIntoBeforeMerge(context.Background(), rb, testSrcZone, testDstZone); !errors.Is(err, db.readErr) {
		t.Fatalf("error = %v", err)
	}
}

// TestCheckResumeMergedInto:清单说映射已写,库里却没有(已撤销 / 被人工删了),续跑只会把每一步都跳过、打出 Done,必须拒绝。
func TestCheckResumeMergedInto(t *testing.T) {
	ctx := context.Background()
	rb, db, _ := newMemBackend()
	m := newMergeManifest(testRunID, testSrcZone, testDstZone, "op", time.Now())
	if err := checkResumeMergedInto(ctx, rb, nil, testSrcZone, testDstZone); err != nil {
		t.Fatalf("no manifest: %v", err)
	}
	if err := checkResumeMergedInto(ctx, rb, m, testSrcZone, testDstZone); err != nil {
		t.Fatalf("mapping step not done yet: %v", err)
	}
	m.markStep(stepMergedInto, "x")
	if err := checkResumeMergedInto(ctx, rb, m, testSrcZone, testDstZone); err == nil || !strings.Contains(err.Error(), "new -manifest-path") {
		t.Fatalf("a manifest whose mapping is gone must be refused: %v", err)
	}
	db.merged[testSrcZone] = mergedIntoValue{Dst: 903, RunID: "r"}
	if err := checkResumeMergedInto(ctx, rb, m, testSrcZone, testDstZone); err == nil {
		t.Fatal("a mapping to another zone must be refused")
	}
	db.merged[testSrcZone] = mergedIntoValue{Dst: testDstZone, RunID: testRunID}
	if err := checkResumeMergedInto(ctx, rb, m, testSrcZone, testDstZone); err != nil {
		t.Fatalf("the mapping is there: %v", err)
	}
}

// ── 写映射 ────────────────────────────────────────────────────────────────────

func TestStepMarkMergedInto_DBThenMirror(t *testing.T) {
	ctx := context.Background()
	rb, db, cache := newMemBackend()
	if err := stepMarkMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, false); err != nil {
		t.Fatalf("mark: %v", err)
	}
	want := `{"dst":902,"run_id":"` + testRunID + `"}`
	if db.merged[testSrcZone].Dst != testDstZone || cache.mirror[testSrcZone] != want {
		t.Fatalf("db=%+v mirror=%q", db.merged, cache.mirror[testSrcZone])
	}
	// 重跑幂等。
	if err := stepMarkMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, false); err != nil {
		t.Fatalf("re-run: %v", err)
	}
}

// TestStepMarkMergedInto_DBFailureWritesNoMirror:库没提交,Redis 一个字节不写。
func TestStepMarkMergedInto_DBFailureWritesNoMirror(t *testing.T) {
	rb, db, cache := newMemBackend()
	db.markErr = errRouteMergedIntoConflict
	if err := stepMarkMergedInto(context.Background(), rb, testSrcZone, testDstZone, testRunID, false); !errors.Is(err, errRouteMergedIntoConflict) {
		t.Fatalf("error = %v", err)
	}
	if len(cache.mirror) != 0 {
		t.Fatalf("no mirror may be written: %v", cache.mirror)
	}
}

// TestStepMarkMergedInto_MirrorFailureResumes:库已提交、镜像没确认:报错(步骤不算完成),重跑走幂等分支补上镜像。
func TestStepMarkMergedInto_MirrorFailureResumes(t *testing.T) {
	ctx := context.Background()
	rb, db, cache := newMemBackend()
	cache.setErr = errors.New("WAITAOF short")
	if err := stepMarkMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, false); !errors.Is(err, cache.setErr) {
		t.Fatalf("error = %v", err)
	}
	if _, ok := db.merged[testSrcZone]; !ok {
		t.Fatal("the DB row is committed first")
	}
	cache.setErr = nil
	if err := stepMarkMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, false); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if cache.mirror[testSrcZone] == "" {
		t.Fatal("the re-run must write the mirror")
	}
}

func TestStepMarkMergedInto_DryRunOnlyReads(t *testing.T) {
	ctx := context.Background()
	rb, db, cache := newMemBackend()
	if err := stepMarkMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, true); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if db.marks != 0 || len(db.merged) != 0 || len(cache.mirror) != 0 {
		t.Fatalf("a dry-run must not write: marks=%d db=%v mirror=%v", db.marks, db.merged, cache.mirror)
	}
	db.merged[testSrcZone] = mergedIntoValue{Dst: 903, RunID: "r"}
	if err := stepMarkMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, true); err == nil {
		t.Fatal("a dry-run must report the conflict too")
	}
}

// ── 撤销 ──────────────────────────────────────────────────────────────────────

func TestCheckUnmergeAllowed(t *testing.T) {
	ctx := context.Background()
	m := newMergeManifest(testRunID, testSrcZone, testDstZone, "op", time.Now())
	for _, tc := range []struct {
		name   string
		merged map[uint32]mergedIntoValue
		want   string
	}{
		{"映射是这份清单写的", map[uint32]mergedIntoValue{901: {Dst: 902, RunID: testRunID}}, ""},
		{"映射已不在_撤销重跑", nil, ""},
		{"目标区之后又被合走_必须逆序撤销", map[uint32]mergedIntoValue{901: {Dst: 902, RunID: testRunID}, 902: {Dst: 903, RunID: "r"}}, "reverse order"},
		{"映射是另一次合服写的", map[uint32]mergedIntoValue{901: {Dst: 902, RunID: "another-run"}}, "another merge"},
		{"映射指向别的区", map[uint32]mergedIntoValue{901: {Dst: 903, RunID: testRunID}}, "another merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rb, db, _ := newMemBackend()
			for k, v := range tc.merged {
				db.merged[k] = v
			}
			err := checkUnmergeAllowed(ctx, rb, m)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("must pass: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	rb, db, _ := newMemBackend()
	db.readErr = errors.New("connection lost")
	if err := checkUnmergeAllowed(ctx, rb, m); !errors.Is(err, db.readErr) {
		t.Fatalf("a read failure must refuse: %v", err)
	}
}

// fakeUnmergeSteps 是 unmergeStepOps 的替身:业务步骤只记顺序,删映射走真的 stepClearMergedInto + 内存路由库。
type fakeUnmergeSteps struct {
	rb     *routeBackend
	dryRun bool
	events []string
	fail   string
}

func (s *fakeUnmergeSteps) step(name string) error {
	s.events = append(s.events, name)
	if s.fail == name {
		return fmt.Errorf("%s failed", name)
	}
	return nil
}

func (s *fakeUnmergeSteps) restoreRank(context.Context) error   { return s.step("rank") }
func (s *fakeUnmergeSteps) restoreTrade(context.Context) error  { return s.step("trade") }
func (s *fakeUnmergeSteps) restoreGuilds(context.Context) error { return s.step("guild") }
func (s *fakeUnmergeSteps) clearMergedInto(ctx context.Context) error {
	if err := s.step("clear"); err != nil {
		return err
	}
	_, err := stepClearMergedInto(ctx, s.rb, testSrcZone, testDstZone, testRunID, s.dryRun)
	return err
}

// TestUnmergeSteps_MappingIsGoneAfterwards:业务数据写回之后才删映射;撤销完成后库与 Redis 都没有映射,别的映射不受影响。
func TestUnmergeSteps_MappingIsGoneAfterwards(t *testing.T) {
	ctx := context.Background()
	rb, db, cache := newMemBackend()
	if err := stepMarkMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, false); err != nil {
		t.Fatal(err)
	}
	db.merged[800] = mergedIntoValue{Dst: 801, RunID: "other"}
	cache.mirror[800] = `{"dst":801,"run_id":"other"}`

	ops := &fakeUnmergeSteps{rb: rb}
	if err := runUnmergeSteps(ctx, ops, nil); err != nil {
		t.Fatalf("unmerge: %v", err)
	}
	if want := []string{"rank", "trade", "guild", "clear"}; !reflect.DeepEqual(ops.events, want) {
		t.Fatalf("order = %v, want %v", ops.events, want)
	}
	if _, _, found, err := db.ReadMergedInto(ctx, testSrcZone); err != nil || found {
		t.Fatalf("zone_merged_into(%d) must be gone: found=%v err=%v", testSrcZone, found, err)
	}
	if v, ok := cache.mirror[testSrcZone]; ok {
		t.Fatalf("the mirror must be gone: %q", v)
	}
	if _, ok := db.merged[800]; !ok || cache.mirror[800] == "" {
		t.Fatal("another merge's mapping must be left alone")
	}
	// 重跑幂等。
	if err := runUnmergeSteps(ctx, &fakeUnmergeSteps{rb: rb}, nil); err != nil {
		t.Fatalf("re-run: %v", err)
	}
}

// TestUnmergeSteps_BusinessFailureKeepsTheMapping:业务写回失败时映射还在,玩家仍解析到目标区(数据还在那边),重跑补齐。
func TestUnmergeSteps_BusinessFailureKeepsTheMapping(t *testing.T) {
	ctx := context.Background()
	rb, db, _ := newMemBackend()
	if err := stepMarkMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, false); err != nil {
		t.Fatal(err)
	}
	ops := &fakeUnmergeSteps{rb: rb, fail: "guild"}
	if err := runUnmergeSteps(ctx, ops, nil); err == nil || !strings.Contains(err.Error(), "guild") {
		t.Fatalf("error = %v", err)
	}
	if _, ok := db.merged[testSrcZone]; !ok || db.clears != 0 {
		t.Fatalf("the mapping must stay until the business data is back: %+v clears=%d", db.merged, db.clears)
	}
	if err := runUnmergeSteps(ctx, &fakeUnmergeSteps{rb: rb}, nil); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if _, ok := db.merged[testSrcZone]; ok {
		t.Fatal("the re-run must delete the mapping")
	}
}

func TestUnmergeSteps_GateFailureStopsBeforeTheStep(t *testing.T) {
	ctx := context.Background()
	rb, db, _ := newMemBackend()
	db.merged[testSrcZone] = mergedIntoValue{Dst: testDstZone, RunID: testRunID}
	frozenGone := errors.New("freeze receipt no longer current")
	calls := 0
	gate := writeGate(func() error {
		calls++
		if calls == 2 {
			return frozenGone
		}
		return nil
	})
	ops := &fakeUnmergeSteps{rb: rb}
	if err := runUnmergeSteps(ctx, ops, gate); !errors.Is(err, frozenGone) {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(ops.events, []string{"rank"}) || db.clears != 0 {
		t.Fatalf("events=%v clears=%d", ops.events, db.clears)
	}
}

// TestStepClearMergedInto_MirrorFailureResumes:库已删、镜像没删掉:报错;重跑库里没东西删了,镜像照样按完整值补删。
func TestStepClearMergedInto_MirrorFailureResumes(t *testing.T) {
	ctx := context.Background()
	rb, db, cache := newMemBackend()
	if err := stepMarkMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, false); err != nil {
		t.Fatal(err)
	}
	cache.delErr = errors.New("connection lost")
	if deleted, err := stepClearMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, false); !errors.Is(err, cache.delErr) || !deleted {
		t.Fatalf("clear = (%v, %v)", deleted, err)
	}
	if _, ok := db.merged[testSrcZone]; ok {
		t.Fatal("the DB row goes first")
	}
	cache.delErr = nil
	if deleted, err := stepClearMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, false); err != nil || deleted {
		t.Fatalf("re-run = (%v, %v), want (false, nil)", deleted, err)
	}
	if len(cache.mirror) != 0 {
		t.Fatalf("the re-run must remove the mirror: %v", cache.mirror)
	}
}

// TestStepClearMergedInto_ForeignMirrorIsNotDeleted:镜像值不是这份清单的,compare-and-delete 不动它。
func TestStepClearMergedInto_ForeignMirrorIsNotDeleted(t *testing.T) {
	ctx := context.Background()
	rb, _, cache := newMemBackend()
	cache.mirror[testSrcZone] = `{"dst":903,"run_id":"other"}`
	if _, err := stepClearMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, false); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cache.mirror[testSrcZone] == "" {
		t.Fatal("a mirror of another merge must be left alone")
	}
}

func TestStepClearMergedInto_DryRunOnlyReads(t *testing.T) {
	ctx := context.Background()
	rb, db, cache := newMemBackend()
	db.merged[testSrcZone] = mergedIntoValue{Dst: testDstZone, RunID: testRunID}
	cache.mirror[testSrcZone] = "x"
	if would, err := stepClearMergedInto(ctx, rb, testSrcZone, testDstZone, testRunID, true); err != nil || !would {
		t.Fatalf("dry-run = (%v, %v)", would, err)
	}
	if db.clears != 0 || cache.mirror[testSrcZone] == "" {
		t.Fatal("a dry-run must not write")
	}
}

// ── 路由只读查询 ──────────────────────────────────────────────────────────────

// TestCollectPlayerIDsWithHomeZoneDB_PagesAcrossShardsAndSorts:翻页游标是聚簇键,返回值是 player_id 升序。
func TestCollectPlayerIDsWithHomeZoneDB_PagesAcrossShardsAndSorts(t *testing.T) {
	db := newMemRouteDB()
	for i := uint64(1); i <= routeReadBatchLimit+5; i++ {
		db.homes[testSrcZone] = append(db.homes[testSrcZone], i*7)
	}
	db.homes[testDstZone] = []uint64{1, 2, 3}
	got, err := collectPlayerIDsWithHomeZoneDB(context.Background(), db, testSrcZone)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != routeReadBatchLimit+5 {
		t.Fatalf("collected %d players", len(got))
	}
	for i, id := range got {
		if id != uint64(i+1)*7 {
			t.Fatalf("got[%d] = %d: the result must be sorted by player_id", i, id)
		}
	}
	db.readErr = errors.New("connection lost")
	if _, err := collectPlayerIDsWithHomeZoneDB(context.Background(), db, testSrcZone); !errors.Is(err, db.readErr) {
		t.Fatalf("a read failure must surface: %v", err)
	}
}

// ── 合服后验证 ────────────────────────────────────────────────────────────────

// mergedIntoVerifyFixture 是 verifyMergedInto 通过时的全部前提;用例逐项破坏。
func mergedIntoVerifyFixture(t *testing.T, mirror func() (any, error)) (auditConfig, *memRouteDB, *mergeManifest) {
	t.Helper()
	rdb, _ := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		if cmd.Name() != "get" {
			return nil, fmt.Errorf("unexpected command %s", cmd.Name())
		}
		return mirror()
	})
	db := newMemRouteDB()
	db.merged[testSrcZone] = mergedIntoValue{Dst: testDstZone, RunID: testRunID}
	db.homes[testSrcZone] = []uint64{1, 2}
	db.homes[testDstZone] = []uint64{3}
	m := newMergeManifest(testRunID, testSrcZone, testDstZone, "op", time.Now())
	m.markStep(stepMergeDone, "x")
	return auditConfig{src: testSrcZone, dst: testDstZone, verify: true, mappingDB: rdb, routes: db, manifest: m}, db, m
}

func TestVerifyMergedInto(t *testing.T) {
	ctx := context.Background()
	good := func() (any, error) { return `{"dst":902,"run_id":"` + testRunID + `"}`, nil }

	cfg, _, _ := mergedIntoVerifyFixture(t, good)
	r := verifyMergedInto(ctx, cfg)
	if r.Severity != "info" || r.ConflictCount != 0 || r.UniqueScope == "" {
		t.Fatalf("a complete merge must pass: %+v", r)
	}
	// 玩家不搬:home_zone=src 的人数不是 0,只是统计。
	if r.SourceCount != 2 || r.TargetCount != 1 {
		t.Fatalf("counts = %d/%d, want the home_zone statistics 2/1", r.SourceCount, r.TargetCount)
	}

	t.Run("清单没走完", func(t *testing.T) {
		cfg, _, m := mergedIntoVerifyFixture(t, good)
		delete(m.Steps, stepMergeDone)
		if r := verifyMergedInto(ctx, cfg); r.Severity != "block" || isInfraAudit(r) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("映射不在", func(t *testing.T) {
		cfg, db, _ := mergedIntoVerifyFixture(t, good)
		delete(db.merged, testSrcZone)
		if r := verifyMergedInto(ctx, cfg); r.Severity != "block" || isInfraAudit(r) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("映射指向别的区", func(t *testing.T) {
		cfg, db, _ := mergedIntoVerifyFixture(t, good)
		db.merged[testSrcZone] = mergedIntoValue{Dst: 903, RunID: testRunID}
		if r := verifyMergedInto(ctx, cfg); r.Severity != "block" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("映射是另一次合服写的", func(t *testing.T) {
		cfg, db, _ := mergedIntoVerifyFixture(t, good)
		db.merged[testSrcZone] = mergedIntoValue{Dst: testDstZone, RunID: "another-run"}
		if r := verifyMergedInto(ctx, cfg); r.Severity != "block" || !strings.Contains(r.Notes, "another-run") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("镜像缺失", func(t *testing.T) {
		cfg, _, _ := mergedIntoVerifyFixture(t, func() (any, error) { return nil, redis.Nil })
		if r := verifyMergedInto(ctx, cfg); r.Severity != "block" || isInfraAudit(r) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("镜像与库不一致", func(t *testing.T) {
		cfg, _, _ := mergedIntoVerifyFixture(t, func() (any, error) { return `{"dst":903,"run_id":"r"}`, nil })
		if r := verifyMergedInto(ctx, cfg); r.Severity != "block" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("库读失败是INFRA", func(t *testing.T) {
		cfg, db, _ := mergedIntoVerifyFixture(t, good)
		db.readErr = errors.New("connection lost")
		if r := verifyMergedInto(ctx, cfg); !isInfraAudit(r) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("路由库没装成是INFRA", func(t *testing.T) {
		cfg, _, _ := mergedIntoVerifyFixture(t, good)
		cfg.routes, cfg.routeErr = nil, errors.New("not paired")
		if r := verifyMergedInto(ctx, cfg); !isInfraAudit(r) || !strings.Contains(r.Notes, "not paired") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("没有mapping_Redis是INFRA", func(t *testing.T) {
		cfg, _, _ := mergedIntoVerifyFixture(t, good)
		cfg.mappingDB = nil
		if r := verifyMergedInto(ctx, cfg); !isInfraAudit(r) {
			t.Fatalf("%+v", r)
		}
	})
}

// TestRunAuditMode_VerifyRowsMatchTheRecoveryCatalog:-verify-merged 的行名与 P7 审计目录一一对应(go/db barrierrestore 校验同一组)。
func TestRunAuditMode_VerifyRowsMatchTheRecoveryCatalog(t *testing.T) {
	rows := runAuditMode(context.Background(), auditConfig{verify: true, tradeSchema: defaultTradeSchema, guildSchema: defaultGuildSchema, src: 901, dst: 902})
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if !reflect.DeepEqual(names, mergeRecoveryRowNames()) {
		t.Fatalf("verify rows = %v, want %v", names, mergeRecoveryRowNames())
	}
	want := []string{"verify:merged_into", "verify:guild_zone", "verify:trade_market_zone", "verify:guild_rank"}
	if !reflect.DeepEqual(mergeRecoveryRowNames(), want) {
		t.Fatalf("recovery catalog = %v, want %v (go/db barrierrestore checks the same names)", mergeRecoveryRowNames(), want)
	}
	for _, r := range runAuditMode(context.Background(), auditConfig{guildSchema: defaultGuildSchema, src: 901, dst: 902}) {
		if strings.HasPrefix(r.Name, "verify:") {
			t.Fatalf("the pre-merge audit must not assert %s", r.Name)
		}
	}
}

// ── 前置门禁 ──────────────────────────────────────────────────────────────────

type fakeTeamAuthority struct {
	inTeam int
	err    error
	asked  []uint64
}

func (f *fakeTeamAuthority) PlayersInTeams(_ context.Context, ids []uint64) (int, error) {
	f.asked = ids
	return f.inTeam, f.err
}

type fakeSceneRegistry struct {
	registered int
	err        error
}

func (f fakeSceneRegistry) SceneNodesInZone(context.Context, uint32) (int, error) {
	return f.registered, f.err
}

// preflightFixture 是全部门禁通过的装配;replies 决定 ZCARD(负载集)与 EXISTS(会话)的应答。
func preflightFixture(t *testing.T, load, session int64) (preflightDeps, preflightParams, *fakeTeamAuthority) {
	t.Helper()
	scene, _ := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) { return load, nil })
	shared, _ := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) { return session, nil })
	team := &fakeTeamAuthority{}
	deps := preflightDeps{sceneRdb: scene, sceneNodes: fakeSceneRegistry{}, sharedRdb: shared, teamAuthority: team}
	return deps, preflightParams{readOnly: true, zone: testSrcZone, scope: "source", playerIDs: []uint64{1, 2}}, team
}

func TestRunPreflight(t *testing.T) {
	ctx := context.Background()
	deps, params, team := preflightFixture(t, 0, 0)
	if err := runPreflight(ctx, deps, params); err != nil {
		t.Fatalf("a clean zone must pass: %v", err)
	}
	if !reflect.DeepEqual(team.asked, params.playerIDs) {
		t.Fatalf("P7 asked about %v", team.asked)
	}

	t.Run("P2_etcd还有注册", func(t *testing.T) {
		deps, params, _ := preflightFixture(t, 0, 0)
		deps.sceneNodes = fakeSceneRegistry{registered: 1}
		if err := runPreflight(ctx, deps, params); !errors.Is(err, errZoneStillLive) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("P2_负载集非空", func(t *testing.T) {
		deps, params, _ := preflightFixture(t, 3, 0)
		if err := runPreflight(ctx, deps, params); !errors.Is(err, errZoneStillLive) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("P6_还有会话", func(t *testing.T) {
		deps, params, _ := preflightFixture(t, 0, 1)
		if err := runPreflight(ctx, deps, params); err == nil || !strings.Contains(err.Error(), "P6") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("P7_还在队伍里", func(t *testing.T) {
		deps, params, team := preflightFixture(t, 0, 0)
		team.inTeam = 1
		if err := runPreflight(ctx, deps, params); err == nil || !strings.Contains(err.Error(), "P7") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("P7_没有读者就拒绝", func(t *testing.T) {
		deps, params, _ := preflightFixture(t, 0, 0)
		deps.teamAuthority = nil
		if err := runPreflight(ctx, deps, params); err == nil || !strings.Contains(err.Error(), "P7") {
			t.Fatalf("error = %v", err)
		}
	})
}

// ── 业务步骤的细节 ────────────────────────────────────────────────────────────

// TestSettleMergedInto:dry-run 不等;取消立刻返回错误(步骤不算完成)。
func TestSettleMergedInto(t *testing.T) {
	s := &liveMergeSteps{o: options{dryRun: true, mergedIntoSettle: time.Hour}}
	if _, err := s.settleMergedInto(context.Background()); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	s = &liveMergeSteps{o: options{mergedIntoSettle: time.Hour}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.settleMergedInto(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait must fail the step: %v", err)
	}
	s = &liveMergeSteps{o: options{}}
	if _, err := s.settleMergedInto(context.Background()); err != nil {
		t.Fatalf("zero settle: %v", err)
	}
}

func TestRequireSameSQLDomain(t *testing.T) {
	if err := requireSameSQLDomain("u@tcp(tidb:4000)/mmorpg", "u@tcp(tidb:4000)/mmorpg_global"); err != nil {
		t.Fatalf("same entry point: %v", err)
	}
	if err := requireSameSQLDomain("u@tcp(tidb:4000)/mmorpg", "u@tcp(other:4000)/mmorpg_global"); err == nil {
		t.Fatal("two entry points must be refused")
	}
	if err := requireSameSQLDomain("not a dsn", "u@tcp(tidb:4000)/mmorpg_global"); err == nil {
		t.Fatal("a bad DSN must be refused")
	}
}

// TestZonesResolvingThrough:解析链经过 zone 的都算,成环拒绝。
func TestZonesResolvingThrough(t *testing.T) {
	got, err := zonesResolvingThrough(map[uint32]uint32{3: 4, 4: 5, 7: 8}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[3 4 5]" {
		t.Fatalf("zones = %v", got)
	}
	// 撤销 5→6 时 5 自己有映射:结果里仍是经过 5 的那些。
	got, err = zonesResolvingThrough(map[uint32]uint32{4: 5, 5: 6}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[4 5]" {
		t.Fatalf("zones = %v", got)
	}
	if _, err := zonesResolvingThrough(map[uint32]uint32{1: 2, 2: 1}, 9); err == nil {
		t.Fatal("a cycle must be refused")
	}
}

// TestCollectPlayerIDsResolvingThrough:已有 4→5 时合 5→6,原始归属 4 的玩家也要过门禁。
func TestCollectPlayerIDsResolvingThrough(t *testing.T) {
	_, db, _ := newMemBackend()
	db.homes[5] = []uint64{50}
	db.homes[4] = []uint64{40}
	db.homes[6] = []uint64{60}
	db.merged[4] = mergedIntoValue{Dst: 5, RunID: "r"}
	got, err := collectPlayerIDsResolvingThrough(context.Background(), db, 5)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[40 50]" {
		t.Fatalf("players = %v", got)
	}
	db.readErr = errors.New("connection lost")
	if _, err := collectPlayerIDsResolvingThrough(context.Background(), db, 5); !errors.Is(err, db.readErr) {
		t.Fatalf("error = %v", err)
	}
}

// TestCheckMergedIntoOwnedByManifest:映射只能由写它的那份清单续跑。
func TestCheckMergedIntoOwnedByManifest(t *testing.T) {
	ctx := context.Background()
	rb, db, _ := newMemBackend()
	owner := &mergeManifest{RunID: testRunID}
	other := &mergeManifest{RunID: "901-902-20261030T000000Z-bbbbbbbb"}
	if err := checkMergedIntoOwnedByManifest(ctx, rb, nil, testSrcZone, testDstZone); err != nil {
		t.Fatalf("no mapping yet: %v", err)
	}
	db.merged[testSrcZone] = mergedIntoValue{Dst: testDstZone, RunID: testRunID}
	if err := checkMergedIntoOwnedByManifest(ctx, rb, owner, testSrcZone, testDstZone); err != nil {
		t.Fatalf("the same manifest must resume: %v", err)
	}
	for name, m := range map[string]*mergeManifest{"no manifest": nil, "another manifest": other} {
		if err := checkMergedIntoOwnedByManifest(ctx, rb, m, testSrcZone, testDstZone); err == nil || !strings.Contains(err.Error(), testRunID) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
	db.readErr = errors.New("connection lost")
	if err := checkMergedIntoOwnedByManifest(ctx, rb, owner, testSrcZone, testDstZone); !errors.Is(err, db.readErr) {
		t.Fatalf("error = %v", err)
	}
}

// TestRefuseManifestBeingUnmerged:撤销半途失败之后,同一份清单不能续跑合服。
func TestRefuseManifestBeingUnmerged(t *testing.T) {
	if err := refuseManifestBeingUnmerged(nil); err != nil {
		t.Fatal(err)
	}
	if err := refuseManifestBeingUnmerged(&mergeManifest{RunID: testRunID}); err != nil {
		t.Fatal(err)
	}
	if err := refuseManifestBeingUnmerged(&mergeManifest{RunID: testRunID, UnmergeStarted: true}); err == nil {
		t.Fatal("a manifest that went through unmerge must be refused")
	}
}
