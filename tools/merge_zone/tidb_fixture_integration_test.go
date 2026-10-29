//go:build merge_integration

package main

// 成功用例与生产使用同一TiDB路由/Team域；五张路由表直接读取DS权威DDL，避免测试维护第二份结构。
import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

func itCreateRouteTables(db *sql.DB) error {
	path := filepath.Join("..", "..", "go", "data_service", "internal", "store", "schema.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("读取DS权威路由DDL: %w", err)
	}
	names := []string{"playerRouteBootstrapDDL", "zoneMergeFenceBootstrapDDL", "zoneMergedIntoBootstrapDDL", "routeMetaBootstrapDDL", "routeReconcileBootstrapDDL"}
	values := make(map[string]string, len(names))
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
				for _, want := range names {
					if n.Name == want {
						if i >= len(vs.Values) {
							return fmt.Errorf("DS DDL %s不是显式字符串常量", want)
						}
						value, err := itDDLString(vs.Values[i])
						if err != nil {
							return fmt.Errorf("DS DDL %s: %w", want, err)
						}
						values[want] = value
					}
				}
			}
		}
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "USE `"+itRouteDB+"`"); err != nil {
		return err
	}
	for _, name := range names {
		ddl, ok := values[name]
		if !ok {
			return fmt.Errorf("DS权威DDL缺少%s", name)
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
	for _, table := range []string{"player_route", "zone_merge_fence", "zone_merged_into"} {
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

// 显式造SQL原件，同时写同rev缓存。绝不在itRun前从缓存反向补SQL，否则会掩盖待测缓存回档。
func itSetRouteHome(t *testing.T, id uint64, zone uint32) {
	t.Helper()
	db, err := sql.Open("mysql", itRouteDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("INSERT INTO player_route(route_shard,player_id,home_zone,rev,updated_by,created_at_ms,updated_at_ms) VALUES(?,?,?,1,'merge-zone-it',0,0) ON DUPLICATE KEY UPDATE home_zone=VALUES(home_zone),rev=rev+1", routeShardArg(id), id, zone)
	if err != nil {
		t.Fatal(err)
	}
	itPublishFixtureRoute(t, db, id)
}

func itSetRoutePlacement(t *testing.T, id uint64, raw string) {
	t.Helper()
	r, err := parsePlacement(raw)
	if err != nil {
		t.Fatalf("SQL原件不能表示畸形落点，请只污染Redis镜像: %v", err)
	}
	db, err := sql.Open("mysql", itRouteDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var frozen any
	if r.Frozen {
		frozen = r.RunID
	}
	_, err = db.Exec("INSERT INTO player_route(route_shard,player_id,storage_id,placement_version,frozen_run_id,rev,updated_by,created_at_ms,updated_at_ms) VALUES(?,?,?,?,?,1,'merge-zone-it',0,0) ON DUPLICATE KEY UPDATE storage_id=VALUES(storage_id),placement_version=VALUES(placement_version),frozen_run_id=VALUES(frozen_run_id),rev=rev+1", routeShardArg(id), id, r.StorageID, r.Version, frozen)
	if err != nil {
		t.Fatal(err)
	}
	itPublishFixtureRoute(t, db, id)
}

func itPublishFixtureRoute(t *testing.T, db *sql.DB, id uint64) {
	t.Helper()
	rdb := itRedis(t, itMappingRD)
	st := &mergeRouteStore{db: db}
	rows, err := st.ReadRoutes(context.Background(), []uint64{id})
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows[id]
	if !ok {
		t.Fatal("夹具SQL原件缺失")
	}
	pub := newRoutePublisher(rdb, itRouteDurability(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pub.publishRoutes(ctx, []mergeRouteRow{row}); err != nil {
		t.Fatal(err)
	}
}

func itDBMergeManifest(run string, src, dst uint32, operator string, now time.Time) *mergeManifest {
	m := newMergeManifest(run, src, dst, operator, now)
	m.RouteSource = routeSourceDB
	return m
}

func itDBRelocateManifest(run string, src, dst, generation uint32, tables []string, players []uint64, operator string, now time.Time) *relocateManifest {
	m := newRelocateManifest(run, src, dst, generation, tables, players, operator, now)
	m.RouteSource = routeSourceDB
	return m
}

// itFixtureRouteStore 从同一真实 TiDB 夹具装配各角色所需的具体存储，不在角色接口之间转换。
func itFixtureRouteStore(t *testing.T) *mergeRouteStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, _, err := openMergeRouteStore(ctx, itRouteDSN(), newMergeRouteWriter("merge-zone-it", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func itFixtureBackend(t *testing.T) *routeBackend {
	t.Helper()
	return &routeBackend{db: itFixtureRouteStore(t), pub: newRoutePublisher(itRedis(t, itMappingRD), itRouteDurability(t))}
}

func itFixturePlacements(t *testing.T) sqlRelocatePlacements {
	t.Helper()
	return sqlRelocatePlacements{store: itFixtureRouteStore(t), pub: newRoutePublisher(itRedis(t, itMappingRD), itRouteDurability(t))}
}

func itClearRoutePlacement(t *testing.T, id uint64) {
	t.Helper()
	db, err := sql.Open("mysql", itRouteDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE player_route SET storage_id=NULL,placement_version=NULL,frozen_run_id=NULL,rev=rev+1 WHERE route_shard=? AND player_id=?", routeShardArg(id), id); err != nil {
		t.Fatal(err)
	}
	itPublishFixtureRoute(t, db, id)
}

func itSeedSQLFence(t *testing.T, zone uint32, run string) {
	t.Helper()
	rb := itFixtureBackend(t)
	_, err := rb.db.AcquireFence(context.Background(), routeFenceAcquire{Zones: []uint32{zone}, RunID: run, Purpose: routeFencePurposeMerge, Src: itSrcZone, Dst: itDstZone, Holder: mergeInProgressValue{Operator: "merge-zone-it"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
}

func itSeedSQLMerged(t *testing.T, zone, dst uint32, run string) {
	t.Helper()
	db, err := sql.Open("mysql", itRouteDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT INTO zone_merged_into(src_zone,dst_zone,run_id,merged_at_ms) VALUES(?,?,?,0)", zone, dst, run); err != nil {
		t.Fatal(err)
	}
}

func itClearSQLMerged(t *testing.T, zone uint32) {
	t.Helper()
	db, err := sql.Open("mysql", itRouteDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM zone_merged_into WHERE src_zone=?", zone); err != nil {
		t.Fatal(err)
	}
	if err := itRedis(t, itMappingRD).Del(context.Background(), mergedIntoKey(zone)).Err(); err != nil {
		t.Fatal(err)
	}
}

// 恢复动作必须核原run，并释放SQL全部64片后才删匹配镜像。单删Redis不再是解除权威围栏。
func itReleaseSQLFences(t *testing.T, zones ...uint32) {
	t.Helper()
	rb := itFixtureBackend(t)
	for _, zone := range zones {
		state, err := rb.db.ReadFence(context.Background(), zone)
		if err != nil {
			t.Fatal(err)
		}
		if !state.Active {
			t.Fatalf("恢复前zone %d无原SQL活围栏", zone)
		}
		n, err := rb.db.ReleaseFence(context.Background(), []uint32{zone}, state.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if n[zone] != int64(routeFenceShards) {
			t.Fatalf("zone %d只释放%d片", zone, n[zone])
		}
		if _, err := rb.pub.deleteFenceMirror(context.Background(), zone, state.HolderJSON); err != nil {
			t.Fatal(err)
		}
	}
}

func itAssertRedisDeleteDoesNotReleaseSQLFence(t *testing.T, args ...string) string {
	t.Helper()
	rb := itFixtureBackend(t)
	db := itOpenRouteDB(t)
	run := ""
	states := make(map[uint32]routeFenceState, 2)
	for _, zone := range []uint32{itSrcZone, itDstZone} {
		state, err := rb.db.ReadFence(context.Background(), zone)
		if err != nil || !state.Active {
			t.Fatalf("缺原SQL围栏: %+v %v", state, err)
		}
		if run != "" && run != state.RunID {
			t.Fatal("两区SQL围栏原run不同")
		}
		run = state.RunID
		states[zone] = state
	}
	rdb := itRedis(t, itMappingRD)
	if err := rdb.Del(context.Background(), mergeFenceKey(itSrcZone), mergeFenceKey(itDstZone)).Err(); err != nil {
		t.Fatal(err)
	}
	out, code := itRun(t, args...)
	if code == 0 || !strings.Contains(out, "merge fence is held by another run") {
		t.Fatalf("只删镜像竟解除SQL围栏 exit=%d\n%s", code, out)
	}
	for _, zone := range []uint32{itSrcZone, itDstZone} {
		state := states[zone]
		itAssertSQLFence(t, db, zone, true, run)
		if err := rb.pub.setFenceMirror(context.Background(), zone, state.HolderJSON, state.Epoch, state.ExpiresAtMs); err != nil {
			t.Fatal(err)
		}
	}
	return run
}

func itAssertSQLFence(t *testing.T, db *sql.DB, zone uint32, active bool, run string) {
	t.Helper()
	var count int
	query := "SELECT COUNT(*) FROM zone_merge_fence WHERE zone_id=? AND shard<64 AND run_id IS NOT NULL AND expires_at_ms>" + routeDBNowMsSQL
	args := []any{zone}
	if active {
		query += " AND run_id=?"
		args = append(args, run)
	}
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	want := 0
	if active {
		want = int(routeFenceShards)
	}
	if count != want {
		t.Errorf("zone %d SQL活围栏片=%d，期望%d，原run=%s", zone, count, want, run)
	}
}

// 持有待改原行，等待真binary在其主键上排队后再注入迟到对象。TiDB不支持TRIGGER；这里不靠sleep猜时序。
func itRunWithLateRow(t *testing.T, schema, table, pk string, id uint64, inject func(), args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := itOpenReadCommitted(t)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var got uint64
	if err := tx.QueryRowContext(ctx, "SELECT `"+pk+"` FROM `"+schema+"`.`"+table+"` WHERE `"+pk+"`=? FOR UPDATE", id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	cmd := itCommand(t, args...)
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	collected := false
	defer func() {
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		if !collected {
			<-done
		}
	}()
	if err := itAwaitLockWaiters(ctx, db, schema, table, 1, 8*time.Second); err != nil {
		t.Fatalf("未观察到真实binary主键等待: %v", err)
	}
	inject()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	runErr := <-done
	collected = true
	return itCommandResult(t, output.Bytes(), runErr)
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
