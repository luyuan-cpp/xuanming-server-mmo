package auditoutbox

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"db/internal/dbtest"
)

const testDBName = "player_store_1000000_db"

func lockResult(v driver.Value) *dbtest.Rows {
	return &dbtest.Rows{Columns: []string{"v"}, Values: [][]driver.Value{{v}}}
}

func argValues(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, arg := range args {
		out[i] = arg.Value
	}
	return out
}

// 探测只走主键前缀:(shard, kind, created_ms >= ?),只取 created_ms、LIMIT 1;没有行时 found=false,不是错误。
func TestSQLStore_OldestProbesThePrimaryKeyPrefix(t *testing.T) {
	var gotArgs []driver.Value
	rowsToReturn := &dbtest.Rows{Columns: []string{"created_ms"}, Values: [][]driver.Value{{int64(42)}}}
	db, _ := dbtest.Open(t, func(_ context.Context, query string, args []driver.NamedValue) (*dbtest.Rows, error) {
		if query != sqlOldest {
			t.Errorf("unexpected statement %q", query)
		}
		gotArgs = argValues(args)
		return rowsToReturn, nil
	})
	store := NewSQLStore(db, testDBName)
	createdMs, found, err := store.Oldest(context.Background(), 3, txlogKind, 1000)
	if err != nil || !found || createdMs != 42 {
		t.Fatalf("Oldest = %d, %v, %v", createdMs, found, err)
	}
	if len(gotArgs) != 3 || gotArgs[0] != int64(3) || gotArgs[1] != int64(txlogKind) || gotArgs[2] != int64(1000) {
		t.Fatalf("args = %v, want shard, kind, from", gotArgs)
	}
	if strings.Contains(sqlOldest, "payload") {
		t.Fatal("the probe must not read payloads (a snapshot row can be 4 MiB)")
	}
	rowsToReturn = &dbtest.Rows{Columns: []string{"created_ms"}}
	if _, found, err := store.Oldest(context.Background(), 3, txlogKind, 1000); err != nil || found {
		t.Fatalf("empty unit: found=%v err=%v", found, err)
	}
}

// 取锁、读、删、放锁都在同一条钉住的连接上,按这个顺序;删除按整主键,参数顺序是 shard、kind、(created_ms, player_id, record_id)…;
// 放锁成功之后连接还回池里。
func TestSQLStore_LockReadDeleteRelease(t *testing.T) {
	var deleteQuery string
	var deleteArgs, lockArgs []driver.Value
	db, rec := dbtest.Open(t, func(_ context.Context, query string, args []driver.NamedValue) (*dbtest.Rows, error) {
		switch query {
		case sqlGetLock:
			lockArgs = argValues(args)
			return lockResult(int64(1)), nil
		case sqlRead:
			return &dbtest.Rows{Columns: []string{"created_ms", "player_id", "record_id", "payload"}, Values: [][]driver.Value{
				{int64(10), int64(7), int64(100), []byte("a")},
				{int64(11), int64(8), int64(101), []byte("b")},
			}}, nil
		case sqlReleaseLock:
			return lockResult(int64(1)), nil
		}
		if strings.HasPrefix(query, sqlDeleteHead) {
			deleteQuery, deleteArgs = query, argValues(args)
			return nil, nil
		}
		t.Errorf("unexpected statement %q", query)
		return nil, nil
	})
	store := NewSQLStore(db, testDBName)
	lock, held, err := store.LockUnit(context.Background(), 3)
	if err != nil || !held {
		t.Fatalf("LockUnit = %v, %v", held, err)
	}
	if len(lockArgs) != 1 || lockArgs[0] != "db_audit_outbox.03."+testDBName {
		t.Fatalf("lock name = %v", lockArgs)
	}
	rows, err := lock.Read(context.Background(), txlogKind, 5, 500)
	if err != nil || len(rows) != 2 || rows[1].RecordID != 101 || string(rows[1].Payload) != "b" || rows[0].PlayerID != 7 {
		t.Fatalf("Read = %+v, %v", rows, err)
	}
	if err := lock.Delete(context.Background(), txlogKind, rows); err != nil {
		t.Fatal(err)
	}
	if deleteQuery != sqlDeleteHead+"(?,?,?),(?,?,?))" {
		t.Fatalf("delete query = %q", deleteQuery)
	}
	want := []driver.Value{int64(3), int64(txlogKind), int64(10), int64(7), int64(100), int64(11), int64(8), int64(101)}
	if len(deleteArgs) != len(want) {
		t.Fatalf("delete args = %v, want %v", deleteArgs, want)
	}
	for i := range want {
		if deleteArgs[i] != want[i] {
			t.Fatalf("delete args = %v, want %v", deleteArgs, want)
		}
	}
	lock.Release()
	got := rec.Statements()
	if len(got) != 4 || got[0] != sqlGetLock || got[1] != sqlRead || !strings.HasPrefix(got[2], sqlDeleteHead) || got[3] != sqlReleaseLock {
		t.Fatalf("statements = %v", got)
	}
	if stats := db.Stats(); stats.OpenConnections != 1 || stats.InUse != 0 {
		t.Fatalf("a cleanly released connection goes back to the pool, stats = %+v", stats)
	}
}

// 别的会话持有这一片的锁:GET_LOCK 回 0,held=false,不发任何别的语句,连接照常还回池里。
func TestSQLStore_BusyLockIsNotHeld(t *testing.T) {
	db, rec := dbtest.Open(t, func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
		if query != sqlGetLock {
			t.Errorf("unexpected statement %q", query)
		}
		return lockResult(int64(0)), nil
	})
	lock, held, err := NewSQLStore(db, testDBName).LockUnit(context.Background(), 9)
	if err != nil || held || lock != nil {
		t.Fatalf("LockUnit = %v, %v, %v", lock, held, err)
	}
	if got := rec.Statements(); len(got) != 1 {
		t.Fatalf("statements = %v", got)
	}
	if stats := db.Stats(); stats.OpenConnections != 1 {
		t.Fatalf("a connection without the lock goes back to the pool, stats = %+v", stats)
	}
}

// GET_LOCK 出错或回 NULL:这个会话说不定已经拿到了锁,连接必须丢掉,不能还回池里。
func TestSQLStore_UncertainLockDropsTheConnection(t *testing.T) {
	cases := map[string]func() (*dbtest.Rows, error){
		"statement error": func() (*dbtest.Rows, error) { return nil, errors.New("read timeout") },
		"NULL result":     func() (*dbtest.Rows, error) { return lockResult(nil), nil },
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			db, _ := dbtest.Open(t, func(context.Context, string, []driver.NamedValue) (*dbtest.Rows, error) { return result() })
			if _, held, err := NewSQLStore(db, testDBName).LockUnit(context.Background(), 1); err == nil || held {
				t.Fatalf("held=%v err=%v, want an error", held, err)
			}
			if stats := db.Stats(); stats.OpenConnections != 0 {
				t.Fatalf("the connection must be dropped, stats = %+v", stats)
			}
		})
	}
}

// 放锁失败或回 0(锁已不属于这个会话):丢掉这条物理连接,服务端随会话结束放锁;不能把可能仍持锁的会话还回池里。
func TestSQLStore_FailedReleaseDropsTheConnection(t *testing.T) {
	cases := map[string]func() (*dbtest.Rows, error){
		"statement error": func() (*dbtest.Rows, error) { return nil, errors.New("connection reset") },
		"not the holder":  func() (*dbtest.Rows, error) { return lockResult(int64(0)), nil },
		"NULL result":     func() (*dbtest.Rows, error) { return lockResult(nil), nil },
	}
	for name, release := range cases {
		t.Run(name, func(t *testing.T) {
			db, _ := dbtest.Open(t, func(_ context.Context, query string, _ []driver.NamedValue) (*dbtest.Rows, error) {
				if query == sqlGetLock {
					return lockResult(int64(1)), nil
				}
				return release()
			})
			lock, held, err := NewSQLStore(db, testDBName).LockUnit(context.Background(), 1)
			if err != nil || !held {
				t.Fatalf("LockUnit = %v, %v", held, err)
			}
			lock.Release()
			if stats := db.Stats(); stats.OpenConnections != 0 {
				t.Fatalf("the connection must be dropped, stats = %+v", stats)
			}
		})
	}
}

// 锁名:前缀、两位片号、库名;不同的库、不同的片各不相同;不超过命名锁 64 字节的上限。
func TestUnitLockName(t *testing.T) {
	if got := unitLockName(testDBName, 3); got != "db_audit_outbox.03.player_store_1000000_db" {
		t.Fatalf("lock name = %q", got)
	}
	if unitLockName(testDBName, 3) == unitLockName("player_store_1000001_db", 3) || unitLockName(testDBName, 3) == unitLockName(testDBName, 13) {
		t.Fatal("lock names must differ across storages and shards")
	}
	if got := unitLockName(strings.Repeat("x", 100), 1); len(got) != maxLockNameBytes {
		t.Fatalf("overlong name length = %d, want truncation to %d", len(got), maxLockNameBytes)
	}
}
