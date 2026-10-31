package authoritysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// 所有用例均不连真库。fake 只替代驱动接缝:按 SQL 文本应答,记下实际发出的每条语句。
type backendFakeRows struct {
	columns []string
	row     []driver.Value
	read    bool
}

func (r *backendFakeRows) Columns() []string { return r.columns }
func (r *backendFakeRows) Close() error      { return nil }
func (r *backendFakeRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	copy(dest, r.row)
	return nil
}

type backendAnswer func(query string) ([]string, []driver.Value, error)

type backendFakeConn struct {
	answer  backendAnswer
	execs   []string
	queries []string
	closed  bool
}

func (c *backendFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fixture: 不应走预处理")
}
func (c *backendFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fixture: 不应开事务")
}
func (c *backendFakeConn) Close() error { c.closed = true; return nil }
func (c *backendFakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.execs = append(c.execs, query)
	return driver.RowsAffected(0), nil
}
func (c *backendFakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	columns, row, err := c.answer(query)
	if err != nil {
		return nil, err
	}
	return &backendFakeRows{columns: columns, row: row}, nil
}

// 每次都交出同一条 fake 连接:用例要看的是这一条连接上发过什么。
type backendFakeConnector struct {
	conn     *backendFakeConn
	connects int
}

func (c *backendFakeConnector) Connect(context.Context) (driver.Conn, error) {
	c.connects++
	return c.conn, nil
}
func (*backendFakeConnector) Driver() driver.Driver { return opaqueDriver{} }

func fakeColumns(n int) []string { return make([]string, n) }

// mysqlServer 描述一台 fake 的 MySQL 兼容服务器;字段都是服务器读回的原文。
type mysqlServer struct {
	version, comment, readOnly, superReadOnly, isolation string
}

func healthyMySQL() mysqlServer {
	return mysqlServer{version: "8.0.36", comment: "MySQL Community Server - GPL", readOnly: "0", superReadOnly: "0", isolation: "READ-COMMITTED"}
}

func (s mysqlServer) answer(query string) ([]string, []driver.Value, error) {
	switch query {
	case mysqlServerSQL:
		return fakeColumns(4), []driver.Value{[]byte(s.version), []byte(s.comment), []byte(s.readOnly), []byte(s.superReadOnly)}, nil
	case mysqlSessionSQL:
		return fakeColumns(5), []driver.Value{[]byte(s.version), []byte(s.comment), []byte(s.readOnly), []byte(s.superReadOnly), []byte(s.isolation)}, nil
	case "SELECT CURRENT_USER(),DATABASE()":
		return fakeColumns(2), []driver.Value{[]byte("fixture@%"), []byte("fixture")}, nil
	}
	return nil, nil, fmt.Errorf("fixture: 未预期的语句 %s", query)
}

func tidbAnswer(query string) ([]string, []driver.Value, error) {
	switch query {
	case "SELECT tidb_version()":
		return fakeColumns(1), []driver.Value{[]byte("Release Version: v8.5.2")}, nil
	case "SELECT @@tidb_txn_mode,@@transaction_isolation,@@tidb_read_staleness,@@tidb_low_resolution_tso,@@tidb_replica_read,@@tidb_read_consistency,@@tidb_snapshot":
		return fakeColumns(7), []driver.Value{[]byte("pessimistic"), []byte("READ-COMMITTED"), []byte("0"), []byte("0"), []byte("leader"), []byte("strict"), []byte("")}, nil
	case "SELECT CURRENT_USER(),DATABASE()":
		return fakeColumns(2), []driver.Value{[]byte("fixture@%"), []byte("fixture")}, nil
	}
	return nil, nil, fmt.Errorf("fixture: 未预期的语句 %s", query)
}

func pinnedDB(t *testing.T, conn *backendFakeConn, backend Backend) *sql.DB {
	t.Helper()
	guarded, err := PinCurrentSession(&backendFakeConnector{conn: conn}, backend, Reader())
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(guarded)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestParseBackend(t *testing.T) {
	for raw, want := range map[string]Backend{"": BackendTiDB, "tidb": BackendTiDB, "mysql": BackendMySQL} {
		got, err := ParseBackend(raw)
		if err != nil || got != want {
			t.Fatalf("ParseBackend(%q) = %q, %v, want %q", raw, got, err, want)
		}
	}
	// 写错的档位不能被猜成任何一档:大小写、空白、别名一律拒绝。
	for _, raw := range []string{"TiDB", "MYSQL", " mysql", "mysql ", "mariadb", "oracle", "true", "false"} {
		if got, err := ParseBackend(raw); err == nil {
			t.Fatalf("ParseBackend(%q) = %q,非法值必须报错", raw, got)
		}
	}
}

func TestLoadBackendForCommandLineTools(t *testing.T) {
	for value, want := range map[string]Backend{"": BackendTiDB, "tidb": BackendTiDB, "mysql": BackendMySQL} {
		got, err := LoadBackend(func(name string) string {
			if name != BackendEnv {
				t.Fatalf("读了别的环境变量: %s", name)
			}
			return value
		})
		if err != nil || got != want {
			t.Fatalf("LoadBackend(%q) = %q, %v", value, got, err)
		}
	}
	if _, err := LoadBackend(func(string) string { return "oracle" }); err == nil {
		t.Fatal("非法环境变量必须报错")
	}
	if _, err := LoadBackend(nil); err == nil {
		t.Fatal("缺 getenv 必须报错")
	}
}

func TestIllegalBackendRefusedBeforeTouchingServer(t *testing.T) {
	const illegal = Backend("oracle")
	ctx := context.Background()
	if _, err := Open(ctx, "root@tcp(127.0.0.1:1)/expected", "expected", illegal, 4, Reader()); err == nil || !strings.Contains(err.Error(), "档位") {
		t.Fatalf("Open 必须先拒非法档位: %v", err)
	}
	if _, err := OpenForMigration(ctx, "root@tcp(127.0.0.1:1)/expected", "expected", illegal, Schema("schema_fixture@%")); err == nil || !strings.Contains(err.Error(), "档位") {
		t.Fatalf("OpenForMigration 必须先拒非法档位: %v", err)
	}
	inner := &backendFakeConnector{conn: &backendFakeConn{answer: healthyMySQL().answer}}
	if _, err := PinCurrentSession(inner, illegal, Reader()); err == nil || inner.connects != 0 {
		t.Fatalf("PinCurrentSession 必须先拒非法档位: %v connects=%d", err, inner.connects)
	}
	// db 是 nil:碰一下就 panic,证明校验在任何查询之前。
	if err := AssertCurrentSession(ctx, nil, illegal); err == nil {
		t.Fatal("AssertCurrentSession 必须先拒非法档位")
	}
}

func TestMySQLPinSendsNoTiDBStatements(t *testing.T) {
	conn := &backendFakeConn{answer: healthyMySQL().answer}
	c := connector{backend: BackendMySQL, permission: Reader()}
	if err := c.pin(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	for _, statement := range append(append([]string(nil), conn.execs...), conn.queries...) {
		if strings.Contains(strings.ToLower(statement), "tidb") {
			t.Fatalf("mysql 档发了 TiDB 语句: %s", statement)
		}
	}
	wantSet := map[string]bool{
		"SET SESSION autocommit=1":                           false,
		"SET SESSION transaction_isolation='READ-COMMITTED'": false,
		"SET SESSION sql_mode='STRICT_TRANS_TABLES'":         false,
		"SET SESSION innodb_lock_wait_timeout=1":             false,
	}
	for _, statement := range conn.execs {
		if _, ok := wantSet[statement]; !ok {
			t.Fatalf("mysql 档发了预期外的 SET: %s", statement)
		}
		wantSet[statement] = true
	}
	for statement, sent := range wantSet {
		if !sent {
			t.Fatalf("mysql 档漏了 %s", statement)
		}
	}
}

func TestTiDBPinKeepsSettingsAndSkipsMySQLProbe(t *testing.T) {
	conn := &backendFakeConn{answer: tidbAnswer}
	c := connector{backend: BackendTiDB, permission: Reader()}
	if err := c.pin(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	sent := map[string]bool{}
	for _, statement := range conn.execs {
		sent[statement] = true
	}
	for _, statement := range tidbSettings {
		if !sent[statement] {
			t.Fatalf("tidb 档漏了 %s", statement)
		}
	}
	for _, query := range conn.queries {
		if strings.Contains(query, "read_only") {
			t.Fatalf("tidb 档不该读 MySQL 只读开关: %s", query)
		}
	}
}

func TestMySQLPinRefusesWrongServerBeforeAnySet(t *testing.T) {
	for name, change := range map[string]func(*mysqlServer){
		"从库 read_only":       func(s *mysqlServer) { s.readOnly = "1" },
		"从库 super_read_only": func(s *mysqlServer) { s.superReadOnly = "1" },
		"VERSION 里有 TiDB":    func(s *mysqlServer) { s.version, s.comment = "8.0.11-TiDB-v8.5.2", "TiDB Server" },
		"server-version 改过的 TiDB": func(s *mysqlServer) {
			s.version, s.comment = "8.0.36", "TiDB Server (Apache License 2.0) Community Edition"
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := healthyMySQL()
			change(&server)
			conn := &backendFakeConn{answer: server.answer}
			err := (connector{backend: BackendMySQL, permission: Reader()}).pin(context.Background(), conn)
			if !errors.Is(err, ErrBackendMismatch) {
				t.Fatalf("必须拒绝: %v", err)
			}
			if len(conn.execs) != 0 {
				t.Fatalf("身份不符不该再动会话: %v", conn.execs)
			}
		})
	}
}

func TestMySQLPinTreatsMissingVariableAsMismatchAndTransportAsTransport(t *testing.T) {
	// MariaDB 没有 @@super_read_only:服务器明确报错,算档位不符。
	unknownVariable := &backendFakeConn{answer: func(string) ([]string, []driver.Value, error) {
		return nil, nil, &mysql.MySQLError{Number: 1193, Message: "Unknown system variable 'super_read_only'"}
	}}
	err := (connector{backend: BackendMySQL, permission: Reader()}).pin(context.Background(), unknownVariable)
	if !errors.Is(err, ErrBackendMismatch) {
		t.Fatalf("没有只读开关的服务器必须拒绝: %v", err)
	}
	// 连接类错误不能被说成部署错误。
	lost := errors.New("fixture: 连接断了")
	transport := &backendFakeConn{answer: func(string) ([]string, []driver.Value, error) { return nil, nil, lost }}
	err = (connector{backend: BackendMySQL, permission: Reader()}).pin(context.Background(), transport)
	if err == nil || errors.Is(err, ErrBackendMismatch) {
		t.Fatalf("连接失败不算档位不符: %v", err)
	}
}

func TestConnectKeepsMismatchReasonAndClosesConnection(t *testing.T) {
	server := healthyMySQL()
	server.superReadOnly = "1"
	conn := &backendFakeConn{answer: server.answer}
	guarded, err := PinCurrentSession(&backendFakeConnector{conn: conn}, BackendMySQL, Reader())
	if err != nil {
		t.Fatal(err)
	}
	_, err = guarded.Connect(context.Background())
	if !errors.Is(err, ErrBackendMismatch) || !strings.Contains(err.Error(), "主库") {
		t.Fatalf("启动错误必须说清连到了从库: %v", err)
	}
	if !conn.closed {
		t.Fatal("核验失败的物理连接必须关闭")
	}
}

func TestMySQLResetSessionRecheckAfterDemotion(t *testing.T) {
	server := healthyMySQL()
	conn := &backendFakeConn{}
	conn.answer = func(query string) ([]string, []driver.Value, error) { return server.answer(query) }
	guarded, err := PinCurrentSession(&backendFakeConnector{conn: conn}, BackendMySQL, Reader())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := guarded.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := raw.(*guardedConn)
	if err := c.ResetSession(context.Background()); err != nil || !c.IsValid() {
		t.Fatalf("主库复用连接应通过: %v valid=%v", err, c.IsValid())
	}
	// 主从切换:同一条池内连接所在实例被降成只读。回池复用前必须发现,不能继续拿它做权威读写。
	server.readOnly = "1"
	if err := c.ResetSession(context.Background()); !errors.Is(err, ErrOutcomeUnknown) || c.IsValid() {
		t.Fatalf("降级后必须隔离这条连接: %v valid=%v", err, c.IsValid())
	}
}

func TestAssertCurrentSessionMySQL(t *testing.T) {
	ctx := context.Background()
	if err := AssertCurrentSession(ctx, pinnedDB(t, &backendFakeConn{answer: healthyMySQL().answer}, BackendMySQL), BackendMySQL); err != nil {
		t.Fatalf("主库 + RC 必须通过: %v", err)
	}
	notCommitted := healthyMySQL()
	notCommitted.isolation = "REPEATABLE-READ"
	if err := AssertCurrentSession(ctx, pinnedDB(t, &backendFakeConn{answer: notCommitted.answer}, BackendMySQL), BackendMySQL); err == nil {
		t.Fatal("隔离级别不是 RC 必须拒绝")
	}
	// 读回一刻才变成只读:pin 已过,Assert 自己也要拦。
	demoted := healthyMySQL()
	conn := &backendFakeConn{}
	conn.answer = func(query string) ([]string, []driver.Value, error) {
		if query == mysqlSessionSQL {
			demoted.readOnly = "1"
		}
		return demoted.answer(query)
	}
	if err := AssertCurrentSession(ctx, pinnedDB(t, conn, BackendMySQL), BackendMySQL); !errors.Is(err, ErrBackendMismatch) {
		t.Fatalf("只读实例必须拒绝: %v", err)
	}
	for _, query := range conn.queries {
		if strings.Contains(strings.ToLower(query), "tidb") {
			t.Fatalf("mysql 档发了 TiDB 语句: %s", query)
		}
	}
}

func TestAssertCurrentSessionZeroValueIsTiDB(t *testing.T) {
	conn := &backendFakeConn{answer: tidbAnswer}
	if err := AssertCurrentSession(context.Background(), pinnedDB(t, conn, ""), ""); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, query := range conn.queries {
		found = found || query == "SELECT tidb_version()"
	}
	if !found {
		t.Fatalf("零值必须走 tidb 档核对: %v", conn.queries)
	}
}

func TestTiDBAssertRefusesMySQL(t *testing.T) {
	// tidb 档接到 MySQL:tidb_version() 不存在。
	conn := &backendFakeConn{answer: func(query string) ([]string, []driver.Value, error) {
		if query == "SELECT tidb_version()" {
			return nil, nil, &mysql.MySQLError{Number: 1305, Message: "FUNCTION tidb_version does not exist"}
		}
		return tidbAnswer(query)
	}}
	if err := AssertCurrentSession(context.Background(), pinnedDB(t, conn, BackendTiDB), BackendTiDB); err == nil || !strings.Contains(err.Error(), "TiDB") {
		t.Fatalf("tidb 档必须拒绝 MySQL: %v", err)
	}
}
