package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeDDL(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 网关的 schema.sql 是 mmorpg 业务表在 TiDB 上的唯一来源(docs/design/no-single-node-horizontal-scaling-20261001.md §6.3):
// 它必须能整份通过 -ddl-file 的检查,拆出来的就是三张表加一行种子。
func TestLoadBusinessDDLAcceptsGatewaySchema(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("取不到源文件路径")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", "..", "..",
		"java", "gateway_node", "src", "main", "resources", "schema.sql"))
	statements, err := loadBusinessDDL(path)
	if err != nil {
		t.Fatal(err)
	}
	creates, inserts := 0, 0
	for _, statement := range statements {
		switch {
		case strings.HasPrefix(statement, "CREATE TABLE IF NOT EXISTS "):
			creates++
		case strings.HasPrefix(statement, "INSERT IGNORE INTO "):
			inserts++
		default:
			t.Fatalf("意外的语句: %.60s", statement)
		}
		if strings.HasPrefix(strings.TrimSpace(statement), "--") {
			t.Fatalf("整行注释没有去掉: %.60s", statement)
		}
	}
	if creates != 3 || inserts != 1 {
		t.Fatalf("CREATE %d 句、INSERT IGNORE %d 句,want 3 和 1", creates, inserts)
	}
}

// 只认两种幂等语句,表名不带库名;有一句不合格就整份拒绝。大小写不敏感,CRLF 行尾照样认。
func TestLoadBusinessDDLRejectsUnsafeStatements(t *testing.T) {
	good := "CREATE TABLE IF NOT EXISTS t (id BIGINT PRIMARY KEY);\n"
	for _, content := range []string{
		"USE mmorpg;",
		"DROP TABLE t;",
		"INSERT INTO t VALUES (1);",
		"ALTER TABLE t ADD c INT;",
		"CREATE TABLE t (id INT);",
		"CREATE TABLE IF NOT EXISTS other.t (id INT);",
		"INSERT IGNORE INTO `other`.`t` VALUES (1);",
		good + "DELETE FROM t;",
		"-- only a comment\n",
		"",
	} {
		if _, err := loadBusinessDDL(writeDDL(t, content)); err == nil {
			t.Errorf("应当拒绝: %q", content)
		}
	}
	if _, err := loadBusinessDDL(filepath.Join("relative", "schema.sql")); err == nil {
		t.Error("相对路径应当拒绝")
	}
	statements, err := loadBusinessDDL(writeDDL(t, "-- 注释\r\n"+good+"insert ignore into t (id) values (1);\r\n"))
	if err != nil || len(statements) != 2 {
		t.Fatalf("statements=%v err=%v, want 2 句", statements, err)
	}
}

// -ddl-file 只用于 prepare-schema;文件不合格时在读取任何连接能力之前就拒绝,闸门表也不建。
func TestDDLFileFlagIsPrepareSchemaOnlyAndCheckedBeforeSQL(t *testing.T) {
	bad := writeDDL(t, "DROP TABLE t;")
	for _, args := range [][]string{
		{"-command", "status", "-ddl-file", bad},
		{"-command", "prepare-schema", "-schema", "mmorpg", "-ddl-file", bad},
	} {
		reads := 0
		getenv := func(string) string { reads++; return "" }
		if err := run(context.Background(), args, getenv, &bytes.Buffer{}); err == nil || reads != 0 {
			t.Fatalf("args=%v: 应在读取能力前拒绝,env_reads=%d err=%v", args, reads, err)
		}
	}
}
