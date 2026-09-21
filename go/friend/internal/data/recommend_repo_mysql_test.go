package data

// recommend_repo_mysql_test.go —— 好友推荐三段 SQL 的真实 MySQL 回归(handoff §3 第 6b 条)。
//
// # 为什么必须打真库
//
// recommend_repo.go 的四类业务排除(自己 / 已是好友 / 任一方向拉黑 / 任一方向仍 pending 的申请)
// **内联写在两条 query 字符串里、各一份**,列名还不一样:RecommendByMutual 用 `f2.friend_player_id`,
// recommendAnchor 在两处 NOT EXISTS 里必须写全限定的 `friend.player_id`(裸 `player_id` 会解析到
// 子查询自己的表上,条件变成"自己拉黑自己",排除静默失效)。拼错列名在 Go 侧完全静态无感,
// 编译、vet、mock 都看不见 —— 只有让 MySQL 真的解析并执行这两条 SQL 才验得到。
// 所以两条 query **分别**验一遍,不因为"长得一样"就只验一条。
//
// # 每一类排除都要造出"子句失效就会露馅"的数据
//
// robot 冒烟的"推荐不含已拉黑的 C"在三账号数据集下结构性不可能失败(C 本来就不在候选池里),
// 那是已登记的假绿,这里不许重蹈。本文件的纪律:
//
//   - 每个"应被排除的人"都**同时满足成为候选的全部条件**(mutual:是我好友的好友;anchor:在 friend 表里
//     有出边且 id ≥ pivot),他不出现的**唯一**原因只能是那一条排除子句;
//   - 断言的是**整个结果集**(ElementsMatch / Equal),不是"不包含某人":后者在查询整体返回空时照绿;
//   - 旁边放**反向对照**:别人之间的拉黑 / pending、我与他之间**已终态**的申请,都不许把人排除掉 ——
//     否则"把 NOT EXISTS 写成恒真"这种坏实现也能通过"被排除的人没出现"。
//
// # RecommendRandom 的 pivot 是随机的,怎么写出确定的断言
//
// pivot 在 [MIN(player_id), MAX(player_id)] 里随机,结果是"id ≥ pivot 的合格候选"。所以:
//   - 排除集与排序 / 截断的精确断言,直接调 recommendAnchor 并**显式给 pivot**(同包可见);
//   - 经 RecommendRandom 走的那一组,让被考察的人恰好是表里 **id 最大**的玩家 —— 无论 pivot 落在哪,
//     `player_id >= pivot` 都覆盖他,于是"他在不在结果里"与随机数无关,红绿都是确定的。
//
// 本文件只读不写业务状态(直写造数据,不经 ensure),所以不调 assertFriendInvariants ——
// 这里的 friend 边没有配套的容量行,那条不变量断言会因为夹具而不是被测代码变红。
//
// 公共夹具(门控、schema、seedPending / seedBlock / mustCount)在 friend_repo_mysql_test.go,
// seedRequestRow 与申请状态常量在 sweep_repo_test.go。

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedRecommendFriendship 直写一对**双向**好友边(与 AcceptFriend 落库的形状一致)。
// 不写容量行:推荐是纯读路径,不看 friend_capacity。
func seedRecommendFriendship(t *testing.T, ctx context.Context, db *sql.DB, a, b uint64) {
	t.Helper()
	for _, edge := range [][2]uint64{{a, b}, {b, a}} {
		_, err := db.ExecContext(ctx,
			"INSERT INTO friend (player_id, friend_player_id, since_ms) VALUES (?, ?, 1)", edge[0], edge[1])
		require.NoError(t, err)
	}
}

func recommendCandidateIDs(candidates []RecommendCandidate) []uint64 {
	ids := make([]uint64, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.CandidatePlayerID)
	}
	return ids
}

// recommendCast 是一套固定的"角色表":同一套关系分别喂给 mutual 与 anchor 两条 query。
// 字段名就是该角色与 me 的关系,用例里按名字引用,失败信息也按名字报,不必回头查 id。
type recommendCast struct {
	me          uint64
	myFriend    uint64 // 已是我的好友                      → 排除
	blockedByMe uint64 // 我拉黑了他                        → 排除
	blockedMe   uint64 // 他拉黑了我                        → 排除
	pendingOut  uint64 // 我发给他的申请仍 pending          → 排除
	pendingIn   uint64 // 他发给我的申请仍 pending          → 排除
	rejected    uint64 // 我与他之间只有**已终态**的申请    → 不排除(反向对照:status 过滤)
	plain       uint64 // 与我毫无关系                      → 不排除(正向对照)
	bystander   uint64 // 与**别人**之间有拉黑与 pending    → 不排除(反向对照:子查询必须关联到 me)
	outsider    uint64 // bystander 那些关系的另一端;在 friend 表里没有任何边,永远不是候选
}

// castRole 给失败信息用:结果集里多出 / 少了一个 id 时,直接说出他是谁、对应哪条子句。
func (c recommendCast) castRole(id uint64) string {
	switch id {
	case c.me:
		return "me 自己(`<> ?` 自排除子句失效)"
	case c.myFriend:
		return "已是好友(NOT IN 好友子查询失效)"
	case c.blockedByMe:
		return "我拉黑的人(friend_block 子句的 me→他 方向失效)"
	case c.blockedMe:
		return "拉黑了我的人(friend_block 子句的 他→me 方向失效)"
	case c.pendingOut:
		return "我已向其发出 pending 申请的人(friend_request 子句的 me→他 方向失效)"
	case c.pendingIn:
		return "已向我发出 pending 申请的人(friend_request 子句的 他→me 方向失效)"
	case c.rejected:
		return "只有终态申请的人(应当出现;没出现说明 r.status = 1 的过滤丢了)"
	case c.plain:
		return "无关系的普通候选(应当出现)"
	case c.bystander:
		return "只与别人有拉黑 / pending 的人(应当出现;没出现说明子查询没有关联到 me)"
	case c.outsider:
		return "在 friend 表里没有边的局外人(不可能是候选)"
	default:
		return "角色表之外的 id"
	}
}

// seedRelationsToMe 写入角色表里"与 me 的关系"那一半(拉黑 / 申请);好友边由各用例按自己的候选池形状去建。
func (c recommendCast) seedRelationsToMe(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	seedBlock(t, ctx, db, c.me, c.blockedByMe)
	seedBlock(t, ctx, db, c.blockedMe, c.me)
	seedPending(t, ctx, db, c.me, c.pendingOut)
	seedPending(t, ctx, db, c.pendingIn, c.me)
	// 终态申请:两个方向各放一条(rejected / accepted 各一),都不许造成排除。
	seedRequestRow(t, ctx, db, c.me, c.rejected, testStatusRejected, 1)
	seedRequestRow(t, ctx, db, c.rejected, c.me, testStatusAccepted, 1)
	// bystander 与 outsider 之间:两个方向的拉黑 + 一条 pending。全都与 me 无关。
	seedBlock(t, ctx, db, c.outsider, c.bystander)
	seedBlock(t, ctx, db, c.bystander, c.outsider)
	seedPending(t, ctx, db, c.outsider, c.bystander)
}

// assertRecommendIDs 断言结果集(不看顺序),并把每个多出 / 缺失的 id 翻译成角色。
func assertRecommendIDs(t *testing.T, c recommendCast, want []uint64, got []RecommendCandidate) {
	t.Helper()
	gotIDs := recommendCandidateIDs(got)
	if assert.ElementsMatch(t, want, gotIDs) {
		return
	}
	wantSet := make(map[uint64]bool, len(want))
	for _, id := range want {
		wantSet[id] = true
	}
	gotSet := make(map[uint64]bool, len(gotIDs))
	for _, id := range gotIDs {
		gotSet[id] = true
		if !wantSet[id] {
			t.Errorf("结果里多出 %d:%s", id, c.castRole(id))
		}
	}
	for _, id := range want {
		if !gotSet[id] {
			t.Errorf("结果里缺了 %d:%s", id, c.castRole(id))
		}
	}
}

// ── RecommendByMutual ─────────────────────────────────────────

// seedMutualGraph:hub 与 hub2 是 me 的好友,角色表里其余每个人都是 hub 的好友 ——
// 于是**每个人(含 me 自己)都是"我好友的好友"**,排除子句是他们不出现的唯一原因。
// hub 与 hub2 也互为好友,因此二者各自经对方可达(同样落在候选池里)。
func seedMutualGraph(t *testing.T, ctx context.Context, db *sql.DB) (c recommendCast, hub, hub2 uint64) {
	t.Helper()
	c = recommendCast{
		me: 55001, myFriend: 55002,
		blockedByMe: 55030, blockedMe: 55031,
		pendingOut: 55040, pendingIn: 55041,
		rejected: 55050, plain: 55020, bystander: 55060,
		outsider: 55900,
	}
	hub, hub2 = 55010, 55011

	seedRecommendFriendship(t, ctx, db, c.me, hub)
	seedRecommendFriendship(t, ctx, db, c.me, hub2)
	seedRecommendFriendship(t, ctx, db, c.me, c.myFriend)
	// hub 认识所有人。me↔hub 的反向边 (hub→me) 正是"自排除"的考题:没有 `f2.friend_player_id <> ?`,
	// me 会以"hub 的好友"的身份被推荐给自己。
	for _, id := range []uint64{c.myFriend, c.blockedByMe, c.blockedMe, c.pendingOut, c.pendingIn,
		c.rejected, c.plain, c.bystander} {
		seedRecommendFriendship(t, ctx, db, hub, id)
	}
	// plain 同时是 hub2 的好友:共同好友数 2,用来验 mutual 计数与降序。
	seedRecommendFriendship(t, ctx, db, hub2, c.plain)
	// hub↔hub2 互为好友:让 hub2 经 hub 可达、真正落进 FOF 候选池。没有这条边时,hub2 不在任何
	// "我好友的好友"的出边上,"hub2 已是好友"的排除断言结构性不可能失败(假绿)。
	// 它不改变计数:plain 仍是 2(经 hub 与 hub2),其余合格候选仍是 1。
	seedRecommendFriendship(t, ctx, db, hub, hub2)

	c.seedRelationsToMe(t, ctx, db)
	return c, hub, hub2
}

func TestRecommendByMutual_AppliesAllFourExclusions(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	c, hub, hub2 := seedMutualGraph(t, ctx, db)

	got, err := repo.RecommendByMutual(ctx, c.me, nil, 100)
	require.NoError(t, err)

	// hub 与 hub2 自己也是"我好友的好友":hub 经 myFriend→hub 与 hub2→hub 可达,hub2 经 hub→hub2 可达。
	// 二者与 myFriend 同属"已是好友"那一类,同样必须被排除;下面单独点名,是为了让失败信息
	// 直接说出"漏掉的是 hub / hub2"。
	assertRecommendIDs(t, c, []uint64{c.plain, c.rejected, c.bystander}, got)
	for _, cand := range got {
		assert.NotEqual(t, hub, cand.CandidatePlayerID, "hub 已是我的好友,不该被推荐")
		assert.NotEqual(t, hub2, cand.CandidatePlayerID, "hub2 已是我的好友,不该被推荐")
	}

	// 共同好友数与排序:plain 经 hub 与 hub2 两条路径可达,必须排第一且 mutual=2;其余都是 1。
	// (同数之间是 RAND() 序,所以只断言第一名。)
	require.NotEmpty(t, got)
	assert.Equal(t, c.plain, got[0].CandidatePlayerID, "共同好友数最多的候选必须排在最前(ORDER BY mutual DESC)")
	for _, cand := range got {
		want := uint32(1)
		if cand.CandidatePlayerID == c.plain {
			want = 2
		}
		assert.Equal(t, want, cand.MutualFriends, "候选 %d 的共同好友数", cand.CandidatePlayerID)
	}
}

// TestRecommendByMutual_HonorsCallerExcludeAndLimit:调用方传入的 exclude(客户端"换一批"回传的 id)
// 由 recommendExcludeClause 拼接,与四类业务排除是两段独立的 SQL,单独验。
func TestRecommendByMutual_HonorsCallerExcludeAndLimit(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	c, _, _ := seedMutualGraph(t, ctx, db)

	// exclude 掉两个本该出现的人;列表里再混一个本来就不会出现的 id(outsider),
	// 验证多占位符的拼接与参数顺序没有错位 —— 错一位时 LIMIT 会吃到一个玩家 id,结果集整个变样。
	got, err := repo.RecommendByMutual(ctx, c.me, []uint64{c.plain, c.outsider, c.rejected}, 100)
	require.NoError(t, err)
	assertRecommendIDs(t, c, []uint64{c.bystander}, got)

	// 全部排除掉 → 空结果且不报错(拼出来的 NOT IN 不能是语法错误)。
	got, err = repo.RecommendByMutual(ctx, c.me, []uint64{c.plain, c.rejected, c.bystander}, 100)
	require.NoError(t, err)
	assert.Empty(t, got)

	// limit 截断:3 个合格候选里取 1 个,取到的必须是共同好友数最多的那个。
	got, err = repo.RecommendByMutual(ctx, c.me, nil, 1)
	require.NoError(t, err)
	require.Len(t, got, 1, "LIMIT 必须生效")
	assert.Equal(t, c.plain, got[0].CandidatePlayerID, "截断发生在排序之后:留下的是 mutual 最高的")

	got, err = repo.RecommendByMutual(ctx, c.me, nil, 2)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	// exclude 与 limit 同时给:占位符顺序是 7 个 playerID → exclude → LIMIT,三段都在场时再验一次。
	got, err = repo.RecommendByMutual(ctx, c.me, []uint64{c.plain}, 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, []uint64{c.rejected, c.bystander}, got[0].CandidatePlayerID)
}

// ── recommendAnchor(直接调,pivot 显式给)──────────────────────

// seedAnchorGraph:anchor 的候选池是"在 friend 表里有出边的玩家"。hub **不是** me 的好友,
// 它与角色表里每个人互为好友,只为让每个人都有出边、从而都落进候选池。
// id 的大小关系是刻意排的(anchor 按 player_id 升序返回,limit / pivot 用例依赖它):
//
//	me < myFriend < hub < plain < blockedByMe < blockedMe < pendingOut < pendingIn < rejected < bystander
func seedAnchorGraph(t *testing.T, ctx context.Context, db *sql.DB) (c recommendCast, hub uint64) {
	t.Helper()
	c = recommendCast{
		me: 53001, myFriend: 53002,
		plain:       53020,
		blockedByMe: 53030, blockedMe: 53031,
		pendingOut: 53040, pendingIn: 53041,
		rejected: 53050, bystander: 53060,
		outsider: 53900,
	}
	hub = 53010

	// me 自己也要有出边(me↔myFriend),否则 me 根本不在候选池里,"自排除"就成了假绿。
	seedRecommendFriendship(t, ctx, db, c.me, c.myFriend)
	for _, id := range []uint64{c.myFriend, c.plain, c.blockedByMe, c.blockedMe, c.pendingOut, c.pendingIn,
		c.rejected, c.bystander} {
		seedRecommendFriendship(t, ctx, db, hub, id)
	}
	// plain 再多两条出边:friend 表一条边一行,没有 GROUP BY 去重时 plain 会在结果里出现 3 次、
	// 还会把 limit 名额吃光。
	seedRecommendFriendship(t, ctx, db, c.plain, c.rejected)
	seedRecommendFriendship(t, ctx, db, c.plain, c.bystander)

	c.seedRelationsToMe(t, ctx, db)
	return c, hub
}

func TestRecommendAnchor_AppliesAllFourExclusions(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	c, hub := seedAnchorGraph(t, ctx, db)

	// 前置自检:每个"应被排除的人"都确实在候选池里(有出边),否则下面的排除断言没有鉴别力。
	for _, id := range []uint64{c.me, c.myFriend, c.blockedByMe, c.blockedMe, c.pendingOut, c.pendingIn} {
		require.NotZero(t, mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend WHERE player_id=?", id),
			"前置条件:%d(%s)必须在 friend 表里有出边", id, c.castRole(id))
	}

	// pivot=1:低于所有 id,整个候选池都在扫描范围内。
	got, err := repo.recommendAnchor(ctx, c.me, nil, 1, 100)
	require.NoError(t, err)
	assertRecommendIDs(t, c, []uint64{hub, c.plain, c.rejected, c.bystander}, got)

	// 顺序与去重:严格按 player_id 升序、每人一次(Equal 同时钉住这两件事)。
	assert.Equal(t, []uint64{hub, c.plain, c.rejected, c.bystander}, recommendCandidateIDs(got),
		"anchor 必须按 player_id 升序返回且每个候选只出现一次(GROUP BY 去重)")
	for _, cand := range got {
		assert.Zero(t, cand.MutualFriends, "random 兜底候选的共同好友数恒为 0(候选 %d)", cand.CandidatePlayerID)
	}
}

func TestRecommendAnchor_HonorsPivotExcludeAndLimit(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	c, hub := seedAnchorGraph(t, ctx, db)

	// pivot 是**闭**下界:从 rejected 的 id 起扫,rejected 自己必须在内,更小的 hub / plain 必须不在。
	got, err := repo.recommendAnchor(ctx, c.me, nil, c.rejected, 100)
	require.NoError(t, err)
	assert.Equal(t, []uint64{c.rejected, c.bystander}, recommendCandidateIDs(got),
		"player_id >= pivot:pivot 自己在内,pivot 之下的候选不在")

	// pivot 高于所有候选 → 空结果、不报错(正向扫没有回绕,这是文件头写明的可接受代价)。
	got, err = repo.recommendAnchor(ctx, c.me, nil, c.outsider, 100)
	require.NoError(t, err)
	assert.Empty(t, got)

	// 调用方 exclude:掐掉 hub 与 rejected,再混一个本来就不出现的 id(验多占位符的拼接)。
	got, err = repo.recommendAnchor(ctx, c.me, []uint64{hub, c.outsider, c.rejected}, 1, 100)
	require.NoError(t, err)
	assert.Equal(t, []uint64{c.plain, c.bystander}, recommendCandidateIDs(got))

	// limit 截断:升序里的前两个。plain 有 3 条出边 —— 没去重的话这里会是 [hub, plain] 碰巧对、
	// 而 limit=3 会是 [hub, plain, plain],所以两个档位都验。
	got, err = repo.recommendAnchor(ctx, c.me, nil, 1, 2)
	require.NoError(t, err)
	assert.Equal(t, []uint64{hub, c.plain}, recommendCandidateIDs(got))
	got, err = repo.recommendAnchor(ctx, c.me, nil, 1, 3)
	require.NoError(t, err)
	assert.Equal(t, []uint64{hub, c.plain, c.rejected}, recommendCandidateIDs(got),
		"limit 数的是去重后的候选,不是 friend 表的行")

	// pivot、exclude、limit 三段同时在场:占位符顺序是 pivot → 6 个 playerID → exclude → LIMIT。
	got, err = repo.recommendAnchor(ctx, c.me, []uint64{c.plain}, c.plain, 1)
	require.NoError(t, err)
	assert.Equal(t, []uint64{c.rejected}, recommendCandidateIDs(got))
}

// ── RecommendRandom(pivot 随机;用"最大 id"把断言做成确定的)──────

// TestRecommendRandom_EmptyTableReturnsNoCandidates:空表时 MIN/MAX 是 NULL,必须是 (空, nil) 而不是 error ——
// 新服 friend 表本来就是空的,把它报成错误会让推荐接口在开服第一天整个不可用。
func TestRecommendRandom_EmptyTableReturnsNoCandidates(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	got, err := repo.RecommendRandom(ctx, 54001, nil, 10)
	require.NoError(t, err, "空表的 MIN/MAX 是 NULL:必须扫进可空类型,不能报 Scan 错误")
	assert.Empty(t, got)
}

// TestRecommendRandom_SinglePlayerRange:表里只有一个 player_id 时 MIN == MAX,pivot 只能取它,
// 结果是确定的。钉的是"pivot 取自真实的 [MIN, MAX]"—— 取一个随机 uint64 的话这里几乎必空。
func TestRecommendRandom_SinglePlayerRange(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const lonely uint64 = 54100
	// 单向边:只让 lonely 出现在 player_id 列里。
	_, err := db.ExecContext(ctx,
		"INSERT INTO friend (player_id, friend_player_id, since_ms) VALUES (?, ?, 1)", lonely, uint64(54101))
	require.NoError(t, err)

	got, err := repo.RecommendRandom(ctx, 54001, nil, 10)
	require.NoError(t, err)
	assert.Equal(t, []uint64{lonely}, recommendCandidateIDs(got))
}

// TestRecommendRandom_ExclusionsHoldForTheMaxIDPlayer 经 RecommendRandom 的完整入口再验一遍四类排除
// 与调用方 exclude(入口自己也会出错:参数传反、exclude 没往下传,直接调 recommendAnchor 验不到)。
//
// 被考察的人 subject 恒为表里 id 最大的玩家,所以无论 pivot 随机到哪,他都在扫描范围内(见文件头)。
// 第一行是**对照**:subject 与 me 毫无关系时必须每次都被推荐 —— 没有它,"subject 从不出现"在
// "夹具没把他放进候选池"的情况下也照绿。
func TestRecommendRandom_ExclusionsHoldForTheMaxIDPlayer(t *testing.T) {
	const (
		defaultMe uint64 = 54001
		filler    uint64 = 54010 // 低位的普通玩家:把 [MIN, MAX] 撑开,让 pivot 真的在一个区间里随机
		hub       uint64 = 54011 // 给 subject 提供出边
		subject   uint64 = 54900 // 表里最大的 player_id
		// pivot 每次随机:多调几次让它落在区间的不同位置。次数与正确性无关(每一次都必须满足断言)。
		calls = 8
	)

	cases := []struct {
		name        string
		me          uint64
		relate      func(t *testing.T, ctx context.Context, db *sql.DB)
		exclude     []uint64
		wantSubject bool
	}{
		{name: "对照:与我无关的最大 id 玩家每次都被推荐", me: defaultMe, wantSubject: true},
		{name: "对照:只有终态申请不构成排除", me: defaultMe, wantSubject: true,
			relate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				seedRequestRow(t, ctx, db, defaultMe, subject, testStatusRejected, 1)
			}},
		{name: "自己", me: subject},
		{name: "已是好友", me: defaultMe,
			relate: func(t *testing.T, ctx context.Context, db *sql.DB) {
				seedRecommendFriendship(t, ctx, db, defaultMe, subject)
			}},
		{name: "我拉黑了他", me: defaultMe,
			relate: func(t *testing.T, ctx context.Context, db *sql.DB) { seedBlock(t, ctx, db, defaultMe, subject) }},
		{name: "他拉黑了我", me: defaultMe,
			relate: func(t *testing.T, ctx context.Context, db *sql.DB) { seedBlock(t, ctx, db, subject, defaultMe) }},
		{name: "我发给他的申请仍 pending", me: defaultMe,
			relate: func(t *testing.T, ctx context.Context, db *sql.DB) { seedPending(t, ctx, db, defaultMe, subject) }},
		{name: "他发给我的申请仍 pending", me: defaultMe,
			relate: func(t *testing.T, ctx context.Context, db *sql.DB) { seedPending(t, ctx, db, subject, defaultMe) }},
		{name: "调用方 exclude 列表", me: defaultMe, exclude: []uint64{filler, subject}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := openFriendTestDB(t)
			repo, _ := newFriendTestRepo(t, db)

			seedRecommendFriendship(t, ctx, db, filler, hub)
			seedRecommendFriendship(t, ctx, db, hub, subject)
			if tc.relate != nil {
				tc.relate(t, ctx, db)
			}
			require.Equal(t, int64(subject), mustCount(t, ctx, db, "SELECT MAX(player_id) FROM friend"),
				"前置条件:subject 必须是 friend 表里最大的 player_id,否则断言会随 pivot 随机")

			for i := 0; i < calls; i++ {
				got, err := repo.RecommendRandom(ctx, tc.me, tc.exclude, 100)
				require.NoError(t, err)
				ids := recommendCandidateIDs(got)
				if tc.wantSubject {
					assert.Contains(t, ids, subject, "第 %d 次:subject 是合格候选且 id ≥ 任何 pivot,必须被推荐", i+1)
				} else {
					assert.NotContains(t, ids, subject, "第 %d 次:subject 属于「%s」,不该被推荐", i+1, tc.name)
				}
				assert.NotContains(t, ids, tc.me, "第 %d 次:不该把玩家推荐给他自己", i+1)
			}
		})
	}
}
