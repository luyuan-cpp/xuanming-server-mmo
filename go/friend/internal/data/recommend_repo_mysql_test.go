package data

// recommend_repo_mysql_test.go —— 好友推荐三段 SQL 的真实 MySQL 回归(handoff §3 第 6b 条)。
//
// # 为什么必须打真库
//
// recommend_repo.go 的四类业务排除(自己 / 已是好友 / 任一方向拉黑 / 任一方向仍 pending 的申请)
// **内联写在两条 query 字符串里、各一份**,列名还不一样:RecommendByMutual 用 `f2.friend_player_id`,
// recommendAnchor 在五处 NOT EXISTS 里必须写派生表别名的全限定 `c.player_id`(裸 `player_id` 会解析到
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
//     有出边、id ≥ pivot,且落在 pivot 起的前 RecommendAnchorWindow 个去重 id 内 —— seedAnchorGraph 的池只有
//     十来个人、远小于 W;WindowCoversBoundedExclusionBudget 与 PlanIsPerRowPrimaryKeyLookups 的关系人都紧贴
//     pivot、都在窗口里),他不出现的**唯一**原因只能是那一条排除子句;
//   - 例外:TestRecommendAnchor_StopsAtWindowWhenSaturated **有意**把一半"拉黑了我"的人(pivot+W..pivot+2W-1)
//     和 5 个合格者放在窗口之外。它断言的是窗口语义(窗口外的人一律不看、不推荐),不是某条排除子句,
//     所以不受上一条"唯一原因"纪律约束;它自己的正向对照(从窗口外那 5 人起扫必须全部返回)另行保证夹具造对了;
//   - 断言的是**整个结果集**(ElementsMatch / Equal),不是"不包含某人":后者在查询整体返回空时照绿;
//   - 旁边放**反向对照**:别人之间的拉黑 / pending、我与他之间**已终态**的申请,都不许把人排除掉 ——
//     否则"把 NOT EXISTS 写成恒真"这种坏实现也能通过"被排除的人没出现"。
//
// # RecommendRandom 的 pivot 是随机的,怎么写出确定的断言
//
// pivot 在 [MIN(player_id), MAX(player_id)] 里随机,结果是"pivot 起前 RecommendAnchorWindow 个去重 id 里的合格候选"
// (RecommendRandom 用例的池至多 4 个玩家、远小于 W,在这些用例里就等于"id ≥ pivot 的合格候选")。所以:
//   - 排除集与排序 / 截断的精确断言,直接调 recommendAnchor 并**显式给 pivot**(同包可见);
//   - 经 RecommendRandom 走的那一组,让被考察的人恰好是表里 **id 最大**的玩家 —— 无论 pivot 落在哪,
//     `player_id >= pivot` 都覆盖他、池远小于 W 所以窗口也一定够到他,于是"他在不在结果里"与随机数无关,
//     红绿都是确定的。
//
// 本文件只读不写业务状态(直写造数据,不经 ensure),所以不调 assertFriendInvariants ——
// 这里的 friend 边没有配套的容量行,那条不变量断言会因为夹具而不是被测代码变红。
//
// 公共夹具(门控、schema、seedPending / seedBlock / mustCount)在 friend_repo_mysql_test.go,
// seedRequestRow 与申请状态常量在 sweep_repo_test.go。

import (
	"context"
	"database/sql"
	"strings"
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
	// plain 再多两条出边:friend 表一条边一行,没有 DISTINCT 去重时 plain 会在结果里出现 3 次、
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
		"anchor 必须按 player_id 升序返回且每个候选只出现一次(DISTINCT 窗口去重)")
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

	// pivot、exclude、limit 三段同时在场:占位符顺序是 pivot → 窗口 W → 6 个 playerID → exclude → LIMIT。
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
// 被考察的人 subject 恒为表里 id 最大的玩家,所以无论 pivot 随机到哪,他都在扫描范围内:id ≥ 任何 pivot,
// 且池里至多 4 个玩家(filler / hub / subject,"已是好友"一例再加 me)、远小于 RecommendAnchorWindow,
// 窗口一定够到他(见文件头)。
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

// ── 扫描上界回归(2026-09-28 缺陷:recommendAnchor 的扫描量随 pivot 之后的玩家数线性增长)──────

func recommendIDRange(first uint64, n int) []uint64 {
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = first + uint64(i)
	}
	return ids
}

// recommendPairsTo 把每个 id 变成 (id, to);recommendPairsFrom 变成 (from, id)。
func recommendPairsTo(ids []uint64, to uint64) [][2]uint64 {
	pairs := make([][2]uint64, 0, len(ids))
	for _, id := range ids {
		pairs = append(pairs, [2]uint64{id, to})
	}
	return pairs
}

func recommendPairsFrom(from uint64, ids []uint64) [][2]uint64 {
	pairs := make([][2]uint64, 0, len(ids))
	for _, id := range ids {
		pairs = append(pairs, [2]uint64{from, id})
	}
	return pairs
}

// 批量直写的前缀与单行模板:只有两个 id 走占位符,其余列用字面量。
const (
	recommendBulkFriendPrefix  = "INSERT INTO friend (player_id, friend_player_id, since_ms) VALUES "
	recommendBulkBlockPrefix   = "INSERT INTO friend_block (player_id, blocked_player_id, since_ms) VALUES "
	recommendBulkPendingPrefix = "INSERT INTO friend_request (from_player_id, to_player_id, request_time_ms, status, updated_ms) VALUES "
	recommendBulkPairRow       = "(?, ?, 1)"
	recommendBulkPendingRow    = "(?, ?, 1, 1, 1)" // status=1 即 pending,与 seedPending 同值
)

// bulkInsertRecommendPairs 用每批 1000 行的多行 INSERT 直写 (a, b) 对:上界回归要造上万行,逐行 INSERT 会慢一个数量级。
// 每批 2000 个占位符,远低于预处理语句 65535 的上限。
func bulkInsertRecommendPairs(t *testing.T, ctx context.Context, db *sql.DB, prefix, row string, pairs [][2]uint64) {
	t.Helper()
	const batch = 1000
	for start := 0; start < len(pairs); start += batch {
		end := min(start+batch, len(pairs))
		var sb strings.Builder
		sb.WriteString(prefix)
		args := make([]any, 0, 2*(end-start))
		for i := start; i < end; i++ {
			if i > start {
				sb.WriteString(",")
			}
			sb.WriteString(row)
			args = append(args, pairs[i][0], pairs[i][1])
		}
		_, err := db.ExecContext(ctx, sb.String(), args...)
		require.NoError(t, err)
	}
}

// seedRecommendUnrelatedExclusionRows 给两张排除表各放两行与被测玩家无关的数据:
// 空表可能被优化器当成 const 表整段消掉,那样测的就不是生产形态。
func seedRecommendUnrelatedExclusionRows(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	seedBlock(t, ctx, db, 3, 4)
	seedBlock(t, ctx, db, 4, 3)
	seedPending(t, ctx, db, 3, 4)
	seedRequestRow(t, ctx, db, 5, 6, testStatusRejected, 1)
}

// analyzeRecommendTables 显式刷新统计信息:InnoDB 持久统计异步重算,批量直写后不刷新,计划会随"统计赶没赶上"而变。
func analyzeRecommendTables(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(ctx, "ANALYZE TABLE friend, friend_block, friend_request")
	require.NoError(t, err)
}

// sessionHandlerReads 读当前会话的 CONNECTION_ID 与 Handler_read_* 各项之和。
// 不用 FLUSH STATUS(要 RELOAD 权限);SHOW SESSION STATUS 与 SELECT CONNECTION_ID() 本身不产生 Handler_read
// (2026-09-28 在 MySQL 26.7.0 上核对)。
func sessionHandlerReads(t *testing.T, ctx context.Context, db *sql.DB) (connID, total uint64) {
	t.Helper()
	require.NoError(t, db.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&connID))
	rows, err := db.QueryContext(ctx, "SHOW SESSION STATUS LIKE 'Handler_read%'")
	require.NoError(t, err)
	defer rows.Close()
	items := 0
	for rows.Next() {
		var name string
		var value uint64
		require.NoError(t, rows.Scan(&name, &value))
		total += value
		items++
	}
	require.NoError(t, rows.Err())
	require.NotZero(t, items, "SHOW SESSION STATUS 没有返回 Handler_read 行:不是 MySQL,读数断言无从成立")
	return connID, total
}

// recommendAnchorWithReads 调一次 recommendAnchor,返回结果与这一次在会话上产生的 Handler_read_* 增量。
// Handler 计数按会话统计:先把连接池钉成一条连接,并在前后各读一次 CONNECTION_ID —— 两次不同(连接被重建)
// 说明计数跨了会话,直接判失败,不给出一个假数。
func recommendAnchorWithReads(t *testing.T, ctx context.Context, db *sql.DB, repo *FriendRepo,
	me uint64, exclude []uint64, pivot uint64, limit uint32) ([]RecommendCandidate, uint64) {
	t.Helper()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	connBefore, readsBefore := sessionHandlerReads(t, ctx, db)
	got, err := repo.recommendAnchor(ctx, me, exclude, pivot, limit)
	require.NoError(t, err)
	connAfter, readsAfter := sessionHandlerReads(t, ctx, db)
	require.Equal(t, connBefore, connAfter, "Handler 计数跨了会话(连接被重建),本次读数无效")
	return got, readsAfter - readsBefore
}

// explainTraditionalRows 跑 EXPLAIN FORMAT=TRADITIONAL,返回全部行(列名 → 值,NULL 记为空串)。
// friend_guard_lock_order_mysql_test.go 的 explainTraditional 只取第一行(那边都是单表语句),这里是多表计划。
func explainTraditionalRows(t *testing.T, ctx context.Context, db *sql.DB, stmt string) []map[string]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, "EXPLAIN FORMAT=TRADITIONAL "+stmt)
	require.NoError(t, err, "EXPLAIN 失败: %s", stmt)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var plan []map[string]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		row := make(map[string]string, len(cols))
		for i, c := range cols {
			row[c] = vals[i].String
		}
		plan = append(plan, row)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, plan, "EXPLAIN 没有返回任何行: %s", stmt)
	return plan
}

// TestRecommendAnchor_ReadsBoundedByWindowNotPoolSize 是 2026-09-28 缺陷的确定性回归(先红后绿)。
// 缺陷:旧写法的 GROUP BY 去重落成临时表,LIMIT 无法提前终止,扫描量 = pivot 之后的全部玩家数。
// 夹具:池里 16×W 个单边玩家,pivot 放在正中(前后各 8×W 人);me 不在池里。
// 断言:结果 = pivot 起的前 20 个;会话 Handler_read_* 增量 ≤ 8×W。
//   - 旧实现:结果对 → 红在读数断言。读数随计划浮动:2026-09-28 本夹具多数实测为 40,967(first 1 / key 16,387 /
//     next 16,386 / rnd_next 8,193),另一轮 52 次 ANALYZE 重采样里也见过 12,334–27,521。下限来自 GROUP BY 临时表
//     回读 pivot 之后全部 8×W 个分组的 rnd_next 8,193:只要旧写法仍落临时表,读数就恒 > 8×W,但最坏余量只有约 1.5 倍;
//   - 新实现:2,150 ≈ 2.1×W。上界取 8×W:理论最坏是 7×W+3 = 窗口生产 ≤ W+2(跳跃扫描是 W+1 次定位 + 1 次
//     read_last;本夹具每人一条边,实测走范围扫描,是 1 次定位 + W 次 next = W+1)+ 物化表扫描 W+1 + 点查 5×W。
//
// pivot 放在正中而不是表头:窗口若退化成"从索引开头全扫再过滤"(EXPLAIN type=index),会把 pivot 之前的 8×W 人
// 也读一遍,同样越界(2026-09-28 实测:给派生表加 GROUP_INDEX(friend PRIMARY) 提示后本夹具读 10,343 次)。
// 夹具先 ANALYZE 再测,所以这条守的是"统计新鲜时 SQL / 提示的改动造成的这种退化";统计本身退化(持久统计 ≤1 行)
// 在用例里进不来,是登记在案的剩余风险,见 recommend_repo.go 文件头第 2 条的 ⚠。断言的是行操作计数,不看墙钟。
func TestRecommendAnchor_ReadsBoundedByWindowNotPoolSize(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	w := int(RecommendAnchorWindow)
	const (
		me   uint64 = 56001 // 没有出边:不在候选池里
		sink uint64 = 1     // 所有出边的另一端;自己没有出边,也不在池里
		base uint64 = 56100000
	)
	poolSize := 16 * w
	pivot := base + uint64(poolSize/2)
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkFriendPrefix, recommendBulkPairRow,
		recommendPairsTo(recommendIDRange(base, poolSize), sink))
	seedRecommendUnrelatedExclusionRows(t, ctx, db)
	analyzeRecommendTables(t, ctx, db)

	got, reads := recommendAnchorWithReads(t, ctx, db, repo, me, []uint64{me}, pivot, 20)
	require.Equal(t, recommendIDRange(pivot, 20), recommendCandidateIDs(got), "没有任何排除时,结果必须是 pivot 起的前 20 个")
	t.Logf("recommendAnchor 会话 Handler_read_* 增量 = %d(上界 8×W = %d;池 %d 人,pivot 之后 %d 人)",
		reads, 8*w, poolSize, poolSize/2)
	assert.LessOrEqual(t, reads, uint64(8*w),
		"扫描量越过了窗口上界:recommendAnchor 又在随 pivot 之后的玩家数线性增长(2026-09-28 缺陷复发)")
}

// TestRecommendAnchor_StopsAtWindowWhenSaturated 钉住上界的可观察后果(先红后绿):
// pivot 之后紧挨着 2×W 个"拉黑了我"的人(唯一没有配置上限的排除类),窗口被他们占满;窗口之外再放 5 个合格者。
//   - 新实现只看前 W 个去重 id → 返回空;每个候选 ≤5 次主键点查 → 读数 ≤ 8×W(2026-09-28 实测 7,170)。
//   - 旧实现一直扫到凑满 → 返回那 5 个 → 红在 assert.Empty。任何正确执行旧 SQL 的计划都必须返回他们,
//     所以这条红与执行计划、引擎都无关。(旧实现在本夹具上的读数随计划浮动,实测多为 8,217、也见过 622,
//     不作为红的依据 —— 旧写法的代价主要花在 hash antijoin (no condition) 的内存比较上,Handler 计数看不见。)
//
// "返回空"是有意的降级(推荐是可降级展示功能),不是漏人;写成断言,是为了让"窗口被删掉 / 被绕过"必然变红。
func TestRecommendAnchor_StopsAtWindowWhenSaturated(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	w := int(RecommendAnchorWindow)
	const (
		me    uint64 = 57001
		sink  uint64 = 1
		pivot uint64 = 57100000
	)
	blockers := recommendIDRange(pivot, 2*w)
	tail := recommendIDRange(pivot+uint64(2*w), 5)
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkFriendPrefix, recommendBulkPairRow, recommendPairsTo(blockers, sink))
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkFriendPrefix, recommendBulkPairRow, recommendPairsTo(tail, sink))
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkBlockPrefix, recommendBulkPairRow, recommendPairsTo(blockers, me))
	analyzeRecommendTables(t, ctx, db)

	// 正向对照:尾部 5 人确实在池里且合格。没有它,"返回空"在夹具没造对时也照绿。
	control, err := repo.recommendAnchor(ctx, me, []uint64{me}, tail[0], 5)
	require.NoError(t, err)
	require.Equal(t, tail, recommendCandidateIDs(control), "前置条件:窗口之外的 5 人必须是合格候选")

	got, reads := recommendAnchorWithReads(t, ctx, db, repo, me, []uint64{me}, pivot, 5)
	t.Logf("饱和窗口:会话 Handler_read_* 增量 = %d(上界 8×W = %d)", reads, 8*w)
	assert.Empty(t, recommendCandidateIDs(got),
		"窗口(前 %d 个去重 id)全被'拉黑了我'的人占满时必须返回空:返回了窗口之外的人,说明扫描越过了窗口上界", w)
	assert.LessOrEqual(t, reads, uint64(8*w), "每个候选的排除判定必须是 ≤5 次主键点查")
}

// TestRecommendAnchor_WindowCoversBoundedExclusionBudget 钉住 RecommendAnchorWindow 的取值依据(新旧实现都绿):
// 所有"有配置上限"的排除全部顶满、且全部紧贴 pivot 时,窗口里仍装得下 20 个合格者,结果与不设窗口时逐条相同。
// 数量用字面量(data 不能 import config),右侧注释是 etc/friend.yaml 的默认值;对账在 config 包的
// TestRecommendAnchorWindowCoversExclusionBudget。实测(2026-09-28):窗口 754 通过、753 只返回 19 个。
//
// 本夹具比生产可达的组合更严:它让"mutual 已追加进 exclude 的 19 个"与"want = 20"同时顶满,而 logic 层
// RecommendFriends 里两者之和恒等于 limit ≤ RecommendMaxLimit(mutual 每选中一人,random 的 want 就少一个)。
// 可达组合的真实需求是 735(见 RecommendAnchorWindow 的注释);按更严的 754 验,窗口留的余量只会更多、不会更少。
func TestRecommendAnchor_WindowCoversBoundedExclusionBudget(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	const (
		me   uint64 = 58000000 // 同时是 pivot:me 与好友有双向边,自己也在池里、占一个窗口位
		sink uint64 = 1
	)
	friends := recommendIDRange(me+1, 200)       // Friend.MaxFriends
	blockedByMe := recommendIDRange(me+201, 200) // Friend.MaxBlocks
	pendingOut := recommendIDRange(me+401, 50)   // Friend.MaxPendingRequests(出站)
	pendingIn := recommendIDRange(me+451, 200)   // Friend.MaxIncomingRequests(入站)
	// 调用方 exclude:客户端回传 RecommendMaxExclude = 64 个 + mutual 已选中后追加的 RecommendMaxLimit-1 = 19 个。
	callerExcluded := recommendIDRange(me+651, 83)
	eligible := recommendIDRange(me+734, 25) // 1+200+200+50+200+83 = 734 个排除者之后

	bulkInsertRecommendPairs(t, ctx, db, recommendBulkFriendPrefix, recommendBulkPairRow,
		append(recommendPairsFrom(me, friends), recommendPairsTo(friends, me)...))
	var others []uint64
	for _, group := range [][]uint64{blockedByMe, pendingOut, pendingIn, callerExcluded, eligible} {
		others = append(others, group...)
	}
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkFriendPrefix, recommendBulkPairRow, recommendPairsTo(others, sink))
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkBlockPrefix, recommendBulkPairRow, recommendPairsFrom(me, blockedByMe))
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkPendingPrefix, recommendBulkPendingRow,
		append(recommendPairsFrom(me, pendingOut), recommendPairsTo(pendingIn, me)...))
	// 反向对照(都在应返回的前 20 人里):eligible[0] 与我只有终态申请;eligible[1] 与别人互相拉黑且有 pending。
	seedRequestRow(t, ctx, db, me, eligible[0], testStatusRejected, 1)
	seedRequestRow(t, ctx, db, eligible[0], me, testStatusAccepted, 1)
	seedBlock(t, ctx, db, eligible[1], 2)
	seedBlock(t, ctx, db, 2, eligible[1])
	seedPending(t, ctx, db, 2, eligible[1])

	exclude := append(append([]uint64{}, callerExcluded...), me)
	got, err := repo.recommendAnchor(ctx, me, exclude, me, 20)
	require.NoError(t, err)
	assert.Equal(t, eligible[:20], recommendCandidateIDs(got),
		"有上限的排除全部顶满时,窗口 %d 仍须装得下 20 个合格者(预算 734 + 20 = 754)", RecommendAnchorWindow)
}

// TestRecommendAnchor_PlanIsPerRowPrimaryKeyLookups 钉住上界成立所依赖的计划形状:五个排除子查询各自是
// eq_ref / PRIMARY / key_len=16 / possible_keys 只有 PRIMARY(完整主键单行点查),计划里没有任何 MATERIALIZED
// 子查询;派生表窗口走 PRIMARY 上的 range 访问。
// 派生表那行断言 type=range 而不只看 key:跳跃扫描(Using index for group-by)与范围扫描在 traditional EXPLAIN 里
// 都是 range,两者互相翻转不会让这条变红;而从索引开头全扫(type=index,key 仍是 PRIMARY)会把 pivot 之前的人
// 也读一遍。2026-09-28 实测:给派生表加 GROUP_INDEX(friend PRIMARY) 提示(统计新鲜)后,本夹具的派生表行变成
// type=index、key=PRIMARY,五个排除子查询仍全是 eq_ref / PRIMARY —— 只断言 key 的话这里全绿;读数 10,368 > 8×W。
// 本用例先 ANALYZE 再 EXPLAIN,所以这条只守"统计新鲜时 SQL / 提示的改动让窗口失去 range 访问"。持久统计退化到
// ≤1 行也会得到同样的 type=index 计划(把 friend 的统计冻结在 n_rows=1 时本夹具读 10,364),但那个状态用例里
// 进不来,生产上只靠 InnoDB auto_recalc,是登记在案的剩余风险(见 recommend_repo.go 文件头第 2 条的 ⚠)。
// 夹具刻意是"窗口大(W 个候选)、me 的关系集小":这种数据会诱使优化器改成先物化排除集。2026-09-28 实测:
// 去掉 SEMIJOIN(FIRSTMATCH) 后 b_in 被物化成 friend_block 主键全索引扫描(type=index)、r_in 被物化成
// friend_request 全表扫描(type=ALL);两个提示都去掉时 r_out 被物化成 idx_status_updated 上 status=1 的全服
// pending 扫描。三者都与玩家总数成正比,在测试的小表上读数看不出来,只有计划看得出来,所以单独守。
// 只去掉子查询上的 FORCE INDEX 时,本夹具上计划不变、只是 possible_keys 多出二级索引:possible_keys 那条断言
// 守的是防线(防统计变化后改选二级索引),不是已实测到的退化。
//
// 计划按生产上限 limit=20 取;顺带核对结果时 limit 取 30:按 id 升序,前 20 个合格者到 pivot+26 就截止,
// 够不到放在 +30..+33 的申请关系人,那样 r_out / r_in 的排除失效、或 status=1 的过滤丢了都照样绿。
// 取 30 后比较范围到 pivot+38:f(+10..+12)、b_out(+20/+21)、b_in(+22/+23)、r_out(+30)、r_in(+32)
// 必须被排除,+31 / +33 只有终态申请、必须出现。
func TestRecommendAnchor_PlanIsPerRowPrimaryKeyLookups(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	w := int(RecommendAnchorWindow)
	const (
		me   uint64 = 56001
		sink uint64 = 1
		base uint64 = 56100000
	)
	poolSize := 16 * w
	pivot := base + uint64(poolSize/2)
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkFriendPrefix, recommendBulkPairRow,
		recommendPairsTo(recommendIDRange(base, poolSize), sink))
	myFriends := recommendIDRange(pivot+10, 3)
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkFriendPrefix, recommendBulkPairRow,
		append(recommendPairsFrom(me, myFriends), recommendPairsTo(myFriends, me)...))
	seedBlock(t, ctx, db, me, pivot+20)
	seedBlock(t, ctx, db, me, pivot+21)
	seedBlock(t, ctx, db, pivot+22, me)
	seedBlock(t, ctx, db, pivot+23, me)
	seedPending(t, ctx, db, me, pivot+30)
	seedRequestRow(t, ctx, db, me, pivot+31, testStatusRejected, 1)
	seedPending(t, ctx, db, pivot+32, me)
	seedRequestRow(t, ctx, db, pivot+33, me, testStatusAccepted, 1)
	seedRecommendUnrelatedExclusionRows(t, ctx, db)
	analyzeRecommendTables(t, ctx, db)

	query, args := recommendAnchorStatement(me, []uint64{me}, pivot, 20)
	plan := explainTraditionalRows(t, ctx, db, inlineNumericArgs(t, query, args...))
	seen := map[string]bool{"f": false, "b_out": false, "b_in": false, "r_out": false, "r_in": false}
	for _, row := range plan {
		table := row["table"]
		assert.NotEqual(t, "MATERIALIZED", row["select_type"],
			"排除子查询被物化了(表 %s,type=%s key=%s):扫描量会跟着被物化的集合走,不再由窗口封顶", table, row["type"], row["key"])
		assert.False(t, strings.HasPrefix(table, "<subquery"), "计划里出现了物化子查询 %s", table)
		if row["select_type"] == "DERIVED" {
			assert.Equal(t, "PRIMARY", row["key"], "窗口必须走 friend 主键(player_id 前缀)")
			assert.Equal(t, "range", row["type"],
				"窗口必须是从 pivot 起的 range 访问(跳跃扫描也显示为 range);type=index 是从索引开头全扫,读数随 pivot 之前的行数增长")
		}
		if _, ok := seen[table]; ok {
			seen[table] = true
			assert.Equal(t, "eq_ref", row["type"], "%s 必须是每个候选一次单行点查", table)
			assert.Equal(t, "PRIMARY", row["key"], "%s 必须走主键", table)
			assert.Equal(t, "16", row["key_len"], "%s 必须用满两列主键", table)
			assert.Equal(t, "PRIMARY", row["possible_keys"], "%s 的 FORCE INDEX (PRIMARY) 丢了:优化器又能挑二级索引去建排除集", table)
		}
	}
	for alias, ok := range seen {
		assert.True(t, ok, "计划里缺了排除子查询 %s(别名被改了,或被物化成 <subqueryN>)", alias)
	}

	const resultLimit = 30
	got, err := repo.recommendAnchor(ctx, me, []uint64{me}, pivot, resultLimit)
	require.NoError(t, err)
	excluded := map[uint64]bool{pivot + 10: true, pivot + 11: true, pivot + 12: true, pivot + 20: true, pivot + 21: true,
		pivot + 22: true, pivot + 23: true, pivot + 30: true, pivot + 32: true}
	want := make([]uint64, 0, resultLimit)
	for id := pivot; len(want) < resultLimit; id++ {
		if !excluded[id] {
			want = append(want, id)
		}
	}
	// 自检:期望列表必须越过 pivot+33,否则 r_out / r_in 与终态申请又落到了比较范围之外(改夹具或 limit 时会触发)。
	require.Greater(t, want[len(want)-1], pivot+33, "结果核对的范围没有覆盖到 pivot+30..+33 的申请关系人")
	assert.Equal(t, want, recommendCandidateIDs(got),
		"pivot+31 / pivot+33 只有终态申请,必须出现;f / b_out / b_in / r_out(+30)/ r_in(+32)的关系人必须被排除")
}
