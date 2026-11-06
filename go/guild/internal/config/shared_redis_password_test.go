package config

import (
	"strings"
	"testing"
)

// sharedRedisEnv 是 os.LookupEnv 形状的桩,单测不读真实环境变量。
func sharedRedisEnv(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

// K8s 的形状:RedisClient 与 PlayerLocatorRedis 的 Password 都是空串,SharedRedisPasswordEnv 指名 Secret 注入的变量。
// 两段都要填上;玩家域没写 Addrs 时回落到 PlayerLocatorRedis,也要拿到同一个口令。
func TestResolveSharedRedisPasswordFillsBothSections(t *testing.T) {
	c := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	c.RedisClient = RedisConf{Host: "redis.infra:6379"}
	c.PlayerLocatorRedis = RedisConf{Host: "redis.infra:6379"}
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw-from-secret"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if c.RedisClient.Password != "pw-from-secret" || c.PlayerLocatorRedis.Password != "pw-from-secret" {
		t.Fatalf("both sections must get the password: cache=%q locator=%q", c.RedisClient.Password, c.PlayerLocatorRedis.Password)
	}
	if got := c.EffectivePlayerRedis().Password; got != "pw-from-secret" {
		t.Fatalf("PlayerRedis falls back to PlayerLocatorRedis and must carry the same password, got %q", got)
	}
}

// PlayerLocatorRedis 没写 Host 时回落 RedisClient:玩家域同样带口令。
func TestResolveSharedRedisPasswordWithLocatorFallback(t *testing.T) {
	c := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	c.RedisClient = RedisConf{Host: "redis.infra:6379"}
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if got := c.EffectivePlayerRedis(); got.Addrs[0] != "redis.infra:6379" || got.Password != "pw" {
		t.Fatalf("PlayerRedis falls back to RedisClient with the password: %+v", got)
	}
}

// 本机 etc/guild.yaml 不写 SharedRedisPasswordEnv:照用字面量。
func TestResolveSharedRedisPasswordKeepsLiteralWithoutEnvName(t *testing.T) {
	var c Config
	c.RedisClient.Password = "local"
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "other"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if c.RedisClient.Password != "local" || c.PlayerLocatorRedis.Password != "" {
		t.Fatalf("literals must stay when SharedRedisPasswordEnv is not set: cache=%q locator=%q", c.RedisClient.Password, c.PlayerLocatorRedis.Password)
	}
}

// 配置指名了变量、变量却没有值:拒启;字面量与变量同时给:拒启。报错点名,不含口令。
func TestResolveSharedRedisPasswordFailsClosed(t *testing.T) {
	missing := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	if err := missing.ResolveSharedRedisPassword(sharedRedisEnv(nil)); err == nil || !strings.Contains(err.Error(), "SHARED_REDIS_PASSWORD") {
		t.Fatalf("a missing variable must be rejected by name, got %v", err)
	}
	both := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	both.PlayerLocatorRedis.Password = "stale-literal"
	err := both.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw"}))
	if err == nil || !strings.Contains(err.Error(), "PlayerLocatorRedis.Password") || strings.Contains(err.Error(), "stale-literal") {
		t.Fatalf("a literal next to the env name must be rejected by key without echoing it, got %v", err)
	}
}
