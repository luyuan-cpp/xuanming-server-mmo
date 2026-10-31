//go:build integration

package playerstore

// 真库用例(规格 §8)。默认不跑:
//
//	GODB_MYSQL_ITEST_DSN='root:pwd@tcp(127.0.0.1:3306)/' go test -tags=integration ./internal/playerstore/...
//
// 账号要能建库。每个用例建一个一次性库,结束后删掉。DSN 指向 TiDB 就是 TiDB 上的验证。
// GODB_IT_REQUIRE=1 时依赖缺失一律 Fatal,不 Skip。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	storepb "proto/player_store"
)

const itDDL = "CREATE TABLE player_store (" +
	"player_id BIGINT UNSIGNED NOT NULL," +
	"kind INT UNSIGNED NOT NULL," +
	"owner_epoch BIGINT UNSIGNED NOT NULL," +
	"save_seq BIGINT UNSIGNED NOT NULL," +
	"schema_gen INT UNSIGNED NOT NULL," +
	"holder_instance VARBINARY(64) NOT NULL," +
	"released INT UNSIGNED NOT NULL," +
	"payload MEDIUMBLOB NOT NULL," +
	"PRIMARY KEY (player_id, kind))"

var itSeq atomic.Int64

func itStore(t *testing.T) (*Store, *sql.DB, context.Context) {
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
	name := fmt.Sprintf("playerstore_it_%d_%d", os.Getpid(), itSeq.Add(1))
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name) })
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, itDDL); err != nil {
		t.Fatal(err)
	}
	store, err := New(db, testKnownGen, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.VerifySchema(ctx); err != nil {
		t.Fatal(err)
	}
	return store, db, ctx
}

// seedOwner 模拟建角:写权行由建角事务写好,Save 和认领都不建它。
func seedOwner(t *testing.T, ctx context.Context, db *sql.DB, player uint64) {
	t.Helper()
	_, err := db.ExecContext(ctx, "INSERT INTO player_store (player_id,kind,owner_epoch,save_seq,schema_gen,holder_instance,released,payload) VALUES (?,0,1,0,1,'',0,'')", player)
	if err != nil {
		t.Fatal(err)
	}
}

func claim(t *testing.T, ctx context.Context, store *Store, player, epoch uint64, instance []byte) *storepb.LoadAndClaimResponse {
	t.Helper()
	resp, err := store.LoadAndClaim(ctx, &storepb.LoadAndClaimRequest{PlayerId: player, OwnerEpoch: epoch, InstanceId: instance, BinarySchemaGen: testKnownGen})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func save(t *testing.T, ctx context.Context, store *Store, player, epoch, seq uint64, instance []byte, release bool, rows ...*storepb.PlayerStoreRow) storepb.SavePlayerResult {
	t.Helper()
	resp, err := store.Save(ctx, &storepb.SavePlayerRequest{PlayerId: player, OwnerEpoch: epoch, SaveSeq: seq, SchemaGen: 1, InstanceId: instance, Rows: rows, Release: release})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetResult()
}

func hot(payload string) *storepb.PlayerStoreRow {
	return &storepb.PlayerStoreRow{Kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT, Payload: []byte(payload)}
}

func asset(payload string) *storepb.PlayerStoreRow {
	return &storepb.PlayerStoreRow{Kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_ASSET, Payload: []byte(payload)}
}

func wantSave(t *testing.T, got, want storepb.SavePlayerResult) {
	t.Helper()
	if got != want {
		t.Fatalf("save = %v, want %v", got, want)
	}
}

func wantClaim(t *testing.T, got *storepb.LoadAndClaimResponse, want storepb.LoadAndClaimResult) {
	t.Helper()
	if got.GetResult() != want {
		t.Fatalf("claim = %v, want %v", got.GetResult(), want)
	}
}

func payloadOf(rows []*storepb.PlayerStoreRow, kind storepb.PlayerStoreKind) string {
	for _, row := range rows {
		if row.GetKind() == kind {
			return string(row.GetPayload())
		}
	}
	return "<missing>"
}

func TestIT_ClaimSaveAndReadBack(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	first := claim(t, ctx, store, 7, 100, instanceA)
	wantClaim(t, first, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED)
	if len(first.GetRows()) != 0 {
		t.Fatalf("a new role has no data rows: %v", first.GetRows())
	}
	wantSave(t, save(t, ctx, store, 7, 100, 1, instanceA, false, hot("h1"), asset("a1")), storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED)
	// 只写有变化的行,没写的行保持原样。
	wantSave(t, save(t, ctx, store, 7, 100, 2, instanceA, false, hot("h2")), storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED)
	again := claim(t, ctx, store, 7, 100, instanceA)
	wantClaim(t, again, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED)
	if payloadOf(again.GetRows(), storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT) != "h2" ||
		payloadOf(again.GetRows(), storepb.PlayerStoreKind_PLAYER_STORE_KIND_ASSET) != "a1" {
		t.Fatalf("rows = %v", again.GetRows())
	}
	if again.GetSaveSeq() != 2 {
		t.Fatalf("save_seq = %d", again.GetSaveSeq())
	}
}

// 认领之后,旧持有者的写被拒;新持有者读到的是旧持有者最后提交的状态。
func TestIT_OldHolderRejectedAfterClaim(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	claim(t, ctx, store, 7, 100, instanceA)
	wantSave(t, save(t, ctx, store, 7, 100, 1, instanceA, false, hot("from-a")), storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED)
	taken := claim(t, ctx, store, 7, 200, instanceB)
	wantClaim(t, taken, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED)
	if payloadOf(taken.GetRows(), storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT) != "from-a" {
		t.Fatalf("rows = %v", taken.GetRows())
	}
	wantSave(t, save(t, ctx, store, 7, 100, 2, instanceA, false, hot("late")), storepb.SavePlayerResult_SAVE_PLAYER_STALE_OWNER)
	// 旧持有者拿到更高的代次也不行:它没认领过。
	wantSave(t, save(t, ctx, store, 7, 300, 2, instanceA, false, hot("late")), storepb.SavePlayerResult_SAVE_PLAYER_UNCLAIMED)
	wantSave(t, save(t, ctx, store, 7, 200, 2, instanceA, false, hot("late")), storepb.SavePlayerResult_SAVE_PLAYER_NOT_HOLDER)
	final := claim(t, ctx, store, 7, 200, instanceB)
	if payloadOf(final.GetRows(), storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT) != "from-a" {
		t.Fatalf("rejected writes must not land: %v", final.GetRows())
	}
}

// 结果未知后重试:同序号不写两次;旧请求晚到不覆盖新内容。
func TestIT_RetryAndLateRequest(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	claim(t, ctx, store, 7, 100, instanceA)
	wantSave(t, save(t, ctx, store, 7, 100, 5, instanceA, false, hot("v5")), storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED)
	wantSave(t, save(t, ctx, store, 7, 100, 5, instanceA, false, hot("v5")), storepb.SavePlayerResult_SAVE_PLAYER_EQUAL)
	wantSave(t, save(t, ctx, store, 7, 100, 6, instanceA, false, hot("v6")), storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED)
	wantSave(t, save(t, ctx, store, 7, 100, 5, instanceA, false, hot("v5")), storepb.SavePlayerResult_SAVE_PLAYER_OLDER_SEQ)
	final := claim(t, ctx, store, 7, 100, instanceA)
	if payloadOf(final.GetRows(), storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT) != "v6" {
		t.Fatalf("rows = %v", final.GetRows())
	}
}

// 代次更高且持有者是自己:放行并抬代次,之后低代次的写被拒。
func TestIT_HigherEpochSameHolderRaisesEpoch(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	claim(t, ctx, store, 7, 100, instanceA)
	wantSave(t, save(t, ctx, store, 7, 150, 1, instanceA, false, hot("h")), storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED)
	wantSave(t, save(t, ctx, store, 7, 100, 2, instanceA, false, hot("old")), storepb.SavePlayerResult_SAVE_PLAYER_STALE_OWNER)
	wantClaim(t, claim(t, ctx, store, 7, 149, instanceB), storepb.LoadAndClaimResult_LOAD_AND_CLAIM_STALE_OWNER)
}

// 释放不是终态:同实例可以继续写并清除 released;别的实例认领之后,原实例被拒。
func TestIT_ReleaseIsNotTerminal(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	claim(t, ctx, store, 7, 100, instanceA)
	released := func() int {
		var v int
		if err := db.QueryRowContext(ctx, "SELECT released FROM player_store WHERE player_id=7 AND kind=0").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	wantSave(t, save(t, ctx, store, 7, 100, 1, instanceA, true), storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED)
	if released() != 1 {
		t.Fatal("release must set released=1")
	}
	wantSave(t, save(t, ctx, store, 7, 100, 1, instanceA, true), storepb.SavePlayerResult_SAVE_PLAYER_EQUAL)
	wantSave(t, save(t, ctx, store, 7, 100, 2, instanceA, false, hot("back")), storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED)
	if released() != 0 {
		t.Fatal("a later save must clear released")
	}
	wantSave(t, save(t, ctx, store, 7, 100, 3, instanceA, true), storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED)
	wantClaim(t, claim(t, ctx, store, 7, 200, instanceB), storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED)
	if released() != 0 {
		t.Fatal("a claim must clear released")
	}
	wantSave(t, save(t, ctx, store, 7, 100, 4, instanceA, false, hot("late")), storepb.SavePlayerResult_SAVE_PLAYER_STALE_OWNER)
}

func TestIT_NoOwnerAndSchemaChecks(t *testing.T) {
	store, db, ctx := itStore(t)
	wantClaim(t, claim(t, ctx, store, 8, 100, instanceA), storepb.LoadAndClaimResult_LOAD_AND_CLAIM_NO_OWNER)
	wantSave(t, save(t, ctx, store, 8, 100, 1, instanceA, false, hot("h")), storepb.SavePlayerResult_SAVE_PLAYER_NO_OWNER)
	var rows int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM player_store").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("Save and claim must not create rows for a role that does not exist")
	}
	seedOwner(t, ctx, db, 7)
	claim(t, ctx, store, 7, 100, instanceA)
	resp, err := store.Save(ctx, &storepb.SavePlayerRequest{PlayerId: 7, OwnerEpoch: 100, SaveSeq: 1, SchemaGen: 3, InstanceId: instanceA})
	if err != nil || resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	resp, err = store.Save(ctx, &storepb.SavePlayerRequest{PlayerId: 7, OwnerEpoch: 100, SaveSeq: 2, SchemaGen: 2, InstanceId: instanceA})
	if err != nil || resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_SCHEMA_REGRESSED {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	old, err := store.LoadAndClaim(ctx, &storepb.LoadAndClaimRequest{PlayerId: 7, OwnerEpoch: 200, InstanceId: instanceB, BinarySchemaGen: 2})
	if err != nil || old.GetResult() != storepb.LoadAndClaimResult_LOAD_AND_CLAIM_SCHEMA_TOO_NEW {
		t.Fatalf("resp=%+v err=%v", old, err)
	}
	wantSave(t, save(t, ctx, store, 7, 100, 3, instanceA, false, hot("still-mine")), storepb.SavePlayerResult_SAVE_PLAYER_SCHEMA_REGRESSED)
}

// nil payload 能写进 NOT NULL 列,读回来是空。
func TestIT_EmptyPayload(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	claim(t, ctx, store, 7, 100, instanceA)
	row := &storepb.PlayerStoreRow{Kind: storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT}
	wantSave(t, save(t, ctx, store, 7, 100, 1, instanceA, false, row), storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED)
	final := claim(t, ctx, store, 7, 100, instanceA)
	if payloadOf(final.GetRows(), storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT) != "" {
		t.Fatalf("rows = %v", final.GetRows())
	}
}

// 两个实例同时认领同一个代次:只有一个成功。
func TestIT_ConcurrentClaimsSameEpoch(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	results := make([]storepb.LoadAndClaimResult, 2)
	var wg sync.WaitGroup
	for i, instance := range [][]byte{instanceA, instanceB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := store.LoadAndClaim(ctx, &storepb.LoadAndClaimRequest{PlayerId: 7, OwnerEpoch: 100, InstanceId: instance, BinarySchemaGen: testKnownGen})
			if err != nil {
				t.Error(err)
				return
			}
			results[i] = resp.GetResult()
		}()
	}
	wg.Wait()
	claimed := 0
	for _, result := range results {
		if result == storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("results = %v, want exactly one claim", results)
	}
}

// 存盘与认领交错:认领成功之后,旧持有者不可能再提交;新持有者读到的序号就是最终序号。
// 这是「不回档」的核心性质。
func TestIT_ConcurrentSavesAndTakeover(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	claim(t, ctx, store, 7, 100, instanceA)

	var lastCommitted atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for q := uint64(1); ; q++ {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := store.Save(ctx, &storepb.SavePlayerRequest{
				PlayerId: 7, OwnerEpoch: 100, SaveSeq: q, SchemaGen: 1, InstanceId: instanceA,
				Rows: []*storepb.PlayerStoreRow{hot(fmt.Sprintf("q%d", q))},
			})
			if err != nil {
				t.Error(err)
				return
			}
			if resp.GetResult() != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED {
				return // 被接管了
			}
			lastCommitted.Store(q)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	taken := claim(t, ctx, store, 7, 200, instanceB)
	wantClaim(t, taken, storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED)
	close(stop)
	<-done
	last := lastCommitted.Load()
	if taken.GetSaveSeq() != last {
		t.Fatalf("claim read seq %d, but the old holder's last commit was %d", taken.GetSaveSeq(), last)
	}
	if want := fmt.Sprintf("q%d", last); last > 0 && payloadOf(taken.GetRows(), storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT) != want {
		t.Fatalf("claim read %q, want %q", payloadOf(taken.GetRows(), storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT), want)
	}
}

// 一笔存盘的多行要么全提交,要么全不提交:用取消的 ctx 模拟事务中断,断言库里没有部分写入。
func TestIT_CanceledSaveLeavesNothing(t *testing.T) {
	store, db, ctx := itStore(t)
	seedOwner(t, ctx, db, 7)
	claim(t, ctx, store, 7, 100, instanceA)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Save(canceled, &storepb.SavePlayerRequest{PlayerId: 7, OwnerEpoch: 100, SaveSeq: 1, SchemaGen: 1, InstanceId: instanceA, Rows: []*storepb.PlayerStoreRow{hot("x"), asset("y")}}); err == nil {
		t.Fatal("a canceled save must report an error")
	}
	final := claim(t, ctx, store, 7, 100, instanceA)
	if len(final.GetRows()) != 0 || final.GetSaveSeq() != 0 {
		t.Fatalf("response = %+v", final)
	}
}

// 表不存在:VerifySchema 拒绝启动。
func TestIT_VerifySchemaRejectsMissingTable(t *testing.T) {
	store, db, ctx := itStore(t)
	if _, err := db.ExecContext(ctx, "DROP TABLE player_store"); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifySchema(ctx); err == nil {
		t.Fatal("missing table must be rejected")
	}
	if _, err := store.Save(ctx, &storepb.SavePlayerRequest{PlayerId: 7, OwnerEpoch: 100, SaveSeq: 1, SchemaGen: 1, InstanceId: instanceA}); err == nil {
		t.Fatal("a save on a missing table must report an error")
	}
}
