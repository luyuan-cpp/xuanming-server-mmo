package assetop

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// ── 2026-09-21 死锁审计的三条回归 ──

// itIsLockConflict 按错误文本认 1213 / 1205。集成测试刻意不直接 import MySQL 驱动(理由见文件头),
// 所以不能 errors.As 到 *mysql.MySQLError;生产侧的分类函数在各业务服务里,按错误号判。
func itIsLockConflict(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Error 1213") || strings.Contains(s, "Error 1205")
}

// itAwaitLockWaiters 轮询 performance_schema,直到当前库 table 上至少有 want 个事务在排队等行锁。
// 读不了 performance_schema(账号无权限 / 不是 MySQL 8)时返回 ok=false,由调用方 Skip:那是环境问题,不是产品缺陷。
func itAwaitLockWaiters(ctx context.Context, db *sql.DB, table string, want int, budget time.Duration) (waiters int, ok bool, err error) {
	const q = `
		SELECT COUNT(DISTINCT w.REQUESTING_ENGINE_TRANSACTION_ID)
		FROM performance_schema.data_lock_waits w
		JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
		WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = ?`
	deadline := time.Now().Add(budget)
	for {
		if qErr := db.QueryRowContext(ctx, q, table).Scan(&waiters); qErr != nil {
			return 0, false, qErr
		}
		if waiters >= want {
			return waiters, true, nil
		}
		if time.Now().After(deadline) {
			return waiters, true, fmt.Errorf("%v 内只看到 %d/%d 个事务排进 %s 的锁等待队列", budget, waiters, want, table)
		}
		// 轮询间的短停顿只是不去空转打爆 MySQL;条件不满足就一直等到预算用尽,不靠 sleep 估时间。
		time.Sleep(10 * time.Millisecond)
	}
}

// TestIntegrationAllocateSeqDoesNotDeadlockWithFinalize:分配与终结在同一玩家同一流上对撞,不得出现 1213。
//
// 原先未决读带 FOR UPDATE,经 idx_pending 取锁"二级 → 主键";终结按主键 CAS 改 status,"主键 → 同一二级项",
// 两者反序成环(Finalize 不锁 seq 行,seq 行挡不住它)。未决读改普通读之后,分配方对 op 表不持任何锁,环不存在。
// 分配与终结都**不重试**(IsRetryable=nil),任何一次 1213 都直接暴露。终结语句的形状与 trade / guild 的
// Finalize 一致:按主键 op_id、带 status 条件的 CAS。
// 旧写法下这条是概率性的红(窗口在一条语句内部);新写法下"零 1213"是确定的。
func TestIntegrationAllocateSeqDoesNotDeadlockWithFinalize(t *testing.T) {
	db, tables := openIntegrationDB(t)
	ctx := context.Background()
	const (
		playerID = 9006
		epoch    = uint64(1700000000000)
		rounds   = 300
	)
	if err := EnsureSeqRow(ctx, db, tables, playerID, testStream, epoch); err != nil {
		t.Fatalf("建 seq 行失败: %v", err)
	}
	noRetry := DefaultTxRetryConfig()
	noRetry.Attempts = 1

	allocated := make(chan uint64, rounds)
	allocErr := make(chan error, 1)
	go func() {
		defer close(allocated)
		for i := 0; i < rounds; i++ {
			var seq uint64
			err := WithTxRetryConfig(ctx, db, noRetry, func(tx *sql.Tx) error {
				alloc, err := AllocateSeq(ctx, tx, tables, playerID, testStream, DefaultLimits, epoch)
				if err != nil {
					return err
				}
				seq = alloc.Seq
				return insertOp(ctx, tx, playerID, testStream, alloc.Epoch, alloc.Seq, itPendingStatus)
			})
			// 终结跟不上时守卫会拒绝:那是守卫在工作,不是本用例要找的东西,跳过这一轮即可。
			if errors.Is(err, ErrTooManyPending) {
				continue
			}
			if err != nil {
				allocErr <- fmt.Errorf("第 %d 轮分配: %w", i+1, err)
				return
			}
			allocated <- seq
		}
		allocErr <- nil
	}()

	var finalizeErr error
	for seq := range allocated {
		var opID uint64
		if err := db.QueryRowContext(ctx,
			"SELECT op_id FROM "+itOpTable+" WHERE player_id = ? AND stream = ? AND stream_epoch = ? AND seq = ?",
			playerID, uint32(testStream), epoch, seq).Scan(&opID); err != nil {
			finalizeErr = fmt.Errorf("读 seq %d 的 op_id: %w", seq, err)
			break
		}
		if err := WithTxRetryConfig(ctx, db, noRetry, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				"UPDATE "+itOpTable+" SET status = ? WHERE op_id = ? AND status = ?", itDoneStatus, opID, itPendingStatus)
			return err
		}); err != nil {
			finalizeErr = fmt.Errorf("终结 op %d: %w", opID, err)
			break
		}
	}
	for range allocated { // 终结提前出错时把分配方剩下的结果排空,让它能退出
	}
	aErr := <-allocErr
	for _, err := range []error{aErr, finalizeErr} {
		if itIsLockConflict(err) {
			t.Fatalf("分配与终结对撞出现锁冲突(1213 / 1205):AllocateSeq 的未决读又加锁了?见 seq.go 注释 —— %v", err)
		}
		if err != nil {
			t.Fatalf("非预期错误: %v", err)
		}
	}
}

// TestIntegrationAllocateSeqGuardHoldsUnderConcurrentAllocators:未决读改普通读之后,并发分配者仍不能超发。
//
// 这是"普通读在 RC 下正确"那段论证的实证:N 个分配者同时抢,每个都留下一行未决;守卫上限是 limit,
// 恰好 limit 个成功,其余全是 ErrTooManyPending。少算未决(RR 快照、或 seq 行没把分配者串行化)会让成功数超过 limit。
func TestIntegrationAllocateSeqGuardHoldsUnderConcurrentAllocators(t *testing.T) {
	db, tables := openIntegrationDB(t)
	ctx := context.Background()
	const (
		playerID = 9007
		epoch    = uint64(1700000000000)
		workers  = 16
	)
	limits := Limits{MaxPending: 4, MaxSpan: 512}
	if err := EnsureSeqRow(ctx, db, tables, playerID, testStream, epoch); err != nil {
		t.Fatalf("建 seq 行失败: %v", err)
	}

	var (
		mu       sync.Mutex
		ok       int
		rejected int
		others   []error
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := WithTxRetry(ctx, db, 1, nil, func(tx *sql.Tx) error {
				alloc, err := AllocateSeq(ctx, tx, tables, playerID, testStream, limits, epoch)
				if err != nil {
					return err
				}
				return insertOp(ctx, tx, playerID, testStream, alloc.Epoch, alloc.Seq, itPendingStatus)
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrTooManyPending):
				rejected++
			default:
				others = append(others, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(others) != 0 {
		t.Fatalf("并发分配出现非预期错误: %v", others)
	}
	if ok != int(limits.MaxPending) || rejected != workers-int(limits.MaxPending) {
		t.Fatalf("上限 %d 时应恰好 %d 个成功、%d 个被守卫拒绝,实际成功 %d、拒绝 %d(成功数超限 = 未决被少算)",
			limits.MaxPending, limits.MaxPending, workers-int(limits.MaxPending), ok, rejected)
	}
}

// TestIntegrationEnsureSeqRowRetrySurvivesRolledBackFirstInserter:首个插入者回滚时,排队的两个补行都要成功。
//
// 手册 "Locks Set by Different SQL Statements" 的三会话例:会话 A 插入 (P,S) 不提交;B、C 对同一键的 INSERT
// 排队做重复键检查;A 回滚之后 B、C 同时继承到同一段间隙上的锁,又都要插入意向锁,InnoDB 牺牲其一(1213)。
// EnsureSeqRow 不重试,这里就是一次失败的请求;EnsureSeqRowRetry 应当把它吸收掉,两个都成功、表里恰好一行。
// 编排靠 performance_schema 观察"两个等待者都已排队"之后才回滚,不靠 sleep。
func TestIntegrationEnsureSeqRowRetrySurvivesRolledBackFirstInserter(t *testing.T) {
	db, tables := openIntegrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const (
		playerID = 9008
		epoch    = uint64(1700000000000)
	)

	first, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("开首个插入者事务失败: %v", err)
	}
	defer first.Rollback()
	if _, err := first.ExecContext(ctx,
		"INSERT INTO "+itSeqTable+" (player_id, stream, next_seq, epoch, updated_ms) VALUES (?, ?, 1, ?, ?)",
		playerID, uint32(testStream), epoch, epoch); err != nil {
		t.Fatalf("首个插入者插入失败: %v", err)
	}

	var retried atomic.Int32
	countingRetryable := func(err error) bool {
		if itIsLockConflict(err) {
			retried.Add(1)
			return true
		}
		return false
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			results <- EnsureSeqRowRetry(ctx, db, tables, playerID, testStream, epoch+uint64(1), countingRetryable)
		}()
	}
	collect := func() []error { return []error{<-results, <-results} }

	if _, ok, err := itAwaitLockWaiters(ctx, db, itSeqTable, 2, 10*time.Second); err != nil || !ok {
		_ = first.Rollback()
		collect()
		if !ok {
			t.Skipf("读 performance_schema.data_lock_waits 失败,无法编排本场景,跳过(不代表通过): %v", err)
		}
		t.Fatalf("夹具编排失败:%v —— 未提交的首个插入应当让两个补行都卡在重复键检查上", err)
	}

	// 回滚:两个等待者此刻继承间隙锁、互相挡住插入意向锁,成环的时刻就在这里。
	if err := first.Rollback(); err != nil {
		collect()
		t.Fatalf("回滚首个插入者失败: %v", err)
	}
	for i, err := range collect() {
		if err != nil {
			t.Fatalf("第 %d 个补行失败(重试应当吸收回滚引发的 1213): %v", i+1, err)
		}
	}
	var rows int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+itSeqTable+" WHERE player_id = ? AND stream = ?", playerID, uint32(testStream)).
		Scan(&rows); err != nil {
		t.Fatalf("数 seq 行失败: %v", err)
	}
	if rows != 1 {
		t.Fatalf("两个补行都成功之后应恰好 1 行,实际 %d 行", rows)
	}
	// 重试次数只记录不断言:它取决于 InnoDB 在回滚时怎样放锁,真库上看到 1 次说明推演成立。
	t.Logf("补行因锁冲突重试了 %d 次", retried.Load())
}
