package assetop

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	assetpb "proto/common/asset"
)

// seq 分配的集成测试:锁序、加锁读、守卫上限这些东西只有在**真的数据库**上才算验过。
//
// 需要环境变量 ASSETOP_TEST_MYSQL_DSN(例:user:pw@tcp(127.0.0.1:3306)/assetop_it),
// 没设就整体 SKIP —— 账号不写进任何文件(AGENTS §11.3)。
// 用例名统一带 Integration,便于 `go test ./assetop -run Integration`。
//
// 连接经 go-zero 的 sqlx 拿 *sql.DB:shared 不直接引 MySQL 驱动,驱动由 sqlx 带进来,
// 这样 shared 的 go.mod 里不会多出一条**直接**依赖。

const (
	itSeqTable = "assetop_it_seq"
	itOpTable  = "assetop_it_op"

	itPendingStatus uint32 = 0
	itDoneStatus    uint32 = 1
)

const itCreateSeqTable = `
CREATE TABLE ` + itSeqTable + ` (
  player_id  BIGINT UNSIGNED NOT NULL,
  stream     INT UNSIGNED NOT NULL,
  next_seq   BIGINT UNSIGNED NOT NULL,
  epoch      BIGINT UNSIGNED NOT NULL DEFAULT 0,
  updated_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (player_id, stream)
)`

const itCreateOpTable = `
CREATE TABLE ` + itOpTable + ` (
  op_id        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  player_id    BIGINT UNSIGNED NOT NULL,
  stream       INT UNSIGNED NOT NULL,
  stream_epoch BIGINT UNSIGNED NOT NULL,
  seq          BIGINT UNSIGNED NOT NULL,
  status       INT UNSIGNED NOT NULL,
  PRIMARY KEY (op_id),
  UNIQUE KEY uk_player_stream_epoch_seq (player_id, stream, stream_epoch, seq),
  KEY idx_pending (player_id, stream, stream_epoch, status, seq)
)`

// openIntegrationDB 建库连接并重建两张测试专用表。测试结束时丢掉它们。
func openIntegrationDB(t *testing.T) (*sql.DB, SeqTables) {
	t.Helper()

	dsn := os.Getenv("ASSETOP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设 ASSETOP_TEST_MYSQL_DSN,跳过 seq 集成测试")
	}
	db, err := sqlx.NewMysql(dsn).RawDB()
	if err != nil {
		t.Fatalf("连数据库失败: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("数据库不可用: %v", err)
	}

	for _, stmt := range []string{
		"DROP TABLE IF EXISTS " + itOpTable,
		"DROP TABLE IF EXISTS " + itSeqTable,
		itCreateSeqTable,
		itCreateOpTable,
	} {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("准备测试表失败(%s): %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec("DROP TABLE IF EXISTS " + itOpTable)
		_, _ = db.Exec("DROP TABLE IF EXISTS " + itSeqTable)
	})

	tables, err := NewSeqTables(itSeqTable, itOpTable, itPendingStatus)
	if err != nil {
		t.Fatalf("表名校验失败: %v", err)
	}
	return db, tables
}

// insertOp 模拟业务仓库在同一事务里插入 outbox 行。
func insertOp(ctx context.Context, tx *sql.Tx, playerID uint64, stream assetpb.AssetOpStream, epoch, seq uint64, status uint32) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO "+itOpTable+" (player_id, stream, stream_epoch, seq, status) VALUES (?, ?, ?, ?, ?)",
		playerID, uint32(stream), epoch, seq, status)
	return err
}

func TestIntegrationEnsureSeqRowWritesEpoch(t *testing.T) {
	db, tables := openIntegrationDB(t)
	ctx := context.Background()
	const playerID = 9001

	if err := EnsureSeqRow(ctx, db, tables, playerID, testStream, 1700000000000); err != nil {
		t.Fatalf("建 seq 行失败: %v", err)
	}
	// 再建一次:纪元不得被改写,否则老 seq 会在 scene 那边变成"另一本簿子"。
	if err := EnsureSeqRow(ctx, db, tables, playerID, testStream, 1800000000000); err != nil {
		t.Fatalf("二次 Ensure 失败: %v", err)
	}

	var epoch, nextSeq uint64
	row := db.QueryRowContext(ctx,
		"SELECT epoch, next_seq FROM "+itSeqTable+" WHERE player_id = ? AND stream = ?", playerID, uint32(testStream))
	if err := row.Scan(&epoch, &nextSeq); err != nil {
		t.Fatalf("读 seq 行失败: %v", err)
	}
	if epoch != 1700000000000 {
		t.Fatalf("纪元应保持首次写入的值,实际 %d", epoch)
	}
	if nextSeq != 1 {
		t.Fatalf("next_seq 应为 1,实际 %d", nextSeq)
	}

	// 没有 seq 行时分配必须明确失败,而不是凭空造一个。
	err := WithTxRetry(ctx, db, 1, nil, func(tx *sql.Tx) error {
		_, err := AllocateSeq(ctx, tx, tables, 9999, testStream, DefaultLimits, 1700000000001)
		return err
	})
	if !errors.Is(err, ErrSeqRowMissing) {
		t.Fatalf("应报 ErrSeqRowMissing,实际: %v", err)
	}
}

func TestIntegrationAllocateSeqIsGapless(t *testing.T) {
	db, tables := openIntegrationDB(t)
	ctx := context.Background()
	const (
		playerID = 9002
		epoch    = uint64(1700000000000)
		workers  = 20
	)
	if err := EnsureSeqRow(ctx, db, tables, playerID, testStream, epoch); err != nil {
		t.Fatalf("建 seq 行失败: %v", err)
	}

	var (
		mu   sync.Mutex
		got  []uint64
		errs []error
		wg   sync.WaitGroup
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := WithTxRetry(ctx, db, 3, alwaysRetryable, func(tx *sql.Tx) error {
				alloc, err := AllocateSeq(ctx, tx, tables, playerID, testStream, DefaultLimits, epoch)
				if err != nil {
					return err
				}
				// 立刻终结:否则未决行会撞上 MaxPending=16 的守卫。
				if err := insertOp(ctx, tx, playerID, testStream, alloc.Epoch, alloc.Seq, itDoneStatus); err != nil {
					return err
				}
				mu.Lock()
				got = append(got, alloc.Seq)
				mu.Unlock()
				return nil
			})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("并发分配出错: %v", errs)
	}
	seen := map[uint64]bool{}
	for _, seq := range got {
		if seq == 0 || seq > workers {
			t.Fatalf("分配出界外的 seq %d", seq)
		}
		if seen[seq] {
			t.Fatalf("seq %d 被分配了两次", seq)
		}
		seen[seq] = true
	}
	if len(seen) != workers {
		t.Fatalf("应当拿到 1..%d 共 %d 个号,实际 %d 个", workers, workers, len(seen))
	}
}

func TestIntegrationAllocateSeqGuards(t *testing.T) {
	db, tables := openIntegrationDB(t)
	ctx := context.Background()
	const (
		playerID = 9003
		epoch    = uint64(1700000000000)
	)
	if err := EnsureSeqRow(ctx, db, tables, playerID, testStream, epoch); err != nil {
		t.Fatalf("建 seq 行失败: %v", err)
	}

	// 连续留下 16 行未决 → 第 17 次必须被守卫拒绝。
	for i := 0; i < int(DefaultLimits.MaxPending); i++ {
		if err := WithTxRetry(ctx, db, 1, nil, func(tx *sql.Tx) error {
			alloc, err := AllocateSeq(ctx, tx, tables, playerID, testStream, DefaultLimits, epoch)
			if err != nil {
				return err
			}
			return insertOp(ctx, tx, playerID, testStream, alloc.Epoch, alloc.Seq, itPendingStatus)
		}); err != nil {
			t.Fatalf("第 %d 次分配失败: %v", i+1, err)
		}
	}
	err := WithTxRetry(ctx, db, 1, nil, func(tx *sql.Tx) error {
		_, err := AllocateSeq(ctx, tx, tables, playerID, testStream, DefaultLimits, epoch)
		return err
	})
	if !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("第 17 次应被守卫拒绝,实际: %v", err)
	}

	// 只留 seq 1 未决、其余终结,再把 next_seq 推到 601 → 跨度守卫生效。
	if _, err := db.ExecContext(ctx,
		"UPDATE "+itOpTable+" SET status = ? WHERE player_id = ? AND seq > 1", itDoneStatus, playerID); err != nil {
		t.Fatalf("清未决行失败: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE "+itSeqTable+" SET next_seq = 601 WHERE player_id = ? AND stream = ?", playerID, uint32(testStream)); err != nil {
		t.Fatalf("推 next_seq 失败: %v", err)
	}
	err = WithTxRetry(ctx, db, 1, nil, func(tx *sql.Tx) error {
		_, err := AllocateSeq(ctx, tx, tables, playerID, testStream, DefaultLimits, epoch)
		return err
	})
	if !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("跨度 600 应被守卫拒绝,实际: %v", err)
	}
}

// 旧纪元的未决行不该挡住新纪元的操作(规格 §4.29:I5 按纪元计)。
func TestIntegrationAllocateSeqCountsOnlyCurrentEpoch(t *testing.T) {
	db, tables := openIntegrationDB(t)
	ctx := context.Background()
	const (
		playerID = 9004
		oldEpoch = uint64(1600000000000)
		newEpoch = uint64(1700000000000)
	)
	if err := EnsureSeqRow(ctx, db, tables, playerID, testStream, newEpoch); err != nil {
		t.Fatalf("建 seq 行失败: %v", err)
	}
	// 旧纪元塞满未决行(库恢复之后就是这个形状)。
	for seq := uint64(1); seq <= uint64(DefaultLimits.MaxPending)+4; seq++ {
		if err := WithTxRetry(ctx, db, 1, nil, func(tx *sql.Tx) error {
			return insertOp(ctx, tx, playerID, testStream, oldEpoch, seq, itPendingStatus)
		}); err != nil {
			t.Fatalf("插旧纪元行失败: %v", err)
		}
	}

	var alloc Alloc
	if err := WithTxRetry(ctx, db, 1, nil, func(tx *sql.Tx) error {
		var err error
		alloc, err = AllocateSeq(ctx, tx, tables, playerID, testStream, DefaultLimits, newEpoch)
		return err
	}); err != nil {
		t.Fatalf("新纪元分配不该被旧纪元的积压挡住: %v", err)
	}
	if alloc.Epoch != newEpoch || alloc.Seq != 1 {
		t.Fatalf("应当拿到新纪元的 seq 1,实际 %+v", alloc)
	}
}

// 注入的错误分类说「可重试」时,整个业务事务重跑一遍。
func TestIntegrationWithTxRetryRetriesOnce(t *testing.T) {
	db, tables := openIntegrationDB(t)
	ctx := context.Background()
	const (
		playerID = 9005
		epoch    = uint64(1700000000000)
	)
	if err := EnsureSeqRow(ctx, db, tables, playerID, testStream, epoch); err != nil {
		t.Fatalf("建 seq 行失败: %v", err)
	}

	injected := errors.New("模拟死锁")
	attempts := 0
	err := WithTxRetry(ctx, db, 2, func(err error) bool { return errors.Is(err, injected) }, func(tx *sql.Tx) error {
		attempts++
		alloc, err := AllocateSeq(ctx, tx, tables, playerID, testStream, DefaultLimits, epoch)
		if err != nil {
			return err
		}
		if attempts == 1 {
			return fmt.Errorf("第一轮失败: %w", injected)
		}
		return insertOp(ctx, tx, playerID, testStream, alloc.Epoch, alloc.Seq, itDoneStatus)
	})
	if err != nil {
		t.Fatalf("第二轮应当成功: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("应当跑两轮,实际 %d", attempts)
	}

	// 第一轮回滚掉了,所以 next_seq 只前进一次:回滚不彻底会在这里露馅。
	var nextSeq uint64
	if err := db.QueryRowContext(ctx,
		"SELECT next_seq FROM "+itSeqTable+" WHERE player_id = ? AND stream = ?", playerID, uint32(testStream)).
		Scan(&nextSeq); err != nil {
		t.Fatalf("读 next_seq 失败: %v", err)
	}
	if nextSeq != 2 {
		t.Fatalf("回滚后 next_seq 应为 2,实际 %d", nextSeq)
	}

	// 不可重试的错误一次就返回。
	attempts = 0
	other := errors.New("语法错")
	err = WithTxRetry(ctx, db, 3, func(err error) bool { return errors.Is(err, injected) }, func(*sql.Tx) error {
		attempts++
		return other
	})
	if !errors.Is(err, other) || attempts != 1 {
		t.Fatalf("不可重试的错误应当只跑一轮,实际 attempts=%d err=%v", attempts, err)
	}
	_ = tables
}

// alwaysRetryable 只用于并发用例:真实服务注入的是按 MySQL 错误号分类的实现。
func alwaysRetryable(error) bool { return true }
