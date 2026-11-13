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

// 控制集群(gate 标记、排队)没写 Addrs 时也落在 Node.RedisClient 那一个实例上,库号、口令、池与超时跟着走;
// 写了 Addrs 就原样生效,与玩家域、登录域互不影响。
func TestControlRedisFallsBackToNodeRedisClientAndIsIndependentlyConfigurable(t *testing.T) {
	c := Config{}
	c.Node.RedisClient = RedisConf{
		Host: "127.0.0.1:6379", Password: "pw", DB: 3, PoolSize: 17,
		DialTimeout: 2 * time.Second, ReadTimeout: 4 * time.Second, WriteTimeout: 5 * time.Second,
	}
	got := c.EffectiveControlRedis()
	if len(got.Addrs) != 1 || got.Addrs[0] != "127.0.0.1:6379" || got.Cluster || got.Password != "pw" || got.DB != 3 {
		t.Fatalf("ControlRedis 没有落在现有实例上: %+v", got)
	}
	if got.PoolSizePerNode != 17 || got.DialTimeoutMs != 2000 || got.ReadTimeoutMs != 4000 || got.WriteTimeoutMs != 5000 {
		t.Fatalf("ControlRedis 没有沿用现有实例的池与超时: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("回落配置必须合法: %v", err)
	}

	c.ControlRedis = playerredis.Config{Addrs: []string{"ctl-a:6379", "ctl-b:6379"}, Cluster: true, PoolSizePerNode: 6}
	got = c.EffectiveControlRedis()
	if len(got.Addrs) != 2 || !got.Cluster || got.PoolSizePerNode != 6 || got.DB != 0 {
		t.Fatalf("ControlRedis 写了地址应原样生效: %+v", got)
	}
	if player := c.EffectivePlayerRedis(); player.Addrs[0] != "127.0.0.1:6379" || player.Cluster {
		t.Fatalf("PlayerRedis 不跟着 ControlRedis 走: %+v", player)
	}
}

// Node.QueueRedis 整段没写:排队与 gate 标记共用控制集群句柄(回 nil);写了就换成它那一个实例,
// 没写的池与超时取与 Node.RedisClient 回落相同的固定口径。
func TestEffectiveQueueRedis(t *testing.T) {
	c := Config{}
	c.Node.RedisClient = RedisConf{Host: "core:6379"}
	if got := c.EffectiveQueueRedis(); got != nil {
		t.Fatalf("没写 Node.QueueRedis 应回 nil,got %+v", got)
	}

	c.Node.QueueRedis = &RedisConf{Host: "redis-queue-g0:6379", Password: "pw", PoolSize: 9, ReadTimeout: 2 * time.Second}
	got := c.EffectiveQueueRedis()
	if got == nil || len(got.Addrs) != 1 || got.Addrs[0] != "redis-queue-g0:6379" || got.Cluster || got.Password != "pw" {
		t.Fatalf("排队实例的配置不对: %+v", got)
	}
	if got.PoolSizePerNode != 9 || got.ReadTimeoutMs != 2000 || got.WriteTimeoutMs != redisDomainFallbackIOTimeoutMs ||
		got.DialTimeoutMs != redisDomainFallbackDialTimeoutMs || got.PoolTimeoutMs != redisDomainFallbackPoolTimeoutMs {
		t.Fatalf("排队实例的池与超时不对: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("排队实例的配置必须合法: %v", err)
	}

	// 负的 PoolSize 不在这里改写,留给建客户端时拒绝(起服即失败)。
	c.Node.QueueRedis = &RedisConf{Host: "redis-queue-g0:6379", PoolSize: -1}
	if err := c.EffectiveQueueRedis().Validate(); err == nil {
		t.Fatal("负的 PoolSize 必须在建客户端时被拒绝")
	}
}

func TestValidateRedisDomainsCoversControlRedis(t *testing.T) {
	good := []Config{
		{ControlRedis: playerredis.Config{PoolSizePerNode: 20}},
		{ControlRedis: playerredis.Config{Addrs: []string{"a:1", "b:2"}, Cluster: true}},
		{ControlRedis: playerredis.Config{Addrs: []string{"a:1"}, DB: 2}},
	}
	for i := range good {
		if err := good[i].ValidateRedisDomains(); err != nil {
			t.Errorf("good case %d: %v", i, err)
		}
	}
	bad := []Config{
		{ControlRedis: playerredis.Config{Cluster: true}}, // 集群没写种子,不能静默落到现有实例
		{ControlRedis: playerredis.Config{Addrs: []string{"a:1", "b:2"}}},
		{ControlRedis: playerredis.Config{Addrs: []string{"a:1"}, Cluster: true, DB: 3}},
		{ControlRedis: playerredis.Config{PoolSizePerNode: playerredis.MaxPoolSizePerNode + 1}},
		{ControlRedis: playerredis.Config{PasswordEnv: "SOME_ENV"}}, // 变量缺失时会静默落成空口令,口令变量名只认 SharedRedisPasswordEnv
	}
	for i := range bad {
		if err := bad[i].ValidateRedisDomains(); err == nil {
			t.Errorf("bad case %d: 应拒绝", i)
		}
	}
}
