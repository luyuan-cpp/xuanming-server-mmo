package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"match/internal/config"
	"schemamigrate"

	"github.com/stretchr/testify/require"
)

func TestMigrationFlagRequiresExplicitMigration(t *testing.T) {
	require.Error(t, validateMigrationFlags(false, true))
	require.NoError(t, validateMigrationFlags(true, true))
	require.NoError(t, validateMigrationFlags(true, false))
	require.NoError(t, validateMigrationFlags(false, false))
}

func TestMigrationUsesIsolatedCredentialsAndPreservesExitCodes(t *testing.T) {
	t.Setenv(migrationUserEnv, "fixture_migrator")
	t.Setenv(migrationPasswordEnv, "fixture-password-not-a-real-secret")
	t.Setenv("MATCH_PROBE_SECRET", "")
	c := config.Config{}
	c.Mode = "pro"
	c.GlobalMatch = config.GlobalMatchConf{Enabled: true, Backend: "tidb", User: "runtime", PasswordEnv: "RUNTIME_PASSWORD", Database: "mmorpg_match"}
	for _, tc := range []struct {
		name   string
		report schemamigrate.Report
		err    error
		code   int
	}{
		{name: "显式迁移成功", code: schemamigrate.ExitOK},
		{name: "人工项保留", report: schemamigrate.Report{Manual: []string{"类型漂移"}}, code: schemamigrate.ExitManual},
		{name: "锁忙原样返回", err: schemamigrate.ErrLockBusy, code: schemamigrate.ExitLockBusy},
		{name: "dirty拒绝", err: schemamigrate.ErrDirty, code: schemamigrate.ExitFailed},
		{name: "错误拒绝", err: errors.New("迁移失败"), code: schemamigrate.ExitFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			code := runMigration(c, true, func(_ context.Context, got config.GlobalMatchConf, allow bool) (schemamigrate.Report, error) {
				calls++
				require.Equal(t, "fixture_migrator", got.User)
				require.Equal(t, migrationPasswordEnv, got.PasswordEnv)
				require.True(t, allow)
				return tc.report, tc.err
			})
			require.Equal(t, tc.code, code)
			require.Equal(t, 1, calls)
			require.Equal(t, "runtime", c.GlobalMatch.User, "迁移不能污染常驻配置")
		})
	}
}

func TestMigrationRejectsMissingCredentialsAndDisabledAuthority(t *testing.T) {
	c := config.Config{}
	c.Mode = "pro"
	c.GlobalMatch = config.GlobalMatchConf{Enabled: true, Backend: "tidb"}
	never := func(context.Context, config.GlobalMatchConf, bool) (schemamigrate.Report, error) {
		t.Fatal("非法迁移配置不能触碰数据库")
		return schemamigrate.Report{}, nil
	}
	t.Setenv(migrationUserEnv, "")
	t.Setenv(migrationPasswordEnv, "fixture")
	require.Equal(t, schemamigrate.ExitFailed, runMigration(c, false, never))
	t.Setenv(migrationUserEnv, "fixture")
	t.Setenv(migrationPasswordEnv, "")
	require.Equal(t, schemamigrate.ExitFailed, runMigration(c, false, never))
	t.Setenv(migrationPasswordEnv, "fixture")
	c.GlobalMatch.Enabled = false
	require.Equal(t, schemamigrate.ExitFailed, runMigration(c, false, never))
	c.GlobalMatch.Enabled, c.GlobalMatch.Backend = true, "oracle"
	require.Equal(t, schemamigrate.ExitFailed, runMigration(c, false, never))
}

// 检查真实main接线，防止-migrate在建Svc之后才被解释而申请号源/启动消费者。
func TestMigrationBranchPrecedesServiceConstruction(t *testing.T) {
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "match_service.go", nil, 0)
	require.NoError(t, err)
	var migrationBranch, serviceConstruction token.Pos
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if statement, ok := node.(*ast.IfStmt); ok {
				if pointer, ok := statement.Cond.(*ast.StarExpr); ok {
					if identifier, ok := pointer.X.(*ast.Ident); ok && identifier.Name == "migrateOnly" {
						migrationBranch = statement.Pos()
					}
				}
			}
			if call, ok := node.(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "NewServiceContextWithAdmission" {
					serviceConstruction = call.Pos()
				}
			}
			return true
		})
	}
	require.NotZero(t, migrationBranch)
	require.NotZero(t, serviceConstruction)
	require.Less(t, migrationBranch, serviceConstruction)
}
