package data

// friend_guard_lock_order_mysql_test.go —— 容量守卫锁序的 InnoDB 死锁(1213)回归。
//
// # 这个文件存在的理由
//
// 移植自 A 仓 services/social/friend/internal/data/friend_guard_lock_order_mysql_test.go
// (2026-08-11)。A 仓那次的经过值得逐字记住,因为它解释了本文件每一条设计:
//
//   - 缺陷是"守卫取得的时机":未命中记录的 `SELECT ... FOR UPDATE` 在 InnoDB RR 下锁的是
//     **键所在的间隙**而不是某一行。N 个不同申请人指向同一个 target 时,探针全部落进同一个
//     supremum 间隙。间隙锁彼此相容(都拿得到),排他点在随后的守卫行:谁抢到守卫谁就去写,
//     而写入意向被其余事务仍持有的间隙锁挡住 → 成环 → 1213。
//   - 缺陷**只在真 MySQL 上出现**(TiDB 没有 gap 锁),而当时 CI 从不设 MySQL 的 DSN,
//     于是这条路径长期"SKIP 在报告里等于 ok",缺陷被盖了一个多月。
//     ⚠ 所以:本文件全体 Skip 时**不代表通过**。FRIEND_TEST_MYSQL_DSN 必须在验收时真的设上,
//     并在交付里写明"跑了、看到了 PASS 而不是 SKIP"。
//
// # 本仓(B 仓)的形态与 A 仓的差别
//
// B 没有独立的 guards 表,守卫就是 `friend_capacity` 的计数行本身(D-10:显式计数锁行
// 即好友数硬上限)。规格 §2.2 因此把纪律统一成一句话:
//
//	**任何写事务,在拿到容量守卫之前,不得做任何锁定读。**
//
// 并配 §2.3 的 READ COMMITTED(RC 没有间隙锁,同一玩家并发拉黑 16 个不同目标这类只碰
// 不同行的事务不再互相挡),代价是 RC 下普通 SELECT 读语句快照,所以守卫之后的**一切**
// 判定读必须用 `FOR UPDATE` 当前读。
//
// # 与 friend_repo_mysql_test.go 的分工(不可互相替代)
//
// 那个文件验"上限不被并发穿透"(业务性质),本文件验"并发路径不产生 1213"(锁序性质)。
// 把守卫挪回锁定读之后,业务断言仍可能碰巧过(先失败的事务被算进另一类错误才会露出来),
// 而本文件必红。
//
// # 场景清单(8 个;新增场景时同步这张表)
//
//	(a)  TestAddFriendGuardBeforeLockingReads_SharedTarget      16 个申请人 → 同一个 target
//	(a') TestAddFriendGuardBeforeLockingReads_DistinctTargets   16 个申请人 → 各自不同的 target
//	(b)  TestBlockGuardBeforeLockingReads_SharedBlocker         同一玩家并发拉黑 16 个目标
//	(c)  TestBlockAndAcceptFriendInterleaved                    同一对玩家 Block 与 AcceptFriend 交错
//	(d)  TestAddFriendAndAcceptFriendOnSamePair                 同一对玩家 AddFriend 与 AcceptFriend(两支)
//	(e)  TestAddFriendPendingCountsAreNotLockingReads_CrossedPendingRows
//	     预置交叉 pending 行,两个**不相干 pair** 并发 AddFriend(⑤⑥ 不加 FOR UPDATE 的双向回归)
//	(f)  TestCapacityRowReclaimRacesWithGuardedWrites           容量行回收与四条写路径并发
//	(g)  TestEnsureCapacityRows_ConcurrentInsertOnReclaimedRowSurvivesDeadlock
//	     回收的 DELETE 压着 X,两个 ensure 同时排队等同一主键;断言两者都成功且 1213 重试一次都没触发
//	     (补行改 ODKU 之前,这里是 delete-marked 记录上的 S→X 成环)
//	(h)  TestBlockInsertOnRowUnderPendingDeleteDoesNotDeadlockWithQueuedUnblock
//	     Unblock 的 DELETE 压着 X,Block ④ 与另一个 Unblock 先后排队;断言两者都不 1213
//
// (a)–(d) 都从空表起跑;(e) 是唯一预置 friend_request 行的,(f)(g) 是仅有的两个有"删守卫行"一方的;
// (g)(h) 是**手工编排**时序的(靠 performance_schema 观察锁等待,不靠并发度去撞),共用 awaitLockWaiters。
//
// 公共夹具(门控、schema、不变量断言、isInnoDBDeadlock)在 friend_repo_mysql_test.go。

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

	drivermysql "github.com/go-sql-driver/mysql"
)

// guardConcurrency = 16。
//
// 刻意高于业务用例的并发度:A 仓的记录是锁序缺陷"随并发度升高才稳定复现",8 并发时
// 时序偶尔错开就不炸。16 是那次能确定性复现的档位,别为了跑得快调低它 ——
// 调低之后这个文件就变成了一条昂贵的绿灯。
const guardConcurrency = 16

// runConcurrent 让 n 个 goroutine 在同一个屏障上起跑,返回按 i 下标对齐的错误。
//
// 两条纪律(AGENTS.md §11.4:测试必须确定、可重复;本批明令禁止用 sleep 代替条件等待):
//   - 起跑用 close(chan) 屏障,不用 time.Sleep —— sleep 既不保证真的同时,又会在慢机器上
//     把并发窗口整个错过(那样测试永远绿);
//   - 结果用下标定位的切片收,不用 append + mutex —— mutex 本身会把 goroutine 排成队,
//     并发度被测试代码自己削掉。
func runConcurrent(t *testing.T, n int, fn func(i int) error) []error {
	t.Helper()
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

// classifyGuardErrors 把并发结果分成"成功 / 预期的业务拒绝 / 不该出现的错误",
// 并在看到 1213 时直接 Fatal(附上锁序的解释,免得下一个人只看到一句 "unexpected error")。
//
// expected 里列的是**本场景允许**的业务拒绝。刻意不写成"非 nil 即算业务拒绝":
// 那样任何真错误(列不存在、连接断了)都会被算成"上限生效",这个文件就废了。
func classifyGuardErrors(t *testing.T, what string, errs []error, expected ...error) (succeeded int) {
	t.Helper()
	for _, err := range errs {
		assertNoDeadlock(t, err, what)
		if err == nil {
			succeeded++
			continue
		}
		matched := false
		for _, want := range expected {
			if errors.Is(err, want) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("%s 出现非预期错误(既不是成功也不是本场景允许的业务拒绝): %v", what, err)
		}
	}
	return succeeded
}

// ── 场景 (a):16 个不同申请人并发 AddFriend 同一个 target ──────
//
// 这是 A 仓死锁的原始形状:所有事务共享 friend_request 上同一段键空间,又都要抢
// 同一个 target 的容量守卫行。
func TestAddFriendGuardBeforeLockingReads_SharedTarget(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const target uint64 = 81001
	lim := defaultTestLimits()
	lim.MaxIncomingRequests = 4 // 低于并发度:同时验"上限不被穿透"

	errs := runConcurrent(t, guardConcurrency, func(i int) error {
		return callAddFriend(ctx, repo, uint64(81100+i), target, lim)
	})

	succeeded := classifyGuardErrors(t, "并发 AddFriend(同一 target)", errs, ErrTargetInboxFull)
	// 锁序修复不得把上限一起放松:恰好 MaxIncomingRequests 条成功。
	// 多了 = 上限被并发穿透;少了 = 有事务被误拒(通常是把锁等待当成了业务失败)。
	if succeeded != int(lim.MaxIncomingRequests) {
		t.Fatalf("成功 %d 条, want %d(多了=上限被穿透,少了=事务被误拒)", succeeded, lim.MaxIncomingRequests)
	}
	if got := mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE to_player_id=? AND status=1", target); got != int64(lim.MaxIncomingRequests) {
		t.Fatalf("落库 pending=%d, want %d", got, lim.MaxIncomingRequests)
	}
	assertFriendInvariants(t, ctx, db)
}

// ── 场景 (a'):16 个申请人并发 AddFriend **各自不同**的 target ──
//
// 守卫行各不相同(不构成排他点),暴露的是另一半:键空间本身跨事务共享。
// 空表时所有 (from,to) 主键都落在同一段间隙里,写入意向互相阻塞。
// 与 (a) 一起覆盖"共享守卫"与"不共享守卫"两侧 —— 只写一侧会漏掉 RR→RC 这一步的收益。
func TestAddFriendGuardBeforeLockingReads_DistinctTargets(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	lim := defaultTestLimits()

	errs := runConcurrent(t, guardConcurrency, func(i int) error {
		return callAddFriend(ctx, repo, uint64(82100+i), uint64(82200+i), lim)
	})

	// 这一组彼此毫无交集,**一条都不该失败**。任何失败都说明锁等待 / 间隙锁在
	// 无关事务之间传染(RR 的 gap lock 就是这么把它们互相挡住的)。
	succeeded := classifyGuardErrors(t, "并发 AddFriend(不同 target)", errs)
	if succeeded != guardConcurrency {
		t.Fatalf("互不相干的并发申请成功 %d 条, want %d", succeeded, guardConcurrency)
	}
	if got := mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE status=1"); got != guardConcurrency {
		t.Fatalf("落库 pending=%d, want %d", got, guardConcurrency)
	}
	assertFriendInvariants(t, ctx, db)
}

// ── 场景 (b):同一玩家并发 Block 16 个不同目标 ────────────────
//
// 与 (a) 同构:共享 blocker 的容量守卫 + 共享 friend_block 的键空间。
// 这条同时是 §2.3 选 RC 的直接依据 —— A 仓实测 RR 下这个形状会 1213。
func TestBlockGuardBeforeLockingReads_SharedBlocker(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const blocker uint64 = 83001
	lim := defaultTestLimits()
	lim.MaxBlocks = 6

	errs := runConcurrent(t, guardConcurrency, func(i int) error {
		return callBlock(ctx, repo, blocker, uint64(83100+i), lim)
	})

	succeeded := classifyGuardErrors(t, "并发 Block", errs, ErrBlockListFull)
	if succeeded != int(lim.MaxBlocks) {
		t.Fatalf("成功拉黑 %d 个, want %d(上限被并发穿透或被误拒)", succeeded, lim.MaxBlocks)
	}
	if got := mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_block WHERE player_id=?", blocker); got != int64(lim.MaxBlocks) {
		t.Fatalf("落库黑名单=%d, want %d", got, lim.MaxBlocks)
	}
	assertFriendInvariants(t, ctx, db)
}

// ── 场景 (c):Block 与 AcceptFriend 交错 ─────────────────────
//
// **本批最关键的一条之一**:它直接验规格 §2.2 的全局锁序。
//
// 两条路径都要锁同一对玩家的容量行,又各自要改 friend / friend_request / friend_block。
// 8 对玩家、每对两个竞争写者 = 16 并发。无论谁赢,库里都不许出现
// "既是好友又拉黑"或"拉黑后仍有 pending" —— 断言落在结构不变量上而不是"谁该赢",
// 所以它不随调度 flaky,也不会因为换个实现就要重写。
func TestBlockAndAcceptFriendInterleaved(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const pairs = guardConcurrency / 2
	const blockerBase, requesterBase uint64 = 84001, 84100
	lim := defaultTestLimits()

	// 每对先有一条 pending:AcceptFriend 才有东西可接受,Block 才有 pending 要取消。
	for i := 0; i < pairs; i++ {
		seedPending(t, ctx, db, requesterBase+uint64(i), blockerBase+uint64(i))
	}

	errs := runConcurrent(t, guardConcurrency, func(i int) error {
		pair := i / 2
		me := blockerBase + uint64(pair)
		peer := requesterBase + uint64(pair)
		if i%2 == 0 {
			return callBlock(ctx, repo, me, peer, lim)
		}
		// AcceptFriend 的参数顺序是 (申请人, 接受者)。
		return repo.AcceptFriend(ctx, peer, me, lim.MaxFriends)
	})

	// 允许的业务拒绝:
	//   - Block 先赢 → AcceptFriend 看到拉黑 → ErrBlocked;或申请已被置终态 → ErrRequestNotFound;
	//   - Accept 先赢 → Block 照常成功(它会删掉刚建的边),所以 Block 侧不该有业务拒绝。
	classifyGuardErrors(t, "Block 与 AcceptFriend 交错", errs, ErrBlocked, ErrRequestNotFound)

	// 结构不变量是本场景的真正断言。
	assertFriendInvariants(t, ctx, db)

	// 再逐对钉一条更强的:每一对最终都被拉黑了(Block 无论早晚都会成功),
	// 所以最终**一定**没有边、也没有 pending。
	// 只断言"无 1213"是不够的:锁序对了但 Block 的删边/减计数漏做,上面的不变量会漏过
	// "Accept 后 Block" 这一支(边还在、却已拉黑)—— 那正好被 assertNoFriendAndBlocked 抓住,
	// 这里再显式写一遍边数,是为了让失败信息直接指向"§3.4 ⑤ 删边"。
	for i := 0; i < pairs; i++ {
		me := blockerBase + uint64(i)
		peer := requesterBase + uint64(i)
		if got := mustCount(t, ctx, db,
			"SELECT COUNT(*) FROM friend_block WHERE player_id=? AND blocked_player_id=?", me, peer); got != 1 {
			t.Fatalf("pair %d:Block 未落库(黑名单行=%d)", i, got)
		}
		if got := mustCount(t, ctx, db,
			"SELECT COUNT(*) FROM friend WHERE (player_id=? AND friend_player_id=?) OR (player_id=? AND friend_player_id=?)",
			me, peer, peer, me); got != 0 {
			t.Fatalf("pair %d:拉黑后仍残留 %d 条好友边(§3.4 ⑤ 的双向删边或减计数漏做)", i, got)
		}
	}
}

// ── 场景 (d):同一对玩家并发 AddFriend 与 AcceptFriend ───────
//
// **这条是本批新引入风险的专属回归,锁序写错必挂。**
//
// 规格 §2.1 的 ABBA 环逐字如下:
//   - AcceptFriend(旧顺序)持有 friend_request(A,B) 的行锁,等 friend_capacity(A)/(B);
//   - AddFriend(F2 新增)持有 friend_capacity(A)/(B),等 friend_request(A,B)。
//
// 互等成环 → 1213。这不是理论风险:同一对玩家"一边接受、一边重发申请"是最常见的时序
// (客户端双击、弱网重试都会撞上)。
// §2.2 的裁定是把 AcceptFriend 的申请行 FOR UPDATE **下移到容量守卫之后**;
// 一旦有人照旧注释把它挪回去,本用例立刻红。
func TestAddFriendAndAcceptFriendOnSamePair(t *testing.T) {
	t.Run("已有 pending", func(t *testing.T) {
		db, ctx := openFriendTestDB(t)
		repo, _ := newFriendTestRepo(t, db)
		const a, b uint64 = 85001, 85002
		lim := defaultTestLimits()
		seedPending(t, ctx, db, a, b)

		errs := runConcurrent(t, guardConcurrency, func(i int) error {
			if i%2 == 0 {
				return callAddFriend(ctx, repo, a, b, lim) // 重发申请
			}
			return repo.AcceptFriend(ctx, a, b, lim.MaxFriends) // 接受
		})

		// AddFriend 在这一支**永远不该成功**:它要么看到 status=1 的申请(ErrRequestAlreadySent),
		// 要么看到已经建好的边(ErrAlreadyFriends)。成功一次就意味着它在守卫内用的是
		// 快照读而不是当前读(RC 的代价,§2.3 ②)。
		// AcceptFriend 恰好成功一次:RowsAffected==1 的 fail-closed 门禁 + 守卫串行化。
		accepted, rejectedAdds := 0, 0
		for i, err := range errs {
			assertNoDeadlock(t, err, "同一对玩家并发 AddFriend / AcceptFriend")
			if i%2 == 0 {
				if err == nil {
					t.Fatalf("AddFriend 在已有 pending / 已是好友时成功了:守卫内的判定读不是当前读")
				}
				if !errors.Is(err, ErrRequestAlreadySent) && !errors.Is(err, ErrAlreadyFriends) {
					t.Fatalf("AddFriend 非预期错误: %v", err)
				}
				rejectedAdds++
				continue
			}
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrRequestNotFound):
			default:
				t.Fatalf("AcceptFriend 非预期错误: %v", err)
			}
		}
		if accepted != 1 {
			t.Fatalf("AcceptFriend 成功 %d 次, want 恰好 1(>1 = 申请被重复消费,0 = 全部被误拒)", accepted)
		}
		if rejectedAdds != guardConcurrency/2 {
			t.Fatalf("AddFriend 被拒 %d 次, want %d", rejectedAdds, guardConcurrency/2)
		}

		if got := mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend"); got != 2 {
			t.Fatalf("好友边=%d, want 2(双向各一条)", got)
		}
		assertFriendInvariants(t, ctx, db)
	})

	t.Run("无 pending", func(t *testing.T) {
		db, ctx := openFriendTestDB(t)
		repo, _ := newFriendTestRepo(t, db)
		const a, b uint64 = 85003, 85004
		lim := defaultTestLimits()

		// 反过来的一支:没有申请可接受,所以是 AddFriend 恰好成功一次。
		// 两支一起跑才覆盖"谁先拿到守卫"的两种顺序;只写一支会让锁序缺陷有一半概率溜过。
		errs := runConcurrent(t, guardConcurrency, func(i int) error {
			if i%2 == 0 {
				return callAddFriend(ctx, repo, a, b, lim)
			}
			return repo.AcceptFriend(ctx, a, b, lim.MaxFriends)
		})

		addOK := 0
		for i, err := range errs {
			assertNoDeadlock(t, err, "同一对玩家并发 AddFriend / AcceptFriend(无 pending)")
			if i%2 == 0 {
				switch {
				case err == nil:
					addOK++
				case errors.Is(err, ErrRequestAlreadySent), errors.Is(err, ErrAlreadyFriends):
				default:
					t.Fatalf("AddFriend 非预期错误: %v", err)
				}
				continue
			}
			// AcceptFriend 可能赶在某次 AddFriend 之后跑,那时它是合法成功。
			switch {
			case err == nil:
			case errors.Is(err, ErrRequestNotFound):
			default:
				t.Fatalf("AcceptFriend 非预期错误: %v", err)
			}
		}
		if addOK != 1 {
			t.Fatalf("AddFriend 成功 %d 次, want 恰好 1(>1 = 重复申请没被去重)", addOK)
		}
		if got := mustCount(t, ctx, db,
			"SELECT COUNT(*) FROM friend_request WHERE from_player_id=? AND to_player_id=?", a, b); got != 1 {
			t.Fatalf("申请行=%d, want 1(主键去重 + 守卫串行化)", got)
		}
		assertFriendInvariants(t, ctx, db)
	})
}

// ── 场景 (e):预置交叉 pending 行,两个不相干 pair 并发 AddFriend ─────────
//
// 这条钉的是 AddFriendRequest 步骤 ⑤⑥ 那两条 pending COUNT **刻意不加 FOR UPDATE** 的决定
// (handoff §3 第 3 条;论证在 friend_repo.go 的 ⑤⑥ 旁)。它的价值是**双向**的:
//
//   - 今天应当绿:普通读不加锁,TRX1 = AddFriend(A→T) 与 TRX2 = AddFriend(B→C) 没有任何共享的锁,
//     两条都必须成功。这证明"不加 FOR UPDATE"在真 InnoDB 上是对的。
//   - 谁哪天"顺手补上 FOR UPDATE",它会立刻红:两条 COUNT 的加锁集合是"from=A 的行"与"to=T 的行",
//     跨玩家对交叉 —— TRX1 先锁 (A,C) 再要 (B,T),TRX2 先锁 (B,T) 再要 (A,C),而两个事务的容量守卫
//     (A,T) 与 (B,C) 毫无交集,守卫拦不住 → 1213。
//
// 为什么 (a)–(d) 测不到:它们都从空表起跑,⑤⑥ 扫到的行集为空,那个环在结构上不可能出现。
// 所以本用例的**全部鉴别力来自预置的这两行** (A,C) 与 (B,T):去掉预置,它就退化成 (a')。
//
// 成环要求两个事务都过了 ⑤ 而都还没做 ⑥,是时序窗口而不是必然 —— 所以一轮放 8 组互不相干的
// 四元组(16 并发,guardConcurrency 档位)、连跑多轮,每轮换一批全新的 id(同一批 id 第二轮会回
// ErrRequestAlreadySent,在 ④ 就返回、根本走不到 ⑤⑥)。
func TestAddFriendPendingCountsAreNotLockingReads_CrossedPendingRows(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	lim := defaultTestLimits() // 上限都 > 0:⑤⑥ 两条 COUNT 只在上限非 0 时才执行

	const (
		rounds        = 8
		quads         = guardConcurrency / 2
		idBase uint64 = 86000000
	)
	// 四元组 (A,B,C,T) 的 id:每轮、每组各占一段,全程不复用。
	quadIDs := func(round, quad int) (a, b, c, target uint64) {
		base := idBase + uint64(round)*1000 + uint64(quad)*10
		return base + 1, base + 2, base + 3, base + 4
	}

	for round := 0; round < rounds; round++ {
		for quad := 0; quad < quads; quad++ {
			a, b, c, target := quadIDs(round, quad)
			seedPending(t, ctx, db, a, c)      // from=A 的集合里有一行,它同时属于 to=C 的集合
			seedPending(t, ctx, db, b, target) // from=B 的集合里有一行,它同时属于 to=T 的集合
		}

		errs := runConcurrent(t, guardConcurrency, func(i int) error {
			a, b, c, target := quadIDs(round, i/2)
			if i%2 == 0 {
				return callAddFriend(ctx, repo, a, target, lim) // TRX1:⑤ 扫 from=A,⑥ 扫 to=T
			}
			return callAddFriend(ctx, repo, b, c, lim) // TRX2:⑤ 扫 from=B,⑥ 扫 to=C
		})

		// 不列任何"允许的业务拒绝":这 16 条申请两两不冲突(不同的申请行、上限远未触及),
		// 一条都不该失败。1213 由 classifyGuardErrors 里的 assertNoDeadlock 单独点名。
		succeeded := classifyGuardErrors(t,
			fmt.Sprintf("预置交叉 pending 行的并发 AddFriend(第 %d 轮)", round+1), errs)
		if succeeded != guardConcurrency {
			t.Fatalf("第 %d 轮成功 %d 条, want %d(两个不相干 pair 的申请不该互相影响)",
				round+1, succeeded, guardConcurrency)
		}
	}

	// 每组最终恰好 4 条 pending:预置的 (A,C)、(B,T) 与新发的 (A,T)、(B,C)。
	// 逐条点名新发的两条,不只看总数:总数对而写错行(例如方向反了)是最难发现的一类错。
	for round := 0; round < rounds; round++ {
		for quad := 0; quad < quads; quad++ {
			a, b, c, target := quadIDs(round, quad)
			if got := readRequestStatus(t, ctx, db, a, target); got != 1 {
				t.Fatalf("第 %d 轮第 %d 组:(A→T) 的申请 status=%d, want 1", round+1, quad, got)
			}
			if got := readRequestStatus(t, ctx, db, b, c); got != 1 {
				t.Fatalf("第 %d 轮第 %d 组:(B→C) 的申请 status=%d, want 1", round+1, quad, got)
			}
		}
	}
	if got, want := mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_request WHERE status=1"),
		int64(rounds*quads*4); got != want {
		t.Fatalf("落库 pending=%d, want %d", got, want)
	}
	// AddFriend 只写申请行:不建边、不动计数。friend_count 与边数一致(此处即全为 0)。
	if got := mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend"); got != 0 {
		t.Fatalf("AddFriend 不该建好友边,却出现了 %d 条", got)
	}
	if got := mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_capacity WHERE friend_count <> 0"); got != 0 {
		t.Fatalf("AddFriend 不该改 friend_count,却有 %d 行非 0", got)
	}
	assertFriendInvariants(t, ctx, db)
}

// ── 场景 (f):容量行回收与四条写路径并发 ───────────────────────
//
// 回收(SweepIdleCapacityRows)是 friend_capacity 唯一的删除方,也是本域唯一会**删守卫行**的参与者。
// 它要同时守住三件事,本用例各有一条断言对应:
//
//  1. 不成环:回收逐行自动提交、任一时刻至多持一把守卫锁。谁把它改成一条批量
//     `DELETE ... LIMIT ?`(在一个语句事务里按二级索引序锁多行),就与写路径"按 player_id 升序
//     锁两行"的顺序相反 → 1213。
//  2. 写路径不因回收而失败:守卫缺行由 runGuardedWrite 重新 ensure 并重试,至多重试两次
//     (上限 capacityGuardMaxAttempts 遍;为什么三遍严格充分见 friend_repo.go 顶部锁序说明 (5))。
//  3. 提交点复核:候选读与删除之间该行可能已被 AcceptFriend 加过好友,DELETE 的 WHERE 必须重判
//     `friend_count = 0`;漏了它,回收会删掉一个有好友的玩家的权威计数行。
//
// # 怎么让竞态真的发生,又不把断言写脆
//
// 回收只删"足够老"的零好友行,而 ensure 建出来的行 created_ms 是当前时刻 —— 用真实时钟时新行永远
// 不够老,回收一行都不会删,这个用例就成了绿灯摆设。造"老行"的两种办法里只有一种可用:
//
//   - ✗ 把 nowMs 拨到未来:所有零好友行(含**刚被重试 ensure 出来的那行**)立刻可回收,
//     这破坏了产品代码"至多重试两次"所依赖的前提(重新建出来的行 created_ms 是当前时刻,
//     不可能马上又被回收 —— 即各方墙钟偏差小于保留期 RetentionDays,config 保证它 ≥ 1 天),
//     重试后仍缺行会成为常态,断言只能放宽成"失败率别太高" —— 既脆又没有鉴别力。
//   - ✓ 回收用真实 nowMs,由写者在每次写之前把容量行的 created_ms 直写成一个很小的值
//     (= 一行陈旧的存量行)。
//
// 第 1 条与第 2 条对"哪几行陈旧"的要求正好相反,所以写者分两组:
//
//   - **严格组**(16 个,走完整的四条写路径):每个写者只让自己这一对里**固定的一行**变陈旧。
//     第一遍尝试随时可能撞上回收;而重试时那一行是 ensure 新建的、另一行从未陈旧过,
//     **第二遍就必然成功**(用不到第三遍)。断言因此写死:四条写路径只许返回 nil,errCapacityRowsMissing
//     露出来一次就是红(要么重试没了,要么重试前没有重新 ensure)。偶数号写者让 id 较小的一行变陈旧、
//     奇数号让较大的一行变陈旧,守卫升序取锁的两个位置都覆盖到。
//     这一组验不了第 1 条:每对只有一行是回收的候选,回收(哪怕写成批量 DELETE)与任何一个写者
//     至多争一行,结构上成不了环。
//   - **双陈旧组**(4 个,只跑 runGuardedWrite 的空 body —— 四条写路径共用的取锁骨架):两行都变陈旧,
//     且让 id **较大**的那行 created_ms **更小**。于是二级索引 (friend_count, created_ms) 的顺序是
//     "先 q 后 p",与守卫的"先 p 后 q"相反:批量 DELETE 会持 q 等 p、写者持 p 等 q → 必成环。
//     今天的逐行实现不"持有并等待",这一组应当一次 1213 都没有。
//     两行都陈旧也正是缺行重试的**最坏形态**:第 1 遍 ensure 空操作 → 回收删 p → 缺行;第 2 遍 ensure
//     重建 p(created_ms = 当前时刻)、q 仍是空操作 → 回收删 q → 缺行;第 3 遍 ensure 重建 q。
//     回收的 DELETE 在提交点复核 `created_ms < 截止点`,重建出来的行不可能再被删,回收者再多,
//     每行也至多被删一次 —— 所以三遍之内必过,这一组与严格组一样**不容忍** errCapacityRowsMissing。
//     前提同上:本用例的回收用真实 nowMs、retentionDays = 1 天,写者与回收者同进程同时钟。
//     (每对玩家由单个写者独占,没有对同一主键的并发 INSERT IGNORE,硬断言不会被场景 (g) 那种
//     ensure 的 1213 干扰。)
//
// # 怎么知道竞态窗口真的被撞上了
//
// 严格组每轮最后有一步对照:同样先让那一行变陈旧,再直接调**不带重试**的 runGuardedWriteOnce
// (空 body),数它报了多少次 errCapacityRowsMissing。这个数不做失败断言:> 0 说明本次运行里
// 回收确实插进了"ensure 之后、守卫之前",上面"只许 nil"的断言因此有鉴别力;= 0 说明这台机器上窗口
// 没被撞上(不是产品错了),本次运行对第 2 条**没有证明力** —— 此时用例在其余硬断言全部通过之后
// 以 **SKIP** 呈现(不是 PASS),报告里一眼可辨,请重跑。把它做成硬断言(Fatalf)会让用例在
// 快机器 / 慢机器上随机红;整场景自动重跑 N 次又会顶到 openFriendTestDB 的 60s 预算。
// "回收真的删过行"则是硬断言(见下):那与机器快慢无关。
// (重试的确定性那一半 —— 用尽仍缺行时 fail-closed —— 在 friend_repo_mysql_test.go 的
// TestRunGuardedWrite_ExhaustedMissingRowsFailClosed。)
func TestCapacityRowReclaimRacesWithGuardedWrites(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	lim := defaultTestLimits()

	// 连接池上限是 32(openFriendTestDB):16 + 4 + 4 = 24 个 goroutine 各占至多一条连接,
	// 不会有人在 database/sql 的队列里排队(排队会把并发度悄悄削掉,见 openFriendTestDB 的说明)。
	const (
		strictWriters             = guardConcurrency
		bothStaleWriters          = 4
		sweepers                  = 4 // 多个回收者各持一份候选名单:覆盖"名单已过时、该行已被别人删 / 已被重建"
		iterations                = 25
		retentionDays             = 1
		batchLimit                = 1000
		strictIDBase       uint64 = 87000000
		bothStaleIDBase    uint64 = 87500000
		staleCreatedMs     uint64 = 2 // 陈旧,但不是 0:与"存量搬迁行"区分开,排障时一眼看得出是本用例造的
		staleOlderCreateMs uint64 = 1 // 双陈旧组里 id 较大的那行用它,使二级索引序与主键序相反
	)
	pairOf := func(base uint64, writer int) (p, q uint64) {
		first := base + uint64(writer)*10
		return first + 1, first + 2 // p < q
	}
	// age 让 playerID 的容量行变成"陈旧的存量行"。行不存在(刚被回收)时影响 0 行,不是错误。
	// 单条自动提交、按主键:它自己任一时刻只持一把锁,不会给本场景引入新的成环来源。
	age := func(playerID, createdMs uint64) error {
		if _, err := db.ExecContext(ctx,
			"UPDATE friend_capacity SET created_ms = ? WHERE player_id = ?", createdMs, playerID); err != nil {
			return fmt.Errorf("age capacity row %d: %w", playerID, err)
		}
		return nil
	}
	// 空 body:拿到守卫就直接提交,不产生任何写。
	noopBody := func(context.Context, *sql.Tx, map[uint64]uint32) error { return nil }

	var (
		sweptRows        atomic.Int64 // 回收实际删掉的行数(硬断言 > 0)
		unretriedMisses  atomic.Int64 // 严格组的对照步:不带重试时撞上缺行的次数(为 0 时用例以 SKIP 呈现)
		writersRemaining sync.WaitGroup
	)
	writersRemaining.Add(strictWriters + bothStaleWriters)
	writersDone := make(chan struct{})
	go func() {
		writersRemaining.Wait()
		close(writersDone)
	}()

	strictWriter := func(w int) error {
		defer writersRemaining.Done()
		p, q := pairOf(strictIDBase, w)
		stale := p
		if w%2 == 1 {
			stale = q
		}
		// step 把"先变陈旧、再走一条写路径"收成一步,并给错误标上是哪一步:
		// 并发用例红了之后,第一件想知道的事就是"哪条写路径、第几轮"。
		step := func(iteration int, name string, write func() error) error {
			if err := age(stale, staleCreatedMs); err != nil {
				return err
			}
			if err := write(); err != nil {
				return fmt.Errorf("严格组 writer %d 第 %d 轮 %s: %w", w, iteration+1, name, err)
			}
			return nil
		}
		for i := 0; i < iterations; i++ {
			if err := step(i, "AddFriend", func() error { return callAddFriend(ctx, repo, p, q, lim) }); err != nil {
				return err
			}
			if err := step(i, "AcceptFriend", func() error { return repo.AcceptFriend(ctx, p, q, lim.MaxFriends) }); err != nil {
				return err
			}
			if i == iterations-1 {
				// 最后一轮停在"已是好友":收尾时每个写者的两行 friend_count 都是 1,
				// 第 3 条(提交点复核)才有东西可验 —— 全员回到零好友的话,被误删的行不留痕迹。
				break
			}
			// 此刻两行 friend_count=1,而陈旧的那行多半还躺在某个回收者的候选名单里
			// (名单是它 friend_count 还是 0 的时候读的):DELETE 的 WHERE 复核必须挡住它。
			if err := step(i, "RemoveFriend", func() error { return repo.RemoveFriend(ctx, p, q) }); err != nil {
				return err
			}
			if err := step(i, "Block", func() error { return callBlock(ctx, repo, p, q, lim) }); err != nil {
				return err
			}
			// Unblock 不取守卫(单条 DELETE),与回收无关;调它只为让下一轮的 AddFriend 不被 ErrBlocked 挡住。
			if err := callUnblock(ctx, repo, p, q); err != nil {
				return fmt.Errorf("严格组 writer %d 第 %d 轮 Unblock: %w", w, i+1, err)
			}

			// 对照步:不带重试的单遍骨架。缺行在这里是**预期内**的结果,只计数。
			if err := age(stale, staleCreatedMs); err != nil {
				return err
			}
			err := repo.runGuardedWriteOnce(ctx, p, q, noopBody)
			switch {
			case err == nil:
			case errors.Is(err, errCapacityRowsMissing):
				unretriedMisses.Add(1)
			default:
				return fmt.Errorf("严格组 writer %d 第 %d 轮 对照步 runGuardedWriteOnce: %w", w, i+1, err)
			}
		}
		return nil
	}

	bothStaleWriter := func(w int) error {
		defer writersRemaining.Done()
		p, q := pairOf(bothStaleIDBase, w)
		// 严格组每轮约 5 次取守卫,这里按同样的总次数跑,两组大致同时收尾。
		for i := 0; i < iterations*5; i++ {
			if err := age(p, staleCreatedMs); err != nil {
				return err
			}
			if err := age(q, staleOlderCreateMs); err != nil {
				return err
			}
			// 任何 error 都原样(%w)上抛,不在这里吞:1213 交给收尾处的 assertNoDeadlock 点名,
			// errCapacityRowsMissing 交给紧随其后的缺行判定 —— 两行各至多被回收一次,三遍之内必过(见函数头)。
			if err := repo.runGuardedWrite(ctx, p, q, noopBody); err != nil {
				return fmt.Errorf("双陈旧组 writer %d 第 %d 次: %w", w, i+1, err)
			}
		}
		return nil
	}

	sweeper := func() error {
		for {
			select {
			case <-writersDone:
				return nil
			default:
			}
			// nowMs 取真实时刻(理由见函数头):只有被 age 过的行才早于截止点。
			// 不 sleep:每一圈至少有一次 MySQL 往返,本身就是让出点;而回收转得越勤,窗口越容易撞上。
			_, deleted, err := repo.SweepIdleCapacityRows(ctx, SweepModeDelete, retentionDays, batchLimit, time.Now().UnixMilli())
			sweptRows.Add(deleted)
			if err != nil {
				return fmt.Errorf("sweeper: %w", err)
			}
		}
	}

	errs := runConcurrent(t, strictWriters+bothStaleWriters+sweepers, func(i int) error {
		switch {
		case i < strictWriters:
			return strictWriter(i)
		case i < strictWriters+bothStaleWriters:
			return bothStaleWriter(i - strictWriters)
		default:
			return sweeper()
		}
	})

	// 第 1、2 条:无 1213;严格组、双陈旧组与回收都只许返回 nil。
	for i, err := range errs {
		assertNoDeadlock(t, err, "容量行回收与写路径并发")
		if errors.Is(err, errCapacityRowsMissing) {
			t.Fatalf("写路径在重试之后仍然缺行(goroutine %d;错误串里写着是哪一组):被 ensure 重建的行 created_ms 是"+
				"当前时刻、不可能再被回收 —— 严格组只有一行陈旧,第二遍必过;双陈旧组两行各至多被回收一次,"+
				"三遍(capacityGuardMaxAttempts)之内必过。所以这说明 runGuardedWrite 没有重试、重试遍数不足 3、"+
				"或重试前没有重新 ensure: %v", i, err)
		}
		if err != nil {
			t.Fatalf("容量行回收与写路径并发出现非预期错误(goroutine %d): %v", i, err)
		}
	}

	if sweptRows.Load() == 0 {
		t.Fatalf("回收在整个并发期间一行都没删:写者每一步都造了陈旧的零好友行,一行不删说明 " +
			"SweepIdleCapacityRows 的 delete 分支没有生效,本用例也就什么都没验到")
	}
	t.Logf("回收共删 %d 行;严格组对照步(不带重试的单遍骨架)撞上守卫缺行 %d 次 —— 为 0 表示本次运行没撞上"+
		"「ensure 之后、守卫之前」的窗口,对「重试生效」没有证明力(用例末尾会以 SKIP 呈现)",
		sweptRows.Load(), unretriedMisses.Load())

	// 第 3 条 + 计数一致性:收尾时严格组每个写者的一对都是好友,两行容量行必须都在、friend_count 都是 1。
	for w := 0; w < strictWriters; w++ {
		p, q := pairOf(strictIDBase, w)
		for _, id := range []uint64{p, q} {
			if got := mustCount(t, ctx, db,
				"SELECT COUNT(*) FROM friend_capacity WHERE player_id=? AND friend_count=1", id); got != 1 {
				t.Fatalf("严格组 writer %d:玩家 %d 有 1 个好友,却找不到 friend_count=1 的容量行(got %d 行)—— "+
					"回收删掉了有好友的行(DELETE 的 WHERE 漏了 friend_count = 0 的提交点复核),或计数漂移", w, id, got)
			}
		}
	}
	// 通用不变量兜底:每个有边的玩家都有容量行,且 friend_count == 边数。
	assertFriendInvariants(t, ctx, db)

	// 必须放在函数**最末**:Skip 会终止用例,放早了会把上面的硬断言一起跳掉。
	// assertFriendInvariants 用的是 assert(不终止),所以先看 t.Failed() —— 已经红了的用例不许被 SKIP 盖成"跳过"。
	if !t.Failed() && unretriedMisses.Load() == 0 {
		t.Skipf("其余硬断言均已通过;但本次运行未撞上「ensure 之后、守卫之前」窗口,对「重试生效」无证明力,请重跑")
	}
}

// ── 场景 (g):回收删行之后,并发 ensure 撞上同一条 delete-marked 主键记录 ──
//
// 回收上线之前 friend_capacity 从不删行:并发补行同一主键时,后到者拿到锁后看见的是**活的**重复键,
// 什么都不用改,成不了环。有了回收的 DELETE 之后出现了 delete-marked 记录,形状就变成 MySQL 手册
// "Deadlocks in InnoDB" 的那个例子 —— 前提是**至少两个 INSERT 同时排队等同一条被 X 锁住(或 delete-marked)
// 的主键记录**,压着 X 的一方可以是回收的 DELETE,也可以是另一个事务的守卫 FOR UPDATE:
//
//   - 补行若用 INSERT IGNORE:重复键检查取的是 **S**。X 一释放,等待者**同时**拿到 S;发现记录是已提交的删除
//     标记 → 不算重复 → 要就地复活 → 各自申请 X,而对方的 S 挡着 → 互等 → InnoDB 牺牲其一(1213)。
//     2026-09-21 真库复现过,当时靠 ensureFriendCapacityRows 的有上限重试吸收。
//   - 现在补行用 INSERT … ON DUPLICATE KEY UPDATE:重复键检查直接取 **X**,等待者只能一个一个拿到,
//     先到者复活记录并提交,后到者看见活行、走 no-op 更新。**根本不成环**,不再依赖重试。
//
// 所以本用例的判据比"两个 ensure 都成功"更严:ensureDeadlockRetries 必须一次都没涨。只断言成功的话,
// 有人把补行改回 INSERT IGNORE,重试依旧能把 1213 吸收成绿 —— 死锁回来了,测试却不红。
//
// # 为什么手工编排,而不是照本文件的惯例拿并发度去撞
//
// 这个环要求"两个等待者**同时**在队列里",撞出来的概率取决于机器快慢,满足不了"产品错了必然红"。
// 所以照手册的步骤一步一步摆:用独立事务执行回收那条 DELETE 且**先不提交**(持住 X)→ 起两个 ensure →
// 在 performance_schema.data_lock_waits 里**看见**两个等待者之后才提交。补行若是 INSERT IGNORE,
// 两个等待者必然同时拿到 S 并成环,重试计数必然涨;ODKU 下必然为 0。
// (InnoDB 的取锁细节是按手册与 row_ins_duplicate_error_in_clust 的已知行为推演的,以真库上跑出来的结果为准。)
//
// 等待者数量靠轮询观察,不靠 sleep 估时间:轮询间的短暂停顿只是不去空转打爆 MySQL,条件不满足就一直等到
// 预算用尽并判红,不会"睡够了就当它们已经在等"。
//
// 本用例读包级计数器 ensureDeadlockRetries,**不能**与其它会触发 ensure 的用例并行(不加 t.Parallel)。
func TestEnsureCapacityRows_ConcurrentInsertOnReclaimedRowSurvivesDeadlock(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const (
		player     uint64 = 88000001
		ensurers          = 2 // 手册原形就是两个等待者;更多等待者会连环牺牲几轮,那是重试上限要覆盖的事,不是本用例要证的
		waitBudget        = 10 * time.Second
	)

	// 1. 先有一行陈旧的零好友容量行(= 回收的合法候选)。
	if err := repo.ensureFriendCapacityRows(ctx, player); err != nil {
		t.Fatalf("夹具:首次 ensure 失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE friend_capacity SET created_ms = 1 WHERE player_id = ?", player); err != nil {
		t.Fatalf("夹具:把容量行改成陈旧行失败: %v", err)
	}

	// 2. 独立事务执行回收的那条 DELETE(语句文本与 sweep_repo.go 的 deleteIdleCapacityRow 逐字一致),先不提交。
	cutoffMs := uint64(time.Now().UnixMilli() - 24*time.Hour.Milliseconds())
	reclaimTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("夹具:开回收事务失败: %v", err)
	}
	// 任何一条提前失败的路径都必须放掉这把 X,否则两个 ensure 会一直挂到 ctx 超时。Commit 之后的 Rollback 无害。
	defer reclaimTx.Rollback()
	result, err := reclaimTx.ExecContext(ctx,
		"DELETE FROM friend_capacity WHERE player_id = ? AND friend_count = 0 AND created_ms < ?", player, cutoffMs)
	if err != nil {
		t.Fatalf("夹具:回收 DELETE 失败: %v", err)
	}
	if removed, err := result.RowsAffected(); err != nil || removed != 1 {
		t.Fatalf("夹具:回收 DELETE 应当恰好删 1 行(此时未提交、持有该主键的 X),got removed=%d err=%v", removed, err)
	}

	// 3. 两个 ensure 对同一玩家起跑:它们的补行 INSERT 会卡在重复键检查上(ODKU 等 X;若被改回 INSERT IGNORE 则等 S)。
	//
	// 末尾"行是重建出来的"那条断言的基准时刻必须取在**起 goroutine 之前**,不能等到第 5 步提交前再取:
	// ensureFriendCapacityRow 的 created_ms 是 ExecContext 的实参 time.Now().UnixMilli(),在 INSERT **发出之前**
	// 求值 —— 也就是该 goroutine 卡进锁等待之前。先拿到锁的一方落库的正是这个阻塞前的时刻;后到者撞上活行
	// 走 ODKU 的 no-op 更新(player_id = player_id),不会改写它。基准若取在"看见两个等待者之后",落库值必然早于基准,
	// 产品完全正确时断言也几乎必红。夹具已把旧行的 created_ms 直写成 1,所以这个更早的下界同样足以区分
	// "重建出来的行"与"DELETE 没生效、原样留着的旧行"。
	beforeEnsureMs := uint64(time.Now().UnixMilli())
	retriesBefore := ensureDeadlockRetries.Load()
	results := make(chan error, ensurers)
	for i := 0; i < ensurers; i++ {
		go func() { results <- repo.ensureFriendCapacityRows(ctx, player) }()
	}
	collect := func() []error {
		errs := make([]error, 0, ensurers)
		for i := 0; i < ensurers; i++ {
			errs = append(errs, <-results)
		}
		return errs
	}

	// 4. 等到两个等待者都**真的**排进了这张表的锁等待队列。
	if _, err := awaitLockWaiters(ctx, db, "friend_capacity", ensurers, waitBudget); err != nil {
		_ = reclaimTx.Rollback()
		collect()
		failOrSkipOnLockWaitError(t, err, "未提交的回收 DELETE 应当让每个补行 INSERT 都卡在重复键检查上")
	}

	// 5. 放掉 X。补行若是 INSERT IGNORE,两个等待者此刻同时拿到 S、成环;ODKU 下只会一个一个拿到 X。
	if err := reclaimTx.Commit(); err != nil {
		collect()
		t.Fatalf("夹具:提交回收事务失败: %v", err)
	}

	// 6. 两个 ensure 都必须成功。这里不用 assertNoDeadlock:它的文案讲的是守卫锁序,与本场景的成因无关。
	for i, err := range collect() {
		if isInnoDBDeadlock(err) {
			t.Fatalf("第 %d 个 ensure 返回 InnoDB 死锁(1213),且连重试上限都用尽了:补行在同一条 delete-marked "+
				"主键记录上互等(见本场景头注)—— %v", i+1, err)
		}
		if err != nil {
			t.Fatalf("第 %d 个 ensure 出现非预期错误: %v", i+1, err)
		}
	}
	// 比"都成功"更严的判据:一次 1213 都不许发生。成功可能只是重试把死锁吸收掉了。
	if retried := ensureDeadlockRetries.Load() - retriesBefore; retried != 0 {
		t.Fatalf("两个 ensure 虽然最终成功,但期间撞了 %d 次 InnoDB 死锁(1213)、靠重试才过:补行又拿回了 S 锁"+
			"(被改回 INSERT IGNORE?)或会话隔离级别不是 RC —— 见 friend_repo.go ensureFriendCapacityRow 与 svc.BuildDSN", retried)
	}
	if got := mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_capacity WHERE player_id=?", player); got != 1 {
		t.Fatalf("ensure 全部成功之后容量行必须在,got %d 行", got)
	}
	// 该玩家没有好友边,权威边数就是 0;用子查询而不是字面量 0,免得将来夹具加了边之后这条断言悄悄失真。
	if got := mustCount(t, ctx, db, `
		SELECT COUNT(*) FROM friend_capacity c
		WHERE c.player_id = ? AND c.friend_count = (SELECT COUNT(*) FROM friend f WHERE f.player_id = c.player_id)`,
		player); got != 1 {
		t.Fatalf("重建出来的容量行 friend_count 必须等于 friend 表的权威边数")
	}
	// 行确实是删掉之后**重建**的(created_ms 是新的),而不是回收那条 DELETE 根本没生效、ensure 全程空操作 ——
	// 后一种情况下上面的断言同样全绿,但什么都没验到。下界是起 ensure 之前取的时刻(为什么不能更晚,见第 3 步):
	// 旧行的 created_ms 是夹具直写的 1,重建行的是 ensure 自己取的当前毫秒时间戳,这个下界足以把两者分开。
	if created := readCapacityCreatedMs(t, ctx, db, player); created < beforeEnsureMs {
		t.Fatalf("容量行的 created_ms=%d 早于起 ensure 之前取的时刻 %d(夹具旧行是 1):这一行不是重建出来的,"+
			"本场景没有真的走到 delete-marked 分支", created, beforeEnsureMs)
	}
	assertFriendInvariants(t, ctx, db)
}

// ── 手工编排场景 (g)(h) 的公共夹具 ──

// lockWaitersQuery 数本库里正在排队等某张表上行锁的事务数。只看当前库、指定表:
// 同一个 MySQL 实例上别的库、别的表的锁等待不算数。
const lockWaitersQuery = `
	SELECT COUNT(DISTINCT w.REQUESTING_ENGINE_TRANSACTION_ID)
	FROM performance_schema.data_lock_waits w
	JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
	WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = ?`

// errLockWaitsUnobservable:测试账号读不了 performance_schema 的锁视图,或实例上根本没有这两张表。
// 这时手工编排做不出来 —— 那不是产品缺陷,由 failOrSkipOnLockWaitError 按验收模式决定跳过还是判红。
// 只有 isLockWaitsUnobservable 认定的错误号才归到这里,见 lockWaitsQueryError。
var errLockWaitsUnobservable = errors.New("读 performance_schema.data_lock_waits 失败")

// awaitLockWaiters 轮询,直到 table 上至少有 want 个事务在排队等行锁,或 budget 用尽(返回错误)。
// 靠轮询**观察**,不靠 sleep 估时间:两次轮询之间的短停顿只是不去空转打爆 MySQL,
// 条件不满足就一直等到预算用尽并报错,不会"睡够了就当它们已经在等"。
// 查询失败时的定性见 lockWaitsQueryError:只有权限 / 对象缺失类错误算"不可观测",ctx 结束与其余错误都判红。
func awaitLockWaiters(ctx context.Context, db *sql.DB, table string, want int, budget time.Duration) (int, error) {
	const pollEvery = 10 * time.Millisecond
	deadline := time.Now().Add(budget)
	for {
		var waiters int
		if err := db.QueryRowContext(ctx, lockWaitersQuery, table).Scan(&waiters); err != nil {
			return 0, lockWaitsQueryError(ctx, err)
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

// lockWaitsQueryError 给 lockWaitersQuery 的失败定性:是"环境看不见锁等待"(可按模式跳过),还是"编排出错"(判红)。
// 2026-09-21 复审前这里把一切查询错误都包成 errLockWaitsUnobservable,连 ctx 超时 / 取消也算,
// 于是用例卡住会被报成 SKIP。现在:
//   - 用例 ctx 已结束(预算用尽 / 被取消):带出 ctx 错误(errors.Is 可认出 context.DeadlineExceeded / Canceled),判红。
//     这时查询失败只是结果,原因是编排卡住或超了预算。先看 ctx.Err() 而不是先看错误本身,是因为 ctx 结束时驱动
//     报出来的形态不固定(可能是 ctx 错误,也可能是 invalid connection 之类),以 ctx 的状态为准。
//   - MySQL 权限 / 对象缺失类错误(isLockWaitsUnobservable):归为 errLockWaitsUnobservable。
//   - 其余一律判红:断连、SQL 写错、服务端内部错误都说明编排本身坏了。
//
// 与 go/trade/internal/data/listing_repo_integration_test.go 的同名助手同一口径(代码只差驱动包的导入别名),
// 改一处要同步另一处。
func lockWaitsQueryError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("轮询锁等待时用例 ctx 已结束(编排卡住或超出预算,不是读不了 performance_schema): %w; 查询错误: %v", ctxErr, err)
	}
	if isLockWaitsUnobservable(err) {
		return fmt.Errorf("%w: %v", errLockWaitsUnobservable, err)
	}
	return fmt.Errorf("查询 performance_schema 锁等待失败,且不是权限 / 对象缺失类错误,按编排失败判红: %w", err)
}

// isLockWaitsUnobservable 只按驱动给出的 MySQL 错误号判定"账号或实例不支持观察锁等待" —— 都是环境问题,
// 与被测的锁行为无关。刻意不匹配错误文本,也不把"查询失败"整体归进来。
// 注意 performance_schema=OFF 时表仍在、只是恒为空,查询不报错,会走到 awaitLockWaiters 的"等待者没到齐"而判红;
// 本仓的 MySQL 8 默认开启,真遇到时先检查 SELECT @@performance_schema。
func isLockWaitsUnobservable(err error) bool {
	var myErr *drivermysql.MySQLError
	if !errors.As(err, &myErr) {
		return false
	}
	switch myErr.Number {
	case 1044, // ER_DBACCESS_DENIED_ERROR:对 performance_schema 库整体无权
		1142, // ER_TABLEACCESS_DENIED_ERROR:对 data_lock_waits / data_locks 没有 SELECT 权限
		1143, // ER_COLUMNACCESS_DENIED_ERROR:只授了部分列的 SELECT 权限
		1146, // ER_NO_SUCH_TABLE:实例没有这两张表(MySQL 8.0 以前、MariaDB 等)
		1227: // ER_SPECIFIC_ACCESS_DENIED_ERROR:缺某项全局权限
		return true
	}
	return false
}

// failOrSkipOnLockWaitError 处理 awaitLockWaiters 的错误。读不了 performance_schema 时:验收模式
// (FRIEND_REQUIRE_MYSQL_TESTS)下判红 —— (g)(h) 是这两类死锁唯一的确定性证据,不许静默跳过;
// 否则跳过(不代表通过)。其余错误(等待者没到齐、ctx 结束、非权限类查询错误)一律判红:编排失败本身就说明
// 锁行为与推演不符,或者用例卡住了。
func failOrSkipOnLockWaitError(t *testing.T, err error, expectation string) {
	t.Helper()
	if errors.Is(err, errLockWaitsUnobservable) {
		if os.Getenv(friendRequireMySQLEnv) != "" {
			t.Fatalf("无法编排本场景(测试账号需要 performance_schema 的 SELECT 权限): %v", err)
		}
		t.Skipf("无法编排本场景,跳过(不代表通过): %v", err)
	}
	t.Fatalf("夹具编排失败:%v —— %s", err, expectation)
}

// ── 场景 (h):Block ④ 在"被未提交删除压住"的 friend_block 记录上,与排队的 Unblock 不成环 ──
//
// Unblock 不拿守卫(自动提交的单条 DELETE),所以同一对 (me, target) 上除了 Block 自己,能来排队的只有 Unblock
// (第二个 Block 会先被守卫挡住)。2026-09-21 死锁审计推出的交错:Unblock#2 持 X 正在删 (me,target) →
// Block 走到 ④、在这条记录上等 → Unblock#3 也来排队等 X → Unblock#2 提交。
//   - ④ 若是 INSERT IGNORE:重复键检查取 **S**。Block 先拿到 S(Unblock#3 的 X 与之冲突,继续等);记录是已提交的
//     删除标记,复活它要升 **X**,却得排在 Unblock#3 等待中的 X 后面,而 Unblock#3 在等 Block 的 S → 1213,
//     且 body 的 1213 不重试,直接变成玩家可见的 ErrStorage。
//   - ④ 现在是 ODKU(insertBlockRowSQL):重复键检查直接取 **X**,不存在升级,只会排队。
//
// # 为什么直接执行 ④ 的 SQL 常量,而不是整条调用 repo.Block
//
// 真实交错要求 Unblock#2 恰好落在 Block 的 ② 与 ④ 之间(② 若撞上 Unblock#2 的 X,RC 下等到之后看见的是删除标记、
// 立即放锁,不参与成环),产品代码里没有能停在 ②④ 之间的钩子,整条调用只能按概率撞。环只取决于 ④ 这一条语句的
// 锁模式,所以本用例在一个 RC 事务里只执行 insertBlockRowSQL(与生产同一个常量,不另抄一份)来扮演"已走到 ④
// 的 Block":锁行为与在 Block 事务里执行时完全相同,时序则可以确定地编排。
//
// 判据的强弱要说清楚:改回 INSERT IGNORE 后是否一定变红,取决于 Unblock#2 提交时 InnoDB 先放锁给谁 —— 先放 ④ 的 S
// 才成环;MySQL 8 的 CATS 按事务权重而不是到达顺序放锁,所以旧写法下本用例**按推演会红,但不保证每次都红**。
// 新写法下两者都成功是确定的。
//
// 编排:夹具先 Block 出一行活的 (me,target) → 独立事务执行 Unblock 的 DELETE 且**先不提交**(持 X)→
// "④ 事务"执行 insertBlockRowSQL(排队)→ 看见 1 个等待者之后才起 repo.Unblock(排在它后面)→
// 看见 2 个等待者 → 提交 Unblock#2 → 两者都必须成功。
func TestBlockInsertOnRowUnderPendingDeleteDoesNotDeadlockWithQueuedUnblock(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const (
		me         uint64 = 88100001
		target     uint64 = 88100002
		waitBudget        = 10 * time.Second
	)

	if err := repo.Block(ctx, me, target, 0); err != nil {
		t.Fatalf("夹具:先拉黑一次造出活行失败: %v", err)
	}

	// Unblock#2:与 Unblock 同一条语句,放进独立事务、先不提交。任何提前失败的路径都必须放掉这把 X,
	// 否则后面两个等待者会一直挂到 ctx 超时。Commit 之后的 Rollback 无害。
	unblock2, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("夹具:开 Unblock#2 事务失败: %v", err)
	}
	defer unblock2.Rollback()
	res, err := unblock2.ExecContext(ctx,
		"DELETE FROM friend_block WHERE player_id=? AND blocked_player_id=?", me, target)
	if err != nil {
		t.Fatalf("夹具:Unblock#2 的 DELETE 失败: %v", err)
	}
	if removed, err := res.RowsAffected(); err != nil || removed != 1 {
		t.Fatalf("夹具:Unblock#2 应当恰好删 1 行(此时未提交、持有该主键的 X),got removed=%d err=%v", removed, err)
	}

	// "已走到 ④ 的 Block":与 Block 的写事务同一隔离级,执行 ④ 的生产常量,成功则提交。
	blockTx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: friendWriteTxIsolation})
	if err != nil {
		t.Fatalf("夹具:开 ④ 事务失败: %v", err)
	}
	defer blockTx.Rollback()
	blockDone := make(chan error, 1)
	go func() {
		_, err := blockTx.ExecContext(ctx, insertBlockRowSQL, me, target, time.Now().UnixMilli())
		if err == nil {
			err = blockTx.Commit()
		}
		blockDone <- err
	}()
	// 先确认 ④ 已经在排队,再放 Unblock#3 进来:"④ 排在前、Unblock#3 排在后"正是这个环的前提。
	if _, err := awaitLockWaiters(ctx, db, "friend_block", 1, waitBudget); err != nil {
		_ = unblock2.Rollback()
		<-blockDone
		failOrSkipOnLockWaitError(t, err, "未提交的 DELETE 应当让 ④ 卡在重复键检查上")
	}

	unblock3Done := make(chan error, 1)
	go func() { unblock3Done <- repo.Unblock(ctx, me, target) }()
	if _, err := awaitLockWaiters(ctx, db, "friend_block", 2, waitBudget); err != nil {
		_ = unblock2.Rollback()
		<-blockDone
		<-unblock3Done
		failOrSkipOnLockWaitError(t, err, "Unblock#3 应当排在 ④ 之后等同一条记录")
	}

	// 放掉 X:旧写法下成环的时刻就在这里。
	if err := unblock2.Commit(); err != nil {
		<-blockDone
		<-unblock3Done
		t.Fatalf("夹具:提交 Unblock#2 失败: %v", err)
	}

	for _, c := range []struct {
		what string
		err  error
	}{{"④(Block 写黑名单)", <-blockDone}, {"Unblock#3", <-unblock3Done}} {
		if isInnoDBDeadlock(c.err) {
			t.Fatalf("%s 返回 InnoDB 死锁(1213):④ 在删除标记记录上 S→X 升级、排在 Unblock#3 等待中的 X 后面"+
				"(insertBlockRowSQL 被改回 INSERT IGNORE 了?见本场景头注)—— %v", c.what, c.err)
		}
		if c.err != nil {
			t.Fatalf("%s 出现非预期错误: %v", c.what, c.err)
		}
	}
	// 终态取决于 InnoDB 先放锁给谁(④ 先:插入后被 Unblock#3 删掉;Unblock#3 先:删空后 ④ 再插入),
	// 两种都合法,所以不断言有没有这一行,只断言全局不变量。
	assertFriendInvariants(t, ctx, db)
}

// TestLockingStatementsArePrimaryKeyPointLookups 钉住 friend_repo.go 顶部锁序说明 (6):
// 守卫之后的每一条锁定语句,执行计划都必须是**完整主键的等值点查 / 点更新**。
//
// 为什么需要这条确定性用例(并发场景已经有了):2026-09-21 首次真库回归里,blockedEitherWay /
// friendEdgeExistsForUpdate 原先的 `(a,b) OR (b,a) ... FOR UPDATE` 被规划成二级覆盖索引全扫描,锁到
// 别的玩家对的行上,与那一对的 Unblock / 删边 DELETE 反序成环(1213);lockCapacityRows 原先的
// `IN (...) FOR UPDATE` 被规划成 PRIMARY 全索引扫描;Block 取消 pending 的 OR 形 UPDATE 走 status 前缀、
// 扫全服 pending 行。这类问题**只取决于执行计划**,并发用例只能按概率撞上,而 EXPLAIN 每次都答得出来。
//
// 做法:对生产代码里**同一个 SQL 常量**做 EXPLAIN(测试里不另抄一份,否则两边漂移时本用例就失去意义),
// 断言 key = PRIMARY 且用满全部主键列(key_len),SELECT 的 access type 必须是 const。
// 先各插一行:点查命中已存在的行时计划才显示 const;表空时 MySQL 只报 "no matching row in const table"。
//
// 把哪一条改回 OR / IN / 前缀范围,本用例就会稳定变红。
func TestLockingStatementsArePrimaryKeyPointLookups(t *testing.T) {
	db, ctx := openFriendTestDB(t)

	for _, seed := range []string{
		"INSERT INTO friend_capacity (player_id, friend_count, created_ms) VALUES (91001, 0, 1), (91002, 0, 1)",
		"INSERT INTO friend_block (player_id, blocked_player_id, since_ms) VALUES (91001, 91002, 1)",
		"INSERT INTO friend (player_id, friend_player_id, since_ms) VALUES (91001, 91002, 1)",
		"INSERT INTO friend_request (from_player_id, to_player_id, request_time_ms, status, updated_ms) VALUES (91001, 91002, 1, 1, 1)",
	} {
		if _, err := db.ExecContext(ctx, seed); err != nil {
			t.Fatalf("种子数据写入失败 %q: %v", seed, err)
		}
	}

	// BIGINT UNSIGNED 占 8 字节:单列主键 key_len=8,两列主键 key_len=16。
	cases := []struct {
		name       string
		sql        string
		args       []any
		wantTypes  []string // UPDATE 按主键点更新时 MySQL 显示 range(rows=1),SELECT 点查是 const
		wantKeyLen string
	}{
		{"lockCapacityRowSQL", lockCapacityRowSQL, []any{91001}, []string{"const"}, "8"},
		{"lockBlockRowSQL", lockBlockRowSQL, []any{91001, 91002}, []string{"const"}, "16"},
		{"lockFriendEdgeRowSQL", lockFriendEdgeRowSQL, []any{91001, 91002}, []string{"const"}, "16"},
		{"cancelPendingRequestSQL", cancelPendingRequestSQL,
			[]any{requestStatusRejected, 1, 91001, 91002, requestStatusPending}, []string{"range", "const"}, "16"},
		// sweep 清理终态申请:逐行按主键删(sweep_repo.go 的 deleteTerminalRequestsBefore)。
		{"deleteTerminalRequestSQL", deleteTerminalRequestSQL,
			[]any{91001, 91002, requestStatusAccepted, requestStatusRejected, 2000000000000}, []string{"range", "const"}, "16"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainTraditional(t, ctx, db, inlineNumericArgs(t, tc.sql, tc.args...))
			if plan["key"] != "PRIMARY" || plan["key_len"] != tc.wantKeyLen {
				t.Fatalf("%s 的执行计划没有用满主键:key=%q key_len=%q(期望 PRIMARY / %s),type=%q。"+
					"锁定语句必须是完整主键的等值条件,否则锁集由优化器决定、会越出这一对玩家(锁序说明 (6))。SQL: %s",
					tc.name, plan["key"], plan["key_len"], tc.wantKeyLen, plan["type"], tc.sql)
			}
			ok := false
			for _, want := range tc.wantTypes {
				if plan["type"] == want {
					ok = true
				}
			}
			if !ok {
				t.Fatalf("%s 的 access type=%q,期望 %v(全索引扫描 index / ALL 会锁到别的玩家对的行)。SQL: %s",
					tc.name, plan["type"], tc.wantTypes, tc.sql)
			}
		})
	}
}

// explainTraditional 跑一条 EXPLAIN FORMAT=TRADITIONAL,返回第一行的列名 → 值(NULL 记为空串)。
// 这几条语句都只涉及一张表,第一行就是它的计划。
func explainTraditional(t *testing.T, ctx context.Context, db *sql.DB, stmt string) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, "EXPLAIN FORMAT=TRADITIONAL "+stmt)
	if err != nil {
		t.Fatalf("EXPLAIN 失败 %q: %v", stmt, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("读取 EXPLAIN 列名失败: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("EXPLAIN 没有返回任何行: %q", stmt)
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("读取 EXPLAIN 结果失败: %v", err)
	}
	plan := make(map[string]string, len(cols))
	for i, c := range cols {
		plan[c] = vals[i].String
	}
	return plan
}

// inlineNumericArgs 把 SQL 里的 ? 依次替换成数字字面量,供 EXPLAIN 使用。
// 只接受整数参数:这几条语句的参数全是 id / 状态码 / 毫秒时间戳,出现别的类型说明用法错了。
func inlineNumericArgs(t *testing.T, query string, args ...any) string {
	t.Helper()
	if strings.Count(query, "?") != len(args) {
		t.Fatalf("占位符 %d 个,参数 %d 个: %s", strings.Count(query, "?"), len(args), query)
	}
	for _, a := range args {
		switch a.(type) {
		case int, int32, int64, uint32, uint64:
		default:
			t.Fatalf("inlineNumericArgs 只接受整数参数,得到 %T", a)
		}
		query = strings.Replace(query, "?", fmt.Sprint(a), 1)
	}
	return query
}
