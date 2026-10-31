//go:build merge_integration

package main

// 成功用例与生产使用同一TiDB路由/Team域；路由表直接读取DS权威DDL，避免测试维护第二份结构。
import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

const (
	itTiDBEnv         = "MERGE_ZONE_IT_TIDB_DSN"
	itRouteDB         = "merge_zone_it_route"
	itRouteGeneration = "902dbe90d01a4706be0c33fa3f8cb901"
)

func itSchemaDSN(schema string) string {
	cfg, err := mysql.ParseDSN(itMySQLBase)
	if err != nil {
		panic("已校验的TiDB测试基址损坏")
	}
	cfg.DBName, cfg.ParseTime, cfg.MultiStatements = schema, true, true
	cfg.Params = map[string]string{"charset": "utf8mb4", "tidb_txn_mode": "'pessimistic'"}
	return cfg.FormatDSN()
}

func itRouteDSN() string { return itSchemaDSN(itRouteDB) }

// 所有破坏性夹具操作之前，只接受显式本机TCP端点。MySQL协议不代表允许回退到MySQL引擎。
func itControlledTiDBBase(raw string) (string, error) {
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		return "", errors.New("TiDB DSN格式错误（不回显凭据）")
	}
	if cfg.DBName != "" || len(cfg.Params) != 0 || cfg.Net != "tcp" {
		return "", errors.New("测试TiDB DSN必须是无库名、无参数的本机TCP实例基址")
	}
	// mysql驱动把parseTime等内建选项从Params移走，仍须拒绝查询串，不能让拼库名时混入第二个问号。
	networkEnd := strings.Index(raw, "@tcp("+cfg.Addr+")/")
	if networkEnd < 0 || raw[networkEnd+len("@tcp("+cfg.Addr+")/"):] != "" {
		return "", errors.New("测试TiDB实例基址不能附带任何查询参数")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return "", errors.New("测试TiDB必须显式指定本机TCP端口")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("测试TiDB只允许数字loopback地址，拒绝触碰外部实例")
	}
	return cfg.FormatDSN(), nil
}

func itProbeTiDB(ctx context.Context, db *sql.DB) error {
	var version, comment string
	if err := db.QueryRowContext(ctx, "SELECT VERSION(),@@version_comment").Scan(&version, &comment); err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(version+" "+comment), "tidb") {
		return errors.New("全球Team成功用例需要真实TiDB，拒绝MySQL替代")
	}
	var check string
	if err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.tidb_enable_check_constraint").Scan(&check); err != nil {
		return err
	}
	if check != "1" && !strings.EqualFold(check, "ON") {
		return errors.New("TiDB CHECK未启用；夹具不修改GLOBAL变量，必须先完成受控环境准备")
	}
	return nil
}

// 旧外置路由库参数只能指向本套独占库，不能再清理任意预迁移库。
func itValidateRouteOverride() error {
	raw := os.Getenv(itRouteDBEnv)
	if raw == "" {
		return nil
	}
	cfg, err := mysql.ParseDSN(raw)
	base, baseErr := mysql.ParseDSN(itMySQLBase)
	if err != nil || baseErr != nil || cfg.Net != base.Net || cfg.Addr != base.Addr || cfg.User != base.User || cfg.Passwd != base.Passwd || cfg.DBName != itRouteDB || len(cfg.Params) != 0 {
		return errors.New("旧MERGE_ZONE_IT_ROUTE_DB_DSN仅允许同TiDB入口的merge_zone_it_route独占库，无额外参数")
	}
	return nil
}

// itRouteDDL 从 go/data_service/internal/store/schema.go 读一条建表 DDL(字符串常量)。
func itRouteDDL(name string) (string, error) {
	path := filepath.Join("..", "..", "go", "data_service", "internal", "store", "schema.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return "", fmt.Errorf("读取DS权威路由DDL: %w", err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != name {
					continue
				}
				if i >= len(vs.Values) {
					return "", fmt.Errorf("DS DDL %s不是显式字符串常量", name)
				}
				value, err := itDDLString(vs.Values[i])
				if err != nil {
					return "", fmt.Errorf("DS DDL %s: %w", name, err)
				}
				return value, nil
			}
		}
	}
	return "", fmt.Errorf("DS权威DDL缺少%s", name)
}

func itCreateRouteTables(db *sql.DB) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "USE `"+itRouteDB+"`"); err != nil {
		return err
	}
	for _, name := range []string{"playerRouteBootstrapDDL", "zoneMergedIntoBootstrapDDL", "routeMetaBootstrapDDL", "routeReconcileBootstrapDDL"} {
		ddl, err := itRouteDDL(name)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(context.Background(), ddl); err != nil {
			return fmt.Errorf("执行DS权威DDL %s: %w", name, err)
		}
	}
	_, err = conn.ExecContext(context.Background(), "INSERT INTO route_meta(id,generation,writable) VALUES(1,?,1)", itRouteGeneration)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(context.Background(), "INSERT INTO route_reconcile(id) VALUES(1)")
	return err
}

func itDDLString(expr ast.Expr) (string, error) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return strconv.Unquote(e.Value)
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			a, err := itDDLString(e.X)
			if err != nil {
				return "", err
			}
			b, err := itDDLString(e.Y)
			return a + b, err
		}
	}
	return "", errors.New("只接受字符串字面量及其拼接；DS DDL表达式变更需要显式审阅")
}

func itResetRouteTables(t *testing.T) {
	t.Helper()
	db, err := sql.Open("mysql", itRouteDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"player_route", "zone_merged_into"} {
		if _, err := db.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("UPDATE route_meta SET generation=?,writable=1 WHERE id=1", itRouteGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE route_reconcile SET leader=NULL,lease_until_ms=NULL,watermark_ms=0,cache_verified_ms=0 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"v":%d,"generation":%q,"watermark_ms":1}`, routeWatermarkFormatVersion, itRouteGeneration)
	if err := itRedis(t, itMappingRD).Set(context.Background(), routeWatermarkKey, raw, 0).Err(); err != nil {
		t.Fatal(err)
	}
}

// itSetRouteHome 造一行 player_route(路由原件)。
func itSetRouteHome(t *testing.T, id uint64, zone uint32) {
	t.Helper()
	db, err := sql.Open("mysql", itRouteDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("INSERT INTO player_route(route_shard,player_id,home_zone,rev,updated_by,created_at_ms,updated_at_ms) VALUES(?,?,?,1,'merge-zone-it',0,0) ON DUPLICATE KEY UPDATE home_zone=VALUES(home_zone),rev=rev+1", uint64(routeRowShard(id)), id, zone)
	if err != nil {
		t.Fatal(err)
	}
}

// itSeedRouteMerged 直接造一行 zone_merged_into(测试代码直接写表:AST 守卫不扫 _test.go)。
func itSeedRouteMerged(t *testing.T, src, dst uint32, run string) {
	t.Helper()
	db, err := sql.Open("mysql", itRouteDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT INTO zone_merged_into (src_zone, dst_zone, run_id, merged_at_ms) VALUES (?, ?, ?, 0)", src, dst, run); err != nil {
		t.Fatal(err)
	}
}

// TiDB原生计划必须含且仅含一个主键Point_Get，不能用伪造的MySQL key_len字段掩盖全表/二级索引扫描。
func itAssertTiDBPrimaryPoint(t *testing.T, rows []map[string]string, stmt string) {
	t.Helper()
	points := 0
	for _, row := range rows {
		id := row["id"]
		if strings.Contains(id, "Point_Get") {
			points++
			n, err := strconv.ParseFloat(row["estRows"], 64)
			if err != nil || n != 1 {
				t.Fatalf("主键点取不是精确一行: %v SQL=%s", rows, stmt)
			}
			access := strings.ToLower(row["access object"])
			if strings.Contains(access, "index:") && !strings.Contains(access, "primary") {
				t.Fatalf("点取使用非主键索引: %v SQL=%s", rows, stmt)
			}
		}
		if strings.Contains(id, "Scan") || strings.Contains(id, "IndexLookUp") {
			t.Fatalf("主键语句退化为扫描: %v SQL=%s", rows, stmt)
		}
	}
	if points != 1 {
		t.Fatalf("需要一个TiDB主键Point_Get，实际%v SQL=%s", rows, stmt)
	}
}
