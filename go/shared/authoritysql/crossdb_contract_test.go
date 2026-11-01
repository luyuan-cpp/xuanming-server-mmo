package authoritysql

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 逻辑库可以搬到别的物理集群,所以运行代码不能有跨库 SQL(docs/design/horizontal-scale-logical-db.md §1):
// 一条 SQL 只碰连接所在的库,别的库的数据走对方服务的 RPC。这里扫 go/ 下的非测试源码,库名拼进表名就失败。

// crossDatabaseShapes 是库名拼进表名的写法。形状靠字面判断,名字起得太巧会漏,能挡住的是常见写法。
var crossDatabaseShapes = []*regexp.Regexp{
	// "`" + schema + "`.`table`"、"FROM `" + schema + "`.`table`"
	regexp.MustCompile("`\"\\s*\\+\\s*[\\w.()]+\\s*\\+\\s*\"`\\."),
	// fmt.Sprintf("`%s`.`%s`", ...)
	regexp.MustCompile("`%[sv]`\\."),
	// fmt.Sprintf("SELECT 1 FROM %s.account_role", schema)
	regexp.MustCompile("(?i)\\b(FROM|JOIN|UPDATE|INTO)\\s+`?%[sv]`?\\."),
	// schema + ".account_role"、cfg.DBName + ".account_role"
	regexp.MustCompile("(?i)\\w*(schema|dbname|db_name|database)\\w*(\\(\\))?\\s*\\+\\s*\"`?\\."),
	// SELECT 1 FROM mmorpg_global.account_role
	regexp.MustCompile("(?i)\\b(FROM|JOIN|UPDATE|INTO)\\s+`?(mmorpg\\w*|zone_\\d+_db|player_store_\\d+_db)`?\\."),
}

// crossDatabaseAllowedDirs 是迁移工具:它们拿迁移账号按库名操作 information_schema 和指定的库,不是运行路径。
// tools/dbmove 不在 go/ 下,不在扫描范围。相对 go/,用 / 分隔。
var crossDatabaseAllowedDirs = []string{"schemamigrate/", "db/internal/migrate/", "db/cmd/migrate/"}

// allowedCrossDatabaseLine 是白名单里唯一的运行代码。qualified 给闸门表加的是本库的库名:
// 库名是写资格的 AuthoritySchema,连接建立时已核 DATABASE() 等于它(pool.go),所以不是跨库。
func allowedCrossDatabaseLine(rel, line string) bool {
	return rel == "shared/authoritysql/writer.go" && strings.Contains(line, "func qualified(")
}

func skippedContractDir(name string) bool {
	switch name {
	case "proto", "generated", "vendor", "testdata":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func matchesCrossDatabaseShape(line string) bool {
	for _, shape := range crossDatabaseShapes {
		if shape.MatchString(line) {
			return true
		}
	}
	return false
}

func scanCrossDatabaseFile(rel string, content []byte) []string {
	for _, dir := range crossDatabaseAllowedDirs {
		if strings.HasPrefix(rel, dir) {
			return nil
		}
	}
	var found []string
	for i, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") || allowedCrossDatabaseLine(rel, line) {
			continue
		}
		if matchesCrossDatabaseShape(line) {
			found = append(found, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}
	return found
}

func TestNoCrossDatabaseSQL(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "shared", "authoritysql", "writer.go")); err != nil {
		t.Fatalf("go/ root not found at %s: %v", root, err)
	}
	var found []string
	scanned := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && skippedContractDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, ".pb.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		found = append(found, scanCrossDatabaseFile(filepath.ToSlash(rel), content)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files under %s; the walk is not covering go/", scanned, root)
	}
	if len(found) != 0 {
		t.Fatalf("cross-database SQL found; go through the owning service's RPC instead:\n%s", strings.Join(found, "\n"))
	}
}

// 形状本身要拦得住,否则上面那个用例永远是绿的。
func TestCrossDatabaseShapes(t *testing.T) {
	violations := []string{
		"	return \"`\" + schema + \"`.`\" + table + \"`\"",
		"	query := \"SELECT 1 FROM `\" + cfg.AccountRoleSchema + \"`.`account_role`\"",
		"	q := fmt.Sprintf(\"SELECT 1 FROM `%s`.`account_role`\", s)",
		"	q := fmt.Sprintf(\"UPDATE %s.player_route SET x=1\", s)",
		"	q := fmt.Sprintf(\"INSERT INTO %v.t VALUES (1)\", s)",
		"	q := \"SELECT 1 FROM \" + routeSchema + \".player_route\"",
		"	q := \"SELECT 1 FROM \" + cfg.DBName + \".player_route\"",
		"	q := \"SELECT 1 FROM mmorpg_global.account_role\"",
		"	q := \"LEFT JOIN `mmorpg_match`.team_launch t ON 1=1\"",
		"	q := \"UPDATE zone_1_db.player_store SET x=1\"",
		"	q := \"INSERT INTO player_store_1000000_db.player_store VALUES (1)\"",
	}
	for _, line := range violations {
		if !matchesCrossDatabaseShape(line) {
			t.Errorf("not caught: %s", line)
		}
	}
	allowed := []string{
		"	const route = \"`\" + PlayerRouteTableName + \"`\"",
		"	quoted[i] = \"`\" + c + \"`\"",
		"	q := \"SELECT 1 FROM account_role WHERE player_id=?\"",
		"	return fmt.Errorf(\"look up column %s.%s: %w\", tableName, column, err)",
		"	row := db.QueryRowContext(ctx, \"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=?\", schema)",
	}
	for _, line := range allowed {
		if matchesCrossDatabaseShape(line) {
			t.Errorf("false positive: %s", line)
		}
	}

	if got := scanCrossDatabaseFile("guild/internal/data/x.go", []byte("package x\n// SELECT 1 FROM mmorpg_match.t\nconst q = \"SELECT 1 FROM mmorpg_match.t\"\n")); len(got) != 1 || !strings.HasPrefix(got[0], "guild/internal/data/x.go:3:") {
		t.Errorf("a comment is skipped and the statement is reported with its line: %v", got)
	}
	if got := scanCrossDatabaseFile("db/internal/migrate/x.go", []byte("const q = \"SELECT 1 FROM mmorpg_match.t\"\n")); len(got) != 0 {
		t.Errorf("migration tools are allowed: %v", got)
	}
	writer := "func qualified(schema, table string) string { return \"`\" + schema + \"`.`\" + table + \"`\" }\nconst q = \"`\" + other + \"`.t\"\n"
	if got := scanCrossDatabaseFile("shared/authoritysql/writer.go", []byte(writer)); len(got) != 1 || !strings.HasPrefix(got[0], "shared/authoritysql/writer.go:2:") {
		t.Errorf("only the qualified helper is allowed in writer.go: %v", got)
	}
}
