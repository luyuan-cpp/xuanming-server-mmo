// TC8 — 按玩家存储落点选库(player-storage-placement.md §6.2)与落库后复核(§6.3)。
//
// 与 key_ordered_consumer_test.go 同一套替身:miniredis 承载排序锁 / 游标 / 重试队列 / 落点键,
// recordingHarness 替换 SQL 半程并记下落到了哪个库(recordedCall.dbName),fakeStores 按落点编号
// 发一个只有库名、没有连接的 proto2mysql 模型。共享缓存回写与读结果发布是真实代码,跑在 miniredis 上。
package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	db_config "db/internal/config"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/metrics"
	db_proto "proto/db"
	"shared/placement"

	"github.com/alicebob/miniredis/v2"
	"github.com/luyuancpp/proto2mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/protobuf/proto"
)

// testZoneID 是测试 worker 所在的 zone(§6.2 的 Z)。
const testZoneID uint32 = 1

const placementTestMsgType = "taskpb.TaskResult"

// fakeStores 是 StoreResolver 的测试替身:按落点编号懒建一个只带库名的模型,可对指定编号注入打开失败。
type fakeStores struct {
	mu     sync.Mutex
	stores map[uint32]*proto_sql.GameDB
	fail   map[uint32]error
}

func newFakeStores() *fakeStores {
	return &fakeStores{stores: map[uint32]*proto_sql.GameDB{}, fail: map[uint32]error{}}
}

func (f *fakeStores) Store(_ context.Context, storageID uint32) (*proto_sql.GameDB, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[storageID]; err != nil {
		return nil, err
	}
	if store := f.stores[storageID]; store != nil {
		return store, nil
	}
	name, ok := placement.StoreDBName(storageID)
	if !ok {
		return nil, errors.New("fake store: storage id 0")
	}
	model := proto2mysql.NewDB()
	model.DBName = name
	store := &proto_sql.GameDB{SqlModel: model}
	f.stores[storageID] = store
	return store, nil
}

func (f *fakeStores) failStore(storageID uint32, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[storageID] = err
}

// newTestPlacementRouter 与生产装配同形:落点键与 worker 共用一个 Redis(Placement.Redis 缺省)。
func newTestPlacementRouter(rc redis.Cmdable) *placementRouter {
	return &placementRouter{rc: rc, zone: testZoneID, stores: newFakeStores()}
}

func storesOf(t *testing.T, w *worker) *fakeStores {
	t.Helper()
	stores, ok := w.placement.stores.(*fakeStores)
	require.True(t, ok, "test worker must use fakeStores")
	return stores
}

func setRedisValue(t *testing.T, mr *miniredis.Miniredis, key, value string) {
	t.Helper()
	require.NoError(t, mr.Set(key, value))
}

func redisListLen(t *testing.T, w *worker, key string) int64 {
	t.Helper()
	n, err := w.redisClient.LLen(w.ctx, key).Result()
	require.NoError(t, err)
	return n
}

func appliedCursorOf(t *testing.T, w *worker, key uint64) (string, bool) {
	t.Helper()
	raw, err := w.redisClient.Get(w.ctx, appliedSeqKey(w.topic, key, placementTestMsgType)).Result()
	if errors.Is(err, redis.Nil) {
		return "", false
	}
	require.NoError(t, err)
	return raw, true
}

func cacheExists(t *testing.T, w *worker, key uint64) bool {
	t.Helper()
	n, err := w.redisClient.Exists(w.ctx, buildCacheKey(&db_proto.DBTask{Key: key, MsgType: placementTestMsgType})).Result()
	require.NoError(t, err)
	return n == 1
}

// oldestReadyTask 查看重试队列里最早的那条(RPOPLPUSH 从右端认领),不出队。
func oldestReadyTask(t *testing.T, w *worker) *db_proto.DBTask {
	t.Helper()
	payload, err := w.redisClient.LIndex(w.ctx, w.retryQueueKey, -1).Bytes()
	require.NoError(t, err)
	_, _, _, taskBytes := unwrapRetryPayload(payload)
	var task db_proto.DBTask
	require.NoError(t, proto.Unmarshal(taskBytes, &task))
	return &task
}

// claimRetry 走生产的 consumeOneRetryTask 认领一条重试(ready → processing、RetryCount+1、按原始分区派发),
// 返回派发到 worker 的任务;认领时就被判进死信(超过重试次数)的返回 nil。
func claimRetry(t *testing.T, w *worker) *workerTask {
	t.Helper()
	c := &KeyOrderedKafkaConsumer{
		redisClient:        w.redisClient,
		topic:              w.topic,
		partitionCount:     w.partition + 1,
		workers:            map[int32]*worker{w.partition: w},
		ctx:                w.ctx,
		retryQueueKey:      w.retryQueueKey,
		retryProcessingKey: w.retryProcessingKey,
		retryDeadQueueKey:  w.retryDeadQueueKey,
		retryMaxTimes:      3,
	}
	require.True(t, c.consumeOneRetryTask(), "ready queue must hold a claimable retry")
	select {
	case task := <-w.taskCh:
		return task
	default:
		return nil
	}
}

func readTaskResult(t *testing.T, w *worker, taskID string) (*db_proto.TaskResult, bool) {
	t.Helper()
	raw, err := w.redisClient.LIndex(w.ctx, "task:result:"+taskID, 0).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false
	}
	require.NoError(t, err)
	var result db_proto.TaskResult
	require.NoError(t, proto.Unmarshal(raw, &result))
	return &result, true
}

func stableRecord(storageID uint32, version uint64) placement.Record {
	return placement.Record{StorageID: storageID, Version: version}
}

func frozenRecord(storageID uint32, version uint64, runID string) placement.Record {
	return placement.Record{StorageID: storageID, Version: version, Frozen: true, RunID: runID}
}

func withRecord(rec placement.Record) placementSnapshot {
	return placementSnapshot{rec: rec, recPresent: true}
}

func (s placementSnapshot) withHome(zone uint32) placementSnapshot {
	s.home, s.homePresent = zone, true
	return s
}

// ---------------------------------------------------------------------------
// §6.2 判定表(纯函数)
// ---------------------------------------------------------------------------

func TestDecideWritePlacement_EveryCellOfTheTable(t *testing.T) {
	const z = testZoneID
	cases := []struct {
		name        string
		snap        placementSnapshot
		required    bool
		wantOutcome string
		wantStorage uint32
	}{
		{name: "home elsewhere is a stale topic (P-6)", snap: placementSnapshot{}.withHome(2),
			wantOutcome: metrics.PlacementStaleTopic},
		{name: "stale topic wins over frozen: a pre-merge write must not land anywhere",
			snap: withRecord(frozenRecord(102, 3, "r1")).withHome(2), wantOutcome: metrics.PlacementStaleTopic},
		{name: "stale topic wins over a stable record", snap: withRecord(stableRecord(1000000, 2)).withHome(2),
			wantOutcome: metrics.PlacementStaleTopic},
		{name: "stale topic wins over missing_required", snap: placementSnapshot{}.withHome(2), required: true,
			wantOutcome: metrics.PlacementStaleTopic},
		{name: "frozen record defers the write", snap: withRecord(frozenRecord(102, 3, "r1")).withHome(z),
			wantOutcome: metrics.PlacementFrozenDeferred, wantStorage: 102},
		{name: "frozen record without home defers the write", snap: withRecord(frozenRecord(1, 1, "r1")),
			wantOutcome: metrics.PlacementFrozenDeferred, wantStorage: 1},
		{name: "stable record places into its storage", snap: withRecord(stableRecord(1000000, 2)).withHome(z),
			wantOutcome: metrics.PlacementPlaced, wantStorage: 1000000},
		{name: "merged player pinned to the source zone's database", snap: withRecord(stableRecord(102, 1)).withHome(z),
			wantOutcome: metrics.PlacementPlaced, wantStorage: 102},
		{name: "record satisfies Required", snap: withRecord(stableRecord(102, 1)), required: true,
			wantOutcome: metrics.PlacementPlaced, wantStorage: 102},
		{name: "no record under Required with home", snap: placementSnapshot{}.withHome(z), required: true,
			wantOutcome: metrics.PlacementMissingRequired},
		{name: "no record under Required without home", snap: placementSnapshot{}, required: true,
			wantOutcome: metrics.PlacementMissingRequired},
		{name: "no record falls back to home (== Z)", snap: placementSnapshot{}.withHome(z),
			wantOutcome: metrics.PlacementHome, wantStorage: z},
		{name: "nothing at all is the legacy route to Z", snap: placementSnapshot{},
			wantOutcome: metrics.PlacementLegacy, wantStorage: z},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideWritePlacement(tc.snap, z, tc.required)
			assert.Equal(t, tc.wantOutcome, got.outcome)
			assert.Equal(t, tc.wantStorage, got.storageID)
		})
	}
}

func TestDecideReadPlacement_EveryCellOfTheRules(t *testing.T) {
	const z = testZoneID
	cases := []struct {
		name        string
		snap        placementSnapshot
		required    bool
		wantOutcome string
		wantStorage uint32
	}{
		{name: "frozen record is read from its source storage", snap: withRecord(frozenRecord(102, 3, "r1")).withHome(2),
			wantOutcome: metrics.PlacementPlaced, wantStorage: 102},
		{name: "stable record", snap: withRecord(stableRecord(1000000, 1)),
			wantOutcome: metrics.PlacementPlaced, wantStorage: 1000000},
		{name: "visitor from another zone reads its home, never stale", snap: placementSnapshot{}.withHome(2),
			wantOutcome: metrics.PlacementHome, wantStorage: 2},
		{name: "nothing at all reads Z", snap: placementSnapshot{},
			wantOutcome: metrics.PlacementLegacy, wantStorage: z},
		{name: "no record under Required fails", snap: placementSnapshot{}.withHome(2), required: true,
			wantOutcome: metrics.PlacementMissingRequired},
		{name: "record satisfies Required", snap: withRecord(stableRecord(102, 1)), required: true,
			wantOutcome: metrics.PlacementPlaced, wantStorage: 102},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideReadPlacement(tc.snap, z, tc.required)
			assert.Equal(t, tc.wantOutcome, got.outcome)
			assert.Equal(t, tc.wantStorage, got.storageID)
		})
	}
}

func TestParsePlacementSnapshot(t *testing.T) {
	t.Run("both keys absent", func(t *testing.T) {
		snap, err := parsePlacementSnapshot([]interface{}{nil, nil})
		require.NoError(t, err)
		assert.Equal(t, placementSnapshot{}, snap)
	})
	t.Run("record and home present", func(t *testing.T) {
		snap, err := parsePlacementSnapshot([]interface{}{"102:3:frozen:run-a", "101"})
		require.NoError(t, err)
		assert.Equal(t, withRecord(frozenRecord(102, 3, "run-a")).withHome(101), snap)
	})
	t.Run("empty home value follows the shared contract: absent", func(t *testing.T) {
		snap, err := parsePlacementSnapshot([]interface{}{nil, ""})
		require.NoError(t, err)
		assert.False(t, snap.homePresent)
	})
	// 畸形必须 fail-closed,绝不能当缺席:缺席会让玩家回落 home / Z 选库。
	for _, vals := range [][]interface{}{
		{"", nil},
		{"102", nil},
		{"0:1", nil},
		{"102:1:moving:run", nil},
		{nil, "abc"},
		{nil, "0"},
		{nil, "1000000"}, // home 必须是 zone 落点编号,否则派生出的是 player_store_* 而不是这个 zone 的库
	} {
		_, err := parsePlacementSnapshot(vals)
		assert.ErrorIs(t, err, placement.ErrMalformed, "values %v must be malformed", vals)
	}
	_, err := parsePlacementSnapshot([]interface{}{int64(1), nil})
	assert.Error(t, err, "an unexpected reply type is an error, not an absent key")
	_, err = parsePlacementSnapshot([]interface{}{nil})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// §6.3 复核表(纯函数)
// ---------------------------------------------------------------------------

func TestRecheckPasses_Table(t *testing.T) {
	const z = testZoneID
	s := withRecord(stableRecord(102, 3))
	cases := []struct {
		name      string
		selected  placementSnapshot
		current   placementSnapshot
		wantWrite bool
		wantRead  bool
	}{
		{name: "stable -> same stable", selected: s, current: s, wantWrite: true, wantRead: true},
		{name: "stable -> same stable, home changed (record decides the route)", selected: s.withHome(z),
			current: s.withHome(2), wantWrite: true, wantRead: true},
		{name: "stable -> frozen same route: write retries, read passes", selected: s,
			current: withRecord(frozenRecord(102, 3, "r1")), wantWrite: false, wantRead: true},
		{name: "stable -> switched to another storage", selected: s,
			current: withRecord(stableRecord(1000000, 4)), wantWrite: false, wantRead: false},
		{name: "stable -> version bump on the same storage (relocated away and back)", selected: s,
			current: withRecord(stableRecord(102, 5)), wantWrite: false, wantRead: false},
		{name: "record disappeared", selected: s, current: placementSnapshot{}, wantWrite: false, wantRead: false},
		{name: "record appeared", selected: placementSnapshot{}, current: withRecord(stableRecord(z, 1)),
			wantWrite: false, wantRead: false},
		{name: "no record -> no record", selected: placementSnapshot{}.withHome(z), current: placementSnapshot{}.withHome(z),
			wantWrite: true, wantRead: true},
		{name: "no record, home appears as Z: route unchanged", selected: placementSnapshot{},
			current: placementSnapshot{}.withHome(z), wantWrite: true, wantRead: true},
		// 设计 §6.3 只比记录;无记录玩家的库由 home 决定,home 变了就是路由变了(见 sameRoute 注释)。
		{name: "no record, home moved elsewhere", selected: placementSnapshot{}.withHome(z),
			current: placementSnapshot{}.withHome(2), wantWrite: false, wantRead: false},
		{name: "read of a frozen record after unfreeze without switch", selected: withRecord(frozenRecord(102, 3, "r1")),
			current: s, wantWrite: true, wantRead: true},
		{name: "read of a frozen record, still frozen by another run", selected: withRecord(frozenRecord(102, 3, "r1")),
			current: withRecord(frozenRecord(102, 3, "r2")), wantWrite: false, wantRead: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantWrite, recheckPasses("write", tc.selected, tc.current, z), "write")
			assert.Equal(t, tc.wantRead, recheckPasses("read", tc.selected, tc.current, z), "read")
		})
	}
}

// ---------------------------------------------------------------------------
// §6.2 写任务:每一格走完整的 handleTask
// ---------------------------------------------------------------------------

func TestHandleTask_WritePlacementCells(t *testing.T) {
	cases := []struct {
		name       string
		record     string // "" = 不写 player:placement
		home       string // "" = 不写 player:zone
		required   bool
		wantDB     string // "" = SQL 不能执行
		wantReady  int64
		wantDead   int64
		wantCursor bool
	}{
		{name: "placed into the global store", record: "1000000:2", home: "1",
			wantDB: "player_store_1000000_db", wantCursor: true},
		{name: "placed into a merged source zone's database", record: "102:1", home: "1",
			wantDB: "zone_102_db", wantCursor: true},
		{name: "home", home: "1", wantDB: "zone_1_db", wantCursor: true},
		{name: "legacy", wantDB: "zone_1_db", wantCursor: true},
		{name: "stale topic goes to the dead queue", home: "2", wantDead: 1},
		{name: "frozen record defers", record: "1000000:2:frozen:run-a", home: "1", wantReady: 1},
		{name: "missing record under Required goes to the dead queue", home: "1", required: true, wantDead: 1},
		{name: "malformed record is a lookup error, never absent", record: "garbage", home: "1", wantReady: 1},
		{name: "malformed home is a lookup error", home: "0", wantReady: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, mr, h := newTestWorker(t, 0)
			w.placement.required = tc.required
			const key uint64 = 9001
			if tc.record != "" {
				setRedisValue(t, mr, placement.Key(key), tc.record)
			}
			if tc.home != "" {
				setRedisValue(t, mr, placement.HomeZoneKey(key), tc.home)
			}

			wt := makeWriteTask(t, key, placementTestMsgType, 10)
			session := attachKafkaClaim(t, w, wt)
			w.handleTask(wt, true)

			calls := h.callsForKey(key)
			if tc.wantDB == "" {
				assert.Empty(t, calls, "SQL must not run")
			} else {
				require.Len(t, calls, 1)
				assert.Equal(t, tc.wantDB, calls[0].dbName)
			}
			assert.Equal(t, tc.wantReady, redisListLen(t, w, w.retryQueueKey), "ready queue")
			assert.Equal(t, tc.wantDead, redisListLen(t, w, w.retryDeadQueueKey), "dead queue")
			_, hasCursor := appliedCursorOf(t, w, key)
			assert.Equal(t, tc.wantCursor, hasCursor, "applied cursor")
			assert.Equal(t, tc.wantCursor, cacheExists(t, w, key), "shared cache is published only for applied writes")
			assert.Equal(t, []int64{9}, session.marked(), "every outcome ends with the Kafka offset ACKed after a durable copy")
		})
	}
}

// Placement.Redis 独立于 RedisClient 时,选库只读它;它连不上 = lookup_error,延后重试(不放行、不回落)。
func TestHandleTask_PlacementRedisDownIsALookupError(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	placementRedis := miniredis.RunT(t)
	prc := redis.NewClient(&redis.Options{Addr: placementRedis.Addr()})
	t.Cleanup(func() { _ = prc.Close() })
	w.placement.rc = prc
	placementRedis.Close()

	const key uint64 = 9002
	w.handleTask(makeWriteTask(t, key, placementTestMsgType, 10), true)

	assert.Empty(t, h.callsForKey(key), "unknown placement must not reach MySQL")
	assert.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))
	_, hasCursor := appliedCursorOf(t, w, key)
	assert.False(t, hasCursor)
}

// 装配缺失 fail-closed:绝不回落到本 zone 库。
func TestHandleTask_MissingPlacementRouterFailsClosed(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	w.placement = nil
	const key uint64 = 9003
	w.handleTask(makeWriteTask(t, key, placementTestMsgType, 10), true)

	assert.Empty(t, h.callsForKey(key))
	assert.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))
}

// ---------------------------------------------------------------------------
// §6.2 读任务
// ---------------------------------------------------------------------------

func TestHandleTask_ReadPlacementCells(t *testing.T) {
	cases := []struct {
		name   string
		record string
		home   string
		wantDB string
	}{
		{name: "frozen record is still read from its source", record: "102:3:frozen:run-a", home: "2",
			wantDB: "zone_102_db"},
		{name: "global store", record: "1000000:1", wantDB: "player_store_1000000_db"},
		// §2.3 的修复:访客从别区登录,预加载发到本 zone 的 topic,读的却必须是 home 库。
		{name: "visitor reads its home zone database", home: "2", wantDB: "zone_2_db"},
		{name: "legacy", wantDB: "zone_1_db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, mr, h := newTestWorker(t, 0)
			const key uint64 = 9101
			if tc.record != "" {
				setRedisValue(t, mr, placement.Key(key), tc.record)
			}
			if tc.home != "" {
				setRedisValue(t, mr, placement.HomeZoneKey(key), tc.home)
			}
			rt := makeReadTask(t, key, placementTestMsgType, "preload")
			rt.seq = 5
			session := attachKafkaClaim(t, w, rt)
			w.handleTask(rt, true)

			calls := h.callsForKey(key)
			require.Len(t, calls, 1)
			assert.Equal(t, tc.wantDB, calls[0].dbName)
			result, ok := readTaskResult(t, w, rt.dbTask.TaskId)
			require.True(t, ok, "the read result must be published")
			assert.True(t, result.Success)
			assert.True(t, cacheExists(t, w, key))
			assert.Equal(t, []int64{4}, session.marked())
		})
	}
}

// Required=true 且无记录:读「任务失败」= 立刻回 Success=false,不重试、不进死信、不碰 MySQL。
func TestHandleTask_ReadMissingRequiredAnswersWithFailure(t *testing.T) {
	w, mr, h := newTestWorker(t, 0)
	w.placement.required = true
	const key uint64 = 9102
	setRedisValue(t, mr, placement.HomeZoneKey(key), "2")

	rt := makeReadTask(t, key, placementTestMsgType, "preload")
	rt.seq = 5
	session := attachKafkaClaim(t, w, rt)
	w.handleTask(rt, true)

	assert.Empty(t, h.callsForKey(key))
	result, ok := readTaskResult(t, w, rt.dbTask.TaskId)
	require.True(t, ok, "the waiter must get an answer instead of timing out")
	assert.False(t, result.Success)
	assert.True(t, strings.Contains(result.Error, metrics.PlacementMissingRequired), "error: %s", result.Error)
	assert.Zero(t, redisListLen(t, w, w.retryQueueKey))
	assert.Zero(t, redisListLen(t, w, w.retryDeadQueueKey))
	assert.False(t, cacheExists(t, w, key))
	assert.Equal(t, []int64{4}, session.marked())
}

func TestHandleTask_ReadLookupErrorIsRetried(t *testing.T) {
	w, mr, h := newTestWorker(t, 0)
	const key uint64 = 9103
	setRedisValue(t, mr, placement.Key(key), "not-a-record")
	rt := makeReadTask(t, key, placementTestMsgType, "preload")
	rt.seq = 5
	w.handleTask(rt, true)

	assert.Empty(t, h.callsForKey(key))
	assert.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))
	_, ok := readTaskResult(t, w, rt.dbTask.TaskId)
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// 重试预算:冻结 / 落点库不可用(写)不耗预算,查询失败照常耗
// ---------------------------------------------------------------------------

func TestFrozenDeferral_NeverExhaustsTheRetryBudget(t *testing.T) {
	w, mr, h := newTestWorker(t, 0)
	const key uint64 = 9201
	setRedisValue(t, mr, placement.Key(key), "102:3:frozen:run-a")
	setRedisValue(t, mr, placement.HomeZoneKey(key), "1")

	task := makeWriteTask(t, key, placementTestMsgType, 10).dbTask
	task.RetryCount = 2 // 再耗一次就会在认领时进死信
	require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, task, 10, 0))

	for round := 0; round < 5; round++ {
		claimed := claimRetry(t, w)
		require.NotNil(t, claimed, "round %d: a frozen write must never be dead-lettered", round)
		w.handleTask(claimed, true)
		assert.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey), "round %d", round)
		assert.Zero(t, redisListLen(t, w, w.retryProcessingKey), "round %d", round)
		assert.Zero(t, redisListLen(t, w, w.retryDeadQueueKey), "round %d", round)
		assert.Equal(t, int32(2), oldestReadyTask(t, w).RetryCount, "round %d: freezing is not a failed attempt", round)
	}
	assert.Empty(t, h.callsForKey(key))

	// 搬库切换后,同一条写落到新库。
	setRedisValue(t, mr, placement.Key(key), "1000000:4")
	claimed := claimRetry(t, w)
	require.NotNil(t, claimed)
	w.handleTask(claimed, true)
	calls := h.callsForKey(key)
	require.Len(t, calls, 1)
	assert.Equal(t, "player_store_1000000_db", calls[0].dbName)
	cursor, ok := appliedCursorOf(t, w, key)
	require.True(t, ok)
	assert.Equal(t, "v2:0:10", cursor)
	assert.Zero(t, redisListLen(t, w, w.retryQueueKey))
	assert.Zero(t, redisListLen(t, w, w.retryProcessingKey))
}

// 对照组:值畸形是查询失败,照常耗预算,几次后进死信由人处理。
func TestPlacementLookupError_ConsumesTheRetryBudget(t *testing.T) {
	w, mr, h := newTestWorker(t, 0)
	const key uint64 = 9202
	setRedisValue(t, mr, placement.Key(key), "garbage")

	task := makeWriteTask(t, key, placementTestMsgType, 10).dbTask
	task.RetryCount = 2
	require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, task, 10, 0))

	claimed := claimRetry(t, w)
	require.NotNil(t, claimed)
	w.handleTask(claimed, true)
	assert.Equal(t, int32(3), oldestReadyTask(t, w).RetryCount, "a failed lookup consumes one attempt")

	assert.Nil(t, claimRetry(t, w), "the exhausted task is dead-lettered at claim time")
	assert.Equal(t, int64(1), redisListLen(t, w, w.retryDeadQueueKey))
	assert.Empty(t, h.callsForKey(key))
}

func TestStoreUnavailable_WriteKeepsBudgetReadSpendsIt(t *testing.T) {
	const key uint64 = 9203
	openErr := errors.New("dial tcp 10.0.0.9:3306: connect: connection refused")

	t.Run("write", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		storesOf(t, w).failStore(1000000, openErr)
		setRedisValue(t, mr, placement.Key(key), "1000000:1")

		wt := makeWriteTask(t, key, placementTestMsgType, 10)
		wt.fromRetry = true
		wt.dbTask.RetryCount = 3 // 认领时已 +1
		wt.retryReceipt = []byte("receipt-store-down-write")
		require.NoError(t, w.redisClient.LPush(w.ctx, w.retryProcessingKey, wt.retryReceipt).Err())
		w.handleTask(wt, true)

		assert.Empty(t, h.callsForKey(key))
		assert.Zero(t, redisListLen(t, w, w.retryProcessingKey))
		assert.Equal(t, int32(2), oldestReadyTask(t, w).RetryCount, "an unopenable store is not a failed write attempt")
	})

	t.Run("read", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		storesOf(t, w).failStore(1000000, openErr)
		setRedisValue(t, mr, placement.Key(key), "1000000:1")

		rt := makeReadTask(t, key, placementTestMsgType, "preload")
		rt.seq = 7
		rt.fromRetry = true
		rt.dbTask.RetryCount = 3
		rt.retryReceipt = []byte("receipt-store-down-read")
		require.NoError(t, w.redisClient.LPush(w.ctx, w.retryProcessingKey, rt.retryReceipt).Err())
		w.handleTask(rt, true)

		assert.Empty(t, h.callsForKey(key))
		assert.Equal(t, int32(3), oldestReadyTask(t, w).RetryCount,
			"reads spend the budget: the waiter has long timed out and an endless read loop only crowds the retry queue")
	})
}

// ---------------------------------------------------------------------------
// §6.3 落库后复核:SQL 已执行、复核之前记录被改
// ---------------------------------------------------------------------------

func TestPostWriteRecheck(t *testing.T) {
	const key uint64 = 9301

	t.Run("stable stays stable: pass", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		setRedisValue(t, mr, placement.Key(key), "102:3")
		wt := makeWriteTask(t, key, placementTestMsgType, 10)
		session := attachKafkaClaim(t, w, wt)
		w.handleTask(wt, true)

		require.Len(t, h.callsForKey(key), 1)
		cursor, ok := appliedCursorOf(t, w, key)
		require.True(t, ok)
		assert.Equal(t, "v2:0:10", cursor)
		assert.True(t, cacheExists(t, w, key))
		assert.Zero(t, redisListLen(t, w, w.retryQueueKey))
		assert.Equal(t, []int64{9}, session.marked())
	})

	t.Run("stable then frozen: retry without cursor, then lands on the new store after the switch", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		setRedisValue(t, mr, placement.Key(key), "102:3")
		// 搬库工具在排序锁过期后冻结并拷完源库:这笔写落进源库时已经赶不上拷贝了。
		h.setAfterCall(func(recordedCall) { setRedisValue(t, mr, placement.Key(key), "102:3:frozen:run-a") })
		wt := makeWriteTask(t, key, placementTestMsgType, 10)
		session := attachKafkaClaim(t, w, wt)
		w.handleTask(wt, true)

		require.Len(t, h.callsForKey(key), 1, "the SQL did execute on the source")
		_, hasCursor := appliedCursorOf(t, w, key)
		assert.False(t, hasCursor, "a write whose copy cannot be proven must not publish its cursor")
		assert.False(t, cacheExists(t, w, key), "nor the shared cache")
		assert.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))
		assert.Zero(t, oldestReadyTask(t, w).RetryCount, "the recheck retry does not consume the budget")
		assert.Equal(t, []int64{9}, session.marked(), "Kafka ACKs only after the retry copy is durable")

		h.setAfterCall(nil)
		setRedisValue(t, mr, placement.Key(key), "1000000:4")
		claimed := claimRetry(t, w)
		require.NotNil(t, claimed)
		w.handleTask(claimed, true)

		calls := h.callsForKey(key)
		require.Len(t, calls, 2)
		assert.Equal(t, "zone_102_db", calls[0].dbName)
		assert.Equal(t, "player_store_1000000_db", calls[1].dbName)
		cursor, ok := appliedCursorOf(t, w, key)
		require.True(t, ok)
		assert.Equal(t, "v2:0:10", cursor)
		assert.True(t, cacheExists(t, w, key))
		assert.Zero(t, redisListLen(t, w, w.retryQueueKey))
		assert.Zero(t, redisListLen(t, w, w.retryProcessingKey))
	})

	t.Run("switched between SQL and recheck: retry lands on the new store", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		setRedisValue(t, mr, placement.Key(key), "102:3")
		h.setAfterCall(func(recordedCall) { setRedisValue(t, mr, placement.Key(key), "1000000:4") })
		w.handleTask(makeWriteTask(t, key, placementTestMsgType, 10), true)
		_, hasCursor := appliedCursorOf(t, w, key)
		assert.False(t, hasCursor)
		require.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))

		h.setAfterCall(nil)
		claimed := claimRetry(t, w)
		require.NotNil(t, claimed)
		w.handleTask(claimed, true)
		calls := h.callsForKey(key)
		require.Len(t, calls, 2)
		assert.Equal(t, "player_store_1000000_db", calls[1].dbName)
		cursor, ok := appliedCursorOf(t, w, key)
		require.True(t, ok)
		assert.Equal(t, "v2:0:10", cursor)
	})

	t.Run("record appears (pin-placement): retry", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		setRedisValue(t, mr, placement.HomeZoneKey(key), "1")
		h.setAfterCall(func(recordedCall) { setRedisValue(t, mr, placement.Key(key), "1:1") })
		w.handleTask(makeWriteTask(t, key, placementTestMsgType, 10), true)
		_, hasCursor := appliedCursorOf(t, w, key)
		assert.False(t, hasCursor)
		assert.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))

		h.setAfterCall(nil)
		claimed := claimRetry(t, w)
		require.NotNil(t, claimed)
		w.handleTask(claimed, true)
		calls := h.callsForKey(key)
		require.Len(t, calls, 2)
		assert.Equal(t, "zone_1_db", calls[1].dbName)
		_, hasCursor = appliedCursorOf(t, w, key)
		assert.True(t, hasCursor)
	})

	t.Run("no record stays no record: pass", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		setRedisValue(t, mr, placement.HomeZoneKey(key), "1")
		w.handleTask(makeWriteTask(t, key, placementTestMsgType, 10), true)
		require.Len(t, h.callsForKey(key), 1)
		_, hasCursor := appliedCursorOf(t, w, key)
		assert.True(t, hasCursor)
		assert.Zero(t, redisListLen(t, w, w.retryQueueKey))
	})

	// 无记录玩家的库由 home 决定:SQL 之后 home 被改(copy 模式合服把行拷走并改了 home),这笔写留在了
	// 已不再是真源的库里,必须重试;重投时 home ≠ Z,按 P-6 进死信由人处理,而不是静默标记成功。
	t.Run("no record but home moved: retry, then stale topic on replay", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		setRedisValue(t, mr, placement.HomeZoneKey(key), "1")
		h.setAfterCall(func(recordedCall) { setRedisValue(t, mr, placement.HomeZoneKey(key), "2") })
		w.handleTask(makeWriteTask(t, key, placementTestMsgType, 10), true)
		_, hasCursor := appliedCursorOf(t, w, key)
		assert.False(t, hasCursor)
		require.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))

		h.setAfterCall(nil)
		claimed := claimRetry(t, w)
		require.NotNil(t, claimed)
		w.handleTask(claimed, true)
		assert.Len(t, h.callsForKey(key), 1, "the replay must not land anywhere")
		assert.Equal(t, int64(1), redisListLen(t, w, w.retryDeadQueueKey))
		assert.Zero(t, redisListLen(t, w, w.retryQueueKey))
		assert.Zero(t, redisListLen(t, w, w.retryProcessingKey))
	})
}

// 复核重投与 per-key LWW:切换后更新的写已经落了新库,迟到的重投被游标判为过期丢弃。
func TestPostWriteRecheck_NewerWriteWinsOverTheReplay(t *testing.T) {
	w, mr, h := newTestWorker(t, 0)
	const key uint64 = 9302
	setRedisValue(t, mr, placement.Key(key), "102:3")
	h.setAfterCall(func(call recordedCall) {
		if extractSeqFromTaskID(call.taskID) == 10 {
			setRedisValue(t, mr, placement.Key(key), "102:3:frozen:run-a")
		}
	})
	w.handleTask(makeWriteTask(t, key, placementTestMsgType, 10), true)
	require.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))

	setRedisValue(t, mr, placement.Key(key), "1000000:4")
	w.handleTask(makeWriteTask(t, key, placementTestMsgType, 11), true)

	claimed := claimRetry(t, w)
	require.NotNil(t, claimed)
	w.handleTask(claimed, true)

	calls := h.callsForKey(key)
	require.Len(t, calls, 2, "the stale replay must be dropped by the cursor guard")
	assert.Equal(t, uint64(11), extractSeqFromTaskID(calls[1].taskID))
	assert.Equal(t, "player_store_1000000_db", calls[1].dbName)
	cursor, ok := appliedCursorOf(t, w, key)
	require.True(t, ok)
	assert.Equal(t, "v2:0:11", cursor)
	assert.Zero(t, redisListLen(t, w, w.retryProcessingKey))
}

func TestPostReadRecheck(t *testing.T) {
	const key uint64 = 9303

	t.Run("stable then frozen: the read passes", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		setRedisValue(t, mr, placement.Key(key), "102:3")
		h.setAfterCall(func(recordedCall) { setRedisValue(t, mr, placement.Key(key), "102:3:frozen:run-a") })
		rt := makeReadTask(t, key, placementTestMsgType, "preload")
		rt.seq = 5
		w.handleTask(rt, true)

		result, ok := readTaskResult(t, w, rt.dbTask.TaskId)
		require.True(t, ok, "freezing does not change the source, the read result stands")
		assert.True(t, result.Success)
		assert.True(t, cacheExists(t, w, key))
		assert.Zero(t, redisListLen(t, w, w.retryQueueKey))
	})

	t.Run("switched: the result is discarded and the read retried on the new store", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		setRedisValue(t, mr, placement.Key(key), "102:3")
		h.setAfterCall(func(recordedCall) { setRedisValue(t, mr, placement.Key(key), "1000000:4") })
		rt := makeReadTask(t, key, placementTestMsgType, "preload")
		rt.seq = 5
		w.handleTask(rt, true)

		_, ok := readTaskResult(t, w, rt.dbTask.TaskId)
		assert.False(t, ok, "a result read from the old store must never be published")
		assert.False(t, cacheExists(t, w, key), "nor written into the shared cache")
		require.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))
		assert.Zero(t, oldestReadyTask(t, w).RetryCount)

		h.setAfterCall(nil)
		claimed := claimRetry(t, w)
		require.NotNil(t, claimed)
		w.handleTask(claimed, true)
		calls := h.callsForKey(key)
		require.Len(t, calls, 2)
		assert.Equal(t, "player_store_1000000_db", calls[1].dbName)
		result, ok := readTaskResult(t, w, rt.dbTask.TaskId)
		require.True(t, ok)
		assert.True(t, result.Success)
	})
}

// ---------------------------------------------------------------------------
// 批内合并:选库发生在合并后的单条任务上,复核绕不过去
// ---------------------------------------------------------------------------

func TestCoalescedBatch_PlacementGuardsTheSurvivor(t *testing.T) {
	const key uint64 = 9401

	t.Run("frozen: superseded writes ACK, the survivor defers and later lands", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		setRedisValue(t, mr, placement.Key(key), "102:3:frozen:run-a")
		w.processTaskBatch([]*workerTask{
			makeWriteTask(t, key, placementTestMsgType, 1),
			makeWriteTask(t, key, placementTestMsgType, 2),
			makeWriteTask(t, key, placementTestMsgType, 3),
		}, true)
		assert.Empty(t, h.callsForKey(key))
		require.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey), "only the newest full snapshot needs a durable copy")

		setRedisValue(t, mr, placement.Key(key), "1000000:4")
		claimed := claimRetry(t, w)
		require.NotNil(t, claimed)
		w.handleTask(claimed, true)
		calls := h.callsForKey(key)
		require.Len(t, calls, 1)
		assert.Equal(t, uint64(3), extractSeqFromTaskID(calls[0].taskID))
		assert.Equal(t, "player_store_1000000_db", calls[0].dbName)
	})

	t.Run("recheck still runs on the survivor", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		setRedisValue(t, mr, placement.Key(key), "102:3")
		h.setAfterCall(func(recordedCall) { setRedisValue(t, mr, placement.Key(key), "102:3:frozen:run-a") })
		w.processTaskBatch([]*workerTask{
			makeWriteTask(t, key, placementTestMsgType, 1),
			makeWriteTask(t, key, placementTestMsgType, 2),
		}, true)
		calls := h.callsForKey(key)
		require.Len(t, calls, 1)
		assert.Equal(t, uint64(2), extractSeqFromTaskID(calls[0].taskID))
		_, hasCursor := appliedCursorOf(t, w, key)
		assert.False(t, hasCursor)
		assert.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))
	})

	t.Run("a read stays a barrier and every task uses the record's store", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		setRedisValue(t, mr, placement.Key(key), "102:1")
		rt := makeReadTask(t, key, placementTestMsgType, "mid")
		rt.seq = 2
		w.processTaskBatch([]*workerTask{
			makeWriteTask(t, key, placementTestMsgType, 1),
			rt,
			makeWriteTask(t, key, placementTestMsgType, 3),
		}, true)
		calls := h.callsForKey(key)
		require.Len(t, calls, 3)
		for _, c := range calls {
			assert.Equal(t, "zone_102_db", c.dbName)
		}
		_, ok := readTaskResult(t, w, rt.dbTask.TaskId)
		assert.True(t, ok)
	})
}

// ---------------------------------------------------------------------------
// 装配与能力标记
// ---------------------------------------------------------------------------

func TestNewKeyOrderedKafkaConsumer_RequiresPlacementDependencies(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	cfg := db_config.Config{ZoneId: testZoneID}

	_, err := NewKeyOrderedKafkaConsumer(cfg, rc, nil, newFakeStores())
	assert.Error(t, err, "missing placement Redis must fail before touching Kafka")
	_, err = NewKeyOrderedKafkaConsumer(cfg, rc, rc, nil)
	assert.Error(t, err, "missing store registry must fail before touching Kafka")
	_, err = NewKeyOrderedKafkaConsumer(db_config.Config{}, rc, rc, newFakeStores())
	assert.Error(t, err, "zone 0 has no database to fall back to")
}

func TestMarkPlacementCapability(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	ctx := context.Background()

	require.NoError(t, rc.Set(ctx, "db:capability:zone:7", "something-older", time.Minute).Err())
	require.NoError(t, MarkPlacementCapability(ctx, rc, 7))
	value, err := rc.Get(ctx, "db:capability:zone:7").Result()
	require.NoError(t, err)
	assert.Equal(t, placement.CapabilityRoutingV1, value)
	ttl, err := rc.TTL(ctx, "db:capability:zone:7").Result()
	require.NoError(t, err)
	assert.Equal(t, PlacementCapabilityTTL, ttl,
		"the marker must expire on its own: a permanent marker keeps vouching for a go/db that was rolled back")
	assert.Less(t, PlacementCapabilityRefreshInterval*2, PlacementCapabilityTTL,
		"two consecutive failed refreshes must not drop the marker")
	assert.Less(t, placementCapabilityWriteTimeout, PlacementCapabilityRefreshInterval)

	assert.Error(t, MarkPlacementCapability(ctx, rc, 0))
	assert.Error(t, MarkPlacementCapability(ctx, nil, 7))
	mr.Close()
	assert.Error(t, MarkPlacementCapability(ctx, rc, 7), "a Redis failure surfaces so the caller can log it")
}

// 回归(评审:能力标记无 TTL):标记是心跳。续写停下(回退到不认识这把键的旧版 / 进程退出)后它在 TTL 内消失,
// 工具不再据此放行 pin 合服 / relocate;续写中的标记跨过任意长时间都在;Redis 故障上报并在恢复后自动补写;
// 心跳停止时不主动 DEL(同 zone 其他副本可能仍在跑)。tick 由测试逐拍驱动,过期用 miniredis 的虚拟时钟。
func TestPlacementCapabilityHeartbeat(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	const zone uint32 = 7
	key := placement.CapabilityKey(zone)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticks := make(chan time.Time)
	reports := make(chan error)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPlacementCapabilityHeartbeat(ctx, rc, zone, ticks, func(err error) { reports <- err })
	}()
	beat := func() error {
		ticks <- time.Time{}
		return <-reports
	}

	// 回退场景:新版写过一次标记后不再续写,TTL 到期即消失。
	require.NoError(t, beat())
	value, err := mr.Get(key)
	require.NoError(t, err)
	assert.Equal(t, placement.CapabilityRoutingV1, value)
	mr.FastForward(PlacementCapabilityTTL)
	assert.False(t, mr.Exists(key), "a go/db that stopped refreshing (rolled back / gone) must stop vouching")

	// 持续续写:累计时间远超 TTL,标记一直在。
	require.NoError(t, beat())
	for i := 0; i < 5; i++ {
		mr.FastForward(PlacementCapabilityRefreshInterval)
		require.True(t, mr.Exists(key), "refresh %d", i)
		require.NoError(t, beat())
	}
	assert.Equal(t, PlacementCapabilityTTL, mr.TTL(key))

	// Redis 故障:续写失败上报给调用方;恢复后下一拍自动补写,不需要重启进程。
	mr.FastForward(PlacementCapabilityTTL)
	require.False(t, mr.Exists(key))
	mr.SetError("LOADING redis is loading")
	assert.Error(t, beat())
	mr.SetError("")
	require.NoError(t, beat())
	assert.True(t, mr.Exists(key), "the next beat after recovery restores the marker")

	// 停止:循环退出、不再续写,但不主动删标记。
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat must return once its context is cancelled")
	}
	assert.True(t, mr.Exists(key), "stopping one replica must not delete the zone's marker")
	mr.FastForward(PlacementCapabilityTTL)
	assert.False(t, mr.Exists(key), "after the last refresher stops the marker expires on its own")
}

func TestPlacementCapabilityHeartbeat_StopsWhenTicksClose(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPlacementCapabilityHeartbeat(context.Background(), rc, 7, ticks, func(error) {})
	}()
	close(ticks)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat must return once its tick source closes")
	}
}

// countLogLines 数 logtest 收集到的 JSON 日志里 level 与 content 同时匹配的行。
func countLogLines(t *testing.T, logs *logtest.Buffer, level, contentSubstr string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry struct {
			Level   string `json:"level"`
			Content string `json:"content"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &entry), "log line is not JSON: %s", line)
		if entry.Level == level && strings.Contains(entry.Content, contentSubstr) {
			n++
		}
	}
	return n
}

// 回归(评审:冻结延后每轮打 ERROR):重试来源的写在搬库冻结期间每轮都被重排,只打 DEBUG,与 routeTask 冻结分支
// 同口径;对照组「落点库不可用(写)」是真实基础设施故障,仍打 ERROR —— 同时证明日志收集本身是通的。
func TestRetryReschedule_FrozenLogsDebugStoreDownLogsError(t *testing.T) {
	t.Run("frozen", func(t *testing.T) {
		logs := logtest.NewCollector(t)
		w, mr, _ := newTestWorker(t, 0)
		const key uint64 = 9204
		setRedisValue(t, mr, placement.Key(key), "102:3:frozen:run-a")
		setRedisValue(t, mr, placement.HomeZoneKey(key), "1")
		task := makeWriteTask(t, key, placementTestMsgType, 10).dbTask
		require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, task, 10, 0))

		for round := 0; round < 3; round++ {
			claimed := claimRetry(t, w)
			require.NotNil(t, claimed, "round %d", round)
			w.handleTask(claimed, true)
			require.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey), "round %d: still deferred", round)
		}
		assert.Zero(t, countLogLines(t, logs, "error", "retry task rescheduled"),
			"a frozen write rescheduled every round must not flood ERROR")
	})

	t.Run("store_down_control", func(t *testing.T) {
		logs := logtest.NewCollector(t)
		w, mr, _ := newTestWorker(t, 0)
		const key uint64 = 9205
		storesOf(t, w).failStore(1000000, errors.New("dial tcp 10.0.0.9:3306: connect: connection refused"))
		setRedisValue(t, mr, placement.Key(key), "1000000:1")
		task := makeWriteTask(t, key, placementTestMsgType, 10).dbTask
		require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, task, 10, 0))

		claimed := claimRetry(t, w)
		require.NotNil(t, claimed)
		w.handleTask(claimed, true)
		assert.Equal(t, 1, countLogLines(t, logs, "error", "retry task rescheduled"),
			"an unreachable placement store is a real infrastructure fault and stays at ERROR")
	})
}
