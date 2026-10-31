package routing

// 路由器的编排与错误分流:假 RouteStore,不连库。SQL 本身的判定由 store 包的契约用例与真库用例钉住。

import (
	"context"
	"errors"
	"testing"
	"time"

	"data_service/internal/store"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 编译期断言:装配层把 *store.RouteStore 直接注入。
var _ RouteStore = (*store.RouteStore)(nil)

// fakeRouteStore 按预置结果回答,并记下每次调用。
type fakeRouteStore struct {
	registerResult store.RouteRegisterResult
	registerErr    error
	registerReqs   []store.RouteRegisterRequest

	rows      map[uint64]store.RouteRow
	readErr   error
	readCalls [][]uint64
}

func (f *fakeRouteStore) RegisterHome(_ context.Context, req store.RouteRegisterRequest) (store.RouteRegisterResult, error) {
	f.registerReqs = append(f.registerReqs, req)
	return f.registerResult, f.registerErr
}

func (f *fakeRouteStore) GetRoutes(_ context.Context, ids []uint64) (map[uint64]store.RouteRow, error) {
	f.readCalls = append(f.readCalls, append([]uint64(nil), ids...))
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := make(map[uint64]store.RouteRow, len(ids))
	for _, id := range ids {
		if row, ok := f.rows[id]; ok {
			out[id] = row
		}
	}
	return out, nil
}

// routeMetric 读默认注册表里一条计数器序列的当前值;序列不存在回 0。计数是进程级累加的,用例一律比较增量。
func routeMetric(t *testing.T, family string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := 0
			for _, l := range m.GetLabel() {
				if labels[l.GetName()] == l.GetValue() {
					match++
				}
			}
			if match == len(labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func registerCount(t *testing.T, outcome string) float64 {
	t.Helper()
	return routeMetric(t, "data_service_route_register_total", map[string]string{"outcome": outcome})
}

func readCount(t *testing.T, path, result string) float64 {
	t.Helper()
	return routeMetric(t, "data_service_route_read_total", map[string]string{"path": path, "result": result})
}

func TestRegisterPlayerZone_Outcomes(t *testing.T) {
	boom := errors.New("Error 1205")
	for _, tc := range []struct {
		name         string
		result       store.RouteRegisterResult
		storeErr     error
		wantLabel    string
		wantConflict uint32
		wantStoreErr bool
	}{
		{name: "新行", result: store.RouteRegisterResult{Outcome: store.RouteRegisterInserted}, wantLabel: "inserted"},
		{name: "补 home", result: store.RouteRegisterResult{Outcome: store.RouteRegisterFilledHome}, wantLabel: "filled_home"},
		{name: "落点不覆盖", result: store.RouteRegisterResult{Outcome: store.RouteRegisterFilledHomePlacementKept}, wantLabel: "filled_home_placement_kept"},
		{name: "幂等", result: store.RouteRegisterResult{Outcome: store.RouteRegisterAlreadySame}, wantLabel: "already_same"},
		{name: "冲突", result: store.RouteRegisterResult{Outcome: store.RouteRegisterConflict, ExistingHome: 8}, wantLabel: "conflict", wantConflict: 8},
		{name: "库故障", storeErr: boom, wantLabel: "store_error", wantStoreErr: true},
		{name: "未知结局按库故障", result: store.RouteRegisterResult{Outcome: store.RouteRegisterOutcome(99)}, wantLabel: "store_error", wantStoreErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRouteStore{registerResult: tc.result, registerErr: tc.storeErr}
			before := registerCount(t, tc.wantLabel)

			err := NewRouter(fake).RegisterPlayerZone(context.Background(), 42, 5, 1000000)

			assert.Equal(t, before+1, registerCount(t, tc.wantLabel), "每次调用恰好计一次结局")
			require.Len(t, fake.registerReqs, 1)
			assert.Equal(t, store.RouteRegisterRequest{PlayerID: 42, HomeZone: 5, StorageID: 1000000}, fake.registerReqs[0])
			var conflict *HomeZoneConflictError
			switch {
			case tc.wantConflict != 0:
				require.ErrorAs(t, err, &conflict)
				assert.Equal(t, HomeZoneConflictError{PlayerID: 42, Existing: tc.wantConflict, Requested: 5}, *conflict)
			case tc.wantStoreErr:
				require.ErrorIs(t, err, ErrRouteStore)
				if tc.storeErr != nil {
					require.ErrorIs(t, err, tc.storeErr, "原始错误要留在链上,运维才查得到")
				}
			default:
				require.NoError(t, err)
			}
		})
	}
}

// 参数非法不碰库:player_id 为 0、home 为 0 或越界、没有落点。
func TestRegisterPlayerZone_InvalidArgsNeverReachStore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		player  uint64
		home    uint32
		storage uint32
	}{
		{"player 0", 0, 5, 1000000},
		{"home 0", 42, 0, 1000000},
		{"home 越界", 42, store.RouteHomeZoneMax + 1, 1000000},
		{"没有落点", 42, 5, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRouteStore{}
			before := registerCount(t, "invalid")
			err := NewRouter(fake).RegisterPlayerZone(context.Background(), tc.player, tc.home, tc.storage)
			require.ErrorIs(t, err, ErrInvalidRegistration)
			assert.Empty(t, fake.registerReqs)
			assert.Equal(t, before+1, registerCount(t, "invalid"))
		})
	}
}

// route store 没装配:读写都 fail-closed,读绝不说「没有归属」。
func TestRouter_MissingStoreFailsClosed(t *testing.T) {
	r := NewRouter(nil)

	before := registerCount(t, "store_error")
	require.ErrorIs(t, r.RegisterPlayerZone(context.Background(), 42, 5, 1000000), ErrRouteStore)
	assert.Equal(t, before+1, registerCount(t, "store_error"))

	beforeRead := readCount(t, "home", "error")
	_, err := r.GetPlayerHomeZone(context.Background(), 42)
	require.ErrorIs(t, err, ErrRouteStore)
	require.NotErrorIs(t, err, ErrHomeZoneNotMapped)
	assert.Equal(t, beforeRead+1, readCount(t, "home", "error"))

	beforeBatch := readCount(t, "batch", "error")
	_, err = r.BatchGetPlayerHomeZone(context.Background(), []uint64{42})
	require.ErrorIs(t, err, ErrRouteStore)
	assert.Equal(t, beforeBatch+1, readCount(t, "batch", "error"))
}

func TestGetPlayerHomeZone(t *testing.T) {
	fake := &fakeRouteStore{rows: map[uint64]store.RouteRow{
		42: {PlayerID: 42, HomeZone: 5, Rev: 1},
		// home_zone 为 NULL 的行(只钉了落点):对归属读来说就是没有。
		43: {PlayerID: 43, StorageID: 1000000, PlacementVersion: 1, Rev: 1},
	}}
	r := NewRouter(fake)

	okBefore := readCount(t, "home", "ok")
	zone, err := r.GetPlayerHomeZone(context.Background(), 42)
	require.NoError(t, err)
	assert.Equal(t, uint32(5), zone)
	assert.Equal(t, okBefore+1, readCount(t, "home", "ok"))
	assert.Equal(t, []uint64{42}, fake.readCalls[0], "单查走一个 id 的点查")

	for _, id := range []uint64{43, 44} {
		nfBefore := readCount(t, "home", "not_found")
		_, err = r.GetPlayerHomeZone(context.Background(), id)
		require.ErrorIs(t, err, ErrHomeZoneNotMapped)
		require.NotErrorIs(t, err, ErrRouteStore)
		assert.Equal(t, nfBefore+1, readCount(t, "home", "not_found"))
	}
	// 文案是 login homezone.IsUnmapped 的契约,改了调用方会把「没有映射」当成故障。
	assert.Contains(t, err.Error(), "no home zone mapping")
}

// 一次 SQL 故障绝不能被报成「这名玩家没有归属」:login 会据此退回建角 zone。
func TestGetPlayerHomeZone_SQLFailureIsNotNotFound(t *testing.T) {
	boom := errors.New("Error 2013: lost connection")
	before := readCount(t, "home", "error")
	_, err := NewRouter(&fakeRouteStore{readErr: boom}).GetPlayerHomeZone(context.Background(), 42)
	require.ErrorIs(t, err, ErrRouteStore)
	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, ErrHomeZoneNotMapped)
	assert.Equal(t, before+1, readCount(t, "home", "error"))
}

func TestBatchGetPlayerHomeZone(t *testing.T) {
	fake := &fakeRouteStore{rows: map[uint64]store.RouteRow{
		42: {PlayerID: 42, HomeZone: 5, Rev: 1},
		43: {PlayerID: 43, StorageID: 1000000, PlacementVersion: 1, Rev: 1}, // home 为 NULL:不进结果
	}}
	before := readCount(t, "batch", "ok")
	got, err := NewRouter(fake).BatchGetPlayerHomeZone(context.Background(), []uint64{42, 43, 44})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]uint32{42: 5}, got, "缺席的 id 不出现在结果里,不是错误")
	assert.Equal(t, before+1, readCount(t, "batch", "ok"))
}

func TestBatchGetPlayerHomeZone_EmptyIssuesNoSQL(t *testing.T) {
	fake := &fakeRouteStore{}
	got, err := NewRouter(fake).BatchGetPlayerHomeZone(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.NotNil(t, got)
	assert.Empty(t, fake.readCalls)
}

// 按 RouteBatchLimit 分块,每块一条 SQL、记一次指标,结果合并。
func TestBatchGetPlayerHomeZone_Chunks(t *testing.T) {
	total := 2*store.RouteBatchLimit + 1
	ids := make([]uint64, total)
	rows := make(map[uint64]store.RouteRow, total)
	for i := range ids {
		ids[i] = uint64(i + 1)
		rows[ids[i]] = store.RouteRow{PlayerID: ids[i], HomeZone: 7, Rev: 1}
	}
	fake := &fakeRouteStore{rows: rows}
	before := readCount(t, "batch", "ok")

	got, err := NewRouter(fake).BatchGetPlayerHomeZone(context.Background(), ids)

	require.NoError(t, err)
	assert.Len(t, got, total)
	require.Len(t, fake.readCalls, 3)
	assert.Len(t, fake.readCalls[0], store.RouteBatchLimit)
	assert.Len(t, fake.readCalls[1], store.RouteBatchLimit)
	assert.Len(t, fake.readCalls[2], 1)
	assert.Equal(t, before+3, readCount(t, "batch", "ok"))
}

// 任一块失败整批报错,不回半份结果:半份会被调用方当成「这些人没有归属」。
func TestBatchGetPlayerHomeZone_AnyChunkFailureFailsWholeBatch(t *testing.T) {
	boom := errors.New("Error 2013: lost connection")
	ids := make([]uint64, store.RouteBatchLimit+1)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	fake := &failingAfterStore{okCalls: 1, err: boom}

	got, err := NewRouter(fake).BatchGetPlayerHomeZone(context.Background(), ids)

	require.ErrorIs(t, err, ErrRouteStore)
	require.ErrorIs(t, err, boom)
	assert.Nil(t, got)
	assert.Equal(t, 2, fake.calls)
}

// failingAfterStore 前 okCalls 次读成功(回空),之后失败。
type failingAfterStore struct {
	fakeRouteStore
	okCalls int
	calls   int
	err     error
}

func (f *failingAfterStore) GetRoutes(context.Context, []uint64) (map[uint64]store.RouteRow, error) {
	f.calls++
	if f.calls > f.okCalls {
		return nil, f.err
	}
	return map[uint64]store.RouteRow{}, nil
}

// 调用方已放弃(ctx 取消 / 超时):不计 error、不包 ErrRouteStore,错误链带 ctx 的错误。
// 记成 error 会触发全局库故障告警,把值班引去查一个正常的全局库。
func TestRouter_CallerAbandonedIsNotAFault(t *testing.T) {
	boom := errors.New("context canceled by driver")
	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"取消", canceledCtx, context.Canceled},
		{"超时", deadlineCtx, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRouter(&fakeRouteStore{readErr: boom})

			homeBefore, batchBefore := readCount(t, "home", "error"), readCount(t, "batch", "error")
			_, err := r.GetPlayerHomeZone(tc.ctx, 42)
			require.ErrorIs(t, err, tc.want)
			require.NotErrorIs(t, err, ErrRouteStore)
			require.NotErrorIs(t, err, ErrHomeZoneNotMapped)

			_, err = r.BatchGetPlayerHomeZone(tc.ctx, []uint64{42})
			require.ErrorIs(t, err, tc.want)
			require.NotErrorIs(t, err, ErrRouteStore)

			assert.Equal(t, homeBefore, readCount(t, "home", "error"))
			assert.Equal(t, batchBefore, readCount(t, "batch", "error"))
		})
	}
}
