package data

// recommend_repo_mysql_test.go —— 好友推荐三段 SQL 的真实 MySQL 回归(handoff §3 第 6b 条)。
//
// # 为什么必须打真库
//
// recommend_repo.go 的四类业务排除(自己 / 已是好友 / 任一方向拉黑 / 任一方向仍 pending 的申请)
// **内联写在两条 query 字符串里、各一份**,列名还不一样:两条都在五处 NOT EXISTS 里写派生表别名的全限定列 ——
// RecommendByMutual 是 `c.candidate_id`(自排除在内层,写 `f2.friend_player_id <> ?`),recommendAnchor 是 `c.player_id`
// (裸 `player_id` 会解析到子查询自己的表上,条件变成"自己拉黑自己",排除静默失效)。拼错列名在 Go 侧完全静态无感,
// 编译、vet、mock 都看不见 —— 只有让 MySQL 真的解析并执行这两条 SQL 才验得到。
// 所以两条 query **分别**验一遍,不因为"长得一样"就只验一条。
//
// # 每一类排除都要造出"子句失效就会露馅"的数据
//
// robot 冒烟的"推荐不含已拉黑的 C"在三账号数据集下结构性不可能失败(C 本来就不在候选池里),
// 那是已登记的假绿,这里不许重蹈。本文件的纪律:
//
//   - 每个"应被排除的人"都**同时满足成为候选的全部条件**(mutual:是我好友的好友,且排名落在前 RecommendAnchorWindow
//     名之内 —— seedMutualGraph 与 seedMutualAdversarialGraph 的候选都远少于 W;anchor:在 friend 表里
//     有出边、id ≥ pivot,且落在 pivot 起的前 RecommendAnchorWindow 个去重 id 内 —— seedAnchorGraph 的池只有
//     十来个人、远小于 W;WindowCoversBoundedExclusionBudget 与 PlanIsPerRowPrimaryKeyLookups 的关系人都紧贴
//     pivot、都在窗口里),他不出现的**唯一**原因只能是那一条排除子句;
//   - 例外:TestRecommendAnchor_StopsAtWindowWhenSaturated **有意**把一半"拉黑了我"的人(pivot+W..pivot+2W-1)
//     和 5 个合格者放在窗口之外。它断言的是窗口语义(窗口外的人一律不看、不推荐),不是某条排除子句,
//     所以不受上一条"唯一原因"纪律约束;它自己的正向对照(从窗口外那 5 人起扫必须全部返回)另行保证夹具造对了;
//     TestRecommendByMutual_StopsAtWindowWhenSaturated 同理:断言的是"排名窗口之外的合格候选不被返回",
//     尾部那 8×W 个合格者不出现的原因是窗口,不是排除子句;
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
	"time"

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
		return "已是好友(NOT EXISTS f 子句失效)"
	case c.blockedByMe:
		return "我拉黑的人(NOT EXISTS b_out 子句失效)"
	case c.blockedMe:
		return "拉黑了我的人(NOT EXISTS b_in 子句失效)"
	case c.pendingOut:
		return "我已向其发出 pending 申请的人(NOT EXISTS r_out 子句失效)"
	case c.pendingIn:
		return "已向我发出 pending 申请的人(NOT EXISTS r_in 子句失效)"
	case c.rejected:
		return "只有终态申请的人(应当出现;没出现说明 r_out / r_in 的 status = 1 过滤丢了)"
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

	// exclude 与 limit 同时给:占位符顺序是 playerID ×2 → 窗口 W → playerID ×5 → exclude → LIMIT,三段都在场时再验一次。
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
	// 共同好友数是真数出来的,不是恒填 0(2026-10-09 之前的行为):me 的好友只有 myFriend,而 myFriend 的好友是
	// me 与 hub —— 所以 hub 与我有 1 个共同好友,其余三人是 0。hub 那个 1 钉的是"兜底路径也数";另外三个 0 钉的是
	// "数的是两个好友列表的交集":plain 有 3 个好友(hub / rejected / bystander)却没有一个是我的好友,
	// 把子查询写成数候选自己的好友数、或者漏了 m1.player_id = me,这里就不是 0。
	wantMutual := map[uint64]uint32{hub: 1, c.plain: 0, c.rejected: 0, c.bystander: 0}
	for _, cand := range got {
		assert.Equal(t, wantMutual[cand.CandidatePlayerID], cand.MutualFriends,
			"候选 %d 的共同好友数", cand.CandidatePlayerID)
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

	// pivot、exclude、limit 三段同时在场:占位符顺序是 playerID(数共同好友)→ pivot → 窗口 W → 6 个 playerID → exclude → LIMIT。
	got, err = repo.recommendAnchor(ctx, c.me, []uint64{c.plain}, c.plain, 1)
	require.NoError(t, err)
	assert.Equal(t, []uint64{c.rejected}, recommendCandidateIDs(got))

	// 第一个 playerID 与 pivot 不能互换(都是 BIGINT,换了不报错)。pivot 取 hub 的 id:参数顺序对时,hub 与我有
	// 1 个共同好友(myFriend);两者互换后,窗口改从 me 的 id 起扫、第一个合格者仍是 hub,而子查询数的变成
	// "hub 的好友里有几个与 hub 有边"= hub 自己的好友数 8 —— id 对、数字错,所以两样一起断言。
	got, err = repo.recommendAnchor(ctx, c.me, nil, hub, 1)
	require.NoError(t, err)
	require.Equal(t, []uint64{hub}, recommendCandidateIDs(got))
	assert.Equal(t, uint32(1), got[0].MutualFriends, "hub 与我有 1 个共同好友(myFriend)")
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
// 不用 FLUSH STATUS:它要 RELOAD 权限,而且会把同一实例上**所有**活动会话的计数一起清零(见 measureSessionHandlerReads)。
// SHOW SESSION STATUS 与 SELECT CONNECTION_ID() 本身不产生 Handler_read(2026-09-28 / 09-29 在 MySQL 26.7.0 上核对)。
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

// measureSessionHandlerReads 在一条钉住的连接上执行纯读的 call,返回结果与**单次**调用在会话上产生的 Handler_read_* 增量。
//
// Handler 计数按会话统计:先把连接池钉成一条连接,并在每次测量前后各读一次 CONNECTION_ID —— 两次不同(连接被重建)
// 说明计数跨了会话,直接判失败,不给出一个假数。
//
// 先空跑一次、再连测两次并要求两次读数相同,防的是**别的会话的 FLUSH STATUS**:它会把同一实例上所有活动会话的计数
// 一起清零(2026-09-29 评审在 MySQL 26.7.0 上实测,约 30 次重放里有 5 次被别的会话打低)。清在两次 SHOW 之间,后读数
// 小于前读数 —— 不拦的话 uint64 差值回绕成约 1.8e19,被误报成"缺陷复发";清在查询中途,读数偏小,可能掩盖回退。
// 空跑让表首次打开时的十几次额外 read_key(冷表)不落进任何一次测量。
//
// 两次读数不同还有第二个原因,不是干扰而是计划本身换了:五条排除反连接的代价相同,它们的先后由优化器按代价排,
// 代价里含"索引有多少在 buffer pool 里"。同实例上别的读负载把夹具表的页挤出去时先后会翻,命中的那条排除从第 k 层
// 换到另一层,读数随之变(窗口饱和的用例每层差约 W 次;2026-10-08 评审实测:同一份数据、同一份统计,
// 35,891 ↔ 34,867)。翻转是一次性的,翻完即稳定,所以两次不等时再测一对,最多 measureRounds 轮;
// 每一轮的读数都记进日志。几轮都不等才判失败。返回"相邻两次相等"那一对里第一次测量的结果与读数。
func measureSessionHandlerReads(t *testing.T, ctx context.Context, db *sql.DB,
	call func() ([]RecommendCandidate, error)) ([]RecommendCandidate, uint64) {
	t.Helper()
	const (
		measureRounds = 3
		disturbed     = "会话 Handler 计数被外部干扰(最可能是同一实例上别的会话执行了 FLUSH STATUS,它会清零所有会话的计数)," +
			"本次读数无效:确认没有并发的 FLUSH STATUS 后重跑,不要放宽断言"
		unstable = "同一条纯读查询连测 %d 轮、每轮两次,读数始终不等(最后一轮 %d / %d)。两种可能:别的会话在执行 FLUSH STATUS;" +
			"或者同实例上有别的读负载在不断把夹具表的页挤出 buffer pool、反连接的先后来回翻。" +
			"在没有其它负载的实例上重跑,不要放宽断言"
	)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	_, err := call() // 空跑,不计数
	require.NoError(t, err)
	for round := 1; ; round++ {
		var got []RecommendCandidate
		var reads [2]uint64
		for i := range reads {
			connBefore, readsBefore := sessionHandlerReads(t, ctx, db)
			res, err := call()
			require.NoError(t, err)
			connAfter, readsAfter := sessionHandlerReads(t, ctx, db)
			require.Equal(t, connBefore, connAfter, "Handler 计数跨了会话(连接被重建),本次读数无效")
			require.GreaterOrEqual(t, readsAfter, readsBefore, "第 %d 轮第 %d 次测量:后读数小于前读数。%s", round, i+1, disturbed)
			reads[i] = readsAfter - readsBefore
			if i == 0 {
				got = res
			}
		}
		if reads[0] == reads[1] {
			return got, reads[0]
		}
		t.Logf("第 %d 轮(共 %d 轮)连测两次读数不等(%d / %d):反连接的先后可能刚翻过", round, measureRounds, reads[0], reads[1])
		require.Less(t, round, measureRounds, unstable, measureRounds, reads[0], reads[1])
	}
}

// recommendAnchorWithReads 用 measureSessionHandlerReads 测一次 recommendAnchor。
func recommendAnchorWithReads(t *testing.T, ctx context.Context, db *sql.DB, repo *FriendRepo,
	me uint64, exclude []uint64, pivot uint64, limit uint32) ([]RecommendCandidate, uint64) {
	t.Helper()
	return measureSessionHandlerReads(t, ctx, db, func() ([]RecommendCandidate, error) {
		return repo.recommendAnchor(ctx, me, exclude, pivot, limit)
	})
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
//     2026-10-09 起选择列表多了数共同好友的子查询,每返回一行另加 2F+1 次;本夹具的 me 没有好友(F=0),
//     所以只多 20 次,实测 2,170。F 不为 0 时的那一项由 TestRecommendAnchor_CountsRealMutualFriendsForOutputRowsOnly 守。
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
// 选择列表里数共同好友的子查询(m1 / m2,2026-10-09 加)也在这里守:访问方式、主键、possible_keys,外加
// "m2 的主键前缀是候选"—— 后者的理由见用例里那段注释。
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

	// 选择列表里数共同好友的子查询(recommendAnchor 第 6 条):m1 是 `player_id = me` 的主键前缀 ref,
	// m2 是 (候选, 我的好友) 的完整主键单行点查,两者都只许走主键。
	mutualWant := map[string]struct{ accessType, keyLen string }{"m1": {"ref", "8"}, "m2": {"eq_ref", "16"}}
	mutualSeen := map[string]bool{}
	for _, row := range plan {
		want, ok := mutualWant[row["table"]]
		if !ok {
			continue
		}
		mutualSeen[row["table"]] = true
		assert.Equal(t, "DEPENDENT SUBQUERY", row["select_type"], "%s 必须是按返回行求值的相关子查询", row["table"])
		assert.Equal(t, want.accessType, row["type"], "%s 的访问方式变了:数共同好友不再是「我的好友前缀 + 逐个主键点查」", row["table"])
		assert.Equal(t, "PRIMARY", row["key"], "%s 必须走主键", row["table"])
		assert.Equal(t, want.keyLen, row["key_len"], "%s 用到的主键列数不对", row["table"])
		assert.Equal(t, "PRIMARY", row["possible_keys"], "%s 的 FORCE INDEX (PRIMARY) 丢了", row["table"])
		if row["table"] == "m2" {
			// m2 的主键前缀必须是候选。把连接条件改成按我的好友去连(m2.player_id = m1.friend_player_id AND
			// m2.friend_player_id = c.player_id)在 MySQL 上计划同形、读数相同、结果也相同,这里的其它断言都还是绿的;
			// 但 TiDB 上相关列 c.player_id 不在索引前缀上就定不了位,m2 会变成每返回一行全扫一遍 friend
			// (2026-10-09 实测 88 万条边的库 17,600,000 行、首次执行 13.6 s,见 recommendAnchor 注释第 6 条 (b))。
			// ref 列形如 "c.player_id,<库名>.m1.friend_player_id":只看开头,不依赖库名(也不带逗号 —— STRAIGHT_JOIN
			// 被去掉、改由 m2 驱动时 ref 只剩 "c.player_id",那种情况由上面 type / key_len 的断言报,不该在这里再误报一次)。
			assert.True(t, strings.HasPrefix(row["ref"], "c.player_id"),
				"m2 的主键前缀必须是候选 c.player_id(实际 ref=%q):连接条件被改成按我的好友去连了?", row["ref"])
		}
	}
	for alias := range mutualWant {
		assert.True(t, mutualSeen[alias], "计划里缺了数共同好友的 %s(别名被改了,或子查询被改写成了别的形状)", alias)
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

// ── RecommendByMutual 扫描上界回归 ──────
// 两个缺陷各有回归:
//   - 2026-09-28 评审缺陷:OR 形 NOT EXISTS 让每个 FOF 行扫一遍全服 pending(读数随全服集合增长)→ M1 / M2 / M2b;
//   - 2026-10-08 缺陷:09-29 的修法对**每个 FOF 行**做五次点查,其中两次落在各候选自己的页上,排除表大于 buffer pool
//     时单次调用 38–51 s → 现在只对排名前 RecommendAnchorWindow 的候选点查,W1 / W2 / W3 / W4 守这个窗口。
//
// 四类排除 + 调用方 exclude + COUNT 排序的语义由上面的 TestRecommendByMutual_AppliesAllFourExclusions /
// TestRecommendByMutual_HonorsCallerExcludeAndLimit 覆盖(它们走的就是生产 SQL,候选远少于窗口,改写后原样适用),
// 这里不重复。M1 / M2 / M2b 共用 seedMutualAdversarialGraph,M1 顺带在这份大夹具上把全量结果逐人核对一遍;
// W1 / W2 共用 seedMutualWindowGraph;W3 / W4 的夹具很小,在用例里内联构造。

// recommendBulkRejectedRow 是一条终态申请行:status=3 即 rejected,与 testStatusRejected 同值。
const recommendBulkRejectedRow = "(?, ?, 1, 3, 1)"

// seedMutualAdversarialGraph 的规模与 id 段(与其它用例的 id 段不重叠)。
const (
	mutualAdvMe              uint64 = 60000000
	mutualAdvFriendBase      uint64 = 60000001 // 我的好友 60000001..60000030
	mutualAdvCandBase        uint64 = 60100000 // FOF 候选 60100000..60100299
	mutualAdvBlockerBase     uint64 = 61000000 // 拉黑我的局外人 61000000..61019999
	mutualAdvPendingBase     uint64 = 62000000 // 与我无关的 pending:62000000+k → 62100000+k
	mutualAdvFriends                = 30
	mutualAdvCandidates             = 300
	mutualAdvPerFriend              = 20 // 好友 i 连候选 (i*10+j) mod 300,j < 20:每个候选恰好经 2 个好友可达
	mutualAdvBlockers               = 20000
	mutualAdvGlobalPending          = 15000
	mutualAdvStarIndex              = 99 // star 与全部 30 个好友都是好友:共同好友数 30,必须排第一
	mutualAdvProductionLimit        = 20 // Friend.RecommendMaxLimit 的默认值:读数与计划都按生产上限取
)

// mutualAdversarialGraph 是评审对抗库的缩小版:"每个 FOF 候选都要走完排除判定、全服 pending 与拉黑我的人都很多"。
//   - 我有 F=30 个好友;每个好友连 20 个候选(每个候选经 2 个好友可达),star 经全部 30 个好友可达;好友 0 与好友 1
//     互为好友(二者因此也是 FOF,必须被"已是好友"排除)。FOF 行数 R = 30×20 + 28(star 补的边)+ 30(好友→我)+ 2 = 660。
//   - 四类排除各放在 FOF 候选上(每人都同时满足成为候选的全部条件,不出现的唯一原因是那条子句),外加调用方 exclude;
//     终态申请两个方向各一人作反向对照(必须出现)。
//   - 2 万个局外人拉黑我,且他们发给我的申请都已是终态(Block 会把双方 pending 置终态;这批终态行让
//     (to_player_id, status) 上的 to=me 区间变大,旧写法因此改走 status=1 的全服扫描 —— 评审库就是这个形状);
//     全服另有 1.5 万条与我无关的 pending。这两个集合都与 FOF 不相交,只贡献"规模"。
type mutualAdversarialGraph struct {
	me             uint64
	star           uint64
	callerExcluded uint64            // 调用方 exclude 里唯一的"真候选"
	callerExclude  []uint64          // 生产形状的 exclude:logic 层总会把 me 自己也放进去
	want           map[uint64]uint32 // 合格候选 → 期望的共同好友数
	excluded       map[uint64]string // 身在 FOF 里、应被排除的人 → 靠哪条子句
	friendCount    uint64            // F:f1 行数
	fofRows        uint64            // R:f2 行数(含"候选就是我自己"的那 F 行)
	blockersOfMe   uint64
	globalPending  uint64
}

func seedMutualAdversarialGraph(t *testing.T, ctx context.Context, db *sql.DB) mutualAdversarialGraph {
	t.Helper()
	me := mutualAdvMe
	friends := recommendIDRange(mutualAdvFriendBase, mutualAdvFriends)
	cands := recommendIDRange(mutualAdvCandBase, mutualAdvCandidates)
	star := cands[mutualAdvStarIndex]

	// 好友边一律双向写(与 AcceptFriend 落库的形状一致)。
	var edges [][2]uint64
	link := func(a, b uint64) { edges = append(edges, [2]uint64{a, b}, [2]uint64{b, a}) }
	for _, f := range friends {
		link(me, f)
	}
	linked := make(map[[2]uint64]bool)
	for i, f := range friends {
		for j := 0; j < mutualAdvPerFriend; j++ {
			c := cands[(i*10+j)%mutualAdvCandidates]
			link(f, c)
			linked[[2]uint64{f, c}] = true
		}
	}
	for _, f := range friends {
		if !linked[[2]uint64{f, star}] {
			link(f, star)
		}
	}
	link(friends[0], friends[1])
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkFriendPrefix, recommendBulkPairRow, edges)

	callerExcluded := cands[60]
	excluded := map[uint64]string{
		// me 不完全满足文件头"唯一原因"的纪律:按生产形状把 me 放进 exclude 的调用里,`<> ?` 与 NOT IN 两处都会排除他。
		// M1 的全量核对因此另跑一遍 exclude 不含 me 的,那一遍里 `<> ?` 是他不出现的唯一原因;
		// TestRecommendByMutual_AppliesAllFourExclusions(exclude = nil)也单独守着这一条。
		me:             "我自己(`<> ?` 自排除失效;exclude 里含 me 时还要 NOT IN 同时失效才会出现)",
		friends[0]:     "已是我的好友,经好友 1 可达(NOT EXISTS f 失效)",
		friends[1]:     "已是我的好友,经好友 0 可达(NOT EXISTS f 失效)",
		cands[10]:      "我拉黑的人(NOT EXISTS b_out 失效)",
		cands[11]:      "我拉黑的人(NOT EXISTS b_out 失效)",
		cands[20]:      "拉黑了我的人(NOT EXISTS b_in 失效)",
		cands[21]:      "拉黑了我的人(NOT EXISTS b_in 失效)",
		cands[30]:      "我发出的 pending(NOT EXISTS r_out 失效)",
		cands[40]:      "发给我的 pending(NOT EXISTS r_in 失效)",
		callerExcluded: "调用方 exclude(NOT IN 拼接失效)",
	}
	seedBlock(t, ctx, db, me, cands[10])
	seedBlock(t, ctx, db, me, cands[11])
	seedBlock(t, ctx, db, cands[20], me)
	seedBlock(t, ctx, db, cands[21], me)
	seedPending(t, ctx, db, me, cands[30])
	seedPending(t, ctx, db, cands[40], me)
	// 反向对照:与我只有终态申请,必须出现(status = 1 的过滤丢了就会被误排除)。
	seedRequestRow(t, ctx, db, me, cands[50], testStatusRejected, 1)
	seedRequestRow(t, ctx, db, cands[51], me, testStatusAccepted, 1)

	blockers := recommendIDRange(mutualAdvBlockerBase, mutualAdvBlockers)
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkBlockPrefix, recommendBulkPairRow, recommendPairsTo(blockers, me))
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkPendingPrefix, recommendBulkRejectedRow, recommendPairsTo(blockers, me))
	pending := make([][2]uint64, 0, mutualAdvGlobalPending)
	for k := uint64(0); k < mutualAdvGlobalPending; k++ {
		pending = append(pending, [2]uint64{mutualAdvPendingBase + k, mutualAdvPendingBase + 100000 + k})
	}
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkPendingPrefix, recommendBulkPendingRow, pending)

	want := make(map[uint64]uint32, len(cands))
	for _, c := range cands {
		if _, bad := excluded[c]; !bad {
			want[c] = 2
		}
	}
	want[star] = mutualAdvFriends

	g := mutualAdversarialGraph{
		me:             me,
		star:           star,
		callerExcluded: callerExcluded,
		callerExclude:  []uint64{me, callerExcluded},
		want:           want,
		excluded:       excluded,
		friendCount:    uint64(mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend WHERE player_id = ?", me)),
		fofRows: uint64(mustCount(t, ctx, db,
			"SELECT COUNT(*) FROM friend f1 JOIN friend f2 ON f2.player_id = f1.friend_player_id WHERE f1.player_id = ?", me)),
		blockersOfMe:  uint64(mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_block WHERE blocked_player_id = ?", me)),
		globalPending: uint64(mustCount(t, ctx, db, "SELECT COUNT(*) FROM friend_request WHERE status = 1")),
	}
	// 前置自检:每个"应被排除的人"都确实是 FOF 候选,否则对应的排除断言没有鉴别力(假绿)。
	for id, reason := range excluded {
		require.NotZero(t, mustCount(t, ctx, db,
			"SELECT COUNT(*) FROM friend f1 JOIN friend f2 ON f2.player_id = f1.friend_player_id WHERE f1.player_id = ? AND f2.friend_player_id = ?",
			me, id), "前置条件:%d(%s)必须是我好友的好友", id, reason)
	}
	return g
}

// mutualReadBound 是 RecommendByMutual 注释里推出的 Handler 读上界 3R + 6W + 3:只由 FOF 行数 R 与窗口 W 决定,
// 与我的好友数(式子里消掉了)、全服 pending 数、拉黑我的人数都无关。
func mutualReadBound(fofRows uint64) uint64 {
	return 3*fofRows + 6*uint64(RecommendAnchorWindow) + 3
}

// readBound 是本夹具的读数上界(本夹具 R=660:3×660 + 6×1024 + 3 = 8,127)。
func (g mutualAdversarialGraph) readBound() uint64 {
	return mutualReadBound(g.fofRows)
}

// assertCandidates 逐人核对结果:不重复、不含任何应被排除的人、共同好友数对;wantAll 时还要求一个合格者都不缺。
func (g mutualAdversarialGraph) assertCandidates(t *testing.T, got []RecommendCandidate, wantAll bool) {
	t.Helper()
	seen := make(map[uint64]bool, len(got))
	for _, c := range got {
		id := c.CandidatePlayerID
		if !assert.False(t, seen[id], "候选 %d 重复出现:GROUP BY 去重丢了", id) {
			continue
		}
		seen[id] = true
		if reason, bad := g.excluded[id]; bad {
			t.Errorf("结果里多出 %d:%s", id, reason)
			continue
		}
		wantMutual, ok := g.want[id]
		if !assert.True(t, ok, "结果里多出 %d:不在夹具的 FOF 候选里", id) {
			continue
		}
		assert.Equal(t, wantMutual, c.MutualFriends, "候选 %d 的共同好友数", id)
	}
	if wantAll {
		for id := range g.want {
			if !seen[id] {
				t.Errorf("结果里缺了合格候选 %d", id)
			}
		}
	}
}

// recommendByMutualWithReads 用 measureSessionHandlerReads 测一次 RecommendByMutual。
func recommendByMutualWithReads(t *testing.T, ctx context.Context, db *sql.DB, repo *FriendRepo,
	me uint64, exclude []uint64, limit uint32) ([]RecommendCandidate, uint64) {
	t.Helper()
	return measureSessionHandlerReads(t, ctx, db, func() ([]RecommendCandidate, error) {
		return repo.RecommendByMutual(ctx, me, exclude, limit)
	})
}

// assertMutualReadsAndResult 断言读数上界与结果;M1 与 M2b 共用。
//
// 读数按 limit = W 测,不按生产上限 20 测:同数候选每次调用进窗口前列的人不同(打散键),按 20 测时"凑满之前碰到几个
// 被排除者"是随机的,读数逐次浮动(2026-10-08 本夹具连测 30 次:2,057–2,062),measureSessionHandlerReads 的
// "连测两次读数相同"会被打破。limit = W 时窗口里每个候选都被判到底(命中第一条排除,或五条都不命中),只要五条
// 反连接的先后不变读数就是确定的(先后变了差几次,见 measureSessionHandlerReads),也正是上界里 5W 那一项的最坏形态。
// 本夹具候选数少于 W,所以这一遍同时是全量结果。
func assertMutualReadsAndResult(t *testing.T, ctx context.Context, db *sql.DB, repo *FriendRepo, g mutualAdversarialGraph) {
	t.Helper()
	bound := g.readBound()
	// 自检:拉黑我的人数与全服 pending 数都要大于上界 —— 任何"把这两个集合之一读一遍"的计划都必然越界,
	// 读数断言才对"退回按集合做排除"有鉴别力。
	require.Greater(t, g.blockersOfMe, bound, "前置条件:拉黑我的人数必须大于读数上界")
	require.Greater(t, g.globalPending, bound, "前置条件:全服 pending 数必须大于读数上界")
	require.Less(t, len(g.want)+len(g.excluded), int(RecommendAnchorWindow),
		"前置条件:本夹具的候选数必须少于窗口,limit = W 的那一遍才是全量结果")

	all, reads := recommendByMutualWithReads(t, ctx, db, repo, g.me, g.callerExclude, RecommendAnchorWindow)
	t.Logf("RecommendByMutual(limit = W)会话 Handler_read_* 增量 = %d(上界 3R+6W+3 = %d;F=%d R=%d W=%d;拉黑我 %d 人,全服 pending %d 条)",
		reads, bound, g.friendCount, g.fofRows, RecommendAnchorWindow, g.blockersOfMe, g.globalPending)
	assert.LessOrEqual(t, reads, bound,
		"读数越过了由 FOF 行数与窗口推出的上界:排除判定又在按全服 pending / 拉黑我的人的集合做了(2026-09-28 缺陷复发)")
	assert.Len(t, all, len(g.want), "候选少于窗口时必须一个合格者都不缺")
	g.assertCandidates(t, all, true)

	// 生产上限:取满、star 排第一。
	const limit = mutualAdvProductionLimit
	got, err := repo.RecommendByMutual(ctx, g.me, g.callerExclude, limit)
	require.NoError(t, err)
	require.Len(t, got, limit, "合格候选远多于 limit,必须取满")
	assert.Equal(t, g.star, got[0].CandidatePlayerID, "共同好友数最多的 star 必须排第一(ORDER BY mutual DESC)")
	g.assertCandidates(t, got, false)
}

// assertMutualPlanIsPerRowPrimaryKeyLookups 对生产 SQL 原文(recommendByMutualStatement 的产物)做 EXPLAIN,
// 断言上界所依赖的计划形状(2026-10-08 在 MySQL 26.7.0 上核对的原样):
//   - 外层恰好六行:驱动行是物化的派生表 <derivedN>(窗口),其余五行是 f / b_out / b_in / r_out / r_in,
//     每行都是 eq_ref / PRIMARY / key_len=16(每个窗口内候选一次单行点查;五行的先后由优化器按统计排,不断言);
//   - 派生表里恰好两行,先 f1 后 f2:f1 按 player_id = 常量做主键前缀 ref,f2 按 f1.friend_player_id 做主键前缀 ref;
//   - 七张真实表的 possible_keys 都只有 PRIMARY;没有 MATERIALIZED、没有 <subqueryN>、没有 hash join。
func assertMutualPlanIsPerRowPrimaryKeyLookups(t *testing.T, ctx context.Context, db *sql.DB, g mutualAdversarialGraph) {
	t.Helper()
	query, args := recommendByMutualStatement(g.me, g.callerExclude, mutualAdvProductionLimit)
	plan := explainTraditionalRows(t, ctx, db, inlineNumericArgs(t, query, args...))

	var outer, derived []map[string]string
	for _, row := range plan {
		table := row["table"]
		require.NotEqual(t, "MATERIALIZED", row["select_type"],
			"表 %s 被物化了(type=%s key=%s):扫描量会跟着被物化的集合走,不再由窗口封顶", table, row["type"], row["key"])
		require.False(t, strings.HasPrefix(table, "<subquery"),
			"计划里出现 %s:有一条排除被物化成集合再做反连接(SEMIJOIN(FIRSTMATCH) 提示丢了?)", table)
		assert.NotContains(t, row["Extra"], "join buffer", "表 %s 走了 hash join:那是对整个集合做反连接", table)
		switch row["select_type"] {
		case "DERIVED":
			derived = append(derived, row)
		default:
			outer = append(outer, row)
		}
	}

	// 内层:f1 驱动、f2 紧随。
	require.Len(t, derived, 2, "派生表里必须恰好是 f1、f2 两张表:%v", plan)
	f1, f2 := derived[0], derived[1]
	assert.Equal(t, "f1", f1["table"], "派生表的驱动表必须是 f1(我的好友,≤ MaxFriends 行);从 f2 起步就是全扫 friend 表(STRAIGHT_JOIN 丢了?)")
	assert.Equal(t, "f2", f2["table"], "f2 必须紧跟 f1")
	for _, row := range derived {
		assert.Equal(t, "PRIMARY", row["key"], "%s 必须走主键", row["table"])
		assert.Equal(t, "PRIMARY", row["possible_keys"], "%s 的 FORCE INDEX (PRIMARY) 丢了:优化器又能挑二级索引", row["table"])
		assert.Equal(t, "ref", row["type"], "%s 必须是主键前缀 ref", row["table"])
		assert.Equal(t, "8", row["key_len"], "%s 只用主键第一列 player_id", row["table"])
	}
	assert.Equal(t, "const", f1["ref"], "f1 的 player_id 必须是常量 me")
	assert.True(t, strings.HasSuffix(f2["ref"], ".f1.friend_player_id"), "f2 必须按 f1.friend_player_id 定位,实际 ref=%s", f2["ref"])

	// 外层:物化窗口驱动,五条排除各是一次主键单行点查。
	require.Len(t, outer, 6, "外层必须恰好是 <derivedN> + 五条排除:%v", plan)
	assert.True(t, strings.HasPrefix(outer[0]["table"], "<derived"),
		"外层的驱动行必须是物化的派生表(窗口),实际是 %s:派生表被合并进外层,窗口 LIMIT 就不再是点查次数的上界", outer[0]["table"])
	lookups := make([]string, 0, 5)
	for _, row := range outer[1:] {
		table := row["table"]
		lookups = append(lookups, table)
		assert.Equal(t, "eq_ref", row["type"], "%s 必须是每个窗口内候选一次单行点查", table)
		assert.Equal(t, "PRIMARY", row["key"], "%s 必须走主键", table)
		assert.Equal(t, "16", row["key_len"], "%s 必须用满两列主键", table)
		assert.Equal(t, "PRIMARY", row["possible_keys"], "%s 的 FORCE INDEX (PRIMARY) 丢了:优化器又能挑二级索引", table)
		assert.Contains(t, row["ref"], "c.candidate_id", "%s 必须按窗口里的候选定位,实际 ref=%s", table, row["ref"])
	}
	assert.ElementsMatch(t, []string{"f", "b_out", "b_in", "r_out", "r_in"}, lookups, "外层的五条排除必须恰好是这五个别名")
}

// TestRecommendByMutual_ReadsBoundedByFOFRowsNotGlobalSets(M1)是 2026-09-28 评审缺陷的确定性回归(先红后绿)。
// 缺陷:旧写法的两条 OR 形 NOT EXISTS 没有完整主键等值,friend_request 那条被做成"每个 FOF 行按 status=1 扫一遍全服
// pending"、friend_block 那条被做成对"我拉黑的 + 拉黑我的"全集的 hash antijoin,读数 ≈ FOF 行数 × 全服 pending 数。
// 断言:会话 Handler_read_* 增量 ≤ 3R + 6W + 3(本夹具 R=660,上界 8,127;按 limit = W 测,理由见 assertMutualReadsAndResult)——
// 生产上 R ≤ MaxFriends²,上界与全服 pending 数、拉黑我的人数无关;limit = W 的全量结果逐人正确;limit=20 取满且 star 第一。
//   - 最初写法(OR 形)为何必红:2026-09-29 按本夹具逐行重建(含 seedPending 的 updated_ms = 当前时间)后重放那条 SQL(服务端
//     预处理语句),计划是 idx_status_updated 上 status=1 的逐行 ref,读 9,334,150 次(Handler_read_next 9,291,954,即约 620 个
//     FOF 行各扫一遍 1.5 万条 pending;评审独立复测同一数字)、3.4–4.8 s,约为上界的 1,100 倍。结果与现在相同,所以红在读数断言。
//     统计不同时它也可能改走 index_merge + hash join(friend_explain_scratch 上就是),那样 union 要把"拉黑我的人"整个读一遍,
//     读数 ≥ 2 万,照样越界 —— 自检保证这两个集合都大于上界。OR 形条件没有完整主键可点查,做不出"每行常数次点查"的计划,
//     所以不论走哪种计划都红。
//   - 09-29 版(每个 FOF 行五次点查)在本夹具上是 4,737 次,**不红** —— 本夹具候选只有 302 个、少于窗口,区分不了它与现在的
//     写法;那一版的回归是 TestRecommendByMutual_ExclusionLookupsBoundedByWindow。
//   - 现在的写法(2026-10-08 实测):limit = W 时 3,446 次(key 2,150 / next 690 / rnd_next 606),limit=20 时 2,057–2,062 次。
//   - 全量核对再跑两遍:一遍按生产形状(exclude 含 me),一遍 exclude 不含 me —— 后一遍里 me 不出现的唯一原因是内层的
//     `<> ?`,删掉那条子句就红在"结果里多出 me"。
//
// 断言的是行操作计数,不看墙钟。
func TestRecommendByMutual_ReadsBoundedByFOFRowsNotGlobalSets(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	g := seedMutualAdversarialGraph(t, ctx, db)
	analyzeRecommendTables(t, ctx, db)

	assertMutualReadsAndResult(t, ctx, db, repo, g)

	// 全量核对:limit 放大到装得下全部合格者,每个合格者一个不缺、共同好友数都对,应排除的一个不出现。
	for _, exclude := range [][]uint64{g.callerExclude, {g.callerExcluded}} {
		all, err := repo.RecommendByMutual(ctx, g.me, exclude, uint32(len(g.want)+10))
		require.NoError(t, err)
		assert.Len(t, all, len(g.want), "exclude=%v", exclude)
		g.assertCandidates(t, all, true)
	}
}

// TestRecommendByMutual_PlanIsPerRowPrimaryKeyLookups(M2)钉住上界成立所依赖的计划形状(统计新鲜时),
// 断言见 assertMutualPlanIsPerRowPrimaryKeyLookups。夹具与 M1 相同:"FOF 行多、me 的关系集小"会诱使优化器先物化排除集。
//   - 最初写法(OR 形)为何必红(结构性的,与统计无关):OR 形子查询没有完整主键等值,friend_block / friend_request 不可能是
//     key_len=16 的 eq_ref;好友 NOT IN 被物化成 <subquery2>;也没有派生表。2026-09-29 在本夹具上实测:
//     <subquery2> MATERIALIZED、b 是 index_merge + hash join、r 是 idx_status_updated 上 status=1 的 ref。
//   - 09-29 版(每个 FOF 行五次点查)为何必红:它没有派生表,f1 / f2 与五条排除同在一层,红在"派生表里必须恰好是 f1、f2"。
//   - 只去掉 SEMIJOIN(FIRSTMATCH):f / b_out / r_out 被物化(出现 <subqueryN>)→ 红;两个提示都去掉:再多一个 r_in 被物化 → 红;
//     去掉任何一处 FORCE INDEX (PRIMARY):possible_keys 多出二级索引 → 红(这一条守的是防线,不是已实测到的退化,
//     见 RecommendByMutual 注释第 5 条)。以上 2026-10-08 在本夹具上逐项实测。
//   - 本用例照绿、要靠别的用例守的两处:去掉 STRAIGHT_JOIN(统计新鲜时计划不变)由
//     TestRecommendByMutual_JoinOrderSurvivesStaleStatistics 守;去掉派生表里的 LIMIT(带 GROUP BY 的派生表照样被物化,
//     EXPLAIN 看不出差别)由 TestRecommendByMutual_ExclusionLookupsBoundedByWindow / _StopsAtWindowWhenSaturated 守。
func TestRecommendByMutual_PlanIsPerRowPrimaryKeyLookups(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	g := seedMutualAdversarialGraph(t, ctx, db)
	analyzeRecommendTables(t, ctx, db)

	assertMutualPlanIsPerRowPrimaryKeyLookups(t, ctx, db, g)
}

// TestRecommendByMutual_JoinOrderSurvivesStaleStatistics(M2b)钉住 STRAIGHT_JOIN(先红后绿):持久统计陈旧时,
// 计划仍是"派生表里先 f1 后 f2 + 窗口内逐候选主键点查",读数仍在上界内。SEMIJOIN(FIRSTMATCH) 在本用例造出的状态下
// 与 M2 一样只靠计划断言守(见下)。
//
// 陈旧统计的造法:建表后、灌数前关掉三张表的 STATS_AUTO_RECALC,灌完不 ANALYZE —— 持久统计停在建表时的空表状态
// (n_diff = 0)。n_diff = 0 时,按 f1.friend_player_id 做的 f2 前缀 ref 每次都被估成整表行数(rec_per_key 取表行数;
// f1 是常量前缀 ref,走 index dive,估得准),优化器于是改成"f2 全索引扫描驱动 + f1 主键点查"。
// 不需要改 mysql.innodb_*_stats,也不需要 FLUSH(RELOAD 权限)。
// 用例先自检:把生产 SQL 的 STRAIGHT_JOIN 换回普通 JOIN 再 EXPLAIN,派生表的驱动表必须变成 f2 —— 证明夹具确实造出了会让
// 连接顺序翻转的陈旧统计;没有这一步,"f1 在前"在统计没造坏时也照绿(假绿)。
// 2026-10-08 按本用例的做法(建表 → 关 STATS_AUTO_RECALC → 灌数,不 ANALYZE、不 FLUSH;EXPLAIN 里 f2 rows = 1,318,
// 即"被估成整表行数")在 SQL 级逐项重放,读数按 limit = W:
//   - 现在的写法:计划与统计新鲜时同形(只是外层五条点查的先后不同),3,445 次。
//   - 去掉 STRAIGHT_JOIN:派生表改由 f2 驱动(PRIMARY 全索引扫描,type=index)、f1 变成 eq_ref → 红在计划断言。读数 4,375,
//     **没有**越过上界 8,127 —— 本夹具 friend 表只有 1,318 行,整表读一遍也不多,所以这一条只靠计划断言和上面的自检守;
//     116 万边的库上同一段连接的退化是 1,529,004 次(见 RecommendByMutual 注释第 1 条)。
//   - 去掉 SEMIJOIN(FIRSTMATCH):与统计新鲜时一样,f / b_out / r_out 被物化(出现 <subqueryN>),b_in / r_in 仍是 eq_ref;
//     读数 3,482,不越界 → 只红在计划断言。
//   - 两个提示都去掉:再多一个 r_in 被物化(idx_to_player 上 to = me、status = 1),读数 3,487,不越界 → 只红在计划断言。
//   - 最初写法(OR 形):驱动表是 f2(idx_friend_player 上的 range),约 930 万次(2026-09-29 实测 9,335,651)→ 两处都红。
//
// 本用例**进不去**的另一种陈旧状态:持久统计里的 n_rows = 0 被重新读回内存(对这几张表 FLUSH TABLES,或 mysqld 重启;
// EXPLAIN 里 f2 rows = 1)。那种状态下去掉 SEMIJOIN 提示才会把 b_in 物化成 friend_block 主键全索引扫描、r_in 物化成
// friend_request 全表扫描(同一份数据 58,462 次读;两个提示都去掉 38,454 次),现在的写法仍是 3,443 次。
// 造出它需要 FLUSH TABLES(RELOAD / FLUSH_TABLES 权限),本用例不做;这层保护目前只有 M2 / M2b 的计划断言
// (去掉提示必然出现 <subqueryN>)间接守着。
func TestRecommendByMutual_JoinOrderSurvivesStaleStatistics(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	tables := []string{"friend", "friend_block", "friend_request"}
	for _, table := range tables {
		_, err := db.ExecContext(ctx, "ALTER TABLE "+table+" STATS_AUTO_RECALC=0")
		require.NoError(t, err)
	}
	// 恢复默认,不把"关掉自动重算"的表留给后来的人工排障(下一条用例反正会 DROP 重建)。
	// t.Cleanup 是 LIFO:这里在 openFriendTestDB 的 TRUNCATE / Close 之后注册,所以先于它们执行,连接还开着。
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, table := range tables {
			if _, err := db.ExecContext(cleanupCtx, "ALTER TABLE "+table+" STATS_AUTO_RECALC=DEFAULT"); err != nil {
				t.Logf("恢复 %s 的 STATS_AUTO_RECALC 失败(不影响本次结论): %v", table, err)
			}
		}
	})
	g := seedMutualAdversarialGraph(t, ctx, db)
	// 刻意不 ANALYZE。

	query, args := recommendByMutualStatement(g.me, g.callerExclude, mutualAdvProductionLimit)
	probe := strings.Replace(query, "STRAIGHT_JOIN", "JOIN", 1)
	probePlan := explainTraditionalRows(t, ctx, db, inlineNumericArgs(t, probe, args...))
	var probeDerived []string
	for _, row := range probePlan {
		if row["select_type"] == "DERIVED" {
			probeDerived = append(probeDerived, row["table"])
		}
	}
	require.Equal(t, []string{"f2", "f1"}, probeDerived,
		"前置条件:去掉 STRAIGHT_JOIN 的同一条 SQL 在陈旧统计下,派生表必须改由 f2 驱动,否则夹具没造出陈旧统计、本用例没有鉴别力"+
			"(先核对 innodb_stats_persistent 是否为 ON)")

	assertMutualPlanIsPerRowPrimaryKeyLookups(t, ctx, db, g)
	assertMutualReadsAndResult(t, ctx, db, repo, g)
}

// ── RecommendByMutual 窗口回归(2026-10-08 缺陷:09-29 版对每个 FOF 行做五次点查,排除表大于 buffer pool 时 38–51 s)──────

// seedMutualWindowGraph 的规模与 id 段(与其它用例的 id 段不重叠)。
const (
	mutualWinMe         uint64 = 63000000
	mutualWinFriendBase uint64 = 63000001 // 我的好友 63000001..63000016
	mutualWinTopBase    uint64 = 63100000 // 头部:W 个候选,各经 2 个好友可达(共同好友数 2)
	mutualWinTailBase   uint64 = 63200000 // 尾部:8×W 个候选,各经 1 个好友可达(共同好友数 1)
	mutualWinFriends           = 16
	mutualWinTailFactor        = 8
)

// mutualWindowGraph 是"候选远多于窗口"的夹具:头部恰好 W 人(共同好友数 2,正好占满排名窗口),尾部 8×W 人
// (共同好友数 1)。候选互不相同,所以 FOF 行数 R = 16(好友→我)+ 2W + 8W = 10,256,去重候选 9W = 9,216。
// 没有任何人与我有排除关系(由调用它的用例按需添加)。
type mutualWindowGraph struct {
	me         uint64
	top        []uint64 // 排名前 W 的候选
	survivor   uint64   // 头部里留作"窗口内唯一合格者"的那一个
	candidates uint64   // 去重候选数
	fofRows    uint64   // R:f2 行数(含"候选就是我自己"的那 F 行)
}

func seedMutualWindowGraph(t *testing.T, ctx context.Context, db *sql.DB) mutualWindowGraph {
	t.Helper()
	w := int(RecommendAnchorWindow)
	me := mutualWinMe
	friends := recommendIDRange(mutualWinFriendBase, mutualWinFriends)
	top := recommendIDRange(mutualWinTopBase, w)
	tail := recommendIDRange(mutualWinTailBase, mutualWinTailFactor*w)

	// 好友边一律双向写(与 AcceptFriend 落库的形状一致)。
	edges := make([][2]uint64, 0, 2*(len(friends)+2*len(top)+len(tail)))
	link := func(a, b uint64) { edges = append(edges, [2]uint64{a, b}, [2]uint64{b, a}) }
	for _, f := range friends {
		link(me, f)
	}
	for j, c := range top {
		link(friends[j%mutualWinFriends], c)
		link(friends[(j+1)%mutualWinFriends], c)
	}
	for k, c := range tail {
		link(friends[k%mutualWinFriends], c)
	}
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkFriendPrefix, recommendBulkPairRow, edges)
	seedRecommendUnrelatedExclusionRows(t, ctx, db)

	g := mutualWindowGraph{
		me:       me,
		top:      top,
		survivor: top[w/2],
		candidates: uint64(mustCount(t, ctx, db,
			"SELECT COUNT(DISTINCT f2.friend_player_id) FROM friend f1 JOIN friend f2 ON f2.player_id = f1.friend_player_id"+
				" WHERE f1.player_id = ? AND f2.friend_player_id <> ?", me, me)),
		fofRows: uint64(mustCount(t, ctx, db,
			"SELECT COUNT(*) FROM friend f1 JOIN friend f2 ON f2.player_id = f1.friend_player_id WHERE f1.player_id = ?", me)),
	}
	// 前置自检(候选数是从库里数出来的,不是切片长度):头部 + 尾部确实都成了我的 FOF 候选;候选远多于窗口;
	// "对每个候选做五次点查"的读数必然越过上界 —— 否则读数断言区分不了 09-29 版(每个 FOF 行都点查)与现在的写法
	// (只点查窗口内的人)。
	require.Equal(t, uint64(len(top)+len(tail)), g.candidates, "前置条件:头部 W 人与尾部 8×W 人都必须是我好友的好友")
	require.Greater(t, g.candidates, 4*uint64(RecommendAnchorWindow), "前置条件:候选数必须远多于窗口")
	require.Greater(t, 5*g.candidates, mutualReadBound(g.fofRows),
		"前置条件:5 × 候选数必须大于读数上界,读数断言才对「点查次数随候选数增长」有鉴别力")
	return g
}

// TestRecommendByMutual_ExclusionLookupsBoundedByWindow(W1)是 2026-10-08 缺陷的确定性回归(先红后绿)。
// 缺陷:09-29 版把五条排除点查放在 f2 的每一行上,点查次数 = 5 × FOF 行数;其中 b_in / r_in 落在各候选自己的叶子页上,
// 排除表大于 buffer pool 时单次调用约 7.6 万次页读、38–51 s。现在只对排名前 W 的候选点查。
// 断言:limit=20 的结果取满、全部来自头部(共同好友数 2);会话 Handler_read_* 增量 ≤ 3R + 6W + 3(本夹具 36,915)。
//   - 09-29 版为何必红:2026-10-08 按本夹具逐行重建后重放那条 SQL,读 80,946 次(10,240 个非自身的 FOF 行各做五次点查;
//     头部每人经两个好友可达,被点查两遍),结果与现在相同,所以红在读数断言;
//   - 现在的写法:30,871 次(排名前 20 都合格,只做 100 次点查;其余是内层的连接、分组与窗口物化)。
//
// 第二条断言更紧,守"外层先按 (mutual, shuffle) 排序、凑够 limit 即停":排名前 limit 都合格时点查只有 5 × limit 次,
// 读数 ≤ 3R + W + 3 + 5×limit(本夹具 31,895)。把外层排序键写成 `ORDER BY c.mutual DESC, RAND()` 时优化器改成
// "对窗口里全部 W 人做完点查再排序",本夹具读 35,891 → 红在这一条(第一条的上界 36,915 拦不住它)。
//
// 本夹具头部恰好 W 人、全部合格,所以 limit=20 时点查次数固定为 100,读数是确定的(与 assertMutualReadsAndResult 里
// "按 20 测会浮动"的那个夹具不同)。断言的是行操作计数,不看墙钟;冷缓存下的耗时差距见 RecommendByMutual 注释的实测表。
func TestRecommendByMutual_ExclusionLookupsBoundedByWindow(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	g := seedMutualWindowGraph(t, ctx, db)
	analyzeRecommendTables(t, ctx, db)

	const limit = mutualAdvProductionLimit
	bound := mutualReadBound(g.fofRows)
	got, reads := recommendByMutualWithReads(t, ctx, db, repo, g.me, []uint64{g.me}, limit)
	t.Logf("RecommendByMutual 会话 Handler_read_* 增量 = %d(上界 3R+6W+3 = %d;R=%d W=%d;去重候选 %d 个)",
		reads, bound, g.fofRows, RecommendAnchorWindow, g.candidates)

	require.Len(t, got, limit, "头部 W 人全部合格,必须取满")
	inTop := make(map[uint64]bool, len(g.top))
	for _, id := range g.top {
		inTop[id] = true
	}
	seen := make(map[uint64]bool, len(got))
	for _, c := range got {
		assert.False(t, seen[c.CandidatePlayerID], "候选 %d 重复出现", c.CandidatePlayerID)
		seen[c.CandidatePlayerID] = true
		assert.True(t, inTop[c.CandidatePlayerID], "候选 %d 不在头部:共同好友数 2 的人没排在 1 的人前面", c.CandidatePlayerID)
		assert.Equal(t, uint32(2), c.MutualFriends, "候选 %d 的共同好友数", c.CandidatePlayerID)
	}
	assert.LessOrEqual(t, reads, bound,
		"读数越过了由 FOF 行数与窗口推出的上界:排除点查又在对每个候选做了,而不是只对排名窗口内的人(2026-10-08 缺陷复发)")
	// 排名前 limit 都合格(本夹具头部全部合格)时,点查只有 5 × limit 次。
	earlyStopBound := 3*g.fofRows + uint64(RecommendAnchorWindow) + 3 + 5*uint64(limit)
	assert.LessOrEqual(t, reads, earlyStopBound,
		"排名前 %d 名都合格,读数却超过了 3R + W + 3 + 5×limit = %d:外层没有先排序、凑够 limit 即停,"+
			"而是对窗口里的人做完了点查(外层排序键不是物化列 c.shuffle 了?)", limit, earlyStopBound)
}

// TestRecommendByMutual_StopsAtWindowWhenSaturated(W2)钉住窗口的语义:只在排名前 W 名里挑,窗口里的人被排除光了
// 就返回偏少 / 返回空,**不**越过窗口去尾部找(由 logic 的 random 兜底补足)。这一条与执行计划无关:
//   - 头部 W 人里除 survivor 外全部拉黑我 → 只返回 survivor 一人;
//   - survivor 也拉黑我 → 返回空,尾部 8×W 个合格候选(共同好友数 1)一个都不出现。
//
// 09-29 版与去掉派生表 LIMIT 的写法都会越过头部、从尾部凑满 20 人 → 红在条数断言(2026-10-08 SQL 级重放:
// 09-29 版两个阶段都返回 20 行)。两个阶段的读数都 ≤ 3R + 6W + 3:"拉黑了我"那条排除排在第 k 层时约
// 30,771 + k×W,2026-10-08 实测过 35,891(k=5)与 34,867 / 34,868(k=4,buffer pool 吃紧时反连接的先后会翻,
// 见 measureSessionHandlerReads),都在上界内。
func TestRecommendByMutual_StopsAtWindowWhenSaturated(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	g := seedMutualWindowGraph(t, ctx, db)

	blockers := make([]uint64, 0, len(g.top)-1)
	for _, id := range g.top {
		if id != g.survivor {
			blockers = append(blockers, id)
		}
	}
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkBlockPrefix, recommendBulkPairRow, recommendPairsTo(blockers, g.me))
	analyzeRecommendTables(t, ctx, db)

	// 正向对照:尾部 8×W 人确实是窗口之外的**合格**候选(是我好友的好友,且与我没有任何拉黑 / 申请关系)——
	// 否则"返回空"也可能只是因为尾部没灌进去或者被别的子句排除了,断言就没有鉴别力。
	tailCandidates := mustCount(t, ctx, db,
		"SELECT COUNT(DISTINCT f2.friend_player_id) FROM friend f1 JOIN friend f2 ON f2.player_id = f1.friend_player_id"+
			" WHERE f1.player_id = ? AND f2.friend_player_id >= ?", g.me, mutualWinTailBase)
	require.EqualValues(t, mutualWinTailFactor*len(g.top), tailCandidates, "前置条件:尾部 8×W 人必须都是我好友的好友")
	require.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_block WHERE (player_id = ? AND blocked_player_id >= ?) OR (blocked_player_id = ? AND player_id >= ?)",
		g.me, mutualWinTailBase, g.me, mutualWinTailBase), "前置条件:尾部的人与我之间不能有拉黑")
	require.Zero(t, mustCount(t, ctx, db,
		"SELECT COUNT(*) FROM friend_request WHERE (from_player_id = ? AND to_player_id >= ?) OR (to_player_id = ? AND from_player_id >= ?)",
		g.me, mutualWinTailBase, g.me, mutualWinTailBase), "前置条件:尾部的人与我之间不能有申请")

	const limit = mutualAdvProductionLimit
	bound := mutualReadBound(g.fofRows)

	got, reads := recommendByMutualWithReads(t, ctx, db, repo, g.me, []uint64{g.me}, limit)
	t.Logf("窗口内只剩 1 个合格者:读数 %d(上界 %d)", reads, bound)
	require.Len(t, got, 1, "窗口里只剩 survivor 一个合格者:多出来的人只能来自窗口之外(派生表的 LIMIT 丢了)")
	assert.Equal(t, g.survivor, got[0].CandidatePlayerID)
	assert.Equal(t, uint32(2), got[0].MutualFriends)
	assert.LessOrEqual(t, reads, bound, "窗口饱和时读数也必须在上界内")

	seedBlock(t, ctx, db, g.survivor, g.me)
	got, reads = recommendByMutualWithReads(t, ctx, db, repo, g.me, []uint64{g.me}, limit)
	t.Logf("窗口全被排除:读数 %d(上界 %d)", reads, bound)
	assert.Empty(t, got, "排名前 W 的人全被排除时必须返回空:返回了人说明越过了窗口(派生表的 LIMIT 丢了)")
	assert.LessOrEqual(t, reads, bound, "窗口饱和时读数也必须在上界内")
}

// TestRecommendAnchor_CountsRealMutualFriendsForOutputRowsOnly 钉住 2026-10-09 的修正(先红后绿):random 兜底给每个
// 返回的候选数出真实的共同好友数,而且只对**返回的**行数。放在 W2 后面,是因为它测的正是 W2 之后发生的事。
//
// 场景就是此前会显示错的那一种:seedMutualWindowGraph 的头部 W 人全部拉黑我,mutual 的排名窗口被占满、返回空
// (W2 的第二阶段),能出人的只剩兜底;兜底从尾部挑到的人都是我好友的好友(各经 1 个好友可达)。
//   - 修正前(选择列表写死 `0 AS mutual`):尾部的人返回时共同好友数是 0 → 红在数字断言;
//   - 现在:每人是 1。对照是头部没被拉黑时从头部起扫,每人是 2 —— 数字不是写死的 1。
//
// 读数断言守"子查询只对返回的行求值":每返回一行 2F+1 次读(F = 我的好友数,本夹具 16),limit=20 共 660 次,
// 整条 ≤ 8×W + limit×(2F+1) = 8,852。2026-10-09 在按本夹具逐行重建的库上实测 2,811 次(窗口生产 + 物化表扫描
// 2,051、点查 5×20、子查询 660);把外层排序改成 `ORDER BY mutual DESC, c.player_id` 后,子查询要对窗口里 W 个人
// 全部求值才能排序,同一份数据 40,963 次 → 红在读数断言。结果不变(每人仍是 1),所以只有读数看得出来。
func TestRecommendAnchor_CountsRealMutualFriendsForOutputRowsOnly(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)
	g := seedMutualWindowGraph(t, ctx, db)
	analyzeRecommendTables(t, ctx, db)

	const limit = mutualAdvProductionLimit

	// 对照:头部还没被拉黑时从头部第一人起扫 —— 每人经 2 个好友可达。
	got, err := repo.recommendAnchor(ctx, g.me, []uint64{g.me}, mutualWinTopBase, limit)
	require.NoError(t, err)
	require.Equal(t, g.top[:limit], recommendCandidateIDs(got), "前置条件:头部前 %d 人都合格,按 id 升序返回", limit)
	for _, c := range got {
		assert.Equal(t, uint32(2), c.MutualFriends, "头部候选 %d 经 2 个好友可达", c.CandidatePlayerID)
	}

	// 头部 W 人全部拉黑我:mutual 的排名窗口被占满,只能靠兜底。
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkBlockPrefix, recommendBulkPairRow, recommendPairsTo(g.top, g.me))
	analyzeRecommendTables(t, ctx, db)
	mutual, err := repo.RecommendByMutual(ctx, g.me, []uint64{g.me}, limit)
	require.NoError(t, err)
	require.Empty(t, mutual, "前置条件:排名窗口全被拉黑我的人占满时 mutual 必须返回空,否则本用例测的不是「只能靠兜底」的场景")

	tail := recommendIDRange(mutualWinTailBase, limit)
	got, reads := recommendAnchorWithReads(t, ctx, db, repo, g.me, []uint64{g.me}, mutualWinTailBase, limit)
	require.Equal(t, tail, recommendCandidateIDs(got), "尾部的人与我没有任何排除关系:从尾部第一人起扫必须按 id 升序取满")
	for _, c := range got {
		assert.Equal(t, uint32(1), c.MutualFriends,
			"尾部候选 %d 是我好友的好友(经 1 个好友可达):兜底路径又把共同好友数填成了别的值", c.CandidatePlayerID)
	}

	w := uint64(RecommendAnchorWindow)
	perRow := 2*uint64(mutualWinFriends) + 1
	bound := 8*w + uint64(limit)*perRow
	t.Logf("recommendAnchor 会话 Handler_read_* 增量 = %d(上界 8×W + limit×(2F+1) = %d;F=%d)", reads, bound, mutualWinFriends)
	// 自检:对窗口里每个人都求值的读数必须越过上界,读数断言才分得出"只对返回的行"与"对整个窗口"。
	require.Greater(t, w*perRow, bound, "前置条件:W×(2F+1) 必须大于读数上界")
	assert.LessOrEqual(t, reads, bound,
		"读数越过了上界:数共同好友的子查询不再只对返回的 %d 行求值(外层排序依赖 mutual 了?见 recommendAnchor 注释第 6 条 (a))", limit)
}

// TestRecommendByMutual_TiesAreShuffledPerCall(W3)夹具的 id 段。
const (
	mutualTieMe       uint64 = 64000000
	mutualTieFriendA  uint64 = 64000001
	mutualTieFriendB  uint64 = 64000002
	mutualTieFriendC  uint64 = 64000003
	mutualTieHeadBase uint64 = 64100000 // 头部 3 人:经好友 A、B 可达,共同好友数 2
	mutualTiePoolBase uint64 = 64200000 // 同分池 40 人:只经好友 C 可达,共同好友数 1
	mutualTieHead            = 3
	mutualTiePool            = 40
)

// TestRecommendByMutual_TiesAreShuffledPerCall(W3)钉住"同数随机":共同好友数相同的候选,每次调用的先后顺序不同
// (客户端"换一批"靠它在共同好友数扁平的图上换出人来),而且打散不能破坏"共同好友数降序"。
// 夹具:3 个共同好友数 2 的头部 + 40 个共同好友数 1 的同分池,limit=20 → 每次都应是"头部 3 人 + 池里任取 17 人"。
// 连调 8 次:每次的结果都合法;8 次的序列不能全都相同。
//   - 打散键本身丢了(内层的 `RAND() AS shuffle` 换成确定的列 / 常量):窗口与顺序都变成确定的,8 次完全相同 → 红;
//   - 本用例**守不住**"只把内层 ORDER BY 里的 shuffle 去掉":43 个候选全在窗口里,外层仍按 c.shuffle 打散,8 次照样
//     8 种序列(2026-10-08 评审的 SQL 级重放)。那种改法坏的是"同数候选多于窗口时每次进窗口的人不同",由
//     TestRecommendByMutual_WindowMembershipRotatesPerCall(W4)守;
//   - 内外打散键不一致以致乱序(例如外层排序键写错):头部 3 人不在前 3 位 → 红。
//
// 随机性断言的误报概率:17 个位置从 40 人里有序抽取,共 40!/23! ≈ 3×10^25 种;8 次独立抽取全相同的概率约 10^-178。
// 2026-10-08 SQL 级重放:8 次调用 8 种序列,头部 3 人每次都在前 3 位。
func TestRecommendByMutual_TiesAreShuffledPerCall(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	me := mutualTieMe
	head := recommendIDRange(mutualTieHeadBase, mutualTieHead)
	pool := recommendIDRange(mutualTiePoolBase, mutualTiePool)
	var edges [][2]uint64
	link := func(a, b uint64) { edges = append(edges, [2]uint64{a, b}, [2]uint64{b, a}) }
	for _, f := range []uint64{mutualTieFriendA, mutualTieFriendB, mutualTieFriendC} {
		link(me, f)
	}
	for _, c := range head {
		link(mutualTieFriendA, c)
		link(mutualTieFriendB, c)
	}
	for _, c := range pool {
		link(mutualTieFriendC, c)
	}
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkFriendPrefix, recommendBulkPairRow, edges)
	seedRecommendUnrelatedExclusionRows(t, ctx, db)
	analyzeRecommendTables(t, ctx, db)

	inHead := make(map[uint64]bool, len(head))
	for _, id := range head {
		inHead[id] = true
	}
	inPool := make(map[uint64]bool, len(pool))
	for _, id := range pool {
		inPool[id] = true
	}

	const (
		limit = mutualAdvProductionLimit
		calls = 8
	)
	sequences := make(map[[limit]uint64]bool, calls)
	for i := 0; i < calls; i++ {
		got, err := repo.RecommendByMutual(ctx, me, []uint64{me}, limit)
		require.NoError(t, err)
		require.Len(t, got, limit, "第 %d 次:头部 3 人 + 同分池 40 人,必须取满", i+1)
		var seq [limit]uint64
		seen := make(map[uint64]bool, limit)
		for pos, c := range got {
			id := c.CandidatePlayerID
			seq[pos] = id
			require.False(t, seen[id], "第 %d 次:候选 %d 重复出现", i+1, id)
			seen[id] = true
			if pos < mutualTieHead {
				assert.True(t, inHead[id], "第 %d 次:第 %d 位必须是共同好友数 2 的头部,实际是 %d(打散破坏了降序)", i+1, pos+1, id)
				assert.Equal(t, uint32(2), c.MutualFriends, "第 %d 次:候选 %d 的共同好友数", i+1, id)
			} else {
				assert.True(t, inPool[id], "第 %d 次:第 %d 位必须来自同分池,实际是 %d", i+1, pos+1, id)
				assert.Equal(t, uint32(1), c.MutualFriends, "第 %d 次:候选 %d 的共同好友数", i+1, id)
			}
		}
		sequences[seq] = true
	}
	assert.Greater(t, len(sequences), 1,
		"同数候选连调 %d 次返回了完全相同的序列:打散键(内层的 RAND() AS shuffle)丢了,客户端「换一批」换不出人", calls)
}

// TestRecommendByMutual_WindowMembershipRotatesPerCall(W4)钉住"同数候选多于窗口时,每次调用进窗口的人不同":
// 谁进窗口由内层 ORDER BY 里的 shuffle 决定。去掉它(`RAND() AS shuffle` 列与外层排序都保留)时,窗口里的人固定不变,
// 窗口之外的同数候选永远推荐不到 —— W3 测不出来(它的候选全在窗口里),2026-10-08 评审在 3.98 万个同数候选的库上
// 实测:那样改之后连调三次是同一批人,原写法三次两两只重叠约 30 人。
// 夹具:1 个好友带 2×W 个同数候选(共同好友数都是 1),没有任何排除;limit = W 时返回的就是整个窗口。连调两次,
// 两个集合必须不同(从 2W 人里取 W 人,两次恰好相同的概率约 10^-615)。
func TestRecommendByMutual_WindowMembershipRotatesPerCall(t *testing.T) {
	db, ctx := openFriendTestDB(t)
	repo, _ := newFriendTestRepo(t, db)

	const (
		me       uint64 = 65000000
		myFriend uint64 = 65000001
		poolBase uint64 = 65100000
	)
	w := int(RecommendAnchorWindow)
	pool := recommendIDRange(poolBase, 2*w)
	edges := make([][2]uint64, 0, 2*(1+len(pool)))
	link := func(a, b uint64) { edges = append(edges, [2]uint64{a, b}, [2]uint64{b, a}) }
	link(me, myFriend)
	for _, c := range pool {
		link(myFriend, c)
	}
	bulkInsertRecommendPairs(t, ctx, db, recommendBulkFriendPrefix, recommendBulkPairRow, edges)
	seedRecommendUnrelatedExclusionRows(t, ctx, db)
	analyzeRecommendTables(t, ctx, db)

	var windows [2]map[uint64]bool
	for i := range windows {
		got, err := repo.RecommendByMutual(ctx, me, []uint64{me}, RecommendAnchorWindow)
		require.NoError(t, err)
		require.Len(t, got, w, "第 %d 次:2×W 个同数候选、无人被排除,limit = W 必须取满整个窗口", i+1)
		windows[i] = make(map[uint64]bool, len(got))
		for _, c := range got {
			windows[i][c.CandidatePlayerID] = true
		}
	}
	assert.NotEqual(t, windows[0], windows[1],
		"同数候选多于窗口时,连调两次进窗口的是同一批人:内层 ORDER BY 里的 shuffle 丢了,窗口外的同数候选永远推荐不到")
}
