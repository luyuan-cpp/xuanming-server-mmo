package config

import (
	"testing"
	"time"

	"shared/playerredis"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 没写 PlayerRedis.Addrs:指向 Redis 段那一个实例,连接身份原样带过去,开发环境不用改配置。
func TestEffectivePlayerRedisFallsBackToRedisSection(t *testing.T) {
	var c Config
	c.Redis.Host = "redis.infra:6379"
	c.Redis.User = "u"
	c.Redis.Pass = "p"
	c.Redis.Tls = true

	got := c.EffectivePlayerRedis()
	assert.Equal(t, []string{"redis.infra:6379"}, got.Addrs)
	assert.False(t, got.Cluster)
	assert.Equal(t, "u", got.Username)
	assert.Equal(t, "p", got.Password)
	assert.True(t, got.TLS)
	assert.Equal(t, playerRedisFallbackPoolSize, got.PoolSizePerNode)
	require.NoError(t, got.Validate())

	// 只写连接池:地址仍取 Redis 段,写了的那一项生效。
	c.PlayerRedis.PoolSizePerNode = 7
	got = c.EffectivePlayerRedis()
	assert.Equal(t, []string{"redis.infra:6379"}, got.Addrs)
	assert.Equal(t, 7, got.PoolSizePerNode)
}

// 写了 Addrs:完全按它来,不混进 Redis 段的连接身份。
func TestEffectivePlayerRedisExplicitCluster(t *testing.T) {
	var c Config
	c.Redis.Host = "redis.infra:6379"
	c.Redis.Pass = "control"
	c.PlayerRedis = playerredis.Config{Addrs: []string{"a:7000", "b:7000"}, Cluster: true, Password: "player"}

	got := c.EffectivePlayerRedis()
	assert.Equal(t, []string{"a:7000", "b:7000"}, got.Addrs)
	assert.True(t, got.Cluster)
	assert.Equal(t, "player", got.Password)
	require.NoError(t, got.Validate())
}

func TestSceneMemberReconcileBudgetsAreBounded(t *testing.T) {
	var zero SceneMemberReconcileConfig
	assert.Equal(t, 10*time.Second, zero.EffectiveInterval())
	assert.Equal(t, 2000, zero.EffectiveMembersPerRound())
	assert.Equal(t, 512, zero.EffectiveScansPerRound())

	wild := SceneMemberReconcileConfig{IntervalSeconds: 1 << 40, MembersPerRound: 1 << 40, ScansPerRound: 1 << 40}
	assert.Equal(t, time.Hour, wild.EffectiveInterval())
	assert.Equal(t, maxSceneMemberReconcilePerRound, wild.EffectiveMembersPerRound())
	assert.Equal(t, maxSceneMemberReconcilePerRound, wild.EffectiveScansPerRound())

	negative := SceneMemberReconcileConfig{IntervalSeconds: -1, MembersPerRound: -1, ScansPerRound: -1}
	assert.Equal(t, 10*time.Second, negative.EffectiveInterval())
	assert.Equal(t, 2000, negative.EffectiveMembersPerRound())
	assert.Equal(t, 512, negative.EffectiveScansPerRound())
}
