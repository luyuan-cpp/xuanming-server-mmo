package auditoutbox

// 经写者闸门连接池的真库用例(设计 §7.1、§7.3):生产里转发循环拿的是存档库的写者闸门池(authoritysql),
// 命名锁、读、按主键删都要在这种池上成立,闸门冻结(搬库切换窗口)时读删都回可重试的 ErrWriterFrozen。
//
// 需要本任务独占的真实库和权限资格原件(MMORPG_SQL_FIXTURE_ADMIN_DSN 与其余 MMORPG_SQL_FIXTURE_* 变量,
// 跑法同 go/shared/authoritysql 的闸门真库用例),没有就跳过;GODB_IT_REQUIRE=1 时缺了一律 Fatal。MySQL 与 TiDB 都要跑一轮。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/luyuancpp/proto2mysql"

	dbpb "proto/common/database"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
)

func randomID(t *testing.T, n int) []byte {
	t.Helper()
	id := make([]byte, n)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return id
}

// gateFixture 建一个随机逻辑库(带一份写者闸门)和发件箱表,给本进程的写者授 DML。
func gateFixture(t *testing.T) (context.Context, *sqltest.Fixture, string) {
	t.Helper()
	dsn := os.Getenv("MMORPG_SQL_FIXTURE_ADMIN_DSN")
	if dsn == "" {
		if os.Getenv("GODB_IT_REQUIRE") == "1" {
			t.Fatal("GODB_IT_REQUIRE=1 but MMORPG_SQL_FIXTURE_ADMIN_DSN is empty")
		}
		t.Skip("needs a dedicated real database and writer qualification artifacts (MMORPG_SQL_FIXTURE_ADMIN_DSN)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	schema := "audit_outbox_gate_" + hex.EncodeToString(randomID(t, 8))
	opts, err := sqltest.OptionsFromEnv(dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	opts.AllowMySQL = true
	fixture, err := sqltest.New(ctx, t, opts)
	if err != nil {
		t.Fatal(err)
	}
	prepare, err := fixture.OpenSchema(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	model := proto2mysql.NewDB()
	outbox := &dbpb.PlayerAuditOutbox{}
	model.RegisterTable(outbox)
	if _, err := prepare.ExecContext(ctx, model.GetCreateTableSQL(outbox)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.GrantTables(ctx, schema, "player_audit_outbox"); err != nil {
		t.Fatal(err)
	}
	return ctx, fixture, schema
}

// 两个写者闸门池(两个副本)抢同一片:同一时刻只有一个拿得到;放锁之后另一个才拿得到。读、删在闸门池上照常。
func TestGate_UnitLockAndForwardingThroughWriterPools(t *testing.T) {
	ctx, fixture, schema := gateFixture(t)
	a, err := fixture.Open(ctx, schema, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fixture.Open(ctx, schema, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ExecContext(ctx, "INSERT INTO player_audit_outbox (shard,kind,created_ms,player_id,record_id,save_seq,payload) VALUES (3,1,10,7,100,1,?),(3,1,11,7,101,1,?)",
		[]byte("a"), []byte("b")); err != nil {
		t.Fatal(err)
	}
	storeA, storeB := NewSQLStore(a, schema), NewSQLStore(b, schema)
	if createdMs, found, err := storeA.Oldest(ctx, 3, txlogKind, 0); err != nil || !found || createdMs != 10 {
		t.Fatalf("Oldest through the writer pool = %d, %v, %v", createdMs, found, err)
	}
	lockA, held, err := storeA.LockUnit(ctx, 3)
	if err != nil || !held {
		t.Fatalf("A: held=%v err=%v", held, err)
	}
	if _, held, err := storeB.LockUnit(ctx, 3); err != nil || held {
		t.Fatalf("B must not get shard 3 while A holds it: held=%v err=%v", held, err)
	}
	rows, err := lockA.Read(ctx, txlogKind, 0, 500)
	if err != nil || len(rows) != 2 {
		t.Fatalf("Read through the writer pool = %+v, %v", rows, err)
	}
	if err := lockA.Delete(ctx, txlogKind, rows[:1]); err != nil {
		t.Fatalf("Delete through the writer gate: %v", err)
	}
	lockA.Release()
	lockB, held, err := storeB.LockUnit(ctx, 3)
	if err != nil || !held {
		t.Fatalf("B must get shard 3 after A released it: held=%v err=%v", held, err)
	}
	left, err := lockB.Read(ctx, txlogKind, 0, 500)
	lockB.Release()
	if err != nil || len(left) != 1 || left[0].RecordID != 101 {
		t.Fatalf("left = %+v, %v", left, err)
	}
}

// 闸门冻结(搬库切换窗口):删回可重试的 ErrWriterFrozen,行留着;放锁那条读也被闸门挡回,连接被丢掉,服务端随会话结束放锁;
// 解冻之后别的副本拿得到锁、删得掉。
func TestGate_FrozenGateIsRetryableAndFreesTheLock(t *testing.T) {
	ctx, fixture, schema := gateFixture(t)
	catalog, err := fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	a, err := fixture.Open(ctx, schema, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fixture.Open(ctx, schema, 2)
	if err != nil {
		t.Fatal(err)
	}
	// 先写一笔:这个 boot 过过闸门之后,冻结时事务外的读也回 ErrWriterFrozen(启动阶段的放行不再适用)。
	if _, err := a.ExecContext(ctx, "INSERT INTO player_audit_outbox (shard,kind,created_ms,player_id,record_id,save_seq,payload) VALUES (5,1,10,7,200,1,?)",
		[]byte("a")); err != nil {
		t.Fatal(err)
	}
	lockA, held, err := NewSQLStore(a, schema).LockUnit(ctx, 5)
	if err != nil || !held {
		t.Fatalf("A: held=%v err=%v", held, err)
	}
	rows, err := lockA.Read(ctx, txlogKind, 0, 500)
	if err != nil || len(rows) != 1 {
		t.Fatalf("Read = %+v, %v", rows, err)
	}
	runID := randomID(t, 16)
	if _, err := catalog.FreezeWriters(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err := lockA.Delete(ctx, txlogKind, rows); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("Delete on a frozen gate = %v, want ErrWriterFrozen", err)
	}
	lockA.Release() // RELEASE_LOCK 被闸门挡回:丢连接,锁随会话结束释放
	if _, err := catalog.ReleaseWriters(ctx, runID); err != nil {
		t.Fatal(err)
	}
	storeB := NewSQLStore(b, schema)
	deadline := time.Now().Add(10 * time.Second)
	for {
		lockB, held, err := storeB.LockUnit(ctx, 5)
		if err != nil {
			t.Fatal(err)
		}
		if held {
			left, err := lockB.Read(ctx, txlogKind, 0, 500)
			if err == nil && len(left) == 1 {
				err = lockB.Delete(ctx, txlogKind, left)
			}
			lockB.Release()
			if err != nil || len(left) != 1 {
				t.Fatalf("after unfreeze: left=%+v err=%v", left, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lock of the dropped session was not freed within 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, found, err := storeB.Oldest(ctx, 5, txlogKind, 0); err != nil || found {
		t.Fatalf("outbox must be empty after the forward: found=%v err=%v", found, err)
	}
}
