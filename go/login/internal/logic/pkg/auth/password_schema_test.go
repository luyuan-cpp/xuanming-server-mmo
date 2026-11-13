package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// goodPasswordTableColumns 是 go/login/model/password_auth_schema.sql 建出来的表在 information_schema 里的样子。
func goodPasswordTableColumns() []passwordTableColumn {
	return []passwordTableColumn{
		{name: "account", dataType: "varchar", width: 191, nullable: false, collation: "utf8mb4_unicode_ci"},
		{name: "password", dataType: "varchar", width: 255, nullable: true, collation: "utf8mb4_unicode_ci"},
	}
}

func TestCheckPasswordTableShapeAcceptsTheSchemaFileShape(t *testing.T) {
	if err := checkPasswordTableShape(goodPasswordTableColumns(), []string{"account"}); err != nil {
		t.Fatalf("建表文件的形状被拒绝: %v", err)
	}
	// 多出来的列不管:本包只读写 account 与 password(指到带旧列的夹具表时不该拒绝启动)。
	withExtra := append(goodPasswordTableColumns(), passwordTableColumn{name: "simple_players", dataType: "mediumblob", nullable: true})
	if err := checkPasswordTableShape(withExtra, []string{"account"}); err != nil {
		t.Fatalf("多出无关列时被拒绝: %v", err)
	}
	// information_schema 的大小写随服务器配置变,列名与排序规则名都不区分大小写比较。
	upper := []passwordTableColumn{
		{name: "ACCOUNT", dataType: "varchar", width: 191, collation: "UTF8MB4_UNICODE_CI"},
		{name: "PASSWORD", dataType: "varchar", width: 255, nullable: true},
	}
	if err := checkPasswordTableShape(upper, []string{"ACCOUNT"}); err != nil {
		t.Fatalf("大小写不同的元数据被拒绝: %v", err)
	}
}

// 每一条拒绝对应运行期的一条前提(见 checkPasswordTableShape 的注释);原来由只能在 MySQL 上跑的存储过程检查。
func TestCheckPasswordTableShapeRejectsEveryBrokenPremise(t *testing.T) {
	mutate := func(change func(columns []passwordTableColumn)) []passwordTableColumn {
		columns := goodPasswordTableColumns()
		change(columns)
		return columns
	}
	cases := []struct {
		name       string
		columns    []passwordTableColumn
		primaryKey []string
		wantInErr  string
	}{
		{"表不存在", nil, nil, "table is missing"},
		{"没有主键:同一个账号可以有两行", goodPasswordTableColumns(), nil, "primary key"},
		{"主键不是 account", goodPasswordTableColumns(), []string{"password"}, "primary key"},
		{"联合主键:account 不再唯一", goodPasswordTableColumns(), []string{"account", "password"}, "primary key"},
		{"account 可空", mutate(func(c []passwordTableColumn) { c[0].nullable = true }), []string{"account"}, "column account"},
		{"account 过宽:超过账号形状门禁的 191", mutate(func(c []passwordTableColumn) { c[0].width = 255 }), []string{"account"}, "column account"},
		{"account 不是字符列", mutate(func(c []passwordTableColumn) { c[0].dataType = "varbinary" }), []string{"account"}, "column account"},
		{"account 的排序规则不同:TiDB 只写 CHARSET 时的缺省值", mutate(func(c []passwordTableColumn) { c[0].collation = "utf8mb4_bin" }), []string{"account"}, "collation"},
		{"password 过窄", mutate(func(c []passwordTableColumn) { c[1].width = 64 }), []string{"account"}, "column password"},
		{"password 是 TEXT:旧表的形状", mutate(func(c []passwordTableColumn) { c[1].dataType = "mediumtext"; c[1].width = 16777215 }), []string{"account"}, "column password"},
		{"缺 password 列", goodPasswordTableColumns()[:1], []string{"account"}, "column password is missing"},
		{"缺 account 列", goodPasswordTableColumns()[1:], []string{"account"}, "column account is missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPasswordTableShape(tc.columns, tc.primaryKey)
			if err == nil {
				t.Fatal("坏形状被接受")
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Fatalf("错误没有指出是哪一条前提不成立: %v", err)
			}
		})
	}
}

// 建表文件与形状守卫是同一份契约的两头:文件里写的列宽与排序规则必须正是守卫认的值,否则按文件建出来的表会让 login 拒绝启动。
// mmorpg 的建表 Job(writer_authority -ddl-file)只认 CREATE TABLE IF NOT EXISTS、表名不带库名,这里一并钉住。
func TestPasswordSchemaFileMatchesTheShapeGuard(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("取不到源文件路径")
	}
	// 本文件在 go/login/internal/logic/pkg/auth,建表文件在 go/login/model。
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "..", "..", "model", "password_auth_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	ddl := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS " + passwordTable + " (",
		"account VARCHAR(191) NOT NULL",
		"password VARCHAR(255) NULL",
		"PRIMARY KEY (account)",
		"COLLATE=" + passwordAccountCollation,
	} {
		if !strings.Contains(ddl, want) {
			t.Fatalf("password_auth_schema.sql 缺少 %q,与形状守卫对不上", want)
		}
	}
	if strings.Count(ddl, "CREATE TABLE") != 1 {
		t.Fatal("password_auth_schema.sql 只该有口令凭据表这一条建表语句")
	}
}
