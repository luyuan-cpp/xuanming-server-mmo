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

// K8s 的形状:Redis.Pass 是空串,SharedRedisPasswordEnv 指名 Secret 注入的变量。玩家域回落也要拿到同一个口令;
// FriendRedis 是独立实例,不能被填上共享 Redis 的口令。
func TestResolveSharedRedisPasswordFillsRedisSection(t *testing.T) {
	c := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	c.Redis.Host = "redis.infra:6379"
	c.FriendRedis.Host = "redis-friend:6379"
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw-from-secret"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if c.Redis.Pass != "pw-from-secret" {
		t.Fatalf("the Redis section must get the password, got %q", c.Redis.Pass)
	}
	if got := c.EffectivePlayerRedis().Password; got != "pw-from-secret" {
		t.Fatalf("PlayerRedis falls back to the Redis section and must carry the same password, got %q", got)
	}
	if c.FriendRedis.Pass != "" {
		t.Fatalf("FriendRedis is a separate instance and must not get the shared password, got %q", c.FriendRedis.Pass)
	}
}

// 本机 etc/friend.yaml 不写 SharedRedisPasswordEnv:照用字面量。
func TestResolveSharedRedisPasswordKeepsLiteralWithoutEnvName(t *testing.T) {
	var c Config
	c.Redis.Pass = "local"
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "other"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if c.Redis.Pass != "local" {
		t.Fatalf("literal password must stay when SharedRedisPasswordEnv is not set, got %q", c.Redis.Pass)
	}
}

// 配置指名了变量、变量却没有值:拒启;字面量与变量同时给:拒启。报错点名,不含口令。
func TestResolveSharedRedisPasswordFailsClosed(t *testing.T) {
	missing := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	if err := missing.ResolveSharedRedisPassword(sharedRedisEnv(nil)); err == nil || !strings.Contains(err.Error(), "SHARED_REDIS_PASSWORD") {
		t.Fatalf("a missing variable must be rejected by name, got %v", err)
	}
	both := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	both.Redis.Pass = "stale-literal"
	err := both.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw"}))
	if err == nil || !strings.Contains(err.Error(), "Redis.Pass") || strings.Contains(err.Error(), "stale-literal") {
		t.Fatalf("a literal next to the env name must be rejected by key without echoing it, got %v", err)
	}
}
