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
//  2. **绝不全表扫**。两条候选查询都必须落在有界的索引区间上:mutual 靠 friend 表的
//     player_id 前缀(我的好友 → 好友的好友),random 靠"先取真实 id 区间再在区间内选锚点"。
//     直接 `ORDER BY RAND()` 或无下界地扫 friend 表,在好友边上百万行时就是一次全表排序。
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
func (r *FriendRepo) RecommendByMutual(ctx context.Context, playerID uint64, exclude []uint64, limit uint32) ([]RecommendCandidate, error) {
	excludeClause, excludeArgs := recommendExcludeClause("f2.friend_player_id", exclude)
	// friend_request 的 status=1 是 pending(取值见 proto/friend/friend_table.proto)。
	// 这里沿用本包既有写法把 1 直接写进 SQL 而不是引一个包级常量:本包其它查询
	// (loadPendingRequestsFromMySQL / HasPendingRequest 等)也是字面量,加一个常量只会
	// 变成"一半用常量一半用字面量"。
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

// RecommendRandom 是 mutual 召回不足时的兜底:在 friend 表的 player_id 区间里随机选一个锚点,
// 沿索引正向扫到凑够 limit 个即止。
//
// **pivot 必须先取真实的 MIN/MAX 再在区间内随机** —— 这是 A 仓注释里写死的一条,原因:
// player_id 是雪花 ID(集中在当前时间窗对应的高位区间),直接取一个随机 uint64 绝大概率
// 落在现有最大 id 之后,`player_id >= pivot` 一行都扫不到,兜底静默返回空。
// MIN/MAX(player_id) 走 friend 表主键 (player_id, friend_player_id) 的最左列,MySQL 直接读
// 索引两端,与表行数无关;pivot ∈ [min,max] 则保证 `player_id >= pivot` 至少有行。
//
// 代价:靠近区间尾部的锚点可能凑不满 limit(正向扫没有回绕)。返回偏少是可接受的 ——
// 推荐是可降级的展示功能,为了凑满而加一次反向扫会把"绝不全表扫"的边界撑开。
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

// recommendAnchor 从 pivot 起沿 friend 表的 player_id 正向扫,命中 limit 个即止。
//
// 有界索引区间 + LIMIT,绝不全表扫。mutual 恒填 0(随机候选没有算共同好友数,
// 为它再做一次 FOF 统计等于把兜底路径的开销抬到与主路径相同)。
//
// GROUP BY player_id 不是为了聚合而是**去重**:friend 表一条边一行,一个有 N 个好友的玩家
// 在这张表里出现 N 次,不去重会让同一个候选在返回里重复 N 遍、还把 limit 名额吃光。
func (r *FriendRepo) recommendAnchor(ctx context.Context, playerID uint64, exclude []uint64, pivot uint64, limit uint32) ([]RecommendCandidate, error) {
	excludeClause, excludeArgs := recommendExcludeClause("player_id", exclude)
	// 两处 NOT EXISTS 里的外层列必须写全限定名 `friend.player_id`:子查询里已经有别名 b / r,
	// 裸 player_id 会先解析到子查询自己的表上(friend_block 也有 player_id 列),
	// 那样条件就变成"自己拉黑自己",排除条件静默失效、一个错都不报。
	query := `SELECT player_id, 0 AS mutual
FROM friend
WHERE player_id >= ?
  AND player_id <> ?
  AND player_id NOT IN (SELECT friend_player_id FROM friend WHERE player_id = ?)
  AND NOT EXISTS (SELECT 1 FROM friend_block b
        WHERE (b.player_id = ? AND b.blocked_player_id = friend.player_id)
           OR (b.player_id = friend.player_id AND b.blocked_player_id = ?))
  AND NOT EXISTS (SELECT 1 FROM friend_request r
        WHERE r.status = 1
          AND ((r.from_player_id = ? AND r.to_player_id = friend.player_id)
            OR (r.from_player_id = friend.player_id AND r.to_player_id = ?)))` + excludeClause + `
GROUP BY player_id
ORDER BY player_id
LIMIT ?`
	args := []any{pivot, playerID, playerID, playerID, playerID, playerID, playerID}
	args = append(args, excludeArgs...)
	args = append(args, int64(limit))
	return r.scanRecommendCandidates(ctx, "random", playerID, query, args)
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
