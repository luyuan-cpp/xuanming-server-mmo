package svc

import (
	"testing"

	"match/internal/config"

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
