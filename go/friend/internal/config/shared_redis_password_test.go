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

// K8s 的形状:ControlRedis.Password 是空串,SharedRedisPasswordEnv 指名 Secret 注入的变量。玩家域回落也要拿到同一个口令;
// 写了自己 Addrs 的玩家域是另一套实例,不能被填上控制集群的口令。
func TestResolveSharedRedisPasswordFillsControlSection(t *testing.T) {
	c := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	c.ControlRedis.Addrs = []string{"redis.infra:6379"}
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw-from-secret"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if got := c.EffectiveControlRedis().Password; got != "pw-from-secret" {
		t.Fatalf("the ControlRedis section must get the password, got %q", got)
	}
	if got := c.EffectivePlayerRedis().Password; got != "pw-from-secret" {
		t.Fatalf("PlayerRedis falls back to ControlRedis and must carry the same password, got %q", got)
	}
	c.PlayerRedis.Addrs = []string{"redis-player:7000"}
	if got := c.EffectivePlayerRedis().Password; got != "" {
		t.Fatalf("a separate PlayerRedis must not get the control password, got %q", got)
	}
}

// 本机 etc/friend.yaml 不写 SharedRedisPasswordEnv:照用字面量。
func TestResolveSharedRedisPasswordKeepsLiteralWithoutEnvName(t *testing.T) {
	var c Config
	c.ControlRedis.Password = "local"
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "other"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if c.ControlRedis.Password != "local" {
		t.Fatalf("literal password must stay when SharedRedisPasswordEnv is not set, got %q", c.ControlRedis.Password)
	}
}

// 配置指名了变量、变量却没有值:拒启;字面量与变量同时给:拒启。报错点名,不含口令。
func TestResolveSharedRedisPasswordFailsClosed(t *testing.T) {
	missing := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	if err := missing.ResolveSharedRedisPassword(sharedRedisEnv(nil)); err == nil || !strings.Contains(err.Error(), "SHARED_REDIS_PASSWORD") {
		t.Fatalf("a missing variable must be rejected by name, got %v", err)
	}
	both := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	both.ControlRedis.Password = "stale-literal"
	err := both.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw"}))
	if err == nil || !strings.Contains(err.Error(), "ControlRedis.Password") || strings.Contains(err.Error(), "stale-literal") {
		t.Fatalf("a literal next to the env name must be rejected by key without echoing it, got %v", err)
	}
}
