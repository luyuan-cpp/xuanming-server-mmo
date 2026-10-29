package routing

// 读方门禁对「调用方已放弃」的处理(route_read.go 的 callerAbandoned,M1-2,2026-10-21):调用方取消 / 超时既不是缓存不可信,
// 也不是全局库故障 —— 不回源、不计 script_error / sql_error,错误链带 ctx 错误。计数从默认 registry 真的 gather 出来比较增量
// (同 logic 包 playerNameOpsCount 的做法:只断言走了哪个分支,看不出「计到了别处」或「根本没注册」)。
// ctx 一律用「已取消」或「截止时间早已过去」的,不依赖墙钟。

import (
	"context"
	"testing"
	"time"

	"data_service/internal/metrics"

	"shared/placement"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routeMetricValue 读默认 registry 里一条计数器序列的当前值(该序列还不存在 = 0)。计数是进程级累加的,用例只比较增量。
func routeMetricValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			matched := 0
			for _, lp := range m.GetLabel() {
				if want, ok := labels[lp.GetName()]; ok && want == lp.GetValue() {
					matched++
				}
			}
			if matched == len(labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// routeReadSQLErrorCount 是 route_read_total{path, result="sql_error"} 的当前值。
func routeReadSQLErrorCount(t *testing.T, path string) float64 {
	t.Helper()
	return routeMetricValue(t, "data_service_route_read_total", map[string]string{"path": path, "result": metrics.RouteReadResultSQLError})
}

// routeReadyVerdictCount 是 route_ready_verdict_total{verdict} 的当前值。
func routeReadyVerdictCount(t *testing.T, verdict string) float64 {
	t.Helper()
	return routeMetricValue(t, "data_service_route_ready_verdict_total", map[string]string{"verdict": verdict})
}

// abandonEntry 是读方门禁的一个入口(R2 / R6 / R3),path 是它在 route_read_total 里的 path。
type abandonEntry struct {
	path string
	call func(r *Router, ctx context.Context) error
}

func abandonEntries() []abandonEntry {
	return []abandonEntry{
		{metrics.RouteReadPathHomeFence, func(r *Router, ctx context.Context) error {
			_, _, err := r.GetPlayerHomeZoneAndMergeFence(ctx, 8901)
			return err
		}},
		{metrics.RouteReadPathHome, func(r *Router, ctx context.Context) error {
			_, err := r.GetPlayerHomeZone(ctx, 8901)
			return err
		}},
		{metrics.RouteReadPathBatch, func(r *Router, ctx context.Context) error {
			_, err := r.BatchGetPlayerHomeZone(ctx, []uint64{8901, 8902})
			return err
		}},
	}
}

// TestReadGate_CallerAbandonedIsNotAFault(M1-2):
//   - 读脚本因调用方取消而失败:不回源(拿着已结束的 ctx 回源必然失败)、不计 script_error / sql_error;
//   - 读脚本跑完(不可信)、回源 SQL 时截止时间已过:不计 sql_error(verdict 照常计 —— 脚本确实跑了);R7 同口径;
//   - 四个入口的错误都带 ctx 错误,不包 ErrRouteReadUnavailable / ErrHomeZoneNotMapped(gRPC 层据此不回 Unavailable + 码 30);
//   - 对照组:ctx 还活着时同一个 SQL 故障照旧计 sql_error、包 ErrRouteReadUnavailable(证明上面的「没计」不是计数读错了)。
func TestReadGate_CallerAbandonedIsNotAFault(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	// 截止时间是 1970 年:ctx.Err() 立刻就是 DeadlineExceeded,与现在几点无关。
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()

	t.Run("script failed after the caller gave up", func(t *testing.T) {
		for _, e := range abandonEntries() {
			reader := &readFakeReader{}
			r, _, fake := newReadGateRouter(t, true, reader)
			fake.err = context.Canceled // 读脚本因调用方取消而没跑成
			scriptErrBefore := routeReadyVerdictCount(t, metrics.RouteReadyVerdictScriptError)
			sqlErrBefore := routeReadSQLErrorCount(t, e.path)

			err := e.call(r, canceled)
			require.Error(t, err, e.path)
			assert.ErrorIs(t, err, context.Canceled, e.path)
			assert.NotErrorIs(t, err, ErrRouteReadUnavailable, "%s: the caller's cancellation is not a player_route failure", e.path)
			assert.NotErrorIs(t, err, ErrHomeZoneNotMapped, e.path)
			assert.Empty(t, reader.homeFenceCalls, "%s: no fallback with a finished ctx", e.path)
			assert.Empty(t, reader.getRoutesCalls, "%s: no fallback with a finished ctx", e.path)
			assert.Equal(t, scriptErrBefore, routeReadyVerdictCount(t, metrics.RouteReadyVerdictScriptError), "%s: not a script_error", e.path)
			assert.Equal(t, sqlErrBefore, routeReadSQLErrorCount(t, e.path), "%s: not a sql_error", e.path)
		}
	})

	t.Run("fallback failed after the deadline", func(t *testing.T) {
		for _, e := range abandonEntries() {
			reader := &readFakeReader{err: context.DeadlineExceeded}
			r, mr, _ := newReadGateRouter(t, true, reader)
			mr.Del(placement.RouteReadyKey) // 标记缺席 → 不可信 → 回源
			absentBefore := routeReadyVerdictCount(t, placement.RouteReadyAbsent.String())
			sqlErrBefore := routeReadSQLErrorCount(t, e.path)

			err := e.call(r, expired)
			require.Error(t, err, e.path)
			assert.ErrorIs(t, err, context.DeadlineExceeded, e.path)
			assert.NotErrorIs(t, err, ErrRouteReadUnavailable, e.path)
			assert.NotErrorIs(t, err, ErrHomeZoneNotMapped, e.path)
			assert.Equal(t, sqlErrBefore, routeReadSQLErrorCount(t, e.path), "%s: not a sql_error", e.path)
			assert.Equal(t, absentBefore+1, routeReadyVerdictCount(t, placement.RouteReadyAbsent.String()),
				"%s: the script did run, its verdict is still counted", e.path)
		}

		// R7(enumerate):db_first 下与开关无关,一律读库。
		reader := &readFakeReader{err: context.DeadlineExceeded}
		r, _, _ := newReadGateRouter(t, false, reader)
		before := routeReadSQLErrorCount(t, metrics.RouteReadPathEnumerate)
		_, err := r.GetAllPlayerIDsInZone(expired, 7)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, ErrRouteReadUnavailable)
		assert.Equal(t, before, routeReadSQLErrorCount(t, metrics.RouteReadPathEnumerate), "enumerate: not a sql_error")
	})

	t.Run("control: a live ctx still counts sql_error", func(t *testing.T) {
		for _, e := range abandonEntries() {
			reader := &readFakeReader{err: errReadBoom}
			r, mr, _ := newReadGateRouter(t, true, reader)
			mr.Del(placement.RouteReadyKey)
			before := routeReadSQLErrorCount(t, e.path)

			err := e.call(r, context.Background())
			require.ErrorIs(t, err, ErrRouteReadUnavailable, e.path)
			assert.Equal(t, before+1, routeReadSQLErrorCount(t, e.path), "%s: a real player_route failure is a sql_error", e.path)
		}
	})
}

// TestCallerAbandoned:ctx 还活着时回 nil(调用方照常处理);已结束时回的错误带 ctx 错误、文案带上下文与原错误,不包任何读方哨兵错误。
func TestCallerAbandoned(t *testing.T) {
	assert.NoError(t, callerAbandoned(context.Background(), "x", errReadBoom))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err := callerAbandoned(canceled, "home for player 1", errReadBoom)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, errReadBoom, "the underlying error only goes into the text")
	assert.NotErrorIs(t, err, ErrRouteReadUnavailable)
	assert.Contains(t, err.Error(), "home for player 1")
	assert.Contains(t, err.Error(), errReadBoom.Error())
}
