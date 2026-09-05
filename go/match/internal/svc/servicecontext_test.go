package svc

import (
	"testing"

	"match/internal/config"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// MatchRedis 未配置:私有 key 与契约 key 回落到同一句柄(本地单库形态)。
func TestNewRedisHandlesFallsBackToShared(t *testing.T) {
	mr := miniredis.RunT(t)
	c := config.Config{}
	c.Redis.RedisConf = redis.RedisConf{Host: mr.Addr(), Type: "node"}

	matchRds, sharedRds := NewRedisHandles(c)
	require.NotNil(t, sharedRds)
	require.Same(t, sharedRds, matchRds, "MatchRedis 缺省时必须与 SharedRedis 同一句柄")
}

// MatchRedis 配置了独立实例:两句柄分离,写入互不可见。
func TestNewRedisHandlesSeparatesWhenConfigured(t *testing.T) {
	shared := miniredis.RunT(t)
	private := miniredis.RunT(t)
	c := config.Config{}
	c.Redis.RedisConf = redis.RedisConf{Host: shared.Addr(), Type: "node"}
	c.MatchRedis = redis.RedisConf{Host: private.Addr(), Type: "node"}

	matchRds, sharedRds := NewRedisHandles(c)
	require.NotSame(t, sharedRds, matchRds)
	require.NoError(t, matchRds.Set("match:{mq}:index", "x"))
	require.True(t, private.Exists("match:{mq}:index"))
	require.False(t, shared.Exists("match:{mq}:index"))
}
