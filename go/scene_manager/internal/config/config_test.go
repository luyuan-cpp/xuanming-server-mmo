package config

import (
	"strings"
	"testing"
	"time"

	"shared/clusterredis"
	"shared/playerredis"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 没写 ControlRedis.Addrs(开发环境):指向本机单实例,池与超时取换句柄之前 go-zero 句柄的量级;段里单独写的池优先。
func TestEffectiveControlRedisFallsBackToLocalInstance(t *testing.T) {
	var c Config
	got := c.EffectiveControlRedis()
	assert.Equal(t, []string{"127.0.0.1:6379"}, got.Addrs)
	assert.False(t, got.Cluster)
	assert.Equal(t, redisFallbackPoolSize, got.PoolSizePerNode)
	assert.Equal(t, redisFallbackIOTimeoutMs, got.ReadTimeoutMs)
	assert.Equal(t, redisFallbackIOTimeoutMs, got.WriteTimeoutMs)
	require.NoError(t, got.Validate())

	c.ControlRedis.PoolSizePerNode = 7
	c.ControlRedis.Password = "p"
	got = c.EffectiveControlRedis()
	assert.Equal(t, 7, got.PoolSizePerNode)
	assert.Equal(t, "p", got.Password)
}

// 没写 PlayerRedis.Addrs(开发环境):指向控制集群那一个实例,连接身份原样带过去;只写连接池时地址仍取控制集群。
func TestEffectivePlayerRedisFallsBackToTheControlInstance(t *testing.T) {
	var c Config
	c.ControlRedis = clusterredis.Config{Addrs: []string{"redis.infra:6379"}, Username: "u", Password: "p", TLS: true}

	got := c.EffectivePlayerRedis()
	assert.Equal(t, []string{"redis.infra:6379"}, got.Addrs)
	assert.False(t, got.Cluster)
	assert.Equal(t, "u", got.Username)
	assert.Equal(t, "p", got.Password)
	assert.True(t, got.TLS)
	assert.Equal(t, redisFallbackPoolSize, got.PoolSizePerNode)
	require.NoError(t, got.Validate())

	c.PlayerRedis.PoolSizePerNode = 7
	got = c.EffectivePlayerRedis()
	assert.Equal(t, []string{"redis.infra:6379"}, got.Addrs)
	assert.Equal(t, 7, got.PoolSizePerNode)
}

// 写了 Addrs:完全按它来,不混进控制集群的连接身份。
func TestEffectivePlayerRedisExplicitCluster(t *testing.T) {
	var c Config
	c.ControlRedis = clusterredis.Config{Addrs: []string{"redis.infra:6379"}, Password: "control"}
	c.PlayerRedis = playerredis.Config{Addrs: []string{"a:7000", "b:7000"}, Cluster: true, Password: "player"}

	got := c.EffectivePlayerRedis()
	assert.Equal(t, []string{"a:7000", "b:7000"}, got.Addrs)
	assert.True(t, got.Cluster)
	assert.Equal(t, "player", got.Password)
	require.NoError(t, got.Validate())
}

// 三段 Redis 配置的拒启条件(ValidateRedisSections):旧的 go-zero Redis 段、ControlRedis.PasswordEnv、控制集群是 Cluster 却不给
// 玩家域地址,任何一条都拒启;合法的开发与集群形状放行。
func TestValidateRedisSections(t *testing.T) {
	var dev Config
	require.NoError(t, dev.ValidateRedisSections(), "开发环境什么都不写:两个域都在本机单实例")

	legacy := Config{}
	legacy.Redis.Host = "redis-ctl-g0.infra:6379"
	err := legacy.ValidateRedisSections()
	require.Error(t, err, "旧形状(go-zero Redis 段)拒启:照它起服,控制域会落到 127.0.0.1:6379")
	assert.Contains(t, err.Error(), "ControlRedis")

	passwordEnv := Config{ControlRedis: clusterredis.Config{PasswordEnv: "X"}}
	require.Error(t, passwordEnv.ValidateRedisSections(), "ControlRedis.PasswordEnv 在变量缺失时静默落成空口令")

	clusterWithoutPlayer := Config{ControlRedis: clusterredis.Config{Addrs: []string{"c0:7000", "c1:7000"}, Cluster: true}}
	err = clusterWithoutPlayer.ValidateRedisSections()
	require.Error(t, err, "控制集群是 Cluster 时玩家域不能回落到它")
	assert.Contains(t, err.Error(), "PlayerRedis.Addrs")

	cluster := clusterWithoutPlayer
	cluster.PlayerRedis = playerredis.Config{Addrs: []string{"p0:7000"}, Cluster: true}
	require.NoError(t, cluster.ValidateRedisSections())

	bad := Config{ControlRedis: clusterredis.Config{Cluster: true}}
	err = bad.ValidateRedisSections()
	require.Error(t, err, "Cluster=true 却没写种子地址是配错了")
	assert.True(t, strings.HasPrefix(err.Error(), "ControlRedis:"), err.Error())
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
