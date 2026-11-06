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

// K8s 的形状:Redis 段(控制域 redis-ctl-g<N>)与 CoreRedis 段(核心)的 Pass 都是空串,SharedRedisPasswordEnv 指名 Secret 注入的变量。
// 两段都要填上,读排空标记的核心连接与玩家域回落拿到的也是这个口令。
func TestResolveSharedRedisPasswordFillsControlAndCore(t *testing.T) {
	c := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	c.Redis.Host = "redis-ctl-g0.infra:6379"
	c.CoreRedis.Host = "redis.infra:6379"
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw-from-secret"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if c.Redis.Pass != "pw-from-secret" || c.CoreRedis.Pass != "pw-from-secret" {
		t.Fatalf("both sections must get the password: control=%q core=%q", c.Redis.Pass, c.CoreRedis.Pass)
	}
	if got := c.EffectiveCoreRedis().Pass; got != "pw-from-secret" {
		t.Fatalf("EffectiveCoreRedis must carry the password, got %q", got)
	}
	if got := c.EffectivePlayerRedis().Password; got != "pw-from-secret" {
		t.Fatalf("PlayerRedis falls back to the Redis section and must carry the same password, got %q", got)
	}
}

// 开发环境不写 CoreRedis:EffectiveCoreRedis 就是 Redis 段,同样带口令。
func TestResolveSharedRedisPasswordWithoutCoreRedisSection(t *testing.T) {
	c := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	c.Redis.Host = "redis.infra:6379"
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if got := c.EffectiveCoreRedis(); got.Host != "redis.infra:6379" || got.Pass != "pw" {
		t.Fatalf("without CoreRedis the core connection is the Redis section with the password: %+v", got)
	}
}

// 本机 etc/scene_manager_service.yaml 不写 SharedRedisPasswordEnv:照用字面量。
func TestResolveSharedRedisPasswordKeepsLiteralWithoutEnvName(t *testing.T) {
	var c Config
	c.Redis.Pass = "local"
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "other"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if c.Redis.Pass != "local" || c.CoreRedis.Pass != "" {
		t.Fatalf("literals must stay when SharedRedisPasswordEnv is not set: control=%q core=%q", c.Redis.Pass, c.CoreRedis.Pass)
	}
}

// 配置指名了变量、变量却没有值:拒启;字面量与变量同时给:拒启。报错点名,不含口令。
func TestResolveSharedRedisPasswordFailsClosed(t *testing.T) {
	missing := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	if err := missing.ResolveSharedRedisPassword(sharedRedisEnv(nil)); err == nil || !strings.Contains(err.Error(), "SHARED_REDIS_PASSWORD") {
		t.Fatalf("a missing variable must be rejected by name, got %v", err)
	}
	both := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	both.CoreRedis.Pass = "stale-literal"
	err := both.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw"}))
	if err == nil || !strings.Contains(err.Error(), "CoreRedis.Pass") || strings.Contains(err.Error(), "stale-literal") {
		t.Fatalf("a literal next to the env name must be rejected by key without echoing it, got %v", err)
	}
}
