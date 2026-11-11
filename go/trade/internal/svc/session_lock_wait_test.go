package svc

// 会话锁等待上限(session_lock_wait.go)的回归测试,不连库:DSN 形状与启动读回探针。
// 真库上的逐连接读回与 friend 共用同一机制(authoritysql 的 pin + 驱动的 DSN 参数),
// 见 go/friend/internal/svc 的 TestAuthorityDB_SessionLockWaitsOnEveryConnection;trade 启动时 openMySQL 每次都跑这道探针。

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

	"shared/authoritysql"
	"trade/internal/config"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDSN_SessionLockWaits(t *testing.T) {
	parse := func(backend string) *drivermysql.Config {
		t.Helper()
		cfg, err := drivermysql.ParseDSN(BuildDSN(config.MySQLConf{Host: "localhost:3306", User: "u", Password: "p", DBName: "mmorpg_trade", Backend: backend}))
		require.NoError(t, err)
		return cfg
	}
	mysqlCfg := parse(string(authoritysql.BackendMySQL))
	tidbCfg := parse(string(authoritysql.BackendTiDB))
	defaultCfg := parse("") // 不写档位就是 tidb(authoritysql.ParseBackend)

	assert.Equal(t, strconv.Itoa(mysqlMetadataLockWaitSeconds), mysqlCfg.Params["lock_wait_timeout"],
		"mysql 档必须带元数据锁等待上限:缺省一年,DDL 排队时同表 DML 会在客户端放弃之后仍然执行")
	for name, cfg := range map[string]*drivermysql.Config{"tidb": tidbCfg, "不写档位": defaultCfg} {
		assert.NotContains(t, cfg.Params, "lock_wait_timeout", "%s:TiDB 档不发 lock_wait_timeout", name)
	}
	for name, cfg := range map[string]*drivermysql.Config{"mysql": mysqlCfg, "tidb": tidbCfg, "不写档位": defaultCfg} {
		assert.NotContains(t, cfg.Params, "innodb_lock_wait_timeout",
			"%s:行锁等待由 authoritysql 逐连接 SET,DSN 里再写一份会在建连后被覆盖,只是一个不生效的第二来源", name)
	}
	// 追加锁等待参数不改动原有的会话参数。
	assert.Equal(t, "'READ-COMMITTED'", mysqlCfg.Params["transaction_isolation"])
	assert.Equal(t, "'STRICT_TRANS_TABLES'", mysqlCfg.Params["sql_mode"])
	assert.Equal(t, "mmorpg_trade", mysqlCfg.DBName)
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
		{"不写档位按 tidb 核", authoritysql.Backend(""), []driver.Value{int64(1)}, nil, probeLockWaitTiDBSQL, ""},
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
