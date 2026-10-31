package svc

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"friend/internal/config"
	"shared/authoritysql"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDSN_AuthorityModeAppliesToEveryConnection(t *testing.T) {
	c := config.MySQLConf{Host: "localhost:4000", User: "test", DBName: "test"}
	for _, backend := range []authoritysql.Backend{authoritysql.BackendMySQL, authoritysql.BackendTiDB} {
		parsed, err := drivermysql.ParseDSN(BuildDSN(c, backend))
		require.NoError(t, err)
		assert.Equal(t, "'READ-COMMITTED'", parsed.Params["transaction_isolation"])
		assert.Equal(t, "'STRICT_TRANS_TABLES'", parsed.Params["sql_mode"])
		if backend == authoritysql.BackendTiDB {
			assert.Equal(t, "'pessimistic'", parsed.Params["tidb_txn_mode"])
			assert.Equal(t, "0", parsed.Params["tidb_read_staleness"])
			assert.Equal(t, "OFF", parsed.Params["tidb_low_resolution_tso"])
			assert.Equal(t, "'leader'", parsed.Params["tidb_replica_read"])
			assert.Equal(t, "'strict'", parsed.Params["tidb_read_consistency"])
			assert.Equal(t, "''", parsed.Params["tidb_snapshot"])
		} else {
			for _, variable := range []string{"tidb_txn_mode", "tidb_read_staleness", "tidb_low_resolution_tso", "tidb_replica_read", "tidb_read_consistency", "tidb_snapshot"} {
				assert.NotContains(t, parsed.Params, variable, "mysql 档不能发送TiDB专有参数")
			}
		}
	}
}

func authorityTestConfig(t *testing.T, envName string) config.MySQLConf {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(envName))
	if dsn == "" {
		requireName := strings.Replace(envName, "_TEST_", "_REQUIRE_", 1)
		requireName = strings.TrimSuffix(requireName, "_DSN") + "_TESTS"
		if os.Getenv(requireName) != "" {
			t.Fatalf("%s要求真实数据库，但%s未设置", requireName, envName)
		}
		t.Skipf("%s未设置，未执行真实权威池校验", envName)
	}
	parsed, err := drivermysql.ParseDSN(dsn)
	require.NoError(t, err)
	return config.MySQLConf{Host: parsed.Addr, User: parsed.User, Password: parsed.Passwd, DBName: parsed.DBName, MaxOpenConn: 3, MaxIdleConn: 3}
}

func TestAuthorityDB_MySQLBackendAndTiDBGate(t *testing.T) {
	c := authorityTestConfig(t, "FRIEND_TEST_MYSQL_DSN")
	db, err := openMySQL(c, authoritysql.BackendMySQL, authoritysql.Reader())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var isolation string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT @@transaction_isolation").Scan(&isolation))
	assert.Equal(t, "READ-COMMITTED", strings.ToUpper(isolation))
	wrong, err := openMySQL(c, authoritysql.BackendTiDB, authoritysql.Reader())
	if wrong != nil {
		_ = wrong.Close()
	}
	require.Error(t, err, "tidb 档不能放行真实MySQL")
}

func TestAuthorityDB_TiDBPessimisticOnEveryConnection(t *testing.T) {
	c := authorityTestConfig(t, "FRIEND_TEST_TIDB_DSN")
	db, err := openMySQL(c, authoritysql.BackendTiDB, authoritysql.Reader())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var conns []*sql.Conn
	connectionIDs := make(map[uint64]bool)
	t.Cleanup(func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	for i := 0; i < 3; i++ {
		conn, err := db.Conn(ctx)
		require.NoError(t, err)
		conns = append(conns, conn)
		var connectionID uint64
		var mode, isolation, replica, consistency, snapshot string
		var staleness, lowResolution int64
		require.NoError(t, conn.QueryRowContext(ctx,
			"SELECT CONNECTION_ID(),@@tidb_txn_mode,@@transaction_isolation,@@tidb_read_staleness,@@tidb_low_resolution_tso,@@tidb_replica_read,@@tidb_read_consistency,@@tidb_snapshot").
			Scan(&connectionID, &mode, &isolation, &staleness, &lowResolution, &replica, &consistency, &snapshot))
		assert.False(t, connectionIDs[connectionID], "必须同时持有三个不同物理连接")
		connectionIDs[connectionID] = true
		assert.Equal(t, "pessimistic", mode)
		assert.Equal(t, "READ-COMMITTED", strings.ToUpper(isolation))
		assert.Zero(t, staleness, "权威列表不能读取历史快照")
		assert.Zero(t, lowResolution, "权威列表不能使用缓存TSO")
		assert.Equal(t, "leader", replica)
		assert.Equal(t, "strict", consistency, "权威列表不能跳过事务锁")
		assert.Empty(t, snapshot, "权威列表不能固定在历史时间点")
	}
	assert.Len(t, connectionIDs, 3)
}
