package config

import (
	"testing"
	"time"

	"shared/playerredis"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 没写 PlayerRedis.Addrs、也没写 CoreRedis(开发环境):指向 Redis 段那一个实例,连接身份原样带过去,不用改配置。
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

// K8s 没建玩家域集群时的形状:Redis 段是控制域实例 redis-ctl-g<N>,CoreRedis 是核心,PlayerRedis 只写池上限。
// 玩家域必须回落到核心:别的服务与 C++ scene 的玩家域都在核心,回落到控制域会让 location / owner_epoch 有两份
// (control-plane-zone-scoping-20261105.md §4.2)。连接身份取核心那一段,不混进控制域的。
func TestEffectivePlayerRedisFallsBackToCoreNotControlInstance(t *testing.T) {
	var c Config
	c.Redis.Host = "redis-ctl-g0.infra:6379"
	c.Redis.User = "control-user"
	c.Redis.Pass = "control"
	c.CoreRedis.Host = "redis.infra:6379"
	c.CoreRedis.User = "core-user"
	c.CoreRedis.Pass = "core"
	c.PlayerRedis.PoolSizePerNode = 7

	got := c.EffectivePlayerRedis()
	assert.Equal(t, []string{"redis.infra:6379"}, got.Addrs)
	assert.False(t, got.Cluster)
	assert.Equal(t, "core-user", got.Username)
	assert.Equal(t, "core", got.Password)
	assert.False(t, got.TLS)
	assert.Equal(t, 7, got.PoolSizePerNode)
	require.NoError(t, got.Validate())
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
