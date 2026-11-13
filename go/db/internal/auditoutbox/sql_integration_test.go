//go:build integration

package auditoutbox

// 真库用例(设计 §7、§12)。默认不跑:
//
//	GODB_MYSQL_ITEST_DSN='root:pwd@tcp(127.0.0.1:3306)/' go test -tags=integration ./internal/auditoutbox/...
//
// 账号要能建库。每个用例建一个一次性库,结束后删掉。DSN 指向 TiDB 就是 TiDB 上的验证(命名锁在 TiDB 上的行为
// 只有这里核得到)。GODB_IT_REQUIRE=1 时依赖缺失一律 Fatal,不 Skip。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"

	dbpb "proto/common/database"
)

var itSeq atomic.Int64

// itOutbox 建一个一次性库和发件箱表,返回连同一个库的两个独立连接池(两个「副本」)和库名。
func itOutbox(t *testing.T) (*sql.DB, *sql.DB, string, context.Context) {
	t.Helper()
	dsn := os.Getenv("GODB_MYSQL_ITEST_DSN")
	if dsn == "" {
		if os.Getenv("GODB_IT_REQUIRE") == "1" {
			t.Fatal("GODB_IT_REQUIRE=1 but GODB_MYSQL_ITEST_DSN is empty")
		}
		t.Skip("GODB_MYSQL_ITEST_DSN is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("auditoutbox_it_%d_%d", os.Getpid(), itSeq.Add(1))
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name) })
	cfg.DBName = name
	open := func() *sql.DB {
		db, err := sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	a, b := open(), open()
	model := proto2mysql.NewDB()
	outbox := &dbpb.PlayerAuditOutbox{}
	model.RegisterTable(outbox)
	if _, err := a.ExecContext(ctx, model.GetCreateTableSQL(outbox)); err != nil {
		t.Fatal(err)
	}
	return a, b, name, ctx
}

func itInsert(t *testing.T, ctx context.Context, db *sql.DB, shard uint32, kind int32, createdMs, playerID, recordID uint64) {
	t.Helper()
	_, err := db.ExecContext(ctx, "INSERT INTO player_audit_outbox (shard,kind,created_ms,player_id,record_id,save_seq,payload) VALUES (?,?,?,?,?,1,?)",
		shard, kind, createdMs, playerID, recordID, []byte(fmt.Sprintf("payload-%d", recordID)))
	if err != nil {
		t.Fatal(err)
	}
}

// 两个副本抢同一片:同一时刻只有一个拿得到;不同的片互不影响;放锁之后另一个才拿得到。
func TestIT_UnitLockIsExclusiveAcrossReplicas(t *testing.T) {
	a, b, name, ctx := itOutbox(t)
	storeA, storeB := NewSQLStore(a, name), NewSQLStore(b, name)
	lockA, held, err := storeA.LockUnit(ctx, 3)
	if err != nil || !held {
		t.Fatalf("A: held=%v err=%v", held, err)
	}
	if _, held, err := storeB.LockUnit(ctx, 3); err != nil || held {
		t.Fatalf("B must not get shard 3 while A holds it: held=%v err=%v", held, err)
	}
	lockB4, held, err := storeB.LockUnit(ctx, 4)
	if err != nil || !held {
		t.Fatalf("B must get another shard: held=%v err=%v", held, err)
	}
	lockB4.Release()
	lockA.Release()
	lockB, held, err := storeB.LockUnit(ctx, 3)
	if err != nil || !held {
		t.Fatalf("B must get shard 3 after A released it: held=%v err=%v", held, err)
	}
	lockB.Release()
}

// 持锁的会话断开(进程崩溃、连接被丢):服务端放锁,别的副本接手。
func TestIT_LockIsFreedWhenTheSessionEnds(t *testing.T) {
	a, b, name, ctx := itOutbox(t)
	lockA, held, err := NewSQLStore(a, name).LockUnit(ctx, 5)
	if err != nil || !held {
		t.Fatalf("A: held=%v err=%v", held, err)
	}
	discard(lockA.(*sqlUnitLock).conn)
	deadline := time.Now().Add(10 * time.Second)
	for {
		lockB, held, err := NewSQLStore(b, name).LockUnit(ctx, 5)
		if err != nil {
			t.Fatal(err)
		}
		if held {
			lockB.Release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the lock of a dropped session was not freed within 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// 探测、读按主键序;删除按整主键,只删给定的行,同片同 kind 的别的行和别的片都不动。
func TestIT_ReadAndDeleteByPrimaryKey(t *testing.T) {
	a, _, name, ctx := itOutbox(t)
	const txlog = int32(txlogKind)
	itInsert(t, ctx, a, 3, txlog, 12, 7, 102)
	itInsert(t, ctx, a, 3, txlog, 10, 8, 100)
	itInsert(t, ctx, a, 3, txlog, 10, 7, 101)
	itInsert(t, ctx, a, 4, txlog, 10, 7, 200)
	itInsert(t, ctx, a, 3, int32(snapshotKind), 10, 7, 300)
	store := NewSQLStore(a, name)
	if createdMs, found, err := store.Oldest(ctx, 3, txlogKind, 0); err != nil || !found || createdMs != 10 {
		t.Fatalf("Oldest = %d, %v, %v", createdMs, found, err)
	}
	if _, found, err := store.Oldest(ctx, 3, txlogKind, 13); err != nil || found {
		t.Fatalf("nothing at or after 13: found=%v err=%v", found, err)
	}
	lock, held, err := store.LockUnit(ctx, 3)
	if err != nil || !held {
		t.Fatalf("held=%v err=%v", held, err)
	}
	defer lock.Release()
	rows, err := lock.Read(ctx, txlogKind, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uint64
	for _, row := range rows {
		ids = append(ids, row.RecordID)
	}
	if !slices.Equal(ids, []uint64{101, 100, 102}) {
		t.Fatalf("read order = %v, want primary key order (created_ms, player_id, record_id)", ids)
	}
	if string(rows[0].Payload) != "payload-101" {
		t.Fatalf("payload = %q", rows[0].Payload)
	}
	if err := lock.Delete(ctx, txlogKind, rows[:2]); err != nil {
		t.Fatal(err)
	}
	// 重删同一批(另一个副本已经删过的情形)不算错。
	if err := lock.Delete(ctx, txlogKind, rows[:2]); err != nil {
		t.Fatal(err)
	}
	left, err := lock.Read(ctx, txlogKind, 0, 500)
	if err != nil || len(left) != 1 || left[0].RecordID != 102 {
		t.Fatalf("left = %+v, %v", left, err)
	}
	var others int
	if err := a.QueryRowContext(ctx, "SELECT COUNT(*) FROM player_audit_outbox WHERE NOT (shard=3 AND kind=?)", txlog).Scan(&others); err != nil || others != 2 {
		t.Fatalf("rows of other shards and kinds = %d, %v; want 2 untouched", others, err)
	}
}
