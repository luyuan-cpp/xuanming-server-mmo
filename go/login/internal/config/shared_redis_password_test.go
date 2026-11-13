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

// K8s 的形状:ConfigMap 里两段 Password 都是空串,SharedRedisPasswordEnv 指名 Secret 注入的变量。
// 两段都要填上;玩家域与登录域没写 Addrs 时回落到 Node.RedisClient,也要拿到同一个口令。
func TestResolveSharedRedisPasswordFillsNodeRedisSections(t *testing.T) {
	c := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	c.Node.RedisClient = RedisConf{Host: "redis.infra:6379"}
	c.Node.QueueRedis = &RedisConf{Host: "redis-queue-g0.infra:6379"}
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw-from-secret"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if c.Node.RedisClient.Password != "pw-from-secret" || c.Node.QueueRedis.Password != "pw-from-secret" {
		t.Fatalf("both Node Redis sections must get the password: core=%q queue=%q", c.Node.RedisClient.Password, c.Node.QueueRedis.Password)
	}
	if got := c.EffectivePlayerRedis().Password; got != "pw-from-secret" {
		t.Fatalf("PlayerRedis falls back to Node.RedisClient and must carry the same password, got %q", got)
	}
	if got := c.EffectiveLoginRedis().Password; got != "pw-from-secret" {
		t.Fatalf("LoginRedis falls back to Node.RedisClient and must carry the same password, got %q", got)
	}
}

// 不拆排队实例时 QueueRedis 整段不写(nil):只填 RedisClient,不能空指针。
func TestResolveSharedRedisPasswordWithoutQueueRedis(t *testing.T) {
	c := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if c.Node.RedisClient.Password != "pw" || c.Node.QueueRedis != nil {
		t.Fatalf("only RedisClient is filled when QueueRedis is absent: %+v", c.Node)
	}
}

// 本机 etc/login.yaml 不写 SharedRedisPasswordEnv:照用字面量,机器上的同名环境变量不读。
func TestResolveSharedRedisPasswordKeepsLiteralWithoutEnvName(t *testing.T) {
	c := Config{}
	c.Node.RedisClient.Password = "local"
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "other"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if c.Node.RedisClient.Password != "local" {
		t.Fatalf("literal password must stay when SharedRedisPasswordEnv is not set, got %q", c.Node.RedisClient.Password)
	}
}

// 配置指名了变量、变量却没有值:拒启;字面量与变量同时给:拒启。报错点名,不含口令。
func TestResolveSharedRedisPasswordFailsClosed(t *testing.T) {
	missing := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	if err := missing.ResolveSharedRedisPassword(sharedRedisEnv(nil)); err == nil || !strings.Contains(err.Error(), "SHARED_REDIS_PASSWORD") {
		t.Fatalf("a missing variable must be rejected by name, got %v", err)
	}
	both := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	both.Node.QueueRedis = &RedisConf{Password: "stale-literal"}
	err := both.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw"}))
	if err == nil || !strings.Contains(err.Error(), "Node.QueueRedis.Password") || strings.Contains(err.Error(), "stale-literal") {
		t.Fatalf("a literal next to the env name must be rejected by key without echoing it, got %v", err)
	}
}

// 控制集群写了地址(上了 redis-control)时,它的口令也由 SharedRedisPasswordEnv 填;同时写字面量与变量名照样拒启。
func TestResolveSharedRedisPasswordFillsControlRedis(t *testing.T) {
	c := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	c.ControlRedis.Addrs = []string{"redis-control-0:6379", "redis-control-1:6379"}
	c.ControlRedis.Cluster = true
	if err := c.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw-from-secret"})); err != nil {
		t.Fatalf("ResolveSharedRedisPassword: %v", err)
	}
	if got := c.EffectiveControlRedis(); got.Password != "pw-from-secret" || !got.Cluster {
		t.Fatalf("ControlRedis must carry the password from the env, got %+v", got.Describe())
	}

	both := Config{SharedRedisPasswordEnv: "SHARED_REDIS_PASSWORD"}
	both.ControlRedis.Password = "stale-literal"
	err := both.ResolveSharedRedisPassword(sharedRedisEnv(map[string]string{"SHARED_REDIS_PASSWORD": "pw"}))
	if err == nil || !strings.Contains(err.Error(), "ControlRedis.Password") || strings.Contains(err.Error(), "stale-literal") {
		t.Fatalf("a literal next to the env name must be rejected by key without echoing it, got %v", err)
	}
}
