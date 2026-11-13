package authoritysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"testing"
	"time"

	databasepb "proto/common/database"
)

// 连接复用时的核对(connector.revalidate,docs/design/player-save-direct-commit.md §9 第 1 条)。
// 全部不连真库:假连接沿用 backend_test.go 的 backendFakeConn 与 writer_frozen_test.go 的 gateConn,只数发出的语句。
// 空闲时长直接传给 revalidate,或改 guardedConn.returnedAt,不靠 sleep 等真实时间。

// tidb 档的运行池,刚用过的连接(空闲不到 idleProbeAfter)复用时一条语句都不发:
// 会话变量在建连时固定之后只有语句能改,而语句包络不放行;对端刚应答过,也不用再探测。
func TestTiDBRuntimeReuseSendsNoStatements(t *testing.T) {
	permissions := map[string]Permission{
		"reader": Reader(),
		"writer": Writer(gateConfig(databasepb.SqlWriterBootGrant_SELECT_ONLY)),
	}
	for name, permission := range permissions {
		for _, idle := range []time.Duration{0, idleProbeAfter - time.Nanosecond} {
			t.Run(name, func(t *testing.T) {
				conn := &backendFakeConn{answer: func(query string) ([]string, []driver.Value, error) {
					return nil, nil, errors.New("fixture: 复用时不该发语句 " + query)
				}}
				c := connector{backend: BackendTiDB, permission: permission}
				if err := c.revalidate(context.Background(), conn, idle); err != nil {
					t.Fatal(err)
				}
				if len(conn.execs)+len(conn.queries) != 0 {
					t.Fatalf("空闲 %v 复用时不该发语句: execs=%v queries=%v", idle, conn.execs, conn.queries)
				}
			})
		}
	}
}

// tidb 档的运行池,空闲超过 idleProbeAfter 的连接复用时发一条读:读回建连时固定的会话变量,
// 一次往返同时当存活探测和会话核对。不重放 SET,也不读 route_meta、注册表。
func TestTiDBRuntimeReuseAfterIdleSendsOneSessionRead(t *testing.T) {
	permissions := map[string]Permission{
		"reader": Reader(),
		"writer": Writer(gateConfig(databasepb.SqlWriterBootGrant_SELECT_ONLY)),
	}
	for name, permission := range permissions {
		for _, idle := range []time.Duration{idleProbeAfter, time.Minute, idleUnknown} {
			t.Run(name, func(t *testing.T) {
				conn := &backendFakeConn{answer: tidbAnswer}
				c := connector{backend: BackendTiDB, permission: permission}
				if err := c.revalidate(context.Background(), conn, idle); err != nil {
					t.Fatal(err)
				}
				if len(conn.execs) != 0 || !reflect.DeepEqual(conn.queries, []string{tidbSessionSQL}) {
					t.Fatalf("空闲 %v 复用只发一条会话读: execs=%v queries=%v", idle, conn.execs, conn.queries)
				}
			})
		}
	}
}

// 真驱动按列类型给值:布尔开关(tidb_low_resolution_tso)是 int64,不是文本。会话核对两种形状都要认,
// 否则真库上每次空闲探测都判成会话不符,连接被反复重建。
func TestTiDBSessionReadAcceptsDriverNativeIntegers(t *testing.T) {
	conn := &backendFakeConn{answer: func(query string) ([]string, []driver.Value, error) {
		columns, row, err := tidbAnswer(query)
		if err == nil && query == tidbSessionSQL {
			row[2], row[3] = int64(0), int64(0)
		}
		return columns, row, err
	}}
	if err := verifyTiDBSession(context.Background(), conn); err != nil {
		t.Fatalf("整数形状的 0 也是建连时固定的值: %v", err)
	}
	conn.answer = func(query string) ([]string, []driver.Value, error) {
		columns, row, err := tidbAnswer(query)
		if err == nil && query == tidbSessionSQL {
			row[3] = int64(1)
		}
		return columns, row, err
	}
	if err := verifyTiDBSession(context.Background(), conn); err == nil {
		t.Fatal("tidb_low_resolution_tso=1 必须判成会话不符")
	}
}

// 空闲探测没有回音(对端没发 FIN 就消失)或读回的会话不是建连时固定的那一组,复用核对都要失败:
// ResetSession 据此回 ErrBadConn,database/sql 换连接(TestIdleTiDBConnectionFailingProbeIsDiscarded)。
// 会话漂移这一格同时守着「驱动复用连接时不重置服务端会话」:哪天驱动升级后开始重置,隔离级别回到库的缺省值,这里先红。
func TestTiDBRuntimeReuseAfterIdleRejectsDeadPeerAndDriftedSession(t *testing.T) {
	drifted := func(column int, value string) backendAnswer {
		return func(query string) ([]string, []driver.Value, error) {
			columns, row, err := tidbAnswer(query)
			if err == nil && query == tidbSessionSQL {
				row[column] = []byte(value)
			}
			return columns, row, err
		}
	}
	answers := map[string]backendAnswer{
		"dead_peer": func(string) ([]string, []driver.Value, error) {
			return nil, nil, errors.New("fixture: read tcp: i/o timeout")
		},
		"txn_mode":         drifted(0, "optimistic"),
		"isolation":        drifted(1, "REPEATABLE-READ"),
		"read_staleness":   drifted(2, "-5"),
		"low_resolution":   drifted(3, "1"),
		"replica_read":     drifted(4, "follower"),
		"read_consistency": drifted(5, "weak"),
		"snapshot":         drifted(6, "2026-01-01 00:00:00"),
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			conn := &backendFakeConn{answer: answer}
			c := connector{backend: BackendTiDB, permission: Reader()}
			if err := c.revalidate(context.Background(), conn, idleProbeAfter); err == nil {
				t.Fatal("探测失败或会话不符时复用核对必须失败")
			}
		})
	}
}

// 守卫这一层:空闲的 tidb 连接探测失败时回 ErrBadConn 并隔离,这条连接上除了那一条探测不再发任何语句;
// 刚归还的连接不探测,直接可用。database/sql 收到 ErrBadConn 之后换连接,由
// TestPooledConnectionFailingLivenessCheckIsReplacedTransparently 覆盖。
func TestIdleTiDBConnectionFailingProbeIsDiscarded(t *testing.T) {
	conn := &backendFakeConn{answer: tidbAnswer}
	guarded, err := PinCurrentSession(&backendFakeConnector{conn: conn}, BackendTiDB, Reader())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := guarded.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := raw.(*guardedConn)
	// 对端从这里起不再应答,而且没有 FIN:驱动的本地存活检查看不出来(fake 不实现 SessionResetter)。
	conn.answer = func(string) ([]string, []driver.Value, error) {
		return nil, nil, errors.New("fixture: read tcp: i/o timeout")
	}
	if !c.IsValid() {
		t.Fatal("归还时连接看起来是好的")
	}
	// 归还时刻放到将来,空闲时长必为负:下面「窗口之内不探测」的断言不取决于机器这一刻快不快。
	c.returnedAt = time.Now().Add(time.Hour)
	sent := len(conn.queries) + len(conn.execs)
	if err := c.ResetSession(context.Background()); err != nil || !c.IsValid() {
		t.Fatalf("刚归还的连接复用不探测: %v valid=%v", err, c.IsValid())
	}
	if got := len(conn.queries) + len(conn.execs); got != sent {
		t.Fatalf("探测窗口之内不该发语句: 之前 %d 条,之后 %d 条", sent, got)
	}
	// 把归还时刻拨回去,不靠 sleep 等真实时间。
	c.returnedAt = time.Now().Add(-time.Minute)
	if err := c.ResetSession(context.Background()); !errors.Is(err, driver.ErrBadConn) || c.IsValid() {
		t.Fatalf("空闲连接探测失败要回 ErrBadConn 并隔离: %v valid=%v", err, c.IsValid())
	}
	if got := len(conn.queries) + len(conn.execs); got != sent+1 || conn.queries[len(conn.queries)-1] != tidbSessionSQL {
		t.Fatalf("失效连接上只发那一条探测: 之前 %d 条,之后 %d 条,最后一条 %q", sent, got, conn.queries[len(conn.queries)-1])
	}
}

// mysql 档复用时只核对端还是不是主库,一条读,不重放 SET;不看空闲时长,刚用过的连接也核(主从切换随时可能发生)。
// 降级之后这条连接被隔离并回 ErrBadConn,由 TestMySQLResetSessionRecheckAfterDemotion 覆盖。
func TestMySQLRuntimeReuseOnlyVerifiesThePrimary(t *testing.T) {
	permissions := map[string]Permission{
		"reader": Reader(),
		"writer": Writer(gateConfig(databasepb.SqlWriterBootGrant_LOCK_TOUCH)),
	}
	for name, permission := range permissions {
		t.Run(name, func(t *testing.T) {
			conn := &backendFakeConn{answer: healthyMySQL().answer}
			c := connector{backend: BackendMySQL, permission: permission}
			if err := c.revalidate(context.Background(), conn, 0); err != nil {
				t.Fatal(err)
			}
			if len(conn.execs) != 0 || !reflect.DeepEqual(conn.queries, []string{mysqlServerSQL}) {
				t.Fatalf("mysql 档复用只发一条主库核对: execs=%v queries=%v", conn.execs, conn.queries)
			}
		})
	}
}

// Schema、Operator 的池不在热路径上,Schema 的包络还放行一条改锁等待的 SET:它们复用时照旧整个重放 pin。
func TestSchemaAndOperatorReuseReplayTheFullPin(t *testing.T) {
	permissions := map[string]Permission{
		"schema":   Schema("fixture@%"),
		"operator": Operator("fixture@%"),
	}
	for name, permission := range permissions {
		t.Run(name, func(t *testing.T) {
			pinned := &backendFakeConn{answer: tidbAnswer}
			reused := &backendFakeConn{answer: tidbAnswer}
			c := connector{backend: BackendTiDB, permission: permission}
			if err := c.pin(context.Background(), pinned); err != nil {
				t.Fatal(err)
			}
			if err := c.revalidate(context.Background(), reused, 0); err != nil {
				t.Fatal(err)
			}
			if len(pinned.execs) == 0 || !reflect.DeepEqual(pinned.execs, reused.execs) || !reflect.DeepEqual(pinned.queries, reused.queries) {
				t.Fatalf("复用要和建连发同样的语句: pin execs=%v queries=%v, reuse execs=%v queries=%v",
					pinned.execs, pinned.queries, reused.execs, reused.queries)
			}
		})
	}
}

// 写者池复用时不再读 route_meta 与注册表。建连之后写者被退休:复用照样通过,下一笔写事务在 slot 行上被拦下。
// 判定从「取连接时、不持锁」挪到了「事务里、行锁下」,结论不变,而且调用方拿到的是确定的 ErrWriterDenied。
func TestRetiredWriterPassesReuseAndIsRefusedByTheGate(t *testing.T) {
	primitives := map[string]databasepb.SqlWriterBootGrant_LockPrimitive{
		"SELECT_ONLY": databasepb.SqlWriterBootGrant_SELECT_ONLY,
		"LOCK_TOUCH":  databasepb.SqlWriterBootGrant_LOCK_TOUCH,
	}
	for name, primitive := range primitives {
		t.Run(name, func(t *testing.T) {
			conn := healthyGateConn()
			config := gateConfig(primitive)
			c := connector{backend: BackendTiDB, permission: Writer(config)}
			// 退休:登记行 REVOKED;slot 行不再 ACTIVE(真库上是删掉,假连接用状态表示),条件写也不再命中。
			conn.registry[4] = int64(2)
			conn.slot[2] = int64(2)
			conn.touched = 0

			if err := c.revalidate(context.Background(), conn, 0); err != nil {
				t.Fatalf("复用不核注册: %v", err)
			}
			if conn.execs != 0 || conn.queries != 0 {
				t.Fatalf("写者池复用不发语句: execs=%d queries=%d", conn.execs, conn.queries)
			}

			gate := runtimeGate{config: config}
			attempt, err := gate.beforeBegin(context.Background(), conn)
			if err == nil {
				err = attempt.enter(context.Background(), conn)
			}
			if !errors.Is(err, ErrWriterDenied) || errors.Is(err, ErrWriterFrozen) {
				t.Fatalf("退休的写者要在事务里被拒,且不是可重试的冻结: %v", err)
			}
		})
	}
}

// connector.revalidate 不重放会话设置的前提:运行池(Reader、Writer)的语句包络不让业务语句改会话变量、默认库或事务边界。
// 要放宽包络先看这里:放行其中任何一类,复用的连接就可能带着被改过的会话去做权威读写,revalidate 得跟着改回逐次重放。
func TestRuntimeEnvelopeKeepsPinnedSessionFixed(t *testing.T) {
	statements := []string{
		"SET SESSION tidb_snapshot='2026-01-01 00:00:00'",
		"SET SESSION transaction_isolation='REPEATABLE-READ'",
		"SET tidb_replica_read='follower'",
		"SET @@session.sql_mode=''",
		"SET autocommit=0",
		"SET SESSION lock_wait_timeout = 5, innodb_lock_wait_timeout = 5", // 只有 Schema 权限放行这一条
		"SET TRANSACTION ISOLATION LEVEL SERIALIZABLE",
		"SET NAMES latin1",
		"USE other_schema",
		"BEGIN",
		"START TRANSACTION",
		"COMMIT",
		"ROLLBACK",
		"SELECT /*+ SET_VAR(tidb_snapshot='x') */ 1",
		"SELECT /*+ SET_VAR(autocommit=0) */ 1",
		"UPDATE /*+ SET_VAR(tidb_txn_mode='optimistic') */ t SET n=1",
		"SELECT 1; SET SESSION tidb_snapshot=''",
	}
	kinds := map[string]permissionKind{"reader": permissionReader, "writer": permissionWriter}
	for name, kind := range kinds {
		for _, statement := range statements {
			if err := validatePermissionStatement(statement, kind); err == nil {
				t.Fatalf("%s 池放行了会改会话的语句: %s", name, statement)
			}
		}
	}
}
