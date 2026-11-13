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

// login 的口令凭据表在 mmorpg 上同样没有别的建表入口(go/login/internal/logic/pkg/auth/password_schema.go):
// 它的建表文件也必须整份通过 -ddl-file 的检查,拆出来恰好是一条建表语句。
func TestLoadBusinessDDLAcceptsLoginPasswordSchema(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("取不到源文件路径")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", "..", "login", "model", "password_auth_schema.sql"))
	statements, err := loadBusinessDDL(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 1 || !strings.HasPrefix(statements[0], "CREATE TABLE IF NOT EXISTS user_accounts ") {
		t.Fatalf("want 口令凭据表一条建表语句, got %d 句: %.80v", len(statements), statements)
	}
}

// -ddl-file 可以重复:按命令行上的先后合并;任何一个文件不合格就整体拒绝,错误只带文件名不带目录。
func TestLoadBusinessDDLFilesMergesInOrderAndRejectsAsAWhole(t *testing.T) {
	first := writeDDL(t, "CREATE TABLE IF NOT EXISTS a (id BIGINT PRIMARY KEY);\n")
	second := writeDDL(t, "CREATE TABLE IF NOT EXISTS b (id BIGINT PRIMARY KEY);\nINSERT IGNORE INTO b (id) VALUES (1);\n")
	statements, err := loadBusinessDDLFiles([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 3 || !strings.Contains(statements[0], " a ") || !strings.Contains(statements[1], " b ") {
		t.Fatalf("两个文件的语句必须按给出的先后合并: %v", statements)
	}
	none, err := loadBusinessDDLFiles(nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("不给 -ddl-file 时只建闸门表: statements=%v err=%v", none, err)
	}
	bad := writeDDL(t, "DROP TABLE a;")
	_, err = loadBusinessDDLFiles([]string{first, bad})
	if err == nil {
		t.Fatal("第二个文件不合格时必须整体拒绝")
	}
	if !strings.Contains(err.Error(), "ddl-file schema.sql:") || strings.Contains(err.Error(), filepath.Dir(bad)) {
		t.Fatalf("错误应指出是哪个文件、不带目录: %v", err)
	}

	// 命令行上重复给:两个文件都进校验,第二个不合格同样在读取任何连接能力之前拒绝。
	reads := 0
	getenv := func(string) string { reads++; return "" }
	args := []string{"-command", "prepare-schema", "-schema", "mmorpg", "-ddl-file", first, "-ddl-file", bad}
	if err := run(context.Background(), args, getenv, &bytes.Buffer{}); err == nil || reads != 0 {
		t.Fatalf("重复的 -ddl-file 里有不合格文件: 应在读取能力前拒绝,env_reads=%d err=%v", reads, err)
	}
	if err := run(context.Background(), []string{"-command", "prepare-schema", "-schema", "mmorpg", "-ddl-file", ""}, getenv, &bytes.Buffer{}); err == nil || reads != 0 {
		t.Fatalf("空的 -ddl-file 应当拒绝,env_reads=%d err=%v", reads, err)
	}
}

// 引号里的分号和 "--" 不拆、不当注释:COMMENT '…;…' 是合法的列注释(网关 schema.sql 的 create_role_closed 就这么写,
// 按分号硬拆时整份文件被拒)。引号之外的 "-- " 注释整行或行尾都去掉。
func TestSplitBusinessDDLKeepsQuotedSemicolonsAndDropsLineComments(t *testing.T) {
	text := strings.Join([]string{
		"-- 文件头注释; 里面的分号不算",
		"CREATE TABLE IF NOT EXISTS t (",
		"  a INT COMMENT '1 = 关;只经 /admin 改', -- 行尾注释; 也不算",
		"  b VARCHAR(8) DEFAULT 'it''s; fine' COMMENT \"x -- y; z\",",
		"  `c;d` INT COMMENT 'tail \\'quoted\\'; still inside'",
		");",
		"INSERT IGNORE INTO t (a) VALUES (1) ;  ",
		";",
	}, "\n")
	statements, err := splitBusinessDDL(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 2 {
		t.Fatalf("want 2 句, got %d: %q", len(statements), statements)
	}
	for _, kept := range []string{"'1 = 关;只经 /admin 改'", "'it''s; fine'", "\"x -- y; z\"", "`c;d`", "'tail \\'quoted\\'; still inside'"} {
		if !strings.Contains(statements[0], kept) {
			t.Errorf("引号里的内容被改动,缺 %s: %s", kept, statements[0])
		}
	}
	for _, dropped := range []string{"文件头注释", "行尾注释"} {
		if strings.Contains(statements[0], dropped) {
			t.Errorf("注释 %q 没有去掉: %s", dropped, statements[0])
		}
	}
	if statements[1] != "INSERT IGNORE INTO t (a) VALUES (1)" {
		t.Errorf("第二句 = %q", statements[1])
	}
	// "a--b" 是减负数,不是注释。
	if got, err := splitBusinessDDL("INSERT IGNORE INTO t (a) VALUES (3--1);"); err != nil || len(got) != 1 || !strings.Contains(got[0], "3--1") {
		t.Errorf("两个连字符后面不是空白时不算注释: %q err=%v", got, err)
	}
}

// 认不出的写法整份拒绝,不猜:引号没闭合;引号之外的 # 与 /* */(块注释里可以藏 /*! … */ 这类可执行内容)。
func TestSplitBusinessDDLRejectsWhatItCannotParse(t *testing.T) {
	for _, text := range []string{
		"CREATE TABLE IF NOT EXISTS t (a INT COMMENT 'never closed);",
		"CREATE TABLE IF NOT EXISTS `t (a INT);",
		"CREATE TABLE IF NOT EXISTS t (a INT) /* block */;",
		"CREATE TABLE IF NOT EXISTS t (a INT); /*! DROP TABLE t */",
		"# hash comment\nCREATE TABLE IF NOT EXISTS t (a INT);",
	} {
		if statements, err := splitBusinessDDL(text); err == nil {
			t.Errorf("应当拒绝: %q -> %q", text, statements)
		}
		if _, err := loadBusinessDDL(writeDDL(t, text)); err == nil {
			t.Errorf("整份文件应当拒绝: %q", text)
		}
	}
	// 引号里的 # 与 /* 是普通字符。
	statements, err := splitBusinessDDL("CREATE TABLE IF NOT EXISTS t (a INT COMMENT '# not a comment /* nor this */');")
	if err != nil || len(statements) != 1 {
		t.Errorf("引号里的 # 与 /* 不该被拒: %q err=%v", statements, err)
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
