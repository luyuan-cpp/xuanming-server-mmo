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
//       SEMIJOIN(FIRSTMATCH) 钉死)。单次调用 Handler 读 ≤ 窗口生产 + (W+1) + 5×W + limit×(2F+1),与 friend /
//       friend_block / friend_request 的行数、"拉黑我的人数"、全服 pending 数都无关;典型 2.1–3.1×W 再加最后那一项。
//       最后一项是给**返回的**每个候选数一次共同好友(2026-10-09 起,见 recommendAnchor 第 6 条):F 是我的好友数
//       (≤ MaxFriends),默认上限下 ≤ 20×401 = 8,020 次,MaxFriends 取天花板 300 时 ≤ 12,020 次。窗口生产随计划不同:
//       跳跃扫描约 W+1 次定位,范围扫描要读完窗口里 W 个人的全部出边(最坏 ≈ W×MaxFriends ≈ 20.5 万次,
//       见 RecommendAnchorWindow 的"代价")。
//       ⚠ SQL 结构(派生表的 LIMIT)封住的只是"pivot 之后最多读 W 个分组";"不从索引开头扫、只读 pivot 之后"
//       靠的是优化器对 `player_id >= ?` 选 range 访问(EXPLAIN type=range,跳跃扫描也显示为 range)。
//       统计声称 friend 表只有 ≤1 行时这一点不成立:2026-09-28 实测退回 type=index、从索引开头全扫,读数随
//       pivot 之前的行数增长(84 万边 / 30 万人的库上 643,575 次;16×W 单边夹具、pivot 居中时 10,343 次);
//       统计声称 ≥2 行时即回到 range。复现到这个状态的做法都是:该表关掉 STATS_AUTO_RECALC 后批量灌数,
//       再 FLUSH TABLE 让 ≤1 行的旧持久统计被重新读回。
//       **这是登记在案的剩余风险,没有测试或运行期兜底**:TestRecommendAnchor_ReadsBoundedByWindowNotPoolSize 的
//       读数断言和 TestRecommendAnchor_PlanIsPerRowPrimaryKeyLookups 的 type=range 断言都在夹具 ANALYZE 之后才测,
//       守的只是"统计新鲜时,SQL 或提示的改动让窗口退化成 type=index"(例如给派生表加 GROUP_INDEX 提示,见下);
//       统计退化本身在用例里复现不了,生产上只靠 InnoDB auto_recalc(默认开启,约 10% 的行变化后后台重算)。
//       不要拿 GROUP_INDEX(friend PRIMARY) 之类的提示去"钉住"窗口:2026-09-28 实测它在统计新鲜时反而把窗口变成
//       从索引开头的 type=index 全扫(5 万玩家 / 100 万边的库 pivot=1025000 读 521,607 次,不加提示 2,151 次)。
//     - mutual(RecommendByMutual):分两段。内层派生表只碰 friend 表:f1(我的好友,F ≤ MaxFriends 行)STRAIGHT_JOIN f2
//       (好友的好友,FOF 行数 R ≤ MaxFriends²)→ 按候选分组数出共同好友数 → 按"共同好友数降序、同数随机"取前
//       RecommendAnchorWindow(W=1024)名(派生表里的 LIMIT 给出)。外层只对这 W 名做排除判定,每人 ≤5 次完整主键
//       单行点查(与 recommendAnchor 同一套 FORCE INDEX (PRIMARY) + SEMIJOIN(FIRSTMATCH))。单次调用 Handler 读
//       ≤ 3R + 6W + 3(GROUP BY 临时表留在内存时成立;默认上限下 ≤ 126,147),与全服 pending 数、"拉黑我的人数"都无关;
//       其中会落到**候选自己**叶子页上的点查(b_in / r_in)≤ 2W 次,与 R 无关 —— 这一条封住的是排除表大于
//       buffer pool 时的耗时。3R 那部分只读 friend 表、按 MaxFriends 的平方增长,所以 config.Validate 把 MaxFriends
//       硬封顶在 300(config 包 maxFriendsCeiling)。
//       沿革:2026-09-28 评审实测的最初写法(两条 OR 形 NOT EXISTS,被做成"每个 FOF 行扫一遍全服 pending")于 09-29 改成
//       "每个 FOF 行五次主键点查"。那一版读数有界,但点查次数是 5R,其中 2R 次落在各候选自己的页上:排除表大于 buffer pool
//       时单次调用 38–51 s(2026-10-08 实测)。同日改成现在的"先排名截窗口、再只对窗口内候选点查"。
//       推导、前提与新旧实测数字见 RecommendByMutual 的注释。
//       与 recommendAnchor 不同,这条在传统优化器下的"统计退化"风险已由 SQL 结构(STRAIGHT_JOIN + 派生表 LIMIT +
//       五条排除上的 SEMIJOIN(FIRSTMATCH),三者缺一不可,见 RecommendByMutual 注释第 1 / 2 / 5 条)消掉。
//     "有界索引区间 + LIMIT"本身**不**构成上界:2026-09-28 在 MySQL 26.7.0、5 万玩家 / 100 万好友边的库上实测,
//     旧写法 `WHERE player_id >= ? ... GROUP BY player_id ORDER BY player_id LIMIT ?` 的 GROUP BY 去重落成临时表、
//     OR 形 NOT EXISTS 被做成 hash antijoin,LIMIT 无法提前终止 —— 扫描量 = pivot 之后的全部玩家数
//     (pivot=1000100 在区间开头:49,899 个分组、1113 ms;pivot=1030000 在中段:20,000 个分组、636 ms),
//     随玩家规模线性增长。
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
// MutualFriends 是与查询者的共同好友数:既在我的好友列表里、又在候选的好友列表里的人有几个。好友边双向成对
// (AcceptFriend / RemoveFriend 的不变量)时两条路径数出的是同一个数 —— 同一个人不管由哪条路径返回,数字都一样,
// 0 就是"没有共同好友"。(两条 SQL 读的边不同:mutual 读 (我, x) 与 (x, 候选),兜底读 (我, x) 与 (候选, x),
// 原因见 recommendAnchor 第 6 条;存在单向的历史行时两者可能差出那几条边。)
//   - mutual 策略的候选恒 > 0(他们本来就是按这个数排名选出来的);
//   - random 兜底的候选通常是 0(有共同好友的人大多已被 mutual 召回),但不恒为 0:mutual 的排名窗口被排除者占满时
//     (见 RecommendByMutual),兜底会挑到窗口之外、实际有共同好友的人,这时填的是他的真实数字。
//
// 2026-10-09 之前兜底路径恒填 0(没有去数),上面第二种人会被客户端显示成"没有共同好友";现在兜底那条查询给每个
// **返回的**候选数一次(recommendAnchor 第 6 条),不多打一次库。
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

// RecommendByMutual 召回"好友的好友"(FOF):f1 是我的好友,f2.friend_player_id 是我好友的好友,
// mutual 是共同好友数(同一个候选经几个 f1 可达)。
//
// 按共同好友数降序、同数随机,取至多 limit 个。同数随机是为了让客户端"换一批"
// 在共同好友数扁平的图上也能换出人来,而不是每次都返回同一串 id。
//
// **只在排名前 RecommendAnchorWindow(W=1024)名里挑。** 窗口里被排除的人超过 W − limit 个时返回偏少(可能为空),
// logic/recommend.go 会用 RecommendRandom 兜底补足。有配置上限的排除类(已是我的好友 / 我拉黑的 / 两个方向的 pending /
// 调用方 exclude)合计不超过窗口预算(推导见 RecommendAnchorWindow,config 包有对账测试),所以偏少只可能来自没有配置
// 上限的"拉黑了我的人"扎堆在排名头部、超出上限的历史行,或上限被调到超出预算。返回的候选永远满足全部排除条件、不重复;
// 偏少只影响数量。此时兜底补进来的人可能是窗口之外的 FOF;兜底那条查询会给他们数出真实的共同好友数
// (见 RecommendCandidate 与 recommendAnchor 第 6 条),所以客户端看到的数字仍然是对的。
// 2026-10-08 逐库比对(按 (id, mutual)):候选不多于 W 时,结果与"不设窗口、对每个 FOF 行做排除"的
// 09-29 版逐行相同(friend_explain_scratch 五个 me、测试夹具、对抗库);候选多于 W 时(3.7 万个合格候选的最坏库、
// 3,601 个候选的聚簇库),新结果是旧结果的子集、共同好友数逐人相同,截断档之上各档的候选集合逐档相同。
//
// SQL(recommendByMutualBaseSQL)的每一处写法都是承重的,改之前先读完:
//
//  1. **内层派生表只碰 friend 表,行数由主键前缀给出,STRAIGHT_JOIN 钉住"先 f1 后 f2"**。f1 是 `player_id = me` 的前缀 ref
//     (F ≤ MaxFriends 行),f2 按 f1.friend_player_id 做前缀 ref(每个 f1 ≤ MaxFriends 行),FOF 行数 R ≤ MaxFriends²
//     (默认 200×200 = 4 万;AcceptFriend 对双方都查 MaxFriends,单人出边数由它封顶,超出上限的历史行按实际行数算)。
//     `f2.friend_player_id <> ?` 在内层就滤掉"候选是我自己"的那 F 行,不占窗口。
//     STRAIGHT_JOIN 是承重的:持久统计陈旧时,优化器会把连接顺序翻成"f2 全索引扫描驱动 + f1 主键点查",读数随 friend
//     表总行数增长。被估错的是 **f2 那一侧,不是 f1** —— f1 是 `player_id = 常量` 的前缀 ref,走 index dive,估得准;
//     持久统计的 n_diff 还是 0(建表后关掉 STATS_AUTO_RECALC 再灌数、不 ANALYZE)时,按 f1.friend_player_id 做的 f2 前缀 ref
//     每次都被估成整表行数。2026-10-08 在 TestRecommendByMutual_JoinOrderSurvivesStaleStatistics 的夹具上复核:去掉
//     STRAIGHT_JOIN 后派生表改由 f2 驱动(type=index,把 friend 整表 1,318 行读一遍;limit = W 时 4,375 次读,保留时
//     3,445 次)。夹具的 friend 表很小所以差距不大;09-29 在 116 万边的库上测过同一段连接,不加 STRAIGHT_JOIN 是
//     1,529,004 次读。
//  2. **派生表里的 LIMIT 就是"按候选点查"次数的上界本身**。带 LIMIT 的派生表不能合并进外层、外层条件也不会下推进去,
//     优化器只能先把它物化成 ≤ W 行;外层的五条排除只对这 ≤ W 行做。去掉这个 LIMIT 就退回 09-29 版的代价:在排除表
//     远大于 buffer pool 的库上实测(窗口饱和场景:39,800 个候选全部拉黑了 me;命中的那条排除排在外层第 k 层时每人
//     做 k 次点查),去掉后 159,803 + k × 39,800 次读、约 4 万次页读、18–21 s(2026-10-01 测到 k=4 的 319,003,
//     10-08 测到 k=3 的 279,203);保留时 121,027 + k × W 次读、约 1,000 次页读、0.5–0.7 s。
//  3. **打散键 `RAND() AS shuffle` 在内层算一次并随窗口一起物化,外层按 `c.mutual DESC, c.shuffle` 排序**。内外用的是
//     同一个值,所以"先截窗口、再排序"不会乱序;同数的候选每次调用进窗口的人与先后顺序都不同(2026-10-08 实测:40 人同分池
//     连调 8 次是 8 种序列;3.98 万个同分候选的库连调 5 次,返回的 5×20 人两两不重叠)。
//     外层**不能**写成 `ORDER BY c.mutual DESC, RAND()`:排序键不再是物化列,优化器就做不出"先排序、凑够 limit 即停",
//     而是对窗口里全部 W 人做完点查再排序(2026-10-01 同一个大库:126,147 次读、约 2,000 次页读、约 1.1 s,
//     对照 121,127 次、约 50 次页读、约 0.1 s)。结果仍然正确,只是每次都付最坏代价;
//     TestRecommendByMutual_ExclusionLookupsBoundedByWindow 的第二条断言(3R + W + 3 + 5×limit)守这一点。
//  4. **五条 NOT EXISTS 按方向拆开,每条都是完整主键等值**(f 我的好友 / b_out 我拉黑的 / b_in 拉黑我的 /
//     r_out 我发出的 pending / r_in 发给我的 pending),与 recommendAnchor 第 4 条同理:`(a=me AND b=x) OR (a=x AND b=me)`
//     没有可用的完整主键,优化器只能去扫 per-me 甚至全服的集合(2026-09-28 的最初缺陷)。拆开逻辑等价:
//     NOT EXISTS(A OR B) ≡ NOT EXISTS(A) AND NOT EXISTS(B);"已是好友"从 NOT IN 改成 NOT EXISTS 也等价(列都是 NOT NULL)。
//  5. **SEMIJOIN(FIRSTMATCH) 是承重的,FORCE INDEX (PRIMARY) 是防线**,两者一起把每条排除钉成"每个窗口内候选一次主键
//     单行点查"。2026-10-08 在 TestRecommendByMutual_ReadsBoundedByFOFRowsNotGlobalSets 的夹具上逐项拆除(EXPLAIN FORMAT=TRADITIONAL):
//     (a) 只去掉 SEMIJOIN 提示:统计新鲜时、以及 n_diff = 0 的陈旧统计下(第 1 条那种状态,也是 M2b 用例造出的状态),
//     f / b_out / r_out 被物化(都是 per-me 的小集合;limit = 20 时读数 2,094,没越界)。真正的退化出现在另一种陈旧
//     状态:持久统计里 n_rows = 0 被重新读回内存(对这几张表 FLUSH TABLES,或 mysqld 重启后;EXPLAIN 里 f2 rows = 1)——
//     这时 b_in 被物化成 friend_block 主键全索引扫描、r_in 被物化成 friend_request 全表扫描,同一份数据 57,076 次读;
//     (b) 两个提示都去掉:在"n_rows = 0 被重读"的状态下 b_in 走 idx_blocked_player 读完 2 万个拉黑我的人、r_out 走
//     idx_status_updated 读完全服 pending,37,071 次读(n_diff = 0 的状态下只比 (a) 多物化一个 r_in,约 2,097 次);
//     (c) 只去掉 FORCE INDEX(子查询上的,或 f1 / f2 上的):各种统计状态下计划都不变,只是 possible_keys 多出二级索引 ——
//     没有实测到退化,留作防线,计划守卫用例用 possible_keys == PRIMARY 钉住。
//     "n_rows = 0 被重读"的状态要 FLUSH TABLES 才造得出来,用例进不去;去掉提示在任何状态下都会出现 <subqueryN>,
//     M2 / M2b 的计划断言因此能拦住"提示被删",但拦不住的是那种状态本身 —— 提示在,它就不成问题(带着两个提示实测:
//     limit = 20 时 2,057 次,limit = W 时 3,443 次;上面 (a)(b) 的 57,076 / 37,071 是 limit = 20 的读数)。
//  6. **外层列一律写派生表别名 `c.candidate_id`**:子查询里 friend / friend_block 也有 player_id / friend_player_id 列,
//     裸列名会先解析到子查询自己的表上,排除静默失效(与 recommendAnchor 第 3 条同一个坑)。
//
// 上界(与全服 pending 数、"拉黑我的人数"、friend 表总行数都无关):设 F = 我的好友数、R = FOF 行数(含"候选是我自己"的
// F 行)、D = 去重后的候选数(≤ R − F),
//
//	Handler 读 ≤ (1+F) f1 + (F+R) f2 + (R−F) 分组临时表定位 + (D+1) 分组临时表扫描 + (W+1) 物化窗口扫描 + 5W 排除点查
//	          ≤ 3R + 6W + 3;默认上限下(R ≤ 40,000)≤ 126,147,MaxFriends 取天花板 300 时 ≤ 276,147。
//
// 外层先把 ≤ W 行的窗口按 (mutual, shuffle) 在内存里排好,再逐人做嵌套循环反连接、凑够 limit 即停:limit = 20 且排名前 20
// 都合格时只做 100 次点查;5W 是窗口里每个人都把五次点查做满时的值(窗口全被排在最后一层的那条排除掉)。
// 外层的 filesort、exclude 的 NOT IN 与 limit 都不产生 Handler 读。式子里的 (R−F) 假定好友边双向成对(每个好友都有
// 一条回到我的边,AcceptFriend / RemoveFriend 的不变量);存在单向的历史行时"候选是我自己"不足 F 行,
// 上界放宽为 3R + 2F + 6W + 3(2026-10-08 评审用 40 条单向边实测,正好多 2F)。
//
// **读数有界不等于耗时有界** —— 耗时 ≈ 读数 × CPU + 随机页读 × IO。五条点查里 f / b_out / r_out 落在 me 自己的主键
// 前缀上(几页,热的);b_in / r_in 落在**各候选自己**的叶子页上,这才是冷缓存下贵的部分。现在它们 ≤ 2W 次,
// 内层读 friend 表是 F+1 段主键前缀范围读,所以单次调用的随机页读约为 F 段范围读 + ≤ 2W,与 R 无关。
// 2026-10-08 实测(MySQL 26.7.0,innodb_buffer_pool_size = 128 MB,服务端预处理语句;读数是会话 Handler_read_* 增量,
// 页读是 Innodb_buffer_pool_reads 增量。库:friend 约 291 万边 / friend_block 约 228 万行、780 MB / friend_request 约 271 万行、
// 1.9 GB,后两张远大于 buffer pool;me 有 200 个好友、39,800 个互不相同的候选,R = 40,000;30 万人拉黑 me;
// exclude 65 个、limit 20):
//
//	09-29 版(每个 FOF 行五次点查):318,957 次读,每次调用约 7.6 万次页读,38–51 s —— 超过 RPC 超时十倍
//	现在:limit=20                 121,127 次读,约 40–140 次页读,0.09–0.12 s(挤出 buffer pool 后与热态相近)
//	     窗口全被排除(39,800 个候选都拉黑了 me):121,027 + k × W 次读(k 是"拉黑了我"那条排除在外层的第几层;
//	                                   实测过 k=3 的 124,099 与 k=4 的 125,123),约 950–1,290 次页读,0.47–0.73 s,返回空
//	     limit=1024(窗口里每人做满点查):126,147 次读(= 上界),约 1,950–2,400 次页读,0.96–1.09 s
//
// 耗时与页读是本机数字,随同机负载与缓存状态浮动;读数只随五条排除在外层的先后变(优化器按代价排,见下)。
// 其它库上"09-29 版 → 现在"的读数(limit=20,热):测试夹具(F=30、R=660)4,737 → 2,057;friend_explain_scratch
// (5 万玩家 / 100 万边,F=20、R=400)1,928 → 1,063;另一个约 4 万 FOF 行的最坏库(排除表小、全在内存)约 31 万次(两次实测 309,687 / 311,907)→ 121,357 次;
// 聚簇库(F=200、R=21,400、3,601 个候选)140,408 → 47,731。读数会随"窗口里排名靠前的人有几个被排除"小幅浮动
// (5 万玩家对抗库同一个 me 连测 1,921 / 1,949),因为同数候选每次进窗口的人不同。
//
// 前提(都不满足时读数会越过上面的式子,但仍与全服集合无关,除非另有说明):
//   - **GROUP BY 的临时表留在内存**(TempTable 引擎,单表上限 tmp_table_size,MySQL 默认 16 MiB)。式子里"分组临时表
//     定位 + 扫描"两项依赖它。2026-10-08 实测(热缓存,耗时随同机负载浮动,两轮实测的区间一并列出):F=400
//     (16 万个分组)仍在内存,481,127 次读、0.17–0.56 s;F=470(22 万个)转存磁盘(Created_tmp_disk_tables = 1),
//     860,390 次读、0.67–0.77 s,比式子多约 19.7 万次(落盘那一刻内存里已有的分组各回读一次,与 R 无关);
//     F=600 是 1,277,690 次、1.05–1.18 s。
//   - **内层的 3R 按 MaxFriends 的平方增长**,RecommendMaxLimit 不进这个式子。所以 config.Validate 把 MaxFriends 硬封顶
//     在 config 包的 maxFriendsCeiling = 300(F=300 实测 271,127 次读、0.10–0.21 s);调大之前必须在目标库上重测,并确认
//     候选分组数仍装得进内存临时表。
//   - **传统优化器**。STRAIGHT_JOIN 与 SEMIJOIN(FIRSTMATCH) 两个提示在 hypergraph_optimizer=on 时都不生效(MySQL 26.7.0
//     默认 off)。2026-10-08 实测:打开后统计新鲜时仍然有界(测试夹具上是 per-me 的三个集合做 hash antijoin、
//     b_in / r_in 逐候选点查,1,101 次读;上面那个大库上是五层逐候选主键点查,约 4.2 万次读);n_diff = 0 的陈旧统计下
//     也还有界(测试夹具约 2,690 次);但在"n_rows = 0 被重新读回"的陈旧状态下(见第 5 条)退化成 friend /
//     friend_block / friend_request 三表全扫(测试夹具 56,703 次读,随三张表的总行数增长)。部署不要打开 hypergraph 优化器。
//   - TiDB 不认 SEMIJOIN 提示(警告 8061 后忽略),认 STRAIGHT_JOIN。2026-10-01 在 v8.5.2 上核对过结果语义
//     (窗口饱和时返回空、打散键内外一致、同数换序);计划与读数要在迁移时按它自己的重新核对。
//     2026-10-09 核对了计划:统计新鲜时内层只读 friend 的 F+R 行,五条排除只对窗口内 ≤ W 人做(排除表很小时会直接
//     扫整张小表);但 STRAIGHT_JOIN 在 TiDB 上只定连接顺序、不定连接算法 —— 统计低估表规模时 b_in / r_in / f2 会变成
//     对大表的整表扫描。迁移到 TiDB 之前要先加
//     它自己的提示,做法与实测数字登记在 docs/handoff/friend-handoff-20260920.md §9.5 第 12 条。
//
// 沿革:2026-09-28 评审在对抗库上实测最初的写法(两条 OR 形 NOT EXISTS + 好友 NOT IN):friend_request 那条被做成
// "每个 FOF 行按 status=1 扫一遍全服 pending",9,215,314 次读、4.6–5.1 s,超过 RPC 超时(测试夹具上约 930 万次)。
// 09-29 改成"每个 FOF 行五次主键点查",读数 ≤ 8R + 2F + 2 + limit,不再随全服集合增长,但点查次数随 R 增长、
// 冷缓存下守不住预算(见上表第一行)。2026-10-08 改成现在的写法。
func (r *FriendRepo) RecommendByMutual(ctx context.Context, playerID uint64, exclude []uint64, limit uint32) ([]RecommendCandidate, error) {
	query, args := recommendByMutualStatement(playerID, exclude, limit)
	return r.scanRecommendCandidates(ctx, "mutual", playerID, query, args)
}

// recommendByMutualBaseSQL 是 RecommendByMutual 的主体(内层派生表:连接、分组、排名、截窗口;外层:五条排除);
// 调用方 exclude 与外层的 ORDER BY / LIMIT 由 recommendByMutualStatement 拼在后面。每一处写法为什么不能动,
// 见 RecommendByMutual 的注释。
//
// friend_request 的 status=1 是 pending(取值见 proto/friend/friend_table.proto)。这里把 1 直接写进 SQL 文本,
// 而 friend_repo.go 的查询(loadPendingRequestsFromMySQL 等)是把 requestStatusPending 当参数传 —— 两种写法指的是
// 同一个值。本文件四处字面量(本常量与 recommendAnchorBaseSQL 的 r_out / r_in 各一处)没有改成参数,是因为占位符里
// 已经有一串同值的 playerID,再插一个不同含义的参数最容易数错位;改 pending 的编号时必须连这四处一起改。
const recommendByMutualBaseSQL = `SELECT c.candidate_id, c.mutual
FROM (SELECT f2.friend_player_id AS candidate_id, COUNT(*) AS mutual, RAND() AS shuffle
      FROM friend f1 FORCE INDEX (PRIMARY)
      STRAIGHT_JOIN friend f2 FORCE INDEX (PRIMARY) ON f2.player_id = f1.friend_player_id
      WHERE f1.player_id = ?
        AND f2.friend_player_id <> ?
      GROUP BY f2.friend_player_id
      ORDER BY mutual DESC, shuffle
      LIMIT ?) AS c
WHERE NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend f FORCE INDEX (PRIMARY)
        WHERE f.player_id = ? AND f.friend_player_id = c.candidate_id)
  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_block b_out FORCE INDEX (PRIMARY)
        WHERE b_out.player_id = ? AND b_out.blocked_player_id = c.candidate_id)
  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_block b_in FORCE INDEX (PRIMARY)
        WHERE b_in.player_id = c.candidate_id AND b_in.blocked_player_id = ?)
  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_request r_out FORCE INDEX (PRIMARY)
        WHERE r_out.from_player_id = ? AND r_out.to_player_id = c.candidate_id AND r_out.status = 1)
  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_request r_in FORCE INDEX (PRIMARY)
        WHERE r_in.from_player_id = c.candidate_id AND r_in.to_player_id = ? AND r_in.status = 1)`

// recommendByMutualStatement 拼出 RecommendByMutual 的 SQL 与参数。纯函数、不碰库:
// TestRecommendByMutual_PlanIsPerRowPrimaryKeyLookups 对它的产物做 EXPLAIN,保证计划守卫测的与生产跑的是同一份文本。
//
// 参数顺序与 ? 的出现顺序一一对应:
// playerID ×2(f1 前缀、<> 自排除)→ 窗口 W → playerID ×5(f、b_out、b_in、r_out、r_in)→ exclude... → limit。
func recommendByMutualStatement(playerID uint64, exclude []uint64, limit uint32) (string, []any) {
	excludeClause, excludeArgs := recommendExcludeClause("c.candidate_id", exclude)
	query := recommendByMutualBaseSQL + excludeClause + "\nORDER BY c.mutual DESC, c.shuffle\nLIMIT ?"
	args := make([]any, 0, 8+len(excludeArgs)+1)
	// 两个 LIMIT 都显式转 int64:database/sql 的默认参数转换器对 uint32 是走 reflect 的,
	// 显式给它一个 driver 原生支持的类型,少一层依赖驱动实现细节的地方。
	args = append(args, playerID, playerID, int64(RecommendAnchorWindow),
		playerID, playerID, playerID, playerID, playerID)
	args = append(args, excludeArgs...)
	args = append(args, int64(limit))
	return query, args
}

// RecommendAnchorWindow 是 random 兜底一次最多检视的候选池玩家数:pivot 起按 player_id 升序的前 W 个去重 id。
// recommendAnchor 的扫描上界由它给出(见 recommendAnchor 注释第 1 条与文件头第 2 条)。
//
// 2026-10-08 起 RecommendByMutual 共用同一个 W:它只对"共同好友数排名前 W"的候选做排除判定(见该函数注释第 2 条)。
// 两条查询要排除的是同一批人,所以下面这份预算对它同样成立,而且更宽裕 —— mutual 在内层就滤掉了自己(不占窗口),
// 也没有"mutual 已选中后追加进 exclude"那 19 个。名字里的 Anchor 是历史沿革,不单指 random。
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
// 754 是**保守上界**:它把"mutual 已选中后追加进 exclude 的 19 个"和"want = 20"各按最大值算了一次,而这两者走不到
// 一起 —— logic/recommend.go 的 RecommendFriends 里 mutual 每选中一人就追加一个 exclude,random 的 want 是
// limit 减去已选中数,所以"mutual 已追加数 + want"恒等于 limit ≤ RecommendMaxLimit。可达组合的真实最大需求是
// 1 + 200 + 200 + 50 + 200 + 64 + 20 = 735。按 754 算只会让 config 包那条对账测试比实际早 19 个名额变红,不会漏判。
// MaxFriends 调到 config 包允许的天花板 300 时,同一算法是 854,仍在 1024 之内。
//
// 代价随 W 线性,但系数取决于派生表生产窗口时选了哪种计划 —— 由采样统计决定,同一份数据上两种都会出现
// (下面的读数都不含 2026-10-09 加的共同好友子查询:它与 W 无关,每返回一行另加 2F+1 次,见 recommendAnchor 第 6 条):
//   - 跳跃扫描(EXPLAIN ANALYZE 显示 Covering index skip scan for deduplication):窗口生产 W+2 次读
//     (W+1 次定位 + 1 次 read_last),单次调用典型 ≈ 2.1×W 次 Handler 读(约 3 ms)。
//     窗口里全是被排除者时 ≤ 7×W+3 = 窗口生产 W+2 + 物化表扫描 W+1 + 点查 5×W。嵌套循环反连接在候选命中第一条
//     排除时就丢弃它、后面几层点查不再做,所以命中的那条排在第 k 层时约 (2+k)×W;连接顺序由优化器按统计排,
//     2026-09-28 实测:4,099(k=2)/ 5,123(k=3)/ 6,147(k=4)/ 7,171(k=5,即 7×W+3)。
//   - 范围扫描 + 流式分组(Covering index range scan + Group (no aggregates)):要读完窗口里 W 个人的全部出边,
//     每人 1 条边时窗口生产 W+1(1 次定位 + W 次 next),全被排除时约 (2+k)×W+2
//     (TestRecommendAnchor_StopsAtWindowWhenSaturated 的夹具:b_in 在第 5 层,实测 7,170 = 7×W+2);
//     每人 2 条边时典型约 3.1×W;最坏是窗口里全是满好友的人,≈ W×MaxFriends ≈ 20.5 万次索引读
//     (AcceptFriend 对双方都查 MaxFriends,单人出边数由它封顶;超出上限的历史行按实际行数算)。
//
// 2026-09-28 实测(MySQL 26.7.0;50 万人每人 2 条边,另有 pivot 起 2000 人每人 202 条边):同一条查询,
// ANALYZE 采样出的 n_diff_pfx01 ≈ 51–53 万时走跳跃扫描、2,151 次;≈ 56 万时走范围扫描、207,974 次,
// EXPLAIN ANALYZE 约 81 ms。最坏情况仍与玩家总数无关、远在 RPC 预算之内,但"典型 2.1×W"不是上界。
//
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
//   - 窗口里被排除的人超过 W - limit 个:只可能来自没有配置上限的"拉黑了我的人"成片落在 pivot 之后、
//     超出上限的历史行,或者好友 / 拉黑 / 申请 / exclude 的上限被调到超出窗口预算(见 RecommendAnchorWindow)。
//
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
// 在窗口里做四类排除 + 调用方 exclude,按 id 升序返回前 limit 个,并给每个**返回的**候选数出与我的共同好友数
// (选择列表里的标量子查询,见第 6 条;不是对整个窗口再做一遍 FOF 统计)。
//
// SQL(recommendAnchorBaseSQL)的每一处写法都是承重的,改之前先读完:
//
//  1. **派生表里的 LIMIT 就是上界本身**。带 LIMIT 的派生表不能合并进外层、外层条件也不会下推进去,优化器只能
//     先把它物化,候选生产在第 W 个分组处停下(跳跃扫描约 W+1 次定位;或范围扫描 + 流式分组,最多
//     W×MaxFriends 条索引项 —— 两种计划随采样统计翻转,实测数字见 RecommendAnchorWindow 的"代价")。
//     去掉这个 LIMIT 就退回旧缺陷:2026-09-28 实测退回扫 49,900 个分组、99,903 次读。
//     派生表上的 FORCE INDEX (PRIMARY) 排除"拿 friend_player_id 二级索引全扫再去重"这条路,但**不**排除
//     对 PRIMARY 做 type=index 的全索引扫描:LIMIT 只封住 pivot 之后的分组数,"不从索引开头扫"靠的是优化器
//     对 `player_id >= ?` 选 range 访问 —— 统计声称表只有 ≤1 行时这一点不成立(2026-09-28 实测,见文件头第 2 条的 ⚠)。
//     计划守卫用例的 type=range 断言与读数上界用例都先 ANALYZE 再测,只守"统计新鲜时 SQL / 提示的改动让窗口失去
//     range 访问";统计退化本身用例进不去,生产上只靠 InnoDB auto_recalc,是登记在案的剩余风险。
//  2. **DISTINCT 负责去重**(取代旧写法的 GROUP BY):friend 表一条边一行,一个有 N 个好友的玩家出现 N 次,
//     不去重会让同一候选重复 N 遍、还把窗口与 limit 名额吃光。
//  3. **外层列一律写派生表别名 `c.player_id`**:子查询里 friend / friend_block 也有 player_id 列,
//     裸 player_id 会先解析到子查询自己的表上,条件变成"自己拉黑自己",排除静默失效、一个错都不报。
//  4. **五条 NOT EXISTS 按方向拆开,每条都是完整主键等值**(f 我的好友 / b_out 我拉黑的 / b_in 拉黑我的 /
//     r_out 我发出的 pending / r_in 发给我的 pending)。旧写法 `(a=me AND b=c) OR (a=c AND b=me)` 没有可用的完整
//     主键,优化器只能去扫 per-me 甚至全服的集合。拆开是逻辑等价的:NOT EXISTS(A OR B) ≡ NOT EXISTS(A) AND
//     NOT EXISTS(B);"已是好友"从 NOT IN 改成 NOT EXISTS 也等价(两边的列都是 NOT NULL)。
//  5. **SEMIJOIN(FIRSTMATCH) 是承重的,FORCE INDEX (PRIMARY) 是防线,两个都要留**:两者一起把每条排除钉成
//     "每个候选一次主键单行点查",并保留"先排序、凑够 limit 即停"的计划形状。2026-09-28 实测
//     (TestRecommendAnchor_PlanIsPerRowPrimaryKeyLookups 同形数据,EXPLAIN FORMAT=TRADITIONAL):
//     (a) 两个提示都不加:五条排除全被物化,其中 r_out 物化成 idx(status, updated_ms) 上 status=1 的全服
//     pending 扫描;(b) 只加 FORCE INDEX(去掉 SEMIJOIN 提示):五条排除全被物化,b_in 成了 friend_block
//     主键全索引扫描、r_in 成了 friend_request 全表扫描。这两种计划都与玩家总数或无上限的 per-me 集合成正比。
//     (c) 只加 SEMIJOIN(FIRSTMATCH)(去掉 FORCE INDEX):在上述夹具、friend_explain_scratch(5 万玩家 /
//     100 万边)和"2 万人拉黑我"的对抗库上,计划都**仍是**逐行主键点查,只是 possible_keys 多出二级索引
//     (如 b_in 的 idx_blocked_player)—— 没有实测到退化。FORCE INDEX 留作防线:防统计信息变化后优化器
//     改用二级索引去建排除集;上面那个计划守卫用例用 possible_keys == PRIMARY 的断言钉住它。
//     TiDB 不认 SEMIJOIN 提示(警告 8061 后忽略)。2026-10-09 在 v8.5.2 上重新核对:统计新鲜、排除表明显大于窗口时,
//     b_in / r_in 是按候选的主键批量点查(≤ W 次);排除表小(实测 1 万行的 friend_block)时统计新鲜也会直接扫整张表。
//     两者都是代价估算的结果、不是 SQL 结构保证的 —— 统计低估表规模时,大表上的 b_in / r_in 同样会变成整表扫描。
//     TiDB 也没有跳跃扫描,窗口生产比 MySQL 贵。迁移到 TiDB 之前要先加它自己的提示,做法与实测数字登记在
//     docs/handoff/friend-handoff-20260920.md §9.5 第 12 条。
//  6. **共同好友数是选择列表里的相关标量子查询,只对返回的行求值**(2026-10-09 加;此前恒填 0,见 RecommendCandidate)。
//     m1 是 `player_id = me` 的主键前缀 ref(我的 F 个好友 x),m2 按 (c.player_id, x) 做完整主键单行点查:数的是
//     "我的好友列表与候选的好友列表的交集"。好友边双向成对时,它与 RecommendByMutual 数的 (我, x)、(x, 候选) 是同一个数
//     (RecommendByMutual 的上界推导同样假定成对);为什么不照抄那两条边,见下面 (b)。
//     每返回一行多 2F+1 次 Handler 读(1 次定位 + F 次 next + F 次点查),整条查询多 limit×(2F+1)。2026-10-09 实测:
//     limit=20 时 F=16 / 20 / 30 / 300 依次多 660 / 820 / 1,220 / 12,020 次;F=20 时 limit=1 / 5 只多 41 / 205 次。
//     两处写法是承重的:
//     (a) **外层 ORDER BY 不能引用 mutual**(例如想把兜底结果按共同好友数降序排)。排序键一旦依赖子查询,它就得对
//     窗口里每个合格者都求值之后才能排序,代价从 limit×(2F+1) 变成 W×(2F+1)(F=300 时约 61.5 万次)。实测 300 人的
//     窗口、F=30(exclude 是自己加 1 个窗口内的候选):1,932 → 19,956 次;exclude 只有自己时是 20,022 次。
//     现在按 c.player_id 排:优化器先给物化窗口排序、逐行做反连接、凑够 limit 即停,
//     子查询只在一行被送出时才求值。TestRecommendAnchor_CountsRealMutualFriendsForOutputRowsOnly 的读数断言守这一点。
//     (b) **m2 的主键前缀必须是候选(`m2.player_id = c.player_id`),不要"对齐"成 RecommendByMutual 的 f2 那样按我的好友
//     去连(`m2.player_id = m1.friend_player_id AND m2.friend_player_id = c.player_id`)**。两种写法在 MySQL 上计划同形、
//     读数相同(都是 m1 前缀 ref + m2 完整主键点查),在成对的数据上结果逐行相同,回归测不出差别(只有单独冷跑时的页读
//     不同,见下面"页读");真正的差别在 TiDB。TiDB 把这条子查询做成
//     对每个返回行执行一次的 Apply,相关列 c.player_id 只有落在索引前缀上才会被拿去定位。2026-10-09 在 v8.5.2 上实测
//     (4,400 人、每人 200 个好友、88 万条边,统计新鲜,limit=20):按候选连,m1 / m2 各读 4,000 行(每返回一行读我的
//     200 行 + 候选的 200 行),整条约 0.1 s;按我的好友连,m2 变成每返回一行全扫一遍 friend —— 17,600,000 行,首次执行 13.6 s。
//     库小的时候不全扫,但每返回一行要读我全部好友的全部出边(F=30 的夹具:13,200 行对 40 行)。统计声称空表、统计停在
//     只有 60 行两种陈旧状态下,按候选连都仍然只读前缀,结果与 MySQL 逐行相同。
//     这样写还有一个好处:MySQL 上不论连接顺序如何,m1 / m2 能走的只有主键前缀或完整主键。去掉 STRAIGHT_JOIN 后优化器
//     可能改由 m2(候选的好友前缀)驱动、m1 点查,同样有界、结果相同(F=30 的夹具:统计新鲜 812 次、n_rows = 0 被重读的
//     状态 1,831 次;保留时 1,932 / 2,951 次;n_diff = 0 的状态下去不去计划都一样)。STRAIGHT_JOIN 与两处
//     FORCE INDEX (PRIMARY) 留着,是为了让代价只取决于我自己的好友数、计划不随统计摆动;计划守卫用例断言 m1 / m2 的
//     访问方式,并断言 m2 的 ref 以 c.player_id 开头(防有人把连接条件改回上面那种)。
//     另外留意占位符:m1 的 `player_id = ?` 是整条语句的第一个(选择列表在 FROM 之前),见 recommendAnchorStatement。
//     页读:m1 读我自己的主键前缀(几页),m2 的点查落在**候选自己**的主键前缀上 —— 正是本条查询生产窗口时刚读过的
//     叶子页,所以子查询不带来新的冷页。2026-10-09 实测(MySQL 26.7.0,innodb_buffer_pool_size = 128 MB;F=300、每个好友
//     300 条边,limit=20,每次测量前把整个库挤出 buffer pool,测了两轮;耗时是本机数字):单独冷跑 14,171 次读、
//     186 次页读、约 0.11–0.12 s —— 页读与恒填 0 的旧写法相同(2,151 次读、186 次页读、约 0.10–0.11 s);
//     全热 0 次页读、23–40 ms(旧写法 11–20 ms)。按我的好友去连的写法在同一个库上单独冷跑是 437–439 次页读、约 0.23 s
//     (多读了我全部好友的页),紧跟在 RecommendByMutual 之后跑时两种写法都是 177 次。
//     "不带来新的冷页"的适用范围:该库窗口里的候选每人 42–57 条边,主键前缀不跨叶子页。候选好友多到前缀跨页、而窗口
//     生产走跳跃扫描(只碰每人前缀的第一页)时,每返回一行至多再读它前缀余下的一两页,合计几十页 —— 这一句是推算,没有实测。
//
// 上界(与表规模无关):Handler 读 ≤ 窗口生产 + (W+1)(物化表扫描)+ 5×W(点查)+ limit×(2F+1)(第 6 条的共同好友
// 子查询),外加 ≤ W 行的内存排序。下面"7×W+3"与 09-28 的实测读数都**不含**最后一项(当时恒填 0);含它时每返回一行
// 另加 2F+1。
// 窗口生产随计划而变(见 RecommendAnchorWindow 的"代价"):跳跃扫描下 ≤ W+2,前三项合计 7×W+3 = 7,171;
// 范围扫描下按窗口里 W 个人的出边数计(最坏约 W×MaxFriends),7×W+3 **不是**上界 —— 例如下一段那个
// 5 万玩家 / 100 万边的库,pivot=1049629、me=1049999 时优化器选了范围扫描(窗口里只有 371 人、每人 20 条边),
// 窗口生产就读了 7,420 条出边,合计 7,898 次。
// 2026-09-28 实测(MySQL 26.7.0,服务端预处理语句):5 万玩家 / 100 万边的库上 pivot=1000100 / 1025000 / 1030000
// 都是 2,151 次(跳跃扫描;旧写法依次 149,725 / 75,055 / 60,055);对抗库(5 万玩家,窗口里塞满 734 个有上限的排除
// + 100 个拉黑我的)4,301 次,旧写法超过 120 s 被 MAX_EXECUTION_TIME 中断;窗口被 2 万个"拉黑我的"占满时返回空
// (旧写法约 22.8 s 后返回窗口外的 10 个)。后者的读数随优化器把 b_in 排在嵌套循环第几层(k)而变,同形库几次实测
// 都走跳跃扫描:4,099(k=2)/ 5,123(k=3)/ 6,147(k=4),即 (2+k)×W+3,都在 7×W+3 之内。
//
// 窗口里合格者不足 limit 时返回偏少(可能为空),见 RecommendRandom 的"代价"。
func (r *FriendRepo) recommendAnchor(ctx context.Context, playerID uint64, exclude []uint64, pivot uint64, limit uint32) ([]RecommendCandidate, error) {
	query, args := recommendAnchorStatement(playerID, exclude, pivot, limit)
	return r.scanRecommendCandidates(ctx, "random", playerID, query, args)
}

// recommendAnchorBaseSQL 是 recommendAnchor 的主体;调用方 exclude 与 ORDER BY / LIMIT 由
// recommendAnchorStatement 拼在后面。每一处写法为什么不能动,见 recommendAnchor 的注释。
const recommendAnchorBaseSQL = `SELECT c.player_id,
       (SELECT COUNT(*) FROM friend m1 FORCE INDEX (PRIMARY)
        STRAIGHT_JOIN friend m2 FORCE INDEX (PRIMARY)
                ON m2.player_id = c.player_id AND m2.friend_player_id = m1.friend_player_id
        WHERE m1.player_id = ?) AS mutual
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
// playerID(选择列表里数共同好友的 m1 前缀)→ pivot → 窗口 W
// → playerID ×6(<> 自排除、f、b_out、b_in、r_out、r_in)→ exclude... → limit。
// 第一个 playerID 排在 pivot 之前,是因为选择列表在 SQL 文本里先于 FROM 出现。它与 pivot 对调不会报错(都是 BIGINT):
// m1 拿 pivot 当 me 去数、窗口改从 me 的 id 起扫 —— 返回的 id 可能照样对,数字是错的
// (TestRecommendAnchor_HonorsPivotExcludeAndLimit 里有这个反例)。
func recommendAnchorStatement(playerID uint64, exclude []uint64, pivot uint64, limit uint32) (string, []any) {
	excludeClause, excludeArgs := recommendExcludeClause("c.player_id", exclude)
	query := recommendAnchorBaseSQL + excludeClause + "\nORDER BY c.player_id\nLIMIT ?"
	args := make([]any, 0, 9+len(excludeArgs)+1)
	// 两个 LIMIT 都显式转 int64,理由同 RecommendByMutual:给驱动一个原生支持的类型。
	args = append(args, playerID, pivot, int64(RecommendAnchorWindow),
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
		// MutualFriends 扫成 uint32 是安全的:两条路径数的都是"我的好友里有几个也是他的好友",
		// 上界是查询者的好友数,受 Friend.MaxFriends 封顶(默认 200)。
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
