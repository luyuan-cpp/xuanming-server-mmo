package config

import (
	"path/filepath"
	"testing"
	"time"

	"shared/playerredis"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/conf"
)

// 仓内 etc/match_service.yaml 必须能加载并通过 Validate。
func TestMatchServiceYamlLoads(t *testing.T) {
	var c Config
	require.NoError(t, conf.Load(filepath.Join("..", "..", "etc", "match_service.yaml"), &c))
}

// validForTest 是能过 Validate 的最小配置:SQL 权威与原角色身份来源都配齐,其余取零值。
func validForTest() Config {
	c := Config{}
	c.Mode = "pro"
	c.GlobalMatch = GlobalMatchConf{Backend: "tidb", Host: "tidb:4000", User: "runtime"}
	c.GlobalLoginSessions = true
	c.LoginRedis.Addrs = []string{"redis:6379"}
	return c
}

// SQL 权威是匹配唯一路径,开发环境与生产同一份代码:缺入口或缺原角色身份来源都不能起服,哪一档都一样。
func TestAuthorityConfigIsRequiredInEveryMode(t *testing.T) {
	for _, mode := range []string{"dev", "test", "pro"} {
		c := validForTest()
		c.Mode = mode
		require.NoError(t, c.Validate(), mode)
		for name, mutate := range map[string]func(c *Config){
			"缺 SQL 入口":      func(c *Config) { c.GlobalMatch.Host = " " },
			"缺 SQL 账号":      func(c *Config) { c.GlobalMatch.User = "" },
			"未知后端":          func(c *Config) { c.GlobalMatch.Backend = "oracle" },
			"未开原角色身份":       func(c *Config) { c.GlobalLoginSessions = false },
			"缺 login Redis": func(c *Config) { c.LoginRedis.Addrs = nil },
		} {
			c := validForTest()
			c.Mode = mode
			mutate(&c)
			require.Error(t, c.Validate(), "%s/%s", mode, name)
		}
	}
}

// 后端只认 tidb / mysql,且都是同一条路径。
func TestAuthorityBackendAcceptsMySQL(t *testing.T) {
	c := validForTest()
	c.GlobalMatch.Backend = "mysql"
	require.NoError(t, c.Validate())
}

// 结果 topic 的耐久门禁只看部署档:本地 Kafka 通常单 broker,dev / test 放行;其余一律严格,与 SQL 后端无关。
func TestResultTopicDurabilityFollowsMode(t *testing.T) {
	for mode, want := range map[string]bool{"dev": false, "test": false, "pre": true, "pro": true, "rt": true} {
		c := validForTest()
		c.Mode = mode
		require.Equal(t, want, c.ResultTopicRequiresDurability(), mode)
	}
	c := validForTest()
	c.Mode = "dev"
	c.GlobalMatch.Backend = "mysql"
	require.False(t, c.ResultTopicRequiresDurability())
	c.Mode = "pro"
	require.True(t, c.ResultTopicRequiresDurability(), "生产档配 mysql 也不放宽")
}

// 2026-10-30(评审 B-6):终局发件箱的节拍、批量与并发进配置。仓内 yaml 写的就是之前写死的值
// (2 秒 / 结果 8 行 / 首投 4 行 / 等待 4 行 / 串行);整段漏写时节拍按 2 秒,批量与并发的零值由 authority 换成同样的默认值。
func TestOutcomeOutboxConfigMatchesPreviousBehavior(t *testing.T) {
	var c Config
	require.NoError(t, conf.Load(filepath.Join("..", "..", "etc", "match_service.yaml"), &c))
	require.Equal(t, OutcomeOutboxConf{IntervalMs: 2000, ResultBatch: 8, FirstBatch: 4, WaitingBatch: 4, Concurrency: 1}, c.GlobalMatch.OutcomeOutbox)
	require.Equal(t, 2*time.Second, c.GlobalMatch.OutcomeOutboxInterval())

	require.Equal(t, 2*time.Second, GlobalMatchConf{}.OutcomeOutboxInterval(), "直接构造 Config 的零值按默认 2 秒")
	require.Equal(t, 2*time.Second, GlobalMatchConf{OutcomeOutbox: OutcomeOutboxConf{IntervalMs: -5}}.OutcomeOutboxInterval())
	require.Equal(t, 500*time.Millisecond, GlobalMatchConf{OutcomeOutbox: OutcomeOutboxConf{IntervalMs: 500}}.OutcomeOutboxInterval())
}

// 没写 PlayerRedis.Addrs:玩家域指向 Redis 段那一个实例;写了就完全按它来。
func TestEffectivePlayerRedis(t *testing.T) {
	var c Config
	c.Redis.Host = "redis.infra:6379"
	c.Redis.Pass = "shared-secret"
	got := c.EffectivePlayerRedis()
	require.Equal(t, []string{"redis.infra:6379"}, got.Addrs)
	require.False(t, got.Cluster)
	require.Equal(t, "shared-secret", got.Password)
	require.Equal(t, playerRedisFallbackPoolSize, got.PoolSizePerNode)
	require.NoError(t, got.Validate())

	c.PlayerRedis = playerredis.Config{Addrs: []string{"a:7000", "b:7000"}, Cluster: true}
	got = c.EffectivePlayerRedis()
	require.Equal(t, []string{"a:7000", "b:7000"}, got.Addrs)
	require.True(t, got.Cluster)
	require.Empty(t, got.Password)
}

// etc 下的 yaml 不写地址,只写明连接池上限。
func TestMatchServiceYamlPlayerRedisStaysOnSharedInstance(t *testing.T) {
	var c Config
	require.NoError(t, conf.Load(filepath.Join("..", "..", "etc", "match_service.yaml"), &c))
	got := c.EffectivePlayerRedis()
	require.Equal(t, []string{c.Redis.Host}, got.Addrs)
	require.Equal(t, 20, got.PoolSizePerNode)
}
