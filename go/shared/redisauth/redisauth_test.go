package redisauth

import (
	"strings"
	"testing"
)

// lookupOf 把一张表包装成 os.LookupEnv 的形状,单测不读真实环境变量。
func lookupOf(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func TestApplyWithoutEnvNameKeepsLiterals(t *testing.T) {
	core, queue := "", "local-literal"
	// 本机 etc/*.yaml 与 dev 档 ConfigMap 不写 SharedRedisPasswordEnv:照用字面量,同名环境变量在也不读。
	err := Apply("", lookupOf(map[string]string{"SHARED_REDIS_PASSWORD": "from-env"}),
		Target{Field: "RedisClient.Password", Password: &core},
		Target{Field: "QueueRedis.Password", Password: &queue})
	if err != nil {
		t.Fatalf("Apply with an empty env name must be a no-op, got %v", err)
	}
	if core != "" || queue != "local-literal" {
		t.Fatalf("literals must stay untouched, got core=%q queue=%q", core, queue)
	}
}

func TestApplyFillsEveryTarget(t *testing.T) {
	core, ctl := "", ""
	err := Apply("SHARED_REDIS_PASSWORD", lookupOf(map[string]string{"SHARED_REDIS_PASSWORD": "s3cret-value"}),
		Target{Field: "CoreRedis.Pass", Password: &core},
		Target{Field: "Redis.Pass", Password: &ctl})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if core != "s3cret-value" || ctl != "s3cret-value" {
		t.Fatalf("every target must get the variable's value, got core=%q ctl=%q", core, ctl)
	}
}

func TestApplyKeepsValueVerbatim(t *testing.T) {
	// 口令原样写入,不去空白、不展开 $:Redis 服务端与 C++ 拿到的都是 Secret 里的原值,这里也不能改。
	const raw = " p@%^:,~#$!&*()=+.${HOME} "
	pass := ""
	if err := Apply("SHARED_REDIS_PASSWORD", lookupOf(map[string]string{"SHARED_REDIS_PASSWORD": raw}), Target{Field: "Redis.Pass", Password: &pass}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if pass != raw {
		t.Fatalf("password must be copied verbatim, got %q", pass)
	}
}

func TestApplyRejectsUnsetOrEmptyVariable(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"unset", map[string]string{}},
		{"empty", map[string]string{"SHARED_REDIS_PASSWORD": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pass := ""
			err := Apply("SHARED_REDIS_PASSWORD", lookupOf(tc.env), Target{Field: "SharedRedis.Password", Password: &pass})
			if err == nil {
				t.Fatal("a named but missing variable must fail closed")
			}
			if !strings.Contains(err.Error(), "SHARED_REDIS_PASSWORD") {
				t.Fatalf("error must name the variable, got %v", err)
			}
			if pass != "" {
				t.Fatalf("target must stay untouched on error, got %q", pass)
			}
		})
	}
}

func TestApplyRejectsLiteralAlongsideEnv(t *testing.T) {
	const secret = "the-real-secret"
	first, second := "", "stale-literal"
	err := Apply("SHARED_REDIS_PASSWORD", lookupOf(map[string]string{"SHARED_REDIS_PASSWORD": secret}),
		Target{Field: "RedisClient.Password", Password: &first},
		Target{Field: "PlayerLocatorRedis.Password", Password: &second})
	if err == nil {
		t.Fatal("a literal password next to SharedRedisPasswordEnv must be rejected")
	}
	if !strings.Contains(err.Error(), "PlayerLocatorRedis.Password") {
		t.Fatalf("error must name the conflicting key, got %v", err)
	}
	if strings.Contains(err.Error(), "stale-literal") || strings.Contains(err.Error(), secret) {
		t.Fatalf("error must not echo any password, got %v", err)
	}
	if first != "" || second != "stale-literal" {
		t.Fatalf("no target may be written when the call fails, got first=%q second=%q", first, second)
	}
}

func TestApplyRejectsMissingField(t *testing.T) {
	err := Apply("SHARED_REDIS_PASSWORD", lookupOf(map[string]string{"SHARED_REDIS_PASSWORD": "x"}), Target{Field: "Redis.Pass"})
	if err == nil || !strings.Contains(err.Error(), "Redis.Pass") {
		t.Fatalf("a target without a field pointer is a wiring bug and must be reported by key, got %v", err)
	}
}

func TestApplyRejectsNilLookup(t *testing.T) {
	pass := ""
	if err := Apply("SHARED_REDIS_PASSWORD", nil, Target{Field: "Redis.Pass", Password: &pass}); err == nil {
		t.Fatal("a nil lookup must be rejected instead of panicking")
	}
}
