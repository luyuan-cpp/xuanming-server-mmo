//go:build merge_integration

package main

// 真库用例:合并、幂等、拒绝成环、拒绝合进已合走的区、撤销、并发。
//
// 运行:go test -tags merge_integration -count=1 -v ./...
//   - MERGE_ZONE_IT_DSN:本机实例基址,数字 loopback TCP、无库名无参数,例如 root:pw@tcp(127.0.0.1:4000)/。
//   - MMORPG_SQL_BACKEND:tidb(缺省)或 mysql,和实例对上,两档都要跑。
//   - MERGE_ZONE_IT_REQUIRE=1:依赖缺失时 FAIL;默认 SKIP。验收必须设 1,SKIP 不能算通过。
//
// 每个用例建一个 merge_zone_it_<随机> 库,结束时删掉,不碰别的库。表结构用权威 DDL:zone_merged_into 从
// go/data_service/internal/store/schema.go 解析,route_meta 取 authoritysql.CatalogBootstrapStatements。

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"shared/authoritysql"
)

const (
	itDSNEnv        = "MERGE_ZONE_IT_DSN"
	itTimeout       = 30 * time.Second
	itSchemaPrefix  = "merge_zone_it_"
	itGeneration    = "902dbe90d01a4706be0c33fa3f8cb901"
	itZoneDDLSymbol = "zoneMergedIntoBootstrapDDL"
)

// requireIT:依赖没有配置时按 MERGE_ZONE_IT_REQUIRE 决定 FAIL 还是 SKIP。
func requireIT(t *testing.T) {
	t.Helper()
	if os.Getenv(itDSNEnv) != "" {
		return
	}
	message := itDSNEnv + " is not set"
	if os.Getenv(itRequireEnvName) == "1" {
		t.Fatal(message)
	}
	t.Skip(message)
}

// itLoopbackBase 校验测试基址:只接受数字 loopback,用例会建库删库,不能碰外部实例。错误不回显 DSN。
func itLoopbackBase(t *testing.T) *mysql.Config {
	t.Helper()
	cfg, err := mysql.ParseDSN(os.Getenv(itDSNEnv))
	if err != nil {
		t.Fatalf("%s does not parse (value not echoed)", itDSNEnv)
	}
	if cfg.Net != "tcp" || cfg.DBName != "" || len(cfg.Params) != 0 {
		t.Fatalf("%s must be a plain TCP base DSN without database or parameters", itDSNEnv)
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		t.Fatalf("%s needs an explicit port", itDSNEnv)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("%s must be a numeric loopback address", itDSNEnv)
	}
	return cfg
}

// itZoneDDL 从 data_service 的 schema.go 读 zone_merged_into 的建表语句。
func itZoneDDL(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "go", "data_service", "internal", "store", "schema.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || value.Names[0].Name != itZoneDDLSymbol || len(value.Values) != 1 {
				continue
			}
			ddl, err := itStringConst(value.Values[0])
			if err != nil {
				t.Fatal(err)
			}
			return ddl
		}
	}
	t.Fatalf("%s not found in %s", itZoneDDLSymbol, path)
	return ""
}

func itStringConst(expr ast.Expr) (string, error) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return strconv.Unquote(e.Value)
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			left, err := itStringConst(e.X)
			if err != nil {
				return "", err
			}
			right, err := itStringConst(e.Y)
			return left + right, err
		}
	}
	return "", errors.New("the DDL is not a concatenation of string literals")
}

// itEnv 是一个一次性库:raw 造数据、核对结果;newStore 按操作员权限开工具自己的连接。
type itEnv struct {
	ctx      context.Context
	raw      *sql.DB
	dsn      string
	backend  authoritysql.Backend
	operator authoritysql.Permission
}

func newITEnv(t *testing.T) *itEnv {
	t.Helper()
	base := itLoopbackBase(t)
	backend, err := authoritysql.LoadBackend(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout)
	t.Cleanup(cancel)

	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	schema := itSchemaPrefix + hex.EncodeToString(nonce[:])

	admin, err := sql.Open("mysql", base.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		if os.Getenv(itRequireEnvName) == "1" {
			t.Fatalf("the instance of %s is unreachable: %v", itDSNEnv, err)
		}
		t.Skipf("the instance of %s is unreachable: %v", itDSNEnv, err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+schema+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), itTimeout)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE `"+schema+"`"); err != nil {
			t.Errorf("drop %s: %v", schema, err)
		}
	})

	cfg := base.Clone()
	cfg.DBName = schema
	dsn := cfg.FormatDSN()
	raw, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	for _, ddl := range []string{itZoneDDL(t), authoritysql.CatalogBootstrapStatements()["route_meta"]} {
		if _, err := raw.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.ExecContext(ctx, "INSERT INTO route_meta(id,generation,writable) VALUES(1,?,1)", itGeneration); err != nil {
		t.Fatal(err)
	}
	var principal string
	if err := raw.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&principal); err != nil {
		t.Fatal(err)
	}
	return &itEnv{ctx: ctx, raw: raw, dsn: dsn, backend: backend, operator: authoritysql.Operator(principal)}
}

func (e *itEnv) newStore(t *testing.T) *routeStore {
	t.Helper()
	store, err := openRouteStore(e.ctx, e.dsn, e.backend, e.operator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func (e *itEnv) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := e.raw.ExecContext(e.ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (e *itEnv) merges(t *testing.T) map[uint32]uint32 {
	t.Helper()
	rows, err := listMappingRows(e.ctx, e.raw)
	if err != nil {
		t.Fatal(err)
	}
	return mergesOf(rows)
}

func (e *itEnv) run(store mappingStore, mode string, src, dst uint32, dryRun bool) (string, error) {
	var out bytes.Buffer
	err := execute(e.ctx, store, options{mode: mode, sourceZone: src, targetZone: dst, dryRun: dryRun}, time.Now(), &out)
	return out.String(), err
}

func TestIT_MergeWritesOneRowWithAuditColumns(t *testing.T) {
	requireIT(t)
	env := newITEnv(t)
	store := env.newStore(t)
	before := time.Now().Add(-time.Minute).UnixMilli()
	if _, err := env.run(store, modeMerge, 901, 902, false); err != nil {
		t.Fatal(err)
	}
	var dst uint32
	var runID string
	var mergedAt int64
	if err := env.raw.QueryRowContext(env.ctx, "SELECT dst_zone, run_id, merged_at_ms FROM zone_merged_into WHERE src_zone = 901").Scan(&dst, &runID, &mergedAt); err != nil {
		t.Fatal(err)
	}
	if dst != 902 || !strings.HasPrefix(runID, "901-902-") || mergedAt < before {
		t.Fatalf("row = dst %d run_id %q merged_at_ms %d", dst, runID, mergedAt)
	}
}

func TestIT_MergeIsIdempotent(t *testing.T) {
	requireIT(t)
	env := newITEnv(t)
	store := env.newStore(t)
	if _, err := env.run(store, modeMerge, 901, 902, false); err != nil {
		t.Fatal(err)
	}
	var firstRun string
	if err := env.raw.QueryRowContext(env.ctx, "SELECT run_id FROM zone_merged_into WHERE src_zone = 901").Scan(&firstRun); err != nil {
		t.Fatal(err)
	}
	// 第二次用晚一小时的时钟:run_id 带时间戳,被改写的话一眼看得出来。
	var out bytes.Buffer
	o := options{mode: modeMerge, sourceZone: 901, targetZone: 902}
	if err := execute(env.ctx, store, o, time.Now().Add(time.Hour), &out); err != nil || !strings.Contains(out.String(), "nothing written") {
		t.Fatalf("re-run: out=%q err=%v", out.String(), err)
	}
	var secondRun string
	if err := env.raw.QueryRowContext(env.ctx, "SELECT run_id FROM zone_merged_into WHERE src_zone = 901").Scan(&secondRun); err != nil {
		t.Fatal(err)
	}
	if secondRun != firstRun {
		t.Fatalf("an idempotent re-run rewrote run_id: %q -> %q", firstRun, secondRun)
	}
}

func TestIT_MergeRefusals(t *testing.T) {
	requireIT(t)
	env := newITEnv(t)
	store := env.newStore(t)
	if _, err := env.run(store, modeMerge, 901, 902, false); err != nil {
		t.Fatal(err)
	}
	// 反向合会成环。
	if _, err := env.run(store, modeMerge, 902, 901, false); !errors.Is(err, errRefused) {
		t.Fatalf("cycle: %v", err)
	}
	// 合进已合走的区。
	if _, err := env.run(store, modeMerge, 903, 901, false); !errors.Is(err, errRefused) {
		t.Fatalf("merged-away target: %v", err)
	}
	// 源区已合进别的区。
	if _, err := env.run(store, modeMerge, 901, 904, false); !errors.Is(err, errRefused) {
		t.Fatalf("another target: %v", err)
	}
	if merges := env.merges(t); len(merges) != 1 || merges[901] != 902 {
		t.Fatalf("a refusal wrote something: %v", merges)
	}
	// 连续合服:链尾可以再往下合。
	if _, err := env.run(store, modeMerge, 902, 903, false); err != nil {
		t.Fatalf("chained merge: %v", err)
	}
	if merges := env.merges(t); len(merges) != 2 || merges[902] != 903 {
		t.Fatalf("merges = %v", merges)
	}
	// 表里有环时不再往上加。
	env.exec(t, "INSERT INTO zone_merged_into (src_zone, dst_zone, run_id, merged_at_ms) VALUES (801, 802, 'seed', 0), (802, 801, 'seed', 0)")
	if _, err := env.run(store, modeMerge, 905, 906, false); !errors.Is(err, errRefused) {
		t.Fatalf("broken table: %v", err)
	}
}

func TestIT_FrozenCatalogRefusesWrites(t *testing.T) {
	requireIT(t)
	env := newITEnv(t)
	store := env.newStore(t)
	if _, err := env.run(store, modeMerge, 901, 902, false); err != nil {
		t.Fatal(err)
	}
	env.exec(t, "UPDATE route_meta SET writable = 0 WHERE id = 1")
	if _, err := env.run(store, modeMerge, 903, 904, false); !errors.Is(err, errRefused) {
		t.Fatalf("merge: %v", err)
	}
	if _, err := env.run(store, modeUnmerge, 901, 902, false); !errors.Is(err, errRefused) {
		t.Fatalf("unmerge: %v", err)
	}
	if merges := env.merges(t); len(merges) != 1 || merges[901] != 902 {
		t.Fatalf("merges = %v", merges)
	}
}

func TestIT_Unmerge(t *testing.T) {
	requireIT(t)
	env := newITEnv(t)
	store := env.newStore(t)
	for _, pair := range [][2]uint32{{901, 902}, {902, 903}} {
		if _, err := env.run(store, modeMerge, pair[0], pair[1], false); err != nil {
			t.Fatal(err)
		}
	}
	// 不要求逆序:先删靠前的一跳。
	if _, err := env.run(store, modeUnmerge, 901, 902, false); err != nil {
		t.Fatal(err)
	}
	if merges := env.merges(t); len(merges) != 1 || merges[902] != 903 {
		t.Fatalf("merges = %v", merges)
	}
	// 不存在是幂等成功。
	out, err := env.run(store, modeUnmerge, 901, 902, false)
	if err != nil || !strings.Contains(out, "not merged") {
		t.Fatalf("re-run: out=%q err=%v", out, err)
	}
	// 映射到别的区不删。
	out, err = env.run(store, modeUnmerge, 902, 999, false)
	if err != nil || !strings.Contains(out, "maps to zone 903") {
		t.Fatalf("mismatch: out=%q err=%v", out, err)
	}
	if merges := env.merges(t); merges[902] != 903 {
		t.Fatalf("merges = %v", merges)
	}
	// 撤销之后可以合进别的区。
	if _, err := env.run(store, modeUnmerge, 902, 903, false); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(store, modeMerge, 902, 904, false); err != nil {
		t.Fatalf("merge after unmerge: %v", err)
	}
}

func TestIT_DryRunAndAuditWithReaderPermission(t *testing.T) {
	requireIT(t)
	env := newITEnv(t)
	writer := env.newStore(t)
	if _, err := env.run(writer, modeMerge, 901, 902, false); err != nil {
		t.Fatal(err)
	}
	reader, err := openRouteStore(env.ctx, env.dsn, env.backend, authoritysql.Reader())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if out, err := env.run(reader, modeMerge, 902, 903, true); err != nil || !strings.Contains(out, "would record") {
		t.Fatalf("dry-run merge: out=%q err=%v", out, err)
	}
	if out, err := env.run(reader, modeUnmerge, 901, 902, true); err != nil || !strings.Contains(out, "would delete") {
		t.Fatalf("dry-run unmerge: out=%q err=%v", out, err)
	}
	if merges := env.merges(t); len(merges) != 1 {
		t.Fatalf("dry-run wrote: %v", merges)
	}
	out, err := env.run(reader, modeAudit, 0, 0, false)
	if err != nil || !strings.Contains(out, "merged zones: 1") || !strings.Contains(out, "final zone 902 includes zones [901 902]") {
		t.Fatalf("audit: out=%q err=%v", out, err)
	}
}

// 并发把同一个源区合进两个目标区:route_meta 的行锁让它们排队,只有一个写成,另一个看到结果后拒绝,
// 或等满 1 秒锁等待报失败(重跑时再拒绝)。不能出现两行,也不能两个都成功。
func TestIT_ConcurrentMergesSerializeOnRouteMeta(t *testing.T) {
	requireIT(t)
	env := newITEnv(t)
	const workers = 8
	stores := make([]*routeStore, workers)
	for i := range stores {
		stores[i] = env.newStore(t)
	}
	var wg sync.WaitGroup
	results := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = env.run(stores[i], modeMerge, 901, uint32(902+i), false)
		}()
	}
	wg.Wait()

	succeeded := 0
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, errRefused):
		default:
			t.Logf("worker %d failed before deciding (allowed: lock wait): %v", i, err)
		}
	}
	merges := env.merges(t)
	if succeeded != 1 || len(merges) != 1 {
		t.Fatalf("succeeded=%d merges=%v results=%v", succeeded, merges, fmt.Sprint(results))
	}
	// 写成的那一行的目标区,就是唯一成功的那个 worker 的目标区。
	for i, err := range results {
		if err == nil && merges[901] != uint32(902+i) {
			t.Fatalf("worker %d succeeded but the row says %d", i, merges[901])
		}
	}
}
