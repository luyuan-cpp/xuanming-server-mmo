package routereconcile

// 对账 leader 的单测:假 Store / 假 Cache / 假 Clock / 记录型 Observer,直接调 tickOnce(任务在启动它的那一拍里同步跑完,
// 下一拍开头收结果 = 「两拍之间跑完」),不起计时器、不依赖墙钟、不真睡。关停的两个用例才起真 goroutine(Run / Start),
// 它们的等待全部由假钟与通道驱动。
//
// 用例与规格的对应:scout 1b-2 reconciler_test 全部(NeverTouchesMergeFenceKeys 按 §13.1 第 3 条改写为 FenceMirrorFollowsDB)、
// VERDICT 补的用例(CatchUp… / LookbackSaturatesAtZero / FailoverIncremental… / IncrementalAfterPrewarm… / ThrottledIncremental… /
// ArmingRefused… / SustainedRedisErrors… / Shutdown…)、§13.1 新增行为各一条。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/metrics"
	"data_service/internal/store"

	"shared/drainproof"
	"shared/placement"
)

// 接口一变,编译期先炸:装配处把 *store.RouteStore / *RedisCache 直接交给 New。
var (
	_ Store = (*store.RouteStore)(nil)
	_ Cache = (*RedisCache)(nil)
)

const (
	testLeader = "route_leader@test-host:0000aaaa"
	testGen    = "0123456789abcdef0123456789abcdef"
	otherGen   = "fedcba9876543210fedcba9876543210"
	otherToken = "ffffffffffffffffffffffffffffffff"
)

var (
	idA = placement.RedisInstanceIdentity{RunID: strings.Repeat("a", 40), Role: "master", MasterReplID: strings.Repeat("1", 40)}
	idB = placement.RedisInstanceIdentity{RunID: strings.Repeat("b", 40), Role: "master", MasterReplID: strings.Repeat("2", 40)}
)

// ── 假钟 ──────────────────────────────────────────────────────────────────────

type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
	// sleepHook 在每次 Sleep 推进之后调(不持锁),用来同步推进假库时钟。
	sleepHook func(d time.Duration)
	// blockSleep:Sleep 一直阻塞到 ctx 结束(Run / Start 用例:每轮只跑一拍);sleeping 在进入阻塞时收到一个信号。
	blockSleep bool
	sleeping   chan struct{}
	wake       chan struct{}
	// afterFires:After 立即就绪(关停有界用例);否则永不就绪。
	afterFires bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0), sleeping: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	block := c.blockSleep
	c.mu.Unlock()
	if block {
		select {
		case c.sleeping <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.wake:
			return nil
		}
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	hook := c.sleepHook
	c.mu.Unlock()
	if hook != nil {
		hook(d)
	}
	return ctx.Err()
}

func (c *fakeClock) After(time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if c.afterFires {
		ch <- c.now
	}
	return ch
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// ── 假库 ──────────────────────────────────────────────────────────────────────

type scanCall struct {
	shard   uint32
	after   store.RouteUpdatedCursor
	limit   int
	updated bool // false = ScanRoutes(全表)
}

type fakeStore struct {
	mu         sync.Mutex
	nowMs      uint64
	holder     string
	leaseUntil uint64
	generation string
	writable   bool
	wm, cvm    uint64
	rows       map[uint64]store.RouteRow
	mergedInto []store.MergedIntoRow
	fences     []store.ActiveMergeFence
	// fenceListHook 非 nil 时按调用序号(从 1 起)给 ListActiveMergeFences 的结果。
	fenceListHook func(n int) []store.ActiveMergeFence
	// fenceZones 保留出现过围栏行的 zone，覆盖已释放但 63 号片仍有行的情况。
	fenceZones map[uint32]struct{}

	peeks, renews, releases, fenceLists int
	releaseErr                          error
	advanceReqs                         []store.ReconcileTickRequest
	freezes                             []store.RouteFreezeReason
	scans                               []scanCall

	// block 非 nil 时 ScanRoutes 先发 scanStarted 信号,再阻塞到 block 关闭(无视 ctx:模拟卡住的库调用)。
	block       chan struct{}
	scanStarted chan struct{}
	scanContext context.Context
}

func newFakeStore() *fakeStore {
	return &fakeStore{nowMs: 1_000_000, generation: testGen, writable: true, rows: map[uint64]store.RouteRow{}, fenceZones: map[uint32]struct{}{}}
}

func (s *fakeStore) setRow(r store.RouteRow) {
	s.mu.Lock()
	s.rows[r.PlayerID] = r
	s.mu.Unlock()
}

func (s *fakeStore) deleteRow(id uint64) {
	s.mu.Lock()
	delete(s.rows, id)
	s.mu.Unlock()
}

func (s *fakeStore) addNow(ms uint64) {
	s.mu.Lock()
	s.nowMs += ms
	s.mu.Unlock()
}

func (s *fakeStore) heldLocked(id string) bool {
	return id != "" && s.holder == id && s.leaseUntil >= s.nowMs
}

func (s *fakeStore) peekLocked() store.RouteLease {
	return store.RouteLease{Held: s.holder != "" && s.leaseUntil >= s.nowMs, Holder: s.holder, LeaseUntilMs: s.leaseUntil, NowMs: s.nowMs}
}

func (s *fakeStore) PeekReconcileLease(context.Context) (store.RouteLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peeks++
	return s.peekLocked(), nil
}

func (s *fakeStore) RenewReconcileLease(_ context.Context, id string, leaseMs uint64) (store.RouteLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renews++
	if s.holder == "" || s.holder == id || s.leaseUntil < s.nowMs {
		s.holder, s.leaseUntil = id, s.nowMs+leaseMs
		return store.RouteLease{Held: true, Holder: id}, nil
	}
	return s.peekLocked(), nil
}

func (s *fakeStore) ReleaseReconcileLease(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
	if s.releaseErr != nil {
		return false, s.releaseErr
	}
	if s.holder != id {
		return false, nil
	}
	s.holder, s.leaseUntil = "", 0
	return true, nil
}

func (s *fakeStore) AdvanceReconcile(_ context.Context, req store.ReconcileTickRequest) (store.ReconcileTickResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := req
	if req.SetCacheVerifiedMs != nil {
		v := *req.SetCacheVerifiedMs
		cp.SetCacheVerifiedMs = &v
	}
	if req.WatermarkKey != nil {
		w := *req.WatermarkKey
		cp.WatermarkKey = &w
	}
	s.advanceReqs = append(s.advanceReqs, cp)
	res := store.ReconcileTickResult{Generation: s.generation, WatermarkMs: s.wm, CacheVerifiedMs: s.cvm, NowMs: s.nowMs}
	switch {
	case !s.heldLocked(req.LeaderID):
		res.Outcome = store.ReconcileTickLeaseLost
		return res, nil
	case !s.writable:
		res.Outcome = store.ReconcileTickAlreadyFrozen
		return res, nil
	}
	if k := req.WatermarkKey; k != nil {
		switch {
		case k.Generation != s.generation:
			res.Outcome, res.FreezeReason = store.ReconcileTickFrozen, store.RouteFreezeGenerationMismatch
			return store.ReconcileTickResult{}, &store.RouteEmergencyError{Observation: res}
		case k.WatermarkMs > s.wm && k.WatermarkMs-s.wm > req.ToleranceMs:
			res.Outcome, res.FreezeReason = store.ReconcileTickFrozen, store.RouteFreezeWatermarkRegressed
			return store.ReconcileTickResult{}, &store.RouteEmergencyError{Observation: res}
		}
	}
	s.wm = max(s.wm, s.nowMs)
	if req.SetCacheVerifiedMs != nil {
		s.cvm = min(*req.SetCacheVerifiedMs, s.nowMs)
	}
	res.Outcome, res.WatermarkMs, res.CacheVerifiedMs = store.ReconcileTickAdvanced, s.wm, s.cvm
	return res, nil
}

func (s *fakeStore) FreezeByLeader(_ context.Context, id string, reason store.RouteFreezeReason, _ time.Duration) (store.ReconcileTickResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.freezes = append(s.freezes, reason)
	res := store.ReconcileTickResult{Generation: s.generation, WatermarkMs: s.wm, CacheVerifiedMs: s.cvm, NowMs: s.nowMs}
	switch {
	case !s.heldLocked(id):
		res.Outcome = store.ReconcileTickLeaseLost
	case !s.writable:
		res.Outcome = store.ReconcileTickAlreadyFrozen
	default:
		res.Outcome, res.FreezeReason = store.ReconcileTickFrozen, reason
		return store.ReconcileTickResult{}, &store.RouteEmergencyError{Observation: res}
	}
	return res, nil
}

func (s *fakeStore) sortedRows(less func(a, b store.RouteRow) bool) []store.RouteRow {
	out := make([]store.RouteRow, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}

func (s *fakeStore) ScanRoutes(ctx context.Context, after store.RouteScanCursor, limit int) ([]store.RouteRow, error) {
	s.mu.Lock()
	block, started := s.block, s.scanStarted
	s.scanContext = ctx
	s.scans = append(s.scans, scanCall{shard: after.Shard, after: store.RouteUpdatedCursor{PlayerID: after.PlayerID}, limit: limit})
	s.mu.Unlock()
	if block != nil {
		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		<-block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.RouteRow
	for _, r := range s.sortedRows(func(a, b store.RouteRow) bool { return store.RouteScanCursorAt(a).Less(store.RouteScanCursorAt(b)) }) {
		if after.Less(store.RouteScanCursorAt(r)) && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeStore) ScanRoutesUpdatedSince(_ context.Context, shard uint32, after store.RouteUpdatedCursor, limit int) ([]store.RouteRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scans = append(s.scans, scanCall{shard: shard, after: after, limit: limit, updated: true})
	var out []store.RouteRow
	for _, r := range s.sortedRows(func(a, b store.RouteRow) bool {
		return store.RouteUpdatedCursorAt(a).Less(store.RouteUpdatedCursorAt(b))
	}) {
		if placement.RouteRowShard(r.PlayerID) == shard && after.Less(store.RouteUpdatedCursorAt(r)) && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeStore) GetRoutes(_ context.Context, ids []uint64) (map[uint64]store.RouteRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uint64]store.RouteRow{}
	for _, id := range ids {
		if r, ok := s.rows[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

func (s *fakeStore) ListMergedInto(context.Context) ([]store.MergedIntoRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.MergedIntoRow(nil), s.mergedInto...), nil
}

func (s *fakeStore) ListActiveMergeFences(context.Context) (store.ActiveMergeFences, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fenceLists++
	fences := s.fences
	if s.fenceListHook != nil {
		fences = s.fenceListHook(s.fenceLists)
	}
	for _, fence := range fences {
		s.fenceZones[fence.ZoneID] = struct{}{}
	}
	return store.ActiveMergeFences{DBNowMs: s.nowMs, Fences: append([]store.ActiveMergeFence(nil), fences...)}, nil
}

func (s *fakeStore) ListMergeFenceZones(context.Context) ([]uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zones []uint32
	for zone := range s.fenceZones {
		zones = append(zones, zone)
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i] < zones[j] })
	return zones, nil
}

func (s *fakeStore) DBNowMs(context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nowMs, nil
}

// ── 假 Redis ──────────────────────────────────────────────────────────────────

type fakeCache struct {
	mu      sync.Mutex
	calls   []string
	id      placement.RedisInstanceIdentity
	probeEr error
	policy  string
	evicted int64
	debug   string
	kv      map[string]string
	// readyWrongType:就绪标记键在但不是 string。
	readyWrongType bool
	// 水位键错误类型与 Redis MGET 的缺席返回分开记录。
	watermarkWrongType bool
	pushes             [][]uint64
	// pushHook 在 Push 判定之前调(持着 c.mu:只许直接改 c.kv / c.id,不许调 fakeCache 的方法)。
	pushHook func(c *fakeCache, rows []store.RouteRow)
	forced   map[uint64]PushOutcome
	arms     int
	pexpire  map[string]uint64
	closed   bool
	// 钩子在不持 c.mu 时调用，可通过同一 Cache 接口读取本拍模拟页面中的键。
	fenceScanHook func(context.Context, uint64, int) ([]FenceMirror, uint64, error)
	fenceGetHook  func(context.Context, []string) ([]FenceMirror, error)
	fenceScans    []fenceScanCall
	fenceGets     [][]string
}

type fenceScanCall struct {
	cursor uint64
	pages  int
}

func newFakeCache() *fakeCache {
	return &fakeCache{id: idA, policy: "noeviction", debug: "no", kv: map[string]string{}, forced: map[uint64]PushOutcome{}, pexpire: map[string]uint64{}}
}

func (c *fakeCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.kv[key]
	return v, ok
}

func (c *fakeCache) set(key, value string) {
	c.mu.Lock()
	c.kv[key] = value
	c.mu.Unlock()
}

func (c *fakeCache) del(keys ...string) {
	c.mu.Lock()
	for _, k := range keys {
		delete(c.kv, k)
	}
	c.mu.Unlock()
}

func (c *fakeCache) setIdentity(id placement.RedisInstanceIdentity) {
	c.mu.Lock()
	c.id = id
	c.mu.Unlock()
}

func (c *fakeCache) callIndex(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, n := range c.calls {
		if n == name {
			return i
		}
	}
	return -1
}

func (c *fakeCache) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, x := range c.calls {
		if x == name {
			n++
		}
	}
	return n
}

func (c *fakeCache) record(name string) { c.calls = append(c.calls, name) }

func (c *fakeCache) Probe(context.Context) (Probe, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("Probe")
	if c.probeEr != nil {
		return Probe{}, c.probeEr
	}
	return Probe{Identity: c.id, MaxmemoryPolicy: c.policy, EvictedKeys: c.evicted, DebugCommand: c.debug}, nil
}

func (c *fakeCache) ReadControl(context.Context) (Control, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("ReadControl")
	if c.probeEr != nil {
		return Control{}, c.probeEr
	}
	ptr := func(k string) *string {
		if k == placement.RouteWatermarkKey && c.watermarkWrongType {
			return nil
		}
		if v, ok := c.kv[k]; ok {
			return &v
		}
		return nil
	}
	return Control{Ready: ptr(placement.RouteReadyKey), ReadyWrongType: c.readyWrongType, Warm: ptr(placement.RouteWarmKey), Watermark: ptr(placement.RouteWatermarkKey), WatermarkWrongType: c.watermarkWrongType}, nil
}

func (c *fakeCache) SelfCheck(context.Context) (placement.RouteReadyRead, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("SelfCheck")
	v, ok := c.kv[placement.RouteReadyKey]
	return placement.RouteReadyRead{MarkerPresent: ok, MarkerRaw: v, Identity: c.id, Values: []any{}}, nil
}

func (c *fakeCache) SetWarmToken(_ context.Context, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("SetWarmToken")
	if !placement.ValidRouteWarmToken(token) {
		return fmt.Errorf("bad token %q", token)
	}
	c.kv[placement.RouteWarmKey] = token
	return nil
}

func (c *fakeCache) ArmReady(_ context.Context, token string, m placement.RouteReadyMarker) (placement.RouteReadyArmOutcome, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("ArmReady")
	c.arms++
	_, args, err := placement.RouteReadyArmKeysAndArgs(token, m)
	if err != nil {
		return 0, err
	}
	if c.kv[placement.RouteWarmKey] != token {
		return placement.RouteReadyArmTokenMismatch, nil
	}
	if _, ok := c.kv[placement.RouteReadyKey]; ok || c.readyWrongType {
		return placement.RouteReadyArmAlreadyPresent, nil
	}
	if c.id.RunID != m.RedisRunID || c.id.MasterReplID != m.MasterReplID || c.id.Role != "master" {
		return placement.RouteReadyArmIdentityChanged, nil
	}
	c.kv[placement.RouteReadyKey] = args[1].(string)
	return placement.RouteReadyArmArmed, nil
}

func (c *fakeCache) casLocked(key, old, next string) bool {
	if cur, ok := c.kv[key]; ok && cur == old {
		c.kv[key] = next
		return true
	}
	return false
}

func (c *fakeCache) cadLocked(key, old string) bool {
	if cur, ok := c.kv[key]; ok && cur == old {
		delete(c.kv, key)
		return true
	}
	return false
}

func (c *fakeCache) CompareAndSetReady(_ context.Context, old, next string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("CompareAndSetReady")
	return c.casLocked(placement.RouteReadyKey, old, next), nil
}

func (c *fakeCache) CompareAndDeleteReady(_ context.Context, old string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("CompareAndDeleteReady")
	return c.cadLocked(placement.RouteReadyKey, old), nil
}

func (c *fakeCache) DeleteReadyWrongType(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("DeleteReadyWrongType")
	c.readyWrongType = false
	delete(c.kv, placement.RouteReadyKey)
	return nil
}

// 与正式 RedisCache 相同：调用方先判错误类型，此方法本身无条件 DEL。
func (c *fakeCache) DeleteWatermarkWrongType(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("DeleteWatermarkWrongType")
	c.watermarkWrongType = false
	delete(c.kv, placement.RouteWatermarkKey)
	return nil
}

func (c *fakeCache) AdvanceWatermark(_ context.Context, prev *string, next string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("AdvanceWatermark")
	if prev == nil {
		if _, ok := c.kv[placement.RouteWatermarkKey]; ok {
			return false, nil
		}
		c.kv[placement.RouteWatermarkKey] = next
		return true, nil
	}
	return c.casLocked(placement.RouteWatermarkKey, *prev, next), nil
}

// Push 按发布脚本的语义模拟(set-if-newer、遗留值守卫、touch 不计),外加 forced 指定的结局。
func (c *fakeCache) Push(_ context.Context, rows []store.RouteRow) ([]PushRowResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("Push")
	ids := make([]uint64, len(rows))
	for i, r := range rows {
		ids[i] = r.PlayerID
	}
	c.pushes = append(c.pushes, ids)
	if c.pushHook != nil {
		c.pushHook(c, rows)
	}
	eq := func(v string, present bool, want string) bool {
		if !present {
			return want == ""
		}
		return v == want
	}
	out := make([]PushRowResult, len(rows))
	for i, row := range rows {
		out[i].PlayerID = row.PlayerID
		if o, ok := c.forced[row.PlayerID]; ok {
			out[i].Outcome = o
			continue
		}
		if row.IsTombstone() {
			out[i].Outcome = PushTombstoneSkipped
			continue
		}
		plc, err := row.PlacementValue()
		if err != nil {
			out[i].Outcome = PushDBInvalid
			continue
		}
		home, rev := row.HomeValue(), placement.FormatRev(row.Rev)
		zk, pk, mk := placement.HomeZoneKey(row.PlayerID), placement.Key(row.PlayerID), placement.MapRevKey(row.PlayerID)
		curZ, hz := c.kv[zk]
		curP, hp := c.kv[pk]
		curM, hm := c.kv[mk]
		if hm && curM == rev && eq(curZ, hz, home) && eq(curP, hp, plc) {
			out[i].Outcome = PushSkippedCurrent
			continue
		}
		if hm {
			if cmp, _ := placement.CompareRev(curM, rev); cmp >= 0 {
				switch {
				case cmp > 0:
					out[i].Outcome, out[i].CacheRev = PushSuperseded, curM
				case eq(curZ, hz, home) && eq(curP, hp, plc):
					out[i].Outcome = PushConfirmed
				default:
					out[i].Outcome = PushConflict
				}
				continue
			}
		} else if (hz && curZ != home) || (hp && curP != plc) {
			out[i].Outcome = PushLegacyConflict
			continue
		}
		if home == "" {
			delete(c.kv, zk)
		} else {
			c.kv[zk] = home
		}
		if plc == "" {
			delete(c.kv, pk)
		} else {
			c.kv[pk] = plc
		}
		c.kv[mk] = rev
		out[i].Outcome = PushApplied
	}
	return out, nil
}

func (c *fakeCache) MirrorMergedInto(_ context.Context, rows []store.MergedIntoRow) (MergedIntoMirrorResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("MirrorMergedInto")
	var res MergedIntoMirrorResult
	for _, r := range rows {
		v, err := placement.EncodeMergedIntoValue(r.DstZone, r.RunID)
		if err != nil {
			res.Errors++
			continue
		}
		k := placement.MergedIntoKey(r.SrcZone)
		cur, ok := c.kv[k]
		switch {
		case !ok:
			c.kv[k] = v
			res.Added++
		case cur == v:
			res.Present++
		default:
			res.Conflicts++
		}
	}
	return res, nil
}

func (c *fakeCache) ScanFenceMirrors(ctx context.Context, cursor uint64, pages int) ([]FenceMirror, uint64, error) {
	c.mu.Lock()
	c.record("ScanFenceMirrors")
	c.fenceScans = append(c.fenceScans, fenceScanCall{cursor: cursor, pages: pages})
	hook := c.fenceScanHook
	c.mu.Unlock()
	if hook != nil {
		return hook(ctx, cursor, pages)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []FenceMirror
	for k, v := range c.kv {
		if strings.HasPrefix(k, fenceMirrorKeyPrefix) {
			out = append(out, FenceMirror{Key: k, Zone: fenceMirrorZone(k), Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, 0, nil
}

func (c *fakeCache) GetFenceMirrors(ctx context.Context, keys []string) ([]FenceMirror, error) {
	c.mu.Lock()
	c.record("GetFenceMirrors")
	c.fenceGets = append(c.fenceGets, append([]string(nil), keys...))
	hook := c.fenceGetHook
	c.mu.Unlock()
	if hook != nil {
		return hook(ctx, keys)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []FenceMirror
	for _, key := range keys {
		if value, ok := c.kv[key]; ok {
			out = append(out, FenceMirror{Key: key, Zone: fenceMirrorZone(key), Value: value})
		}
	}
	return out, nil
}

func (c *fakeCache) SetFenceMirror(_ context.Context, zone uint32, holder string, epoch, expiresAtMs uint64) (placement.MergeFenceMirrorOutcome, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("SetFenceMirror")
	keys, args, err := placement.MergeFenceMirrorArgs(zone, holder, epoch, expiresAtMs)
	if err != nil {
		return 0, err
	}
	if cur, ok := c.kv[keys[0]]; ok {
		got, has := placement.MergeFenceHolderEpoch(cur)
		if !has {
			return placement.MergeFenceMirrorForeign, nil
		}
		if got > epoch {
			return placement.MergeFenceMirrorStale, nil
		}
	}
	c.kv[keys[0]] = holder
	c.pexpire[keys[0]] = expiresAtMs + placement.MergeFenceMirrorMarginMs
	_ = args
	return placement.MergeFenceMirrorWritten, nil
}

func (c *fakeCache) CompareAndDeleteFenceMirror(_ context.Context, key, value string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("CompareAndDeleteFenceMirror")
	return c.cadLocked(key, value), nil
}

func (c *fakeCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// ── 记录型 Observer ───────────────────────────────────────────────────────────

type observation struct {
	marker, writable bool
	age              float64
	verdict          string
}

type fakeObserver struct {
	mu           sync.Mutex
	ticks        []string
	events       map[string]int
	pushes       map[string]int
	mirrors      map[string]int
	leader       []bool
	observations []observation
	cleared      int
}

func newFakeObserver() *fakeObserver {
	return &fakeObserver{events: map[string]int{}, pushes: map[string]int{}, mirrors: map[string]int{}}
}

func (o *fakeObserver) Tick(r string) { o.mu.Lock(); o.ticks = append(o.ticks, r); o.mu.Unlock() }
func (o *fakeObserver) Event(e string) {
	o.mu.Lock()
	o.events[e]++
	o.mu.Unlock()
}
func (o *fakeObserver) Push(phase, result string, n int) {
	o.mu.Lock()
	o.pushes[phase+"/"+result] += n
	o.mu.Unlock()
}
func (o *fakeObserver) Mirror(result string, n int) {
	o.mu.Lock()
	o.mirrors[result] += n
	o.mu.Unlock()
}
func (o *fakeObserver) TaskSeconds(string, time.Duration) {}
func (o *fakeObserver) TaskRows(string, int)              {}
func (o *fakeObserver) Leader(held bool) {
	o.mu.Lock()
	o.leader = append(o.leader, held)
	o.mu.Unlock()
}
func (o *fakeObserver) LeaderObservations(marker, writable bool, age float64, verdict string) {
	o.mu.Lock()
	o.observations = append(o.observations, observation{marker, writable, age, verdict})
	o.mu.Unlock()
}
func (o *fakeObserver) ClearLeaderObservations() {
	o.mu.Lock()
	o.cleared++
	o.mu.Unlock()
}

func (o *fakeObserver) event(e string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.events[e]
}

func (o *fakeObserver) push(phase, result string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pushes[phase+"/"+result]
}

func (o *fakeObserver) isLeader() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.leader) > 0 && o.leader[len(o.leader)-1]
}

// seqReader 是确定性的「随机源」:每个字节递增,令牌两两不同。
type seqReader struct {
	mu sync.Mutex
	n  byte
}

func (s *seqReader) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range p {
		s.n++
		p[i] = s.n
	}
	return len(p), nil
}

// ── 装配 ──────────────────────────────────────────────────────────────────────

type harness struct {
	t        *testing.T
	ctx      context.Context
	r        *Reconciler
	st       *fakeStore
	c        *fakeCache
	clk      *fakeClock
	obs      *fakeObserver
	dangerMu sync.Mutex
	dangers  []store.ReconcileTickResult
}

func newHarness(t *testing.T, arm bool, tweak func(rc *config.RouteReconcileConfig)) *harness {
	t.Helper()
	rc := config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst}.Normalize()
	rc.Reconcile.ArmReadyMarker = arm
	if tweak != nil {
		tweak(&rc.Reconcile)
	}
	h := &harness{t: t, ctx: context.Background(), st: newFakeStore(), c: newFakeCache(), clk: newFakeClock(), obs: newFakeObserver()}
	r, err := New(rc, Deps{Store: h.st, Cache: h.c, LeaderID: testLeader, Random: &seqReader{}, Clock: h.clk, Observer: h.obs, OnDanger: func(_ context.Context, danger *store.RouteEmergencyError) error {
		h.dangerMu.Lock()
		defer h.dangerMu.Unlock()
		h.dangers = append(h.dangers, danger.Observation)
		return nil // 仅记录交给维护 owner 的真实 typed 观察，不冒充 SQL 冻结。
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.syncTasks = true
	h.r = r
	return h
}

func (h *harness) assertDanger(reason store.RouteFreezeReason) {
	h.t.Helper()
	h.dangerMu.Lock()
	defer h.dangerMu.Unlock()
	if len(h.dangers) != 1 || h.dangers[0].FreezeReason != reason || !h.r.halted.Load() || !h.st.writable {
		h.t.Fatalf("危险观察=%+v halted=%t writable=%t；应停止本 boot，SQL 必须等正式维护 owner", h.dangers, h.r.halted.Load(), h.st.writable)
	}
}

func (h *harness) tick() string { return h.r.tickOnce(h.ctx) }

// ticks 跑 n 拍,每拍之前把单调时钟推进 step。返回每拍的结果。
func (h *harness) ticks(n int, step time.Duration) []string {
	out := make([]string, n)
	for i := range out {
		h.clk.advance(step)
		out[i] = h.tick()
	}
	return out
}

// started 返回最近一拍启动(并已同步跑完)的任务阶段;这一拍没有启动任务回 0。
func (h *harness) started() taskPhase {
	if h.r.syncResult == nil {
		return 0
	}
	return h.r.syncResult.phase
}

func (h *harness) mustTick(want string) {
	h.t.Helper()
	if got := h.tick(); got != want {
		h.t.Fatalf("tick = %q, want %q", got, want)
	}
}

func homeRow(id uint64, home uint32, rev, updated uint64) store.RouteRow {
	return store.RouteRow{PlayerID: id, HomeZone: home, Rev: rev, UpdatedBy: "test", CreatedAtMs: 1, UpdatedAtMs: updated}
}

func markerFor(t *testing.T, id placement.RedisInstanceIdentity, gen string, wm uint64) string {
	t.Helper()
	raw, err := placement.EncodeRouteReadyMarker(placement.RouteReadyMarker{
		Generation: gen, RedisRunID: id.RunID, MasterReplID: id.MasterReplID, SetAtMs: 1, WatermarkMs: wm, Leader: "route_leader@old:00000000",
	})
	if err != nil {
		t.Fatalf("encode marker: %v", err)
	}
	return raw
}

func watermarkFor(t *testing.T, gen string, wm uint64) string {
	t.Helper()
	raw, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: gen, WatermarkMs: wm})
	if err != nil {
		t.Fatalf("encode watermark: %v", err)
	}
	return raw
}

func holderJSON(run string, epoch uint64) string {
	return fmt.Sprintf(`{"run_id":%q,"fence_epoch":%d}`, run, epoch)
}

// firstUpdatedScan 返回第一次按 updated_at_ms 扫库的起点(没有回 nil)。
func (h *harness) firstUpdatedScan(from int) *scanCall {
	h.st.mu.Lock()
	defer h.st.mu.Unlock()
	for i := from; i < len(h.st.scans); i++ {
		if h.st.scans[i].updated {
			c := h.st.scans[i]
			return &c
		}
	}
	return nil
}

func (h *harness) scanCount() int {
	h.st.mu.Lock()
	defer h.st.mu.Unlock()
	return len(h.st.scans)
}

func (h *harness) lastAdvance() store.ReconcileTickRequest {
	h.st.mu.Lock()
	defer h.st.mu.Unlock()
	if len(h.st.advanceReqs) == 0 {
		h.t.Fatal("no AdvanceReconcile call")
	}
	return h.st.advanceReqs[len(h.st.advanceReqs)-1]
}

func (h *harness) pushedIDs() []uint64 {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	var out []uint64
	for _, p := range h.c.pushes {
		out = append(out, p...)
	}
	return out
}

func (h *harness) readyPresent() bool {
	_, ok := h.c.get(placement.RouteReadyKey)
	return ok
}

// ── 构造与租约 ────────────────────────────────────────────────────────────────

// TestNotConstructedInRedisMode:redis 模式与急停(Enabled=false)一律不构造(规格 scout D1)。
func TestNotConstructedInRedisMode(t *testing.T) {
	deps := Deps{Store: newFakeStore(), Cache: newFakeCache(), LeaderID: testLeader, Random: &seqReader{}}
	if _, err := New(config.RouteStoreConfig{}.Normalize(), deps); !errors.Is(err, ErrDisabled) {
		t.Fatalf("redis mode must not construct the leader, got %v", err)
	}
	off := false
	rc := config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst}.Normalize()
	rc.Reconcile.Enabled = &off
	if _, err := New(rc, deps); !errors.Is(err, ErrDisabled) {
		t.Fatalf("db_first with Enabled=false must not construct the leader, got %v", err)
	}
	rc.Reconcile.Enabled = nil
	if _, err := New(rc.Normalize(), Deps{Cache: newFakeCache(), LeaderID: testLeader, Random: &seqReader{}}); err == nil {
		t.Fatal("a missing route store must be rejected")
	}
}

// TestNonLeaderOnlyPeeks:别人持有有效租约时只 Peek,不 Renew,不碰 Redis(不读身份、不写任何键)。
func TestNonLeaderOnlyPeeks(t *testing.T) {
	h := newHarness(t, true, nil)
	h.st.holder, h.st.leaseUntil = "route_leader@other:11111111", h.st.nowMs+10_000
	for i := 0; i < 3; i++ {
		h.mustTick(metrics.RouteReconcileTickNotLeader)
	}
	if h.st.renews != 0 || h.st.peeks != 3 {
		t.Fatalf("renews=%d peeks=%d, want 0 / 3", h.st.renews, h.st.peeks)
	}
	if len(h.c.calls) != 0 {
		t.Fatalf("a non-leader must not touch the mapping redis, got calls %v", h.c.calls)
	}
	if len(h.obs.observations) != 0 {
		t.Fatal("a non-leader must not export leader observations")
	}
}

// TestPrewarmArmsOnlyWhenArmEnabled:Arm=true 走全量预热并挂标记;Arm=false 只追平(catch_up),从不挂标记。
func TestPrewarmArmsOnlyWhenArmEnabled(t *testing.T) {
	for _, arm := range []bool{true, false} {
		t.Run(fmt.Sprintf("arm=%t", arm), func(t *testing.T) {
			h := newHarness(t, arm, nil)
			h.st.setRow(homeRow(7, 3, 1, 900_000))
			h.mustTick(metrics.RouteReconcileTickOK)
			want := phaseCatchUp
			if arm {
				want = phasePrewarm
			}
			if got := h.started(); got != want {
				t.Fatalf("first task = %s, want %s", got, want)
			}
			h.ticks(3, time.Second)
			if arm != h.readyPresent() || (h.obs.event(metrics.RouteReconcileEventMarkerArmed) == 1) != arm {
				t.Fatalf("arm=%t: marker present=%t armed events=%d", arm, h.readyPresent(), h.obs.event(metrics.RouteReconcileEventMarkerArmed))
			}
			if !arm && h.c.arms != 0 {
				t.Fatalf("Arm=false must never call ArmReady, got %d", h.c.arms)
			}
			if z, _ := h.c.get(placement.HomeZoneKey(7)); z != "3" {
				t.Fatalf("player 7 not pushed (zone key %q)", z)
			}
		})
	}
}

// TestPrewarmConvergesWithNewerConcurrentPublish:预热推旧 rev 时在线写者已发布了新 rev(superseded),复读库 rev 不小于缓存 rev
// → 正常竞态,不冻结,照常挂标记。
func TestPrewarmConvergesWithNewerConcurrentPublish(t *testing.T) {
	h := newHarness(t, true, nil)
	h.st.setRow(homeRow(7, 3, 1, 900_000))
	once := false
	h.c.pushHook = func(c *fakeCache, rows []store.RouteRow) {
		if once {
			return
		}
		once = true
		newer := homeRow(7, 3, 2, 990_000)
		newer.StorageID, newer.PlacementVersion = 3, 1
		h.st.setRow(newer)
		c.kv[placement.HomeZoneKey(7)], c.kv[placement.Key(7)], c.kv[placement.MapRevKey(7)] = "3", "3:1", "2"
	}
	h.ticks(2, 0)
	if len(h.st.freezes) != 0 {
		t.Fatalf("a newer concurrent publish must not freeze, got %v", h.st.freezes)
	}
	if h.obs.push(metrics.RouteReconcilePhasePrewarm, metrics.RouteReconcilePushSuperseded) != 1 {
		t.Fatalf("want one superseded row, got pushes %v", h.obs.pushes)
	}
	if !h.readyPresent() || h.obs.event(metrics.RouteReconcileEventMarkerArmed) != 1 {
		t.Fatal("the prewarm must still arm the marker")
	}
}

// TestCacheAheadFreezes:superseded 之后复读库 rev < 缓存 rev → cache_ahead:FreezeByLeader,不挂标记,之后不再推送。
func TestCacheAheadFreezes(t *testing.T) {
	h := newHarness(t, true, nil)
	h.st.setRow(homeRow(9, 3, 1, 900_000))
	h.c.set(placement.HomeZoneKey(9), "3")
	h.c.set(placement.MapRevKey(9), "5")
	h.mustTick(metrics.RouteReconcileTickOK)
	if len(h.st.freezes) != 1 || h.st.freezes[0] != store.RouteFreezeCacheAhead || !h.st.writable {
		t.Fatalf("freezes=%v writable=%t, want one cache_ahead freeze", h.st.freezes, h.st.writable)
	}
	h.assertDanger(store.RouteFreezeCacheAhead)
	h.r.collectTaskResult(h.ctx)
	if h.obs.event(metrics.RouteReconcileEventFreezeCacheAhead) != 0 || h.obs.push(metrics.RouteReconcilePhasePrewarm, metrics.RouteReconcilePushCacheAhead) != 1 {
		t.Fatalf("events=%v pushes=%v", h.obs.events, h.obs.pushes)
	}
	pushes := len(h.c.pushes)
	h.r.Run(h.ctx) // halted 后循环直接退出，不能再起新发布。
	if len(h.c.pushes) != pushes || h.readyPresent() || h.c.arms != 0 {
		t.Fatalf("frozen: pushes %d → %d, marker=%t arms=%d; want no pushes and no marker", pushes, len(h.c.pushes), h.readyPresent(), h.c.arms)
	}
}

// TestSupersededRowMissingInDBFreezesCacheAhead(核实意见 nit f):superseded 之后复读不到这一行 → 按 cache_ahead 冻结。
func TestSupersededRowMissingInDBFreezesCacheAhead(t *testing.T) {
	h := newHarness(t, true, nil)
	h.st.setRow(homeRow(9, 3, 1, 900_000))
	h.c.set(placement.MapRevKey(9), "4")
	h.c.set(placement.HomeZoneKey(9), "3")
	h.c.pushHook = func(*fakeCache, []store.RouteRow) { h.st.deleteRow(9) }
	h.mustTick(metrics.RouteReconcileTickOK)
	if len(h.st.freezes) != 1 || h.st.freezes[0] != store.RouteFreezeCacheAhead {
		t.Fatalf("a superseded row that vanished from the database must freeze as cache_ahead, got %v", h.st.freezes)
	}
	h.assertDanger(store.RouteFreezeCacheAhead)
}

// ── 身份变化与回看修补 ────────────────────────────────────────────────────────

// TestIdentityChangeDropsMarkerBeforeRepair:标记在而身份不符 → 先 CAD(关门),再 64 片按 updated_at_ms 扫描、推送,最后挂标记;
// 起点 = min(标记水位, cvm) − 60s。
func TestIdentityChangeDropsMarkerBeforeRepair(t *testing.T) {
	h := newHarness(t, true, nil)
	h.c.setIdentity(idB)
	h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 500_000))
	h.st.cvm = 400_000
	h.st.setRow(homeRow(11, 3, 1, 360_000))
	h.st.setRow(homeRow(12, 3, 1, 300_000))
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.started() != phaseRepair {
		t.Fatalf("task = %s, want repair", h.started())
	}
	if h.obs.event(metrics.RouteReconcileEventMarkerDroppedIdentity) != 1 {
		t.Fatal("the stale marker must be dropped")
	}
	first := h.firstUpdatedScan(0)
	if first == nil || first.after.UpdatedAtMs != 340_000 || first.after.PlayerID != 0 {
		t.Fatalf("repair scan starts at %+v, want updated_at_ms >= 340000 (min(500000, 400000) - 60000)", first)
	}
	if ids := h.pushedIDs(); len(ids) != 1 || ids[0] != 11 {
		t.Fatalf("repair pushed %v, want only player 11 (inside the lookback window)", ids)
	}
	h.mustTick(metrics.RouteReconcileTickOK)
	cad, push, arm := h.c.callIndex("CompareAndDeleteReady"), h.c.callIndex("Push"), h.c.callIndex("ArmReady")
	if !(cad >= 0 && cad < push && push < arm) {
		t.Fatalf("order CAD(%d) → Push(%d) → ArmReady(%d) violated: %v", cad, push, arm, h.c.calls)
	}
	raw, _ := h.c.get(placement.RouteReadyKey)
	m, err := placement.DecodeRouteReadyMarker(raw)
	if err != nil || m.RedisRunID != idB.RunID {
		t.Fatalf("re-armed marker %q (%v) must carry the new identity", raw, err)
	}
}

// TestOldBackupMarkerUsesItsWatermarkAsLookbackStart:旧备份里的标记带着备份时刻的水位,回看起点按它算(比库 cvm 更早)。
func TestOldBackupMarkerUsesItsWatermarkAsLookbackStart(t *testing.T) {
	h := newHarness(t, true, nil)
	h.c.setIdentity(idB)
	h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 100_000))
	h.st.cvm = 900_000
	h.mustTick(metrics.RouteReconcileTickOK)
	if first := h.firstUpdatedScan(0); first == nil || first.after.UpdatedAtMs != 40_000 {
		t.Fatalf("repair scan starts at %+v, want 40000 (marker watermark 100000 - 60000)", first)
	}
}

// TestLookbackSaturatesAtZero(VERDICT must-1):cvm=0、标记水位很小时,起点饱和到 0,不回绕成接近 2^64 的数(那会扫不到任何行)。
func TestLookbackSaturatesAtZero(t *testing.T) {
	t.Run("repair", func(t *testing.T) {
		h := newHarness(t, true, nil)
		h.c.setIdentity(idB)
		h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 1_000))
		h.st.setRow(homeRow(5, 3, 1, 10))
		h.mustTick(metrics.RouteReconcileTickOK)
		if first := h.firstUpdatedScan(0); first == nil || first.after.UpdatedAtMs != 0 {
			t.Fatalf("repair scan starts at %+v, want 0", first)
		}
		if ids := h.pushedIDs(); len(ids) != 1 {
			t.Fatalf("a saturated start must still see old rows, pushed %v", ids)
		}
	})
	t.Run("catch_up", func(t *testing.T) {
		h := newHarness(t, false, nil)
		h.st.wm = 5_000
		h.c.set(placement.RouteWatermarkKey, watermarkFor(t, testGen, 5_000))
		h.mustTick(metrics.RouteReconcileTickOK)
		if h.started() != phaseCatchUp {
			t.Fatalf("task = %s, want catch_up", h.started())
		}
		if first := h.firstUpdatedScan(0); first == nil || first.after.UpdatedAtMs != 0 {
			t.Fatalf("catch-up scan starts at %+v, want 0 (cvm 0 - lookback saturated)", first)
		}
	})
}

// TestIdentityFlipDuringPrewarmDoesNotArm:预热途中身份变了(结束时复读身份不同)→ 不产出挂标记候选、不推进 cvm,下一拍重新预热。
func TestIdentityFlipDuringPrewarmDoesNotArm(t *testing.T) {
	h := newHarness(t, true, nil)
	h.st.setRow(homeRow(7, 3, 1, 900_000))
	h.c.pushHook = func(c *fakeCache, _ []store.RouteRow) { c.id = idB }
	h.mustTick(metrics.RouteReconcileTickOK)
	h.c.pushHook = nil
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.c.arms != 0 || h.readyPresent() {
		t.Fatalf("an identity flip during the prewarm must not arm (arms=%d)", h.c.arms)
	}
	if req := h.lastAdvance(); req.SetCacheVerifiedMs != nil {
		t.Fatalf("an untrusted prewarm must not advance cache_verified_ms, got %d", *req.SetCacheVerifiedMs)
	}
	if h.started() != phasePrewarm {
		t.Fatalf("the next tick must prewarm again on the new instance, got %s", h.started())
	}
}

// TestRepairOnlyWhenMarkerPresentAndIdentityDiffers(§13.1 第 1 条):回看修补只用于「标记在、身份不符」;标记在且身份相符只做增量。
func TestRepairOnlyWhenMarkerPresentAndIdentityDiffers(t *testing.T) {
	h := newHarness(t, true, nil)
	h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 500_000))
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.started() != phaseIncremental || h.obs.event(metrics.RouteReconcileEventMarkerDroppedIdentity) != 0 {
		t.Fatalf("matching marker: task=%s dropped=%d, want an incremental and no drop", h.started(), h.obs.event(metrics.RouteReconcileEventMarkerDroppedIdentity))
	}

	h2 := newHarness(t, true, nil)
	h2.c.setIdentity(idB)
	h2.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 500_000))
	h2.mustTick(metrics.RouteReconcileTickOK)
	if h2.started() != phaseRepair {
		t.Fatalf("mismatching marker: task=%s, want repair", h2.started())
	}
}

// TestMarkerAbsentAlwaysPrewarms(§13.1 第 1 条):标记不在一律全量预热 —— VI 非空、身份没变(人工 DEL)或身份也变了(空实例重启),
// 都不做回看修补。
func TestMarkerAbsentAlwaysPrewarms(t *testing.T) {
	for _, flip := range []bool{false, true} {
		t.Run(fmt.Sprintf("identity_changed=%t", flip), func(t *testing.T) {
			h := newHarness(t, true, nil)
			h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 500_000))
			h.mustTick(metrics.RouteReconcileTickOK)
			if h.r.vi == nil {
				t.Fatal("a matching marker must set the verified identity")
			}
			h.c.del(placement.RouteReadyKey)
			if flip {
				h.c.setIdentity(idB)
			}
			h.mustTick(metrics.RouteReconcileTickOK)
			if h.started() != phasePrewarm {
				t.Fatalf("marker absent: task = %s, want prewarm", h.started())
			}
		})
	}
}

// TestStaleArmIsDroppedNextTick:过期 leader 迟到挂上的、带旧实例身份的标记,现任 leader 下一拍按身份判失效撤掉并修补。
func TestStaleArmIsDroppedNextTick(t *testing.T) {
	h := newHarness(t, true, nil)
	h.mustTick(metrics.RouteReconcileTickOK) // 预热
	h.mustTick(metrics.RouteReconcileTickOK) // 挂标记
	if !h.readyPresent() {
		t.Fatal("setup: the marker must be armed")
	}
	stale := markerFor(t, idB, testGen, 999_999)
	h.c.set(placement.RouteReadyKey, stale)
	h.mustTick(metrics.RouteReconcileTickOK)
	if raw, _ := h.c.get(placement.RouteReadyKey); raw == stale {
		t.Fatal("a marker carrying another instance's identity must be dropped on the next tick")
	}
	if h.obs.event(metrics.RouteReconcileEventMarkerDroppedIdentity) != 1 || h.started() != phaseRepair {
		t.Fatalf("dropped=%d task=%s, want one drop and a repair", h.obs.event(metrics.RouteReconcileEventMarkerDroppedIdentity), h.started())
	}
}

// TestMarkerGenerationMismatchFreezes:标记世代 ≠ 库世代(库被重建 / 换库)→ 冻结映射写者;身份相符的标记不动(解冻流程对齐世代)。
func TestMarkerGenerationMismatchFreezes(t *testing.T) {
	h := newHarness(t, true, nil)
	h.c.set(placement.RouteReadyKey, markerFor(t, idA, otherGen, 500_000))
	h.mustTick(metrics.RouteReconcileTickDBError)
	h.assertDanger(store.RouteFreezeGenerationMismatch)
	if len(h.st.freezes) != 1 || h.st.freezes[0] != store.RouteFreezeGenerationMismatch || h.obs.event(metrics.RouteReconcileEventFreezeGeneration) != 0 {
		t.Fatalf("freezes=%v events=%v", h.st.freezes, h.obs.events)
	}
	if !h.readyPresent() {
		t.Fatal("a frozen leader keeps a marker whose identity still matches")
	}
}

// ── 回档检测与冻结 ────────────────────────────────────────────────────────────

// TestWatermarkRegressionFreezesAndKeepsMarker(M3):水位键水位 > 库水位 → 冻结;标记不动、不推送。
func TestWatermarkRegressionFreezesAndKeepsMarker(t *testing.T) {
	h := newHarness(t, true, nil)
	h.st.wm = 1_000_000
	h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 900_000))
	h.c.set(placement.RouteWatermarkKey, watermarkFor(t, testGen, 2_000_000))
	h.mustTick(metrics.RouteReconcileTickDBError)
	h.assertDanger(store.RouteFreezeWatermarkRegressed)
	if !h.st.writable || h.obs.event(metrics.RouteReconcileEventFreezeWatermark) != 0 {
		t.Fatalf("writable=%t events=%v, want a watermark freeze", h.st.writable, h.obs.events)
	}
	if req := h.lastAdvance(); req.WatermarkKey == nil || req.WatermarkKey.WatermarkMs != 2_000_000 {
		t.Fatalf("AdvanceReconcile must receive the watermark key, got %+v", req.WatermarkKey)
	}
	if !h.readyPresent() || len(h.c.pushes) != 0 {
		t.Fatalf("frozen: marker present=%t pushes=%d, want the marker kept and nothing pushed", h.readyPresent(), len(h.c.pushes))
	}
	if wk, _ := h.c.get(placement.RouteWatermarkKey); wk != watermarkFor(t, testGen, 2_000_000) {
		t.Fatalf("the watermark key must not be advanced while frozen, got %q", wk)
	}
}

// TestWatermarkKeyAdvancedMonotonicallyAfterCommit(M3):提交后 SET NX / CAS 单调推进;版本更新的水位键不碰、也不交给库判定。
func TestWatermarkKeyAdvancedMonotonicallyAfterCommit(t *testing.T) {
	h := newHarness(t, false, nil)
	h.mustTick(metrics.RouteReconcileTickOK)
	if wk, _ := h.c.get(placement.RouteWatermarkKey); wk != watermarkFor(t, testGen, 1_000_000) {
		t.Fatalf("first tick must SET NX the watermark key, got %q", wk)
	}
	h.st.addNow(5_000)
	h.mustTick(metrics.RouteReconcileTickOK)
	if wk, _ := h.c.get(placement.RouteWatermarkKey); wk != watermarkFor(t, testGen, 1_005_000) {
		t.Fatalf("the watermark key must follow the committed watermark, got %q", wk)
	}
	newer := `{"v":2,"generation":"x"}`
	h.c.set(placement.RouteWatermarkKey, newer)
	h.st.addNow(5_000)
	h.mustTick(metrics.RouteReconcileTickOK)
	if req := h.lastAdvance(); req.WatermarkKey != nil {
		t.Fatalf("a newer-format watermark key must not be judged, got %+v", req.WatermarkKey)
	}
	if wk, _ := h.c.get(placement.RouteWatermarkKey); wk != newer {
		t.Fatalf("a newer-format watermark key must be left untouched, got %q", wk)
	}
	if n := h.c.count("DeleteWatermarkWrongType"); n != 0 {
		t.Fatalf("正常和新版格式的水位不应进入错误类型删除分支，调用次数=%d", n)
	}
}

func TestWatermarkWrongTypeIsDeletedAndRebuiltByReconciler(t *testing.T) {
	h := newHarness(t, false, nil)
	h.c.watermarkWrongType = true
	h.c.set(placement.RouteWatermarkKey, "错误类型占位")
	h.mustTick(metrics.RouteReconcileTickOK)
	if n := h.c.count("DeleteWatermarkWrongType"); n != 1 {
		t.Fatalf("错误类型水位应由 Reconciler 删除一次，调用次数=%d", n)
	}
	ctl, err := h.c.ReadControl(h.ctx)
	want := watermarkFor(t, testGen, 1_000_000)
	if err != nil || ctl.WatermarkWrongType || ctl.Watermark == nil || *ctl.Watermark != want {
		t.Fatalf("错误类型删除后应从已提交库水位重建：control=%+v err=%v", ctl, err)
	}
	if req := h.lastAdvance(); req.WatermarkKey != nil {
		t.Fatalf("错误类型的水位不得作为回档判定证据：%+v", req.WatermarkKey)
	}
	h.st.addNow(5_000)
	h.mustTick(metrics.RouteReconcileTickOK)
	if n := h.c.count("DeleteWatermarkWrongType"); n != 1 {
		t.Fatalf("重建后的正常水位不得重复删除，调用次数=%d", n)
	}
}

// TestFrozenStopsPushesButDropsOnIdentityChange:冻结期不推送、不起任务;标记身份不符时撤掉,相符时保留。
func TestFrozenStopsPushesButDropsOnIdentityChange(t *testing.T) {
	for _, flip := range []bool{false, true} {
		t.Run(fmt.Sprintf("identity_changed=%t", flip), func(t *testing.T) {
			h := newHarness(t, true, nil)
			h.st.writable = false
			h.st.setRow(homeRow(7, 3, 1, 900_000))
			h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 500_000))
			if flip {
				h.c.setIdentity(idB)
			}
			h.mustTick(metrics.RouteReconcileTickFrozen)
			h.ticks(2, time.Second)
			if len(h.c.pushes) != 0 || h.r.running != nil || h.started() != 0 {
				t.Fatalf("frozen: pushes=%d running=%v", len(h.c.pushes), h.r.running)
			}
			if h.readyPresent() == flip {
				t.Fatalf("identity_changed=%t: marker present=%t", flip, h.readyPresent())
			}
		})
	}
}

// ── 租约 ──────────────────────────────────────────────────────────────────────

// TestLeaseExpiryMidPrewarmAbortsBeforeNextBatch:本地租约在两批之间过期 → 下一批之前中止;下一拍降级。
func TestLeaseExpiryMidPrewarmAbortsBeforeNextBatch(t *testing.T) {
	h := newHarness(t, true, func(rc *config.RouteReconcileConfig) { rc.PushBatch = 2 })
	for id := uint64(1); id <= 5; id++ {
		h.st.setRow(homeRow(id, 3, 1, 900_000))
	}
	h.c.pushHook = func(*fakeCache, []store.RouteRow) { h.clk.advance(9 * time.Second) }
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.scanCount() != 1 || len(h.c.pushes) != 1 {
		t.Fatalf("scans=%d pushes=%d, want the prewarm to stop before its second batch", h.scanCount(), len(h.c.pushes))
	}
	h.c.pushHook = nil
	h.mustTick(metrics.RouteReconcileTickLeaseLost)
	if h.obs.event(metrics.RouteReconcileEventTaskAborted) != 1 || h.obs.isLeader() || h.c.arms != 0 {
		t.Fatalf("aborted=%d leader=%t arms=%d", h.obs.event(metrics.RouteReconcileEventTaskAborted), h.obs.isLeader(), h.c.arms)
	}
}

// TestSustainedRedisErrorsReleaseLease(§4.2 should):连续读不到 mapping Redis 超过 RedisFailureReleaseMs → 让出租约并降级,
// 之后同样长的时间里不再抢(连 Peek 都不发)。
func TestSustainedRedisErrorsReleaseLease(t *testing.T) {
	h := newHarness(t, true, nil)
	h.mustTick(metrics.RouteReconcileTickOK)
	h.c.mu.Lock()
	h.c.probeEr = errors.New("connection refused")
	h.c.mu.Unlock()
	res := h.ticks(10, time.Second)
	for _, r := range res {
		if r != metrics.RouteReconcileTickRedisError {
			t.Fatalf("ticks while redis is down = %v", res)
		}
	}
	if h.st.releases != 0 {
		t.Fatal("the lease must be kept before RedisFailureReleaseMs elapses")
	}
	h.ticks(1, time.Second)
	if h.st.releases != 1 || h.st.holder != "" || h.obs.event(metrics.RouteReconcileEventLeaseReleasedRedisUnreachable) != 1 || h.obs.isLeader() {
		t.Fatalf("releases=%d holder=%q events=%v leader=%t", h.st.releases, h.st.holder, h.obs.events, h.obs.isLeader())
	}
	peeks := h.st.peeks
	h.ticks(1, time.Second)
	if h.st.peeks != peeks {
		t.Fatal("during the hold-off the replica must not try to re-acquire")
	}
}

// TestSelfCheckExportedOnlyByLeader(§13.1 第 8 条):leader 每拍用读方脚本自检并导出;掉主时删掉观测型 gauge。
func TestSelfCheckExportedOnlyByLeader(t *testing.T) {
	h := newHarness(t, true, nil)
	h.mustTick(metrics.RouteReconcileTickOK)
	h.mustTick(metrics.RouteReconcileTickOK)
	obs := h.obs.observations[len(h.obs.observations)-1]
	if obs.verdict != placement.RouteReadyTrusted.String() || !obs.marker || !obs.writable {
		t.Fatalf("after arming the self-check must be trusted, got %+v", obs)
	}
	h.clk.advance(3 * time.Second)
	h.st.mu.Lock()
	h.st.holder, h.st.leaseUntil = "route_leader@other:22222222", h.st.nowMs+10_000
	h.st.mu.Unlock()
	h.mustTick(metrics.RouteReconcileTickLeaseLost)
	if h.obs.cleared != 1 || h.obs.isLeader() {
		t.Fatalf("losing the lease must clear the leader observations (cleared=%d)", h.obs.cleared)
	}
}

// ── 挂标记的前置与闩锁 ────────────────────────────────────────────────────────

// TestLegacyConflictOrFencedBlocksArming:预热有未决结局(遗留值冲突 / 围栏)→ 不挂标记,进 armingBlocked。
func TestLegacyConflictOrFencedBlocksArming(t *testing.T) {
	for _, kind := range []string{"legacy_conflict", "fenced"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, true, nil)
			h.st.setRow(homeRow(7, 3, 1, 900_000))
			if kind == "fenced" {
				h.c.forced[7] = PushFenced
			} else {
				h.c.set(placement.HomeZoneKey(7), "9")
			}
			h.ticks(3, 0)
			if h.c.arms != 0 || h.readyPresent() {
				t.Fatalf("an unresolved row must block arming (arms=%d)", h.c.arms)
			}
			if h.obs.event(metrics.RouteReconcileEventArmingRefusedUnresolved) != 1 || h.r.latch == nil {
				t.Fatalf("refused=%d latch=%v", h.obs.event(metrics.RouteReconcileEventArmingRefusedUnresolved), h.r.latch)
			}
		})
	}
}

// TestArmingRefusedDoesNotRestartFullPrewarm(§4.2 should):被拒之后不再重跑全量预热,只做增量并复读未决行;未决清空后
// 用闩住的那份预热结果挂标记。
func TestArmingRefusedDoesNotRestartFullPrewarm(t *testing.T) {
	h := newHarness(t, true, func(rc *config.RouteReconcileConfig) {
		rc.IncrementalIntervalMs = 1000
	})
	h.st.setRow(homeRow(7, 3, 1, 900_000))
	h.c.set(placement.HomeZoneKey(7), "9")
	prewarms := 0
	count := func() {
		if h.started() == phasePrewarm {
			prewarms++
		}
	}
	h.mustTick(metrics.RouteReconcileTickOK)
	count()
	for i := 0; i < 4; i++ {
		h.ticks(1, time.Second)
		count()
	}
	if h.c.arms != 0 || prewarms != 1 {
		t.Fatalf("blocked: arms=%d prewarms=%d, want 0 / 1", h.c.arms, prewarms)
	}
	h.c.del(placement.HomeZoneKey(7)) // 运维修掉了遗留值
	for i := 0; i < 3; i++ {
		h.ticks(1, time.Second)
		count()
	}
	if prewarms != 1 || !h.readyPresent() || h.c.arms != 1 || h.obs.event(metrics.RouteReconcileEventArmingRefusedUnresolved) != 1 {
		t.Fatalf("after the fix: prewarms=%d marker=%t arms=%d refused=%d, want 1 / true / 1 / 1",
			prewarms, h.readyPresent(), h.c.arms, h.obs.event(metrics.RouteReconcileEventArmingRefusedUnresolved))
	}
}

// TestWipedControlKeysResetLatch(§13.1 第 6 条):rewarm 一条 DEL 删掉三把控制键 → 闩锁解除、重取淘汰基线、全量预热。
func TestWipedControlKeysResetLatch(t *testing.T) {
	h := newHarness(t, true, nil)
	h.st.setRow(homeRow(7, 3, 1, 900_000))
	h.c.set(placement.HomeZoneKey(7), "9")
	h.ticks(2, 0)
	if h.r.latch == nil {
		t.Fatal("setup: arming must be blocked")
	}
	h.c.del(placement.RouteReadyKey, placement.RouteWarmKey, placement.RouteWatermarkKey)
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.started() != phasePrewarm {
		t.Fatalf("after rewarm the leader must prewarm again, got %s", h.started())
	}
}

// TestEvictionPolicyOrEvictedKeysRefuseArmingAndDrop:淘汰策略不是 noeviction、或 evicted_keys 比基线大 → 撤标记、拒绝挂标记,
// 不白跑全量预热。
func TestEvictionPolicyOrEvictedKeysRefuseArmingAndDrop(t *testing.T) {
	t.Run("policy", func(t *testing.T) {
		h := newHarness(t, true, nil)
		h.c.policy = "allkeys-lru"
		h.ticks(3, time.Second)
		if h.c.arms != 0 || h.obs.event(metrics.RouteReconcileEventArmingRefusedEviction) != 1 {
			t.Fatalf("arms=%d refused=%d", h.c.arms, h.obs.event(metrics.RouteReconcileEventArmingRefusedEviction))
		}
		if h.r.latch == nil || h.c.count("SetWarmToken") != 0 {
			t.Fatal("no full prewarm may start while eviction is possible")
		}
	})
	t.Run("evicted_keys", func(t *testing.T) {
		h := newHarness(t, true, nil)
		h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 500_000))
		h.mustTick(metrics.RouteReconcileTickOK)
		h.c.mu.Lock()
		h.c.evicted = 3
		h.c.mu.Unlock()
		h.mustTick(metrics.RouteReconcileTickOK)
		if h.readyPresent() || h.obs.event(metrics.RouteReconcileEventMarkerDroppedEviction) != 1 {
			t.Fatalf("an eviction must drop the marker (present=%t)", h.readyPresent())
		}
		if h.obs.event(metrics.RouteReconcileEventArmingRefusedEviction) != 1 || h.c.arms != 0 {
			t.Fatalf("refused=%d arms=%d", h.obs.event(metrics.RouteReconcileEventArmingRefusedEviction), h.c.arms)
		}
	})
}

// TestDebugCommandEnabledRefusesArming(§13.1 第 5 条):enable-debug-command 不是 no → 撤现存标记、拒绝挂标记;改回 no(重启)才预热。
func TestDebugCommandEnabledRefusesArming(t *testing.T) {
	h := newHarness(t, true, nil)
	h.c.debug = "yes"
	h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 500_000))
	h.ticks(3, time.Second)
	if h.readyPresent() || h.c.arms != 0 || h.c.count("SetWarmToken") != 0 {
		t.Fatalf("debug enabled: marker=%t arms=%d tokens=%d", h.readyPresent(), h.c.arms, h.c.count("SetWarmToken"))
	}
	if h.obs.event(metrics.RouteReconcileEventArmingRefusedDebugEnabled) != 1 {
		t.Fatalf("arming_refused_debug_enabled = %d, want 1 (counted on transition)", h.obs.event(metrics.RouteReconcileEventArmingRefusedDebugEnabled))
	}
	h.c.mu.Lock()
	h.c.debug = "no"
	h.c.mu.Unlock()
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.started() != phasePrewarm {
		t.Fatalf("debug disabled: task = %s, want prewarm", h.started())
	}
}

// TestArmRefusedOnWarmTokenMismatchFallsBackToPrewarm(M2):预热完成后令牌被换(FLUSH / rewarm / 新 leader)→ 挂标记脚本回 0,
// VI 清空,下一拍全量预热。
func TestArmRefusedOnWarmTokenMismatchFallsBackToPrewarm(t *testing.T) {
	h := newHarness(t, true, nil)
	h.mustTick(metrics.RouteReconcileTickOK)
	h.c.set(placement.RouteWarmKey, otherToken)
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.c.arms != 1 || h.readyPresent() || h.r.vi != nil {
		t.Fatalf("arms=%d marker=%t vi=%v, want one refused attempt", h.c.arms, h.readyPresent(), h.r.vi)
	}
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.started() != phasePrewarm {
		t.Fatalf("after a token mismatch the next tick must prewarm, got %s", h.started())
	}
}

// TestDisarmDropsMarker(M4):Arm=false 时每拍按原值撤掉现存标记(撤销信任)。
func TestDisarmDropsMarker(t *testing.T) {
	h := newHarness(t, false, nil)
	h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 500_000))
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.readyPresent() || h.obs.event(metrics.RouteReconcileEventMarkerDroppedDisarmed) != 1 {
		t.Fatalf("Arm=false must drop the marker (present=%t events=%v)", h.readyPresent(), h.obs.events)
	}
}

// ── 围栏镜像 ──────────────────────────────────────────────────────────────────

// TestFenceMirrorFollowsDB(取代 NeverTouchesMergeFenceKeys,§13.1 第 3 条):库里生效的围栏补镜像(过期 = 库 expires_at + 10 分钟),
// 库里释放之后复读确认再按完整值删;别的键一概不碰。
func TestFenceMirrorFollowsDB(t *testing.T) {
	h := newHarness(t, false, nil)
	holder := holderJSON("run1", 3)
	h.st.fences = []store.ActiveMergeFence{{ZoneID: 5, RunID: "run1", ExpiresAtMs: 2_000_000, HolderJSON: holder, FenceEpoch: 3}}
	h.mustTick(metrics.RouteReconcileTickOK)
	key := placement.MergeFenceKey(5)
	if v, _ := h.c.get(key); v != holder || h.c.pexpire[key] != 2_000_000+placement.MergeFenceMirrorMarginMs {
		t.Fatalf("mirror %q pexpireat %d", v, h.c.pexpire[key])
	}
	h.st.mu.Lock()
	h.st.fences = nil
	h.st.mu.Unlock()
	h.mustTick(metrics.RouteReconcileTickOK)
	if _, ok := h.c.get(key); ok || h.obs.mirrors[metrics.RouteReconcileMirrorFenceDeleted] != 1 {
		t.Fatalf("a released fence's mirror must be deleted (mirrors %v)", h.obs.mirrors)
	}
	for k := range h.c.kv {
		if strings.HasPrefix(k, fenceMirrorKeyPrefix) {
			t.Fatalf("unexpected fence mirror key left: %s", k)
		}
	}
}

// TestStaleFenceMirrorDeletedOnlyAfterDBRecheck:第一次读库不生效、复读库时已生效(两次读之间刚获取)→ 不删;
// 下一拍按库值(更大的 epoch)覆写;释放之后两次读都不生效才删。
func TestStaleFenceMirrorDeletedOnlyAfterDBRecheck(t *testing.T) {
	h := newHarness(t, false, nil)
	key := placement.MergeFenceKey(5)
	old, newer := holderJSON("run1", 3), holderJSON("run2", 4)
	h.c.set(key, old)
	active := []store.ActiveMergeFence{{ZoneID: 5, RunID: "run2", ExpiresAtMs: 3_000_000, HolderJSON: newer, FenceEpoch: 4}}
	h.st.fenceListHook = func(n int) []store.ActiveMergeFence {
		switch n {
		case 1:
			return nil // 第一拍第一次读:还没获取
		case 2, 3:
			return active // 第一拍复读、第二拍:已获取
		default:
			return nil // 之后:已释放
		}
	}
	h.mustTick(metrics.RouteReconcileTickOK)
	if v, _ := h.c.get(key); v != old || h.obs.mirrors[metrics.RouteReconcileMirrorFenceDeleted] != 0 {
		t.Fatalf("a mirror whose fence became active between the two reads must not be deleted (value %q)", v)
	}
	h.mustTick(metrics.RouteReconcileTickOK)
	if v, _ := h.c.get(key); v != newer {
		t.Fatalf("the active fence must overwrite the older-epoch mirror, got %q", v)
	}
	h.mustTick(metrics.RouteReconcileTickOK)
	if _, ok := h.c.get(key); ok {
		t.Fatal("after release the mirror must be deleted")
	}
}

// TestForeignFenceBlocksArmingAndIsNotDeleted(§13.1 第 3 条):解不出 fence_epoch 的镜像(旧版 merge_zone 写的)不删、计 foreign_fence、
// 阻止挂标记;它消失之后才预热、挂标记。
func TestForeignFenceBlocksArmingAndIsNotDeleted(t *testing.T) {
	h := newHarness(t, true, nil)
	key := placement.MergeFenceKey(5)
	h.c.set(key, `{"run_id":"legacy"}`)
	h.ticks(2, time.Second)
	if v, ok := h.c.get(key); !ok || v != `{"run_id":"legacy"}` {
		t.Fatal("a foreign fence mirror must never be deleted")
	}
	if h.obs.event(metrics.RouteReconcileEventForeignFence) < 2 || h.obs.event(metrics.RouteReconcileEventArmingRefusedUnresolved) != 1 {
		t.Fatalf("events %v", h.obs.events)
	}
	if h.c.arms != 0 || h.c.count("SetWarmToken") != 0 {
		t.Fatal("no prewarm / arming while a foreign fence mirror exists")
	}
	h.c.del(key)
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.started() != phasePrewarm {
		t.Fatalf("after the foreign mirror is gone: task = %s, want prewarm", h.started())
	}
	h.mustTick(metrics.RouteReconcileTickOK)
	if !h.readyPresent() {
		t.Fatal("the marker must be armed after the prewarm")
	}
}

// ── 追平、增量与 cvm(VERDICT must-1 / must-2)──────────────────────────────────

// TestCatchUpCompletesSetsVerifiedIdentityAndDoesNotRequeue:Arm=false 下追平完成且首尾身份相同 → 置 VI,不再重排;
// 下一拍把 cvm 写成追平开始时的库时钟。
func TestCatchUpCompletesSetsVerifiedIdentityAndDoesNotRequeue(t *testing.T) {
	h := newHarness(t, false, nil)
	h.st.nowMs = 2_000_000
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.started() != phaseCatchUp {
		t.Fatalf("task = %s, want catch_up", h.started())
	}
	h.st.addNow(1_000)
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.r.vi == nil || *h.r.vi != idA {
		t.Fatal("a completed catch-up must set the verified identity")
	}
	if req := h.lastAdvance(); req.SetCacheVerifiedMs == nil || *req.SetCacheVerifiedMs != 2_000_000 {
		t.Fatalf("cache_verified_ms must be set to the catch-up start, got %+v", req.SetCacheVerifiedMs)
	}
	for i := 0; i < 5; i++ {
		h.ticks(1, time.Second)
		if h.started() == phaseCatchUp {
			t.Fatal("the catch-up must not be re-queued once the identity is verified")
		}
	}
}

// TestFailoverIncrementalStartsFromPersistedCvm:新 leader 的首次增量从库里持久化的 cvm − Overlap 开始。
func TestFailoverIncrementalStartsFromPersistedCvm(t *testing.T) {
	h := newHarness(t, true, nil)
	h.st.cvm, h.st.wm, h.st.nowMs = 5_000_000, 5_000_000, 5_100_000
	h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 5_000_000))
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.started() != phaseIncremental {
		t.Fatalf("task = %s, want incremental", h.started())
	}
	if first := h.firstUpdatedScan(0); first == nil || first.after.UpdatedAtMs != 4_985_000 {
		t.Fatalf("incremental starts at %+v, want 4985000 (persisted cvm 5000000 - 15000)", first)
	}
}

// TestIncrementalAfterPrewarmStartsAtPrewarmStart:长任务(预热)结束后的首次增量从预热开始时的库时钟 − Overlap 开始,
// cvm 写成预热开始时刻,而不是结束时刻。
func TestIncrementalAfterPrewarmStartsAtPrewarmStart(t *testing.T) {
	// 本例只验证长预热后的起点 / cvm 窗口；50 秒超过默认增量间隔，但仍在合法的 60 秒租约内。
	// 租约中途过期由 TestLeaseExpiryMidPrewarmAbortsBeforeNextBatch 单独验证。
	h := newHarness(t, true, func(rc *config.RouteReconcileConfig) { rc.LeaseMs = 60_000 })
	h.st.nowMs = 2_000_000
	h.st.setRow(homeRow(7, 3, 1, 1_000))
	h.c.pushHook = func(*fakeCache, []store.RouteRow) { h.st.addNow(50_000) }
	h.mustTick(metrics.RouteReconcileTickOK)
	h.c.pushHook = nil
	scansBefore := h.scanCount()
	h.mustTick(metrics.RouteReconcileTickOK)
	if req := h.lastAdvance(); req.SetCacheVerifiedMs == nil || *req.SetCacheVerifiedMs != 2_000_000 {
		t.Fatalf("cache_verified_ms after the prewarm = %+v, want its start 2000000", req.SetCacheVerifiedMs)
	}
	if h.started() != phaseIncremental {
		t.Fatalf("task = %s, want incremental", h.started())
	}
	if first := h.firstUpdatedScan(scansBefore); first == nil || first.after.UpdatedAtMs != 1_985_000 {
		t.Fatalf("incremental starts at %+v, want 1985000 (prewarm start - overlap)", first)
	}
}

// TestThrottledIncrementalDoesNotAdvanceCvmPastItsStart:被限速拖长的增量只把 cvm 推进到它开始时的库时钟。
func TestThrottledIncrementalDoesNotAdvanceCvmPastItsStart(t *testing.T) {
	h := newHarness(t, true, func(rc *config.RouteReconcileConfig) {
		rc.PushMaxRowsPerSec = 100
		rc.PushBatch = 50
	})
	h.st.cvm, h.st.wm, h.st.nowMs = 2_995_000, 2_995_000, 3_000_000
	h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 2_995_000))
	for i := uint64(1); i <= 120; i++ {
		h.st.setRow(homeRow(i*64, 3, 1, 2_990_000))
	}
	h.clk.sleepHook = func(d time.Duration) { h.st.addNow(uint64(d.Milliseconds())) }
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.started() != phaseIncremental || len(h.clk.slept) == 0 {
		t.Fatalf("task=%s sleeps=%d, want a throttled incremental", h.started(), len(h.clk.slept))
	}
	h.clk.sleepHook = nil
	if h.st.nowMs <= 3_000_000 {
		t.Fatal("setup: the database clock must have moved during the throttled incremental")
	}
	h.mustTick(metrics.RouteReconcileTickOK)
	if req := h.lastAdvance(); req.SetCacheVerifiedMs == nil || *req.SetCacheVerifiedMs != 3_000_000 {
		t.Fatalf("cache_verified_ms = %+v, want the incremental start 3000000", req.SetCacheVerifiedMs)
	}
}

// TestIncrementalWindowCursorMath:64 个片各从 {cvm − Overlap, 0} 起翻页,下一页从上一页最后一行接着翻;窗口之前的行不推。
func TestIncrementalWindowCursorMath(t *testing.T) {
	h := newHarness(t, true, func(rc *config.RouteReconcileConfig) { rc.PushBatch = 2 })
	h.st.cvm, h.st.wm, h.st.nowMs = 1_000_000, 1_000_000, 1_000_000
	h.c.set(placement.RouteReadyKey, markerFor(t, idA, testGen, 1_000_000))
	h.st.setRow(homeRow(3, 3, 1, 984_999))     // 窗口之前
	h.st.setRow(homeRow(3+64, 3, 1, 985_000))  // 恰在起点
	h.st.setRow(homeRow(3+128, 3, 1, 990_000)) // 窗口内
	h.st.setRow(homeRow(3+192, 3, 1, 999_000)) // 窗口内,第二页
	h.mustTick(metrics.RouteReconcileTickOK)
	if h.started() != phaseIncremental {
		t.Fatalf("task = %s, want incremental", h.started())
	}
	h.st.mu.Lock()
	scans := append([]scanCall(nil), h.st.scans...)
	h.st.mu.Unlock()
	firsts := map[uint32]store.RouteUpdatedCursor{}
	var shard3 []store.RouteUpdatedCursor
	for _, s := range scans {
		if !s.updated {
			continue
		}
		if _, ok := firsts[s.shard]; !ok {
			firsts[s.shard] = s.after
		}
		if s.shard == 3 {
			shard3 = append(shard3, s.after)
		}
	}
	if len(firsts) != int(placement.RouteRowShards) {
		t.Fatalf("scanned %d shards, want %d", len(firsts), placement.RouteRowShards)
	}
	for shard, c := range firsts {
		if c.UpdatedAtMs != 985_000 || c.PlayerID != 0 {
			t.Fatalf("shard %d starts at %+v, want {985000, 0}", shard, c)
		}
	}
	want := []store.RouteUpdatedCursor{{UpdatedAtMs: 985_000}, {UpdatedAtMs: 990_000, PlayerID: 3 + 128}}
	if len(shard3) != len(want) || shard3[0] != want[0] || shard3[1] != want[1] {
		t.Fatalf("shard 3 pages %+v, want %+v", shard3, want)
	}
	ids := h.pushedIDs()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if fmt.Sprint(ids) != fmt.Sprint([]uint64{3 + 64, 3 + 128, 3 + 192}) {
		t.Fatalf("pushed %v, want the three rows inside the window", ids)
	}
}

// ── 关停 ──────────────────────────────────────────────────────────────────────

// newAsyncHarness 是关停用例的装配:任务跑在真 goroutine 里,Sleep 阻塞到 ctx 结束(每次 Run 只跑一拍)。
func newAsyncHarness(t *testing.T) *harness {
	h := newHarness(t, true, nil)
	h.r.syncTasks = false
	h.clk.blockSleep = true
	return h
}

// TestShutdownReleasesLease:Start 起循环,stop 取消之后 Run 退出:释放租约、关客户端、导出 leader=0 并删观测型 gauge。
func TestShutdownReleasesLease(t *testing.T) {
	h := newAsyncHarness(t)
	h.r.syncTasks = true // 首拍任务确定完成后再取消普通生命周期。
	stop := h.r.Start(context.Background())
	<-h.clk.sleeping // 第一拍跑完(当选并起了预热)
	if !stop(time.Second) {
		t.Fatal("stop must report that Run exited")
	}
	if h.st.releases != 1 || h.st.holder != "" {
		t.Fatalf("releases=%d holder=%q, want the lease released", h.st.releases, h.st.holder)
	}
	if !h.c.closed || h.obs.isLeader() || h.obs.cleared == 0 {
		t.Fatalf("closed=%t leader=%t cleared=%d", h.c.closed, h.obs.isLeader(), h.obs.cleared)
	}
}

// TestShutdownBoundedAndReleasesLeaseBestEffort：调用者有界返回，后台 owner 必须等实际任务 join 后才释放资源。
func TestShutdownBoundedAndReleasesLeaseBestEffort(t *testing.T) {
	h := newAsyncHarness(t)
	h.st.block = make(chan struct{})
	h.st.scanStarted = make(chan struct{}, 1)
	h.st.releaseErr = errors.New("database unreachable")
	var unblock sync.Once
	defer unblock.Do(func() { close(h.st.block) })
	finish := h.r.StartWithReport(context.Background())
	<-h.st.scanStarted
	<-h.clk.sleeping
	wait, cancel := context.WithCancel(context.Background())
	cancel()
	if err := finish(wait); !errors.Is(err, context.Canceled) {
		t.Fatalf("未 join 时应有界返回取消，got %v", err)
	}
	h.st.mu.Lock()
	releases := h.st.releases
	h.st.mu.Unlock()
	h.c.mu.Lock()
	closed := h.c.closed
	h.c.mu.Unlock()
	if releases != 0 || closed {
		t.Fatal("卡住任务仍归 owner；不能提前释放租约或关闭缓存")
	}
	h.clk.wake <- struct{}{}
	unblock.Do(func() { close(h.st.block) })
	joined, stopWait := context.WithTimeout(context.Background(), time.Second)
	defer stopWait()
	if err := finish(joined); err == nil {
		t.Fatal("释放租约失败必须保留，不能宣称排空成功")
	}
	if h.st.releases != 1 || !h.c.closed {
		t.Fatalf("releases=%d closed=%t，join 后仍须尽力释放", h.st.releases, h.c.closed)
	}
	if _, err := h.r.SettledDigest(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("失败必须保留 HOLD，got %v", err)
	}
}

func TestTrackEffectFailureThenSuccessKeepsHold(t *testing.T) {
	effects := drainproof.New()
	failure := errors.New("结果未知")
	calls := 0
	if err := trackEffect(context.Background(), effects, func() error { calls++; return failure }); !errors.Is(err, failure) {
		t.Fatalf("got %v", err)
	}
	if err := trackEffect(context.Background(), effects, func() error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	effects.CloseAdmission()
	if calls != 2 || effects.Pending() != 0 {
		t.Fatalf("calls=%d pending=%d", calls, effects.Pending())
	}
	if err := effects.Wait(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("后续成功不得清除历史 UNKNOWN，got %v", err)
	}
	if _, err := effects.SettledDigest(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("不得签发 settled 摘要，got %v", err)
	}
}

func TestTrackEffectPreCancelledDoesNotDispatch(t *testing.T) {
	effects := drainproof.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := trackEffect(ctx, effects, func() error { called = true; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	effects.CloseAdmission()
	if called || effects.Pending() != 0 {
		t.Fatal("预取消调用不得进入底层副作用")
	}
	if err := effects.Wait(context.Background()); err != nil {
		t.Fatalf("未派发不能伪造未知结果：%v", err)
	}
}

func TestStopWithReportDoesNotCancelAcceptedTask(t *testing.T) {
	h := newAsyncHarness(t)
	h.st.block = make(chan struct{})
	h.st.scanStarted = make(chan struct{}, 1)
	var unblock sync.Once
	defer unblock.Do(func() { close(h.st.block) })
	finish := h.r.StartWithReport(context.Background())
	<-h.st.scanStarted
	<-h.clk.sleeping
	wait, cancel := context.WithCancel(context.Background())
	cancel()
	if err := finish(wait); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	h.st.mu.Lock()
	acceptedContext := h.st.scanContext
	h.st.mu.Unlock()
	if acceptedContext == nil || acceptedContext.Err() != nil {
		t.Fatal("正式 stop 只能关闭新拍，不能取消已接收的任务")
	}
	h.clk.wake <- struct{}{}
	unblock.Do(func() { close(h.st.block) })
	joined, stopWait := context.WithTimeout(context.Background(), time.Second)
	defer stopWait()
	if err := finish(joined); err != nil {
		t.Fatalf("既有任务完成后应确定 join：%v", err)
	}
	if digest, err := h.r.SettledDigest(context.Background()); err != nil || len(digest) != 32 {
		t.Fatalf("确定排空后才产生摘要：len=%d err=%v", len(digest), err)
	}
	if h.st.releases != 1 || !h.c.closed {
		t.Fatal("必须在 join 后释放租约和缓存")
	}
}
