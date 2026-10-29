package main

// -route-source=db 下各写入点流程的单测(D1 第 2 批;规格 §6.3、§6.4、§13.3 第 1–5 条)。
//
// 库原语与发布层都用接口替身(fakeRouteDB / fakeRouteCache):本组用例只证「本层的推进规则」——
// 提交之后发布失败不推进依赖步骤、重跑走幂等分支补发布、围栏失权即停、redis 源一个字节不变。
// 原语自身的 SQL 语句序列由 route_store_test.go 的假驱动钉住,真库并发语义由 merge_integration 用例证。
// 本文件放公共替身与步骤 5(W6 → W5)、R8 读方、guild / trade 单行事务的闸门用例;其余写入点见同名的 *_db_test.go。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// errFakeUnexpected 是替身里没有配置的方法被调用时的错误:脚本没写到的调用不能被当成正常路径悄悄通过。
var errFakeUnexpected = errors.New("fake: unexpected call")

// fakeRouteDB 是 routeDBOps 的替身:每个方法一个可替换的函数(nil = 不该被调用),调用按顺序记名。
type fakeRouteDB struct {
	mu    sync.Mutex
	calls []string

	acquire     func(req routeFenceAcquire) (routeFenceGrant, error)
	takeOver    func(req routeFenceAcquire, prev string) (routeFenceGrant, error)
	renew       func(zones []uint32, runID string, ttl time.Duration) (map[uint32]uint64, error)
	release     func(zones []uint32, runID string) (map[uint32]int64, error)
	releaseCtx  func(ctx context.Context) // 可空:看 ReleaseFence 收到的 ctx(释放 / 清理有没有时限,MZ2-6)
	readFence   func(zone uint32) (routeFenceState, error)
	readRoutes  func(ids []uint64) (map[uint64]mergeRouteRow, error)
	scanByHome  func(zone uint32, after routeScanCursor, limit int) ([]mergeRouteRow, error)
	readMerged  func(src uint32) (uint32, string, bool, error)
	markMerged  func(src, dst uint32, fenceRunID, markRunID string) (string, error)
	clearMerged func(src, dst uint32, fenceRunID, markRunID string) (bool, error)
	remap       func(src, dst uint32, runID string, ids []uint64) (routeBatchResult[mappingOutcome], error)
	restore     func(src, dst uint32, runID string, ids []uint64) (routeBatchResult[mappingOutcome], error)
	pin         func(src, dst uint32, runID string, ids []uint64) (routeBatchResult[pinVerdict], error)
	pinZone     func(zone uint32, after routeScanCursor, limit int) (routeBatchResult[pinVerdict], routeScanCursor, bool, error)
	backfill    func(zone uint32, ids []uint64) (routeBatchResult[routeBackfillVerdict], error)
}

func (f *fakeRouteDB) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeRouteDB) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRouteDB) count(name string) int {
	n := 0
	for _, c := range f.called() {
		if c == name {
			n++
		}
	}
	return n
}

func (f *fakeRouteDB) AcquireFence(_ context.Context, req routeFenceAcquire) (routeFenceGrant, error) {
	f.record("AcquireFence")
	if f.acquire == nil {
		return routeFenceGrant{}, errFakeUnexpected
	}
	return f.acquire(req)
}

func (f *fakeRouteDB) TakeOverFence(_ context.Context, req routeFenceAcquire, prev string) (routeFenceGrant, error) {
	f.record("TakeOverFence")
	if f.takeOver == nil {
		return routeFenceGrant{}, errFakeUnexpected
	}
	return f.takeOver(req, prev)
}

func (f *fakeRouteDB) RenewFence(_ context.Context, zones []uint32, runID string, ttl time.Duration) (map[uint32]uint64, error) {
	f.record("RenewFence")
	if f.renew == nil {
		return nil, errFakeUnexpected
	}
	return f.renew(zones, runID, ttl)
}

func (f *fakeRouteDB) ReleaseFence(ctx context.Context, zones []uint32, runID string) (map[uint32]int64, error) {
	f.record("ReleaseFence")
	if f.releaseCtx != nil {
		f.releaseCtx(ctx)
	}
	if f.release == nil {
		return nil, errFakeUnexpected
	}
	return f.release(zones, runID)
}

func (f *fakeRouteDB) ReadFence(_ context.Context, zone uint32) (routeFenceState, error) {
	f.record("ReadFence")
	if f.readFence == nil {
		return routeFenceState{}, errFakeUnexpected
	}
	return f.readFence(zone)
}

func (f *fakeRouteDB) ReadRoutes(_ context.Context, ids []uint64) (map[uint64]mergeRouteRow, error) {
	f.record("ReadRoutes")
	if f.readRoutes == nil {
		return nil, errFakeUnexpected
	}
	return f.readRoutes(ids)
}

func (f *fakeRouteDB) ScanByHome(_ context.Context, zone uint32, after routeScanCursor, limit int) ([]mergeRouteRow, error) {
	f.record("ScanByHome")
	if f.scanByHome == nil {
		return nil, errFakeUnexpected
	}
	return f.scanByHome(zone, after, limit)
}

func (f *fakeRouteDB) ReadMergedInto(_ context.Context, src uint32) (uint32, string, bool, error) {
	f.record("ReadMergedInto")
	if f.readMerged == nil {
		return 0, "", false, errFakeUnexpected
	}
	return f.readMerged(src)
}

func (f *fakeRouteDB) MarkMergedInto(_ context.Context, src, dst uint32, fenceRunID, markRunID string) (string, error) {
	f.record("MarkMergedInto")
	if f.markMerged == nil {
		return "", errFakeUnexpected
	}
	return f.markMerged(src, dst, fenceRunID, markRunID)
}

func (f *fakeRouteDB) ClearMergedInto(_ context.Context, src, dst uint32, fenceRunID, markRunID string) (bool, error) {
	f.record("ClearMergedInto")
	if f.clearMerged == nil {
		return false, errFakeUnexpected
	}
	return f.clearMerged(src, dst, fenceRunID, markRunID)
}

func (f *fakeRouteDB) RemapHomeBatch(_ context.Context, src, dst uint32, runID string, ids []uint64) (routeBatchResult[mappingOutcome], error) {
	f.record("RemapHomeBatch")
	if f.remap == nil {
		return routeBatchResult[mappingOutcome]{}, errFakeUnexpected
	}
	return f.remap(src, dst, runID, ids)
}

func (f *fakeRouteDB) RestoreHomeBatch(_ context.Context, src, dst uint32, runID string, ids []uint64) (routeBatchResult[mappingOutcome], error) {
	f.record("RestoreHomeBatch")
	if f.restore == nil {
		return routeBatchResult[mappingOutcome]{}, errFakeUnexpected
	}
	return f.restore(src, dst, runID, ids)
}

func (f *fakeRouteDB) PinPlacementBatch(_ context.Context, src, dst uint32, runID string, ids []uint64) (routeBatchResult[pinVerdict], error) {
	f.record("PinPlacementBatch")
	if f.pin == nil {
		return routeBatchResult[pinVerdict]{}, errFakeUnexpected
	}
	return f.pin(src, dst, runID, ids)
}

func (f *fakeRouteDB) PinZonePlacementBatch(_ context.Context, zone uint32, after routeScanCursor, limit int) (routeBatchResult[pinVerdict], routeScanCursor, bool, error) {
	f.record("PinZonePlacementBatch")
	if f.pinZone == nil {
		return routeBatchResult[pinVerdict]{}, after, false, errFakeUnexpected
	}
	return f.pinZone(zone, after, limit)
}

func (f *fakeRouteDB) BackfillHomeBatch(_ context.Context, zone uint32, ids []uint64) (routeBatchResult[routeBackfillVerdict], error) {
	f.record("BackfillHomeBatch")
	if f.backfill == nil {
		return routeBatchResult[routeBackfillVerdict]{}, errFakeUnexpected
	}
	return f.backfill(zone, ids)
}

// fakeRouteCache 是 routeCacheOps 的替身:缺省一律成功;各 *Err 钩子按调用序号注入失败。调用按顺序记名,发布的行逐批留底。
type fakeRouteCache struct {
	mu        sync.Mutex
	calls     []string
	published [][]mergeRouteRow
	caughtUp  [][]mergeRouteRow

	publishErr func(call int, rows []mergeRouteRow) error
	setFence   func(zone uint32, holder string, epoch, expiresAtMs uint64) error
	delFence   func(zone uint32, holder string) (bool, error)
	setMerged  func(src uint32, value string) error
	delMerged  func(src uint32, value string) (bool, error)
}

func (c *fakeRouteCache) record(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name)
}

func (c *fakeRouteCache) called() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *fakeRouteCache) publishRoutes(_ context.Context, rows []mergeRouteRow) (routePublishReport, error) {
	c.record("publishRoutes")
	c.mu.Lock()
	n := len(c.published)
	c.published = append(c.published, append([]mergeRouteRow(nil), rows...))
	hook := c.publishErr
	c.mu.Unlock()
	if hook != nil {
		if err := hook(n, rows); err != nil {
			return routePublishReport{}, err
		}
	}
	return routePublishReport{Rows: len(rows), Applied: len(rows)}, nil
}

func (c *fakeRouteCache) catchUpRoutes(_ context.Context, rows []mergeRouteRow) (routeCatchUpReport, error) {
	c.record("catchUpRoutes")
	c.mu.Lock()
	c.caughtUp = append(c.caughtUp, append([]mergeRouteRow(nil), rows...))
	c.mu.Unlock()
	return routeCatchUpReport{Published: routePublishReport{Rows: len(rows), Applied: len(rows)}}, nil
}

func (c *fakeRouteCache) setFenceMirror(_ context.Context, zone uint32, holder string, epoch, expiresAtMs uint64) error {
	c.record(fmt.Sprintf("setFenceMirror(%d)", zone))
	if c.setFence == nil {
		return nil
	}
	return c.setFence(zone, holder, epoch, expiresAtMs)
}

func (c *fakeRouteCache) deleteFenceMirror(_ context.Context, zone uint32, holder string) (bool, error) {
	c.record(fmt.Sprintf("deleteFenceMirror(%d)", zone))
	if c.delFence == nil {
		return true, nil
	}
	return c.delFence(zone, holder)
}

func (c *fakeRouteCache) setMergedIntoMirror(_ context.Context, src uint32, value string) error {
	c.record("setMergedIntoMirror")
	if c.setMerged == nil {
		return nil
	}
	return c.setMerged(src, value)
}

func (c *fakeRouteCache) deleteMergedIntoMirror(_ context.Context, src uint32, value string) (bool, error) {
	c.record("deleteMergedIntoMirror")
	if c.delMerged == nil {
		return true, nil
	}
	return c.delMerged(src, value)
}

func (c *fakeRouteCache) publishedIDs() [][]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]uint64, len(c.published))
	for i, rows := range c.published {
		for _, r := range rows {
			out[i] = append(out[i], r.PlayerID)
		}
	}
	return out
}

// ── 夹具 ─────────────────────────────────────────────────────────────────────

const (
	testDBSrc     = uint32(901)
	testDBDst     = uint32(902)
	testDBRun     = "901-902-20261019T000000Z-aaaaaaaa"
	testDBMarkRun = "901-902-20261018T000000Z-mmmmmmmm"
)

func newTestRouteBackend() (*routeBackend, *fakeRouteDB, *fakeRouteCache) {
	db, cache := &fakeRouteDB{}, &fakeRouteCache{}
	return &routeBackend{db: db, pub: cache}, db, cache
}

// testIDs 返回 1..n。
func testIDs(n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = uint64(i + 1)
	}
	return out
}

// homeMoveFake 模拟 RemapHomeBatch / RestoreHomeBatch:home 在 home 表里,from 的改成 to(Changed),已是 to 的计 Already。
// Target 是提交后处于目标状态的全部行(与 route_store.go moveHomeBatch 同口径)。
func homeMoveFake(home map[uint64]uint32, from, to uint32) func(uint32, uint32, string, []uint64) (routeBatchResult[mappingOutcome], error) {
	return func(_, _ uint32, _ string, ids []uint64) (routeBatchResult[mappingOutcome], error) {
		res := newRouteBatchResult[mappingOutcome](len(ids))
		for _, id := range ids {
			h, ok := home[id]
			v := classifyRouteHome(mergeRouteRow{PlayerID: id, HomeZone: h}, ok, from, to)
			res.Verdicts[id] = v
			switch v {
			case mappingChanged:
				home[id] = to
				res.Changed++
				res.Target = append(res.Target, mergeRouteRow{PlayerID: id, HomeZone: to, Rev: 2})
			case mappingAlready:
				res.Target = append(res.Target, mergeRouteRow{PlayerID: id, HomeZone: to, Rev: 2})
			}
		}
		return res, nil
	}
}

func homesAt(zone uint32, ids []uint64) map[uint64]uint32 {
	m := make(map[uint64]uint32, len(ids))
	for _, id := range ids {
		m[id] = zone
	}
	return m
}

// countingGate 是一个第 failAt 次(从 1 起)起返回失权的闸门;failAt <= 0 = 永远放行。
func countingGate(failAt int) (writeGate, *int) {
	n := 0
	return func() error {
		n++
		if failAt > 0 && n >= failAt {
			return fmt.Errorf("%w: test gate tripped at check %d", errRouteFenceLost, n)
		}
		return nil
	}, &n
}

// ── 步骤 5:W6 → W5 ──────────────────────────────────────────────────────────

func dbMappingBackend(home map[uint64]uint32) (*routeBackend, *fakeRouteDB, *fakeRouteCache) {
	rb, db, cache := newTestRouteBackend()
	db.markMerged = func(src, dst uint32, fenceRunID, markRunID string) (string, error) {
		if src != testDBSrc || dst != testDBDst || fenceRunID != testDBRun || markRunID != testDBMarkRun {
			return "", fmt.Errorf("MarkMergedInto(%d, %d, %q, %q)", src, dst, fenceRunID, markRunID)
		}
		return encodeRouteMergedIntoValue(dst, markRunID)
	}
	db.remap = homeMoveFake(home, testDBSrc, testDBDst)
	return rb, db, cache
}

// TestPlayerMappingStepDB_MarkerMirrorThenBatchesPublished:W6 先于 W5,镜像确认之后才改映射;每批「事务 → 发布」之后才下一批;
// 完成判据 = complete() 原判据且全员发布确认。
func TestPlayerMappingStepDB_MarkerMirrorThenBatchesPublished(t *testing.T) {
	ids := testIDs(routeWriteBatchLimit + 7)
	rb, db, cache := dbMappingBackend(homesAt(testDBSrc, ids))
	rep, err := runPlayerMappingStep(context.Background(), rb, nil, ids, testDBSrc, testDBDst, testDBRun, testDBMarkRun, nil, false)
	if err != nil {
		t.Fatalf("runPlayerMappingStep: %v", err)
	}
	if rep.Changed != len(ids) || rep.Already != 0 || rep.Published != len(ids) {
		t.Fatalf("report = %+v, want changed=%d published=%d", rep, len(ids), len(ids))
	}
	if got, want := db.called(), []string{"MarkMergedInto", "RemapHomeBatch", "RemapHomeBatch"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("db calls = %v, want %v", got, want)
	}
	if got, want := cache.called(), []string{"setMergedIntoMirror", "publishRoutes", "publishRoutes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cache calls = %v, want %v", got, want)
	}
	pub := cache.publishedIDs()
	if len(pub[0]) != routeWriteBatchLimit || len(pub[1]) != 7 {
		t.Fatalf("published batch sizes = %d / %d, want %d / 7", len(pub[0]), len(pub[1]), routeWriteBatchLimit)
	}
}

// TestPlayerMappingStepDB_MirrorFailureDoesNotRemap:W6 的库已提交、镜像未确认 → 不推进 W5(一个人都不改)。
func TestPlayerMappingStepDB_MirrorFailureDoesNotRemap(t *testing.T) {
	ids := testIDs(3)
	rb, db, cache := dbMappingBackend(homesAt(testDBSrc, ids))
	cache.setMerged = func(uint32, string) error { return fmt.Errorf("%w: WAITAOF acked 0", errRoutePublishNotDurable) }
	_, err := runPlayerMappingStep(context.Background(), rb, nil, ids, testDBSrc, testDBDst, testDBRun, testDBMarkRun, nil, false)
	if !errors.Is(err, errRoutePublishNotDurable) || !strings.Contains(err.Error(), "映射一个没改") {
		t.Fatalf("error = %v, want the marker-mirror failure", err)
	}
	if db.count("RemapHomeBatch") != 0 {
		t.Fatalf("db calls = %v: W5 must not start before the merged_into mirror is confirmed", db.called())
	}
}

// TestPlayerMappingStepDB_PublishFailureStopsBeforeNextBatch:第一批已提交、发布失败 → 不进第二批、步骤未完成(返回错误)。
func TestPlayerMappingStepDB_PublishFailureStopsBeforeNextBatch(t *testing.T) {
	ids := testIDs(routeWriteBatchLimit + 1)
	rb, db, cache := dbMappingBackend(homesAt(testDBSrc, ids))
	cache.publishErr = func(call int, _ []mergeRouteRow) error {
		if call == 0 {
			return fmt.Errorf("%w: 1 of 500 rows", errRoutePublishRejected)
		}
		return nil
	}
	rep, err := runPlayerMappingStep(context.Background(), rb, nil, ids, testDBSrc, testDBDst, testDBRun, testDBMarkRun, nil, false)
	if !errors.Is(err, errRoutePublishRejected) || !strings.Contains(err.Error(), "NOT done") {
		t.Fatalf("error = %v, want the batch publish failure", err)
	}
	if db.count("RemapHomeBatch") != 1 {
		t.Fatalf("db calls = %v: the second batch must not run after the first batch's publish failed", db.called())
	}
	if rep.Published != 0 {
		t.Fatalf("published = %d, want 0 (nothing confirmed)", rep.Published)
	}
}

// TestPlayerMappingStepDB_RerunRepublishesAlreadyMoved:上一次提交了却没发布成 → 重跑全员走「已是 dst」分支、照样发布,
// 完成判据通过(Already + 全员确认)。
func TestPlayerMappingStepDB_RerunRepublishesAlreadyMoved(t *testing.T) {
	ids := testIDs(4)
	rb, db, cache := dbMappingBackend(homesAt(testDBDst, ids))
	db.markMerged = func(_, dst uint32, _, markRunID string) (string, error) {
		return encodeRouteMergedIntoValue(dst, markRunID)
	}
	rep, err := runPlayerMappingStep(context.Background(), rb, nil, ids, testDBSrc, testDBDst, testDBRun, testDBMarkRun, nil, false)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if rep.Changed != 0 || rep.Already != len(ids) || rep.Published != len(ids) {
		t.Fatalf("report = %+v, want already=%d published=%d", rep, len(ids), len(ids))
	}
	if got := cache.publishedIDs(); len(got) != 1 || !reflect.DeepEqual(got[0], ids) {
		t.Fatalf("republished = %v, want %v", got, ids)
	}
}

// TestPlayerMappingStepDB_FenceLostStopsTheNextBatch:第二批之前围栏失权 → 第一批照常提交与发布,第二批不跑,错误带 errRouteFenceLost。
func TestPlayerMappingStepDB_FenceLostStopsTheNextBatch(t *testing.T) {
	ids := testIDs(routeWriteBatchLimit + 2)
	rb, db, cache := dbMappingBackend(homesAt(testDBSrc, ids))
	gate, checks := countingGate(3) // 第 1 次:步骤开始;第 2 次:第一批之前;第 3 次:第二批之前
	rep, err := runPlayerMappingStep(context.Background(), rb, nil, ids, testDBSrc, testDBDst, testDBRun, testDBMarkRun, gate, false)
	if !errors.Is(err, errRouteFenceLost) {
		t.Fatalf("error = %v, want errRouteFenceLost", err)
	}
	if db.count("RemapHomeBatch") != 1 || len(cache.publishedIDs()) != 1 || rep.Published != routeWriteBatchLimit || *checks != 3 {
		t.Fatalf("db=%v published=%d report=%+v checks=%d", db.called(), len(cache.publishedIDs()), rep, *checks)
	}
}

// TestPlayerMappingStepDB_IncompleteIsRefused:清单里有人在库里没有行 / 指向第三个 zone → complete() 失败,步骤不标完成。
func TestPlayerMappingStepDB_IncompleteIsRefused(t *testing.T) {
	ids := testIDs(3)
	home := homesAt(testDBSrc, ids)
	delete(home, 2)
	home[3] = 777
	rb, _, _ := dbMappingBackend(home)
	_, err := runPlayerMappingStep(context.Background(), rb, nil, ids, testDBSrc, testDBDst, testDBRun, testDBMarkRun, nil, false)
	if err == nil || !strings.Contains(err.Error(), "only 1 of the 3 manifest players") {
		t.Fatalf("error = %v, want the complete() refusal", err)
	}
}

// TestPlayerMappingStepDB_DryRunOnlyReads:dry-run 只读库(merged_into 与玩家行),不写库、不写 Redis。
func TestPlayerMappingStepDB_DryRunOnlyReads(t *testing.T) {
	ids := testIDs(3)
	rb, db, cache := newTestRouteBackend()
	db.readMerged = func(uint32) (uint32, string, bool, error) { return 0, "", false, nil }
	db.readRoutes = func(ids []uint64) (map[uint64]mergeRouteRow, error) {
		out := map[uint64]mergeRouteRow{}
		for _, id := range ids {
			out[id] = mergeRouteRow{PlayerID: id, HomeZone: testDBSrc, Rev: 1}
		}
		return out, nil
	}
	rep, err := runPlayerMappingStep(context.Background(), rb, nil, ids, testDBSrc, testDBDst, testDBRun, testDBMarkRun, nil, true)
	if err != nil || rep.Changed != 3 {
		t.Fatalf("dry-run = (%+v, %v)", rep, err)
	}
	if got, want := db.called(), []string{"ReadMergedInto", "ReadRoutes"}; !reflect.DeepEqual(got, want) || len(cache.called()) != 0 {
		t.Fatalf("dry-run calls db=%v cache=%v, want db=%v and no cache call", got, cache.called(), want)
	}
}

// recordedRedis 记下一次运行发给 Redis 的全部命令参数(单条与 pipeline 分开、各自按发送顺序)。
func recordedRedis(f *fakeRouteRedis) (singles, pipelines [][]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.singles {
		singles = append(singles, c.Args())
	}
	for _, p := range f.pipelines {
		for _, c := range p {
			pipelines = append(pipelines, c.Args())
		}
	}
	return singles, pipelines
}

// assertSameRedisTraffic:redis 源经分派函数发出的命令,与直接调今天的 Redis 实现发出的命令逐条相同。
func assertSameRedisTraffic(t *testing.T, reply func(int, redis.Cmder) (any, error), legacy, dispatched func(rdb *redis.Client) error) {
	t.Helper()
	legacyRdb, legacyRec := newFakeRouteRedis(t, reply)
	legacyErr := legacy(legacyRdb)
	newRdb, newRec := newFakeRouteRedis(t, reply)
	newErr := dispatched(newRdb)
	if fmt.Sprint(legacyErr) != fmt.Sprint(newErr) {
		t.Fatalf("redis source changed the outcome: legacy %v, dispatched %v", legacyErr, newErr)
	}
	ls, lp := recordedRedis(legacyRec)
	ns, np := recordedRedis(newRec)
	if !reflect.DeepEqual(ls, ns) || !reflect.DeepEqual(lp, np) {
		t.Fatalf("redis source changed the Redis traffic:\n legacy singles %v pipelines %v\n dispatched singles %v pipelines %v", ls, lp, ns, np)
	}
	if len(ls)+len(lp) == 0 {
		t.Fatal("no Redis command was recorded: the comparison proves nothing")
	}
}

// mappingRedisReply 是步骤 5 的 Redis 应答:merged_into 不在、SET 成功、SCRIPT LOAD 回 sha、CAS 一律「本次改了」。
func mappingRedisReply(_ int, cmd redis.Cmder) (any, error) {
	switch cmd.Name() {
	case "get":
		return nil, redis.Nil
	case "set":
		return "OK", nil
	case "script":
		return "0123456789abcdef0123456789abcdef01234567", nil
	case "evalsha":
		return int64(mappingChanged), nil
	}
	return nil, fmt.Errorf("unexpected command %v", cmd.Args())
}

// TestPlayerMappingStep_RedisSourceUnchanged:rb == nil 时步骤 5 发出的 Redis 命令与改动前(markMergedInto → remapPlayerMapping)逐条相同,
// 完成判据照旧,且不碰任何库原语(redis 源没有 backend 可碰)。
func TestPlayerMappingStep_RedisSourceUnchanged(t *testing.T) {
	ids := testIDs(3)
	ctx := context.Background()
	assertSameRedisTraffic(t, mappingRedisReply,
		func(rdb *redis.Client) error {
			if err := markMergedInto(ctx, rdb, testDBSrc, testDBDst, testDBMarkRun, false); err != nil {
				return err
			}
			rep, err := remapPlayerMapping(ctx, rdb, ids, testDBSrc, testDBDst, false)
			if err != nil {
				return err
			}
			return rep.complete(len(ids))
		},
		func(rdb *redis.Client) error {
			rep, err := runPlayerMappingStep(ctx, nil, rdb, ids, testDBSrc, testDBDst, testDBRun, testDBMarkRun, nil, false)
			if err == nil && rep.Published != 0 {
				return fmt.Errorf("redis source reported %d published rows", rep.Published)
			}
			return err
		})
}

// ── R8 读方 ───────────────────────────────────────────────────────────────────

// TestCollectPlayerIDsWithHomeZoneDB_PagesAcrossShardsAndSorts:按聚簇键翻页(满页才翻下一页,游标 = 上一页最后一行),
// 返回按 player_id 升序。
func TestCollectPlayerIDsWithHomeZoneDB_PagesAcrossShardsAndSorts(t *testing.T) {
	_, db, _ := newTestRouteBackend()
	var full []mergeRouteRow
	for i := 0; i < routeReadBatchLimit; i++ {
		full = append(full, mergeRouteRow{PlayerID: uint64(64*i + 5), HomeZone: testDBSrc})
	}
	var cursors []routeScanCursor
	db.scanByHome = func(zone uint32, after routeScanCursor, limit int) ([]mergeRouteRow, error) {
		if zone != testDBSrc || limit != routeReadBatchLimit {
			return nil, fmt.Errorf("ScanByHome(%d, %d)", zone, limit)
		}
		cursors = append(cursors, after)
		if len(cursors) == 1 {
			return full, nil
		}
		return []mergeRouteRow{{PlayerID: 3, HomeZone: testDBSrc}}, nil
	}
	ids, err := collectPlayerIDsWithHomeZoneDB(context.Background(), db, testDBSrc)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(ids) != routeReadBatchLimit+1 || ids[0] != 3 || ids[1] != 5 {
		t.Fatalf("ids = %d entries starting %v, want %d sorted ids starting [3 5]", len(ids), ids[:2], routeReadBatchLimit+1)
	}
	if want := []routeScanCursor{{}, routeCursorOf(full[len(full)-1].PlayerID)}; !reflect.DeepEqual(cursors, want) {
		t.Fatalf("cursors = %v, want %v", cursors, want)
	}
}

// TestRouteDBMappingPresence:「有归属」= 有行且 home 非 NULL;只有落点的行不算。
func TestRouteDBMappingPresence(t *testing.T) {
	_, db, _ := newTestRouteBackend()
	db.readRoutes = func([]uint64) (map[uint64]mergeRouteRow, error) {
		return map[uint64]mergeRouteRow{1: {PlayerID: 1, HomeZone: testDBSrc}, 2: {PlayerID: 2, StorageID: testDBSrc, PlacementVersion: 1}}, nil
	}
	got, err := routeDBMappingPresence(db)(context.Background(), []uint64{1, 2, 3})
	if err != nil || !reflect.DeepEqual(got, []bool{true, false, false}) {
		t.Fatalf("presence = (%v, %v), want [true false false]", got, err)
	}
	db.readRoutes = func([]uint64) (map[uint64]mergeRouteRow, error) { return nil, errors.New("1205") }
	if _, err := routeDBMappingPresence(db)(context.Background(), []uint64{1}); err == nil {
		t.Fatal("a DB error must not read as absence")
	}
}

// ── guild / trade:单行事务之前的闸门 ────────────────────────────────────────

// TestGatedZoneRowTxBeginner_StopsBeforeTheNextRow:闸门在第二个 id 的事务之前失权 → 第一行已提交、第二行起不开事务,错误带 errRouteFenceLost
// (不当锁冲突重试)。nil 闸门 = 今天的行为(全部改完)。
func TestGatedZoneRowTxBeginner_StopsBeforeTheNextRow(t *testing.T) {
	retry := lockRetryPolicy{attempts: 3, baseBackoff: time.Millisecond, maxBackoff: time.Millisecond,
		sleep: func(context.Context, time.Duration) error { return nil }}
	f := newFakeZoneTable(map[uint64]uint32{11: 901, 12: 901, 13: 901})
	gate, _ := countingGate(2)
	n, err := rewriteZoneRows(context.Background(), gatedZoneRowTxBeginner(f.begin, gate), retry, testZoneTarget, []uint64{11, 12, 13}, 901, 902)
	if !errors.Is(err, errRouteFenceLost) || n != 1 {
		t.Fatalf("rewrite = (%d, %v), want 1 row and errRouteFenceLost", n, err)
	}
	if want := []string{"begin", "lock 11", "update 11", "commit"}; !reflect.DeepEqual(f.log, want) {
		t.Fatalf("log = %v, want %v", f.log, want)
	}
	f = newFakeZoneTable(map[uint64]uint32{11: 901, 12: 901})
	if n, err := rewriteZoneRows(context.Background(), gatedZoneRowTxBeginner(f.begin, nil), retry, testZoneTarget, []uint64{11, 12}, 901, 902); err != nil || n != 2 {
		t.Fatalf("nil gate = (%d, %v), want both rows rewritten", n, err)
	}
}

// ── 装配:库与 mapping Redis 成对 ─────────────────────────────────────────────

// TestOpenRunRouteBackend_RedisSourcePairing:redis 源见到水位键就拒绝(这个 Redis 已归 db_first 管),没有水位键返回 nil
// (各写入点走今天的 Redis 路径)。
func TestOpenRunRouteBackend_RedisSourcePairing(t *testing.T) {
	ctx := context.Background()
	present := false
	rdb, _ := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		if cmd.Name() != "get" || cmd.Args()[1] != routeWatermarkKey {
			return nil, fmt.Errorf("unexpected %v", cmd.Args())
		}
		if !present {
			return nil, redis.Nil
		}
		return `{"v":1,"generation":"0123456789abcdef0123456789abcdef","watermark_ms":5}`, nil
	})
	ro := routeEntryOptions{source: routeSourceRedis}
	rb, err := openRunRouteBackend(ctx, ro, rdb, testDBRun)
	if err != nil || rb != nil {
		t.Fatalf("redis source without a watermark = (%v, %v), want (nil, nil)", rb, err)
	}
	present = true
	if _, err := openRunRouteBackend(ctx, ro, rdb, testDBRun); !errors.Is(err, errRoutePairing) {
		t.Fatalf("redis source with a watermark: error = %v, want errRoutePairing", err)
	}
	if _, err := openRunRouteBackend(ctx, routeEntryOptions{source: "bogus"}, rdb, testDBRun); !errors.Is(err, errRouteInvalidArgument) {
		t.Fatalf("unknown source: error = %v", err)
	}
	var nilRB *routeBackend
	nilRB.Close() // redis 源的 backend 是 nil,Close 必须是空操作
}

// TestRouteEntryOptionsOf:main 的 flag 原样进入口参数。
func TestRouteEntryOptionsOf(t *testing.T) {
	o := options{routeSource: routeSourceDB, routeDBDSN: "u:p@tcp(h:1)/g", waitAOFLocal: 1, waitAOFReplicas: 1, waitAOFTimeoutMs: 900,
		takeOverFenceRunID: testDBRun}
	want := routeEntryOptions{source: routeSourceDB, dbDSN: "u:p@tcp(h:1)/g", waitAOFLocal: 1, waitAOFReplicas: 1, waitAOFTimeoutMs: 900,
		takeOverRunID: testDBRun}
	if got := routeEntryOptionsOf(o); got != want {
		t.Fatalf("routeEntryOptionsOf = %+v, want %+v", got, want)
	}
}
