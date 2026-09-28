package data

import (
	"context"
	"database/sql"
	"fmt"
	// math/rand/v2 而不是 v1:pivot 要在 uint64 区间里取随机,v2 的 Uint64N 直接吃 uint64,
	// v1 只有 Int63n —— 那要先把区间宽度窄化成 int64,窄化本身就是一处静默错误源。
	// 推荐用不着可复现的随机序列,所以全局默认源(自带种子)足够,不引入自己的 *rand.Rand。
	"math/rand/v2"
	"strings"
)

// 好友推荐的 SQL 侧(移植自 A 仓 services/social/friend/internal/data/friend_repo.go 的
// RecommendByMutual / RecommendRandom / recommendAnchor,表名与列名换成本仓 friend 独占库的形态)。
//
// 动手改之前先读懂这四条约束,它们决定了本文件为什么长这样:
//
//  1. **推荐是纯读路径:不进事务、不加容量守卫**(规格 §3.7)。它读的 friend / friend_block /
//     friend_request 三张表随时可能正被写路径改,本文件几条查询之间也没有任何快照一致性。
//     也就是说:**推荐结果允许轻微陈旧,不做一致性承诺** —— 返回的候选可能在返回那一刻刚被对方
//     拉黑、刚成为好友、或刚给你发了申请。这是可接受的:真正的守门在 AddFriend 的权威事务里
//     (它在容量守卫内重查拉黑 / 好友边 / 申请行),这里漏掉一个排除条件最坏只是"推荐了一个
//     加不上的人",不会写坏数据。
//     ⚠ 反过来的方向是禁止的:**本文件的查询绝不能被复用成 AddFriend 的前置判定** ——
//     它们不持任何锁,拿它当判定就是把权威事务降级成 check-then-act。
//
//  2. **每条候选查询都要有与玩家总数无关的扫描上界,而且上界必须由 SQL 结构给出,不能指望优化器"会提前停"**。
//     - random(recommendAnchor):只看 pivot 起按 player_id 升序的前 RecommendAnchorWindow(W=1024)个去重 id
//       (派生表里的 LIMIT 给出),每个候选的排除判定是 ≤5 次完整主键单行点查(FORCE INDEX (PRIMARY) +
//       SEMIJOIN(FIRSTMATCH) 钉死)。单次调用 Handler 读 ≤ 窗口生产 + (W+1) + 5×W,与 friend / friend_block /
//       friend_request 的行数、"拉黑我的人数"、全服 pending 数都无关;典型 ≈ 2.1×W。
//     - mutual(RecommendByMutual):外层行数由 friend 表 player_id 前缀给出(我的好友 → 好友的好友),上界 MaxFriends²;
//       ⚠ 但它的两条 OR 形 NOT EXISTS 仍可能被做成"每个 FOF 行扫一遍全服 pending",2026-09-28 在对抗数据上实测过
//       (见 RecommendByMutual 的注释),属已登记待修项,**目前不满足本条**。
//     "有界索引区间 + LIMIT"本身**不**构成上界:2026-09-28 在 MySQL 26.7.0、5 万玩家 / 100 万好友边的库上实测,
//     旧写法 `WHERE player_id >= ? ... GROUP BY player_id ORDER BY player_id LIMIT ?` 的 GROUP BY 去重落成临时表、
//     OR 形 NOT EXISTS 被做成 hash antijoin,LIMIT 无法提前终止 —— 扫描量 = pivot 之后的全部玩家数
//     (pivot 在区间开头:49,899 个分组、1113 ms;中段:20,000 个分组、636 ms),随玩家规模线性增长。
//     直接 `ORDER BY RAND()` 或无下界地扫 friend 表同理:好友边上百万行时就是一次全表排序。
//
//  3. **候选池只有"有过好友边的玩家"**。random 兜底是从 friend 表里挑锚点,所以一个好友数为 0 的
//     玩家永远不会被推荐出去,新服开服初期推荐会返回空。这不是缺陷而是库边界的必然结果:
//     mmorpg_friend 里没有玩家名册表,而 D-14 第 6 条禁止跨库去读别人的玩家表。
//     将来要覆盖零好友玩家,只能等有了权威名册(或由 player 域提供批量接口)再加一种策略。
//
//  4. 排除条件四类逐条对齐 A 仓:自己 / 已是我的好友 / 任一方向拉黑 / 任一方向仍 pending 的申请,
//     再加调用方传来的 exclude(客户端"换一批"时回传的已看过 id + 本次已选中的)。
//     少一类都会让客户端看到一个点下去必然失败的候选。

// RecommendCandidate 是一条推荐候选(SQL 侧结果行,由 logic 组装成 pb.RecommendEntry)。
//
// MutualFriends 是与查询者的共同好友数:mutual 策略的候选 > 0,random 兜底候选恒为 0。
// 0 的含义是"没有共同好友",不是"未知" —— 客户端按它排序,不能拿它判断数据是否缺失。
type RecommendCandidate struct {
	CandidatePlayerID uint64
	MutualFriends     uint32
}

// recommendExcludeClause 把 exclude 列表拼成 ` AND <col> NOT IN (?,?,...)` 与对应参数。
// exclude 为空时返回 ("", nil),调用方原样拼进 SQL 即可(拼一个空的 NOT IN () 是语法错误)。
//
// 名字带 recommend 前缀是刻意的:本包还有别的批次在并行加文件,通用名(excludeClause)
// 很容易和别人的同名 helper 撞成重复声明。
//
// 条数由 logic 层按 Friend.RecommendMaxExclude 封顶后才进来,所以这里不再自检长度 ——
// 占位符个数无上限会让 MySQL 的 prepared statement 参数数量成为客户端可控量。
func recommendExcludeClause(col string, exclude []uint64) (string, []any) {
	if len(exclude) == 0 {
		return "", nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(exclude)), ",")
	args := make([]any, 0, len(exclude))
	for _, id := range exclude {
		args = append(args, id)
	}
	return " AND " + col + " NOT IN (" + placeholders + ")", args
}

// RecommendByMutual 召回"好友的好友"(FOF):f1 是我的好友,f2.friend_player_id 是我好友的好友。
//
// 按共同好友数降序、同数随机(RAND()),取至多 limit 个。同数随机是为了让客户端"换一批"
// 在共同好友数扁平的图上也能换出人来,而不是每次都返回同一串 id。
//
// 开销的最坏情况要说清楚:JOIN 的结果集上界是 MaxFriends²(默认 200×200 = 4 万行),
// 分组后再按 RAND() 排序。这是一次排序不是一次全表扫,且被两个配置(MaxFriends、
// RecommendMaxLimit)封顶,所以可控;但把 MaxFriends 调大到几千时这条查询会先出问题,
// 改那个阈值的人必须知道这件事。
//
// ⚠ 已知问题(2026-09-28 实测,已登记待修,本次未改):上面的 MaxFriends² 只是外层 JOIN 的行数上界。两条 OR 形
// NOT EXISTS 没有可用的完整主键等值,优化器可能把 friend_request 那条做成"每个 FOF 行按 status=1 索引扫一遍全服
// pending"、把 friend_block 那条做成对"我拉黑的 + 拉黑我的"全集的 hash antijoin (no condition)。在 5 万玩家、
// 全服 pending 15,251 行、拉黑我的 20,100 人、600 个 FOF 行的对抗库上:9,215,314 次 Handler 读、约 4.6–5.1 s,
// 超过 RPC 超时。修法与 recommendAnchor 相同(按方向拆成五条完整主键 NOT EXISTS + FORCE INDEX (PRIMARY) +
// SEMIJOIN(FIRSTMATCH);同一对抗库上实测 5,408 次读),另开任务做。
func (r *FriendRepo) RecommendByMutual(ctx context.Context, playerID uint64, exclude []uint64, limit uint32) ([]RecommendCandidate, error) {
	excludeClause, excludeArgs := recommendExcludeClause("f2.friend_player_id", exclude)
	// friend_request 的 status=1 是 pending(取值见 proto/friend/friend_table.proto)。
	// 这里把 1 直接写进 SQL 文本,而 friend_repo.go 的查询(loadPendingRequestsFromMySQL 等)
	// 是把 requestStatusPending 当参数传 —— 两种写法指的是同一个值。本文件三处字面量
	// (这里一处,recommendAnchorBaseSQL 的 r_out / r_in 各一处)没有改成参数,是因为占位符里已经有一串同值的
	// playerID,再插一个不同含义的参数最容易数错位;改 pending 的编号时必须连这三处一起改。
	query := `SELECT f2.friend_player_id, COUNT(*) AS mutual
FROM friend f1
JOIN friend f2 ON f1.friend_player_id = f2.player_id
WHERE f1.player_id = ?
  AND f2.friend_player_id <> ?
  AND f2.friend_player_id NOT IN (SELECT friend_player_id FROM friend WHERE player_id = ?)
  AND NOT EXISTS (SELECT 1 FROM friend_block b
        WHERE (b.player_id = ? AND b.blocked_player_id = f2.friend_player_id)
           OR (b.player_id = f2.friend_player_id AND b.blocked_player_id = ?))
  AND NOT EXISTS (SELECT 1 FROM friend_request r
        WHERE r.status = 1
          AND ((r.from_player_id = ? AND r.to_player_id = f2.friend_player_id)
            OR (r.from_player_id = f2.friend_player_id AND r.to_player_id = ?)))` + excludeClause + `
GROUP BY f2.friend_player_id
ORDER BY mutual DESC, RAND()
LIMIT ?`
	args := []any{playerID, playerID, playerID, playerID, playerID, playerID, playerID}
	args = append(args, excludeArgs...)
	// LIMIT 显式转 int64:database/sql 的默认参数转换器对 uint32 是走 reflect 的,
	// 显式给它一个 driver 原生支持的类型,少一层依赖驱动实现细节的地方。
	args = append(args, int64(limit))
	return r.scanRecommendCandidates(ctx, "mutual", playerID, query, args)
}

// RecommendAnchorWindow 是 random 兜底一次最多检视的候选池玩家数:pivot 起按 player_id 升序的前 W 个去重 id。
// recommendAnchor 的扫描上界由它给出(见 recommendAnchor 注释第 1 条与文件头第 2 条)。
//
// 取值 1024 的依据(与 etc/friend.yaml 的默认阈值对账):窗口里可能被"有配置上限的排除"占掉的去重 id 至多
//
//	自己 1 + MaxFriends 200 + MaxBlocks 200 + MaxPendingRequests 50 + MaxIncomingRequests 200
//	+ RecommendMaxExclude 64 + mutual 已选中后追加进 exclude 的 RecommendMaxLimit-1 = 19,合计 734 个;
//
// 再装下 want ≤ RecommendMaxLimit = 20 个合格者,窗口至少 754,才能保证"各类排除都在上限内时,结果与不设窗口
// 逐条相同"(2026-09-28 实测:754 恰好凑满 20、753 只有 19)。取 1024,多出的 270 个位置留给唯一没有配置上限的
// 一类("拉黑了我的人":MaxBlocks 只限拉黑方自己的名单)和超出上限的历史行。
//
// 代价随 W 线性:典型 ≈ 2.1×W 次 Handler 读(约 3 ms),窗口里全是被排除者时 ≈ 7×W。
// ⚠ 调大上面任一阈值都要同步复核本值:internal/config 的 TestRecommendAnchorWindowCoversExclusionBudget
// 在 etc/friend.yaml 超预算时会红。超了不会推荐出错人,只会在排除者扎堆于 pivot 之后时推荐偏少。
// 导出只是为了让 config 包的测试能引用(config import data,data 不能反向 import config)。
const RecommendAnchorWindow uint32 = 1024

// RecommendRandom 是 mutual 召回不足时的兜底:在 friend 表的 player_id 区间里随机选一个锚点 pivot,
// 从"pivot 起按 player_id 升序的前 RecommendAnchorWindow 个去重 id"里按升序取前 limit 个合格候选
// (见 recommendAnchor)。
//
// **pivot 必须先取真实的 MIN/MAX 再在区间内随机** —— 这是 A 仓注释里写死的一条,原因:
// player_id 是雪花 ID(集中在当前时间窗对应的高位区间),直接取一个随机 uint64 绝大概率
// 落在现有最大 id 之后,`player_id >= pivot` 一行都扫不到,兜底静默返回空。
// MIN/MAX(player_id) 走 friend 表主键 (player_id, friend_player_id) 的最左列,MySQL 直接读
// 索引两端,与表行数无关;pivot ∈ [min,max] 则保证 `player_id >= pivot` 至少有行。
//
// 代价:返回可能少于 limit(可能为空),两种来源都可接受 —— 推荐是可降级的展示功能:
//   - pivot 靠近区间尾部时,pivot 之后本来就不足 limit 个合格者(正向扫,没有回绕);
//   - 窗口里被排除的人超过 W - limit 个:只可能来自没有配置上限的"拉黑了我的人"成片落在 pivot 之后,
//     或者好友 / 拉黑 / 申请 / exclude 的上限被调到超出窗口预算(见 RecommendAnchorWindow)。
// 为了凑满而越过窗口继续扫、或加一次回绕扫,都会把扫描上界重新撑开 —— 那正是 2026-09-28 修掉的缺陷。
// 返回的候选永远满足全部排除条件、不重复;偏少只影响数量。
func (r *FriendRepo) RecommendRandom(ctx context.Context, playerID uint64, exclude []uint64, limit uint32) ([]RecommendCandidate, error) {
	var minID, maxID sql.Null[uint64]
	if err := r.db.QueryRowContext(ctx,
		`SELECT MIN(player_id), MAX(player_id) FROM friend`).Scan(&minID, &maxID); err != nil {
		return nil, fmt.Errorf("recommend id range for player %d: %w", playerID, err)
	}
	// 空表时 MIN/MAX 返回 NULL(不是 0),此时没有任何兜底候选。
	// 这不是错误:新服 friend 表本来就是空的,返回 (nil, nil) 让上层拿到空候选集。
	//
	// 两端都判 Valid(A 仓只判了 MAX):MIN 为 NULL 而 MAX 不为 NULL 在 SQL 语义下不可能,
	// 但真发生时 lo 会静默取成 0,pivot 随之落到区间之外的低位 —— 那就是一次从表头开始的
	// 大范围扫描,正是本文件第 2 条要避免的事。多判一个字段换掉一种静默退化,值得。
	if !minID.Valid || !maxID.Valid || maxID.V == 0 {
		return nil, nil
	}
	lo, hi := minID.V, maxID.V
	pivot := lo
	if hi > lo {
		// span 至少为 2。溢出只会发生在 hi-lo == MaxUint64 的极端取值上(雪花 id 的差值
		// 远小于 2^63,现实中到不了),但 Uint64N(0) 是 panic,所以仍然显式兜一层:
		// 宽度算成 0 就退回 lo,推荐偏斜远好过进程崩。
		if span := hi - lo + 1; span > 0 {
			pivot = lo + rand.Uint64N(span)
		}
	}
	return r.recommendAnchor(ctx, playerID, exclude, pivot, limit)
}

// recommendAnchor 取 pivot 起按 player_id 升序的前 RecommendAnchorWindow(W)个去重 id 作为窗口,
// 在窗口里做四类排除 + 调用方 exclude,按 id 升序返回前 limit 个。mutual 恒填 0(随机候选没有算共同好友数,
// 为它再做一次 FOF 统计等于把兜底路径的开销抬到与主路径相同)。
//
// SQL(recommendAnchorBaseSQL)的每一处写法都是承重的,改之前先读完:
//
//  1. **派生表里的 LIMIT 就是上界本身**。带 LIMIT 的派生表不能合并进外层、外层条件也不会下推进去,优化器只能
//     先把它物化,候选生产在第 W 个分组处停下(跳跃扫描约 W+1 次定位;或范围扫描 + 流式分组,最多
//     W×MaxFriends 条索引项)。去掉这个 LIMIT 就退回旧缺陷:2026-09-28 实测退回扫 49,900 个分组、99,903 次读。
//     派生表上的 FORCE INDEX (PRIMARY) 排除"拿 friend_player_id 二级索引全扫再去重"这条路。
//  2. **DISTINCT 负责去重**(取代旧写法的 GROUP BY):friend 表一条边一行,一个有 N 个好友的玩家出现 N 次,
//     不去重会让同一候选重复 N 遍、还把窗口与 limit 名额吃光。
//  3. **外层列一律写派生表别名 `c.player_id`**:子查询里 friend / friend_block 也有 player_id 列,
//     裸 player_id 会先解析到子查询自己的表上,条件变成"自己拉黑自己",排除静默失效、一个错都不报。
//  4. **五条 NOT EXISTS 按方向拆开,每条都是完整主键等值**(f 我的好友 / b_out 我拉黑的 / b_in 拉黑我的 /
//     r_out 我发出的 pending / r_in 发给我的 pending)。旧写法 `(a=me AND b=c) OR (a=c AND b=me)` 没有可用的完整
//     主键,优化器只能去扫 per-me 甚至全服的集合。拆开是逻辑等价的:NOT EXISTS(A OR B) ≡ NOT EXISTS(A) AND
//     NOT EXISTS(B);"已是好友"从 NOT IN 改成 NOT EXISTS 也等价(两边的列都是 NOT NULL)。
//  5. **FORCE INDEX (PRIMARY) 与 SEMIJOIN(FIRSTMATCH) 缺一不可**:两者一起把每条排除钉成"每个候选一次主键单行
//     点查",并保留"先排序、凑够 limit 即停"的计划形状。2026-09-28 实测(TestRecommendAnchor_PlanIsPerRowPrimaryKeyLookups
//     同形数据):两个提示都不加时,r_out 被物化成 idx(status, updated_ms) 上 status=1 的全服 pending 扫描;
//     只加 FORCE INDEX 时,b_in 被物化成 friend_block 主键全索引扫描、r_in 被物化成 friend_request 全表扫描;
//     只加 FIRSTMATCH 时,b_in 改走 blocked_player_id 二级索引的 hash join、失去提前终止。
//     这些计划都与玩家总数或无上限的 per-me 集合成正比。
//     TiDB 不认 SEMIJOIN 提示(警告 8061 后忽略;v8.5.2 上实测计划仍有界),迁移时要按它自己的计划重新核对。
//
// 上界(与表规模无关):Handler 读 ≤ 窗口生产 + (W+1)(物化表扫描)+ 5×W(点查),外加 ≤ W 行的内存排序。
// 2026-09-28 实测(MySQL 26.7.0,服务端预处理语句):5 万玩家 / 100 万边的库上 pivot 在开头与中段都是 2,151 次
// (旧写法 149,725 / 75,055);对抗库(5 万玩家,窗口里塞满 734 个有上限的排除 + 100 个拉黑我的)4,301 次,
// 旧写法超过 120 s 被 MAX_EXECUTION_TIME 中断;窗口被 2 万个"拉黑我的"占满时 6,147 次、返回空
// (旧写法约 22.8 s 后返回窗口外的 10 个)。
//
// 窗口里合格者不足 limit 时返回偏少(可能为空),见 RecommendRandom 的"代价"。
func (r *FriendRepo) recommendAnchor(ctx context.Context, playerID uint64, exclude []uint64, pivot uint64, limit uint32) ([]RecommendCandidate, error) {
	query, args := recommendAnchorStatement(playerID, exclude, pivot, limit)
	return r.scanRecommendCandidates(ctx, "random", playerID, query, args)
}

// recommendAnchorBaseSQL 是 recommendAnchor 的主体;调用方 exclude 与 ORDER BY / LIMIT 由
// recommendAnchorStatement 拼在后面。每一处写法为什么不能动,见 recommendAnchor 的注释。
const recommendAnchorBaseSQL = `SELECT c.player_id, 0 AS mutual
FROM (SELECT DISTINCT player_id FROM friend FORCE INDEX (PRIMARY)
      WHERE player_id >= ?
      ORDER BY player_id
      LIMIT ?) AS c
WHERE c.player_id <> ?
  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend f FORCE INDEX (PRIMARY)
        WHERE f.player_id = ? AND f.friend_player_id = c.player_id)
  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_block b_out FORCE INDEX (PRIMARY)
        WHERE b_out.player_id = ? AND b_out.blocked_player_id = c.player_id)
  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_block b_in FORCE INDEX (PRIMARY)
        WHERE b_in.player_id = c.player_id AND b_in.blocked_player_id = ?)
  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_request r_out FORCE INDEX (PRIMARY)
        WHERE r_out.from_player_id = ? AND r_out.to_player_id = c.player_id AND r_out.status = 1)
  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_request r_in FORCE INDEX (PRIMARY)
        WHERE r_in.from_player_id = c.player_id AND r_in.to_player_id = ? AND r_in.status = 1)`

// recommendAnchorStatement 拼出 recommendAnchor 的 SQL 与参数。纯函数、不碰库:
// TestRecommendAnchor_PlanIsPerRowPrimaryKeyLookups 对它的产物做 EXPLAIN,保证计划守卫测的与生产跑的是同一份文本。
//
// 参数顺序与 ? 的出现顺序一一对应:
// pivot → 窗口 W → playerID ×6(<> 自排除、f、b_out、b_in、r_out、r_in)→ exclude... → limit。
func recommendAnchorStatement(playerID uint64, exclude []uint64, pivot uint64, limit uint32) (string, []any) {
	excludeClause, excludeArgs := recommendExcludeClause("c.player_id", exclude)
	query := recommendAnchorBaseSQL + excludeClause + "\nORDER BY c.player_id\nLIMIT ?"
	args := make([]any, 0, 8+len(excludeArgs)+1)
	// 两个 LIMIT 都显式转 int64,理由同 RecommendByMutual:给驱动一个原生支持的类型。
	args = append(args, pivot, int64(RecommendAnchorWindow),
		playerID, playerID, playerID, playerID, playerID, playerID)
	args = append(args, excludeArgs...)
	args = append(args, int64(limit))
	return query, args
}

// scanRecommendCandidates 执行候选查询并扫成 []RecommendCandidate。
//
// kind 只进错误信息,取值是有限的两个("mutual" / "random"),不进任何指标 label。
// 返回 nil 切片(而不是空切片)表示没有候选:调用方只做 len / range,两者等价,
// 不必为此多分配一次。
func (r *FriendRepo) scanRecommendCandidates(ctx context.Context, kind string, playerID uint64, query string, args []any) ([]RecommendCandidate, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("recommend %s for player %d: %w", kind, playerID, err)
	}
	defer rows.Close()

	var candidates []RecommendCandidate
	for rows.Next() {
		var c RecommendCandidate
		// MutualFriends 扫成 uint32 是安全的:它的上界是查询者的好友数,受 Friend.MaxFriends
		// 封顶(默认 200);random 路径更是常量 0。
		if err := rows.Scan(&c.CandidatePlayerID, &c.MutualFriends); err != nil {
			return nil, fmt.Errorf("scan recommend %s for player %d: %w", kind, playerID, err)
		}
		candidates = append(candidates, c)
	}
	// rows.Err() 必须查:驱动在迭代中途断连时 Next() 只是返回 false,
	// 不查就会把"连接断了"当成"没有候选",推荐静默变空。
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recommend %s for player %d: %w", kind, playerID, err)
	}
	return candidates, nil
}
