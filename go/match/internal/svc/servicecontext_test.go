package svc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"match/internal/config"
	"shared/clusterredis"
	"shared/drainproof"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

// MatchRedis 没写 Addrs:私有 key 落到 Redis 段那一个实例(本地单库形态)。
func TestNewMatchRedisFallsBackToRedisSection(t *testing.T) {
	mr := miniredis.RunT(t)
	c := config.Config{}
	c.Redis.Host = mr.Addr()

	matchRds, err := NewMatchRedis(c)
	require.NoError(t, err)
	t.Cleanup(func() { _ = matchRds.Close() })
	require.NoError(t, matchRds.Set(context.Background(), "spectate:battles:active:{sba:0}", "x", 0).Err())
	require.True(t, mr.Exists("spectate:battles:active:{sba:0}"))
}

// MatchRedis 写了独立实例:写入只落在它上面。句柄带 match 私有集群的键守卫,玩家域的键发不出去。
func TestNewMatchRedisSeparatesWhenConfigured(t *testing.T) {
	shared := miniredis.RunT(t)
	private := miniredis.RunT(t)
	c := config.Config{}
	c.Redis.Host = shared.Addr()
	c.MatchRedis = clusterredis.Config{Addrs: []string{private.Addr()}}

	matchRds, err := NewMatchRedis(c)
	require.NoError(t, err)
	t.Cleanup(func() { _ = matchRds.Close() })
	require.NoError(t, matchRds.Set(context.Background(), "spectate:battles:active:{sba:0}", "x", 0).Err())
	require.True(t, private.Exists("spectate:battles:active:{sba:0}"))
	require.False(t, shared.Exists("spectate:battles:active:{sba:0}"))

	err = matchRds.Set(context.Background(), "player:{42}:location", "x", 0).Err()
	require.ErrorIs(t, err, clusterredis.ErrNotMatchPrivateKey)
	require.False(t, private.Exists("player:{42}:location"), "被守卫拒绝的命令不到服务端")
}

// Redis 段没写地址、MatchRedis 也没写 Addrs:构造报错并点名 MatchRedis,不 panic。
func TestNewMatchRedisRejectsInvalidConfig(t *testing.T) {
	_, err := NewMatchRedis(config.Config{})
	require.ErrorContains(t, err, "MatchRedis")
}

// match 模块(含测试)不许再导入 go-zero 的 Redis 句柄:它写死 MaxRetries=3,读超时或 EOF 之后会把已经写出的命令再发一遍
// (断线重连清单 R8),也没有键守卫。私有集群与玩家域都走 shared/clusterredis 的构造器,测试用生产同一个构造器。
func TestMatchModuleDoesNotImportGoZeroRedis(t *testing.T) {
	// 拆成两段拼:整串写在这里会让本文件扫到自己。
	const goZeroRedis = `"github.com/zeromicro/go-zero/core/stores/` + `redis"`
	root := filepath.Join("..", "..")
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			// 根目录写成 "../..",它的 Name() 是 "..":不排除它的话「点开头的目录」这条会把整棵树跳过。
			if name := d.Name(); path != root && (name == "generated" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		require.NotContains(t, string(raw), goZeroRedis, "%s imports the go-zero Redis handle; use shared/clusterredis", path)
		return nil
	})
	require.NoError(t, err)
	require.Greater(t, scanned, 50, "扫描范围应是整个 match 模块")
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
