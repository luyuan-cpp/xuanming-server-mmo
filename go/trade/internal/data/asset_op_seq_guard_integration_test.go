//go:build integration

package data

// asset_op_seq_guard_integration_test.go —— 建 seq 行的守卫锁序在真 MySQL 上的 1213 回归。
//
// 跑法与门控同 listing_repo_integration_test.go(TRADE_TEST_MYSQL_DSN + 可选的验收开关
// TRADE_REQUIRE_MYSQL_TESTS),公共助手(awaitLockWaiters / failOrSkipOnLockWaitError /
// isInnoDBDeadlock / openIntegrationDB)直接复用那个文件,不另抄一份。
// ⚠ 本文件整组 Skip **不代表通过**:缺陷只在真 InnoDB 上出现(TiDB 没有间隙锁)。
//
// # 被修的缺陷(2026-09-21 死锁审计 #16 在 trade 的同形)
//
// 旧写法在**业务事务之外**用自动提交 INSERT IGNORE 建 (player_id, stream) 的 seq 行。seq 行建出后
// 永不删除,所以并发补行的后到者拿 S 看到的是已提交的重复键、判重即结束,不会升 X。只剩一种 1213:
// 首个插入者在提交前**回滚**(连接被 KILL、刷盘失败、实例关闭、上层 ctx 到期),排在它那条未提交
// 记录后面做重复键检查的多个 INSERT 会同时继承到同一段间隙上的锁,随后各自申请插入意向锁、被对方
// 挡住,InnoDB 牺牲其一 —— 这正是 MySQL 手册 "Deadlocks in InnoDB" 的三会话例。
//
// 修法:建行挪进业务事务,排在一行**已存在、已提交**的哨兵守卫行 (player_id=0, stream=0) 之下。
// 于是同一时刻最多一个插入者,排队者等的是守卫行上的记录锁;守卫持有者回滚只是放锁,不会把锁继承
// 成间隙锁。推导与"守卫行为什么选哨兵行"见 asset_op_repo.go 的守卫一节。
//
// # 场景清单(新增场景时同步这张表)
//
//	(a) TestLegacyEnsureSeqRowDeadlocksWhenFirstInserterRollsBack   **红对照**:旧的事务外 INSERT IGNORE
//	    在"首插者回滚 + 两个排队者"的编排下必须复现 1213(恰好 1 个牺牲、1 个成功)
//	(b) TestGuardedEnsureSeqRowsTxSurvivesRolledBackFirstInserter    **绿**:同一编排走产品路径,
//	    两个排队者都成功、零 1213、终态恰好 1 行,且它们排的是**守卫行**而不是对方未提交的 seq 记录
//	(c) TestGuardedFirstTimeCreationForDistinctPlayersDoesNotDeadlock 不同玩家并发首次建行(全部排在
//	    同一把守卫上)零 1213、每人一行
//	(d) TestSeqGuardLockIsPrimaryKeyPointLookupOnRealDB              EXPLAIN 回归:探针与守卫锁定读
//	    都必须 key=PRIMARY、key_len=12(两列主键用满)
//	(e) TestEnsureSeqGuardRowIsIdempotent                            bootstrap 幂等:重复跑不改纪元、只有一行
//
// (a)(b)(c) 都是**手工编排**时序的(靠 performance_schema 观察锁等待,不靠并发度去撞):要求"两个等待者
// 同时在队列里",撞出来的概率取决于机器快慢,满足不了"产品错了必然红"。
// (a) 不红时 (b) 就什么都没证明,所以 (a) 失败的文案必须点明这一点。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	assetpb "proto/common/asset"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// seqRaceBudget 是一次编排的总预算:集成测试不许无界等待,真卡住时在这里超时判红。
	seqRaceBudget = 30 * time.Second
	// seqWaitBudget 是每一步"等某件事发生"的上限。正常都是毫秒级。
	seqWaitBudget = 10 * time.Second

	// 纪元:首插者与排队者取不同值,终态的 epoch 能指认这一行到底是谁建出来的。
	// 两个排队者**共用** seqWaiterEpochMs —— 放锁后谁先拿到守卫行由 InnoDB 决定(CATS 按事务权重,
	// 不按到达顺序),用例不该预设赢家,所以终态断言只能用"排队者共用的那个值"。
	seqHolderEpochMs uint64 = 1_800_000_000_001
	seqWaiterEpochMs uint64 = 1_800_000_000_777
)

// itSeqStream 是被测的流。取 trade 独占的 DEBIT(不变量 I6);哨兵行用的是 UNSPECIFIED,两者不会撞。
var itSeqStream = assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT

// openSeqGuardDB 建一份干净的库并返回 outbox 仓库。
//
// 门控、DSN 校验、库名断言与建表都复用 openIntegrationDB(它按 Tables() 跑 schemamigrate.Up,
// seq / outbox 两张表也在清单里,所以一定被建出来)。本函数只负责把这两张表清空。
//
// 清空用 **TRUNCATE 而不是 DELETE**,这是本文件的正确性前提而不是风格偏好:DELETE 会留下已提交的
// 删除标记记录,而后续用例恰恰要在同一批主键上做并发 INSERT —— 那是另一个死锁形状(重复键检查取 S、
// 发现要就地复活、各自升 X,friend 场景 (g) 与 trade 收藏红对照就是它)。混进来会让红对照因为错误的
// 原因变红、让绿用例莫名发红。TRUNCATE 内部重建表,不留任何行级历史。
func openSeqGuardDB(t *testing.T) (*sql.DB, *AssetOpRepo) {
	t.Helper()
	db, _ := openIntegrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, table := range []string{AssetOpTableName, AssetOpSeqTableName} {
		_, err := db.ExecContext(ctx, "TRUNCATE TABLE "+table)
		require.NoErrorf(t, err, "夹具:清空 %s 失败", table)
	}
	repo, err := NewAssetOpRepo(db, 10*time.Second)
	require.NoError(t, err)
	return db, repo
}

// bootstrapSeqGuardRow 跑一次启动期 bootstrap(产品路径 EnsureSeqGuardRow),纪元用 seqHolderEpochMs。
func bootstrapSeqGuardRow(t *testing.T, repo *AssetOpRepo) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), seqWaitBudget)
	defer cancel()
	require.NoError(t, repo.EnsureSeqGuardRow(ctx, seqHolderEpochMs), "夹具:bootstrap 哨兵守卫行")
}

// ── 编排夹具 ────────────────────────────────────────────────

// seqActor 是一次在独立 READ COMMITTED 事务里跑的建行尝试。
//
// **提交由 actor 自己的 goroutine 做**(commitSelf=true),不交给用例主 goroutine。这一点是有意的:
// 守卫行的 X 持到事务提交为止,如果让主 goroutine 按 waiters[0]、waiters[1] 的固定顺序去"等返回再提交",
// 而 InnoDB 先把守卫行给了 waiters[1],主 goroutine 就会一直等 waiters[0](它还卡在守卫行上)直到预算
// 用尽 —— 产品完全正确却判红。让每个 actor 自己提交之后,完成顺序对用例不再有任何意义。
type seqActor struct {
	name     string
	tx       *sql.Tx
	finished chan struct{} // body(及自提交)结束后关闭;关闭之后才能读 err
	err      error
}

// startSeqActor 开一个 RC 事务并在 goroutine 里跑 body。
// commitSelf=true 时 body 成功后由同一个 goroutine 提交(失败则回滚),err 带出 body 或提交的错误;
// commitSelf=false 时事务留着不提交(扮演持锁方),由调用方决定何时放锁。
func startSeqActor(t *testing.T, ctx context.Context, db *sql.DB, name string, commitSelf bool,
	body func(context.Context, *sql.Tx) error) *seqActor {
	t.Helper()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err, "夹具:开 %s 的事务失败", name)
	a := &seqActor{name: name, tx: tx, finished: make(chan struct{})}
	go func() {
		defer close(a.finished)
		a.err = body(ctx, tx)
		if !commitSelf {
			return
		}
		if a.err != nil {
			_ = tx.Rollback()
			return
		}
		a.err = tx.Commit()
	}()
	return a
}

// waitSeqActor 等 a 返回(之后才能读 a.err)。
func waitSeqActor(t *testing.T, a *seqActor) {
	t.Helper()
	select {
	case <-a.finished:
	case <-time.After(seqWaitBudget):
		t.Fatalf("%s 在 %v 内没有返回:持锁方已经放锁,它仍卡着 —— 锁行为与本用例的推演不符", a.name, seqWaitBudget)
	}
}

// startSeqRowRace 把现场摆到"首插者刚回滚"的那一刻,返回两个排队的建行事务(各自会自提交),
// 结局由调用方判定:
//
//  1. 首插者:在一个**不提交**的 RC 事务里建 (player, stream) 的 seq 行 —— 此时它持有这条新记录的 X
//     (legacy 形态),或者持有守卫行的 X **加上**这条新记录的 X(guarded 形态);
//  2. 两个排队者:各自在自己的 RC 事务里对**同一个** key 建行;
//  3. 确认两者都真的排进了 seq 表的锁等待队列(轮询 performance_schema,不靠 sleep 估时间);
//     observe 非 nil 时在这一刻回调 —— 这是唯一能看清"它们等的是哪一行"的时刻;
//  4. 首插者 **ROLLBACK** —— 旧语句成环的时刻就在这里。
//
// 用 ROLLBACK 而不是另一条连接 KILL:两者对 InnoDB 是同一件事(回滚这个事务、放掉它的锁、把刚插入的
// 记录退回未提交状态),而 ROLLBACK 是确定性的、不需要 PROCESS 权限、也不会误伤连接池里别的连接
// (KILL 的误伤风险见 listing_repo_integration_test.go 里 startDeleteMarkedFavoriteRace 的清理注释)。
//
// 编排失败一律 Fatal(唯一例外:读不了 performance_schema 且不在验收模式时 Skip),不返回半成品现场。
func startSeqRowRace(t *testing.T, db *sql.DB, player uint64,
	holderBody, waiterBody func(context.Context, *sql.Tx) error,
	observe func(context.Context)) (context.Context, [2]*seqActor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), seqRaceBudget)
	var (
		holder  *seqActor
		waiters [2]*seqActor
	)
	t.Cleanup(func() {
		// 顺序同收藏用例的清理:先取消 ctx(驱动只关本地 socket、不发 KILL QUERY;空闲连接上的事务会收到
		// 真正的 ROLLBACK,在途语句一拿到锁就执行完、随即发现断连回滚)→ 等各 actor 退出并回滚。
		// 已提交的事务再回滚是 ErrTxDone,无害。
		cancel()
		for _, a := range append(waiters[:], holder) {
			if a == nil {
				continue
			}
			select {
			case <-a.finished:
				_ = a.tx.Rollback()
			case <-time.After(seqWaitBudget):
				t.Errorf("清理:%s 在取消 ctx 之后 %v 内仍未返回", a.name, seqWaitBudget)
			}
		}
	})

	// 1. 首插者:必须真的插进去,否则它手上没有那条新记录的 X,编排不成立。
	//    "插进去了"用它**自己的事务**去数(同一事务看得见自己的未提交写);换别的连接去数只会看到 0 行。
	holder = startSeqActor(t, ctx, db, "首插者", false, holderBody)
	waitSeqActor(t, holder)
	require.NoError(t, holder.err, "夹具:首插者建行失败(它必须先成功持有新记录的 X,再由第 4 步回滚)")
	require.Equal(t, 1, seqRowCount(t, ctx, holder.tx, player),
		"夹具:首插者提交前,它自己的事务里应当已经有这一行")

	// 2. 两个排队者(自提交)。
	for i := range waiters {
		waiters[i] = startSeqActor(t, ctx, db, fmt.Sprintf("排队者#%d", i+1), true, waiterBody)
	}

	// 3. 等到两个排队者都**真的**排进了这张表的锁等待队列。
	if _, err := awaitLockWaiters(ctx, db, AssetOpSeqTableName, len(waiters), seqWaitBudget); err != nil {
		failOrSkipOnLockWaitError(t, err, "首插者未放锁,两个建行事务都应当卡在锁等待里")
	}
	if observe != nil {
		observe(ctx)
	}

	// 4. 放锁 = 回滚。旧写法在这里成环。
	require.NoError(t, holder.tx.Rollback(), "夹具:回滚首插者失败")
	return ctx, waiters
}

// seqRowQuerier 是 *sql.DB 与 *sql.Tx 的公共子集:本文件有些读必须在某个特定事务里发(见 startSeqRowRace 第 1 步)。
type seqRowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// seqRowCount 数某玩家某条流在 seq 表里的行数。
// 带完整主键等值,所以不会把哨兵行算进来;这条 COUNT 只存在于测试里,与
// TestNoAggregateQueryOverSeqTable 看守的"生产代码不许对这张表做聚合"不冲突。
func seqRowCount(t *testing.T, ctx context.Context, q seqRowQuerier, player uint64) int {
	t.Helper()
	var n int
	err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+AssetOpSeqTableName+" WHERE `player_id` = ? AND `stream` = ?",
		player, uint32(itSeqStream)).Scan(&n)
	require.NoError(t, err)
	return n
}

// readSeqRow 读某玩家 seq 行的纪元与 next_seq(已提交状态)。
func readSeqRow(t *testing.T, ctx context.Context, db *sql.DB, player uint64) (epoch, nextSeq uint64) {
	t.Helper()
	err := db.QueryRowContext(ctx,
		"SELECT `epoch`, `next_seq` FROM "+AssetOpSeqTableName+" WHERE `player_id` = ? AND `stream` = ?",
		player, uint32(itSeqStream)).Scan(&epoch, &nextSeq)
	require.NoError(t, err)
	return epoch, nextSeq
}

// legacyEnsureSeqRow 是旧写法的那条语句(文本由 legacyEnsureSeqRowSQL 给,与 assetop 逐字一致 ——
// 见那个常量的注释与 TestLegacyEnsureSeqRowSQLMatchesAssetop)。
// 放进显式事务只为造出"插了先别提交"的形态:assetop 只提供吃 *sql.DB 的自动提交入口,
// 而主键等值插入的锁行为与语句所在的事务形态无关。
func legacyEnsureSeqRow(player, epochMs uint64) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, legacyEnsureSeqRowSQL, player, uint32(itSeqStream), epochMs, epochMs)
		return err
	}
}

// guardedEnsureSeqRow 是产品路径:普通读 →(缺行时)锁守卫行 → 建行。
func guardedEnsureSeqRow(repo *AssetOpRepo, player, epochMs uint64) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		return repo.EnsureSeqRowsTx(ctx, tx, epochMs, SeqKey{PlayerID: player, Stream: itSeqStream})
	}
}

// ── 场景 (a):红对照 ────────────────────────────────────────

// TestLegacyEnsureSeqRowDeadlocksWhenFirstInserterRollsBack 是**红对照**:旧的事务外 INSERT IGNORE
// 在"首插者回滚 + 两个排队者"的编排下必须复现 1213,且恰好一个排队者被牺牲、另一个成功。
//
// 它不验产品代码,验的是"编排确实走到了成环形状"。它不红的时候,场景 (b) 的"不死锁"就什么都没证明
// (可能只是这个 MySQL 版本在这条路径上根本不成环),所以这里失败的文案必须点明这一点。
func TestLegacyEnsureSeqRowDeadlocksWhenFirstInserterRollsBack(t *testing.T) {
	const player uint64 = 9_300_001
	db, _ := openSeqGuardDB(t)

	_, waiters := startSeqRowRace(t, db, player,
		legacyEnsureSeqRow(player, seqHolderEpochMs),
		legacyEnsureSeqRow(player, seqWaiterEpochMs),
		nil)

	var deadlocked, succeeded int
	for _, w := range waiters {
		waitSeqActor(t, w)
		switch {
		case isInnoDBDeadlock(w.err):
			deadlocked++
		case w.err == nil:
			succeeded++
		default:
			t.Fatalf("%s 出现非预期错误(既不是成功也不是 1213): %v", w.name, w.err)
		}
	}
	if deadlocked != 1 || succeeded != 1 {
		t.Fatalf("红对照没有复现死锁:期望恰好 1 个 1213、1 个成功,got 1213=%d 成功=%d。"+
			"说明首插者回滚时两个排队者没有同时拿到查重锁(或记录已被 purge),编排没走到成环形状,"+
			"TestGuardedEnsureSeqRowsTxSurvivesRolledBackFirstInserter 在这个 MySQL 版本上失去证明力(见本文件头注)",
			deadlocked, succeeded)
	}
}

// ── 场景 (b):绿 ───────────────────────────────────────────

// TestGuardedEnsureSeqRowsTxSurvivesRolledBackFirstInserter 是**绿**:同一编排换成产品路径
// EnsureSeqRowsTx,两个排队者都必须成功、一次 1213 都不许发生、终态恰好一行。
//
// 判据比"都成功"更严的两处:
//   - **零 1213**:产品路径这一层不做重试(重试在 assetop.WithTxRetry 的整事务层),所以任何 1213 都会
//     原样返回,这里断言它一次都没出现 —— 有人把建行挪回守卫之前、或挪回事务外,这条必红。
//   - **排队者排的是守卫行**:靠 performance_schema 的 LOCK_DATA 看一眼等的到底是哪一行。排在对方未提交的
//     seq 记录上正是被修掉的那个形状,而那时锁等待数同样是 2 —— 只看数量分不出来。
//     LOCK_DATA 在某些配置下是 NULL,这时只记日志不判红:它是加强断言,不是本用例的门控。
func TestGuardedEnsureSeqRowsTxSurvivesRolledBackFirstInserter(t *testing.T) {
	const player uint64 = 9_300_002
	db, repo := openSeqGuardDB(t)
	bootstrapSeqGuardRow(t, repo)

	var waitedOn []string
	ctx, waiters := startSeqRowRace(t, db, player,
		guardedEnsureSeqRow(repo, player, seqHolderEpochMs),
		guardedEnsureSeqRow(repo, player, seqWaiterEpochMs),
		func(c context.Context) { waitedOn = lockDataOfWaiters(t, c, db, AssetOpSeqTableName) })

	for _, w := range waiters {
		waitSeqActor(t, w)
		if isInnoDBDeadlock(w.err) {
			t.Fatalf("%s 撞了 InnoDB 死锁(1213):守卫行没有把并发首次建行串行化 —— "+
				"检查建行是否仍排在守卫行之前、或又挪回了事务外(见 asset_op_repo.go 守卫一节): %v", w.name, w.err)
		}
		require.NoErrorf(t, w.err, "%s 出现非预期错误", w.name)
	}

	// 终态:恰好一行(INSERT IGNORE 的幂等),纪元是**排队者**写下的值。
	// 等于首插者的值就说明它的 ROLLBACK 没生效,本用例根本没走到被测分支。
	assert.Equal(t, 1, seqRowCount(t, ctx, db, player), "两个排队者都成功之后必须恰好一行")
	epoch, nextSeq := readSeqRow(t, ctx, db, player)
	assert.Equal(t, seqWaiterEpochMs, epoch,
		"纪元应当是排队者写下的值:等于首插者的值说明它的 ROLLBACK 没生效,编排没走到被测分支")
	assert.Equal(t, uint64(1), nextSeq, "新建的 seq 行 next_seq 必须从 1 起(后到者是空操作,不许改)")

	// 加强断言:排队者等的是哨兵守卫行 (0, 0),不是对方未提交的 seq 记录。
	if len(waitedOn) == 0 {
		t.Log("performance_schema 没给出 LOCK_DATA,跳过「排的是守卫行」这条加强断言(不影响上面的零 1213 判据)")
		return
	}
	wantKey := fmt.Sprintf("%d,%d", seqGuardPlayerID, uint32(seqGuardStream))
	for _, data := range waitedOn {
		assert.Equalf(t, wantKey, strings.ReplaceAll(data, " ", ""),
			"排队者等的是 %q,应当是哨兵守卫行 %q —— 等在别的行上说明守卫没生效"+
				"(排在对方未提交的 seq 记录上,正是被修掉的那个形状)", data, wantKey)
	}
}

// lockDataOfWaiters 返回正在 table 上等行锁的那些请求的 LOCK_DATA(每个等待事务一条)。
// 读不到(权限不足 / 该列为 NULL)时返回空切片,由调用方决定判红还是降级成日志。
func lockDataOfWaiters(t *testing.T, ctx context.Context, db *sql.DB, table string) []string {
	t.Helper()
	const q = `
		SELECT l.LOCK_DATA
		FROM performance_schema.data_lock_waits w
		JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
		WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = ? AND l.LOCK_TYPE = 'RECORD'`
	rows, err := db.QueryContext(ctx, q, table)
	if err != nil {
		t.Logf("读 LOCK_DATA 失败(加强断言降级为日志): %v", err)
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var data sql.NullString
		if err := rows.Scan(&data); err != nil {
			t.Logf("扫 LOCK_DATA 失败(加强断言降级为日志): %v", err)
			return nil
		}
		if !data.Valid {
			return nil // 这一列不可用:整条加强断言降级
		}
		out = append(out, data.String)
	}
	if err := rows.Err(); err != nil {
		t.Logf("遍历 LOCK_DATA 失败(加强断言降级为日志): %v", err)
		return nil
	}
	return out
}

// ── 场景 (c):不同玩家并发首次建行 ─────────────────────────

// TestGuardedFirstTimeCreationForDistinctPlayersDoesNotDeadlock:守卫行是**全局**的一行,
// 不同玩家的首次建行也全部排在它上面。这条验的是"排队而不是成环":N 个事务先后拿到守卫行的 X,
// 各自插自己那一行,零 1213、每人恰好一行。
//
// 挡板锁守卫行用的是**产品常量** sqlLockSeqGuardRow,所以"挡板锁的正是产品要锁的那一行"这件事不靠人工核对。
func TestGuardedFirstTimeCreationForDistinctPlayersDoesNotDeadlock(t *testing.T) {
	const (
		basePlayer uint64 = 9_300_101
		ensurers          = 4
	)
	db, repo := openSeqGuardDB(t)
	bootstrapSeqGuardRow(t, repo)

	// 每个建行者 + 挡板 + 观察查询同时占连接;空闲池太小的话每轮都在重新建连,与被测行为无关地拖慢。
	db.SetMaxIdleConns(ensurers + 4)

	ctx, cancel := context.WithTimeout(context.Background(), seqRaceBudget)
	actors := make([]*seqActor, 0, ensurers)
	var blockTx *sql.Tx
	t.Cleanup(func() {
		cancel()
		for _, a := range actors {
			select {
			case <-a.finished:
				_ = a.tx.Rollback()
			case <-time.After(seqWaitBudget):
				t.Errorf("清理:%s 在取消 ctx 之后 %v 内仍未返回", a.name, seqWaitBudget)
			}
		}
		if blockTx != nil {
			_ = blockTx.Rollback()
		}
	})

	blockTx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err, "夹具:开守卫行挡板事务失败")
	var one int
	require.NoError(t,
		blockTx.QueryRowContext(ctx, sqlLockSeqGuardRow, seqGuardPlayerID, uint32(seqGuardStream)).Scan(&one),
		"夹具:挡板应当锁住哨兵守卫行(锁不到 = bootstrap 没把它建出来)")

	for i := 0; i < ensurers; i++ {
		player := basePlayer + uint64(i)
		actors = append(actors, startSeqActor(t, ctx, db, fmt.Sprintf("建行者#%d", i+1), true,
			guardedEnsureSeqRow(repo, player, seqWaiterEpochMs)))
	}
	if _, err := awaitLockWaiters(ctx, db, AssetOpSeqTableName, ensurers, seqWaitBudget); err != nil {
		failOrSkipOnLockWaitError(t, err, "挡板持有守卫行的 X 时,每个建行者都应当卡在守卫行上")
	}

	require.NoError(t, blockTx.Rollback(), "夹具:放掉守卫行挡板失败")
	for i, a := range actors {
		waitSeqActor(t, a)
		if isInnoDBDeadlock(a.err) {
			t.Fatalf("%s 撞了 InnoDB 死锁(1213):守卫行下的串行建行不该成环: %v", a.name, a.err)
		}
		require.NoErrorf(t, a.err, "%s 出现非预期错误", a.name)
		assert.Equal(t, 1, seqRowCount(t, ctx, db, basePlayer+uint64(i)), "每个玩家恰好一行")
	}
}

// ── 场景 (d):EXPLAIN 回归 ─────────────────────────────────

// TestSeqGuardLockIsPrimaryKeyPointLookupOnRealDB:探针与守卫行锁定读都必须走 PRIMARY 且 key_len 用满。
//
// key_len=12 = player_id(bigint unsigned,8)+ stream(int unsigned,4);两列都 NOT NULL,没有空值字节。
// 少一列就退化成前缀范围,锁集会越出哨兵行、把别的玩家的 seq 行一起锁进来,与 AllocateSeq 跨玩家成环。
func TestSeqGuardLockIsPrimaryKeyPointLookupOnRealDB(t *testing.T) {
	db, _ := openSeqGuardDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), seqWaitBudget)
	defer cancel()

	for _, tc := range []struct{ name, stmt string }{
		{"存在性探针(普通读)", sqlSeqRowExists},
		{"守卫行锁定点查", sqlLockSeqGuardRow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainSeqStmt(t, ctx, db, tc.stmt, seqGuardPlayerID, uint32(seqGuardStream))
			assert.Equal(t, "PRIMARY", plan["key"], "必须走主键:%s(完整计划 %v)", tc.stmt, plan)
			assert.Equal(t, "12", plan["key_len"], "主键两列必须用满(8+4):%s(完整计划 %v)", tc.stmt, plan)
		})
	}
}

// explainSeqStmt 跑一次 EXPLAIN(传统格式)并返回小写列名 → 值。
// 带 FOR UPDATE 的语句照样能 EXPLAIN:优化器只出计划,不取锁。
func explainSeqStmt(t *testing.T, ctx context.Context, db *sql.DB, stmt string, args ...any) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, "EXPLAIN "+stmt, args...)
	require.NoErrorf(t, err, "EXPLAIN %s", stmt)
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	require.NoError(t, err)
	require.Truef(t, rows.Next(), "EXPLAIN 没有返回任何行: %s", stmt)
	cells := make([]sql.NullString, len(cols))
	dest := make([]any, len(cols))
	for i := range cells {
		dest[i] = &cells[i]
	}
	require.NoError(t, rows.Scan(dest...))
	out := make(map[string]string, len(cols))
	for i, c := range cols {
		out[strings.ToLower(c)] = cells[i].String
	}
	require.NoError(t, rows.Err())
	return out
}

// ── 场景 (e):bootstrap 幂等 ───────────────────────────────

// TestEnsureSeqGuardRowIsIdempotent:bootstrap 每次起服都会跑,多副本会同时跑。
// 重复跑必须只有一行、纪元不被改写(INSERT IGNORE 的语义)。纪元被改写说明语句从 INSERT IGNORE 变成了
// upsert —— 同一条语句也在给**玩家** seq 行用,那时纪元被改写会让 scene 认为整条流需要重置(§4.29)。
func TestEnsureSeqGuardRowIsIdempotent(t *testing.T) {
	db, repo := openSeqGuardDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), seqWaitBudget)
	defer cancel()

	require.NoError(t, repo.EnsureSeqGuardRow(ctx, seqHolderEpochMs))
	require.NoError(t, repo.EnsureSeqGuardRow(ctx, seqWaiterEpochMs), "第二次 bootstrap 必须是空操作")

	var rows, epoch, nextSeq uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+AssetOpSeqTableName+" WHERE `player_id` = ? AND `stream` = ?",
		seqGuardPlayerID, uint32(seqGuardStream)).Scan(&rows))
	assert.Equal(t, uint64(1), rows, "哨兵守卫行必须恰好一行")
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT `epoch`, `next_seq` FROM "+AssetOpSeqTableName+" WHERE `player_id` = ? AND `stream` = ?",
		seqGuardPlayerID, uint32(seqGuardStream)).Scan(&epoch, &nextSeq))
	assert.Equal(t, seqHolderEpochMs, epoch, "第二次 bootstrap 不许改写纪元(INSERT IGNORE 而不是 upsert)")
	assert.Equal(t, uint64(1), nextSeq)

	// 纪元必须为正是 assetop 的硬前置;bootstrap 传 0 要当场失败,而不是写出一行 epoch=0 的守卫行
	// (那个值在 AllocateSeq 里是 ErrSeqRowCorrupt)。
	require.Error(t, repo.EnsureSeqGuardRow(ctx, 0), "纪元为 0 时必须失败")
}
