package config

import (
	"strings"
	"testing"
)

// mysqlEnv 把一张表包装成 os.LookupEnv 的形状,单测不读真实环境变量。
func mysqlEnv(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func TestResolveMySQLPasswordFillsFromNamedVariable(t *testing.T) {
	c := Config{MySQLPasswordEnv: "MMORPG_MYSQL_PASSWORD"}
	if err := c.ResolveMySQLPassword(mysqlEnv(map[string]string{"MMORPG_MYSQL_PASSWORD": "from-secret"})); err != nil {
		t.Fatalf("ResolveMySQLPassword: %v", err)
	}
	if c.MySQL.Password != "from-secret" {
		t.Fatalf("MySQL.Password = %q, want the variable's value", c.MySQL.Password)
	}
}

func TestResolveMySQLPasswordKeepsLiteralWithoutEnvKey(t *testing.T) {
	// 本机 etc/player_inbox.yaml 不写 MySQLPasswordEnv:照用字面量,同名环境变量在也不读。
	c := Config{MySQL: MySQLConf{Password: "local-literal"}}
	if err := c.ResolveMySQLPassword(mysqlEnv(map[string]string{"MMORPG_MYSQL_PASSWORD": "from-secret"})); err != nil {
		t.Fatalf("ResolveMySQLPassword: %v", err)
	}
	if c.MySQL.Password != "local-literal" {
		t.Fatalf("without MySQLPasswordEnv the literal must stay, got %q", c.MySQL.Password)
	}
}

func TestResolveMySQLPasswordFailsClosed(t *testing.T) {
	missing := Config{MySQLPasswordEnv: "MMORPG_MYSQL_PASSWORD"}
	if err := missing.ResolveMySQLPassword(mysqlEnv(nil)); err == nil || !strings.Contains(err.Error(), "MMORPG_MYSQL_PASSWORD") {
		t.Fatalf("a named but unset variable must be rejected, got %v", err)
	}
	both := Config{MySQLPasswordEnv: "MMORPG_MYSQL_PASSWORD", MySQL: MySQLConf{Password: "stale-literal"}}
	err := both.ResolveMySQLPassword(mysqlEnv(map[string]string{"MMORPG_MYSQL_PASSWORD": "from-secret"}))
	if err == nil || !strings.Contains(err.Error(), "MySQL.Password") || strings.Contains(err.Error(), "stale-literal") {
		t.Fatalf("a literal next to MySQLPasswordEnv must be rejected without echoing it, got %v", err)
	}
}
