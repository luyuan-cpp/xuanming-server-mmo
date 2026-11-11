package config

import (
	"strings"
	"testing"
)

// spectateEnv 把一张表包装成 os.LookupEnv 的形状,单测不读真实环境变量。
func spectateEnv(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

const (
	spectateTestTicketSecret = "spectate-ticket-secret-0123456789abcdef"
	spectateTestOpsSecret    = "spectate-ops-caller-secret-0123456789abcd"
)

// prodSpectateConfig 是 K8s 上的形状:生产模式、中继打开、两把密钥都只写变量名。
func prodSpectateConfig() Config {
	var c Config
	c.Mode = "pro"
	c.Spectate = SpectateConf{RelayEnabled: true, TicketSecretEnv: "MMORPG_SPECTATE_TOKEN_SECRET", OpsCallerSecretEnv: "MMORPG_MATCH_OPS_CALLER_SECRET"}
	return c
}

func TestValidateSpectateDefersNamedSecretsUntilResolved(t *testing.T) {
	// conf.MustLoad 调 Validate 时密钥还没填:指名了变量的密钥先不判长度,否则 match 在 K8s 上加载配置就拒启。
	c := prodSpectateConfig()
	if err := c.validateSpectate(); err != nil {
		t.Fatalf("named but not yet resolved secrets must not fail load-time validation, got %v", err)
	}
}

func TestResolveSpectateSecretsFillsAndValidates(t *testing.T) {
	c := prodSpectateConfig()
	err := c.ResolveSpectateSecrets(spectateEnv(map[string]string{
		"MMORPG_SPECTATE_TOKEN_SECRET":   spectateTestTicketSecret,
		"MMORPG_MATCH_OPS_CALLER_SECRET": spectateTestOpsSecret,
	}))
	if err != nil {
		t.Fatalf("ResolveSpectateSecrets: %v", err)
	}
	if c.Spectate.TicketSecret != spectateTestTicketSecret || c.Spectate.OpsCallerSecret != spectateTestOpsSecret {
		t.Fatalf("both secrets must come from their variables, got ticket=%q ops=%q", c.Spectate.TicketSecret, c.Spectate.OpsCallerSecret)
	}
	if !c.Spectate.RelayUsable() || !c.Spectate.OpsEntryEnabled() {
		t.Fatal("relay and ops entry must be usable once the secrets are filled")
	}
}

func TestResolveSpectateSecretsChecksLengthAfterFilling(t *testing.T) {
	c := prodSpectateConfig()
	err := c.ResolveSpectateSecrets(spectateEnv(map[string]string{
		"MMORPG_SPECTATE_TOKEN_SECRET":   "short",
		"MMORPG_MATCH_OPS_CALLER_SECRET": spectateTestOpsSecret,
	}))
	if err == nil || !strings.Contains(err.Error(), "Spectate.TicketSecret") || strings.Contains(err.Error(), "short") {
		t.Fatalf("a too short secret from the variable must be rejected without echoing it, got %v", err)
	}
}

func TestResolveSpectateSecretsFailsClosedOnMissingVariable(t *testing.T) {
	c := prodSpectateConfig()
	err := c.ResolveSpectateSecrets(spectateEnv(map[string]string{"MMORPG_SPECTATE_TOKEN_SECRET": spectateTestTicketSecret}))
	if err == nil || !strings.Contains(err.Error(), "MMORPG_MATCH_OPS_CALLER_SECRET") {
		t.Fatalf("a named but unset variable must be rejected, got %v", err)
	}
}

func TestResolveSpectateSecretsKeepsLiteralsWithoutEnvKeys(t *testing.T) {
	// 本机 etc/match_service.yaml 不写指名键:照用字面量,同名环境变量在也不读;加载期的长度校验照旧生效。
	var c Config
	c.Mode = "pro"
	c.Spectate = SpectateConf{RelayEnabled: true, TicketSecret: "too-short-literal"}
	if err := c.validateSpectate(); err == nil {
		t.Fatal("a short literal must still fail validation when no variable is named")
	}
	c.Spectate.TicketSecret = spectateTestTicketSecret
	if err := c.ResolveSpectateSecrets(spectateEnv(map[string]string{"MMORPG_SPECTATE_TOKEN_SECRET": "from-env"})); err != nil {
		t.Fatalf("ResolveSpectateSecrets: %v", err)
	}
	if c.Spectate.TicketSecret != spectateTestTicketSecret {
		t.Fatalf("without TicketSecretEnv the literal must stay, got %q", c.Spectate.TicketSecret)
	}
}
