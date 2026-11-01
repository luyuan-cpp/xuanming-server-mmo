package rolefence

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	rfpb "proto/role_fence"
	"shared/authoritysql/sqltest"
)

// fenceFixture 在本例独占的随机库里建 role_fence 和一张代表业务挂起的表,回写者池。
func fenceFixture(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("MMORPG_SQL_FIXTURE_ADMIN_DSN")
	if dsn == "" {
		t.Skip("需本任务独占数据库及真实权限资格原件")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "rolefence_it_" + hex.EncodeToString(random[:])
	opts, err := sqltest.OptionsFromEnv(dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	opts.AllowMySQL = true
	fixture, err := sqltest.New(ctx, t, opts)
	if err != nil {
		t.Fatal(err)
	}
	install, err := fixture.OpenSchema(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = EnsureSchema(ctx, install); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = install.ExecContext(ctx, "CREATE TABLE fence_it_pending (player_id BIGINT UNSIGNED NOT NULL,PRIMARY KEY(player_id))"); err != nil {
		t.Fatal(err)
	}
	if err = fixture.GrantTables(ctx, schema, TableName, "fence_it_pending"); err != nil {
		t.Fatal(err)
	}
	db, err := fixture.Open(ctx, schema, 8)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, db
}

func readFence(t *testing.T, ctx context.Context, db *sql.DB, player uint64) Fence {
	t.Helper()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	fence, err := Lock(ctx, tx, player)
	if err != nil {
		t.Fatal(err)
	}
	return fence
}

func TestTiDBApplyMergesByVersionAndLockReadsItBack(t *testing.T) {
	ctx, db := fenceFixture(t)
	if err := VerifySchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	token := RemovalToken(11)
	frozen := &rfpb.RoleFenceState{PlayerId: 11, Account: "原账号", OwnerRevision: 5, FreezeToken: token, Version: 7, HomeZone: 3}
	if applied, err := Apply(ctx, db, frozen); err != nil || !applied {
		t.Fatalf("insert: applied %v err %v", applied, err)
	}
	want := Fence{PlayerID: 11, Account: "原账号", OwnerRevision: 5, FreezeToken: token, Version: 7, HomeZone: 3}
	if got := readFence(t, ctx, db, 11); got != want {
		t.Fatalf("got %+v", got)
	}
	for _, version := range []uint64{7, 6} {
		older := &rfpb.RoleFenceState{PlayerId: 11, Account: "别人", OwnerRevision: 4, Version: version, HomeZone: 9}
		if applied, err := Apply(ctx, db, older); err != nil || applied {
			t.Fatalf("version %d: applied %v err %v", version, applied, err)
		}
		if got := readFence(t, ctx, db, 11); got != want {
			t.Fatalf("version %d changed the replica: %+v", version, got)
		}
	}
	// 一条语句里所有列都要跟着 version 一起改:version 不排最后的话,其余列读到新 version 就不更新了。
	sold := &rfpb.RoleFenceState{PlayerId: 11, Account: "买家", OwnerRevision: 6, Version: 8, HomeZone: 4}
	if applied, err := Apply(ctx, db, sold); err != nil || !applied {
		t.Fatalf("newer: applied %v err %v", applied, err)
	}
	want = Fence{PlayerID: 11, Account: "买家", OwnerRevision: 6, Version: 8, HomeZone: 4}
	if got := readFence(t, ctx, db, 11); got != want {
		t.Fatalf("got %+v", got)
	}
	tombstone := &rfpb.RoleFenceState{PlayerId: 11, OwnerRevision: 7, Version: 9, HomeZone: 4}
	if applied, err := Apply(ctx, db, tombstone); err != nil || !applied {
		t.Fatalf("tombstone: applied %v err %v", applied, err)
	}
	if got := readFence(t, ctx, db, 11); got.Account != "" || got.Version != 9 {
		t.Fatalf("got %+v", got)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if fence, err := Lock(ctx, tx, 12); !errors.Is(err, ErrMissing) || fence.PlayerID != 12 {
		t.Fatalf("fence %+v err %v", fence, err)
	}
}

// 准备令牌转最终冻结:令牌不变,只有 freeze_final 和版本变,真库上这一列要跟着版本走、读得回来。
func TestTiDBApplyCarriesFreezeFinal(t *testing.T) {
	ctx, db := fenceFixture(t)
	preparing := &rfpb.RoleFenceState{PlayerId: 21, Account: "seller", OwnerRevision: 1, FreezeToken: 77, Version: 2, HomeZone: 1}
	if _, err := Apply(ctx, db, preparing); err != nil {
		t.Fatal(err)
	}
	if got := readFence(t, ctx, db, 21); got.FreezeFinal || got.FreezeToken != 77 {
		t.Fatalf("got %+v", got)
	}
	final := &rfpb.RoleFenceState{PlayerId: 21, Account: "seller", OwnerRevision: 1, FreezeToken: 77, FreezeFinal: true, Version: 3, HomeZone: 1}
	if applied, err := Apply(ctx, db, final); err != nil || !applied {
		t.Fatalf("applied %v err %v", applied, err)
	}
	if got := readFence(t, ctx, db, 21); !got.FreezeFinal || got.Version != 3 {
		t.Fatalf("got %+v", got)
	}
	if applied, err := Apply(ctx, db, preparing); err != nil || applied {
		t.Fatalf("applied %v err %v", applied, err)
	}
	if got := readFence(t, ctx, db, 21); !got.FreezeFinal {
		t.Fatalf("迟到的准备状态不能把最终冻结改回去: %+v", got)
	}
}

// 规格 §3.5 的串行点:业务写入和冻结只能有一方先赢。票先赢则冻结读到挂起并回滚,冻结先赢则业务在 Lock 上被拒。
func TestTiDBPrepareAndBusinessWriteAreMutuallyExclusive(t *testing.T) {
	ctx, db := fenceFixture(t)
	s, err := NewParticipantServer(db, func(ctx context.Context, tx *sql.Tx, state *rfpb.RoleFenceState) (string, error) {
		var player uint64
		err := tx.QueryRowContext(ctx, "SELECT player_id FROM fence_it_pending WHERE player_id=? FOR UPDATE", state.PlayerId).Scan(&player)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return "pending", nil
	}, testCoordinatorSecret)
	if err != nil {
		t.Fatal(err)
	}
	for player := uint64(100); player < 120; player++ {
		if _, err = Apply(ctx, db, &rfpb.RoleFenceState{PlayerId: player, Account: "seller", OwnerRevision: 1, Version: 1, HomeZone: 1}); err != nil {
			t.Fatal(err)
		}
		prepare := signedPrepare(t, &rfpb.RoleFenceState{PlayerId: player, Account: "seller", OwnerRevision: 1, FreezeToken: 1000 + player, Version: 2, HomeZone: 1})
		start := make(chan struct{})
		var wg sync.WaitGroup
		var clean bool
		var prepareErr, businessErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			out, err := s.PrepareRoleFreeze(ctx, prepare)
			clean, prepareErr = out.GetClean(), err
		}()
		go func() {
			defer wg.Done()
			<-start
			businessErr = func() error {
				tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				if _, err = LockAll(ctx, tx, Expect{PlayerID: player, Account: "seller", OwnerRevision: 1, Reject: errDenied}); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, "INSERT INTO fence_it_pending(player_id) VALUES (?)", player); err != nil {
					return err
				}
				return tx.Commit()
			}()
		}()
		close(start)
		wg.Wait()
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		var stale *StaleError
		if businessErr != nil && !errors.As(businessErr, &stale) {
			t.Fatal(businessErr)
		}
		var pending int
		if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM fence_it_pending WHERE player_id=?", player).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if clean == (pending == 1) {
			t.Fatalf("player %d: clean=%v pending=%d business=%v", player, clean, pending, businessErr)
		}
		if got := readFence(t, ctx, db, player); clean != (got.FreezeToken != 0) {
			t.Fatalf("player %d: clean=%v replica %+v", player, clean, got)
		}
	}
}
