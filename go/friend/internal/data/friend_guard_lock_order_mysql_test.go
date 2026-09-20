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
// 公共夹具(门控、schema、不变量断言、isInnoDBDeadlock)在 friend_repo_mysql_test.go。

import (
	"errors"
	"sync"
	"testing"
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
