package svc

import (
	"context"
	"testing"

	"match/internal/config"
	"shared/drainproof"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// MatchRedis 未配置:私有 key 回落到 Redis 段的共享库(本地单库形态)。
func TestNewRedisHandlesFallsBackToShared(t *testing.T) {
	mr := miniredis.RunT(t)
	c := config.Config{}
	c.Redis.RedisConf = redis.RedisConf{Host: mr.Addr(), Type: "node"}

	matchRds := NewRedisHandles(c)
	require.NoError(t, matchRds.Set("spectate:battles:active", "x"))
	require.True(t, mr.Exists("spectate:battles:active"))
}

// MatchRedis 配置了独立实例:写入只落在它上面。
func TestNewRedisHandlesSeparatesWhenConfigured(t *testing.T) {
	shared := miniredis.RunT(t)
	private := miniredis.RunT(t)
	c := config.Config{}
	c.Redis.RedisConf = redis.RedisConf{Host: shared.Addr(), Type: "node"}
	c.MatchRedis = redis.RedisConf{Host: private.Addr(), Type: "node"}

	matchRds := NewRedisHandles(c)
	require.NoError(t, matchRds.Set("spectate:battles:active", "x"))
	require.True(t, private.Exists("spectate:battles:active"))
	require.False(t, shared.Exists("spectate:battles:active"))
}

// 关停等结果消费组 lag 探针退出:StopAndReport 先停后台(取消探针的 ctx),再等探针 goroutine 关掉退出信号,之后才关资源。
// 替身探针与真探针同一形状:跟着后台 ctx 退出。只登记一次;只有 RootAdmission / RelayAdmission 的最小 ServiceContext,其余资源为空。
func TestStopAndReportWaitsForResultLagProbeToExit(t *testing.T) {
	sc := &ServiceContext{RootAdmission: drainproof.New(), RelayAdmission: drainproof.New()}
	background, stop := context.WithCancel(context.Background())
	defer stop()
	require.NoError(t, sc.BindBackgroundStop(stop))
	probeDone := make(chan struct{})
	require.ErrorIs(t, sc.BindResultLagProbe(nil), drainproof.ErrHold)
	require.NoError(t, sc.BindResultLagProbe(probeDone))
	require.ErrorIs(t, sc.BindResultLagProbe(make(chan struct{})), drainproof.ErrHold, "只能登记一次")
	go func() {
		<-background.Done()
		close(probeDone)
	}()
	require.NoError(t, sc.StopAndReport(context.Background()))
	select {
	case <-probeDone:
	default:
		t.Fatal("StopAndReport 返回前探针必须已经退出")
	}
	require.True(t, sc.resourcesClosed)
}

// 探针到关停预算用完仍没退出:说不清后台是否都停了,按 HOLD 报,不关资源。
// 关停预算在停后台的同一刻取消(之前的步骤都没有在途工作,不受它影响),确定地落在等探针这一步。
func TestStopAndReportHoldsWhenResultLagProbeDoesNotExit(t *testing.T) {
	sc := &ServiceContext{RootAdmission: drainproof.New(), RelayAdmission: drainproof.New()}
	budget, expire := context.WithCancel(context.Background())
	defer expire()
	require.NoError(t, sc.BindBackgroundStop(expire))
	require.NoError(t, sc.BindResultLagProbe(make(chan struct{})))
	err := sc.StopAndReport(budget)
	require.ErrorIs(t, err, drainproof.ErrHold)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, sc.resourcesClosed, "探针没退出就不能宣称资源已关")
	sc.RelayAdmission.CloseAdmission()
	require.ErrorIs(t, sc.RelayAdmission.Wait(context.Background()), drainproof.ErrHold)
}
