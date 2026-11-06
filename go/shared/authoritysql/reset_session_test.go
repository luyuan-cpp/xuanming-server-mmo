package authoritysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"
)

// resettableFakeConn 在 backendFakeConn 上加 driver.SessionResetter。dead 置位后,复用前的存活检查回 ErrBadConn,
// 与 go-sql-driver 发现空闲连接已被服务端关掉(库重启、切主、链路断开空闲连接)时的行为一致。
type resettableFakeConn struct {
	*backendFakeConn
	dead bool
}

func (c *resettableFakeConn) ResetSession(context.Context) error {
	if c.dead {
		return driver.ErrBadConn
	}
	return nil
}

// dialingFakeConnector 每次建连给一条新连接,记下全部连接。
type dialingFakeConnector struct {
	conns []*resettableFakeConn
}

func (d *dialingFakeConnector) Connect(context.Context) (driver.Conn, error) {
	server := healthyMySQL()
	conn := &resettableFakeConn{backendFakeConn: &backendFakeConn{answer: func(query string) ([]string, []driver.Value, error) {
		if query == placementQuery {
			return fakeColumns(1), []driver.Value{[]byte("1")}, nil
		}
		return server.answer(query)
	}}}
	d.conns = append(d.conns, conn)
	return conn, nil
}

func (*dialingFakeConnector) Driver() driver.Driver { return opaqueDriver{} }

// 库重启之后池里的空闲连接已经失效:database/sql 取出它、复用前存活检查失败时,守卫回 ErrBadConn,
// database/sql 关掉它、新建一条,业务调用照常成功,失效连接上一条语句都不再发
// (docs/design/infra-reconnect-overview.md §4 R2)。
func TestPooledConnectionFailingLivenessCheckIsReplacedTransparently(t *testing.T) {
	dial := &dialingFakeConnector{}
	guarded, err := PinCurrentSession(dial, BackendMySQL, Reader())
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(guarded)
	t.Cleanup(func() { _ = db.Close() })
	queryOne(t, db)
	if len(dial.conns) != 1 {
		t.Fatalf("第一次查询只建一条连接: %d", len(dial.conns))
	}
	stale := dial.conns[0]
	sentBefore := len(stale.queries) + len(stale.execs)
	stale.dead = true

	queryOne(t, db) // 业务调用不能因为池里有失效连接而失败

	if len(dial.conns) != 2 {
		t.Fatalf("失效的空闲连接要换成新连接: %d", len(dial.conns))
	}
	if !stale.closed {
		t.Fatal("失效的连接要关掉")
	}
	if sent := len(stale.queries) + len(stale.execs); sent != sentBefore {
		t.Fatalf("失效连接上不能再发任何语句: 之前 %d 条,之后 %d 条", sentBefore, sent)
	}
}
