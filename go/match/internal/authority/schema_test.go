package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"schemamigrate"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAuthoritySchemaRequiresCleanPlan(t *testing.T) {
	readFailure := errors.New("读取schema失败")
	for _, tc := range []struct {
		name   string
		report schemamigrate.Report
		err    error
		wantOK bool
	}{
		{name: "完整schema", wantOK: true},
		{name: "只读告警不阻断", report: schemamigrate.Report{Warnings: []string{"多余列"}}, wantOK: true},
		{name: "缺表必须先显式迁移", report: schemamigrate.Report{Statements: []string{"CREATE TABLE 缺表"}}},
		{name: "人工项不得自动放行", report: schemamigrate.Report{Manual: []string{"列类型漂移"}}},
		{name: "dirty台账", err: schemamigrate.ErrDirty},
		{name: "核对未知", err: readFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := requireCurrentSchema(context.Background(), nil, "fixture", func(_ context.Context, _ *sql.DB, opts schemamigrate.Options) (schemamigrate.Report, error) {
				calls++
				require.Equal(t, "fixture", opts.Database)
				require.False(t, opts.AllowModifyColumn, "运行账号不能承继迁移改列授权")
				require.NotEmpty(t, opts.Tables)
				return tc.report, tc.err
			})
			require.Equal(t, 1, calls)
			if tc.wantOK {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				if tc.err != nil {
					require.ErrorIs(t, err, tc.err)
				}
			}
		})
	}
}

// 真实账号权限证明：Plan和候选索引检查不需要CREATE/ALTER；缺表也不能由运行账号补建。
func TestTiDBAuthorityStartupUsesDMLAccountAndRequiresCurrentSchema(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var database string
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database))
	user := "matchrt_" + uuid.NewString()[:8]
	password := uuid.NewString()
	_, err := s.DB.ExecContext(ctx, fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", user, password))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropErr := s.DB.Exec(fmt.Sprintf("DROP USER '%s'@'%%'", user))
		if dropErr != nil {
			t.Errorf("清理隔离运行账号失败: %v", dropErr)
		}
	})
	_, err = s.DB.ExecContext(ctx, fmt.Sprintf("GRANT SELECT,INSERT,UPDATE,DELETE ON `%s`.* TO '%s'@'%%'", database, user))
	require.NoError(t, err)
	c := mysql.NewConfig()
	c.Net, c.Addr, c.DBName, c.User, c.Passwd = "tcp", os.Getenv("MATCH_IT_TIDB"), database, user, password
	c.Timeout, c.ReadTimeout, c.WriteTimeout = 3*time.Second, 3*time.Second, 3*time.Second
	runtimeDB, err := sql.Open("mysql", c.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { runtimeDB.Close() })
	runtimeDB.SetMaxOpenConns(4)
	require.NoError(t, runtimeDB.PingContext(ctx))
	require.NoError(t, requireCurrentSchema(ctx, runtimeDB, database, schemamigrate.Plan))
	require.NoError(t, (&Store{DB: runtimeDB}).CheckCandidateIndexes(ctx))
	_, err = runtimeDB.ExecContext(ctx, "CREATE TABLE forbidden_runtime_ddl(id BIGINT PRIMARY KEY)")
	var permissionErr *mysql.MySQLError
	require.ErrorAs(t, err, &permissionErr)
	require.EqualValues(t, 1142, permissionErr.Number, "权限拒绝必须来自服务端，不能用网络失败冒充")
	_, err = runtimeDB.ExecContext(ctx, "ALTER TABLE match_global_policy ADD COLUMN forbidden_runtime_column BIGINT")
	require.ErrorAs(t, err, &permissionErr)
	require.EqualValues(t, 1142, permissionErr.Number)
	_, err = s.DB.ExecContext(ctx, "DROP TABLE match_global_policy")
	require.NoError(t, err)
	require.Error(t, requireCurrentSchema(ctx, runtimeDB, database, schemamigrate.Plan))
	var remaining int
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=? AND table_name='match_global_policy'", database).Scan(&remaining))
	require.Zero(t, remaining, "运行检查不得偷偷补DDL")
}

// 旧库迁移只补列、不补普通索引:观战意图清理用的索引缺了,启动核对必须拒绝接流,
// 不能带着每分钟一次的全表扫描上线(观战第二轮评审 R2-GO 一般-4)。
func TestTiDBCandidateIndexesRequireSpectatorIntentPurgeIndex(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, s.CheckCandidateIndexes(ctx))
	var index string
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT index_name FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='match_spectator_intent' AND seq_in_index=1 AND column_name='state' LIMIT 1").Scan(&index))
	_, err := s.DB.ExecContext(ctx, fmt.Sprintf("DROP INDEX `%s` ON match_spectator_intent", index))
	require.NoError(t, err)

	err = s.CheckCandidateIndexes(ctx)

	require.Error(t, err)
	require.Contains(t, err.Error(), "match_spectator_intent")
}
