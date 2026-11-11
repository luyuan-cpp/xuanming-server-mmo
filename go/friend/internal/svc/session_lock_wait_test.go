package svc

// 会话锁等待上限(session_lock_wait.go)的回归测试:DSN 形状与启动读回探针不连库;
// 真库用例 TestAuthorityDB_SessionLockWaitsOnEveryConnection 由 FRIEND_TEST_MYSQL_DSN / FRIEND_TEST_TIDB_DSN 门控。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"friend/internal/config"
	"shared/authoritysql"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDSN_SessionLockWaits(t *testing.T) {
	c := config.MySQLConf{Host: "localhost:3306", User: "u", Password: "p", DBName: "mmorpg_friend"}
	mysqlCfg, err := drivermysql.ParseDSN(BuildDSN(c, authoritysql.BackendMySQL))
	require.NoError(t, err)
	tidbCfg, err := drivermysql.ParseDSN(BuildDSN(c, authoritysql.BackendTiDB))
	require.NoError(t, err)

	assert.Equal(t, strconv.Itoa(mysqlMetadataLockWaitSeconds), mysqlCfg.Params["lock_wait_timeout"],
		"mysql 档必须带元数据锁等待上限:缺省一年,DDL 排队时同表 DML 会在客户端放弃之后仍然执行")
	assert.NotContains(t, tidbCfg.Params, "lock_wait_timeout", "TiDB 档不发 lock_wait_timeout")
	for name, cfg := range map[string]*drivermysql.Config{"mysql": mysqlCfg, "tidb": tidbCfg} {
		assert.NotContains(t, cfg.Params, "innodb_lock_wait_timeout",
			"%s 档:行锁等待由 authoritysql 逐连接 SET,DSN 里再写一份会在建连后被覆盖,只是一个不生效的第二来源", name)
	}
	// 追加锁等待参数不改动原有的会话参数。
	assert.Equal(t, "'READ-COMMITTED'", mysqlCfg.Params["transaction_isolation"])
	assert.Equal(t, "'STRICT_TRANS_TABLES'", mysqlCfg.Params["sql_mode"])
	assert.Equal(t, "'pessimistic'", tidbCfg.Params["tidb_txn_mode"])
}

func TestVerifySessionLockWaits(t *testing.T) {
	broken := errors.New("fixture: connection lost")
	cases := []struct {
		name     string
		backend  authoritysql.Backend
		values   []driver.Value
		queryErr error
		wantSQL  string
		wantErr  string // 空串 = 期望通过
	}{
		{"mysql 两项都是期望值", authoritysql.BackendMySQL, []driver.Value{int64(1), int64(1)}, nil, probeLockWaitMySQLSQL, ""},
		{"mysql 行锁等待回到缺省 50 s", authoritysql.BackendMySQL, []driver.Value{int64(50), int64(1)}, nil, probeLockWaitMySQLSQL, "innodb_lock_wait_timeout=50"},
		{"mysql 元数据锁等待没生效(缺省一年)", authoritysql.BackendMySQL, []driver.Value{int64(1), int64(31536000)}, nil, probeLockWaitMySQLSQL, "session lock_wait_timeout=31536000"},
		{"tidb 只核行锁等待", authoritysql.BackendTiDB, []driver.Value{int64(1)}, nil, probeLockWaitTiDBSQL, ""},
		{"tidb 行锁等待不对", authoritysql.BackendTiDB, []driver.Value{int64(50)}, nil, probeLockWaitTiDBSQL, "innodb_lock_wait_timeout=50"},
		{"读不到会话变量", authoritysql.BackendMySQL, nil, broken, probeLockWaitMySQLSQL, "read session lock wait timeouts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &lockWaitProbe{values: tc.values, err: tc.queryErr}
			db := sql.OpenDB(lockWaitProbeConnector{probe: probe})
			t.Cleanup(func() { _ = db.Close() })

			err := verifySessionLockWaits(context.Background(), db, tc.backend)

			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
			if tc.queryErr != nil {
				assert.ErrorIs(t, err, tc.queryErr)
			}
			assert.Equal(t, []string{tc.wantSQL}, probe.recorded(), "每个档位恰好读一次自己的那组会话变量")
		})
	}
}

// 真库:每条物理连接上锁等待都是期望值(DSN 参数与 authoritysql 的逐连接初始化都生效,不只是探针碰到的那一条)。
func TestAuthorityDB_SessionLockWaitsOnEveryConnection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		backend authoritysql.Backend
	}{
		{"mysql", "FRIEND_TEST_MYSQL_DSN", authoritysql.BackendMySQL},
		{"tidb", "FRIEND_TEST_TIDB_DSN", authoritysql.BackendTiDB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := authorityTestConfig(t, tc.env)
			db, err := openMySQL(c, tc.backend, authoritysql.Reader())
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var conns []*sql.Conn
			t.Cleanup(func() {
				for _, conn := range conns {
					_ = conn.Close()
				}
			})
			seen := make(map[uint64]bool)
			for i := 0; i < 3; i++ {
				conn, err := db.Conn(ctx)
				require.NoError(t, err)
				conns = append(conns, conn)
				var connectionID uint64
				var innodbWait int64
				require.NoError(t, conn.QueryRowContext(ctx, "SELECT CONNECTION_ID(), @@SESSION.innodb_lock_wait_timeout").Scan(&connectionID, &innodbWait))
				assert.False(t, seen[connectionID], "必须同时持有三个不同物理连接")
				seen[connectionID] = true
				assert.EqualValues(t, sessionInnodbLockWaitSeconds, innodbWait)
				if tc.backend == authoritysql.BackendMySQL {
					var metadataWait int64
					require.NoError(t, conn.QueryRowContext(ctx, "SELECT @@SESSION.lock_wait_timeout").Scan(&metadataWait))
					assert.EqualValues(t, mysqlMetadataLockWaitSeconds, metadataWait)
				}
			}
			assert.Len(t, seen, 3)
		})
	}
}

// lockWaitProbe 是探针用例的假驱动:每条查询原样录下,回一行 values 或 err。
type lockWaitProbe struct {
	mu      sync.Mutex
	queries []string
	values  []driver.Value
	err     error
}

func (p *lockWaitProbe) record(query string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queries = append(p.queries, strings.Join(strings.Fields(query), " "))
}

func (p *lockWaitProbe) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.queries...)
}

type lockWaitProbeConnector struct{ probe *lockWaitProbe }

func (c lockWaitProbeConnector) Connect(context.Context) (driver.Conn, error) {
	return lockWaitProbeConn{probe: c.probe}, nil
}
func (lockWaitProbeConnector) Driver() driver.Driver { return lockWaitProbeDriver{} }

type lockWaitProbeDriver struct{}

func (lockWaitProbeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fixture: connector only")
}

type lockWaitProbeConn struct{ probe *lockWaitProbe }

func (lockWaitProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fixture: prepare not supported")
}
func (lockWaitProbeConn) Close() error              { return nil }
func (lockWaitProbeConn) Begin() (driver.Tx, error) { return nil, errors.New("fixture: tx not supported") }
func (c lockWaitProbeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.probe.record(query)
	if c.probe.err != nil {
		return nil, c.probe.err
	}
	columns := make([]string, len(c.probe.values))
	for i := range columns {
		columns[i] = "v" + strconv.Itoa(i)
	}
	return &lockWaitProbeRows{columns: columns, values: c.probe.values}, nil
}

type lockWaitProbeRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

func (r *lockWaitProbeRows) Columns() []string { return r.columns }
func (r *lockWaitProbeRows) Close() error      { return nil }
func (r *lockWaitProbeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.values)
	return nil
}
