package routing

// 第 3 批读方门禁(route_read.go)的编排:miniredis + 假 RouteReader + 假脚本执行器,不连库。
//
// miniredis 不支持 INFO 的 server / replication 段(读脚本在它上面必然报错),所以「可信」路径要绕开脚本执行:
// fakeReadyRedis 按两段读脚本的语义在 miniredis 的数据上拼回复(身份由用例给),注入 Router.routeReadEval。
// 脚本本身在真 Redis 上的语义由 router_integration_test.go 的 TestRouteReadyRead_RealRedis* 验;miniredis 上真跑脚本
// 必然失败这件事本身也有用 —— 它就是「脚本执行出错 → 回源 SQL」与启动探针拒启的用例(TestProbeRouteReadyRead_*)。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"data_service/internal/config"
	"data_service/internal/store"

	"shared/placement"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 假实例的身份(40 个小写十六进制字符)与标记里的世代。
const (
	readyRunID      = "0123456789abcdef0123456789abcdef01234567"
	readyReplID     = "89abcdef0123456789abcdef0123456789abcdef"
	readyGeneration = "00112233445566778899aabbccddeeff"
)

var errReadBoom = errors.New("player_route: connection reset by peer")

// readyMarkerFor 编码一个记下 (runID, replID) 的合法标记。
func readyMarkerFor(t *testing.T, runID, replID string) string {
	t.Helper()
	raw, err := placement.EncodeRouteReadyMarker(placement.RouteReadyMarker{
		Generation:   readyGeneration,
		RedisRunID:   runID,
		MasterReplID: replID,
		SetAtMs:      1,
		WatermarkMs:  1,
		Leader:       "data_service@unit-test",
	})
	require.NoError(t, err)
	return raw
}

// fakeReadyRedis 在 miniredis 的数据上模拟两段读脚本。runID / role / replID 是「本实例」此刻的身份;
// err 非 nil 时模拟脚本执行出错,reply 非 nil 时原样回它(造形状不对的回复);mismatchOnCall 让第 n 次调用(从 1 起)
// 回一个不同的 run_id(造「这一块不可信」)。calls 记下每次的 KEYS,args 记下每次的 ARGV。
type fakeReadyRedis struct {
	mr             *miniredis.Miniredis
	runID          string
	role           string
	replID         string
	err            error
	reply          any
	mismatchOnCall int
	scripts        []string
	calls          [][]string
	args           [][]any
}

func (f *fakeReadyRedis) eval(_ context.Context, script string, keys []string, args ...any) (any, error) {
	f.scripts = append(f.scripts, script)
	f.calls = append(f.calls, append([]string(nil), keys...))
	f.args = append(f.args, append([]any(nil), args...))
	if f.err != nil {
		return nil, f.err
	}
	if f.reply != nil {
		return f.reply, nil
	}
	get := func(k string) any {
		if v, err := f.mr.Get(k); err == nil {
			return v
		}
		return nil
	}
	runID := f.runID
	if f.mismatchOnCall == len(f.calls) {
		runID = strings.Repeat("f", 40)
	}
	out := []any{get(keys[0]), runID, f.role, f.replID}
	switch script {
	case placement.RouteReadyReadScript:
		for _, k := range keys[1:] {
			out = append(out, get(k))
		}
	default:
		return nil, fmt.Errorf("fakeReadyRedis: unexpected script %q", script)
	}
	return out, nil
}

// readFakeReader 是 RouteReader 的假实现:按预置数据回答,记下每次入参。err 非 nil 时三个方法都回它;
// failGetRoutesOn 让第 n 次 GetRoutes(从 1 起)失败。pages 按调用顺序作为 ScanRoutesByHome 的每一页。
type readFakeReader struct {
	rows            map[uint64]store.RouteRow
	err             error
	failGetRoutesOn int
	pages           [][]store.RouteRow
	getRoutesCalls  [][]uint64
	scanCalls       []store.RouteScanCursor
	scanLimits      []int
}

var _ RouteReader = (*readFakeReader)(nil)

// 接口一变,编译期先炸:*store.RouteStore 必须逐字满足 RouteReader(svc 把同一个实例注入两边)。
var _ RouteReader = (*store.RouteStore)(nil)

func (f *readFakeReader) GetRoutes(_ context.Context, ids []uint64) (map[uint64]store.RouteRow, error) {
	f.getRoutesCalls = append(f.getRoutesCalls, append([]uint64(nil), ids...))
	if f.err != nil {
		return nil, f.err
	}
	if f.failGetRoutesOn != 0 && len(f.getRoutesCalls) == f.failGetRoutesOn {
		return nil, errReadBoom
	}
	out := make(map[uint64]store.RouteRow, len(ids))
	for _, id := range ids {
		if row, ok := f.rows[id]; ok {
			out[id] = row
		}
	}
	return out, nil
}

// newReadGateRouter 建一个 db_first 的 Router(mapping Redis 指向 miniredis),开关按 on 取值;reader 非 nil 时注入
// (不能把 nil 的 *readFakeReader 传给 WithRouteReader,那是「非 nil 接口包着 nil 指针」)。读脚本的执行换成 fakeReadyRedis,
// 身份与标记匹配(调用方再改字段造各种 verdict),标记默认已挂。
func newReadGateRouter(t *testing.T, on bool, reader *readFakeReader) (*Router, *miniredis.Miniredis, *fakeReadyRedis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := config.Config{
		MappingRedis: redis.RedisConf{Host: mr.Addr(), Type: "node"},
		RouteStore: config.RouteStoreConfig{
			Mode:               config.RouteStoreModeDBFirst,
			WaitAOFNumLocal:    intPtr(0),
			WaitAOFNumReplicas: intPtr(0),
			ReadRequiresReady:  on,
		},
	}
	var opts []RouterOption
	if reader != nil {
		opts = append(opts, WithRouteReader(reader))
	}
	r := NewRouter(c, opts...)
	t.Cleanup(r.Close)
	require.Equal(t, on, r.readRequiresReady, "NewRouter must take RouteStore.ReadRequiresReady")
	fake := &fakeReadyRedis{mr: mr, runID: readyRunID, role: "master", replID: readyReplID}
	require.NoError(t, mr.Set(placement.RouteReadyKey, readyMarkerFor(t, readyRunID, readyReplID)))
	r.routeReadEval = fake.eval
	return r, mr, fake
}

// TestReadGateOffKeepsTodaysPaths:开关为 false 时 R3 / R6 与今天逐字相同 —— 一次读脚本都不跑、回源读者一次都不碰,
// 结果与缺席 / 故障的口径照旧(缺席 NotFound,不是 ErrRouteReadUnavailable)。
func TestReadGateOffKeepsTodaysPaths(t *testing.T) {
	reader := &readFakeReader{rows: map[uint64]store.RouteRow{8201: {PlayerID: 8201, HomeZone: 9, Rev: 1}}}
	r, mr, fake := newReadGateRouter(t, false, reader)
	ctx := context.Background()
	require.NoError(t, mr.Set(mappingKey(8201), "5"))

	zone, err := r.GetPlayerHomeZone(ctx, 8201)
	require.NoError(t, err)
	assert.Equal(t, uint32(5), zone)
	got, err := r.BatchGetPlayerHomeZone(ctx, []uint64{8201, 8202})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{8201: 5}, got)

	_, err = r.GetPlayerHomeZone(ctx, 8202)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped)
	assert.NotErrorIs(t, err, ErrRouteReadUnavailable)

	assert.Empty(t, fake.calls, "the ready read script must not run while ReadRequiresReady=false")
	assert.Empty(t, reader.getRoutesCalls)
}

// readyUntrustedCases 是「副本不可信」的每一种来源:六个非 Trusted 的 verdict、脚本执行出错、回复形状不对。
func readyUntrustedCases() []struct {
	name  string
	setup func(mr *miniredis.Miniredis, f *fakeReadyRedis)
} {
	return []struct {
		name  string
		setup func(mr *miniredis.Miniredis, f *fakeReadyRedis)
	}{
		{"absent", func(mr *miniredis.Miniredis, _ *fakeReadyRedis) { mr.Del(placement.RouteReadyKey) }},
		{"malformed", func(mr *miniredis.Miniredis, _ *fakeReadyRedis) { _ = mr.Set(placement.RouteReadyKey, "not-json") }},
		{"newer_version", func(mr *miniredis.Miniredis, _ *fakeReadyRedis) { _ = mr.Set(placement.RouteReadyKey, `{"v":2}`) }},
		{"identity_unavailable", func(_ *miniredis.Miniredis, f *fakeReadyRedis) { f.runID = "" }},
		{"not_master", func(_ *miniredis.Miniredis, f *fakeReadyRedis) { f.role = "slave" }},
		{"identity_mismatch (restart)", func(_ *miniredis.Miniredis, f *fakeReadyRedis) { f.runID = strings.Repeat("a", 40) }},
		{"identity_mismatch (failover)", func(_ *miniredis.Miniredis, f *fakeReadyRedis) { f.replID = strings.Repeat("b", 40) }},
		{"script error", func(_ *miniredis.Miniredis, f *fakeReadyRedis) {
			f.err = errors.New("ERR This Redis command is not allowed from script")
		}},
		{"unexpected reply shape", func(_ *miniredis.Miniredis, f *fakeReadyRedis) { f.reply = []any{"x"} }},
	}
}

// TestReadyHome_SQLFailureIsUnavailableNotNotFound:不可信 + 库故障 → ErrRouteReadUnavailable(Unavailable),
// 绝不包 ErrHomeZoneNotMapped;回源读者没装配同样 fail-closed。
func TestReadyHome_SQLFailureIsUnavailableNotNotFound(t *testing.T) {
	reader := &readFakeReader{err: errReadBoom}
	r, mr, _ := newReadGateRouter(t, true, reader)
	mr.Del(placement.RouteReadyKey)
	_, err := r.GetPlayerHomeZone(context.Background(), 8501)
	require.ErrorIs(t, err, ErrRouteReadUnavailable)
	require.ErrorIs(t, err, errReadBoom)
	assert.NotErrorIs(t, err, ErrHomeZoneNotMapped)

	r, mr, _ = newReadGateRouter(t, true, nil)
	mr.Del(placement.RouteReadyKey)
	_, err = r.GetPlayerHomeZone(context.Background(), 8501)
	require.ErrorIs(t, err, ErrRouteReadUnavailable, "no route reader assembled must fail closed")
	assert.NotErrorIs(t, err, ErrHomeZoneNotMapped)

	// 可信时没有回源,读者缺席也不影响。
	require.NoError(t, mr.Set(placement.RouteReadyKey, readyMarkerFor(t, readyRunID, readyReplID)))
	require.NoError(t, mr.Set(mappingKey(8501), "4"))
	zone, err := r.GetPlayerHomeZone(context.Background(), 8501)
	require.NoError(t, err)
	assert.Equal(t, uint32(4), zone)
}

// TestReadyHome_TrustedAndFallback:R6(GetPlayerHomeZone,ClientForPlayer 也走它):可信用缓存;
// 不可信按单 id 的 GetRoutes 回源;库无行 / home 为 NULL → NotFound;库故障 → ErrRouteReadUnavailable。
func TestReadyHome_TrustedAndFallback(t *testing.T) {
	reader := &readFakeReader{rows: map[uint64]store.RouteRow{
		8601: {PlayerID: 8601, HomeZone: 9, Rev: 2},
		8602: {PlayerID: 8602, StorageID: placement.DefaultGlobalStorageID, PlacementVersion: 1, Rev: 1},
	}}
	r, mr, fake := newReadGateRouter(t, true, reader)
	ctx := context.Background()
	require.NoError(t, mr.Set(mappingKey(8601), "5"))

	zone, err := r.GetPlayerHomeZone(ctx, 8601)
	require.NoError(t, err)
	assert.Equal(t, uint32(5), zone, "trusted: cache")
	assert.Equal(t, placement.RouteReadyReadScript, fake.scripts[0])
	assert.Equal(t, []string{placement.RouteReadyKey, mappingKey(8601)}, fake.calls[0])
	_, err = r.GetPlayerHomeZone(ctx, 8602)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped, "trusted: a missing key is authoritative")
	assert.Empty(t, reader.getRoutesCalls)

	fake.role = "slave"
	zone, err = r.GetPlayerHomeZone(ctx, 8601)
	require.NoError(t, err)
	assert.Equal(t, uint32(9), zone, "untrusted: player_route")
	_, err = r.GetPlayerHomeZone(ctx, 8602)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped, "a row with only a placement has no home")
	_, err = r.GetPlayerHomeZone(ctx, 8603)
	require.ErrorIs(t, err, ErrHomeZoneNotMapped)
	assert.Equal(t, [][]uint64{{8601}, {8602}, {8603}}, reader.getRoutesCalls)

	reader.err = errReadBoom
	_, err = r.GetPlayerHomeZone(ctx, 8601)
	require.ErrorIs(t, err, ErrRouteReadUnavailable)
	assert.NotErrorIs(t, err, ErrHomeZoneNotMapped)
}

// TestReadyBatch_ChunksTrustedAndUntrusted:R3 按 store.RouteBatchLimit 分块,每块一段脚本(KEYS = ready + 至多 1000 个);
// 可信的块用缓存,不可信的块整块回源(GetRoutes 的 IN 也是至多 1000 个),结果合并。
func TestReadyBatch_ChunksTrustedAndUntrusted(t *testing.T) {
	const n = 2*store.RouteBatchLimit + 500
	ids := make([]uint64, n)
	rows := map[uint64]store.RouteRow{}
	for i := range ids {
		ids[i] = uint64(900001 + i)
		if i%3 == 0 {
			rows[ids[i]] = store.RouteRow{PlayerID: ids[i], HomeZone: 9, Rev: 1}
		}
	}
	reader := &readFakeReader{rows: rows}
	r, mr, fake := newReadGateRouter(t, true, reader)
	ctx := context.Background()
	for i, id := range ids {
		if i%3 == 0 {
			require.NoError(t, mr.Set(mappingKey(id), "5"))
		}
	}

	// 第 2 块不可信:它那 1000 个 id 走库(zone 9),另外两块走缓存(zone 5)。
	fake.mismatchOnCall = 2
	got, err := r.BatchGetPlayerHomeZone(ctx, ids)
	require.NoError(t, err)
	require.Len(t, fake.calls, 3)
	assert.Len(t, fake.calls[0], 1+store.RouteBatchLimit)
	assert.Len(t, fake.calls[1], 1+store.RouteBatchLimit)
	assert.Len(t, fake.calls[2], 1+500)
	for _, keys := range fake.calls {
		assert.Equal(t, placement.RouteReadyKey, keys[0])
	}
	require.Len(t, reader.getRoutesCalls, 1)
	assert.Equal(t, ids[store.RouteBatchLimit:2*store.RouteBatchLimit], reader.getRoutesCalls[0])
	for i, id := range ids {
		want, present := uint32(0), i%3 == 0
		if present {
			want = 5
			if i >= store.RouteBatchLimit && i < 2*store.RouteBatchLimit {
				want = 9
			}
		}
		zone, ok := got[id]
		require.Equal(t, present, ok, "id %d", id)
		require.Equal(t, want, zone, "id %d", id)
	}
}

// TestReadyBatch_AnyChunkFailureFailsWholeBatch:不可信时任一块回源失败整批报错、不回半份结果;可信块里的畸形值同今天整批报错。
func TestReadyBatch_AnyChunkFailureFailsWholeBatch(t *testing.T) {
	ids := make([]uint64, store.RouteBatchLimit+10)
	for i := range ids {
		ids[i] = uint64(910001 + i)
	}
	reader := &readFakeReader{failGetRoutesOn: 2}
	r, mr, _ := newReadGateRouter(t, true, reader)
	mr.Del(placement.RouteReadyKey)
	got, err := r.BatchGetPlayerHomeZone(context.Background(), ids)
	require.ErrorIs(t, err, ErrRouteReadUnavailable)
	assert.Nil(t, got, "no partial result")
	assert.Len(t, reader.getRoutesCalls, 2)

	r, mr, _ = newReadGateRouter(t, true, nil)
	mr.Del(placement.RouteReadyKey)
	_, err = r.BatchGetPlayerHomeZone(context.Background(), ids[:3])
	require.ErrorIs(t, err, ErrRouteReadUnavailable, "no route reader assembled must fail closed")

	r, mr, _ = newReadGateRouter(t, true, &readFakeReader{})
	require.NoError(t, mr.Set(mappingKey(ids[1]), "not-a-zone"))
	got, err = r.BatchGetPlayerHomeZone(context.Background(), ids[:3])
	require.Error(t, err)
	assert.Nil(t, got)
	assert.NotErrorIs(t, err, ErrRouteReadUnavailable, "a malformed trusted value is a cache fault, not a fallback failure")
	assert.Contains(t, err.Error(), "invalid zone mapping")
}

// TestReadyBatch_EmptyIssuesNothing:空列表回空 map,不跑脚本、不碰库。
func TestReadyBatch_EmptyIssuesNothing(t *testing.T) {
	reader := &readFakeReader{}
	r, _, fake := newReadGateRouter(t, true, reader)
	got, err := r.BatchGetPlayerHomeZone(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.NotNil(t, got)
	assert.Empty(t, fake.calls)
	assert.Empty(t, reader.getRoutesCalls)
}

// TestProbeRouteReadyRead_FailsWhenScriptCannotRun:启动探针在读脚本跑不起来时报错(调用方据此拒启)。
// miniredis 不支持 INFO server,正好是「INFO 在脚本里不可用」的情形;连不上的地址是另一种。
func TestProbeRouteReadyRead_FailsWhenScriptCannotRun(t *testing.T) {
	mr := miniredis.RunT(t)
	_, err := ProbeRouteReadyRead(context.Background(), redis.RedisConf{Host: mr.Addr(), Type: "node"})
	require.Error(t, err, "the ready read script calls INFO server, which miniredis rejects")

	addr := mr.Addr()
	mr.Close()
	_, err = ProbeRouteReadyRead(context.Background(), redis.RedisConf{Host: addr, Type: "node"})
	require.Error(t, err, "an unreachable mapping redis must fail the probe")
}

// TestReadyReadScriptFailsOnMiniredisFallsBackToSQL:不注入假执行器、直接在 miniredis 上跑真脚本(INFO 报错)——
// 证明「脚本执行出错 → 回源库」走的是生产的那条执行路径(mappingRedis.EvalCtx),不是假执行器的模拟。
func TestReadyReadScriptFailsOnMiniredisFallsBackToSQL(t *testing.T) {
	reader := &readFakeReader{rows: map[uint64]store.RouteRow{8701: {PlayerID: 8701, HomeZone: 9, Rev: 1}}}
	r, mr, _ := newReadGateRouter(t, true, reader)
	r.routeReadEval = r.mappingRedis.EvalCtx
	require.NoError(t, mr.Set(mappingKey(8701), "5"))

	zone, err := r.GetPlayerHomeZone(context.Background(), 8701)
	require.NoError(t, err)
	assert.Equal(t, uint32(9), zone)
	assert.Equal(t, [][]uint64{{8701}}, reader.getRoutesCalls)
}
