package config

import (
	"testing"
	"time"

	"shared/playerredis"
)

// 没写 Addrs 时玩家域、登录域都落在 Node.RedisClient 那一个实例的同一个库上,开发环境不用改就能跑;
// Redis 段写了的口令与超时跟着走。
func TestRedisDomainsFallBackToNodeRedisClient(t *testing.T) {
	c := Config{}
	c.Node.RedisClient = RedisConf{
		Host: "127.0.0.1:6379", Password: "pw", DB: 3, PoolSize: 17,
		DialTimeout: 2 * time.Second, ReadTimeout: 4 * time.Second, WriteTimeout: 5 * time.Second,
	}
	for name, got := range map[string]playerredis.Config{"player": c.EffectivePlayerRedis(), "login": c.EffectiveLoginRedis()} {
		if len(got.Addrs) != 1 || got.Addrs[0] != "127.0.0.1:6379" || got.Cluster || got.Password != "pw" || got.DB != 3 {
			t.Fatalf("%s 没有落在现有实例上: %+v", name, got)
		}
		if got.PoolSizePerNode != 17 || got.DialTimeoutMs != 2000 || got.ReadTimeoutMs != 4000 || got.WriteTimeoutMs != 5000 {
			t.Fatalf("%s 没有沿用现有实例的池与超时: %+v", name, got)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("%s 回落配置必须合法: %v", name, err)
		}
	}
}

// Redis 段没写池与超时:取固定口径,不是 playerredis 的 1s 缺省(读写比现在收紧 3 倍会把慢 Redis 变成登录失败)。
func TestRedisDomainFallbackDefaults(t *testing.T) {
	c := Config{}
	c.Node.RedisClient = RedisConf{Host: "127.0.0.1:6379"}
	got := c.EffectiveLoginRedis()
	if got.PoolSizePerNode != redisDomainFallbackPoolSize || got.DialTimeoutMs != redisDomainFallbackDialTimeoutMs ||
		got.ReadTimeoutMs != redisDomainFallbackIOTimeoutMs || got.WriteTimeoutMs != redisDomainFallbackIOTimeoutMs ||
		got.PoolTimeoutMs != redisDomainFallbackPoolTimeoutMs {
		t.Fatalf("回落口径不对: %+v", got)
	}
}

// 两段各走各的:登录量大到要隔离时只改一段的地址。这一段里单独写的连接池优先于回落值。
func TestRedisDomainsAreIndependentlyConfigurable(t *testing.T) {
	c := Config{}
	c.Node.RedisClient = RedisConf{Host: "control:6379", PoolSize: 20}
	c.PlayerRedis = playerredis.Config{PoolSizePerNode: 7}
	c.LoginRedis = playerredis.Config{Addrs: []string{"login-a:6379", "login-b:6379"}, Cluster: true, PoolSizePerNode: 5}

	player := c.EffectivePlayerRedis()
	if player.Addrs[0] != "control:6379" || player.PoolSizePerNode != 7 {
		t.Fatalf("PlayerRedis 没写地址应落在现有实例,连接池取单独写的值: %+v", player)
	}
	login := c.EffectiveLoginRedis()
	if len(login.Addrs) != 2 || !login.Cluster || login.PoolSizePerNode != 5 {
		t.Fatalf("LoginRedis 写了地址应原样生效: %+v", login)
	}
}

func TestValidateRedisDomains(t *testing.T) {
	good := []Config{
		{},
		{PlayerRedis: playerredis.Config{PoolSizePerNode: 20}, LoginRedis: playerredis.Config{PoolSizePerNode: 20}},
		{LoginRedis: playerredis.Config{Addrs: []string{"a:1", "b:2"}, Cluster: true}},
	}
	for i := range good {
		if err := good[i].ValidateRedisDomains(); err != nil {
			t.Errorf("good case %d: %v", i, err)
		}
	}
	bad := []Config{
		{PlayerRedis: playerredis.Config{Cluster: true}}, // 集群没写种子,不能静默落到控制域
		{LoginRedis: playerredis.Config{Cluster: true}},
		{LoginRedis: playerredis.Config{Addrs: []string{"a:1", "b:2"}}},
		{LoginRedis: playerredis.Config{Addrs: []string{"a:1"}, Cluster: true, DB: 3}},
		{PlayerRedis: playerredis.Config{PoolSizePerNode: playerredis.MaxPoolSizePerNode + 1}},
	}
	for i := range bad {
		if err := bad[i].ValidateRedisDomains(); err == nil {
			t.Errorf("bad case %d: 应拒绝", i)
		}
	}
}
