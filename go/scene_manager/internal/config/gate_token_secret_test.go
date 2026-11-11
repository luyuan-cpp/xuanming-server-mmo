package config

import (
	"strings"
	"testing"
)

// gateTokenEnv 把一张表包装成 os.LookupEnv 的形状,单测不读真实环境变量。
func gateTokenEnv(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func TestResolveGateTokenSecretFillsFromNamedVariable(t *testing.T) {
	c := Config{GateTokenSecretEnv: "MMORPG_GATE_TOKEN_SECRET"}
	if err := c.ResolveGateTokenSecret(gateTokenEnv(map[string]string{"MMORPG_GATE_TOKEN_SECRET": "from-secret"})); err != nil {
		t.Fatalf("ResolveGateTokenSecret: %v", err)
	}
	if c.GateTokenSecret != "from-secret" {
		t.Fatalf("GateTokenSecret = %q, want the variable's value", c.GateTokenSecret)
	}
}

func TestResolveGateTokenSecretKeepsLiteralWithoutEnvKey(t *testing.T) {
	// 本机 etc/*.yaml 不写 GateTokenSecretEnv:照用字面量,同名环境变量在也不读。
	c := Config{GateTokenSecret: "local-literal"}
	if err := c.ResolveGateTokenSecret(gateTokenEnv(map[string]string{"MMORPG_GATE_TOKEN_SECRET": "from-secret"})); err != nil {
		t.Fatalf("ResolveGateTokenSecret: %v", err)
	}
	if c.GateTokenSecret != "local-literal" {
		t.Fatalf("without GateTokenSecretEnv the literal must stay, got %q", c.GateTokenSecret)
	}
}

func TestResolveGateTokenSecretFailsClosed(t *testing.T) {
	missing := Config{GateTokenSecretEnv: "MMORPG_GATE_TOKEN_SECRET"}
	if err := missing.ResolveGateTokenSecret(gateTokenEnv(nil)); err == nil || !strings.Contains(err.Error(), "MMORPG_GATE_TOKEN_SECRET") {
		t.Fatalf("a named but unset variable must be rejected, got %v", err)
	}
	both := Config{GateTokenSecretEnv: "MMORPG_GATE_TOKEN_SECRET", GateTokenSecret: "stale-literal"}
	err := both.ResolveGateTokenSecret(gateTokenEnv(map[string]string{"MMORPG_GATE_TOKEN_SECRET": "from-secret"}))
	if err == nil || !strings.Contains(err.Error(), "GateTokenSecret") || strings.Contains(err.Error(), "stale-literal") {
		t.Fatalf("a literal next to GateTokenSecretEnv must be rejected without echoing it, got %v", err)
	}
}
