package logic

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/config"
	"player_locator/internal/svc"
	pb "proto/player_locator"
	"shared/leader"
	"shared/playerredis"
	"shared/redisslot"
)

// 会话对账(死 gate 清扫、写者检查、补建 / 审计、就绪标记、选主让位)的行为测试。
// 约定:miniredis + 注入的固定时钟 + 不限速的 pacer + 受控的拍,不读墙钟;并发经 go-redis Hook 在指定命令
// 之后同步注入,结果确定。玩家域客户端挂着生产的发送前守卫(newTestSvcCtx),跨 slot 的命令在这里就失败。

const testReconcileInterval = 60 * time.Second

// fakeClusterView 是可在测试中途切换返回值的 clusterView;并发安全(run 的用例在另一个 goroutine 里读它)。
type fakeClusterView struct {
	mu          sync.Mutex
	live        map[string]struct{}
	liveErr     error
	all         map[string]struct{}
	capable     map[string]struct{}
	writersErr  error
	liveCalls   int
	writerCalls int
}

func newFakeClusterView(liveGates ...string) *fakeClusterView {
	f := &fakeClusterView{}
	f.setLive(liveGates...)
	f.setWriters([]string{"locator-1"}, []string{"locator-1"})
	return f
}

func toSet(items []string) map[string]struct{} {
	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}
	return set
}

func (f *fakeClusterView) setLive(gates ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live, f.liveErr = toSet(gates), nil
}

func (f *fakeClusterView) setLiveErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.liveErr = err
}

func (f *fakeClusterView) setWriters(all, capable []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.all, f.capable, f.writersErr = toSet(all), toSet(capable), nil
}

func (f *fakeClusterView) setWritersErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writersErr = err
}

func (f *fakeClusterView) LiveGateInstances(context.Context) (map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.liveCalls++
	if f.liveErr != nil {
		return nil, f.liveErr
	}
	return f.live, nil
}

func (f *fakeClusterView) SessionWriters(context.Context) (map[string]struct{}, map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writerCalls++
	if f.writersErr != nil {
		return nil, nil, f.writersErr
	}
	return f.all, f.capable, nil
}

func (f *fakeClusterView) liveCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveCalls
}

func (f *fakeClusterView) writerCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writerCalls
}

// testClock 是注入的时钟;只有用例显式 Advance 时才走。
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Unix(1_800_000_000, 0)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testReconcileConf() config.ReconcileConf {
	return config.ReconcileConf{
		AuditIntervalSeconds:    86400,
		ScanCount:               1000,
		ScanPagesPerSecond:      -1,
		MaxTransitionsPerSecond: -1,
		DeadGateMembersPerRound: 30000,
	}
}

// testScanSessions 是单测里的 deps.scanSessions:经玩家域客户端一路 SCAN 到底(miniredis 不支持 INFO server,
// playerredis.ScanEachMaster 核不了 run_id;拓扑变化的路径由用例注入 ErrTopologyChanged)。走客户端,commandRecorder 看得见。
func testScanSessions(ctx context.Context, c redis.UniversalClient, match string, count int64, fn func(keys []string) error) error {
	var cursor uint64
	for {
		keys, next, err := c.Scan(ctx, cursor, match, count).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := fn(keys); err != nil {
				return err
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

// testReconcilerDeps:不限速、拍立即到、补建循环的间隔与重试退避立即到。用例按需替换。
func testReconcilerDeps(view clusterView, clock *testClock) reconcilerDeps {
	return reconcilerDeps{
		view:     view,
		now:      clock.Now,
		newPacer: func(int) pacer { return noopPacer{} },
		newTicker: func(time.Duration) (func(context.Context) error, func()) {
			return func(ctx context.Context) error { return ctx.Err() }, func() {}
		},
		sleep:        func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		scanSessions: testScanSessions,
	}
}

// injectedRedisError 模拟服务端回复的错误(实现 redis.Error,与 go-redis 解析出的 "-ERR ..." 回复同类)。
type injectedRedisError string

func (e injectedRedisError) Error() string { return string(e) }

func (injectedRedisError) RedisError() {}

// cmdArg 返回命令的第 i 个参数的字符串形式(越界为 "")。
func cmdArg(cmd redis.Cmder, i int) string {
	args := cmd.Args()
	if i >= len(args) {
		return ""
	}
	return fmt.Sprint(args[i])
}

// isScriptOn 判断 cmd 是不是 KEYS 恰为 keys 的 EVAL / EVALSHA(go-redis 的 Script.Run 先发 EVALSHA,NOSCRIPT 时再发 EVAL)。
func isScriptOn(cmd redis.Cmder, keys ...string) bool {
	if name := cmd.Name(); name != "evalsha" && name != "eval" {
		return false
	}
	if cmdArg(cmd, 2) != strconv.Itoa(len(keys)) {
		return false
	}
	for i, key := range keys {
		if cmdArg(cmd, 3+i) != key {
			return false
		}
	}
	return true
}

// fixedSlots 是持有固定 slot 的 slotOwner。
type fixedSlots []uint16

func (f fixedSlots) OwnedSlots() []uint16 { return f }

// registrySlotsOwner 是单测用的 slotOwner:相当于持有全部 slot,但只列出 miniredis 里真有登记集的那些,省掉每拍 16384 次空读。
type registrySlotsOwner struct {
	mr *miniredis.Miniredis
}

func (o registrySlotsOwner) OwnedSlots() []uint16 {
	var slots []uint16
	for _, key := range o.mr.Keys() {
		if gateRegistryKeyShape.MatchString(key) {
			slots = append(slots, redisslot.HashSlot(key))
		}
	}
	slices.Sort(slots)
	return slots
}

func newTestReconciler(t *testing.T, sc *svc.ServiceContext, view clusterView) *sessionReconciler {
	t.Helper()
	return newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, testReconcilerDeps(view, newTestClock()))
}

func newTestSweeper(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, view clusterView) *gateSweeper {
	t.Helper()
	return newGateSweeper(sc, registrySlotsOwner{mr}, testReconcileConf(), testReconcileInterval, testReconcilerDeps(view, newTestClock()))
}

// blockUntilDone 是「等到 ctx 结束」的 sleep / tick:run 的用例用它让补建循环或写者循环停在等待处。
func blockUntilDone(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

// secondClient 是同一个 miniredis 上的另一组客户端,不挂 Hook:用来在 Hook 里注入并发写。
func secondClient(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis) *svc.ServiceContext {
	t.Helper()
	ctrl := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { ctrl.Close() })
	player := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { player.Close() })
	return &svc.ServiceContext{Config: sc.Config, RedisClient: ctrl, PlayerRedis: player}
}

// commandRecorder 计数客户端发出的命令(含 pipeline),after 在每条普通命令返回之后同步调用。
// fail 返回非 nil 时这条命令不发出,直接以该错误返回(模拟传输错误或服务端错误回复;仍计数);pipeline 里任何一条命中,整条 pipeline 失败。
type commandRecorder struct {
	mu       sync.Mutex
	counts   map[string]int
	mgetKeys int
	scans    []string // 每次 SCAN 的游标参数,按发出顺序(含失败的)
	sscans   []string // 每次 SSCAN 的游标参数,按发出顺序(含失败的)
	before   func(ctx context.Context, cmd redis.Cmder)
	after    func(cmd redis.Cmder)
	fail     func(cmd redis.Cmder) error
}

func newCommandRecorder() *commandRecorder { return &commandRecorder{counts: map[string]int{}} }

func (h *commandRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *commandRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.before != nil {
			h.before(ctx, cmd)
		}
		if h.fail != nil {
			if err := h.fail(cmd); err != nil {
				cmd.SetErr(err)
				h.record(cmd)
				return err
			}
		}
		err := next(ctx, cmd)
		h.record(cmd)
		if h.after != nil {
			h.after(cmd)
		}
		return err
	}
}

func (h *commandRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if h.fail != nil {
			for _, cmd := range cmds {
				if err := h.fail(cmd); err != nil {
					for _, c := range cmds {
						c.SetErr(err)
						h.record(c)
					}
					return err
				}
			}
		}
		err := next(ctx, cmds)
		for _, cmd := range cmds {
			h.record(cmd)
			// 批量读会话走 pipeline(按 slot 分组),「读完之后注入并发写」的用例要在这里接上。
			if h.after != nil && cmd.Name() == "mget" {
				h.after(cmd)
			}
		}
		return err
	}
}

func (h *commandRecorder) record(cmd redis.Cmder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts[cmd.Name()]++
	switch cmd.Name() {
	case "mget":
		h.mgetKeys += len(cmd.Args()) - 1
	case "scan":
		h.scans = append(h.scans, cmdArg(cmd, 1))
	case "sscan":
		h.sscans = append(h.sscans, cmdArg(cmd, 2))
	}
}

func (h *commandRecorder) sscanCursors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sscans...)
}

func (h *commandRecorder) count(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[name]
}

func (h *commandRecorder) mgetKeyCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mgetKeys
}

func mustIndexRoundOK(t *testing.T, s *gateSweeper) indexRoundReport {
	t.Helper()
	out := s.indexRound(context.Background())
	require.NoError(t, out.err)
	return out.report
}

func mustWriterRoundOK(t *testing.T, r *sessionReconciler) indexRoundReport {
	t.Helper()
	out := r.writerRound(context.Background())
	require.NoError(t, out.err)
	return out.report
}

// writeRawSession 直接写会话字节、不维护索引:模拟不维护索引的旧版写者或外部篡改。
func writeRawSession(t *testing.T, mr *miniredis.Miniredis, s *pb.PlayerSession) []byte {
	t.Helper()
	data, err := proto.Marshal(s)
	require.NoError(t, err)
	require.NoError(t, mr.Set(sessionKey(s.PlayerId), string(data)))
	return data
}

var leaseReadyKeyShape = regexp.MustCompile(`^player:leases:\{[a-z]+\}$`)

// leaseMembers 返回全部 slot 的 ready 租约成员(排序后);一个都没有时为 nil。
func leaseMembers(t *testing.T, mr *miniredis.Miniredis) []string {
	t.Helper()
	var out []string
	for _, key := range mr.Keys() {
		if !leaseReadyKeyShape.MatchString(key) || mr.Type(key) != "zset" {
			continue
		}
		members, err := mr.ZMembers(key)
		require.NoError(t, err)
		out = append(out, members...)
	}
	sort.Strings(out)
	return out
}

func setReadyMarker(t *testing.T, mr *miniredis.Miniredis, ttl time.Duration) {
	t.Helper()
	require.NoError(t, mr.Set(gateIndexReadyKey, gateIndexReadyValuePrefix+"1"))
	mr.SetTTL(gateIndexReadyKey, ttl)
}

// seedIndexMember 直接往这名玩家 slot 上的 gate 索引加成员并登记 gate,不碰会话:模拟旧写者、局部清库留下的脏成员。
func seedIndexMember(t *testing.T, mr *miniredis.Miniredis, gate string, pid uint64) {
	t.Helper()
	seedRawIndexMember(t, mr, redisslot.PlayerSlot(pid), gate, strconv.FormatUint(pid, 10))
}

func seedRawIndexMember(t *testing.T, mr *miniredis.Miniredis, slot uint16, gate, member string) {
	t.Helper()
	_, err := mr.SetAdd(gateSessionIndexKey(slot, gate), member)
	require.NoError(t, err)
	_, err = mr.SetAdd(gateRegistryKey(slot), gate)
	require.NoError(t, err)
}

// gateKeysOf 返回登记集里这个 gate 的全部索引键(按 slot 排序)。
func gateKeysOf(t *testing.T, mr *miniredis.Miniredis, gate string) []gateSlot {
	t.Helper()
	var pairs []gateSlot
	for _, key := range mr.Keys() {
		if !gateRegistryKeyShape.MatchString(key) {
			continue
		}
		registered, err := mr.IsMember(key, gate)
		require.NoError(t, err)
		if registered {
			pairs = append(pairs, gateSlot{slot: redisslot.HashSlot(key), gate: gate})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].slot < pairs[j].slot })
	return pairs
}

// pidsInSameSlot 返回从 base 起的 n 个同 slot 的玩家 id(含 base)。同一把索引键上的多成员用例要用它:
// 相邻的 id 几乎总在不同的 slot。
func pidsInSameSlot(base uint64, n int) []uint64 {
	slot := redisslot.PlayerSlot(base)
	pids := []uint64{base}
	for pid := base + 1; len(pids) < n; pid++ {
		if redisslot.PlayerSlot(pid) == slot {
			pids = append(pids, pid)
		}
	}
	return pids
}

// ---------------------------------------------------------------------------
// 死 gate 清扫
// ---------------------------------------------------------------------------

func TestIndexRound_DeadGateNeedsTwoConsecutiveRounds(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9001, 100, idxGateA, 1)))
	before := readSessionBytes(t, sc, 9001)
	r := newTestSweeper(t, sc, mr, newFakeClusterView()) // A 不在 etcd

	rep := mustIndexRoundOK(t, r)
	assert.Empty(t, rep.deadGates, "第一拍缺席只挂嫌疑")
	assert.Equal(t, before, readSessionBytes(t, sc, 9001))

	rep = mustIndexRoundOK(t, r)
	assert.Equal(t, []string{idxGateA}, rep.deadGates)
	after := readSession(t, sc, 9001)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, after.State)
	assert.Equal(t, uint32(2), after.SessionVersion)
	assert.Contains(t, leaseMembers(t, mr), "9001")
	assert.Nil(t, indexMembers(t, mr, idxGateA), "死 gate 处理完后索引键被删")
	assert.NotContains(t, registryMembers(t, mr), idxGateA, "并移出登记集")
	assert.Equal(t, 1, rep.actions[reconcileActionToDisconnecting])
	assert.Equal(t, []gateSlot{{slot: redisslot.PlayerSlot(9001), gate: idxGateA}}, rep.released)
}

func TestIndexRound_LiveGateUntouched(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9002, 100, idxGateA, 1)))
	before := readSessionBytes(t, sc, 9002)
	r := newTestSweeper(t, sc, mr, newFakeClusterView(idxGateA))

	for i := 0; i < 4; i++ {
		rep := mustIndexRoundOK(t, r)
		assert.Equal(t, 1, rep.liveIndexKeys)
	}
	assert.Equal(t, before, readSessionBytes(t, sc, 9002))
	assert.Equal(t, []string{"9002"}, indexMembers(t, mr, idxGateA))
	assert.Empty(t, leaseMembers(t, mr))
}

// 缺席、存活、缺席:必须**连续**两拍缺席才动手;去抖在存活那一拍清零。
func TestIndexRound_ReappearanceResetsSuspicion(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9003, 100, idxGateA, 1)))
	before := readSessionBytes(t, sc, 9003)
	view := newFakeClusterView()
	r := newTestSweeper(t, sc, mr, view)

	mustIndexRoundOK(t, r) // 缺席
	view.setLive(idxGateA)
	mustIndexRoundOK(t, r) // 存活
	view.setLive()
	mustIndexRoundOK(t, r) // 缺席(重新计 1)
	assert.Equal(t, before, readSessionBytes(t, sc, 9003))

	mustIndexRoundOK(t, r) // 连续第二拍缺席
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, 9003).State)
}

// 本项的核心性质:对账不扫全键空间,MGET 的只是死 gate 的成员。
func TestIndexRound_DoesNotScanKeyspace(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	for i := 0; i < 5000; i++ {
		require.NoError(t, mr.Set("noise:"+strconv.Itoa(i), "x"))
	}
	for i := uint64(0); i < 10; i++ {
		require.NoError(t, callSetSession(sc, indexTestSession(9100+i, 100, idxGateA, 1)))
	}
	for i := uint64(0); i < 3; i++ {
		require.NoError(t, callSetSession(sc, indexTestSession(9200+i, 100, idxGateB, 1)))
	}
	rec := newCommandRecorder()
	sc.PlayerRedis.AddHook(rec)
	r := newTestSweeper(t, sc, mr, newFakeClusterView(idxGateA)) // B 死了

	mustIndexRoundOK(t, r)
	rep := mustIndexRoundOK(t, r)

	assert.Equal(t, 0, rec.count("scan"), "清扫不得 SCAN 全键空间")
	assert.Equal(t, 3, rec.mgetKeyCount(), "只 MGET 死 gate 上的 3 个成员")
	assert.Equal(t, 3, rep.actions[reconcileActionToDisconnecting])
	assert.Equal(t, 10, rep.liveIndexKeys, "这些玩家各在一个 slot 上,10 把索引键")
	assert.Equal(t, 0, rec.count("scard"), "gauge 用登记集里的键数,不逐键 SCARD")
}

// 只处理分给本副本的 slot:同一个死 gate 在别的 slot 上的会话归持有那些 slot 的副本。
func TestIndexRound_OnlyProcessesOwnedSlots(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	const mine, theirs = uint64(9880), uint64(9881)
	require.NotEqual(t, redisslot.PlayerSlot(mine), redisslot.PlayerSlot(theirs))
	require.NoError(t, callSetSession(sc, indexTestSession(mine, 100, idxGateA, 1)))
	require.NoError(t, callSetSession(sc, indexTestSession(theirs, 100, idxGateA, 1)))
	r := newGateSweeper(sc, fixedSlots{redisslot.PlayerSlot(mine)}, testReconcileConf(), testReconcileInterval,
		testReconcilerDeps(newFakeClusterView(), newTestClock()))

	mustIndexRoundOK(t, r)
	mustIndexRoundOK(t, r)

	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, mine).State)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, readSession(t, sc, theirs).State)
	assert.Equal(t, []string{"9881"}, indexMembers(t, mr, idxGateA))
}

// 持有全部 slot 时登记集分批读(真实的 16384 个 slot,不靠 registrySlotsOwner 收窄),每个 slot 都读得到。
func TestIndexRound_AllSlotsAreCovered(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9882, 100, idxGateA, 1)))
	rec := newCommandRecorder()
	sc.PlayerRedis.AddHook(rec)
	r := newGateSweeper(sc, fixedSlots(slotRange(0, redisslot.SlotCount)), testReconcileConf(), testReconcileInterval,
		testReconcilerDeps(newFakeClusterView(), newTestClock()))

	mustIndexRoundOK(t, r)

	assert.Equal(t, redisslot.SlotCount, rec.count("smembers"))
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, readSession(t, sc, 9882).State, "第一拍只挂嫌疑")
	mustIndexRoundOK(t, r)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, 9882).State)
}

// 分不到段的副本不读 etcd(副本数超过段数时大多数副本是这样);让出段的副本照常读,靠这一读判断能不能回去。
func TestIndexRound_EtcdReadOnlyWhenItMatters(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	view := newFakeClusterView()
	r := newGateSweeper(sc, fixedSlots(nil), testReconcileConf(), testReconcileInterval, testReconcilerDeps(view, newTestClock()))

	mustIndexRoundOK(t, r)
	assert.Equal(t, 0, view.liveCallCount(), "没有段、也没让出:不读 etcd")

	r.eligible.Store(false)
	mustIndexRoundOK(t, r)
	assert.Equal(t, 1, view.liveCallCount(), "让出段的副本要试一次")
}

// 死 gate 索引里的三类脏成员(会话已在别的 gate 上 ONLINE、会话键不存在、成员不是 player_id)都被摘除,
// 别的 gate 上的会话一个字节都不动。
func TestIndexRound_PrunesStaleMembers(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9301, 100, idxGateB, 1)))
	beforeB := readSessionBytes(t, sc, 9301)
	seedIndexMember(t, mr, idxGateA, 9301)
	seedIndexMember(t, mr, idxGateA, 9302)
	seedRawIndexMember(t, mr, redisslot.PlayerSlot(9301), idxGateA, "junk")
	r := newTestSweeper(t, sc, mr, newFakeClusterView(idxGateB))

	mustIndexRoundOK(t, r)
	rep := mustIndexRoundOK(t, r)

	assert.Nil(t, indexMembers(t, mr, idxGateA))
	assert.Equal(t, beforeB, readSessionBytes(t, sc, 9301))
	assert.Equal(t, []string{"9301"}, indexMembers(t, mr, idxGateB))
	assert.Equal(t, 1, rep.actions[reconcileActionPrunedStale])
	assert.Equal(t, 1, rep.actions[reconcileActionPrunedMissing])
	assert.Equal(t, 1, rep.actions[reconcileActionPrunedBadMember])
	assert.Empty(t, leaseMembers(t, mr))
}

// 成员是合法的 player_id,却不属于这把索引的 slot:声明不出同槽的会话键,按坏成员直接摘,那名玩家的会话不动。
func TestIndexRound_MemberOfAnotherSlotPrunedAsBadMember(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	const owner, stranger = uint64(9301), uint64(9302)
	require.NotEqual(t, redisslot.PlayerSlot(owner), redisslot.PlayerSlot(stranger))
	require.NoError(t, callSetSession(sc, indexTestSession(stranger, 100, idxGateB, 1)))
	before := readSessionBytes(t, sc, stranger)
	seedRawIndexMember(t, mr, redisslot.PlayerSlot(owner), idxGateA, strconv.FormatUint(stranger, 10))
	r := newTestSweeper(t, sc, mr, newFakeClusterView(idxGateB))

	mustIndexRoundOK(t, r)
	rep := mustIndexRoundOK(t, r)

	assert.Nil(t, indexMembers(t, mr, idxGateA))
	assert.Equal(t, 1, rep.actions[reconcileActionPrunedBadMember])
	assert.Equal(t, before, readSessionBytes(t, sc, stranger))
}

// 会话字节解不开,或载荷里的 player_id 与成员不符:只摘成员,会话原样保留、不入租约。
func TestIndexRound_MalformedSessionPrunedNotModified(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	garbage := string([]byte{0xff, 0xff, 0xff})
	require.NoError(t, mr.Set(sessionKey(9401), garbage))
	wrongOwner, err := proto.Marshal(indexTestSession(9999, 100, idxGateA, 1))
	require.NoError(t, err)
	require.NoError(t, mr.Set(sessionKey(9402), string(wrongOwner)))
	seedIndexMember(t, mr, idxGateA, 9401)
	seedIndexMember(t, mr, idxGateA, 9402)
	r := newTestSweeper(t, sc, mr, newFakeClusterView())

	mustIndexRoundOK(t, r)
	rep := mustIndexRoundOK(t, r)

	assert.Equal(t, 2, rep.actions[reconcileActionPrunedMalformed])
	assert.Equal(t, 0, rep.actions[reconcileActionToDisconnecting])
	got1, _ := mr.Get(sessionKey(9401))
	got2, _ := mr.Get(sessionKey(9402))
	assert.Equal(t, garbage, got1)
	assert.Equal(t, string(wrongOwner), got2)
	assert.Empty(t, leaseMembers(t, mr))
}

// etcd 存活集读不出来:整拍不推进去抖、不转换;恢复后仍要连续两拍缺席才动手。
func TestIndexRound_EtcdErrorSkipsWithoutProgress(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9501, 100, idxGateA, 1)))
	before := readSessionBytes(t, sc, 9501)
	view := newFakeClusterView()
	view.setLiveErr(errors.New("etcd unavailable"))
	r := newTestSweeper(t, sc, mr, view)

	for i := 0; i < 5; i++ {
		out := r.indexRound(context.Background())
		require.Error(t, out.err)
		assert.Equal(t, stageEtcd, out.stage)
	}
	assert.Equal(t, before, readSessionBytes(t, sc, 9501))

	view.setLive() // 恢复,A 缺席
	mustIndexRoundOK(t, r)
	assert.Equal(t, before, readSessionBytes(t, sc, 9501), "恢复后的第一拍只挂嫌疑")
	mustIndexRoundOK(t, r)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, 9501).State)
}

// 判定与提交之间玩家顶号换到 B:转换 CAS 失败成 no-op,会话保持在 B 上 ONLINE,成员在 B 不在 A,不入租约。
func TestIndexRound_ConcurrentReplaceDuringRoundIsNoop(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9601, 100, idxGateA, 1)))
	r := newTestSweeper(t, sc, mr, newFakeClusterView(idxGateB))
	mustIndexRoundOK(t, r)

	other := secondClient(t, sc, mr)
	rec := newCommandRecorder()
	var once sync.Once
	rec.after = func(cmd redis.Cmder) {
		if cmd.Name() != "mget" {
			return
		}
		once.Do(func() {
			require.NoError(t, callSetSession(other, indexTestSession(9601, 200, idxGateB, 2)))
		})
	}
	sc.PlayerRedis.AddHook(rec)

	rep := mustIndexRoundOK(t, r)

	s := readSession(t, sc, 9601)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, s.State)
	assert.Equal(t, idxGateB, s.GateInstanceId)
	assert.Equal(t, []string{"9601"}, indexMembers(t, mr, idxGateB))
	assert.Empty(t, indexMembers(t, mr, idxGateA))
	assert.Empty(t, leaseMembers(t, mr))
	assert.Equal(t, 1, rep.actions[reconcileActionCASConflict])
	assert.Equal(t, 0, rep.actions[reconcileActionToDisconnecting])
}

// 每拍预算:单个死 gate 的成员数超过一拍预算时分几拍处理完,这一拍照常完成;
// 第二个死 gate 在第一拍之后仍被识别(计入 deadGates),预算轮到它时处理。
func TestIndexRound_BudgetSpreadsDeadGateAcrossRounds(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	for i := uint64(0); i < 6; i++ {
		require.NoError(t, callSetSession(sc, indexTestSession(9700+i, 100, idxGateA, 1)))
	}
	require.NoError(t, callSetSession(sc, indexTestSession(9800, 100, idxGateB, 1)))
	cfg := testReconcileConf()
	cfg.DeadGateMembersPerRound = 4
	cfg.ScanCount = 4
	r := newGateSweeper(sc, registrySlotsOwner{mr}, cfg, testReconcileInterval, testReconcilerDeps(newFakeClusterView(), newTestClock()))

	mustIndexRoundOK(t, r)
	rep := mustIndexRoundOK(t, r)
	assert.Equal(t, []string{idxGateA, idxGateB}, rep.deadGates, "两个死 gate 都已确认")
	assert.Equal(t, 4, rep.actions[reconcileActionToDisconnecting], "本拍只用掉 4 个成员的预算")
	assert.Equal(t, 3, rep.deadIndexKeys, "进度:A 剩 2、B 剩 1(这些玩家各在一个 slot 上)")
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, readSession(t, sc, 9800).State)

	for i := 0; i < 2; i++ {
		mustIndexRoundOK(t, r)
	}
	for i := uint64(0); i < 6; i++ {
		assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, 9700+i).State)
	}
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, 9800).State)
	assert.Empty(t, registryMembers(t, mr), "两个死 gate 处理完后都已释放")
}

// 一拍内多页续扫:一把死 gate 索引键的成员数超过 SSCAN 的 COUNT、预算充足时,这一拍内按游标连续扫多页(processDeadGates 里
// 「next != 0 且预算还有余」的续扫分支),不把剩下的页都拖到下一拍。
//
// miniredis 与真实 Redis 的差异:miniredis 的 SSCAN 游标是排序后成员列表的偏移,转换会 SREM 掉已扫过的成员,
// 后面的成员前移,游标就跳过了一部分;真实 Redis 的游标按哈希桶推进,删除不影响覆盖。所以这里只断言「一拍内转换数
// 大于一页、SSCAN 至少两次」,释放留到下一拍再断言(真实 Redis 上第一拍就会释放,下一拍的断言同样成立)。
func TestIndexRound_MultiPageDeadGateSweepInOneRound(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	pids := pidsInSameSlot(9870, 6)
	for _, pid := range pids {
		require.NoError(t, callSetSession(sc, indexTestSession(pid, 100, idxGateA, 1)))
	}
	cfg := testReconcileConf()
	cfg.ScanCount = 2
	r := newGateSweeper(sc, registrySlotsOwner{mr}, cfg, testReconcileInterval, testReconcilerDeps(newFakeClusterView(), newTestClock()))
	mustIndexRoundOK(t, r) // 第一拍只挂嫌疑

	rec := newCommandRecorder()
	sc.PlayerRedis.AddHook(rec)
	rep := mustIndexRoundOK(t, r)

	assert.Greater(t, rep.actions[reconcileActionToDisconnecting], int(cfg.ScanCount), "一拍内处理了不止一页")
	assert.GreaterOrEqual(t, rec.count("sscan"), 2)

	mustIndexRoundOK(t, r)
	for _, pid := range pids {
		assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, pid).State)
	}
	assert.Nil(t, indexMembers(t, mr, idxGateA))
	assert.NotContains(t, registryMembers(t, mr), idxGateA, "扫完一遍、索引空了才释放")
}

// 死 gate(实为分区存活,或 etcd 视图误判)的索引释放与并发 SetSession 竞态:释放之前有同 slot 的玩家经 SetSession 回到这个 gate。
// I2 的唯一保障是两件事都在 Redis 单线程上原子执行:SetSession 同时 SADD 成员与登记集;释放脚本内 SCARD==0 才 DEL。
//
// Hook 在释放脚本之前注入 SetSession。另外,凡是单独发出的、针对这把索引键的 SCARD 之后 / DEL 之前也注入 ——
// 现行实现没有这种命令,不会触发;有人把释放改成 Go 侧「先 SCARD 再 DEL」或无条件 DEL 时,注入就落在两步之间,
// 新成员被删,I1 断言变红。
func TestIndexRound_ReleaseRacesConcurrentSetSessionOnDeadGate(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	pids := pidsInSameSlot(9850, 2)
	first, second := pids[0], pids[1]
	secondMember := strconv.FormatUint(second, 10)
	require.NoError(t, callSetSession(sc, indexTestSession(first, 100, idxGateA, 1)))
	// A 不在 etcd;第一拍只挂嫌疑。
	r := newTestSweeper(t, sc, mr, newFakeClusterView())
	mustIndexRoundOK(t, r)

	other := secondClient(t, sc, mr)
	pair := gateSlot{slot: redisslot.PlayerSlot(first), gate: idxGateA}
	indexKeyA := gateSessionIndexKey(pair.slot, pair.gate)
	registryKey := gateRegistryKey(pair.slot)
	var injected []byte
	var once sync.Once
	inject := func() {
		once.Do(func() {
			require.NoError(t, callSetSession(other, indexTestSession(second, 200, idxGateA, 1)))
			injected = readSessionBytes(t, other, second)
		})
	}
	rec := newCommandRecorder()
	rec.before = func(_ context.Context, cmd redis.Cmder) {
		switch {
		case isScriptOn(cmd, indexKeyA, registryKey): // 释放脚本:KEYS = 索引键、登记集
			inject()
		case cmd.Name() == "del" && cmdArg(cmd, 1) == indexKeyA:
			inject()
		}
	}
	rec.after = func(cmd redis.Cmder) {
		if cmd.Name() == "scard" && cmdArg(cmd, 1) == indexKeyA {
			inject()
		}
	}
	sc.PlayerRedis.AddHook(rec)

	rep := mustIndexRoundOK(t, r)

	require.NotNil(t, injected, "释放之前必须注入过并发 SetSession")
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, first).State)
	assert.NotContains(t, rep.released, pair, "索引里有新成员,不得释放")
	assert.Equal(t, []string{secondMember}, indexMembers(t, mr, idxGateA))
	assert.Contains(t, registryMembers(t, mr), idxGateA)
	assert.Equal(t, injected, readSessionBytes(t, sc, second), "新会话一个字节都没动")
	assertGateIndexInvariant(t, sc, mr, []uint64{first, second}, []string{idxGateA}, "after the race")

	// A 仍不在 etcd、仍在登记集里:保持确认死亡,下一拍直接处理新成员并释放。
	rep = mustIndexRoundOK(t, r)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, second).State)
	assert.Contains(t, rep.released, pair)
	assert.NotContains(t, registryMembers(t, mr), idxGateA)
	assertGateIndexInvariant(t, sc, mr, []uint64{first, second}, []string{idxGateA}, "after the next round")
}

// 毒键:死 gate A 的索引里有一个成员,它的会话键被写成了 hash(外部篡改 / 误写)。MGET 对非 string 回 nil,判成
// missing,守卫脚本里的 GET 报 WRONGTYPE。只跳过这一个成员(member_error),每一拍照常完成、不让出段:A 的其余成员与
// 死 gate B 照常转换、B 照常释放;毒成员留在 A 的索引里,它所在的索引键不释放(由 DeadGatesPending 暴露)。
func TestIndexRound_WrongTypeSessionKeySkipsMemberNotRound(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9881, 100, idxGateA, 1)))
	require.NoError(t, callSetSession(sc, indexTestSession(9882, 100, idxGateB, 1)))
	mr.HSet(sessionKey(9880), "field", "not a session")
	seedIndexMember(t, mr, idxGateA, 9880)
	r := newTestSweeper(t, sc, mr, newFakeClusterView())
	mustIndexRoundOK(t, r)

	for round := 0; round < maxConsecutiveRoundFailures; round++ {
		out := r.indexRound(context.Background())
		require.NoErrorf(t, out.err, "round %d: 一个毒键不得让整拍失败", round)
		assert.Equal(t, 1, out.report.actions[reconcileActionMemberError])
	}
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, 9881).State)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, 9882).State)
	assert.NotContains(t, registryMembers(t, mr), idxGateB, "B 照常释放")
	assert.Contains(t, registryMembers(t, mr), idxGateA, "A 留着毒成员,不释放")
	assert.Equal(t, []string{"9880"}, indexMembers(t, mr, idxGateA))
	assert.Equal(t, "hash", mr.Type(sessionKey(9880)), "毒键原样保留,留给排查")
}

// 某把索引键被写成了别的类型:死 gate A 的 SSCAN 报 WRONGTYPE,只跳过这把键(gate_error),每一拍照常完成,死 gate B 照常处理并释放。
// 存活 gate C 的索引键这一拍根本不碰:gauge 只用登记集里的键数。
func TestIndexRound_WrongTypeIndexKeySkipsGateNotRound(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9891, 100, idxGateB, 1)))
	for _, seed := range []struct {
		pid  uint64
		gate string
	}{{9892, idxGateA}, {9893, idxGateC}} {
		slot := redisslot.PlayerSlot(seed.pid)
		require.NoError(t, mr.Set(gateSessionIndexKey(slot, seed.gate), "not a set"))
		_, err := mr.SetAdd(gateRegistryKey(slot), seed.gate)
		require.NoError(t, err)
	}
	// A、B 死,C 活。第一拍:A、B 挂嫌疑。
	r := newTestSweeper(t, sc, mr, newFakeClusterView(idxGateC))
	mustIndexRoundOK(t, r)

	out := r.indexRound(context.Background())

	require.NoError(t, out.err)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, 9891).State)
	assert.Contains(t, out.report.released, gateSlot{slot: redisslot.PlayerSlot(9891), gate: idxGateB})
	assert.Equal(t, 1, out.report.actions[reconcileActionGateError], "A 的 SSCAN 一次")
	assert.ElementsMatch(t, []string{idxGateA, idxGateC}, registryMembers(t, mr))
}

// 某个 slot 的登记集被写成了别的类型:只跳过这个 slot(gate_error),别的 slot 照常。
func TestIndexRound_WrongTypeRegistryKeySkipsSlotNotRound(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9895, 100, idxGateA, 1)))
	badSlot := redisslot.PlayerSlot(9896)
	require.NoError(t, mr.Set(gateRegistryKey(badSlot), "not a set"))
	r := newGateSweeper(sc, fixedSlots{redisslot.PlayerSlot(9895), badSlot}, testReconcileConf(), testReconcileInterval,
		testReconcilerDeps(newFakeClusterView(), newTestClock()))

	out := r.indexRound(context.Background())
	require.NoError(t, out.err)
	assert.Equal(t, 1, out.report.actions[reconcileActionGateError])
	out = r.indexRound(context.Background())
	require.NoError(t, out.err)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, 9895).State)
}

// 一拍里的单键错误超过上限:不再当作「个别键坏了」,整拍失败(计入让出段的门槛),错误链里保留原始的 WRONGTYPE。
func TestIndexRound_TooManyKeyTypeErrorsFailsRound(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	for i := 0; i <= maxKeyTypeErrorsPerRound; i++ {
		pid := uint64(20000 + i)
		mr.HSet(sessionKey(pid), "field", "x")
		seedIndexMember(t, mr, idxGateA, pid)
	}
	r := newTestSweeper(t, sc, mr, newFakeClusterView())
	mustIndexRoundOK(t, r)

	out := r.indexRound(context.Background())

	require.Error(t, out.err)
	assert.Equal(t, stageRedis, out.stage)
	assert.True(t, isKeyTypeError(out.err))
}

// 一个 slot 的租约键被写成了别的类型:只影响这个 slot 的玩家。转换报 WRONGTYPE 时跳过(member_error),脚本在写会话之前就报错,
// 会话原样、成员留在索引里,键修好后下一拍重判;别的 slot 照常转换,这一拍不算失败。
func TestIndexRound_SlotLeaseKeyWrongTypeOnlyAffectsThatSlot(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	const poisoned, healthy = uint64(9895), uint64(9896)
	require.NotEqual(t, redisslot.PlayerSlot(poisoned), redisslot.PlayerSlot(healthy))
	require.NoError(t, callSetSession(sc, indexTestSession(poisoned, 100, idxGateA, 1)))
	require.NoError(t, callSetSession(sc, indexTestSession(healthy, 100, idxGateA, 1)))
	before := readSessionBytes(t, sc, poisoned)
	require.NoError(t, mr.Set(leaseKeysForPlayer(poisoned).ready, "not a zset")) // SetSession 之后再坏掉,否则 SetSession 自己就失败
	r := newTestSweeper(t, sc, mr, newFakeClusterView())
	mustIndexRoundOK(t, r)

	out := r.indexRound(context.Background())

	require.NoError(t, out.err)
	assert.Equal(t, 1, out.report.actions[reconcileActionMemberError])
	assert.Equal(t, before, readSessionBytes(t, sc, poisoned))
	assert.Equal(t, []string{"9895"}, indexMembers(t, mr, idxGateA))
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, healthy).State)
}

// ---------------------------------------------------------------------------
// 写者检查
// ---------------------------------------------------------------------------

// etcd 读写者列表出错:只跳过本拍写者检查,不删就绪标记、不动代次,标记租约不被续(照常走时);
// 补建也不会因此启动。这一拍记为 etcd 失败(计入让位门槛)。
func TestWriterRound_SessionWritersErrorKeepsMarkerAndGen(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	setReadyMarker(t, mr, 100*time.Second)
	require.NoError(t, mr.Set(gateIndexGenKey, "5"))
	view := newFakeClusterView()
	view.setWritersErr(errors.New("etcd timeout"))
	clock := newTestClock()
	r := newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, testReconcilerDeps(view, clock))
	require.NoError(t, mr.Set(gateIndexLastAuditKey, strconv.FormatInt(clock.Now().Unix(), 10)))

	for i := 0; i < 2; i++ {
		out := r.writerRound(context.Background())
		require.Error(t, out.err)
		assert.Equal(t, stageEtcd, out.stage)
	}
	assert.True(t, mr.Exists(gateIndexReadyKey))
	assert.Equal(t, 100*time.Second, mr.TTL(gateIndexReadyKey), "出错的拍不续租")
	gen, _ := mr.Get(gateIndexGenKey)
	assert.Equal(t, "5", gen)

	rec := newCommandRecorder()
	sc.PlayerRedis.AddHook(rec)
	step := r.repairStep(context.Background())
	assert.Equal(t, "", step.loop, "标记还在、审计未到期:不跑补建")
	assert.Equal(t, 0, rec.count("scan"))
}

// 只有一次成功的 etcd 读里确实看到「已注册却没有能力键」的 locator,才删标记并递增代次。
func TestWriterRound_LegacyWriterClearsMarkerAndBumpsGen(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	setReadyMarker(t, mr, 100*time.Second)
	require.NoError(t, mr.Set(gateIndexGenKey, "5"))
	view := newFakeClusterView()
	view.setWriters([]string{"locator-new", "locator-old"}, []string{"locator-new"})
	r := newTestReconciler(t, sc, view)

	rep := mustWriterRoundOK(t, r)

	assert.Equal(t, 1, rep.legacyWriters)
	assert.False(t, mr.Exists(gateIndexReadyKey))
	gen, _ := mr.Get(gateIndexGenKey)
	assert.Equal(t, "6", gen)
}

// 零旧写者的成功拍给已存在的标记续租到 readyLease;别的版本前缀的标记不续;不存在的标记不新建。
func TestWriterRound_RenewsExistingReadyMarkerOnly(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	r := newTestReconciler(t, sc, newFakeClusterView())

	rep := mustWriterRoundOK(t, r)
	assert.False(t, rep.readyRenewed)
	assert.False(t, mr.Exists(gateIndexReadyKey), "续期不得新建标记")

	setReadyMarker(t, mr, 5*time.Second)
	rep = mustWriterRoundOK(t, r)
	assert.True(t, rep.readyRenewed)
	assert.Equal(t, r.readyLease(), mr.TTL(gateIndexReadyKey))

	require.NoError(t, mr.Set(gateIndexReadyKey, "v1:1"))
	mr.SetTTL(gateIndexReadyKey, 5*time.Second)
	rep = mustWriterRoundOK(t, r)
	assert.False(t, rep.readyRenewed, "全局索引时代(v1)的标记不算数")
	assert.Equal(t, 5*time.Second, mr.TTL(gateIndexReadyKey))
}

// ---------------------------------------------------------------------------
// 补建 / 审计
// ---------------------------------------------------------------------------

// 旧写者写的 ONLINE 会话不在索引里:补建补齐成员与登记集并设就绪标记(带租约),随后清扫两拍内完成转换。
func TestRepairPass_BackfillsLegacySessionAndSetsReady(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	writeRawSession(t, mr, indexTestSession(9901, 100, idxGateA, 1))
	view := newFakeClusterView() // A 已死
	r := newTestReconciler(t, sc, view)

	step := r.repairStep(context.Background())
	assert.Equal(t, reconcileLoopRepair, step.loop)
	assert.Equal(t, reconcileResultOK, step.result)
	assert.Equal(t, 1, step.report.repaired)
	assert.Equal(t, []string{"9901"}, indexMembers(t, mr, idxGateA))
	assert.Contains(t, registryMembers(t, mr), idxGateA)
	assert.True(t, mr.Exists(gateIndexReadyKey))
	assert.Equal(t, r.readyLease(), mr.TTL(gateIndexReadyKey))
	assert.True(t, mr.Exists(gateIndexLastAuditKey), "完整补建同时记下审计时刻")

	sweeper := newTestSweeper(t, sc, mr, view)
	mustIndexRoundOK(t, sweeper)
	mustIndexRoundOK(t, sweeper)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, 9901).State)
}

// 已是成员、但本 slot 的登记集被局部清库删掉了:补建补回登记集(I2),否则这个 gate 死了清扫看不到它。
func TestRepairPass_RestoresMissingRegistryEntry(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9902, 100, idxGateA, 1)))
	mr.Del(gateRegistryKey(redisslot.PlayerSlot(9902)))
	require.Empty(t, registryMembers(t, mr))
	r := newTestReconciler(t, sc, newFakeClusterView(idxGateA))

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileResultOK, step.result)
	assert.Equal(t, 0, step.report.repaired)
	assert.Equal(t, []string{idxGateA}, registryMembers(t, mr))
}

// 补建开始时就有旧写者:照样补,但代次已被这次检查递增,结束时不设标记。
func TestRepairPass_LegacyWriterBlocksReady(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	writeRawSession(t, mr, indexTestSession(9902, 100, idxGateA, 1))
	view := newFakeClusterView(idxGateA)
	view.setWriters([]string{"locator-new", "locator-old"}, []string{"locator-new"})
	r := newTestReconciler(t, sc, view)

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileResultGenChanged, step.result)
	assert.False(t, mr.Exists(gateIndexReadyKey))
	assert.Equal(t, []string{"9902"}, indexMembers(t, mr, idxGateA), "补进来的成员照样有效")
}

// 补建开始前 etcd 读写者失败:这一遍不开始,也不设标记。
func TestRepairPass_WriterCheckErrorDoesNotStart(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	view := newFakeClusterView()
	view.setWritersErr(errors.New("etcd timeout"))
	rec := newCommandRecorder()
	sc.PlayerRedis.AddHook(rec)
	r := newTestReconciler(t, sc, view)

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileResultEtcdError, step.result)
	assert.Equal(t, 0, rec.count("scan"))
	assert.False(t, mr.Exists(gateIndexReadyKey))
}

// 补建进行中(第一页之后)别的拍看到了旧写者、递增了代次:这一遍结束时「代次未变才设」失败,不设标记。
func TestRepairPass_GenChangeMidPassBlocksReady(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	for i := uint64(0); i < 3; i++ {
		writeRawSession(t, mr, indexTestSession(9910+i, 100, idxGateA, 1))
	}
	cfg := testReconcileConf()
	cfg.ScanCount = 1
	other := secondClient(t, sc, mr)
	rec := newCommandRecorder()
	var once sync.Once
	rec.after = func(cmd redis.Cmder) {
		if cmd.Name() != "scan" {
			return
		}
		once.Do(func() {
			require.NoError(t, markLegacyWritersScript.Run(context.Background(), other.RedisClient,
				[]string{gateIndexGenKey, gateIndexReadyKey}).Err())
		})
	}
	sc.PlayerRedis.AddHook(rec)
	r := newSessionReconciler(sc, cfg, testReconcileInterval, testReconcilerDeps(newFakeClusterView(idxGateA), newTestClock()))

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileResultGenChanged, step.result)
	assert.False(t, mr.Exists(gateIndexReadyKey))
}

// 不续期时标记自然过期(没有新版 leader 在跑:一次性回滚、对账关闭),下一任 leader 从补建开始。
func TestReadyMarker_ExpiresWithoutRenewalAndNextLeaderBackfills(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9920, 100, idxGateA, 1)))
	first := newTestReconciler(t, sc, newFakeClusterView(idxGateA))
	require.Equal(t, reconcileResultOK, first.repairStep(context.Background()).result)
	require.True(t, mr.Exists(gateIndexReadyKey))

	mr.FastForward(first.readyLease() + time.Second)
	require.False(t, mr.Exists(gateIndexReadyKey), "没人续期,标记按租约过期")

	rec := newCommandRecorder()
	sc.PlayerRedis.AddHook(rec)
	next := newTestReconciler(t, sc, newFakeClusterView(idxGateA))
	step := next.repairStep(context.Background())
	assert.Equal(t, reconcileLoopRepair, step.loop)
	assert.Equal(t, reconcileResultOK, step.result)
	assert.Positive(t, rec.count("scan"), "下一任 leader 重新全键补建")
	assert.True(t, mr.Exists(gateIndexReadyKey))
}

// DISCONNECTING 会话不补;MGET 之后会话被改写(脚本按字节守卫返回 -1)也不补。
func TestRepairPass_SkipsNonOnlineAndChangedSessions(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	disconnecting := indexTestSession(9930, 100, idxGateA, 2)
	disconnecting.State = pb.PlayerSessionState_SESSION_STATE_DISCONNECTING
	writeRawSession(t, mr, disconnecting)
	writeRawSession(t, mr, indexTestSession(9931, 100, idxGateA, 1))

	other := secondClient(t, sc, mr)
	rec := newCommandRecorder()
	var once sync.Once
	rec.after = func(cmd redis.Cmder) {
		if cmd.Name() != "mget" {
			return
		}
		once.Do(func() {
			changed, err := proto.Marshal(indexTestSession(9931, 200, idxGateA, 2))
			require.NoError(t, err)
			require.NoError(t, other.PlayerRedis.Set(context.Background(), sessionKey(9931), changed, 0).Err())
		})
	}
	sc.PlayerRedis.AddHook(rec)
	r := newTestReconciler(t, sc, newFakeClusterView(idxGateA))

	step := r.repairStep(context.Background())

	assert.Equal(t, 0, step.report.repaired)
	assert.Nil(t, indexMembers(t, mr, idxGateA))
}

// 标记存在时人为删掉一个成员:审计补回来,并计为 audit_repaired(不变量被破坏)。
func TestAuditPass_CountsRepairedWhenReady(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9940, 100, idxGateA, 1)))
	_, err := mr.SRem(gateSessionIndexKey(redisslot.PlayerSlot(9940), idxGateA), "9940")
	require.NoError(t, err)
	setReadyMarker(t, mr, 100*time.Second)
	clock := newTestClock()
	r := newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, testReconcilerDeps(newFakeClusterView(idxGateA), clock))

	step := r.repairStep(context.Background()) // last_audit 缺失 = 到期

	assert.Equal(t, reconcileLoopAudit, step.loop)
	assert.Equal(t, reconcileResultOK, step.result)
	assert.Equal(t, 1, step.report.repaired)
	assert.Equal(t, 1, step.report.actions[reconcileActionAuditRepaired])
	assert.Equal(t, []string{"9940"}, indexMembers(t, mr, idxGateA))
	last, _ := mr.Get(gateIndexLastAuditKey)
	assert.Equal(t, strconv.FormatInt(clock.Now().Unix(), 10), last)
}

// 审计时机以控制域里的上次审计时刻为准:换一个新的 reconciler(换主)不重置计时,到期才跑。
func TestAudit_TimingPersistedAcrossLeaders(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	setReadyMarker(t, mr, 100*time.Second)
	clock := newTestClock()
	require.NoError(t, mr.Set(gateIndexLastAuditKey, strconv.FormatInt(clock.Now().Unix()-100, 10)))
	rec := newCommandRecorder()
	sc.PlayerRedis.AddHook(rec)

	first := newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, testReconcilerDeps(newFakeClusterView(), clock))
	assert.Equal(t, "", first.repairStep(context.Background()).loop)

	clock.Advance(time.Duration(86400-100-1) * time.Second)
	next := newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, testReconcilerDeps(newFakeClusterView(), clock))
	assert.Equal(t, "", next.repairStep(context.Background()).loop, "差 1 秒未到期")
	assert.Equal(t, 0, rec.count("scan"))

	clock.Advance(time.Second)
	step := next.repairStep(context.Background())
	assert.Equal(t, reconcileLoopAudit, step.loop)
	assert.Positive(t, rec.count("scan"))
	last, _ := mr.Get(gateIndexLastAuditKey)
	assert.Equal(t, strconv.FormatInt(clock.Now().Unix(), 10), last)
}

// 审计对**存活** gate 的索引也按字节守卫清脏成员;真成员不动。
func TestAuditPass_PrunesDirtyMembersOnLiveGates(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9950, 100, idxGateA, 1)))
	require.NoError(t, callSetSession(sc, indexTestSession(9951, 100, idxGateB, 1)))
	before := readSessionBytes(t, sc, 9950)
	seedIndexMember(t, mr, idxGateA, 9951)
	seedIndexMember(t, mr, idxGateA, 9952)
	seedRawIndexMember(t, mr, redisslot.PlayerSlot(9950), idxGateA, "junk")
	setReadyMarker(t, mr, 100*time.Second)
	r := newTestReconciler(t, sc, newFakeClusterView(idxGateA, idxGateB))

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileLoopAudit, step.loop)
	assert.Equal(t, []string{"9950"}, indexMembers(t, mr, idxGateA))
	assert.Equal(t, []string{"9951"}, indexMembers(t, mr, idxGateB))
	assert.Equal(t, before, readSessionBytes(t, sc, 9950))
	assert.Equal(t, 1, step.report.actions[reconcileActionPrunedStale])
	assert.Equal(t, 1, step.report.actions[reconcileActionPrunedMissing])
	assert.Equal(t, 1, step.report.actions[reconcileActionPrunedBadMember])
	assert.Empty(t, leaseMembers(t, mr), "审计不转换会话")
}

// 登记集里有 gate、索引已经空了(摘脏成员的路径不收紧登记集):审计把它移出登记集,不留到 gate 死。
func TestAuditPass_RemovesEmptyIndexFromRegistry(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	slot := redisslot.PlayerSlot(9953)
	_, err := mr.SetAdd(gateRegistryKey(slot), idxGateA)
	require.NoError(t, err)
	setReadyMarker(t, mr, 100*time.Second)
	r := newTestReconciler(t, sc, newFakeClusterView(idxGateA))

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileLoopAudit, step.loop)
	assert.Equal(t, reconcileResultOK, step.result)
	assert.Empty(t, registryMembers(t, mr))
}

// last_audit 比本 leader 的时钟超前一个审计周期以上:只可能是写入者的时钟坏了(例如 NTP 故障快了几天)。当坏值处理、
// 照常审计;否则此后所有 leader 都算出负数,审计停摆到各自时钟追上为止。偏差在一个周期以内的照常比较。
func TestAudit_LastAuditFarInFutureTreatedAsDue(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	setReadyMarker(t, mr, 100*time.Second)
	clock := newTestClock()
	r := newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, testReconcilerDeps(newFakeClusterView(), clock))
	now := clock.Now().Unix()

	require.NoError(t, mr.Set(gateIndexLastAuditKey, strconv.FormatInt(now+3600, 10)))
	assert.Equal(t, "", r.repairStep(context.Background()).loop, "偏差小于审计周期:照常比较,未到期")

	require.NoError(t, mr.Set(gateIndexLastAuditKey, strconv.FormatInt(now+86400+3600, 10)))
	step := r.repairStep(context.Background())
	assert.Equal(t, reconcileLoopAudit, step.loop)
	assert.Equal(t, reconcileResultOK, step.result)
	last, _ := mr.Get(gateIndexLastAuditKey)
	assert.Equal(t, strconv.FormatInt(now, 10), last, "审计完成后改写成本 leader 的时刻")
}

// 补建一遍内处理一页失败(一次抖动):退避后重做这一页,这一遍照常完成并设上就绪标记,不作废几个小时的进度。SCAN 不重做。
func TestRepairPass_RetriesFailedBackfillPage(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	writeRawSession(t, mr, indexTestSession(9970, 100, idxGateA, 1))
	rec := newCommandRecorder()
	var mgets atomic.Int32
	rec.fail = func(cmd redis.Cmder) error {
		if cmd.Name() == "mget" && mgets.Add(1) == 1 {
			return errors.New("i/o timeout")
		}
		return nil
	}
	sc.PlayerRedis.AddHook(rec)
	var sleeps []time.Duration
	deps := testReconcilerDeps(newFakeClusterView(idxGateA), newTestClock())
	deps.sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return ctx.Err()
	}
	r := newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, deps)

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileResultOK, step.result)
	assert.True(t, mr.Exists(gateIndexReadyKey))
	assert.Equal(t, 1, rec.count("scan"), "重做的是这一页,不是 SCAN")
	assert.Equal(t, []time.Duration{passPageRetryBase}, sleeps)
	assert.Equal(t, 1, step.report.pageRetries)
	assert.Equal(t, []string{"9970"}, indexMembers(t, mr, idxGateA))
}

// SCAN 本身失败:ScanEachMaster 按 fail-closed 让整遍失败,不设就绪标记,下一拍重来。
func TestRepairPass_ScanFailureFailsThePass(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	writeRawSession(t, mr, indexTestSession(9971, 100, idxGateA, 1))
	rec := newCommandRecorder()
	rec.fail = func(cmd redis.Cmder) error {
		if cmd.Name() == "scan" {
			return errors.New("i/o timeout")
		}
		return nil
	}
	sc.PlayerRedis.AddHook(rec)
	r := newTestReconciler(t, sc, newFakeClusterView(idxGateA))

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileResultRedisError, step.result)
	assert.False(t, mr.Exists(gateIndexReadyKey))
	assert.Equal(t, 1, rec.count("scan"))
}

// 审计(或补建)第二阶段的第二页 SSCAN 失败一次:按同一个非零游标重试,这一遍照常完成并记下审计时刻。
func TestAuditPass_RetriesFailedIndexPageWithSameCursor(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	for _, pid := range pidsInSameSlot(9975, 5) {
		require.NoError(t, callSetSession(sc, indexTestSession(pid, 100, idxGateA, 1)))
	}
	setReadyMarker(t, mr, 100*time.Second)
	cfg := testReconcileConf()
	cfg.ScanCount = 2
	rec := newCommandRecorder()
	var sscans atomic.Int32
	rec.fail = func(cmd redis.Cmder) error {
		if cmd.Name() == "sscan" && sscans.Add(1) == 2 {
			return errors.New("read: connection reset by peer")
		}
		return nil
	}
	sc.PlayerRedis.AddHook(rec)
	clock := newTestClock()
	r := newSessionReconciler(sc, cfg, testReconcileInterval, testReconcilerDeps(newFakeClusterView(idxGateA), clock))

	step := r.repairStep(context.Background()) // last_audit 缺失 = 到期

	assert.Equal(t, reconcileLoopAudit, step.loop)
	assert.Equal(t, reconcileResultOK, step.result)
	cursors := rec.sscanCursors()
	require.GreaterOrEqual(t, len(cursors), 3)
	assert.Equal(t, []string{"0", "2", "2"}, cursors[:3], "第二页失败后按同一个非零游标重试")
	assert.Equal(t, 1, step.report.pageRetries)
	last, _ := mr.Get(gateIndexLastAuditKey)
	assert.Equal(t, strconv.FormatInt(clock.Now().Unix(), 10), last)
	assert.Len(t, indexMembers(t, mr, idxGateA), 5, "审计不动真成员")
}

// 持续失败:同一页按指数退避重试 passPageMaxAttempts 次后放弃这一遍(redis_error),不设就绪标记;
// 补建循环一拍后再来一遍。
func TestRepairPass_GivesUpAfterMaxAttempts(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	writeRawSession(t, mr, indexTestSession(9985, 100, idxGateA, 1))
	rec := newCommandRecorder()
	rec.fail = func(cmd redis.Cmder) error {
		if cmd.Name() == "mget" {
			return errors.New("i/o timeout")
		}
		return nil
	}
	sc.PlayerRedis.AddHook(rec)
	var sleeps []time.Duration
	deps := testReconcilerDeps(newFakeClusterView(idxGateA), newTestClock())
	deps.sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return ctx.Err()
	}
	r := newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, deps)

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileResultRedisError, step.result)
	assert.False(t, mr.Exists(gateIndexReadyKey))
	assert.Equal(t, passPageMaxAttempts, rec.count("mget"))
	var want []time.Duration
	for backoff, i := passPageRetryBase, 1; i < passPageMaxAttempts; i++ {
		want = append(want, backoff)
		backoff = min(backoff*2, passPageRetryMax)
	}
	assert.Equal(t, want, sleeps, "指数退避,次数有上限")
}

// 审计失败后设冷却键(Redis 侧 EX):冷却期内审计不到期,不让持续的故障每拍重开一遍全键扫描;失败的审计不记完成时刻;
// 冷却过后照常重试。
func TestAuditPass_FailureSetsCooldownKey(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	setReadyMarker(t, mr, 100*time.Hour)
	var failing atomic.Bool
	failing.Store(true)
	rec := newCommandRecorder()
	rec.fail = func(cmd redis.Cmder) error {
		if failing.Load() && cmd.Name() == "scan" {
			return errors.New("i/o timeout")
		}
		return nil
	}
	sc.PlayerRedis.AddHook(rec)
	r := newTestReconciler(t, sc, newFakeClusterView())

	step := r.repairStep(context.Background())
	assert.Equal(t, reconcileLoopAudit, step.loop)
	assert.Equal(t, reconcileResultRedisError, step.result)
	assert.False(t, mr.Exists(gateIndexLastAuditKey), "失败的审计不记完成时刻")
	assert.True(t, mr.Exists(gateIndexAuditCooldownKey))
	assert.Equal(t, auditFailureCooldown, mr.TTL(gateIndexAuditCooldownKey))

	failing.Store(false)
	assert.Equal(t, "", r.repairStep(context.Background()).loop, "冷却期内不审计")

	mr.FastForward(auditFailureCooldown)
	step = r.repairStep(context.Background())
	assert.Equal(t, reconcileLoopAudit, step.loop)
	assert.Equal(t, reconcileResultOK, step.result)
	assert.True(t, mr.Exists(gateIndexLastAuditKey))
}

// 扫描期间拓扑或节点身份变了(主从切换、重启、slot 迁移;ScanEachMaster 返回 ErrTopologyChanged):游标没有覆盖保证,
// 这一遍作废、不设就绪标记;补进来的成员照样有效。下一遍(拓扑不再变)正常设上标记。
func TestRepairPass_TopologyChangeDiscardsPass(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	writeRawSession(t, mr, indexTestSession(9990, 100, idxGateA, 1))
	var scans atomic.Int32
	deps := testReconcilerDeps(newFakeClusterView(idxGateA), newTestClock())
	deps.scanSessions = func(ctx context.Context, c redis.UniversalClient, match string, count int64, fn func(keys []string) error) error {
		if err := testScanSessions(ctx, c, match, count, fn); err != nil {
			return err
		}
		if scans.Add(1) == 1 {
			return playerredis.ErrTopologyChanged
		}
		return nil
	}
	r := newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, deps)

	step := r.repairStep(context.Background())
	assert.Equal(t, reconcileResultRedisError, step.result)
	assert.False(t, mr.Exists(gateIndexReadyKey))
	assert.Equal(t, []string{"9990"}, indexMembers(t, mr, idxGateA), "补进来的成员照样有效")

	step = r.repairStep(context.Background())
	assert.Equal(t, reconcileResultOK, step.result)
	assert.True(t, mr.Exists(gateIndexReadyKey))
}

// 补建遍里有 ONLINE 会话因为它那把索引键类型不对而没能核对 / 补进索引:跳过它、计 member_error,这一遍照常
// 扫完(别的 gate 上漏的会话照样补),但结束时不设就绪标记(incomplete)—— 否则就是在仍有会话缺成员时宣布 I1 成立。
func TestRepairPass_SkippedCandidateBlocksReady(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	writeRawSession(t, mr, indexTestSession(9993, 100, idxGateA, 1))
	writeRawSession(t, mr, indexTestSession(9994, 100, idxGateB, 1))
	require.NoError(t, mr.Set(gateSessionIndexKey(redisslot.PlayerSlot(9993), idxGateA), "not a set"))
	r := newTestReconciler(t, sc, newFakeClusterView(idxGateA, idxGateB))

	step := r.repairStep(context.Background())

	assert.Equal(t, reconcileLoopRepair, step.loop)
	assert.Equal(t, reconcileResultIncomplete, step.result)
	assert.Equal(t, 1, step.report.skippedCandidates)
	assert.Equal(t, 1, step.report.actions[reconcileActionMemberError])
	assert.False(t, mr.Exists(gateIndexReadyKey))
	assert.Equal(t, []string{"9994"}, indexMembers(t, mr, idxGateB), "别的 gate 上漏的会话照样补")
}

// ---------------------------------------------------------------------------
// 任期:让位与生命周期
// ---------------------------------------------------------------------------

// 让补建 / 审计循环什么都不做:标记在、审计刚做过。写者检查次数因此只来自写者循环。
func quietRepairLoop(t *testing.T, mr *miniredis.Miniredis, clock *testClock) {
	t.Helper()
	setReadyMarker(t, mr, 100*time.Second)
	require.NoError(t, mr.Set(gateIndexLastAuditKey, strconv.FormatInt(clock.Now().Unix(), 10)))
}

// etcd 连续 3 拍读不出写者列表,leader 主动让位(run 返回 true)。
func TestRun_YieldsAfterConsecutiveFailures(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	view := newFakeClusterView()
	view.setWritersErr(errors.New("etcd unreachable from this pod"))
	clock := newTestClock()
	quietRepairLoop(t, mr, clock)
	deps := testReconcilerDeps(view, clock)
	deps.sleep = blockUntilDone
	r := newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, deps)

	assert.True(t, r.run(context.Background()))
	assert.Equal(t, maxConsecutiveRoundFailures, view.writerCallCount())
}

// 失败不满 3 拍、但距上次成功已超过 3 拍(每拍拖得很长):同样让位。
func TestRun_YieldsWhenNoSuccessForThreeIntervals(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	view := newFakeClusterView()
	view.setWritersErr(errors.New("etcd slow"))
	clock := newTestClock()
	quietRepairLoop(t, mr, clock)
	deps := testReconcilerDeps(view, clock)
	deps.sleep = blockUntilDone
	deps.newTicker = func(time.Duration) (func(context.Context) error, func()) {
		return func(ctx context.Context) error {
			clock.Advance(4 * testReconcileInterval)
			return ctx.Err()
		}, func() {}
	}
	r := newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, deps)

	assert.True(t, r.run(context.Background()))
	assert.Equal(t, 2, view.writerCallCount())
}

// 让位后:选主锁已被释放(健康副本立刻能抢到),本进程先等一个 TTL 再竞选。用真实选主器 + miniredis。
func TestCampaign_YieldReleasesLeaderKeyAndBacksOff(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	view := newFakeClusterView()
	view.setWritersErr(errors.New("etcd unreachable from this pod"))
	deps := testReconcilerDeps(view, newTestClock())
	deps.sleep = blockUntilDone
	elector := leader.New(leader.NewGoRedisStore(sc.RedisClient), reconcilerLeaderKey,
		leader.Options{TTL: reconcilerLeaderTTL, ID: "test-replica"})

	var heldDuringTerm atomic.Bool
	backoff := make(chan time.Duration, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		campaignSessionReconciler(ctx, elector.Run, reconcilerLeaderTTL,
			func(ctx context.Context, d time.Duration) error {
				backoff <- d
				<-ctx.Done()
				return ctx.Err()
			},
			func(lctx context.Context) bool {
				heldDuringTerm.Store(mr.Exists(reconcilerLeaderKey))
				return newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, deps).run(lctx)
			})
	}()

	select {
	case d := <-backoff:
		assert.Equal(t, reconcilerLeaderTTL, d)
	case <-time.After(10 * time.Second): // 只是防挂死的护栏,正确性不依赖它
		t.Fatal("reconciler never yielded")
	}
	assert.True(t, heldDuringTerm.Load(), "任期内持有选主锁")
	assert.False(t, mr.Exists(reconcilerLeaderKey), "让位后选主锁已释放,等待期间不再持有")

	cancel()
	<-done
}

// run 返回时补建循环已经停下:取消前补建正卡在一条 SCAN 上,run 等它返回后才返回,之后不再有在途命令。
func TestRun_WaitsForRepairLoopBeforeReturning(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9960, 100, idxGateA, 1))) // 补建要有键可扫

	var inflight atomic.Int32
	scanStarted := make(chan struct{})
	var once sync.Once
	rec := newCommandRecorder()
	rec.before = func(ctx context.Context, cmd redis.Cmder) {
		if cmd.Name() != "scan" {
			return
		}
		inflight.Add(1)
		once.Do(func() { close(scanStarted) })
		<-ctx.Done() // 模拟一页很慢的 SCAN:直到任期取消
	}
	rec.after = func(cmd redis.Cmder) {
		if cmd.Name() == "scan" {
			inflight.Add(-1)
		}
	}
	sc.PlayerRedis.AddHook(rec)

	deps := testReconcilerDeps(newFakeClusterView(idxGateA), newTestClock())
	deps.sleep = blockUntilDone
	deps.newTicker = func(time.Duration) (func(context.Context) error, func()) {
		return func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, func() {}
	}
	r := newSessionReconciler(sc, testReconcileConf(), testReconcileInterval, deps)

	lctx, cancel := context.WithCancel(context.Background())
	returned := make(chan bool, 1)
	go func() { returned <- r.run(lctx) }()

	select {
	case <-scanStarted:
	case <-time.After(10 * time.Second): // 防挂死护栏
		t.Fatal("repair loop never issued SCAN")
	}
	cancel()
	assert.False(t, <-returned, "任期取消不是让位")
	assert.Equal(t, int32(0), inflight.Load(), "run 返回时补建的 SCAN 已经返回")
}

// 两个限速器分开:转换限速给清扫,扫描限速给补建 / 审计;转换限速调低时每拍成员预算被截到半拍的量。
func TestSeparatePacersAndBudget(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	var rates []int
	deps := testReconcilerDeps(newFakeClusterView(), newTestClock())
	deps.newPacer = func(perSecond int) pacer {
		rates = append(rates, perSecond)
		return &intervalPacer{}
	}
	cfg := testReconcileConf()
	cfg.MaxTransitionsPerSecond = 100
	cfg.ScanPagesPerSecond = 7

	s := newGateSweeper(sc, registrySlotsOwner{mr}, cfg, testReconcileInterval, deps)
	r := newSessionReconciler(sc, cfg, testReconcileInterval, deps)

	assert.Equal(t, []int{100, 7}, rates)
	assert.NotSame(t, s.transitionPacer, r.scanPacer)
	assert.Equal(t, 3000, s.memberBudget, "100/s × 60s / 2")
}

// ---------------------------------------------------------------------------
// 清扫副本的进退
// ---------------------------------------------------------------------------

// 实例级的服务端错误(READONLY:主从切换中连到了只读副本;OOM、LOADING、BUSY 同理)也是 redis.Error,但不是单键问题,
// 不得按「跳过成员」吸收:整拍失败,连续 3 拍退出分片,让别的副本(可能连着新主)接手。会话一个字节都不动。
func TestSweeper_LeavesShardOnInstanceLevelRedisError(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9897, 100, idxGateA, 1)))
	before := readSessionBytes(t, sc, 9897)
	rec := newCommandRecorder()
	rec.fail = func(cmd redis.Cmder) error {
		if (cmd.Name() == "evalsha" || cmd.Name() == "eval") && cmdArg(cmd, 3) == sessionKey(9897) {
			return injectedRedisError("READONLY You can't write against a read only replica.")
		}
		return nil
	}
	sc.PlayerRedis.AddHook(rec)
	view := newFakeClusterView()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rounds := 0
	deps := testReconcilerDeps(view, newTestClock())
	deps.newTicker = func(time.Duration) (func(context.Context) error, func()) {
		return func(ctx context.Context) error {
			rounds++
			if rounds == 1+maxConsecutiveRoundFailures { // 第一拍挂嫌疑,之后连续 3 拍转换失败
				cancel()
			}
			return ctx.Err()
		}, func() {}
	}
	s := newGateSweeper(sc, registrySlotsOwner{mr}, testReconcileConf(), testReconcileInterval, deps)

	s.run(ctx)

	assert.False(t, s.eligible.Load(), "实例级错误必须计入退出门槛")
	assert.Equal(t, 1+maxConsecutiveRoundFailures, view.liveCallCount())
	assert.Equal(t, before, readSessionBytes(t, sc, 9897))
}

// etcd 连续 3 拍读不出来就退出分片;之后每拍照常试,第一次成功就回来。
func TestSweeper_LeavesShardOnEtcdFailureAndRejoinsAfterSuccess(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9898, 100, idxGateA, 1)))
	view := newFakeClusterView(idxGateA)
	view.setLiveErr(errors.New("etcd unreachable from this pod"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var s *gateSweeper
	rounds := 0
	deps := testReconcilerDeps(view, newTestClock())
	deps.newTicker = func(time.Duration) (func(context.Context) error, func()) {
		return func(ctx context.Context) error {
			rounds++
			switch rounds {
			case maxConsecutiveRoundFailures:
				assert.False(t, s.eligible.Load(), "连续失败后退出分片")
				view.setLive(idxGateA)
			case maxConsecutiveRoundFailures + 1:
				assert.True(t, s.eligible.Load(), "成功一拍就回来")
				cancel()
			default:
				assert.True(t, s.eligible.Load(), "没到门槛不退出")
			}
			return ctx.Err()
		}, func() {}
	}
	s = newGateSweeper(sc, registrySlotsOwner{mr}, testReconcileConf(), testReconcileInterval, deps)

	s.run(ctx)

	assert.Equal(t, maxConsecutiveRoundFailures+1, rounds)
}

// 失败不满 3 拍、但距上次成功已超过 3 拍(每拍拖得很长):同样退出分片。
func TestSweeper_LeavesShardWhenNoSuccessForThreeIntervals(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	require.NoError(t, callSetSession(sc, indexTestSession(9899, 100, idxGateA, 1)))
	view := newFakeClusterView()
	view.setLiveErr(errors.New("etcd slow"))
	clock := newTestClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rounds := 0
	deps := testReconcilerDeps(view, clock)
	deps.newTicker = func(time.Duration) (func(context.Context) error, func()) {
		return func(ctx context.Context) error {
			rounds++
			clock.Advance(4 * testReconcileInterval)
			if rounds == 2 {
				cancel()
			}
			return ctx.Err()
		}, func() {}
	}
	s := newGateSweeper(sc, registrySlotsOwner{mr}, testReconcileConf(), testReconcileInterval, deps)

	s.run(ctx)

	assert.False(t, s.eligible.Load())
}

// 一个副本退出之后,存活副本经分片接管它的段,并把那些 slot 上死 gate 的会话转换掉:无主段不会永远没人扫。
func TestGateSweep_SurvivorSweepsTakenOverSlots(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	const pid = uint64(9961)
	slot := redisslot.PlayerSlot(pid)
	require.NoError(t, callSetSession(sc, indexTestSession(pid, 100, idxGateA, 1)))
	clock := newTestClock()
	first := newTestShard(sc, shardRoleGateSweep, "replica-a", clock)
	second := newTestShard(sc, shardRoleGateSweep, "replica-b", clock)
	for i := 0; i < 2; i++ {
		first.cycle(ctx)
		second.cycle(ctx)
	}
	first.cycle(ctx)
	second.cycle(ctx)
	survivor := first
	if slices.Contains(first.OwnedSlots(), slot) {
		survivor = second
	}
	require.NotContains(t, survivor.OwnedSlots(), slot, "这个 slot 一开始归另一个副本")
	s := newGateSweeper(sc, survivor, testReconcileConf(), testReconcileInterval, testReconcilerDeps(newFakeClusterView(), clock))
	mustIndexRoundOK(t, s)
	rep := mustIndexRoundOK(t, s)
	assert.Empty(t, rep.deadGates, "存活副本的段上还没有这个 gate")
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, readSession(t, sc, pid).State)

	// 另一个副本不再心跳:段租约与成员登记都到期。
	mr.FastForward(shardLeaseTTL + time.Second)
	clock.Advance(shardMemberTTL + time.Second)
	survivor.cycle(ctx)
	require.Contains(t, survivor.OwnedSlots(), slot)

	mustIndexRoundOK(t, s)
	mustIndexRoundOK(t, s)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, readSession(t, sc, pid).State)
}

// ---------------------------------------------------------------------------
// 确定性序列测试:固定种子在多个 gate 间随机交错 SetSession / Reconnect / SetDisconnecting / MarkOffline /
// 清扫转换,每一步之后对全部玩家与 gate 断言 I1(以及 I2:索引非空 ⇒ gate 在本 slot 的登记集),且不落空串键。
// ---------------------------------------------------------------------------

func TestGateIndex_InvariantHoldsOverRandomSequence(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(20261018))
	// 一半同 slot、一半各在各的 slot:同 slot 才碰得到「同一把索引键多成员」「登记集该不该收紧」。
	pids := append(pidsInSameSlot(8001, 3), 8002, 8003, 8004)
	gates := []string{idxGateA, idxGateB, idxGateC, ""} // "" = 旧版 login 写的无实例号会话
	r := newTestSweeper(t, sc, mr, newFakeClusterView())

	readCurrent := func(pid uint64) *pb.PlayerSession {
		data, err := sc.PlayerRedis.Get(ctx, sessionKey(pid)).Bytes()
		if err != nil {
			return nil
		}
		s := &pb.PlayerSession{}
		require.NoError(t, proto.Unmarshal(data, s))
		return s
	}
	var nextSessionID uint32 = 1000

	for step := 0; step < 600; step++ {
		pid := pids[rng.Intn(len(pids))]
		gate := gates[rng.Intn(len(gates))]
		current := readCurrent(pid)
		nextSessionID++
		op := rng.Intn(6)
		switch op {
		case 0, 1: // SetSession:首登或顶号(偶尔故意给错版本,验证被拒时索引不动)
			version := uint32(1)
			if current != nil {
				version = current.SessionVersion + 1
				if rng.Intn(5) == 0 {
					version += 3
				}
			}
			_ = callSetSession(sc, indexTestSession(pid, nextSessionID, gate, version))
		case 2: // Reconnect(只有 DISCONNECTING 才会成功,其余被拒)
			_, err := NewReconnectLogic(ctx, sc).Reconnect(&pb.ReconnectRequest{
				PlayerId: pid, NewSessionId: nextSessionID, GateId: "2", GateInstanceId: gate,
				RequestId: fmt.Sprintf("rc-%d", step),
			})
			require.NoError(t, err)
		case 3: // SetDisconnecting:当前会话号或错号;实例号取记录值、空或别的 gate
			if current == nil {
				continue
			}
			sessionID := current.SessionId
			if rng.Intn(4) == 0 {
				sessionID++
			}
			reqGate := []string{current.GateInstanceId, "", gate}[rng.Intn(3)]
			_, err := NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
				PlayerId: pid, SessionId: sessionID, GateInstanceId: reqGate,
			})
			require.NoError(t, err)
		case 4: // MarkOffline:精确或陈旧的 expected
			if current == nil {
				continue
			}
			version := current.SessionVersion
			if rng.Intn(4) == 0 {
				version++
			}
			_, err := NewMarkOfflineLogic(ctx, sc).MarkOffline(&pb.MarkOfflineRequest{
				PlayerId: pid, ExpectedSessionId: current.SessionId, ExpectedSessionVersion: version,
				ExpectedGateInstanceId: current.GateInstanceId,
			})
			require.NoError(t, err)
		case 5: // 清扫把某个 gate 当作已确认死亡处理一遍(转换 + 摘除 + 空了就释放)
			if gate == "" {
				continue
			}
			rep := indexRoundReport{actions: map[string]int{}}
			require.NoError(t, r.processDeadGates(ctx, gateKeysOf(t, mr, gate), &rep))
		}
		assertGateIndexInvariant(t, sc, mr, pids, gates, fmt.Sprintf("step=%d op=%d pid=%d gate=%q", step, op, pid, gate))
	}
}

// assertGateIndexInvariant 断言 I1 / I2,并确认没有名为 "" 的键。
func assertGateIndexInvariant(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, pids []uint64, gates []string, where string) {
	t.Helper()
	ctx := context.Background()
	for _, pid := range pids {
		want := ""
		if data, err := sc.PlayerRedis.Get(ctx, sessionKey(pid)).Bytes(); err == nil {
			s := &pb.PlayerSession{}
			require.NoError(t, proto.Unmarshal(data, s), where)
			want = indexedGate(s)
		}
		for _, gate := range gates {
			if gate == "" {
				continue
			}
			member, err := mr.IsMember(gateSessionIndexKey(redisslot.PlayerSlot(pid), gate), strconv.FormatUint(pid, 10))
			if err != nil {
				member = false // 索引键不存在
			}
			require.Equalf(t, want == gate, member, "%s: I1 broken for player %d on gate %s (session indexed gate %q)",
				where, pid, gate, want)
		}
	}
	for _, key := range mr.Keys() {
		m := gateIndexKeyShape.FindStringSubmatch(key)
		if m == nil {
			continue
		}
		members, err := mr.Members(key)
		require.NoError(t, err, where)
		if len(members) == 0 {
			continue
		}
		registered, err := mr.IsMember(gateRegistryKey(redisslot.HashSlot(key)), m[1])
		require.Truef(t, err == nil && registered, "%s: I2 broken: %s has members but gate %s is not registered", where, key, m[1])
	}
	require.False(t, mr.Exists(""), where)
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

func TestIsKeyTypeError(t *testing.T) {
	wrongType := "WRONGTYPE Operation against a key holding the wrong kind of value"
	assert.True(t, isKeyTypeError(injectedRedisError(wrongType)), "Redis 7:错误码前缀保留")
	assert.True(t, isKeyTypeError(fmt.Errorf("prune member: %w",
		injectedRedisError("ERR Error running script (call to f_x): @user_script:2: "+wrongType))), "Redis 6:包在脚本错误里")
	for _, msg := range []string{
		"READONLY You can't write against a read only replica.",
		"LOADING Redis is loading the dataset in memory",
		"OOM command not allowed when used memory > 'maxmemory'.",
		"BUSY Redis is busy running a script.",
		"MASTERDOWN Link with MASTER is down",
	} {
		assert.False(t, isKeyTypeError(injectedRedisError(msg)), "实例级故障不得当成单键问题:%s", msg)
	}
	assert.False(t, isKeyTypeError(errors.New("dial tcp: i/o timeout "+wrongType)), "不是服务端回复的错误,文本里有这个词也不算")
	assert.False(t, isKeyTypeError(context.DeadlineExceeded))
	assert.False(t, isKeyTypeError(nil))
}

func TestClassifyIndexedMember(t *testing.T) {
	onlineA, err := proto.Marshal(indexTestSession(10, 1, idxGateA, 1))
	require.NoError(t, err)
	onlineB, err := proto.Marshal(indexTestSession(10, 1, idxGateB, 1))
	require.NoError(t, err)
	disc := indexTestSession(10, 1, idxGateA, 2)
	disc.State = pb.PlayerSessionState_SESSION_STATE_DISCONNECTING
	discA, err := proto.Marshal(disc)
	require.NoError(t, err)
	otherOwner, err := proto.Marshal(indexTestSession(11, 1, idxGateA, 1))
	require.NoError(t, err)

	cases := []struct {
		name   string
		member string
		raw    []byte
		found  bool
		want   indexedAction
	}{
		{"online on this gate", "10", onlineA, true, indexedOnlineHere},
		{"online elsewhere", "10", onlineB, true, indexedStale},
		{"disconnecting", "10", discA, true, indexedStale},
		{"session missing", "10", nil, false, indexedMissing},
		{"garbage bytes", "10", []byte{0xff, 0xff, 0xff}, true, indexedMalformed},
		{"payload player mismatch", "10", otherOwner, true, indexedMalformed},
		{"non numeric", "junk", nil, false, indexedBadMember},
		{"zero", "0", nil, false, indexedBadMember},
		{"non canonical", "010", onlineA, true, indexedBadMember},
		{"signed", "+10", onlineA, true, indexedBadMember},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, got := classifyIndexedMember(tc.member, tc.raw, tc.found, idxGateA)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCollectNodeUUIDs(t *testing.T) {
	values := [][]byte{
		gateNodeInfoWithFutureFields(reconcileGateUUIDA),
		[]byte(reconcileGateUUIDA), // allocated/ 占位
		[]byte(reconcileGateUUIDB),
	}
	assert.Equal(t, toSet([]string{reconcileGateUUIDA, reconcileGateUUIDB}), collectNodeUUIDs(values))
}

func TestCapabilityUUIDs(t *testing.T) {
	keys := []string{
		GateSessionIndexCapabilityPrefix + "loc-1",
		GateSessionIndexCapabilityPrefix, // 键尾为空
		"/mmorpg/capability/other/v1/loc-2",
		"/mmorpg/capability/player_locator/gate_session_index/v1/loc-3", // 全局索引时代的写者不算有能力
	}
	assert.Equal(t, toSet([]string{"loc-1"}), capabilityUUIDs(keys))
}

func TestLegacyWriters(t *testing.T) {
	assert.Empty(t, legacyWriters(toSet([]string{"a", "b"}), toSet([]string{"a", "b", "c"})),
		"能力键多于注册(正在退出的实例)不算旧写者")
	assert.Equal(t, []string{"b", "d"}, legacyWriters(toSet([]string{"a", "b", "d"}), toSet([]string{"a"})))
}

func TestIndexedGate(t *testing.T) {
	assert.Equal(t, "", indexedGate(nil))
	assert.Equal(t, idxGateA, indexedGate(indexTestSession(1, 1, idxGateA, 1)))
	s := indexTestSession(1, 1, idxGateA, 1)
	s.State = pb.PlayerSessionState_SESSION_STATE_DISCONNECTING
	assert.Equal(t, "", indexedGate(s))
	assert.Equal(t, "", indexedGate(indexTestSession(1, 1, "", 1)))
}
