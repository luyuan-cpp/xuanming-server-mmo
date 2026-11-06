package logic

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
)

// errUnreachableStore 是 unreachableDB 每次取连接时回的错误。
var errUnreachableStore = errors.New("fixture: store unreachable")

type unreachableConnector struct{}

func (unreachableConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errUnreachableStore
}

func (unreachableConnector) Driver() driver.Driver { return unreachableDriver{} }

type unreachableDriver struct{}

func (unreachableDriver) Open(string) (driver.Conn, error) { return nil, errUnreachableStore }

// unreachableDB 是每次取连接都失败的库(2026-11-05):CommitBattleOutcome 对永久不合格的原件仍会复读一次既有终局,
// 用它证明复读失败时依旧按原件冲突永久拒绝,不被改判成存储故障。零值 sql.DB 没有连接器,一读就 panic,不能再用。
func unreachableDB(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(unreachableConnector{})
	t.Cleanup(func() { _ = db.Close() })
	return db
}
