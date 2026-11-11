package discovery

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeClock 是注入连接池的单调时钟,测试手动拨动。
type fakeClock struct{ now time.Duration }

func (c *fakeClock) read() time.Duration { return c.now }

// newTestPool 返回用注入时钟、按 endpoint 记拨号次数的连接池。passthrough 的 grpc.NewClient 只建对象,不真的建连。
func newTestPool(clock *fakeClock, dials map[string]int) *connPool {
	return newConnPool(clock.read, func(endpoint string) (*grpc.ClientConn, error) {
		dials[endpoint]++
		return grpc.NewClient("passthrough:///"+endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	})
}

// 同一 endpoint 复用一条连接,不重复拨号。
func TestConnPoolReusesConnection(t *testing.T) {
	dials := map[string]int{}
	pool := newTestPool(&fakeClock{}, dials)
	first, err := pool.get("match:1")
	require.NoError(t, err)
	second, err := pool.get("match:1")
	require.NoError(t, err)
	require.Same(t, first, second)
	require.Equal(t, 1, dials["match:1"])
}

// 空闲回收按「最后一次取用」计时:取用过的连接重新计时,满 maxIdle 才关;关掉之后再取会重新拨号。
func TestConnPoolClosesOnlyIdleConnections(t *testing.T) {
	clock := &fakeClock{}
	dials := map[string]int{}
	pool := newTestPool(clock, dials)

	busy, err := pool.get("busy:1")
	require.NoError(t, err)
	idle, err := pool.get("idle:1")
	require.NoError(t, err)

	clock.now = 9 * time.Minute
	_, err = pool.get("busy:1") // busy 在第 9 分钟又被取用
	require.NoError(t, err)
	closed, remaining := pool.closeIdle(10 * time.Minute)
	require.Equal(t, 0, closed, "都还没空闲满 10 分钟")
	require.Equal(t, 2, remaining)

	clock.now = 10 * time.Minute
	closed, remaining = pool.closeIdle(10 * time.Minute)
	require.Equal(t, 1, closed, "idle 从第 0 分钟起没人用,满 10 分钟关掉")
	require.Equal(t, 1, remaining)
	require.Equal(t, connectivity.Shutdown, idle.GetState())
	require.NotEqual(t, connectivity.Shutdown, busy.GetState(), "busy 第 9 分钟才用过,不能关")

	again, err := pool.get("idle:1")
	require.NoError(t, err)
	require.NotSame(t, idle, again, "关掉之后再取要重新拨号")
	require.Equal(t, 2, dials["idle:1"])
	require.Equal(t, 1, dials["busy:1"])
}

// 节点下线:立即关闭并移除,不等空闲回收;移除不存在的 endpoint 什么也不做。
func TestConnPoolRemove(t *testing.T) {
	dials := map[string]int{}
	pool := newTestPool(&fakeClock{}, dials)
	conn, err := pool.get("chat:1")
	require.NoError(t, err)

	pool.remove("chat:1")
	require.Equal(t, connectivity.Shutdown, conn.GetState())
	pool.remove("chat:1")

	_, remaining := pool.closeIdle(time.Hour)
	require.Equal(t, 0, remaining)
	_, err = pool.get("chat:1")
	require.NoError(t, err)
	require.Equal(t, 2, dials["chat:1"], "移除之后再取要重新拨号")
}
