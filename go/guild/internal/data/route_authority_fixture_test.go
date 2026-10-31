package data

// 只用于随机隔离库：AST读取DS唯一正式nullable DDL，禁止测试自行维护第二套route表语义。
import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"shared/placement"
)

func createGuildRouteFixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)
	path := filepath.Join(filepath.Dir(source), "..", "..", "..", "data_service", "internal", "store", "schema.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	names := []string{"playerRouteBootstrapDDL", "zoneMergedIntoBootstrapDDL", "routeMetaBootstrapDDL", "routeReconcileBootstrapDDL"}
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	values := map[string]string{}
	var literal func(ast.Expr) string
	literal = func(expr ast.Expr) string {
		switch v := expr.(type) {
		case *ast.BasicLit:
			require.Equal(t, token.STRING, v.Kind)
			value, e := strconv.Unquote(v.Value)
			require.NoError(t, e)
			return value
		case *ast.BinaryExpr:
			require.Equal(t, token.ADD, v.Op)
			return literal(v.X) + literal(v.Y)
		default:
			t.Fatalf("DS正式DDL表达式变化，须显式审阅: %T", expr)
			return ""
		}
	}
	for _, decl := range file.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if wanted[name.Name] {
					require.Less(t, i, len(value.Values))
					values[name.Name] = literal(value.Values[i])
				}
			}
		}
	}
	for _, name := range names {
		ddl, ok := values[name]
		require.True(t, ok, "DS正式DDL缺少%s", name)
		_, err = db.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO route_meta(id,generation,writable) VALUES(1,'6775696c64747269616c',1)")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO route_reconcile(id) VALUES(1)")
	require.NoError(t, err)
	for _, player := range []uint64{11, 22} {
		_, err = db.ExecContext(ctx, "INSERT INTO player_route(route_shard,player_id,home_zone,rev,updated_by,created_at_ms,updated_at_ms) VALUES(?,?,1,1,'guild-trial-it',1,1)", placement.RouteRowShard(player), player)
		require.NoError(t, err)
	}
}
