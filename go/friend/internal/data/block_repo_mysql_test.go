package data

// block_repo_mysql_test.go —— 黑名单(规格 §3.4)的真实 InnoDB 回归。
//
// Block 是本批唯一一个"一个事务里改三张表"的写路径(friend_block 插入、friend 双向删边并
// 减计数、friend_request 两个方向置终态)。三件事里任何一件漏做都不会报错,只会让玩家看到
// 一个说不通的状态,所以这里逐件钉死:
//
//	⑤ 删边必须减计数 —— 漏减则 friend_count 永久偏高,玩家"永远加不满好友"且零报错;
//	⑥ 两个方向的 pending 都要取消 —— 只清一个方向会留下"我拉黑了他,他的申请还在我的收件箱"。
//
// 幂等也必须验:客户端重试、双击、Kafka 重投都会让 Block 被调第二次。第二次若占掉一个
// 新名额,玩家的黑名单上限会被自己的重试吃光。
//
// 公共夹具(门控、schema、seed*、不变量断言)在 friend_repo_mysql_test.go。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlock_IsIdempotentAndDoesNotConsumeExtraSlot(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const me, target uint64 = 31001, 31002
	lim := defaultTestLimits()
	lim.MaxBlocks = 1 // 名额恰好 1:重复拉黑若占新名额,第二次必然报满 —— 这条才有鉴别力

	require.NoError(t, callBlock(ctx, repo, me, target, lim))
	require.NoError(t, callBlock(ctx, repo, me, target, lim),
		"重复拉黑同一人必须幂等成功:客户端重试 / 双击会走到这里")

	assert.Equal(t, int64(1), mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_block WHERE player_id=?", me),
		"重复拉黑不得产生第二行,也不得占掉第二个名额")
	assertFriendInvariants(t, ctx, db)
}

func TestBlock_EnforcesMaxBlocks(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const me uint64 = 32001
	lim := defaultTestLimits()
	lim.MaxBlocks = 2

	require.NoError(t, callBlock(ctx, repo, me, 32101, lim))
	require.NoError(t, callBlock(ctx, repo, me, 32102, lim))
	assert.ErrorIs(t, callBlock(ctx, repo, me, 32103, lim), ErrBlockListFull,
		"黑名单必须有硬上限:没有上限时客户端可用互不相同的 target 把 friend_block 无界撑大")

	assert.Equal(t, int64(2), mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_block WHERE player_id=?", me),
		"被拒的那次不得落库")
	// 上限只约束"我拉黑了多少人",不约束"多少人拉黑了我" —— 后者不由我控制,
	// 若被算进同一个上限,一个玩家就能靠拉黑别人来消耗别人的名额。
	require.NoError(t, callBlock(ctx, repo, 32104, me, lim))
	assertFriendInvariants(t, ctx, db)
}

// TestBlock_DeletesFriendEdgesAndDecrementsCount 钉规格 §3.4 ⑤。
//
// 这是本文件最重要的一条:删边而不减计数在库里留下的痕迹只有 friend_count 偏大,
// 没有任何日志、没有任何错误码,玩家的症状是"好友位显示还有空位却加不上人"。
func TestBlock_DeletesFriendEdgesAndDecrementsCount(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const me, target, other uint64 = 33001, 33002, 33003
	lim := defaultTestLimits()

	// 先把 me 与两个人都变成好友:留一个无关好友(other)在场,
	// 才能分辨"减了 1"和"把计数清成 0"这两种错。
	seedPending(t, ctx, db, target, me)
	require.NoError(t, repo.AcceptFriend(ctx, target, me, lim.MaxFriends))
	seedPending(t, ctx, db, other, me)
	require.NoError(t, repo.AcceptFriend(ctx, other, me, lim.MaxFriends))
	require.Equal(t, int64(2), mustCount(t, ctx, db,
		"SELECT friend_count FROM friend_capacity WHERE player_id=?", me))

	require.NoError(t, callBlock(ctx, repo, me, target, lim))

	// 双向边都要没:只删一个方向会让对方的好友列表里还留着我。
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend WHERE player_id=? AND friend_player_id=?", me, target))
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend WHERE player_id=? AND friend_player_id=?", target, me))

	// 两侧计数都要减 1(按各自的 RowsAffected),而不是只减发起方。
	assert.Equal(t, int64(1), mustCount(t, ctx, db,
		"SELECT friend_count FROM friend_capacity WHERE player_id=?", me),
		"发起方的 friend_count 必须随删边减 1(§3.4 ⑤)")
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT friend_count FROM friend_capacity WHERE player_id=?", target),
		"被拉黑方的 friend_count 也必须减 1 —— 他丢了一条边")
	// 无关好友一条都不许被牵连。
	assert.Equal(t, int64(1), mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend WHERE player_id=? AND friend_player_id=?", me, other))

	// 幂等第二次:已经没有边了,RowsAffected==0,计数**不得**再减(否则会减成负数 / 被夹到 0,
	// 与真实边数脱节)。
	require.NoError(t, callBlock(ctx, repo, me, target, lim))
	assert.Equal(t, int64(1), mustCount(t, ctx, db,
		"SELECT friend_count FROM friend_capacity WHERE player_id=?", me),
		"没有边可删时不得再减计数(必须按 RowsAffected 决定)")
	assertFriendInvariants(t, ctx, db)
}

// TestBlock_CancelsPendingInBothDirections 钉规格 §3.4 ⑥。
func TestBlock_CancelsPendingInBothDirections(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const me, target, bystander uint64 = 34001, 34002, 34003
	lim := defaultTestLimits()

	seedPending(t, ctx, db, target, me) // 他申请我
	seedPending(t, ctx, db, me, target) // 我也申请过他
	seedPending(t, ctx, db, bystander, me)

	// seedPending 直写的 updated_ms 是"现在"(非零)。不先归零的话,下面两条 NotZero 断言
	// 结构性不可能失败 —— Block 的 ⑥ 哪怕漏写 updated_ms 也照绿(handoff §3 第 8 条登记的假绿)。
	// 手法照 TestAddFriend_WritesUpdatedMs:两个方向都直写归零,再看状态迁移是否**重新**写上。
	_, err := db.ExecContext(ctx,
		`UPDATE friend_request SET updated_ms=0
		 WHERE (from_player_id=? AND to_player_id=?) OR (from_player_id=? AND to_player_id=?)`,
		target, me, me, target)
	require.NoError(t, err)
	require.Zero(t, readRequestUpdatedMs(t, ctx, db, target, me), "前置条件:归零必须真的生效,否则下面的 NotZero 没有鉴别力")
	require.Zero(t, readRequestUpdatedMs(t, ctx, db, me, target), "前置条件:归零必须真的生效,否则下面的 NotZero 没有鉴别力")

	require.NoError(t, callBlock(ctx, repo, me, target, lim))

	// 断言**精确值**而不是"不等于 pending":置成 accepted(2) 同样能过 NotEqual,
	// 却会让被拉黑的一对在历史里显示成"申请已通过"。
	assert.Equal(t, testStatusRejected, readRequestStatus(t, ctx, db, target, me),
		"他发给我的 pending 必须被置成 rejected(3);置成 accepted(2)会让被拉黑的一对在历史里显示成『申请已通过』")
	assert.Equal(t, testStatusRejected, readRequestStatus(t, ctx, db, me, target),
		"我发给他的 pending 同样必须是 rejected(3)")
	// 终态行要写 updated_ms,否则 sweep 永远回收不掉它们(sweep 按 (status, updated_ms) 过滤)。
	assert.NotZero(t, readRequestUpdatedMs(t, ctx, db, target, me),
		"Block 取消 pending 时必须写 updated_ms,否则 sweep 的保留期过滤失效")
	assert.NotZero(t, readRequestUpdatedMs(t, ctx, db, me, target))
	// 无关第三方的申请不许被连带取消。
	assert.Equal(t, int64(1), readRequestStatus(t, ctx, db, bystander, me),
		"只取消这一对之间的 pending,别人的申请不受影响")
	assertFriendInvariants(t, ctx, db)
}

// TestBlock_InvalidatesCachedFriendList:Block 提交后必须失效双方的好友列表缓存。
//
// 漏了这一步,被拉黑的人会在缓存 TTL(默认 30 分钟)内继续出现在好友列表里 ——
// 玩家点了拉黑、界面没反应,而服务端"一切正常"。
func TestBlock_InvalidatesCachedFriendList(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, mr := newFriendTestRepo(t, db)

	const me, target uint64 = 35001, 35002
	lim := defaultTestLimits()
	seedPending(t, ctx, db, target, me)
	require.NoError(t, repo.AcceptFriend(ctx, target, me, lim.MaxFriends))

	// 先把缓存烘热,并确认它真的写进去了(否则下面"缓存不见了"这条断言恒真 = 永远绿)。
	warm, err := repo.GetFriendList(ctx, me)
	require.NoError(t, err)
	require.Len(t, warm, 1)
	require.True(t, mr.Exists(friendListKey(me)), "前置条件:缓存必须已被回填,否则本用例没有鉴别力")

	require.NoError(t, callBlock(ctx, repo, me, target, lim))
	assert.False(t, mr.Exists(friendListKey(me)), "Block 提交后必须失效发起方的好友列表缓存")
	assert.False(t, mr.Exists(friendListKey(target)), "被拉黑方的缓存同样要失效 —— 他的列表里也少了一个人")

	after, err := repo.GetFriendList(ctx, me)
	require.NoError(t, err)
	assert.Empty(t, after, "失效之后重新读必须看到 MySQL 的新事实")
}

func TestUnblock_IsIdempotentAndDoesNotRestoreFriendship(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const me, target uint64 = 36001, 36002
	lim := defaultTestLimits()
	seedPending(t, ctx, db, target, me)
	require.NoError(t, repo.AcceptFriend(ctx, target, me, lim.MaxFriends))
	require.NoError(t, callBlock(ctx, repo, me, target, lim))

	require.NoError(t, callUnblock(ctx, repo, me, target))
	require.NoError(t, callUnblock(ctx, repo, me, target), "解除拉黑必须幂等")
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_block WHERE player_id=? AND blocked_player_id=?", me, target))

	// 解除拉黑**不**恢复好友关系:关系是双方各自的意思,不能由一次解除单方面重建。
	assert.Zero(t, mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend"),
		"Unblock 不得恢复被 Block 删掉的好友边")
	assert.Zero(t, mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_request WHERE status=1"),
		"Unblock 不得复活被取消的 pending 申请")

	// 解除之后可以重新走正常流程。
	require.NoError(t, callAddFriend(ctx, repo, me, target, lim))
	assert.Equal(t, int64(1), readRequestStatus(t, ctx, db, me, target))
	assertFriendInvariants(t, ctx, db)
}

// TestBlock_RejectsSelf:拉黑自己没有任何语义,而且一旦落库就会同时踩两条不变量
// ("既是好友又拉黑"的自反情形、以及 §3.4 ⑤ 的自删边)。
//
// 这里只断言"不落库"而不断言具体错误码:错误码的映射是 logic 层的职责
// (那里回 constants.ErrInvalidParameter,见 internal/logic 的用例)。
func TestBlock_RejectsSelf(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const me uint64 = 37001
	lim := defaultTestLimits()
	_ = callBlock(ctx, repo, me, me, lim) // 报错或幂等无操作都可以,但不许落库
	assert.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_block WHERE player_id=?", me),
		"拉黑自己不得落库")
	assertFriendInvariants(t, ctx, db)
}

// TestListBlocks_AppliesHardLimit 钉 F2-7 在黑名单读上的那一半。
//
// 列表读没有 LIMIT 时,一个被撑大的黑名单会一次性拖回整张表并塞进一个 gate 单包
// (1KB 量级),表现为该玩家每次打开界面都超时 —— 且服务端指标上只是"慢",不报错。
func TestListBlocks_AppliesHardLimit(t *testing.T) {
	db, ctx := openFriendTestDB(t)

	const me uint64 = 38001
	const rows = 7
	for i := 0; i < rows; i++ {
		seedBlock(t, ctx, db, me, uint64(38100+i))
	}

	// 上限由 NewFriendRepo 时交给 repo 保管(不是逐调用传),所以要验限幅只能换一个 repo。
	roomy, _ := newFriendTestRepoWithLimit(t, db, rows)
	all, err := callListBlocks(ctx, roomy, me)
	require.NoError(t, err)
	assert.Len(t, all, rows, "上限大于等于实际行数时必须全部返回")

	const hardLimit uint32 = 3
	tight, _ := newFriendTestRepoWithLimit(t, db, hardLimit)
	capped, err := callListBlocks(ctx, tight, me)
	require.NoError(t, err)
	assert.Len(t, capped, int(hardLimit),
		"必须真的做了限幅(一个被撑大的黑名单不能一次性拖回整张表)。"+
			"⚠ 这条**区分不了** SQL 里的 LIMIT 与 Go 里的截断 —— 两种实现的返回条数一样,"+
			"要区分只能看 EXPLAIN。别在注释里声称这里测到了 SQL 侧限幅")

	// 只列我拉黑的人,不列拉黑我的人:反向也返回的话,玩家会在自己的黑名单里
	// 看到一个自己没拉黑过、也解除不掉的人。
	//
	// ⚠ 必须换一个上限**远大于**预置行数的 repo:用 roomy(上限恰好 = rows)时,
	// 一个错误的双向实现返回 8 行也会被 LIMIT 截成 7 行,Len 断言恒绿。
	seedBlock(t, ctx, db, 38999, me)
	wide, _ := newFriendTestRepoWithLimit(t, db, 100)
	mine, err := callListBlocks(ctx, wide, me)
	require.NoError(t, err)
	assert.Len(t, mine, rows, "ListBlocks 只返回 player_id = me 的行")
	for _, b := range mine {
		assert.NotEqual(t, uint64(38999), b.BlockedPlayerID,
			"拉黑我的人不得出现在我的黑名单里(玩家会看到一个自己没拉黑过、也解除不掉的人)")
	}
}
