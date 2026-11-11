package logic

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"player_locator/internal/svc"
	"shared/redisslot"
)

// 补建 / 审计按段分摊(index_pass.go)。miniredis 不支持 CLUSTER 命令,deps.listSlotKeys 换成从 miniredis 按 slot 挑键;
// leader 等待时的 sleep 换成「让 worker 走一步」,整遍在一个 goroutine 里确定地跑完,不依赖墙钟。

// countingSlotKeys 是单测的 slot 列键:从 miniredis 里挑出属于这个 slot 的键,并记下列过哪些 slot。fail 里的 slot 返回错误。
type countingSlotKeys struct {
	mr     *miniredis.Miniredis
	mu     sync.Mutex
	listed map[uint16]int
	fail   map[uint16]error
}

func newCountingSlotKeys(mr *miniredis.Miniredis) *countingSlotKeys {
	return &countingSlotKeys{mr: mr, listed: map[uint16]int{}, fail: map[uint16]error{}}
}

func (c *countingSlotKeys) list(_ context.Context, _ redis.UniversalClient, slot uint16, limit int) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail[slot]; err != nil {
		return nil, err
	}
	c.listed[slot]++
	var keys []string
	for _, key := range c.mr.Keys() {
		if redisslot.HasHashTag(key) && redisslot.HashSlot(key) == slot {
			keys = append(keys, key)
		}
	}
	if len(keys) > limit {
		return nil, errors.New("too many keys")
	}
	return keys, nil
}

func (c *countingSlotKeys) listedSlots() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.listed)
}

// segmentRange 持有 [lo, hi) 这些段。
type segmentRange struct{ lo, hi int }

func (s segmentRange) OwnedSegments() []int {
	segs := make([]int, 0, s.hi-s.lo)
	for c := s.lo; c < s.hi; c++ {
		segs = append(segs, c)
	}
	return segs
}

var allSegments = segmentRange{0, slotSegmentCount}

// newShardedFixture 装配一个按段分摊的 leader 和一个 worker:leader 每次等待时 worker 走一步。
func newShardedFixture(t *testing.T, sc *svc.ServiceContext, view clusterView, clock *testClock, lister *countingSlotKeys, owner segmentOwner) (*sessionReconciler, *indexPassWorker) {
	t.Helper()
	workerDeps := testReconcilerDeps(view, clock)
	workerDeps.listSlotKeys = lister.list
	worker := newIndexPassWorker(newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, workerDeps), owner, lister.list)
	leaderDeps := testReconcilerDeps(view, clock)
	leaderDeps.listSlotKeys = lister.list
	leaderDeps.sleep = func(ctx context.Context, _ time.Duration) error {
		worker.step(ctx)
		return ctx.Err()
	}
	return newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, leaderDeps), worker
}

// 与单机补建同一个场景(TestRepairPass_BackfillsLegacySessionAndSetsReady):分摊之后结果一样,而且 16384 个 slot 都列过一次,
// 结束时这一遍的描述和进度都删掉。
func TestShardedPass_BackfillSetsReadyLikeLocalPass(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	writeRawSession(t, mr, indexTestSession(9901, 100, idxGateA, 1))
	lister := newCountingSlotKeys(mr)
	r, _ := newShardedFixture(t, sc, newFakeClusterView(), newTestClock(), lister, allSegments)

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileLoopRepair, step.loop)
	assert.Equal(t, reconcileResultOK, step.result)
	assert.Equal(t, 1, step.report.repaired)
	assert.Equal(t, []string{"9901"}, indexMembers(t, mr, idxGateA))
	assert.Contains(t, registryMembers(t, mr), idxGateA)
	assert.True(t, mr.Exists(gateIndexReadyKey), "全部段做完、代次没变才设就绪标记")
	assert.True(t, mr.Exists(gateIndexLastAuditKey))
	assert.Equal(t, redisslot.SlotCount, lister.listedSlots(), "每个 slot 都列过")
	assert.False(t, mr.Exists(gateIndexPassKey), "做完的一遍要结束")
	assert.False(t, mr.Exists(gateIndexPassProgressKey))
}

// 审计同样分摊:清存活 gate 上的脏成员,记审计时刻(TestAuditPass_PrunesDirtyMembersOnLiveGates 的分摊版)。
func TestShardedPass_AuditPrunesAndRecordsLastAudit(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9950, 100, idxGateA, 1)))
	seedIndexMember(t, mr, idxGateA, 9952) // 会话不存在的脏成员
	setReadyMarker(t, mr, 100*time.Second)
	clock := newTestClock()
	r, _ := newShardedFixture(t, sc, newFakeClusterView(idxGateA), clock, newCountingSlotKeys(mr), allSegments)

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileLoopAudit, step.loop)
	assert.Equal(t, reconcileResultOK, step.result)
	assert.Equal(t, []string{"9950"}, indexMembers(t, mr, idxGateA))
	last, _ := mr.Get(gateIndexLastAuditKey)
	assert.Equal(t, strconv.FormatInt(clock.Now().Unix(), 10), last)
}

// 换主:上一任发布的一遍做到一半就卸任,下一任种类与代次都没变,接着这一遍做,已经做完的段不再重扫。
func TestShardedPass_NextLeaderResumesFromProgress(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	writeRawSession(t, mr, indexTestSession(9901, 100, idxGateA, 1))
	view := newFakeClusterView()
	clock := newTestClock()

	firstLister := newCountingSlotKeys(mr)
	first, firstWorker := newShardedFixture(t, sc, view, clock, firstLister, segmentRange{0, 10})
	ctx, cancel := context.WithCancel(context.Background())
	first.deps.sleep = func(ctx context.Context, _ time.Duration) error {
		firstWorker.step(ctx)
		cancel() // 任期在第一次等待里结束
		return ctx.Err()
	}
	step := first.repairStep(ctx)
	assert.Equal(t, "", step.result, "被取消的一遍不计数")
	require.True(t, mr.Exists(gateIndexPassKey), "没做完的一遍留给下一任")
	assert.Equal(t, 10*slotsPerSegment, firstLister.listedSlots())

	nextLister := newCountingSlotKeys(mr)
	next, _ := newShardedFixture(t, sc, view, clock, nextLister, allSegments)
	step = next.repairStep(context.Background())

	assert.Equal(t, reconcileResultOK, step.result)
	assert.Equal(t, redisslot.SlotCount-10*slotsPerSegment, nextLister.listedSlots(), "前 10 段已经做完,不再重扫")
	assert.True(t, mr.Exists(gateIndexReadyKey))
}

// 发布一遍:种类与代次都一样就接着用;代次变了(期间看到过旧写者)或种类变了,换新编号、清空进度。
func TestBeginPassScript_ReusesOnlySameKindAndGen(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	begin := func(kind passKind, gen string) indexPass {
		raw, err := beginPassScript.Run(ctx, sc.RedisClient, []string{gateIndexPassKey, gateIndexPassSeqKey, gateIndexPassProgressKey},
			string(kind), gen).Text()
		require.NoError(t, err)
		pass, ok := parseIndexPass(raw)
		require.True(t, ok, raw)
		return pass
	}

	first := begin(passKindBackfill, "")
	mr.HSet(gateIndexPassProgressKey, "3", passProgress{passID: first.id, next: 200}.value())
	assert.Equal(t, first, begin(passKindBackfill, ""), "同种类同代次:接着做")
	assert.True(t, mr.Exists(gateIndexPassProgressKey))

	second := begin(passKindBackfill, "1")
	assert.NotEqual(t, first.id, second.id, "代次变了换新一遍")
	assert.False(t, mr.Exists(gateIndexPassProgressKey), "进度清空")

	third := begin(passKindAudit, "1")
	assert.NotEqual(t, second.id, third.id, "种类变了换新一遍")
}

// 进度只能从读到的那一份往前推:旧主人写不回已被新主人推进过的进度,也写不进已经结束的一遍。
func TestPassAdvanceScript_ComparesBeforeWrite(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	pass := indexPass{id: "7", kind: passKindAudit, gen: "2"}
	require.NoError(t, mr.Set(gateIndexPassKey, pass.value()))
	advance := func(expected, next string) int {
		n, err := passAdvanceScript.Run(ctx, sc.RedisClient, []string{gateIndexPassKey, gateIndexPassProgressKey},
			pass.id, "5", expected, next).Int()
		require.NoError(t, err)
		return n
	}

	p1 := passProgress{passID: pass.id, next: 321}.value()
	assert.Equal(t, 1, advance("", p1), "没有进度时从空值写入")
	assert.Equal(t, 0, advance("", passProgress{passID: pass.id, next: 322}.value()), "旧主人读到的是空值,写不回")
	p2 := passProgress{passID: pass.id, next: 322}.value()
	assert.Equal(t, 1, advance(p1, p2))
	assert.Equal(t, p2, mr.HGet(gateIndexPassProgressKey, "5"))

	ended, err := endPassScript.Run(ctx, sc.RedisClient, []string{gateIndexPassKey, gateIndexPassProgressKey}, pass.id).Int()
	require.NoError(t, err)
	assert.Equal(t, 1, ended)
	assert.Equal(t, 0, advance("", p1), "一遍结束之后谁都写不进")
}

// 段在做的中途换了主人:做完手上这个 slot 就停,进度记到这里,剩下的归新主人。
func TestIndexPassWorker_StopsWhenSegmentMovesAway(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	pass := indexPass{id: "1", kind: passKindBackfill, gen: ""}
	require.NoError(t, mr.Set(gateIndexPassKey, pass.value()))
	owner := &shrinkingOwner{segments: []int{0}, keepFor: 2} // step 的循环一次 + 第一个 slot 前的核对一次
	lister := newCountingSlotKeys(mr)
	deps := testReconcilerDeps(newFakeClusterView(), newTestClock())
	worker := newIndexPassWorker(newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, deps), owner, lister.list)

	worker.step(context.Background())

	raw, err := sc.RedisClient.HGet(context.Background(), gateIndexPassProgressKey, "0").Result()
	require.NoError(t, err)
	prog, ok := parsePassProgress(raw)
	require.True(t, ok, raw)
	assert.Equal(t, 1, prog.next, "只做了第一个 slot")
	assert.Equal(t, 1, lister.listedSlots())
}

// 列一个 slot 失败:这一段停在那个 slot,下一拍从那里接着做,前面做过的不重做。
func TestIndexPassWorker_RetriesFailedSlotNextStep(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	pass := indexPass{id: "1", kind: passKindAudit, gen: ""}
	require.NoError(t, mr.Set(gateIndexPassKey, pass.value()))
	lister := newCountingSlotKeys(mr)
	lister.fail[5] = errors.New("injected listing failure")
	deps := testReconcilerDeps(newFakeClusterView(), newTestClock())
	worker := newIndexPassWorker(newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, deps), segmentRange{0, 1}, lister.list)

	worker.step(context.Background())
	raw := mr.HGet(gateIndexPassProgressKey, "0")
	prog, ok := parsePassProgress(raw)
	require.True(t, ok, raw)
	assert.Equal(t, 5, prog.next, "停在失败的 slot")

	delete(lister.fail, 5)
	worker.step(context.Background())
	raw = mr.HGet(gateIndexPassProgressKey, "0")
	prog, ok = parsePassProgress(raw)
	require.True(t, ok, raw)
	assert.Equal(t, segmentEnd(0), prog.next, "下一步做完这一段")
	assert.Equal(t, 1, lister.listed[0], "做过的 slot 不重列")
}

// 等着的这一遍被换掉(短暂重叠的另一个 leader 发布了新的一遍)或删掉:这一遍作废,不据此设就绪标记。
func TestShardedPass_ReplacedPassFailsTheWait(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	lister := newCountingSlotKeys(mr)
	r, _ := newShardedFixture(t, sc, newFakeClusterView(), newTestClock(), lister, segmentRange{0, 0})
	r.deps.sleep = func(ctx context.Context, _ time.Duration) error {
		mr.Del(gateIndexPassKey)
		return ctx.Err()
	}

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileResultRedisError, step.result)
	assert.False(t, mr.Exists(gateIndexReadyKey))
}

// shrinkingOwner 前 keepFor 次返回 segments,之后什么都不持有。
type shrinkingOwner struct {
	mu       sync.Mutex
	segments []int
	keepFor  int
	calls    int
}

func (o *shrinkingOwner) OwnedSegments() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if o.calls <= o.keepFor {
		return o.segments
	}
	return nil
}
