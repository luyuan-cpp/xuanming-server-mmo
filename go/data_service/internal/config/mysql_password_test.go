package config

import (
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// mysqlEnv 把一张表包装成 os.LookupEnv 的形状,单测不读真实环境变量。
func mysqlEnv(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func TestResolveMySQLPasswordFillsFromNamedVariable(t *testing.T) {
	c, err := Config{MySQLPasswordEnv: "MMORPG_MYSQL_PASSWORD"}.ResolveMySQLPassword(mysqlEnv(map[string]string{"MMORPG_MYSQL_PASSWORD": "from-secret"}))
	if err != nil {
		t.Fatalf("ResolveMySQLPassword: %v", err)
	}
	if c.SnapshotMySQL.Password != "from-secret" {
		t.Fatalf("SnapshotMySQL.Password = %q, want the variable's value", c.SnapshotMySQL.Password)
	}
}

func TestResolveMySQLPasswordKeepsLiteralWithoutEnvKey(t *testing.T) {
	// 本机 etc/data_service.yaml 不写 MySQLPasswordEnv:照用字面量,同名环境变量在也不读。
	c, err := Config{SnapshotMySQL: SnapshotMySQLConfig{Password: "local-literal"}}.ResolveMySQLPassword(mysqlEnv(map[string]string{"MMORPG_MYSQL_PASSWORD": "from-secret"}))
	if err != nil {
		t.Fatalf("ResolveMySQLPassword: %v", err)
	}
	if c.SnapshotMySQL.Password != "local-literal" {
		t.Fatalf("without MySQLPasswordEnv the literal must stay, got %q", c.SnapshotMySQL.Password)
	}
}

func TestResolveMySQLPasswordFailsClosed(t *testing.T) {
	if _, err := (Config{MySQLPasswordEnv: "MMORPG_MYSQL_PASSWORD"}).ResolveMySQLPassword(mysqlEnv(nil)); err == nil || !strings.Contains(err.Error(), "MMORPG_MYSQL_PASSWORD") {
		t.Fatalf("a named but unset variable must be rejected, got %v", err)
	}
	both := Config{MySQLPasswordEnv: "MMORPG_MYSQL_PASSWORD", SnapshotMySQL: SnapshotMySQLConfig{Password: "stale-literal"}}
	_, err := both.ResolveMySQLPassword(mysqlEnv(map[string]string{"MMORPG_MYSQL_PASSWORD": "from-secret"}))
	if err == nil || !strings.Contains(err.Error(), "SnapshotMySQL.Password") || strings.Contains(err.Error(), "stale-literal") {
		t.Fatalf("a literal next to MySQLPasswordEnv must be rejected without echoing it, got %v", err)
	}
}

// SnapshotMySQLConfig.Password 带缺省值 apppass123,而部署写的是显式空串 Password: "" 再指名变量。这条钉住 go-zero 对显式空串
// 不回填缺省值:回填了,ResolveMySQLPassword 会把缺省值当成字面量而拒启,data_service 在 K8s 上起不来。
func TestExplicitEmptySnapshotPasswordStaysEmptyAfterLoad(t *testing.T) {
	var section struct {
		SnapshotMySQL SnapshotMySQLConfig `json:",optional"`
	}
	if err := conf.LoadFromYamlBytes([]byte("SnapshotMySQL:\n  Password: \"\"\n"), &section); err != nil {
		t.Fatalf("LoadFromYamlBytes: %v", err)
	}
	if section.SnapshotMySQL.Password != "" {
		t.Fatalf("an explicit empty Password must stay empty after loading, got %q", section.SnapshotMySQL.Password)
	}
}
