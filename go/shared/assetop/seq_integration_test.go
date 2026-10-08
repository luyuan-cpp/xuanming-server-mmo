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
//
// # 验收开关 ASSETOP_REQUIRE_MYSQL_TESTS(与 go/friend 的 FRIEND_REQUIRE_MYSQL_TESTS 同一口径)
//
// 它**不是**第二个门控(门控仍只有 ASSETOP_TEST_MYSQL_DSN 一个),只决定"跳过算不算失败"。设了之后:
//   - ASSETOP_TEST_MYSQL_DSN 为空 → TestAssetopIntegrationGateIsHonored 判红(见
//     lock_waits_query_error_test.go),不再让"整组静默 Skip"与"全绿"在报告里长得一样;
//   - 读不了 performance_schema 的锁视图 → EnsureSeqRowRetry 那条编排用例判红,不再静默 Skip。
//
// 验收一律这样跑(PowerShell,工作目录 go/shared;账号必须能 SELECT performance_schema.data_locks /
// data_lock_waits,库级授权的 appuser 通常不行,用一次性库上的 root;密码见 deploy/docker-compose.yml,
// 不要写进任何被跟踪的文件):
//
//	$env:ASSETOP_TEST_MYSQL_DSN='root:<root 密码>@tcp(127.0.0.1:3306)/assetop_it'
//	$env:ASSETOP_REQUIRE_MYSQL_TESTS='1'
//	go test ./assetop/ -count=1 -v
//
// 通过标准:-v 输出里本文件带 Integration 的用例全部 PASS,且 TestAssetopIntegrationGateIsHonored 也 PASS
// (它只在验收模式下生效,DSN 为空时判红)。
//
// ⚠ 本文件没有 build tag,所以不设 DSN 时用例仍会跑到、然后 Skip;判断"到底跑了没有"只能看 -v 输出里的
// PASS / SKIP,或者把上面的开关打开让 Skip 变红。

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

// itMySQLErrorNumberIs 报告 err 的文本里是否带着 MySQL 错误号 number。
//
// 集成测试刻意不直接 import MySQL 驱动(理由见文件头:shared 的 go.mod 里不多出一条直接依赖),所以拿不到
// *mysql.MySQLError 去读 .Number,只能认驱动生成的文本。go-sql-driver 的 MySQLError.Error() 只有两种形态
// (v1.9.0 errors.go):"Error <号>: <文案>" 与带 SQLSTATE 的 "Error <号> (<SQLSTATE>): <文案>",
// 所以判据是"`Error <号>` 紧跟 ':' 或 ' ('"。不用 strings.Contains 裸比,是因为那样 "Error 11420" 会被当成
// 1142;这里多要一个后缀字符就能把它挡掉。错误被 fmt.Errorf("%w") 包过时前缀不在开头,所以扫全串而不是只看开头。
//
// 文本判定的脆弱性只会往**判红**的方向失效:认不出号 → 不归"不可观测" → 判红。它绝不会把该红的报成 SKIP,
// 而这正是 2026-09-21 复审要的方向(见 itLockWaitsQueryError)。
func itMySQLErrorNumberIs(err error, number uint16) bool {
	if err == nil {
		return false
	}
	needle := fmt.Sprintf("Error %d", number)
	for s := err.Error(); ; {
		at := strings.Index(s, needle)
		if at < 0 {
			return false
		}
		rest := s[at+len(needle):]
		if strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, " (") {
			return true
		}
		s = rest
	}
}

// itIsLockConflict 认 1213(死锁)/ 1205(锁等待超时)。生产侧的分类函数在各业务服务里,按错误号判。
func itIsLockConflict(err error) bool {
	return itMySQLErrorNumberIs(err, 1213) || itMySQLErrorNumberIs(err, 1205)
}

// assetopRequireMySQLEnv 是验收开关(见文件头注):设了就把"跳过"一律判红。
// 它**不是**第二个门控,门控仍只有 ASSETOP_TEST_MYSQL_DSN 一个。
const assetopRequireMySQLEnv = "ASSETOP_REQUIRE_MYSQL_TESTS"

// itRequireMySQLTests 报告是否处于验收模式。
func itRequireMySQLTests() bool {
	return os.Getenv(assetopRequireMySQLEnv) != ""
}

// errItLockWaitsUnobservable:测试账号读不了 performance_schema 的锁视图,或实例上根本没有这两张表。
// 这时手工编排做不出来 —— 那不是产品缺陷,由 itFailOrSkipOnLockWaitError 按验收模式决定跳过还是判红。
// 只有 itIsLockWaitsUnobservable 认定的错误号才归到这里,见 itLockWaitsQueryError。
var errItLockWaitsUnobservable = errors.New("读 performance_schema.data_lock_waits 失败")

// itAwaitLockWaiters 轮询,直到当前库 table 上至少有 want 个事务在排队等行锁,或 budget 用尽(返回错误)。
// 靠轮询**观察**,不靠 sleep 估时间:两次轮询之间的短停顿只是不去空转打爆 MySQL,
// 条件不满足就一直等到预算用尽并报错,不会"睡够了就当它们已经在等"。
// 查询失败时的定性见 itLockWaitsQueryError:只有权限 / 对象缺失类错误算"不可观测",ctx 结束与其余错误都判红。
func itAwaitLockWaiters(ctx context.Context, db *sql.DB, table string, want int, budget time.Duration) (int, error) {
	const q = `
		SELECT COUNT(DISTINCT w.REQUESTING_ENGINE_TRANSACTION_ID)
		FROM performance_schema.data_lock_waits w
		JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
		WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = ?`
	const pollEvery = 10 * time.Millisecond
	deadline := time.Now().Add(budget)
	for {
		var waiters int
		if err := db.QueryRowContext(ctx, q, table).Scan(&waiters); err != nil {
			return 0, itLockWaitsQueryError(ctx, err)
		}
		if waiters >= want {
			return waiters, nil
		}
		if time.Now().After(deadline) {
			return waiters, fmt.Errorf("%v 内只看到 %d/%d 个事务排进 %s 的锁等待队列", budget, waiters, want, table)
		}
		time.Sleep(pollEvery)
	}
}

// itLockWaitsQueryError 给锁等待查询的失败定性:是"环境看不见锁等待"(可按模式跳过),还是"编排出错"(判红)。
//
// 2026-09-21 复审前这里把一切查询错误都当成"不可观测"(返回 ok=false),连 ctx 超时 / 取消也算,
// 于是用例卡住会被报成 SKIP —— 该红的时候不红。现在:
//   - 用例 ctx 已结束(预算用尽 / 被取消):带出 ctx 错误(errors.Is 可认出 context.DeadlineExceeded / Canceled),判红。
//     这时查询失败只是结果,原因是编排卡住或超了预算。先看 ctx.Err() 而不是先看错误本身,是因为 ctx 结束时驱动
//     报出来的形态不固定(可能是 ctx 错误,也可能是 invalid connection 之类),以 ctx 的状态为准。
//   - MySQL 权限 / 对象缺失类错误(itIsLockWaitsUnobservable):归为 errItLockWaitsUnobservable。
//   - 其余一律判红:断连、SQL 写错、服务端内部错误都说明编排本身坏了。
//
// 与 go/friend/internal/data/friend_guard_lock_order_mysql_test.go、go/trade/internal/data/listing_repo_integration_test.go
// 的 lockWaitsQueryError 同一口径(只差:那两处能 errors.As 到 *mysql.MySQLError,这里按错误文本认号),
// 改一处要同步另两处。定性回归在 lock_waits_query_error_test.go。
func itLockWaitsQueryError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("轮询锁等待时用例 ctx 已结束(编排卡住或超出预算,不是读不了 performance_schema): %w; 查询错误: %v", ctxErr, err)
	}
	if itIsLockWaitsUnobservable(err) {
		return fmt.Errorf("%w: %v", errItLockWaitsUnobservable, err)
	}
	return fmt.Errorf("查询 performance_schema 锁等待失败,且不是权限 / 对象缺失类错误,按编排失败判红: %w", err)
}

// itIsLockWaitsUnobservable 只按 MySQL 错误号判定"账号或实例不支持观察锁等待" —— 都是环境问题,
// 与被测的锁行为无关。刻意不匹配错误文案(文案随版本与语言变),也不把"查询失败"整体归进来。
// 注意 performance_schema=OFF 时表仍在、只是恒为空,查询不报错,会走到 itAwaitLockWaiters 的"等待者没到齐"而判红;
// 本仓的 MySQL 8 默认开启,真遇到时先检查 SELECT @@performance_schema。
func itIsLockWaitsUnobservable(err error) bool {
	for _, number := range []uint16{
		1044, // ER_DBACCESS_DENIED_ERROR:对 performance_schema 库整体无权
		1142, // ER_TABLEACCESS_DENIED_ERROR:对 data_lock_waits / data_locks 没有 SELECT 权限
		1143, // ER_COLUMNACCESS_DENIED_ERROR:只授了部分列的 SELECT 权限
		1146, // ER_NO_SUCH_TABLE:实例没有这两张表(MySQL 8.0 以前、MariaDB 等)
		1227, // ER_SPECIFIC_ACCESS_DENIED_ERROR:缺某项全局权限
	} {
		if itMySQLErrorNumberIs(err, number) {
			return true
		}
	}
	return false
}

// itFailOrSkipOnLockWaitError 处理 itAwaitLockWaiters 的错误。读不了 performance_schema 时:验收模式
// (ASSETOP_REQUIRE_MYSQL_TESTS)下判红 —— 那条用例是"首个插入者回滚后两个排队者仍要成功"唯一的确定性证据,
// 不许静默跳过;否则跳过(不代表通过)。其余错误(等待者没到齐、ctx 结束、非权限类查询错误)一律判红:
// 编排失败本身就说明锁行为与推演不符,或者用例卡住了。
func itFailOrSkipOnLockWaitError(t *testing.T, err error, expectation string) {
	t.Helper()
	if errors.Is(err, errItLockWaitsUnobservable) {
		if itRequireMySQLTests() {
			t.Fatalf("%s 已设置(验收模式),无法编排本场景(测试账号需要 performance_schema 的 SELECT 权限): %v",
				assetopRequireMySQLEnv, err)
		}
		t.Skipf("无法编排本场景(测试账号需要 performance_schema 的 SELECT 权限),跳过 —— 不代表通过: %v", err)
	}
	t.Fatalf("夹具编排失败:%v —— %s", err, expectation)
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

	if _, err := itAwaitLockWaiters(ctx, db, itSeqTable, 2, 10*time.Second); err != nil {
		// 先把编排里起的两个 goroutine 收干净(回滚解锁 → 它们各自返回),再交给 itFailOrSkipOnLockWaitError
		// 定性:Skip / Fatal 都会结束本用例,留着未回收的 goroutine 只会让后面的用例看到脏连接。
		_ = first.Rollback()
		collect()
		itFailOrSkipOnLockWaitError(t, err, "未提交的首个插入应当让两个补行都卡在重复键检查上")
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
